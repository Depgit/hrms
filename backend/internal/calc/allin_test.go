package calc

import (
	"math"
	"math/rand"
	"testing"
)

// The all-inclusive gross (owner, 26 Nov 2026, reworked): G is the gross salary, split into Basic, HRA, Special
// allowance and (G ≤ the bonus ceiling) the statutory bonus; the employer's PF, ESI, EDLI and LWF are deducted from
// the employee; the company bears admin, gratuity, PL encashment and medical; CTC = G + those + variable + Reimbursement.

var permRules = TypeRules{Statutory: true, LWF: true, Bonus: true, Grat: true, PlEnc: true, Variable: true, Ded: "all", Leave: true, Docs: true}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.005 {
		t.Fatalf("%s = %.4f, want %.4f", what, got, want)
	}
}

// checkAllIn works out in on the all-inclusive gross and checks what holds for every month: the earnings add up to G
// earned (bonus inside, never on top), the deductions are the employee's and the employer's shares, net and CTC.
func checkAllIn(t *testing.T, in Input) Result {
	t.Helper()
	in.AllIn = true
	r := Calc(in)
	if !r.AllIn {
		t.Fatal("not worked out as all-inclusive")
	}
	g := in.Gross + in.Inc // G earned this month
	near(t, "gross", r.Gross, g)
	near(t, "offered", r.Offered, g)
	if r.Fee == 0 {
		near(t, "Basic + HRA + SA + bonus", r.Basic+r.Hra+r.Sa+r.Bonus, g)
	}
	near(t, "total earnings", r.TotalEarn, g+r.VarPay+r.Reimb)
	near(t, "employer's shares deducted", r.ErDed, r.ErPF+r.ErESI+r.Edli+r.LwfEr)
	near(t, "deductions", r.Ded, r.EePF+r.EeESI+r.LwfEe+r.Meal+r.Accom+r.Tds+r.CompDed+r.ErDed)
	near(t, "net", r.Net, r.TotalEarn-r.Ded)
	near(t, "employer cost", r.EmployerCost, r.Admin+r.Grat+r.Med)
	near(t, "CTC", r.CTC, g+r.Admin+r.Grat+r.PlEnc+r.Med+r.Variable+r.Reimb)
	return r
}

func TestAllInAboveBonusCeiling(t *testing.T) {
	S := DefaultStructure
	r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 28000, Structure: &S})
	if r.Bonus != 0 || r.BonusIn {
		t.Fatalf("bonus %.2f above the bonus ceiling", r.Bonus)
	}
	// G is split as any gross: Basic 14,000, HRA 7,000, SA 7,000; no ESI above the ceiling.
	near(t, "Basic", r.Basic, 14000)
	near(t, "HRA", r.Hra, 7000)
	near(t, "SA", r.Sa, 7000)
	if r.EsiOn || r.ErESI != 0 || r.EeESI != 0 {
		t.Fatal("ESI above the ceiling")
	}
	near(t, "employee PF", r.EePF, 1680)
	near(t, "employer PF", r.ErPF, 1680)
	near(t, "EDLI", r.Edli, 70)
	near(t, "admin", r.Admin, 70)
	near(t, "net", r.Net, 28000-1680-1680-70)
	// The old rule: the same split, the employer's shares a company cost on top.
	old := Calc(Input{Type: Permanent, Rules: &permRules, Gross: 28000, Structure: &S})
	near(t, "old net", old.Net, 28000-1680)
	near(t, "medical insurance (above the ESI ceiling, a company cost)", r.Med, 500)
	near(t, "CTC", r.CTC, 28000+70+r.Grat+r.PlEnc+500)
}

