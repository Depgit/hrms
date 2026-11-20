package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// setPolicy merges settings into the work policy named.
func setPolicy(t *testing.T, st *store.Store, name string, patch map[string]any) {
	t.Helper()
	ctx := context.Background()
	pol, err := st.DB.C("work_policy").FindOne(ctx, bson.M{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	if pol == nil {
		t.Fatalf("work policy %q not found", name)
	}
	m := map[string]any{}
	raw, _ := json.Marshal(pol["settings"])
	_ = json.Unmarshal(raw, &m)
	var merge func(dst, src map[string]any)
	merge = func(dst, src map[string]any) {
		for k, v := range src {
			if sv, ok := v.(map[string]any); ok {
				dv, _ := dst[k].(map[string]any)
				if dv == nil {
					dv = map[string]any{}
				}
				merge(dv, sv)
				dst[k] = dv
				continue
			}
			dst[k] = v
		}
	}
	merge(m, patch)
	if _, err := st.DB.C("work_policy").UpdateMany(ctx, bson.M{"name": name}, bson.M{"$set": bson.M{"settings": m}}); err != nil {
		t.Fatal(err)
	}
}

func dayIn(ym string, weekday time.Weekday, nth int) string {
	d := mustDate(ym + "-01")
	for n := 0; d.Format("2006-01") == ym; d = d.AddDate(0, 0, 1) {
		if d.Weekday() == weekday {
			n++
			if n == nth {
				return d.Format("2006-01-02")
			}
		}
	}
	return ""
}

// punchDay stores a day's punches as an MIS upload, replacing the day's punches if it has some (an upsert on the
// employee and date).
func punchDay(t *testing.T, st *store.Store, id, date, in, out string) {
	t.Helper()
	if _, err := st.DB.C("attendance_day").Upsert(context.Background(), bson.M{"employee_id": id, "date": date},
		bson.M{"in_time": in, "out_time": out, "out_date": nil, "source": "MIS upload"}, bson.M{"by_name": "deep test"}); err != nil {
		t.Fatal(err)
	}
}

// deep2MonthsBefore is a date minus an interval of months as PostgreSQL counts it: the same day of the month, or
// the month's last day when it is shorter.
func deep2MonthsBefore(date string, months int) string {
	d := mustDate(date)
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -months, 0)
	day := min(d.Day(), first.AddDate(0, 1, -1).Day())
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

// deep2DaysAgo is current_date - n in the business zone.
func deep2DaysAgo(n int) string { return store.DateOf(time.Now().AddDate(0, 0, -n)) }

// ---------- Time: no overtime, night allowance, comp-off already credited, clubbing, team cover, locked months ----------

func TestDeepTimeAndLeave(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1008", "EMP-1011", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	setPolicy(t, st, "Operations – 6 day", map[string]any{"ot": map[string]any{"on": true, "pre": false, "rate": "2x", "after": 30, "elig": "all", "cap": 75},
		"shift": map[string]any{"nightFrom": "22:00", "nightTo": "06:00", "nightAllow": 100}, "compoff": true, "hol": map[string]any{"work": "choose"}})
	// Overtime and comp-off were removed (8 Nov 2026): the old settings above stay in the stored policy and are ignored.
	t.Run("no overtime, night allowance still paid", func(t *testing.T) {
		// EMP-1020 works the M shift (06:00-14:00): 3.5 hours over on one day, a night start on another.
		d1, d2 := dayIn(open, time.Tuesday, 2), dayIn(open, time.Wednesday, 2)
		punchDay(t, st, "EMP-1020", d1, "06:00", "17:30")
		punchDay(t, st, "EMP-1020", d2, "05:30", "14:00")
		row := c.row(t, open, "EMP-1020")
		var night map[string]any
		for _, l := range row["add"].([]any) {
			m := l.(map[string]any)
			if strings.HasPrefix(m["label"].(string), "Overtime") {
				t.Fatalf("no overtime line: %v", m)
			}
			if strings.HasPrefix(m["label"].(string), "Night shift") {
				night = m
			}
		}
		if _, ok := row["otHours"]; ok {
			t.Fatalf("no overtime hours: %v", row["otHours"])
		}
		if night == nil || num(night["amount"]) < 100 {
			t.Fatalf("night allowance %v", night)
		}
		// 3.5 hours over the shift is not a deviation any more, so there is nothing to approve.
		if s, r := c.do("POST", "/api/deviations/decide", "EMP-1008", "Property Manager", map[string]any{"items": []any{map[string]any{"employeeId": "EMP-1020", "date": d1}}, "status": "OK"}); s != 200 || r["done"] != 0.0 {
			t.Fatal(r)
		}
	})

	t.Run("no new comp-off; credited comp-off stays usable", func(t *testing.T) {
		sun := dayIn(open, time.Sunday, 2)
		punchDay(t, st, "EMP-1020", sun, "06:00", "15:30")
		// Work on a weekly off is not a deviation, so nothing is marked OK and no comp-off is credited.
		if s, r := c.do("POST", "/api/deviations/decide", "EMP-1008", "Property Manager", map[string]any{"items": []any{map[string]any{"employeeId": "EMP-1020", "date": sun}}, "status": "OK"}); s != 200 || r["done"] != 0.0 {
			t.Fatal(r)
		}
		n, _ := st.DB.C("compoff_credit").Count(ctx, bson.M{"employee_id": "EMP-1020"})
		if n != 0 {
			t.Fatalf("comp-off credited: %d", n)
		}
		// A credit made before the change stays usable until it is used or expires.
		if _, err := st.DB.C("compoff_credit").Insert(ctx, bson.M{"employee_id": "EMP-1020", "date": sun, "days": 1, "expires": mustDate(sun).AddDate(0, 0, 90).Format("2006-01-02"),
			"hours": 9.5, "by_name": "before the change"}); err != nil {
			t.Fatal(err)
		}
		_, bal := c.do("GET", "/api/leave/balances/me", "EMP-1020", "Employee", nil)
		got := -1.0
		for _, x := range bal["types"].([]any) {
			if m := x.(map[string]any); m["type"] == "Comp-off" {
				got = num(m["compoff"])
			}
		}
		if got != 1 {
			t.Fatalf("comp-off balance %v", got)
		}
		next := ymAdd(today()[:7], 1)
		d1, d2 := dayIn(next, time.Monday, 2), dayIn(next, time.Tuesday, 3)
		if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "kind": "leave", "from": next + "-01", "to": next + "-28", "until": next + "-28", "reason": "Deep test"}); s != 200 {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Comp-off", "from": d1, "to": d1}}); s != 200 {
			t.Fatal("the credited comp-off day can be used:", r)
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Comp-off", "from": d2, "to": d2}}); s != 400 || !strings.Contains(fmt.Sprint(r), "Comp-off balance") {
			t.Fatalf("only the one credited day: %d %v", s, r)
		}
	})

	t.Run("clubbing, team cover and the staffing limit", func(t *testing.T) {
		_, _ = st.DB.C("leave_plan").UpdateMany(ctx, bson.M{"name": "Standard leave plan", "lrx": bson.M{"$type": "object"}}, bson.M{"$set": bson.M{"lrx.club": "no"}})
		next := ymAdd(today()[:7], 1)
		cl, pl := dayIn(next, time.Wednesday, 3), dayIn(next, time.Thursday, 3)
		_, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Casual leave", "from": cl, "to": cl}})
		clID, _ := r["id"].(string)
		if clID == "" {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": "Privilege leave", "from": pl, "to": pl}}); s != 400 || !strings.Contains(fmt.Sprint(r), "cannot be joined") {
			t.Fatalf("clubbing: %d %v", s, r)
		}
		// The colleague in F&B at the same property is off the same day: team cover shows it; without an exception the limit stops it.
		if s, r := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", map[string]any{"wf": "leave", "employeeId": "EMP-1032", "payload": map[string]any{"ltype": "Casual leave", "from": cl, "to": cl}}); s != 400 || !strings.Contains(fmt.Sprint(r), "Too many people") {
			t.Fatalf("staffing limit: %d %v", s, r)
		}
		c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1032", "kind": "leave", "from": cl, "to": cl, "until": cl, "reason": "Cover arranged"})
		if s, r := c.do("POST", "/api/approvals", "EMP-1002", "HR Admin", map[string]any{"wf": "leave", "employeeId": "EMP-1032", "payload": map[string]any{"ltype": "Casual leave", "from": cl, "to": cl}}); s != 200 {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/approvals/"+clID, "EMP-1002", "HR Admin", nil)
		off, _ := d["teamOff"].([]any)
		if len(off) != 1 || off[0].(map[string]any)["id"] != "EMP-1032" {
			t.Fatalf("team cover %v", d["teamOff"])
		}
	})

	t.Run("a locked month needs an exception to regularize", func(t *testing.T) {
		// Attendance closure in the run checklist is the lock now (Month close is gone): cleared, then closed.
		c.pcClear(t, open, "attendance")
		if s, r := c.do("POST", "/api/payroll/"+open+"/closures/attendance/close", "EMP-1011", "Payroll", nil); s != 200 {
			t.Fatal(r)
		}
		date := dayIn(open, time.Thursday, 3)
		_, _ = st.DB.C("attendance_day").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020", "date": date})
		body := map[string]any{"wf": "regularize", "payload": map[string]any{"date": date, "rtype": "No Punch", "in": "06:00", "out": "14:00", "reason": "Forgot to punch"}}
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", body); s != 400 || !strings.Contains(fmt.Sprint(r), "locked by payroll") {
			t.Fatalf("%d %v", s, r)
		}
		c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "kind": "regularize", "from": date, "to": date, "until": today(), "reason": "Biometric fault"})
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", body); s != 200 {
			t.Fatalf("with an exception: %v", r)
		}
	})
}

