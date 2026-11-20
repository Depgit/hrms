package store

// Registry additions for pay components (workforce plan, module 3; 17 Nov 2026):
//
//   - pay_component (new): the component library the designer builds structures from: code (unique, lower case),
//     name, kind (fixed, variable), side (earn, ded), role (basic, da, hra or none), method (amount, pct_ctc,
//     pct_gross, pct_basic, pct_of, remainder), value, of (pct_of: another component's code), min and max (a month;
//     null = none), cycle (variable: 1, 3, 6 or 12 months), the flags taxable, pf, esi, bonus and payslip, and status.
//     A structure keeps a copy of each component it uses (salary_structure.rates.comps), so editing the library never
//     changes a structure already in use.
//   - salary_structure.rates.comps / rates.designed (inside the existing jsonb): the structure's components.
//     Migration 014 converts every existing structure into its equivalent template (calc.LegacyComponents: Basic as %
//     of gross, HRA as % of Basic, Special allowance as the remainder, variable as "legacy accrued % of gross") and
//     changes nothing else; a structure without comps is converted on the fly, with the same result.
//   - var_target (new): a person's monthly variable target for designed structures, dated like gross_history:
//     employee_id, from_month, total (₹ a month), by_comp (optional ₹ a month per component code), reason, by.
//   - var_payout (new): the variable amount HR enters to pay: employee_id, ym (the payroll month, or "fnf" for the
//     full and final), code (the component), amount, note, by_name, at. One per person, month and component.
//
// The three collections never existed in PostgreSQL, so they are kept out of schemaTables (import-pg does not list
// them) while C() applies their rules. Existing pay data (gross_history, employee_pay, structure_map, payroll rows) is
// not touched, so every legacy employee's pay is unchanged (hrms pay-compare proves it on a copy).

import (
	"context"
	"encoding/json"
	"regexp"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
)

const (
	PayComponentColl = "pay_component"
	VarTargetColl    = "var_target"
	VarPayoutColl    = "var_payout"
)

