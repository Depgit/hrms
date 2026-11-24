package api

// Earn by working days (owner, 24 Nov 2026; evening: "Working days means Paid days for leaves", credited as soon as
// the days are complete, "accrued only on active employee"): a leave plan's rules (leave.LV.Earn) say "every N paid
// days, these leave types earn these days"; a type in a rule is earned only that way. A day's paid days come from the
// attendance engine's settled day (the same month view as My attendance and payroll): 1 less its loss of pay
// (attend.DayOut.Paid), so present, late, called to work, paid leave, week offs and holidays 1, a half day 0.5, absent
// and loss of pay 0. They are counted day by day through the leave year, up to the day before the balance date and
// never today or later (a leaver up to the last working day: leave.Through); a block is credited from the day after
// its Nth paid day. Nothing is stored, so a later regularization, or a no-punch day turning to loss of pay once the
// regularization window is over, changes the credit.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// leaveBalOf is a balance type's balance on a date for the employee: the plan's rules, the imported opening, the
// requests (approved, pending), the leave days called to work and worked (returned) and, for a type in an Earn by
// working days rule, the paid days.
func leaveBalOf(ctx context.Context, q store.Q, ts *timeSet, e *emp, p *workPolicy, cfg *leave.TypeCfg, name, on string, approved, pending []leave.Taken) (leave.Balance, error) {
	var lv leave.LV
	if pl := ts.plan(p); pl != nil {
		lv = pl.LV
	}
	op := openingOf(ctx, q, e.ID, name, on)
	var wk *leave.Worked
	if _, in := lv.BlockFor(name, *cfg); in {
		var err error
		if wk, err = workedFor(ctx, q, ts, e, p, *cfg, lv, on, op); err != nil {
			return leave.Balance{}, err
		}
	}
	// Leave days called to work and worked come back (work_call.go): taken off what the approved requests used.
	ret, err := calledReturns(ctx, q, ts, e, p, name)
	if err != nil {
		return leave.Balance{}, err
	}
	if len(ret) > 0 {
		approved = append(append([]leave.Taken{}, approved...), ret...)
	}
	return leave.ComputeWith(name, *cfg, lv, p.S.ProbMonths(e.Grade), e.DOJ, on, op, approved, pending, wk), nil
}

// workedFor is the employee's paid days on each date that an earned-by-working-days balance on the date on can reach:
// the leave year of on, or, when unused days carry forward, back to the year of the opening balance (or joining), each
// calendar month settled by the attendance engine as My attendance shows it. Only an active employee earns: someone
// leaving or left (status Inactive, or on notice) up to the last working day; an inactive record without one (an offer
// declined before joining) and a joiner not active yet (Pre-boarding) earn nothing.
func workedFor(ctx context.Context, q store.Q, ts *timeSet, e *emp, p *workPolicy, cfg leave.TypeCfg, lv leave.LV, on string, op *leave.Opening) (*leave.Worked, error) {
	lwd := ""
	if e.LWD != nil {
		lwd = *e.LWD
	}
	wk := &leave.Worked{Days: map[string]float64{}}
	if e.Status != "Active" && (e.Status != "Inactive" || lwd == "") {
		return wk, nil
	}
	wk.Through = leave.Through(on, today(), lwd)
	ys, _ := leave.Year(lv.Year, e.DOJ, on)
	from := ys.Format("2006-01-02")
	if cfg.CF > 0 { // carry forward reaches back year by year, to the opening's leave year or to joining
		from = e.DOJ
		if op != nil {
			s, _ := leave.Year(lv.Year, e.DOJ, op.At)
			from = s.Format("2006-01-02")
		}
	}
	from = max(from, e.DOJ)
	// Months before the first punch or approved leave on record (from before the HRMS) are left out: the count starts
	// on the first of the month that has it, the first month payroll could have seen.
	first := ""
	if r, err := q.C("attendance_day").FindOne(ctx, bson.D{{Key: "employee_id", Value: e.ID}},
		options.FindOne().SetSort(bson.D{{Key: "date", Value: 1}}).SetProjection(bson.D{{Key: "date", Value: 1}})); err != nil {
		return nil, err
	} else if r != nil {
		first = str(r["date"])
	}
	if r, err := q.C("approval_request").FindOne(ctx, leaveRoutesOf(e.ID, "", "Approved"),
		options.FindOne().SetSort(bson.D{{Key: "payload.from", Value: 1}}).SetProjection(bson.D{{Key: "payload.from", Value: 1}})); err != nil {
		return nil, err
	} else if r != nil {
		if pl, _ := r["payload"].(map[string]any); pl != nil && (first == "" || str(pl["from"]) < first) {
			first = str(pl["from"])
		}
	}
	to := wk.Through
	if len(first) < 7 || len(e.DOJ) != 10 {
		return wk, nil
	}
	from = max(from, first[:7]+"-01")
	if from > to {
		return wk, nil
	}
	m, err := loadMonth(ctx, q, ts, []string{e.ID}, from[:7]+"-01", policy.LastDayOf(to[:7]))
	if err != nil {
		return nil, err
	}
	for ym := from[:7]; ym <= to[:7]; ym = policy.D(ym+"-01").AddDate(0, 1, 0).Format("2006-01") {
		for _, d := range m.month(e, p, ym).Days {
			if d.Date < from || d.Date > to {
				continue
			}
			if v := d.Paid(); v > 0 {
				wk.Days[d.Date] = v
			}
		}
	}
	return wk, nil
}
