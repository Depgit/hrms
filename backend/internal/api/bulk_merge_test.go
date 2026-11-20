package api_test

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Salary breakup with the variable payout columns (17 Nov 2026, the Variable payouts template merged in): one row
// changes the meal deduction and pays the variable component; both apply.
func TestBulkSalaryWithPayout(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011")
	open := pdOpenYm(t, c)
	s, made := c.do("POST", "/api/comp/structures/designed", "EMP-1002", "HR Admin", map[string]any{"name": "Sales 2027", "comps": payCompExample()})
	if s != 200 {
		t.Fatalf("designed: %v", made)
	}
	if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1002", "HR Admin", map[string]any{"structureId": made["id"], "fromDate": open + "-01", "ids": []string{"EMP-1020"}}); s != 200 {
		t.Fatalf("map: %v", r)
	}
	// A payout needs a variable target (18 Nov 2026).
	if s, r := c.do("PUT", "/api/pay/variable/target", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "fromMonth": open, "total": 3000, "reason": "Test"}); s != 200 {
		t.Fatalf("target: %v", r)
	}
	reason := apiDoc(st, "simple_master", bson.M{"kind": "change_reason", "used_for": "Salary", "status": "Active"})
	if reason == nil {
		t.Skip("no salary change reason")
	}
	file := "Employee ID,Monthly Gross,Variable Pay %,Variable Payout Cycle,Meal Deduction,Accommodation Deduction,Salary Structure,Structure Effective From,Reason for change,Effective From,Payout Month,Payout Component,Variable Payout (₹),Payout Note\n" +
		"EMP-1020,,,,750,,,," + reason["name"].(string) + "," + open + "-01," + open + ",,9000,Q rating\n"
	s, r := c.sendFile("/api/bulk/sal/apply", "EMP-1002", "HR Admin", "sal.csv", []byte(file))
	rs := bulkRows(t, r)
	if s != 200 || apiFloat(r["ok"]) != 1 || changeTo(rs[2], "Meal deduction") != "₹750" || changeTo(rs[2], "Performance pay in "+ymLong(open)) != "₹9,000" {
		t.Fatalf("apply: %d %v", s, r)
	}
	if v := apiDoc(st, store.VarPayoutColl, bson.M{"employee_id": "EMP-1020", "ym": open, "code": "perf"}); v == nil || apiFloat(v["amount"]) != 9000 {
		t.Fatalf("payout: %v", v)
	}
	if p := apiDoc(st, "employee_pay", bson.M{"employee_id": "EMP-1020"}); p == nil || apiFloat(p["meal"]) != 750 {
		t.Fatalf("meal: %v", p)
	}
}

// Import employees then the later-change templates (17 Nov 2026): the imported sizes and opening balances count as
// given (not in the no-sizes list) and come back in the current-data downloads; those downloads uploaded as they are
// change nothing and ask for nothing.
func TestBulkImportedValuesCountAsGiven(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002")
	head := "Employee Code,Full Name,Employment Type,Date of Joining,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Role,Skill,Mobile,Email,Date of Birth,Gender,PAN,Aadhaar,UAN,Existing UAN?,ESIC No,Bank Name,Account Number,IFSC,Status,Last Working Day,Opening CL Balance,Opening PL Balance,Opening SL Balance,Shirt / T-shirt Size,Trouser Waist,Trouser Length,Shoe Size (UK)\n"
	row := "NG-9,Imported Person,Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,Kitchen,Cooking,,,,,,,,,,,,,,,4,6,,M,32,30,8\n"
	if s, r := c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(head+row)); s != 200 || apiFloat(r["ok"]) != 1 {
		t.Fatalf("import: %d %v", s, r)
	}
	_, sz := c.do("GET", "/api/inventory/sizes", "EMP-1002", "HR Admin", nil)
	if strings.Contains(fmt.Sprint(sz["missing"]), "NG-9") {
		t.Fatalf("imported sizes listed as missing: %v", sz["missing"])
	}
	// The current data, as uploaded back unchanged (NG-9's rows only).
	current := func(tpl string) string {
		s, b := c.raw("/api/bulk/"+tpl+"/download?data=1", "EMP-1002", "HR Admin")
		if s != 200 {
			t.Fatalf("%s download: %d %s", tpl, s, b)
		}
		x, err := excelize.OpenReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := x.GetRows("Data")
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write(rows[0])
		for _, r := range rows[1:] {
			if len(r) > 0 && r[0] == "NG-9" {
				_ = w.Write(r)
			}
		}
		w.Flush()
		return buf.String()
	}
	lv := current("lvopen")
	if !strings.Contains(lv, "NG-9,Casual leave,4,") || !strings.Contains(lv, "NG-9,Privilege leave,6,") {
		t.Fatalf("opening balances not prefilled: %s", lv)
	}
	if !strings.Contains(current("uni"), "NG-9,M,32,30,8") {
		t.Fatal("sizes not prefilled")
	}
	for _, tpl := range []string{"lvopen", "upd", "uni"} {
		s, r := c.sendFile("/api/bulk/"+tpl+"/check", "EMP-1002", "HR Admin", tpl+".csv", []byte(current(tpl)))
		if s != 200 || apiFloat(r["ok"]) != 0 {
			t.Fatalf("%s unchanged upload: %d %v", tpl, s, r)
		}
		for _, x := range r["results"].([]any) {
			if m := x.(map[string]any); len(m["errors"].([]any)) > 0 || len(m["changes"].([]any)) > 0 {
				t.Fatalf("%s unchanged upload: %v", tpl, m)
			}
		}
	}
}
