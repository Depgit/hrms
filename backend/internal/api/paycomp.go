package api

// Pay structure designer (workforce plan, module 3; built 17 Nov 2026).
//
//   - Component library (Compensation › Pay components, comp.structure): components with a kind (Fixed, Variable), a
//     side (earning, deduction), a method (fixed ₹; % of CTC; % of fixed gross; % of Basic; % of another component;
//     remainder), an optional minimum and maximum, and the flags taxable, PF wage, ESI wage, bonus wage and shown on
//     the payslip. Variable components have a payout cycle (monthly, quarterly, half-yearly, yearly), counted from the
//     person's joining month (var_start when variable pay started later): paid when each cycle is complete
//     (calc.VarDue; owner, 18 Nov 2026).
//   - Structures from components: a designed structure is a salary_structure version like any other (named, mapped
//     to people by date in structure_map, a mid-month change split by days); its rates keep the statutory settings
//     (PF, ESI, bonus, medical, gratuity, days basis) and rates.comps the components. Every structure made before
//     this change carries its converted template (migration 014) and is paid exactly as before.
//   - Entering pay: by CTC (here: fixed + variable a month) or by the fixed monthly gross, with the variable part as
//     an amount or a % of the total (₹35,000 = ₹20,000 fixed + ₹15,000 variable). The fixed part is the gross on the
//     gross history; the variable part is the person's monthly variable target (var_target, dated).
//   - Variable pay of a designed structure is a manual amount (decided 17 Nov): when a component's payout is due,
//     HR enters the amount (Payroll › Variable pay, or the payout columns of Bulk update › Salary breakup). Not entered: not paid, and the
//     payroll checks list who. The full and final takes an amount for the current cycle.
//   - Compliance: a warning (never a block) when Basic (+ DA) is under 50% of total pay, the labour codes' wage rule.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

func (s *Server) payCompRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/comp/components", s.a(s.compComponents))
	m.HandleFunc("POST /api/comp/components", s.a(s.compComponentSave))
	m.HandleFunc("PUT /api/comp/components/{id}", s.a(s.compComponentSave))
	m.HandleFunc("GET /api/comp/structures/{id}/components", s.a(s.structureComponents))
	m.HandleFunc("PUT /api/comp/structures/{id}/components", s.a(s.structureComponentsPut))
	m.HandleFunc("POST /api/comp/structures/designed", s.a(s.structureDesignedCreate))
	m.HandleFunc("POST /api/comp/pay-preview", s.a(s.payPreview))
	m.HandleFunc("GET /api/pay/variable", s.a(s.varPayList))
	m.HandleFunc("PUT /api/pay/variable", s.a(s.varPayPut))
	m.HandleFunc("PUT /api/pay/variable/target", s.a(s.varTargetPut))
	m.HandleFunc("PUT /api/fnf/{id}/variable", s.a(s.fnfVariablePut))
}

// ---------- The pay engine's inputs ----------

// varTargetPt is a person's variable target from a month: in mode "pct" a % of the fixed monthly gross (Pct; Total is
// what it came to when entered), else the total ₹ a month and, optionally, by component.
type varTargetPt struct {
	From   string
	Total  float64
	ByComp map[string]float64
	Mode   string
	Pct    float64
}

func varTargetOf(r store.Row) varTargetPt {
	t := varTargetPt{From: str(r["from_month"]), Total: payengineFloat(r["total"]), ByComp: byCompOf(r["by_comp"]), Mode: "amount"}
	if str(r["mode"]) == "pct" {
		t.Mode, t.Pct, t.ByComp = "pct", payengineFloat(r["pct"]), nil
	}
	return t
}

// apply puts the target into a calculation: a % of the full fixed gross, or the ₹ total (and per component).
func (t *varTargetPt) apply(in *calc.Input) {
	if t.Mode == "pct" {
		pct := t.Pct
		in.VarTarget, in.VarTargets, in.VarTargetPct = nil, nil, &pct
		return
	}
	total := t.Total
	in.VarTarget, in.VarTargets, in.VarTargetPct = &total, t.ByComp, nil
}

// text is the target as people read it: "11.11% of fixed gross" or "₹5,000 a month".
func (t *varTargetPt) text() string {
	if t == nil {
		return "None"
	}
	if t.Mode == "pct" {
		return strconv.FormatFloat(t.Pct, 'f', -1, 64) + "% of fixed gross"
	}
	return rupees(t.Total) + " a month"
}

// varMonthly is the monthly variable target a calculation worked out (the variable components' targets).
func varMonthly(r calc.Result) float64 {
	t := 0.0
	for _, l := range r.Comps {
		if l.Kind == calc.CompVariable {
			t += l.Target
		}
	}
	return calc.Round2(t)
}

// loadVarPay loads the dated variable targets of the people in the month and the amounts entered for the month.
func (pc *payCtx) loadVarPay(ctx context.Context, q store.Q, ofThem bson.M) error {
	ts, err := q.C(store.VarTargetColl).Find(ctx, bson.M{"employee_id": ofThem, "from_month": bson.M{"$lte": pc.ym}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: 1}}))
	if err != nil {
		return err
	}
	for _, r := range ts {
		id := str(r["employee_id"])
		pc.vtarget[id] = append(pc.vtarget[id], varTargetOf(r))
	}
	ps, err := q.C(store.VarPayoutColl).Find(ctx, bson.M{"employee_id": ofThem, "ym": pc.ym})
	if err != nil {
		return err
	}
	for _, r := range ps {
		id := str(r["employee_id"])
		if pc.vpaid[id] == nil {
			pc.vpaid[id] = map[string]float64{}
		}
		pc.vpaid[id][str(r["code"])] = payengineFloat(r["amount"])
	}
	return nil
}

func byCompOf(v any) map[string]float64 {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]float64{}
	for k, x := range m {
		out[k] = payengineFloat(x)
	}
	return out
}

// targetAt is the person's variable target in force in month ym (nil: none).
func targetAt(l []varTargetPt, ym string) *varTargetPt {
	var t *varTargetPt
	for i := range l {
		if l[i].From <= ym {
			t = &l[i]
		}
	}
	return t
}

// varInputs adds a designed structure's per-person inputs: the variable target and the amounts entered this month.
func (pc *payCtx) varInputs(id, ym string, in *calc.Input) {
	if t := targetAt(pc.vtarget[id], ym); t != nil {
		t.apply(in)
	}
	in.VarPaid = pc.vpaid[id]
}

// varTargetInput adds a person's variable target in month ym to a calculation on a structure made from components
// (nothing for a converted structure), and says whether it did.
func varTargetInput(ctx context.Context, q store.Q, id, ym string, st calc.Structure, in *calc.Input) bool {
	if !st.Designed {
		return false
	}
	t := targetAt(loadTargets(ctx, q, id), ym)
	if t == nil {
		return false
	}
	t.apply(in)
	return true
}

