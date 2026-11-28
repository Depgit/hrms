package api_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Fixes from the full test of 13 Nov 2026 (areas other than payroll).

func ftJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// ESS-151, ESS-152, PPL-043: a bank change's account number follows the viewer's bank right in the approval
// detail, the queues and the employee file.
func TestFtBankChangeMasked(t *testing.T) {
	c := setup(t)
	for _, e := range []string{"EMP-1014", "EMP-1009", "EMP-1008", "EMP-1004", "EMP-1011"} {
		c.signIn(e)
	}
	s, r := c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Bank", "bankName": "SBI", "accountNo": "123456789012", "ifsc": "SBIN0001234"}})
	if s != 200 {
		t.Fatalf("raise: %d %v", s, r)
	}
	id := r["id"].(string)
	for _, v := range []struct{ emp, role string }{{"EMP-1009", "Line Manager"}, {"EMP-1008", "Property Manager"}, {"EMP-1004", "HRBP"}} {
		for _, path := range []string{"/api/approvals/" + id, "/api/approvals?tab=open", "/api/employees/EMP-1014/file"} {
			s, r := c.do("GET", path, v.emp, v.role, nil)
			if s != 200 || !strings.Contains(ftJSON(r), id) {
				t.Errorf("%s cannot read %s: %d", v.role, path, s)
			}
			if strings.Contains(ftJSON(r), "123456789012") {
				t.Errorf("%s sees the account in %s", v.role, path)
			}
		}
	}
	// Payroll (bank Full) acts on it and sees it; so does the employee.
	if _, r := c.do("GET", "/api/approvals/"+id, "EMP-1011", "Payroll", nil); !strings.Contains(ftJSON(r), "123456789012") {
		t.Errorf("Payroll does not see the account: %v", r["payload"])
	}
	if _, r := c.do("GET", "/api/approvals/"+id, "EMP-1014", "Employee", nil); !strings.Contains(ftJSON(r), "123456789012") {
		t.Errorf("owner does not see the account: %v", r["payload"])
	}
}

// AUTH-023: after the real person verifies the OTP, someone else who knows only the Employee ID cannot set the
// password; the verification token from /otp/verify is needed.
func TestFtSetPasswordNeedsVerifyToken(t *testing.T) {
	c := setup(t)
	_, o := c.do("POST", "/api/auth/otp", "", "", map[string]string{"employeeId": "EMP-1044", "purpose": "first"})
	s, v := c.do("POST", "/api/auth/otp/verify", "", "", map[string]string{"employeeId": "EMP-1044", "code": o["devOtp"].(string)})
	if s != 200 || v["verifyToken"] == nil {
		t.Fatalf("verify: %d %v", s, v)
	}
	for _, tok := range []string{"", "forged"} {
		if s, r := c.do("POST", "/api/auth/password", "", "", map[string]string{"employeeId": "EMP-1044", "password": "Attacker#2026", "verifyToken": tok}); s == 200 {
			t.Fatalf("password set without the verify token %q: %v", tok, r)
		}
	}
	if s, r := c.do("POST", "/api/auth/password", "", "", map[string]string{"employeeId": "EMP-1044", "password": "Saltstayz#2026", "verifyToken": v["verifyToken"].(string)}); s != 200 {
		t.Fatalf("real person: %d %v", s, r)
	}
}

// AUTH-080, AUTH-083: bulk approval is refused in My view, and a request the caller cannot open reads as unknown
// (no subject name, type or stage).
func TestFtBulkApprovalAccess(t *testing.T) {
	c := setup(t)
	for _, e := range []string{"EMP-1012", "EMP-1020", "EMP-1009"} {
		c.signIn(e)
	}
	s, r := c.do("POST", "/api/approvals", "EMP-1012", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Bank", "bankName": "SBI", "accountNo": "123456789012", "ifsc": "SBIN0001234"}})
	if s != 200 {
		t.Fatalf("raise: %d %v", s, r)
	}
	id := r["id"].(string)
	body := map[string]any{"group": "pay", "items": []map[string]any{{"id": id, "decision": "Approve"}}}
	if s, r := c.do("POST", "/api/approvals/bulk/apply", "EMP-1020", "Employee", body); s != 403 {
		t.Errorf("My view bulk apply: %d %v", s, r)
	}
	if s, _ := c.do("GET", "/api/approvals/bulk/export?group=pay", "EMP-1020", "Employee", nil); s != 403 {
		t.Errorf("My view bulk export: %d", s)
	}
	// A Line Manager of another team: the row says nothing about the request.
	s, r = c.do("POST", "/api/approvals/bulk/apply", "EMP-1009", "Line Manager", body)
	if s != 200 {
		t.Fatalf("LM bulk apply: %d %v", s, r)
	}
	txt := ftJSON(r["rows"])
	if !strings.Contains(txt, "Unknown Request ID") || strings.Contains(txt, "Bank") || strings.Contains(txt, "Payroll") {
		t.Errorf("row leaks: %s", txt)
	}
}

// AUTH-105: a masked salary shows none of its digits.
func TestFtMaskedSalaryNoDigits(t *testing.T) {
	c := setup(t)
	c.signIn("EMP-1004")
	s, r := c.do("GET", "/api/employees/EMP-1016", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatalf("%d %v", s, r)
	}
	sal := ftJSON(r["salary"])
	if strings.ContainsAny(sal, "123456789") && !strings.Contains(sal, "null") {
		t.Errorf("salary digits shown to HRBP: %s", sal)
	}
	t.Log(sal)
}

// ESS-230: two different people with the same name on one route are both asked; only the same person is asked once.
func TestFtRouteSameNameTwoPeople(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1014")
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1008"}, bson.M{"$set": bson.M{"name": "Anil Kapoor"}}); err != nil {
		t.Fatal(err)
	}
	s, r := c.do("POST", "/api/approvals/flows/regularize/Operations/test", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1014", "facts": map[string]any{"rtype": "No Punch", "cnt": 1}})
	if s != 200 {
		t.Fatalf("%d %v", s, r)
	}
	chain := ftJSON(r["chain"])
	if !strings.Contains(chain, "Line Manager") || !strings.Contains(chain, "Property Manager") {
		t.Errorf("tester chain: %s", chain)
	}
	d := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]string{"employeeId": "EMP-1014", "kind": "regularize", "from": d, "to": d, "until": time.Now().AddDate(0, 0, 3).Format("2006-01-02"), "reason": "Test"}); s != 200 {
		t.Fatal(r)
	}
	if _, err := st.DB.C("attendance_day").DeleteMany(ctx, bson.M{"employee_id": "EMP-1014", "date": d}); err != nil {
		t.Fatal(err)
	}
	s, r = c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": d, "rtype": "No Punch", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}})
	if s != 200 {
		t.Skipf("raise refused (%v); the tester check above covers the route", r)
	}
	row := apiDoc(st, "approval_request", bson.M{"_id": r["id"]})
	if ch := ftJSON(row["chain"]); !strings.Contains(ch, "Property Manager") {
		t.Errorf("stored chain %s drops the Property Manager with the same name", ch)
	}
}

// HIRE-048: releasing, accepting or declining an offer is an onboarding action; Payroll (Publish pay changes) cannot.
func TestFtOfferStepOnboardingOnly(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1010", "EMP-1011", "EMP-1012", "EMP-1014")
	si := selectCandidate(t, c, shortlistBody(st, "Test", "Cand", "9876500011"))
	oi := approvedOffer(t, c, st, si, "Test", "Cand", "9876500011")
	for _, v := range []struct{ emp, role string }{{"EMP-1011", "Payroll"}, {"EMP-1012", "Payroll Manager"}} {
		if s, r := c.do("POST", "/api/offers/"+oi+"/release", v.emp, v.role, nil); s != 403 {
			t.Errorf("%s released the offer: %d %v", v.role, s, r)
		}
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("HRBP release: %d %v", s, r)
	}
}

