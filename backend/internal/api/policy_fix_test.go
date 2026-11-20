package api_test

// Work policies clean-up (owner's decisions, 8 Nov 2026): the order of policy mappings, probation by grade,
// the pro-rata discipline score and the leave block below it, no overtime or comp-off in payroll, no
// extra-hours, rest or off-day deviations, no break, the night allowance kept, and rest checked on the roster.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

const pol6 = "Operations – 6 day" // EMP-1020's policy (via Default)

// execSQL makes one change of a test's setup directly in the database (it ran SQL before MongoDB) and stops the
// test when it fails.
func execSQL(t *testing.T, st *store.Store, change func(ctx context.Context, db store.Q) error) {
	t.Helper()
	if err := change(context.Background(), st.DB); err != nil {
		t.Fatal(err)
	}
}

// setEmp1020 changes columns of EMP-1020.
func setEmp1020(t *testing.T, st *store.Store, set bson.M) {
	t.Helper()
	execSQL(t, st, func(ctx context.Context, db store.Q) error {
		_, err := db.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": set})
		return err
	})
}

// notOK records a deviation of EMP-1020 marked Not OK on a date (unless that day already has a decision).
func notOK(t *testing.T, st *store.Store, date string) {
	t.Helper()
	execSQL(t, st, func(ctx context.Context, db store.Q) error {
		_, err := db.C("deviation_decision").Upsert(ctx, bson.M{"employee_id": "EMP-1020", "date": date}, nil,
			bson.M{"planned": "M", "actual": "G", "kinds": []string{"Shift"}, "status": "Not OK", "by_name": "policy fix test"})
		return err
	})
}

func policyID(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	r, err := st.DB.C("work_policy").FindOne(context.Background(), bson.M{"name": name})
	if err != nil || r == nil {
		t.Fatal(err, name)
	}
	return fmt.Sprint(r["id"])
}

// ---------- 3. Policy order: Individual > Location > Cluster > City > State > Department > Designation > Grade > Employment type ----------

