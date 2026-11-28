package attend

import (
	"reflect"
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

var shifts = []Shift{{Code: "G", Start: 540, End: 1080}, {Code: "M", Start: 360, End: 840}, {Code: "E", Start: 840, End: 1320}, {Code: "N", Start: 1320, End: 360}}

func shiftOf(c string) *Shift {
	for i := range shifts {
		if shifts[i].Code == c {
			return &shifts[i]
		}
	}
	return nil
}

// status and deviation for one day planned on shift code, with the prototype's default policy.
func run(p policy.Settings, code, in, out string) (string, []string, string) {
	plan := Plan{Code: code, Shift: shiftOf(code)}
	rec := Day(nil, &Raw{In: in, Out: out}, "2030-04-10", p.Att.MaxSpan)
	v := Deviate(p, plan, rec, "2030-04-10", "L1", shifts)
	ls, ov := LateStartFor(v, shifts)
	st, _ := Status(p, plan, rec, ls, ov)
	if v == nil {
		return st, nil, ""
	}
	return st, v.Kinds, v.Actual
}

// Cases from the pack's t73.py and uat36.py, run with the default policy.
func TestDeviationDefaults(t *testing.T) {
	p := policy.Defaults()
	cases := []struct {
		code, in, out, status string
		kinds                 []string
		actual                string
	}{
		{"M", "06:00", "14:00", "Present", nil, ""},                      // on time
		{"M", "05:30", "14:00", "Present", nil, ""},                      // 30 min early is inside the allowance
		{"M", "05:29", "14:00", "Present", []string{"Shift"}, "M"},       // 31 min early is a deviation
		{"M", "05:00", "14:00", "Present", []string{"Shift"}, "M"},       // 60 min early, still nearest M
		{"M", "07:00", "14:00", "Half day", nil, ""},                     // 60 late is inside the tolerance; 7 h is a half day
		{"M", "07:01", "14:00", "Half day", []string{"Shift"}, "Custom"}, // 61 late, no shift starts near 07:01
		{"M", "06:00", "16:30", "Present", nil, ""},                      // 2.5 h extra is not a deviation (no overtime)
		{"M", "06:00", "13:00", "Half day", nil, ""},                     // early out is off by default
		{"E", "14:00", "23:30", "Present", nil, ""},                      // 1.5 h extra is not a deviation
		{"N", "21:35", "06:00", "Present", nil, ""},                      // 25 min early on a night shift
		{"M", "06:30", "15:30", "Late", nil, ""},                         // uat36: 30 min late keeps the late mark
		{"M", "07:30", "16:30", "Present", []string{"Shift"}, "Custom"},  // uat36: 90 min late, attendance by hours, no late mark
		{"M", "14:00", "22:00", "Present", []string{"Shift"}, "E"},       // uat36: planned M, worked E
		{"M", "06:00", "19:00", "Present", nil, ""},                      // uat36: extra hours alone, no longer a deviation
		{"M", "08:00", "18:00", "Present", []string{"Shift"}, "G"},       // reference sheet, A Singh (no extra-hours kind)
		{"E", "08:00", "18:00", "Present", []string{"Shift"}, "G"},       // reference sheet, B Singh day 2
		{"N", "22:00", "07:00", "Present", nil, ""},                      // reference sheet, C Singh: 1 h over is inside the tolerance
	}
	for _, c := range cases {
		st, kinds, actual := run(p, c.code, c.in, c.out)
		if c.actual == "" {
			c.actual = c.code // without a Shift deviation the actual shift is the planned one
		}
		if st != c.status || !reflect.DeepEqual(kinds, c.kinds) || (c.kinds != nil && actual != c.actual) {
			t.Errorf("%s %s-%s: got %s %v %s, want %s %v %s", c.code, c.in, c.out, st, kinds, actual, c.status, c.kinds, c.actual)
		}
	}
}

func TestDeviationSettings(t *testing.T) {
	p := policy.Defaults()
	p.RS.EarlyIn, p.RS.OutTol = 0, 30
	if _, k, _ := run(p, "M", "05:30", "14:00"); !reflect.DeepEqual(k, []string{"Shift"}) {
		t.Errorf("earlyIn 0: any early arrival is a deviation, got %v", k)
	}
	if _, k, _ := run(p, "M", "06:00", "13:00"); !reflect.DeepEqual(k, []string{"Early exit"}) {
		t.Errorf("outTol 30: leaving an hour early is a deviation, got %v", k)
	}
	p = policy.Defaults()
	p.RS.DevStart = false
	if _, k, _ := run(p, "M", "08:00", "18:00"); k != nil {
		t.Errorf("start switched off, got %v", k)
	}
	// No break is taken off and there is no late half day (both settings were removed): see policy_fix_test.go.
	if st, _, _ := run(policy.Defaults(), "M", "06:00", "14:00"); st != "Present" {
		t.Errorf("8 h day is a full day, no break taken off: %s", st)
	}
	// Work on a weekly off or a leave day is not a deviation any more.
	if v := Deviate(policy.Defaults(), Plan{Code: "WO"}, &Rec{In: "14:00", Out: "22:00"}, "2030-04-10", "L1", shifts); v != nil {
		t.Errorf("worked on weekly off: %+v", v)
	}
	if v := Deviate(policy.Defaults(), Plan{Code: "L"}, &Rec{In: "06:00", Out: "15:00"}, "2030-04-10", "L1", shifts); v != nil {
		t.Errorf("worked on leave day: %+v", v)
	}
}

// Punch rules from t72.py: an over-long span gives its out punch to the next day.
func TestPunchRules(t *testing.T) {
	p := policy.Defaults()
	g := Plan{Code: "G", Shift: shiftOf("G")}
	st := func(prev, cur *Raw, date string, plan Plan, max float64) (*Rec, string) {
		r := Day(prev, cur, date, max)
		s, _ := Status(p, plan, r, nil, false)
		return r, s
	}
	if _, s := st(nil, &Raw{In: "08:00", Out: "18:00"}, "2026-08-03", g, 16); s != "Present" {
		t.Error("normal 10 h day", s)
	}
	b := &Raw{In: "08:00", Out: "02:00"}
	if r, s := st(nil, b, "2026-08-05", g, 16); s != "Missing out punch" || r.Orphan != "02:00" {
		t.Error("18 h span drops the out punch", s, r)
	}
	if r, s := st(b, nil, "2026-08-06", g, 16); s != "Missing out punch" || !r.Carried || r.In != "02:00" {
		t.Error("the dropped out becomes the next day's check-in", s, r)
	}
	if r, s := st(b, &Raw{In: "09:00", Out: "17:00"}, "2026-08-06", g, 16); s != "Present" || r.In != "02:00" || r.Out != "17:00" {
		t.Error("carried check-in with the day's own punches", s, r)
	}
	if r, s := st(nil, &Raw{In: "08:00", Out: "18:00", OutDate: "2026-08-11"}, "2026-08-10", g, 16); s != "Missing out punch" || r.Orphan == "" {
		t.Error("34 h with dates is a missing punch", s, r)
	}
	if _, s := st(nil, &Raw{In: "22:00", Out: "07:00"}, "2026-08-12", Plan{Code: "N", Shift: shiftOf("N")}, 16); s != "Present" {
		t.Error("9 h night shift", s)
	}
	if _, s := st(nil, b, "2026-08-14", g, 0); s != "Present" {
		t.Error("max span 0 switches the rule off", s)
	}
	// Short rest after yesterday's punches is no longer a deviation (rest is checked on the roster only).
	if v := Deviate(p, Plan{Code: "M", Shift: shiftOf("M")}, &Rec{In: "06:00", Out: "14:00"}, "2026-08-21", "L1", shifts); v != nil {
		t.Error("short rest is not a deviation", v)
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"N", "M", 0}, {"E", "M", 480}, {"M", "E", 1440}, {"E", "N", 1440}} {
		if g, _ := RestBetween(shiftOf(c.a), shiftOf(c.b)); g != c.want {
			t.Errorf("rest %s→%s = %d, want %d", c.a, c.b, g, c.want)
		}
	}
}

