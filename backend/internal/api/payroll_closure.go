package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Payroll run checklist ("Before you run payroll", Payroll › Payroll run): five closures for the open month.
// Each lists what is still pending, split into "Not raised" (waits on the employee) and "Waiting for approval"
// (waits on the current approver). A closure is Done by itself when nothing is pending; Payroll then closes it
// (pay.run 2), with a name and a time. The dry run and every later step need all five closed (payrollStep).
// Reopening needs the Payroll Manager (pay.approve 2) or HR Admin, with a reason, and is audited.
//
// Closing Attendance locks attendance for the month (payroll_month.locked_at, as Month close did), so a
// regularization for a locked day is refused. Closing Leave refuses new leave on the cycle's days up to the close. Claims,
// deductions, advances and arrear recoveries approved after their closure is closed go into the next month
// (payLineMonth). A bank change raised after Bank verification is closed is shown flagged on the closure.
//
// There are no shortcuts: an item stops being pending only when the person it waits on acts. A day nobody raised
// clears when the employee (or someone allowed to raise for them) raises a regularization or a leave and it is
// decided; a waiting request clears when its approver decides it (Payroll verifies or rejects a bank change
// inline, as its approver). Waiting regularizations and leave can still be rejected at the cut-off, as Month
// close did, and an absence or a missing punch nobody explained can be marked as loss of pay by Payroll (markLop, a
// missing punch as a full or half day, audited), which is how leavers and people whose leave plan has no Loss of pay
// get through. Someone with no bank account on record can have the month's salary held (holdNoBank, audited): the
// item moves to On hold and stops being pending, and the hold is released once an account is on record.
// (payroll_closure_item, from the first version, is no longer used.)

var closureKeys = []string{"attendance", "leave", "bank", "claim", "deduction"}

type closureDef struct{ Title, Help string }

var closureDefs = map[string]closureDef{
	"attendance": {"Attendance closure",
		"Missing punches and no-punch working days with no regularization raised, and regularizations waiting for approval. A day clears when a regularization or a leave is raised for it and decided. Waiting regularizations can be rejected at the cut-off, as Month close did. An absence nobody explained can be marked as loss of pay (a full day), and a missing punch nobody explained as a full or a half day of loss of pay (the punched half counts as worked); each is audited. Closing it locks attendance for the month: a regularization for a locked day is refused, and a later correction is paid as an arrear."},
	"leave": {"Leave closure",
		"Absences (no punch, not regularized) with no leave applied, and leave requests waiting for approval. Attendance asks whether the person worked; Leave asks whether they were on leave. A no-punch day with neither is listed once, under Attendance, so here are the absences whose regularization was rejected: they clear when a leave is applied and decided. Waiting leave can be rejected at the cut-off, and an unexplained absence marked as loss of pay. Closing it refuses new leave for the cycle's days up to the close."},
	"bank": {"Bank verification closure",
		"People in this run with no bank account on record (they clear when the account is added or a bank change is raised and verified), and bank account changes waiting for Payroll to verify. Verify or reject them here or in Claims and changes. A salary stays on hold while its bank change waits. The salary of someone with no account can be held for this month (audited): it is calculated but left out of the bank file, listed on Salary holds, and paid in the next open month as held salary released once an account is on record. Still without one next month, they are listed again."},
	"claim": {"Reimbursement closure",
		"Reimbursement claims waiting for approval: each clears when its approver approves or rejects it. Approved after the close, a claim is paid next month."},
	"deduction": {"Deduction closure",
		"Salary deductions, advances and arrear recoveries for this month waiting for approval: each clears when its approver decides it. Approved after the close, they start next month."},
}

const (
	grpNotRaised = "Not raised"
	grpWaiting   = "Waiting for approval"
)

type closureItem struct {
	Ref        string   `json:"ref"` // "<ym>:<closure>:<item>", the key messages use
	Group      string   `json:"group"`
	EmployeeID string   `json:"employeeId"`
	Name       string   `json:"name"`
	What       string   `json:"what"`
	When       string   `json:"when,omitempty"` // the date or dates, ready to show
	Date       string   `json:"date,omitempty"`
	Amount     *float64 `json:"amount,omitempty"`
	RequestID  string   `json:"requestId,omitempty"`
	WaitsOn    string   `json:"waitsOn"`
	WaitsOnID  string   `json:"waitsOnId,omitempty"`
	WaitsAs    string   `json:"waitsAs"` // Employee, or the approver's stage
	Note       string   `json:"note,omitempty"`
	CanVerify  bool     `json:"canVerify,omitempty"` // a bank change the caller may verify or reject, as its approver
	CanLop     bool     `json:"canLop,omitempty"`    // an unexplained absence or missing punch Payroll may mark as loss of pay at the cut-off
	LopHalf    bool     `json:"lopHalf,omitempty"`   // a missing punch: marked as a full or a half day of loss of pay (an absence is always a full day)
	CanHold    bool     `json:"canHold,omitempty"`   // no bank account on record: Payroll may hold the salary for the month
	HoldID     string   `json:"holdId,omitempty"`    // on hold: the salary hold that holds the month
	Messaged   string   `json:"messaged,omitempty"`  // "Anil Kapoor, 09 Nov 14:05"
	Late       bool     `json:"late,omitempty"`      // appeared after the closure was closed
	Text       string   `json:"text,omitempty"`      // the default message about this item alone
	closure    string
	key        string
	phrase     string
	stage      string
	inScope    bool
	held       bool // no bank account, but the month's salary is on hold: listed under On hold, not pending
}

type closureMsg struct {
	To     string   `json:"to"`
	ToName string   `json:"toName"`
	As     string   `json:"as"`
	Refs   []string `json:"refs"`
	Text   string   `json:"text"`
}

type closureState struct {
	Key       string         `json:"key"`
	Title     string         `json:"title"`
	Help      string         `json:"help"`
	Status    string         `json:"status"` // Done | Pending | Closed
	Pending   int            `json:"pending"`
	NotRaised int            `json:"notRaised"`
	Waiting   int            `json:"waiting"`
	Hidden    int            `json:"hidden"` // pending items about people outside the caller's access
	Late      int            `json:"late"`
	ClosedAt  string         `json:"closedAt,omitempty"`
	ClosedBy  string         `json:"closedBy,omitempty"`
	Reopened  map[string]any `json:"reopened,omitempty"`
	Items     []*closureItem `json:"items"`
	Held      int            `json:"held"`   // bank: people with no bank account whose salary for the month is on hold
	OnHold    []*closureItem `json:"onHold"` // bank: those of them in the caller's access (not pending)
	UanN      int            `json:"uanN"`   // bank: people who pay PF with no UAN on record (held at the calculation; not pending)
	Uan       []*closureItem `json:"uan"`    // bank: those of them in the caller's access
	Messages  []closureMsg   `json:"messages"`
	Desk      []closureMsg   `json:"desk"` // attendance: a summary to the attendance desk (MIS)
	closed    bool
	all       []*closureItem // every item still open, in scope or not
}

