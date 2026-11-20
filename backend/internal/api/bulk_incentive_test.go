package api_test

import (
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Bulk update › Incentives (owner, 14 Nov 2026): rows become "Incentive" earnings; a closed or locked month and a
// duplicate (employee, month, remark) are refused; the line shows on the row, Upcoming pay lines and the full register.
func TestBulkIncentives(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pdOpenYm(t, c)
	inc := func(emp, role, csv string, apply bool) (int, map[string]any) {
		act := "check"
		if apply {
			act = "apply"
		}
		return c.sendFile("/api/bulk/incentive/"+act, emp, role, "inc.csv", []byte("Employee ID,Month,Amount (₹),Remark\n"+csv))
	}
	// Who: Payroll (pay items at Edit) or HR Admin.
	if s, _ := inc("EMP-1004", "HRBP", "EMP-1020,"+open+",100,x\n", false); s != 403 {
		t.Fatalf("HRBP: %d", s)
	}
	if s, _ := inc("EMP-1020", "Employee", "EMP-1020,"+open+",100,x\n", false); s != 403 {
		t.Fatalf("employee: %d", s)
	}
	_, list := c.do("GET", "/api/bulk", "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(list), "Incentives") {
		t.Fatalf("template list: %v", list)
	}
	csv := "EMP-1020," + open + ",5000,Sales target Q3\n" + // 2: good
		"EMP-1016," + ymText(open, "Jan-2006") + ",\"2,500\",Festival\n" + // 3: good, Mon-YYYY
		"EMP-1020," + open + ",5000,Sales target Q3\n" + // 4: the same row twice
		"EMP-1020," + ymAdd(open, -1) + ",1000,Old month\n" + // 5: closed
		"EMP-1020," + open + ",0,Zero\n" + // 6: amount
		"EMP-9999," + open + ",100,Nobody\n" // 7: unknown
	s, chk := inc("EMP-1011", "Payroll", csv, false)
	rs := bulkRows(t, chk)
	if s != 200 || apiFloat(chk["ok"]) != 2 || changeTo(rs[2], "Incentive") != "₹5,000 in "+ymLong(open) || changeTo(rs[3], "Incentive") != "₹2,500 in "+ymLong(open) {
		t.Fatalf("check: %d %v", s, chk)
	}
	for line, want := range map[int]string{4: "Appears again (first on line 2)", 5: "already paid and closed", 6: "Amount must be", 7: "not found"} {
		if !strings.Contains(rowErr(rs[line]), want) {
			t.Fatalf("line %d: %s, want %q", line, rowErr(rs[line]), want)
		}
	}
	if apiCount(st, "ledger", bson.M{"label": "Incentive"}) != 0 {
		t.Fatal("a check changed something")
	}
	if _, ap := inc("EMP-1011", "Payroll", csv, true); apiFloat(ap["ok"]) != 2 {
		t.Fatalf("apply: %v", ap)
	}
	l := apiDoc(st, "ledger", bson.M{"employee_id": "EMP-1020", "label": "Incentive"})
	if l == nil || l["ym"] != open || l["kind"] != "earn" || apiFloat(l["amount"]) != 5000 || l["reason"] != "Sales target Q3" || l["void"] != false {
		t.Fatalf("line: %v", l)
	}
	if apiCount(st, "audit_log", bson.M{"action": "Incentive added"}) != 2 {
		t.Fatal("not audited")
	}
	// Uploading again is safe: the same employee, month and remark is refused; another remark is a new incentive.
	_, again := inc("EMP-1011", "Payroll", "EMP-1020,"+open+",5000,Sales target Q3\nEMP-1020,"+open+",700,Referral bonus\n", false)
	if ra := bulkRows(t, again); apiFloat(again["ok"]) != 1 || !strings.Contains(rowErr(ra[2]), "Already loaded") {
		t.Fatalf("again: %v", again)
	}
	// Upcoming pay lines.
	if _, up := c.do("GET", "/api/payroll/upcoming", "EMP-1011", "Payroll", nil); !strings.Contains(fmt.Sprint(up["lines"]), "Sales target Q3") {
		t.Fatalf("upcoming: %v", up["lines"])
	}
	// The row and the full register: an earning, not PF or ESI wages.
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	row := c.row(t, open, "EMP-1020")
	r := row["r"].(map[string]any)
	if !strings.Contains(fmt.Sprint(row["add"]), "Incentive") || num(row["totalEarn"]) < num(r["totalEarn"])+5000-0.01 {
		t.Fatalf("row: add %v totalEarn %v r.totalEarn %v", row["add"], row["totalEarn"], r["totalEarn"])
	}
	if num(r["gross"]) > num(row["gross"])+0.01 || num(r["pfBase"]) > num(r["basic"])+0.01 {
		t.Fatalf("incentive in PF or ESI wages: %v", r)
	}
	f, _ := bfBook(t, c, "/api/payroll/"+open+"/full-register.xlsx", "EMP-1012", "Payroll Manager")
	rows := bfSheet(t, f, "Full register")
	col := -1
	for i, h := range rows[1] {
		if h == "Incentive" {
			col = i
		}
	}
	if col < 0 || rows[1][col-1] != "Variable pay" {
		t.Fatalf("no Incentive column after Variable pay: %v", rows[1])
	}
	found := false
	for _, x := range rows[2:] {
		if x[0] == "EMP-1020" {
			found = strings.ReplaceAll(x[col], ",", "") == "5000" || strings.HasPrefix(strings.ReplaceAll(x[col], ",", ""), "5000")
		}
	}
	if !found {
		t.Fatal("EMP-1020's incentive is not in the register's Incentive column")
	}
	// Locked: the open month's Reimbursement closure is closed, so it goes to the next month or later.
	_, lk := inc("EMP-1002", "HR Admin", "EMP-1020,"+open+",900,After the close\nEMP-1020,"+ymAdd(open, 1)+",900,After the close\n", false)
	if rl := bulkRows(t, lk); apiFloat(lk["ok"]) != 1 || !strings.Contains(rowErr(rl[2]), "locked for new pay lines") || !strings.Contains(rowErr(rl[2]), ymLong(ymAdd(open, 1))) {
		t.Fatalf("locked month: %v", lk)
	}
}
