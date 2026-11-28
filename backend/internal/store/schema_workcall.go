package store

// Called to work on a holiday or a leave day (owner, 24 Nov 2026):
//
//   - work_call (new): one document per employee and date HR calls them to work: kind "holiday" (a holiday on their
//     calendar) or "leave" (a day of their approved leave: the request, its type and the days of the date, 1 or 0.5),
//     the reason, who called and when. noted_at is when the leave request's history got the "worked" line (set once,
//     by the attendance sweep). When the person works the day it is Present and the leave of the date is returned in
//     every balance (api/work_call.go, attend/called.go); nothing else is stored.
//
// The deviation allowances of a work policy (owner, same day) live in work_policy.settings.dev (jsonb): no new field.
//
// work_call never existed in PostgreSQL, so it is kept out of schemaTables (import-pg does not list it) while C()
// applies its rules. Migration 027 creates it and its index. Additive only: nothing existing is rewritten.

import (
	"context"
)

// WorkCallColl holds the days employees are called to work on a holiday or leave day.
const WorkCallColl = "work_call"

var workCallTable = &Table{
	Name: WorkCallColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "date", Type: TDate, NotNull: true},
		{Name: "kind", Type: TText, NotNull: true},
		{Name: "leave_request_id", Type: TText},
		{Name: "leave_type", Type: TText},
		{Name: "days", Type: TNumeric},
		{Name: "reason", Type: TText, NotNull: true},
		{Name: "by_id", Type: TText},
		{Name: "by_name", Type: TText, NotNull: true},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "noted_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "work_call_emp_date_key", Keys: []IndexKey{{Field: "employee_id"}, {Field: "date"}}, Unique: true},
		{Name: "work_call_date_idx", Keys: []IndexKey{{Field: "date"}}},
	},
	FKs: []FK{
		{Name: "work_call_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "work_call_kind_check", Conds: []Cond{{Col: "kind", Op: "in", Val: []any{"holiday", "leave"}}}},
	},
}

var _ = registerWorkCall()

func registerWorkCall() bool {
	t := workCallTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if emp := tableByName["employee"]; emp != nil { // an Employee ID re-key carries the calls over
		emp.refBy = append(emp.refBy, refFrom{t, t.FKs[0]})
	}
	migrations = append(migrations, migration{"027_work_call", false, func(ctx context.Context, s *Store) error {
		return ensureColl(ctx, s.DB.db, WorkCallColl, workCallTable.Indexes)
	}})
	return true
}
