package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/store"
)

// The joiner's self-fill link (owner's decision of 11 Nov 2026). HR sends the new joiner a link, by email from the
// company mailbox or by Copy link / WhatsApp; the joiner opens it, confirms a 6-digit code emailed to them, fills
// the joining details Form setup gives to the joiner (filled_by) and uploads their documents, then submits to HR.
// HR still sees and can correct every field, fills HR's own fields and saves as before; the joiner never creates
// the Employee ID.
//
// Security:
//   - The link token is 32 random bytes (base64url) and only its SHA-256 is stored. One link per offer is live;
//     a new one replaces the old, which stops working. A link lasts 7 days, works only while HR could still edit the
//     joining details (offer Released, Accepted or Returned by Payroll, candidate not closed) and turns read-only
//     once the joiner submits.
//   - Opening it needs a 6-digit code sent to the email the link was sent to, so a forwarded link is useless.
//     Codes are hashed, last 10 minutes and are single-use; 5 wrong tries lock a code, and 15 wrong codes on a link
//     in all (a new code never gives them back) lock the link (HR sends a new one); 30 seconds between codes, 5 an
//     hour. Every limit is taken in one atomic update of the stored link, so calls sent at the same moment cannot
//     get past it. Without email the code cannot be emailed: HR gives it by phone from the Onboarding panel (shown
//     once), after calling the mobile number on the offer; HR's bell says when the joiner opens the link or asks.
//   - The right code opens a session of 60 minutes (renewed while in use) for that one link: a bearer kept in the
//     browser's sessionStorage, sent as X-Join-Session, stored hashed. It is not a sign-in: no /api route that
//     needs a signed-in user accepts it.
//   - /api/join answers only the company name, the joiner's name, the offer number, type and joining date, and the
//     fields and documents the joiner fills (definitions, current values, uploaded or not): never pay, managers,
//     HR's fields or anyone else's data. Every unusable link gets the same answer, and a link stops when the offer
//     is declined, the candidate is closed or Payroll verifies. Each address may make 300 calls in 10 minutes, and
//     20 unknown-link calls an hour before its unknown links wait (a real link still opens); behind a proxy the
//     address is the one the proxy names (TRUSTED_PROXIES). The token never appears in the API's log.

const (
	joinLinkTTL    = 7 * 24 * time.Hour
	joinCodeTTL    = 10 * time.Minute
	joinCodeTries  = 5
	joinCodeWait   = 30 * time.Second
	joinCodeSends  = 5                             // codes per hour
	joinCodeLocks  = 3                             // locked codes' worth of wrong tries before the link locks
	joinLinkTries  = joinCodeTries * joinCodeLocks // wrong codes on one link in all, over every code
	joinAskWait    = 10 * time.Minute              // between the joiner's Ask HR to call me
	joinSessionTTL = 60 * time.Minute
	joinHeader     = "X-Join-Session"
)

var (
	joinEditable = []string{"Accepted", "Released", "Returned by Payroll"}
	reJoinToken  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

const (
	joinInvalid = "This link has expired or is no longer in use. Ask HR to send you a new link."
	joinMailOff = "Email is not set up on this server, so the link cannot be emailed. Use Copy link or Share on WhatsApp, or ask your administrator to set SMTP_HOST."
)

func (s *Server) joiningRoutes(m *http.ServeMux) {
	// HR's side (signed in, Onboarding rights, data scope).
	m.HandleFunc("GET /api/joiner-link/{id}", s.a(s.joinerLinkGet))
	m.HandleFunc("POST /api/joiner-link/{id}", s.a(s.joinerLinkSend))
	m.HandleFunc("POST /api/joiner-link/{id}/{action}", s.a(s.joinerLinkAct))
	// The joiner's side: no sign-in.
	m.HandleFunc("GET /api/join/{token}", s.joinH(s.joinStatus))
	m.HandleFunc("POST /api/join/{token}/code", s.joinH(s.joinCode))
	m.HandleFunc("POST /api/join/{token}/verify", s.joinH(s.joinVerify))
	m.HandleFunc("GET /api/join/{token}/form", s.joinH(s.joinFormGet))
	m.HandleFunc("PUT /api/join/{token}/form", s.joinH(s.joinFormSave))
	m.HandleFunc("POST /api/join/{token}/files", s.joinH(s.joinUpload))
	m.HandleFunc("POST /api/join/{token}/submit", s.joinH(s.joinSubmit))
}

// ---------- Rate limits of the public pages ----------

// joinLimiter counts, per client address, the calls to /api/join (300 in 10 minutes) and the calls with a link
// that does not exist (20 an hour). Past the first the address waits; past the second only its calls with unknown
// links are refused, so nobody can shut a real joiner out by sending made-up links from the same address (all
// joiners can share one: an office, a mobile network, a proxy that did not say who sent the call).
type joinLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	bad  map[string][]time.Time
}

const (
	joinHitWindow = 10 * time.Minute
	joinHitMax    = 300
	joinBadWindow = time.Hour
	joinBadMax    = 20
)

func newJoinLimiter() *joinLimiter {
	return &joinLimiter{hits: map[string][]time.Time{}, bad: map[string][]time.Time{}}
}

func joinPrune(l []time.Time, since time.Time) []time.Time {
	i := 0
	for i < len(l) && l[i].Before(since) {
		i++
	}
	return l[i:]
}

// allow records a call from ip and says whether it may go ahead.
func (l *joinLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.hits) > 50000 { // forget idle addresses
		for k, v := range l.hits {
			if len(joinPrune(v, now.Add(-joinHitWindow))) == 0 {
				delete(l.hits, k)
			}
		}
		for k, v := range l.bad {
			if len(joinPrune(v, now.Add(-joinBadWindow))) == 0 {
				delete(l.bad, k)
			}
		}
	}
	h := append(joinPrune(l.hits[ip], now.Add(-joinHitWindow)), now)
	l.hits[ip] = h
	return len(h) <= joinHitMax
}

// badLink records a call with a link that does not exist and says whether the address is past its limit.
func (l *joinLimiter) badLink(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := append(joinPrune(l.bad[ip], now.Add(-joinBadWindow)), now)
	l.bad[ip] = b
	return len(b) > joinBadMax
}

// defaultTrustedProxies: the machine itself (the web server or the development proxy in front of the API).
var defaultTrustedProxies = []string{"127.0.0.0/8", "::1/128"}

