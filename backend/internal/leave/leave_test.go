package leave

import (
	"reflect"
	"strings"
	"testing"
)

func TestBalanceMonthly(t *testing.T) {
	cl := Default(KindBalance)
	cl.Days = 12 // 1 a month
	lv := LV{Year: "cal", Prorate: true}
	// Joined in 2025: January to March 2026 is 3 months of credit.
	b := Compute(cl, lv, 6, "2025-03-10", "2026-03-15", nil, []Taken{{"2026-02-02", 1}}, []Taken{{"2026-03-20", 0.5}, {"2025-12-01", 2}})
	if b.Credit != 3 || b.Used != 1 || b.Pending != 0.5 || b.Bal != 1.5 {
		t.Fatalf("%+v", b)
	}
	// Joiner in the year: credit from the joining month.
	if b := Compute(cl, lv, 6, "2026-02-20", "2026-04-01", nil, nil, nil); b.Credit != 3 {
		t.Fatalf("joined Feb, by Apr: %+v", b)
	}
}

func TestBalanceAfterProbation(t *testing.T) {
	pl := Default(KindBalance)
	pl.Days, pl.From = 12, "prob"
	lv := LV{Year: "cal", Prorate: true}
	// Joined 2026-01-15, 6 months probation ends 2026-07-15: July is the first month that ends after it.
	if b := Compute(pl, lv, 6, "2026-01-15", "2026-06-30", nil, nil, nil); b.Credit != 0 {
		t.Fatalf("nothing during probation: %+v", b)
	}
	if b := Compute(pl, lv, 6, "2026-01-15", "2026-09-01", nil, nil, nil); b.Credit != 3 {
		t.Fatalf("July, August, September: %+v", b)
	}
	pl.Catchup = true // back-credit January to July at confirmation
	if b := Compute(pl, lv, 6, "2026-01-15", "2026-09-01", nil, nil, nil); b.Credit != 9 {
		t.Fatalf("catch-up adds 6 months: %+v", b)
	}
	pl.InProb = false
	if b := Compute(pl, lv, 6, "2026-01-15", "2026-03-01", nil, nil, nil); !b.InProbBlock {
		t.Fatal("not usable in probation")
	}
}

func TestBalanceUpfrontAndOpening(t *testing.T) {
	sl := Default(KindBalance)
	sl.Days, sl.Credit = 6, "upfront"
	lv := LV{Year: "cal", Prorate: true}
	if b := Compute(sl, lv, 6, "2020-01-01", "2026-05-01", nil, nil, nil); b.Credit != 6 {
		t.Fatalf("full year upfront: %+v", b)
	}
	if b := Compute(sl, lv, 6, "2026-07-10", "2026-08-01", nil, nil, nil); b.Credit != 3 {
		t.Fatalf("joined in July: 6 of 12 months: %+v", b)
	}
	cl := Default(KindBalance)
	cl.Days = 12
	// Opening 4.5 as at 31 Mar, then April and May credit; leave before the opening is not counted again.
	b := Compute(cl, lv, 6, "2020-01-01", "2026-05-10", &Opening{"2026-03-31", 4.5}, []Taken{{"2026-02-01", 2}, {"2026-04-10", 1}}, nil)
	if b.Credit != 6.5 || b.Used != 1 || b.Bal != 5.5 {
		t.Fatalf("%+v", b)
	}
}

func TestYear(t *testing.T) {
	s, e := Year("fin", "2020-01-01", "2026-02-10")
	if s.Format("2006-01-02") != "2025-04-01" || e.Format("2006-01-02") != "2026-03-31" {
		t.Fatal(s, e)
	}
	s, _ = Year("anniv", "2021-08-20", "2026-05-01")
	if s.Format("2006-01-02") != "2025-08-20" {
		t.Fatal(s)
	}
}

func TestCover(t *testing.T) {
	d := []string{"Fri", "Sat", "Sun", "Mon"}
	off := []bool{false, true, true, false}
	if got := Cover(d, off, "exclude"); !reflect.DeepEqual(got, []string{"Fri", "Mon"}) {
		t.Error(got)
	}
	if got := Cover(d, off, "sandwich"); len(got) != 4 {
		t.Error("offs between two leave days count", got)
	}
	if got := Cover([]string{"Sat", "Sun", "Mon"}, []bool{true, true, false}, "sandwich"); !reflect.DeepEqual(got, []string{"Mon"}) {
		t.Error("offs at the edge do not count", got)
	}
}

func TestWindow(t *testing.T) {
	cl := Default(KindBalance)
	cl.Back, cl.MaxReq = 3, 3
	if er := WindowErrors(cl, "Casual leave", "2026-11-10", "2026-11-06", 1); len(er) != 1 {
		t.Error("4 days back with a 3 day window", er)
	}
	if er := WindowErrors(cl, "Casual leave", "2026-11-10", "2026-11-12", 4); len(er) != 1 {
		t.Error("4 days in one request, limit 3", er)
	}
	pl := Default(KindBalance)
	pl.Adv, pl.AdvOnly = 15, true
	if er := WindowErrors(pl, "Privilege leave", "2026-11-10", "2026-11-20", 2); len(er) != 1 {
		t.Error("15 days notice", er)
	}
	if er := WindowErrors(pl, "Privilege leave", "2026-11-10", "2026-11-09", 1); len(er) != 1 {
		t.Error("no back-dating", er)
	}
}

// Short leave (owner, 20 Nov 2026): at most the plan's hours (default 2) and the plan's number a month (default 2).
func TestShortLeaveRules(t *testing.T) {
	lv := LV{}
	if er := ShortErrors(lv, 9*60, 11*60, 1); len(er) != 0 {
		t.Fatalf("2 hours, second this month: %v", er)
	}
	if er := ShortErrors(lv, 9*60, 11*60+15, 0); len(er) != 1 || !strings.Contains(er[0], "at most 2 hours") {
		t.Fatalf("2h15 allowed: %v", er)
	}
	if er := ShortErrors(lv, 16*60, 17*60, 2); len(er) != 1 || !strings.Contains(er[0], "2 times a month") {
		t.Fatalf("third this month allowed: %v", er)
	}
	if er := ShortErrors(LV{ShortN: 3, ShortH: 3}, 9*60, 12*60, 2); len(er) != 0 {
		t.Fatalf("plan of 3 a month, 3 hours: %v", er)
	}
	if er := ShortErrors(LV{ShortOff: true}, 9*60, 10*60, 0); len(er) != 1 {
		t.Fatalf("switched off: %v", er)
	}
}
