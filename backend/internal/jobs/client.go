package jobs

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	mrand "math/rand/v2"
	"os"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// QueueConfig configures one queue a client works.
type QueueConfig struct {
	// MaxWorkers is how many of the queue's jobs this client runs at once (1–10000).
	MaxWorkers int
}

// TxBinder is a transaction the caller started: Bind returns a context whose database calls run in that
// transaction, Database the database it runs on. store.Q satisfies it.
type TxBinder interface {
	Bind(ctx context.Context) context.Context
	Database() *mongo.Database
}

// Config configures a client. Zero values take the defaults noted.
type Config struct {
	// ID names this client in job documents and in the leader lease. Default: host, pid, start time.
	ID string
	// Queues this client works, with their worker counts. Empty = insert-only client.
	Queues map[string]QueueConfig
	// Workers the client runs; needed when Queues is set.
	Workers *Workers
	// PeriodicJobs are inserted on their schedule by the elected leader.
	PeriodicJobs []*PeriodicJob
	// MaxAttempts is the default number of tries for a job. Default 25.
	MaxAttempts int
	// JobTimeout is the default time a job may run before its context is cancelled. Default 1 minute;
	// -1 = no timeout.
	JobTimeout time.Duration
	// FetchPollInterval is how often each queue looks for available jobs. Inserts made through this client
	// outside a transaction are picked up at once. Default 500ms.
	FetchPollInterval time.Duration
	// JobLease is how long a running job is held without its lease being renewed. The client renews the
	// lease of its running jobs every JobLease/3; a job whose server died is rescued (retried, or discarded
	// when out of attempts) once its lease has run out. Default 1 minute.
	JobLease time.Duration
	// How long finished jobs are kept before the leader deletes them: completed and cancelled 24 hours,
	// discarded 7 days (River's defaults). -1 = keep them.
	CompletedJobRetentionPeriod time.Duration
	CancelledJobRetentionPeriod time.Duration
	DiscardedJobRetentionPeriod time.Duration
	// Logger receives the queue's own messages. Default: warnings and errors to stderr.
	Logger *slog.Logger
}

// timing holds the internal intervals (River's values); tests in this package shorten them.
type timing struct {
	electInterval     time.Duration // how often a client tries to become (or stay) leader
	electJitter       time.Duration
	leaderTTL         time.Duration // a leader that stops renewing loses the lease after this
	schedulerInterval time.Duration // retryable/scheduled jobs whose time has come → available
	rescuerInterval   time.Duration
	cleanerInterval   time.Duration
	heartbeat         time.Duration // lease renewal of running jobs
}

// errStop is the cause of the work context's cancellation when the client stops.
var errStop = errors.New("jobs: client stopping")

// Client inserts jobs and, once started, works them.
type Client struct {
	db      *mongo.Database
	cfg     Config
	t       timing
	log     *slog.Logger
	workers map[string]func(row *JobRow) workUnit
	kinds   []string
	queues  []string
	wake    map[string]chan struct{}

	mu          sync.Mutex
	running     bool
	fetchCancel context.CancelFunc
	workCancel  context.CancelCauseFunc
	stopped     chan struct{}

	leader atomic.Bool

	runMu  sync.Mutex
	runIDs map[int64]int // jobs this client is running, for lease renewal
}

