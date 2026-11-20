package attend

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/saltstayz/hrms/internal/policy"
)

// Per-shift attendance rules (Masters › Shifts › Set shift). Every rule is optional: a rule left empty
// follows the employee's work policy, so a shift with no rules behaves exactly as before. The rules
// travel with the day's plan (Plan.Rules); Effective lays them over the policy and the engine reads
// the merged values.

// Adj is a weekly or monthly adjustment: extra hours cover short-hours days, up to Hours (HHH:MM).
type Adj struct {
	On    bool   `json:"on"`
	Hours string `json:"hours,omitempty"`
}

// Shortfall says which off-day hours also count as extra hours for the adjustments.
type Shortfall struct {
	WO     bool `json:"wo,omitempty"`     // weekly-off working hours
	Hol    bool `json:"hol,omitempty"`    // holiday working hours
	OptHol bool `json:"optHol,omitempty"` // optional holiday working hours
}

// Rules are a shift version's attendance rules. Times are HH:MM; durations HH:MM (HHH:MM for adjustments).
type Rules struct {
	NextDay       *bool      `json:"nextDay,omitempty"`   // the shift ends on the next calendar day (must agree with the times)
	StartFrom     string     `json:"startFrom,omitempty"` // shift | punch
	EndFrom       string     `json:"endFrom,omitempty"`   // shift | punch
	AddMissingOut *bool      `json:"addMissingOut,omitempty"`
	ForcedOut     string     `json:"forcedOut,omitempty"`   // time of day used for a missing out punch
	EarlyInMax    string     `json:"earlyInMax,omitempty"`  // punch window before the start
	LateOutMax    string     `json:"lateOutMax,omitempty"`  // punch window after the end
	OfficeHours   string     `json:"officeHours,omitempty"` // shortfall and extra hours are measured against it
	ShiftIn       string     `json:"shiftIn,omitempty"`     // current | previous | next
	SinglePunch   string     `json:"singlePunch,omitempty"` // absent | half | present
	MaxHours      string     `json:"maxHours,omitempty"`    // cap on hours counted for a day
	BasedOn       string     `json:"basedOn,omitempty"`     // hours | shift | punch
	AbsentBelow   string     `json:"absentBelow,omitempty"`
	HalfBelow     string     `json:"halfBelow,omitempty"`
	FullAbove     string     `json:"fullAbove,omitempty"`
	WeeklyAdj     *Adj       `json:"weeklyAdj,omitempty"`
	MonthlyAdj    *Adj       `json:"monthlyAdj,omitempty"`
	Shortfall     *Shortfall `json:"shortfall,omitempty"`
	// The OT rule (otAbove) and "Actual hours without break" (withoutBreak) were removed: no overtime is paid and
	// no break is taken off. Stored versions that still carry them read without them.
}

// Normalize drops parts that say nothing (an adjustment that is off, no shortfall inclusions) and trims text.
func (r *Rules) Normalize() {
	if r == nil {
		return
	}
	for _, s := range []*string{&r.StartFrom, &r.EndFrom, &r.ForcedOut, &r.EarlyInMax, &r.LateOutMax, &r.OfficeHours, &r.ShiftIn, &r.SinglePunch,
		&r.MaxHours, &r.BasedOn, &r.AbsentBelow, &r.HalfBelow, &r.FullAbove} {
		*s = strings.TrimSpace(*s)
	}
	if r.WeeklyAdj != nil && !r.WeeklyAdj.On {
		r.WeeklyAdj = nil
	}
	if r.MonthlyAdj != nil && !r.MonthlyAdj.On {
		r.MonthlyAdj = nil
	}
	if r.Shortfall != nil && *r.Shortfall == (Shortfall{}) {
		r.Shortfall = nil
	}
}

// Empty: no rule is set, so the work policy decides everything.
func (r *Rules) Empty() bool { return r == nil || *r == (Rules{}) }

// Windowed: the day's record is built from the punches inside the shift's window, not the calendar day.
func (r *Rules) Windowed() bool {
	return r != nil && ((r.NextDay != nil && *r.NextDay) || (r.ShiftIn != "" && r.ShiftIn != "current") || r.EarlyInMax != "" || r.LateOutMax != "")
}

