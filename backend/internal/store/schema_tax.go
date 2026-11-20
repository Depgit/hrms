package store

// Registry additions for the payroll test fixes (13 Nov 2026):
//
//   - tds_history.source, by_name, at: "opening" marks salary TDS deducted before go-live and loaded from a file
//     (Tax and Form 16 › Opening TDS); rows written by payroll have no source.
//   - tax_window.reopened_at, reopened_by, reopen_reason: the last time the Payroll Manager reopened a finalisation.
//
// All are null on existing documents and nothing reads them as required, so no migration is needed.

var _ = registerTax()

func registerTax() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "tds_history":
			t.Columns = append(t.Columns,
				Column{Name: "source", Type: TText},
				Column{Name: "by_name", Type: TText},
				Column{Name: "at", Type: TTimestamptz})
		case "tax_window":
			t.Columns = append(t.Columns,
				Column{Name: "reopened_at", Type: TTimestamptz},
				Column{Name: "reopened_by", Type: TText},
				Column{Name: "reopen_reason", Type: TText})
		}
	}
	return true
}
