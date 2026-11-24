package attend

import (
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

// devMonth runs the allowances over the days, then the month.
func devMonth(p policy.Settings, days []DayIn) Month {
	in := make([]DevIn, len(days))
	for i, d := range days {
		in[i] = DevIn{Date: d.Date, Plan: d.Plan, Rec: d.Rec, Skip: d.Leave != "" || d.Called, Done: true}
	}
	for i, x := range Deviations(p, in) {
		x := x
		days[i].Dev = &x
	}
	return FinalMonth(p, days)
}

var tenToSix = &Shift{Code: "D", Start: 600, End: 1080} // 10:00–18:00

func devDay(d, in, out string) DayIn {
	return DayIn{Date: d, Plan: Plan{Code: "D", Shift: tenToSix}, Rec: &Rec{In: in, Out: out}}
}

// Late check-in allowed 120 minutes, 2 times a month (owner, 24 Nov 2026): the first two late check-ins within the
// minutes are Present, not late marks, and cause no late half day; the third is a late mark, and so is one beyond the
// minutes (which does not use up the count). With no late marks allowed, each late mark is half a day of loss of pay.
func TestDevLateIn(t *testing.T) {
	p := policy.Defaults()
	p.Grace, p.Att.LateN, p.Att.LateAct = 15, 0, "half"
	days := func() []DayIn {
		return []DayIn{
			devDay("2026-11-02", "11:00", "19:00"), // 60 min late: 1 of 2
			devDay("2026-11-03", "12:00", "20:00"), // 120 min late: 2 of 2
			devDay("2026-11-04", "11:30", "19:30"), // 3rd: a late mark, reported
			devDay("2026-11-05", "12:30", "20:30"), // 150 min: beyond the minutes, a late mark, reported
			devDay("2026-11-06", "10:10", "18:10"), // inside the grace: on time, no deviation
		}
	}
	off := FinalMonth(p, days())
	if off.Counts["Late"] != 4 || off.LOP != 2 {
		t.Fatalf("rules off: %d late, LOP %v; want 4 late, LOP 2", off.Counts["Late"], off.LOP)
	}
	p.Dev.LateIn = policy.DevRule{On: true, Mins: 120, N: 2}
	d := days()
	m := devMonth(p, d)
	want := []string{"Present", "Present", "Late", "Late", "Present"}
	for i, o := range m.Days {
		if o.Status != want[i] {
			t.Errorf("%s: %s (%s), want %s", o.Date, o.Status, o.Note, want[i])
		}
	}
	if !strings.Contains(m.Days[0].Note, "(1 of 2)") || !strings.Contains(m.Days[1].Note, "(2 of 2)") {
		t.Errorf("notes: %q, %q", m.Days[0].Note, m.Days[1].Note)
	}
	if m.Counts["Late"] != 2 || m.LateOver != 2 || m.LOP != 1 || m.Counts["Late check-in allowed"] != 2 || m.Counts["Beyond the deviation allowance"] != 2 {
		t.Fatalf("late %d, over %d, LOP %v, counts %v; want 2 late, 2 over, LOP 1", m.Counts["Late"], m.LateOver, m.LOP, m.Counts)
	}
	if ev := d[2].Dev.Events; len(ev) != 1 || ev[0].K != 3 || ev[0].Text() != "late check-in 1 h 30 min, 3rd this month (allowed 2)" {
		t.Errorf("3rd day event: %+v", ev)
	}
	if ev := d[3].Dev.Events; len(ev) != 1 || ev[0].K != 0 || ev[0].Text() != "late check-in 2 h 30 min, more than the 2 h allowed" {
		t.Errorf("beyond the minutes: %+v", ev)
	}
	// No hours are credited: an allowed late check-in that leaves at the shift end is short of the full day.
	short := devMonth(p, []DayIn{devDay("2026-11-02", "11:00", "18:00")})
	if o := short.Days[0]; o.Status != "Half day" || o.LOP != 0.5 {
		t.Errorf("allowed late, 7 h worked: %s LOP %v, want Half day 0.5", o.Status, o.LOP)
	}
}

// Early check-in allowed 120 minutes once a month: the day's hours count from the in punch (the shift slides to
// 08:00–16:00), even under a shift that counts from its start; the second time they count as before. An early
// check-out is measured against the slid end, and allowed once.
func TestDevEarlyIn(t *testing.T) {
	p := policy.Defaults()
	p.RS.EarlyIn, p.Att.Early = 30, 10
	p.Dev.EarlyIn = policy.DevRule{On: true, Mins: 120, N: 1}
	p.Dev.EarlyOut = policy.DevRule{On: true, Mins: 60, N: 1}
	days := []DayIn{
		devDay("2026-11-02", "08:00", "16:00"), // allowed: 8 h, and 16:00 is no early check-out
		devDay("2026-11-03", "08:00", "16:00"), // 2nd: 2 h early is beyond the policy's 30 min, so from the punch: 8 h; 16:00 is 2 h early: beyond the minutes
		devDay("2026-11-04", "10:00", "17:00"), // early check-out 60 min: allowed (1 of 1)
		devDay("2026-11-05", "10:00", "17:30"), // 2nd early check-out: reported
	}
	m := devMonth(p, days)
	if o := m.Days[0]; o.Status != "Present" || !strings.Contains(o.Note, "Early check-in allowed, 2 h (1 of 1)") || strings.Contains(o.Note, "check-out") {
		t.Errorf("allowed early check-in: %s %q", o.Status, o.Note)
	}
	// The shift's "Attendance based on Shift" was removed (24 Nov 2026): the policy counts a check-in more than its early
	// allowance before the start from the punch.
	if h, _ := Effective(&p, nil).Counted(Plan{Code: "D", Shift: tenToSix}, days[1].Rec); m.Days[1].Status != "Present" || h != 8 {
		t.Errorf("second early check-in: %s with %v h, want Present from the 08:00 punch (8 h)", m.Days[1].Status, h)
	}
	if ev := days[1].Dev.Events; len(ev) != 2 || ev[0].Rule != DevEarlyIn || ev[0].K != 2 || ev[1].Rule != DevEarlyOut || ev[1].K != 0 {
		t.Errorf("second day events: %+v", ev)
	}
	if al := days[2].Dev.Allowed(); al == nil || !al.EarlyOut || !strings.Contains(m.Days[2].Note, "Early check-out allowed, 1 h (1 of 1)") {
		t.Errorf("allowed early check-out: %+v %q", al, m.Days[2].Note)
	}
	if ev := days[3].Dev.Events; len(ev) != 1 || ev[0].Rule != DevEarlyOut || ev[0].K != 2 {
		t.Errorf("second early check-out: %+v", ev)
	}
	// The roster deviation of the allowed early check-out is gone; the allowed early check-in is no Shift deviation.
	plan, _, _ := days[0].Dev.Allowed().Apply(Plan{Code: "D", Shift: tenToSix}, nil, false)
	if v := Deviate(p, plan, days[0].Rec, days[0].Date, "", nil); v != nil {
		t.Errorf("allowed early check-in still a deviation: %+v", v)
	}
	p.RS.OutTol = 10
	if v := days[2].Dev.Allowed().Trim(Deviate(p, Plan{Code: "D", Shift: tenToSix}, days[2].Rec, days[2].Date, "", nil)); v != nil {
		t.Errorf("allowed early check-out still a deviation: %+v", v)
	}
}

// Called to work on a holiday or leave day: worked (full-day hours) is Present with no loss of pay, a leave day's
// leave is returned (1, or 0.5 for a half-day leave) and loses its loss of pay; worked days count 1 even on a
// holiday. A single punch or short day leaves the holiday or leave as it was: never more loss of pay.
func TestCalledToWork(t *testing.T) {
	p := policy.Defaults()
	plan := Plan{Code: "D", Shift: tenToSix}
	rec := func(in, out string) *Rec { return &Rec{In: in, Out: out} }
	days := []DayIn{
		{Date: "2026-11-02", Leave: "Casual leave", Called: true, CallPlan: plan, Rec: rec("10:20", "18:30")},
		{Date: "2026-11-03", Leave: "Loss of pay", Unpaid: true, Called: true, CallPlan: plan, Rec: rec("10:00", "18:00")},
		{Date: "2026-11-04", Plan: Plan{Code: "PH"}, Called: true, CallPlan: plan, Rec: rec("10:00", "18:00")},
		{Date: "2026-11-05", Plan: Plan{Code: "PH"}, Called: true, CallPlan: plan, Rec: rec("10:00", "")},
		{Date: "2026-11-06", Leave: "Loss of pay", Unpaid: true, Called: true, CallPlan: plan, Rec: rec("10:00", "13:00")},
		{Date: "2026-11-07", Leave: "Casual leave", Half: true, HalfOf: "first", Plan: plan, Called: true, CallPlan: plan, Rec: rec("10:00", "18:00")},
		{Date: "2026-11-08", Plan: Plan{Code: "PH"}, Called: true, CallPlan: plan},
	}
	m := FinalMonth(p, days)
	type want struct {
		status   string
		lop, ret float64
		called   bool
	}
	// Came = any punch (owner: "if they come then present"): a single punch and a 3-hour day are Present too.
	wants := []want{{"Present", 0, 1, true}, {"Present", 0, 1, true}, {"Present", 0, 0, true}, {"Present", 0, 0, true},
		{"Present", 0, 1, true}, {"Present", 0, 0.5, true}, {"Holiday", 0, 0, false}}
	for i, o := range m.Days {
		w := wants[i]
		if o.Status != w.status || o.LOP != w.lop || o.Returned != w.ret || o.Called != w.called {
			t.Errorf("%s: %s LOP %v returned %v called %v (%q); want %+v", o.Date, o.Status, o.LOP, o.Returned, o.Called, o.Note, w)
		}
	}
	if m.LOP != 0 || m.Counts["Called to work"] != 6 {
		t.Errorf("month LOP %v, called %d; want 0 and 6", m.LOP, m.Counts["Called to work"])
	}
	if w := m.Days[2].Paid(); w != 1 {
		t.Errorf("worked holiday counted %v for leave by working days, want 1", w)
	}
	if !strings.Contains(m.Days[0].Note, "Casual leave returned (1 day)") || !strings.Contains(m.Days[4].Note, "Loss of pay returned (1 day)") || !strings.Contains(m.Days[3].Note, "no out punch") ||
		!strings.Contains(m.Days[6].Note, "no punch: the holiday stands") {
		t.Errorf("notes: %q / %q / %q", m.Days[0].Note, m.Days[4].Note, m.Days[6].Note)
	}
}

// The action of a day beyond the allowance (owner, 24 Nov 2026). Late check-in 120 minutes once a month, no late marks
// allowed (each late mark is half a day of loss of pay). The 2nd late check-in (60 min) and one of 150 min are beyond
// the allowance. No action: two late marks, both over the limit (LOP 1). Absent: LOP 1 each. Half day until
// regularized: 0.5 each. Present until regularized: none. With an action the day is no late mark: nothing counted in
// the late-mark limit, so no double penalty. The single-day status (DevDay.Status) agrees with the month.
func TestDevAction(t *testing.T) {
	p := policy.Defaults()
	p.Grace, p.Att.LateN, p.Att.LateAct = 15, 0, "half"
	days := func() []DayIn {
		return []DayIn{
			devDay("2026-11-02", "11:00", "19:00"), // 60 min late: allowed (1 of 1)
			devDay("2026-11-03", "11:00", "19:00"), // 2nd: beyond the times allowed
			devDay("2026-11-04", "12:30", "20:30"), // 150 min: beyond the minutes
			devDay("2026-11-05", "10:00", "18:00"),
		}
	}
	for _, x := range []struct {
		act, status string
		lop         float64
		late, over  int
	}{{policy.DevActNone, "Late", 1, 2, 2}, {"", "Late", 1, 2, 2}, {policy.DevActAbsent, StAbsentReg, 2, 0, 0},
		{policy.DevActHalf, StHalfReg, 1, 0, 0}, {policy.DevActPresent, StPresentReg, 0, 0, 0}} {
		p.Dev.LateIn = policy.DevRule{On: true, Mins: 120, N: 1, Act: x.act}
		d := days()
		m := devMonth(p, d)
		for _, i := range []int{1, 2} {
			o := m.Days[i]
			if o.Status != x.status || (x.status != "Late" && o.Reg != x.act) {
				t.Errorf("%q %s: %s reg %q (%s), want %s", x.act, o.Date, o.Status, o.Reg, o.Note, x.status)
			}
			if st, _ := d[i].Dev.Status(p, d[i].Plan, d[i].Rec, nil, false); st != x.status {
				t.Errorf("%q %s: single-day status %s, want %s", x.act, o.Date, st, x.status)
			}
			if x.status != "Late" && !strings.Contains(o.Note, ": beyond the allowance → "+x.status) {
				t.Errorf("%q note: %q", x.act, o.Note)
			}
		}
		if m.Days[0].Status != "Present" || m.Days[0].LOP != 0 || m.Days[3].Status != "Present" {
			t.Errorf("%q: the allowed day %s, the on-time day %s", x.act, m.Days[0].Status, m.Days[3].Status)
		}
		if m.LOP != x.lop || m.Counts["Late"] != x.late || m.LateOver != x.over {
			t.Errorf("%q: LOP %v, late %d, over %d; want %v, %d, %d", x.act, m.LOP, m.Counts["Late"], m.LateOver, x.lop, x.late, x.over)
		}
		if x.status != "Late" && m.Counts[x.status] != 2 {
			t.Errorf("%q: counts %v", x.act, m.Counts)
		}
	}

	// Two rules beyond on one day: the harsher action wins. 60 min late (beyond 30: Present until regularized) and
	// 60 min early out (beyond 30: Half day until regularized), 6 h worked: Half day until regularized, LOP 0.5.
	p.Dev.LateIn = policy.DevRule{On: true, Mins: 30, N: 5, Act: policy.DevActPresent}
	p.Dev.EarlyOut = policy.DevRule{On: true, Mins: 30, N: 5, Act: policy.DevActHalf}
	two := devMonth(p, []DayIn{devDay("2026-11-02", "11:00", "17:00")})
	if o := two.Days[0]; o.Status != StHalfReg || o.LOP != 0.5 || o.Reg != policy.DevActHalf || two.Counts["Late"] != 0 {
		t.Errorf("two rules beyond: %s LOP %v reg %q (%s)", o.Status, o.LOP, o.Reg, o.Note)
	}
	p.Dev.EarlyOut.Act = policy.DevActAbsent
	if o := devMonth(p, []DayIn{devDay("2026-11-02", "11:00", "17:00")}).Days[0]; o.Status != StAbsentReg || o.LOP != 1 {
		t.Errorf("absent over present: %s LOP %v", o.Status, o.LOP)
	}

	// The hours rules still apply: 5 h late and 3 h worked is Absent (short hours) with Half day until regularized,
	// LOP 1 (not 1.5), marked to regularize.
	p.Dev.EarlyOut = policy.DevRule{}
	p.Dev.LateIn = policy.DevRule{On: true, Mins: 30, N: 5, Act: policy.DevActHalf}
	if o := devMonth(p, []DayIn{devDay("2026-11-02", "15:00", "18:00")}).Days[0]; o.Status != "Absent (short hours)" || o.LOP != 1 || o.Reg != policy.DevActHalf ||
		!strings.Contains(o.Note, "the hours or punches give more loss of pay: Absent (short hours)") {
		t.Errorf("hours worse than the action: %s LOP %v reg %q (%s)", o.Status, o.LOP, o.Reg, o.Note)
	}
}
