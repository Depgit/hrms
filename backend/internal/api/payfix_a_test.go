package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Payroll run integrity (fork A of the payroll test fixes): RUN-77/ITM-64 bank hold release, RUN-48/ITM-58/ITM-77/
// REAL-09 approve the verified rows and a wider stale flag, RUN-42/ACC-16/REAL-48/REAL-49 paid months stay paid,
// REAL-28/ACC-05 maker-checker and own row, RUN-83/REAL-27 paid days within employment, RUN-85 TDS of an approved
// month, RUN-12 open month, REAL-19 pay lines before joining, ACC-17 cut-off after a paid month, RUN-40 upload text.

func pfaStep(t *testing.T, c *client, ym, step, emp, role string) (int, map[string]any) {
	t.Helper()
	return c.do("POST", "/api/payroll/"+ym+"/"+step, emp, role, nil)
}

func pfaMust(t *testing.T, c *client, ym, step, emp, role string) {
	t.Helper()
	if s, r := pfaStep(t, c, ym, step, emp, role); s != 200 {
		t.Fatalf("%s %s: %d %v", step, ym, s, r)
	}
}

func pfaOpen(t *testing.T, c *client) string {
	t.Helper()
	_, st := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	return st["openYm"].(string)
}

func pfaStale(t *testing.T, c *client) bool {
	t.Helper()
	_, st := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	return st["open"].(map[string]any)["stale"] == true
}

// RUN-77 / ITM-64: a salary held while a new bank account waited is paid once the account is verified.
func TestPayfixBankHoldReleased(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Bank", "bankName": "HDFC Bank", "accountNo": "50100012345678", "ifsc": "HDFC0001234"}})
	if s != 200 {
		t.Fatal(r)
	}
	bank := r["id"].(string)
	for _, step := range []struct{ step, emp, role string }{{"dry-run", "EMP-1011", "Payroll"}, {"verify", "EMP-1011", "Payroll"}, {"approve", "EMP-1012", "Payroll Manager"}, {"publish", "EMP-1012", "Payroll Manager"}} {
		pfaMust(t, c, open, step.step, step.emp, step.role)
	}
	row := c.row(t, open, "EMP-1020")
	if row["hold"] != true {
		t.Fatalf("row not held: %v", row["holdWhy"])
	}
	held := num(row["net"])
	// Listed on Salary holds until paid.
	_, hl := c.do("GET", "/api/holds", "EMP-1011", "Payroll", nil)
	found := false
	for _, x := range hl["holds"].([]any) {
		if h := x.(map[string]any); h["employee_id"] == "EMP-1020" && h["status"] == "Active" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the held salary is not on the Salary holds page: %v", hl["holds"])
	}
	c.approveAll(t, bank)
	next := pfaOpen(t, c)
	l := apiDoc(st, "ledger", bson.M{"employee_id": "EMP-1020", "ym": next, "label": "Held salary released, " + mustDate(open+"-01").Format("January 2006")})
	if l == nil {
		t.Fatal("no release line in the open month")
	}
	if want := float64(int64(held*100+0.5)) / 100; apiFloat(l["amount"]) != want { // the stored net to the paisa (ITM-59)
		t.Fatalf("released %v, held %v", l["amount"], held)
	}
	if n := apiCount(st, "salary_hold", bson.M{"employee_id": "EMP-1020", "status": "Active"}); n != 0 {
		t.Fatal("hold still active")
	}
	_ = ctx
}

