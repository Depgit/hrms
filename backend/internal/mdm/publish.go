package mdm

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// State documents (mdm_state).
const (
	statePos       = "pos"               // n: the last outbox position handed out
	stateWatcher   = "watcher"           // the watcher's lease (owner, expires_at) and resume token
	stateReconcile = "reconcile_nightly" // day: the business date of the last nightly reconciliation
)

// Publish recomputes the snapshots of these employees and, for each one that differs from the last snapshot
// published (mdm_published), writes the outbox event and the new published snapshot in ONE transaction (one per
// chunk of employees). fence, when set, runs first in each transaction: the watcher checks there that it still
// holds its lease. It returns how many events were written.
//
// Positions come from a counter document updated inside the same transaction, so two writers conflict on it and
// one waits for the other: pos order is commit order, and a reader never sees a later pos before an earlier one.
// Comparing with the last published hash makes Publish idempotent: running it again for the same employees (a
// watcher resuming, a reconciliation, a duplicate change) writes nothing.
func (s *Service) Publish(ctx context.Context, codes []string, fence func(ctx context.Context, tx store.Q) error) (int, error) {
	codes = uniqSorted(codes)
	if len(codes) == 0 {
		return 0, nil
	}
	if err := s.ensureCounter(ctx); err != nil {
		return 0, err
	}
	total := 0
	for i := 0; i < len(codes); i += s.o.ChunkSize {
		chunk := codes[i:min(i+s.o.ChunkSize, len(codes))]
		var n int
		err := s.st.Tx(ctx, func(tx store.Q) error {
			n = 0
			if fence != nil {
				if err := fence(ctx, tx); err != nil {
					return err
				}
			}
			var err error
			n, err = s.publishTx(ctx, tx, chunk)
			return err
		})
		if err != nil {
			if total > 0 {
				s.wake()
			}
			return total, err
		}
		total += n
	}
	if total > 0 {
		s.wake()
	}
	return total, nil
}

func (s *Service) ensureCounter(ctx context.Context) error {
	ok, err := s.st.DB.C(collState).Exists(ctx, bson.M{"_id": statePos, "n": bson.M{"$type": "long"}})
	if err != nil || ok {
		return err
	}
	_, err = s.st.DB.C(collState).Upsert(ctx, bson.M{"_id": statePos, "n": nil}, bson.M{"n": int64(0), "updated_at": s.now()}, nil)
	if code, _ := store.ErrCode(err); code == "23505" {
		return nil // made by another server meanwhile
	}
	return err
}

type pubChange struct {
	code string
	data map[string]any
	hash string
	prev store.Row
}

func (s *Service) publishTx(ctx context.Context, tx store.Q, codes []string) (int, error) {
	snaps, err := snapshots(ctx, tx, codes)
	if err != nil {
		return 0, err
	}
	pubs, err := tx.C(collPublished).Find(ctx, bson.M{"employee_code": bson.M{"$in": codes}})
	if err != nil {
		return 0, err
	}
	prev := make(map[string]store.Row, len(pubs))
	for _, p := range pubs {
		prev[str(p["employee_code"])] = p
	}
	var changes []pubChange
	for _, code := range codes {
		d, ok := snaps[code]
		if !ok {
			continue // no such employee (employees are never deleted; nothing to publish)
		}
		h := Hash(d)
		p := prev[code]
		if p != nil && str(p["hash"]) == h {
			continue
		}
		changes = append(changes, pubChange{code: code, data: d, hash: h, prev: p})
	}
	if len(changes) == 0 {
		return 0, nil
	}
	ctr, err := tx.C(collState).FindOneAndUpdate(ctx, bson.M{"_id": statePos}, bson.M{"$inc": bson.M{"n": int64(len(changes))}})
	if err != nil {
		return 0, err
	}
	if ctr == nil {
		return 0, errors.New("the outbox position counter is missing")
	}
	first := i64(ctr["n"]) - int64(len(changes)) + 1
	now := tx.Now()
	ts := utc(now)
	docs := make([]any, 0, len(changes))
	for i, c := range changes {
		seq := int64(1)
		var was map[string]any
		if c.prev != nil {
			seq = i64(c.prev["sequence"]) + 1
			was, _ = c.prev["data"].(map[string]any)
		}
		id, pos := store.NewID(), first+int64(i)
		docs = append(docs, bson.M{"pos": pos, "event_id": id, "event_type": eventTypeOf(was, c.data), "schema_version": SchemaVersion,
			"timestamp": ts, "source": Source, "employee_code": c.code, "sequence": seq, "data": c.data, "created_at": now})
		if _, err := tx.C(collPublished).Upsert(ctx, bson.M{"employee_code": c.code},
			bson.M{"sequence": seq, "hash": c.hash, "data": c.data, "event_id": id, "pos": pos, "updated_at": now}, nil); err != nil {
			return 0, err
		}
	}
	if err := tx.C(collOutbox).InsertMany(ctx, docs); err != nil {
		return 0, err
	}
	return len(changes), nil
}

// AllEmployeeCodes lists every employee code, sorted.
func (s *Service) AllEmployeeCodes(ctx context.Context) ([]string, error) {
	ids, err := s.st.DB.C("employee").Distinct(ctx, "_id", nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, v := range ids {
		if c := str(v); c != "" {
			out = append(out, c)
		}
	}
	return uniqSorted(out), nil
}

// Head is the last outbox position written (0 when there is none).
func (s *Service) Head(ctx context.Context, q store.Q) (int64, error) {
	r, err := q.C(collOutbox).Find(ctx, nil, findSortLimit(bson.D{{Key: "pos", Value: -1}}, 1))
	if err != nil || len(r) == 0 {
		return 0, err
	}
	return i64(r[0]["pos"]), nil
}