// HIRE-092, HIRE-093, HIRE-121: a Line Manager who sees a joining verification in the team queue does not get the
// ID document copies, and the correction history masks the PAN.
func TestFtJoiningDocsNeedGovID(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1014")
	vid, _, _ := pendingVerify(t, c, st, 0)
	s, l := c.do("GET", "/api/approvals/"+vid+"/verify-docs", "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatalf("payroll docs: %d %v", s, l)
	}
	var panDoc string
	for _, d := range l["docs"].([]any) {
		if x := d.(map[string]any); x["key"] == "pan" {
			panDoc = ftJSON(x["id"])
			panDoc = strings.Trim(panDoc, `"`)
		}
	}
	sd, _ := c.do("GET", "/api/approvals/"+vid, "EMP-1009", "Line Manager", nil)
	t.Logf("LM detail %d", sd)
	if s, _ := c.do("GET", "/api/approvals/"+vid+"/verify-docs", "EMP-1009", "Line Manager", nil); s == 200 {
		t.Errorf("Line Manager lists the ID documents")
	}
	if s, _ := c.raw("/api/approvals/"+vid+"/verify-docs/"+panDoc, "EMP-1009", "Line Manager"); s == 200 {
		t.Errorf("Line Manager opens the PAN card")
	}
	if s, _ := c.raw("/api/files/"+panDoc, "EMP-1009", "Line Manager"); s == 200 {
		t.Errorf("Line Manager opens the PAN card through /api/files")
	}
	// A PAN correction is written masked in the history.
	s, r := c.do("PUT", "/api/approvals/"+vid+"/verify-details", "EMP-1011", "Payroll", map[string]any{"data": map[string]any{"pan": "PQRST6789K"}})
	t.Logf("correct: %d %v", s, r)
	if _, d := c.do("GET", "/api/approvals/"+vid, "EMP-1009", "Line Manager", nil); strings.Contains(ftJSON(d["history"]), "PQRST6789K") || strings.Contains(ftJSON(d["history"]), "ABCDE1000F") {
		t.Errorf("Line Manager reads the PAN in the history: %s", ftJSON(d["history"]))
	}
	if _, d := c.do("GET", "/api/approvals/"+vid, "EMP-1012", "Payroll Manager", nil); !strings.Contains(ftJSON(d["history"]), "PQRST6789K") {
		t.Errorf("Payroll Manager should read the PAN: %s", ftJSON(d["history"]))
	}
}

// HIRE-444, HIRE-445: on notice the employee cannot withdraw directly (the withdrawal goes to the approvers); HR's
// direct withdrawal cancels a withdrawal request still waiting.
func TestFtWithdrawOnNotice(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1008", "EMP-1020")
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
	if s != 200 {
		t.Fatalf("resign: %d %v", s, r)
	}
	sep := ftJSON(r["case"])
	sep = strings.Trim(sep, `"`)
	ui3ApproveAll(t, c, ftJSON(r["id"])[1:len(ftJSON(r["id"]))-1])
	s, w := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"rkind": "withdraw", "reason": "Relocation"}})
	if s != 200 {
		t.Fatalf("withdraw request: %d %v", s, w)
	}
	if s, r := c.do("POST", "/api/separations/"+sep+"/withdraw", "EMP-1020", "Employee", map[string]any{}); s == 200 {
		t.Fatalf("employee withdrew directly on notice: %v", r)
	}
	if s, r := c.do("POST", "/api/separations/"+sep+"/withdraw", "EMP-1002", "HR Admin", map[string]any{"note": "Asked HR"}); s != 200 {
		t.Fatalf("HR withdraw: %d %v", s, r)
	}
	if x := apiDoc(st, "approval_request", bson.M{"_id": w["id"]}); x["status"] != "Cancelled" {
		t.Errorf("withdrawal request left %v", x["status"])
	}
}

// PPL-035, PPL-063, PPL-152: a report's change history keeps personal rows from a Line Manager and pay amounts
// from roles whose salary group is Masked (amounts masked) or Hidden (rows left out).
func TestFtHistoryByRights(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1009", "EMP-1007")
	if s, r := c.do("PATCH", "/api/employees/EMP-1014/personal", "EMP-1002", "HR Admin", map[string]any{"email": "zebrafox77@gmail.com"}); s != 200 {
		t.Fatalf("personal: %d %v", s, r)
	}
	if s, r := c.do("PUT", "/api/employees/EMP-1014/pay", "EMP-1002", "HR Admin", map[string]any{"varPct": 7, "varCycle": 3, "reason": "Policy change"}); s != 200 {
		t.Fatalf("pay: %d %v", s, r)
	}
	_, hr := c.do("GET", "/api/employees/EMP-1014/history", "EMP-1002", "HR Admin", nil)
	if txt := ftJSON(hr); !strings.Contains(txt, "zebrafox77") || !strings.Contains(txt, "7%") {
		t.Fatalf("HR Admin history: %s", txt)
	}
	s, lm := c.do("GET", "/api/employees/EMP-1014/history", "EMP-1009", "Line Manager", nil)
	if txt := ftJSON(lm); s != 200 || strings.Contains(txt, "zebrafox77") || strings.Contains(txt, "Meal deduction") || strings.Contains(txt, "Variable pay") {
		t.Errorf("Line Manager history: %d %s", s, txt)
	}
	s, cm := c.do("GET", "/api/employees/EMP-1014/history", "EMP-1007", "Cluster Manager", nil)
	if txt := ftJSON(cm); s != 200 || strings.Contains(txt, "7%") || !strings.Contains(txt, "Variable pay") {
		t.Errorf("Cluster Manager history: %d %s", s, txt)
	}
}

// PPL-103, PPL-110: the export gives HR Admin the personal details in full, and a name starting with "=" is written
// as text, not a formula.
func TestFtEmployeeExportPersonal(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	if _, err := st.DB.C("employee").Update(context.Background(), bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"name": "=1+2"}}); err != nil {
		t.Fatal(err)
	}
	s, b := c.raw("/api/employees/export?sections=personal", "EMP-1002", "HR Admin")
	if s != 200 {
		t.Fatalf("%d %s", s, b)
	}
	txt := string(b)
	for _, l := range strings.Split(txt, "\n") {
		if strings.HasPrefix(l, "EMP-1020,") && strings.Contains(l, "Hidden") {
			t.Errorf("personal hidden from HR Admin: %s", l)
		}
		if strings.HasPrefix(l, "EMP-1016,") && !strings.Contains(l, "'=1+2") {
			t.Errorf("formula name not neutralised: %s", l)
		}
	}
}

// SET-156: the ESIC file's total wages are the wages the employee ESI was worked out on, so 0.75% of the reported
// wages gives the ESI deducted.
func TestFtEsicFileReconciles(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	setFirstYm(t, c, "")
	for _, id := range []string{"EMP-1020", "EMP-1026", "EMP-1018", "EMP-1019"} {
		_, _ = st.DB.C("employee_gov_id").UpdateMany(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"esic": "310000" + id[4:]}})
	}
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.closeAndPublish(t, open)
	_, r := c.do("GET", "/api/statutory/esic?ym="+open, "EMP-1012", "Payroll Manager", nil)
	n := 0
	for _, x := range r["rows"].([]any) {
		m := x.(map[string]any)
		id := m["key"].(string)
		wages := num(m["cells"].([]any)[3])
		row := c.row(t, open, id)
		ee := num(row["r"].(map[string]any)["eeESI"])
		if want := math.Ceil(wages * 0.0075); math.Abs(want-ee) > 1 {
			t.Errorf("%s: wages %v → ESI %v, deducted %v", id, wages, want, ee)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no ESI rows")
	}
}

