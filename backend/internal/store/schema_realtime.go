package store

// Registry additions for the Realtime India attendance portal connector (16 Nov 2026):
//
//   - employee.attendance_code: the employee's code in the Realtime portal (its EmpCode or CardNo; leading zeros
//     do not matter). Null: matched by the number of the Employee ID instead (EMP-1050 ↔ 1050).
//   - company_setting.realtime_pull: the connector's settings (on, every minutes) and the last run's result. The
//     portal's address and sign-in come from the environment only, never the database.
//   - company_setting.realtime_match: the latest "Match portal codes" result (api/realtime_match.go): when the
//     portal's employee list was fetched, the proposals and the portal employees with no HRMS match.
//
// Both are new and null on existing documents, so no migration rewrites anything.

var _ = registerRealtime()

func registerRealtime() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "employee":
			t.Columns = append(t.Columns, Column{Name: "attendance_code", Type: TText})
		case "company_setting":
			t.Columns = append(t.Columns, Column{Name: "realtime_pull", Type: TJSONB})
			t.Columns = append(t.Columns, Column{Name: "realtime_match", Type: TJSONB})
		}
	}
	return true
}
