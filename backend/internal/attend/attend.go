// Package attend turns punches into day records and statuses (spec 11.2-11.5). Pure functions
// ported from the prototype (attRec, netHours, attStatus, devCalc, finalAtt); no database access.
package attend

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/saltstayz/hrms/internal/policy"
)

// Min parses HH:MM into minutes after midnight; ok is false for anything else.
func Min(t string) (int, bool) {
	t = strings.TrimSpace(t)
	var h, m int
	if len(t) < 4 || len(t) > 5 {
		return 0, false
	}
	if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 || t[len(t)-3] != ':' {
		return 0, false
	}
	return h*60 + m, true
}

func HHMM(m int) string {
	m = ((m % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// Shift is a shift master row in minutes. A split shift (owner, 24 Nov 2026) has a second part, Start2–End2, after
// the first part (Start–End) on the same day: 06:00–10:00 + 15:00–19:00. The gap between the parts is the break; it
// never counts as hours worked.
type Shift struct {
	Code         string
	Start, End   int
	Start2, End2 int        // the second part of a split shift; both 0 for a shift in one part
	Maps         []ShiftMap // where it applies: its switched-on Department + Location rows; nil = everywhere (a shift not from the master)
}

// ShiftMap is one row of where a shift applies (owner, 24 Nov 2026): a department and a location id, "" for all.
type ShiftMap struct{ Dept, Loc string }

// Split: the shift has a second part.
func (s Shift) Split() bool { return s.End2 > s.Start2 && s.Start2 > 0 }

// LastEnd is the end of the shift's last part (minutes after midnight, before 24:00 like End).
func (s Shift) LastEnd() int {
	if s.Split() {
		return s.End2
	}
	return s.End
}

// Timing is the shift's timings as text: "09:00–18:00", or "06:00–10:00 + 15:00–19:00" for a split shift.
func (s Shift) Timing() string { return TimingText(HHMM(s.Start), HHMM(s.End), s.part2()) }

func (s Shift) part2() string {
	if !s.Split() {
		return ""
	}
	return HHMM(s.Start2) + "–" + HHMM(s.End2)
}

// TimingText is a shift's timings from its stored times: "09:00–18:00", with " + 15:00–19:00" when part2 is given.
func TimingText(start, end, part2 string) string {
	t := start + "–" + end
	if part2 != "" {
		t += " + " + part2
	}
	return t
}

// Parts are the shift's parts in minutes from its start's midnight (the end past midnight when it ends next day).
func (s Shift) Parts() []Window {
	if s.Split() {
		return []Window{{s.Start, s.End}, {s.Start2, s.End2}}
	}
	e := s.End
	if e <= s.Start {
		e += 1440
	}
	return []Window{{s.Start, e}}
}

// RunsAt: some row of the shift is at the location (or at all locations), for any department.
func (s Shift) RunsAt(loc string) bool {
	if s.Maps == nil {
		return true
	}
	for _, m := range s.Maps {
		if m.Loc == "" || m.Loc == loc {
			return true
		}
	}
	return false
}

// RunsFor: the shift is offered to, and valid for, an employee of the department at the location: one of its rows
// matches both (a side left "" in the row matches any).
func (s Shift) RunsFor(dept, loc string) bool {
	if s.Maps == nil {
		return true
	}
	for _, m := range s.Maps {
		if (m.Dept == "" || m.Dept == dept) && (m.Loc == "" || m.Loc == loc) {
			return true
		}
	}
	return false
}

// Hours is the planned working length of the shift (a split shift's two parts added together, without the gap).
func (s Shift) Hours() float64 {
	if s.Split() {
		return float64(s.End-s.Start+s.End2-s.Start2) / 60
	}
	return float64(s.length()) / 60
}

// length is the planned span in minutes, from the start to the end of the last part; a shift whose end is not after
// its start ends the next day.
func (s Shift) length() int {
	b := s.LastEnd()
	if b <= s.Start {
		b += 1440
	}
	return b - s.Start
}

// Raw is the stored day: first IN and last OUT, as uploaded or punched.
type Raw struct {
	In, Out string
	OutDate string // set only when the out is on a later date than the next day
	Src     string
}

// Rec is the day after the punch rules: an over-long previous day can carry its out punch in.
type Rec struct {
	In, Out string
	Src     string
	Carried bool   // the check-in came from yesterday's out punch
	Orphan  string // today's out was dropped because the span was too long
}

func (r Rec) Empty() bool { return r.In == "" && r.Out == "" }

// SpanH is the in→out span in hours, using the out date when it is given.
func SpanH(r *Raw, date string) float64 {
	if r == nil || r.In == "" || r.Out == "" {
		return 0
	}
	a, ok1 := Min(r.In)
	b, ok2 := Min(r.Out)
	if !ok1 || !ok2 {
		return 0
	}
	if r.OutDate != "" && r.OutDate > date {
		return float64(policy.DayDiff(date, r.OutDate)*1440+b-a) / 60
	}
	if b <= a {
		b += 1440
	}
	return float64(b-a) / 60
}

func over(r *Raw, date string, maxSpan float64) bool {
	h := SpanH(r, date)
	return h > 8 && maxSpan > 0 && h > maxSpan
}

// Day applies the punch rules (attRec): a previous day whose in→out span passes the limit gives
// its out punch to today as the check-in; a too-long span today drops today's out.
func Day(prev *Raw, cur *Raw, date string, maxSpan float64) *Rec {
	var r *Rec
	if cur != nil {
		r = &Rec{In: cur.In, Out: cur.Out, Src: cur.Src}
	}
	pd := policy.AddDays(date, -1)
	if prev != nil && prev.Out != "" && over(prev, pd, maxSpan) {
		od := prev.OutDate
		if od == "" { // no out date: the out is on the day of the in, or the next day when its time is not later
			od = pd
			if a, ok1 := Min(prev.In); ok1 {
				if b, ok2 := Min(prev.Out); ok2 && b <= a {
					od = date
				}
			}
		}
		if od == date {
			ts := []string{prev.Out}
			if cur != nil {
				for _, t := range []string{cur.In, cur.Out} {
					if t != "" {
						ts = append(ts, t)
					}
				}
			}
			sort.Slice(ts, func(i, j int) bool { a, _ := Min(ts[i]); b, _ := Min(ts[j]); return a < b })
			src := prev.Src
			if cur != nil {
				src = cur.Src
			}
			r = &Rec{In: ts[0], Src: src, Carried: true}
			if len(ts) > 1 {
				r.Out = ts[len(ts)-1]
			}
		}
	}
	if r != nil && r.Out != "" {
		x := Raw{In: r.In, Out: r.Out}
		if cur != nil && !r.Carried {
			x.OutDate = cur.OutDate
		}
		if over(&x, date, maxSpan) {
			r.Orphan, r.Out = r.Out, ""
		}
	}
	return r
}

// NetHours: early time inside the allowance is not counted (unless the policy counts it). No break is
// taken off: hours are in to out.
func NetHours(p policy.Settings, a, b int, plannedStart *int) float64 {
	if b <= a {
		b += 1440
	}
	if plannedStart != nil && p.RS.EarlyCount != "yes" {
		sg := ((a-*plannedStart+2160)%1440 + 1440) % 1440
		sg -= 720
		if sg < 0 && -sg <= p.RS.EarlyIn {
			a += -sg
		}
	}
	return math.Max(0, float64(b-a)/60)
}

// Plan is what the roster says for the day.
type Plan struct {
	Code  string // shift code, WO, PH or L (paid leave)
	Shift *Shift // the planned shift when Code is a shift (the version in effect on the date)
	Rules *Rules // that shift version's attendance rules; nil = the work policy decides
	// Short are the approved short leaves of the day (owner, 20 Nov 2026): each window is excused, so arriving after
	// it, leaving at its start or working fewer hours by it is no late mark, early exit or short-hours day.
	Short []Window
}

// Window is a time window of a day, in minutes after midnight (To after From).
type Window struct{ From, To int }

// ShortMins is the minutes of short leave on the day.
func (pl Plan) ShortMins() int {
	n := 0
	for _, w := range pl.Short {
		if w.To > w.From {
			n += w.To - w.From
		}
	}
	return n
}

// ExcusedStart is the time the late mark is measured from with the day's short leave: a window that starts by the
// start (within tol minutes) moves it to the window's end, and a window starting by that end moves it on again.
func (pl Plan) ExcusedStart(start, tol int) int {
	for moved := true; moved; {
		moved = false
		for _, w := range pl.Short {
			if w.To > start && w.From <= start+tol {
				start, moved = w.To, true
			}
		}
	}
	return start
}

// excusedEnd is the shift end with the day's short leave: a window that reaches the end (within tol minutes) moves it
// back to the window's start.
func (pl Plan) excusedEnd(end, tol int) int {
	for moved := true; moved; {
		moved = false
		for _, w := range pl.Short {
			if w.From < end && w.To >= end-tol {
				end, moved = w.From, true
			}
		}
	}
	return end
}

// shortCredit is the hours of short leave inside the shift that were not worked (the worked span a–b, b after a):
// they count as worked.
func (pl Plan) shortCredit(a, b int) float64 {
	if pl.Shift == nil || len(pl.Short) == 0 {
		return 0
	}
	s, e := pl.Shift.Start, pl.Shift.Start+pl.Shift.length()
	ov := func(x1, x2, y1, y2 int) int { return max(0, min(x2, y2)-max(x1, y1)) }
	m := 0
	for _, w := range pl.Short {
		f, t := w.From, w.To
		if t <= f {
			continue
		}
		if f < s && f+1440 < e { // a night shift: the window is after midnight
			f, t = f+1440, t+1440
		}
		for _, p := range pl.Shift.Parts() { // a split shift's gap is no work time, so short leave there credits nothing
			in := ov(f, t, p.From, p.To)
			m += in - min(in, ov(max(f, p.From), min(t, p.To), a, b))
		}
	}
	return float64(m) / 60
}

// Status is the daily status (attStatus). With override, lateStart replaces the planned start
// (a Shift deviation: measured against the shift actually worked; nil for Custom = no late mark).
// The day's shift rules (plan.Rules) are laid over the policy; without rules this is the policy's status.
func Status(p policy.Settings, plan Plan, rec *Rec, lateStart *int, override bool) (string, string) {
	ev := Effective(&p, plan.Rules).Eval(plan, rec, lateStart, override)
	return ev.Status, ev.Tone
}

// ---------- Rest and deviations ----------

// RestBetween is the rest in minutes between shift c1 on one day and c2 on the next (from the end of c1's last part).
// The shift rule "Consider shift in" was removed (owner, 24 Nov 2026): every shift is recorded on the day it starts.
func RestBetween(c1, c2 *Shift) (int, bool) {
	if c1 == nil || c2 == nil {
		return 0, false
	}
	return 1440 + c2.Start - (c1.Start + c1.length()), true
}

type Deviation struct {
	Planned string   `json:"planned"`
	Actual  string   `json:"actual"` // a shift code, or Custom
	In      string   `json:"in"`
	Out     string   `json:"out"`
	Hours   float64  `json:"hours"`
	PlanH   float64  `json:"planH"`
	Kinds   []string `json:"kinds"`
	// Early: the check-in was before the planned start. Arriving early is never a late mark, whichever shift's start
	// is nearest (attendance rules test, 24 Nov 2026: 09:29 on a 10:00 shift was Late against a 09:00 shift).
	Early bool `json:"-"`
}

func circ(a, b int) int {
	x := a - b
	if x < 0 {
		x = -x
	}
	x %= 1440
	if 1440-x < x {
		return 1440 - x
	}
	return x
}

// Nearest is the shift at the location whose start is closest to the check-in, within tol.
func Nearest(shifts []Shift, loc string, in, tol int) string {
	best, bd := "Custom", 1<<30
	for _, s := range shifts {
		if !s.RunsAt(loc) {
			continue
		}
		if d := circ(in, s.Start); d < bd {
			best, bd = s.Code, d
		}
	}
	if bd <= tol {
		return best
	}
	return "Custom"
}

// Deviate compares the roster with the punches (devCalc). nil when they agree. Only a working day with a
// planned shift can deviate: arriving outside the allowed window (Shift) or leaving early (Early exit). Work on
// a weekly off, holiday or leave day, extra hours and short rest are not deviations (rest is checked on the roster).
func Deviate(p policy.Settings, plan Plan, rec *Rec, date, loc string, shifts []Shift) *Deviation {
	if rec == nil || rec.In == "" || rec.Out == "" {
		return nil
	}
	a, ok1 := Min(rec.In)
	b, ok2 := Min(rec.Out)
	if !ok1 || !ok2 {
		return nil
	}
	if b <= a {
		b += 1440
	}
	ps := plan.Shift
	if ps == nil {
		return nil
	}
	v := &Deviation{Planned: plan.Code, Actual: plan.Code, In: rec.In, Out: rec.Out, Hours: math.Round(float64(b-a)/60*10) / 10, PlanH: ps.Hours()}
	sg := ((a-ps.Start+2160)%1440+1440)%1440 - 720
	if sg > 0 && len(plan.Short) > 0 { // arriving after an approved short leave at the start is on time (owner, 20 Nov 2026)
		sg = max(0, ((a-plan.ExcusedStart(ps.Start, p.RS.DevTol)+2160)%1440+1440)%1440-720)
	}
	if p.RS.DevStart && (sg < -p.RS.EarlyIn || sg > p.RS.DevTol) {
		v.Kinds = append(v.Kinds, "Shift")
		v.Actual = Nearest(shifts, loc, a, p.RS.DevTol)
		v.Early = sg < 0
	}
	if p.RS.OutTol > 0 && !contains(v.Kinds, "Shift") {
		end := ps.LastEnd()      // a split shift ends with its second part
		if len(plan.Short) > 0 { // leaving for an approved short leave at the end is no early exit
			end = plan.excusedEnd(end, p.RS.OutTol)
		}
		so := ((b%1440-end+2160)%1440+1440)%1440 - 720
		if so < -p.RS.OutTol && so > -720 {
			v.Kinds = append(v.Kinds, "Early exit")
		}
	}
	if len(v.Kinds) == 0 {
		return nil
	}
	return v
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// LateStartFor: with a Shift deviation the late mark is measured against the actual shift (devLateStart).
func LateStartFor(v *Deviation, shifts []Shift) (start *int, override bool) {
	if v == nil || !contains(v.Kinds, "Shift") {
		return nil, false
	}
	if v.Early { // came before the planned start: no late mark, attendance follows hours
		return nil, true
	}
	for _, s := range shifts {
		if s.Code == v.Actual {
			st := s.Start
			return &st, true
		}
	}
	return nil, true // Custom: no late mark, attendance follows hours
}

// ---------- Month (finalAtt) ----------

// DayIn is everything known about one day of the month.
type DayIn struct {
	Date     string
	Plan     Plan
	Rec      *Rec
	Leave    string // approved leave type on this day
	Unpaid   bool   // the leave type is unpaid
	Half     bool   // a half-day leave: the other half is worked (Rec), or follows the no-punch rule at half weight
	HalfOf   string // which half is leave: "first" or "second" ("" for a request made before the choice)
	Before   bool   // before joining
	After    bool   // after the last working day
	Future   bool
	LateFrom *int    // start of the shift actually worked, when a Shift deviation exists
	Override bool    // a Shift deviation exists (LateFrom nil = Custom, no late mark)
	AgeDays  int     // days since the date, for the regularization window
	OptHol   bool    // an optional holiday on the employee's calendar (for the shortfall inclusions)
	Dev      *DevDay // the work policy's deviation allowances on the day (devallow.go)
	Called   bool    // called to work on this holiday or leave day (called.go); CallPlan and Rec decide if it was worked
	CallPlan Plan    // the shift the called day is measured against
}

type DayOut struct {
	Date   string  `json:"date"`
	Status string  `json:"status"`
	Tone   string  `json:"tone"`
	Code   string  `json:"code"`
	LOP    float64 `json:"lop"`
	In     string  `json:"in,omitempty"`
	Out    string  `json:"out,omitempty"`
	Src    string  `json:"src,omitempty"`
	Leave  string  `json:"leave,omitempty"`
	Note   string  `json:"note,omitempty"` // how a short-hours day was settled
	// Called to work on a holiday or leave day and worked (called.go): Present, and Returned days of the date's leave
	// (1, or 0.5 for a half-day leave) go back to the balance.
	Called   bool    `json:"called,omitempty"`
	Returned float64 `json:"returned,omitempty"`
	// Reg: beyond a deviation allowance with an action (absent, half or present; devallow.go): the day waits for a
	// regularization of the date.
	Reg string `json:"reg,omitempty"`
}

// Paid is the day's paid days for leave earned by working days (owner, 24 Nov 2026, evening: "Working days means Paid
// days for leaves"): 1 less the day's loss of pay as settled above. Present, late, called to work, paid leave, a week
// off and a holiday 1; a half day 0.5; half a day of paid leave with the other half worked 1; absent, loss of pay leave
// and a no-punch day once it has turned to loss of pay 0 (inside the regularization window it has none, so it counts
// until then); a day beyond a deviation allowance by its loss of pay. Days outside employment (before joining, after
// the last working day) and upcoming days are 0; callers count only days that are over. The month's late-mark half
// days (Month.LateOver) belong to no day and are not taken off.
func (o DayOut) Paid() float64 {
	if o.Code == "–" || o.Status == "Upcoming" {
		return 0
	}
	return math.Max(0, 1-o.LOP)
}

type Month struct {
	Days     []DayOut       `json:"days"`
	Counts   map[string]int `json:"counts"`
	LOP      float64        `json:"lop"`
	Outside  int            `json:"outside"`
	LateOver int            `json:"lateOver"`
	PaidDays float64        `json:"paidDays"`
	DaysN    int            `json:"daysN"`
}

// FinalMonth works out each day's status and the loss of pay (spec 11.4). Every day follows the work policy p: the
// shift's own attendance rules were removed (24 Nov 2026), so the short-hours settling below (shorts, extras,
// adjust) never has a day to settle.
func FinalMonth(p policy.Settings, days []DayIn) Month {
	m := Month{Counts: map[string]int{}, DaysN: len(days)}
	var shorts []*shortDay
	var extras []extraHours
	ruled := false // without any shift rules there is no short-hours day to cover
	for i := range days {
		ruled = ruled || days[i].Plan.Rules != nil
	}
	for _, d := range days {
		o := DayOut{Date: d.Date}
		if d.Rec != nil {
			o.In, o.Out, o.Src = d.Rec.In, d.Rec.Out, d.Rec.Src
		}
		h, called := calledWorked(p, d) // called to work on a holiday or leave day and worked (called.go)
		d.Plan, d.LateFrom, d.Override = d.Dev.Lay(d.Plan, d.LateFrom, d.Override)
		switch {
		case d.Before:
			o.Status, o.Tone, o.Code = "Before joining", "n", "–"
			m.Outside++
		case d.After:
			o.Status, o.Tone, o.Code = "After last working day", "n", "–"
			m.Outside++
		case called:
			o.settleCalled(d, h)
			m.Counts["Present"]++
			m.Counts["Called to work"]++
		case d.Leave != "":
			o.Status, o.Leave = d.Leave, d.Leave
			if d.Unpaid {
				o.Tone, o.Code, o.LOP = "bad", "LOP", 1
				m.Counts["Loss of pay leave"]++
			} else {
				o.Tone, o.Code = "info", "L"
				m.Counts["Paid leave"]++
			}
			if d.Half { // spec 11.4: half a day of leave; the other half is worked when punched (the shift's half-day rule)
				o.Status += " (half day" + map[string]string{"first": ", first half", "second": ", second half"}[d.HalfOf] + ")"
				if d.Unpaid {
					o.LOP = 0.5
				}
				switch {
				case (d.Rec == nil || d.Rec.Empty()) && !d.Future && (p.Att.Absent == "lopnow" || (p.Att.Absent == "lop" && d.AgeDays > p.RR.Back)):
					o.LOP += 0.5
					o.Note = "No punch for the other half"
				case d.Rec != nil && !d.Rec.Empty():
					// The other half is present when its hours reach the half-day hours of the day's shift (the
					// work policy's without shift rules); a single punch counts as present, as before (RUN-15).
					e := Effective(&p, d.Plan.Rules)
					if h, ok := e.Counted(d.Plan, d.Rec); ok && h < e.AbsentH && !d.Future {
						o.LOP += 0.5
						o.Status += " + Absent"
						o.Note = "Other half: worked " + HoursText(h) + ", under the half-day hours (" + HoursText(e.AbsentH) + ")"
					} else {
						o.Status += " + Present"
						if ok {
							o.Note = "Other half worked " + HoursText(h)
						}
					}
				}
			}
		case d.Future:
			o.Status, o.Tone = "Upcoming", "n"
			m.Counts["Upcoming"]++
		default:
			e := Effective(&p, d.Plan.Rules)
			ev := e.Eval(d.Plan, d.Rec, d.LateFrom, d.Override)
			o.Status, o.Tone = ev.Status, ev.Tone
			if off := d.Plan.Code == "WO" || d.Plan.Code == "PH"; off && d.Rec != nil && !d.Rec.Empty() {
				// Punched on a week off or holiday: never loss of pay, never a late mark (payroll test RUN-78). The
				// hours still count towards covering short days below.
				o.Status, o.Tone, o.Note = map[bool]string{true: "Week off", false: "Holiday"}[d.Plan.Code == "WO"], "n", "Worked on the day off"
				if ev.Counted {
					o.Note += " (" + HoursText(ev.Hours) + ")"
				}
			}
			if ruled && ev.Counted { // hours that can cover a short-hours day (the week is worked out only when one exists)
				x := extraHours{week: d.Date, h: ev.Hours, normal: math.Max(0, ev.Hours-e.OfficeH)}
				switch {
				case d.Plan.Code == "WO":
					x.kind = "wo"
				case d.Plan.Code == "PH":
					x.kind = "hol"
				case d.OptHol:
					x.kind = "opt"
				}
				if x.normal > 0 || x.kind != "" {
					extras = append(extras, x)
				}
			}
			if sm := d.Plan.ShortMins(); sm > 0 && d.Plan.Code != "WO" && d.Plan.Code != "PH" { // shown as "Short leave 2h"
				o.Leave = "Short leave " + shortHours(sm)
				m.Counts["Short leave"]++
			}
			switch {
			case o.devAct(d, p.Att.Missing, m.Counts): // beyond a deviation allowance: the rule's action settles the day (devallow.go)
			case o.Status == "Present":
				o.Code = "P"
				m.Counts["Present"]++
			case o.Status == "Late":
				o.Code = "LT"
				m.Counts["Late"]++
			case o.Status == "Half day":
				o.Code, o.LOP = "H", 0.5
				m.Counts["Half day"]++
			case o.Status == "Short hours": // settled below, once the extra hours of the week and month are known
				shorts = append(shorts, &shortDay{i: len(m.Days), week: d.Date, short: math.Max(0, e.OfficeH-ev.Hours), e: e, ev: ev})
			case strings.HasPrefix(o.Status, "Absent"):
				o.Code, o.LOP = "A", 1
				m.Counts["No punch"]++
			case o.Status == "No punch":
				o.Code = "N"
				m.Counts["No punch"]++
				if p.Att.Absent == "lopnow" || (p.Att.Absent == "lop" && d.AgeDays > p.RR.Back) {
					o.LOP = 1
				}
			case o.Status == "Week off":
				o.Code = "W"
				m.Counts["Week off"]++
			case o.Status == "Holiday":
				o.Code = "PH"
				m.Counts["Holiday"]++
			default:
				o.Code = "M"
				m.Counts["Missing punch"]++
				switch e.Missing { // the single-punch rule of the day's shift (the policy without one)
				case "half":
					o.LOP = 0.5
				case "absent":
					o.LOP = 1
				}
			}
		}
		o.dayNotes(d, called, m.Counts)
		m.LOP += o.LOP
		m.Days = append(m.Days, o)
	}
	if len(shorts) > 0 {
		pool := make([]*extraHours, len(extras))
		for i := range extras {
			extras[i].week = weekOf(extras[i].week)
			pool[i] = &extras[i]
		}
		for _, s := range shorts {
			s.week = weekOf(s.week)
		}
		adjust(shorts, pool)
		for _, s := range shorts {
			o := &m.Days[s.i]
			txt, prev := "Short by "+HoursText(s.short), o.Note // prev: the deviation allowance notes (dayNotes)
			if s.by != "" {
				o.Status, o.Tone, o.Note = s.ev.FullAs, s.ev.FullTone, txt+", covered by the "+s.by+" adjustment"
			} else {
				o.Status, o.Tone, o.Note = "Half day", "warn", txt+", not covered"
			}
			o.Note = joinNote(o.Note, prev)
			switch o.Status {
			case "Present":
				o.Code = "P"
				m.Counts["Present"]++
			case "Late":
				o.Code = "LT"
				m.Counts["Late"]++
			default:
				o.Code, o.LOP = "H", 0.5
				m.Counts["Half day"]++
				m.LOP += 0.5
			}
			o.regPresent()
		}
	}
	if p.Att.LateAct == "half" && m.Counts["Late"] > p.Att.LateN {
		m.LateOver = m.Counts["Late"] - p.Att.LateN
		m.LOP += float64(m.LateOver) * 0.5
	}
	if in := float64(m.DaysN - m.Outside); m.LOP > in {
		m.LOP = in
	}
	m.PaidDays = float64(m.DaysN-m.Outside) - m.LOP
	return m
}

// shortHours is "2h", "1h 30m" or "45m".
func shortHours(m int) string {
	switch h, mm := m/60, m%60; {
	case mm == 0:
		return fmt.Sprintf("%dh", h)
	case h == 0:
		return fmt.Sprintf("%dm", mm)
	default:
		return fmt.Sprintf("%dh %dm", h, mm)
	}
}

// StartScore is where a month's discipline score starts (spec 11.5): 100 for someone employed the whole
// month, else 100 × days employed in the month ÷ days in the month, rounded. lwd is "" when not leaving.
func StartScore(ym, doj, lwd string) int {
	first, last := ym+"-01", policy.LastDayOf(ym)
	from, to := first, last
	if doj > from {
		from = doj
	}
	if lwd != "" && lwd < to {
		to = lwd
	}
	if to < from {
		return 0
	}
	days, all := policy.DayDiff(from, to)+1, policy.DayDiff(first, last)+1
	return int(math.Round(100 * float64(days) / float64(all)))
}

// Scaled is a 0-100 figure (a score limit) scaled to a month that starts at start.
func Scaled(v, start int) int { return int(math.Round(float64(v) * float64(start) / 100)) }

// Score is the discipline score: the start less the points of each deviation marked Not OK, never below 0.
func Score(start, notOK, pts int) int {
	s := start - notOK*pts
	if s < 0 {
		return 0
	}
	return s
}

// Band reads a score against its start: Good from 90% of it, Watch from 70%, else Poor. A month that
// starts at 100 has the bands 90 and 70; a joiner who starts at 33 is Good from 30.
func Band(score, start int) string {
	switch {
	case score*100 >= 90*start:
		return "Good"
	case score*100 >= 70*start:
		return "Watch"
	}
	return "Poor"
}
