package api_test

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
)

// "Download all details" offers the prototype's groups, including work policy, approvers and service history, and
// the uniform sizes (13 Nov 2026, uniform.go).
func TestExportGroups(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004")
	_, o := c.do("GET", "/api/employees/export/options", "EMP-1002", "HR Admin", nil)
	if secs := o["sections"].([]any); len(secs) != 9 {
		t.Fatalf("HR Admin gets the 9 groups, got %v", secs)
	}
	_, h := c.do("GET", "/api/employees/export/options", "EMP-1004", "HRBP", nil)
	if secs := h["sections"].([]any); len(secs) != 1 {
		t.Fatalf("HRBP still downloads job and location only, got %v", secs)
	}
	req, _ := http.NewRequest("GET", c.base+"/api/employees/export?sections=salary,policy,appr,hist", nil)
	req.Header.Set("Authorization", "Bearer "+c.tokens["EMP-1002"])
	req.Header.Set("X-Role", "HR Admin")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res.StatusCode)
	}
	defer res.Body.Close()
	rows, err := csv.NewReader(res.Body).ReadAll()
	if err != nil || len(rows) < 10 {
		t.Fatal(err, len(rows))
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	for _, h := range []string{"Structure effective from", "Tax regime", "Work policy", "Mapped through", "Holiday calendar", "Leave approvers", "Claim approvers (up to ₹5,000)", "Times rejoined", "Earlier service"} {
		if _, ok := col[h]; !ok {
			t.Fatalf("column %q missing from %v", h, rows[0])
		}
	}
	var r []string
	for _, x := range rows[1:] {
		if x[0] == "EMP-1020" {
			r = x
		}
	}
	if r == nil || r[col["Work policy"]] == "" || r[col["Tax regime"]] == "" || r[col["Times rejoined"]] != "0" {
		t.Fatalf("EMP-1020 row: %v", r)
	}
	if v := r[col["Leave approvers"]]; v == "" || !strings.Contains(v, "Line Manager: ") {
		t.Fatalf("leave route is named stage by stage, got %q", v)
	}
}