// parseProxies reads TRUSTED_PROXIES: addresses or CIDR ranges of the proxies in front of the API.
func parseProxies(list []string) ([]*net.IPNet, error) {
	out := []*net.IPNet{}
	for _, x := range list {
		if x = strings.TrimSpace(x); x == "" {
			continue
		}
		if !strings.Contains(x, "/") {
			if ip := net.ParseIP(x); ip != nil && ip.To4() != nil {
				x += "/32"
			} else {
				x += "/128"
			}
		}
		_, n, err := net.ParseCIDR(x)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: %q is not an address or a range", x)
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Server) trustedProxy(ip net.IP) bool {
	for _, n := range s.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address a call came from. When it came through a trusted proxy (TRUSTED_PROXIES; by default the
// machine itself), the proxy's X-Forwarded-For says who sent it: the last address in it that is not a trusted proxy
// (earlier ones can be made up by the sender). Without a trusted proxy the header is ignored.
func (s *Server) clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil || !s.trustedProxy(ip) {
		return host
	}
	hops := []string{}
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				hops = append(hops, x)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		h := net.ParseIP(hops[i])
		if h == nil {
			break // not an address: stop at what the trusted proxies vouched for
		}
		if !s.trustedProxy(h) {
			return h.String()
		}
		host = h.String()
	}
	return host
}

// joinH wraps a public joiner handler: the rate limit, and headers that keep the answers out of caches and the
// link out of Referer headers.
func (s *Server) joinH(fn handlerFn) http.HandlerFunc {
	inner := s.h(func(r *http.Request) (any, error) {
		if !s.joinGuard.allow(s.clientIP(r)) {
			return nil, &apiError{Status: 429, Message: "Too many attempts. Wait a while and try again."}
		}
		return fn(r)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		inner(w, r)
	}
}

// ---------- Shared helpers ----------

func joinSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func joinCodeHash(linkID, code string) string { return sha(linkID + ":" + code) }

func newJoinCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(900000))
	return fmt.Sprintf("%06d", n.Int64()+100000)
}

// maskEmail shows enough of an address to recognise it: me•••@example.com.
func maskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at <= 0 {
		return ""
	}
	user := e[:at]
	if len(user) > 2 {
		user = user[:2]
	} else {
		user = user[:1]
	}
	return user + "•••" + e[at:]
}

func joinTime(v any) (time.Time, bool) {
	t, ok := v.(time.Time)
	return t, ok
}

// joinState is a link's state for HR: live (sent, not opened), opened, submitted, expired, locked or stopped.
func joinState(l store.Row, now time.Time) string {
	switch {
	case l["revoked_at"] != nil:
		return "stopped"
	case l["locked_at"] != nil:
		return "locked"
	case l["submitted_at"] != nil:
		return "submitted"
	}
	if t, ok := joinTime(l["expires_at"]); ok && t.Before(now) {
		return "expired"
	}
	if l["opened_at"] != nil {
		return "opened"
	}
	return "live"
}

// joinOfferEditable: HR could edit the offer's joining details now (the JoiningModal's own rule).
func joinOfferEditable(ctx context.Context, q store.Q, o store.Row) bool {
	if !containsStr(joinEditable, str(o["status"])) {
		return false
	}
	if cand := str(o["candidate_id"]); cand != "" {
		if x, _ := onboardingOne(ctx, q, "candidate", cand); onboardingCol(x, "stage") == "Closed" {
			return false
		}
	}
	return true
}

func (s *Server) joinURL(token string) string { return s.appURL + "/join/" + token }

func (s *Server) mailKind() string {
	if s.mail == nil {
		return "off"
	}
	return s.mail.Kind()
}

func companyName(ctx context.Context, q store.Q) string {
	if cs, _ := onboardingOne(ctx, q, "company_setting", 1); cs != nil && str(cs["name"]) != "" {
		return str(cs["name"])
	}
	return "Saltstayz"
}

// joinerFields are the fields the joiner fills for this offer: theirs in Form setup, asked at joining, switched on
// and for this employment type. UAN is listed too; it is asked only when an existing UAN is declared.
func joinerFields(fs []onbField, typ string) []onbField {
	out := []onbField{}
	for _, f := range fs {
		if f.byJoiner() && f.Join > 0 && f.On && containsStr(f.Types, typ) {
			out = append(out, f)
		}
	}
	return out
}

// joinerDocs are the documents the joiner uploads for this offer (as HR's joining dialog lists them), with the
// stage the HR dialog shows: the stricter of offer release and joining.
func joinerDocs(ds []onbDoc, typ, dept string) []onbDoc {
	out := []onbDoc{}
	for _, d := range ds {
		if d.FilledBy == store.FilledByJoiner && (d.Join > 0 || d.Offer > 0) && d.applies(typ, dept) {
			d.Join = max(d.Join, d.Offer)
			out = append(out, d)
		}
	}
	return out
}

// ---------- Email ----------

func (s *Server) joinLinkMail(company, name, to, url string, expires time.Time, note string) mail.Message {
	exp := expires.In(store.Loc()).Format("02 Jan 2006")
	text := fmt.Sprintf("Dear %s,\n\nWelcome to %s. Please fill in your joining details and upload your documents using this link:\n\n%s\n\n", name, company, url)
	if note != "" {
		text += "A note from HR: " + note + "\n\n"
	}
	text += fmt.Sprintf("The link works until %s. When you open it, we will email you a 6-digit code at this address to confirm it is you. "+
		"You can save and continue later. Please do not share the link or the code with anyone.\n\nIf you have a question, reply to this email.\n\n%s HR\n", exp, company)
	e := html.EscapeString
	h := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;font-size:15px;line-height:1.5;color:#111;max-width:560px">
<p>Dear %s,</p><p>Welcome to %s. Please fill in your joining details and upload your documents using this link:</p>
<p><a href="%s" style="display:inline-block;background:#0E1113;color:#fff;padding:12px 18px;border-radius:8px;text-decoration:none;font-weight:bold">Fill in my joining details</a></p>
<p style="font-size:13px;color:#555">Or copy this address into your browser: %s</p>`, e(name), e(company), e(url), e(url))
	if note != "" {
		h += fmt.Sprintf(`<p><b>A note from HR:</b> %s</p>`, e(note))
	}
	h += fmt.Sprintf(`<p>The link works until <b>%s</b>. When you open it, we will email you a 6-digit code at this address to confirm it is you. You can save and continue later. Please do not share the link or the code with anyone.</p>
<p>If you have a question, reply to this email.</p><p>%s HR</p></div>`, e(exp), e(company))
	return mail.Message{To: to, Subject: "Complete your joining details for " + company, Text: text, HTML: h}
}

func joinCodeMail(company, to, code string) mail.Message {
	text := fmt.Sprintf("Your code for the %s joining form is %s.\n\nIt is valid for 10 minutes. Do not share it with anyone. If you did not ask for it, ignore this email.\n\n%s HR\n", company, code, company)
	h := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;font-size:15px;line-height:1.5;color:#111;max-width:560px"><p>Your code for the %s joining form is</p>
<p style="font-size:28px;font-weight:bold;letter-spacing:4px">%s</p><p>It is valid for 10 minutes. Do not share it with anyone. If you did not ask for it, ignore this email.</p><p>%s HR</p></div>`,
		html.EscapeString(company), code, html.EscapeString(company))
	return mail.Message{To: to, Subject: "Your code for the " + company + " joining form", Text: text, HTML: h}
}

// ---------- HR's side ----------

// joinerLinkView is a link as HR's panel shows it.
func joinerLinkView(l store.Row, now time.Time) map[string]any {
	if l == nil {
		return nil
	}
	return map[string]any{"id": l["id"], "state": joinState(l, now), "createdAt": l["created_at"], "createdBy": l["created_by_name"],
		"expiresAt": l["expires_at"], "sentAt": l["sent_at"], "sentTo": l["sent_to_email"], "openedAt": l["opened_at"], "verifiedAt": l["verified_at"],
		"savedAt": l["saved_at"], "submittedAt": l["submitted_at"], "via": l["via"], "note": l["note"], "joinerKeys": l["joiner_keys"],
		"codeBy": l["code_by"], "lockedAt": l["locked_at"], "revokedAt": l["revoked_at"], "revokeReason": l["revoke_reason"]}
}

