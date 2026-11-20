package store

// Registry additions for the PF choice of high earners without a UAN and the family on the joining form (owner,
// 19 Nov 2026):
//
//   - employee_pay.pf_choice: "member" (wants PF deducted: the normal structure) or "optout" (no PF: PF exempt and the
//     whole fixed gross paid as Basic). Asked on the joining form only when Existing UAN? is No and the monthly gross is
//     above company_setting.pf_exempt_gross; null for everyone else (the PF rules as before). pf_choice_by,
//     pf_choice_at and pf_choice_reason say who, when and why (the joining form, or HR on the employee's PF card).
//   - employee_pay.all_basic and all_basic_from: the per-employee "all Basic" flag the payroll honours (from the month
//     "YYYY-MM"), and only while the person is PF exempt; set with the "optout" choice. Null: the structure as it is.
//   - employee_personal.spouse: the spouse's name (required on the joining form when Married), for the medical cover.
//   - company_setting.pf_exempt_gross now reads ₹25,000 when the company has not saved one (was ₹20,000).
//
// Migration 021 adds the Form setup rows "Do you want PF deducted from your salary?" (pfChoice) and "Spouse name"
// (spouse), names the UAN field "UAN number" (when it still has its first label), and takes the "Medical insurance
// nomination" drop-down off the forms: there is one plan (MedicalPlan), stored for every new joiner. Nothing else is
// rewritten; every new field is null on existing documents and reads as before.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// PF choices (employee_pay.pf_choice) and the joining form's answers.
const (
	PfChoiceMember = "member"
	PfChoiceOptOut = "optout"
	// MedicalPlan is the only medical insurance plan (owner, 19 Nov 2026).
	MedicalPlan = "Employee + spouse + up to 2 children"
)

var _ = registerPfChoice()

func registerPfChoice() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "employee_pay":
			t.Columns = append(t.Columns,
				Column{Name: "pf_choice", Type: TText},
				Column{Name: "pf_choice_by", Type: TText},
				Column{Name: "pf_choice_at", Type: TTimestamptz},
				Column{Name: "pf_choice_reason", Type: TText},
				Column{Name: "all_basic", Type: TBool},
				Column{Name: "all_basic_from", Type: TText})
			t.Checks = append(t.Checks, Check{Name: "employee_pay_pf_choice_check", Conds: []Cond{{Col: "pf_choice", Op: "in", Val: []any{PfChoiceMember, PfChoiceOptOut}}}})
		case "employee_personal":
			t.Columns = append(t.Columns, Column{Name: "spouse", Type: TText})
		}
	}
	migrations = append(migrations, migration{"021_pf_choice", false, func(ctx context.Context, s *Store) error {
		return s.Tx(ctx, func(tx Q) error { _, err := AddPfChoiceRows(ctx, tx); return err })
	}})
	resetHooks = append(resetHooks, func(ctx context.Context, tx Q) error { _, err := AddPfChoiceRows(ctx, tx); return err })
	return true
}

// AddPfChoiceRows adds the Form setup rows pfChoice and spouse when missing, names the UAN field "UAN number" while
// it is still "UAN", and takes the medical nomination drop-down off the forms (one plan). It returns the rows
// changed; safe to run again.
func AddPfChoiceRows(ctx context.Context, q Q) (int, error) {
	n := 0
	rows := []bson.M{
		// Required when asked: Existing UAN? No and a gross above the threshold (api/onboarding.go onbField.applies).
		{"key": "pfChoice", "label": "Do you want PF deducted from your salary?", "section": "Identity and statutory", "kind": "sel", "options": "Yes,No",
			"on_form": true, "sys": false, "custom": false, "offer_state": int32(0), "join_state": int32(2), "types": []string{"Permanent"},
			"sort": int32(23), "filled_by": FilledByJoiner},
		// Required when Married (api/onboarding.go onbField.applies).
		{"key": "spouse", "label": "Spouse name", "section": "Personal", "kind": "text", "options": nil,
			"on_form": true, "sys": false, "custom": false, "offer_state": int32(0), "join_state": int32(2), "types": []string{"Permanent", "Retainer", "Contractual"},
			"sort": int32(6), "filled_by": FilledByJoiner},
	}
	for _, r := range rows {
		ok, err := q.C("onb_field").Exists(ctx, bson.M{"key": r["key"]})
		if err != nil {
			return n, err
		}
		if ok {
			continue
		}
		if _, err := q.C("onb_field").Insert(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	m, err := q.C("onb_field").UpdateMany(ctx, bson.M{"key": "uan", "label": "UAN"}, bson.M{"$set": bson.M{"label": "UAN number"}})
	if err != nil {
		return n, err
	}
	n += int(m)
	m, err = q.C("onb_field").UpdateMany(ctx, bson.M{"key": "medical", "$or": bson.A{bson.M{"on_form": true}, bson.M{"options": bson.M{"$ne": MedicalPlan}}}},
		bson.M{"$set": bson.M{"on_form": false, "options": MedicalPlan, "join_state": int32(0)}})
	return n + int(m), err
}
