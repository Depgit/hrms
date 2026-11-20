package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
)

// Variable pay of a designed structure is due when a cycle counted from the joining month is complete, and a payout
// needs a variable target (owner, 18 Nov 2026).
func TestVarPayoutCycleAndTarget(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011")
	open := pdOpenYm(t, c)
	comps := payCompExample()
	comps[4]["cycle"] = 3
	s, made := c.do("POST", "/api/comp/structures/designed", "EMP-1002", "HR Admin", map[string]any{"name": "Quarterly", "comps": comps})
	if s != 200 {
		t.Fatalf("designed: %v", made)
	}
	// Joined two months before the open month: the first quarter completes in the open month.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"doj": ymAdd(open, -2) + "-01"}}); err != nil {
		t.Fatal(err)
	}
	if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1002", "HR Admin", map[string]any{"structureId": made["id"], "fromDate": open + "-01", "ids": []string{"EMP-1020"}}); s != 200 {
		t.Fatalf("map: %v", r)
	}
	pay := func(ym string, amount float64) (int, map[string]any) {
		return c.do("PUT", "/api/pay/variable", "EMP-1011", "Payroll", map[string]any{"ym": ym, "employeeId": "EMP-1020", "code": "perf", "amount": amount})
	}
	// No target: refused, with a clear message; the list shows it.
	if s, r := pay(open, 5000); s != 400 || !strings.Contains(fmt.Sprint(r), "no variable target") {
		t.Fatalf("payout without a target: %d %v", s, r)
	}
	_, vl := c.do("GET", "/api/pay/variable?ym="+open, "EMP-1011", "Payroll", nil)
	if !strings.Contains(fmt.Sprint(vl["rows"]), "noTarget:true") || fmt.Sprint(vl["due"]) != "0" {
		t.Fatalf("list without a target: %v", vl)
	}
	if s, r := c.do("PUT", "/api/pay/variable/target", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "fromMonth": open, "total": 3000, "reason": "Test"}); s != 200 {
		t.Fatalf("target: %v", r)
	}
	if s, r := pay(open, 9000); s != 200 {
		t.Fatalf("payout with a target: %v", r)
	}
	// The next month is not due; the message names the next payout, three months on.
	if s, r := pay(ymAdd(open, 1), 100); s != 400 || !strings.Contains(fmt.Sprint(r), "next payout") {
		t.Fatalf("payout off the cycle: %d %v", s, r)
	}
}

// The offer budget check compares the location's total monthly cost (salary plus the employer's PF, ESI and other
// contributions) with its cost budget, not the gross.
func TestBudgetCheckTotalCost(t *testing.T) {
	_, _, st := setupFull(t)
	ctx := context.Background()
	q := st.DB
	e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": "EMP-1020"})
	loc := fmt.Sprint(e["location_id"])
	cost, err := api.LocMonthlyCost(ctx, q, loc)
	if err != nil || cost <= 0 {
		t.Fatalf("location cost %v: %v", cost, err)
	}
	hire := api.OfferMonthlyCost(ctx, q, "Permanent", loc, 20000)
	if hire <= 20000 {
		t.Fatalf("a ₹20,000 hire costs %v: the employer's contributions are missing", hire)
	}
	set := func(budget float64) {
		if _, err := q.C("location").Update(ctx, bson.M{"_id": loc}, bson.M{"$set": bson.M{"payroll_budget": budget, "headcount_budget": int64(0)}}); err != nil {
			t.Fatal(err)
		}
	}
	set(cost + 20000 + 1) // room for the gross, not for the total cost
	if over := api.BudgetOver(ctx, q, "Permanent", loc, 20000); !strings.Contains(strings.Join(over, ","), "location cost budget") {
		t.Fatalf("over the cost budget not caught: %v (cost %v, hire %v)", over, cost, hire)
	}
	set(cost + hire + 1)
	if over := api.BudgetOver(ctx, q, "Permanent", loc, 20000); strings.Contains(strings.Join(over, ","), "location cost budget") {
		t.Fatalf("within the cost budget: %v", over)
	}
}
