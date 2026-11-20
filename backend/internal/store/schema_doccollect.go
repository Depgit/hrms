package store

// Registry additions for collecting documents from existing employees (owner's request of 12 Nov 2026: an employee
// who did not come through the joining flow, imported or added directly, may upload their own documents within a
// window, 90 days by default, so the company has proper documentation for them):
//
//   - doc_window: one document per employee given a window: the day it started (start), how it came (source:
//     "startup" for everyone on file when the feature first ran, "import" for an HR Admin import, "added" for
//     anyone the sweep finds later), the days HR added (extra_days) and the reminders already sent (reminders,
//     each key once, so a reminder is never sent twice).
//   - document.review_*: HR's check of an uploaded document: review_state "verified" or "returned" (sent back,
//     with review_note as the reason), who and when. A new upload is a new document row, so it starts unchecked.
//   - onb_setting.doc_window_days and doc_window_on: the window's length and whether the collection runs at all.
//     Null until HR Admin saves them; the app then reads the defaults (90 days, on).
//
// doc_window never existed in PostgreSQL, so it is kept out of schemaTables like joining_link: Tables() (and with it
// `hrms import-pg`) does not list it, while C() still applies its rules. Migration 007 only creates it with its
// indexes; it never rewrites an existing document. The windows themselves are opened by the API's sweep
// (internal/api/doc_collect.go), which runs on start and every 15 minutes.

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// DocWindowColl holds the document collection windows.
const DocWindowColl = "doc_window"

// Defaults of the document collection while onb_setting has none.
const (
	DefaultDocWindowDays = 90
	DefaultDocWindowOn   = true
)

// Review states of a document.
const (
	DocVerified = "verified"
	DocReturned = "returned"
)

var docWindowTable = &Table{
	Name: DocWindowColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "start", Type: TDate, NotNull: true},
		{Name: "source", Type: TText, NotNull: true},
		{Name: "extra_days", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
		{Name: "reminders", Type: TText, Array: true, NotNull: true, Default: Default{Kind: DefConst, Value: []string{}}},
		{Name: "reminded_at", Type: TTimestamptz}, // last reminder HR sent by hand
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "updated_at", Type: TTimestamptz},
		{Name: "updated_by", Type: TText},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "doc_window_employee_key", Keys: []IndexKey{{Field: "employee_id"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "doc_window_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "doc_window_source_check", Conds: []Cond{{Col: "source", Op: "in", Val: []any{"startup", "import", "added"}}}},
		{Name: "doc_window_extra_days_check", Conds: []Cond{{Col: "extra_days", Op: ">=", Val: float64(0)}}},
	},
}

var _ = registerDocCollect()

func registerDocCollect() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "document":
			t.Columns = append(t.Columns,
				Column{Name: "review_state", Type: TText},
				Column{Name: "review_note", Type: TText},
				Column{Name: "review_by", Type: TText},
				Column{Name: "review_at", Type: TTimestamptz})
			t.Checks = append(t.Checks, Check{Name: "document_review_state_check",
				Conds: []Cond{{Col: "review_state", Op: "in", Val: []any{DocVerified, DocReturned}}}})
		case "onb_setting":
			t.Columns = append(t.Columns,
				Column{Name: "doc_window_days", Type: TInt4},
				Column{Name: "doc_window_on", Type: TBool})
			t.Checks = append(t.Checks, Check{Name: "onb_setting_doc_window_days_check",
				Conds: []Cond{{Col: "doc_window_days", Op: ">=", Val: float64(1)}, {Col: "doc_window_days", Op: "<=", Val: float64(365)}}})
		}
	}
	t := docWindowTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if emp := tableByName["employee"]; emp != nil {
		emp.refBy = append(emp.refBy, refFrom{t, t.FKs[0]})
	}
	return true
}

// ensureDocWindow creates doc_window and its indexes when missing. It never touches a document.
func ensureDocWindow(ctx context.Context, db *mongo.Database) error {
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: DocWindowColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, DocWindowColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", DocWindowColl, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, DocWindowColl, docWindowTable.Indexes)
}

// migrateDocCollect is migration 007: the doc_window collection with its unique index. The new fields of document
// and onb_setting need nothing: they are read as "not set" until written.
func migrateDocCollect(ctx context.Context, s *Store) error { return ensureDocWindow(ctx, s.DB.db) }

// healDocCollect makes sure doc_window exists on every start (a database restored from before it, or an import
// with -replace). Cheap when it is there.
func healDocCollect(ctx context.Context, s *Store) error { return ensureDocWindow(ctx, s.DB.db) }
