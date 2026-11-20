package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// A corrected sheet that changes every row of the run is applied in one transaction that computes the rows and
// the totals once (MongoDB aborts a transaction after 60s; doing the whole month's work per row failed past about
// 1,700 rows). The result must be what applying the edits one by one gave: each stored row as a fresh dry run
// computes it, the month's totals, and one audit entry per sheet row whose "before" is the record as it stood,
// including a person listed twice.
func TestPayrollSheetBulkApply(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1011")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.pcCloseAll(t, open)
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	_, rows := c.do("GET", "/api/payroll/"+open+"/rows?size=500", "EMP-1011", "Payroll", nil)
	list := []any{}
	for _, x := range rows["rows"].([]any) { // nobody edits their own row (ACC-05): the uploader's row stays out of the sheet
		if x.(map[string]any)["employeeId"] != "EMP-1011" {
			list = append(list, x)
		}
	}
	if len(list) < 20 {
		t.Fatalf("run rows %d", len(list))
	}
	// One person already has an edit before the sheet comes in.
	first := list[0].(map[string]any)
	firstID := first["employeeId"].(string)
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/"+firstID, "EMP-1011", "Payroll", map[string]any{"otherEarn": 50, "reason": "Earlier edit"}); s != 200 {
		t.Fatal(r)
	}
	var sheet strings.Builder
	sheet.WriteString("Emp ID,Paid days,Other earnings,Other deductions,Adjustment reason\n")
	for i, x := range list {
		o := x.(map[string]any)
		fmt.Fprintf(&sheet, "%s,%v,%d,%d,  Sheet fix %d  \n", o["employeeId"], o["paid"], 100+i, i%3, i)
	}
	// The second person again at the end: the later line wins, and its audit "before" is the earlier line's record.
	second := list[1].(map[string]any)
	secondID := second["employeeId"].(string)
	fmt.Fprintf(&sheet, "%s,%v,777,0,Second line\n", secondID, second["paid"])
	auditsBefore, _ := st.DB.C("audit_log").Count(ctx, bson.M{"action": "Edited payroll row"})

	s, r := c.sendFile("/api/payroll/"+open+"/sheet/apply", "EMP-1011", "Payroll", "sheet.csv", []byte(sheet.String()))
	if s != 200 || r["applied"] != true || num(r["problems"]) != 0 || int(num(r["ready"])) != len(list)+1 {
		t.Fatalf("apply %d %v", s, r)
	}

	// The overrides: one per person, the last line for the person listed twice, the reason trimmed.
	ovs, _ := st.DB.C("payroll_override").Find(ctx, bson.M{"ym": open})
	if len(ovs) != len(list) {
		t.Fatalf("overrides %d, want %d", len(ovs), len(list))
	}
	for _, ov := range ovs {
		switch id := fmt.Sprint(ov["employee_id"]); id {
		case secondID:
			if ov["other_earn"] != 777.0 || ov["reason"] != "Second line" {
				t.Fatalf("person listed twice: %v", ov)
			}
		case firstID:
			if ov["other_earn"] != 100.0 || ov["reason"] != "Sheet fix 0" {
				t.Fatalf("first person: %v", ov)
			}
		}
	}

	// One audit entry per sheet line, in sheet order, each with the record as it stood before that line.
	audits, _ := st.DB.C("audit_log").Find(ctx, bson.M{"action": "Edited payroll row"}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	audits = audits[auditsBefore:]
	if len(audits) != len(list)+1 {
		t.Fatalf("audit entries %d, want %d", len(audits), len(list)+1)
	}
	if audits[0]["entity_id"] != open+"/"+firstID || fmt.Sprint(audits[0]["before"].(map[string]any)["other_earn"]) != "50" ||
		audits[0]["after"].(map[string]any)["reason"] != "Sheet fix 0" {
		t.Fatalf("first audit: %v", audits[0])
	}
	if audits[1]["before"] != nil {
		t.Fatalf("no record before the second person's first line: %v", audits[1]["before"])
	}
	last := audits[len(audits)-1]
	if last["entity_id"] != open+"/"+secondID || fmt.Sprint(last["before"].(map[string]any)["other_earn"]) != "101" ||
		fmt.Sprint(last["after"].(map[string]any)["otherEarn"]) != "777" {
		t.Fatalf("the repeated line's audit: %v", last)
	}

	// The stored rows and totals are what a fresh dry run computes from the same edits.
	stored := payrollSheetRows(t, st, open)
	_, cl := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil)
	totals := cl["month"].(map[string]any)["totals"]
	if row := c.row(t, open, secondID); num(row["oe"]) != 777 {
		t.Fatalf("stored row of the person listed twice: oe %v", row["oe"])
	}
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	fresh := payrollSheetRows(t, st, open)
	if len(stored) != len(fresh) {
		t.Fatalf("rows %d after the sheet, %d after a fresh run", len(stored), len(fresh))
	}
	for id, want := range fresh {
		if stored[id] != want {
			t.Fatalf("row %s after the sheet:\n%s\nfresh run:\n%s", id, stored[id], want)
		}
	}
	_, cl = c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil)
	if a, b := fmt.Sprint(totals), fmt.Sprint(cl["month"].(map[string]any)["totals"]); a != b {
		t.Fatalf("totals after the sheet %s, after a fresh run %s", a, b)
	}

	// Once verified, a sheet saves nothing and says why, as before.
	if s, r := c.do("POST", "/api/payroll/"+open+"/verify", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	_, rows = c.do("GET", "/api/payroll/"+open+"/rows?size=500", "EMP-1011", "Payroll", nil)
	o := rows["rows"].([]any)[2].(map[string]any)
	again := fmt.Sprintf("Emp ID,Paid days,Other earnings,Other deductions,Adjustment reason\n%s,%v,999,0,Too late\n", o["employeeId"], o["paid"])
	if s, r := c.sendFile("/api/payroll/"+open+"/sheet/apply", "EMP-1011", "Payroll", "sheet.csv", []byte(again)); s != 400 || !strings.Contains(fmt.Sprint(r), "Send it back to the dry run") {
		t.Fatalf("apply once verified: %d %v", s, r)
	}
	if ov, _ := st.DB.C("payroll_override").FindOne(ctx, bson.M{"ym": open, "employee_id": o["employeeId"]}); ov["other_earn"] == 999.0 {
		t.Fatal("saved after the month was verified")
	}
}

// payrollSheetRows is the month's stored rows by person, as JSON without the time they were computed.
func payrollSheetRows(t *testing.T, st *store.Store, ym string) map[string]string {
	t.Helper()
	rows, err := st.DB.C("payroll_row").Find(context.Background(), bson.M{"ym": ym})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		delete(r, "computed_at")
		b, _ := json.Marshal(r)
		out[fmt.Sprint(r["employee_id"])] = string(b)
	}
	return out
}
