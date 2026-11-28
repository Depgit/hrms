package store

// Registry addition for the all-inclusive gross (owner, 26 Nov 2026; calc/allin.go, api/allin.go):
//
//   - company_setting.allin_from: "YYYY-MM", the first payroll month in which the monthly gross offered (G) also pays
//     the employer's PF (with EPS), ESI, EPFO admin + EDLI and LWF, with the statutory bonus inside G when G is at most
//     the bonus ceiling, and variable pay is paid every month by the days paid. Empty or missing: off (no default).
//     Set on Compensation by someone with pay.setup at Edit; audited. A change that would alter an approved or
//     published month is refused.
//
// New and read as "off" on existing documents: no migration.

var _ = registerAllIn()

func registerAllIn() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "company_setting" {
			t.Columns = append(t.Columns, Column{Name: "allin_from", Type: TText})
		}
	}
	return true
}
