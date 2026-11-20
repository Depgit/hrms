package api

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// The pay engine: everything a month's pay rows need is loaded in a few queries, then each
// row is computed in memory (spec 14 "Per month", prototype payRow). Used by the dry run, the
// employee file and the payslip preview.

func ymTime(ym string) time.Time { t, _ := time.Parse("2006-01", ym); return t }

func addYm(ym string, n int) string { return ymTime(ym).AddDate(0, n, 0).Format("2006-01") }

func nextYm(ym string) string { return addYm(ym, 1) }

func ymLabel(ym string) string { return ymTime(ym).Format("January 2006") }

func isYm(s string) bool { _, err := time.Parse("2006-01", s); return err == nil && len(s) == 7 }

// openYm is the payroll month new pay lines go into: the month after the last approved run,
// else the first month on record, else the month set in payroll settings, else last month.
func openYm(ctx context.Context, q store.Q) string {
	months := q.C("payroll_month")
	newest := options.FindOne().SetSort(bson.D{{Key: "ym", Value: -1}})
	if last, _ := months.FindOne(ctx, bson.M{"status": bson.M{"$in": bson.A{"Approved", "Published"}}}, newest); last != nil {
		return nextYm(str(last["ym"]))
	}
	if first, _ := months.FindOne(ctx, nil, options.FindOne().SetSort(bson.D{{Key: "ym", Value: 1}})); first != nil {
		return str(first["ym"])
	}
	if set, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}); set != nil {
		if ym := str(set["first_ym"]); isYm(ym) {
			return ym
		}
	}
	return addYm(today()[:7], -1)
}

// window is the attendance period paid in a month: the calendar month, or 26th to 25th.
func window(ym, cutoff string) (string, string) {
	if cutoff == "26-25" {
		return addYm(ym, -1) + "-26", ym + "-25"
	}
	return ym + "-01", policy.LastDayOf(ym)
}

func cutoffSetting(ctx context.Context, q store.Q) string {
	c := "calendar"
	if set, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}); set != nil {
		if v, ok := set["cutoff"].(string); ok {
			c = v
		}
	}
	return c
}

// payEligible picks who is paid in month ym (spec 15.1 #3), as a filter on employee documents: not
// pre-boarding, joined by the month's last day, and active (not left before the month), left in or
// after the month, or with pay lines waiting in the month.
func payEligible(ctx context.Context, q store.Q, ym string) (bson.M, error) {
	first, last := ym+"-01", policy.LastDayOf(ym)
	withLines, err := q.C("ledger").Distinct(ctx, "employee_id", bson.M{"ym": ym, "void": false})
	if err != nil {
		return nil, err
	}
	return bson.M{"status": bson.M{"$ne": "Pre-boarding"}, "doj": bson.M{"$lte": last}, "$or": bson.A{
		bson.M{"status": "Active", "$or": bson.A{bson.M{"lwd": nil}, bson.M{"lwd": bson.M{"$gte": first}}}},
		bson.M{"status": "Inactive", "lwd": bson.M{"$gte": first}},
		bson.M{"_id": bson.M{"$in": withLines}},
	}}, nil
}

// payengineAnd is the AND of filters on the same documents (nil and empty ones left out).
func payengineAnd(filters ...any) any {
	all := bson.A{}
	for _, f := range filters {
		switch x := f.(type) {
		case nil:
			continue
		case bson.M:
			if len(x) == 0 {
				continue
			}
		case bson.D:
			if len(x) == 0 {
				continue
			}
		}
		all = append(all, f)
	}
	switch len(all) {
	case 0:
		return bson.D{}
	case 1:
		return all[0]
	}
	return bson.D{{Key: "$and", Value: all}}
}

// payengineJSON decodes a stored jsonb value (read back as maps and slices) into out, as pgx scanned
// jsonb with encoding/json: fields the JSON leaves out keep what out already holds.
func payengineJSON(v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// payengineFloat is a stored number as float64 (0 for null).
func payengineFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case int16:
		return float64(x)
	}
	return 0
}

// payengineFloatPtr is a nullable stored number (nil for null).
func payengineFloatPtr(v any) *float64 {
	if v == nil {
		return nil
	}
	f := payengineFloat(v)
	return &f
}