func shortDay(d string) string { return policy.D(d).Format("2 Jan") }

func daysText(n float64) string {
	switch {
	case n == 0.5:
		return "half a day"
	case n == 1:
		return "1 day"
	}
	return fmt.Sprintf("%v days", n)
}

// payBy is the date a message asks for: the end of the cycle while it is ahead, else two days from today.
func payBy(to string) string {
	if to > today() {
		return shortDay(to)
	}
	return shortDay(policy.AddDays(today(), 2))
}

func missingText(d attend.DayOut) string {
	switch {
	case d.In != "" && d.Out == "":
		return "Missing punch: in " + d.In + ", no out time"
	case d.In == "" && d.Out != "":
		return "Missing punch: out " + d.Out + ", no in time"
	}
	return "Missing punch"
}

// closuresOpen names the closures of month ym that are not closed.
func closuresOpen(ctx context.Context, q store.Q, ym string) []string {
	done := map[string]bool{}
	if rows, err := q.C("payroll_closure").Find(ctx, bson.M{"ym": ym, "closed_at": bson.M{"$ne": nil}}); err == nil {
		for _, r := range rows {
			done[str(r["closure"])] = true
		}
	}
	out := []string{}
	for _, k := range closureKeys {
		if !done[k] {
			out = append(out, closureDefs[k].Title)
		}
	}
	return out
}

// closureGate refuses processing salary (dry run and every later step) while a closure is open.
func closureGate(ctx context.Context, q store.Q, ym string) error {
	if open := closuresOpen(ctx, q, ym); len(open) > 0 {
		return bad("Salary for %s cannot be processed until all five closures in the checklist are closed. Still open: %s", ymLabel(ym), strings.Join(open, ", "))
	}
	return nil
}

// payLineMonth is the month a pay line approved now goes into: the open month, or the next one once the
// open month's closure (claim: Reimbursement, deduction: Deduction) is closed.
func payLineMonth(ctx context.Context, q store.Q, closure, open string) string {
	if ok, _ := q.C("payroll_closure").Exists(ctx, bson.M{"ym": open, "closure": closure, "closed_at": bson.M{"$ne": nil}}); ok {
		return nextYm(open)
	}
	return open
}

// leaveClosedMsg refuses leave on the dates of a month whose Leave closure is closed ("" when none is): the cycle's
// days up to the day it was closed. Payroll may close before the cycle ends (salary paid on the last working day);
// leave planned for the days after that is still allowed, and a change it makes is paid as an arrear.
func leaveClosedMsg(ctx context.Context, q store.Q, from, to string) string {
	rows, err := q.C("payroll_closure").Find(ctx, bson.M{"closure": "leave", "closed_at": bson.M{"$ne": nil}},
		options.Find().SetSort(bson.D{{Key: "ym", Value: 1}}))
	if err != nil {
		return ""
	}
	type cl struct{ ym, on string }
	list := []cl{}
	for _, r := range rows {
		x := cl{ym: str(r["ym"])}
		if t, ok := r["closed_at"].(time.Time); ok {
			x.on = store.DateOf(t)
		}
		list = append(list, x)
	}
	cut := cutoffSetting(ctx, q)
	for _, x := range list {
		f, t := window(x.ym, cut)
		if x.on < t {
			t = x.on
		}
		if from <= t && to >= f {
			return fmt.Sprintf("Leave for %s is closed by Payroll (%s to %s). Ask the Payroll Manager to reopen it, or choose later dates", ymLabel(x.ym), longDate(f), longDate(t))
		}
	}
	return ""
}

// checklist loads the five closures of month ym; with items, also what is pending in each, as the caller
// (c) may see it. Items about people outside c's access are counted, not listed; a nil c sees everything.
func (s *Server) checklist(ctx context.Context, q store.Q, c *Ctx, ym string, withItems bool) (map[string]*closureState, []*closureState, error) {
	byKey := map[string]*closureState{}
	order := []*closureState{}
	for _, k := range closureKeys {
		d := closureDefs[k]
		cs := &closureState{Key: k, Title: d.Title, Help: d.Help, Items: []*closureItem{}, OnHold: []*closureItem{}, Uan: []*closureItem{}, Messages: []closureMsg{}, Desk: []closureMsg{}}
		byKey[k] = cs
		order = append(order, cs)
	}
	rows, err := q.C("payroll_closure").Find(ctx, bson.M{"ym": ym})
	if err != nil {
		return nil, nil, err
	}
	stamp := func(v any) string { s, _ := payengineStamp(v, "02 Jan 2006 15:04").(string); return s }
	for _, r := range rows {
		cs := byKey[str(r["closure"])]
		if cs == nil {
			continue
		}
		cs.closed = r["closed_at"] != nil
		if cs.closed {
			cs.ClosedAt, cs.ClosedBy = stamp(r["closed_at"]), str(r["closed_by"])
		}
		if at := stamp(r["reopened_at"]); at != "" {
			cs.Reopened = map[string]any{"at": at, "by": str(r["reopened_by"]), "reason": str(r["reopen_reason"])}
		}
	}
	if withItems {
		if err := s.closureItems(ctx, q, c, ym, byKey); err != nil {
			return nil, nil, err
		}
	}
	for _, cs := range order {
		switch {
		case cs.closed:
			cs.Status = "Closed"
		case cs.Pending == 0:
			cs.Status = "Done"
		default:
			cs.Status = "Pending"
		}
	}
	return byKey, order, nil
}

// describeRequest is what a waiting request is, its dates or amount, and how a message names it.
func describeRequest(req *Request, name string) (what, when string, amt *float64, phrase string) {
	p := payloadOf(req)
	money := func(v float64) *float64 { x := math.Abs(v); return &x }
	switch req.WF {
	case "regularize":
		times := []string{}
		if p.In != "" {
			times = append(times, "in "+p.In)
		}
		if p.Out != "" {
			times = append(times, "out "+p.Out)
		}
		what = "Regularization, " + strings.ToLower(p.RType)
		if len(times) > 0 {
			what += ": " + strings.Join(times, ", ")
		}
		when, phrase = longDate(p.Date), fmt.Sprintf("%s (%s, regularization for %s)", req.ID, name, shortDay(p.Date))
	case "leave":
		what = fmt.Sprintf("%s, %s", p.LType, daysText(req.Facts.Days))
		when = longDate(p.From)
		if p.To != p.From {
			when = shortDay(p.From) + " to " + longDate(p.To)
		}
		phrase = fmt.Sprintf("%s (%s, %s leave)", req.ID, name, daysText(req.Facts.Days))
	case "p_change":
		what, phrase = "New bank account: "+p.BankName, fmt.Sprintf("%s (%s, bank account change)", req.ID, name)
	case "p_claim":
		amt, what, when = money(p.Amount), "Reimbursement claim: "+p.ClaimType, longDate(p.BillDate)
		phrase = fmt.Sprintf("%s (%s, %s claim)", req.ID, name, calc.Inr(p.Amount))
	case "p_ded":
		amt, what = money(p.Amount), "Deduction: "+p.DedType
		if p.Instalments > 1 {
			what += fmt.Sprintf(", in %d instalments", p.Instalments)
		}
		when, phrase = ymLabel(p.StartYm), fmt.Sprintf("%s (%s, %s deduction)", req.ID, name, calc.Inr(p.Amount))
	case "p_adv":
		amt, what = money(p.Amount), fmt.Sprintf("Salary advance, recovered in %d instalment%s", max(1, p.Instalments), map[bool]string{true: "", false: "s"}[p.Instalments <= 1])
		when, phrase = ymLabel(p.PayMonth), fmt.Sprintf("%s (%s, %s advance)", req.ID, name, calc.Inr(p.Amount))
	case "p_arrear":
		amt, what = money(p.Amount), "Arrear recovery"
		if p.SlipText != "" {
			what += ": " + p.SlipText
		}
		when, phrase = ymLabel(p.PayMonth), fmt.Sprintf("%s (%s, %s arrear recovery)", req.ID, name, calc.Inr(math.Abs(p.Amount)))
	}
	return
}