// joinerCurrent is the offer's live link, else its newest one (nil when none was ever made).
func joinerCurrent(ctx context.Context, q store.Q, offerID string) (store.Row, error) {
	l, err := q.C(store.JoiningLinkColl).FindOne(ctx, bson.M{"live_offer": offerID})
	if err != nil || l != nil {
		return l, err
	}
	return q.C(store.JoiningLinkColl).FindOne(ctx, bson.M{"offer_id": offerID}, options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}))
}

func (s *Server) joinerStatus(ctx context.Context, q store.Q, c *Ctx, o store.Row) (map[string]any, error) {
	l, err := joinerCurrent(ctx, q, str(o["id"]))
	if err != nil {
		return nil, err
	}
	d := rowData(o)
	email := strings.TrimSpace(d["email"])
	if !reEmail.MatchString(email) {
		email = ""
	}
	mobile := strings.ReplaceAll(d["phone"], " ", "")
	if !reMobile.MatchString(mobile) {
		mobile = ""
	}
	// joinerKeys: every field the joiner has filled in on any of the offer's links (a reopened form is a new link).
	keys := []string{}
	if ks, err := q.C(store.JoiningLinkColl).Distinct(ctx, "joiner_keys", bson.M{"offer_id": o["id"]}); err == nil {
		for _, k := range ks {
			if k, ok := k.(string); ok && k != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
	}
	return map[string]any{"mail": s.mailKind(), "email": email, "mobile": mobile, "editable": joinOfferEditable(ctx, q, o), "status": o["status"],
		"canSend": onbEditor(c), "company": companyName(ctx, q), "link": joinerLinkView(l, time.Now()), "joinerKeys": keys}, nil
}

func (s *Server) joinerLinkGet(r *http.Request, c *Ctx) (any, error) {
	if err := s.canSeeOffers(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	o, err := s.loadOffer(ctx, s.st.DB, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return s.joinerStatus(ctx, s.st.DB, c, o)
}

// joinerNewLink makes the offer's new live link (the old one stops), inside tx. It returns the token.
func joinerNewLink(ctx context.Context, tx store.Q, c *Ctx, o store.Row, via, note string) (string, store.Row, error) {
	now := tx.Now()
	offerID := str(o["id"])
	if _, err := tx.C(store.JoiningLinkColl).UpdateMany(ctx, bson.M{"live_offer": offerID}, bson.M{"$set": bson.M{"live_offer": nil, "revoked_at": now,
		"revoked_by": c.Name + " (" + c.EmployeeID + ")", "revoke_reason": "Replaced by a new link", "session_hash": nil, "code_hash": nil}}); err != nil {
		return "", nil, err
	}
	d := rowData(o)
	email, mobile := strings.TrimSpace(d["email"]), strings.ReplaceAll(d["phone"], " ", "")
	tok := joinSecret()
	doc := bson.M{"offer_id": offerID, "live_offer": offerID, "token_hash": sha(tok), "sent_to_email": strPtr(email), "sent_to_mobile": strPtr(mobile),
		"via": []string{via}, "note": strPtr(note), "created_by": c.EmployeeID, "created_by_name": c.Name, "created_role": strPtr(c.Role),
		"expires_at": now.Add(joinLinkTTL)}
	id, err := tx.C(store.JoiningLinkColl).Insert(ctx, doc)
	if err != nil {
		return "", nil, err
	}
	l, err := tx.C(store.JoiningLinkColl).FindOne(ctx, bson.M{"_id": id})
	return tok, l, err
}

// joinerSendBody: via is how HR is sending it (email, copy, whatsapp, renew = Send a new link). token, when HR's
// browser still holds the live link (made earlier in this session), reuses it instead of making a new one.
type joinerSendBody struct {
	Via   string `json:"via"`
	Token string `json:"token"`
}

// joinerLinkSend makes (or, with the live link's token, reuses) the joiner's link and emails it when asked. The
// raw link is in the answer only: HR copies or shares it from there.
func (s *Server) joinerLinkSend(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	var b joinerSendBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !containsStr([]string{"email", "copy", "whatsapp", "renew"}, b.Via) {
		return nil, bad("Say how the link is sent: email, copy, whatsapp or renew")
	}
	return s.joinerIssue(r.Context(), c, r.PathValue("id"), b.Via, b.Token, "")
}

// joinerIssue makes or reuses the link (see joinerLinkSend); note is HR's note when reopening.
func (s *Server) joinerIssue(ctx context.Context, c *Ctx, offerID, via, token, note string) (any, error) {
	wantMail := via == "email"
	if wantMail && s.mail == nil {
		return nil, bad(joinMailOff)
	}
	var tok, email, name string
	var link store.Row
	reused := false
	err := s.st.Tx(ctx, func(tx store.Q) error {
		tok, link, reused = token, nil, false
		o, err := s.loadOffer(ctx, tx, c, offerID)
		if err != nil {
			return err
		}
		if _, err := tx.C("offer").Lock(ctx, bson.M{"_id": o["id"]}); err != nil {
			return err
		}
		if !joinOfferEditable(ctx, tx, o) {
			return bad("The joiner's link can be sent once the offer is released, until Payroll verifies the joining details")
		}
		name = str(o["candidate_name"])
		email = strings.TrimSpace(rowData(o)["email"])
		if !reEmail.MatchString(email) {
			email = ""
		}
		if wantMail && email == "" {
			return bad("There is no email address on this offer. Use Copy link or Share on WhatsApp")
		}
		cur, err := tx.C(store.JoiningLinkColl).FindOne(ctx, bson.M{"live_offer": o["id"]})
		if err != nil {
			return err
		}
		now := tx.Now()
		if via != "renew" && via != "reopen" && token != "" && cur != nil && subtle.ConstantTimeCompare([]byte(sha(token)), []byte(str(cur["token_hash"]))) == 1 &&
			(joinState(cur, now) == "live" || joinState(cur, now) == "opened") {
			reused, link = true, cur
			if _, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": cur["id"]}, bson.M{"$addToSet": bson.M{"via": via}}); err != nil {
				return err
			}
		} else {
			tok, link, err = joinerNewLink(ctx, tx, c, o, via, note)
			if err != nil {
				return err
			}
		}
		action := "Joiner's link created for " + name + " (" + via + ")"
		switch {
		case via == "reopen":
			action = "Joining details reopened for " + name + ": " + note
		case reused:
			action = "Joiner's link shared again with " + name + " (" + via + ")"
		}
		return audit(ctx, tx, c, action, "offer", str(o["id"]), nil, map[string]any{"link": link["id"], "via": via})
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"url": s.joinURL(tok), "linkId": link["id"], "token": tok, "reused": reused, "emailed": false}
	// Email: when asked, or for a new or reopened link when email is set up and the offer has an address.
	if wantMail || ((via == "renew" || via == "reopen") && s.mail != nil && email != "") {
		comp := companyName(ctx, s.st.DB)
		exp, _ := joinTime(link["expires_at"])
		if err := s.mail.Send(ctx, s.joinLinkMail(comp, name, email, s.joinURL(tok), exp, note)); err != nil {
			out["emailError"] = "The email could not be sent (" + err.Error() + "). The link works: copy it or share it on WhatsApp."
		} else {
			out["emailed"] = true
			_ = s.st.Tx(ctx, func(tx store.Q) error {
				if _, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": link["id"]}, bson.M{"$set": bson.M{"sent_at": tx.Now()}, "$addToSet": bson.M{"via": "email"}}); err != nil {
					return err
				}
				return audit(ctx, tx, c, "Joiner's link emailed to "+email, "offer", offerID, nil, map[string]any{"link": link["id"]})
			})
		}
	}
	if o, err := s.loadOffer(ctx, s.st.DB, c, offerID); err == nil {
		if st, err := s.joinerStatus(ctx, s.st.DB, c, o); err == nil {
			out["status"] = st
		}
	}
	return out, nil
}

// joinerLinkAct: reopen (a new link after the joiner submitted, with HR's note), stop (the live link stops
// working), code (a code HR reads out to the joiner on the phone, shown once).
func (s *Server) joinerLinkAct(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = decode(r, &b)
	ctx, id := r.Context(), r.PathValue("id")
	switch r.PathValue("action") {
	case "reopen":
		note := strings.TrimSpace(b.Note)
		if note == "" || len(note) > 300 {
			return nil, bad("Write a short note for the joiner: what to correct or add")
		}
		o, err := s.loadOffer(ctx, s.st.DB, c, id)
		if err != nil {
			return nil, err
		}
		cur, err := joinerCurrent(ctx, s.st.DB, str(o["id"]))
		if err != nil {
			return nil, err
		}
		if cur == nil || cur["submitted_at"] == nil || cur["revoked_at"] != nil {
			return nil, bad("Reopen is for details the joiner has already submitted. The link is still open for them")
		}
		return s.joinerIssue(ctx, c, id, "reopen", "", note)
	case "stop":
		var out any
		err := s.st.Tx(ctx, func(tx store.Q) error {
			o, err := s.loadOffer(ctx, tx, c, id)
			if err != nil {
				return err
			}
			n, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"live_offer": o["id"]}, bson.M{"$set": bson.M{"live_offer": nil, "revoked_at": tx.Now(),
				"revoked_by": c.Name + " (" + c.EmployeeID + ")", "revoke_reason": "Stopped by HR", "session_hash": nil, "code_hash": nil}})
			if err != nil {
				return err
			}
			if n == 0 {
				return bad("There is no link to stop")
			}
			out = map[string]string{"result": "The link no longer works"}
			return audit(ctx, tx, c, "Joiner's link stopped for "+str(o["candidate_name"]), "offer", str(o["id"]), nil, nil)
		})
		return out, err
	case "code":
		var code string
		var exp time.Time
		err := s.st.Tx(ctx, func(tx store.Q) error {
			o, err := s.loadOffer(ctx, tx, c, id)
			if err != nil {
				return err
			}
			l, err := tx.C(store.JoiningLinkColl).Lock(ctx, bson.M{"live_offer": o["id"]})
			if err != nil {
				return err
			}
			now := tx.Now()
			if l == nil || (joinState(l, now) != "live" && joinState(l, now) != "opened") || !joinOfferEditable(ctx, tx, o) {
				return bad("Send the joiner a link first. A code is only needed while the link is open")
			}
			code, exp = newJoinCode(), now.Add(joinCodeTTL)
			if _, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": l["id"]}, bson.M{"$set": bson.M{"code_hash": joinCodeHash(str(l["id"]), code),
				"code_by": "phone", "code_sent_at": now, "code_expires_at": exp, "code_tries": 0, "code_locked": false}}); err != nil {
				return err
			}
			return audit(ctx, tx, c, "Joining code given by phone to "+str(o["candidate_name"]), "offer", str(o["id"]), nil, nil)
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"code": code, "expiresAt": exp}, nil
	}
	return nil, notFound("")
}

