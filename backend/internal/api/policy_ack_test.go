package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Company policy acknowledgement (Part C): HR publishes a policy, the employee uploads a signed copy, the
// reporting manager approves or returns it, and the payroll run holds the salary while anything is pending.
// EMP-1020 and EMP-1014 report to EMP-1009 (Line Manager); EMP-1004 is the HRBP for their property.
func TestPolicyAck(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1014", "EMP-1020", "EMP-1021"} {
		c.signIn(e)
	}
	const hr, hrRole, emp, mgr = "EMP-1002", "HR Admin", "EMP-1020", "EMP-1009"
	str := func(v any) string { s, _ := v.(string); return s }
	num := func(v any) int { f, _ := v.(float64); return int(f) }

	// Setup: only HR Admin adds policies; the document is a PDF uploaded under a draft key.
	s, doc := c.upload(hr, hrRole, "policy", "DRAFT-0123456789ab", "document")
	if s != 200 {
		t.Fatalf("policy upload %d %v", s, doc)
	}
	if s, _ := c.upload(emp, "Employee", "policy", "DRAFT-0123456789cd", "document"); s != 403 {
		t.Fatalf("an employee cannot upload a policy: %d", s)
	}
	if s, _ := c.do("POST", "/api/company-policies", emp, "Employee", map[string]any{"title": "Code of conduct", "version": "1.0", "effectiveFrom": "2026-01-01", "docId": doc["id"]}); s != 403 {
		t.Fatalf("an employee cannot add a policy: %d", s)
	}
	if s, r := c.do("POST", "/api/company-policies", hr, hrRole, map[string]any{"title": "Code of conduct", "version": "1.0", "effectiveFrom": "2026-01-01",
		"audience": map[string]any{"all": false}, "docId": doc["id"]}); s != 400 {
		t.Fatalf("an audience is needed: %d %v", s, r)
	}
	s, r := c.do("POST", "/api/company-policies", hr, hrRole, map[string]any{"title": "Code of conduct", "version": "1.0", "effectiveFrom": "2026-01-01",
		"audience": map[string]any{"all": true}, "newJoiners": true, "docId": doc["id"]})
	if s != 200 {
		t.Fatalf("create %d %v", s, r)
	}
	pid := str(r["id"])
	// A draft is due for nobody.
	if _, m := c.do("GET", "/api/me/policies", emp, "Employee", nil); len(m["items"].([]any)) != 0 {
		t.Fatalf("draft shown to the employee: %v", m)
	}
	if s, r := c.do("POST", "/api/company-policies/"+pid+"/publish", hr, hrRole, nil); s != 200 {
		t.Fatalf("publish %d %v", s, r)
	}
	active := apiCount(st, "employee", bson.M{"status": "Active"})
	_, l := c.do("GET", "/api/company-policies", hr, hrRole, nil)
	counts := l["policies"].([]any)[0].(map[string]any)["counts"].(map[string]any)
	if num(counts["Not submitted"]) != active || num(counts["total"]) != active {
		t.Fatalf("counts %v, %d active", counts, active)
	}

	// The employee's list: due, with the month that will be held.
	_, m := c.do("GET", "/api/me/policies", emp, "Employee", nil)
	items := m["items"].([]any)
	if len(items) != 1 || num(m["pending"]) != 1 || m["holdLabel"] == nil {
		t.Fatalf("my policies %v", m)
	}
	it := items[0].(map[string]any)
	ver := fmt.Sprint(num(it["version_id"]))
	if it["status"] != "Not submitted" || str(it["policy_doc"]) == "" {
		t.Fatalf("item %v", it)
	}
	if s, _ := c.do("GET", "/api/files/"+str(it["policy_doc"]), emp, "Employee", nil); s != 200 {
		t.Fatalf("the employee downloads the policy: %d", s)
	}

	// Signed copy: only the employee uploads their own, then sends it.
	if s, _ := c.upload("EMP-1021", "Employee", "policy_ack", emp+"/"+ver, "signed"); s != 403 {
		t.Fatalf("someone else uploads the copy: %d", s)
	}
	if s, r := c.do("POST", "/api/me/policies/"+ver+"/submit", emp, "Employee", map[string]any{"docId": str(it["policy_doc"])}); s != 400 {
		t.Fatalf("the policy file is not a signed copy: %d %v", s, r)
	}
	_, signed := c.upload(emp, "Employee", "policy_ack", emp+"/"+ver, "signed")
	s, r = c.do("POST", "/api/me/policies/"+ver+"/submit", emp, "Employee", map[string]any{"docId": signed["id"]})
	if s != 200 || r["status"] != "Pending" || !strings.HasPrefix(str(r["id"]), "PA-") {
		t.Fatalf("submit %d %v", s, r)
	}
	req := str(r["id"])
	if s, _ := c.do("POST", "/api/me/policies/"+ver+"/submit", emp, "Employee", map[string]any{"docId": signed["id"]}); s != 400 {
		t.Fatal("a second copy while one is waiting")
	}
	if s, _ := c.do("POST", "/api/approvals", emp, "Employee", map[string]any{"wf": "p_ack", "payload": map[string]any{}}); s != 400 {
		t.Fatal("p_ack is not raised from Approvals")
	}
	_, m = c.do("GET", "/api/me/policies", emp, "Employee", nil)
	it = m["items"].([]any)[0].(map[string]any)
	if it["status"] != "Waiting for manager" || it["approver"] != "Anil Kapoor" {
		t.Fatalf("waiting %v", it)
	}

	// The manager's inbox has it, with the copy openable; an HRBP acts from Open, not from their inbox.
	inbox := func(e, role, tab string) map[string]any {
		_, q := c.do("GET", "/api/approvals?tab="+tab+"&wf=p_ack", e, role, nil)
		for _, x := range q["list"].([]any) {
			if x.(map[string]any)["id"] == req {
				return x.(map[string]any)
			}
		}
		return nil
	}
	if x := inbox(mgr, "Line Manager", "mine"); x == nil || x["canAct"] != true {
		t.Fatalf("manager inbox %v", x)
	}
	if inbox("EMP-1004", "HRBP", "mine") != nil {
		t.Fatal("the HRBP's inbox lists only what is named to them")
	}
	if x := inbox("EMP-1004", "HRBP", "open"); x == nil || x["canAct"] != true {
		t.Fatalf("HRBP can act from Open: %v", x)
	}
	if s, _ := c.do("GET", "/api/files/"+str(signed["id"]), mgr, "Line Manager", nil); s != 200 {
		t.Fatalf("manager opens the signed copy: %d", s)
	}
	if s, _ := c.do("GET", "/api/files/"+str(signed["id"]), "EMP-1021", "Employee", nil); s != 403 {
		t.Fatalf("a colleague opens the signed copy: %d", s)
	}

	// The payroll run holds the salary while it is pending.
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := str(state["openYm"])
	c.pcCloseAll(t, open) // the signed copy waiting for the manager is kept on hold in Deduction closure
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("dry run %d %v", s, r)
	}
	holdOf := func(e string) (string, string, bool) {
		h := apiDoc(st, "salary_hold", bson.M{"employee_id": e, "status": "Active", "source_id": "PACK"})
		rowHold, _ := apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": e})["hold"].(bool)
		return str(h["id"]), str(h["reason"]), rowHold
	}
	if id, reason, rowHold := holdOf(emp); id == "" || reason != "Company policy not acknowledged: Code of conduct" || !rowHold {
		t.Fatalf("hold %q %q row %v", id, reason, rowHold)
	}
	_, ck := c.do("GET", "/api/payroll/"+open+"/checks", "EMP-1011", "Payroll", nil)
	found := false
	for _, x := range ck["list"].([]any) {
		if x := x.(map[string]any); x["title"] == "Company policies acknowledged" {
			found = x["ok"] == false && strings.Contains(str(x["detail"]), "held")
		}
	}
	if !found {
		t.Fatalf("run check %v", ck)
	}
	_, f := c.do("GET", "/api/employees/"+emp+"/file", hr, hrRole, nil)
	found = false
	for _, x := range f["items"].([]any) {
		if x := x.(map[string]any); x["key"] == "policies" {
			found = x["state"] == "hold" && strings.Contains(str(x["text"]), "1 pending, with Anil Kapoor")
		}
	}
	if !found {
		t.Fatalf("readiness %v", f["items"])
	}
	_, dash := c.do("GET", "/api/dashboard", hr, hrRole, nil)
	if !strings.Contains(fmt.Sprint(dash["alerts"]), "have company policies pending") {
		t.Fatalf("dashboard alert %v", dash["alerts"])
	}

	// Returned with a comment, sent again, approved: the hold is released and the run pays.
	if s, _ := c.do("POST", "/api/approvals/"+req+"/reject", mgr, "Line Manager", map[string]any{}); s != 400 {
		t.Fatal("returning needs a comment")
	}
	if s, r := c.do("POST", "/api/approvals/"+req+"/reject", mgr, "Line Manager", map[string]any{"note": "Sign every page"}); s != 200 {
		t.Fatalf("return %d %v", s, r)
	}
	_, m = c.do("GET", "/api/me/policies", emp, "Employee", nil)
	it = m["items"].([]any)[0].(map[string]any)
	if it["status"] != "Returned" || it["comment"] != "Sign every page" || it["comment_by"] != "Anil Kapoor" {
		t.Fatalf("returned %v", it)
	}
	_, signed2 := c.upload(emp, "Employee", "policy_ack", emp+"/"+ver, "signed")
	_, r = c.do("POST", "/api/me/policies/"+ver+"/submit", emp, "Employee", map[string]any{"docId": signed2["id"], "note": "All pages signed"})
	req = str(r["id"])
	if s, r := c.do("POST", "/api/approvals/"+req+"/approve", mgr, "Line Manager", map[string]any{}); s != 200 || r["status"] != "Approved" {
		t.Fatalf("approve %d %v", s, r)
	}
	if id, _, _ := holdOf(emp); id != "" {
		t.Fatal("hold not released on approval")
	}
	_, d := c.do("GET", "/api/approvals/"+req, emp, "Employee", nil)
	if !strings.Contains(fmt.Sprint(d["history"]), "salary hold is released") {
		t.Fatalf("history %v", d["history"])
	}
	c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil)
	if _, _, rowHold := holdOf(emp); rowHold {
		t.Fatal("still held in the recalculated run")
	}
	if id, _, rowHold := holdOf("EMP-1014"); id == "" || !rowHold {
		t.Fatal("others with the policy pending stay held")
	}

	// An HRBP approves on behalf of the manager; recorded as such.
	_, m = c.do("GET", "/api/me/policies", "EMP-1014", "Employee", nil)
	ver14 := fmt.Sprint(num(m["items"].([]any)[0].(map[string]any)["version_id"]))
	_, s14 := c.upload("EMP-1014", "Employee", "policy_ack", "EMP-1014/"+ver14, "signed")
	_, r = c.do("POST", "/api/me/policies/"+ver14+"/submit", "EMP-1014", "Employee", map[string]any{"docId": s14["id"]})
	req14 := str(r["id"])
	if s, r := c.do("POST", "/api/approvals/"+req14+"/approve", "EMP-1004", "HRBP", map[string]any{}); s != 200 || r["status"] != "Approved" {
		t.Fatalf("HRBP approve %d %v", s, r)
	}
	_, d = c.do("GET", "/api/approvals/"+req14, hr, hrRole, nil)
	if !strings.Contains(fmt.Sprint(d["history"]), "(HRBP, on behalf of Line Manager)") {
		t.Fatalf("HRBP history %v", d["history"])
	}

	// A month already approved with the salary held: the approval pays the held month in the open month.
	_, m = c.do("GET", "/api/me/policies", "EMP-1021", "Employee", nil)
	ver21 := fmt.Sprint(num(m["items"].([]any)[0].(map[string]any)["version_id"]))
	row21 := apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": "EMP-1021"})
	held21, _ := row21["hold"].(bool)
	net21 := apiFloat(row21["net"])
	if !held21 || net21 <= 0 {
		t.Fatalf("EMP-1021 held %v net %v", held21, net21)
	}
	if _, err := st.DB.C("payroll_month").UpdateMany(ctx, bson.M{"ym": open}, bson.M{"$set": bson.M{"status": "Approved"}}); err != nil {
		t.Fatal(err)
	}
	_, s21 := c.upload("EMP-1021", "Employee", "policy_ack", "EMP-1021/"+ver21, "signed")
	_, r = c.do("POST", "/api/me/policies/"+ver21+"/submit", "EMP-1021", "Employee", map[string]any{"docId": s21["id"]})
	mgr21 := str(apiDoc(st, "employee", bson.M{"_id": "EMP-1021"})["manager_id"])
	if s, r := c.do("POST", "/api/approvals/"+str(r["id"])+"/approve", hr, hrRole, map[string]any{"note": "Checked the signed copy"}); s != 200 || r["status"] != "Approved" {
		t.Fatalf("approve 1021 %d %v (manager %s)", s, r, mgr21)
	}
	released := apiDoc(st, "ledger", bson.M{"employee_id": "EMP-1021", "label": bson.M{"$regex": "^Held salary released", "$options": "s"}, "void": false})
	label, amt := str(released["label"]), apiFloat(released["amount"])
	if label == "" || amt != float64(int(net21*100+0.5))/100 { // the stored net to the paisa (ITM-59)
		t.Fatalf("held month paid: %q %v (net %v)", label, amt, net21)
	}
	if _, err := st.DB.C("payroll_month").UpdateMany(ctx, bson.M{"ym": open}, bson.M{"$set": bson.M{"status": "Dry run"}}); err != nil {
		t.Fatal(err)
	}

	// Report.
	_, rep := c.do("GET", "/api/reports/policy_ack?ds=Signed", hr, hrRole, nil)
	if fmt.Sprint(rep["cols"]) != "[Code Name Department Location Policy Version Status Submitted on Approved by Approved on]" {
		t.Fatalf("report cols %v", rep["cols"])
	}
	if rows, _ := rep["rows"].([]any); len(rows) != 3 {
		t.Fatalf("signed rows %v", rep["rows"])
	}

	// A new version is due again and cancels copies of the old one; retiring ends it and frees the holds.
	_, sub := c.do("GET", "/api/me/policies", "EMP-1014", "Employee", nil)
	if sub["items"].([]any)[0].(map[string]any)["status"] != "Signed" {
		t.Fatal("EMP-1014 signed")
	}
	_, mv := c.do("GET", "/api/me/policies", "EMP-1009", "Line Manager", nil)
	ver9 := fmt.Sprint(num(mv["items"].([]any)[0].(map[string]any)["version_id"]))
	_, s9 := c.upload(mgr, "Line Manager", "policy_ack", mgr+"/"+ver9, "signed")
	_, r = c.do("POST", "/api/me/policies/"+ver9+"/submit", mgr, "Line Manager", map[string]any{"docId": s9["id"]})
	req9 := str(r["id"])
	_, doc2 := c.upload(hr, hrRole, "policy", "DRAFT-0123456789ef", "document")
	if s, r := c.do("POST", "/api/company-policies/"+pid+"/version", hr, hrRole, map[string]any{"version": "1.0", "effectiveFrom": "2026-02-01", "docId": doc2["id"]}); s != 400 {
		t.Fatalf("same version twice: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/company-policies/"+pid+"/version", hr, hrRole, map[string]any{"version": "2.0", "effectiveFrom": "2026-02-01", "docId": doc2["id"], "note": "New section on guests"}); s != 200 {
		t.Fatalf("version %d %v", s, r)
	}
	_, sub = c.do("GET", "/api/me/policies", "EMP-1014", "Employee", nil)
	if x := sub["items"].([]any)[0].(map[string]any); x["status"] != "Not submitted" || x["version"] != "2.0" {
		t.Fatalf("due again %v", x)
	}
	_, d = c.do("GET", "/api/approvals/"+req9, hr, hrRole, nil)
	if d["status"] != "Cancelled" {
		t.Fatalf("old copy not cancelled: %v", d["status"])
	}
	if s, _ := c.do("POST", "/api/company-policies/"+pid+"/retire", hr, hrRole, map[string]any{"note": "x"}); s != 400 {
		t.Fatal("retiring needs a reason")
	}
	if s, r := c.do("POST", "/api/company-policies/"+pid+"/retire", hr, hrRole, map[string]any{"note": "Replaced by the handbook"}); s != 200 {
		t.Fatalf("retire %d %v", s, r)
	}
	left := apiCount(st, "salary_hold", bson.M{"status": "Active", "source_id": "PACK"})
	if left != 0 {
		t.Fatalf("%d holds left after retiring", left)
	}
	if _, m := c.do("GET", "/api/me/policies", emp, "Employee", nil); num(m["pending"]) != 0 || len(m["items"].([]any)) != 0 {
		t.Fatalf("retired policy still due: %v", m)
	}
}
