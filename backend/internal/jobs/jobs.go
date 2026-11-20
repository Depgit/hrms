// Package jobs is a background job queue stored in MongoDB (no Redis). It replaces River, which
// needed PostgreSQL, and mirrors the parts of River's API the app used so code written for River
// ports mechanically:
//
//   - job args are plain structs with a Kind (and optional InsertOpts);
//   - a job can be inserted in the caller's transaction and then exists only if it commits (InsertTx);
//   - failed jobs retry with River's backoff (attempt^4 seconds, ±10% jitter) up to MaxAttempts
//     (default 25) and are then discarded; a job's context times out after a minute unless its worker
//     says otherwise;
//   - unique jobs (by args, by period, among chosen states) are enforced by a unique index;
//   - periodic jobs are inserted only by the elected leader when several servers run;
//   - jobs whose server died (their lease was not renewed) are rescued, finished jobs are cleaned up.
//
// Storage: the collection job_queue holds one document per job; job_queue_meta holds the leader
// lease and the job id sequence. NewClient creates the indexes (EnsureIndexes).
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Collection names. Diagnostics and tests may read job_queue directly (state, kind, attempt, errors …).
const (
	CollectionJobs = "job_queue"
	CollectionMeta = "job_queue_meta"
)

// Defaults, as in River.
const (
	QueueDefault       = "default"
	PriorityDefault    = 1
	MaxAttemptsDefault = 25
	JobTimeoutDefault  = time.Minute
)

// JobState is the state of a job, with River's names and values.
type JobState string

const (
	JobStateAvailable JobState = "available" // ready to be worked once scheduled_at has passed
	JobStateCancelled JobState = "cancelled" // the worker returned JobCancel(…); never retried
	JobStateCompleted JobState = "completed" // worked successfully
	JobStateDiscarded JobState = "discarded" // failed MaxAttempts times
	JobStatePending   JobState = "pending"   // inserted with InsertOpts.Pending; never worked until moved
	JobStateRetryable JobState = "retryable" // failed; waits for its next try at scheduled_at
	JobStateRunning   JobState = "running"   // claimed by a worker
	JobStateScheduled JobState = "scheduled" // inserted with a future ScheduledAt
)

// JobStates returns every state.
func JobStates() []JobState {
	return []JobState{JobStateAvailable, JobStateCancelled, JobStateCompleted, JobStateDiscarded,
		JobStatePending, JobStateRetryable, JobStateRunning, JobStateScheduled}
}

// UniqueOptsByStateDefault is the set of states uniqueness is enforced among when UniqueOpts.ByState is
// empty: every state except cancelled and discarded (River's default).
func UniqueOptsByStateDefault() []JobState {
	return []JobState{JobStateAvailable, JobStateCompleted, JobStatePending, JobStateRetryable, JobStateRunning, JobStateScheduled}
}

// JobArgs is a job's payload: a struct that encodes to a JSON object, with a kind naming its worker.
type JobArgs interface {
	Kind() string
}

// JobArgsWithInsertOpts lets job args carry default insert options. Options passed to Insert win.
type JobArgsWithInsertOpts interface {
	InsertOpts() InsertOpts
}

// InsertOpts are the options for one insert. Zero values fall back to the args' InsertOpts, then to the
// client defaults.
type InsertOpts struct {
	// MaxAttempts is the number of tries (the first run included) before the job is discarded. Default 25.
	MaxAttempts int
	// Pending inserts the job in the pending state: it is never worked until something moves it.
	Pending bool
	// Priority 1 (highest, default) to 4. Higher-priority available jobs are claimed first.
	Priority int
	// Queue is the queue the job goes to. Default QueueDefault.
	Queue string
	// ScheduledAt is when the job may run at the earliest. Zero = now.
	ScheduledAt time.Time
	// UniqueOpts makes the job unique; the zero value means not unique.
	UniqueOpts UniqueOpts
}

// UniqueOpts makes a job unique, as in River. As soon as one option is set, the kind counts towards
// uniqueness (unless ExcludeKind); each further option adds a dimension.
type UniqueOpts struct {
	// ByArgs: the encoded args (all top-level keys, sorted) are part of the unique key.
	ByArgs bool
	// ByPeriod: the job's scheduled time (or now), rounded down to a multiple of the period in UTC, is
	// part of the key, so one such job can exist per period. At least one second.
	ByPeriod time.Duration
	// ByQueue: the queue is part of the key.
	ByQueue bool
	// ByState: uniqueness holds only among jobs currently in these states. Empty = the default
	// (UniqueOptsByStateDefault). When set it must contain available, pending, running and scheduled.
	ByState []JobState
	// ExcludeKind leaves the kind out of the key (needs ByArgs, ByQueue or ByPeriod).
	ExcludeKind bool
}

