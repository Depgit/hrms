package store

// Tests of the store wrapper against a real MongoDB replica set. Set HRMS_TEST_MONGO_URL, e.g.
// mongodb://localhost:27017/?replicaSet=rs0&directConnection=true; HRMS_TEST_MONGO_STORE_DB names the database
// (default hrms_test_store), which is dropped and rebuilt. Skipped otherwise.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	testOnce sync.Once
	testErr  error
)

func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	name := os.Getenv("HRMS_TEST_MONGO_STORE_DB")
	if name == "" {
		name = "hrms_test_store"
	}
	ctx := context.Background()
	s, err := Open(ctx, url, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	testOnce.Do(func() {
		if testErr = s.DB.Database().Drop(ctx); testErr != nil {
			return
		}
		testErr = s.Migrate(ctx, func(string) {})
	})
	if testErr != nil {
		t.Fatal(testErr)
	}
	if err := s.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func wantCode(t *testing.T, err error, code string) (string, string) {
	t.Helper()
	c, d := ErrCode(err)
	if c != code {
		t.Fatalf("want code %s, got %q (%v)", code, c, err)
	}
	return c, d
}

func mustInsert(t *testing.T, ctx context.Context, q Q, coll string, doc bson.M) any {
	t.Helper()
	id, err := q.C(coll).Insert(ctx, doc)
	if err != nil {
		t.Fatalf("insert %s: %v", coll, err)
	}
	return id
}

// region → state → city, the smallest chain of foreign keys.
func orgChain(t *testing.T, ctx context.Context, q Q) (region, state, city string) {
	region = mustInsert(t, ctx, q, "region", bson.M{"name": "North"}).(string)
	state = mustInsert(t, ctx, q, "state", bson.M{"name": "Delhi", "region_id": region}).(string)
	city = mustInsert(t, ctx, q, "city", bson.M{"name": "Delhi", "state_id": state}).(string)
	return
}

func TestMigrationsAndReferenceData(t *testing.T) {
	s, ctx := testStore(t)
	names, err := s.DB.Database().ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, tb := range Tables() {
		if !have[tb.Name] {
			t.Fatalf("collection %s missing", tb.Name)
		}
	}
	if n, _ := s.DB.C("holiday").Count(ctx, nil); n != 48 {
		t.Fatalf("holidays: %d", n)
	}
	// The calendars got new ids and the holidays point at them.
	cals, _ := s.DB.C("holiday_calendar").Distinct(ctx, "id", nil)
	refs, _ := s.DB.C("holiday").Distinct(ctx, "calendar_id", nil)
	if len(cals) != 4 || len(refs) != 4 {
		t.Fatalf("calendars %v, holiday refs %v", cals, refs)
	}
	for _, r := range refs {
		found := false
		for _, c := range cals {
			found = found || c == r
		}
		if !found {
			t.Fatalf("holiday points at a missing calendar %v", r)
		}
	}
	// Counters: past the reference rows, or at START.
	if n, _ := NextSeq(ctx, s.DB, "inv_item_id_seq"); n != 10 {
		t.Fatalf("inv_item_id_seq next = %d, want 10", n)
	}
	if n, _ := NextSeq(ctx, s.DB, "approval_request_seq"); n != 3001 {
		t.Fatalf("approval_request_seq next = %d, want 3001", n)
	}
	if n, _ := NextSeq(ctx, s.DB, "holiday_calendar_seq_seq"); n != 5 {
		t.Fatalf("holiday_calendar_seq_seq next = %d, want 5", n)
	}
	// A second Migrate does nothing.
	var applied []string
	if err := s.Migrate(ctx, func(m string) { applied = append(applied, m) }); err != nil || len(applied) > 0 {
		t.Fatalf("re-migrate: %v %v", applied, err)
	}
}

func TestInsertDefaultsNullsAndTyping(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	region, _, city := orgChain(t, ctx, q)
	r, err := q.C("region").FindOne(ctx, bson.M{"id": region})
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	// Every column, defaults applied, nulls explicit, `id` not `_id`, typed as pgx read them.
	for _, k := range []string{"id", "name", "status", "inactive_since", "inactive_reason", "created_at", "seq"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("region row lacks %s: %v", k, r)
		}
	}
	if _, ok := r["_id"]; ok {
		t.Fatal("_id in a Row")
	}
	if r["status"] != "Active" || r["inactive_since"] != nil {
		t.Fatalf("defaults: %v", r)
	}
	if _, ok := r["seq"].(int64); !ok {
		t.Fatalf("seq is %T", r["seq"])
	}
	if ts, ok := r["created_at"].(time.Time); !ok || ts.Location().String() != Zone() {
		t.Fatalf("created_at %T %v", r["created_at"], r["created_at"])
	}
	if raw, _ := q.Database().Collection("region").FindOne(ctx, bson.M{"_id": region}).Raw(); raw == nil {
		t.Fatal("region not stored under _id")
	}
	// int2 → int16, int4 → int32, numeric rounded to its scale, date and time formats, jsonb from json tags.
	mustInsert(t, ctx, q, "role", bson.M{"name": "Tester"})
	mustInsert(t, ctx, q, "role_right", bson.M{"role_name": "Tester", "right_id": "x.y", "level": 2.0})
	rr, _ := q.C("role_right").FindOne(ctx, bson.M{"role_name": "Tester"})
	if _, ok := rr["level"].(int16); !ok {
		t.Fatalf("level %T", rr["level"])
	}
	type rule struct {
		Min float64 `json:"minAmount"`
		On  bool    `json:"on"`
	}
	mustInsert(t, ctx, q, "pay_band", bson.M{"city_id": city, "grade": "G1", "min_gross": 1000.005, "max_gross": 2675})
	pb, _ := q.C("pay_band").FindOne(ctx, bson.M{"city_id": city})
	if pb["min_gross"] != 1000.01 || pb["max_gross"] != 2675.0 {
		t.Fatalf("numeric: %v %v", pb["min_gross"], pb["max_gross"])
	}
	if _, ok := pb["id"]; ok {
		t.Fatal("an ObjectID _id must not be read back")
	}
	sh := mustInsert(t, ctx, q, "shift", bson.M{"code": "G", "name": "General", "start_time": "09:00:00", "end_time": time.Date(2026, 1, 1, 18, 30, 0, 0, time.UTC),
		"location_ids": []string{"7A0F3C3E-2B6D-4C59-9E4B-0B8E3D2B1A10"}, "rules": rule{Min: 5, On: true}})
	r, _ = q.C("shift").FindOne(ctx, bson.M{"_id": sh})
	if r["start_time"] != "09:00" || r["end_time"] != "18:30" {
		t.Fatalf("times: %v %v", r["start_time"], r["end_time"])
	}
	ids, _ := r["location_ids"].([]any)
	if len(ids) != 1 || ids[0] != "7a0f3c3e-2b6d-4c59-9e4b-0b8e3d2b1a10" {
		t.Fatalf("uuid[]: %#v", r["location_ids"])
	}
	rules, _ := r["rules"].(map[string]any)
	if rules["minAmount"] != 5.0 || rules["on"] != true {
		t.Fatalf("jsonb: %#v", r["rules"])
	}
	if _, ok := r["break_minutes"].(int32); !ok {
		t.Fatalf("break_minutes %T", r["break_minutes"])
	}
	mustInsert(t, ctx, q, "holiday_calendar", bson.M{"name": "Test cal"})
	hc, _ := q.C("holiday_calendar").FindOne(ctx, bson.M{"name": "Test cal"})
	mustInsert(t, ctx, q, "holiday", bson.M{"calendar_id": hc["id"], "date": time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC), "name": "Holi", "kind": "National"})
	h, _ := q.C("holiday").FindOne(ctx, bson.M{"calendar_id": hc["id"], "date": time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)})
	if h == nil || h["date"] != "2026-03-04" {
		t.Fatalf("date: %v", h)
	}
	if _, ok := h["id"].(int64); !ok {
		t.Fatalf("holiday id %T", h["id"])
	}
	// Bad values: a malformed uuid, an impossible date, an unknown column.
	_, err = q.C("state").Insert(ctx, bson.M{"name": "X", "region_id": "not-a-uuid"})
	wantCode(t, err, "22P02")
	_, err = q.C("holiday").Insert(ctx, bson.M{"calendar_id": hc["id"], "date": "2026-02-30", "name": "Bad", "kind": "National"})
	wantCode(t, err, "22007")
	_, err = q.C("region").Insert(ctx, bson.M{"name": "Y", "nope": 1})
	wantCode(t, err, "42703")
}