// ---------- Separation: settlement lines, privacy, windows, clearance roles ----------

func TestDeepSeparation(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1020")
	_, _ = st.DB.C("leave_plan").UpdateMany(ctx, bson.M{"lv": bson.M{"$type": "object"}}, bson.M{"$set": bson.M{"lv.plEnc": true}})
	signClearance := func(sep string) {
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1002", "HR Admin", nil)
		for _, x := range d["clearance"].([]any) {
			if m := x.(map[string]any); m["auto"] == nil {
				if s, r := c.do("POST", fmt.Sprintf("/api/separations/%s/clearance/%v", sep, m["id"]), "EMP-1002", "HR Admin", nil); s != 200 {
					t.Fatal(r)
				}
			}
		}
	}
	lines := func(id string) map[string]float64 {
		_, pv := c.do("GET", "/api/fnf/"+id, "EMP-1011", "Payroll", nil)
		out := map[string]float64{}
		for _, l := range pv["lines"].([]any) {
			m := l.(map[string]any)
			out[m["key"].(string)] = num(m["amount"])
		}
		return out
	}

	t.Run("termination with pay in lieu, gratuity and leave encashment", func(t *testing.T) {
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"doj": deep2MonthsBefore(store.Today(), 6*12+2)}})
		_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "type": "Termination", "reason": "Misconduct", "noticeOption": "pay",
			"note": "Repeated cash shortages after two warnings"})
		sep := r["id"].(string)
		c.approveAll(t, r["requestId"].(string))
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1016"}, bson.M{"$set": bson.M{"lwd": deep2DaysAgo(1)}})
		signClearance(sep)
		var gross, days float64
		if g := apiDoc(st, "gross_history", bson.M{"employee_id": "EMP-1016"}, options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}})); g != nil {
			gross = apiFloat(g["gross"])
		}
		if x := apiDoc(st, "separation", bson.M{"_id": sep}); x != nil {
			days = apiFloat(x["notice_days"])
		}
		L := lines("EMP-1016")
		basic := math.Round(gross / 2)
		if !near(L["grat"], math.Round(basic*15/26*6), 1) {
			t.Fatalf("gratuity %v for 6 years on basic %v", L["grat"], basic)
		}
		if !near(L["nip"], math.Round(gross/30*days), 1) { // rounded once (EXT-17)
			t.Fatalf("notice pay %v, want %v", L["nip"], math.Round(gross/30*days))
		}
		if L["enc"] <= 0 {
			t.Fatal("privilege leave is encashed when the plan says so")
		}
	})

	t.Run("death: gratuity without the 5-year rule", func(t *testing.T) {
		_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1020", "type": "Death", "lwd": today()})
		c.approveAll(t, r["requestId"].(string))
		if L := lines("EMP-1020"); L["grat"] <= 0 {
			t.Fatalf("gratuity on death %v", L)
		}
	})

	t.Run("absconding: notice not served is recovered", func(t *testing.T) {
		_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1022", "type": "Absconding", "note": "Not reachable since last week"})
		sep := r["id"].(string)
		old := time.Now().AddDate(0, 0, -8).Format("2006-01-02")
		_, _ = st.DB.C("separation").Update(ctx, bson.M{"_id": sep}, bson.M{"$set": bson.M{"show_cause": []map[string]any{{"n": 1, "at": old, "by": "x"}, {"n": 2, "at": time.Now().AddDate(0, 0, -4).Format("2006-01-02"), "by": "x"}}}})
		if s, r := c.do("POST", "/api/separations/"+sep+"/send", "EMP-1004", "HRBP", nil); s != 200 {
			t.Fatal(r)
		}
		var req string
		if x := apiDoc(st, "separation", bson.M{"_id": sep}); x != nil {
			req, _ = x["request_id"].(string)
		}
		c.approveAll(t, req)
		if L := lines("EMP-1022"); L["nns"] <= 0 {
			t.Fatalf("notice not served %v", L)
		}
	})

	t.Run("privacy, withdrawal window and clearance roles", func(t *testing.T) {
		_, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Resignation", "reason": "Higher studies",
			"givenOn": time.Now().AddDate(0, 0, -10).Format("2006-01-02")})
		sep := r["id"].(string)
		if s, r := c.do("POST", "/api/separations/"+sep+"/withdraw", "EMP-1002", "HR Admin", nil); s != 400 || !strings.Contains(fmt.Sprint(r), "withdrawn up to") {
			t.Fatalf("withdrawal after 7 days: %d %v", s, r)
		}
		var req string
		if x := apiDoc(st, "separation", bson.M{"_id": sep}); x != nil {
			req, _ = x["request_id"].(string)
		}
		c.approveAll(t, req)
		_, d := c.do("GET", "/api/separations/"+sep, "EMP-1002", "HR Admin", nil)
		for _, x := range d["clearance"].([]any) {
			m := x.(map[string]any)
			if m["owner"] == "Finance" && m["auto"] == nil {
				if s, _ := c.do("POST", fmt.Sprintf("/api/separations/%s/clearance/%v", sep, m["id"]), "EMP-1004", "HRBP", nil); s != 403 {
					t.Fatal("an HRBP does not sign Finance items")
				}
				break
			}
		}
		ratings := map[string]any{"Manager": 3, "Work and shifts": 3, "Pay and benefits": 3, "Growth": 3, "Workplace and team": 3}
		if s, r := c.do("POST", "/api/separations/"+sep+"/interview", "EMP-1002", "HR Admin", map[string]any{"mainReason": "Higher studies", "ratings": ratings, "recommend": "Maybe"}); s != 200 {
			t.Fatal(r)
		}
		// EMP-1026 reports to EMP-1009, who signs as manager but must not read the exit interview.
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1026"}, bson.M{"$set": bson.M{"manager_id": "EMP-1009"}})
		s, d2 := c.do("GET", "/api/separations/"+sep, "EMP-1009", "Line Manager", nil)
		if s != 200 || d2["exitInterview"] != nil || d2["note"] != nil {
			t.Fatalf("the reporting manager sees the case but not the interview: %d %v", s, d2["exitInterview"])
		}
	})
}

