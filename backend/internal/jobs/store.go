package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// jobDoc is a job as stored in job_queue.
//
// Uniqueness: unique_key (hex sha256, kept for the job's life) and unique_states are set on unique jobs.
// unique_lock holds a copy of unique_key only while the job is in one of its unique_states; a sparse
// unique index on unique_lock is what makes the job unique (River uses a partial unique index on
// unique_key with a state bitmask for the same thing).
//
// Leases: a running job carries lease_owner (the client id) and lease_until. The owning client renews the
// lease while the job runs; a running job whose lease ran out (its server died) is rescued by the leader.
type jobDoc struct {
	ID             int64          `bson:"_id"`
	Kind           string         `bson:"kind"`
	Queue          string         `bson:"queue"`
	Args           string         `bson:"args"` // the args as JSON, exactly as encoded
	State          JobState       `bson:"state"`
	Priority       int            `bson:"priority"`
	Attempt        int            `bson:"attempt"`
	MaxAttempts    int            `bson:"max_attempts"`
	AttemptedAt    *time.Time     `bson:"attempted_at"`
	AttemptedBy    []string       `bson:"attempted_by"`
	CreatedAt      time.Time      `bson:"created_at"`
	ScheduledAt    time.Time      `bson:"scheduled_at"`
	FinalizedAt    *time.Time     `bson:"finalized_at"`
	Errors         []AttemptError `bson:"errors"`
	Periodic       bool           `bson:"periodic,omitempty"`
	UniqueKey      string         `bson:"unique_key,omitempty"`
	UniqueStates   []JobState     `bson:"unique_states,omitempty"`
	UniqueLock     string         `bson:"unique_lock,omitempty"`
	UniqueConflict string         `bson:"unique_conflict,omitempty"`
	LeaseOwner     string         `bson:"lease_owner,omitempty"`
	LeaseUntil     *time.Time     `bson:"lease_until,omitempty"`
}

func (d *jobDoc) row() *JobRow {
	return &JobRow{
		ID:           d.ID,
		Attempt:      d.Attempt,
		AttemptedAt:  d.AttemptedAt,
		AttemptedBy:  d.AttemptedBy,
		CreatedAt:    d.CreatedAt,
		EncodedArgs:  []byte(d.Args),
		Errors:       d.Errors,
		FinalizedAt:  d.FinalizedAt,
		Kind:         d.Kind,
		MaxAttempts:  d.MaxAttempts,
		Priority:     d.Priority,
		Queue:        d.Queue,
		ScheduledAt:  d.ScheduledAt,
		State:        d.State,
		UniqueKey:    d.UniqueKey,
		UniqueStates: d.UniqueStates,
	}
}

func key(k string, v any) bson.E { return bson.E{Key: k, Value: v} }

// EnsureIndexes creates the queue's indexes (and so its collections). It is idempotent; NewClient calls it.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	_, err := db.Collection(CollectionJobs).Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Claiming: available jobs of a queue in priority, scheduled time, id order.
		{Keys: bson.D{key("state", 1), key("queue", 1), key("priority", 1), key("scheduled_at", 1), key("_id", 1)},
			Options: options.Index().SetName("job_fetch")},
		// Uniqueness (see jobDoc).
		{Keys: bson.D{key("unique_lock", 1)}, Options: options.Index().SetName("job_unique_lock").SetUnique(true).SetSparse(true)},
		// The scheduler (retryable/scheduled jobs whose time has come), the cleaner and the rescuer.
		{Keys: bson.D{key("state", 1), key("scheduled_at", 1)}, Options: options.Index().SetName("job_state_scheduled_at")},
		{Keys: bson.D{key("state", 1), key("finalized_at", 1)}, Options: options.Index().SetName("job_state_finalized_at")},
		{Keys: bson.D{key("state", 1), key("lease_until", 1)}, Options: options.Index().SetName("job_state_lease_until")},
		{Keys: bson.D{key("kind", 1), key("state", 1)}, Options: options.Index().SetName("job_kind_state")},
	})
	if err != nil {
		return fmt.Errorf("jobs: create indexes: %w", err)
	}
	// The meta collection (leader lease, id sequence) only needs _id; make sure it exists so that the first
	// insert inside a transaction does not have to create it.
	if err := db.CreateCollection(ctx, CollectionMeta); err != nil {
		var ce mongo.CommandError
		if !errors.As(err, &ce) || ce.Code != 48 { // 48 NamespaceExists
			return fmt.Errorf("jobs: create %s: %w", CollectionMeta, err)
		}
	}
	return nil
}

// nextJobID hands out job ids from a counter (like River's bigserial). It must run outside any transaction,
// so that concurrent transactions inserting jobs do not conflict on the counter (callers pass a context
// without a session); ids of rolled-back inserts are skipped, as with a Postgres sequence.
func nextJobID(ctx context.Context, db *mongo.Database) (int64, error) {
	var seq struct {
		N int64 `bson:"n"`
	}
	for try := 0; ; try++ {
		err := db.Collection(CollectionMeta).FindOneAndUpdate(ctx, bson.M{"_id": "seq"}, bson.M{"$inc": bson.M{"n": int64(1)}},
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&seq)
		if err != nil && mongo.IsDuplicateKeyError(err) && try < 3 {
			continue // two first-ever upserts raced; the counter exists now
		}
		return seq.N, err
	}
}