// NewClient returns a client for the queue stored in db and creates the queue's indexes.
func NewClient(db *mongo.Database, config *Config) (*Client, error) {
	if db == nil {
		return nil, errors.New("jobs: NewClient needs a database")
	}
	if config == nil {
		config = &Config{}
	}
	cfg := *config
	cfg.MaxAttempts = cmp.Or(cfg.MaxAttempts, MaxAttemptsDefault)
	cfg.JobTimeout = cmp.Or(cfg.JobTimeout, JobTimeoutDefault)
	cfg.FetchPollInterval = cmp.Or(cfg.FetchPollInterval, 500*time.Millisecond)
	cfg.JobLease = cmp.Or(cfg.JobLease, time.Minute)
	cfg.CompletedJobRetentionPeriod = cmp.Or(cfg.CompletedJobRetentionPeriod, 24*time.Hour)
	cfg.CancelledJobRetentionPeriod = cmp.Or(cfg.CancelledJobRetentionPeriod, 24*time.Hour)
	cfg.DiscardedJobRetentionPeriod = cmp.Or(cfg.DiscardedJobRetentionPeriod, 7*24*time.Hour)
	if cfg.ID == "" {
		host, _ := os.Hostname()
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		cfg.ID = fmt.Sprintf("%s_%d_%s_%s", host, os.Getpid(), time.Now().UTC().Format("2006_01_02T15_04_05"), hex.EncodeToString(b))
	}
	switch {
	case cfg.MaxAttempts < 1:
		return nil, errors.New("jobs: MaxAttempts cannot be less than one")
	case cfg.JobTimeout < -1:
		return nil, errors.New("jobs: JobTimeout cannot be negative, except for -1 (infinite)")
	case cfg.FetchPollInterval < time.Millisecond:
		return nil, errors.New("jobs: FetchPollInterval must be at least 1ms")
	case cfg.JobLease < 0:
		return nil, errors.New("jobs: JobLease cannot be negative")
	case cfg.CompletedJobRetentionPeriod < -1 || cfg.CancelledJobRetentionPeriod < -1 || cfg.DiscardedJobRetentionPeriod < -1:
		return nil, errors.New("jobs: retention periods cannot be negative, except for -1 (keep)")
	case len(cfg.Queues) > 0 && cfg.Workers == nil:
		return nil, errors.New("jobs: Workers must be set if Queues is set")
	}
	for name, q := range cfg.Queues {
		if name == "" {
			return nil, errors.New("jobs: a queue name must not be empty")
		}
		if q.MaxWorkers < 1 || q.MaxWorkers > 10000 {
			return nil, fmt.Errorf("jobs: MaxWorkers of queue %q must be between 1 and 10000", name)
		}
	}
	for i, p := range cfg.PeriodicJobs {
		if p == nil || p.schedule == nil || p.constructor == nil {
			return nil, fmt.Errorf("jobs: periodic job %d needs a schedule and a constructor", i)
		}
	}
	c := &Client{
		db:  db,
		cfg: cfg,
		t: timing{
			electInterval:     5 * time.Second,
			electJitter:       time.Second,
			leaderTTL:         15 * time.Second,
			schedulerInterval: 5 * time.Second,
			rescuerInterval:   min(30*time.Second, cfg.JobLease/2),
			cleanerInterval:   30 * time.Second,
			heartbeat:         cfg.JobLease / 3,
		},
		log:     cfg.Logger,
		workers: map[string]func(row *JobRow) workUnit{},
		wake:    map[string]chan struct{}{},
		runIDs:  map[int64]int{},
		stopped: closedChan(),
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	if cfg.Workers != nil {
		maps.Copy(c.workers, cfg.Workers.byKind)
	}
	c.kinds = slices.Sorted(maps.Keys(c.workers))
	c.queues = slices.Sorted(maps.Keys(cfg.Queues))
	for _, q := range c.queues {
		c.wake[q] = make(chan struct{}, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := EnsureIndexes(ctx, db); err != nil {
		return nil, err
	}
	return c, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// ID is the client's id.
func (c *Client) ID() string { return c.cfg.ID }

// IsLeader tells whether this client currently holds the leader lease (and so inserts periodic jobs).
func (c *Client) IsLeader() bool { return c.leader.Load() }

func (c *Client) jobs() *mongo.Collection { return c.db.Collection(CollectionJobs) }
func (c *Client) meta() *mongo.Collection { return c.db.Collection(CollectionMeta) }

// ---------- Inserting ----------

// Insert inserts a job on its own (never inside a caller's transaction).
func (c *Client) Insert(ctx context.Context, args JobArgs, opts *InsertOpts) (*InsertResult, error) {
	d, err := c.newDoc(args, opts, time.Now(), nil)
	if err != nil {
		return nil, err
	}
	ctx = mongo.NewSessionContext(ctx, nil) // a session in ctx must not pull the insert into a transaction
	res, err := insertDoc(ctx, ctx, c.db, d)
	if err == nil && !res.UniqueSkippedAsDuplicate {
		c.notify(d.Queue)
	}
	return res, err
}

// InsertTx inserts a job in the caller's transaction: the job exists (and runs) only if it commits.
// Like any write in a MongoDB transaction, a failure here aborts the transaction.
func (c *Client) InsertTx(ctx context.Context, tx TxBinder, args JobArgs, opts *InsertOpts) (*InsertResult, error) {
	if tx == nil || tx.Database() == nil {
		return nil, errors.New("jobs: InsertTx needs a transaction")
	}
	d, err := c.newDoc(args, opts, time.Now(), nil)
	if err != nil {
		return nil, err
	}
	res, err := insertDoc(tx.Bind(ctx), mongo.NewSessionContext(ctx, nil), tx.Database(), d)
	if err == nil && !res.UniqueSkippedAsDuplicate {
		// The commit comes later; look again shortly rather than waiting a whole poll interval.
		time.AfterFunc(100*time.Millisecond, func() { c.notify(d.Queue) })
	}
	return res, err
}

// newDoc merges the options as River does (explicit opts, then the args' InsertOpts, then defaults).
// periodicAt is the run time of a periodic job, used as its scheduled time.
func (c *Client) newDoc(args JobArgs, opts *InsertOpts, now time.Time, periodicAt *time.Time) (*jobDoc, error) {
	if args == nil {
		return nil, errors.New("jobs: job args must not be nil")
	}
	kind := args.Kind()
	if kind == "" {
		return nil, errors.New("jobs: a job kind must not be empty")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("jobs: error marshaling args to JSON: %w", err)
	}
	if opts == nil {
		opts = &InsertOpts{}
	}
	var argOpts InsertOpts
	if a, ok := args.(JobArgsWithInsertOpts); ok {
		argOpts = a.InsertOpts()
	}
	maxAttempts := cmp.Or(opts.MaxAttempts, argOpts.MaxAttempts, c.cfg.MaxAttempts)
	priority := cmp.Or(opts.Priority, argOpts.Priority, PriorityDefault)
	queue := cmp.Or(opts.Queue, argOpts.Queue, QueueDefault)
	if maxAttempts < 1 {
		return nil, errors.New("jobs: MaxAttempts cannot be less than one")
	}
	if priority < 1 || priority > 4 {
		return nil, errors.New("jobs: priority must be between 1 and 4")
	}
	uniq := opts.UniqueOpts
	if uniq.isEmpty() {
		uniq = argOpts.UniqueOpts
	}
	if err := uniq.validate(); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	state, at := JobStateAvailable, now
	switch {
	case !opts.ScheduledAt.IsZero():
		state, at = JobStateScheduled, opts.ScheduledAt
	case !argOpts.ScheduledAt.IsZero():
		state, at = JobStateScheduled, argOpts.ScheduledAt
	case periodicAt != nil:
		at = *periodicAt
	}
	if state == JobStateScheduled && !at.After(now) {
		state = JobStateAvailable // River leaves it to the scheduler; it can run now
	}
	if opts.Pending {
		state = JobStatePending
	}
	d := &jobDoc{
		Kind: kind, Queue: queue, Args: string(encoded), State: state, Priority: priority, MaxAttempts: maxAttempts,
		AttemptedBy: []string{}, CreatedAt: now, ScheduledAt: at, Errors: []AttemptError{}, Periodic: periodicAt != nil,
	}
	if !uniq.isEmpty() {
		if d.UniqueKey, err = uniq.uniqueKey(kind, encoded, queue, at); err != nil {
			return nil, fmt.Errorf("jobs: %w", err)
		}
		d.UniqueStates = uniq.states()
	}
	return d, nil
}

// insertDoc stores a new job. ctx carries the caller's transaction, if any; idCtx has no session (the id
// counter is never part of a transaction). A unique job is an upsert on its unique lock, so a duplicate
// is found, not an error that would abort the caller's transaction.
func insertDoc(ctx, idCtx context.Context, db *mongo.Database, d *jobDoc) (*InsertResult, error) {
	id, err := nextJobID(idCtx, db)
	if err != nil {
		return nil, fmt.Errorf("jobs: next job id: %w", err)
	}
	d.ID = id
	coll := db.Collection(CollectionJobs)
	if d.UniqueKey == "" {
		if _, err := coll.InsertOne(ctx, d); err != nil {
			return nil, fmt.Errorf("jobs: insert %s job: %w", d.Kind, err)
		}
		return &InsertResult{Job: d.row()}, nil
	}
	inSession := mongo.SessionFromContext(ctx) != nil
	if !slices.Contains(d.UniqueStates, d.State) {
		return nil, fmt.Errorf("jobs: a unique job cannot be inserted %s unless that state is in its UniqueOpts.ByState", d.State)
	}
	for try := 0; try < 5; try++ {
		// The filter's equality sets unique_lock on the inserted document.
		up, err := coll.UpdateOne(ctx, bson.M{"unique_lock": d.UniqueKey}, bson.M{"$setOnInsert": d}, options.UpdateOne().SetUpsert(true))
		if err != nil {
			if mongo.IsDuplicateKeyError(err) && !inSession {
				continue // a concurrent insert of the same unique job won; read it next time round
			}
			return nil, fmt.Errorf("jobs: insert unique %s job: %w", d.Kind, err)
		}
		if up.UpsertedCount == 1 {
			d.UniqueLock = d.UniqueKey
			return &InsertResult{Job: d.row()}, nil
		}
		var existing jobDoc
		err = coll.FindOne(ctx, bson.M{"unique_lock": d.UniqueKey}).Decode(&existing)
		if errors.Is(err, mongo.ErrNoDocuments) {
			continue // it left its unique states just now; try again
		}
		if err != nil {
			return nil, fmt.Errorf("jobs: insert unique %s job: %w", d.Kind, err)
		}
		return &InsertResult{Job: existing.row(), UniqueSkippedAsDuplicate: true}, nil
	}
	return nil, fmt.Errorf("jobs: insert unique %s job: kept conflicting with concurrent inserts", d.Kind)
}

// JobGet returns a stored job.
func (c *Client) JobGet(ctx context.Context, id int64) (*JobRow, error) {
	var d jobDoc
	err := c.jobs().FindOne(ctx, bson.M{"_id": id}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d.row(), nil
}

func (c *Client) notify(queue string) {
	if ch, ok := c.wake[queue]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (c *Client) notifyAll() {
	for _, q := range c.queues {
		c.notify(q)
	}
}

// ---------- Starting and stopping ----------

// Start starts fetching and working jobs, leader election (periodic jobs and maintenance), and lease
// renewal, in background goroutines. Jobs run with a context derived from ctx: cancelling ctx stops the
// client and cancels running jobs (like StopAndCancel); Stop stops gracefully.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil
	}
	if len(c.queues) == 0 || len(c.kinds) == 0 {
		return errors.New("jobs: Queues and Workers must be configured for a client to start working")
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := c.db.Client().Ping(pctx, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("jobs: error making initial connection to database: %w", err)
	}
	fetchCtx, fetchCancel := context.WithCancel(ctx)
	workCtx, workCancel := context.WithCancelCause(ctx)
	hbCtx, hbCancel := context.WithCancel(context.WithoutCancel(ctx))
	stopped := make(chan struct{})
	c.running, c.fetchCancel, c.workCancel, c.stopped = true, fetchCancel, workCancel, stopped

	var producers sync.WaitGroup
	for _, q := range c.queues {
		producers.Add(1)
		go func() {
			defer producers.Done()
			c.produce(fetchCtx, workCtx, q)
		}()
	}
	electDone := make(chan struct{})
	go func() {
		defer close(electDone)
		c.elect(fetchCtx)
	}()
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		c.heartbeat(hbCtx)
	}()
	go func() {
		<-fetchCtx.Done()
		producers.Wait() // every running job has finished and its result is stored
		workCancel(errStop)
		hbCancel()
		<-hbDone
		<-electDone // resigned leadership
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
		close(stopped)
	}()
	return nil
}

// Stop stops fetching jobs and waits for the running ones to finish. If ctx ends first, Stop returns its
// error and the client goes on stopping in the background.
func (c *Client) Stop(ctx context.Context) error { return c.stop(ctx, false) }

// StopAndCancel is Stop that also cancels the contexts of running jobs; a job that then returns the
// context's error goes back to available without using up an attempt.
func (c *Client) StopAndCancel(ctx context.Context) error { return c.stop(ctx, true) }

func (c *Client) stop(ctx context.Context, cancelWork bool) error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}
	fetchCancel, workCancel, stopped := c.fetchCancel, c.workCancel, c.stopped
	c.mu.Unlock()
	if cancelWork {
		workCancel(errStop)
	}
	fetchCancel()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stopped is closed once the client has stopped (already closed when it is not running).
func (c *Client) Stopped() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// ---------- Fetching and working ----------

// produce claims jobs of one queue while it has free workers, polling every FetchPollInterval (or when
// woken by a local insert or a finished job). It returns once fetchCtx ends and its running jobs are done.
func (c *Client) produce(fetchCtx, workCtx context.Context, queue string) {
	limit := c.cfg.Queues[queue].MaxWorkers
	done := make(chan struct{}, limit)
	var running sync.WaitGroup
	defer running.Wait()
	poll := time.NewTicker(c.cfg.FetchPollInterval)
	defer poll.Stop()
	active := 0
	for {
		for active < limit && fetchCtx.Err() == nil {
			d, err := c.claim(fetchCtx, queue)
			if err != nil {
				if fetchCtx.Err() == nil {
					c.log.Error("jobs: fetching jobs failed", "queue", queue, "error", err)
				}
				break
			}
			if d == nil {
				break
			}
			active++
			running.Add(1)
			go func() {
				defer running.Done()
				c.execute(workCtx, d)
				done <- struct{}{}
			}()
		}
		select {
		case <-fetchCtx.Done():
			return
		case <-done:
			active--
		drain:
			for {
				select {
				case <-done:
					active--
				default:
					break drain
				}
			}
		case <-c.wake[queue]:
		case <-poll.C:
		}
	}
}

// claim atomically takes the next available job of the queue whose time has come: it becomes running,
// with this client's lease. Only kinds this client has workers for are taken.
func (c *Client) claim(fetchCtx context.Context, queue string) (*jobDoc, error) {
	// Not cancelled by Stop half-way: a job claimed but never run would wait for the rescuer.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(fetchCtx), 15*time.Second)
	defer cancel()
	now := time.Now()
	var d jobDoc
	err := c.jobs().FindOneAndUpdate(ctx,
		bson.M{"state": JobStateAvailable, "queue": queue, "scheduled_at": bson.M{"$lte": now}, "kind": bson.M{"$in": c.kinds}},
		bson.M{
			"$set":  bson.M{"state": JobStateRunning, "attempted_at": now, "lease_owner": c.cfg.ID, "lease_until": now.Add(c.cfg.JobLease)},
			"$inc":  bson.M{"attempt": 1},
			"$push": bson.M{"attempted_by": bson.M{"$each": bson.A{c.cfg.ID}, "$slice": -100}},
		},
		options.FindOneAndUpdate().
			SetSort(bson.D{key("priority", 1), key("scheduled_at", 1), key("_id", 1)}).
			SetReturnDocument(options.After),
	).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.track(d.ID, 1)
	return &d, nil
}

func (c *Client) track(id int64, delta int) {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.runIDs[id] += delta; c.runIDs[id] <= 0 {
		delete(c.runIDs, id)
	}
}

func (c *Client) trackedIDs() []int64 {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	return slices.Collect(maps.Keys(c.runIDs))
}

type runResult struct {
	err         error
	panicVal    any
	trace       string
	unmarshaled bool
}

func (c *Client) execute(workCtx context.Context, d *jobDoc) {
	defer c.track(d.ID, -1)
	row := d.row()
	unit := c.workers[row.Kind](row)
	start := time.Now()
	res := c.run(workCtx, unit, row)
	c.finish(workCtx, d, unit, res, start)
}

// run works the job with its timeout, turning a panic into a failed try.
func (c *Client) run(workCtx context.Context, unit workUnit, row *JobRow) (res runResult) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("jobs: panic recovery; possible bug with Worker", "job_id", row.ID, "kind", row.Kind, "panic_val", fmt.Sprint(r))
			res.panicVal, res.trace = r, string(debug.Stack())
		}
	}()
	if err := unit.unmarshal(); err != nil {
		res.err = err
		return res
	}
	res.unmarshaled = true
	ctx := workCtx
	if timeout := cmp.Or(unit.timeout(), c.cfg.JobTimeout); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	res.err = unit.work(ctx)
	return res
}

