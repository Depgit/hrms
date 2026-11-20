package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Fixes from the 3 Nov 2026 tester report: approval routing (#4-#7), personal details (#3),
// leave plan save (#1), salary hold month (#12) and pilot pay lines (#13).

func (c *client) request(t *testing.T, id string) map[string]any {
	t.Helper()
	s, r := c.do("GET", "/api/approvals/"+id, "EMP-1001", "CHRO", nil)
	if s != 200 {
		t.Fatalf("get %s: %v", id, r)
	}
	return r
}

func nextYm(ym string) string { return mustDate(ym+"-01").AddDate(0, 1, 0).Format("2006-01") }

func waitingOn(r map[string]any) string {
	chain := r["chain"].([]any)
	step := int(num(r["step"]))
	if step >= len(chain) {
		return ""
	}
	return chain[step].(string)
}

func notes(r map[string]any) string {
	out := []string{}
	for _, h := range r["history"].([]any) {
		m := h.(map[string]any)
		if n, _ := m["note"].(string); n != "" {
			out = append(out, m["action"].(string)+": "+n)
		}
	}
	return strings.Join(out, " | ")
}

func TestApprovalRouting(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1001", "EMP-1002", "EMP-1003", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	advance := func(emp, by, role string) (int, map[string]any) {
		return c.do("POST", "/api/approvals", by, role, map[string]any{"wf": "p_adv", "employeeId": emp,
			"payload": map[string]any{"amount": 6000, "instalments": 2, "payMonth": open, "note": "Routing test"}})
	}

	t.Run("the raiser is never asked to approve: their stage moves on (AR-3010, CLM-3012)", func(t *testing.T) {
		// Sunita (EMP-1011) reports to Sandeep, the HR Admin, who raises her advance.
		s, out := advance("EMP-1011", "EMP-1002", "HR Admin")
		if s != 200 {
			t.Fatal(out)
		}
		r := c.request(t, out["id"].(string))
		if w := waitingOn(r); w == "Line Manager" || w == "" {
			t.Fatalf("waiting on %q: %s", w, notes(r))
		}
		if !strings.Contains(notes(r), "Sandeep Rawat (Line Manager) raised this request. Moved to") {
			t.Fatalf("history: %s", notes(r))
		}
		// Sandeep sees why there is no Approve button; he still cannot decide it, even as HR Admin.
		_, mine := c.do("GET", "/api/approvals/"+out["id"].(string), "EMP-1002", "HR Admin", nil)
		if mine["canAct"] != false || !strings.Contains(fmt.Sprint(mine["whyNot"]), "You raised this request") {
			t.Fatalf("raiser view: canAct %v whyNot %v", mine["canAct"], mine["whyNot"])
		}
		if s, _ := c.do("POST", "/api/approvals/"+out["id"].(string)+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "mine"}); s != 403 {
			t.Fatal("the raiser approved their own request")
		}
	})

	t.Run("HR Admin steps in on a request someone else raised, with a comment", func(t *testing.T) {
		_, out := advance("EMP-1020", "EMP-1011", "Payroll")
		id := out["id"].(string)
		_, v := c.do("GET", "/api/approvals/"+id, "EMP-1002", "HR Admin", nil)
		if v["canAct"] != true || v["whyNot"] != nil {
			t.Fatalf("HR Admin view %v %v", v["canAct"], v["whyNot"])
		}
		if s, _ := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{}); s != 400 {
			t.Fatal("an override needs a comment")
		}
		if s, r := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "Manager on leave"}); s != 200 {
			t.Fatal(r)
		}
		// The Payroll stage was Sunita's, who raised it: it went to the Payroll Manager instead.
		if r := c.request(t, id); strings.Contains(fmt.Sprint(r["chain"]), `Payroll]`) && waitingOn(r) == "Payroll" {
			t.Fatalf("Sunita asked to approve her own request: %v", r["chain"])
		}
	})

	t.Run("the person the request is about is skipped as an approver (ADV-3023)", func(t *testing.T) {
		// Anil (EMP-1009) becomes the HRBP of his own property.
		maps := st.DB.C("user_role_map")
		if _, err := maps.UpdateMany(ctx, bson.M{"role_name": "HRBP", "employee_id": "EMP-1004"}, bson.M{"$set": bson.M{"employee_id": "EMP-1009"}}); err != nil {
			t.Fatal(err)
		}
		defer maps.UpdateMany(ctx, bson.M{"role_name": "HRBP", "employee_id": "EMP-1009"}, bson.M{"$set": bson.M{"employee_id": "EMP-1004"}})
		_, out := advance("EMP-1009", "EMP-1011", "Payroll")
		id := out["id"].(string)
		r := c.request(t, id)
		for i := 0; i < 6 && r["status"] == "Pending"; i++ {
			if w := waitingOn(r); w == "HRBP" {
				t.Fatalf("Anil is asked to approve his own advance: %s", notes(r))
			}
			if s, x := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1001", "CHRO", map[string]string{"note": "ok"}); s != 200 {
				// CHRO acts only on CHRO stages; the HR Admin steps in for the rest.
				if s, x = c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "ok"}); s != 200 {
					t.Fatal(x)
				}
			}
			r = c.request(t, id)
		}
		if !strings.Contains(notes(r), "Anil Kapoor (HRBP) is the person this request is about") {
			t.Fatalf("history: %s", notes(r))
		}
	})

	t.Run("escalation skips the same person and empty roles (RS-3022)", func(t *testing.T) {
		// Anil is both Karan's manager and the HRBP of the property: escalating to him again is useless.
		maps := st.DB.C("user_role_map")
		if _, err := maps.UpdateMany(ctx, bson.M{"role_name": "HRBP", "employee_id": "EMP-1004"}, bson.M{"$set": bson.M{"employee_id": "EMP-1009"}}); err != nil {
			t.Fatal(err)
		}
		defer maps.UpdateMany(ctx, bson.M{"role_name": "HRBP", "employee_id": "EMP-1009"}, bson.M{"$set": bson.M{"employee_id": "EMP-1004"}})
		later := ymAdd(today()[:7], 1)
		date := dayIn(later, time.Tuesday, 2)
		s, out := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Casual leave", "from": date, "to": date, "note": "Family function"}})
		if s != 200 {
			t.Fatal(out)
		}
		id := out["id"].(string)
		if s, r := c.do("POST", "/api/approvals/"+id+"/escalate", "EMP-1002", "HR Admin", nil); s != 200 {
			t.Fatal(r)
		}
		r := c.request(t, id)
		if w := waitingOn(r); w != "HRBP Corporate" {
			t.Fatalf("escalated to %q, want one role above Anil: %s", w, notes(r))
		}

		// Nobody mapped as Property Manager: after the manager approves, the empty last stage escalates.
		if _, err := maps.UpdateMany(ctx, bson.M{"role_name": "Property Manager"}, bson.M{"$set": bson.M{"active": false}}); err != nil {
			t.Fatal(err)
		}
		defer maps.UpdateMany(ctx, bson.M{"role_name": "Property Manager"}, bson.M{"$set": bson.M{"active": true}})
		date = dayIn(later, time.Wednesday, 2)
		s, out = c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Casual leave", "from": date, "to": date, "note": "Family function"}})
		if s != 200 {
			t.Fatal(out)
		}
		id = out["id"].(string)
		if s, r := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1009", "Line Manager", map[string]string{}); s != 200 {
			t.Fatal(r)
		}
		r = c.request(t, id)
		if r["status"] != "Pending" || waitingOn(r) == "Property Manager" || !strings.Contains(notes(r), "No one is named for Property Manager. Escalated to") {
			t.Fatalf("empty stage not routed: %s waiting on %s: %s", r["status"], waitingOn(r), notes(r))
		}
	})

	t.Run("open requests reroute when the approver changes after raising", func(t *testing.T) {
		date := dayIn(ymAdd(today()[:7], 1), time.Thursday, 2)
		s, out := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", map[string]any{"wf": "leave", "employeeId": "EMP-1020",
			"payload": map[string]any{"ltype": "Casual leave", "from": date, "to": date, "note": "Raised by HR"}})
		if s != 200 {
			t.Fatal(out)
		}
		id := out["id"].(string)
		if w := waitingOn(c.request(t, id)); w != "Line Manager" {
			t.Fatalf("starts with %s", w)
		}
		// Karan now reports to Sandeep, who raised the request: the sweep moves it on.
		emps := st.DB.C("employee")
		if _, err := emps.Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"manager_id": "EMP-1002"}}); err != nil {
			t.Fatal(err)
		}
		defer emps.Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"manager_id": "EMP-1009"}})
		n, err := app.RerouteOpen(ctx)
		if err != nil || n < 1 {
			t.Fatalf("rerouted %d: %v", n, err)
		}
		if w := waitingOn(c.request(t, id)); w == "Line Manager" {
			t.Fatal("still waiting on the raiser")
		}
		if again, _ := app.RerouteOpen(ctx); again != 0 {
			t.Fatalf("a second sweep moved %d more", again)
		}
	})
}

