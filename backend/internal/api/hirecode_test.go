package api_test

// Hiring IDs of imported employees from the Employee code (owner's rule of 24 Nov 2026, store/schema_hirecode.go):
// EMP-000004 → SI-000004; a number a candidate or another employee has falls back to the candidate series;
// candidate_seq moves past the numbers used; migration 025 rewrites existing employees and leaves hires alone.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// hcTimeline is the employee's status timeline: the hiring IDs and the "Hiring IDs: …" entries.
func hcTimeline(t *testing.T, c *client, id string) (map[string]any, []map[string]any) {
	t.Helper()
	s, tl := c.do("GET", "/api/employees/"+id+"/timeline", "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("timeline %s: %v", id, tl)
	}
	var ev []map[string]any
	for _, e := range tl["events"].([]any) {
		if m := e.(map[string]any); strings.HasPrefix(fmt.Sprint(m["action"]), "Hiring IDs: ") {
			ev = append(ev, m)
		}
	}
	ids, _ := tl["hireIds"].(map[string]any)
	return ids, ev
}

func hcCounter(t *testing.T, st *store.Store) int {
	t.Helper()
	x, err := st.DB.C("counters").FindOne(context.Background(), bson.D{{Key: "_id", Value: "candidate_seq"}})
	if err != nil || x == nil {
		t.Fatal("no candidate_seq counter", err)
	}
	return toIntT(x["value"])
}

func TestHireIDsFromCodeImport(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	si, _ := hireAccepted(t, c, st, "Asha", "Rao", "9876523456") // candidate number 1
	row := func(code string) string {
		return "\n" + code + ",New " + code + hfImportBase + "Kitchen,Cooking,,,,,,,,,,,,,,,,,"
	}
	file := hfImportHead + row("ZQ-000004") + row("YR-0001") + row("XP-04") + row("7350") + "\n"
	if _, r := c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(file)); r["ok"] != 4.0 {
		t.Fatal(r)
	}
	// The code's digits, exactly.
	ids, ev := hcTimeline(t, c, "ZQ-000004")
	if ids["si"] != "SI-000004" || ids["oi"] != "OI-000004" || ids["ji"] != "JI-000004" || ids["via"] != "code" || len(ev) != 1 || ev[0]["action"] != "Hiring IDs: SI-000004, OI-000004, JI-000004" {
		t.Fatalf("from the code: %v %v", ids, ev)
	}
	if e := stagesEmployee(st, "ZQ-000004"); toIntT(e["hire_no"]) != 4 || e["hire_digits"] != "000004" {
		t.Fatalf("stored: %v %v", e["hire_no"], e["hire_digits"])
	}
	if ids, _ := hcTimeline(t, c, "7350"); ids["si"] != "SI-7350" {
		t.Fatalf("digits only: %v", ids)
	}
	// Taken by a candidate, or by an employee earlier in the same file: the candidate series, and the timeline says why.
	for code, who := range map[string]string{"YR-0001": "candidate " + si, "XP-04": "employee ZQ-000004"} {
		e := stagesEmployee(st, code)
		ids, ev := hcTimeline(t, c, code)
		if e["hire_digits"] != nil || toIntT(e["hire_no"]) <= 1 || toIntT(e["hire_no"]) == 4 || ids["via"] != "import" || ids["si"] != fmt.Sprintf("SI-%04d", toIntT(e["hire_no"])) ||
			len(ev) != 1 || !strings.Contains(fmt.Sprint(ev[0]["reason"]), "already used by "+who) {
			t.Fatalf("%s: %v %v %v", code, e["hire_no"], ids, ev)
		}
	}
	if a, b := toIntT(stagesEmployee(st, "YR-0001")["hire_no"]), toIntT(stagesEmployee(st, "XP-04")["hire_no"]); a == b {
		t.Fatalf("one number twice: %d", a)
	}
	// candidate_seq moved past the highest number used: the next shortlist is SI-7351.
	if n := hcCounter(t, st); n < 7350 {
		t.Fatalf("candidate_seq %d", n)
	}
	if next := selectCandidate(t, c, shortlistBody(st, "Ravi", "Das", "9876500001")); next != "SI-7351" {
		t.Fatalf("next candidate %s", next)
	}
}

