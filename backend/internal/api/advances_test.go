package api_test

// Salary advances asked for by the employee, Payroll's change of the recovery, the company's limits, and opening
// advances loaded from Excel (advances.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// advLines are an advance's ledger lines (not void), in month order.
func advLines(st *store.Store, src string) []map[string]any {
	rows, _ := st.DB.C("ledger").Find(context.Background(), bson.M{"source_id": src, "void": false}, options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
	out := []map[string]any{}
	for _, x := range rows {
		out = append(out, map[string]any{"ym": x["ym"], "kind": x["kind"], "amount": apiFloat(x["amount"]), "label": x["label"]})
	}
	return out
}

// postXlsx sends a file as multipart "file".
func (c *client) postXlsx(path, emp, role, name string, data []byte) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	f, _ := w.CreateFormFile("file", name)
	_, _ = f.Write(data)
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestSelfAdvance(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1001", "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1014", "EMP-1016", "EMP-1020")
	ask := func(emp string, body map[string]any) (int, map[string]any) {
		return c.do("POST", "/api/approvals", emp, "Employee", map[string]any{"wf": "p_adv", "payload": body})
	}

	// The form: eligible, the months and the limit (1 × monthly gross by default).
	s, f := c.do("GET", "/api/me/advance", "EMP-1020", "Employee", nil)
	open, _ := f["openYm"].(string)
	gross := apiFloat(f["gross"])
	if s != 200 || f["eligible"] != true || gross <= 0 || apiFloat(f["maxAmount"]) != float64(int(gross)) || f["payMonth"] != open || f["startYm"] != ymAdd(open, 1) {
		t.Fatalf("form: %d %v", s, f)
	}
	rules, _ := f["rules"].(map[string]any)
	if apiFloat(rules["maxMultiple"]) != 1 || rules["oneOpen"] != true {
		t.Fatalf("default rules: %v", rules)
	}

	t.Run("eligibility", func(t *testing.T) {
		if _, f := c.do("GET", "/api/me/advance", "EMP-1014", "Employee", nil); f["eligible"] != false || !strings.Contains(fmt.Sprint(f["why"]), "permanent") {
			t.Fatalf("contractual form: %v", f)
		}
		if s, r := ask("EMP-1014", map[string]any{"amount": 2000, "instalments": 2, "note": "Family function"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "permanent") {
			t.Fatalf("contractual staff ask: %d %v", s, r)
		}
		// The employee no longer picks the instalments (owner, 14 Nov 2026): the default is the full amount in the first
		// month, so 13 is not refused but ignored (checked in TestAdvanceEligibilityAndInstalments).
		if s, _ := ask("EMP-1020", map[string]any{"amount": 2000, "instalments": 2, "note": ""}); s != 400 {
			t.Fatal("a reason is needed")
		}
	})

	t.Run("limit", func(t *testing.T) {
		s, r := ask("EMP-1020", map[string]any{"amount": gross + 1, "instalments": 6, "note": "House repair"})
		if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "up to") {
			t.Fatalf("above 1 × gross: %d %v", s, r)
		}
		// The simulator shows the same problem before sending.
		_, sim := c.do("POST", "/api/approvals/simulate", "EMP-1020", "Employee", map[string]any{"wf": "p_adv", "payload": map[string]any{"amount": gross + 1, "instalments": 6, "note": "House repair"}})
		if !strings.Contains(fmt.Sprint(sim["problems"]), "up to") || len(sim["chain"].([]any)) == 0 {
			t.Fatalf("simulate: %v", sim)
		}
		// Payroll settings: only pay.setup at Edit changes them; 0 means no limit.
		if s, _ := c.do("PUT", "/api/advances/settings", "EMP-1020", "Employee", map[string]any{"maxMultiple": 0}); s != 403 {
			t.Fatal("an employee changed the limits")
		}
		if s, _ := c.do("PUT", "/api/advances/settings", "EMP-1012", "Payroll Manager", map[string]any{"maxMultiple": 13}); s != 400 {
			t.Fatal("13 × gross accepted")
		}
		if s, r := c.do("PUT", "/api/advances/settings", "EMP-1012", "Payroll Manager", map[string]any{"maxMultiple": 0}); s != 200 {
			t.Fatal(r)
		}
		if _, f := c.do("GET", "/api/me/advance", "EMP-1020", "Employee", nil); apiFloat(f["maxAmount"]) != 0 || f["eligible"] != true {
			t.Fatalf("no limit: %v", f)
		}
		if apiCount(st, "audit_log", bson.M{"action": "Salary advance limits updated"}) != 1 {
			t.Fatal("the change is audited")
		}
		if s, r := c.do("PUT", "/api/advances/settings", "EMP-1012", "Payroll Manager", map[string]any{"maxMultiple": 1.5}); s != 200 {
			t.Fatal(r)
		}
		_, g := c.do("GET", "/api/advances/settings", "EMP-1011", "Payroll", nil)
		if r := g["rules"].(map[string]any); apiFloat(r["maxMultiple"]) != 1.5 || r["oneOpen"] != true || g["canEdit"] != true {
			t.Fatalf("settings: %v", g)
		}
	})

	var adv string
	t.Run("one at a time, cancel", func(t *testing.T) {
		s, r := ask("EMP-1020", map[string]any{"amount": gross + 1, "instalments": 4, "note": "House repair", "payMonth": "2030-01", "startYm": "2031-01"})
		if s != 200 {
			t.Fatal(r)
		}
		first := r["id"].(string)
		doc := apiDoc(st, "approval_request", bson.M{"_id": first})
		p := doc["payload"].(map[string]any)
		if p["payMonth"] != open || p["startYm"] != ymAdd(open, 1) || doc["raised_by"] != "EMP-1020" {
			t.Fatalf("the server sets the months: %v", doc)
		}
		if s, r := ask("EMP-1020", map[string]any{"amount": 1000, "instalments": 1, "note": "Second one"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "waiting for approval") {
			t.Fatalf("a second open advance: %d %v", s, r)
		}
		if _, f := c.do("GET", "/api/me/advance", "EMP-1020", "Employee", nil); f["eligible"] != false || f["open"] == nil {
			t.Fatalf("form with one open: %v", f)
		}
		// In My requests with Cancel; the employee cancels it.
		_, mine := c.do("GET", "/api/me/requests?wf=all", "EMP-1020", "Employee", nil)
		found := false
		for _, x := range mine["list"].([]any) {
			if m := x.(map[string]any); m["id"] == first {
				found = m["canCancel"] == true
			}
		}
		if !found {
			t.Fatal("My requests lists the advance with Cancel")
		}
		if s, r := c.do("POST", "/api/approvals/"+first+"/cancel", "EMP-1020", "Employee", nil); s != 200 {
			t.Fatal(r)
		}
		s, r = ask("EMP-1020", map[string]any{"amount": 9000, "instalments": 3, "note": "School fees"})
		if s != 200 {
			t.Fatal(r)
		}
		adv = r["id"].(string)
	})

	t.Run("an advance Payroll raised is not the employee's to cancel", func(t *testing.T) {
		_, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_adv", "employeeId": "EMP-1016",
			"payload": map[string]any{"amount": 3000, "instalments": 1, "payMonth": open, "note": "Raised by Payroll"}})
		id, _ := r["id"].(string)
		if id == "" {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/approvals/"+id+"/cancel", "EMP-1016", "Employee", nil); s != 403 {
			t.Fatal("the employee cancelled Payroll's advance")
		}
	})

	t.Run("Payroll changes the recovery at its step", func(t *testing.T) {
		terms := map[string]any{"instalments": 6, "payMonth": ymAdd(open, 1), "startYm": ymAdd(open, 3)}
		for i := 0; i < 8; i++ {
			d := c.request(t, adv)
			if waitingOn(d) == "Payroll" {
				break
			}
			if s, _ := c.do("PUT", "/api/approvals/"+adv+"/advance-terms", "EMP-1011", "Payroll", terms); s != 403 {
				t.Fatalf("changed before the Payroll step (%s)", waitingOn(d))
			}
			if s, r := c.do("POST", "/api/approvals/"+adv+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "test"}); s != 200 {
				t.Fatal(r)
			}
		}
		if s, _ := c.do("PUT", "/api/approvals/"+adv+"/advance-terms", "EMP-1011", "Payroll", map[string]any{"instalments": 6, "payMonth": ymAdd(open, 2), "startYm": ymAdd(open, 3)}); s != 400 {
			t.Fatal("pay month two months ahead")
		}
		if s, _ := c.do("PUT", "/api/approvals/"+adv+"/advance-terms", "EMP-1011", "Payroll", map[string]any{"instalments": 6, "payMonth": ymAdd(open, 1), "startYm": ymAdd(open, 1)}); s != 400 {
			t.Fatal("recovery in the pay month")
		}
		if s, _ := c.do("PUT", "/api/approvals/"+adv+"/advance-terms", "EMP-1020", "Employee", terms); s == 200 {
			t.Fatal("the employee changed the terms")
		}
		if s, r := c.do("PUT", "/api/approvals/"+adv+"/advance-terms", "EMP-1011", "Payroll", terms); s != 200 {
			t.Fatal(r)
		}
		if h := notes(c.request(t, adv)); !strings.Contains(h, "Recovery changed by Sunita Rao (Payroll): 1 instalment") || !strings.Contains(h, "→ 6 instalments") {
			t.Fatalf("history: %s", h)
		}
		if apiCount(st, "audit_log", bson.M{"action": "Advance recovery changed", "entity_id": adv}) != 1 {
			t.Fatal("the change is audited")
		}
		c.approveAll(t, adv)
		l := advLines(st, adv)
		if len(l) != 7 || l[0]["kind"] != "earn" || l[0]["ym"] != ymAdd(open, 1) || l[0]["amount"] != 9000.0 || l[1]["ym"] != ymAdd(open, 3) || l[1]["amount"] != 1500.0 || l[6]["ym"] != ymAdd(open, 8) {
			t.Fatalf("lines: %v", l)
		}
		if s, _ := c.do("PUT", "/api/approvals/"+adv+"/advance-terms", "EMP-1011", "Payroll", terms); s != 400 {
			t.Fatal("changed after approval")
		}
		// Approved with a balance: still one at a time.
		if s, r := ask("EMP-1020", map[string]any{"amount": 1000, "instalments": 1, "note": "Another"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "being recovered") {
			t.Fatalf("while recovering: %d %v", s, r)
		}
		// With one-at-a-time off it is allowed.
		if s, r := c.do("PUT", "/api/advances/settings", "EMP-1012", "Payroll Manager", map[string]any{"oneOpen": false}); s != 200 {
			t.Fatal(r)
		}
		if s, r := ask("EMP-1020", map[string]any{"amount": 1000, "instalments": 1, "note": "Another"}); s != 200 {
			t.Fatalf("one-at-a-time off: %d %v", s, r)
		}
		// My view lists the open advance with its balance.
		_, pay := c.do("GET", "/api/me/dashboard?tab=pay", "EMP-1020", "Employee", nil)
		if !strings.Contains(fmt.Sprint(pay["items"]), adv) {
			t.Fatalf("My view: %v", pay["items"])
		}
	})
}