// varMissing names the variable components due in the month that have a target and no amount entered.
func (pc *payCtx) varMissing(e *emp, lines []calc.CompLine) []string {
	var out []string
	anchor := varAnchorOf(e.DOJ, pc.pay[e.ID].VarStart)
	for _, l := range lines {
		if l.Kind != calc.CompVariable || l.Target <= 0 || !calc.VarDue(pc.ym, anchor, l.Cycle) {
			continue
		}
		if _, ok := pc.vpaid[e.ID][l.Code]; !ok {
			out = append(out, l.Name)
		}
	}
	return out
}

// varAnchorOf is the month a person's variable payout cycle is counted from (calc.VarDue): the joining month of the
// current service (doj; a rejoiner's employee record carries the new joining date), or var_start when variable pay
// started later.
func varAnchorOf(doj, start string) string {
	if len(doj) < 7 {
		return start
	}
	if len(start) > 7 {
		start = start[:7]
	}
	return varAnchorYm(doj[:7], start, nil, 0)
}

// varAnchorFor loads a person's anchor (varAnchorOf).
func varAnchorFor(ctx context.Context, q store.Q, id string) string {
	doj, start := "", ""
	if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"doj": 1})); e != nil {
		doj = str(e["doj"])
	}
	if p, _ := q.C("employee_pay").FindOne(ctx, bson.M{"employee_id": id}, options.FindOne().SetProjection(bson.M{"var_start": 1})); p != nil {
		start = str(p["var_start"])
	}
	return varAnchorOf(doj, start)
}

// varDueWord is when a component is paid, for a message: its cycle counted from the anchor, and the next payout
// month from ym on.
func varDueWord(anchor, ym string, cycle int) string {
	w := cycleWord(cycle)
	if cycle <= 1 || anchor == "" {
		return w
	}
	return fmt.Sprintf("%s, counted from %s (next payout %s)", w, ymLabel(anchor), ymLabel(varNextPayout(anchor, ym, cycle)))
}

// varTargetIn says whether the person had a variable target above 0 for component c in some month of the payout
// cycle that ends in ym: their own target when one is in force (a % or ₹ total, or the component's own share), else
// what the structure gives the component (a fixed amount or a % above 0; the balancing component gives nothing).
func varTargetIn(l []varTargetPt, c calc.Component, ym string, cycle int) bool {
	if cycle < 1 {
		cycle = 1
	}
	for k := 0; k < cycle; k++ {
		t := targetAt(l, addYm(ym, -k))
		switch {
		case t == nil:
			if c.Method != calc.MRemainder && c.Value > 0 {
				return true
			}
		case t.Mode == "pct":
			if t.Pct > 0 {
				return true
			}
		default:
			if v, ok := t.ByComp[c.Code]; ok {
				if v > 0 {
					return true
				}
			} else if t.Total > 0 {
				return true
			}
		}
	}
	return false
}

// varNoTarget is the message refusing a payout to someone with no variable target in the cycle ("" when they have one).
func varNoTarget(ctx context.Context, q store.Q, id, name, ym string, c calc.Component) string {
	if varTargetIn(loadTargets(ctx, q, id), c, ym, c.Cycle) {
		return ""
	}
	return fmt.Sprintf("%s has no variable target for %s (none or 0), so no payout can be entered. Set a monthly target first (Payroll › Variable pay, or Bulk update › Salary breakup)", name, c.Name)
}

// ---------- Checks ----------

var reCompCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,19}$`)

var compMethods = map[string]string{calc.MAmount: "Fixed amount", calc.MPctCTC: "% of CTC", calc.MPctGross: "% of fixed gross", calc.MPctBasic: "% of Basic",
	calc.MPctOf: "% of another component", calc.MRemainder: "Remainder (balancing figure)", calc.MPerEmp: "Amount set on each employee"}

// checkComponent checks one component on its own and tidies it (variable pay is never PF, ESI or bonus wage, as
// today: spec 14 "Variable is not in PF wage and currently not in ESI wage").
func checkComponent(c *calc.Component) error {
	c.Code, c.Name = strings.ToLower(strings.TrimSpace(c.Code)), strings.TrimSpace(c.Name)
	switch {
	case !reCompCode.MatchString(c.Code):
		return bad("Code “%s”: lower-case letters, digits and _, starting with a letter, at most 20", c.Code)
	case c.Name == "" || len([]rune(c.Name)) > 40:
		return bad("Give %s a name of at most 40 characters", c.Code)
	case c.Kind != calc.CompFixed && c.Kind != calc.CompVariable:
		return bad("%s is Fixed or Variable", c.Name)
	case c.Side != calc.SideEarn && c.Side != calc.SideDed:
		return bad("%s is an earning or a deduction", c.Name)
	case compMethods[c.Method] == "":
		return bad("%s: choose how it is worked out", c.Name)
	case c.Kind == calc.CompVariable && c.Side == calc.SideDed:
		return bad("%s: a variable component is an earning", c.Name)
	case c.Side == calc.SideDed && c.Method == calc.MRemainder:
		return bad("%s: a deduction cannot be the balancing figure", c.Name)
	case c.Value < 0 || c.Min < 0 || c.Max < 0:
		return bad("%s: amounts and percentages cannot be negative", c.Name)
	case c.Method != calc.MAmount && c.Method != calc.MRemainder && c.Method != calc.MPerEmp && c.Value > 100 && c.Method != calc.MPctOf:
		return bad("%s: a percentage is at most 100", c.Name)
	case c.Value > 1e7:
		return bad("%s: the amount is too large", c.Name)
	case c.Max > 0 && c.Min > c.Max:
		return bad("%s: the minimum is above the maximum", c.Name)
	case c.Method == calc.MRemainder && (c.Min > 0 || c.Max > 0):
		return bad("%s: the balancing figure has no minimum or maximum", c.Name)
	case c.Method == calc.MPctOf && c.Of == "":
		return bad("%s: choose the component it is a %% of", c.Name)
	case c.Role != "" && c.Role != calc.RoleBasic && c.Role != calc.RoleDA && c.Role != calc.RoleHRA:
		return bad("%s: the role is Basic, DA, HRA or none", c.Name)
	case c.Role != "" && (c.Kind != calc.CompFixed || c.Side != calc.SideEarn):
		return bad("%s: only a fixed earning can be Basic, DA or HRA", c.Name)
	case c.Method == calc.MPerEmp && (c.Kind != calc.CompFixed || c.Side != calc.SideEarn || c.Role != ""):
		return bad("%s: an amount set on each employee is a fixed earning with no Basic, DA or HRA role", c.Name)
	}
	if c.Method == calc.MPerEmp {
		c.Value = 0
	}
	if c.Method != calc.MPctOf {
		c.Of = ""
	}
	if c.Method == calc.MRemainder {
		c.Value = 0
	}
	if c.Kind == calc.CompVariable {
		if c.Cycle == 0 {
			c.Cycle = 1
		}
		if c.Cycle != 1 && c.Cycle != 3 && c.Cycle != 6 && c.Cycle != 12 {
			return bad("%s: the payout cycle is monthly, quarterly, half-yearly or yearly", c.Name)
		}
		c.PF, c.ESI, c.Bonus = false, false, false
	} else {
		c.Cycle = 0
	}
	if c.Side == calc.SideDed {
		c.PF, c.ESI, c.Bonus, c.Taxable = false, false, false, false
	}
	return nil
}

// checkComps checks a designed structure's components together: unique codes, one fixed balancing earning, a
// Basic, references that exist and no loop.
func checkComps(comps []calc.Component) error {
	if len(comps) == 0 || len(comps) > 30 {
		return bad("A structure has 1 to 30 components")
	}
	seen := map[string]*calc.Component{}
	fixedRem, varRem, basic := 0, 0, 0
	for i := range comps {
		c := &comps[i]
		if c.Method == calc.MLegacyVar {
			return bad("%s: the automatic variable %% is kept for the converted structures only", c.Name)
		}
		if err := checkComponent(c); err != nil {
			return err
		}
		if seen[c.Code] != nil {
			return bad("Two components have the code %s", c.Code)
		}
		seen[c.Code] = c
		switch {
		case c.Method == calc.MRemainder && c.Kind == calc.CompFixed:
			fixedRem++
		case c.Method == calc.MRemainder:
			varRem++
		}
		if c.Role == calc.RoleBasic {
			basic++
		}
	}
	switch {
	case fixedRem != 1:
		return bad("Exactly one fixed earning is the balancing figure (the rest of the fixed gross), for example Special allowance")
	case varRem > 1:
		return bad("At most one variable component is the balancing figure")
	case basic == 0:
		return bad("Mark one fixed earning as Basic: PF, gratuity, bonus and the labour-code wage check read it")
	}
	for _, c := range comps {
		if c.Method == calc.MPctOf {
			o := seen[c.Of]
			switch {
			case o == nil:
				return bad("%s is a %% of %s, which is not in the structure", c.Name, c.Of)
			case o.Code == c.Code:
				return bad("%s cannot be a %% of itself", c.Name)
			case c.Kind == calc.CompFixed && o.Kind == calc.CompVariable:
				return bad("%s is fixed, so it cannot be a %% of the variable %s", c.Name, o.Name)
			}
		}
		if c.Method == calc.MPctBasic && (c.Role == calc.RoleBasic || c.Role == calc.RoleDA) {
			return bad("%s is Basic or DA, so it cannot be a %% of Basic", c.Name)
		}
	}
	// No loop: every fixed component resolves (a test calculation leaves none unworked).
	S := calc.DefaultStructure
	S.Comps, S.Designed = comps, true
	amts := map[string]float64{} // a per-employee amount shows its line only when it is above 0
	for _, c := range comps {
		if c.Method == calc.MPerEmp {
			amts[c.Code] = 1
		}
	}
	r := calc.Calc(calc.Input{Type: calc.Permanent, Gross: 100000, Structure: &S, EmpAmts: amts})
	if len(r.Comps) < len(comps) {
		return bad("The components refer to each other in a loop")
	}
	for _, l := range r.Comps {
		if math.IsNaN(l.Amount) || math.IsInf(l.Amount, 0) {
			return bad("The components refer to each other in a loop")
		}
	}
	if err := compLoop(comps); err != nil {
		return err
	}
	return nil
}

// compLoop finds a reference loop among the fixed components (pct_of chains, and % of Basic for a Basic that is
// itself a % of something else).
func compLoop(comps []calc.Component) error {
	by := map[string]calc.Component{}
	for _, c := range comps {
		by[c.Code] = c
	}
	deps := func(c calc.Component) []string {
		switch c.Method {
		case calc.MPctOf:
			return []string{c.Of}
		case calc.MPctBasic:
			var out []string
			for _, o := range comps {
				if o.Kind == calc.CompFixed && o.Side == calc.SideEarn && (o.Role == calc.RoleBasic || o.Role == calc.RoleDA) {
					out = append(out, o.Code)
				}
			}
			return out
		}
		return nil
	}
	state := map[string]int{}
	var visit func(code string) bool
	visit = func(code string) bool {
		switch state[code] {
		case 1:
			return false
		case 2:
			return true
		}
		state[code] = 1
		for _, d := range deps(by[code]) {
			if !visit(d) {
				return false
			}
		}
		state[code] = 2
		return true
	}
	for _, c := range comps {
		if !visit(c.Code) {
			return bad("%s is part of a loop of components that refer to each other", c.Name)
		}
	}
	return nil
}

// designedRates are the rates a designed structure keeps beside its components: the statutory settings of base, with
// the percentage split and the automatic variable switched off (the components decide the split).
func designedRates(base calc.Structure, comps []calc.Component) calc.Structure {
	st := base
	st.Comps, st.Designed, st.Converted = comps, true, false
	st.VarPct, st.VarCycle, st.HraPct = 0, 1, 0
	st.BasicPct = 50
	for _, c := range comps {
		if c.Role == calc.RoleBasic && c.Method == calc.MPctGross && c.Value > 0 {
			st.BasicPct = c.Value
		}
	}
	st.PfWage = "basic"
	return st
}

// wageNote is the labour-code wage check of a calculation: Basic (+ DA) against total pay (fixed + variable).
func wageNote(r calc.Result) map[string]any {
	pct, ok := calc.WageShare(r)
	out := map[string]any{"pct": pct, "ok": ok}
	if !ok {
		out["note"] = fmt.Sprintf("Basic + DA is %v%% of total pay (%s of %s). The labour codes count wages as at least 50%% of total pay: the rest above half is added back to wages for PF, gratuity and other dues.",
			pct, calc.Inr(math.Round(r.Basic)), calc.Inr(math.Round(r.Gross+r.Variable)))
	}
	return out
}

// structureWage is a structure's wage check on sample pay: the lowest share of three fixed grosses, without variable.
func structureWage(st calc.Structure) map[string]any {
	var worst map[string]any
	for _, g := range []float64{15000, 30000, 60000} {
		r := calc.Calc(calc.Input{Type: calc.Permanent, Gross: g, Structure: &st})
		w := wageNote(r)
		if worst == nil || w["pct"].(float64) < worst["pct"].(float64) {
			worst = w
			worst["gross"] = g
		}
	}
	return worst
}

// ---------- Component library ----------

func compRowOf(r store.Row) map[string]any {
	return map[string]any{"id": r["id"], "code": r["code"], "name": r["name"], "kind": r["kind"], "side": r["side"], "role": str(r["role"]), "method": r["method"],
		"value": payengineFloat(r["value"]), "of": str(r["of"]), "min": payengineFloat(r["min"]), "max": payengineFloat(r["max"]), "cycle": int(payengineFloat(r["cycle"])),
		"taxable": r["taxable"] == true, "pf": r["pf"] == true, "esi": r["esi"] == true, "bonus": r["bonus"] == true, "payslip": r["payslip"] == true, "status": r["status"]}
}

func (s *Server) compComponents(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 1 && c.Level("pay.setup") < 1 {
		return nil, forbidden("")
	}
	rows, err := s.st.DB.C(store.PayComponentColl).Find(r.Context(), nil, options.Find().SetSort(bson.D{{Key: "kind", Value: 1}, {Key: "side", Value: -1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, x := range rows {
		out = append(out, compRowOf(x))
	}
	methods := []map[string]string{}
	for _, k := range []string{calc.MAmount, calc.MPctCTC, calc.MPctGross, calc.MPctBasic, calc.MPctOf, calc.MRemainder} {
		methods = append(methods, map[string]string{"key": k, "label": compMethods[k]})
	}
	return map[string]any{"components": out, "methods": methods, "can": c.Level("comp.structure") >= 2}, nil
}

// compComponentSave adds a component to the library (POST) or changes one (PUT). Structures keep their own copy,
// so a change here reaches only structures made or edited afterwards.
func (s *Server) compComponentSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		calc.Component
		Status string `json:"status"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	comp := b.Component
	if err := checkComponent(&comp); err != nil {
		return nil, err
	}
	if comp.Method == calc.MPctOf && comp.Of == comp.Code {
		return nil, bad("%s cannot be a %% of itself", comp.Name)
	}
	status := "Active"
	if b.Status == "Inactive" {
		status = "Inactive"
	}
	ctx, id := r.Context(), r.PathValue("id")
	doc := bson.M{"code": comp.Code, "name": comp.Name, "kind": comp.Kind, "side": comp.Side, "role": nullIfEmpty(comp.Role), "method": comp.Method, "value": comp.Value,
		"of": nullIfEmpty(comp.Of), "min": nullIfZero(comp.Min), "max": nullIfZero(comp.Max), "cycle": nullIfZeroInt(comp.Cycle), "taxable": comp.Taxable, "pf": comp.PF,
		"esi": comp.ESI, "bonus": comp.Bonus, "payslip": comp.Payslip, "status": status}
	var out string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		coll := tx.C(store.PayComponentColl)
		if dup, _ := coll.FindOne(ctx, bson.M{"$or": bson.A{bson.M{"code": comp.Code}, bson.M{"name": bulkCI(comp.Name)}}, "_id": bson.M{"$ne": id}}); dup != nil {
			return bad("A component with the code %s or the name %s already exists", comp.Code, comp.Name)
		}
		if id == "" {
			doc["created_by"] = c.Name
			newID, err := coll.Insert(ctx, doc)
			if err != nil {
				return err
			}
			out = str(newID)
			return audit(ctx, tx, c, "Pay component added", store.PayComponentColl, out, nil, doc)
		}
		old, err := coll.Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		if old == nil {
			return notFound("Component not found")
		}
		doc["updated_at"] = time.Now()
		if _, err := coll.Update(ctx, bson.M{"_id": id}, bson.M{"$set": doc}); err != nil {
			return err
		}
		out = id
		return audit(ctx, tx, c, "Pay component changed", store.PayComponentColl, id, compRowOf(old), doc)
	})
	return map[string]string{"id": out}, err
}

