package api_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// GET /api/employees?q= finds people approximately (word order, partial words, the code without its prefix,
// initials, a typo), best match first, and never outside the caller's data scope.
func TestPeopleSearch(t *testing.T) {
	c := setup(t)
	for _, id := range []string{"EMP-1002", "EMP-1009", "EMP-1004"} {
		c.signIn(id)
	}
	search := func(emp, role, q, extra string) []map[string]any {
		t.Helper()
		s, b := c.do("GET", "/api/employees?size=20&q="+url.QueryEscape(q)+extra, emp, role, nil)
		if s != 200 {
			t.Fatalf("%s %q: %d %v", role, q, s, b)
		}
		out := []map[string]any{}
		for _, r := range b["rows"].([]any) {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	first := func(rows []map[string]any) string {
		if len(rows) == 0 {
			return ""
		}
		return rows[0]["id"].(string)
	}
	has := func(rows []map[string]any, id string) bool {
		for _, r := range rows {
			if r["id"] == id {
				return true
			}
		}
		return false
	}
	for _, x := range []struct{ q, want string }{
		{"Anil Kapoor", "EMP-1009"},
		{"kapoor anil", "EMP-1009"}, // word order
		{"kap an", "EMP-1009"},      // partial words
		{"1009", "EMP-1009"},        // code without prefix
		{"emp1009", "EMP-1009"},     // code without the dash
		{"EMP-1009", "EMP-1009"},
		{"kapor", "EMP-1009"},       // a typo
		{"anli kapoor", "EMP-1009"}, // two letters swapped
		{"rakesh bajaj", "EMP-1008"},
		{"bajaj rakesh", "EMP-1008"},
	} {
		if got := first(search("EMP-1002", "HR Admin", x.q, "")); got != x.want {
			t.Errorf("HR Admin %q: first %q, want %s", x.q, got, x.want)
		}
	}
	// Initials: "ak" finds Anil Kapoor, "rb" Rakesh Bajaj.
	if rows := search("EMP-1002", "HR Admin", "ak", ""); !has(rows, "EMP-1009") {
		t.Errorf("initials ak: %v", rows)
	}
	if rows := search("EMP-1002", "HR Admin", "rb", ""); !has(rows, "EMP-1008") {
		t.Errorf("initials rb: %v", rows)
	}
	// No fit, no rows; the total is the number that fit.
	if s, b := c.do("GET", "/api/employees?q=zzqqxx", "EMP-1002", "HR Admin", nil); s != 200 || b["total"].(float64) != 0 {
		t.Errorf("no match: %d %v", s, b)
	}
	// Other filters still apply.
	for _, r := range search("EMP-1002", "HR Admin", "a", "&status=Active&type=Permanent") {
		if r["status"] != "Active" || r["type"] != "Permanent" {
			t.Errorf("filter not kept: %v", r)
		}
	}
	// Scope: a line manager finds only their reports, whatever the query (a typo fallback included).
	all := search("EMP-1002", "HR Admin", "a", "")
	if len(all) == 0 {
		t.Fatal("HR Admin finds nobody for a")
	}
	for _, q := range []string{"a", "sandeep rawat", "sandep", "sr", "1002"} {
		for _, r := range search("EMP-1009", "Line Manager", q, "") {
			if r["manager_id"] != "EMP-1009" {
				t.Errorf("Line Manager %q found %v (manager %v), outside their team", q, r["id"], r["manager_id"])
			}
		}
	}
	if has(search("EMP-1009", "Line Manager", "Sandeep Rawat", ""), "EMP-1002") {
		t.Error("Line Manager found Sandeep Rawat")
	}
	// An HRBP (some locations) sees nobody outside them, typo pass included.
	s, b := c.do("GET", "/api/employees?size=200", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatalf("HRBP list: %d %v", s, b)
	}
	inScope := map[string]bool{}
	for _, r := range b["rows"].([]any) {
		inScope[r.(map[string]any)["id"].(string)] = true
	}
	s, b = c.do("GET", "/api/employees?size=200", "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatal(b)
	}
	outside := ""
	for _, r := range b["rows"].([]any) {
		m := r.(map[string]any)
		if !inScope[m["id"].(string)] {
			outside = m["name"].(string)
			break
		}
	}
	if outside == "" {
		t.Fatal("everyone is in the HRBP's scope; pick another HRBP")
	}
	for _, q := range []string{outside, outside + "x", "a"} {
		for _, r := range search("EMP-1004", "HRBP", q, "") {
			if !inScope[r["id"].(string)] {
				t.Errorf("HRBP %q found %v outside their scope", q, r["id"])
			}
		}
	}
	// The employee role has no directory: still refused.
	if s, _ := c.do("GET", "/api/employees?q=anil", "EMP-1009", "Employee", nil); s != 403 {
		t.Errorf("Employee role: %d, want 403", s)
	}
}

// The search stays fast with 3,000 more employees: a plain query and a typo (which looks at everyone in scope).
func TestPeopleSearchScale(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	src, err := st.DB.C("employee").FindOne(ctx, bson.D{{Key: "_id", Value: "EMP-1020"}})
	if err != nil || src == nil {
		t.Fatal("EMP-1020", err)
	}
	first := []string{"Rahul", "Priya", "Amit", "Neha", "Vikram", "Pooja", "Suresh", "Kavita", "Arjun", "Sneha", "Manoj", "Ritu"}
	last := []string{"Sharma", "Verma", "Gupta", "Singh", "Mehta", "Iyer", "Nair", "Reddy", "Joshi", "Chopra", "Malhotra", "Bhatia"}
	emps := []any{}
	for g := 1; g <= 3000; g++ {
		e := bson.M{"id": fmt.Sprintf("EMP-%d", 60000+g), "name": fmt.Sprintf("%s %s %d", first[g%len(first)], last[(g/len(first))%len(last)], g), "status": "Active", "verified": true}
		for _, k := range []string{"type", "doj", "department_id", "designation_id", "grade", "location_id", "shift_code", "manager_id", "attendance_mode"} {
			e[k] = src[k]
		}
		emps = append(emps, e)
	}
	if err := st.DB.C("employee").InsertMany(ctx, emps); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"priya sharma", "malhotra", "vikarm mehta", "rs", "anil kapor"} {
		t0 := time.Now()
		s, b := c.do("GET", "/api/employees?size=20&q="+url.QueryEscape(q), "EMP-1002", "HR Admin", nil)
		took := time.Since(t0)
		if s != 200 || b["total"].(float64) == 0 {
			t.Errorf("%q: %d %v", q, s, b["total"])
		}
		t.Logf("%q: %v matches in %v", q, b["total"], took)
		if took > 1500*time.Millisecond {
			t.Errorf("%q took %v", q, took)
		}
	}
}
