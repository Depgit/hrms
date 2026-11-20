package api_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/policy"
)

// My view, part A: the tax regime is chosen once a financial year (locked once saved, Payroll or HR Admin can
// unlock it with a reason), and a work-policy switch for employees' own reimbursement claims.

func TestMyViewTaxRegimeOncePerYear(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1011", "EMP-1020"} {
		c.signIn(e)
	}
	_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	fy := tax["fy"].(string)
	y, _ := strconv.Atoi(fy[:4])
	next := fmt.Sprintf("%d-%02d", y+1, (y+2)%100)
	lk := tax["regimeLock"].(map[string]any)
	if lk["locked"] != false || lk["nextFrom"] != fmt.Sprintf("%d-04-01", y+1) {
		t.Fatalf("no choice made yet, open until next April: %v", lk)
	}
	w := tax["windows"].(map[string]any)
	win := map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "declForce": true, "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": true}
	if s, r := c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win); s != 200 {
		t.Fatal(r)
	}

	// The first save is the choice; after it the regime cannot change for the year.
	if s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", map[string]any{"regime": "New", "items": map[string]any{}, "otherIncome": 1000, "submit": true}); s != 200 {
		t.Fatal(r)
	}
	_, tax = c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	if lk := tax["regimeLock"].(map[string]any); lk["locked"] != true || lk["at"] == nil {
		t.Fatalf("saved choice is locked: %v", lk)
	}
	s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", map[string]any{"regime": "Old", "items": map[string]any{"c_ppf": 50000}, "submit": true})
	want := fmt.Sprintf("You chose the new regime for FY %s. You can choose again from 1 April %d.", fy, y+1)
	if s != 400 || r["error"] != want {
		t.Fatalf("regime change refused with %q, got %d %v", want, s, r)
	}
	// Declarations stay editable on the chosen regime.
	if s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", map[string]any{"regime": "New", "items": map[string]any{}, "otherIncome": 5000, "submit": true}); s != 200 {
		t.Fatal(r)
	}
	_, tax = c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	if d := tax["declaration"].(map[string]any); d["regime"] != "New" || d["otherIncome"] != 5000.0 {
		t.Fatalf("declaration updated on the same regime: %v", d)
	}

	// Unlocking: Payroll or HR Admin, with a reason, never for themselves, audited.
	path := "/api/tax/employee/EMP-1002/regime-unlock"
	if s, _ := c.do("POST", path, "EMP-1020", "Employee", map[string]any{"reason": "wrong choice"}); s != 403 {
		t.Fatal("an employee cannot unlock a regime choice")
	}
	if s, _ := c.do("POST", path, "EMP-1002", "HR Admin", map[string]any{"reason": "wrong choice"}); s != 403 {
		t.Fatal("nobody unlocks their own regime choice")
	}
	if s, _ := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"reason": " "}); s != 400 {
		t.Fatal("unlocking needs a reason")
	}
	_, view := c.do("GET", "/api/tax/employee/EMP-1002", "EMP-1011", "Payroll", nil)
	if view["canUnlock"] != true {
		t.Fatalf("Payroll can unlock: %v", view["canUnlock"])
	}
	if s, r := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"reason": "Chose the new regime by mistake"}); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("POST", path, "EMP-1011", "Payroll", map[string]any{"reason": "again"}); s != 400 {
		t.Fatal("an open choice cannot be unlocked again")
	}
	n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"entity": "tax_declaration", "entity_id": "EMP-1002/" + fy, "action": bson.M{"$regex": "^Tax regime choice unlocked"}})
	if n != 1 {
		t.Fatalf("the unlock is audited once, found %d", n)
	}
	_, tax = c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	lk = tax["regimeLock"].(map[string]any)
	if lk["locked"] != false || lk["unlockedBy"] == nil || lk["reason"] != "Chose the new regime by mistake" {
		t.Fatalf("unlocked with who and why: %v", lk)
	}
	// The employee chooses again, and the new choice locks again.
	if s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", map[string]any{"regime": "Old", "items": map[string]any{"c_ppf": 50000}, "submit": true}); s != 200 {
		t.Fatal(r)
	}
	if s, _ := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", map[string]any{"regime": "New", "items": map[string]any{}, "submit": true}); s != 400 {
		t.Fatal("the new choice is locked again")
	}

	// HR Admin can unlock someone else's choice too.
	if s, r := c.do("PUT", "/api/tax/me", "EMP-1020", "Employee", map[string]any{"regime": "New", "items": map[string]any{}, "submit": true}); s != 200 {
		t.Fatal(r)
	}
	if s, r := c.do("POST", "/api/tax/employee/EMP-1020/regime-unlock", "EMP-1002", "HR Admin", map[string]any{"reason": "Correction asked by the employee"}); s != 200 {
		t.Fatal(r)
	}

	// The choice opens again with the next financial year: its declaration is a new choice.
	_, nt := c.do("GET", "/api/tax/me?fy="+next, "EMP-1002", "Employee", nil)
	if nt["fy"] == next {
		if lk := nt["regimeLock"].(map[string]any); lk["locked"] != false {
			t.Fatalf("next year's choice is open: %v", lk)
		}
	}
	if s, r := c.do("PUT", "/api/tax/windows?fy="+next, "EMP-1002", "HR Admin", win); s != 200 {
		t.Fatal(r)
	}
	if s, r := c.do("PUT", "/api/tax/me?fy="+next, "EMP-1002", "Employee", map[string]any{"regime": "New", "items": map[string]any{}, "submit": true}); s != 200 {
		t.Fatalf("a new year's regime is chosen afresh: %v", r)
	}
	// Each year's declaration as fy:regime:locked, by year.
	decls, _ := st.DB.C("tax_declaration").Find(ctx, bson.M{"employee_id": "EMP-1002"}, options.Find().SetSort(bson.D{{Key: "fy", Value: 1}}))
	years := []string{}
	for _, d := range decls {
		years = append(years, fmt.Sprintf("%v:%v:%t", d["fy"], d["regime"], d["regime_locked_at"] != nil))
	}
	regimes := strings.Join(years, ",")
	if regimes != fy+":Old:true,"+next+":New:true" {
		t.Fatalf("one locked choice per year, got %s", regimes)
	}
}