func (s *Server) closureItems(ctx context.Context, q store.Q, c *Ctx, ym string, byKey map[string]*closureState) error {
	from, to := window(ym, cutoffSetting(ctx, q))
	eligible, err := payEligible(ctx, q, ym)
	if err != nil {
		return err
	}
	list, err := loadEmps(ctx, q, eligible)
	if err != nil {
		return err
	}
	inRun := map[string]bool{}
	for _, e := range list {
		inRun[e.ID] = true
	}
	ids := ids(list)
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return err
	}
	md, err := loadMonth(ctx, q, ts, ids, from, to)
	if err != nil {
		return err
	}
	// The regularization raised for each day of the cycle, by status (pending first, then approved, rejected).
	regSt, regID := map[string]string{}, map[string]string{}
	rank := map[string]int{"Pending": 4, "Approved": 3, "Rejected": 2, "Cancelled": 1}
	regs, err := q.C("approval_request").Find(ctx, bson.M{"wf": "regularize", "employee_id": bson.M{"$in": ids}, "payload.date": bson.M{"$gte": from, "$lte": to}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}).
			SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "payload.date", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return err
	}
	for _, r := range regs {
		d, _ := r["payload"].(map[string]any)
		if k, st := str(r["employee_id"])+"/"+str(d["date"]), str(r["status"]); rank[st] >= rank[regSt[k]] {
			regSt[k], regID[k] = st, str(r["id"])
		}
	}
	add := func(it *closureItem) {
		it.Ref = ym + ":" + it.closure + ":" + it.key
		byKey[it.closure].all = append(byKey[it.closure].all, it)
	}

	// Days: missing punches and absences with nothing raised for them.
	for _, e := range list {
		p, _ := ts.policyFor(e)
		mo := md.span(e, p, from, to)
		for _, d := range mo.Days {
			if d.Code != "M" && d.Code != "N" && d.Code != "A" {
				continue
			}
			if md.leaveOn(e, d.Date, true) != nil { // a leave is raised for the day: it waits under Leave closure
				continue
			}
			k := e.ID + "/" + d.Date
			st, absent := regSt[k], d.Code != "M"
			if st == "Pending" || st == "Approved" || (st == "Rejected" && !absent) {
				continue // waiting under Attendance closure, or decided
			}
			it := &closureItem{Group: grpNotRaised, EmployeeID: e.ID, Name: e.Name, When: longDate(d.Date), Date: d.Date, WaitsOn: e.Name, WaitsOnID: e.ID, WaitsAs: "Employee", key: k,
				CanLop: true, LopHalf: !absent}
			switch {
			case st == "Rejected":
				it.closure, it.What, it.phrase = "leave", "Absent: regularization "+regID[k]+" was rejected, and no leave is applied", shortDay(d.Date)
			case absent:
				it.closure, it.What, it.phrase = "attendance", "No punch on a working day", shortDay(d.Date)+" (no punch)"
				it.Note = "Also an absence with no leave applied. Listed once, here: a regularization if they worked, or a leave if they were off, clears it."
			default:
				it.closure, it.What, it.phrase = "attendance", missingText(d), shortDay(d.Date)+" (missing punch)"
			}
			add(it)
		}
	}
	// People in the run with no bank account on record (and none waiting to be verified), by employee ID.
	banked, err := q.C("employee_bank").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}, "from_month": bson.M{"$lte": ym}})
	if err != nil {
		return err
	}
	changing, err := q.C("approval_request").Distinct(ctx, "employee_id", bson.M{"wf": "p_change", "status": "Pending", "payload.kind": "Bank",
		"employee_id": bson.M{"$in": ids}})
	if err != nil {
		return err
	}
	hasBank := map[string]bool{}
	for _, id := range append(banked, changing...) {
		hasBank[str(id)] = true
	}
	// Their salary holds: a hold that covers this month takes the person off the pending list (On hold); a hold
	// placed here for an earlier month can be held again for this one.
	holdRows, err := q.C("salary_hold").Find(ctx, bson.M{"status": "Active", "employee_id": bson.M{"$in": ids}})
	if err != nil {
		return err
	}
	holdOf := map[string]store.Row{}
	for _, h := range holdRows {
		holdOf[str(h["employee_id"])] = h
	}
	for _, e := range list {
		if hasBank[e.ID] {
			continue
		}
		it := &closureItem{closure: "bank", key: e.ID + "/bank", Group: grpNotRaised, EmployeeID: e.ID, Name: e.Name, What: "No bank account on record",
			WaitsOn: e.Name, WaitsOnID: e.ID, WaitsAs: "Employee", CanHold: true}
		if h := holdOf[e.ID]; h != nil {
			hid, from, through := str(h["id"]), str(h["from_ym"]), str(h["to_ym"])
			switch {
			case holdCovers(from, through, ym):
				it.held, it.CanHold, it.HoldID, it.When = true, false, hid, holdMonths(from, through)
				it.Note = str(h["reason"])
				if n := strings.TrimSpace(str(h["note"])); n != "" && n != it.Note {
					it.Note += ": " + n
				}
				if at, _ := payengineStamp(h["at"], "02 Jan 2006").(string); at != "" {
					it.Note += " (" + str(h["by_name"]) + ", " + at + ")"
				}
			case str(h["source_id"]) == noBankHoldSource && through != "" && through < ym:
				it.HoldID = hid
				it.Note = fmt.Sprintf("Salary for %s is on hold (%s). Holding it again adds %s to the same hold.", holdMonths(from, through), hid, ymLabel(ym))
			default: // a hold that starts in a later month
				it.CanHold, it.HoldID = false, hid
				it.Note = fmt.Sprintf("%s holds the salary from %s. To hold %s as well, release it in Salary holds and hold it here.", hid, ymLabel(from), ymLabel(ym))
			}
		}
		add(it)
	}
	// Requests waiting for approval.
	// regularizations and leave in the cycle, bank changes, claims, deductions, advances and arrear recoveries
	reqs, err := q.C("approval_request").Find(ctx, bson.M{"status": "Pending", "$or": bson.A{
		bson.M{"wf": "regularize", "payload.date": bson.M{"$gte": from, "$lte": to}},
		bson.M{"wf": "leave", "payload.from": bson.M{"$lte": to}, "payload.to": bson.M{"$gte": from}},
		bson.M{"wf": "p_change", "payload.kind": "Bank"},
		bson.M{"wf": bson.M{"$in": bson.A{"p_claim", "p_ded", "p_adv"}}},
		bson.M{"wf": "p_arrear", "payload.amount": bson.M{"$lt": 0}},
	}}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return err
	}
	for _, row := range reqs {
		req, err := loadRequest(ctx, q, str(row["id"]), false)
		if err != nil || req.Step >= len(req.Chain) {
			continue
		}
		p := payloadOf(req)
		it := &closureItem{Group: grpWaiting, EmployeeID: req.EmployeeID, RequestID: req.ID, key: req.ID}
		switch req.WF {
		case "regularize":
			it.closure = "attendance"
		case "leave":
			it.closure = "leave"
		case "p_change":
			it.closure = "bank"
		case "p_claim":
			it.closure = "claim"
		case "p_ded":
			it.closure = "deduction"
			if p.StartYm != "" && p.StartYm > ym {
				continue
			}
		case "p_adv", "p_arrear":
			it.closure = "deduction"
			if p.PayMonth != "" && p.PayMonth > ym {
				continue
			}
		}
		if req.WF == "p_change" && !inRun[req.EmployeeID] {
			continue
		}
		sub, err := loadSubject(ctx, q, req.EmployeeID)
		if err != nil {
			continue
		}
		it.Name = sub.Name
		it.What, it.When, it.Amount, it.phrase = describeRequest(req, sub.Name)
		it.stage = req.Chain[req.Step]
		it.WaitsAs = it.stage
		if per := resolvePerson(ctx, q, it.stage, sub, req.Facts); per.ID != "" {
			it.WaitsOn, it.WaitsOnID = per.Name, per.ID
		} else {
			it.WaitsOn = "No one in place (" + it.stage + ")"
		}
		if it.closure == "bank" && c != nil {
			it.CanVerify = canAct(ctx, q, c, req)
		}
		add(it)
	}

	// PF applies and no UAN is on record: listed under Bank verification, not pending (pfuan.go).
	uanItems, err := uanChecklist(ctx, q, ym, list)
	if err != nil {
		return err
	}

	// The caller's access.
	scoped := map[string]bool{}
	who := []string{}
	for _, cs := range byKey {
		for _, it := range cs.all {
			who = append(who, it.EmployeeID)
		}
	}
	for _, it := range uanItems {
		who = append(who, it.EmployeeID)
	}
	if c == nil || (c.Scope.Access == "all" && len(c.Scope.Departments) == 0) {
		for _, id := range who {
			scoped[id] = true
		}
	} else if len(who) > 0 {
		scope, err := c.ScopeFilter(ctx, q, "")
		if err != nil {
			return err
		}
		// (Every employee has a department, so the old JOIN department d kept them all.)
		in, err := q.C("employee").Distinct(ctx, "_id", payengineAnd(bson.M{"_id": bson.M{"$in": who}}, scope))
		if err != nil {
			return err
		}
		for _, id := range in {
			scoped[str(id)] = true
		}
	}

	for _, it := range uanItems {
		bank := byKey["bank"]
		bank.UanN++
		if scoped[it.EmployeeID] {
			bank.Uan = append(bank.Uan, it)
		}
	}

	// Who was messaged about each item, and the messages whose items are resolved (done).
	messaged := map[string]string{}
	open := map[string]string{} // ref -> whom it waits on, for every item still open
	by := payBy(to)
	for _, k := range closureKeys {
		cs := byKey[k]
		kept := []*closureItem{}
		for _, it := range cs.all {
			if cs.closed && (k == "claim" || k == "deduction") {
				continue // approved from now on, it goes into the next month
			}
			it.inScope = scoped[it.EmployeeID]
			if it.held { // the month's salary is on hold: not pending, and messages about it are done
				cs.Held++
				if it.inScope {
					cs.OnHold = append(cs.OnHold, it)
				}
				continue
			}
			it.Late = cs.closed
			if it.WaitsOnID != "" {
				it.Text = closureNotice(ym, k, it.Group, []*closureItem{it}, by)
			}
			open[it.Ref] = it.WaitsOnID
			kept = append(kept, it)
			switch {
			case it.Late:
				cs.Late++
			default:
				cs.Pending++
				if it.Group == grpNotRaised {
					cs.NotRaised++
				} else {
					cs.Waiting++
				}
			}
			if !it.inScope {
				if !it.Late {
					cs.Hidden++
				}
				continue
			}
			cs.Items = append(cs.Items, it)
		}
		cs.all = kept
		sort.SliceStable(cs.Items, func(i, j int) bool {
			a, b := cs.Items[i], cs.Items[j]
			if a.Group != b.Group {
				return a.Group == grpNotRaised
			}
			if a.EmployeeID != b.EmployeeID && a.Group == grpNotRaised {
				return a.EmployeeID < b.EmployeeID
			}
			return a.Date < b.Date
		})
	}
	// The payroll messages about this month's items (a ref "<ym>:…"), oldest first, with the recipient's name.
	nrows, err := q.C("notice").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"kind": bson.M{"$regex": "^payroll"}, "ref": bson.M{"$regex": "^" + regexp.QuoteMeta(ym+":")}}},
		bson.M{"$lookup": bson.M{"from": "employee", "localField": "to_employee_id", "foreignField": "_id", "as": "e"}},
		bson.M{"$unwind": "$e"},
		bson.M{"$sort": bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}},
		bson.M{"$project": bson.M{"kind": 1, "to_employee_id": 1, "ref": 1, "done_at": 1, "created_at": 1, "name": "$e.name"}},
	})
	if err != nil {
		return err
	}
	var resolved []int64
	for _, n := range nrows {
		id, _ := n["id"].(int64)
		kind, to, name := str(n["kind"]), str(n["to_employee_id"]), str(n["name"])
		refs, done := payengineStrings(n["ref"]), n["done_at"] != nil
		at, _ := payengineStamp(n["created_at"], "02 Jan 15:04").(string)
		still := false
		for _, r := range refs {
			if w, ok := open[r]; ok && (kind != "payroll" || w == to) {
				still = true
			}
			if kind == "payroll" {
				messaged[r] = name + ", " + at
			}
		}
		if !done && !still {
			resolved = append(resolved, id)
		}
	}
	if len(resolved) > 0 {
		if _, err := q.C("notice").UpdateMany(ctx, bson.M{"_id": bson.M{"$in": resolved}, "done_at": nil}, bson.M{"$set": bson.M{"done_at": q.Now()}}); err != nil {
			return err
		}
	}
	desk, _ := attendanceDesk(ctx, q)
	for _, k := range closureKeys {
		cs := byKey[k]
		type grp struct {
			to, group string
			its       []*closureItem
		}
		groups, idx := []*grp{}, map[string]*grp{}
		for _, it := range cs.Items {
			it.Messaged = messaged[it.Ref]
			if it.WaitsOnID == "" {
				continue
			}
			g := idx[it.WaitsOnID+"|"+it.Group]
			if g == nil {
				g = &grp{to: it.WaitsOnID, group: it.Group}
				idx[it.WaitsOnID+"|"+it.Group] = g
				groups = append(groups, g)
			}
			g.its = append(g.its, it)
		}
		for _, g := range groups {
			refs := []string{}
			for _, it := range g.its {
				refs = append(refs, it.Ref)
			}
			as := "Employee"
			if g.group == grpWaiting {
				as = g.its[0].stage
			}
			cs.Messages = append(cs.Messages, closureMsg{To: g.to, ToName: g.its[0].WaitsOn, As: as, Refs: refs, Text: closureNotice(ym, k, g.group, g.its, by)})
		}
		if k == "attendance" && len(cs.Items) > 0 {
			refs := []string{}
			nr, wt := 0, 0
			for _, it := range cs.Items {
				refs = append(refs, it.Ref)
				if it.Group == grpNotRaised {
					nr++
				} else {
					wt++
				}
			}
			for _, d := range desk {
				cs.Desk = append(cs.Desk, closureMsg{To: d.ID, ToName: d.Name, As: "MIS Executive", Refs: refs,
					Text: fmt.Sprintf("Payroll for %s: %d attendance items are still pending (%d not raised, %d waiting for approval). Please follow them up by %s.", ymLabel(ym), len(refs), nr, wt, by)})
			}
		}
	}
	return nil
}

