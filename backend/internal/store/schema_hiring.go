package store

// Registry additions for the hiring lifecycle (workforce plan, module 1, decisions of 16 Nov 2026):
//
//   - candidate: offer_no (the Offer ID OI-…, issued only when HR records the acceptance), offer_version, waiting
//     (the no-show / stopped-coming window: kind, since, days, extensions, call log), bgv (the background check),
//     ji_at (the Joining ID record was made at joining: the new flow) and approved_at (the Employee ID was issued).
//     New closed kinds: Offer lapsed, Withdrew before joining, No-show, Declined on joining day, Early exit.
//   - offer: offer_no, version, expires_on, query_at / query_text (the candidate's question about the breakup),
//     lapsed_at.
//   - employee: stage (Joining, Probation, Confirmed, On notice, Suspended, Long leave, Absconding; status stays
//     Pre-boarding/Active/Inactive), stage_since, alias_ids (the Joining ID JI-… after the record moved to its
//     Employee ID, so lookups by the old ID still resolve).
//   - employee_code_settings.no_emp_id: employment types that never get an Employee ID (contract staff keep their
//     own series' number from joining).
//   - onb_setting: wait_days, wait_auto_close, bgv_blocks, offer_days, offer_remind_days.
//   - offer_version: every version of a candidate's offer (v1, v2…); the accepted one is frozen.
//   - lifecycle_event: the status timeline of a candidate and of an employee (who, when, from → to, reason).
//
// offer_version and lifecycle_event never existed in PostgreSQL, so, like joining_link, they are kept out of
// schemaTables (import-pg does not list them) while C() applies their rules.

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	OfferVersionColl   = "offer_version"
	LifecycleEventColl = "lifecycle_event"
)

// EmployeeStages are the employee lifecycle stages (employee.stage). Joining is set by the system at joining.
var EmployeeStages = []string{"Joining", "Probation", "Confirmed", "On notice", "Suspended", "Long leave", "Absconding"}

// HireClosedKinds are the closed kinds the hiring lifecycle adds to the candidate.
var HireClosedKinds = []string{"Offer lapsed", "Withdrew before joining", "No-show", "Declined on joining day", "Early exit"}

var offerVersionTable = &Table{
	Name: OfferVersionColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "candidate_id", Type: TText, NotNull: true},
		{Name: "offer_id", Type: TText, NotNull: true},
		{Name: "version", Type: TInt4, NotNull: true},
		{Name: "type", Type: TText, NotNull: true},
		{Name: "doj", Type: TDate, NotNull: true},
		{Name: "grade", Type: TText},
		{Name: "gross", Type: TNumeric, NotNull: true, Prec: 12, Scale: 2},
		{Name: "data", Type: TJSONB, NotNull: true, Default: Default{Kind: DefConst, Value: "{}"}},
		// status: Pending approval, Not approved, Sent, Question, Revised, Accepted, Declined, Lapsed.
		{Name: "status", Type: TText, NotNull: true},
		{Name: "answer", Type: TText}, // the question asked, or why it was declined
		{Name: "answered_at", Type: TTimestamptz},
		{Name: "answered_by", Type: TText},
		{Name: "released_at", Type: TTimestamptz},
		{Name: "expires_on", Type: TDate},
		{Name: "frozen", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
		{Name: "created_by", Type: TText, NotNull: true},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "offer_version_candidate_key", Keys: []IndexKey{{Field: "candidate_id"}, {Field: "version"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "offer_version_candidate_id_fkey", Col: "candidate_id", RefTable: "candidate", RefCol: "id"},
		{Name: "offer_version_offer_id_fkey", Col: "offer_id", RefTable: "offer", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "offer_version_status_check", Conds: []Cond{{Col: "status", Op: "in", Val: []any{"Pending approval", "Not approved", "Sent", "Question",
			"Revised", "Accepted", "Declined", "Lapsed"}}}},
	},
}

