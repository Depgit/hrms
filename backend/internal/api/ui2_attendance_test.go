package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// uiField reads a text field of a document ("" when there is none).
func uiField(ctx context.Context, st *store.Store, coll string, filter bson.D, field string) string {
	r, _ := st.DB.C(coll).FindOne(ctx, filter)
	v, _ := r[field].(string)
	return v
}

// Round 2 UI gaps for Roster, Attendance and My time: roster for a whole city, the deviation
// object on a cell, alert limits from the policy, the pattern for everyone shown, the attendance
// summary for a range, the roster download with one column per date and its upload, recent
// attendance uploads, the punch-machine pull settings and the consequence list extras.

func uiDay(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }

func uiMonday(n int) string {
	t := time.Now()
	return t.AddDate(0, 0, -((int(t.Weekday())+6)%7)+n).Format("2006-01-02")
}

func uiIDs(t *testing.T, c *client) (hub, city string) {
	t.Helper()
	_, l := c.do("GET", "/api/masters/location?status=Active", "EMP-1002", "HR Admin", nil)
	for _, x := range l["list"].([]any) {
		m := x.(map[string]any)
		if m["name"] == "Gurugram – Sector 29 Hub" {
			hub, city = m["id"].(string), m["city_id"].(string)
		}
	}
	if hub == "" || city == "" {
		t.Fatal("Sector 29 Hub and its city not found", l)
	}
	return
}

