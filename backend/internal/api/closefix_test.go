package api_test

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Two actions on the payroll checklist (payroll_closure.go, 14 Nov 2026): a missing punch nobody explained is marked
// as a full or a half day of loss of pay (Attendance closure), and the salary of someone with no bank account on
// record is held for the month (Bank verification closure), released automatically once an account is on record.

// cfItems lists the Not raised items of a closure about one employee.
func cfItems(x map[string]any, key, emp string) []map[string]any {
	out := []map[string]any{}
	for _, v := range pcClosure(x, key)["items"].([]any) {
		if m := v.(map[string]any); m["employeeId"] == emp && m["group"] == "Not raised" {
			out = append(out, m)
		}
	}
	return out
}

// cfPunchAllBut gives emp full punches (the shift's times) on every attendance-closure day of month ym, and only an
// in punch on day, so day is the person's one item: a missing punch.
func cfPunchAllBut(t *testing.T, c *client, ym, emp, day string) {
	t.Helper()
	ctx := context.Background()
	in, out := "09:00", "18:00"
	if e := apiDoc(c.st, "employee", bson.M{"_id": emp}); e != nil {
		if sh := apiDoc(c.st, "shift", bson.M{"code": e["shift_code"]}); sh != nil {
			if a, b := strings.TrimSpace(asStr(sh["start_time"])), strings.TrimSpace(asStr(sh["end_time"])); len(a) >= 5 && len(b) >= 5 && b[:5] > a[:5] {
				in, out = a[:5], b[:5]
			}
		}
	}
	x := pdChecklist(t, c, ym)
	found := false
	for _, it := range cfItems(x, "attendance", emp) {
		d := it["date"].(string)
		doc := bson.M{"employee_id": emp, "date": d, "in_time": in, "out_time": out, "source": "Punch machine", "by_name": "Test seed"}
		if d == day {
			found = true
			doc = bson.M{"employee_id": emp, "date": d, "in_time": in, "source": "Punch machine", "by_name": "Test seed"}
		}
		if _, err := c.st.DB.C("attendance_day").Insert(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatalf("%s has no attendance item on %s", emp, day)
	}
	items := cfItems(pdChecklist(t, c, ym), "attendance", emp)
	if len(items) != 1 || items[0]["date"] != day || !strings.HasPrefix(items[0]["what"].(string), "Missing punch") {
		t.Fatalf("%s should have one missing punch on %s: %v", emp, day, items)
	}
	if items[0]["canLop"] != true || items[0]["lopHalf"] != true {
		t.Fatalf("a missing punch offers Mark as LOP with a full or half day: %v", items[0])
	}
}

func asStr(v any) string { s, _ := v.(string); return s }

// cfAbsentees are employees with absences in the Attendance closure, with those days, by employee ID.
func cfAbsentees(t *testing.T, x map[string]any, skip ...string) ([]string, map[string][]string) {
	t.Helper()
	days := map[string][]string{}
	for _, v := range pcClosure(x, "attendance")["items"].([]any) {
		m := v.(map[string]any)
		if m["group"] != "Not raised" || m["canLop"] != true || m["lopHalf"] == true {
			continue
		}
		emp := m["employeeId"].(string)
		skipped := false
		for _, s := range skip {
			skipped = skipped || s == emp
		}
		if !skipped {
			days[emp] = append(days[emp], m["date"].(string))
		}
	}
	ids := []string{}
	for id := range days {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, days
}

// cfDay is one day of an employee's attendance month.
func cfDay(t *testing.T, c *client, emp, day string) map[string]any {
	t.Helper()
	s, a := c.do("GET", "/api/attendance/month/"+emp+"?ym="+day[:7], "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("attendance month: %d %v", s, a)
	}
	for _, v := range a["month"].(map[string]any)["days"].([]any) {
		if d := v.(map[string]any); d["date"] == day {
			return d
		}
	}
	t.Fatalf("no %s in %s's month", day, emp)
	return nil
}

// A missing punch marked as a full day is 1 day of loss of pay in payroll; marked as a half day, 0.5 (stored as the
// leave module stores a half-day leave; the punched half is worked). Wrong roles and a closed closure are refused.
func TestCloseFixMissingPunchLop(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	ym := pdOpenYm(t, c)
	ids, days := cfAbsentees(t, pdChecklist(t, c, ym), "EMP-1011", "EMP-1012")
	if len(ids) < 2 {
		t.Fatalf("need two employees with absences in %s: %v", ym, ids)
	}
	full, half := ids[0], ids[1]
	dFull, dHalf := days[full][0], days[half][0]
	cfPunchAllBut(t, c, ym, full, dFull)
	cfPunchAllBut(t, c, ym, half, dHalf)
	refFull, refHalf := ym+":attendance:"+full+"/"+dFull, ym+":attendance:"+half+"/"+dHalf
	path := "/api/payroll/" + ym + "/closures/attendance/mark-lop"

	// Only Payroll's run rights mark days; a reason of 5 characters is needed.
	if s, _ := c.do("POST", path, "EMP-1020", "Employee", map[string]any{"refs": []string{refHalf}, "half": true, "reason": "No out punch"}); s != 403 {
		t.Fatalf("employee marking LOP: %d", s)
	}
	if s, _ := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"refs": []string{refHalf}, "half": true, "reason": "abc"}); s != 400 {
		t.Fatalf("short reason: %d", s)
	}

	if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"refs": []string{refFull}, "reason": "No out punch, no reply by the cut-off"}); s != 200 || r["marked"] != float64(1) || r["fullDays"] != float64(1) {
		t.Fatalf("mark full day: %d %v", s, r)
	}
	if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"refs": []string{refHalf}, "half": true, "reason": "Left at lunch, no out punch"}); s != 200 || r["halfDays"] != float64(1) || r["lopDays"] != 0.5 {
		t.Fatalf("mark half day: %d %v", s, r)
	}
	x := pdChecklist(t, c, ym)
	if len(cfItems(x, "attendance", full)) != 0 || len(cfItems(x, "attendance", half)) != 0 {
		t.Fatal("marked missing punches are still pending")
	}
	// Stored as the leave module stores leave: a full Loss of pay day, and a half-day leave (half, one date, 0.5 days).
	if apiCount(st, "approval_request", bson.M{"wf": "leave", "employee_id": full, "status": "Approved", "payload.ltype": "Loss of pay", "payload.cov": dFull, "facts.days": 1.0}) != 1 {
		t.Fatal("no full-day loss of pay for the missing punch")
	}
	h := apiDoc(st, "approval_request", bson.M{"wf": "leave", "employee_id": half, "status": "Approved", "payload.ltype": "Loss of pay", "payload.from": dHalf})
	if h == nil || apiFloat(h["facts"].(map[string]any)["days"]) != 0.5 {
		t.Fatalf("half day not stored as a half-day leave: %v", h)
	}
	if p := h["payload"].(map[string]any); p["half"] != true || p["to"] != dHalf {
		t.Fatalf("half-day payload: %v", p)
	}
	if apiCount(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Marked 1 missing punch as a half day as loss of pay"}}) != 1 {
		t.Fatal("half-day mark not audited")
	}
	// The employee's calendar.
	if d := cfDay(t, c, full, dFull); d["code"] != "LOP" || d["lop"] != float64(1) {
		t.Fatalf("full day on the calendar: %v", d)
	}
	if d := cfDay(t, c, half, dHalf); d["code"] != "LOP" || d["lop"] != 0.5 || !strings.Contains(d["status"].(string), "half day") {
		t.Fatalf("half day on the calendar: %v", d)
	}

	// Payroll: the marked day is the month's only loss of pay.
	c.pcCloseAll(t, ym)
	if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"all": true, "reason": "After the close"}); s != 400 || !strings.Contains(r["error"].(string), "is closed") {
		t.Fatalf("mark-lop on a closed closure: %d %v", s, r)
	}
	pfaMust(t, c, ym, "dry-run", "EMP-1011", "Payroll")
	if r := c.row(t, ym, full); num(r["lop"]) != 1 {
		t.Fatalf("full-day missing punch: payroll LOP %v, want 1", r["lop"])
	}
	if r := c.row(t, ym, half); num(r["lop"]) != 0.5 {
		t.Fatalf("half-day missing punch: payroll LOP %v, want 0.5", r["lop"])
	}
}

