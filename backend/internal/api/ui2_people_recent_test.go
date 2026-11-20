package api_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// The dashboard's scoped "Recent activity" must list exactly what the old EXISTS query listed (the per-row join is
// the reference), for every kind of scope, without joining every audit row of the window: a narrow scope over a
// large log used to take tens of seconds.
func TestUI2PeopleRecentScoped(t *testing.T) {
	_, _, st := setupFull(t)
	ctx := context.Background()
	db := st.DB.Database()
	rng := rand.New(rand.NewSource(7))

	emps, err := st.DB.C("employee").Find(ctx, bson.M{})
	if err != nil || len(emps) < 5 {
		t.Fatal("seed employees", err, len(emps))
	}
	depts, _ := st.DB.C("department").Find(ctx, bson.M{})
	if len(depts) == 0 {
		t.Fatal("seed departments")
	}
	pick := func(rows []store.Row) store.Row { return rows[rng.Intn(len(rows))] }
	most := func(key string) string {
		n := map[string]int{}
		best := ""
		for _, e := range emps {
			if v, ok := e[key].(string); ok && v != "" {
				if n[v]++; n[v] > n[best] {
					best = v
				}
			}
		}
		return best
	}
	mgr, loc := most("manager_id"), most("location_id")
	deptName := map[string]string{}
	for _, d := range depts {
		deptName[fmt.Sprint(d["id"])] = fmt.Sprint(d["name"])
	}
	var mgrDept string // a department some of the manager's reports are in
	for _, e := range emps {
		if e["manager_id"] == mgr {
			mgrDept = deptName[fmt.Sprint(e["department_id"])]
			break
		}
	}

	// Offers and candidates as request subjects, in and out of scope, one of each without a department.
	someDept := fmt.Sprint(depts[0]["id"])
	subject := func(coll string, i int, dept any, manager string) string {
		id := store.NewID()
		doc := bson.M{"_id": id, "manager_id": manager, "location_id": loc, "department_id": dept}
		if coll == "offer" {
			doc["candidate_name"] = fmt.Sprintf("Offer %d", i)
		} else {
			doc["name"], doc["no"] = fmt.Sprintf("Candidate %d", i), int32(900000+i)
		}
		if _, err := db.Collection(coll).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var reqIDs []any
	request := func(field, sid string) {
		id := store.NewID()
		doc := bson.M{"_id": id, "employee_id": nil, "offer_id": nil, "candidate_id": nil, "wf": "test", "status": "Pending"}
		doc[field] = sid
		if _, err := db.Collection("approval_request").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
		reqIDs = append(reqIDs, id)
	}
	for i := 0; i < 6; i++ {
		var dept any = someDept
		if i == 5 {
			dept = nil
		}
		other := fmt.Sprint(pick(emps)["id"])
		request("offer_id", subject("offer", i, dept, mgr))
		request("offer_id", subject("offer", 10+i, dept, other))
		request("candidate_id", subject("candidate", i, dept, mgr))
		request("candidate_id", subject("candidate", 10+i, dept, other))
		request("employee_id", fmt.Sprint(pick(emps)["id"]))
	}
	if seeded, _ := st.DB.C("approval_request").Distinct(ctx, "_id", bson.M{"wf": bson.M{"$ne": "test"}}); len(seeded) > 0 {
		reqIDs = append(reqIDs, seeded...)
	}

	// The audit log: activity about employees, requests and other things, some of it older than 180 days.
	last, _ := st.DB.C("audit_log").FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}}))
	next := int64(0)
	if last != nil {
		next = last["id"].(int64)
	}
	logRows := func(n int, gen func(i int) (entity string, id any, age time.Duration)) {
		docs := make([]any, 0, n)
		for i := 0; i < n; i++ {
			entity, id, age := gen(i)
			next++
			docs = append(docs, bson.M{"_id": next, "actor": fmt.Sprintf("Actor %d", next%7), "action": fmt.Sprintf("Action %d", next),
				"entity": entity, "entity_id": id, "at": time.Now().Add(-age)})
		}
		if _, err := db.Collection("audit_log").InsertMany(ctx, docs); err != nil {
			t.Fatal(err)
		}
		if err := store.SetSeq(ctx, st.DB, "audit_log_id_seq", next); err != nil { // later audit entries go after these
			t.Fatal(err)
		}
	}
	day := 24 * time.Hour
	logRows(1500, func(int) (string, any, time.Duration) {
		age := time.Duration(rng.Int63n(int64(200 * day)))
		switch r := rng.Intn(10); {
		case r < 5:
			return "employee", pick(emps)["id"], age
		case r < 8:
			return "approval_request", reqIDs[rng.Intn(len(reqIDs))], age
		case r == 8:
			return "employee", "EMP-NOBODY", age
		default:
			return "policy", pick(emps)["id"], age
		}
	})
	// The newest rows match nobody, so the list for everyone has to read past several batches.
	logRows(300, func(int) (string, any, time.Duration) { return "employee", "EMP-NOBODY", time.Minute })

	scopes := []access.Scope{
		{Access: "all"},
		{Access: "all", Departments: []string{mgrDept}},
		{Access: "locs", Locations: []string{loc}},
		{Access: "locs", Locations: []string{loc}, Departments: []string{mgrDept}},
		{Access: "team", EmployeeID: mgr},
		{Access: "team", EmployeeID: mgr, Departments: []string{mgrDept}},
		{Access: "self", EmployeeID: mgr},
		{Access: "self", EmployeeID: fmt.Sprint(emps[len(emps)-1]["id"])},
		{Access: "team", EmployeeID: "EMP-NOBODY"},
	}
	want := map[int][]store.Row{}
	for i, sc := range scopes {
		ref, err := api.UI2PeopleRecentJoined(ctx, st.DB, sc)
		if err != nil {
			t.Fatal(err)
		}
		got, err := api.UI2PeopleRecent(ctx, st.DB, sc)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(ref) {
			t.Fatalf("scope %+v:\n got %v\nwant %v", sc, got, ref)
		}
		if sc.Access == "all" && len(got) != 8 {
			t.Fatalf("scope %+v: want 8 rows, got %d", sc, len(got))
		}
		want[i] = ref
	}

	// A large log about people outside a narrow scope: still the same answer, and quickly.
	logRows(30000, func(int) (string, any, time.Duration) {
		return "employee", "EMP-NOBODY", time.Duration(rng.Int63n(int64(day)))
	})
	for i, sc := range scopes {
		start := time.Now()
		got, err := api.UI2PeopleRecent(ctx, st.DB, sc)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("scope %+v took %v", sc, d)
		}
		if fmt.Sprint(got) != fmt.Sprint(want[i]) {
			t.Fatalf("scope %+v after the unmatched rows:\n got %v\nwant %v", sc, got, want[i])
		}
	}
}
