package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Set shift: shift versions by effective date, per-shift attendance rules, and day records built from the
// punches inside a shift's window (night shifts, Consider shift in, early-in and late-out limits).

func shiftIDOf(t *testing.T, c *client, code string) string {
	t.Helper()
	_, r := c.do("GET", "/api/masters/shift", "EMP-1002", "HR Admin", nil)
	for _, x := range r["list"].([]any) {
		if m := x.(map[string]any); m["code"] == code {
			return m["id"].(string)
		}
	}
	t.Fatalf("no shift %s", code)
	return ""
}

func shiftRow(t *testing.T, c *client, code string) map[string]any {
	t.Helper()
	_, r := c.do("GET", "/api/masters/shift", "EMP-1002", "HR Admin", nil)
	for _, x := range r["list"].([]any) {
		if m := x.(map[string]any); m["code"] == code {
			return m
		}
	}
	return nil
}

// shiftBody is a PUT body from the shift as it stands, with the changes given.
func shiftBody(t *testing.T, c *client, id string, set map[string]any) map[string]any {
	t.Helper()
	s, v := c.do("GET", "/api/shifts/"+id, "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("get shift: %v", v)
	}
	sh := v["shift"].(map[string]any)
	b := map[string]any{"effectiveFrom": today(), "startTime": sh["startTime"], "endTime": sh["endTime"], "breakMinutes": sh["breakMinutes"], "name": sh["name"],
		"locationIds": sh["locationIds"], "rules": map[string]any{}, "note": ""}
	for k, x := range set {
		b[k] = x
	}
	return b
}

func putShift(t *testing.T, c *client, id string, body map[string]any) map[string]any {
	t.Helper()
	s, r := c.do("PUT", "/api/shifts/"+id, "EMP-1002", "HR Admin", body)
	if s != 200 {
		t.Fatalf("put shift: %d %v", s, r)
	}
	return r
}

// monthDay is the day's status in the month register (FinalMonth).
func monthDay(t *testing.T, c *client, emp, d string) map[string]any {
	t.Helper()
	s, r := c.do("GET", "/api/attendance/month/"+emp+"?ym="+d[:7], "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatalf("month: %v", r)
	}
	for _, x := range r["month"].(map[string]any)["days"].([]any) {
		if m := x.(map[string]any); m["date"] == d {
			return m
		}
	}
	t.Fatalf("no day %s", d)
	return nil
}

func rosterOn(t *testing.T, st *store.Store, emp, d, code string) {
	t.Helper()
	if _, err := st.DB.C("roster_entry").Upsert(context.Background(), bson.D{{Key: "employee_id", Value: emp}, {Key: "date", Value: d}},
		bson.M{"code": code}, bson.M{"by_name": "shift test"}); err != nil {
		t.Fatal(err)
	}
}

type stored struct{ in, out, outDate, src string }

func storedDay(t *testing.T, st *store.Store, emp, d string) *stored {
	t.Helper()
	r, err := st.DB.C("attendance_day").FindOne(context.Background(), bson.D{{Key: "employee_id", Value: emp}, {Key: "date", Value: d}})
	if err != nil || r == nil {
		return nil
	}
	var s stored
	s.in, _ = r["in_time"].(string)
	s.out, _ = r["out_time"].(string)
	s.outDate, _ = r["out_date"].(string)
	s.src, _ = r["source"].(string)
	return &s
}

// shiftVersions matches the versions of the shift with this code.
func shiftVersions(t *testing.T, st *store.Store, code string, more ...bson.E) bson.D {
	t.Helper()
	sh, err := st.DB.C("shift").FindOne(context.Background(), bson.D{{Key: "code", Value: code}})
	if err != nil || sh == nil {
		t.Fatalf("no shift %s: %v", code, err)
	}
	return append(bson.D{{Key: "shift_id", Value: sh["id"]}}, more...)
}

