package store

// Registry additions for the Reimbursement in the fixed pay and for short leave (owner, 20 Nov 2026):
//
//   - employee_pay.comp_amts: the person's own ₹ a month for the per-employee pay components (calc.MPerEmp), by
//     component code, e.g. {"reimb": 3000}. Null or missing: 0, the pay as before.
//   - pay_component.method "per_emp": the amount is entered on each employee.
//   - Short leave needs no new field outside jsonb: a short leave is a leave request (wf leave) with payload.short,
//     payload.fromTime / toTime and payload.ltype "Short leave"; the plan's limits are leave_plan.lv.shortOff / shortN /
//     shortH; a half day's half is payload.halfOf ("first" or "second").
//
// Migration 024 adds the "Reimbursement" component (calc.ReimbComponent) to the component library and to every salary
// structure that carries components, just before its fixed balancing component (Special allowance). Nobody has an
// amount, so every calculation is unchanged (`hrms pay-compare -dump` before and `-against` after proves it on a copy).

import (
	"context"
	"encoding/json"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
)

var _ = registerReimb()

func registerReimb() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "employee_pay" {
			t.Columns = append(t.Columns, Column{Name: "comp_amts", Type: TJSONB})
		}
	}
	migrations = append(migrations, migration{"024_reimbursement", false, func(ctx context.Context, s *Store) error {
		return s.Tx(ctx, func(tx Q) error {
			if _, err := SeedPayComponents(ctx, tx); err != nil {
				return err
			}
			_, err := AddReimbToStructures(ctx, tx)
			return err
		})
	}})
	return true
}

// AddReimbToStructures puts the Reimbursement component into every structure's rates.comps that has none, and returns
// how many structures changed. Only rates.comps is set. Safe to run again.
func AddReimbToStructures(ctx context.Context, q Q) (int, error) {
	list, err := q.C("salary_structure").Find(ctx, bson.M{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, x := range list {
		rates, _ := x["rates"].(map[string]any)
		if c, ok := rates["comps"].([]any); !ok || len(c) == 0 {
			continue // converted on the fly (calc.LegacyComponents, which has it)
		}
		b, err := json.Marshal(rates["comps"])
		if err != nil {
			return n, err
		}
		var comps []calc.Component
		if err := json.Unmarshal(b, &comps); err != nil {
			return n, err
		}
		next, added := calc.WithReimb(comps)
		if !added {
			continue
		}
		docs, err := toDocs(next)
		if err != nil {
			return n, err
		}
		if _, err := q.C("salary_structure").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"rates.comps": docs}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