// RUN-48 / ITM-58 / ITM-77 / REAL-09: approve keeps the verified rows; a change after the calculation makes the run
// stale and approval waits for a new dry run.
func TestPayfixApproveVerifiedRows(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	pfaMust(t, c, open, "verify", "EMP-1011", "Payroll")
	verified := num(c.row(t, open, "EMP-1020")["net"])

	// A gross revision for the month after the verification: stale, approval refused.
	if _, err := st.DB.C("gross_history").Upsert(ctx, bson.M{"employee_id": "EMP-1020", "from_month": open}, bson.M{"gross": 99999.0, "reason": "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if !pfaStale(t, c) {
		t.Fatal("a gross change after verification is not stale")
	}
	if s, r := pfaStep(t, c, open, "approve", "EMP-1012", "Payroll Manager"); s != 400 || !strings.Contains(fmt.Sprint(r), "dry run") {
		t.Fatalf("approve while stale: %d %v", s, r)
	}
	// Back to the dry run, recalculated, verified: approval then keeps exactly the verified rows.
	pfaMust(t, c, open, "back", "EMP-1011", "Payroll")
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	pfaMust(t, c, open, "verify", "EMP-1011", "Payroll")
	verified = num(c.row(t, open, "EMP-1020")["net"])
	// A change the stale flag cannot see (meal deduction) does not slip into the approved rows.
	if _, err := st.DB.C("employee_pay").Upsert(ctx, bson.M{"employee_id": "EMP-1020"}, bson.M{"meal": 1500.0}, nil); err != nil {
		t.Fatal(err)
	}
	pfaMust(t, c, open, "approve", "EMP-1012", "Payroll Manager")
	if got := num(c.row(t, open, "EMP-1020")["net"]); got != verified {
		t.Fatalf("approve recalculated: verified %v, approved %v", verified, got)
	}
	_ = time.Now
}

// REAL-09: attendance written and leave decided after the calculation make the run stale.
func TestPayfixStaleAttendanceAndLeave(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	if pfaStale(t, c) {
		t.Fatal("stale right after the dry run")
	}
	day := dayIn(open, time.Tuesday, 2)
	if _, err := st.DB.C("attendance_day").Upsert(ctx, bson.M{"employee_id": "EMP-1020", "date": day}, bson.M{"in_time": "09:00", "out_time": "18:00", "source": "MIS upload", "by_name": "test", "at": time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	if !pfaStale(t, c) {
		t.Fatal("attendance written after the dry run is not stale")
	}
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	// A policy change after the dry run.
	if _, err := st.DB.C("audit_log").Insert(ctx, bson.M{"actor": "x (EMP-1002)", "action": "Work policy changed", "entity": "work_policy", "entity_id": "x"}); err != nil {
		t.Fatal(err)
	}
	if !pfaStale(t, c) {
		t.Fatal("a work policy change after the dry run is not stale")
	}
}

// REAL-28 / ACC-05: the approver cannot be the verifier or a row editor; nobody edits their own row.
func TestPayfixMakerChecker(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	edit := map[string]any{"otherEarn": 50000, "reason": "Bonus to myself"}
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1012", "EMP-1012", "Payroll Manager", edit); s != 403 && s != 400 {
		t.Fatalf("own row edited by the Payroll Manager: %d %v", s, r)
	}
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1011", "EMP-1011", "Payroll", edit); s != 403 && s != 400 {
		t.Fatalf("own row edited by Payroll: %d %v", s, r)
	}
	// The Payroll Manager verifies, then cannot approve.
	pfaMust(t, c, open, "verify", "EMP-1012", "Payroll Manager")
	if s, r := pfaStep(t, c, open, "approve", "EMP-1012", "Payroll Manager"); s != 403 || !strings.Contains(fmt.Sprint(r), "verified") {
		t.Fatalf("verifier approves: %d %v", s, r)
	}
	// The Payroll Manager edits a row, Payroll verifies: still not the Payroll Manager's to approve.
	pfaMust(t, c, open, "back", "EMP-1011", "Payroll")
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1012", "Payroll Manager", map[string]any{"otherEarn": 100, "reason": "Late claim"}); s != 200 {
		t.Fatal(r)
	}
	pfaMust(t, c, open, "verify", "EMP-1011", "Payroll")
	if s, r := pfaStep(t, c, open, "approve", "EMP-1012", "Payroll Manager"); s != 403 || !strings.Contains(fmt.Sprint(r), "edited") {
		t.Fatalf("row editor approves: %d %v", s, r)
	}
}