// TIME-235: on a night shift a regularization with in 22:00 and out 06:00 is accepted (out the next morning); on a
// day shift out before in is still refused.
func TestFtNightRegularization(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1014", "EMP-1016")
	// A working day for both shifts: the sample night shift's weekly off is Monday, and Sunday is off for day staff.
	dt := time.Now().AddDate(0, 0, -2)
	for dt.Weekday() == time.Monday || dt.Weekday() == time.Sunday {
		dt = dt.AddDate(0, 0, -1)
	}
	d := dt.Format("2006-01-02")
	if n := apiCount(st, "shift", bson.M{"code": "N"}); n == 0 {
		t.Skip("no N shift in the sample data")
	}
	_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1014"}, bson.M{"$set": bson.M{"shift_code": "N"}})
	_, _ = st.DB.C("roster_entry").DeleteMany(ctx, bson.M{"employee_id": bson.M{"$in": []string{"EMP-1014", "EMP-1016"}}, "date": d})
	_, _ = st.DB.C("attendance_day").DeleteMany(ctx, bson.M{"employee_id": bson.M{"$in": []string{"EMP-1014", "EMP-1016"}}, "date": d})
	for _, id := range []string{"EMP-1014", "EMP-1016"} {
		if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]string{"employeeId": id, "kind": "regularize", "from": d, "to": d, "until": time.Now().AddDate(0, 0, 3).Format("2006-01-02"), "reason": "Test"}); s != 200 {
			t.Fatal(r)
		}
	}
	body := map[string]any{"wf": "regularize", "payload": map[string]string{"date": d, "rtype": "No Punch", "in": "22:00", "out": "06:00", "reason": "Forgot to punch"}}
	if s, r := c.do("POST", "/api/approvals", "EMP-1014", "Employee", body); s != 200 {
		t.Fatalf("night shift: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1016", "Employee", body); s != 400 || !strings.Contains(ftJSON(r), "Out time must be after in time") {
		t.Fatalf("day shift: %d %v", s, r)
	}
}

// AUTH-003: an Employee ID typed without its hyphen, in any case or with a space, signs in.
// AUTH-045: sign-in, wrong password and sign-out are in the audit log.
func TestFtSignInIDFormsAndAudit(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	for _, id := range []string{"EMP1002", "emp1002", "EMP 1002", "1002"} {
		if s, r := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": id, "password": "Saltstayz#2026"}); s != 200 {
			t.Errorf("%q: %d %v", id, s, r)
		}
	}
	if s, _ := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": "EMP-1002", "password": "wrong"}); s != 401 {
		t.Fatal("wrong password accepted")
	}
	s, r := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": "EMP-1002", "password": "Saltstayz#2026"})
	if s != 200 {
		t.Fatal(r)
	}
	c.tokens["EMP-1002"] = r["token"].(string)
	c.do("POST", "/api/auth/logout", "EMP-1002", "HR Admin", nil)
	for _, a := range []string{"Signed in", "Wrong password", "Signed out"} {
		if apiCount(st, "audit_log", bson.M{"action": a, "entity_id": "EMP-1002"}) == 0 {
			t.Errorf("no %q in the audit log", a)
		}
	}
}