func TestAllInBonusInsideAtOrBelowCeiling(t *testing.T) {
	S := DefaultStructure
	for _, g := range []float64{21000, 18000, 12000} {
		r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: g, Structure: &S})
		if !r.BonusIn || r.Bonus <= 0 {
			t.Fatalf("G %.0f: no bonus inside G", g)
		}
		near(t, "bonus", r.Bonus, math.Min(r.Basic, 7000)*S.BonusPct/100)
		near(t, "Basic", r.Basic, (g-r.Bonus)*S.BasicPct/100)
		if !r.EsiOn {
			t.Fatalf("G %.0f: ESI should apply", g)
		}
		esiWage := g - r.Bonus // the bonus is not in the ESI wage
		near(t, "employer ESI", r.ErESI, math.Ceil(esiWage*S.EsiEr/100-1e-9))
		near(t, "employee ESI", r.EeESI, math.Ceil(esiWage*S.EsiEe/100-1e-9))
		near(t, "net", r.Net, g-r.EePF-r.ErPF-r.EeESI-r.ErESI-r.Edli)
		// Not on top: the old rule paid G + bonus.
		old := Calc(Input{Type: Permanent, Rules: &permRules, Gross: g, Structure: &S})
		near(t, "old total earnings", old.TotalEarn, g+old.Bonus)
		if r.TotalEarn > g+1e-9 {
			t.Fatalf("G %.0f: earnings %.2f above G", g, r.TotalEarn)
		}
	}
	// Bonus decided on G: none just above the ceiling.
	if r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 21001, Structure: &S}); r.Bonus != 0 || r.BonusIn {
		t.Fatalf("G 21,001: bonus %.2f", r.Bonus)
	}
	// Bonus off for the type: none.
	noBonus := permRules
	noBonus.Bonus = false
	if r := checkAllIn(t, Input{Type: Permanent, Rules: &noBonus, Gross: 15000, Structure: &S}); r.Bonus != 0 {
		t.Fatalf("bonus %.2f with bonus off for the type", r.Bonus)
	}
}

func TestAllInEsiOnG(t *testing.T) {
	S := DefaultStructure
	// ESI on G ≤ the ceiling, off above (G, not the gross less the bonus, decides).
	if r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 21000, Structure: &S}); !r.EsiOn {
		t.Fatal("G 21,000: no ESI")
	}
	if r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 21500, Structure: &S}); r.EsiOn || r.ErESI != 0 {
		t.Fatal("G 21,500: ESI on")
	}
	// ESI kept to the end of its contribution period: on above the ceiling, both shares deducted.
	r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 24000, Structure: &S, EsiKeep: true})
	if !r.EsiOn || r.ErESI == 0 || r.EeESI == 0 {
		t.Fatal("ESI kept: not deducted")
	}
	for g := 5000.0; g <= 60000; g += 37 {
		checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: g, Structure: &S})
	}
}

func TestAllInPfWageCapAndLwf(t *testing.T) {
	S := DefaultStructure // PF wage ceiling ₹25,000, applied
	lwf := LwfSeed["Haryana"]
	r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 80000, Structure: &S, Lwf: &lwf, Month: 11})
	if r.PfBase != S.PfCeil || r.ErPF != 3000 || r.EePF != 3000 {
		t.Fatalf("PF wage %.2f, employer %.2f, employee %.2f; want the ₹25,000 ceiling, 3,000 each", r.PfBase, r.ErPF, r.EePF)
	}
	near(t, "admin", r.Admin, 125)
	near(t, "EDLI", r.Edli, 75)
	near(t, "employer LWF (0.4% of G, max 68)", r.LwfEr, 68)
	near(t, "employee LWF (0.2% of G, max 34)", r.LwfEe, 34)
	near(t, "net", r.Net, 80000-3000-3000-75-34-68)
	// A part month: G earned split the same way, the deductions on the earned wages (ceilings as always).
	full := 80000.0
	p := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: full * 10 / 30, EligGross: &full, Structure: &S, Lwf: &lwf, Month: 11})
	near(t, "part-month Basic", p.Basic, full*10/30/2)
	near(t, "part-month employer PF", p.ErPF, math.Round(p.Basic*0.12))
	near(t, "part-month EDLI", p.Edli, p.Basic*0.005)
}

