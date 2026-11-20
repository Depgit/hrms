package api

import (
	"strings"
	"testing"
)

// The PF exemption rule (owner, 14 Nov 2026: "Gross > ₹20,000 and no UAN"), on its own.
func TestPfExemptRuleUnit(t *testing.T) {
	no, yes := false, true
	rules := pfRules{ExemptGross: 20000, UanHold: true}
	cases := []struct {
		name   string
		f      pfFacts
		exempt bool
		why    string
	}{
		{"joiner above the gross, no UAN", pfFacts{Type: "Permanent", Existing: &no, Gross: 25000}, true, "above ₹20,000"},
		{"gross at the threshold", pfFacts{Type: "Permanent", Existing: &no, Gross: 20000}, false, "not above"},
		{"existing member", pfFacts{Type: "Permanent", Existing: &yes, Gross: 50000}, false, "existing UAN"},
		{"not answered", pfFacts{Type: "Permanent", Gross: 50000}, false, "not recorded"},
		{"UAN on record", pfFacts{Type: "Permanent", Existing: &no, HasUan: true, Gross: 50000}, false, "UAN is on record"},
		{"no PF for the type", pfFacts{Type: "Contractual", Existing: &no, Gross: 50000}, false, "does not apply"},
	}
	for _, c := range cases {
		got, why := pfRule(&c.f, rules)
		if got != c.exempt || !strings.Contains(why, c.why) {
			t.Errorf("%s: %v %q, want %v (%q)", c.name, got, why, c.exempt, c.why)
		}
	}
	// A higher threshold: the same joiner is a member.
	if got, _ := pfRule(&pfFacts{Type: "Permanent", Existing: &no, Gross: 25000}, pfRules{ExemptGross: 30000}); got {
		t.Error("threshold 30,000 still exempts 25,000")
	}
	// The legal warning: exempt with Basic at or below ₹15,000, never for a member.
	if w := pfBasicWarning(true, 12500); !strings.Contains(w, "₹12,500") || !strings.Contains(w, "may still require PF") {
		t.Errorf("warning %q", w)
	}
	if pfBasicWarning(true, 15000) == "" || pfBasicWarning(true, 15001) != "" || pfBasicWarning(false, 10000) != "" {
		t.Error("warning at 15,000 only for the exempt")
	}
	for v, want := range map[any]string{true: "true", false: "false", "Yes": "true", "No": "false", nil: "nil", "": "nil"} {
		got := existingUanVal(v)
		if (got == nil && want != "nil") || (got != nil && ((*got && want != "true") || (!*got && want != "false"))) {
			t.Errorf("existing_uan %v: %v", v, got)
		}
	}
}

// The owner's example of variable pay on each person's own cycle: quarterly, joined in May 2026 (mid-month counts as
// month 1), so the payouts are July and October 2026, January and April 2027.
func TestVariableOwnCycleOwnersExample(t *testing.T) {
	anchor := varAnchorYm("2026-05", "", nil, 0)
	if anchor != "2026-05" {
		t.Fatalf("anchor %s", anchor)
	}
	want := map[string]string{"2026-05": "2026-07", "2026-07": "2026-07", "2026-08": "2026-10", "2026-11": "2027-01", "2027-02": "2027-04"}
	for ym, next := range want {
		if got := varNextPayout(anchor, ym, 3); got != next {
			t.Errorf("next payout from %s: %s, want %s", ym, got, next)
		}
	}
	// Variable pay that started after joining counts from its start month (var_start), or for an older change from
	// var_from when nothing was paid before it; a change of an existing % keeps the joining month.
	if a := varAnchorYm("2024-03", "2026-11", nil, 0); a != "2026-11" || varNextPayout(a, "2026-11", 3) != "2027-01" {
		t.Errorf("var_start anchor %s", a)
	}
	zero, ten := 0.0, 10.0
	if a := varAnchorYm("2024-03", "", &varFrom{From: "2026-11", Prev: &zero}, 5); a != "2026-11" {
		t.Errorf("var_from from nothing: %s", a)
	}
	if a := varAnchorYm("2024-03", "", &varFrom{From: "2026-11", Prev: &ten}, 0); a != "2024-03" {
		t.Errorf("var_from from 10%%: %s", a)
	}
	if a := varAnchorYm("2024-03", "", &varFrom{From: "2026-11"}, 5); a != "2024-03" {
		t.Errorf("var_from with the structure's 5%% before: %s", a)
	}
	// A start before joining never moves the anchor back.
	if a := varAnchorYm("2026-05", "2026-01", nil, 0); a != "2026-05" {
		t.Errorf("start before joining: %s", a)
	}
}