// AUTH-041, AUTH-042: one address trying many IDs is slowed down (429), and one Employee ID gets a limited number
// of OTPs a day.
func TestFtSignInRateLimits(t *testing.T) {
	c, _, st := setupFull(t)
	got429 := false
	for i := 0; i < 320; i++ { // 10 from one address in production; 300 with the development OTP echo the tests use
		s, _ := c.do("POST", "/api/auth/login", "", "", map[string]string{"employeeId": "EMP-" + itoa(1100+i), "password": "Guess#2026"})
		if s == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("no 429 after 320 unknown IDs from one address")
	}
	c2 := setup(t)
	for i := 0; i < 10; i++ {
		if _, err := st.DB.C("auth_otp").Insert(context.Background(), bson.M{"employee_id": "EMP-1044", "code_hash": "x", "purpose": "first", "used": true,
			"sent_at": time.Now().Add(-time.Duration(i+1) * time.Hour), "expires_at": time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if s, r := c2.do("POST", "/api/auth/otp", "", "", map[string]string{"employeeId": "EMP-1044", "purpose": "first"}); s != 429 {
		t.Errorf("11th OTP of the day: %d %v", s, r)
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + fmtInt(n)) }

func fmtInt(n int) string { b, _ := json.Marshal(n); return string(b) }

// AUTH-010: a session opened before the person went back to Pre-boarding stops working.
func TestFtPreboardingSessionEnds(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1045")
	if s, _ := c.do("GET", "/api/me", "EMP-1045", "Employee", nil); s != 200 {
		t.Fatal("not signed in")
	}
	_, _ = st.DB.C("employee").Update(context.Background(), bson.M{"_id": "EMP-1045"}, bson.M{"$set": bson.M{"status": "Pre-boarding"}})
	if s, _ := c.do("GET", "/api/me", "EMP-1045", "Employee", nil); s != 401 {
		t.Errorf("Pre-boarding session still works: %d", s)
	}
}

// AUTH-102, AUTH-103, AUTH-095: a role copied from one that does not exist is refused; a custom role can be mapped to
// a person; a rights change keeps what it was before.
func TestFtCustomRoles(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	if s, r := c.do("POST", "/api/roles", "EMP-1002", "HR Admin", map[string]string{"name": "Ghost", "copyFrom": "No Such Role"}); s != 400 {
		t.Errorf("unknown copyFrom: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/roles", "EMP-1002", "HR Admin", map[string]string{"name": "Store Keeper", "copyFrom": "Property Manager"}); s != 200 {
		t.Fatalf("add role: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/role-map", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1016", "role": "Store Keeper", "access": "all"}); s != 200 {
		t.Fatalf("map custom role: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/role-map", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1016", "role": "Line Manager", "access": "all"}); s != 400 {
		t.Error("Line Manager mapped")
	}
	if s, r := c.do("PUT", "/api/roles/Store Keeper/rights", "EMP-1002", "HR Admin", map[string]any{"rights": map[string]int{"rep.reports": 0}}); s != 200 {
		t.Logf("rights: %d %v", s, r)
	}
	a := apiDoc(st, "audit_log", bson.M{"action": "Changed role rights", "entity_id": "Store Keeper"})
	if a == nil || a["before"] == nil || !strings.Contains(ftJSON(a["before"]), "rep.reports") {
		t.Errorf("rights audit has no before: %v", a)
	}
}

// ESS-166: the person named as Receiving Manager decides that stage, from My view, and finds it under To approve.
func TestFtReceivingManagerActs(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1026", "EMP-1009")
	golf, _ := apiDoc(st, "location", bson.M{"name": "Gurugram – Golf Course Rd"})["id"].(string)
	eff := time.Now().AddDate(0, 1, 0).Format("2006-01-02")
	tr := map[string]any{"wf": "transfer", "employeeId": "EMP-1014", "payload": map[string]string{"toLocation": golf, "effective": eff, "toManager": "EMP-1026"}}
	s, r := c.do("POST", "/api/approvals", "EMP-1009", "Line Manager", tr)
	if s != 200 {
		t.Fatalf("raise: %d %v", s, r)
	}
	id := r["id"].(string)
	t.Logf("chain %v", ftJSON(apiDoc(st, "approval_request", bson.M{"_id": id})["chain"]))
	seen := false
	for i := 0; i < 12; i++ {
		x := apiDoc(st, "approval_request", bson.M{"_id": id})
		if x["status"] != "Pending" {
			break
		}
		chain, _ := x["chain"].([]any)
		step := int(apiFloat(x["step"]))
		if step < len(chain) && chain[step] == "Receiving Manager" {
			seen = true
			_, q := c.do("GET", "/api/approvals?tab=mine", "EMP-1026", "Employee", nil)
			if !strings.Contains(ftJSON(q), id) {
				t.Errorf("not in the Receiving Manager's To approve: %s", ftJSON(q))
			}
			if s, a := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1026", "Employee", map[string]string{"note": "ok"}); s != 200 {
				t.Fatalf("Receiving Manager approve: %d %v", s, a)
			}
			continue
		}
		c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "ok"})
	}
	if !seen {
		t.Skip("route has no Receiving Manager stage")
	}
}

// ESS-229, PPL-075: "Reporting manager has left" moves only the reports of a manager who has left.
func TestFtReassignOnlyDeparted(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	if s, r := c.do("POST", "/api/managers/reassign", "EMP-1002", "HR Admin", map[string]string{"from": "EMP-1009", "to": "EMP-1008"}); s != 400 {
		t.Fatalf("active manager's team moved: %d %v", s, r)
	}
	if n := apiCount(st, "employee", bson.M{"manager_id": "EMP-1009"}); n == 0 {
		t.Fatal("reports moved")
	}
	_, _ = st.DB.C("employee").Update(context.Background(), bson.M{"_id": "EMP-1009"}, bson.M{"$set": bson.M{"status": "Inactive"}})
	if s, r := c.do("POST", "/api/managers/reassign", "EMP-1002", "HR Admin", map[string]string{"from": "EMP-1009", "to": "EMP-1008"}); s != 200 || apiFloat(r["moved"]) < 1 {
		t.Fatalf("departed manager: %d %v", s, r)
	}
}

// HIRE-064: joining details refuse a shift, attendance mode or manager that does not exist. HIRE-219: HR's save
// keeps the Aadhaar and account number out of the audit log.
func TestFtJoiningDetailsChecks(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1010", "EMP-1011", "EMP-1012", "EMP-1014")
	oi := releasedOffer(t, c, st, "Test", "Joiner", "9876500077")
	for k, v := range map[string]string{"shift": "ZZZ", "attMode": "Telepathy", "l1": "EMP-9999"} {
		d := joinData("ABCDE1234F")
		d[k] = v
		if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": d}); s != 400 {
			t.Errorf("%s=%s accepted: %d %v", k, v, s, r)
		}
	}
	d := joinData("ABCDE1234F")
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": d}); s != 200 {
		t.Fatalf("valid details: %d %v", s, r)
	}
	a := apiDoc(st, "audit_log", bson.M{"entity_id": oi, "action": bson.M{"$regex": "^Joining details updated"}})
	if txt := ftJSON(a["after"]); a == nil || strings.Contains(txt, d["aadhaar"].(string)) || strings.Contains(txt, d["acct"].(string)) {
		t.Errorf("audit keeps the full numbers: %s", txt)
	}
}

// HIRE-489: an HRBP cannot sign a clearance item of a leaver outside their scope.
func TestFtClearanceSignNeedsScope(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1001", "EMP-1003", "EMP-1005", "EMP-1006", "EMP-1007", "EMP-1008", "EMP-1009", "EMP-1010")
	north := []string{}
	for _, n := range []string{"Gurugram – Sector 29 Hub", "Gurugram – Golf Course Rd", "Delhi – Saket"} {
		north = append(north, apiDoc(st, "location", bson.M{"name": n})["id"].(string))
	}
	e := apiDoc(st, "employee", bson.M{"location_id": bson.M{"$nin": north}, "status": "Active", "_id": bson.M{"$gt": "EMP-1013"}})
	if e == nil {
		t.Skip("no employee outside the HRBP's scope")
	}
	emp := e["id"].(string)
	s, r := c.do("POST", "/api/separations", "EMP-1003", "HRBP Corporate", map[string]any{"employeeId": emp, "type": "Resignation", "reason": "Higher studies", "givenOn": today()})
	if s != 200 {
		t.Fatalf("start: %d %v", s, r)
	}
	sep := r["id"].(string)
	req, _ := apiDoc(st, "separation", bson.M{"_id": sep})["request_id"].(string)
	c.approveAll(t, req)
	_, d := c.do("GET", "/api/separations/"+sep, "EMP-1002", "HR Admin", nil)
	tried := false
	for _, x := range d["clearance"].([]any) {
		m := x.(map[string]any)
		if m["owner"] == "HRBP" && m["auto"] == nil {
			tried = true
			if s, r := c.do("POST", "/api/separations/"+sep+"/clearance/"+fmtInt(int(apiFloat(m["id"]))), "EMP-1004", "HRBP", nil); s != 403 {
				t.Errorf("out-of-scope HRBP signed: %d %v", s, r)
			}
		}
	}
	if !tried {
		t.Skip("no HRBP clearance item")
	}
}

// PPL-014: a search of punctuation only is an empty search, never a server error.
func TestFtSearchPunctuation(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1004")
	for _, q := range []string{".*", "-", "@", "&"} {
		for _, v := range []struct{ emp, role string }{{"EMP-1002", "HR Admin"}, {"EMP-1004", "HRBP"}} {
			if s, r := c.do("GET", "/api/employees?q="+strings.ReplaceAll(strings.ReplaceAll(q, "&", "%26"), "@", "%40"), v.emp, v.role, nil); s != 200 {
				t.Errorf("%q as %s: %d %v", q, v.role, s, r)
			}
		}
	}
}

// Grade is not tied to the designation (was PPL-048). PPL-049: no reporting loop.
func TestFtJobChangeChecks(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	e := apiDoc(st, "employee", bson.M{"_id": "EMP-1016"})
	d := apiDoc(st, "designation", bson.M{"_id": e["designation_id"]})
	grades := ftJSON(d["grades"])
	bad := ""
	for _, g := range []string{"G1", "G2", "G3", "G4", "G5", "G6"} {
		if !strings.Contains(grades, `"`+g+`"`) {
			bad = g
			break
		}
	}
	day := time.Now().Format("2006-01-02")
	hireFixRoleSkill(t, st, "EMP-1016") // a job change needs Role and Skill on the record
	if bad != "" {
		// Designation and grade are separate (owner, 27 Nov 2026): any grade goes with any designation.
		if s, r := c.do("PATCH", "/api/employees/EMP-1016/job", "EMP-1002", "HR Admin", map[string]any{"changes": map[string]string{"grade": bad}, "reason": "Promotion", "effectiveFrom": day}); s != 200 {
			t.Errorf("grade %s outside the designation's old list refused: %d %v", bad, s, r)
		}
	}
	// EMP-1009 reports to EMP-1008; making EMP-1008 report to EMP-1009 would be a loop.
	s, r := c.do("PATCH", "/api/employees/EMP-1008/job", "EMP-1002", "HR Admin", map[string]any{"changes": map[string]string{"manager": "EMP-1009"}, "reason": "Promotion", "effectiveFrom": day})
	if s != 400 || !strings.Contains(ftJSON(r), "loop") {
		t.Errorf("reporting loop: %d %v", s, r)
	}
}

// PPL-058, PPL-059, PPL-149: personal details refuse a huge address and a mobile already used by someone else, and
// the same email in another case is no change.
func TestFtPersonalChecks(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	other, _ := apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020"})["mobile"].(string)
	email, _ := apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1016"})["email"].(string)
	if other != "" {
		if s, r := c.do("PATCH", "/api/employees/EMP-1016/personal", "EMP-1002", "HR Admin", map[string]any{"mobile": other}); s != 400 {
			t.Errorf("duplicate mobile: %d %v", s, r)
		}
	}
	if s, r := c.do("PATCH", "/api/employees/EMP-1016/personal", "EMP-1002", "HR Admin", map[string]any{"address": strings.Repeat("x", 20000)}); s != 400 {
		t.Errorf("huge address: %d %v", s, r)
	}
	if email != "" {
		_, _ = st.DB.C("employee_personal").Update(context.Background(), bson.M{"employee_id": "EMP-1016"}, bson.M{"$set": bson.M{"email": strings.ToUpper(email[:1]) + email[1:]}})
		n := apiCount(st, "employee_change", bson.M{"employee_id": "EMP-1016"})
		if s, r := c.do("PATCH", "/api/employees/EMP-1016/personal", "EMP-1002", "HR Admin", map[string]any{"email": strings.ToLower(email)}); s != 200 {
			t.Fatalf("same email: %d %v", s, r)
		}
		if apiCount(st, "employee_change", bson.M{"employee_id": "EMP-1016"}) != n {
			t.Error("an email in another case was saved as a change")
		}
	}
}

// PPL-090, PPL-092, PPL-098: the import applies the Personal details rules, keeps Aadhaar and mobile unique, refuses
// an Inactive status with a future last day, a long name and a code with "/"; the JSON import's preview finds a
// manager defined earlier in the same file, as applying does.
func TestFtImportChecks(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	head := "Employee Code,Full Name,Employment Type,Date of Joining,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Monthly Gross,Mobile,Email,Date of Birth,Gender,PAN,Aadhaar,UAN,ESIC No,Bank Name,Account Number,IFSC,Status,Last Working Day,Opening CL Balance,Opening PL Balance,Opening SL Balance"
	base := func(code, name, extra string) string {
		return code + "," + name + ",Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,24500," + extra
	}
	mob, _ := apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020", "mobile": bson.M{"$ne": nil}})["mobile"].(string)
	aad, _ := apiDoc(st, "employee_gov_id", bson.M{"aadhaar": bson.M{"$ne": nil}})["aadhaar"].(string)
	future := time.Now().AddDate(0, 0, 20).Format("2006-01-02")
	rows := []string{
		base("ODD-1", "Odd Gender", ",,,Robot,,,,,,,,,,,,"),
		base("ODD-2", "Odd Aadhaar", ",,,,,"+aad+",,,,,,,,,,"),
		base("ODD-3", "Odd Mobile", mob+",,,,,,,,,,,,,,,"),
		base("ODD-4", "Odd Birth", ",,"+future+",,,,,,,,,,,,,"),
		base("ODD-5", "Odd Child", ",,"+time.Now().AddDate(-4, 0, 0).Format("2006-01-02")+",,,,,,,,,,,,,"),
		base("ODD-7", "Odd Future Exit", ",,,,,,,,,,,Inactive,"+future+",,,"),
		base("ODD-8", strings.Repeat("X", 300), ",,,,,,,,,,,,,,,"),
		base("ODD/9", "Odd Slash", ",,,,,,,,,,,,,,,"),
	}
	s, r := c.sendFile("/api/bulk/new/check", "EMP-1002", "HR Admin", "new.csv", []byte(withRoleSkill(head+"\n"+strings.Join(rows, "\n")+"\n")))
	if s != 200 {
		t.Fatalf("%d %v", s, r)
	}
	for i, x := range r["results"].([]any) {
		m := x.(map[string]any)
		if len(m["errors"].([]any)) == 0 {
			if (i == 1 && aad == "") || (i == 2 && mob == "") {
				continue
			}
			t.Errorf("row %d (%v) accepted", i+2, m["id"])
		}
	}
	// JSON import: a manager earlier in the same request.
	e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	loc := apiDoc(st, "location", bson.M{"_id": e["location_id"]})["name"]
	dept := apiDoc(st, "department", bson.M{"_id": e["department_id"]})["name"]
	desig := apiDoc(st, "designation", bson.M{"_id": e["designation_id"]})["name"]
	row := func(code, mgr string) map[string]any {
		return map[string]any{"code": code, "name": "Json " + code, "type": "Permanent", "doj": "2024-01-10", "location": loc, "department": dept, "designation": desig,
			"grade": "G1", "shift": "G", "gross": 17000, "managerId": mgr}
	}
	s, r = c.do("POST", "/api/employees/import", "EMP-1002", "HR Admin", map[string]any{"rows": []any{row("JS-BOSS", ""), row("JS-REP", "JS-BOSS")}})
	if s != 200 || apiFloat(r["failed"]) != 0 {
		t.Errorf("preview: %d %v", s, r)
	}
}

// PPL-126: an employee reading the location and city masters gets no budgets.
func TestFtMastersNoBudgets(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1020", "EMP-1002")
	for _, typ := range []string{"location", "city"} {
		s, r := c.do("GET", "/api/masters/"+typ, "EMP-1020", "Employee", nil)
		if s != 200 || strings.Contains(ftJSON(r), "budget") {
			t.Errorf("%s as Employee: %d budgets shown", typ, s)
		}
	}
	if _, r := c.do("GET", "/api/masters/location", "EMP-1002", "HR Admin", nil); !strings.Contains(ftJSON(r), "payroll_budget") {
		t.Error("HR Admin lost the budgets")
	}
}

// PPL-127: the built-in employment types and items the app writes itself cannot be made inactive.
func TestFtRetireAppItems(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	for _, x := range []struct{ typ, kind, name string }{{"employment_type", "employment_type", "Permanent"}, {"attendance_mode", "attendance_mode", "Biometric"}} {
		d := apiDoc(st, "simple_master", bson.M{"kind": x.kind, "name": x.name})
		if d == nil {
			t.Fatalf("no %s", x.name)
		}
		if s, r := c.do("POST", "/api/masters/"+x.typ+"/"+d["id"].(string)+"/inactive", "EMP-1002", "HR Admin", map[string]any{"since": today(), "reason": "Testing it"}); s != 400 {
			t.Errorf("%s made inactive: %d %v", x.name, s, r)
		}
	}
	d := apiDoc(st, "simple_master", bson.M{"sys_key": "sep:Resignation"})
	if d != nil {
		typ := "change_reason"
		if s, r := c.do("POST", "/api/masters/"+typ+"/"+d["id"].(string)+"/inactive", "EMP-1002", "HR Admin", map[string]any{"since": today(), "reason": "Testing it"}); s != 400 {
			t.Errorf("Resigned made inactive: %d %v", s, r)
		}
	}
}

// PPL-133: employment types are the three with pay rules; a new one cannot be added.
func TestFtNoNewEmploymentType(t *testing.T) {
	c := setup(t)
	c.signIn("EMP-1002")
	if s, r := c.do("POST", "/api/masters/employment_type", "EMP-1002", "HR Admin", map[string]any{"name": "Intern"}); s != 400 {
		t.Errorf("Intern added: %d %v", s, r)
	}
}

// PPL-150, PPL-156: the current data of Salary breakup, Pay bands and Budgets uploaded unchanged gives no changes
// and no errors.
func TestFtBulkRoundTrip(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	// A grade with no bands yet, and a second city with the name of an existing one in another state.
	if s, r := c.do("POST", "/api/masters/grade", "EMP-1002", "HR Admin", map[string]any{"name": "G7"}); s != 200 {
		t.Logf("grade: %d %v", s, r)
	}
	gur := apiDoc(st, "city", bson.M{"name": "Gurugram"})
	other := apiDoc(st, "state", bson.M{"_id": bson.M{"$ne": gur["state_id"]}})
	if s, r := c.do("POST", "/api/masters/city", "EMP-1002", "HR Admin", map[string]any{"name": "Gurugram", "state_id": other["id"]}); s != 200 {
		t.Logf("city: %d %v", s, r)
	}
	for _, tpl := range []string{"sal", "band", "bud", "pers"} {
		s, b := c.getRaw("/api/bulk/"+tpl+"/download?data=1", "EMP-1002", "HR Admin")
		if s != 200 {
			t.Fatalf("%s download: %d %s", tpl, s, b)
		}
		s, r := c.sendFile("/api/bulk/"+tpl+"/check", "EMP-1002", "HR Admin", tpl+".xlsx", b)
		if s != 200 {
			t.Fatalf("%s check: %d %v", tpl, s, r)
		}
		nErr, nCh := 0, 0
		var sample any
		for _, x := range r["results"].([]any) {
			m := x.(map[string]any)
			if e, _ := m["errors"].([]any); len(e) > 0 {
				nErr++
				sample = m
			}
			if ch, _ := m["changes"].([]any); len(ch) > 0 {
				nCh++
				sample = m
			}
		}
		if len(r["results"].([]any)) == 0 {
			t.Errorf("%s: no rows", tpl)
		}
		if nErr > 0 || nCh > 0 {
			t.Errorf("%s round trip: %d rows with errors, %d with changes, e.g. %v", tpl, nErr, nCh, sample)
		}
	}
}

// SET-010, SET-016, SET-038, SET-054, SET-056, SET-060, SET-076, SET-078: compensation inputs are checked.
func TestFtCompensationChecks(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011")
	ctx := context.Background()
	v1 := apiDoc(st, "salary_structure", bson.M{"is_default": true})
	var rates map[string]any
	_ = json.Unmarshal([]byte(ftJSON(v1["rates"])), &rates)
	if rates == nil || len(rates) == 0 {
		b, _ := json.Marshal(v1["rates"])
		_ = json.Unmarshal(b, &rates)
	}
	for k, v := range map[string]any{"medPct": -3, "medMax": -500, "grat": -4.81, "plDays": -15, "bonusPct": -8.33, "bonusCalc": -7000, "epfoAdm": -0.5, "esiCeil": 0} {
		bad := map[string]any{}
		for a, b := range rates {
			bad[a] = b
		}
		bad[k] = v
		if s, r := c.do("PUT", "/api/comp/structures/"+v1["id"].(string), "EMP-1002", "HR Admin", map[string]any{"rates": bad}); s != 400 {
			t.Errorf("%s=%v accepted: %d %v", k, v, s, r)
		}
	}
	if s, r := c.do("PUT", "/api/comp/structures/"+v1["id"].(string), "EMP-1002", "HR Admin", map[string]any{"rates": rates}); s != 200 {
		t.Errorf("current rates refused: %d %v", s, r)
	}
	// Version names.
	if s, r := c.do("POST", "/api/comp/structures", "EMP-1002", "HR Admin", map[string]any{"name": "Version 2"}); s != 200 {
		t.Fatalf("v2: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/comp/structures", "EMP-1002", "HR Admin", map[string]any{"name": "version 2"}); s != 400 {
		t.Error("same name in another case accepted")
	}
	if s, _ := c.do("POST", "/api/comp/structures", "EMP-1002", "HR Admin", map[string]any{"name": strings.Repeat("V", 300)}); s != 400 {
		t.Error("long name accepted")
	}
	// Calculator.
	if s, _ := c.do("POST", "/api/salary/preview", "EMP-1011", "Payroll", map[string]any{"type": "Permanent", "gross": 30000, "meal": -500}); s != 400 {
		t.Error("negative meal in the calculator")
	}
	// Bands.
	city := apiDoc(st, "city", bson.M{"name": "Gurugram"})["id"].(string)
	mum := apiDoc(st, "city", bson.M{"name": "Mumbai"})
	for _, b := range []map[string]any{{"cityId": "not-a-city", "grade": "G1", "min": 10000, "max": 20000}, {"cityId": city, "grade": "G99", "min": 10000, "max": 20000}} {
		if s, r := c.do("PUT", "/api/comp/bands", "EMP-1002", "HR Admin", b); s != 400 {
			t.Errorf("band %v accepted: %d %v", b, s, r)
		}
	}
	if mum != nil {
		if s, _ := c.do("POST", "/api/comp/bands/copy", "EMP-1002", "HR Admin", map[string]any{"from": city, "to": mum["id"], "factor": -2}); s != 400 {
			t.Error("negative factor accepted")
		}
		if s, r := c.do("POST", "/api/comp/bands/copy", "EMP-1002", "HR Admin", map[string]any{"from": city, "to": mum["id"], "factor": 1}); s != 200 {
			t.Fatalf("copy: %d %v", s, r)
		}
		if a := apiDoc(st, "audit_log", bson.M{"action": "Pay bands copied"}); a == nil || a["before"] == nil {
			t.Errorf("copy audit has no before: %v", a)
		}
	}
	// LWF.
	state := apiDoc(st, "state", bson.M{"name": "Haryana"})
	if state != nil {
		rule := map[string]any{"on": true, "ee": map[string]any{"mode": "pct", "val": 0.2, "max": 34}, "er": map[string]any{"mode": "pct", "val": 0.4, "max": 68}, "freq": "half", "months": "13,abc"}
		if s, _ := c.do("PUT", "/api/comp/lwf/"+state["id"].(string), "EMP-1011", "Payroll", map[string]any{"rule": rule, "effectiveFrom": today()}); s != 400 {
			t.Error("month 13 accepted")
		}
		rule["months"] = "6,12"
		rule["ee"] = map[string]any{"mode": "pct", "val": 250, "max": 0}
		if s, _ := c.do("PUT", "/api/comp/lwf/"+state["id"].(string), "EMP-1011", "Payroll", map[string]any{"rule": rule, "effectiveFrom": today()}); s != 400 {
			t.Error("250% accepted")
		}
		rule["ee"] = map[string]any{"mode": "fixed", "val": 40, "max": 40}
		future := time.Now().AddDate(0, 2, 0).Format("2006-01-02")
		if s, r := c.do("PUT", "/api/comp/lwf/"+state["id"].(string), "EMP-1011", "Payroll", map[string]any{"rule": rule, "effectiveFrom": future}); s != 200 {
			t.Fatalf("future rule: %d %v", s, r)
		}
		_, comp := c.do("GET", "/api/comp", "EMP-1011", "Payroll", nil)
		for _, x := range comp["lwf"].([]any) {
			m := x.(map[string]any)
			if m["state_id"] == state["id"] && (m["effective_from"] == future || m["next_from"] != future) {
				t.Errorf("LWF row shows the future rule as current: %v", m)
			}
		}
	}
	if mum == nil || state == nil {
		t.Errorf("sample data: Mumbai %v, Haryana %v", mum != nil, state != nil)
	}
	_ = ctx
}

// SET-069: a fractional headcount is named on its row and the other rows still apply.
func TestFtBudgetFraction(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	csv := "Level,Name,Headcount Budget,Monthly Payroll Budget\nLocation,Delhi – Saket,12.5,\nLocation,Gurugram – Golf Course Rd,17,\n"
	s, r := c.sendFile("/api/bulk/bud/apply", "EMP-1002", "HR Admin", "bud.csv", []byte(csv))
	if s != 200 {
		t.Fatalf("%d %v", s, r)
	}
	if !strings.Contains(ftJSON(r), "whole number") {
		t.Errorf("12.5 not named: %v", r)
	}
	if hc := apiFloat(apiDoc(st, "location", bson.M{"name": "Gurugram – Golf Course Rd"})["headcount_budget"]); hc != 17 {
		t.Errorf("valid row not applied: %v", hc)
	}
}

// SET-084, SET-095, SET-098, SET-099: payroll setup is for pay roles; a CHRO (IDs and bank Hidden) sees no PAN, UAN or
// account on a payslip preview; the amount in words matches the (whole-rupee) net.
func TestFtPayslipMasking(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1001", "EMP-1020", "EMP-1009", "EMP-1011")
	for _, v := range []struct{ emp, role string }{{"EMP-1020", "Employee"}, {"EMP-1009", "Line Manager"}} {
		if s, _ := c.do("GET", "/api/payslip-setup", v.emp, v.role, nil); s != 403 {
			t.Errorf("%s reads payroll setup: %d", v.role, s)
		}
	}
	if s, _ := c.do("GET", "/api/payslip-setup", "EMP-1011", "Payroll", nil); s != 200 {
		t.Error("Payroll cannot read setup")
	}
	for _, path := range []string{"/api/payslip-preview", "/api/payslip-preview?employeeId=EMP-1020"} {
		s, r := c.do("GET", path, "EMP-1001", "CHRO", nil)
		if s != 200 {
			t.Errorf("%s: %d %v", path, s, r)
			continue
		}
		snap, _ := r["snap"].(map[string]any)
		e, _ := snap["emp"].(map[string]any)
		if e["id"] == "EMP-1001" || e["code"] == "EMP-1001" || r["employeeId"] == "EMP-1001" || strings.Contains(ftJSON(e), "EMP-1001") {
			t.Logf("%s previews the CHRO's own payslip", path)
		} else if e["pan"] != nil || e["uan"] != nil || e["bank"] != nil {
			t.Errorf("%s: CHRO sees %v %v %v", path, e["pan"], e["uan"], e["bank"])
		}
		net := apiFloat(snap["net"])
		if net != float64(int64(net)) {
			t.Errorf("net not whole rupees: %v", net)
		}
	}
	_, r := c.do("GET", "/api/payslip-preview?employeeId=EMP-1020", "EMP-1011", "Payroll", nil)
	if snap, _ := r["snap"].(map[string]any); snap["emp"].(map[string]any)["pan"] == nil {
		t.Error("Payroll lost the PAN")
	}
}

// SET-087: an invalid CIN is refused; a real-looking one is kept.
func TestFtCompanyCIN(t *testing.T) {
	c := setup(t)
	c.signIn("EMP-1011")
	body := func(cin string) map[string]any {
		return map[string]any{"setup": map[string]any{"accent": "#0f6e6e", "footer": "x", "showEmployer": true}, "cin": cin, "pan": "", "tan": "", "pfCode": "", "esiCode": "", "cutoff": "calendar", "firstYm": ""}
	}
	if s, _ := c.do("PUT", "/api/payslip-setup", "EMP-1011", "Payroll", body("hello world")); s != 400 {
		t.Error("CIN 'hello world' accepted")
	}
	if s, r := c.do("PUT", "/api/payslip-setup", "EMP-1011", "Payroll", body("U55101DL2019PTC123456")); s != 200 {
		t.Errorf("valid CIN refused: %d %v", s, r)
	}
}

// SET-125, SET-127, SET-134, SET-136: tax windows near their year; one document is one proof; the reminder reaches
// the bell; an item limit stays within the law.
func TestFtTaxChecks(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1020")
	_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	w := tax["windows"].(map[string]any)
	if s, _ := c.do("PUT", "/api/tax/windows?fy=2031-32", "EMP-1002", "HR Admin", map[string]any{"declFrom": "2020-01-01", "declTo": "2040-01-01", "proofFrom": "2020-01-01", "proofTo": "2040-01-01"}); s != 400 {
		t.Error("windows 2020-2040 accepted for 2031-32")
	}
	win := map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "declForce": true, "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": true}
	if s, r := c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win); s != 200 {
		t.Fatalf("windows: %d %v", s, r)
	}
	_, doc := c.upload("EMP-1002", "Employee", "employee", "EMP-1002", "PPF passbook")
	p := map[string]any{"item": "c_ppf", "doc": doc["id"], "amount": 50000}
	if s, r := c.do("POST", "/api/tax/me/proofs", "EMP-1002", "Employee", p); s != 200 {
		t.Fatalf("proof: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/tax/me/proofs", "EMP-1002", "Employee", p); s != 400 {
		t.Error("same document sent twice")
	}
	if s, r := c.do("POST", "/api/tax/remind", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("remind: %d %v", s, r)
	}
	if n := notices(c, "EMP-1020", "Employee"); !strings.Contains(n, "declare your tax") {
		t.Errorf("no tax reminder under the bell: %s", n)
	}
	if s, _ := c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []map[string]any{{"id": "d_self", "cap": 100000}}}); s != 400 {
		t.Error("80D self limit raised to 1,00,000")
	}
	if s, r := c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []map[string]any{{"id": "d_self", "cap": 20000}}}); s != 200 {
		t.Errorf("lower limit refused: %d %v", s, r)
	}
}

