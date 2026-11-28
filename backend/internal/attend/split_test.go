package attend

import (
	"math"
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

// Split shift (owner, 24 Nov 2026): 06:00–10:00 + 15:00–19:00. Hours are the time worked inside the two parts added
// together (the gap never counts); the day's status follows the hour rules; the late mark is measured from the first
// part's start.

var split = &Shift{Code: "SP", Start: 6 * 60, End: 10 * 60, Start2: 15 * 60, End2: 19 * 60}

func TestSplitShiftHours(t *testing.T) {
	p := policy.Defaults() // full day 8 h, half day 4 h, grace 15 min, early check-in 30 min not counted
	if !split.Split() || split.Hours() != 8 || split.LastEnd() != 19*60 || split.Timing() != "06:00–10:00 + 15:00–19:00" {
		t.Fatalf("split shift: split %v hours %v last end %v timing %q", split.Split(), split.Hours(), split.LastEnd(), split.Timing())
	}
	if one := shiftOf("G"); one.Split() || one.Timing() != "09:00–18:00" || one.LastEnd() != one.End {
		t.Fatalf("a shift in one part: %+v %q", one, one.Timing())
	}
	cases := []struct {
		in, out string
		hours   float64
		status  string
	}{
		{"06:00", "19:00", 8, "Present"},    // in across both parts: 13 h less the 5 h gap
		{"05:45", "19:00", 8, "Present"},    // early inside the allowance counts from the start
		{"06:00", "10:00", 4, "Half day"},   // one part worked is a half day by the hours
		{"15:00", "19:00", 4, "Half day"},   // the second part alone
		{"06:00", "13:00", 4, "Half day"},   // left during the gap: the gap does not count
		{"12:00", "19:00", 4, "Half day"},   // came during the gap
		{"14:50", "19:00", 4, "Half day"},   // early for the second part: the minutes in the gap do not count
		{"06:10", "19:00", 8, "Present"},    // late inside the grace: credited, not late
		{"06:30", "19:30", 8, "Late"},       // 30 min late, made up after the end: a full day, late from 06:00
		{"06:30", "19:00", 7.5, "Half day"}, // 30 min late, not made up
		{"07:00", "09:00", 2, "Absent (short hours)"},
	}
	for _, c := range cases {
		a, _ := Min(c.in)
		b, _ := Min(c.out)
		if h := Effective(&p, nil).Hours(a, b, split); c.in != "06:10" && math.Abs(h-c.hours) > 1e-9 { // 06:10: the grace minutes are credited by Eval
			t.Errorf("%s-%s: %v h, want %v", c.in, c.out, h, c.hours)
		}
		plan := Plan{Code: "SP", Shift: split}
		ev := Effective(&p, nil).Eval(plan, &Rec{In: c.in, Out: c.out}, nil, false)
		if ev.Status != c.status || math.Abs(ev.Hours-c.hours) > 1e-9 {
			t.Errorf("%s-%s: %s %v h, want %s %v h", c.in, c.out, ev.Status, ev.Hours, c.status, c.hours)
		}
	}
}

func TestSplitShiftMonth(t *testing.T) {
	p := policy.Defaults() // a missing punch counts as a half day
	day := func(d, in, out string) DayIn {
		return DayIn{Date: d, Plan: Plan{Code: "SP", Shift: split}, Rec: &Rec{In: in, Out: out}}
	}
	m := FinalMonth(p, []DayIn{day("2026-11-02", "06:00", "19:00"), day("2026-11-03", "15:00", "19:00"), day("2026-11-04", "06:00", "13:00"),
		day("2026-11-05", "06:00", "")})
	want := []struct {
		code string
		lop  float64
	}{{"P", 0}, {"H", 0.5}, {"H", 0.5}, {"M", 0.5}}
	for i, w := range want {
		if m.Days[i].Code != w.code || m.Days[i].LOP != w.lop {
			t.Errorf("day %d: %+v, want %s lop %v", i, m.Days[i], w.code, w.lop)
		}
	}
	if m.LOP != 1.5 {
		t.Errorf("month loss of pay %v, want 1.5", m.LOP)
	}
	// A missing out auto-closed at the shift end ends at the second part's end.
	p.Att.AutoOut = "end"
	if m = FinalMonth(p, []DayIn{day("2026-11-05", "06:00", "")}); m.Days[0].Code != "P" {
		t.Errorf("auto-closed at 19:00: %+v", m.Days[0])
	}
}

func TestSplitShiftDeviationAndShortLeave(t *testing.T) {
	p := policy.Defaults()
	p.RS.OutTol = 15
	plan := Plan{Code: "SP", Shift: split}
	// Leaving at the end of the second part is no early exit; leaving at the end of the first part is.
	if v := Deviate(p, plan, &Rec{In: "06:00", Out: "19:00"}, "2026-11-02", "L1", []Shift{*split}); v != nil {
		t.Errorf("full split day deviates: %+v", v)
	}
	if v := Deviate(p, plan, &Rec{In: "06:00", Out: "10:00"}, "2026-11-02", "L1", []Shift{*split}); v == nil || v.Kinds[0] != "Early exit" {
		t.Errorf("left after the first part: %+v", v)
	}
	// A short leave inside the gap credits nothing; one inside a part credits what was not worked.
	gap := Plan{Code: "SP", Shift: split, Short: []Window{{11 * 60, 13 * 60}}}
	if c := gap.shortCredit(6*60, 19*60); c != 0 {
		t.Errorf("short leave in the gap credited %v h", c)
	}
	in := Plan{Code: "SP", Shift: split, Short: []Window{{17 * 60, 19 * 60}}}
	if c := in.shortCredit(6*60, 17*60); c != 2 {
		t.Errorf("short leave at the end of the second part credited %v h, want 2", c)
	}
}
