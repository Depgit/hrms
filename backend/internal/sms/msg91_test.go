package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMobile(t *testing.T) {
	for in, want := range map[string]string{"9876543210": "919876543210", "+91 98765-43210": "919876543210", "919876543210": "919876543210", "": ""} {
		if got := Mobile(in); got != want {
			t.Errorf("Mobile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromEnvNeedsBoth(t *testing.T) {
	env := map[string]string{"MSG91_AUTH_KEY": "k"}
	if FromEnv(func(k string) string { return env[k] }) != nil {
		t.Fatal("a key without a flow must mean SMS is not set up")
	}
	env["MSG91_SMS_FLOW_ID"] = "f"
	if FromEnv(func(k string) string { return env[k] }) == nil {
		t.Fatal("key and flow must set SMS up")
	}
}

// The request must carry the flow, the number and the code as the OTP variable, and a refusal inside an HTTP 200
// must be an error (the case that once looked like success).
func TestSendOTP(t *testing.T) {
	var got struct {
		FlowID     string              `json:"flow_id"`
		Recipients []map[string]string `json:"recipients"`
	}
	var key, path string
	reply := `{"type":"success","message":"abc"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, path = r.Header.Get("authkey"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	m := &MSG91{AuthKey: "secret", FlowID: "flow1", BaseURL: srv.URL, HTTP: srv.Client()}
	if err := m.SendOTP(context.Background(), "98765 43210", "123456"); err != nil {
		t.Fatal(err)
	}
	if key != "secret" || path != "/api/v5/flow/" || got.FlowID != "flow1" || len(got.Recipients) != 1 ||
		got.Recipients[0]["mobiles"] != "919876543210" || got.Recipients[0]["OTP"] != "123456" {
		t.Fatalf("bad request: key=%q path=%q body=%+v", key, path, got)
	}
	reply = `{"type":"error","message":"template not approved"}`
	if err := m.SendOTP(context.Background(), "9876543210", "123456"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refusal in a 200 reply must be an error, got %v", err)
	}
	if err := m.SendOTP(context.Background(), "12345", "123456"); err == nil {
		t.Fatal("a short number must be refused before calling MSG91")
	}
}
