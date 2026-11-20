package api

import (
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/calc"
)

// The joining form's conditional questions (owner, 19 Nov 2026): the PF choice only with no UAN and a gross above
// ₹25,000; the UAN number (12 digits) when Existing UAN? is Yes; the spouse's name when Married.
func TestJoinFormAskedUnit(t *testing.T) {
	on := func(key string) onbField {
		return onbField{Key: key, On: true, Join: 2, Types: []string{"Permanent", "Contractual", "Retainer"}, Threshold: 25000}
	}
	pf := on("pfChoice")
	cases := []struct {
		typ  string
		d    map[string]string
		want bool
	}{
		{"Permanent", map[string]string{"uanAvail": "No", "gross": "25001"}, true},
		{"Permanent", map[string]string{"uanAvail": "No", "gross": "25000"}, false}, // gross > threshold, not ≥
		{"Permanent", map[string]string{"uanAvail": "Yes", "gross": "60000"}, false},
		{"Permanent", map[string]string{"gross": "60000"}, false},                     // not answered
		{"Contractual", map[string]string{"uanAvail": "No", "gross": "60000"}, false}, // no PF for the type
	}
	for _, c := range cases {
		if got := pf.applies(c.typ, c.d); got != c.want {
			t.Errorf("pfChoice %s %v: %v", c.typ, c.d, got)
		}
	}
	fs := []onbField{pf, on("spouse"), on("uan")}
	// Asked and not answered: required.
	errs := joinAskedErrors(fs, "Permanent", map[string]string{"uanAvail": "No", "gross": "30000", "marital": "Married"})
	if len(errs) != 2 || !strings.Contains(errs[0], "PF deducted") || !strings.Contains(errs[1], "Spouse name is required") {
		t.Fatalf("errors: %v", errs)
	}
	if errs := joinAskedErrors(fs, "Permanent", map[string]string{"uanAvail": "No", "gross": "30000", "marital": "Married", "pfChoice": "No", "spouse": "Asha"}); len(errs) != 0 {
		t.Fatalf("answered: %v", errs)
	}
	if errs := joinAskedErrors(fs, "Permanent", map[string]string{"uanAvail": "No", "gross": "30000", "marital": "Married", "pfChoice": "Maybe", "spouse": "Asha"}); len(errs) != 1 {
		t.Fatalf("Yes or No only: %v", errs)
	}
	// Single (or the gross at the threshold): neither is asked.
	if errs := joinAskedErrors(fs, "Permanent", map[string]string{"uanAvail": "No", "gross": "25000", "marital": "Single"}); len(errs) != 0 {
		t.Fatalf("single: %v", errs)
	}
	if on("spouse").applies("Permanent", map[string]string{"marital": "Single"}) || !on("spouse").applies("Permanent", map[string]string{"marital": "Married"}) {
		t.Fatal("spouse asked only when married")
	}
	// UAN number: asked (and required, join state 2) only with an existing UAN; 12 digits.
	u := on("uan")
	if !u.applies("Permanent", map[string]string{"uanAvail": "Yes"}) || u.applies("Permanent", map[string]string{"uanAvail": "No"}) {
		t.Fatal("UAN number asked only when Existing UAN? is Yes")
	}
	for v, ok := range map[string]bool{"100100001234": true, "10010000123": false, "1001000012345": false, "10010000123A": false} {
		var e []string
		checkFormat(map[string]string{"uan": v}, &e)
		if (len(e) == 0) != ok {
			t.Errorf("UAN %q: %v", v, e)
		}
	}
	if pfChoiceWord("No") != "optout" || pfChoiceWord("Yes") != "member" || pfChoiceWord("") != "" {
		t.Fatal("the answer words")
	}
}

// The TDS projection of someone paid as all Basic (no PF choice, 19 Nov 2026): under the old regime declared rent
// gives no HRA exemption (HRA is 0), Basic is the gross and no employee PF is counted; the normal split still gets it.
func TestTaxAllBasicNoHraExemption(t *testing.T) {
	e := &emp{Type: "Permanent", DOJ: "2020-01-01"}
	st := calc.DefaultStructure
	d := &taxDecl{Regime: "Old", Items: map[string]float64{"hra": 240000}}
	normal := taxFigureExtra(e, 80000, &st, "Gurugram", d, map[string]float64{}, "2026-27", calc.TaxItems, 0, false)
	ab := taxFigureExtra(e, 80000, &st, "Gurugram", d, map[string]float64{}, "2026-27", calc.TaxItems, 0, true)
	if normal == nil || ab == nil || normal.Declared.HraExempt <= 0 {
		t.Fatalf("normal split: %+v", normal)
	}
	if ab.Declared.HraExempt != 0 || ab.PFYear != 0 || ab.Declared.OldTax <= normal.Declared.OldTax || ab.Annual != ab.Declared.OldTax {
		t.Fatalf("all Basic: hra exempt %v pf %v old tax %v (normal %v)", ab.Declared.HraExempt, ab.PFYear, ab.Declared.OldTax, normal.Declared.OldTax)
	}
}
