package store

// Registry addition for a mid-month gross revision (payroll test ITM-28, user decision 13 Nov 2026):
// gross_history keeps the exact date a gross applies from (from_date, "YYYY-MM-DD"), next to the month key
// (from_month). The month of that date is paid split by days: the earlier gross for the days before the date,
// the new gross from it. Rows written before this change have no from_date and apply from the 1st, as they
// always did; no migration is needed. The PostgreSQL build never had it; an import leaves it null.

var _ = registerGrossDate()

func registerGrossDate() bool {
	for _, t := range schemaTables {
		if t.Name == "gross_history" && t.cols == nil {
			t.Columns = append(t.Columns, Column{Name: "from_date", Type: TDate})
		}
	}
	return true
}