// Mark all with absences and missing punches: the one Full or Half choice applies to the missing punches, absences
// stay full days; the Attendance closure can then be closed.
func TestCloseFixMarkAllMix(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1020")
	ym := pdOpenYm(t, c)
	ids, days := cfAbsentees(t, pdChecklist(t, c, ym))
	emp := ""
	for _, id := range ids {
		if len(days[id]) >= 3 {
			emp = id
			break
		}
	}
	if emp == "" {
		t.Fatalf("nobody with three absences in %s", ym)
	}
	punch := days[emp][1]
	if _, err := st.DB.C("attendance_day").Insert(context.Background(), bson.M{"employee_id": emp, "date": punch, "in_time": "09:00", "source": "Punch machine", "by_name": "Test seed"}); err != nil {
		t.Fatal(err)
	}
	x := pdChecklist(t, c, ym)
	abs, punches := 0, 0
	for _, v := range pcClosure(x, "attendance")["items"].([]any) {
		if m := v.(map[string]any); m["group"] == "Not raised" && m["canLop"] == true {
			if m["lopHalf"] == true {
				punches++
			} else {
				abs++
			}
		}
	}
	if punches < 1 || abs < 2 {
		t.Fatalf("want absences and a missing punch: %d absences, %d missing punches", abs, punches)
	}
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/mark-lop", "EMP-1011", "Payroll", map[string]any{"all": true, "half": true, "reason": "Nothing raised by the cut-off"}); s != 200 ||
		r["absences"] != float64(abs) || r["halfDays"] != float64(punches) || r["fullDays"] != float64(0) {
		t.Fatalf("mark all: %d %v (want %d absences, %d half days)", s, r, abs, punches)
	}
	// The person's absences in one full-day request, the missing punch a half day of its own.
	others := []string{}
	for _, d := range days[emp] {
		if d != punch {
			others = append(others, d)
		}
	}
	fullReq := apiDoc(st, "approval_request", bson.M{"wf": "leave", "employee_id": emp, "payload.lopMark": true, "payload.half": bson.M{"$ne": true}})
	if fullReq == nil || apiFloat(fullReq["facts"].(map[string]any)["days"]) != float64(len(others)) {
		t.Fatalf("absences of %s: %v", emp, fullReq)
	}
	if apiCount(st, "approval_request", bson.M{"wf": "leave", "employee_id": emp, "payload.lopMark": true, "payload.half": true, "payload.from": punch, "facts.days": 0.5}) != 1 {
		t.Fatal("the missing punch is not a half day")
	}
	if d := cfDay(t, c, emp, punch); d["lop"] != 0.5 {
		t.Fatalf("missing punch day: %v", d)
	}
	if d := cfDay(t, c, emp, others[0]); d["lop"] != float64(1) || d["code"] != "LOP" {
		t.Fatalf("absence day: %v", d)
	}
	// Nothing left to mark; with waiting regularizations rejected at the cut-off the closure closes.
	x = pdChecklist(t, c, ym)
	for _, v := range pcClosure(x, "attendance")["items"].([]any) {
		if m := v.(map[string]any); m["group"] == "Not raised" {
			t.Fatalf("still not raised: %v", m)
		}
	}
	if pcClosure(x, "attendance")["waiting"].(float64) > 0 {
		c.do("POST", "/api/payroll/"+ym+"/closures/attendance/reject-waiting", "EMP-1011", "Payroll", map[string]any{"all": true})
	}
	if cl := pcClosure(pdChecklist(t, c, ym), "attendance"); cl["status"] != "Done" {
		t.Fatalf("attendance closure not clear: %v pending", cl["pending"])
	}
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/close", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("close attendance: %d %v", s, r)
	}
}

