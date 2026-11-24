package leave

import (
	"fmt"
	"strings"
	"testing"
)

// days adds n paid days of w each from the first of the month ym.
func days(wk *Worked, ym string, n int, w float64) {
	for i := 1; i <= n; i++ {
		wk.Days[fmt.Sprintf("%s-%02d", ym, i)] = w
	}
}

func wk(through string) *Worked { return &Worked{Days: map[string]float64{}, Through: through} }

// rule7: every 7 paid days, 1 day of Casual leave and 1 of Privilege leave (the numbers are the test's own).
func rule7() (LV, map[string]TypeCfg) {
	cl, pl := Default(KindBalance), Default(KindBalance)
	cl.Days, pl.Days = 0, 0 // no cap
	return LV{Year: "cal", Prorate: true, Earn: []EarnRule{{Days: 7, Types: []EarnType{{"Casual leave", 1}, {"Privilege leave", 1}}}}},
		map[string]TypeCfg{"Casual leave": cl, "Privilege leave": pl}
}

// Earn by working days (owner, 24 Nov 2026): whole blocks only, the rest counts toward the next block.
func TestEarnBlocks(t *testing.T) {
	lv, types := rule7()
	for _, c := range []struct {
		n            int
		credit, left float64
	}{{6, 0, 6}, {7, 1, 0}, {17, 2, 3}} {
		w := wk("2026-10-31")
		days(w, "2026-10", c.n, 1)
		for _, name := range []string{"Casual leave", "Privilege leave"} {
			b := ComputeWith(name, types[name], lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w)
			if b.Credit != c.credit || b.Worked == nil || b.Worked.Left != c.left || b.Worked.ToNext != 7-c.left || b.Bal != c.credit {
				t.Fatalf("%s, %d paid days: %+v %+v", name, c.n, b, b.Worked)
			}
		}
	}
	// Half days count 0.5: 12 whole and 4 halves are 14 days, two blocks; 6 whole and 1 half are 6.5, none.
	w := wk("2026-10-31")
	days(w, "2026-10", 16, 1)
	for i := 13; i <= 16; i++ {
		w.Days[fmt.Sprintf("2026-10-%02d", i)] = 0.5
	}
	if b := ComputeWith("Casual leave", types["Casual leave"], lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w); b.Credit != 2 || b.Worked.Days != 14 ||
		len(b.Worked.Credits) != 2 || b.Worked.Credits[0].On != "2026-10-08" || b.Worked.Credits[1].On != "2026-10-17" {
		t.Fatalf("half days: %+v", b.Worked)
	}
	w = wk("2026-10-31")
	days(w, "2026-10", 6, 1)
	w.Days["2026-10-20"] = 0.5
	if b := ComputeWith("Casual leave", types["Casual leave"], lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w); b.Credit != 0 || b.Worked.Left != 6.5 {
		t.Fatalf("6.5 days: %+v", b.Worked)
	}
	// Days earned in halves: 1.5 a block.
	lv.Earn[0].Types[0].Days = 1.5
	w = wk("2026-10-31")
	days(w, "2026-10", 14, 1)
	if b := ComputeWith("Casual leave", types["Casual leave"], lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w); b.Credit != 3 {
		t.Fatalf("1.5 a block: %+v", b.Worked)
	}
}

// paidFrom adds a paid day (1) on every date from..to.
func paidFrom(w *Worked, from, to string) {
	for _, d := range Dates(from, to) {
		w.Days[d] = 1
	}
}

