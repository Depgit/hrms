package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Sample org: EMP-1002 HR Admin, EMP-1004 HRBP (north), EMP-1009 Line Manager, EMP-1020 permanent staff.

func meRights(t *testing.T, c *client, emp, role string) map[string]any {
	t.Helper()
	s, me := c.do("GET", "/api/me", emp, role, nil)
	if s != 200 {
		t.Fatalf("me %s: %d %v", emp, s, me)
	}
	ctx, _ := me["ctx"].(map[string]any)
	r, _ := ctx["rights"].(map[string]any)
	return r
}

func TestPersonalRights(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	const hr, hrRole = "EMP-1002", "HR Admin"

	// A grant adds a right in every role view; the person's other rights stay.
	if v := meRights(t, c, "EMP-1009", "Line Manager")["emp.salary"]; v != nil && v.(float64) != 0 {
		t.Fatalf("line manager starts without salary: %v", v)
	}
	s, r := c.do("POST", "/api/user-rights", hr, hrRole, map[string]any{"employeeId": "EMP-1009", "right": "emp.salary", "kind": "grant", "level": 1, "reason": "Audit project"})
	if s != 200 {
		t.Fatalf("grant: %d %v", s, r)
	}
	grantID := r["id"].(string)
	for _, role := range []string{"Line Manager", "Employee"} {
		if v, _ := meRights(t, c, "EMP-1009", role)["emp.salary"].(float64); v != 1 {
			t.Fatalf("grant in the %s view: %v", role, v)
		}
	}
	// Bad requests: unknown right, a grant at No access, a last day in the past.
	for _, b := range []map[string]any{
		{"employeeId": "EMP-1009", "right": "nope", "kind": "grant", "level": 1},
		{"employeeId": "EMP-1009", "right": "emp.bank", "kind": "grant", "level": 0},
		{"employeeId": "EMP-1009", "right": "emp.bank", "kind": "grant", "level": 1, "until": "2020-01-01"},
	} {
		if s, _ := c.do("POST", "/api/user-rights", hr, hrRole, b); s != 400 {
			t.Fatalf("accepted %v: %d", b, s)
		}
	}
	// A block cuts a right the role gives; HR Admin cannot be blocked and keeps every right.
	if v, _ := meRights(t, c, "EMP-1004", "HRBP")["emp.export"].(float64); v != 2 {
		t.Fatalf("HRBP exports by default: %v", v)
	}
	if s, r := c.do("POST", "/api/user-rights", hr, hrRole, map[string]any{"employeeId": "EMP-1004", "right": "emp.export", "kind": "block", "level": 0, "until": time.Now().AddDate(0, 1, 0).Format("2006-01-02")}); s != 200 {
		t.Fatalf("block: %d %v", s, r)
	}
	if v, _ := meRights(t, c, "EMP-1004", "HRBP")["emp.export"].(float64); v != 0 {
		t.Fatalf("blocked export still %v", v)
	}
	if s, _ := c.do("GET", "/api/employees/export", "EMP-1004", "HRBP", nil); s == 200 {
		t.Fatal("blocked export must be refused")
	}
	if s, _ := c.do("POST", "/api/user-rights", hr, hrRole, map[string]any{"employeeId": hr, "right": "emp.salary", "kind": "block", "level": 0}); s != 400 {
		t.Fatalf("HR Admin blocked: %d", s)
	}
	rs := meRights(t, c, hr, hrRole)
	for _, k := range []string{"emp.salary", "acc.users", "pay.approve", "pol.policies"} {
		if v, _ := rs[k].(float64); v != 2 {
			t.Fatalf("HR Admin %s = %v", k, v)
		}
	}
	// Only an HR Admin gives or takes HR Admin, even with Role mapping edit given personally.
	if s, r := c.do("POST", "/api/user-rights", hr, hrRole, map[string]any{"employeeId": "EMP-1004", "right": "acc.users", "kind": "grant", "level": 2}); s != 200 {
		t.Fatalf("grant acc.users: %d %v", s, r)
	}
	adminMap := apiDoc(st, "user_role_map", bson.M{"employee_id": hr, "role_name": "HR Admin"})
	if s, _ := c.do("PATCH", "/api/role-map/"+adminMap["id"].(string), "EMP-1004", "HRBP", map[string]any{"active": false}); s != 403 {
		t.Fatalf("an HRBP switched HR Admin off: %d", s)
	}
	if s, _ := c.do("POST", "/api/role-map", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "role": "HR Admin", "access": "all"}); s != 403 {
		t.Fatalf("an HRBP gave HR Admin: %d", s)
	}
	// Someone who is not HR Admin gives a right only up to their own level.
	if s, _ := c.do("POST", "/api/user-rights", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "right": "pay.approve", "kind": "grant", "level": 2}); s != 403 {
		t.Fatalf("HRBP gave payroll approval: %d", s)
	}
	// The last HR Admin stays.
	if s, _ := c.do("PATCH", "/api/role-map/"+adminMap["id"].(string), hr, hrRole, map[string]any{"active": false}); s != 400 {
		t.Fatalf("last HR Admin switched off: %d", s)
	}

	// Who can see what: the level and where it comes from; the file downloads.
	s, eff := c.do("GET", "/api/access/person/EMP-1009/effective", hr, hrRole, nil)
	if s != 200 {
		t.Fatalf("effective: %d %v", s, eff)
	}
	found := false
	for _, x := range eff["rights"].([]any) {
		m := x.(map[string]any)
		if m["id"] == "emp.salary" {
			found = true
			srcs := fmt.Sprint(m["sources"])
			if m["level"].(float64) != 1 || !strings.Contains(srcs, "grant") {
				t.Fatalf("salary source %v", m)
			}
		}
	}
	if !found {
		t.Fatal("no emp.salary row")
	}
	if s, _ := c.getRaw("/api/access/person/EMP-1004/effective.xlsx", hr, hrRole); s != 200 {
		t.Fatalf("effective sheet %d", s)
	}
	if s, l := c.do("GET", "/api/user-rights?employee=EMP-1009", hr, hrRole, nil); s != 200 || len(l["list"].([]any)) != 1 {
		t.Fatalf("list %d %v", s, l)
	}
	// Ending a grant takes it away; the row stays as history.
	if s, r := c.do("DELETE", "/api/user-rights/"+grantID, hr, hrRole, nil); s != 200 {
		t.Fatalf("end: %d %v", s, r)
	}
	if v, _ := meRights(t, c, "EMP-1009", "Line Manager")["emp.salary"].(float64); v != 0 {
		t.Fatalf("ended grant still gives %v", v)
	}
	if n := apiCount(st, "user_right", bson.M{"employee_id": "EMP-1009"}); n != 1 {
		t.Fatalf("history rows %d", n)
	}
	if apiCount(st, "audit_log", bson.M{"entity": "user_right"}) < 4 {
		t.Fatal("personal rights are audited")
	}
}

