package attend

import (
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

// TIME-180, TIME-183, TIME-200: hours worked on a weekly off cost no pay (4 h, 3 h); TIME-262: a regularized
// 06:05-14:00 on the 8-hour M shift is Present (arrival inside the grace).
func TestFtDayOffAndGrace(t *testing.T) {
	p := policy.Defaults()
	m := &Shift{Code: "M", Start: 6 * 60, End: 14 * 60}
	days := []DayIn{
		{Date: "2026-10-18", Plan: Plan{Code: "WO"}, Rec: &Rec{In: "10:00", Out: "14:00"}, AgeDays: 20},
		{Date: "2026-10-25", Plan: Plan{Code: "WO"}, Rec: &Rec{In: "10:00", Out: "13:00"}, AgeDays: 20},
		{Date: "2026-11-10", Plan: Plan{Code: "M", Shift: m}, Rec: &Rec{In: "06:05", Out: "14:00", Src: "Regularized"}, AgeDays: 3},
	}
	mo := FinalMonth(p, days)
	if mo.LOP != 0 {
		t.Fatalf("LOP %v: %+v", mo.LOP, mo.Days)
	}
	if mo.Days[0].Code != "W" || mo.Days[1].Code != "W" || mo.Days[2].Status != "Present" {
		t.Fatalf("%+v", mo.Days)
	}
}

// TIME-231: the rest gap reads "Consider shift in": a shift recorded the day after it starts runs a day earlier.
func TestFtRestGapShiftIn(t *testing.T) {
	g := &Shift{Code: "G", Start: 9 * 60, End: 18 * 60}
	nxt := &Shift{Code: "NXT", Start: 22 * 60, End: 6 * 60}
	if m, _ := RestBetweenRuled(g, nil, nxt, &Rules{ShiftIn: "next"}); m != 4*60 {
		t.Errorf("rest %d min, want 240", m)
	}
	if m, _ := RestBetween(g, nxt); m != 28*60 {
		t.Errorf("plain rest %d min, want 1680", m)
	}
}