// safeNextRetry asks the worker for its retry time; a panic there means the default.
func safeNextRetry(unit workUnit) (t time.Time) {
	defer func() {
		if recover() != nil {
			t = time.Time{}
		}
	}()
	return unit.nextRetry()
}

// defaultNextRetry is River's backoff: errorCount^4 seconds, ±10% jitter.
func defaultNextRetry(now time.Time, errorCount int) time.Time {
	secs := math.Pow(float64(errorCount), 4)
	secs += secs * (mrand.Float64()*0.2 - 0.1)
	secs = min(secs, 100*365*24*3600) // stays a valid duration
	return now.Add(time.Duration(secs * float64(time.Second)))
}

// uniqueLock adds the unique lock to set (or unset) for a job moving to state.
func uniqueLock(d *jobDoc, state JobState, set, unset bson.M) {
	if d.UniqueKey == "" {
		return
	}
	if slices.Contains(d.UniqueStates, state) {
		set["unique_lock"] = d.UniqueKey
	} else {
		unset["unique_lock"] = ""
	}
}

// finish records a try's outcome, as River's executor does: completed; cancelled (JobCancel); back to
// available when the client was stopped with StopAndCancel; discarded once out of attempts; otherwise
// retried at the worker's NextRetry or the default backoff (available at once when that is within the
// scheduler interval, retryable otherwise).
func (c *Client) finish(workCtx context.Context, d *jobDoc, unit workUnit, res runResult, start time.Time) {
	now := time.Now()
	set := bson.M{}
	unset := bson.M{"lease_owner": "", "lease_until": ""}
	update := bson.M{}
	failure := ""
	if res.panicVal != nil {
		failure = fmt.Sprintf("jobs: job panicked: %v", res.panicVal)
	} else if res.err != nil {
		failure = res.err.Error()
	}
	var cancelErr *JobCancelError
	var state JobState
	switch {
	case failure == "":
		state = JobStateCompleted
		set["finalized_at"] = now
	case res.panicVal == nil && errors.As(res.err, &cancelErr):
		state = JobStateCancelled
		set["finalized_at"] = now
	case res.panicVal == nil && errors.Is(context.Cause(workCtx), errStop) && (errors.Is(res.err, context.Canceled) || errors.Is(res.err, errStop)):
		// Interrupted by StopAndCancel: not counted as a try.
		state = JobStateAvailable
		set["scheduled_at"] = now
		update["$inc"] = bson.M{"attempt": -1}
		failure = ""
	case d.Attempt >= d.MaxAttempts:
		state = JobStateDiscarded
		set["finalized_at"] = now
	default:
		var next time.Time
		if res.unmarshaled {
			next = safeNextRetry(unit)
		}
		if next.IsZero() || next.Before(now) {
			next = defaultNextRetry(now, len(d.Errors)+1)
		}
		set["scheduled_at"] = next
		state = JobStateRetryable
		if next.Sub(now) <= c.t.schedulerInterval {
			state = JobStateAvailable
		}
	}
	set["state"] = state
	uniqueLock(d, state, set, unset)
	update["$set"], update["$unset"] = set, unset
	if failure != "" {
		update["$push"] = bson.M{"errors": AttemptError{At: start, Attempt: d.Attempt, Error: failure, Trace: res.trace}}
		level := slog.LevelInfo
		if state == JobStateDiscarded {
			level = slog.LevelWarn
		}
		c.log.Log(context.Background(), level, "jobs: job errored", "job_id", d.ID, "kind", d.Kind, "attempt", d.Attempt,
			"max_attempts", d.MaxAttempts, "state", string(state), "error", failure)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(workCtx), 30*time.Second)
	defer cancel()
	filter := bson.M{"_id": d.ID, "state": JobStateRunning, "attempt": d.Attempt}
	for try := 1; ; try++ {
		r, err := c.jobs().UpdateOne(ctx, filter, update)
		if err == nil {
			if r.MatchedCount == 0 {
				c.log.Warn("jobs: job was no longer running when its try ended (rescued?)", "job_id", d.ID, "kind", d.Kind)
			}
			return
		}
		if mongo.IsDuplicateKeyError(err) && set["unique_lock"] != nil {
			// Another job holds the unique key: finish this one without it.
			delete(set, "unique_lock")
			unset["unique_lock"] = ""
			continue
		}
		if try >= 5 || ctx.Err() != nil {
			c.log.Error("jobs: failed to record the job's result", "job_id", d.ID, "kind", d.Kind, "state", string(state), "error", err)
			return
		}
		time.Sleep(time.Duration(try) * 200 * time.Millisecond)
	}
}

