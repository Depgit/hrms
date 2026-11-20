package store

// Registry additions for the joiner's self-fill link (owner's decision of 11 Nov 2026: the new joiner fills their
// own joining details and uploads their documents from a link HR sends; HR picks per field who fills it):
//
//   - onb_field.filled_by and onb_document.filled_by: "joiner" or "hr". Null on rows that came from an import
//     before it was set; the app then reads the default (DefaultFilledBy), and every start fills it in.
//   - joining_link: one document per link sent for an offer. The token is kept only as a SHA-256 hash; at most one
//     link per offer is live (live_offer, unique while set). The 6-digit code and the joiner's session are kept
//     on it as hashes too.
//
// joining_link never existed in PostgreSQL, so it is kept out of schemaTables like the mdm_* collections: Tables()
// (and with it `hrms import-pg`) does not list it, while C() still applies its rules.

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// JoiningLinkColl holds the joiner links.
const JoiningLinkColl = "joining_link"

// Who fills a field or document of the joining form.
const (
	FilledByJoiner = "joiner"
	FilledByHR     = "hr"
)

var filledByCheck = Cond{Col: "filled_by", Op: "in", Val: []any{FilledByJoiner, FilledByHR}}

var joiningLinkTable = &Table{
	Name: JoiningLinkColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "offer_id", Type: TText, NotNull: true},
		{Name: "live_offer", Type: TText}, // = offer_id while this is the offer's current link; null once replaced or stopped
		{Name: "token_hash", Type: TText, NotNull: true},
		{Name: "sent_to_email", Type: TText},
		{Name: "sent_to_mobile", Type: TText},
		// via: email, copy, whatsapp, renew, reopen.
		{Name: "via", Type: TText, Array: true, NotNull: true, Default: Default{Kind: DefConst, Value: []string{}}},
		// note: HR's note when the form was reopened for the joiner.
		{Name: "note", Type: TText},
		{Name: "created_by", Type: TText, NotNull: true},
		{Name: "created_by_name", Type: TText, NotNull: true},
		{Name: "created_role", Type: TText},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "expires_at", Type: TTimestamptz, NotNull: true},
		{Name: "sent_at", Type: TTimestamptz}, // last emailed
		{Name: "opened_at", Type: TTimestamptz},
		{Name: "verified_at", Type: TTimestamptz}, // first correct code
		{Name: "code_hash", Type: TText},
		{Name: "code_by", Type: TText}, // email | phone (HR read it out) | dev
		{Name: "code_sent_at", Type: TTimestamptz},
		{Name: "code_expires_at", Type: TTimestamptz},
		{Name: "code_tries", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
		{Name: "code_locked", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
		{Name: "code_locks", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
		{Name: "code_sends", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
		{Name: "code_window_at", Type: TTimestamptz},
		// code_fails: wrong codes entered on this link in all, over every code (15 lock the link). A try takes one
		// before its code is compared; a right code gives it back.
		{Name: "code_fails", Type: TInt4, NotNull: true, Default: Default{Kind: DefConst, Value: int32(0)}},
		// asked_at: the joiner last asked HR for a code (no email: HR gives it on the phone).
		{Name: "asked_at", Type: TTimestamptz},
		{Name: "session_hash", Type: TText},
		{Name: "session_expires_at", Type: TTimestamptz},
		{Name: "joiner_keys", Type: TText, Array: true, NotNull: true, Default: Default{Kind: DefConst, Value: []string{}}},
		{Name: "saved_at", Type: TTimestamptz},
		{Name: "submitted_at", Type: TTimestamptz},
		{Name: "locked_at", Type: TTimestamptz}, // too many wrong codes: HR sends a new link
		{Name: "revoked_at", Type: TTimestamptz},
		{Name: "revoked_by", Type: TText},
		{Name: "revoke_reason", Type: TText},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "joining_link_token_hash_key", Keys: []IndexKey{{Field: "token_hash"}}, Unique: true},
		{Name: "joining_link_live_offer_key", Keys: []IndexKey{{Field: "live_offer"}}, Unique: true,
			Partial: bson.D{{Key: "live_offer", Value: bson.D{{Key: "$type", Value: "string"}}}}},
		{Name: "joining_link_offer_idx", Keys: []IndexKey{{Field: "offer_id"}, {Field: "created_at", Desc: true}}},
	},
	FKs: []FK{
		{Name: "joining_link_offer_id_fkey", Col: "offer_id", RefTable: "offer", RefCol: "id"},
	},
	Checks: []Check{
		{Name: "joining_link_code_by_check", Conds: []Cond{{Col: "code_by", Op: "in", Val: []any{"email", "phone", "dev"}}}},
	},
}

// Registered while the package's variables are initialised, before schema.go's init builds the column maps, so the
// new columns are part of the registry from the start.
var _ = registerJoining()

func registerJoining() bool {
	for _, t := range schemaTables {
		if (t.Name == "onb_field" || t.Name == "onb_document") && t.cols == nil {
			t.Columns = append(t.Columns, Column{Name: "filled_by", Type: TText})
			t.Checks = append(t.Checks, Check{Name: t.Name + "_filled_by_check", Conds: []Cond{filledByCheck}})
		}
	}
	t := joiningLinkTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t
	if off := tableByName["offer"]; off != nil {
		off.refBy = append(off.refBy, refFrom{t, t.FKs[0]})
	}
	return true
}

// joinerSections are the form sections the joiner fills by default; the rest (Job, Location and reporting, Hiring,
// Compensation) are HR's.
var joinerSections = map[string]bool{"Personal": true, "Identity and statutory": true, "Bank": true, "Benefits": true}

// hrOnlyKeys are fields HR always fills: they pick from the company's own lists (grades, people, shifts,
// attendance modes, salary structures) or set the job.
var hrOnlyKeys = map[string]bool{"grade": true, "l1": true, "l2": true, "shift": true, "attMode": true, "structId": true,
	"type": true, "dept": true, "desig": true, "city": true, "loc": true, "endDate": true, "agency": true}

// JoinerAllowed says whether a field may be given to the joiner: never a system field, the approval flow or work
// policy, anything in Compensation, or a field that picks from the company's lists (hrOnlyKeys). A drop-down needs
// its own options (or is the qualification list, which the joiner's form loads).
func JoinerAllowed(key, section, kind string, sys bool, options string) bool {
	if sys || kind == "flow" || kind == "policy" || section == "Compensation" || hrOnlyKeys[key] {
		return false
	}
	return kind != "sel" || options != "" || key == "qual"
}

// DefaultFilledBy is who fills a field when Form setup has not said: the joiner for the sections Personal,
// Identity and statutory, Bank and Benefits (when the field may be theirs at all), HR otherwise.
func DefaultFilledBy(key, section, kind string, sys bool, options string) string {
	if joinerSections[section] && JoinerAllowed(key, section, kind, sys, options) {
		return FilledByJoiner
	}
	return FilledByHR
}

// FillJoinerDefaults sets filled_by on every onb_field and onb_document row that has none: fields by section
// (DefaultFilledBy), documents to the joiner. It returns how many rows it set. Rows already set are left alone.
func FillJoinerDefaults(ctx context.Context, q Q) (int, error) {
	n := 0
	rows, err := q.C("onb_field").Find(ctx, bson.M{"filled_by": nil})
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		key, _ := r["key"].(string)
		section, _ := r["section"].(string)
		kind, _ := r["kind"].(string)
		opts, _ := r["options"].(string)
		sys, _ := r["sys"].(bool)
		if _, err := q.C("onb_field").Update(ctx, bson.M{"key": key, "filled_by": nil},
			bson.M{"$set": bson.M{"filled_by": DefaultFilledBy(key, section, kind, sys, opts)}}); err != nil {
			return n, err
		}
		n++
	}
	m, err := q.C("onb_document").UpdateMany(ctx, bson.M{"filled_by": nil}, bson.M{"$set": bson.M{"filled_by": FilledByJoiner}})
	return n + int(m), err
}