func TestPersonalDetails(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1020")

	t.Run("HR edits a person's mobile, email, birth date and address", func(t *testing.T) {
		s, r := c.do("PATCH", "/api/employees/EMP-1026/personal", "EMP-1002", "HR Admin", map[string]any{"mobile": "98765 00026", "email": "Rahul.V@example.com",
			"dob": "1994-02-11", "address": "12 MG Road, Gurugram", "ecName": "Asha Verma", "ecRelation": "Mother", "ecMobile": "9876500099"})
		if s != 200 || len(r["changes"].([]any)) < 4 {
			t.Fatal(s, r)
		}
		_, e := c.do("GET", "/api/employees/EMP-1026", "EMP-1002", "HR Admin", nil)
		p := e["personal"].(map[string]any)
		if p["mobile"] != "9876500026" || p["email"] != "rahul.v@example.com" || p["dob"] != "1994-02-11" || p["ec_relation"] != "Mother" {
			t.Fatalf("saved %v", p)
		}
		_, h := c.do("GET", "/api/employees/EMP-1026/history", "EMP-1002", "HR Admin", nil)
		if !strings.Contains(fmt.Sprint(h["list"]), "Mobile") {
			t.Fatalf("no history: %v", h)
		}
		if _, r := c.do("PATCH", "/api/employees/EMP-1026/personal", "EMP-1002", "HR Admin", map[string]any{"mobile": "9876500026"}); len(r["changes"].([]any)) != 0 {
			t.Fatal("the same value is not a change")
		}
	})

	t.Run("checks and rights", func(t *testing.T) {
		for _, b := range []map[string]any{{"mobile": "12345"}, {"mobile": ""}, {"email": "not-an-email"}, {"dob": "2030-01-01"}, {"gender": "Unknown"},
			{"ecName": "Asha", "ecRelation": "Neighbour", "ecMobile": "9876500098"}} {
			if s, r := c.do("PATCH", "/api/employees/EMP-1026/personal", "EMP-1002", "HR Admin", b); s != 400 {
				t.Fatalf("%v accepted: %v", b, r)
			}
		}
		if s, _ := c.do("PATCH", "/api/employees/EMP-1026/personal", "EMP-1002", "HR Admin", map[string]any{"id": "x"}); s != 400 {
			t.Fatal("unknown fields are refused")
		}
		if s, _ := c.do("PATCH", "/api/employees/EMP-1002/personal", "EMP-1002", "HR Admin", map[string]any{"mobile": "9876500002"}); s != 403 {
			t.Fatal("own details go through a change request")
		}
		if s, _ := c.do("PATCH", "/api/employees/EMP-1026/personal", "EMP-1011", "Payroll", map[string]any{"mobile": "9876500002"}); s != 403 {
			t.Fatal("Payroll only views personal details")
		}
		if s, _ := c.do("PATCH", "/api/employees/EMP-1026/personal", "EMP-1020", "Employee", map[string]any{"mobile": "9876500002"}); s != 403 {
			t.Fatal("an employee cannot edit someone else")
		}
		if s, _ := c.do("PATCH", "/api/employees/EMP-1017/personal", "EMP-1004", "HRBP", map[string]any{"mobile": "9876500017"}); s != 403 {
			t.Fatal("HRBP edits only mapped locations")
		}
		if s, r := c.do("PATCH", "/api/employees/EMP-1020/personal", "EMP-1004", "HRBP", map[string]any{"address": "Sector 29"}); s != 200 {
			t.Fatal(r)
		}
	})

	t.Run("a person without a mobile can sign in once HR adds it", func(t *testing.T) {
		if _, err := st.DB.C("employee_personal").DeleteMany(ctx, bson.M{"employee_id": "EMP-1027"}); err != nil {
			t.Fatal(err)
		}
		if s, _ := c.do("POST", "/api/auth/otp", "", "", map[string]string{"employeeId": "EMP-1027", "purpose": "first"}); s == 200 {
			t.Fatal("OTP sent with no mobile")
		}
		if s, r := c.do("PATCH", "/api/employees/EMP-1027/personal", "EMP-1002", "HR Admin", map[string]any{"mobile": "9876500027"}); s != 200 {
			t.Fatal(r)
		}
		_, o := c.do("POST", "/api/auth/otp", "", "", map[string]string{"employeeId": "EMP-1027", "purpose": "first"})
		if o["devOtp"] == nil || !strings.HasSuffix(fmt.Sprint(o["mask"]), "27") {
			t.Fatalf("OTP after the mobile was added: %v", o)
		}
	})

	t.Run("bulk template: Personal details", func(t *testing.T) {
		csv := "Employee ID,Mobile,Personal Email,Date of Birth,Gender,Address,Father or Spouse Name,Marital Status,Emergency Contact Name,Emergency Contact Relation,Emergency Contact Mobile\n" +
			"EMP-1026,9876511126,,1994-02-12,,,,Married,,,\n" +
			"EMP-1009,123,,,,,,,,,\n"
		_, chk := c.sendFile("/api/bulk/pers/check", "EMP-1002", "HR Admin", "pers.csv", []byte(csv))
		res := chk["results"].([]any)
		if chk["ok"] != 1.0 || len(res) != 2 || len(res[1].(map[string]any)["errors"].([]any)) == 0 {
			t.Fatalf("check %v", chk)
		}
		if _, ap := c.sendFile("/api/bulk/pers/apply", "EMP-1002", "HR Admin", "pers.csv", []byte(csv)); ap["ok"] != 1.0 {
			t.Fatalf("apply %v", ap)
		}
		_, e := c.do("GET", "/api/employees/EMP-1026", "EMP-1002", "HR Admin", nil)
		if p := e["personal"].(map[string]any); p["mobile"] != "9876511126" || p["marital"] != "Married" || p["address"] != "12 MG Road, Gurugram" {
			t.Fatalf("after bulk %v", p)
		}
	})

	t.Run("an approved change request creates the details row when there is none", func(t *testing.T) {
		if _, err := st.DB.C("employee_personal").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020"}); err != nil {
			t.Fatal(err)
		}
		s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Personal", "email": "karan@example.com", "note": "New email"}})
		if s != 200 {
			t.Fatal(r)
		}
		c.approveAll(t, r["id"].(string))
		email, _ := apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020"})["email"].(string)
		if email != "karan@example.com" {
			t.Fatalf("approved change lost: %q", email)
		}
	})
}

