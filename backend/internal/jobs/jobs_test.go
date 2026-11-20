package jobs

// Integration tests against a real MongoDB replica set. Set HRMS_TEST_MONGO_URL, e.g.
// mongodb://localhost:27017/?replicaSet=rs0&directConnection=true; skipped otherwise. They use (and drop)
// the database HRMS_TEST_JOBS_DB, default hrms_test_jobs.

import (
	"cmp"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func testDB(t *testing.T) *mongo.Database {
	t.Helper()
	url := os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	cl, err := mongo.Connect(options.Client().ApplyURI(url))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Disconnect(context.Background()) })
	db := cl.Database(cmp.Or(os.Getenv("HRMS_TEST_JOBS_DB"), "hrms_test_jobs"))
	if err := db.Drop(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

type workArgs struct {
	N int `json:"n"`
}

func (workArgs) Kind() string { return "work" }

// fnWorker works "work" jobs with a function; timeout and retry override the defaults when set.
type fnWorker struct {
	WorkerDefaults[workArgs]
	fn      func(ctx context.Context, j *Job[workArgs]) error
	timeout time.Duration
	retry   func(j *Job[workArgs]) time.Time
}

func (w *fnWorker) Work(ctx context.Context, j *Job[workArgs]) error { return w.fn(ctx, j) }
func (w *fnWorker) Timeout(*Job[workArgs]) time.Duration             { return w.timeout }
func (w *fnWorker) NextRetry(j *Job[workArgs]) time.Time {
	if w.retry != nil {
		return w.retry(j)
	}
	return time.Time{}
}

// newClient returns a client with short internal intervals (the defaults are River's 5s/15s/30s).
func newClient(t *testing.T, db *mongo.Database, w *fnWorker, mod func(*Config)) *Client {
	t.Helper()
	workers := NewWorkers()
	if w != nil {
		AddWorker(workers, w)
	}
	cfg := &Config{
		Queues:            map[string]QueueConfig{QueueDefault: {MaxWorkers: 5}},
		Workers:           workers,
		FetchPollInterval: 50 * time.Millisecond,
	}
	if mod != nil {
		mod(cfg)
	}
	c, err := NewClient(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.t.electInterval, c.t.electJitter, c.t.leaderTTL = 100*time.Millisecond, 0, time.Second
	c.t.schedulerInterval, c.t.rescuerInterval, c.t.cleanerInterval = 200*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	return c
}

func start(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func get(t *testing.T, c *Client, id int64) *JobRow {
	t.Helper()
	j, err := c.JobGet(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func waitState(t *testing.T, c *Client, id int64, state JobState, d time.Duration) *JobRow {
	t.Helper()
	var j *JobRow
	waitFor(t, d, "job "+string(state), func() bool { j = get(t, c, id); return j.State == state })
	return j
}

func insert(t *testing.T, c *Client, args JobArgs, opts *InsertOpts) *InsertResult {
	t.Helper()
	res, err := c.Insert(context.Background(), args, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func countKind(t *testing.T, db *mongo.Database, kind string) int64 {
	t.Helper()
	n, err := db.Collection(CollectionJobs).CountDocuments(context.Background(), bson.M{"kind": kind})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInsertAndWork(t *testing.T) {
	db := testDB(t)
	got := make(chan int, 1)
	c := newClient(t, db, &fnWorker{fn: func(_ context.Context, j *Job[workArgs]) error { got <- j.Args.N; return nil }},
		func(cfg *Config) { cfg.FetchPollInterval = 5 * time.Second }) // only the insert's wake-up can pick it up fast
	start(t, c)
	res := insert(t, c, workArgs{N: 7}, nil)
	if res.Job.ID < 1 || res.Job.State != JobStateAvailable || res.Job.MaxAttempts != MaxAttemptsDefault || res.Job.Queue != QueueDefault ||
		res.Job.Priority != 1 || string(res.Job.EncodedArgs) != `{"n":7}` || res.UniqueSkippedAsDuplicate {
		t.Fatalf("inserted %+v", res.Job)
	}
	select {
	case n := <-got:
		if n != 7 {
			t.Fatal("args", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job not worked promptly after insert")
	}
	j := waitState(t, c, res.Job.ID, JobStateCompleted, 3*time.Second)
	if j.Attempt != 1 || len(j.Errors) != 0 || j.FinalizedAt == nil || j.AttemptedAt == nil || len(j.AttemptedBy) != 1 || j.AttemptedBy[0] != c.ID() {
		t.Fatalf("completed job %+v", j)
	}
	var raw bson.M
	_ = db.Collection(CollectionJobs).FindOne(context.Background(), bson.M{"_id": j.ID}).Decode(&raw)
	if _, ok := raw["lease_owner"]; ok {
		t.Fatal("lease kept after completion", raw)
	}
	// Ids keep counting up.
	if res2 := insert(t, c, workArgs{N: 8}, nil); res2.Job.ID != res.Job.ID+1 {
		t.Fatal("ids", res.Job.ID, res2.Job.ID)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	db := testDB(t)
	var calls atomic.Int32
	c := newClient(t, db, &fnWorker{fn: func(context.Context, *Job[workArgs]) error {
		if calls.Add(1) == 1 {
			return errors.New("boom")
		}
		return nil
	}}, nil)
	start(t, c)
	res := insert(t, c, workArgs{N: 1}, nil)
	j := waitState(t, c, res.Job.ID, JobStateCompleted, 10*time.Second)
	if j.Attempt != 2 || len(j.Errors) != 1 || j.Errors[0].Error != "boom" || j.Errors[0].Attempt != 1 || calls.Load() != 2 {
		t.Fatalf("job %+v, %d calls", j, calls.Load())
	}
	// River's backoff for the first retry: 1^4 = 1 second, ±10%.
	if gap := j.AttemptedAt.Sub(j.Errors[0].At); gap < 850*time.Millisecond || gap > 3*time.Second {
		t.Fatal("first retry after", gap)
	}
	if d := defaultNextRetry(time.Now(), 3).Sub(time.Now()); d < 72*time.Second || d > 90*time.Second {
		t.Fatal("third retry backoff", d) // 3^4 = 81s ±10%
	}
}

func TestDiscardAfterMaxAttempts(t *testing.T) {
	db := testDB(t)
	var calls atomic.Int32
	c := newClient(t, db, &fnWorker{
		fn: func(context.Context, *Job[workArgs]) error {
			if calls.Add(1) == 2 {
				panic("kaput")
			}
			return errors.New("still failing")
		},
		retry: func(*Job[workArgs]) time.Time { return time.Now().Add(20 * time.Millisecond) },
	}, nil)
	start(t, c)
	res := insert(t, c, workArgs{}, &InsertOpts{MaxAttempts: 3})
	j := waitState(t, c, res.Job.ID, JobStateDiscarded, 5*time.Second)
	if j.Attempt != 3 || len(j.Errors) != 3 || j.FinalizedAt == nil || calls.Load() != 3 {
		t.Fatalf("job %+v, %d calls", j, calls.Load())
	}
	if !strings.Contains(j.Errors[1].Error, "panicked: kaput") || j.Errors[1].Trace == "" || j.Errors[2].Error != "still failing" {
		t.Fatalf("errors %+v", j.Errors)
	}
	// MaxAttempts from the args' own InsertOpts, overridden by explicit options.
	ua := uniqueArgs{ID: "max"}
	if r := insert(t, c, ua, nil); r.Job.MaxAttempts != 4 {
		t.Fatal("args opts", r.Job.MaxAttempts)
	}
	if r := insert(t, c, uniqueArgs{ID: "max2"}, &InsertOpts{MaxAttempts: 9}); r.Job.MaxAttempts != 9 {
		t.Fatal("explicit opts", r.Job.MaxAttempts)
	}
}

func TestJobCancel(t *testing.T) {
	db := testDB(t)
	c := newClient(t, db, &fnWorker{fn: func(context.Context, *Job[workArgs]) error { return JobCancel(errors.New("not wanted")) }}, nil)
	start(t, c)
	res := insert(t, c, workArgs{}, nil)
	j := waitState(t, c, res.Job.ID, JobStateCancelled, 3*time.Second)
	if j.Attempt != 1 || len(j.Errors) != 1 || !strings.Contains(j.Errors[0].Error, "not wanted") || j.FinalizedAt == nil {
		t.Fatalf("job %+v", j)
	}
}

// uniqueArgs is unique by args among the default states and carries MaxAttempts 4.
type uniqueArgs struct {
	ID string `json:"id"`
}

func (uniqueArgs) Kind() string { return "unique_args" }
func (uniqueArgs) InsertOpts() InsertOpts {
	return InsertOpts{MaxAttempts: 4, UniqueOpts: UniqueOpts{ByArgs: true}}
}

type periodArgs struct {
	Note string `json:"note"`
}

func (periodArgs) Kind() string { return "period" }
func (periodArgs) InsertOpts() InsertOpts {
	return InsertOpts{UniqueOpts: UniqueOpts{ByPeriod: time.Hour}}
}

func TestUniqueByArgs(t *testing.T) {
	db := testDB(t)
	c := newClient(t, db, nil, func(cfg *Config) { cfg.Queues = nil })
	a1 := insert(t, c, uniqueArgs{ID: "a"}, nil)
	a2 := insert(t, c, uniqueArgs{ID: "a"}, nil)
	b := insert(t, c, uniqueArgs{ID: "b"}, nil)
	if a1.UniqueSkippedAsDuplicate || !a2.UniqueSkippedAsDuplicate || a2.Job.ID != a1.Job.ID || b.UniqueSkippedAsDuplicate || b.Job.ID == a1.Job.ID {
		t.Fatal(a1, a2, b)
	}
	if a1.Job.UniqueKey == "" || a1.Job.UniqueKey == b.Job.UniqueKey || len(a1.Job.UniqueStates) != 6 {
		t.Fatalf("%+v", a1.Job)
	}
	if n := countKind(t, db, "unique_args"); n != 2 {
		t.Fatal("stored", n)
	}
	// Key order in the encoded args does not matter.
	k1, _ := (&UniqueOpts{ByArgs: true}).uniqueKey("k", []byte(`{"b":1,"a":2}`), "default", time.Now())
	k2, _ := (&UniqueOpts{ByArgs: true}).uniqueKey("k", []byte(`{"a":2,"b":1}`), "default", time.Now())
	if k1 != k2 {
		t.Fatal("args key depends on key order")
	}
}

func TestUniqueByPeriod(t *testing.T) {
	db := testDB(t)
	c := newClient(t, db, nil, func(cfg *Config) { cfg.Queues = nil })
	hour := time.Now().Add(48 * time.Hour).Truncate(time.Hour)
	first := insert(t, c, periodArgs{Note: "one"}, &InsertOpts{ScheduledAt: hour.Add(time.Minute)})
	same := insert(t, c, periodArgs{Note: "two"}, &InsertOpts{ScheduledAt: hour.Add(50 * time.Minute)}) // args don't count
	next := insert(t, c, periodArgs{Note: "one"}, &InsertOpts{ScheduledAt: hour.Add(61 * time.Minute)})
	if first.UniqueSkippedAsDuplicate || first.Job.State != JobStateScheduled || !same.UniqueSkippedAsDuplicate || same.Job.ID != first.Job.ID ||
		next.UniqueSkippedAsDuplicate {
		t.Fatal(first.Job, same, next.Job)
	}
	// Without ScheduledAt the bucket is the current period: two inserts now are one job.
	n1 := insert(t, c, periodArgs{}, nil)
	n2 := insert(t, c, periodArgs{}, nil)
	if n1.UniqueSkippedAsDuplicate || !n2.UniqueSkippedAsDuplicate {
		t.Fatal(n1, n2)
	}
	if _, err := c.Insert(context.Background(), workArgs{}, &InsertOpts{UniqueOpts: UniqueOpts{ByPeriod: time.Millisecond}}); err == nil {
		t.Fatal("ByPeriod under a second accepted")
	}
}

func TestUniqueByState(t *testing.T) {
	db := testDB(t)
	var calls atomic.Int32
	c := newClient(t, db, &fnWorker{fn: func(_ context.Context, j *Job[workArgs]) error {
		calls.Add(1)
		if j.Args.N == 99 {
			return errors.New("fails") // MaxAttempts 1 → discarded
		}
		return nil
	}}, nil)
	start(t, c)
	ctx := context.Background()
	// As drive_upload: unique among waiting/running jobs only; a completed one does not block a new one.
	active := &InsertOpts{UniqueOpts: UniqueOpts{ByArgs: true, ByState: []JobState{
		JobStateAvailable, JobStatePending, JobStateRunning, JobStateRetryable, JobStateScheduled}}}
	r1 := insert(t, c, workArgs{N: 1}, active)
	waitState(t, c, r1.Job.ID, JobStateCompleted, 3*time.Second)
	r2 := insert(t, c, workArgs{N: 1}, active)
	if r2.UniqueSkippedAsDuplicate || r2.Job.ID == r1.Job.ID {
		t.Fatal("a completed job blocked a new one outside its unique states", r2)
	}
	waitState(t, c, r2.Job.ID, JobStateCompleted, 3*time.Second)
	// While one waits (scheduled in the future), a duplicate is refused.
	later := &InsertOpts{ScheduledAt: time.Now().Add(time.Hour), UniqueOpts: active.UniqueOpts}
	w1 := insert(t, c, workArgs{N: 2}, later)
	w2 := insert(t, c, workArgs{N: 2}, active)
	if !w2.UniqueSkippedAsDuplicate || w2.Job.ID != w1.Job.ID || w2.Job.State != JobStateScheduled {
		t.Fatal("waiting duplicate", w1.Job, w2)
	}
	// Default states: a completed job still blocks; a discarded one does not.
	def := &InsertOpts{MaxAttempts: 1, UniqueOpts: UniqueOpts{ByArgs: true}}
	d1 := insert(t, c, workArgs{N: 3}, def)
	waitState(t, c, d1.Job.ID, JobStateCompleted, 3*time.Second)
	if d2 := insert(t, c, workArgs{N: 3}, def); !d2.UniqueSkippedAsDuplicate || d2.Job.ID != d1.Job.ID {
		t.Fatal("completed job did not block under the default states", d2)
	}
	x1 := insert(t, c, workArgs{N: 99}, def)
	waitState(t, c, x1.Job.ID, JobStateDiscarded, 3*time.Second)
	if x2 := insert(t, c, workArgs{N: 99}, def); x2.UniqueSkippedAsDuplicate {
		t.Fatal("discarded job blocked a new one", x2)
	}
	// ByState must hold the required states.
	_, err := c.Insert(ctx, workArgs{N: 4}, &InsertOpts{UniqueOpts: UniqueOpts{ByState: []JobState{JobStateAvailable, JobStatePending, JobStateScheduled}}})
	if err == nil || !strings.Contains(err.Error(), "missing: running") {
		t.Fatal("ByState without running", err)
	}
}

// A unique job that gave up its lock while retryable (retryable not among its states) and finds the key
// taken when its retry comes is discarded, as River's scheduler does.
func TestUniqueRetryableConflict(t *testing.T) {
	db := testDB(t)
	var first atomic.Bool
	c := newClient(t, db, &fnWorker{
		fn: func(_ context.Context, j *Job[workArgs]) error {
			if j.Args.N == 5 && first.CompareAndSwap(false, true) {
				return errors.New("first try fails")
			}
			return nil
		},
		retry: func(*Job[workArgs]) time.Time { return time.Now().Add(1500 * time.Millisecond) }, // > scheduler interval → retryable
	}, nil)
	start(t, c)
	opts := &InsertOpts{UniqueOpts: UniqueOpts{ByArgs: true, ByState: []JobState{JobStateAvailable, JobStatePending, JobStateRunning, JobStateScheduled}}}
	r1 := insert(t, c, workArgs{N: 5}, opts)
	waitState(t, c, r1.Job.ID, JobStateRetryable, 3*time.Second)
	// The retryable job no longer holds the key: a new one may be inserted (scheduled later, so it waits).
	r2 := insert(t, c, workArgs{N: 5}, &InsertOpts{ScheduledAt: time.Now().Add(time.Hour), UniqueOpts: opts.UniqueOpts})
	if r2.UniqueSkippedAsDuplicate {
		t.Fatal("retryable job outside its unique states still blocked")
	}
	waitState(t, c, r1.Job.ID, JobStateDiscarded, 5*time.Second)
	var raw bson.M
	_ = db.Collection(CollectionJobs).FindOne(context.Background(), bson.M{"_id": r1.Job.ID}).Decode(&raw)
	if raw["unique_conflict"] != "scheduler_discarded" {
		t.Fatal(raw)
	}
}

// testTx is a TxBinder on a plain mongo session, standing in for store.Q.
type testTx struct {
	db   *mongo.Database
	sess *mongo.Session
}

func (x testTx) Bind(ctx context.Context) context.Context {
	return mongo.NewSessionContext(ctx, x.sess)
}
func (x testTx) Database() *mongo.Database { return x.db }

func TestInsertTx(t *testing.T) {
	db := testDB(t)
	got := make(chan int, 4)
	c := newClient(t, db, &fnWorker{fn: func(_ context.Context, j *Job[workArgs]) error { got <- j.Args.N; return nil }}, nil)
	start(t, c)
	ctx := context.Background()
	inTx := func(n int, commit bool) *InsertResult {
		sess, err := db.Client().StartSession()
		if err != nil {
			t.Fatal(err)
		}
		defer sess.EndSession(ctx)
		if err := sess.StartTransaction(); err != nil {
			t.Fatal(err)
		}
		tx := testTx{db: db, sess: sess}
		// Something else the transaction writes, to check it commits or aborts together with the job.
		if _, err := db.Collection("tx_probe").InsertOne(tx.Bind(ctx), bson.M{"n": n}); err != nil {
			t.Fatal(err)
		}
		res, err := c.InsertTx(ctx, tx, workArgs{N: n}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Not visible outside the transaction, so not worked yet.
		time.Sleep(300 * time.Millisecond)
		if _, err := c.JobGet(ctx, res.Job.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("job visible before commit", err)
		}
		if commit {
			err = sess.CommitTransaction(ctx)
		} else {
			err = sess.AbortTransaction(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	committed := inTx(1, true)
	select {
	case n := <-got:
		if n != 1 {
			t.Fatal(n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("committed job not worked")
	}
	waitState(t, c, committed.Job.ID, JobStateCompleted, 3*time.Second)

	aborted := inTx(2, false)
	time.Sleep(500 * time.Millisecond)
	if _, err := c.JobGet(ctx, aborted.Job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("aborted job exists", err)
	}
	select {
	case n := <-got:
		t.Fatal("aborted job worked", n)
	default:
	}
	if n, _ := db.Collection("tx_probe").CountDocuments(ctx, bson.M{}); n != 1 {
		t.Fatal("probe rows", n)
	}

	// A unique job inside a transaction: the duplicate is found, not an error that aborts the transaction.
	sess, _ := db.Client().StartSession()
	defer sess.EndSession(ctx)
	_ = sess.StartTransaction()
	tx := testTx{db: db, sess: sess}
	u1, err := c.InsertTx(ctx, tx, uniqueArgs{ID: "tx"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := c.InsertTx(ctx, tx, uniqueArgs{ID: "tx"}, nil)
	if err != nil || !u2.UniqueSkippedAsDuplicate || u2.Job.ID != u1.Job.ID {
		t.Fatal("duplicate in tx", u2, err)
	}
	if err := sess.CommitTransaction(ctx); err != nil {
		t.Fatal("commit after a skipped duplicate", err)
	}
	if n := countKind(t, db, "unique_args"); n != 1 {
		t.Fatal("unique jobs stored", n)
	}
}

type tickArgs struct{}

func (tickArgs) Kind() string { return "tick" }

type tickWorker struct {
	WorkerDefaults[tickArgs]
	n *atomic.Int32
}

func (w *tickWorker) Work(context.Context, *Job[tickArgs]) error { w.n.Add(1); return nil }

func TestPeriodicRunOnStart(t *testing.T) {
	db := testDB(t)
	var works atomic.Int32
	var ticks atomic.Int32
	c := newClient(t, db, &fnWorker{fn: func(context.Context, *Job[workArgs]) error { works.Add(1); return nil }}, func(cfg *Config) {
		AddWorker(cfg.Workers, &tickWorker{n: &ticks})
		cfg.PeriodicJobs = []*PeriodicJob{
			NewPeriodicJob(PeriodicInterval(time.Hour), func() (JobArgs, *InsertOpts) { return workArgs{N: 1}, nil }, &PeriodicJobOpts{RunOnStart: true}),
			NewPeriodicJob(PeriodicInterval(200*time.Millisecond), func() (JobArgs, *InsertOpts) { return tickArgs{}, nil }, nil),
			NewPeriodicJob(PeriodicInterval(100*time.Millisecond), func() (JobArgs, *InsertOpts) { return nil, nil }, nil), // skipped
		}
	})
	t0 := time.Now()
	start(t, c)
	waitFor(t, 2*time.Second, "RunOnStart job", func() bool { return works.Load() == 1 })
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Fatal("RunOnStart took", d)
	}
	waitFor(t, 3*time.Second, "periodic runs", func() bool { return ticks.Load() >= 4 })
	var firstTick struct {
		At time.Time `bson:"scheduled_at"`
	}
	_ = db.Collection(CollectionJobs).FindOne(context.Background(), bson.M{"kind": "tick"}, options.FindOne().SetSort(bson.M{"_id": 1})).Decode(&firstTick)
	if d := firstTick.At.Sub(t0); d < 150*time.Millisecond {
		t.Fatal("a periodic job without RunOnStart ran at once:", d)
	}
	if works.Load() != 1 {
		t.Fatal("hourly job ran", works.Load())
	}
	var raw bson.M
	_ = db.Collection(CollectionJobs).FindOne(context.Background(), bson.M{"kind": "tick"}).Decode(&raw)
	if raw["periodic"] != true {
		t.Fatal("periodic flag", raw)
	}
}

func TestPeriodicSingleLeader(t *testing.T) {
	db := testDB(t)
	var ticks atomic.Int32
	mk := func() *Client {
		return newClient(t, db, nil, func(cfg *Config) {
			AddWorker(cfg.Workers, &tickWorker{n: &ticks})
			cfg.PeriodicJobs = []*PeriodicJob{
				NewPeriodicJob(PeriodicInterval(200*time.Millisecond), func() (JobArgs, *InsertOpts) { return tickArgs{}, nil }, &PeriodicJobOpts{RunOnStart: true}),
			}
		})
	}
	a, b := mk(), mk()
	start(t, a)
	start(t, b)
	waitFor(t, 2*time.Second, "a leader", func() bool { return a.IsLeader() || b.IsLeader() })
	time.Sleep(2 * time.Second)
	if a.IsLeader() == b.IsLeader() {
		t.Fatal("leaders:", a.IsLeader(), b.IsLeader())
	}
	// One leader inserts about 2s/200ms = 10 (+1 on start); two would insert twice as many.
	if n := countKind(t, db, "tick"); n < 8 || n > 14 {
		t.Fatal("periodic jobs inserted in 2s:", n)
	}
	// Both clients work jobs, but only the leader inserts them. When it stops, the other takes over.
	leader, other := a, b
	if b.IsLeader() {
		leader, other = b, a
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := leader.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "the other client to lead", other.IsLeader)
	before := countKind(t, db, "tick")
	time.Sleep(time.Second)
	if after := countKind(t, db, "tick"); after-before < 3 || after-before > 8 {
		t.Fatal("periodic jobs after takeover:", after-before)
	}
	var lease bson.M
	_ = db.Collection(CollectionMeta).FindOne(context.Background(), bson.M{"_id": "leader"}).Decode(&lease)
	if lease["leader_id"] != other.ID() {
		t.Fatal("lease", lease)
	}
}

func TestStopWaitsForRunningJob(t *testing.T) {
	db := testDB(t)
	started := make(chan struct{})
	ctxErr := make(chan error, 1)
	c := newClient(t, db, &fnWorker{fn: func(ctx context.Context, _ *Job[workArgs]) error {
		close(started)
		time.Sleep(800 * time.Millisecond) // ignores ctx on purpose
		ctxErr <- ctx.Err()
		return nil
	}}, nil)
	start(t, c)
	res := insert(t, c, workArgs{}, nil)
	<-started
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 600*time.Millisecond {
		t.Fatal("Stop returned before the job finished", d)
	}
	if j := get(t, c, res.Job.ID); j.State != JobStateCompleted {
		t.Fatal("after Stop", j.State)
	}
	if e := <-ctxErr; e != nil {
		t.Fatal("a graceful stop cancelled the job's context", e)
	}
	select {
	case <-c.Stopped():
	default:
		t.Fatal("Stopped not closed")
	}
	// A job inserted after Stop is not worked; Start again works it.
	r2 := insert(t, c, workArgs{N: 2}, nil)
	time.Sleep(300 * time.Millisecond)
	if get(t, c, r2.Job.ID).State != JobStateAvailable {
		t.Fatal("worked while stopped")
	}
}

func TestStopAndCancel(t *testing.T) {
	db := testDB(t)
	started := make(chan struct{}, 1)
	c := newClient(t, db, &fnWorker{fn: func(ctx context.Context, _ *Job[workArgs]) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}, nil)
	start(t, c)
	res := insert(t, c, workArgs{}, nil)
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.StopAndCancel(ctx); err != nil {
		t.Fatal(err)
	}
	// Interrupted by the stop: back to available, the try not counted.
	if j := get(t, c, res.Job.ID); j.State != JobStateAvailable || j.Attempt != 0 || len(j.Errors) != 0 {
		t.Fatalf("after StopAndCancel %+v", j)
	}
}

func TestWorkerTimeout(t *testing.T) {
	db := testDB(t)
	w := &fnWorker{timeout: 150 * time.Millisecond, fn: func(ctx context.Context, _ *Job[workArgs]) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}}
	c := newClient(t, db, w, nil)
	start(t, c)
	t0 := time.Now()
	res := insert(t, c, workArgs{}, &InsertOpts{MaxAttempts: 1})
	j := waitState(t, c, res.Job.ID, JobStateDiscarded, 3*time.Second)
	if len(j.Errors) != 1 || !strings.Contains(j.Errors[0].Error, "deadline exceeded") || time.Since(t0) > 2*time.Second {
		t.Fatalf("errors %+v after %s", j.Errors, time.Since(t0))
	}
	// Worker timeout 0 → the client's JobTimeout.
	w.timeout = 0
	c2 := newClient(t, db, &fnWorker{fn: w.fn}, func(cfg *Config) { cfg.JobTimeout = 100 * time.Millisecond })
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	start(t, c2)
	res2 := insert(t, c2, workArgs{}, &InsertOpts{MaxAttempts: 1})
	j2 := waitState(t, c2, res2.Job.ID, JobStateDiscarded, 3*time.Second)
	if !strings.Contains(j2.Errors[0].Error, "deadline exceeded") {
		t.Fatal(j2.Errors)
	}
}

// A running job whose server died (lease not renewed) is retried; a live one keeps its lease.
func TestRescueAndLease(t *testing.T) {
	db := testDB(t)
	var calls atomic.Int32
	slow := make(chan struct{})
	c := newClient(t, db, &fnWorker{fn: func(_ context.Context, j *Job[workArgs]) error {
		calls.Add(1)
		if j.Args.N == 2 {
			<-slow
		}
		return nil
	}}, func(cfg *Config) { cfg.JobLease = 600 * time.Millisecond })
	ctx := context.Background()
	// A job claimed by a server that then died.
	res := insert(t, c, workArgs{N: 1}, nil)
	past := time.Now().Add(-time.Second)
	if _, err := db.Collection(CollectionJobs).UpdateOne(ctx, bson.M{"_id": res.Job.ID}, bson.M{
		"$set": bson.M{"state": JobStateRunning, "attempt": 1, "attempted_at": past, "lease_owner": "dead", "lease_until": past}}); err != nil {
		t.Fatal(err)
	}
	start(t, c)
	j := waitState(t, c, res.Job.ID, JobStateCompleted, 5*time.Second)
	if j.Attempt != 2 || len(j.Errors) != 1 || j.Errors[0].Error != "Stuck job rescued by Rescuer" || calls.Load() != 1 {
		t.Fatalf("rescued job %+v", j)
	}
	// A job running longer than its lease is renewed, never rescued.
	long := insert(t, c, workArgs{N: 2}, nil)
	waitState(t, c, long.Job.ID, JobStateRunning, 2*time.Second)
	time.Sleep(1500 * time.Millisecond)
	close(slow)
	j2 := waitState(t, c, long.Job.ID, JobStateCompleted, 2*time.Second)
	if j2.Attempt != 1 || len(j2.Errors) != 0 {
		t.Fatalf("renewed job %+v", j2)
	}
}

func TestCleaner(t *testing.T) {
	db := testDB(t)
	c := newClient(t, db, &fnWorker{fn: func(context.Context, *Job[workArgs]) error { return nil }}, nil)
	ctx := context.Background()
	now := time.Now()
	docs := map[int64]bson.M{
		101: {"state": JobStateCompleted, "finalized_at": now.Add(-25 * time.Hour)},     // gone
		102: {"state": JobStateCompleted, "finalized_at": now.Add(-time.Hour)},          // kept
		103: {"state": JobStateCancelled, "finalized_at": now.Add(-25 * time.Hour)},     // gone
		104: {"state": JobStateDiscarded, "finalized_at": now.Add(-25 * time.Hour)},     // kept (7 days)
		105: {"state": JobStateDiscarded, "finalized_at": now.Add(-8 * 24 * time.Hour)}, // gone
	}
	for id, d := range docs {
		d["_id"], d["kind"], d["queue"] = id, "old", QueueDefault
		if _, err := db.Collection(CollectionJobs).InsertOne(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	start(t, c)
	waitFor(t, 3*time.Second, "cleanup", func() bool { return countKind(t, db, "old") == 2 })
	for _, id := range []int64{102, 104} {
		if n, _ := db.Collection(CollectionJobs).CountDocuments(ctx, bson.M{"_id": id}); n != 1 {
			t.Fatal("deleted", id)
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	db := testDB(t)
	var running, peak atomic.Int32
	var mu sync.Mutex
	done := 0
	c := newClient(t, db, &fnWorker{fn: func(context.Context, *Job[workArgs]) error {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		running.Add(-1)
		mu.Lock()
		done++
		mu.Unlock()
		return nil
	}}, func(cfg *Config) { cfg.Queues = map[string]QueueConfig{QueueDefault: {MaxWorkers: 3}} })
	for i := range 12 {
		insert(t, c, workArgs{N: i}, nil)
	}
	start(t, c)
	waitFor(t, 5*time.Second, "all jobs", func() bool { mu.Lock(); defer mu.Unlock(); return done == 12 })
	if peak.Load() != 3 {
		t.Fatal("peak concurrency", peak.Load())
	}
}

func TestConfigErrors(t *testing.T) {
	db := testDB(t)
	if _, err := NewClient(db, &Config{Queues: map[string]QueueConfig{QueueDefault: {MaxWorkers: 1}}}); err == nil {
		t.Fatal("queues without workers")
	}
	if _, err := NewClient(db, &Config{Queues: map[string]QueueConfig{QueueDefault: {}}, Workers: NewWorkers()}); err == nil {
		t.Fatal("MaxWorkers 0")
	}
	c, err := NewClient(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("an insert-only client started")
	}
	if _, err := c.Insert(context.Background(), workArgs{}, &InsertOpts{Priority: 5}); err == nil {
		t.Fatal("priority 5")
	}
	w := NewWorkers()
	AddWorker(w, &fnWorker{})
	if err := AddWorkerSafely(w, &fnWorker{}); err == nil {
		t.Fatal("duplicate worker")
	}
}
