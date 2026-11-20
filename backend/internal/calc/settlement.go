package calc

import (
	"math"
	"time"
)

// Full and final helpers (spec 18.2) and leave credit (spec 12).

const day = 24 * time.Hour

func date(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }

// ServiceYMD is the service from doj to lwd with both days counted, in completed calendar years, months and days
// (1 Feb 2022 to 31 Oct 2026 is 4 years 9 months 0 days). ok is false when the dates are missing or out of order.
func ServiceYMD(doj, lwd string) ([3]int, bool) {
	a, b := date(doj), date(lwd)
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return [3]int{}, false
	}
	b = b.AddDate(0, 0, 1) // the last working day is a day of service
	months := (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
	if a.AddDate(0, months, 0).After(b) {
		months--
	}
	days := int(b.Sub(a.AddDate(0, months, 0)).Hours() / 24)
	return [3]int{months / 12, months % 12, days}, true
}

// Gratuity: service of 4 years 9 months or more (any service on death or disability), measured in calendar
// years, months and days with the last working day counted; a fraction over 6 months rounds up (6 months
// exactly does not); basic x 15 / 26 x years; capped at Rs 20 lakh. ok is false when no gratuity is payable.
func Gratuity(basic float64, doj, lwd string, deathOrDisability, permanent bool) (years int, amount float64, ok bool) {
	if !permanent {
		return 0, 0, false
	}
	svc, valid := ServiceYMD(doj, lwd)
	if !valid {
		return 0, 0, false
	}
	months := svc[0]*12 + svc[1]
	if !(months >= 57 || (deathOrDisability && (months > 0 || svc[2] > 0))) {
		return 0, 0, false
	}
	y := svc[0]
	if svc[1] > 6 || (svc[1] == 6 && svc[2] > 0) {
		y++
	}
	if y < 1 {
		y = 1
	}
	return y, math.Min(2000000, math.Round(basic*15/26*float64(y))), true
}

// LeaveEncashment on exit: basic / 26 x balance, the balance floored to half a day.
func LeaveEncashment(basic, plBalance float64) (days, amount float64) {
	days = math.Max(0, math.Floor(plBalance*2)/2)
	return days, math.Round(basic / 26 * days)
}

// NoticePay in lieu (termination) or recovery for notice not served (absconding): gross / 30 x days, rounded once.
func NoticePay(gross float64, days int) float64 { return math.Round(gross / 30 * float64(days)) }

// PayDate is the date salary for month ym is paid under the pay schedule's pay day: "1st of next month",
// "5th of next month" or (anything else) the last working day, the last day of the month that is not a
// Saturday or Sunday.
func PayDate(ym, payDay string) string {
	first := date(ym + "-01")
	switch payDay {
	case "1st of next month":
		return first.AddDate(0, 1, 0).Format("2006-01-02")
	case "5th of next month":
		return first.AddDate(0, 1, 4).Format("2006-01-02")
	}
	d := first.AddDate(0, 1, -1)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, -1)
	}
	return d.Format("2006-01-02")
}

// FnfPayMonth is the latest payroll month that pays within 45 days of the last working day (spec 18.2), by
// the pay schedule's pay day; never before the open payroll month (when that is already late, the open month).
func FnfPayMonth(lwd, openYm, payDay string) string {
	due := date(lwd).Add(45 * day).Format("2006-01-02")
	ym := lwd[:7]
	for next := date(ym+"-01").AddDate(0, 1, 0).Format("2006-01"); PayDate(next, payDay) <= due; next = date(next+"-01").AddDate(0, 1, 0).Format("2006-01") {
		ym = next
	}
	if ym < openYm {
		return openYm
	}
	return ym
}

type LeaveCredit struct {
	Days    float64
	Credit  string // "monthly" | "upfront"
	From    string // "doj" | "prob"
	Catchup bool
}

func monthsBetween(a, b time.Time) int {
	return (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
}

// Credited leave up to onDate in the leave year starting yearStart (spec 12).
func Credited(c LeaveCredit, doj string, probMonths int, yearStart, onDate string, prorate bool) float64 {
	dj, ys, at := date(doj), date(yearStart), date(onDate)
	pe := dj.AddDate(0, probMonths, 0)
	start := ys
	if dj.After(ys) {
		start = time.Date(dj.Year(), dj.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	if c.Credit == "upfront" {
		if !prorate || !dj.After(ys) {
			return c.Days
		}
		ye := ys.AddDate(1, 0, -1)
		left := math.Min(float64(monthsBetween(start, ye)+1), 12)
		return Round2(c.Days * left / 12)
	}
	months := monthsBetween(start, at) + 1
	credit := 0.0
	for i := 0; i < months; i++ {
		end := time.Date(start.Year(), start.Month()+time.Month(i)+1, 0, 0, 0, 0, 0, time.UTC)
		if c.From == "doj" || !end.Before(pe) {
			credit += c.Days / 12
		}
	}
	if c.From == "prob" && c.Catchup && pe.After(start) && !pe.After(at) {
		credit += c.Days / 12 * float64(monthsBetween(start, pe))
	}
	return Round2(credit)
}