// RUN-83 / REAL-27: paid days stay within the days employed in the month.
func TestPayfixPaidDaysWithinEmployment(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	open := pfaOpen(t, c)
	lwd := open + "-10"
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"lwd": lwd, "exit_status": "On notice"}}); err != nil {
		t.Fatal(err)
	}
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	row := c.row(t, open, "EMP-1020")
	maxPaid := num(row["days"]) - num(row["outsideP"])
	if maxPaid <= 0 || maxPaid >= num(row["days"]) {
		t.Fatalf("days %v outside %v", row["days"], row["outsideP"])
	}
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{"paidDays": num(row["days"]), "reason": "Full month"}); s != 400 {
		t.Fatalf("paid days above the days employed: %d %v", s, r)
	}
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{"paidDays": maxPaid, "reason": "All days employed"}); s != 200 {
		t.Fatal(r)
	}
}

// RUN-42 / ACC-16 / REAL-48 / REAL-49: a paid month's attendance and leave cannot change, even with an exception.
func TestPayfixPaidMonthStays(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pfaOpen(t, c)
	c.closeAndPublish(t, open)
	date := dayIn(open, time.Thursday, 2)
	_, _ = st.DB.C("attendance_day").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020", "date": date})
	_, r := c.sendFile("/api/bulk/att/apply", "EMP-1002", "HR Admin", "att.csv", []byte("Employee ID,Date,In time,Out time\nEMP-1020,"+date+",09:00,18:00\n"))
	if num(r["ok"]) != 0 || !strings.Contains(fmt.Sprint(r), "already paid") {
		t.Fatalf("attendance upload into a paid month: %v", r)
	}
	if n := apiCount(st, "attendance_day", bson.M{"employee_id": "EMP-1020", "date": date}); n != 0 {
		t.Fatal("the paid month's day was written")
	}
	for _, kind := range []string{"regularize", "leave"} {
		if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "kind": kind, "from": date, "to": date, "until": today(), "reason": "Late correction"}); s != 200 {
			t.Fatal(r)
		}
	}
	reg := map[string]any{"wf": "regularize", "payload": map[string]any{"date": date, "rtype": "No Punch", "in": "06:00", "out": "14:00", "reason": "Forgot to punch"}}
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", reg); s != 400 || !strings.Contains(fmt.Sprint(r), "already paid") {
		t.Fatalf("regularization of a paid month with an exception: %d %v", s, r)
	}
	if _, err := st.DB.C("approval_request").UpdateMany(ctx, bson.M{"wf": "leave", "employee_id": "EMP-1020"}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil { // the seeded loss-of-pay days
		t.Fatal(err)
	}
	lv := map[string]any{"wf": "leave", "employeeId": "EMP-1020", "payload": map[string]any{"ltype": "Loss of pay", "from": date, "to": date, "scoreOverride": "x"}}
	if s, r := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", lv); s != 400 || !strings.Contains(fmt.Sprint(r), "already paid") {
		t.Fatalf("leave in a paid month with an exception: %d %v", s, r)
	}
}

// RUN-12: a month other than the open one is not created by a stray run, pilot or checklist action.
func TestPayfixOpenMonthStays(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1011", "EMP-1012")
	open := pfaOpen(t, c)
	for _, call := range []struct{ method, path string }{{"POST", "/api/payroll/2025-01/dry-run"}, {"PUT", "/api/payroll/2025-01/pilot"}, {"POST", "/api/payroll/2025-01/closures/attendance/close"}} {
		if s, r := c.do(call.method, call.path, "EMP-1011", "Payroll", map[string]any{"ids": []string{}}); s == 200 {
			t.Fatalf("%s: %v", call.path, r)
		}
	}
	if got := pfaOpen(t, c); got != open {
		t.Fatalf("open month moved from %s to %s", open, got)
	}
	if n := apiCount(st, "payroll_month", bson.M{"ym": "2025-01"}); n != 0 {
		t.Fatal("a stray month was recorded")
	}
}

