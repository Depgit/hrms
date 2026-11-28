package calc

// The all-inclusive gross (owner, 26 Nov 2026; reworked the same evening after the owner saw the Compensation tab).
// From the month set (company_setting.allin_from) the monthly gross G that HR offers is the gross salary:
//
//   - Earnings: G split by the structure into Basic, HRA, Special allowance (…) and, when G is at most the bonus
//     ceiling and the employment type gets bonus, a "Statutory bonus" line (BonusPct of min(bonus wage, BonusCalc),
//     the bonus wage being the split's Basic). The bonus is INSIDE G: fixed gross F + bonus = G (AllInFixed finds F).
//   - Deductions from the employee: employee PF and ESI as always, and also the employer's PF (with EPS), the
//     employer's ESI, EDLI and the employer's LWF, each worked out normally on the wages of G's split (PF wage and its
//     ceiling, ESI wage = the fixed gross, ESI when G ≤ the ESI ceiling, EDLI on Basic up to ₹15,000, LWF on G with
//     its rates and caps). Net pay = G − all deductions (+ variable pay and the Reimbursement).
//   - The company bears only the EPFO admin charges, gratuity and PL encashment (and employer medical insurance where
//     it applies, as before). CTC = G + admin + gratuity + PL encashment + medical + variable target + Reimbursement.
//   - Variable pay and the Reimbursement stay outside G, paid every month by the days paid: variable = target × days
//     paid ÷ days (no HR entry); the Reimbursement as built on 25 Nov 2026.
//   - A part month: G earned = G × days paid ÷ days, split the same way (F and the bonus by the same share); the
//     deductions are worked out on the earned wages as always.
//   - A retainer (no PF, ESI, bonus or LWF) and a PF exempt / opted-out person simply have nothing of what does not apply.
//
// The employer's PF, ESI, EDLI and LWF are still the employer's contributions to the authorities (ECR, ESI challan,
// LWF): Result.ErPF, Eps, ErEpf, ErESI, Edli and LwfEr keep their amounts; only who bears them changes.

import "math"

// allInFull is G a month: the full-month amount (EligGross), else the gross given.
func allInFull(p Input) float64 {
	if p.EligGross != nil {
		return math.Max(0, *p.EligGross)
	}
	return math.Max(0, p.Gross+p.Inc)
}

// allInBonusOn says whether the statutory bonus is a line inside G (decided on G).
func allInBonusOn(S Structure, T TypeRules, g float64) bool {
	return T.Bonus && S.BonusOn && g <= S.BonusCeil
}

// AllInFixed is the full month's fixed gross F (what the structure splits into Basic, HRA, Special allowance…) for an
// all-inclusive gross G (EligGross when given, else Gross + Inc): G itself when there is no bonus, else the F with
// F + bonus(F) = G, found to the paisa (the bonus grows with F, so the search is plain).
func AllInFixed(p Input) float64 {
	S, T := rulesOf(p)
	g := allInFull(p)
	if g <= 0 {
		return 0
	}
	if !allInBonusOn(S, T, g) {
		return g
	}
	on := true
	q := p
	q.AllIn, q.Classic, q.Inc = false, false, 0
	q.bonusElig, q.bonusAmt, q.allInG, q.autoVar, q.ratio = &on, nil, g, false, nil
	q.VarPaid, q.VarTotal = nil, nil
	total := func(cents int64) float64 {
		f := float64(cents) / 100
		q.Gross, q.EligGross = f, &f
		return f + calcOne(q).Bonus
	}
	lo, hi := int64(0), int64(math.Round(g*100))
	for lo < hi { // the largest F with F + bonus ≤ G
		m := lo + (hi-lo+1)/2
		if total(m) <= g+1e-9 {
			lo = m
		} else {
			hi = m - 1
		}
	}
	return float64(lo) / 100
}

// calcAllIn is the month's calculation on an all-inclusive gross (see the top of this file).
func calcAllIn(p Input) Result {
	S, T := rulesOf(p)
	earn := math.Max(0, p.Gross+p.Inc)
	full := allInFull(p)
	ratio := 0.0
	if full > 0 {
		ratio = earn / full
	}
	f := AllInFixed(p)
	fe := f * ratio
	bonus := earn - fe // the bonus inside G earned: G's split by the same share (0 without a bonus)
	if bonus < 0.005 {
		bonus = 0
	}
	bonusIn := allInBonusOn(S, T, full)
	g := full
	q := p
	q.AllIn = false
	q.Gross, q.Inc, q.EligGross = fe, 0, &g // ESI and medical decided on G
	q.bonusElig, q.bonusAmt, q.allInG, q.autoVar, q.ratio = &bonusIn, &bonus, full, true, &ratio
	q.VarPaid, q.VarTotal = nil, nil // HR-entered payouts are not used: variable pay is automatic
	r := calcOne(q)
	if !bonusIn {
		r.Bonus = 0
	}
	// LWF on the gross G earned (its rates and caps), the employer's share deducted from the employee.
	if r.LwfOn {
		r.LwfEe = LwfAmountFor(p.Lwf, "ee", earn, p.Month)
		r.LwfEr = LwfAmountFor(p.Lwf, "er", earn, p.Month)
	}
	r.AllIn, r.Offered, r.OfferedFull, r.BonusIn = true, earn, full, r.Bonus > 0
	r.Gross = fe + r.Bonus // = G earned
	if r.Fee != 0 {
		r.Fee = r.Gross
	}
	r.ErDed = r.ErPF + r.ErESI + r.Edli + r.LwfEr
	r.Ded = r.EePF + r.EeESI + r.LwfEe + r.Meal + r.Accom + r.Tds + r.CompDed + r.ErDed
	r.TotalEarn = r.Gross + r.VarPay + r.Reimb
	r.Net = r.TotalEarn - r.Ded
	r.EmployerCost = r.Admin + r.Med + r.Grat
	r.CTC = earn + r.Admin + r.Grat + r.PlEnc + r.Med + r.Variable + r.Reimb
	return r
}
