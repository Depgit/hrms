package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

// GET /api/separations is filtered and limited in the database (the old WHERE … ORDER BY started_at DESC LIMIT
// 500) and reads each row's employee, location, department, designation and clearance in batches, not per case.
// The cases listed must still be exactly what the old query returned: an oracle below applies the old rules to
// every case.
func TestSepListFilteredAndBatched(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	q := st.DB
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1010", "EMP-1020")

	// Two real cases (with approval requests): a resignation and an HR-started termination.
	if _, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}}); r["case"] == nil {
		t.Fatal(r)
	}
	if s, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "type": "Termination", "reason": "Misconduct",
		"noticeOption": "pay", "note": "Repeated cash shortages after two warnings"}); s != 200 {
		t.Fatalf("termination: %d %v", s, r)
	}
	// 530 older closed or cancelled cases spread over everyone, including the callers themselves (a cancelled
	// termination of the caller is hidden from them).
	emps, err := q.C("employee").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil || len(emps) < 10 {
		t.Fatal(err, len(emps))
	}
	base := time.Now().Add(-48 * time.Hour)
	docs := []any{}
	for i := 0; i < 530; i++ {
		e := emps[(i*7)%len(emps)]
		typ, status := "Resignation", "Closed"
		if i%5 == 0 {
			typ, status = "Termination", "Cancelled"
		}
		docs = append(docs, bson.M{"_id": store.NewID(), "employee_id": e["id"], "type": typ, "status": status, "given_on": "2026-01-05",
			"started_by": "EMP-1002", "started_at": base.Add(-time.Duration(i) * time.Minute)})
	}
	for _, me := range []string{"EMP-1002", "EMP-1004", "EMP-1009", "EMP-1010"} { // the newest closed-out cases are about the callers
		docs = append(docs, bson.M{"_id": store.NewID(), "employee_id": me, "type": "Termination", "status": "Cancelled", "given_on": "2026-01-05",
			"started_by": "EMP-1002", "started_at": base.Add(time.Minute)})
	}
	if err := q.C("separation").InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}

	for _, who := range []struct{ emp, role string }{{"EMP-1002", "HR Admin"}, {"EMP-1004", "HRBP"}, {"EMP-1010", "Function Head"},
		{"EMP-1009", "Line Manager"}, {"EMP-1009", "Employee"}, {"EMP-1020", "Employee"}} {
		t.Run(who.emp+" as "+who.role, func(t *testing.T) {
			want := sepListOracle(t, q, who.emp, who.role)
			s, l := c.do("GET", "/api/separations", who.emp, who.role, nil)
			if s != 200 {
				t.Fatalf("%d %v", s, l)
			}
			got := []string{}
			for _, x := range l["cases"].([]any) {
				got = append(got, x.(map[string]any)["id"].(string))
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("listed %d cases, want %d (first difference matters)\n got %.300v\nwant %.300v", len(got), len(want), got, want)
			}
			t.Logf("%d cases", len(want))
			total := 0
			for _, n := range l["counts"].(map[string]any) {
				total += int(num(n))
			}
			if total != len(want) {
				t.Fatalf("counts add up to %d, want %d", total, len(want))
			}
			if who.role == "HR Admin" && len(want) != 500 {
				t.Fatalf("HR Admin oracle has %d cases, want the 500 limit", len(want))
			}
		})
	}

	t.Run("rows carry the employee's names and manager", func(t *testing.T) {
		_, l := c.do("GET", "/api/separations", "EMP-1002", "HR Admin", nil)
		for _, x := range l["cases"].([]any) {
			row := x.(map[string]any)
			e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": row["employeeId"]})
			loc, _ := q.C("location").FindOne(ctx, bson.M{"_id": e["location_id"]})
			dept, _ := q.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]})
			desig, _ := q.C("designation").FindOne(ctx, bson.M{"_id": e["designation_id"]})
			var name, ln, dn, gn string
			var mgr any
			if loc != nil && dept != nil && desig != nil {
				name, ln, dn, gn, mgr = e["name"].(string), loc["name"].(string), dept["name"].(string), desig["name"].(string), e["manager_id"]
			}
			if row["name"] != name || row["location"] != ln || row["dept"] != dn || row["designation"] != gn || row["managerId"] != mgr {
				t.Fatalf("row %v, want %q %q %q %q %v", row, name, ln, dn, gn, mgr)
			}
		}
	})

	t.Run("a whole list reads each collection a fixed number of times", func(t *testing.T) {
		db := q.Database()
		if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err(); err != nil {
			t.Skip("profiler not available:", err)
		}
		defer db.RunCommand(ctx, bson.D{{Key: "profile", Value: 0}})
		from := time.Now()
		if s, _ := c.do("GET", "/api/separations", "EMP-1002", "HR Admin", nil); s != 200 {
			t.Fatal(s)
		}
		_ = db.RunCommand(ctx, bson.D{{Key: "profile", Value: 0}})
		// (separation itself is read once more per closed case, by closedNext, as the old code did.)
		for _, coll := range []string{"location", "designation", "department", "clearance", "clearance_item"} {
			n, err := db.Collection("system.profile").CountDocuments(ctx, bson.M{"ns": db.Name() + "." + coll, "ts": bson.M{"$gte": from}})
			if err != nil {
				t.Fatal(err)
			}
			if n > 5 {
				t.Errorf("%s read %d times for one list of 500 cases", coll, n)
			}
		}
	})
}

