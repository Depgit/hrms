package api_test

// Offer at joining (offer_at_joining.go): choose → raise on the joining day → Mark joined refused before the offer's
// approval → approve → Mark joined: the offer is accepted (Offer ID), frozen and its letter released at joining.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOfferAtJoining(t *testing.T) {
	c, srv, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")

	si := selectCandidate(t, c, shortlistBody(st, "Ravi", "Kumar", "9876503001"))
	if x := hireCand(t, c, si); x["can"].(map[string]any)["offerAtJoining"] != true {
		t.Fatalf("a selected candidate can take Offer at joining: %v", x["can"])
	}
	if s, _ := c.do("POST", "/api/candidates/"+si+"/offer-at-joining", "EMP-1004", "HRBP", map[string]any{"date": hireDay(-1)}); s != 400 {
		t.Fatal("the expected joining date is today or later")
	}
	if s, r := c.do("POST", "/api/candidates/"+si+"/offer-at-joining", "EMP-1004", "HRBP", map[string]any{"date": today(), "note": "UT hire"}); s != 200 {
		t.Fatalf("choose: %v", r)
	}
	x := hireCand(t, c, si)
	if x["bucket"] != "Offer at joining" || !strings.Contains(fmt.Sprint(x["status"]), "waiting for the joining day") {
		t.Fatalf("waiting for the joining day: %v %v", x["bucket"], x["status"])
	}
	if _, p := c.do("GET", "/api/candidates?stage=Offer+at+joining", "EMP-1004", "HRBP", nil); p["counts"].(map[string]any)["Offer at joining"] != 1.0 {
		t.Fatalf("its own pipeline count: %v", p["counts"])
	}
	if s, _ := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 400 {
		t.Fatal("no Mark joined before the offer is raised")
	}

	// Raise offer at joining: the same offer form, then Offer approval as usual.
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatalf("aadhaar: %v", r)
	}
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": offerData(st, "Ravi", "Kumar", "9876503001"), "candidate": si, "atJoining": true})
	if s != 200 {
		t.Fatalf("raise at joining: %v", r)
	}
	oi, req := r["id"].(string), r["request"].(map[string]any)["id"].(string)
	s, r = c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Waiting for offer approval") {
		t.Fatalf("Mark joined waits for the approval: %d %v", s, r)
	}
	ui3ApproveAll(t, c, req)
	if s, _ := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil); s != 400 {
		t.Fatal("an offer at joining is not released")
	}
	if res, err := srv.HireSweep(ctx, time.Now().AddDate(0, 0, 30)); err != nil || res.Lapsed != 0 {
		t.Fatalf("never lapses: %+v %v", res, err)
	}
	x = hireCand(t, c, si)
	if x["bucket"] != "Offer at joining" || x["can"].(map[string]any)["markJoined"] != true {
		t.Fatalf("approved: Mark joined: %v %v", x["bucket"], x["can"])
	}

	// Mark joined: joining is the acceptance.
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || !strings.HasPrefix(fmt.Sprint(r["id"]), "JI-") {
		t.Fatalf("joined: %d %v", s, r)
	}
	o, _ := st.DB.C("offer").FindOne(ctx, bson.D{{Key: "_id", Value: oi}})
	if o["accepted_at"] == nil || o["offer_no"] != oi || o["letter_at"] != "joining" || o["letter_released_at"] == nil || o["at_joining"] != true {
		t.Fatalf("accepted, Offer ID, letter released: %v", o)
	}
	v, _ := st.DB.C("offer_version").FindOne(ctx, bson.D{{Key: "candidate_id", Value: si}, {Key: "version", Value: int32(1)}})
	if v["status"] != "Accepted" || v["frozen"] != true {
		t.Fatalf("v1 frozen: %v", v)
	}
	x = hireCand(t, c, si)
	acts := hireActions(x)
	for _, want := range []string{"Offer at joining chosen", "Offer v1 raised at joining", "Offer v1 approved", "Offer accepted at joining: Offer ID " + oi, "Offer letter released at joining", "Joined"} {
		if !strings.Contains(acts, want) {
			t.Fatalf("timeline %q: %s", want, acts)
		}
	}
	if x["stage"] != "Joined" || x["offer_no"] != oi {
		t.Fatalf("joined: %v %v", x["stage"], x["offer_no"])
	}

	// Switching back: the usual offer raised instead clears the choice.
	si2 := selectCandidate(t, c, shortlistBody(st, "Sunil", "Verma", "9876503002"))
	c.do("POST", "/api/candidates/"+si2+"/offer-at-joining", "EMP-1004", "HRBP", map[string]any{"date": hireDay(3)})
	if s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": offerData(st, "Sunil", "Verma", "9876503002"), "candidate": si2, "atJoining": true}); s != 400 {
		t.Fatalf("raised on the joining day, not before: %v", r)
	}
	approvedOffer(t, c, st, si2, "Sunil", "Verma", "9876503002")
	if x := hireCand(t, c, si2); x["bucket"] != "Offer" || x["oaj_at"] != nil || !strings.Contains(hireActions(x), "Switched back to the usual offer") {
		t.Fatalf("switched back: %v %v", x["bucket"], hireActions(x))
	}
}