// The owner's example (24 Nov 2026, evening): joined on 15 Jan, every day paid (week offs and holidays count), a rule
// of 30 paid days: the 30th paid day is 13 Feb, so the leave is credited from 14 Feb, not at the end of the month.
func TestEarnCreditedTheDayAfter(t *testing.T) {
	lv, types := rule7()
	lv.Earn[0].Days = 30
	cl := types["Casual leave"]
	w := &Worked{Days: map[string]float64{}}
	paidFrom(w, "2026-01-15", "2026-03-31")
	at := func(on string) Balance {
		w.Through = Through(on, "2026-11-24", "")
		return ComputeWith("Casual leave", cl, lv, 0, "2026-01-15", on, nil, nil, nil, w)
	}
	if b := at("2026-02-13"); b.Credit != 0 || b.Worked.Days != 29 || b.Worked.ToNext != 1 || b.Worked.Through != "2026-02-12" {
		t.Fatalf("13 Feb, 29 paid days: %+v", b.Worked)
	}
	if b := at("2026-02-14"); b.Credit != 1 || b.Worked.Days != 30 || len(b.Worked.Credits) != 1 || b.Worked.Credits[0] != (EarnedOn{On: "2026-02-14", Blocks: 1, Credit: 1}) {
		t.Fatalf("14 Feb, the day after the 30th paid day: %+v", b.Worked)
	}
	// The next block: 14 Feb to 15 Mar (15 paid days in February, 15 in March), credited from 16 Mar.
	if b := at("2026-03-15"); b.Credit != 1 || b.Worked.Left != 29 {
		t.Fatalf("15 Mar: %+v", b.Worked)
	}
	if b := at("2026-03-16"); b.Credit != 2 || b.Worked.Credits[1].On != "2026-03-16" || len(b.Worked.Months) != 3 || b.Worked.Months[0] != (EarnedMonth{"2026-01", 17}) {
		t.Fatalf("16 Mar: %+v", b.Worked)
	}
	// Today's paid day is not over yet: a balance today counts up to yesterday.
	w.Through = Through("2026-02-14", "2026-02-14", "")
	if b := ComputeWith("Casual leave", cl, lv, 0, "2026-01-15", "2026-02-14", nil, nil, nil, w); b.Credit != 1 {
		t.Fatalf("today 14 Feb: %+v", b.Worked)
	}
	w.Through = Through("2026-02-20", "2026-02-13", "")
	if b := ComputeWith("Casual leave", cl, lv, 0, "2026-01-15", "2026-02-20", nil, nil, nil, w); b.Credit != 0 || b.Worked.Through != "2026-02-12" {
		t.Fatalf("a leave date after today counts only days over by today: %+v", b.Worked)
	}
	// Loss of pay days are 0 (left out): two of them before 13 Feb push the credit two paid days later, to 16 Feb.
	w.Days["2026-01-20"], w.Days["2026-01-21"] = 0, 0
	if b := at("2026-02-15"); b.Credit != 0 {
		t.Fatalf("15 Feb with 2 days of loss of pay: %+v", b.Worked)
	}
	if b := at("2026-02-16"); b.Credit != 1 || b.Worked.Credits[0].On != "2026-02-16" {
		t.Fatalf("16 Feb with 2 days of loss of pay: %+v", b.Worked)
	}
}

// Nothing counts after the last working day: a leaver's balance on a later date stops at it; on the last working
// day itself (F&F) that day counts once it has passed.
func TestEarnStopsAtLastWorkingDay(t *testing.T) {
	lv, types := rule7()
	cl := types["Casual leave"]
	w := &Worked{Days: map[string]float64{}}
	paidFrom(w, "2026-03-01", "2026-03-31") // the engine gives 0 after the last working day; Through stops there too
	for _, c := range []struct {
		on   string
		want float64
	}{{"2026-03-10", 1}, {"2026-03-14", 2}, {"2026-03-31", 2}, {"2026-11-24", 2}} {
		w.Through = Through(c.on, "2026-11-24", "2026-03-14")
		if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", c.on, nil, nil, nil, w); b.Credit != c.want || (c.on >= "2026-03-14" && b.Worked.Days != 14) {
			t.Errorf("on %s, last working day 14 Mar: %+v", c.on, b.Worked)
		}
	}
	// Not active (Through ""): nothing.
	w.Through = ""
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-04-10", nil, nil, nil, w); b.Credit != 0 || b.Worked.Days != 0 || b.Worked.Through != "" {
		t.Fatalf("not active: %+v", b.Worked)
	}
}

