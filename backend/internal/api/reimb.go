package api

// Reimbursement (owner, 20 Nov 2026; outside the gross since 25 Nov 2026). Every salary structure carries a fixed
// earning "Reimbursement" (calc.ReimbComponent, method "per-employee amount"): its ₹ a month is entered on each employee
// (Pay options, Bulk update › Salary breakup column "Reimbursement (₹)", the salary and pay previews, the offer) and
// kept in employee_pay.comp_amts.reimb. It is paid OUTSIDE the fixed monthly gross, like variable pay: the gross split
// (Basic, HRA, Special allowance) is exactly as with no reimbursement, the amount is earned by the days paid as the gross
// is, it is in no PF, ESI or bonus wage, and it is added to total earnings, net pay and the CTC/cost. Flags by default:
// taxable (the component designer can change it). Any amount of zero or more is taken (no "room" check any more).
// Nobody has one until HR enters it, so pay is unchanged (migration 024; `hrms pay-compare`).

import (
	"context"
	"math"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const reimbCol = "Reimbursement (₹)"

// reimbOf is the person's Reimbursement ₹ a month (0: none).
func reimbOf(p payOptions) float64 { return p.CompAmts[calc.ReimbCode] }

// hasPerEmp: the structure has a per-employee component with this code.
func hasPerEmp(st calc.Structure, code string) bool {
	for _, c := range calc.ComponentsOf(st) {
		if c.Code == code && c.Method == calc.MPerEmp && c.Kind == calc.CompFixed {
			return true
		}
	}
	return false
}

// checkReimb refuses a Reimbursement the structure cannot pay: negative, on a structure without the component, or for
// a retainer.
func checkReimb(st calc.Structure, structName, typ string, amt float64) error {
	switch {
	case amt < 0:
		return bad("Reimbursement cannot be negative")
	case amt == 0:
		return nil
	case !hasPerEmp(st, calc.ReimbCode):
		return bad("%s has no Reimbursement component. Add it in Compensation › Pay components", or(structName, "This salary structure"))
	case calc.EmploymentType(typ) == calc.Retainer:
		return bad("A retainer is paid one retainer fee, so it has no Reimbursement")
	}
	return nil
}

// reimbCheckFor checks an amount for a person on the structure in force on date.
func reimbCheckFor(ctx context.Context, q store.Q, id, typ, date string, amt float64) error {
	if amt == 0 {
		return nil
	}
	name, st := structureFor(ctx, q, id, date)
	return checkReimb(st, name, typ, amt)
}

// setReimb records a person's Reimbursement ₹ a month (employee_pay.comp_amts.reimb) with the change history; nil when
// it is unchanged.
func setReimb(ctx context.Context, tx store.Q, c *Ctx, id string, cur, amt float64, reason, eff, source string) (*bulkChange, error) {
	if math.Abs(cur-amt) < 0.005 {
		return nil, nil
	}
	var v any = calc.Round2(amt)
	if amt == 0 {
		v = nil
	}
	if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"comp_amts." + calc.ReimbCode: v}, nil); err != nil {
		return nil, err
	}
	ch := bulkChange{Field: "Reimbursement", From: rupees(cur), To: rupees(amt)}
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": ch.Field, "old_value": ch.From, "new_value": ch.To, "reason": reason,
		"effective_from": eff, "source": source, "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, c, "Reimbursement changed", "employee", id, map[string]any{"reimb": cur}, map[string]any{"reimb": amt, "reason": reason}); err != nil {
		return nil, err
	}
	return &ch, nil
}

// reimbAmts is the per-employee amounts for a preview with a Reimbursement of amt (nil when 0).
func reimbAmts(amt float64) map[string]float64 {
	if amt <= 0 {
		return nil
	}
	return map[string]float64{calc.ReimbCode: amt}
}

// offerReimb is the offer's Reimbursement, ₹ a month (offer.data.reimb; 0 when blank).
func offerReimb(d map[string]string) float64 {
	if v := numOr(d["reimb"]); v != nil {
		return *v
	}
	return 0
}

// offerReimbProblems checks the offer's Reimbursement against its salary structure (the one chosen, else the default),
// as Pay options does for an employee.
func offerReimbProblems(ctx context.Context, q store.Q, j offerJob, d map[string]string, errs *[]string) {
	v := strings.TrimSpace(d["reimb"])
	if v == "" {
		return
	}
	amt, err := strconv.ParseFloat(v, 64)
	if err != nil || amt < 0 {
		*errs = append(*errs, "Reimbursement must be an amount of zero or more")
		return
	}
	if amt == 0 || j.Type == "" {
		return
	}
	sid := d["structId"]
	if sid == "" {
		sid = compStructureDefault(ctx, q)
	}
	name, st := "", calc.DefaultStructure
	if x, s, err := structureDoc(ctx, q, sid); err == nil {
		name, st = str(x["name"]), s
	}
	if err := checkReimb(st, name, j.Type, amt); err != nil {
		*errs = append(*errs, err.Error())
	}
}
