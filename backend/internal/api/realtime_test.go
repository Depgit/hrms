package api_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Realtime India attendance portal connector (16 Nov 2026): matching portal codes to employees, storing swipes as
// punches, the Attendance code bulk column, and the setup endpoints. The portal itself is faked (internal/realtime
// tests the client against a fake portal).

func TestRealtimeMatch(t *testing.T) {
	emps := []store.Row{{"id": "EMP-1050"}, {"id": "EMP-1020", "attendance_code": "00007777"}, {"id": "EMP-2000", "attendance_code": "1060"},
		{"id": "EMP-1061"}, {"id": "EMP-3001", "attendance_code": "55"}, {"id": "EMP-3002", "attendance_code": "055"}}
	// "Match by Employee ID number" off (the default): only Attendance codes match.
	for _, c := range [][3]string{{"1050", "00001050", ""}, {"1061", "", ""}, {"7777", "00007777", "EMP-1020"}, {"1060", "", "EMP-2000"}} {
		if got := api.RealtimeMatch(emps, false, c[0], c[1]); got != c[2] {
			t.Errorf("off %s/%s: got %q, want %q", c[0], c[1], got, c[2])
		}
	}
	for _, c := range []struct{ code, card, want string }{
		{"1050", "00001050", "EMP-1050"}, // by the Employee ID's number
		{"00001050", "", "EMP-1050"},     // leading zeros do not matter
		{"7777", "00007777", "EMP-1020"}, // by attendance code
		{"", "7777", "EMP-1020"},         // the card number alone
		{"1020", "00001020", ""},         // EMP-1020 has its own code: its number is not used
		{"1060", "", "EMP-2000"},         // the code wins over EMP-1060's number (there is none anyway)
		{"55", "", ""},                   // two employees share 55: neither is guessed
		{"99999", "00099999", ""},        // nobody
		{"1061", "", "EMP-1061"},
	} {
		if got := api.RealtimeMatch(emps, true, c.code, c.card); got != c.want {
			t.Errorf("%s/%s: got %q, want %q", c.code, c.card, got, c.want)
		}
	}
}

