package api_test

// Deep tests: the rules that the sprint tests do not reach. Each test starts from a fresh
// database with the sample organisation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

func ymAdd(ym string, n int) string {
	t, _ := time.Parse("2006-01", ym)
	return t.AddDate(0, n, 0).Format("2006-01")
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// approveAs approves a request to the end with HR Admin overrides.
func (c *client) approveAll(t *testing.T, id string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		_, d := c.do("GET", "/api/approvals/"+id, "EMP-1002", "HR Admin", nil)
		if d["status"] != "Pending" {
			if d["status"] != "Approved" {
				t.Fatalf("%s ended %v", id, d["status"])
			}
			return
		}
		if s, r := c.do("POST", "/api/approvals/"+id+"/approve", "EMP-1002", "HR Admin", map[string]string{"note": "deep test"}); s != 200 {
			t.Fatalf("approve %s: %v", id, r)
		}
	}
	t.Fatalf("%s never approved", id)
}

func (c *client) getRaw(path, emp, role string) (int, []byte) {
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (c *client) row(t *testing.T, ym, id string) map[string]any {
	t.Helper()
	s, r := c.do("GET", "/api/payroll/"+ym+"/rows/"+id, "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatalf("row %s: %v", id, r)
	}
	return r["row"].(map[string]any)
}

func num(v any) float64 { f, _ := v.(float64); return f }

func (c *client) closeAndPublish(t *testing.T, ym string) {
	t.Helper()
	c.pcCloseAll(t, ym) // the run checklist, in place of Month close's reject-and-lock
	for _, step := range []struct{ path, emp, role string }{{"dry-run", "EMP-1011", "Payroll"}, {"verify", "EMP-1011", "Payroll"},
		{"approve", "EMP-1012", "Payroll Manager"}, {"publish", "EMP-1012", "Payroll Manager"}} {
		if s, r := c.do("POST", "/api/payroll/"+ym+"/"+step.path, step.emp, step.role, nil); s != 200 {
			t.Fatalf("%s %s: %v", step.path, ym, r)
		}
	}
}

func signAll(c *client, ids ...string) {
	for _, e := range ids {
		c.signIn(e)
	}
}

func setFirstYm(t *testing.T, c *client, ym string) {
	t.Helper()
	body := map[string]any{"setup": map[string]any{"accent": "#0f6e6e", "footer": "Computer generated", "showEmployer": true}, "cin": "", "pan": "AAACS1234B", "tan": "DELS12345A",
		"pfCode": "", "esiCode": "", "cutoff": "calendar", "firstYm": ym}
	if s, r := c.do("PUT", "/api/payslip-setup", "EMP-1002", "HR Admin", body); s != 200 {
		t.Fatal(r)
	}
}

// ---------- Payroll: proration rules ----------

func TestDeepPayrollRules(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)

	t.Run("mid-month structure change blends by days", func(t *testing.T) {
		_, v2 := c.do("POST", "/api/comp/structures", "EMP-1002", "HR Admin", map[string]any{"name": "Version 2"})
		id := v2["id"].(string)
		_, comp := c.do("GET", "/api/comp", "EMP-1002", "HR Admin", nil)
		var rates map[string]any
		for _, x := range comp["structures"].([]any) {
			if m := x.(map[string]any); m["id"] == id {
				rates = m["rates"].(map[string]any)
			}
		}
		rates["basicPct"] = 40.0
		if s, r := c.do("PUT", "/api/comp/structures/"+id, "EMP-1002", "HR Admin", map[string]any{"name": "Version 2", "rates": rates}); s != 200 {
			t.Fatal(r)
		}
		from := open + "-15"
		if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1002", "HR Admin", map[string]any{"structureId": id, "fromDate": from, "ids": []string{"EMP-1020"}}); s != 200 || r["mapped"] != 1.0 {
			t.Fatal(r)
		}
		row := c.row(t, open, "EMP-1020")
		if row["blended"] != true {
			t.Fatal("a structure change inside the month blends")
		}
		days := float64(mustDate(open+"-01").AddDate(0, 1, -1).Day())
		earned := num(row["earned"])
		want := earned * (0.5*14/days + 0.4*(days-14)/days)
		if !near(num(row["r"].(map[string]any)["basic"]), want, 1) {
			t.Fatalf("blended basic %v, want %v", row["r"].(map[string]any)["basic"], want)
		}
	})

	t.Run("variable pay pays the cycle's accruals", func(t *testing.T) {
		// EMP-1016 joined at the start of two months ago with 10% variable every 3 months: this month pays.
		doj := ymAdd(open, -2) + "-01"
		if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"doj": doj}}); err != nil {
			t.Fatal(err)
		}
		_, _ = st.DB.C("gross_history").UpdateMany(ctx, bson.M{"employee_id": "EMP-1016"}, bson.M{"$set": bson.M{"from_month": doj[:7]}})
		_, _ = st.DB.C("employee_pay").Upsert(ctx, bson.M{"employee_id": "EMP-1016"}, bson.M{"var_pct": 10, "var_cycle": 3}, nil)
		var gross float64
		if g, _ := st.DB.C("gross_history").FindOne(ctx, bson.M{"employee_id": "EMP-1016"}, options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}})); g != nil {
			gross, _ = g["gross"].(float64)
		}
		r := c.row(t, open, "EMP-1016")["r"].(map[string]any)
		want := num(r["variable"]) + 2*gross*0.10
		if !near(num(r["varPay"]), want, 1) {
			t.Fatalf("variable payout %v, want %v (this month %v + two full months)", r["varPay"], want, r["variable"])
		}
		// The month before is not a payout month.
		pc := c.row(t, open, "EMP-1020")["r"].(map[string]any)
		if num(pc["varPay"]) != 0 {
			t.Fatal("no variable set for EMP-1020")
		}
	})

	t.Run("retainer TDS and contractual pay", func(t *testing.T) {
		r := c.row(t, open, "EMP-1021")
		calc := r["r"].(map[string]any)
		if !near(num(calc["tds"]), num(r["earned"])*0.10, 0.01) || num(calc["eePF"]) != 0 || num(calc["eeESI"]) != 0 {
			t.Fatalf("retainer: tds %v on %v, pf %v", calc["tds"], r["earned"], calc["eePF"])
		}
		k := c.row(t, open, "EMP-1014")["r"].(map[string]any)
		if num(k["eePF"]) != 0 || num(k["ded"]) != 0 {
			t.Fatalf("contractual staff have no deductions: %v", k["ded"])
		}
	})

	t.Run("cut-off 26th to 25th", func(t *testing.T) {
		body := map[string]any{"setup": map[string]any{"accent": "#0f6e6e", "footer": "x", "showEmployer": true}, "cin": "", "pan": "", "tan": "", "pfCode": "", "esiCode": "", "cutoff": "26-25", "firstYm": ""}
		if s, r := c.do("PUT", "/api/payslip-setup", "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		_, cl := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil)
		if cl["from"] != ymAdd(open, -1)+"-26" || cl["to"] != open+"-25" {
			t.Fatalf("window %v to %v", cl["from"], cl["to"])
		}
		row := c.row(t, open, "EMP-1020")
		window := mustDate(open+"-25").Sub(mustDate(ymAdd(open, -1)+"-26")).Hours()/24 + 1
		total := 0.0
		for _, v := range row["counts"].(map[string]any) {
			total += num(v)
		}
		if total != window {
			t.Fatalf("days counted %v, window %v", total, window)
		}
		body["cutoff"] = "calendar"
		c.do("PUT", "/api/payslip-setup", "EMP-1002", "HR Admin", body)
	})

	t.Run("pilot, edits after verify, back to dry run", func(t *testing.T) {
		if s, r := c.do("PUT", "/api/payroll/"+open+"/pilot", "EMP-1011", "Payroll", map[string]any{"ids": []string{"EMP-1020", "EMP-1016"}}); s != 200 {
			t.Fatal(r)
		}
		// A pilot is a dry run too: it waits for the checklist like the full run.
		if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "Still open") {
			t.Fatal("the pilot needs the checklist closed", r)
		}
		c.pcCloseAll(t, open)
		c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil)
		_, rows := c.do("GET", "/api/payroll/"+open+"/rows", "EMP-1011", "Payroll", nil)
		if rows["total"] != 2.0 {
			t.Fatalf("pilot rows %v", rows["total"])
		}
		if s, r := c.do("POST", "/api/payroll/"+open+"/verify", "EMP-1011", "Payroll", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "pilot") {
			t.Fatal("verify needs the pilot cleared", r)
		}
		c.do("PUT", "/api/payroll/"+open+"/pilot", "EMP-1011", "Payroll", map[string]any{"ids": []string{}})
		c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil)
		_, rows = c.do("GET", "/api/payroll/"+open+"/rows", "EMP-1011", "Payroll", nil)
		if rows["total"].(float64) < 50 {
			t.Fatalf("full run rows %v", rows["total"])
		}
		c.do("POST", "/api/payroll/"+open+"/verify", "EMP-1011", "Payroll", nil)
		if s, _ := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{"paidDays": 10, "reason": "late edit"}); s != 400 {
			t.Fatal("no edits once verified")
		}
		if s, r := c.do("POST", "/api/payroll/"+open+"/back", "EMP-1011", "Payroll", nil); s != 200 || r["status"] != "Dry run" {
			t.Fatal(r)
		}
		if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{"paidDays": 10, "reason": "late edit"}); s != 200 {
			t.Fatal(r)
		}
		c.do("PATCH", "/api/payroll/"+open+"/rows/EMP-1020", "EMP-1011", "Payroll", map[string]any{})
		if c.row(t, open, "EMP-1020")["edited"] == true {
			t.Fatal("an empty edit clears the override")
		}
	})

	t.Run("corrected sheet upload", func(t *testing.T) {
		s, raw := c.getRaw("/api/payroll/"+open+"/sheet.xlsx", "EMP-1011", "Payroll")
		if s != 200 {
			t.Fatal(s)
		}
		f, err := excelize.OpenReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := f.GetRows("Data")
		head := rows[0]
		col := func(n string) int {
			for i, h := range head {
				if h == n {
					return i
				}
			}
			t.Fatalf("no column %s", n)
			return -1
		}
		target := -1
		for i, r := range rows {
			if i > 0 && r[0] == "EMP-1016" {
				target = i
			}
		}
		set := func(c int, v string) {
			cell, _ := excelize.CoordinatesToCellName(c+1, target+1)
			_ = f.SetCellValue("Data", cell, v)
		}
		set(col("Paid days"), "20")
		set(col("Other earnings"), "750")
		var buf bytes.Buffer
		_ = f.Write(&buf)
		upload := func(action string) (int, map[string]any) {
			return c.sendFile("/api/payroll/"+open+"/sheet/"+action, "EMP-1011", "Payroll", "sheet.xlsx", buf.Bytes())
		}
		if _, r := upload("check"); r["problems"] != 1.0 {
			t.Fatalf("a change without a reason is a problem: %v", r)
		}
		set(col("Adjustment reason"), "Attendance corrected by HRBP")
		buf.Reset()
		_ = f.Write(&buf)
		if _, r := upload("apply"); r["applied"] != true || r["ready"] != 1.0 {
			t.Fatal(r)
		}
		row := c.row(t, open, "EMP-1016")
		if row["paid"] != 20.0 || row["oe"] != 750.0 {
			t.Fatalf("after upload paid %v oe %v", row["paid"], row["oe"])
		}
	})
}

