package api

import (
	"context"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Salary TDS (spec 16, prototype salTaxFY / salTds): the year's tax for the months a person is
// employed in the financial year, on their regime, less TDS already deducted and the previous
// employer's TDS, spread over the months left. Recomputed every run; frozen once published.

type taxDecl struct {
	Regime       string             `json:"regime"`
	Status       string             `json:"status"`
	Items        map[string]float64 `json:"items"`
	LandlordName string             `json:"landlordName"`
	LandlordPAN  string             `json:"landlordPan"`
	Other        float64            `json:"otherIncome"`
	PrevIncome   float64            `json:"prevIncome"`
	PrevTDS      float64            `json:"prevTds"`
	Submitted    *time.Time         `json:"submittedAt"`
	Final        *time.Time         `json:"finalAt"`
	FinalDiff    *float64           `json:"finalDiff"`
}

func newDecl() *taxDecl { return &taxDecl{Regime: "New", Status: "Draft", Items: map[string]float64{}} }

type taxData struct {
	decl  map[string]*taxDecl
	appr  map[string]map[string]float64 // approved proof amounts by item
	hist  map[string]map[string]float64 // Section 192 TDS by month
	items []calc.TaxItem                // declaration items counted: switched on, with Payroll's limits
	extra map[string]float64            // taxable salary in the year beyond the monthly gross: incentives (payroll only)
}

func loadTaxData(ctx context.Context, q store.Q, fy string, ids []string) (*taxData, error) {
	t := &taxData{decl: map[string]*taxDecl{}, appr: map[string]map[string]float64{}, hist: map[string]map[string]float64{}}
	ids = compNonNil(ids)
	decls, err := q.C("tax_declaration").Find(ctx, bson.M{"fy": fy, "employee_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	for _, x := range decls {
		d := newDecl()
		d.Regime, d.Status, d.LandlordName, d.LandlordPAN = str(x["regime"]), str(x["status"]), str(x["landlord_name"]), str(x["landlord_pan"])
		if err := compJSONInto(x["items"], &d.Items); err != nil {
			return nil, err
		}
		d.Other, _ = x["other_income"].(float64)
		d.PrevIncome, _ = x["prev_income"].(float64)
		d.PrevTDS, _ = x["prev_tds"].(float64)
		d.Submitted, d.Final = taxTime(x["submitted_at"]), taxTime(x["final_at"])
		if v, ok := x["final_diff"].(float64); ok {
			d.FinalDiff = &v
		}
		t.decl[str(x["employee_id"])] = d
	}
	// Approved proof amounts by person and item.
	sums, err := q.C("tax_proof").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"fy": fy, "status": "Approved", "employee_id": bson.M{"$in": ids}}},
		bson.M{"$group": bson.M{"_id": bson.M{"e": "$employee_id", "item": "$item"}, "v": bson.M{"$sum": "$approved"}}},
	})
	if err != nil {
		return nil, err
	}
	for _, x := range sums {
		g, _ := x["id"].(map[string]any)
		id, item := str(g["e"]), str(g["item"])
		v, _ := x["v"].(float64)
		if t.appr[id] == nil {
			t.appr[id] = map[string]float64{}
		}
		t.appr[id][item] = compRound2(v)
	}
	months := calc.FYMonths(fy)
	hist, err := q.C("tds_history").Find(ctx, bson.M{"section": "192", "ym": bson.M{"$gte": months[0], "$lte": months[11]}, "employee_id": bson.M{"$in": ids}},
		options.Find().SetProjection(compFields("employee_id", "ym", "amount")))
	if err != nil {
		return nil, err
	}
	for _, x := range hist {
		id := str(x["employee_id"])
		if t.hist[id] == nil {
			t.hist[id] = map[string]float64{}
		}
		t.hist[id][str(x["ym"])], _ = x["amount"].(float64)
	}
	ti, err := loadTaxItems(ctx, q)
	if err != nil {
		return nil, err
	}
	t.items = countedTaxItems(ti)
	return t, nil
}

func (t *taxData) declOf(id string) *taxDecl {
	if d := t.decl[id]; d != nil {
		return d
	}
	return newDecl()
}

// salaryTdsType: permanent staff (all deductions and payslip documents) have Section 192 TDS.
func salaryTdsType(typ string) bool {
	r := calc.RulesFor(calc.EmploymentType(typ))
	return r.Ded == "all" && r.Docs
}

