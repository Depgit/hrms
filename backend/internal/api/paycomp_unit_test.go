package api

import (
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/calc"
)

// A designed structure's payslip lines and register columns add up to the row's figures, to the paisa, with an
// awkward proration.
func TestCompSlipLinesFoot(t *testing.T) {
	S := calc.DefaultStructure
	S.Designed = true
	S.Comps = []calc.Component{
		{Code: "basic", Name: "Basic", Kind: calc.CompFixed, Side: calc.SideEarn, Role: calc.RoleBasic, Method: calc.MPctGross, Value: 45.5, PF: true, ESI: true, Taxable: true, Payslip: true},
		{Code: "hra", Name: "HRA", Kind: calc.CompFixed, Side: calc.SideEarn, Role: calc.RoleHRA, Method: calc.MPctBasic, Value: 37.3, ESI: true, Taxable: true, Payslip: true},
		{Code: "conv", Name: "Conveyance", Kind: calc.CompFixed, Side: calc.SideEarn, Method: calc.MAmount, Value: 1601.11, ESI: true},
		{Code: "sa", Name: "Special allowance", Kind: calc.CompFixed, Side: calc.SideEarn, Method: calc.MRemainder, ESI: true, Taxable: true, Payslip: true},
		{Code: "club", Name: "Club", Kind: calc.CompFixed, Side: calc.SideDed, Method: calc.MPctOf, Of: "conv", Value: 13.7, Payslip: true},
		{Code: "perf", Name: "Performance pay", Kind: calc.CompVariable, Side: calc.SideEarn, Method: calc.MRemainder, Cycle: 1, Taxable: true, Payslip: true},
	}
	full, vt := 23457.77, 5000.0
	in := calc.RowIn{Days: 31, LOP: 3.5, DaysN: 31, Gross: full, Calc: func(earned float64, _ *float64) calc.Result {
		return calc.Calc(calc.Input{Type: calc.Permanent, Gross: earned, EligGross: &full, Structure: &S, VarTarget: &vt, VarPaid: map[string]float64{"perf": 3333.33}})
	}}
	o := &PayRowOut{Row: calc.PayRow(in)}
	f := figuresOf(o)
	fixed, vars, ded := compSlipLines(o, f)
	sum := func(l []slipLine) float64 {
		t := 0.0
		for _, x := range l {
			t = calc.Round2(t + x.Amount)
		}
		return t
	}
	if got, want := sum(fixed), calc.Round2(f.Basic+f.Hra+f.Sa); got != want {
		t.Fatalf("fixed lines %v, want %v (%+v)", got, want, fixed)
	}
	if fixed[len(fixed)-1].Label != "Other allowances" {
		t.Fatalf("a component not on the payslip is folded: %+v", fixed)
	}
	if sum(vars) != f.VarPay || f.VarPay != 3333.33 {
		t.Fatalf("variable %v %v", sum(vars), f.VarPay)
	}
	if sum(ded) != f.CompDed || f.CompDed == 0 {
		t.Fatalf("deductions %v %v", sum(ded), f.CompDed)
	}
	// The earnings foot to total earnings and the deductions to total deductions.
	if e := calc.Round2(sum(fixed) + f.Bonus + sum(vars) + f.Add + f.OE); e != f.TotE {
		t.Fatalf("earnings %v, total %v", e, f.TotE)
	}
	if d := calc.Round2(f.PF + f.ESI + f.LWF + f.MealAcc + f.Tds + f.Rec + f.OD + sum(ded)); d != f.TotD {
		t.Fatalf("deductions %v, total %v", d, f.TotD)
	}
	cols := regColsFor([]*regRow{{o: o, f: f}})
	if len(cols) != len(regCols)+len(o.R.Comps) {
		t.Fatalf("register: %d columns", len(cols))
	}
	if len(regColsFor([]*regRow{{o: &PayRowOut{}}})) != len(regCols) {
		t.Fatal("a converted structure's register changed")
	}
}

// The Reimbursement must fit in what Basic and HRA leave of the gross (owner, 20 Nov 2026): on Version 1 and a gross
// of ₹30,000 that is ₹7,500.
func TestReimbRoom(t *testing.T) {
	st := calc.DefaultStructure
	if err := checkReimb(st, "Version 1", "Permanent", 30000, 7500); err != nil {
		t.Fatalf("7,500 refused: %v", err)
	}
	if err := checkReimb(st, "Version 1", "Permanent", 30000, 7600); err == nil || !strings.Contains(err.Error(), "₹7500") {
		t.Fatalf("7,600 allowed or wrong message: %v", err)
	}
	if err := checkReimb(st, "Version 1", "Retainer", 30000, 1000); err == nil {
		t.Fatal("a retainer got a Reimbursement")
	}
}
