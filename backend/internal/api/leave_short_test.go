package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A half day (owner, 20 Nov 2026) counts 0.5 against the balance; a half day of Loss of pay is 0.5 LOP with the other
// half worked.
func TestLeaveHalfDayBalanceAndLop(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1020")
	c.signIn("EMP-1002")
	d := time.Now()
	for d = d.AddDate(0, 0, 1); d.Weekday() != time.Monday; d = d.AddDate(0, 0, 1) {
	}
	day := d.AddDate(0, 0, 1).Format("2006-01-02")
	_, before := c.do("GET", "/api/leave/balances/me?on="+day, "EMP-1020", "Employee", nil)
	pend := func(x map[string]any) float64 {
		for _, v := range x["types"].([]any) {
			if r := v.(map[string]any); r["type"] == "Casual leave" {
				return r["balance"].(map[string]any)["pending"].(float64)
			}
		}
		t.Fatalf("no Casual leave in %v", x)
		return 0
	}
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave",
		"payload": map[string]any{"ltype": "Casual leave", "from": day, "to": day, "half": true, "halfOf": "second"}})
	if s != 200 {
		t.Fatalf("half day: %d %v", s, r)
	}
	_, after := c.do("GET", "/api/leave/balances/me?on="+day, "EMP-1020", "Employee", nil)
	if got := pend(after) - pend(before); got != 0.5 {
		t.Fatalf("half day waiting %v, want 0.5", got)
	}
	req, _ := st.DB.C("approval_request").FindOne(context.Background(), bson.M{"_id": r["id"]})
	if p := req["payload"].(map[string]any); p["halfOf"] != "second" {
		t.Fatalf("half chosen: %v", p)
	}
	// Unpaid half day, the other half worked 14:00 to 18:00: 0.5 LOP.
	ym := today()[:7]
	lop := ym + "-02"
	if lop >= today() {
		ym = prevYm(ym)
		lop = ym + "-02"
	}
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 0.5}, map[string]any{"ltype": "Loss of pay", "from": lop, "to": lop, "half": true, "halfOf": "first"})
	if _, err := st.DB.C("attendance_day").Upsert(context.Background(), bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: lop}},
		bson.M{"in_time": "14:00", "out_time": "18:00", "source": "Punch machine", "by_name": "Test seed"}, nil); err != nil {
		t.Fatal(err)
	}
	_, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
	for _, v := range a["month"].(map[string]any)["days"].([]any) {
		if x := v.(map[string]any); x["date"] == lop {
			if x["lop"] != 0.5 || !strings.Contains(x["status"].(string), "first half") {
				t.Fatalf("unpaid half day: %v", x)
			}
			return
		}
	}
	t.Fatalf("%s not in the month", lop)
}

// Short leave (owner, 20 Nov 2026): over 2 hours and beyond 2 a month are refused; an approved one at the start of
// the shift takes away the late mark and keeps the day Present.
func TestShortLeave(t *testing.T) {
	c, _, st := setupFull(t)
	c.signIn("EMP-1020")
	c.signIn("EMP-1002")
	ctx := context.Background()
	d := time.Now().AddDate(0, 0, -1)
	for d.Weekday() == time.Sunday || d.Weekday() == time.Saturday {
		d = d.AddDate(0, 0, -1)
	}
	day := d.Format("2006-01-02")
	if _, err := st.DB.C("roster_entry").Upsert(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: day}}, bson.M{"code": "G", "by_name": "test"}, nil); err != nil {
		t.Fatal(err)
	}
	raise := func(from, to string) (int, map[string]any) {
		return c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave",
			"payload": map[string]any{"short": true, "from": day, "fromTime": from, "toTime": to, "note": "Bank visit"}})
	}
	if s, r := raise("09:00", "11:30"); s != 400 || !strings.Contains(fmt.Sprint(r), "at most 2 hours") {
		t.Fatalf("2h30 short leave: %d %v", s, r)
	}
	s, r := raise("09:00", "11:00")
	if s != 200 {
		t.Fatalf("2h short leave: %d %v", s, r)
	}
	first := r["id"]
	if s, r := raise("15:00", "16:00"); s != 200 {
		t.Fatalf("second short leave: %d %v", s, r)
	}
	if s, r := raise("16:00", "17:00"); s != 400 || !strings.Contains(fmt.Sprint(r), "2 times a month") {
		t.Fatalf("third short leave in the month: %d %v", s, r)
	}
	req, _ := st.DB.C("approval_request").FindOne(ctx, bson.M{"_id": first})
	if p, f := req["payload"].(map[string]any), req["facts"].(map[string]any); p["ltype"] != "Short leave" || p["to"] != day || (f["days"] != nil && f["days"] != 0.0) {
		t.Fatalf("stored short leave: %v %v", p, f)
	}
	if _, err := st.DB.C("attendance_day").Upsert(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: day}},
		bson.M{"in_time": "11:00", "out_time": "18:05", "source": "Punch machine", "by_name": "Test seed"}, nil); err != nil {
		t.Fatal(err)
	}
	status := func() map[string]any {
		_, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+day[:7], "EMP-1002", "HR Admin", nil)
		for _, v := range a["month"].(map[string]any)["days"].([]any) {
			if x := v.(map[string]any); x["date"] == day {
				return x
			}
		}
		t.Fatalf("%s not in the month", day)
		return nil
	}
	if x := status(); x["status"] == "Present" {
		t.Fatalf("waiting short leave already excuses the day: %v", x)
	}
	if _, err := st.DB.C("approval_request").Update(ctx, bson.M{"_id": first}, bson.M{"$set": bson.M{"status": "Approved"}}); err != nil {
		t.Fatal(err)
	}
	if x := status(); x["status"] != "Present" || x["lop"] != 0.0 || x["leave"] != "Short leave 2h" {
		t.Fatalf("approved short leave: %v", x)
	}
}

// The offer's Reimbursement (owner, 20 Nov 2026) goes to the employee at joining; outside the gross since 25 Nov, so
// only a negative amount is refused.
func TestOfferReimbursementToEmployee(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012")
	si := selectCandidate(t, c, shortlistBody(st, "Riya", "Sharma", "9876501234"))
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatalf("aadhaar: %v", r)
	}
	d := offerData(st, "Riya", "Sharma", "9876501234") // gross 26,000
	d["reimb"] = -5
	if _, r := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": d, "candidate": si}); !strings.Contains(fmt.Sprint(r["errors"]), "zero or more") {
		t.Fatalf("a negative amount not refused: %v", r["errors"])
	}
	d["reimb"] = 7000 // outside the gross (25 Nov 2026): more than Basic and HRA leave is fine
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": d, "candidate": si})
	if s != 200 {
		t.Fatalf("offer: %v", r)
	}
	ui3ApproveAll(t, c, r["request"].(map[string]any)["id"].(string))
	oi := r["id"].(string)
	for _, step := range []string{"release", "accept"} {
		if s, r := c.do("POST", "/api/offers/"+oi+"/"+step, "EMP-1004", "HRBP", nil); s != 200 {
			t.Fatalf("%s: %v", step, r)
		}
	}
	s, r = c.do("POST", "/api/candidates/"+si+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()})
	if s != 200 {
		t.Fatalf("joined: %v", r)
	}
	p, _ := st.DB.C("employee_pay").FindOne(context.Background(), bson.M{"employee_id": r["id"]})
	if a, _ := p["comp_amts"].(map[string]any); a == nil || a["reimb"] != 7000.0 {
		t.Fatalf("employee_pay of %v: %v", r["id"], p)
	}
}