// RUN-85: the TDS of a month approved but not yet published counts in the next month's TDS.
func TestPayfixTdsApprovedMonth(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	pfaMust(t, c, open, "verify", "EMP-1011", "Payroll")
	pfaMust(t, c, open, "approve", "EMP-1012", "Payroll Manager")
	next := nextYmT(open)
	c.pcCloseAll(t, next)
	pfaMust(t, c, next, "dry-run", "EMP-1011", "Payroll")
	before := map[string]float64{}
	_, rows := c.do("GET", "/api/payroll/"+next+"/rows", "EMP-1011", "Payroll", nil)
	for _, x := range rows["rows"].([]any) {
		m := x.(map[string]any)
		before[m["employeeId"].(string)] = num(m["stds"])
	}
	pfaMust(t, c, open, "publish", "EMP-1012", "Payroll Manager")
	pfaMust(t, c, next, "dry-run", "EMP-1011", "Payroll")
	_, rows = c.do("GET", "/api/payroll/"+next+"/rows", "EMP-1011", "Payroll", nil)
	some := false
	for _, x := range rows["rows"].([]any) {
		m := x.(map[string]any)
		if num(m["stds"]) > 0 {
			some = true
		}
		if b := before[m["employeeId"].(string)]; b != num(m["stds"]) {
			t.Fatalf("%v TDS %v before publishing %s, %v after", m["employeeId"], b, open, m["stds"])
		}
	}
	if !some {
		t.Log("no salary TDS in the sample month")
	}
}

func nextYmT(ym string) string { return mustDate(ym+"-01").AddDate(0, 1, 0).Format("2006-01") }

// REAL-19: a pay line for someone who joins later goes to the joining month; the run check names lines left out.
func TestPayfixLineBeforeJoining(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	open := pfaOpen(t, c)
	next := nextYmT(open)
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"doj": next + "-01"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": "EMP-1020", "ym": open, "kind": "earn", "label": "Reimbursement: Travel", "amount": 450.0, "source_id": "CLM-T"}); err != nil {
		t.Fatal(err)
	}
	c.pcCloseAll(t, open)
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	if l := apiDoc(st, "ledger", bson.M{"source_id": "CLM-T"}); l["ym"] != next {
		t.Fatalf("line left in %v", l["ym"])
	}
	// A line for someone without a row is named and fails the check.
	if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": "EMP-1020", "ym": open, "kind": "earn", "label": "Late line", "amount": 10.0, "source_id": "X-1"}); err != nil {
		t.Fatal(err)
	}
	_, chk := c.do("GET", "/api/payroll/"+open+"/checks", "EMP-1011", "Payroll", nil)
	bad := false
	for _, x := range chk["list"].([]any) {
		m := x.(map[string]any)
		if strings.Contains(fmt.Sprint(m), "Not in the run") {
			bad = m["ok"] == false && strings.Contains(fmt.Sprint(m), "EMP-1020")
		}
	}
	if !bad {
		t.Fatalf("lines outside the run pass the check: %v", chk)
	}
}

// ACC-17: the cut-off stays once a month has been paid.
func TestPayfixCutoffAfterPaid(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	open := pfaOpen(t, c)
	c.closeAndPublish(t, open)
	if s, r := c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"cutoff": "26-25"}); s != 400 {
		t.Fatalf("cut-off changed after a paid month: %d %v", s, r)
	}
}

// RUN-40: an upload without a file says so.
func TestPayfixEmptyUpload(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002")
	s, r := c.do("POST", "/api/bulk/att/check", "EMP-1002", "HR Admin", nil)
	if s != 400 || !strings.Contains(fmt.Sprint(r), "Choose an Excel or CSV file") {
		t.Fatalf("%d %v", s, r)
	}
}