func TestFinalMonth(t *testing.T) {
	p := policy.Defaults()
	g := Plan{Code: "G", Shift: shiftOf("G")}
	day := func(d, in, out string) DayIn {
		return DayIn{Date: d, Plan: g, Rec: Day(nil, &Raw{In: in, Out: out}, d, 16), AgeDays: 20}
	}
	days := []DayIn{
		{Date: "2026-09-01", Before: true},
		day("2026-09-02", "09:00", "18:00"),                      // present
		day("2026-09-03", "09:00", "14:00"),                      // half day 0.5
		day("2026-09-04", "09:00", ""),                           // missing out 0.5
		{Date: "2026-09-05", Plan: g, AgeDays: 20},               // no punch, window passed: 1
		{Date: "2026-09-06", Plan: Plan{Code: "WO"}},             // week off
		{Date: "2026-09-07", Leave: "Casual leave"},              // paid leave
		{Date: "2026-09-08", Leave: "Loss of pay", Unpaid: true}, // 1
	}
	for i := 9; i <= 13; i++ { // five late days, three allowed: two over = 1 day
		d := "2026-09-" + []string{"09", "10", "11", "12", "13"}[i-9]
		days = append(days, day(d, "09:30", "18:30"))
	}
	m := FinalMonth(p, days)
	if m.Counts["Late"] != 5 || m.LateOver != 2 || m.LOP != 4 || m.Outside != 1 || m.PaidDays != 8 {
		t.Fatalf("late %d over %d lop %v outside %d paid %v", m.Counts["Late"], m.LateOver, m.LOP, m.Outside, m.PaidDays)
	}
}

