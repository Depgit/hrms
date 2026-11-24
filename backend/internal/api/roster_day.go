package api

// Individual roster changes in a roster cell (owner, 24 Nov 2026: "individual roster change allow this as well").
// A click on a cell of Shift and roster › Roster opens a picker (Roster.tsx) instead of cycling the codes:
//
//   - Other time for this day (code OT): a start and an end, with an optional second part, checked like a split shift
//     in Masters › Shifts (the second part starts after the first ends, all on the same day; a day in one part may end
//     the next day). No shift master is needed: the times are stored on the roster_entry. The engine treats the day
//     exactly like a shift with those timings (monthData.shiftPlan): hours, the split gap, the late mark, the punch
//     window (dayPlanner) and the deviation allowances. The attendance rules are the work policy's (no shift master,
//     so no shift rules); a day in one part that ends after midnight is recorded like a night shift (Shift ending
//     next day). It is a roster change like any other: the lock of past dates, the late-change reason, the rest rules
//     and the roster log all apply, and the On duty count counts it.
//   - Called to work: a shift put on a holiday (PH on the employee's calendar) or a day of approved leave calls the
//     person to work, with that shift: the same work_call record, engine, right (att.records at Edit), scope and
//     refusals as Attendance › Called to work (work_call.go), and the reason it needs. The shift is stored on the call
//     (shift_code) and the day is measured against it. The cell shows the shift with a mark; removing the call from the
//     cell (DELETE /api/attendance/called/{id}) puts the holiday or leave back. A shift on a holiday as a plain roster
//     change is refused (rosterCheckDay): the owner's rule replaces the old "PH → shift = an ordinary working day".
//     Week off cells keep today's behaviour.

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// otherCode is the roster code of a cell with other time for the day. It is a roster code (rosterCodes): no shift
// master can take it.
const otherCode = "OT"

// otherBody is a one-day timing as sent and shown: HH:MM, the second part empty for a day in one part.
type otherBody struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Start2 string `json:"start2,omitempty"`
	End2   string `json:"end2,omitempty"`
}

// otherShift checks a one-day timing like the times of a shift (Masters › Shifts) and returns it as a shift.
func otherShift(b *otherBody) (attend.Shift, error) {
	b.Start, b.End, b.Start2, b.End2 = strings.TrimSpace(b.Start), strings.TrimSpace(b.End), strings.TrimSpace(b.Start2), strings.TrimSpace(b.End2)
	if !reTime.MatchString(b.Start) {
		return attend.Shift{}, bad("Start must be HH:MM, for example 10:00")
	}
	if !reTime.MatchString(b.End) {
		return attend.Shift{}, bad("End must be HH:MM, for example 18:00")
	}
	st, _ := attend.Min(b.Start)
	en, _ := attend.Min(b.End)
	if st == en {
		return attend.Shift{}, bad("Start and end cannot be the same")
	}
	if b.Start2 != "" || b.End2 != "" {
		if en <= st {
			return attend.Shift{}, bad("A day in two parts must start and end on the same day: the first part must end after it starts")
		}
		if k, msg := splitProblem(st, en, b.Start2, b.End2, false); k != "" {
			return attend.Shift{}, bad("%s", msg)
		}
	}
	return shiftOf(otherCode, b.Start, b.End, b.Start2, b.End2, nil), nil
}

// otherRules are the rules of a day with other time: none (the work policy decides), except that a day in one part
// ending after midnight is recorded like a night shift.
func otherRules(sh attend.Shift) *attend.Rules {
	if !sh.Split() && sh.End <= sh.Start {
		t := true
		return &attend.Rules{NextDay: &t}
	}
	return nil
}

// otherOfRow is a roster_entry's other time (nil for a cell without one).
func otherOfRow(r store.Row) *attend.Shift {
	st, en := str(r["start_time"]), str(r["end_time"])
	if st == "" || en == "" {
		return nil
	}
	sh := shiftOf(otherCode, st, en, str(r["start2_time"]), str(r["end2_time"]), nil)
	return &sh
}

// otherOut is a timing as shown in a form.
func otherOut(sh *attend.Shift) *otherBody {
	if sh == nil {
		return nil
	}
	b := &otherBody{Start: attend.HHMM(sh.Start), End: attend.HHMM(sh.End)}
	if sh.Split() {
		b.Start2, b.End2 = attend.HHMM(sh.Start2), attend.HHMM(sh.End2)
	}
	return b
}

// otherFields are a roster_entry's time fields for a timing (all null for nil).
func otherFields(sh *attend.Shift) bson.M {
	m := bson.M{"start_time": nil, "end_time": nil, "start2_time": nil, "end2_time": nil}
	if sh != nil {
		b := otherOut(sh)
		m["start_time"], m["end_time"] = b.Start, b.End
		if b.Start2 != "" {
			m["start2_time"], m["end2_time"] = b.Start2, b.End2
		}
	}
	return m
}

