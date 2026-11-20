package store

// Registry additions for the owner's requests of 14 Nov 2026 (Builder 1): job role and skill masters, PAN
// verification, children's names, "Existing UAN?", a per-employee medical insurance amount, ESIC and medical cards,
// and opening optional holiday balances.
//
//   - simple_master.kind may also be job_role (an employee's job role, NOT an access role of internal/access) and
//     skill. Both are plain named masters with active/inactive, like qualifications.
//   - employee.job_role and employee.skill: the names of the employee's job role and skill (null: not given, as for
//     everyone hired before; required for new joiners). employee.existing_uan: the joiner said they already have a
//     UAN (true), or not (false); null means unknown.
//   - employee_personal.child1 and child2: the names of the first and second child (optional).
//   - employee_gov_id.pan_status, pan_name, pan_checked_at, pan_checked_by, pan_checked: the last PAN check, the name
//     the provider returned, when and by whom, and the PAN that was checked. A status is shown only while the PAN on
//     record is still the one checked; any other PAN is "Not checked".
//   - offer.pan_check: the same, for a PAN entered on the joining form before there is an employee record; it is
//     copied to the record with the PAN.
//   - employee_pay.medical: the employee's monthly medical insurance (₹), replacing the structure's rate and cap
//     (null: the structure's rule). It is an employer cost, not a deduction.
//   - company_setting.pan_verify: the PAN verification provider, its base URL, on/off and the API key sealed with the
//     app's secret key (base64 of AES-GCM, as the Drive token).
//   - opt_hol_opening: opening optional holiday balances by employee and "as at" date (new collection).
//   - onb_field: Role (jobRole) and Skill (jobSkill), required at offer like the department; Father or spouse name,
//     1st and 2nd child name (optional, filled by the joiner). onb_document: ESIC card and Medical card (not asked
//     on the forms; loaded by Bulk update › ESIC and medical cards).
//
// Every field is new and null on existing documents, so nothing is rewritten. Migration 009 creates opt_hol_opening
// and inserts the missing Form setup rows; it never changes a row that is there.

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// OptHolOpeningColl holds opening optional holiday balances.
const OptHolOpeningColl = "opt_hol_opening"

// Simple master kinds of the job role and skill masters.
const (
	KindJobRole = "job_role"
	KindSkill   = "skill"
)

