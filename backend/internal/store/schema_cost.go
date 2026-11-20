package store

// Registry additions for the cost and budget console (workforce plan, module 6; decisions of 17 Nov 2026: no expense
// ledger, the only costs are salary and accommodation):
//
//   - cluster: headcount_budget and cost_budget (monthly). Null = not set: the cluster's budget is then the total of
//     its properties' budgets (location.headcount_budget and location.payroll_budget, which stay the source for
//     properties).
//   - cost_snapshot (new): one document per payroll month, written when the month is published (or by the
//     maintenance sweep for a month published before this build): every person's cost lines, each property's
//     empty-unit cost, the place names and the budgets as they were, so the console's figures for that month never
//     change afterwards.
//
// cost_snapshot never existed in PostgreSQL, so it is kept out of schemaTables (import-pg does not list it) while C()
// applies its rules. Migration 015 creates it and its index; nothing existing is rewritten.

import (
	"context"
	"sort"
)

// CostSnapshotColl holds the frozen cost figures of each published payroll month.
const CostSnapshotColl = "cost_snapshot"

var costSnapshotTable = &Table{
	Name: CostSnapshotColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "ym", Type: TChar, NotNull: true, Len: 7},
		{Name: "frozen_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "frozen_by", Type: TText, NotNull: true},
		{Name: "data", Type: TJSONB, NotNull: true},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "cost_snapshot_ym_key", Keys: []IndexKey{{Field: "ym"}}, Unique: true},
	},
}

var _ = registerCost()

func registerCost() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "cluster" {
			t.Columns = append(t.Columns,
				Column{Name: "headcount_budget", Type: TInt4},
				Column{Name: "cost_budget", Type: TNumeric, Prec: 14, Scale: 2})
			t.Checks = append(t.Checks,
				Check{Name: "cluster_headcount_budget_check", Expr: "CHECK (headcount_budget >= 0)", Conds: []Cond{{Col: "headcount_budget", Op: ">=", Val: float64(0)}}},
				Check{Name: "cluster_cost_budget_check", Expr: "CHECK (cost_budget >= 0)", Conds: []Cond{{Col: "cost_budget", Op: ">=", Val: float64(0)}}})
		}
	}
	t := costSnapshotTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	migrations = append(migrations, migration{"015_cost_console", false, migrateCost})
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
	return true
}

// migrateCost is migration 015: the cost_snapshot collection and its index. Additive only; the snapshots of months
// already published are written by the maintenance sweep (api.CostFreezeSweep).
func migrateCost(ctx context.Context, s *Store) error {
	return ensureColl(ctx, s.DB.db, CostSnapshotColl, costSnapshotTable.Indexes)
}
