package api

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Background jobs run on internal/jobs, a job queue stored in MongoDB (no Redis). Jobs can be
// inserted in the same transaction as the data change that needs them, retry on failure,
// and periodic jobs are scheduled only by the elected leader when several servers run.

const maintenanceEvery = 5 * time.Minute

// MaintenanceArgs is the periodic sweep: master retirements, shift versions reaching their date,
// scheduled job changes, status by date, and approval escalation. Every step is safe to run twice.
type MaintenanceArgs struct{}

func (MaintenanceArgs) Kind() string { return "maintenance" }

func (MaintenanceArgs) InsertOpts() jobs.InsertOpts {
	// At most one sweep per period, even if two servers or a manual run insert one.
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: maintenanceEvery}}
}

type maintenanceWorker struct {
	jobs.WorkerDefaults[MaintenanceArgs]
	s *Server
}

func (w *maintenanceWorker) Work(ctx context.Context, _ *jobs.Job[MaintenanceArgs]) error {
	return w.s.runMaintenance(ctx)
}

func (s *Server) runMaintenance(ctx context.Context) error {
	// Masters whose inactive date has come: inactive_since is a date ("YYYY-MM-DD"), so a string comparison
	// with today works, and $lte never matches a null (the IS NOT NULL of the SQL).
	today := store.Today()
	for _, t := range []string{"region", "state", "city", "cluster", "location", "department", "designation", "grade", "shift", "simple_master"} {
		if _, err := s.st.DB.C(t).UpdateMany(ctx, bson.M{"status": "Active", "inactive_since": bson.M{"$lte": today}},
			bson.M{"$set": bson.M{"status": "Inactive"}}); err != nil {
			return fmt.Errorf("retire %s: %w", t, err)
		}
	}
	if _, err := ApplyShiftVersions(ctx, s.st.DB, ""); err != nil {
		return fmt.Errorf("shift versions: %w", err)
	}
	if _, err := ApplyScheduled(ctx, s.st); err != nil {
		return fmt.Errorf("scheduled changes: %w", err)
	}
	if _, err := applyScheduledPolicies(ctx, s.st); err != nil {
		return fmt.Errorf("scheduled work policy changes: %w", err)
	}
	if _, err := s.RerouteOpen(ctx); err != nil {
		return fmt.Errorf("approval reroute: %w", err)
	}
	if _, err := s.SLASweep(ctx); err != nil {
		return fmt.Errorf("approval escalation: %w", err)
	}
	if _, err := s.PfExemptSweep(ctx); err != nil { // PF exemption of joiners (pfuan.go)
		return fmt.Errorf("PF exemption: %w", err)
	}
	s.costSweepLogged(ctx)    // cost snapshots of published months not frozen yet (cost.go)
	s.hstSweepLogged(ctx)     // accommodation reminders: lease end, rent increase, check-out after exit or transfer (hst.go)
	s.hstWaitSweepLogged(ctx) // joiners waiting for a bed get one when it frees up (hst_auto.go)
	return nil
}

