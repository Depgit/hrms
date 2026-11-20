package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Pay components (workforce plan, module 3): nothing changes for the converted structures. Every person of the
// sample organisation is calculated as before components and as now, over five months, with a second percentage
// version (other rates, PF on basic + special allowance, quarterly variable) mapped from inside a month, personal
// variable %, cycles, meal deductions and a mid-month gross revision; first with the structures converted on the
// fly, then with the template migration 014 stores. There must be no difference at all.
func TestPayCompareLegacyUnchanged(t *testing.T) {
	_, app, st := setupFull(t)
	ctx := context.Background()
	q := st.DB
	open := ""
	if m, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}); m != nil {
		open, _ = m["first_ym"].(string)
	}
	if open == "" {
		open = today()[:7]
	}
	v2 := calc.DefaultStructure
	v2.BasicPct, v2.HraPct, v2.VarPct, v2.VarCycle, v2.PfWage = 40, 50, 10, 3, "basicSa"
	sid, err := q.C("salary_structure").Insert(ctx, bson.M{"name": "Version 2", "rates": v2})
	if err != nil {
		t.Fatal(err)
	}
	emps, _ := q.C("employee").Find(ctx, bson.M{"status": "Active"})
	if len(emps) < 10 {
		t.Fatalf("only %d employees", len(emps))
	}
	for i, e := range emps {
		id := fmt.Sprint(e["id"])
		switch i % 5 {
		case 0: // a structure change inside a month: blended by days
			_, _ = q.C("structure_map").Insert(ctx, bson.M{"employee_id": id, "from_date": ymAdd(open, -2) + "-11", "structure_id": sid})
		case 1:
			_, _ = q.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"var_pct": 12.5, "var_cycle": int32(6), "meal": 500.0, "accommodation": 0.0}, nil)
		case 2: // a gross revision from the 15th
			_, _ = q.C("gross_history").Upsert(ctx, bson.M{"employee_id": id, "from_month": ymAdd(open, -1)},
				bson.M{"gross": 31234.56, "from_date": ymAdd(open, -1) + "-15", "reason": "Test"}, nil)
		case 3:
			_, _ = q.C("structure_map").Insert(ctx, bson.M{"employee_id": id, "from_date": "2000-01-01", "structure_id": sid})
		}
	}
	months := []string{ymAdd(open, -3), ymAdd(open, -2), ymAdd(open, -1), open, ymAdd(open, 1)}
	check := func(when string) {
		t.Helper()
		total := 0
		for _, ym := range months {
			res, err := app.PayCompare(ctx, ym)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Diffs) > 0 {
				t.Fatalf("%s, %s: %d people differ, first %s: %v", when, ym, len(res.Diffs), res.Diffs[0].EmployeeID, res.Diffs[0].Fields)
			}
			if res.Designed != 0 {
				t.Fatalf("%s: %d on designed structures", ym, res.Designed)
			}
			total += res.Compared
		}
		if total < 20 {
			t.Fatalf("%s: only %d rows compared", when, total)
		}
	}
	check("converted on the fly")
	n, err := store.ConvertLegacyStructures(ctx, q)
	if err != nil || n < 2 {
		t.Fatalf("converted %d: %v", n, err)
	}
	if again, _ := store.ConvertLegacyStructures(ctx, q); again != 0 {
		t.Fatalf("converted again: %d", again)
	}
	x, _ := q.C("salary_structure").FindOne(ctx, bson.M{"_id": sid})
	comps, _ := x["rates"].(map[string]any)["comps"].([]any)
	if len(comps) != 4 || comps[0].(map[string]any)["method"] != "pct_gross" || comps[3].(map[string]any)["method"] != "legacy_var" {
		t.Fatalf("stored template: %v", comps)
	}
	check("stored template")
}

func payCompExample() []map[string]any {
	return []map[string]any{
		{"code": "basic", "name": "Basic", "kind": "fixed", "side": "earn", "role": "basic", "method": "pct_gross", "value": 50, "taxable": true, "pf": true, "esi": true, "bonus": true, "payslip": true},
		{"code": "hra", "name": "HRA", "kind": "fixed", "side": "earn", "role": "hra", "method": "pct_basic", "value": 40, "taxable": true, "esi": true, "payslip": true},
		{"code": "sa", "name": "Special allowance", "kind": "fixed", "side": "earn", "method": "remainder", "taxable": true, "esi": true, "payslip": true},
		{"code": "canteen", "name": "Canteen", "kind": "fixed", "side": "ded", "method": "amount", "value": 300, "payslip": true},
		{"code": "perf", "name": "Performance pay", "kind": "variable", "side": "earn", "method": "remainder", "cycle": 1, "taxable": true, "payslip": true},
	}
}

