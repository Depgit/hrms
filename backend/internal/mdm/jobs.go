package mdm

import (
	"context"
	"time"

	"github.com/saltstayz/hrms/internal/jobs"
)

// The job queue side (internal/jobs; no Redis): push delivery runs as a job, inserted every DeliverEvery by the
// leader and right after events are written; the nightly reconciliation is checked every ReconcileCheckEvery.

const (
	DeliverEvery        = 15 * time.Second
	ReconcileCheckEvery = 10 * time.Minute
)

// DeliverArgs is a push delivery run.
type DeliverArgs struct{}

func (DeliverArgs) Kind() string { return "mdm_deliver" }

func (DeliverArgs) InsertOpts() jobs.InsertOpts {
	// One run waiting or running at a time, whatever inserts it (the schedule, a kick after new events, a
	// server restart).
	return jobs.InsertOpts{MaxAttempts: 3, UniqueOpts: jobs.UniqueOpts{ByState: []jobs.JobState{
		jobs.JobStateAvailable, jobs.JobStatePending, jobs.JobStateRunning, jobs.JobStateScheduled, jobs.JobStateRetryable}}}
}

// ReconcileArgs checks whether the nightly reconciliation is due, and runs it.
type ReconcileArgs struct{}

func (ReconcileArgs) Kind() string { return "mdm_reconcile" }

func (ReconcileArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{MaxAttempts: 3, UniqueOpts: jobs.UniqueOpts{ByPeriod: ReconcileCheckEvery}}
}

type deliverWorker struct {
	jobs.WorkerDefaults[DeliverArgs]
	s *Service
}

func (w *deliverWorker) Timeout(*jobs.Job[DeliverArgs]) time.Duration { return 10 * time.Minute }

func (w *deliverWorker) Work(ctx context.Context, _ *jobs.Job[DeliverArgs]) error {
	_, err := w.s.DeliverDue(ctx)
	return err
}

type reconcileWorker struct {
	jobs.WorkerDefaults[ReconcileArgs]
	s *Service
}

func (w *reconcileWorker) Timeout(*jobs.Job[ReconcileArgs]) time.Duration { return 30 * time.Minute }

func (w *reconcileWorker) Work(ctx context.Context, _ *jobs.Job[ReconcileArgs]) error {
	_, err := w.s.ReconcileIfDue(ctx)
	return err
}

// AddWorkers registers the delivery and reconciliation workers.
func (s *Service) AddWorkers(w *jobs.Workers) {
	jobs.AddWorker(w, &deliverWorker{s: s})
	jobs.AddWorker(w, &reconcileWorker{s: s})
}

// DeliverPeriodicJob inserts a delivery run every DeliverEvery (and when a server becomes leader).
func DeliverPeriodicJob() *jobs.PeriodicJob {
	return jobs.NewPeriodicJob(jobs.PeriodicInterval(DeliverEvery), func() (jobs.JobArgs, *jobs.InsertOpts) { return DeliverArgs{}, nil },
		&jobs.PeriodicJobOpts{RunOnStart: true})
}

// ReconcilePeriodicJob checks every ReconcileCheckEvery whether the nightly reconciliation is due.
func ReconcilePeriodicJob() *jobs.PeriodicJob {
	return jobs.NewPeriodicJob(jobs.PeriodicInterval(ReconcileCheckEvery), func() (jobs.JobArgs, *jobs.InsertOpts) { return ReconcileArgs{}, nil },
		&jobs.PeriodicJobOpts{RunOnStart: true})
}

// Start wires the service to a running job client and starts the watcher: new events insert a delivery run at
// once, and the watcher works (when it holds the lease) until ctx ends or the client stops. id names this server.
func (s *Service) Start(ctx context.Context, c *jobs.Client, id string) *Watcher {
	s.SetKick(func() {
		ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.Insert(ictx, DeliverArgs{}, nil); err != nil {
			s.logf("queueing a delivery: %v", err)
		}
	})
	w := s.NewWatcher(id)
	wctx, cancel := context.WithCancel(ctx)
	stopped := c.Stopped()
	go func() {
		select {
		case <-stopped:
		case <-wctx.Done():
		}
		cancel()
	}()
	go w.Run(wctx)
	return w
}
