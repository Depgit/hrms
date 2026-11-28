package api_test

import (
	"fmt"
	"testing"
)

// The SI/OI/JI series can be moved forward to start after the Realtime portal's codes (owner, 26 Nov 2026), never
// back, and only by someone with the hiring setup right.
func TestHireNextNumber(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	if s, _ := c.do("PUT", "/api/onboarding/settings", "EMP-1004", "HRBP", map[string]any{"nextNo": 3220}); s != 403 {
		t.Fatalf("HRBP set the hiring number: %d", s)
	}
	if s, r := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"nextNo": 3220}); s != 200 {
		t.Fatalf("set: %d %v", s, r)
	}
	if _, r := c.do("GET", "/api/onboarding/settings", "EMP-1002", "HR Admin", nil); fmt.Sprint(r["nextNo"]) != "3220" {
		t.Fatalf("next after set: %v", r["nextNo"])
	}
	if s, r := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"nextNo": 100}); s != 400 {
		t.Fatalf("moved back: %d %v", s, r)
	}
	if si := selectCandidate(t, c, shortlistBody(st, "Nina", "Rao", "9876503220")); si != "SI-3220" {
		t.Fatalf("first candidate after the move: %s", si)
	}
	if s, r := c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]any{"nextNo": 3220}); s != 400 {
		t.Fatalf("3220 is used now, yet accepted: %d %v", s, r)
	}
}