// A structure made from components, end to end: designer checks, the CTC split, the wage warning, the variable
// target, the payroll check for an amount not entered, the amount entered (screen and bulk), the payslip, the full
// and final.
func TestPayComponentsDesigned(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pdOpenYm(t, c)

	// The library and a converted structure's template.
	s, lib := c.do("GET", "/api/comp/components", "EMP-1002", "HR Admin", nil)
	if s != 200 || len(lib["components"].([]any)) < 4 {
		t.Fatalf("library: %d %v", s, lib)
	}
	if s, _ := c.do("GET", "/api/comp/components", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee sees the library: %d", s)
	}
	if s, r := c.do("POST", "/api/comp/components", "EMP-1002", "HR Admin", map[string]any{"code": "conv", "name": "Conveyance", "kind": "fixed", "side": "earn",
		"method": "amount", "value": 1600, "max": 1600, "esi": true, "payslip": true}); s != 200 {
		t.Fatalf("add component: %v", r)
	}
	if s, _ := c.do("POST", "/api/comp/components", "EMP-1002", "HR Admin", map[string]any{"code": "conv", "name": "Other", "kind": "fixed", "side": "earn", "method": "amount"}); s != 400 {
		t.Fatalf("duplicate code: %d", s)
	}
	_, comp := c.do("GET", "/api/comp", "EMP-1002", "HR Admin", nil)
	var defID string
	for _, x := range comp["structures"].([]any) {
		if m := x.(map[string]any); m["is_default"] == true {
			defID = m["id"].(string)
		}
	}
	_, conv := c.do("GET", "/api/comp/structures/"+defID+"/components", "EMP-1002", "HR Admin", nil)
	if conv["converted"] != true || len(conv["comps"].([]any)) != 4 || conv["wage"].(map[string]any)["ok"] != true {
		t.Fatalf("converted template: %v", conv)
	}

	// Designer checks.
	bad := payCompExample()
	bad[4]["method"], bad[4]["kind"] = "remainder", "fixed"
	if s, r := c.do("POST", "/api/comp/structures/designed", "EMP-1002", "HR Admin", map[string]any{"name": "Two balances", "comps": bad}); s != 400 || !strings.Contains(fmt.Sprint(r), "balancing") {
		t.Fatalf("two balancing: %d %v", s, r)
	}
	loop := payCompExample()
	loop = append(loop, map[string]any{"code": "x", "name": "X", "kind": "fixed", "side": "earn", "method": "pct_of", "of": "y", "value": 10},
		map[string]any{"code": "y", "name": "Y", "kind": "fixed", "side": "earn", "method": "pct_of", "of": "x", "value": 10})
	if s, r := c.do("POST", "/api/comp/structures/designed", "EMP-1002", "HR Admin", map[string]any{"name": "Loop", "comps": loop}); s != 400 || !strings.Contains(fmt.Sprint(r), "loop") {
		t.Fatalf("loop: %d %v", s, r)
	}
	s, made := c.do("POST", "/api/comp/structures/designed", "EMP-1002", "HR Admin", map[string]any{"name": "Sales 2027", "comps": payCompExample()})
	if s != 200 {
		t.Fatalf("designed: %v", made)
	}
	sid := made["id"].(string)

	// Preview: ₹35,000 = ₹20,000 fixed + ₹15,000 variable, entered by CTC with the variable as an amount.
	s, pv := c.do("POST", "/api/comp/pay-preview", "EMP-1002", "HR Admin", map[string]any{"structureId": sid, "type": "Permanent", "payBy": "ctc", "amount": 35000, "varBy": "amount", "var": 15000})
	b, _ := pv["breakup"].(map[string]any)
	if s != 200 || num(pv["fixed"]) != 20000 || num(pv["variable"]) != 15000 || num(b["basic"]) != 10000 || num(b["hra"]) != 4000 || num(b["variable"]) != 15000 {
		t.Fatalf("preview: %v", pv)
	}
	if w := pv["wage"].(map[string]any); w["ok"] != false || num(w["pct"]) != 28.57 || !strings.Contains(fmt.Sprint(w["note"]), "50%") {
		t.Fatalf("wage: %v", w)
	}
	// By gross with the variable as 30% of the total.
	if _, p2 := c.do("POST", "/api/comp/pay-preview", "EMP-1002", "HR Admin", map[string]any{"structureId": sid, "payBy": "gross", "amount": 21000, "varBy": "pct", "var": 30}); num(p2["variable"]) != 9000 {
		t.Fatalf("by gross: %v", p2)
	}

	// EMP-1020 moves to it from the open month; the variable target is set.
	if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1002", "HR Admin", map[string]any{"structureId": sid, "fromDate": open + "-01", "ids": []string{"EMP-1020"}}); s != 200 {
		t.Fatalf("map: %v", r)
	}
	if s, r := c.do("PUT", "/api/pay/variable/target", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "fromMonth": open, "total": 15000, "reason": "Test"}); s != 200 {
		t.Fatalf("target: %v", r)
	}
	if s, _ := c.do("PUT", "/api/pay/variable/target", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1016", "fromMonth": open, "total": 100}); s != 400 {
		t.Fatalf("target on a percentage structure: %d", s)
	}
	_, vl := c.do("GET", "/api/pay/variable?ym="+open, "EMP-1011", "Payroll", nil)
	if fmt.Sprint(vl["due"]) != "1" || fmt.Sprint(vl["entered"]) != "0" || !strings.Contains(fmt.Sprint(vl["rows"]), "EMP-1020") {
		t.Fatalf("variable list: %v", vl)
	}

	// Not entered: not paid, and the payroll checks say so.
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	row := c.row(t, open, "EMP-1020")
	r := row["r"].(map[string]any)
	if num(r["varPay"]) != 0 || !strings.Contains(fmt.Sprint(row["varMissing"]), "Performance pay") || num(r["compDed"]) != 300 || len(r["comps"].([]any)) != 5 {
		t.Fatalf("row before entry: varMissing %v r %v", row["varMissing"], r)
	}
	_, ck := c.do("GET", "/api/payroll/"+open+"/checks", "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(ck), "Variable pay due but not entered") {
		t.Fatalf("checks: %v", ck)
	}

	// Entered on the screen, then changed by the bulk template; paid in the run.
	if s, r := c.do("PUT", "/api/pay/variable", "EMP-1020", "Employee", map[string]any{"ym": open, "employeeId": "EMP-1020", "code": "perf", "amount": 5}); s != 403 {
		t.Fatalf("employee enters: %d %v", s, r)
	}
	if s, r := c.do("PUT", "/api/pay/variable", "EMP-1011", "Payroll", map[string]any{"ym": open, "employeeId": "EMP-1020", "code": "perf", "amount": 12000, "note": "Q rating"}); s != 200 {
		t.Fatalf("enter: %v", r)
	}
	s, chk := c.sendFile("/api/bulk/sal/check", "EMP-1011", "Payroll", "v.csv", []byte("Employee ID,Payout Month,Payout Component,Variable Payout (₹),Payout Note\nEMP-1020,"+open+",Performance pay,13000,Revised\nEMP-1016,"+open+",Performance pay,10,x\n"))
	rs := bulkRows(t, chk)
	if s != 200 || apiFloat(chk["ok"]) != 1 || changeTo(rs[2], "Performance pay in "+ymLong(open)) != "₹13,000" || !strings.Contains(rowErr(rs[3]), "not a variable component") {
		t.Fatalf("bulk check: %v", chk)
	}
	if v := apiDoc(st, store.VarPayoutColl, bson.M{"employee_id": "EMP-1020"}); apiFloat(v["amount"]) != 12000 {
		t.Fatalf("a check changed the amount: %v", v)
	}
	if _, ap := c.sendFile("/api/bulk/sal/apply", "EMP-1011", "Payroll", "v.csv", []byte("Employee ID,Payout Month,Payout Component,Variable Payout (₹),Payout Note\nEMP-1020,"+open+",perf,13000,Revised\n")); apiFloat(ap["ok"]) != 1 {
		t.Fatalf("bulk apply: %v", ap)
	}
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	row = c.row(t, open, "EMP-1020")
	r = row["r"].(map[string]any)
	if num(r["varPay"]) != 13000 || row["varMissing"] != nil {
		t.Fatalf("row after entry: %v %v", row["varMissing"], r["varPay"])
	}
	_, ck = c.do("GET", "/api/payroll/"+open+"/checks", "EMP-1011", "Payroll", nil)
	if strings.Contains(fmt.Sprint(ck), "Variable pay due but not entered") {
		t.Fatalf("checks after entry: %v", ck)
	}
	// The payslip preview shows the components by name.
	if s, slip := c.do("GET", "/api/payslip-preview?ym="+open+"&employeeId=EMP-1020", "EMP-1011", "Payroll", nil); s != 200 ||
		!strings.Contains(fmt.Sprint(slip), "Performance pay") || !strings.Contains(fmt.Sprint(slip), "Canteen") || strings.Contains(fmt.Sprint(slip), "House rent allowance") {
		t.Fatalf("payslip: %d %v", s, slip)
	}

	// The full and final takes an amount for the current cycle.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"lwd": open + "-20"}}); err != nil {
		t.Fatal(err)
	}
	_, f := c.do("GET", "/api/fnf/EMP-1020", "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(f["lines"]), "not entered") {
		t.Fatalf("fnf before: %v", f["lines"])
	}
	if s, r := c.do("PUT", "/api/fnf/EMP-1020/variable", "EMP-1011", "Payroll", map[string]any{"code": "perf", "amount": 4000}); s != 200 {
		t.Fatalf("fnf amount: %v", r)
	}
	_, f = c.do("GET", "/api/fnf/EMP-1020", "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(f["lines"]), "Performance pay, current cycle") || !strings.Contains(fmt.Sprint(f["lines"]), "4000") {
		t.Fatalf("fnf after: %v", f["lines"])
	}
}