func TestMyViewReimbursementSwitch(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1020"} {
		c.signIn(e)
	}
	// A policy stored before the setting has no "claims" key and keeps the default, Yes.
	if p, err := policy.Parse([]byte(`{"sun":"all","grace":10}`)); err != nil || !p.Claims.On {
		t.Fatalf("missing key means yes: %v %v", p.Claims, err)
	}
	if p, _ := policy.Parse([]byte(`{"claims":{"on":false}}`)); p.Claims.On {
		t.Fatal("stored No is kept")
	}
	if _, me := c.do("GET", "/api/me", "EMP-1020", "Employee", nil); me["claimsOn"] != true {
		t.Fatalf("claims are on by default: %v", me["claimsOn"])
	}
	claim := func(emp, role, owner string) (int, map[string]any) {
		_, doc := c.upload(emp, role, "employee", owner, "Claim bill")
		return c.do("POST", "/api/approvals", emp, role, map[string]any{"wf": "p_claim", "employeeId": owner,
			"payload": map[string]any{"claimType": "Travel", "billDate": today(), "amount": 1200, "note": "Cab", "doc": doc["id"]}})
	}
	if s, r := claim("EMP-1020", "Employee", "EMP-1020"); s != 200 {
		t.Fatalf("own claim while on: %v", r)
	}

	// Switch it off on every policy through Work policies; the change is in the policy history.
	_, list := c.do("GET", "/api/policies", "EMP-1002", "HR Admin", nil)
	pols := list["list"].([]any)
	for _, x := range pols {
		id := x.(map[string]any)["id"].(string)
		_, one := c.do("GET", "/api/policies/"+id, "EMP-1002", "HR Admin", nil)
		p := one["policy"].(map[string]any)
		set := p["settings"].(map[string]any)
		if cl, _ := set["claims"].(map[string]any); cl["on"] != true {
			t.Fatalf("existing policy reads as on: %v", set["claims"])
		}
		// An older client that does not send the switch keeps it on.
		delete(set, "claims")
		body := map[string]any{"name": p["name"], "settings": set, "leavePlanId": p["leavePlanId"], "calendarId": p["calendarId"], "calMap": p["calMap"]}
		if s, r := c.do("PUT", "/api/policies/"+id, "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		_, one = c.do("GET", "/api/policies/"+id, "EMP-1002", "HR Admin", nil)
		if cl := one["policy"].(map[string]any)["settings"].(map[string]any)["claims"].(map[string]any); cl["on"] != true {
			t.Fatalf("a save without the key keeps Yes: %v", cl)
		}
		set["claims"] = map[string]any{"on": false}
		if s, r := c.do("PUT", "/api/policies/"+id, "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		_, one = c.do("GET", "/api/policies/"+id, "EMP-1002", "HR Admin", nil)
		found := false
		for _, h := range one["history"].([]any) {
			for _, ch := range h.(map[string]any)["changes"].([]any) {
				m := ch.(map[string]any)
				found = found || (m["field"] == "settings.claims.on" && m["from"] == true && m["to"] == false)
			}
		}
		if !found {
			t.Fatalf("history records the switch: %v", one["history"])
		}
	}
	if _, me := c.do("GET", "/api/me", "EMP-1020", "Employee", nil); me["claimsOn"] != false {
		t.Fatalf("claims are off for the employee's policy: %v", me["claimsOn"])
	}
	s, r := claim("EMP-1020", "Employee", "EMP-1020")
	if msg, _ := r["error"].(string); s != 400 || !strings.Contains(msg, "does not allow reimbursement claims") {
		t.Fatalf("own claim refused when off: %d %v", s, r)
	}
	// Payroll or HR can still raise one for the employee, as before.
	if s, r := claim("EMP-1002", "HR Admin", "EMP-1020"); s != 200 {
		t.Fatalf("HR raises a claim for the employee: %v", r)
	}
	n, _ := st.DB.C("approval_request").Count(ctx, bson.M{"wf": "p_claim", "employee_id": "EMP-1020"})
	if n != 2 {
		t.Fatalf("two claims for EMP-1020 (own while on, HR's while off), found %d", n)
	}
}
