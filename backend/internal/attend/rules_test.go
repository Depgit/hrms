package attend

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

// ---------- The engine as it was before shift rules, kept verbatim as the reference ----------

func legacyStatus(p policy.Settings, plan Plan, rec *Rec, lateStart *int, override bool) (string, string) {
	if rec == nil || rec.Empty() {
		switch plan.Code {
		case "WO":
			return "Week off", "n"
		case "PH":
			return "Holiday", "n"
		}
		return "No punch", "bad"
	}
	if rec.In == "" {
		return "Missing in punch", "warn"
	}
	a, _ := Min(rec.In)
	var b int
	if rec.Out == "" {
		if p.Att.AutoOut == "end" && plan.Shift != nil {
			b = plan.Shift.End
		} else {
			return "Missing out punch", "warn"
		}
	} else {
		b, _ = Min(rec.Out)
	}
	if b <= a {
		b += 1440
	}
	var ps *int
	if plan.Shift != nil {
		ps = &plan.Shift.Start
	}
	h := NetHours(p, a, b, ps)
	ls := ps
	if override {
		ls = lateStart
	}
	// Decision 13 Nov 2026: a late arrival inside the grace counts as on time (its minutes are credited).
	if l := lateBy(a, ls); l > 0 && float64(l) <= p.Grace {
		h += float64(l) / 60
	}
	if h < p.HalfH {
		return "Absent (short hours)", "bad"
	}
	if h < p.FullH {
		return "Half day", "warn"
	}
	if ls != nil && float64(a) > float64(*ls)+p.Grace {
		return "Late", "warn"
	}
	return "Present", "ok"
}