type taxFig struct {
	FY       string             `json:"fy"`
	First    string             `json:"first"`
	Last     string             `json:"last"`
	Months   int                `json:"months"`
	Gross    float64            `json:"gross"`           // monthly gross used for the projection
	Reimb    float64            `json:"reimb,omitempty"` // the Reimbursement a month, outside the gross (counted when taxable)
	Basis    string             `json:"basis"`           // declared | approved
	Est      calc.TaxResult     `json:"est"`
	Declared calc.TaxResult     `json:"declared"`
	Approved calc.TaxResult     `json:"approved"`
	Annual   float64            `json:"annual"`
	PFYear   float64            `json:"pfYear"`
	Groups   map[string]float64 `json:"groups"`
}

func monthsIn(a, b string) int {
	n := 0
	for m := a; m <= b && n < 24; m = nextYm(m) {
		n++
	}
	return n
}

// figure works out the year's tax for one person in a financial year.
func taxFigure(e *emp, gross float64, st *calc.Structure, city string, d *taxDecl, appr map[string]float64, fy string) *taxFig {
	return taxFigureWith(e, gross, st, city, d, appr, fy, calc.TaxItems)
}

// taxFigureWith is taxFigure counting the given declaration items (Payroll's limits and on/off).
func taxFigureWith(e *emp, gross float64, st *calc.Structure, city string, d *taxDecl, appr map[string]float64, fy string, list []calc.TaxItem) *taxFig {
	return taxFigureExtra(e, gross, st, city, d, appr, fy, list, 0, false)
}

// taxFigureExtra is taxFigureWith with extra taxable salary in the year on top of the monthly gross (the year's
// incentives, bulk_incentive.go). allBasic: the person chose no PF and is paid the whole fixed gross as Basic
// (pfuan.go): the projection uses that split (HRA 0, so no HRA exemption; no employee PF), as the payroll pays it.
//
// mods set the rest of the month's inputs (the per-employee amounts, the variable target, the all-inclusive gross).
// On the all-inclusive gross (allin.go) the salary is the gross G (with the statutory bonus inside it) plus the variable
// pay paid every month and the taxable Reimbursement; the employer's PF deducted from the employee counts as the
// employee's PF does (80C in the old regime), the employer's ESI as the employee's ESI does (not at all).
func taxFigureExtra(e *emp, gross float64, st *calc.Structure, city string, d *taxDecl, appr map[string]float64, fy string, list []calc.TaxItem, extra float64, allBasic bool, mods ...func(*calc.Input)) *taxFig {
	ms := calc.FYMonths(fy)
	a, b := ms[0], ms[11]
	if e.DOJ[:7] > a {
		a = e.DOJ[:7]
	}
	if e.LWD != nil && (*e.LWD)[:7] < b {
		b = (*e.LWD)[:7]
	}
	n := monthsIn(a, b)
	if n <= 0 || gross <= 0 {
		return nil
	}
	// One month's breakup times the months employed: the PF ceiling applies month by month, so a part year is not
	// worked out on an averaged gross (SET-111). For a full year it is the same as before.
	in := calc.Input{Type: calc.EmploymentType(e.Type), Gross: gross, Structure: st, PfExempt: allBasic, AllBasic: allBasic}
	for _, m := range mods { // the per-employee amounts (the Reimbursement may be set as not taxable, reimb.go) and the rest
		m(&in)
	}
	r := calc.Calc(in)
	k := float64(n)
	est := func(items map[string]float64) (calc.TaxResult, map[string]float64) {
		groups, rent := calc.GroupDeductionsWith(items, list)
		salYear := gross*k + extra
		if r.NonTax != 0 || r.Reimb != 0 { // the Reimbursement is outside the gross (25 Nov 2026); less what is not taxable
			salYear = (gross+r.Reimb-r.NonTax)*k + extra
		}
		pfYear := r.EePF * k
		if r.AllIn { // the gross G (with the statutory bonus), the Reimbursement and the monthly variable pay
			salYear = (r.Gross+r.Reimb+r.VarPay-r.NonTax)*k + extra
			pfYear = (r.EePF + r.ErPF) * k // the employer's PF is deducted from the employee: counted as the employee's PF is
		}
		return calc.TaxEstimate(calc.TaxInput{SalaryYear: salYear, BasicYear: r.Basic * k, HraYear: r.Hra * k, EmployeePfYear: pfYear,
			OtherIncome: d.Other, PrevEmployerIncome: d.PrevIncome, RentYear: rent, Metro: calc.MetroCities[city], Deductions: groups}), groups
	}
	f := &taxFig{FY: fy, First: a, Last: b, Months: n, Gross: gross, Reimb: calc.Round2(r.Reimb), Basis: "declared", PFYear: calc.Round2(r.EePF * k)}
	if r.AllIn { // the employer's PF deducted from the employee too
		f.PFYear = calc.Round2((r.EePF + r.ErPF) * k)
	}
	f.Declared, f.Groups = est(d.Items)
	f.Approved, _ = est(appr)
	f.Est = f.Declared
	if d.Final != nil {
		f.Basis, f.Est = "approved", f.Approved
		_, f.Groups = est(appr)
	}
	f.Annual = f.Est.NewTax
	if d.Regime == "Old" {
		f.Annual = f.Est.OldTax
	}
	return f
}

