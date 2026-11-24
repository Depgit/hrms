package attend

import (
	"fmt"
	"strings"

	"github.com/saltstayz/hrms/internal/policy"
)

// Deviation allowances of the work policy (policy.DevRules, owner 24 Nov 2026). With a shift of 10:00–18:00 and
// "±2 h, 5 times a month", the person may work 08:00–16:00 or 12:00–20:00 on up to 5 days; beyond that the
// normal rules apply and the admin is informed.
//
//   - Late check-in: a check-in after the start + the late-mark grace and at most Mins minutes after the start. On
//     the first N such days of the calendar month the day is no late mark (Present, not counted in "Late marks allowed
//     in a month" or its half day). No hours are credited: the day still needs its full-day hours.
//   - Early check-in: a check-in more than the policy's early check-in allowance (rs.earlyIn) and at most Mins minutes
//     before the start. On the first N such days the shift slides earlier by those minutes, so hours count from the
//     in punch (whatever rs.earlyCount says) and 08:00–16:00 is a full 8 h.
//   - Early check-out: a check-out more than the early-out grace (att.early) and at most Mins minutes before the end
//     (the end slides with an allowed early check-in the same day). On the first N such days: a note, no early-exit
//     flag. No hours are ever credited.
//
// "Such days" are the days within the minutes, counted in date order through the calendar month; a check-in or
// check-out beyond the minutes, or beyond the N-th day, is reported (DevEvent) and gets its rule's action (below: No
// action follows the rules as before). Leave days, regularized days, days called to work, week offs and holidays are
// never deviation days.

// DevIn is one day for the allowances.
type DevIn struct {
	Date string
	Plan Plan // the roster plan of the day (a working day has a shift)
	Rec  *Rec
	Skip bool // not a deviation day: leave, regularized, called to work, outside employment or still to come
	Done bool // the day is over: only then is its last out punch final (early check-out)
	// Rules are the allowances of the day when they are not the work policy's: the employee's own rules laid over the
	// policy's from their "applies from" date (policy.DevOwnRules). nil = the policy's.
	Rules *policy.DevRules
}

// Allow is what the allowances allow on one day.
type Allow struct {
	LateIn   bool     // an allowed late check-in: no late mark
	Slide    int      // an allowed early check-in: the shift runs this many minutes earlier
	EarlyOut bool     // an allowed early check-out: no early-exit flag
	Notes    []string // "Late check-in allowed (2 of 5)"
}

// Dev rules, as named in DevEvent.Rule.
const (
	DevLateIn   = "late-in"
	DevEarlyIn  = "early-in"
	DevEarlyOut = "early-out"
)

// DevEvent is a deviation the allowance does not cover: the admin is informed.
type DevEvent struct {
	Rule string `json:"rule"`
	Mins int    `json:"mins"`          // minutes late or early
	K    int    `json:"k"`             // the day's number among the month's deviations within the minutes; 0 when beyond the minutes
	N    int    `json:"n"`             // times allowed in a month
	M    int    `json:"m"`             // minutes allowed
	Act  string `json:"act,omitempty"` // the rule's action beyond the allowance ("" = No action: policy.DevRule.Action)
}

// What is "late check-in", "early check-in" or "early check-out".
func (v DevEvent) What() string {
	return map[string]string{DevLateIn: "late check-in", DevEarlyIn: "early check-in", DevEarlyOut: "early check-out"}[v.Rule]
}

// Text is the event in words, without the person and date: "late check-in 2 h 10 min, 6th this month (allowed 5)".
func (v DevEvent) Text() string {
	if v.K == 0 {
		return fmt.Sprintf("%s %s, more than the %s allowed", v.What(), MinText(v.Mins), MinText(v.M))
	}
	return fmt.Sprintf("%s %s, %s this month (allowed %d)", v.What(), MinText(v.Mins), Ordinal(v.K), v.N)
}

// DevDay is one day's result.
type DevDay struct {
	Allow  *Allow
	Events []DevEvent
}

