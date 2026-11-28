package api

import (
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/realtime"
)

// The matcher on made-up people (realtime_match.go): codes only, never names (owner, 28 Nov 2026).
func TestRealtimeMatchPlan(t *testing.T) {
	hr := []rmHR{
		{ID: "EMP-1050", Name: "Asha Verma"},                                     // portal code 1050 → Sure
		{ID: "EMP-002201", Name: "Ravi Kumar"},                                   // leading zeros do not matter → Sure 2201 (by card)
		{ID: "EMP-1055", Name: "Neha Singh", Aliases: []string{"JI-3201"}},       // no 1055 on the portal; the Joining ID's 3201 → Sure
		{ID: "EMP-4000", Name: "Pooja Rani"},                                     // 4000 is a code and another's card → Check
		{ID: "EMP-5000", Name: "Same Name"}, {ID: "EMP-5001", Name: "Same Name"}, // same names, different codes: each its own
		{ID: "EMP-6000", Name: "Karan Mehta"}, // the portal's Karan Mehta has code 6100: never by name → None
		{ID: "EMP-9001", Name: "Used Person"}, // the portal's 9001 is already given → None
		{ID: "RET-7000", Name: "Mohan Lal"},   // retainer series number too
	}
	portal := []realtime.Employee{
		{Name: "Asha V", Code: "1050", Card: "00001050"},
		{Name: "Kumar Ravi", Code: "X22", Card: "00002201"},
		{Name: "N Singh", Code: "003201"},
		{Name: "Pooja Rani", Code: "4000"}, {Name: "Someone Else", Code: "4100", Card: "00004000"},
		{Name: "Same Name", Code: "5001"}, {Name: "Same Name", Code: "5000"},
		{Name: "Karan Mehta", Code: "6100"},
		{Name: "Used Person", Code: "9001"},
		{Name: "Mohan Lal", Code: "7000"},
		{Name: "Left Over", Code: "9100"},
	}
	plan := rmPlanFor(hr, portal, map[string]bool{"9001": true})
	if plan.Used != 1 || len(plan.Props) != len(hr) {
		t.Fatalf("used %d, props %d", plan.Used, len(plan.Props))
	}
	want := map[string]string{"EMP-1050": "Sure 1050", "EMP-002201": "Sure X22", "EMP-1055": "Sure 003201", "EMP-4000": "Check",
		"EMP-5000": "Sure 5000", "EMP-5001": "Sure 5001", "EMP-6000": "None", "EMP-9001": "None", "RET-7000": "Sure 7000"}
	for _, p := range plan.Props {
		got := p.Level
		if p.Pick != nil {
			got += " " + p.Pick.Code
		}
		if got != want[p.EmployeeID] {
			t.Errorf("%s: got %q (%s), want %q", p.EmployeeID, got, p.Reason, want[p.EmployeeID])
		}
		if p.Level == rmLevelLikely {
			t.Errorf("%s: Likely is never proposed now", p.EmployeeID)
		}
	}
	if p := plan.Props[2]; !strings.Contains(p.Reason, "JI-3201") {
		t.Errorf("alias reason: %s", p.Reason)
	}
	if p := plan.Props[3]; len(p.Cands) != 2 {
		t.Errorf("EMP-4000 candidates: %+v", p.Cands)
	}
	var left []string
	for _, c := range plan.Leftover {
		left = append(left, c.Code)
	}
	if strings.Join(left, ",") != "6100,9100" { // Karan Mehta of the portal is nobody's: the name is not a code
		t.Fatalf("leftover: %v", left)
	}
}