// ---------- Payroll: ESI period, LWF months, holds, carry-over, exited staff ----------

func TestDeepPayrollMonths(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	// Start payroll in a month after the ESI period begins, so a raise keeps ESI to the period end.
	now := time.Now()
	periodStart := fmt.Sprintf("%d-10", now.Year())
	if now.Month() < 4 {
		periodStart = fmt.Sprintf("%d-10", now.Year()-1)
	} else if now.Month() < 10 {
		periodStart = fmt.Sprintf("%d-04", now.Year())
	}
	open := ymAdd(periodStart, 1)
	setFirstYm(t, c, open)
	if _, s := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil); s["openYm"] != open {
		t.Fatalf("open month %v, want %v", s["openYm"], open)
	}

	t.Run("ESI continues to the end of the period after a raise", func(t *testing.T) {
		// EMP-1020 is on 18,000 at the period start; a raise to 25,000 from this month keeps ESI.
		_, _ = st.DB.C("gross_history").Insert(ctx, bson.M{"employee_id": "EMP-1020", "from_month": open, "gross": 25000, "old_gross": 18000, "reason": "Raise"})
		r := c.row(t, open, "EMP-1020")["r"].(map[string]any)
		if r["esiOn"] != true || num(r["med"]) != 0 {
			t.Fatalf("ESI on %v, medical %v", r["esiOn"], r["med"])
		}
		// Someone above the ceiling at the period start gets medical cover, not ESI.
		r = c.row(t, open, "EMP-1002")["r"].(map[string]any)
		if r["esiOn"] == true || num(r["med"]) == 0 {
			t.Fatal("above the ceiling: medical, not ESI")
		}
	})

	t.Run("LWF only in its months", func(t *testing.T) {
		// Delhi deducts LWF in June and December; Haryana every month.
		delhi := c.row(t, open, "EMP-1016")["r"].(map[string]any)
		want := 0.0
		if m := open[5:]; m == "06" || m == "12" {
			want = 0.75
		}
		if num(delhi["lwfEe"]) != want {
			t.Fatalf("Delhi LWF in %s: %v, want %v", open, delhi["lwfEe"], want)
		}
		if gurgaon := c.row(t, open, "EMP-1020")["r"].(map[string]any); num(gurgaon["lwfEe"]) <= 0 {
			t.Fatal("Haryana LWF every month")
		}
	})

	var holdNet float64
	t.Run("held month paid on release; carry-over of a negative net", func(t *testing.T) {
		if s, r := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1026", "reason": "Bank or KYC issue", "note": "Account frozen by the bank", "fromYm": open}); s != 200 {
			t.Fatal(r)
		}
		_, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 100000, "instalments": 1, "dedType": "Excess payment recovery", "note": "Salary paid twice in error"}})
		c.approveAll(t, r["id"].(string))
		c.closeAndPublish(t, open)
		row := c.row(t, open, "EMP-1026")
		holdNet = num(row["net"])
		if row["hold"] != true || holdNet <= 0 {
			t.Fatal("EMP-1026 is held")
		}
		_, bank := c.getRaw("/api/payroll/"+open+"/bank-advice.xlsx", "EMP-1012", "Payroll Manager")
		f, _ := excelize.OpenReader(bytes.NewReader(bank))
		rows, _ := f.GetRows("Data")
		for _, x := range rows {
			if x[0] == "EMP-1026" {
				t.Fatal("held salary is left out of the bank advice")
			}
		}
		k := c.row(t, open, "EMP-1020")
		if num(k["net"]) != 0 || num(k["carry"]) <= 0 {
			t.Fatalf("net %v carry %v", k["net"], k["carry"])
		}
		var carried float64
		if l, _ := st.DB.C("ledger").FindOne(ctx, bson.M{"employee_id": "EMP-1020", "ym": ymAdd(open, 1), "label": bson.M{"$regex": "^Carried over"}}); l != nil {
			carried, _ = l["amount"].(float64)
		}
		if !near(carried, num(k["carry"]), 0.01) {
			t.Fatalf("carried %v, want %v", carried, k["carry"])
		}
		_, holds := c.do("GET", "/api/holds", "EMP-1011", "Payroll", nil)
		var hid string
		for _, h := range holds["holds"].([]any) {
			if m := h.(map[string]any); m["employee_id"] == "EMP-1026" {
				hid = m["id"].(string)
				if !near(num(m["held"]), holdNet, 0.01) {
					t.Fatalf("held so far %v, want %v", m["held"], holdNet)
				}
			}
		}
		_, rel := c.do("POST", "/api/holds/"+hid+"/release", "EMP-1011", "Payroll", map[string]string{"note": "KYC done"})
		if !near(num(rel["paid"]), holdNet, 0.01) { // the stored net, to the paisa, as the bank advice pays it (ITM-59)
			t.Fatalf("released %v, want %v", rel["paid"], holdNet)
		}
		next := c.row(t, ymAdd(open, 1), "EMP-1026")
		found := false
		for _, l := range next["add"].([]any) {
			found = found || strings.HasPrefix(l.(map[string]any)["label"].(string), "Held salary released")
		}
		if !found || next["hold"] == true {
			t.Fatal("next month pays the held salary and is not on hold")
		}
	})

	t.Run("deductions for people who have left, claims after the window", func(t *testing.T) {
		lwd := ymAdd(open, -1) + "-20"
		if s, r := c.do("POST", "/api/employees/EMP-1023/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": lwd, "reason": "Resigned"}); s != 200 {
			t.Fatal(r)
		}
		_, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1023",
			"payload": map[string]any{"amount": 3000, "instalments": 3, "dedType": "Damage or loss of property", "note": "Broken tablet on 2 Oct", "heard": "Yes"}})
		c.approveAll(t, r["id"].(string))
		lines, _ := st.DB.C("ledger").Find(ctx, bson.M{"source_id": r["id"]})
		yms := []string{}
		for _, l := range lines {
			yms = append(yms, l["ym"].(string))
		}
		n := len(yms)
		for _, ym := range yms {
			if ym < ymAdd(open, 1) {
				t.Fatalf("deduction for a leaver lands in %s, before the open month", ym)
			}
		}
		if n != 1 {
			t.Fatalf("a leaver's deduction is one instalment, got %d", n)
		}
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1023"}, bson.M{"$set": bson.M{"lwd": store.DateOf(time.Now().AddDate(0, 0, -20))}})
		c.signIn("EMP-1023")
		_, doc := c.upload("EMP-1023", "Employee", "employee", "EMP-1023", "Bill")
		if s, r := c.do("POST", "/api/approvals", "EMP-1023", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 500, "claimType": "Travel", "billDate": today(), "doc": doc["id"]}}); s != 400 || !strings.Contains(fmt.Sprint(r), "15 days") {
			t.Fatalf("claims close 15 days after the last day: %d %v", s, r)
		}
	})

	t.Run("negative arrear is a recovery", func(t *testing.T) {
		_, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_arrear", "employeeId": "EMP-1016",
			"payload": map[string]any{"amount": -1200, "forYm": ymAdd(open, -1), "arrearType": "Attendance correction", "slipText": "Excess paid for two absent days"}})
		c.approveAll(t, r["id"].(string))
		var kind string
		var amt float64
		if l, _ := st.DB.C("ledger").FindOne(ctx, bson.M{"source_id": r["id"]}); l != nil {
			kind, _ = l["kind"].(string)
			amt, _ = l["amount"].(float64)
		}
		if kind != "ded" || amt != 1200 {
			t.Fatalf("%s %v", kind, amt)
		}
	})
}

// ---------- Payroll: the background job for large runs ----------

func TestDeepPayrollJob(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1011")
	api.SetPayInline(10)
	defer api.SetPayInline(2000)
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := app.StartJobs(jctx); err != nil {
		t.Fatal(err)
	}
	defer app.StopJobs(ctx)
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.pcCloseAll(t, open)
	_, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil)
	if r["queued"] != true {
		t.Fatalf("over the limit the run is queued: %v", r)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var status string
		var job any
		if m, _ := st.DB.C("payroll_month").FindOne(ctx, bson.M{"ym": open}); m != nil {
			status, _ = m["status"].(string)
			job = m["job"]
		}
		if job == nil && status == "Dry run" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: status %s", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	n, _ := st.DB.C("payroll_row").Count(ctx, bson.M{"ym": open})
	if n < 50 {
		t.Fatalf("job wrote %d rows", n)
	}
	var _ store.Row
	_ = multipart.NewWriter
	_ = json.Marshal
}
