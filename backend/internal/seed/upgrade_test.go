package seed

// Upgrading a database from the approval grids (approval_matrix) to the rule lists (approval_flow), in either order
// of `hrms seed` and the migrations, keeps HR's edited grids. The database is HRMS_TEST_MONGO_SEED_DB with
// "_upgrade" added (default hrms_test_seed_upgrade), dropped first.

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

func upgradeStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	url := os.Getenv("HRMS_TEST_MONGO_URL")
	if url == "" {
		t.Skip("HRMS_TEST_MONGO_URL not set")
	}
	name := os.Getenv("HRMS_TEST_MONGO_SEED_DB")
	if name == "" {
		name = "hrms_test_seed"
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url, name+"_upgrade")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, ctx
}

func fpp(v float64) *float64 { return &v }

// preUpgrade leaves the database as it was before the rule lists: approval_matrix holds the seed grids as edited by
// edit, there is no approval_flow and migrations 004 and 005 are not recorded.
func preUpgrade(t *testing.T, ctx context.Context, st *store.Store, edit func(map[string]map[approval.Flow]approval.Matrix)) {
	t.Helper()
	db := st.DB.Database()
	if err := db.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection(store.MigrationsColl).DeleteMany(ctx, bson.M{"_id": bson.M{"$in": []string{"004_approval_rules", "005_seeded_ranges_meet"}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Collection(store.ApprovalFlowColl).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	grids := approval.SeedMatrices()
	edit(grids)
	for wf, fl := range grids {
		for flow, m := range fl {
			cols, _ := json.Marshal(m.Cols)
			rows, _ := json.Marshal(m.Rows)
			noResp := "escalate"
			if wf == "leaveC" {
				noResp = "wait"
			}
			if _, err := st.DB.C("approval_matrix").Insert(ctx, bson.M{"wf": wf, "flow": string(flow), "cols": string(cols), "rows": string(rows), "no_response": noResp, "updated_by": "HR"}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func flowOf(t *testing.T, ctx context.Context, st *store.Store, wf, flow string) (approval.RuleSet, store.Row) {
	t.Helper()
	r, err := st.DB.C(store.ApprovalFlowColl).FindOne(ctx, bson.M{"wf": wf, "flow": flow})
	if err != nil || r == nil {
		t.Fatalf("no rule list %s/%s: %v", wf, flow, err)
	}
	b, _ := json.Marshal(r["rules"])
	var rs approval.RuleSet
	if err := json.Unmarshal(b, &rs.Rules); err != nil {
		t.Fatal(err)
	}
	return rs, r
}

func chainOf(rs approval.RuleSet, f approval.Facts) []string {
	return approval.Build(rs, f, func(r string) string { return r }).Chain
}

// HR made a 1-day casual leave (Operations) also need the HRBP.
func hrEdit(g map[string]map[approval.Flow]approval.Matrix) {
	g["leaveC"][approval.Operations].Rows[0].Cells = map[string]string{approval.LM: "A", approval.PM: "A", approval.HB: "A"}
}

func checkUpgraded(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	rs, row := flowOf(t, ctx, st, "leaveC", "Operations")
	if c := chainOf(rs, approval.Facts{WF: "leave", Roll: 1}); len(c) != 3 {
		t.Errorf("HR's edited grid was not kept: 1-day casual leave goes to %v", c)
	}
	if row["no_response"] != "wait" || row["updated_by"] != "HR" {
		t.Errorf("the grid's settings were not kept: %v %v", row["no_response"], row["updated_by"])
	}
	// The untouched seed grids get the seeded lists whose ranges meet.
	rs, _ = flowOf(t, ctx, st, "p_claim", "Operations")
	if !reflect.DeepEqual(rs, approval.SeedFlows()["p_claim"][approval.Operations]) {
		t.Errorf("p_claim: %+v", rs)
	}
	if c := chainOf(rs, approval.Facts{WF: "p_claim", Amt: 5000.5}); len(c) != 3 {
		t.Errorf("a ₹5,000.50 claim goes to %v", c)
	}
	if n, _ := st.DB.C(store.ApprovalFlowColl).Count(ctx, nil); n != 34 {
		t.Errorf("%d rule lists", n)
	}
	if n, _ := st.DB.C("approval_matrix").Count(ctx, nil); n != 34 {
		t.Errorf("%d grids kept", n)
	}
}

// The README says to run `make seed` after upgrading: seed may run before the app migrates.
func TestUpgradeSeedBeforeMigrate(t *testing.T) {
	st, ctx := upgradeStore(t)
	preUpgrade(t, ctx, st, hrEdit)
	if err := Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	checkUpgraded(t, ctx, st)
}

func TestUpgradeMigrateThenSeed(t *testing.T) {
	st, ctx := upgradeStore(t)
	preUpgrade(t, ctx, st, hrEdit)
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.MigrateApprovalFlow(ctx); err != nil || n != 0 {
		t.Fatalf("second conversion: %d %v", n, err)
	}
	if err := Reference(ctx, st); err != nil {
		t.Fatal(err)
	}
	checkUpgraded(t, ctx, st)
}

// A grid with one "From" cleared and another typed as -1, on overlapping rows, converts and routes as it did.
func TestUpgradeNegativeFrom(t *testing.T) {
	st, ctx := upgradeStore(t)
	preUpgrade(t, ctx, st, func(g map[string]map[approval.Flow]approval.Matrix) {
		m := g["leaveC"][approval.Operations]
		m.Rows[0].C.Min = fpp(-1)
		m.Rows[1].C.Min = nil
		m.Rows[1].Cells = map[string]string{approval.LM: "A", approval.PM: "A", approval.HB: "A"}
	})
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatalf("serve would exit: %v", err)
	}
	rs, _ := flowOf(t, ctx, st, "leaveC", "Operations")
	for days, want := range map[float64]int{0.5: 3, 1: 3, 2: 3, 2.5: 0, 3: 3} {
		if c := chainOf(rs, approval.Facts{WF: "leave", Roll: days}); len(c) != want {
			t.Errorf("%v days: %v", days, c)
		}
	}
	if _, err := approval.Check(rs.Rules, func(string) bool { return true }); err != nil {
		t.Errorf("HR cannot save the converted list: %v", err)
	}
}

// Migration 005 replaces a list still exactly as seeded before, and leaves an edited one alone.
func TestUpgradeSeededRanges(t *testing.T) {
	st, ctx := upgradeStore(t)
	preUpgrade(t, ctx, st, func(map[string]map[approval.Flow]approval.Matrix) {})
	db := st.DB.Database()
	// The state after the first release of the rule lists: every grid converted as it is, 004 recorded, 005 not.
	old := approval.SeedMatrices()
	for _, wf := range []string{"leaveC", "p_claim"} {
		for _, flow := range []approval.Flow{approval.Operations, approval.Corporate} {
			rules := approval.MustConvert(wf, old[wf][flow]).Rules
			if wf == "p_claim" && flow == approval.Corporate {
				rules[0].Approvers = []string{approval.LM}
			}
			if _, err := st.DB.C(store.ApprovalFlowColl).Insert(ctx, bson.M{"wf": wf, "flow": string(flow), "rules": rules}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	seeded := approval.SeedFlows()
	for _, k := range [][2]string{{"leaveC", "Operations"}, {"leaveC", "Corporate"}, {"p_claim", "Operations"}} {
		if rs, _ := flowOf(t, ctx, st, k[0], k[1]); !reflect.DeepEqual(rs, seeded[k[0]][approval.Flow(k[1])]) {
			t.Errorf("%v not upgraded: %+v", k, rs)
		}
	}
	rs, _ := flowOf(t, ctx, st, "p_claim", "Corporate")
	if rs.Rules[0].Approvers[0] != approval.LM || len(rs.Rules[0].Approvers) != 1 || rs.Rules[0].Label != "Up to ₹5,000" || len(rs.Rules) != 4 {
		t.Errorf("an edited list was changed: %+v", rs)
	}
	// An import that stopped after 004 was recorded: the next start converts what is missing.
	if _, err := db.Collection(store.ApprovalFlowColl).DeleteMany(ctx, bson.M{"wf": "leaveP"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, func(string) {}); err != nil {
		t.Fatal(err)
	}
	flowOf(t, ctx, st, "leaveP", "Operations")
	flowOf(t, ctx, st, "leaveP", "Corporate")
}