func TestHireIDsFromCodeMigration(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	ctx := context.Background()
	si, _ := hireAccepted(t, c, st, "Asha", "Rao", "9876523456")
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
		t.Fatal(r)
	}
	hire := hireCand(t, c, si)["employee_id"].(string)
	// A second series with a number the sample staff have (EMP-1005), imported before the rule: as migration 018 left it.
	file := hfImportHead + "\nCQ-1005,Second Series" + hfImportBase + "Kitchen,Cooking,,,,,,,,,,,,,,,,,\n"
	if _, r := c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(file)); r["ok"] != 1.0 {
		t.Fatal(r)
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "CQ-1005"}, bson.M{"$set": bson.M{"hire_no": nil, "hire_digits": nil}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C(store.LifecycleEventColl).UpdateMany(ctx, bson.M{"employee_id": "CQ-1005"}, bson.M{"$set": bson.M{"action": "Hiring IDs: old"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackfillHireNos(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	hireBefore := stagesEmployee(st, hire)
	_, hireEv := hcTimeline(t, c, hire)
	series := toIntT(stagesEmployee(st, "EMP-1020")["hire_no"])
	n, err := store.HireIDsFromCode(ctx, st.DB)
	if err != nil || n == 0 {
		t.Fatalf("migration: %d %v", n, err)
	}
	// The sample staff take their code's number; the one timeline entry is rewritten, not added to.
	ids, ev := hcTimeline(t, c, "EMP-1020")
	if ids["si"] != "SI-1020" || ids["ji"] != "JI-1020" || ids["via"] != "code" || len(ev) != 1 || ev[0]["action"] != "Hiring IDs: SI-1020, OI-1020, JI-1020" ||
		!strings.Contains(fmt.Sprint(ev[0]["reason"]), "migration 025") || !strings.Contains(fmt.Sprint(ev[0]["reason"]), fmt.Sprintf("SI-%04d", series)) {
		t.Fatalf("EMP-1020: %v %v", ids, ev)
	}
	// Two series on one number: the first by joining date has it, the other keeps a candidate-series number.
	a, b := stagesEmployee(st, "EMP-1005"), stagesEmployee(st, "CQ-1005")
	if fmt.Sprint(a["doj"]) >= fmt.Sprint(b["doj"]) { // same day: by Employee code
		a, b = b, a
	}
	if toIntT(a["hire_no"]) != 1005 || a["hire_digits"] != "1005" || b["hire_digits"] != nil || toIntT(b["hire_no"]) == 1005 || toIntT(b["hire_no"]) <= 0 {
		t.Fatalf("collision: %v/%v %v/%v", a["hire_no"], a["hire_digits"], b["hire_no"], b["hire_digits"])
	}
	// A hire through a candidate is untouched.
	after := stagesEmployee(st, hire)
	if _, ev := hcTimeline(t, c, hire); toIntT(after["hire_no"]) != toIntT(hireBefore["hire_no"]) || after["hire_digits"] != nil || fmt.Sprint(ev) != fmt.Sprint(hireEv) {
		t.Fatalf("hire changed: %v %v", after["hire_no"], ev)
	}
	// Numbers unique, candidate_seq past them all.
	snap := map[string]string{}
	seen, top := map[int]string{}, 0
	emps, _ := st.DB.C("employee").Find(ctx, bson.M{})
	for _, e := range emps {
		no := toIntT(e["hire_no"])
		if no <= 0 || seen[no] != "" {
			t.Fatalf("%v: number %d (also %s)", e["id"], no, seen[no])
		}
		seen[no], top = fmt.Sprint(e["id"]), max(top, no)
		snap[fmt.Sprint(e["id"])] = fmt.Sprint(e["hire_no"], e["hire_digits"])
	}
	if hcCounter(t, st) < top {
		t.Fatalf("candidate_seq %d below %d", hcCounter(t, st), top)
	}
	// Idempotent.
	events := apiCount(st, store.LifecycleEventColl, bson.M{"action": bson.M{"$regex": "^Hiring IDs"}})
	if n, err := store.HireIDsFromCode(ctx, st.DB); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
	emps, _ = st.DB.C("employee").Find(ctx, bson.M{})
	for _, e := range emps {
		if snap[fmt.Sprint(e["id"])] != fmt.Sprint(e["hire_no"], e["hire_digits"]) {
			t.Fatalf("second run changed %v", e["id"])
		}
	}
	if k := apiCount(st, store.LifecycleEventColl, bson.M{"action": bson.M{"$regex": "^Hiring IDs"}}); k != events {
		t.Fatalf("timeline entries %d → %d", events, k)
	}
}
