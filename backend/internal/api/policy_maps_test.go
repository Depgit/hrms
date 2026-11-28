package api_test

import (
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// One mapping, one policy (owner, 24 Nov 2026): mapping the same level and value again on another policy is refused
// on add, on switching on and on edit; Edit moves a mapping to another policy, with history on both.
func TestPolicyMapDuplicates(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	const hr, hrRole = "EMP-1002", "HR Admin"
	pol := func(name string) string { return apiDoc(st, "work_policy", bson.M{"name": name})["id"].(string) }
	ops, six, corp := pol("Operations – 24x7 rotational"), pol("Operations – 6 day"), pol("Corporate office – 5.5 day")
	hubID := apiDoc(st, "location", bson.M{"name": "Gurugram – Sector 29 Hub"})["id"].(string)
	maps := func(m ...map[string]any) map[string]any { return map[string]any{"maps": m} }
	fo := map[string]any{"level": "department", "ref": "Front Office", "on": true}
	const dupOps = "Already applied to Operations – 24x7 rotational. Switch it off there or edit that mapping."

	// Add: refused at the preview and at the save; nothing stored.
	if s, r := c.do("POST", "/api/policies/"+six+"/maps/preview", hr, hrRole, maps(fo)); s != 409 || r["error"] != dupOps {
		t.Fatalf("preview of a mapping another policy has: %d %v", s, r)
	}
	if s, r := c.do("PUT", "/api/policies/"+six+"/maps", hr, hrRole, map[string]any{"maps": []any{fo}, "resolution": map[string]any{"mode": "replace"}}); s != 409 || r["error"] != dupOps {
		t.Fatalf("save of a mapping another policy has: %d %v", s, r)
	}
	if apiCount(st, "work_policy_map", bson.M{"policy_id": six, "ref": "Front Office"}) != 0 ||
		apiCount(st, "work_policy_map", bson.M{"policy_id": ops, "ref": "Front Office", "on_map": true}) != 1 {
		t.Fatal("a refused mapping changed something")
	}
	// The same mapping twice on its own policy is one mapping.
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps", hr, hrRole, maps(fo, fo)); s != 200 || apiCount(st, "work_policy_map", bson.M{"policy_id": ops, "ref": "Front Office"}) != 1 {
		t.Fatalf("listed twice on its own policy: %d %v", s, r)
	}
	// Switching on a switched-off copy while another policy has it on.
	if _, err := st.DB.C("work_policy_map").Insert(t.Context(), bson.M{"policy_id": six, "level": "department", "ref": "Front Office", "on_map": false, "created_by": "test"}); err != nil {
		t.Fatal(err)
	}
	if s, r := c.do("PUT", "/api/policies/"+six+"/maps", hr, hrRole, maps(fo)); s != 409 || r["error"] != dupOps {
		t.Fatalf("switch on: %d %v", s, r)
	}
	// Department + Location pairs too; a different level covering the same people is not a duplicate.
	combo := map[string]any{"level": "dept_location", "ref": "Front Office|" + hubID, "on": true}
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps", hr, hrRole, maps(fo, combo)); s != 200 {
		t.Fatalf("combo on its department's policy: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/policies/"+corp+"/maps/preview", hr, hrRole, maps(combo)); s != 409 || r["error"] != dupOps {
		t.Fatalf("the same pair on another policy: %d %v", s, r)
	}
	sales := map[string]any{"level": "dept_location", "ref": "Sales|" + hubID, "on": true}
	if s, r := c.do("POST", "/api/policies/"+six+"/maps/preview", hr, hrRole, maps(sales)); s != 200 {
		t.Fatalf("a pair over a department mapped elsewhere is not a duplicate: %d %v", s, r)
	}

	// Edit: the value into one another policy has is refused.
	rowOf := func(pid, level, ref string) string {
		t.Helper()
		x := apiDoc(st, "work_policy_map", bson.M{"policy_id": pid, "level": level, "ref": ref, "on_map": true})
		if x == nil {
			t.Fatalf("no mapping %s %s on %s", level, ref, pid)
		}
		return fmt.Sprint(x["id"])
	}
	foRow := rowOf(ops, "department", "Front Office")
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps/"+foRow, hr, hrRole, map[string]any{"ref": "Sales"}); s != 409 ||
		r["error"] != "Already applied to Corporate office – 5.5 day. Switch it off there or edit that mapping." {
		t.Fatalf("edit into a mapping another policy has: %d %v", s, r)
	}
	// Edit › move the pair to another policy: previewed as a move, then saved; the old mapping is kept, off.
	cRow := rowOf(ops, "dept_location", "Front Office|"+hubID)
	s, pv := c.do("POST", "/api/policies/"+ops+"/maps/"+cRow+"/preview", hr, hrRole, map[string]any{"policyId": six})
	if s != 200 || pv["move"] != true || len(pv["moves"].([]any)) != 1 || len(pv["conflicts"].([]any)) != 0 {
		t.Fatalf("move preview: %d %v", s, pv)
	}
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps/"+cRow, hr, hrRole, map[string]any{"policyId": six}); s != 200 {
		t.Fatalf("move: %d %v", s, r)
	}
	if apiCount(st, "work_policy_map", bson.M{"policy_id": ops, "level": "dept_location", "on_map": false}) != 1 ||
		apiCount(st, "work_policy_map", bson.M{"policy_id": six, "level": "dept_location", "ref": "Front Office|" + hubID, "on_map": true}) != 1 {
		t.Fatal("after the move")
	}
	if _, ep := c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil); ep["policy"] != "Operations – 6 day" || ep["level"] != "dept_location" {
		t.Fatalf("EMP-1009 after the move: %v", ep)
	}
	for _, pid := range []string{ops, six} {
		if apiCount(st, "config_history", bson.M{"entity": "work_policy", "entity_id": pid, "changes.field": "mapping moved"}) != 1 {
			t.Fatalf("history of %s", pid)
		}
	}
	if apiCount(st, "audit_log", bson.M{"action": "Work policy mapping edited"}) != 1 {
		t.Fatal("edit not audited")
	}
	// A switched-off mapping is history: switch it on (or add a new one) to change it.
	off := apiDoc(st, "work_policy_map", bson.M{"policy_id": ops, "level": "dept_location", "on_map": false})
	if s, r := c.do("PUT", fmt.Sprintf("/api/policies/%s/maps/%v", ops, off["id"]), hr, hrRole, map[string]any{"policyId": corp}); s != 400 {
		t.Fatalf("edit of a switched-off mapping: %d %v", s, r)
	}
	// Switching the old copy on again: the pair is now on Operations – 6 day.
	combo["on"] = true
	if s, r := c.do("PUT", "/api/policies/"+ops+"/maps", hr, hrRole, maps(fo, combo)); s != 409 || !strings.Contains(fmt.Sprint(r["error"]), "Operations – 6 day") {
		t.Fatalf("switching the moved pair on again: %d %v", s, r)
	}
}

