package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/store"
)

// Sign-in rules (spec 6.5).
const (
	otpTTL          = 5 * time.Minute
	otpMaxTries     = 3
	otpResend       = 30 * time.Second
	passwordGrace   = 10 * time.Minute
	loginMaxFails   = 5
	loginLock       = 5 * time.Minute // owner, 27 Nov 2026: was 15
	sessionLifetime = 12 * time.Hour
)

var (
	hashPassword = access.HashPassword
	randomHex    = access.RandomHex
)

// SMSSender delivers a free-text message by SMS or WhatsApp (nothing uses it yet).
type SMSSender func(mobile, message string) error

// OTPSender texts a one-time code. The wording is the provider's approved template; only the code goes in
// (MSG91 flow, internal/sms). Nil means the OTP cannot be sent.
type OTPSender func(ctx context.Context, mobile, code string) error

func (s *Server) newSession(ctx context.Context, q store.Q, employeeID string) (string, error) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	_, err := q.C("auth_session").Insert(ctx, bson.M{"token_hash": sha(tok), "employee_id": employeeID, "expires_at": time.Now().Add(sessionLifetime)})
	return tok, err
}

// authNewest sorts a person's OTPs newest first.
func authNewest() *options.FindOneOptionsBuilder {
	return options.FindOne().SetSort(bson.D{{Key: "sent_at", Value: -1}})
}

// authMobile is the mobile number on record for the person ("" when there is none).
func authMobile(ctx context.Context, q store.Q, id string) (string, error) {
	p, err := q.C("employee_personal").FindOne(ctx, bson.M{"employee_id": id})
	if err != nil || p == nil {
		return "", err
	}
	return str(p["mobile"]), nil
}

// authEmail is the personal email on record ("" when there is none).
func authEmail(ctx context.Context, q store.Q, id string) string {
	p, err := q.C("employee_personal").FindOne(ctx, bson.M{"employee_id": id})
	if err != nil || p == nil {
		return ""
	}
	return strings.TrimSpace(str(p["email"]))
}

// otpMail is the sign-in code by email: the same words as the SMS, and a note for someone who did not ask.
func otpMail(company, name, to, code string) mail.Message {
	hi := "Hello"
	if name != "" {
		hi = "Dear " + name
	}
	text := fmt.Sprintf("%s,\n\nYour %s HRMS OTP is %s. It is valid for 5 minutes. Do not share it with anyone.\n\nIf you did not ask for it, ignore this email and tell HR.\n\n%s HR\n", hi, company, code, company)
	e := html.EscapeString
	h := fmt.Sprintf(`<div style="font-family:Arial,sans-serif;font-size:15px;line-height:1.5;color:#111;max-width:520px">
<p>%s,</p><p>Your %s HRMS OTP is</p>
<p style="font-size:28px;font-weight:bold;letter-spacing:6px;font-family:monospace;margin:8px 0">%s</p>
<p>It is valid for 5 minutes. Do not share it with anyone.</p>
<p style="color:#666;font-size:13px">If you did not ask for it, ignore this email and tell HR.</p><p>%s HR</p></div>`, e(hi), e(company), code, e(company))
	return mail.Message{To: to, Subject: company + " HRMS sign-in OTP", Text: text, HTML: h}
}

type idBody struct {
	EmployeeID string `json:"employeeId"`
	Purpose    string `json:"purpose,omitempty"`
	Code       string `json:"code,omitempty"`
	Password   string `json:"password,omitempty"`
	// VerifyToken is what /otp/verify returned: the password call must carry it, so only the person who entered
	// the OTP can set the password (AUTH-023).
	VerifyToken string `json:"verifyToken,omitempty"`
}

var reIDNoHyphen = regexp.MustCompile(`^([A-Z]+)(\d+)$`)

// authID is the Employee ID as typed at sign-in: numbers alone become EMP-<n> (spec 6.5.2), and a code typed without
// its hyphen (EMP1002, emp1002, EMP 1002) is found as EMP-1002 when no employee has the code as typed (AUTH-003).
func (s *Server) authID(ctx context.Context, raw string) string {
	id := access.NormaliseEmployeeID(raw)
	if m := reIDNoHyphen.FindStringSubmatch(id); m != nil {
		if ok, _ := s.st.DB.C("employee").Exists(ctx, bson.M{"_id": id}); !ok {
			return m[1] + "-" + m[2]
		}
	}
	return id
}

