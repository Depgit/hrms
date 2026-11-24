package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Roster (spec 11.1): the published roster drives attendance. Only changes from the policy
// default are stored; late changes need a reason and show in the discipline report.

var rosterReasons = []string{"Guest demand or occupancy", "Staff absent or sick", "Employee request", "Event or banquet", "Vacancy cover", "Other"}

func weekStart(d string) string {
	t := policy.D(d)
	return policy.ISO(t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)))
}

type rosterRowOut struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Designation string        `json:"designation"`
	Policy      string        `json:"policy"`
	Location    string        `json:"location"`
	LocationID  string        `json:"locationId"`
	Department  string        `json:"department"`
	Codes       []string      `json:"codes"` // the codes usable at the employee's property
	Cells       []rosterCellO `json:"cells"`
	Warnings    []string      `json:"warnings"`
	DevOwn      bool          `json:"devOwn,omitempty"` // deviation allowances of their own in effect (dev_own.go)
}

type rosterCellO struct {
	Code    string     `json:"code"`
	Changed bool       `json:"changed,omitempty"`
	Late    bool       `json:"late,omitempty"`
	Tip     string     `json:"tip,omitempty"`
	Time    string     `json:"time,omitempty"`    // the shift's timings on the day: "06:00–10:00 + 15:00–19:00" for a split shift
	Pending string     `json:"pending,omitempty"` // leave waiting for approval
	Dev     *rosterDev `json:"dev,omitempty"`     // the day worked differently from the roster
	// The individual changes of the cell picker (roster_day.go).
	Other   *otherBody   `json:"other,omitempty"`   // OT: other time for this day
	Leave   string       `json:"leave,omitempty"`   // approved leave on the day (paid or unpaid): a shift calls them to work
	Holiday bool         `json:"holiday,omitempty"` // a holiday on their calendar: a shift calls them to work
	Call    *rosterCallO `json:"call,omitempty"`    // called to work on the day
}

// rosterCallO is a call to work shown in a roster cell.
type rosterCallO struct {
	ID        string `json:"id"`
	Shift     string `json:"shift"` // the call's shift, else the one the day is measured against
	Time      string `json:"time,omitempty"`
	Kind      string `json:"kind"` // holiday | leave
	Leave     string `json:"leave,omitempty"`
	Reason    string `json:"reason"`
	By        string `json:"by"`
	At        string `json:"at"`
	Active    bool   `json:"active"`    // the day still is a holiday or leave day (else the call does nothing)
	Removable bool   `json:"removable"` // the month is not paid
}

// rosterDev is a deviation on the roster grid, with the manager's decision (Pending until taken).
type rosterDev struct {
	Planned string   `json:"planned"`
	Actual  string   `json:"actual"`
	Kinds   []string `json:"kinds"`
	In      string   `json:"in"`
	Out     string   `json:"out"`
	Hours   float64  `json:"hours"`
	Status  string   `json:"status"`
}

