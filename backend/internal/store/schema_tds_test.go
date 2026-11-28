package store

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Migration 030: a structure on the old default retainer TDS (10%) moves to 1%, audited; any other rate an admin set
// is kept; a second run changes nothing.
func TestTdsOldDefaultToOne(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	old := mustInsert(t, ctx, q, "salary_structure", bson.M{"name": "Version 1", "rates": bson.M{"basicPct": 50.0, "tdsRate": 10.0}}).(string)
	own := mustInsert(t, ctx, q, "salary_structure", bson.M{"name": "Version 2", "rates": bson.M{"basicPct": 50.0, "tdsRate": 2.0}}).(string)
	n, err := TdsOldDefaultToOne(ctx, q)
	if err != nil || n != 1 {
		t.Fatalf("changed %d %v, want 1", n, err)
	}
	rate := func(id string) any {
		x, _ := q.C("salary_structure").FindOne(ctx, bson.M{"_id": id})
		return x["rates"].(map[string]any)["tdsRate"]
	}
	if rate(old) != 1.0 || rate(own) != 2.0 {
		t.Fatalf("rates %v %v", rate(old), rate(own))
	}
	if c, _ := q.C("audit_log").Count(ctx, bson.M{"entity": "salary_structure", "entity_id": old}); c != 1 {
		t.Fatalf("audit rows %d", c)
	}
	if n, err := TdsOldDefaultToOne(ctx, q); err != nil || n != 0 {
		t.Fatalf("second run %d %v", n, err)
	}
}