// Hold salary on "No bank account on record": the item moves to On hold, the closure closes, the salary is calculated
// but left out of the bank file; next month the person is listed again and held again, and once a bank change is
// verified the held month is paid in the open month as "Held salary released, <month>".
func TestCloseFixBankHold(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	const emp = "EMP-1020"
	if _, err := st.DB.C("employee_bank").DeleteMany(ctx, bson.M{"employee_id": emp}); err != nil {
		t.Fatal(err)
	}
	ym := pdOpenYm(t, c)
	ref := ym + ":bank:" + emp + "/bank"
	x := pdChecklist(t, c, ym)
	it := pcItem(pcClosure(x, "bank"), ref)
	if it == nil || it["canHold"] != true {
		t.Fatalf("no Hold salary on %s: %v", ref, it)
	}
	path := "/api/payroll/" + ym + "/closures/bank/hold"
	if s, _ := c.do("POST", path, emp, "Employee", map[string]any{"refs": []string{ref}, "reason": "No bank account on record"}); s != 403 {
		t.Fatalf("employee holding a salary: %d", s)
	}
	if s, _ := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"refs": []string{ref}, "reason": "none"}); s != 400 {
		t.Fatalf("short reason: %d", s)
	}
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/hold", "EMP-1011", "Payroll", map[string]any{"all": true, "reason": "No bank account on record"}); s != 400 {
		t.Fatalf("hold on the attendance closure: %d %v", s, r)
	}
	if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"refs": []string{ref}, "reason": "No bank account on record"}); s != 200 || r["held"] != float64(1) {
		t.Fatalf("hold: %d %v", s, r)
	}
	h := apiDoc(st, "salary_hold", bson.M{"employee_id": emp, "status": "Active"})
	if h == nil || h["from_ym"] != ym || h["to_ym"] != ym || h["source_id"] != "no-bank" || h["reason"] != "No bank account on record" {
		t.Fatalf("hold stored: %v", h)
	}
	x = pdChecklist(t, c, ym)
	bank := pcClosure(x, "bank")
	if pcItem(bank, ref) != nil {
		t.Fatal("held person still pending")
	}
	onHold := false
	for _, v := range bank["onHold"].([]any) {
		if m := v.(map[string]any); m["employeeId"] == emp && m["holdId"] == h["id"] {
			onHold = true
		}
	}
	if !onHold || bank["held"].(float64) < 1 {
		t.Fatalf("not under On hold: %v", bank["onHold"])
	}
	// Everyone else without an account: Hold all; the closure is then clear and closes, and is refused after.
	if bank["notRaised"].(float64) > 0 {
		if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"all": true, "reason": "No bank account on record"}); s != 200 {
			t.Fatalf("hold all: %d %v", s, r)
		}
	}
	c.pcClear(t, ym, "bank") // bank changes waiting are verified
	if cl := pcClosure(pdChecklist(t, c, ym), "bank"); cl["pending"].(float64) != 0 {
		t.Fatalf("bank closure still pending: %v", cl["items"])
	}
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/bank/close", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("close bank: %d %v", s, r)
	}
	if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"all": true, "reason": "No bank account on record"}); s != 400 || !strings.Contains(r["error"].(string), "is closed") {
		t.Fatalf("hold on a closed closure: %d %v", s, r)
	}
	// Salary holds lists it; My view shows it.
	_, hl := c.do("GET", "/api/holds", "EMP-1011", "Payroll", nil)
	listed := false
	for _, v := range hl["holds"].([]any) {
		if m := v.(map[string]any); m["id"] == h["id"] && m["status"] == "Active" && m["to_ym"] == ym {
			listed = true
		}
	}
	if !listed {
		t.Fatal("not on Salary holds")
	}
	_, my := c.do("GET", "/api/me/dashboard?tab=pay", emp, "Employee", nil)
	if !strings.Contains(strings.ToLower(fmt.Sprint(my["holds"])), "no bank account on record") {
		t.Fatalf("My view does not show the hold: %v", my["holds"])
	}

	// The run: calculated, held, left out of the bank file.
	c.pcCloseAll(t, ym)
	for _, step := range []struct{ step, emp, role string }{{"dry-run", "EMP-1011", "Payroll"}, {"verify", "EMP-1011", "Payroll"}, {"approve", "EMP-1012", "Payroll Manager"}} {
		pfaMust(t, c, ym, step.step, step.emp, step.role)
	}
	row := c.row(t, ym, emp)
	held := num(row["net"])
	if row["hold"] != true || !strings.Contains(asStr(row["holdWhy"]), "No bank account on record") || held <= 0 {
		t.Fatalf("row not held: %v %v %v", row["hold"], row["holdWhy"], row["net"])
	}
	_, raw := c.getRaw("/api/payroll/"+ym+"/bank-advice.xlsx", "EMP-1012", "Payroll Manager")
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := f.GetRows("Data")
	for _, r := range rows {
		if len(r) > 0 && r[0] == emp {
			t.Fatal("held salary is in the bank file")
		}
	}

	// Next month: still no account, so listed again and held again (the same hold, now through this month).
	next := pdOpenYm(t, c)
	if next == ym {
		t.Fatal("the month did not move on")
	}
	nref := next + ":bank:" + emp + "/bank"
	it = pcItem(pcClosure(pdChecklist(t, c, next), "bank"), nref)
	if it == nil || it["canHold"] != true || it["holdId"] != h["id"] {
		t.Fatalf("not listed again next month: %v", it)
	}
	if s, r := c.do("POST", "/api/payroll/"+next+"/closures/bank/hold", "EMP-1011", "Payroll", map[string]any{"refs": []string{nref}, "reason": "Still no bank account"}); s != 200 {
		t.Fatalf("hold again: %d %v", s, r)
	}
	if h2 := apiDoc(st, "salary_hold", bson.M{"_id": h["id"]}); h2["to_ym"] != next || h2["status"] != "Active" || apiCount(st, "salary_hold", bson.M{"employee_id": emp}) != 1 {
		t.Fatalf("hold not moved on: %v", h2)
	}
	if pcItem(pcClosure(pdChecklist(t, c, next), "bank"), nref) != nil {
		t.Fatal("held again, still pending")
	}

	// The account: the employee raises a bank change and Payroll verifies it. The held month is paid now.
	s, r := c.do("POST", "/api/approvals", emp, "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Bank", "bankName": "HDFC Bank", "accountNo": "50100012345678", "ifsc": "HDFC0001234"}})
	if s != 200 {
		t.Fatal(r)
	}
	c.approveAll(t, r["id"].(string))
	if h3 := apiDoc(st, "salary_hold", bson.M{"_id": h["id"]}); h3["status"] != "Released" {
		t.Fatalf("hold not released: %v", h3)
	}
	l := apiDoc(st, "ledger", bson.M{"employee_id": emp, "ym": next, "void": false, "label": "Held salary released, " + mustDate(ym+"-01").Format("January 2006")})
	if l == nil {
		t.Fatal("no Held salary released line in the open month")
	}
	if want := float64(int64(held*100+0.5)) / 100; apiFloat(l["amount"]) != want {
		t.Fatalf("released %v, held %v", l["amount"], held)
	}
	x = pdChecklist(t, c, next)
	if pcItem(pcClosure(x, "bank"), nref) != nil || len(pcClosure(x, "bank")["onHold"].([]any)) != 0 {
		t.Fatal("the account is on record: nothing left about it")
	}
	// The next run pays this month and the released one, not on hold.
	c.pcCloseAll(t, next)
	pfaMust(t, c, next, "dry-run", "EMP-1011", "Payroll")
	nrow := c.row(t, next, emp)
	paid := false
	for _, v := range nrow["add"].([]any) {
		paid = paid || strings.HasPrefix(v.(map[string]any)["label"].(string), "Held salary released")
	}
	if nrow["hold"] == true || !paid {
		t.Fatalf("next month: hold %v, release paid %v", nrow["hold"], paid)
	}
}