func legacyFinalMonth(p policy.Settings, days []DayIn) Month {
	m := Month{Counts: map[string]int{}, DaysN: len(days)}
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
		case d.Future:
			o.Status, o.Tone = "Upcoming", "n"
			m.Counts["Upcoming"]++
		default:
			o.Status, o.Tone = legacyStatus(p, d.Plan, d.Rec, d.LateFrom, d.Override)
			if (d.Plan.Code == "WO" || d.Plan.Code == "PH") && d.Rec != nil && !d.Rec.Empty() { // RUN-78: a day off stays a day off
				o.Status, o.Tone, o.Note = map[bool]string{true: "Week off", false: "Holiday"}[d.Plan.Code == "WO"], "n", "Worked on the day off"
				if ev := Effective(&p, nil).Eval(d.Plan, d.Rec, d.LateFrom, d.Override); ev.Counted {
					o.Note += " (" + HoursText(ev.Hours) + ")"
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
				switch p.Att.Missing {
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

// policies covers the settings the engine reads, so equivalence is checked beyond the defaults.
func policies() []policy.Settings {
	out := []policy.Settings{policy.Defaults()}
	p := policy.Defaults()
	p.RS.EarlyCount = "yes"
	p.Att.AutoOut, p.Att.Missing, p.Att.Absent = "end", "absent", "lopnow"
	out = append(out, p)
	p = policy.Defaults()
	p.RS.EarlyIn, p.Att.Missing = 0, "present"
	out = append(out, p)
	p = policy.Defaults()
	p.FullH, p.HalfH, p.Grace, p.RS.EarlyIn, p.Att.Absent = 9, 4.5, 5, 45, "present"
	out = append(out, p)
	return out
}

func hm(m int) string { return HHMM(m) }

// The day's status is the work policy's alone (owner, 24 Nov 2026: the shift's attendance rules were removed): hours,
// status and deviations match the engine as it was before shift rules for every policy variant, every shift and a
// dense grid of punches (with and without the out punch), whatever the plan's rules hold (nil, empty, next day).
func TestEmptyRulesAreToday(t *testing.T) {
	n := 0
	yes, no := true, false
	for _, p := range policies() {
		for _, r := range []*Rules{nil, {}, {NextDay: &yes}, {NextDay: &no}} {
			e := Effective(&p, r)
			for si := -1; si < len(shifts); si++ {
				var sh *Shift
				plan := Plan{Code: "WO", Rules: r}
				if si >= 0 {
					sh = &shifts[si]
					plan = Plan{Code: sh.Code, Shift: sh, Rules: r}
				}
				for a := 0; a < 1440; a += 13 {
					for b := 0; b < 1440; b += 29 {
						var ps *int
						if sh != nil {
							ps = &sh.Start
						}
						if got, want := e.Hours(a, b, sh), NetHours(p, a, b, ps); got != want {
							t.Fatalf("hours %s-%s shift %v: %v, want %v", hm(a), hm(b), sh, got, want)
						}
						rec := &Rec{In: hm(a), Out: hm(b)}
						for _, rc := range []*Rec{rec, {In: hm(a)}, {Out: hm(b)}, nil} {
							for _, ov := range []bool{false, true} {
								ls := ps
								if ov && sh != nil {
									x := sh.Start + 120
									ls = &x
								}
								gs, gt := Status(p, plan, rc, ls, ov)
								ws, wt := legacyStatus(p, Plan{Code: plan.Code, Shift: sh}, rc, ls, ov)
								if gs != ws || gt != wt {
									t.Fatalf("status %+v plan %s: %s %s, want %s %s", rc, plan.Code, gs, gt, ws, wt)
								}
								n++
							}
						}
						if sh != nil { // extra hours are no longer a deviation (no overtime is paid)
							if v := Deviate(p, plan, rec, "2030-04-10", "L1", shifts); v != nil && contains(v.Kinds, "Extra hours") {
								t.Fatalf("extra hours %s-%s %s: %+v", hm(a), hm(b), sh.Code, v)
							}
						}
					}
				}
			}
		}
	}
	if n < 100000 {
		t.Fatalf("only %d cases", n)
	}
}

// Empty rules = today for whole months: random months (plans, punches, leave, joining, policy variants)
// give exactly the old FinalMonth.
func TestEmptyRulesMonthIsToday(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	pols := policies()
	for k := 0; k < 400; k++ {
		p := pols[k%len(pols)]
		p.Att.LateAct = []string{"half", "warn", "none"}[k%3]
		days := []DayIn{}
		for i := 0; i < 31; i++ {
			d := policy.AddDays("2026-10-01", i)
			in := DayIn{Date: d, AgeDays: rng.Intn(20), OptHol: rng.Intn(9) == 0}
			switch x := rng.Intn(20); {
			case x == 0:
				in.Before = true
			case x == 1:
				in.After = true
			case x == 2:
				in.Leave, in.Unpaid = "Casual leave", rng.Intn(2) == 0
			case x == 3:
				in.Future = true
			default:
				code := []string{"G", "M", "E", "N", "WO", "PH"}[rng.Intn(6)]
				in.Plan = Plan{Code: code, Shift: shiftOf(code)}
				if rng.Intn(8) > 0 {
					a, b := rng.Intn(1440), rng.Intn(1440)
					r := &Raw{In: hm(a), Out: hm(b)}
					switch rng.Intn(6) {
					case 0:
						r.Out = ""
					case 1:
						r.In = ""
					}
					in.Rec = Day(nil, r, d, p.Att.MaxSpan)
				}
				if rng.Intn(10) == 0 {
					s := 600
					in.LateFrom, in.Override = &s, true
				}
			}
			days = append(days, in)
		}
		want := legacyFinalMonth(p, days)
		yes := true
		for _, r := range []*Rules{nil, {}, {NextDay: &yes}} {
			ds := append([]DayIn{}, days...)
			for i := range ds {
				ds[i].Plan.Rules = r
			}
			if got := FinalMonth(p, ds); !reflect.DeepEqual(got, want) {
				t.Fatalf("month %d (rules %v) differs:\n got %+v\nwant %+v", k, r, got, want)
			}
		}
	}
}

func TestDurAndWindowed(t *testing.T) {
	for s, want := range map[string]int{"4:30": 270, "04:30": 270, "0:00": 0, "120:15": 7215, "12": -1, "4:60": -1, "-1:00": -1, "+1:00": -1, "1234:00": -1, "4:3": -1, "": -1} {
		got, ok := Dur(s)
		if (want < 0 && ok) || (want >= 0 && (!ok || got != want)) {
			t.Errorf("Dur(%q) = %d %v, want %d", s, got, ok, want)
		}
	}
	yes := true
	for r, want := range map[*Rules]bool{nil: false, {}: false, {NextDay: &yes}: true} {
		if r.Windowed(0, 0) != want {
			t.Errorf("Windowed(%+v) = %v", r, !want)
		}
		// The work policy's punch limits window every day (owner, 24 Nov 2026).
		if !r.Windowed(60, 0) || !r.Windowed(0, 120) {
			t.Errorf("Windowed(%+v) with a policy punch limit must be true", r)
		}
	}
}

// ---------- Each rule ----------

var g = shiftOf("G") // 09:00-18:00

func st(p policy.Settings, r *Rules, in, out string) string {
	s, _ := Status(p, Plan{Code: "G", Shift: g, Rules: r}, &Rec{In: in, Out: out}, nil, false)
	return s
}

func hrs(p policy.Settings, r *Rules, sh *Shift, in, out string) float64 {
	a, _ := Min(in)
	b, _ := Min(out)
	return Effective(&p, r).Hours(a, b, sh)
}

// Hours follow the work policy: an early check-in inside its allowance counts from the shift start, staying late counts.
func TestStartAndEndFrom(t *testing.T) {
	p := policy.Defaults() // early check-in allowance 30 min, not counted
	cases := []struct {
		in, out string
		want    float64
	}{
		{"08:45", "17:00", 8},  // inside the allowance: from the shift start
		{"08:00", "17:00", 9},  // outside it: from the punch
		{"09:00", "20:00", 11}, // staying late counts
	}
	for _, c := range cases {
		if got := hrs(p, nil, g, c.in, c.out); got != c.want {
			t.Errorf("%s-%s: %v h, want %v", c.in, c.out, got, c.want)
		}
	}
}

// A missing out punch follows the work policy (the shift's Add missing last out entry and Forced time out were
// removed, 24 Nov 2026): auto-close at the end of the shift's last part.
func TestMissingOutAndForcedOut(t *testing.T) {
	p := policy.Defaults() // autoOut none: a missing out punch stays missing
	if s := st(p, nil, "09:00", ""); s != "Missing out punch" {
		t.Error("policy:", s)
	}
	pe := policy.Defaults()
	pe.Att.AutoOut = "end"
	if s := st(pe, nil, "09:00", ""); s != "Present" {
		t.Error("auto-close at the shift end (9 h):", s)
	}
	sp := &Shift{Code: "SP", Start: 360, End: 600, Start2: 900, End2: 1140}
	if h, ok := Effective(&pe, nil).Counted(Plan{Code: "SP", Shift: sp}, &Rec{In: "06:00"}); !ok || h != 8 {
		t.Error("a split shift auto-closes at the end of its second part, the gap not counted:", h, ok)
	}
}

// The policy's hour thresholds decide every day: no short-hours band, whatever the shift.
func TestThresholdsAndShortHours(t *testing.T) {
	p := policy.Defaults() // half day below 8 h, absent below 4 h
	yes := true
	for _, r := range []*Rules{nil, {NextDay: &yes}} {
		for out, want := range map[string]string{"12:30": "Absent (short hours)", "13:00": "Half day", "16:59": "Half day", "17:00": "Present"} {
			if s := st(p, r, "09:00", out); s != want {
				t.Errorf("rules %+v 09:00-%s: %s, want %s", r, out, s, want)
			}
		}
	}
}

func TestSinglePunchByDay(t *testing.T) {
	p := policy.Defaults() // a missing punch counts as a half day
	one := &Rec{In: "09:00"}
	day := func(d string, r *Rules) DayIn {
		return DayIn{Date: d, Plan: Plan{Code: "G", Shift: g, Rules: r}, Rec: one}
	}
	// The shift's single punch rule was removed (24 Nov 2026): the work policy's "A day with a missing punch counts
	// as" decides every day, whatever rules the shift has.
	no := false
	m := FinalMonth(p, []DayIn{day("2026-11-02", nil), day("2026-11-03", &Rules{NextDay: &no})})
	if lops := []float64{m.Days[0].LOP, m.Days[1].LOP}; !reflect.DeepEqual(lops, []float64{0.5, 0.5}) || m.LOP != 1 {
		t.Errorf("single punch by the work policy: %v total %v", lops, m.LOP)
	}
	p.Att.Missing = "absent"
	if m = FinalMonth(p, []DayIn{day("2026-11-02", &Rules{NextDay: &no})}); m.LOP != 1 {
		t.Errorf("policy absent: %+v", m.Days[0])
	}
}

func TestEffectiveFallbacks(t *testing.T) {
	p := policy.Defaults()
	e := Effective(&p, nil)
	if e.AbsentH != p.HalfH || e.HalfH != p.FullH || e.OfficeH != p.FullH || e.Missing != p.Att.Missing || e.AutoOut {
		t.Errorf("policy values: %+v", e)
	}
	yes := true
	if Effective(&p, &Rules{NextDay: &yes}) != e {
		t.Error("a shift's rules change nothing but the punch window")
	}
}

// REAL-25 (decision 13 Nov 2026): on an 8-hour shift with an 8-hour full day, arriving inside the grace
// is on time (Present, a full day); beyond the grace the hour thresholds and the late mark apply as before.
func TestGraceArrivalOnEightHourShift(t *testing.T) {
	p := policy.Defaults() // full day 8 h, half day 4 h, grace 15 min
	m := &Shift{Code: "M", Start: 6 * 60, End: 14 * 60}
	plan := Plan{Code: "M", Shift: m}
	for in, want := range map[string]string{"06:00": "Present", "06:04": "Present", "06:15": "Present", "06:16": "Half day", "06:40": "Half day"} {
		if s, _ := Status(p, plan, &Rec{In: in, Out: "14:00"}, nil, false); s != want {
			t.Errorf("%s-14:00: %s, want %s", in, s, want)
		}
	}
	// Inside grace but leaving early is still short: the credit is only the late minutes.
	if s, _ := Status(p, plan, &Rec{In: "06:10", Out: "13:50"}, nil, false); s != "Half day" {
		t.Errorf("06:10-13:50: %s, want Half day", s)
	}
	// A late arrival with enough hours is still a late mark.
	if s, _ := Status(p, plan, &Rec{In: "06:30", Out: "14:45"}, nil, false); s != "Late" {
		t.Errorf("06:30-14:45: %s, want Late", s)
	}
	// Night shift across midnight: 22:00-06:00, in at 22:10.
	n := &Shift{Code: "N", Start: 22 * 60, End: 6 * 60}
	if s, _ := Status(p, Plan{Code: "N", Shift: n}, &Rec{In: "22:10", Out: "06:00"}, nil, false); s != "Present" {
		t.Errorf("night 22:10-06:00: %s, want Present", s)
	}
	// The month: no LOP for the grace day.
	mo := FinalMonth(p, []DayIn{{Date: "2026-10-07", Plan: plan, Rec: &Rec{In: "06:04", Out: "14:00"}}})
	if d := mo.Days[0]; d.Status != "Present" || d.LOP != 0 {
		t.Errorf("month day: %+v", d)
	}
}
