package api

import (
	"math"
	"testing"

	"github.com/saltstayz/hrms/internal/calc"
)

// REAL-40, REAL-41, RUN-58, REAL-42: the ECR figures of a row.
func TestFixBEcrCells(t *testing.T) {
	r := calc.Calc(calc.Input{Type: calc.Permanent, Gross: 50000}) // PF wages 25,000 (company ceiling)
	o := &PayRowOut{Row: calc.Row{Days: 31, Paid: 25, LOP: 0, Outside: 6, OutsideP: 6, R: r, TotalEarn: r.TotalEarn + 30000}}
	c := ecrCells(o)
	gross, epf, eps, edli, ee, epsAmt, diff, ncp := c[0].(float64), c[1].(float64), c[2].(float64), c[3].(float64), c[4].(float64), c[5].(float64), c[6].(float64), c[7].(float64)
	if gross != 50000 {
		t.Fatalf("gross wages leave out reimbursements: %v", gross)
	}
	if epf != 25000 || eps != 15000 || edli != 15000 {
		t.Fatalf("wages EPF %v EPS %v EDLI %v", epf, eps, edli)
	}
	if ee != 3000 || epsAmt != 1250 || diff != 1750 {
		t.Fatalf("EE %v EPS %v ER difference %v", ee, epsAmt, diff)
	}
	if ncp != 0 {
		t.Fatalf("NCP leaves out the days after the last working day: %v", ncp)
	}
	// A row saved before outsideP was kept: the window's count.
	o.OutsideP, o.Paid, o.LOP = 0, 23, 2
	if c := ecrCells(o); c[7].(float64) != 2 {
		t.Fatalf("NCP for an old row: %v", c[7])
	}
	// Under the EPS ceiling, EPS is 8.33% of the wages.
	r2 := calc.Calc(calc.Input{Type: calc.Permanent, Gross: 20000})
	c2 := ecrCells(&PayRowOut{Row: calc.Row{Days: 30, Paid: 30, R: r2}})
	if c2[2].(float64) != 10000 || c2[5].(float64) != 833 || c2[6].(float64) != 367 {
		t.Fatalf("EPS under the ceiling: %v", c2)
	}
}

// REAL-39: placeholders count as blank.
func TestFixBStatIDClean(t *testing.T) {
	for in, want := range map[string]string{"No": "", " NA ": "", "n/a": "", "-": "", "1001 2345 6789": "100123456789", "3100001020": "3100001020"} {
		if got := statIDClean(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if !reUAN.MatchString("100100001020") || reUAN.MatchString("10011020") || !reESIC.MatchString("3100001020") || reESIC.MatchString("311020") {
		t.Fatal("formats")
	}
}

// ITM-59, RUN-65, RUN-66: the register and payslip figures foot to the stored totals and net, and show the loss of
// pay apart from the days outside employment.
func TestFixBFiguresFoot(t *testing.T) {
	for _, g := range []float64{16673.37, 21001, 26066.6, 31234.567, 55555.55} {
		in := calc.RowIn{Days: 31, LOP: 1.5, Outside: 9, DaysN: 31, Gross: g,
			Calc: func(e float64, v *float64) calc.Result {
				return calc.Calc(calc.Input{Type: calc.Permanent, Gross: e, Month: 10})
			},
			Lines: []calc.Line{{Kind: "earn", Label: "Travel", Amount: 1234.56}, {Kind: "ded", Label: "Advance", Amount: 1000.01}}, OtherDed: 0.33}
		row := calc.PayRow(in)
		o := &PayRowOut{Row: row}
		f := figuresOf(o)
		earn := calc.Round2(f.Basic + f.Hra + f.Sa + f.Bonus + f.VarPay + f.Add + f.OE)
		ded := calc.Round2(f.PF + f.ESI + f.LWF + f.MealAcc + f.Tds + f.Rec + f.OD)
		if earn != f.TotE || ded != f.TotD || calc.Round2(f.TotE-f.TotD+f.RoundOff) != f.Net || f.Net != math.Round(f.Net) || f.Net != row.Net {
			t.Fatalf("gross %v: earnings %v of %v, deductions %v of %v, net %v (stored %v)", g, earn, f.TotE, ded, f.TotD, f.Net, row.Net)
		}
		if f.LOP != 1.5 || f.Outside != 9 {
			t.Fatalf("LOP %v outside %v", f.LOP, f.Outside)
		}
		if math.Abs(f.Sa-calc.Round2(row.R.Sa)) > 0.05 {
			t.Fatalf("special allowance moved too far: %v vs %v", f.Sa, row.R.Sa)
		}
	}
}
