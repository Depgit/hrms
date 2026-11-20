package calc

import "math"

// Income tax estimate and monthly salary TDS (spec section 16). Surcharge is not included.

type slab struct{ upTo, rate float64 }

var (
	NewRegimeSlabs = []slab{{400000, 0}, {800000, .05}, {1200000, .10}, {1600000, .15}, {2000000, .20}, {2400000, .25}, {math.Inf(1), .30}}
	OldRegimeSlabs = []slab{{250000, 0}, {500000, .05}, {1000000, .20}, {math.Inf(1), .30}}
)

// SectionCaps are the per-group limits (0 = no limit). Employee PF counts under 80C.
var SectionCaps = map[string]float64{"80C": 150000, "80CCD(1B)": 50000, "80D": 0, "80E": 0, "80G": 0, "80TTA": 10000, "24(b)": 200000, "LTA": 0}

var MetroCities = map[string]bool{"Delhi": true, "Mumbai": true, "Kolkata": true, "Chennai": true}

// TaxItem is one declaration line (prototype TSEC); Cap 0 = only the section's limit applies.
type TaxItem struct {
	ID    string  `json:"id"`
	Group string  `json:"group"`
	Name  string  `json:"name"`
	Cap   float64 `json:"cap"`
}

var TaxItems = []TaxItem{
	{"c_ppf", "80C", "Public Provident Fund (PPF)", 0}, {"c_elss", "80C", "ELSS mutual funds", 0}, {"c_life", "80C", "Life insurance premium", 0},
	{"c_tuit", "80C", "Children tuition fees", 0}, {"c_hlp", "80C", "Home loan principal repayment", 0}, {"c_nsc", "80C", "NSC or 5-year tax saver FD", 0},
	{"c_ssy", "80C", "Sukanya Samriddhi", 0}, {"c_oth", "80C", "Other 80C investment", 0},
	{"n_1b", "80CCD(1B)", "NPS, additional contribution", 50000},
	{"d_self", "80D", "Health insurance: self, spouse, children", 25000}, {"d_par", "80D", "Health insurance: parents (up to 50,000 if senior citizen)", 50000},
	{"d_chk", "80D", "Preventive health check-up", 5000},
	{"e_edu", "80E", "Education loan interest", 0}, {"g_don", "80G", "Donations", 0}, {"t_sav", "80TTA", "Savings account interest", 10000},
	{"h_int", "24(b)", "Home loan interest, self-occupied", 200000}, {"l_lta", "LTA", "Leave travel allowance (LTA)", 0},
	{"hra", "HRA", "House rent paid in the year", 0},
}

var TaxGroupNames = map[string]string{"80C": "Section 80C", "80CCD(1B)": "Section 80CCD(1B)", "80D": "Section 80D, medical insurance", "80E": "Section 80E",
	"80G": "Section 80G", "80TTA": "Section 80TTA / 80TTB", "24(b)": "Section 24(b), home loan interest", "LTA": "Leave travel allowance", "HRA": "House rent allowance"}

// GroupDeductions caps each declared item and adds them up by section; rent (HRA) is kept apart.
func GroupDeductions(items map[string]float64) (groups map[string]float64, rent float64) {
	return GroupDeductionsWith(items, TaxItems)
}

// GroupDeductionsWith is GroupDeductions over a given list of items (Payroll's limits, with the
// items switched off left out).
func GroupDeductionsWith(items map[string]float64, list []TaxItem) (groups map[string]float64, rent float64) {
	groups = map[string]float64{}
	for _, it := range list {
		v := items[it.ID]
		if v <= 0 {
			continue
		}
		if it.ID == "hra" {
			rent = v
			continue
		}
		if it.Cap > 0 {
			v = math.Min(v, it.Cap)
		}
		groups[it.Group] += v
	}
	return groups, rent
}

// FY is the financial year label of a month: 2026-10 → 2026-27.
func FY(ym string) string {
	y := atoi(ym[:4])
	if atoi(ym[5:7]) < 4 {
		y--
	}
	return itoa(y) + "-" + pad2(int64((y+1)%100))
}