func (s *Server) otpSend(r *http.Request) (any, error) {
	var b idBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Purpose != "first" && b.Purpose != "forgot" {
		return nil, bad("purpose must be first or forgot")
	}
	ctx, id, ip := r.Context(), s.authID(r.Context(), b.EmployeeID), s.clientIP(r)
	if err := s.authGuard.blocked(ip, id); err != nil {
		return nil, err
	}
	if err := s.authGuard.otpSend(ip, id); err != nil {
		return nil, err
	}
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if e == nil {
		s.authGuard.failed(ip, id)
		return nil, bad("We could not find that Employee ID. Check it, or ask HR.")
	}
	if e["status"] == "Pre-boarding" {
		return nil, bad("Your account opens on your joining date.")
	}
	mobile, err := authMobile(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	mask := access.MaskMobile(mobile)
	// The OTP also goes to the personal email on record when the company mailbox is set up (owner, 27 Nov 2026).
	// The development echo would write the code to the server log: never used for OTPs.
	email := authEmail(ctx, s.st.DB, id)
	byMail := s.mail != nil && s.mail.Kind() != "echo" && email != ""
	bySMS := mask != "" && (s.otp != nil || s.sms != nil)
	if mask == "" && !byMail {
		return nil, bad("There is no mobile number on record for this Employee ID. Ask HR to add it.")
	}
	if !bySMS && !byMail && !s.devOTP { // never say "sent" when nothing can be sent
		return nil, bad("OTP by SMS is not set up yet. Ask HR or IT.")
	}
	if last, err := s.st.DB.C("auth_otp").FindOne(ctx, bson.M{"employee_id": id}, authNewest()); err == nil && last != nil {
		if at, ok := last["sent_at"].(time.Time); ok && time.Since(at) < otpResend {
			return nil, bad("Wait 30 seconds before asking for a new OTP.")
		}
	}
	if err := s.otpDailyCap(ctx, id); err != nil {
		return nil, err
	}
	hasPw, _ := s.st.DB.C("auth_credential").Exists(ctx, bson.M{"employee_id": id})
	purpose := b.Purpose
	if purpose == "first" && hasPw {
		purpose = "forgot" // "first time" by someone with a password is a reset
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(900000))
	code := fmt.Sprintf("%06d", n.Int64()+100000)
	err = s.st.Tx(ctx, func(tx store.Q) error {
		// A new OTP replaces any earlier one still open.
		if _, err := tx.C("auth_otp").UpdateMany(ctx, bson.M{"employee_id": id, "used": false}, bson.M{"$set": bson.M{"used": true}}); err != nil {
			return err
		}
		_, err := tx.C("auth_otp").Insert(ctx, bson.M{"employee_id": id, "code_hash": sha(id + ":" + code), "purpose": purpose, "expires_at": time.Now().Add(otpTTL)})
		return err
	})
	if err != nil {
		return nil, err
	}
	smsOK, mailOK := false, false
	if bySMS {
		var err error
		if s.otp != nil {
			err = s.otp(ctx, mobile, code)
		} else {
			err = s.sms(mobile, "Your Saltstayz HRMS OTP is "+code+". Valid for 5 minutes. Do not share it.")
		}
		if err != nil {
			log.Printf("OTP SMS to %s failed: %v", mask, err) // the code is never logged
		}
		smsOK = err == nil
	}
	if byMail {
		err := s.mail.Send(ctx, otpMail(companyName(ctx, s.st.DB), str(e["name"]), email, code))
		if err != nil {
			log.Printf("OTP email to %s failed: %v", maskEmail(email), err)
		}
		mailOK = err == nil
	}
	if (bySMS || byMail) && !smsOK && !mailOK {
		return nil, bad("We could not send the OTP right now. Try again in a minute.")
	}
	out := map[string]any{"employeeId": id, "purpose": purpose}
	if smsOK || (!bySMS && !mailOK && mask != "") { // dev echo shows the SMS preview to the mobile
		out["mask"] = mask
	}
	if mailOK {
		out["emailMask"] = maskEmail(email)
	}
	if s.devOTP {
		out["devOtp"] = code
	}
	return out, nil
}

func (s *Server) otpVerify(r *http.Request) (any, error) {
	var b idBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id, ip := r.Context(), s.authID(r.Context(), b.EmployeeID), s.clientIP(r)
	if err := s.authGuard.blocked(ip, id); err != nil {
		return nil, err
	}
	otps := s.st.DB.C("auth_otp")
	o, err := otps.FindOne(ctx, bson.M{"employee_id": id, "used": false}, authNewest())
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, bad("Request an OTP first.")
	}
	if o["expires_at"].(time.Time).Before(time.Now()) {
		_, _ = otps.Update(ctx, bson.M{"_id": o["id"]}, bson.M{"$set": bson.M{"used": true}})
		return nil, bad("The OTP expired. Request a new one.")
	}
	if subtle.ConstantTimeCompare([]byte(o["code_hash"].(string)), []byte(sha(id+":"+strings.TrimSpace(b.Code)))) != 1 {
		s.authGuard.failed(ip, id)
		tries := int(o["tries"].(int32)) + 1
		_, _ = otps.Update(ctx, bson.M{"_id": o["id"]}, bson.M{"$set": bson.M{"tries": tries, "used": tries >= otpMaxTries}})
		if tries >= otpMaxTries {
			return nil, bad("Too many wrong OTP entries. Request a new OTP.")
		}
		return nil, bad("That OTP is not correct. %d attempt%s left.", otpMaxTries-tries, plural(otpMaxTries-tries))
	}
	// A one-time token binds the password step to this verification: knowing the Employee ID is not enough.
	vt := randomHex(24)
	if _, err = otps.Update(ctx, bson.M{"_id": o["id"]}, bson.M{"$set": bson.M{"verified_at": time.Now(), "verify_hash": sha(vt)}}); err != nil {
		return nil, err
	}
	return map[string]string{"verifyToken": vt}, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (s *Server) setPassword(r *http.Request) (any, error) {
	var b idBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), s.authID(r.Context(), b.EmployeeID)
	o, err := s.st.DB.C("auth_otp").FindOne(ctx, bson.M{"employee_id": id, "used": false, "verified_at": bson.M{"$ne": nil}}, authNewest())
	if err != nil {
		return nil, err
	}
	if o == nil || o["expires_at"].(time.Time).Add(passwordGrace).Before(time.Now()) {
		return nil, bad("Verify your mobile number again.")
	}
	if vh, _ := o["verify_hash"].(string); vh == "" || subtle.ConstantTimeCompare([]byte(vh), []byte(sha(strings.TrimSpace(b.VerifyToken)))) != 1 {
		return nil, bad("Verify your mobile number again.")
	}
	mobile, _ := authMobile(ctx, s.st.DB, id)
	if p := access.PasswordProblems(b.Password, id, mobile); len(p) > 0 {
		return nil, badD("Your password needs: "+strings.ToLower(strings.Join(p, ", "))+".", p)
	}
	salt := randomHex(16)
	hash := hashPassword(b.Password, salt)
	var tok string
	err = s.st.Tx(ctx, func(tx store.Q) error {
		// The new password replaces any earlier one, and clears the wrong tries and any lock.
		if _, err := tx.C("auth_credential").Upsert(ctx, bson.M{"employee_id": id},
			bson.M{"salt": salt, "hash": hash, "fails": 0, "locked_until": nil, "updated_at": tx.Now()}, nil); err != nil {
			return err
		}
		if _, err := tx.C("auth_otp").Update(ctx, bson.M{"_id": o["id"]}, bson.M{"$set": bson.M{"used": true}}); err != nil {
			return err
		}
		if _, err := tx.C("auth_session").DeleteMany(ctx, bson.M{"employee_id": id}); err != nil {
			return err
		}
		action := "Password reset"
		if o["purpose"] == "first" {
			action = "Password created"
		}
		if err := audit(ctx, tx, &Ctx{Name: id, EmployeeID: id}, action, "employee", id, nil, nil); err != nil {
			return err
		}
		var err error
		tok, err = s.newSession(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{"token": tok}, nil
}

func (s *Server) login(r *http.Request) (any, error) {
	var b idBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id, ip := r.Context(), s.authID(r.Context(), b.EmployeeID), s.clientIP(r)
	if err := s.authGuard.blocked(ip, id); err != nil {
		return nil, err
	}
	generic := "Employee ID or password is incorrect. First time here? Activate with OTP."
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil || e == nil {
		s.authGuard.failed(ip, id)
		return nil, unauthorized(generic)
	}
	if e["status"] == "Pre-boarding" {
		return nil, bad("Your account opens on your joining date.")
	}
	creds := s.st.DB.C("auth_credential")
	a, err := creds.FindOne(ctx, bson.M{"employee_id": id})
	if err != nil {
		return nil, err
	}
	if a == nil {
		s.authGuard.failed(ip, id)
		return nil, unauthorized(generic)
	}
	if lu, ok := a["locked_until"].(time.Time); ok && lu.After(time.Now()) {
		return nil, &apiError{Status: 423, Message: fmt.Sprintf("Too many wrong attempts. Try again in %d minutes, or reset your password with OTP.", int(time.Until(lu).Minutes())+1)}
	}
	if subtle.ConstantTimeCompare([]byte(hashPassword(b.Password, a["salt"].(string))), []byte(a["hash"].(string))) != 1 {
		s.authGuard.failed(ip, id)
		fails := int(a["fails"].(int32)) + 1
		who := &Ctx{Name: str(e["name"]), EmployeeID: id}
		if fails >= loginMaxFails {
			_, _ = creds.Update(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"fails": 0, "locked_until": time.Now().Add(loginLock)}})
			_ = audit(ctx, s.st.DB, who, "Account locked after wrong passwords", "employee", id, nil, map[string]any{"ip": ip, "minutes": int(loginLock.Minutes())})
			return nil, unauthorized("Too many wrong attempts. The account is locked for 5 minutes. You can reset your password with OTP.")
		}
		_, _ = creds.Update(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"fails": fails}})
		_ = audit(ctx, s.st.DB, who, "Wrong password", "employee", id, nil, map[string]any{"ip": ip, "attemptsLeft": loginMaxFails - fails})
		return nil, unauthorized(fmt.Sprintf("%s (%d attempt%s left)", generic, loginMaxFails-fails, plural(loginMaxFails-fails)))
	}
	_, _ = creds.Update(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"fails": 0, "locked_until": nil}})
	tok, err := s.newSession(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	// Sign-ins, wrong passwords, locks and sign-outs are in the audit log, so a suspicious sign-in can be traced
	// (AUTH-045). No secrets: who, when and from which address.
	_ = audit(ctx, s.st.DB, &Ctx{Name: str(e["name"]), EmployeeID: id}, "Signed in", "employee", id, nil, map[string]any{"ip": ip})
	return map[string]string{"token": tok}, nil
}

