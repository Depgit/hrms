package store

// Where a shift applies, by Department + Location (owner, 24 Nov 2026: "Shift applies to Location and Department"):
//
//   - shift_map (new): one row per department and location pair a shift applies to. department_id null = all
//     departments, location_id null = all locations: "Front Office at Gurgaon office", "All departments at Noida",
//     "All departments at all locations". on_map false = switched off (kept; it can be switched on again). A shift is
//     offered to, and valid for, an employee when one of its switched-on rows matches their department and location;
//     a shift without a switched-on row is offered to nobody. The same pair twice on a shift is refused (unique index).
//     changed_by / changed_at: who last switched the row on or off.
//   - shift.location_ids ("Locations: All / Selected locations only") is no longer read or written. It stays on the
//     existing documents as it was, so an older binary still reads what it had.
//
// Migration 029 creates shift_map and gives every shift without rows the rows of its location_ids, with no change in
// who sees what: no list (all locations) → All departments at all locations; each listed location → All departments
// at that location. ShiftMapsFromLocations does the same after a seed or an import. It runs again safely: a shift that
// already has rows is left alone.
//
// shift_map never existed in PostgreSQL, so it is kept out of schemaTables (import-pg does not list it) while C()
// applies its rules.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ShiftMapColl holds where each shift applies (department + location rows).
const ShiftMapColl = "shift_map"

var shiftMapTable = &Table{
	Name: ShiftMapColl,
	Columns: []Column{
		{Name: "id", Type: TUUID, NotNull: true, Default: Default{Kind: DefUUID}},
		{Name: "shift_id", Type: TUUID, NotNull: true},
		{Name: "department_id", Type: TUUID}, // null = all departments
		{Name: "location_id", Type: TUUID},   // null = all locations
		{Name: "on_map", Type: TBool, NotNull: true, Default: Default{Kind: DefConst, Value: true}},
		{Name: "created_by", Type: TText},
		{Name: "created_at", Type: TTimestamptz, NotNull: true, Default: Default{Kind: DefNow}},
		{Name: "changed_by", Type: TText},
		{Name: "changed_at", Type: TTimestamptz},
	},
	PK:     []string{"id"},
	IDIsPK: true,
	Indexes: []Index{
		{Name: "shift_map_pair_key", Keys: []IndexKey{{Field: "shift_id"}, {Field: "department_id"}, {Field: "location_id"}}, Unique: true},
	},
	FKs: []FK{
		{Name: "shift_map_shift_id_fkey", Col: "shift_id", RefTable: "shift", RefCol: "id"},
		{Name: "shift_map_department_id_fkey", Col: "department_id", RefTable: "department", RefCol: "id"},
		{Name: "shift_map_location_id_fkey", Col: "location_id", RefTable: "location", RefCol: "id"},
	},
}

var _ = registerShiftMap()

func registerShiftMap() bool {
	t := shiftMapTable
	t.cols = map[string]*Column{}
	for i := range t.Columns {
		t.cols[t.Columns[i].Name] = &t.Columns[i]
	}
	tableByName[t.Name] = t // a write checks the foreign keys; nothing is ever deleted from shift, department or location
	migrations = append(migrations, migration{"029_shift_map", false, func(ctx context.Context, s *Store) error {
		if err := ensureColl(ctx, s.DB.db, ShiftMapColl, shiftMapTable.Indexes); err != nil {
			return err
		}
		_, err := ShiftMapsFromLocations(ctx, s.DB)
		return err
	}})
	return true
}

// ShiftMapsFromLocations gives every shift that has no shift_map row the rows of its location_ids (see the file
// comment): All departments at all locations when it lists none, else All departments at each listed location (an id
// that is no location, or listed twice, is skipped: it matched nobody). It returns how many rows it added.
func ShiftMapsFromLocations(ctx context.Context, q Q) (int, error) {
	shifts, err := q.C("shift").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.D{{Key: "location_ids", Value: 1}}))
	if err != nil || len(shifts) == 0 {
		return 0, err
	}
	mapped, err := q.C(ShiftMapColl).Distinct(ctx, "shift_id", nil)
	if err != nil {
		return 0, err
	}
	has := map[string]bool{}
	for _, v := range mapped {
		if s, ok := v.(string); ok {
			has[s] = true
		}
	}
	locs, err := q.C("location").Distinct(ctx, "_id", nil)
	if err != nil {
		return 0, err
	}
	isLoc := map[string]bool{}
	for _, v := range locs {
		if s, ok := v.(string); ok {
			isLoc[s] = true
		}
	}
	n := 0
	for _, sh := range shifts {
		id, _ := sh["id"].(string)
		if id == "" || has[id] {
			continue
		}
		if sh["location_ids"] == nil {
			if _, err := q.C(ShiftMapColl).Insert(ctx, bson.M{"shift_id": id, "department_id": nil, "location_id": nil, "created_by": "Migration 029 (was All locations)"}); err != nil {
				return n, err
			}
			n++
			continue
		}
		seen := map[string]bool{}
		for _, l := range shiftMapStrings(sh["location_ids"]) {
			if !isLoc[l] || seen[l] {
				continue
			}
			seen[l] = true
			if _, err := q.C(ShiftMapColl).Insert(ctx, bson.M{"shift_id": id, "department_id": nil, "location_id": l, "created_by": "Migration 029 (was a selected location)"}); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// shiftMapStrings reads a stored list of ids.
func shiftMapStrings(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case bson.A:
		return shiftMapStrings([]any(l))
	}
	return nil
}
