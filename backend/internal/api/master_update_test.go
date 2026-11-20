package api_test

// Master update (one workbook, a sheet per master) and Edit on every master, with the rename cascade.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

type mbSh struct {
	name string
	rows [][]any // the first row is the header
}

// mbFile builds a workbook from sheets.
func mbFile(t *testing.T, sheets ...mbSh) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	for i, s := range sheets {
		if i == 0 {
			_ = f.SetSheetName("Sheet1", s.name)
		} else if _, err := f.NewSheet(s.name); err != nil {
			t.Fatal(err)
		}
		for j, r := range s.rows {
			r := r
			_ = f.SetSheetRow(s.name, fmt.Sprintf("A%d", j+1), &r)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mbOpen(t *testing.T, b []byte) *excelize.File {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a workbook: %v (%s)", err, string(b[:min(len(b), 200)]))
	}
	return f
}

func mbBytes(t *testing.T, f *excelize.File) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mbRun posts a workbook to check or apply and returns the result.
func (c *client) mbRun(t *testing.T, action, emp, role string, file []byte) map[string]any {
	t.Helper()
	s, r := c.sendFile("/api/master-update/"+action, emp, role, "masters.xlsx", file)
	if s != 200 {
		t.Fatalf("%s: %d %v", action, s, r)
	}
	return r
}

func mbSheetOf(t *testing.T, r map[string]any, sheet string) map[string]any {
	t.Helper()
	for _, x := range r["sheets"].([]any) {
		if m := x.(map[string]any); m["sheet"] == sheet {
			return m
		}
	}
	t.Fatalf("no %s in the result: %v", sheet, r)
	return nil
}

func mbRowsOf(s map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, x := range s["rows"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func mbText(v any) string { return fmt.Sprint(v) }

func mbHeader(cols ...string) []any {
	out := []any{}
	for _, c := range cols {
		out = append(out, c)
	}
	return out
}

// mbDB reads and writes the test database for these tests; any failure stops the test.
type mbDB struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
}

// one is the first document of coll that matches filter (in the order opts give).
func (d mbDB) one(coll string, filter any, opts ...options.Lister[options.FindOneOptions]) store.Row {
	d.t.Helper()
	r, err := d.st.DB.C(coll).FindOne(d.ctx, filter, opts...)
	if err != nil || r == nil {
		d.t.Fatalf("%s %v: %v (found %v)", coll, filter, err, r)
	}
	return r
}

// id is the id of the first document of coll that matches filter.
func (d mbDB) id(coll string, filter any) string {
	d.t.Helper()
	return fmt.Sprint(d.one(coll, filter)["id"])
}

// count is how many documents of coll match filter, as text.
func (d mbDB) count(coll string, filter any) string {
	d.t.Helper()
	n, err := d.st.DB.C(coll).Count(d.ctx, filter)
	if err != nil {
		d.t.Fatalf("count %s %v: %v", coll, filter, err)
	}
	return fmt.Sprint(n)
}

// list is the given field of every document of coll that matches filter, sorted by it, joined with commas ("<nil>"
// when nothing matches).
func (d mbDB) list(coll string, filter any, field string) string {
	d.t.Helper()
	rows, err := d.st.DB.C(coll).Find(d.ctx, filter, options.Find().SetSort(bson.D{{Key: field, Value: 1}}))
	if err != nil {
		d.t.Fatalf("%s %v: %v", coll, filter, err)
	}
	if len(rows) == 0 {
		return "<nil>"
	}
	l := []string{}
	for _, r := range rows {
		l = append(l, fmt.Sprint(r[field]))
	}
	return strings.Join(l, ",")
}

// write stops the test when a write fails.
func (d mbDB) write(_ any, err error) {
	d.t.Helper()
	if err != nil {
		d.t.Fatal(err)
	}
}

// texts joins a list of names with commas.
func (d mbDB) texts(v any) string {
	l := []string{}
	for _, x := range v.([]any) {
		l = append(l, fmt.Sprint(x))
	}
	return strings.Join(l, ",")
}

// gradeRules counts the approval rules (of every rule list) with a condition that lists grade by name.
func (d mbDB) gradeRules(grade string) string {
	d.t.Helper()
	ms, err := d.st.DB.C("approval_flow").Find(d.ctx, nil)
	if err != nil {
		d.t.Fatal(err)
	}
	n := 0
	for _, m := range ms {
		rules, _ := m["rules"].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			when, _ := rule["when"].([]any)
			for _, c := range when {
				cond, _ := c.(map[string]any)
				list, _ := cond["list"].([]any)
				if cond["k"] == "grades" && containsAny(list, grade) {
					n++
				}
			}
		}
	}
	return fmt.Sprint(n)
}

func TestMasterUpdateWorkbook(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	db := mbDB{t, ctx, st}
	c.signIn("EMP-1002")
	c.signIn("EMP-1020")
	all := []string{"Regions", "States", "Cities", "Clusters", "Locations", "Departments", "Grades", "Designations", "Shifts", "Qualifications", "Employment types", "Attendance modes", "Change reasons", "Roles", "Skills"}

	var current []byte
	t.Run("download with current data: a sheet per master, Row ID last, drop-downs", func(t *testing.T) {
		s, b := c.getRaw("/api/master-update/download?data=1", "EMP-1002", "HR Admin")
		if s != 200 {
			t.Fatalf("download: %d %s", s, b)
		}
		current = b
		f := mbOpen(t, b)
		defer f.Close()
		want := append(append([]string{"How to fill"}, all...), "Lists")
		if got := f.GetSheetList(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("sheets %v, want %v", got, want)
		}
		if v, _ := f.GetSheetVisible("Lists"); v {
			t.Fatal("Lists is visible")
		}
		head, _ := f.GetRows("States")
		if strings.Join(head[0], "|") != "Name|Region|Status|Inactive from|Inactive reason|Row ID (leave as is; blank for a new item)" {
			t.Fatalf("States header %v", head[0])
		}
		sh, _ := f.GetRows("Shifts")
		if strings.Join(sh[0][:6], "|") != "Name|Code|Start (HH:MM)|End (HH:MM)|Break minutes|Locations (All locations, or names separated by ;)" {
			t.Fatalf("Shifts header %v", sh[0])
		}
		regions, _ := f.GetRows("Regions")
		if len(regions) != 4 || regions[1][0] != "North" || regions[1][4] != db.id("region", bson.M{"name": "North"}) {
			t.Fatalf("Regions rows %v", regions)
		}
		var spl []string
		for _, r := range sh {
			if len(r) > 1 && r[1] == "SPL" {
				spl = r
			}
		}
		if len(spl) < 9 || spl[6] != "Inactive" || spl[7] != "2026-04-01" || spl[8] != "Discontinued" {
			t.Fatalf("the retired Split shift: %v", spl)
		}
		dvs, _ := f.GetDataValidations("States")
		if len(dvs) < 3 {
			t.Fatalf("States drop-downs: %d", len(dvs))
		}
		for _, d := range dvs {
			if strings.HasPrefix(d.Sqref, "B2") && d.ErrorStyle != nil && *d.ErrorStyle != "warning" {
				t.Fatalf("the Region drop-down stops a new region: %+v", d)
			}
		}
		pane, _ := f.GetPanes("Regions")
		if !pane.Freeze || pane.YSplit != 1 {
			t.Fatalf("header not frozen: %+v", pane)
		}
	})

	t.Run("re-uploaded unchanged: no change anywhere", func(t *testing.T) {
		r := c.mbRun(t, "check", "EMP-1002", "HR Admin", current)
		if num(r["changes"]) != 0 || num(r["errors"]) != 0 {
			t.Fatalf("changes %v errors %v: %v", r["changes"], r["errors"], r["sheets"])
		}
		for _, n := range all {
			s := mbSheetOf(t, r, n)
			if num(s["same"]) == 0 || len(mbRowsOf(s)) != 0 {
				t.Fatalf("%s: %v", n, s)
			}
		}
	})

	t.Run("blank format: headers and drop-downs, no rows", func(t *testing.T) {
		s, b := c.getRaw("/api/master-update/download", "EMP-1002", "HR Admin")
		if s != 200 {
			t.Fatal(s)
		}
		f := mbOpen(t, b)
		defer f.Close()
		for _, n := range all {
			if rows, _ := f.GetRows(n); len(rows) != 1 {
				t.Fatalf("%s has %d rows", n, len(rows))
			}
		}
		if n := db.count("audit_log", bson.M{"action": bson.M{"$regex": "^Downloaded the Master update workbook"}}); n != "2" {
			t.Fatalf("downloads audited: %s", n)
		}
	})

	t.Run("no edit right: refused", func(t *testing.T) {
		if s, _ := c.getRaw("/api/master-update/download", "EMP-1020", "Employee"); s != 403 {
			t.Fatalf("employee download: %d", s)
		}
		if s, _ := c.sendFile("/api/master-update/check", "EMP-1020", "Employee", "m.xlsx", current); s != 403 {
			t.Fatalf("employee check: %d", s)
		}
		if s, r := c.sendFile("/api/master-update/check", "EMP-1002", "HR Admin", "m.csv", []byte("Name\nEast\n")); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "CSV") {
			t.Fatalf("a CSV file: %d %v", s, r)
		}
	})

	t.Run("a region, a state, a city, a cluster and a location in one file", func(t *testing.T) {
		file := mbFile(t,
			mbSh{"Regions", [][]any{mbHeader("Name"), {"East"}}},
			mbSh{"States", [][]any{mbHeader("Name", "Region"), {"Odisha", "East"}}},
			mbSh{"Cities", [][]any{mbHeader("Name", "State"), {"Bhubaneswar", "Odisha"}}},
			mbSh{"Clusters", [][]any{mbHeader("Name", "City"), {"Bhubaneswar Cluster 1", "Bhubaneswar"}}},
			mbSh{"Locations", [][]any{mbHeader("Name", "Cluster"), {"Bhubaneswar – Patia", "Bhubaneswar Cluster 1"}}},
		)
		r := c.mbRun(t, "check", "EMP-1002", "HR Admin", file)
		if num(r["changes"]) != 5 || num(r["errors"]) != 0 {
			t.Fatalf("check: %v", r)
		}
		if n := db.count("region", bson.M{"name": "East"}); n != "0" {
			t.Fatal("check wrote a region")
		}
		if miss := fmt.Sprint(r["missing"]); !strings.Contains(miss, "Grades") || strings.Contains(miss, "Regions") {
			t.Fatalf("missing sheets: %s", miss)
		}
		if rows := mbRowsOf(mbSheetOf(t, r, "Locations")); len(rows) != 1 || rows[0]["kind"] != "new" || mbText(rows[0]["changes"]) != "[map[field:Added from: to:]]" {
			t.Fatalf("location row: %v", rows)
		}
		r = c.mbRun(t, "apply", "EMP-1002", "HR Admin", file)
		if r["applied"] != true || num(r["changes"]) != 5 {
			t.Fatalf("apply: %v", r)
		}
		l := db.one("location", bson.M{"name": "Bhubaneswar – Patia"})
		k := db.one("cluster", bson.M{"_id": l["cluster_id"]})
		ci := db.one("city", bson.M{"_id": k["city_id"]})
		s := db.one("state", bson.M{"_id": ci["state_id"]})
		rg := db.one("region", bson.M{"_id": s["region_id"]})
		if got := fmt.Sprint(rg["name"], " › ", s["name"], " › ", ci["name"], " › ", k["name"]); got != "East › Odisha › Bhubaneswar › Bhubaneswar Cluster 1" {
			t.Fatalf("hierarchy %s", got)
		}
		if n := db.count("audit_log", bson.M{"action": bson.M{"$in": bson.A{"Added region", "Added state", "Added city", "Added cluster", "Added location"}}}); n != "5" {
			t.Fatalf("items audited: %s", n)
		}
		if n := db.count("audit_log", bson.M{"action": "Master update applied: 5 changes"}); n != "1" {
			t.Fatal("summary not audited")
		}
	})

	t.Run("duplicates, unknown Row ID and ambiguous parent names", func(t *testing.T) {
		north := db.id("region", bson.M{"name": "North"})
		r := c.mbRun(t, "check", "EMP-1002", "HR Admin", mbFile(t,
			mbSh{"Regions", [][]any{mbHeader("Name", "Status", "Inactive from", "Inactive reason", "Row ID"), {"Central"}, {"central"}, {"North", "", "", "", north}, {"North again", "", "", "", north},
				{"Nowhere", "", "", "", "00000000-0000-0000-0000-000000000001"}}},
			mbSh{"Cities", [][]any{mbHeader("Name", "State"), {"Aurangabad", "Maharashtra"}, {"Aurangabad", "Odisha"}, {"Aurangabad", "Maharashtra"}}},
			mbSh{"Clusters", [][]any{mbHeader("Name", "City"), {"Aurangabad Cluster 1", "Aurangabad"}, {"Aurangabad Cluster 1", "Aurangabad (Maharashtra)"}, {"Puri Cluster", "Puri"}}},
		))
		reg := mbRowsOf(mbSheetOf(t, r, "Regions"))
		errs := map[float64]string{}
		for _, x := range reg {
			errs[num(x["line"])] = mbText(x["errors"])
		}
		if !strings.Contains(errs[3], "Appears again, first on line 2") || !strings.Contains(errs[5], "Appears again, first on line 4") || !strings.Contains(errs[6], "Row ID") {
			t.Fatalf("region rows: %v", reg)
		}
		cities := mbSheetOf(t, r, "Cities")
		if num(cities["new"]) != 2 || num(cities["errors"]) != 1 {
			t.Fatalf("cities: %v", cities)
		}
		cl := mbRowsOf(mbSheetOf(t, r, "Clusters"))
		if len(cl) != 3 || !strings.Contains(mbText(cl[0]["errors"]), "is in more than one state. Write it as “Aurangabad (Maharashtra)” or “Aurangabad (Odisha)”") ||
			cl[1]["kind"] != "new" || !strings.Contains(mbText(cl[2]["errors"]), "City “Puri” is not in Cities") {
			t.Fatalf("clusters: %v", cl)
		}
	})

	t.Run("a missing sheet changes nothing", func(t *testing.T) {
		regions := func() string { // every region's name and status, by name, then the states and the active locations
			rows, err := st.DB.C("region").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
			if err != nil {
				t.Fatal(err)
			}
			l := []string{}
			for _, r := range rows {
				l = append(l, fmt.Sprint(r["name"], r["status"]))
			}
			return strings.Join(l, ",") + db.count("state", nil) + db.count("location", bson.M{"status": "Active"})
		}
		before := regions()
		r := c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Grades", [][]any{mbHeader("Name"), {"G9"}}}))
		if num(r["changes"]) != 1 || !strings.Contains(fmt.Sprint(r["missing"]), "Regions") {
			t.Fatalf("apply: %v", r)
		}
		after := regions()
		if before != after {
			t.Fatalf("other masters changed: %s → %s", before, after)
		}
	})

	t.Run("inactive with a reason and the impact; scheduled; reactivated", func(t *testing.T) {
		saket := db.id("location", bson.M{"name": "Delhi – Saket"})
		users := db.count("employee", bson.M{"location_id": saket, "status": "Active"})
		head := mbHeader("Name", "Cluster", "Status", "Inactive from", "Inactive reason", "Row ID")
		bad := c.mbRun(t, "check", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Locations", [][]any{head,
			{"Delhi – Saket", "", "Inactive", "", "Because", saket}, {"Gurugram – Golf Course Rd", "", "Inactive", "2020-01-01", "Discontinued"}, {"New place", "Delhi Cluster 1", "Inactive"}}}))
		rows := mbRowsOf(mbSheetOf(t, bad, "Locations"))
		if len(rows) != 3 || !strings.Contains(mbText(rows[0]["errors"]), "is not a Master data reason") || !strings.Contains(mbText(rows[1]["errors"]), "today or a future date") ||
			!strings.Contains(mbText(rows[2]["errors"]), "A new item starts Active") {
			t.Fatalf("bad retirements: %v", rows)
		}
		r := c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Locations", [][]any{head, {"Delhi – Saket", "", "Inactive", "", "discontinued", saket}}}))
		ch := mbText(mbRowsOf(mbSheetOf(t, r, "Locations"))[0]["changes"])
		if !strings.Contains(ch, "Marked inactive from") || !strings.Contains(ch, "Discontinued · "+users+" active employee") {
			t.Fatalf("retired: %s", ch)
		}
		loc := db.one("location", bson.M{"_id": saket})
		if got := fmt.Sprint(loc["status"], " ", loc["inactive_since"], " ", loc["inactive_reason"]); got != "Inactive "+today()+" Discontinued" {
			t.Fatalf("location %s", got)
		}
		r = c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Locations", [][]any{head, {"Delhi – Saket", "", "Active", "", "", saket}}}))
		if ch := mbText(mbRowsOf(mbSheetOf(t, r, "Locations"))[0]["changes"]); !strings.Contains(ch, "Reactivated") {
			t.Fatalf("reactivated: %s", ch)
		}
		loc = db.one("location", bson.M{"_id": saket})
		since, _ := loc["inactive_since"].(string)
		if got := fmt.Sprint(loc["status"]) + since; got != "Active" {
			t.Fatalf("location %s", got)
		}
		later := time.Now().AddDate(0, 1, 0).Format("2006-01-02")
		r = c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Grades", [][]any{mbHeader("Name", "Status", "Inactive from", "Inactive reason"), {"G9", "Active", later, "Created by mistake"}}}))
		if ch := mbText(mbRowsOf(mbSheetOf(t, r, "Grades"))[0]["changes"]); !strings.Contains(ch, "(scheduled)") {
			t.Fatalf("scheduled: %s", ch)
		}
		g9 := db.one("grade", bson.M{"name": "G9"})
		if got := fmt.Sprint(g9["status"], " ", g9["inactive_since"]); got != "Active "+later {
			t.Fatalf("grade %s", got)
		}
		// The current data downloaded again and uploaded unchanged is still no change (scheduled and inactive items too).
		_, b := c.getRaw("/api/master-update/download?data=1", "EMP-1002", "HR Admin")
		if r := c.mbRun(t, "check", "EMP-1002", "HR Admin", b); num(r["changes"]) != 0 || num(r["errors"]) != 0 {
			t.Fatalf("unchanged again: %v", r["sheets"])
		}
	})

	t.Run("rights per sheet: an organisation-only editor", func(t *testing.T) {
		// HRBP: Masters organisation at 2 (change), Masters job at 1 (view), whether the role had them or not.
		for right, level := range map[string]int{"mas.org": 2, "mas.job": 1} {
			db.write(st.DB.C("role_right").Upsert(ctx, bson.M{"role_name": "HRBP", "right_id": right}, bson.M{"level": level}, nil))
		}
		c.signIn("EMP-1004")
		s, b := c.getRaw("/api/master-update/download?data=1", "EMP-1004", "HRBP")
		if s != 200 {
			t.Fatalf("download: %d %s", s, b)
		}
		f := mbOpen(t, b)
		if got := strings.Join(f.GetSheetList(), "|"); got != "How to fill|Regions|States|Cities|Clusters|Locations|Lists" {
			t.Fatalf("org-only sheets: %s", got)
		}
		f.Close()
		r := c.mbRun(t, "apply", "EMP-1004", "HRBP", mbFile(t, mbSh{"Regions", [][]any{mbHeader("Name"), {"North East"}}}, mbSh{"Grades", [][]any{mbHeader("Name"), {"G10"}}}))
		g := mbSheetOf(t, r, "Grades")
		if g["refused"] != true || !strings.Contains(mbText(g["note"]), "Masters: job") || num(r["changes"]) != 1 {
			t.Fatalf("job sheet refused, org sheet applied: %v", r)
		}
		if db.count("grade", bson.M{"name": "G10"}) != "0" || db.count("region", bson.M{"name": "North East"}) != "1" {
			t.Fatal("rights not kept per sheet")
		}
	})
}