func TestScopeByArea(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002")
	const hr, hrRole = "EMP-1002", "HR Admin"
	gurCluster := apiDoc(st, "cluster", bson.M{"name": "Gurugram Cluster 1"})
	if gurCluster == nil {
		t.Fatal("no Gurugram cluster")
	}
	locs, _ := st.DB.C("location").Distinct(ctx, "_id", bson.M{"cluster_id": gurCluster["id"]})
	want := apiCount(st, "employee", bson.M{"status": bson.M{"$ne": "Inactive"}, "location_id": bson.M{"$in": locs}})
	// Finance has no one-holder rule, so it can be mapped by cluster to anyone.
	if s, r := c.do("POST", "/api/role-map", hr, hrRole, map[string]any{"employeeId": "EMP-1020", "role": "Finance", "access": "clusters", "areaIds": []string{}}); s != 400 {
		t.Fatalf("no cluster picked: %d %v", s, r)
	}
	s, r := c.do("POST", "/api/role-map", hr, hrRole, map[string]any{"employeeId": "EMP-1020", "role": "Finance", "access": "clusters", "areaIds": []string{gurCluster["id"].(string)}})
	if s != 200 {
		t.Fatalf("map by cluster: %d %v", s, r)
	}
	visible := func() int {
		t.Helper()
		_, p := c.do("GET", "/api/access/person/EMP-1020?role=Finance", hr, hrRole, nil)
		return int(p["visible"].(float64))
	}
	if v := visible(); v != want {
		t.Fatalf("cluster scope sees %d, want %d", v, want)
	}
	// A location added to the cluster later is covered at once.
	newLoc, err := st.DB.C("location").Insert(ctx, bson.M{"name": "Gurugram – New Tower", "cluster_id": gurCluster["id"]})
	if err != nil {
		t.Fatal(err)
	}
	far := apiDoc(st, "employee", bson.M{"status": "Active", "location_id": bson.M{"$nin": locs}})
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": far["id"]}, bson.M{"$set": bson.M{"location_id": newLoc}}); err != nil {
		t.Fatal(err)
	}
	if v := visible(); v != want+1 {
		t.Fatalf("after a new location: %d, want %d", v, want+1)
	}
	// By state: Haryana holds Gurugram.
	har := apiDoc(st, "state", bson.M{"name": "Haryana"})
	m := apiDoc(st, "user_role_map", bson.M{"employee_id": "EMP-1020", "role_name": "Finance"})
	if s, r := c.do("PATCH", "/api/role-map/"+m["id"].(string), hr, hrRole, map[string]any{"access": "states", "areaIds": []string{har["id"].(string)}}); s != 200 {
		t.Fatalf("by state: %d %v", s, r)
	}
	if v := visible(); v != want+1 {
		t.Fatalf("state scope sees %d", v)
	}
	// The list shows the areas; mappings by locations read as before (no area_ids).
	_, l := c.do("GET", "/api/role-map", hr, hrRole, nil)
	for _, x := range l["list"].([]any) {
		row := x.(map[string]any)
		_, has := row["area_ids"]
		if row["employee_id"] == "EMP-1020" && row["role_name"] == "Finance" {
			if !has || row["access"] != "states" {
				t.Fatalf("area mapping row %v", row)
			}
		} else if has {
			t.Fatalf("area_ids on a mapping by locations: %v", row)
		}
	}
}

