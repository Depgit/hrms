package store

// Registry additions for staff accommodation (workforce plan, module 4; decisions of 16–17 Nov 2026):
//
//   - hst_unit: a flat, house or PG the company rents for staff: name, kind, address, cluster, the properties it
//     serves in order of nearness (near_ids; the first is the property that carries its empty-unit cost), gender
//     (Male, Female, Any), beds, landlord and contact, status, and other fixed monthly items (fixed_items: a list of
//     {label, amount}).
//   - hst_lease: a unit's leases, the history kept: start, end (null = ongoing), notice period in days, monthly rent,
//     deposit, maintenance, yearly increase (% and the date of the first increase; it repeats every year on that
//     date). Leases of a unit never overlap. Documents are `document` rows with owner_kind "hst_lease".
//   - hst_cost: the variable costs HR enters for a unit and month (electricity, water, Wi-Fi, housekeeping, other).
//   - hst_stay: one person's stay in a unit's bed: check-in, check-out (null = staying). The person is in the unit on
//     the days check_in ≤ day < check_out, so the check-out day is free for the next person. Stays on one bed never
//     overlap, and nor do one person's stays.
//   - company_setting.hst: the module's settings (byDays: "Split by days stayed", off = equal per person; and the
//     reminder windows). Accommodation is never part of payroll (decided 17 Nov): the statutory switches of the first
//     build ("stat") are removed by healHst.
//
// The four collections never existed in PostgreSQL, so, like offer_version, they are kept out of schemaTables
// (import-pg does not list them) while C() applies their rules. Migration 012 creates them and their indexes.
// Nothing existing is rewritten; the per-employee accommodation deduction (employee_pay.accommodation) is untouched.

import (
	"context"
	"regexp"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	HstUnitColl  = "hst_unit"
	HstLeaseColl = "hst_lease"
	HstCostColl  = "hst_cost"
	HstStayColl  = "hst_stay"
)

var hstUnitTable = &Table{
	Name: HstUnitColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "name", Type: TText, NotNull: true},
		{Name: "kind", Type: TText, NotNull: true},
		{Name: "address", Type: TText},
		{Name: "cluster_id", Type: TUUID, NotNull: true},
		{Name: "near_ids", Type: TUUID, Array: true, NotNull: true, Default: Default{Kind: DefConst, Value: []string{}}},
		{Name: "gender", Type: TText, NotNull: true},
		{Name: "beds", Type: TInt4, NotNull: true},
		{Name: "landlord", Type: TText},
		{Name: "landlord_phone", Type: TText},
		{Name: "status", Type: TText, NotNull: true, Default: Default{Kind: DefConst, Value: "Active"}},
		{Name: "fixed_items", Type: TJSONB, NotNull: true, Default: Default{Kind: DefConst, Value: "[]"}},
		{Name: "note", Type: TText},
		{Name: "created_by", Type: TText, NotNull: true},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "updated_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "hst_unit_name_key", Keys: []IndexKey{{Field: "name"}}, Unique: true, Lower: true},
		{Name: "hst_unit_cluster_idx", Keys: []IndexKey{{Field: "cluster_id"}}},
	},
	FKs: []FK{
		{Name: "hst_unit_cluster_id_fkey", Col: "cluster_id", RefTable: "cluster", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "hst_unit_kind_check", Conds: []Cond{{Col: "kind", Op: "in", Val: []any{"Flat", "House", "PG"}}}},
		{Name: "hst_unit_gender_check", Conds: []Cond{{Col: "gender", Op: "in", Val: []any{"Male", "Female", "Any"}}}},
		{Name: "hst_unit_status_check", Conds: []Cond{{Col: "status", Op: "in", Val: []any{"Active", "Inactive"}}}},
		{Name: "hst_unit_beds_check", Conds: []Cond{{Col: "beds", Op: ">=", Val: float64(1)}, {Col: "beds", Op: "<=", Val: float64(200)}}},
	},
}

var hstLeaseTable = &Table{
	Name: HstLeaseColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "unit_id", Type: TUUID, NotNull: true},
		{Name: "start", Type: TDate, NotNull: true},
		{Name: "end", Type: TDate}, // null: ongoing
		{Name: "notice_days", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
		{Name: "rent", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2},
		{Name: "deposit", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2, Default: Default{Kind: DefConst, Value: float64(0)}},
		{Name: "maintenance", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2, Default: Default{Kind: DefConst, Value: float64(0)}},
		{Name: "inc_pct", Type: TNumeric, NotNull: true, Prec: 6, Scale: 2, Default: Default{Kind: DefConst, Value: float64(0)}},
		{Name: "inc_on", Type: TDate}, // the first yearly increase; repeats every year on that date
		{Name: "note", Type: TText},
		{Name: "created_by", Type: TText, NotNull: true},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "updated_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "hst_lease_unit_idx", Keys: []IndexKey{{Field: "unit_id"}, {Field: "start"}}},
	},
	FKs: []FK{
		{Name: "hst_lease_unit_id_fkey", Col: "unit_id", RefTable: HstUnitColl, RefCol: "id"},
	},
	Checks: []Check{
		{Name: "hst_lease_rent_check", Conds: []Cond{{Col: "rent", Op: ">=", Val: float64(0)}, {Col: "deposit", Op: ">=", Val: float64(0)},
			{Col: "maintenance", Op: ">=", Val: float64(0)}, {Col: "notice_days", Op: ">=", Val: float64(0)}}},
		{Name: "hst_lease_dates_check", Conds: []Cond{{Col: "end", Op: ">=", ValCol: "start"}}},
	},
}

