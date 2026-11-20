package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Fixes from the payroll test (fork D): attendance, leave, closure, claims, items and approval routing.

func pdOpenYm(t *testing.T, c *client) string {
	t.Helper()
	s, x := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatalf("payroll state: %v", x)
	}
	return x["openYm"].(string)
}

func pdChecklist(t *testing.T, c *client, ym string) map[string]any {
	t.Helper()
	s, x := c.do("GET", "/api/payroll/"+ym+"/checklist", "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatalf("checklist %s: %v", ym, x)
	}
	return x
}

// RUN-16 / REAL-16 / EXT-25: Payroll marks unexplained absences as loss of pay at the cut-off, including a leaver's
// days up to the last working day; the closure items clear and the day counts as LOP.
func TestPayfixMarkLop(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1011", "EMP-1002", "EMP-1020"} {
		c.signIn(e)
	}
	ym := pdOpenYm(t, c)
	x := pdChecklist(t, c, ym)
	cl := pcClosure(x, "attendance")
	var pick map[string]any
	for _, v := range cl["items"].([]any) {
		if m := v.(map[string]any); m["canLop"] == true {
			pick = m
			break
		}
	}
	if pick == nil {
		t.Fatalf("no absence offered for marking as LOP in %s: %v", ym, cl["items"])
	}
	emp, day, ref := pick["employeeId"].(string), pick["date"].(string), pick["ref"].(string)
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/mark-lop", "EMP-1011", "Payroll", map[string]any{"refs": []string{ref}}); s != 400 {
		t.Fatalf("mark-lop without a reason: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/mark-lop", "EMP-1011", "Payroll", map[string]any{"refs": []string{ref}, "reason": "Absent, no response by cut-off"}); s != 200 || r["marked"] != float64(1) {
		t.Fatalf("mark-lop: %d %v", s, r)
	}
	x = pdChecklist(t, c, ym)
	if pcItem(pcClosure(x, "attendance"), ref) != nil {
		t.Fatalf("item %s still pending after mark-lop", ref)
	}
	if apiCount(st, "approval_request", bson.M{"wf": "leave", "employee_id": emp, "status": "Approved", "payload.ltype": "Loss of pay", "payload.cov": day}) != 1 {
		t.Fatal("no loss-of-pay record for the marked day")
	}
	if apiCount(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Marked 1 absence as loss of pay"}}) != 1 {
		t.Fatal("mark-lop not audited")
	}
	// The day shows as loss of pay on the employee's attendance month.
	s, a := c.do("GET", "/api/attendance/month/"+emp+"?ym="+day[:7], "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("attendance month: %d %v", s, a)
	}
	found := false
	for _, v := range a["month"].(map[string]any)["days"].([]any) {
		if d := v.(map[string]any); d["date"] == day {
			found = d["code"] == "LOP" && d["lop"] == float64(1)
		}
	}
	if !found {
		t.Fatalf("attendance for %s on %s is not LOP", emp, day)
	}

	// A leaver (inactive, LWD inside the cycle) whose absences cannot take a leave request.
	from := x["from"].(string)
	lwd := from[:8] + "05"
	var leaver string
	for _, v := range pcClosure(x, "attendance")["items"].([]any) {
		if m := v.(map[string]any); m["canLop"] == true && m["employeeId"] != emp {
			leaver = m["employeeId"].(string)
			break
		}
	}
	if leaver == "" {
		t.Skip("no second employee with absences")
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": leaver}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": lwd}}); err != nil {
		t.Fatal(err)
	}
	x = pdChecklist(t, c, ym)
	refs := []string{}
	for _, k := range []string{"attendance", "leave"} {
		for _, v := range pcClosure(x, k)["items"].([]any) {
			if m := v.(map[string]any); m["employeeId"] == leaver {
				if m["canLop"] != true {
					t.Fatalf("leaver's absence not markable: %v", m)
				}
				if m["date"].(string) > lwd {
					t.Fatalf("leaver listed after LWD: %v", m)
				}
				refs = append(refs, m["ref"].(string))
			}
		}
	}
	if len(refs) == 0 {
		t.Fatal("leaver has no absences listed")
	}
	if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/mark-lop", "EMP-1011", "Payroll", map[string]any{"refs": refs, "reason": "Left without notice"}); s != 200 {
		t.Fatalf("mark-lop leaver: %d %v", s, r)
	}
	x = pdChecklist(t, c, ym)
	for _, k := range []string{"attendance", "leave"} {
		for _, v := range pcClosure(x, k)["items"].([]any) {
			if m := v.(map[string]any); m["employeeId"] == leaver {
				t.Fatalf("leaver still pending: %v", m)
			}
		}
	}
	// Employees cannot use it.
	if s, _ := c.do("POST", "/api/payroll/"+ym+"/closures/attendance/mark-lop", "EMP-1020", "Employee", map[string]any{"all": true, "reason": "Trying it out"}); s != 403 {
		t.Fatalf("employee mark-lop: %d", s)
	}
}

