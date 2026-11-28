package api

// Short leave (owner, 20 Nov 2026): leave for part of one day, from a time to a time (at most the plan's hours, 2 by
// default), with a reason. It is a leave request (wf leave, so the leave approval flow, its hierarchy and HR's "Skip
// approval" apply) marked payload.short, with payload.ltype "Short leave", from = to = the date and payload.fromTime /
// toTime; facts.days is 0, so no balance moves. The leave plan sets how many a month (2 by default) and the most hours
// each (leave.LV.Short); beyond either it is refused. Once approved, the window is excused on the day (attend.Plan.Short):
// no late mark, early exit, short-hours penalty or late / not-in alert for it, and the day stays Present; attendance
// shows "Short leave 2h". It is never a day of leave: the month's leave, the staffing limit and the overlap checks of
// full-day leave leave it out.

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/store"
)

// notShort leaves short leave out of a leave request filter.
var notShort = bson.E{Key: "payload.short", Value: bson.D{{Key: "$ne", Value: true}}}

// shortOn lists an employee's short leaves on a date, pending and approved (except is a request id to leave out).
func shortOn(ctx context.Context, q store.Q, empID, date, except string) ([]shortSpan, error) {
	rows, err := q.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "employee_id", Value: empID}, {Key: "payload.short", Value: true},
		{Key: "payload.from", Value: date}, {Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Pending", "Approved"}}}}},
		options.Find().SetProjection(bson.D{{Key: "status", Value: 1}, {Key: "payload.fromTime", Value: 1}, {Key: "payload.toTime", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := []shortSpan{}
	for _, r := range rows {
		if sp, ok := shortSpanOf(r); ok && sp.ID != except {
			out = append(out, sp)
		}
	}
	return out, nil
}

func shortSpanOf(r store.Row) (shortSpan, bool) {
	pl, _ := r["payload"].(map[string]any)
	a, ok1 := attend.Min(str(pl["fromTime"]))
	b, ok2 := attend.Min(str(pl["toTime"]))
	return shortSpan{ID: str(r["id"]), Status: str(r["status"]), Window: attend.Window{From: a, To: b}}, ok1 && ok2 && b > a
}

// loadShorts loads the short leaves (approved and pending) of the employees in who between from and to.
func (m *monthData) loadShorts(ctx context.Context, q store.Q, who bson.E, from, to string) error {
	rows, err := q.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "payload.short", Value: true}, who,
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Approved", "Pending"}}}}, {Key: "payload.from", Value: timectxBetween(from, to)}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "status", Value: 1}, {Key: "payload.from", Value: 1},
			{Key: "payload.fromTime", Value: 1}, {Key: "payload.toTime", Value: 1}}))
	if err != nil {
		return err
	}
	for _, r := range rows {
		sp, ok := shortSpanOf(r)
		if !ok {
			continue
		}
		id, pl := str(r["employee_id"]), r["payload"].(map[string]any)
		if m.shorts[id] == nil {
			m.shorts[id] = map[string][]shortSpan{}
		}
		d := str(pl["from"])
		m.shorts[id][d] = append(m.shorts[id][d], sp)
	}
	return nil
}

// shortWindows are the approved short leave windows of an employee on a date (nil: none).
func (m *monthData) shortWindows(id, d string) []attend.Window {
	var out []attend.Window
	for _, s := range m.shorts[id][d] {
		if s.Status == "Approved" {
			out = append(out, s.Window)
		}
	}
	return out
}

// shortLeaveFacts are a short leave's facts: no days (no balance), the C (casual) approval group, and the payload
// made tidy (one date, the type name). An overlap with another leave on the date is refused here.
func shortLeaveFacts(ctx context.Context, q store.Q, empID string, p *requestPayload) (approval.Facts, error) {
	f := approval.Facts{LeaveGroup: "C"}
	p.Short, p.LType, p.Half, p.HalfOf, p.Cov, p.Doc = true, leave.ShortLeaveType, false, "", nil, ""
	if !isDate(p.From) {
		return f, bad("Pick the date of the short leave")
	}
	p.To = p.From
	a, ok1 := attend.Min(p.FromTime)
	b, ok2 := attend.Min(p.ToTime)
	if !ok1 || !ok2 {
		return f, bad("Give the from-time and the to-time, like 09:00 and 11:00")
	}
	if b <= a {
		return f, bad("The to-time must be after the from-time")
	}
	if day, _ := q.C("approval_request").Exists(ctx, append(leaveRoutesOf(empID, "", "Pending", "Approved"),
		bson.E{Key: "payload.to", Value: bson.D{{Key: "$gte", Value: p.From}}}, bson.E{Key: "payload.from", Value: bson.D{{Key: "$lte", Value: p.From}}})); day {
		return f, bad("There is already a leave request on this date")
	}
	others, err := shortOn(ctx, q, empID, p.From, "")
	if err != nil {
		return f, err
	}
	for _, o := range others {
		if a < o.To && o.From < b {
			return f, bad("Another short leave on %s covers part of %s to %s", longDate(p.From), p.FromTime, p.ToTime)
		}
	}
	return f, nil
}

