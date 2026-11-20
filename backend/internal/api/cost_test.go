package api_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// costNodes lists every node of a cost tree, depth first.
func costNodes(n map[string]any) []map[string]any {
	out := []map[string]any{n}
	if ch, ok := n["children"].([]any); ok {
		for _, x := range ch {
			out = append(out, costNodes(x.(map[string]any))...)
		}
	}
	return out
}

func costNode(t *testing.T, tree map[string]any, kind, id string) map[string]any {
	t.Helper()
	for _, n := range costNodes(tree) {
		if n["kind"] == kind && str(n["id"]) == id {
			return n
		}
	}
	t.Fatalf("no %s %s in the tree", kind, id)
	return nil
}

// The cost console: a published month's totals are its payroll rows plus accommodation, the minimum-wage top-ups are
// no cost, the over-budget flag, the snapshot frozen at publishing that never changes, and the property scope.
func TestCostConsole(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	const ym = "2026-09"
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.02 }
	signAll(c, "EMP-1002")

	people := hstPeople(t, st, 40, "EMP-1002")
	a := people[0]
	signAll(c, a)
	loc := str(apiDoc(st, "employee", bson.M{"_id": a})["location_id"])
	cluster := str(apiDoc(st, "location", bson.M{"_id": loc})["cluster_id"])
	var b, otherLoc string
	for _, p := range people[1:] {
		if l := str(apiDoc(st, "employee", bson.M{"_id": p})["location_id"]); l != loc {
			b, otherLoc = p, l
			break
		}
	}
	if b == "" {
		t.Fatal("no second property among the sample staff")
	}
	otherCluster := str(apiDoc(st, "location", bson.M{"_id": otherLoc})["cluster_id"])

	// The month is published with two pay rows; A has a 2,000 meal deduction (a minimum-wage top-up).
	if _, err := st.DB.C("payroll_month").Upsert(ctx, bson.M{"ym": ym}, bson.M{"status": "Published"}, nil); err != nil {
		t.Fatal(err)
	}
	type pr struct {
		id, loc                                              string
		earn, bonus, erPF, admin, edli, esi, lwf, grat, meal float64
	}
	rows := []pr{{a, loc, 30000, 500, 1800, 75, 75, 0, 20, 700, 2000}, {b, otherLoc, 18000, 0, 1800, 0, 0, 585, 0, 0, 0}}
	sum := 0.0
	for _, r := range rows {
		data := fmt.Sprintf(`{"employeeId":%q,"name":"Test %s","locId":%q,"totalEarn":%v,"net":1,"r":{"bonus":%v,"erPF":%v,"admin":%v,"edli":%v,"erESI":%v,"lwfEr":%v,"grat":%v,"meal":%v}}`,
			r.id, r.id, r.loc, r.earn, r.bonus, r.erPF, r.admin, r.edli, r.esi, r.lwf, r.grat, r.meal)
		if _, err := st.DB.C("payroll_row").Insert(ctx, bson.M{"ym": ym, "employee_id": r.id, "net": 1.0, "data": data}); err != nil {
			t.Fatal(err)
		}
		sum += r.earn + r.erPF + r.admin + r.edli + r.esi + r.lwf + r.grat - r.meal
	}
	// Accommodation: A stays all September in a 12,000 unit; an empty 6,000 unit serves the other property.
	u1 := hstUnit(t, c, "Cost Flat 1", cluster, "Any", 2, loc)
	hstOK(t, c, "POST", "/api/hst/units/"+u1+"/leases", map[string]any{"start": "2026-01-01", "rent": 12000, "noticeDays": 30})
	if s, r := hstIn(c, a, u1, "2026-09-01", 0); s != 200 {
		t.Fatalf("check-in: %d %v", s, r)
	}
	u2 := hstUnit(t, c, "Cost Flat 2", otherCluster, "Any", 2, otherLoc)
	hstOK(t, c, "POST", "/api/hst/units/"+u2+"/leases", map[string]any{"start": "2026-01-01", "rent": 6000, "noticeDays": 30})
	hc, err := api.HstMonthCost(ctx, st.DB, ym)
	if err != nil || hc.Shares[a] != 12000 || hc.Empty[otherLoc] != 6000 {
		t.Fatalf("accommodation: %v %+v", err, hc)
	}
	accom := 0.0
	for _, v := range hc.Shares {
		accom += v
	}
	for _, v := range hc.Empty {
		accom += v
	}

	if s, _ := c.do("GET", "/api/cost?ym="+ym, a, "Employee", nil); s != 403 {
		t.Fatalf("an employee sees the cost console: %d", s)
	}
	r := hstOK(t, c, "GET", "/api/cost?ym="+ym, nil)
	meta, tree := r["meta"].(map[string]any), r["tree"].(map[string]any)
	if meta["mode"] != "actual" || meta["frozen"] == true {
		t.Fatalf("meta before freezing: %v", meta)
	}
	if !near(num(tree["total"]), sum+accom) || num(tree["people"]) != 2 || !near(num(tree["topUp"]), 2000) {
		t.Fatalf("company total %v people %v top-ups %v, want %v", tree["total"], tree["people"], tree["topUp"], sum+accom)
	}
	// A's 2,000 meal top-up is not cost: salary (with the bonus) is the earnings less 2,000.
	pp := hstOK(t, c, "GET", "/api/cost/people?ym="+ym+"&kind=property&id="+loc, nil)["people"].([]any)
	var la map[string]any
	for _, x := range pp {
		if x.(map[string]any)["employeeId"] == a {
			la = x.(map[string]any)
		}
	}
	if la == nil || !near(num(la["salary"])+num(la["bonus"]), 30000-2000) || !near(num(la["total"]), 30000-2000+1800+75+75+20+700+12000) {
		t.Fatalf("A's cost line: %v", la)
	}

	// Over budget: 10,000 for A's property is far below its cost; a cluster figure replaces the total of its properties.
	hstOK(t, c, "PUT", "/api/cost/budget/location/"+loc, map[string]any{"cost": 10000})
	pn := costNode(t, hstOK(t, c, "GET", "/api/cost?ym="+ym, nil)["tree"].(map[string]any), "property", loc)
	if pn["over"] != true || num(pn["budget"]) != 10000 || num(pn["pct"]) <= 100 || num(pn["diff"]) >= 0 {
		t.Fatalf("over budget: %v", pn)
	}
	hstOK(t, c, "PUT", "/api/cost/budget/location/"+loc, map[string]any{"cost": 900000})
	hstOK(t, c, "PUT", "/api/cost/budget/cluster/"+cluster, map[string]any{"cost": 5000, "headcount": 3})
	tree = hstOK(t, c, "GET", "/api/cost?ym="+ym, nil)["tree"].(map[string]any)
	if pn := costNode(t, tree, "property", loc); pn["over"] == true {
		t.Fatalf("within budget: %v", pn)
	}
	if cn := costNode(t, tree, "cluster", cluster); num(cn["budget"]) != 5000 || cn["over"] != true || num(cn["hcBudget"]) != 3 {
		t.Fatalf("cluster's own budget: %v", cn)
	}
	if apiCount(st, "audit_log", bson.M{"action": "Budget changed (cost console)"}) != 3 {
		t.Fatal("budget changes not audited")
	}

	// Frozen at publishing (here by the sweep, as for a month published before the console): later changes to pay
	// rows, budgets and accommodation do not move the month.
	if _, err := api.CostFreezeSweep(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	r = hstOK(t, c, "GET", "/api/cost?ym="+ym, nil)
	frozen := r["tree"].(map[string]any)
	if m := r["meta"].(map[string]any); m["frozen"] != true || m["frozenAt"] == nil {
		t.Fatalf("not frozen: %v", m)
	}
	if _, err := st.DB.C("payroll_row").UpdateMany(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"data.totalEarn": 99999.0}}); err != nil {
		t.Fatal(err)
	}
	hstOK(t, c, "PUT", "/api/cost/budget/location/"+loc, map[string]any{"cost": 1})
	hstOK(t, c, "PUT", "/api/hst/units/"+u1+"/costs/"+ym, map[string]any{"electricity": 5000})
	after := hstOK(t, c, "GET", "/api/cost?ym="+ym, nil)["tree"].(map[string]any)
	if fmt.Sprint(after) != fmt.Sprint(frozen) {
		t.Fatalf("the frozen month changed:\n%v\n%v", frozen, after)
	}
	if n, err := api.CostFreezeSweep(ctx, st.DB); err != nil || apiCount(st, store.CostSnapshotColl, bson.M{"ym": ym}) != 1 || n != 0 {
		t.Fatalf("frozen twice: %v %d", err, n)
	}

	// The open month is a projection from the salary structures (no attendance), with a trend and the headcount plan.
	if pr := hstOK(t, c, "GET", "/api/cost", nil); pr["meta"].(map[string]any)["mode"] != "projected" || num(pr["tree"].(map[string]any)["people"]) == 0 ||
		num(pr["tree"].(map[string]any)["salary"]) <= 0 {
		t.Fatalf("open month: %v", pr["meta"])
	}
	if tr := hstOK(t, c, "GET", "/api/cost/trend?kind=cluster&id="+cluster, nil)["months"].([]any); len(tr) != 6 {
		t.Fatalf("trend: %v", tr)
	}
	hstOK(t, c, "GET", "/api/cost/headcount", nil)

	// Scope: a Property Manager mapped to A's property sees that property only, and no per-person salaries.
	var pm string
	for _, p := range people[1:] {
		if p != b && apiCount(st, "user_role_map", bson.M{"employee_id": p}) == 0 {
			pm = p
			break
		}
	}
	if pm == "" {
		t.Fatal("no unmapped employee for the scope check")
	}
	if _, err := st.DB.C("user_role_map").Insert(ctx, bson.M{"employee_id": pm, "role_name": "Property Manager", "access": "locs", "location_ids": []string{loc}, "created_by": "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("role_right").Upsert(ctx, bson.M{"role_name": "Property Manager", "right_id": "cost.view"}, bson.M{"level": int16(1)}, nil); err != nil {
		t.Fatal(err)
	}
	signAll(c, pm)
	s, sr := c.do("GET", "/api/cost?ym="+ym, pm, "Property Manager", nil)
	if s != 200 {
		t.Fatalf("scoped view: %d %v", s, sr)
	}
	props := []string{}
	for _, n := range costNodes(sr["tree"].(map[string]any)) {
		if n["kind"] == "property" {
			props = append(props, str(n["id"]))
		}
	}
	if len(props) != 1 || props[0] != loc {
		t.Fatalf("a property-scoped user sees %v, want only %s", props, loc)
	}
	if s, _ := c.do("GET", "/api/cost/people?ym="+ym, pm, "Property Manager", nil); s != 403 {
		t.Fatalf("per-person cost without salary access: %d", s)
	}
	if s, _ := c.do("PUT", "/api/cost/budget/location/"+loc, pm, "Property Manager", map[string]any{"cost": 5}); s != 403 {
		t.Fatalf("budget changed without cost.budget: %d", s)
	}
}
