package attend

import (
	"testing"

	"github.com/saltstayz/hrms/internal/policy"
)

// Paid days for leave earned by working days (owner, 24 Nov 2026, evening: "Working days means Paid days for
// leaves"): 1 less the day's loss of pay. Present, late, paid leave, week offs and holidays 1 (worked or not); a half
// day 0.5; a no-punch day 1 while inside the regularization window and 0 once it is loss of pay; loss of pay leave 0;
// days outside employment and upcoming days 0. Over the days that are over it is the month's paid days.
func TestPaidDays(t *testing.T) {
	p := policy.Defaults()
	p.Att.Absent, p.Att.Missing, p.RR.Back = "lop", "half", 7
	gs := &Shift{Code: "G", Start: 9 * 60, End: 18 * 60}
	work := func(d, in, out string) DayIn {
		return DayIn{Date: d, Plan: Plan{Code: "G", Shift: gs}, Rec: &Rec{In: in, Out: out}, AgeDays: 20}
	}
	days := []DayIn{
		work("2026-11-02", "09:00", "18:00"),                                                            // present
		work("2026-11-03", "09:40", "18:00"),                                                            // late
		work("2026-11-04", "09:00", "14:00"),                                                            // half day (5 h)
		{Date: "2026-11-05", Plan: Plan{Code: "G", Shift: gs}, AgeDays: 3},                              // no punch, inside the window
		{Date: "2026-11-06", Plan: Plan{Code: "G", Shift: gs}, AgeDays: 30},                             // no punch, now loss of pay
		work("2026-11-07", "09:00", ""),                                                                 // missing punch: half day of loss of pay
		{Date: "2026-11-08", Plan: Plan{Code: "WO"}, AgeDays: 20},                                       // week off
		{Date: "2026-11-09", Plan: Plan{Code: "WO"}, Rec: &Rec{In: "09:00", Out: "18:00"}, AgeDays: 20}, // worked on a week off
		{Date: "2026-11-10", Plan: Plan{Code: "PH"}, AgeDays: 20},                                       // holiday
		{Date: "2026-11-11", Leave: "Casual leave"},                                                     // paid leave
		{Date: "2026-11-12", Leave: "Loss of pay", Unpaid: true},                                        // loss of pay leave
		{Date: "2026-11-13", Leave: "Casual leave", Half: true, HalfOf: "first", Plan: Plan{Code: "G", Shift: gs}, Rec: &Rec{In: "14:00", Out: "18:00"}},              // half CL + worked
		{Date: "2026-11-14", Leave: "Loss of pay", Unpaid: true, Half: true, HalfOf: "first", Plan: Plan{Code: "G", Shift: gs}, Rec: &Rec{In: "14:00", Out: "18:00"}}, // half LOP + worked
		{Date: "2026-11-16", Leave: "Casual leave", Half: true, HalfOf: "first", Plan: Plan{Code: "G", Shift: gs}, AgeDays: 30},                                       // half CL, other half no punch
		{Date: "2026-11-17", Before: true},
		{Date: "2026-11-18", After: true},
		{Date: "2026-11-19", Plan: Plan{Code: "G", Shift: gs}, Future: true},
	}
	want := []float64{1, 1, 0.5, 1, 0, 0.5, 1, 1, 1, 1, 0, 1, 0.5, 0.5, 0, 0, 0}
	m := FinalMonth(p, days)
	sum := 0.0
	for i, d := range m.Days {
		if got := d.Paid(); got != want[i] {
			t.Errorf("%s %q (%s, LOP %v): paid %v, want %v", d.Date, d.Status, d.Code, d.LOP, got, want[i])
		}
		sum += d.Paid()
	}
	// The month's paid days count the upcoming day (no loss of pay yet); leave counts it only once it is over.
	if sum != 10 || sum != m.PaidDays-1 {
		t.Errorf("month total %v, want 10 (month paid days %v less the upcoming day)", sum, m.PaidDays)
	}
}
