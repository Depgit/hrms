// Package sms sends the one-time code as a text message through MSG91's Flow API: the message wording lives in the
// DLT-approved template behind the flow, and the code goes in as its OTP variable.
package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MSG91 sends through one flow (MSG91_SMS_FLOW_ID) with the account's auth key (MSG91_AUTH_KEY).
type MSG91 struct {
	AuthKey string
	FlowID  string
	BaseURL string // https://api.msg91.com unless a test points it elsewhere
	HTTP    *http.Client
}

// FromEnv is nil when either setting is empty, which means SMS is not set up.
func FromEnv(get func(string) string) *MSG91 {
	key, flow := strings.TrimSpace(get("MSG91_AUTH_KEY")), strings.TrimSpace(get("MSG91_SMS_FLOW_ID"))
	if key == "" || flow == "" {
		return nil
	}
	return &MSG91{AuthKey: key, FlowID: flow, BaseURL: "https://api.msg91.com", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// Mobile gives the number MSG91 expects: digits only, with 91 in front of a 10-digit Indian number.
func Mobile(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	m := strings.TrimPrefix(b.String(), "00")
	if len(m) == 10 {
		return "91" + m
	}
	return m
}

// SendOTP texts the code. MSG91 can answer HTTP 200 and still refuse the message, so the reply body is read too.
func (m *MSG91) SendOTP(ctx context.Context, mobile, code string) error {
	to := Mobile(mobile)
	if len(to) < 11 {
		return fmt.Errorf("the mobile number %q is not valid", mobile)
	}
	payload, _ := json.Marshal(map[string]any{
		"flow_id":    m.FlowID,
		"recipients": []map[string]string{{"mobiles": to, "OTP": code}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.BaseURL, "/")+"/api/v5/flow/", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authkey", m.AuthKey)
	res, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode >= 300 {
		return fmt.Errorf("MSG91 replied %s: %s", res.Status, body)
	}
	var reply struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Message any    `json:"message"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return nil // not JSON: trust the HTTP status
	}
	if strings.EqualFold(reply.Type, "error") || strings.EqualFold(reply.Status, "fail") || strings.EqualFold(reply.Status, "error") {
		return fmt.Errorf("MSG91 refused the message: %s", body)
	}
	return nil
}
