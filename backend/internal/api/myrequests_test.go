package api_test

import (
	"fmt"
	"strings"
	"testing"
)

// My view without Approvals: the employee follows every request about them from the Dashboard
// (GET /api/me/requests?wf=all), sees who each one waits on, and cancels only their own kinds. Requests that
// Payroll or HR raise about them (a deduction here) are listed but cannot be cancelled by them.
func TestMyRequestsList(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1018", "EMP-1020")
	raise := func(emp, role string, body map[string]any) string {
		t.Helper()
		s, r := c.do("POST", "/api/approvals", emp, role, body)
		id, _ := r["id"].(string)
		if s != 200 || id == "" {
			t.Fatalf("%s raising %v: %d %v", emp, body["wf"], s, r)
		}
		return id
	}
	list := func(emp string) map[string]map[string]any {
		t.Helper()
		s, r := c.do("GET", "/api/me/requests?wf=all", emp, "Employee", nil)
		if s != 200 {
			t.Fatalf("my requests: %d %v", s, r)
		}
		out := map[string]map[string]any{}
		l, _ := r["list"].([]any)
		for _, x := range l {
			m := x.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out
	}
	_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Bill")
	claim := raise("EMP-1020", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 750, "claimType": "Travel", "billDate": today(), "doc": doc["id"]}})
	change := raise("EMP-1020", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Personal", "mobile": "9876543210"}})
	ded := raise("EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
		"payload": map[string]any{"amount": 1500, "instalments": 1, "dedType": "Cash shortage", "note": "Till short on Monday", "heard": "Yes"}})
	_, doc2 := c.upload("EMP-1018", "Employee", "employee", "EMP-1018", "Bill")
	other := raise("EMP-1018", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 300, "claimType": "Travel", "billDate": today(), "doc": doc2["id"]}})

	mine := list("EMP-1020")
	for _, id := range []string{claim, change, ded} {
		r := mine[id]
		if r == nil {
			t.Fatalf("%s missing from My requests: %v", id, mine)
		}
		if r["status"] != "Pending" || r["waiting_on"] == nil || r["waiting_name"] == nil || r["stage_since"] == nil {
			t.Fatalf("%s shows who it waits on and since when: %v", id, r)
		}
	}
	if mine[other] != nil {
		t.Fatal("another employee's request is not in My requests")
	}
	if list("EMP-1018")[claim] != nil {
		t.Fatal("my claim is not in a colleague's list")
	}
	if mine[claim]["canCancel"] != true || mine[change]["canCancel"] != true {
		t.Fatalf("own claim and change can be cancelled: %v %v", mine[claim]["canCancel"], mine[change]["canCancel"])
	}
	if mine[ded]["canCancel"] != false || mine[ded]["raised_by"] != "EMP-1011" || fmt.Sprint(mine[ded]["raised_by_name"]) == "" || mine[ded]["raised_by_name"] == "EMP-1011" {
		t.Fatalf("Payroll's deduction is listed with who raised it, without Cancel: %v", mine[ded])
	}
	for _, r := range mine {
		if r["wf"] == "joining" || r["wf"] == "verify" {
			t.Fatalf("onboarding approvals are left out: %v", r)
		}
	}

	// The Dashboard card counts the same requests and its Needs attention item opens the list.
	_, ov := c.do("GET", "/api/me/dashboard?tab=ov", "EMP-1020", "Employee", nil)
	pend := 0
	for _, r := range mine {
		if r["status"] == "Pending" {
			pend++
		}
	}
	if ov["pending"] != float64(pend) {
		t.Fatalf("My requests waiting %v, list has %d pending", ov["pending"], pend)
	}
	if a := fmt.Sprint(ov["alerts"]); !strings.Contains(a, "/#my-requests") || strings.Contains(a, "/approvals") {
		t.Fatalf("the My requests alert opens the Dashboard list, not Approvals: %v", a)
	}

	// The request detail (read-only in My view) says whether the employee may cancel it.
	if _, d := c.do("GET", "/api/approvals/"+ded, "EMP-1020", "Employee", nil); d["canCancel"] != false || d["canAct"] != false {
		t.Fatalf("deduction detail: canCancel %v canAct %v", d["canCancel"], d["canAct"])
	}
	if _, d := c.do("GET", "/api/approvals/"+claim, "EMP-1020", "Employee", nil); d["canCancel"] != true {
		t.Fatalf("claim detail: canCancel %v", d["canCancel"])
	}

	// Cancel: never a deduction raised about you, in any role view; your own requests, yes.
	if s, r := c.do("POST", "/api/approvals/"+ded+"/cancel", "EMP-1020", "Employee", nil); s != 403 || !strings.Contains(fmt.Sprint(r["error"]), "raised by") {
		t.Fatalf("an employee cannot cancel Payroll's deduction: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals/"+claim+"/cancel", "EMP-1020", "Employee", nil); s != 200 {
		t.Fatalf("own claim cancelled: %d %v", s, r)
	}
	if r := list("EMP-1020")[claim]; r["status"] != "Cancelled" || r["canCancel"] != false {
		t.Fatalf("cancelled claim: %v", r)
	}
	// HR Admin cancels anyone else's request, but a deduction about themselves follows the employee's rule.
	own := raise("EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1002",
		"payload": map[string]any{"amount": 900, "instalments": 1, "dedType": "Cash shortage", "note": "Till short", "heard": "Yes"}})
	if s, _ := c.do("POST", "/api/approvals/"+own+"/cancel", "EMP-1002", "HR Admin", nil); s != 403 {
		t.Fatalf("HR Admin cannot cancel a deduction about themselves: %d", s)
	}
	if s, r := c.do("POST", "/api/approvals/"+ded+"/cancel", "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("HR Admin cancels another employee's deduction: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals/"+own+"/cancel", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("whoever raised it can cancel it: %d %v", s, r)
	}

	// The old forms still work: a workflow is still needed, and wf=all is only ever the caller's own.
	if s, _ := c.do("GET", "/api/me/requests", "EMP-1020", "Employee", nil); s != 400 {
		t.Fatal("a workflow or all is needed")
	}
	if s, _ := c.do("GET", "/api/me/requests?wf=all&employeeId=EMP-1018", "EMP-1020", "Employee", nil); s != 200 || list("EMP-1020")[other] != nil {
		t.Fatal("an employee parameter is ignored")
	}
}

