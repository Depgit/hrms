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
// punches inside a shift's window (night shifts; the work policy's early-in and late-out limits, 24 Nov 2026), split
// shifts.

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
		"rules": map[string]any{}, "note": ""}
	for k, x := range set {
		b[k] = x
	}
	return b
}

// allRow is a new shift's one row: all departments at all locations.
var allRow = []map[string]any{{"departmentId": "", "locationId": ""}}

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
		if len(vers) != 1 || cur["effectiveFrom"] != "2000-01-01" || cur["startTime"] != "06:00" || pd["policy"] == "" || pd["text"] != nil {
			t.Fatalf("versions %v current %v defaults %v", vers, cur, pd)
		}
		// Where it applies: the seed gave M no locations, so migration 029's rule makes it all departments at all locations.
		if maps := v["maps"].([]any); len(maps) != 1 || maps[0].(map[string]any)["label"] != "All departments at all locations" || maps[0].(map[string]any)["on"] != true {
			t.Fatalf("maps %v", maps)
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
			// Split shift (24 Nov 2026): M is 06:00-14:00.
			{map[string]any{"start2Time": "15:00"}, "end2Time", "Second part end"},
			{map[string]any{"start2Time": "13:00", "end2Time": "17:00"}, "start2Time", "after the first part ends (14:00)"},
			{map[string]any{"start2Time": "15:00", "end2Time": "15:00"}, "end2Time", "must end after it starts"},
			{map[string]any{"startTime": "22:00", "endTime": "02:00", "start2Time": "03:00", "end2Time": "06:00", "rules": map[string]any{"nextDay": true}}, "end2Time", "same day"},
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
		// The removed punch, single punch and attendance rules are refused like any unknown rule (24 Nov 2026), and so are
		// the old locations and a change of the rows through Save.
		for _, k := range []string{"mystery", "earlyInMax", "lateOutMax", "startFrom", "endFrom", "addMissingOut", "forcedOut", "officeHours", "shiftIn", "singlePunch",
			"maxHours", "absentBelow", "halfBelow", "fullAbove"} {
			if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"rules": map[string]any{k: "1:00"}})); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "unknown field") {
				t.Errorf("rule %s: %d %v", k, s, r)
			}
		}
		for _, rule := range []map[string]any{{"basedOn": "shift"}, {"weeklyAdj": map[string]any{"on": true, "hours": "4:00"}}, {"monthlyAdj": map[string]any{"on": true, "hours": "4:00"}},
			{"shortfall": map[string]any{"wo": true}}} {
			if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"rules": rule})); s != 400 {
				t.Errorf("rule %v: %d %v", rule, s, r)
			}
		}
		if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"locationIds": nil})); s != 400 {
			t.Errorf("locationIds: %d %v", s, r)
		}
		if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"maps": []map[string]any{{"departmentId": ""}}})); s != 400 {
			t.Errorf("maps through Save: %d %v", s, r)
		}
		n, _ := st.DB.C("shift_version").Count(ctx, shiftVersions(t, st, "M"))
		if n != 1 {
			t.Fatalf("a refused save wrote %d versions", n)
		}
	})

	t.Run("versions by date and the mirror row", func(t *testing.T) {
		// EMP-1020 works 07:00-15:30 on two days: late on M (06:00-14:00); a version from the 10th starts at 07:00.
		d1, d2, from := open+"-05", open+"-12", open+"-10"
		punchDay(t, st, "EMP-1020", d1, "07:00", "15:30")
		punchDay(t, st, "EMP-1020", d2, "07:00", "15:30")
		if s := monthDay(t, c, "EMP-1020", d2)["status"]; s != "Late" {
			t.Fatalf("before the change 07:00 is late: %v", s)
		}
		r := putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": from, "startTime": "07:00", "endTime": "15:00", "note": "Later shift test"}))
		if len(r["versions"].([]any)) != 2 || r["current"].(map[string]any)["effectiveFrom"] != from {
			t.Fatalf("versions %v", r["versions"])
		}
		if s1, s2 := monthDay(t, c, "EMP-1020", d1)["status"], monthDay(t, c, "EMP-1020", d2)["status"]; s1 != "Late" || s2 != "Present" {
			t.Fatalf("the version applies from its date: %s %s", s1, s2)
		}
		row := shiftRow(t, c, "M")
		if row["effective_from"] != from || row["start_time"] != "07:00" {
			t.Fatalf("the row mirrors the version in effect today: %v", row)
		}
		var by, note string
		if v, _ := st.DB.C("shift_version").FindOne(ctx, shiftVersions(t, st, "M", bson.E{Key: "effective_from", Value: from})); v != nil {
			by, _ = v["created_by"].(string)
			note, _ = v["note"].(string)
		}
		if !strings.Contains(by, "EMP-1002") || note != "Later shift test" {
			t.Fatalf("version by %q note %q", by, note)
		}
		audits, _ := st.DB.C("audit_log").Count(ctx, bson.D{{Key: "action", Value: "Changed shift"}, {Key: "entity", Value: "shift"}, {Key: "entity_id", Value: mID}})
		if audits != 1 {
			t.Fatalf("audit %d", audits)
		}
		// Saving the same date again replaces that version; back to 06:00-14:00 from the 10th.
		putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": from, "startTime": "06:00", "endTime": "14:00"}))
		if s := monthDay(t, c, "EMP-1020", d2)["status"]; s != "Late" {
			t.Fatalf("replaced version: %s", s)
		}
		if n, _ := st.DB.C("shift_version").Count(ctx, shiftVersions(t, st, "M")); n != 2 {
			t.Fatalf("versions after the replace: %d", n)
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
			s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"effectiveFrom": d, "startTime": "06:30"}))
			if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "locked by payroll") {
				t.Errorf("from %s: %d %v", d, s, r)
			}
		}
		if s, r := c.do("PUT", "/api/shifts/"+mID, "EMP-1002", "HR Admin", shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "startTime": "06:30"})); s != 200 {
			t.Errorf("the open month: %v", r)
		}
		_, _ = st.DB.C("payroll_month").UpdateMany(ctx, bson.D{{Key: "ym", Value: prev}}, bson.M{"$set": bson.M{"locked_at": nil}})
		putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "startTime": "06:00"}))
	})

	t.Run("create with the first version; the master create still works", func(t *testing.T) {
		body := map[string]any{"code": "x1", "name": "Late night", "effectiveFrom": "", "startTime": "23:00", "endTime": "07:00", "breakMinutes": 30,
			"rules": map[string]any{"nextDay": true}}
		if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "at least one row") {
			t.Fatalf("create without rows: %d %v", s, r)
		}
		fo := apiDoc(st, "department", bson.M{"name": "Front Office"})["id"]
		body["maps"] = []map[string]any{{"departmentId": fo, "locationId": ""}, {"departmentId": fo, "locationId": ""}}
		if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body); s != 409 && s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "already on this shift") {
			t.Fatalf("create with a row twice: %d %v", s, r)
		}
		body["maps"] = []map[string]any{{"departmentId": fo, "locationId": ""}}
		s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body)
		if s != 200 || r["shift"].(map[string]any)["code"] != "X1" || r["current"].(map[string]any)["effectiveFrom"] != "2000-01-01" {
			t.Fatalf("create: %d %v", s, r)
		}
		if maps := r["maps"].([]any); len(maps) != 1 || maps[0].(map[string]any)["label"] != "Front Office at all locations" || len(r["mapHistory"].([]any)) != 1 {
			t.Fatalf("create rows: %v %v", maps, r["mapHistory"])
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
		if maps := v["maps"].([]any); len(maps) != 1 || maps[0].(map[string]any)["label"] != "All departments at all locations" {
			t.Fatalf("master create applies everywhere: %v", maps)
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
		punchDay(t, st, "EMP-1020", d, "07:00", "15:30")
		b := shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "startTime": "07:00", "endTime": "15:00",
			"employeeId": "EMP-1020", "from": open + "-01", "to": policyLast(open)})
		s, r := c.do("POST", "/api/shifts/"+mID+"/preview", "EMP-1002", "HR Admin", b)
		if s != 200 || num(r["changed"]) < 1 {
			t.Fatalf("preview: %d %v", s, r)
		}
		for _, x := range r["days"].([]any) {
			if m := x.(map[string]any); m["date"] == d && (m["old"] != "Late" || m["new"] != "Present") {
				t.Fatalf("preview day %v", m)
			}
		}
		n, _ := st.DB.C("shift_version").Count(ctx, shiftVersions(t, st, "M", bson.E{Key: "start_time", Value: "07:00"}))
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
			if strings.Contains(fmt.Sprint(r), "does not apply to") {
				t.Skip("N is not mapped to this employee's location")
			}
			t.Fatal(r)
		}
		if x := storedDay(t, st, e6, d); x == nil || x.in != "22:00" || x.out != "06:00" || x.outDate != d1 {
			t.Fatalf("rebuilt after the roster change: %+v", x)
		}
	})

	// The maximum early in and late out punch are the work policy's (owner, 24 Nov 2026; they were on each shift). Last:
	// the limits window every day of the people on the policy.
	t.Run("the work policy's early-in and late-out limits build the day", func(t *testing.T) {
		if s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", map[string]any{"code": "WIN", "name": "Windowed day", "startTime": "09:00", "endTime": "18:00",
			"breakMinutes": 0, "maps": allRow, "rules": map[string]any{}, "effectiveFrom": ""}); s != 200 {
			t.Fatal(r)
		}
		const e4 = "EMP-1024"
		d := dayAgo(4)
		rosterOn(t, st, e4, d, "WIN")
		// No limit (0, as every policy has today): the calendar day, so the first and last punch are taken.
		push([3]string{e4, d + " 07:30", ""}, [3]string{e4, d + " 08:10", ""}, [3]string{e4, d + " 18:40", ""}, [3]string{e4, d + " 20:30", ""})
		if x := storedDay(t, st, e4, d); x == nil || x.in != "07:30" || x.out != "20:30" {
			t.Fatalf("no limit, calendar day: %+v", x)
		}
		// The policy gets 1 h early and 2 h late from d: saving it builds the day again from the punches. 07:30 is earlier
		// than 09:00 - 1:00 and 20:30 later than 18:00 + 2:00, so neither is this shift's.
		_, m := c.do("GET", "/api/attendance/month/"+e4+"?ym="+d[:7], "EMP-1002", "HR Admin", nil)
		pname, _ := m["policy"].(string)
		pol, err := st.DB.C("work_policy").FindOne(context.Background(), bson.M{"name": pname})
		if err != nil || pol == nil {
			t.Fatalf("policy %q: %v", pname, err)
		}
		pid := pol["id"].(string)
		set := func(early, late float64) (int, map[string]any) {
			_, g := c.do("GET", "/api/policies/"+pid, "EMP-1002", "HR Admin", nil) // the settings as the policy screen has them
			sts := g["policy"].(map[string]any)["settings"].(map[string]any)
			att := sts["att"].(map[string]any)
			att["earlyInMax"], att["lateOutMax"] = early, late
			return c.do("PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"settings": sts, "effectiveFrom": d})
		}
		if s, r := set(12.5, 0); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Maximum early in punch allowed must be from 0 to 12 h") {
			t.Fatalf("over 12 h: %d %v", s, r)
		}
		if s, r := set(1, 2); s != 200 {
			t.Fatalf("save limits: %d %v", s, r)
		}
		if x := storedDay(t, st, e4, d); x == nil || x.in != "08:10" || x.out != "18:40" {
			t.Fatalf("window from the policy: %+v", x)
		}
		if s := monthDay(t, c, e4, d)["in"]; s != "08:10" {
			t.Fatalf("the month reads the rebuilt day: %v", s)
		}
		// Back to no limit: the calendar day again.
		if s, r := set(0, 0); s != 200 {
			t.Fatalf("clear limits: %d %v", s, r)
		}
		if x := storedDay(t, st, e4, d); x == nil || x.in != "07:30" || x.out != "20:30" {
			t.Fatalf("limits cleared, calendar day: %+v", x)
		}
	})
}

