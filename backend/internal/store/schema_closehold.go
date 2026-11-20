package store

// Registry addition for holding a salary from the payroll checklist (Bank verification closure, 14 Nov 2026): a
// salary hold placed there for someone with no bank account on record holds one payroll month at a time, so
// salary_hold.to_ym is the last month it holds ("YYYY-MM"; null, as on every hold before this change and on holds
// placed in Salary holds, means every month from from_ym on). Holding the next month again moves to_ym on and sets
// extended_at, so a run calculated before that is shown as stale. Both are null on records written before; no
// migration is needed. The PostgreSQL build never had them; an import leaves them null.

var _ = registerCloseHold()

func registerCloseHold() bool {
	for _, t := range schemaTables {
		if t.Name == "salary_hold" && t.cols == nil {
			t.Columns = append(t.Columns,
				Column{Name: "to_ym", Type: TChar, Len: 7},
				Column{Name: "extended_at", Type: TTimestamptz})
		}
	}
	return true
}