// monthTds is the Section 192 TDS for a month: tax still due, spread over the months left.
func monthTds(f *taxFig, d *taxDecl, hist map[string]float64, ym string) float64 {
	if f == nil || ym < f.First || ym > f.Last {
		return 0
	}
	paid := 0.0
	for m, v := range hist {
		if m < ym {
			paid += v
		}
	}
	due := math.Max(0, f.Annual-paid-d.PrevTDS)
	return math.Round(due / float64(max(1, monthsIn(ym, f.Last))))
}

// loadSalaryTds fills the month's Section 192 TDS per employee.
func (pc *payCtx) loadSalaryTds(ctx context.Context, q store.Q, list []*emp) error {
	fy := calc.FY(pc.ym)
	td, err := loadTaxData(ctx, q, fy, ids(list))
	if err != nil {
		return err
	}
	if err := addApprovedTds(ctx, q, td, fy, pc.ym, ids(list)); err != nil { // RUN-85: approved, not yet published
		return err
	}
	pc.tax = td
	if td.extra, err = incentivesInYear(ctx, q, fy, ids(list)); err != nil {
		return err
	}
	if err := varPayoutsInYear(ctx, q, fy, ids(list), td.extra, pc.allInFrom); err != nil { // variable amounts entered (paycomp.go)
		return err
	}
	for _, e := range list {
		if !salaryTdsType(e.Type) {
			continue
		}
		d := td.declOf(e.ID)
		f := taxFigureExtra(e, pc.grossAt(e.ID, pc.ym), pc.structure(pc.structureOn(e.ID, pc.ym+"-01")), pc.cities[e.CityID], d, td.appr[e.ID], fy, td.items,
			td.extra[e.ID], pc.pay[e.ID].allBasicIn(pc.ym), pc.taxInputs(e))
		pc.salaryTds[e.ID] = monthTds(f, d, td.hist[e.ID], pc.ym)
	}
	return nil
}

// figureFor is the tax figure of one employee in the payroll context.
func (pc *payCtx) figureFor(e *emp, fy string) *taxFig {
	if !salaryTdsType(e.Type) || pc.tax == nil {
		return nil
	}
	return taxFigureExtra(e, pc.grossAt(e.ID, pc.ym), pc.structure(pc.structureOn(e.ID, pc.ym+"-01")), pc.cities[e.CityID], pc.tax.declOf(e.ID), pc.tax.appr[e.ID], fy, pc.tax.items,
		pc.tax.extra[e.ID], pc.pay[e.ID].allBasicIn(pc.ym), pc.taxInputs(e))
}

// taxInputs are the month's inputs the tax projection adds: the per-employee amounts and, on the all-inclusive gross
// (allin.go), the rule, the LWF and the variable target (paid every month).
func (pc *payCtx) taxInputs(e *emp) func(*calc.Input) {
	return func(in *calc.Input) {
		in.EmpAmts = pc.pay[e.ID].CompAmts
		if !pc.allIn(pc.ym) {
			return
		}
		in.AllIn, in.PfExempt, in.Lwf = true, in.PfExempt || pc.pay[e.ID].PfExempt, pc.lwf[e.StateID]
		in.Structure = withMedical(in.Structure, pc.pay[e.ID].Medical)
		if in.Structure.Designed {
			pc.varInputs(e.ID, pc.ym, in)
		} else {
			in.VarPct = pc.varPctOpt(e.ID, pc.ym)
		}
	}
}

// taxReadiness is the employee file's tax line.
func (s *Server) taxReadiness(ctx context.Context, e *emp) readyItem {
	if !salaryTdsType(e.Type) {
		return readyItem{"tax", "Tax structure", "na", e.Type + " staff do not declare tax with the company", "—"}
	}
	fy := calc.FY(openYm(ctx, s.st.DB))
	w := taxWindowsFor(ctx, s.st.DB, fy)
	d, _ := s.st.DB.C("tax_declaration").FindOne(ctx, bson.M{"employee_id": e.ID, "fy": fy}, options.FindOne().SetProjection(compFields("status", "regime", "final_at")))
	status, regime, final := str(d["status"]), str(d["regime"]), taxTime(d["final_at"])
	n, _ := s.st.DB.C("tax_proof").Count(ctx, bson.M{"employee_id": e.ID, "fy": fy, "status": "Pending"})
	pending := int(n)
	switch {
	case pending > 0:
		return readyItem{"tax", "Tax structure", "pending", itoa(pending) + " proof" + map[bool]string{true: "", false: "s"}[pending == 1] + " waiting for Payroll", "Payroll"}
	case final != nil:
		return readyItem{"tax", "Tax structure", "ready", "Finalised", "—"}
	case status == "Declared":
		return readyItem{"tax", "Tax structure", "ready", regime + " regime declared", "Employee"}
	case w.DeclOpen:
		return readyItem{"tax", "Tax structure", "pending", "Declaration not submitted. New regime applies until it is", "Employee"}
	}
	return readyItem{"tax", "Tax structure", "ready", "Not declared. New regime applies", "Employee"}
}