// ---------- Statutory files and tax figures against the formulas ----------

func TestDeepStatutoryAndTax(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	setFirstYm(t, c, "")
	for _, id := range []string{"EMP-1020", "EMP-1002", "EMP-1026"} { // UAN 10010000 and ESIC 310000, each followed by the number in the employee id (12 and 10 digits)
		_, _ = st.DB.C("employee_gov_id").UpdateMany(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"uan": "10010000" + id[4:], "esic": "310000" + id[4:]}})
	}
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.closeAndPublish(t, open)

	t.Run("ECR line", func(t *testing.T) {
		row := c.row(t, open, "EMP-1020")
		r := row["r"].(map[string]any)
		s, txt := c.getRaw("/api/statutory/ecr.txt?ym="+open, "EMP-1012", "Payroll Manager")
		if s != 200 {
			t.Fatal(s)
		}
		var line string
		for _, l := range strings.Split(string(txt), "\n") {
			if strings.HasPrefix(l, "100100001020#~#") {
				line = l
			}
		}
		// Gross wages without reimbursements or bonus; EPS and EDLI wages capped at ₹15,000 (REAL-40, REAL-41); NCP days
		// without the days outside employment (REAL-42).
		epsWage := math.Min(math.Round(num(r["pfBase"])), 15000)
		eps := math.Min(math.Round(epsWage*0.0833), math.Round(num(r["erPF"])))
		want := fmt.Sprintf("100100001020#~#Karan Khan#~#%v#~#%v#~#%v#~#%v#~#%v#~#%v#~#%v#~#%v#~#0", math.Round(num(r["gross"])+num(r["varPay"])), math.Round(num(r["pfBase"])), epsWage,
			epsWage, math.Round(num(r["eePF"])), eps, math.Round(num(r["erPF"]))-eps, math.Max(0, math.Round(num(row["days"])-num(row["paid"])-num(row["outsideP"]))))
		if line != want {
			t.Fatalf("ECR line\n got %s\nwant %s", line, want)
		}
		if strings.Contains(string(txt), "Sandeep Rawat") == false {
			t.Fatal("EMP-1002 has a UAN and PF")
		}
	})

	t.Run("ESI contribution only for covered people", func(t *testing.T) {
		_, r := c.do("GET", "/api/statutory/esic?ym="+open, "EMP-1012", "Payroll Manager", nil)
		ids := map[string]bool{}
		for _, x := range r["rows"].([]any) {
			ids[fmt.Sprint(x.(map[string]any)["cells"].([]any)[1])] = true
		}
		if !ids["Karan Khan"] || ids["Sandeep Rawat"] {
			t.Fatalf("ESI rows %v", ids)
		}
	})

	t.Run("insurance and ESI range files, people already sent", func(t *testing.T) {
		from, to := time.Now().AddDate(0, 0, -7).Format("2006-01-02"), today()
		joined := bson.M{"$in": []string{"EMP-1003", "EMP-1026"}}
		_, _ = st.DB.C("employee").UpdateMany(ctx, bson.M{"_id": joined}, bson.M{"$set": bson.M{"doj": deep2DaysAgo(2)}})
		_, _ = st.DB.C("gross_history").UpdateMany(ctx, bson.M{"employee_id": joined}, bson.M{"$set": bson.M{"from_month": deep2DaysAgo(2)[:7]}})
		_, _ = st.DB.C("gross_history").UpdateMany(ctx, bson.M{"employee_id": "EMP-1026"}, bson.M{"$set": bson.M{"gross": 18000}})
		c.do("POST", "/api/employees/EMP-1005/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": time.Now().AddDate(0, 0, -1).Format("2006-01-02"), "reason": "Resigned"})
		_, ins := c.do("GET", "/api/statutory/insurance?from="+from+"&to="+to, "EMP-1012", "Payroll Manager", nil)
		acts := map[string]string{}
		for _, x := range ins["rows"].([]any) {
			cells := x.(map[string]any)["cells"].([]any)
			acts[cells[1].(string)] = cells[0].(string)
		}
		if acts["EMP-1003"] != "Add" || acts["EMP-1005"] != "Remove" || acts["EMP-1026"] != "" {
			t.Fatalf("insurance actions %v", acts)
		}
		_, esi := c.do("GET", "/api/statutory/esi?from="+from+"&to="+to, "EMP-1012", "Payroll Manager", nil)
		got := false
		for _, x := range esi["rows"].([]any) {
			cells := x.(map[string]any)["cells"].([]any)
			got = got || (cells[1] == "EMP-1026" && cells[0] == "Enrol")
		}
		if !got {
			t.Fatal("EMP-1026 is enrolled in ESI")
		}
		if s, _ := c.getRaw("/api/statutory/insurance/sheet?from="+from+"&to="+to, "EMP-1012", "Payroll Manager"); s != 200 {
			t.Fatal(s)
		}
		_, again := c.do("GET", "/api/statutory/insurance?from="+from+"&to="+to, "EMP-1012", "Payroll Manager", nil)
		if again["sent"] != float64(len(again["rows"].([]any))) {
			t.Fatalf("every row is marked sent: %v of %d", again["sent"], len(again["rows"].([]any)))
		}
		_, raw := c.getRaw("/api/statutory/insurance/sheet?skipSent=1&from="+from+"&to="+to, "EMP-1012", "Payroll Manager")
		f, _ := excelize.OpenReader(bytes.NewReader(raw))
		rows, _ := f.GetRows("Data")
		if len(rows) != 1 {
			t.Fatalf("people already sent are left out: %d rows", len(rows)-1)
		}
	})

	t.Run("tax: old regime HRA, new regime rebate, retainer 26Q, final Form 16", func(t *testing.T) {
		yes := true
		_, me := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		w := me["windows"].(map[string]any)
		c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "declForce": yes, "proofFrom": w["proofFrom"], "proofTo": w["proofTo"]})
		c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", map[string]any{"regime": "Old", "items": map[string]any{"hra": 300000}, "landlordName": "R Kapoor", "landlordPan": "ABCPK1234F", "submit": true})
		_, me = c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		f := me["figure"].(map[string]any)
		g, n := num(f["gross"]), num(f["months"])
		basic, hra := 0.5*g*n, 0.25*g*n
		want := math.Max(0, math.Min(hra, math.Min(300000-0.1*basic, 0.4*basic)))
		if !near(num(f["est"].(map[string]any)["hraExempt"]), want, 1) {
			t.Fatalf("HRA exemption %v, want %v (Gurugram is not a metro)", f["est"].(map[string]any)["hraExempt"], want)
		}
		c.signIn("EMP-1020")
		_, low := c.do("GET", "/api/tax/me", "EMP-1020", "Employee", nil)
		if lf := low["figure"].(map[string]any)["est"].(map[string]any); num(lf["newTax"]) != 0 {
			t.Fatalf("taxable income up to 12 lakh pays no tax: %v", lf["newTax"])
		}
		fy := me["fy"].(string)
		q := map[string]int{"01": 4, "02": 4, "03": 4, "04": 1, "05": 1, "06": 1, "07": 2, "08": 2, "09": 2, "10": 3, "11": 3, "12": 3}[open[5:]]
		s, raw := c.getRaw(fmt.Sprintf("/api/tds/return/26Q/%d/sheet?fy=%s", q, fy), "EMP-1012", "Payroll Manager")
		if s != 200 {
			t.Fatal(s, string(raw))
		}
		x, _ := excelize.OpenReader(bytes.NewReader(raw))
		rows, _ := x.GetRows("Data")
		found := false
		for _, r := range rows {
			found = found || (len(r) > 3 && r[0] == "EMP-1021" && r[3] == "194J")
		}
		if !found {
			t.Fatal("the retainer's 194J TDS is in 26Q")
		}
		march := calcMarch(fy)
		_, _ = st.DB.C("payroll_month").Upsert(ctx, bson.M{"ym": march}, bson.M{"status": "Published"}, nil)
		_, _ = st.DB.C("tds_return").Insert(ctx, bson.M{"fy": fy, "form": "24Q", "quarter": 4, "receipt": "QRCPT1234", "by_name": "x"})
		if s, r := c.do("POST", "/api/form16/release?fy="+fy, "EMP-1012", "Payroll Manager", nil); s != 200 || r["final"] != true {
			t.Fatalf("after March and the Q4 filing Form 16 is final: %v", r)
		}
	})
}

