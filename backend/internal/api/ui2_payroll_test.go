package api_test

// Round 2 payroll gaps (prototype E1): payslip preview, held sums, stale runs, unchanged rows,
// statutory city filter and file list, deductor details, Form 16 basis and withdrawal, tax
// reminders, export and items, the pay day and structure mapping candidates.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

func sheetRows(t *testing.T, raw []byte) [][]string {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not an Excel file: %v (%s)", err, raw)
	}
	defer f.Close()
	rows, err := f.GetRows(f.GetSheetName(0))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// ui2PayrollCityOf is the city of an employee's location (employee → location → cluster → city), "" when unknown.
func ui2PayrollCityOf(ctx context.Context, st *store.Store, id string) string {
	e, _ := st.DB.C("employee").FindOne(ctx, bson.M{"_id": id})
	if e == nil {
		return ""
	}
	l, _ := st.DB.C("location").FindOne(ctx, bson.M{"_id": e["location_id"]})
	if l == nil {
		return ""
	}
	k, _ := st.DB.C("cluster").FindOne(ctx, bson.M{"_id": l["cluster_id"]})
	if k == nil {
		return ""
	}
	city, _ := k["city_id"].(string)
	return city
}

func findRow(l any, key, id string) map[string]any {
	xs, _ := l.([]any)
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok && m[key] == id {
			return m
		}
	}
	return nil
}

