package api

// Called to work on a holiday or a leave day (owner, 24 Nov 2026: "some emp … during the holiday or leave if they are
// asked to work then mark their attendance … if they come then present"; on a leave day the leave comes back).
//
// Attendance › Called to work: HR picks employees, a date that is a holiday on their calendar or a day of their
// approved leave (week offs not yet), and a reason. Right: att.records at Edit (the right HR uses to change
// attendance: MIS upload), within the caller's data scope; listing needs att.records View. Audited; refused in a
// payroll month already approved or published (paidMonthMsg) and, like an MIS upload, in a month locked by payroll
// without an exception; removable while the month is open.
//
// The engine decides from the punches (attend/called.go): the day is Present ("Called to work") when the person came
// (any punch; hours, a late arrival or a missing out do not matter, owner 24 Nov 2026). Then a leave day's leave is returned in every balance (leaveBalOf → calledReturns: 1 day, 0.5 for a half-day
// leave; the request stays approved, its history gets a line), an unpaid leave loses its loss of pay, and Earn by
// working days counts the day 1, even on a holiday. No punch leaves the holiday or the leave exactly as it was.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// callDoc is a work_call document.
type callDoc struct {
	ID, Emp, Date, Kind, ReqID, LType, Reason, By, At string
	Shift                                             string // the shift chosen on the roster ("" for a call from the Called to work tab)
	Days                                              float64
	Noted                                             bool
}

func callOfRow(r store.Row) *callDoc {
	return &callDoc{ID: str(r["id"]), Emp: str(r["employee_id"]), Date: str(r["date"]), Kind: str(r["kind"]), ReqID: str(r["leave_request_id"]),
		LType: str(r["leave_type"]), Reason: str(r["reason"]), By: str(r["by_name"]), At: timectxStamp(r["created_at"], "02 Jan 2006 15:04"),
		Shift: str(r["shift_code"]), Days: timectxNum(r["days"]), Noted: r["noted_at"] != nil}
}

// loadCalls reads the calls of the employees between from and to.
func (m *monthData) loadCalls(ctx context.Context, q store.Q, who bson.E, from, to string) error {
	rows, err := q.C(store.WorkCallColl).Find(ctx, bson.D{who, {Key: "date", Value: timectxBetween(from, to)}})
	if err != nil {
		return err
	}
	m.calls = map[string]map[string]*callDoc{}
	for _, r := range rows {
		c := callOfRow(r)
		if m.calls[c.Emp] == nil {
			m.calls[c.Emp] = map[string]*callDoc{}
		}
		m.calls[c.Emp][c.Date] = c
	}
	return nil
}

// callOf is the employee's call on the date (nil when none).
func (m *monthData) callOf(id, d string) *callDoc { return m.calls[id][d] }

// callIn: the employee is called to work on the date and the date still is a holiday or a day of their approved
// leave, inside their employment (a call on a day that changed since does nothing).
func (m *monthData) callIn(e *emp, p *workPolicy, d string) bool {
	if m.callOf(e.ID, d) == nil || d < e.DOJ || (e.LWD != nil && d > *e.LWD) {
		return false
	}
	return m.leaveOn(e, d, false) != nil || m.rosterCode(e, p, d) == "PH"
}

// callPlan is the shift a called day is measured against: the shift chosen when the call was made on the roster
// (roster_day.go), else the roster's shift of the day, else the employee's own.
func (m *monthData) callPlan(e *emp, p *workPolicy, d string) attend.Plan {
	if cd := m.callOf(e.ID, d); cd != nil && cd.Shift != "" {
		if s, r, ok := m.ts.shiftOn(cd.Shift, d); ok {
			return attend.Plan{Code: cd.Shift, Shift: &s, Rules: r, Short: m.shortWindows(e.ID, d)}
		}
	}
	if pl := m.shiftPlan(e, p, d); pl.Shift != nil {
		return pl
	}
	base := m.ts.baseShift(e)
	if s, r, ok := m.ts.shiftOn(base, d); ok {
		return attend.Plan{Code: base, Shift: &s, Rules: r, Short: m.shortWindows(e.ID, d)}
	}
	return attend.Plan{Code: base}
}