// migrateJoining is migration 006: the joining_link collection with its indexes, and filled_by on every Form setup
// row (FillJoinerDefaults).
func migrateJoining(ctx context.Context, s *Store) error {
	if err := ensureJoiningLink(ctx, s.DB.db); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		_, err := FillJoinerDefaults(ctx, tx)
		return err
	})
}

func ensureJoiningLink(ctx context.Context, db *mongo.Database) error {
	names, err := db.ListCollectionNames(ctx, bson.D{{Key: "name", Value: JoiningLinkColl}})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := db.CreateCollection(ctx, JoiningLinkColl); err != nil {
			var ce mongo.CommandError
			if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
				return fmt.Errorf("create %s: %w", JoiningLinkColl, err)
			}
		}
	}
	return mdmCreateIndexes(ctx, db, JoiningLinkColl, joiningLinkTable.Indexes)
}

// healJoining fills in filled_by on Form setup rows that have none (rows an import brought in after migration 006),
// and code_fails on links made before it existed. Migrate runs it on every start; it is cheap when nothing is missing.
func healJoining(ctx context.Context, s *Store, log func(string)) error {
	if err := ensureJoiningLink(ctx, s.DB.db); err != nil {
		return err
	}
	if _, err := s.DB.C(JoiningLinkColl).Raw().UpdateMany(ctx, bson.M{"code_fails": bson.M{"$exists": false}}, bson.M{"$set": bson.M{"code_fails": int32(0)}}); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx Q) error {
		n, err := FillJoinerDefaults(ctx, tx)
		if err == nil && n > 0 {
			log(fmt.Sprintf("set who fills %d Form setup item(s) (joiner or HR)", n))
		}
		return err
	})
}