// payengineIntPtr is a nullable stored integer (nil for null).
func payengineIntPtr(v any) *int {
	if v == nil {
		return nil
	}
	n := int(payengineFloat(v))
	return &n
}

// payengineStrings is a stored text[] as []string: nil for null, as pgx scanned it.
func payengineStrings(v any) []string {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, str(x))
	}
	return out
}

// payengineFill gives every row the keys a query selected, null where the document had nothing (a LEFT
// JOIN without a match).
func payengineFill(rows []store.Row, keys ...string) {
	for _, r := range rows {
		for _, k := range keys {
			if _, ok := r[k]; !ok {
				r[k] = nil
			}
		}
	}
}

// payengineStamp formats a stored time as to_char did ("02 Jan 2006 15:04" for 'DD Mon YYYY HH24:MI'), nil for null.
func payengineStamp(v any, layout string) any {
	if t, ok := v.(time.Time); ok {
		return t.In(store.Loc()).Format(layout)
	}
	return nil
}

type grossPt struct {
	From  string
	Date  string // the exact date it applies from ("" = the 1st of From); a mid-month date splits that month (ITM-28)
	Gross float64
}

type structPt struct {
	From string
	ID   string
}

type payOverride struct {
	PaidDays  *float64 `json:"paidDays"`
	OtherEarn float64  `json:"otherEarn"`
	OtherDed  float64  `json:"otherDed"`
	Reason    string   `json:"reason"`
	By        string   `json:"by"`
}

type payCtx struct {
	ym, from, to string
	ts           *timeSet
	md           *monthData
	emps         map[string]*emp
	cities       map[string]string
	gross        map[string][]grossPt
	structs      map[string]calc.Structure
	structNames  map[string]string
	defStruct    string
	smap         map[string][]structPt
	pay          map[string]payOptions
	tdsPct       map[string]*float64
	varFrom      map[string]varFrom       // the month the variable pay % applies from, and the % before it (RUN-31)
	lwf          map[string]*calc.LwfRule // by state
	lines        map[string][]calc.Line
	ov           map[string]payOverride
	hold         map[string]string // why the row is on hold
	prevVar      map[string]map[string]float64
	prevNet      map[string]float64
	salaryTds    map[string]float64
	tax          *taxData
	// Pay components (paycomp.go): each person's dated variable targets and the variable amounts entered for this
	// month, by component code. classic runs the calculation as before components (hrms pay-compare only).
	vtarget map[string][]varTargetPt
	vpaid   map[string]map[string]float64
	classic bool
}

