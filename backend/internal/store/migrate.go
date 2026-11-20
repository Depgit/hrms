package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MigrationsColl records the applied migrations ({_id: name, applied_at}).
const MigrationsColl = "schema_migrations"

type migration struct {
	name string
	// data: the migration inserts reference data. An import from PostgreSQL brings that data with it, so
	// MigrateForImport records such a migration as applied without running it.
	data bool
	up   func(ctx context.Context, s *Store) error
}

// The migrations, applied in order, once each. Add new ones at the end (index changes go in the registry and in
// a migration that creates them).
var migrations = []migration{
	{"001_collections", false, func(ctx context.Context, s *Store) error { return createCollections(ctx, s.DB.db) }},
	{"002_reference_data", true, func(ctx context.Context, s *Store) error {
		return s.Tx(ctx, func(tx Q) error { return insertBaseline(ctx, tx) })
	}},
	// Master data sync (SOP-ARCH-MDM-01, schema_mdm.go): the mdm_* collections and location.property_id.
	{"003_master_data_sync", false, func(ctx context.Context, s *Store) error { return migrateMDM(ctx, s) }},
	// Approval rule lists (schema_flow.go): approval_flow, every grid of approval_matrix converted (the grids stay).
	{"004_approval_rules", false, func(ctx context.Context, s *Store) error { return migrateApprovalFlow(ctx, s) }},
	// The seeded rule lists of leave and of the amounts, still untouched, get ranges that meet (schema_flow.go).
	{"005_seeded_ranges_meet", false, func(ctx context.Context, s *Store) error { return migrateSeededRanges(ctx, s) }},
	// The joiner's self-fill link (schema_joining.go): joining_link, and who fills each Form setup item.
	{"006_joiner_link", false, func(ctx context.Context, s *Store) error { return migrateJoining(ctx, s) }},
	// Documents from existing employees (schema_doccollect.go): the doc_window collection. Additive only.
	{"007_doc_collection", false, func(ctx context.Context, s *Store) error { return migrateDocCollect(ctx, s) }},
	// Uniform sizes (schema_uniform.go): the uniform_size collection, the Uniform section of Form setup and the size
	// type of uniform items named for it. Additive only: uniform issues and pay lines are not touched. An import
	// records it without running it, so the copied Form setup stays as the source has it; the start after the import
	// adds the rest (healUniform).
	{"008_uniform_sizes", true, func(ctx context.Context, s *Store) error { return migrateUniform(ctx, s) }},
}

// Migrate applies the migrations that have not been applied yet.
func (s *Store) Migrate(ctx context.Context, log func(string)) error {
	return s.migrate(ctx, log, false)
}

// MigrateForImport applies the schema migrations and records the reference-data ones as applied without running
// them (the imported PostgreSQL data already holds that data).
func (s *Store) MigrateForImport(ctx context.Context, log func(string)) error {
	return s.migrate(ctx, log, true)
}

func (s *Store) migrate(ctx context.Context, log func(string), importing bool) error {
	coll := s.DB.db.Collection(MigrationsColl)
	for _, m := range migrations {
		n, err := coll.CountDocuments(ctx, bson.D{{Key: "_id", Value: m.name}})
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if importing && m.data {
			log("recorded " + m.name + " (its data comes with the import)")
		} else {
			if err := m.up(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", m.name, err)
			}
			log("applied " + m.name)
		}
		if _, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: m.name}, {Key: "applied_at", Value: time.Now()}}); err != nil {
			return err
		}
	}
	if importing { // the import converts its grids itself, after the copy
		return nil
	}
	if err := healApprovalFlows(ctx, s, log); err != nil {
		return err
	}
	if err := healJoining(ctx, s, log); err != nil {
		return err
	}
	if err := healDocCollect(ctx, s); err != nil {
		return err
	}
	if err := healHst(ctx, s); err != nil {
		return err
	}
	return healUniform(ctx, s, log)
}

// createCollections creates every collection of the registry with its indexes, and the sequences' counters at
// their START (only those that do not exist). It can run again safely.
func createCollections(ctx context.Context, db *mongo.Database) error {
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	want := []string{MigrationsColl, countersColl, locksColl}
	for _, t := range schemaTables {
		want = append(want, t.Name)
	}
	for _, n := range want {
		if have[n] {
			continue
		}
		if err := db.CreateCollection(ctx, n); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", n, err)
			}
		}
	}
	for _, t := range schemaTables {
		var models []mongo.IndexModel
		for _, ix := range t.Indexes {
			keys := bson.D{}
			for _, k := range ix.Keys {
				dir := 1
				if k.Desc {
					dir = -1
				}
				keys = append(keys, bson.E{Key: k.Field, Value: dir})
			}
			o := options.Index().SetName(ix.Name)
			if ix.Unique {
				o.SetUnique(true)
			}
			if len(ix.Partial) > 0 {
				o.SetPartialFilterExpression(ix.Partial)
			}
			if ix.Lower {
				o.SetCollation(&options.Collation{Locale: "en", Strength: 2})
			}
			models = append(models, mongo.IndexModel{Keys: keys, Options: o})
		}
		if len(models) == 0 {
			continue
		}
		if _, err := db.Collection(t.Name).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("indexes of %s: %w", t.Name, err)
		}
	}
	for _, sq := range schemaSequences {
		if err := ensureCounter(ctx, db, sq.Name); err != nil {
			return err
		}
	}
	return nil
}

