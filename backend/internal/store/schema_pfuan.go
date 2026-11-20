package store

// Registry additions for PF for new joiners and employee salary advances (owner's requests of 14 Nov 2026):
//
//   - employee_pay.pf_exempt*: the PF exemption of a joiner whose monthly gross is above the company's threshold and
//     who has no existing UAN (existing_uan false). pf_exempt is the flag the payroll reads (true: no employee or
//     employer PF, EPS or EDLI, not in the ECR); pf_exempt_reason, pf_exempt_at and pf_exempt_by say why, when and who
//     (System for the automatic decision); pf_exempt_manual is true when Payroll or HR Admin set it by hand, so the
//     automatic decision leaves it alone.
//   - employee_pay.var_start: the month variable pay started for someone who had none before ("YYYY-MM"), set when the
//     variable % goes from nothing to something; the variable payout cycle is counted from it instead of the joining
//     month (owner, 14 Nov 2026: the cycle is each person's own).
//   - company_setting.pf_exempt_gross: the gross above which a joiner with no UAN is PF exempt (null: ₹25,000; ₹20,000 until 19 Nov 2026).
//   - company_setting.uan_hold: hold the salary of a PF member with no UAN on record at the payroll run (null: on).
//   - company_setting.adv_min_months: the months of service an employee needs before asking for an advance (null: 3).
//   - company_setting.adv_max_instalments: the most monthly instalments an advance is recovered in (null: 12).
//
// Every field is new and null on records written before, which then read as before (not exempt) or as the defaults,
// so no migration is needed. The PostgreSQL build never had them; an import leaves them null.

// Defaults while company_setting has none of these settings.
const (
	DefaultPfExemptGross     = 25000.0 // ₹20,000 until 19 Nov 2026 (schema_pfchoice.go)
	DefaultUanHold           = true
	DefaultAdvMinMonths      = 3
	DefaultAdvMaxInstalments = 12
)

var _ = registerPfUan()

func registerPfUan() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "employee_pay":
			t.Columns = append(t.Columns,
				Column{Name: "pf_exempt", Type: TBool},
				Column{Name: "pf_exempt_reason", Type: TText},
				Column{Name: "pf_exempt_at", Type: TTimestamptz},
				Column{Name: "pf_exempt_by", Type: TText},
				Column{Name: "pf_exempt_manual", Type: TBool},
				Column{Name: "var_start", Type: TText})
		case "company_setting":
			t.Columns = append(t.Columns,
				Column{Name: "pf_exempt_gross", Type: TNumeric, Prec: 12, Scale: 2},
				Column{Name: "uan_hold", Type: TBool},
				Column{Name: "adv_min_months", Type: TInt4},
				Column{Name: "adv_max_instalments", Type: TInt4})
			t.Checks = append(t.Checks,
				Check{Name: "company_setting_pf_exempt_gross_check", Conds: []Cond{{Col: "pf_exempt_gross", Op: ">=", Val: float64(0)}}},
				Check{Name: "company_setting_adv_min_months_check", Conds: []Cond{{Col: "adv_min_months", Op: ">=", Val: float64(0)}}},
				Check{Name: "company_setting_adv_max_instalments_check", Conds: []Cond{{Col: "adv_max_instalments", Op: ">=", Val: float64(1)}}})
		}
	}
	return true
}
