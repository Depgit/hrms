// Package pgimport copies an existing PostgreSQL installation of the app into the MongoDB database
// (`hrms import-pg -from postgres://… [-replace]`). Everything it does in the source happens in ONE transaction that
// is rolled back at the end, so the source is left exactly as it was and is read as one consistent snapshot even
// while the old server still runs: inside it, the source is brought up to date with the legacy SQL migrations, as
// the old `hrms migrate` did (without the River job tables), then every table of the store's registry is copied
// with the store's type conversions, each counter is set from its Postgres sequence, the approval grids are
// converted into rule lists (approval_flow), and the document counts are checked against the source.
package pgimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/pgimport/legacy"
	"github.com/saltstayz/hrms/internal/store"
)

// Options of an import.
type Options struct {
	From     string       // the source PostgreSQL URL
	Replace  bool         // clear a non-empty target first (otherwise a non-empty target is refused)
	ResetMDM bool         // with Replace, also clear the master data sync (see keepMDM)
	Log      func(string) // progress lines
	Batch    int          // rows per insert (default 1000)
}

// Count is one table's row count in the source and document count in the target.
type Count struct {
	Table      string
	Source     int64
	Target     int64
	Mismatched bool
}

// ErrNotEmpty is returned when the target already holds data and Replace is not set.
var ErrNotEmpty = errors.New("the target database is not empty")

