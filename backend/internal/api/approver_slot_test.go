package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Approver names tab, a name picked in a cell (owner, 26 Nov 2026): saved as Role mapping, moved from the old holder,
// cleared; Role mapping Edit needed; an inactive person refused.
func TestApproverSlotCell(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const hr, hrRole = "EMP-1002", "HR Admin"
	ctx := context.Background()

	_, d := c.do("GET", "/api/approvals/approvers", hr, hrRole, nil)
	var placeID, role string
	for _, l := range d["locations"].([]any) {
		lm := l.(map[string]any)
		for _, r := range []string{"Property Manager", "Cluster Manager", "Operations Head", "HRBP"} {
			if p, _ := lm["people"].(map[string]any)[r].(map[string]any); p != nil && p["id"] == "" {
				placeID, role = lm["id"].(string), r
				break
			}
		}
		if placeID != "" {
			break
		}
	}
	if placeID == "" {
		t.Fatal("no empty property slot")
	}
	holder := func() string {
		_, d := c.do("GET", "/api/approvals/approvers", hr, hrRole, nil)
		for _, l := range d["locations"].([]any) {
			if lm := l.(map[string]any); lm["id"] == placeID {
				return lm["people"].(map[string]any)[role].(map[string]any)["id"].(string)
			}
		}
		return "?"
	}
	put := func(emp, empRole string, id any) (int, map[string]any) {
		return c.do("PUT", "/api/approvals/approvers/slot", emp, empRole, map[string]any{"role": role, "level": "Property", "place": placeID, "employeeId": id})
	}
	// Two active people without this role: the first and the second holder.
	var a, b string
	for _, e := range apiDocs(st, "employee", bson.M{"status": "Active"}) {
		id := e["id"].(string)
		if id == hr || apiCount(st, "user_role_map", bson.M{"employee_id": id, "role_name": role}) > 0 {
			continue
		}
		if a == "" {
			a = id
		} else if b == "" {
			b = id
			break
		}
	}
	if b == "" {
		t.Fatal("not enough employees")
	}
	covers := func(id string) bool {
		m := apiDoc(st, "user_role_map", bson.M{"employee_id": id, "role_name": role})
		return m != nil && m["active"] == true && strings.Contains(strings.Join(apprStrs(m["location_ids"]), ","), placeID)
	}

	// Without Role mapping Edit: 403, nothing saved.
	if s, _ := put("EMP-1020", "Employee", a); s != 403 {
		t.Fatalf("employee: %d", s)
	}
	// An inactive person is refused.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1021"}, bson.M{"$set": bson.M{"status": "Inactive"}}); err != nil {
		t.Fatal(err)
	}
	if s, r := put(hr, hrRole, "EMP-1021"); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "not active") {
		t.Fatalf("inactive: %d %v", s, r)
	}
	if holder() != "" {
		t.Fatal("inactive person was mapped")
	}

	// Set: a gets a Selected locations mapping with the property.
	if s, r := put(hr, hrRole, a); s != 200 {
		t.Fatalf("set: %d %v", s, r)
	}
	if !covers(a) || holder() != a {
		t.Fatalf("set: mapping %v, holder %s", apiDoc(st, "user_role_map", bson.M{"employee_id": a, "role_name": role}), holder())
	}
	// Change: a loses the place (their only one, so the mapping is switched off), b gets it.
	if s, r := put(hr, hrRole, b); s != 200 {
		t.Fatalf("change: %d %v", s, r)
	}
	if covers(a) || !covers(b) || holder() != b {
		t.Fatalf("change: a %v, b %v, holder %s", apiDoc(st, "user_role_map", bson.M{"employee_id": a, "role_name": role}),
			apiDoc(st, "user_role_map", bson.M{"employee_id": b, "role_name": role}), holder())
	}
	// Clear.
	if s, r := put(hr, hrRole, nil); s != 200 {
		t.Fatalf("clear: %d %v", s, r)
	}
	if covers(b) || holder() != "" {
		t.Fatalf("clear: holder %s", holder())
	}
	if n := apiCount(st, "audit_log", bson.M{"entity": "user_role_map", "action": bson.M{"$regex": `\(Approver names\)`}}); n != 4 { // set, change (a out, b in), clear
		t.Fatalf("audit: %d", n)
	}
}
