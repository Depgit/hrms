package store

// Registry additions for salary advances asked for by employees (owner's request of 12 Nov 2026), two settings on
// company_setting kept with the pay schedule (Payroll › Pay schedule and components):
//
//   - adv_max_multiple: the most an employee may ask for, as a multiple of their monthly gross (0: no limit).
//   - adv_one_open: an employee may have only one advance open at a time (waiting for approval, or approved with a
//     balance still to recover).
//
// Both are null until someone saves them; the app then reads the defaults (1 × monthly gross, one at a time), so no
// migration is needed. The PostgreSQL build never had them; an import leaves them null.

// Defaults of the advance settings while company_setting has none.
const (
	DefaultAdvMaxMultiple = 1.0
	DefaultAdvOneOpen     = true
)

var _ = registerAdvance()

func registerAdvance() bool {
	for _, t := range schemaTables {
		if t.Name == "company_setting" && t.cols == nil {
			t.Columns = append(t.Columns,
				Column{Name: "adv_max_multiple", Type: TNumeric, Prec: 5, Scale: 2},
				Column{Name: "adv_one_open", Type: TBool})
			t.Checks = append(t.Checks, Check{Name: "company_setting_adv_max_multiple_check",
				Conds: []Cond{{Col: "adv_max_multiple", Op: ">=", Val: float64(0)}}})
		}
	}
	return true
}