func TestMasterRenames(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	db := mbDB{t, ctx, st}
	c.signIn("EMP-1002")
	c.signIn("EMP-1010") // Function Head, mapped to the Sales department by name
	A := func(method, path string, body any) map[string]any {
		t.Helper()
		s, r := c.do(method, path, "EMP-1002", "HR Admin", body)
		if s != 200 {
			t.Fatalf("%s %s: %d %v", method, path, s, r)
		}
		return r
	}
	bySort := bson.D{{Key: "sort", Value: 1}, {Key: "_id", Value: 1}}
	policy := fmt.Sprint(db.one("work_policy", nil, options.FindOne().SetSort(bySort))["id"])
	other := fmt.Sprint(db.one("work_policy", nil, options.FindOne().SetSort(bySort).SetSkip(1))["id"])
	frontDesk := db.id("designation", bson.M{"name": "Front Desk Associate"})
	frontOffice := db.id("department", bson.M{"name": "Front Office"})
	hub := db.id("location", bson.M{"name": "Gurugram – Sector 29 Hub"})
	// An offer still open, holding its department, designation and grade by name in the form data.
	db.write(st.DB.C("offer").Insert(ctx, bson.M{"id": "OFR-9001", "candidate_name": "Test Candidate", "type": "Permanent",
		"doj": time.Now().In(store.Loc()).AddDate(0, 0, 10).Format("2006-01-02"), "department_id": frontOffice, "designation_id": frontDesk, "grade": "G1",
		"location_id": hub, "gross": 20000, "raised_by": "EMP-1002",
		"data": map[string]any{"dept": "Front Office", "desig": "Front Desk Associate", "grade": "G1", "attMode": "Geo-fenced app", "qual": "Graduate"}}))

	t.Run("grade rename by Edit: employees, designations, pay bands, policies, approval rules, offers", func(t *testing.T) {
		g1 := db.id("grade", bson.M{"name": "G1"})
		emps := db.count("employee", bson.M{"grade": "G1"})
		if emps == "0" {
			t.Fatal("the sample has nobody on G1")
		}
		db.write(st.DB.C("work_policy_map").Insert(ctx, bson.M{"policy_id": policy, "level": "grade", "ref": "G1", "created_by": "test"}))
		// The policy's notice settings get probation months for G1 and G2.
		settings, _ := db.one("work_policy", bson.M{"_id": policy})["settings"].(map[string]any)
		notice, _ := settings["notice"].(map[string]any)
		if notice == nil {
			notice = map[string]any{}
		}
		notice["grade"] = map[string]any{"G1": map[string]any{"probM": 3}, "G2": map[string]any{"probM": 4}}
		db.write(st.DB.C("work_policy").Update(ctx, bson.M{"_id": policy}, bson.M{"$set": bson.M{"settings.notice": notice}}))
		rules := db.gradeRules("G1")
		if rules == "0" {
			t.Fatal("no approval rule lists G1")
		}
		db.write(st.DB.C("employee_change").Insert(ctx, bson.M{"employee_id": "EMP-1020", "field": "Grade", "old_value": "G2", "new_value": "G1", "reason": "Promotion",
			"effective_from": time.Now().In(store.Loc()).AddDate(0, 0, 30).Format("2006-01-02"), "state": "Scheduled", "apply_column": "grade", "apply_value": "G1", "changed_by": "test"}))
		r := A("PATCH", "/api/masters/grade/"+g1, map[string]any{"name": "Grade 1"})
		if r["name"] != "Grade 1" || r["referencesUpdated"] == nil {
			t.Fatalf("patch: %v", r)
		}
		for what, got := range map[string]string{
			"employees":         db.count("employee", bson.M{"grade": "Grade 1"}),
			"old on employees":  db.count("employee", bson.M{"grade": "G1"}),
			"designations":      db.count("designation", bson.M{"grades": "G1"}),
			"pay bands":         db.count("pay_band", bson.M{"grade": "G1"}),
			"policy map":        db.count("work_policy_map", bson.M{"level": "grade", "ref": "G1"}),
			"policy grade":      db.count("work_policy", bson.M{"settings.notice.grade.G1": bson.M{"$exists": true}}),
			"approval rules":    db.gradeRules("G1"),
			"offer":             db.count("offer", bson.M{"$or": bson.A{bson.M{"grade": "G1"}, bson.M{"data.grade": "G1"}}}),
			"scheduled changes": db.count("employee_change", bson.M{"apply_column": "grade", "apply_value": "G1", "state": "Scheduled"}),
		} {
			want := "0"
			if what == "employees" {
				want = emps
			}
			if got != want {
				t.Errorf("%s: %s, want %s", what, got, want)
			}
		}
		if got := db.count("pay_band", bson.M{"grade": "Grade 1"}); got != "5" {
			t.Errorf("pay bands on the new name: %s", got)
		}
		settings, _ = db.one("work_policy", bson.M{"_id": policy})["settings"].(map[string]any)
		notice, _ = settings["notice"].(map[string]any)
		byGrade, _ := notice["grade"].(map[string]any)
		g1Row, _ := byGrade["Grade 1"].(map[string]any)
		if got := fmt.Sprint(g1Row["probM"]); got != "3" {
			t.Errorf("probation by grade: %s", got)
		}
		if got := db.gradeRules("Grade 1"); got != rules {
			t.Errorf("approval rules on the new name: %s, want %s", got, rules)
		}
		if got := db.count("audit_log", bson.M{"action": "Changed grade", "after.references_updated": bson.M{"$exists": true}}); got != "1" {
			t.Errorf("rename audited with its references: %s", got)
		}
		// The job change screen still takes the grade by its new name.
		if s, r := c.do("PATCH", "/api/employees/EMP-1020/job", "EMP-1002", "HR Admin", map[string]any{"changes": map[string]any{"grade": "Grade 1"}, "reason": "Promotion", "effectiveFrom": today()}); s != 200 {
			t.Errorf("job change to the renamed grade: %d %v", s, r)
		}
	})

	t.Run("department rename by the workbook: policy map, role mapping scope, documents, company policies, offers", func(t *testing.T) {
		sales := db.id("department", bson.M{"name": "Sales"})
		firstDoc := db.one("onb_document", nil, options.FindOne().SetSort(bson.D{{Key: "sort", Value: 1}}))
		db.write(st.DB.C("onb_document").Update(ctx, bson.M{"key": firstDoc["key"]}, bson.M{"$set": bson.M{"depts": []string{"Sales"}}}))
		db.write(st.DB.C("company_policy").Insert(ctx, bson.M{"id": "CP-900", "title": "Sales code", "audience": map[string]any{"departments": []any{"Sales"}}, "created_by": "test"}))
		db.write(st.DB.C("offer").Update(ctx, bson.M{"_id": "OFR-9001"}, bson.M{"$set": bson.M{"data.dept": "Sales"}}))
		before := db.count("work_policy_map", bson.M{"level": "department", "ref": "Sales"})
		r := c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Departments", [][]any{mbHeader("Name", "Approval flow", "Status", "Inactive from", "Inactive reason", "Row ID"),
			{"Sales and Marketing", "Corporate", "Active", "", "", sales}}}))
		row := mbRowsOf(mbSheetOf(t, r, "Departments"))[0]
		if ch := mbText(row["changes"]); !strings.Contains(ch, "from:Sales") || !strings.Contains(ch, "to:Sales and Marketing") || !strings.Contains(ch, "also updated") || !strings.Contains(ch, "role mappings") {
			t.Fatalf("change: %s", ch)
		}
		for what, got := range map[string]string{
			"policy map":       db.count("work_policy_map", bson.M{"level": "department", "ref": "Sales and Marketing"}),
			"role mapping":     db.count("user_role_map", bson.M{"departments": "Sales and Marketing"}),
			"documents":        db.count("onb_document", bson.M{"depts": "Sales and Marketing"}),
			"company policies": db.count("company_policy", bson.M{"audience.departments": "Sales and Marketing"}),
			"offer":            db.count("offer", bson.M{"data.dept": "Sales and Marketing"}),
		} {
			want := "1"
			if what == "policy map" {
				want = before
			}
			if got != want || got == "0" {
				t.Errorf("%s: %s, want %s", what, got, want)
			}
		}
		if got := db.count("user_role_map", bson.M{"departments": "Sales"}); got != "0" {
			t.Errorf("old name left in role mappings: %s", got)
		}
		// The Function Head's department scope still reaches the department's people.
		s, l := c.do("GET", "/api/employees", "EMP-1010", "Function Head", nil)
		list, _ := l["list"].([]any)
		if l["rows"] != nil {
			list, _ = l["rows"].([]any)
		}
		if s != 200 || len(list) == 0 {
			t.Fatalf("Function Head sees nobody after the rename: %d %v", s, l)
		}
	})

	t.Run("designation rename: policy map, and a name shared with another department", func(t *testing.T) {
		se := db.id("designation", bson.M{"name": "Sales Executive"})
		db.write(st.DB.C("work_policy_map").Insert(ctx, bson.M{"policy_id": policy, "level": "designation", "ref": "Sales Executive", "created_by": "test"}))
		A("PATCH", "/api/masters/designation/"+se, map[string]any{"name": "Sales Officer"})
		if got := db.count("work_policy_map", bson.M{"level": "designation", "ref": "Sales Officer", "policy_id": policy}); got != "1" {
			t.Fatalf("policy map renamed: %s", got)
		}
		hk := db.id("department", bson.M{"name": "Housekeeping"})
		fb := db.id("department", bson.M{"name": "F&B"})
		A("POST", "/api/masters/designation", map[string]any{"name": "Supervisor", "department_id": hk, "grades": []string{"G2"}})
		sup := A("POST", "/api/masters/designation", map[string]any{"name": "Supervisor", "department_id": fb, "grades": []string{"G2"}})
		db.write(st.DB.C("work_policy_map").Insert(ctx, bson.M{"policy_id": other, "level": "designation", "ref": "Supervisor", "created_by": "test"}))
		// The workbook renames the F&B one (by its Row ID); the Housekeeping one keeps the old name.
		r := c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Designations", [][]any{mbHeader("Name", "Department", "Grades", "Row ID"),
			{"F&B Lead", "F&B", "G2, G3", sup["id"]}}}))
		if num(r["changes"]) != 1 {
			t.Fatalf("apply: %v", r)
		}
		if got := db.list("work_policy_map", bson.M{"level": "designation", "on_map": true, "policy_id": other}, "ref"); got != "F&B Lead,Supervisor" {
			t.Fatalf("both names keep the policy: %s", got)
		}
		if got := db.texts(db.one("designation", bson.M{"_id": sup["id"]})["grades"]); got != "G2,G3" {
			t.Fatalf("grades: %s", got)
		}
		A("POST", "/api/masters/designation", map[string]any{"name": "Supervisor", "department_id": db.id("department", bson.M{"name": "Front Office"}), "grades": []string{"G2"}})
		dup := c.mbRun(t, "check", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Designations", [][]any{mbHeader("Name", "Department", "Grades"), {"Supervisor", "", "G2"}, {"Lead", "F&B", "G99"}}}))
		rows := mbRowsOf(mbSheetOf(t, dup, "Designations"))
		if len(rows) != 2 || !strings.Contains(mbText(rows[0]["errors"]), "in more than one department") && !strings.Contains(mbText(rows[0]["errors"]), "Give the Department") ||
			!strings.Contains(mbText(rows[1]["errors"]), "Grade G99 is not in Grades") {
			t.Fatalf("designation errors: %v", rows)
		}
	})

	t.Run("a region and a grade renamed in the file: the rows that name them still point at them", func(t *testing.T) {
		_, b := c.getRaw("/api/master-update/download?data=1", "EMP-1002", "HR Admin")
		f := mbOpen(t, b)
		for sheet, ren := range map[string][2]string{"Regions": {"North", "North India"}, "Grades": {"G6", "Grade 6"}} {
			rows, _ := f.GetRows(sheet)
			for i, r := range rows {
				if r[0] == ren[0] {
					_ = f.SetCellValue(sheet, fmt.Sprintf("A%d", i+1), ren[1])
				}
			}
		}
		r := c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbBytes(t, f))
		f.Close()
		if num(r["changes"]) != 2 || num(r["errors"]) != 0 || num(mbSheetOf(t, r, "States")["changed"]) != 0 || num(mbSheetOf(t, r, "Designations")["changed"]) != 0 {
			t.Fatalf("rename: %v", r["sheets"])
		}
		if got := fmt.Sprint(db.one("region", bson.M{"_id": db.one("state", bson.M{"name": "Delhi"})["region_id"]})["name"]); got != "North India" {
			t.Fatalf("Delhi is in %s", got)
		}
		if got := db.texts(db.one("designation", bson.M{"name": "Sales Head"})["grades"]); got != "Grade 6" {
			t.Fatalf("Sales Head grades %s", got)
		}
	})

	t.Run("attendance mode and qualification renames", func(t *testing.T) {
		geo := db.id("simple_master", bson.M{"kind": "attendance_mode", "name": "Geo-fenced app"})
		n := db.count("employee", bson.M{"attendance_mode": "Geo-fenced app"})
		A("PATCH", "/api/masters/attendance_mode/"+geo, map[string]any{"name": "Mobile app (geo-fenced)"})
		if got := db.count("employee", bson.M{"attendance_mode": "Mobile app (geo-fenced)"}); got != n || n == "0" {
			t.Fatalf("employees on the renamed mode: %s of %s", got, n)
		}
		grad := db.id("simple_master", bson.M{"kind": "qualification", "name": "Graduate"})
		A("PATCH", "/api/masters/qualification/"+grad, map[string]any{"name": "Graduate degree"})
		data, _ := db.one("offer", bson.M{"_id": "OFR-9001"})["data"].(map[string]any)
		if got := fmt.Sprint(data["attMode"], " / ", data["qual"]); got != "Mobile app (geo-fenced) / Graduate degree" {
			t.Fatalf("offer form: %s", got)
		}
	})
}