// attendanceDesk is the people mapped to MIS Executive: Month close's "send to MIS" goes to them (an active
// mapping to the role while the role is active, people still employed), by employee ID.
func attendanceDesk(ctx context.Context, q store.Q) ([]Person, error) {
	out := []Person{}
	const role = "MIS Executive"
	if on, err := q.C("role").Exists(ctx, bson.M{"name": role, "active": true}); err != nil || !on {
		return out, err
	}
	mapped, err := q.C("user_role_map").Distinct(ctx, "employee_id", bson.M{"role_name": role, "active": true})
	if err != nil {
		return out, err
	}
	rows, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": mapped}, "status": bson.M{"$ne": "Inactive"}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "name", Value: 1}}))
	for _, r := range rows {
		out = append(out, Person{ID: str(r["id"]), Name: str(r["name"])})
	}
	return out, err
}

// closureNotice is the default message about items that wait on one person.
func closureNotice(ym, closure, group string, its []*closureItem, by string) string {
	mon := ymLabel(ym)
	list := func() string {
		l := []string{}
		for i, it := range its {
			if i == 5 {
				l = append(l, fmt.Sprintf("and %d more", len(its)-5))
				break
			}
			l = append(l, it.phrase)
		}
		return strings.Join(l, ", ")
	}
	if group == grpWaiting {
		if len(its) == 1 {
			return fmt.Sprintf("Payroll for %s is waiting on your approval of %s. Please decide by %s.", mon, its[0].phrase, by)
		}
		return fmt.Sprintf("Payroll for %s is waiting on your approval of %d requests: %s. Please decide by %s.", mon, len(its), list(), by)
	}
	switch closure {
	case "attendance":
		if len(its) == 1 {
			what := "a missing punch"
			if strings.HasPrefix(its[0].What, "No punch") {
				what = "no punch"
			}
			return fmt.Sprintf("Payroll for %s: your attendance on %s shows %s. Please raise a regularization if you worked, or apply for leave, by %s.", mon, shortDay(its[0].Date), what, by)
		}
		return fmt.Sprintf("Payroll for %s: your attendance shows %d days to correct: %s. Please raise a regularization for the days you worked, or apply for leave, by %s.", mon, len(its), list(), by)
	case "leave":
		if len(its) == 1 {
			return fmt.Sprintf("Payroll for %s: you were absent on %s with no leave applied. Please apply for leave by %s, or the day is paid as loss of pay.", mon, its[0].phrase, by)
		}
		return fmt.Sprintf("Payroll for %s: you were absent on %d days with no leave applied: %s. Please apply for leave by %s, or the days are paid as loss of pay.", mon, len(its), list(), by)
	}
	return fmt.Sprintf("Payroll for %s: there is no bank account on record for you. Please add it in My pay and profile, My details, by %s, so your salary can be paid.", mon, by)
}