// openingFile fills the downloaded template's first sheet with rows (the name column left as typed).
func openingFile(t *testing.T, tpl []byte, rows [][]string) []byte {
	t.Helper()
	x, err := excelize.OpenReader(bytes.NewReader(tpl))
	if err != nil {
		t.Fatal(err)
	}
	sh := x.GetSheetList()[0]
	for i, r := range rows {
		for j, v := range r {
			cn, _ := excelize.ColumnNumberToName(j + 1)
			_ = x.SetCellStr(sh, fmt.Sprintf("%s%d", cn, i+2), v)
		}
	}
	var buf bytes.Buffer
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOpeningAdvances(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)

	// The template: the columns and How to fill; Payroll and HR Admin only.
	if s, _ := c.raw("/api/advances/opening/template", "EMP-1004", "HRBP"); s != 403 {
		t.Fatal("HRBP downloads the template")
	}
	s, tpl := c.raw("/api/advances/opening/template", "EMP-1011", "Payroll")
	if s != 200 {
		t.Fatalf("template: %d %s", s, tpl)
	}
	x, err := excelize.OpenReader(bytes.NewReader(tpl))
	if err != nil {
		t.Fatal(err)
	}
	sheets := x.GetSheetList()
	head, _ := x.GetRows(sheets[0])
	if len(sheets) != 2 || sheets[1] != "How to fill" || strings.Join(head[0], "|") != "Employee ID|Employee name|Advance date|Original amount|Balance still to recover|Monthly recovery|Recovery starts|Reference / reason" {
		t.Fatalf("template sheets %v head %v", sheets, head)
	}

	past := time.Now().AddDate(0, -3, 0).Format("2006-01-02")
	rows := [][]string{
		{"EMP-1020", "Karan", past, "30000", "9000", "4000", open, "Voucher 2231"},          // add: 4000, 4000, 1000
		{"EMP-1016", "x", past, "₹12,000", "12,000", "3000", ymAdd(open, 1), "Register 14"}, // add: 4 × 3000
		{"EMP-9999", "", past, "1000", "500", "100", open, "Unknown"},                       // unknown
		{"EMP-1014", "", past, "1000", "500", "100", open, "Contract staff"},                // not permanent
		{"EMP-1016", "", past, "1000", "1500", "100", open, "Too much"},                     // balance > original
		{"EMP-1016", "", past, "1000", "0", "100", open, "Nothing left"},                    // balance 0
		{"EMP-1016", "", past, "1000", "500", "0", open, "No monthly"},                      // monthly 0
		{"EMP-1016", "", past, "1000", "500", "100", ymAdd(open, -1), "Too early"},          // before the open month
		{"emp-1020", "", past, "5000", "1000", "500", open, " voucher   2231 "},             // same as row 2
		{"EMP-1016", "", "2099-01-01", "1000", "500", "100", open, "Future date"},           // advance date ahead
		{"EMP-1016", "", past, "100000", "100000", "100", open, "Far too long"},             // 1000 months
	}
	file := openingFile(t, tpl, rows)
	if s, _ := c.postXlsx("/api/advances/opening/preview", "EMP-1004", "HRBP", "o.xlsx", file); s != 403 {
		t.Fatal("HRBP previews")
	}
	if s, r := c.postXlsx("/api/advances/opening/preview", "EMP-1011", "Payroll", "o.csv", []byte("a,b")); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), ".xlsx") {
		t.Fatalf("csv: %d %v", s, r)
	}
	s, pv := c.postXlsx("/api/advances/opening/preview", "EMP-1011", "Payroll", "opening.xlsx", file)
	if s != 200 || apiFloat(pv["add"]) != 2 || apiFloat(pv["skip"]) != 9 || apiFloat(pv["balance"]) != 21000 {
		t.Fatalf("preview: %d %v", s, pv)
	}
	want := map[int]string{4: "Unknown Employee ID", 5: "permanent", 6: "more than the original", 7: "nothing is left", 8: "Monthly recovery", 9: "before the open payroll month",
		10: "Same employee and reference as row 2", 11: "future", 12: "60"}
	for _, r := range pv["rows"].([]any) {
		m := r.(map[string]any)
		line := int(apiFloat(m["line"]))
		if w, ok := want[line]; ok && (m["outcome"] != "skip" || !strings.Contains(fmt.Sprint(m["reason"]), w)) {
			t.Fatalf("row %d: %v", line, m)
		}
		if line == 2 && (m["outcome"] != "add" || apiFloat(m["months"]) != 3 || apiFloat(m["last"]) != 1000 || m["name"] == "Karan") {
			t.Fatalf("row 2: %v", m)
		}
	}
	if apiCount(st, "approval_request", bson.M{"payload.opening": true}) != 0 {
		t.Fatal("the preview changed something")
	}

	// Confirm: two added, no earning line, the recovery lines cover the balance.
	s, res := c.postXlsx("/api/advances/opening/apply", "EMP-1011", "Payroll", "opening.xlsx", file)
	if s != 200 || apiFloat(res["added"]) != 2 || apiFloat(res["skipped"]) != 9 || apiFloat(res["failed"]) != 0 {
		t.Fatalf("apply: %d %v", s, res)
	}
	var a1, a2 string
	for _, r := range res["rows"].([]any) {
		m := r.(map[string]any)
		switch apiFloat(m["line"]) {
		case 2:
			a1, _ = m["id"].(string)
		case 3:
			a2, _ = m["id"].(string)
		}
	}
	if !strings.HasPrefix(a1, "ADV-") || !strings.HasPrefix(a2, "ADV-") {
		t.Fatalf("ids %q %q", a1, a2)
	}
	d := apiDoc(st, "approval_request", bson.M{"_id": a1})
	p := d["payload"].(map[string]any)
	if d["status"] != "Approved" || p["opening"] != true || apiFloat(p["amount"]) != 30000 || p["reference"] != "Voucher 2231" || p["advanceDate"] != past || d["raised_by"] != "EMP-1011" {
		t.Fatalf("stored: %v", d)
	}
	l := advLines(st, a1)
	if len(l) != 3 || l[0]["kind"] != "ded" || l[0]["ym"] != open || l[0]["amount"] != 4000.0 || l[2]["amount"] != 1000.0 || l[2]["ym"] != ymAdd(open, 2) || l[0]["label"] != "Advance recovery (1 of 3)" {
		t.Fatalf("lines %v", l)
	}
	if l2 := advLines(st, a2); len(l2) != 4 || l2[0]["ym"] != ymAdd(open, 1) || l2[3]["amount"] != 3000.0 {
		t.Fatalf("lines 2 %v", l2)
	}
	if apiCount(st, "ledger", bson.M{"source_id": bson.M{"$in": bson.A{a1, a2}}, "kind": "earn"}) != 0 {
		t.Fatal("an opening advance has no earning line")
	}
	au := apiDoc(st, "audit_log", bson.M{"action": "Opening advances uploaded: 2 added, 9 skipped, 0 failed"})
	if au == nil || !strings.Contains(fmt.Sprint(au["after"]), a1) {
		t.Fatalf("audit: %v", au)
	}

	// The same file again: both are duplicates now.
	_, again := c.postXlsx("/api/advances/opening/preview", "EMP-1011", "Payroll", "opening.xlsx", file)
	if apiFloat(again["add"]) != 0 || !strings.Contains(fmt.Sprint(again["rows"]), "Already loaded as "+a1) {
		t.Fatalf("duplicate guard: %v", again)
	}
	if _, r := c.postXlsx("/api/advances/opening/apply", "EMP-1011", "Payroll", "opening.xlsx", file); apiFloat(r["added"]) != 0 {
		t.Fatalf("loaded twice: %v", r)
	}

	// In the Advances list, tagged by its payload, with its balance; and in My view.
	_, list := c.do("GET", "/api/pay-items/advances", "EMP-1011", "Payroll", nil)
	found := false
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if m["id"] == a1 {
			found = m["payload"].(map[string]any)["opening"] == true && apiFloat(m["balance"]) == 9000
		}
	}
	if !found {
		t.Fatal("the Advances list shows the opening advance with its balance")
	}
	_, pay := c.do("GET", "/api/me/dashboard?tab=pay", "EMP-1020", "Employee", nil)
	if !strings.Contains(fmt.Sprint(pay["items"]), "opening balance") {
		t.Fatalf("My view: %v", pay["items"])
	}
	// The employee's own request counts it as open.
	if _, f := c.do("GET", "/api/me/advance", "EMP-1020", "Employee", nil); f["eligible"] != false || !strings.Contains(fmt.Sprint(f["why"]), a1) {
		t.Fatalf("form: %v", f)
	}

	// Skip next and recover balance work on an opening advance.
	if s, r := c.do("POST", "/api/advances/"+a2+"/skip", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	if l := advLines(st, a2); len(l) != 4 || l[0]["ym"] != ymAdd(open, 2) || l[3]["ym"] != ymAdd(open, 5) {
		t.Fatalf("after skip %v", l)
	}
	if s, r := c.do("POST", "/api/advances/"+a2+"/recover", "EMP-1011", "Payroll", map[string]any{"ym": ymAdd(open, 1)}); s != 200 {
		t.Fatal(r)
	}
	if l := advLines(st, a2); len(l) != 1 || l[0]["amount"] != 12000.0 || l[0]["ym"] != ymAdd(open, 1) {
		t.Fatalf("after recover %v", l)
	}

	// Full and final brings forward what is scheduled after the last working day (the open month).
	_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"lwd": open + "-15"}})
	_, fnf := c.do("GET", "/api/fnf/EMP-1020", "EMP-1011", "Payroll", nil)
	rec := 0.0
	for _, x := range fnf["lines"].([]any) {
		if m := x.(map[string]any); m["key"] == "rec" {
			rec = apiFloat(m["amount"])
			if !strings.Contains(fmt.Sprint(m["label"]), "Advance recovery") {
				t.Fatalf("rec line %v", m)
			}
		}
	}
	// Every recovery scheduled after the open month (the opening advance's 4,000 and 1,000, and any the sample data has).
	want2 := 0.0
	later, _ := st.DB.C("ledger").Find(ctx, bson.M{"employee_id": "EMP-1020", "void": false, "kind": "ded", "ym": bson.M{"$gt": open}, "source_id": bson.M{"$regex": "^(ADV|DED|UNI|AST)-"}})
	for _, x := range later {
		want2 += apiFloat(x["amount"])
	}
	if rec != want2 || rec < 5000 {
		t.Fatalf("F&F recovery %v (want %v, at least 5000): %v", rec, want2, fnf["lines"])
	}
}

