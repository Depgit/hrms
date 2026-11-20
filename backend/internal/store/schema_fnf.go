package store

// Registry addition for the full and final (payroll test EXT-42): fnf.receivable, the amount a leaver owes when the
// settlement nets below zero ({amount, status: Open | Recovered | Waived, note, by_name, at}). Such a settlement is
// not paid through payroll; Payroll closes it as recovered or waived. Null on every other settlement, so no
// migration is needed; the PostgreSQL build never had it.

var _ = registerFnf()

func registerFnf() bool {
	for _, t := range schemaTables {
		if t.Name == "fnf" && t.cols == nil {
			t.Columns = append(t.Columns, Column{Name: "receivable", Type: TJSONB})
		}
	}
	return true
}
