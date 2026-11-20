package store

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Migration 008 (as Reset applies it): the Uniform section of Form setup, the size types of the seeded uniform items
// and the uniform_size collection. Running it again changes nothing, keeps HR's choices and never touches a uniform
// issue or a pay line.
func TestUniformMigration(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	for _, f := range UniformFields {
		r, _ := q.C("onb_field").FindOne(ctx, bson.M{"key": f.Key})
		if r == nil || r["section"] != UniformSection || r["kind"] != "sel" || r["filled_by"] != FilledByJoiner || r["join_state"] != int16(1) || r["offer_state"] != int16(0) {
			t.Fatalf("%s: %v", f.Key, r)
		}
		if !JoinerAllowed(f.Key, UniformSection, "sel", false, r["options"].(string)) || DefaultFilledBy(f.Key, UniformSection, "sel", false, "S,M") != FilledByJoiner {
			t.Fatalf("%s is the joiner's by default", f.Key)
		}
	}
	if r, _ := q.C("onb_field").FindOne(ctx, bson.M{"key": "uniWaist"}); r["options"] != "26,28,30,32,34,36,38,40,42,44" {
		t.Fatalf("waist options %v", r["options"])
	}
	if r, _ := q.C("onb_field").FindOne(ctx, bson.M{"key": "uniShoe"}); r["options"] != "4,5,6,7,8,9,10,11,12" {
		t.Fatalf("shoe options %v", r["options"])
	}
	for name, want := range map[string]any{"Shirt": SizeShirt, "Trousers": SizeTrouser, "Shoes": SizeShoes, "Apron": nil, "Name badge": nil} {
		if r, _ := q.C("inv_item").FindOne(ctx, bson.M{"name": name}); r == nil || r["size_type"] != want {
			t.Fatalf("%s: %v, want %v", name, r["size_type"], want)
		}
	}
	// HR's choices stay; a missing field comes back; issues and pay lines are not touched.
	if _, err := q.C("onb_field").Update(ctx, bson.M{"key": "uniShirt"}, bson.M{"$set": bson.M{"filled_by": FilledByHR, "join_state": 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("inv_item").Update(ctx, bson.M{"name": "Apron"}, bson.M{"$set": bson.M{"size_type": SizeOther}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("onb_field").Delete(ctx, bson.M{"key": "uniShoe"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("employee").Raw().InsertOne(ctx, bson.M{"_id": "UNI-T1", "name": "Test", "type": "Permanent", "doj": "2026-01-01", "status": "Active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("ledger").Raw().InsertOne(ctx, bson.M{"_id": int64(990001), "employee_id": "UNI-T1", "ym": "2026-07", "kind": "earn", "label": "Uniform deposit refund", "amount": 900.0,
		"source_id": "UNI-1", "void": false}); err != nil {
		t.Fatal(err)
	}
	before, _ := q.C("ledger").Find(ctx, bson.M{"employee_id": "UNI-T1"})
	if err := migrateUniform(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := migrateUniform(ctx, s); err != nil {
		t.Fatal("run twice: ", err)
	}
	if r, _ := q.C("onb_field").FindOne(ctx, bson.M{"key": "uniShirt"}); r["filled_by"] != FilledByHR || r["join_state"] != int16(2) {
		t.Fatalf("HR's choice stays: %v", r)
	}
	if n, _ := q.C("onb_field").Count(ctx, bson.M{"key": "uniShoe"}); n != 1 {
		t.Fatalf("uniShoe back once: %d", n)
	}
	if r, _ := q.C("inv_item").FindOne(ctx, bson.M{"name": "Apron"}); r["size_type"] != SizeOther {
		t.Fatal("a tagged item stays")
	}
	after, _ := q.C("ledger").Find(ctx, bson.M{"employee_id": "UNI-T1"})
	if len(after) != 1 || len(before) != 1 || after[0]["ym"] != before[0]["ym"] || after[0]["void"] != false {
		t.Fatalf("pay lines untouched: %v", after)
	}
	// The collection: one document per employee, a size type check on items.
	if _, err := q.C(UniformSizeColl).Insert(ctx, bson.M{"employee_id": "UNI-T1", "shirt": "M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C(UniformSizeColl).Insert(ctx, bson.M{"employee_id": "UNI-T1", "shirt": "L"}); err == nil {
		t.Fatal("one size document per employee")
	}
	if _, err := q.C("inv_item").Update(ctx, bson.M{"name": "Apron"}, bson.M{"$set": bson.M{"size_type": "hat"}}); err == nil {
		t.Fatal("size_type check")
	}
	// Names that say what an item is.
	for name, want := range map[string]string{"T-Shirt": SizeShirt, "Polo shirt": SizeShirt, "Black trousers": SizeTrouser, "Cargo pants": SizeTrouser,
		"Safety shoes": SizeShoes, "Cap": "", "Name badge": ""} {
		if got := GuessSizeType(name); got != want {
			t.Fatalf("%s: %q, want %q", name, got, want)
		}
	}
	if l := DefaultUniformSizes(); strings.Join(l["shirt"], ",") != "XS,S,M,L,XL,XXL,3XL,4XL" || strings.Join(l["length"], ",") != "28,29,30,31,32,33,34,35,36" {
		t.Fatal(l)
	}
}
