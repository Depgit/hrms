package calc

// Pay components (workforce plan, module 3; built 17 Nov 2026). A salary structure is a list of components: Fixed or
// Variable, earning or deduction, each with a calculation method (a fixed ₹ amount; % of CTC; % of the fixed gross;
// % of Basic; % of another component; the remainder) and an optional minimum and maximum, plus flags: taxable, PF
// wage, ESI wage, bonus wage and shown on the payslip. Variable components have a payout cycle.
//
// Every structure made before this change is converted into an equivalent template (LegacyComponents): Basic + DA as
// % of gross, HRA as % of Basic, Special allowance as the remainder, and variable pay as "legacy accrued % of gross",
// which keeps today's automatic accrual and payout on the person's own cycle. The engine evaluates that template with
// the same arithmetic, in the same order, as the calculation it replaces, so legacy pay is unchanged to the paisa;
// `hrms pay-compare` and TestLegacyComponentsSameAsClassic prove it (Input.Classic runs the old calculation).
//
// Variable components of a designed structure are paid as an amount HR enters when a payout is due (decided 17 Nov
// 2026): each person has a monthly target per component; the month's accrual (target × days paid) counts in the CTC,
// and the payout is what HR entered (Input.VarPaid). Not entered: not paid (the payroll checks list it).

import "math"

// Component kinds, sides and methods.
const (
	CompFixed    = "fixed"
	CompVariable = "variable"
	SideEarn     = "earn"
	SideDed      = "ded"

	MAmount    = "amount"     // a fixed ₹ amount a month
	MPctCTC    = "pct_ctc"    // % of the monthly CTC (fixed gross + variable target)
	MPctGross  = "pct_gross"  // % of the fixed monthly gross
	MPctBasic  = "pct_basic"  // % of Basic (+ DA): the components with role basic or da
	MPctOf     = "pct_of"     // % of another component (Of)
	MRemainder = "remainder"  // the balancing figure
	MLegacyVar = "legacy_var" // migration 014's variable: % of gross accrued and paid every cycle (automatic); kept for safety, unused since 017
	// MPerEmp is a fixed ₹ a month entered on each employee (Input.EmpAmts by the component's code; employee_pay.comp_amts),
	// 0 for someone with none: the "Reimbursement" component (owner, 20 Nov 2026). It is part of the fixed gross, so the
	// balancing component (Special allowance) gets less and the gross stays as entered.
	MPerEmp = "per_emp"
)

// ReimbCode is the code of the per-employee "Reimbursement" component every structure carries (migration 024).
const ReimbCode = "reimb"

// ReimbComponent is the fixed "Reimbursement" component: a ₹ amount entered per employee, part of the fixed gross,
// taxable, outside the PF, ESI and bonus wages, shown on the payslip.
var ReimbComponent = Component{Code: ReimbCode, Name: "Reimbursement", Kind: CompFixed, Side: SideEarn, Method: MPerEmp, Taxable: true, Payslip: true}

// Component roles the statutory rules read: basic and da make the labour-code wage and the Basic that PF admin,
// EDLI, gratuity and PL encashment use; hra is shown as HRA.
const (
	RoleBasic = "basic"
	RoleDA    = "da"
	RoleHRA   = "hra"
)

// Component is one line of a structure (a snapshot of the component library entry, with the structure's own method
// and value).
type Component struct {
	Code    string  `json:"code"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`           // fixed | variable
	Side    string  `json:"side"`           // earn | ded
	Role    string  `json:"role,omitempty"` // basic | da | hra | ""
	Method  string  `json:"method"`
	Value   float64 `json:"value"`
	Of      string  `json:"of,omitempty"`    // pct_of: the component's code
	Min     float64 `json:"min,omitempty"`   // a month, 0 = none
	Max     float64 `json:"max,omitempty"`   // a month, 0 = none
	Cycle   int     `json:"cycle,omitempty"` // variable: 1, 3, 6 or 12 months
	Taxable bool    `json:"taxable"`
	PF      bool    `json:"pf"`
	ESI     bool    `json:"esi"`
	Bonus   bool    `json:"bonus"`
	Payslip bool    `json:"payslip"`
}

