package store

// Individual roster changes (owner, 24 Nov 2026: "individual roster change allow this as well"):
//
//   - roster_entry.start_time / end_time / start2_time / end2_time: "Other time for this day", a one-day timing set in
//     a roster cell without a shift master (code OT). start–end is the first part, start2–end2 an optional second
//     part (a split day, same checks as a split shift). All four null on every other cell, which is every cell that
//     exists today.
//   - roster_log.from_time / to_time: the timings of an OT cell a change went from or to ("10:00–14:00 + 17:00–21:00"),
//     null otherwise.
//   - work_call.shift_code: the shift a call to work made from the roster is measured against (hours, late mark, the
//     punch window). Null for a call made on Attendance › Called to work, which is measured as before (the roster's
//     shift of the day, else the employee's own).
//   - emp_dev_allow (new): one employee's own deviation allowances (Early check-in, Late check-in, Early check-out),
//     each "use the policy's rule", On with its own minutes and times, or Off, as versions by effective_from. The
//     engine uses the version in effect on each date over the work policy's rules.
//
// Migration 028 creates emp_dev_allow and its index. Additive only: nothing existing is rewritten; the new fields are
// null on existing documents.

import "context"

// EmpDevAllowColl holds the employees' own deviation allowances, by version.
const EmpDevAllowColl = "emp_dev_allow"

var empDevAllowTable = &Table{
	Name: EmpDevAllowColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "effective_from", Type: TDate, NotNull: true},
		{Name: "rules", Type: TJSONB, NotNull: true},
		{Name: "note", Type: TText},
		{Name: "by_id", Type: TText},
		{Name: "by_name", Type: TText, NotNull: true},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "emp_dev_allow_emp_from_key", Keys: []IndexKey{{Field: "employee_id"}, {Field: "effective_from"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "emp_dev_allow_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
}

var _ = registerRosterDay()

func registerRosterDay() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "roster_entry":
			t.Columns = append(t.Columns, Column{Name: "start_time", Type: TTime}, Column{Name: "end_time", Type: TTime},
				Column{Name: "start2_time", Type: TTime}, Column{Name: "end2_time", Type: TTime})
		case "roster_log":
			t.Columns = append(t.Columns, Column{Name: "from_time", Type: TText}, Column{Name: "to_time", Type: TText})
		}
	}
	// work_call is registered by its own file; its columns are mapped again so the order of the files does not matter.
	wc := workCallTable
	wc.Columns = append(wc.Columns, Column{Name: "shift_code", Type: TText})
	if wc.cols != nil {
		wc.cols = map[string]*Column{}
		for i := range wc.Columns {
			wc.cols[wc.Columns[i].Name] = &wc.Columns[i]
		}
	}
	t := empDevAllowTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if emp := tableByName["employee"]; emp != nil { // an Employee ID re-key carries the rules over
		emp.refBy = append(emp.refBy, refFrom{t, t.FKs[0]})
	}
	migrations = append(migrations, migration{"028_roster_day", false, func(ctx context.Context, s *Store) error {
		return ensureColl(ctx, s.DB.db, EmpDevAllowColl, empDevAllowTable.Indexes)
	}})
	return true
}
