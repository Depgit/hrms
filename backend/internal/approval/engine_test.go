package approval

import (
	"reflect"
	"testing"
)

var people = map[string]string{LM: "Anil", PM: "Rakesh", HB: "Meera", CM: "Vivek", CH: "Pooja", FH: "Rohit", OH: "Aditya", CO: "Anjali", PY: "Sunita", PYM: "Deepak", FIN: "Mahesh"}

func resolve(role string) string {
	if p, ok := people[role]; ok {
		return p
	}
	return Unassigned
}

var M = SeedFlows()

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFirstRuleThatFitsDecides(t *testing.T) {
	c := Build(M["leaveP"][Operations], Facts{WF: "leave", Roll: 7, Grade: "G3"}, resolve)
	eq(t, c.Chain, []string{LM, PM, HB, CM})
	roles := []string{}
	for _, n := range c.Notify {
		roles = append(roles, n.Role)
	}
	eq(t, roles, []string{CH, FH, OH})
	eq(t, c.Why, []string{"Grade G3–G4"}) // the rule that fits first; the grid listed every matching row
	// Otherwise names no rule.
	c = Build(M["joining"][Operations], Facts{WF: "joining"}, resolve)
	eq(t, c.Chain, []string{HB, CM})
	eq(t, c.Why, []string{})
}

func TestRuleOrderAndSwitchedOff(t *testing.T) {
	g := func(l ...string) []Condition { return []Condition{{K: "grades", List: l}} }
	s := RuleSet{Rules: []Rule{
		{ID: "a", Label: "Senior", On: true, When: g("G5"), Approvers: []string{LM, HB, CO}, Notify: []string{HB, FH}},
		{ID: "b", Label: "Any grade listed", On: true, When: g("G1", "G5"), Approvers: []string{PM, LM}},
		{ID: "o", Label: OtherwiseLabel, Approvers: []string{LM}},
	}}
	c := Build(s, Facts{Grade: "G5"}, resolve)
	eq(t, c.Chain, []string{LM, HB, CO})     // the steps in the rule's order
	eq(t, c.Notify, []Notify{{FH, "Rohit"}}) // HRBP approves, so is not also told
	eq(t, c.Why, []string{"Senior"})
	eq(t, Build(s, Facts{Grade: "G1"}, resolve).Chain, []string{PM, LM})
	eq(t, Build(s, Facts{Grade: "G9"}, resolve).Chain, []string{LM}) // Otherwise, even though it is not switched on
	s.Rules[0].On = false
	eq(t, Build(s, Facts{Grade: "G5"}, resolve).Chain, []string{PM, LM})
	eq(t, Pick(RuleSet{}, Facts{}), -1)
	eq(t, Build(RuleSet{}, Facts{}, resolve).Chain, []string{})
}

func TestJoining(t *testing.T) {
	eq(t, Build(M["joining"][Operations], Facts{WF: "joining", AboveBudget: true}, resolve).Chain, []string{HB, CM, CH})
	eq(t, Build(M["joining"][Operations], Facts{WF: "joining", InitiatorRole: HB}, resolve).Chain, []string{CM})
}

func TestSeparationExitTypes(t *testing.T) {
	eq(t, Build(M["resign"][Operations], Facts{WF: "resign", SKind: "Termination", Grade: "G1"}, resolve).Chain, []string{HB, CH, CM, CO})
	eq(t, Build(M["resign"][Operations], Facts{WF: "resign", Grade: "G1"}, resolve).Chain, []string{LM, PM})
}

func TestSamePersonAskedOnce(t *testing.T) {
	c := Build(M["leaveC"][Operations], Facts{WF: "leave", Roll: 1}, func(r string) string {
		if r == PM {
			return "Anil"
		}
		return resolve(r)
	})
	eq(t, c.Chain, []string{LM})
}

func TestAmounts(t *testing.T) {
	eq(t, Build(M["p_claim"][Operations], Facts{WF: "p_claim", Amt: 30000}, resolve).Chain, []string{LM, PY, PYM, FIN})
	eq(t, Build(M["p_claim"][Corporate], Facts{WF: "p_claim", Amt: 5000}, resolve).Chain, []string{LM, PY})
}

// blockedBy marks people who cannot decide; the blocked stages route on.
func blockedBy(out map[string]bool) func(string, []string) string {
	return func(stage string, _ []string) string {
		if p := resolve(stage); out[p] {
			return p + " cannot decide"
		}
		return ""
	}
}