// calledWorked: the employee was called to work on the date and worked it (the one-day views; the month register
// settles it in FinalMonth the same way).
func (m *monthData) calledWorked(e *emp, p *workPolicy, d string) bool {
	if d > m.today || !m.callIn(e, p, d) {
		return false
	}
	_, ok := attend.CalledWorked(p.S, m.callPlan(e, p, d), m.rec(e, p, d))
	return ok
}

// calledReturns are the days of leave type name returned by calls to work that were worked, as negative takings on
// the leave request's first day (the leave year its days were counted in).
func calledReturns(ctx context.Context, q store.Q, ts *timeSet, e *emp, p *workPolicy, name string) ([]leave.Taken, error) {
	rows, err := q.C(store.WorkCallColl).Find(ctx, bson.M{"employee_id": e.ID, "kind": "leave", "date": bson.M{"$lte": today()}},
		options.Find().SetSort(bson.D{{Key: "date", Value: 1}}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	first, last := str(rows[0]["date"]), str(rows[len(rows)-1]["date"])
	m, err := loadMonth(ctx, q, ts, []string{e.ID}, first[:7]+"-01", policy.LastDayOf(last[:7]))
	if err != nil {
		return nil, err
	}
	months := map[string]map[string]attend.DayOut{}
	var out []leave.Taken
	for _, r := range rows {
		d := str(r["date"])
		l := m.leaveOn(e, d, false)
		if l == nil || l.Type != name {
			continue
		}
		mo, ok := months[d[:7]]
		if !ok {
			mo = map[string]attend.DayOut{}
			for _, x := range m.month(e, p, d[:7]).Days {
				mo[x.Date] = x
			}
			months[d[:7]] = mo
		}
		if x := mo[d]; x.Called && x.Returned > 0 {
			out = append(out, leave.Taken{From: l.From, Days: -x.Returned})
		}
	}
	return out, nil
}

// ---------- Routes ----------

// callList is the month's calls in the caller's scope, with what came of each.
func (s *Server) callList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.records", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ym := or(r.URL.Query().Get("ym"), today()[:7])
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.DB.C(store.WorkCallColl).Find(ctx, bson.M{"date": timectxBetween(from, to)}, options.Find().SetSort(bson.D{{Key: "date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	idl := []string{}
	for _, x := range rows {
		idl = append(idl, str(x["employee_id"]))
	}
	in, err := timectxInScope(ctx, s.st.DB, c, idl)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	emps, err := loadEmps(ctx, s.st.DB, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: idl}}}})
	if err != nil {
		return nil, err
	}
	byID := map[string]*emp{}
	keep := []string{}
	for _, e := range emps {
		if in[e.ID] {
			byID[e.ID] = e
			keep = append(keep, e.ID)
		}
	}
	m, err := loadMonth(ctx, s.st.DB, ts, keep, from, to)
	if err != nil {
		return nil, err
	}
	days := map[string]map[string]attend.DayOut{}
	paid := paidMonthMsg(ctx, s.st.DB, from, to)
	out := []map[string]any{}
	for _, x := range rows {
		cd := callOfRow(x)
		e := byID[cd.Emp]
		if e == nil {
			continue
		}
		p, _ := ts.policyFor(e)
		if days[e.ID] == nil {
			days[e.ID] = map[string]attend.DayOut{}
			for _, o := range m.month(e, p, ym).Days {
				days[e.ID][o.Date] = o
			}
		}
		o := days[e.ID][cd.Date]
		row := map[string]any{"id": cd.ID, "employeeId": e.ID, "name": e.Name, "location": e.Location, "department": e.Dept, "date": cd.Date, "kind": cd.Kind, "shift": cd.Shift,
			"leaveType": cd.LType, "days": cd.Days, "reason": cd.Reason, "by": cd.By, "at": cd.At, "status": o.Status, "tone": o.Tone, "in": o.In, "out": o.Out,
			"note": o.Note, "worked": o.Called, "returned": o.Returned, "removable": paidMonthMsg(ctx, s.st.DB, cd.Date, cd.Date) == ""}
		switch {
		case cd.Date > today():
			row["result"] = "Upcoming"
		case o.Called:
			row["result"] = "Worked"
		case !m.callIn(e, p, cd.Date):
			row["result"] = "No longer a holiday or leave day"
		default:
			row["result"] = "Not worked"
		}
		out = append(out, row)
	}
	return map[string]any{"rows": out, "ym": ym, "paid": paid}, nil
}

// callAdd calls one or more employees to work on a date.
func (s *Server) callAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.records", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeIDs []string `json:"employeeIds"`
		Date        string   `json:"date"`
		Reason      string   `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	switch {
	case len(b.EmployeeIDs) == 0 || len(b.EmployeeIDs) > 200:
		return nil, bad("Choose up to 200 employees")
	case !isDate(b.Date):
		return nil, bad("Choose the date")
	case b.Reason == "":
		return nil, bad("Give the reason they are called to work")
	case len(b.Reason) > 500:
		return nil, bad("Keep the reason under 500 characters")
	}
	ctx := r.Context()
	for _, id := range b.EmployeeIDs {
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	added := 0
	err = s.st.Tx(ctx, func(tx store.Q) error {
		added = 0
		if msg := paidMonthMsg(ctx, tx, b.Date, b.Date); msg != "" {
			return bad("%s", msg)
		}
		emps, err := loadEmps(ctx, tx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: b.EmployeeIDs}}}})
		if err != nil {
			return err
		}
		byID := map[string]*emp{}
		for _, e := range emps {
			byID[e.ID] = e
		}
		m, err := loadMonth(ctx, tx, ts, ids(emps), b.Date, b.Date)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, id := range b.EmployeeIDs {
			e := byID[id]
			if e == nil {
				return notFound("Employee " + id + " not found")
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if err := s.callInsert(ctx, tx, c, ts, m, e, b.Date, b.Reason, ""); err != nil {
				return err
			}
			added++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"added": added}, nil
}

// callInsert calls one employee to work on a date (m holds the date), with the shift chosen on the roster (shift; ""
// from the Called to work tab): the refusals of the Called to work tab, the leave request's history and the audit.
func (s *Server) callInsert(ctx context.Context, tx store.Q, c *Ctx, ts *timeSet, m *monthData, e *emp, date, reason, shift string) error {
	who := e.Name + " (" + e.ID + ")"
	switch {
	case e.Status == "Pre-boarding" || date < e.DOJ:
		return bad("%s joins on %s: they cannot be called to work before that", who, longDate(e.DOJ))
	case e.LWD != nil && date > *e.LWD:
		return bad("%s's last working day is %s", who, longDate(*e.LWD))
	case m.callOf(e.ID, date) != nil:
		return conflict("%s is already called to work on %s", who, longDate(date))
	}
	if ym := lockedMonthOf(ctx, tx, date); ym != "" && !exceptionOpen(ctx, tx, e.ID, "regularize", date, date) {
		return bad("Attendance for %s is locked by payroll. Ask the Payroll Manager for an exception for %s", ymLabel(ym), who)
	}
	p, _ := ts.policyFor(e)
	doc := bson.M{"employee_id": e.ID, "date": date, "reason": reason, "by_id": strPtr(c.EmployeeID), "by_name": c.Name, "shift_code": timectxNullIf(shift)}
	what := ""
	if l := m.leaveOn(e, date, false); l != nil {
		days := 1.0
		if l.Half {
			days = 0.5
		}
		doc["kind"], doc["leave_request_id"], doc["leave_type"], doc["days"] = "leave", l.ID, l.Type, days
		what = l.Type + " (" + l.ID + ")"
	} else {
		switch m.rosterCode(e, p, date) {
		case "PH":
			doc["kind"] = "holiday"
			what = "holiday"
			if n := ts.holidays(p, e.CityID)[date]; n != "" {
				what = "holiday, " + n
			}
		case "WO":
			return bad("%s is a week off for %s. Calling to work is for holidays and days of approved leave", longDate(date), who)
		default:
			return bad("%s is not a holiday or a day of approved leave for %s", longDate(date), who)
		}
	}
	if shift != "" {
		what += ", shift " + shift
	}
	id, err := tx.C(store.WorkCallColl).Insert(ctx, doc)
	if err != nil {
		return err
	}
	if doc["kind"] == "leave" {
		if err := history(ctx, tx, str(doc["leave_request_id"]), "", "Called to work", c.Name,
			fmt.Sprintf("Called to work on %s: %s. If they work the day, its leave is returned.", longDate(date), reason)); err != nil {
			return err
		}
	}
	return audit(ctx, tx, c, fmt.Sprintf("%s called to work on %s (%s): %s", who, longDate(date), what, reason), store.WorkCallColl, fmt.Sprint(id), nil, doc)
}

// callRemove takes a call back while the month is open.
func (s *Server) callRemove(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.records", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	err := s.st.Tx(ctx, func(tx store.Q) error {
		row, err := tx.C(store.WorkCallColl).FindOne(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		if row == nil {
			return notFound("Not found")
		}
		cd := callOfRow(row)
		if err := c.MustInScope(ctx, tx, cd.Emp); err != nil {
			return err
		}
		if msg := paidMonthMsg(ctx, tx, cd.Date, cd.Date); msg != "" {
			return bad("%s", msg)
		}
		if ym := lockedMonthOf(ctx, tx, cd.Date); ym != "" && !exceptionOpen(ctx, tx, cd.Emp, "regularize", cd.Date, cd.Date) {
			return bad("Attendance for %s is locked by payroll. Ask the Payroll Manager for an exception", ymLabel(ym))
		}
		if _, err := tx.C(store.WorkCallColl).Delete(ctx, bson.M{"_id": id}); err != nil {
			return err
		}
		if cd.Shift != "" { // a day already punched goes back from the call's shift window (roster_day.go)
			if err := callRebuild(ctx, tx, cd.Emp, cd.Date); err != nil {
				return err
			}
		}
		if cd.Kind == "leave" && cd.ReqID != "" {
			if ok, err := tx.C("approval_request").Exists(ctx, bson.M{"_id": cd.ReqID}); err != nil {
				return err
			} else if ok {
				if err := history(ctx, tx, cd.ReqID, "", "Call to work removed", c.Name, "Not called to work on "+longDate(cd.Date)+" any more."); err != nil {
					return err
				}
			}
		}
		return audit(ctx, tx, c, fmt.Sprintf("Call to work of %s on %s removed", cd.Emp, longDate(cd.Date)), store.WorkCallColl, id, row, nil)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"removed": true}, nil
}

// callNoteSweep writes the "worked" line in the leave request's history of each leave day called to work and worked
// (once: work_call.noted_at), for calls of the last 62 days.
func (s *Server) callNoteSweep(ctx context.Context, ts *timeSet, d string) (int, error) {
	q := s.st.DB
	rows, err := q.C(store.WorkCallColl).Find(ctx, bson.M{"kind": "leave", "noted_at": nil, "date": bson.M{"$lt": d, "$gte": policy.AddDays(d, -62)}},
		options.Find().SetSort(bson.D{{Key: "date", Value: 1}})) // each employee's first call opens the range loaded
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	by := map[string][]*callDoc{}
	for _, r := range rows {
		cd := callOfRow(r)
		by[cd.Emp] = append(by[cd.Emp], cd)
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	n := 0
	for _, id := range keys {
		e, err := loadEmp(ctx, q, id)
		if err != nil {
			continue // left out of the employee list (no department or place): nothing to settle
		}
		p, _ := ts.policyFor(e)
		l := by[id]
		m, err := loadMonth(ctx, q, ts, []string{id}, l[0].Date[:7]+"-01", policy.LastDayOf(d[:7]))
		if err != nil {
			return n, err
		}
		months := map[string]map[string]attend.DayOut{}
		for _, cd := range l {
			mo, ok := months[cd.Date[:7]]
			if !ok {
				mo = map[string]attend.DayOut{}
				for _, x := range m.month(e, p, cd.Date[:7]).Days {
					mo[x.Date] = x
				}
				months[cd.Date[:7]] = mo
			}
			x := mo[cd.Date]
			if !x.Called || x.Returned == 0 || cd.Date == d { // today is not over: noted from tomorrow
				continue
			}
			lv := m.leaveOn(e, cd.Date, false)
			if lv == nil {
				continue
			}
			if err := s.st.Tx(ctx, func(tx store.Q) error {
				if _, err := tx.C(store.WorkCallColl).Update(ctx, bson.M{"_id": cd.ID, "noted_at": nil}, bson.M{"$set": bson.M{"noted_at": tx.Now()}}); err != nil {
					return err
				}
				days := "1 day"
				if x.Returned != 1 {
					days = "half a day"
				}
				return history(ctx, tx, lv.ID, "", "Worked on a leave day", "System",
					fmt.Sprintf("Called to work and worked on %s (%s in to %s out): %s of %s returned. The request stays approved.", longDate(cd.Date),
						or(x.In, "—"), or(x.Out, "—"), days, lv.Type))
			}); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
