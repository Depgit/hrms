package store

import (
	"context"
	"errors"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A failed database operation inside a MongoDB transaction aborts the transaction on the server (as an error did
// in PostgreSQL: "current transaction is aborted"). If the code then carries on, every later operation, and the
// commit, fails with NoSuchTransaction, which the driver treats as transient and retries for up to two minutes.
// txState remembers the first failure of a transaction attempt, so later calls return that error at once and Tx
// returns it even when fn swallowed it.
type txState struct {
	mu  sync.Mutex
	err error
}

// AbortedError is returned by a call made in a transaction that an earlier failed operation already aborted.
type AbortedError struct{ Err error }

func (e *AbortedError) Error() string {
	return "the transaction was aborted by an earlier error: " + e.Err.Error()
}

func (e *AbortedError) Unwrap() error { return e.Err }

func (st *txState) failed() error {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return &AbortedError{st.err}
	}
	return nil
}

// note records an error the server returned inside the transaction. Errors of the store's own checks (the server
// never saw the write) and "no document" do not abort a transaction.
func (q Q) note(err error) error {
	if err == nil || q.st == nil || errors.Is(err, mongo.ErrNoDocuments) {
		return err
	}
	var se *Error
	if errors.As(err, &se) && se.Err == nil {
		return err
	}
	var ae *AbortedError
	if errors.As(err, &ae) {
		return err
	}
	q.st.mu.Lock()
	if q.st.err == nil {
		q.st.err = err
	}
	q.st.mu.Unlock()
	return err
}

func (q Q) failed() error { return q.st.failed() }

// ---------- the public collection API ----------

// Find returns every matching document as a Row (an empty, non-nil slice when none match). Filters are bson.M or
// bson.D; a top-level `id` means `_id` when the primary key is id; nil matches everything.
func (c Coll) Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) ([]Row, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	r, err := c.findRows(ctx, filter, opts...)
	return r, c.q.note(err)
}

// FindOne returns the first matching document, or nil, nil when none matches.
func (c Coll) FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) (Row, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	r, err := c.findOneRow(ctx, filter, opts...)
	return r, c.q.note(err)
}

// FindInto decodes every matching document into *[]T with the driver's decoder (bson tags; `_id` stays `_id`;
// dates in time.Local). A *[]Row gets Rows as Find returns them.
func (c Coll) FindInto(ctx context.Context, filter, outSlicePtr any, opts ...options.Lister[options.FindOptions]) error {
	if err := c.q.failed(); err != nil {
		return err
	}
	return c.q.note(c.findInto(ctx, filter, outSlicePtr, opts...))
}

// FindOneInto decodes the first matching document into *T (a *Row gets a Row); ErrNoRows when none matches.
func (c Coll) FindOneInto(ctx context.Context, filter, outPtr any, opts ...options.Lister[options.FindOneOptions]) error {
	if err := c.q.failed(); err != nil {
		return err
	}
	return c.q.note(c.findOneInto(ctx, filter, outPtr, opts...))
}

// Aggregate runs a pipeline ([]bson.D, bson.A, mongo.Pipeline or []bson.M) and returns Rows: top-level `_id` →
// `id` (unless the result also has an `id`; ObjectIDs dropped) and the registry typing of this collection's columns
// where a result field has a column's name and a compatible value. Write `_id` inside the pipeline; documents
// joined by $lookup keep their `_id`. Not allowed in a transaction: $unionWith, $out, $merge.
func (c Coll) Aggregate(ctx context.Context, pipeline any) ([]Row, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	r, err := c.aggregateRows(ctx, pipeline)
	return r, c.q.note(err)
}

// AggregateInto decodes a pipeline's results into *[]T with the driver's decoder (*[]Row gets Aggregate's Rows).
func (c Coll) AggregateInto(ctx context.Context, pipeline, outSlicePtr any) error {
	if err := c.q.failed(); err != nil {
		return err
	}
	return c.q.note(c.aggregateInto(ctx, pipeline, outSlicePtr))
}

// Count is count(*) of the matching documents.
func (c Coll) Count(ctx context.Context, filter any) (int64, error) {
	if err := c.q.failed(); err != nil {
		return 0, err
	}
	n, err := c.count(ctx, filter)
	return n, c.q.note(err)
}

// Exists reports whether a document matches.
func (c Coll) Exists(ctx context.Context, filter any) (bool, error) {
	if err := c.q.failed(); err != nil {
		return false, err
	}
	b, err := c.exists(ctx, filter)
	return b, c.q.note(err)
}

