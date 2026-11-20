package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Leave (spec 12) and regularization (spec 11.7) on top of the one approval engine.

var regReasons = []string{"Forgot to punch", "Biometric or device issue", "Field or site duty", "Work from home", "Roster changed", "Other"}

// takenOf lists an employee's leave requests of a type, approved and pending, as counted days.
func takenOf(ctx context.Context, q store.Q, empID, ltype string) (approved, pending []leave.Taken, err error) {
	rows, err := q.C("approval_request").Find(ctx, leaveRoutesOf(empID, ltype, "Approved", "Pending"),
		options.Find().SetProjection(bson.D{{Key: "payload.from", Value: 1}, {Key: "facts.days", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		pl, _ := r["payload"].(map[string]any)
		fa, _ := r["facts"].(map[string]any)
		t := leave.Taken{From: str(pl["from"]), Days: timectxNum(fa["days"])}
		if r["status"] == "Approved" {
			approved = append(approved, t)
		} else {
			pending = append(pending, t)
		}
	}
	return approved, pending, nil
}

// leaveRoutesOf matches an employee's leave requests of a type ("" for any type) in the given statuses.
func leaveRoutesOf(empID, ltype string, status ...string) bson.D {
	f := bson.D{{Key: "wf", Value: "leave"}, {Key: "employee_id", Value: empID}, notShort} // a short leave is no day of leave
	if ltype != "" {
		f = append(f, bson.E{Key: "payload.ltype", Value: ltype})
	}
	return append(f, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: status}}})
}

// leaveRoutesDays is the sum of the counted days (facts.days) of the matching requests.
func leaveRoutesDays(ctx context.Context, q store.Q, filter bson.D) (float64, error) {
	rows, err := q.C("approval_request").Find(ctx, filter, options.Find().SetProjection(bson.D{{Key: "facts.days", Value: 1}}))
	if err != nil {
		return 0, err
	}
	sum := 0.0
	for _, r := range rows {
		fa, _ := r["facts"].(map[string]any)
		sum += timectxNum(fa["days"])
	}
	return sum, nil
}

// leaveRoutesMonth matches a date kept as text in a JSON document when it falls in the month ym.
func leaveRoutesMonth(ym string) bson.D {
	return bson.D{{Key: "$regex", Value: "^" + regexp.QuoteMeta(ym)}}
}

func openingOf(ctx context.Context, q store.Q, empID, ltype, on string) *leave.Opening {
	r, err := q.C("leave_opening").FindOne(ctx, bson.D{{Key: "employee_id", Value: empID}, {Key: "leave_type", Value: ltype}, {Key: "at", Value: bson.D{{Key: "$lte", Value: on}}}},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}}))
	if err != nil || r == nil {
		return nil
	}
	return &leave.Opening{At: str(r["at"]), Balance: timectxNum(r["balance"])}
}

// typeAvail: the type is switched on in the plan, fits this employee and is allowed for their employment type.
func typeAvail(cfg *leave.TypeCfg, lt leaveType, e *emp) bool {
	return cfg != nil && cfg.On && lt.Status == "Active" && containsStr(cfg.Types, e.Type) && (cfg.Gender == "" || cfg.Gender == "all" || cfg.Gender == e.Gender) &&
		leaveTypeOK(e.Type, lt.Name)
}

// leaveTypeOK: the employment type's leave eligibility (none, only chosen leave types, or all) allows this type.
func leaveTypeOK(empType, ltype string) bool {
	return calc.RulesFor(calc.EmploymentType(empType)).LeaveTypeAllowed(ltype)
}

// leaveTypeRefusal is why an employment type cannot have a leave type ("" when it can).
func leaveTypeRefusal(empType, ltype string) string {
	if leaveTypeOK(empType, ltype) {
		return ""
	}
	if !calc.RulesFor(calc.EmploymentType(empType)).Leave {
		return fmt.Sprintf("%s employees have no leave (Compensation › Rules by employment type)", empType)
	}
	return fmt.Sprintf("%s is not allowed for %s employees (Compensation › Rules by employment type)", ltype, empType)
}

