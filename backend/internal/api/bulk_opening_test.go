package api_test

// Bulk update › Opening uniform and Opening advances (bulk_opening.go, owner's request of 13 Nov 2026): staff moved
// onto the HRMS mid-year whose uniform and advance recoveries started under the old system.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// ymText writes a month in another layout ("Jan-2006", "01/2006").
func ymText(ym, layout string) string {
	t, _ := time.Parse("2006-01", ym)
	return t.Format(layout)
}

func ymLong(ym string) string { t, _ := time.Parse("2006-01", ym); return t.Format("January 2006") }

// bulkRows is a bulk check or apply's results by line.
func bulkRows(t *testing.T, r map[string]any) map[int]map[string]any {
	t.Helper()
	out := map[int]map[string]any{}
	l, ok := r["results"].([]any)
	if !ok {
		t.Fatalf("no results: %v", r)
	}
	for _, x := range l {
		m := x.(map[string]any)
		out[int(apiFloat(m["line"]))] = m
	}
	return out
}

// changeTo is the "to" of a row's change by field ("" when there is none).
func changeTo(m map[string]any, field string) string {
	for _, x := range m["changes"].([]any) {
		if ch := x.(map[string]any); ch["field"] == field {
			return fmt.Sprint(ch["to"])
		}
	}
	return ""
}

func rowErr(m map[string]any) string { return fmt.Sprint(m["errors"]) }

// permanentStaff are active permanent employees who joined over two years before ym, other than those named.
func permanentStaff(t *testing.T, st *store.Store, before string, skip ...string) []string {
	rows, err := st.DB.C("employee").Find(context.Background(), bson.M{"status": "Active", "type": "Permanent", "doj": bson.M{"$lt": before + "-01"},
		"lwd": nil, "_id": bson.M{"$nin": append([]string{"EMP-1001", "EMP-1002", "EMP-1004", "EMP-1008", "EMP-1009", "EMP-1011", "EMP-1012"}, skip...)}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}))
	if err != nil || len(rows) < 6 {
		t.Fatalf("permanent staff: %v %d", err, len(rows))
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, fmt.Sprint(r["id"]))
	}
	return out
}

