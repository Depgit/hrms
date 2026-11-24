package attend

import (
	"strings"

	"github.com/saltstayz/hrms/internal/policy"
)

// Called to work on a holiday or a leave day (owner, 24 Nov 2026: "if they are asked to work then mark their
// attendance … if they come then present"; on a leave day the leave comes back). HR assigns the date; the engine
// decides from the punches:
//
//   - Came: the day has a punch (owner's rule, taken literally on 24 Nov 2026: hours, a late arrival or a missing
//     out punch do not matter). The day is Present ("Called to work", the hours shown when there is an in and an
//     out), with no loss of pay; on a leave day the date's leave is returned (1 day, or 0.5 for a half-day leave:
//     DayOut.Returned) and an unpaid leave loses its loss of pay.
//   - No punch: the holiday or the leave stays exactly as it was, with a note. A call never adds loss of pay.

// calledWorked: the day was called to work and worked; h is the hours counted.
func calledWorked(p policy.Settings, d DayIn) (float64, bool) {
	if !d.Called || d.Before || d.After || d.Future {
		return 0, false
	}
	return CalledWorked(p, d.CallPlan, d.Rec)
}

// CalledWorked: a day called to work with these punches is worked: the person came (any punch). h is the hours
// counted under the rules of the shift plan, 0 without an in and an out.
func CalledWorked(p policy.Settings, plan Plan, rec *Rec) (float64, bool) {
	if rec == nil || rec.Empty() {
		return 0, false
	}
	h, _ := Effective(&p, plan.Rules).Counted(plan, rec)
	return h, true
}

// settleCalled makes the called day Present.
func (o *DayOut) settleCalled(d DayIn, h float64) {
	o.Status, o.Tone, o.Code, o.Called = "Present", "ok", "P", true
	o.Note = "Called to work, worked " + HoursText(h)
	if h == 0 {
		o.Note = "Called to work, came"
		if d.Rec != nil && d.Rec.In != "" && d.Rec.Out == "" {
			o.Note += " (in " + d.Rec.In + ", no out punch)"
		}
	}
	if d.Leave != "" {
		o.Returned = 1
		if d.Half {
			o.Returned = 0.5
		}
		o.Note += "; " + d.Leave + " returned (" + map[bool]string{true: "half a day", false: "1 day"}[d.Half] + ")"
	}
}

// dayNotes adds the day's deviation allowance notes and counts, and the note of a call that was not worked.
func (o *DayOut) dayNotes(d DayIn, called bool, counts map[string]int) {
	if d.Called && !called && !d.Before && !d.After && !d.Future {
		what := "the holiday stands"
		if d.Leave != "" {
			what = "the leave stands"
		}
		o.Note = joinNote(o.Note, "Called to work, no punch: "+what)
	}
	if d.Dev == nil || called || d.Before || d.After || d.Future || d.Leave != "" {
		return
	}
	if al := d.Dev.Allow; al != nil {
		for _, n := range al.Notes {
			o.Note = joinNote(o.Note, n)
		}
		if al.LateIn {
			counts["Late check-in allowed"]++
		}
		if al.Slide > 0 {
			counts["Early check-in allowed"]++
		}
		if al.EarlyOut {
			counts["Early check-out allowed"]++
		}
	}
	for _, v := range d.Dev.Events {
		t := v.Text()
		t = strings.ToUpper(t[:1]) + t[1:] + ": beyond the allowance"
		if v.Act != "" { // the rule's action (devallow.go): "… → Half day until regularized"
			t += " → " + ActStatus(v.Act)
		}
		o.Note = joinNote(o.Note, t)
		counts["Beyond the deviation allowance"]++
	}
	if o.Reg != "" {
		counts[ActStatus(o.Reg)]++ // the days beyond an allowance by their action, whatever the hours made them
		if o.Status != ActStatus(o.Reg) && o.Status != "Short hours" {
			o.Note = joinNote(o.Note, "the hours or punches give more loss of pay: "+o.Status)
		}
	}
}

// Allowed is the day's allowance (nil when none).
func (d *DevDay) Allowed() *Allow {
	if d == nil {
		return nil
	}
	return d.Allow
}

// Trim takes out of a roster deviation what the day's allowance covers: an allowed late check-in is no Shift
// deviation, an allowed early check-out no Early exit (an allowed early check-in is measured on the earlier shift by
// Apply). nil when nothing is left.
func (al *Allow) Trim(v *Deviation) *Deviation {
	if al == nil || v == nil {
		return v
	}
	keep := []string{}
	for _, k := range v.Kinds {
		if (k == "Shift" && (al.LateIn || al.Slide > 0)) || (k == "Early exit" && al.EarlyOut) {
			continue
		}
		keep = append(keep, k)
	}
	if len(keep) == 0 {
		return nil
	}
	x := *v
	x.Kinds = keep
	if !contains(keep, "Shift") {
		x.Actual = x.Planned
	}
	return &x
}

// joinNote adds a note after another with "; ".
func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}
