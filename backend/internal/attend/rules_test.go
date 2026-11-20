package attend

import (
	"math"
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

// Empty rules = today: hours, status and deviations match the old engine for every policy variant, every
// shift and a dense grid of punches (with and without the out punch), whether the plan has nil or empty rules.
func TestEmptyRulesAreToday(t *testing.T) {
	n := 0
	for _, p := range policies() {
		for _, r := range []*Rules{nil, {}, {WeeklyAdj: &Adj{}, Shortfall: &Shortfall{}}} {
			r.Normalize()
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
		for _, r := range []*Rules{nil, {}} {
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
	for r, want := range map[*Rules]bool{nil: false, {}: false, {NextDay: &yes}: true, {ShiftIn: "current"}: false, {ShiftIn: "previous"}: true, {EarlyInMax: "1:00"}: true,
		{LateOutMax: "2:00"}: true, {HalfBelow: "5:00"}: false} {
		if r.Windowed() != want {
			t.Errorf("Windowed(%+v) = %v", r, !want)
		}
	}
	r := &Rules{WeeklyAdj: &Adj{On: false, Hours: "4:00"}, Shortfall: &Shortfall{}, StartFrom: " shift "}
	r.Normalize()
	if r.WeeklyAdj != nil || r.Shortfall != nil || r.StartFrom != "shift" || r.Empty() {
		t.Errorf("normalize: %+v", r)
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

func TestStartAndEndFrom(t *testing.T) {
	p := policy.Defaults() // early check-in allowance 30 min, not counted
	cases := []struct {
		r       *Rules
		in, out string
		want    float64
	}{
		{nil, "08:45", "17:00", 8},                           // inside the allowance: from the shift start
		{nil, "08:00", "17:00", 9},                           // outside it: from the punch
		{&Rules{StartFrom: "shift"}, "08:00", "17:00", 8},    // from the shift start, however early
		{&Rules{StartFrom: "punch"}, "08:45", "17:00", 8.25}, // from the in punch
		{&Rules{StartFrom: "shift"}, "09:30", "18:00", 8.5},  // a late arrival still counts from the punch
		{nil, "09:00", "20:00", 11},                          // staying late counts (today)
		{&Rules{EndFrom: "shift"}, "09:00", "20:00", 9},      // hours stop at the shift end
		{&Rules{EndFrom: "punch"}, "09:00", "20:00", 11},
		{&Rules{EndFrom: "shift"}, "09:00", "16:00", 7},                     // leaving early is not padded
		{&Rules{BasedOn: "shift"}, "08:00", "20:00", 9},                     // only hours inside the shift
		{&Rules{BasedOn: "shift"}, "18:30", "20:00", 0},                     // all outside the shift
		{&Rules{MaxHours: "10:00"}, "06:00", "20:00", 10},                   // capped
		{&Rules{MaxHours: "10:00", EndFrom: "shift"}, "06:00", "20:00", 10}, // 06:00-18:00 by the end rule is 12 h, capped at 10
	}
	for _, c := range cases {
		if got := hrs(p, c.r, g, c.in, c.out); got != c.want {
			t.Errorf("%+v %s-%s: %v h, want %v", c.r, c.in, c.out, got, c.want)
		}
	}
	// Night shift: the end rule stops at 06:00 the next morning.
	n := shiftOf("N")
	if got := hrs(p, &Rules{EndFrom: "shift"}, n, "22:00", "08:00"); got != 8 {
		t.Errorf("night shift end from shift: %v", got)
	}
}

func TestMissingOutAndForcedOut(t *testing.T) {
	p := policy.Defaults() // autoOut none: a missing out punch stays missing
	yes, no := true, false
	if s := st(p, nil, "09:00", ""); s != "Missing out punch" {
		t.Error("policy:", s)
	}
	if s := st(p, &Rules{AddMissingOut: &yes}, "09:00", ""); s != "Present" {
		t.Error("add missing out at the shift end (9 h):", s)
	}
	if s := st(p, &Rules{AddMissingOut: &yes, ForcedOut: "15:00"}, "09:00", ""); s != "Half day" {
		t.Error("forced time out 15:00 gives 6 h:", s)
	}
	if s := st(p, &Rules{ForcedOut: "15:00"}, "09:00", ""); s != "Missing out punch" {
		t.Error("forced time out alone does not add the out:", s)
	}
	pe := policy.Defaults()
	pe.Att.AutoOut = "end"
	if s := st(pe, &Rules{AddMissingOut: &no}, "09:00", ""); s != "Missing out punch" {
		t.Error("the shift switches the policy's auto out off:", s)
	}
	n := Plan{Code: "N", Shift: shiftOf("N"), Rules: &Rules{AddMissingOut: &yes, ForcedOut: "05:00"}}
	if s, _ := Status(p, n, &Rec{In: "22:00"}, nil, false); s != "Half day" {
		t.Error("night shift forced out 05:00 the next morning is 7 h:", s)
	}
	if h, ok := Effective(&p, n.Rules).Counted(n, &Rec{In: "22:00"}); !ok || h != 7 {
		t.Error("counted", h, ok)
	}
}

func TestThresholdsAndShortHours(t *testing.T) {
	p := policy.Defaults() // half day below 8 h, absent below 4 h
	r := &Rules{AbsentBelow: "3:00", HalfBelow: "5:00", FullAbove: "8:00"}
	for out, want := range map[string]string{"11:30": "Absent (short hours)", "13:00": "Half day", "15:00": "Short hours", "17:00": "Present", "18:00": "Present"} {
		if s := st(p, r, "09:00", out); s != want {
			t.Errorf("09:00-%s: %s, want %s", out, s, want)
		}
	}
	if s := st(p, nil, "09:00", "15:00"); s != "Half day" {
		t.Error("policy has no short-hours band:", s)
	}
	if s := st(p, &Rules{FullAbove: "9:00"}, "09:00", "17:30"); s != "Short hours" {
		t.Error("full day rule alone: half day is the policy's 8 h, 8.5 h is short:", s)
	}
	if s := st(p, &Rules{HalfBelow: "6:00"}, "09:00", "16:00"); s != "Short hours" {
		t.Error("half day rule alone: the full day rule stays at the policy's 8 h:", s)
	}
	// With no adjustment a short-hours day is a half day in the month.
	m := FinalMonth(p, []DayIn{{Date: "2026-11-02", Plan: Plan{Code: "G", Shift: g, Rules: r}, Rec: &Rec{In: "09:00", Out: "15:00"}}})
	if d := m.Days[0]; d.Status != "Half day" || d.LOP != 0.5 || !strings.Contains(d.Note, "Short by 2:00, not covered") || m.Counts["Half day"] != 1 {
		t.Errorf("uncovered short day: %+v %v", d, m.Counts)
	}
}

func TestBasedOnPunch(t *testing.T) {
	p := policy.Defaults()
	r := &Rules{BasedOn: "punch"}
	for _, c := range []struct{ in, out, want string }{{"09:00", "11:00", "Present"}, {"09:30", "11:00", "Late"}, {"09:00", "", "Present"}, {"", "18:00", "Missing in punch"}} {
		if s := st(p, r, c.in, c.out); s != c.want {
			t.Errorf("punch %s-%s: %s, want %s", c.in, c.out, s, c.want)
		}
	}
	if s, _ := Status(p, Plan{Code: "WO", Rules: r}, &Rec{In: "09:00", Out: "10:00"}, nil, false); s != "Absent (short hours)" {
		t.Error("a weekly off is not a working day:", s)
	}
}

func TestSinglePunchByDay(t *testing.T) {
	p := policy.Defaults() // a missing punch counts as a half day
	one := &Rec{In: "09:00"}
	day := func(d string, r *Rules) DayIn {
		return DayIn{Date: d, Plan: Plan{Code: "G", Shift: g, Rules: r}, Rec: one}
	}
	m := FinalMonth(p, []DayIn{day("2026-11-02", nil), day("2026-11-03", &Rules{SinglePunch: "absent"}), day("2026-11-04", &Rules{SinglePunch: "present"}),
		day("2026-11-05", &Rules{SinglePunch: "half"})})
	if lops := []float64{m.Days[0].LOP, m.Days[1].LOP, m.Days[2].LOP, m.Days[3].LOP}; !reflect.DeepEqual(lops, []float64{0.5, 1, 0, 0.5}) || m.LOP != 2 {
		t.Errorf("single punch by each day's shift: %v total %v", lops, m.LOP)
	}
	yes := true
	m = FinalMonth(p, []DayIn{day("2026-11-02", &Rules{SinglePunch: "absent", AddMissingOut: &yes})})
	if m.Days[0].Status != "Present" || m.LOP != 0 {
		t.Errorf("with a forced out the day is not a single punch: %+v", m.Days[0])
	}
}

// ---------- Weekly and monthly adjustment ----------

// shortRules: half day below 5 h, full day from 8 h, so 5-8 h is short hours; office hours 8 h.
func shortRules(mod func(r *Rules)) *Rules {
	r := &Rules{HalfBelow: "5:00", FullAbove: "8:00"}
	if mod != nil {
		mod(r)
	}
	return r
}

func gday(d, in, out string, r *Rules) DayIn {
	return DayIn{Date: d, Plan: Plan{Code: "G", Shift: g, Rules: r}, Rec: &Rec{In: in, Out: out}, AgeDays: 3}
}

func statuses(m Month) []string {
	out := []string{}
	for _, d := range m.Days {
		out = append(out, d.Status)
	}
	return out
}

func TestWeeklyAdjustment(t *testing.T) {
	p := policy.Defaults()
	wk := shortRules(func(r *Rules) { r.WeeklyAdj = &Adj{On: true, Hours: "4:00"} })
	// Week of Mon 2 Nov 2026: Monday 7 h (1 h short), Tuesday 10 h (2 h extra).
	m := FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", wk), gday("2026-11-03", "09:00", "19:00", wk)})
	if s := statuses(m); s[0] != "Present" || m.LOP != 0 || !strings.Contains(m.Days[0].Note, "weekly") {
		t.Errorf("covered by the week's extra hours: %v %v %+v", s, m.LOP, m.Days[0])
	}
	// Extra hours later in the week count too; the short day is settled first come, oldest first.
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "15:30", wk), gday("2026-11-03", "09:00", "16:00", wk), gday("2026-11-06", "09:00", "19:00", wk)})
	if s := statuses(m); !reflect.DeepEqual(s, []string{"Present", "Half day", "Present"}) {
		t.Errorf("1.5 h and 1 h short, 2 h extra: the oldest is covered, 0.5 h is not enough for the next: %v", s)
	}
	// Cap: 0:30 a week cannot cover a 1 h shortfall.
	capped := shortRules(func(r *Rules) { r.WeeklyAdj = &Adj{On: true, Hours: "0:30"} })
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", capped), gday("2026-11-03", "09:00", "19:00", capped)})
	if s := statuses(m); s[0] != "Half day" || m.LOP != 0.5 {
		t.Errorf("over the weekly cap: %v", s)
	}
	// Extra hours of another week do not count for the weekly adjustment.
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", wk), gday("2026-11-09", "09:00", "19:00", wk)})
	if s := statuses(m); s[0] != "Half day" {
		t.Errorf("next week's extra hours: %v", s)
	}
	// Late and covered stays late.
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:30", "16:30", wk), gday("2026-11-03", "09:00", "19:00", wk)})
	if s := statuses(m); s[0] != "Late" || m.Counts["Late"] != 1 {
		t.Errorf("late short day covered: %v", s)
	}
	// Office hours: shortfall measured against 9 h needs 2 h.
	off := shortRules(func(r *Rules) { r.WeeklyAdj = &Adj{On: true, Hours: "4:00"}; r.OfficeHours = "9:00" })
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", off), gday("2026-11-03", "09:00", "19:00", off)})
	if s := statuses(m); s[0] != "Half day" { // 2 h short, only 1 h extra beyond 9 h
		t.Errorf("office hours 9: %v", s)
	}
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", off), gday("2026-11-03", "09:00", "20:00", off)})
	if s := statuses(m); s[0] != "Present" {
		t.Errorf("office hours 9, 2 h extra: %v", s)
	}
	// Extra hours count after the max-hours cap.
	mx := shortRules(func(r *Rules) { r.WeeklyAdj = &Adj{On: true, Hours: "4:00"}; r.MaxHours = "8:30" })
	m = FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", mx), gday("2026-11-03", "09:00", "20:00", mx)})
	if s := statuses(m); s[0] != "Half day" {
		t.Errorf("only 0.5 h extra after the cap: %v", s)
	}
}

func TestShortfallInclusions(t *testing.T) {
	p := policy.Defaults()
	worked := func(code, d string) DayIn {
		return DayIn{Date: d, Plan: Plan{Code: code}, Rec: &Rec{In: "10:00", Out: "12:00"}}
	}
	run := func(in Shortfall, extra DayIn) string {
		r := shortRules(func(r *Rules) { r.WeeklyAdj = &Adj{On: true, Hours: "4:00"}; r.Shortfall = &in })
		return FinalMonth(p, []DayIn{gday("2026-11-02", "09:00", "16:00", r), extra}).Days[0].Status
	}
	if s := run(Shortfall{}, worked("WO", "2026-11-08")); s != "Half day" {
		t.Error("weekly-off hours not included:", s)
	}
	if s := run(Shortfall{WO: true}, worked("WO", "2026-11-08")); s != "Present" {
		t.Error("weekly-off hours included:", s)
	}
	if s := run(Shortfall{WO: true}, worked("PH", "2026-11-05")); s != "Half day" {
		t.Error("holiday hours need their own tick:", s)
	}
	if s := run(Shortfall{Hol: true}, worked("PH", "2026-11-05")); s != "Present" {
		t.Error("holiday hours included:", s)
	}
	opt := gday("2026-11-04", "09:00", "17:00", nil) // 8 h on an optional holiday: no extra beyond office hours
	opt.OptHol = true
	if s := run(Shortfall{}, opt); s != "Half day" {
		t.Error("optional holiday not included:", s)
	}
	if s := run(Shortfall{OptHol: true}, opt); s != "Present" {
		t.Error("optional holiday hours included:", s)
	}
}

func TestMonthlyAdjustment(t *testing.T) {
	p := policy.Defaults()
	mo := shortRules(func(r *Rules) { r.MonthlyAdj = &Adj{On: true, Hours: "10:00"} })
	wkOnly := shortRules(func(r *Rules) { r.WeeklyAdj = &Adj{On: true, Hours: "10:00"} })
	both := shortRules(func(r *Rules) {
		r.WeeklyAdj = &Adj{On: true, Hours: "1:00"}
		r.MonthlyAdj = &Adj{On: true, Hours: "1:30"}
	})
	// Short in week 1, extra in week 3.
	days := func(r *Rules) []DayIn {
		return []DayIn{gday("2026-11-02", "09:00", "16:00", r), gday("2026-11-17", "09:00", "19:00", r)}
	}
	if s := statuses(FinalMonth(p, days(wkOnly))); s[0] != "Half day" {
		t.Error("weekly only:", s)
	}
	m := FinalMonth(p, days(mo))
	if s := statuses(m); s[0] != "Present" || !strings.Contains(m.Days[0].Note, "monthly") {
		t.Error("monthly:", s, m.Days[0].Note)
	}
	// Weekly first, then monthly, each with its own cap: three 1 h short days, 5 h extra in week 1.
	ds := []DayIn{gday("2026-11-02", "09:00", "16:00", both), gday("2026-11-03", "09:00", "16:00", both), gday("2026-11-04", "09:00", "16:00", both),
		gday("2026-11-05", "09:00", "22:00", both)}
	m = FinalMonth(p, ds)
	if s := statuses(m); !reflect.DeepEqual(s[:3], []string{"Present", "Present", "Half day"}) || !strings.Contains(m.Days[0].Note, "weekly") || !strings.Contains(m.Days[1].Note, "monthly") {
		t.Errorf("weekly cap 1 h then monthly cap 1.5 h: %v %q %q", s, m.Days[0].Note, m.Days[1].Note)
	}
	if m.LOP != 0.5 || m.Counts["Present"] != 3 || m.Counts["Half day"] != 1 {
		t.Errorf("lop %v counts %v", m.LOP, m.Counts)
	}
}

func TestEffectiveFallbacks(t *testing.T) {
	p := policy.Defaults()
	e := Effective(&p, nil)
	if e.AbsentH != p.HalfH || e.HalfH != p.FullH || e.FullH != p.FullH || e.OfficeH != p.FullH || e.MaxH != 0 || e.Missing != p.Att.Missing || e.AutoOut || e.ForcedOut != -1 {
		t.Errorf("policy fallbacks: %+v", e)
	}
	e = Effective(&p, &Rules{FullAbove: "9:00"})
	if e.OfficeH != 9 || e.HalfH != 8 {
		t.Errorf("office hours follow the full day rule: %+v", e)
	}
	if math.Abs(Effective(&p, &Rules{OfficeHours: "7:45"}).OfficeH-7.75) > 1e-9 {
		t.Error("office hours")
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