func TestPolicyMappingOrder(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002")
	ref := map[string]string{"employee": "EMP-1020"}
	// EMP-1020's value at every level: the place from the location up to the state, department and designation by
	// name, grade and employment type.
	one := func(coll string, id any) store.Row {
		t.Helper()
		r, err := st.DB.C(coll).FindOne(ctx, bson.M{"_id": id})
		if err != nil || r == nil {
			t.Fatalf("%s %v: %v", coll, id, err)
		}
		return r
	}
	e := one("employee", "EMP-1020")
	l := one("location", e["location_id"])
	k := one("cluster", l["cluster_id"])
	ci := one("city", k["city_id"])
	d, g := one("department", e["department_id"]), one("designation", e["designation_id"])
	for lv, v := range map[string]any{"location": e["location_id"], "cluster": l["cluster_id"], "city": k["city_id"], "state": ci["state_id"],
		"department": d["name"], "designation": g["name"], "grade": e["grade"], "type": e["type"]} {
		ref[lv] = fmt.Sprint(v)
	}
	levels := []string{"employee", "location", "cluster", "city", "state", "department", "designation", "grade", "type"}
	via := map[string]string{"employee": "Employee", "location": "Location", "cluster": "Cluster", "city": "City", "state": "State", "department": "Department",
		"designation": "Designation", "grade": "Grade", "type": "Employment type"}
	ids := map[string]string{}
	// Overlapping mappings: the API now refuses one without HR's choice (policy_conflict.go), but the order still
	// decides for overlaps saved before that, so they are switched on in the database directly.
	mapOn := func(lv string) {
		t.Helper()
		wm := st.DB.C("work_policy_map")
		f := bson.M{"policy_id": ids[lv], "level": lv, "ref": ref[lv]}
		if ok, _ := wm.Exists(ctx, f); ok {
			if _, err := wm.Update(ctx, f, bson.M{"$set": bson.M{"on_map": true}}); err != nil {
				t.Fatal(err)
			}
		} else if _, err := wm.Insert(ctx, bson.M{"policy_id": ids[lv], "level": lv, "ref": ref[lv], "created_by": "test"}); err != nil {
			t.Fatal(err)
		}
	}
	// Created in reverse order, so the sort order of the policies cannot be what decides.
	for i := len(levels) - 1; i >= 0; i-- {
		lv := levels[i]
		s, r := c.do("POST", "/api/policies", "EMP-1002", "HR Admin", map[string]any{"name": "Order " + lv, "copyFrom": policyID(t, st, pol6)})
		if s != 200 {
			t.Fatal(r)
		}
		ids[lv] = r["id"].(string)
		mapOn(lv)
	}
	check := func(wantName, wantVia string) {
		t.Helper()
		_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
		p, _ := e["policy"].(map[string]any)
		if p == nil || p["name"] != wantName || p["via"] != wantVia {
			t.Fatalf("employee record: %v, want %s via %s", p, wantName, wantVia)
		}
		_, cov := c.do("GET", "/api/policies/coverage?q=EMP-1020", "EMP-1002", "HR Admin", nil)
		rows, _ := cov["rows"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["policy"] != wantName || rows[0].(map[string]any)["via"] != wantVia {
			t.Fatalf("coverage: %v, want %s via %s", rows, wantName, wantVia)
		}
	}
	// Every level matches: the most specific wins; switching it off hands over to the next level, all nine in turn.
	for _, lv := range levels {
		check("Order "+lv, via[lv])
		if s, r := c.do("PUT", "/api/policies/"+ids[lv]+"/maps", "EMP-1002", "HR Admin", map[string]any{"maps": []any{map[string]any{"level": lv, "ref": ref[lv], "on": false}}}); s != 200 {
			t.Fatal(r)
		}
	}
	check(pol6, "Default")
	// City beats Department (it came after it before), State beats Department, Grade beats Employment type.
	for _, pair := range [][2]string{{"city", "department"}, {"state", "designation"}, {"grade", "type"}} {
		for _, lv := range pair {
			mapOn(lv)
		}
		check("Order "+pair[0], via[pair[0]])
		for _, lv := range pair {
			c.do("PUT", "/api/policies/"+ids[lv]+"/maps", "EMP-1002", "HR Admin", map[string]any{"maps": []any{map[string]any{"level": lv, "ref": ref[lv], "on": false}}})
		}
	}
}

// ---------- 2. Probation by grade ----------

func TestProbationEndByGrade(t *testing.T) {
	s := policy.Defaults() // 6 months
	s.Notice.Grade = map[string]map[string]int{"G2": {"probM": 3}, "G3": {"probM": 0}, "G4": {"conf": 90}}
	for _, c := range []struct {
		grade, end string
		months     int
	}{{"G1", "2026-07-15", 6}, {"G2", "2026-04-15", 3}, {"G3", "", 0}, {"G4", "2026-07-15", 6}} {
		if got := s.ProbEnd(c.grade, "2026-01-15"); got != c.end || s.ProbMonths(c.grade) != c.months {
			t.Errorf("%s: ends %q (%d months), want %q (%d)", c.grade, got, s.ProbMonths(c.grade), c.end, c.months)
		}
	}
	if !s.InProbation("G2", "2026-01-15", "2026-04-14") || s.InProbation("G2", "2026-01-15", "2026-04-15") || s.InProbation("G3", "2026-01-15", "2026-01-16") {
		t.Error("in probation follows the grade's months")
	}
	if !s.InProbation("G1", "2026-01-15", "2026-07-14") {
		t.Error("a grade without months uses the policy's")
	}
}

func TestProbationByGrade(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1020")
	var grade string
	if e, _ := st.DB.C("employee").FindOne(ctx, bson.M{"_id": "EMP-1020"}); e != nil {
		grade, _ = e["grade"].(string)
	}
	doj := time.Now().AddDate(0, -2, 0).Format("2006-01-02")
	setEmp1020(t, st, bson.M{"doj": doj})
	pid := policyID(t, st, pol6)
	// The grade's months are saved through the policy editor (with the notice for that grade next to them).
	save := func(row map[string]any) (int, map[string]any) {
		_, g := c.do("GET", "/api/policies/"+pid, "EMP-1002", "HR Admin", nil)
		p := g["policy"].(map[string]any)
		set := p["settings"].(map[string]any)
		set["prob"] = 6
		set["notice"] = map[string]any{"prob": 15, "conf": 60, "grade": map[string]any{grade: row}}
		set["ot"] = map[string]any{"on": true} // a page opened before the clean-up still saves; the removed setting is ignored
		return c.do("PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"name": p["name"], "settings": set, "leavePlanId": p["leavePlanId"],
			"calendarId": p["calendarId"], "calMap": p["calMap"]})
	}
	record := func() (float64, any) {
		_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
		p := e["policy"].(map[string]any)
		return num(p["probMonths"]), p["probEnds"]
	}
	notice := func() map[string]any {
		_, r := c.do("GET", "/api/me/separation", "EMP-1020", "Employee", nil)
		return r["notice"].(map[string]any)
	}
	if s, r := save(map[string]any{"probM": 30}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "Probation for") {
		t.Fatalf("30 months is refused: %d %v", s, r)
	}
	// Grade: 1 month of probation, so two months after joining it is over: notice after confirmation.
	if s, r := save(map[string]any{"probM": 1, "prob": 10, "conf": 40}); s != 200 {
		t.Fatal(r)
	}
	var stored string
	if p, _ := st.DB.C("work_policy").FindOne(ctx, bson.M{"_id": pid}); p != nil {
		b, _ := json.Marshal(p["settings"])
		stored = string(b)
	}
	if strings.Contains(stored, `"ot"`) {
		t.Fatalf("a removed setting was stored: %s", stored)
	}
	end1 := policy.ISO(policy.D(doj).AddDate(0, 1, 0))
	if m, end := record(); m != 1 || end != end1 {
		t.Fatalf("employee record: %v months, ends %v, want 1 and %s", m, end, end1)
	}
	if n := notice(); n["inProbation"] != false || num(n["noticeDays"]) != 40 {
		t.Fatalf("confirmed by the grade's months: %v", n)
	}
	// Grade: 12 months, so still in probation: the grade's notice in probation.
	save(map[string]any{"probM": 12, "prob": 10, "conf": 40})
	end12 := policy.ISO(policy.D(doj).AddDate(0, 12, 0))
	if m, end := record(); m != 12 || end != end12 {
		t.Fatalf("employee record: %v months, ends %v", m, end)
	}
	if n := notice(); n["inProbation"] != true || num(n["noticeDays"]) != 10 {
		t.Fatalf("in probation by the grade's months: %v", n)
	}
	pr := getRep(t, c, "probation?from="+end12+"&to="+end12, "EMP-1002", "HR Admin")
	found := false
	for _, r := range pr.rows {
		if r[0] == "EMP-1020" {
			found = r[6].(float64) == 12 && r[7] == end12
		}
	}
	if !found {
		t.Fatalf("probation report by grade: %v", pr.rows)
	}
	// Blank: the policy default (6 months) and its notice.
	save(map[string]any{})
	if m, end := record(); m != 6 || end != policy.ISO(policy.D(doj).AddDate(0, 6, 0)) {
		t.Fatalf("policy default: %v %v", m, end)
	}
	if n := notice(); n["inProbation"] != true || num(n["noticeDays"]) != 15 {
		t.Fatalf("policy notice in probation: %v", n)
	}
	// 0 months for the grade: no probation at all.
	save(map[string]any{"probM": 0})
	if m, end := record(); m != 0 || end != nil {
		t.Fatalf("no probation: %v %v", m, end)
	}
	if n := notice(); n["inProbation"] != false || num(n["noticeDays"]) != 60 {
		t.Fatalf("no probation, notice after confirmation: %v", n)
	}
}

