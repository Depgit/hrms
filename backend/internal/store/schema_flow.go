package store

// Registry addition for the approval rule lists (owner's decision of 11 Nov 2026: approval routes are ordered rule
// lists, the first rule that fits decides):
//
//   - approval_flow {wf, flow, rules, no_response, updated_by, updated_at}: one document per workflow key (leaveC and
//     leaveP for leave) and flow (Operations or Corporate). rules is the list of approval.Rule, the last one
//     "Otherwise". This is what the app reads and HR edits.
//   - approval_matrix (the grids of condition rows × role columns used until then) is kept untouched as the
//     archive: migration 004 converts each grid into approval_flow (approval.Convert routes every request exactly
//     as the grid did) and leaves the grid where it was, so nothing is lost; nothing reads it any more except that
//     conversion. `hrms import-pg` still copies PostgreSQL's approval_matrix into it and converts after the copy.
//
// approval_flow never existed in PostgreSQL, so it is kept out of schemaTables like the mdm_* collections: Tables()
// (and with it the import, which copies every listed table) does not list it, while C() still applies its rules.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
)

// ApprovalFlowColl holds the approval rule lists.
const ApprovalFlowColl = "approval_flow"

var approvalFlowTable = &Table{
	Name: ApprovalFlowColl,
	Columns: []Column{
		{Name: "wf", Type: TText, NotNull: true},
		{Name: "flow", Type: TText, NotNull: true},
		{Name: "rules", Type: TJSONB, NotNull: true},
		{Name: "no_response", Type: TText, NotNull: true, Default: Default{Kind: DefConst, Value: "escalate"}},
		{Name: "updated_by", Type: TText},
		{Name: "updated_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		// hierarchy_off (18 Nov 2026, meaning changed 19 Nov): the "Approval hierarchy required" tick is cleared, so
		// requests still go to the approvers but HR may skip approval with a reason. Absent or false = ticked.
		{Name: "hierarchy_off", Type: TBool},
	},
	PK: []string{"wf", "flow"},
	Indexes: []Index{
		{Name: "approval_flow_pkey", Keys: []IndexKey{{Field: "wf"}, {Field: "flow"}}, Unique: true},
	},
	Checks: []Check{
		{Name: "approval_flow_flow_check", Conds: []Cond{{Col: "flow", Op: "in", Val: []any{"Operations", "Corporate"}}}},
		{Name: "approval_flow_no_response_check", Conds: []Cond{{Col: "no_response", Op: "in", Val: []any{"escalate", "auto", "wait"}}}},
	},
}

// Registered while the package's variables are initialised, like the master data sync's collections.
var _ = registerApprovalFlow()

func registerApprovalFlow() bool {
	t := approvalFlowTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	return true
}

// migrateApprovalFlow is migration 004: the approval_flow collection with its index, then every stored grid
// converted (ConvertApprovalMatrices). It can run again safely; `hrms import-pg` runs it again after the copy.
func migrateApprovalFlow(ctx context.Context, s *Store) error {
	if err := ensureApprovalFlow(ctx, s.DB.db); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		_, err := ConvertApprovalMatrices(ctx, tx)
		return err
	})
}

// MigrateApprovalFlow converts the grids an import has just copied (see migrateApprovalFlow).
func (s *Store) MigrateApprovalFlow(ctx context.Context) (int, error) {
	if err := ensureApprovalFlow(ctx, s.DB.db); err != nil {
		return 0, err
	}
	n := 0
	err := s.Tx(ctx, func(tx Q) error {
		var err error
		n, err = ConvertApprovalMatrices(ctx, tx)
		return err
	})
	return n, err
}

func ensureApprovalFlow(ctx context.Context, db *mongo.Database) error {
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: ApprovalFlowColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, ApprovalFlowColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", ApprovalFlowColl, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, ApprovalFlowColl, approvalFlowTable.Indexes)
}

