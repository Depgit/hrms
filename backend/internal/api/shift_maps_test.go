package api_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// shiftRowsOf are a shift's Department + Location rows from Set shift, by label.
func shiftRowsOf(t *testing.T, c *client, id string) map[string]map[string]any {
	t.Helper()
	s, v := c.do("GET", "/api/shifts/"+id, "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("get shift: %d %v", s, v)
	}
	out := map[string]map[string]any{}
	for _, x := range v["maps"].([]any) {
		m := x.(map[string]any)
		out[m["label"].(string)] = m
	}
	return out
}

// rosterCodesOf are the codes the roster offers the employee for the week of d.
func rosterCodesOf(t *testing.T, c *client, emp, loc, d string) (codes []string, cells []any) {
	t.Helper()
	st, ro := c.do("GET", "/api/roster?week="+d+"&location="+loc, "EMP-1002", "HR Admin", nil)
	l, ok := ro["rows"].([]any)
	if st != 200 || !ok {
		t.Fatalf("roster: %d %v", st, ro)
	}
	for _, x := range l {
		if r := x.(map[string]any); r["id"] == emp {
			for _, k := range r["codes"].([]any) {
				codes = append(codes, k.(string))
			}
			return codes, r["cells"].([]any)
		}
	}
	t.Fatalf("%s not on the roster", emp)
	return nil, nil
}