func nullIfZero(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullIfZeroInt(v int) any {
	if v == 0 {
		return nil
	}
	return int32(v)
}

// ---------- Structures from components ----------

// structureDoc reads a structure version: the document and its rates (with its components, converted when it has
// none of its own).
func structureDoc(ctx context.Context, q store.Q, id string) (store.Row, calc.Structure, error) {
	st := calc.DefaultStructure
	x, err := q.C("salary_structure").FindOne(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, st, err
	}
	if x == nil {
		return nil, st, notFound("Version not found")
	}
	if err := payengineJSON(x["rates"], &st); err != nil {
		return nil, st, err
	}
	return x, st, nil
}

func (s *Server) structureComponents(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 1 && c.Level("pay.setup") < 1 && c.Level("onb.raise") < 2 {
		return nil, forbidden("")
	}
	x, st, err := structureDoc(r.Context(), s.st.DB, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": x["id"], "name": x["name"], "designed": st.Designed, "comps": calc.ComponentsOf(st), "converted": !st.Designed,
		"wage": structureWage(st), "can": c.Level("comp.structure") >= 2}, nil
}

// structureDesignedCreate makes a new version from components: the statutory settings copied from a version
// (copyFrom, else the default), the components as given (checked).
func (s *Server) structureDesignedCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		Name     string           `json:"name"`
		CopyFrom string           `json:"copyFrom"`
		Comps    []calc.Component `json:"comps"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
		return nil, bad("Name the structure, for example Sales 2027")
	}
	if err := checkComps(b.Comps); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := structureNameOK(ctx, s.st.DB, b.Name, ""); err != nil {
		return nil, err
	}
	base := calc.DefaultStructure
	src := b.CopyFrom
	if src == "" {
		src = compStructureDefault(ctx, s.st.DB)
	}
	if src != "" {
		if _, st, err := structureDoc(ctx, s.st.DB, src); err == nil {
			base = st
		} else if b.CopyFrom != "" {
			return nil, bad("Unknown version to copy the statutory settings from")
		}
	}
	rates := designedRates(base, b.Comps)
	if err := checkRates(rates); err != nil {
		return nil, err
	}
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		newID, err := tx.C("salary_structure").Insert(ctx, bson.M{"name": b.Name, "rates": rates})
		if err != nil {
			return err
		}
		id = str(newID)
		return audit(ctx, tx, c, "Salary structure made from components", "salary_structure", id, nil, map[string]any{"name": b.Name, "copyFrom": src, "comps": b.Comps})
	})
	return map[string]any{"id": id, "wage": structureWage(rates)}, err
}

// structureComponentsPut changes a designed structure's components. Like the rates, the change applies to every month
// not yet approved; to change pay from a date, make a new version and map people to it from that date.
func (s *Server) structureComponentsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("comp.structure", 2); err != nil {
		return nil, err
	}
	var b struct {
		Comps []calc.Component `json:"comps"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := checkComps(b.Comps); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	var wage map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("salary_structure").Lock(ctx, bson.M{"_id": id}); err != nil {
			return err
		}
		_, st, err := structureDoc(ctx, tx, id)
		if err != nil {
			return err
		}
		if !st.Designed {
			return bad("This version uses the percentage rates (its components are converted from them). Make a new structure from components instead")
		}
		old := st.Comps
		rates := designedRates(st, b.Comps)
		if _, err := tx.C("salary_structure").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"rates": rates}}); err != nil {
			return err
		}
		wage = structureWage(rates)
		return audit(ctx, tx, c, "Salary structure components changed", "salary_structure", id, old, b.Comps)
	})
	return map[string]any{"wage": wage}, err
}

