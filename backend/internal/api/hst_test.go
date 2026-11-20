package api_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// hstOK runs a call as HR Admin and fails the test unless it answers 200.
func hstOK(t *testing.T, c *client, method, path string, body any) map[string]any {
	t.Helper()
	s, r := c.do(method, path, "EMP-1002", "HR Admin", body)
	if s != 200 {
		t.Fatalf("%s %s: %d %v", method, path, s, r)
	}
	return r
}

func hstUnit(t *testing.T, c *client, name, cluster, gender string, beds int, near ...string) string {
	t.Helper()
	r := hstOK(t, c, "POST", "/api/hst/units", map[string]any{"name": name, "kind": "Flat", "address": "Test address", "clusterId": cluster, "near": near,
		"gender": gender, "beds": beds, "landlord": "Mr Landlord", "landlordPhone": "9876543210", "fixed": []any{}})
	return r["id"].(string)
}

func hstIn(c *client, emp, unit, in string, bed int) (int, map[string]any) {
	return c.do("POST", "/api/hst/stays", "EMP-1002", "HR Admin", map[string]any{"employeeId": emp, "unitId": unit, "checkIn": in, "bed": bed})
}

// hstPeople returns active employees other than skip, oldest code first.
func hstPeople(t *testing.T, st *store.Store, n int, skip ...string) []string {
	t.Helper()
	rows, err := st.DB.C("employee").Find(context.Background(), bson.M{"status": "Active", "_id": bson.M{"$nin": skip}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(n)))
	if err != nil || len(rows) < n {
		t.Fatalf("people: %v %d", err, len(rows))
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r["id"].(string))
	}
	return out
}

