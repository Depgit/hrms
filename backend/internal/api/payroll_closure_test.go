package api_test

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// The payroll run checklist (payroll_closure.go) and in-app messages (notices.go): five closures, pending items
// split into Not raised and Waiting for approval, Close only when nothing is pending, the dry run refused until
// all five are closed, reopening by the Payroll Manager with a reason, and messages to whom an item waits on.

// pcCloseAll makes nothing pending in month ym and closes its five closures as Payroll, the way a test that runs
// payroll needs. There is no shortcut in the app, so each item is really cleared: waiting regularizations and leave
// are rejected at the cut-off (as Month close did), waiting claims, deductions and bank changes are decided by their
// approvers, and the days nobody raised get what an employee would raise, seeded the way other tests seed data
// (the test database has no punches, so every working day is a no-punch day): a missing punch gets a regularization
// that was rejected, and absences an approved loss-of-pay leave for exactly those days. Pay stays as it was: those
// days were loss of pay already.
func (c *client) pcCloseAll(t *testing.T, ym string) {
	t.Helper()
	keys := []string{"attendance", "leave", "bank", "claim", "deduction"}
	c.pcClear(t, ym, keys...)
	for _, k := range keys {
		if s, r := c.do("POST", "/api/payroll/"+ym+"/closures/"+k+"/close", "EMP-1011", "Payroll", nil); s != 200 && !strings.Contains(fmt.Sprint(r), "already closed") {
			t.Fatalf("close %s %s: %v", k, ym, r)
		}
	}
}