func TestUI2Payroll(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1001", "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012", "EMP-1013", "EMP-1014", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	delhi := ui2PayrollCityOf(ctx, st, "EMP-1016")

	t.Run("pay schedule and pay day", func(t *testing.T) {
		s, r := c.do("GET", "/api/payroll/schedule", "EMP-1011", "Payroll", nil)
		if s != 200 || r["payDay"] != "Last working day" || r["canEdit"] != true || r["cutoff"] != "calendar" {
			t.Fatalf("%d %v", s, r)
		}
		if s, r := c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"payDay": "5th of next month"}); s != 200 {
			t.Fatal(r)
		}
		if _, r := c.do("GET", "/api/payroll/schedule", "EMP-1011", "Payroll", nil); r["payDay"] != "5th of next month" {
			t.Fatal(r)
		}
		if s, _ := c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"payDay": "Every Friday"}); s != 400 {
			t.Fatal("an unknown pay day is refused")
		}
		if s, r := c.do("GET", "/api/payroll/schedule", "EMP-1013", "Finance Head", nil); s != 200 || r["canEdit"] != false {
			t.Fatalf("Finance Head views the schedule: %d %v", s, r)
		}
		if s, _ := c.do("PUT", "/api/payroll/schedule", "EMP-1013", "Finance Head", map[string]any{"payDay": "Last working day"}); s != 403 {
			t.Fatal("the schedule is changed with payroll settings rights only")
		}
		// The cut-off changes on its own, and the payslip setup keeps everything else.
		if s, r := c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"cutoff": "26-25"}); s != 200 {
			t.Fatal(r)
		}
		_, ps := c.do("GET", "/api/payslip-setup", "EMP-1011", "Payroll", nil)
		if ps["cutoff"] != "26-25" || ps["setup"].(map[string]any)["accent"] != "#0f6e6e" {
			t.Fatal(ps)
		}
		c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"cutoff": "calendar"})
	})

	t.Run("deductor details", func(t *testing.T) {
		body := map[string]any{"tan": "dels12345a", "pan": "AAACS1234B", "rp": "Deepak Arora", "rpd": "Payroll Manager"}
		if s, _ := c.do("PUT", "/api/tds/deductor", "EMP-1011", "Payroll", body); s != 403 {
			t.Fatal("deductor details are saved by a payroll approver")
		}
		if s, r := c.do("PUT", "/api/tds/deductor", "EMP-1012", "Payroll Manager", body); s != 200 {
			t.Fatal(r)
		}
		_, tds := c.do("GET", "/api/tds", "EMP-1012", "Payroll Manager", nil)
		d := tds["deductor"].(map[string]any)
		if d["tan"] != "DELS12345A" || d["pan"] != "AAACS1234B" || d["responsible_name"] != "Deepak Arora" || d["responsible_desig"] != "Payroll Manager" {
			t.Fatal(d)
		}
		if s, _ := c.do("PUT", "/api/tds/deductor", "EMP-1012", "Payroll Manager", map[string]any{"tan": "X1"}); s != 400 {
			t.Fatal("a wrong TAN is refused")
		}
		if s, r := c.do("PUT", "/api/tds/deductor", "EMP-1012", "Payroll Manager", map[string]any{"name": "Deepak A", "desig": "Manager"}); s != 200 {
			t.Fatal(r)
		}
		_, tds = c.do("GET", "/api/tds", "EMP-1012", "Payroll Manager", nil)
		if d := tds["deductor"].(map[string]any); d["tan"] != "DELS12345A" || d["responsible_name"] != "Deepak A" {
			t.Fatal("fields left out keep their value", d)
		}
		_, ps := c.do("GET", "/api/payslip-setup", "EMP-1011", "Payroll", nil)
		if ps["setup"].(map[string]any)["accent"] != "#0f6e6e" {
			t.Fatal("the payslip setup is left alone")
		}
	})

	t.Run("form 16 basis and release switch", func(t *testing.T) {
		_, f := c.do("GET", "/api/form16", "EMP-1011", "Payroll", nil)
		rows, _ := f["rows"].([]any)
		if len(rows) == 0 {
			t.Fatal(f)
		}
		for _, x := range rows {
			m := x.(map[string]any)
			if m["basis"] != "declared" && m["basis"] != "approved" {
				t.Fatal("basis", m)
			}
			want := num(m["annual"]) - num(m["done"]) - num(m["prevTds"])
			if want < 0 {
				want = 0
			}
			if _, ok := m["prevTds"].(float64); !ok || !near(num(m["toDeduct"]), want, 0.01) {
				t.Fatal("still to deduct", m)
			}
		}
		if _, one := c.do("GET", "/api/form16/EMP-1002", "EMP-1011", "Payroll", nil); one["basis"] != "declared" {
			t.Fatal(one["basis"])
		}
		if s, _ := c.do("POST", "/api/form16/release", "EMP-1011", "Payroll", map[string]any{"on": false}); s != 403 {
			t.Fatal("release needs a payroll approver")
		}
		if s, r := c.do("POST", "/api/form16/release", "EMP-1012", "Payroll Manager", map[string]any{}); s != 200 || r["released"] != true {
			t.Fatal(r)
		}
		if _, f := c.do("GET", "/api/form16", "EMP-1011", "Payroll", nil); f["released"] != true {
			t.Fatal(f["released"])
		}
		if s, r := c.do("POST", "/api/form16/release", "EMP-1012", "Payroll Manager", map[string]any{"on": false}); s != 200 || r["released"] != false {
			t.Fatal(r)
		}
		if _, f := c.do("GET", "/api/form16", "EMP-1011", "Payroll", nil); f["released"] != false {
			t.Fatal("withdrawn", f["released"])
		}
		if s, _ := c.do("GET", "/api/form16/me", "EMP-1020", "Employee", nil); s != 404 {
			t.Fatal("a withdrawn Form 16 is not shown to the employee")
		}
		if s, r := c.do("POST", "/api/form16/release", "EMP-1012", "Payroll Manager", map[string]any{"on": true}); s != 200 || r["released"] != true {
			t.Fatal(r)
		}
	})

	t.Run("tax declarations: approved, remind, export, items", func(t *testing.T) {
		_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		w := tax["windows"].(map[string]any)
		yes, no := true, false
		win := map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "declForce": yes, "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": no}
		if s, r := c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win); s != 200 {
			t.Fatal(r)
		}
		s, r := c.do("POST", "/api/tax/remind", "EMP-1011", "Payroll", nil)
		if s != 200 || num(r["count"]) < 2 {
			t.Fatalf("%d %v", s, r)
		}
		before := num(r["count"])
		if s, _ := c.do("POST", "/api/tax/remind", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees cannot send reminders")
		}
		decl := map[string]any{"regime": "Old", "items": map[string]any{"c_ppf": 150000, "d_self": 25000}, "submit": true}
		if s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", decl); s != 200 {
			t.Fatal(r)
		}
		if _, r := c.do("POST", "/api/tax/remind", "EMP-1011", "Payroll", nil); num(r["count"]) != before-1 {
			t.Fatalf("EMP-1002 has declared: %v, before %v", r["count"], before)
		}
		n, _ := st.DB.C("tax_reminder").Count(ctx, nil)
		if n != 2 {
			t.Fatal("each reminder is logged", n)
		}
		_, list := c.do("GET", "/api/tax", "EMP-1011", "Payroll", nil)
		me := findRow(list["rows"], "id", "EMP-1002")
		if me == nil || num(me["declaredDed"]) < 175000 || num(me["approved"]) <= 0 || num(me["approved"]) >= num(me["declaredDed"]) {
			t.Fatal("declared and approved deductions", me)
		}
		// Items and limits: a lower limit and switching an item off change what is counted.
		s, items := c.do("GET", "/api/tax/items", "EMP-1011", "Payroll", nil)
		if s != 200 || items["canEdit"] != true || findRow(items["items"], "id", "hra") != nil {
			t.Fatal(items)
		}
		if d := findRow(items["items"], "id", "d_self"); d == nil || d["cap"] != 25000.0 || d["active"] != true || d["section"] == "" {
			t.Fatal(d)
		}
		ded := func() float64 {
			_, x := c.do("GET", "/api/tax/employee/EMP-1002", "EMP-1011", "Payroll", nil)
			return num(x["figure"].(map[string]any)["declared"].(map[string]any)["oldDeductions"])
		}
		d0 := ded()
		if s, r := c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []any{map[string]any{"id": "d_self", "cap": 10000}}}); s != 200 {
			t.Fatal(r)
		}
		if d1 := ded(); !near(d0-d1, 15000, 0.5) {
			t.Fatalf("limit 10,000: %v then %v", d0, d1)
		}
		c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []any{map[string]any{"id": "d_self", "active": false}}})
		if d2 := ded(); !near(d0-d2, 25000, 0.5) {
			t.Fatalf("switched off: %v then %v", d0, d2)
		}
		_, mine := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		if it := findRow(mine["items"], "id", "d_self"); it == nil || it["active"] != false || it["cap"] != 10000.0 {
			t.Fatal("the employee's form gets the item settings", it)
		}
		if s, _ := c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []any{map[string]any{"id": "hra", "cap": 1}}}); s != 400 {
			t.Fatal("rent is not an item with a limit")
		}
		if s, _ := c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []any{map[string]any{"id": "zz", "cap": 1}}}); s != 400 {
			t.Fatal("unknown item")
		}
		if s, _ := c.do("PUT", "/api/tax/items", "EMP-1020", "Employee", map[string]any{"items": []any{map[string]any{"id": "d_self", "active": true}}}); s != 403 {
			t.Fatal("employees cannot change items")
		}
		c.do("PUT", "/api/tax/items", "EMP-1011", "Payroll", map[string]any{"items": []any{map[string]any{"id": "d_self", "cap": 25000, "active": true}}})
		if d3 := ded(); !near(d3, d0, 0.5) {
			t.Fatal("restored", d3, d0)
		}
		// The export has the prototype's columns.
		s, raw := c.getRaw("/api/tax/declarations.xlsx", "EMP-1011", "Payroll")
		if s != 200 {
			t.Fatalf("%d %s", s, raw)
		}
		xs := sheetRows(t, raw)
		if len(xs) < 2 || xs[0][5] != "Approved deductions" || xs[0][12] != "To recover" {
			t.Fatal(xs)
		}
		found := false
		for _, x := range xs[1:] {
			found = found || (x[0] == "EMP-1002" && x[2] == "Old" && x[4] == fmt.Sprint(num(me["declaredDed"])))
		}
		if !found {
			t.Fatal("EMP-1002 in the export", xs)
		}
		if s, _ := c.getRaw("/api/tax/declarations.xlsx", "EMP-1020", "Employee"); s != 403 {
			t.Fatal("employees cannot export declarations")
		}
		win["declForce"] = no
		c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win)
		if s, _ := c.do("POST", "/api/tax/remind", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("no reminders while the declaration window is closed")
		}
	})

	t.Run("statutory preview, city and files created", func(t *testing.T) {
		from, to := "2000-01-01", today()
		q := "/api/statutory/pf?from=" + from + "&to=" + to
		s, all := c.do("GET", q, "EMP-1012", "Payroll Manager", nil)
		if s != 200 || len(all["rows"].([]any)) == 0 {
			t.Fatal(all)
		}
		// Preview needs only payroll view rights; confidential columns follow the role's masking.
		s, chro := c.do("GET", q, "EMP-1001", "CHRO", nil)
		if s != 200 || len(chro["rows"].([]any)) != len(all["rows"].([]any)) {
			t.Fatalf("CHRO preview: %d %v", s, chro["error"])
		}
		for i, x := range chro["rows"].([]any) {
			full := all["rows"].([]any)[i].(map[string]any)["cells"].([]any)
			cells := x.(map[string]any)["cells"].([]any)
			if cells[1] != "EMP-1001" && full[9] != "" && full[9] != nil && cells[9] != nil { // Aadhaar (column 9); own data is shown in full
				t.Fatal("CHRO does not see Aadhaar", cells)
			}
			if cells[13] != full[13] {
				t.Fatal("CHRO sees wages in full", cells[13], full[13])
			}
		}
		if s, _ := c.getRaw("/api/statutory/pf/sheet?from="+from+"&to="+to, "EMP-1001", "CHRO"); s != 403 {
			t.Fatal("downloads keep pay.export")
		}
		if s, _ := c.do("GET", q, "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees cannot preview statutory files")
		}
		// One city.
		_, city := c.do("GET", q+"&city="+delhi, "EMP-1012", "Payroll Manager", nil)
		rows := city["rows"].([]any)
		if len(rows) == 0 || len(rows) >= len(all["rows"].([]any)) {
			t.Fatalf("Delhi rows %d of %d", len(rows), len(all["rows"].([]any)))
		}
		for _, x := range rows {
			id := x.(map[string]any)["cells"].([]any)[1].(string)
			if cid := ui2PayrollCityOf(ctx, st, id); cid != delhi {
				t.Fatal(id, "is not in Delhi")
			}
		}
		// A download is remembered and can be downloaded again with the same rows.
		s, raw := c.getRaw("/api/statutory/pf/sheet?from="+from+"&to="+to+"&city="+delhi, "EMP-1012", "Payroll Manager")
		if s != 200 || len(sheetRows(t, raw)) != len(rows)+1 {
			t.Fatalf("download %d", s)
		}
		s, h := c.do("GET", "/api/statutory/history", "EMP-1001", "CHRO", nil)
		files, _ := h["files"].([]any)
		if s != 200 || len(files) != 1 || h["canDownload"] != false {
			t.Fatal(h)
		}
		f := files[0].(map[string]any)
		if f["file"] != "pf" || num(f["rows"]) != float64(len(rows)) || f["cityId"] != delhi || f["period"] != from+"_to_"+to || f["by"] == "" {
			t.Fatal(f)
		}
		id := fmt.Sprint(f["id"])
		s, raw = c.getRaw("/api/statutory/history/"+id+"/sheet", "EMP-1012", "Payroll Manager")
		if s != 200 || len(sheetRows(t, raw)) != len(rows)+1 {
			t.Fatalf("again %d", s)
		}
		if s, _ := c.getRaw("/api/statutory/history/"+id+"/sheet", "EMP-1001", "CHRO"); s != 403 {
			t.Fatal("downloading again needs pay.export")
		}
		if _, h := c.do("GET", "/api/statutory/history", "EMP-1012", "Payroll Manager", nil); len(h["files"].([]any)) != 1 || h["canDownload"] != true {
			t.Fatal("downloading again adds no entry", h)
		}
		if s, _ := c.do("GET", "/api/statutory/history", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees cannot see the file list")
		}
	})

	t.Run("structure mapping candidates", func(t *testing.T) {
		s, r := c.do("GET", "/api/comp/structure-map/candidates", "EMP-1011", "Payroll", nil)
		if s != 200 || r["canApply"] != true {
			t.Fatal(r)
		}
		n, _ := st.DB.C("employee").Count(ctx, bson.M{"status": "Active"})
		active := int(n)
		total := 0.0
		for _, v := range r["versions"].([]any) {
			total += num(v.(map[string]any)["today"])
		}
		def := r["default"].(string)
		if int(total) != active || r["from"] != def || len(r["rows"].([]any)) != active {
			t.Fatalf("on versions today %v of %d; rows %d", total, active, len(r["rows"].([]any)))
		}
		if e := findRow(r["rows"], "id", "EMP-1020"); e == nil || num(e["gross"]) <= 0 {
			t.Fatal("gross", e)
		}
		_, v2 := c.do("POST", "/api/comp/structures", "EMP-1002", "HR Admin", map[string]any{"name": "Version 2"})
		v2id := v2["id"].(string)
		if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1011", "Payroll", map[string]any{"structureId": v2id, "fromDate": today(), "ids": []string{"EMP-1020"}, "from": def}); s != 200 || r["mapped"] != 1.0 {
			t.Fatal(r)
		}
		_, r = c.do("GET", "/api/comp/structure-map/candidates?from="+v2id+"&date="+today(), "EMP-1011", "Payroll", nil)
		e := findRow(r["rows"], "id", "EMP-1020")
		if len(r["rows"].([]any)) != 1 || e == nil {
			t.Fatal(r["rows"])
		}
		h := e["history"].([]any)
		if len(h) == 0 || h[len(h)-1].(map[string]any)["structure"] != "Version 2" || h[len(h)-1].(map[string]any)["from"] != today() {
			t.Fatal(h)
		}
		lg := r["log"].([]any)[0].(map[string]any)
		if lg["toName"] != "Version 2" || lg["fromName"] == "" || lg["count"] != 1.0 || lg["date"] != today() {
			t.Fatal(lg)
		}
		_, r = c.do("GET", "/api/comp/structure-map/candidates?city="+delhi+"&type=Permanent", "EMP-1011", "Payroll", nil)
		for _, x := range r["rows"].([]any) {
			if m := x.(map[string]any); m["cityId"] != delhi || m["type"] != "Permanent" {
				t.Fatal(m)
			}
		}
		// Payroll applies a mapping with pay.run 2 even without the structure right, for chosen people.
		structureRight := bson.M{"role_name": "Payroll", "right_id": "comp.structure"}
		if _, err := st.DB.C("role_right").Update(ctx, structureRight, bson.M{"$set": bson.M{"level": 1}}); err != nil {
			t.Fatal(err)
		}
		if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1011", "Payroll", map[string]any{"structureId": v2id, "fromDate": today(), "ids": []string{"EMP-1022"}}); s != 200 {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", "/api/comp/structure-map", "EMP-1011", "Payroll", map[string]any{"structureId": v2id, "fromDate": today(), "type": "Retainer"}); s != 400 {
			t.Fatal("without the structure right only chosen people are mapped")
		}
		if s, _ := c.do("POST", "/api/comp/structure-map", "EMP-1004", "HRBP", map[string]any{"structureId": v2id, "fromDate": today(), "ids": []string{"EMP-1020"}}); s != 403 {
			t.Fatal("HRBP cannot map")
		}
		if s, _ := c.do("GET", "/api/comp/structure-map/candidates", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees cannot list candidates")
		}
		_, _ = st.DB.C("role_right").Update(ctx, structureRight, bson.M{"$set": bson.M{"level": 2}})
	})

	t.Run("payslip preview, held sums, stale run, unchanged rows", func(t *testing.T) {
		pv := "/api/payslip-preview?employeeId=EMP-1020&ym=" + open
		s, r := c.do("GET", pv, "EMP-1011", "Payroll", nil)
		if s != 200 || r["preview"] != true || r["ym"] != open || r["label"] == "" || r["setup"] == nil {
			t.Fatalf("%d %v", s, r)
		}
		snap := r["snap"].(map[string]any)
		if num(snap["net"]) <= 0 || snap["emp"].(map[string]any)["id"] != "EMP-1020" || len(snap["earn"].([]any)) == 0 {
			t.Fatal(snap)
		}
		if s, _ := c.do("GET", "/api/payslip-preview?employeeId=EMP-1014", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("contractual staff get no payslip")
		}
		if s, _ := c.do("GET", pv, "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("an employee does not preview payslips")
		}
		if s, _ := c.do("GET", "/api/payslip-preview?employeeId=EMP-1020&ym=2026-13", "EMP-1011", "Payroll", nil); s != 400 {
			t.Fatal("bad month")
		}
		// HRBP sees salaries masked.
		s, hb := c.do("GET", pv, "EMP-1004", "HRBP", nil)
		if s != 200 || hb["masked"] != true {
			t.Fatalf("HRBP: %d %v", s, hb)
		}
		if _, isNum := hb["snap"].(map[string]any)["net"].(float64); isNum {
			t.Fatal("HRBP sees the net pay masked")
		}
		// The setup screen's preview without an employee still works.
		if pv0 := func() map[string]any {
			_, x := c.do("GET", "/api/payslip-preview", "EMP-1011", "Payroll", nil)
			return x
		}(); pv0["snap"] == nil {
			t.Fatal(pv0)
		}

		// A hold: sums leave the held salary out and count it.
		if s, r := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1016", "reason": "Other", "note": "Testing the hold", "fromYm": open}); s != 200 {
			t.Fatal(r)
		}
		c.pcCloseAll(t, open)
		if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
			t.Fatal(r)
		}
		_, cl := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil)
		mo := cl["month"].(map[string]any)
		if mo["stale"] != false {
			t.Fatal("just calculated", mo)
		}
		tot := mo["totals"].(map[string]any)
		_, rows := c.do("GET", "/api/payroll/"+open+"/rows?size=500", "EMP-1011", "Payroll", nil)
		sum := rows["sum"].(map[string]any)
		if num(sum["held"]) != 1 || !near(num(sum["net"]), num(tot["net"]), 0.5) || !near(num(sum["heldNet"]), num(tot["heldNet"]), 0.5) || num(sum["heldNet"]) <= 0 {
			t.Fatal(sum, tot)
		}
		_, dl := c.do("GET", "/api/payroll/"+open+"/rows?size=500&city="+delhi, "EMP-1011", "Payroll", nil)
		if ds := dl["sum"].(map[string]any); num(ds["held"]) != 1 || num(ds["net"]) >= num(sum["net"]) {
			t.Fatal("held in the filtered view", ds)
		}
		// The dry-run row is the preview.
		row := c.row(t, open, "EMP-1020")
		if _, r := c.do("GET", pv, "EMP-1011", "Payroll", nil); !near(num(r["snap"].(map[string]any)["net"]), num(row["net"]), 0.01) {
			t.Fatal("preview net", r["snap"].(map[string]any)["net"], row["net"])
		}

		// A pay line added after the calculation makes the run stale until it is recalculated.
		if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": "EMP-1020", "ym": open, "kind": "earn", "label": "Arrear for test", "amount": 1000, "reason": "test"}); err != nil {
			t.Fatal(err)
		}
		if _, cl := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil); cl["month"].(map[string]any)["stale"] != true {
			t.Fatal("stale after a new pay line")
		}
		if _, s := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil); s["open"].(map[string]any)["stale"] != true {
			t.Fatal("stale on the payroll state")
		}
		c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil)
		if _, cl := c.do("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil); cl["month"].(map[string]any)["stale"] != false {
			t.Fatal("not stale after recalculating")
		}

		// A corrected sheet: rows left as they are are counted as unchanged.
		a, b := c.row(t, open, "EMP-1020"), c.row(t, open, "EMP-1022")
		csv := fmt.Sprintf("Emp ID,Paid days,Other earnings,Other deductions,Adjustment reason\nEMP-1020,%v,0,0,\nEMP-1022,%v,500,0,Festival bonus\n", a["paid"], b["paid"])
		s, chk := c.sendFile("/api/payroll/"+open+"/sheet/check", "EMP-1011", "Payroll", "sheet.csv", []byte(csv))
		if s != 200 || num(chk["unchanged"]) != 1 || num(chk["ready"]) != 1 || len(chk["changes"].([]any)) != 1 {
			t.Fatalf("%d %v", s, chk)
		}

		// Once published the preview is the published payslip.
		c.closeAndPublish(t, open)
		if _, r := c.do("GET", pv, "EMP-1011", "Payroll", nil); r["preview"] != false || r["snap"] == nil {
			t.Fatal("published", r["preview"])
		}
		if _, r := c.do("GET", "/api/payslip-preview?employeeId=EMP-1020", "EMP-1011", "Payroll", nil); r["preview"] != true || r["ym"] != nextYm(open) {
			t.Fatal("the next open month", r["ym"], r["preview"])
		}
	})
}
