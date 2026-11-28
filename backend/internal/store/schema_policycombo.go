package store

// Registry additions for work policies and holiday calendars (owner, 24 Nov 2026):
//
//   - work_policy_map.level may also be a department together with one place: dept_location, dept_cluster,
//     dept_city or dept_state. The ref of such a mapping is "<department name>|<place id>", for example
//     "People Ops|<id of Gurgaon office>". A person matches when both their department and their place do. Combined
//     mappings rank after an employee mapping and before every single level (api/timectx.go, policyLevels).
//   - holiday_calendar.year: the year the calendar's holidays are in (a holiday edited to another year is refused).
//     Null on existing calendars: their year is read from the name ("NCR 2026"), else from their holidays when they
//     all fall in one year (api/policies.go, calYear).
//
// No migration: both are additive and existing documents are valid as they are.

var _ = registerPolicyCombo()

// ComboLevels are the work policy mapping levels of a department together with one place, the most specific first.
var ComboLevels = []string{"dept_location", "dept_cluster", "dept_city", "dept_state"}

func registerPolicyCombo() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "work_policy_map":
			for i := range t.Checks {
				if t.Checks[i].Name != "work_policy_map_level_check" {
					continue
				}
				for j := range t.Checks[i].Conds {
					if c := &t.Checks[i].Conds[j]; c.Col == "level" && c.Op == "in" {
						vals := append([]any{}, c.Val.([]any)...)
						for _, l := range ComboLevels {
							vals = append(vals, l)
						}
						c.Val = vals
					}
				}
				t.Checks[i].Expr = "CHECK (level IN ('employee','location','cluster','department','designation','grade','type','city','state'," +
					"'dept_location','dept_cluster','dept_city','dept_state'))"
			}
		case "holiday_calendar":
			t.Columns = append(t.Columns, Column{Name: "year", Type: TInt4})
		}
	}
	return true
}
