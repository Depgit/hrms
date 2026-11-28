package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// rdSetup seeds EMP-1020's previous month punched 09:00–18:00 every day and returns helpers: the month as the
// attendance engine settles it, one day of it, and a punch setter.
func rdSetup(t *testing.T, c *client) (ym string, month func() map[string]any, day func(map[string]any, string) map[string]any, punch func(d, in, out string)) {
	t.Helper()
	ctx := context.Background()
	ym = prevYm(today()[:7])
	punch = func(d, in, out string) {
		t.Helper()
		f := bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: d}}
		if in == "" {
			if _, err := c.st.DB.C("attendance_day").Delete(ctx, f); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := c.st.DB.C("attendance_day").Upsert(ctx, f, bson.M{"in_time": in, "out_time": out, "source": "Punch machine", "by_name": "Test seed"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for d := ym + "-01"; d[:7] == ym; d = lwAddDay(d) {
		punch(d, "09:00", "18:00")
	}
	month = func() map[string]any {
		t.Helper()
		s, a := c.do("GET", "/api/attendance/month/EMP-1020?ym="+ym, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatalf("attendance month: %d %v", s, a)
		}
		return a["month"].(map[string]any)
	}
	day = func(mo map[string]any, d string) map[string]any {
		t.Helper()
		for _, v := range mo["days"].([]any) {
			if x := v.(map[string]any); x["date"] == d {
				return x
			}
		}
		t.Fatalf("no day %s", d)
		return nil
	}
	return
}

// rdPolicy changes settings of EMP-1020's work policy (path → value, e.g. "rs.past") and saves it.
func rdPolicy(t *testing.T, c *client, ym string, change map[string]any) {
	t.Helper()
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
	for k, v := range change {
		parts := strings.Split(k, ".")
		m := set
		for _, p := range parts[:len(parts)-1] {
			if m[p] == nil {
				m[p] = map[string]any{}
			}
			m = m[p].(map[string]any)
		}
		m[parts[len(parts)-1]] = v
	}
	if s, r := c.do("PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"name": pol["name"], "settings": set, "leavePlanId": pol["leavePlanId"],
		"calendarId": pol["calendarId"], "calMap": pol["calMap"], "effectiveFrom": today()}); s != 200 {
		t.Fatalf("save policy: %d %v", s, r)
	}
}

// Called to work from the roster (owner, 24 Nov 2026): a shift put on a holiday or a leave cell calls the person to
// work with that shift. A holiday called but not punched stays a holiday with no loss of pay; a casual leave day called
// and punched is Present and its day comes back. A plain shift on the holiday is refused; only att.records at Edit
// may call; a paid month refuses.
func TestRosterCalledToWork(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1009", "EMP-1020"} {
		c.signIn(e)
	}
	ym, month, day, punch := rdSetup(t, c)
	var present []string
	for _, v := range month()["days"].([]any) {
		if x := v.(map[string]any); x["code"] == "P" {
			present = append(present, x["date"].(string))
		}
	}
	if len(present) < 6 {
		t.Fatalf("seeded month has %d present days", len(present))
	}
	hol, cl, hol2 := present[1], present[2], present[3]
	punch(hol, "", "") // nobody came on the holiday
	cals, err := st.DB.C("holiday_calendar").Distinct(ctx, "_id", bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cals {
		for _, d := range []string{hol, hol2} {
			if _, err := st.DB.C("holiday").Insert(ctx, bson.M{"calendar_id": id, "date": d, "name": "Test holiday", "kind": "National", "on_list": true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	pcSeed(t, c, "leave", "EMP-1020", "Approved", map[string]any{"wf": "leave", "days": 1}, map[string]any{"ltype": "Casual leave", "from": cl, "to": cl})
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
	// The roster row: a shift usable at the employee's property, and the cells as the grid shows them.
	loc := str(apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["location_id"])
	row := func(d string) (map[string]any, map[string]any) {
		t.Helper()
		s, g := c.do("GET", "/api/roster?location="+loc+"&week="+d, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatalf("roster: %d %v", s, g)
		}
		for _, v := range g["rows"].([]any) {
			if r := v.(map[string]any); r["id"] == "EMP-1020" {
				for i, x := range g["days"].([]any) {
					if x == d {
						return r, r["cells"].([]any)[i].(map[string]any)
					}
				}
			}
		}
		t.Fatalf("EMP-1020 not on the roster of %s", d)
		return nil, nil
	}
	r, cell := row(hol)
	if cell["code"] != "PH" || cell["holiday"] != true {
		t.Fatalf("holiday cell: %v", cell)
	}
	shift := ""
	for _, x := range r["codes"].([]any) {
		if x != "WO" && x != "PH" {
			shift = x.(string)
			break
		}
	}
	if shift == "" {
		t.Fatalf("no shift at the property: %v", r["codes"])
	}
	cellCall := func(emp, role, d string, call bool, reason string) (int, map[string]any) {
		return c.do("POST", "/api/roster/cell", emp, role, map[string]any{"employeeId": "EMP-1020", "date": d, "code": shift, "why": "Event or banquet", "call": call, "reason": reason})
	}
	// A plain shift on the holiday is refused: it would make it an ordinary working day.
	if s, x := cellCall("EMP-1002", "HR Admin", hol, false, ""); s != 400 || !strings.Contains(fmt.Sprint(x), "is a holiday") {
		t.Fatalf("plain shift on a holiday: %d %v", s, x)
	}
	if s, x := cellCall("EMP-1002", "HR Admin", cl, false, ""); s != 400 || !strings.Contains(fmt.Sprint(x), "approved leave") {
		t.Fatalf("plain shift on leave: %d %v", s, x)
	}
	// Who may call: att.records at Edit, not a line manager or the employee.
	if s, x := cellCall("EMP-1009", "Line Manager", hol, true, "Banquet"); s != 403 {
		t.Fatalf("line manager calling: %d %v", s, x)
	}
	if s, x := cellCall("EMP-1020", "Employee", hol, true, "Banquet"); s != 403 {
		t.Fatalf("employee calling: %d %v", s, x)
	}
	if s, x := cellCall("EMP-1002", "HR Admin", hol, true, " "); s != 400 || !strings.Contains(fmt.Sprint(x), "reason") {
		t.Fatalf("no reason: %d %v", s, x)
	}
	for _, d := range []string{hol, cl} {
		if s, x := cellCall("EMP-1002", "HR Admin", d, true, "Banquet"); s != 200 {
			t.Fatalf("call on %s: %d %v", d, s, x)
		}
	}
	if w := apiDoc(st, "work_call", bson.M{"employee_id": "EMP-1020", "date": hol}); w["shift_code"] != shift || w["kind"] != "holiday" {
		t.Fatalf("call stored: %v", w)
	}
	mo := month()
	// The holiday called but not punched: still a holiday, no loss of pay.
	if x := day(mo, hol); x["status"] != "Holiday" || x["lop"].(float64) != 0 || x["called"] != nil ||
		!strings.Contains(fmt.Sprint(x["note"]), "Called to work, no punch: the holiday stands") {
		t.Fatalf("holiday called, no punch: %v", x)
	}
	// The leave day called and punched: Present, the day back in the balance.
	if x := day(mo, cl); x["status"] != "Present" || x["called"] != true || x["returned"] != 1.0 || x["lop"].(float64) != 0 {
		t.Fatalf("leave day called and worked: %v", x)
	}
	if b := balance(); b != bal0+1 {
		t.Fatalf("casual leave balance %v, want %v + 1", b, bal0)
	}
	if _, x := row(hol); x["call"] == nil || x["call"].(map[string]any)["shift"] != shift || x["call"].(map[string]any)["active"] != true {
		t.Fatalf("called cell: %v", x)
	}
	// A roster change on a called day waits for the call to be removed; removing it puts the holiday back.
	if s, x := c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": hol, "code": "WO", "why": "Other"}); s != 400 ||
		!strings.Contains(fmt.Sprint(x), "Remove the call") {
		t.Fatalf("roster change on a called day: %d %v", s, x)
	}
	_, x := row(hol)
	if s, r := c.do("DELETE", "/api/attendance/called/"+x["call"].(map[string]any)["id"].(string), "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("remove: %d %v", s, r)
	}
	if _, x := row(hol); x["call"] != nil || x["code"] != "PH" {
		t.Fatalf("cell after the call is removed: %v", x)
	}
	// A paid month refuses a call.
	if _, err := st.DB.C("payroll_month").Upsert(ctx, bson.M{"ym": ym}, bson.M{"status": "Published"}, nil); err != nil {
		t.Fatal(err)
	}
	if s, x := cellCall("EMP-1002", "HR Admin", hol2, true, "Banquet"); s != 400 || !strings.Contains(fmt.Sprint(x), "already paid") {
		t.Fatalf("call in a paid month: %d %v", s, x)
	}
}

// Other time for this day (owner, 24 Nov 2026): a roster cell gets its own timings, here a split day 06:00–10:00 +
// 15:00–19:00 (8 h). The engine treats the day like a shift with those timings: the gap between the parts never
// counts, the late mark runs from the first part's start. A second part that starts before the first ends is
// refused; a past date follows the roster's lock.
func TestRosterOtherTime(t *testing.T) {
	c, _, _ := setupFull(t)
	c.signIn("EMP-1002")
	ym, month, day, punch := rdSetup(t, c)
	var present []string
	for _, v := range month()["days"].([]any) {
		if x := v.(map[string]any); x["code"] == "P" {
			present = append(present, x["date"].(string))
		}
	}
	if len(present) < 6 {
		t.Fatalf("seeded month has %d present days", len(present))
	}
	a, b, d3 := present[1], present[3], present[5]
	split := map[string]any{"start": "06:00", "end": "10:00", "start2": "15:00", "end2": "19:00"}
	set := func(d string, tm map[string]any) (int, map[string]any) {
		return c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": d, "code": "OT", "time": tm, "why": "Event or banquet"})
	}
	// The roster locks past dates by default: the change is refused like any other.
	rdPolicy(t, c, ym, map[string]any{"rs.past": "lock"})
	if s, x := set(a, split); s != 400 || !strings.Contains(fmt.Sprint(x), "locked") {
		t.Fatalf("past date with the lock: %d %v", s, x)
	}
	rdPolicy(t, c, ym, map[string]any{"rs.past": "free", "fullH": 8, "halfH": 4, "grace": 15, "att.minRest": 0})
	if s, x := set(a, map[string]any{"start": "06:00", "end": "10:00", "start2": "09:00", "end2": "19:00"}); s != 400 || !strings.Contains(fmt.Sprint(x), "second part must start after") {
		t.Fatalf("overlapping parts: %d %v", s, x)
	}
	for _, d := range []string{a, b, d3} {
		if s, x := set(d, split); s != 200 {
			t.Fatalf("other time on %s: %d %v", d, s, x)
		}
	}
	punch(a, "06:00", "17:00")  // 11 h in to out, 5 h of it in the gap: 6 h
	punch(b, "06:30", "19:30")  // 13 h less the 5 h gap: 8 h, 30 minutes after the first part's start
	punch(d3, "06:00", "19:00") // 8 h, on time
	mo := month()
	for d, want := range map[string]string{a: "Half day", b: "Late", d3: "Present"} {
		if x := day(mo, d); x["status"] != want {
			t.Errorf("%s: %v, want %s", d, x, want)
		}
	}
	_, g := c.do("GET", "/api/attendance?ym="+ym+"&employee=EMP-1020", "EMP-1002", "HR Admin", nil)
	for _, v := range g["days"].([]any) {
		if x := v.(map[string]any); x["date"] == a && x["planned"] != "OT" {
			t.Fatalf("planned code on %s: %v", a, x)
		}
	}
}

// One employee's own deviation allowance (owner, 24 Nov 2026): the policy allows 2 late check-ins of up to 120 minutes
// a month and EMP-1020 is late by 60 minutes on four days. Their own rule (On, 120 minutes, 4 times) wins: no late
// mark on any of the four. "Use the policy's rule" falls back to 2; Off gives four late marks. Only the work policy
// right may give it; On without both numbers and a date in a paid month are refused.
func TestDevOwnRules(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1009", "EMP-1020"} {
		c.signIn(e)
	}
	ym, month, _, punch := rdSetup(t, c)
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
	rdPolicy(t, c, ym, map[string]any{"att.lateN": 0, "att.lateAct": "half", "dev.lateIn": map[string]any{"on": true, "mins": 120, "n": 2, "act": "none"}})
	statuses := func() []string {
		byDate := map[string]string{}
		for _, v := range month()["days"].([]any) {
			x := v.(map[string]any)
			byDate[x["date"].(string)] = x["status"].(string)
		}
		out := []string{}
		for _, d := range late {
			out = append(out, byDate[d])
		}
		return out
	}
	if got := strings.Join(statuses(), ","); got != "Present,Present,Late,Late" {
		t.Fatalf("policy rule: %s", got)
	}
	save := func(emp, role string, lateIn map[string]any, from string) (int, map[string]any) {
		return c.do("POST", "/api/attendance/dev-allow/EMP-1020", emp, role, map[string]any{"effectiveFrom": from, "lateIn": lateIn})
	}
	on := map[string]any{"use": "on", "mins": 120, "n": 4, "act": "none"}
	if s, x := save("EMP-1009", "Line Manager", on, ym+"-01"); s != 403 {
		t.Fatalf("line manager: %d %v", s, x)
	}
	if s, x := save("EMP-1020", "Employee", on, ym+"-01"); s != 403 {
		t.Fatalf("employee: %d %v", s, x)
	}
	if s, x := save("EMP-1002", "HR Admin", map[string]any{"use": "on", "mins": 120}, ym+"-01"); s != 400 || !strings.Contains(fmt.Sprint(x), "Late check-in is on") {
		t.Fatalf("on without times: %d %v", s, x)
	}
	if s, x := save("EMP-1002", "HR Admin", on, ym+"-01"); s != 200 || x["saved"] != true {
		t.Fatalf("save own rule: %d %v", s, x)
	}
	if got := strings.Join(statuses(), ","); got != "Present,Present,Present,Present" {
		t.Fatalf("own rule over the policy's: %s", got)
	}
	if s, x := save("EMP-1002", "HR Admin", map[string]any{"use": "policy"}, ym+"-01"); s != 200 {
		t.Fatalf("back to the policy's: %d %v", s, x)
	}
	if got := strings.Join(statuses(), ","); got != "Present,Present,Late,Late" {
		t.Fatalf("use the policy's rule: %s", got)
	}
	if s, x := save("EMP-1002", "HR Admin", map[string]any{"use": "off"}, ym+"-01"); s != 200 {
		t.Fatalf("off: %d %v", s, x)
	}
	if got := strings.Join(statuses(), ","); got != "Late,Late,Late,Late" {
		t.Fatalf("off for the employee: %s", got)
	}
	if s, g := c.do("GET", "/api/attendance/dev-allow/EMP-1020", "EMP-1002", "HR Admin", nil); s != 200 || len(g["versions"].([]any)) != 1 {
		t.Fatalf("versions: %d %v", s, g)
	}
	if n := apiCount(st, "audit_log", bson.M{"entity": "emp_dev_allow", "entity_id": "EMP-1020"}); n != 3 {
		t.Fatalf("audit entries %d, want 3", n)
	}
	if _, err := st.DB.C("payroll_month").Upsert(ctx, bson.M{"ym": ym}, bson.M{"status": "Published"}, nil); err != nil {
		t.Fatal(err)
	}
	if s, x := save("EMP-1002", "HR Admin", on, late[1]); s != 400 || !strings.Contains(fmt.Sprint(x), "already paid") {
		t.Fatalf("a date in a paid month: %d %v", s, x)
	}
}