// syncLegacyComps keeps a converted structure's template in step with its rates (structureSave): a designed
// structure keeps its components whatever the rates editor sends.
func syncLegacyComps(old, next calc.Structure) calc.Structure {
	if old.Converted { // converted to the variable target (migration 017): the fixed components follow the rates
		next.Comps, next.Designed, next.Converted, next.VarPct = calc.TargetComponents(next), true, true, 0
		return next
	}
	if old.Designed {
		return designedRates(next, old.Comps)
	}
	next.Designed = false
	next.Comps = nil
	if len(old.Comps) > 0 {
		next.Comps = calc.LegacyComponents(next)
	}
	return next
}

// ---------- Entering pay: by CTC or by fixed gross ----------

// payEntry is how pay is entered: by "gross" (the fixed monthly gross) or "ctc" (fixed + variable a month), and the
// variable part as an amount ("amount") or a % of the total ("pct").
type payEntry struct {
	By     string  `json:"payBy"`
	Amount float64 `json:"amount"`
	VarBy  string  `json:"varBy"`
	Var    float64 `json:"var"`
}

// split works out the fixed monthly gross and the variable a month.
func (p payEntry) split() (fixed, variable float64, err error) {
	if p.Amount <= 0 {
		return 0, 0, bad("Enter the monthly amount")
	}
	if p.Var < 0 || (p.VarBy == "pct" && p.Var >= 100) {
		return 0, 0, bad("The variable part is 0 or more, and below 100%% of the total")
	}
	switch p.By {
	case "ctc":
		variable = p.Var
		if p.VarBy == "pct" {
			variable = p.Amount * p.Var / 100
		}
		fixed = p.Amount - variable
	default:
		fixed, variable = p.Amount, p.Var
		if p.VarBy == "pct" {
			variable = p.Amount * p.Var / (100 - p.Var)
		}
	}
	fixed, variable = calc.Round2(fixed), calc.Round2(variable)
	if fixed <= 0 {
		return 0, 0, bad("The variable part leaves no fixed pay")
	}
	return fixed, variable, nil
}