func TestPolicyConflictCheck(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	hub := apiDoc(st, "location", bson.M{"name": "Gurugram – Sector 29 Hub"})
	hubID := hub["id"].(string)
	// Who at the hub already gets a policy through a mapping (seeded: departments, employment types).
	frontOffice := apiDoc(st, "employee", bson.M{"location_id": hubID, "status": "Active", "_id": "EMP-1009"}) // Front Office
	if frontOffice == nil {
		t.Fatal("EMP-1009 is not at the hub")
	}
	newPolicy := func(name string) string {
		t.Helper()
		s, r := c.do("POST", "/api/policies", hr, hrRole, map[string]any{"name": name})
		if s != 200 {
			t.Fatal(r)
		}
		return r["id"].(string)
	}
	p1 := newPolicy("Hub 5.5 day")
	maps := map[string]any{"maps": []any{map[string]any{"level": "location", "ref": hubID, "on": true}}}

	// Preview: grouped by the policy and mapping the people have now.
	s, pv := c.do("POST", "/api/policies/"+p1+"/maps/preview", hr, hrRole, maps)
	if s != 200 {
		t.Fatalf("preview %d %v", s, pv)
	}
	groups := pv["conflicts"].([]any)
	if len(groups) == 0 {
		t.Fatalf("no conflicts at the hub: %v", pv)
	}
	var fo map[string]any
	for _, g := range groups {
		if m := g.(map[string]any); m["ref"] == "Front Office" {
			fo = m
		}
	}
	if fo == nil || !strings.Contains(fo["text"].(string), "Operations – 24x7 rotational is already applied here (through the Front Office department) to") {
		t.Fatalf("front office group %v", groups)
	}
	inConflict := int(pv["people"].(float64))

	// Without a choice the save is refused; nothing is stored.
	if s, r := c.do("PUT", "/api/policies/"+p1+"/maps", hr, hrRole, maps); s != 409 {
		t.Fatalf("conflicting save: %d %v", s, r)
	}
	if apiCount(st, "work_policy_map", bson.M{"policy_id": p1}) != 0 {
		t.Fatal("a refused save stored a mapping")
	}

	// Keep: they stay where they are as explicit exceptions; the new mapping covers the rest.
	body := map[string]any{"maps": maps["maps"], "resolution": map[string]any{"mode": "keep"}}
	if s, r := c.do("PUT", "/api/policies/"+p1+"/maps", hr, hrRole, body); s != 200 {
		t.Fatalf("keep: %d %v", s, r)
	}
	if n := apiCount(st, "work_policy_map", bson.M{"level": "employee", "exception": true, "on_map": true}); n != inConflict {
		t.Fatalf("exceptions %d, want %d", n, inConflict)
	}
	_, ep := c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil)
	if ep["policy"] != "Operations – 24x7 rotational" || ep["exception"] != true || !strings.Contains(ep["why"].(string), "Kept on") {
		t.Fatalf("effective policy after keep: %v", ep)
	}
	if others := ep["others"].([]any); len(others) < 2 {
		t.Fatalf("other matches: %v", others)
	}
	// The person sees their own; another employee's needs the directory.
	if s, _ := c.do("GET", "/api/employees/EMP-1020/policy", "EMP-1020", "Employee", nil); s != 200 {
		t.Fatalf("own policy: %d", s)
	}
	if s, _ := c.do("GET", "/api/employees/EMP-1009/policy", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("someone else's policy: %d", s)
	}

	// Replace on a second policy: the people in conflict move to it.
	p2 := newPolicy("Hub 5 day")
	s, pv = c.do("POST", "/api/policies/"+p2+"/maps/preview", hr, hrRole, maps)
	if s != 200 || len(pv["moves"].([]any)) != 1 {
		t.Fatalf("the hub mapping moving from Hub 5.5 day is a move: %v", pv)
	}
	body = map[string]any{"maps": maps["maps"], "resolution": map[string]any{"mode": "replace"}}
	if s, r := c.do("PUT", "/api/policies/"+p2+"/maps", hr, hrRole, body); s != 200 {
		t.Fatalf("replace: %d %v", s, r)
	}
	if _, ep = c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil); ep["policy"] != "Hub 5 day" || ep["exception"] != true {
		t.Fatalf("after replace: %v", ep)
	}

	// Pick: only the named person moves.
	p3 := newPolicy("Front office pilot")
	fmaps := map[string]any{"maps": []any{map[string]any{"level": "department", "ref": "Front Office", "on": true}}}
	_, pv = c.do("POST", "/api/policies/"+p3+"/maps/preview", hr, hrRole, fmaps)
	if int(pv["people"].(float64)) < 2 {
		t.Fatalf("front office conflicts: %v", pv)
	}
	if s, _ := c.do("PUT", "/api/policies/"+p3+"/maps", hr, hrRole, map[string]any{"maps": fmaps["maps"], "resolution": map[string]any{"mode": "pick", "move": []string{"EMP-0000"}}}); s != 400 {
		t.Fatalf("picking someone not in conflict: %d", s)
	}
	if s, r := c.do("PUT", "/api/policies/"+p3+"/maps", hr, hrRole, map[string]any{"maps": fmaps["maps"], "resolution": map[string]any{"mode": "pick", "move": []string{"EMP-1009"}}}); s != 200 {
		t.Fatalf("pick: %d %v", s, r)
	}
	if _, ep = c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil); ep["policy"] != "Front office pilot" {
		t.Fatalf("picked person: %v", ep)
	}
	other := apiDoc(st, "employee", bson.M{"status": "Active", "_id": bson.M{"$ne": "EMP-1009"}, "department_id": frontOffice["department_id"]})
	if _, ep = c.do("GET", "/api/employees/"+other["id"].(string)+"/policy", hr, hrRole, nil); ep["policy"] == "Front office pilot" {
		t.Fatalf("not picked but moved: %v", ep)
	}
	// An employee-level mapping is never a conflict (it is the explicit choice).
	if s, r := c.do("PUT", "/api/policies/"+p1+"/maps", hr, hrRole, map[string]any{"maps": []any{map[string]any{"level": "location", "ref": hubID, "on": false},
		map[string]any{"level": "employee", "ref": "EMP-1020", "on": true}}}); s != 200 {
		t.Fatalf("employee mapping: %d %v", s, r)
	}

	// Overlap report: everyone matched by mappings of two or more policies, with who wins.
	s, ov := c.do("GET", "/api/policies/overlaps", hr, hrRole, nil)
	if s != 200 || int(ov["total"].(float64)) == 0 {
		t.Fatalf("overlaps %d %v", s, ov)
	}
	row := ov["rows"].([]any)[0].(map[string]any)
	if ms := row["matches"].([]any); len(ms) < 2 || ms[0].(map[string]any)["wins"] != true {
		t.Fatalf("overlap row %v", row)
	}
	if s, _ := c.getRaw("/api/policies/overlaps.xlsx", hr, hrRole); s != 200 {
		t.Fatalf("overlap sheet %d", s)
	}
	_ = ctx
}