// Units, leases, check-in and check-out: no double booking, the equal and by-days splits, empty-unit cost, suggestions,
// check-out prompts and reminders.
func TestHstAllocationAndSplit(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	ctx := context.Background()
	if s, _ := c.do("GET", "/api/hst/setup", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("an employee sees accommodation setup: %d", s)
	}
	if s, _ := c.do("POST", "/api/hst/units", "EMP-1020", "Employee", map[string]any{}); s != 403 {
		t.Fatalf("an employee adds a unit: %d", s)
	}
	me := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	loc := me["location_id"].(string)
	cluster := apiDoc(st, "location", bson.M{"_id": loc})["cluster_id"].(string)
	other := apiDoc(st, "location", bson.M{"_id": bson.M{"$ne": loc}})["id"].(string)
	gender := str(apiDoc(st, "employee_personal", bson.M{"employee_id": "EMP-1020"})["gender"])

	if s, r := c.do("POST", "/api/hst/units", "EMP-1002", "HR Admin", map[string]any{"name": "No property", "kind": "Flat", "clusterId": cluster, "near": []string{},
		"gender": "Any", "beds": 2}); s != 400 || !strings.Contains(fmt.Sprint(r), "property") {
		t.Fatalf("a unit without a property: %d %v", s, r)
	}
	u1 := hstUnit(t, c, "Test Flat 1", cluster, "Any", 2, loc)
	if s, _ := c.do("POST", "/api/hst/units", "EMP-1002", "HR Admin", map[string]any{"name": "test flat 1", "kind": "Flat", "clusterId": cluster, "near": []string{loc},
		"gender": "Any", "beds": 1}); s != 400 {
		t.Fatalf("a second unit with the same name: %d", s)
	}
	hstOK(t, c, "POST", "/api/hst/units/"+u1+"/leases", map[string]any{"start": "2026-01-01", "rent": 12000, "deposit": 24000, "noticeDays": 30})
	if s, _ := c.do("POST", "/api/hst/units/"+u1+"/leases", "EMP-1002", "HR Admin", map[string]any{"start": "2026-06-01", "rent": 13000}); s != 400 {
		t.Fatalf("an overlapping lease: %d", s)
	}

	p := hstPeople(t, st, 3, "EMP-1020", "EMP-1002")
	a, b, cc := p[0], p[1], p[2]
	if s, r := hstIn(c, a, u1, "2026-10-01", 0); s != 200 || r["bed"] != 1.0 {
		t.Fatalf("check-in A: %d %v", s, r)
	}
	if s, r := hstIn(c, b, u1, "2026-11-16", 0); s != 200 || r["bed"] != 2.0 {
		t.Fatalf("check-in B: %d %v", s, r)
	}
	// No double booking: the unit is full, bed 1 is taken, and A cannot stay in two places at once.
	if s, r := hstIn(c, cc, u1, "2026-11-20", 0); s != 400 || !strings.Contains(fmt.Sprint(r), "no free bed") {
		t.Fatalf("full unit: %d %v", s, r)
	}
	if s, _ := hstIn(c, cc, u1, "2026-11-20", 1); s != 400 {
		t.Fatalf("bed 1 booked twice: %d", s)
	}
	u2 := hstUnit(t, c, "Test Flat 2", cluster, "Any", 3, other, loc)
	if s, r := hstIn(c, a, u2, "2026-11-01", 0); s != 400 || !strings.Contains(fmt.Sprint(r), "already stays") {
		t.Fatalf("A in two units: %d %v", s, r)
	}
	if apiCount(st, store.HstStayColl, nil) != 2 {
		t.Fatal("a refused check-in was stored")
	}

	// The split (decided 17 Nov): 12k a month, A all month, B from the 16th. Equal per person (the default): 6k each,
	// and no empty-unit cost although B was not there for half the month.
	m, err := api.HstMonthCost(ctx, st.DB, "2026-11")
	if err != nil {
		t.Fatal(err)
	}
	if m.Method != "equal" || m.Shares[a] != 6000 || m.Shares[b] != 6000 || m.Units[u1].Empty != 0 || m.Units[u1].Cost != 12000 {
		t.Fatalf("November split: %+v", m.Units[u1])
	}
	// Before anyone stayed: the whole month is empty-unit cost for the unit's property.
	if m, _ := api.HstMonthCost(ctx, st.DB, "2026-09"); m.Empty[loc] != 12000 || len(m.Shares) != 0 {
		t.Fatalf("September empty cost: %+v", m.Empty)
	}
	// Variable costs are part of the month cost.
	hstOK(t, c, "PUT", "/api/hst/units/"+u1+"/costs/2026-11", map[string]any{"electricity": 1800, "water": 300, "wifi": 900})
	if m, _ := api.HstMonthCost(ctx, st.DB, "2026-11"); m.Shares[a] != 7500 || m.Shares[b] != 7500 {
		t.Fatalf("with 3,000 of bills: %v", m.Shares)
	}
	// The "Split by days stayed" option: A alone 1–15 Nov, shared 16–30.
	if s, _ := c.do("PUT", "/api/hst/settings", "EMP-1020", "Employee", map[string]any{"byDays": true, "leaseDays": 60, "incDays": 30, "boardDays": 60}); s != 403 {
		t.Fatalf("an employee changed the split: %d", s)
	}
	hstOK(t, c, "PUT", "/api/hst/settings", map[string]any{"byDays": true, "leaseDays": 60, "incDays": 30, "boardDays": 60})
	if set := hstOK(t, c, "GET", "/api/hst/setup", nil)["settings"].(map[string]any); set["byDays"] != true {
		t.Fatalf("settings: %v", set)
	}
	if m, _ := api.HstMonthCost(ctx, st.DB, "2026-11"); m.Method != "days" || m.Shares[a] != 11250 || m.Shares[b] != 3750 {
		t.Fatalf("by days, with 3,000 of bills: %v", m.Shares)
	}
	if r := hstOK(t, c, "GET", "/api/hst/split?ym=2026-11", nil); r["method"] != "days" {
		t.Fatalf("split method: %v", r["method"])
	}
	hstOK(t, c, "PUT", "/api/hst/settings", map[string]any{"byDays": false, "leaseDays": 60, "incDays": 30, "boardDays": 60})

	// Suggestions for EMP-1020: units of the cluster with a free bed, the one serving their property first.
	u3 := hstUnit(t, c, "Test Flat 3", cluster, "Any", 1, loc)
	opp := map[string]string{"Male": "Female", "Female": "Male"}[gender]
	if opp != "" {
		u4 := hstUnit(t, c, "Test Flat 4", cluster, opp, 4, loc)
		if s, r := hstIn(c, "EMP-1020", u4, "2026-11-20", 0); s != 400 || !strings.Contains(fmt.Sprint(r), "staff") {
			t.Fatalf("gender mismatch allowed: %d %v", s, r)
		}
	}
	sg := hstOK(t, c, "GET", "/api/hst/suggest?employeeId=EMP-1020&date=2026-11-20", nil)
	us := sg["units"].([]any)
	if len(us) != 2 || us[0].(map[string]any)["id"] != u3 || us[1].(map[string]any)["id"] != u2 {
		t.Fatalf("suggestions: %v", us)
	}

	// Check-out: the check-out day is free; once only; an edit cannot double-book.
	if s, r := c.do("POST", "/api/hst/stays/"+hstStayID(t, st, a)+"/checkout", "EMP-1002", "HR Admin", map[string]any{"date": "2026-12-11", "reason": "Moved"}); s != 200 {
		t.Fatalf("check-out: %v", r)
	}
	if s, _ := c.do("POST", "/api/hst/stays/"+hstStayID(t, st, a)+"/checkout", "EMP-1002", "HR Admin", map[string]any{"date": "2026-12-12"}); s != 400 {
		t.Fatal("checked out twice")
	}
	if s, _ := c.do("PUT", "/api/hst/stays/"+hstStayID(t, st, b), "EMP-1002", "HR Admin", map[string]any{"checkIn": "2026-11-16", "bed": 1}); s != 400 {
		t.Fatal("an edit moved B onto A's bed while A was there")
	}
	if s, r := hstIn(c, cc, u1, "2026-12-11", 1); s != 200 {
		t.Fatalf("bed 1 free on A's check-out day: %v", r)
	}
	if s, _ := c.do("PUT", "/api/hst/units/"+u1, "EMP-1002", "HR Admin", map[string]any{"name": "Test Flat 1", "kind": "Flat", "clusterId": cluster, "near": []string{loc},
		"gender": "Any", "beds": 1}); s != 400 {
		t.Fatal("beds reduced below a bed in use")
	}
	if m, _ := api.HstMonthCost(ctx, st.DB, "2026-12"); m.Units[u1].Cost != 12000 || m.Units[u1].Allocated+m.Units[u1].Empty != 12000 {
		t.Fatalf("December does not foot: %+v", m.Units[u1])
	}

	// History per person and per unit.
	if r := hstOK(t, c, "GET", "/api/hst/stays?employeeId="+a, nil); len(r["stays"].([]any)) != 1 {
		t.Fatalf("A's history: %v", r)
	}
	if r := hstOK(t, c, "GET", "/api/hst/units/"+u1+"?ym=2026-11", nil); len(r["stays"].([]any)) != 3 || r["month"].(map[string]any)["cost"] != 15000.0 {
		t.Fatalf("unit history: %v", r["month"])
	}

	// B leaves: HR is told to check them out, once.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": b}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": "2026-11-30"}}); err != nil {
		t.Fatal(err)
	}
	att := hstOK(t, c, "GET", "/api/hst/attention", nil)["list"].([]any)
	found := false
	for _, x := range att {
		if m := x.(map[string]any); m["stay"].(map[string]any)["employeeId"] == b {
			found = m["kind"] == "exit" && m["suggest"] == "2026-12-01"
		}
	}
	if !found {
		t.Fatalf("attention: %v", att)
	}
	// A lease ending in 30 days and a rent increase in 10: reminders to HR.
	now := time.Now()
	day := store.DateOf(now)
	hstOK(t, c, "POST", "/api/hst/units/"+u2+"/leases", map[string]any{"start": "2025-01-01", "end": store.DateOf(now.AddDate(0, 0, 30)), "rent": 9000, "noticeDays": 30})
	hstOK(t, c, "POST", "/api/hst/units/"+u3+"/leases", map[string]any{"start": "2025-01-01", "rent": 8000, "incPct": 5, "incOn": store.DateOf(now.AddDate(0, 0, 10))})
	n, err := app.HstSweep(ctx, now)
	if err != nil || n == 0 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	for _, pre := range []string{"hst:checkout:" + hstStayID(t, st, b), "hst:lease-end:", "hst:lease-notice:", "hst:inc:"} {
		if apiCount(st, "notice", bson.M{"to_employee_id": "EMP-1002", "ref": bson.M{"$regex": "^" + pre}}) != 1 {
			t.Fatalf("no %s reminder to the HR Admin on %s", pre, day)
		}
	}
	if n, _ := app.HstSweep(ctx, now); n != 0 {
		t.Fatalf("reminders sent twice: %d", n)
	}

	// The occupancy board and its Excel file.
	bd := hstOK(t, c, "GET", "/api/hst/board?ym=2026-11", nil)
	tot := bd["total"].(map[string]any)
	if tot["units"].(float64) < 3 || tot["cost"].(float64) < 15000 || len(bd["ending"].([]any)) != 1 {
		t.Fatalf("board: %v", bd)
	}
	if s, _ := c.getRaw("/api/hst/board.xlsx?ym=2026-11", "EMP-1002", "HR Admin"); s != 200 {
		t.Fatalf("board export: %d", s)
	}
	if apiCount(st, "audit_log", bson.M{"entity": bson.M{"$in": bson.A{store.HstUnitColl, store.HstLeaseColl, store.HstStayColl, store.HstCostColl}}}) < 10 {
		t.Fatal("changes not audited")
	}
}