func TestBulkOpeningUniform(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1014", "EMP-1020")
	first := uniOpenYm(c) // no Deduction closure is closed: the first HRMS deduction month is the open month
	now := time.Now().Format("2006-01")
	staff := permanentStaff(t, st, ymAdd(now, -25), "EMP-1016", "EMP-1020")
	leaver, onNotice, pre := staff[0], staff[1], staff[2]

	// Who sees it: HR Admin (with Uniform sizes and the other opening templates), whoever issues uniform (HRBP), not
	// an employee.
	keys := func(emp, role string) string {
		_, l := c.do("GET", "/api/bulk", emp, role, nil)
		ks := []string{}
		for _, x := range l["list"].([]any) {
			m := x.(map[string]any)
			ks = append(ks, fmt.Sprint(m["key"]))
			if m["key"] == "openuni" && !strings.Contains(fmt.Sprint(m["note"]), ymLong(first)) {
				t.Fatalf("the note names the first HRMS month %s: %v", first, m["note"])
			}
		}
		return "," + strings.Join(ks, ",") + ","
	}
	if k := keys("EMP-1002", "HR Admin"); !strings.Contains(k, ",openuni,") || !strings.Contains(k, ",openadv,") || !strings.Contains(k, ",uni,") || !strings.Contains(k, ",lvopen,") {
		t.Fatalf("HR Admin templates: %s", k)
	}
	if k := keys("EMP-1004", "HRBP"); !strings.Contains(k, ",openuni,") || strings.Contains(k, ",openadv,") {
		t.Fatalf("HRBP templates: %s", k)
	}
	if s, _ := c.sendFile("/api/bulk/openuni/check", "EMP-1020", "Employee", "u.csv", []byte("Employee ID\n")); s != 403 {
		t.Fatal("an employee cannot load opening uniform")
	}

	// The template: its columns, drop-downs and How to fill with the owner's example.
	s, tpl := c.raw("/api/bulk/openuni/download", "EMP-1002", "HR Admin")
	if s != 200 {
		t.Fatalf("template %d", s)
	}
	x, err := excelize.OpenReader(bytes.NewReader(tpl))
	if err != nil {
		t.Fatal(err)
	}
	head, _ := x.GetRows("Data")
	if strings.Join(head[0], "|") != "Employee ID|Issued month|Uniform amount (₹)|Instalments|Items / note|Recover" {
		t.Fatalf("columns %v", head[0])
	}
	how, _ := x.GetRows("How to fill")
	if h := fmt.Sprint(how); !strings.Contains(h, "EMP-1020 | 2026-09 | 5000 | 2") || !strings.Contains(h, "only ₹2,500 is deducted, from October salary") ||
		!strings.Contains(h, "The first HRMS deduction month is now "+ymLong(first)) {
		t.Fatalf("How to fill: %v", h)
	}

	// The people: a leaver (inactive, left in the first HRMS month), one on notice and one pre-boarding.
	set := func(id string, m bson.M) {
		if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": m}); err != nil {
			t.Fatal(err)
		}
	}
	set(leaver, bson.M{"status": "Inactive", "lwd": first + "-10", "exit_reason": "Resigned"})
	set(onNotice, bson.M{"exit_status": "On notice", "lwd": ymAdd(first, 1) + "-20"})
	set(pre, bson.M{"status": "Pre-boarding"})

	prev := ymAdd(first, -1)
	csv := "Employee ID,Issued month,Uniform amount (₹),Instalments,Items / note,Recover\n" +
		"EMP-1020," + ymText(prev, "Jan-2006") + ",5000,2,Shirt and trousers,Yes\n" + // 2: the owner's case
		"EMP-1016," + ymText(first, "01/2006") + ",\"3,000\",,,\n" + // 3: Setup split, all in the HRMS
		"EMP-1014," + prev + ",2000,2,Contract staff,\n" + // 4: not permanent: recorded only
		staff[3] + "," + prev + ",1500,,Spare shirt,No\n" + // 5: Recover No: recorded only
		staff[4] + "," + ymAdd(first, -12) + ",1200,2,,\n" + // 6: refund due before the HRMS
		"EMP-1020," + ymAdd(now, 1) + ",1000,,,\n" + // 7: future
		"EMP-1020," + ymAdd(now, -25) + ",1000,,,\n" + // 8: more than 24 months back
		"EMP-1020," + ymAdd(first, -2) + ",0,,,\n" + // 9: amount 0
		"EMP-1020," + ymAdd(first, -3) + ",100001,,,\n" + // 10: amount above 1,00,000
		"EMP-1020," + ymAdd(first, -4) + ",1000,13,,\n" + // 11: 13 instalments
		"EMP-9999," + prev + ",1000,,,\n" + // 12: unknown
		pre + "," + prev + ",1000,,,\n" + // 13: pre-boarding
		"EMP-1020," + ymText(prev, "Jan-2006") + ",5000,2,,\n" + // 14: the same row again
		"EMP-1020," + ymAdd(first, -5) + ",1000,1,,Maybe\n" + // 15: Recover not Yes or No
		"EMP-1016," + prev + ",1000,3,,\n" + // 16: split to the paisa, remainder last
		leaver + "," + prev + ",4000,2,,\n" + // 17: inactive: no refund
		onNotice + "," + prev + ",2400,2,,\n" // 18: leaving before the refund month
	_, chk := c.sendFile("/api/bulk/openuni/check", "EMP-1002", "HR Admin", "openuni.csv", []byte(csv))
	rs := bulkRows(t, chk)
	if apiFloat(chk["ok"]) != 7 {
		t.Fatalf("7 good rows: %v", chk)
	}
	refund := ymAdd(first, 6) // 6 months after the last deduction (the first HRMS month)
	r2 := rs[2]
	if len(r2["errors"].([]any)) != 0 || changeTo(r2, "Recovered before HRMS") != "₹2,500: "+ymText(prev, "Jan 2006")+" ₹2,500" ||
		changeTo(r2, "HRMS deductions") != ymText(first, "Jan 2006")+" ₹2,500" || changeTo(r2, "Refund") != "₹5,000 in "+ymLong(refund) ||
		changeTo(r2, "Uniform issued") != "₹5,000 in "+ymLong(prev)+", 2 instalments" {
		t.Fatalf("the owner's case: %v", r2)
	}
	if r3 := rs[3]; changeTo(r3, "Recovered before HRMS") != "nothing" || changeTo(r3, "HRMS deductions") != ymText(first, "Jan 2006")+" ₹1,500 · "+ymText(ymAdd(first, 1), "Jan 2006")+" ₹1,500" ||
		!strings.Contains(changeTo(r3, "Uniform issued"), "Setup split 50%") {
		t.Fatalf("Setup split: %v", r3)
	}
	for _, l := range []int{4, 5} {
		if m := rs[l]; !strings.Contains(changeTo(m, "Uniform issued"), "recorded only") || len(m["changes"].([]any)) != 1 {
			t.Fatalf("row %d is recorded only: %v", l, m)
		}
	}
	want := map[int]string{6: "already refunded before the HRMS; nothing to load", 7: "in the future", 8: "more than 24 months back", 9: "1 to 1,00,000", 10: "1 to 1,00,000",
		11: "1 to 12", 12: "not found", 13: "pre-boarding", 14: "Appears again", 15: "Recover must be Yes or No"}
	for l, w := range want {
		if !strings.Contains(rowErr(rs[l]), w) {
			t.Fatalf("row %d: want %q, got %v", l, w, rs[l])
		}
	}
	if m := rs[16]; changeTo(m, "Recovered before HRMS") != "₹333.33: "+ymText(prev, "Jan 2006")+" ₹333.33" ||
		changeTo(m, "HRMS deductions") != ymText(first, "Jan 2006")+" ₹333.33 · "+ymText(ymAdd(first, 1), "Jan 2006")+" ₹333.34" {
		t.Fatalf("equal split to the paisa: %v", m)
	}
	if m := rs[17]; !strings.HasPrefix(changeTo(m, "Refund"), "not paid") {
		t.Fatalf("inactive: %v", m)
	}
	if m := rs[18]; !strings.HasPrefix(changeTo(m, "Refund"), "not paid") {
		t.Fatalf("on notice, leaving before the refund month: %v", m)
	}
	if n := apiCount(st, "uniform_issue", bson.M{"opening": true}); n != 0 {
		t.Fatal("a check changes nothing")
	}

	// Apply.
	_, ap := c.sendFile("/api/bulk/openuni/apply", "EMP-1002", "HR Admin", "openuni.csv", []byte(csv))
	if apiFloat(ap["ok"]) != 7 {
		t.Fatalf("apply: %v", ap)
	}
	issue := func(emp, ym string) store.Row {
		return apiDoc(st, "uniform_issue", bson.M{"employee_id": emp, "opening": true, "issued_ym": ym})
	}
	u := issue("EMP-1020", prev)
	if u == nil || u["store_id"] != nil || u["recover"] != true || apiFloat(u["amount"]) != 5000 || u["note"] != "Shirt and trousers" || u["issued_on"] != prev+"-01" ||
		u["refund_rule"] != "6 months after the last deduction" || apiFloat(u["instalments"]) != 2 {
		t.Fatalf("issue: %v", u)
	}
	id := fmt.Sprint(u["id"])
	if apiCount(st, "inv_move", bson.M{"ref": id}) != 0 {
		t.Fatal("no stock movement")
	}
	l := advLines(st, id)
	if len(l) != 2 || l[0]["kind"] != "ded" || l[0]["ym"] != first || l[0]["amount"] != 2500.0 || l[0]["label"] != "Uniform recovery (2 of 2)" ||
		l[1]["kind"] != "earn" || l[1]["ym"] != refund || l[1]["amount"] != 5000.0 || l[1]["label"] != "Uniform deposit refund" {
		t.Fatalf("pay lines: %v", l)
	}
	// The Uniform card: the schedule with the month recovered before the HRMS, what is recovered and the refund.
	_, card := c.do("GET", "/api/employees/EMP-1020/uniform", "EMP-1002", "HR Admin", nil)
	var row map[string]any
	for _, x := range card["issues"].([]any) {
		if m := x.(map[string]any); m["id"] == id {
			row = m
		}
	}
	if row == nil || row["opening"] != true || row["issuedYm"] != prev || apiFloat(row["recovered"]) != 2500 || apiFloat(row["toRecover"]) != 2500 ||
		row["refundStatus"] != "scheduled" || row["refundYm"] != refund {
		t.Fatalf("card: %v", row)
	}
	sched := row["schedule"].([]any)
	if len(sched) != 2 || sched[0].(map[string]any)["before"] != true || sched[0].(map[string]any)["ym"] != prev || sched[1].(map[string]any)["ym"] != first {
		t.Fatalf("schedule: %v", sched)
	}
	// Recorded only: no pay line.
	for _, emp := range []string{"EMP-1014", staff[3]} {
		x := issue(emp, prev)
		if x == nil || x["recover"] != false || apiCount(st, "ledger", bson.M{"source_id": x["id"]}) != 0 {
			t.Fatalf("%s recorded only: %v", emp, x)
		}
	}
	// The leavers: the recovery stands, the refund is cancelled as on leaving, and the full and final says so.
	for _, emp := range []string{leaver, onNotice} {
		x := issue(emp, prev)
		ref := uniRefund(st, fmt.Sprint(x["id"]))
		if ref["void"] != true || !strings.HasPrefix(fmt.Sprint(ref["void_reason"]), "No uniform refund: left before the refund month") {
			t.Fatalf("%s refund: %v", emp, ref)
		}
		if apiCount(st, "ledger", bson.M{"source_id": x["id"], "kind": "ded", "void": false}) != 1 {
			t.Fatalf("%s: the HRMS deduction stands", emp)
		}
	}
	_, pv := c.do("GET", "/api/fnf/"+leaver, "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(pv["lines"]), "Uniform deposit refund not paid") {
		t.Fatalf("F&F of the leaver: %v", pv["lines"])
	}
	_, card = c.do("GET", "/api/employees/"+leaver+"/uniform", "EMP-1002", "HR Admin", nil)
	if fmt.Sprint(card["issues"].([]any)[0].(map[string]any)["refundStatus"]) != "cancelled" {
		t.Fatalf("leaver card: %v", card["issues"])
	}

	// Uploading the same file again changes nothing.
	_, again := c.sendFile("/api/bulk/openuni/apply", "EMP-1002", "HR Admin", "openuni.csv", []byte(csv))
	if apiFloat(again["ok"]) != 0 || !strings.Contains(rowErr(bulkRows(t, again)[2]), "Already loaded as "+id) {
		t.Fatalf("reload: %v", again)
	}

	// An Excel file with a date cell for the month, and the service basis: the refund once 12 months are complete.
	if s, r := c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 50, "refundAfter": 6, "refundBasis": "service", "serviceMonths": 12}); s != 200 {
		t.Fatal(r)
	}
	f, _ := excelize.OpenReader(bytes.NewReader(tpl))
	when, _ := time.Parse("2006-01-02", first+"-01")
	dateStyle, _ := f.NewStyle(&excelize.Style{NumFmt: 15}) // d-mmm-yy
	_ = f.SetSheetRow("Data", "A2", &[]any{staff[5], when, 900, 3})
	_ = f.SetCellStyle("Data", "B2", "B2", dateStyle)
	var buf bytes.Buffer
	_ = f.Write(&buf)
	_, xr := c.sendFile("/api/bulk/openuni/check", "EMP-1002", "HR Admin", "openuni.xlsx", buf.Bytes())
	m := bulkRows(t, xr)[2]
	doj := fmt.Sprint(apiDoc(st, "employee", bson.M{"_id": staff[5]})["doj"])
	wantRefund := uniMax(uniServiceDone(doj, 12), ymAdd(first, 3))
	if apiFloat(xr["ok"]) != 1 || changeTo(m, "Refund") != "₹900 in "+ymLong(wantRefund) || changeTo(m, "HRMS deductions") != ymText(first, "Jan 2006")+" ₹300 · "+
		ymText(ymAdd(first, 1), "Jan 2006")+" ₹300 · "+ymText(ymAdd(first, 2), "Jan 2006")+" ₹300" {
		t.Fatalf("xlsx with a date cell, service basis: %v", xr)
	}

	// The exit clearance does not ask for an opening issue back (it has no items).
	if n := apiCount(st, "uniform_issue", bson.M{"employee_id": "EMP-1020", "returned": false, "opening": bson.M{"$ne": true}}); n != 0 {
		t.Fatalf("EMP-1020 holds %d ordinary issues", n)
	}
}