// noticeLink is the page a message opens, and the role view it belongs to.
func noticeLink(closure, group string, its []*closureItem) (string, string) {
	if group == grpWaiting {
		if closure == "bank" {
			return "/payroll?tab=claims&sub=changes", "Payroll"
		}
		view := approval.StageRole(its[0].stage)
		if len(its) == 1 {
			return "/approvals?id=" + its[0].RequestID, view
		}
		return "/approvals", view
	}
	switch closure {
	case "attendance":
		return "/me?tab=att", "Employee"
	case "leave":
		return "/me?tab=leave", "Employee"
	}
	return "/me?tab=me", "Employee"
}

// ---------- Handlers ----------

// checklistGet: the month (as the run needs it) and its five closures with what is pending.
func (s *Server) checklistGet(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := loadPayMonth(ctx, s.st.DB, ym, false)
	if err != nil {
		return nil, err
	}
	m.Stale = payStale(ctx, s.st.DB, m)
	open := openYm(ctx, s.st.DB)
	cut := cutoffSetting(ctx, s.st.DB)
	from, to := window(ym, cut)
	_, order, err := s.checklist(ctx, s.st.DB, c, ym, ym == open && (m.Status == "Open" || m.Status == "Dry run"))
	if err != nil {
		return nil, err
	}
	still := []string{}
	for _, cs := range order {
		if !cs.closed {
			still = append(still, cs.Title)
		}
	}
	warn, err := setupWarnings(ctx, s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ym": ym, "month": m, "from": from, "to": to, "cutoff": cut, "by": payBy(to), "cycleEnded": to < today(), "current": ym == open,
		"closures": order, "allClosed": len(still) == 0, "open": still, "warnings": warn,
		"canClose": c.Level("pay.run") >= 2, "canReopen": c.Level("pay.approve") >= 2 || c.Role == "HR Admin"}, nil
}

type closureBody struct {
	Refs     []string `json:"refs"`
	All      bool     `json:"all"`
	Note     string   `json:"note"`
	Reason   string   `json:"reason"`
	Half     bool     `json:"half"` // mark-lop: the missing punches chosen are a half day of loss of pay (absences stay a full day)
	Messages []struct {
		To   string   `json:"to"`
		Refs []string `json:"refs"`
		Text string   `json:"text"`
	} `json:"messages"`
}

