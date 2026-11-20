package store

// Registry additions for the cross-product master data sync (SOP-ARCH-MDM-01, internal/mdm):
//
//   - location.property_id: the CMS property a location is (CMS is the source of truth for property_id). Null until
//     the location is linked; unique when set; set once (the app refuses to change it).
//   - the mdm_* collections: HRMS's employee outbox and published snapshots, push subscribers and pull-feed keys,
//     the inbox and local read-only copy of CMS properties, the connection settings, reconciliation runs and the
//     watcher's state.
//
// The mdm_* collections never existed in PostgreSQL, so they are kept out of schemaTables: Tables() (and with it
// `hrms import-pg`, which copies every listed table) does not list them, while C() still applies their rules.
// Migration 003_master_data_sync creates them with their indexes.

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The event types HRMS publishes for an employee (SOP §4).
var mdmEmployeeEventTypes = []any{"EMPLOYEE_CREATED", "EMPLOYEE_UPDATED", "EMPLOYEE_SEPARATED", "EMPLOYEE_REACTIVATED"}

// locationPropertyIndex makes property_id unique among the locations that have one.
var locationPropertyIndex = Index{Name: "location_property_id_key", Keys: []IndexKey{{Field: "property_id"}}, Unique: true,
	Partial: bson.D{{Key: "property_id", Value: bson.D{{Key: "$type", Value: "string"}}}}}