func TestRealtimePortalPull(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	ctx := context.Background()
	day := time.Now().AddDate(0, 0, -3).Format("2006-01-02")

	// Attendance code through Employee master update: a bad code and a shared code are refused.
	_, l := c.do("GET", "/api/bulk", "EMP-1002", "HR Admin", nil)
	for _, x := range l["list"].([]any) {
		if m := x.(map[string]any); (m["key"] == "upd" || m["key"] == "new") && !strings.HasSuffix(strings.Join(toStrs(m["cols"].([]any)), ","), ",Attendance code") {
			t.Fatalf("%s columns: %v", m["key"], m["cols"])
		}
	}
	head := "Employee ID,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Role,Skill,Medical,Accommodation,Reason for change,Effective From,Attendance code\n"
	blank := strings.Repeat(",", 17)
	file := head + "EMP-1020" + blank + "00007777\nEMP-1011" + blank + "77 77\n"
	if _, r := c.sendFile("/api/bulk/upd/apply", "EMP-1002", "HR Admin", "upd.csv", []byte(file)); r["ok"] != 1.0 || !strings.Contains(b1Changes(r, 0), "Attendance code:  → 00007777") ||
		!strings.Contains(b1Errs(r, 1), "letters and digits") {
		t.Fatalf("attendance code: %v", r)
	}
	if apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["attendance_code"] != "00007777" {
		t.Fatal("not stored")
	}
	if _, r := c.sendFile("/api/bulk/upd/check", "EMP-1002", "HR Admin", "upd.csv", []byte(head+"EMP-1011"+blank+"7777\n")); !strings.Contains(b1Errs(r, 0), "already") {
		t.Fatalf("shared code: %v", r)
	}

	// Import employees takes it too.
	nh := "Employee Code,Full Name,Employment Type,Date of Joining,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Role,Skill,Mobile,Email,Date of Birth,Gender,PAN,Aadhaar,UAN,Existing UAN?,ESIC No,Bank Name,Account Number,IFSC,Status,Last Working Day,Opening CL Balance,Opening PL Balance,Opening SL Balance,Attendance code\n"
	base := ",Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,Kitchen,Cooking,,,,,,,,No,,,,,,,,,,"
	if _, r := c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(nh+"RT-1,Code Given"+base+"4444\nRT-2,Code Taken"+base+"7777\n")); r["ok"] != 1.0 ||
		!strings.Contains(b1Errs(r, 1), "already") {
		t.Fatalf("import: %v", r)
	}
	if apiDoc(st, "employee", bson.M{"_id": "RT-1"})["attendance_code"] != "4444" || apiDoc(st, "employee", bson.M{"_id": "RT-2"}) != nil {
		t.Fatal("import: code stored, taken code refused")
	}

	// The portal's report of the day (made-up rows).
	rows := []realtime.Row{
		{EmpCode: "7777", CardNo: "00007777", Name: "Twenty", Date: day, Times: []string{"09:02", "13:00", "18:30"}}, // EMP-1020 by code
		{EmpCode: "1009", CardNo: "00001009", Name: "Nine", Date: day, Times: []string{"10:00", "19:00"}},            // EMP-1009 by number
		{EmpCode: "1011", CardNo: "00001011", Name: "Eleven", Date: day, Times: []string{"22:10", "06:05"}},          // night: next day
		{EmpCode: "1020", CardNo: "00001020", Name: "Not twenty", Date: day, Times: []string{"09:00"}},               // EMP-1020 has a code
		{EmpCode: "99999", CardNo: "00099999", Name: "Nobody", Date: day, Times: []string{"11:00", "12:00"}},
		{EmpCode: "1014", CardNo: "00001014", Name: "Absent", Date: day},
	}
	calls := 0
	undo := api.SetRealtime(realtime.Config{BaseURL: "http://203.0.113.10:83", User: "u", Pass: "p"}, true,
		func(_ context.Context, _ realtime.Config, d time.Time) ([]realtime.Row, error) {
			calls++
			if d.Format("2006-01-02") != day {
				return nil, errors.New("no report")
			}
			return rows, nil
		})
	defer undo()
	// Off by default; this company's Employee IDs are the portal's numbers, so HR turns number matching on.
	if s, g := c.do("GET", "/api/attendance/realtime", "EMP-1002", "HR Admin", nil); s != 200 || g["byId"] != false {
		t.Fatalf("byId default: %v", g)
	}
	if s, g := c.do("PUT", "/api/attendance/realtime", "EMP-1002", "HR Admin", map[string]any{"on": false, "every": 30, "byId": true}); s != 200 || g["byId"] != true {
		t.Fatalf("byId on: %d %v", s, g)
	}
	res, err := app.RealtimeRun(ctx, day, day, "Test")
	if err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true || res["rows"] != 6 || res["withSwipes"] != 5 || res["swipes"] != 10 || res["added"] != 7 || res["matched"] != 3 || res["unmatchedCount"] != 2 {
		t.Fatalf("result: %v", res)
	}
	if l := fmt.Sprint(res["unmatched"]); !strings.Contains(l, "99999") || !strings.Contains(l, "Not twenty") {
		t.Fatalf("unmatched: %s", l)
	}
	if n := apiCount(st, "punch", bson.M{"employee_id": "EMP-1020", "source": "Realtime portal"}); n != 3 {
		t.Fatalf("EMP-1020 punches: %d", n)
	}
	ist := time.FixedZone("IST", 19800)
	next, _ := time.ParseInLocation("2006-01-02 15:04", day+" 06:05", ist)
	if apiCount(st, "punch", bson.M{"employee_id": "EMP-1011", "ts": next.AddDate(0, 0, 1)}) != 1 {
		t.Fatal("the 06:05 swipe after 22:10 is on the next day")
	}
	// The same report again adds nothing.
	if res, _ = app.RealtimeRun(ctx, day, day, "Test"); res["added"] != 0 || res["swipes"] != 10 {
		t.Fatalf("again: %v", res)
	}

	// A day the portal fails on is reported; the others still count.
	prev := time.Now().AddDate(0, 0, -4).Format("2006-01-02")
	if res, _ = app.RealtimeRun(ctx, prev, day, "Test"); res["ok"] != false || res["days"] != 1 || !strings.Contains(fmt.Sprint(res["errors"]), "no report") {
		t.Fatalf("one day failed: %v", res)
	}

	// Setup page: status, settings and who may see them.
	s, g := c.do("GET", "/api/attendance/realtime", "EMP-1002", "HR Admin", nil)
	if s != 200 || g["envSet"] != true || g["host"] != "203.0.113.10:83" || g["on"] != false || g["every"] != 30.0 || g["last"] == nil {
		t.Fatalf("get: %d %v", s, g)
	}
	if strings.Contains(fmt.Sprint(g), "\"p\"") || strings.Contains(fmt.Sprint(g), "User") {
		t.Fatal("the account is never returned")
	}
	if s, _ := c.do("GET", "/api/attendance/realtime", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee: %d", s)
	}
	if s, _ := c.do("PUT", "/api/attendance/realtime", "EMP-1002", "HR Admin", map[string]any{"on": true, "every": 5}); s != 400 {
		t.Fatalf("every 5: %d", s)
	}
	if s, g := c.do("PUT", "/api/attendance/realtime", "EMP-1002", "HR Admin", map[string]any{"on": true, "every": 45}); s != 200 || g["on"] != true || g["every"] != 45.0 {
		t.Fatalf("put: %d %v", s, g)
	}
	if s, r := c.do("POST", "/api/attendance/realtime/pull", "EMP-1002", "HR Admin", map[string]any{"from": "2026-01-01", "to": "2026-03-01"}); s != 400 || !strings.Contains(fmt.Sprint(r), "31 days") {
		t.Fatalf("range: %d %v", s, r)
	}

	// Without the account in the environment: nothing runs, and it cannot be switched on.
	undo2 := api.SetRealtime(realtime.Config{}, false, nil)
	defer undo2()
	calls = 0
	if res, _ = app.RealtimeRun(ctx, day, day, "Test"); calls != 0 || !strings.Contains(fmt.Sprint(res["errors"]), "REALTIME_URL") {
		t.Fatalf("no account: %v", res)
	}
	if err := app.RealtimeDue(ctx); err != nil || calls != 0 {
		t.Fatal(err, calls)
	}
	if s, _ := c.do("PUT", "/api/attendance/realtime", "EMP-1002", "HR Admin", map[string]any{"on": true, "every": 30}); s != 400 {
		t.Fatalf("on without account: %d", s)
	}
}
