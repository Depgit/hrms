package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Called to work on a holiday or leave day (owner, 24 Nov 2026), end to end on last month: HR Admin calls EMP-1020
// to work on a day of casual leave, a day of loss-of-pay leave and a holiday, each punched 09:00–18:00. The days turn
// Present; the casual leave comes back in the balance, the loss of pay goes, the holiday adds none. Only att.records at
// Edit may call (not the employee, not a line manager); a working day is refused; a paid month refuses adding and
// removing.
func TestCalledToWork(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1009", "EMP-1020"} {
		c.signIn(e)
	}
	ym := prevYm(today()[:7])
	for d := ym + "-01"; d[:7] == ym; d = lwAddDay(d) {
		if _, err := st.DB.C("attendance_day").Upsert(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: d}},
			bson.M{"in_time": "09:00", "out_time": "18:00", "source": "Punch machine", "by_name": "Test seed"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	month := func() map[string]any {
		s, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatalf("attendance month: %d %v", s, a)
		}
		return a["month"].(map[string]any)
	}
	day := func(mo map[string]any, d string) map[string]any {
		for _, v := range mo["days"].([]any) {
			if x := v.(map[string]any); x["date"] == d {
				return x
			}
		}
		t.Fatalf("no day %s", d)
		return nil
	}
	var present []string
	for _, v := range month()["days"].([]any) {
		if x := v.(map[string]any); x["code"] == "P" {
			present = append(present, x["date"].(string))
		}
	}
	if len(present) < 6 {
		t.Fatalf("seeded month has %d present days", len(present))
	}
	cl, lop, hol, work := present[1], present[2], present[3], present[4]
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 1}, map[string]any{"ltype": "Casual leave", "from": cl, "to": cl})
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 1}, map[string]any{"ltype": "Loss of pay", "from": lop, "to": lop})
	cals, err := st.DB.C("holiday_calendar").Distinct(ctx, "_id", bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cals {
		if _, err := st.DB.C("holiday").Insert(ctx, bson.M{"calendar_id": id, "date": hol, "name": "Test holiday", "kind": "National", "on_list": true}); err != nil {
			t.Fatal(err)
		}
	}
	before := month()
	if day(before, cl)["code"] != "L" || day(before, lop)["code"] != "LOP" || day(before, hol)["status"] != "Holiday" {
		t.Fatalf("seeded days: %v / %v / %v", day(before, cl), day(before, lop), day(before, hol))
	}
	balance := func() float64 {
		_, b := c.do("GET", "/api/leave/balances/me?on="+cl, "EMP-1020", "Employee", nil)
		for _, v := range b["types"].([]any) {
			if r := v.(map[string]any); r["type"] == "Casual leave" {
				return r["balance"].(map[string]any)["bal"].(float64)
			}
		}
		t.Fatalf("no casual leave balance: %v", b)
		return 0
	}
	bal0 := balance()

	call := func(emp, role, date, reason string) (int, map[string]any) {
		return c.do("POST", "/api/attendance/called", emp, role, map[string]any{"employeeIds": []string{"EMP-1020"}, "date": date, "reason": reason})
	}
	// Who may call: att.records at Edit (HR Admin), not the employee or a line manager (View only).
	if s, r := call("EMP-1020", "Employee", cl, "Inventory count"); s != 403 {
		t.Fatalf("employee calling: %d %v", s, r)
	}
	if s, r := call("EMP-1009", "Line Manager", cl, "Inventory count"); s != 403 {
		t.Fatalf("line manager calling: %d %v", s, r)
	}
	if s, r := call("EMP-1002", "HR Admin", cl, " "); s != 400 {
		t.Fatalf("no reason: %d %v", s, r)
	}
	if s, r := call("EMP-1002", "HR Admin", work, "Inventory count"); s != 400 || !strings.Contains(fmt.Sprint(r), "not a holiday or a day of approved leave") {
		t.Fatalf("a working day: %d %v", s, r)
	}
	for _, d := range []string{cl, lop, hol} {
		if s, r := call("EMP-1002", "HR Admin", d, "Inventory count"); s != 200 {
			t.Fatalf("call on %s: %d %v", d, s, r)
		}
	}
	if s, r := call("EMP-1002", "HR Admin", cl, "Again"); s != 409 {
		t.Fatalf("called twice: %d %v", s, r)
	}
	after := month()
	for _, d := range []string{cl, lop, hol} {
		if x := day(after, d); x["status"] != "Present" || x["called"] != true || x["lop"].(float64) != 0 {
			t.Fatalf("called day %s: %v", d, x)
		}
	}
	if day(after, cl)["returned"] != 1.0 || day(after, hol)["returned"] != nil {
		t.Fatalf("returned: %v / %v", day(after, cl), day(after, hol))
	}
	if got, want := after["lop"].(float64), before["lop"].(float64)-1; got != want {
		t.Fatalf("month loss of pay %v, want %v (the loss-of-pay day worked)", got, want)
	}
	if b := balance(); b != bal0+1 {
		t.Fatalf("casual leave balance %v, want %v + 1", b, bal0)
	}
	_, l := c.do("GET", "/api/attendance/called?ym="+ym, "EMP-1002", "HR Admin", nil)
	if rows := l["rows"].([]any); len(rows) != 3 || rows[0].(map[string]any)["result"] != "Worked" {
		t.Fatalf("list: %v", l)
	}
	// The leave request's history: called, then worked with the day returned (the sweep notes it once).
	req := apiDoc(st, "approval_request", bson.M{"employee_id": "EMP-1020", "payload.from": cl})
	if res, err := app.AttDevRun(ctx, time.Now()); err != nil || res.Noted != 2 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if res, _ := app.AttDevRun(ctx, time.Now()); res.Noted != 0 {
		t.Fatalf("noted twice: %+v", res)
	}
	if n := apiCount(st, "approval_history", bson.M{"request_id": req["id"], "action": bson.M{"$in": bson.A{"Called to work", "Worked on a leave day"}}}); n != 2 {
		t.Fatalf("history lines %d, want 2", n)
	}

	// Removable while the month is open; a paid month refuses both adding and removing.
	_, l = c.do("GET", "/api/attendance/called?ym="+ym, "EMP-1002", "HR Admin", nil)
	ids := map[string]string{}
	for _, v := range l["rows"].([]any) {
		x := v.(map[string]any)
		ids[x["date"].(string)] = x["id"].(string)
	}
	if s, r := c.do("DELETE", "/api/attendance/called/"+ids[hol], "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("remove: %d %v", s, r)
	}
	if x := day(month(), hol); x["status"] != "Holiday" || x["called"] != nil {
		t.Fatalf("holiday after the call is removed: %v", x)
	}
	if _, err := st.DB.C("payroll_month").Upsert(ctx, bson.M{"ym": ym}, bson.M{"status": "Published"}, nil); err != nil {
		t.Fatal(err)
	}
	if s, r := call("EMP-1002", "HR Admin", hol, "Inventory count"); s != 400 || !strings.Contains(fmt.Sprint(r), "already paid") {
		t.Fatalf("call in a paid month: %d %v", s, r)
	}
	if s, r := c.do("DELETE", "/api/attendance/called/"+ids[cl], "EMP-1002", "HR Admin", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "already paid") {
		t.Fatalf("remove in a paid month: %d %v", s, r)
	}
}