func (s *Server) rosterGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 1); err != nil {
		return nil, err
	}
	ctx, qv := r.Context(), r.URL.Query()
	loc, city, dept := qv.Get("location"), qv.Get("city"), qv.Get("department")
	wk := weekStart(or(qv.Get("week"), today()))
	days := make([]string, 7)
	for i := range days {
		days[i] = policy.AddDays(wk, i)
	}
	emps, err := rosterEmps(ctx, s.st.DB, c, loc, city, dept)
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	ym := wk[:7]
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), ym+"-01", policy.LastDayOf(days[6][:7]))
	if err != nil {
		return nil, err
	}
	rows := []rosterRowOut{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		row := rosterRowOut{ID: e.ID, Name: e.Name, Designation: e.Desig, Policy: p.Name, Location: e.Location, LocationID: e.LocID, Department: e.Dept,
			Codes: ts.codesFor(e), Warnings: []string{}}
		codes := []string{}
		if v := ts.ownDevOn(e.ID, days[6]); v != nil && v.R.Own() {
			row.DevOwn = true
		}
		for _, d := range days {
			cell := rosterCellO{Code: m.planned(e, p, d).Code}
			if sh := m.dayShift(e, d, cell.Code); sh != nil {
				cell.Time = sh.Timing()
			}
			if cell.Code == otherCode {
				cell.Other = otherOut(m.dayShift(e, d, otherCode))
			}
			if l := m.leaveOn(e, d, false); l != nil {
				cell.Leave = l.Type
			} else {
				cell.Holiday = m.isHolidayCell(e, p, d)
			}
			if cd := m.callOf(e.ID, d); cd != nil {
				pl := m.callPlan(e, p, d)
				cell.Call = &rosterCallO{ID: cd.ID, Shift: pl.Code, Kind: cd.Kind, Leave: cd.LType, Reason: cd.Reason, By: cd.By, At: cd.At, Active: m.callIn(e, p, d),
					Removable: paidMonthMsg(ctx, s.st.DB, d, d) == ""}
				if pl.Shift != nil {
					cell.Call.Time = pl.Shift.Timing()
				}
			}
			if rc, ok := m.roster[e.ID][d]; ok {
				cell.Changed, cell.Late = true, rc.Late
				cell.Tip = fmt.Sprintf("Changed%s by %s, %s. Was %s", map[bool]string{true: " late", false: ""}[rc.Late], rc.By, rc.At, or(rc.From, "—"))
				if rc.Why != "" {
					cell.Tip += ". " + rc.Why
				}
				if rc.Dev {
					cell.Tip = "Set from an approved deviation. Was " + or(rc.From, "—")
				}
			}
			if l := m.leaveOn(e, d, true); l != nil && l.Status == "Pending" {
				cell.Pending = l.Type
			}
			if v, x := m.deviation(e, p, d); v != nil {
				cell.Dev = &rosterDev{Planned: v.Planned, Actual: v.Actual, Kinds: v.Kinds, In: v.In, Out: v.Out, Hours: v.Hours, Status: "Pending"}
				if x != nil {
					cell.Dev.Status = x.Status
				}
			}
			row.Cells = append(row.Cells, cell)
			codes = append(codes, cell.Code)
		}
		row.Warnings = m.rosterWarnings(e, p, days, codes)
		rows = append(rows, row)
	}
	// The properties shown, each with its publish state for the week; the roster is published
	// when every one of them is.
	locIDs, locs := []string{}, []map[string]any{}
	if loc != "" {
		locIDs = append(locIDs, loc)
	}
	for _, e := range emps {
		if !containsStr(locIDs, e.LocID) {
			locIDs = append(locIDs, e.LocID)
		}
	}
	pubs, _ := rosterPublished(ctx, s.st.DB, locIDs, or(dept, "All"), wk)
	var pub any
	all := len(pubs) > 0
	for _, x := range pubs {
		var rec any
		if x["by_name"] != nil {
			rec = map[string]any{"by_name": x["by_name"], "at": x["at"], "late": x["late"]}
			if pub == nil {
				pub = rec
			}
		} else {
			all = false
		}
		locs = append(locs, map[string]any{"id": x["id"], "name": x["name"], "published": rec})
	}
	if !all {
		pub = nil
	}
	codes := ts.codesAt(locIDs...)
	if len(locIDs) == 0 { // nobody shown: every active shift
		codes = []string{}
		for _, sh := range ts.list {
			codes = append(codes, sh.Code)
		}
		codes = append(codes, "WO", "PH")
	}
	// Each shift code's timings for the legend, as in effect on the week's first day.
	timings := map[string]string{}
	for _, code := range codes {
		if sh, _, ok := ts.shiftOn(code, days[0]); ok {
			timings[code] = sh.Timing()
		}
	}
	lead, past, reason := 2, "lock", true
	if len(emps) > 0 {
		p, _ := ts.policyFor(emps[0])
		lead, past, reason = p.S.RS.Lead, p.S.RS.Past, p.S.RS.Reason
	}
	alerts, _ := s.rosterAlerts(ctx, c, ym)
	return map[string]any{"week": wk, "days": days, "rows": rows, "codes": codes, "timings": timings, "published": pub, "locations": locs, "reasons": rosterReasons,
		"rules": map[string]any{"lead": lead, "past": past, "reason": reason}, "alerts": alerts, "today": today()}, nil
}