var optHolOpeningTable = &Table{
	Name: OptHolOpeningColl,
	Columns: []Column{
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "at", Type: TDate, NotNull: true},
		{Name: "balance", Type: TNumeric, NotNull: true, Prec: 6, Scale: 2},
		{Name: "created_by", Type: TText},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK: []string{"employee_id", "at"},
	Indexes: []Index{
		{Name: "opt_hol_opening_pkey", Keys: []IndexKey{{Field: "employee_id"}, {Field: "at"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "opt_hol_opening_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
}

var _ = registerB1()

func registerB1() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "simple_master":
			for i := range t.Checks {
				if t.Checks[i].Name != "simple_master_kind_check" {
					continue
				}
				for j := range t.Checks[i].Conds {
					if c := &t.Checks[i].Conds[j]; c.Col == "kind" && c.Op == "in" {
						if l, ok := c.Val.([]any); ok {
							c.Val = append(l, KindJobRole, KindSkill)
						}
					}
				}
			}
		case "employee":
			t.Columns = append(t.Columns, Column{Name: "job_role", Type: TText}, Column{Name: "skill", Type: TText}, Column{Name: "existing_uan", Type: TBool})
		case "employee_personal":
			t.Columns = append(t.Columns, Column{Name: "child1", Type: TText}, Column{Name: "child2", Type: TText})
		case "employee_gov_id":
			t.Columns = append(t.Columns, Column{Name: "pan_status", Type: TText}, Column{Name: "pan_name", Type: TText},
				Column{Name: "pan_checked_at", Type: TTimestamptz}, Column{Name: "pan_checked_by", Type: TText}, Column{Name: "pan_checked", Type: TText})
		case "employee_pay":
			t.Columns = append(t.Columns, Column{Name: "medical", Type: TNumeric, Prec: 10, Scale: 2})
		case "offer":
			t.Columns = append(t.Columns, Column{Name: "pan_check", Type: TJSONB})
		case "company_setting":
			t.Columns = append(t.Columns, Column{Name: "pan_verify", Type: TJSONB})
		}
	}
	t := optHolOpeningTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if emp := tableByName["employee"]; emp != nil {
		emp.refBy = append(emp.refBy, refFrom{t, t.FKs[0]})
	}
	migrations = append(migrations, migration{"009_role_skill_cards", true, migrateB1})
	resetHooks = append(resetHooks, func(ctx context.Context, tx Q) error { _, err := AddB1Rows(ctx, tx); return err })
	return true
}

// resetHooks run inside Reset after the reference data (Form setup rows other migrations add).
var resetHooks []func(ctx context.Context, tx Q) error

// b1Field is a Form setup field this change adds.
type b1Field struct {
	Key, Label, Section, Kind string
	Offer, Join, Sort         int32
	Sys                       bool
	FilledBy                  string
}

// B1Fields are the Form setup fields added on 14 Nov 2026. Role and Skill are asked on the offer and required, like
// the department, and offer the active master names; the family names are optional and filled by the joiner. The
// sort places Father or spouse name before Marital status (5), the children after it, Role after Designation (43).
var B1Fields = []b1Field{
	{Key: "jobRole", Label: "Role", Section: "Job", Kind: "sel", Offer: 2, Join: 0, Sort: 43, Sys: true, FilledBy: FilledByHR},
	{Key: "jobSkill", Label: "Skill", Section: "Job", Kind: "sel", Offer: 2, Join: 0, Sort: 46, Sys: true, FilledBy: FilledByHR},
	{Key: "fatherSpouse", Label: "Father or spouse name", Section: "Personal", Kind: "text", Offer: 0, Join: 1, Sort: 5, FilledBy: FilledByJoiner},
	{Key: "child1", Label: "1st child name", Section: "Personal", Kind: "text", Offer: 0, Join: 1, Sort: 6, FilledBy: FilledByJoiner},
	{Key: "child2", Label: "2nd child name", Section: "Personal", Kind: "text", Offer: 0, Join: 1, Sort: 6, FilledBy: FilledByJoiner},
}

// Card document keys (onb_document): loaded by Bulk update › ESIC and medical cards.
const (
	DocESICCard    = "esicCard"
	DocMedicalCard = "medicalCard"
)

// AddB1Rows inserts the Form setup fields and the card document types that are missing. A row already there is
// left exactly as it is. It returns how many rows it added.
func AddB1Rows(ctx context.Context, q Q) (int, error) {
	n := 0
	for _, f := range B1Fields {
		if ok, err := q.C("onb_field").Exists(ctx, bson.M{"key": f.Key}); err != nil || ok {
			if err != nil {
				return n, err
			}
			continue
		}
		if _, err := q.C("onb_field").Insert(ctx, bson.M{"key": f.Key, "label": f.Label, "section": f.Section, "kind": f.Kind, "options": nil,
			"on_form": true, "sys": f.Sys, "custom": false, "offer_state": f.Offer, "join_state": f.Join,
			"types": []string{"Permanent", "Retainer", "Contractual"}, "sort": f.Sort, "filled_by": f.FilledBy}); err != nil {
			return n, err
		}
		n++
	}
	for i, d := range [][2]string{{DocESICCard, "ESIC card"}, {DocMedicalCard, "Medical card"}} {
		if ok, err := q.C("onb_document").Exists(ctx, bson.M{"key": d[0]}); err != nil || ok {
			if err != nil {
				return n, err
			}
			continue
		}
		// Not asked on the offer or joining form (on_form false): the cards come after joining.
		if _, err := q.C("onb_document").Insert(ctx, bson.M{"key": d[0], "label": d[1], "on_form": false, "sys": false, "offer_state": int32(0),
			"join_state": int32(0), "types": []string{"Permanent", "Retainer", "Contractual"}, "depts": []string{}, "sort": int32(90 + i),
			"filled_by": FilledByJoiner}); err != nil { // every document is the joiner's by default; on_form false keeps it off the forms
			return n, err
		}
		n++
	}
	return n, nil
}

// ensureOptHolOpening creates opt_hol_opening and its index when missing. It never touches a document.
func ensureOptHolOpening(ctx context.Context, db *mongo.Database) error {
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: OptHolOpeningColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, OptHolOpeningColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", OptHolOpeningColl, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, OptHolOpeningColl, optHolOpeningTable.Indexes)
}

// migrateB1 is migration 009: the opt_hol_opening collection, and the Form setup fields and card document types
// that are missing. Additive only.
func migrateB1(ctx context.Context, s *Store) error {
	if err := ensureOptHolOpening(ctx, s.DB.db); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		_, err := AddB1Rows(ctx, tx)
		return err
	})
}

// CardDocKey says whether a document key is one of the cards (ESIC card, Medical card).
func CardDocKey(k string) bool { return k == DocESICCard || k == DocMedicalCard }