func calcMarch(fy string) string { return fmt.Sprintf("%d-03", atoi4(fy[:4])+1) }

func atoi4(s string) int { n := 0; fmt.Sscanf(s, "%d", &n); return n }

// ---------- Every mapped role against every module: rights, scope and the download right ----------

func TestDeepRoleMatrix(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	type holder struct{ role, emp, access string }
	maps, _ := st.DB.C("user_role_map").Find(ctx, bson.M{"active": true}, options.Find().SetSort(bson.D{{Key: "role_name", Value: 1}}))
	holders := []holder{}
	for _, m := range maps {
		h := holder{}
		h.role, _ = m["role_name"].(string)
		h.emp, _ = m["employee_id"].(string)
		h.access, _ = m["access"].(string)
		holders = append(holders, h)
	}
	signed := map[string]bool{}
	for _, h := range holders {
		if !signed[h.emp] {
			c.signIn(h.emp)
			signed[h.emp] = true
		}
	}
	level := func(role, right string) int {
		if r := apiDoc(st, "role_right", bson.M{"role_name": role, "right_id": right}); r != nil {
			l, _ := r["level"].(int16)
			return int(l)
		}
		return 0
	}
	anyOf := func(role string, rights ...string) bool {
		for _, r := range rights {
			if level(role, r) >= 1 {
				return true
			}
		}
		return false
	}
	from, to := time.Now().AddDate(0, -1, 0).Format("2006-01-02"), today()
	checks := []struct {
		path string
		ok   func(role string) bool
	}{
		{"/api/payroll", func(r string) bool { return level(r, "pay.run") >= 1 }},
		{"/api/comp", func(r string) bool { return anyOf(r, "comp.structure", "comp.bands", "comp.budget", "pay.setup") }},
		{"/api/tax", func(r string) bool { return anyOf(r, "pay.run", "pay.claims") }},
		{"/api/tds", func(r string) bool { return level(r, "pay.run") >= 1 }},
		{"/api/form16", func(r string) bool { return level(r, "pay.run") >= 1 }},
		{"/api/fnf", func(r string) bool { return anyOf(r, "pay.run", "pay.claims") }},
		{"/api/holds", func(r string) bool { return anyOf(r, "pay.claims", "emp.assets") }},
		{"/api/inventory", func(r string) bool { return level(r, "emp.assets") >= 1 }},
		{"/api/reports", func(r string) bool { return level(r, "rep.reports") >= 1 }},
		{"/api/devices", func(r string) bool { return level(r, "att.setup") >= 1 }},
		{"/api/employees", func(r string) bool { return level(r, "emp.directory") >= 1 }},
		// Preview with payroll view rights (round 2, E1); downloads keep pay.export.
		{"/api/statutory/pf?from=" + from + "&to=" + to, func(r string) bool { return level(r, "pay.export") >= 2 || level(r, "pay.run") >= 1 }},
	}
	for _, h := range holders {
		for _, ck := range checks {
			s, body := c.do("GET", ck.path, h.emp, h.role, nil)
			want := ck.ok(h.role)
			if (s == 200) != want || (s != 200 && s != 403) {
				t.Errorf("%s (%s) GET %s: %d, want allowed=%v (%v)", h.role, h.emp, ck.path, s, want, body["error"])
			}
		}
		// Scope: a location-scoped role sees only its places, in lists and in reports.
		if h.access == "locs" {
			in := map[string]bool{}
			if m := apiDoc(st, "user_role_map", bson.M{"employee_id": h.emp, "role_name": h.role}); m != nil {
				locs, _ := m["location_ids"].([]any)
				for _, l := range locs {
					if s, ok := l.(string); ok {
						in[s] = true
					}
				}
			}
			if s, list := c.do("GET", "/api/employees?size=100", h.emp, h.role, nil); s == 200 {
				for _, e := range list["rows"].([]any) {
					if m := e.(map[string]any); !in[m["location_id"].(string)] {
						t.Errorf("%s sees %s outside their places", h.role, m["id"])
					}
				}
			}
			if s, rep := c.do("GET", "/api/reports/emp_master", h.emp, h.role, nil); s == 200 {
				for _, row := range rep["rows"].([]any) {
					var loc string
					if e := apiDoc(st, "employee", bson.M{"_id": row.([]any)[0]}); e != nil {
						loc, _ = e["location_id"].(string)
					}
					if !in[loc] {
						t.Errorf("%s report shows %v outside their places", h.role, row.([]any)[0])
					}
				}
			}
		}
		// The download right: roles without it get no files even where they can view.
		var dl bool
		if r := apiDoc(st, "role", bson.M{"name": h.role}); r != nil {
			dl, _ = r["can_download"].(bool)
		}
		if level(h.role, "rep.reports") >= 2 {
			s, _ := c.getRaw("/api/reports/emp_master/sheet", h.emp, h.role)
			if (s == 200) != dl {
				t.Errorf("%s report download: %d, download right %v", h.role, s, dl)
			}
		}
	}
	// Self-service only for employees: no one else's payslip, tax or separation.
	c.signIn("EMP-1020")
	for _, p := range []string{"/api/payslips?employeeId=EMP-1002", "/api/tax/employee/EMP-1002", "/api/employees/EMP-1002/file", "/api/inventory/employee/EMP-1002"} {
		if s, _ := c.do("GET", p, "EMP-1020", "Employee", nil); s != 403 {
			t.Errorf("employee reached %s: %d", p, s)
		}
	}
}