// heartbeat renews the leases of this client's running jobs until ctx ends.
func (c *Client) heartbeat(ctx context.Context) {
	tick := time.NewTicker(c.t.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ids := c.trackedIDs()
		if len(ids) == 0 {
			continue
		}
		uctx, cancel := context.WithTimeout(ctx, c.t.heartbeat)
		_, err := c.jobs().UpdateMany(uctx, bson.M{"_id": bson.M{"$in": ids}, "state": JobStateRunning, "lease_owner": c.cfg.ID},
			bson.M{"$set": bson.M{"lease_until": time.Now().Add(c.cfg.JobLease)}})
		cancel()
		if err != nil && ctx.Err() == nil {
			c.log.Warn("jobs: renewing job leases failed", "error", err)
		}
	}
}

// ---------- Leader election and the leader's work ----------

// tryLead takes the leader lease when it is free or expired, or renews it when this client holds it.
// Expiry uses the database server's clock ($$NOW), so servers with drifting clocks agree.
func (c *Client) tryLead(ctx context.Context) (bool, error) {
	// One atomic upsert: take the lease when it is ours, missing or expired; otherwise leave it as it is.
	// ($expr is not allowed in an upsert's filter, so the decision is made in the update pipeline.)
	me := c.cfg.ID
	update := mongo.Pipeline{
		bson.D{key("$set", bson.M{"_take": bson.M{"$or": bson.A{
			bson.M{"$eq": bson.A{"$leader_id", me}},
			bson.M{"$lt": bson.A{bson.M{"$ifNull": bson.A{"$expires_at", time.Unix(0, 0)}}, "$$NOW"}},
		}}})},
		bson.D{key("$set", bson.M{
			"elected_at": bson.M{"$cond": bson.A{bson.M{"$and": bson.A{"$_take", bson.M{"$ne": bson.A{"$leader_id", me}}}}, "$$NOW", "$elected_at"}},
			"leader_id":  bson.M{"$cond": bson.A{"$_take", me, "$leader_id"}},
			"expires_at": bson.M{"$cond": bson.A{"$_take", bson.M{"$add": bson.A{"$$NOW", c.t.leaderTTL.Milliseconds()}}, "$expires_at"}},
		})},
		bson.D{key("$unset", "_take")},
	}
	var lease struct {
		LeaderID string `bson:"leader_id"`
	}
	err := c.meta().FindOneAndUpdate(ctx, bson.M{"_id": "leader"}, update,
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&lease)
	if mongo.IsDuplicateKeyError(err) {
		return false, nil // two first-ever elections raced; the other one created the lease
	}
	if err != nil {
		return false, err
	}
	return lease.LeaderID == me, nil
}