var hstCostTable = &Table{
	Name: HstCostColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "unit_id", Type: TUUID, NotNull: true},
		{Name: "ym", Type: TText, NotNull: true},
		{Name: "items", Type: TJSONB, NotNull: true, Default: Default{Kind: DefConst, Value: "{}"}},
		{Name: "total", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2},
		{Name: "by_name", Type: TText, NotNull: true},
		{Name: "at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "hst_cost_unit_ym_key", Keys: []IndexKey{{Field: "unit_id"}, {Field: "ym"}}, Unique: true},
		{Name: "hst_cost_ym_idx", Keys: []IndexKey{{Field: "ym"}}},
	},
	FKs: []FK{
		{Name: "hst_cost_unit_id_fkey", Col: "unit_id", RefTable: HstUnitColl, RefCol: "id"},
	},
	Checks: []Check{
		{Name: "hst_cost_ym_check", Conds: []Cond{{Col: "ym", Op: "~", Val: regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)}}},
		{Name: "hst_cost_total_check", Conds: []Cond{{Col: "total", Op: ">=", Val: float64(0)}}},
	},
}

var hstStayTable = &Table{
	Name: HstStayColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "unit_id", Type: TUUID, NotNull: true},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "bed", Type: TInt4, NotNull: true},
		{Name: "check_in", Type: TDate, NotNull: true},
		{Name: "check_out", Type: TDate}, // null: staying
		{Name: "note", Type: TText},
		{Name: "out_reason", Type: TText},
		{Name: "created_by", Type: TText, NotNull: true},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "out_by", Type: TText},
		{Name: "out_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "hst_stay_unit_idx", Keys: []IndexKey{{Field: "unit_id"}, {Field: "check_in"}}},
		{Name: "hst_stay_employee_idx", Keys: []IndexKey{{Field: "employee_id"}, {Field: "check_in"}}},
	},
	FKs: []FK{
		{Name: "hst_stay_unit_id_fkey", Col: "unit_id", RefTable: HstUnitColl, RefCol: "id"},
		{Name: "hst_stay_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "hst_stay_bed_check", Conds: []Cond{{Col: "bed", Op: ">=", Val: float64(1)}}},
		{Name: "hst_stay_dates_check", Conds: []Cond{{Col: "check_out", Op: ">", ValCol: "check_in"}}},
	},
}

var hstTables = []*Table{hstUnitTable, hstLeaseTable, hstCostTable, hstStayTable}

var _ = registerHst()

func registerHst() bool {
	byName := map[string]*Table{}
	for _, t := range schemaTables {
		byName[t.Name] = t
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "company_setting":
			t.Columns = append(t.Columns, Column{Name: "hst", Type: TJSONB})
		case "document":
			for i := range t.Checks {
				if t.Checks[i].Name != "document_owner_kind_check" {
					continue
				}
				for j := range t.Checks[i].Conds {
					if c := &t.Checks[i].Conds[j]; c.Col == "owner_kind" && c.Op == "in" {
						if l, ok := c.Val.([]any); ok {
							c.Val = append(l, "hst_lease")
						}
					}
				}
			}
		}
	}
	for _, t := range hstTables {
		t.cols = map[string]*Column{}
		for i := range t.Columns {
			t.cols[t.Columns[i].Name] = &t.Columns[i]
		}
		tableByName[t.Name] = t
		byName[t.Name] = t
	}
	for _, t := range hstTables {
		for _, fk := range t.FKs {
			if rt := byName[fk.RefTable]; rt != nil {
				rt.refBy = append(rt.refBy, refFrom{t, fk})
			}
		}
	}
	migrations = append(migrations, migration{"012_accommodation", false, migrateHst})
	return true
}

// The migrations of the schema_*.go files are appended as each file's variables are set up (file name order);
// init runs after all of them, so the list ends in name order whichever file added what.
func init() {
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
}

// migrateHst is migration 012: the four accommodation collections and their indexes. Additive only.
func migrateHst(ctx context.Context, s *Store) error {
	for _, t := range hstTables {
		if err := ensureColl(ctx, s.DB.db, t.Name, t.Indexes); err != nil {
			return err
		}
	}
	return nil
}

// healHst drops the statutory switches the first build stored in company_setting.hst.stat (accommodation is not part
// of payroll, decided 17 Nov). Idempotent and cheap: it touches nothing once they are gone.
func healHst(ctx context.Context, s *Store) error {
	_, err := s.DB.db.Collection("company_setting").UpdateMany(ctx, bson.M{"hst.stat": bson.M{"$exists": true}}, bson.M{"$unset": bson.M{"hst.stat": ""}})
	return err
}
