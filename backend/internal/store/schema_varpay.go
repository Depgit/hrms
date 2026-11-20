package store

// Registry addition for the variable pay effective month (payroll test RUN-31): employee_pay keeps the month the
// current variable pay % applies from (var_from, "YYYY-MM") and the % in force before it (var_prev_pct; null = the
// salary structure's). Months before var_from accrue at the earlier %, so a change does not reach back into the
// variable cycle. Both are null on records written before this change, which then apply as they always did; no
// migration is needed. The PostgreSQL build never had them; an import leaves them null.

var _ = registerVarPay()

func registerVarPay() bool {
	for _, t := range schemaTables {
		if t.Name == "employee_pay" && t.cols == nil {
			t.Columns = append(t.Columns,
				Column{Name: "var_from", Type: TText},
				Column{Name: "var_prev_pct", Type: TNumeric, Prec: 5, Scale: 2})
		}
	}
	return true
}