// device returns a punch pusher for a new device.
func device(t *testing.T, c *client) func(punches ...[3]string) {
	_, dev := c.do("POST", "/api/devices", "EMP-1002", "HR Admin", map[string]any{"name": "Shift test gate"})
	key := dev["key"].(string)
	return func(punches ...[3]string) {
		t.Helper()
		l := []any{}
		for _, p := range punches {
			l = append(l, map[string]any{"employeeId": p[0], "time": p[1], "direction": p[2]})
		}
		buf, _ := json.Marshal(map[string]any{"punches": l})
		req, _ := http.NewRequest("POST", c.base+"/api/punches", bytes.NewReader(buf))
		req.Header.Set("X-Device-Key", key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if res.StatusCode != 200 || int(num(out["accepted"])) != len(punches) {
			t.Fatalf("push: %d %v", res.StatusCode, out)
		}
	}
}

func dayAgo(n int) string { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }

func TestShiftVersionsAndRules(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	mID := shiftIDOf(t, c, "M") // EMP-1020 works M, 06:00-14:00

	t.Run("view: first version, policy defaults", func(t *testing.T) {
		s, v := c.do("GET", "/api/shifts/"+mID, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatal(v)
		}
		vers := v["versions"].([]any)
		cur := v["current"].(map[string]any)
		pd := v["policyDefaults"].(map[string]any)
		if len(vers) != 1 || cur["effectiveFrom"] != "2000-01-01" || cur["startTime"] != "06:00" || pd["policy"] == "" || pd["text"].(map[string]any)["halfBelow"] == "" {
			t.Fatalf("versions %v current %v defaults %v", vers, cur, pd)
		}
		if r := shiftRow(t, c, "M"); r["effective_from"] != "2000-01-01" || fmt.Sprint(r["rules"]) != "map[]" {
			t.Fatalf("list row %v", r)
		}
	})

	t.Run("rights", func(t *testing.T) {
		body := shiftBody(t, c, mID, nil)
		for _, x := range [][2]string{{"EMP-1020", "Employee"}, {"EMP-1011", "Payroll"}} {
			if s, _ := c.do("GET", "/api/shifts/"+mID, x[0], x[1], nil); s != 403 {
				t.Errorf("%s reads the shift: %d", x[1], s)
			}
			if s, _ := c.do("PUT", "/api/shifts/"+mID, x[0], x[1], body); s != 403 {
				t.Errorf("%s changes the shift: %d", x[1], s)
			}
			if s, _ := c.do("POST", "/api/shifts", x[0], x[1], body); s != 403 {
				t.Errorf("%s adds a shift: %d", x[1], s)
			}
		}
		// Masters: job at view level reads the shift but cannot save it.
		right := bson.D{{Key: "role_name", Value: "Payroll"}, {Key: "right_id", Value: "mas.job"}}
		if _, err := st.DB.C("role_right").UpdateMany(ctx, right, bson.M{"$set": bson.M{"level": 1}}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.C("role_right").Upsert(ctx, right, nil, bson.M{"level": 1}); err != nil {
			t.Fatal(err)
		}
		if s, r := c.do("GET", "/api/shifts/"+mID, "EMP-1011", "Payroll", nil); s != 200 {
			t.Errorf("view right reads: %d %v", s, r)
		}
		if s, _ := c.do("PUT", "/api/shifts/"+mID, "EMP-1011", "Payroll", body); s != 403 {
			t.Errorf("view right saves: %d", s)
		}
	})

	t.Run("validation", func(t *testing.T) {
		bad := []struct {
			set   map[string]any
			field string
			msg   string
		}{
			{map[string]any{"rules": map[string]any{"nextDay": true}}, "nextDay", "end time must be at or before the start"},
			{map[string]any{"startTime": "22:00", "endTime": "06:00", "rules": map[string]any{"nextDay": false}}, "nextDay", "Choose Yes"},
			{map[string]any{"rules": map[string]any{"absentBelow": "6:00", "halfBelow": "5:00"}}, "absentBelow", "must not be above"},
			{map[string]any{"rules": map[string]any{"halfBelow": "9:30"}}, "halfBelow", "from the work policy"},
			{map[string]any{"rules": map[string]any{"halfBelow": "4.30"}}, "halfBelow", "HH:MM"},
			{map[string]any{"rules": map[string]any{"officeHours": "25:00"}}, "officeHours", "at most 24:00"},
			{map[string]any{"rules": map[string]any{"forcedOut": "25:00"}}, "forcedOut", "time of day"},
			{map[string]any{"rules": map[string]any{"weeklyAdj": map[string]any{"on": true}}}, "weeklyAdj", "enter the hours"},
			{map[string]any{"rules": map[string]any{"monthlyAdj": map[string]any{"on": true, "hours": "x"}}}, "monthlyAdj", "HHH:MM"},
			{map[string]any{"rules": map[string]any{"shiftIn": "tomorrow"}}, "shiftIn", "must be one of"},
			{map[string]any{"rules": map[string]any{"basedOn": "mood"}}, "basedOn", "must be one of"},
			{map[string]any{"startTime": "6:00"}, "startTime", "HH:MM"},
			{map[string]any{"breakMinutes": 600}, "breakMinutes", "shorter than the shift"},
			{map[string]any{"effectiveFrom": "2026-13-01"}, "effectiveFrom", "date"},
			{map[string]any{"code": "Q"}, "code", "cannot change"},
			{map[string]any{"name": " "}, "name", "name"},
		}
		for _, b := range bad {
			s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, b.set))
			det, _ := r["details"].(map[string]any)
			if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), b.msg) || det["field"] != b.field {
				t.Errorf("%v: %d %v", b.set, s, r)
			}
		}
		if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"rules": map[string]any{"mystery": 1}})); s != 400 {
			t.Errorf("unknown rule: %d %v", s, r)
		}
		n, _ := st.DB.C("shift_version").Count(ctx, shiftVersions(t, st, "M"))
		if n != 1 {
			t.Fatalf("a refused save wrote %d versions", n)
		}
	})

	t.Run("versions by date and the mirror row", func(t *testing.T) {
		// EMP-1020 works 6 h on two days; a version from the 10th makes 5-6 h a full day.
		d1, d2, from := open+"-05", open+"-12", open+"-10"
		punchDay(t, st, "EMP-1020", d1, "06:00", "12:00")
		punchDay(t, st, "EMP-1020", d2, "06:00", "12:00")
		if monthDay(t, c, "EMP-1020", d2)["status"] != "Half day" {
			t.Fatal("before the change 6 h is a half day")
		}
		r := putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": from, "rules": map[string]any{"halfBelow": "5:00", "fullAbove": "6:00"}, "note": "Short shift test"}))
		if len(r["versions"].([]any)) != 2 || r["current"].(map[string]any)["effectiveFrom"] != from {
			t.Fatalf("versions %v", r["versions"])
		}
		if s1, s2 := monthDay(t, c, "EMP-1020", d1)["status"], monthDay(t, c, "EMP-1020", d2)["status"]; s1 != "Half day" || s2 != "Present" {
			t.Fatalf("the version applies from its date: %s %s", s1, s2)
		}
		row := shiftRow(t, c, "M")
		if row["effective_from"] != from || row["rules"].(map[string]any)["halfBelow"] != "5:00" {
			t.Fatalf("the row mirrors the version in effect today: %v", row)
		}
		var by, note string
		if v, _ := st.DB.C("shift_version").FindOne(ctx, shiftVersions(t, st, "M", bson.E{Key: "effective_from", Value: from})); v != nil {
			by, _ = v["created_by"].(string)
			note, _ = v["note"].(string)
		}
		if !strings.Contains(by, "EMP-1002") || note != "Short shift test" {
			t.Fatalf("version by %q note %q", by, note)
		}
		audits, _ := st.DB.C("audit_log").Count(ctx, bson.D{{Key: "action", Value: "Changed shift"}, {Key: "entity", Value: "shift"}, {Key: "entity_id", Value: mID}})
		if audits != 1 {
			t.Fatalf("audit %d", audits)
		}
		// Saving the same date again replaces that version.
		putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": from, "rules": map[string]any{"halfBelow": "5:00", "fullAbove": "7:00"}}))
		if s := monthDay(t, c, "EMP-1020", d2)["status"]; s != "Half day" {
			t.Fatalf("replaced version, 6 h is short with no adjustment: %s", s)
		}
		// Back to the policy from the 10th.
		putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": from, "rules": map[string]any{}}))
		if fmt.Sprint(shiftRow(t, c, "M")["rules"]) != "map[]" {
			t.Fatal("rules cleared")
		}
	})

	t.Run("a future version moves onto the row in the maintenance sweep", func(t *testing.T) {
		gID := shiftIDOf(t, c, "G")
		tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
		r := putShift(t, c, gID, shiftBody(t, c, gID, map[string]any{"effectiveFrom": tomorrow, "startTime": "10:00", "endTime": "19:00"}))
		if r["current"].(map[string]any)["startTime"] != "09:00" || shiftRow(t, c, "G")["start_time"] != "09:00" || shiftRow(t, c, "G")["next_from"] != tomorrow {
			t.Fatalf("a future version waits: %v %v", r["current"], shiftRow(t, c, "G"))
		}
		// The date arrives.
		if _, err := st.DB.C("shift_version").UpdateMany(ctx, bson.D{{Key: "effective_from", Value: tomorrow}}, bson.M{"$set": bson.M{"effective_from": store.Today()}}); err != nil {
			t.Fatal(err)
		}
		jctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if err := app.StartJobs(jctx); err != nil {
			t.Fatal(err)
		}
		defer app.StopJobs(ctx)
		deadline := time.Now().Add(15 * time.Second)
		for shiftRow(t, c, "G")["start_time"] != "10:00" {
			if time.Now().After(deadline) {
				t.Fatalf("not moved: %v", shiftRow(t, c, "G"))
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	t.Run("locked month", func(t *testing.T) {
		prev := ymAdd(open, -1)
		if _, err := st.DB.C("payroll_month").Upsert(ctx, bson.D{{Key: "ym", Value: prev}}, bson.M{"locked_at": time.Now()}, bson.M{"status": "Open", "locked_by": "test"}); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{prev + "-15", ymAdd(open, -2) + "-20"} {
			s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"effectiveFrom": d, "rules": map[string]any{"halfBelow": "5:00"}}))
			if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "locked by payroll") {
				t.Errorf("from %s: %d %v", d, s, r)
			}
		}
		if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "rules": map[string]any{"halfBelow": "5:00", "fullAbove": "8:00"}})); s != 200 {
			t.Errorf("the open month: %v", r)
		}
		_, _ = st.DB.C("payroll_month").UpdateMany(ctx, bson.D{{Key: "ym", Value: prev}}, bson.M{"$set": bson.M{"locked_at": nil}})
		putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "rules": map[string]any{}}))
	})

	t.Run("create with the first version; the master create still works", func(t *testing.T) {
		body := map[string]any{"code": "x1", "name": "Late night", "effectiveFrom": "", "startTime": "23:00", "endTime": "07:00", "breakMinutes": 30, "locationIds": nil,
			"rules": map[string]any{"nextDay": true, "singlePunch": "absent"}}
		s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body)
		if s != 200 || r["shift"].(map[string]any)["code"] != "X1" || r["current"].(map[string]any)["effectiveFrom"] != "2000-01-01" {
			t.Fatalf("create: %d %v", s, r)
		}
		if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "already exists") {
			t.Fatalf("duplicate code: %d %v", s, r)
		}
		s, m := c.do("POST", "/api/masters/shift", "EMP-1002", "HR Admin", map[string]any{"code": "X2", "name": "Mid", "start_time": "11:00", "end_time": "20:00"})
		if s != 200 {
			t.Fatal(m)
		}
		_, v := c.do("GET", "/api/shifts/"+m["id"].(string), "EMP-1002", "HR Admin", nil)
		if vs := v["versions"].([]any); len(vs) != 1 || vs[0].(map[string]any)["startTime"] != "11:00" {
			t.Fatalf("master create writes the first version: %v", vs)
		}
		// A timing changed in the list applies from today as a version; earlier days keep the old one.
		if s, r := c.do("PATCH", "/api/masters/shift/"+m["id"].(string), "EMP-1002", "HR Admin", map[string]any{"start_time": "12:00"}); s != 200 {
			t.Fatal(r)
		}
		_, v = c.do("GET", "/api/shifts/"+m["id"].(string), "EMP-1002", "HR Admin", nil)
		if vs := v["versions"].([]any); len(vs) != 2 || vs[0].(map[string]any)["startTime"] != "12:00" || vs[0].(map[string]any)["effectiveFrom"] != today() || vs[1].(map[string]any)["startTime"] != "11:00" {
			t.Fatalf("list change as a version: %v", vs)
		}
	})

	t.Run("preview", func(t *testing.T) {
		d := open + "-06"
		punchDay(t, st, "EMP-1020", d, "06:00", "12:00")
		b := shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "rules": map[string]any{"halfBelow": "5:00", "fullAbove": "6:00"},
			"employeeId": "EMP-1020", "from": open + "-01", "to": policyLast(open)})
		s, r := c.do("POST", "/api/shifts/"+mID+"/preview", "EMP-1002", "HR Admin", b)
		if s != 200 || num(r["changed"]) < 1 {
			t.Fatalf("preview: %d %v", s, r)
		}
		for _, x := range r["days"].([]any) {
			if m := x.(map[string]any); m["date"] == d && (m["old"] != "Half day" || m["new"] != "Present") {
				t.Fatalf("preview day %v", m)
			}
		}
		n, _ := st.DB.C("shift_version").Count(ctx, bson.D{{Key: "rules.fullAbove", Value: "6:00"}})
		if n != 0 {
			t.Fatal("preview saved a version")
		}
	})
}