// ---------- 5a. Pro-rata discipline score ----------

func monthDays(ym string) int { return policy.DayDiff(ym+"-01", policy.LastDayOf(ym)) + 1 }

func TestDisciplineScoreProRata(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	now := time.Now()
	ym, td := now.Format("2006-01"), now.Format("2006-01-02")
	dim := monthDays(ym)
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"pts": 5}})
	notOK(t, st, td)

	// Joined part way through the month: the score starts at 100 × days employed ÷ days in the month.
	day := now.Day()
	if day == 1 {
		day = 2 // joining on the 1st is a full month
	}
	doj := fmt.Sprintf("%s-%02d", ym, day)
	setEmp1020(t, st, bson.M{"doj": doj})
	start := int(math.Round(100 * float64(dim-day+1) / float64(dim)))
	_, d := c.do("GET", "/api/me/discipline", "EMP-1020", "Employee", nil)
	want := fmt.Sprintf("joined %d %s", day, now.Format("Jan"))
	if num(d["start"]) != float64(start) || d["startNote"] != want {
		t.Fatalf("start %v %v, want %d (%s)", d["start"], d["startNote"], start, want)
	}
	// The Not OK of today counts when today is a day of employment.
	if wantScore := start - 5; doj <= td && num(d["score"]) != float64(wantScore) {
		t.Fatalf("score %v, want %d", d["score"], wantScore)
	}
	// Bands follow the start: a joiner without a Not OK is Good, not Poor.
	execSQL(t, st, func(ctx context.Context, db store.Q) error {
		_, err := db.C("deviation_decision").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020"})
		return err
	})
	if _, d = c.do("GET", "/api/me/discipline", "EMP-1020", "Employee", nil); num(d["score"]) != float64(start) || d["band"] != "Good" {
		t.Fatalf("a joiner with no Not OK: %v %v", d["score"], d["band"])
	}
	// Two Not OK of 5 points: 100 → 90 is Good; the same 10 points off a start of 30 is Poor.
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"pts": 10}})
	notOK(t, st, td)
	if _, d = c.do("GET", "/api/me/discipline", "EMP-1020", "Employee", nil); start < 100 && doj <= td {
		band := "Good"
		if s := float64(start - 10); s*100 < 70*float64(start) {
			band = "Poor"
		} else if s*100 < 90*float64(start) {
			band = "Watch"
		}
		if d["band"] != band {
			t.Fatalf("band of %v from %d: %v, want %s", d["score"], start, d["band"], band)
		}
	}

	// Left part way through the month (last working day the 10th): pro-rata too, shown in the discipline report.
	setEmp1020(t, st, bson.M{"doj": "2024-01-01", "lwd": ym + "-10"})
	start = int(math.Round(100 * 10 / float64(dim)))
	_, r := c.do("GET", "/api/roster/discipline?ym="+ym, "EMP-1002", "HR Admin", nil)
	var row map[string]any
	for _, x := range r["people"].([]any) {
		if m := x.(map[string]any); m["id"] == "EMP-1020" {
			row = m
		}
	}
	if row == nil || num(row["start"]) != float64(start) || !strings.Contains(fmt.Sprint(row["startNote"]), "10 "+now.Format("Jan")) {
		t.Fatalf("leaver: %v, want start %d", row, start)
	}
	// A full month starts at 100 with no note.
	setEmp1020(t, st, bson.M{"lwd": nil})
	if _, d = c.do("GET", "/api/me/discipline", "EMP-1020", "Employee", nil); num(d["start"]) != 100 || fmt.Sprint(d["startNote"]) != "" {
		t.Fatalf("full month: %v %v", d["start"], d["startNote"])
	}
	ds := getRep(t, c, "disc_score?ym="+ym, "EMP-1002", "HR Admin")
	for _, x := range ds.rows {
		if x[0] == "EMP-1020" && x[ds.col("Started at")] != 100.0 {
			t.Fatalf("report started at: %v", x)
		}
	}
}