// Deviations runs the allowances over the days, in date order; the count restarts each calendar month. Each day
// follows its own rules (DevIn.Rules, the employee's own from their date) or else the policy's; a rule's count runs
// on through the month when the employee's rules change in it. The result has one entry per day (empty when the rules
// are off or the day has no deviation).
func Deviations(p policy.Settings, days []DevIn) []DevDay {
	out := make([]DevDay, len(days))
	on := p.Dev.Any()
	for _, d := range days {
		on = on || (d.Rules != nil && d.Rules.Any())
	}
	if !on {
		return out
	}
	count := map[string]int{} // rule + month → days within the minutes so far
	grace := int(p.Grace)
	for i, d := range days {
		if d.Skip || d.Rec == nil || d.Rec.In == "" || d.Plan.Shift == nil || d.Plan.Code == "WO" || d.Plan.Code == "PH" || d.Plan.Code == "L" {
			continue
		}
		r := p.Dev
		if d.Rules != nil {
			r = *d.Rules
		}
		if !r.Any() {
			continue
		}
		a, ok := Min(d.Rec.In)
		if !ok {
			continue
		}
		sh, ym := d.Plan.Shift, d.Date[:7]
		al := &Allow{}
		take := func(rule string, mins int, rr policy.DevRule) bool {
			if mins > rr.Mins {
				out[i].Events = append(out[i].Events, DevEvent{Rule: rule, Mins: mins, N: rr.N, M: rr.Mins, Act: rr.Action()})
				return false
			}
			count[rule+ym]++
			k := count[rule+ym]
			if k > rr.N {
				out[i].Events = append(out[i].Events, DevEvent{Rule: rule, Mins: mins, K: k, N: rr.N, M: rr.Mins, Act: rr.Action()})
				return false
			}
			al.Notes = append(al.Notes, fmt.Sprintf("%s allowed, %s (%d of %d)", map[string]string{DevLateIn: "Late check-in", DevEarlyIn: "Early check-in",
				DevEarlyOut: "Early check-out"}[rule], MinText(mins), k, rr.N))
			return true
		}
		if sg := signedMins(a, sh.Start); sg < 0 {
			if early := -sg; r.EarlyIn.Active() && early > p.RS.EarlyIn && take(DevEarlyIn, early, r.EarlyIn) {
				al.Slide = early
			}
		} else if r.LateIn.Active() {
			st := sh.Start
			if len(d.Plan.Short) > 0 { // an approved short leave at the start moves the start to its end
				st = d.Plan.ExcusedStart(st, grace)
			}
			if late := signedMins(a, st); late > grace && take(DevLateIn, late, r.LateIn) {
				al.LateIn = true
			}
		}
		if r.EarlyOut.Active() && d.Done && d.Rec.Out != "" {
			if b, ok := Min(d.Rec.Out); ok {
				end := sh.LastEnd() - al.Slide
				if len(d.Plan.Short) > 0 { // leaving for an approved short leave at the end is no early check-out
					end = d.Plan.excusedEnd(end, p.Att.Early)
				}
				if so := signedMins(b, end); so < 0 && so > -720 && -so > p.Att.Early && take(DevEarlyOut, -so, r.EarlyOut) {
					al.EarlyOut = true
				}
			}
		}
		if al.LateIn || al.Slide > 0 || al.EarlyOut {
			out[i].Allow = al
		}
	}
	return out
}

// Apply lays the allowance over the day's plan and late-mark start: an allowed early check-in runs the shift earlier
// (hours from the in punch, the end earlier too), an allowed late check-in takes the late mark away.
func (al *Allow) Apply(plan Plan, ls *int, override bool) (Plan, *int, bool) {
	if al == nil {
		return plan, ls, override
	}
	if al.Slide > 0 && plan.Shift != nil {
		sh := slid(*plan.Shift, al.Slide)
		plan.Shift = &sh
	}
	if al.LateIn {
		ls, override = nil, true // no late mark; attendance follows the hours
	}
	return plan, ls, override
}

// ---------- Beyond the allowance: the rule's action (owner, 24 Nov 2026) ----------
//
// A day beyond an allowance (past the times allowed in the month, or past the minutes) gets its rule's Action:
//
//   - No action: the usual rules (the late mark and "Late marks allowed in a month", the hours rules).
//   - Absent: Absent, loss of pay 1, until a regularization of the date is approved.
//   - Half day until regularized: loss of pay 0.5 until regularized (as the missing-punch option of that name).
//   - Present until regularized: Present, no loss of pay, flagged for regularization.
//
// With Absent, Half day or Present the action replaces the day's late-mark handling: no late mark, so no double
// penalty and nothing counted again in "Late marks allowed in a month". The hours rules still apply as on any day:
// when they give the day more loss of pay than the action (worked under the absent hours with Half day, say), the
// hours stand; the loss of pay is the larger of the two, never both. Two rules beyond on one day: the harsher action
// wins (Absent > Half day > Present > No action). A regularized day is never a deviation day, so once a regularization
// of the date is approved the day follows the corrected times like any regularized day. The bell goes in every case.

// Statuses of a day the action settles.
const (
	StAbsentReg  = "Absent until regularized"
	StHalfReg    = "Half day until regularized"
	StPresentReg = "Present until regularized"
	untilReg     = " until regularized"
)

// actRank orders the actions: the harsher wins.
func actRank(act string) int {
	switch act {
	case policy.DevActAbsent:
		return 3
	case policy.DevActHalf:
		return 2
	case policy.DevActPresent:
		return 1
	}
	return 0
}

// actLOP is the loss of pay of a day the action settles.
func actLOP(act string) float64 {
	return map[string]float64{policy.DevActAbsent: 1, policy.DevActHalf: 0.5}[act]
}

// ActStatus is the status of a day settled by the action ("" for No action).
func ActStatus(act string) string {
	return map[string]string{policy.DevActAbsent: StAbsentReg, policy.DevActHalf: StHalfReg, policy.DevActPresent: StPresentReg}[act]
}

