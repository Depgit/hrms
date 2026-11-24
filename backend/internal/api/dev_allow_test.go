package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Deviation allowances through the attendance month (owner, 24 Nov 2026): EMP-1020's policy allows no late marks
// (each one is half a day of loss of pay). Four days of last month start 60 minutes late (a full day worked). With
// "Late check-in: 120 minutes, 2 times a month" on, the first two are Present and no late mark, the other two are
// late marks: the loss of pay drops by one day. A rule switched on without its numbers is refused.
func TestDevAllowLateIn(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	ym := prevYm(today()[:7])
	s, a := c.do("GET", "/api/attendance?ym="+ym+"&employee=EMP-1020", "EMP-1002", "HR Admin", nil)
	if s != 200 || a["shift"] == nil {
		t.Fatalf("employee month: %d %v", s, a)
	}
	sh := a["shift"].(map[string]any)
	hm := func(x string, add int) string {
		var h, m int
		fmt.Sscanf(x, "%d:%d", &h, &m)
		t := (h*60 + m + add + 1440) % 1440
		return fmt.Sprintf("%02d:%02d", t/60, t%60)
	}
	punch := func(d, in, out string) {
		if _, err := st.DB.C("attendance_day").Upsert(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: d}},
			bson.M{"in_time": in, "out_time": out, "source": "Punch machine", "by_name": "Test seed"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	start, end := sh["start"].(string), sh["end"].(string)
	for d := ym + "-01"; d[:7] == ym; d = lwAddDay(d) {
		punch(d, start, end)
	}
	month := func() map[string]any {
		s, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatalf("attendance month: %d %v", s, a)
		}
		return a["month"].(map[string]any)
	}
	var late []string
	for _, v := range month()["days"].([]any) {
		if x := v.(map[string]any); x["code"] == "P" && len(late) < 4 {
			late = append(late, x["date"].(string))
		}
	}
	if len(late) < 4 {
		t.Fatalf("seeded month has %d present days", len(late))
	}
	for _, d := range late {
		punch(d, hm(start, 60), hm(end, 60))
	}
	// The employee's policy.
	_, mo := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
	_, l := c.do("GET", "/api/policies", "EMP-1002", "HR Admin", nil)
	var pid string
	for _, v := range l["list"].([]any) {
		if p := v.(map[string]any); p["name"] == mo["policy"] {
			pid = p["id"].(string)
		}
	}
	_, pg := c.do("GET", "/api/policies/"+pid, "EMP-1002", "HR Admin", nil)
	pol := pg["policy"].(map[string]any)
	set := pol["settings"].(map[string]any)
	set["att"].(map[string]any)["lateN"], set["att"].(map[string]any)["lateAct"] = 0, "half"
	save := func(rule map[string]any) (int, map[string]any) {
		set["dev"] = map[string]any{"lateIn": rule}
		return c.do("PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"name": pol["name"], "settings": set, "leavePlanId": pol["leavePlanId"],
			"calendarId": pol["calendarId"], "calMap": pol["calMap"], "effectiveFrom": today()})
	}
	if s, r := save(map[string]any{"on": true, "mins": 120, "n": 0}); s != 400 || !strings.Contains(fmt.Sprint(r), "Late check-in is on") {
		t.Fatalf("on without times: %d %v", s, r)
	}
	if s, r := save(map[string]any{"on": false}); s != 200 {
		t.Fatalf("save without the rule: %d %v", s, r)
	}
	off := month()
	if off["lateOver"].(float64) != 4 {
		t.Fatalf("rule off: %v late marks over the limit, want 4", off["lateOver"])
	}
	if s, r := save(map[string]any{"on": true, "mins": 120, "n": 2, "act": "none"}); s != 200 {
		t.Fatalf("save the rule: %d %v", s, r)
	}
	on := month()
	byDate := map[string]map[string]any{}
	for _, v := range on["days"].([]any) {
		x := v.(map[string]any)
		byDate[x["date"].(string)] = x
	}
	for i, d := range late {
		want := map[bool]string{true: "Present", false: "Late"}[i < 2]
		if x := byDate[d]; x["status"] != want {
			t.Errorf("%s: %v, want %s", d, x, want)
		}
	}
	if !strings.Contains(fmt.Sprint(byDate[late[1]]["note"]), "(2 of 2)") || !strings.Contains(fmt.Sprint(byDate[late[2]]["note"]), "3rd this month (allowed 2)") {
		t.Errorf("notes: %v / %v", byDate[late[1]]["note"], byDate[late[2]]["note"])
	}
	if on["lateOver"].(float64) != 2 || on["lop"].(float64) != off["lop"].(float64)-1 {
		t.Fatalf("rule on: over %v, LOP %v (off %v); want 2 over and one day less", on["lateOver"], on["lop"], off["lop"])
	}
	// The admin is informed of the 3rd late check-in (a sweep that runs on that day), once, whatever the alert settings.
	at, _ := time.ParseInLocation("2006-01-02 15:04", late[2]+" 23:00", time.FixedZone("IST", 19800))
	if res, err := app.AttDevRun(ctx, at); err != nil || res.Informed < 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	key := "att-dev:late-in:" + late[2] + ":EMP-1020"
	n := apiCount(st, "notice", bson.M{"ref": key, "to_employee_id": "EMP-1002"})
	if r := apiDoc(st, "notice", bson.M{"ref": key}); n != 1 || !strings.Contains(fmt.Sprint(r["text"]), "late check-in 1 h on ") ||
		!strings.Contains(fmt.Sprint(r["text"]), "3rd this month (allowed 2)") {
		t.Fatalf("HR Admin notices %d: %v", n, r)
	}
	if _, err := app.AttDevRun(ctx, at); err != nil || apiCount(st, "notice", bson.M{"ref": key, "to_employee_id": "EMP-1002"}) != 1 {
		t.Fatalf("informed twice: %v", err)
	}
}