type taxWindows struct {
	FY         string  `json:"fy"`
	DeclFrom   string  `json:"declFrom"`
	DeclTo     string  `json:"declTo"`
	DeclForce  *bool   `json:"declForce"`
	ProofFrom  string  `json:"proofFrom"`
	ProofTo    string  `json:"proofTo"`
	ProofForce *bool   `json:"proofForce"`
	DeclOpen   bool    `json:"declOpen"`
	ProofOpen  bool    `json:"proofOpen"`
	Finalised  *string `json:"finalised"`
}

// taxWindowsFor reads a year's windows; without settings the declaration runs April to
// December and proofs 5 January to 10 February.
func taxWindowsFor(ctx context.Context, q store.Q, fy string) taxWindows {
	y := fy[:4]
	ny := itoa(atoiSafe(y) + 1)
	w := taxWindows{FY: fy, DeclFrom: y + "-04-01", DeclTo: y + "-12-31", ProofFrom: ny + "-01-05", ProofTo: ny + "-02-10"}
	var df, dt, pf, pt *string
	if x, _ := q.C("tax_window").FindOne(ctx, bson.M{"fy": fy}); x != nil {
		df, dt, pf, pt = taxText(x["decl_from"]), taxText(x["decl_to"]), taxText(x["proof_from"]), taxText(x["proof_to"])
		if v, ok := x["decl_force"].(bool); ok {
			w.DeclForce = &v
		}
		if v, ok := x["proof_force"].(bool); ok {
			w.ProofForce = &v
		}
		// "12 Feb 2027 by Name" once finalised (null while either part is missing).
		if day, by := compDay(x["finalised_at"]), taxText(x["finalised_by"]); day != nil && by != nil {
			f := day.(string) + " by " + *by
			w.Finalised = &f
		}
	}
	for _, p := range []struct {
		src *string
		dst *string
	}{{df, &w.DeclFrom}, {dt, &w.DeclTo}, {pf, &w.ProofFrom}, {pt, &w.ProofTo}} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	open := func(force *bool, a, b string) bool {
		if force != nil {
			return *force
		}
		t := today()
		return t >= a && t <= b
	}
	w.DeclOpen, w.ProofOpen = open(w.DeclForce, w.DeclFrom, w.DeclTo), open(w.ProofForce, w.ProofFrom, w.ProofTo)
	if w.Finalised != nil {
		w.DeclOpen, w.ProofOpen = false, false
	}
	return w
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// tdsDue is the deposit due date: the 7th of the next month, 30 April for March.
func tdsDue(ym string) string {
	if ym[5:7] == "03" {
		return ym[:4] + "-04-30"
	}
	return nextYm(ym) + "-07"
}

// incentivesInYear adds up each person's incentive lines (not cancelled) in the financial year's months: taxable salary
// the monthly gross does not show (bulk_incentive.go).
func incentivesInYear(ctx context.Context, q store.Q, fy string, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	ms := calc.FYMonths(fy)
	lines, err := q.C("ledger").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}, "kind": "earn", "label": incentiveLabel, "void": false,
		"ym": bson.M{"$gte": ms[0], "$lte": ms[11]}}, options.Find().SetProjection(bson.M{"employee_id": 1, "amount": 1}))
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		out[str(l["employee_id"])] += payengineFloat(l["amount"])
	}
	return out, nil
}

// varPayoutsInYear adds to extra the variable amounts entered for the financial year's months (designed structures):
// taxable salary, like incentives.
//
// From allInFrom on (the all-inclusive gross) amounts entered are not paid (variable pay is automatic), so not counted.
func varPayoutsInYear(ctx context.Context, q store.Q, fy string, ids []string, extra map[string]float64, allInFrom string) error {
	ms := calc.FYMonths(fy)
	ym := bson.M{"$gte": ms[0], "$lte": ms[11]}
	if allInFrom != "" {
		ym["$lt"] = allInFrom
	}
	rows, err := q.C(store.VarPayoutColl).Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}, "ym": ym},
		options.Find().SetProjection(bson.M{"employee_id": 1, "amount": 1}))
	if err != nil {
		return err
	}
	for _, r := range rows {
		extra[str(r["employee_id"])] += payengineFloat(r["amount"])
	}
	return nil
}
