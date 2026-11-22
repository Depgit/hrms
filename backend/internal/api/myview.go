package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// My view (employee self-service) pieces behind the Dashboard, Attendance › Records › By date and the
// Employee directory. Every query here is scoped to the caller themself (or, for the colleague lists, to
// the caller's own department and location) whatever role view is chosen, and returns no pay, mobile,
// personal email, birth date or ID of anyone else.

// myDashboard is GET /api/me/dashboard?tab=ov|pay|att|cmp: the caller's own figures for My view.
func (s *Server) myDashboard(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	switch r.URL.Query().Get("tab") {
	case "pay":
		return s.myDashPay(ctx, c)
	case "att":
		return s.myDashAtt(ctx, c)
	case "cmp":
		return s.myDashDetails(ctx, c)
	}
	return s.myDashOverview(ctx, c)
}

// myDashOverview: the caller's open requests, this month's attendance, the payroll month, their tenure,
// what needs their attention, activity on their own record, and colleagues' birthdays and anniversaries.
func (s *Server) myDashOverview(ctx context.Context, c *Ctx) (map[string]any, error) {
	me, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"doj": 1, "exit_status": 1, "lwd": 1}))
	if err != nil {
		return nil, err
	}
	if me == nil {
		return nil, store.ErrNoRows
	}
	doj, _ := employeesTime(me["doj"])
	lwd, onNotice := employeesTime(me["lwd"])
	onNotice = onNotice && me["exit_status"] == "On notice"
	pending := myPendingCount(ctx, s.st.DB, c.EmployeeID) // the same requests as My requests (myrequests.go)
	att, _ := s.myDashAtt(ctx, c)
	det, _ := s.myDashDetails(ctx, c)

	type alert struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Page   string `json:"page"`
	}
	alerts := []alert{}
	if det != nil {
		if miss := det["missing"].([]string); len(miss) > 0 {
			alerts = append(alerts, alert{"My details", strings.Join(miss, ", ") + " not on record yet", "/me?tab=me"})
		}
	}
	if att != nil {
		if n := att["toFix"].(int); n > 0 {
			alerts = append(alerts, alert{"Attendance", myPlural(n, "day needs", "days need") + " regularization this month", "/me?tab=att"})
		}
	}
	holds, _ := myviewHolds(ctx, s.st.DB, c.EmployeeID)
	for _, h := range holds {
		alerts = append(alerts, alert{"Salary on hold", str(h["reason"]), "/me?tab=slips"})
	}
	if onNotice {
		alerts = append(alerts, alert{"Separation", "On notice. Last working day " + lwd.Format("02 Jan 2006"), "/my-exit"})
	}
	if pending > 0 {
		alerts = append(alerts, alert{"My requests", myPlural(int(pending), "request is", "requests are") + " waiting for approval", "/#my-requests"})
	}

	recent, err := myviewRecent(ctx, s.st.DB, c.EmployeeID)
	if err != nil {
		recent = []store.Row{}
	}
	cel, err := s.myCelebrations(ctx, c.EmployeeID, today())
	if err != nil {
		return nil, err
	}
	years := math.Floor(time.Since(doj).Hours()/24/365.25*10) / 10
	out := map[string]any{"pending": pending, "doj": doj.Format("2006-01-02"), "tenure": math.Max(0, years), "alerts": alerts, "recent": recent,
		"payroll": s.dashPayStatus(ctx), "celebrations": cel}
	if att != nil {
		out["present"], out["workDays"] = att["present"], att["upTo"]
	}
	return out, nil
}

// myviewHolds is the person's active salary holds, oldest first.
func myviewHolds(ctx context.Context, q store.Q, emp string) ([]store.Row, error) {
	return q.C("salary_hold").Find(ctx, bson.M{"employee_id": emp, "status": "Active"},
		options.Find().SetSort(bson.D{{Key: "at", Value: 1}, {Key: "_id", Value: 1}}))
}

// myviewRecent is the activity of the past 180 days on the person's own record and on the requests about them
// (but not an exit HR started that is not approved yet): the last 8, newest first.
func myviewRecent(ctx context.Context, q store.Q, emp string) ([]store.Row, error) {
	reqs, err := q.C("approval_request").Distinct(ctx, "_id", bson.M{"employee_id": emp, "$nor": bson.A{hrExitHiddenFilter("")}})
	if err != nil {
		return nil, err
	}
	return q.C("audit_log").Find(ctx, bson.M{"at": bson.M{"$gt": time.Now().AddDate(0, 0, -180)}, "$or": bson.A{
		bson.M{"entity": "employee", "entity_id": emp},
		bson.M{"entity": "approval_request", "entity_id": bson.M{"$in": reqs}},
	}}, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(8).SetProjection(bson.M{"_id": 0, "action": 1, "actor": 1, "at": 1}))
}