// An account added directly (an import or Bulk update, not a bank change) is enough: the next payroll run releases
// the hold.
func TestCloseFixBankHoldReleasedByRun(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	const emp = "EMP-1020"
	if _, err := st.DB.C("employee_bank").DeleteMany(ctx, bson.M{"employee_id": emp}); err != nil {
		t.Fatal(err)
	}
	ym := pdOpenYm(t, c)
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/bank/hold", "EMP-1002", "HR Admin", map[string]any{"refs": []string{ym + ":bank:" + emp + "/bank"}, "reason": "No bank account on record"}); s != 200 {
		t.Fatalf("hold as HR Admin: %d %v", s, r)
	}
	c.pcCloseAll(t, ym)
	for _, step := range []struct{ step, emp, role string }{{"dry-run", "EMP-1011", "Payroll"}, {"verify", "EMP-1011", "Payroll"}, {"approve", "EMP-1012", "Payroll Manager"}} {
		pfaMust(t, c, ym, step.step, step.emp, step.role)
	}
	next := pdOpenYm(t, c)
	if _, err := st.DB.C("employee_bank").Insert(ctx, bson.M{"employee_id": emp, "bank_name": "Test Bank", "account_no": "123456789012", "ifsc": "TEST0000001", "from_month": next, "created_by": "Test seed"}); err != nil {
		t.Fatal(err)
	}
	c.pcCloseAll(t, next)
	pfaMust(t, c, next, "dry-run", "EMP-1011", "Payroll")
	if n := apiCount(st, "salary_hold", bson.M{"employee_id": emp, "status": "Active"}); n != 0 {
		t.Fatal("hold still active after the run")
	}
	if apiDoc(st, "ledger", bson.M{"employee_id": emp, "ym": next, "void": false, "label": "Held salary released, " + mustDate(ym+"-01").Format("January 2006")}) == nil {
		t.Fatal("no release line")
	}
}