// stopJoinerLink stops the offer's live link (and its session) when the hire no longer needs it: the offer was
// declined, the candidate did not join, or Payroll verified the details. by names who or what stopped it.
func stopJoinerLink(ctx context.Context, tx store.Q, offerID, by, reason string) error {
	if offerID == "" {
		return nil
	}
	_, err := tx.C(store.JoiningLinkColl).UpdateMany(ctx, bson.M{"live_offer": offerID}, bson.M{"$set": bson.M{"live_offer": nil, "revoked_at": tx.Now(),
		"revoked_by": by, "revoke_reason": reason, "session_hash": nil, "code_hash": nil}})
	return err
}

// joinerLinkSummaries is the short link status the pipeline shows per offer: Link sent, Link opened, Joiner
// submitted, Link expired, Link locked.
func joinerLinkSummaries(ctx context.Context, q store.Q, offerIDs []string) map[string]string {
	out := map[string]string{}
	if len(offerIDs) == 0 {
		return out
	}
	links, err := q.C(store.JoiningLinkColl).Find(ctx, bson.M{"live_offer": bson.M{"$in": offerIDs}})
	if err != nil {
		return out
	}
	now := time.Now()
	for _, l := range links {
		out[str(l["offer_id"])] = map[string]string{"live": "Link sent", "opened": "Link opened", "submitted": "Joiner submitted",
			"expired": "Link expired", "locked": "Link locked"}[joinState(l, now)]
	}
	return out
}

// ---------- The joiner's side ----------

// joinCtx is an opened link: the link, the offer as onboarding lists it, and whether the joiner can still edit.
type joinCtx struct {
	link     store.Row
	offer    store.Row
	editable bool // the offer's details are open and the joiner has not submitted
	actor    *Ctx // the joiner, for the audit log
}

// joinAlive says whether the offer still needs the joiner's link: HR can edit the joining details, or the joiner
// has submitted and the details are with Payroll. A declined, closed or verified hire has no use for it, so the
// link stops answering (and its session no longer reads the details).
func joinAlive(ctx context.Context, q store.Q, o store.Row, submitted bool) (open, alive bool) {
	open = joinOfferEditable(ctx, q, o)
	if open {
		return true, true
	}
	if !submitted || str(o["status"]) != "With Payroll" {
		return false, false
	}
	if cand := str(o["candidate_id"]); cand != "" {
		if x, _ := onboardingOne(ctx, q, "candidate", cand); onboardingCol(x, "stage") == "Closed" {
			return false, false
		}
	}
	return false, true
}