// Owner's decision of 14 Nov 2026: only permanent employees with 3 months' service ask; the default recovery is the
// full amount in the first month after the payout; the manager sets the instalments when approving.
func TestAdvanceEligibilityAndInstalments(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1001", "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1014", "EMP-1016", "EMP-1020")
	ask := func(emp string, body map[string]any) (int, map[string]any) {
		return c.do("POST", "/api/approvals", emp, "Employee", map[string]any{"wf": "p_adv", "payload": body})
	}
	_, f := c.do("GET", "/api/me/advance", "EMP-1016", "Employee", nil)
	open := f["openYm"].(string)
	if rules := f["rules"].(map[string]any); apiFloat(rules["minMonths"]) != 3 || apiFloat(rules["maxInstalments"]) != 12 || apiFloat(f["instalments"]) != 1 {
		t.Fatalf("defaults: %v", f)
	}

	// A contractual employee: refused on the form and by the API, with the reason.
	if _, f := c.do("GET", "/api/me/advance", "EMP-1014", "Employee", nil); f["eligible"] != false || !strings.Contains(fmt.Sprint(f["why"]), "permanent employees only") {
		t.Fatalf("contractual form: %v", f)
	}
	if s, r := ask("EMP-1014", map[string]any{"amount": 2000, "note": "Family function"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "permanent employees only") {
		t.Fatalf("contractual ask: %d %v", s, r)
	}
	// Two months' service: refused until three months are done.
	doj := mustDate(today()).AddDate(0, -2, 0).Format("2006-01-02")
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"doj": doj}}); err != nil {
		t.Fatal(err)
	}
	if _, f := c.do("GET", "/api/me/advance", "EMP-1020", "Employee", nil); f["eligible"] != false || !strings.Contains(fmt.Sprint(f["why"]), "after 3 months' service") {
		t.Fatalf("2 months' service form: %v", f)
	}
	if s, r := ask("EMP-1020", map[string]any{"amount": 2000, "note": "Family function"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "after 3 months' service") {
		t.Fatalf("2 months' service ask: %d %v", s, r)
	}
	if _, sim := c.do("POST", "/api/approvals/simulate", "EMP-1020", "Employee", map[string]any{"wf": "p_adv", "payload": map[string]any{"amount": 2000, "note": "x"}}); !strings.Contains(fmt.Sprint(sim["problems"]), "3 months' service") {
		t.Fatalf("simulate: %v", sim)
	}
	// The setting: 2 months makes them eligible (Payroll settings at Edit, audited).
	if s, r := c.do("PUT", "/api/advances/settings", "EMP-1012", "Payroll Manager", map[string]any{"minMonths": 2, "maxInstalments": 6}); s != 200 {
		t.Fatal(r)
	}
	if _, f := c.do("GET", "/api/me/advance", "EMP-1020", "Employee", nil); f["eligible"] != true || apiFloat(f["maxInstalments"]) != 6 {
		t.Fatalf("2 months allowed: %v", f)
	}

	// Default recovery: the full amount in the first month after the payout, whatever the employee sends.
	s, r := ask("EMP-1016", map[string]any{"amount": 6000, "instalments": 6, "note": "Rent deposit"})
	if s != 200 {
		t.Fatal(r)
	}
	one := r["id"].(string)
	if p := apiDoc(st, "approval_request", bson.M{"_id": one})["payload"].(map[string]any); apiFloat(p["instalments"]) != 1 {
		t.Fatalf("stored instalments: %v", p)
	}
	c.approveAll(t, one)
	l := advLines(st, one)
	if len(l) != 2 || l[0]["kind"] != "earn" || l[0]["amount"] != 6000.0 || l[1]["kind"] != "ded" || l[1]["amount"] != 6000.0 || l[1]["ym"] != ymAdd(fmt.Sprint(l[0]["ym"]), 1) ||
		l[1]["label"] != "Advance recovery (1 of 1)" {
		t.Fatalf("default recovery lines: %v", l)
	}

	// The manager reads the wish in the remark and sets 3 instalments when approving.
	s, r = ask("EMP-1020", map[string]any{"amount": 9000, "note": "Medical bills, please deduct in 3 months"})
	if s != 200 {
		t.Fatal(r)
	}
	adv := r["id"].(string)
	d := c.request(t, adv)
	if waitingOn(d) != "Line Manager" {
		t.Fatalf("first stage %s", waitingOn(d))
	}
	mgr := fmt.Sprint(apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["manager_id"])
	role := "Employee"
	if m := apiDoc(st, "user_role_map", bson.M{"employee_id": mgr}); m != nil {
		role = fmt.Sprint(m["role_name"])
	}
	c.signIn(mgr)
	if s, r := c.do("POST", "/api/approvals/"+adv+"/approve", mgr, role, map[string]any{"note": "ok", "instalments": 7}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "1 to 6") {
		t.Fatalf("above the company's most: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals/"+adv+"/approve", "EMP-1016", "Employee", map[string]any{"instalments": 3}); s != 403 {
		t.Fatalf("someone else set the instalments: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals/"+adv+"/approve", mgr, role, map[string]any{"note": "As asked", "instalments": 3}); s != 200 {
		t.Fatalf("manager approves with 3: %d %v", s, r)
	}
	if h := notes(c.request(t, adv)); !strings.Contains(h, "Instalments set by") || !strings.Contains(h, "(Line Manager) when approving: 3 monthly instalments") {
		t.Fatalf("history: %s", h)
	}
	if apiCount(st, "audit_log", bson.M{"action": "Advance instalments set", "entity_id": adv}) != 1 {
		t.Fatal("not audited")
	}
	c.approveAll(t, adv)
	l = advLines(st, adv)
	if len(l) != 4 || l[1]["amount"] != 3000.0 || l[3]["label"] != "Advance recovery (3 of 3)" || l[1]["ym"] != ymAdd(fmt.Sprint(l[0]["ym"]), 1) {
		t.Fatalf("3 instalments: %v", l)
	}
	_ = open
}