func TestBulkOpeningAdvances(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1020")
	first := uniOpenYm(c)
	now := time.Now().Format("2006-01")
	staff := permanentStaff(t, st, ymAdd(now, -12), "EMP-1016", "EMP-1020")

	if k := fmt.Sprint(c.do("GET", "/api/bulk", "EMP-1004", "HRBP", nil)); strings.Contains(k, "openadv") {
		t.Fatal("HRBP does not load opening advances")
	}
	// The company allows up to 3 × the monthly gross, one advance at a time.
	if s, r := c.do("PUT", "/api/advances/settings", "EMP-1011", "Payroll", map[string]any{"maxMultiple": 3, "oneOpen": true}); s != 200 {
		t.Fatal(r)
	}
	s, tpl := c.raw("/api/bulk/openadv/download", "EMP-1011", "Payroll")
	x, err := excelize.OpenReader(bytes.NewReader(tpl))
	if s != 200 || err != nil {
		t.Fatalf("template %d %v", s, err)
	}
	head, _ := x.GetRows("Data")
	how, _ := x.GetRows("How to fill")
	if strings.Join(head[0], "|") != "Employee ID|Advance amount (₹)|Advance given month|Deduction start month|Monthly deduction (₹)|Deduction closing month|Reference / reason" ||
		!strings.Contains(fmt.Sprint(how), "EMP-1020 | 30000 | 2026-06 | 2026-07 | 5000 | 2026-12 | Voucher 2231") {
		t.Fatalf("template %v %v", head[0], how)
	}

	csv := "Employee ID,Advance amount (₹),Advance given month,Deduction start month,Monthly deduction (₹),Deduction closing month,Reference / reason\n" +
		fmt.Sprintf("EMP-1020,30000,%s,%s,5000,%s,Voucher 2231\n", ymAdd(first, -4), ymAdd(first, -3), ymAdd(first, 2)) + // 2: 15,000 recovered, 3 × 5,000 left
		fmt.Sprintf("EMP-1016,\"12,000\",%s,%s,5000,%s,\n", ymText(ymAdd(first, -3), "Jan-2006"), ymText(ymAdd(first, -2), "01/2006"), ymAdd(first, 0)) + // 3: 2,000 left
		fmt.Sprintf("%s,10000,%s,%s,3000,%s,Wrong close\n", staff[0], ymAdd(first, -1), ymAdd(first, -1), ymAdd(first, 1)) + // 4: closing should be first+2
		fmt.Sprintf("%s,6000,%s,%s,2000,%s,All done\n", staff[1], ymAdd(first, -5), ymAdd(first, -4), ymAdd(first, -2)) + // 5: nothing left
		fmt.Sprintf("%s,6000,%s,%s,2000,%s,Early start\n", staff[2], ymAdd(first, -1), ymAdd(first, -2), ymAdd(first, 0)) + // 6: start before given
		fmt.Sprintf("%s,200000,%s,%s,10000,%s,Too much\n", staff[3], ymAdd(first, -1), first, ymAdd(first, 19)) + // 7: above 3 × gross, still loaded (opening: no limits)
		fmt.Sprintf("EMP-1014,6000,%s,%s,2000,%s,Contract\n", ymAdd(first, -1), first, ymAdd(first, 2)) + // 8: not permanent
		fmt.Sprintf("EMP-1020,4000,%s,%s,2000,%s,Second\n", ymAdd(first, -1), first, ymAdd(first, 1)) + // 9: a second open advance, still loaded
		fmt.Sprintf("%s,4000,%s,%s,2000,%s,Starts later\n", staff[0], first, ymAdd(first, 1), ymAdd(first, 2)) // 10: nothing recovered yet
	_, chk := c.sendFile("/api/bulk/openadv/check", "EMP-1011", "Payroll", "openadv.csv", []byte(csv))
	rs := bulkRows(t, chk)
	if apiFloat(chk["ok"]) != 5 {
		t.Fatalf("5 good rows: %v", chk)
	}
	F := func(ym string) string { return ymText(ym, "Jan 2006") }
	if m := rs[2]; changeTo(m, "Recovered before HRMS") != "₹15,000: "+F(ymAdd(first, -3))+" ₹5,000 · "+F(ymAdd(first, -2))+" ₹5,000 · "+F(ymAdd(first, -1))+" ₹5,000" ||
		changeTo(m, "HRMS deductions") != F(first)+" ₹5,000 · "+F(ymAdd(first, 1))+" ₹5,000 · "+F(ymAdd(first, 2))+" ₹5,000" || changeTo(m, "Balance to recover") != "₹15,000" {
		t.Fatalf("row 2: %v", m)
	}
	if m := rs[3]; changeTo(m, "Recovered before HRMS") != "₹10,000: "+F(ymAdd(first, -2))+" ₹5,000 · "+F(ymAdd(first, -1))+" ₹5,000" ||
		changeTo(m, "HRMS deductions") != F(first)+" ₹2,000" || changeTo(m, "Balance to recover") != "₹2,000" {
		t.Fatalf("row 3 (remainder): %v", m)
	}
	want := map[int]string{4: "Deduction closing month should be " + ymLong(ymAdd(first, 2)), 5: "Nothing left to recover", 6: "before the month the advance was given",
		8: "permanent"}
	for l, w := range want {
		if !strings.Contains(rowErr(rs[l]), w) {
			t.Fatalf("row %d: want %q, got %v", l, w, rs[l])
		}
	}
	if m := rs[10]; changeTo(m, "Recovered before HRMS") != "nothing" || changeTo(m, "HRMS deductions") != F(ymAdd(first, 1))+" ₹2,000 · "+F(ymAdd(first, 2))+" ₹2,000" {
		t.Fatalf("row 10: %v", m)
	}
	if apiCount(st, "approval_request", bson.M{"payload.opening": true}) != 0 {
		t.Fatal("a check changes nothing")
	}

	_, ap := c.sendFile("/api/bulk/openadv/apply", "EMP-1011", "Payroll", "openadv.csv", []byte(csv))
	if apiFloat(ap["ok"]) != 5 {
		t.Fatalf("apply: %v", ap)
	}
	adv := func(emp string) store.Row {
		return apiDoc(st, "approval_request", bson.M{"employee_id": emp, "wf": "p_adv", "payload.opening": true})
	}
	a := adv("EMP-1020")
	p := a["payload"].(map[string]any)
	if a["status"] != "Approved" || apiFloat(p["amount"]) != 30000 || apiFloat(p["balance"]) != 15000 || apiFloat(p["monthly"]) != 5000 || p["startYm"] != first ||
		apiFloat(p["instalments"]) != 3 || p["advanceDate"] != ymAdd(first, -4) || p["reference"] != "Voucher 2231" || apiFloat(p["recoveredBefore"]) != 15000 ||
		p["closingYm"] != ymAdd(first, 2) || p["deductionStartYm"] != ymAdd(first, -3) {
		t.Fatalf("stored: %v", a)
	}
	l := advLines(st, fmt.Sprint(a["id"]))
	if len(l) != 3 || l[0]["ym"] != first || l[0]["label"] != "Advance recovery (1 of 3)" || l[2]["ym"] != ymAdd(first, 2) || l[2]["amount"] != 5000.0 {
		t.Fatalf("lines: %v", l)
	}
	if h := apiDoc(st, "approval_history", bson.M{"request_id": a["id"]}); !strings.Contains(fmt.Sprint(h["note"]), "advance given in "+ymLong(ymAdd(first, -4))) {
		t.Fatalf("history: %v", h)
	}
	b := adv("EMP-1016")
	if bp := b["payload"].(map[string]any); bp["reference"] != "Opening advance given "+ymLong(ymAdd(first, -3)) || apiFloat(bp["balance"]) != 2000 {
		t.Fatalf("blank reference: %v", b)
	}
	if l := advLines(st, fmt.Sprint(b["id"])); len(l) != 1 || l[0]["amount"] != 2000.0 || l[0]["ym"] != first {
		t.Fatalf("remainder line: %v", l)
	}

	// The same as the opening advances upload of Employee dues › Advances: its file for the same advance (the balance
	// from the first HRMS month) stores the same request and lines.
	old := openingFile(t, func() []byte { _, b := c.raw("/api/advances/opening/template", "EMP-1011", "Payroll"); return b }(),
		[][]string{{staff[1], "", time.Now().AddDate(0, -4, 0).Format("2006-01-02"), "30000", "15000", "5000", first, "Voucher 77"}})
	if s, r := c.postXlsx("/api/advances/opening/apply", "EMP-1011", "Payroll", "o.xlsx", old); s != 200 || apiFloat(r["added"]) != 1 {
		t.Fatalf("old upload: %d %v", s, r)
	}
	o := adv(staff[1])
	op := o["payload"].(map[string]any)
	for _, k := range []string{"amount", "balance", "monthly", "instalments", "startYm", "opening"} {
		if fmt.Sprint(op[k]) != fmt.Sprint(p[k]) {
			t.Fatalf("%s: old %v, bulk %v", k, op[k], p[k])
		}
	}
	ol := advLines(st, fmt.Sprint(o["id"]))
	if fmt.Sprint(ol) != fmt.Sprint(l) {
		t.Fatalf("lines: old %v, bulk %v", ol, l)
	}

	// Again: refused, by the reference and by one open at a time.
	_, again := c.sendFile("/api/bulk/openadv/check", "EMP-1011", "Payroll", "openadv.csv", []byte(csv))
	ra := bulkRows(t, again)
	if apiFloat(again["ok"]) != 0 || !strings.Contains(rowErr(ra[2]), "Already loaded as "+fmt.Sprint(a["id"])) || !strings.Contains(rowErr(ra[3]), "Already loaded as "+fmt.Sprint(b["id"])) {
		t.Fatalf("reload: %v", again)
	}
	// The Advances list shows them, as any opening advance.
	_, list := c.do("GET", "/api/pay-items/advances", "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(list["items"]), fmt.Sprint(a["id"])) {
		t.Fatal("the Advances list shows the bulk opening advance")
	}
}