// joinOpen finds the link of the token in the path and checks it is usable; with session, also the joiner's
// session. Every unusable link gets the same answer.
func (s *Server) joinOpen(r *http.Request, session bool) (*joinCtx, error) {
	ctx, tok := r.Context(), r.PathValue("token")
	invalid := &apiError{Status: 404, Message: joinInvalid}
	unknown := func() error {
		if s.joinGuard.badLink(s.clientIP(r)) {
			return &apiError{Status: 429, Message: "Too many attempts. Wait a while and try again."}
		}
		return invalid
	}
	if !reJoinToken.MatchString(tok) {
		return nil, unknown()
	}
	l, err := s.st.DB.C(store.JoiningLinkColl).FindOne(ctx, bson.M{"token_hash": sha(tok)})
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, unknown()
	}
	now := time.Now()
	switch joinState(l, now) {
	case "stopped", "expired":
		return nil, invalid
	case "locked":
		return nil, &apiError{Status: 403, Message: joinLinkLocked}
	}
	rows, err := onboardingOffers(ctx, s.st.DB, bson.D{{Key: "_id", Value: l["offer_id"]}}, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, invalid
	}
	o := rows[0]
	o["status"] = offerStatus(o)
	open, alive := joinAlive(ctx, s.st.DB, o, l["submitted_at"] != nil)
	if !alive { // declined, closed, verified, or HR's details moved on before the joiner submitted
		return nil, invalid
	}
	jc := &joinCtx{link: l, offer: o, editable: open && l["submitted_at"] == nil,
		actor: &Ctx{Name: "Joiner " + str(o["candidate_name"]), EmployeeID: str(o["id"]), Role: "Joiner"}}
	if session {
		bearer := strings.TrimSpace(r.Header.Get(joinHeader))
		exp, _ := joinTime(l["session_expires_at"])
		if bearer == "" || l["session_hash"] == nil || exp.Before(now) ||
			subtle.ConstantTimeCompare([]byte(sha(bearer)), []byte(str(l["session_hash"]))) != 1 {
			return nil, unauthorized("Your session has ended. Enter a new code to continue.")
		}
		// Kept alive while in use.
		_, _ = s.st.DB.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": l["id"], "session_hash": l["session_hash"]},
			bson.M{"$set": bson.M{"session_expires_at": now.Add(joinSessionTTL)}})
	}
	return jc, nil
}

const joinLinkLocked = "Too many wrong codes were entered. Ask HR to send you a new link."

// joinCodeTo is where the joiner's code goes: their email when email is set up, else HR gives it by phone.
func (s *Server) joinCodeTo(l store.Row) (by, to string) {
	email := str(l["sent_to_email"])
	if s.mail != nil && reEmail.MatchString(email) {
		return "email", email
	}
	return "phone", ""
}

// joinFromHR: the joiner cannot ask for a code on the page (no email, not development): HR gives it on the phone.
func (s *Server) joinFromHR(l store.Row) bool {
	by, _ := s.joinCodeTo(l)
	return by == "phone" && !s.devOTP
}

// joinNewCodeMsg tells the joiner how to get a new code: on the page, or from HR on the phone.
func (s *Server) joinNewCodeMsg(l store.Row, why string) string {
	if s.joinFromHR(l) {
		return why + " Call HR for a new code, or press Ask HR to call me."
	}
	return why + " Ask for a new code."
}

// joinTellHR puts a message in the bell of whoever made the link: the joiner needs a code given on the phone.
func joinTellHR(ctx context.Context, tx store.Q, l, o store.Row, opened bool) error {
	name, mobile := str(o["candidate_name"]), strings.ReplaceAll(rowData(o)["phone"], " ", "")
	text := name + " needs a code for the joining form (" + str(o["id"]) + ")."
	if opened {
		text = name + " opened the joining link and needs a code (" + str(o["id"]) + ")."
	}
	if mobile != "" {
		text += " Call them on " + mobile + " and use Give a code by phone."
	} else {
		text += " Call them and use Give a code by phone."
	}
	return sendNotice(ctx, tx, str(l["created_by"]), "", text, "/onboarding?id="+str(o["id"]), str(l["created_role"]), nil)
}

// joinStatus is what the link shows before the code: the company, whether a code is needed (or the details were
// sent), where the code goes, and who in HR made the link. The first open is recorded; without email, HR is told
// in the bell that the joiner is waiting for a code.
func (s *Server) joinStatus(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, false)
	if err != nil {
		return nil, err
	}
	ctx, l := r.Context(), jc.link
	fromHR := s.joinFromHR(l)
	if l["opened_at"] == nil {
		_ = s.st.Tx(ctx, func(tx store.Q) error {
			set := bson.M{"opened_at": tx.Now()}
			if fromHR && l["submitted_at"] == nil {
				set["asked_at"] = tx.Now()
			}
			n, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": l["id"], "opened_at": nil}, bson.M{"$set": set})
			if err != nil || n == 0 {
				return err
			}
			if fromHR && l["submitted_at"] == nil {
				if err := joinTellHR(ctx, tx, l, jc.offer, true); err != nil {
					return err
				}
			}
			return audit(ctx, tx, jc.actor, "Joiner opened the link", "offer", str(l["offer_id"]), nil, map[string]any{"link": l["id"]})
		})
	}
	state := "code"
	if l["submitted_at"] != nil {
		state = "submitted"
	}
	by, to := s.joinCodeTo(l)
	out := map[string]any{"company": companyName(ctx, s.st.DB), "state": state, "codeBy": by, "codeTo": maskEmail(to), "hrName": l["created_by_name"],
		"askHR": fromHR}
	if exp, ok := joinTime(l["code_expires_at"]); ok && exp.After(time.Now()) && l["code_hash"] != nil && l["code_locked"] != true {
		out["codeOut"] = true // a code is waiting to be entered
	}
	return out, nil
}

// joinAskHR: without email the joiner presses Ask HR to call me; HR gets a message in the bell, at most once in
// 10 minutes (taken in one atomic update, so a burst of presses sends one).
func (s *Server) joinAskHR(ctx context.Context, jc *joinCtx) (any, error) {
	now := time.Now()
	l := jc.link
	var told bool
	err := s.st.Tx(ctx, func(tx store.Q) error {
		told = false
		n, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": l["id"], "submitted_at": nil, "revoked_at": nil, "locked_at": nil,
			"$or": bson.A{bson.M{"asked_at": nil}, bson.M{"asked_at": bson.M{"$lte": now.Add(-joinAskWait)}}}}, bson.M{"$set": bson.M{"asked_at": now}})
		if err != nil || n == 0 {
			return err
		}
		told = true
		return joinTellHR(ctx, tx, l, jc.offer, false)
	})
	if err != nil {
		return nil, err
	}
	msg := "We have told " + str(l["created_by_name"]) + " in HR. They will call you with a code."
	if !told {
		msg = "HR has already been told. They will call you with a code. If nobody calls within 10 minutes, ask again."
	}
	return map[string]any{"sent": false, "by": "phone", "asked": true, "message": msg}, nil
}