// PayRowOut is one person's row in a payroll month, stored as payroll_row.data.
type PayRowOut struct {
	EmployeeID string         `json:"employeeId"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Grade      string         `json:"grade"`
	Dept       string         `json:"dept"`
	Desig      string         `json:"desig"`
	City       string         `json:"city"`
	CityID     string         `json:"cityId"`
	Location   string         `json:"location"`
	LocID      string         `json:"locId"`
	ManagerID  string         `json:"managerId"`
	DOJ        string         `json:"doj"`
	LWD        string         `json:"lwd,omitempty"`
	Status     string         `json:"status"`
	Structure  string         `json:"structure"`
	Blended    bool           `json:"blended,omitempty"`
	Counts     map[string]int `json:"counts"`
	Missing    int            `json:"missing"`
	Prev       float64        `json:"prev"`
	Hold       bool           `json:"hold"`
	HoldWhy    string         `json:"holdWhy,omitempty"`
	Edited     bool           `json:"edited"`
	Reason     string         `json:"reason,omitempty"`
	Flags      []string       `json:"flags"`
	Nights     int            `json:"nights,omitempty"`
	// VarMissing names the variable components of a designed structure due this month with no amount entered: not
	// paid, and listed in the payroll checks.
	VarMissing []string `json:"varMissing,omitempty"`
	calc.Row
}

func (o *PayRowOut) scoped() access.ScopedEmployee {
	return access.ScopedEmployee{ID: o.EmployeeID, LocationID: o.LocID, Department: o.Dept, ManagerID: o.ManagerID}
}

// loadPayCtx loads pay data for the employees paid in month ym who match filter (a filter on employee
// documents; nil for everyone paid).
func loadPayCtx(ctx context.Context, q store.Q, ym string, filter any) (*payCtx, []*emp, error) {
	eligible, err := payEligible(ctx, q, ym)
	if err != nil {
		return nil, nil, err
	}
	return loadPayCtxOpt(ctx, q, ym, true, eligible, filter)
}

// loadPayCtxOpt is loadPayCtx with the base selection given (payEligible for a month, a financial year's
// base for tax figures, nil for anyone) and, without attendance, no punches or roster loaded (tax figures
// need only pay data). base and filter are filters on employee documents.
func loadPayCtxOpt(ctx context.Context, q store.Q, ym string, attendance bool, base, filter any) (*payCtx, []*emp, error) {
	syncTypeRules(ctx, q) // a payroll run in the background uses the stored rules by employment type too
	from, to := window(ym, cutoffSetting(ctx, q))
	last := policy.LastDayOf(ym)
	list, err := loadEmps(ctx, q, payengineAnd(base, filter))
	if err != nil {
		return nil, nil, err
	}
	pc := &payCtx{ym: ym, from: from, to: to, emps: map[string]*emp{}, cities: map[string]string{}, gross: map[string][]grossPt{},
		structs: map[string]calc.Structure{}, structNames: map[string]string{}, smap: map[string][]structPt{}, pay: map[string]payOptions{}, varFrom: map[string]varFrom{},
		tdsPct: map[string]*float64{}, lwf: map[string]*calc.LwfRule{}, lines: map[string][]calc.Line{}, ov: map[string]payOverride{},
		hold: map[string]string{}, prevVar: map[string]map[string]float64{}, prevNet: map[string]float64{}, salaryTds: map[string]float64{},
		vtarget: map[string][]varTargetPt{}, vpaid: map[string]map[string]float64{}}
	ids := ids(list)
	for _, e := range list {
		pc.emps[e.ID] = e
	}
	if attendance {
		if pc.ts, err = loadTimeSet(ctx, q); err != nil {
			return nil, nil, err
		}
		if pc.md, err = loadMonth(ctx, q, pc.ts, ids, from, to); err != nil {
			return nil, nil, err
		}
	}
	ofThem := bson.M{"$in": ids} // employee_id of the people loaded
	cities, err := q.C("city").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range cities {
		pc.cities[str(r["id"])] = str(r["name"])
	}
	gross, err := q.C("gross_history").Find(ctx, bson.M{"employee_id": ofThem},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range gross {
		id := str(r["employee_id"])
		pc.gross[id] = append(pc.gross[id], grossPt{From: str(r["from_month"]), Date: str(r["from_date"]), Gross: payengineFloat(r["gross"])})
	}
	structs, err := q.C("salary_structure").Find(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range structs {
		id := str(r["id"])
		var st calc.Structure
		_ = payengineJSON(r["rates"], &st)
		pc.structs[id], pc.structNames[id] = st, str(r["name"])
		if r["is_default"] == true {
			pc.defStruct = id
		}
	}
	maps, err := q.C("structure_map").Find(ctx, bson.M{"employee_id": ofThem},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_date", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range maps {
		id := str(r["employee_id"])
		pc.smap[id] = append(pc.smap[id], structPt{From: str(r["from_date"]), ID: str(r["structure_id"])})
	}
	pays, err := q.C("employee_pay").Find(ctx, bson.M{"employee_id": ofThem})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range pays {
		id := str(r["employee_id"])
		pc.pay[id] = payOptions{VarPct: payengineFloatPtr(r["var_pct"]), VarCycle: payengineIntPtr(r["var_cycle"]),
			Meal: payengineFloat(r["meal"]), Accom: payengineFloat(r["accommodation"]), Medical: payengineFloatPtr(r["medical"]), PfExempt: r["pf_exempt"] == true, VarStart: str(r["var_start"]),
			AllBasic: r["all_basic"] == true, AllBasicFrom: str(r["all_basic_from"]), CompAmts: byCompOf(r["comp_amts"])}
		pc.tdsPct[id] = payengineFloatPtr(r["tds_pct"])
		if f := str(r["var_from"]); f != "" {
			pc.varFrom[id] = varFrom{From: f, Prev: payengineFloatPtr(r["var_prev_pct"])}
		}
	}
	// The labour welfare fund rule in force in each state on the month's last day (the latest that started by then).
	rules, err := q.C("lwf_rule").Find(ctx, bson.M{"effective_from": bson.M{"$lte": last}},
		options.Find().SetSort(bson.D{{Key: "state_id", Value: 1}, {Key: "effective_from", Value: -1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rules {
		st := str(r["state_id"])
		if _, seen := pc.lwf[st]; seen {
			continue
		}
		var rule calc.LwfRule
		_ = payengineJSON(r["rule"], &rule)
		pc.lwf[st] = &rule
	}
	lines, err := q.C("ledger").Find(ctx, bson.M{"ym": ym, "void": false, "employee_id": ofThem},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range lines {
		id := str(r["employee_id"])
		lid, _ := r["id"].(int64)
		pc.lines[id] = append(pc.lines[id], calc.Line{ID: lid, Kind: str(r["kind"]), Label: str(r["label"]), Amount: payengineFloat(r["amount"]),
			Reason: str(r["reason"]), Source: str(r["source_id"])})
	}
	ovs, err := q.C("payroll_override").Find(ctx, bson.M{"ym": ym, "employee_id": ofThem})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range ovs {
		pc.ov[str(r["employee_id"])] = payOverride{PaidDays: payengineFloatPtr(r["paid_days"]), OtherEarn: payengineFloat(r["other_earn"]),
			OtherDed: payengineFloat(r["other_ded"]), Reason: str(r["reason"]), By: str(r["by_name"])}
	}
	// On hold: an active salary hold that started by this month and has not ended before it (a hold placed from the
	// payroll checklist holds its months only, to_ym), else a new bank account waiting for verification.
	holds, err := q.C("salary_hold").Find(ctx, bson.M{"status": "Active", "from_ym": bson.M{"$lte": ym}, "employee_id": ofThem, "$or": holdThrough(ym)})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range holds {
		if id := str(r["employee_id"]); pc.hold[id] == "" {
			pc.hold[id] = str(r["reason"]) + " since " + str(r["from_ym"])
		}
	}
	banks, err := q.C("approval_request").Find(ctx, bson.M{"wf": "p_change", "status": "Pending", "payload.kind": "Bank", "employee_id": ofThem},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range banks {
		if id := str(r["employee_id"]); pc.hold[id] == "" {
			pc.hold[id] = "New bank account waiting for verification"
		}
	}
	// The stored rows of the eleven months before: the variable accrued, and last month's net.
	prev, err := q.C("payroll_row").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"ym": bson.M{"$gte": addYm(ym, -11), "$lt": ym}, "employee_id": ofThem}},
		bson.M{"$project": bson.M{"_id": 0, "employee_id": 1, "ym": 1, "net": 1, "variable": "$data.r.variable"}},
	})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range prev {
		id, m := str(r["employee_id"]), str(r["ym"])
		if pc.prevVar[id] == nil {
			pc.prevVar[id] = map[string]float64{}
		}
		if v := payengineFloatPtr(r["variable"]); v != nil {
			pc.prevVar[id][m] = *v
		}
		if m == addYm(ym, -1) {
			pc.prevNet[id] = payengineFloat(r["net"])
		}
	}
	if err := pc.loadVarPay(ctx, q, ofThem); err != nil {
		return nil, nil, err
	}
	if err := pc.loadSalaryTds(ctx, q, list); err != nil {
		return nil, nil, err
	}
	return pc, list, nil
}

func (pc *payCtx) grossAt(id, ym string) float64 {
	return grossInMonth(pc.gross[id], ym)
}

// grossInMonth is the gross for month ym from a history sorted by month: the latest revision from that month or
// earlier, and when that revision starts after the 1st of ym, the earlier gross for the days before it and the new
// one from it (user decision 13 Nov 2026, ITM-28).
func grossInMonth(h []grossPt, ym string) float64 {
	g, prev, date := 0.0, 0.0, ""
	for _, p := range h {
		if p.From <= ym {
			prev, g, date = g, p.Gross, p.Date
		}
	}
	if g == 0 && len(h) > 0 && h[0].From > ym {
		return 0 // not on the payroll yet in that month
	}
	return calc.SplitGross(ym, prev, g, date)
}

// structureOn is the structure mapped on a date, else the default.
func (pc *payCtx) structureOn(id, date string) string {
	sid := pc.defStruct
	for _, p := range pc.smap[id] {
		if p.From <= date {
			sid = p.ID
		}
	}
	return sid
}

func (pc *payCtx) structure(sid string) *calc.Structure {
	if st, ok := pc.structs[sid]; ok {
		return &st
	}
	st := calc.DefaultStructure
	return &st
}

// varFrom is when an employee's variable pay % changed: the month it applies from and the % before (nil: the
// structure's).
type varFrom struct {
	From string
	Prev *float64
}

// varPctOpt is the employee's own variable pay % for month ym (nil: the structure's): the % before a change for
// months before it applies.
func (pc *payCtx) varPctOpt(id, ym string) *float64 {
	if v, ok := pc.varFrom[id]; ok && ym < v.From {
		return v.Prev
	}
	return pc.pay[id].VarPct
}

// variablePctAt is the employee's variable share for month ym (0 when the type has no variable pay).
func (pc *payCtx) variablePctAt(e *emp, st *calc.Structure, ym string) float64 {
	if !calc.RulesFor(calc.EmploymentType(e.Type)).Variable {
		return 0
	}
	if p := pc.varPctOpt(e.ID, ym); p != nil {
		return *p
	}
	return st.VarPct
}

// varAnchor is the month an employee's variable payout cycle is counted from: the joining month, or the month
// variable pay started when it started after joining (var_start; for a change saved before var_start was kept, var_from
// when the % before it was nothing). A payout falls in the anchor month + cycle − 1, then every cycle months.
func (pc *payCtx) varAnchor(e *emp, st *calc.Structure) string {
	var vf *varFrom
	if v, ok := pc.varFrom[e.ID]; ok {
		vf = &v
	}
	return varAnchorYm(e.DOJ[:7], pc.pay[e.ID].VarStart, vf, st.VarPct)
}

// varAnchorYm is varAnchor's rule on its own: doj and start are "YYYY-MM" ("" start: none); vf the last change of the
// variable % (nil: none); structPct the structure's standard %, the % before a change that had none of its own.
func varAnchorYm(doj, start string, vf *varFrom, structPct float64) string {
	a := doj
	if start == "" && vf != nil {
		before := structPct
		if vf.Prev != nil {
			before = *vf.Prev
		}
		if before <= 0 {
			start = vf.From
		}
	}
	if start > a {
		a = start
	}
	return a
}

// prevVariable lists the accruals of the earlier months of the variable cycle: the stored row
// when that month was run, a joining month pro rata by days, months before joining nothing.
func (pc *payCtx) prevVariable(e *emp, cycle int, pctAt func(ym string) float64) []float64 {
	out := []float64{}
	doj := e.DOJ[:7]
	for k := 1; k < cycle; k++ {
		pm := addYm(pc.ym, -k)
		if pm < doj {
			break
		}
		if v, ok := pc.prevVar[e.ID][pm]; ok {
			out = append(out, v)
			continue
		}
		v := pc.grossAt(e.ID, pm) * pctAt(pm) / 100
		if pm == doj {
			dim := float64(calc.PayDays(pm, "calendar"))
			v = v * (dim - float64(policy.DayDiff(pm+"-01", e.DOJ))) / dim
		}
		out = append(out, v)
	}
	return out
}

// row computes one person's pay for the month.
func (pc *payCtx) row(e *emp) *PayRowOut {
	ym := pc.ym
	// Without attendance loaded (the cost console's projection, cost.go) the month is a full month: no loss of pay,
	// no days outside employment, no night shifts.
	var p *workPolicy
	var mo attend.Month
	if pc.md != nil {
		p, _ = pc.ts.policyFor(e)
		mo = pc.md.span(e, p, pc.from, pc.to)
	}
	first, last := ym+"-01", policy.LastDayOf(ym)
	sid := pc.structureOn(e.ID, first)
	st := pc.structure(sid)
	gross := pc.grossAt(e.ID, ym)
	pay := pc.pay[e.ID]
	month := int(ymTime(ym).Month())
	// The variable payout cycle is the person's own: counted from the joining month, or from the month variable pay
	// started when it started later (owner, 14 Nov 2026). Calc counts it as month of the year, which works for cycles of
	// 1, 3, 6 and 12 months.
	joinMonth := int(ymTime(pc.varAnchor(e, st)).Month())
	esiKeep := calc.EsiKeep(ym, e.DOJ[:7], func(m string) float64 { return pc.grossAt(e.ID, m) }, st.EsiCeil)
	// A structure change inside the month blends both calculations by days (spec 13).
	type part struct {
		sid string
		w   float64
	}
	parts := []part{{sid, 1}}
	cuts := []string{}
	for _, m := range pc.smap[e.ID] {
		if m.From > first && m.From <= last {
			cuts = append(cuts, m.From)
		}
	}
	if len(cuts) > 0 {
		sort.Strings(cuts)
		dim := float64(policy.DayDiff(first, last) + 1)
		pts := append([]string{first}, cuts...)
		parts = parts[:0]
		for i, f := range pts {
			end := policy.AddDays(last, 1)
			if i+1 < len(pts) {
				end = pts[i+1]
			}
			parts = append(parts, part{pc.structureOn(e.ID, f), float64(policy.DayDiff(f, end)) / dim})
		}
	}
	var lwf *calc.LwfRule
	if r := pc.lwf[e.StateID]; r != nil {
		lwf = r
	}
	in := calc.RowIn{Days: calc.PayDays(ym, st.Denom), LOP: mo.LOP, Outside: float64(mo.Outside), DaysN: mo.DaysN, Gross: gross,
		Lines: append([]calc.Line{}, pc.lines[e.ID]...), SalaryTds: pc.salaryTds[e.ID]}
	// No overtime and no pay for off-day work: the company pays none. The night shift allowance stays.
	nights := 0
	if p != nil {
		nights = pc.nightShifts(e, p)
	}
	if nights > 0 && p.S.Shift.NightAllow > 0 {
		in.Lines = append(in.Lines, calc.Line{Kind: "earn", Label: "Night shift allowance (" + itoa(nights) + ")", Amount: float64(nights) * p.S.Shift.NightAllow})
	}
	if o, ok := pc.ov[e.ID]; ok {
		in.PaidOverride, in.OtherEarn, in.OtherDed = o.PaidDays, o.OtherEarn, o.OtherDed
	}
	in.Calc = func(earned float64, tot *float64) calc.Result {
		res := make([]calc.Result, len(parts))
		ws := make([]float64, len(parts))
		for i, pt := range parts {
			s := pc.structure(pt.sid)
			eg := gross
			in := calc.Input{Type: calc.EmploymentType(e.Type), Gross: earned, EligGross: &eg, Month: month, JoinMonth: joinMonth,
				VarPct: pc.varPctOpt(e.ID, ym), VarCycle: pay.VarCycle, VarTotal: tot, Meal: pay.Meal, Accom: pay.Accom, TdsPct: pc.tdsPct[e.ID],
				EsiKeep: esiKeep, Structure: withMedical(s, pay.Medical), Lwf: lwf, PfExempt: pay.PfExempt, AllBasic: pay.allBasicIn(ym), Classic: pc.classic,
				EmpAmts: pay.CompAmts}
			if !pc.classic && s.Designed {
				pc.varInputs(e.ID, ym, &in)
			}
			res[i] = calc.Calc(in)
			ws[i] = pt.w
		}
		return calc.Blend(res, ws)
	}
	cyc := st.VarCycle
	if pay.VarCycle != nil {
		cyc = *pay.VarCycle
	}
	in.PrevVariable = pc.prevVariable(e, cyc, func(m string) float64 { return pc.variablePctAt(e, st, m) })
	row := calc.PayRow(in)
	out := &PayRowOut{EmployeeID: e.ID, Name: e.Name, Type: e.Type, Grade: e.Grade, Dept: e.Dept, Desig: e.Desig, City: pc.cities[e.CityID], CityID: e.CityID,
		Location: e.Location, LocID: e.LocID, DOJ: e.DOJ, Status: e.Status, Structure: pc.structNames[sid], Blended: len(parts) > 1,
		Counts: mo.Counts, Missing: mo.Counts["Missing punch"] + mo.Counts["No punch"], Nights: nights, Row: row}
	if e.ManagerID != nil {
		out.ManagerID = *e.ManagerID
	}
	if e.LWD != nil {
		out.LWD = *e.LWD
	}
	if o, ok := pc.ov[e.ID]; ok {
		out.Edited, out.Reason = o.PaidDays != nil, o.Reason
	}
	out.Prev = pc.prevNet[e.ID]
	if why := pc.hold[e.ID]; why != "" {
		out.Hold, out.HoldWhy = true, why
	}
	if !pc.classic && st.Designed {
		out.VarMissing = pc.varMissing(e, row.R.Comps)
	}
	out.Flags = rowFlags(out, ym)
	return out
}

// Dry-run flags (spec 15.1 #3).
var payFlags = []struct{ Key, Label string }{
	{"hold", "On hold"}, {"edited", "Paid days edited"}, {"adj", "Manual adjustment"}, {"miss", "Missing punch"}, {"var", "Net changed more than 15%"},
	{"zero", "Zero or negative net"}, {"extra", "Has arrears or claims"}, {"recov", "Has recoveries"}, {"join", "Joined this month"},
}

func rowFlags(o *PayRowOut, ym string) []string {
	f := []string{}
	add := func(ok bool, k string) {
		if ok {
			f = append(f, k)
		}
	}
	add(o.Hold, "hold")
	add(o.Edited, "edited")
	add(o.OE != 0 || o.OD != 0, "adj")
	add(o.Missing > 0 && !o.Edited, "miss")
	add(calc.MovedMore(o.Net, o.Prev), "var")
	add(o.Net <= 0, "zero")
	add(len(o.Add) > 0, "extra")
	add(len(o.Ded) > 0, "recov")
	add(o.DOJ[:7] == ym, "join")
	return f
}

func round0(v float64) int64 { return int64(math.Round(v)) }

// loadRows reads the stored rows of a month, by employee id.
func loadRows(ctx context.Context, q store.Q, ym string) ([]*PayRowOut, error) {
	rows, err := q.C("payroll_row").Find(ctx, bson.M{"ym": ym},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}}).SetProjection(bson.D{{Key: "data", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := []*PayRowOut{}
	for _, r := range rows {
		o := &PayRowOut{}
		if err := payengineJSON(r["data"], o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// nightShifts counts the days in the pay window whose punches touch the policy's night hours (Night shift
// card: night hours and the allowance a night).
func (pc *payCtx) nightShifts(e *emp, p *workPolicy) (nights int) {
	nf, ok1 := attend.Min(p.S.Shift.NightFrom)
	nt, ok2 := attend.Min(p.S.Shift.NightTo)
	if !ok1 || !ok2 || p.S.Shift.NightAllow <= 0 {
		return 0
	}
	for d := pc.from; d <= pc.to && d <= pc.md.today; d = policy.AddDays(d, 1) {
		if d < e.DOJ || (e.LWD != nil && d > *e.LWD) {
			continue
		}
		if r := pc.md.rec(e, p, d); r != nil && r.In != "" && r.Out != "" {
			a, _ := attend.Min(r.In)
			b, _ := attend.Min(r.Out)
			if touchesNight(a, b, nf, nt) {
				nights++
			}
		}
	}
	return nights
}

// touchesNight reports whether a worked span [a, b] (minutes, b past midnight when smaller)
// overlaps the night window [nf, nt].
func touchesNight(a, b, nf, nt int) bool {
	if b <= a {
		b += 1440
	}
	if nt <= nf {
		nt += 1440
	}
	for _, shift := range []int{-1440, 0, 1440} {
		if a < nt+shift && b > nf+shift {
			return true
		}
	}
	return false
}
