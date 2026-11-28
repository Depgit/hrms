package api_test

// Attendance rules report (owner, 24 Nov 2026: "test every attendance rule I have set, using dummy attendance data,
// one line item per rule"). End to end through the API, as HR and the punch machines use it: the work policy is saved
// with PUT /api/policies, the roster through the roster cell, punches go in through the punch-machine API
// (POST /api/punches, the ingestPunches path live data uses), and each day is read back from the HR attendance month
// (GET /api/attendance/month), the roster deviations (GET /api/deviations) and, for pay, the payroll row.
//
// Every expected result is written from the rule's help text (Policies.tsx, DevAllow.tsx, ShiftForm.tsx,
// RosterCell.tsx, CLAUDE.md), never from the engine's output: a difference is a failed row. The numbers are test
// values (the owner sets the real ones). Each case is one row of docs/attendance-rules-test.html; with ATTRULES_JSON
// set, the rows are also written there as JSON.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

type arRow struct {
	N       int     `json:"n"`
	Area    string  `json:"area"`
	Rule    string  `json:"rule"`
	Where   string  `json:"where"`
	Setting string  `json:"setting"`
	Shift   string  `json:"shift"`
	Date    string  `json:"date"`
	In      string  `json:"in"`
	Out     string  `json:"out"`
	Expect  string  `json:"expect"`
	Status  string  `json:"status"`
	LOP     float64 `json:"lop"`
	Note    string  `json:"note"`
	Extra   string  `json:"extra"`
	Match   bool    `json:"match"`
	Why     string  `json:"why"`
}

var arRows []arRow

type arCase struct{ rule, where, setting, shift, date, in, out, expect string }

const (
	arEmp = "EMP-1020"
	arTS  = "10:00–18:00"
)

// Where each rule is set, as the admin sees it.
const (
	arWhereHours  = "Work policy › Working time › Working hours"
	arWhereLate   = "Work policy › Attendance › Lateness and early exit"
	arWhereDev    = "Work policy › Working time › What counts as a deviation"
	arWhereRoster = "Work policy › Working time › Roster discipline (late arrival) + What counts as a deviation"
	arWherePunch  = "Work policy › Attendance › Punching"
	arWhereAbs    = "Work policy › Attendance › Absence and missing punches"
	arWhereAllow  = "Work policy › Attendance › Deviation allowances"
	arWhereOwn    = "Shift and roster › Roster › employee › Deviation allowances (own rules)"
)

type arEnv struct {
	t    *testing.T
	c    *client
	app  *api.Server
	st   *store.Store
	pid  string
	ym   string   // last month
	work []string // last month's working days not used yet (no punch, not a week off or holiday)
	wo   []string // last month's week offs
	ph   []string // last month's holidays
	key  string   // the test punch machine's key
	area string
}

// arBase are the test values every group starts from.
func arBase() map[string]any {
	off := func() map[string]any { return map[string]any{"on": false, "mins": 0, "n": 0} }
	return map[string]any{"fullH": 8, "halfH": 4, "grace": 15, "att.lateN": 2, "att.lateAct": "half", "att.early": 10,
		"rs.earlyIn": 30, "rs.earlyCount": "no", "rs.devTol": 60, "rs.outTol": 0, "rs.devStart": true, "rs.past": "free",
		"att.missing": "half", "att.absent": "lop", "rr.back": 7, "rr.cutoff": true, "att.maxSpan": 16, "att.autoOut": "none", "att.minRest": 0,
		"att.earlyInMax": 0, "att.lateOutMax": 0, "dev": map[string]any{"earlyIn": off(), "lateIn": off(), "earlyOut": off()}}
}

func arSetup(t *testing.T) *arEnv {
	c, app, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", arEmp)
	e := &arEnv{t: t, c: c, app: app, st: st, ym: prevYm(today()[:7])}
	_, mo := c.do("GET", "/api/attendance/month/"+arEmp+"?ym="+e.ym, "EMP-1002", "HR Admin", nil)
	_, l := c.do("GET", "/api/policies", "EMP-1002", "HR Admin", nil)
	for _, v := range l["list"].([]any) {
		if p := v.(map[string]any); p["name"] == mo["policy"] {
			e.pid = p["id"].(string)
		}
	}
	if e.pid == "" {
		t.Fatalf("policy of %s: %v", arEmp, mo["policy"])
	}
	for _, v := range mo["month"].(map[string]any)["days"].([]any) {
		x := v.(map[string]any)
		d := x["date"].(string)
		switch x["status"] {
		case "No punch":
			e.work = append(e.work, d)
		case "Week off":
			e.wo = append(e.wo, d)
		case "Holiday":
			e.ph = append(e.ph, d)
		}
	}
	e.newShift(map[string]any{"code": "TS", "name": "Test day", "startTime": "10:00", "endTime": "18:00", "rules": map[string]any{}})
	_, dev := c.do("POST", "/api/devices", "EMP-1002", "HR Admin", map[string]any{"name": "Attendance rules test gate"})
	e.key, _ = dev["key"].(string)
	if e.key == "" {
		t.Fatalf("device: %v", dev)
	}
	e.set(arBase())
	return e
}

func (e *arEnv) newShift(b map[string]any) {
	e.t.Helper()
	b["breakMinutes"], b["maps"], b["effectiveFrom"] = 0, allRow, ""
	if s, r := e.c.do("POST", "/api/shifts", "EMP-1002", "HR Admin", b); s != 200 {
		e.t.Fatalf("shift %v: %d %v", b["code"], s, r)
	}
}

// set changes settings of the employee's work policy (path → value, e.g. "rs.devTol") and saves it as HR does.
func (e *arEnv) set(ch map[string]any) {
	e.t.Helper()
	_, pg := e.c.do("GET", "/api/policies/"+e.pid, "EMP-1002", "HR Admin", nil)
	pol := pg["policy"].(map[string]any)
	set := pol["settings"].(map[string]any)
	for k, v := range ch {
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
	if s, r := e.c.do("PUT", "/api/policies/"+e.pid, "EMP-1002", "HR Admin", map[string]any{"name": pol["name"], "settings": set, "leavePlanId": pol["leavePlanId"],
		"calendarId": pol["calendarId"], "calMap": pol["calMap"], "effectiveFrom": ymAdd(e.ym, -1) + "-01"}); s != 200 {
		e.t.Fatalf("save policy %v: %d %v", ch, s, r)
	}
}

// dev is a deviation allowance rule.
func arRule(mins, n int, act string) map[string]any {
	return map[string]any{"on": true, "mins": mins, "n": n, "act": act}
}

func (e *arEnv) roster(d, code string) {
	e.t.Helper()
	if s, r := e.c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": arEmp, "date": d, "code": code, "why": "Attendance rules test"}); s != 200 {
		e.t.Fatalf("roster %s %s: %d %v", d, code, s, r)
	}
}