// closureAct: close, reopen, reject-waiting (at the cut-off, as Month close did), mark-lop (an absence or a missing
// punch as loss of pay), hold (the salary of someone with no bank account) and message. There is no way to set an
// item aside: a closure closes only when nothing is pending.
func (s *Server) closureAct(r *http.Request, c *Ctx) (any, error) {
	key, act := r.PathValue("key"), r.PathValue("action")
	if _, ok := closureDefs[key]; !ok {
		return nil, notFound("")
	}
	if !containsStr([]string{"close", "reopen", "reject-waiting", "mark-lop", "hold", "message"}, act) {
		return nil, notFound("")
	}
	min := 2
	if act == "reopen" {
		min = 1
	}
	ym, err := s.payYm(r, c, min)
	if err != nil {
		return nil, err
	}
	if act == "reopen" && c.Level("pay.approve") < 2 && c.Role != "HR Admin" {
		return nil, forbidden("Reopening a closure needs the Payroll Manager or HR Admin")
	}
	var b closureBody
	if r.ContentLength > 0 {
		if err := decode(r, &b); err != nil {
			return nil, err
		}
	}
	ctx := r.Context()
	out := map[string]any{}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		m, err := ensurePayMonth(ctx, tx, ym) // locks the month: one checklist change at a time
		if err != nil {
			return err
		}
		if open := openYm(ctx, tx); ym != open {
			return bad("Only the open payroll month (%s) has a checklist to change", ymLabel(open))
		}
		switch m.Status {
		case "Open", "Dry run":
		case "Verified":
			return bad("%s is verified. Send it back to the dry run first", ymLabel(ym))
		default:
			return bad("%s is already %s", ymLabel(ym), strings.ToLower(m.Status))
		}
		byKey, _, err := s.checklist(ctx, tx, c, ym, act != "reopen")
		if err != nil {
			return err
		}
		cs := byKey[key]
		what := strings.ToLower(cs.Title[:1]) + cs.Title[1:]
		switch act {
		case "close":
			if cs.closed {
				return bad("%s is already closed", cs.Title)
			}
			if cs.Pending > 0 {
				return bad("%s still has %d pending item%s (%d not raised, %d waiting for approval). Close it when nothing is pending", cs.Title, cs.Pending,
					map[bool]string{true: "", false: "s"}[cs.Pending == 1], cs.NotRaised, cs.Waiting)
			}
			if _, err := tx.C("payroll_closure").Upsert(ctx, bson.M{"ym": ym, "closure": key}, bson.M{"closed_at": tx.Now(), "closed_by": c.Name}, nil); err != nil {
				return err
			}
			if key == "attendance" { // the attendance lock Month close used to set
				if _, err := tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"locked_at": tx.Now(), "locked_by": c.Name}}); err != nil {
					return err
				}
			}
			// Messages about this closure's items are done.
			if _, err := tx.C("notice").UpdateMany(ctx, bson.M{"done_at": nil, "ref": bson.M{"$regex": "^" + regexp.QuoteMeta(ym+":"+key+":")}},
				bson.M{"$set": bson.M{"done_at": tx.Now()}}); err != nil {
				return err
			}
			out["status"] = "Closed"
			return audit(ctx, tx, c, fmt.Sprintf("Closed %s for payroll %s", what, ymLabel(ym)), "payroll_closure", ym+"/"+key, nil, nil)
		case "reopen":
			reason := strings.TrimSpace(b.Reason)
			if len(reason) < 5 {
				return bad("Give the reason for reopening")
			}
			if !cs.closed {
				return bad("%s is not closed", cs.Title)
			}
			if _, err := tx.C("payroll_closure").Update(ctx, bson.M{"ym": ym, "closure": key}, bson.M{"$set": bson.M{"closed_at": nil, "closed_by": nil,
				"reopened_at": tx.Now(), "reopened_by": c.Name, "reopen_reason": reason}}); err != nil {
				return err
			}
			if key == "attendance" {
				if _, err := tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"locked_at": nil, "locked_by": nil}}); err != nil {
					return err
				}
			}
			out["status"] = "Reopened"
			return audit(ctx, tx, c, fmt.Sprintf("Reopened %s for payroll %s: %s", what, ymLabel(ym), reason), "payroll_closure", ym+"/"+key,
				map[string]any{"closedAt": cs.ClosedAt, "closedBy": cs.ClosedBy}, map[string]any{"reason": reason})
		case "mark-lop":
			return markLop(ctx, tx, c, ym, key, cs, b, out)
		case "hold":
			return holdNoBank(ctx, tx, c, ym, key, cs, b, out)
		case "reject-waiting":
			if key != "attendance" && key != "leave" {
				return bad("Only regularizations and leave requests are rejected at the payroll cut-off")
			}
			if cs.closed {
				return bad("%s is closed", cs.Title)
			}
			note := strings.TrimSpace(b.Note)
			if note == "" {
				note = "Closed at payroll cut-off"
			}
			n := 0
			for _, it := range cs.Items {
				if it.Group != grpWaiting || it.RequestID == "" || (!b.All && len(b.Refs) > 0 && !containsStr(b.Refs, it.Ref)) {
					continue
				}
				hit, err := tx.C("approval_request").Update(ctx, bson.M{"_id": it.RequestID, "status": "Pending"}, bson.M{"$set": bson.M{"status": "Rejected"}})
				if err != nil {
					return err
				}
				if hit == 0 {
					continue
				}
				if err := history(ctx, tx, it.RequestID, it.stage, "Rejected", c.Name, note); err != nil {
					return err
				}
				n++
			}
			if n == 0 {
				return bad("Nothing is waiting for approval in %s", what)
			}
			out["rejected"] = n
			return audit(ctx, tx, c, fmt.Sprintf("Rejected %d waiting request%s at the payroll cut-off (%s)", n, map[bool]string{true: "", false: "s"}[n == 1], what), "payroll_closure", ym+"/"+key, nil, note)
		}
		// message
		if len(b.Messages) == 0 {
			return bad("Nothing to send")
		}
		items := map[string]*closureItem{}
		for _, it := range cs.Items {
			items[it.Ref] = it
		}
		desk := map[string]string{}
		if key == "attendance" {
			dl, _ := attendanceDesk(ctx, tx)
			for _, d := range dl {
				desk[d.ID] = d.Name
			}
		}
		sent := 0
		for _, msg := range b.Messages {
			text := strings.TrimSpace(msg.Text)
			if len(text) < 3 || len(text) > 1000 {
				return bad("Write the message (up to 1000 characters)")
			}
			if len(msg.Refs) == 0 {
				return bad("Choose what the message is about")
			}
			its := []*closureItem{}
			for _, ref := range msg.Refs {
				it := items[ref]
				if it == nil {
					return bad("%s is no longer pending, or is outside your access. Reload the checklist", ref)
				}
				its = append(its, it)
			}
			to, kind, toName := msg.To, "payroll", ""
			same := true
			for _, it := range its {
				same = same && it.WaitsOnID != "" && it.WaitsOnID == its[0].WaitsOnID
			}
			switch {
			case to == "" || (same && to == its[0].WaitsOnID):
				if !same {
					return bad("A message goes to one person: these items wait on different people, or on no one")
				}
				to, toName = its[0].WaitsOnID, its[0].WaitsOn
			case desk[to] != "":
				kind, toName = "payroll-desk", desk[to]
			default:
				return bad("Send the message to the person the items wait on")
			}
			link, view := "/attendance", "MIS Executive"
			if kind == "payroll" {
				link, view = noticeLink(key, its[0].Group, its)
			}
			id, err := tx.C("notice").Insert(ctx, bson.M{"to_employee_id": to, "from_employee_id": c.EmployeeID, "kind": kind, "text": text, "link": link,
				"view": view, "ref": msg.Refs})
			if err != nil {
				return err
			}
			if err := audit(ctx, tx, c, fmt.Sprintf("Sent a payroll message to %s (%s)", toName, what), "notice", fmt.Sprint(id), nil,
				map[string]any{"to": to, "refs": msg.Refs, "text": text}); err != nil {
				return err
			}
			sent++
		}
		out["sent"] = sent
		return nil
	})
	return out, err
}

