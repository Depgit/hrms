package api_test

// The Shortlisting stage of Form setup (owner, 26 Nov 2026; onboarding_stages.go shortlistProblems): what is Mandatory
// at shortlisting is required on the shortlist (400 without it) and Not needed is not, for fields and documents alike;
// values and documents given there carry forward to the offer and the joining details.

import (
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func shortlistStage(t *testing.T, c *client, kind, key string, v int) {
	t.Helper()
	if s, r := c.do("PATCH", "/api/onboarding/setup/"+kind+"/"+key, "EMP-1002", "HR Admin", map[string]any{"shortlist": v}); s != 200 {
		t.Fatalf("set %s %s: %d %v", kind, key, s, r)
	}
}

func TestShortlistStage(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")

	// Defaults: what the shortlist form asked before; no document.
	_, setup := c.do("GET", "/api/onboarding/setup", "EMP-1002", "HR Admin", nil)
	want := map[string]float64{"first": 2, "loc": 2, "phone": 2, "email": 2, "dob": 2, "gender": 2, "addr": 1, "aadhaar": 0, "type": 0, "flow": 0}
	for _, x := range setup["fields"].([]any) {
		f := x.(map[string]any)
		if w, ok := want[fmt.Sprint(f["key"])]; ok && w != f["shortlist"] {
			t.Fatalf("default %v: %v", f["key"], f["shortlist"])
		}
	}
	for _, x := range setup["docs"].([]any) {
		if d := x.(map[string]any); d["shortlist"] != 0.0 {
			t.Fatalf("document default %v: %v", d["key"], d["shortlist"])
		}
	}
	// Locked: the names are always asked, the approval flow never; values are 0 to 2.
	for _, p := range [][3]any{{"fields", "first", 0}, {"fields", "flow", 1}, {"fields", "aadhaar", 3}} {
		if s, _ := c.do("PATCH", fmt.Sprintf("/api/onboarding/setup/%s/%s", p[0], p[1]), "EMP-1002", "HR Admin", map[string]any{"shortlist": p[2]}); s != 400 {
			t.Fatalf("%v accepted", p)
		}
	}

	// A field Mandatory at shortlisting is refused without it; a field Not needed is not required.
	shortlistStage(t, c, "fields", "aadhaar", 2)
	shortlistStage(t, c, "fields", "gender", 0)
	body := shortlistBody(st, "Asha", "Rao", "9876502211")
	data := body["data"].(map[string]any)
	delete(data, "gender")
	s, r := c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body)
	if errs := fmt.Sprint(r["details"]); s != 400 || !strings.Contains(errs, "Aadhaar number is required") || strings.Contains(errs, "Gender") {
		t.Fatalf("mandatory field: %d %v", s, r)
	}
	data["aadhaar"] = "123412341234"

	// A document Mandatory at shortlisting: the same, uploaded under the form's draft key.
	shortlistStage(t, c, "docs", "pan", 2)
	if s, r = c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body); s != 400 || !strings.Contains(fmt.Sprint(r["details"]), "PAN card must be uploaded with the shortlist") {
		t.Fatalf("mandatory document: %d %v", s, r)
	}
	draft := "DRAFT-SL-0123456789ab"
	if s, r := c.upload("EMP-1004", "HRBP", "offer", draft, "pan"); s != 200 {
		t.Fatalf("upload: %v", r)
	}
	body["draft"] = draft
	if s, _ := c.do("POST", "/api/candidates", "EMP-1002", "HR Admin", body); s != 403 { // someone else's uploads
		t.Fatalf("another raiser claimed the documents: %d", s)
	}
	s, r = c.do("POST", "/api/candidates", "EMP-1004", "HRBP", body)
	if s != 200 {
		t.Fatalf("shortlist: %d %v", s, r)
	}
	si := r["id"].(string)
	if stagesCount(st, "document", bson.D{{Key: "owner_kind", Value: "candidate"}, {Key: "owner_id", Value: si}, {Key: "doc_key", Value: "pan"}}) != 1 ||
		stagesCount(st, "document", bson.D{{Key: "owner_id", Value: draft}}) != 0 {
		t.Fatal("the PAN card belongs to the candidate")
	}

	// The offer form starts from it; an approver without the ID right does not read the number.
	_, x := c.do("GET", "/api/candidates/"+si, "EMP-1004", "HRBP", nil)
	if pre := x["prefill"].(map[string]any); pre["aadhaar"] != "123412341234" || pre["phone"] != "9876502211" {
		t.Fatalf("prefill: %v", pre)
	}
	_, x = c.do("GET", "/api/candidates/"+si, "EMP-1009", "Line Manager", nil)
	if pre, _ := x["prefill"].(map[string]any); pre == nil || pre["aadhaar"] != nil || x["data"].(map[string]any)["aadhaar"] != nil {
		t.Fatalf("approver reads the Aadhaar number: %v", x["prefill"])
	}

	// Carried forward: the offer raised without it keeps the value for the joining details, and the PAN card counts.
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	if s, r := c.do("POST", "/api/candidates/"+si+"/interview", "EMP-1009", "Line Manager", map[string]any{"date": today(), "result": "Selected", "feedback": "Good"}); s != 200 {
		t.Fatalf("interview: %v", r)
	}
	oi := approvedOffer(t, c, st, si, "Asha", "Rao", "9876502211")
	_, o := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	if d := o["data"].(map[string]any); d["aadhaar"] != "123412341234" {
		t.Fatalf("joining details: %v", d)
	}
	if docs := o["docs"].(map[string]any); docs["pan"] == nil {
		t.Fatalf("PAN card at joining: %v", docs)
	}
}