// push sends punches (time "2026-10-05 10:00", direction) from the test machine.
func (e *arEnv) push(ps ...[2]string) {
	e.t.Helper()
	l := []any{}
	for _, p := range ps {
		l = append(l, map[string]any{"employeeId": arEmp, "time": p[0], "direction": p[1]})
	}
	buf, _ := json.Marshal(map[string]any{"punches": l})
	req, _ := http.NewRequest("POST", e.c.base+"/api/punches", bytes.NewReader(buf))
	req.Header.Set("X-Device-Key", e.key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if res.StatusCode != 200 || int(num(out["accepted"])) != len(ps) {
		e.t.Fatalf("push %v: %d %v", ps, res.StatusCode, out)
	}
}

// punch sends an IN at in and an OUT at out on date d (either may be "").
func (e *arEnv) punch(d, in, out string) {
	e.t.Helper()
	var ps [][2]string
	if in != "" {
		ps = append(ps, [2]string{d + " " + in, "IN"})
	}
	if out != "" {
		ps = append(ps, [2]string{d + " " + out, "OUT"})
	}
	if len(ps) > 0 {
		e.push(ps...)
	}
}

// on rosters the shift on d and punches in and out.
func (e *arEnv) on(code, d, in, out string) {
	e.t.Helper()
	e.roster(d, code)
	e.punch(d, in, out)
}

func (e *arEnv) next() string {
	e.t.Helper()
	if len(e.work) == 0 {
		e.t.Fatal("out of working days in " + e.ym)
	}
	d := e.work[0]
	e.work = e.work[1:]
	return d
}

func (e *arEnv) take(d string) {
	for i, x := range e.work {
		if x == d {
			e.work = append(e.work[:i:i], e.work[i+1:]...)
			return
		}
	}
}

// pair is two working days in a row, both taken.
func (e *arEnv) pair() (string, string) {
	e.t.Helper()
	for i := 0; i+1 < len(e.work); i++ {
		if a, b := e.work[i], e.work[i+1]; lwAddDay(a) == b {
			e.take(a)
			e.take(b)
			return a, b
		}
	}
	e.t.Fatal("no two working days in a row left")
	return "", ""
}

func (e *arEnv) month(ym string) map[string]any {
	e.t.Helper()
	s, a := e.c.do("GET", "/api/attendance/month/"+arEmp+"?ym="+ym, "EMP-1002", "HR Admin", nil)
	if s != 200 {
		e.t.Fatalf("attendance month %s: %d %v", ym, s, a)
	}
	return a["month"].(map[string]any)
}

func arDayOf(mo map[string]any, d string) map[string]any {
	for _, v := range mo["days"].([]any) {
		if x := v.(map[string]any); x["date"] == d {
			return x
		}
	}
	return nil
}

func (e *arEnv) day(d string) map[string]any {
	e.t.Helper()
	x := arDayOf(e.month(d[:7]), d)
	if x == nil {
		e.t.Fatalf("no day %s", d)
	}
	return x
}

// devOf is the roster deviation (Planned against Actual) of the day in words, "" when there is none.
func (e *arEnv) devOf(d string) string {
	e.t.Helper()
	_, r := e.c.do("GET", "/api/deviations?from="+d+"&to="+d+"&q="+arEmp, "EMP-1002", "HR Admin", nil)
	rows, _ := r["rows"].([]any)
	for _, v := range rows {
		x := v.(map[string]any)
		if x["employeeId"] != arEmp || x["date"] != d {
			continue
		}
		dv := x["dev"].(map[string]any)
		var kinds []string
		for _, k := range dv["kinds"].([]any) {
			kinds = append(kinds, map[string]string{"Shift": "arrival outside the window", "Early exit": "early exit"}[k.(string)])
		}
		return fmt.Sprintf("Roster deviation: %s (planned %v, actual %v)", strings.Join(kinds, ", "), dv["planned"], dv["actual"])
	}
	return ""
}

// payRow is the payroll row of the employee for a month (computed live while the month is open).
func (e *arEnv) payRow(ym string) map[string]any {
	e.t.Helper()
	s, r := e.c.do("GET", "/api/payroll/"+ym+"/rows/"+arEmp, "EMP-1011", "Payroll", nil)
	if s != 200 {
		e.t.Fatalf("payroll row %s: %d %v", ym, s, r)
	}
	return r["row"].(map[string]any)
}

// ---------- checks: each returns "" when it holds, else the difference in words ----------

func arS(v any) string { s, _ := v.(string); return s }

func wantStatus(d map[string]any, want ...string) string {
	got := arS(d["status"])
	for _, w := range want {
		if got == w {
			return ""
		}
	}
	return fmt.Sprintf("status is “%s”, expected “%s”", got, strings.Join(want, "” or “"))
}

func wantPrefix(d map[string]any, p string) string {
	if got := arS(d["status"]); !strings.HasPrefix(got, p) {
		return fmt.Sprintf("status is “%s”, expected “%s…”", got, p)
	}
	return ""
}

func wantLOP(d map[string]any, v float64) string {
	if g := num(d["lop"]); math.Abs(g-v) > 1e-9 {
		return fmt.Sprintf("loss of pay is %v, expected %v", g, v)
	}
	return ""
}

func wantNote(d map[string]any, s string) string {
	if !strings.Contains(arS(d["note"]), s) {
		return fmt.Sprintf("note “%s” does not say “%s”", arS(d["note"]), s)
	}
	return ""
}

func wantNoNote(d map[string]any, s string) string {
	if strings.Contains(arS(d["note"]), s) {
		return fmt.Sprintf("note “%s” should not say “%s”", arS(d["note"]), s)
	}
	return ""
}

func wantThat(ok bool, msg string) string {
	if ok {
		return ""
	}
	return msg
}

// row records one case: d is the day as the attendance month gives it (nil for a month or balance row).
func (e *arEnv) row(cs arCase, d map[string]any, extra string, probs ...string) {
	e.t.Helper()
	r := arRow{N: len(arRows) + 1, Area: e.area, Rule: cs.rule, Where: cs.where, Setting: cs.setting, Shift: cs.shift, Date: cs.date, In: cs.in, Out: cs.out,
		Expect: cs.expect, Extra: extra, LOP: math.NaN()}
	if d != nil {
		r.Status, r.LOP, r.Note = arS(d["status"]), num(d["lop"]), arS(d["note"])
		if lv := arS(d["leave"]); strings.HasPrefix(lv, "Short leave") {
			r.Extra = strings.TrimPrefix(strings.Join([]string{r.Extra, "Shown as “" + lv + "”"}, "; "), "; ")
		}
		if d["called"] == true && num(d["returned"]) > 0 {
			r.Extra = strings.TrimPrefix(strings.Join([]string{r.Extra, fmt.Sprintf("Leave returned: %v day", d["returned"])}, "; "), "; ")
		}
	}
	var bad []string
	for _, p := range probs {
		if p != "" {
			bad = append(bad, p)
		}
	}
	r.Match, r.Why = len(bad) == 0, strings.Join(bad, "; ")
	if math.IsNaN(r.LOP) {
		r.LOP = -1 // no day: not shown
	}
	arRows = append(arRows, r)
	if p := os.Getenv("ATTRULES_JSON"); p != "" {
		b, _ := json.MarshalIndent(arRows, "", " ")
		_ = os.WriteFile(p, b, 0o644)
	}
	if !r.Match {
		e.t.Errorf("#%d %s | %s | %s %s–%s: %s", r.N, e.area, cs.rule, cs.date, cs.in, cs.out, r.Why)
	}
}

// ---------- Working hours, late-mark grace, early check-in allowance, roster deviations ----------

func TestAttRulesHours(t *testing.T) {
	e := arSetup(t)

	e.area = "Working hours (full day, half day)"
	hours := func(setting string, cases []struct {
		in, out, exp, st string
		lop              float64
	}) {
		for _, x := range cases {
			d := e.next()
			e.on("TS", d, x.in, x.out)
			got := e.day(d)
			e.row(arCase{"Full-day and half-day hours", arWhereHours, setting, arTS, d, x.in, x.out, x.exp}, got, "", wantPrefix(got, x.st), wantLOP(got, x.lop))
		}
	}
	type hc = struct {
		in, out, exp, st string
		lop              float64
	}
	hours("Full day 8 h, half day 4 h (test values)", []hc{
		{"10:00", "18:00", "8 h worked = the full-day hours: Present, no loss of pay.", "Present", 0},
		{"10:00", "17:59", "7 h 59 min: under the full-day hours but not under the half-day hours: Half day, loss of pay ½ day.", "Half day", 0.5},
		{"10:00", "14:00", "4 h = the half-day hours: Half day, loss of pay ½ day.", "Half day", 0.5},
		{"10:00", "13:59", "3 h 59 min: under the half-day hours: Absent, loss of pay 1 day.", "Absent", 1},
	})
	e.set(map[string]any{"fullH": 7.5, "halfH": 3.75})
	hours("Full day 7 h 30 min, half day 3 h 45 min (test values, hours + minutes)", []hc{
		{"10:00", "17:30", "7 h 30 min = the full-day hours: Present.", "Present", 0},
		{"10:00", "17:29", "7 h 29 min: 1 min under the full-day hours: Half day.", "Half day", 0.5},
		{"10:00", "13:45", "3 h 45 min = the half-day hours: Half day.", "Half day", 0.5},
		{"10:00", "13:44", "3 h 44 min: 1 min under the half-day hours: Absent.", "Absent", 1},
	})
	e.set(arBase())

	e.area = "Late-mark grace"
	g := "Grace 15 min (test value); full day 8 h"
	d := e.next()
	e.on("TS", d, "10:15", "18:00")
	x := e.day(d)
	e.row(arCase{"Late-mark grace", arWhereLate, g, arTS, d, "10:15", "18:00",
		"15 min late = inside the grace: on time, and the 15 min are credited, so 7 h 45 min counts as a full day: Present, no late mark."}, x, "",
		wantStatus(x, "Present"), wantLOP(x, 0))
	d = e.next()
	e.on("TS", d, "10:16", "18:16")
	x = e.day(d)
	e.row(arCase{"Late-mark grace", arWhereLate, g, arTS, d, "10:16", "18:16",
		"16 min late = beyond the grace: Late mark (counted in “Late marks allowed in a month”); 8 h worked, so no loss of pay on the day."}, x, "",
		wantStatus(x, "Late"), wantLOP(x, 0))
	d = e.next()
	e.on("TS", d, "10:16", "18:00")
	x = e.day(d)
	e.row(arCase{"Late-mark grace", arWhereLate, g, arTS, d, "10:16", "18:00",
		"16 min late and leaves at 18:00: nothing is credited beyond the grace, 7 h 44 min: Half day."}, x, "",
		wantStatus(x, "Half day"), wantLOP(x, 0.5))

	e.area = "Early check-in allowance (time before the start)"
	d = e.next()
	e.on("TS", d, "09:30", "17:30")
	x = e.day(d)
	e.row(arCase{"Time worked before the planned start", arWhereDev, "Early check-in allowed 30 min; time before the start: Not counted (test values)", arTS, d, "09:30", "17:30",
		"30 min early (inside the allowance) is not counted: hours from 10:00 to 17:30 = 7 h 30 min: Half day. Not a deviation."}, x, e.devOf(d),
		wantStatus(x, "Half day"), wantLOP(x, 0.5), wantThat(e.devOf(d) == "", "a roster deviation is shown"))
	e.set(map[string]any{"rs.earlyCount": "yes"})
	x = e.day(d)
	e.row(arCase{"Time worked before the planned start", arWhereDev, "Early check-in allowed 30 min; time before the start: Counted (test values)", arTS, d, "09:30", "17:30",
		"Counted: hours from 09:30 to 17:30 = 8 h: Present."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))
	e.set(arBase())
	d = e.next()
	e.on("TS", d, "09:29", "17:29")
	x = e.day(d)
	dv := e.devOf(d)
	e.row(arCase{"Time worked before the planned start", arWhereDev, "Early check-in allowed 30 min; Not counted (test values)", arTS, d, "09:29", "17:29",
		"31 min early = beyond the allowance: a deviation (Planned against Actual). The Not counted choice applies to the allowance only, so the 8 h from 09:29 count: Present. Arriving early is never a late mark."},
		x, dv, wantStatus(x, "Present"), wantLOP(x, 0), wantThat(dv != "", "no roster deviation shown"))

	e.area = "Roster deviation (Planned against Actual)"
	e.set(map[string]any{"rs.outTol": 30})
	rs := "Late arrival window 60 min; early check-in allowed 30 min; leaving early 30 min; arrival outside the window is a deviation: Yes (test values)"
	devCase := func(in, out, exp, st string, lop float64, wantDev string) string {
		d := e.next()
		e.on("TS", d, in, out)
		x := e.day(d)
		dv := e.devOf(d)
		chk := wantThat(dv == "", "a roster deviation is shown: "+dv)
		if wantDev != "" {
			chk = wantThat(strings.Contains(dv, wantDev), "expected a roster deviation “"+wantDev+"”, got “"+dv+"”")
		}
		e.row(arCase{"What counts as a deviation", arWhereRoster, rs, arTS, d, in, out, exp}, x, dv, wantStatus(x, st), wantLOP(x, lop), chk)
		return d
	}
	devCase("10:59", "18:59", "59 min late = inside the 60 min window: no deviation; only the usual late mark: Late.", "Late", 0, "")
	late61 := devCase("11:01", "19:01", "61 min late = outside the window: a deviation, Planned against Actual, for the manager to decide (no usual late mark); 8 h worked: Present.", "Present", 0, "arrival outside the window")
	devCase("09:30", "18:00", "30 min early = inside the early check-in allowance: no deviation; Present.", "Present", 0, "")
	devCase("09:29", "18:00", "31 min early = earlier than the allowance: a deviation (Planned against Actual). Arriving early is never a late mark: Present.", "Present", 0, "arrival outside the window")
	devCase("10:00", "17:31", "Leaves 29 min before the end = inside the 30 min: no early-exit deviation; 7 h 31 min: Half day.", "Half day", 0.5, "")
	early31 := devCase("10:00", "17:29", "Leaves 31 min before the end: an early-exit deviation; 7 h 29 min: Half day.", "Half day", 0.5, "early exit")
	e.set(map[string]any{"rs.devStart": false})
	x = e.day(late61)
	dv = e.devOf(late61)
	e.row(arCase{"Arrival outside the allowed window is a deviation", arWhereRoster, "Same, with “Arrival outside the allowed window is a deviation”: No", arTS, late61, "11:01", "19:01",
		"Not a deviation now (setting No): no Planned against Actual; 61 min late is measured from 10:00: Late."}, x, dv,
		wantStatus(x, "Late"), wantLOP(x, 0), wantThat(dv == "", "a roster deviation is still shown: "+dv))
	e.set(map[string]any{"rs.devStart": true, "rs.outTol": 0})
	x = e.day(early31)
	dv = e.devOf(early31)
	e.row(arCase{"Leaving early: minutes before the end", arWhereRoster, "Leaving early 0 min (0 switches it off)", arTS, early31, "10:00", "17:29",
		"0 switches the early-exit deviation off: no deviation; 7 h 29 min: Half day."}, x, dv,
		wantStatus(x, "Half day"), wantThat(dv == "", "a roster deviation is still shown: "+dv))
}

// ---------- Late marks allowed in a month, with the payroll ----------

func TestAttRulesLateMarks(t *testing.T) {
	e := arSetup(t)
	e.area = "Late marks allowed in a month"
	var late []string
	for i := 0; i < 4; i++ {
		d := e.next()
		e.on("TS", d, "10:30", "18:30")
		late = append(late, d)
	}
	// Every other working day of the month is a full day on the employee's own shift (06:00–14:00): the month's only
	// loss of pay can come from the late marks.
	var ps [][2]string
	for _, d := range e.work {
		ps = append(ps, [2]string{d + " 06:00", "IN"}, [2]string{d + " 14:00", "OUT"})
	}
	e.work = nil
	e.push(ps...)
	set := "Late marks allowed 2 a month; after that: Half day of leave or loss of pay; grace 15 min (test values)"
	mo := e.month(e.ym)
	for i, d := range late {
		x := arDayOf(mo, d)
		exp := fmt.Sprintf("Late mark no. %d of the month (30 min late, 8 h worked): Late, within the 2 allowed, no loss of pay.", i+1)
		probs := []string{wantStatus(x, "Late"), wantLOP(x, 0)}
		extra := ""
		if i >= 2 {
			exp = fmt.Sprintf("Late mark no. %d: beyond the 2 allowed: half a day of loss of pay in the month for it.", i+1)
			extra = fmt.Sprintf("Month: %v late marks over the limit, month loss of pay %v", mo["lateOver"], mo["lop"])
			probs = append(probs, wantThat(num(mo["lateOver"]) == 2 && num(mo["lop"]) == 1, "month: "+extra+"; expected 2 over and 1 day"))
		}
		e.row(arCase{"Late marks allowed in a month", arWhereLate, set, arTS, d, "10:30", "18:30", exp}, x, extra, probs...)
	}
	monthRow := func(setting, exp string, want float64) {
		mo := e.month(e.ym)
		pr := e.payRow(e.ym)
		extra := fmt.Sprintf("Attendance month: loss of pay %v (late marks over the limit %v). Payroll: loss of pay %v, paid days %v of %v",
			mo["lop"], mo["lateOver"], pr["lop"], pr["paid"], pr["days"])
		e.row(arCase{"Late marks allowed in a month (payroll)", arWhereLate, setting, arTS + " (late days), 06:00–14:00 other days", e.ym, "4 days 10:30", "18:30", exp}, nil, extra,
			wantThat(num(mo["lop"]) == want, fmt.Sprintf("attendance month loss of pay %v, expected %v", mo["lop"], want)),
			wantThat(num(pr["lop"]) == want, fmt.Sprintf("payroll loss of pay %v, expected %v", pr["lop"], want)),
			wantThat(num(pr["paid"]) == num(pr["days"])-want, fmt.Sprintf("payroll paid days %v, expected %v", pr["paid"], num(pr["days"])-want)))
	}
	monthRow(set, "The month: 4 late marks, 2 allowed, 2 over × ½ day = 1 day of loss of pay; payroll paid days = the month's days less 1.", 1)
	e.set(map[string]any{"att.lateN": 3})
	monthRow("Late marks allowed 3; Half day (test values)", "3 allowed: 1 over: ½ day of loss of pay in the month and in payroll.", 0.5)
	e.set(map[string]any{"att.lateN": 4})
	monthRow("Late marks allowed 4; Half day (test values)", "4 allowed = 4 late marks: none over: no loss of pay.", 0)
	e.set(map[string]any{"att.lateN": 2, "att.lateAct": "warn"})
	monthRow("Late marks allowed 2; after that: Warning only", "Warning only: no loss of pay for the late marks.", 0)
	e.set(map[string]any{"att.lateAct": "none"})
	monthRow("Late marks allowed 2; after that: No action", "No action: no loss of pay for the late marks.", 0)
}

// ---------- Missing punch, no punch, longest gap, auto-close ----------

func TestAttRulesPunches(t *testing.T) {
	e := arSetup(t)

	// No punch: two working days in a row, k and k+1 days ago, with the regularization window k days (test value).
	var dIn, dOut string
	k := 0
	isWork := func(d string) bool {
		x := arDayOf(e.month(d[:7]), d)
		return x != nil && x["status"] == "No punch"
	}
	for k = 3; k < 30; k++ {
		if a, b := dayAgo(k), dayAgo(k+1); isWork(a) && isWork(b) {
			dIn, dOut = a, b
			break
		}
	}
	if dIn == "" {
		t.Fatal("no two working days in a row in the last month")
	}
	e.take(dIn)
	e.take(dOut)
	e.roster(dIn, "TS")
	e.roster(dOut, "TS")

	e.area = "Missing punch"
	ms := func(v string) string {
		return "A day with a missing punch counts as: " + v
	}
	d := e.next()
	e.on("TS", d, "10:00", "")
	x := e.day(d)
	e.row(arCase{"A day with a missing punch counts as", arWhereAbs, ms("Half day until regularized"), arTS, d, "10:00", "—",
		"Only the in punch: a missing punch day, Half day until regularized: loss of pay ½ day."}, x, "", wantPrefix(x, "Missing"), wantLOP(x, 0.5))
	e.set(map[string]any{"att.missing": "absent"})
	x = e.day(d)
	e.row(arCase{"A day with a missing punch counts as", arWhereAbs, ms("Absent"), arTS, d, "10:00", "—",
		"Absent: loss of pay 1 day."}, x, "", wantThat(strings.HasPrefix(arS(x["status"]), "Missing") || strings.HasPrefix(arS(x["status"]), "Absent"), "status "+arS(x["status"])), wantLOP(x, 1))
	e.set(map[string]any{"att.missing": "present"})
	x = e.day(d)
	e.row(arCase{"A day with a missing punch counts as", arWhereAbs, ms("Present until regularized"), arTS, d, "10:00", "—",
		"Present until regularized: no loss of pay, still flagged to regularize."}, x, "", wantThat(strings.HasPrefix(arS(x["status"]), "Missing") || strings.HasPrefix(arS(x["status"]), "Present"), "status "+arS(x["status"])), wantLOP(x, 0))
	e.set(map[string]any{"att.missing": "half"})
	d2 := e.next()
	e.on("TS", d2, "", "18:00")
	x = e.day(d2)
	e.row(arCase{"A day with a missing punch counts as", arWhereAbs, ms("Half day until regularized"), arTS, d2, "—", "18:00",
		"Only the out punch: a missing punch day: loss of pay ½ day."}, x, "", wantPrefix(x, "Missing"), wantLOP(x, 0.5))
	e.area = "If out punch is missing (auto-close)"
	e.set(map[string]any{"att.autoOut": "end"})
	x = e.day(d)
	e.row(arCase{"If out punch is missing", arWherePunch, "Auto-close at shift end", arTS, d, "10:00", "—",
		"The missing out punch is closed at the shift end, 18:00: 8 h: Present."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))
	e.set(map[string]any{"att.autoOut": "none"})

	e.area = "No punch"
	win := fmt.Sprintf("regularization window %d days (test value)", k)
	e.set(map[string]any{"rr.back": k})
	x = e.day(dIn)
	e.row(arCase{"No punch at all counts as", arWhereAbs, "Loss of pay after the regularization window; " + win, arTS, dIn, "—", "—",
		fmt.Sprintf("No punch, %d days ago: still inside the window: no loss of pay yet.", k)}, x, "", wantStatus(x, "No punch"), wantLOP(x, 0))
	x = e.day(dOut)
	e.row(arCase{"No punch at all counts as", arWhereAbs, "Loss of pay after the regularization window; " + win, arTS, dOut, "—", "—",
		fmt.Sprintf("No punch, %d days ago: the window is over: loss of pay 1 day.", k+1)}, x, "", wantStatus(x, "No punch"), wantLOP(x, 1))
	e.set(map[string]any{"att.absent": "lopnow"})
	x = e.day(dIn)
	e.row(arCase{"No punch at all counts as", arWhereAbs, "Loss of pay straight away; " + win, arTS, dIn, "—", "—",
		"Straight away: loss of pay 1 day even inside the window."}, x, "", wantLOP(x, 1))
	e.set(map[string]any{"att.absent": "present"})
	x = e.day(dOut)
	e.row(arCase{"No punch at all counts as", arWhereAbs, "Present; " + win, arTS, dOut, "—", "—",
		"Counts as Present: no loss of pay, even after the window."}, x, "", wantLOP(x, 0))
	e.set(map[string]any{"att.absent": "lop", "rr.back": 7})

	e.area = "Longest gap between check-in and check-out"
	e.set(map[string]any{"att.maxSpan": 10})
	gap := "Longest gap 10 h (test value)"
	a, b := e.pair()
	e.on("TS", a, "10:00", "20:01")
	x = e.day(a)
	e.row(arCase{"Longest gap between check-in and check-out", arWhereAbs, gap, arTS, a, "10:00", "20:01",
		"10 h 1 min between in and out = more than 10 h: the check-out is treated as a missing punch: missing punch day, loss of pay ½ day."}, x, "",
		wantPrefix(x, "Missing"), wantLOP(x, 0.5))
	e.on("TS", b, "10:00", "18:00")
	x = e.day(b)
	e.row(arCase{"Longest gap between check-in and check-out", arWhereAbs, gap, arTS, b, "10:00", "18:00",
		"The next day, punched 10:00 to 18:00: the previous evening's 20:01 punch is not this day's out punch; the day keeps its own punches: Present."}, x,
		fmt.Sprintf("Day read as in %v, out %v", x["in"], x["out"]), wantStatus(x, "Present"), wantLOP(x, 0), wantThat(x["out"] == "18:00", fmt.Sprintf("out shown as %v", x["out"])))
	d = e.next()
	e.on("TS", d, "10:00", "19:59")
	x = e.day(d)
	e.row(arCase{"Longest gap between check-in and check-out", arWhereAbs, gap, arTS, d, "10:00", "19:59",
		"9 h 59 min = inside 10 h: a normal day: Present."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))
	e.set(map[string]any{"att.maxSpan": 0})
	x = e.day(a)
	e.row(arCase{"Longest gap between check-in and check-out", arWhereAbs, "Longest gap 0 (off)", arTS, a, "10:00", "20:01",
		"0 switches the rule off: 10 h 1 min is a normal day: Present."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))
}

// ---------- Punch window ----------

func TestAttRulesPunchWindow(t *testing.T) {
	e := arSetup(t)
	e.area = "Punch window (maximum early in / late out punch)"
	e.set(map[string]any{"att.earlyInMax": 0.5, "att.lateOutMax": 0.5})
	set := "Maximum early in punch 0 h 30 min, maximum late out punch 0 h 30 min (test values)"
	for _, c := range []struct{ in, out, exp, st string }{
		{"09:31", "18:00", "In punch 29 min before the start: inside the 30 min window, taken: Present.", "Present"},
		{"09:30", "18:00", "In punch exactly 30 min before the start: inside the window, taken: Present.", "Present"},
		{"09:29", "18:00", "In punch 31 min before the start: outside the window, not taken: a missing punch day (loss of pay ½ day).", "Missing"},
		{"10:00", "18:29", "Out punch 29 min after the end: inside the window, taken: Present.", "Present"},
		{"10:00", "18:30", "Out punch exactly 30 min after the end: inside the window, taken: Present.", "Present"},
		{"10:00", "18:31", "Out punch 31 min after the end: outside the window, not taken: a missing punch day (loss of pay ½ day).", "Missing"},
		{"09:29", "18:31", "Both punches outside the window: neither is taken: the day has no punch (No punch rule).", "No punch"},
	} {
		d := e.next()
		e.on("TS", d, c.in, c.out)
		x := e.day(d)
		lop := 0.0
		switch c.st {
		case "Missing":
			lop = 0.5
		case "No punch":
			lop = 1
		}
		e.row(arCase{"Maximum early in / late out punch allowed", arWherePunch, set, arTS, d, c.in, c.out, c.exp}, x,
			fmt.Sprintf("Day read as in %v, out %v", or(arS(x["in"]), "none"), or(arS(x["out"]), "none")), wantPrefix(x, c.st), wantLOP(x, lop))
	}
	e.set(map[string]any{"att.earlyInMax": 0, "att.lateOutMax": 0})
	d := e.next()
	e.on("TS", d, "08:00", "20:00")
	x := e.day(d)
	e.row(arCase{"Maximum early in / late out punch allowed", arWherePunch, "Both 0 h 0 min (no limit)", arTS, d, "08:00", "20:00",
		"No limit: a punch 2 h before the start and 2 h after the end are both taken: Present."}, x,
		fmt.Sprintf("Day read as in %v, out %v", x["in"], x["out"]), wantStatus(x, "Present"), wantThat(x["in"] == "08:00" && x["out"] == "20:00", "punches not taken"))
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// ---------- Deviation allowances ----------

func TestAttRulesDevAllow(t *testing.T) {
	e := arSetup(t)
	ctx := context.Background()
	e.area = "Deviation allowances"
	// The roster's late-arrival window is 3 h here so it does not overlap the 2 h allowances (the 60 min window with
	// them is its own case below).
	all := func(li, ei, eo string) map[string]any {
		return map[string]any{"rs.devTol": 180, "dev": map[string]any{"lateIn": arRule(120, 2, li), "earlyIn": arRule(120, 2, ei), "earlyOut": arRule(120, 2, eo)}}
	}
	e.set(all("none", "none", "none"))
	base := "Late check-in, Early check-in, Early check-out: each 120 min (2 h), 2 times a month (test values)"
	act := func(rule, a string) string { return base + "; " + rule + " action: " + a }
	L1, L2, L3, L4, L9 := e.next(), e.next(), e.next(), e.next(), e.next()
	e.on("TS", L1, "11:59", "19:59")
	e.on("TS", L2, "12:00", "18:00")
	e.on("TS", L3, "10:30", "18:30")
	e.on("TS", L4, "12:01", "20:01")
	e.on("TS", L9, "12:30", "14:00")
	EI1, EI2, EI3, EI4, EI6 := e.next(), e.next(), e.next(), e.next(), e.next()
	e.on("TS", EI1, "08:00", "16:00")
	e.on("TS", EI2, "08:31", "16:31")
	e.on("TS", EI3, "08:30", "18:00")
	e.on("TS", EI4, "07:59", "18:00")
	e.on("TS", EI6, "09:45", "18:00")
	EO0, EO1, EO2, EO3, EO4, HX := e.next(), e.next(), e.next(), e.next(), e.next(), e.next()
	e.on("TS", EO0, "10:00", "17:50")
	e.on("TS", EO1, "10:00", "16:00")
	e.on("TS", EO2, "10:00", "17:49")
	e.on("TS", EO3, "10:00", "17:00")
	e.on("TS", EO4, "10:00", "15:59")
	e.on("TS", HX, "10:30", "15:00")
	rc := func(rule, setting, d, in, out, exp string) arCase {
		return arCase{rule, arWhereAllow, setting, arTS, d, in, out, exp}
	}

	// Late check-in.
	mo := e.month(e.ym)
	x := arDayOf(mo, L1)
	e.row(rc("Late check-in: within the minutes and times", act("Late check-in", "No action"), L1, "11:59", "19:59",
		"1 h 59 min late, 1st time: allowed: no late mark, a note “allowed (1 of 2)”; 8 h worked: Present."), x, "",
		wantStatus(x, "Present"), wantLOP(x, 0), wantNote(x, "Late check-in allowed, 1 h 59 min (1 of 2)"))
	x = arDayOf(mo, L2)
	e.row(rc("Late check-in: within the minutes and times", act("Late check-in", "No action"), L2, "12:00", "18:00",
		"Exactly 2 h late, 2nd time: allowed, no late mark; but no hours are credited: 6 h worked: Half day."), x, "",
		wantStatus(x, "Half day"), wantLOP(x, 0.5), wantNote(x, "(2 of 2)"))
	x = arDayOf(mo, L3)
	e.row(rc("Late check-in: beyond the times", act("Late check-in", "No action"), L3, "10:30", "18:30",
		"3rd late check-in in the month (2 allowed): beyond the allowance: noted; No action = the usual rules: Late mark."), x, "",
		wantStatus(x, "Late"), wantNote(x, "3rd this month (allowed 2): beyond the allowance"))
	x = arDayOf(mo, L4)
	e.row(rc("Late check-in: beyond the minutes", act("Late check-in", "No action"), L4, "12:01", "20:01",
		"2 h 1 min late: more than the 2 h: beyond the allowance: noted; No action = the usual rules: Late mark."), x, "",
		wantStatus(x, "Late"), wantNote(x, "more than the 2 h allowed"))
	for _, a := range []struct {
		act, label, st string
		lop            float64
	}{{"absent", "Absent", "Absent until regularized", 1}, {"half", "Half day until regularized", "Half day until regularized", 0.5}, {"present", "Present until regularized", "Present until regularized", 0}} {
		e.set(all(a.act, "none", "none"))
		x = e.day(L3)
		e.row(rc("Late check-in: action beyond the allowance", act("Late check-in", a.label), L3, "10:30", "18:30",
			fmt.Sprintf("3rd time = beyond: the action decides the day: %s, loss of pay %v, until a regularization is approved; no late mark on top.", a.st, a.lop)), x, "",
			wantStatus(x, a.st), wantLOP(x, a.lop))
	}
	e.set(all("half", "none", "none"))
	x = e.day(L4)
	e.row(rc("Late check-in: action beyond the allowance", act("Late check-in", "Half day until regularized"), L4, "12:01", "20:01",
		"More than 2 h late = beyond: Half day until regularized, loss of pay ½ day."), x, "", wantStatus(x, "Half day until regularized"), wantLOP(x, 0.5))
	x = e.day(L9)
	e.row(rc("Action vs hours: never both", act("Late check-in", "Half day until regularized"), L9, "12:30", "14:00",
		"2 h 30 min late and only 1 h 30 min worked: the hours give Absent (1 day), more than the action's ½ day: the hours stand, loss of pay 1 day, never both."), x, "",
		wantPrefix(x, "Absent"), wantLOP(x, 1))
	mo = e.month(e.ym)
	cnt, _ := mo["counts"].(map[string]any)
	e.row(rc("Action replaces the late mark", act("Late check-in", "Half day until regularized"), e.ym, "", "",
		"Days settled by the action carry no late mark, so they are not counted in “Late marks allowed in a month”: no late marks in the month."), nil,
		fmt.Sprintf("Month: late marks %v, over the limit %v", or(fmt.Sprint(cnt["Late"]), "0"), mo["lateOver"]),
		wantThat(cnt["Late"] == nil && num(mo["lateOver"]) == 0, fmt.Sprintf("late marks %v, over %v", cnt["Late"], mo["lateOver"])))

	// Interaction with the roster's late-arrival window (60 min) and the bell.
	e.set(map[string]any{"rs.devTol": 60, "dev": map[string]any{"lateIn": arRule(120, 2, "none"), "earlyIn": arRule(120, 2, "none"), "earlyOut": arRule(120, 2, "none")}})
	x = e.day(L1)
	dv := e.devOf(L1)
	e.row(rc("Allowance and the roster's late-arrival window", base+"; late arrival window 60 min", L1, "11:59", "19:59",
		"1 h 59 min late is past the 60 min window, but the late check-in is allowed: no Planned against Actual deviation, no late mark: Present."), x, dv,
		wantStatus(x, "Present"), wantThat(dv == "", "roster deviation shown: "+dv))
	x = e.day(L4)
	dv = e.devOf(L4)
	e.row(rc("Allowance and the roster's late-arrival window", base+"; late arrival window 60 min; No action", L4, "12:01", "20:01",
		"2 h 1 min late, beyond the allowance with No action: the usual rules, which make it a Planned against Actual deviation for the manager (no usual late mark); 8 h worked: Present; noted as beyond the allowance."), x, dv,
		wantStatus(x, "Present"), wantNote(x, "more than the 2 h allowed"), wantThat(dv != "", "no roster deviation shown"))
	e.set(all("none", "none", "none"))
	mgr := arS(apiDoc(e.st, "employee", bson.M{"_id": arEmp})["manager_id"])
	for _, dd := range []string{L1, L3} {
		at, _ := time.ParseInLocation("2006-01-02 15:04", dd+" 23:00", time.FixedZone("IST", 19800))
		if _, err := e.app.AttDevRun(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	key := "att-dev:late-in:" + L3 + ":" + arEmp
	nHR, nMgr := apiCount(e.st, "notice", bson.M{"ref": key, "to_employee_id": "EMP-1002"}), apiCount(e.st, "notice", bson.M{"ref": key, "to_employee_id": mgr})
	txt := arS(apiDoc(e.st, "notice", bson.M{"ref": key})["text"])
	e.row(rc("Bell to HR Admin and the manager", act("Late check-in", "No action"), L3, "10:30", "18:30",
		"Beyond the allowance: the HR Admins and the reporting manager get a bell message (in every case, also with No action)."), nil,
		fmt.Sprintf("Bell to HR Admin: %d, to manager %s: %d. Text: “%s”", nHR, mgr, nMgr, txt),
		wantThat(nHR == 1 && nMgr == 1, "bell not sent to both"), wantThat(strings.Contains(txt, "late check-in 30 min"), "bell text"))
	n1 := apiCount(e.st, "notice", bson.M{"ref": "att-dev:late-in:" + L1 + ":" + arEmp})
	e.row(rc("Bell to HR Admin and the manager", act("Late check-in", "No action"), L1, "11:59", "19:59",
		"Within the allowance: no bell."), nil, fmt.Sprintf("Bell messages for the day: %d", n1), wantThat(n1 == 0, "a bell was sent"))

	// The employee's own rules.
	own := func(lateIn map[string]any) {
		if s, r := e.c.do("POST", "/api/attendance/dev-allow/"+arEmp, "EMP-1002", "HR Admin", map[string]any{"effectiveFrom": e.ym + "-01", "lateIn": lateIn,
			"earlyIn": map[string]any{"use": "policy"}, "earlyOut": map[string]any{"use": "policy"}}); s != 200 {
			t.Fatalf("own rules: %d %v", s, r)
		}
	}
	own(map[string]any{"use": "on", "mins": 60, "n": 1, "act": "absent"})
	x = e.day(L1)
	e.row(arCase{"Employee's own rule overrides the policy", arWhereOwn, "Own Late check-in: 60 min, once a month, Absent (policy: 120 min, 2 times, No action)", arTS, L1, "11:59", "19:59",
		"The own rule wins: 1 h 59 min is more than its 60 min: Absent until regularized, loss of pay 1 day."}, x, "", wantStatus(x, "Absent until regularized"), wantLOP(x, 1))
	own(map[string]any{"use": "off"})
	x = e.day(L1)
	e.row(arCase{"Employee's own rule overrides the policy", arWhereOwn, "Own Late check-in: Off", arTS, L1, "11:59", "19:59",
		"Off for this person: no allowance: the usual rules: Late mark."}, x, "", wantStatus(x, "Late"))
	own(map[string]any{"use": "policy"})
	x = e.day(L1)
	e.row(arCase{"Employee's own rule overrides the policy", arWhereOwn, "Own Late check-in: Use the policy's rule", arTS, L1, "11:59", "19:59",
		"Back to the policy's rule (120 min, 2 times): allowed: Present, no late mark."}, x, "", wantStatus(x, "Present"), wantNote(x, "(1 of 2)"))

	// Early check-in.
	mo = e.month(e.ym)
	x = arDayOf(mo, EI1)
	e.row(rc("Early check-in: within (the shift slides)", act("Early check-in", "No action"), EI1, "08:00", "16:00",
		"2 h early (= the 2 h allowed), 1st time: the shift slides to 08:00–16:00: hours from the in punch, 8 h: Present (a full day), with a note."), x, e.devOf(EI1),
		wantStatus(x, "Present"), wantLOP(x, 0), wantNote(x, "Early check-in allowed, 2 h (1 of 2)"), wantThat(e.devOf(EI1) == "", "roster deviation shown"))
	x = arDayOf(mo, EI2)
	e.row(rc("Early check-in: within (the shift slides)", act("Early check-in", "No action"), EI2, "08:31", "16:31",
		"1 h 29 min early, 2nd time: allowed: 08:31–16:31 is a full day: Present."), x, "", wantStatus(x, "Present"), wantNote(x, "(2 of 2)"))
	x = arDayOf(mo, EI3)
	e.row(rc("Early check-in: beyond the times", act("Early check-in", "No action"), EI3, "08:30", "18:00",
		"3rd time: beyond the allowance: noted; No action = usual rules: worked to 18:00: Present."), x, "",
		wantStatus(x, "Present"), wantNote(x, "3rd this month (allowed 2): beyond the allowance"))
	x = arDayOf(mo, EI4)
	e.row(rc("Early check-in: beyond the minutes", act("Early check-in", "No action"), EI4, "07:59", "18:00",
		"2 h 1 min early: more than the 2 h: beyond the allowance: noted; No action = usual rules: Present."), x, "",
		wantStatus(x, "Present"), wantNote(x, "more than the 2 h allowed"))
	x = arDayOf(mo, EI6)
	e.row(rc("Early check-in: inside the early check-in allowance", act("Early check-in", "No action")+"; early check-in allowance 30 min (Working time)", EI6, "09:45", "18:00",
		"15 min early = inside the 30 min early check-in allowance of Working time: not counted, no note: Present."), x, "",
		wantStatus(x, "Present"), wantNoNote(x, "Early check-in"))
	e.set(all("none", "absent", "none"))
	x = e.day(EI3)
	e.row(rc("Early check-in: action beyond the allowance", act("Early check-in", "Absent"), EI3, "08:30", "18:00",
		"3rd time = beyond: Absent until regularized, loss of pay 1 day."), x, "", wantStatus(x, "Absent until regularized"), wantLOP(x, 1))
	e.set(all("none", "none", "none"))

	// Early check-out.
	mo = e.month(e.ym)
	x = arDayOf(mo, EO0)
	e.row(rc("Early check-out: inside the early-out grace", act("Early check-out", "No action")+"; early-out grace 10 min", EO0, "10:00", "17:50",
		"10 min early = the early-out grace: not an early check-out, no note; 7 h 50 min worked: Half day (the grace does not add hours)."), x, "",
		wantStatus(x, "Half day"), wantNoNote(x, "Early check-out"))
	x = arDayOf(mo, EO1)
	e.row(rc("Early check-out: within", act("Early check-out", "No action"), EO1, "10:00", "16:00",
		"2 h early, 1st time: allowed: a note only, no early-exit deviation; no hours credited: 6 h: Half day."), x, "",
		wantStatus(x, "Half day"), wantNote(x, "Early check-out allowed, 2 h (1 of 2)"))
	x = arDayOf(mo, EO2)
	e.row(rc("Early check-out: within", act("Early check-out", "No action"), EO2, "10:00", "17:49",
		"11 min early (1 min past the grace), 2nd time: allowed, a note; 7 h 49 min: Half day."), x, "",
		wantStatus(x, "Half day"), wantNote(x, "(2 of 2)"))
	x = arDayOf(mo, EO3)
	e.row(rc("Early check-out: beyond the times", act("Early check-out", "No action"), EO3, "10:00", "17:00",
		"3rd time: beyond: noted; No action = usual rules: 7 h: Half day."), x, "",
		wantStatus(x, "Half day"), wantNote(x, "3rd this month (allowed 2): beyond the allowance"))
	e.set(all("none", "none", "present"))
	x = e.day(EO4)
	e.row(rc("Early check-out: beyond the minutes, hours give more", act("Early check-out", "Present until regularized"), EO4, "10:00", "15:59",
		"2 h 1 min early = beyond: Present until regularized would be no loss of pay, but the hours (5 h 59 min) give a Half day: the hours stand, loss of pay ½ day."), x, "",
		wantStatus(x, "Half day"), wantLOP(x, 0.5), wantNote(x, "more than the 2 h allowed"))

	// Harsher action wins (HX: 4th late check-in in the month, and 3 h early check-out).
	for _, h := range []struct {
		li, eo, lbl, st string
		lop             float64
	}{{"present", "absent", "Late check-in: Present until regularized; Early check-out: Absent", "Absent until regularized", 1},
		{"absent", "present", "Late check-in: Absent; Early check-out: Present until regularized", "Absent until regularized", 1},
		{"present", "half", "Late check-in: Present until regularized; Early check-out: Half day until regularized", "Half day until regularized", 0.5}} {
		e.set(all(h.li, "none", h.eo))
		x = e.day(HX)
		e.row(rc("Two rules beyond on one day: the harsher action wins", base+"; "+h.lbl, HX, "10:30", "15:00",
			fmt.Sprintf("Late check-in beyond (4th time) and early check-out beyond (3 h): the harsher action wins: %s, loss of pay %v.", h.st, h.lop)), x, "",
			wantStatus(x, h.st), wantLOP(x, h.lop))
	}

	// A regularization approved clears the action.
	e.area = "Regularization"
	e.set(map[string]any{"rr.back": 60, "rr.cutoff": false, "rs.devTol": 180, "dev": map[string]any{"lateIn": arRule(120, 2, "half"), "earlyIn": arRule(120, 2, "none"), "earlyOut": arRule(120, 2, "none")}})
	before := e.day(L3)
	s, r := e.c.do("POST", "/api/approvals", arEmp, "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": L3, "rtype": "All Other",
		"in": "10:00", "out": "18:00", "reason": "Forgot to punch"}})
	if s != 200 {
		t.Fatalf("regularization: %d %v", s, r)
	}
	e.c.approveAll(t, r["id"].(string))
	x = e.day(L3)
	e.row(arCase{"Approved regularization clears the action", "Regularization request (approved)", act("Late check-in", "Half day until regularized") + "; regularization window 60 days (test value)",
		arTS, L3, "10:30 → 10:00", "18:30 → 18:00", "Before: Half day until regularized. After the regularization 10:00–18:00 is approved: the day follows the corrected times: Present, no loss of pay."}, x,
		"Before: "+arS(before["status"])+fmt.Sprintf(", loss of pay %v", before["lop"]),
		wantStatus(before, "Half day until regularized"), wantStatus(x, "Present"), wantLOP(x, 0))
}

// ---------- Weekly off, holiday, called to work, leave, short leave, regularization ----------

func TestAttRulesDaysOff(t *testing.T) {
	e := arSetup(t)
	ctx := context.Background()
	if len(e.wo) < 2 {
		t.Fatalf("week offs in %s: %v", e.ym, e.wo)
	}
	// Test holidays on five working days of last month, on every calendar.
	var hol []string
	for i := 0; i < 5; i++ {
		hol = append(hol, e.next())
	}
	cals, err := e.st.DB.C("holiday_calendar").Distinct(ctx, "_id", bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cals {
		for _, d := range hol {
			if _, err := e.st.DB.C("holiday").Insert(ctx, bson.M{"calendar_id": id, "date": d, "name": "Test holiday", "kind": "National", "on_list": true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.area = "Weekly off and holiday"
	wh := "Work policy › Weekly off (Sundays) and the holiday calendar"
	x := e.day(e.wo[0])
	e.row(arCase{"Weekly off, no punch", wh, "Sundays off", "—", e.wo[0], "—", "—", "Week off: paid, no loss of pay."}, x, "", wantStatus(x, "Week off"), wantLOP(x, 0))
	x = e.day(hol[0])
	e.row(arCase{"Holiday, no punch", wh, "Holiday on the calendar", "—", hol[0], "—", "—", "Holiday: paid, no loss of pay."}, x, "", wantStatus(x, "Holiday"), wantLOP(x, 0))
	e.punch(e.wo[1], "10:00", "18:00")
	x = e.day(e.wo[1])
	e.row(arCase{"Worked on a weekly off", wh, "Sundays off", "—", e.wo[1], "10:00", "18:00",
		"Stays a Week off (paid), the work is noted (“worked on the day off”, 8 h); no late mark, no loss of pay, not a deviation."}, x, "",
		wantStatus(x, "Week off"), wantLOP(x, 0), wantNote(x, "Worked on the day off"))
	e.punch(hol[1], "10:00", "18:00")
	x = e.day(hol[1])
	e.row(arCase{"Worked on a holiday", wh, "Holiday on the calendar", "—", hol[1], "10:00", "18:00",
		"Stays a Holiday (paid), the work is noted; no loss of pay."}, x, "", wantStatus(x, "Holiday"), wantLOP(x, 0), wantNote(x, "Worked on the day off"))

	// Leave days (approved requests).
	leave := func(d, typ string, half bool, halfOf string) {
		p := map[string]any{"ltype": typ, "from": d, "to": d}
		days := 1.0
		if half {
			p["half"], p["halfOf"], days = true, halfOf, 0.5
		}
		pcSeed(t, e.c, "leave", arEmp, "Approved", map[string]any{"wf": "leave", "days": days}, p)
	}
	clBal := func() float64 {
		_, b := e.c.do("GET", "/api/leave/balances/me", arEmp, "Employee", nil)
		for _, v := range b["types"].([]any) {
			if r := v.(map[string]any); r["type"] == "Casual leave" {
				return num(r["balance"].(map[string]any)["bal"])
			}
		}
		t.Fatalf("no Casual leave balance: %v", b)
		return 0
	}
	e.area = "Called to work"
	wc := "Attendance › Called to work"
	call := func(d string) {
		if s, r := e.c.do("POST", "/api/attendance/called", "EMP-1002", "HR Admin", map[string]any{"employeeIds": []string{arEmp}, "date": d, "reason": "Rush at the property"}); s != 200 {
			t.Fatalf("call %s: %d %v", d, s, r)
		}
	}
	call(hol[2])
	e.punch(hol[2], "10:00", "18:00")
	x = e.day(hol[2])
	e.row(arCase{"Called to work on a holiday, came", wc, "Called on a holiday", "Own shift", hol[2], "10:00", "18:00",
		"Came (a punch): Present, no loss of pay, noted as called to work."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0), wantNote(x, "Called to work"))
	call(hol[3])
	x = e.day(hol[3])
	e.row(arCase{"Called to work on a holiday, did not come", wc, "Called on a holiday", "Own shift", hol[3], "—", "—",
		"No punch: the holiday stands, no penalty."}, x, "", wantStatus(x, "Holiday"), wantLOP(x, 0), wantNote(x, "the holiday stands"))
	if s, r := e.c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": arEmp, "date": hol[4], "code": "TS", "call": true, "reason": "Audit"}); s != 200 {
		t.Fatalf("roster call: %d %v", s, r)
	}
	e.punch(hol[4], "10:30", "18:30")
	x = e.day(hol[4])
	e.row(arCase{"Called to work from the roster cell", "Shift and roster › Roster cell › Called to work (shift chosen)", "Called on a holiday with shift 10:00–18:00", arTS, hol[4], "10:30", "18:30",
		"Came (any punch; being late does not matter): Present, no loss of pay."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0), wantNote(x, "Called to work"))
	cl1, cl2, lop1, cl3 := e.next(), e.next(), e.next(), e.next()
	for _, d := range []string{cl1, cl2, cl3} {
		e.roster(d, "TS")
		leave(d, "Casual leave", false, "")
	}
	e.roster(lop1, "TS")
	leave(lop1, "Loss of pay", false, "")
	bal0 := clBal()
	call(cl1)
	e.punch(cl1, "10:00", "18:00")
	x = e.day(cl1)
	bal1 := clBal()
	e.row(arCase{"Called to work on a leave day, came", wc, "Called on a day of approved Casual leave", arTS, cl1, "10:00", "18:00",
		"Came: Present, and the day of leave comes back to the balance (+1)."}, x, fmt.Sprintf("Casual leave balance %v → %v", bal0, bal1),
		wantStatus(x, "Present"), wantLOP(x, 0), wantThat(bal1 == bal0+1, "leave not returned"))
	call(cl2)
	x = e.day(cl2)
	e.row(arCase{"Called to work on a leave day, did not come", wc, "Called on a day of approved Casual leave", arTS, cl2, "—", "—",
		"No punch: the leave stands (paid), no penalty."}, x, "", wantStatus(x, "Casual leave"), wantLOP(x, 0), wantNote(x, "the leave stands"))
	call(lop1)
	e.punch(lop1, "10:00", "18:00")
	x = e.day(lop1)
	e.row(arCase{"Called to work on a loss-of-pay leave day, came", wc, "Called on a day of approved Loss of pay leave", arTS, lop1, "10:00", "18:00",
		"Came: Present, and the leave's loss of pay goes (0)."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))
	call(cl3)
	e.punch(cl3, "10:00", "")
	x = e.day(cl3)
	e.row(arCase{"Called to work on a leave day, one punch only", wc, "Called on a day of approved Casual leave", arTS, cl3, "10:00", "—",
		"Came (any punch, even without an out punch): Present, leave returned."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))

	e.area = "Leave"
	lw := "Leave request (approved)"
	d := e.next()
	e.roster(d, "TS")
	leave(d, "Casual leave", false, "")
	x = e.day(d)
	e.row(arCase{"Full-day paid leave", lw, "Casual leave, full day", arTS, d, "—", "—", "Paid leave: no loss of pay."}, x, "", wantPrefix(x, "Casual leave"), wantLOP(x, 0))
	d = e.next()
	e.roster(d, "TS")
	leave(d, "Loss of pay", false, "")
	x = e.day(d)
	e.row(arCase{"Loss-of-pay leave", lw, "Loss of pay, full day", arTS, d, "—", "—", "Unpaid leave: loss of pay 1 day."}, x, "", wantPrefix(x, "Loss of pay"), wantLOP(x, 1))
	half := func(typ, halfOf, in, out, exp, st string, lop float64) {
		d := e.next()
		e.on("TS", d, in, out)
		leave(d, typ, true, halfOf)
		x := e.day(d)
		e.row(arCase{"Half-day leave", lw, typ + ", half day (" + halfOf + " half); half-day hours 4 h (test value)", arTS, d, or(in, "—"), or(out, "—"), exp}, x, "",
			wantThat(strings.Contains(arS(x["status"]), st), "status “"+arS(x["status"])+"”, expected it to say “"+st+"”"), wantLOP(x, lop))
	}
	half("Casual leave", "first", "14:00", "18:00", "Half paid leave; the other half worked 4 h (= the half-day hours): Present for that half, no loss of pay.", "+ Present", 0)
	half("Casual leave", "second", "", "", "Half paid leave; no punch for the other half, after the regularization window: loss of pay ½ day.", "half day", 0.5)
	half("Casual leave", "first", "14:00", "17:00", "Half paid leave; the other half worked only 3 h (under the half-day hours): Absent for that half, loss of pay ½ day.", "+ Absent", 0.5)
	half("Loss of pay", "first", "14:00", "18:00", "Half unpaid leave (½ day loss of pay); the other half worked: loss of pay ½ day in all.", "+ Present", 0.5)

	e.area = "Short leave"
	e.set(map[string]any{"rs.outTol": 30})
	sl := "Leave › Short leave"
	short := func(status, from, to, in, out, exp, st string, lop float64, wantDev bool) {
		d := e.next()
		e.on("TS", d, in, out)
		pcSeed(t, e.c, "leave", arEmp, status, map[string]any{"wf": "leave", "days": 0}, map[string]any{"short": true, "ltype": "Short leave", "from": d, "to": d,
			"fromTime": from, "toTime": to})
		x := e.day(d)
		dv := e.devOf(d)
		e.row(arCase{"Short leave", sl, fmt.Sprintf("Short leave %s–%s (%s); grace 15 min; leaving early 30 min is a deviation", from, to, strings.ToLower(status)), arTS, d, in, out, exp}, x, dv,
			wantStatus(x, st), wantLOP(x, lop), wantThat((dv != "") == wantDev, "roster deviation: “"+dv+"”"))
	}
	short("Approved", "10:00", "12:00", "12:00", "18:00", "Approved short leave at the start: arriving at 12:00 is on time (no late mark, no deviation); its 2 h count as worked: Present.", "Present", 0, false)
	short("Approved", "16:00", "18:00", "10:00", "16:00", "Approved short leave at the end: leaving at 16:00 is no early exit; Present.", "Present", 0, false)
	short("Pending", "10:00", "12:00", "12:00", "18:00", "A short leave still waiting for approval excuses nothing: 2 h late, 6 h worked: Half day.", "Half day", 0.5, true)
	e.set(map[string]any{"rs.outTol": 0})

	e.area = "Regularization"
	d = e.next()
	e.on("TS", d, "10:00", "")
	before := e.day(d)
	e.set(map[string]any{"rr.back": 60, "rr.cutoff": false})
	s, r := e.c.do("POST", "/api/approvals", arEmp, "Employee", map[string]any{"wf": "regularize", "payload": map[string]string{"date": d, "rtype": "All Other",
		"in": "10:00", "out": "18:00", "reason": "Forgot to punch"}})
	if s != 200 {
		t.Fatalf("regularization: %d %v", s, r)
	}
	e.c.approveAll(t, r["id"].(string))
	x = e.day(d)
	e.row(arCase{"Approved regularization clears a missing punch", "Regularization request (approved)", "Missing punch: Half day until regularized; regularization window 60 days (test value)", arTS, d, "10:00", "— → 18:00",
		"Before: missing punch, ½ day loss of pay. After 10:00–18:00 is approved: Present, no loss of pay."}, x,
		"Before: "+arS(before["status"])+fmt.Sprintf(", loss of pay %v", before["lop"]),
		wantPrefix(before, "Missing"), wantStatus(x, "Present"), wantLOP(x, 0))
}

// ---------- Split shift, night shift, other time for a day ----------

func TestAttRulesShifts(t *testing.T) {
	e := arSetup(t)
	e.newShift(map[string]any{"code": "SP", "name": "Test split", "startTime": "06:00", "endTime": "10:00", "start2Time": "15:00", "end2Time": "19:00",
		"rules": map[string]any{"nextDay": false}})
	e.newShift(map[string]any{"code": "TN", "name": "Test night", "startTime": "22:00", "endTime": "06:00", "rules": map[string]any{"nextDay": true}})
	e.area = "Split shift"
	sp := "06:00–10:00 + 15:00–19:00"
	sw := "Masters › Shifts › Second part (split shift)"
	d := e.next()
	e.roster(d, "SP")
	e.push([2]string{d + " 06:00", "IN"}, [2]string{d + " 10:00", "OUT"}, [2]string{d + " 15:00", "IN"}, [2]string{d + " 19:00", "OUT"})
	x := e.day(d)
	e.row(arCase{"Split shift: both parts", sw, "Split shift; full day 8 h, half day 4 h", sp, d, "06:00, 15:00", "10:00, 19:00",
		"Both parts worked: 4 h + 4 h = 8 h (the gap is not counted): Present."}, x, "", wantStatus(x, "Present"), wantLOP(x, 0))
	for _, c := range []struct{ in, out, exp, st string }{
		{"06:00", "10:00", "Only the first part: 4 h: Half day.", "Half day"},
		{"15:00", "19:00", "Only the second part: 4 h: Half day.", "Half day"},
		{"08:00", "17:00", "In at 08:00, out at 17:00: 9 h less the 5 h gap (10:00–15:00), which never counts: 4 h: Half day.", "Half day"},
	} {
		d := e.next()
		e.on("SP", d, c.in, c.out)
		x := e.day(d)
		e.row(arCase{"Split shift: one part / the gap", sw, "Split shift; full day 8 h, half day 4 h", sp, d, c.in, c.out, c.exp}, x, "", wantStatus(x, c.st), wantLOP(x, 0.5))
	}

	e.area = "Night shift (crossing midnight)"
	nw := "Masters › Shifts › Shift ending next day: Yes"
	a, b := e.pair()
	e.roster(a, "TN")
	e.push([2]string{a + " 22:00", "IN"}, [2]string{b + " 06:00", "OUT"})
	x = e.day(a)
	nx := e.day(b)
	e.row(arCase{"Night shift: which date it counts on", nw, "Night shift 22:00–06:00", "22:00–06:00", a, "22:00 (" + a + ")", "06:00 (" + b + ")",
		"Counts on the date the shift starts: Present on that date, 8 h; the 06:00 out punch is not also a punch of the next date."}, x,
		fmt.Sprintf("Next date (%s): %s, in %v", b, arS(nx["status"]), or(arS(nx["in"]), "none")),
		wantStatus(x, "Present"), wantLOP(x, 0), wantThat(x["in"] == "22:00" && x["out"] == "06:00", fmt.Sprintf("night read as %v–%v", x["in"], x["out"])),
		wantThat(nx["in"] == nil || nx["in"] == "", "the next date took the 06:00 punch"))
	a, b = e.pair()
	e.roster(a, "TN")
	e.push([2]string{a + " 22:30", "IN"}, [2]string{b + " 06:30", "OUT"})
	x = e.day(a)
	e.row(arCase{"Night shift: late mark", nw, "Night shift 22:00–06:00; grace 15 min", "22:00–06:00", a, "22:30 (" + a + ")", "06:30 (" + b + ")",
		"30 min late across midnight: Late mark, 8 h: no loss of pay on the day."}, x, "", wantStatus(x, "Late"), wantLOP(x, 0))

	e.area = "Other time for this day (roster)"
	ow := "Shift and roster › Roster cell › Other time for this day"
	other := func(tm map[string]any, label, in, out, exp, st string) {
		d := e.next()
		if s, r := e.c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": arEmp, "date": d, "code": "OT", "time": tm, "why": "Attendance rules test"}); s != 200 {
			t.Fatalf("other time: %d %v", s, r)
		}
		e.punch(d, in, out)
		x := e.day(d)
		e.row(arCase{"Other time for this day", ow, "Other time " + label, label, d, in, out, exp}, x, "", wantStatus(x, st), wantLOP(x, 0))
	}
	other(map[string]any{"start": "12:00", "end": "20:00"}, "12:00–20:00", "12:00", "20:00", "Measured against the day's own time, 12:00–20:00: on time, 8 h: Present.", "Present")
	other(map[string]any{"start": "12:00", "end": "20:00"}, "12:00–20:00", "12:30", "20:30", "30 min after the day's own start: Late mark.", "Late")
	other(map[string]any{"start": "08:00", "end": "12:00", "start2": "16:00", "end2": "20:00"}, "08:00–12:00 + 16:00–20:00", "08:00", "20:00",
		"Two parts: 12 h in to out less the 4 h gap = 8 h: Present.", "Present")
}

// ---------- Leave earned on paid days ----------

func TestAttRulesLeaveEarn(t *testing.T) {
	e := arSetup(t)
	e.area = "Leave earned on paid days"
	_, mine := e.c.do("GET", "/api/leave/balances/me", arEmp, "Employee", nil)
	_, all := e.c.do("GET", "/api/leave-plans", "EMP-1002", "HR Admin", nil)
	var plan map[string]any
	for _, v := range all["plans"].([]any) {
		if p := v.(map[string]any)["plan"].(map[string]any); p["name"] == mine["plan"] {
			plan = p
		}
	}
	if plan == nil {
		t.Fatalf("plan %v", mine["plan"])
	}
	lv := plan["lv"].(map[string]any)
	lv["earn"] = []any{map[string]any{"days": 5, "types": []any{map[string]any{"type": "Casual leave", "days": 1}}}}
	if s, r := e.c.do("PUT", "/api/leave-plans/"+plan["id"].(string), "EMP-1002", "HR Admin", map[string]any{"name": plan["name"], "description": plan["description"],
		"lv": lv, "lrx": plan["lrx"], "types": plan["types"], "effectiveFrom": today()}); s != 200 {
		t.Fatalf("leave plan: %d %v", s, r)
	}
	worked := func(on string) map[string]any {
		_, b := e.c.do("GET", "/api/leave/balances/me?on="+on, arEmp, "Employee", nil)
		for _, v := range b["types"].([]any) {
			if r := v.(map[string]any); r["type"] == "Casual leave" {
				w, _ := r["balance"].(map[string]any)["worked"].(map[string]any)
				return w
			}
		}
		t.Fatalf("no Casual leave on %s: %v", on, b)
		return nil
	}
	// From the 1st of last month: week offs and holidays 1, working days punched 10:00–18:00 (1), the 3rd one
	// 10:00–14:00 (a half day, 0.5), until 10 paid days.
	type pd struct {
		d, what string
		v, cum  float64
	}
	var seq []pd
	cum, nWork := 0.0, 0
	for d := e.ym + "-01"; cum < 10 && d[:7] == e.ym; d = lwAddDay(d) {
		p := pd{d: d, v: 1}
		switch {
		case containsS(e.wo, d):
			p.what = "week off"
		case containsS(e.ph, d):
			p.what = "holiday"
		default:
			nWork++
			e.take(d)
			if nWork == 3 {
				e.on("TS", d, "10:00", "14:00")
				p.what, p.v = "half day", 0.5
			} else {
				e.on("TS", d, "10:00", "18:00")
				p.what = "present"
			}
		}
		cum += p.v
		p.cum = cum
		seq = append(seq, p)
	}
	var parts []string
	for _, p := range seq {
		parts = append(parts, fmt.Sprintf("%s %s %v", p.d[8:], p.what, p.v))
	}
	story := strings.Join(parts, ", ")
	at := func(n float64) (string, float64, float64) { // the date the cumulative reaches n, the cumulative the day before and on it
		prev := 0.0
		for _, p := range seq {
			if p.cum >= n {
				return p.d, prev, p.cum
			}
			prev = p.cum
		}
		t.Fatalf("never %v paid days: %v", n, seq)
		return "", 0, 0
	}
	set := "Earn by working days: every 5 paid days, Casual leave 1 day (test values)"
	w0 := worked(e.ym + "-01")
	if num(w0["days"]) != 0 {
		t.Fatalf("paid days counted before %s: %v", e.ym, w0)
	}
	d5, before5, on5 := at(5)
	w := worked(d5)
	e.row(arCase{"Paid days and the credit", "Leave plan › Earn by working days", set, arTS, d5, "", "",
		fmt.Sprintf("Balance on %s, the day the 5th paid day is completed: the days counted are those before it (%v): no credit yet. Days from the 1st: %s.", d5, before5, story)}, nil,
		fmt.Sprintf("Paid days counted %v, credit %v", w["days"], w["credit"]),
		wantThat(num(w["days"]) == before5, fmt.Sprintf("paid days %v, expected %v", w["days"], before5)), wantThat(num(w["credit"]) == 0, "credit already given"))
	n5 := lwAddDay(d5)
	w = worked(n5)
	e.row(arCase{"Paid days and the credit", "Leave plan › Earn by working days", set, arTS, n5, "", "",
		fmt.Sprintf("The day after the 5th paid day (%s): %v paid days counted: 1 day of Casual leave credited.", d5, on5)}, nil,
		fmt.Sprintf("Paid days counted %v, credit %v", w["days"], w["credit"]),
		wantThat(num(w["days"]) == on5, fmt.Sprintf("paid days %v, expected %v", w["days"], on5)), wantThat(num(w["credit"]) == 1, "credit not 1"))
	d10, _, on10 := at(10)
	n10 := lwAddDay(d10)
	w = worked(n10)
	var credits []string
	for _, c := range w["credits"].([]any) {
		credits = append(credits, arS(c.(map[string]any)["on"]))
	}
	sort.Strings(credits)
	e.row(arCase{"Paid days and the credit", "Leave plan › Earn by working days", set, arTS, n10, "", "",
		fmt.Sprintf("The day after the 10th paid day: %v paid days, 2 days credited, the first from %s, the second from %s (the half day counts ½).", on10, n5, n10)}, nil,
		fmt.Sprintf("Paid days counted %v, credit %v, credited on %s", w["days"], w["credit"], strings.Join(credits, " and ")),
		wantThat(num(w["days"]) == on10, fmt.Sprintf("paid days %v, expected %v", w["days"], on10)), wantThat(num(w["credit"]) == 2, "credit not 2"),
		wantThat(len(credits) == 2 && credits[0] == n5 && credits[1] == n10, "credit dates "+strings.Join(credits, ", ")))
}
