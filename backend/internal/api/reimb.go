package api

// Reimbursement in the fixed pay (owner, 20 Nov 2026). Every salary structure carries a fixed earning "Reimbursement"
// (calc.ReimbComponent, method "per-employee amount"): its ₹ a month is entered on each employee (Pay options, Bulk
// update › Salary breakup column "Reimbursement (₹)", the salary and pay previews) and kept in
// employee_pay.comp_amts.reimb. It is part of the fixed monthly gross: Basic and HRA as the structure says, then the
// Reimbursement, then the balancing component (Special allowance) takes what is left, so the gross and CTC stay as
// entered. Flags by default: taxable, not PF, ESI or bonus wage (the component designer can change them). An amount
// above what is left after Basic and HRA (the balancing figure with no reimbursement) is refused. Nobody has one until
// HR enters it, so pay is unchanged (migration 024; `hrms pay-compare -dump` / `-against`).

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

// reimbRoom is what the fixed gross leaves after Basic, HRA and the other fixed earnings that are not the balancing
// figure: the most the Reimbursement can be. With amt as the Reimbursement the balancing figure stops at 0, so the
// fixed earnings go over the gross by exactly amt − room.
func reimbRoom(st calc.Structure, typ string, gross, amt float64) float64 {
	r := calc.Calc(calc.Input{Type: calc.EmploymentType(typ), Gross: gross, Structure: &st, EmpAmts: reimbAmts(amt)})
	over := math.Max(0, r.Basic+r.Hra+r.Sa+r.Reimb-gross)
	return calc.Round2(math.Max(0, amt-over))
}

// checkReimb refuses a Reimbursement the structure cannot hold: negative, on a structure without the component, or
// more than what is left after Basic and HRA.
func checkReimb(st calc.Structure, structName, typ string, gross, amt float64) error {
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
	if room := reimbRoom(st, typ, gross, amt); amt > room+0.005 {
		return bad("Reimbursement %s is more than what is left after Basic and HRA (%s on a monthly gross of %s). Lower it or raise the gross", inr(amt), inr(room), inr(gross))
	}
	return nil
}

// reimbCheckFor checks an amount for a person on their gross (gross > 0: the gross about to be set) and the structure
// in force on date.
func reimbCheckFor(ctx context.Context, q store.Q, id, typ, date string, gross, amt float64) error {
	if amt == 0 {
		return nil
	}
	if gross <= 0 {
		g, ok := grossFor(ctx, q, id, date[:7])
		if !ok {
			return bad("Set the monthly gross before the Reimbursement")
		}
		gross = g
	}
	name, st := structureFor(ctx, q, id, date)
	return checkReimb(st, name, typ, gross, amt)
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

// offerReimbProblems checks the offer's Reimbursement against its gross and salary structure (the one chosen, else the
// default), as Pay options does for an employee.
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
	if amt == 0 || j.Gross <= 0 || j.Type == "" {
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
	if err := checkReimb(st, name, j.Type, j.Gross, amt); err != nil {
		*errs = append(*errs, err.Error())
	}
}
