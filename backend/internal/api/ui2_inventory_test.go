package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// ui2InvAssetID is the id of the asset with this serial (0 when there is none).
func ui2InvAssetID(st *store.Store, serial string) int64 {
	id, _ := apiDoc(st, "asset", bson.M{"serial": serial})["id"].(int64)
	return id
}

// Round 2, Employee dues: uniform setup and kits, asset names, dated stock movements, issue from the
// employee's store with the configured recovery, partial uniform returns, damaged assets, counted
// write-offs and the uniform issues list.
func TestUI2Inventory(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1003", "EMP-1004", "EMP-1011", "EMP-1020")
	shirt := apiFloat(apiDoc(st, "inv_item", bson.M{"name": "Shirt"})["id"])
	trousers := apiFloat(apiDoc(st, "inv_item", bson.M{"name": "Trousers"})["id"])
	laptop := apiFloat(apiDoc(st, "inv_item", bson.M{"name": "Laptop"})["id"])
	hrbp := "hrbp:EMP-1004"
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }
	line := func(id float64, q int) map[string]any { return map[string]any{"itemId": id, "qty": q} }
	list := func(m map[string]any) []any { l, _ := m["list"].([]any); return l }

	t.Run("setup, items and kits", func(t *testing.T) {
		s, d := c.do("GET", "/api/inventory/setup", "EMP-1002", "HR Admin", nil)
		if s != 200 || d["firstPct"] != 50.0 || d["refundAfter"] != 6.0 || d["canEdit"] != true || len(d["kits"].([]any)) < 2 {
			t.Fatal(d)
		}
		if _, d := c.do("GET", "/api/inventory/setup", "EMP-1004", "HRBP", nil); d["canEdit"] != false {
			t.Fatal("only HR Admin edits the setup")
		}
		if s, _ := c.do("PUT", "/api/inventory/setup", "EMP-1004", "HRBP", map[string]any{"firstPct": 100, "refundAfter": 12}); s != 403 {
			t.Fatal("an HRBP does not change the recovery")
		}
		if s, _ := c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 60, "refundAfter": 9}); s != 400 {
			t.Fatal("refund after 6 or 12 months only")
		}
		if s, r := c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 100, "refundAfter": 12}); s != 200 {
			t.Fatal(r)
		}
		s, it := c.do("POST", "/api/inventory/items", "EMP-1002", "HR Admin", map[string]any{"kind": "uniform", "name": "Cap", "price": 150})
		if s != 200 {
			t.Fatal(it)
		}
		cap := it["id"].(float64)
		if s, _ := c.do("POST", "/api/inventory/items", "EMP-1002", "HR Admin", map[string]any{"kind": "uniform", "name": "cap", "price": 150}); s != 400 {
			t.Fatal("duplicate item")
		}
		if s, _ := c.do("POST", "/api/inventory/items", "EMP-1002", "HR Admin", map[string]any{"kind": "uniform", "name": "Belt"}); s != 400 {
			t.Fatal("a uniform item needs a price")
		}
		s, pr := c.do("POST", "/api/inventory/items", "EMP-1002", "HR Admin", map[string]any{"kind": "asset", "name": "Printer"})
		if s != 200 {
			t.Fatal(pr)
		}
		if s, r := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", cap), "EMP-1002", "HR Admin", map[string]any{"price": 175}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", pr["id"]), "EMP-1002", "HR Admin", map[string]any{"kind": "uniform"}); s != 400 {
			t.Fatal("a uniform item needs a price")
		}
		if s, r := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", pr["id"]), "EMP-1002", "HR Admin", map[string]any{"kind": "uniform", "price": 300}); s != 200 {
			t.Fatal("the kind changes while nothing is stocked", r)
		}
		s, k := c.do("POST", "/api/inventory/kits", "EMP-1002", "HR Admin", map[string]any{"name": "Kitchen kit", "lines": map[string]int{fmt.Sprint(shirt): 2, fmt.Sprint(cap): 1}})
		if s != 200 {
			t.Fatal(k)
		}
		if s, _ := c.do("POST", "/api/inventory/kits", "EMP-1002", "HR Admin", map[string]any{"name": "Bad kit", "lines": map[string]int{fmt.Sprint(laptop): 1}}); s != 400 {
			t.Fatal("a kit holds uniform items only")
		}
		if s, r := c.do("PUT", fmt.Sprintf("/api/inventory/kits/%v", k["id"]), "EMP-1002", "HR Admin", map[string]any{"name": "Kitchen kit", "lines": map[string]int{fmt.Sprint(shirt): 3, fmt.Sprint(cap): 0}}); s != 200 {
			t.Fatal(r)
		}
		_, kits := c.do("GET", "/api/inventory/kits", "EMP-1004", "HRBP", nil)
		found := false
		for _, x := range list(kits) {
			m := x.(map[string]any)
			if m["name"] == "Kitchen kit" {
				ls := m["lines"].(map[string]any)
				found = ls[fmt.Sprint(shirt)] == 3.0 && ls[fmt.Sprint(cap)] == nil
			}
		}
		if !found {
			t.Fatal(kits)
		}
		if s, r := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", cap), "EMP-1002", "HR Admin", map[string]any{"status": "Inactive", "reason": "No longer used"}); s != 200 {
			t.Fatal(r)
		}
		_, d = c.do("GET", "/api/inventory/setup", "EMP-1002", "HR Admin", nil)
		for _, x := range d["items"].([]any) {
			if m := x.(map[string]any); m["id"] == cap && (m["status"] != "Inactive" || m["price"] != 175.0) {
				t.Fatal(m)
			}
		}
	})

	t.Run("dated stock with asset names", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/inventory/stock/add", "EMP-1002", "HR Admin", map[string]any{"to": "central", "date": day(1), "lines": []any{line(shirt, 1)}}); s != 400 {
			t.Fatal("a future date is refused")
		}
		if s, r := c.do("POST", "/api/inventory/stock/add", "EMP-1002", "HR Admin", map[string]any{"to": "central", "date": day(-3), "note": "INV-1", "lines": []any{
			line(shirt, 20), line(trousers, 10), map[string]any{"itemId": laptop, "serials": []string{"lpx-1", "LPX-2"}, "name": "Dell Latitude 5440"}}}); s != 200 {
			t.Fatal(r)
		}
		_, inv := c.do("GET", "/api/inventory?store=central", "EMP-1002", "HR Admin", nil)
		named := 0
		for _, x := range inv["assets"].([]any) {
			if m := x.(map[string]any); m["name"] == "Dell Latitude 5440" {
				named++
			}
		}
		if named != 2 {
			t.Fatalf("asset names: %v", inv["assets"])
		}
		for _, x := range inv["moves"].([]any) {
			if m := x.(map[string]any); m["kind"] == "Addition" && m["date"] != day(-3) {
				t.Fatalf("addition dated %v", m["date"])
			}
		}
		if s, r := c.do("POST", "/api/inventory/stock/transfer", "EMP-1003", "HRBP Corporate", map[string]any{"from": "central", "to": hrbp, "date": day(-1), "lines": []any{
			line(shirt, 8), line(trousers, 4), map[string]any{"itemId": laptop, "serials": []string{"LPX-1"}}}}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", shirt), "EMP-1002", "HR Admin", map[string]any{"kind": "asset"}); s != 400 {
			t.Fatal("an item with stock keeps its kind")
		}
	})

	var uni string
	t.Run("issue from the employee's store with the configured recovery", func(t *testing.T) {
		if s, _ := c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": day(-2), "lines": []any{line(shirt, 9)}}); s != 400 {
			t.Fatal("the HRBP store holds only 8 shirts")
		}
		s, u := c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": day(-2), "lines": []any{line(shirt, 2), line(trousers, 1), line(shirt, 1)}})
		if s != 200 || u["store"] != hrbp || u["amount"] != 3*450.0+550 {
			t.Fatal(u)
		}
		uni = u["id"].(string)
		_, open := c.do("GET", "/api/pay-items/deductions", "EMP-1011", "Payroll", nil)
		first := open["openYm"].(string)
		if m := day(-2)[:7]; m > first {
			first = m
		}
		_, l := c.do("GET", "/api/inventory/uniform-issues", "EMP-1004", "HRBP", nil)
		var got map[string]any
		for _, x := range list(l) {
			if m := x.(map[string]any); m["id"] == uni {
				got = m
			}
		}
		if got == nil || got["date"] != day(-2) || got["total"] != 1900.0 || got["employeeId"] != "EMP-1020" {
			t.Fatal(l)
		}
		sch := got["schedule"].([]any)
		if len(sch) != 1 || sch[0].(map[string]any)["ym"] != first || sch[0].(map[string]any)["amount"] != 1900.0 || got["refundYm"] != ymAdd(first, 12) {
			t.Fatalf("100%% in %s, refund 12 months later: %v %v", first, sch, got["refundYm"])
		}
		if len(got["lines"].([]any)) != 2 {
			t.Fatal("the same item twice is one line")
		}
		_, inv := c.do("GET", "/api/inventory?store="+hrbp, "EMP-1004", "HRBP", nil)
		refs := 0
		for _, x := range inv["moves"].([]any) {
			if m := x.(map[string]any); m["kind"] == "Issue" && m["ref"] == uni {
				refs++
			}
		}
		if refs != 2 {
			t.Fatalf("issue movements carry the uniform issue id: %d", refs)
		}
		// The HRBP issues from their own store; an employee outside every HRBP's area from central.
		if s, u := c.do("POST", "/api/inventory/issue", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "lines": []any{line(shirt, 1)}}); s != 200 || u["store"] != hrbp {
			t.Fatal(u)
		}
		if s, u := c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1018", "lines": []any{line(shirt, 1)}}); s != 200 || u["store"] != "central" {
			t.Fatal(u)
		}
		if s, _ := c.do("GET", "/api/inventory/uniform-issues", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees do not list uniform issues")
		}
		aid := ui2InvAssetID(st, "LPX-1")
		if s, r := c.do("POST", "/api/inventory/issue", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "assetId": aid, "date": day(-1)}); s != 200 || r["store"] != hrbp {
			t.Fatal(r)
		}
		issued, _ := apiDoc(st, "asset", bson.M{"_id": aid})["issued_at"].(string)
		if issued != day(-1) {
			t.Fatalf("issued on %s", issued)
		}
	})

	t.Run("partial uniform return and a damaged asset", func(t *testing.T) {
		_, held := c.do("GET", "/api/inventory/employee/EMP-1020", "EMP-1004", "HRBP", nil)
		if held["store"] != hrbp || len(held["assets"].([]any)) != 1 || held["assets"].([]any)[0].(map[string]any)["name"] != "Dell Latitude 5440" {
			t.Fatal(held)
		}
		if s, _ := c.do("POST", "/api/inventory/return", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "lines": []any{line(shirt, 4)}}); s != 400 {
			t.Fatal("only 3 shirts are held")
		}
		if s, r := c.do("POST", "/api/inventory/return", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "lines": []any{line(shirt, 2)}, "note": "Uniform returned"}); s != 200 || r["units"] != 2.0 {
			t.Fatal(r)
		}
		u := apiDoc(st, "uniform_issue", bson.M{"_id": uni})
		returned, _ := u["returned"].(bool)
		lines, _ := u["lines"].([]any)
		retTwo := false // a line with 2 back
		for _, l := range lines {
			if m, _ := l.(map[string]any); m["ret"] == 2.0 {
				retTwo = true
			}
		}
		if returned || !retTwo {
			t.Fatalf("partly returned: %v %v", returned, lines)
		}
		if s, r := c.do("POST", "/api/inventory/return", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "lines": []any{line(shirt, 1), line(trousers, 1)}}); s != 200 {
			t.Fatal(r)
		}
		returned, _ = apiDoc(st, "uniform_issue", bson.M{"_id": uni})["returned"].(bool)
		if !returned {
			t.Fatal("everything back: the issue is returned")
		}
		n := apiCount(st, "ledger", bson.M{"source_id": uni, "void": false})
		if n != 2 {
			t.Fatalf("a return does not reverse the recovery and refund: %d lines", n)
		}
		aid := ui2InvAssetID(st, "LPX-1")
		if s, _ := c.do("POST", "/api/inventory/return", "EMP-1004", "HRBP", map[string]any{"assetId": aid, "recover": 500}); s != 400 {
			t.Fatal("a recovery is for a damaged or lost asset")
		}
		if s, r := c.do("POST", "/api/inventory/return", "EMP-1004", "HRBP", map[string]any{"assetId": aid, "damaged": true, "recover": 2500, "note": "Screen cracked"}); s != 200 || r["store"] != hrbp {
			t.Fatal(r)
		}
		ded := apiDoc(st, "ledger", bson.M{"source_id": fmt.Sprintf("AST-%d", aid), "kind": "ded"})
		amt := apiFloat(ded["amount"])
		label, _ := ded["label"].(string)
		if amt != 2500 || !strings.Contains(label, "Dell Latitude 5440") {
			t.Fatalf("damage recovery %v %q", amt, label)
		}
		status, _ := apiDoc(st, "asset", bson.M{"_id": aid})["status"].(string)
		wo := apiCount(st, "inv_move", bson.M{"kind": "Write-off", "serials": "LPX-1"})
		if status != "Damaged" || wo != 1 {
			t.Fatalf("damaged and written off: %s %d", status, wo)
		}
	})

	t.Run("counted write-off", func(t *testing.T) {
		have := func() float64 {
			_, inv := c.do("GET", "/api/inventory?store="+hrbp, "EMP-1004", "HRBP", nil)
			for _, x := range inv["summary"].([]any) {
				if m := x.(map[string]any); m["id"] == trousers {
					return m["closing"].(float64)
				}
			}
			return -1
		}
		before := have()
		if s, _ := c.do("POST", "/api/inventory/stock/writeoff", "EMP-1004", "HRBP", map[string]any{"store": hrbp, "lines": []any{line(trousers, 1)}, "note": "x"}); s != 400 {
			t.Fatal("a reason is needed")
		}
		if s, _ := c.do("POST", "/api/inventory/stock/writeoff", "EMP-1004", "HRBP", map[string]any{"store": hrbp, "lines": []any{line(trousers, 99)}, "note": "Torn"}); s != 400 {
			t.Fatal("more than the store holds")
		}
		if s, _ := c.do("POST", "/api/inventory/stock/writeoff", "EMP-1004", "HRBP", map[string]any{"store": "central", "lines": []any{line(trousers, 1)}, "note": "Torn"}); s != 403 {
			t.Fatal("an HRBP writes off only in their own store")
		}
		if s, _ := c.do("POST", "/api/inventory/stock/writeoff", "EMP-1011", "Payroll", map[string]any{"store": hrbp, "lines": []any{line(trousers, 1)}, "note": "Torn"}); s != 403 {
			t.Fatal("Payroll does not write off stock")
		}
		if s, r := c.do("POST", "/api/inventory/stock/writeoff", "EMP-1004", "HRBP", map[string]any{"store": hrbp, "lines": []any{line(trousers, 2)}, "note": "Torn in the wash"}); s != 200 || r["units"] != 2.0 {
			t.Fatal(r)
		}
		if after := have(); after != before-2 {
			t.Fatalf("closing %v → %v", before, after)
		}
		if s, r := c.do("POST", "/api/inventory/stock/writeoff", "EMP-1002", "HR Admin", map[string]any{"store": "central", "lines": []any{line(laptop, 1)}, "note": "Water damage"}); s != 200 {
			t.Fatal(r)
		}
		status, _ := apiDoc(st, "asset", bson.M{"serial": "LPX-2"})["status"].(string)
		if status != "Damaged" {
			t.Fatal("the available laptop is written off")
		}
	})
}