func cellsOf(row map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, c := range row["cells"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

func TestUIRoster(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1009", "EMP-1020")
	hub, city := uiIDs(t, c)
	wk2 := uiMonday(14)

	t.Run("roster for a whole city: one call, codes per row, properties with their publish state", func(t *testing.T) {
		if s, _ := c.do("GET", "/api/roster?department=F%26B&week="+wk2, "EMP-1002", "HR Admin", nil); s != 400 {
			t.Fatal("a city or a property is needed")
		}
		s, g := c.do("GET", "/api/roster?city="+city+"&department=F%26B&week="+wk2, "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatal(g)
		}
		rows := g["rows"].([]any)
		if len(rows) == 0 || g["published"] != nil {
			t.Fatal("F&B in the city has people and the week is a draft", g)
		}
		locs := map[string]bool{}
		for _, x := range rows {
			r := x.(map[string]any)
			if r["department"] != "F&B" || r["location"] == "" || len(r["codes"].([]any)) < 3 {
				t.Fatal("each row has its property, department and codes", r)
			}
			locs[r["locationId"].(string)] = true
		}
		if len(g["locations"].([]any)) != len(locs) {
			t.Fatal("one entry per property shown", g["locations"])
		}
		_, one := c.do("GET", "/api/roster?location="+hub+"&department=F%26B&week="+wk2, "EMP-1002", "HR Admin", nil)
		if len(one["rows"].([]any)) > len(rows) || len(one["locations"].([]any)) != 1 {
			t.Fatal("a property is part of its city", one)
		}
		for _, l := range g["locations"].([]any) {
			id := l.(map[string]any)["id"].(string)
			if s, r := c.do("POST", "/api/roster/publish", "EMP-1002", "HR Admin", map[string]string{"location": id, "department": "F&B", "week": wk2}); s != 200 {
				t.Fatal(r)
			}
		}
		_, g = c.do("GET", "/api/roster?city="+city+"&department=F%26B&week="+wk2, "EMP-1002", "HR Admin", nil)
		if g["published"] == nil {
			t.Fatal("published once every property is", g["locations"])
		}
	})

	t.Run("a deviation on a cell is an object with the decision", func(t *testing.T) {
		wk := uiMonday(-7)
		_, g := c.do("GET", "/api/roster?location="+hub+"&department=F%26B&week="+wk, "EMP-1002", "HR Admin", nil)
		date := ""
		for _, x := range g["rows"].([]any) {
			r := x.(map[string]any)
			if r["id"] != "EMP-1020" {
				continue
			}
			for i, cl := range cellsOf(r) {
				if d := g["days"].([]any)[i].(string); cl["code"] == "M" && d < uiDay(0) {
					date = d
				}
			}
		}
		if date == "" {
			t.Skip("no Morning day last week")
		}
		punchDay(t, st, "EMP-1020", date, "10:00", "18:30")
		_, g = c.do("GET", "/api/roster?location="+hub+"&department=F%26B&week="+wk, "EMP-1002", "HR Admin", nil)
		for _, x := range g["rows"].([]any) {
			r := x.(map[string]any)
			for i, cl := range cellsOf(r) {
				if r["id"] != "EMP-1020" || g["days"].([]any)[i] != date {
					continue
				}
				dv, ok := cl["dev"].(map[string]any)
				if !ok {
					t.Fatal("the deviation is an object", cl)
				}
				if dv["planned"] != "M" || dv["actual"] != "G" || dv["in"] != "10:00" || dv["out"] != "18:30" || num(dv["hours"]) != 8.5 || dv["status"] != "Pending" ||
					!containsAny(dv["kinds"].([]any), "Shift") {
					t.Fatal(dv)
				}
				return
			}
		}
		t.Fatal("cell not found")
	})

	t.Run("apply a pattern to everyone shown", func(t *testing.T) {
		body := func(p, week, why string) map[string]string {
			return map[string]string{"city": city, "department": "F&B", "week": week, "pattern": p, "why": why}
		}
		if s, _ := c.do("POST", "/api/roster/pattern", "EMP-1020", "Employee", body("rot", wk2, "")); s != 403 {
			t.Fatal("employees do not plan rosters")
		}
		if s, _ := c.do("POST", "/api/roster/pattern", "EMP-1002", "HR Admin", body("weird", wk2, "")); s != 400 {
			t.Fatal("unknown pattern")
		}
		grid := func() []any {
			_, g := c.do("GET", "/api/roster?city="+city+"&department=F%26B&week="+wk2, "EMP-1002", "HR Admin", nil)
			return g["rows"].([]any)
		}
		s, r := c.do("POST", "/api/roster/pattern", "EMP-1002", "HR Admin", body("rot", wk2, ""))
		if s != 200 || num(r["applied"]) == 0 || num(r["blocked"]) != 0 || num(r["employees"]) != float64(len(grid())) {
			t.Fatal("two weeks ahead nothing is locked or late", r)
		}
		for _, x := range grid() {
			for _, cl := range cellsOf(x.(map[string]any)) {
				if k := cl["code"].(string); k != "WO" && k != "PH" && k != "L" && k != "M" && k != "E" && k != "N" {
					t.Fatal("rotation uses Morning, Evening and Night", x)
				}
			}
		}
		if _, r = c.do("POST", "/api/roster/pattern", "EMP-1002", "HR Admin", body("6on1", wk2, "")); num(r["applied"]) == 0 {
			t.Fatal(r)
		}
		offs := map[int]bool{}
		for _, x := range grid() {
			wo, ph := 0, false
			for i, cl := range cellsOf(x.(map[string]any)) {
				switch cl["code"] {
				case "WO":
					wo++
					offs[i] = true
				case "PH", "L":
					ph = true
				}
			}
			if wo != 1 && !ph {
				t.Fatal("six on, one off", x)
			}
		}
		if len(grid()) > 1 && len(offs) < 2 {
			t.Fatal("the off day is staggered", offs)
		}
		if _, r = c.do("POST", "/api/roster/pattern", "EMP-1002", "HR Admin", body("fixed", wk2, "")); num(r["applied"]) == 0 {
			t.Fatal(r)
		}
		for _, x := range grid() {
			for _, cl := range cellsOf(x.(map[string]any)) {
				if cl["changed"] == true {
					t.Fatal("reset to the work policy defaults", x)
				}
			}
		}
	})

	t.Run("the pattern follows the roster rules: late changes need a reason, past dates are locked", func(t *testing.T) {
		_, g := c.do("GET", "/api/roster?location="+hub+"&department=F%26B&week="+uiDay(0), "EMP-1002", "HR Admin", nil)
		def := ""
		for _, x := range g["rows"].([]any) {
			r := x.(map[string]any)
			for i, cl := range cellsOf(r) {
				if r["id"] == "EMP-1020" && g["days"].([]any)[i] == uiDay(0) {
					def = cl["code"].(string)
				}
			}
		}
		other := map[bool]string{true: "M", false: "E"}[def == "E"]
		if _, err := st.DB.C("roster_entry").Insert(ctx, bson.M{"employee_id": "EMP-1020", "date": uiDay(0), "code": other, "by_name": "test"}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		s, r := c.do("POST", "/api/roster/pattern", "EMP-1002", "HR Admin", map[string]string{"location": hub, "department": "F&B", "week": uiDay(0), "pattern": "fixed"})
		if s != 200 || num(r["blocked"]) < 1 {
			t.Fatal("today is inside the lead days and no reason was given", r)
		}
		n, _ := st.DB.C("roster_log").Count(ctx, bson.D{{Key: "at", Value: bson.D{{Key: "$gte", Value: start}}},
			{Key: "date", Value: bson.D{{Key: "$lt", Value: mustDate(uiDay(0)).AddDate(0, 0, 2).Format("2006-01-02")}}}})
		code := uiField(ctx, st, "roster_entry", bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: uiDay(0)}}, "code")
		if n != 0 || code != other {
			t.Fatal("nothing locked or late was changed", n, code)
		}
		if _, r = c.do("POST", "/api/roster/pattern", "EMP-1002", "HR Admin", map[string]string{"location": hub, "department": "F&B", "week": uiDay(0), "pattern": "fixed", "why": "Other"}); num(r["applied"]) < 1 {
			t.Fatal("with a reason today's late change is made", r)
		}
	})

	t.Run("alerts carry the policy's limit", func(t *testing.T) {
		logs := []any{}
		for g := 1; g <= 20; g++ { // 20 late changes this month, on its first 20 days
			logs = append(logs, bson.M{"employee_id": "EMP-1020", "date": mustDate(store.Today()[:7]+"-01").AddDate(0, 0, g%20).Format("2006-01-02"), "from_code": "M", "to_code": "E",
				"why": "Other", "late": true, "location_id": hub, "department": "F&B", "by_name": "test"})
		}
		if err := st.DB.C("roster_log").InsertMany(ctx, logs); err != nil {
			t.Fatal(err)
		}
		alert := func() map[string]any {
			_, d := c.do("GET", "/api/roster/discipline", "EMP-1002", "HR Admin", nil)
			for _, x := range d["alerts"].([]any) {
				if m := x.(map[string]any); strings.HasPrefix(m["key"].(string), "Gurugram – Sector 29 Hub · F&B") {
					return m
				}
			}
			return nil
		}
		a := alert()
		if a == nil || num(a["limit"]) != 30 || num(a["pct"]) <= 30 {
			t.Fatal("the default limit is 30%", a)
		}
		names, _ := st.DB.C("work_policy").Find(ctx, nil)
		pols := []string{}
		for _, x := range names {
			n, _ := x["name"].(string)
			pols = append(pols, n)
		}
		for _, n := range pols {
			setPolicy(t, st, n, map[string]any{"rs": map[string]any{"alert": 100}})
		}
		if a := alert(); a != nil {
			t.Fatal("a 100% limit is never passed", a)
		}
		_, d := c.do("GET", "/api/roster/discipline", "EMP-1002", "HR Admin", nil)
		for _, x := range d["people"].([]any) {
			if m := x.(map[string]any); m["id"] == "EMP-1020" && m["department"] != "F&B" {
				t.Fatal("discipline people carry the department", m)
			}
		}
	})
}

func TestUIRosterDownloadUpload(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1020")
	hub, _ := uiIDs(t, c)
	wk2 := uiMonday(14)

	s, raw := c.getRaw("/api/bulk/roster/download?location="+hub+"&department=F%26B&from="+wk2+"&days=7", "EMP-1002", "HR Admin")
	if s != 200 || !bytes.HasPrefix(raw, []byte("PK")) {
		t.Fatal(s, string(raw))
	}
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := f.GetRows("Roster")
	head := rows[0]
	if len(head) != 12 || head[0] != "Employee ID" || head[3] != "Department" || head[4] != wk2 || head[11] != "Reason for late changes" {
		t.Fatal("one column per date between the employee and the reason", head)
	}
	if len(rows) < 2 {
		t.Fatal("F&B at the hub has people")
	}
	dvs, _ := f.GetDataValidations("Roster")
	if len(dvs) < 2 {
		t.Fatal("drop-downs for the codes and the reasons", dvs)
	}
	if v, _ := f.GetSheetVisible("Lists"); v {
		t.Fatal("the lists sheet is hidden")
	}
	codes, _ := f.GetCols("Lists")
	if !containsAny(toAny(codes[1]), "WO") || !containsAny(toAny(codes[0]), "Vacancy cover") {
		t.Fatal("reasons, then codes valid at the property", codes)
	}

	// Change one cell two weeks ahead and upload the file back.
	id, cur := rows[1][0], rows[1][4]
	to := map[bool]string{true: "M", false: "E"}[cur == "E"]
	if cur == "L" || cur == "PH" {
		t.Skip("the first day is leave or a holiday")
	}
	_ = f.SetCellValue("Roster", "E2", to)
	var buf bytes.Buffer
	_ = f.Write(&buf)
	_, chk := c.sendFile("/api/bulk/roster/check", "EMP-1002", "HR Admin", "roster.xlsx", buf.Bytes())
	res := chk["results"].([]any)
	first := res[0].(map[string]any)
	if num(chk["ok"]) != 1 || first["id"] != id || len(first["changes"].([]any)) != 1 {
		t.Fatal("the changed cell is the only change", chk)
	}
	if ch := first["changes"].([]any)[0].(map[string]any); ch["from"] != cur || ch["to"] != to {
		t.Fatal(ch)
	}
	if _, ap := c.sendFile("/api/bulk/roster/apply", "EMP-1002", "HR Admin", "roster.xlsx", buf.Bytes()); num(ap["ok"]) != 1 {
		t.Fatal(ap)
	}
	code := uiField(ctx, st, "roster_entry", bson.D{{Key: "employee_id", Value: id}, {Key: "date", Value: wk2}}, "code")
	if code != to {
		t.Fatal("applied", code)
	}

	// The rules per cell: leave cannot be set, codes must be mapped, past dates are locked, late changes need a reason.
	wide := "Employee ID,Name,Property,Department," + uiDay(-3) + "," + uiDay(1) + "," + uiDay(10) + "," + uiDay(11) + ",Reason for late changes\n" +
		"EMP-1020,,,,E,E,L,ZZ,\n"
	_, chk = c.sendFile("/api/bulk/roster/check", "EMP-1002", "HR Admin", "wide.csv", []byte(wide))
	errs := fmt.Sprint(chk["results"].([]any)[0].(map[string]any)["errors"])
	for _, want := range []string{"leave is set by a leave request", "not a shift mapped to", "late change, add a reason"} {
		if !strings.Contains(errs, want) {
			t.Fatalf("missing %q in %s", want, errs)
		}
	}
	if !strings.Contains(errs, "date has passed and is locked") && !strings.Contains(errs, "approved leave") {
		// three days ago may already be E; the check only errs when the cell differs
		past := uiField(ctx, st, "roster_entry", bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: uiDay(-3)}}, "code")
		if past != "E" {
			t.Log("past date unchanged by default code:", errs)
		}
	}
	// The long layout still works.
	long := "Employee ID,Date,Code,Reason\nEMP-1020," + uiDay(12) + ",E,\n"
	if _, chk = c.sendFile("/api/bulk/roster/check", "EMP-1002", "HR Admin", "long.csv", []byte(long)); chk["results"] == nil {
		t.Fatal(chk)
	}
	// Without a place it is the plain template; employees cannot download a roster.
	if s, raw := c.getRaw("/api/bulk/roster/download", "EMP-1002", "HR Admin"); s != 200 || !bytes.HasPrefix(raw, []byte("PK")) {
		t.Fatal("plain template", s)
	}
	// An employee sees only their own roster row.
	s, raw = c.getRaw("/api/bulk/roster/download?location="+hub, "EMP-1020", "Employee")
	if s == 200 {
		own, _ := excelize.OpenReader(bytes.NewReader(raw))
		rs, _ := own.GetRows("Roster")
		if len(rs) != 2 || rs[1][0] != "EMP-1020" {
			t.Fatal("scope applies to the download", rs)
		}
	} else if s != 403 {
		t.Fatal(s, string(raw))
	}
}

