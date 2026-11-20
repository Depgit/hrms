package mdm

import (
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Publishing directly: event types, sequences, the snapshot's content, idempotency.
func TestPublishTypesAndSequence(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	addEmployee(t, ctx, st, o, "EMP-1001", "Asha Rao", nil)
	addEmployee(t, ctx, st, o, "EMP-1002", "Vikram Sen", bson.M{"manager_id": "EMP-1001"})
	if n, err := svc.Publish(ctx, []string{"EMP-1002", "EMP-1001", "EMP-1001"}, nil); err != nil || n != 2 {
		t.Fatalf("first publish: %d %v", n, err)
	}
	if n, err := svc.Publish(ctx, []string{"EMP-1001", "EMP-1002"}, nil); err != nil || n != 0 {
		t.Fatalf("publishing unchanged employees wrote %d events (%v)", n, err)
	}
	rows := outbox(t, ctx, st)
	if got, want := summarize(rows), []evSummary{{EmployeeCreated, "EMP-1001", 1}, {EmployeeCreated, "EMP-1002", 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if i64(rows[0]["pos"]) != 1 || i64(rows[1]["pos"]) != 2 {
		t.Fatalf("positions %v %v", rows[0]["pos"], rows[1]["pos"])
	}
	d, _ := rows[1]["data"].(map[string]any)
	want := map[string]any{"employee_code": "EMP-1002", "name": "Vikram Sen", "status": "Active", "exit_status": nil, "employment_type": "Permanent",
		"designation": "Steward", "department": "Food & Beverage", "grade": "G1", "location": "Saket", "property_id": nil,
		"manager_code": "EMP-1001", "doj": "2026-01-05", "lwd": nil}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("snapshot\n%v\nwant\n%v", d, want)
	}
	if len(d) != len(SnapshotFields) {
		t.Fatalf("snapshot has %d keys, documented %d", len(d), len(SnapshotFields))
	}
	if ts := str(rows[0]["timestamp"]); len(ts) != 24 || ts[len(ts)-1] != 'Z' {
		t.Fatalf("timestamp %q is not UTC RFC 3339 with milliseconds", ts)
	}
	if string(Canonical(d)) == "" || Hash(d) != Hash(want) {
		t.Fatal("hash differs for equal snapshots")
	}
	pub, _ := st.DB.C(collPublished).FindOne(ctx, bson.M{"employee_code": "EMP-1002"})
	if str(pub["hash"]) != Hash(want) || i64(pub["sequence"]) != 1 || i64(pub["pos"]) != 2 || str(pub["event_id"]) != str(rows[1]["event_id"]) {
		t.Fatalf("published %v", pub)
	}

	setEmp(t, ctx, st, "EMP-1001", bson.M{"status": "Inactive", "exit_status": "Exited", "lwd": "2026-11-10"})
	setEmp(t, ctx, st, "EMP-1002", bson.M{"name": "Vikram Sen Gupta"})
	if _, err := svc.Publish(ctx, []string{"EMP-1001", "EMP-1002"}, nil); err != nil {
		t.Fatal(err)
	}
	setEmp(t, ctx, st, "EMP-1001", bson.M{"status": "Active", "exit_status": nil, "lwd": nil})
	if _, err := svc.Publish(ctx, []string{"EMP-1001"}, nil); err != nil {
		t.Fatal(err)
	}
	got := summarize(outbox(t, ctx, st)[2:])
	if want := []evSummary{{EmployeeSeparated, "EMP-1001", 2}, {EmployeeUpdated, "EMP-1002", 2}, {EmployeeReactivated, "EMP-1001", 3}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// The watcher: an insert or update emits exactly one event; unpublished fields and _lock bumps emit nothing; a
// designation rename emits updates for its employees.
func TestWatcherEmits(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	addEmployee(t, ctx, st, o, "EMP-2001", "Meera Nair", bson.M{"designation_id": o.desig2, "department_id": o.dept2})
	w, _ := startWatcher(t, svc, "server-a")
	watcherReady(t, ctx, st, w)
	if got := summarize(outbox(t, ctx, st)); !reflect.DeepEqual(got, []evSummary{{EmployeeCreated, "EMP-2001", 1}}) {
		t.Fatalf("start-up rescan: %v", got)
	}

	addEmployee(t, ctx, st, o, "EMP-2002", "Rahul Das", nil)
	waitFor(t, "the insert's event", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) >= 2 })
	setEmp(t, ctx, st, "EMP-2002", bson.M{"name": "Rahul K Das"})
	waitFor(t, "the update's event", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) >= 3 })
	time.Sleep(300 * time.Millisecond)
	rows := outbox(t, ctx, st)
	if got, want := summarize(rows), []evSummary{{EmployeeCreated, "EMP-2001", 1}, {EmployeeCreated, "EMP-2002", 1}, {EmployeeUpdated, "EMP-2002", 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if d, _ := rows[2]["data"].(map[string]any); d["name"] != "Rahul K Das" {
		t.Fatalf("update carries %v", d)
	}

	// Fields the snapshot does not show.
	b := w.Batches()
	setEmp(t, ctx, st, "EMP-2002", bson.M{"verified": true, "attendance_mode": "Mobile app", "resign_at": "2026-12-01"})
	settle(t, w, b)
	// SELECT … FOR UPDATE: the hidden _lock bump.
	b = w.Batches()
	if err := st.Tx(ctx, func(tx store.Q) error {
		_, err := tx.C("employee").Lock(ctx, bson.M{"_id": "EMP-2002"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	settle(t, w, b)
	// A master change that is not a name.
	b = w.Batches()
	if _, err := st.DB.C("designation").Update(ctx, bson.M{"_id": o.desig}, bson.M{"$set": bson.M{"grades": []string{"G1", "G2"}}}); err != nil {
		t.Fatal(err)
	}
	settle(t, w, b)
	if n := len(outbox(t, ctx, st)); n != 3 {
		t.Fatalf("unpublished changes emitted events: %d", n)
	}

	// A designation rename: an update for each of its employees, and only them.
	addEmployee(t, ctx, st, o, "EMP-2003", "Imran Ali", nil)
	waitFor(t, "EMP-2003's event", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) == 4 })
	if _, err := st.DB.C("designation").Update(ctx, bson.M{"_id": o.desig}, bson.M{"$set": bson.M{"name": "Senior Steward"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rename's events", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) >= 6 })
	time.Sleep(300 * time.Millisecond)
	rows = outbox(t, ctx, st)
	if got, want := sortedSummaries(rows[4:]), []evSummary{{EmployeeUpdated, "EMP-2002", 3}, {EmployeeUpdated, "EMP-2003", 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rename events %v, want %v", got, want)
	}
	for _, r := range rows[4:] {
		if d, _ := r["data"].(map[string]any); d["designation"] != "Senior Steward" {
			t.Fatalf("rename event carries %v", d)
		}
	}
	// A location rename and a link to a CMS property reach its employees too.
	if _, err := st.DB.C("location").Update(ctx, bson.M{"_id": o.loc}, bson.M{"$set": bson.M{"property_id": "P-77"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link's events", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) >= 8 })
	for _, r := range outbox(t, ctx, st)[6:] {
		if d, _ := r["data"].(map[string]any); d["property_id"] != "P-77" || d["location"] != "Saket" {
			t.Fatalf("link event carries %v", d)
		}
	}
}

// Two watchers (two servers): one leads and writes; the other takes over when the first stops.
func TestTwoWatchers(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	svc2 := newSvc(openStore(t), Options{})
	w1, stop1 := startWatcher(t, svc, "server-1")
	watcherReady(t, ctx, st, w1)
	w2, _ := startWatcher(t, svc2, "server-2")
	for i := 0; i < 10; i++ {
		if !w1.IsLeader() || w2.IsLeader() {
			t.Fatalf("leaders: server-1 %v, server-2 %v", w1.IsLeader(), w2.IsLeader())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i, code := range []string{"EMP-3001", "EMP-3002", "EMP-3003"} {
		addEmployee(t, ctx, st, o, code, "Person "+code, nil)
		setEmp(t, ctx, st, code, bson.M{"name": "Person " + code + " Jr"})
		waitFor(t, code+"'s events", 10*time.Second, func() bool {
			rows := outbox(t, ctx, st)
			return len(rows) >= i+1 && i64(rows[len(rows)-1]["sequence"]) >= 1
		})
	}
	waitFor(t, "every change published", 10*time.Second, func() bool {
		pubs, _ := st.DB.C(collPublished).Find(ctx, bson.M{"data.name": bson.M{"$regex": "Jr$"}})
		return len(pubs) == 3
	})
	time.Sleep(500 * time.Millisecond)
	seen := map[string]bool{}
	for _, r := range outbox(t, ctx, st) {
		k := str(r["employee_code"]) + "/" + str(r["data"].(map[string]any)["name"])
		if seen[k] {
			t.Fatalf("event %s written twice", k)
		}
		seen[k] = true
	}
	if w2.IsLeader() || w2.Batches() != 0 {
		t.Fatalf("the second watcher worked while the first led (batches %d)", w2.Batches())
	}

	before := len(outbox(t, ctx, st))
	stop1()
	waitFor(t, "server-2 to take over", 10*time.Second, func() bool {
		r, _ := st.DB.C(collState).FindOne(ctx, bson.M{"_id": stateWatcher})
		return w2.IsLeader() && r != nil && str(r["owner"]) == "server-2"
	})
	if w2.Rescans() != 0 {
		t.Fatal("server-2 rescanned instead of resuming from server-1's position")
	}
	addEmployee(t, ctx, st, o, "EMP-3004", "Late Joiner", nil)
	waitFor(t, "server-2's event", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) == before+1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(outbox(t, ctx, st)); n != before+1 {
		t.Fatalf("%d events after the takeover, want %d", n, before+1)
	}
}

// A watcher restarted after downtime resumes from the saved position: nothing missed, nothing repeated. Without a
// usable position it rescans.
func TestWatcherResume(t *testing.T) {
	svc, st, ctx := testEnv(t, Options{})
	o := seedOrg(t, ctx, st)
	w1, stop1 := startWatcher(t, svc, "server-1")
	watcherReady(t, ctx, st, w1)
	addEmployee(t, ctx, st, o, "EMP-4001", "First", nil)
	waitFor(t, "EMP-4001's event", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) == 1 })
	stop1()
	tok := func() string {
		r, _ := st.DB.C(collState).FindOne(ctx, bson.M{"_id": stateWatcher})
		return str(r["token"])
	}
	if tok() == "" {
		t.Fatal("no position saved")
	}

	// Downtime: changes nobody is watching.
	addEmployee(t, ctx, st, o, "EMP-4002", "Second", nil)
	setEmp(t, ctx, st, "EMP-4001", bson.M{"name": "First Renamed"})
	setEmp(t, ctx, st, "EMP-4002", bson.M{"verified": true})

	w2, stop2 := startWatcher(t, svc, "server-2")
	waitFor(t, "the missed changes", 10*time.Second, func() bool { return len(outbox(t, ctx, st)) >= 3 })
	time.Sleep(500 * time.Millisecond)
	got := sortedSummaries(outbox(t, ctx, st))
	if want := []evSummary{{EmployeeCreated, "EMP-4001", 1}, {EmployeeUpdated, "EMP-4001", 2}, {EmployeeCreated, "EMP-4002", 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after resuming: %v, want %v", got, want)
	}
	if w2.Rescans() != 0 {
		t.Fatal("the watcher rescanned although it had a position")
	}
	stop2()

	// The position is lost: a rescan publishes what changed meanwhile, once.
	if _, err := st.DB.C(collState).Update(ctx, bson.M{"_id": stateWatcher}, bson.M{"$set": bson.M{"token": nil}}); err != nil {
		t.Fatal(err)
	}
	setEmp(t, ctx, st, "EMP-4002", bson.M{"name": "Second Renamed"})
	w3, stop3 := startWatcher(t, svc, "server-3")
	watcherReady(t, ctx, st, w3)
	if w3.Rescans() != 1 {
		t.Fatalf("rescans %d, want 1", w3.Rescans())
	}
	if got := summarize(outbox(t, ctx, st)[3:]); !reflect.DeepEqual(got, []evSummary{{EmployeeUpdated, "EMP-4002", 2}}) {
		t.Fatalf("after the rescan: %v", got)
	}
	stop3()

	// A position the server refuses (an expired or damaged token) also falls back to a rescan.
	if _, err := st.DB.C(collState).Update(ctx, bson.M{"_id": stateWatcher}, bson.M{"$set": bson.M{"token": "8263F0C2D5000000012B022C0100296E5A1004"}}); err != nil {
		t.Fatal(err)
	}
	setEmp(t, ctx, st, "EMP-4001", bson.M{"status": "Inactive"})
	w4, _ := startWatcher(t, svc, "server-4")
	watcherReady(t, ctx, st, w4)
	// The refused token is already stored, so "ready" can be seen before the server has refused it: wait for the
	// rescan itself rather than checking right away.
	waitFor(t, "the rescan after a refused position", 10*time.Second, func() bool { return w4.Rescans() >= 1 })
	if w4.Rescans() != 1 {
		t.Fatalf("rescans %d after a refused position, want 1", w4.Rescans())
	}
	if got := summarize(outbox(t, ctx, st)[4:]); !reflect.DeepEqual(got, []evSummary{{EmployeeSeparated, "EMP-4001", 3}}) {
		t.Fatalf("after the refused position: %v", got)
	}
}