// myviewNames maps the ids of documents of a collection to their names.
func myviewNames(ctx context.Context, q store.Q, coll string, ids []any) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil // "$in" with no list is an error in MongoDB (an organisation of one has no colleagues)
	}
	rows, err := q.C(coll).Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		out[str(r["id"])] = str(r["name"])
	}
	return out, nil
}

// myviewSum adds up a money field over rows (numbers, or numbers written as text inside JSON), to the paisa as the
// numeric sum was.
func myviewSum(rows []store.Row, get func(store.Row) any) float64 {
	var sum float64
	for _, r := range rows {
		switch v := get(r).(type) {
		case float64:
			sum += v
		case int32:
			sum += float64(v)
		case int64:
			sum += float64(v)
		case string:
			f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
			sum += f
		}
	}
	return calc.Round2(sum)
}

func myPlural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// celebration is one colleague's birthday or work anniversary. Date is this year's (or next year's)
// occurrence, never the year of birth; Years is set for work anniversaries only.
type celebration struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Designation string `json:"designation"`
	Location    string `json:"location"`
	Department  string `json:"department"`
	Date        string `json:"date"`
	Years       int    `json:"years,omitempty"`
	ThisWeek    bool   `json:"thisWeek"`
}

// nextOccurrence is the first date on or after from with the month and day of d (29 February falls on
// 28 February in other years).
func nextOccurrence(d time.Time, from time.Time) time.Time {
	for y := from.Year(); ; y++ {
		day := d.Day()
		if d.Month() == time.February && day == 29 && !(y%4 == 0 && (y%100 != 0 || y%400 == 0)) {
			day = 28
		}
		o := time.Date(y, d.Month(), day, 0, 0, 0, 0, time.UTC)
		if !o.Before(from) {
			return o
		}
	}
}

