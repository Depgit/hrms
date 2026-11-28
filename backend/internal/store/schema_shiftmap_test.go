package store

import (
	"sort"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Migration 029: a shift's locations become Department + Location rows with no change in who sees what. All locations
// (none listed) → All departments at all locations; each listed location → All departments at it; an id that is no
// location matched nobody and is skipped; a shift with rows is left alone; a second run adds nothing.
func TestShiftMapsFromLocations(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	_, _, city := orgChain(t, ctx, q)
	cluster := mustInsert(t, ctx, q, "cluster", bson.M{"name": "Delhi 1", "city_id": city}).(string)
	l1 := mustInsert(t, ctx, q, "location", bson.M{"name": "Saket", "cluster_id": cluster}).(string)
	l2 := mustInsert(t, ctx, q, "location", bson.M{"name": "Noida", "cluster_id": cluster}).(string)
	gone := "00000000-0000-4000-8000-000000000009"
	all := mustInsert(t, ctx, q, "shift", bson.M{"code": "G", "name": "General", "start_time": "09:00", "end_time": "18:00"}).(string)
	some := mustInsert(t, ctx, q, "shift", bson.M{"code": "N", "name": "Night", "start_time": "22:00", "end_time": "06:00", "location_ids": []string{l1, l2, gone, l1}}).(string)
	none := mustInsert(t, ctx, q, "shift", bson.M{"code": "X", "name": "Nowhere", "start_time": "10:00", "end_time": "19:00", "location_ids": []string{}}).(string)
	kept := mustInsert(t, ctx, q, "shift", bson.M{"code": "K", "name": "Kept", "start_time": "10:00", "end_time": "19:00"}).(string)
	mustInsert(t, ctx, q, ShiftMapColl, bson.M{"shift_id": kept, "location_id": l2, "on_map": false})

	n, err := ShiftMapsFromLocations(ctx, q)
	if err != nil || n != 3 {
		t.Fatalf("rows added: %d %v, want 3", n, err)
	}
	rows := func(shift string) []string {
		t.Helper()
		l, err := q.C(ShiftMapColl).Find(ctx, bson.M{"shift_id": shift})
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range l {
			d, _ := r["department_id"].(string)
			loc, _ := r["location_id"].(string)
			out = append(out, d+"|"+loc+"|"+map[bool]string{true: "on", false: "off"}[r["on_map"] == true])
		}
		sort.Strings(out)
		return out
	}
	want := []string{"|" + l1 + "|on", "|" + l2 + "|on"}
	sort.Strings(want)
	if got := rows(all); len(got) != 1 || got[0] != "||on" {
		t.Errorf("all locations: %v", got)
	}
	if got := rows(some); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("listed locations: %v, want %v", got, want)
	}
	if got := rows(none); len(got) != 0 {
		t.Errorf("an empty list matched nobody: %v", got)
	}
	if got := rows(kept); len(got) != 1 || got[0] != "|"+l2+"|off" {
		t.Errorf("a shift with rows is left alone: %v", got)
	}
	if n, err := ShiftMapsFromLocations(ctx, q); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
	// The same pair twice on a shift is refused by the index.
	if _, err := q.C(ShiftMapColl).Insert(ctx, bson.M{"shift_id": all}); err == nil {
		t.Fatal("All at all twice on one shift")
	}
}