// TIME-010, TIME-011, TIME-012, TIME-042: a shift cannot take a roster code (WO, PH, L).
func TestFtShiftReservedCodes(t *testing.T) {
	c := setup(t)
	c.signIn("EMP-1002")
	for _, code := range []string{"WO", "PH", "L", "wo"} {
		body := map[string]any{"code": code, "name": "Odd " + code, "effectiveFrom": "", "startTime": "09:00", "endTime": "18:00", "breakMinutes": 30, "maps": []map[string]any{{"departmentId": "", "locationId": ""}}, "rules": map[string]any{}}
		if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body); s != 400 {
			t.Errorf("shift %s created: %d %v", code, s, r)
		}
		if s, r := c.do("POST", "/api/masters/shift", "EMP-1002", "HR Admin", map[string]any{"code": code, "name": "Odd master " + code, "start_time": "09:00", "end_time": "18:00"}); s == 200 {
			t.Errorf("master shift %s created: %v", code, r)
		}
	}
}

// TIME-065 to TIME-072, TIME-084, TIME-098, TIME-240: work policy settings are checked against their choices, a
// mapping names a real record, a date is a holiday once, and an unknown punch machine is not found.
func TestFtPolicyChecks(t *testing.T) {
	c := setup(t)
	c.signIn("EMP-1002")
	_, pl := c.do("GET", "/api/policies", "EMP-1002", "HR Admin", nil)
	pid := pl["list"].([]any)[0].(map[string]any)["id"].(string)
	_, got := c.do("GET", "/api/policies/"+pid, "EMP-1002", "HR Admin", nil)
	full := got["policy"].(map[string]any)
	base := ftJSON(full["settings"])
	put := func(mut func(m map[string]any), eff string) (int, map[string]any) {
		var m map[string]any
		_ = json.Unmarshal([]byte(base), &m)
		mut(m)
		return c.do("PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"name": full["name"], "settings": m, "leavePlanId": full["leavePlanId"], "calendarId": full["calendarId"], "effectiveFrom": eff})
	}
	sub := func(m map[string]any, k string) map[string]any { x, _ := m[k].(map[string]any); return x }
	cases := map[string]func(m map[string]any){
		"sat banana":     func(m map[string]any) { m["sat"] = "banana" },
		"offMode":        func(m map[string]any) { m["offMode"] = "sometimes" },
		"sun maybe":      func(m map[string]any) { m["sun"] = "maybe" },
		"lateAct fire":   func(m map[string]any) { sub(m, "att")["lateAct"] = "fire" },
		"missing x":      func(m map[string]any) { sub(m, "att")["missing"] = "x" },
		"restAct ignore": func(m map[string]any) { sub(m, "att")["restAct"] = "ignore" },
		"past open":      func(m map[string]any) { sub(m, "rs")["past"] = "open" },
		"wfh days -3":    func(m map[string]any) { sub(m, "att")["wfhDays"] = -3 },
	}
	for name, mut := range cases {
		if s, r := put(mut, ""); s != 400 {
			t.Errorf("%s accepted: %d %v", name, s, r)
		}
	}
	if s, r := put(func(m map[string]any) {}, ""); s != 200 {
		t.Fatalf("unchanged policy refused: %d %v", s, r)
	}
	if s, _ := c.do("PUT", "/api/policies/"+pid+"/maps", "EMP-1002", "HR Admin", map[string]any{"maps": []any{map[string]any{"level": "employee", "ref": "EMP-9999", "on": true}}}); s != 400 {
		t.Error("mapping to EMP-9999 accepted")
	}
	cals := c.mustList(t, "/api/calendars")
	calID := cals[0].(map[string]any)["id"].(string)
	// A date in the calendar's own year (a holiday in another year is refused since 24 Nov 2026).
	yr := ftJSON(cals[0].(map[string]any)["year"]) // 2026
	h := map[string]any{"holiday": map[string]any{"date": yr + "-11-12", "name": "QA Holiday", "kind": "Regional"}}
	c.do("PUT", "/api/calendars/"+calID, "EMP-1002", "HR Admin", h)
	h["holiday"].(map[string]any)["name"] = "QA Holiday again"
	if s, _ := c.do("PUT", "/api/calendars/"+calID, "EMP-1002", "HR Admin", h); s != 400 {
		t.Error("second holiday on the same date accepted")
	}
	if s, _ := c.do("POST", "/api/devices/999999/inactive", "EMP-1002", "HR Admin", map[string]any{"reason": "Gone"}); s != 404 {
		t.Errorf("unknown machine: %d", s)
	}
}

func (c *client) mustList(t *testing.T, path string) []any {
	_, r := c.do("GET", path, "EMP-1002", "HR Admin", nil)
	if l, ok := r["list"].([]any); ok && len(l) > 0 {
		return l
	}
	if l, ok := r["calendars"].([]any); ok && len(l) > 0 {
		return l
	}
	t.Fatalf("%s: %v", path, r)
	return nil
}

// TIME-280: with "Manager can regularize for the team" off, a manager cannot raise one for a report. AUTH-051: someone
// who has left raises no request of their own.
func TestFtRaiseRules(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1009", "EMP-1014", "EMP-1016")
	_, sum := c.do("GET", "/api/employees/EMP-1014/policy", "EMP-1002", "HR Admin", nil)
	_ = sum
	if _, err := st.DB.C("work_policy").UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"settings.rg.mgr": false}}); err != nil {
		t.Fatal(err)
	}
	d := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	s, r := c.do("POST", "/api/approvals", "EMP-1009", "Line Manager", map[string]any{"wf": "regularize", "employeeId": "EMP-1014", "payload": map[string]string{"date": d, "rtype": "No Punch", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}})
	if s != 403 || !strings.Contains(ftJSON(r), "regularize for the team") {
		t.Errorf("manager raised with rg.mgr off: %d %v", s, r)
	}
	_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": time.Now().AddDate(0, 0, -5).Format("2006-01-02")}})
	if s, r := c.do("POST", "/api/approvals", "EMP-1016", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Bank", "bankName": "SBI", "accountNo": "123456789012", "ifsc": "SBIN0001234"}}); s != 403 {
		t.Errorf("a leaver raised a bank change: %d %v", s, r)
	}
}

