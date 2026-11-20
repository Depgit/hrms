package api_test

import (
	"fmt"
	"strings"
	"testing"
)

// An exit HR starts about someone is not shown to that person until it is approved (owner's decision, 11 Nov 2026).
func TestHRStartedExitHiddenUntilApproved(t *testing.T) {
	c, _, _ := setupFull(t)
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1016", "EMP-1022"} {
		c.signIn(e)
	}
	listed := func(emp, reqID string) bool {
		t.Helper()
		s, b := c.getRaw("/api/me/requests?wf=all", emp, "Employee")
		if s != 200 {
			t.Fatalf("my requests %d %s", s, b)
		}
		return strings.Contains(string(b), reqID)
	}
	pending := func(emp string) float64 {
		t.Helper()
		_, d := c.do("GET", "/api/me/dashboard", emp, "Employee", nil)
		return num(d["pending"])
	}

	before := pending("EMP-1016")
	s, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "type": "Termination", "reason": "Misconduct",
		"noticeOption": "pay", "note": "Repeated cash shortages after two warnings"})
	if s != 200 || r["requestId"] == nil {
		t.Fatalf("termination: %d %v", s, r)
	}
	req, sep := r["requestId"].(string), fmt.Sprint(r["id"])

	t.Run("while pending the employee sees nothing of it", func(t *testing.T) {
		if listed("EMP-1016", req) {
			t.Fatal("pending termination in My requests")
		}
		if s, b := c.getRaw("/api/me/requests?wf=resign", "EMP-1016", "Employee"); s != 200 || strings.Contains(string(b), req) {
			t.Fatalf("pending termination in the resign list: %d %s", s, b)
		}
		if n := pending("EMP-1016"); n != before {
			t.Fatalf("waiting count %v, was %v", n, before)
		}
		if s, _ := c.do("GET", "/api/approvals/"+req, "EMP-1016", "Employee", nil); s != 404 {
			t.Fatalf("request detail %d, want 404", s)
		}
		if s, _ := c.do("GET", "/api/separations/"+sep, "EMP-1016", "Employee", nil); s != 404 {
			t.Fatalf("case detail %d, want 404", s)
		}
		if _, m := c.do("GET", "/api/me/separation", "EMP-1016", "Employee", nil); m["case"] != nil {
			t.Fatalf("Separation page shows the case: %v", m["case"])
		}
		if s, b := c.getRaw("/api/me/dashboard", "EMP-1016", "Employee"); s != 200 || strings.Contains(string(b), req) {
			t.Fatalf("dashboard activity mentions it: %s", b)
		}
		_, m := c.do("GET", "/api/me/separation", "EMP-1016", "Employee", nil)
		if why := fmt.Sprint(m["notice"]); !strings.Contains(why, "speak to HR") {
			t.Fatalf("Separation page reason: %v", m["notice"])
		}
		s, e := c.do("POST", "/api/approvals", "EMP-1016", "Employee", map[string]any{"wf": "resign", "payload": map[string]any{"reason": "Relocation"}})
		if s != 400 || !strings.Contains(fmt.Sprint(e["error"]), "speak to HR") {
			t.Fatalf("own resignation while a hidden case is open: %d %v", s, e)
		}
	})

	t.Run("HR still sees and decides it", func(t *testing.T) {
		if s, _ := c.do("GET", "/api/approvals/"+req, "EMP-1002", "HR Admin", nil); s != 200 {
			t.Fatalf("HR Admin detail %d", s)
		}
		if s, _ := c.do("GET", "/api/separations/"+sep, "EMP-1004", "HRBP", nil); s != 200 {
			t.Fatalf("HRBP case %d", s)
		}
	})

	t.Run("an absconding case at show-cause is hidden too", func(t *testing.T) {
		_, a := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1022", "type": "Absconding", "note": "Not reachable on phone since Monday"})
		if a["id"] == nil {
			t.Fatal(a)
		}
		if s, _ := c.do("GET", "/api/separations/"+fmt.Sprint(a["id"]), "EMP-1022", "Employee", nil); s != 404 {
			t.Fatalf("absconding case detail %d, want 404", s)
		}
	})

	t.Run("once approved the employee sees it", func(t *testing.T) {
		c.approveAll(t, req)
		if !listed("EMP-1016", req) {
			t.Fatal("approved termination not in My requests")
		}
		if s, _ := c.do("GET", "/api/approvals/"+req, "EMP-1016", "Employee", nil); s != 200 {
			t.Fatalf("request detail after approval %d", s)
		}
		if _, m := c.do("GET", "/api/me/separation", "EMP-1016", "Employee", nil); m["case"] == nil {
			t.Fatal("Separation page does not show the approved case")
		}
	})
}
