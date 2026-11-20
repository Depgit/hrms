package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The approval flow editor: rule lists per workflow and flow (the first rule that fits decides), saved whole with
// checks, tested with sample facts, renamed with the grade master, and used by every new request.
func TestApprovalFlowRules(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1020")
	hr := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		return c.do(method, path, "EMP-1002", "HR Admin", body)
	}
	flowOf := func(wf, flow string) map[string]any {
		t.Helper()
		s, l := hr("GET", "/api/approvals/flows", nil)
		if s != 200 {
			t.Fatalf("flows: %d %v", s, l)
		}
		for _, x := range l["list"].([]any) {
			if m := x.(map[string]any); m["wf"] == wf && m["flow"] == flow {
				return m
			}
		}
		t.Fatalf("no flow %s/%s", wf, flow)
		return nil
	}
	rule := func(label string, when []any, approvers, notify []string) map[string]any {
		return map[string]any{"label": label, "on": true, "when": when, "approvers": approvers, "notify": notify}
	}
	otherwise := func(approvers ...string) map[string]any { return rule("Otherwise", []any{}, approvers, []string{}) }
	grades := func(l ...string) map[string]any { return map[string]any{"k": "grades", "list": l} }

	t.Run("every workflow and flow has a rule list ending in Otherwise", func(t *testing.T) {
		_, l := hr("GET", "/api/approvals/flows", nil)
		list := l["list"].([]any)
		if len(list) != 34 {
			t.Fatalf("%d rule lists", len(list))
		}
		for _, x := range list {
			m := x.(map[string]any)
			rules := m["rules"].([]any)
			last := rules[len(rules)-1].(map[string]any)
			if last["label"] != "Otherwise" || len(last["when"].([]any)) != 0 || m["no_response"] == nil {
				t.Fatalf("%v/%v: %v", m["wf"], m["flow"], m)
			}
		}
		lp := flowOf("leaveP", "Operations")["rules"].([]any)
		first := lp[0].(map[string]any)
		if first["label"] != "Grade G3–G4" || fmt.Sprint(first["approvers"]) != "[Line Manager Property Manager HRBP Cluster Manager]" ||
			fmt.Sprint(first["notify"]) != "[HRBP Corporate Function Head Operations Head]" {
			t.Fatalf("leaveP first rule: %v", first)
		}
		if flowOf("regularize", "Operations")["no_response"] != "auto" {
			t.Fatal("regularize keeps auto-approve when nobody acts")
		}
	})

	t.Run("a test request shows the rule that decides and the route", func(t *testing.T) {
		s, r := hr("POST", "/api/approvals/flows/leaveP/Operations/test", map[string]any{"facts": map[string]any{"roll": 7, "grade": "G3"}})
		if s != 200 || r["rule"] != float64(1) || r["label"] != "Grade G3–G4" || r["otherwise"] != false ||
			fmt.Sprint(r["chain"]) != "[Line Manager Property Manager HRBP Cluster Manager]" || len(r["notify"].([]any)) != 3 {
			t.Fatalf("%d %v", s, r)
		}
		_, r = hr("POST", "/api/approvals/flows/leaveP/Operations/test", map[string]any{"facts": map[string]any{"roll": 7, "grade": "G1"}})
		if r["label"] != "Up to 10 days in 30 days" || fmt.Sprint(r["chain"]) != "[Line Manager Property Manager HRBP Cluster Manager]" {
			t.Fatalf("7 days, G1: %v", r)
		}
		// The seeded ranges meet: 5.5 days (between the sheet's "up to 5" and "6 to 10") routes as 6 to 10, and half a
		// day of casual leave as 1 day; nothing falls through to an Otherwise that asks nobody.
		_, r = hr("POST", "/api/approvals/flows/leaveP/Operations/test", map[string]any{"facts": map[string]any{"roll": 5.5, "grade": "G1"}})
		if r["rule"] != float64(4) || fmt.Sprint(r["chain"]) != "[Line Manager Property Manager HRBP Cluster Manager]" {
			t.Fatalf("5.5 days: %v", r)
		}
		_, r = hr("POST", "/api/approvals/flows/leaveC/Operations/test", map[string]any{"facts": map[string]any{"days": 0.5}})
		if r["label"] != "Up to 1 day in 30 days" || fmt.Sprint(r["chain"]) != "[Line Manager Property Manager]" {
			t.Fatalf("half a day: %v", r)
		}
		_, r = hr("POST", "/api/approvals/flows/p_claim/Corporate/test", map[string]any{"facts": map[string]any{"amt": 5000.5}})
		if r["label"] != "Up to ₹25,000" || fmt.Sprint(r["chain"]) != "[Line Manager Payroll Payroll Manager]" {
			t.Fatalf("₹5,000.50: %v", r)
		}
		_, r = hr("POST", "/api/approvals/flows/resign/Operations/test", map[string]any{"facts": map[string]any{"skind": "Termination"}})
		if r["label"] != "Termination" || fmt.Sprint(r["chain"]) != "[HRBP HRBP Corporate Cluster Manager CHRO]" {
			t.Fatalf("termination: %v", r)
		}
		// With an employee: the people are named, and the grade is the employee's when none is given.
		s, r = hr("POST", "/api/approvals/flows/p_claim/Operations/test", map[string]any{"facts": map[string]any{"amt": 400}, "employeeId": "EMP-1020"})
		people, _ := r["people"].([]any)
		if s != 200 || r["label"] != "Up to ₹5,000" || len(people) != 2 || people[0].(map[string]any)["name"] == "" {
			t.Fatalf("with an employee: %d %v", s, r)
		}
		if s, _ := hr("POST", "/api/approvals/flows/nothing/Operations/test", map[string]any{"facts": map[string]any{}}); s != 404 {
			t.Fatalf("unknown flow: %d", s)
		}
		if s, _ := hr("POST", "/api/approvals/flows/leaveP/Operations/test", map[string]any{"facts": map[string]any{"days": "seven"}}); s != 400 {
			t.Fatalf("bad facts: %d", s)
		}
	})

	t.Run("the plan names the rule that decided apart from what was added on top", func(t *testing.T) {
		s, r := hr("POST", "/api/approvals/simulate", map[string]any{"wf": "transfer", "employeeId": "EMP-1020", "payload": map[string]any{"toDepartment": "Finance"}})
		why := fmt.Sprint(r["why"])
		if s != 200 || r["rule"] == "" || !strings.Contains(why, "Moving into a corporate department") || r["rule"] == "Moving into a corporate department" {
			t.Fatalf("%d rule %v, why %v", s, r["rule"], r["why"])
		}
	})

	t.Run("saving checks the list", func(t *testing.T) {
		path := "/api/approvals/flows/p_claim/Operations"
		amount := func(min, max float64) map[string]any { return map[string]any{"k": "amount", "min": min, "max": max} }
		bad := map[string][]any{
			"A flow needs at least the Otherwise rule":                                {},
			"The last rule must be Otherwise, with no conditions":                     {rule("Big", []any{amount(1, 9)}, []string{"Payroll"}, nil)},
			"Rule 1 (“Nothing”) has no condition. Add one, or delete the rule":        {rule("Nothing", []any{}, []string{"Payroll"}, nil), otherwise()},
			"Nobody cannot approve requests":                                          {rule("Big", []any{amount(1, 9)}, []string{"Nobody"}, nil), otherwise()},
			"Rule 1 (“Big”) lists Payroll twice as an approver":                       {rule("Big", []any{amount(1, 9)}, []string{"Payroll", "CHRO", "Payroll"}, nil), otherwise()},
			"Rule 1 (“Big”): Payroll cannot both approve and be told. Keep one":       {rule("Big", []any{amount(1, 9)}, []string{"Payroll"}, []string{"Payroll"}), otherwise()},
			"Rule 1 (“Big”): the upper number cannot be below the lower one":          {rule("Big", []any{amount(9, 1)}, []string{"Payroll"}, nil), otherwise()},
			"Rule 1 (“Some”): pick at least one grade":                                {rule("Some", []any{grades()}, []string{"Payroll"}, nil), otherwise()},
			"Rule 1 (“Fuel”): pick a claim type (Travel, Mobile and internet, Other)": {rule("Fuel", []any{map[string]any{"k": "ctype", "val": "Fuel"}}, []string{"Payroll"}, nil), otherwise()},
			"Rule 1 (“Big”) checks the amount twice. Keep one condition of each kind": {rule("Big", []any{amount(1, 9), amount(3, 4)}, []string{"Payroll"}, nil), otherwise()},
			"Rule 2 (“Otherwise”): HRBP cannot both approve and be told. Keep one":    {rule("Big", []any{amount(1, 9)}, []string{"Payroll"}, nil), rule("", []any{}, []string{"HRBP"}, []string{"HRBP"})},
			"Rule 1 (“Big”): the numbers cannot be below 0":                           {rule("Big", []any{map[string]any{"k": "amount", "max": -5}}, []string{"Payroll"}, nil), otherwise()},
			// Only what a claim carries can be checked, and the receiving side only takes part in a transfer.
			"Rule 1 (“Far”): this request type has no transfer type to check. Remove that condition":     {rule("Far", []any{map[string]any{"k": "tkind", "val": "Cross city"}}, []string{"Payroll"}, nil), otherwise()},
			"Rule 2 (“Otherwise”): Receiving Manager only takes part in an employee transfer. Remove it": {rule("Big", []any{amount(1, 9)}, []string{"Payroll"}, nil), otherwise("Receiving Manager")},
		}
		for want, rules := range bad {
			if s, r := hr("PUT", path, map[string]any{"rules": rules}); s != 400 || r["error"] != want {
				t.Errorf("%d %v, want %q", s, r, want)
			}
		}
		if s, r := hr("PUT", path, map[string]any{"rules": []any{otherwise("Payroll")}, "noResponse": "sometimes"}); s != 400 || r["error"] != "If no response must be escalate, auto or wait" {
			t.Errorf("no response: %d %v", s, r)
		}
		if s, r := hr("PUT", "/api/approvals/flows/leaveC/Operations", map[string]any{"rules": []any{rule("Far", []any{map[string]any{"k": "tkind", "val": "Cross city"}}, []string{"Line Manager"}, nil), otherwise("Line Manager")}}); s != 400 ||
			r["error"] != "Rule 1 (“Far”): this request type has no transfer type to check. Remove that condition" {
			t.Errorf("transfer type on leave: %d %v", s, r)
		}
		if s, r := hr("PUT", "/api/approvals/flows/transfer/Corporate", map[string]any{"rules": []any{rule("Far", []any{map[string]any{"k": "tkind", "val": "Cross city"}}, []string{"Line Manager", "Receiving Manager"}, nil), otherwise("Receiving Manager")}}); s != 200 {
			t.Errorf("a transfer takes the receiving side: %d %v", s, r)
		}
		if s, _ := hr("PUT", "/api/approvals/flows/interview/Operations", map[string]any{"rules": []any{otherwise("Line Manager")}}); s != 400 {
			t.Errorf("interview: %d", s)
		}
		if s, _ := hr("PUT", "/api/approvals/flows/nothing/Operations", map[string]any{"rules": []any{otherwise("Line Manager")}}); s != 404 {
			t.Errorf("unknown flow: %d", s)
		}
		if s, _ := c.do("PUT", path, "EMP-1020", "Employee", map[string]any{"rules": []any{otherwise("Payroll")}}); s != 403 {
			t.Errorf("an employee saved a flow: %d", s)
		}
		if s, _ := hr("PUT", path, map[string]any{"rows": []any{}, "cols": []any{}}); s != 400 {
			t.Errorf("the old grid body: %d", s)
		}
	})

	t.Run("a saved list routes new requests; names and ids are filled in, and the change is audited", func(t *testing.T) {
		rules := []any{
			rule("", []any{map[string]any{"k": "amount", "max": 1000}}, []string{"Payroll"}, []string{"Line Manager"}),
			rule("Large", []any{map[string]any{"k": "amount", "min": 1000.01}}, []string{"Line Manager", "Payroll Manager", "Payroll"}, nil),
			map[string]any{"label": "anything", "on": false, "when": []any{}, "approvers": []string{"Payroll"}, "notify": []string{}},
		}
		s, r := hr("PUT", "/api/approvals/flows/p_claim/Operations", map[string]any{"rules": rules, "noResponse": "wait"})
		if s != 200 {
			t.Fatalf("%d %v", s, r)
		}
		saved := flowOf("p_claim", "Operations")
		got := saved["rules"].([]any)
		first, last := got[0].(map[string]any), got[2].(map[string]any)
		if first["label"] != "Up to ₹1,000" || first["id"] == "" || last["label"] != "Otherwise" || last["on"] != true || saved["no_response"] != "wait" || saved["updated_by"] == nil {
			t.Fatalf("saved: %v", saved)
		}
		sim := func(amount float64) map[string]any {
			t.Helper()
			s, r := c.do("POST", "/api/approvals/simulate", "EMP-1020", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": amount, "claimType": "Other", "billDate": today()}})
			if s != 200 {
				t.Fatalf("simulate %v: %d %v", amount, s, r)
			}
			return r
		}
		if r := sim(400); fmt.Sprint(r["chain"]) != "[Payroll]" || fmt.Sprint(r["why"]) != "[Up to ₹1,000]" || r["rule"] != "Up to ₹1,000" || len(r["notify"].([]any)) != 1 {
			t.Fatalf("400: %v %v %v", r["chain"], r["why"], r["notify"])
		}
		if r := sim(5000); fmt.Sprint(r["chain"]) != "[Line Manager Payroll Manager Payroll]" || fmt.Sprint(r["why"]) != "[Large]" {
			t.Fatalf("5000: %v %v", r["chain"], r["why"])
		}
		if n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"action": "Changed approval flow", "entity": "approval_flow", "entity_id": "p_claim/Operations"}); n != 1 {
			t.Fatalf("audit entries: %d", n)
		}
		// Switched off: skipped.
		rules[1].(map[string]any)["on"] = false
		if s, r := hr("PUT", "/api/approvals/flows/p_claim/Operations", map[string]any{"rules": rules}); s != 200 {
			t.Fatal(r)
		}
		if r := sim(5000); fmt.Sprint(r["chain"]) != "[Payroll]" || len(r["why"].([]any)) != 0 || r["rule"] != "Otherwise" {
			t.Fatalf("5000 with Large off: %v %v", r["chain"], r["why"])
		}
		// The setup health lists a rule in use that has no approver.
		rules[1].(map[string]any)["on"], rules[1].(map[string]any)["approvers"] = true, []string{}
		if s, r := hr("PUT", "/api/approvals/flows/p_claim/Operations", map[string]any{"rules": rules}); s != 200 {
			t.Fatal(r)
		}
		_, h := hr("GET", "/api/approvals/health", nil)
		found := false
		for _, x := range h["list"].([]any) {
			m := x.(map[string]any)
			if m["title"] == "Every condition has at least one approver" {
				found = m["ok"] == false && strings.Contains(fmt.Sprint(m["detail"]), "Large")
			}
		}
		if !found {
			t.Fatalf("health: %v", h)
		}
	})

	t.Run("renaming a grade renames it in the rules", func(t *testing.T) {
		_, gl := hr("GET", "/api/masters/grade", nil)
		gid := ""
		for _, x := range gl["list"].([]any) {
			if m := x.(map[string]any); m["name"] == "G3" {
				gid = fmt.Sprint(m["id"])
			}
		}
		if s, r := hr("PATCH", "/api/masters/grade/"+gid, map[string]any{"name": "Grade 3"}); s != 200 {
			t.Fatalf("rename: %d %v", s, r)
		}
		first := flowOf("leaveP", "Operations")["rules"].([]any)[0].(map[string]any)
		if fmt.Sprint(first["when"]) != "[map[k:grades list:[Grade 3 G4]]]" || first["label"] != "Grade G3–G4" {
			t.Fatalf("renamed rule: %v", first)
		}
		_, r := hr("POST", "/api/approvals/flows/leaveP/Operations/test", map[string]any{"facts": map[string]any{"roll": 1, "grade": "Grade 3"}})
		if r["rule"] != float64(1) {
			t.Fatalf("the renamed grade fits the rule: %v", r)
		}
	})
}