// markLop records unexplained absences and missing punches of the Attendance or Leave closure as loss of pay at the
// payroll cut-off (spec 15.1 #1, "reject pending and lock"): for a day nobody raised anything for, including a
// leaver's days up to the last working day and people whose leave plan has no Loss of pay (contractual, retainer).
// An absence is a full day. A missing punch is a full day, or with b.Half a half day: stored as the leave module
// stores a half-day leave (payload.half, one date, facts.days 0.5), so the attendance engine counts 0.5 loss of pay
// and the punched half as worked. The full days of a person become one approved Loss of pay leave raised by Payroll
// (cov lists the days), each half day its own; the reason is in their history, so the attendance engine, payroll,
// the employee's calendar and the leave report all show it, and the closure item clears. Audited.
func markLop(ctx context.Context, tx store.Q, c *Ctx, ym, key string, cs *closureState, b closureBody, out map[string]any) error {
	if key != "attendance" && key != "leave" {
		return bad("Only absences and missing punches in the attendance and leave closures are marked as loss of pay")
	}
	if cs.closed {
		return bad("%s is closed", cs.Title)
	}
	reason := strings.TrimSpace(cmpOr(b.Reason, b.Note))
	if len(reason) < 5 {
		return bad("Give the reason for marking these days as loss of pay")
	}
	full, half := map[string][]string{}, map[string][]string{}
	order := []string{}
	nAbs, nFull, nHalf := 0, 0, 0
	for _, it := range cs.Items {
		if !it.CanLop || it.Group != grpNotRaised || (!b.All && !containsStr(b.Refs, it.Ref)) {
			continue
		}
		if _, ok := full[it.EmployeeID]; !ok {
			if _, ok := half[it.EmployeeID]; !ok {
				order = append(order, it.EmployeeID)
			}
		}
		switch {
		case it.LopHalf && b.Half:
			half[it.EmployeeID] = append(half[it.EmployeeID], it.Date)
			nHalf++
		case it.LopHalf:
			full[it.EmployeeID] = append(full[it.EmployeeID], it.Date)
			nFull++
		default:
			full[it.EmployeeID] = append(full[it.EmployeeID], it.Date)
			nAbs++
		}
	}
	if len(order) == 0 {
		return bad("Choose the absences or missing punches to mark as loss of pay (days with nothing raised)")
	}
	raise := func(emp string, ds []string, isHalf bool) error {
		seq, err := store.NextSeq(ctx, tx, "approval_request_seq")
		if err != nil {
			return err
		}
		id := fmt.Sprintf("LV-%d", seq)
		days, note := float64(len(ds)), "Marked as loss of pay at the payroll cut-off: "+reason
		payload := bson.M{"ltype": "Loss of pay", "from": ds[0], "to": ds[len(ds)-1], "cov": ds, "reason": reason, "lopMark": true}
		if isHalf { // a half-day leave, as the leave module stores one: one date, half, 0.5 days
			days, note = 0.5, "Marked as half a day of loss of pay at the payroll cut-off (the punched half counts as worked): "+reason
			payload["half"] = true
		}
		if _, err := tx.C("approval_request").Insert(ctx, bson.M{"_id": id, "wf": "leave", "employee_id": emp, "raised_by": c.EmployeeID, "flow": "Payroll",
			"facts":   bson.M{"wf": "leave", "days": days},
			"payload": payload,
			"chain":   []string{}, "step": 0, "status": "Approved"}); err != nil {
			return err
		}
		return history(ctx, tx, id, "", "Applied", c.Name, note)
	}
	refs := []string{}
	for _, emp := range order {
		if ds := full[emp]; len(ds) > 0 {
			sort.Strings(ds)
			if err := raise(emp, ds, false); err != nil {
				return err
			}
		}
		hs := half[emp]
		sort.Strings(hs)
		for _, d := range hs {
			if err := raise(emp, []string{d}, true); err != nil {
				return err
			}
		}
		for _, d := range append(append([]string{}, full[emp]...), hs...) {
			refs = append(refs, ym+":"+key+":"+emp+"/"+d)
		}
	}
	if _, err := tx.C("notice").UpdateMany(ctx, bson.M{"done_at": nil, "ref": bson.M{"$in": refs}}, bson.M{"$set": bson.M{"done_at": tx.Now()}}); err != nil {
		return err
	}
	n := nAbs + nFull + nHalf
	out["marked"], out["absences"], out["fullDays"], out["halfDays"] = n, nAbs, nFull, nHalf
	out["lopDays"] = float64(nAbs+nFull) + 0.5*float64(nHalf)
	parts := []string{}
	if nAbs > 0 {
		parts = append(parts, countOf(nAbs, "absence", "absences"))
	}
	if nFull > 0 {
		parts = append(parts, countOf(nFull, "missing punch", "missing punches")+" as a full day")
	}
	if nHalf > 0 {
		parts = append(parts, countOf(nHalf, "missing punch", "missing punches")+" as a half day")
	}
	return audit(ctx, tx, c, fmt.Sprintf("Marked %s as loss of pay at the payroll cut-off (%s)", strings.Join(parts, " and "), strings.ToLower(cs.Title)), "payroll_closure", ym+"/"+key, nil,
		map[string]any{"days": full, "halfDays": half, "reason": reason})
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// noBankHoldWhy is the reason of a salary hold placed from the payroll checklist for someone with no bank account on
// record; noBankHoldSource marks those holds, released once an account is on record (syncBankHolds).
const (
	noBankHoldWhy    = "No bank account on record"
	noBankHoldSource = "no-bank"
)

// holdCovers says whether a hold from month from, through month through ("" = every later month), holds month ym.
func holdCovers(from, through, ym string) bool {
	return from != "" && from <= ym && (through == "" || through >= ym)
}

// holdMonths names the months a hold holds: "November 2026", "November to December 2026", or "from November 2026".
func holdMonths(from, through string) string {
	switch {
	case through == "":
		return "from " + ymLabel(from)
	case through == from:
		return ymLabel(from)
	}
	return ymLabel(from) + " to " + ymLabel(through)
}

// holdThrough is the filter part of a hold that holds month ym as far as its end goes (to_ym null, or ym or later);
// from_ym is checked separately.
func holdThrough(ym string) bson.A {
	return bson.A{bson.M{"to_ym": nil}, bson.M{"to_ym": bson.M{"$gte": ym}}}
}

// holdNoBank holds the salary of people in the Bank verification closure with no bank account on record, for the
// open month, so the closure can close: the salary is calculated but not paid (left out of the bank file) and the
// hold is listed on Salary holds. It is the hold Salary holds places (placeHold), held for this month only
// (to_ym); someone still without an account next month is listed again and held again, which moves the same hold
// on. Once an account is on record, syncBankHolds releases it and the held months are paid in the open month as
// "Held salary released, <month>". Payroll can still release it by hand in Salary holds. Audited.
func holdNoBank(ctx context.Context, tx store.Q, c *Ctx, ym, key string, cs *closureState, b closureBody, out map[string]any) error {
	if key != "bank" {
		return bad("Only salaries of people with no bank account on record are held from the checklist")
	}
	if cs.closed {
		return bad("%s is closed", cs.Title)
	}
	reason := strings.TrimSpace(cmpOr(b.Reason, b.Note))
	if len(reason) < 5 {
		return bad("Give the reason for holding the salary")
	}
	its := []*closureItem{}
	for _, it := range cs.Items {
		if it.CanHold && it.Group == grpNotRaised && (b.All || containsStr(b.Refs, it.Ref)) {
			its = append(its, it)
		}
	}
	if len(its) == 0 {
		return bad("Choose the people with no bank account on record whose salary to hold")
	}
	refs, held := []string{}, []string{}
	for _, it := range its {
		h, err := tx.C("salary_hold").FindOne(ctx, bson.M{"employee_id": it.EmployeeID, "status": "Active"})
		if err != nil {
			return err
		}
		if h != nil { // held here for an earlier month: the same hold now holds this month too
			from, through := str(h["from_ym"]), str(h["to_ym"])
			if str(h["source_id"]) != noBankHoldSource || through == "" || through >= ym {
				return bad("%s already has a salary hold (%s). Reload the checklist", it.Name, str(h["id"]))
			}
			if _, err := tx.C("salary_hold").Update(ctx, bson.M{"_id": h["id"]}, bson.M{"$set": bson.M{"to_ym": ym, "extended_at": tx.Now()}}); err != nil {
				return err
			}
			if err := audit(ctx, tx, c, fmt.Sprintf("Salary hold extended to %s (no bank account on record)", ymLabel(ym)), "salary_hold", str(h["id"]),
				map[string]any{"months": holdMonths(from, through)}, map[string]any{"months": holdMonths(from, ym), "reason": reason}); err != nil {
				return err
			}
			held = append(held, str(h["id"]))
		} else {
			id, err := placeHold(ctx, tx, c.Name, it.EmployeeID, noBankHoldWhy, reason, ym, noBankHoldSource)
			if err != nil {
				return err
			}
			if _, err := tx.C("salary_hold").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"to_ym": ym}}); err != nil {
				return err
			}
			if err := audit(ctx, tx, c, "Salary put on hold", "salary_hold", id, nil, map[string]any{"employeeId": it.EmployeeID, "reason": noBankHoldWhy, "note": reason,
				"fromYm": ym, "toYm": ym, "from": "Payroll checklist, bank verification closure"}); err != nil {
				return err
			}
			held = append(held, id)
		}
		refs = append(refs, it.Ref)
	}
	if _, err := tx.C("notice").UpdateMany(ctx, bson.M{"done_at": nil, "ref": bson.M{"$in": refs}}, bson.M{"$set": bson.M{"done_at": tx.Now()}}); err != nil {
		return err
	}
	out["held"], out["holds"] = len(held), held
	return audit(ctx, tx, c, fmt.Sprintf("Held %s for payroll %s (no bank account on record)", countOf(len(held), "salary", "salaries"), ymLabel(ym)),
		"payroll_closure", ym+"/"+key, nil, map[string]any{"holds": held, "reason": reason})
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// setupWarnings are set-up gaps that make the month's pay wrong without anything failing (payroll test REAL-07,
// REAL-11, REAL-52), shown above the checklist: a work policy with no holiday calendar for people in the run (every
// holiday becomes a no-punch working day), a leave plan without Loss of pay for an employment type in the run
// (their absences cannot take a leave; Payroll marks them as loss of pay at the cut-off), and blank company
// statutory codes (payslips, ECR, ESI and Form 16 need them).
func setupWarnings(ctx context.Context, q store.Q, ym string) ([]string, error) {
	out := []string{}
	eligible, err := payEligible(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	list, err := loadEmps(ctx, q, eligible)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil, err
	}
	noCal, noLop := map[string]int{}, map[string]int{}
	calOrder, lopOrder := []string{}, []string{}
	for _, e := range list {
		p, _ := ts.policyFor(e)
		if ts.holidays(p, e.CityID) == nil {
			if noCal[p.Name] == 0 {
				calOrder = append(calOrder, p.Name)
			}
			noCal[p.Name]++
		}
		if cfg := ts.typeCfg(p, "Loss of pay"); cfg == nil || !cfg.On || !containsStr(cfg.Types, e.Type) {
			k := p.Name + "\x00" + e.Type
			if noLop[k] == 0 {
				lopOrder = append(lopOrder, k)
			}
			noLop[k]++
		}
	}
	people := func(n int) string {
		return fmt.Sprintf("%d %s", n, map[bool]string{true: "person", false: "people"}[n == 1])
	}
	for _, name := range calOrder {
		out = append(out, fmt.Sprintf("Work policy %q has no holiday calendar for %s in this run: holidays count as working days, so each shows as a no-punch day. Attach a calendar in Policies before the dry run.", name, people(noCal[name])))
	}
	for _, k := range lopOrder {
		pt := strings.SplitN(k, "\x00", 2)
		out = append(out, fmt.Sprintf("The leave plan of work policy %q has no Loss of pay for %s employees (%s in this run): their absences cannot take a leave. Switch it on in the leave plan, or mark the absences as loss of pay at the cut-off.", pt[0], pt[1], people(noLop[k])))
	}
	if w, err := pfBasicWarnings(ctx, q, ym, list); err != nil {
		return nil, err
	} else if w != "" {
		out = append(out, w)
	}
	if co, err := companyInfo(ctx, q); err == nil {
		miss := []string{}
		for _, x := range []struct{ n, v string }{{"PAN", co.PAN}, {"TAN", co.TAN}, {"PF code", co.PFCode}, {"ESI code", co.ESICode}} {
			if strings.TrimSpace(x.v) == "" {
				miss = append(miss, x.n)
			}
		}
		if len(miss) > 0 {
			out = append(out, "The company's "+strings.Join(miss, ", ")+" "+map[bool]string{true: "is", false: "are"}[len(miss) == 1]+" blank (Masters › Company): payslips, the ECR, the ESI file and Form 16 need them.")
		}
	}
	return out, nil
}