// myCelebrations lists active colleagues in the caller's own department and at the caller's own location
// with a birthday or work anniversary from the Monday of this week to 30 days after on.
func (s *Server) myCelebrations(ctx context.Context, self, on string) ([]celebration, error) {
	t := policy.D(on)
	start := t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)) // Monday of this week
	weekEnd, end := start.AddDate(0, 0, 6), t.AddDate(0, 0, 30)
	out := []celebration{}
	me, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": self}, options.FindOne().SetProjection(bson.M{"department_id": 1, "location_id": 1}))
	if err != nil || me == nil {
		return out, err
	}
	rows, err := s.st.DB.C("employee").Find(ctx, bson.M{"status": "Active", "_id": bson.M{"$ne": self},
		"$or": bson.A{bson.M{"department_id": me["department_id"]}, bson.M{"location_id": me["location_id"]}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).
			SetProjection(bson.M{"name": 1, "designation_id": 1, "location_id": 1, "department_id": 1, "doj": 1}))
	if err != nil {
		return nil, err
	}
	var ids, desigIDs, locIDs, deptIDs []any
	for _, e := range rows {
		ids, desigIDs = append(ids, e["id"]), append(desigIDs, e["designation_id"])
		locIDs, deptIDs = append(locIDs, e["location_id"]), append(deptIDs, e["department_id"])
	}
	desigs, err := myviewNames(ctx, s.st.DB, "designation", desigIDs)
	if err != nil {
		return nil, err
	}
	locs, err := myviewNames(ctx, s.st.DB, "location", locIDs)
	if err != nil {
		return nil, err
	}
	depts, err := myviewNames(ctx, s.st.DB, "department", deptIDs)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return out, nil // nobody else in the department or location
	}
	births, err := s.st.DB.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"employee_id": 1, "dob": 1}))
	if err != nil {
		return nil, err
	}
	dobOf := map[string]any{}
	for _, p := range births {
		dobOf[str(p["employee_id"])] = p["dob"]
	}
	for _, e := range rows {
		name := str(e["name"])
		desig, ok1 := desigs[str(e["designation_id"])]
		loc, ok2 := locs[str(e["location_id"])]
		dept, ok3 := depts[str(e["department_id"])]
		doj, ok4 := employeesTime(e["doj"])
		if !ok1 || !ok2 || !ok3 || !ok4 {
			continue
		}
		var dob *time.Time
		if t, ok := employeesTime(dobOf[str(e["id"])]); ok {
			dob = &t
		}
		add := func(kind string, o time.Time, years int) {
			if o.After(end) {
				return
			}
			out = append(out, celebration{kind, name, desig, loc, dept, o.Format("2006-01-02"), years, !o.After(weekEnd)})
		}
		if dob != nil {
			add("Birthday", nextOccurrence(*dob, start), 0)
		}
		if o := nextOccurrence(doj, start); o.Year() > doj.Year() {
			add("Work anniversary", o, o.Year()-doj.Year())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out, nil
}

// fyOf is the financial year (April to March) holding a date: its first and last month and its label.
func fyOf(d string) (string, string, string) {
	y := policy.D(d).Year()
	if policy.D(d).Month() < time.April {
		y--
	}
	return fmt.Sprintf("%d-04", y), fmt.Sprintf("%d-03", y+1), fmt.Sprintf("FY %d-%02d", y, (y+1)%100)
}

// payDateOf is the date salary for month ym is paid, from the pay schedule's pay day.
func payDateOf(ym, payDay string) string { return calc.PayDate(ym, payDay) }

// myDashPay is the caller's own pay: the last published month, this financial year's earnings and TDS,
// the next pay date, active salary holds, and their claims and advances still open.
func (s *Server) myDashPay(ctx context.Context, c *Ctx) (map[string]any, error) {
	out := map[string]any{"last": nil}
	published, err := s.st.DB.C("payroll_month").Distinct(ctx, "ym", bson.M{"status": "Published"})
	if err != nil {
		return nil, err
	}
	if r, _ := s.st.DB.C("payroll_row").FindOne(ctx, bson.M{"employee_id": c.EmployeeID, "ym": bson.M{"$in": published}},
		options.FindOne().SetSort(bson.D{{Key: "ym", Value: -1}})); r != nil {
		net, _ := r["net"].(float64)
		out["last"] = map[string]any{"ym": r["ym"], "label": ymLabel(str(r["ym"])), "net": calc.Round2(net), "held": r["hold"]}
	}
	from, to, label := fyOf(today())
	var earned, tds float64
	var months int64
	if rows, err := s.st.DB.C("payroll_row").Find(ctx, bson.M{"employee_id": c.EmployeeID, "ym": bson.M{"$in": published, "$gte": from, "$lte": to}},
		options.Find().SetProjection(bson.M{"data.totalEarn": 1})); err == nil {
		earned = myviewSum(rows, func(r store.Row) any { d, _ := r["data"].(map[string]any); return d["totalEarn"] })
		months = int64(len(rows))
	}
	if rows, err := s.st.DB.C("tds_history").Find(ctx, bson.M{"employee_id": c.EmployeeID, "ym": bson.M{"$gte": from, "$lte": to}},
		options.Find().SetProjection(bson.M{"amount": 1})); err == nil {
		tds = myviewSum(rows, func(r store.Row) any { return r["amount"] })
	}
	out["fy"] = map[string]any{"label": label, "from": from, "to": to, "earned": calc.Round2(earned), "tds": calc.Round2(tds), "months": months}

	open := openYm(ctx, s.st.DB)
	var payDay string
	if cs, _ := s.st.DB.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"pay_day": 1})); cs != nil {
		payDay = str(cs["pay_day"])
	}
	pd := payDateOf(open, payDay)
	out["next"] = map[string]any{"ym": open, "label": ymLabel(open), "date": pd, "payDay": payDay, "passed": pd < today()}

	holds := []store.Row{}
	if active, err := myviewHolds(ctx, s.st.DB, c.EmployeeID); err == nil {
		for _, h := range active {
			since := ""
			if at, ok := h["at"].(time.Time); ok {
				since = at.Format("02 Jan 2006")
			}
			holds = append(holds, store.Row{"reason": h["reason"], "from_ym": h["from_ym"], "to_ym": h["to_ym"], "since": since})
		}
	}
	out["holds"] = holds

	// Claims and advances that are waiting for approval, or approved and not yet paid or recovered.
	items, err := myviewOpenItems(ctx, s.st.DB, c.EmployeeID, open)
	if err != nil {
		return nil, err
	}
	apprUIWaitingNames(ctx, s.st.DB, items)
	list := []map[string]any{}
	for _, it := range items {
		p, _ := it["payload"].(map[string]any)
		x := map[string]any{"id": it["id"], "status": it["status"], "created": it["created"], "waitingOn": it["waiting_on"], "amount": p["amount"]}
		if n, ok := it["waiting_name"]; ok {
			x["waitingName"] = n
		}
		if it["wf"] == "p_adv" {
			x["kind"], x["balance"] = "Salary advance", it["balance"]
			if p["opening"] == true { // loaded as an opening balance (advances.go): paid before the HRMS
				x["kind"], x["opening"] = "Salary advance (opening balance)", true
			}
		} else {
			x["kind"] = "Claim · " + str(p["claimType"])
		}
		if ym := str(it["pay_ym"]); ym != "" {
			x["payMonth"] = ym
		} else if ym := str(p["payMonth"]); ym != "" {
			x["payMonth"] = ym
		}
		list = append(list, x)
	}
	out["items"] = list
	// My uniform: the sizes on record and the uniform issued, with the recovery and the expected refund (uniform.go).
	if e, _ := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"name": 1, "type": 1, "status": 1, "doj": 1, "lwd": 1})); e != nil {
		u, err := s.uniformView(ctx, s.st.DB, c, e)
		if err != nil {
			return nil, err
		}
		out["uniform"] = map[string]any{"sizes": u["sizes"], "labels": u["labels"], "issues": u["issues"], "rule": u["rule"]}
	}
	var typ string
	if e, _ := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"type": 1})); e != nil {
		typ = str(e["type"])
	}
	out["payslips"] = calc.RulesFor(calc.EmploymentType(typ)).Docs
	return out, nil
}