func TestCompanyPolicyAudienceAreas(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020", "EMP-1014")
	const hr, hrRole = "EMP-1002", "HR Admin"
	e20 := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	loc := apiDoc(st, "location", bson.M{"_id": e20["location_id"]})
	cl := apiDoc(st, "cluster", bson.M{"_id": loc["cluster_id"]})
	mumbai := apiDoc(st, "city", bson.M{"name": "Mumbai"})
	n := 0
	create := func(title string, aud map[string]any) string {
		t.Helper()
		n++
		s, doc := c.upload(hr, hrRole, "policy", fmt.Sprintf("DRAFT-0123456789%02d", n), "document")
		if s != 200 {
			t.Fatalf("upload %d %v", s, doc)
		}
		s, r := c.do("POST", "/api/company-policies", hr, hrRole, map[string]any{"title": title, "version": "1.0", "effectiveFrom": "2026-01-01", "audience": aud, "newJoiners": true, "docId": doc["id"], "publish": true})
		if s != 200 {
			t.Fatalf("create %s: %d %v", title, s, r)
		}
		return r["id"].(string)
	}
	create("Cluster rule", map[string]any{"clusters": []string{cl["id"].(string)}})
	create("Mumbai rule", map[string]any{"cities": []string{mumbai["id"].(string)}})
	create("Named rule", map[string]any{"employees": []string{"EMP-1014"}})
	if s, _ := c.do("POST", "/api/company-policies", hr, hrRole, map[string]any{"title": "Bad", "version": "1.0", "effectiveFrom": "2026-01-01", "audience": map[string]any{"employees": []string{"EMP-0000"}}}); s != 400 {
		t.Fatalf("unknown employee accepted: %d", s)
	}
	titles := func(emp string) string {
		_, m := c.do("GET", "/api/me/policies", emp, "Employee", nil)
		out := []string{}
		for _, x := range m["items"].([]any) {
			out = append(out, x.(map[string]any)["title"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := titles("EMP-1020"); !strings.Contains(got, "Cluster rule") || strings.Contains(got, "Mumbai rule") || strings.Contains(got, "Named rule") {
		t.Fatalf("EMP-1020 policies: %s", got)
	}
	if got := titles("EMP-1014"); !strings.Contains(got, "Named rule") {
		t.Fatalf("EMP-1014 policies: %s", got)
	}
}

func TestLeaveEligibilityByType(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	types := func() []string {
		t.Helper()
		_, b := c.do("GET", "/api/leave/balances/EMP-1020", hr, hrRole, nil)
		out := []string{}
		for _, x := range b["types"].([]any) {
			out = append(out, x.(map[string]any)["type"].(string))
		}
		return out
	}
	before := types()
	if !containsS(before, "Casual leave") || !containsS(before, "Sick leave") {
		t.Fatalf("permanent staff types: %v", before)
	}
	rules := map[string]any{"statutory": true, "lwf": true, "bonus": true, "grat": true, "plEnc": true, "variable": true, "ded": "all", "leave": true, "docs": true}
	put := func(extra map[string]any) (int, map[string]any) {
		b := map[string]any{}
		for k, v := range rules {
			b[k] = v
		}
		for k, v := range extra {
			b[k] = v
		}
		return c.do("PUT", "/api/comp/types/Permanent", hr, hrRole, b)
	}
	if s, _ := put(map[string]any{"leaveTypes": []string{"No such leave"}}); s != 400 {
		t.Fatalf("unknown leave type accepted: %d", s)
	}
	// Only chosen: Sick leave.
	if s, r := put(map[string]any{"leaveTypes": []string{"Sick leave"}}); s != 200 {
		t.Fatalf("only sick: %d %v", s, r)
	}
	if got := types(); containsS(got, "Casual leave") || !containsS(got, "Sick leave") {
		t.Fatalf("only sick: %v", got)
	}
	day := time.Now()
	for day = day.AddDate(0, 0, 1); day.Weekday() != time.Monday; day = day.AddDate(0, 0, 1) {
	}
	d := day.AddDate(0, 0, 1).Format("2006-01-02") // a Tuesday
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]string{"ltype": "Casual leave", "from": d, "to": d}})
	if s != 400 || !strings.Contains(fmt.Sprint(r), "not allowed for Permanent employees") {
		t.Fatalf("casual leave refused: %d %v", s, r)
	}
	// None.
	if s, r := put(map[string]any{"leave": false}); s != 200 {
		t.Fatalf("none: %d %v", s, r)
	}
	if got := types(); len(got) != 0 {
		t.Fatalf("no leave: %v", got)
	}
	// All again.
	if s, r := put(nil); s != 200 {
		t.Fatalf("all: %d %v", s, r)
	}
	if got := types(); len(got) != len(before) {
		t.Fatalf("all: %v, before %v", got, before)
	}
}

func containsS(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
