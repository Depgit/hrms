package calc

import (
	"math"
	"testing"
)

// User decision 13 Nov (RUN-53, ITM-59): net pay is rounded to the nearest rupee, and the rounding is kept on the row
// so earnings − deductions + rounding off = net.
func TestNetRoundedToRupee(t *testing.T) {
	for _, g := range []float64{16673.37, 21001, 22206.94, 26066.6, 31234.567, 55555.55} {
		row := PayRow(RowIn{Days: 31, DaysN: 31, LOP: 1.5, Gross: g, OtherDed: 0.33,
			Lines: []Line{{Kind: "earn", Label: "Travel", Amount: 1234.56}},
			Calc:  func(e float64, _ *float64) Result { return Calc(Input{Type: Permanent, Gross: e, Month: 10}) }})
		if row.Net != math.Round(row.Net) {
			t.Fatalf("gross %v: net %v is not whole rupees", g, row.Net)
		}
		if Round2(row.TotalEarn-row.TotalDed+row.RoundOff) != row.Net || math.Abs(row.RoundOff) > 0.5 {
			t.Fatalf("gross %v: %v − %v + %v ≠ %v", g, row.TotalEarn, row.TotalDed, row.RoundOff, row.Net)
		}
	}
	// A negative remainder is carried, not rounded.
	row := PayRow(RowIn{Days: 30, DaysN: 30, Gross: 15000, Lines: []Line{{Kind: "ded", Label: "Recovery", Amount: 20000.4}},
		Calc: func(e float64, _ *float64) Result { return Calc(Input{Type: Permanent, Gross: e}) }})
	if row.Net != 0 || row.RoundOff != 0 || row.Carry <= 0 {
		t.Fatalf("carry row: net %v round %v carry %v", row.Net, row.RoundOff, row.Carry)
	}
}

// User decision 13 Nov (REAL-40): EPS on wages up to ₹15,000 (at most ₹1,250), the employer's remainder to EPF, EDLI
// on wages up to ₹15,000 — in the salary calculation itself.
func TestEpsSplitInCalc(t *testing.T) {
	r := Calc(Input{Type: Permanent, Gross: 50000}) // PF wages 25,000 under the company ceiling
	if r.Eps != 1250 || Round2(r.Eps+r.ErEpf) != r.ErPF || r.ErEpf != 1750 {
		t.Fatalf("EPS %v EPF %v of employer %v", r.Eps, r.ErEpf, r.ErPF)
	}
	if r.Edli != 75 {
		t.Fatalf("EDLI %v, want 75 (0.5%% of 15,000)", r.Edli)
	}
	r = Calc(Input{Type: Permanent, Gross: 20000}) // PF wages 10,000
	if r.Eps != 833 || r.ErEpf != 367 {
		t.Fatalf("under the ceiling: EPS %v EPF %v", r.Eps, r.ErEpf)
	}
}