// rosterPublished lists the properties (by name) with the week's publish record for the department, when there
// is one (a property without one still shows): id, name, by_name, at, late.
func rosterPublished(ctx context.Context, q store.Q, locIDs []string, dept, wk string) ([]store.Row, error) {
	locs, err := q.C("location").Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: locIDs}}}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	marks, err := q.C("roster_publish").Find(ctx, bson.D{{Key: "location_id", Value: bson.D{{Key: "$in", Value: locIDs}}}, {Key: "department", Value: dept}, {Key: "week_start", Value: wk}})
	if err != nil {
		return nil, err
	}
	byLoc := map[string]store.Row{}
	for _, p := range marks {
		byLoc[str(p["location_id"])] = p
	}
	out := []store.Row{}
	for _, l := range locs {
		x := store.Row{"id": l["id"], "name": l["name"], "by_name": nil, "at": nil, "late": nil}
		if p := byLoc[str(l["id"])]; p != nil {
			x["by_name"], x["at"], x["late"] = p["by_name"], p["at"], p["late"]
		}
		out = append(out, x)
	}
	return out, nil
}

// rosterWarnings mirrors rosterWarn: no weekly off, short rest, unmapped shift, too many days in a row, off cap.
func (m *monthData) rosterWarnings(e *emp, p *workPolicy, days, codes []string) []string {
	w := []string{}
	hasOff := false
	for _, c := range codes {
		if c == "WO" || c == "PH" {
			hasOff = true
		}
	}
	if !hasOff {
		w = append(w, "No weekly off")
	}
	for i := 0; i+1 < len(codes); i++ {
		a, b := m.dayShift(e, days[i], codes[i]), m.dayShift(e, days[i+1], codes[i+1])
		if a != nil && b != nil {
			if g, _ := attend.RestBetween(a, b); p.S.Att.MinRest > 0 && float64(g) < p.S.Att.MinRest*60 {
				w = append(w, fmt.Sprintf("%s then %s leaves %.1f h of rest", codes[i], codes[i+1], math.Max(0, float64(g)/60)))
			}
		}
	}
	for _, c := range codes {
		if sh, ok := m.ts.shifts[c]; ok && !sh.RunsFor(e.DeptID, e.LocID) {
			w = append(w, "Shift "+c+" is not mapped to this department and location")
			break
		}
	}
	run, mx := 0, 0
	for _, c := range codes {
		if c == "WO" || c == "PH" || c == "L" {
			run = 0
		} else {
			run++
			if run > mx {
				mx = run
			}
		}
	}
	if lim := p.S.WO.Consec; lim > 0 && mx > lim {
		w = append(w, fmt.Sprintf("%d days in a row, policy allows %d", mx, lim))
	}
	ym := days[0][:7]
	if total, over := p.S.MonthOffs(ym, func(d string) string { return m.rosterCode(e, p, d) }); over {
		w = append(w, fmt.Sprintf("%d offs in %s, policy cap is %d", total, policy.D(ym+"-01").Format("January"), p.S.OffCap.Max))
	}
	return w
}

// rosterSet applies one cell change with the policy's rules (rosSet). It returns a warning when
// the rest gap is short and the policy only warns.
func (s *Server) rosterSet(ctx context.Context, tx store.Q, c *Ctx, ts *timeSet, e *emp, date, code, why, via string) (warn string, late bool, err error) {
	return s.rosterSetDay(ctx, tx, c, ts, e, date, code, nil, why, via)
}

// rosterSetDay is rosterSet with other time for the day (code OT and its timing; nil for any other code).
func (s *Server) rosterSetDay(ctx context.Context, tx store.Q, c *Ctx, ts *timeSet, e *emp, date, code string, other *attend.Shift, why, via string) (warn string, late bool, err error) {
	now, same, warn, late, err := rosterCheckDay(ts, e, date, code, other, why, via, func() (*monthData, error) {
		return loadMonth(ctx, tx, ts, []string{e.ID}, policy.AddDays(date, -1), policy.AddDays(date, 1))
	})
	if err != nil || same {
		return "", false, err
	}
	cur := now.Code
	p, _ := ts.policyFor(e)
	def := p.S.DefCode(e.ID, ts.baseShift(e), date, ts.holidays(p, e.CityID))
	by := c.Name + " (" + c.Role + ")"
	cell := bson.D{{Key: "employee_id", Value: e.ID}, {Key: "date", Value: date}}
	if code == def { // back to the policy default: nothing is stored for the day
		if _, err := tx.C("roster_entry").Delete(ctx, cell); err != nil {
			return "", false, err
		}
	} else {
		set := bson.M{"code": code, "why": timectxNullIf(why), "late": late, "from_code": cur, "by_dev": via == "deviation", "by_name": by, "at": tx.Now()}
		for k, v := range otherFields(other) {
			set[k] = v
		}
		if _, err := tx.C("roster_entry").Upsert(ctx, cell, set, nil); err != nil {
			return "", false, err
		}
	}
	_, err = tx.C("roster_log").Insert(ctx, bson.M{"employee_id": e.ID, "date": date, "from_code": cur, "to_code": code, "why": timectxNullIf(why), "late": late,
		"by_dev": via == "deviation", "location_id": e.LocID, "department": e.Dept, "by_name": by,
		"from_time": timectxNullIf(timingOf(now.Other)), "to_time": timectxNullIf(timingOf(other))})
	if err == nil && date <= today() { // a day already punched moves to or from a shift's punch window (Set shift)
		_, ro, _ := ts.shiftOn(cur, date)
		_, rn, _ := ts.shiftOn(code, date)
		if now.Other != nil {
			ro = otherRules(*now.Other)
		}
		if other != nil {
			rn = otherRules(*other)
		}
		if early, lateOut := attend.PunchLimits(&p.S); ro.Windowed(early, lateOut) || rn.Windowed(early, lateOut) {
			err = rebuildPunchDays(ctx, tx, ts, map[string]map[string]bool{e.ID: {policy.AddDays(date, -1): true, date: true, policy.AddDays(date, 1): true}}, true)
		}
	}
	return warn, late, err
}