// ---------- 5b. Leave not allowed when the discipline score is below ----------

func TestLeaveBlockedByDisciplineScore(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1009", "EMP-1020")
	now := time.Now()
	ym, td := now.Format("2006-01"), now.Format("2006-01-02")
	next := ymAdd(ym, 1)
	leaveDay := dayIn(next, time.Wednesday, 2)
	leave := func(ltype string) map[string]any {
		return map[string]any{"wf": "leave", "payload": map[string]any{"ltype": ltype, "from": leaveDay, "to": leaveDay}}
	}
	forEmp := func(b map[string]any) map[string]any { b["employeeId"] = "EMP-1020"; return b }
	problems := func(emp, role string, body map[string]any) (string, map[string]any) {
		t.Helper()
		s, r := c.do("POST", "/api/approvals/simulate", emp, role, body)
		if s != 200 {
			t.Fatalf("simulate: %d %v", s, r)
		}
		return fmt.Sprint(r["problems"]), r
	}
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"pts": 40, "leaveBelow": 70}})
	notOK(t, st, td) // 100 − 40 = 60, below 70
	block := "Leave is not allowed while the discipline score is below 70 (now 60)"

	// Refused for every leave type, raised by the employee, the manager or HR, and shown before sending.
	for _, lt := range []string{"Casual leave", "Sick leave", "Loss of pay"} {
		if p, r := problems("EMP-1020", "Employee", leave(lt)); !strings.Contains(p, block) || r["scoreBlock"] != block || r["canOverride"] == true {
			t.Fatalf("%s, own request: %v %v %v", lt, p, r["scoreBlock"], r["canOverride"])
		}
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", leave("Casual leave")); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), block) {
		t.Fatalf("employee: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1009", "Line Manager", forEmp(leave("Casual leave"))); s != 400 || !strings.Contains(fmt.Sprint(r), block) {
		t.Fatalf("manager: %d %v", s, r)
	}
	if s, r := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", forEmp(leave("Casual leave"))); s != 400 || !strings.Contains(fmt.Sprint(r), block) {
		t.Fatalf("HR without a reason: %d %v", s, r)
	}
	// Only HR Admin (or the HRBP in scope) can allow it, never the employee or the manager.
	if _, r := problems("EMP-1002", "HR Admin", forEmp(leave("Casual leave"))); r["canOverride"] != true {
		t.Fatalf("HR Admin may allow it: %v", r)
	}
	for _, x := range [][2]string{{"EMP-1020", "Employee"}, {"EMP-1009", "Line Manager"}} {
		b := forEmp(leave("Casual leave"))
		b["payload"].(map[string]any)["scoreOverride"] = "Please"
		if s, r := c.do("POST", "/api/approvals", x[0], x[1], b); s != 400 || !strings.Contains(fmt.Sprint(r), "Only HR Admin") && !strings.Contains(fmt.Sprint(r), "Nobody allows") {
			t.Fatalf("%s cannot allow it: %d %v", x[1], s, r)
		}
	}
	// HR Admin allows this one request with a reason: raised, and audited with the reason.
	b := forEmp(leave("Casual leave"))
	b["payload"].(map[string]any)["scoreOverride"] = "Family emergency, confirmed by the manager"
	s, r := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", b)
	if s != 200 {
		t.Fatalf("HR override: %d %v", s, r)
	}
	id := r["id"].(string)
	var action, reason, stored string
	entry, err := st.DB.C("audit_log").FindOne(ctx, bson.M{"entity": "approval_request", "entity_id": id, "action": bson.M{"$regex": "below the discipline score"}})
	if err != nil || entry == nil {
		t.Fatal("no audit entry for the override:", err)
	}
	action, _ = entry["action"].(string)
	after, _ := entry["after"].(map[string]any)
	reason, _ = after["reason"].(string)
	if req, _ := st.DB.C("approval_request").FindOne(ctx, bson.M{"_id": id}); req != nil {
		payload, _ := req["payload"].(map[string]any)
		stored, _ = payload["scoreOverride"].(string)
	}
	if reason != "Family emergency, confirmed by the manager" || stored != reason || !strings.Contains(action, "EMP-1020") {
		t.Fatalf("audit %q %q, stored %q", action, reason, stored)
	}
	// The override is for that one request: the next one is refused again.
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Sick leave", "from": dayIn(next, time.Thursday, 3), "to": dayIn(next, time.Thursday, 3)}}); s != 400 || !strings.Contains(fmt.Sprint(r), block) {
		t.Fatalf("next request: %d %v", s, r)
	}

	// 0 switches it off (another date: the first one now has the allowed request).
	leaveDay = dayIn(next, time.Tuesday, 4)
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"leaveBelow": 0}})
	if p, r := problems("EMP-1020", "Employee", leave("Sick leave")); strings.Contains(p, "discipline score") || r["scoreBlock"] != nil {
		t.Fatalf("0 is off: %v", p)
	}

	// Pro-rata: a joiner's limit scales like the score. The month starts at S; the limit of 70 becomes round(70 × S ÷ 100).
	day := now.Day()
	if day == 1 {
		day = 2
	}
	setEmp1020(t, st, bson.M{"doj": fmt.Sprintf("%s-%02d", ym, day)})
	dim := monthDays(ym)
	start := int(math.Round(100 * float64(dim-day+1) / float64(dim)))
	limit := int(math.Round(70 * float64(start) / 100))
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"leaveBelow": 70, "pts": start - limit}}) // score = the limit: allowed
	if p, _ := problems("EMP-1002", "HR Admin", forEmp(leave("Loss of pay"))); strings.Contains(p, "discipline score") {
		t.Fatalf("at the scaled limit %d (start %d): %v", limit, start, p)
	}
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"pts": start - limit + 1}}) // one below
	want := fmt.Sprintf("below %d (now %d)", limit, limit-1)
	if p, _ := problems("EMP-1002", "HR Admin", forEmp(leave("Loss of pay"))); !strings.Contains(p, want) || !strings.Contains(p, fmt.Sprintf("Started at %d", start)) {
		t.Fatalf("below the scaled limit, want %q: %v", want, p)
	}
}

