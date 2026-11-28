package api_test

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Combined mapping Department + place through the API (owner, 24 Nov 2026): saved, it decides before the single
// department; the conflict check sees it; switched off it is ignored and kept; a department rename follows it.
func TestPolicyComboMapping(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	const hr, hrRole = "EMP-1002", "HR Admin"
	hub := apiDoc(st, "location", bson.M{"name": "Gurugram – Sector 29 Hub"})
	hubID, clusterID := hub["id"].(string), hub["cluster_id"].(string)
	if apiDoc(st, "employee", bson.M{"_id": "EMP-1009", "location_id": hubID}) == nil {
		t.Fatal("EMP-1009 is not at the hub")
	}
	ops := apiDoc(st, "work_policy", bson.M{"name": "Operations – 24x7 rotational"})["id"].(string)
	fo := "Front Office|" + hubID
	deptMap := map[string]any{"level": "department", "ref": "Front Office", "on": true}
	combo := map[string]any{"level": "dept_location", "ref": fo, "on": true}

	// A department or place that does not exist is refused.
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps", hr, hrRole, map[string]any{"maps": []any{deptMap,
		map[string]any{"level": "dept_location", "ref": "No such dept|" + hubID, "on": true}}}); s != 400 {
		t.Fatalf("unknown department in a combo: %d %v", s, r)
	}
	// On the policy that already has the department: no conflict, and the combo now decides for those people.
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps", hr, hrRole, map[string]any{"maps": []any{deptMap, combo}}); s != 200 {
		t.Fatalf("combo save: %d %v", s, r)
	}
	_, ep := c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil)
	if ep["policyId"] != ops || ep["level"] != "dept_location" || ep["via"] != "Department + Location" ||
		!strings.Contains(ep["through"].(string), "the Front Office department at location Gurugram – Sector 29 Hub") {
		t.Fatalf("effective policy through the combo: %v", ep)
	}
	atHub := apiCount(st, "employee", bson.M{"status": bson.M{"$ne": "Inactive"}, "location_id": hubID,
		"department_id": apiDoc(st, "department", bson.M{"name": "Front Office"})["id"]})
	_, pg := c.do("GET", "/api/policies/"+ops, hr, hrRole, nil)
	for _, m := range pg["policy"].(map[string]any)["maps"].([]any) {
		if mm := m.(map[string]any); mm["level"] == "dept_location" && int(mm["employees"].(float64)) != atHub {
			t.Fatalf("combo employees %v, want %d", mm["employees"], atHub)
		}
	}

	// The conflict check sees the combo: a new policy on the hub's cluster for Front Office meets it.
	s, r := c.do("POST", "/api/policies", hr, hrRole, map[string]any{"name": "Front office cluster"})
	if s != 200 {
		t.Fatal(r)
	}
	q := r["id"].(string)
	cl := map[string]any{"maps": []any{map[string]any{"level": "dept_cluster", "ref": "Front Office|" + clusterID, "on": true}}}
	_, pv := c.do("POST", "/api/policies/"+q+"/maps/preview", hr, hrRole, cl)
	var hit map[string]any
	for _, g := range pv["conflicts"].([]any) {
		if m := g.(map[string]any); m["level"] == "dept_location" && m["ref"] == fo {
			hit = m
		}
	}
	if hit == nil || int(hit["count"].(float64)) != atHub || !strings.Contains(hit["text"].(string), "through the Front Office department at location") {
		t.Fatalf("conflict through the combo: %v", pv)
	}
	if s, r := c.do("PUT", "/api/policies/"+q+"/maps", hr, hrRole, cl); s != 409 {
		t.Fatalf("combo conflict saved without a choice: %d %v", s, r)
	}

	// Switched off: ignored (the department decides again) and kept.
	combo["on"] = false
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps", hr, hrRole, map[string]any{"maps": []any{deptMap, combo}}); s != 200 {
		t.Fatalf("combo off: %d %v", s, r)
	}
	if _, ep = c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil); ep["level"] != "department" {
		t.Fatalf("after the combo is off: %v", ep)
	}
	if apiCount(st, "work_policy_map", bson.M{"level": "dept_location", "ref": fo, "on_map": false}) != 1 {
		t.Fatal("a switched-off combo was not kept")
	}

	// A department rename carries over into the combo's ref.
	dep := apiDoc(st, "department", bson.M{"name": "Front Office"})["id"].(string)
	if s, r := c.do("PATCH", "/api/masters/department/"+dep, hr, hrRole, map[string]any{"name": "Front Desk"}); s != 200 {
		t.Fatalf("rename: %d %v", s, r)
	}
	if apiCount(st, "work_policy_map", bson.M{"level": "dept_location", "ref": "Front Desk|" + hubID}) != 1 {
		t.Fatal("the combo kept the old department name")
	}
	_ = context.Background()
}