// rosterCheck applies the rules of one cell change without writing anything. month gives the roster around the
// date (the day before and after). It returns the code the cell has now (cur == code: nothing to change),
// whether the change is late, a warning when the rest gap is short and the policy only warns, and the rule that
// refuses the change as err.
func rosterCheck(ts *timeSet, e *emp, date, code, why, via string, month func() (*monthData, error)) (cur, warn string, late bool, err error) {
	now, _, warn, late, err := rosterCheckDay(ts, e, date, code, nil, why, via, month)
	return now.Code, warn, late, err
}

// rosterCheckDay is rosterCheck with other time for the day (code OT and its timing). now is the cell as it is (its
// code and other time); same is true when the change would leave it so.
func rosterCheckDay(ts *timeSet, e *emp, date, code string, other *attend.Shift, why, via string, month func() (*monthData, error)) (now rosterCell, same bool, warn string, late bool, err error) {
	fail := func(err error) (rosterCell, bool, string, bool, error) { return rosterCell{}, false, "", false, err }
	p, _ := ts.policyFor(e)
	switch _, isShift := ts.shifts[code]; {
	case code == otherCode && other == nil:
		return fail(bad("Give the times for this day"))
	case code != otherCode && other != nil:
		return fail(bad("Times are given only with Other time for this day"))
	case !isShift && code != "WO" && code != "PH" && code != otherCode:
		return fail(bad("%s is not a shift, WO or PH", code))
	}
	if sh, ok := ts.shifts[code]; ok && !sh.RunsFor(e.DeptID, e.LocID) {
		return fail(bad("%s", shiftNotForMsg(code, e.Dept, e.Location)))
	}
	if date < e.DOJ || (e.LWD != nil && date > *e.LWD) {
		return fail(bad("%s is outside %s's employment", longDate(date), e.Name))
	}
	m, err := month()
	if err != nil {
		return fail(err)
	}
	if l := m.leaveOn(e, date, false); l != nil && !m.unpaid(l.Type) {
		return fail(bad("%s is approved leave. Change it by cancelling the leave", longDate(date)))
	}
	if m.callOf(e.ID, date) != nil && m.callIn(e, p, date) && via != "deviation" { // roster_day.go
		return fail(bad("%s is called to work on %s. Remove the call to work first, then change the roster", e.Name, longDate(date)))
	}
	now = rosterCell{Code: m.rosterCode(e, p, date)}
	if x, ok := m.roster[e.ID][date]; ok {
		now.Other = x.Other
	}
	newSh := other
	if newSh == nil && !rosterCodes[code] {
		if sh, _, ok := ts.shiftOn(code, date); ok {
			newSh = &sh
		}
	}
	if newSh != nil && m.isHolidayCell(e, p, date) && via != "deviation" {
		// A shift on a holiday calls the person to work (owner, 24 Nov 2026), never a plain working day.
		return fail(bad("%s is a holiday for %s. A shift on a holiday calls them to work: choose Called to work on the roster with the shift and the reason, or use Attendance › Called to work",
			longDate(date), e.Name))
	}
	if now.Code == code && (code != otherCode || sameTiming(now.Other, other)) {
		return now, true, "", false, nil
	}
	lead := policy.DayDiff(today(), date)
	if lead < 0 && p.S.RS.Past == "lock" && via != "deviation" {
		return fail(bad("%s has passed and is locked. If the day was worked differently, the employee regularizes with the reason Roster changed", longDate(date)))
	}
	late = lead < p.S.RS.Lead && via != "deviation"
	if late && p.S.RS.Reason && strings.TrimSpace(why) == "" {
		return fail(bad("This is a late change, inside %d days of the date. Give a reason", p.S.RS.Lead))
	}
	if newSh != nil && p.S.Att.MinRest > 0 {
		need := int(p.S.Att.MinRest * 60)
		label := code
		if other != nil {
			label = otherCode + " " + other.Timing()
		}
		prev, next := policy.AddDays(date, -1), policy.AddDays(date, 1)
		pc, nc := m.rosterCode(e, p, prev), m.rosterCode(e, p, next)
		for _, pair := range []struct {
			a, b   *attend.Shift
			la, lb string
		}{{m.dayShift(e, prev, pc), newSh, pc, label}, {newSh, m.dayShift(e, next, nc), label, nc}} {
			if pair.a == nil || pair.b == nil {
				continue
			}
			if g, _ := attend.RestBetween(pair.a, pair.b); g < need {
				msg := fmt.Sprintf("%s then %s leaves %.1f h of rest, the minimum is %v h", pair.la, pair.lb, math.Max(0, float64(g)/60), p.S.Att.MinRest)
				if p.S.Att.RestAct == "block" && via != "deviation" {
					return fail(bad("Not changed. %s", msg))
				}
				warn = msg
			}
		}
	}
	return now, false, warn, late, nil
}