// ---------- 4, 6-10. No overtime or comp-off, no removed deviations, no break, night allowance kept ----------

func TestNoOvertimeNightAllowanceKept(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	// Every removed setting switched on, as a policy stored before the clean-up would have it.
	setPolicy(t, st, pol6, map[string]any{"ot": map[string]any{"on": true, "pre": false, "rate": "2x", "after": 0, "elig": "all", "cap": 75}, "compoff": true,
		"hol": map[string]any{"work": "pay"}, "att": map[string]any{"brkMin": 60, "brkAfter": 6, "lateHalf": 10},
		"rs":    map[string]any{"otTol": 0, "devExtra": true, "devOff": true, "devRest": true},
		"shift": map[string]any{"nightFrom": "22:00", "nightTo": "06:00", "nightAllow": 120}})
	tue, wed, sun := dayIn(open, time.Tuesday, 2), dayIn(open, time.Wednesday, 2), dayIn(open, time.Sunday, 2)
	punchDay(t, st, "EMP-1020", tue, "06:00", "18:00") // M shift: 4 h over
	punchDay(t, st, "EMP-1020", wed, "05:30", "14:00") // touches the night hours
	punchDay(t, st, "EMP-1020", sun, "06:00", "15:00") // a weekly off worked
	row := c.row(t, open, "EMP-1020")
	var night map[string]any
	for _, l := range row["add"].([]any) {
		m := l.(map[string]any)
		label := m["label"].(string)
		if strings.Contains(strings.ToLower(label), "overtime") || strings.Contains(strings.ToLower(label), "comp-off") {
			t.Fatalf("no overtime or off-day pay: %v", m)
		}
		if strings.HasPrefix(label, "Night shift") {
			night = m
		}
	}
	if _, ok := row["otHours"]; ok {
		t.Fatalf("no overtime hours: %v", row)
	}
	if night == nil || num(night["amount"]) < 120 || num(night["amount"]) != math.Round(num(night["amount"])/120)*120 {
		t.Fatalf("night shift allowance %v", night)
	}
	n, _ := st.DB.C("compoff_credit").Count(context.Background(), bson.M{"employee_id": "EMP-1020"})
	if n != 0 {
		t.Fatalf("no comp-off credited: %d", n)
	}
}

