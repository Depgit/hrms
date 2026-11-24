package attend

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/saltstayz/hrms/internal/policy"
)

// A shift version keeps its timings and, as Rules, only whether it ends on the next calendar day (set from the times).
// The shift's own attendance rules were removed (owner, 24 Nov 2026: "under shift remove the Attendance rules card"):
// Max hours, Attendance based on, the Absent, Half day and Full day rules, the weekly and monthly adjustments and the
// shortfall inclusions. The work policy decides every day, whatever its shift. Stored versions that still carry any
// of them read without them (no live version had one set). The rules travel with the day's plan (Plan.Rules).

// Rules are a shift version's rules beyond its timings.
type Rules struct {
	NextDay *bool `json:"nextDay,omitempty"` // the shift ends on the next calendar day (must agree with the times)
	// Removed: the OT rule (otAbove) and "Actual hours without break" (withoutBreak) on 8 Nov 2026; the punch rules and
	// the single punch rule (startFrom, endFrom, addMissingOut, forcedOut, earlyInMax, lateOutMax, officeHours, shiftIn,
	// singlePunch) and the attendance rules (maxHours, basedOn, absentBelow, halfBelow, fullAbove, weeklyAdj, monthlyAdj,
	// shortfall) on 24 Nov 2026. A save naming any of them is refused; a stored version reads without them.
}

// Empty: no rule is set.
func (r *Rules) Empty() bool { return r == nil || *r == (Rules{}) }

// EndsNextDay: the shift's rules say it ends on the next calendar day.
func (r *Rules) EndsNextDay() bool { return r != nil && r.NextDay != nil && *r.NextDay }

// Windowed: the day's record is built from the punches inside the shift's window, not the calendar day: the shift
// ends the next day, or the work policy limits the punch window (earlyIn, lateOut: the policy's maximum early in and
// late out punch in minutes, 0 = no limit).
func (r *Rules) Windowed(earlyIn, lateOut int) bool {
	return r.EndsNextDay() || earlyIn > 0 || lateOut > 0
}

// PunchLimits are the work policy's maximum early in and late out punch in minutes (0 = no limit).
func PunchLimits(p *policy.Settings) (earlyIn, lateOut int) {
	if p == nil {
		return 0, 0
	}
	return int(math.Round(p.Att.EarlyInMax * 60)), int(math.Round(p.Att.LateOutMax * 60))
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

// Eff is the work policy as the engine reads it for one day. The shift's own attendance rules were removed (24 Nov
// 2026), so it holds the policy's values whatever the day's shift.
type Eff struct {
	p       *policy.Settings
	AutoOut bool    // a missing out punch gets the shift end (the work policy's "If out punch is missing")
	Missing string  // a day with one punch: half | absent | present (the work policy's)
	AbsentH float64 // worked less: absent (the policy's half-day hours)
	HalfH   float64 // worked less (and not absent): half day (the policy's full-day hours)
	OfficeH float64 // the full-day hours
}

// Effective is the work policy for a day. The shift's rules (the second argument) no longer change anything: the
// argument stays so the callers keep their shape.
func Effective(p *policy.Settings, _ *Rules) Eff {
	return Eff{p: p, AutoOut: p.Att.AutoOut == "end", Missing: p.Att.Missing, AbsentH: p.HalfH, HalfH: p.FullH, OfficeH: p.FullH}
}

// Hours are the hours counted between check-in a and check-out b (minutes; b past midnight when smaller): an early
// check-in inside the policy's early allowance counts from the shift start. No break is taken off, except on a split
// shift: the gap between its two parts never counts (owner, 24 Nov 2026), so the time in to out loses the part of it
// inside the gap. Without a split shift this is NetHours.
func (e Eff) Hours(a, b int, sh *Shift) float64 {
	if b <= a {
		b += 1440
	}
	gap := 0
	if sh != nil {
		sg := ((a-sh.Start+2160)%1440+1440)%1440 - 720 // check-in relative to the nearest shift start
		start := a - sg
		if e.p.RS.EarlyCount != "yes" && sg < 0 && -sg <= e.p.RS.EarlyIn {
			a += -sg
		}
		if sh.Split() { // the gap between the parts, on the same day as the start
			g1, g2 := start+sh.End-sh.Start, start+sh.Start2-sh.Start
			gap = max(0, min(b, g2)-max(a, g1))
		}
	}
	return math.Max(0, float64(b-a-gap)/60)
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
		b = plan.Shift.LastEnd()
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

// DayEval is one day's status and the hours counted.
type DayEval struct {
	Status, Tone string
	Hours        float64 // hours counted, when Counted
	Counted      bool
	// FullAs and FullTone were what a covered short-hours day became. No day is short-hours since the shift's rules
	// were removed (24 Nov 2026), so they stay empty; FinalMonth still reads them.
	FullAs, FullTone string
}

// Eval is the daily status (attStatus) under the work policy: absent below its half-day hours, a half day below its
// full-day hours, else present (or late).
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
	}
	// Arriving inside the grace minutes counts as on time (business decision, 13 Nov 2026): the minutes
	// lost to a late arrival within grace are credited, so an 8-hour shift worked from start+grace to its
	// end is still a full day. Beyond grace nothing is credited: the hour thresholds and the late mark
	// apply as before.
	if late := lateBy(a, ls); late > 0 && float64(late) <= p.Grace {
		h += float64(late) / 60
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

// ---------- Weekly and monthly adjustment (removed) ----------

// The shift's weekly and monthly adjustments covered short-hours days with extra hours. They were removed with the
// shift's attendance rules (owner, 24 Nov 2026) and no day is short-hours any more, so what FinalMonth still keeps for
// them (extraHours, shortDay, adjust) does nothing.

type extraHours struct {
	week   string
	kind   string  // "" a working day; wo, hol or opt
	h      float64 // hours counted on the day
	normal float64 // hours beyond office hours
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

// adjust covered short-hours days with the shift's adjustments; with those removed nothing is covered.
func adjust([]*shortDay, []*extraHours) {}