// REAL-07 / REAL-11 / REAL-52: the checklist warns about a work policy without a holiday calendar, a leave plan
// without Loss of pay for an employment type in the run, and blank company statutory codes.
func TestPayfixSetupWarnings(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1011")
	ym := pdOpenYm(t, c)
	if _, err := st.DB.C("work_policy").UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"calendar_id": nil, "cal_map": bson.M{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"tan": ""}}); err != nil {
		t.Fatal(err)
	}
	x := pdChecklist(t, c, ym)
	w := fmt.Sprint(x["warnings"])
	for _, want := range []string{"has no holiday calendar", "TAN"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warnings miss %q: %s", want, w)
		}
	}
	// Loss of pay switched off in every plan: every employment type in the run is named.
	plans, _ := st.DB.C("leave_plan").Find(ctx, nil)
	for _, p := range plans {
		if _, err := st.DB.C("leave_plan").Update(ctx, bson.M{"_id": p["id"]}, bson.M{"$set": bson.M{"types.Loss of pay.on": false}}); err != nil {
			t.Fatal(err)
		}
	}
	x = pdChecklist(t, c, ym)
	if w := fmt.Sprint(x["warnings"]); !strings.Contains(w, "no Loss of pay for Permanent employees") {
		t.Fatalf("no leave-plan warning: %s", w)
	}
}

// RUN-15: an approved half-day Loss of pay leave is 0.5 LOP on the employee's month, not 1.
func TestPayfixHalfDayLop(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	ym := today()[:7]
	day := ym + "-02"
	if day >= today() {
		ym = prevYm(ym)
		day = ym + "-02"
	}
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 0.5}, map[string]any{"ltype": "Loss of pay", "from": day, "to": day, "half": true})
	// The other half worked (14:00 to 18:00).
	if _, err := st.DB.C("attendance_day").Insert(context.Background(), bson.M{"employee_id": "EMP-1020", "date": day, "in_time": "14:00", "out_time": "18:00", "source": "Punch machine", "by_name": "Test seed"}); err != nil {
		t.Fatal(err)
	}
	s, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("attendance month: %d %v", s, a)
	}
	for _, v := range a["month"].(map[string]any)["days"].([]any) {
		if d := v.(map[string]any); d["date"] == day {
			if d["code"] != "LOP" || d["lop"] != 0.5 || !strings.Contains(d["status"].(string), "half day") {
				t.Fatalf("half-day LOP on %s: %v", day, d)
			}
			return
		}
	}
	t.Fatalf("%s not in the month", day)
}

func prevYm(ym string) string {
	t, _ := time.Parse("2006-01", ym)
	return t.AddDate(0, -1, 0).Format("2006-01")
}

// RUN-13 / REAL-12: Payroll opens exceptions (spec 10.2); an employee cannot.
func TestPayfixPayrollOpensExceptions(t *testing.T) {
	c, _, _ := setupFull(t)
	c.signIn("EMP-1011")
	c.signIn("EMP-1020")
	d := today()
	if s, r := c.do("POST", "/api/exceptions", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1021", "kind": "leave", "from": d, "to": d, "until": d, "reason": "Back-dated leave"}); s != 200 {
		t.Fatalf("Payroll exception: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/exceptions", "EMP-1020", "Employee", map[string]any{"employeeId": "EMP-1021", "kind": "leave", "from": d, "to": d, "until": d, "reason": "Back-dated leave"}); s != 403 {
		t.Fatalf("employee exception: %d", s)
	}
}

// REAL-61: the person named for the waiting stage decides it from any of their views (a manager covering for one who
// left is named as Line Manager without holding that role); nobody else gains anything.
func TestPayfixNamedApproverAnyView(t *testing.T) {
	c, _, _ := setupFull(t)
	for _, e := range []string{"EMP-1020", "EMP-1009", "EMP-1008"} {
		c.signIn(e)
	}
	d := time.Now()
	for d = d.AddDate(0, 0, 1); d.Weekday() != time.Monday; d = d.AddDate(0, 0, 1) {
	}
	day := d.AddDate(0, 0, 1).Format("2006-01-02")
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]string{"ltype": "Casual leave", "from": day, "to": day}})
	if s != 200 {
		t.Fatal(r)
	}
	id := r["id"].(string)
	if s, _ := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1008", "Employee", nil); s != 403 {
		t.Fatal("the Property Manager cannot decide the Line Manager stage")
	}
	if s, r := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1009", "Employee", nil); s != 200 || r["next"] != "Property Manager" {
		t.Fatalf("named line manager in another view: %d %v", s, r)
	}
}