// A block spans months (4 days in January and 3 in February complete one, credited the day after the 7th); the count
// starts again in a new leave year.
func TestEarnAcrossMonthsAndYears(t *testing.T) {
	lv, types := rule7()
	cl := types["Casual leave"]
	w := wk("2026-02-28")
	days(w, "2026-01", 4, 1)
	days(w, "2026-02", 3, 1)
	b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-03-05", nil, nil, nil, w)
	if b.Credit != 1 || len(b.Worked.Months) != 2 || b.Worked.Months[1] != (EarnedMonth{"2026-02", 3}) || len(b.Worked.Credits) != 1 || b.Worked.Credits[0].On != "2026-02-04" {
		t.Fatalf("block across months: %+v", b.Worked)
	}
	// The day before the credit: 6 days counted.
	w.Through = "2026-02-02"
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-02-03", nil, nil, nil, w); b.Credit != 0 || b.Worked.Days != 6 {
		t.Fatalf("one day short: %+v", b.Worked)
	}
	// 6 days in December 2025 and 1 in January 2026: no block in either year.
	w = wk("2026-01-31")
	days(w, "2025-12", 6, 1)
	days(w, "2026-01", 1, 1)
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-02-10", nil, nil, nil, w); b.Credit != 0 || b.Worked.Days != 1 {
		t.Fatalf("new leave year restarts the count: %+v", b.Worked)
	}
	// 10 in December: one block in 2025 (3 carried toward the next block are dropped at the year end), 1 in 2026.
	w = wk("2026-01-31")
	days(w, "2025-12", 10, 1)
	days(w, "2026-01", 1, 1)
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2025-12-31", nil, nil, nil, w); b.Credit != 1 || b.Worked.Left != 3 {
		t.Fatalf("December 2025: %+v", b.Worked)
	}
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-02-10", nil, nil, nil, w); b.Credit != 0 || b.Worked.Days != 1 || b.Worked.ToNext != 6 {
		t.Fatalf("January 2026 starts from 0: %+v", b.Worked)
	}
}

// A type in a rule is earned only by paid days: no upfront or monthly credit; Days a year caps it when set.
func TestEarnOnlyAndCap(t *testing.T) {
	lv, types := rule7()
	cl := types["Casual leave"]
	cl.Days = 8
	for _, credit := range []string{CreditUpfront, CreditMonthly} {
		cl.Credit = credit
		if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, wk("2026-10-31")); b.Credit != 0 || b.Bal != 0 {
			t.Fatalf("%s type in a rule, no days worked: %+v", credit, b)
		}
	}
	w := wk("2026-10-31")
	for m := 1; m <= 10; m++ {
		days(w, fmt.Sprintf("2026-%02d", m), 26, 1)
	}
	// 260 days = 37 blocks: capped at 8; without a cap 37.
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w); b.Credit != 8 || b.Worked.Earned != 37 || b.Worked.Cap != 8 || len(b.Worked.Credits) != 8 {
		t.Fatalf("cap 8: %+v", b.Worked)
	}
	cl.Days = 0
	if b := ComputeWith("Casual leave", cl, lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w); b.Credit != 37 {
		t.Fatalf("no cap: %+v", b.Worked)
	}
	// Sick leave, in no rule, stays upfront.
	sl := Default(KindBalance)
	sl.Days, sl.Credit = 6, CreditUpfront
	if b := ComputeWith("Sick leave", sl, lv, 0, "2020-01-01", "2026-11-10", nil, nil, nil, w); b.Credit != 6 || b.Worked != nil {
		t.Fatalf("type in no rule: %+v", b)
	}
}

