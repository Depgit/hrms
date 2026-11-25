package store

// Retainer TDS 1% (owner, 25 Nov 2026: "TDS 1 percent, why are you using TDS 10%? It is 1%, and it should be
// editable"). calc.DefaultStructure's TdsRate is now 1; migration 030 sets every stored salary structure whose
// rates.tdsRate is still 10 (the old default) to 1, with an audit_log row per structure. A structure whose rate an
// admin set to anything else is left alone. The rate stays editable per structure (Compensation › structure "Retainer
// TDS") and per employee (employee_pay.tds_pct, Pay options). On 25 Nov 2026 hrms had no Retainer, so no pay changes.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var _ = registerTds()

func registerTds() bool {
	migrations = append(migrations, migration{"030_retainer_tds_1pct", false, func(ctx context.Context, s *Store) error {
		return s.Tx(ctx, func(tx Q) error {
			_, err := TdsOldDefaultToOne(ctx, tx)
			return err
		})
	}})
	return true
}

// TdsOldDefaultToOne sets rates.tdsRate 10 → 1 on every salary structure that has the old default, audits each, and
// returns how many changed. Safe to run again.
func TdsOldDefaultToOne(ctx context.Context, q Q) (int, error) {
	list, err := q.C("salary_structure").Find(ctx, bson.M{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, x := range list {
		rates, _ := x["rates"].(map[string]any)
		if rates == nil {
			continue
		}
		var v float64
		switch t := rates["tdsRate"].(type) {
		case float64:
			v = t
		case int32:
			v = float64(t)
		case int64:
			v = float64(t)
		default:
			continue
		}
		if v != 10 {
			continue
		}
		if _, err := q.C("salary_structure").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"rates.tdsRate": 1.0}}); err != nil {
			return n, err
		}
		if _, err := q.C("audit_log").Insert(ctx, bson.M{"actor": "System (migration 030)", "action": "Retainer TDS set to 1% (was the old default 10%)",
			"entity": "salary_structure", "entity_id": x["id"], "before": bson.M{"tdsRate": v}, "after": bson.M{"tdsRate": 1.0}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
