package store

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// SysKey is a master item the app writes itself (a change reason or an attendance mode). The app finds it by its
// fixed key, so HR can rename it in Masters; what it is used for cannot change.
type SysKey struct {
	Key, Kind, UsedFor, Name string // Name: the name it is created with
	Why                      string // what the app uses it for
}

const sepWhy = "separation cases record it"

// SysKeys is the list migration 030 also sets on an existing database.
var SysKeys = []SysKey{
	{"reason.transfer", "change_reason", "Employee update", "Transfer", "approved transfers are recorded with it"},
	{"reason.excess_recovery", "change_reason", "Deduction", "Excess payment recovery", "salary recoveries from Bulk update use it, and it needs no hearing"},
	{"reason.absent_hold", "change_reason", "Hold", "Absent without notice", "absconding cases hold the salary with it"},
	{"sep:Resignation", "change_reason", "Separation", "Resigned", sepWhy},
	{"sep:Termination", "change_reason", "Separation", "Terminated", sepWhy},
	{"sep:Absconding", "change_reason", "Separation", "Absconded", sepWhy},
	{"sep:Contract end", "change_reason", "Separation", "Contract ended", sepWhy},
	{"sep:Retirement", "change_reason", "Separation", "Retired", sepWhy},
	{"sep:Death", "change_reason", "Separation", "Death or permanent disability", sepWhy},
	{"mode.default", "attendance_mode", "", "Biometric", "new joiners get it when no mode is chosen"},
}

// AssignSysKeys gives each listed item its key, once (after seeding a new database): the first-created master item
// of that kind, use and name, unless some item already has the key.
func AssignSysKeys(ctx context.Context, q Q) error {
	sm := q.C("simple_master")
	for _, k := range SysKeys {
		if taken, err := sm.Exists(ctx, bson.D{{Key: "sys_key", Value: k.Key}}); err != nil || taken {
			if err != nil {
				return err
			}
			continue
		}
		var usedFor any = k.UsedFor
		if k.UsedFor == "" { // COALESCE(used_for,'') = ''
			usedFor = bson.D{{Key: "$in", Value: bson.A{nil, ""}}}
		}
		first, err := sm.FindOne(ctx, bson.D{{Key: "kind", Value: k.Kind}, {Key: "used_for", Value: usedFor}, {Key: "name", Value: k.Name}},
			options.FindOne().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "seq", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return err
		}
		if first == nil {
			continue
		}
		if _, err := sm.Update(ctx, bson.D{{Key: "_id", Value: first["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "sys_key", Value: k.Key}}}}); err != nil {
			return err
		}
	}
	return nil
}

// SysKeyWhy says what the app uses a keyed item for.
func SysKeyWhy(key string) string {
	for _, k := range SysKeys {
		if k.Key == key {
			return k.Why
		}
	}
	return ""
}

// SysName is the current name of a keyed item (whatever HR renamed it to), or the name it was created with.
func SysName(ctx context.Context, q Q, key string) string {
	r, err := q.C("simple_master").FindOne(ctx, bson.D{{Key: "sys_key", Value: key}}, options.FindOne().SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err == nil && r != nil {
		if n, _ := r["name"].(string); n != "" {
			return n
		}
	}
	for _, k := range SysKeys {
		if k.Key == key {
			return k.Name
		}
	}
	return ""
}