// REAL-15: HR Admin raises leave for someone whose line manager is the CHRO and whose chain ends at HR Admin. After
// the line manager approves, HR Admin (the raiser) and CHRO (already asked) cannot decide it: the request completes
// instead of waiting for ever.
func TestPayfixRaiserOnlyLastStage(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1001")
	sub := apiDoc(st, "employee", bson.M{"manager_id": "EMP-1001", "_id": bson.M{"$ne": "EMP-1002"}, "status": "Active"})
	if sub == nil {
		t.Skip("nobody reports to the CHRO")
	}
	emp := sub["id"].(string)
	day := today()
	if _, err := st.DB.C("approval_request").Insert(ctx, bson.M{"_id": "LV-99001", "wf": "leave", "employee_id": emp, "raised_by": "EMP-1002", "flow": "Corporate",
		"facts": bson.M{"wf": "leave", "days": 1.0}, "payload": bson.M{"ltype": "Loss of pay", "from": day, "to": day}, "chain": []string{"Line Manager", "HR Admin"}, "step": 0, "status": "Pending"}); err != nil {
		t.Fatal(err)
	}
	s, r := c.do("POST", "/api/approvals/LV-99001/approve", "EMP-1001", "CHRO", nil)
	if s != 200 || r["status"] != "Approved" {
		t.Fatalf("approve: %d %v (history %v)", s, r, apiDoc(st, "approval_request", bson.M{"_id": "LV-99001"}))
	}
}

// ITM-25: an arrear is raised by Payroll only, not by an HRBP. ITM-32: the same bill cannot go on a second claim.
func TestPayfixArrearRightAndClaimBill(t *testing.T) {
	c, _, _ := setupFull(t)
	for _, e := range []string{"EMP-1004", "EMP-1011", "EMP-1020"} {
		c.signIn(e)
	}
	open := pdOpenYm(t, c)
	arr := map[string]any{"wf": "p_arrear", "employeeId": "EMP-1044", "payload": map[string]any{"amount": 1200, "forYm": prevYm(open), "arrearType": "Missed payment", "slipText": "Shift allowance"}}
	if s, r := c.do("POST", "/api/approvals", "EMP-1004", "HRBP", arr); s != 403 {
		t.Fatalf("HRBP raised an arrear: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", arr); s != 200 {
		t.Fatalf("Payroll arrear: %d %v", s, r)
	}
	_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Bill")
	claim := map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 800, "claimType": "Mobile and internet", "billDate": today(), "doc": doc["id"]}}
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", claim); s != 200 {
		t.Fatalf("first claim: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", claim); s != 400 || !strings.Contains(fmt.Sprint(r), "already on claim") {
		t.Fatalf("second claim with the same bill: %d %v", s, r)
	}
}

// ITM-54: a lost-asset recovery raised after the Deduction closure is closed goes into the next month.
func TestPayfixAssetRecoveryFollowsClosure(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1011")
	open := pdOpenYm(t, c)
	if _, err := st.DB.C("payroll_closure").Upsert(ctx, bson.M{"ym": open, "closure": "deduction"}, bson.M{"closed_at": time.Now(), "closed_by": "Test"}, nil); err != nil {
		t.Fatal(err)
	}
	A := func(method, path string, body any) map[string]any {
		t.Helper()
		s, r := c.do(method, path, "EMP-1002", "HR Admin", body)
		if s != 200 {
			t.Fatalf("%s %s: %d %v", method, path, s, r)
		}
		return r
	}
	var laptop float64
	for _, i := range A("GET", "/api/inventory", nil)["items"].([]any) {
		if m := i.(map[string]any); m["name"] == "Laptop" {
			laptop = m["id"].(float64)
		}
	}
	A("POST", "/api/inventory/stock/add", map[string]any{"to": "central", "lines": []any{map[string]any{"itemId": laptop, "serials": []string{"LP-54"}}}})
	aid, _ := apiDoc(st, "asset", bson.M{"serial": "LP-54"})["id"].(int64)
	A("POST", "/api/inventory/issue", map[string]any{"store": "central", "employeeId": "EMP-1020", "assetId": aid})
	A("POST", fmt.Sprintf("/api/inventory/assets/%d/lost", aid), map[string]any{"note": "Stolen from the bus", "recover": 1500})
	l := apiDoc(st, "ledger", bson.M{"source_id": fmt.Sprintf("AST-%d", aid)})
	if l == nil || l["ym"] == open {
		t.Fatalf("recovery after the Deduction closure went into %v (open %s)", l["ym"], open)
	}
}