// The action of a day beyond the allowance (owner, 24 Nov 2026), through the API. A rule switched on without an action
// is refused, on the policy and on the employee's own rules. "Late check-in 120 min, once a month" with Half day until
// regularized: the 2nd late check-in is "Half day until regularized" (LOP 0.5, no late mark), offered for
// regularization and listed in the attendance issues; outside the regularization window the request is refused, with
// an exception it goes through and, approved, the day follows the corrected times (Present). The employee's own rule
// with Absent overrides the policy's action; "use the policy's rule" takes the policy's action back. The bell says
// the action.
func TestDevAllowAction(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1020")
	ym, month, day, punch := rdSetup(t, c)
	s, a := c.do("GET", "/api/attendance?ym="+ym+"&employee=EMP-1020", "EMP-1002", "HR Admin", nil)
	if s != 200 || a["shift"] == nil {
		t.Fatalf("employee month: %d %v", s, a)
	}
	sh := a["shift"].(map[string]any)
	hm := func(x string, add int) string {
		var h, m int
		fmt.Sscanf(x, "%d:%d", &h, &m)
		t := (h*60 + m + add + 1440) % 1440
		return fmt.Sprintf("%02d:%02d", t/60, t%60)
	}
	start, end := sh["start"].(string), sh["end"].(string)
	for d := ym + "-01"; d[:7] == ym; d = lwAddDay(d) {
		punch(d, start, end)
	}
	var late []string
	last := "" // the month's last working day: beyond the allowance too, and near the top of the issues list (latest first)
	for _, v := range month()["days"].([]any) {
		if x := v.(map[string]any); x["code"] == "P" {
			if len(late) < 3 {
				late = append(late, x["date"].(string))
			} else {
				last = x["date"].(string)
			}
		}
	}
	if len(late) < 3 || last == "" {
		t.Fatalf("seeded month has %d present days", len(late))
	}
	for _, d := range append(late, last) {
		punch(d, hm(start, 60), hm(end, 60)) // 60 min late, a full day worked
	}
	// The policy: no late marks allowed; the rule without an action is refused.
	_, mo := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
	_, l := c.do("GET", "/api/policies", "EMP-1002", "HR Admin", nil)
	var pid string
	for _, v := range l["list"].([]any) {
		if p := v.(map[string]any); p["name"] == mo["policy"] {
			pid = p["id"].(string)
		}
	}
	_, pg := c.do("GET", "/api/policies/"+pid, "EMP-1002", "HR Admin", nil)
	pol := pg["policy"].(map[string]any)
	set := pol["settings"].(map[string]any)
	set["att"].(map[string]any)["lateN"], set["att"].(map[string]any)["lateAct"] = 0, "half"
	save := func(rule map[string]any) (int, map[string]any) {
		set["dev"] = map[string]any{"lateIn": rule}
		return c.do("PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"name": pol["name"], "settings": set, "leavePlanId": pol["leavePlanId"],
			"calendarId": pol["calendarId"], "calMap": pol["calMap"], "effectiveFrom": today()})
	}
	if s, r := save(map[string]any{"on": true, "mins": 120, "n": 1}); s != 400 || !strings.Contains(fmt.Sprint(r), "Late check-in is on: choose the action") {
		t.Fatalf("on without an action: %d %v", s, r)
	}
	if s, r := save(map[string]any{"on": true, "mins": 120, "n": 1, "act": "later"}); s != 400 {
		t.Fatalf("an action not in the list: %d %v", s, r)
	}
	if s, r := save(map[string]any{"on": true, "mins": 120, "n": 1, "act": "half"}); s != 200 {
		t.Fatalf("save the rule: %d %v", s, r)
	}
	m := month()
	if x := day(m, late[0]); x["status"] != "Present" {
		t.Errorf("allowed day: %v", x)
	}
	for _, d := range append(late[1:], last) {
		if x := day(m, d); x["status"] != "Half day until regularized" || x["lop"] != 0.5 || x["reg"] != "half" ||
			!strings.Contains(fmt.Sprint(x["note"]), "beyond the allowance → Half day until regularized") {
			t.Errorf("beyond the allowance: %v", x)
		}
	}
	if m["lateOver"].(float64) != 0 || m["counts"].(map[string]any)["Late"] != nil {
		t.Errorf("no late mark on top of the action: over %v, counts %v", m["lateOver"], m["counts"])
	}
	// Offered for regularization: the attendance issues list it.
	_, iss := c.do("GET", "/api/attendance/issues?ym="+ym, "EMP-1002", "HR Admin", nil)
	listed := false
	for _, v := range iss["list"].([]any) {
		x := v.(map[string]any)
		listed = listed || (x["id"] == "EMP-1020" && x["date"] == last && x["status"] == "Half day until regularized")
	}
	if !listed {
		t.Errorf("not in the attendance issues: %v", iss)
	}
	// The regularization window applies; with an exception the request goes through, and approved it clears the day.
	reg := map[string]any{"wf": "regularize", "payload": map[string]string{"date": late[1], "rtype": "All Other", "in": start, "out": end, "reason": "Forgot to punch"}}
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", reg); s != 400 || !strings.Contains(fmt.Sprint(r), "Regularization is allowed within") {
		t.Fatalf("outside the window: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]string{"employeeId": "EMP-1020", "kind": "regularize", "from": late[1], "to": late[1],
		"until": lwAddDay(today()), "reason": "Test"}); s != 200 {
		t.Fatalf("exception: %d %v", s, r)
	}
	s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", reg)
	if s != 200 {
		t.Fatalf("raise: %d %v", s, r)
	}
	c.approveAll(t, r["id"].(string))
	m = month()
	if x := day(m, late[1]); x["status"] != "Present" || x["lop"] != nil && x["lop"] != 0.0 || x["reg"] != nil || x["src"] != "Regularized" {
		t.Errorf("regularized: %v", x)
	}
	if x := day(m, late[2]); x["status"] != "Half day until regularized" {
		t.Errorf("the other day beyond: %v", x)
	}
	// The employee's own rule: On without an action refused; with Absent it overrides the policy's Half day.
	own := func(lateIn map[string]any) (int, map[string]any) {
		return c.do("POST", "/api/attendance/dev-allow/EMP-1020", "EMP-1002", "HR Admin", map[string]any{"effectiveFrom": ym + "-01", "lateIn": lateIn})
	}
	if s, x := own(map[string]any{"use": "on", "mins": 120, "n": 1}); s != 400 || !strings.Contains(fmt.Sprint(x), "choose the action") {
		t.Fatalf("own rule without an action: %d %v", s, x)
	}
	if s, x := own(map[string]any{"use": "on", "mins": 120, "n": 1, "act": "absent"}); s != 200 {
		t.Fatalf("own rule: %d %v", s, x)
	}
	if x := day(month(), late[2]); x["status"] != "Absent until regularized" || x["lop"] != 1.0 || x["reg"] != "absent" {
		t.Errorf("own action over the policy's: %v", x)
	}
	if s, x := own(map[string]any{"use": "policy"}); s != 200 {
		t.Fatalf("back to the policy's: %d %v", s, x)
	}
	if x := day(month(), late[2]); x["status"] != "Half day until regularized" {
		t.Errorf("the policy's action: %v", x)
	}
	// The bell says the action.
	at, _ := time.ParseInLocation("2006-01-02 15:04", late[2]+" 23:00", time.FixedZone("IST", 19800))
	if _, err := app.AttDevRun(ctx, at); err != nil {
		t.Fatal(err)
	}
	if r := apiDoc(st, "notice", bson.M{"ref": "att-dev:late-in:" + late[2] + ":EMP-1020"}); !strings.Contains(fmt.Sprint(r["text"]), "→ Half day until regularized") {
		t.Errorf("bell: %v", r)
	}
}