// TestDeepLeaveInPayroll: approved leave inside the payroll month is paid; loss-of-pay leave is not.
func TestDeepLeaveInPayroll(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	before := c.row(t, open, "EMP-1020")
	if s, r := c.do("POST", "/api/exceptions", "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1020", "kind": "leave", "from": open + "-01", "to": open + "-28", "until": today(), "reason": "Back-dated leave"}); s != 200 {
		t.Fatal(r)
	}
	// Working days only: a date that is a holiday in the city calendar is refused (needs no leave).
	days := []string{dayIn(open, time.Monday, 2), dayIn(open, time.Wednesday, 2), dayIn(open, time.Monday, 3), dayIn(open, time.Wednesday, 3), dayIn(open, time.Thursday, 3)}
	for _, lt := range []string{"Casual leave", "Loss of pay"} {
		for len(days) > 0 {
			d := days[0]
			days = days[1:]
			s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "leave", "payload": map[string]any{"ltype": lt, "from": d, "to": d}})
			if s == 400 && strings.Contains(fmt.Sprint(r), "weekly off or holiday") {
				continue
			}
			if s != 200 {
				t.Fatalf("%s on %s: %v", lt, d, r)
			}
			c.approveAll(t, r["id"].(string))
			break
		}
	}
	after := c.row(t, open, "EMP-1020")
	counts := after["counts"].(map[string]any)
	if num(counts["Paid leave"]) != 1 || num(counts["Loss of pay leave"]) != 1 {
		t.Fatalf("counts %v", counts)
	}
	// Casual leave replaces a day with no punch (one loss-of-pay day less); loss-of-pay leave stays unpaid.
	if num(after["lop"]) != num(before["lop"])-1 {
		t.Fatalf("loss of pay %v, was %v", after["lop"], before["lop"])
	}
}