// A split shift (owner, 24 Nov 2026): hours are the time worked inside its two parts, the gap never counts; the day's
// status follows the hour rules; the timings show both parts.
func TestSplitShift(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	body := map[string]any{"code": "SP", "name": "Split", "startTime": "06:00", "endTime": "10:00", "start2Time": "15:00", "end2Time": "19:00", "breakMinutes": 0,
		"maps": allRow, "rules": map[string]any{"nextDay": false}, "effectiveFrom": ""}
	s, r := c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", body)
	if s != 200 {
		t.Fatal(r)
	}
	if cur := r["current"].(map[string]any); cur["start2Time"] != "15:00" || cur["end2Time"] != "19:00" {
		t.Fatalf("second part stored: %v", cur)
	}
	if row := shiftRow(t, c, "SP"); row["start2_time"] != "15:00" || row["end2_time"] != "19:00" {
		t.Fatalf("the list row mirrors the second part: %v", row)
	}
	const emp = "EMP-1021"
	for i, x := range []struct{ in, out, want string }{
		{"06:00", "19:00", "Present"},  // both parts: 13 h less the 5 h gap = 8 h
		{"06:00", "10:00", "Half day"}, // the first part alone: 4 h
		{"15:00", "19:00", "Half day"}, // the second part alone: 4 h (the late mark is measured from 06:00)
		{"06:00", "13:00", "Half day"}, // leaving in the gap: the gap does not count
		{"06:30", "19:00", "Half day"}, // 30 min late, past the grace: 7 h 30 min
	} {
		d := dayAgo(3 + i)
		rosterOn(t, st, emp, d, "SP")
		punchDay(t, st, emp, d, x.in, x.out)
		if got := monthDay(t, c, emp, d)["status"]; got != x.want {
			t.Errorf("%s-%s on the split shift: %v, want %s", x.in, x.out, got, x.want)
		}
	}
	// Roster cells and legend show both parts.
	_, ro := c.do("GET", "/api/roster?week="+dayAgo(3), "EMP-1002", "HR Admin", nil)
	if tm, _ := ro["timings"].(map[string]any); tm != nil && tm["SP"] != nil && tm["SP"] != "06:00–10:00 + 15:00–19:00" {
		t.Errorf("roster legend: %v", tm["SP"])
	}
	// No second part again, from today: a shift in one part.
	id := r["shift"].(map[string]any)["id"].(string)
	r = putShift(t, c, id, map[string]any{"effectiveFrom": today(), "startTime": "06:00", "endTime": "14:00", "start2Time": "", "end2Time": "", "breakMinutes": 0, "name": "Split",
		"rules": map[string]any{"nextDay": false}, "note": ""})
	if cur := r["current"].(map[string]any); cur["start2Time"] != nil || cur["endTime"] != "14:00" {
		t.Fatalf("second part removed: %v", cur)
	}
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
		bson.M{"$set": bson.M{"rules.otAbove": "8:30", "rules.withoutBreak": true, "rules.earlyInMax": "1:00", "rules.singlePunch": "absent"}}); err != nil {
		t.Fatal(err)
	}
	_, v := c.do("GET", "/api/shifts/"+mID, "EMP-1002", "HR Admin", nil)
	for _, x := range v["versions"].([]any) {
		if r, _ := x.(map[string]any)["rules"].(map[string]any); r != nil && (r["otAbove"] != nil || r["withoutBreak"] != nil || r["earlyInMax"] != nil || r["singlePunch"] != nil) {
			t.Fatalf("removed rules shown: %v", r)
		}
	}
	if row := c.row(t, open, "EMP-1020"); row["otHours"] != nil {
		t.Fatalf("no overtime from a stored OT rule, got %v", row["otHours"])
	}
	putShift(t, c, mID, shiftBody(t, c, mID, map[string]any{"effectiveFrom": open + "-01", "rules": v["current"].(map[string]any)["rules"]}))
}
