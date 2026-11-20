package store

// Registry additions for opening uniform (owner's request of 13 Nov 2026: Bulk update › Opening uniform). Uniform
// issued to existing employees before the HRMS, whose recovery may have started under the old system, is recorded as
// a uniform_issue marked opening, with no store and no stock movement:
//
//   - opening: true for such an issue (null on every other issue).
//   - issued_ym: the month it was issued ("YYYY-MM"); issued_on is the 1st of that month.
//   - instalments: the instalments asked for in the file (null: Setup's split).
//   - note: the items or a note, as given in the file.
//   - before_hrms: the instalments before the HRMS's first deduction month, recovered outside the HRMS
//     ([{ym, amount}]); they have no pay line.
//   - first_ym: the HRMS's first deduction month when it was loaded.
//
// store_id may be null, for an opening issue only. The fields are new and null on every issue made before, so no
// migration is needed. The PostgreSQL build never had them; an import leaves them null.

var _ = registerUniformOpening()

func registerUniformOpening() bool {
	for _, t := range schemaTables {
		if t.Name != "uniform_issue" || t.cols != nil {
			continue
		}
		for i := range t.Columns {
			if t.Columns[i].Name == "store_id" {
				t.Columns[i].NotNull = false
			}
		}
		t.Columns = append(t.Columns,
			Column{Name: "opening", Type: TBool},
			Column{Name: "issued_ym", Type: TText},
			Column{Name: "instalments", Type: TInt4},
			Column{Name: "note", Type: TText},
			Column{Name: "before_hrms", Type: TJSONB},
			Column{Name: "first_ym", Type: TText})
	}
	return true
}