var payComponentTable = &Table{
	Name: PayComponentColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "code", Type: TText, NotNull: true},
		{Name: "name", Type: TText, NotNull: true},
		{Name: "kind", Type: TText, NotNull: true},
		{Name: "side", Type: TText, NotNull: true},
		{Name: "role", Type: TText},
		{Name: "method", Type: TText, NotNull: true},
		{Name: "value", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2, Default: Default{Kind: DefConst, Value: float64(0)}},
		{Name: "of", Type: TText},
		{Name: "min", Type: TNumeric, Prec: 12, Scale: 2},
		{Name: "max", Type: TNumeric, Prec: 12, Scale: 2},
		{Name: "cycle", Type: TInt4},
		{Name: "taxable", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
		{Name: "pf", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
		{Name: "esi", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
		{Name: "bonus", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
		{Name: "payslip", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
		{Name: "status", Type: TText, NotNull: true, Default: Default{Kind: DefConst, Value: "Active"}},
		{Name: "created_by", Type: TText},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "updated_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "pay_component_code_key", Keys: []IndexKey{{Field: "code"}}, Unique: true},
		{Name: "pay_component_name_key", Keys: []IndexKey{{Field: "name"}}, Unique: true, Lower: true},
	},
	Checks: []Check{
		{Name: "pay_component_kind_check", Conds: []Cond{{Col: "kind", Op: "in", Val: []any{"fixed", "variable"}}}},
		{Name: "pay_component_side_check", Conds: []Cond{{Col: "side", Op: "in", Val: []any{"earn", "ded"}}}},
		{Name: "pay_component_method_check", Conds: []Cond{{Col: "method", Op: "in", Val: []any{"amount", "pct_ctc", "pct_gross", "pct_basic", "pct_of", "remainder", "per_emp"}}}},
		{Name: "pay_component_status_check", Conds: []Cond{{Col: "status", Op: "in", Val: []any{"Active", "Inactive"}}}},
		{Name: "pay_component_value_check", Conds: []Cond{{Col: "value", Op: ">=", Val: float64(0)}}},
	},
}

var varTargetTable = &Table{
	Name: VarTargetColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "from_month", Type: TText, NotNull: true},
		{Name: "total", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2},
		{Name: "by_comp", Type: TJSONB},
		{Name: "reason", Type: TText},
		{Name: "created_by", Type: TText},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "var_target_emp_month_key", Keys: []IndexKey{{Field: "employee_id"}, {Field: "from_month"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "var_target_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "var_target_month_check", Conds: []Cond{{Col: "from_month", Op: "~", Val: regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)}}},
		{Name: "var_target_total_check", Conds: []Cond{{Col: "total", Op: ">=", Val: float64(0)}}},
	},
}

var varPayoutTable = &Table{
	Name: VarPayoutColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "ym", Type: TText, NotNull: true},
		{Name: "code", Type: TText, NotNull: true},
		{Name: "amount", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2},
		{Name: "note", Type: TText},
		{Name: "by_name", Type: TText},
		{Name: "at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "var_payout_emp_ym_code_key", Keys: []IndexKey{{Field: "employee_id"}, {Field: "ym"}, {Field: "code"}}, Unique: true},
		{Name: "var_payout_ym_idx", Keys: []IndexKey{{Field: "ym"}}},
	},
	FKs: []FK{
		{Name: "var_payout_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "var_payout_ym_check", Conds: []Cond{{Col: "ym", Op: "~", Val: regexp.MustCompile(`^(\d{4}-(0[1-9]|1[0-2])|fnf)$`)}}},
		{Name: "var_payout_amount_check", Conds: []Cond{{Col: "amount", Op: ">=", Val: float64(0)}}},
	},
}

var payCompTables = []*Table{payComponentTable, varTargetTable, varPayoutTable}

var _ = registerPayComp()

func registerPayComp() bool {
	byName := map[string]*Table{}
	for _, t := range schemaTables {
		byName[t.Name] = t
	}
	for _, t := range payCompTables {
		t.cols = map[string]*Column{}
		for i := range t.Columns {
			t.cols[t.Columns[i].Name] = &t.Columns[i]
		}
		tableByName[t.Name] = t
		byName[t.Name] = t
	}
	for _, t := range payCompTables {
		for _, fk := range t.FKs {
			if rt := byName[fk.RefTable]; rt != nil {
				rt.refBy = append(rt.refBy, refFrom{t, fk})
			}
		}
	}
	migrations = append(migrations, migration{"014_pay_components", false, migratePayComp})
	resetHooks = append(resetHooks, func(ctx context.Context, tx Q) error { _, err := SeedPayComponents(ctx, tx); return err })
	return true
}

// migratePayComp is migration 014: the three collections and their indexes, the starting component library, and
// every existing salary structure's equivalent component template (rates.comps, only where it is missing). Nothing
// else is written: no rate, no gross, no mapping, no pay row. Safe on live data and safe to run again.
func migratePayComp(ctx context.Context, s *Store) error {
	for _, t := range payCompTables {
		if err := ensureColl(ctx, s.DB.db, t.Name, t.Indexes); err != nil {
			return err
		}
	}
	if _, err := ConvertLegacyStructures(ctx, s.DB); err != nil {
		return err
	}
	_, err := SeedPayComponents(ctx, s.DB)
	return err
}

// ConvertLegacyStructures writes each salary structure's equivalent component template into rates.comps where the
// structure has none, and returns how many it converted. Only that one field is set.
func ConvertLegacyStructures(ctx context.Context, q Q) (int, error) {
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
		if c, ok := rates["comps"].([]any); ok && len(c) > 0 {
			continue
		}
		b, err := json.Marshal(rates)
		if err != nil {
			return n, err
		}
		var st calc.Structure
		if err := json.Unmarshal(b, &st); err != nil {
			return n, err
		}
		comps, err := toDocs(calc.LegacyComponents(st))
		if err != nil {
			return n, err
		}
		if _, err := q.C("salary_structure").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"rates.comps": comps}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// toDocs turns a value into what JSON gives (maps and slices), as the jsonb rates are stored.
func toDocs(v any) ([]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out []any
	return out, json.Unmarshal(b, &out)
}

// StartingComponents is the library a new installation (and migration 014) starts with.
var StartingComponents = []calc.Component{
	{Code: "basic", Name: "Basic", Kind: calc.CompFixed, Side: calc.SideEarn, Role: calc.RoleBasic, Method: calc.MPctGross, Value: 50, Taxable: true, PF: true, ESI: true, Bonus: true, Payslip: true},
	{Code: "da", Name: "Dearness allowance", Kind: calc.CompFixed, Side: calc.SideEarn, Role: calc.RoleDA, Method: calc.MPctBasic, Value: 0, Taxable: true, PF: true, ESI: true, Bonus: true, Payslip: true},
	{Code: "hra", Name: "HRA", Kind: calc.CompFixed, Side: calc.SideEarn, Role: calc.RoleHRA, Method: calc.MPctBasic, Value: 40, Taxable: true, ESI: true, Payslip: true},
	{Code: "sa", Name: "Special allowance", Kind: calc.CompFixed, Side: calc.SideEarn, Method: calc.MRemainder, Taxable: true, ESI: true, Payslip: true},
	calc.ReimbComponent, // the per-employee Reimbursement (migration 024)
	{Code: "perf", Name: "Performance pay", Kind: calc.CompVariable, Side: calc.SideEarn, Method: calc.MRemainder, Cycle: 3, Taxable: true, Payslip: true},
}

// SeedPayComponents adds the starting components whose code is not in the library yet, and returns how many.
func SeedPayComponents(ctx context.Context, q Q) (int, error) {
	n := 0
	for _, c := range StartingComponents {
		if ok, err := q.C(PayComponentColl).Exists(ctx, bson.M{"code": c.Code}); err != nil || ok {
			if err != nil {
				return n, err
			}
			continue
		}
		doc := bson.M{"code": c.Code, "name": c.Name, "kind": c.Kind, "side": c.Side, "method": c.Method, "value": c.Value, "taxable": c.Taxable,
			"pf": c.PF, "esi": c.ESI, "bonus": c.Bonus, "payslip": c.Payslip, "created_by": "System"}
		if c.Role != "" {
			doc["role"] = c.Role
		}
		if c.Cycle > 0 {
			doc["cycle"] = int32(c.Cycle)
		}
		if _, err := q.C(PayComponentColl).Insert(ctx, doc); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