// payPreview is the breakup of an amount entered by CTC or by gross, on a structure version or on components being
// designed, with the labour-code wage check. A converted (percentage) structure takes the variable as its % of gross.
func (s *Server) payPreview(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 1 && c.Level("pay.setup") < 1 && c.Level("onb.raise") < 2 && c.Level("comp.bands") < 1 && c.Level("pay.run") < 1 {
		return nil, forbidden("")
	}
	var b struct {
		payEntry
		Type        string           `json:"type"`
		StructureID string           `json:"structureId"`
		Comps       []calc.Component `json:"comps"`
		LocationID  string           `json:"locationId"`
		Month       int              `json:"month"`
		Reimb       float64          `json:"reimb"` // the Reimbursement (outside the gross), ₹ a month (reimb.go)
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	t := calc.EmploymentType(b.Type)
	if b.Type == "" {
		t = calc.Permanent
	}
	if _, ok := calc.DefaultTypeRules[t]; !ok {
		return nil, bad("Employment type must be Permanent, Retainer or Contractual")
	}
	fixed, variable, err := b.split()
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	st := calc.DefaultStructure
	if b.StructureID != "" {
		if _, st, err = structureDoc(ctx, s.st.DB, b.StructureID); err != nil {
			return nil, bad("Unknown salary structure")
		}
	} else if def := compStructureDefault(ctx, s.st.DB); def != "" {
		_, st, _ = structureDoc(ctx, s.st.DB, def)
	}
	if len(b.Comps) > 0 {
		if err := checkComps(b.Comps); err != nil {
			return nil, err
		}
		st = designedRates(st, b.Comps)
	}
	if err := checkReimb(st, "", string(t), b.Reimb); err != nil {
		return nil, err
	}
	in := calc.Input{Type: t, Gross: fixed, Structure: &st, Month: b.Month, EmpAmts: reimbAmts(b.Reimb), AllIn: allInPreview(ctx, s.st.DB)}
	if b.LocationID != "" {
		in.Lwf = lwfFor(ctx, s.st.DB, b.LocationID, today())
	}
	out := map[string]any{"fixed": fixed, "variable": variable, "total": calc.Round2(fixed + variable), "designed": st.Designed}
	if st.Designed {
		in.VarTarget = &variable
		in.CTC = fixed + variable
	} else if variable > 0 {
		pct := calc.Round2(variable / fixed * 100)
		in.VarPct = &pct
		out["varPct"] = pct
	}
	res := calc.Calc(in)
	out["breakup"], out["ctcYear"], out["wage"], out["rules"], out["lwfNote"] = res, calc.Round2(res.CTC*12), wageNote(res), calc.RulesFor(t), lwfNoteText(in.Lwf)
	out["allIn"] = allInFrom(ctx, s.st.DB)
	if variable > 0 && !calc.RulesFor(t).Variable {
		out["warn"] = string(t) + " staff do not earn variable pay, so the variable part is not paid"
	}
	return out, nil
}

// ---------- Variable pay: due payouts and the amounts HR enters ----------

func (s *Server) varPayList(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.claims") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	ym := r.URL.Query().Get("ym")
	open := openYm(ctx, s.st.DB)
	if !isYm(ym) {
		ym = open
	}
	if msg := allInVarRefusal(ctx, s.st.DB, ym); msg != "" { // all-inclusive gross: automatic, nothing to enter
		return map[string]any{"ym": ym, "open": open, "rows": []any{}, "due": 0, "entered": 0, "can": false, "canTarget": c.Level("emp.salary") >= 2,
			"allIn": allInFrom(ctx, s.st.DB), "allInNote": msg}, nil
	}
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	rows, err := varDueRows(ctx, s.st.DB, ym, scope)
	if err != nil {
		return nil, err
	}
	due, entered := 0, 0
	for _, x := range rows {
		for _, l := range x["comps"].([]map[string]any) {
			if l["due"] == true && l["noTarget"] != true {
				due++
				if l["paid"] != nil {
					entered++
				}
			}
		}
	}
	return map[string]any{"ym": ym, "open": open, "rows": rows, "due": due, "entered": entered,
		"can": (c.Level("pay.run") >= 2 || c.Level("pay.claims") >= 2) && ym >= open, "canTarget": c.Level("emp.salary") >= 2}, nil
}

// varDueRows lists the people paid in month ym on a designed structure with variable components: each component's
// monthly target, the cycle, whether a payout is due, and the amount entered (nil: none).
func varDueRows(ctx context.Context, q store.Q, ym string, scope any) ([]map[string]any, error) {
	eligible, err := payEligible(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, q, ym, false, eligible, scope)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range list {
		sid := pc.structureOn(e.ID, ym+"-01")
		st := pc.structure(sid)
		anchor := varAnchorOf(e.DOJ, pc.pay[e.ID].VarStart)
		defs := map[string]calc.Component{}
		for _, c := range calc.ComponentsOf(*st) {
			defs[c.Code] = c
		}
		if !st.Designed || !calc.RulesFor(calc.EmploymentType(e.Type)).Variable {
			continue
		}
		gross := pc.grossAt(e.ID, ym)
		in := calc.Input{Type: calc.EmploymentType(e.Type), Gross: gross, Structure: st}
		pc.varInputs(e.ID, ym, &in)
		res := calc.Calc(in)
		comps := []map[string]any{}
		for _, l := range res.Comps {
			if l.Kind != calc.CompVariable {
				continue
			}
			due := calc.VarDue(ym, anchor, l.Cycle)
			var paid any
			if v, ok := pc.vpaid[e.ID][l.Code]; ok {
				paid = v
			}
			noTarget := l.Target <= 0 && !varTargetIn(pc.vtarget[e.ID], defs[l.Code], ym, l.Cycle)
			comps = append(comps, map[string]any{"code": l.Code, "name": l.Name, "cycle": l.Cycle, "target": calc.Round2(l.Target),
				"cycleTarget": calc.Round2(l.Target * float64(l.Cycle)), "due": due, "paid": paid, "noTarget": noTarget,
				"from": anchor, "next": varNextPayout(anchor, ym, l.Cycle)})
		}
		if len(comps) == 0 {
			continue
		}
		var target any
		if t := targetAt(pc.vtarget[e.ID], ym); t != nil {
			target = map[string]any{"from": t.From, "total": t.Total, "byComp": t.ByComp, "mode": t.Mode, "pct": t.Pct, "monthly": varMonthly(res), "text": t.text()}
		}
		out = append(out, map[string]any{"employeeId": e.ID, "name": e.Name, "location": e.Location, "type": e.Type, "structure": pc.structNames[sid],
			"gross": gross, "target": target, "comps": comps})
	}
	return out, nil
}

// varPayAllowed: Payroll (run or claims at Edit) or HR Admin enter variable payouts.
func varPayAllowed(c *Ctx) bool {
	return c.Level("pay.run") >= 2 || c.Level("pay.claims") >= 2 || c.Role == "HR Admin"
}

// setVarPayout records (or with amount nil clears) the variable amount for a person, month (or "fnf") and component,
// after checking that the component is a variable component of the structure the person is on.
func setVarPayout(ctx context.Context, tx store.Q, c *Ctx, id, ym, code string, amount *float64, note string) (string, error) {
	e, err := loadEmp(ctx, tx, id)
	if err != nil {
		return "", err
	}
	date := ym + "-01"
	if ym == "fnf" {
		if e.LWD == nil {
			return "", bad("%s has no last working day", e.Name)
		}
		date = *e.LWD
	}
	_, st := structureFor(ctx, tx, id, date)
	var comp *calc.Component
	for _, x := range calc.ComponentsOf(st) {
		if x.Code == code && x.Kind == calc.CompVariable && x.Method != calc.MLegacyVar {
			x := x
			comp = &x
		}
	}
	if !st.Designed || comp == nil {
		return "", bad("%s has no variable component %s on their salary structure", e.Name, code)
	}
	if !calc.RulesFor(calc.EmploymentType(e.Type)).Variable {
		return "", bad("%s staff do not earn variable pay", e.Type)
	}
	// All-inclusive gross (allin.go): variable pay is paid every month by the days paid, the last month too; nothing
	// is entered for those months (clearing an amount entered before is allowed).
	if paidIn := date[:7]; amount != nil {
		if msg := allInVarRefusal(ctx, tx, paidIn); msg != "" {
			return "", bad("%s", msg)
		}
	}
	if ym != "fnf" {
		if anchor := varAnchorFor(ctx, tx, id); !calc.VarDue(ym, anchor, comp.Cycle) {
			return "", bad("%s is not due in %s: it is paid %s", comp.Name, ymLabel(ym), varDueWord(anchor, ym, comp.Cycle))
		}
		if open := openYm(ctx, tx); ym < open {
			return "", bad("%s is already paid and closed. Enter amounts for %s or later", ymLabel(ym), ymLabel(open))
		}
		// No target, no payout (owner, 18 Nov 2026). Clearing an amount or entering 0 is always allowed. The full and
		// final keeps taking an amount for the current cycle.
		if amount != nil && *amount > 0 {
			if msg := varNoTarget(ctx, tx, id, e.Name, ym, *comp); msg != "" {
				return "", bad("%s", msg)
			}
		}
	}
	key := bson.M{"employee_id": id, "ym": ym, "code": code}
	old, err := tx.C(store.VarPayoutColl).FindOne(ctx, key)
	if err != nil {
		return "", err
	}
	if amount == nil {
		if old != nil {
			if _, err := tx.C(store.VarPayoutColl).Delete(ctx, key); err != nil {
				return "", err
			}
		}
	} else {
		if *amount < 0 || *amount > 1e7 {
			return "", bad("The amount is 0 or more, at most ₹1,00,00,000")
		}
		if _, err := tx.C(store.VarPayoutColl).Upsert(ctx, key, bson.M{"amount": calc.Round2(*amount), "note": nullIfEmpty(note), "by_name": c.Name, "at": time.Now()}, nil); err != nil {
			return "", err
		}
	}
	var before, after any
	if old != nil {
		before = payengineFloat(old["amount"])
	}
	if amount != nil {
		after = *amount
	}
	label := comp.Name
	return label, audit(ctx, tx, c, "Variable pay amount entered", store.VarPayoutColl, id+"/"+ym+"/"+code, before, map[string]any{"amount": after, "note": note})
}

// cycleWord is a payout cycle in words: counted from the person's joining month (calc.VarDue).
func cycleWord(n int) string {
	switch n {
	case 3:
		return "quarterly (every 3 months from joining)"
	case 6:
		return "half-yearly (every 6 months from joining)"
	case 12:
		return "yearly (every 12 months from joining)"
	}
	return "monthly"
}

func (s *Server) varPayPut(r *http.Request, c *Ctx) (any, error) {
	if !varPayAllowed(c) {
		return nil, forbidden("")
	}
	var b struct {
		Ym         string   `json:"ym"`
		EmployeeID string   `json:"employeeId"`
		Code       string   `json:"code"`
		Amount     *float64 `json:"amount"`
		Note       string   `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !isYm(b.Ym) {
		return nil, bad("Give the payroll month")
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		_, err := setVarPayout(ctx, tx, c, b.EmployeeID, b.Ym, b.Code, b.Amount, strings.TrimSpace(b.Note))
		return err
	})
}

// varTargetPut sets a person's monthly variable target from a month (emp.salary at Edit), as a total and optionally
// per component.
func (s *Server) varTargetPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.salary", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeID string             `json:"employeeId"`
		FromMonth  string             `json:"fromMonth"`
		Mode       string             `json:"mode"` // "pct" (% of fixed gross) or "amount" (₹ a month, the default)
		Value      *float64           `json:"value"`
		Total      float64            `json:"total"` // mode amount without value
		ByComp     map[string]float64 `json:"byComp"`
		Reason     string             `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		v := b.Total
		if b.Value != nil {
			v = *b.Value
		}
		_, err := setVarTarget(ctx, tx, c, b.EmployeeID, b.FromMonth, b.Mode, v, b.ByComp, b.Reason, "Variable pay")
		return err
	})
}