// Distinct returns the distinct values of a field among the matching documents, typed like Row values, in
// Mongo's order (sort them yourself where the order matters).
func (c Coll) Distinct(ctx context.Context, field string, filter any) ([]any, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	v, err := c.distinct(ctx, field, filter)
	return v, c.q.note(err)
}

// Insert is INSERT … RETURNING id: it fills defaults and nulls, coerces values to the column types, enforces
// NOT NULL, CHECKs and foreign keys, and returns the id (the `_id` of an id primary key, else the id column, else
// the ObjectID). doc is a bson.M, bson.D, map or struct (bson tags).
func (c Coll) Insert(ctx context.Context, doc any) (any, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	id, err := c.insertOne(ctx, doc)
	return id, c.q.note(err)
}

// InsertMany inserts documents in order as one statement (CopyFrom / a multi-row INSERT): one now() for all,
// sequence values reserved as a block, foreign keys checked together (rows of the same batch count).
func (c Coll) InsertMany(ctx context.Context, docs []any) error {
	if err := c.q.failed(); err != nil {
		return err
	}
	return c.q.note(c.insertMany(ctx, docs))
}

// Update is UPDATE … for one document; it returns how many matched (0 or 1). The update uses operators ($set,
// $inc, $unset — stored as null —, $push, …); values are coerced and checked like an insert's, foreign keys
// checked, and changing a key another table references is refused (23503).
func (c Coll) Update(ctx context.Context, filter, update any) (int64, error) {
	if err := c.q.failed(); err != nil {
		return 0, err
	}
	n, err := c.update(ctx, filter, update, false)
	return n, c.q.note(err)
}

// UpdateMany is UPDATE … for every matching document; it returns how many matched.
func (c Coll) UpdateMany(ctx context.Context, filter, update any) (int64, error) {
	if err := c.q.failed(); err != nil {
		return 0, err
	}
	n, err := c.update(ctx, filter, update, true)
	return n, c.q.note(err)
}

// Upsert is INSERT … ON CONFLICT: set is written on both paths (DO UPDATE SET; nil for DO NOTHING), setOnInsert
// only when a document is inserted; the filter's equality fields, set, setOnInsert and every other column's default
// or null make up the inserted document, which must pass NOT NULL and the CHECKs (as the proposed row did in
// PostgreSQL). It reports whether a document was inserted.
func (c Coll) Upsert(ctx context.Context, filter any, set, setOnInsert bson.M) (bool, error) {
	if err := c.q.failed(); err != nil {
		return false, err
	}
	b, err := c.upsert(ctx, filter, set, setOnInsert)
	return b, c.q.note(err)
}

// FindOneAndUpdate is UPDATE … RETURNING for one document: it returns the document after the update (unless the
// options ask for options.Before), or nil when nothing matched. With SetUpsert(true) a missing document is inserted
// with every column filled, as Upsert does.
func (c Coll) FindOneAndUpdate(ctx context.Context, filter, update any, opts ...options.Lister[options.FindOneAndUpdateOptions]) (Row, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	r, err := c.findOneAndUpdate(ctx, filter, update, opts...)
	return r, c.q.note(err)
}

// Delete removes one matching document; it refuses (23503) when another table still references it.
func (c Coll) Delete(ctx context.Context, filter any) (int64, error) {
	if err := c.q.failed(); err != nil {
		return 0, err
	}
	n, err := c.del(ctx, filter, false)
	return n, c.q.note(err)
}

// DeleteMany removes every matching document; it refuses (23503) when another table still references one.
func (c Coll) DeleteMany(ctx context.Context, filter any) (int64, error) {
	if err := c.q.failed(); err != nil {
		return 0, err
	}
	n, err := c.del(ctx, filter, true)
	return n, c.q.note(err)
}

// Lock is SELECT … FOR UPDATE of one document inside a transaction: it bumps the document's hidden _lock field, so
// a concurrent transaction that locks or writes the same document conflicts and runs again after this one commits.
// It returns the document (nil if none matches; options may give a sort). Outside a transaction it is FindOne.
func (c Coll) Lock(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) (Row, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	r, err := c.lock(ctx, filter, opts...)
	return r, c.q.note(err)
}

// LockAll is SELECT … FOR UPDATE of every matching document (see Lock); it returns them like Find.
func (c Coll) LockAll(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) ([]Row, error) {
	if err := c.q.failed(); err != nil {
		return nil, err
	}
	r, err := c.lockAll(ctx, filter, opts...)
	return r, c.q.note(err)
}