// Dur parses a duration H:MM, HH:MM or HHH:MM into minutes.
func Dur(s string) (int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ':')
	if i < 1 || i > 3 || len(s)-i != 3 {
		return 0, false
	}
	for j, c := range s {
		if j != i && (c < '0' || c > '9') {
			return 0, false
		}
	}
	h, _ := strconv.Atoi(s[:i])
	m, _ := strconv.Atoi(s[i+1:])
	if m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// DurText formats minutes as H:MM.
func DurText(m int) string { return fmt.Sprintf("%d:%02d", m/60, m%60) }

// HoursText formats hours as H:MM.
func HoursText(h float64) string { return DurText(int(math.Round(h * 60))) }

func durH(s string) (float64, bool) {
	m, ok := Dur(s)
	return float64(m) / 60, ok
}

// Eff is the work policy with a shift's rules laid over it: everything the engine reads for one day.
// With no rules it holds the policy's own values, so the engine works exactly as before.
type Eff struct {
	p         *policy.Settings
	AutoOut   bool    // a missing out punch gets the forced out time (else the shift end)
	ForcedOut int     // minutes after midnight; -1 = the shift end
	Missing   string  // a day with one punch: half | absent | present
	StartFrom string  // "" = the policy's early check-in allowance; shift; punch
	StopAtEnd bool    // hours stop at the shift end
	AbsentH   float64 // worked less: absent
	HalfH     float64 // worked less (and not absent): half day
	FullH     float64 // worked this or more: full day; between HalfH and FullH: short hours
	OfficeH   float64 // shortfall and extra hours are measured against it
	MaxH      float64 // 0 = no cap
	ByPunch   bool    // any in punch on a working day is a full day
	WeeklyOn  bool
	WeeklyH   float64
	MonthlyOn bool
	MonthlyH  float64
	Incl      Shortfall
}

// Effective merges a shift's rules over the work policy (nil or empty rules = the policy as it is).
func Effective(p *policy.Settings, r *Rules) Eff {
	e := Eff{p: p, AutoOut: p.Att.AutoOut == "end", ForcedOut: -1, Missing: p.Att.Missing, AbsentH: p.HalfH, HalfH: p.FullH, FullH: p.FullH, OfficeH: p.FullH}
	if r.Empty() {
		return e
	}
	if r.AddMissingOut != nil {
		e.AutoOut = *r.AddMissingOut
	}
	if m, ok := Min(r.ForcedOut); ok {
		e.ForcedOut = m
	}
	switch r.SinglePunch {
	case "absent", "half", "present":
		e.Missing = r.SinglePunch
	}
	switch r.StartFrom {
	case "shift", "punch":
		e.StartFrom = r.StartFrom
	}
	e.StopAtEnd = r.EndFrom == "shift"
	switch r.BasedOn {
	case "shift":
		e.StartFrom, e.StopAtEnd = "shift", true
	case "punch":
		e.ByPunch = true
	}
	if h, ok := durH(r.AbsentBelow); ok {
		e.AbsentH = h
	}
	if h, ok := durH(r.HalfBelow); ok {
		e.HalfH = h
	}
	if h, ok := durH(r.FullAbove); ok {
		e.FullH = h
	}
	e.OfficeH = e.FullH
	if h, ok := durH(r.OfficeHours); ok {
		e.OfficeH = h
	}
	if h, ok := durH(r.MaxHours); ok && h > 0 {
		e.MaxH = h
	}
	if a := r.WeeklyAdj; a != nil && a.On {
		e.WeeklyH, _ = durH(a.Hours)
		e.WeeklyOn = true
	}
	if a := r.MonthlyAdj; a != nil && a.On {
		e.MonthlyH, _ = durH(a.Hours)
		e.MonthlyOn = true
	}
	if r.Shortfall != nil {
		e.Incl = *r.Shortfall
	}
	return e
}

