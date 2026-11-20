package api

import (
	"testing"

	"github.com/saltstayz/hrms/internal/calc"
)

// The employee's monthly medical amount (Employee master update › Medical) replaces the structure's rate and cap in
// the payroll calculation: payengine passes withMedical(s, pay.Medical) to calc.
func TestMedicalAmountReachesCalc(t *testing.T) {
	s := calc.DefaultStructure
	in := func(st *calc.Structure) calc.Result {
		return calc.Calc(calc.Input{Type: calc.Permanent, Gross: 40000, Structure: st})
	}
	if r := in(&s); !r.MedOn || r.Med != 500 {
		t.Fatalf("structure rule: %v %v", r.MedOn, r.Med)
	}
	amt := 1200.0
	if r := in(withMedical(&s, &amt)); !r.MedOn || r.Med != 1200 {
		t.Fatalf("employee amount: %v %v", r.MedOn, r.Med)
	}
	zero := 0.0
	if r := in(withMedical(&s, &zero)); r.MedOn || r.Med != 0 {
		t.Fatalf("zero: %v %v", r.MedOn, r.Med)
	}
	if s.MedMax != 500 {
		t.Fatal("the shared structure is not changed")
	}
}