func TestMasterFixedNamesAndMoves(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	db := mbDB{t, ctx, st}
	c.signIn("EMP-1002")
	refused := func(path string, body any, want string) {
		t.Helper()
		s, r := c.do("PATCH", path, "EMP-1002", "HR Admin", body)
		if s != 400 || !strings.Contains(fmt.Sprint(r["error"]), want) {
			t.Fatalf("PATCH %s %v: %d %v, want %q", path, body, s, r, want)
		}
	}
	perm := db.id("simple_master", bson.M{"kind": "employment_type", "name": "Permanent"})
	g := db.id("shift", bson.M{"code": "G"})
	transfer := db.id("simple_master", bson.M{"kind": "change_reason", "name": "Transfer", "used_for": "Employee update"})

	t.Run("an employment type's name, a shift's code and the use of the app's own reasons cannot change", func(t *testing.T) {
		refused("/api/masters/employment_type/"+perm, map[string]any{"name": "Permanent staff"}, "employment type's name cannot change")
		refused("/api/masters/shift/"+g, map[string]any{"code": "GX"}, "code of a shift cannot change")
		refused("/api/masters/change_reason/"+transfer, map[string]any{"used_for": "Salary"}, "what it is used for cannot change")
		_, l := c.do("GET", "/api/masters/employment_type", "EMP-1002", "HR Admin", nil)
		for _, x := range l["list"].([]any) {
			if x.(map[string]any)["fixed"] == nil {
				t.Fatalf("employment type not marked fixed: %v", x)
			}
		}
		r := c.mbRun(t, "check", "EMP-1002", "HR Admin", mbFile(t,
			mbSh{"Shifts", [][]any{mbHeader("Name", "Code", "Start", "End", "Break minutes", "Locations", "Row ID"), {"General", "GX", "09:00", "18:00", 30, "All locations", g}}},
			mbSh{"Employment types", [][]any{mbHeader("Name", "Row ID"), {"Permanent staff", perm}}}))
		if e := mbText(mbRowsOf(mbSheetOf(t, r, "Shifts"))[0]["errors"]); !strings.Contains(e, "code of a shift cannot change") {
			t.Fatalf("shift code in the file: %s", e)
		}
		if e := mbText(mbRowsOf(mbSheetOf(t, r, "Employment types"))[0]["errors"]); !strings.Contains(e, "cannot change") {
			t.Fatalf("employment type in the file: %s", e)
		}
		if got := db.count("simple_master", bson.M{"kind": "employment_type", "name": "Permanent"}); got != "1" {
			t.Fatal("employment type renamed")
		}
	})

	t.Run("a designation with active holders cannot move to another department", func(t *testing.T) {
		fd := db.id("designation", bson.M{"name": "Front Desk Associate"})
		hk := db.id("department", bson.M{"name": "Housekeeping"})
		n := db.count("employee", bson.M{"designation_id": fd, "status": "Active"})
		refused("/api/masters/designation/"+fd, map[string]any{"department_id": hk}, n+" active employee")
		refused("/api/masters/designation/"+fd, map[string]any{"department_id": hk}, "hold this designation in Front Office; move them first with Bulk update")
		_, tr := c.do("POST", "/api/masters/designation", "EMP-1002", "HR Admin", map[string]any{"name": "Trainee", "department_id": db.id("department", bson.M{"name": "Front Office"}), "grades": []string{"G1"}})
		if s, r := c.do("PATCH", "/api/masters/designation/"+fmt.Sprint(tr["id"]), "EMP-1002", "HR Admin", map[string]any{"department_id": hk}); s != 200 {
			t.Fatalf("an unused designation moves: %d %v", s, r)
		}
	})

	t.Run("a location moves to another cluster with its employees; a shift timing from today", func(t *testing.T) {
		saket := db.id("location", bson.M{"name": "Delhi – Saket"})
		n := db.count("employee", bson.M{"location_id": saket, "status": "Active"})
		r := c.mbRun(t, "check", "EMP-1002", "HR Admin", mbFile(t, mbSh{"Locations", [][]any{mbHeader("Name", "Cluster"), {"Delhi – Saket", "Gurugram Cluster 1"}}}))
		ch := mbText(mbRowsOf(mbSheetOf(t, r, "Locations"))[0]["changes"])
		if !strings.Contains(ch, "field:Cluster from:Delhi Cluster 1") || !strings.Contains(ch, "its "+n+" active employee") {
			t.Fatalf("move: %s", ch)
		}
		s, p := c.do("PATCH", "/api/masters/location/"+saket, "EMP-1002", "HR Admin", map[string]any{"cluster_id": db.id("cluster", bson.M{"name": "Gurugram Cluster 1"})})
		if s != 200 || mbText(p["activeEmployeesMoved"]) != n {
			t.Fatalf("patch move: %d %v", s, p)
		}
		inactive := db.id("region", bson.M{"name": "West"})
		db.write(st.DB.C("region").Update(ctx, bson.M{"_id": inactive}, bson.M{"$set": bson.M{"status": "Inactive"}}))
		eShift := db.id("shift", bson.M{"code": "E"})
		before := db.count("shift_version", bson.M{"shift_id": eShift})
		r = c.mbRun(t, "apply", "EMP-1002", "HR Admin", mbFile(t,
			mbSh{"States", [][]any{mbHeader("Name", "Region"), {"Goa", "West"}}},
			mbSh{"Shifts", [][]any{mbHeader("Name", "Code", "Start (HH:MM)", "End (HH:MM)"), {"Evening", "e", "15:00", "23:00"}, {"Late", "LT", "14:00", "14:00"}}})) // L is a roster code (TIME-012)
		if e := mbText(mbRowsOf(mbSheetOf(t, r, "States"))[0]["errors"]); !strings.Contains(e, "Region “West” is inactive") {
			t.Fatalf("inactive parent: %s", e)
		}
		sh := mbRowsOf(mbSheetOf(t, r, "Shifts"))
		if len(sh) != 2 || !strings.Contains(mbText(sh[0]["changes"]), "from today, as a new shift version") || !strings.Contains(mbText(sh[1]["errors"]), "cannot be the same") {
			t.Fatalf("shifts: %v", sh)
		}
		if got := db.count("shift_version", bson.M{"shift_id": db.id("shift", bson.M{"code": "E"})}); got == before {
			t.Fatal("no version from today")
		}
		if got := fmt.Sprint(db.one("shift", bson.M{"code": "E"})["start_time"]); got != "15:00" {
			t.Fatalf("shift start %s", got)
		}
	})
}