// leaveBalances is every type available to the employee with its balance on a date.
func (s *Server) leaveBalances(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	if id == "me" {
		id = c.EmployeeID
	}
	if id != c.EmployeeID {
		if err := c.Need("emp.directory", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	on := or(r.URL.Query().Get("on"), today())
	if !isDate(on) {
		return nil, bad("Give a date")
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	p, via := ts.policyFor(e)
	pl := ts.plan(p)
	out := []map[string]any{}
	for _, name := range ts.order {
		lt := ts.types[name]
		cfg := ts.typeCfg(p, name)
		if !typeAvail(cfg, lt, e) {
			continue
		}
		row := map[string]any{"type": name, "kind": lt.Kind, "paid": lt.Paid, "applyBy": cfg.ApplyBy, "docAfter": cfg.DocAfter, "docLabel": cfg.DocLabel,
			"rules": ruleText(*cfg, lt)}
		if lt.Kind == leave.KindBalance {
			ap, pe, err := takenOf(ctx, s.st.DB, id, name)
			if err != nil {
				return nil, err
			}
			row["balance"] = leave.Compute(*cfg, pl.LV, p.S.ProbMonths(e.Grade), e.DOJ, on, openingOf(ctx, s.st.DB, id, name, on), ap, pe)
		}
		if name == "Comp-off" { // no new comp-off is earned; what was credited stays usable until used or expired
			row["compoff"] = compoffBalance(ctx, s.st.DB, id, on)
		}
		out = append(out, row)
	}
	planName, half := "", false
	var short map[string]any
	if pl != nil {
		planName, half = pl.Name, pl.LV.Half
		if on, n, h := pl.LV.Short(); on && calc.RulesFor(calc.EmploymentType(e.Type)).Leave {
			short = map[string]any{"perMonth": n, "maxHours": h}
		}
	}
	// Optional holidays: the policy's number a year and an opening balance loaded for this year (b1_bulk.go).
	optHol := map[string]any{"perYear": p.S.OptHol, "opening": optHolOpeningOf(ctx, s.st.DB, id, on)}
	return map[string]any{"types": out, "policy": p.Name, "via": via, "plan": planName, "half": half, "scoreBlock": leaveScoreBlock(ctx, s.st.DB, e, p),
		"optHol": optHol, "short": short}, nil
}

func ruleText(c leave.TypeCfg, lt leaveType) string {
	parts := []string{}
	switch lt.Kind {
	case leave.KindBalance:
		parts = append(parts, fmt.Sprintf("%v days a year", c.Days))
	case leave.KindEvent:
		parts = append(parts, fmt.Sprintf("up to %v days an occasion", c.Days))
	}
	if !lt.Paid {
		parts = append(parts, "unpaid")
	}
	if c.Adv > 0 {
		parts = append(parts, fmt.Sprintf("%d days notice", c.Adv))
	}
	if c.AdvOnly || c.Back == 0 {
		parts = append(parts, "cannot be back-dated")
	} else {
		parts = append(parts, fmt.Sprintf("can be applied up to %d days after", c.Back))
	}
	if c.MaxReq > 0 && lt.Kind != leave.KindEvent {
		parts = append(parts, fmt.Sprintf("max %v days a request", c.MaxReq))
	}
	if c.DocAfter > 0 {
		parts = append(parts, fmt.Sprintf("%s after %v days", strings.ToLower(c.DocLabel), c.DocAfter))
	}
	return strings.Join(parts, " · ")
}

// leaveCover works out which dates of a request count, from the roster and the type's rule.
func leaveCover(ctx context.Context, q store.Q, ts *timeSet, e *emp, p *workPolicy, cfg *leave.TypeCfg, from, to string) ([]string, error) {
	dates := leave.Dates(from, to)
	m, err := loadMonth(ctx, q, ts, []string{e.ID}, from, to)
	if err != nil {
		return nil, err
	}
	off := make([]bool, len(dates))
	for i, d := range dates {
		c := m.rosterCode(e, p, d)
		off[i] = c == "WO" || c == "PH"
	}
	mode := "include"
	if cfg != nil {
		mode = cfg.Offs
	}
	return leave.Cover(dates, off, mode), nil
}

// leaveFacts computes the condition facts of a leave request: counted days, the 30-day rolling
// total of the same type and the approval group from the plan (spec 7.3).
func leaveFacts(ctx context.Context, q store.Q, empID string, p *requestPayload) (approval.Facts, error) {
	f := approval.Facts{}
	if p.Short || p.LType == leave.ShortLeaveType {
		return shortLeaveFacts(ctx, q, empID, p)
	}
	if !p.Half {
		p.HalfOf = ""
	} else if p.HalfOf != "second" {
		p.HalfOf = "first" // the first half unless the second is chosen
	}
	if !isDate(p.From) || !isDate(p.To) || p.To < p.From {
		return f, bad("Give a valid from and to date")
	}
	if p.LType == "" {
		return f, bad("Choose the leave type")
	}
	if policy.DayDiff(p.From, p.To) > 366 {
		return f, bad("A leave request can cover at most a year")
	}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return f, err
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return f, err
	}
	pol, _ := ts.policyFor(e)
	cfg := ts.typeCfg(pol, p.LType)
	cov, err := leaveCover(ctx, q, ts, e, pol, cfg, p.From, p.To)
	if err != nil {
		return f, err
	}
	p.Cov = cov
	f.Days = float64(len(cov))
	if p.Half && f.Days == 1 {
		f.Days = 0.5
	}
	if overlap, _ := q.C("approval_request").Exists(ctx, append(leaveRoutesOf(empID, "", "Pending", "Approved"),
		bson.E{Key: "payload.to", Value: bson.D{{Key: "$gte", Value: p.From}}}, bson.E{Key: "payload.from", Value: bson.D{{Key: "$lte", Value: p.To}}})); overlap {
		return f, bad("There is already a leave request on these dates")
	}
	if short, _ := q.C("approval_request").Exists(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "employee_id", Value: empID}, {Key: "payload.short", Value: true},
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Pending", "Approved"}}}}, {Key: "payload.from", Value: bson.D{{Key: "$gte", Value: p.From}, {Key: "$lte", Value: p.To}}}}); short {
		return f, bad("There is a short leave on these dates. Cancel it first, then apply for the leave")
	}
	lo := policy.AddDays(p.From, -30)
	prior, err := leaveRoutesDays(ctx, q, append(leaveRoutesOf(empID, p.LType, "Pending", "Approved"),
		bson.E{Key: "payload.to", Value: bson.D{{Key: "$gte", Value: lo}}}, bson.E{Key: "payload.from", Value: bson.D{{Key: "$lte", Value: p.To}}}))
	if err != nil {
		return f, err
	}
	f.Roll = f.Days + prior
	f.LeaveGroup = "P"
	if cfg != nil && cfg.Grp != "" {
		f.LeaveGroup = cfg.Grp
	} else if g := leaveGroups[p.LType]; g != "" {
		f.LeaveGroup = g
	}
	return f, nil
}

var hrRoles = []string{"HR Admin", "HRBP", "HRBP Corporate"}

// leaveErrors are the eligibility, balance and window rules (ltErrors + winErrors).
func leaveErrors(ctx context.Context, q store.Q, c *Ctx, empID string, p *requestPayload, days float64) []string {
	if p.Short {
		return shortLeaveErrors(ctx, q, empID, p)
	}
	er := []string{}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return []string{err.Error()}
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return []string{"Employee not found"}
	}
	pol, _ := ts.policyFor(e)
	lt, known := ts.types[p.LType]
	cfg := ts.typeCfg(pol, p.LType)
	switch {
	case e.Status != "Active":
		return []string{fmt.Sprintf("%s is %s. Leave is only for active employees", e.Name, strings.ToLower(e.Status))}
	case !known || lt.Status != "Active":
		return []string{p.LType + " is not available"}
	case cfg == nil || !cfg.On:
		return []string{p.LType + " is not part of this employee's leave plan"}
	}
	// A low discipline score this month stops every leave request, whoever raises it, unless HR Admin (or an
	// HRBP for someone in their scope) allows this one request with a reason (audited when it is raised).
	if block := leaveScoreBlock(ctx, q, e, pol); block == "" {
		p.ScoreOverride = ""
	} else if p.ScoreOverride = strings.TrimSpace(p.ScoreOverride); p.ScoreOverride == "" {
		er = append(er, block)
	} else if err := canScoreOverride(ctx, q, c, e.ID); err != nil {
		er = append(er, block+". "+err.Error())
	}
	if !containsStr(cfg.Types, e.Type) {
		er = append(er, fmt.Sprintf("%s does not apply to %s employees", p.LType, e.Type))
	} else if why := leaveTypeRefusal(e.Type, p.LType); why != "" {
		er = append(er, why)
	}
	if cfg.Gender != "" && cfg.Gender != "all" && cfg.Gender != e.Gender {
		er = append(er, fmt.Sprintf("%s is only for %s", p.LType, map[string]string{"Female": "women", "Male": "men"}[cfg.Gender]))
	}
	if mo := monthsBetween(e.DOJ[:7], p.From[:7]); cfg.MinSvc > 0 && mo < cfg.MinSvc {
		er = append(er, fmt.Sprintf("%s needs %d months of service. %s has %d", p.LType, cfg.MinSvc, e.Name, mo))
	}
	if cfg.ApplyBy == "hr" && !containsStr(hrRoles, c.Role) {
		er = append(er, p.LType+" can only be applied by HRBP or Corporate HR on behalf of the employee")
	}
	if p.From < e.DOJ {
		er = append(er, "Leave cannot start before the joining date, "+longDate(e.DOJ))
	}
	if e.LWD != nil && p.To > *e.LWD {
		er = append(er, "Leave cannot go beyond the last working day, "+longDate(*e.LWD))
	}
	if cfg.OnNotice == "restrict" {
		pendingResign, _ := q.C("approval_request").Exists(ctx, bson.D{{Key: "wf", Value: "resign"}, {Key: "employee_id", Value: e.ID}, {Key: "status", Value: "Pending"}})
		if (e.Exit != nil && *e.Exit == "On notice") || pendingResign {
			er = append(er, p.LType+" cannot be taken during the notice period")
		}
	}
	if days == 0 {
		er = append(er, "Every date in this request is a weekly off or holiday, so no "+strings.ToLower(p.LType)+" is needed")
	}
	if p.Half {
		if pl := ts.plan(pol); pl == nil || !pl.LV.Half {
			er = append(er, "Half-day leave is not allowed in this leave plan")
		} else if p.From != p.To {
			er = append(er, "A half day is one date")
		}
	}
	inProb := pol.S.InProbation(e.Grade, e.DOJ, p.From)
	switch lt.Kind {
	case leave.KindBalance:
		ap, pe, err := takenOf(ctx, q, e.ID, p.LType)
		if err != nil {
			return append(er, err.Error())
		}
		b := leave.Compute(*cfg, ts.plan(pol).LV, pol.S.ProbMonths(e.Grade), e.DOJ, p.From, openingOf(ctx, q, e.ID, p.LType, p.From), ap, pe)
		if b.InProbBlock {
			er = append(er, p.LType+" cannot be used during probation under this leave plan")
		} else if days > b.Bal+b.Neg {
			er = append(er, fmt.Sprintf("%s balance is %v day%s%s. This request needs %v. Use Loss of pay or another leave type", p.LType, b.Bal, map[bool]string{true: "", false: "s"}[b.Bal == 1],
				map[bool]string{true: fmt.Sprintf(" after %v waiting for approval", b.Pending), false: ""}[b.Pending > 0], days))
		}
	case leave.KindEvent:
		if !cfg.InProb && inProb {
			er = append(er, p.LType+" cannot be used during probation")
		}
		if cfg.Days > 0 && days > cfg.Days {
			er = append(er, fmt.Sprintf("%s is %v days at most for one occasion. This request is %v", p.LType, cfg.Days, days))
		}
		if cfg.Times > 0 {
			n64, _ := q.C("approval_request").Count(ctx, leaveRoutesOf(e.ID, p.LType, "Pending", "Approved"))
			if n := int(n64); n >= cfg.Times {
				er = append(er, fmt.Sprintf("%s can be taken %d time%s in service. %d already taken or requested", p.LType, cfg.Times, map[bool]string{true: "", false: "s"}[cfg.Times == 1], n))
			}
		}
	default:
		if !cfg.InProb && inProb {
			er = append(er, p.LType+" cannot be used during probation")
		}
		if p.LType == "Comp-off" { // only comp-off credited before it was discontinued, until it is used or expires
			if b := compoffBalance(ctx, q, e.ID, p.From); days > b {
				er = append(er, fmt.Sprintf("Comp-off balance on %s is %v day%s. This request needs %v", longDate(p.From), b, map[bool]string{true: "", false: "s"}[b == 1], days))
			}
		}
	}
	// Clubbing: casual and privilege leave cannot touch each other when the plan says so.
	if pl := ts.plan(pol); pl != nil && pl.LRX.Club == "no" && (p.LType == "Casual leave" || p.LType == "Privilege leave") {
		other := map[string]string{"Casual leave": "Privilege leave", "Privilege leave": "Casual leave"}[p.LType]
		if ok, _ := q.C("approval_request").Exists(ctx, append(leaveRoutesOf(e.ID, other, "Pending", "Approved"),
			bson.E{Key: "payload.from", Value: bson.D{{Key: "$lte", Value: policy.AddDays(p.To, 1)}}}, bson.E{Key: "payload.to", Value: bson.D{{Key: "$gte", Value: policy.AddDays(p.From, -1)}}})); ok {
			er = append(er, fmt.Sprintf("%s cannot be joined to %s under this leave plan. Leave a working day between them", p.LType, strings.ToLower(other)))
		}
	}
	if cfg.DocAfter > 0 && days > cfg.DocAfter {
		if p.Doc == "" {
			er = append(er, fmt.Sprintf("Attach %s. It is needed when %s is more than %v day%s", or(cfg.DocLabel, "the document"), p.LType, cfg.DocAfter, map[bool]string{true: "", false: "s"}[cfg.DocAfter == 1]))
		} else if ok, _ := q.C("document").Exists(ctx, bson.D{{Key: "_id", Value: p.Doc}, {Key: "$or", Value: bson.A{bson.D{{Key: "employee_id", Value: e.ID}}, bson.D{{Key: "owner_id", Value: e.ID}}}}}); !ok {
			er = append(er, "The attached document was not found")
		}
	}
	// A paid month's leave stays as paid, exception or not: a late correction is an arrear or recovery.
	if msg := paidMonthMsg(ctx, q, p.From, p.To); msg != "" {
		return append(er, msg)
	}
	if exceptionOpen(ctx, q, e.ID, "leave", p.From, p.To) {
		return er
	}
	// After Leave closure in the payroll checklist, leave on that cycle's dates is refused (payroll_closure.go).
	if msg := leaveClosedMsg(ctx, q, p.From, p.To); msg != "" {
		er = append(er, msg)
	}
	er = append(er, leave.WindowErrors(*cfg, p.LType, today(), p.From, days)...)
	if cfg.Blk {
		for _, b := range pol.S.Blackouts {
			if b.On && !(p.To < b.From || p.From > b.To) {
				er = append(er, fmt.Sprintf("%s (%s to %s) is a blackout period for planned leave. Ask Payroll or HR for an exception", b.Name, longDate(b.From), longDate(b.To)))
			}
		}
	}
	if cfg.MaxMonth > 0 {
		used, _ := leaveRoutesDays(ctx, q, append(leaveRoutesOf(e.ID, p.LType, "Pending", "Approved"), bson.E{Key: "payload.from", Value: leaveRoutesMonth(p.From[:7])}))
		if used+days > cfg.MaxMonth {
			er = append(er, fmt.Sprintf("%s is limited to %v days a month. %v already taken or requested", p.LType, cfg.MaxMonth, used))
		}
	}
	// Staffing: at most the policy's share of the team at this property can be off on the same day.
	if pl := ts.plan(pol); pl != nil && pl.LRX.StaffOn && len(p.Cov) <= 62 {
		team, mates := leaveRoutesTeam(ctx, q, e)
		if lim := int(math.Max(1, math.Floor(float64(team)*float64(pl.LRX.StaffPct)/100))); team > 1 {
			// The team's other leave, approved or waiting (anyone at the property in the department, whatever
			// their status), each counted on the dates it covers.
			reqs, _ := q.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Pending", "Approved"}}}},
				{Key: "employee_id", Value: bson.D{{Key: "$in", Value: mates}, {Key: "$ne", Value: e.ID}}}, notShort},
				options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "payload.cov", Value: 1}, {Key: "payload.from", Value: 1}, {Key: "payload.to", Value: 1}}))
			for _, d := range p.Cov {
				who := map[string]bool{}
				for _, r := range reqs {
					if leaveRoutesCovers(r, d) {
						who[str(r["employee_id"])] = true
					}
				}
				if off := len(who); off+1 > lim {
					er = append(er, fmt.Sprintf("Too many people from %s at this location are already on leave on %s (limit %d of %d). Ask the Line Manager to plan cover", e.Dept, longDate(d), lim, team))
					break
				}
			}
		}
	}
	return er
}