func TestPolicyRules(t *testing.T) {
	p := policy.Defaults()
	p.Sat = "2,4"
	for d, want := range map[string]bool{"2026-11-01": true, "2026-11-07": false, "2026-11-14": true, "2026-11-28": true, "2026-11-10": false} {
		if p.IsWO(d) != want {
			t.Errorf("%s weekly off = %v", d, !want)
		}
	}
	r := policy.Defaults()
	r.OffMode = "rotational"
	// EMP-1014: 1014 % 6 = 0, so Monday is the weekly off.
	if r.DefCode("EMP-1014", "G", "2026-11-02", nil) != "WO" || r.DefCode("EMP-1014", "G", "2026-11-01", nil) != "G" {
		t.Error("rotational off by employee number")
	}
	if p.DefCode("EMP-1", "M", "2026-11-08", policy.Holidays{"2026-11-08": "Diwali"}) != "WO" {
		t.Error("a Sunday holiday stays a weekly off")
	}
	if p.DefCode("EMP-1", "M", "2026-11-09", policy.Holidays{"2026-11-09": "X"}) != "PH" {
		t.Error("holiday")
	}
	if _, cyc, last := p.RegDeadline("2026-10-29"); cyc != "2026-11-03" || last != "2026-11-03" {
		t.Error("deadline is the earlier of 7 days and cycle end + 3", cyc, last)
	}
	if n, over := p.MonthOffs("2026-11", func(d string) string {
		if p.IsWO(d) {
			return "WO"
		}
		return ""
	}); n != 7 || over {
		t.Error("5 Sundays + 2 Saturdays in Nov 2026", n, over)
	}
	p.Notice.Grade["G5"] = map[string]int{"conf": 90}
	if p.NoticeDays("G5", false) != 90 || p.NoticeDays("G5", true) != 15 || p.NoticeDays("G1", false) != 60 {
		t.Error("notice by grade")
	}
}

