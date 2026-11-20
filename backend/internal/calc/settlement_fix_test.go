package calc

import "testing"

// Payroll test fixes: gratuity service in calendar years, months and days with the last working day counted
// (EXT-12, EXT-13), notice pay rounded once (EXT-17) and the full and final pay month inside 45 days (EXT-38).

func TestGratuityCalendarService(t *testing.T) {
	for _, c := range []struct {
		doj, lwd string
		years    int
		ok       bool
	}{
		{"2022-02-01", "2026-10-31", 5, true},  // exactly 4 years 9 months with the LWD: eligible (EXT-12)
		{"2022-02-01", "2026-10-30", 0, false}, // one day short
		{"2021-05-01", "2026-10-31", 5, true},  // exactly 5 years 6 months: not rounded up (EXT-13)
		{"2021-05-01", "2026-11-01", 6, true},  // 5 years 6 months 1 day: rounded up
		{"2016-01-10", "2026-11-13", 11, true}, // 10 years 10 months
		{"2024-08-01", "2026-10-10", 0, false}, // 2 years
	} {
		y, _, ok := Gratuity(20000, c.doj, c.lwd, false, true)
		if ok != c.ok || y != c.years {
			t.Errorf("%s..%s: %d %v, want %d %v", c.doj, c.lwd, y, ok, c.years, c.ok)
		}
	}
	if y, a, ok := Gratuity(20000, "2024-08-01", "2026-10-10", true, true); !ok || y != 2 || a != 23077 {
		t.Fatalf("death: any service: %d %v %v", y, a, ok)
	}
	if y, ok := ServiceYMD("2022-02-01", "2026-10-31"); y != [3]int{4, 9, 0} || !ok {
		t.Fatalf("service %v", y)
	}
}

func TestNoticePayRoundsOnce(t *testing.T) {
	if v := NoticePay(25000, 60); v != 50000 {
		t.Fatalf("notice pay %v", v)
	}
	if v := NoticePay(27000, 60); v != 54000 {
		t.Fatalf("notice pay %v", v)
	}
}

func TestFnfPayMonthWithin45Days(t *testing.T) {
	for _, c := range []struct{ lwd, open, payDay, want string }{
		{"2026-11-20", "2026-11", "Last working day", "2026-12"},  // due 4 Jan: December pays 31 Dec (January's 29 Jan is late)
		{"2026-11-13", "2026-11", "Last working day", "2026-11"},  // due 28 Dec: December pays 31 Dec, too late
		{"2026-11-16", "2026-11", "Last working day", "2026-12"},  // due 31 Dec
		{"2026-11-20", "2026-11", "5th of next month", "2026-11"}, // due 4 Jan: December pays 5 Jan, too late
		{"2026-11-23", "2026-10", "1st of next month", "2026-12"}, // due 7 Jan: December pays 1 Jan
		{"2026-08-10", "2026-11", "Last working day", "2026-11"},  // already late: never before the open month
	} {
		if got := FnfPayMonth(c.lwd, c.open, c.payDay); got != c.want {
			t.Errorf("%s open %s %s: %s, want %s", c.lwd, c.open, c.payDay, got, c.want)
		}
	}
	if PayDate("2026-10", "Last working day") != "2026-10-30" || PayDate("2026-10", "5th of next month") != "2026-11-05" {
		t.Fatal("pay date")
	}
}
