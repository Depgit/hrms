package store

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Migration 006 (as Reset applies it): who fills each Form setup item, by section; the system fields stay HR's;
// every document is the joiner's. Rows set by hand are never changed; rows without a value (an import) get one.
func TestJoinerDefaults(t *testing.T) {
	s, ctx := testStore(t)
	q := s.DB
	want := map[string]string{
		"gender": FilledByJoiner, "dob": FilledByJoiner, "aadhaar": FilledByJoiner, "pan": FilledByJoiner, "uanAvail": FilledByJoiner,
		"bankName": FilledByJoiner, "acct": FilledByJoiner, "ifsc": FilledByJoiner, "medical": FilledByJoiner, "ecPhone": FilledByJoiner,
		"first": FilledByHR, "last": FilledByHR, // system fields
		"shift": FilledByHR, "attMode": FilledByHR, "l1": FilledByHR, "l2": FilledByHR, // Location and reporting
		"skill": FilledByHR, "qual": FilledByHR, "recruiter": FilledByHR, "gross": FilledByHR, "varPct": FilledByHR, "type": FilledByHR,
	}
	for k, v := range want {
		r, _ := q.C("onb_field").FindOne(ctx, bson.M{"key": k})
		if r == nil || r["filled_by"] != v {
			t.Fatalf("%s: %v, want %s", k, r["filled_by"], v)
		}
	}
	if n, _ := q.C("onb_document").Count(ctx, bson.M{"filled_by": bson.M{"$ne": FilledByJoiner}}); n != 0 {
		t.Fatalf("%d documents not the joiner's", n)
	}
	if n, _ := q.C("onb_field").Count(ctx, bson.M{"filled_by": nil}); n != 0 {
		t.Fatalf("%d fields without filled_by", n)
	}
	// HR's choice stays; rows without a value (as an import leaves them) get the default.
	if _, err := q.C("onb_field").Update(ctx, bson.M{"key": "skill"}, bson.M{"$set": bson.M{"filled_by": FilledByJoiner}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("onb_field").Raw().UpdateOne(ctx, bson.M{"key": "pan"}, bson.M{"$set": bson.M{"filled_by": nil}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.C("onb_document").Raw().UpdateOne(ctx, bson.M{"key": "police"}, bson.M{"$unset": bson.M{"filled_by": ""}}); err != nil {
		t.Fatal(err)
	}
	n, err := FillJoinerDefaults(ctx, q)
	if err != nil || n != 2 {
		t.Fatalf("filled %d (%v), want 2", n, err)
	}
	for k, v := range map[string]string{"skill": FilledByJoiner, "pan": FilledByJoiner} {
		if r, _ := q.C("onb_field").FindOne(ctx, bson.M{"key": k}); r["filled_by"] != v {
			t.Fatalf("%s: %v", k, r["filled_by"])
		}
	}
	if r, _ := q.C("onb_document").FindOne(ctx, bson.M{"key": "police"}); r["filled_by"] != FilledByJoiner {
		t.Fatalf("police: %v", r["filled_by"])
	}
	// The check refuses anything but joiner or hr.
	if _, err := q.C("onb_field").Update(ctx, bson.M{"key": "pan"}, bson.M{"$set": bson.M{"filled_by": "candidate"}}); err == nil {
		t.Fatal("filled_by check")
	}
	// Who may fill what.
	if JoinerAllowed("gross", "Compensation", "number", true, "") || JoinerAllowed("l1", "Location and reporting", "sel", false, "") ||
		JoinerAllowed("flow", "Job", "flow", true, "") || !JoinerAllowed("x_1", "Job", "text", false, "") || !JoinerAllowed("qual", "Job", "sel", false, "") ||
		JoinerAllowed("x_2", "Personal", "sel", false, "") {
		t.Fatal("JoinerAllowed")
	}
	// joining_link: one live link per offer, unique token hashes, the offer must exist.
	if _, err := q.C(JoiningLinkColl).Insert(ctx, bson.M{"offer_id": "OI-9999", "token_hash": "h1", "created_by": "EMP-1", "created_by_name": "A",
		"expires_at": time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("a link for an offer that does not exist")
	}
}