// myviewOpenItems is the person's claims and advances still open, newest first (up to 20): pending, or approved
// with ledger lines (not void) from the open month on. balance is what those lines still recover (deductions),
// pay_ym the first month a line pays out in (null when none does).
func myviewOpenItems(ctx context.Context, q store.Q, emp, open string) ([]store.Row, error) {
	reqs, err := q.C("approval_request").Find(ctx, bson.M{"employee_id": emp, "wf": bson.M{"$in": bson.A{"p_claim", "p_adv"}},
		"status": bson.M{"$in": bson.A{"Pending", "Approved"}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).
			SetProjection(bson.M{"wf": 1, "status": 1, "payload": 1, "chain": 1, "step": 1, "created_at": 1}))
	if err != nil {
		return nil, err
	}
	ids := []any{}
	for _, r := range reqs {
		ids = append(ids, r["id"])
	}
	lines, err := q.C("ledger").Find(ctx, bson.M{"source_id": bson.M{"$in": ids}, "void": false},
		options.Find().SetProjection(bson.M{"source_id": 1, "kind": 1, "amount": 1, "ym": 1}))
	if err != nil {
		return nil, err
	}
	linesOf := map[string][]store.Row{}
	for _, l := range lines {
		linesOf[str(l["source_id"])] = append(linesOf[str(l["source_id"])], l)
	}
	out := []store.Row{}
	for _, r := range reqs {
		var ahead, deductions []store.Row
		var payYm any
		for _, l := range linesOf[str(r["id"])] {
			ym := str(l["ym"])
			if ym >= open {
				ahead = append(ahead, l)
				if l["kind"] == "ded" {
					deductions = append(deductions, l)
				}
			}
			if l["kind"] == "earn" && (payYm == nil || ym < payYm.(string)) {
				payYm = ym
			}
		}
		if r["status"] != "Pending" && len(ahead) == 0 {
			continue
		}
		created := ""
		if t, ok := r["created_at"].(time.Time); ok {
			created = store.DateOf(t)
		}
		out = append(out, store.Row{"id": r["id"], "wf": r["wf"], "status": r["status"], "payload": r["payload"], "chain": r["chain"], "step": r["step"],
			"created": created, "waiting_on": myrequestsWaitingOn(r), "balance": myviewSum(deductions, func(l store.Row) any { return l["amount"] }), "pay_ym": payYm})
		if len(out) == 20 {
			break
		}
	}
	return out, nil
}

// myDashAtt is the caller's own attendance this month so far: present, late, missing punches, leave.
func (s *Server) myDashAtt(ctx context.Context, c *Ctx) (map[string]any, error) {
	t := today()
	ym := t[:7]
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, []string{e.ID}, ym+"-01", policy.LastDayOf(ym))
	if err != nil {
		return nil, err
	}
	p, _ := ts.policyFor(e)
	mo := m.month(e, p, ym)
	k := mo.Counts
	toFix := 0
	for _, d := range mo.Days {
		if d.Date > t || m.regs[e.ID][d.Date] != "" {
			continue
		}
		if st := summaryKey(d.Status); st == "Missing punch" || st == "No punch" {
			toFix++
		}
	}
	pend, _ := s.st.DB.C("approval_request").Count(ctx, bson.M{"employee_id": c.EmployeeID, "status": "Pending", "wf": bson.M{"$in": bson.A{"regularize", "leave"}}})
	upTo := 0
	for _, d := range mo.Days {
		if d.Date <= t {
			upTo++
		}
	}
	return map[string]any{"ym": ym, "label": ymLabel(ym), "present": k["Present"] + k["Late"] + k["Half day"], "late": k["Late"], "halfDay": k["Half day"],
		"missing": k["Missing punch"], "noPunch": k["No punch"], "leave": k["Paid leave"] + k["Loss of pay leave"], "lopLeave": k["Loss of pay leave"],
		"weekOff": k["Week off"], "holiday": k["Holiday"], "lop": mo.LOP, "lateOver": mo.LateOver, "toFix": toFix, "pending": pend, "upTo": upTo, "policy": p.Name}, nil
}