type baselineTable struct {
	Table string
	Rows  []Row
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// insertBaseline inserts the reference rows the SQL migrations inserted (baseline_gen.go): random uuids are
// generated afresh (and the foreign keys pointing at them follow), and the counters move past the ids used.
func insertBaseline(ctx context.Context, tx Q) error {
	remap := map[string]map[string]string{}
	for _, bt := range baselineRows {
		t := tableByName[bt.Table]
		if t == nil {
			return fmt.Errorf("reference data for unknown table %s", bt.Table)
		}
		idc := t.Col("id")
		fresh := t.IDIsPK && idc != nil && idc.Default.Kind == DefUUID
		if fresh {
			remap[t.Name] = map[string]string{}
		}
		docs := make([]any, 0, len(bt.Rows))
		for _, r := range bt.Rows {
			d := bson.M{}
			for k, v := range r {
				d[k] = v
			}
			for _, fk := range t.FKs {
				if m := remap[fk.RefTable]; m != nil {
					if s, ok := d[fk.Col].(string); ok {
						if n, ok := m[s]; ok {
							d[fk.Col] = n
						}
					}
				}
			}
			if fresh {
				if old, ok := d["id"].(string); ok {
					n := NewID()
					remap[t.Name][old] = n
					d["id"] = n
				}
			}
			docs = append(docs, d)
		}
		if err := tx.C(t.Name).InsertMany(ctx, docs); err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
	}
	for name, last := range baselineCounters {
		if err := ensureCounter(ctx, tx.db, name); err != nil {
			return err
		}
		if _, err := tx.db.Collection(countersColl).UpdateOne(noSession{ctx}, bson.D{{Key: "_id", Value: name}},
			bson.D{{Key: "$max", Value: bson.D{{Key: "value", Value: last}}}}); err != nil {
			return err
		}
	}
	return nil
}

// ClearData empties every collection except schema_migrations — the registry's, the counters, the locks and any
// other the app keeps (indexes stay) — and puts the counters back at their START.
func (s *Store) ClearData(ctx context.Context) error { return s.ClearDataExcept(ctx) }

// ClearDataExcept is ClearData leaving the named collections as they are (import-pg -replace keeps the master
// data sync's collections, which have no PostgreSQL counterpart).
func (s *Store) ClearDataExcept(ctx context.Context, keep ...string) error {
	db := s.DB.db
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return err
	}
	kept := map[string]bool{MigrationsColl: true}
	for _, n := range keep {
		kept[n] = true
	}
	for _, n := range names {
		if kept[n] || strings.HasPrefix(n, "system.") {
			continue
		}
		if _, err := db.Collection(n).DeleteMany(ctx, bson.D{}); err != nil {
			return err
		}
	}
	for _, sq := range schemaSequences {
		if err := ensureCounter(ctx, db, sq.Name); err != nil {
			return err
		}
	}
	return nil
}

// Reset makes the database look freshly migrated: ClearData, then the reference data of migration 002 again, its
// approval grids converted (migration 004), who fills each Form setup item set (migration 006) and the uniform
// sizes' Form setup fields and item size types (migration 008). The migrations must have run. For tests.
func (s *Store) Reset(ctx context.Context) error {
	if err := s.ClearData(ctx); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		if err := insertBaseline(ctx, tx); err != nil {
			return err
		}
		if _, err := ConvertApprovalMatrices(ctx, tx); err != nil {
			return err
		}
		if _, err := FillJoinerDefaults(ctx, tx); err != nil {
			return err
		}
		if _, err := AddUniformFields(ctx, tx); err != nil {
			return err
		}
		for _, h := range resetHooks { // Form setup rows of later migrations (schema_b1.go)
			if err := h(ctx, tx); err != nil {
				return err
			}
		}
		_, err := TagUniformItems(ctx, tx)
		return err
	})
}

// ImportRows writes rows copied from PostgreSQL as they are: each column coerced to its stored form (a missing
// column is null), `id` → `_id` for an id primary key, and no defaults, checks or foreign keys (the source
// enforced them). For hrms import-pg.
func ImportRows(ctx context.Context, q Q, table string, rows []Row) error {
	t := tableByName[table]
	if t == nil {
		return fmt.Errorf("import: %s is not in the schema registry", table)
	}
	docs := make([]any, len(rows))
	for i, r := range rows {
		for k := range r {
			if t.Col(k) == nil {
				return unknownColumn(t, k)
			}
		}
		d := make(bson.D, 0, len(t.Columns))
		if t.IDIsPK {
			v, err := coerce(t, t.Col("id"), r["id"])
			if err != nil {
				return err
			}
			d = append(d, bson.E{Key: "_id", Value: v})
		}
		for j := range t.Columns {
			col := &t.Columns[j]
			if t.field(col.Name) == "_id" {
				continue
			}
			v, err := coerce(t, col, r[col.Name])
			if err != nil {
				return fmt.Errorf("%s.%s: %w", t.Name, col.Name, err)
			}
			d = append(d, bson.E{Key: col.Name, Value: v})
		}
		docs[i] = d
	}
	if len(docs) == 0 {
		return nil
	}
	_, err := q.db.Collection(table).InsertMany(q.Bind(ctx), docs)
	return mapErr(t, err)
}
