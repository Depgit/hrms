package calc

import (
	"math"
	"testing"
)

// The guide's month example (spec 14): Gurugram, gross 30,000, variable 10% every 3 months,
// joined 20 October. Advance 12,000 paid in November, recovered 4 x 3,000 from December;
// cash shortage 6,000 as 3 x 2,000 from November; arrear 3,500 for October paid in December.
func TestGuideMonthExample(t *testing.T) {
	haryana := LwfSeed["Haryana"]
	vp, cyc := 10.0, 3
	accrual := func(ym string) float64 { // variable accrued in an earlier cycle month
		if ym == "2026-10" {
			return 30000 * 12.0 / 31 * 0.10
		}
		return 3000
	}
	cycleMonths := func(ym string) []string {
		all := []string{"2026-10", "2026-11", "2026-12", "2027-01", "2027-02", "2027-03"}
		for i, m := range all {
			if m == ym {
				out := []string{}
				for k := 1; k < cyc && i-k >= 0; k++ {
					out = append(out, all[i-k])
				}
				return out
			}
		}
		return nil
	}
	lines := map[string][]Line{
		"2026-11": {{Kind: "earn", Label: "Salary advance", Amount: 12000}, {Kind: "ded", Label: "Cash shortage (1 of 3)", Amount: 2000}},
		"2026-12": {{Kind: "ded", Label: "Advance recovery (1 of 4)", Amount: 3000}, {Kind: "ded", Label: "Cash shortage (2 of 3)", Amount: 2000}, {Kind: "earn", Label: "Arrear", Amount: 3500}},
		"2027-01": {{Kind: "ded", Label: "Advance recovery (2 of 4)", Amount: 3000}, {Kind: "ded", Label: "Cash shortage (3 of 3)", Amount: 2000}},
		"2027-02": {{Kind: "ded", Label: "Advance recovery (3 of 4)", Amount: 3000}},
		"2027-03": {{Kind: "ded", Label: "Advance recovery (4 of 4)", Amount: 3000}},
	}
	want := map[string]float64{"2026-11": 38166, "2026-12": 33827, "2027-01": 23166, "2027-02": 25166, "2027-03": 34166, "2027-04": 28166}
	for ym, w := range want {
		month := int((ym[5]-'0')*10 + ym[6] - '0')
		var prev []float64
		for _, m := range cycleMonths(ym) {
			prev = append(prev, accrual(m))
		}
		row := PayRow(RowIn{Days: PayDays(ym, "calendar"), DaysN: PayDays(ym, "calendar"), Gross: 30000, Lines: lines[ym], PrevVariable: prev,
			Calc: func(earned float64, tot *float64) Result {
				return Calc(Input{Type: Permanent, Gross: earned, EligGross: f64(30000), Month: month, JoinMonth: 10, VarPct: &vp, VarCycle: &cyc, VarTotal: tot, Lwf: &haryana})
			}})
		if math.Round(row.Net) != w {
			t.Errorf("%s net %.2f, want %.0f (var pay %.2f)", ym, row.Net, w, row.R.VarPay)
		}
	}
}

func f64(v float64) *float64 { return &v }

func TestPayRowCarryAndPaidDays(t *testing.T) {
	row := PayRow(RowIn{Days: 30, DaysN: 30, LOP: 2, Outside: 3, Gross: 15000, Lines: []Line{{Kind: "ded", Label: "Recovery", Amount: 20000}},
		Calc: func(earned float64, _ *float64) Result {
			return Calc(Input{Type: Permanent, Gross: earned, EligGross: f64(15000)})
		}})
	if row.Paid != 25 || math.Abs(row.Earned-12500) > 0.01 {
		t.Fatalf("paid %.2f earned %.2f", row.Paid, row.Earned)
	}
	if row.Net != 0 || row.Carry <= 0 {
		t.Fatalf("net %.2f carry %.2f", row.Net, row.Carry)
	}
	pd := 10.0
	if PaidDays(31, 4, 0, 31, &pd) != 10 || PaidDays(31, 0, 31, 31, nil) != 0 {
		t.Fatal("paid days override or outside")
	}
}

func TestInstalmentsBlendWords(t *testing.T) {
	if got := Instalments(10000, 3); got[0] != 3333 || got[2] != 3334 {
		t.Fatalf("instalments %v", got)
	}
	a := Calc(Input{Type: Permanent, Gross: 20000})
	b := Calc(Input{Type: Permanent, Gross: 20000, Structure: &Structure{BasicPct: 40, HraPct: 50, PfEe: 12, PfEr: 12, PfCeil: 25000, PfCap: true, PfWage: "basic", EsiEe: 0.75, EsiEr: 3.25, EsiCeil: 21000}})
	m := Blend([]Result{a, b}, []float64{0.5, 0.5})
	if math.Abs(m.Basic-(a.Basic+b.Basic)/2) > 0.001 || !m.EsiOn {
		t.Fatalf("blend basic %.2f", m.Basic)
	}
	if w := RupeeWords(28166); w != "Twenty Eight Thousand One Hundred Sixty Six" {
		t.Fatalf("words %q", w)
	}
	if w := RupeeWords(46235.68); w != "Forty Six Thousand Two Hundred Thirty Five and Sixty Eight Paise" { // RUN-66: the words match the figure
		t.Fatal(w)
	}
	if w := RupeeWords(22206.94); w != "Twenty Two Thousand Two Hundred Six and Ninety Four Paise" {
		t.Fatal(w)
	}
	if w := RupeeWords(0.5); w != "Zero and Fifty Paise" {
		t.Fatal(w)
	}
	if w := RupeeWords(12345678); w != "One Crore Twenty Three Lakh Forty Five Thousand Six Hundred Seventy Eight" {
		t.Fatalf("words %q", w)
	}
	if Inr(1234567.5) != "₹12,34,567.50" || Inr(999) != "₹999" {
		t.Fatalf("inr %s %s", Inr(1234567.5), Inr(999))
	}
	ch := RunChecks([]CheckRow{{Name: "A", Net: 100, Prev: 100}, {Name: "B", Net: 0, Prev: 50}}, LineTotals{})
	if ch[0].OK || ch[2].OK || ch[1].OK {
		t.Fatalf("checks %+v", ch)
	}
	// A pilot leaves out other people's pay lines: the check says so instead of hiding them.
	ch = RunChecks([]CheckRow{{Name: "A", Net: 100}}, LineTotals{N: 1, Ded: 1510, OutN: 1, OutEarn: 30000, Pilot: true})
	if want := "1 line · additions ₹0 · deductions ₹1,510. Not in this pilot: 1 line (additions ₹30,000 · deductions ₹0) for people outside it; the full dry run pays them"; ch[4].Detail != want {
		t.Fatalf("pilot lines %q", ch[4].Detail)
	}
}