// joinCode emails the joiner a new code (or, in development, returns it); without email it asks HR to call.
func (s *Server) joinCode(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, false)
	if err != nil {
		return nil, err
	}
	ctx, l := r.Context(), jc.link
	if l["submitted_at"] != nil {
		return nil, bad("You have already sent your details to HR.")
	}
	if s.joinFromHR(l) {
		return s.joinAskHR(ctx, jc)
	}
	by, to := s.joinCodeTo(l)
	links := s.st.DB.C(store.JoiningLinkColl)
	now := time.Now()
	// A new hour of codes: one request starts it (the filter makes the reset happen once).
	if _, err := links.Update(ctx, bson.M{"_id": l["id"], "$or": bson.A{bson.M{"code_window_at": nil}, bson.M{"code_window_at": bson.M{"$lte": now.Add(-time.Hour)}}}},
		bson.M{"$set": bson.M{"code_sends": 0, "code_window_at": now}}); err != nil {
		return nil, err
	}
	code, codeBy := newJoinCode(), by
	if by == "phone" { // development without email: the code is shown on the page
		codeBy = "dev"
	}
	hash := joinCodeHash(str(l["id"]), code)
	// The 30-second wait and the 5 an hour are checked and taken in one atomic update, so requests sent at the same
	// moment get one code (and one email) between them. A new code gets 5 tries of its own; the link's 15 wrong codes
	// in all (code_fails) carry on.
	got, err := links.FindOneAndUpdate(ctx, bson.M{"_id": l["id"], "submitted_at": nil, "revoked_at": nil, "locked_at": nil,
		"code_sends": bson.M{"$lt": joinCodeSends},
		"$or":        bson.A{bson.M{"code_sent_at": nil}, bson.M{"code_sent_at": bson.M{"$lte": now.Add(-joinCodeWait)}}, bson.M{"code_by": "phone"}}},
		bson.M{"$set": bson.M{"code_hash": hash, "code_by": codeBy, "code_sent_at": now, "code_expires_at": now.Add(joinCodeTTL), "code_tries": 0, "code_locked": false},
			"$inc": bson.M{"code_sends": 1}})
	if err != nil {
		return nil, err
	}
	if got == nil {
		cur, err := links.FindOne(ctx, bson.M{"_id": l["id"]})
		switch {
		case err != nil:
			return nil, err
		case cur == nil || cur["revoked_at"] != nil:
			return nil, &apiError{Status: 404, Message: joinInvalid}
		case cur["locked_at"] != nil:
			return nil, &apiError{Status: 403, Message: joinLinkLocked}
		case cur["submitted_at"] != nil:
			return nil, bad("You have already sent your details to HR.")
		case toInt(cur["code_sends"]) >= joinCodeSends:
			return nil, &apiError{Status: 429, Message: "You have asked for 5 codes in the last hour. Try again later, or ask HR."}
		}
		return nil, bad("Wait 30 seconds before asking for a new code.")
	}
	out := map[string]any{"sent": by == "email", "by": by, "to": maskEmail(to)}
	if by == "email" {
		if err := s.mail.Send(ctx, joinCodeMail(companyName(ctx, s.st.DB), to, code)); err != nil {
			_, _ = links.Update(ctx, bson.M{"_id": l["id"], "code_hash": hash}, bson.M{"$set": bson.M{"code_hash": nil}})
			return nil, &apiError{Status: 503, Message: "We could not send the email just now. Try again in a minute, or ask HR."}
		}
	}
	if s.devOTP {
		out["devCode"] = code
	}
	return out, nil
}

// joinVerifyRefused explains why a try could not be counted, from the link as it is now.
func (s *Server) joinVerifyRefused(ctx context.Context, id any, now time.Time) error {
	l, err := s.st.DB.C(store.JoiningLinkColl).FindOne(ctx, bson.M{"_id": id})
	switch {
	case err != nil:
		return err
	case l == nil || l["revoked_at"] != nil:
		return &apiError{Status: 404, Message: joinInvalid}
	case l["locked_at"] != nil || toInt(l["code_fails"]) >= joinLinkTries:
		return &apiError{Status: 403, Message: joinLinkLocked}
	case l["code_hash"] == nil:
		if s.joinFromHR(l) {
			return bad("HR has not given you a code yet. Call HR, or press Ask HR to call me.")
		}
		return bad("Ask for a code first.")
	case l["code_locked"] == true || toInt(l["code_tries"]) >= joinCodeTries:
		return bad("%s", s.joinNewCodeMsg(l, "Too many wrong codes."))
	}
	if exp, _ := joinTime(l["code_expires_at"]); exp.Before(now) {
		return bad("%s", s.joinNewCodeMsg(l, "The code has expired."))
	}
	return bad("Ask for a code first.")
}