// sameTiming: two timings (nil for none) are the same.
func sameTiming(a, b *attend.Shift) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Start == b.Start && a.End == b.End && a.Start2 == b.Start2 && a.End2 == b.End2
}

// timingOf is a timing as text ("" for none).
func timingOf(sh *attend.Shift) string {
	if sh == nil {
		return ""
	}
	return sh.Timing()
}

// dayShift is the shift a roster code stands for on the date: the version of a shift master in effect, or the cell's
// other time (nil for WO, PH, L or an unknown code).
func (m *monthData) dayShift(e *emp, d, code string) *attend.Shift {
	if code == otherCode {
		if x, ok := m.roster[e.ID][d]; ok && x.Other != nil {
			sh := *x.Other
			return &sh
		}
		return nil
	}
	if rosterCodes[code] {
		return nil
	}
	if sh, _, ok := m.ts.shiftOn(code, d); ok {
		return &sh
	}
	return nil
}

// isHolidayCell: the date is a holiday on the employee's calendar (the policy default is PH) and the roster keeps it
// so, with no approved leave on it. A shift there is a call to work.
func (m *monthData) isHolidayCell(e *emp, p *workPolicy, d string) bool {
	return m.leaveOn(e, d, false) == nil && m.rosterCode(e, p, d) == "PH" &&
		p.S.DefCode(e.ID, m.ts.baseShift(e), d, m.ts.holidays(p, e.CityID)) == "PH"
}

// rosterCall calls the employee to work with a shift on a holiday or a day of approved leave, from the roster; with a
// call already there it changes the call's shift (and its reason when one is given).
func (s *Server) rosterCall(ctx context.Context, tx store.Q, c *Ctx, ts *timeSet, e *emp, date, code, reason string) (string, error) {
	sh, ok := ts.shifts[code]
	if !ok {
		return "", bad("Choose the shift they are called to work")
	}
	if !sh.RunsFor(e.DeptID, e.LocID) {
		return "", bad("%s", shiftNotForMsg(code, e.Dept, e.Location))
	}
	if reason = strings.TrimSpace(reason); len(reason) > 500 {
		return "", bad("Keep the reason under 500 characters")
	}
	if msg := paidMonthMsg(ctx, tx, date, date); msg != "" {
		return "", bad("%s", msg)
	}
	m, err := loadMonth(ctx, tx, ts, []string{e.ID}, date, date)
	if err != nil {
		return "", err
	}
	if cd := m.callOf(e.ID, date); cd != nil {
		if ym := lockedMonthOf(ctx, tx, date); ym != "" && !exceptionOpen(ctx, tx, e.ID, "regularize", date, date) {
			return "", bad("Attendance for %s is locked by payroll. Ask the Payroll Manager for an exception", ymLabel(ym))
		}
		set := bson.M{}
		if cd.Shift != code {
			set["shift_code"] = code
		}
		if reason != "" && reason != cd.Reason {
			set["reason"] = reason
		}
		if len(set) == 0 {
			return "unchanged", nil
		}
		if _, err := tx.C(store.WorkCallColl).Update(ctx, bson.M{"_id": cd.ID}, bson.M{"$set": set}); err != nil {
			return "", err
		}
		if err := audit(ctx, tx, c, fmt.Sprintf("Call to work of %s (%s) on %s changed: shift %s → %s%s", e.Name, e.ID, longDate(date), or(cd.Shift, "of the roster"), code,
			map[bool]string{true: ", reason: " + reason, false: ""}[set["reason"] != nil]), store.WorkCallColl, cd.ID, map[string]any{"shift_code": cd.Shift, "reason": cd.Reason}, set); err != nil {
			return "", err
		}
		return "changed", callRebuild(ctx, tx, e.ID, date)
	}
	if reason == "" {
		return "", bad("Give the reason they are called to work")
	}
	if err := s.callInsert(ctx, tx, c, ts, m, e, date, reason, code); err != nil {
		return "", err
	}
	return "called", callRebuild(ctx, tx, e.ID, date)
}

// callRebuild brings a day already punched (and its neighbours) in line with a call's shift window: only machine days
// that change are written (rebuildPunchDays with the hook).
func callRebuild(ctx context.Context, tx store.Q, id, date string) error {
	if date > today() {
		return nil
	}
	return rebuildPunchDays(ctx, tx, nil, map[string]map[string]bool{id: {policy.AddDays(date, -1): true, date: true, policy.AddDays(date, 1): true}}, true)
}