// Holiday calendars: a new calendar starts empty; a holiday is edited within the calendar's year, never onto another
// holiday's date, never in a paid month; a calendar in use cannot be made inactive and an inactive one cannot be picked.
func TestHolidayCalendarEdit(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002")
	const hr, hrRole = "EMP-1002", "HR Admin"
	put := func(id string, body map[string]any) (int, map[string]any) {
		return c.do("PUT", "/api/calendars/"+id, hr, hrRole, body)
	}
	ncr := apiDoc(st, "holiday_calendar", bson.M{"name": "NCR 2026"})["id"].(string)
	if s, r := c.do("POST", "/api/calendars", hr, hrRole, map[string]any{"name": "QA 2026"}); s != 200 {
		t.Fatalf("new calendar: %d %v", s, r)
	}
	qa := apiDoc(st, "holiday_calendar", bson.M{"name": "QA 2026"})
	qaID := qa["id"].(string)
	if n := apiCount(st, "holiday", bson.M{"calendar_id": qaID}); n != 0 || qa["year"] == nil {
		t.Fatalf("new calendar: %d holidays, year %v", n, qa["year"])
	}
	if s, _ := c.do("POST", "/api/calendars", hr, hrRole, map[string]any{"name": "QA copy 2026", "copyFrom": ncr}); s != 200 ||
		apiCount(st, "holiday", bson.M{"calendar_id": apiDoc(st, "holiday_calendar", bson.M{"name": "QA copy 2026"})["id"]}) == 0 {
		t.Fatal("copy from another calendar")
	}
	add := func(date, name, kind string) int64 {
		t.Helper()
		if s, r := put(qaID, map[string]any{"holiday": map[string]any{"date": date, "name": name, "kind": kind}}); s != 200 {
			t.Fatalf("add %s: %d %v", date, s, r)
		}
		h := apiDoc(st, "holiday", bson.M{"calendar_id": qaID, "date": date})
		switch v := h["id"].(type) {
		case int64:
			return v
		case int32:
			return int64(v)
		}
		t.Fatalf("holiday id %T", h["id"])
		return 0
	}
	a := add("2026-03-10", "Holi", "Regional")
	add("2026-04-10", "Good Friday", "National")
	edit := func(id int64, date, name, kind string) (int, map[string]any) {
		return put(qaID, map[string]any{"holiday": map[string]any{"id": id, "date": date, "name": name, "kind": kind}})
	}
	if s, r := edit(a, "2027-03-10", "Holi", "Regional"); s != 400 || !strings.Contains(r["error"].(string), "not in 2026") {
		t.Fatalf("a date outside the year: %d %v", s, r)
	}
	if s, r := put(qaID, map[string]any{"holiday": map[string]any{"date": "2025-12-25", "name": "Christmas", "kind": "National"}}); s != 400 {
		t.Fatalf("a new holiday outside the year: %d %v", s, r)
	}
	if s, r := edit(a, "2026-04-10", "Holi", "Regional"); s != 400 || !strings.Contains(r["error"].(string), "already a holiday") {
		t.Fatalf("onto another holiday's date: %d %v", s, r)
	}
	if s, r := edit(a, "2026-03-11", "Holi (Dhulandi)", "Optional"); s != 200 {
		t.Fatalf("edit: %d %v", s, r)
	}
	if h := apiDoc(st, "holiday", bson.M{"_id": a}); h["date"] != "2026-03-11" || h["name"] != "Holi (Dhulandi)" || h["kind"] != "Optional" {
		t.Fatalf("edited holiday %v", h)
	}
	if apiCount(st, "audit_log", bson.M{"entity": "holiday_calendar", "entity_id": qaID}) < 3 {
		t.Fatal("holiday changes not audited")
	}

	// A paid month: neither the new date nor the old one may be in it; nor may a holiday in it be switched.
	if _, err := st.DB.C("payroll_month").Insert(ctx, bson.M{"ym": "2026-05", "status": "Published"}); err != nil {
		t.Fatal(err)
	}
	if s, r := edit(a, "2026-05-04", "Holi", "Regional"); s != 400 || !strings.Contains(r["error"].(string), "already paid") {
		t.Fatalf("moved into a paid month: %d %v", s, r)
	}
	if _, err := st.DB.C("holiday").Insert(ctx, bson.M{"calendar_id": qaID, "date": "2026-05-01", "name": "Labour Day", "kind": "Regional"}); err != nil {
		t.Fatal(err)
	}
	may := apiDoc(st, "holiday", bson.M{"calendar_id": qaID, "date": "2026-05-01"})["id"]
	if s, r := put(qaID, map[string]any{"holiday": map[string]any{"id": may, "date": "2026-06-01", "name": "Labour Day", "kind": "Regional"}}); s != 400 ||
		!strings.Contains(r["error"].(string), "already paid") {
		t.Fatalf("moved out of a paid month: %d %v", s, r)
	}
	if s, r := put(qaID, map[string]any{"holiday": map[string]any{"id": may, "on": false}}); s != 400 {
		t.Fatalf("switched off in a paid month: %d %v", s, r)
	}

	// Inactive: refused while a policy uses it (409 naming them); an inactive calendar cannot be picked.
	if s, r := put(ncr, map[string]any{"active": false}); s != 409 || !strings.Contains(r["error"].(string), "Operations – 6 day") {
		t.Fatalf("deactivating a calendar in use: %d %v", s, r)
	}
	if s, r := put(qaID, map[string]any{"active": false}); s != 200 {
		t.Fatalf("deactivating an unused calendar: %d %v", s, r)
	}
	pid := apiDoc(st, "work_policy", bson.M{"name": "Operations – 6 day"})["id"].(string)
	_, pg := c.do("GET", "/api/policies/"+pid, hr, hrRole, nil)
	p := pg["policy"].(map[string]any)
	body := map[string]any{"name": p["name"], "settings": p["settings"], "leavePlanId": p["leavePlanId"], "calendarId": qaID, "calMap": p["calMap"]}
	if s, r := c.do("PUT", "/api/policies/"+pid, hr, hrRole, body); s != 400 || !strings.Contains(r["error"].(string), "inactive") {
		t.Fatalf("an inactive calendar picked: %d %v", s, r)
	}
	cm := map[string]any{}
	for k, v := range p["calMap"].(map[string]any) {
		cm[k] = v
	}
	city := apiDoc(st, "city", bson.M{"name": "Gurugram"})["id"].(string)
	cm[city] = qaID
	body["calendarId"], body["calMap"] = p["calendarId"], cm
	if s, _ := c.do("PUT", "/api/policies/"+pid, hr, hrRole, body); s != 400 {
		t.Fatalf("an inactive calendar picked for a city: %d", s)
	}
	if s, _ := put(qaID, map[string]any{"active": true, "name": "QA holidays"}); s != 200 {
		t.Fatal("reactivate and rename")
	}
	if x := apiDoc(st, "holiday_calendar", bson.M{"_id": qaID}); x["status"] != "Active" || x["name"] != "QA holidays" || x["year"] == nil {
		t.Fatalf("after rename %v", x)
	}
	if s, _ := put(qaID, map[string]any{"name": "NCR 2026"}); s != 400 {
		t.Fatal("a second calendar with the same name")
	}
	if s, r := c.do("PUT", "/api/policies/"+pid, hr, hrRole, body); s != 200 {
		t.Fatalf("an active calendar picked for a city: %d %v", s, r)
	}
	if s, _ := put(qaID, map[string]any{"active": false}); s != 409 {
		t.Fatal("deactivated while a policy uses it for a city")
	}
}