var lifecycleEventTable = &Table{
	Name: LifecycleEventColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "subject", Type: TText, NotNull: true}, // candidate | employee
		{Name: "candidate_id", Type: TText},
		{Name: "employee_id", Type: TText},
		{Name: "action", Type: TText, NotNull: true},
		{Name: "from_state", Type: TText},
		{Name: "to_state", Type: TText},
		{Name: "reason", Type: TText},
		{Name: "on_date", Type: TDate},
		{Name: "by_name", Type: TText, NotNull: true},
		{Name: "by_id", Type: TText},
		{Name: "at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "lifecycle_event_candidate_idx", Keys: []IndexKey{{Field: "candidate_id"}, {Field: "at"}}},
		{Name: "lifecycle_event_employee_idx", Keys: []IndexKey{{Field: "employee_id"}, {Field: "at"}}},
	},
	FKs: []FK{
		{Name: "lifecycle_event_candidate_id_fkey", Col: "candidate_id", RefTable: "candidate", RefCol: "id"},
		{Name: "lifecycle_event_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "lifecycle_event_subject_check", Conds: []Cond{{Col: "subject", Op: "in", Val: []any{"candidate", "employee"}}}},
	},
}

// employeeAliasIndex finds a record by its old Joining ID.
var employeeAliasIndex = Index{Name: "employee_alias_idx", Keys: []IndexKey{{Field: "alias_ids"}}}

var _ = registerHiring()

func registerHiring() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "candidate":
			t.Columns = append(t.Columns, Column{Name: "offer_no", Type: TText}, Column{Name: "offer_version", Type: TInt4},
				Column{Name: "waiting", Type: TJSONB}, Column{Name: "bgv", Type: TJSONB}, Column{Name: "ji_at", Type: TTimestamptz},
				Column{Name: "approved_at", Type: TTimestamptz})
			for i := range t.Checks {
				if t.Checks[i].Name != "candidate_closed_kind_check" {
					continue
				}
				for j := range t.Checks[i].Conds {
					if c := &t.Checks[i].Conds[j]; c.Col == "closed_kind" && c.Op == "in" {
						if l, ok := c.Val.([]any); ok {
							for _, k := range HireClosedKinds {
								l = append(l, k)
							}
							c.Val = l
						}
					}
				}
			}
		case "offer":
			t.Columns = append(t.Columns, Column{Name: "offer_no", Type: TText}, Column{Name: "version", Type: TInt4}, Column{Name: "expires_on", Type: TDate},
				Column{Name: "query_at", Type: TTimestamptz}, Column{Name: "query_text", Type: TText}, Column{Name: "lapsed_at", Type: TTimestamptz})
		case "employee":
			t.Columns = append(t.Columns, Column{Name: "stage", Type: TText}, Column{Name: "stage_since", Type: TDate},
				Column{Name: "alias_ids", Type: TText, Array: true})
			stages := make([]any, len(EmployeeStages))
			for i, s := range EmployeeStages {
				stages[i] = s
			}
			t.Checks = append(t.Checks, Check{Name: "employee_stage_check", Conds: []Cond{{Col: "stage", Op: "in", Val: stages}}})
		case "employee_code_settings":
			t.Columns = append(t.Columns, Column{Name: "no_emp_id", Type: TText, Array: true})
		case "onb_setting":
			t.Columns = append(t.Columns, Column{Name: "wait_days", Type: TInt4}, Column{Name: "wait_auto_close", Type: TBool},
				Column{Name: "bgv_blocks", Type: TBool}, Column{Name: "bgv_off", Type: TBool}, Column{Name: "offer_days", Type: TInt4}, Column{Name: "offer_remind_days", Type: TInt4})
		}
	}
	for _, t := range []*Table{offerVersionTable, lifecycleEventTable} {
		t.cols = map[string]*Column{}
		for i := range t.Columns {
			t.cols[t.Columns[i].Name] = &t.Columns[i]
		}
		tableByName[t.Name] = t
	}
	// The registry's tables by name: tableByName is filled for them only by schema.go's init, after this runs.
	byName := map[string]*Table{}
	for _, t := range schemaTables {
		byName[t.Name] = t
	}
	for _, t := range []*Table{offerVersionTable, lifecycleEventTable} {
		for _, fk := range t.FKs {
			if rt := byName[fk.RefTable]; rt != nil {
				rt.refBy = append(rt.refBy, refFrom{t, fk})
			}
		}
	}
	// In order of name, wherever the other migrations were registered from.
	migrations = append(migrations, migration{"010_hiring_lifecycle", false, migrateHiring})
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
	return true
}

func ensureColl(ctx context.Context, db *mongo.Database, name string, ixs []Index) error {
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: name}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, name); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", name, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, name, ixs)
}