// setVarTarget writes a person's variable target from month from (the open month or later; a month already paid is
// not recalculated), returning the change for the caller's list.
// mode is "pct" (value: % of the fixed monthly gross) or "amount" (value: ₹ a month; "" too).
func setVarTarget(ctx context.Context, tx store.Q, c *Ctx, id, from, mode string, value float64, byComp map[string]float64, reason, source string) (*bulkChange, error) {
	e, err := loadEmp(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if !isYm(from) {
		return nil, bad("Give the month the target applies from, like 2026-11")
	}
	if open := openYm(ctx, tx); from < open {
		return nil, bad("%s is already paid. A target applies from %s or later", ymLabel(from), ymLabel(open))
	}
	if from < e.DOJ[:7] {
		return nil, bad("%s is before the joining month", ymLabel(from))
	}
	total := value
	switch mode {
	case "pct":
		if value < 0 || value > 100 {
			return nil, bad("A variable target in %% is 0 to 100%% of the fixed gross")
		}
		if len(byComp) > 0 {
			return nil, bad("A variable target in %% is one figure; give per-component targets in ₹")
		}
		g, _ := grossFor(ctx, tx, id, from)
		total = calc.Round2(g * value / 100)
	case "", "amount":
		mode = "amount"
		if value < 0 || value > 1e7 {
			return nil, bad("The variable target is 0 or more")
		}
	default:
		return nil, bad("A variable target is in %% of fixed gross or in ₹ a month")
	}
	_, st := structureFor(ctx, tx, id, from+"-01")
	if !st.Designed {
		return nil, bad("%s's salary structure pays variable as a %% of gross: change the Variable pay %% instead", e.Name)
	}
	codes := map[string]bool{}
	for _, x := range st.Comps {
		if x.Kind == calc.CompVariable {
			codes[x.Code] = true
		}
	}
	sum := 0.0
	for k, v := range byComp {
		if !codes[k] {
			return nil, bad("%s is not a variable component of %s's structure", k, e.Name)
		}
		if v < 0 {
			return nil, bad("A component's target cannot be negative")
		}
		sum += v
	}
	if sum > total+0.005 {
		return nil, bad("The components' targets add up to more than the total")
	}
	cur := targetAt(loadTargets(ctx, tx, id), from)
	was := cur.text()
	next := &varTargetPt{Mode: mode, Pct: value, Total: total}
	set := bson.M{"mode": mode, "pct": nil, "total": calc.Round2(total), "reason": nullIfEmpty(strings.TrimSpace(reason)), "created_by": c.Name + " (" + c.Role + ")"}
	if mode == "pct" {
		set["pct"] = value
	}
	if len(byComp) > 0 {
		set["by_comp"] = byComp
	} else {
		set["by_comp"] = nil
	}
	if _, err := tx.C(store.VarTargetColl).Upsert(ctx, bson.M{"employee_id": id, "from_month": from}, set, nil); err != nil {
		return nil, err
	}
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": "Variable pay target (a month)", "old_value": was, "new_value": next.text(),
		"reason": nullIfEmpty(reason), "effective_from": from + "-01", "source": source, "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, c, "Variable pay target set", store.VarTargetColl, id+"/"+from, was, map[string]any{"mode": mode, "value": value, "total": total, "byComp": byComp}); err != nil {
		return nil, err
	}
	return &bulkChange{"Variable target a month", was, next.text(), "from " + from}, nil
}

func loadTargets(ctx context.Context, q store.Q, id string) []varTargetPt {
	rows, _ := q.C(store.VarTargetColl).Find(ctx, bson.M{"employee_id": id}, options.Find().SetSort(bson.D{{Key: "from_month", Value: 1}}))
	out := []varTargetPt{}
	for _, r := range rows {
		out = append(out, varTargetOf(r))
	}
	return out
}

