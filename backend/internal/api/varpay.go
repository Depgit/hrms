package api

// Variable pay on each person's own cycle (owner's request of 14 Nov 2026). The payout cycle (1, 3, 6 or 12 months)
// is counted from the employee's joining month, as spec 14 has it ("payout in month m when ((m − joinMonth) mod cycle)
// == cycle − 1"): quarterly, joined in May 2026, the payouts fall in July and October 2026, January 2027 and so on. A
// mid-month joiner counts the joining month as month 1, and that month's accrual is pro rata by the days paid, as
// before; only the month a payout falls in is decided here, never its amount.
//
// When variable pay starts after joining (the % goes from nothing to something, by Edit pay options or the Salary
// breakup bulk update), the cycle is counted from the month it starts instead: employee_pay.var_start. For a change
// saved before var_start was kept, var_from is used when the % before it was nothing (varAnchorYm, payengine.go).

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// varStartSet adds var_start to an employee_pay update when the variable % goes from nothing (no % of their own and
// none in the structure, or 0) to something from month from. The structure's standard % stands in for a nil %.
func varStartSet(ctx context.Context, q store.Q, id string, before, after *float64, from string, set bson.M) {
	_, st := structureFor(ctx, q, id, from+"-01")
	eff := func(p *float64) float64 {
		if p != nil {
			return *p
		}
		return st.VarPct
	}
	if eff(before) <= 0 && eff(after) > 0 {
		set["var_start"] = from
	}
}

// varNextPayout is the first payout month of the variable cycle on or after month ym, counted from anchor (both
// "YYYY-MM"); cycle below 1 counts as 1.
func varNextPayout(anchor, ym string, cycle int) string {
	if cycle < 1 {
		cycle = 1
	}
	m := addYm(anchor, cycle-1)
	for m < ym {
		m = addYm(m, cycle)
	}
	return m
}

// varPayoutInfo is what the employee's pay section shows about the variable cycle: the month it is counted from and
// the next payout month from the open payroll month on ("" when the type earns no variable pay or the % is 0).
func varPayoutInfo(ctx context.Context, q store.Q, id, typ, doj string, p payOptions, st calc.Structure) map[string]any {
	if !calc.RulesFor(calc.EmploymentType(typ)).Variable || len(doj) < 7 {
		return nil
	}
	if st.Designed { // amounts entered by HR (paycomp.go), due on the same cycle counted from joining (calc.VarDue)
		for _, c := range calc.ComponentsOf(st) {
			if c.Kind == calc.CompVariable && c.Method != calc.MLegacyVar {
				anchor := varAnchorOf(doj, p.VarStart)
				from := openYm(ctx, q)
				if anchor > from {
					from = anchor
				}
				return map[string]any{"from": anchor, "cycle": max(1, c.Cycle), "next": varNextPayout(anchor, from, c.Cycle)}
			}
		}
		return nil
	}
	pct, cyc := st.VarPct, st.VarCycle
	if p.VarPct != nil {
		pct = *p.VarPct
	}
	if p.VarCycle != nil {
		cyc = *p.VarCycle
	}
	if pct <= 0 {
		return nil
	}
	var vf *varFrom
	if r, _ := q.C("employee_pay").FindOne(ctx, bson.M{"employee_id": id}); r != nil && str(r["var_from"]) != "" {
		vf = &varFrom{From: str(r["var_from"]), Prev: payengineFloatPtr(r["var_prev_pct"])}
	}
	anchor := varAnchorYm(doj[:7], p.VarStart, vf, st.VarPct)
	from := openYm(ctx, q)
	if anchor > from {
		from = anchor
	}
	return map[string]any{"from": anchor, "cycle": max(1, cyc), "next": varNextPayout(anchor, from, cyc)}
}
