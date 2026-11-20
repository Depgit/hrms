package mdm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// The change capture ("transactional database hook", SOP §5): a MongoDB change stream on the collections the
// published snapshot is read from. Only committed changes appear in a change stream, so an event is published
// only for a change that committed. Each change is turned into the employees it can affect and handed to Publish,
// which compares their fresh snapshots with the last published ones: a bump of the hidden _lock field, or of any
// field the snapshot does not show, publishes nothing.
//
// One watcher works at a time across all servers: it holds a lease (mdm_state "watcher", renewed every few
// seconds, taken over when it expires) and every publishing transaction checks that it still holds it. The stream's
// resume token is saved with the lease after each batch, so a restarted watcher resumes where the last one left
// off; resuming re-publishes nothing (the hashes match) and skips nothing (the token is saved only after the
// batch's events are written). When there is no token, or the token is too old to resume from (the oplog moved
// on), the watcher opens a new stream first and then rescans every employee — the same as the nightly
// reconciliation — so no change made during the rescan is lost.

// watchedColls are the collections whose changes can change a published snapshot.
var watchedColls = bson.A{"employee", "designation", "department", "location", "grade"}

// masterFields are the fields of a master whose change changes the snapshots of its employees.
var masterFields = map[string]map[string]bool{
	"designation": {"name": true},
	"department":  {"name": true},
	"grade":       {"name": true},
	"location":    {"name": true, "property_id": true},
}

var (
	errLostLease   = errors.New("mdm: the watcher lost its lease")
	errInvalidated = errors.New("mdm: the change stream was invalidated")
)

// Watcher captures employee changes. Run it on every server; only the lease holder works.
type Watcher struct {
	svc       *Service
	id        string
	TTL       time.Duration // lease length (15s); renewed every TTL/3
	Retry     time.Duration // how often a waiting watcher tries to take the lease (2s)
	Await     time.Duration // how long one read of the stream waits for changes (1s)
	SaveEvery time.Duration // how often an idle stream's position is saved (10s)
	leader    atomic.Bool
	batches   atomic.Int64
	rescans   atomic.Int64
}

// NewWatcher returns a watcher identified by id (unique per server process).
func (s *Service) NewWatcher(id string) *Watcher {
	return &Watcher{svc: s, id: id, TTL: 15 * time.Second, Retry: 2 * time.Second, Await: time.Second, SaveEvery: 10 * time.Second}
}

// IsLeader reports whether this watcher holds the lease and is working.
func (w *Watcher) IsLeader() bool { return w.leader.Load() }

// Batches counts the batches of changes this watcher has handled (tests).
func (w *Watcher) Batches() int64 { return w.batches.Load() }

// Rescans counts the full rescans this watcher has made (no saved position, or one it could not resume from).
func (w *Watcher) Rescans() int64 { return w.rescans.Load() }

// Run works until ctx ends, then gives the lease up.
func (w *Watcher) Run(ctx context.Context) {
	defer w.release()
	for ctx.Err() == nil {
		tok, ok, err := w.acquire(ctx)
		if err != nil && ctx.Err() == nil {
			w.svc.logf("watcher: taking the lease: %v", err)
		}
		if !ok {
			sleepCtx(ctx, w.Retry)
			continue
		}
		w.leader.Store(true)
		err = w.lead(ctx, tok)
		w.leader.Store(false)
		if ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, errLostLease):
			w.svc.logf("watcher: lease lost; another server works")
		case errors.Is(err, errInvalidated):
			w.svc.logf("watcher: change stream invalidated; starting over with a rescan")
		case err != nil:
			w.svc.logf("watcher: %v", err)
			sleepCtx(ctx, time.Second)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// acquire takes or renews the lease; it returns the saved resume token.
func (w *Watcher) acquire(ctx context.Context) (string, bool, error) {
	if err := w.svc.ensureState(ctx, stateWatcher); err != nil {
		return "", false, err
	}
	now := w.svc.now()
	r, err := w.svc.st.DB.C(collState).FindOneAndUpdate(ctx,
		bson.M{"_id": stateWatcher, "$or": bson.A{bson.M{"owner": w.id}, bson.M{"owner": nil}, bson.M{"expires_at": bson.M{"$lt": now}}}},
		bson.M{"$set": bson.M{"owner": w.id, "expires_at": now.Add(w.TTL), "updated_at": now}})
	if err != nil || r == nil {
		return "", false, err
	}
	return str(r["token"]), true, nil
}

func (w *Watcher) renew(ctx context.Context) error {
	now := w.svc.now()
	r, err := w.svc.st.DB.C(collState).FindOneAndUpdate(ctx, bson.M{"_id": stateWatcher, "owner": w.id},
		bson.M{"$set": bson.M{"expires_at": now.Add(w.TTL), "updated_at": now}})
	if err != nil {
		return err
	}
	if r == nil {
		return errLostLease
	}
	return nil
}

// release gives the lease up (it expires at once), so another server takes over without waiting.
func (w *Watcher) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = w.svc.st.DB.C(collState).Update(ctx, bson.M{"_id": stateWatcher, "owner": w.id},
		bson.M{"$set": bson.M{"expires_at": w.svc.now(), "updated_at": w.svc.now()}})
}

