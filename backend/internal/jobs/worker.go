package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Worker works jobs of one kind (the kind of T).
type Worker[T JobArgs] interface {
	// NextRetry returns when to retry the job after a failure; the zero time = the default backoff.
	NextRetry(job *Job[T]) time.Time
	// Timeout returns how long Work may run before its context is cancelled: 0 = the client's
	// JobTimeout (default 1 minute), -1 = no timeout.
	Timeout(job *Job[T]) time.Duration
	// Work does the job. An error (or a panic) fails this try; JobCancel(err) cancels the job.
	Work(ctx context.Context, job *Job[T]) error
}

// WorkerDefaults gives a worker the default NextRetry and Timeout; embed it and write Work.
type WorkerDefaults[T JobArgs] struct{}

// NextRetry returns the zero time: the client's default backoff applies.
func (WorkerDefaults[T]) NextRetry(*Job[T]) time.Time { return time.Time{} }

// Timeout returns 0: the client's JobTimeout applies.
func (WorkerDefaults[T]) Timeout(*Job[T]) time.Duration { return 0 }

// Workers is the set of workers a client runs, one per kind.
type Workers struct {
	byKind map[string]func(row *JobRow) workUnit
}

// NewWorkers returns an empty set; add workers with AddWorker.
func NewWorkers() *Workers {
	return &Workers{byKind: map[string]func(row *JobRow) workUnit{}}
}

// AddWorker registers a worker for the kind of T. It panics when the kind is empty or already taken.
func AddWorker[T JobArgs](workers *Workers, worker Worker[T]) {
	if err := AddWorkerSafely(workers, worker); err != nil {
		panic(err)
	}
}

// AddWorkerSafely is AddWorker returning an error instead of panicking.
func AddWorkerSafely[T JobArgs](workers *Workers, worker Worker[T]) error {
	if workers == nil || worker == nil {
		return errors.New("jobs: AddWorker needs a Workers set and a worker")
	}
	var args T
	kind := args.Kind()
	if kind == "" {
		return errors.New("jobs: a job kind must not be empty")
	}
	if _, dup := workers.byKind[kind]; dup {
		return fmt.Errorf("jobs: worker for kind %q is already registered", kind)
	}
	workers.byKind[kind] = func(row *JobRow) workUnit { return &typedWorkUnit[T]{worker: worker, row: row} }
	return nil
}

// workUnit is one job bound to its worker, with the args type erased.
type workUnit interface {
	unmarshal() error
	work(ctx context.Context) error
	timeout() time.Duration
	nextRetry() time.Time
}

type typedWorkUnit[T JobArgs] struct {
	worker Worker[T]
	row    *JobRow
	job    *Job[T]
}

func (u *typedWorkUnit[T]) unmarshal() error {
	u.job = &Job[T]{JobRow: u.row}
	if err := json.Unmarshal(u.row.EncodedArgs, &u.job.Args); err != nil {
		return fmt.Errorf("jobs: decode args of %s job %d: %w", u.row.Kind, u.row.ID, err)
	}
	return nil
}

func (u *typedWorkUnit[T]) work(ctx context.Context) error { return u.worker.Work(ctx, u.job) }
func (u *typedWorkUnit[T]) timeout() time.Duration         { return u.worker.Timeout(u.job) }
func (u *typedWorkUnit[T]) nextRetry() time.Time           { return u.worker.NextRetry(u.job) }

// PeriodicSchedule gives the next run time after a given time.
type PeriodicSchedule interface {
	Next(current time.Time) time.Time
}

type periodicInterval struct{ d time.Duration }

func (p periodicInterval) Next(t time.Time) time.Time { return t.Add(p.d) }

// PeriodicInterval runs a periodic job every d.
func PeriodicInterval(d time.Duration) PeriodicSchedule { return periodicInterval{d: d} }

// PeriodicJobConstructor returns the args (and optional insert options) of the next run; nil args skip it.
type PeriodicJobConstructor func() (JobArgs, *InsertOpts)

// PeriodicJobOpts are a periodic job's options.
type PeriodicJobOpts struct {
	// RunOnStart inserts a run as soon as a client becomes leader, besides the schedule.
	RunOnStart bool
}

// PeriodicJob is a job inserted on a schedule by the leader.
type PeriodicJob struct {
	schedule    PeriodicSchedule
	constructor PeriodicJobConstructor
	runOnStart  bool
}

// NewPeriodicJob returns a periodic job: constructor makes each run's args, schedule says when.
func NewPeriodicJob(schedule PeriodicSchedule, constructor PeriodicJobConstructor, opts *PeriodicJobOpts) *PeriodicJob {
	p := &PeriodicJob{schedule: schedule, constructor: constructor}
	if opts != nil {
		p.runOnStart = opts.RunOnStart
	}
	return p
}