// rosterOverlay records a checked change in m as rosterSet would store it (the policy default is not stored),
// so the next check of the same row sees it.
func (m *monthData) rosterOverlay(e *emp, p *workPolicy, date, code string) {
	if code == p.S.DefCode(e.ID, m.ts.baseShift(e), date, m.ts.holidays(p, e.CityID)) {
		delete(m.roster[e.ID], date)
		return
	}
	if m.roster[e.ID] == nil {
		m.roster[e.ID] = map[string]rosterCell{}
	}
	m.roster[e.ID][date] = rosterCell{Code: code}
}

// rosterCell changes one cell: a shift, WO or PH; other time for the day (code OT with time); or, with call, a call
// to work with the shift on a holiday or a day of leave (roster_day.go), which needs the right of Attendance › Called
// to work (att.records at Edit) rather than the roster's.
func (s *Server) rosterCell(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		EmployeeID string     `json:"employeeId"`
		Date       string     `json:"date"`
		Code       string     `json:"code"`
		Why        string     `json:"why"`
		Time       *otherBody `json:"time"`   // with code OT: other time for this day
		Call       bool       `json:"call"`   // called to work with the shift (code) on a holiday or leave day
		Reason     string     `json:"reason"` // the reason of the call
	}
	if c.Level("ros.roster") < 2 && c.Level("att.records") < 2 {
		return nil, forbidden("")
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	right := "ros.roster"
	if b.Call {
		right = "att.records"
	}
	if err := c.Need(right, 2); err != nil {
		return nil, err
	}
	if !isDate(b.Date) {
		return nil, bad("Give the date")
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, b.EmployeeID)
	if err != nil {
		return nil, err
	}
	code := strings.ToUpper(strings.TrimSpace(b.Code))
	if b.Call {
		var done string
		err = s.st.Tx(ctx, func(tx store.Q) error {
			var err error
			done, err = s.rosterCall(ctx, tx, c, ts, e, b.Date, code, b.Reason)
			return err
		})
		return map[string]any{"call": done}, err
	}
	var other *attend.Shift
	if b.Time != nil || code == otherCode {
		if b.Time == nil || code != otherCode {
			return nil, bad("Other time for this day needs the code OT and the times")
		}
		sh, err := otherShift(b.Time)
		if err != nil {
			return nil, err
		}
		other = &sh
	}
	var warn string
	var late bool
	err = s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		warn, late, err = s.rosterSetDay(ctx, tx, c, ts, e, b.Date, code, other, b.Why, "")
		return err
	})
	return map[string]any{"warning": warn, "late": late}, err
}