// Round 2, salary holds and deductions: people absent without notice, waiving a deduction's
// balance, the raiser's name, proof uploaded by Payroll, and the payslip contact and ID switch.
func TestUI2Dues(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1020")
	list := func(m map[string]any) []any { l, _ := m["list"].([]any); return l }

	t.Run("not at work without notice", func(t *testing.T) {
		yday := time.Now().AddDate(0, 0, -1)
		for i := 0; i < 15; i++ {
			punchDay(t, st, "EMP-1016", yday.AddDate(0, 0, -i).Format("2006-01-02"), "09:00", "18:00")
		}
		if _, err := st.DB.C("salary_hold").Insert(ctx, bson.M{"_id": "HLD-900", "employee_id": "EMP-1022", "reason": "Absent without notice", "note": "test hold",
			"from_ym": store.Today()[:7], "by_name": "test"}); err != nil {
			t.Fatal(err)
		}
		s, d := c.do("GET", "/api/holds/absent?days=10", "EMP-1011", "Payroll", nil)
		if s != 200 {
			t.Fatal(d)
		}
		seen := map[string]float64{}
		for _, x := range list(d) {
			m := x.(map[string]any)
			seen[m["employeeId"].(string)] = m["days"].(float64)
			if m["name"] == nil || m["department"] == nil || m["location"] == nil {
				t.Fatal(m)
			}
		}
		if seen["EMP-1018"] < 5 || seen["EMP-1018"] > 10 {
			t.Fatalf("no punches: listed with 5 to 10 days, got %v", seen["EMP-1018"])
		}
		if _, ok := seen["EMP-1016"]; ok {
			t.Fatal("someone punching every day is not listed")
		}
		if _, ok := seen["EMP-1022"]; ok {
			t.Fatal("someone already on hold is not listed")
		}
		if s, _ := c.do("GET", "/api/holds/absent?days=40", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("window too long")
		}
		if s, _ := c.do("GET", "/api/holds/absent", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees do not see this")
		}
	})

	var ded string
	t.Run("deduction with proof, waive the balance", func(t *testing.T) {
		_, meta := c.do("GET", "/api/pay-items/deductions", "EMP-1011", "Payroll", nil)
		types := meta["dedTypes"].([]any)
		if len(types) == 0 {
			t.Fatal(meta)
		}
		if s, _ := c.upload("EMP-1011", "Payroll", "employee", "EMP-1020", "Deduction proof"); s != 403 {
			t.Fatal("Payroll does not write to the employee's documents")
		}
		if s, _ := c.upload("EMP-1011", "Payroll", "approval", "EMP-1011/ded", "Deduction proof"); s != 403 {
			t.Fatal("no proof for a deduction from your own salary")
		}
		s, f := c.upload("EMP-1011", "Payroll", "approval", "EMP-1020/ded-1", "Deduction proof")
		if s != 200 {
			t.Fatal(f)
		}
		_, other := c.upload("EMP-1011", "Payroll", "approval", "EMP-1016/ded-2", "Deduction proof")
		body := func(doc any) map[string]any {
			return map[string]any{"wf": "p_ded", "employeeId": "EMP-1020", "payload": map[string]any{"dedType": types[0], "amount": 3000, "instalments": 3, "heard": "Yes",
				"note": "Cash short by 3,000 on 14 Oct, counted by two staff", "doc": doc}}
		}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", body(other["id"])); s != 400 {
			t.Fatal("proof uploaded for another employee")
		}
		s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", body(f["id"]))
		if s != 200 {
			t.Fatal(r)
		}
		ded = r["id"].(string)
		if s, _ := c.getRaw("/api/files/"+f["id"].(string), "EMP-1011", "Payroll"); s != 200 {
			t.Fatal("Payroll opens the proof")
		}
		if s, _ := c.getRaw("/api/files/"+f["id"].(string), "EMP-1020", "Employee"); s != 403 {
			t.Fatal("the proof is not in the employee's own documents")
		}
		if s, _ := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1011", "Payroll", map[string]string{"note": "Not approved yet"}); s != 400 {
			t.Fatal("only an approved deduction is waived")
		}
		c.approveAll(t, ded)
		if s, _ := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1004", "HRBP", map[string]string{"note": "Goodwill"}); s != 403 {
			t.Fatal("an HRBP does not waive deductions")
		}
		if s, _ := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1011", "Payroll", map[string]string{"note": ""}); s != 400 {
			t.Fatal("a reason is needed")
		}
		s, w := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1011", "Payroll", map[string]string{"note": "Cash found in the safe"})
		if s != 200 || w["waived"] != 3000.0 {
			t.Fatal(w)
		}
		if s, _ := c.do("POST", "/api/pay-items/deductions/"+ded+"/waive", "EMP-1011", "Payroll", map[string]string{"note": "Again"}); s != 400 {
			t.Fatal("nothing left to waive")
		}
		voided := apiCount(st, "ledger", bson.M{"source_id": ded, "void": true, "void_reason": bson.M{"$regex": "^Waived by .*Cash found", "$options": "s"}})
		if voided != 3 {
			t.Fatalf("three instalments voided, got %d", voided)
		}
		_, l := c.do("GET", "/api/pay-items/deductions", "EMP-1011", "Payroll", nil)
		for _, x := range l["items"].([]any) {
			if m := x.(map[string]any); m["id"] == ded {
				if m["waived"] != 3000.0 || m["waive_note"] != "Cash found in the safe" || m["waived_by"] == nil || m["raised_by_name"] == nil || m["balance"] != 0.0 {
					t.Fatal(m)
				}
				return
			}
		}
		t.Fatal("deduction not listed")
	})

	t.Run("payslip contact and the ID switch", func(t *testing.T) {
		_, g := c.do("GET", "/api/payslip-setup", "EMP-1011", "Payroll", nil)
		if g["setup"].(map[string]any)["showIds"] != true {
			t.Fatal("the codes show unless turned off")
		}
		put := func(setup map[string]any) (int, map[string]any) {
			return c.do("PUT", "/api/payslip-setup", "EMP-1011", "Payroll", map[string]any{"setup": setup, "cin": "", "pan": "", "tan": "", "pfCode": "", "esiCode": "",
				"cutoff": g["cutoff"], "firstYm": ""})
		}
		if s, _ := put(map[string]any{"accent": "#0f6e6e", "footer": "Computer generated", "email": "payroll@", "showIds": true}); s != 400 {
			t.Fatal("a wrong email is refused")
		}
		if s, r := put(map[string]any{"accent": "#0f6e6e", "footer": "Computer generated", "email": "payroll@saltstayz.com", "phone": "+91 124 400 5000", "showIds": false}); s != 200 {
			t.Fatal(r)
		}
		_, g = c.do("GET", "/api/payslip-setup", "EMP-1011", "Payroll", nil)
		x := g["setup"].(map[string]any)
		if x["showIds"] != false || x["email"] != "payroll@saltstayz.com" || x["phone"] != "+91 124 400 5000" {
			t.Fatal(x)
		}
	})
}
