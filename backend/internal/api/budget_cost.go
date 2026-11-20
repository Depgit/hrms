package api

// Budgets count the total cost to the company (owner, 18 Nov 2026): a property's monthly cost budget is compared with
// salary (earnings less the bonus and the meal and accommodation minimum-wage top-ups) plus the employer's
// contributions (PF with EPFO admin and EDLI, ESI, LWF, gratuity, bonus, medical insurance), the figures the Cost and
// budget page counts (cost.go costLineOf), never with the gross alone. The Budgets bulk column is "Monthly Cost Budget
// (CTC)"; a file with the old name "Monthly Payroll Budget" is still read (bulk.go). Compensation › Budgets shows the
// same total, and the offer budget check (onboarding.go budgetOver) adds the new hire's projected total to it.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	budCostCol    = "Monthly Cost Budget (CTC)"
	budCostColOld = "Monthly Payroll Budget"
)

// costTotalOf is a pay row's total cost to the company, as the Cost and budget page counts it (accommodation apart).
func costTotalOf(o *PayRowOut) float64 {
	l := costLineOf(o)
	return l.Salary + l.contrib()
}

// locMonthlyCost is the projected total monthly cost per property of the active employees matching filter (nil:
// everyone), for the open payroll month: each person's full month from their salary structure, with the pay lines
// already entered, as the Cost and budget page projects it.
func locMonthlyCost(ctx context.Context, q store.Q, filter bson.M) (map[string]float64, error) {
	ym := openYm(ctx, q)
	eligible, err := payEligible(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	f := bson.M{"status": "Active"}
	for k, v := range filter {
		f[k] = v
	}
	pc, list, err := loadPayCtxOpt(ctx, q, ym, false, eligible, f)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, e := range list {
		out[e.LocID] += costTotalOf(pc.row(e))
	}
	for k, v := range out {
		out[k] = calc.Round2(v)
	}
	return out, nil
}

// offerMonthlyCost is a new hire's projected total monthly cost (costTotalOf) from the offer's gross, employment type
// and property, on the default salary structure: a full month, without the minimum-wage top-up (j.TopUp).
func offerMonthlyCost(ctx context.Context, q store.Q, j offerJob) float64 {
	st := calc.DefaultStructure
	if x, _ := q.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}); x != nil {
		_ = payengineJSON(x["rates"], &st)
	}
	in := calc.Input{Type: calc.EmploymentType(j.Type), Gross: j.Gross, Structure: &st}
	if j.LocID != "" {
		in.Lwf = lwfFor(ctx, q, j.LocID, today())
	}
	res := calc.Calc(in)
	res.Meal, res.Accom = j.TopUp, 0 // the top-up is in the gross and is no company cost
	return calc.Round2(costTotalOf(&PayRowOut{Row: calc.Row{R: res, TotalEarn: res.TotalEarn}}))
}