func TestEarnRulesChecked(t *testing.T) {
	_, types := rule7()
	types["Maternity leave"] = Default(KindEvent)
	kind := func(n string) string {
		if n == "Maternity leave" {
			return KindEvent
		}
		return KindBalance
	}
	ok := LV{Earn: []EarnRule{{Days: 7, Types: []EarnType{{"Casual leave", 1}}}, {Days: 10.5, Types: []EarnType{{"Privilege leave", 1.5}}}}}
	if why := EarnProblem(ok, types, kind); why != "" {
		t.Fatal(why)
	}
	for want, lv := range map[string]LV{
		"already in the rule for every 7 paid days": {Earn: []EarnRule{{Days: 7, Types: []EarnType{{"Casual leave", 1}}}, {Days: 10, Types: []EarnType{{"Casual leave", 1}}}}},
		"enter the paid days":                       {Earn: []EarnRule{{Days: 0, Types: []EarnType{{"Casual leave", 1}}}}},
		"add at least one leave type":               {Earn: []EarnRule{{Days: 7}}},
		"enter the days earned":                     {Earn: []EarnRule{{Days: 7, Types: []EarnType{{"Casual leave", 0.3}}}}},
		"not a leave type with a balance":           {Earn: []EarnRule{{Days: 7, Types: []EarnType{{"Maternity leave", 1}}}}},
		"choose the leave type":                     {Earn: []EarnRule{{Days: 7, Types: []EarnType{{"", 1}}}}},
	} {
		if why := EarnProblem(lv, types, kind); !strings.Contains(why, want) {
			t.Errorf("want %q, got %q", want, why)
		}
	}
}

// A plan saved with the per-type shape of the morning of 24 Nov (Credit "worked", PerDays) reads as a rule of its own.
func TestEarnOldShape(t *testing.T) {
	cl := Default(KindBalance)
	cl.Days, cl.Credit, cl.PerDays = 12, "worked", 20
	lv := LV{Year: "cal"}
	if b, in := lv.BlockFor("Casual leave", cl); !in || b.Every != 20 || b.Per != 1 {
		t.Fatalf("read before Normalize: %+v %v", b, in)
	}
	types := map[string]TypeCfg{"Casual leave": cl}
	Normalize(&lv, types)
	if len(lv.Earn) != 1 || lv.Earn[0].Days != 20 || lv.Earn[0].Types[0] != (EarnType{"Casual leave", 1}) || types["Casual leave"].Credit != CreditMonthly || types["Casual leave"].PerDays != 0 {
		t.Fatalf("normalized: %+v %+v", lv.Earn, types)
	}
	Normalize(&lv, types) // again: nothing more
	if len(lv.Earn) != 1 {
		t.Fatalf("twice: %+v", lv.Earn)
	}
}

func TestWorkedThrough(t *testing.T) {
	for _, c := range []struct{ on, today, lwd, want string }{
		{"2026-11-24", "2026-11-24", "", "2026-11-23"},           // today: up to yesterday
		{"2026-12-01", "2026-12-01", "", "2026-11-30"},           // no wait for the month end
		{"2026-12-15", "2026-11-24", "", "2026-11-23"},           // a future date: only days over by today
		{"2026-06-15", "2026-11-24", "", "2026-06-14"},           // a past date: the day before it
		{"2026-11-18", "2026-11-24", "2026-11-18", "2026-11-18"}, // a leaver on the last working day (F&F): that day, once passed
		{"2026-11-24", "2026-11-24", "2026-11-18", "2026-11-18"}, // nothing after the last working day
		{"2026-11-24", "2026-11-24", "2026-11-24", "2026-11-23"}, // not before the day is over
		{"2026-11-24", "2026-11-24", "2026-12-10", "2026-11-23"}, // on notice
		{"2026-12-20", "2026-11-24", "2026-12-10", "2026-11-23"}, // on notice, a later date
	} {
		if got := Through(c.on, c.today, c.lwd); got != c.want {
			t.Errorf("Through(%s, %s, %q) = %s, want %s", c.on, c.today, c.lwd, got, c.want)
		}
	}
}