func TestAllInPfOptOutAndRetainer(t *testing.T) {
	S := DefaultStructure
	// PF exempt, all Basic: no PF or EDLI deducted, no admin; above the ESI ceiling nothing else but LWF.
	r := checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 30000, Structure: &S, PfExempt: true, AllBasic: true})
	if r.ErPF != 0 || r.Admin != 0 || r.Edli != 0 || r.EePF != 0 {
		t.Fatal("PF on a PF exempt person")
	}
	near(t, "Basic", r.Basic, 30000)
	near(t, "net", r.Net, 30000)
	// PF exempt below the ESI ceiling: bonus inside, both ESI shares deducted.
	r = checkAllIn(t, Input{Type: Permanent, Rules: &permRules, Gross: 18000, Structure: &S, PfExempt: true, AllBasic: true})
	if !r.EsiOn || r.ErPF != 0 || r.Bonus <= 0 {
		t.Fatal("want ESI and a bonus, no PF")
	}
	near(t, "bonus on all-Basic", r.Bonus, 7000*S.BonusPct/100)
	// Retainer: nothing statutory; the fee is G, TDS 1% of it.
	ret := DefaultTypeRules[Retainer]
	pct := 1.0
	r = checkAllIn(t, Input{Type: Retainer, Rules: &ret, Gross: 50000, Structure: &S, TdsPct: &pct})
	near(t, "fee", r.Fee, 50000)
	near(t, "TDS", r.Tds, 500)
	near(t, "net", r.Net, 49500)
	if r.ErDed != 0 || r.Bonus != 0 || r.Admin != 0 {
		t.Fatal("retainer: employer shares, bonus or admin")
	}
	near(t, "retainer CTC", r.CTC, 50000)
}

func TestAllInVariableProRataAndReimbursement(t *testing.T) {
	S := DefaultStructure
	S.Comps, S.Designed, S.Converted = TargetComponents(S), true, true
	full := 26000.0
	for _, paidDays := range []float64{30, 20, 7.5} {
		ratio := paidDays / 30
		earned := full * ratio
		target := 5000.0
		in := Input{Type: Permanent, Rules: &permRules, Gross: earned, EligGross: &full, Structure: &S, VarTarget: &target,
			EmpAmts: map[string]float64{ReimbCode: 3000}, VarPaid: map[string]float64{"var": 99999}}
		r := checkAllIn(t, in)
		// Variable: target × days paid ÷ days, every month, whatever HR entered.
		near(t, "variable paid", r.VarPay, target*paidDays/30)
		near(t, "variable accrued", r.Variable, target*paidDays/30)
		if r.VarCycle != 1 {
			t.Fatalf("variable cycle %d", r.VarCycle)
		}
		// Reimbursement unchanged: amount × days paid ÷ days, as without the rule.
		old := in
		near(t, "reimbursement", r.Reimb, Calc(old).Reimb)
		near(t, "reimbursement", r.Reimb, 3000*ratio)
		// Neither is in the PF or ESI wage: the same deductions as without them.
		bare := Calc(Input{AllIn: true, Type: Permanent, Rules: &permRules, Gross: earned, EligGross: &full, Structure: &S})
		near(t, "deductions", r.Ded, bare.Ded)
	}
	// A target in %: of G.
	pct := 10.0
	r := Calc(Input{AllIn: true, Type: Permanent, Rules: &permRules, Gross: full, Structure: &S, VarTargetPct: &pct})
	near(t, "variable from % of G", r.VarPay, 2600)
	// A rate structure (not designed): the variable % of G, monthly.
	R := DefaultStructure
	R.VarPct, R.VarCycle = 10, 3
	r = Calc(Input{AllIn: true, Type: Permanent, Rules: &permRules, Gross: full / 2, EligGross: &full, Structure: &R, Month: 5, JoinMonth: 1})
	near(t, "variable % monthly", r.VarPay, 1300)
}

// TestAllInOffUnchanged: without the setting the calculation is exactly as before (the all-inclusive fields empty).
func TestAllInOffUnchanged(t *testing.T) {
	rng := rand.New(rand.NewSource(26))
	S := DefaultStructure
	for i := 0; i < 2000; i++ {
		g := float64(5000 + rng.Intn(90000))
		r := Calc(Input{Type: Permanent, Rules: &permRules, Gross: g, Structure: &S})
		if r.AllIn || r.Offered != 0 || r.ErDed != 0 {
			t.Fatal("all-inclusive fields set without the rule")
		}
		if r.Gross != g {
			t.Fatalf("gross %.2f ≠ %.2f", r.Gross, g)
		}
	}
}