// fence runs first in each publishing transaction: it fails when another watcher holds the lease. Writing the
// lease document also makes a takeover that happens meanwhile conflict with the transaction.
func (w *Watcher) fence(ctx context.Context, tx store.Q) error {
	n, err := tx.C(collState).Update(ctx, bson.M{"_id": stateWatcher, "owner": w.id}, bson.M{"$set": bson.M{"updated_at": tx.Now()}})
	if err != nil {
		return err
	}
	if n == 0 {
		return errLostLease
	}
	return nil
}

func (w *Watcher) saveToken(ctx context.Context, tok string) error {
	n, err := w.svc.st.DB.C(collState).Update(ctx, bson.M{"_id": stateWatcher, "owner": w.id},
		bson.M{"$set": bson.M{"token": tok, "updated_at": w.svc.now()}})
	if err != nil {
		return err
	}
	if n == 0 {
		return errLostLease
	}
	return nil
}

func (w *Watcher) open(ctx context.Context, tok string) (*mongo.ChangeStream, error) {
	match := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "ns.coll", Value: bson.D{{Key: "$in", Value: watchedColls}}},
			{Key: "operationType", Value: bson.D{{Key: "$in", Value: bson.A{"insert", "update", "replace"}}}}},
		bson.D{{Key: "operationType", Value: "invalidate"}},
	}}}
	o := options.ChangeStream().SetMaxAwaitTime(w.Await)
	if tok != "" {
		o.SetResumeAfter(bson.D{{Key: "_data", Value: tok}})
	}
	return w.svc.st.DB.Database().Watch(ctx, mongo.Pipeline{{{Key: "$match", Value: match}}}, o)
}

// cannotResume says whether opening the stream from a saved token failed because of the token (too old, invalid):
// the server refused it, as opposed to the network failing.
func cannotResume(err error) bool {
	var se mongo.ServerError
	return errors.As(err, &se)
}

func tokenOf(raw bson.Raw) string {
	if raw == nil {
		return ""
	}
	if v, err := raw.LookupErr("_data"); err == nil {
		if s, ok := v.StringValueOK(); ok {
			return s
		}
	}
	return ""
}

func (w *Watcher) lead(ctx context.Context, tok string) error {
	var cs *mongo.ChangeStream
	var err error
	if tok != "" {
		if cs, err = w.open(ctx, tok); err != nil {
			if !cannotResume(err) {
				return err
			}
			w.svc.logf("watcher: cannot resume from the saved position (%v); rescanning every employee", err)
			cs = nil
		}
	}
	if cs == nil {
		// A new stream first, then the rescan: a change committed during the rescan is in the stream.
		if cs, err = w.open(ctx, ""); err != nil {
			return err
		}
		start := tokenOf(cs.ResumeToken())
		w.rescans.Add(1)
		codes, err := w.svc.AllEmployeeCodes(ctx)
		if err == nil {
			_, err = w.svc.Publish(ctx, codes, w.fence)
		}
		if err == nil {
			err = w.saveToken(ctx, start)
		}
		if err != nil {
			cs.Close(context.WithoutCancel(ctx))
			return err
		}
		tok = start
	}
	defer cs.Close(context.WithoutCancel(ctx))
	saved, lastSave, lastRenew := tok, time.Now(), time.Now()
	for ctx.Err() == nil {
		codes, got, invalid, err := w.readBatch(ctx, cs)
		if err != nil {
			return err
		}
		if got {
			if len(codes) > 0 {
				if _, err := w.svc.Publish(ctx, codes, w.fence); err != nil {
					return err
				}
			}
			w.batches.Add(1)
		}
		if invalid || (!got && cs.ID() == 0) {
			_ = w.saveToken(ctx, "") // resume is impossible after an invalidate: start over
			return errInvalidated
		}
		if cur := tokenOf(cs.ResumeToken()); cur != "" && cur != saved && (got || time.Since(lastSave) >= w.SaveEvery) {
			if err := w.saveToken(ctx, cur); err != nil {
				return err
			}
			saved, lastSave = cur, time.Now()
		}
		if time.Since(lastRenew) >= w.TTL/3 {
			if err := w.renew(ctx); err != nil {
				return err
			}
			lastRenew = time.Now()
		}
	}
	return nil
}