// leaveRoutesTeam is the employee's team at the property: how many are active in the department there, and
// everyone in it whatever their status (whose leave counts against the staffing limit).
func leaveRoutesTeam(ctx context.Context, q store.Q, e *emp) (int, []string) {
	d, err := q.C("department").FindOne(ctx, bson.D{{Key: "name", Value: e.Dept}}, options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil || d == nil {
		return 0, []string{}
	}
	rows, err := q.C("employee").Find(ctx, bson.D{{Key: "location_id", Value: e.LocID}, {Key: "department_id", Value: d["id"]}},
		options.Find().SetProjection(bson.D{{Key: "status", Value: 1}}))
	if err != nil {
		return 0, []string{}
	}
	team, mates := 0, []string{}
	for _, r := range rows {
		mates = append(mates, str(r["id"]))
		if r["status"] == "Active" {
			team++
		}
	}
	return team, mates
}

// leaveRoutesCovers is whether a leave request counts on date d: its list of counted dates has d, or, when it
// has no such list, d is between its from and to.
func leaveRoutesCovers(r store.Row, d string) bool {
	pl, _ := r["payload"].(map[string]any)
	cov, has := pl["cov"]
	if !has {
		from, ok1 := pl["from"].(string)
		to, ok2 := pl["to"].(string)
		return ok1 && ok2 && from <= d && to >= d
	}
	switch x := cov.(type) {
	case []any:
		for _, v := range x {
			if v == d {
				return true
			}
		}
	case map[string]any:
		_, ok := x[d]
		return ok
	case string:
		return x == d
	}
	return false
}

// regErrors are the regularization rules (spec 11.7).
func regErrors(ctx context.Context, q store.Q, empID string, p *requestPayload) []string {
	er := []string{}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return []string{err.Error()}
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return []string{"Employee not found"}
	}
	pol, _ := ts.policyFor(e)
	if p.Date < e.DOJ {
		return []string{"Cannot regularize a date before joining"}
	}
	if pol.S.RG.Reason && p.Reason == "" {
		er = append(er, "Choose a reason")
	} else if p.Reason != "" && !containsStr(regReasons, p.Reason) {
		er = append(er, "Choose one of the reasons: "+strings.Join(regReasons, ", "))
	}
	if p.Reason == "Roster changed" {
		if ok, _ := q.C("roster_entry").Exists(ctx, bson.D{{Key: "employee_id", Value: empID}, {Key: "date", Value: p.Date}}); !ok {
			er = append(er, "No roster change is recorded for this employee on that date. Choose the actual reason")
		}
	}
	a, okA := attend.Min(p.In)
	b, okB := attend.Min(p.Out)
	if (p.In != "" && !okA) || (p.Out != "" && !okB) {
		er = append(er, "Times must look like 09:30")
	}
	if p.In == "" && p.Out == "" {
		er = append(er, "Give the corrected in time, out time or both")
	}
	m, err := loadMonth(ctx, q, ts, []string{empID}, p.Date, p.Date)
	if okA && okB && b <= a {
		// On a shift that ends the next day (a night shift) the out time is the next morning (TIME-235), as the
		// MIS upload reads it.
		overnight := false
		if err == nil {
			if pl := m.planned(e, pol, p.Date); pl.Shift != nil && pl.Shift.End <= pl.Shift.Start {
				overnight = true
			}
		}
		if !overnight {
			er = append(er, "Out time must be after in time")
		}
	}
	if err == nil {
		if st, _ := m.status(e, pol, p.Date); st == "Present" {
			er = append(er, "The system already shows a complete record for this day")
		}
	}
	if pending, _ := q.C("approval_request").Exists(ctx, bson.D{{Key: "wf", Value: "regularize"}, {Key: "employee_id", Value: empID}, {Key: "status", Value: "Pending"},
		{Key: "payload.date", Value: p.Date}}); pending {
		er = append(er, "A regularization for this date is already waiting for approval")
	}
	if msg := paidMonthMsg(ctx, q, p.Date, p.Date); msg != "" {
		return append(er, msg)
	}
	if exceptionOpen(ctx, q, empID, "regularize", p.Date, p.Date) {
		return er
	}
	// After the payroll month close, a correction is paid as an arrear instead (spec 11.7, 15.1).
	if locked, _ := q.C("payroll_month").Exists(ctx, bson.D{{Key: "ym", Value: p.Date[:7]}, {Key: "$or", Value: shiftsLocked}}); locked {
		return append(er, fmt.Sprintf("Attendance for %s is locked by payroll. Ask the Payroll Manager for an exception", ymLabel(p.Date[:7])))
	}
	byAge, byCycle, _ := pol.S.RegDeadline(p.Date)
	if today() > byAge {
		er = append(er, fmt.Sprintf("Regularization is allowed within %d days of the date. The last day for %s was %s", pol.S.RR.Back, longDate(p.Date), longDate(byAge)))
	} else if byCycle != "" && today() > byCycle {
		er = append(er, fmt.Sprintf("The payroll cycle for %s closed on %s. Ask Payroll for an exception", policy.D(p.Date).Format("January 2006"), longDate(byCycle)))
	}
	if pol.S.RR.Max > 0 {
		n64, _ := q.C("approval_request").Count(ctx, bson.D{{Key: "wf", Value: "regularize"}, {Key: "employee_id", Value: empID}, {Key: "payload.date", Value: leaveRoutesMonth(p.Date[:7])},
			{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Pending", "Approved"}}}}})
		if n := int(n64); n >= pol.S.RR.Max {
			er = append(er, fmt.Sprintf("Only %d regularizations are allowed in a month and %d are already raised", pol.S.RR.Max, n))
		}
	}
	return er
}

// finalizeRegularize writes the corrected times onto the day (later uploads do not overwrite it).
func finalizeRegularize(ctx context.Context, tx store.Q, r *Request) (string, error) {
	var p requestPayload
	_ = json.Unmarshal(r.Payload, &p)
	// A blank time keeps the one the day has (none on a new day).
	day := bson.D{{Key: "employee_id", Value: r.EmployeeID}, {Key: "date", Value: p.Date}}
	set := bson.M{"out_date": nil, "source": "Regularized", "by_name": "Regularization " + r.ID, "at": tx.Now()}
	if p.In != "" {
		set["in_time"] = p.In
	}
	if p.Out != "" {
		set["out_time"] = p.Out
	}
	if _, err := tx.C("attendance_day").Upsert(ctx, day, set, nil); err != nil {
		return "", err
	}
	in, out := "—", "—"
	if d, _ := tx.C("attendance_day").FindOne(ctx, day); d != nil {
		in, out = or(str(d["in_time"]), "—"), or(str(d["out_time"]), "—")
	}
	return fmt.Sprintf("Attendance for %s corrected to %s to %s. Hours will count in payroll.", longDate(p.Date), in, out), nil
}

// canScoreOverride: HR Admin, or an HRBP for someone in their data scope, can let one leave request through
// a discipline-score block. Never for their own leave.
func canScoreOverride(ctx context.Context, q store.Q, c *Ctx, empID string) error {
	if empID == c.EmployeeID {
		return forbidden("Nobody allows their own leave below the discipline score")
	}
	switch c.Role {
	case "HR Admin":
		return nil
	case "HRBP", "HRBP Corporate":
		if ok, _ := c.InScope(ctx, q, empID); ok {
			return nil
		}
	}
	return forbidden("Only HR Admin, or the HRBP for this employee, can allow leave below the discipline score")
}

// noticeDaysFor is the employee's notice period today, from their work policy and grade.
func noticeDaysFor(ctx context.Context, q store.Q, empID string) (days int, inProb bool, err error) {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return 0, false, err
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return 0, false, err
	}
	p, _ := ts.policyFor(e)
	inProb = p.S.InProbation(e.Grade, e.DOJ, today())
	return p.S.NoticeDays(e.Grade, inProb), inProb, nil
}

// compoffBalance is the comp-off credit still valid on a date, less comp-off leave taken or asked for.
func compoffBalance(ctx context.Context, q store.Q, empID, on string) float64 {
	credit := 0.0
	if rows, err := q.C("compoff_credit").Find(ctx, bson.D{{Key: "employee_id", Value: empID}, {Key: "date", Value: bson.D{{Key: "$lte", Value: on}}}, {Key: "expires", Value: bson.D{{Key: "$gte", Value: on}}}},
		options.Find().SetProjection(bson.D{{Key: "days", Value: 1}})); err == nil {
		for _, r := range rows {
			credit += timectxNum(r["days"])
		}
	}
	used, _ := leaveRoutesDays(ctx, q, leaveRoutesOf(empID, "Comp-off", "Pending", "Approved"))
	return math.Max(0, credit-used)
}