// joinVerify checks the code and opens the joiner's session.
//
// Every try, right or wrong, first takes one of the code's 5 tries and one of the link's 15 in a single atomic
// update of the stored link, and the code is then compared with the code stored at that moment. Tries sent at the
// same moment therefore cannot get past the limits (at most 5 are compared per code, 15 per link), a right code
// after the limit is refused like a wrong one, and a new code never gives back the link's tries.
func (s *Server) joinVerify(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, false)
	if err != nil {
		return nil, err
	}
	var b struct {
		Code string `json:"code"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), jc.link["id"]
	links := s.st.DB.C(store.JoiningLinkColl)
	now := time.Now()
	after, err := links.FindOneAndUpdate(ctx, bson.M{"_id": id, "revoked_at": nil, "locked_at": nil, "code_hash": bson.M{"$type": "string"},
		"code_locked": false, "code_expires_at": bson.M{"$gt": now}, "code_tries": bson.M{"$lt": joinCodeTries},
		"code_fails": bson.M{"$not": bson.M{"$gte": joinLinkTries}}},
		bson.M{"$inc": bson.M{"code_tries": 1, "code_fails": 1}})
	if err != nil {
		return nil, err
	}
	if after == nil {
		return nil, s.joinVerifyRefused(ctx, id, now)
	}
	code := strings.TrimSpace(b.Code)
	if subtle.ConstantTimeCompare([]byte(joinCodeHash(str(id), code)), []byte(str(after["code_hash"]))) == 1 {
		bearer := joinSecret()
		set := bson.M{"session_hash": sha(bearer), "session_expires_at": now.Add(joinSessionTTL), "code_hash": nil, "code_tries": 0}
		if after["verified_at"] == nil {
			set["verified_at"] = now
		}
		// The right code gives back the try it took; it is used once (another request may have used it a moment ago).
		n, err := links.Update(ctx, bson.M{"_id": id, "code_hash": after["code_hash"], "locked_at": nil, "revoked_at": nil},
			bson.M{"$set": set, "$inc": bson.M{"code_fails": -1}})
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, s.joinVerifyRefused(ctx, id, now)
		}
		return map[string]any{"session": bearer, "expiresIn": int(joinSessionTTL.Seconds())}, nil
	}
	tries, fails := toInt(after["code_tries"]), toInt(after["code_fails"])
	if fails >= joinLinkTries { // the link's last try: it locks, and HR sends a new link
		_ = s.st.Tx(ctx, func(tx store.Q) error {
			n, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": id, "locked_at": nil},
				bson.M{"$set": bson.M{"locked_at": now, "code_locked": true, "code_hash": nil, "session_hash": nil}, "$inc": bson.M{"code_locks": 1}})
			if err != nil || n == 0 {
				return err
			}
			return audit(ctx, tx, jc.actor, "Joiner's link locked after too many wrong codes", "offer", str(jc.link["offer_id"]), nil, map[string]any{"link": id})
		})
		return nil, &apiError{Status: 403, Message: joinLinkLocked}
	}
	if tries >= joinCodeTries {
		_, _ = links.Update(ctx, bson.M{"_id": id, "code_hash": after["code_hash"], "code_locked": false},
			bson.M{"$set": bson.M{"code_locked": true}, "$inc": bson.M{"code_locks": 1}})
		return nil, bad("%s", s.joinNewCodeMsg(after, "Too many wrong codes."))
	}
	left := min(joinCodeTries-tries, joinLinkTries-fails)
	return nil, bad("That code is not correct. %d %s left.", left, map[bool]string{true: "try", false: "tries"}[left == 1])
}

// joinFieldView is a field as the joiner's form shows it.
type joinFieldView struct {
	Key      string            `json:"key"`
	Label    string            `json:"label"`
	Section  string            `json:"section"`
	Kind     string            `json:"kind"`
	Options  []string          `json:"options"`
	Required bool              `json:"required"`
	OnlyIf   map[string]string `json:"onlyIf,omitempty"`
}

// joinOptions are a drop-down's choices: its own list, or the active qualifications.
func joinOptions(ctx context.Context, q store.Q, f onbField) []string {
	out := []string{}
	if f.Kind != "sel" {
		return out
	}
	if f.Options != nil && strings.TrimSpace(*f.Options) != "" {
		for _, x := range strings.Split(*f.Options, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	if f.Key == "qual" {
		rows, _ := q.C("simple_master").Find(ctx, bson.M{"kind": "qualification", "status": "Active"}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
		for _, r := range rows {
			out = append(out, str(r["name"]))
		}
	}
	return out
}

// joinForm is the joiner's whole form: who and what (no pay, no managers), their fields with the values so far,
// and their documents with what is uploaded.
func (s *Server) joinForm(ctx context.Context, jc *joinCtx) (map[string]any, error) {
	q, o := s.st.DB, jc.offer
	fs, ds, err := loadSetup(ctx, q)
	if err != nil {
		return nil, err
	}
	typ, dept := str(o["type"]), str(o["department"])
	d := rowData(o)
	byJoiner := map[string]bool{}
	if ks, err := q.C(store.JoiningLinkColl).Distinct(ctx, "joiner_keys", bson.M{"offer_id": o["id"]}); err == nil {
		for _, k := range ks {
			byJoiner[str(k)] = true
		}
	}
	fields, values := []joinFieldView{}, map[string]string{}
	for _, f := range joinerFields(fs, typ) {
		v := joinFieldView{Key: f.Key, Label: f.Label, Section: f.Section, Kind: f.Kind, Options: joinOptions(ctx, q, f), Required: f.Join == 2}
		switch f.Key {
		case "uan":
			v.OnlyIf = map[string]string{"uanAvail": "Yes"}
		case "spouse":
			v.OnlyIf = map[string]string{"marital": "Married"}
		case "pfChoice":
			// Asked only for a gross above the threshold (the joiner does not see the gross), once they say they have no UAN.
			if !f.applies(typ, map[string]string{"uanAvail": "No", "gross": d["gross"]}) {
				continue
			}
			v.OnlyIf = map[string]string{"uanAvail": "No"}
		case "uanAvail":
			// Only the joiner knows: the offer's "No" (the offer form's default) is not shown as their answer, and
			// they must choose, so a UAN to transfer is not missed.
			v.Required = true
		}
		fields = append(fields, v)
		if x, ok := d[f.Key]; ok && (f.Key != "uanAvail" || byJoiner[f.Key]) {
			values[f.Key] = x
		}
	}
	kind, owner := offerDocOwner(o)
	have, err := docsOf(ctx, q, kind, owner)
	if err != nil {
		return nil, err
	}
	docs := []map[string]any{}
	for _, x := range joinerDocs(ds, typ, dept) {
		var up any
		if u := have[x.Key]; u != nil {
			up = map[string]any{"fileName": u["file_name"], "uploadedAt": u["uploaded_at"]}
		}
		docs = append(docs, map[string]any{"key": x.Key, "label": x.Label, "required": x.Join == 2, "uploaded": up})
	}
	return map[string]any{"company": companyName(ctx, q), "name": o["candidate_name"], "offerNo": o["id"], "type": typ, "doj": o["doj"],
		"note": jc.link["note"], "editable": jc.editable, "submittedAt": jc.link["submitted_at"], "fields": fields, "values": values, "docs": docs}, nil
}

func (s *Server) joinFormGet(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, true)
	if err != nil {
		return nil, err
	}
	return s.joinForm(r.Context(), jc)
}

// joinCheckValues validates what the joiner sent for their fields: the formats HR's save checks (checkFormat), and
// each value against its field's kind (a choice from the list, a date, a number, an email, a 10-digit mobile).
func joinCheckValues(fs map[string]onbField, opts map[string][]string, in map[string]string) []string {
	errs := []string{}
	checkFormat(in, &errs)
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, f := in[k], fs[k]
		if v == "" {
			continue
		}
		switch f.Kind {
		case "sel":
			if l := opts[k]; len(l) > 0 && !containsStr(l, v) {
				errs = append(errs, f.Label+": choose one of the options")
			}
		case "date":
			if !isDate(v) {
				errs = append(errs, f.Label+" must be a date")
			}
		case "number":
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				errs = append(errs, f.Label+" must be a number")
			}
		case "email":
			if k != "email" && !reEmail.MatchString(v) {
				errs = append(errs, f.Label+" looks invalid")
			}
		case "tel":
			if k != "phone" && k != "ecPhone" && !reMobile.MatchString(strings.ReplaceAll(v, " ", "")) {
				errs = append(errs, f.Label+" must be 10 digits")
			}
		}
	}
	return dedupe(errs)
}

// onlyIfMet: a field shown only on other answers (OnlyIf) is asked when every one of them is given.
func onlyIfMet(only, values map[string]string) bool {
	for k, v := range only {
		if values[k] != v {
			return false
		}
	}
	return true
}

// joinFixMsg says, for the joiner, which field is wrong and what to type (the wording of the hints on their form).
var joinFixMsg = map[string]string{
	"aadhaar": "Aadhaar: enter the 12 digits", "pan": "PAN: 10 characters, like ABCDE1234F", "ifsc": "IFSC: 11 characters, like HDFC0001234",
	"acct": "Account number: 6 to 18 digits, no spaces or dashes", "uan": "UAN: 12 digits", "ecPhone": "Emergency contact mobile: enter 10 digits",
	"phone": "Mobile number: enter 10 digits", "dob": "Date of birth: must be a past date", "email": "Personal email: check the address",
}

// joinProblems checks the joiner's values one field at a time: the keys that are wrong and, for each, what to fix.
func joinProblems(fs map[string]onbField, opts map[string][]string, in map[string]string) (map[string]bool, []string) {
	wrong, msgs := map[string]bool{}, []string{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		errs := joinCheckValues(fs, opts, map[string]string{k: in[k]})
		if len(errs) == 0 {
			continue
		}
		wrong[k] = true
		if m := joinFixMsg[k]; m != "" {
			msgs = append(msgs, m)
		} else {
			msgs = append(msgs, errs...)
		}
	}
	return wrong, dedupe(msgs)
}

// joinNormalize keeps numbers as digits: the spaces people type in Aadhaar, account, UAN and mobile numbers go.
func joinNormalize(fs map[string]onbField, in map[string]string) {
	for k, v := range in {
		switch {
		case k == "aadhaar" || k == "acct" || k == "uan" || fs[k].Kind == "tel":
			in[k] = strings.Join(strings.Fields(v), "")
		case k == "pan" || k == "ifsc":
			in[k] = strings.ToUpper(v)
		}
	}
}

// joinFormSave saves the joiner's draft: only their own fields; any other key is refused. A value in the wrong
// format is not saved and is listed in problems; the rest is saved, so the joiner can stop half way and come back.
func (s *Server) joinFormSave(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, true)
	if err != nil {
		return nil, err
	}
	if !jc.editable {
		return nil, bad("Your details are with HR now and can no longer be changed here.")
	}
	var b struct {
		Data map[string]any `json:"data"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	in := cleanData(b.Data)
	fs, _, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	typ := str(jc.offer["type"])
	mine, opts := map[string]onbField{}, map[string][]string{}
	for _, f := range joinerFields(fs, typ) {
		mine[f.Key] = f
		opts[f.Key] = joinOptions(ctx, s.st.DB, f)
	}
	labels := map[string]string{}
	for _, f := range fs {
		labels[f.Key] = f.Label
	}
	notMine := []string{}
	for k := range b.Data {
		if _, ok := mine[k]; !ok {
			l := labels[k]
			if l == "" {
				l = k
			}
			notMine = append(notMine, l)
		}
	}
	if len(notMine) > 0 {
		sort.Strings(notMine)
		return nil, badD("These details are filled in by HR, not on your form: "+strings.Join(notMine, ", "), notMine)
	}
	joinNormalize(mine, in)
	wrong, problems := joinProblems(mine, opts, in)
	skipped := []string{}
	for k := range wrong {
		delete(in, k)
		skipped = append(skipped, k)
	}
	sort.Strings(skipped)
	err = s.st.Tx(ctx, func(tx store.Q) error {
		o, err := tx.C("offer").Lock(ctx, bson.M{"_id": jc.offer["id"]})
		if err != nil {
			return err
		}
		if o == nil {
			return notFound(joinInvalid)
		}
		set, filled := bson.D{}, []string{}
		for k, v := range in {
			set = append(set, bson.E{Key: "data." + k, Value: v})
			if v != "" {
				filled = append(filled, k)
			}
		}
		sort.Slice(set, func(i, j int) bool { return set[i].Key < set[j].Key })
		sort.Strings(filled)
		if len(set) > 0 {
			if _, err := tx.C("offer").Update(ctx, bson.M{"_id": o["id"]}, bson.D{{Key: "$set", Value: set}}); err != nil {
				return err
			}
		}
		up := bson.M{"$set": bson.M{"saved_at": tx.Now()}}
		if len(filled) > 0 {
			up["$addToSet"] = bson.M{"joiner_keys": bson.M{"$each": filled}}
		}
		if _, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": jc.link["id"]}, up); err != nil {
			return err
		}
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// The audit names what changed, not the values (Aadhaar, PAN and bank details stay out of the log).
		return audit(ctx, tx, jc.actor, "Joiner saved joining details", "offer", str(o["id"]), nil, map[string]any{"fields": keys})
	})
	if err != nil {
		return nil, err
	}
	if _, ok := in["pan"]; ok {
		s.panCheckOffer(ctx, jc.actor, str(jc.offer["id"])) // the joiner's PAN, when verification is on (b1_pan.go)
	}
	return map[string]any{"saved": true, "problems": problems, "skipped": skipped}, nil
}