func (s *Server) logout(r *http.Request, c *Ctx) (any, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	_, err := s.st.DB.C("auth_session").DeleteMany(r.Context(), bson.M{"token_hash": sha(tok)})
	if err == nil {
		_ = audit(r.Context(), s.st.DB, &Ctx{Name: c.Name, EmployeeID: c.EmployeeID}, "Signed out", "employee", c.EmployeeID, nil, nil)
	}
	return nil, err
}

// me returns the caller, their available role views and what the current view allows.
func (s *Server) me(r *http.Request, c *Ctx) (any, error) {
	e, err := authJobLine(r.Context(), s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	// claimsOn: the work policy lets the person raise their own reimbursement claims (My pay › Reimbursements).
	return map[string]any{"ctx": s.meCtx(r.Context(), c), "employee": e, "claimsOn": claimsOn(r.Context(), s.st.DB, c.EmployeeID)}, nil // + mgrCases, scopeLocations, scopeDepartments
}

// authJobLine is the person's id, name, type, status and grade with the names of their department, designation and
// location — nil when a part is missing (the old inner joins found no row).
func authJobLine(ctx context.Context, q store.Q, id string) (store.Row, error) {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil || e == nil {
		return nil, err
	}
	out := store.Row{"id": e["id"], "name": e["name"], "type": e["type"], "status": e["status"], "grade": e["grade"]}
	for _, j := range []struct{ coll, ref, as string }{
		{"department", "department_id", "department"}, {"designation", "designation_id", "designation"}, {"location", "location_id", "location"}} {
		x, err := q.C(j.coll).FindOne(ctx, bson.M{"_id": e[j.ref]})
		if err != nil || x == nil {
			return nil, err
		}
		out[j.as] = x["name"]
	}
	return out, nil
}