// Payroll test RUN-15: a half-day leave counts half a day; a half-day loss of pay is 0.5 LOP, a half-day paid leave
// with the other half worked is no LOP, and with nothing punched the other half follows the no-punch rule.
// RUN-78: punches on a week off or holiday never make loss of pay.
func TestHalfDayLeaveAndDayOffPunches(t *testing.T) {
	p := policy.Defaults()
	g := Plan{Code: "G", Shift: shiftOf("G")}
	rec := func(d, in, out string) *Rec { return Day(nil, &Raw{In: in, Out: out}, d, 16) }
	days := []DayIn{
		{Date: "2026-10-15", Leave: "Loss of pay", Unpaid: true, Half: true, Rec: rec("2026-10-15", "09:00", "13:00"), AgeDays: 20}, // 0.5
		{Date: "2026-10-16", Leave: "Casual leave", Half: true, Rec: rec("2026-10-16", "14:00", "18:00"), AgeDays: 20},              // 0
		{Date: "2026-10-17", Leave: "Casual leave", Half: true, AgeDays: 20},                                                        // other half no punch: 0.5
		{Date: "2026-10-18", Plan: Plan{Code: "WO"}, Rec: rec("2026-10-18", "10:00", "13:00"), AgeDays: 20},                         // worked 3h on a week off: 0
		{Date: "2026-10-19", Plan: Plan{Code: "PH"}, Rec: rec("2026-10-19", "10:00", ""), AgeDays: 20},                              // missing punch on a holiday: 0
		{Date: "2026-10-20", Plan: g, Rec: rec("2026-10-20", "09:00", "18:00"), AgeDays: 20},
	}
	m := FinalMonth(p, days)
	got := []float64{}
	for _, d := range m.Days {
		got = append(got, d.LOP)
	}
	if m.LOP != 1 || got[0] != 0.5 || got[1] != 0 || got[2] != 0.5 || got[3] != 0 || got[4] != 0 {
		t.Fatalf("LOP %v by day %v (%+v)", m.LOP, got, m.Days)
	}
	if m.Days[3].Code != "W" || m.Days[4].Code != "PH" {
		t.Fatalf("day off codes %q %q", m.Days[3].Code, m.Days[4].Code)
	}
}

// Short leave (owner, 20 Nov 2026): an approved 2-hour short leave at the start of a 09:00–18:00 shift makes an 11:00
// arrival on time and the day Present (no late mark, no short hours); without it the same punches are Late. One at the
// end makes leaving at 16:00 no early exit.
func TestShortLeaveExcusesLate(t *testing.T) {
	p := policy.Defaults()
	p.RS.OutTol, p.RS.DevStart, p.RS.DevTol = 15, true, 30
	g := Plan{Code: "G", Shift: &Shift{Code: "G", Start: 9 * 60, End: 18 * 60}}
	rec := &Rec{In: "11:00", Out: "18:00"}
	if st, _ := Status(p, g, rec, nil, false); st != "Half day" {
		t.Fatalf("without short leave: %s", st)
	}
	if st, _ := Status(p, g, &Rec{In: "11:00", Out: "20:00"}, nil, false); st != "Late" {
		t.Fatalf("without short leave, 9 hours: %s", st)
	}
	s := g
	s.Short = []Window{{9 * 60, 11 * 60}}
	for _, r := range []*Rec{rec, {In: "11:00", Out: "20:00"}} {
		if st, _ := Status(p, s, r, nil, false); st != "Present" {
			t.Fatalf("with short leave 09:00-11:00, %s-%s: %s", r.In, r.Out, st)
		}
	}
	if v := Deviate(p, s, rec, "2026-11-18", "L1", []Shift{*s.Shift}); v != nil {
		t.Fatalf("deviation with short leave: %+v", v)
	}
	m := FinalMonth(p, []DayIn{{Date: "2026-11-18", Plan: s, Rec: rec, AgeDays: 2}})
	if d := m.Days[0]; d.Status != "Present" || d.LOP != 0 || d.Leave != "Short leave 2h" || m.Counts["Late"] != 0 {
		t.Fatalf("month day %+v counts %v", d, m.Counts)
	}
	e := g
	e.Short = []Window{{16 * 60, 18 * 60}}
	early := &Rec{In: "09:00", Out: "16:00"}
	if v := Deviate(p, g, early, "2026-11-18", "L1", []Shift{*g.Shift}); v == nil {
		t.Fatal("leaving at 16:00 without short leave is an early exit")
	}
	if v := Deviate(p, e, early, "2026-11-18", "L1", []Shift{*e.Shift}); v != nil {
		t.Fatalf("early exit with short leave: %+v", v)
	}
	if st, _ := Status(p, e, early, nil, false); st != "Present" {
		t.Fatalf("7 hours worked + 2 excused: %s", st)
	}
}
