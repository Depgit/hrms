package calc

import "time"

// SplitGross is the gross for month ym (YYYY-MM) when a revision applies from date (YYYY-MM-DD) inside it: the
// earlier gross for the days before the date and the new gross from it, by calendar days of the month (the same
// day split as a structure change, spec 13; user decision 13 Nov 2026, payroll test ITM-28). A date on the 1st, a
// date outside the month, or no earlier gross (a joining month) gives the new gross for the whole month.
func SplitGross(ym string, before, after float64, date string) float64 {
	if before <= 0 || len(date) != 10 || date[:7] != ym {
		return after
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil || t.Day() == 1 {
		return after
	}
	dim := float64(t.AddDate(0, 1, -t.Day()).Day())
	pre := float64(t.Day() - 1)
	return Round2((before*pre + after*(dim-pre)) / dim)
}