// elect keeps trying to be leader every electInterval; while leader it runs lead. On the way out it
// resigns so another server can take over at once.
func (c *Client) elect(ctx context.Context) {
	var leadCancel context.CancelFunc
	var leadDone chan struct{}
	stopLeading := func() {
		if leadCancel != nil {
			leadCancel()
			<-leadDone
			leadCancel = nil
		}
	}
	defer func() {
		was := leadCancel != nil
		stopLeading()
		if was {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if _, err := c.meta().DeleteOne(rctx, bson.M{"_id": "leader", "leader_id": c.cfg.ID}); err != nil {
				c.log.Warn("jobs: resigning leadership failed", "error", err)
			}
			cancel()
		}
	}()
	for {
		tctx, cancel := context.WithTimeout(ctx, max(c.t.electInterval, time.Second))
		ok, err := c.tryLead(tctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.Warn("jobs: leader election failed", "error", err)
		}
		switch {
		case ok && leadCancel == nil:
			lctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			leadCancel, leadDone = cancel, done
			go func() {
				defer close(done)
				c.lead(lctx)
			}()
		case !ok && leadCancel != nil:
			c.log.Warn("jobs: leadership lost", "client_id", c.cfg.ID)
			stopLeading()
		}
		wait := c.t.electInterval
		if c.t.electJitter > 0 {
			wait += time.Duration(mrand.Int64N(int64(c.t.electJitter)))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// lead is the leader's work until ctx ends: periodic jobs (RunOnStart ones at once), the scheduler, the
// rescuer and the cleaner.
func (c *Client) lead(ctx context.Context) {
	c.leader.Store(true)
	defer c.leader.Store(false)
	type periodicRun struct {
		job  *PeriodicJob
		next time.Time
	}
	now := time.Now()
	runs := make([]periodicRun, 0, len(c.cfg.PeriodicJobs))
	for _, p := range c.cfg.PeriodicJobs {
		runs = append(runs, periodicRun{job: p, next: nextRun(p, now)})
		if p.runOnStart {
			c.insertPeriodic(ctx, p, now)
		}
	}
	c.scheduleDue(ctx)
	c.rescue(ctx)
	c.clean(ctx)
	sched := time.NewTicker(c.t.schedulerInterval)
	defer sched.Stop()
	rescue := time.NewTicker(c.t.rescuerInterval)
	defer rescue.Stop()
	clean := time.NewTicker(c.t.cleanerInterval)
	defer clean.Stop()
	for {
		wait := time.Hour
		for _, r := range runs {
			wait = min(wait, max(time.Until(r.next), 0))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			margin := time.Now().Add(100 * time.Millisecond)
			for i := range runs {
				if runs[i].next.Before(margin) {
					c.insertPeriodic(ctx, runs[i].job, runs[i].next)
					runs[i].next = nextRun(runs[i].job, runs[i].next)
				}
			}
		case <-sched.C:
			c.scheduleDue(ctx)
		case <-rescue.C:
			c.rescue(ctx)
		case <-clean.C:
			c.clean(ctx)
		}
		timer.Stop()
	}
}

// nextRun is the schedule's next time, never earlier than a second after the last one.
func nextRun(p *PeriodicJob, after time.Time) time.Time {
	n := p.schedule.Next(after)
	if !n.After(after) {
		n = after.Add(time.Second)
	}
	return n
}

func (c *Client) insertPeriodic(ctx context.Context, p *PeriodicJob, at time.Time) {
	args, opts := p.constructor()
	if args == nil {
		return
	}
	d, err := c.newDoc(args, opts, time.Now(), &at)
	if err == nil {
		ictx := mongo.NewSessionContext(ctx, nil)
		var res *InsertResult
		if res, err = insertDoc(ictx, ictx, c.db, d); err == nil && !res.UniqueSkippedAsDuplicate {
			c.notify(d.Queue)
		}
	}
	if err != nil && ctx.Err() == nil {
		c.log.Error("jobs: inserting a periodic job failed", "kind", args.Kind(), "error", err)
	}
}

// scheduleDue makes retryable and scheduled jobs whose time has come available (River's scheduler). A
// unique job that gave up its unique lock while retryable takes it back; if another job holds the key by
// then, it is discarded, as River does.
func (c *Client) scheduleDue(ctx context.Context) {
	now := time.Now()
	due := func(extra bson.M) bson.M {
		f := bson.M{"state": bson.M{"$in": bson.A{JobStateRetryable, JobStateScheduled}}, "scheduled_at": bson.M{"$lte": now}}
		maps.Copy(f, extra)
		return f
	}
	moved := int64(0)
	r, err := c.jobs().UpdateMany(ctx, due(bson.M{"$or": bson.A{
		bson.M{"unique_key": bson.M{"$exists": false}},
		bson.M{"unique_lock": bson.M{"$exists": true}},
	}}), bson.M{"$set": bson.M{"state": JobStateAvailable}})
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("jobs: scheduler failed", "error", err)
		}
		return
	}
	moved += r.ModifiedCount
	var relock []jobDoc
	cur, err := c.jobs().Find(ctx, due(bson.M{"unique_key": bson.M{"$exists": true}, "unique_lock": bson.M{"$exists": false}}),
		options.Find().SetLimit(1000))
	if err == nil {
		err = cur.All(ctx, &relock)
	}
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("jobs: scheduler failed", "error", err)
		}
		return
	}
	for _, d := range relock {
		f := bson.M{"_id": d.ID, "state": d.State, "unique_lock": bson.M{"$exists": false}}
		r, err := c.jobs().UpdateOne(ctx, f, bson.M{"$set": bson.M{"state": JobStateAvailable, "unique_lock": d.UniqueKey}})
		if mongo.IsDuplicateKeyError(err) {
			_, err = c.jobs().UpdateOne(ctx, f, bson.M{"$set": bson.M{"state": JobStateDiscarded, "finalized_at": now, "unique_conflict": "scheduler_discarded"}})
		} else if err == nil {
			moved += r.ModifiedCount
		}
		if err != nil && ctx.Err() == nil {
			c.log.Error("jobs: scheduler failed", "job_id", d.ID, "error", err)
		}
	}
	if moved > 0 {
		c.notifyAll()
	}
}

