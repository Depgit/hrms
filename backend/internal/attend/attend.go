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

// Shift is a shift master row in minutes.
type Shift struct {
	Code       string
	Start, End int
	Locations  []string // nil = every location
}

func (s Shift) RunsAt(loc string) bool {
	if s.Locations == nil {
		return true
	}
	for _, l := range s.Locations {
		if l == loc {
			return true
		}
	}
	return false
}

// Hours is the planned length of the shift.
func (s Shift) Hours() float64 { return float64(s.length()) / 60 }

// length is the planned length in minutes; a shift whose end is not after its start ends the next day.
func (s Shift) length() int {
	b := s.End
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
		if od == "" {
			od = date
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
}

// Status is the daily status (attStatus). With override, lateStart replaces the planned start
// (a Shift deviation: measured against the shift actually worked; nil for Custom = no late mark).
// The day's shift rules (plan.Rules) are laid over the policy; without rules this is the policy's status.
func Status(p policy.Settings, plan Plan, rec *Rec, lateStart *int, override bool) (string, string) {
	ev := Effective(&p, plan.Rules).Eval(plan, rec, lateStart, override)
	return ev.Status, ev.Tone
}

// ---------- Rest and deviations ----------

// RestBetween is the rest in minutes between shift c1 on one day and c2 on the next.
func RestBetween(c1, c2 *Shift) (int, bool) {
	return RestBetweenRuled(c1, nil, c2, nil)
}

// RestBetweenRuled is RestBetween with each shift's "Consider shift in" rule: a shift recorded on the day before it
// starts (previous) runs a day later than its date, one recorded on the day after it starts (next) a day earlier
// (TIME-231).
func RestBetweenRuled(c1 *Shift, r1 *Rules, c2 *Shift, r2 *Rules) (int, bool) {
	if c1 == nil || c2 == nil {
		return 0, false
	}
	off := func(r *Rules) int {
		if r == nil {
			return 0
		}
		switch r.ShiftIn {
		case "previous":
			return 1440
		case "next":
			return -1440
		}
		return 0
	}
	end := c1.End
	if end <= c1.Start {
		end += 1440
	}
	return 1440 + c2.Start + off(r2) - (end + off(r1)), true
}

type Deviation struct {
	Planned string   `json:"planned"`
	Actual  string   `json:"actual"` // a shift code, or Custom
	In      string   `json:"in"`
	Out     string   `json:"out"`
	Hours   float64  `json:"hours"`
	PlanH   float64  `json:"planH"`
	Kinds   []string `json:"kinds"`
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
	if p.RS.DevStart && (sg < -p.RS.EarlyIn || sg > p.RS.DevTol) {
		v.Kinds = append(v.Kinds, "Shift")
		v.Actual = Nearest(shifts, loc, a, p.RS.DevTol)
	}
	if p.RS.OutTol > 0 && !contains(v.Kinds, "Shift") {
		so := ((b%1440-ps.End+2160)%1440+1440)%1440 - 720
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
	Before   bool   // before joining
	After    bool   // after the last working day
	Future   bool
	LateFrom *int // start of the shift actually worked, when a Shift deviation exists
	Override bool // a Shift deviation exists (LateFrom nil = Custom, no late mark)
	AgeDays  int  // days since the date, for the regularization window
	OptHol   bool // an optional holiday on the employee's calendar (for the shortfall inclusions)
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

// FinalMonth works out each day's status and the loss of pay (spec 11.4). Each day follows its own
// shift's rules (Plan.Rules): the hour thresholds, the single-punch rule and, for short-hours days, the
// weekly and monthly adjustments. Without rules every day follows the policy p, as before.
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
		switch {
		case d.Before:
			o.Status, o.Tone, o.Code = "Before joining", "n", "–"
			m.Outside++
		case d.After:
			o.Status, o.Tone, o.Code = "After last working day", "n", "–"
			m.Outside++
		case d.Leave != "":
			o.Status, o.Leave = d.Leave, d.Leave
			if d.Unpaid {
				o.Tone, o.Code, o.LOP = "bad", "LOP", 1
				m.Counts["Loss of pay leave"]++
			} else {
				o.Tone, o.Code = "info", "L"
				m.Counts["Paid leave"]++
			}
			if d.Half { // spec 11.4: half a day of leave; the other half was worked if anything is punched
				o.Status += " (half day)"
				if d.Unpaid {
					o.LOP = 0.5
				}
				if (d.Rec == nil || d.Rec.Empty()) && !d.Future && (p.Att.Absent == "lopnow" || (p.Att.Absent == "lop" && d.AgeDays > p.RR.Back)) {
					o.LOP += 0.5
					o.Note = "No punch for the other half"
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
			switch {
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
			txt := "Short by " + HoursText(s.short)
			if s.by != "" {
				o.Status, o.Tone, o.Note = s.ev.FullAs, s.ev.FullTone, txt+", covered by the "+s.by+" adjustment"
			} else {
				o.Status, o.Tone, o.Note = "Half day", "warn", txt+", not covered"
			}
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