func policyLast(ym string) string { return mustDate(ym+"-01").AddDate(0, 1, -1).Format("2006-01-02") }

// Night shifts and punch windows: machine punches become one day record per shift.
func TestShiftPunchWindows(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	push := device(t, c)
	nID := shiftIDOf(t, c, "N") // 22:00-06:00
	const emp = "EMP-1021"
	dA, dA1 := dayAgo(6), dayAgo(5)
	dB, dB1 := dayAgo(3), dayAgo(2)
	for _, d := range []string{dA, dB} {
		rosterOn(t, st, emp, d, "N")
	}

	// Today, before the shift says it ends the next day: calendar days split the night.
	push([3]string{emp, dA + " 22:02", "IN"}, [3]string{emp, dA1 + " 06:04", "OUT"})
	if s := monthDay(t, c, emp, dA)["status"]; s != "Missing out punch" {
		t.Fatalf("calendar day: %s", s)
	}

	// Shift ending next day: Yes, from before dA. Saving it rebuilds the punch-machine days.
	putShift(t, c, nID, shiftBody(t, c, nID, map[string]any{"effectiveFrom": ymAdd(dA[:7], 0) + "-01", "rules": map[string]any{"nextDay": true}}))
	if x := storedDay(t, st, emp, dA); x == nil || x.in != "22:02" || x.out != "06:04" || x.outDate != dA1 {
		t.Fatalf("rebuilt night: %+v", x)
	}
	if x := storedDay(t, st, emp, dA1); x != nil {
		t.Fatalf("the out punch is not also the next day's: %+v", x)
	}
	if s := monthDay(t, c, emp, dA)["status"]; s != "Present" {
		t.Fatalf("one present day: %s", s)
	}

	t.Run("22:00-06:00 from machine punches is one present day", func(t *testing.T) {
		// The night, then a day shift on the next date (calendar day): the 06:00 out belongs to the night only.
		push([3]string{emp, dB + " 21:55", "IN"}, [3]string{emp, dB1 + " 06:01", "OUT"}, [3]string{emp, dB1 + " 13:58", "IN"}, [3]string{emp, dB1 + " 22:03", "OUT"})
		if x := storedDay(t, st, emp, dB); x == nil || x.in != "21:55" || x.out != "06:01" || x.outDate != dB1 || x.src != "Punch machine" {
			t.Fatalf("night %+v", x)
		}
		if x := storedDay(t, st, emp, dB1); x == nil || x.in != "13:58" || x.out != "22:03" {
			t.Fatalf("next day keeps its own punches: %+v", x)
		}
		if d := monthDay(t, c, emp, dB); d["status"] != "Present" || d["in"] != "21:55" || d["out"] != "06:01" {
			t.Fatalf("status %v", d)
		}
		// A regularized day is never rebuilt.
		if _, err := st.DB.C("attendance_day").UpdateMany(context.Background(), bson.D{{Key: "employee_id", Value: emp}, {Key: "date", Value: dB}},
			bson.M{"$set": bson.M{"source": "Regularized", "in_time": "22:00"}}); err != nil {
			t.Fatal(err)
		}
		push([3]string{emp, dB + " 21:40", "IN"})
		if x := storedDay(t, st, emp, dB); x.in != "22:00" || x.src != "Regularized" {
			t.Fatalf("regularized day changed: %+v", x)
		}
	})

	t.Run("consider shift in previous and next day", func(t *testing.T) {
		mk := func(code, start, end string, rules map[string]any) {
			if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", map[string]any{"code": code, "name": code, "startTime": start, "endTime": end, "breakMinutes": 0,
				"locationIds": nil, "rules": rules, "effectiveFrom": ""}); s != 200 {
				t.Fatal(r)
			}
		}
		// 00:30-08:30 duty counted on the evening's date (previous day).
		mk("NP", "00:30", "08:30", map[string]any{"shiftIn": "previous"})
		const e2 = "EMP-1022"
		d, d1 := dayAgo(9), dayAgo(8)
		rosterOn(t, st, e2, d, "NP")
		push([3]string{e2, d1 + " 00:27", "IN"}, [3]string{e2, d1 + " 08:35", "OUT"})
		if x := storedDay(t, st, e2, d); x == nil || x.in != "00:27" || x.out != "08:35" || x.outDate != "" {
			t.Fatalf("previous: %+v", x)
		}
		if x := storedDay(t, st, e2, d1); x != nil {
			t.Fatalf("previous: nothing left on the punch date: %+v", x)
		}
		if s := monthDay(t, c, e2, d)["status"]; s != "Present" {
			t.Fatalf("previous: %s", s)
		}
		// 22:00-06:00 counted on the day it ends (next day).
		mk("NX", "22:00", "06:00", map[string]any{"nextDay": true, "shiftIn": "next"})
		const e3 = "EMP-1023"
		f, f1 := dayAgo(12), dayAgo(11)
		rosterOn(t, st, e3, f1, "NX")
		push([3]string{e3, f + " 21:58", ""}, [3]string{e3, f1 + " 06:02", ""})
		if x := storedDay(t, st, e3, f1); x == nil || x.in != "21:58" || x.out != "06:02" || x.outDate != "" {
			t.Fatalf("next: %+v", x)
		}
		if s := monthDay(t, c, e3, f1)["status"]; s != "Present" {
			t.Fatalf("next: %s", s)
		}
		if x := storedDay(t, st, e3, f); x != nil {
			t.Fatalf("next: nothing left on the evening: %+v", x)
		}
	})

	t.Run("early-in and late-out windows", func(t *testing.T) {
		if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", map[string]any{"code": "WIN", "name": "Windowed day", "startTime": "09:00", "endTime": "18:00",
			"breakMinutes": 0, "locationIds": nil, "rules": map[string]any{"earlyInMax": "1:00", "lateOutMax": "2:00"}, "effectiveFrom": ""}); s != 200 {
			t.Fatal(r)
		}
		const e4 = "EMP-1024"
		d := dayAgo(4)
		rosterOn(t, st, e4, d, "WIN")
		// 07:30 is earlier than 09:00 - 1:00, 20:30 later than 18:00 + 2:00: neither is this shift's.
		push([3]string{e4, d + " 07:30", ""}, [3]string{e4, d + " 08:10", ""}, [3]string{e4, d + " 18:40", ""}, [3]string{e4, d + " 20:30", ""})
		if x := storedDay(t, st, e4, d); x == nil || x.in != "08:10" || x.out != "18:40" {
			t.Fatalf("window: %+v", x)
		}
		// Without limits (a calendar day) the first and last punch would be taken.
		const e5 = "EMP-1025"
		push([3]string{e5, d + " 07:30", ""}, [3]string{e5, d + " 20:30", ""})
		if x := storedDay(t, st, e5, d); x == nil || x.in != "07:30" || x.out != "20:30" {
			t.Fatalf("calendar day: %+v", x)
		}
	})

	t.Run("a roster change on a punched day rebuilds it", func(t *testing.T) {
		const e6 = "EMP-1026"
		// The night N ends next day from the 1st of dA's month (set above), so use dA: always inside that version.
		d, d1 := dA, dA1
		push([3]string{e6, d + " 22:00", "IN"}, [3]string{e6, d1 + " 06:00", "OUT"})
		if x := storedDay(t, st, e6, d); x == nil || x.out != "" {
			t.Fatalf("calendar day first: %+v", x)
		}
		// Past dates are locked by default; a deviation decision moves the roster (via "deviation").
		if _, err := st.DB.C("work_policy").UpdateMany(context.Background(), nil, bson.M{"$set": bson.M{"settings.rs.past": "free", "settings.att.minRest": 0}}); err != nil {
			t.Fatal(err)
		}
		s, r := c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": e6, "date": d, "code": "N", "why": "Worked the night"})
		if s != 200 {
			if strings.Contains(fmt.Sprint(r), "not mapped") {
				t.Skip("N is not mapped to this employee's location")
			}
			t.Fatal(r)
		}
		if x := storedDay(t, st, e6, d); x == nil || x.in != "22:00" || x.out != "06:00" || x.outDate != d1 {
			t.Fatalf("rebuilt after the roster change: %+v", x)
		}
	})
}

