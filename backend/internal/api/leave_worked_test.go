package api_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Earn by working days (owner, 24 Nov 2026; evening: paid days, credited the day after the Nth, active employees
// only), end to end: the employee's leave plan gets the rule "every 7 paid days: Casual leave 1, Privilege leave 1"
// (Casual leave otherwise upfront, 100 a year: no upfront credit may come); a type in two rules is refused. Last month
// punched every day (week offs and holidays too), one day of approved casual leave, one of loss of pay and one short
// day. The month's paid days are its days less the loss of pay: present, late, paid leave, week offs and holidays 1, a
// half day 0.5, loss of pay 0, as payroll's paid days. Counted up to yesterday; nothing after the last working day; a
// joiner not active yet earns nothing.
func TestLeaveByDaysWorked(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1020")
	c.signIn("EMP-1002")
	_, mine := c.do("GET", "/api/leave/balances/me", "EMP-1020", "Employee", nil)
	_, all := c.do("GET", "/api/leave-plans", "EMP-1002", "HR Admin", nil)
	var plan map[string]any
	for _, v := range all["plans"].([]any) {
		if p := v.(map[string]any)["plan"].(map[string]any); p["name"] == mine["plan"] {
			plan = p
		}
	}
	if plan == nil {
		t.Fatalf("plan %v not listed", mine["plan"])
	}
	put := func(earn []any) (int, map[string]any) {
		lv := plan["lv"].(map[string]any)
		lv["earn"] = earn
		return c.do("PUT", "/api/leave-plans/"+plan["id"].(string), "EMP-1002", "HR Admin", map[string]any{"name": plan["name"], "description": plan["description"],
			"lv": lv, "lrx": plan["lrx"], "types": plan["types"], "effectiveFrom": today()})
	}
	cl := plan["types"].(map[string]any)["Casual leave"].(map[string]any)
	cl["credit"], cl["days"] = "upfront", 100
	rule := func(days float64, types ...string) map[string]any {
		r := []any{}
		for _, t := range types {
			r = append(r, map[string]any{"type": t, "days": 1})
		}
		return map[string]any{"days": days, "types": r}
	}
	if s, r := put([]any{rule(7, "Casual leave"), rule(10, "Casual leave")}); s != 400 || !strings.Contains(fmt.Sprint(r), "can be in only one rule") {
		t.Fatalf("a type in two rules: %d %v", s, r)
	}
	if s, r := put([]any{rule(7, "Casual leave", "Privilege leave")}); s != 200 {
		t.Fatalf("save the rule: %d %v", s, r)
	}
	ym := prevYm(today()[:7])
	punch := func(d, in, out string) {
		if _, err := st.DB.C("attendance_day").Upsert(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: d}},
			bson.M{"in_time": in, "out_time": out, "source": "Punch machine", "by_name": "Test seed"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for d := ym + "-01"; d[:7] == ym; d = lwAddDay(d) {
		punch(d, "09:00", "18:00")
	}
	month := func() map[string]any {
		s, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatalf("attendance month: %d %v", s, a)
		}
		return a["month"].(map[string]any)
	}
	present := []string{}
	for _, v := range month()["days"].([]any) {
		if x := v.(map[string]any); x["code"] == "P" {
			present = append(present, x["date"].(string))
		}
	}
	if len(present) < 20 {
		t.Fatalf("seeded month has %d present days", len(present))
	}
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 1}, map[string]any{"ltype": "Casual leave", "from": present[0], "to": present[0]})
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 1}, map[string]any{"ltype": "Loss of pay", "from": present[1], "to": present[1]})
	punch(present[2], "09:00", "14:00")
	cnt := map[string]float64{}
	for k, v := range month()["counts"].(map[string]any) {
		cnt[k] = v.(float64)
	}
	if cnt["Week off"]+cnt["Holiday"] == 0 || cnt["Paid leave"] != 1 || cnt["Loss of pay leave"] != 1 {
		t.Fatalf("month counts: %v", cnt)
	}
	mo := month()
	nDays := float64(len(mo["days"].([]any)))
	want := nDays - 1 - 0.5*cnt["Half day"] // the loss of pay day and the half day
	if paid := mo["paidDays"].(float64) + 0.5*mo["lateOver"].(float64); want != paid {
		t.Fatalf("paid days %v, the month's paid days %v (%v)", want, paid, cnt)
	}
	_, b := c.do("GET", "/api/leave/balances/me", "EMP-1020", "Employee", nil)
	var row map[string]any
	for _, v := range b["types"].([]any) {
		if r := v.(map[string]any); r["type"] == "Casual leave" {
			row = r
		}
	}
	if row == nil || !strings.Contains(row["rules"].(string), "1 day for every 7 paid days, up to 100 days a year") {
		t.Fatalf("Casual leave row: %v", row)
	}
	bal := row["balance"].(map[string]any)
	w, _ := bal["worked"].(map[string]any)
	if w == nil {
		t.Fatalf("no days worked on the balance: %v", bal)
	}
	var got map[string]any
	for _, v := range w["months"].([]any) {
		if m := v.(map[string]any); m["month"] == ym {
			got = m
		}
	}
	if got == nil || got["days"] != want {
		t.Fatalf("%s: paid days %v, want %v (%v)", ym, got, want, w)
	}
	// Whole blocks of 7 over the year's paid days up to yesterday, each credited the day after its 7th; nothing upfront.
	total := w["days"].(float64)
	credit := math.Floor(total / 7)
	if w["credit"] != credit || bal["credit"] != credit || w["left"] != total-7*credit || w["through"] != lwAddDays(today(), -1) || len(w["credits"].([]any)) != int(credit) {
		t.Fatalf("credit %v, want %v from %v days (%v)", bal["credit"], credit, total, w)
	}
	casual := func() map[string]any {
		_, b := c.do("GET", "/api/leave/balances/EMP-1020", "EMP-1002", "HR Admin", nil)
		for _, v := range b["types"].([]any) {
			if r := v.(map[string]any); r["type"] == "Casual leave" {
				return r["balance"].(map[string]any)["worked"].(map[string]any)
			}
		}
		t.Fatalf("no Casual leave: %v", b)
		return nil
	}
	// Left on the 10th of last month: nothing counts after the last working day.
	lwd := ym + "-10"
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": lwd}}); err != nil {
		t.Fatal(err)
	}
	if w := casual(); w["through"] != lwd || w["days"].(float64) >= total || w["months"].([]any)[len(w["months"].([]any))-1].(map[string]any)["month"] != ym {
		t.Fatalf("after the last working day: %v", w)
	}
	// Not active yet (Pre-boarding): nothing earned.
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"status": "Pre-boarding", "lwd": nil}}); err != nil {
		t.Fatal(err)
	}
	if w := casual(); w["days"] != 0.0 || w["credit"] != 0.0 || w["through"] != "" {
		t.Fatalf("pre-boarding: %v", w)
	}
}

func lwAddDay(d string) string {
	t, _ := time.Parse("2006-01-02", d)
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}

func lwAddDays(d string, n int) string {
	t, _ := time.Parse("2006-01-02", d)
	return t.AddDate(0, 0, n).Format("2006-01-02")
}
