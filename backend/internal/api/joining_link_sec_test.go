package api_test

// Security of the joiner's link under pressure (review of 11 Nov 2026): code guesses sent at the same moment, new
// codes that must not give back tries, code emails asked for together, callers behind a proxy, the token in the
// log, and a link whose hire is over.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

type joinHit struct {
	status int
	body   map[string]any
}

// burst sends n calls at the same moment (released together) and returns the answers in order.
func burst(t *testing.T, n int, call func(i int) (int, map[string]any)) []joinHit {
	t.Helper()
	out := make([]joinHit, n)
	var ready, done sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-gate
			s, b := call(i)
			out[i] = joinHit{s, b}
		}(i)
	}
	ready.Wait()
	close(gate)
	done.Wait()
	return out
}

func joinLinkRow(t *testing.T, st *store.Store, token string) bson.M {
	t.Helper()
	var l bson.M
	if err := st.DB.C("joining_link").Raw().FindOne(context.Background(), bson.M{"token_hash": joinSha(token)}).Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

func joinInt(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case int:
		return x
	}
	return -1
}

// A burst of guesses with the right code among them: at most 5 are compared per code, the right one is refused
// once the code's tries are used, and over several codes the link locks after 15 wrong codes in all.
func TestJoinerCodeGuessBurst(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	fake := &mail.Fake{}
	m := joinServer(t, st, c, fake, false)
	const email = "riya.sharma@example.com"
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)

	code := codeFor(t, m.base, tok, fake, email)
	wrong := func(i int) string {
		w := fmt.Sprintf("%06d", 100000+i)
		if w == code {
			w = "999999"
		}
		return w
	}
	// 60 at once, the right code at index 45.
	hits := burst(t, 60, func(i int) (int, map[string]any) {
		g := wrong(i)
		if i == 45 {
			g = code
		}
		return jdo(t, m.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": g})
	})
	notCorrect, sessions := 0, 0
	for _, h := range hits {
		switch {
		case h.status == 200 && h.body["session"] != nil:
			sessions++
		case strings.Contains(fmt.Sprint(h.body["error"]), "not correct"):
			notCorrect++
		}
	}
	// Each compared guess took one of the link's tries (a right one gives it back): at most 5 were compared.
	l := joinLinkRow(t, st, tok)
	fails := joinInt(l["code_fails"])
	if notCorrect > 4 || fails < 0 || fails+sessions > 5 || sessions > 1 {
		t.Fatalf("%d told not correct, %d sessions, code_fails %d: more than 5 guesses were compared", notCorrect, sessions, fails)
	}
	if sessions == 1 && hits[45].status != 200 {
		t.Fatal("a session for a wrong code")
	}
	// Whatever happened, the link has counted every wrong guess it compared, and no more than 15 can ever be made.
	for round := 0; round < 4; round++ {
		rewind(t, st, tok)
		s, _ := jdo(t, m.base, "POST", "/api/join/"+tok+"/code", "", nil)
		if s == 403 {
			break
		}
		if s != 200 {
			t.Fatalf("round %d code: %d", round, s)
		}
		burst(t, 30, func(i int) (int, map[string]any) {
			return jdo(t, m.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": "000000"})
		})
	}
	l = joinLinkRow(t, st, tok)
	if l["locked_at"] == nil || joinInt(l["code_fails"]) > 15 {
		t.Fatalf("after bursts on several codes: locked_at %v, code_fails %v", l["locked_at"], l["code_fails"])
	}
	if s, r := jdo(t, m.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": code}); s != 403 || !strings.Contains(fmt.Sprint(r["error"]), "new link") {
		t.Fatalf("a locked link: %d %v", s, r)
	}
	if n := apiCount(st, "audit_log", bson.M{"action": "Joiner's link locked after too many wrong codes", "entity_id": oi}); n != 1 {
		t.Fatalf("lock audited %d times", n)
	}
}

// A new code gives the code 5 tries of its own but never gives back the link's: 4 wrong tries on each of 3 codes,
// then 3 more on a fourth, lock the link at 15.
func TestJoinerNewCodeKeepsLinkTries(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	fake := &mail.Fake{}
	m := joinServer(t, st, c, fake, false)
	const email = "riya.sharma@example.com"
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	n := 0
	for round := 0; round < 4; round++ {
		if round > 0 {
			rewind(t, st, tok)
		}
		code := codeFor(t, m.base, tok, fake, email)
		w := "100000"
		if w == code {
			w = "100001"
		}
		for i := 0; i < 4; i++ {
			n++
			s, r := jdo(t, m.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": w})
			if n < 15 && (s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "not correct")) {
				t.Fatalf("wrong try %d: %d %v", n, s, r)
			}
			if n == 15 {
				if s != 403 || !strings.Contains(fmt.Sprint(r["error"]), "Ask HR to send you a new link") {
					t.Fatalf("the 15th wrong code: %d %v", s, r)
				}
				l := joinLinkRow(t, st, tok)
				if l["locked_at"] == nil || joinInt(l["code_fails"]) != 15 {
					t.Fatalf("link after 15: %v %v", l["locked_at"], l["code_fails"])
				}
				return
			}
		}
		if l := joinLinkRow(t, st, tok); joinInt(l["code_fails"]) != n || l["locked_at"] != nil {
			t.Fatalf("after round %d: code_fails %v", round, l["code_fails"])
		}
	}
	t.Fatal("the link never locked")
}

// 30 requests for a code at the same moment send one code email.
func TestJoinerCodeSendBurst(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	fake := &mail.Fake{}
	m := joinServer(t, st, c, fake, false)
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := m.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	before := fake.Count()
	hits := burst(t, 30, func(int) (int, map[string]any) { return jdo(t, m.base, "POST", "/api/join/"+tok+"/code", "", nil) })
	ok := 0
	for _, h := range hits {
		if h.status == 200 {
			ok++
		} else if !strings.Contains(fmt.Sprint(h.body["error"]), "Wait 30 seconds") {
			t.Fatalf("refused with %d %v", h.status, h.body)
		}
	}
	if ok != 1 || fake.Count()-before != 1 {
		t.Fatalf("%d codes given, %d emails sent; want 1 and 1", ok, fake.Count()-before)
	}
	if l := joinLinkRow(t, st, tok); joinInt(l["code_sends"]) != 1 {
		t.Fatalf("code_sends %v", l["code_sends"])
	}
	// The hourly cap holds the same way: 5 codes, then 429, even when asked together after each wait.
	for i := 0; i < 6; i++ {
		rewind(t, st, tok)
		hits = burst(t, 10, func(int) (int, map[string]any) { return jdo(t, m.base, "POST", "/api/join/"+tok+"/code", "", nil) })
		got, capped := 0, 0
		for _, h := range hits {
			switch h.status {
			case 200:
				got++
			case 429:
				capped++
			}
		}
		if (i < 4 && got != 1) || (i >= 4 && (got != 0 || capped != 10)) {
			t.Fatalf("hour round %d: %d given, %d capped", i, got, capped)
		}
	}
	if fake.Count()-before != 5 {
		t.Fatalf("%d code emails in the hour, want 5", fake.Count()-before)
	}
}

// Behind a proxy: X-Forwarded-For from a trusted proxy (the machine itself by default) names the caller, so one
// sender's made-up links do not shut out other joiners; from anyone else the header is ignored.
func TestJoinerRateLimitBehindProxy(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := c.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	get := func(base, path, xff string) int {
		req, _ := http.NewRequest("GET", base+"/api/join/"+path, nil)
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	newSrv := func(trusted []string) string {
		app := api.New(st, api.Options{Files: &storage.Memory{}, TrustedProxies: trusted})
		srv := httptest.NewServer(app.Routes())
		t.Cleanup(srv.Close)
		return srv.URL
	}
	// Trusted (loopback, the default): each sender counts on its own.
	base := newSrv(nil)
	for i := 0; i < 20; i++ {
		get(base, fmt.Sprintf("%043d", i), "203.0.113.7")
	}
	if s := get(base, fmt.Sprintf("%043d", 50), "198.51.100.1, 203.0.113.7"); s != 429 {
		t.Fatalf("the sender's 21st made-up link (a made-up first hop does not help): %d", s)
	}
	if s := get(base, tok, "203.0.113.7"); s != 200 {
		t.Fatalf("the same sender with a real link: %d", s)
	}
	if s := get(base, fmt.Sprintf("%043d", 51), "203.0.113.8"); s != 404 {
		t.Fatalf("another joiner behind the same proxy: %d", s)
	}
	if s := get(base, tok, "203.0.113.8"); s != 200 {
		t.Fatalf("another joiner's real link: %d", s)
	}
	// Not trusted: the header is ignored, so changing it does not escape the limit.
	base = newSrv([]string{"10.255.255.1"})
	for i := 0; i < 20; i++ {
		get(base, fmt.Sprintf("%043d", i), fmt.Sprintf("192.0.2.%d", i))
	}
	if s := get(base, fmt.Sprintf("%043d", 60), "192.0.2.200"); s != 429 {
		t.Fatalf("a made-up X-Forwarded-For from an untrusted caller: %d", s)
	}
	if _, err := api.ParseTrustedProxies("10.0.0.0/8, 192.168.1.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ParseTrustedProxies("10.0.0.0/8, proxy.local"); err == nil {
		t.Fatal("TRUSTED_PROXIES with a name")
	}
}

// The API log never shows a joiner link's token.
func TestJoinerTokenNotLogged(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	oi := releasedOffer(t, c, st, "Riya", "Sharma", "9876501234")
	_, r := c.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	jdo(t, c.base, "GET", "/api/join/"+tok, "", nil)
	_, x := jdo(t, c.base, "POST", "/api/join/"+tok+"/code", "", nil)
	jdo(t, c.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": fmt.Sprint(x["devCode"])})
	jdo(t, c.base, "GET", "/api/join/"+tok+"/form", "x", nil)
	time.Sleep(50 * time.Millisecond) // the log line is written after the answer
	out := buf.String()
	if strings.Contains(out, tok) || strings.Contains(out, tok[:12]) {
		t.Fatalf("the token is in the log:\n%s", out)
	}
	if !strings.Contains(out, "POST /api/join/•••/verify") || !strings.Contains(out, "GET /api/join/•••/form") {
		t.Fatalf("the joiner's calls are not logged (masked):\n%s", out)
	}
}

// A submitted link stops when the hire is over: the offer declined, or Payroll verified the details. Its session
// no longer reads the details, and the link gets the same answer as any dead link.
func TestJoinerLinkStopsWhenHireEnds(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	ctx := context.Background()
	open := func(first, phone string) (string, string, string) {
		oi := releasedOffer(t, c, st, first, "Nair", phone)
		_, r := c.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
		tok := linkToken(t, r)
		_, x := jdo(t, c.base, "POST", "/api/join/"+tok+"/code", "", nil)
		sess := verify(t, c.base, tok, x["devCode"].(string))
		if s, _ := jdo(t, c.base, "PUT", "/api/join/"+tok+"/form", sess, map[string]any{"data": map[string]any{"aadhaar": "123412341234", "pan": "ABCDE1234F"}}); s != 200 {
			t.Fatal("save")
		}
		// Submitted (the documents are not the point here).
		if _, err := st.DB.C("joining_link").Raw().UpdateOne(ctx, bson.M{"token_hash": joinSha(tok)}, bson.M{"$set": bson.M{"submitted_at": time.Now()}}); err != nil {
			t.Fatal(err)
		}
		if s, r := jdo(t, c.base, "GET", "/api/join/"+tok+"/form", sess, nil); s != 200 || r["editable"] != false {
			t.Fatalf("submitted form: %d %v", s, r)
		}
		return oi, tok, sess
	}
	dead := func(what, tok, sess string) {
		t.Helper()
		s1, r1 := jdo(t, c.base, "GET", "/api/join/"+tok+"/form", sess, nil)
		s2, r2 := jdo(t, c.base, "GET", "/api/join/"+tok, "", nil)
		if s1 != 404 || s2 != 404 || strings.Contains(fmt.Sprint(r1), "1234") || !strings.Contains(fmt.Sprint(r2["error"]), "no longer in use") {
			t.Fatalf("%s: form %d %v / status %d %v", what, s1, r1, s2, r2)
		}
	}
	oi, tok, sess := open("Priya", "9876501201")
	if s, r := c.do("POST", "/api/offers/"+oi+"/decline", "EMP-1004", "HRBP", map[string]string{"reason": "Took another job"}); s != 200 {
		t.Fatal(r)
	}
	dead("declined", tok, sess)
	if l := joinLinkRow(t, st, tok); l["revoked_at"] == nil || l["live_offer"] != nil || l["session_hash"] != nil || l["revoke_reason"] != "The offer was declined" {
		t.Fatalf("declined: the link was not stopped: %v", l)
	}
	// Verified by Payroll (set as the verification does): the details are on the employee record now.
	oi, tok, sess = open("Kiran", "9876501202")
	if _, err := st.DB.C("offer").Raw().UpdateOne(ctx, bson.M{"_id": oi}, bson.M{"$set": bson.M{"verified_at": time.Now()}}); err != nil {
		t.Fatal(err)
	}
	dead("verified", tok, sess)
	// With Payroll, the joiner still sees what they sent (read-only).
	oi, tok, sess = open("Meera", "9876501203")
	if _, err := st.DB.C("offer").Raw().UpdateOne(ctx, bson.M{"_id": oi}, bson.M{"$set": bson.M{"verify_status": "Pending"}}); err != nil {
		t.Fatal(err)
	}
	if s, r := jdo(t, c.base, "GET", "/api/join/"+tok+"/form", sess, nil); s != 200 || r["editable"] != false {
		t.Fatalf("with Payroll: %d %v", s, r)
	}
}

// Without email, HR's bell says when the joiner first opens the link: they are waiting for a code by phone.
func TestJoinerOpenedTellsHRWithoutEmail(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	quiet := joinServer(t, st, c, nil, false)
	oi := releasedOffer(t, c, st, "Arjun", "Mehta", "9876501299")
	_, r := quiet.do("POST", "/api/joiner-link/"+oi, "EMP-1004", "HRBP", map[string]string{"via": "copy"})
	tok := linkToken(t, r)
	for i := 0; i < 2; i++ {
		if s, x := jdo(t, quiet.base, "GET", "/api/join/"+tok, "", nil); s != 200 || x["askHR"] != true || x["hrName"] == nil || x["name"] != nil {
			t.Fatalf("open: %d %v", s, x)
		}
	}
	if n := notices(c, "EMP-1004", "HRBP"); !strings.Contains(n, "Arjun Mehta opened the joining link and needs a code ("+oi+"). Call them on 9876501299") {
		t.Fatalf("HR's bell: %s", n)
	}
	if n := apiCount(st, "notice", bson.M{"to_employee_id": "EMP-1004", "text": bson.M{"$regex": "^Arjun Mehta opened"}}); n != 1 {
		t.Fatalf("%d opened notices", n)
	}
	// Asking right after the open notice does not ring again.
	if _, x := jdo(t, quiet.base, "POST", "/api/join/"+tok+"/code", "", nil); x["asked"] != true || !strings.Contains(fmt.Sprint(x["message"]), "already been told") {
		t.Fatalf("ask: %v", x)
	}
	// Wrong-code wording in phone mode names the way to a new code.
	_, hc := quiet.do("POST", "/api/joiner-link/"+oi+"/code", "EMP-1004", "HRBP", nil)
	w := "000000"
	if hc["code"] == w {
		w = "111111"
	}
	var s int
	var x map[string]any
	for i := 0; i < 5; i++ {
		s, x = jdo(t, quiet.base, "POST", "/api/join/"+tok+"/verify", "", map[string]string{"code": w})
	}
	if s != 400 || !strings.Contains(fmt.Sprint(x["error"]), "Call HR for a new code") {
		t.Fatalf("locked code in phone mode: %d %v", s, x)
	}
}
