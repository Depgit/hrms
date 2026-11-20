package mdm

// Tests against a real MongoDB replica set (change streams and transactions need one). Set HRMS_TEST_MONGO_URL,
// e.g. mongodb://localhost:27017/?replicaSet=rs0&directConnection=true; HRMS_TEST_MONGO_MDM_DB names the database
// (default hrms_test_mdm), which is dropped and rebuilt. Skipped otherwise.

import (
	"context"
	"log"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

var (
	testOnce sync.Once
	testErr  error
	testKey  = []byte("0123456789abcdef0123456789abcdef")
)

func testDB(t *testing.T) (url, name string) {
	t.Helper()
	url = os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	name = os.Getenv("HRMS_TEST_MONGO_MDM_DB")
	if name == "" {
		name = "hrms_test_mdm"
	}
	return url, name
}

// openStore opens another client on the test database (a second "server").
func openStore(t *testing.T) *store.Store {
	t.Helper()
	url, name := testDB(t)
	st, err := store.Open(context.Background(), url, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// testEnv gives a freshly reset database and a service over it.
func testEnv(t *testing.T, o Options) (*Service, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	st := openStore(t)
	testOnce.Do(func() {
		if testErr = st.DB.Database().Drop(ctx); testErr != nil {
			return
		}
		testErr = st.Migrate(ctx, func(string) {})
	})
	if testErr != nil {
		t.Fatal(testErr)
	}
	if err := st.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	return newSvc(st, o), st, ctx
}

func newSvc(st *store.Store, o Options) *Service {
	if o.Key == nil {
		o.Key = testKey
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	return New(st, o)
}

type org struct {
	dept, dept2, desig, desig2, loc, loc2, cluster string
}

func ins(t *testing.T, ctx context.Context, st *store.Store, coll string, doc bson.M) string {
	t.Helper()
	id, err := st.DB.C(coll).Insert(ctx, doc)
	if err != nil {
		t.Fatalf("insert %s: %v", coll, err)
	}
	return id.(string)
}

func seedOrg(t *testing.T, ctx context.Context, st *store.Store) org {
	t.Helper()
	var o org
	region := ins(t, ctx, st, "region", bson.M{"name": "North"})
	state := ins(t, ctx, st, "state", bson.M{"name": "Delhi", "region_id": region})
	city := ins(t, ctx, st, "city", bson.M{"name": "New Delhi", "state_id": state})
	o.cluster = ins(t, ctx, st, "cluster", bson.M{"name": "South Delhi", "city_id": city})
	o.loc = ins(t, ctx, st, "location", bson.M{"name": "Saket", "cluster_id": o.cluster})
	o.loc2 = ins(t, ctx, st, "location", bson.M{"name": "Noida 62", "cluster_id": o.cluster})
	ins(t, ctx, st, "grade", bson.M{"name": "G1"})
	ins(t, ctx, st, "grade", bson.M{"name": "G2"})
	o.dept = ins(t, ctx, st, "department", bson.M{"name": "Food & Beverage"})
	o.dept2 = ins(t, ctx, st, "department", bson.M{"name": "Housekeeping"})
	o.desig = ins(t, ctx, st, "designation", bson.M{"name": "Steward", "department_id": o.dept, "grades": []string{"G1"}})
	o.desig2 = ins(t, ctx, st, "designation", bson.M{"name": "Room Attendant", "department_id": o.dept2, "grades": []string{"G1", "G2"}})
	return o
}

func addEmployee(t *testing.T, ctx context.Context, st *store.Store, o org, code, name string, extra bson.M) {
	t.Helper()
	doc := bson.M{"_id": code, "name": name, "type": "Permanent", "doj": "2026-01-05", "department_id": o.dept, "designation_id": o.desig,
		"grade": "G1", "location_id": o.loc, "shift_code": "G", "status": "Active"}
	for k, v := range extra {
		doc[k] = v
	}
	ins(t, ctx, st, "employee", doc)
}

func setEmp(t *testing.T, ctx context.Context, st *store.Store, code string, set bson.M) {
	t.Helper()
	if n, err := st.DB.C("employee").Update(ctx, bson.M{"_id": code}, bson.M{"$set": set}); err != nil || n != 1 {
		t.Fatalf("update %s: %d %v", code, n, err)
	}
}

func outbox(t *testing.T, ctx context.Context, st *store.Store) []store.Row {
	t.Helper()
	rows, err := st.DB.C(collOutbox).Find(ctx, nil, findSortLimit(bson.D{{Key: "pos", Value: 1}}, 0))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// startWatcher runs a watcher until the returned stop is called (also at the end of the test).
func startWatcher(t *testing.T, svc *Service, id string) (*Watcher, func()) {
	t.Helper()
	w := svc.NewWatcher(id)
	w.TTL, w.Retry, w.Await, w.SaveEvery = 3*time.Second, 100*time.Millisecond, 200*time.Millisecond, time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return w, stop
}

// watcherReady waits until w leads and has saved a stream position (its start-up rescan is done).
func watcherReady(t *testing.T, ctx context.Context, st *store.Store, w *Watcher) {
	t.Helper()
	waitFor(t, "the watcher to lead", 10*time.Second, func() bool {
		r, _ := st.DB.C(collState).FindOne(ctx, bson.M{"_id": stateWatcher})
		return w.IsLeader() && r != nil && str(r["owner"]) == w.id && str(r["token"]) != ""
	})
}

// settle waits until w has handled a batch after the moment it is called (the change just made has been seen).
func settle(t *testing.T, w *Watcher, from int64) {
	t.Helper()
	waitFor(t, "the watcher to see the change", 10*time.Second, func() bool { return w.Batches() > from })
}

type evSummary struct {
	Type string
	Code string
	Seq  int64
}

func summarize(rows []store.Row) []evSummary {
	out := make([]evSummary, len(rows))
	for i, r := range rows {
		out[i] = evSummary{str(r["event_type"]), str(r["employee_code"]), i64(r["sequence"])}
	}
	return out
}

func sortedSummaries(rows []store.Row) []evSummary {
	out := summarize(rows)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}