// CompLine is a component's amount in a calculation: fixed components what is earned (or deducted) this month;
// variable ones the amount paid this month, with the monthly target and this month's accrual.
type CompLine struct {
	Code    string  `json:"code"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Side    string  `json:"side"`
	Amount  float64 `json:"amount"`
	Target  float64 `json:"target,omitempty"`
	Accrued float64 `json:"accrued,omitempty"`
	Cycle   int     `json:"cycle,omitempty"`
	Taxable bool    `json:"taxable,omitempty"`
	Payslip bool    `json:"payslip,omitempty"`
}

// LegacyComponents is the component template equivalent to a structure's percentage rates.
func LegacyComponents(S Structure) []Component {
	return []Component{
		{Code: "basic", Name: "Basic + DA", Kind: CompFixed, Side: SideEarn, Role: RoleBasic, Method: MPctGross, Value: S.BasicPct, Taxable: true, PF: true, ESI: true, Bonus: true, Payslip: true},
		{Code: "hra", Name: "HRA", Kind: CompFixed, Side: SideEarn, Role: RoleHRA, Method: MPctBasic, Value: S.HraPct, Taxable: true, ESI: true, Payslip: true},
		ReimbComponent, // 0 unless the person has an amount (migration 024), so the result is as before
		{Code: "sa", Name: "Special allowance", Kind: CompFixed, Side: SideEarn, Method: MRemainder, Taxable: true, PF: S.PfWage == "basicSa", ESI: true, Payslip: true},
		{Code: "var", Name: "Variable pay", Kind: CompVariable, Side: SideEarn, Method: MLegacyVar, Value: S.VarPct, Cycle: S.VarCycle, Taxable: true, Payslip: true},
	}
}

// TargetComponents is the template of a structure converted to the manual variable target (migration 017; decided
// 17 Nov 2026): the fixed components of LegacyComponents, unchanged, and one variable component, "Variable pay", the
// balancing variable (the person's whole monthly target), paid on the structure's payout cycle as HR enters it.
func TargetComponents(S Structure) []Component {
	l := LegacyComponents(S)
	cyc := S.VarCycle
	if cyc != 3 && cyc != 6 && cyc != 12 {
		cyc = 1
	}
	l[len(l)-1] = Component{Code: "var", Name: "Variable pay", Kind: CompVariable, Side: SideEarn, Method: MRemainder, Cycle: cyc, Taxable: true, Payslip: true}
	return l
}

// ComponentsOf is the structure's components: its own, else the template converted from its rates.
func ComponentsOf(S Structure) []Component {
	if len(S.Comps) > 0 {
		return S.Comps
	}
	return LegacyComponents(S)
}

// VarDue says whether a designed variable component with payout cycle `cycle` (months) is due in month ym, the cycle
// counted from the month anchor (the joining month, or the month variable pay started; both "YYYY-MM"): it is paid
// when a cycle is complete, in anchor + cycle − 1 and every cycle months after (owner, 18 Nov 2026; before, cycles
// ended with the financial year's quarters). Quarterly from May: July, October, January, April; half-yearly from May:
// October, April; yearly from May: April. Monthly is every month from the anchor on. Never before the first payout.
func VarDue(ym, anchor string, cycle int) bool {
	if cycle < 1 {
		cycle = 1
	}
	m, a := ymIndex(ym), ymIndex(anchor)
	if m < 0 {
		return false
	}
	if a < 0 { // no anchor: every month for a monthly component only
		return cycle == 1
	}
	d := m - a
	return d >= cycle-1 && d%cycle == cycle-1
}

// ymIndex is a "YYYY-MM" month as a count of months (-1 when it is not one).
func ymIndex(ym string) int {
	if len(ym) < 7 || ym[4] != '-' {
		return -1
	}
	y, m := 0, 0
	for _, c := range ym[:4] {
		if c < '0' || c > '9' {
			return -1
		}
		y = y*10 + int(c-'0')
	}
	for _, c := range ym[5:7] {
		if c < '0' || c > '9' {
			return -1
		}
		m = m*10 + int(c-'0')
	}
	if m < 1 || m > 12 {
		return -1
	}
	return y*12 + m - 1
}

// WithReimb is comps with the Reimbursement component placed before the fixed balancing component (or after the
// fixed earnings) when it has no per-employee component yet; ok is false when nothing was added.
func WithReimb(comps []Component) ([]Component, bool) {
	at := -1
	for i, c := range comps {
		if c.Method == MPerEmp || c.Code == ReimbCode {
			return comps, false
		}
		if at < 0 && c.Kind == CompFixed && c.Side == SideEarn && c.Method == MRemainder {
			at = i
		}
	}
	if at < 0 {
		at = 0
		for i, c := range comps {
			if c.Kind == CompFixed && c.Side == SideEarn {
				at = i + 1
			}
		}
	}
	out := append([]Component{}, comps[:at]...)
	out = append(out, ReimbComponent)
	return append(out, comps[at:]...), true
}

// head is what the components (or the classic rates) give the rest of the calculation.
type head struct {
	basic, hra, sa      float64
	reimb               float64 // per-employee fixed amounts (MPerEmp earnings)
	pfWage, bonusBase   float64
	nonESI, nonTax, ded float64
	variable, varPay    float64
	cyc                 int
	lines               []CompLine
}

// classicHead is the calculation before components, kept unchanged (Input.Classic) for the before/after comparison.
func classicHead(p Input, S Structure, T TypeRules, gross float64, jm int) head {
	vp := 0.0
	if T.Variable {
		vp = S.VarPct
		if p.VarPct != nil {
			vp = *p.VarPct
		}
		vp /= 100
	}
	variable := gross * vp
	cyc := S.VarCycle
	if p.VarCycle != nil {
		cyc = *p.VarCycle
	}
	if cyc < 1 {
		cyc = 1
	}
	varPay := 0.0
	if variable > 0 && p.Month > 0 && (((p.Month-jm)%cyc)+cyc)%cyc == cyc-1 {
		varPay = variable * float64(cyc)
		if p.VarTotal != nil {
			varPay = *p.VarTotal
		}
	}
	basic := gross * S.BasicPct / 100
	hra := basic * S.HraPct / 100
	sa := math.Max(0, gross-basic-hra)
	pfWage := basic
	if S.PfWage == "basicSa" {
		pfWage = basic + sa
	}
	if p.AllBasic {
		basic, hra, sa, pfWage = gross, 0, 0, gross
	}
	return head{basic: basic, hra: hra, sa: sa, pfWage: pfWage, bonusBase: basic, variable: variable, varPay: varPay, cyc: cyc}
}

func capped(v float64, c Component, scale float64) float64 {
	if c.Min > 0 && v < c.Min*scale {
		v = c.Min * scale
	}
	if c.Max > 0 && v > c.Max*scale {
		v = c.Max * scale
	}
	return v
}

// compHead evaluates the components. gross is the earned fixed gross, full the month's full fixed gross.
func compHead(p Input, S Structure, T TypeRules, gross, full float64, jm int) head {
	comps := ComponentsOf(S)
	ratio := 1.0
	if full > 0 && gross != full {
		ratio = gross / full
	}
	if p.VarTarget == nil && p.VarTargetPct != nil { // a target in %: of the full fixed monthly gross
		t := full * *p.VarTargetPct / 100
		p.VarTarget = &t
	}
	ctc := p.CTC
	if ctc <= 0 {
		ctc = full
		if p.VarTarget != nil {
			ctc += *p.VarTarget
		}
	}
	val := map[string]float64{}
	done := map[string]bool{}
	isFixedEarn := func(c Component) bool { return c.Kind != CompVariable && c.Side != SideDed }
	// basicSum is Basic (+ DA) evaluated so far, in the components' order (0 + basic is basic exactly).
	basicSum := func() (float64, bool) {
		s := 0.0
		for _, c := range comps {
			if isFixedEarn(c) && (c.Role == RoleBasic || c.Role == RoleDA) {
				if !done[c.Code] {
					return 0, false
				}
				s += val[c.Code]
			}
		}
		return s, true
	}
	// Fixed components (earnings and deductions), dependencies first; the remainder once every other fixed earning is known.
	for pass := 0; pass <= len(comps); pass++ {
		progress := false
		for _, c := range comps {
			if c.Kind == CompVariable || done[c.Code] {
				continue
			}
			var v float64
			ok := true
			scale := ratio
			if c.Side == SideDed {
				scale = 1 // a fixed deduction is not prorated by days
			}
			switch c.Method {
			case MAmount:
				v = c.Value * scale
			case MPerEmp:
				v = p.EmpAmts[c.Code] * scale
			case MPctCTC:
				v = ctc * c.Value / 100 * scale
			case MPctGross:
				v = gross * c.Value / 100
			case MPctBasic:
				var b float64
				if b, ok = basicSum(); ok {
					v = b * c.Value / 100
				}
			case MPctOf:
				if ok = done[c.Of]; ok {
					v = val[c.Of] * c.Value / 100
				}
			case MRemainder:
				rem := gross
				for _, o := range comps {
					if o.Code == c.Code || !isFixedEarn(o) {
						continue
					}
					if !done[o.Code] {
						ok = false
						break
					}
					rem -= val[o.Code]
				}
				v = math.Max(0, rem)
			}
			if !ok {
				continue
			}
			if c.Method != MRemainder {
				v = capped(v, c, scale)
			}
			val[c.Code], done[c.Code], progress = v, true, true
		}
		if !progress {
			break
		}
	}
	h := head{cyc: 1}
	designed := len(S.Comps) > 0 && S.Designed
	if p.AllBasic { // the whole fixed gross as Basic (Input.AllBasic): one taxable line, in the PF, ESI and bonus wage
		h.basic, h.pfWage, h.bonusBase = gross, gross, gross
		if designed {
			code, name := "BASIC", "Basic"
			for _, c := range comps {
				if isFixedEarn(c) && c.Role == RoleBasic {
					code, name = c.Code, c.Name
					break
				}
			}
			h.lines = append(h.lines, CompLine{Code: code, Name: name, Kind: CompFixed, Side: SideEarn, Amount: gross, Taxable: true, Payslip: true})
		}
	}
	for _, c := range comps {
		if c.Kind == CompVariable {
			continue
		}
		v := val[c.Code]
		if p.AllBasic && c.Side != SideDed {
			continue
		}
		if c.Side == SideDed {
			if T.Ded != "all" {
				v = 0
			}
			h.ded += v
		} else {
			switch {
			case c.Role == RoleBasic || c.Role == RoleDA:
				h.basic += v
			case c.Role == RoleHRA:
				h.hra += v
			case c.Method == MPerEmp:
				h.reimb += v
			default:
				h.sa += v
			}
			if c.PF {
				h.pfWage += v
			}
			if c.Bonus {
				h.bonusBase += v
			}
			if !c.ESI {
				h.nonESI += v
			}
			if !c.Taxable {
				h.nonTax += v
			}
		}
		if designed && (c.Method != MPerEmp || v != 0) { // a per-employee amount of 0 is no line (pay as before)
			h.lines = append(h.lines, CompLine{Code: c.Code, Name: c.Name, Kind: CompFixed, Side: c.Side, Amount: v, Taxable: c.Taxable, Payslip: c.Payslip})
		}
	}
	// Variable components.
	var vars []Component
	for _, c := range comps {
		if c.Kind == CompVariable && c.Side != SideDed {
			vars = append(vars, c)
		}
	}
	targets := map[string]float64{}
	if len(vars) > 0 {
		full1 := func(code string) float64 { // a fixed component's full-month value
			if ratio > 0 {
				return val[code] / ratio
			}
			return val[code]
		}
		b, _ := basicSum()
		if ratio > 0 {
			b /= ratio
		}
		var rem *Component
		sum, free := 0.0, 0.0
		for i, c := range vars {
			if c.Method == MLegacyVar {
				continue
			}
			if t, ok := p.VarTargets[c.Code]; ok {
				targets[c.Code] = t
				sum += t
				continue
			}
			var t float64
			switch c.Method {
			case MAmount:
				t = c.Value
			case MPctCTC:
				t = ctc * c.Value / 100
			case MPctGross:
				t = full * c.Value / 100
			case MPctBasic:
				t = b * c.Value / 100
			case MPctOf:
				if tv, ok := targets[c.Of]; ok {
					t = tv * c.Value / 100
				} else {
					t = full1(c.Of) * c.Value / 100
				}
			case MRemainder:
				rem = &vars[i]
				continue
			}
			t = capped(t, c, 1)
			targets[c.Code] = t
			sum += t
			free += t
		}
		if rem != nil {
			total := ctc - full
			if p.VarTarget != nil {
				total = *p.VarTarget
			}
			targets[rem.Code] = capped(math.Max(0, total-sum), *rem, 1)
		} else if p.VarTarget != nil && free > 0 && math.Abs(sum-*p.VarTarget) > 0.005 {
			// No balancing component: the components worked out by their methods share the person's target in proportion.
			k := math.Max(0, *p.VarTarget-(sum-free)) / free
			for _, c := range vars {
				if _, own := p.VarTargets[c.Code]; c.Method != MLegacyVar && !own {
					targets[c.Code] *= k
				}
			}
		}
	}
	for _, c := range vars {
		if c.Method == MLegacyVar {
			// The converted variable pay: exactly the classic accrual and payout.
			vp := 0.0
			if T.Variable {
				vp = c.Value
				if p.VarPct != nil {
					vp = *p.VarPct
				}
				vp /= 100
			}
			variable := gross * vp
			cyc := c.Cycle
			if p.VarCycle != nil {
				cyc = *p.VarCycle
			}
			if cyc < 1 {
				cyc = 1
			}
			varPay := 0.0
			if variable > 0 && p.Month > 0 && (((p.Month-jm)%cyc)+cyc)%cyc == cyc-1 {
				varPay = variable * float64(cyc)
				if p.VarTotal != nil {
					varPay = *p.VarTotal
				}
			}
			h.variable += variable
			h.varPay += varPay
			h.cyc = cyc
			if designed {
				h.lines = append(h.lines, CompLine{Code: c.Code, Name: c.Name, Kind: CompVariable, Side: SideEarn, Amount: varPay, Accrued: variable, Cycle: cyc, Taxable: c.Taxable, Payslip: c.Payslip})
			}
			continue
		}
		t := targets[c.Code]
		paid := p.VarPaid[c.Code]
		if !T.Variable {
			t, paid = 0, 0
		}
		h.variable += t * ratio
		h.varPay += paid
		if !c.Taxable {
			h.nonTax += paid
		}
		cyc := c.Cycle
		if cyc < 1 {
			cyc = 1
		}
		h.lines = append(h.lines, CompLine{Code: c.Code, Name: c.Name, Kind: CompVariable, Side: SideEarn, Amount: paid, Target: t, Accrued: t * ratio, Cycle: cyc, Taxable: c.Taxable, Payslip: c.Payslip})
	}
	return h
}

// WageShare is Basic (+ DA) as a % of total pay (the fixed gross and the variable target): the labour codes count
// wages as at least 50% of total pay. ok is false below 50%.
func WageShare(r Result) (pct float64, ok bool) {
	total := r.Gross + r.Variable
	if total <= 0 {
		return 0, true
	}
	pct = Round2(r.Basic / total * 100)
	return pct, pct >= 50
}
