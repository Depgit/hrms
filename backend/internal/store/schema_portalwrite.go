package store

// Registry additions for sending employees to the Realtime portal (17 Nov 2026; api/portal_write.go): HR presses
// "Send to portal" and confirms a preview; nothing is sent by itself.
//
//   - employee.portal_sync (jsonb): the last confirmed send for the person: state (on, left, check = the reply was
//     not understood), code, action, at, by, hash (of the HRMS details sent, to show "changed since last send"),
//     values (those details) and message (the portal's reply). Null: never sent from the HRMS.
//   - company_setting.portal_write (jsonb): code_start (the first number "next free code" may give), card_pad (digits
//     of the card number, 8 like the portal's 00001050; 0 = the code as it is), since (when sending started: exits
//     before it are not listed to mark left), and lists (the portal form's departments, designations, branches and
//     shifts as last read, with when).
//   - portal_map (new): HR's one-time mapping of HRMS values to the portal's list choices: kind (dept, desig, branch,
//     shift), ref (the HRMS department, designation, property id or shift code) and value (the portal's choice,
//     as its text).
//   - portal_send (new): every confirmed send: employee_id, action, code, ok, unclear, the fields sent, the portal's
//     message, at and by. Never changed after it is written.
//
// Migration 016 creates the two collections and their indexes, gives HR Admin the new right att.portal (Edit) and
// records when sending started. Nothing existing is rewritten.

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// PortalMapColl and PortalSendColl hold the mapping and the send log.
const (
	PortalMapColl  = "portal_map"
	PortalSendColl = "portal_send"
)

var portalMapTable = &Table{
	Name: PortalMapColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "kind", Type: TText, NotNull: true},
		{Name: "ref", Type: TText, NotNull: true},
		{Name: "value", Type: TText, NotNull: true},
		{Name: "updated_by", Type: TText},
		{Name: "updated_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:      []string{"id"},
	IDIsPK:  true,
	Indexes: []Index{{Name: "portal_map_key", Keys: []IndexKey{{Field: "kind"}, {Field: "ref"}}, Unique: true}},
	Checks:  []Check{{Name: "portal_map_kind_check", Conds: []Cond{{Col: "kind", Op: "in", Val: []any{"dept", "desig", "branch", "shift"}}}}},
}

var portalSendTable = &Table{
	Name: PortalSendColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "employee_id", Type: TText, NotNull: true},
		{Name: "action", Type: TText, NotNull: true},
		{Name: "code", Type: TText},
		{Name: "ok", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
		{Name: "unclear", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: false}},
		{Name: "sent", Type: TJSONB},
		{Name: "message", Type: TText},
		{Name: "by", Type: TText},
		{Name: "at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
	},
	PK:      []string{"id"},
	IDIsPK:  true,
	Indexes: []Index{{Name: "portal_send_employee_idx", Keys: []IndexKey{{Field: "employee_id"}, {Field: "at"}}}},
	FKs:     []FK{{Name: "portal_send_employee_id_fkey", Col: "employee_id", RefTable: "employee", RefCol: "id"}},
	Checks:  []Check{{Name: "portal_send_action_check", Conds: []Cond{{Col: "action", Op: "in", Val: []any{"create", "update", "left"}}}}},
}

var portalTables = []*Table{portalMapTable, portalSendTable}

var _ = registerPortalWrite()

func registerPortalWrite() bool {
	byName := map[string]*Table{}
	for _, t := range schemaTables {
		byName[t.Name] = t
		if t.cols != nil {
			continue
		}
		switch t.Name {
		case "employee":
			t.Columns = append(t.Columns, Column{Name: "portal_sync", Type: TJSONB})
		case "company_setting":
			t.Columns = append(t.Columns, Column{Name: "portal_write", Type: TJSONB})
		}
	}
	for _, t := range portalTables {
		t.cols = map[string]*Column{}
		for i := range t.Columns {
			t.cols[t.Columns[i].Name] = &t.Columns[i]
		}
		tableByName[t.Name] = t
		byName[t.Name] = t
	}
	for _, t := range portalTables {
		for _, fk := range t.FKs {
			if rt := byName[fk.RefTable]; rt != nil {
				rt.refBy = append(rt.refBy, refFrom{t, fk})
			}
		}
	}
	migrations = append(migrations, migration{"016_portal_write", false, migratePortalWrite})
	return true
}

// migratePortalWrite is migration 016: the portal_map and portal_send collections, HR Admin's att.portal right
// (HR Admin has every right anyway; the row shows it in Roles and rights) and the start of sending. Additive only.
func migratePortalWrite(ctx context.Context, s *Store) error {
	for _, t := range portalTables {
		if err := ensureColl(ctx, s.DB.db, t.Name, t.Indexes); err != nil {
			return err
		}
	}
	q := s.DB
	if ok, err := q.C("role").Exists(ctx, bson.M{"name": "HR Admin"}); err != nil {
		return err
	} else if ok {
		if _, err := q.C("role_right").Upsert(ctx, bson.M{"role_name": "HR Admin", "right_id": "att.portal"}, nil, bson.M{"level": 2}); err != nil {
			return err
		}
	}
	cs, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1})
	if err != nil || cs == nil {
		return err
	}
	switch cs["portal_write"].(type) {
	case map[string]any, bson.M, bson.D:
		return nil
	}
	_, err = q.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"portal_write": map[string]any{"since": time.Now().Format("2006-01-02")}}})
	return err
}