// fnfVariablePut records the variable amount the full and final pays for the current cycle (nil clears it).
func (s *Server) fnfVariablePut(r *http.Request, c *Ctx) (any, error) {
	if !varPayAllowed(c) {
		return nil, forbidden("")
	}
	var b struct {
		Code   string   `json:"code"`
		Amount *float64 `json:"amount"`
		Note   string   `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	if done, _ := s.st.DB.C("fnf").Exists(ctx, bson.M{"employee_id": id, "status": bson.M{"$in": bson.A{"Approved", "Paid"}}}); done {
		return nil, bad("The full and final is already approved")
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		_, err := setVarPayout(ctx, tx, c, id, "fnf", b.Code, b.Amount, strings.TrimSpace(b.Note))
		return err
	})
}

// fnfVariableLines are the full and final's variable pay lines for a designed structure: the amount HR entered for the
// current cycle (paid), or, with none entered, a note with the target.
func fnfVariableLines(ctx context.Context, q store.Q, e *emp) []fnfLine {
	if e.LWD == nil || !calc.RulesFor(calc.EmploymentType(e.Type)).Variable {
		return nil
	}
	_, st := structureFor(ctx, q, e.ID, *e.LWD)
	if !st.Designed {
		return nil
	}
	if allInOn(ctx, q, (*e.LWD)[:7]) { // all-inclusive gross: paid with the last month's salary, by the days paid
		return []fnfLine{{Key: "var:allin", Kind: "info", Label: "Variable pay", Amount: 0, Note: "Paid every month with salary, by the days paid (all-inclusive gross): nothing more in the full and final"}}
	}
	gross, _ := grossFor(ctx, q, e.ID, (*e.LWD)[:7])
	in := calc.Input{Type: calc.EmploymentType(e.Type), Gross: gross, Structure: &st}
	if t := targetAt(loadTargets(ctx, q, e.ID), (*e.LWD)[:7]); t != nil {
		t.apply(&in)
	}
	res := calc.Calc(in)
	paid := map[string]store.Row{}
	if rows, err := q.C(store.VarPayoutColl).Find(ctx, bson.M{"employee_id": e.ID, "ym": "fnf"}); err == nil {
		for _, r := range rows {
			paid[str(r["code"])] = r
		}
	}
	out := []fnfLine{}
	for _, l := range res.Comps {
		if l.Kind != calc.CompVariable {
			continue
		}
		target := fmt.Sprintf("target %s a month, paid %s", calc.Inr(calc.Round2(l.Target)), cycleWord(l.Cycle))
		if p := paid[l.Code]; p != nil {
			note := "Entered by " + str(p["by_name"]) + "; " + target
			if n := str(p["note"]); n != "" {
				note = n + ". " + note
			}
			out = append(out, fnfLine{Key: "var:" + l.Code, Kind: "earn", Label: l.Name + ", current cycle", Amount: payengineFloat(p["amount"]), Note: note})
			continue
		}
		if l.Target > 0 {
			out = append(out, fnfLine{Key: "var:" + l.Code, Kind: "info", Label: l.Name + ", current cycle: not entered", Amount: 0, Note: "Enter the amount to pay, if any (" + target + ")"})
		}
	}
	return out
}

// varRunChecks is the payroll check for variable pay not entered: only when some row has a designed variable
// component due.
func varRunChecks(rows []*PayRowOut) []calc.Check {
	var who []string
	for _, o := range rows {
		if len(o.VarMissing) > 0 {
			who = append(who, o.Name+" ("+strings.Join(o.VarMissing, ", ")+")")
		}
	}
	if len(who) == 0 {
		return nil
	}
	sort.Strings(who)
	more := ""
	if len(who) > 6 {
		more = " and " + strconv.Itoa(len(who)-6) + " more"
		who = who[:6]
	}
	return []calc.Check{{Title: "Variable pay due but not entered", OK: false,
		Detail: strings.Join(who, "; ") + more + ". Not paid this month: enter the amounts in Payroll › Variable pay (0 to pay nothing), then run the dry run again"}}
}

// ---------- Bulk update › Salary breakup: pay by total and the variable target ----------

const (
	salTotalCol  = "Total Pay (Fixed + Variable)"
	salTargetCol = "Variable Target"
	salModeCol   = "Variable Target In"
	salCompHint  = "Variable Target is the person's variable pay target, paid as HR enters it when due; Variable Target In says how it is given: % (of the fixed monthly gross) or ₹ (a month; blank keeps the current way, else ₹). Total Pay (Fixed + Variable) enters pay as a total a month: the fixed gross is the total less the variable target. These columns are optional; Variable Pay % and Variable Payout Cycle are only for a structure still on percentage rates."
)

// salCompCols reads the two pay-component columns of a Salary breakup row: the variable target (vt) and the fixed
// gross the total pay works out to (tp). designed says whether the person's structure (the row's new one, else the
// one on Effective From) is made from components; cur is their target in force then.
func salCompCols(ctx context.Context, tx store.Q, id, typ string, r bulkRow, num func(string, float64) *float64, gross, vp *float64, res *bulkResult) (vt *float64, mode string, tp *float64, cur *varTargetPt, designed bool) {
	vt = num(salTargetCol, 0)
	total := num(salTotalCol, 1)
	date := excelDate(r.V["Effective From"])
	if !isDate(date) {
		date = today()
	}
	var st calc.Structure
	if v := r.V["Salary Structure"]; v != "" {
		if x, _ := bulkFindCI(ctx, tx, "salary_structure", "name", v, bson.D{{Key: "status", Value: "Active"}}, "name"); x != nil {
			_, st, _ = structureDoc(ctx, tx, str(x["id"]))
		}
	} else {
		_, st = structureFor(ctx, tx, id, date)
	}
	designed = st.Designed
	cur = targetAt(loadTargets(ctx, tx, id), date[:7])
	switch m := strings.TrimSpace(r.V[salModeCol]); {
	case m == "%" || strings.EqualFold(m, "pct"):
		mode = "pct"
	case m == "₹" || strings.EqualFold(m, "rs") || strings.EqualFold(m, "inr") || strings.EqualFold(m, "amount"):
		mode = "amount"
	case m != "":
		res.Errors = append(res.Errors, salModeCol+" is % or ₹")
	case cur != nil:
		mode = cur.Mode
	default:
		mode = "amount"
	}
	if mode == "pct" && vt != nil && *vt > 100 {
		res.Errors = append(res.Errors, salTargetCol+" in % is 0 to 100")
	}
	if total == nil {
		return vt, mode, nil, cur, designed
	}
	var g float64
	if designed {
		t := &varTargetPt{Mode: mode}
		switch {
		case vt != nil && mode == "pct":
			t.Pct = *vt
		case vt != nil:
			t.Total = *vt
		case cur != nil:
			t = cur
		}
		if t.Mode == "pct" {
			g = calc.Round2(*total / (1 + t.Pct/100))
		} else {
			g = calc.Round2(*total - t.Total)
		}
	} else {
		pct := 0.0
		if calc.RulesFor(calc.EmploymentType(typ)).Variable {
			pct = st.VarPct
			if p := payFor(ctx, tx, id, typ); p.VarPct != nil {
				pct = *p.VarPct
			}
			if vp != nil {
				pct = *vp
			}
		}
		g = calc.Round2(*total / (1 + pct/100))
	}
	switch {
	case g <= 0:
		res.Errors = append(res.Errors, salTotalCol+" leaves no fixed gross after the variable part")
		return vt, mode, nil, cur, designed
	case gross != nil && math.Abs(*gross-g) > 0.005:
		res.Errors = append(res.Errors, fmt.Sprintf("Monthly Gross %s and %s (fixed %s) disagree: give one of them", rupees(*gross), salTotalCol, rupees(g)))
		return vt, mode, nil, cur, designed
	}
	return vt, mode, &g, cur, designed
}