// Run imports the source into st's database and returns the per-collection counts.
func Run(ctx context.Context, st *store.Store, o Options) ([]Count, error) {
	log := o.Log
	if log == nil {
		log = func(string) {}
	}
	if o.Batch <= 0 {
		o.Batch = 1000
	}
	cfg, err := pgx.ParseConfig(o.From)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["timezone"] = store.Zone()
	pg, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("source not reachable: %w", err)
	}
	defer pg.Close(context.WithoutCancel(ctx))
	// One snapshot of the source for the whole copy, never committed: the migrations below only bring this
	// transaction's view of the source up to date.
	tx, err := pg.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// 1. The source, up to date (in the transaction only).
	if err := MigrateLegacy(ctx, tx, log); err != nil {
		return nil, fmt.Errorf("bringing the source up to date: %w", err)
	}

	// 2. The target: empty, or cleared with Replace.
	var used []string
	var links []store.Row // location → CMS property links kept across a Replace
	for _, t := range store.Tables() {
		n, err := st.DB.C(t.Name).Count(ctx, nil)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			used = append(used, fmt.Sprintf("%s (%d)", t.Name, n))
		}
	}
	if len(used) > 0 {
		if !o.Replace {
			return nil, fmt.Errorf("%w: %s; run with -replace to clear it first", ErrNotEmpty, strings.Join(used, ", "))
		}
		log(fmt.Sprintf("clearing %d non-empty collections (-replace)", len(used)))
		if o.ResetMDM {
			if err := st.ClearData(ctx); err != nil {
				return nil, err
			}
		} else {
			if links, err = keepMDM(ctx, st, log); err != nil {
				return nil, err
			}
		}
	}
	if err := st.MigrateForImport(ctx, log); err != nil {
		return nil, err
	}

	// 3. Every table.
	var counts []Count
	for _, t := range store.Tables() {
		n, err := copyTable(ctx, tx, st, t, o.Batch)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name, err)
		}
		log(fmt.Sprintf("copied %-28s %d", t.Name, n))
	}

	if err := restoreLinks(ctx, st, links, log); err != nil {
		return nil, err
	}

	// The approval grids become rule lists (approval_flow), routing every request as before; the grids stay.
	if n, err := st.MigrateApprovalFlow(ctx); err != nil {
		return nil, fmt.Errorf("approval rules: %w", err)
	} else {
		log(fmt.Sprintf("converted %d approval grid(s) into rule lists", n))
	}

	// Who fills each Form setup item (the joiner's link): the defaults where the source had none.
	if n, err := store.FillJoinerDefaults(ctx, st.DB); err != nil {
		return nil, fmt.Errorf("form setup: %w", err)
	} else if n > 0 {
		log(fmt.Sprintf("set who fills %d Form setup item(s) (joiner or HR)", n))
	}

	// River's job tables are not imported, so a payroll run that was queued in Postgres would never run here and
	// its month would look "still being calculated" forever. Clear payroll_month.job; HR starts the run again.
	if n, err := st.DB.C("payroll_month").UpdateMany(ctx, bson.M{"job": bson.M{"$ne": nil}}, bson.M{"$set": bson.M{"job": nil}}); err != nil {
		return nil, fmt.Errorf("payroll_month: clearing queued runs: %w", err)
	} else if n > 0 {
		log(fmt.Sprintf("cleared %d payroll run(s) queued in the source's job queue (start them again)", n))
	}

	// 4. Counters from the sequences.
	for _, s := range store.Sequences() {
		var last int64
		var called bool
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT last_value, is_called FROM %s`, pgx.Identifier{s.Name}.Sanitize())).Scan(&last, &called)
		if err != nil {
			return nil, fmt.Errorf("sequence %s: %w", s.Name, err)
		}
		if !called {
			last-- // nextval returns last_value itself
		}
		if err := store.SetSeq(ctx, st.DB, s.Name, last); err != nil {
			return nil, err
		}
	}
	log(fmt.Sprintf("set %d counters from the sequences", len(store.Sequences())))

	// 5. Counts, source against target.
	bad := 0
	for _, t := range store.Tables() {
		var src int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, pgx.Identifier{t.Name}.Sanitize())).Scan(&src); err != nil {
			return nil, err
		}
		dst, err := st.DB.C(t.Name).Count(ctx, nil)
		if err != nil {
			return nil, err
		}
		c := Count{Table: t.Name, Source: src, Target: dst, Mismatched: src != dst}
		if c.Mismatched {
			bad++
		}
		counts = append(counts, c)
	}
	if bad > 0 {
		return counts, fmt.Errorf("%d collection(s) do not have the source's row count", bad)
	}
	return counts, nil
}

// keepMDM clears the target for a Replace (without ResetMDM) except the master data sync's collections (they never existed in
// PostgreSQL, so the import has nothing to put in their place), and returns the location → property links to put
// back after the copy. The CMS already knows every employee in mdm_published, so the next server compares the
// imported employees with those snapshots and publishes only what changed, at the next outbox position. The
// watcher's saved stream position is dropped: the import's own deletes and inserts are not changes to replay, and
// without a position the watcher rescans every employee against the kept snapshots instead.
func keepMDM(ctx context.Context, st *store.Store, log func(string)) ([]store.Row, error) {
	links, err := st.DB.C("location").Find(ctx, bson.M{"property_id": bson.M{"$type": "string"}},
		options.Find().SetProjection(bson.M{"id": 1, "property_id": 1}))
	if err != nil {
		return nil, err
	}
	var keep []string
	for _, t := range store.MDMTables() {
		keep = append(keep, t.Name)
	}
	if err := st.ClearDataExcept(ctx, keep...); err != nil {
		return nil, err
	}
	if _, err := st.DB.C("mdm_state").Update(ctx, bson.M{"_id": "watcher"}, bson.M{"$set": bson.M{"token": nil}}); err != nil {
		return nil, err
	}
	log(fmt.Sprintf("kept the master data sync (mdm_*, %d linked location(s)); -reset-mdm clears it too", len(links)))
	return links, nil
}

// restoreLinks puts location.property_id back on the imported locations that had one before a Replace.
func restoreLinks(ctx context.Context, st *store.Store, links []store.Row, log func(string)) error {
	var lost []string
	for _, l := range links {
		n, err := st.DB.C("location").Update(ctx, bson.M{"id": l["id"], "property_id": nil}, bson.M{"$set": bson.M{"property_id": l["property_id"]}})
		if err != nil {
			return fmt.Errorf("location %v: putting back property_id %v: %w", l["id"], l["property_id"], err)
		}
		if n == 0 {
			lost = append(lost, fmt.Sprintf("%v (%v)", l["id"], l["property_id"]))
		}
	}
	if len(links) > 0 {
		log(fmt.Sprintf("put back the CMS property link of %d location(s)", len(links)-len(lost)))
	}
	if len(lost) > 0 {
		log("not in the source any more, so unlinked: " + strings.Join(lost, ", "))
	}
	return nil
}

// MigrateLegacy applies the legacy SQL migrations the source lacks, each in its own savepoint of tx, recording them
// in schema_migrations — what the PostgreSQL version's `hrms migrate` did, minus River's own tables. Nothing is
// kept unless the caller commits tx (Run never does).
func MigrateLegacy(ctx context.Context, pg pgx.Tx, log func(string)) error {
	if _, err := pg.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(legacy.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		var done bool
		if err := pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, n).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, _ := legacy.FS.ReadFile(n)
		err := pgx.BeginFunc(ctx, pg, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, n)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		log("source: applied " + n + " (in the import's transaction only; the source is not changed)")
	}
	return nil
}

// copyTable streams one table into its collection in batches.
//
// Rows are copied in the source's physical order, the order a PostgreSQL table scan returned them where a query
// had no ORDER BY or tied on it. The collection's stored ($natural) order is then that same order, and the app
// reads in stored order wherever the SQL relied on it (role views, role mappings, ties), so an imported
// installation shows lists in the order it did before. A plain table is read ORDER BY ctid; a partitioned one is
// scanned partition by partition (no parallel workers, no synchronized scans, so each partition starts at its
// first block), which is how PostgreSQL itself read it.
func copyTable(ctx context.Context, pg pgx.Tx, st *store.Store, t *store.Table, batch int) (int64, error) {
	var kind string
	if err := pg.QueryRow(ctx, `SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)`, pgx.Identifier{t.Name}.Sanitize()).Scan(&kind); err != nil {
		return 0, fmt.Errorf("source table: %w", err)
	}
	order := " ORDER BY ctid"
	if kind == "p" {
		order = ""
		if _, err := pg.Exec(ctx, `SET LOCAL synchronize_seqscans = off; SET LOCAL max_parallel_workers_per_gather = 0`); err != nil {
			return 0, err
		}
	}
	rows, err := pg.Query(ctx, `SELECT * FROM `+pgx.Identifier{t.Name}.Sanitize()+order)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	var buf []store.Row
	var n int64
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := store.ImportRows(ctx, st.DB, t.Name, buf); err != nil {
			return err
		}
		n += int64(len(buf))
		buf = buf[:0]
		return nil
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return n, err
		}
		r := make(store.Row, len(vals))
		for i, fd := range fds {
			col := t.Col(fd.Name)
			if col == nil {
				return n, fmt.Errorf("column %s is not in the registry", fd.Name)
			}
			v, err := FromPG(col, vals[i], fd.DataTypeOID)
			if err != nil {
				return n, fmt.Errorf("column %s: %w", fd.Name, err)
			}
			r[fd.Name] = v
		}
		buf = append(buf, r)
		if len(buf) >= batch {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	return n, flush()
}

// FromPG converts a value pgx read into what the store's coercion takes for the column: uuid → string,
// numeric → float64, date → YYYY-MM-DD, time → HH:MM, timestamptz → time.Time, jsonb → JSON text (so a JSON
// string stays a string), arrays element by element.
func FromPG(col *store.Column, v any, oid uint32) (any, error) {
	if v == nil {
		return nil, nil
	}
	if col.Type == store.TJSONB {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
	if col.Array {
		l, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("array expected, got %T", v)
		}
		out := make([]any, len(l))
		for i, e := range l {
			x, err := scalarFromPG(col, e)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	}
	return scalarFromPG(col, v)
}

func scalarFromPG(col *store.Column, v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case [16]byte:
		return pgtype.UUID{Bytes: x, Valid: true}.String(), nil
	case pgtype.Numeric:
		f, err := x.Float64Value()
		if err != nil {
			return nil, err
		}
		if !f.Valid {
			return nil, nil
		}
		return f.Float64, nil
	case pgtype.Time:
		if !x.Valid {
			return nil, nil
		}
		m := x.Microseconds / 60_000_000
		return fmt.Sprintf("%02d:%02d", m/60, m%60), nil
	case time.Time:
		if col.Type == store.TDate {
			return x.Format("2006-01-02"), nil
		}
		return x, nil
	}
	return v, nil
}