// pcClear makes nothing pending in the named closures of month ym (see pcCloseAll), without closing them.
func (c *client) pcClear(t *testing.T, ym string, keys ...string) {
	t.Helper()
	ctx := context.Background()
	want := func(k string) bool {
		for _, x := range keys {
			if x == k {
				return true
			}
		}
		return false
	}
	get := func() map[string]any {
		t.Helper()
		s, x := c.do("GET", "/api/payroll/"+ym+"/checklist", "EMP-1011", "Payroll", nil)
		if s != 200 {
			t.Fatalf("checklist %s: %v", ym, x)
		}
		return x
	}
	for _, k := range []string{"attendance", "leave"} {
		if want(k) {
			c.do("POST", "/api/payroll/"+ym+"/closures/"+k+"/reject-waiting", "EMP-1011", "Payroll", map[string]any{"all": true})
		}
	}
	x := get()
	for _, k := range []string{"bank", "claim", "deduction"} {
		if cl := pcClosure(x, k); want(k) && cl["status"] != "Closed" {
			for _, it := range cl["items"].([]any) {
				if id, _ := it.(map[string]any)["requestId"].(string); id != "" {
					c.approveAll(t, id)
				}
			}
		}
	}
	x = get()
	absent := map[string][]string{}
	for _, k := range []string{"attendance", "leave", "bank"} {
		cl := pcClosure(x, k)
		if !want(k) || cl["status"] == "Closed" {
			continue
		}
		for _, it := range cl["items"].([]any) {
			m := it.(map[string]any)
			emp, d := m["employeeId"].(string), fmt.Sprint(m["date"])
			switch {
			case m["group"] != "Not raised":
			case k == "bank":
				if _, err := c.st.DB.C("employee_bank").Insert(ctx, bson.M{"employee_id": emp, "bank_name": "Test Bank", "account_no": "123456789012", "ifsc": "TEST0000001",
					"from_month": ym, "created_by": "Test seed"}); err != nil {
					t.Fatal(err)
				}
			case strings.HasPrefix(m["what"].(string), "Missing punch"):
				pcSeed(t, c, "regularize", emp, "Rejected", map[string]any{"wf": "regularize"}, map[string]any{"date": d, "rtype": "All Other", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"})
			default:
				absent[emp] = append(absent[emp], d)
			}
		}
	}
	for emp, days := range absent {
		sort.Strings(days)
		pcSeed(t, c, "leave", emp, "Approved", map[string]any{"wf": "leave", "days": len(days)}, map[string]any{"ltype": "Loss of pay", "from": days[0], "to": days[len(days)-1], "cov": days})
	}
}

// pcSeed stores a request already decided, as if the employee had raised it and the approver had decided it.
func pcSeed(t *testing.T, c *client, wf, emp, status string, facts, payload map[string]any) {
	t.Helper()
	ctx := context.Background()
	n, err := store.NextSeq(ctx, c.st.DB, "approval_request_seq")
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%s-%d", map[string]string{"leave": "LV", "regularize": "AR"}[wf], n)
	if _, err := c.st.DB.C("approval_request").Insert(ctx, bson.M{"_id": id, "wf": wf, "employee_id": emp, "raised_by": emp, "flow": "Operations",
		"facts": facts, "payload": payload, "chain": []string{"Line Manager"}, "step": 1, "status": status}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.DB.C("approval_history").Insert(ctx, bson.M{"request_id": id, "stage": "Line Manager", "action": status, "by_name": "Test seed",
		"note": "Seeded for the payroll checklist"}); err != nil {
		t.Fatal(err)
	}
}

// pcDecide approves a request stage by stage as the person each stage waits on (from the checklist), in that
// person's role view: the right approver decides.
func pcDecide(t *testing.T, c *client, ym, key, id string) {
	t.Helper()
	for i := 0; i < 8; i++ {
		_, x := c.do("GET", "/api/payroll/"+ym+"/checklist", "EMP-1011", "Payroll", nil)
		var it map[string]any
		for _, v := range pcClosure(x, key)["items"].([]any) {
			if m := v.(map[string]any); m["requestId"] == id {
				it = m
			}
		}
		if it == nil {
			return // decided
		}
		who, as := it["waitsOnId"].(string), it["waitsAs"].(string)
		if c.tokens[who] == "" {
			c.signIn(who)
		}
		if s, r := c.do("POST", "/api/approvals/"+id+"/approve", who, as, map[string]string{"note": "Checked"}); s != 200 {
			t.Fatalf("%s approving %s as %s: %v", who, id, as, r)
		}
	}
	t.Fatalf("%s never decided", id)
}

func pcClosure(x map[string]any, key string) map[string]any {
	for _, c := range x["closures"].([]any) {
		if m := c.(map[string]any); m["key"] == key {
			return m
		}
	}
	return nil
}

func pcItem(cl map[string]any, ref string) map[string]any {
	for _, it := range cl["items"].([]any) {
		if m := it.(map[string]any); m["ref"] == ref {
			return m
		}
	}
	return nil
}

func pcNotices(t *testing.T, c *client, emp string) []map[string]any {
	t.Helper()
	s, r := c.do("GET", "/api/notices", emp, "Employee", nil)
	if s != 200 {
		t.Fatalf("notices for %s: %v", emp, r)
	}
	out := []map[string]any{}
	for _, n := range r["notices"].([]any) {
		out = append(out, n.(map[string]any))
	}
	return out
}

func TestPayrollChecklist(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1013", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	P := func(path string, body any) (int, map[string]any) {
		return c.do("POST", "/api/payroll/"+open+path, "EMP-1011", "Payroll", body)
	}
	get := func() map[string]any {
		t.Helper()
		s, x := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil)
		if s != 200 {
			t.Fatalf("checklist: %v", x)
		}
		return x
	}

	// Rights: viewing needs pay.run 1, closing pay.run 2.
	if s, _ := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatal("an employee cannot see the checklist")
	}
	if s, _ := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1013", "Finance Head", nil); s != 200 {
		t.Fatal("pay.run 1 sees the checklist")
	}
	if s, _ := c.do("POST", "/api/payroll/"+open+"/closures/bank/close", "EMP-1013", "Finance Head", nil); s != 403 {
		t.Fatal("closing needs pay.run 2")
	}

	// Five closures, none closed: the dry run is refused, naming what is open.
	x := get()
	keys := []string{}
	for _, cl := range x["closures"].([]any) {
		keys = append(keys, cl.(map[string]any)["key"].(string))
	}
	if strings.Join(keys, ",") != "attendance,leave,bank,claim,deduction" || x["allClosed"] != false {
		t.Fatalf("closures %v %v", keys, x["allClosed"])
	}
	if s, r := P("/dry-run", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "Still open: Attendance closure, Leave closure, Bank verification closure, Reimbursement closure, Deduction closure") {
		t.Fatalf("dry run before the checklist: %d %v", s, r)
	}

	// With no punches on record, a working day is a no-punch day nobody raised: listed once, under Attendance.
	day := dayIn(open, time.Thursday, 2)
	dayRef := open + ":attendance:EMP-1020/" + day
	it := pcItem(pcClosure(x, "attendance"), dayRef)
	if it == nil || it["group"] != "Not raised" || it["waitsOnId"] != "EMP-1020" || !strings.Contains(fmt.Sprint(it["note"]), "Listed once") {
		t.Fatalf("no-punch day %v", it)
	}
	if pcItem(pcClosure(x, "leave"), open+":leave:EMP-1020/"+day) != nil {
		t.Fatal("the same absence is not listed under Leave too")
	}
	if pcClosure(x, "attendance")["status"] != "Pending" || pcClosure(x, "bank")["status"] != "Done" {
		t.Fatalf("statuses %v %v", pcClosure(x, "attendance")["status"], pcClosure(x, "bank")["status"])
	}

	// A regularization raised for the day waits on the line manager.
	c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "kind": "regularize", "from": day, "to": day, "until": today(), "reason": "Biometric fault"})
	_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]any{"date": day, "rtype": "No Punch", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}})
	reg, _ := r["id"].(string)
	if reg == "" {
		t.Fatal(r)
	}
	regRef := open + ":attendance:" + reg
	x = get()
	if it := pcItem(pcClosure(x, "attendance"), regRef); it == nil || it["group"] != "Waiting for approval" || it["waitsOnId"] != "EMP-1009" || pcItem(pcClosure(x, "attendance"), dayRef) != nil {
		t.Fatalf("waiting regularization %v", it)
	}

	// A message to the approver: it shows under their bell, in any view, and on the checklist.
	if s, r := P("/closures/attendance/message", map[string]any{"messages": []any{map[string]any{"to": "EMP-1012", "refs": []string{regRef}, "text": "Please decide"}}}); s != 400 {
		t.Fatalf("a message goes to whom the item waits on: %v", r)
	}
	if s, r := P("/closures/attendance/message", map[string]any{"messages": []any{map[string]any{"refs": []string{regRef}, "text": "Payroll is waiting on your approval of " + reg}}}); s != 200 || r["sent"] != 1.0 {
		t.Fatalf("message: %d %v", s, r)
	}
	ns := pcNotices(t, c, "EMP-1009")
	if len(ns) != 1 || ns[0]["link"] != "/approvals?id="+reg || ns[0]["view"] != "Line Manager" || ns[0]["from_name"] != "Sunita Rao" || ns[0]["read"] != false {
		t.Fatalf("approver's message %v", ns)
	}
	if it := pcItem(pcClosure(get(), "attendance"), regRef); !strings.HasPrefix(fmt.Sprint(it["messaged"]), "Anil Kapoor, ") {
		t.Fatalf("messaged %v", it["messaged"])
	}
	// audited counts the audit entries whose action matches a LIKE pattern: the text before and after its one %.
	audited := func(before, after string) int64 {
		re := "^" + regexp.QuoteMeta(before) + ".*" + regexp.QuoteMeta(after) + "$"
		n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"action": bson.M{"$regex": re, "$options": "s"}})
		return n
	}
	audits := audited("Sent a payroll message to Anil Kapoor", "")
	if audits != 1 {
		t.Fatal("sending is audited")
	}
	nid := fmt.Sprint(ns[0]["id"])
	if s, _ := c.do("POST", "/api/notices/"+nid+"/read", "EMP-1020", "Employee", nil); s != 404 {
		t.Fatal("only the recipient reads a message")
	}
	if s, _ := c.do("POST", "/api/notices/"+nid+"/read", "EMP-1009", "Employee", nil); s != 200 {
		t.Fatal("read")
	}
	if _, r := c.do("GET", "/api/notices", "EMP-1009", "Line Manager", nil); r["unread"] != 0.0 || r["open"] != 1.0 {
		t.Fatalf("after reading %v", r)
	}

	// Rejected at the cut-off: the message is done, and the day is an absence with no leave, under Leave.
	if s, r := P("/closures/attendance/reject-waiting", map[string]any{"all": true}); s != 200 || r["rejected"] != 1.0 {
		t.Fatalf("reject at cut-off: %v", r)
	}
	if ns := pcNotices(t, c, "EMP-1009"); ns[0]["done"] != true {
		t.Fatalf("done once decided %v", ns)
	}
	x = get()
	leaveRef := open + ":leave:EMP-1020/" + day
	if it := pcItem(pcClosure(x, "leave"), leaveRef); it == nil || it["group"] != "Not raised" || it["waitsOnId"] != "EMP-1020" || pcItem(pcClosure(x, "attendance"), dayRef) != nil {
		t.Fatalf("absence under Leave %v", it)
	}

	// The employee is asked to apply for leave; the message is done once they do, and the leave waits on the manager.
	if s, r := P("/closures/leave/message", map[string]any{"messages": []any{map[string]any{"refs": []string{leaveRef}, "text": "Please apply for leave"}}}); s != 200 {
		t.Fatal(r)
	}
	if ns := pcNotices(t, c, "EMP-1020"); len(ns) != 1 || ns[0]["view"] != "Employee" || ns[0]["link"] != "/me?tab=leave" || ns[0]["done"] != false {
		t.Fatalf("employee's message %v", ns)
	}
	c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "kind": "leave", "from": day, "to": day, "until": today(), "reason": "Late application"})
	_, r = c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", map[string]any{"wf": "leave", "employeeId": "EMP-1020", "payload": map[string]any{"ltype": "Loss of pay", "from": day, "to": day, "scoreOverride": "Payroll checklist test"}})
	lv, _ := r["id"].(string)
	if lv == "" {
		t.Fatal(r)
	}
	if ns := pcNotices(t, c, "EMP-1020"); ns[0]["done"] != true {
		t.Fatalf("done once raised %v", ns)
	}
	x = get()
	if it := pcItem(pcClosure(x, "leave"), open+":leave:"+lv); it == nil || it["group"] != "Waiting for approval" || it["waitsOnId"] == "" {
		t.Fatalf("leave waiting %v", it)
	}
	if s, r := P("/closures/leave/close", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "pending") {
		t.Fatalf("close with items pending: %v", r)
	}
	// There is no way to set an item aside: the leave clears only when its approvers decide it.
	if s, _ := P("/closures/leave/settle", map[string]any{"all": true, "note": "Set aside"}); s != 404 {
		t.Fatal("settling is not an action")
	}
	pcDecide(t, c, open, "leave", lv)
	if cl := pcClosure(get(), "leave"); cl["status"] != "Done" || cl["pending"] != 0.0 {
		t.Fatalf("leave closure after the leave is decided %v %v", cl["status"], cl["pending"])
	}
	if s, r := P("/closures/leave/close", nil); s != 200 {
		t.Fatalf("close leave: %v", r)
	}
	other := dayIn(open, time.Wednesday, 3)
	if s, r := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", map[string]any{"wf": "leave", "employeeId": "EMP-1020", "payload": map[string]any{"ltype": "Loss of pay", "from": other, "to": other, "scoreOverride": "x"}}); s != 400 || !strings.Contains(fmt.Sprint(r), "is closed by Payroll") {
		t.Fatalf("leave after Leave closure: %d %v", s, r)
	}

	// A bank change waits on Payroll, who rejects it inline as its approver.
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Bank", "bankName": "HDFC Bank", "accountNo": "50100012345678", "ifsc": "HDFC0001234"}}); s != 200 {
		t.Fatal(r)
	} else {
		bankRef := open + ":bank:" + r["id"].(string)
		if it := pcItem(pcClosure(get(), "bank"), bankRef); it == nil || it["waitsOnId"] != "EMP-1011" || it["canVerify"] != true {
			t.Fatalf("bank change %v", it)
		}
		if s, r := c.do("POST", "/api/approvals/"+r["id"].(string)+"/reject", "EMP-1011", "Payroll", map[string]string{"note": "Account name does not match"}); s != 200 {
			t.Fatal(r)
		}
		if pcItem(pcClosure(get(), "bank"), bankRef) != nil {
			t.Fatal("a rejected bank change is no longer pending")
		}
	}

	// A claim waiting for approval keeps Reimbursement open until it is decided; one approved after the close is paid next month.
	claim := func() string {
		_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Bill") // one bill per claim: a bill is never claimed twice
		_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 900, "claimType": "Travel", "billDate": today(), "doc": doc["id"]}})
		id, _ := r["id"].(string)
		if id == "" {
			t.Fatal(r)
		}
		return id
	}
	paidIn := func(id string) (ym string) {
		if l, _ := st.DB.C("ledger").FindOne(ctx, bson.M{"source_id": id}); l != nil {
			ym, _ = l["ym"].(string)
		}
		return
	}
	clm := claim()
	if it := pcItem(pcClosure(get(), "claim"), open+":claim:"+clm); it == nil || it["waitsOnId"] != "EMP-1009" || num(it["amount"]) != 900 {
		t.Fatalf("claim item %v", it)
	}
	if s, r := P("/closures/claim/close", nil); s != 400 {
		t.Fatalf("claim closure with a claim waiting: %v", r)
	}
	pcDecide(t, c, open, "claim", clm)
	if ym := paidIn(clm); ym != open {
		t.Fatalf("claim approved before the close is paid in %q, want %s", ym, open)
	}
	if s, r := P("/closures/claim/close", nil); s != 200 {
		t.Fatalf("close claims: %v", r)
	}
	late := claim()
	c.approveAll(t, late)
	if ym := paidIn(late); ym != nextYm(open) {
		t.Fatalf("claim approved after the close is paid in %q, want %s", ym, nextYm(open))
	}

	// Everything else cleared and closed: attendance is locked, and a regularization for the cycle is refused.
	c.pcCloseAll(t, open)
	x = get()
	if x["allClosed"] != true || pcClosure(x, "attendance")["status"] != "Closed" || pcClosure(x, "attendance")["closedBy"] != "Sunita Rao" {
		t.Fatalf("all closed %v", x["open"])
	}
	isLocked := func() bool {
		m, _ := st.DB.C("payroll_month").FindOne(ctx, bson.M{"ym": open})
		return m != nil && m["locked_at"] != nil
	}
	locked := isLocked()
	if !locked {
		t.Fatal("closing Attendance locks attendance")
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "regularize", "payload": map[string]any{"date": other, "rtype": "No Punch", "in": "09:00", "out": "18:00", "reason": "Forgot to punch"}}); s != 400 || !strings.Contains(fmt.Sprint(r), "locked by payroll") {
		t.Fatalf("regularization after Attendance closure: %d %v", s, r)
	}

	// Reopening: not Payroll; the Payroll Manager with a reason, audited; the dry run waits until it is closed again.
	if s, _ := c.do("POST", "/api/payroll/"+open+"/closures/attendance/reopen", "EMP-1011", "Payroll", map[string]any{"reason": "Late punches arrived"}); s != 403 {
		t.Fatal("Payroll cannot reopen")
	}
	if s, _ := c.do("POST", "/api/payroll/"+open+"/closures/attendance/reopen", "EMP-1012", "Payroll Manager", map[string]any{"reason": ""}); s != 400 {
		t.Fatal("reopening needs a reason")
	}
	if s, r := c.do("POST", "/api/payroll/"+open+"/closures/attendance/reopen", "EMP-1012", "Payroll Manager", map[string]any{"reason": "Late punches arrived"}); s != 200 {
		t.Fatal(r)
	}
	locked = isLocked()
	audits = audited("Reopened attendance closure for payroll ", ": Late punches arrived")
	if locked || audits != 1 {
		t.Fatalf("reopen unlocks (%v) and is audited (%d)", locked, audits)
	}
	if ro := pcClosure(get(), "attendance")["reopened"].(map[string]any); ro["by"] != "Deepak Arora" {
		t.Fatalf("reopened %v", ro)
	}
	if s, r := P("/dry-run", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "Still open: Attendance closure") {
		t.Fatalf("dry run with Attendance reopened: %v", r)
	}
	c.pcCloseAll(t, open)
	if s, r := P("/dry-run", nil); s != 200 || r["status"] != "Dry run" {
		t.Fatalf("dry run with all five closed: %v", r)
	}
	if s, r := P("/verify", nil); s != 200 {
		t.Fatal(r)
	}
	if s, r := c.do("POST", "/api/payroll/"+open+"/closures/leave/reopen", "EMP-1012", "Payroll Manager", map[string]any{"reason": "A leave came in late"}); s != 400 || !strings.Contains(fmt.Sprint(r), "back to the dry run") {
		t.Fatalf("no reopening once verified: %v", r)
	}

	// Dismissed from the Dashboard: gone from the open list, still listed (dismissed) under the bell.
	ns = pcNotices(t, c, "EMP-1020")
	if s, _ := c.do("POST", fmt.Sprintf("/api/notices/%v/dismiss", ns[0]["id"]), "EMP-1020", "Employee", nil); s != 200 {
		t.Fatal("dismiss")
	}
	if ns = pcNotices(t, c, "EMP-1020"); ns[0]["dismissed"] != true {
		t.Fatalf("dismissed %v", ns)
	}
}