func actTone(act string) string {
	return map[string]string{policy.DevActAbsent: "bad", policy.DevActHalf: "warn", policy.DevActPresent: "ok"}[act]
}

// UntilReg: the status is one an action beyond an allowance gave the day, waiting for a regularization.
func UntilReg(st string) bool { return strings.HasSuffix(st, untilReg) }

// BaseStatus is the status without "until regularized": "Half day until regularized" → "Half day".
func BaseStatus(st string) string { return strings.TrimSuffix(st, untilReg) }

// hoursLOP is the most loss of pay the hours rules give a day of status st (missing: the policy's missing-punch
// rule). A short-hours day is at most a half day.
func hoursLOP(st, missing string) float64 {
	switch {
	case strings.HasPrefix(st, "Absent"), st == "No punch":
		return 1
	case st == "Half day", st == "Short hours":
		return 0.5
	case strings.Contains(st, "Missing"):
		return map[string]float64{"half": 0.5, "absent": 1}[missing]
	}
	return 0
}

// Act is the day's action: the harshest of its rules beyond the allowance ("" when none, or No action).
func (d *DevDay) Act() string {
	if d == nil {
		return ""
	}
	act := ""
	for _, v := range d.Events {
		if actRank(v.Act) > actRank(act) {
			act = v.Act
		}
	}
	return act
}

// Lay lays the day's allowance over the plan and the late-mark start (Allow.Apply); with an action beyond the
// allowance the late mark goes too: the action replaces it.
func (d *DevDay) Lay(plan Plan, ls *int, override bool) (Plan, *int, bool) {
	plan, ls, override = d.Allowed().Apply(plan, ls, override)
	if d.Act() != "" {
		ls, override = nil, true
	}
	return plan, ls, override
}

// Status is the day's status (Status) under the allowance and the action of a rule beyond it.
func (d *DevDay) Status(p policy.Settings, plan Plan, rec *Rec, ls *int, override bool) (string, string) {
	plan, ls, override = d.Lay(plan, ls, override)
	st, tone := Status(p, plan, rec, ls, override)
	if act := d.Act(); act != "" && actLOP(act) >= hoursLOP(st, p.Att.Missing) {
		return ActStatus(act), actTone(act)
	}
	return st, tone
}

// devAct settles a working day beyond an allowance by the day's action, unless the hours rules give it more loss of
// pay (o.Status is the hours rules' status, without a late mark). Either way the day is marked to regularize (Reg).
// false: the day goes on as the hours rules have it.
func (o *DayOut) devAct(d DayIn, missing string, counts map[string]int) bool {
	act := d.Dev.Act()
	if act == "" {
		return false
	}
	o.Reg = act
	if hoursLOP(o.Status, missing) > actLOP(act) {
		return false // the hours stand; with Present, a short-hours day is settled by the adjustments (regPresent)
	}
	o.Status, o.Tone = ActStatus(act), actTone(act)
	switch act {
	case policy.DevActAbsent:
		o.Code, o.LOP = "A", 1
		counts["No punch"]++ // with the other absent days (as Absent (short hours))
	case policy.DevActHalf:
		o.Code, o.LOP = "H", 0.5
		counts["Half day"]++
	default:
		o.Code = "P"
		counts["Present"]++
	}
	return true
}

// regPresent: a short-hours day beyond an allowance with Present until regularized, covered by the adjustments,
// reads Present until regularized.
func (o *DayOut) regPresent() {
	if o.Reg == policy.DevActPresent && o.Status == "Present" {
		o.Status = StPresentReg
	}
}

// slid is the shift run m minutes earlier (a split shift's second part too, unless it would cross midnight).
func slid(s Shift, m int) Shift {
	mod := func(x int) int { return ((x % 1440) + 1440) % 1440 }
	if s.Split() {
		if s.Start-m < 0 {
			return s
		}
		s.Start2, s.End2 = s.Start2-m, s.End2-m
	}
	s.Start, s.End = mod(s.Start-m), mod(s.End-m)
	return s
}

// signedMins is minute a relative to the nearest occurrence of minute ref: negative before it, positive after.
func signedMins(a, ref int) int { return ((a-ref+2160)%1440+1440)%1440 - 720 }

// MinText is minutes in words: "2 h 10 min", "2 h", "45 min".
func MinText(m int) string {
	switch h, mm := m/60, m%60; {
	case h == 0:
		return fmt.Sprintf("%d min", mm)
	case mm == 0:
		return fmt.Sprintf("%d h", h)
	default:
		return fmt.Sprintf("%d h %d min", h, mm)
	}
}

// Ordinal is 1st, 2nd, 3rd, 4th … 11th, 12th, 13th, 21st.
func Ordinal(n int) string {
	suf := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suf = "st"
		case 2:
			suf = "nd"
		case 3:
			suf = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suf)
}