// The shift's OT rule was removed (8 Nov 2026): a new rule is refused, a stored one is ignored, and payroll has
// no overtime.
func TestShiftOvertimeRule(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	setPolicy(t, st, "Operations – 6 day", map[string]any{"ot": map[string]any{"on": true, "pre": false, "rate": "2x", "after": 30, "elig": "all", "cap": 75}})
	d := dayIn(open, time.Tuesday, 2)
	punchDay(t, st, "EMP-1020", d, "06:00", "16:00") // M shift 06:00-14:00: 10 h
	if row := c.row(t, open, "EMP-1020"); row["otHours"] != nil {
		t.Fatalf("no overtime from the policy, got %v", row["otHours"])
	}
	mID := shiftIDOf(t, c, "M")
	if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "rules": map[string]any{"otAbove": "8:30"}})); s != 400 {
		t.Fatalf("the OT rule is gone: %d %v", s, r)
	}
	// A version stored with the OT rule (and without break) still reads, without them.
	if _, err := st.DB.C("shift_version").UpdateMany(context.Background(), bson.D{{Key: "shift_id", Value: mID}},
		bson.M{"$set": bson.M{"rules.otAbove": "8:30", "rules.withoutBreak": true}}); err != nil {
		t.Fatal(err)
	}
	_, v := c.do("GET", "/api/shifts/"+mID, "EMP-1002", "HR Admin", nil)
	for _, x := range v["versions"].([]any) {
		if r, _ := x.(map[string]any)["rules"].(map[string]any); r != nil && (r["otAbove"] != nil || r["withoutBreak"] != nil) {
			t.Fatalf("removed rules shown: %v", r)
		}
	}
	if row := c.row(t, open, "EMP-1020"); row["otHours"] != nil {
		t.Fatalf("no overtime from a stored OT rule, got %v", row["otHours"])
	}
	putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "rules": v["current"].(map[string]any)["rules"]}))
}