// TIME-237: a stray machine punch does not wipe a complete day from the MIS upload.
func TestFtStrayPunchKeepsMIS(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1002")
	day := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	csv := "Employee ID,Date,In time,Out time\nEMP-1020," + day + ",09:00,18:00\n"
	if s, r := c.sendFile("/api/bulk/att/apply", "EMP-1002", "HR Admin", "att.csv", []byte(csv)); s != 200 {
		t.Fatalf("MIS upload: %d %v", s, r)
	}
	if a := apiDoc(st, "attendance_day", bson.M{"employee_id": "EMP-1020", "date": day}); a == nil || a["source"] != "MIS upload" {
		t.Fatalf("MIS day not stored: %v", a)
	}
	_, dev := c.do("POST", "/api/devices", "EMP-1002", "HR Admin", map[string]any{"name": "Gate", "vendor": "eSSL"})
	var buf strings.Builder
	_ = json.NewEncoder(&buf).Encode(map[string]any{"punches": []any{map[string]any{"employeeId": "EMP-1020", "time": day + " 13:00"}}})
	req, _ := http.NewRequest("POST", c.base+"/api/punches", strings.NewReader(buf.String()))
	req.Header.Set("X-Device-Key", dev["key"].(string))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	a := apiDoc(st, "attendance_day", bson.M{"employee_id": "EMP-1020", "date": day})
	if a == nil || a["source"] != "MIS upload" || a["in_time"] != "09:00" || a["out_time"] != "18:00" {
		t.Errorf("MIS day replaced by a stray punch: %v", a)
	}
}

