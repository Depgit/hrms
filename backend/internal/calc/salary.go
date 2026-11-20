// Package calc holds the pure calculation library (spec section 14): salary,
// statutory items, tax, attendance status and settlement. No I/O; every function is
// deterministic and unit tested against the spec's golden values.
package calc

import (
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type EmploymentType string

const (
	Permanent   EmploymentType = "Permanent"
	Retainer    EmploymentType = "Retainer"
	Contractual EmploymentType = "Contractual"
)

// Structure is one salary structure version. All rates are admin-editable.
type Structure struct {
	BasicPct  float64 `json:"basicPct"`
	HraPct    float64 `json:"hraPct"`
	VarPct    float64 `json:"varPct"`
	VarCycle  int     `json:"varCycle"`
	PfEe      float64 `json:"pfEe"`
	PfEr      float64 `json:"pfEr"`
	PfCeil    float64 `json:"pfCeil"`
	PfCap     bool    `json:"pfCap"`
	PfWage    string  `json:"pfWage"` // "basic" or "basicSa"
	EpfoAdm   float64 `json:"epfoAdm"`
	EsiEe     float64 `json:"esiEe"`
	EsiEr     float64 `json:"esiEr"`
	EsiCeil   float64 `json:"esiCeil"`
	MedOn     bool    `json:"medOn"`
	MedPct    float64 `json:"medPct"`
	MedMax    float64 `json:"medMax"`
	Grat      float64 `json:"grat"`
	PlDays    float64 `json:"plDays"`
	Denom     string  `json:"denom"` // "calendar" (actual days in the month), "30" or "26"
	BonusOn   bool    `json:"bonusOn"`
	BonusCeil float64 `json:"bonusCeil"`
	BonusPct  float64 `json:"bonusPct"`
	BonusCalc float64 `json:"bonusCalc"`
	TdsRate   float64 `json:"tdsRate"`
	// Comps are the structure's pay components (components.go): a designed structure's own (Designed), or the
	// template converted from the rates above (migration 014; kept in step with them). None: converted on the fly.
	Comps    []Component `json:"comps,omitempty"`
	Designed bool        `json:"designed,omitempty"`
	// Converted: a percentage structure moved to the manual variable target (migration 017, 17 Nov 2026). It is
	// Designed, and its components still follow the rates above (TargetComponents); VarCycle is the variable
	// component's payout cycle and VarPct is not used.
	Converted bool `json:"converted,omitempty"`
}

// EpsCeil is the statutory wage ceiling for EPS and EDLI (₹15,000): EPS is 8.33% of at most this.
const EpsCeil = 15000.0

// EpsShare is the employer's pension (EPS) share: 8.33% of PF wages up to the statutory ₹15,000, to the rupee, never
// more than the employer's PF share; the rest of that share goes to EPF. The salary calculation and the ECR both use it.
func EpsShare(pfWages, erPF float64) float64 {
	if erPF <= 0 {
		return 0
	}
	return math.Min(math.Round(math.Min(math.Round(pfWages), EpsCeil)*0.0833), erPF)
}

// DefaultStructure is structure version 1 from the spec.
var DefaultStructure = Structure{
	BasicPct: 50, HraPct: 50, VarPct: 0, VarCycle: 1,
	PfEe: 12, PfEr: 12, PfCeil: 25000, PfCap: true, PfWage: "basic", EpfoAdm: 0.5,
	EsiEe: 0.75, EsiEr: 3.25, EsiCeil: 21000,
	MedOn: true, MedPct: 3, MedMax: 500,
	Grat: 4.81, PlDays: 15, Denom: "calendar",
	BonusOn: true, BonusCeil: 21000, BonusPct: 8.33, BonusCalc: 7000,
	TdsRate: 10,
}

// TypeRules is what an employment type switches on or off.
type TypeRules struct {
	Statutory bool   `json:"statutory"`
	LWF       bool   `json:"lwf"`
	Bonus     bool   `json:"bonus"`
	Grat      bool   `json:"grat"`
	PlEnc     bool   `json:"plEnc"`
	Variable  bool   `json:"variable"`
	Ded       string `json:"ded"` // "all", "tds" or "none"
	Leave     bool   `json:"leave"`
	Docs      bool   `json:"docs"`
	// LeaveTypes: with Leave on, only these leave types (empty: every type the leave plan gives). Leave off means
	// no leave at all. Set under Compensation › Rules by employment type.
	LeaveTypes []string `json:"leaveTypes,omitempty"`
}

// LeaveTypeAllowed says whether the employment type's rules let its people have leave type name: none when leave
// is off, the chosen types when some are chosen, else every type (the leave plan decides the rest).
func (r TypeRules) LeaveTypeAllowed(name string) bool {
	if !r.Leave {
		return false
	}
	if len(r.LeaveTypes) == 0 {
		return true
	}
	for _, t := range r.LeaveTypes {
		if t == name {
			return true
		}
	}
	return false
}

// DefaultTypeRules are the rules a new installation starts with (stored in comp_type_rule and
// editable in Compensation). Read the rules in force with RulesFor, never this map directly.
var DefaultTypeRules = map[EmploymentType]TypeRules{
	Permanent:   {Statutory: true, LWF: true, Bonus: true, Grat: true, PlEnc: true, Variable: true, Ded: "all", Leave: true, Docs: true},
	Contractual: {Bonus: true, Variable: true, Ded: "none"},
	Retainer:    {Ded: "tds"},
}

// typeRules holds the stored rules once the API has loaded them (SetTypeRules).
var typeRules atomic.Pointer[map[EmploymentType]TypeRules]

// RulesFor is the employment type's rules in force: the stored rules, else the defaults.
func RulesFor(t EmploymentType) TypeRules {
	if m := typeRules.Load(); m != nil {
		if r, ok := (*m)[t]; ok {
			return r
		}
	}
	return DefaultTypeRules[t]
}

// TypeRulesNow is every type's rules in force.
func TypeRulesNow() map[EmploymentType]TypeRules {
	out := map[EmploymentType]TypeRules{}
	for t := range DefaultTypeRules {
		out[t] = RulesFor(t)
	}
	return out
}

// SetTypeRules makes stored rules the rules in force; types not in m keep their defaults.
func SetTypeRules(m map[EmploymentType]TypeRules) {
	cp := make(map[EmploymentType]TypeRules, len(m))
	for k, v := range m {
		cp[k] = v
	}
	typeRules.Store(&cp)
}

type LwfAmount struct {
	Mode string  `json:"mode"` // "fixed" or "pct"
	Val  float64 `json:"val"`
	Max  float64 `json:"max"` // 0 = no maximum
}

// LwfRule is a state's labour welfare fund rule.
type LwfRule struct {
	On     bool      `json:"on"`
	Ee     LwfAmount `json:"ee"`
	Er     LwfAmount `json:"er"`
	Freq   string    `json:"freq"`   // "monthly", "half" or "year"
	Months string    `json:"months"` // e.g. "6,12"
}

// LwfSeed holds the seeded rules, keyed by state name.
var LwfSeed = map[string]LwfRule{
	"Delhi":      {On: true, Ee: LwfAmount{"fixed", 0.75, 0}, Er: LwfAmount{"fixed", 2.25, 0}, Freq: "half", Months: "6,12"},
	"Haryana":    {On: true, Ee: LwfAmount{"pct", 0.2, 34}, Er: LwfAmount{"pct", 0.4, 68}, Freq: "monthly"},
	"Chandigarh": {On: true, Ee: LwfAmount{"fixed", 5, 0}, Er: LwfAmount{"fixed", 20, 0}, Freq: "monthly"},
}

// Input to Calc. Pointer fields are optional overrides; nil means "use the structure".
type Input struct {
	Type      EmploymentType
	Gross     float64  // earned (prorated) monthly gross
	EligGross *float64 // full gross for ESI / medical / bonus eligibility; defaults to Gross
	Inc       float64
	Month     int // 1-12; 0 = annualised view
	JoinMonth int
	VarPct    *float64
	VarCycle  *int
	VarTotal  *float64 // payout amount override (sum of the cycle's prorated accruals)
	Meal      float64
	Accom     float64
	TdsPct    *float64
	EsiKeep   bool // ESI continues to the end of the contribution period
	Structure *Structure
	Lwf       *LwfRule
	Rules     *TypeRules
	// PfExempt: a joiner exempt from PF (gross above the company's threshold, no existing UAN; owner, 14 Nov 2026):
	// no employee or employer PF, EPS, EDLI or EPFO admin. ESI, bonus and the rest are unchanged.
	PfExempt bool
	// AllBasic: the whole fixed gross is paid as Basic (no HRA, no special allowance; owner, 19 Nov 2026), for a PF
	// exempt person who chose not to have PF deducted (employee_pay.pf_choice "optout", api/pfuan.go). The variable
	// pay, component deductions, ESI, LWF, TDS and bonus rules are worked out as for everyone else.
	AllBasic bool
	// Classic runs the calculation as it was before pay components (the rates only), for the before/after
	// comparison (hrms pay-compare). Live pay never sets it.
	Classic bool
	// Designed structures (components.go): the monthly CTC for "% of CTC" (0: fixed gross + VarTarget), the person's
	// monthly variable target (nil: by each component's method) and per-component targets, and the variable amounts
	// HR entered to pay this month, by component code.
	CTC        float64
	VarTarget  *float64
	VarTargets map[string]float64
	// VarTargetPct is the person's monthly variable target as a % of the full fixed monthly gross (used when
	// VarTarget is nil): the target "in %" mode (17 Nov 2026).
	VarTargetPct *float64
	VarPaid      map[string]float64
}

type Result struct {
	Gross        float64 `json:"gross"`
	Basic        float64 `json:"basic"`
	Hra          float64 `json:"hra"`
	Sa           float64 `json:"sa"`
	Fee          float64 `json:"fee,omitempty"` // Retainer: the retainer fee, the whole fixed pay (no Basic, HRA or Special allowance)
	PfBase       float64 `json:"pfBase"`
	Variable     float64 `json:"variable"`
	VarPay       float64 `json:"varPay"`
	VarCycle     int     `json:"varCycle"`
	EePF         float64 `json:"eePF"`
	ErPF         float64 `json:"erPF"`
	Eps          float64 `json:"eps"`   // the employer's share to the pension scheme: 8.33% of PF wages up to EpsCeil (≤ ₹1,250)
	ErEpf        float64 `json:"erEpf"` // the rest of the employer's share, to EPF
	Admin        float64 `json:"admin"`
	Edli         float64 `json:"edli"`
	PfExempt     bool    `json:"pfExempt,omitempty"` // PF exempt joiner: no PF, EPS, EDLI or admin
	EsiOn        bool    `json:"esiOn"`
	EeESI        float64 `json:"eeESI"`
	ErESI        float64 `json:"erESI"`
	MedOn        bool    `json:"medOn"`
	Med          float64 `json:"med"`
	Bonus        float64 `json:"bonus"`
	LwfOn        bool    `json:"lwfOn"`
	LwfEe        float64 `json:"lwfEe"`
	LwfEr        float64 `json:"lwfEr"`
	Meal         float64 `json:"meal"`
	Accom        float64 `json:"accom"`
	Tds          float64 `json:"tds"`
	Ded          float64 `json:"ded"`
	TotalEarn    float64 `json:"totalEarn"`
	Net          float64 `json:"net"`
	Grat         float64 `json:"grat"`
	PlEnc        float64 `json:"plEnc"`
	EmployerCost float64 `json:"employerCost"`
	CTC          float64 `json:"ctc"`
	// Designed structures only: each component's amount, the component deductions, and the earnings not taxable.
	Comps   []CompLine `json:"comps,omitempty"`
	CompDed float64    `json:"compDed,omitempty"`
	NonTax  float64    `json:"nonTax,omitempty"`
}

// LwfAmountFor returns the employee ("ee") or employer ("er") LWF for a month.
// With month 0 a half-yearly or yearly amount is shown as its monthly average.
func LwfAmountFor(r *LwfRule, side string, gross float64, month int) float64 {
	if r == nil || !r.On {
		return 0
	}
	s := r.Ee
	if side == "er" {
		s = r.Er
	}
	base := s.Val
	if s.Mode == "pct" {
		base = gross * s.Val / 100
		if s.Max > 0 {
			base = math.Min(base, s.Max)
		}
	}
	if r.Freq == "half" || r.Freq == "year" {
		var months []int
		for _, p := range strings.Split(r.Months, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
				months = append(months, n)
			}
		}
		if month == 0 {
			return base * float64(len(months)) / 12
		}
		for _, m := range months {
			if m == month {
				return base
			}
		}
		return 0
	}
	return base
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Calc is the salary engine for one month (prototype `calc`).
func Calc(p Input) Result {
	S := DefaultStructure
	if p.Structure != nil {
		S = *p.Structure
	}
	T := RulesFor(p.Type)
	if p.Rules != nil {
		T = *p.Rules
	}
	stat := b2f(T.Statutory)
	gross := math.Max(0, p.Gross+p.Inc)
	elig := gross
	if p.EligGross != nil {
		elig = *p.EligGross
	}
	jm := p.JoinMonth
	if jm == 0 {
		jm = 1
	}

	var h head
	if p.Classic {
		h = classicHead(p, S, T, gross, jm)
	} else {
		h = compHead(p, S, T, gross, elig, jm)
	}
	variable, varPay, cyc := h.variable, h.varPay, h.cyc
	basic, hra, sa, pfWage := h.basic, h.hra, h.sa, h.pfWage
	pfBase, adminBase := pfWage, basic
	if S.PfCap {
		pfBase = math.Min(pfWage, S.PfCeil)
		adminBase = math.Min(basic, S.PfCeil)
	}
	// The member's PF shares to the rupee, as EPFO works them out, so payslip, register and ECR agree (RUN-57). EDLI is on wages up to
	// the statutory ₹15,000 (EpsCeil), whatever the company's PF ceiling (REAL-40).
	pf := stat
	if p.PfExempt {
		pf = 0
	}
	eePF := math.Round(pf * pfBase * S.PfEe / 100)
	erPF := math.Round(pf * pfBase * S.PfEr / 100)
	admin := pf * adminBase * S.EpfoAdm / 100
	edli := pf * math.Min(adminBase, EpsCeil) * 0.5 / 100
	eps := EpsShare(pfBase, erPF)

	esiElig, esiGross := elig, gross
	if h.nonESI != 0 { // components outside the ESI wage (designed structures)
		esiGross = gross - h.nonESI
	}
	esiOn := T.Statutory && (esiElig <= S.EsiCeil || p.EsiKeep)
	var eeESI, erESI float64
	if esiOn {
		eeESI = math.Ceil(esiGross*S.EsiEe/100 - 1e-9)
		erESI = math.Ceil(esiGross*S.EsiEr/100 - 1e-9)
	}
	// ESI or employer medical cover, never both: ESI kept to the end of its period replaces medical.
	medOn := T.Statutory && S.MedOn && esiElig > S.EsiCeil && !esiOn
	med := 0.0
	if medOn {
		med = math.Min(gross*S.MedPct/100, S.MedMax)
	}
	bonus := 0.0
	if T.Bonus && S.BonusOn && elig <= S.BonusCeil {
		calcBase := S.BonusCalc
		if calcBase <= 0 {
			calcBase = 7000
		}
		bonus = math.Min(h.bonusBase, calcBase) * S.BonusPct / 100
	}

	lwfOn := T.LWF && p.Lwf != nil && p.Lwf.On
	var lwfEe, lwfEr float64
	if lwfOn {
		lwfEe = LwfAmountFor(p.Lwf, "ee", gross, p.Month)
		lwfEr = LwfAmountFor(p.Lwf, "er", gross, p.Month)
	}

	var meal, accom, tds float64
	if T.Ded == "all" {
		meal, accom = p.Meal, p.Accom
	}
	if T.Ded == "tds" {
		rate := S.TdsRate
		if p.TdsPct != nil {
			rate = *p.TdsPct
		}
		tds = gross * rate / 100
	}
	ded := eePF + eeESI + lwfEe + meal + accom + tds
	if h.ded != 0 { // component deductions (designed structures)
		ded += h.ded
	}
	totalEarn := gross + bonus + varPay
	grat := b2f(T.Grat) * basic * S.Grat / 100
	plEnc := b2f(T.PlEnc) * basic / 30 * S.PlDays / 12
	employerCost := erPF + admin + edli + erESI + lwfEr + med + grat

	res := Result{
		Gross: gross, Basic: basic, Hra: hra, Sa: sa, PfBase: pfBase,
		Variable: variable, VarPay: varPay, VarCycle: cyc,
		EePF: eePF, ErPF: erPF, Eps: eps, ErEpf: erPF - eps, Admin: admin, Edli: edli, PfExempt: p.PfExempt && T.Statutory,
		EsiOn: esiOn, EeESI: eeESI, ErESI: erESI, MedOn: medOn, Med: med, Bonus: bonus,
		LwfOn: lwfOn, LwfEe: lwfEe, LwfEr: lwfEr, Meal: meal, Accom: accom, Tds: tds,
		Ded: ded, TotalEarn: totalEarn, Net: totalEarn - ded, Grat: grat, PlEnc: plEnc,
		EmployerCost: employerCost, CTC: gross + variable + bonus + employerCost + plEnc,
		Comps: h.lines, CompDed: h.ded, NonTax: h.nonTax,
	}
	if p.Type == Retainer {
		retainerOnly(&res)
	}
	return res
}

// retainerOnly shows a retainer's pay as the retainer value only (owner, 18 Nov 2026): one "Retainer fee" (the fixed
// gross earned), no Basic, HRA or Special allowance split and no fixed component lines; the deduction lines and any
// variable amount paid stay. Only the display figures change: every amount, the TDS (section 194J), net pay and the
// CTC are worked out exactly as before.
func retainerOnly(r *Result) {
	r.Fee = r.Gross
	r.Basic, r.Hra, r.Sa, r.PfBase = 0, 0, 0, 0
	if len(r.Comps) == 0 {
		return
	}
	keep := []CompLine{}
	for _, l := range r.Comps {
		if l.Side == SideDed || (l.Kind == CompVariable && (l.Amount != 0 || l.Target != 0)) {
			keep = append(keep, l)
		}
	}
	r.Comps = keep
	if len(keep) == 0 {
		r.Comps = nil
	}
}

// PayDays is the proration basis for a YYYY-MM month: actual days when the basis is
// "calendar" (spec 21 #3 corrects the prototype's fixed 31), otherwise the fixed basis.
func PayDays(ym, denom string) int {
	if denom != "calendar" {
		n, _ := strconv.Atoi(denom)
		return n
	}
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return 30
	}
	return t.AddDate(0, 1, -1).Day()
}

// EsiPeriodStart is the contribution period start (Apr-Sep, Oct-Mar) for a YYYY-MM month.
func EsiPeriodStart(ym string) string {
	y, _ := strconv.Atoi(ym[:4])
	m, _ := strconv.Atoi(ym[5:7])
	switch {
	case m >= 10:
		return ym[:4] + "-10"
	case m >= 4:
		return ym[:4] + "-04"
	default:
		return strconv.Itoa(y-1) + "-10"
	}
}

// EsiKeep reports whether ESI continues in ym because gross was within the ceiling when
// the period began (or when the employee joined during it). Not on the first month.
func EsiKeep(ym, dojYm string, grossAt func(string) float64, ceiling float64) bool {
	from := EsiPeriodStart(ym)
	if dojYm > from {
		from = dojYm
	}
	if ym <= from {
		return false
	}
	return grossAt(from) <= ceiling
}

// Round2 rounds to paise.
func Round2(v float64) float64 { return math.Round(v*100) / 100 }