// StartJobs starts the job queue client: workers plus the periodic maintenance sweep.
func (s *Server) StartJobs(ctx context.Context) error {
	workers := jobs.NewWorkers()
	jobs.AddWorker(workers, &maintenanceWorker{s: s})
	jobs.AddWorker(workers, &payrollRunWorker{s: s})
	jobs.AddWorker(workers, &pullWorker{s: s})
	jobs.AddWorker(workers, &realtimeTickWorker{s: s})
	jobs.AddWorker(workers, &realtimeRunWorker{s: s})
	jobs.AddWorker(workers, &realtimeMatchWorker{s: s})
	jobs.AddWorker(workers, &driveUploadWorker{s: s})
	jobs.AddWorker(workers, &driveSweepWorker{s: s})
	jobs.AddWorker(workers, &docSweepWorker{s: s})
	jobs.AddWorker(workers, &attAlertWorker{s: s})
	jobs.AddWorker(workers, &attDevWorker{s: s})
	jobs.AddWorker(workers, &hireSweepWorker{s: s})
	jobs.AddWorker(workers, &portalWriteWorker{s: s})
	jobs.AddWorker(workers, &portalSweepWorker{s: s})
	mdmPeriodic := s.mdmStartJobs(workers) // master data sync: push delivery and the nightly reconciliation
	c, err := jobs.NewClient(s.st.DB.Database(), &jobs.Config{
		Queues:  map[string]jobs.QueueConfig{jobs.QueueDefault: {MaxWorkers: 10}},
		Workers: workers,
		PeriodicJobs: []*jobs.PeriodicJob{
			jobs.NewPeriodicJob(jobs.PeriodicInterval(maintenanceEvery),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return MaintenanceArgs{}, nil },
				&jobs.PeriodicJobOpts{RunOnStart: true}),
			// Punch pull: checks every minute, pulls when the configured interval has passed.
			jobs.NewPeriodicJob(jobs.PeriodicInterval(time.Minute),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return PullArgs{}, nil }, nil),
			// Realtime India portal: checks every minute, pulls when it is on and its interval has passed (realtime_pull.go).
			jobs.NewPeriodicJob(jobs.PeriodicInterval(time.Minute),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return RealtimeTickArgs{}, nil }, nil),
			// Google Drive: anything still waiting goes up (drive.go).
			jobs.NewPeriodicJob(jobs.PeriodicInterval(driveSweepEvery),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return DriveSweepArgs{}, nil }, nil),
			// Documents from existing employees: windows and reminders (doc_collect.go).
			jobs.NewPeriodicJob(jobs.PeriodicInterval(docSweepEvery),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return DocSweepArgs{}, nil },
				&jobs.PeriodicJobOpts{RunOnStart: true}),
			// Attendance alerts: missing punch, late, not punched in, manager digest, cut-off (att_alerts.go).
			jobs.NewPeriodicJob(jobs.PeriodicInterval(attAlertEvery),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return AttAlertArgs{}, nil }, nil),
			// Deviations beyond a work policy's allowance to HR Admins and the manager, and leave days called to work and
			// worked noted on their request (dev_allow.go, work_call.go); independent of the alert settings.
			jobs.NewPeriodicJob(jobs.PeriodicInterval(attDevEvery),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return AttDevArgs{}, nil }, nil),
			// Hiring lifecycle: offer reminders and lapse, waiting-window reminders and automatic close (hiring.go).
			jobs.NewPeriodicJob(jobs.PeriodicInterval(hireSweepEvery),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return HireSweepArgs{}, nil }, &jobs.PeriodicJobOpts{RunOnStart: true}),
			// Realtime portal: automatic writes due after changes and exits; waiting ones while sending is off (portal_auto.go).
			jobs.NewPeriodicJob(jobs.PeriodicInterval(portalSweepGap),
				func() (jobs.JobArgs, *jobs.InsertOpts) { return PortalSweepArgs{}, nil }, nil),
			mdmPeriodic[0], mdmPeriodic[1],
		},
	})
	if err != nil {
		return err
	}
	if err := c.Start(ctx); err != nil {
		return err
	}
	s.jobs = c
	s.mdmRun(ctx, c) // the employee change-stream watcher (mdm_routes.go)
	log.Printf("job queue started (maintenance every %s, punch pull checked and Google Drive uploads swept every minute)", maintenanceEvery)
	return nil
}

// StopJobs lets running jobs finish, then stops the client.
func (s *Server) StopJobs(ctx context.Context) {
	if s.jobs != nil {
		if err := s.jobs.Stop(ctx); err != nil {
			log.Printf("job queue stop: %v", err)
		}
	}
}

// Enqueue inserts a job inside the caller's transaction: it exists only if the data change commits.
// (The transaction callback may run more than once; an aborted try takes its job with it.)
func (s *Server) Enqueue(ctx context.Context, tx store.Q, args jobs.JobArgs) (*jobs.InsertResult, error) {
	if s.jobs == nil {
		return nil, fmt.Errorf("job queue is not running")
	}
	return s.jobs.InsertTx(ctx, tx, args, nil)
}
