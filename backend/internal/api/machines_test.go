package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Where each record is stored: own property's machine first, then the next machine of the cluster with space.
func TestMachinePlan(t *testing.T) {
	ms := [][3]any{{1, "A", 2}, {2, "B", 2}, {3, "C", 1}}
	ps := [][3]string{{"a1", "A", "2024-01-01"}, {"a2", "A", "2024-02-01"}, {"a3", "A", "2024-03-01"}, {"b1", "B", "2023-01-01"},
		{"x1", "X", "2022-01-01"}, {"a4", "A", "2025-01-01"}, {"c1", "C", "2025-06-01"}}
	got, left := api.MachinePlan(ms, ps)
	// A's machine takes a1, a2 (earliest first); B's takes b1; C's takes c1. Then the rest, earliest first: x1 (no
	// machine of its own: from the first machine) → B's last place; a3 and a4 find no space anywhere.
	want := map[string]int{"a1": 1, "a2": 1, "b1": 2, "c1": 3, "x1": 2}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %d, want %d (%v)", k, got[k], v, got)
		}
	}
	if _, ok := got["a3"]; ok || len(left) != 2 || left[0] != "a3" || left[1] != "a4" {
		t.Fatalf("left: %v %v", left, got)
	}
	// The same input gives the same plan.
	again, _ := api.MachinePlan(ms, ps)
	if fmt.Sprint(again) != fmt.Sprint(got) {
		t.Fatal("not deterministic")
	}
}