// ConvertApprovalMatrices writes, for every grid in approval_matrix that has no rule list yet, its rule list
// (approval.Convert) with the grid's no_response, updated_by and updated_at. A grid still exactly as seeded gets the
// seeded list with meeting ranges instead (approval.SeedFlowUpgrade). Grids stay as they are. It returns how many
// rule lists it wrote; a rule list that exists (converted before, seeded, or edited since) is never replaced.
func ConvertApprovalMatrices(ctx context.Context, q Q) (int, error) {
	grids, err := q.C("approval_matrix").Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "wf", Value: 1}, {Key: "flow", Value: 1}}))
	if err != nil {
		return 0, err
	}
	have, err := q.C(ApprovalFlowColl).Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"wf": 1, "flow": 1}))
	if err != nil {
		return 0, err
	}
	done := map[string]bool{}
	for _, h := range have {
		done[fmt.Sprint(h["wf"], "/", h["flow"])] = true
	}
	n := 0
	for _, g := range grids {
		wf, _ := g["wf"].(string)
		flow, _ := g["flow"].(string)
		if done[wf+"/"+flow] {
			continue
		}
		var m approval.Matrix
		if err := flowJSON(g["cols"], &m.Cols); err != nil {
			return n, fmt.Errorf("approval grid %s/%s: %w", wf, flow, err)
		}
		if err := flowJSON(g["rows"], &m.Rows); err != nil {
			return n, fmt.Errorf("approval grid %s/%s: %w", wf, flow, err)
		}
		rs, err := approval.Convert(wf, m)
		if err != nil {
			return n, fmt.Errorf("approval grid %s/%s: %w", wf, flow, err)
		}
		if up, ok := approval.SeedFlowUpgrade(wf, approval.Flow(flow), rs.Rules); ok {
			rs = up
		}
		ins := bson.M{"rules": rs.Rules, "no_response": g["no_response"], "updated_by": g["updated_by"]}
		if g["no_response"] == nil {
			delete(ins, "no_response")
		}
		if g["updated_at"] != nil {
			ins["updated_at"] = g["updated_at"]
		}
		added, err := q.C(ApprovalFlowColl).Upsert(ctx, bson.M{"wf": wf, "flow": flow}, nil, ins)
		if err != nil {
			return n, err
		}
		if added {
			n++
		}
	}
	return n, nil
}

// migrateSeededRanges is migration 005: a rule list still exactly as seeded before (the sheet's ranges, with gaps a
// request could fall into and need no approver) becomes the seeded list whose ranges meet
// (approval.SeedFlowUpgrade). A list HR has changed is left as it is.
func migrateSeededRanges(ctx context.Context, s *Store) error {
	return s.Tx(ctx, func(tx Q) error {
		_, err := UpgradeSeededFlows(ctx, tx)
		return err
	})
}

// UpgradeSeededFlows replaces every stored rule list that is still exactly as seeded before with the seeded list
// whose ranges meet, and returns how many it replaced.
func UpgradeSeededFlows(ctx context.Context, q Q) (int, error) {
	rows, err := q.C(ApprovalFlowColl).Find(ctx, bson.M{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		var rules []approval.Rule
		if err := flowJSON(r["rules"], &rules); err != nil {
			continue // not a list this step knows; left as it is
		}
		wf, _ := r["wf"].(string)
		flow, _ := r["flow"].(string)
		up, ok := approval.SeedFlowUpgrade(wf, approval.Flow(flow), rules)
		if !ok {
			continue
		}
		if _, err := q.C(ApprovalFlowColl).Update(ctx, bson.M{"wf": wf, "flow": flow}, bson.M{"$set": bson.M{"rules": up.Rules, "updated_at": q.Now()}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// healApprovalFlows gives every grid that still has no rule list one (ConvertApprovalMatrices). Migrate runs it on
// every start, so a database whose conversion did not finish (an import stopped by an error after migration 004
// was recorded) gets its rule lists the next time the app starts. It is two reads when nothing is missing.
func healApprovalFlows(ctx context.Context, s *Store, log func(string)) error {
	if err := ensureApprovalFlow(ctx, s.DB.db); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		n, err := ConvertApprovalMatrices(ctx, tx)
		if err == nil && n > 0 {
			log(fmt.Sprintf("converted %d approval grid(s) that had no rule list", n))
		}
		return err
	})
}

// flowJSON decodes a stored jsonb value (a decoded document, or JSON text) into out.
func flowJSON(v any, out any) error {
	if s, ok := v.(string); ok {
		return json.Unmarshal([]byte(s), out)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
