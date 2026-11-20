package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var jsonMarshal = json.Marshal

const (
	countersColl = "counters" // {_id: sequence name, value: the last value handed out}
	locksColl    = "locks"    // {_id: advisory lock key, n: times taken}
)

// NextSeq is nextval('name'): the sequence's next value. Like a Postgres sequence it runs outside any transaction
// (a rolled-back or retried transaction leaves a gap) and starts at the sequence's START.
func NextSeq(ctx context.Context, q Q, name string) (int64, error) {
	return nextSeqBlock(ctx, q, name, 1)
}

// nextSeqBlock takes n values at once and returns the first.
func nextSeqBlock(ctx context.Context, q Q, name string, n int64) (int64, error) {
	ctx = noSession{ctx}
	coll := q.db.Collection(countersColl)
	for attempt := 0; attempt < 3; attempt++ {
		var doc struct {
			Value int64 `bson:"value"`
		}
		err := coll.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: name}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "value", Value: n}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if err == nil {
			return doc.Value - n + 1, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return 0, err
		}
		if err := ensureCounter(ctx, q.db, name); err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("sequence %s: the counter could not be created", name)
}

// ensureCounter creates a sequence's counter at its START if it does not exist.
func ensureCounter(ctx context.Context, db *mongo.Database, name string) error {
	start, ok := seqStart[name]
	if !ok {
		start = 1
	}
	_, err := db.Collection(countersColl).UpdateOne(noSession{ctx}, bson.D{{Key: "_id", Value: name}},
		bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "value", Value: start - 1}}}}, options.UpdateOne().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return err
	}
	return nil
}

// SetSeq is setval('name', last): the next NextSeq returns last+1.
func SetSeq(ctx context.Context, q Q, name string, last int64) error {
	_, err := q.db.Collection(countersColl).UpdateOne(noSession{ctx}, bson.D{{Key: "_id", Value: name}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "value", Value: last}}}}, options.UpdateOne().SetUpsert(true))
	return err
}

// AdvisoryLock is pg_advisory_xact_lock(key): it writes the key's document in the locks collection inside the
// transaction, so another transaction taking the same key conflicts and is retried after this one commits.
// Outside a transaction it does nothing (the lock would have been released at once).
func AdvisoryLock(ctx context.Context, tx Q, key string) error {
	if !tx.InTx() {
		return nil
	}
	if err := tx.failed(); err != nil {
		return err
	}
	coll := tx.db.Collection(locksColl)
	// Create the key's document first, outside the transaction, so two transactions never both insert it.
	_, err := coll.UpdateOne(noSession{ctx}, bson.D{{Key: "_id", Value: key}},
		bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "n", Value: int64(0)}}}}, options.UpdateOne().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return err
	}
	_, err = coll.UpdateOne(tx.Bind(ctx), bson.D{{Key: "_id", Value: key}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "n", Value: int64(1)}}}})
	return tx.note(err)
}
