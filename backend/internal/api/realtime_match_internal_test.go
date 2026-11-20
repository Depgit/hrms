package api

import (
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/realtime"
)

// The matcher on made-up people (realtime_match.go).

func TestRealtimeMatchNames(t *testing.T) {
	tk := rmTokens
	if !rmSameName(tk("Mr. Ravi  KUMAR"), tk("kumar ravi")) || !rmSameName(tk("Smt. Asha Devi"), tk("Asha Devi")) {
		t.Fatal("same name: honorifics, case, dots, order")
	}
	if rmSameName(tk("00000007"), tk("00000007")) {
		t.Fatal("a name with no letters matches nothing")
	}
	if !rmSimilarName(tk("Vikas Kumar"), tk("Vikash Kumar")) || !rmSimilarName(tk("Ravi Kumar Yadav"), tk("Ravi Yadav")) {
		t.Fatal("similar: spelling, a middle name")
	}
	if rmSimilarName(tk("Ravi Kumar"), tk("Amit Kumar")) || rmSimilarName(tk("Ravi"), tk("Ravi Kumar")) {
		t.Fatal("not similar: a shared surname, or one word")
	}
	if !rmText("Front Office", "front office") || !rmText("SaltStayz Amara", "Amara", "saltstayz") || rmText("Kitchen", "Front Office") {
		t.Fatal("masters")
	}
}

func TestRealtimeMatchPlan(t *testing.T) {
	hr := []rmHR{
		{ID: "E1", Name: "Asha Verma", Dept: "Front Office", Mobile: "9876500001"},                              // same mobile and name → Sure
		{ID: "E2", Name: "Ravi Kumar", DOJ: "2023-06-05"},                                                       // same name + date of joining → Sure
		{ID: "E3", Name: "Neha Singh"},                                                                          // same name only → Likely
		{ID: "E4", Name: "Amit Sharma", DOJ: "2022-01-10"},                                                      // one of two with the name, the date agrees → Likely
		{ID: "E5", Name: "Pooja Rani"},                                                                          // two with the name, nothing else → Check
		{ID: "E6", Name: "Vikas Yadav", Dept: "Kitchen"},                                                        // similar name + department → Likely
		{ID: "E7", Name: "Sunil Das", Mobile: "9876500077"},                                                     // same mobile, other name → Likely
		{ID: "E8", Name: "Nobody Atall"},                                                                        // nothing → None
		{ID: "E9", Name: "Karan Mehta", DOB: "1990-01-01"}, {ID: "E10", Name: "Karan Mehta", DOB: "1990-01-01"}, // both pick the same → Check
		{ID: "E11", Name: "Used Person"}, // the only one with the name has a used code
	}
	portal := []realtime.Employee{
		{Name: "Asha Verma", Code: "1050", Card: "00001050", Phone: "9876500001", Dept: "Housekeeping"},
		{Name: "Kumar Ravi", Code: "2201", Card: "00002201", DOJ: "2023-06-05"},
		{Name: "Neha Singh", Code: "3001", Card: "00003001", Dept: "Kitchen"},
		{Name: "Amit Sharma", Code: "4001", DOJ: "2022-01-10"}, {Name: "Amit Sharma", Code: "4002", DOJ: "2021-03-03"},
		{Name: "Pooja Rani", Code: "5001"}, {Name: "Pooja Rani", Code: "5002"},
		{Name: "Vikash Yadav", Code: "6001", Dept: "Kitchen"},
		{Name: "S. Das Gupta", Code: "7001", Phone: "9876500077"},
		{Name: "Karan Mehta", Code: "8001", DOB: "1990-01-01"},
		{Name: "Used Person", Code: "9001", Card: "00009001"},
		{Name: "Left Over", Code: "9100"},
		{Name: "Shared Phone A", Code: "9200", Phone: "9000000000"}, {Name: "Shared Phone B", Code: "9201", Phone: "9000000000"},
	}
	plan := rmPlanFor(hr, portal, map[string]bool{"9001": true})
	if plan.Used != 1 || len(plan.Props) != len(hr) {
		t.Fatalf("used %d, props %d", plan.Used, len(plan.Props))
	}
	want := map[string]string{"E1": "Sure 1050", "E2": "Sure 2201", "E3": "Likely 3001", "E4": "Likely 4001", "E5": "Check", "E6": "Likely 6001",
		"E7": "Likely 7001", "E8": "None", "E9": "Check", "E10": "Check", "E11": "None"}
	for _, p := range plan.Props {
		got := p.Level
		if p.Pick != nil {
			got += " " + p.Pick.Code
		}
		if got != want[p.EmployeeID] {
			t.Errorf("%s: got %q (%s), want %q", p.EmployeeID, got, p.Reason, want[p.EmployeeID])
		}
		switch p.EmployeeID {
		case "E5":
			if len(p.Cands) != 2 {
				t.Errorf("E5 candidates: %+v", p.Cands)
			}
		case "E9":
			if len(p.Cands) == 0 || p.Cands[0].Code != "8001" || !strings.Contains(p.Reason, "E10") {
				t.Errorf("E9: %+v", p)
			}
		case "E7":
			if !strings.Contains(p.Reason, "name differs") {
				t.Errorf("E7 reason: %s", p.Reason)
			}
		}
	}
	var left []string
	for _, c := range plan.Leftover {
		left = append(left, c.Code)
	}
	if strings.Join(left, ",") != "4002,9100,9200,9201" { // the other Amit Sharma is nobody's
		t.Fatalf("leftover: %v", left)
	}
}
