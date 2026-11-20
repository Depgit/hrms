package api_test

import (
	"strings"
	"testing"
)

// Which report each role sees (#21) and the joiners export's masking (#14).
func TestReportAccessAndJoinerMask(t *testing.T) {
	c := setup(t)
	const hr, hrRole, bp, bpRole = "EMP-1002", "HR Admin", "EMP-1004", "HRBP"
	c.signIn(hr)
	c.signIn(bp)

	// Untick Employee master list for HRBP: gone from the list; running and downloading it are refused.
	if s, r := c.do("PUT", "/api/roles/HRBP/reports", hr, hrRole, map[string]any{"reports": map[string]bool{"emp_master": false}}); s != 200 {
		t.Fatalf("untick: %d %v", s, r)
	}
	_, list := c.do("GET", "/api/reports", bp, bpRole, nil)
	for _, x := range list["reports"].([]any) {
		if x.(map[string]any)["id"] == "emp_master" {
			t.Fatal("an unticked report is still listed")
		}
	}
	if s, _ := c.do("GET", "/api/reports/emp_master", bp, bpRole, nil); s != 403 {
		t.Fatalf("running an unticked report: %d", s)
	}
	if s, _ := c.getRaw("/api/reports/emp_master/sheet", bp, bpRole); s != 403 {
		t.Fatalf("downloading an unticked report: %d", s)
	}
	if s, _ := c.do("GET", "/api/reports/joiners", bp, bpRole, nil); s != 200 {
		t.Fatalf("a ticked report still runs: %d", s)
	}
	_, ra := c.do("GET", "/api/access/reports?role=HRBP", hr, hrRole, nil)
	found := false
	for _, g := range ra["groups"].([]any) {
		for _, x := range g.(map[string]any)["reports"].([]any) {
			if m := x.(map[string]any); m["id"] == "emp_master" {
				found = m["on"] == false
			}
		}
	}
	if !found {
		t.Fatalf("the role's report list shows the untick: %v", ra)
	}
	// HR Admin always sees every report and cannot be changed.
	if s, _ := c.do("GET", "/api/reports/emp_master", hr, hrRole, nil); s != 200 {
		t.Fatalf("HR Admin runs every report: %d", s)
	}
	if s, _ := c.getRaw("/api/reports/emp_master/sheet", hr, hrRole); s != 200 {
		t.Fatalf("HR Admin downloads every report: %d", s)
	}
	if s, _ := c.do("PUT", "/api/roles/HR%20Admin/reports", hr, hrRole, map[string]any{"reports": map[string]bool{"emp_master": false}}); s != 400 {
		t.Fatalf("HR Admin's reports are fixed: %d", s)
	}
	// Ticking it again gives it back.
	c.do("PUT", "/api/roles/HRBP/reports", hr, hrRole, map[string]any{"reports": map[string]bool{"emp_master": true}})
	if s, _ := c.do("GET", "/api/reports/emp_master", bp, bpRole, nil); s != 200 {
		t.Fatalf("ticked again: %d", s)
	}

	// Joiners export: Aadhaar masked for a role whose ID mode is Masked, in full for HR Admin.
	if s, r := c.do("PUT", "/api/roles/HRBP/rights", hr, hrRole, map[string]any{"sensitive": map[string]string{"govId": "Masked"}}); s != 200 {
		t.Fatalf("mask IDs: %d %v", s, r)
	}
	aadhaar := func(emp, role string) map[string]string {
		_, rep := c.do("GET", "/api/reports/joiners", emp, role, nil)
		cols, _ := rep["cols"].([]any)
		col := -1
		for i, h := range cols {
			if h == "Aadhaar" {
				col = i
			}
		}
		if col < 0 {
			t.Fatalf("no Aadhaar column: %v", cols)
		}
		out := map[string]string{}
		for _, r := range rep["rows"].([]any) {
			row := r.([]any)
			if v, ok := row[col].(string); ok {
				out[row[0].(string)] = v
			}
		}
		return out
	}
	full, masked := aadhaar(hr, hrRole), aadhaar(bp, bpRole)
	if len(masked) == 0 {
		t.Fatal("the HRBP sees no joiner with an Aadhaar")
	}
	_, csv := c.getRaw("/api/reports/joiners/sheet?format=csv", bp, bpRole)
	for id, v := range masked {
		if id == bp {
			continue
		}
		if !strings.HasPrefix(v, "•") || v == full[id] || !strings.HasSuffix(full[id], v[len(v)-4:]) {
			t.Fatalf("%s: Aadhaar %q not masked (full %q)", id, v, full[id])
		}
		if strings.Contains(string(csv), full[id]) {
			t.Fatalf("%s: the download shows the full Aadhaar", id)
		}
	}
}