// Hours are the hours counted between check-in a and check-out b (minutes; b past midnight when
// smaller): the start rule (the policy's early allowance, the shift start or the punch), the end rule
// and the cap. No break is taken off. With no rules this is NetHours.
func (e Eff) Hours(a, b int, sh *Shift) float64 {
	if b <= a {
		b += 1440
	}
	if sh != nil {
		sg := ((a-sh.Start+2160)%1440+1440)%1440 - 720 // check-in relative to the nearest shift start
		start := a - sg
		switch e.StartFrom {
		case "shift":
			if sg < 0 {
				a = start
			}
		case "punch":
		default:
			if e.p.RS.EarlyCount != "yes" && sg < 0 && -sg <= e.p.RS.EarlyIn {
				a += -sg
			}
		}
		if e.StopAtEnd {
			if end := start + sh.length(); b > end {
				b = end
			}
			if b <= a {
				return 0
			}
		}
	}
	h := math.Max(0, float64(b-a)/60)
	if e.MaxH > 0 && h > e.MaxH {
		h = e.MaxH
	}
	return h
}

// span is the day's in and out in minutes, with a missing out filled in when the rules say so.
// ok is false when the day has no usable pair.
func (e Eff) span(plan Plan, rec *Rec) (a, b int, ok bool) {
	if rec == nil || rec.In == "" {
		return 0, 0, false
	}
	a, _ = Min(rec.In)
	if rec.Out == "" {
		if !e.AutoOut || plan.Shift == nil {
			return a, 0, false
		}
		b = plan.Shift.End
		if e.ForcedOut >= 0 {
			b = e.ForcedOut
		}
	} else {
		b, _ = Min(rec.Out)
	}
	if b <= a {
		b += 1440
	}
	return a, b, true
}

// Counted is the hours counted for the day, when it has an in and an out (or a filled-in out).
func (e Eff) Counted(plan Plan, rec *Rec) (float64, bool) {
	a, b, ok := e.span(plan, rec)
	if !ok {
		return 0, false
	}
	return e.Hours(a, b, plan.Shift), true
}

// DayEval is one day's status with what the month needs for the adjustments.
type DayEval struct {
	Status, Tone string
	Hours        float64 // hours counted, when Counted
	Counted      bool
	FullAs       string // a short-hours day that is covered: Present or Late
	FullTone     string
}

// Eval is the daily status (attStatus) under the merged rules.
func (e Eff) Eval(plan Plan, rec *Rec, lateStart *int, override bool) DayEval {
	p := e.p
	if rec == nil || rec.Empty() {
		switch plan.Code {
		case "WO":
			return DayEval{Status: "Week off", Tone: "n"}
		case "PH":
			return DayEval{Status: "Holiday", Tone: "n"}
		}
		return DayEval{Status: "No punch", Tone: "bad"}
	}
	var ps *int
	if plan.Shift != nil {
		ps = &plan.Shift.Start
	}
	ls := ps
	if override {
		ls = lateStart
	}
	if ls != nil && len(plan.Short) > 0 { // an approved short leave at the start moves the late mark to its end
		x := plan.ExcusedStart(*ls, int(p.Grace))
		ls = &x
	}
	if e.ByPunch && plan.Shift != nil && rec.In != "" {
		// Attendance based on punch: an in punch makes it a full day; the hour rules are not used.
		ev := DayEval{Status: "Present", Tone: "ok"}
		a, b, ok := e.span(plan, rec)
		if ok {
			ev.Hours, ev.Counted = e.Hours(a, b, plan.Shift), true
		}
		if ls != nil && float64(a) > float64(*ls)+p.Grace {
			ev.Status, ev.Tone = "Late", "warn"
		}
		return ev
	}
	if rec.In == "" {
		return DayEval{Status: "Missing in punch", Tone: "warn"}
	}
	a, b, ok := e.span(plan, rec)
	if !ok {
		return DayEval{Status: "Missing out punch", Tone: "warn"}
	}
	h := e.Hours(a, b, plan.Shift)
	if c := plan.shortCredit(a, b); c > 0 { // the short leave's hours count as worked
		h += c
		if e.MaxH > 0 && h > e.MaxH {
			h = e.MaxH
		}
	}
	// Arriving inside the grace minutes counts as on time (business decision, 13 Nov 2026): the minutes
	// lost to a late arrival within grace are credited, so an 8-hour shift worked from start+grace to its
	// end is still a full day. Beyond grace nothing is credited: the hour thresholds and the late mark
	// apply as before.
	if late := lateBy(a, ls); late > 0 && float64(late) <= p.Grace {
		h += float64(late) / 60
		if e.MaxH > 0 && h > e.MaxH {
			h = e.MaxH
		}
	}
	ev := DayEval{Hours: h, Counted: true}
	full, tone := "Present", "ok"
	if ls != nil && float64(a) > float64(*ls)+p.Grace {
		full, tone = "Late", "warn"
	}
	switch {
	case h < e.AbsentH:
		ev.Status, ev.Tone = "Absent (short hours)", "bad"
	case h < e.HalfH:
		ev.Status, ev.Tone = "Half day", "warn"
	case h < e.FullH:
		ev.Status, ev.Tone, ev.FullAs, ev.FullTone = "Short hours", "warn", full, tone
	default:
		ev.Status, ev.Tone = full, tone
	}
	return ev
}

