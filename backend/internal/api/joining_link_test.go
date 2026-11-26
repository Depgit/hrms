package api_test

// The joiner's self-fill link (joining_link.go): HR sends it, the joiner confirms a code, fills their own fields,
// uploads their documents and submits; HR sees and saves. Email goes to a fake mailbox, never anywhere real.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

var (
	reJoinURL  = regexp.MustCompile(`/join/([A-Za-z0-9_-]{43})`)
	reJoinCode = regexp.MustCompile(`\b(\d{6})\b`)
)

func joinSha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// jdo calls a public /api/join route: no sign-in, the joiner's session (if any) in X-Join-Session.
func jdo(t *testing.T, base, method, path, session string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, base+path, &buf)
	if session != "" {
		req.Header.Set("X-Join-Session", session)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if out == nil {
		out = map[string]any{}
	}
	return res.StatusCode, out
}

// jupload uploads a file through the joiner's link.
func jupload(t *testing.T, base, token, session, key, name string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("docKey", key)
	f, _ := w.CreateFormFile("file", name)
	_, _ = f.Write(content)
	_ = w.Close()
	req, _ := http.NewRequest("POST", base+"/api/join/"+token+"/files", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Join-Session", session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

var joinPDF = []byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")

// joinServer is a second API on the same database whose mailbox is the fake one (and, unless dev, without the
// development echo of codes).
func joinServer(t *testing.T, st *store.Store, c *client, fake mail.Sender, dev bool) *client {
	app := api.New(st, api.Options{DevOTP: dev, Files: &storage.Memory{}, Mail: fake, AppURL: "https://hr.example.com/"})
	srv := httptest.NewServer(app.Routes())
	t.Cleanup(srv.Close)
	return &client{t: t, base: srv.URL, tokens: c.tokens, st: st}
}

// releasedOffer takes a new candidate to an accepted offer and returns its OI id.
func releasedOffer(t *testing.T, c *client, st *store.Store, first, last, phone string) string {
	t.Helper()
	si := selectCandidate(t, c, shortlistBody(st, first, last, phone))
	oi := approvedOffer(t, c, st, si, first, last, phone)
	for _, step := range []string{"release", "accept"} {
		if s, r := c.do("POST", "/api/offers/"+oi+"/"+step, "EMP-1004", "HRBP", nil); s != 200 {
			t.Fatalf("%s: %v", step, r)
		}
	}
	return oi
}

func linkToken(t *testing.T, r map[string]any) string {
	t.Helper()
	m := reJoinURL.FindStringSubmatch(fmt.Sprint(r["url"]))
	if m == nil {
		t.Fatalf("no link in %v", r)
	}
	return m[1]
}

// codeFor asks for a code by email and reads it from the fake mailbox.
func codeFor(t *testing.T, base, token string, fake *mail.Fake, to string) string {
	t.Helper()
	before := fake.Count()
	if s, r := jdo(t, base, "POST", "/api/join/"+token+"/code", "", nil); s != 200 || r["sent"] != true {
		t.Fatalf("code: %d %v", s, r)
	}
	if fake.Count() != before+1 {
		t.Fatal("no code email")
	}
	m := fake.Last(to)
	if !strings.Contains(m.Subject, "Your code for the Saltstayz joining form") {
		t.Fatalf("code mail: %v", m.Subject)
	}
	return reJoinCode.FindStringSubmatch(m.Text)[1]
}

// rewind moves a link's last code back a minute, past the 30-second wait.
func rewind(t *testing.T, st *store.Store, token string) {
	t.Helper()
	if _, err := st.DB.C("joining_link").Raw().UpdateOne(context.Background(), bson.M{"token_hash": joinSha(token)},
		bson.M{"$set": bson.M{"code_sent_at": time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
}

func verify(t *testing.T, base, token, code string) string {
	t.Helper()
	s, r := jdo(t, base, "POST", "/api/join/"+token+"/verify", "", map[string]string{"code": code})
	if s != 200 || r["session"] == nil {
		t.Fatalf("verify: %d %v", s, r)
	}
	return r["session"].(string)
}

func TestJoinerLink(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1014")
	fake := &mail.Fake{}
	m := joinServer(t, st, c, fake, false)
	const email = "riya.sharma@example.com"

	// Before release there is nothing to send.
	si := selectCandidate(t, c, shortlistBody(st, "Riya", "Sharma", "9876501234"))
	oi := approvedOffer(t, c, st, si, "Riya", "Sharma", "9876501234")
	if s, r := m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "once the offer is released") {
		t.Fatalf("before release: %d %v", s, r)
	}
	for _, step := range []string{"release", "accept"} {
		c.do("POST", "/api/offers/"+oi+"/"+step, "EMP-1004", "HRBP", nil)
	}

	// No SMTP: a clear error, and the panel says so.
	if s, r := c.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "email"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Email is not set up") {
		t.Fatalf("no SMTP: %d %v", s, r)
	}
	if _, r := c.do("GET", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", nil); r["mail"] != "off" || r["email"] != email || r["mobile"] != "9876501234" || r["link"] != nil {
		t.Fatalf("status without SMTP: %v", r)
	}
	if s, _ := m.do("POST", "/api/joiner-link/"+oi, "EMP-1014", "Employee", map[string]string{"via": "copy"}); s != 403 {
		t.Fatal("only onboarding roles send the link")
	}

	// Email the link: the fake mailbox gets it; only the token's hash is stored.
	s, r := m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "email"})
	if s != 200 || r["emailed"] != true {
		t.Fatalf("email: %d %v", s, r)
	}
	tokA := linkToken(t, r)
	lm := fake.Last(email)
	if lm.Subject != "Complete your joining details for Saltstayz" || !strings.Contains(lm.Text, "https://hr.example.com/join/"+tokA) ||
		!strings.Contains(lm.Text, "Dear Riya Sharma") || !strings.Contains(lm.Text, "6-digit code") || !strings.Contains(lm.HTML, tokA) {
		t.Fatalf("link mail: %+v", lm)
	}
	l := apiDoc(st, "joining_link", bson.M{"offer_id": oi})
	if l["token_hash"] != joinSha(tokA) || l["sent_at"] == nil || l["sent_to_email"] != email || !strings.Contains(fmt.Sprint(l), "email") {
		t.Fatalf("stored link: %v", l)
	}
	if strings.Contains(fmt.Sprint(l), tokA) {
		t.Fatal("the raw token is stored")
	}
	if exp := l["expires_at"].(time.Time); exp.Sub(time.Now()) < 6*24*time.Hour || exp.Sub(time.Now()) > 7*24*time.Hour+time.Minute {
		t.Fatalf("expires %v", exp)
	}

	// Opening: no name before the code; a wrong or made-up link gets the same answer.
	s, r = jdo(t, m.base, "GET", "/api/join/"+tokA, "", nil)
	if s != 200 || r["state"] != "code" || r["codeBy"] != "email" || r["codeTo"] != "ri•••@example.com" || r["company"] != "Saltstayz" || r["name"] != nil {
		t.Fatalf("open: %d %v", s, r)
	}
	if apiCount(st, "audit_log", bson.M{"action": "Joiner opened the link", "entity_id": oi}) != 1 {
		t.Fatal("open audited")
	}
	bogus := strings.Repeat("A", 43)
	s1, r1 := jdo(t, m.base, "GET", "/api/join/"+bogus, "", nil)
	s2, r2 := jdo(t, m.base, "GET", "/api/join/short", "", nil)
	if s1 != 404 || s2 != 404 || r1["error"] != r2["error"] || !strings.Contains(fmt.Sprint(r1["error"]), "no longer in use") {
		t.Fatalf("unknown links: %d %v / %d %v", s1, r1, s2, r2)
	}

	// The code: emailed, 30 seconds between codes, 5 wrong tries lock it, it expires.
	code := codeFor(t, m.base, tokA, fake, email)
	if s, r := jdo(t, m.base, "POST", "/api/join/"+tokA+"/code", "", nil); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Wait 30 seconds") {
		t.Fatalf("resend wait: %d %v", s, r)
	}
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 1; i <= 5; i++ {
		s, r := jdo(t, m.base, "POST", "/api/join/"+tokA+"/verify", "", map[string]string{"code": wrong})
		if s != 400 || (i < 5 && !strings.Contains(fmt.Sprint(r["error"]), fmt.Sprintf("%d tr", 5-i))) || (i == 5 && !strings.Contains(fmt.Sprint(r["error"]), "Too many wrong codes")) {
			t.Fatalf("wrong %d: %v", i, r)
		}
	}
	if s, r := jdo(t, m.base, "POST", "/api/join/"+tokA+"/verify", "", map[string]string{"code": code}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Too many wrong codes") {
		t.Fatalf("a locked code stays locked: %d %v", s, r)
	}
	rewind(t, st, tokA)
	code = codeFor(t, m.base, tokA, fake, email)
	if _, err := st.DB.C("joining_link").Raw().UpdateOne(ctx, bson.M{"token_hash": joinSha(tokA)}, bson.M{"$set": bson.M{"code_expires_at": time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if s, r := jdo(t, m.base, "POST", "/api/join/"+tokA+"/verify", "", map[string]string{"code": code}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "expired") {
		t.Fatalf("expired code: %d %v", s, r)
	}
	rewind(t, st, tokA)
	code = codeFor(t, m.base, tokA, fake, email)
	sessA := verify(t, m.base, tokA, code)
	if s, _ := jdo(t, m.base, "POST", "/api/join/"+tokA+"/verify", "", map[string]string{"code": code}); s != 400 {
		t.Fatal("a code is used once")
	}

	// The form: who and what, the joiner's own fields and documents, nothing of HR's or pay.
	if s, _ := jdo(t, m.base, "GET", "/api/join/"+tokA+"/form", "", nil); s != 401 {
		t.Fatal("the form needs the session")
	}
	s, f := jdo(t, m.base, "GET", "/api/join/"+tokA+"/form", sessA, nil)
	if s != 200 || f["name"] != "Riya Sharma" || f["offerNo"] != oi || f["type"] != "Permanent" || f["editable"] != true {
		t.Fatalf("form: %d %v", s, f)
	}
	keys := []string{}
	for _, x := range f["fields"].([]any) {
		keys = append(keys, x.(map[string]any)["key"].(string))
	}
	ks := " " + strings.Join(keys, " ") + " "
	for _, k := range []string{"gender", "dob", "aadhaar", "pan", "uanAvail", "uan", "bankName", "acct", "ifsc", "pfChoice", "spouse"} {
		if !strings.Contains(ks, " "+k+" ") {
			t.Fatalf("joiner field %s missing: %s", k, ks)
		}
	}
	for _, k := range []string{"shift", "attMode", "l1", "l2", "gross", "grade", "skill", "recruiter", "first", "email", "phone"} {
		if strings.Contains(ks, " "+k+" ") {
			t.Fatalf("HR field %s on the joiner's form: %s", k, ks)
		}
	}
	raw, _ := json.Marshal(f)
	for _, leak := range []string{"26000", "gross", "EMP-1009", "manager", "breakup", "ctc", "Front Office Supervisor", "9876501234"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the joiner's form shows %q: %s", leak, raw)
		}
	}
	docs := []string{}
	for _, x := range f["docs"].([]any) {
		d := x.(map[string]any)
		docs = append(docs, fmt.Sprint(d["key"], "=", d["uploaded"] != nil))
	}
	if ds := strings.Join(docs, " "); !strings.Contains(ds, "aadhaar=true") || !strings.Contains(ds, "pan=false") || !strings.Contains(ds, "police=false") {
		t.Fatalf("documents: %s", ds)
	}

	// Session scope: not a sign-in, and not another offer's link.
	for _, h := range []string{"Authorization", "X-Join-Session"} {
		req, _ := http.NewRequest("GET", m.base+"/api/offers/"+oi, nil)
		v := sessA
		if h == "Authorization" {
			v = "Bearer " + sessA
		}
		req.Header.Set(h, v)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("the joiner's session as %s on a signed-in route: %d", h, res.StatusCode)
		}
	}
	oiB := releasedOffer(t, c, st, "Kavya", "Nair", "9876501299")
	_, rb := m.do("POST", "/api/joiner-link/"+oiB, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tokB := linkToken(t, rb)
	if s, _ := jdo(t, m.base, "GET", "/api/join/"+tokB+"/form", sessA, nil); s != 401 {
		t.Fatal("offer A's session reads offer B")
	}
	if s, _ := jdo(t, m.base, "PUT", "/api/join/"+tokB+"/form", sessA, map[string]any{"data": map[string]any{"gender": "Male"}}); s != 401 {
		t.Fatal("offer A's session writes offer B")
	}
	if s, _ := jupload(t, m.base, tokB, sessA, "pan", "pan.joinPDF", joinPDF); s != 401 {
		t.Fatal("offer A's session uploads to offer B")
	}

	// Saving: only the joiner's keys; HR's are refused; the HR save's validators apply.
	if s, r := jdo(t, m.base, "PUT", "/api/join/"+tokA+"/form", sessA, map[string]any{"data": map[string]any{"gender": "Female", "shift": "G", "gross": 90000}}); s != 400 ||
		!strings.Contains(fmt.Sprint(r["error"]), "filled in by HR") || !strings.Contains(fmt.Sprint(r["error"]), "Shift") {
		t.Fatalf("HR keys: %d %v", s, r)
	}
	// A wrong value is not saved and is listed with what to fix; the good values in the same save are kept.
	for body, want := range map[string]string{
		`{"pan":"BAD"}`: "PAN: 10 characters, like ABCDE1234F", `{"aadhaar":"1234"}`: "Aadhaar: enter the 12 digits", `{"ifsc":"XX"}`: "IFSC: 11 characters, like HDFC0001234",
		`{"gender":"Robot"}`: "choose one of the options", `{"dob":"2030-01-01"}`: "past date", `{"ecPhone":"12345"}`: "Emergency contact mobile: enter 10 digits",
		`{"acct":"12"}`: "Account number",
	} {
		var d map[string]any
		_ = json.Unmarshal([]byte(body), &d)
		d["ecName"] = "Asha Sharma"
		s, r := jdo(t, m.base, "PUT", "/api/join/"+tokA+"/form", sessA, map[string]any{"data": d})
		if s != 200 || !strings.Contains(fmt.Sprint(r["problems"]), want) || strings.Contains(fmt.Sprint(r["skipped"]), "ecName") {
			t.Fatalf("%s: %d %v", body, s, r)
		}
		od := apiDoc(st, "offer", bson.M{"_id": oi})["data"].(map[string]any)
		for k := range d { // the value from the shortlist (gender, date of birth) may stay; the wrong one is not saved
			if k != "ecName" && od[k] == d[k] {
				t.Fatalf("%s: the wrong value was saved: %v", body, od[k])
			}
		}
		if od["ecName"] != "Asha Sharma" {
			t.Fatalf("%s: the good value was not kept: %v", body, od["ecName"])
		}
	}
	// Spaces typed in numbers are not kept.
	good := map[string]any{"gender": "Female", "dob": "1996-04-12", "aadhaar": "1234 1234 1234", "pan": "abcde1234f", "uanAvail": "No", "pfChoice": "No",
		"bankName": "HDFC Bank", "acct": "50100123456789", "ifsc": "hdfc0001234", "ecName": "Asha Sharma", "ecPhone": "98765 00000"}
	if s, r := jdo(t, m.base, "PUT", "/api/join/"+tokA+"/form", sessA, map[string]any{"data": good}); s != 200 {
		t.Fatalf("save: %d %v", s, r)
	}
	o := apiDoc(st, "offer", bson.M{"_id": oi})
	od := o["data"].(map[string]any)
	if od["pan"] != "ABCDE1234F" || od["ifsc"] != "HDFC0001234" || od["acct"] != "50100123456789" || od["aadhaar"] != "123412341234" || od["ecPhone"] != "9876500000" ||
		od["shift"] != nil || od["first"] != "Riya" {
		t.Fatalf("offer data: %v", od)
	}
	if l := apiDoc(st, "joining_link", bson.M{"token_hash": joinSha(tokA)}); !strings.Contains(fmt.Sprint(l["joiner_keys"]), "aadhaar") || l["saved_at"] == nil {
		t.Fatalf("joiner keys: %v", l["joiner_keys"])
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": "Joiner saved joining details"}); a == nil || strings.Contains(fmt.Sprint(a), "123412341234") || a["actor"] != "Joiner Riya Sharma ("+oi+")" {
		t.Fatalf("save audit (names, not values): %v", a)
	}

	// Submitting with documents missing is refused, and says what is missing.
	if s, r := jdo(t, m.base, "POST", "/api/join/"+tokA+"/submit", sessA, nil); s != 400 || !strings.Contains(fmt.Sprint(r["details"]), "Upload your PAN card") || !strings.Contains(fmt.Sprint(r["details"]), "Upload your Police verification") {
		t.Fatalf("submit with missing documents: %d %v", s, r)
	}
	// Uploads: only the joiner's documents, the usual limits; stored with the candidate, where HR sees them.
	if s, r := jupload(t, m.base, tokA, sessA, "resume", "cv.joinPDF", joinPDF); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "not asked") {
		t.Fatalf("a document not on the form: %d %v", s, r)
	}
	if s, r := jupload(t, m.base, tokA, sessA, "pan", "pan.txt", []byte("hello, plain text")); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "PDF, JPG or PNG") {
		t.Fatalf("a text file: %d %v", s, r)
	}
	if s, _ := jupload(t, m.base, tokA, "", "pan", "pan.joinPDF", joinPDF); s != 401 {
		t.Fatal("upload needs the session")
	}
	before := nextCode(st)
	for _, k := range []string{"pan", "photo", "bankProof", "police"} {
		if s, r := jupload(t, m.base, tokA, sessA, k, k+".joinPDF", joinPDF); s != 200 || r["fileName"] != k+".joinPDF" {
			t.Fatalf("upload %s: %d %v", k, s, r)
		}
	}
	if d := apiDoc(st, "document", bson.M{"owner_kind": "candidate", "owner_id": si, "doc_key": "pan", "replaced_at": nil}); d == nil || d["uploaded_by"] != "Riya Sharma (joiner)" {
		t.Fatalf("stored with the candidate: %v", d)
	}
	if nextCode(st) != before {
		t.Fatal("the joiner created an Employee ID")
	}

	// Submit: read-only after, HR is told.
	if s, r := jdo(t, m.base, "POST", "/api/join/"+tokA+"/submit", sessA, nil); s != 200 || r["submittedAt"] == nil {
		t.Fatalf("submit: %d %v", s, r)
	}
	if s, r := jdo(t, m.base, "PUT", "/api/join/"+tokA+"/form", sessA, map[string]any{"data": map[string]any{"gender": "Male"}}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "with HR now") {
		t.Fatalf("after submit: %d %v", s, r)
	}
	if s, _ := jupload(t, m.base, tokA, sessA, "pan", "pan.joinPDF", joinPDF); s != 400 {
		t.Fatal("upload after submit")
	}
	if s, _ := jdo(t, m.base, "POST", "/api/join/"+tokA+"/submit", sessA, nil); s != 400 {
		t.Fatal("submitted twice")
	}
	if _, r := jdo(t, m.base, "GET", "/api/join/"+tokA, "", nil); r["state"] != "submitted" {
		t.Fatalf("status after submit: %v", r)
	}
	if _, r := jdo(t, m.base, "GET", "/api/join/"+tokA+"/form", sessA, nil); r["editable"] != false || r["submittedAt"] == nil {
		t.Fatalf("form after submit: %v", r)
	}
	if n := notices(c, "EMP-1004", "HRBP"); !strings.Contains(n, "Riya Sharma submitted joining details ("+oi+")") {
		t.Fatalf("HR's bell: %s", n)
	}
	if _, r := m.do("GET", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", nil); r["link"].(map[string]any)["state"] != "submitted" || r["mail"] != "smtp" {
		t.Fatalf("HR's panel: %v", r)
	}
	_, l2 := c.do("GET", "/api/candidates", "EMP-1004", "HRBP", nil)
	if !strings.Contains(fmt.Sprint(l2["rows"]), "joinerLink:Joiner submitted") {
		t.Fatal("the pipeline row shows the link")
	}

	// HR sees the values, with who fills each field, and saves HR's own fields as before.
	_, ho := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	by := map[string]string{}
	for _, x := range ho["joinFields"].([]any) {
		by[x.(map[string]any)["key"].(string)] = x.(map[string]any)["filledBy"].(string)
	}
	if by["pan"] != "joiner" || by["shift"] != "hr" || ho["data"].(map[string]any)["aadhaar"] != "123412341234" {
		t.Fatalf("HR's view: %v", by)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": map[string]any{"pan": "ABCDE1234F", "shift": "G", "attMode": "Biometric", "l1": "EMP-1009"}}); s != 200 {
		t.Fatalf("HR save: %d %v", s, r)
	}

	// Reopen for the joiner: a note, a new link (emailed), the old one stops.
	if s, _ := m.do("POST", "/api/joiner-link/"+oi+"/reopen", "EMP-1004", "HRBP", map[string]string{"note": ""}); s != 400 {
		t.Fatal("reopen needs a note")
	}
	s, r = m.do("POST", "/api/joiner-link/"+oi+"/reopen", "EMP-1004", "HRBP", map[string]string{"note": "Upload a clearer photo"})
	if s != 200 || r["emailed"] != true || !strings.Contains(fake.Last(email).Text, "A note from HR: Upload a clearer photo") {
		t.Fatalf("reopen: %d %v", s, r)
	}
	tokA2 := linkToken(t, r)
	if s, _ := jdo(t, m.base, "GET", "/api/join/"+tokA, "", nil); s != 404 {
		t.Fatal("the submitted link still opens")
	}
	sessA2 := verify(t, m.base, tokA2, codeFor(t, m.base, tokA2, fake, email))
	if _, f := jdo(t, m.base, "GET", "/api/join/"+tokA2+"/form", sessA2, nil); f["note"] != "Upload a clearer photo" || f["editable"] != true || f["values"].(map[string]any)["pan"] != "ABCDE1234F" {
		t.Fatalf("reopened form: %v", f)
	}
	if apiCount(st, "audit_log", bson.M{"entity_id": oi, "action": bson.M{"$regex": "^Joining details reopened"}}) != 1 {
		t.Fatal("reopen audited")
	}

	// A new link revokes the old; the same link can be shared again with its token.
	_, r = m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "renew"})
	tokA3 := linkToken(t, r)
	if s, _ := jdo(t, m.base, "GET", "/api/join/"+tokA2+"/form", sessA2, nil); s != 404 {
		t.Fatal("the replaced link still works")
	}
	_, r = m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "whatsapp", "token": tokA3})
	if r["reused"] != true || linkToken(t, r) != tokA3 {
		t.Fatalf("share again: %v", r)
	}
	if n := apiCount(st, "joining_link", bson.M{"offer_id": oi, "live_offer": oi}); n != 1 {
		t.Fatalf("%d live links", n)
	}

	// Stop: the link no longer works.
	if s, _ := m.do("POST", "/api/joiner-link/"+oi+"/stop", "EMP-1004", "HRBP", nil); s != 200 {
		t.Fatal("stop")
	}
	if s, _ := jdo(t, m.base, "GET", "/api/join/"+tokA3, "", nil); s != 404 {
		t.Fatal("a stopped link opens")
	}

	// The link closes when the offer leaves the editable statuses (offer B declined).
	if s, _ := jdo(t, m.base, "GET", "/api/join/"+tokB, "", nil); s != 200 {
		t.Fatal("B open")
	}
	if s, r := c.do("POST", "/api/offers/"+oiB+"/decline", "EMP-1004", "HRBP", map[string]string{"reason": "Took another job"}); s != 200 {
		t.Fatal(r)
	}
	if s, r := jdo(t, m.base, "GET", "/api/join/"+tokB, "", nil); s != 404 || !strings.Contains(fmt.Sprint(r["error"]), "no longer in use") {
		t.Fatalf("declined offer's link: %d %v", s, r)
	}
}

