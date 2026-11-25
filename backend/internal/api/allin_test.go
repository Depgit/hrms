package api_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The all-inclusive gross from a month (owner, 26 Nov 2026; allin.go): the setting applies from its month only, a
// published month is never changed by it, and a change that would alter an approved or published month is refused.
func TestAllInGrossFromMonth(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1016")
	put := func(emp, role, from string) (int, map[string]any) {
		return c.do("PUT", "/api/comp/all-in", emp, role, map[string]any{"from": from})
	}
	if s, _ := put("EMP-1016", "Employee", "2030-01"); s != 403 {
		t.Fatalf("an employee changed the setting: %d", s)
	}
	if s, _ := put("EMP-1002", "HR Admin", "2026-13"); s != 400 {
		t.Fatal("a bad month was accepted")
	}
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	next := addMonth(open, 1)
	id := "EMP-1016"

	// The open month, off: the old rule. Published, then its stored row is the reference.
	c.closeAndPublish(t, open)
	before := apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": id})
	if before == nil {
		t.Fatalf("no row for %s in %s", id, open)
	}
	r0 := c.row(t, open, id)["r"].(map[string]any)
	if r0["allIn"] == true {
		t.Fatal("all-inclusive before the setting")
	}

	// From the published month: refused. From the next month: saved and audited.
	if s, r := put("EMP-1002", "HR Admin", open); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "already approved or published") {
		t.Fatalf("from a published month: %d %v", s, r)
	}
	if s, r := put("EMP-1002", "HR Admin", next); s != 200 || r["from"] != next {
		t.Fatalf("set %s: %d %v", next, s, r)
	}
	if n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"entity": "company_setting", "action": bson.M{"$regex": "^All-inclusive gross"}}); n < 1 {
		t.Fatal("not audited")
	}
	if _, g := c.do("GET", "/api/comp/all-in", "EMP-1002", "HR Admin", nil); g["from"] != next {
		t.Fatalf("GET: %v", g)
	}

	// The published month is unchanged; the next month is paid on the all-inclusive gross.
	after := apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": id})
	if fmt.Sprint(after["data"]) != fmt.Sprint(before["data"]) || after["net"] != before["net"] {
		t.Fatal("the published month changed")
	}
	c.pcCloseAll(t, next)
	if s, r := c.do("POST", "/api/payroll/"+next+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("dry run %s: %v", next, r)
	}
	row := c.row(t, next, id)
	r := row["r"].(map[string]any)
	if r["allIn"] != true {
		t.Fatalf("%s not all-inclusive: %v", next, r)
	}
	g, earned := num(row["gross"]), num(row["earned"])
	if math.Abs(num(r["offeredFull"])-g) > 0.005 || math.Abs(num(r["offered"])-earned) > 0.01 {
		t.Fatalf("offered %v / %v, row gross %v earned %v", r["offeredFull"], r["offered"], g, earned)
	}
	// The gross is G earned; the employer's PF, ESI, EDLI and LWF are deducted from the employee; the company's cost is
	// the admin charges, gratuity and medical insurance.
	if math.Abs(num(r["gross"])-earned) > 0.01 {
		t.Fatalf("gross %v, G earned %v", r["gross"], earned)
	}
	erDed := num(r["erPF"]) + num(r["erESI"]) + num(r["edli"]) + num(r["lwfEr"])
	if math.Abs(num(r["erDed"])-erDed) > 0.01 || (num(r["erPF"]) > 0 && num(r["erDed"]) == 0) {
		t.Fatalf("employer's shares deducted %v, want %v", r["erDed"], erDed)
	}
	if math.Abs(num(r["employerCost"])-(num(r["admin"])+num(r["grat"])+num(r["med"]))) > 0.01 {
		t.Fatalf("employer cost %v", r["employerCost"])
	}
	if math.Abs(num(row["totalDed"])-num(r["ded"])-num(row["stds"])) > 0.02 && len(row["ded"].([]any)) == 0 && num(row["od"]) == 0 {
		t.Fatalf("row deductions %v vs %v", row["totalDed"], r["ded"])
	}
	// The payslip lists the employer's PF as a deduction.
	s, slip := c.do("GET", "/api/payslip-preview?ym="+next+"&employeeId="+id, "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatalf("payslip preview: %v", slip)
	}
	if num(r["erPF"]) > 0 {
		snap, _ := slip["snap"].(map[string]any)
		if !strings.Contains(fmt.Sprint(snap["ded"]), "Employer PF (incl. EPS)") {
			t.Fatalf("payslip deductions %v", snap["ded"])
		}
	}
	// Variable payouts are not entered for months on the rule.
	if s, v := c.do("GET", "/api/pay/variable?ym="+next, "EMP-1011", "Payroll", nil); s != 200 || v["allInNote"] == nil || v["can"] != false {
		t.Fatalf("variable pay list: %d %v", s, v)
	}
	if s, v := c.do("PUT", "/api/pay/variable", "EMP-1011", "Payroll", map[string]any{"ym": next, "employeeId": id, "code": "var", "amount": 1000}); s != 400 {
		t.Fatalf("variable payout entered: %d %v", s, v)
	}

	// Publish the next month: now neither switching off nor moving the month may alter it.
	c.closeAndPublish(t, next)
	pub := apiDoc(st, "payroll_row", bson.M{"ym": next, "employee_id": id})
	for _, to := range []string{"", addMonth(next, 1), open} {
		if s, r := put("EMP-1002", "HR Admin", to); s != 400 {
			t.Fatalf("change to %q with %s published: %d %v", to, next, s, r)
		}
	}
	if got := apiDoc(st, "payroll_row", bson.M{"ym": next, "employee_id": id}); fmt.Sprint(got["data"]) != fmt.Sprint(pub["data"]) {
		t.Fatal("a published month changed")
	}
	// Saving the same month again changes nothing: allowed.
	if s, r := put("EMP-1002", "HR Admin", next); s != 200 || r["from"] != next {
		t.Fatalf("same month again: %d %v", s, r)
	}
}

// addMonth is "YYYY-MM" plus n months.
func addMonth(ym string, n int) string {
	var y, m int
	fmt.Sscanf(ym, "%d-%d", &y, &m)
	m += n
	for m > 12 {
		m -= 12
		y++
	}
	return fmt.Sprintf("%04d-%02d", y, m)
}