func hstStayID(t *testing.T, st *store.Store, emp string) string {
	t.Helper()
	r := apiDoc(st, store.HstStayColl, bson.M{"employee_id": emp}, options.FindOne().SetSort(bson.D{{Key: "check_in", Value: -1}}))
	if r == nil {
		t.Fatalf("no stay of %s", emp)
	}
	return r["id"].(string)
}

// Accommodation is not payroll (decided 17 Nov): a month calculated for a person with and without a stay gives the
// same payroll row, payslip, payroll sheet row and salary register row, the open run is not marked for
// recalculation by an accommodation change, and the manual accommodation deduction (employee_pay.accommodation,
// the minimum-wage top-up) stays on the payslip as before.
func TestHstPayrollUnaffected(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012", "EMP-1020")
	ctx := context.Background()
	const who = "EMP-1020"
	open := pfaOpen(t, c)
	c.pcCloseAll(t, open)
	// The manual accommodation deduction, untouched by the module.
	if _, err := st.DB.C("employee_pay").Upsert(ctx, bson.M{"employee_id": who}, bson.M{"accommodation": 1500.0}, nil); err != nil {
		t.Fatal(err)
	}
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	// Full pay for the month, so PF, ESI and the rest are not zero.
	r0 := c.row(t, open, who)
	full := num(r0["days"]) - num(r0["outsideP"])
	if s, r := c.do("PATCH", "/api/payroll/"+open+"/rows/"+who, "EMP-1011", "Payroll", map[string]any{"paidDays": full, "otherEarn": 0, "otherDed": 0, "reason": "Full month for the test"}); s != 200 {
		t.Fatalf("paid days: %v", r)
	}
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	if pfaStale(t, c) {
		t.Fatal("the run is stale before the accommodation changes")
	}
	type outputs struct {
		row, snap map[string]any
		sheet     []string
		register  []string
	}
	capture := func() outputs {
		var o outputs
		o.row = c.row(t, open, who)
		_, pv := c.do("GET", "/api/payslip-preview?employeeId="+who+"&ym="+open, "EMP-1011", "Payroll", nil)
		o.snap, _ = pv["snap"].(map[string]any)
		f, _ := bfBook(t, c, "/api/payroll/"+open+"/full-register.xlsx", "EMP-1012", "Payroll Manager")
		reg := bfSheet(t, f, "Full register")
		o.register = append([]string{}, reg[1]...)
		for _, x := range reg[2:] {
			if len(x) > 0 && x[0] == who {
				o.register = append(o.register, x...)
			}
		}
		sf, _ := bfBook(t, c, "/api/payroll/"+open+"/sheet.xlsx", "EMP-1011", "Payroll")
		sh := bfSheet(t, sf, "Data")
		o.sheet = append([]string{}, sh[0]...)
		for _, x := range sh[1:] {
			if len(x) > 0 && x[0] == who {
				o.sheet = append(o.sheet, x...)
			}
		}
		return o
	}
	before := capture()
	if num(before.row["r"].(map[string]any)["eePF"]) == 0 {
		t.Fatalf("no PF to compare: %v", before.row["r"])
	}
	if before.snap == nil {
		t.Fatal("no payslip preview")
	}
	hasLine := func(snap map[string]any, side, label string) float64 {
		for _, l := range snap[side].([]any) {
			if m := l.(map[string]any); m["label"] == label {
				return num(m["amount"])
			}
		}
		return 0
	}
	if hasLine(before.snap, "ded", "Accommodation") != 1500 {
		t.Fatalf("the manual accommodation deduction is not on the payslip: %v", before.snap["ded"])
	}

	// A stay all month in a ₹6,000 unit, with bills.
	loc := apiDoc(st, "employee", bson.M{"_id": who})["location_id"].(string)
	cluster := apiDoc(st, "location", bson.M{"_id": loc})["cluster_id"].(string)
	u := hstUnit(t, c, "Payroll Flat", cluster, "Any", 1, loc)
	hstOK(t, c, "POST", "/api/hst/units/"+u+"/leases", map[string]any{"start": "2020-01-01", "rent": 6000})
	hstOK(t, c, "PUT", "/api/hst/units/"+u+"/costs/"+open, map[string]any{"electricity": 900})
	if s, r := hstIn(c, who, u, "2020-01-01", 0); s != 200 {
		t.Fatalf("check-in: %v", r)
	}
	hstOK(t, c, "PUT", "/api/hst/settings", map[string]any{"byDays": true, "leaseDays": 60, "incDays": 30, "boardDays": 60})
	if m, _ := api.HstMonthCost(ctx, st.DB, open); m.Shares[who] != 6900 {
		t.Fatalf("the cost source: %v", m.Shares)
	}
	if pfaStale(t, c) {
		t.Fatal("an accommodation change marked the payroll run for recalculation")
	}
	pfaMust(t, c, open, "dry-run", "EMP-1011", "Payroll")
	after := capture()
	if _, ok := after.row["hst"]; ok {
		t.Fatalf("the payroll row carries accommodation: %v", after.row["hst"])
	}
	for _, k := range []string{"calculatedAt", "at"} { // when the row was worked out, not what it says
		delete(before.row, k)
		delete(after.row, k)
	}
	if !reflect.DeepEqual(before.row, after.row) {
		t.Fatalf("payroll row changed:\n%v\n%v", before.row, after.row)
	}
	if !reflect.DeepEqual(before.snap, after.snap) {
		t.Fatalf("payslip changed:\n%v\n%v", before.snap, after.snap)
	}
	if !reflect.DeepEqual(before.register, after.register) {
		t.Fatalf("salary register changed:\n%v\n%v", before.register, after.register)
	}
	for _, col := range []string{"Company accommodation", "Accommodation recovery"} {
		if bfCol(after.register, col) >= 0 {
			t.Fatalf("register column %q", col)
		}
	}
	for _, side := range []string{"earn", "ded"} {
		for _, l := range after.snap[side].([]any) {
			if lb := fmt.Sprint(l.(map[string]any)["label"]); lb == "Company accommodation" || lb == "Accommodation recovery" {
				t.Fatalf("payslip line %q", lb)
			}
		}
	}
	if !reflect.DeepEqual(before.sheet, after.sheet) || bfCol(after.sheet, "Company accommodation") >= 0 || bfCol(after.sheet, "Meal and accommodation") < 0 {
		t.Fatalf("payroll sheet changed:\n%v\n%v", before.sheet, after.sheet)
	}
}