func TestForeignKeys(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	region, state, _ := orgChain(t, ctx, q)
	missing := NewID()
	_, err := q.C("state").Insert(ctx, bson.M{"name": "Nowhere", "region_id": missing})
	_, d := wantCode(t, err, "23503")
	if d != "Key (region_id)=("+missing+") is not present in table \"region\"." {
		t.Fatalf("detail %q", d)
	}
	_, err = q.C("state").Update(ctx, bson.M{"id": state}, bson.M{"$set": bson.M{"region_id": missing}})
	wantCode(t, err, "23503")
	_, err = q.C("region").Delete(ctx, bson.M{"id": region})
	_, d = wantCode(t, err, "23503")
	if !strings.Contains(d, `is still referenced from table "state"`) {
		t.Fatalf("detail %q", d)
	}
	// Unreferenced: deletes; a self-reference inside one batch is fine.
	other := mustInsert(t, ctx, q, "region", bson.M{"name": "South"})
	if n, err := q.C("region").Delete(ctx, bson.M{"id": other}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, _ := q.C("region").Count(ctx, nil); n != 1 {
		t.Fatalf("regions: %d", n)
	}
}

func TestChecksNotNullAndDuplicates(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	_, err := q.C("region").Insert(ctx, bson.M{"name": "R", "status": "Paused"})
	_, d := wantCode(t, err, "23514")
	if !strings.HasPrefix(d, "Failing row contains (") {
		t.Fatalf("detail %q", d)
	}
	_, err = q.C("region").Insert(ctx, bson.M{"status": "Active"})
	wantCode(t, err, "23514")
	id := mustInsert(t, ctx, q, "region", bson.M{"name": "R"})
	_, err = q.C("region").Update(ctx, bson.M{"id": id}, bson.M{"$set": bson.M{"status": "Paused"}})
	wantCode(t, err, "23514")
	_, err = q.C("region").Update(ctx, bson.M{"id": id}, bson.M{"$unset": bson.M{"name": ""}})
	wantCode(t, err, "23514")
	_, err = q.C("approval_setting").Update(ctx, bson.M{"id": 1}, bson.M{"$set": bson.M{"sla_hours": 0}})
	wantCode(t, err, "23514")
	// A two-column CHECK on update needs the stored row: max_gross >= min_gross.
	_, _, city := orgChain(t, ctx, q)
	mustInsert(t, ctx, q, "pay_band", bson.M{"city_id": city, "grade": "G2", "min_gross": 100, "max_gross": 200})
	_, err = q.C("pay_band").Update(ctx, bson.M{"city_id": city, "grade": "G2"}, bson.M{"$set": bson.M{"max_gross": 50}})
	wantCode(t, err, "23514")
	// $inc that would break next_no > 0.
	mustInsert(t, ctx, q, "employee_code_series", bson.M{"key": "T", "prefix": "T-", "pad": 4, "next_no": 1})
	_, err = q.C("employee_code_series").Update(ctx, bson.M{"key": "T"}, bson.M{"$inc": bson.M{"next_no": -1}})
	wantCode(t, err, "23514")
	if _, err := q.C("employee_code_series").Update(ctx, bson.M{"key": "T"}, bson.M{"$inc": bson.M{"next_no": 1}}); err != nil {
		t.Fatal(err)
	}
	// Duplicates.
	_, err = q.C("region").Insert(ctx, bson.M{"name": "R"})
	_, d = wantCode(t, err, "23505")
	if d != "Key (name)=(R) already exists." {
		t.Fatalf("detail %q", d)
	}
	_, err = q.C("approval_setting").Insert(ctx, bson.M{"id": 1})
	_, d = wantCode(t, err, "23505")
	if d != "Key (id)=(1) already exists." {
		t.Fatalf("detail %q", d)
	}
	// A unique index on a nullable column allows any number of nulls (partial), but not two equal values.
	for _, n := range []string{"A", "B", "C"} {
		mustInsert(t, ctx, q, "simple_master", bson.M{"kind": "qualification", "name": n})
	}
	if _, err := q.C("simple_master").UpdateMany(ctx, bson.M{"name": bson.M{"$in": []string{"A", "B"}}}, bson.M{"$set": bson.M{"sys_key": nil}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("simple_master").Update(ctx, bson.M{"name": "A"}, bson.M{"$set": bson.M{"sys_key": "k1"}}); err != nil {
		t.Fatal(err)
	}
	_, err = q.C("simple_master").Update(ctx, bson.M{"name": "B"}, bson.M{"$set": bson.M{"sys_key": "k1"}})
	wantCode(t, err, "23505")
	// NULLS NOT DISTINCT (kind, name, used_for): a second (qualification, A, null) is a duplicate.
	_, err = q.C("simple_master").Insert(ctx, bson.M{"kind": "qualification", "name": "A"})
	wantCode(t, err, "23505")
	// The error reaches writeErr's codes through wrapping.
	if c, _ := ErrCode(errors.Join(errors.New("ctx"), err)); c != "23505" {
		t.Fatal(c)
	}
}

func TestUpsert(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	f := bson.M{"wf": "transfer", "flow": "Operations"}
	ins, err := q.C("approval_matrix").Upsert(ctx, f, bson.M{"cols": []string{"HRBP"}, "rows": []any{}}, bson.M{"updated_by": "seed"})
	if err != nil || !ins {
		t.Fatal(ins, err)
	}
	r, _ := q.C("approval_matrix").FindOne(ctx, f)
	if r["no_response"] != "escalate" || r["updated_by"] != "seed" || r["wf"] != "transfer" {
		t.Fatalf("inserted: %v", r)
	}
	ins, err = q.C("approval_matrix").Upsert(ctx, f, bson.M{"cols": []string{"CHRO"}}, bson.M{"rows": []any{}, "updated_by": "other"})
	if err != nil || ins {
		t.Fatal(ins, err)
	}
	r, _ = q.C("approval_matrix").FindOne(ctx, f)
	if c, _ := r["cols"].([]any); len(c) != 1 || c[0] != "CHRO" || r["updated_by"] != "seed" {
		t.Fatalf("updated: %v", r)
	}
	// DO NOTHING: only setOnInsert; an existing document is left alone.
	ins, err = q.C("grade").Upsert(ctx, bson.M{"name": "G1"}, nil, nil)
	if err != nil || !ins {
		t.Fatal(ins, err)
	}
	g, _ := q.C("grade").FindOne(ctx, bson.M{"name": "G1"})
	if _, ok := g["id"].(string); !ok || g["status"] != "Active" {
		t.Fatalf("grade: %v", g)
	}
	ins, err = q.C("grade").Upsert(ctx, bson.M{"name": "G1"}, nil, nil)
	if err != nil || ins {
		t.Fatal(ins, err)
	}
	// FindOneAndUpdate returns the document after the update by default.
	after, err := q.C("grade").FindOneAndUpdate(ctx, bson.M{"name": "G1"}, bson.M{"$set": bson.M{"status": "Inactive"}})
	if err != nil || after["status"] != "Inactive" {
		t.Fatal(after, err)
	}
	none, err := q.C("grade").FindOneAndUpdate(ctx, bson.M{"name": "G9"}, bson.M{"$set": bson.M{"status": "Inactive"}})
	if err != nil || none != nil {
		t.Fatal(none, err)
	}
	before, _ := q.C("grade").FindOneAndUpdate(ctx, bson.M{"name": "G1"}, bson.M{"$set": bson.M{"status": "Active"}},
		options.FindOneAndUpdate().SetReturnDocument(options.Before))
	if before["status"] != "Inactive" {
		t.Fatal(before)
	}
}

func TestTransactions(t *testing.T) {
	s, ctx := testStore(t)
	// Commit.
	err := s.Tx(ctx, func(tx Q) error {
		if !tx.InTx() || s.DB.InTx() {
			t.Fatal("InTx")
		}
		_, err := tx.C("region").Insert(ctx, bson.M{"name": "In"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Abort: nothing stays, and the error comes back as is.
	boom := errors.New("boom")
	err = s.Tx(ctx, func(tx Q) error {
		if _, err := tx.C("region").Insert(ctx, bson.M{"name": "Out"}); err != nil {
			return err
		}
		// Visible inside, not outside.
		if ok, _ := tx.C("region").Exists(ctx, bson.M{"name": "Out"}); !ok {
			t.Fatal("not visible inside")
		}
		if ok, _ := s.DB.C("region").Exists(ctx, bson.M{"name": "Out"}); ok {
			t.Fatal("visible outside before commit")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if ok, _ := s.DB.C("region").Exists(ctx, bson.M{"name": "Out"}); ok {
		t.Fatal("aborted insert stayed")
	}
	// now() is one instant per transaction.
	_ = s.Tx(ctx, func(tx Q) error {
		a, _ := tx.C("region").Insert(ctx, bson.M{"name": "T1"})
		time.Sleep(5 * time.Millisecond)
		b, _ := tx.C("region").Insert(ctx, bson.M{"name": "T2"})
		ra, _ := tx.C("region").FindOne(ctx, bson.M{"id": a})
		rb, _ := tx.C("region").FindOne(ctx, bson.M{"id": b})
		if !ra["created_at"].(time.Time).Equal(rb["created_at"].(time.Time)) {
			t.Error("now() differs inside one transaction")
		}
		return nil
	})
	// A sequence is not rolled back.
	var inTx int64
	_ = s.Tx(ctx, func(tx Q) error {
		inTx, _ = NextSeq(ctx, tx, "offer_seq")
		return boom
	})
	if n, _ := NextSeq(ctx, s.DB, "offer_seq"); n != inTx+1 || inTx != 1001 {
		t.Fatalf("offer_seq: in tx %d, after %d", inTx, n)
	}
}

// A failed write aborts a Mongo transaction (as an error aborted a Postgres one): code that swallows the error
// must get it back at once, not a two-minute retry loop of NoSuchTransaction.
func TestSwallowedErrorAbortsTransaction(t *testing.T) {
	s, ctx := testStore(t)
	mustInsert(t, ctx, s.DB, "grade", bson.M{"name": "D"})
	start := time.Now()
	runs := 0
	err := s.Tx(ctx, func(tx Q) error {
		runs++
		_, _ = tx.C("grade").Insert(ctx, bson.M{"name": "D"}) // duplicate, ignored
		_, err := tx.C("grade").Insert(ctx, bson.M{"name": "E"})
		var ae *AbortedError
		if !errors.As(err, &ae) {
			t.Errorf("the next call: %v", err)
		}
		return nil
	})
	if c, _ := ErrCode(err); c != "23505" || runs != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v, runs %d, took %v", err, runs, time.Since(start))
	}
	if ok, _ := s.DB.C("grade").Exists(ctx, bson.M{"name": "E"}); ok {
		t.Fatal("the aborted transaction wrote")
	}
	// A store check (the server never saw the write) does not abort the transaction.
	err = s.Tx(ctx, func(tx Q) error {
		if _, err := tx.C("grade").Insert(ctx, bson.M{"name": "X", "status": "Paused"}); err == nil {
			t.Error("check not enforced")
		}
		_, err := tx.C("grade").Insert(ctx, bson.M{"name": "F"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteConflictRetries(t *testing.T) {
	s, ctx := testStore(t)
	id := mustInsert(t, ctx, s.DB, "employee_code_series", bson.M{"key": "W", "prefix": "W-", "pad": 4, "next_no": 1})
	inside := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		first := true
		done <- s.Tx(ctx, func(tx Q) error {
			if _, err := tx.C("employee_code_series").Update(ctx, bson.M{"_id": id}, bson.M{"$inc": bson.M{"next_no": 1}}); err != nil {
				return err
			}
			if first {
				first = false
				close(inside)
				<-release // hold the write until the other transaction has hit the conflict
			}
			return nil
		})
	}()
	<-inside
	// A second writer of the same document while the first is open conflicts; its callback runs again once the
	// first has committed, and sees the first's write.
	var attempts atomic.Int32
	var once sync.Once
	err2 := s.Tx(ctx, func(tx Q) error {
		attempts.Add(1)
		_, err := tx.C("employee_code_series").Update(ctx, bson.M{"_id": id}, bson.M{"$inc": bson.M{"next_no": 10}})
		if err != nil {
			once.Do(func() { close(release) })
		}
		return err
	})
	if err2 != nil {
		t.Fatal(err2)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r, _ := s.DB.C("employee_code_series").FindOne(ctx, bson.M{"_id": id})
	if r["next_no"] != int32(12) {
		t.Fatalf("next_no %v (both increments must land)", r["next_no"])
	}
	if attempts.Load() < 2 {
		t.Fatalf("the conflicting transaction ran %d time(s); it had to be retried", attempts.Load())
	}
}

func TestLockSerializes(t *testing.T) {
	s, ctx := testStore(t)
	mustInsert(t, ctx, s.DB, "employee_code_series", bson.M{"key": "L", "prefix": "L-", "pad": 4, "next_no": 1})
	// Read-modify-write under Lock from several goroutines: no update may be lost.
	const workers = 5
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Tx(ctx, func(tx Q) error {
				r, err := tx.C("employee_code_series").Lock(ctx, bson.M{"key": "L"})
				if err != nil || r == nil {
					return errors.Join(err, errors.New("not found"))
				}
				if _, ok := r["_lock"]; ok {
					return errors.New("_lock visible")
				}
				n := r["next_no"].(int32)
				time.Sleep(20 * time.Millisecond)
				_, err = tx.C("employee_code_series").Update(ctx, bson.M{"key": "L"}, bson.M{"$set": bson.M{"next_no": n + 1}})
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r, _ := s.DB.C("employee_code_series").FindOne(ctx, bson.M{"key": "L"})
	if r["next_no"] != int32(1+workers) {
		t.Fatalf("next_no %v, want %d: a locked read-modify-write was lost", r["next_no"], 1+workers)
	}
	// AdvisoryLock serializes the same way.
	var counter int
	var wg2 sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			_ = s.Tx(ctx, func(tx Q) error {
				if err := AdvisoryLock(ctx, tx, "test-key"); err != nil {
					return err
				}
				_, err := tx.C("region").Insert(ctx, bson.M{"name": "adv-" + NewID()})
				return err
			})
			counter++
		}()
	}
	wg2.Wait()
	if n, _ := s.DB.C("region").Count(ctx, bson.M{"name": bson.M{"$regex": "^adv-"}}); n != 3 {
		t.Fatalf("advisory-locked inserts: %d", n)
	}
}

func TestAuditLogAppendOnly(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	id := mustInsert(t, ctx, q, "audit_log", bson.M{"actor": "EMP-1", "action": "test"})
	if _, ok := id.(int64); !ok {
		t.Fatalf("audit id %T", id)
	}
	_, err := q.C("audit_log").Update(ctx, bson.M{"id": id}, bson.M{"$set": bson.M{"action": "changed"}})
	if c, _ := ErrCode(err); c != "P0001" || !strings.Contains(err.Error(), "append-only") {
		t.Fatal(err)
	}
	_, err = q.C("audit_log").DeleteMany(ctx, nil)
	if c, _ := ErrCode(err); c != "P0001" {
		t.Fatal(err)
	}
	if n, _ := q.C("audit_log").Count(ctx, nil); n != 1 {
		t.Fatal(n)
	}
}

func TestReadHelpers(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	for _, n := range []string{"b", "a", "c"} {
		mustInsert(t, ctx, q, "grade", bson.M{"name": n})
	}
	rows, err := q.C("grade").Find(ctx, bson.M{"name": bson.M{"$in": []string{"a", "b", "z"}}}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil || len(rows) != 2 || rows[0]["name"] != "a" {
		t.Fatal(rows, err)
	}
	empty, _ := q.C("grade").Find(ctx, bson.M{"name": "zz"})
	if empty == nil || len(empty) != 0 {
		t.Fatal("Find must return an empty, non-nil slice")
	}
	var one Row
	if err := q.C("grade").FindOneInto(ctx, bson.M{"name": "zz"}, &one); !errors.Is(err, ErrNoRows) {
		t.Fatal(err)
	}
	type g struct {
		ID   string `bson:"_id"`
		Name string `bson:"name"`
	}
	var gs []g
	if err := q.C("grade").FindInto(ctx, nil, &gs, options.Find().SetSort(bson.D{{Key: "id", Value: 1}})); err != nil || len(gs) != 3 || gs[0].ID == "" {
		t.Fatal(gs, err)
	}
	agg, err := q.C("grade").Aggregate(ctx, bson.A{bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$status"}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}}})
	if err != nil || len(agg) != 1 || agg[0]["id"] != "Active" || agg[0]["n"] != int32(3) {
		t.Fatal(agg, err)
	}
	if ok, _ := q.C("grade").Exists(ctx, bson.M{"name": "a"}); !ok {
		t.Fatal("exists")
	}
	if n, _ := q.C("grade").Count(ctx, nil); n != 3 {
		t.Fatal(n)
	}
	if n, err := q.C("grade").DeleteMany(ctx, bson.M{"name": "a"}); n != 1 || err != nil {
		t.Fatal(n, err)
	}
}
