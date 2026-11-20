package api_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// uniOpenYm is the open payroll month.
func uniOpenYm(c *client) string {
	_, d := c.do("GET", "/api/pay-items/deductions", "EMP-1011", "Payroll", nil)
	return d["openYm"].(string)
}

// uniRefund is the uniform refund pay line of an issue (void or not), or nil.
func uniRefund(st *store.Store, id string) store.Row {
	return apiDoc(st, "ledger", bson.M{"source_id": id, "kind": "earn"})
}

// uniServiceDone is the month someone who joined on doj completes n months (as the refund rule counts it).
func uniServiceDone(doj string, n int) string {
	if doj[8:] == "01" {
		return ymAdd(doj[:7], n-1)
	}
	return ymAdd(doj[:7], n)
}

func uniMax(a, b string) string {
	if a > b {
		return a
	}
	return b
}

// Uniform sizes (owner's request of 13 Nov 2026): the size lists, an employee's sizes with who may change them,
// issuing by size, the sizes summary, both refund rules, a leaver's refund (cancelled, also by the full and final
// and by the daily sweep, and put back when the leaving is undone), the bulk template, the import and the download.
func TestUniformSizes(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1016", "EMP-1020")
	item := func(name string) float64 { return apiFloat(apiDoc(st, "inv_item", bson.M{"name": name})["id"]) }
	shirt, trousers, shoes := item("Shirt"), item("Trousers"), item("Shoes")
	open := uniOpenYm(c)
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

	t.Run("setup: defaults, size lists and item size types", func(t *testing.T) {
		_, d := c.do("GET", "/api/inventory/setup", "EMP-1002", "HR Admin", nil)
		if d["refundBasis"] != "deduction" || d["serviceMonths"] != 6.0 || d["rule"] != "6 months after the last deduction" {
			t.Fatalf("defaults: %v %v %v", d["refundBasis"], d["serviceMonths"], d["rule"])
		}
		sz := d["sizes"].(map[string]any)
		if fmt.Sprint(sz["shirt"]) != "[XS S M L XL XXL 3XL 4XL]" || fmt.Sprint(sz["waist"]) != "[26 28 30 32 34 36 38 40 42 44]" ||
			fmt.Sprint(sz["length"]) != "[28 29 30 31 32 33 34 35 36]" || fmt.Sprint(sz["shoe"]) != "[4 5 6 7 8 9 10 11 12]" {
			t.Fatalf("size lists: %v", sz)
		}
		types := map[string]any{}
		for _, x := range d["items"].([]any) {
			m := x.(map[string]any)
			types[m["name"].(string)] = m["size_type"]
		}
		if types["Shirt"] != "shirt" || types["Trousers"] != "trouser" || types["Shoes"] != "shoes" || types["Apron"] != nil {
			t.Fatalf("size types: %v", types)
		}
		if s, _ := c.do("PUT", "/api/inventory/size-lists", "EMP-1004", "HRBP", map[string]any{"shirt": []string{"S"}}); s != 403 {
			t.Fatal("HR Admin keeps the size lists")
		}
		for _, bad := range []map[string]any{{"shirt": []string{"S", "s"}}, {"shirt": []string{}}, {"shoe": []string{"8,9"}}, {"hat": []string{"S"}}} {
			if s, _ := c.do("PUT", "/api/inventory/size-lists", "EMP-1002", "HR Admin", bad); s != 400 {
				t.Fatalf("refused: %v", bad)
			}
		}
		withXL := []string{"XS", "S", "M", "L", "XL", "XXL", "3XL", "4XL", "5XL"}
		if s, r := c.do("PUT", "/api/inventory/size-lists", "EMP-1002", "HR Admin", map[string]any{"shirt": withXL}); s != 200 || fmt.Sprint(r["waist"]) != "[26 28 30 32 34 36 38 40 42 44]" {
			t.Fatal(s, r)
		}
		if o := apiDoc(st, "onb_field", bson.M{"key": "uniShirt"})["options"]; o != strings.Join(withXL, ",") {
			t.Fatalf("the joining form offers the new list: %v", o)
		}
		_, su := c.do("GET", "/api/onboarding/setup", "EMP-1002", "HR Admin", nil)
		found := 0
		for _, x := range su["fields"].([]any) {
			if f := x.(map[string]any); f["section"] == "Uniform" {
				found++
				if f["filledBy"] != "joiner" || f["join"] != 1.0 || f["offer"] != 0.0 {
					t.Fatalf("uniform field %v", f)
				}
			}
		}
		if found != 4 {
			t.Fatalf("4 uniform fields in Form setup, got %d", found)
		}
		if s, _ := c.do("PATCH", "/api/onboarding/setup/fields/uniWaist", "EMP-1002", "HR Admin", map[string]any{"filledBy": "hr"}); s != 200 {
			t.Fatal("switch to HR")
		}
		if s, _ := c.do("PATCH", "/api/onboarding/setup/fields/uniWaist", "EMP-1002", "HR Admin", map[string]any{"filledBy": "joiner"}); s != 200 {
			t.Fatal("and back to the joiner")
		}
		// An item's size type: set, checked, and guessed from the name for a new item.
		apron := item("Apron")
		if s, _ := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", apron), "EMP-1002", "HR Admin", map[string]any{"sizeType": "hat"}); s != 400 {
			t.Fatal("unknown size type")
		}
		if s, r := c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", apron), "EMP-1002", "HR Admin", map[string]any{"sizeType": "shirt"}); s != 200 {
			t.Fatal(r)
		}
		if v := apiDoc(st, "inv_item", bson.M{"_id": int32(apron)})["size_type"]; v != "shirt" {
			t.Fatalf("apron: %v", v)
		}
		c.do("PATCH", fmt.Sprintf("/api/inventory/items/%v", apron), "EMP-1002", "HR Admin", map[string]any{"sizeType": "other"})
		s, r := c.do("POST", "/api/inventory/items", "EMP-1002", "HR Admin", map[string]any{"kind": "uniform", "name": "Polo T-shirt", "price": 300})
		if s != 200 || apiDoc(st, "inv_item", bson.M{"name": "Polo T-shirt"})["size_type"] != "shirt" {
			t.Fatal("a new T-shirt is issued by shirt size", r)
		}
	})

	t.Run("an employee's sizes: validation, rights, history", func(t *testing.T) {
		s, d := c.do("GET", "/api/employees/EMP-1020/uniform", "EMP-1020", "Employee", nil)
		if s != 200 || d["canEdit"] != false || d["sizes"].(map[string]any)["shirt"] != "" {
			t.Fatal(s, d)
		}
		if s, _ := c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1020", "Employee", map[string]any{"shirt": "M"}); s != 403 {
			t.Fatal("an employee asks HR")
		}
		if s, _ := c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1009", "Line Manager", map[string]any{"shirt": "M"}); s != 403 {
			t.Fatal("a line manager only reads")
		}
		if s, r := c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1004", "HRBP", map[string]any{"shirt": "XXXL", "waist": "33", "shoe": "13"}); s != 400 || len(r["details"].([]any)) != 3 {
			t.Fatal("sizes off the lists", r)
		}
		if s, r := c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1004", "HRBP", map[string]any{"shirt": "m", "waist": "32", "length": "30", "shoe": "8"}); s != 200 || len(r["changes"].([]any)) != 4 {
			t.Fatal(s, r)
		}
		u := apiDoc(st, store.UniformSizeColl, bson.M{"employee_id": "EMP-1020"})
		if u["shirt"] != "M" || u["waist"] != "32" || u["length"] != "30" || u["shoe"] != "8" || !strings.Contains(fmt.Sprint(u["updated_by"]), "HRBP") {
			t.Fatalf("stored as listed: %v", u)
		}
		if n := apiCount(st, "employee_change", bson.M{"employee_id": "EMP-1020", "reason": "Uniform sizes"}); n != 4 {
			t.Fatalf("change history: %d", n)
		}
		if n := apiCount(st, "audit_log", bson.M{"action": "Uniform sizes changed", "entity_id": "EMP-1020"}); n != 1 {
			t.Fatalf("audit: %d", n)
		}
		// Payroll manages inventory (dues at Edit), so it may change sizes too; nothing changed is no change.
		if s, r := c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1011", "Payroll", map[string]any{"shirt": "M"}); s != 200 || len(r["changes"].([]any)) != 0 {
			t.Fatal(s, r)
		}
		// A size taken off its list stays on the record until it is changed.
		c.do("PUT", "/api/inventory/size-lists", "EMP-1002", "HR Admin", map[string]any{"shoe": []string{"6", "7", "9", "10"}})
		if s, r := c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1004", "HRBP", map[string]any{"shoe": "8", "shirt": "L"}); s != 200 || len(r["changes"].([]any)) != 1 {
			t.Fatal("an unlisted size kept as it is", s, r)
		}
		c.do("PUT", "/api/inventory/size-lists", "EMP-1002", "HR Admin", map[string]any{"shoe": []string{"4", "5", "6", "7", "8", "9", "10", "11", "12"}})
		c.do("PUT", "/api/employees/EMP-1020/uniform", "EMP-1004", "HRBP", map[string]any{"shirt": "M"})
		_, d = c.do("GET", "/api/employees/EMP-1020/uniform", "EMP-1004", "HRBP", nil)
		if d["canEdit"] != true || d["sizes"].(map[string]any)["shirt"] != "M" || d["updated"] == nil {
			t.Fatal(d)
		}
	})

	var uni string
	t.Run("issue by size", func(t *testing.T) {
		if s, r := c.do("POST", "/api/inventory/stock/add", "EMP-1002", "HR Admin", map[string]any{"to": "central", "lines": []any{
			map[string]any{"itemId": shirt, "qty": 30}, map[string]any{"itemId": trousers, "qty": 10}, map[string]any{"itemId": shoes, "qty": 10}}}); s != 200 {
			t.Fatal(r)
		}
		issue := func(emp string, lines ...map[string]any) (int, map[string]any) {
			return c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"store": "central", "employeeId": emp, "lines": lines})
		}
		ln := func(id float64, q int, size string) map[string]any {
			return map[string]any{"itemId": id, "qty": q, "size": size}
		}
		for _, bad := range []map[string]any{ln(shirt, 1, "ZZ"), ln(trousers, 1, "33/30"), ln(trousers, 1, "32/40"), ln(shoes, 1, "13")} {
			if s, _ := issue("EMP-1020", bad); s != 400 {
				t.Fatalf("size refused: %v", bad)
			}
		}
		s, u := issue("EMP-1020", ln(shirt, 2, "m"), ln(shirt, 1, "L"), ln(trousers, 1, "32 / 30"), ln(shoes, 1, "8"), ln(shirt, 1, "M"))
		if s != 200 || u["amount"] != 4*450.0+550+900 {
			t.Fatal(s, u)
		}
		uni = u["id"].(string)
		got := map[string]float64{}
		for _, x := range apiDoc(st, "uniform_issue", bson.M{"_id": uni})["lines"].([]any) {
			m := x.(map[string]any)
			got[fmt.Sprint(m["name"], " ", m["size"])] = apiFloat(m["qty"])
		}
		if len(got) != 4 || got["Shirt M"] != 3 || got["Shirt L"] != 1 || got["Trousers 32/30"] != 1 || got["Shoes 8"] != 1 {
			t.Fatalf("one line per item and size: %v", got)
		}
		if r := apiDoc(st, "uniform_issue", bson.M{"_id": uni})["refund_rule"]; r != "6 months after the last deduction" {
			t.Fatalf("refund rule %v", r)
		}
		first := uniMax(today()[:7], open)
		if ref := uniRefund(st, uni); ref["ym"] != ymAdd(first, 7) || ref["label"] != "Uniform deposit refund" {
			t.Fatalf("refund 6 months after the second instalment: %v (first %s)", ref["ym"], first)
		}
		_, l := c.do("GET", "/api/inventory/uniform-issues?employeeId=EMP-1020", "EMP-1004", "HRBP", nil)
		var row map[string]any
		for _, x := range l["list"].([]any) {
			if m := x.(map[string]any); m["id"] == uni {
				row = m
			}
		}
		if row == nil || row["refundStatus"] != "scheduled" || row["refundYm"] != ymAdd(first, 7) || row["toRecover"] != 3250.0 || len(row["schedule"].([]any)) != 2 {
			t.Fatalf("issue row: %v", row)
		}
		// My view shows the employee their recovery and the expected refund.
		_, my := c.do("GET", "/api/me/dashboard?tab=pay", "EMP-1020", "Employee", nil)
		mu := my["uniform"].(map[string]any)
		if mu["sizes"].(map[string]any)["waist"] != "32" || len(mu["issues"].([]any)) < 1 || mu["issues"].([]any)[0].(map[string]any)["refundYm"] != ymAdd(first, 7) {
			t.Fatalf("my uniform: %v", mu)
		}
		_, eu := c.do("GET", "/api/employees/EMP-1020/uniform", "EMP-1020", "Employee", nil)
		if len(eu["issues"].([]any)) < 1 || eu["rule"] != "6 months after the last deduction" {
			t.Fatal(eu)
		}
	})

	t.Run("sizes summary", func(t *testing.T) {
		if s, _ := c.do("GET", "/api/inventory/sizes", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("dues rights")
		}
		c.do("PUT", "/api/employees/EMP-1016/uniform", "EMP-1002", "HR Admin", map[string]any{"shirt": "M", "waist": "34"})
		_, d := c.do("GET", "/api/inventory/sizes", "EMP-1002", "HR Admin", nil)
		active := apiCount(st, "employee", bson.M{"status": "Active"})
		if int(apiFloat(d["total"])) != active || d["recorded"] != 2.0 || len(d["missing"].([]any)) != active-2 {
			t.Fatalf("totals: %v of %d, recorded %v, missing %d", d["total"], active, d["recorded"], len(d["missing"].([]any)))
		}
		count := map[string]float64{}
		none := map[string]float64{}
		for _, g := range d["groups"].([]any) {
			gm := g.(map[string]any)
			none[gm["key"].(string)] = apiFloat(gm["notRecorded"])
			for _, x := range gm["rows"].([]any) {
				m := x.(map[string]any)
				count[gm["key"].(string)+":"+m["size"].(string)] = apiFloat(m["count"])
			}
		}
		if count["shirt:M"] != 2 || count["waist:32"] != 1 || count["waist:34"] != 1 || count["length:30"] != 1 || count["shoe:8"] != 1 || count["shirt:XS"] != 0 {
			t.Fatalf("counts: %v", count)
		}
		if none["length"] != float64(active-1) {
			t.Fatalf("not recorded: %v", none)
		}
		pairs := fmt.Sprint(d["trousers"])
		if !strings.Contains(pairs, "length:30") || !strings.Contains(pairs, "length:—") {
			t.Fatalf("trousers by waist and length: %s", pairs)
		}
		for _, m := range d["missing"].([]any) {
			if id := m.(map[string]any)["id"]; id == "EMP-1020" || id == "EMP-1016" {
				t.Fatal("people with sizes are not missing")
			}
		}
		e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
		dept := fmt.Sprint(apiDoc(st, "department", bson.M{"_id": e["department_id"]})["name"])
		_, f := c.do("GET", "/api/inventory/sizes?loc="+url.QueryEscape(fmt.Sprint(e["location_id"]))+"&dept="+url.QueryEscape(dept), "EMP-1002", "HR Admin", nil)
		if apiFloat(f["total"]) < 1 || apiFloat(f["total"]) >= apiFloat(d["total"]) {
			t.Fatalf("narrowed: %v of %v", f["total"], d["total"])
		}
		s, b := c.getRaw("/api/inventory/sizes/sheet", "EMP-1002", "HR Admin")
		if s != 200 || !bytes.HasPrefix(b, []byte("PK")) {
			t.Fatalf("sheet: %d", s)
		}
		if s, _ := c.getRaw("/api/inventory/sizes/sheet", "EMP-1020", "Employee"); s != 403 {
			t.Fatal("sheet rights")
		}
	})

	t.Run("refund after months of employment", func(t *testing.T) {
		for _, bad := range []map[string]any{{"firstPct": 50, "refundAfter": 6, "refundBasis": "joining"}, {"firstPct": 50, "refundAfter": 6, "refundBasis": "service", "serviceMonths": 61}} {
			if s, _ := c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", bad); s != 400 {
				t.Fatalf("refused: %v", bad)
			}
		}
		before := uniRefund(st, uni)["ym"]
		if s, r := c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 50, "refundAfter": 6, "refundBasis": "service", "serviceMonths": 24}); s != 200 {
			t.Fatal(r)
		}
		// A caller that does not know the basis keeps it.
		c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 50, "refundAfter": 6})
		_, d := c.do("GET", "/api/inventory/setup", "EMP-1002", "HR Admin", nil)
		if d["refundBasis"] != "service" || d["serviceMonths"] != 24.0 || d["rule"] != "after 24 months of employment" || fmt.Sprint(d["sizes"].(map[string]any)["shirt"]) != "[XS S M L XL XXL 3XL 4XL 5XL]" {
			t.Fatalf("setup kept: %v %v %v %v", d["refundBasis"], d["serviceMonths"], d["rule"], d["sizes"])
		}
		if uniRefund(st, uni)["ym"] != before {
			t.Fatal("an issue made before keeps its refund month")
		}
		// A recent joiner: refunded the month they complete 24 months.
		doj := day(-20)
		if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"doj": doj}}); err != nil {
			t.Fatal(err)
		}
		s, u := c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"store": "central", "employeeId": "EMP-1016", "lines": []any{map[string]any{"itemId": shirt, "qty": 2, "size": "M"}}})
		if s != 200 {
			t.Fatal(u)
		}
		first := uniMax(today()[:7], open)
		want := uniMax(uniServiceDone(doj, 24), ymAdd(first, 2))
		if ref := uniRefund(st, u["id"].(string)); ref["ym"] != want || u["refundYm"] != want {
			t.Fatalf("refund when 24 months are complete: %v (want %s, joined %s)", ref["ym"], want, doj)
		}
		if r := apiDoc(st, "uniform_issue", bson.M{"_id": u["id"]})["refund_rule"]; r != "after 24 months of employment" {
			t.Fatal(r)
		}
		// Someone who completed the months long ago: the month after the last deduction.
		c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 50, "refundAfter": 6, "refundBasis": "service", "serviceMonths": 1})
		_, u2 := c.do("POST", "/api/inventory/issue", "EMP-1002", "HR Admin", map[string]any{"store": "central", "employeeId": "EMP-1020", "lines": []any{map[string]any{"itemId": shirt, "qty": 1, "size": "M"}}})
		if ref := uniRefund(st, u2["id"].(string)); ref["ym"] != ymAdd(first, 2) {
			t.Fatalf("the month after the last deduction: %v", ref["ym"])
		}
		c.do("PUT", "/api/inventory/setup", "EMP-1002", "HR Admin", map[string]any{"firstPct": 50, "refundAfter": 6, "refundBasis": "deduction"})
		if _, d := c.do("GET", "/api/inventory/setup", "EMP-1002", "HR Admin", nil); d["rule"] != "6 months after the last deduction" {
			t.Fatal(d["rule"])
		}
	})

	t.Run("a leaver gets no refund; the recovery stands", func(t *testing.T) {
		id := apiDoc(st, "uniform_issue", bson.M{"employee_id": "EMP-1016"})["id"].(string)
		deds := apiCount(st, "ledger", bson.M{"source_id": id, "kind": "ded", "void": false})
		if s, r := c.do("POST", "/api/employees/EMP-1016/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": today(), "reason": "Resigned"}); s != 200 {
			t.Fatal(r)
		}
		ref := uniRefund(st, id)
		if ref["void"] != true || !strings.HasPrefix(fmt.Sprint(ref["void_reason"]), "No uniform refund: left before the refund month") {
			t.Fatalf("refund cancelled: %v", ref)
		}
		if n := apiCount(st, "ledger", bson.M{"source_id": id, "kind": "ded", "void": false}); n != deds {
			t.Fatal("the recovery stands")
		}
		_, l := c.do("GET", "/api/inventory/uniform-issues?employeeId=EMP-1016", "EMP-1002", "HR Admin", nil)
		if row := l["list"].([]any)[0].(map[string]any); row["refundStatus"] != "cancelled" {
			t.Fatal(row)
		}
		_, pv := c.do("GET", "/api/fnf/EMP-1016", "EMP-1011", "Payroll", nil)
		var info map[string]any
		for _, x := range pv["lines"].([]any) {
			m := x.(map[string]any)
			if m["key"] == "unirefund" {
				info = m
			}
			if m["key"] == "earn2" {
				t.Fatalf("the refund is not brought forward: %v", m)
			}
		}
		if info == nil || info["kind"] != "info" || info["amount"] != 900.0 || !strings.Contains(fmt.Sprint(info["note"]), "recovery stands") {
			t.Fatalf("F&F shows the refund not paid: %v", pv["lines"])
		}
		// Undo inactive: the refund is due again.
		if s, r := c.do("POST", "/api/employees/EMP-1016/reactivate", "EMP-1002", "HR Admin", nil); s != 200 {
			t.Fatal(r)
		}
		if ref := uniRefund(st, id); ref["void"] != false {
			t.Fatalf("refund back: %v", ref)
		}
		// On notice: the full and final cancels it on approval.
		if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"exit_status": "On notice", "lwd": day(5)}}); err != nil {
			t.Fatal(err)
		}
		_, pv = c.do("GET", "/api/fnf/EMP-1016", "EMP-1011", "Payroll", nil)
		if !strings.Contains(fmt.Sprint(pv["lines"]), "unirefund") {
			t.Fatalf("F&F lines: %v", pv["lines"])
		}
		s, f := c.do("POST", "/api/fnf/EMP-1016", "EMP-1011", "Payroll", nil)
		if s != 200 {
			t.Fatal(f)
		}
		c.approveAll(t, f["id"].(string))
		ref = uniRefund(st, id)
		if ref["void"] != true || !strings.Contains(fmt.Sprint(ref["void_reason"]), "full and final "+f["id"].(string)) {
			t.Fatalf("cancelled by the full and final: %v", ref)
		}
		// The daily sweep: a last working day that passed without the record saying so.
		ids := []string{}
		docs, _ := st.DB.C("uniform_issue").Find(ctx, bson.M{"employee_id": "EMP-1020"})
		for _, x := range docs {
			ids = append(ids, x["id"].(string))
		}
		if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"lwd": day(-1)}}); err != nil {
			t.Fatal(err)
		}
		// A leaver from before the rule started (migration 008 ran today) keeps the refund schedule.
		if _, err := api.ApplyScheduled(ctx, st); err != nil {
			t.Fatal(err)
		}
		if r := uniRefund(st, ids[0]); r["void"] != false {
			t.Fatalf("a leaver from before the rule keeps the refund: %v", r)
		}
		mig := st.DB.Database().Collection(store.MigrationsColl)
		if _, err := mig.UpdateOne(ctx, bson.M{"_id": "008_uniform_sizes"}, bson.M{"$set": bson.M{"applied_at": time.Now().AddDate(0, 0, -30)}}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = mig.UpdateOne(ctx, bson.M{"_id": "008_uniform_sizes"}, bson.M{"$set": bson.M{"applied_at": time.Now()}})
		}()
		if _, err := api.ApplyScheduled(ctx, st); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if r := uniRefund(st, id); r["ym"].(string) > day(-1)[:7] && r["void"] != true {
				t.Fatalf("swept: %v", r)
			}
		}
		if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"}); e["status"] != "Inactive" {
			t.Fatal(e["status"])
		}
	})

	t.Run("bulk upload, import and download", func(t *testing.T) {
		s, b := c.getRaw("/api/bulk/uni/download?data=1", "EMP-1002", "HR Admin")
		if s != 200 || !bytes.HasPrefix(b, []byte("PK")) {
			t.Fatalf("template: %d", s)
		}
		if s, _ := c.getRaw("/api/bulk/uni/download", "EMP-1009", "Line Manager"); s != 403 {
			t.Fatal("template rights")
		}
		file := "Employee ID,Shirt / T-shirt Size,Trouser Waist,Trouser Length,Shoe Size (UK)\nEMP-1022,xl,34,32,9\nEMP-1004,ZZ,,,\nEMP-1046,,,,\n"
		_, chk := c.sendFile("/api/bulk/uni/check", "EMP-1002", "HR Admin", "uni.csv", []byte(file))
		res := fmt.Sprint(chk["results"])
		if chk["ok"] != 1.0 || !strings.Contains(res, "is not on the list") || apiDoc(st, store.UniformSizeColl, bson.M{"employee_id": "EMP-1022"}) != nil {
			t.Fatalf("check: %v", chk)
		}
		if _, ap := c.sendFile("/api/bulk/uni/apply", "EMP-1002", "HR Admin", "uni.csv", []byte(file)); ap["ok"] != 1.0 {
			t.Fatal(ap)
		}
		if u := apiDoc(st, store.UniformSizeColl, bson.M{"employee_id": "EMP-1022"}); u["shirt"] != "XL" || u["waist"] != "34" || u["length"] != "32" || u["shoe"] != "9" {
			t.Fatal(u)
		}
		// Import employees: the size columns are optional; given, they are checked and saved.
		_, list := c.do("GET", "/api/bulk", "EMP-1002", "HR Admin", nil)
		for _, x := range list["list"].([]any) {
			if m := x.(map[string]any); m["key"] == "new" && !strings.Contains(fmt.Sprint(m["optional"]), "Shoe Size (UK)") {
				t.Fatalf("import template: %v", m)
			}
		}
		e := apiDoc(st, "employee", bson.M{"_id": "EMP-1022"})
		name := func(coll string, id any) string { return fmt.Sprint(apiDoc(st, coll, bson.M{"_id": id})["name"]) }
		row := map[string]any{"name": "Uniform Person", "type": "Permanent", "doj": "2026-01-05", "location": name("location", e["location_id"]), "department": name("department", e["department_id"]),
			"designation": name("designation", e["designation_id"]), "grade": e["grade"], "shift": e["shift_code"], "gross": 17000, "mobile": "9876512399", "shirt": "L", "waist": "30", "shoe": "7"}
		bad := map[string]any{}
		for k, v := range row {
			bad[k] = v
		}
		bad["shoe"] = "15"
		if s, r := c.do("POST", "/api/employees/import", "EMP-1002", "HR Admin", map[string]any{"rows": []any{bad}}); s != 200 || r["failed"] != 1.0 || !strings.Contains(fmt.Sprint(r["results"]), "Shoe size (UK) “15” is not on the list") {
			t.Fatal(s, r)
		}
		s, r := c.do("POST", "/api/employees/import", "EMP-1002", "HR Admin", map[string]any{"rows": []any{row}, "apply": true})
		if s != 200 || r["ok"] != 1.0 {
			t.Fatal(s, r)
		}
		code := r["results"].([]any)[0].(map[string]any)["code"]
		if u := apiDoc(st, store.UniformSizeColl, bson.M{"employee_id": code}); u["shirt"] != "L" || u["waist"] != "30" || u["shoe"] != "7" || u["length"] != nil {
			t.Fatalf("imported sizes: %v", u)
		}
		// The employee details download has the group.
		s, b = c.getRaw("/api/employees/export?sections=uniform", "EMP-1002", "HR Admin")
		rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
		if s != 200 || err != nil {
			t.Fatal(s, err)
		}
		if strings.Join(rows[0][len(rows[0])-4:], "|") != "Shirt / T-shirt size|Trouser waist|Trouser length|Shoe size (UK)" {
			t.Fatal(rows[0])
		}
		for _, x := range rows[1:] {
			if x[0] == "EMP-1022" && strings.Join(x[len(x)-4:], "|") != "XL|34|32|9" {
				t.Fatal(x)
			}
		}
	})
}

