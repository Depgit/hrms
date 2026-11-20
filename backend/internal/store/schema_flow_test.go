package store

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/approval"
)

// Migration 004: every grid becomes a rule list that routes as it did, with its no-response setting and who changed
// it last; the grids stay; running it again changes nothing and never replaces a rule list HR has edited since.
func TestApprovalFlowMigration(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	// Reset converted migration 002's grids (shortlist, interview, p_ack, p_pip).
	grids, _ := q.C("approval_matrix").Count(ctx, nil)
	flows, _ := q.C(ApprovalFlowColl).Count(ctx, nil)
	if grids == 0 || flows != grids {
		t.Fatalf("%d grids, %d rule lists after Reset", grids, flows)
	}
	iv, _ := q.C(ApprovalFlowColl).FindOne(ctx, bson.M{"wf": "interview", "flow": "Operations"})
	if iv["no_response"] != "wait" {
		t.Fatalf("interview keeps its no-response setting: %v", iv)
	}
	// An edited grid (as HR left it in the old editor).
	m := approval.SeedMatrices()["leaveP"][approval.Operations]
	m.Rows[3].On = false
	when := time.Date(2026, 11, 2, 10, 30, 0, 0, time.UTC)
	if _, err := q.C("approval_matrix").Insert(ctx, bson.M{"wf": "leaveP", "flow": "Operations", "cols": m.Cols, "rows": m.Rows,
		"no_response": "auto", "updated_by": "Asha", "updated_at": when}); err != nil {
		t.Fatal(err)
	}
	n, err := s.MigrateApprovalFlow(ctx)
	if err != nil || n != 1 {
		t.Fatalf("converted %d (%v), want 1", n, err)
	}
	r, _ := q.C(ApprovalFlowColl).FindOne(ctx, bson.M{"wf": "leaveP", "flow": "Operations"})
	if r["no_response"] != "auto" || r["updated_by"] != "Asha" || !r["updated_at"].(time.Time).Equal(when) {
		t.Fatalf("kept settings: %v", r)
	}
	var rules []approval.Rule
	if err := flowJSON(r["rules"], &rules); err != nil {
		t.Fatal(err)
	}
	want := approval.MustConvert("leaveP", m)
	if len(rules) != len(want.Rules) || rules[len(rules)-1].Label != approval.OtherwiseLabel {
		t.Fatalf("rules %+v", rules)
	}
	f := approval.Facts{WF: "leave", Roll: 7, Grade: "G3"} // the switched-off grade row no longer applies
	if c := approval.Build(approval.RuleSet{Rules: rules}, f, func(r string) string { return r }); len(c.Chain) != 4 || len(c.Notify) != 0 {
		t.Fatalf("route %v", c)
	}
	// Again: nothing new. An edited rule list stays as HR left it.
	if _, err := q.C(ApprovalFlowColl).Update(ctx, bson.M{"wf": "leaveP", "flow": "Operations"}, bson.M{"$set": bson.M{"rules": []approval.Rule{{Label: approval.OtherwiseLabel, On: true}}}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MigrateApprovalFlow(ctx); err != nil || n != 0 {
		t.Fatalf("second run converted %d (%v)", n, err)
	}
	r, _ = q.C(ApprovalFlowColl).FindOne(ctx, bson.M{"wf": "leaveP", "flow": "Operations"})
	if l, _ := r["rules"].([]any); len(l) != 1 {
		t.Fatalf("edited rule list replaced: %v", r["rules"])
	}
	if g, _ := q.C("approval_matrix").Count(ctx, nil); g != grids+1 {
		t.Fatalf("grids kept: %d", g)
	}
	// The registry applies its checks.
	if _, err := q.C(ApprovalFlowColl).Insert(ctx, bson.M{"wf": "x", "flow": "Nowhere", "rules": []any{}}); err == nil {
		t.Fatal("flow check")
	}
}