// changeEvent is the part of a change event the watcher reads.
type changeEvent struct {
	OperationType string `bson:"operationType"`
	NS            struct {
		Coll string `bson:"coll"`
	} `bson:"ns"`
	DocumentKey       bson.Raw `bson:"documentKey"`
	UpdateDescription *struct {
		UpdatedFields bson.Raw `bson:"updatedFields"`
		RemovedFields []string `bson:"removedFields"`
	} `bson:"updateDescription"`
}

// readBatch reads what the stream has now (waiting up to Await when nothing is there), up to 1000 changes, and
// returns the employees they affect.
func (w *Watcher) readBatch(ctx context.Context, cs *mongo.ChangeStream) (codes []string, got, invalid bool, err error) {
	for n := 0; n < 1000 && cs.TryNext(ctx); n++ {
		got = true
		var ev changeEvent
		if err := cs.Decode(&ev); err != nil {
			return nil, got, false, err
		}
		if ev.OperationType == "invalidate" {
			return codes, got, true, nil
		}
		more, err := w.svc.affected(ctx, ev)
		if err != nil {
			return nil, got, false, err
		}
		codes = append(codes, more...)
		if cs.RemainingBatchLength() == 0 {
			break
		}
	}
	return codes, got, false, cs.Err()
}

// touches says whether an update changed one of these fields (top-level names).
func touches(ev changeEvent, fields map[string]bool) bool {
	if ev.OperationType != "update" {
		return true // insert, replace: the whole document
	}
	if ev.UpdateDescription == nil {
		return true
	}
	if els, err := ev.UpdateDescription.UpdatedFields.Elements(); err == nil {
		for _, el := range els {
			if fields[topField(el.Key())] {
				return true
			}
		}
	}
	for _, k := range ev.UpdateDescription.RemovedFields {
		if fields[topField(k)] {
			return true
		}
	}
	return false
}

func topField(k string) string {
	if i := strings.IndexByte(k, '.'); i >= 0 {
		return k[:i]
	}
	return k
}

// affected lists the employees a change can affect.
func (s *Service) affected(ctx context.Context, ev changeEvent) ([]string, error) {
	var id string
	if v, err := ev.DocumentKey.LookupErr("_id"); err == nil {
		id, _ = v.StringValueOK()
	}
	if id == "" {
		return nil, nil
	}
	if ev.NS.Coll == "employee" {
		if touches(ev, employeeFields) {
			return []string{id}, nil
		}
		return nil, nil
	}
	fields, ok := masterFields[ev.NS.Coll]
	if !ok || ev.OperationType == "insert" || !touches(ev, fields) {
		return nil, nil // a new master has no employees yet
	}
	var filter bson.M
	switch ev.NS.Coll {
	case "designation":
		filter = bson.M{"designation_id": id}
	case "department":
		filter = bson.M{"department_id": id}
	case "location":
		filter = bson.M{"location_id": id}
	case "grade": // employees hold the grade by name (a rename rewrites them too)
		g, err := s.st.DB.C("grade").FindOne(ctx, bson.M{"_id": id})
		if err != nil || g == nil {
			return nil, err
		}
		filter = bson.M{"grade": g["name"]}
	}
	ids, err := s.st.DB.C("employee").Distinct(ctx, "_id", filter)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, v := range ids {
		out = append(out, str(v))
	}
	return out, nil
}