func TestEarnAfterProbation(t *testing.T) {
	lv, types := rule7()
	pl := types["Privilege leave"]
	pl.From = "prob"
	w := wk("2026-08-31")
	for m := 1; m <= 8; m++ {
		days(w, fmt.Sprintf("2026-%02d", m), 21, 1)
	}
	// Joined 1 Jan, 3 months probation ends 1 Apr: April to August, 105 days = 15 blocks.
	if b := ComputeWith("Privilege leave", pl, lv, 3, "2026-01-01", "2026-09-10", nil, nil, nil, w); b.Credit != 15 || b.Worked.Days != 105 {
		t.Fatalf("after probation: %+v", b.Worked)
	}
	if b := ComputeWith("Privilege leave", pl, lv, 3, "2026-01-01", "2026-03-31", nil, nil, nil, w); b.Credit != 0 {
		t.Fatalf("in probation: %+v", b)
	}
	// Back-dated at confirmation: January to August once probation is over, 168 days = 24 blocks.
	pl.Catchup = true
	if b := ComputeWith("Privilege leave", pl, lv, 3, "2026-01-01", "2026-09-10", nil, nil, nil, w); b.Credit != 24 || b.Worked.Credits[0] != (EarnedOn{On: "2026-04-01", Blocks: 9, Credit: 9}) {
		t.Fatalf("catch-up: %+v", b.Worked)
	}
}

func TestEarnCarryAndOpening(t *testing.T) {
	lv, types := rule7()
	pl := types["Privilege leave"]
	pl.CF = 30
	w := wk("2026-02-28")
	days(w, "2025-11", 21, 1) // 3 blocks in 2025
	days(w, "2026-01", 14, 1) // 2 blocks in 2026
	used := []Taken{{From: "2025-12-10", Days: 1}}
	b := ComputeWith("Privilege leave", pl, lv, 0, "2024-06-01", "2026-02-10", nil, used, nil, w)
	if b.Carry != 2 || b.Credit != 4 || b.Bal != 4 {
		t.Fatalf("carry: %+v", b)
	}
	// An opening of 5 as at 15 Jan 2026: only the 16th onward count (none here), so 5.
	if b := ComputeWith("Privilege leave", pl, lv, 0, "2024-06-01", "2026-02-10", &Opening{At: "2026-01-15", Balance: 5}, used, nil, w); b.Credit != 5 || b.Carry != 0 {
		t.Fatalf("opening: %+v", b)
	}
}

// Monthly and upfront types give the same balances whatever attendance and rules for other types are passed.
func TestWorkedLeavesOtherCreditsAlone(t *testing.T) {
	w := &Worked{Days: map[string]float64{"2026-03-02": 1, "2026-04-01": 1}, Through: "2026-04-30"}
	lv := LV{Year: "cal", Prorate: true}
	rules := lv
	rules.Earn = []EarnRule{{Days: 1, Types: []EarnType{{"Privilege leave", 1}}}}
	for _, credit := range []string{CreditMonthly, CreditUpfront, ""} {
		c := Default(KindBalance)
		c.Days, c.Credit, c.CF, c.From, c.Catchup = 12, credit, 5, "prob", true
		for _, on := range []string{"2026-05-10", "2027-02-01"} {
			a := Compute(c, lv, 6, "2025-03-10", on, nil, []Taken{{"2026-02-02", 1}}, nil)
			b := ComputeWith("Casual leave", c, rules, 6, "2025-03-10", on, nil, []Taken{{"2026-02-02", 1}}, nil, w)
			if a != b || b.Worked != nil {
				t.Fatalf("%q on %s: %+v vs %+v", credit, on, a, b)
			}
		}
	}
}