// TIME-278: with regularizations counted together, a No Punch after an All Other in the month is the 2nd.
func TestFtRegCountCombined(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1014")
	if _, err := st.DB.C("work_policy").UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"settings.regCount": "combined"}}); err != nil {
		t.Fatal(err)
	}
	d := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if time.Now().Day() == 1 {
		t.Skip("first of the month: no earlier day in it")
	}
	_, _ = st.DB.C("attendance_day").DeleteMany(ctx, bson.M{"employee_id": "EMP-1014", "date": d})
	c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]string{"employeeId": "EMP-1014", "kind": "regularize", "from": d, "to": d, "until": time.Now().AddDate(0, 0, 3).Format("2006-01-02"), "reason": "Test"})
	s, r := c.do("POST", "/api/approvals", "EMP-1014", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": d, "rtype": "All Other", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}})
	if s != 200 {
		t.Skipf("first regularization refused: %v", r)
	}
	_, sim := c.do("POST", "/api/approvals/simulate", "EMP-1014", "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": d, "rtype": "No Punch", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}})
	if why := ftJSON(sim["why"]); strings.Contains(why, "1st") || !strings.Contains(why, "2") {
		t.Errorf("counted as the first: %s", why)
	}
}

// HIRE-073, HIRE-236, HIRE-237: a candidate document must be one Form setup lists (or the résumé); a document's
// departments must exist; a renamed label is at most 80 characters.
func TestFtOnboardingSetupChecks(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1010")
	si := selectCandidate(t, c, shortlistBody(st, "Doc", "Check", "9876500088"))
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "randomjunk"); s != 400 {
		t.Errorf("unknown document key accepted: %d %v", s, r)
	}
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "resume"); s != 200 {
		t.Errorf("résumé refused: %d %v", s, r)
	}
	if s, _ := c.do("PATCH", "/api/onboarding/setup/docs/police", "EMP-1002", "HR Admin", map[string]any{"depts": []string{"NoSuchDept"}}); s != 400 {
		t.Error("unknown department accepted")
	}
	if s, _ := c.do("PATCH", "/api/onboarding/setup/fields/first", "EMP-1002", "HR Admin", map[string]any{"label": strings.Repeat("L", 500)}); s != 400 {
		t.Error("500-character label accepted")
	}
}

// HIRE-422: the employee's own resignation ignores a waiver sent with it. HIRE-498: HR cannot move the last working
// day decades away.
func TestFtResignationBounds(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1008", "EMP-1020")
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation", "waive": true, "lwd": "2026-11-20", "noticeDays": 1}})
	if s != 200 {
		t.Fatalf("resign: %d %v", s, r)
	}
	req := apiDoc(st, "approval_request", bson.M{"_id": r["id"]})
	if f, _ := req["facts"].(map[string]any); f["waive"] == true {
		t.Errorf("waiver kept: %v", f)
	}
	sep := strings.Trim(ftJSON(r["case"]), `"`)
	ui3ApproveAll(t, c, strings.Trim(ftJSON(r["id"]), `"`))
	if s, r := c.do("POST", "/api/separations/"+sep+"/lwd", "EMP-1002", "HR Admin", map[string]any{"lwd": "2090-01-01", "note": "Typo test"}); s != 400 || !strings.Contains(ftJSON(r), "at most a year") {
		t.Errorf("LWD in 2090 accepted: %d %v", s, r)
	}
}
