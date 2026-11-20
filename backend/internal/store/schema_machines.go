package store

// Registry additions for punch machines as one pool per cluster (workforce plan, module 7; 17 Nov 2026). Each
// machine (T304F Mini) holds about 1,000 people; the machines of a cluster are one shared store, so a person can
// punch at any machine of their cluster and a move between properties of one cluster needs no re-enrolment.
//
//   - attendance_device: capacity (people the machine holds; null = the default of company_setting.machine_pool,
//     1,000), and what the Realtime portal's machine list says about it: portal_branch (its Branch Name), model,
//     machine_no, io_mode (In, Out, In/Out), portal_status (an online/offline column, when the page has one), online
//     (null = not known), portal_seen_at (the last import that listed it) and portal_gone (true when the last import
//     did not list it). All null on existing devices.
//   - company_setting.machine_pool (jsonb): default_capacity, and the last import and portal check.
//   - machine_enrol (new): one row per employee who is (or was) enrolled on a cluster's machines: cluster_id (null
//     once removed), since, how (baseline, hand, portal) and task, the last to-do done for them (kind, from, to, when,
//     by, how). It is the HRMS's record of who is on which cluster; the to-do list is the difference between it and
//     where each active employee works now.
//
// machine_enrol never existed in PostgreSQL, so it is kept out of schemaTables (import-pg does not list it) while
// C() applies its rules. Migration 013 creates it and records every active employee as enrolled on their
// property's cluster (they punch today). Nothing existing is rewritten.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// MachineEnrolColl records who is enrolled on which cluster's punch machines.
const MachineEnrolColl = "machine_enrol"

var machineEnrolTable = &Table{
	Name: MachineEnrolColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "cluster_id", Type: TUUID},
		{Name: "since", Type: TTimestamptz},
		{Name: "how", Type: TText},
		{Name: "task", Type: TJSONB},
		{Name: "task_at", Type: TTimestamptz},
		{Name: "updated_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "machine_enrol_employee_key", Keys: []IndexKey{{Field: "employee_id"}}, Unique: true},
		{Name: "machine_enrol_task_idx", Keys: []IndexKey{{Field: "task_at"}}},
	},
}

var _ = registerMachines()

func registerMachines() bool {
	for _, t := range schemaTables {
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "attendance_device":
			t.Columns = append(t.Columns,
				Column{Name: "capacity", Type: TInt4},
				Column{Name: "portal_branch", Type: TText},
				Column{Name: "model", Type: TText},
				Column{Name: "machine_no", Type: TText},
				Column{Name: "io_mode", Type: TText},
				Column{Name: "portal_status", Type: TText},
				Column{Name: "online", Type: TBool},
				Column{Name: "portal_seen_at", Type: TTimestamptz},
				Column{Name: "portal_gone", Type: TBool})
			t.Checks = append(t.Checks, Check{Name: "attendance_device_capacity_check", Expr: "CHECK (capacity >= 0 AND capacity <= 100000)",
				Conds: []Cond{{Col: "capacity", Op: ">=", Val: float64(0)}, {Col: "capacity", Op: "<=", Val: float64(100000)}}})
		case "company_setting":
			t.Columns = append(t.Columns, Column{Name: "machine_pool", Type: TJSONB})
		}
	}
	t := machineEnrolTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	migrations = append(migrations, migration{"013_machine_pool", false, migrateMachines})
	sort.SliceStable(migrations, func(i, j int) bool { return migrations[i].name < migrations[j].name })
	return true
}

// migrateMachines is migration 013: the machine_enrol collection and its indexes, and the baseline (every active
// employee enrolled on their property's cluster).
func migrateMachines(ctx context.Context, s *Store) error {
	db := s.DB.db
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: MachineEnrolColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, MachineEnrolColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", MachineEnrolColl, err)
			}
		}
	}
	if err := mdmCreateIndexes(ctx, db, MachineEnrolColl, machineEnrolTable.Indexes); err != nil {
		return err
	}
	_, err = MachineBaseline(ctx, s.DB, time.Now())
	return err
}

// MachineBaseline records every active employee who has no machine_enrol row as enrolled on the cluster of their
// property (how "baseline"), and returns how many it recorded.
func MachineBaseline(ctx context.Context, q Q, now time.Time) (int, error) {
	locs, err := q.C("location").Find(ctx, bson.M{})
	if err != nil {
		return 0, err
	}
	clusterOf := map[string]any{}
	for _, l := range locs {
		clusterOf[fmt.Sprint(l["id"])] = l["cluster_id"]
	}
	have := map[string]bool{}
	ids, err := q.C(MachineEnrolColl).Distinct(ctx, "employee_id", bson.M{})
	if err != nil {
		return 0, err
	}
	for _, v := range ids {
		have[fmt.Sprint(v)] = true
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"status": "Active"})
	if err != nil {
		return 0, err
	}
	docs := []any{}
	for _, e := range emps {
		id := fmt.Sprint(e["id"])
		cl := clusterOf[fmt.Sprint(e["location_id"])]
		if have[id] || cl == nil {
			continue
		}
		docs = append(docs, bson.M{"employee_id": id, "cluster_id": cl, "since": now, "how": "baseline"})
	}
	if len(docs) == 0 {
		return 0, nil
	}
	return len(docs), q.C(MachineEnrolColl).InsertMany(ctx, docs)
}
