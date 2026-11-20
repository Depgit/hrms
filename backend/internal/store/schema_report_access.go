package store

// Which report each role sees (test sheet #21, 18 Nov 2026):
//
//   - role_report (new): one document per role and report, `on` true when the role's people see that report in
//     Reports (list, run and download). A report with no document is on, so a report or a role added later keeps
//     the behaviour so far: every report the role's rights allow. HR Admin always sees every report and has none.
//     A report also still needs the rights it always needed (Reports right, payroll reports the payroll right, …).
//
// role_report never existed in PostgreSQL, so it is kept out of schemaTables (import-pg does not list it) while C()
// applies its rules. Migration 020 creates it and its index and writes a ticked document for every role and every
// report (ReportIDs, set by the api package): the behaviour so far. Nothing existing is rewritten.

import (
	"context"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// RoleReportColl holds the report ticks of each role.
const RoleReportColl = "role_report"

// ReportIDs lists every report id (set by the api package, which owns the report list); migration 020 ticks each
// for every role. Nil: the migration only creates the collection (a missing tick reads as on).
var ReportIDs func() []string

var roleReportTable = &Table{
	Name: RoleReportColl,
	Columns: []Column{
		{Name: "role_name", Type: TText, NotNull: true},
		{Name: "report_id", Type: TText, NotNull: true},
		{Name: "on", Type: TBool, NotNull: true},
		{Name: "updated_by", Type: TText},
		{Name: "updated_at", Type: TTimestamptz},
	},
	PK: []string{"role_name", "report_id"},
	Indexes: []Index{
		{Name: "role_report_pkey", Keys: []IndexKey{{Field: "role_name"}, {Field: "report_id"}}, Unique: true},
	},
}

var _ = registerReportAccess()

func registerReportAccess() bool {
	t := roleReportTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	migrations = append(migrations, migration{"020_report_access", false, migrateReportAccess})
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
	return true
}

// migrateReportAccess is migration 020: the role_report collection and its index, and a tick for every role (but
// HR Admin) and every report. Safe to run again: a document already there is left as it is.
func migrateReportAccess(ctx context.Context, s *Store) error {
	if err := ensureColl(ctx, s.DB.db, RoleReportColl, roleReportTable.Indexes); err != nil {
		return err
	}
	if ReportIDs == nil {
		return nil
	}
	ids := ReportIDs()
	return s.Tx(ctx, func(tx Q) error {
		roles, err := tx.C("role").Find(ctx, bson.M{})
		if err != nil {
			return err
		}
		now := time.Now()
		for _, r := range roles {
			name, _ := r["name"].(string)
			if name == "" || name == "HR Admin" {
				continue
			}
			for _, id := range ids {
				if _, err := tx.C(RoleReportColl).Upsert(ctx, bson.M{"role_name": name, "report_id": id}, nil,
					bson.M{"on": true, "updated_by": "migration 020", "updated_at": now}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
