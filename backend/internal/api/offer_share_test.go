package api_test

import (
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/mail"
)

// TestOfferLetterSend: the letter goes again to another address or on WhatsApp once released; rights, refusals,
// the saved address and the timeline.
func TestOfferLetterSend(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1014")
	fake := &mail.Fake{}
	m := joinServer(t, st, c, fake, false)
	si := selectCandidate(t, c, shortlistBody(st, "Abdul", "Kalam", "9876501299"))
	oi := approvedOffer(t, c, st, si, "Abdul", "Kalam", "9876501299")
	path := "/api/offers/" + oi + "/letter/send"
	// The HR who raised the offer (EMP-1004) is copied on the email (owner, 27 Nov 2026).
	if _, err := st.DB.C("employee_personal").UpdateMany(t.Context(), bson.M{"employee_id": "EMP-1004"}, bson.M{"$set": bson.M{"email": "raiser.hr@example.com"}}); err != nil {
		t.Fatal(err)
	}
	// The breakup of v1 is kept when raised (offer_paysnap.go).
	if v, _ := st.DB.C("offer_version").FindOne(t.Context(), bson.M{"candidate_id": si, "version": 1}); v == nil || v["pay"] == nil {
		t.Fatalf("v1 has no breakup kept: %v", v)
	}
	alt := "abdul.alt@example.com"

	if s, r := m.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "email", "to": alt}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "once the offer is released") {
		t.Fatalf("before release: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatalf("release: %v", r)
	}
	if s, _ := m.do("POST", path, "EMP-1014", "Employee", map[string]any{"via": "email", "to": alt}); s != 403 {
		t.Fatalf("employee: %d", s)
	}
	if s, r := c.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "email", "to": alt}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Email is not set up") {
		t.Fatalf("no SMTP: %d %v", s, r)
	}
	if s, _ := m.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "email", "to": "not-an-email"}); s != 400 {
		t.Fatal("bad address accepted")
	}

	// Another address, not kept: the letter is in the mail, the offer keeps its email.
	if s, r := m.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "email", "to": alt}); s != 200 {
		t.Fatalf("email: %v", r)
	}
	if got := fake.Last(alt); !strings.Contains(got.Subject, "offer letter") || !strings.Contains(got.Text, "Abdul") || len(got.Cc) != 1 || got.Cc[0] != "raiser.hr@example.com" {
		t.Fatalf("mail: %v", got)
	}
	// The version's breakup as sent: shown to onboarding with the gross, not worked out again.
	_, cv := c.do("GET", "/api/candidates/"+si, "EMP-1004", "HRBP", nil)
	vs, _ := cv["offerVersions"].([]any)
	if len(vs) == 0 {
		t.Fatalf("no versions: %v", cv["offerVersions"])
	}
	pay, _ := vs[0].(map[string]any)["pay"].(map[string]any)
	if b, _ := pay["breakup"].(map[string]any); b == nil || b["gross"] != vs[0].(map[string]any)["gross"] || pay["live"] != nil || pay["rates"] == nil {
		t.Fatalf("version pay: %v", pay)
	}
	o, _ := st.DB.C("offer").FindOne(t.Context(), bson.M{"_id": oi})
	if d, _ := o["data"].(map[string]any); d["email"] == alt {
		t.Fatal("address saved without asking")
	}
	// Kept: the offer's email changes.
	if s, _ := m.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "email", "to": alt, "save": true}); s != 200 {
		t.Fatal("email + save")
	}
	o, _ = st.DB.C("offer").FindOne(t.Context(), bson.M{"_id": oi})
	if d, _ := o["data"].(map[string]any); d["email"] != alt {
		t.Fatalf("address not saved: %v", d["email"])
	}

	// WhatsApp: a 10-digit number, the letter text back, nothing mailed.
	if s, _ := m.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "whatsapp", "to": "12345"}); s != 400 {
		t.Fatal("short number accepted")
	}
	n := fake.Count()
	s, r := m.do("POST", path, "EMP-1004", "HRBP", map[string]any{"via": "whatsapp", "to": "+91 98765 01200"})
	if s != 200 || !strings.Contains(fmt.Sprint(r["text"]), "Abdul") || fake.Count() != n {
		t.Fatalf("whatsapp: %d %v", s, r)
	}
	if k, _ := st.DB.C("lifecycle_event").Count(t.Context(), bson.M{"candidate_id": si, "action": bson.M{"$regex": "^Offer letter (emailed|shared)"}}); k != 3 {
		t.Fatalf("timeline entries: %d", k)
	}
}