// Bulk update › Work policy mapping: a pair on another policy is an error unless Move = Yes; Active = No switches a
// pair off; unknown names are errors; the same pair twice in one file is refused on the later row.
func TestBulkPolicyMapping(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	hub := "Gurugram – Sector 29 Hub"
	hubID := apiDoc(st, "location", bson.M{"name": hub})["id"].(string)
	pol := func(name string) string { return apiDoc(st, "work_policy", bson.M{"name": name})["id"].(string) }
	ops, six := pol("Operations – 24x7 rotational"), pol("Operations – 6 day")
	if s, _ := c.sendFile("/api/bulk/polmap/check", "EMP-1020", "Employee", "m.csv", []byte("Work Policy\n")); s != 403 {
		t.Fatalf("an employee may not upload policy mappings: %d", s)
	}
	run := func(action, csv string) map[int]map[string]any {
		t.Helper()
		s, r := c.sendFile("/api/bulk/polmap/"+action, hr, hrRole, "polmap.csv", []byte(csv))
		if s != 200 {
			t.Fatalf("%s: %d %v", action, s, r)
		}
		return bulkRows(t, r)
	}
	// First: Front Office at the hub onto the 24x7 policy.
	head := "Work Policy,Department,Location,Active,Move\n"
	rows := run("apply", head+"Operations – 24x7 rotational,Front Office,"+hub+",,\n")
	if rowErr(rows[2]) != "[]" || apiCount(st, "work_policy_map", bson.M{"policy_id": ops, "level": "dept_location", "ref": "Front Office|" + hubID, "on_map": true}) != 1 {
		t.Fatalf("pair added: %v", rows[2])
	}
	rows = run("check", head+"Operations – 24x7 rotational,Front Office,"+hub+",Yes,\n") // already on that policy: no change
	if len(rows[2]["changes"].([]any)) != 0 || rowErr(rows[2]) != "[]" {
		t.Fatalf("already there: %v", rows[2])
	}
	csv := head +
		"Operations – 6 day,Front Office," + hub + ",,\n" + // 2: on another policy, no Move: error
		"No such policy,Front Office,Nowhere,,\n" + // 3: unknown policy and location
		"Operations – 6 day,No such dept," + hub + ",,\n" + // 4: unknown department
		"Operations – 6 day,F&B," + hub + ",No,\n" + // 5: Active No on a policy that does not have it
		"Operations – 24x7 rotational,front office," + hub + ",Yes,Yes\n" // 6: the same pair again (case aside): refused
	rows = run("check", csv)
	if !strings.Contains(rowErr(rows[2]), "Already applied to Operations – 24x7 rotational") {
		t.Fatalf("duplicate: %v", rows[2])
	}
	if e := rowErr(rows[3]); !strings.Contains(e, "Work policy “No such policy” not found") || !strings.Contains(e, "Location “Nowhere” not found") {
		t.Fatalf("unknown names: %v", rows[3])
	}
	if !strings.Contains(rowErr(rows[4]), "Department “No such dept” not found") || !strings.Contains(rowErr(rows[5]), "Not applied to Operations – 6 day") {
		t.Fatalf("unknown department / not there: %v %v", rows[4], rows[5])
	}
	if !strings.Contains(rowErr(rows[6]), "Appears again (first on line 2)") {
		t.Fatalf("same pair twice: %v", rows[6])
	}
	// Move = Yes moves it, with the people it moves counted; then Active = No switches it off.
	rows = run("apply", head+"Operations – 6 day,Front Office,"+hub+",Yes,Yes\n")
	if rowErr(rows[2]) != "[]" || changeTo(rows[2], "Work policy") != "Operations – 6 day" {
		t.Fatalf("move: %v", rows[2])
	}
	if ch := rows[2]["changes"].([]any)[0].(map[string]any); !strings.Contains(fmt.Sprint(ch["note"]), "work policy changes") || strings.HasPrefix(fmt.Sprint(ch["note"]), "0 ") {
		t.Fatalf("people moved: %v", ch)
	}
	if apiCount(st, "work_policy_map", bson.M{"policy_id": ops, "level": "dept_location", "on_map": true}) != 0 ||
		apiCount(st, "work_policy_map", bson.M{"policy_id": six, "level": "dept_location", "ref": "Front Office|" + hubID, "on_map": true}) != 1 {
		t.Fatal("not moved")
	}
	if _, ep := c.do("GET", "/api/employees/EMP-1009/policy", hr, hrRole, nil); ep["policy"] != "Operations – 6 day" {
		t.Fatalf("EMP-1009 after the move: %v", ep)
	}
	rows = run("apply", head+"Operations – 6 day,Front Office,"+hub+",No,\n")
	if rowErr(rows[2]) != "[]" || changeTo(rows[2], "Applies to Operations – 6 day") != "Off" ||
		apiCount(st, "work_policy_map", bson.M{"level": "dept_location", "on_map": true}) != 0 || apiCount(st, "work_policy_map", bson.M{"level": "dept_location"}) != 2 {
		t.Fatalf("switch off: %v", rows[2])
	}
	// Current data: the pairs mapped now (none).
	if s, _ := c.raw("/api/bulk/polmap/download?data=1", hr, hrRole); s != 200 {
		t.Fatalf("current data: %d", s)
	}
}
