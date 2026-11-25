package api_test

import (
	"context"
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Retainer TDS editable per employee (owner, 25 Nov 2026): Pay options sets a retainer's own TDS %, which the payroll
// uses instead of the structure's (1% by default); a permanent employee cannot get one; tdsSet with null goes back to
// the structure's rate.
func TestRetainerTdsOverride(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011")
	ctx := context.Background()
	ret, _ := st.DB.C("employee").FindOne(ctx, bson.M{"type": "Retainer", "status": "Active"})
	if ret == nil {
		t.Skip("no retainer in the seed")
	}
	id := ret["id"].(string)
	if s, r := c.do("PUT", "/api/employees/"+id+"/pay", "EMP-1002", "HR Admin", map[string]any{"meal": 0, "accommodation": 0, "tdsSet": true, "tdsPct": 3, "reason": "Policy change"}); s != 200 {
		t.Fatalf("set 3%%: %d %v", s, r)
	}
	if p := apiDoc(st, "employee_pay", bson.M{"employee_id": id}); p["tds_pct"] != 3.0 {
		t.Fatalf("tds_pct %v", p["tds_pct"])
	}
	if s, _ := c.do("PUT", "/api/employees/"+id+"/pay", "EMP-1002", "HR Admin", map[string]any{"meal": 0, "accommodation": 0, "tdsSet": true, "tdsPct": 60, "reason": "Policy change"}); s != 400 {
		t.Fatal("60% allowed")
	}
	if s, _ := c.do("PUT", "/api/employees/EMP-1016/pay", "EMP-1002", "HR Admin", map[string]any{"meal": 0, "accommodation": 0, "tdsSet": true, "tdsPct": 2, "reason": "Policy change"}); s != 400 {
		t.Fatal("a permanent employee got a TDS %")
	}
	// Without tdsSet the rate stays.
	if s, r := c.do("PUT", "/api/employees/"+id+"/pay", "EMP-1002", "HR Admin", map[string]any{"meal": 0, "accommodation": 0, "reason": "Policy change"}); s != 200 {
		t.Fatalf("plain save: %v", r)
	}
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.pcCloseAll(t, open)
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("dry run: %v", r)
	}
	row := func() map[string]any {
		_, rows := c.do("GET", "/api/payroll/"+open+"/rows?q="+id, "EMP-1011", "Payroll", nil)
		l, _ := rows["rows"].([]any)
		if len(l) != 1 {
			t.Fatalf("rows %v", rows)
		}
		return l[0].(map[string]any)
	}
	r := row()["r"].(map[string]any)
	if gross := r["gross"].(float64); r["tdsRate"] != 3.0 || math.Abs(r["tds"].(float64)-gross*0.03) > 0.01 {
		t.Fatalf("own 3%%: tds %v rate %v on %v", r["tds"], r["tdsRate"], gross)
	}
	// Back to the structure's rate (Version 1 in the seed: 1%, calc.DefaultStructure).
	if s, r := c.do("PUT", "/api/employees/"+id+"/pay", "EMP-1002", "HR Admin", map[string]any{"meal": 0, "accommodation": 0, "tdsSet": true, "tdsPct": nil, "reason": "Policy change"}); s != 200 {
		t.Fatalf("clear: %v", r)
	}
	if p := apiDoc(st, "employee_pay", bson.M{"employee_id": id}); p["tds_pct"] != nil {
		t.Fatalf("tds_pct after clear %v", p["tds_pct"])
	}
}
