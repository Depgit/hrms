package api_test

// Payroll test fixes, part B: tax finalisation, opening TDS, download audit, salary masking.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// RUN-87: finalising before the proof window's end date needs an explicit early step with a reason; the Payroll
// Manager can reopen a finalisation (audited).
func TestFixBTaxFinaliseWaitsForProofWindow(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	w := tax["windows"].(map[string]any)
	no := false
	c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": no})
	if s, r := c.do("POST", "/api/tax/finalise", "EMP-1012", "Payroll Manager", nil); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "proof window runs to") {
		t.Fatalf("finalised before the proof window ended: %d %v", s, r)
	}
	if s, _ := c.do("POST", "/api/tax/finalise", "EMP-1012", "Payroll Manager", map[string]any{"force": true}); s != 400 {
		t.Fatal("an early finalisation needs a reason")
	}
	if s, r := c.do("POST", "/api/tax/finalise", "EMP-1012", "Payroll Manager", map[string]any{"force": true, "reason": "Board asked"}); s != 200 {
		t.Fatal(r)
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Tax finalised"}}); a == nil || !strings.Contains(fmt.Sprint(a["after"]), "Board asked") {
		t.Fatalf("the early finalisation and its reason are audited: %v", a)
	}
	if s, _ := c.do("POST", "/api/tax/finalise/reopen", "EMP-1011", "Payroll", map[string]any{"reason": "x y z"}); s != 403 {
		t.Fatal("only a payroll approver reopens")
	}
	if s, _ := c.do("POST", "/api/tax/finalise/reopen", "EMP-1012", "Payroll Manager", map[string]any{}); s != 400 {
		t.Fatal("reopening needs a reason")
	}
	if s, r := c.do("POST", "/api/tax/finalise/reopen", "EMP-1012", "Payroll Manager", map[string]any{"reason": "Proofs still coming"}); s != 200 {
		t.Fatal(r)
	}
	_, tax = c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
	if tax["windows"].(map[string]any)["finalised"] != nil {
		t.Fatal("the finalisation is undone")
	}
	if n, _ := st.DB.C("tax_declaration").Count(context.Background(), bson.M{"final_at": bson.M{"$ne": nil}}); n != 0 {
		t.Fatalf("%d declarations still finalised", n)
	}
	if apiDoc(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Tax finalisation reopened"}}) == nil {
		t.Fatal("the reopening is audited")
	}
}

// REAL-32 / RUN-94: opening TDS for the months before go-live is loaded from a file (checked, then saved), and the
// monthly TDS then spreads only the tax still due. ACC-19: downloads are audited.
func TestFixBOpeningTds(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	setFirstYm(t, c, "")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	if open[5:] < "05" && open[5:] > "03" {
		t.Skip("needs months of the year before the open month")
	}
	c.pcCloseAll(t, open)
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	_, rows := c.do("GET", "/api/payroll/"+open+"/rows", "EMP-1011", "Payroll", nil)
	who, before := "", 0.0
	list, _ := rows["rows"].([]any)
	for _, x := range list {
		m := x.(map[string]any)
		if v := num(m["stds"]); v > before {
			who, before = fmt.Sprint(m["employeeId"]), v
		}
	}
	if who == "" {
		t.Skip("nobody pays salary TDS in the sample")
	}
	prev := ymAdd(open, -1)
	// A file with problems saves nothing.
	bad := "Emp ID,Month,TDS deducted\nEMP-9999," + prev + ",100\n" + who + "," + open + ",100\n" + who + "," + prev + ",abc\n"
	if s, r := c.postXlsx("/api/tds/opening/check", "EMP-1011", "Payroll", "o.csv", []byte(bad)); s != 200 || num(r["problems"]) != 3 {
		t.Fatalf("check: %d %v", s, r)
	}
	if s, _ := c.postXlsx("/api/tds/opening/apply", "EMP-1011", "Payroll", "o.csv", []byte(bad)); s != 400 {
		t.Fatal("a file with problems is refused")
	}
	if s, _ := c.postXlsx("/api/tds/opening/check", "EMP-1002", "Employee", "o.csv", []byte(bad)); s != 403 {
		t.Fatal("Payroll only")
	}
	good := "Emp ID,Month,TDS deducted\n" + who + "," + prev + "," + fmt.Sprint(before) + "\n"
	if s, r := c.postXlsx("/api/tds/opening/apply", "EMP-1011", "Payroll", "o.csv", []byte(good)); s != 200 || r["applied"] != true {
		t.Fatalf("apply: %d %v", s, r)
	}
	if h := apiDoc(st, "tds_history", bson.M{"employee_id": who, "ym": prev, "section": "192"}); h == nil || h["source"] != "opening" || num(h["amount"]) != before {
		t.Fatalf("opening TDS stored: %v", h)
	}
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	after := num(c.row(t, open, who)["stds"])
	if after >= before {
		t.Fatalf("the month's TDS counts the tax already deducted: %v before, %v after", before, after)
	}
	_, l := c.do("GET", "/api/tds/opening", "EMP-1011", "Payroll", nil)
	if num(l["total"]) != before {
		t.Fatalf("opening list: %v", l)
	}
	// ACC-19: the template download is audited, like every other download.
	if s, _ := c.getRaw("/api/tds/opening/template.xlsx", "EMP-1011", "Payroll"); s != 200 {
		t.Fatal(s)
	}
	if s, _ := c.getRaw("/api/payroll/"+open+"/sheet.xlsx", "EMP-1011", "Payroll"); s != 200 {
		t.Fatal(s)
	}
	for _, f := range []string{"opening-tds-", "payroll-sheet-" + open} {
		if n, _ := st.DB.C("audit_log").Count(ctx, bson.M{"entity": "download", "entity_id": bson.M{"$regex": "^" + f}}); n == 0 {
			t.Fatalf("download of %s not audited", f)
		}
	}
}

// ACC-24: a masked salary shows nothing of the amount.
func TestFixBMaskedSalary(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1004")
	seen := 0
	for i := 1001; i <= 1030; i++ {
		if i == 1004 {
			continue // their own salary is in full
		}
		s, e := c.do("GET", fmt.Sprintf("/api/employees/EMP-%d", i), "EMP-1004", "HRBP", nil)
		if s != 200 {
			continue
		}
		sal, _ := e["salary"].(map[string]any)
		if sal == nil || sal["gross"] == nil {
			continue
		}
		seen++
		for _, k := range []string{"gross", "ctc", "net"} {
			if v := fmt.Sprint(sal[k]); v != "₹ ••••" {
				t.Fatalf("EMP-%d %s masked as %q", i, k, v)
			}
		}
	}
	if seen == 0 {
		t.Skip("HRBP sees no salaries in the sample")
	}
}