// myDashDetails is which of the caller's own statutory and bank details are still missing (never the values).
func (s *Server) myDashDetails(ctx context.Context, c *Ctx) (map[string]any, error) {
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"type": 1}))
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, store.ErrNoRows
	}
	typ := str(e["type"])
	g, err := s.st.DB.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": c.EmployeeID})
	if err != nil {
		return nil, err
	}
	p, err := s.st.DB.C("employee_personal").FindOne(ctx, bson.M{"employee_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"dob": 1}))
	if err != nil {
		return nil, err
	}
	bank, err := s.st.DB.C("employee_bank").Exists(ctx, bson.M{"employee_id": c.EmployeeID})
	if err != nil {
		return nil, err
	}
	pan, aadhaar, uan := str(g["pan"]) != "", str(g["aadhaar"]) != "", str(g["uan"]) != ""
	dob := p["dob"] != nil
	type item struct {
		Label string `json:"label"`
		OK    bool   `json:"ok"`
		Note  string `json:"note"`
	}
	l := []item{
		{"PAN", pan, "Needed for income tax (TDS) and Form 16"},
		{"Aadhaar", aadhaar, "Needed for PF and ESI enrolment"},
		{"Bank account", bank, "Salary cannot be paid without it"},
		{"Date of birth", dob, "Needed for PF, insurance and tax"},
	}
	if typ == "Permanent" {
		l = append(l, item{"UAN", uan, "Your PF account number"})
	}
	missing := []string{}
	for _, x := range l {
		if !x.OK {
			missing = append(missing, x.Label)
		}
	}
	return map[string]any{"items": l, "missing": missing}, nil
}

// ---------- Attendance › Records › By date, as a period for one person ----------

// attendancePeriod is GET /api/attendance/day?from=&to=[&employeeId=]: one row per date for one person,
// in the columns of the single-date list. The caller's own days need no right (like the month register);
// another person's need att.records and must be in the caller's scope.
func (s *Server) attendancePeriod(r *http.Request, c *Ctx) (any, error) {
	ctx, qv := r.Context(), r.URL.Query()
	from, to := qv.Get("from"), or(qv.Get("to"), today())
	if !isDate(from) || !isDate(to) || to < from {
		return nil, bad("Choose a from and a to date")
	}
	if policy.DayDiff(from, to) > 92 {
		return nil, bad("Choose a period of up to three months")
	}
	id := or(qv.Get("employeeId"), c.EmployeeID)
	if id != c.EmployeeID {
		if err := c.Need("att.records", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	if to > today() {
		to = today()
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	counts := map[string]int{}
	if from <= to {
		m, err := loadMonth(ctx, s.st.DB, ts, []string{id}, from, to)
		if err != nil {
			return nil, err
		}
		p, _ := ts.policyFor(e)
		for d := from; d <= to; d = policy.AddDays(d, 1) {
			if d < e.DOJ || (e.LWD != nil && d > *e.LWD) {
				continue
			}
			row := attDayRow(m, e, p, d, counts)
			row["date"] = d
			rows = append(rows, row)
		}
	}
	return map[string]any{"from": from, "to": to, "employeeId": id, "rows": rows, "counts": counts}, nil
}
