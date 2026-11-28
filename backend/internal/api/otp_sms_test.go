package api_test

// The one-time code must really leave through the SMS sender, never be shown unless the dev echo is on, and never be
// reported as sent when nothing can send it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/storage"
)

func TestOTPDelivery(t *testing.T) {
	_, _, st := setupFull(t) // an empty database with the sample organisation; EMP-1002 has a mobile number
	send := func(opts api.Options) (int, map[string]any) {
		opts.Files = &storage.Memory{}
		srv := httptest.NewServer(api.New(st, opts).Routes())
		defer srv.Close()
		b, _ := json.Marshal(map[string]string{"employeeId": "EMP-1002", "purpose": "forgot"})
		res, err := http.Post(srv.URL+"/api/auth/otp", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	msg := func(out map[string]any) string { b, _ := json.Marshal(out); return strings.ToLower(string(b)) }

	// Nothing can send it and the dev echo is off: refuse, do not pretend.
	if code, out := send(api.Options{}); code == 200 || !strings.Contains(msg(out), "not set up") {
		t.Fatalf("no sender, no echo: %d %v", code, out)
	}
	// A sender: it gets the person's mobile and a 6-digit code, and the reply carries no code.
	var gotMobile, gotCode string
	code, out := send(api.Options{OTP: func(_ context.Context, mobile, c string) error { gotMobile, gotCode = mobile, c; return nil }})
	if code != 200 || out["devOtp"] != nil {
		t.Fatalf("with a sender: %d %v", code, out)
	}
	if !regexp.MustCompile(`^\d{10}$`).MatchString(gotMobile) || !regexp.MustCompile(`^\d{6}$`).MatchString(gotCode) {
		t.Fatalf("sender got mobile %q code %q", gotMobile, gotCode)
	}
	// A failing sender is an error the person can read, and the code is not in it.
	st.DB.C("auth_otp").DeleteMany(context.Background(), nil) // lift the 30-second pause between codes
	code, out = send(api.Options{OTP: func(context.Context, string, string) error { return errors.New("MSG91 down") }})
	if code == 200 || strings.Contains(msg(out), "msg91") || !strings.Contains(msg(out), "could not send") {
		t.Fatalf("failing sender: %d %v", code, out)
	}

	// Email too (owner, 27 Nov 2026): the same code goes to the personal email on record; the reply masks both.
	ctx := context.Background()
	st.DB.C("employee_personal").UpdateMany(ctx, map[string]any{"employee_id": "EMP-1002"}, map[string]any{"$set": map[string]any{"email": "otp.check@example.com"}})
	st.DB.C("auth_otp").DeleteMany(ctx, nil)
	fake := &mail.Fake{}
	code, out = send(api.Options{Mail: fake, OTP: func(_ context.Context, _, c string) error { gotCode = c; return nil }})
	m := fake.Last("otp.check@example.com")
	if code != 200 || out["mask"] == nil || out["emailMask"] != "ot•••@example.com" || !strings.Contains(m.Text, gotCode) || strings.Contains(m.Subject, gotCode) {
		t.Fatalf("sms + email: %d %v / %q %q", code, out, m.Subject, gotCode)
	}
	// SMS down, email works: still sent, the reply names only the email.
	st.DB.C("auth_otp").DeleteMany(ctx, nil)
	code, out = send(api.Options{Mail: fake, OTP: func(context.Context, string, string) error { return errors.New("MSG91 down") }})
	if code != 200 || out["mask"] != nil || out["emailMask"] == nil {
		t.Fatalf("sms down, email up: %d %v", code, out)
	}
	// Email only (no SMS sender): sent by email.
	st.DB.C("auth_otp").DeleteMany(ctx, nil)
	n := fake.Count()
	if code, out = send(api.Options{Mail: fake}); code != 200 || fake.Count() != n+1 || out["mask"] != nil {
		t.Fatalf("email only: %d %v", code, out)
	}
}