// joinUpload stores a document the joiner uploads, with the offer's other documents (the candidate SI-…, or an
// offer raised before stages), where HR sees it. It never creates the Employee ID: HR's save does.
func (s *Server) joinUpload(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, true)
	if err != nil {
		return nil, err
	}
	if !jc.editable {
		return nil, bad("Your details are with HR now and can no longer be changed here.")
	}
	key, err := uploadForm(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	_, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, d := range joinerDocs(ds, str(jc.offer["type"]), str(jc.offer["department"])) {
		ok = ok || d.Key == key
	}
	if !ok {
		return nil, bad("This document is not asked for on your form")
	}
	f, err := uploadFile(r)
	if err != nil {
		return nil, err
	}
	kind, owner := offerDocOwner(jc.offer)
	row, err := s.storeDoc(ctx, jc.actor, kind, owner, key, f, str(jc.offer["candidate_name"])+" (joiner)")
	if err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "fileName": row["file_name"], "uploadedAt": row["uploaded_at"]}, nil
}

// joinSubmit sends the details to HR: every required field and document must be in, and the formats right. The
// link then turns read-only, and whoever sent it (and whoever raised the offer) gets a message in the bell.
func (s *Server) joinSubmit(r *http.Request) (any, error) {
	jc, err := s.joinOpen(r, true)
	if err != nil {
		return nil, err
	}
	if !jc.editable {
		return nil, bad("You have already sent your details to HR.")
	}
	ctx := r.Context()
	form, err := s.joinForm(ctx, jc)
	if err != nil {
		return nil, err
	}
	values := form["values"].(map[string]string)
	// Everything to fix, in one list: each line names the field or document and what to do.
	fs, _, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	mine, opts := map[string]onbField{}, map[string][]string{}
	for _, f := range joinerFields(fs, str(jc.offer["type"])) {
		mine[f.Key] = f
		opts[f.Key] = joinOptions(ctx, s.st.DB, f)
	}
	todo, asked := []string{}, map[string]string{}
	for _, f := range form["fields"].([]joinFieldView) {
		if !onlyIfMet(f.OnlyIf, values) {
			continue
		}
		if v := strings.TrimSpace(values[f.Key]); v != "" {
			asked[f.Key] = v
		} else if f.Required {
			verb := "fill this in"
			if f.Kind == "sel" {
				verb = "choose one"
			}
			todo = append(todo, f.Label+": "+verb)
		}
	}
	_, wrongs := joinProblems(mine, opts, asked)
	todo = append(todo, wrongs...)
	for _, d := range form["docs"].([]map[string]any) {
		if d["required"] == true && d["uploaded"] == nil {
			todo = append(todo, "Upload your "+d["label"].(string))
		}
	}
	if len(todo) > 0 {
		return nil, badD("Please fix these before you submit: "+strings.Join(todo, "; "), todo)
	}
	var at time.Time
	err = s.st.Tx(ctx, func(tx store.Q) error {
		l, err := tx.C(store.JoiningLinkColl).Lock(ctx, bson.M{"_id": jc.link["id"]})
		if err != nil {
			return err
		}
		if l == nil || l["submitted_at"] != nil || l["revoked_at"] != nil {
			return bad("You have already sent your details to HR.")
		}
		at = tx.Now()
		if _, err := tx.C(store.JoiningLinkColl).Update(ctx, bson.M{"_id": l["id"]}, bson.M{"$set": bson.M{"submitted_at": at}}); err != nil {
			return err
		}
		o := jc.offer
		offerID, name := str(o["id"]), str(o["candidate_name"])
		raw, err := onboardingOne(ctx, tx, "offer", offerID)
		if err != nil {
			return err
		}
		link := "/onboarding"
		if cand := str(o["candidate_id"]); cand != "" {
			link = "/onboarding?id=" + offerID
		}
		text := fmt.Sprintf("%s submitted joining details (%s)", name, offerID)
		sent := map[string]bool{}
		for _, to := range [][2]string{{str(l["created_by"]), str(l["created_role"])}, {str(onboardingCol(raw, "raised_by")), ""}} {
			if to[0] == "" || sent[to[0]] {
				continue
			}
			sent[to[0]] = true
			if err := sendNotice(ctx, tx, to[0], "", text, link, to[1], nil); err != nil {
				return err
			}
		}
		return audit(ctx, tx, jc.actor, "Joiner submitted joining details", "offer", offerID, nil, map[string]any{"link": l["id"]})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"submittedAt": at}, nil
}