// Without email: the code is given by HR on the phone (shown once), or shown on the page in development; three
// locked codes lock the link.
func TestJoinerLinkWithoutEmail(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := c.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	if !strings.HasPrefix(fmt.Sprint(r["url"]), "http://localhost:5173/join/") {
		t.Fatalf("APP_URL default: %v", r["url"])
	}
	if _, s := jdo(t, c.base, "GET", "/api/join/"+tok, "", nil); s["codeBy"] != "phone" || s["codeTo"] != "" {
		t.Fatalf("status: %v", s)
	}
	// Development (OTP_DEV_ECHO): the code is in the answer, like the sign-in OTP.
	s, x := jdo(t, c.base, "POST", "/api/join/"+tok+"/code", "", nil)
	if s != 200 || x["sent"] != false || len(fmt.Sprint(x["devCode"])) != 6 {
		t.Fatalf("dev code: %d %v", s, x)
	}
	verify(t, c.base, tok, x["devCode"].(string))
	// Production without email: the joiner asks HR to call (HR's bell; once in 10 minutes); HR's panel gives the
	// code once.
	quiet := joinServer(t, st, c, nil, false)
	if _, s := jdo(t, quiet.base, "GET", "/api/join/"+tok, "", nil); s["askHR"] != true || s["hrName"] == nil {
		t.Fatalf("status without email: %v", s)
	}
	for i := 0; i < 3; i++ {
		if s, r := jdo(t, quiet.base, "POST", "/api/join/"+tok+"/code", "", nil); s != 200 || r["asked"] != true || r["devCode"] != nil {
			t.Fatalf("ask HR: %d %v", s, r)
		}
	}
	if n := apiCount(st, "notice", bson.M{"to_employee_id": "EMP-1004", "text": bson.M{"$regex": "Riya Sharma needs a code for the joining form \\(" + oi + "\\)\\. Call them on 9876501234"}}); n != 1 {
		t.Fatalf("%d notices for HR, want 1", n)
	}
	if s, _ := quiet.do("POST", "/api/joiner-link/"+oi+"/code", "EMP-1009", "Line Manager", nil); s != 403 {
		t.Fatal("only onboarding roles give codes")
	}
	_, hc := quiet.do("POST", "/api/joiner-link/"+oi+"/code", "EMP-1004", "HRBP", nil)
	if len(fmt.Sprint(hc["code"])) != 6 {
		t.Fatalf("phone code: %v", hc)
	}
	if apiDoc(st, "joining_link", bson.M{"token_hash": joinSha(tok)})["code_hash"] == hc["code"] {
		t.Fatal("the code is stored as it is")
	}
	verify(t, quiet.base, tok, hc["code"].(string))
	// Three codes locked by wrong tries lock the link; HR sees it and sends a new one.
	for lock := 1; lock <= 3; lock++ {
		_, hc = quiet.do("POST", "/api/joiner-link/"+oi+"/code", "EMP-1004", "HRBP", nil)
		wrong := "000000"
		if hc["code"] == wrong {
			wrong = "111111"
		}
		var s int
		var r map[string]any
		for i := 0; i < 5; i++ {
			s, r = jdo(t, quiet.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": wrong})
		}
		if lock < 3 && (s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Call HR for a new code")) {
			t.Fatalf("lock %d: %d %v", lock, s, r)
		}
		if lock == 3 && (s != 403 || !strings.Contains(fmt.Sprint(r["error"]), "Ask HR to send you a new link")) {
			t.Fatalf("link lock: %d %v", s, r)
		}
	}
	if s, _ := jdo(t, quiet.base, "GET", "/api/join/"+tok, "", nil); s != 403 {
		t.Fatal("a locked link opens")
	}
	if _, r := quiet.do("GET", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", nil); r["link"].(map[string]any)["state"] != "locked" || r["mail"] != "off" {
		t.Fatalf("HR sees the lock: %v", r)
	}
	if apiCount(st, "audit_log", bson.M{"action": "Joiner's link locked after too many wrong codes"}) != 1 {
		t.Fatal("lock audited")
	}

	// Made-up links: after 20 from one address, that address's made-up links wait; a real link still opens (all
	// joiners may share one address).
	_, r = quiet.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "renew"})
	good := linkToken(t, r)
	for i := 0; i < 20; i++ {
		if s, _ := jdo(t, quiet.base, "GET", "/api/join/"+fmt.Sprintf("%043d", i), "", nil); s != 404 {
			t.Fatalf("probe %d: %d", i, s)
		}
	}
	if s, _ := jdo(t, quiet.base, "GET", "/api/join/"+fmt.Sprintf("%043d", 99), "", nil); s != 429 {
		t.Fatalf("the 21st made-up link: %d", s)
	}
	if s, _ := jdo(t, quiet.base, "GET", "/api/join/"+good, "", nil); s != 200 {
		t.Fatalf("a real link after 20 made-up ones: %d", s)
	}
}