// shortLeaveErrors are the short leave rules: an active employee who takes leave, a working day in their service,
// the plan's hours and monthly number, and the payroll windows of any leave.
func shortLeaveErrors(ctx context.Context, q store.Q, empID string, p *requestPayload) []string {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return []string{err.Error()}
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return []string{"Employee not found"}
	}
	if e.Status != "Active" {
		return []string{fmt.Sprintf("%s is %s. Leave is only for active employees", e.Name, strings.ToLower(e.Status))}
	}
	if !calc.RulesFor(calc.EmploymentType(e.Type)).Leave {
		return []string{fmt.Sprintf("%s employees have no leave (Compensation › Rules by employment type)", e.Type)}
	}
	pol, _ := ts.policyFor(e)
	pl := ts.plan(pol)
	if pl == nil {
		return []string{"This employee has no leave plan"}
	}
	er := []string{}
	if p.From < e.DOJ {
		er = append(er, "Leave cannot start before the joining date, "+longDate(e.DOJ))
	}
	if e.LWD != nil && p.From > *e.LWD {
		er = append(er, "Leave cannot go beyond the last working day, "+longDate(*e.LWD))
	}
	if m, err := loadMonth(ctx, q, ts, []string{e.ID}, p.From, p.From); err == nil {
		if c := m.rosterCode(e, pol, p.From); c == "WO" || c == "PH" {
			er = append(er, longDate(p.From)+" is a weekly off or holiday, so no short leave is needed")
		}
	}
	a, _ := attend.Min(p.FromTime)
	b, _ := attend.Min(p.ToTime)
	n64, _ := q.C("approval_request").Count(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "employee_id", Value: e.ID}, {Key: "payload.short", Value: true},
		{Key: "payload.from", Value: leaveRoutesMonth(p.From[:7])}, {Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Pending", "Approved"}}}}})
	er = append(er, leave.ShortErrors(pl.LV, a, b, int(n64))...)
	// A paid month stays as paid; after the leave closure or the regularization window, Payroll opens an exception.
	if msg := paidMonthMsg(ctx, q, p.From, p.From); msg != "" {
		return append(er, msg)
	}
	if exceptionOpen(ctx, q, e.ID, "leave", p.From, p.From) {
		return er
	}
	if msg := leaveClosedMsg(ctx, q, p.From, p.From); msg != "" {
		er = append(er, msg)
	}
	if byAge, _, _ := pol.S.RegDeadline(p.From); today() > byAge {
		er = append(er, fmt.Sprintf("Short leave can be applied up to %d days after the date. Ask Payroll for an exception", pol.S.RR.Back))
	}
	return er
}

// repShortLeave lists the short leaves in the dates chosen: who, when, the window, the hours, the reason and the status.
func repShortLeave(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Date", "Code", "Name", "Location", "Department", "From", "To", "Hours", "Reason", "Request", "Status"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	rows, err := q.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "payload.short", Value: true}, {Key: "employee_id", Value: repIn(repEmpIDs(list))},
		{Key: "payload.from", Value: bson.D{{Key: "$gte", Value: f.From}, {Key: "$lte", Value: f.To}}}},
		options.Find().SetSort(bson.D{{Key: "payload.from", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, r := range rows {
		e := idx[str(r["employee_id"])]
		pl, _ := r["payload"].(map[string]any)
		sp, _ := shortSpanOf(r)
		out = append(out, []any{str(pl["from"]), e.E["id"], e.E["name"], e.Loc, e.Dept, str(pl["fromTime"]), str(pl["toTime"]),
			calc.Round2(float64(sp.To-sp.From) / 60), str(pl["note"]), str(r["id"]), str(r["status"])})
	}
	return cols, out, nil
}
