package api_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Round 3, reports: every report returns the prototype's columns in its order with the values the
// prototype computes, takes the prototype's filters, keeps the scope and masks confidential values.

type repOut struct {
	cols []string
	rows [][]any
	m    map[string]any
}

func (r repOut) col(name string) int {
	for i, c := range r.cols {
		if c == name {
			return i
		}
	}
	return -1
}

func getRep(t *testing.T, c *client, path, emp, role string) repOut {
	t.Helper()
	s, m := c.do("GET", "/api/reports/"+path, emp, role, nil)
	if s != 200 {
		t.Fatalf("%s as %s: %d %v", path, role, s, m["error"])
	}
	out := repOut{m: m, cols: toStrs(m["cols"].([]any))}
	for _, r := range m["rows"].([]any) {
		out.rows = append(out.rows, r.([]any))
	}
	return out
}

func wantCols(t *testing.T, r repOut, want string) {
	t.Helper()
	if got := strings.Join(r.cols, ", "); got != want {
		t.Fatalf("columns\n got: %s\nwant: %s", got, want)
	}
}

func dayOffset(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

func TestUI3Reports(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1003", "EMP-1004", "EMP-1008", "EMP-1011")
	const hr, hrRole = "EMP-1002", "HR Admin"

	t.Run("filters as in the prototype", func(t *testing.T) {
		_, list := c.do("GET", "/api/reports", hr, hrRole, nil)
		flt, extra := map[string]string{}, map[string]string{}
		for _, x := range list["reports"].([]any) {
			m := x.(map[string]any)
			flt[m["id"].(string)] = strings.Join(toStrs(m["filters"].([]any)), " ")
			ex := []string{}
			for _, e := range m["extra"].([]any) {
				e := e.(map[string]any)
				opts := []string{}
				for _, o := range e["o"].([]any) {
					opts = append(opts, strings.Join(toStrs(o.([]any)), "="))
				}
				ex = append(ex, e["k"].(string)+":"+strings.Join(opts, ","))
			}
			extra[m["id"].(string)] = strings.Join(ex, " ")
		}
		for id, want := range map[string]string{"cost": "month group city dept type", "bank": "month city type", "statutory": "month dept type", "retainer": "month city dept",
			"separations": "city dept type", "tds": "month city dept type", "roster_late": "month city dept", "deviations": "range city dept", "approvals": "city",
			"deductions": "city dept", "activity": "", "late": "month city dept", "mgr_score": "month", "nominations": "city dept", "exit_interviews": "city dept"} {
			if flt[id] != want {
				t.Errorf("%s filters %q, want %q", id, flt[id], want)
			}
		}
		if extra["roster_late"] != "late:=All changes,late=Late changes only" {
			t.Errorf("roster changes: %q", extra["roster_late"])
		}
		if !strings.Contains(extra["deviations"], "Pending=Waiting") {
			t.Errorf("deviations decision: %q", extra["deviations"])
		}
	})

	t.Run("workforce", func(t *testing.T) {
		m := getRep(t, c, "emp_master", hr, hrRole)
		wantCols(t, m, "Code, Name, Type, Status, Date of joining, City, Location, Department, Designation, Grade, Reporting manager, Shift, Work policy, Attendance mode, Exit status")
		for _, r := range m.rows {
			if r[12] == "" {
				t.Fatalf("every employee has a work policy: %v", r)
			}
		}

		// Days left on notice; the exit with tenure and the attrition line.
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"exit_status": "On notice", "lwd": dayOffset(20), "resign_at": store.Today()}})
		n := getRep(t, c, "on_notice", hr, hrRole)
		wantCols(t, n, "Code, Name, Type, Department, City, Resigned on, Last working day, Days left, Reason")
		if len(n.rows) != 1 || n.rows[0][0] != "EMP-1020" || n.rows[0][7] != 20.0 {
			t.Fatalf("days left: %v", n.rows)
		}
		var doj string
		if e, _ := st.DB.C("employee").FindOneAndUpdate(ctx, bson.M{"_id": "EMP-1021"}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": dayOffset(-10), "exit_reason": "Better pay"}}); e != nil {
			doj, _ = e["doj"].(string)
		}
		ex := getRep(t, c, "exits?from="+dayOffset(-15)+"&to="+dayOffset(-5), hr, hrRole)
		wantCols(t, ex, "Code, Name, Type, Department, City, Date of joining, Last working day, Tenure (months), Reason, Status")
		a, b := mustDate(doj), mustDate(dayOffset(-10))
		months := (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
		if b.Day() < a.Day() {
			months--
		}
		got := false
		for _, r := range ex.rows {
			if r[0] == "EMP-1021" {
				got = r[7] == float64(months) && r[9] == "Inactive" && r[8] == "Better pay"
			}
		}
		if !got {
			t.Fatalf("tenure %d: %v", months, ex.rows)
		}
		if note, _ := ex.m["note"].(string); !strings.HasPrefix(note, fmt.Sprintf("%d exit", len(ex.rows))) || !strings.Contains(note, "% annualised") {
			t.Fatalf("attrition note: %q", note)
		}

		// Birthdays and anniversaries for one month.
		ym := time.Now().Format("2006-01")
		if _, err := st.DB.C("employee_personal").Upsert(ctx, bson.M{"employee_id": "EMP-1022"}, bson.M{"dob": "1990-" + ym[5:] + "-15"}, nil); err != nil {
			t.Fatal(err)
		}
		bd := getRep(t, c, "birthdays?ym="+ym, hr, hrRole)
		wantCols(t, bd, "Event, Code, Name, Department, City, Date, Years")
		found := false
		for _, r := range bd.rows {
			if r[0] == "Birthday" && r[1] == "EMP-1022" && r[5] == ym+"-15" {
				found = true
			}
			if r[0] == "Work anniversary" && !strings.HasSuffix(r[6].(string), " years") {
				t.Fatalf("years: %v", r)
			}
		}
		if !found {
			t.Fatalf("birthday on the 15th: %v", bd.rows)
		}

		// Probation: days from today to the end.
		pr := getRep(t, c, "probation", hr, hrRole)
		wantCols(t, pr, "Code, Name, Type, Department, City, Date of joining, Probation (months), Probation ends, Days from today, Reporting manager")
		for _, r := range pr.rows {
			end := mustDate(r[7].(string))
			if d := int(end.Sub(mustDate(today())).Hours() / 24); float64(d) != r[8] {
				t.Fatalf("days from today %d: %v", d, r)
			}
		}

		// The monthly gross on joiners only with the salary right.
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1023"}, bson.M{"$set": bson.M{"doj": store.DateOf(time.Now().AddDate(0, 0, -3))}})
		j := getRep(t, c, "joiners?from="+dayOffset(-5)+"&to="+today(), hr, hrRole)
		wantCols(t, j, "Code, Name, Type, Date of joining, City, Location, Department, Designation, Grade, Reporting manager, "+
			"Father or spouse name, Date of birth, Gender, Mobile, Email, Aadhaar, PAN, UAN, Existing UAN?, ESIC number, Bank name, Bank account, IFSC, Monthly gross, "+
			"PF nominee 1, PF nominee 1 relation, PF nominee 1 date of birth, PF nominee 1 share %, Medical member 1, Medical member 1 relation, Medical member 1 date of birth")
		got = false
		for _, r := range j.rows {
			if g, ok := r[j.col("Monthly gross")].(float64); r[0] == "EMP-1023" && ok && g > 0 {
				got = true
			}
		}
		if !got {
			t.Fatalf("joiner with the monthly gross: %v", j.rows)
		}
		if pm := getRep(t, c, "joiners", "EMP-1008", "Property Manager"); pm.col("Monthly gross") >= 0 {
			t.Fatal("no gross without the salary right")
		}

		// Separations: clearance by owner; the employment type filter.
		sp := getRep(t, c, "separations", hr, hrRole)
		wantCols(t, sp, "Case, Code, Name, Location, Department, Type, Reason, Started, Last working day, Stage, Manager clearance, HRBP clearance, Finance clearance, Exit interview, Rehire, Full and final")
		ei := getRep(t, c, "exit_interviews", hr, hrRole)
		wantCols(t, ei, "Code, Name, Location, Department, Reporting manager, Last working day, Main reason, Would recommend, Manager, Work and shifts, Pay and benefits, Growth, Workplace and team, Comments")
		if s, _ := c.do("GET", "/api/reports/exit_interviews", "EMP-1011", "Payroll", nil); s != 404 {
			t.Fatal("exit interviews are for HR")
		}
	})

	t.Run("attendance and roster", func(t *testing.T) {
		ym := time.Now().Format("2006-01")
		l := getRep(t, c, "late?ym="+ym, hr, hrRole)
		wantCols(t, l, "Code, Name, Location, Department, Shift, Late days, Late share of working days, Reporting manager")
		for _, r := range l.rows {
			if !strings.HasSuffix(r[6].(string), "%") {
				t.Fatalf("late share: %v", r)
			}
		}
		at := getRep(t, c, "att_summary?ym="+ym, hr, hrRole)
		wantCols(t, at, "Code, Name, City, Department, Present, Late, Half day, Missing punch, No punch, Loss of pay days, Days after last working day")

		// Roster changes: all of them, or late ones only, with the timing.
		loc, _ := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})["location_id"].(string)
		for i, late := range []bool{true, false} {
			if _, err := st.DB.C("roster_log").Insert(ctx, bson.M{"employee_id": "EMP-1020", "date": ym + fmt.Sprintf("-%02d", i+1), "from_code": "M", "to_code": "E",
				"why": "Guest demand", "late": late, "location_id": loc, "department": "F&B", "by_name": "Rakesh Bajaj"}); err != nil {
				t.Fatal(err)
			}
		}
		rc := getRep(t, c, "roster_late?ym="+ym, hr, hrRole)
		wantCols(t, rc, "Date, Code, Name, Location, Department, From shift, To shift, Changed by, Timing, Reason")
		if len(rc.rows) != 2 || rc.rows[0][8] != "Late" || rc.rows[1][8] != "In time" || rc.rows[0][5] != "M" || rc.rows[0][6] != "E" {
			t.Fatalf("all changes: %v", rc.rows)
		}
		if lo := getRep(t, c, "roster_late?late=late&ym="+ym, hr, hrRole); len(lo.rows) != 1 || lo.rows[0][8] != "Late" {
			t.Fatalf("late changes only: %v", lo.rows)
		}
		if _, ok := rc.m["note"].(string); !ok {
			t.Fatal("the alert line")
		}

		dv := getRep(t, c, "deviations?ds=Pending", hr, hrRole)
		wantCols(t, dv, "Date, Code, Name, Location, Department, Reporting manager, Planned, Actual, In, Out, Hours, What differs, Decision, Decided by, Note")
		for _, r := range dv.rows {
			if r[12] != "Pending" {
				t.Fatalf("pending only: %v", r)
			}
		}
		ds := getRep(t, c, "disc_score?ym="+ym, hr, hrRole)
		wantCols(t, ds, "Code, Name, Location, Department, Reporting manager, Rostered days, Deviations, OK, Not OK, Waiting, Started at, Score, Band")
		ms := getRep(t, c, "mgr_score?ym="+ym, hr, hrRole)
		wantCols(t, ms, "Manager, Team, Rostered days, Deviations, Deviation share %, Marked OK, Marked Not OK, Waiting, Late roster changes, All roster changes, Score, Flag")
		var mgr string
		if e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"}); e["manager_id"] != nil {
			mgr, _ = apiDoc(st, "employee", bson.M{"_id": e["manager_id"]})["name"].(string)
		}
		changed := false
		for _, r := range ms.rows {
			if r[0] == mgr {
				changed = r[9].(float64) >= 2 && r[8].(float64) >= 1
			}
			days, dev := r[2].(float64), r[3].(float64)
			if days > 0 {
				if share := float64(int(dev/days*1000+0.5)) / 10; r[4] != share {
					t.Fatalf("deviation share %v: %v", share, r)
				}
			}
			if f := r[11].(string); f != "" && f != "Roster not matching the team" {
				t.Fatalf("flag: %v", r)
			}
		}
		if !changed {
			t.Fatalf("the manager of EMP-1020 has both roster changes, one late: %v", ms.rows)
		}
		cn := getRep(t, c, "consequences", hr, hrRole)
		wantCols(t, cn, "Date, Code, Name, Location, Department, Type, Number, PIP days, Reason, Issued by, Approver role, Decided by, Plan from, Plan to, Status, Result")
		lv := getRep(t, c, "leave_bal", hr, hrRole)
		wantCols(t, lv, "Code, Name, Department, City, CL balance, CL used, PL balance, PL used, SL balance, SL used")
	})

	t.Run("payroll", func(t *testing.T) {
		_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
		open, _ := state["openYm"].(string)
		c.pcCloseAll(t, open)
		if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
			t.Fatal(r)
		}
		reg := getRep(t, c, "register?ym="+open, "EMP-1011", "Payroll")
		wantCols(t, reg, "Code, Name, Type, City, Paid days, Total earnings, PF, ESI, TDS, Total deductions, Rounding off, Net pay, Bank")
		net := 0.0
		for _, r := range reg.rows {
			net += r[11].(float64)
			short := r[5].(float64) < r[9].(float64) && r[11].(float64) == 0                             // deductions above earnings: the rest is carried to next month
			if !short && calc.Round2(r[5].(float64)-r[9].(float64)+r[10].(float64)) != r[11].(float64) { // the row foots (ITM-59)
				t.Fatalf("register row does not foot: %v", r)
			}
		}
		sum := reg.m["sum"].([]any)
		if len(reg.rows) == 0 || sum[0] != "Total" || sum[11] != net || sum[12] != "" {
			t.Fatalf("total row %v, net %v", sum, net)
		}
		if fmt.Sprint(reg.m["money"]) != "[5 6 7 8 9 10 11]" {
			t.Fatalf("money columns %v", reg.m["money"])
		}
		// The download has the same columns and the total row.
		s, csv := c.getRaw("/api/reports/register/sheet?format=csv&ym="+open, "EMP-1002", "HR Admin")
		lines := strings.Split(strings.TrimSpace(string(bytes.TrimPrefix(csv, []byte("\xef\xbb\xbf")))), "\n")
		if s != 200 || lines[0] != strings.Join(reg.cols, ",") || !strings.HasPrefix(lines[len(lines)-1], "Total,") {
			t.Fatalf("csv %d %q … %q", s, lines[0], lines[len(lines)-1])
		}

		// Cost by gender; statutory by department; retainers by department; TDS by type.
		cg := getRep(t, c, "cost?group=gender&ym="+open, "EMP-1011", "Payroll")
		wantCols(t, cg, "Group, People paid, Gross earnings, Employer contributions, Total cost to company")
		people := 0.0
		for _, r := range cg.rows {
			if r[0] != "Male" && r[0] != "Female" && r[0] != "Not recorded" && r[0] != "Other" {
				t.Fatalf("gender groups: %v", cg.rows)
			}
			if r[4] != r[2].(float64)+r[3].(float64) && r[4].(float64)-r[2].(float64)-r[3].(float64) > 1 {
				t.Fatalf("total cost: %v", r)
			}
			people += r[1].(float64)
		}
		if int(people) != len(reg.rows) {
			t.Fatalf("everyone paid is in a group: %v of %d", people, len(reg.rows))
		}
		if fb := getRep(t, c, "cost?group=city&dept=F%26B&ym="+open, "EMP-1011", "Payroll"); len(fb.rows) == 0 || fb.m["sum"].([]any)[1].(float64) >= people {
			t.Fatal("the department filter on cost")
		}
		stat := getRep(t, c, "statutory?ym="+open, "EMP-1011", "Payroll")
		wantCols(t, stat, "City, Employee PF, Employer PF, EPFO admin, Employee ESI, Employer ESI, LWF employee, LWF employer, TDS, Medical insurance")
		statFB := getRep(t, c, "statutory?dept=F%26B&ym="+open, "EMP-1011", "Payroll")
		if fmt.Sprint(statFB.m["sum"]) == fmt.Sprint(stat.m["sum"]) {
			t.Fatal("the department filter on the statutory summary")
		}
		rt := getRep(t, c, "retainer?dept=F%26B&ym="+open, "EMP-1011", "Payroll")
		wantCols(t, rt, "Code, Name, Type, City, Paid days, Amount, TDS, Net payment")
		for _, r := range rt.rows {
			var d string
			if e := apiDoc(st, "employee", bson.M{"_id": r[0]}); e != nil {
				d, _ = apiDoc(st, "department", bson.M{"_id": e["department_id"]})["name"].(string)
			}
			if d != "F&B" || r[2] == "Permanent" {
				t.Fatalf("retainers in F&B: %v", r)
			}
		}
		td := getRep(t, c, "tds?type=Retainer&ym="+open, "EMP-1011", "Payroll")
		wantCols(t, td, "Code, Name, Type, Section, Regime, Yearly tax, Deducted this year, TDS for the month, Still to deduct")
		if len(td.rows) == 0 {
			t.Fatal("retainers have TDS under 194J")
		}
		for _, r := range td.rows {
			if r[2] != "Retainer" || r[3] != "194J" {
				t.Fatalf("type filter: %v", r)
			}
		}
		perm := getRep(t, c, "tds?type=Permanent&ym="+open, "EMP-1011", "Payroll")
		for _, r := range perm.rows {
			if d := r[8].(float64) - max(0, r[5].(float64)-r[6].(float64)); r[3] != "192" || d > 1 || d < -1 {
				t.Fatalf("still to deduct: %v", r)
			}
		}
		bk := getRep(t, c, "bank?ym="+open, "EMP-1011", "Payroll")
		wantCols(t, bk, "Code, Name, Bank account, IFSC, Net pay")
		if note, _ := bk.m["note"].(string); len(bk.rows) != 0 || !strings.Contains(note, "approved") {
			t.Fatalf("bank advice waits for approval: %v", bk.m)
		}

		// Masking: a role whose salary mode is Masked sees masked money and a masked total.
		_, _ = st.DB.C("role_sensitive").UpdateMany(ctx, bson.M{"role_name": "Payroll", "grp": "salary"}, bson.M{"$set": bson.M{"mode": "Masked"}})
		mk := getRep(t, c, "register?ym="+open, "EMP-1011", "Payroll")
		for _, r := range mk.rows {
			if r[0] == "EMP-1011" {
				continue // own pay stays in full
			}
			if v, ok := r[11].(string); !ok || !strings.Contains(v, "••••") {
				t.Fatalf("net pay masked: %v", r)
			}
			if _, ok := r[4].(float64); !ok {
				t.Fatalf("paid days are not confidential: %v", r)
			}
		}
		if v, _ := mk.m["sum"].([]any)[11].(string); !strings.Contains(v, "••••") {
			t.Fatalf("total masked: %v", mk.m["sum"])
		}
		_, _ = st.DB.C("role_sensitive").UpdateMany(ctx, bson.M{"role_name": "HRBP Corporate", "grp": "salary"}, bson.M{"$set": bson.M{"mode": "Masked"}})
		if jg := getRep(t, c, "joiners", "EMP-1003", "HRBP Corporate"); jg.col("Monthly gross") < 0 {
			t.Fatal("HRBP Corporate has the salary right")
		} else {
			for _, r := range jg.rows {
				if v, ok := r[jg.col("Monthly gross")].(string); r[0] != "EMP-1003" && r[jg.col("Monthly gross")] != nil && (!ok || !strings.Contains(v, "••••")) {
					t.Fatalf("gross masked: %v", r)
				}
			}
		}
		_, _ = st.DB.C("role_sensitive").UpdateMany(ctx, bson.M{"grp": "salary", "role_name": bson.M{"$in": bson.A{"Payroll", "HRBP Corporate"}}}, bson.M{"$set": bson.M{"mode": "Full"}})

		// Holds: the held amount, hidden from a role without salary rights.
		if s, r := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1020", "reason": "Absent without notice", "note": "Not reporting since Monday", "fromYm": open}); s != 200 {
			t.Fatal(r)
		}
		h := getRep(t, c, "holds", hr, hrRole)
		wantCols(t, h, "Hold, Code, Name, Reason, Details, From, Held (₹), Status, Raised by, Raised on, Released by, Released on, Release note")
		if len(h.rows) != 1 || h.rows[0][1] != "EMP-1020" || h.rows[0][7] != "Active" || h.rows[0][6] != 0.0 || h.rows[0][3] != "Absent without notice" {
			t.Fatalf("active hold, nothing held before a month is approved: %v", h.rows)
		}
		if pm := getRep(t, c, "holds", "EMP-1008", "Property Manager"); len(pm.rows) != 1 || pm.rows[0][6] != nil {
			t.Fatalf("held amount hidden without salary rights: %v", pm.rows)
		}
		dd := getRep(t, c, "deductions", hr, hrRole)
		wantCols(t, dd, "Deduction, Code, Name, Type, Amount (₹), Instalments, First month, Status, Employee heard, Raised by, Raised on, Details")
		pl := getRep(t, c, "pay_lines?ym="+open, hr, hrRole)
		wantCols(t, pl, "Code, Name, Kind, Item, Amount, Source, Reason")
	})

	t.Run("approvals, compliance and inventory", func(t *testing.T) {
		if s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1022",
			"payload": map[string]any{"amount": 900, "instalments": 1, "dedType": "Cash shortage", "note": "Till short", "heard": "Yes"}}); s != 200 {
			t.Fatal(r)
		}
		ap := getRep(t, c, "approvals?wf=p_ded&rs=Pending", hr, hrRole) // no range: all dates
		wantCols(t, ap, "Request, Workflow, Employee, Department, City, Status, Waiting on, Stage, Raised")
		if len(ap.rows) != 1 || ap.rows[0][1] != "Salary deduction" || ap.rows[0][6] == "" {
			t.Fatalf("pending deduction: %v", ap.rows)
		}
		wl := getRep(t, c, "workload", hr, hrRole)
		wantCols(t, wl, "Waiting on, Pending requests, Of which")
		ok := false
		for _, r := range wl.rows {
			if r[0] == ap.rows[0][6] && strings.Contains(r[2].(string), "Salary deduction 1") {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("workload by stage: %v", wl.rows)
		}
		ms := getRep(t, c, "masters", hr, hrRole)
		wantCols(t, ms, "Master, Name, Status, Under, Active employees using it")
		// A location with its cluster.
		var loc, cluster string
		locs, _ := st.DB.C("location").Find(ctx, nil)
		for _, l := range locs {
			if k := apiDoc(st, "cluster", bson.M{"_id": l["cluster_id"]}); k != nil {
				loc, _ = l["name"].(string)
				cluster, _ = k["name"].(string)
				break
			}
		}
		found := false
		for _, r := range ms.rows {
			if r[0] == "Location" && r[1] == loc {
				found = r[3] == cluster
			}
		}
		if !found {
			t.Fatalf("a location sits under its cluster (%s, %s)", loc, cluster)
		}
		au := getRep(t, c, "activity", hr, hrRole)
		wantCols(t, au, "When, What happened")
		if len(au.rows) == 0 || !strings.Contains(au.rows[0][1].(string), " · ") {
			t.Fatalf("activity: %v", au.rows)
		}
		mi := getRep(t, c, "missing", hr, hrRole)
		wantCols(t, mi, "Code, Name, Type, Department, City, Status, Missing, Count")
		for _, r := range mi.rows {
			if float64(len(strings.Split(r[6].(string), ", "))) != r[7] {
				t.Fatalf("count of missing: %v", r)
			}
		}
		iv := getRep(t, c, "inv_moves", hr, hrRole)
		wantCols(t, iv, "Store, Item, Opening, Additions (purchase), Transferred in, Returned by employees, Issued to employees, Transferred out, Written off, Closing, Unit price, Closing value, Serial check")
		ie := getRep(t, c, "inv_deductions", hr, hrRole)
		wantCols(t, ie, "Date, Store, Employee code, Name, City, Movement, Item, Qty, Value, Salary deduction plan, Deducted in this month")
	})

	t.Run("scope", func(t *testing.T) {
		// The people at the HRBP's mapped places.
		in := map[string]bool{}
		if m := apiDoc(st, "user_role_map", bson.M{"employee_id": "EMP-1004", "role_name": "HRBP"}); m != nil {
			emps, _ := st.DB.C("employee").Find(ctx, bson.M{"location_id": bson.M{"$in": m["location_ids"]}})
			for _, e := range emps {
				id, _ := e["id"].(string)
				in[id] = true
			}
		}
		for _, p := range []string{"nominations", "deviations", "disc_score", "missing", "leave_bal", "late", "separations"} {
			r := getRep(t, c, p, "EMP-1004", "HRBP")
			code := r.col("Code")
			for _, row := range r.rows {
				if !in[row[code].(string)] {
					t.Fatalf("%s shows %v outside the HRBP's places", p, row[code])
				}
			}
		}
	})
}