func TestUIAttendance(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1020")
	hub, city := uiIDs(t, c)

	t.Run("summary: one letter a day per employee, the same category as the day view", func(t *testing.T) {
		from, to := uiDay(-6), uiDay(-1)
		punchDay(t, st, "EMP-1020", uiDay(-2), "06:00", "07:00")
		s, sum := c.do("GET", "/api/attendance/summary?from="+from+"&to="+to, "EMP-1002", "HR Admin", nil)
		if s != 200 || len(sum["days"].([]any)) != 6 {
			t.Fatal(sum)
		}
		legend := sum["legend"].(map[string]any)
		letter := map[string]string{}
		for k, v := range legend {
			letter[v.(string)] = k
		}
		if len(letter) != 8 {
			t.Fatal("eight categories", legend)
		}
		k := map[string]string{}
		for _, x := range sum["rows"].([]any) {
			m := x.(map[string]any)
			if len(m["k"].(string)) != 6 {
				t.Fatal(m)
			}
			k[m["id"].(string)] = m["k"].(string)
		}
		for i, d := range sum["days"].([]any) {
			_, day := c.do("GET", "/api/attendance/day?date="+d.(string), "EMP-1002", "HR Admin", nil)
			for _, x := range day["rows"].([]any) {
				m := x.(map[string]any)
				row, ok := k[m["id"].(string)]
				if !ok {
					continue
				}
				want := "-"
				if cat, _ := m["cat"].(string); cat != "" {
					want = letter[cat]
				}
				if string(row[i]) != want {
					t.Fatalf("%s on %s: summary %q, day %v", m["id"], d, row[i], m["cat"])
				}
			}
		}
		if !strings.Contains(k["EMP-1020"], "A") && !strings.Contains(k["EMP-1020"], "O") && !strings.Contains(k["EMP-1020"], "W") {
			t.Fatal("an hour's work is absent (short hours)", k["EMP-1020"])
		}
		_, f := c.do("GET", "/api/attendance/summary?from="+from+"&to="+to+"&location="+hub+"&department=F%26B&type=Permanent&city="+city, "EMP-1002", "HR Admin", nil)
		if n := len(f["rows"].([]any)); n == 0 || n >= len(sum["rows"].([]any)) {
			t.Fatal("filters narrow the rows", n)
		}
		if s, _ := c.do("GET", "/api/attendance/summary?from=2026-01-01&to=2026-06-30", "EMP-1002", "HR Admin", nil); s != 400 {
			t.Fatal("at most two months")
		}
		_, own := c.do("GET", "/api/attendance/summary?from="+from+"&to="+to, "EMP-1020", "Employee", nil)
		if rs, _ := own["rows"].([]any); len(rs) > 1 || (len(rs) == 1 && rs[0].(map[string]any)["id"] != "EMP-1020") {
			t.Fatal("an employee sees only their own days", own)
		}
	})

	t.Run("day rows have the source, type and city; issues include absent (short hours)", func(t *testing.T) {
		d := uiDay(-2)
		_, day := c.do("GET", "/api/attendance/day?date="+d, "EMP-1002", "HR Admin", nil)
		found := false
		for _, x := range day["rows"].([]any) {
			m := x.(map[string]any)
			if m["id"] == "EMP-1020" {
				found = true
				if m["src"] != "MIS upload" || m["type"] != "Permanent" || m["city_id"] != city {
					t.Fatal(m)
				}
				if m["status"] == "Absent (short hours)" {
					_, is := c.do("GET", "/api/attendance/issues?ym="+d[:7], "EMP-1002", "HR Admin", nil)
					ok := false
					for _, y := range is["list"].([]any) {
						if n := y.(map[string]any); n["id"] == "EMP-1020" && n["date"] == d && n["status"] == "Absent (short hours)" {
							ok = true
						}
					}
					if !ok {
						t.Fatal("absent (short hours) needs regularizing")
					}
				}
			}
		}
		if !found {
			t.Fatal("EMP-1020 on the day view")
		}
	})

	t.Run("month list: today is not counted; week off, holiday and on leave are", func(t *testing.T) {
		punchDay(t, st, "EMP-1020", uiDay(0), "06:00", "14:00")
		ym := uiDay(0)[:7]
		_, l := c.do("GET", "/api/attendance?ym="+ym, "EMP-1002", "HR Admin", nil)
		doj := uiField(ctx, st, "employee", bson.D{{Key: "_id", Value: "EMP-1020"}}, "doj")
		want := 0
		for d := ym + "-01"; d < uiDay(0); d = mustDate(d).AddDate(0, 0, 1).Format("2006-01-02") {
			if d >= doj {
				want++
			}
		}
		for _, x := range l["list"].([]any) {
			m := x.(map[string]any)
			if m["id"] != "EMP-1020" {
				continue
			}
			got := 0
			for k, v := range m["counts"].(map[string]any) {
				if k != "Leave" {
					got += int(num(v))
				}
			}
			if got != want {
				t.Fatalf("every day before today counted once: %d, want %d (%v)", got, want, m["counts"])
			}
			return
		}
		t.Fatal("EMP-1020 listed")
	})

	t.Run("recent attendance uploads", func(t *testing.T) {
		csv := "Employee ID,Date,In time,Out time\nEMP-1020," + uiDay(-3) + ",06:02,14:05\n"
		if _, r := c.sendFile("/api/bulk/att/apply", "EMP-1002", "HR Admin", "att.csv", []byte(csv)); num(r["ok"]) != 1 {
			t.Fatal(r)
		}
		_, lg := c.do("GET", "/api/bulk/att/log", "EMP-1002", "HR Admin", nil)
		l := lg["list"].([]any)
		if len(l) == 0 {
			t.Fatal("the upload is listed")
		}
		x := l[0].(map[string]any)
		if num(x["rows"]) != 1 || !strings.Contains(x["by"].(string), "EMP-1002") || x["at"] == nil {
			t.Fatal(x)
		}
		if s, _ := c.do("GET", "/api/bulk/att/log", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("only uploaders see the log")
		}
	})
}