// lateBy is how many minutes after the late-mark start ls the check-in a is (0 when on time or early, or
// when there is no start). Times wrap at midnight: the check-in is taken relative to the nearest start.
func lateBy(a int, ls *int) int {
	if ls == nil {
		return 0
	}
	sg := ((a-*ls+2160)%1440+1440)%1440 - 720
	if sg < 0 {
		return 0
	}
	return sg
}

// ---------- Weekly and monthly adjustment ----------

type extraHours struct {
	week   string
	kind   string  // "" a working day; wo, hol or opt
	h      float64 // hours counted on the day
	normal float64 // hours beyond office hours
	used   float64
}

func (x *extraHours) left(in Shortfall) float64 {
	u := x.normal
	switch x.kind {
	case "wo":
		u = 0
		if in.WO {
			u = x.h
		}
	case "hol":
		u = 0
		if in.Hol {
			u = x.h
		}
	case "opt":
		if in.OptHol {
			u = x.h
		}
	}
	return math.Max(0, u-x.used)
}

type shortDay struct {
	i     int
	week  string
	short float64
	e     Eff
	ev    DayEval
	by    string
}

// weekOf is the Monday of the date's week.
func weekOf(d string) string { return policy.AddDays(d, -((int(policy.D(d).Weekday()) + 6) % 7)) }

// cover takes need hours from the extra hours in the pool (oldest first) when they cover it in full.
func cover(pool []*extraHours, need float64, in Shortfall) bool {
	have := 0.0
	for _, x := range pool {
		have += x.left(in)
	}
	if have+1e-9 < need {
		return false
	}
	for _, x := range pool {
		if need <= 1e-9 {
			break
		}
		t := math.Min(need, x.left(in))
		x.used += t
		need -= t
	}
	return true
}

// adjust settles the short-hours days: the weekly adjustment first (Monday to Sunday, inside the days
// given), then the monthly one; a day not covered in full becomes a half day.
func adjust(shorts []*shortDay, extras []*extraHours) {
	used := map[string]float64{}
	for _, s := range shorts {
		if !s.e.WeeklyOn {
			continue
		}
		pool := []*extraHours{}
		for _, x := range extras {
			if x.week == s.week {
				pool = append(pool, x)
			}
		}
		if s.short <= s.e.WeeklyH-used[s.week]+1e-9 && cover(pool, s.short, s.e.Incl) {
			used[s.week] += s.short
			s.by = "weekly"
		}
	}
	month := 0.0
	for _, s := range shorts {
		if s.by != "" || !s.e.MonthlyOn {
			continue
		}
		if s.short <= s.e.MonthlyH-month+1e-9 && cover(extras, s.short, s.e.Incl) {
			month += s.short
			s.by = "monthly"
		}
	}
}