// A resignation started in My view › Separation is listed with its route, and the employee withdraws it before
// approval by cancelling it (the separation case shows Withdrawn).
func TestMyRequestsResignation(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1020")
	_, sep := c.do("GET", "/api/me/separation", "EMP-1020", "Employee", nil)
	reasons, _ := sep["reasons"].([]any)
	if len(reasons) == 0 {
		t.Fatalf("exit reasons: %v", sep)
	}
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": reasons[0], "note": "Moving cities"}})
	if s != 200 {
		t.Fatalf("resign: %d %v", s, r)
	}
	id := r["id"].(string)
	_, l := c.do("GET", "/api/me/requests?wf=all", "EMP-1020", "Employee", nil)
	var row map[string]any
	for _, x := range l["list"].([]any) {
		if m := x.(map[string]any); m["id"] == id {
			row = m
		}
	}
	if row == nil || row["canCancel"] != true || row["waiting_on"] == nil || len(row["chain"].([]any)) == 0 {
		t.Fatalf("resignation listed with its route and Cancel: %v", row)
	}
	if s, r := c.do("POST", "/api/approvals/"+id+"/cancel", "EMP-1020", "Employee", nil); s != 200 {
		t.Fatalf("withdraw before approval: %d %v", s, r)
	}
	_, sep = c.do("GET", "/api/me/separation", "EMP-1020", "Employee", nil)
	if cs, _ := sep["case"].(map[string]any); cs == nil || cs["stage"] != "Withdrawn" {
		t.Fatalf("case after withdrawing: %v", sep["case"])
	}
}