func TestReportedScreens(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)

	t.Run("leave plan saves with the editable fields only (#1)", func(t *testing.T) {
		_, d := c.do("GET", "/api/leave-plans", "EMP-1002", "HR Admin", nil)
		plan := d["plans"].([]any)[0].(map[string]any)["plan"].(map[string]any)
		id := plan["id"].(string)
		body := map[string]any{"name": plan["name"], "description": "Edited", "lv": plan["lv"], "lrx": plan["lrx"], "types": plan["types"]}
		if s, r := c.do("PUT", "/api/leave-plans/"+id, "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		if s, r := c.do("PUT", "/api/leave-plans/"+id, "EMP-1002", "HR Admin", plan); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "id") {
			t.Fatalf("whole plan with id: %d %v", s, r)
		}
	})

	t.Run("a hold starting next month does not hold this month (#12)", func(t *testing.T) {
		next := nextYm(open)
		if s, r := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1026", "reason": "Pending clearance or handover", "note": "Resigned", "fromYm": next}); s != 200 {
			t.Fatal(r)
		}
		state := func(ym string) string {
			_, f := c.do("GET", "/api/employees/EMP-1026/file?ym="+ym, "EMP-1011", "Payroll", nil)
			for _, it := range f["items"].([]any) {
				if m := it.(map[string]any); m["key"] == "hold" {
					return m["state"].(string)
				}
			}
			return "none"
		}
		if got := state(open); got != "ready" {
			t.Fatalf("%s shows hold state %s", open, got)
		}
		if got := state(next); got != "hold" {
			t.Fatalf("%s shows hold state %s", next, got)
		}
	})

	t.Run("pilot run checks name the pay lines left out (#13)", func(t *testing.T) {
		if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": "EMP-1016", "ym": open, "kind": "earn", "label": "Reimbursement: Travel", "amount": 30000,
			"source_id": "CLM-T", "reason": "Test"}); err != nil {
			t.Fatal(err)
		}
		c.do("PUT", "/api/payroll/"+open+"/pilot", "EMP-1011", "Payroll", map[string]any{"ids": []string{"EMP-1020"}})
		c.pcCloseAll(t, open)
		if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
			t.Fatal(r)
		}
		_, ch := c.do("GET", "/api/payroll/"+open+"/checks", "EMP-1011", "Payroll", nil)
		if !strings.Contains(fmt.Sprint(ch["list"]), "Not in this pilot: 1 line (additions ₹30,000") {
			t.Fatalf("checks %v", ch)
		}
	})
}
