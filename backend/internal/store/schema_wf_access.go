package store

// Registry additions for access and policy conflicts (workforce plan, modules 2 and 5; 17 Nov 2026):
//
//   - user_role_map.access may also be clusters, cities or states; user_role_map.area_ids holds the ids of those
//     clusters, cities or states. The locations they cover are worked out on every request, so a location added to
//     a cluster later is covered at once. Null on every existing mapping (all or locs, as before).
//   - user_right (new collection): a right given to or blocked for one person on top of their roles: right_id, kind
//     (grant | block), level (grant: the level given; block: the highest level left, 0 = none), an optional last
//     day (until), the reason, and who set and who ended it. Ended rows stay (active false) as the history.
//   - work_policy_map.exception: true on an employee-level mapping saved by the policy conflict check (HR chose
//     Replace, Keep or Pick); work_policy_map.note says why ("Kept on 6-day policy when 5.5-day policy was put on
//     Gurugram"). Both null on existing mappings.
//   - company_policy.audience (jsonb) may also hold clusters, cities and employees (no schema change).
//   - comp_type_rule.rules (jsonb) may also hold leaveTypes: with leave on, only these leave types (no schema
//     change).
//
// Migration 011 creates user_right and its index. Nothing existing is rewritten.

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// UserRightColl holds personal rights (grants and blocks).
const UserRightColl = "user_right"

// Role mapping access values.
const (
	AccessAll      = "all"
	AccessLocs     = "locs"
	AccessClusters = "clusters"
	AccessCities   = "cities"
	AccessStates   = "states"
)

var userRightTable = &Table{
	Name: UserRightColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "right_id", Type: TText, NotNull: true},
		{Name: "kind", Type: TText, NotNull: true},
		{Name: "level", Type: TInt2, NotNull: true},
		{Name: "until", Type: TDate},
		{Name: "reason", Type: TText},
		{Name: "active", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
		{Name: "created_by", Type: TText},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "ended_by", Type: TText},
		{Name: "ended_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "user_right_employee_idx", Keys: []IndexKey{{Field: "employee_id"}, {Field: "active"}}},
	},
	FKs: []FK{
		{Name: "user_right_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "user_right_kind_check", Expr: "CHECK (kind IN ('grant','block'))", Conds: []Cond{{Col: "kind", Op: "in", Val: []any{"grant", "block"}}}},
		{Name: "user_right_level_check", Expr: "CHECK (level >= 0 AND level <= 2)", Conds: []Cond{{Col: "level", Op: ">=", Val: float64(0)}, {Col: "level", Op: "<=", Val: float64(2)}}},
	},
}

var _ = registerAccessPolicy()

func registerAccessPolicy() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "user_role_map":
			t.Columns = append(t.Columns, Column{Name: "area_ids", Type: TUUID, Array: true})
			for i := range t.Checks {
				if t.Checks[i].Name != "user_role_map_access_check" {
					continue
				}
				t.Checks[i].Expr = "CHECK (access IN ('all','locs','clusters','cities','states'))"
				t.Checks[i].Conds = []Cond{{Col: "access", Op: "in", Val: []any{AccessAll, AccessLocs, AccessClusters, AccessCities, AccessStates}}}
			}
		case "work_policy_map":
			t.Columns = append(t.Columns, Column{Name: "exception", Type: TBool}, Column{Name: "note", Type: TText})
		}
	}
	t := userRightTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if emp := tableByName["employee"]; emp != nil {
		emp.refBy = append(emp.refBy, refFrom{t, t.FKs[0]})
	}
	migrations = append(migrations, migration{"011_access_policy", false, migrateAccessPolicy})
	return true
}

// migrateAccessPolicy is migration 011: the user_right collection and its index. Additive only.
func migrateAccessPolicy(ctx context.Context, s *Store) error {
	db := s.DB.db
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: UserRightColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, UserRightColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", UserRightColl, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, UserRightColl, userRightTable.Indexes)
}
