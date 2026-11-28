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

// The rest gap runs from the end of the shift's last part ("Consider shift in" was removed from shifts, 24 Nov 2026).
func TestFtRestGapShiftIn(t *testing.T) {
	g := &Shift{Code: "G", Start: 9 * 60, End: 18 * 60}
	nxt := &Shift{Code: "NXT", Start: 22 * 60, End: 6 * 60}
	if m, _ := RestBetween(g, nxt); m != 28*60 {
		t.Errorf("plain rest %d min, want 1680", m)
	}
	sp := &Shift{Code: "SP", Start: 6 * 60, End: 10 * 60, Start2: 15 * 60, End2: 19 * 60}
	if m, _ := RestBetween(sp, g); m != 14*60 {
		t.Errorf("rest after a split shift %d min, want 840 (19:00 to 09:00)", m)
	}
}