func TestRouteVacant(t *testing.T) {
	chain, step, notes := RouteVacant([]string{LM, PM}, 0, blockedBy(map[string]bool{"Anil": true, "Rakesh": true}))
	eq(t, chain[step], CM)
	eq(t, notes, []string{"Anil cannot decide. Moved to " + PM, "Rakesh cannot decide. Escalated to " + CM})

	// Nobody named at the last stage, nor in the escalation line: the fallback roles decide.
	chain, step, notes = RouteVacant([]string{LM, PM}, 1, blockedBy(map[string]bool{"Rakesh": true, "Vivek": true, "Aditya": true}))
	eq(t, chain, []string{LM, "HR Admin"})
	eq(t, step, 1)
	eq(t, len(notes), 3)

	// Even the fallback cannot decide: the request stays at the last role tried, with a note.
	_, _, notes = RouteVacant([]string{CO}, 0, func(string, []string) string { return "Blocked" })
	eq(t, notes[len(notes)-1], "Blocked. Nobody else can decide it")

	// A stage that can be decided stays where it is.
	_, step, notes = RouteVacant([]string{LM, PM}, 0, blockedBy(nil))
	eq(t, step, 0)
	eq(t, len(notes), 0)
}

// The gaps between the sheet's ranges route as the next range up (TestSeedFlowsMeet): half a day of sick leave, a
// claim of ₹5,000.50.
func TestNoGapsInSeededRanges(t *testing.T) {
	eq(t, Build(M["leaveC"][Operations], Facts{WF: "leave", Days: 0.5}, resolve).Chain, []string{LM, PM})
	eq(t, Build(M["leaveC"][Operations], Facts{WF: "leave", Roll: 1.5}, resolve).Chain, []string{LM, PM})
	eq(t, Build(M["leaveC"][Operations], Facts{WF: "leave", Roll: 2.5}, resolve).Chain, []string{LM, PM, HB})
	eq(t, Build(M["leaveP"][Operations], Facts{WF: "leave", Roll: 5.5, Grade: "G1"}, resolve).Chain, []string{LM, PM, HB, CM})
	eq(t, Build(M["p_claim"][Operations], Facts{WF: "p_claim", Amt: 5000.5}, resolve).Chain, []string{LM, PY, PYM})
	eq(t, Build(M["p_claim"][Operations], Facts{WF: "p_claim", Amt: 25000.5}, resolve).Chain, []string{LM, PY, PYM, FIN})
	eq(t, Build(M["p_fnf"][Corporate], Facts{WF: "p_fnf", Amt: 100000.25}, resolve).Chain, []string{PY, PYM, FIN})
}

func TestCheckFits(t *testing.T) {
	other := Rule{Label: OtherwiseLabel}
	if err := CheckFits("leaveC", []Rule{{Label: "x", When: []Condition{{K: "tkind", Val: "Cross city"}}}, other}); err == nil ||
		err.Error() != "Rule 1 (“x”): this request type has no transfer type to check. Remove that condition" {
		t.Fatal(err)
	}
	if err := CheckFits("leaveP", []Rule{{Label: "x", When: []Condition{{K: "days", Max: f(2)}}}, {Approvers: []string{LM, RM}}}); err == nil ||
		err.Error() != "Rule 2 (“Otherwise”): Receiving Manager only takes part in an employee transfer. Remove it" {
		t.Fatal(err)
	}
	if err := CheckFits("transfer", []Rule{{Label: "x", When: []Condition{{K: "tkind", Val: "Cross city"}}, Approvers: []string{RM}}, other}); err != nil {
		t.Fatal(err)
	}
	for wf, flows := range SeedFlows() {
		for flow, rs := range flows {
			if err := CheckFits(wf, rs.Rules); err != nil {
				t.Errorf("seeded %s/%s: %v", wf, flow, err)
			}
		}
	}
}

// Grade rules on every flow whose request has a grade (owner, 26 Nov 2026: offer approval by grade).
func TestGradeOnMoreFlows(t *testing.T) {
	for _, wf := range []string{"joining", "verify", "p_arrear", "p_change", "p_fnf", "p_pip"} {
		if !KindFits(wf, "grades") {
			t.Fatalf("%s: grade rule not allowed", wf)
		}
	}
	if KindFits("shortlist", "grades") {
		t.Fatal("shortlist has no grade yet")
	}
	s := RuleSet{Rules: []Rule{
		{ID: "a", Label: "Grade G3", On: true, When: []Condition{{K: "grades", List: []string{"G3"}}}, Approvers: []string{PM, LM}},
		{ID: "o", Label: OtherwiseLabel, Approvers: []string{LM}},
	}}
	eq(t, Build(s, Facts{WF: "joining", Grade: "G3"}, resolve).Chain, []string{PM, LM})
	eq(t, Build(s, Facts{WF: "joining", Grade: "G1"}, resolve).Chain, []string{LM})
}