func (s *Server) rosterPublish(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.publish", 2); err != nil {
		return nil, err
	}
	var b struct {
		Location   string `json:"location"`
		Department string `json:"department"`
		Week       string `json:"week"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !isDate(b.Week) || b.Location == "" {
		return nil, bad("Choose the property and week")
	}
	ctx := r.Context()
	if c.Scope.Access == "locs" && !containsStr(c.Scope.Locations, b.Location) {
		return nil, forbidden("This property is outside your access")
	}
	wk := weekStart(b.Week)
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	lead := 2
	if emps, _ := scopedEmps(ctx, s.st.DB, c, bson.D{{Key: "status", Value: "Active"}, {Key: "location_id", Value: b.Location}}); len(emps) > 0 {
		p, _ := ts.policyFor(emps[0])
		lead = p.S.RS.Lead
	}
	late := policy.DayDiff(today(), wk) < lead
	return map[string]bool{"late": late}, s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("roster_publish").Upsert(ctx, bson.D{{Key: "location_id", Value: b.Location}, {Key: "department", Value: or(b.Department, "All")}, {Key: "week_start", Value: wk}},
			bson.M{"late": late, "by_name": c.Name + " (" + c.Role + ")", "at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Roster published for the week of %s%s", longDate(wk), map[bool]string{true: " (late)", false: ""}[late]), "roster", b.Location, nil, b)
	})
}

// rosterAlerts flags property · department pairs (5 changes or more) whose late-change share passes
// the limit in the work policy (rs.alert, 30% by default) of the people changed (prototype rosAlerts).
func (s *Server) rosterAlerts(ctx context.Context, c *Ctx, ym string) ([]map[string]any, error) {
	out := []map[string]any{}
	logs, err := rosterMonthLog(ctx, s.st.DB, c, ym)
	if err != nil {
		return out, err
	}
	// The changes in scope at a property that still exists, by property · department.
	type agg struct {
		key       any
		n, late   int64
		minEmp    string
		hasMinEmp bool
	}
	groups, order := map[rosterKey]*agg{}, []rosterKey{}
	for _, g := range logs.scoped {
		l := logs.locs[str(g["location_id"])]
		if l == nil {
			continue
		}
		k := rosterJoin(l["name"], g["department"])
		x := groups[k]
		if x == nil {
			x = &agg{key: k.value()}
			groups[k], order = x, append(order, k)
		}
		x.n++
		if g["late"] == true {
			x.late++
		}
		if e := str(g["employee_id"]); !x.hasMinEmp || e < x.minEmp {
			x.minEmp, x.hasMinEmp = e, true
		}
	}
	rows := []store.Row{}
	for _, k := range order {
		x := groups[k]
		if x.n < 5 {
			continue
		}
		rows = append(rows, store.Row{"key": x.key, "n": x.n, "late": x.late, "pct": math.Round(100 * float64(x.late) / float64(x.n)), "emp": x.minEmp})
	}
	if len(rows) == 0 {
		return out, nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := rows[i]["pct"].(float64), rows[j]["pct"].(float64); a != b {
			return a > b
		}
		return str(rows[i]["key"]) < str(rows[j]["key"])
	})
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return out, err
	}
	for _, x := range rows {
		limit := 30
		if e, err := loadEmp(ctx, s.st.DB, x["emp"].(string)); err == nil {
			if p, _ := ts.policyFor(e); p.S.RS.Alert > 0 {
				limit = p.S.RS.Alert
			}
		}
		if pct, _ := x["pct"].(float64); pct > float64(limit) {
			delete(x, "emp")
			x["limit"] = limit
			out = append(out, x)
		}
	}
	return out, nil
}

// rosterLog is a month's roster changes made by people (not from approved deviations): all of them, and those
// of employees in the caller's scope, with the locations they name.
type rosterLog struct {
	all, scoped []store.Row
	locs        map[string]store.Row
}

func rosterMonthLog(ctx context.Context, q store.Q, c *Ctx, ym string) (*rosterLog, error) {
	all, err := q.C("roster_log").Find(ctx, bson.D{{Key: "date", Value: timectxMonth(ym)}, {Key: "by_dev", Value: false}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	empIDs, locIDs := []string{}, []string{}
	for _, g := range all {
		empIDs, locIDs = append(empIDs, str(g["employee_id"])), append(locIDs, str(g["location_id"]))
	}
	in, err := timectxInScope(ctx, q, c, empIDs)
	if err != nil {
		return nil, err
	}
	locs, err := timectxByID(ctx, q, "location", locIDs, "name")
	if err != nil {
		return nil, err
	}
	l := &rosterLog{all: all, scoped: []store.Row{}, locs: locs}
	for _, g := range all {
		if in[str(g["employee_id"])] {
			l.scoped = append(l.scoped, g)
		}
	}
	return l, nil
}

// rosterKey is a group key that may be null (a key with a missing part is null).
type rosterKey struct {
	null bool
	s    string
}

func (k rosterKey) value() any {
	if k.null {
		return nil
	}
	return k.s
}

// rosterJoin is name || ' · ' || department.
func rosterJoin(name, dept any) rosterKey {
	a, ok1 := name.(string)
	b, ok2 := dept.(string)
	if !ok1 || !ok2 {
		return rosterKey{null: true}
	}
	return rosterKey{s: a + " · " + b}
}

// rosterRole is the role in a "Name (Role)" signature.
var rosterRole = regexp.MustCompile(`\(([^()]+)\)$`)

// ---------- Discipline report (spec 11.5) ----------

func (s *Server) discipline(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ym := or(r.URL.Query().Get("ym"), today()[:7])
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	logs, err := rosterMonthLog(ctx, s.st.DB, c, ym)
	if err != nil {
		return nil, err
	}
	// group counts the changes in scope by a key: changes, late ones, people changed and the most common reason
	// (the first in order among equally common ones), most late changes first.
	group := func(keyOf func(g store.Row) rosterKey) []store.Row {
		type agg struct {
			key     rosterKey
			n, late int64
			people  map[string]bool
			whys    map[string]int
		}
		groups, order := map[rosterKey]*agg{}, []rosterKey{}
		for _, g := range logs.scoped {
			k := keyOf(g)
			x := groups[k]
			if x == nil {
				x = &agg{key: k, people: map[string]bool{}, whys: map[string]int{}}
				groups[k], order = x, append(order, k)
			}
			x.n++
			if g["late"] == true {
				x.late++
			}
			x.people[str(g["employee_id"])] = true
			if w, ok := g["why"].(string); ok {
				x.whys[w]++
			}
		}
		rows := []store.Row{}
		for _, k := range order {
			x := groups[k]
			var reason any
			best := 0
			for w, n := range x.whys {
				if n > best || (n == best && w < reason.(string)) {
					reason, best = w, n
				}
			}
			rows = append(rows, store.Row{"key": k.value(), "n": x.n, "late": x.late, "people": int64(len(x.people)), "reason": reason})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a["late"].(int64) != b["late"].(int64) {
				return a["late"].(int64) > b["late"].(int64)
			}
			if a["n"].(int64) != b["n"].(int64) {
				return a["n"].(int64) > b["n"].(int64)
			}
			return str(a["key"]) < str(b["key"])
		})
		return rows
	}
	// By property · department (a change at a location that no longer exists is counted without a key).
	byPlace := group(func(g store.Row) rosterKey {
		var name any
		if l := logs.locs[str(g["location_id"])]; l != nil {
			name = l["name"]
		}
		return rosterJoin(name, g["department"])
	})
	// By the role of who made the change (prototype vRosDisc "By who made the change": Role); the log
	// records "Name (Role)".
	byWho := group(func(g store.Row) rosterKey {
		by := str(g["by_name"])
		if m := rosterRole.FindStringSubmatch(by); m != nil {
			return rosterKey{s: m[1]}
		}
		return rosterKey{s: by}
	})
	var regAll, regRoster, pubLate int64
	// Regularizations of the month for people in scope, and those for a roster change.
	if regs, err := s.st.DB.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "regularize"}, {Key: "payload.date", Value: bson.D{{Key: "$regex", Value: "^" + regexp.QuoteMeta(ym)}}}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "payload.reason", Value: 1}})); err == nil {
		who := []string{}
		for _, r := range regs {
			who = append(who, str(r["employee_id"]))
		}
		if in, err := timectxInScope(ctx, s.st.DB, c, who); err == nil {
			for _, r := range regs {
				if !in[str(r["employee_id"])] {
					continue
				}
				regAll++
				if pl, _ := r["payload"].(map[string]any); pl["reason"] == "Roster changed" {
					regRoster++
				}
			}
		}
	}
	pubLate, _ = s.st.DB.C("roster_publish").Count(ctx, bson.D{{Key: "week_start", Value: timectxMonth(ym)}, {Key: "late", Value: true}})

	// Scores: employees (the month's start, 100 or pro-rata, less Not OK × points) and the managers who plan
	// their rosters.
	emps, err := scopedEmps(ctx, s.st.DB, c, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}})
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	type mgr struct {
		ID, Name                       string
		Team, Days, Dev, OK, No, Pend  int
		Changes, Late, Alert, Planning int
		Share                          float64
		Flag                           bool
	}
	mgrs := map[string]*mgr{}
	people := []map[string]any{}
	names := map[string]string{}
	for _, e := range emps {
		names[e.ID] = e.Name
	}
	changes := map[string][2]int{} // everyone's changes of the month, made by people
	for _, g := range logs.all {
		x := changes[str(g["employee_id"])]
		x[0]++
		if g["late"] == true {
			x[1]++
		}
		changes[str(g["employee_id"])] = x
	}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		days, dev, ok, no, pend := 0, 0, 0, 0, 0
		for d := from; d <= to && d <= today(); d = policy.AddDays(d, 1) {
			if d < e.DOJ || (e.LWD != nil && d > *e.LWD) {
				continue
			}
			if c := m.planned(e, p, d).Code; c != "WO" && c != "PH" && c != "L" {
				days++
			}
			v, x := m.deviation(e, p, d)
			if v == nil {
				continue
			}
			dev++
			switch {
			case x == nil:
				pend++
			case x.Status == "OK":
				ok++
			default:
				no++
			}
		}
		sc := scoreFor(e, p, ym, no)
		people = append(people, map[string]any{"id": e.ID, "name": e.Name, "location": e.Location, "department": e.Dept, "days": days, "dev": dev, "ok": ok, "no": no, "pending": pend,
			"score": sc.Score, "band": sc.Band, "start": sc.Start, "startNote": sc.Note})
		if e.ManagerID != nil {
			g := mgrs[*e.ManagerID]
			if g == nil {
				g = &mgr{ID: *e.ManagerID, Name: names[*e.ManagerID], Alert: p.S.RS.MgrAlert}
				mgrs[*e.ManagerID] = g
			}
			g.Team++
			g.Days += days
			g.Dev += dev
			g.OK += ok
			g.No += no
			g.Pend += pend
			g.Changes += changes[e.ID][0]
			g.Late += changes[e.ID][1]
		}
	}
	// Managers outside the list shown get their name from the employee record.
	missing := []string{}
	for _, g := range mgrs {
		if g.Name == "" {
			missing = append(missing, g.ID)
		}
	}
	if found, err := timectxByID(ctx, s.st.DB, "employee", missing, "name"); err == nil {
		for _, g := range mgrs {
			if x := found[g.ID]; g.Name == "" && x != nil {
				g.Name = str(x["name"])
			}
		}
	}
	mgrOut := []map[string]any{}
	for _, g := range mgrs {
		if g.Days > 0 {
			g.Share = math.Round(float64(g.Dev)/float64(g.Days)*1000) / 10
		}
		lateShare := 0.0
		if g.Changes > 0 {
			lateShare = float64(g.Late) / float64(g.Changes) * 100
		}
		g.Planning = int(math.Max(0, math.Round(100-2*g.Share-lateShare/2)))
		g.Flag = g.Share > float64(g.Alert)
		mgrOut = append(mgrOut, map[string]any{"id": g.ID, "name": g.Name, "team": g.Team, "days": g.Days, "dev": g.Dev, "ok": g.OK, "no": g.No, "pending": g.Pend,
			"share": g.Share, "changes": g.Changes, "late": g.Late, "planning": g.Planning, "flag": g.Flag})
	}
	sort.Slice(people, func(i, j int) bool { return people[i]["score"].(int) < people[j]["score"].(int) })
	sort.Slice(mgrOut, func(i, j int) bool { return mgrOut[i]["planning"].(int) < mgrOut[j]["planning"].(int) })
	var total, late int64
	for _, x := range byPlace {
		total += x["n"].(int64)
		late += x["late"].(int64)
	}
	alerts, _ := s.rosterAlerts(ctx, c, ym)
	return map[string]any{"ym": ym, "changes": total, "late": late, "pubLate": pubLate, "regAll": regAll, "regRoster": regRoster,
		"byPlace": byPlace, "byWho": byWho, "people": people, "managers": mgrOut, "alerts": alerts}, nil
}