// rescue retries (or discards, when out of attempts) running jobs whose lease has run out: their server
// stopped renewing it, so it died or lost the database.
func (c *Client) rescue(ctx context.Context) {
	now := time.Now()
	var stuck []jobDoc
	cur, err := c.jobs().Find(ctx, bson.M{"state": JobStateRunning, "lease_until": bson.M{"$lt": now}}, options.Find().SetLimit(1000))
	if err == nil {
		err = cur.All(ctx, &stuck)
	}
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("jobs: rescuer failed", "error", err)
		}
		return
	}
	for _, d := range stuck {
		set := bson.M{}
		unset := bson.M{"lease_owner": "", "lease_until": ""}
		state := JobStateRetryable
		if d.Attempt >= d.MaxAttempts {
			state = JobStateDiscarded
			set["finalized_at"] = now
		} else {
			set["scheduled_at"] = defaultNextRetry(now, len(d.Errors)+1)
		}
		set["state"] = state
		uniqueLock(&d, state, set, unset)
		update := bson.M{"$set": set, "$unset": unset,
			"$push": bson.M{"errors": AttemptError{At: now, Attempt: d.Attempt, Error: "Stuck job rescued by Rescuer"}}}
		filter := bson.M{"_id": d.ID, "state": JobStateRunning, "attempt": d.Attempt, "lease_until": bson.M{"$lt": now}}
		r, err := c.jobs().UpdateOne(ctx, filter, update)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Error("jobs: rescuer failed", "job_id", d.ID, "error", err)
			}
			continue
		}
		if r.ModifiedCount > 0 {
			c.log.Warn("jobs: rescued a stuck job", "job_id", d.ID, "kind", d.Kind, "owner", d.LeaseOwner, "state", string(state))
		}
	}
}

// clean deletes finished jobs past their retention period (River's cleaner).
func (c *Client) clean(ctx context.Context) {
	now := time.Now()
	for _, x := range []struct {
		state JobState
		keep  time.Duration
	}{
		{JobStateCompleted, c.cfg.CompletedJobRetentionPeriod},
		{JobStateCancelled, c.cfg.CancelledJobRetentionPeriod},
		{JobStateDiscarded, c.cfg.DiscardedJobRetentionPeriod},
	} {
		if x.keep < 0 {
			continue
		}
		if _, err := c.jobs().DeleteMany(ctx, bson.M{"state": x.state, "finalized_at": bson.M{"$lt": now.Add(-x.keep)}}); err != nil {
			if ctx.Err() == nil {
				c.log.Error("jobs: cleaner failed", "error", err)
			}
			return
		}
	}
}