func TestMachinePool(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1020")
	ctx := context.Background()
	const hr, role = "EMP-1002", "HR Admin"
	locID := func(name string) string { return fmt.Sprint(apiDoc(st, "location", bson.M{"name": name})["id"]) }
	hub, golf, saket := locID("Gurugram – Sector 29 Hub"), locID("Gurugram – Golf Course Rd"), locID("Delhi – Saket")
	active := func(locs ...string) int {
		return apiCount(st, "employee", bson.M{"status": "Active", "location_id": bson.M{"$in": locs}})
	}
	gurugram := active(hub, golf)
	if gurugram < 2 || active(saket) < 1 {
		t.Fatalf("sample org: %d in Gurugram, %d in Saket", gurugram, active(saket))
	}

	portal := []realtime.Machine{
		{No: "1", Model: "T304F Mini", Serial: "RSSA1", Name: "Sector 29", Mode: "In/Out", Branch: "SaltStayz Sector 29 Hub"},
		{No: "2", Model: "T304F Mini", Serial: "RSSA2", Name: "Golf Course", Mode: "In/Out", Branch: "Saltstayz Golf Course Rd"},
		{No: "3", Model: "RS 9W", Serial: "RSSA3", Name: "Saket", Mode: "In", Branch: "SaltStayz Saket"},
		{No: "4", Model: "RS 9W", Serial: "RSSA4", Name: "Spare1", Mode: "In/Out", Branch: "Corporate Office"},
	}
	cfg := realtime.Config{BaseURL: "http://203.0.113.10:83", User: "u", Pass: "p"}
	undo := api.SetMachinePortal(cfg, true, func(context.Context, realtime.Config) ([]realtime.Machine, error) { return portal, nil })
	defer undo()

	// Rights: an employee neither sees nor imports.
	if s, _ := c.do("GET", "/api/attendance/machines", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee GET: %d", s)
	}
	if s, _ := c.do("POST", "/api/attendance/machines/import", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee import: %d", s)
	}

	// Import: four machines, vendor Realtime, none mapped yet; again: updated, not added.
	if s, r := c.do("POST", "/api/attendance/machines/import", hr, role, nil); s != 200 || r["added"] != 4.0 || r["updated"] != 0.0 {
		t.Fatalf("import: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/attendance/machines/import", hr, role, nil); s != 200 || r["added"] != 0.0 || r["updated"] != 4.0 {
		t.Fatalf("import again: %d %v", s, r)
	}
	if d := apiDoc(st, "attendance_device", bson.M{"serial": "RSSA3"}); d == nil || d["vendor"] != "Realtime" || d["model"] != "RS 9W" || d["portal_branch"] != "SaltStayz Saket" || d["location_id"] != nil {
		t.Fatalf("device: %v", d)
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": "Punch machines imported from the portal: 4 new, 0 updated"}); a == nil {
		t.Fatal("import not audited")
	}
	// A machine the portal no longer lists is marked, not removed.
	portal = portal[:3]
	c.do("POST", "/api/attendance/machines/import", hr, role, nil)
	if d := apiDoc(st, "attendance_device", bson.M{"serial": "RSSA4"}); d == nil || d["portal_gone"] != true {
		t.Fatalf("gone: %v", d)
	}

	// Board: suggestions from the branch names; "use suggestions" maps the three that match.
	_, b := c.do("GET", "/api/attendance/machines", hr, role, nil)
	if b["unmapped"] != 4.0 || b["defaultCapacity"] != 1000.0 {
		t.Fatalf("board: %v", b)
	}
	hints := map[string]string{}
	for _, x := range b["machines"].([]any) {
		m := x.(map[string]any)
		if h, ok := m["hint"].(map[string]any); ok {
			hints[fmt.Sprint(m["serial"])] = fmt.Sprint(h["location"])
		}
	}
	if hints["RSSA1"] != "Gurugram – Sector 29 Hub" || hints["RSSA2"] != "Gurugram – Golf Course Rd" || hints["RSSA3"] != "Delhi – Saket" || hints["RSSA4"] != "" {
		t.Fatalf("hints: %v", hints)
	}
	if s, r := c.do("POST", "/api/attendance/machines/use-hints", hr, role, nil); s != 200 || r["mapped"] != 3.0 {
		t.Fatalf("use hints: %d %v", s, r)
	}

	// Capacity: the Gurugram cluster's two machines hold exactly its people now (one machine 1, the other the rest).
	dev := func(serial string) string {
		return fmt.Sprint(apiDoc(st, "attendance_device", bson.M{"serial": serial})["id"])
	}
	if s, r := c.do("POST", "/api/attendance/machines/"+dev("RSSA1")+"/map", hr, role, map[string]any{"capacity": 1}); s != 200 {
		t.Fatalf("capacity: %v", r)
	}
	c.do("POST", "/api/attendance/machines/"+dev("RSSA2")+"/map", hr, role, map[string]any{"capacity": gurugram - 1})
	if s, _ := c.do("POST", "/api/attendance/machines/"+dev("RSSA2")+"/map", hr, role, map[string]any{"capacity": -1}); s != 400 {
		t.Fatal("negative capacity")
	}
	_, b = c.do("GET", "/api/attendance/machines", hr, role, nil)
	var gc map[string]any
	for _, x := range b["clusters"].([]any) {
		if m := x.(map[string]any); m["name"] == "Gurugram Cluster 1" {
			gc = m
		}
	}
	if gc == nil || gc["capacity"] != float64(gurugram) || gc["people"] != float64(gurugram) || gc["level"] != "full" || gc["pct"] != 100.0 || gc["machines"] != 2.0 || gc["unknown"] != 2.0 {
		t.Fatalf("cluster: %v", gc)
	}
	// Where the records go: one on the Sector 29 machine, the rest of the hub overflow to Golf Course.
	_, pl := c.do("GET", "/api/attendance/machines/plan?clusterId="+fmt.Sprint(gc["id"]), hr, role, nil)
	onHub, overflow := 0, 0
	for _, x := range pl["people"].([]any) {
		m := x.(map[string]any)
		if strings.Contains(fmt.Sprint(m["machine"]), "RSSA1") {
			onHub++
		}
		if m["overflow"] == true {
			overflow++
		}
	}
	if onHub != 1 || overflow != active(hub)-1 || pl["unplaced"] != 0.0 {
		t.Fatalf("plan: %d on the hub machine, %d overflow: %v", onHub, overflow, pl)
	}

	// Before the baseline everyone active is a joiner to enrol; after it, nothing to do.
	_, td := c.do("GET", "/api/attendance/machines/todo", hr, role, nil)
	if n := len(td["open"].([]any)); n != apiCount(st, "employee", bson.M{"status": "Active"}) {
		t.Fatalf("before baseline: %d", n)
	}
	if s, r := c.do("POST", "/api/attendance/machines/baseline", hr, role, nil); s != 200 || r["recorded"] == 0.0 {
		t.Fatalf("baseline: %v", r)
	}
	if _, td = c.do("GET", "/api/attendance/machines/todo", hr, role, nil); len(td["open"].([]any)) != 0 {
		t.Fatalf("after baseline: %v", td["open"])
	}

	// Transfer into the full cluster: refused, with the details the web app asks about; allowed with the override.
	var mover string
	for _, e := range apiDocs(st, "employee", bson.M{"status": "Active", "location_id": saket}) {
		if e["id"] != "EMP-1002" && e["id"] != "EMP-1004" {
			mover = fmt.Sprint(e["id"])
			break
		}
	}
	hireFixRoleSkill(t, st, mover) // a job change needs Role and Skill on the record (hirefix.go)
	job := map[string]any{"changes": map[string]any{"location": "Gurugram – Golf Course Rd"}, "reason": "Data correction", "effectiveFrom": today()}
	s, r := c.do("PATCH", "/api/employees/"+mover+"/job", hr, role, job)
	if d, _ := r["details"].(map[string]any); s != 409 || d["kind"] != "cluster_full" || !strings.Contains(fmt.Sprint(r["error"]), "Gurugram Cluster 1 is full") {
		t.Fatalf("full: %d %v", s, r)
	}
	if s, r := c.do("PATCH", "/api/employees/"+mover+"/job?machineOverride=1", hr, role, job); s != 200 {
		t.Fatalf("override: %d %v", s, r)
	}
	if a := apiDoc(st, "audit_log", bson.M{"entity_id": mover, "action": bson.M{"$regex": "^Put into Gurugram Cluster 1 although"}}); a == nil {
		t.Fatal("override not audited")
	}
	// A move inside a cluster is never checked and creates no task.
	var inside string
	for _, e := range apiDocs(st, "employee", bson.M{"status": "Active", "location_id": hub}) {
		if e["id"] != "EMP-1002" && e["id"] != "EMP-1004" {
			inside = fmt.Sprint(e["id"])
			break
		}
	}
	hireFixRoleSkill(t, st, inside)
	if s, r := c.do("PATCH", "/api/employees/"+inside+"/job", hr, role, map[string]any{"changes": map[string]any{"location": "Gurugram – Golf Course Rd"}, "reason": "Data correction", "effectiveFrom": today()}); s != 200 {
		t.Fatalf("inside the cluster: %d %v", s, r)
	}
	// An exit: removal.
	var leaver string
	for _, e := range apiDocs(st, "employee", bson.M{"status": "Active", "location_id": golf}) {
		if id := fmt.Sprint(e["id"]); id != "EMP-1002" && id != "EMP-1004" && id != mover && id != inside {
			leaver = id
			break
		}
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": leaver}, bson.M{"$set": bson.M{"status": "Inactive", "attendance_code": "4001"}}); err != nil {
		t.Fatal(err)
	}
	// A joiner on the Joining ID into the full cluster: refused, then allowed with the override.
	si, _ := hireAccepted(t, c, st, "Riya", "Sharma", "9876501234")
	s, r = c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if d, _ := r["details"].(map[string]any); s != 409 || d["kind"] != "cluster_full" {
		t.Fatalf("joiner into a full cluster: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined?machineOverride=1", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || r["id"] != "JI-0001" {
		t.Fatalf("joined anyway: %d %v", s, r)
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "JI-0001"}, bson.M{"$set": bson.M{"attendance_code": "05001"}}); err != nil {
		t.Fatal(err)
	}

	_, td = c.do("GET", "/api/attendance/machines/todo", hr, role, nil)
	tasks := map[string]map[string]any{}
	for _, x := range td["open"].([]any) {
		m := x.(map[string]any)
		tasks[fmt.Sprint(m["employeeId"])] = m
	}
	if len(tasks) != 3 || tasks[inside] != nil {
		t.Fatalf("tasks: %v", td["open"])
	}
	if m := tasks[mover]; m["kind"] != "Move" || m["to"] != "Gurugram Cluster 1" || m["from"] != "Delhi Cluster 1" ||
		!strings.HasPrefix(fmt.Sprint(m["task"]), "Enrol in cluster Gurugram Cluster 1") || !strings.HasSuffix(fmt.Sprint(m["task"]), "remove from cluster Delhi Cluster 1") {
		t.Fatalf("move: %v", m)
	}
	if m := tasks[leaver]; m["kind"] != "Remove" || m["task"] != "Remove from cluster Gurugram Cluster 1" {
		t.Fatalf("exit: %v", m)
	}
	if m := tasks["JI-0001"]; m["kind"] != "Enrol" || !strings.HasPrefix(fmt.Sprint(m["task"]), "Enrol in cluster Gurugram Cluster 1") {
		t.Fatalf("joiner: %v", m)
	}
	// Excel export of the list.
	if s, _ := c.do("GET", "/api/attendance/machines/todo.xlsx", hr, role, nil); s != 200 {
		t.Fatalf("xlsx: %d", s)
	}

	// The portal's employee list: the joiner's code is in a Sector 29 Hub branch (enrolled) and the leaver's is gone
	// (removed); the mover has no code and is left for HR.
	undoE := api.SetRealtimeEmployees(cfg, true, func(context.Context, realtime.Config) ([]realtime.Employee, error) {
		return []realtime.Employee{{Name: "Riya Sharma", Code: "5001", Card: "00005001", Branch: "SaltStayz Sector 29 Hub"}, {Name: "Someone", Code: "9999", Branch: "Elsewhere"}}, nil
	})
	defer undoE()
	srvApp := api.New(st, api.Options{DevOTP: true})
	if _, err := srvApp.RealtimeMatchRun(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	_, td = c.do("GET", "/api/attendance/machines/todo", hr, role, nil)
	open := td["open"].([]any)
	if len(open) != 1 || open[0].(map[string]any)["employeeId"] != mover || !strings.Contains(fmt.Sprint(open[0].(map[string]any)["note"]), "No Attendance code") {
		t.Fatalf("after the portal check: %v", open)
	}
	if e := apiDoc(st, store.MachineEnrolColl, bson.M{"employee_id": "JI-0001"}); e == nil || e["cluster_id"] != gc["id"] || e["how"] != "portal" {
		t.Fatalf("joiner enrolled: %v", e)
	}
	if e := apiDoc(st, store.MachineEnrolColl, bson.M{"employee_id": leaver}); e == nil || e["cluster_id"] != nil {
		t.Fatalf("leaver removed: %v", e)
	}
	// Ticked by hand.
	if s, _ := c.do("POST", "/api/attendance/machines/todo/done", "EMP-1020", "Employee", map[string]any{"employeeIds": []string{mover}}); s != 403 {
		t.Fatal("employee ticks")
	}
	if s, r := c.do("POST", "/api/attendance/machines/todo/done", hr, role, map[string]any{"employeeIds": []string{mover}}); s != 200 || r["done"] != 1.0 {
		t.Fatalf("tick: %d %v", s, r)
	}
	_, td = c.do("GET", "/api/attendance/machines/todo", hr, role, nil)
	if len(td["open"].([]any)) != 0 || len(td["done"].([]any)) < 3 {
		t.Fatalf("done: %v", td)
	}
	if s, _ := c.do("POST", "/api/attendance/machines/todo/done", hr, role, map[string]any{"employeeIds": []string{mover}}); s != 400 {
		t.Fatal("ticked twice")
	}

	// The check the onboarding pages can call.
	if s, r := c.do("GET", "/api/attendance/machines/check?locationId="+saket, hr, role, nil); s != 200 || r["level"] != "ok" || r["cluster"] != "Delhi Cluster 1" {
		t.Fatalf("check: %d %v", s, r)
	}
	if s, r := c.do("GET", "/api/attendance/machines/check?locationId="+hub, hr, role, nil); s != 200 || r["level"] != "full" {
		t.Fatalf("check full: %d %v", s, r)
	}
	// Default capacity.
	if s, _ := c.do("POST", "/api/attendance/machines/settings", hr, role, map[string]any{"defaultCapacity": 0}); s != 400 {
		t.Fatal("zero default")
	}
	if s, _ := c.do("POST", "/api/attendance/machines/settings", hr, role, map[string]any{"defaultCapacity": 800}); s != 200 {
		t.Fatal("default")
	}
	if _, b = c.do("GET", "/api/attendance/machines", hr, role, nil); b["defaultCapacity"] != 800.0 {
		t.Fatalf("default: %v", b["defaultCapacity"])
	}
}

// apiDocs is every document of coll matching filter.
func apiDocs(st *store.Store, coll string, filter any) []store.Row {
	r, _ := st.DB.C(coll).Find(context.Background(), filter)
	return r
}
