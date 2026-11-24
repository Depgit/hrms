package store

// Registry additions for split shifts (owner, 24 Nov 2026):
//
//   - shift.start2_time / shift.end2_time and shift_version.start2_time / shift_version.end2_time: the second part of
//     a split shift, for example 06:00–10:00 + 15:00–19:00 (start_time–end_time is the first part). Both null for a
//     shift in one part, which is every shift that exists today. The gap between the parts is the break and never
//     counts as hours worked (attend.Shift, attend.Eff.Hours). The shift row mirrors the version in effect today.
//
// No migration: the fields are additive and existing documents are valid as they are. The punch rules and the single
// punch rule removed from shifts the same day stay inside the stored rules (jsonb) of older versions and are read
// without them; no live version has any of them set.

var _ = registerSplitShift()

func registerSplitShift() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		if t.Name == "shift" || t.Name == "shift_version" {
			t.Columns = append(t.Columns, Column{Name: "start2_time", Type: TTime}, Column{Name: "end2_time", Type: TTime})
		}
	}
	return true
}