func TestNoRemovedDeviationsOrBreak(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	setPolicy(t, st, pol6, map[string]any{"att": map[string]any{"brkMin": 60, "brkAfter": 6, "lateHalf": 10, "minRest": 11},
		"rs": map[string]any{"otTol": 0, "devExtra": true, "devOff": true, "devRest": true}})
	// Days in the last fortnight: a Sunday (weekly off), and a Tuesday and Wednesday in the same week.
	back := func(wd time.Weekday) time.Time {
		d := time.Now().AddDate(0, 0, -8)
		for d.Weekday() != wd {
			d = d.AddDate(0, 0, -1)
		}
		return d
	}
	iso := func(d time.Time) string { return d.Format("2006-01-02") }
	sun, tue := back(time.Sunday), back(time.Tuesday)
	wed := tue.AddDate(0, 0, 1)
	punchDay(t, st, "EMP-1020", iso(sun), "06:00", "15:00") // work on the weekly off
	punchDay(t, st, "EMP-1020", iso(tue), "06:00", "21:30") // M shift, 7.5 h extra and only 8.5 h before the next start
	punchDay(t, st, "EMP-1020", iso(wed), "06:00", "14:00") // on time, after a short rest
	from, to := iso(time.Now().AddDate(0, 0, -20)), iso(time.Now().AddDate(0, 0, -1))
	_, r := c.do("GET", "/api/deviations?from="+from+"&to="+to+"&q=EMP-1020", "EMP-1002", "HR Admin", nil)
	for _, x := range r["rows"].([]any) {
		m := x.(map[string]any)
		kinds := fmt.Sprint(m["dev"].(map[string]any)["kinds"])
		for _, k := range []string{"Extra hours", "Short rest", "Worked on"} {
			if strings.Contains(kinds, k) {
				t.Fatalf("removed deviation %s on %v: %v", k, m["date"], kinds)
			}
		}
		if d := m["date"]; d == iso(sun) || d == iso(tue) || d == iso(wed) {
			t.Fatalf("no deviation on %v: %v", d, kinds)
		}
	}
	// No break and no late half day: the 8 h day is Present, and so is the long day.
	day := func(d string) map[string]any { return monthDay(t, c, "EMP-1020", d) }
	if x := day(iso(wed)); x["status"] != "Present" {
		t.Fatalf("8 h day without a break deducted: %v", x)
	}
	if x := day(iso(tue)); x["status"] != "Present" {
		t.Fatalf("long day: %v", x)
	}
	punchDay(t, st, "EMP-1020", iso(wed), "06:20", "14:30") // 20 min late: a late mark, never a half day
	if x := day(iso(wed)); x["status"] != "Late" {
		t.Fatalf("late, not half day: %v", x)
	}
}