// FYMonths lists April to March of a financial year label.
func FYMonths(fy string) []string {
	y := atoi(fy[:4])
	out := []string{}
	for i := 0; i < 12; i++ {
		m := 4 + i
		yy := y
		if m > 12 {
			m -= 12
			yy++
		}
		out = append(out, itoa(yy)+"-"+pad2(int64(m)))
	}
	return out
}

// Quarter of a month in the financial year: Apr-Jun 1 ... Jan-Mar 4.
func Quarter(ym string) int {
	m := atoi(ym[5:7])
	switch {
	case m >= 4 && m <= 6:
		return 1
	case m >= 7 && m <= 9:
		return 2
	case m >= 10:
		return 3
	}
	return 4
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func SlabTax(income float64, slabs []slab) float64 {
	tax, lo := 0.0, 0.0
	for _, s := range slabs {
		if income > lo {
			tax += (math.Min(income, s.upTo) - lo) * s.rate
		}
		lo = s.upTo
	}
	return tax
}

// HraExemption = min(HRA, rent - 10% basic, 50% basic metro / 40% non-metro).
func HraExemption(hraYear, rentYear, basicYear float64, metro bool) float64 {
	if rentYear <= 0 {
		return 0
	}
	pct := 0.4
	if metro {
		pct = 0.5
	}
	return math.Max(0, math.Min(hraYear, math.Min(rentYear-0.1*basicYear, pct*basicYear)))
}

type TaxInput struct {
	SalaryYear, BasicYear, HraYear, EmployeePfYear float64
	OtherIncome, PrevEmployerIncome, RentYear      float64
	Metro                                          bool
	Deductions                                     map[string]float64 // by section group
}

type TaxResult struct {
	Income        float64 `json:"income"`
	NewTaxable    float64 `json:"newTaxable"`
	OldTaxable    float64 `json:"oldTaxable"`
	OldDeductions float64 `json:"oldDeductions"`
	HraExempt     float64 `json:"hraExempt"`
	NewTax        float64 `json:"newTax"`
	OldTax        float64 `json:"oldTax"`
}

func withCess(v float64) float64 { return math.Round(v * 1.04) }

func TaxEstimate(t TaxInput) TaxResult {
	income := t.SalaryYear + t.OtherIncome + t.PrevEmployerIncome
	groups := map[string]float64{}
	for k, v := range t.Deductions {
		groups[k] = v
	}
	groups["80C"] += t.EmployeePfYear
	ded := 0.0
	for g, v := range groups {
		if c := SectionCaps[g]; c > 0 {
			v = math.Min(v, c)
		}
		ded += v
	}
	hra := HraExemption(t.HraYear, t.RentYear, t.BasicYear, t.Metro)

	newTaxable := math.Max(0, income-75000)
	nTax := SlabTax(newTaxable, NewRegimeSlabs)
	if newTaxable <= 1200000 {
		nTax = 0 // rebate
	} else {
		nTax = math.Min(nTax, newTaxable-1200000) // marginal relief
	}
	oldTaxable := math.Max(0, income-50000-ded-hra)
	oTax := SlabTax(oldTaxable, OldRegimeSlabs)
	if oldTaxable <= 500000 {
		oTax = math.Max(0, oTax-12500)
	}
	return TaxResult{income, newTaxable, oldTaxable, ded, hra, withCess(nTax), withCess(oTax)}
}

// MonthlyTds (Section 192): tax still due spread over the months left; recomputed each run.
func MonthlyTds(annualTax, alreadyDeducted, prevEmployerTds float64, monthsLeftIncludingThis int) float64 {
	due := math.Max(0, annualTax-alreadyDeducted-prevEmployerTds)
	if monthsLeftIncludingThis < 1 {
		monthsLeftIncludingThis = 1
	}
	return math.Round(due / float64(monthsLeftIncludingThis))
}
