package pgimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

func TestFromPG(t *testing.T) {
	col := func(table, name string) *store.Column {
		c := store.TableOf(table).Col(name)
		if c == nil {
			t.Fatalf("%s.%s not in the registry", table, name)
		}
		return c
	}
	u := [16]byte{0x7a, 0x0f, 0x3c, 0x3e, 0x2b, 0x6d, 0x4c, 0x59, 0x9e, 0x4b, 0x0b, 0x8e, 0x3d, 0x2b, 0x1a, 0x10}
	var num pgtype.Numeric
	_ = num.Scan("1234.50")
	cases := []struct {
		col  *store.Column
		in   any
		want any
	}{
		{col("region", "id"), u, "7a0f3c3e-2b6d-4c59-9e4b-0b8e3d2b1a10"},
		{col("pay_band", "min_gross"), num, 1234.5},
		{col("shift", "start_time"), pgtype.Time{Microseconds: (9*60 + 30) * 60_000_000, Valid: true}, "09:30"},
		{col("holiday", "date"), time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC), "2026-03-04"},
		{col("shift", "location_ids"), []any{u}, []any{"7a0f3c3e-2b6d-4c59-9e4b-0b8e3d2b1a10"}},
		{col("approval_matrix", "cols"), []any{"HRBP"}, json.RawMessage(`["HRBP"]`)},
		{col("approval_matrix", "rows"), "just a string", json.RawMessage(`"just a string"`)},
		{col("notice", "ref"), []any{"a"}, []any{"a"}},
		{col("region", "name"), nil, nil},
	}
	for _, c := range cases {
		got, err := FromPG(c.col, c.in, 0)
		if err != nil {
			t.Fatal(c.col.Name, err)
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(c.want)
		if string(gb) != string(wb) {
			t.Errorf("%s: %s, want %s", c.col.Name, gb, wb)
		}
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got, _ := FromPG(col("region", "created_at"), ts, 0); got != ts {
		t.Errorf("timestamptz: %v", got)
	}
}

// TestImport copies a PostgreSQL database (HRMS_TEST_PG_URL; the import leaves it unchanged, but point it at a copy
// anyway) into HRMS_TEST_MONGO_IMPORT_DB (default hrms_test_import) on HRMS_TEST_MONGO_URL.
func TestImport(t *testing.T) {
	from, url := os.Getenv("HRMS_TEST_PG_URL"), os.Getenv("HRMS_TEST_MONGO_URL")
	if from == "" || url == "" {
		t.Skip("HRMS_TEST_PG_URL and HRMS_TEST_MONGO_URL not both set")
	}
	name := os.Getenv("HRMS_TEST_MONGO_IMPORT_DB")
	if name == "" {
		name = "hrms_test_import"
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url, name)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pg, err := pgx.Connect(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close(ctx)
	// The source's schema and migration list, before and after: the import must not change the source.
	shape := func() string {
		var s string
		if err := pg.QueryRow(ctx, `SELECT concat_ws(';', (SELECT string_agg(table_name || '.' || column_name, ',' ORDER BY table_name, column_name)
			FROM information_schema.columns WHERE table_schema='public'), (SELECT string_agg(name, ',' ORDER BY name) FROM schema_migrations))`).Scan(&s); err != nil {
			// schema_migrations does not exist yet: an empty source
			var n int64
			if err := pg.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%d tables, no schema_migrations", n)
		}
		return s
	}
	before := shape()
	counts, err := Run(ctx, st, Options{From: from, Replace: true, Log: func(s string) { t.Log(s) }})
	for _, c := range counts {
		if c.Source > 0 || c.Mismatched {
			t.Logf("%-28s source %6d  target %6d", c.Table, c.Source, c.Target)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if after := shape(); after != before {
		t.Errorf("the import changed the source:\nbefore %.300s\nafter  %.300s", before, after)
	}
	// Every approval grid became a rule list ending in Otherwise; the grids stay.
	grids, _ := st.DB.C("approval_matrix").Count(ctx, nil)
	flows, err := st.DB.C(store.ApprovalFlowColl).Find(ctx, nil)
	if err != nil || grids == 0 || int64(len(flows)) != grids {
		t.Errorf("%d approval grids, %d rule lists (%v)", grids, len(flows), err)
	}
	for _, f := range flows {
		rules, _ := f["rules"].([]any)
		if len(rules) == 0 || rules[len(rules)-1].(map[string]any)["label"] != "Otherwise" {
			t.Errorf("%v/%v: rules %v", f["wf"], f["flow"], f["rules"])
		}
	}
	// Every plain table's documents are stored in the source's physical order (the order a table scan returned
	// rows where a query had no ORDER BY), which the app reads back as $natural order. Checked on a source that
	// is already up to date: the migrations' own rows get random ids each time they run.
	tx, err := pg.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	if err := MigrateLegacy(ctx, tx, func(string) { pending++ }); err != nil {
		t.Fatal(err)
	}
	for _, tb := range store.Tables() {
		if pending > 0 {
			t.Logf("stored order not checked: the source lacks %d legacy migrations", pending)
			break
		}
		var kind string
		if err := tx.QueryRow(ctx, `SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)`, pgx.Identifier{tb.Name}.Sanitize()).Scan(&kind); err != nil {
			t.Fatal(tb.Name, err)
		}
		if kind != "r" || len(tb.PK) == 0 {
			continue
		}
		cols := make([]string, len(tb.PK))
		for i, c := range tb.PK {
			cols[i] = pgx.Identifier{c}.Sanitize() + "::text"
		}
		rows, err := tx.Query(ctx, `SELECT concat_ws('|', `+strings.Join(cols, ", ")+`) FROM `+pgx.Identifier{tb.Name}.Sanitize()+` ORDER BY ctid`)
		if err != nil {
			t.Fatal(tb.Name, err)
		}
		want, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(tb.Name, err)
		}
		docs, err := st.DB.C(tb.Name).Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}}))
		if err != nil {
			t.Fatal(tb.Name, err)
		}
		got := make([]string, len(docs))
		for i, d := range docs {
			parts := make([]string, len(tb.PK))
			for j, c := range tb.PK {
				parts[j] = fmt.Sprint(d[c])
			}
			got[i] = strings.Join(parts, "|")
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: stored order %v, source's physical order %v", tb.Name, got, want)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// A second import into the now non-empty database is refused without Replace.
	if _, err := Run(ctx, st, Options{From: from}); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty target: %v", err)
	}
}