// A new joiner's sizes: the Uniform section of the joining details, checked against the lists, copied to the
// employee record when the Employee ID is made.
func TestUniformJoining(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009")
	c.do("PUT", "/api/onboarding/settings", "EMP-1002", "HR Admin", map[string]string{"employeeIdAt": "joining"})
	si := selectCandidate(t, c, shortlistBody(st, "Uma", "Size", "9876501288"))
	oi := approvedOffer(t, c, st, si, "Uma", "Size", "9876501288")
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil)
	_, o := c.do("GET", "/api/offers/"+oi, "EMP-1004", "HRBP", nil)
	keys := ""
	for _, x := range o["joinFields"].([]any) {
		if f := x.(map[string]any); f["section"] == "Uniform" {
			keys += f["key"].(string) + " "
		}
	}
	if keys != "uniShirt uniWaist uniLength uniShoe " {
		t.Fatalf("joining asks the sizes: %q", keys)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": map[string]any{"uniShirt": "Huge"}}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Shirt / T-shirt size") {
		t.Fatal(s, r)
	}
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": map[string]any{"uniShirt": "L", "uniWaist": "30", "uniLength": "31", "uniShoe": "7"}}); s != 200 {
		t.Fatal(r)
	}
	want := "JI-0001" // the record is made at joining, on the Joining ID (hiring.go)
	if s, r := c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 || r["id"] != want {
		t.Fatal(s, r)
	}
	u := apiDoc(st, store.UniformSizeColl, bson.M{"employee_id": want})
	if u["shirt"] != "L" || u["waist"] != "30" || u["length"] != "31" || u["shoe"] != "7" {
		t.Fatalf("sizes on the record: %v", u)
	}
}