func TestRosterRestStillChecked(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002")
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }
	// A date whose next day is a working M day (not a Sunday off).
	n := 9
	for time.Now().AddDate(0, 0, n+1).Weekday() == time.Sunday || time.Now().AddDate(0, 0, n).Weekday() == time.Sunday {
		n++
	}
	// N (22:00-06:00) the night before an M day (06:00) leaves no rest: blocked under "Do not allow the change".
	s, r := c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": day(n), "code": "N"})
	if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "of rest, the minimum is 6 h") {
		t.Fatalf("rest blocks the roster: %d %v", s, r)
	}
	// "Allow it with a warning": saved, with the warning.
	setPolicy(t, st, pol6, map[string]any{"att": map[string]any{"restAct": "warn"}})
	if s, r = c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": day(n), "code": "N"}); s != 200 || !strings.Contains(fmt.Sprint(r["warning"]), "rest") {
		t.Fatalf("rest warns: %d %v", s, r)
	}
	// The shift-change notice is rs.lead: inside it a change is late and needs a reason.
	setPolicy(t, st, pol6, map[string]any{"rs": map[string]any{"lead": 5}})
	if s, r = c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": day(4), "code": "G"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "inside 5 days") {
		t.Fatalf("late change needs a reason: %d %v", s, r)
	}
	if s, r = c.do("POST", "/api/roster/cell", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "date": day(4), "code": "G", "why": "Staff absent or sick"}); s != 200 || r["late"] != true {
		t.Fatalf("late change: %d %v", s, r)
	}
}