// ITM-89: a back-dated gross before a later revision already on record changes only the months up to that revision,
// against the gross actually paid.
func TestPayfixRevisionArrearMonthByMonth(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1011")
	open := pdOpenYm(t, c)
	m4, m3 := addMonths(open, -4), addMonths(open, -3)
	if _, err := st.DB.C("gross_history").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020"}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []struct {
		ym string
		g  float64
	}{{"2020-01", 53400}, {m3, 50000}} {
		if _, err := st.DB.C("gross_history").Insert(ctx, bson.M{"employee_id": "EMP-1020", "from_month": h.ym, "gross": h.g, "reason": "Test", "created_by": "Test"}); err != nil {
			t.Fatal(err)
		}
	}
	csv := "Employee ID,New Gross,Effective From\nEMP-1020,52000," + m4 + "\n"
	s, r := c.sendFile("/api/bulk/gross/check", "EMP-1002", "HR Admin", "gross.csv", []byte(csv))
	if s != 200 {
		t.Fatalf("check: %d %v", s, r)
	}
	if txt := fmt.Sprint(r); !strings.Contains(txt, "recovery of ₹1,400 for 1 month") {
		t.Fatalf("expected a recovery of 1,400 for 1 month: %s", txt)
	}
}

func addMonths(ym string, n int) string {
	t, _ := time.Parse("2006-01", ym)
	return t.AddDate(0, n, 0).Format("2006-01")
}

// RUN-31: a variable pay % set from the open month never pays earlier months of the cycle at the new %. Since 14 Nov
// 2026 variable pay that starts then also starts its own cycle then (varpay.go).
func TestPayfixVariableFromEffectiveMonth(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1011")
	open := pdOpenYm(t, c)
	// Joined so that the open month ends a 3-month cycle.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"doj": addMonths(open, -14) + "-01"}}); err != nil {
		t.Fatal(err)
	}
	reason := apiDoc(st, "simple_master", bson.M{"kind": "change_reason", "used_for": "Salary", "status": "Active"})
	if reason == nil {
		t.Skip("no salary change reason")
	}
	csv := "Employee ID,Monthly Gross,Variable Pay %,Variable Payout Cycle,Meal Deduction,Accommodation Deduction,Salary Structure,Structure Effective From,Reason for change,Effective From\n" +
		"EMP-1020,,10,3,,,,," + reason["name"].(string) + "," + open + "-01\n"
	if s, r := c.sendFile("/api/bulk/sal/apply", "EMP-1002", "HR Admin", "sal.csv", []byte(csv)); s != 200 || num(r["ok"]) != 1 {
		t.Fatalf("bulk sal: %d %v", s, r)
	}
	c.pcCloseAll(t, open)
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("dry run: %d %v", s, r)
	}
	row := apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": "EMP-1020"})
	d := row["data"].(map[string]any)
	r := d["r"].(map[string]any)
	// Owner, 14 Nov 2026 (variable pay on each person's own cycle): variable pay that starts in the open month, where
	// there was none before, starts its cycle there. So the open month is month 1 of 3 and pays nothing yet (before,
	// the cycle ran from joining and paid this one month's accrual); the payout comes two months later, all at the new
	// %, so nothing earlier is paid at the new % either way.
	if vp, v := apiFloat(r["varPay"]), apiFloat(r["variable"]); vp != 0 || v <= 0 {
		t.Fatalf("variable payout %v with accrual %v (cycle %v): the cycle starts in the open month", vp, v, r["varCycle"])
	}
	if p := apiDoc(st, "employee_pay", bson.M{"employee_id": "EMP-1020"}); p["var_start"] != open {
		t.Fatalf("var_start %v, want %s", p["var_start"], open)
	}
}