// Where a shift applies (owner, 24 Nov 2026): Department + Location rows, either side All. The roster offers a shift
// only when a switched-on row matches the employee; a new assignment outside the rows is refused, the planned days stay.
func TestShiftMapping(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	mID := shiftIDOf(t, c, "M") // EMP-1020 works M
	e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	dept, loc := e["department_id"].(string), e["location_id"].(string)
	locName := apiDoc(st, "location", bson.M{"_id": loc})["name"].(string)
	other := apiDoc(st, "department", bson.M{"_id": bson.M{"$ne": dept}, "status": "Active"})
	otherID, otherName := other["id"].(string), other["name"].(string)
	// A working day ahead, inside the roster's reach.
	day := time.Now().AddDate(0, 0, 9)
	for day.Weekday() == time.Sunday || day.Weekday() == time.Saturday {
		day = day.AddDate(0, 0, 1)
	}
	d := day.Format("2006-01-02")
	has := func(l []string, v string) bool {
		for _, x := range l {
			if x == v {
				return true
			}
		}
		return false
	}

	rows := shiftRowsOf(t, c, mID)
	all := rows["All departments at all locations"]
	if len(rows) != 1 || all == nil {
		t.Fatalf("M from migration 029's rule: %v", rows)
	}
	if codes, _ := rosterCodesOf(t, c, "EMP-1020", loc, d); !has(codes, "M") {
		t.Fatalf("All at all: M offered, %v", codes)
	}
	if s, r := c.do("POST", "/api/shifts/"+mID+"/maps", "EMP-1020", "Employee", map[string]any{"departmentId": otherID}); s != 403 {
		t.Fatalf("an employee changes the rows: %d %v", s, r)
	}
	// Another department at EMP-1020's location; the same pair again is refused.
	if s, r := c.do("POST", "/api/shifts/"+mID+"/maps", hr, hrRole, map[string]any{"departmentId": otherID, "locationId": loc}); s != 200 {
		t.Fatalf("add: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/shifts/"+mID+"/maps", hr, hrRole, map[string]any{"departmentId": otherID, "locationId": loc}); s != 409 || !strings.Contains(fmt.Sprint(r["error"]), "already on this shift") {
		t.Fatalf("the same pair twice: %d %v", s, r)
	}
	// All at all switched off: EMP-1020's department has no row, so M is no longer offered nor accepted; the planned day stays M.
	s, r := c.do("PUT", "/api/shifts/"+mID+"/maps/"+all["id"].(string), hr, hrRole, map[string]any{"on": false})
	if s != 200 || num(r["outsideNow"]) < 1 {
		t.Fatalf("switch off: %d %v", s, r)
	}
	codes, cells := rosterCodesOf(t, c, "EMP-1020", loc, d)
	if has(codes, "M") {
		t.Fatalf("no match: M still offered, %v", codes)
	}
	planned := 0
	for _, x := range cells {
		if x.(map[string]any)["code"] == "M" {
			planned++
		}
	}
	if planned == 0 {
		t.Fatalf("the planned days of EMP-1020 changed: %v", cells)
	}
	if s, r := c.do("POST", "/api/roster/cell", hr, hrRole, map[string]any{"employeeId": "EMP-1020", "date": d, "code": "WO", "why": "test"}); s != 200 {
		t.Fatalf("to WO: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/roster/cell", hr, hrRole, map[string]any{"employeeId": "EMP-1020", "date": d, "code": "M", "why": "test"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "does not apply to") {
		t.Fatalf("a new assignment outside the rows: %d %v", s, r)
	}
	// All departments at the location: EMP-1020 matches through All.
	if s, r := c.do("POST", "/api/shifts/"+mID+"/maps", hr, hrRole, map[string]any{"departmentId": "", "locationId": loc}); s != 200 {
		t.Fatalf("add All departments at %s: %d %v", locName, s, r)
	}
	if codes, _ := rosterCodesOf(t, c, "EMP-1020", loc, d); !has(codes, "M") {
		t.Fatalf("All departments at the location: M offered, %v", codes)
	}
	if s, r := c.do("POST", "/api/roster/cell", hr, hrRole, map[string]any{"employeeId": "EMP-1020", "date": d, "code": "M", "why": "test"}); s != 200 {
		t.Fatalf("back to M: %d %v", s, r)
	}
	// The last switched-on row cannot go.
	rows = shiftRowsOf(t, c, mID)
	pair, atLoc := rows[otherName+" at "+locName], rows["All departments at "+locName]
	if pair == nil || atLoc == nil {
		t.Fatalf("rows %v", rows)
	}
	if s, r := c.do("PUT", "/api/shifts/"+mID+"/maps/"+pair["id"].(string), hr, hrRole, map[string]any{"on": false}); s != 200 {
		t.Fatalf("switch off the pair: %d %v", s, r)
	}
	if s, r := c.do("PUT", "/api/shifts/"+mID+"/maps/"+atLoc["id"].(string), hr, hrRole, map[string]any{"on": false}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "last row") {
		t.Fatalf("last row off: %d %v", s, r)
	}
	if s, r := c.do("DELETE", "/api/shifts/"+mID+"/maps/"+atLoc["id"].(string), hr, hrRole, nil); s != 400 {
		t.Fatalf("last row removed: %d %v", s, r)
	}
	if s, r := c.do("DELETE", "/api/shifts/"+mID+"/maps/"+pair["id"].(string), hr, hrRole, nil); s != 200 {
		t.Fatalf("remove a switched-off row: %d %v", s, r)
	}
	if s, r := c.do("PUT", "/api/shifts/"+mID+"/maps/"+all["id"].(string), hr, hrRole, map[string]any{"on": true}); s != 200 {
		t.Fatalf("switch All at all back on: %d %v", s, r)
	}
	_, v := c.do("GET", "/api/shifts/"+mID, hr, hrRole, nil)
	if h := v["mapHistory"].([]any); len(h) != 6 {
		t.Fatalf("history: %d %v", len(h), h)
	}
	if n := apiCount(st, "audit_log", bson.M{"entity": "shift", "entity_id": mID, "action": bson.M{"$regex": "^Shift mapping"}}); n != 6 {
		t.Fatalf("audit: %d", n)
	}
}

// Bulk update › Shift mapping: rows added, switched on and off, and the refusals.
func TestBulkShiftMapping(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	hub := "Gurugram – Sector 29 Hub"
	mID := apiDoc(st, "shift", bson.M{"code": "M"})["id"].(string)
	if s, _ := c.sendFile("/api/bulk/shiftmap/check", "EMP-1020", "Employee", "m.csv", []byte("Shift Code\n")); s != 403 {
		t.Fatalf("an employee may not upload shift mappings: %d", s)
	}
	run := func(action, csv string) map[int]map[string]any {
		t.Helper()
		s, r := c.sendFile("/api/bulk/shiftmap/"+action, hr, hrRole, "shiftmap.csv", []byte(csv))
		if s != 200 {
			t.Fatalf("%s: %d %v", action, s, r)
		}
		return bulkRows(t, r)
	}
	on := func() int { return apiCount(st, "shift_map", bson.M{"shift_id": mID, "on_map": true}) }
	head := "Shift Code,Department,Location,Active\n"
	rows := run("apply", head+"m,Front Office,"+hub+",\n")
	if rowErr(rows[2]) != "[]" || len(rows[2]["changes"].([]any)) != 1 || on() != 2 {
		t.Fatalf("pair added: %v (on %d)", rows[2], on())
	}
	rows = run("check", head+"M,Front Office,"+hub+",Yes\n") // already on: no change
	if len(rows[2]["changes"].([]any)) != 0 || rowErr(rows[2]) != "[]" {
		t.Fatalf("already there: %v", rows[2])
	}
	csv := head +
		"ZZ,All,All,Yes\n" + // 2: unknown shift
		"M,No such dept,,\n" + // 3: unknown department
		"M,F&B," + hub + ",No\n" + // 4: not on the shift
		"M,Front Office," + hub + ",Yes\n" + // 5: on already: no change
		"m,front office," + hub + ",yes\n" + // 6: the same row again (case aside)
		"M,All,All,Maybe\n" // 7: Active not Yes or No
	rows = run("check", csv)
	if !strings.Contains(rowErr(rows[2]), "Shift “ZZ” not found") || !strings.Contains(rowErr(rows[3]), "Department “No such dept” not found") ||
		!strings.Contains(rowErr(rows[4]), "is not on shift M") || !strings.Contains(rowErr(rows[7]), "Active must be Yes or No") {
		t.Fatalf("refusals: %v | %v | %v | %v", rows[2], rows[3], rows[4], rows[7])
	}
	if !strings.Contains(rowErr(rows[6]), "Appears again (first on line 5)") {
		t.Fatalf("same row twice: %v", rows[6])
	}
	// All at all off (blank = All), then the last row is refused; blank Active switches All at all back on.
	rows = run("apply", head+"M,,,No\n")
	if rowErr(rows[2]) != "[]" || changeTo(rows[2], "Shift M applies to All departments at all locations") != "Off" || on() != 1 {
		t.Fatalf("switch off: %v", rows[2])
	}
	rows = run("check", head+"M,Front Office,"+hub+",No\n")
	if !strings.Contains(rowErr(rows[2]), "last row") {
		t.Fatalf("last row: %v", rows[2])
	}
	rows = run("apply", head+"M,All,All,\n")
	if rowErr(rows[2]) != "[]" || on() != 2 || apiCount(st, "shift_map", bson.M{"shift_id": mID}) != 2 {
		t.Fatalf("switched back on: %v", rows[2])
	}
	if s, _ := c.raw("/api/bulk/shiftmap/download?data=1", hr, hrRole); s != 200 {
		t.Fatalf("current data: %d", s)
	}
}
