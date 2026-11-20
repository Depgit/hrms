package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// pfSetJoiner gives an employee a monthly gross, an existing_uan answer (nil: not answered) and no UAN on record.
// existing_uan belongs to the joining form (written there by the other builder); the test writes it directly.
func pfSetJoiner(t *testing.T, st *store.Store, id string, gross float64, existing any) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.DB.C("gross_history").UpdateMany(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"gross": gross}}); err != nil {
		t.Fatal(err)
	}
	set := bson.M{"$set": bson.M{"existing_uan": existing}}
	if existing == nil {
		set = bson.M{"$unset": bson.M{"existing_uan": ""}}
	}
	if _, err := st.DB.C("employee").Raw().UpdateOne(ctx, bson.M{"_id": id}, set); err != nil {
		t.Fatal(err)
	}
	pfNoUan(t, st, id)
}

func pfNoUan(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.DB.C("employee_gov_id").Update(context.Background(), bson.M{"employee_id": id}, bson.M{"$set": bson.M{"uan": nil}}); err != nil {
		t.Fatal(err)
	}
}

func pfSweep(t *testing.T, app *api.Server) {
	t.Helper()
	if _, err := app.PfExemptSweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func pfExempt(st *store.Store, id string) bool {
	p := apiDoc(st, "employee_pay", bson.M{"employee_id": id})
	return p != nil && p["pf_exempt"] == true
}

// PF exemption: yes for a joiner above ₹25,000 (₹20,000 until 19 Nov 2026) with no UAN, no otherwise; the Basic warning; the manual override.
func TestPfExemptJoiner(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012", "EMP-1020")
	pfSetJoiner(t, st, "EMP-1020", 27000, false) // exempt: Basic 13,500, so warned
	pfSetJoiner(t, st, "EMP-1016", 18000, false) // not above the threshold
	pfSetJoiner(t, st, "EMP-1021", 40000, true)  // an existing member
	pfSetJoiner(t, st, "EMP-1022", 40000, nil)   // not answered: unknown
	pfSweep(t, app)
	if !pfExempt(st, "EMP-1020") {
		t.Fatal("EMP-1020 is not PF exempt")
	}
	for _, id := range []string{"EMP-1016", "EMP-1021", "EMP-1022"} {
		if pfExempt(st, id) {
			t.Fatalf("%s is PF exempt", id)
		}
	}
	if apiCount(st, "audit_log", bson.M{"action": "PF exemption decided: exempt", "entity_id": "EMP-1020", "actor": "System"}) != 1 {
		t.Fatal("the decision is audited")
	}
	s, g := c.do("GET", "/api/employees/EMP-1020/pf-exempt", "EMP-1011", "Payroll", nil)
	if s != 200 || g["exempt"] != true || g["manual"] == true || !strings.Contains(fmt.Sprint(g["reason"]), "above ₹25,000") || g["by"] != "System" || g["canEdit"] != true {
		t.Fatalf("status: %d %v", s, g)
	}
	if !strings.Contains(fmt.Sprint(g["warning"]), "Basic ₹13,500") {
		t.Fatalf("Basic ≤ 15,000 warning: %v", g["warning"])
	}
	if _, g := c.do("GET", "/api/employees/EMP-1016/pf-exempt", "EMP-1011", "Payroll", nil); g["exempt"] != false || g["warning"] != "" {
		t.Fatalf("EMP-1016: %v", g)
	}
	// The payroll checklist warns about the same person.
	open := pdOpenYm(t, c)
	if w := fmt.Sprint(pdChecklist(t, c, open)["warnings"]); !strings.Contains(w, "PF exempt with Basic ≤ ₹15,000: check") || !strings.Contains(w, "EMP-1020") {
		t.Fatalf("checklist warnings: %s", w)
	}
	// A raised gross threshold makes the next sweep take it back (no PF history yet).
	if s, r := c.do("PUT", "/api/payroll/pf-settings", "EMP-1012", "Payroll Manager", map[string]any{"exemptGross": 30000}); s != 200 {
		t.Fatal(r)
	}
	pfSweep(t, app)
	if pfExempt(st, "EMP-1020") {
		t.Fatal("still exempt below the new threshold")
	}
	if s, _ := c.do("PUT", "/api/payroll/pf-settings", "EMP-1020", "Employee", map[string]any{"exemptGross": 20000}); s != 403 {
		t.Fatal("an employee changed the PF settings")
	}
	if s, r := c.do("PUT", "/api/payroll/pf-settings", "EMP-1012", "Payroll Manager", map[string]any{"exemptGross": 20000}); s != 200 {
		t.Fatal(r)
	}
	if apiCount(st, "audit_log", bson.M{"action": "PF settings updated"}) != 2 {
		t.Fatal("settings changes are audited")
	}
	pfSweep(t, app)
	if !pfExempt(st, "EMP-1020") {
		t.Fatal("exempt again at 20,000")
	}

	// The manual override: Payroll or HR Admin, with a reason; the sweep leaves it alone; back to the rule.
	if s, _ := c.do("PUT", "/api/employees/EMP-1020/pf-exempt", "EMP-1004", "HRBP", map[string]any{"mode": "member", "reason": "Wants PF"}); s != 403 {
		t.Fatal("HRBP overrode the exemption")
	}
	if s, _ := c.do("PUT", "/api/employees/EMP-1020/pf-exempt", "EMP-1011", "Payroll", map[string]any{"mode": "member", "reason": ""}); s != 400 {
		t.Fatal("no reason accepted")
	}
	s, o := c.do("PUT", "/api/employees/EMP-1020/pf-exempt", "EMP-1011", "Payroll", map[string]any{"mode": "member", "reason": "Asked to stay in PF"})
	if s != 200 || o["exempt"] != false || o["manual"] != true || !strings.Contains(fmt.Sprint(o["reason"]), "Asked to stay in PF") {
		t.Fatalf("override: %d %v", s, o)
	}
	pfSweep(t, app)
	if pfExempt(st, "EMP-1020") {
		t.Fatal("the sweep undid the manual override")
	}
	if s, o := c.do("PUT", "/api/employees/EMP-1016/pf-exempt", "EMP-1002", "HR Admin", map[string]any{"mode": "exempt", "reason": "Opted out at joining"}); s != 200 || o["exempt"] != true || o["warning"] == "" {
		t.Fatalf("HR Admin exempts by hand: %d %v", s, o)
	}
	if s, o := c.do("PUT", "/api/employees/EMP-1020/pf-exempt", "EMP-1011", "Payroll", map[string]any{"mode": "auto", "reason": "Back to the rule"}); s != 200 || o["exempt"] != true || o["manual"] == true {
		t.Fatalf("back to the rule: %d %v", s, o)
	}
	if apiCount(st, "audit_log", bson.M{"action": "PF exemption changed by hand"}) != 3 {
		t.Fatal("overrides are audited")
	}
}

// The UAN check: a PF member with no UAN has the salary held at the run; a PF exempt joiner is not held and pays no PF;
// the next run releases the hold once the UAN is entered and pays the held month.
func TestUanHoldAndRelease(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	pfSetJoiner(t, st, "EMP-1020", 27000, false) // PF exempt: no hold
	pfNoUan(t, st, "EMP-1016")                   // a PF member with no UAN
	pfSweep(t, app)
	ym := pdOpenYm(t, c)

	// The checklist lists EMP-1016 under Bank verification › UAN not provided, not as pending.
	var bank map[string]any
	for _, x := range pdChecklist(t, c, ym)["closures"].([]any) {
		if m := x.(map[string]any); m["key"] == "bank" {
			bank = m
		}
	}
	ids := fmt.Sprint(bank["uan"])
	if num(bank["uanN"]) < 1 || !strings.Contains(ids, "EMP-1016") || strings.Contains(ids, "EMP-1020") || strings.Contains(fmt.Sprint(bank["items"]), "UAN not provided") {
		t.Fatalf("bank closure: uanN %v uan %s items %v", bank["uanN"], ids, bank["items"])
	}

	c.pcCloseAll(t, ym)
	pfaMust(t, c, ym, "dry-run", "EMP-1011", "Payroll")
	h := apiDoc(st, "salary_hold", bson.M{"employee_id": "EMP-1016", "status": "Active"})
	if h == nil || h["source_id"] != "no-uan" || h["reason"] != "UAN not provided" || h["from_ym"] != ym || h["by_name"] != "System" {
		t.Fatalf("hold: %v", h)
	}
	if row := c.row(t, ym, "EMP-1016"); row["hold"] != true || !strings.Contains(fmt.Sprint(row["holdWhy"]), "UAN not provided") {
		t.Fatalf("row not held: %v", row["holdWhy"])
	}
	ex := c.row(t, ym, "EMP-1020")
	r := ex["r"].(map[string]any)
	if ex["hold"] == true || num(r["eePF"]) != 0 || num(r["erPF"]) != 0 || num(r["edli"]) != 0 || r["pfExempt"] != true {
		t.Fatalf("exempt row: hold %v r %v", ex["hold"], r)
	}
	if apiDoc(st, "salary_hold", bson.M{"employee_id": "EMP-1020", "status": "Active"}) != nil {
		t.Fatal("the PF exempt joiner is held")
	}
	pfaMust(t, c, ym, "verify", "EMP-1011", "Payroll")
	pfaMust(t, c, ym, "approve", "EMP-1012", "Payroll Manager")
	// The ECR leaves the exempt joiner out.
	if _, e := c.do("GET", "/api/statutory/ecr?ym="+ym, "EMP-1012", "Payroll Manager", nil); strings.Contains(fmt.Sprint(e["rows"]), "EMP-1020") {
		t.Fatal("PF exempt joiner in the ECR")
	}

	// Next month the UAN is on record: the run releases the hold and pays the held month.
	next := pdOpenYm(t, c)
	if _, err := st.DB.C("employee_gov_id").Update(ctx, bson.M{"employee_id": "EMP-1016"}, bson.M{"$set": bson.M{"uan": "100100001016"}}); err != nil {
		t.Fatal(err)
	}
	c.pcCloseAll(t, next)
	pfaMust(t, c, next, "dry-run", "EMP-1011", "Payroll")
	if apiDoc(st, "salary_hold", bson.M{"_id": h["id"], "status": "Released", "released_by": "System"}) == nil {
		t.Fatal("hold not released by the run")
	}
	if apiDoc(st, "ledger", bson.M{"employee_id": "EMP-1016", "ym": next, "void": false, "label": "Held salary released, " + ymLong(ym)}) == nil {
		t.Fatal("no Held salary released line")
	}
	if c.row(t, next, "EMP-1016")["hold"] == true {
		t.Fatal("still held")
	}

	// Switched off: nobody is held for a missing UAN.
	if s, r := c.do("PUT", "/api/payroll/pf-settings", "EMP-1012", "Payroll Manager", map[string]any{"uanHold": false}); s != 200 {
		t.Fatal(r)
	}
	pfNoUan(t, st, "EMP-1016")
	pfaMust(t, c, next, "dry-run", "EMP-1011", "Payroll")
	if apiDoc(st, "salary_hold", bson.M{"employee_id": "EMP-1016", "status": "Active"}) != nil {
		t.Fatal("held with the UAN check off")
	}
	if _, g := c.do("GET", "/api/payroll/pf-settings", "EMP-1011", "Payroll", nil); g["rules"].(map[string]any)["uanHold"] != false {
		t.Fatalf("settings: %v", g)
	}
}

// The PF choice (owner, 19 Nov 2026): "No" (optout) by HR on the PF card makes the person PF exempt and paid as all
// Basic in the payroll (HRA and special allowance 0, no PF either side); only above the threshold with no UAN; a
// reason and the right are needed; audited. Someone without the choice keeps the normal structure.
func TestPfChoiceOptOut(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012")
	pfSetJoiner(t, st, "EMP-1020", 30000, false) // no UAN, above ₹25,000: may choose
	pfSetJoiner(t, st, "EMP-1016", 25000, false) // at the threshold: no choice
	pfSweep(t, app)
	body := map[string]any{"choice": "optout", "reason": "Chose no PF in writing"}
	if s, _ := c.do("PUT", "/api/employees/EMP-1020/pf-choice", "EMP-1004", "HRBP", body); s != 403 {
		t.Fatal("HRBP changed the PF choice")
	}
	if s, _ := c.do("PUT", "/api/employees/EMP-1020/pf-choice", "EMP-1011", "Payroll", map[string]any{"choice": "optout", "reason": ""}); s != 400 {
		t.Fatal("no reason accepted")
	}
	if s, _ := c.do("PUT", "/api/employees/EMP-1016/pf-choice", "EMP-1011", "Payroll", body); s != 400 {
		t.Fatal("a choice at the threshold accepted")
	}
	s, o := c.do("PUT", "/api/employees/EMP-1020/pf-choice", "EMP-1011", "Payroll", body)
	ch, _ := o["choice"].(map[string]any)
	if s != 200 || o["exempt"] != true || ch["value"] != "optout" || ch["allBasic"] != true || ch["by"] == nil {
		t.Fatalf("choice: %d %v", s, o)
	}
	if apiCount(st, "audit_log", bson.M{"action": "PF choice changed", "entity_id": "EMP-1020"}) != 1 {
		t.Fatal("the choice is not audited")
	}
	pfSweep(t, app) // the sweep keeps it
	if !pfExempt(st, "EMP-1020") {
		t.Fatal("the sweep undid the choice")
	}
	ym := pdOpenYm(t, c)
	c.pcCloseAll(t, ym)
	pfaMust(t, c, ym, "dry-run", "EMP-1011", "Payroll")
	r := c.row(t, ym, "EMP-1020")["r"].(map[string]any)
	if num(r["gross"]) <= 0 || num(r["basic"]) != num(r["gross"]) || num(r["hra"]) != 0 || num(r["sa"]) != 0 || num(r["eePF"]) != 0 || num(r["erPF"]) != 0 || r["pfExempt"] != true {
		t.Fatalf("opted-out row: %v", r)
	}
	n := c.row(t, ym, "EMP-1016")["r"].(map[string]any) // no choice: the normal split and PF
	if num(n["hra"]) == 0 || num(n["sa"]) == 0 || num(n["eePF"]) == 0 || num(n["basic"]) == num(n["gross"]) {
		t.Fatalf("normal row: %v", n)
	}
}