// sepListOracle applies the old SQL to every case: `separation s JOIN employee e JOIN department d LEFT JOIN
// approval_request r WHERE (scope OR e.manager_id=me) AND NOT (s.employee_id=me AND hidden) ORDER BY s.started_at
// DESC LIMIT 500`, with the scope and the emp.sep level loadCtx gives the caller.
func sepListOracle(t *testing.T, q store.Q, emp, role string) []string {
	t.Helper()
	ctx := context.Background()
	sc := access.Scope{Access: "self", EmployeeID: emp}
	switch role {
	case "Employee":
	case "Line Manager":
		sc.Access = "team"
	default:
		m, err := q.C("user_role_map").FindOne(ctx, bson.M{"employee_id": emp, "role_name": role, "active": true})
		if err != nil || m == nil {
			t.Fatalf("no %s mapping for %s: %v", role, emp, err)
		}
		sc.Access = m["access"].(string)
		sc.Locations, sc.Departments = sepOracleStrs(m["location_ids"]), sepOracleStrs(m["departments"])
	}
	level := 0
	if r, _ := q.C("role_right").FindOne(ctx, bson.M{"role_name": role, "right_id": "emp.sep"}); r != nil {
		level = int(r["level"].(int16))
	}
	seps, err := q.C("separation").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "started_at", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		t.Fatal(err)
	}
	byID := func(coll string) map[any]store.Row {
		rows, err := q.C(coll).Find(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		m := map[any]store.Row{}
		for _, r := range rows {
			m[r["id"]] = r
		}
		return m
	}
	emps, depts, reqs := byID("employee"), byID("department"), byID("approval_request")
	out := []string{}
	for _, s := range seps {
		e := emps[s["employee_id"]]
		if e == nil {
			continue
		}
		d := depts[e["department_id"]]
		if d == nil {
			continue
		}
		mgr, _ := e["manager_id"].(string)
		loc, _ := e["location_id"].(string)
		ok := mgr == emp || (level >= 1 && sc.Allows(access.ScopedEmployee{ID: e["id"].(string), LocationID: loc, Department: d["name"].(string), ManagerID: mgr}))
		reqStatus := ""
		if s["request_id"] != nil {
			if r := reqs[s["request_id"]]; r != nil {
				reqStatus, _ = r["status"].(string)
			}
		}
		hidden := s["employee_id"] == emp && s["type"] != "Resignation" && reqStatus != "Approved"
		if ok && !hidden {
			out = append(out, s["id"].(string))
		}
		if len(out) == 500 {
			break
		}
	}
	return out
}

// sepOracleStrs is a text[] column as strings (nil when it is empty or null).
func sepOracleStrs(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
	case []string:
		out = append(out, l...)
	}
	return out
}