var mdmTables = []*Table{
	{
		// One document per published employee event, in commit order (pos).
		Name: "mdm_outbox",
		Columns: []Column{
			{Name: "pos", Type: TInt8, NotNull: true},
			{Name: "event_id", Type: TUUID, NotNull: true},
			{Name: "event_type", Type: TText, NotNull: true},
			{Name: "schema_version", Type: TText, NotNull: true},
			{Name: "timestamp", Type: TText, NotNull: true}, // UTC RFC 3339, exactly as published
			{Name: "source", Type: TText, NotNull: true, Default: Default{Kind: DefConst, Value: "HRMS"}},
			{Name: "employee_code", Type: TText, NotNull: true},
			{Name: "sequence", Type: TInt8, NotNull: true},
			{Name: "data", Type: TJSONB, NotNull: true},
			{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		},
		PK: []string{"pos"},
		Indexes: []Index{
			{Name: "mdm_outbox_pkey", Keys: []IndexKey{{Field: "pos"}}, Unique: true},
			{Name: "mdm_outbox_event_id_key", Keys: []IndexKey{{Field: "event_id"}}, Unique: true},
			{Name: "mdm_outbox_employee_sequence_key", Keys: []IndexKey{{Field: "employee_code"}, {Field: "sequence"}}, Unique: true},
		},
		Checks: []Check{
			{Name: "mdm_outbox_event_type_check", Conds: []Cond{{Col: "event_type", Op: "in", Val: mdmEmployeeEventTypes}}},
			{Name: "mdm_outbox_sequence_check", Conds: []Cond{{Col: "sequence", Op: ">", Val: float64(0)}}},
		},
	},
	{
		// The last snapshot published per employee: what the next change is compared with.
		Name: "mdm_published",
		Columns: []Column{
			{Name: "employee_code", Type: TText, NotNull: true},
			{Name: "sequence", Type: TInt8, NotNull: true},
			{Name: "hash", Type: TText, NotNull: true},
			{Name: "data", Type: TJSONB, NotNull: true},
			{Name: "event_id", Type: TUUID, NotNull: true},
			{Name: "pos", Type: TInt8, NotNull: true},
			{Name: "updated_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		},
		PK: []string{"employee_code"},
		Indexes: []Index{
			{Name: "mdm_published_pkey", Keys: []IndexKey{{Field: "employee_code"}}, Unique: true},
		},
	},
	{
		// A consumer that receives the events by signed webhook (push), in pos order, never skipping.
		Name: "mdm_subscriber",
		Columns: []Column{
			{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
			{Name: "name", Type: TText, NotNull: true},
			{Name: "url", Type: TText, NotNull: true},
			{Name: "secret", Type: TBytea, NotNull: true}, // sealed with the app's secret key
			{Name: "secret_last4", Type: TText, NotNull: true},
			{Name: "event_types", Type: TText, Array: true, NotNull: true, Default: Default{Kind: DefConst, Value: []string{}}}, // empty: all
			{Name: "active", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
			{Name: "cursor", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}}, // last pos delivered (or passed)
			{Name: "attempts", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
			{Name: "next_attempt_at", Type: TTimestamptz},
			{Name: "last_error", Type: TText},
			{Name: "failing_since", Type: TTimestamptz},
			{Name: "last_status", Type: TInt4},
			{Name: "last_delivered_at", Type: TTimestamptz},
			{Name: "claim_until", Type: TTimestamptz},
			{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
			{Name: "created_by", Type: TText},
		},
		PK:     []string{"id"},
		IDIsPK: true,
		Indexes: []Index{
			{Name: "mdm_subscriber_name_key", Keys: []IndexKey{{Field: "name"}}, Unique: true, Lower: true},
		},
		Checks: []Check{
			{Name: "mdm_subscriber_cursor_check", Conds: []Cond{{Col: "cursor", Op: ">=", Val: float64(0)}}},
		},
	},
	{
		// A consumer's API key for the pull feed and the snapshot endpoints; only its hash is kept.
		Name: "mdm_feed_key",
		Columns: []Column{
			{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
			{Name: "name", Type: TText, NotNull: true},
			{Name: "key_hash", Type: TText, NotNull: true},
			{Name: "key_last4", Type: TText, NotNull: true},
			{Name: "active", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
			{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
			{Name: "created_by", Type: TText},
			{Name: "last_used_at", Type: TTimestamptz},
		},
		PK:     []string{"id"},
		IDIsPK: true,
		Indexes: []Index{
			{Name: "mdm_feed_key_key_hash_key", Keys: []IndexKey{{Field: "key_hash"}}, Unique: true},
			{Name: "mdm_feed_key_name_key", Keys: []IndexKey{{Field: "name"}}, Unique: true, Lower: true},
		},
	},
	{
		// Every property event the CMS sent, by event_id (idempotency), with what HRMS did with it.
		Name: "mdm_inbox",
		Columns: []Column{
			{Name: "event_id", Type: TText, NotNull: true},
			{Name: "event_type", Type: TText},
			{Name: "schema_version", Type: TText},
			{Name: "property_id", Type: TText},
			{Name: "event_time", Type: TTimestamptz},
			{Name: "sequence", Type: TInt8},
			{Name: "status", Type: TText, NotNull: true},
			{Name: "reason", Type: TText},
			{Name: "payload", Type: TJSONB},
			{Name: "received_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		},
		PK: []string{"event_id"},
		Indexes: []Index{
			{Name: "mdm_inbox_pkey", Keys: []IndexKey{{Field: "event_id"}}, Unique: true},
			{Name: "mdm_inbox_received_idx", Keys: []IndexKey{{Field: "received_at", Desc: true}}},
		},
		Checks: []Check{
			{Name: "mdm_inbox_status_check", Conds: []Cond{{Col: "status", Op: "in", Val: []any{"applied", "ignored", "rejected"}}}},
		},
	},
	{
		// The local read-only copy of the CMS's properties (CMS is the source of truth for property_id).
		Name: "mdm_property",
		Columns: []Column{
			{Name: "property_id", Type: TText, NotNull: true},
			{Name: "name", Type: TText, NotNull: true},
			{Name: "city", Type: TText},
			{Name: "state", Type: TText},
			{Name: "status", Type: TText, NotNull: true},
			{Name: "last_event_at", Type: TTimestamptz}, // the CMS time of the newest state applied
			{Name: "last_event_id", Type: TText},
			{Name: "last_sequence", Type: TInt8},
			{Name: "source", Type: TText, NotNull: true}, // event | reconcile
			{Name: "updated_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		},
		PK: []string{"property_id"},
		Indexes: []Index{
			{Name: "mdm_property_pkey", Keys: []IndexKey{{Field: "property_id"}}, Unique: true},
		},
		Checks: []Check{
			{Name: "mdm_property_status_check", Conds: []Cond{{Col: "status", Op: "in", Val: []any{"Active", "Inactive"}}}},
			{Name: "mdm_property_source_check", Conds: []Cond{{Col: "source", Op: "in", Val: []any{"event", "reconcile"}}}},
		},
	},
	{
		// The CMS connection (one document, id 1).
		Name: "mdm_setting",
		Columns: []Column{
			{Name: "id", Type: TInt4, NotNull: true},
			{Name: "inbound_secret", Type: TBytea}, // sealed
			{Name: "inbound_secret_last4", Type: TText},
			{Name: "cms_base_url", Type: TText},
			{Name: "cms_api_key", Type: TBytea}, // sealed
			{Name: "cms_api_key_last4", Type: TText},
			{Name: "properties_from_cms", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
			{Name: "updated_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
			{Name: "updated_by", Type: TText},
		},
		PK:     []string{"id"},
		IDIsPK: true,
		Checks: []Check{
			{Name: "mdm_setting_id_check", Conds: []Cond{{Col: "id", Op: "=", Val: float64(1)}}},
		},
	},
	{
		// One document per reconciliation (SOP §6).
		Name: "mdm_reconcile_run",
		Columns: []Column{
			{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
			{Name: "at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
			{Name: "finished_at", Type: TTimestamptz},
			{Name: "trigger", Type: TText, NotNull: true},
			{Name: "checked", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}},
			{Name: "fixed", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}},
			{Name: "errors", Type: TJSONB, NotNull: true, Default: Default{Kind: DefConst, Value: "[]"}},
			{Name: "employees_checked", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}},
			{Name: "employees_fixed", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}},
			{Name: "properties_checked", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}},
			{Name: "properties_fixed", Type: TInt8, NotNull: true, Default: Default{Kind: DefConst, Value: int64(0)}},
		},
		PK:     []string{"id"},
		IDIsPK: true,
		Indexes: []Index{
			{Name: "mdm_reconcile_run_at_idx", Keys: []IndexKey{{Field: "at", Desc: true}}},
		},
		Checks: []Check{
			{Name: "mdm_reconcile_run_trigger_check", Conds: []Cond{{Col: "trigger", Op: "in", Val: []any{"nightly", "manual"}}}},
		},
	},
	{
		// Small state documents: the watcher's lease and resume token ("watcher"), the outbox position counter
		// ("pos"), the nightly reconciliation's last day ("reconcile_nightly").
		Name: "mdm_state",
		Columns: []Column{
			{Name: "id", Type: TText, NotNull: true},
			{Name: "owner", Type: TText},
			{Name: "expires_at", Type: TTimestamptz},
			{Name: "token", Type: TText},
			{Name: "n", Type: TInt8},
			{Name: "day", Type: TText},
			{Name: "updated_at", Type: TTimestamptz},
		},
		PK:     []string{"id"},
		IDIsPK: true,
	},
}

// MDMTables lists the master data sync collections (not part of Tables()).
func MDMTables() []*Table { return mdmTables }

// Registered while the package's variables are initialised, before schema.go's init builds the column maps, so
// location's new column is part of the registry from the start.
var _ = registerMDM()

func registerMDM() bool {
	for _, t := range schemaTables {
		if t.Name == "location" && t.cols == nil {
			t.Columns = append(t.Columns, Column{Name: "property_id", Type: TText})
			t.Indexes = append(t.Indexes, locationPropertyIndex)
		}
	}
	for _, t := range mdmTables {
		t.cols = map[string]*Column{}
		for i := range t.Columns {
			t.cols[t.Columns[i].Name] = &t.Columns[i]
		}
		tableByName[t.Name] = t
	}
	return true
}

// migrateMDM is migration 003: the mdm_* collections and their indexes, and location.property_id (null on every
// existing location, so documents keep every column; unique when set).
func migrateMDM(ctx context.Context, s *Store) error {
	db := s.DB.db
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, t := range mdmTables {
		if !have[t.Name] {
			if err := db.CreateCollection(ctx, t.Name); err != nil {
				var ce mongo.CommandError
				if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
					return fmt.Errorf("create %s: %w", t.Name, err)
				}
			}
		}
		if err := mdmCreateIndexes(ctx, db, t.Name, t.Indexes); err != nil {
			return err
		}
	}
	if _, err := db.Collection("location").UpdateMany(ctx, bson.D{{Key: "property_id", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "property_id", Value: nil}}}}); err != nil {
		return fmt.Errorf("location.property_id: %w", err)
	}
	return mdmCreateIndexes(ctx, db, "location", []Index{locationPropertyIndex})
}

func mdmCreateIndexes(ctx context.Context, db *mongo.Database, coll string, ixs []Index) error {
	var models []mongo.IndexModel
	for _, ix := range ixs {
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
		return nil
	}
	if _, err := db.Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
		return fmt.Errorf("indexes of %s: %w", coll, err)
	}
	return nil
}