// TestReplaceKeepsMDM: a Replace import keeps the master data sync (the CMS link has no PostgreSQL counterpart, so
// the import has nothing to put in its place) and puts location.property_id back; ResetMDM clears it all.
// Same source as TestImport; the target is HRMS_TEST_MONGO_IMPORT_DB with "_mdm" appended.
func TestReplaceKeepsMDM(t *testing.T) {
	from, url := os.Getenv("HRMS_TEST_PG_URL"), os.Getenv("HRMS_TEST_MONGO_URL")
	if from == "" || url == "" {
		t.Skip("HRMS_TEST_PG_URL and HRMS_TEST_MONGO_URL not both set")
	}
	name := os.Getenv("HRMS_TEST_MONGO_IMPORT_DB")
	if name == "" {
		name = "hrms_test_import"
	}
	name += "_mdm"
	ctx := context.Background()
	st, err := store.Open(ctx, url, name)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	logf := func(s string) { t.Log(s) }
	if _, err := Run(ctx, st, Options{From: from, Replace: true, ResetMDM: true, Log: logf}); err != nil {
		t.Fatal(err)
	}
	loc, err := st.DB.C("location").FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.D{{Key: "id", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	if loc == nil {
		t.Skip("the source has no location")
	}
	// The CMS link as a server that talks to the CMS leaves it.
	raw := func(coll string, doc bson.M) {
		t.Helper()
		if _, err := st.DB.C(coll).Raw().InsertOne(ctx, doc); err != nil {
			t.Fatal(coll, err)
		}
	}
	if _, err := st.DB.C("location").Raw().UpdateOne(ctx, bson.M{"_id": loc["id"]}, bson.M{"$set": bson.M{"property_id": "PROP-T1"}}); err != nil {
		t.Fatal(err)
	}
	raw("location", bson.M{"_id": "gone-location", "name": "Not in the source", "property_id": "PROP-GONE"})
	raw("mdm_subscriber", bson.M{"_id": "sub-1", "name": "CMS", "url": "http://cms.invalid/hook", "cursor": int64(7)})
	raw("mdm_feed_key", bson.M{"_id": "key-1", "name": "CMS pull", "key_hash": "h", "key_last4": "abcd"})
	raw("mdm_setting", bson.M{"_id": int32(1), "cms_base_url": "http://cms.invalid", "properties_from_cms": true})
	raw("mdm_published", bson.M{"_id": "EMP-1001", "sequence": int64(3), "hash": "x", "pos": int64(7)})
	raw("mdm_outbox", bson.M{"_id": int64(7), "employee_code": "EMP-1001", "sequence": int64(3), "event_type": "EMPLOYEE_UPDATED"})
	raw("mdm_property", bson.M{"_id": "PROP-T1", "name": "Hub", "status": "Active", "source": "event"})
	raw("mdm_state", bson.M{"_id": "pos", "n": int64(7)})
	raw("mdm_state", bson.M{"_id": "watcher", "owner": nil, "token": "resume-here"})
	mdmCounts := func() map[string]int64 {
		m := map[string]int64{}
		for _, tb := range store.MDMTables() {
			n, err := st.DB.C(tb.Name).Count(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			m[tb.Name] = n
		}
		return m
	}
	before := mdmCounts()

	// Replace: the CMS link stays.
	if _, err := Run(ctx, st, Options{From: from, Replace: true, Log: logf}); err != nil {
		t.Fatal(err)
	}
	if after := mdmCounts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("mdm collections after Replace: %v, want %v", after, before)
	}
	pos, err := st.DB.C("mdm_state").FindOne(ctx, bson.M{"_id": "pos"})
	if err != nil || pos["n"] != int64(7) {
		t.Errorf("outbox position after Replace: %v (%v), want 7", pos, err)
	}
	w, err := st.DB.C("mdm_state").FindOne(ctx, bson.M{"_id": "watcher"})
	if err != nil || w["token"] != nil {
		t.Errorf("watcher state after Replace: %v (%v), want no saved position (a rescan)", w, err)
	}
	got, err := st.DB.C("location").FindOne(ctx, bson.M{"id": loc["id"]})
	if err != nil || got["property_id"] != "PROP-T1" {
		t.Errorf("location %v property_id after Replace: %v (%v), want PROP-T1", loc["id"], got["property_id"], err)
	}
	if n, _ := st.DB.C("location").Count(ctx, bson.M{"property_id": "PROP-GONE"}); n != 0 {
		t.Errorf("a location not in the source survived Replace")
	}

	// Replace with ResetMDM: everything goes, as in a first import.
	if _, err := Run(ctx, st, Options{From: from, Replace: true, ResetMDM: true, Log: logf}); err != nil {
		t.Fatal(err)
	}
	for coll, n := range mdmCounts() {
		if n != 0 {
			t.Errorf("%s after ResetMDM: %d documents", coll, n)
		}
	}
	if n, _ := st.DB.C("location").Count(ctx, bson.M{"property_id": bson.M{"$type": "string"}}); n != 0 {
		t.Errorf("%d linked location(s) after ResetMDM", n)
	}
	if err := st.Client.Database(name).Drop(ctx); err != nil {
		t.Log(err)
	}
}