// Form setup: who fills each field and document (Filled by), with the rules on what the joiner may fill.
func TestJoinerFormSetup(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	_, setup := c.do("GET", "/api/onboarding/setup", "EMP-1002", "HR Admin", nil)
	by := map[string]string{}
	for _, x := range setup["fields"].([]any) {
		by[x.(map[string]any)["key"].(string)] = x.(map[string]any)["filledBy"].(string)
	}
	for _, x := range setup["docs"].([]any) {
		by["doc:"+x.(map[string]any)["key"].(string)] = x.(map[string]any)["filledBy"].(string)
	}
	if by["pan"] != "joiner" || by["bankName"] != "joiner" || by["medical"] != "joiner" || by["shift"] != "hr" || by["l1"] != "hr" || by["gross"] != "hr" ||
		by["recruiter"] != "hr" || by["first"] != "hr" || by["doc:police"] != "joiner" || by["doc:aadhaar"] != "joiner" {
		t.Fatalf("defaults: %v", by)
	}
	if s, _ := c.do("PATCH", "/api/onboarding/setup/fields/pan", "EMP-1004", "HRBP", map[string]string{"filledBy": "hr"}); s != 403 {
		t.Fatal("Form setup needs onb.setup at level 2")
	}
	for path, want := range map[string]string{"fields/gross": "always filled by HR", "fields/l1": "always filled by HR", "fields/first": "always filled by HR"} {
		if s, r := c.do("PATCH", "/api/onboarding/setup/"+path, "EMP-1002", "HR Admin", map[string]string{"filledBy": "joiner"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), want) {
			t.Fatalf("%s: %d %v", path, s, r)
		}
	}
	if s, _ := c.do("PATCH", "/api/onboarding/setup/fields/pan", "EMP-1002", "HR Admin", map[string]string{"filledBy": "candidate"}); s != 400 {
		t.Fatal("only joiner or hr")
	}
	for path, v := range map[string]string{"fields/pan": "hr", "fields/skill": "joiner", "docs/police": "hr"} {
		if s, r := c.do("PATCH", "/api/onboarding/setup/"+path, "EMP-1002", "HR Admin", map[string]string{"filledBy": v}); s != 200 {
			t.Fatalf("%s: %v", path, r)
		}
	}
	if f := apiDoc(st, "onb_field", bson.M{"key": "pan"}); f["filled_by"] != "hr" {
		t.Fatal(f)
	}
	// New custom items: a field by its section, a document the joiner's; a Compensation field never the joiner's.
	_, a := c.do("POST", "/api/onboarding/setup/fields", "EMP-1002", "HR Admin", map[string]any{"label": "Languages spoken", "section": "Personal", "kind": "text", "offer": 0, "join": 1})
	_, b := c.do("POST", "/api/onboarding/setup/fields", "EMP-1002", "HR Admin", map[string]any{"label": "Uniform size", "section": "Job", "kind": "text", "offer": 0, "join": 1})
	_, d := c.do("POST", "/api/onboarding/setup/docs", "EMP-1002", "HR Admin", map[string]any{"label": "Driving licence", "offer": 0, "join": 1})
	if apiDoc(st, "onb_field", bson.M{"key": a["key"]})["filled_by"] != "joiner" || apiDoc(st, "onb_field", bson.M{"key": b["key"]})["filled_by"] != "hr" ||
		apiDoc(st, "onb_document", bson.M{"key": d["key"]})["filled_by"] != "joiner" {
		t.Fatal("custom item defaults")
	}
	if s, _ := c.do("POST", "/api/onboarding/setup/fields", "EMP-1002", "HR Admin", map[string]any{"label": "Bonus", "section": "Compensation", "kind": "number", "join": 1, "filledBy": "joiner"}); s != 400 {
		t.Fatal("a Compensation field for the joiner")
	}

	// The joiner's form follows: PAN is HR's now (refused), skill is the joiner's, the police check is not asked.
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := c.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	_, x := jdo(t, c.base, "POST", "/api/join/"+tok+"/code", "", nil)
	sess := verify(t, c.base, tok, x["devCode"].(string))
	_, f := jdo(t, c.base, "GET", "/api/join/"+tok+"/form", sess, nil)
	fl, _ := json.Marshal(f["fields"])
	dl, _ := json.Marshal(f["docs"])
	if strings.Contains(string(fl), `"key":"pan"`) || !strings.Contains(string(fl), `"key":"skill"`) || strings.Contains(string(dl), `"key":"police"`) ||
		!strings.Contains(string(fl), "Languages spoken") || strings.Contains(string(fl), "Uniform size") || !strings.Contains(string(dl), "Driving licence") {
		t.Fatalf("form after setup changes: %s %s", fl, dl)
	}
	if s, r := jdo(t, c.base, "PUT", "/api/join/"+tok+"/form", sess, map[string]any{"data": map[string]any{"pan": "ABCDE1234F"}}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "PAN") {
		t.Fatalf("PAN refused: %d %v", s, r)
	}
}
