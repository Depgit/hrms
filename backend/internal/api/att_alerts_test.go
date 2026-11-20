package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Attendance alerts (att_alerts.go): each alert type, once per person and day, the stale punch feed and a silent
// location holding absences back, and leave, week off and manager-marked people left out.
func TestAttendanceAlerts(t *testing.T) {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	ctx := context.Background()
	ist := time.FixedZone("IST", 5*3600+1800)

	// A past Tuesday to Saturday, so the day and the one before are working days we control with the roster.
	day := time.Now().In(ist).AddDate(0, 0, -3)
	for day.Weekday() == time.Sunday || day.Weekday() == time.Monday {
		day = day.AddDate(0, 0, -1)
	}
	d, y := day.Format("2006-01-02"), day.AddDate(0, 0, -1).Format("2006-01-02")
	now := time.Date(day.Year(), day.Month(), day.Day(), 11, 30, 0, 0, ist)

	ins := func(coll string, doc bson.M) {
		t.Helper()
		if _, err := st.DB.C(coll).Insert(ctx, doc); err != nil {
			t.Fatalf("%s: %v", coll, err)
		}
	}
	roster := func(id, date, code string) {
		ins("roster_entry", bson.M{"employee_id": id, "date": date, "code": code, "by_name": "Test"})
	}
	punch := func(id, date, in, out string) {
		doc := bson.M{"employee_id": id, "date": date, "in_time": in, "source": "Punch machine", "by_name": "Test"}
		if out != "" {
			doc["out_time"] = out
		}
		ins("attendance_day", doc)
	}
	// Location beaf9df4… (hub): EMP-1038 on time, EMP-1020 late, EMP-1026 no punch, EMP-1032 on leave, EMP-1044 week
	// off, EMP-1014 manager marked. Location b48e4643…: nobody punches, EMP-1027 has no punch.
	for _, id := range []string{"EMP-1038", "EMP-1020", "EMP-1026", "EMP-1032", "EMP-1027", "EMP-1014"} {
		roster(id, d, "G")
		roster(id, y, "G")
	}
	roster("EMP-1044", d, "WO")
	punch("EMP-1038", d, "08:55", "")
	punch("EMP-1020", d, "10:12", "")
	punch("EMP-1038", y, "09:00", "") // no out: missing out punch
	punch("EMP-1020", y, "09:00", "18:00")
	ins("approval_request", bson.M{"_id": "LV-ALERT-1", "wf": "leave", "raised_by": "EMP-1032", "employee_id": "EMP-1032", "flow": "leave", "status": "Approved",
		"facts": bson.M{"days": 1}, "chain": bson.A{}, "payload": bson.M{"ltype": "Casual leave", "from": d, "to": d}})
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1014"}, bson.M{"$set": bson.M{"attendance_mode": "Manager marked"}}); err != nil {
		t.Fatal(err)
	}

	// Settings: HR Admin only; all off at first.
	if s, g := c.do("GET", "/api/attendance/alerts", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee sees settings: %d %v", s, g)
	}
	s, g := c.do("GET", "/api/attendance/alerts", "EMP-1002", "HR Admin", nil)
	if set := g["settings"].(map[string]any); s != 200 || set["missingOn"] != false || set["digestAt"] != "11:00" || set["notInAfter"] != 30.0 {
		t.Fatalf("defaults: %d %v", s, g)
	}
	if res, err := app.AttAlertsRun(ctx, now); err != nil || len(res.Sent) != 0 {
		t.Fatalf("all off sends nothing: %v %v", res, err)
	}
	on := map[string]any{"missingOn": true, "missingAt": "09:00", "lateOn": true, "notInOn": true, "notInAfter": 30, "notInManager": true,
		"digestOn": true, "digestAt": "11:00", "cutoffOn": true, "cutoffDays": 5, "cutoffAt": "10:00"}
	bad := map[string]any{}
	for k, v := range on {
		bad[k] = v
	}
	bad["digestAt"] = "11"
	if s, _ := c.do("PUT", "/api/attendance/alerts", "EMP-1002", "HR Admin", bad); s != 400 {
		t.Fatal("a bad time is refused")
	}
	if s, _ := c.do("PUT", "/api/attendance/alerts", "EMP-1020", "Employee", on); s != 403 {
		t.Fatal("an employee cannot change them")
	}
	if s, g := c.do("PUT", "/api/attendance/alerts", "EMP-1002", "HR Admin", on); s != 200 || g["settings"].(map[string]any)["lateOn"] != true {
		t.Fatalf("save: %d %v", s, g)
	}
	// The cut-off reminder is due today: the days before the end of the payroll window (calendar month here).
	end := time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, ist)
	cut := int(end.Sub(time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, ist)).Hours() / 24)
	set := bson.M{}
	for k, v := range on {
		set[k] = v
	}
	set["cutoffDays"] = cut
	if _, err := st.DB.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"att_alerts": set}}); err != nil {
		t.Fatal(err)
	}

	notice := func(kind, date, who string) store.Row {
		return apiDoc(st, "notice", bson.M{"ref": "att-alert:" + kind + ":" + date + ":" + who})
	}
	feed := func(at time.Time, ok bool) {
		t.Helper()
		if _, err := st.DB.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"realtime_pull": bson.M{"on": true, "every": 30,
			"last": bson.M{"at": at, "ok": ok}}}}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. The Realtime pull last ran 2 hours ago (interval 30 minutes): no absence is claimed.
	feed(now.Add(-2*time.Hour), true)
	res, err := app.AttAlertsRun(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped["punch feed stale: no Realtime portal pull since "+now.Add(-2*time.Hour).In(ist).Format("15:04")] == 0 {
		t.Fatalf("stale skipped: %v", res.Skipped)
	}
	if notice("missing", y, "EMP-1038") != nil || notice("notin", d, "EMP-1026") != nil {
		t.Fatal("no missing-punch or not-in alert while the feed is stale")
	}
	late := notice("late", d, "EMP-1020") // a punch that arrived is still reported
	if late == nil || !strings.Contains(str(late["text"]), "In at 10:12 today, 1h 12m late (shift 09:00") || late["kind"] != "attendance" || late["link"] != "/me?tab=att" {
		t.Fatalf("late: %v", late)
	}
	dg := notice("digest", d, "EMP-1009")
	if dg == nil || !strings.Contains(str(dg["text"]), "Not punched in: not listed, the no Realtime portal pull since") ||
		!strings.Contains(str(dg["text"]), "Late 1: Karan Khan (in 10:12, 1h 12m late)") || !strings.Contains(str(dg["text"]), "Present 1: Karan Joshi (08:55)") ||
		!strings.Contains(str(dg["text"]), "On leave 1: Divya Patil (Casual leave)") {
		t.Fatalf("digest while stale: %v", dg)
	}
	// The cut-off reminder does not depend on today's feed: EMP-1026 has no punch on any day of the window.
	co := notice("cutoff", d[:7], "EMP-1026")
	if co == nil || !strings.Contains(str(co["text"]), "will be loss of pay unless you regularise") || !strings.Contains(str(co["text"]), "(no punch)") {
		t.Fatalf("cut-off: %v", co)
	}
	if notice("cutoff", d[:7], "EMP-1014") != nil {
		t.Fatal("manager marked: no cut-off reminder")
	}

	// 2. A failed pull is stale too.
	feed(now.Add(-5*time.Minute), false)
	if res, _ = app.AttAlertsRun(ctx, now); res.Skipped["punch feed stale: the last Realtime portal pull failed"] == 0 {
		t.Fatalf("failed pull: %v", res.Skipped)
	}

	// 3. A fresh pull: missing punch, not punched in (with the manager), and a silent location held back.
	feed(now.Add(-5*time.Minute), true)
	if res, err = app.AttAlertsRun(ctx, now); err != nil {
		t.Fatal(err)
	}
	mp := notice("missing", y, "EMP-1038")
	if mp == nil || !strings.Contains(str(mp["text"]), "Missing out punch on ") || !strings.Contains(str(mp["text"]), "(in 09:00, no out). Regularise it by ") {
		t.Fatalf("missing punch: %v", mp)
	}
	if notice("missing", y, "EMP-1020") != nil {
		t.Fatal("a complete day is not reported")
	}
	ni := notice("notin", d, "EMP-1026")
	if ni == nil || str(ni["to_employee_id"]) != "EMP-1026" || !strings.Contains(str(ni["text"]), "No punch-in yet today (shift 09:00–18:00)") {
		t.Fatalf("not in: %v", ni)
	}
	mg := apiDoc(st, "notice", bson.M{"to_employee_id": "EMP-1009", "ref": bson.M{"$regex": "^att-alert:notin-mgr:" + d + ":EMP-1009:"}})
	if mg == nil || !strings.Contains(str(mg["text"]), "Rahul Verma (EMP-1026, shift 09:00)") || mg["view"] != "Line Manager" {
		t.Fatalf("manager: %v", mg)
	}
	for _, id := range []string{"EMP-1032", "EMP-1044", "EMP-1014", "EMP-1038", "EMP-1020"} {
		if notice("notin", d, id) != nil {
			t.Fatalf("%s: leave, week off, manager marked or punched: no not-in alert", id)
		}
	}
	if notice("notin", d, "EMP-1027") != nil {
		t.Fatal("nobody has punched at EMP-1027's location: not called absent")
	}
	silent := false
	for k := range res.Skipped {
		silent = silent || (strings.HasPrefix(k, "no punches at ") && strings.HasSuffix(k, "yet (machine may be offline)"))
	}
	if !silent {
		t.Fatalf("silent location recorded: %v", res.Skipped)
	}

	// 4. Once only: the next sweep sends nothing new.
	if res, err = app.AttAlertsRun(ctx, now.Add(5*time.Minute)); err != nil || len(res.Sent) != 0 {
		t.Fatalf("again: %v %v", res.Sent, err)
	}
	if n, _ := st.DB.C("notice").Count(ctx, bson.M{"ref": "att-alert:late:" + d + ":EMP-1020"}); n != 1 {
		t.Fatalf("late sent %d times", n)
	}

	// 5. The card: today's counts. (The sweeps ran on a past date, so GET shows them only on that day; check the stored totals.)
	last := apiDoc(st, "company_setting", bson.M{"_id": 1})["att_alerts_last"].(map[string]any)
	sent := last["sent"].(map[string]any)
	if fmt.Sprint(sent["late"]) == "0" || sent["notin"] == nil || sent["digest"] == nil || last["day"] != d || last["cutoff_done"] != d[:7] {
		t.Fatalf("totals: %v", last)
	}

	// 6. A second server's sweep while one holds the lease does nothing.
	if _, err := st.DB.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"att_alerts_lease": now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if res, _ = app.AttAlertsRun(ctx, now.Add(10*time.Minute)); res.Note != "" || len(res.Sent) != 0 {
		t.Fatalf("leased: %v", res)
	}

	// 7. Send a test to me.
	if s, g := c.do("POST", "/api/attendance/alerts/test", "EMP-1002", "HR Admin", map[string]any{}); s != 200 || g["sent"] != true {
		t.Fatalf("test: %d %v", s, g)
	}
	if apiDoc(st, "notice", bson.M{"to_employee_id": "EMP-1002", "kind": "attendance", "ref": bson.M{"$regex": "^att-alert:test:"}}) == nil {
		t.Fatal("test message in the bell")
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