func (o *UniqueOpts) isEmpty() bool {
	return !o.ByArgs && o.ByPeriod == 0 && !o.ByQueue && o.ByState == nil && !o.ExcludeKind
}

var requiredUniqueStates = []JobState{JobStateAvailable, JobStatePending, JobStateRunning, JobStateScheduled}

func (o *UniqueOpts) validate() error {
	if o.isEmpty() {
		return nil
	}
	if o.ByPeriod != 0 && o.ByPeriod < time.Second {
		return errors.New("UniqueOpts.ByPeriod should not be less than 1 second")
	}
	if o.ExcludeKind && !o.ByArgs && !o.ByQueue && o.ByPeriod == 0 {
		return errors.New("UniqueOpts.ExcludeKind requires ByArgs, ByQueue, or ByPeriod")
	}
	all := JobStates()
	for _, s := range o.ByState {
		if !slices.Contains(all, s) {
			return fmt.Errorf("UniqueOpts.ByState contains invalid state %q", s)
		}
	}
	if len(o.ByState) == 0 {
		return nil
	}
	var missing []string
	for _, s := range requiredUniqueStates {
		if !slices.Contains(o.ByState, s) {
			missing = append(missing, string(s))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("UniqueOpts.ByState must contain all required states, missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (o *UniqueOpts) states() []JobState {
	if len(o.ByState) > 0 {
		return slices.Clone(o.ByState)
	}
	return UniqueOptsByStateDefault()
}

// uniqueKey builds the key the way River does (kind, sorted args, UTC period start, queue) and hashes it.
func (o *UniqueOpts) uniqueKey(kind string, encodedArgs []byte, queue string, scheduledAt time.Time) (string, error) {
	var sb strings.Builder
	if !o.ExcludeKind {
		sb.WriteString("&kind=" + kind)
	}
	if o.ByArgs {
		sorted, err := sortedArgs(encodedArgs)
		if err != nil {
			return "", err
		}
		sb.WriteString("&args=")
		sb.Write(sorted)
	}
	if o.ByPeriod != 0 {
		sb.WriteString("&period=" + scheduledAt.Truncate(o.ByPeriod).UTC().Format(time.RFC3339))
	}
	if o.ByQueue {
		sb.WriteString("&queue=" + queue)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:]), nil
}

// sortedArgs re-encodes a JSON object with its top-level keys sorted (values as they were).
func sortedArgs(encoded []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(encoded))
	if trimmed == "" || trimmed == "[]" {
		return []byte("{}"), nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &m); err != nil {
		return nil, errors.New("unique args must encode a JSON object")
	}
	return json.Marshal(m) // encoding/json writes map keys sorted
}

// AttemptError is one failed try, kept on the job.
type AttemptError struct {
	At      time.Time `bson:"at" json:"at"`
	Attempt int       `bson:"attempt" json:"attempt"`
	Error   string    `bson:"error" json:"error"`
	Trace   string    `bson:"trace,omitempty" json:"trace,omitempty"`
}

// JobRow is a stored job.
type JobRow struct {
	ID           int64
	Attempt      int // tries started so far (1 while the first try runs)
	AttemptedAt  *time.Time
	AttemptedBy  []string // client ids, most recent last
	CreatedAt    time.Time
	EncodedArgs  []byte // the args as JSON
	Errors       []AttemptError
	FinalizedAt  *time.Time // set on completed, cancelled and discarded
	Kind         string
	MaxAttempts  int
	Priority     int
	Queue        string
	ScheduledAt  time.Time
	State        JobState
	UniqueKey    string // hex sha256 of the unique key; "" when not unique
	UniqueStates []JobState
}

// Job is a stored job with its decoded args, as handed to a worker.
type Job[T JobArgs] struct {
	*JobRow
	Args T
}

// InsertResult is what Insert and InsertTx return.
type InsertResult struct {
	Job *JobRow
	// UniqueSkippedAsDuplicate is true when a unique job like this one already existed; Job is that job.
	UniqueSkippedAsDuplicate bool
}

// JobCancelError is returned by JobCancel.
type JobCancelError struct{ err error }

func (e *JobCancelError) Error() string {
	if e.err == nil {
		return "jobs: job cancelled"
	}
	return "jobs: job cancelled: " + e.err.Error()
}

func (e *JobCancelError) Unwrap() error { return e.err }

// JobCancel, returned from Work, cancels the job: it is not retried, whatever its attempts.
func JobCancel(err error) error { return &JobCancelError{err: err} }

// ErrNotFound is returned by JobGet for an unknown id.
var ErrNotFound = errors.New("jobs: job not found")
