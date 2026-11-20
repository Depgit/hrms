package store

// Registry additions for attendance alerts (owner, 16 Nov 2026: tell employees and managers about missing punches,
// late arrivals and no-shows from the punches the HRMS receives):
//
//   - company_setting.att_alerts: which alerts are on and their times (all off until HR turns them on).
//   - company_setting.att_alerts_last: the last sweep's result (when, what was sent today, why anything was skipped).
//   - company_setting.att_alerts_lease: until when a sweep holds the run, so two servers never sweep at once.
//
// The messages themselves are bell notices (kind "attendance"); each carries a key in notice.ref
// ("att-alert:<type>:<date>:<employee>") that marks it sent, so an alert goes out once per person, type and day.
// All three fields are new and read as "not set" on existing documents: no migration rewrites anything.

var _ = registerAttAlerts()

func registerAttAlerts() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "company_setting" {
			t.Columns = append(t.Columns,
				Column{Name: "att_alerts", Type: TJSONB},
				Column{Name: "att_alerts_last", Type: TJSONB},
				Column{Name: "att_alerts_lease", Type: TTimestamptz})
		}
	}
	return true
}