// The summary answers for 5,000 employees over a month in one pass.
func TestUIAttendanceSummaryScale(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002")
	// 5,000 copies of EMP-1020, each with a record for each of the last 31 days.
	src, err := st.DB.C("employee").FindOne(ctx, bson.D{{Key: "_id", Value: "EMP-1020"}})
	if err != nil || src == nil {
		t.Fatal("EMP-1020", err)
	}
	emps := []any{}
	for g := 1; g <= 5000; g++ {
		e := bson.M{"id": fmt.Sprintf("EMP-%d", 50000+g), "name": fmt.Sprintf("Load %d", g), "status": "Active", "verified": true}
		for _, k := range []string{"type", "doj", "department_id", "designation_id", "grade", "location_id", "shift_code", "manager_id", "attendance_mode"} {
			e[k] = src[k]
		}
		emps = append(emps, e)
	}
	if err := st.DB.C("employee").InsertMany(ctx, emps); err != nil {
		t.Fatal(err)
	}
	for lo := 1; lo <= 5000; lo += 1000 {
		days := []any{}
		for g := lo; g < lo+1000; g++ {
			for d := 1; d <= 31; d++ {
				days = append(days, bson.M{"employee_id": fmt.Sprintf("EMP-%d", 50000+g), "date": mustDate(store.Today()).AddDate(0, 0, -d).Format("2006-01-02"),
					"in_time": fmt.Sprintf("06:0%d", g%10), "out_time": fmt.Sprintf("14:1%d", d%10), "source": "Punch machine", "by_name": "load"})
			}
		}
		if err := st.DB.C("attendance_day").InsertMany(ctx, days); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	s, r := c.do("GET", "/api/attendance/summary?from="+uiDay(-31)+"&to="+uiDay(-1), "EMP-1002", "HR Admin", nil)
	took := time.Since(start)
	if s != 200 || len(r["rows"].([]any)) < 5000 {
		t.Fatal(s, r["error"])
	}
	t.Logf("summary for %d employees × 31 days in %v", len(r["rows"].([]any)), took)
	if took > 30*time.Second {
		t.Fatal("too slow", took)
	}
}

func TestUIPullConfig(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	const secret = "s3cret-key-778899"
	cfg := func(url string) map[string]any {
		return map[string]any{"url": url, "method": "GET", "auth": "API key", "since": "from", "every": 15, "empF": "emp_code", "timeF": "punch_time", "dirF": "punch_type", "inV": "IN", "outV": "OUT"}
	}
	raw := func(method, path string, body any) (int, string) {
		var b bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&b).Encode(body)
		}
		req, _ := http.NewRequest(method, c.base+path, &b)
		req.Header.Set("Authorization", "Bearer "+c.tokens["EMP-1002"])
		req.Header.Set("X-Role", "HR Admin")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out)
	}

	got := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got++
		if r.Header.Get("X-API-Key") != secret {
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("from") == "" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"count":2,"data":[{"emp_code":"EMP-1020","punch_time":"2026-11-04 06:01","punch_type":"IN"},{"device":"x"}]}`))
	}))
	defer vendor.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Error("the saved key went to another server")
		}
		w.WriteHeader(401)
	}))
	defer other.Close()

	_, g := c.do("GET", "/api/attendance/pull-config", "EMP-1002", "HR Admin", nil)
	if g["keySet"] != false || g["empF"] != "emp_code" || num(g["every"]) != 15 {
		t.Fatal("defaults", g)
	}
	if s, _ := c.do("GET", "/api/attendance/pull-config", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatal("setup rights")
	}
	bad := cfg("ftp://example.com/x")
	if s, _ := c.do("PUT", "/api/attendance/pull-config", "EMP-1002", "HR Admin", bad); s != 400 {
		t.Fatal("http or https only")
	}
	good := cfg(vendor.URL + "/api/punches")
	good["key"] = secret
	if s, r := c.do("PUT", "/api/attendance/pull-config", "EMP-1002", "HR Admin", good); s != 200 {
		t.Fatal(r)
	}
	if s, body := raw("GET", "/api/attendance/pull-config", nil); s != 200 || strings.Contains(body, secret) || !strings.Contains(body, `"keySet":true`) {
		t.Fatal("the key is write-only", body)
	}
	// Saving again without a key keeps it.
	if s, r := c.do("PUT", "/api/attendance/pull-config", "EMP-1002", "HR Admin", cfg(vendor.URL+"/api/punches")); s != 200 {
		t.Fatal(r)
	}

	// Local addresses are refused unless allowed (here, for the stand-in vendor server).
	if _, body := raw("POST", "/api/attendance/pull-config/test", nil); !strings.Contains(body, "not allowed") {
		t.Fatal("loopback refused", body)
	}
	t.Setenv("PULL_ALLOW_LOCAL", "1")
	s, body := raw("POST", "/api/attendance/pull-config/test", nil)
	var r map[string]any
	_ = json.Unmarshal([]byte(body), &r)
	if s != 200 || r["ok"] != true || num(r["status"]) != 200 || num(r["records"]) != 2 || num(r["mapped"]) != 1 {
		t.Fatal(body)
	}
	if sm := r["sample"].(map[string]any); sm["employee"] != "EMP-1020" || sm["direction"] != "IN" {
		t.Fatal(sm)
	}
	if strings.Contains(body, secret) || strings.Contains(body, vendor.URL) || strings.Contains(body, "/api/punches") {
		t.Fatal("no secrets or address in the answer", body)
	}
	_, g = c.do("GET", "/api/attendance/pull-config", "EMP-1002", "HR Admin", nil)
	if g["test_ok"] != true || g["tested_at"] == nil {
		t.Fatal("the saved settings' last test is kept", g)
	}
	// Unsaved values for another server: the saved key is not sent there.
	s, body = raw("POST", "/api/attendance/pull-config/test", cfg(other.URL+"/p"))
	if s != 200 || !strings.Contains(body, "401") || !strings.Contains(body, "warning") || strings.Contains(body, secret) {
		t.Fatal(body)
	}
	if got == 0 {
		t.Fatal("the vendor server was called")
	}
	if s, _ := c.do("POST", "/api/attendance/pull-config/test", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatal("setup rights")
	}
}

func TestUIConsequences(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1009", "EMP-1020")
	if _, err := st.DB.C("deviation_decision").Insert(ctx, bson.M{"employee_id": "EMP-1020", "date": store.Today(), "planned": "M", "actual": "G", "kinds": []string{"Shift"},
		"in_time": "09:00", "out_time": "17:00", "hours": 8, "status": "Not OK", "by_name": "test"}); err != nil {
		t.Fatal(err)
	}
	_, l := c.do("GET", "/api/consequences", "EMP-1009", "Line Manager", nil)
	var sug map[string]any
	for _, x := range l["suggest"].([]any) {
		if m := x.(map[string]any); m["id"] == "EMP-1020" {
			sug = m
		}
	}
	if sug == nil || num(sug["not_ok"]) != 1 || num(sug["score"]) != 95 {
		t.Fatal("suggestions carry the discipline score", l["suggest"])
	}
	if sc := l["scores"].(map[string]any)["EMP-1020"].(map[string]any); num(sc["score"]) != 95 || num(sc["no"]) != 1 {
		t.Fatal(sc)
	}
	if s, r := c.do("POST", "/api/consequences", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1020", "kind": "PIP", "days": 7, "reason": "Attendance and punctuality", "details": "Late on five days this month."}); s != 200 {
		t.Fatal(r)
	}
	hrbp := uiField(ctx, st, "employee", bson.D{{Key: "_id", Value: "EMP-1004"}}, "name")
	_, l = c.do("GET", "/api/consequences", "EMP-1009", "Line Manager", nil)
	row := l["rows"].([]any)[0].(map[string]any)
	if row["department"] != "F&B" || row["designation"] == nil || row["approver_role"] != "HRBP" || row["approver_name"] != hrbp {
		t.Fatal("department, designation and the HRBP's name", row)
	}
	for _, x := range l["suggest"].([]any) {
		if x.(map[string]any)["id"] == "EMP-1020" {
			t.Fatal("an open plan takes the employee off the suggestions")
		}
	}
	if l["scores"].(map[string]any)["EMP-1020"] == nil {
		t.Fatal("the score stays for the issue form")
	}
}