// migrateHiring is migration 010: the offer_version and lifecycle_event collections, the alias index on employee,
// and the Offer IDs already issued. Until now the Offer ID was issued when the interview said Selected, so every
// candidate selected before this migration keeps the OI- number they already have (offer_no on the candidate and
// on their offer). Nothing else is changed: no employee, no stage, no offer terms. Safe to run on live data, and
// again (it only fills offer_no where it is missing).
func migrateHiring(ctx context.Context, s *Store) error {
	db := s.DB.db
	if err := ensureColl(ctx, db, OfferVersionColl, offerVersionTable.Indexes); err != nil {
		return err
	}
	if err := ensureColl(ctx, db, LifecycleEventColl, lifecycleEventTable.Indexes); err != nil {
		return err
	}
	if err := mdmCreateIndexes(ctx, db, "employee", []Index{employeeAliasIndex}); err != nil {
		return err
	}
	_, err := BackfillOfferNos(ctx, s.DB)
	return err
}

// BackfillOfferNos gives every selected candidate without an Offer ID the OI- number they were issued under the
// old rule (and the same on their offer). It returns how many candidates it set.
func BackfillOfferNos(ctx context.Context, q Q) (int, error) {
	cur, err := q.db.Collection("candidate").Find(q.Bind(ctx), bson.D{{Key: "interview_result", Value: "Selected"},
		{Key: "$or", Value: bson.A{bson.D{{Key: "offer_no", Value: nil}}, bson.D{{Key: "offer_no", Value: bson.D{{Key: "$exists", Value: false}}}}}}},
		options.Find().SetProjection(bson.D{{Key: "no", Value: 1}}))
	if err != nil {
		return 0, err
	}
	var rows []bson.M
	if err := cur.All(q.Bind(ctx), &rows); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		no := 0
		switch v := r["no"].(type) {
		case int32:
			no = int(v)
		case int64:
			no = int(v)
		case float64:
			no = int(v)
		}
		if no <= 0 {
			continue
		}
		oi := fmt.Sprintf("OI-%04d", no)
		if _, err := q.db.Collection("candidate").UpdateOne(q.Bind(ctx), bson.D{{Key: "_id", Value: r["_id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "offer_no", Value: oi}}}}); err != nil {
			return n, err
		}
		if _, err := q.db.Collection("offer").UpdateOne(q.Bind(ctx), bson.D{{Key: "candidate_id", Value: r["_id"]}, {Key: "offer_no", Value: nil}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "offer_no", Value: oi}}}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------- Re-key: a Joining-ID record moves to its Employee ID ----------

// RefField is a stored field that holds an employee's id; Array when it is a list of ids.
type RefField struct {
	Coll, Field string
	Array       bool
}

// EmployeeRefFields lists every field of the registry that holds an employee's id: each foreign key to employee
// (the employee's own manager columns included), and every column named employee_id or …_employee_id that has no
// foreign key. Sorted, without repeats.
func EmployeeRefFields() []RefField {
	seen := map[string]bool{}
	out := []RefField{}
	add := func(t *Table, col string) {
		k := t.Name + "." + col
		if seen[k] || col == "id" {
			return
		}
		seen[k] = true
		c := t.cols[col]
		out = append(out, RefField{Coll: t.Name, Field: t.field(col), Array: c != nil && c.Array})
	}
	if emp := tableByName["employee"]; emp != nil {
		for _, r := range emp.refBy {
			add(r.table, r.fk.Col)
		}
	}
	names := make([]string, 0, len(tableByName))
	for n := range tableByName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := tableByName[n]
		for _, c := range t.Columns {
			if c.Type == TText && (c.Name == "employee_id" || (len(c.Name) > 12 && c.Name[len(c.Name)-12:] == "_employee_id") ||
				(c.Array && (c.Name == "employees" || c.Name == "employee_ids"))) {
				add(t, c.Name)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Coll != out[j].Coll {
			return out[i].Coll < out[j].Coll
		}
		return out[i].Field < out[j].Field
	})
	return out
}

// rekeySkip are collections that never hold an employee's id as data: the app's bookkeeping, and the audit log
// (append-only history: it keeps the ID the record had at the time; the alias resolves it).
var rekeySkip = map[string]bool{MigrationsColl: true, countersColl: true, locksColl: true, "job_queue": true, "job_queue_meta": true, "audit_log": true}

// RekeyEmployee moves the employee record oldID to newID inside tx: the record is copied under the new id with
// oldID added to alias_ids, every reference is pointed at the new id, and the old record is removed. The
// references are the registry's (EmployeeRefFields) plus an employee_id field in any collection the registry
// does not know (collections added later). Values inside JSON snapshots (payslips, payloads) are left as they
// were written: they record the ID at that time. It returns the number of documents changed per collection.
//
// Writes go to the driver directly, so the foreign-key checks that would refuse the in-between state (the new
// record exists, the references still point at the old one) are not run; the transaction makes it all or nothing.
func RekeyEmployee(ctx context.Context, tx Q, oldID, newID string) (map[string]int64, error) {
	if !tx.InTx() {
		return nil, errors.New("re-key: needs a transaction")
	}
	if oldID == "" || newID == "" || oldID == newID {
		return nil, fmt.Errorf("re-key: bad ids %q → %q", oldID, newID)
	}
	bctx := tx.Bind(ctx)
	emp := tx.db.Collection("employee")
	raw, err := emp.FindOne(bctx, bson.D{{Key: "_id", Value: oldID}}).Raw()
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("re-key: no employee %s", oldID)
		}
		return nil, err
	}
	if n, err := emp.CountDocuments(bctx, bson.D{{Key: "_id", Value: newID}}); err != nil || n > 0 {
		if err == nil {
			err = fmt.Errorf("re-key: %s already exists", newID)
		}
		return nil, err
	}
	var doc bson.D
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	aliases := bson.A{}
	out := bson.D{{Key: "_id", Value: newID}}
	for _, e := range doc {
		switch e.Key {
		case "_id":
		case "alias_ids":
			if l, ok := e.Value.(bson.A); ok {
				aliases = append(aliases, l...)
			}
		default:
			out = append(out, e)
		}
	}
	aliases = append(aliases, oldID)
	out = append(out, bson.E{Key: "alias_ids", Value: aliases})
	if _, err := emp.InsertOne(bctx, out); err != nil {
		return nil, err
	}
	changed := map[string]int64{}
	covered := map[string]bool{}
	for _, f := range EmployeeRefFields() {
		covered[f.Coll+"."+f.Field] = true
		var res *mongo.UpdateResult
		if f.Array {
			res, err = tx.db.Collection(f.Coll).UpdateMany(bctx, bson.D{{Key: f.Field, Value: oldID}}, bson.D{{Key: "$set", Value: bson.D{{Key: f.Field + ".$[x]", Value: newID}}}},
				options.UpdateMany().SetArrayFilters([]any{bson.D{{Key: "x", Value: oldID}}}))
		} else {
			res, err = tx.db.Collection(f.Coll).UpdateMany(bctx, bson.D{{Key: f.Field, Value: oldID}}, bson.D{{Key: "$set", Value: bson.D{{Key: f.Field, Value: newID}}}})
		}
		if err != nil {
			return nil, fmt.Errorf("re-key %s.%s: %w", f.Coll, f.Field, err)
		}
		if res.ModifiedCount > 0 {
			changed[f.Coll] += res.ModifiedCount
		}
	}
	// Work policy mapped to this one person (an employee-level mapping or exception names them in ref).
	if res, err := tx.db.Collection("work_policy_map").UpdateMany(bctx, bson.D{{Key: "level", Value: "employee"}, {Key: "ref", Value: oldID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "ref", Value: newID}}}}); err != nil {
		return nil, fmt.Errorf("re-key work_policy_map.ref: %w", err)
	} else if res.ModifiedCount > 0 {
		changed["work_policy_map"] += res.ModifiedCount
	}
	// Collections the registry does not describe (added by later code): their employee_id field.
	names, err := tx.db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if rekeySkip[n] || tableByName[n] != nil || covered[n+".employee_id"] || len(n) > 7 && n[:7] == "system." {
			continue
		}
		res, err := tx.db.Collection(n).UpdateMany(bctx, bson.D{{Key: "employee_id", Value: oldID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "employee_id", Value: newID}}}})
		if err != nil {
			return nil, fmt.Errorf("re-key %s.employee_id: %w", n, err)
		}
		if res.ModifiedCount > 0 {
			changed[n] += res.ModifiedCount
		}
	}
	if _, err := emp.DeleteOne(bctx, bson.D{{Key: "_id", Value: oldID}}); err != nil {
		return nil, err
	}
	return changed, nil
}

// EmployeeByAlias is the id of the record that has id as an old ID (alias_ids), or "".
func EmployeeByAlias(ctx context.Context, q Q, id string) string {
	var r bson.M
	if err := q.db.Collection("employee").FindOne(q.Bind(ctx), bson.D{{Key: "alias_ids", Value: id}}, options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}})).Decode(&r); err != nil {
		return ""
	}
	s, _ := r["_id"].(string)
	return s
}
