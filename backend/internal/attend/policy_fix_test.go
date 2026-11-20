package attend

import (
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

// Work policies clean-up (8 Nov 2026): the engine no longer takes off a break, applies a late half day, or
// raises extra-hours, short-rest or off-day deviations; the discipline score starts pro-rata for a part month.

// oldPolicy is a policy stored before the clean-up, with every removed setting switched on.
func oldPolicy(t *testing.T) policy.Settings {
	t.Helper()
	p, err := policy.Parse([]byte(`{"compoff": true, "hol": {"work": "pay", "prob": true}, "shift": {"split": true, "rest": 11, "change": 2, "nightAllow": 150},
		"att": {"brkMin": 60, "brkAfter": 6, "lateHalf": 30, "minRest": 11}, "ot": {"on": true, "after": 30, "rate": "2x", "cap": 75},
		"rs": {"otTol": 30, "devExtra": true, "devRest": true, "devOff": true, "pts": 5}}`))
	if err != nil {
		t.Fatal("a stored policy with removed settings must still parse:", err)
	}
	return p
}

func TestOldPolicyStillParses(t *testing.T) {
	p := oldPolicy(t)
	if p.Shift.NightAllow != 150 || p.Att.MinRest != 11 || p.Shift.Rest != 11 || p.Shift.Change != 2 || p.RS.Pts != 5 || !p.Hol.Prob {
		t.Errorf("kept settings read as stored: %+v %+v %+v", p.Shift, p.Att, p.RS)
	}
}

func TestNoBreakDeducted(t *testing.T) {
	p := oldPolicy(t) // brkMin 60 after 6 h, as stored
	g := shiftOf("G")
	if h := NetHours(p, 540, 1080, &g.Start); h != 9 {
		t.Errorf("09:00-18:00 is 9 h, no break taken off: %v", h)
	}
	if h := Effective(&p, nil).Hours(540, 1080, g); h != 9 {
		t.Errorf("Effective hours, no break: %v", h)
	}
	if h := Effective(&p, &Rules{FullAbove: "8:00"}).Hours(540, 1080, g); h != 9 {
		t.Errorf("with shift rules, no break: %v", h)
	}
	// An 8 h day stays a full day (with the old 60 min break it was 7 h, a half day).
	if st, _, _ := run(p, "M", "06:00", "14:00"); st != "Present" {
		t.Errorf("8 h day: %s", st)
	}
}

func TestNoLateHalfDay(t *testing.T) {
	p := oldPolicy(t) // lateHalf 30, as stored
	if st, _, _ := run(p, "M", "06:45", "15:00"); st != "Late" {
		t.Errorf("45 min late is a late mark, not a half day: %s", st)
	}
	r := &Rules{FullAbove: "8:00"}
	if ev := Effective(&p, r).Eval(Plan{Code: "M", Shift: shiftOf("M"), Rules: r}, &Rec{In: "06:45", Out: "15:00"}, nil, false); ev.Status != "Late" {
		t.Errorf("with shift rules: %s", ev.Status)
	}
}

func TestNoExtraRestOrOffDayDeviations(t *testing.T) {
	p := oldPolicy(t) // devExtra, devRest and devOff were on
	m := Plan{Code: "M", Shift: shiftOf("M")}
	if v := Deviate(p, m, &Rec{In: "06:00", Out: "20:00"}, "2026-11-10", "L1", shifts); v != nil {
		t.Errorf("14 h on a 8 h shift is not a deviation: %+v", v)
	}
	if v := Deviate(p, m, &Rec{In: "06:00", Out: "14:00"}, "2026-11-10", "L1", shifts); v != nil {
		t.Errorf("rest after the previous day is not checked on the punches: %+v", v)
	}
	for _, code := range []string{"WO", "PH", "L"} {
		if v := Deviate(p, Plan{Code: code}, &Rec{In: "14:00", Out: "22:00"}, "2026-11-10", "L1", shifts); v != nil {
			t.Errorf("work on %s is not a deviation: %+v", code, v)
		}
	}
	// The deviations that stay: arriving outside the window, and leaving early when switched on.
	if v := Deviate(p, m, &Rec{In: "14:00", Out: "22:00"}, "2026-11-10", "L1", shifts); v == nil || v.Kinds[0] != "Shift" || v.Actual != "E" {
		t.Errorf("planned M, worked E: %+v", v)
	}
	p.RS.OutTol = 30
	if v := Deviate(p, m, &Rec{In: "06:00", Out: "12:00"}, "2026-11-10", "L1", shifts); v == nil || v.Kinds[0] != "Early exit" {
		t.Errorf("early exit: %+v", v)
	}
}

func TestStartScoreProRata(t *testing.T) {
	for _, c := range []struct {
		ym, doj, lwd string
		want         int
	}{
		{"2026-11", "2025-01-01", "", 100},           // the whole month
		{"2026-11", "2026-11-01", "", 100},           // joined on the 1st
		{"2026-11", "2026-11-21", "", 33},            // joined 21 Nov: 10 of 30 days
		{"2026-11", "2026-11-30", "", 3},             // the last day only
		{"2026-11", "2025-01-01", "2026-11-10", 33},  // left 10 Nov: 10 of 30 days
		{"2026-11", "2026-11-11", "2026-11-20", 33},  // joined and left in the month
		{"2026-11", "2025-01-01", "2026-12-15", 100}, // leaves next month
		{"2026-11", "2026-12-01", "", 0},             // not yet joined
		{"2026-02", "2026-02-15", "", 50},            // 14 of 28 days
		{"2026-10", "2026-10-16", "", 52},            // 16 of 31 days, rounded
	} {
		if got := StartScore(c.ym, c.doj, c.lwd); got != c.want {
			t.Errorf("%s joined %s left %q: start %d, want %d", c.ym, c.doj, c.lwd, got, c.want)
		}
	}
	if s := Score(33, 1, 5); s != 28 {
		t.Errorf("33 less one Not OK of 5 points: %d", s)
	}
	if s := Score(33, 9, 5); s != 0 {
		t.Errorf("never below 0: %d", s)
	}
	if s := Score(100, 2, 5); s != 90 {
		t.Errorf("full month: %d", s)
	}
}

func TestBandsScaleWithTheStart(t *testing.T) {
	for _, c := range []struct {
		score, start int
		want         string
	}{
		{100, 100, "Good"}, {90, 100, "Good"}, {89, 100, "Watch"}, {70, 100, "Watch"}, {69, 100, "Poor"},
		{33, 33, "Good"}, {30, 33, "Good"}, {29, 33, "Watch"}, {24, 33, "Watch"}, {23, 33, "Poor"},
		{0, 0, "Good"}, // not employed in the month
	} {
		if got := Band(c.score, c.start); got != c.want {
			t.Errorf("score %d of %d: %s, want %s", c.score, c.start, got, c.want)
		}
	}
	if Scaled(70, 33) != 23 || Scaled(70, 100) != 70 || Scaled(0, 33) != 0 {
		t.Error("a limit scales with the start", Scaled(70, 33), Scaled(70, 100))
	}
}
