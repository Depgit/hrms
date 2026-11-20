package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Part B of the My view changes: the employee's own dashboard, the By date period, roster edits outside My
// view, and the department directory.

func mbList(v any) []any { l, _ := v.([]any); return l }
func mbMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// mbName is an employee's name ("" when there is no such employee).
func mbName(ctx context.Context, st *store.Store, id string) string {
	e, _ := st.DB.C("employee").FindOne(ctx, bson.M{"_id": id})
	n, _ := e["name"].(string)
	return n
}

func TestMyViewDashboard(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1020", "EMP-1018", "EMP-1009", "EMP-1002")
	ctx := context.Background()
	now := time.Now()
	day := func(n int) time.Time { return time.Date(now.Year(), now.Month(), now.Day()+n, 0, 0, 0, 0, time.UTC) }
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	set := func(coll string, filter, fields bson.M) {
		t.Helper()
		must(st.DB.C(coll).UpdateMany(ctx, filter, bson.M{"$set": fields}))
	}
	// EMP-1020 is F&B at Gurugram Sector 29 Hub. EMP-1018: F&B in Hyderabad (same department). EMP-1014: Front
	// Office at Sector 29 Hub (same location). EMP-1023: Housekeeping in Bengaluru (neither). EMP-1024: F&B, left.
	in3 := day(3)
	set("employee_personal", bson.M{"employee_id": "EMP-1018"}, bson.M{"dob": time.Date(1985, in3.Month(), in3.Day(), 0, 0, 0, 0, time.UTC)})
	in5 := day(5)
	set("employee", bson.M{"_id": "EMP-1014"}, bson.M{"doj": time.Date(in5.Year()-3, in5.Month(), in5.Day(), 0, 0, 0, 0, time.UTC)})
	set("employee_personal", bson.M{"employee_id": "EMP-1023"}, bson.M{"dob": time.Date(1990, in3.Month(), in3.Day(), 0, 0, 0, 0, time.UTC)})
	set("employee_personal", bson.M{"employee_id": "EMP-1024"}, bson.M{"dob": time.Date(1991, in3.Month(), in3.Day(), 0, 0, 0, 0, time.UTC)})
	set("employee", bson.M{"_id": "EMP-1024"}, bson.M{"status": "Inactive", "lwd": store.DateOf(time.Now().AddDate(0, 0, -10))})
	set("employee_personal", bson.M{"employee_id": "EMP-1032"}, bson.M{"dob": day(45)}) // outside the 30 days

	t.Run("overview: birthdays and anniversaries of department and location colleagues, never the birth year", func(t *testing.T) {
		s, d := c.do("GET", "/api/me/dashboard", "EMP-1020", "Employee", nil)
		if s != 200 {
			t.Fatalf("%d %v", s, d)
		}
		cel := mbList(d["celebrations"])
		raw, _ := json.Marshal(cel)
		if strings.Contains(string(raw), "1985") || strings.Contains(string(raw), "dob") {
			t.Fatalf("birth year or date of birth sent: %s", raw)
		}
		find := func(name, kind string) map[string]any {
			for _, x := range cel {
				if m := mbMap(x); m["name"] == name && m["kind"] == kind {
					return m
				}
			}
			return nil
		}
		n1018, n1014, n1023, n1024, n1032 := mbName(ctx, st, "EMP-1018"), mbName(ctx, st, "EMP-1014"), mbName(ctx, st, "EMP-1023"),
			mbName(ctx, st, "EMP-1024"), mbName(ctx, st, "EMP-1032")
		b := find(n1018, "Birthday")
		if b == nil || b["date"] != in3.Format("2006-01-02") || b["designation"] == "" || b["location"] == "" || b["years"] != nil {
			t.Fatalf("department colleague's birthday %v in %s", b, raw)
		}
		a := find(n1014, "Work anniversary")
		if a == nil || a["years"] != 3.0 || a["date"] != in5.Format("2006-01-02") {
			t.Fatalf("location colleague's anniversary %v in %s", a, raw)
		}
		if find(n1023, "Birthday") != nil || find(n1024, "Birthday") != nil || find(n1032, "Birthday") != nil {
			t.Fatalf("someone outside the department and location, someone who left, or a birthday after 30 days: %s", raw)
		}
		for _, x := range cel {
			if dt := mbMap(x)["date"].(string); dt > day(30).Format("2006-01-02") {
				t.Fatalf("outside the window: %v", x)
			}
		}
		for _, k := range []string{"pending", "tenure", "alerts", "recent", "payroll"} {
			if _, ok := d[k]; !ok {
				t.Fatalf("overview misses %s: %v", k, d)
			}
		}
		for _, k := range []string{"byCity", "byDept", "byType", "active", "attrition"} {
			if _, ok := d[k]; ok {
				t.Fatalf("headcount figure %s in My view: %v", k, d)
			}
		}
	})

	t.Run("payroll: only the caller's own pay", func(t *testing.T) {
		ym := now.Format("2006-01")
		must(st.DB.C("payroll_month").Upsert(ctx, bson.M{"ym": ym}, bson.M{"status": "Published"}, nil))
		for id, v := range map[string][2]float64{"EMP-1020": {21000, 18500}, "EMP-1018": {55000, 47000}} {
			data := fmt.Sprintf(`{"employeeId":%q,"totalEarn":%v,"net":%v}`, id, v[0], v[1])
			must(st.DB.C("payroll_row").Insert(ctx, bson.M{"ym": ym, "employee_id": id, "net": v[1], "data": data}))
		}
		must(nil, st.DB.C("tds_history").InsertMany(ctx, []any{
			bson.M{"employee_id": "EMP-1020", "ym": ym, "section": "192", "amount": 750},
			bson.M{"employee_id": "EMP-1018", "ym": ym, "section": "192", "amount": 4200},
		}))
		_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Claim bill")
		if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 640, "claimType": "Travel", "billDate": today(), "doc": doc["id"]}}); s != 200 {
			t.Fatalf("claim: %v", r)
		}
		_, doc2 := c.upload("EMP-1018", "Employee", "employee", "EMP-1018", "Claim bill")
		if s, r := c.do("POST", "/api/approvals", "EMP-1018", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 990, "claimType": "Travel", "billDate": today(), "doc": doc2["id"]}}); s != 200 {
			t.Fatalf("other claim: %v", r)
		}
		s, d := c.do("GET", "/api/me/dashboard?tab=pay", "EMP-1020", "Employee", nil)
		if s != 200 {
			t.Fatalf("%d %v", s, d)
		}
		last, fy := mbMap(d["last"]), mbMap(d["fy"])
		if last["net"] != 18500.0 || last["ym"] != ym {
			t.Fatalf("last pay %v", last)
		}
		if fy["earned"] != 21000.0 || fy["tds"] != 750.0 || fy["months"] != 1.0 {
			t.Fatalf("financial year %v", fy)
		}
		nx := mbMap(d["next"])
		if nx["date"] == "" || nx["ym"] == ym { // the next month is the open one now
			t.Fatalf("next pay %v", nx)
		}
		items := mbList(d["items"])
		if len(items) != 1 || mbMap(items[0])["amount"] != 640.0 || mbMap(items[0])["status"] != "Pending" {
			t.Fatalf("own claims only: %v", items)
		}
		raw, _ := json.Marshal(d)
		if strings.Contains(string(raw), "47000") || strings.Contains(string(raw), "990") || strings.Contains(string(raw), "4200") {
			t.Fatalf("another person's pay: %s", raw)
		}
		// The same answer in any role view: it is always the caller's own.
		_, hr := c.do("GET", "/api/me/dashboard?tab=pay", "EMP-1002", "HR Admin", nil)
		if mbMap(hr["last"]) != nil && mbMap(hr["last"])["net"] == 18500.0 {
			t.Fatalf("HR Admin's own pay shows someone else's: %v", hr["last"])
		}
	})

	t.Run("attendance and details: own month and own missing details", func(t *testing.T) {
		s, a := c.do("GET", "/api/me/dashboard?tab=att", "EMP-1020", "Employee", nil)
		if s != 200 || a["ym"] != now.Format("2006-01") {
			t.Fatalf("%d %v", s, a)
		}
		for _, k := range []string{"present", "late", "missing", "noPunch", "leave", "toFix"} {
			if _, ok := a[k]; !ok {
				t.Fatalf("attendance misses %s: %v", k, a)
			}
		}
		set("employee_gov_id", bson.M{"employee_id": "EMP-1020"}, bson.M{"pan": nil})
		_, m := c.do("GET", "/api/me/dashboard?tab=cmp", "EMP-1020", "Employee", nil)
		if !containsAny(mbList(m["missing"]), "PAN") {
			t.Fatalf("missing PAN not listed: %v", m)
		}
		raw, _ := json.Marshal(m)
		var aad string
		if g, _ := st.DB.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": "EMP-1020"}); g != nil {
			aad, _ = g["aadhaar"].(string)
		}
		if aad != "" && strings.Contains(string(raw), aad) {
			t.Fatalf("details sent with values: %s", raw)
		}
		_, ov := c.do("GET", "/api/me/dashboard", "EMP-1020", "Employee", nil)
		if !strings.Contains(fmt.Sprint(ov["alerts"]), "PAN") {
			t.Fatalf("overview alert for missing PAN: %v", ov["alerts"])
		}
	})

	t.Run("the team dashboard is unchanged for other views", func(t *testing.T) {
		s, d := c.do("GET", "/api/dashboard", "EMP-1002", "HR Admin", nil)
		if s != 200 || d["byCity"] == nil || d["active"] == nil {
			t.Fatalf("%d %v", s, d)
		}
	})
}

func TestMyViewAttendancePeriod(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1020", "EMP-1009", "EMP-1014")
	from := time.Now().AddDate(0, 0, -9).Format("2006-01-02")
	to := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	s, d := c.do("GET", "/api/attendance/day?from="+from+"&to="+to, "EMP-1020", "Employee", nil)
	if s != 200 {
		t.Fatalf("%d %v", s, d)
	}
	rows := mbList(d["rows"])
	if len(rows) != 9 {
		t.Fatalf("want one row per date (9), got %d: %v", len(rows), rows)
	}
	for i, x := range rows {
		r := mbMap(x)
		want := time.Now().AddDate(0, 0, -9+i).Format("2006-01-02")
		if r["date"] != want || r["id"] != "EMP-1020" || r["status"] == nil || r["location"] == nil || r["planned"] == nil {
			t.Fatalf("row %d: %v", i, r)
		}
	}
	// Another person: refused in My view; allowed to their manager in the Line Manager view.
	if s, _ := c.do("GET", "/api/attendance/day?from="+from+"&to="+to+"&employeeId=EMP-1014", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("an employee read someone else's days: %d", s)
	}
	if s, r := c.do("GET", "/api/attendance/day?from="+from+"&to="+to+"&employeeId=EMP-1020", "EMP-1009", "Line Manager", nil); s != 200 || len(mbList(r["rows"])) != 9 {
		t.Fatalf("manager: %d %v", s, r)
	}
	if s, _ := c.do("GET", "/api/attendance/day?from="+from+"&to="+to+"&employeeId=EMP-1020", "EMP-1014", "Employee", nil); s != 403 {
		t.Fatalf("a peer read the employee's days: %d", s)
	}
	if s, _ := c.do("GET", "/api/attendance/day?from=2026-01-01&to=2026-06-30", "EMP-1020", "Employee", nil); s != 400 {
		t.Fatalf("a period over three months: %d", s)
	}
	// The single date list is as before.
	if s, r := c.do("GET", "/api/attendance/day?date="+to, "EMP-1009", "Line Manager", nil); s != 200 || r["date"] != to || len(mbList(r["rows"])) == 0 {
		t.Fatalf("single date: %d %v", s, r)
	}
}

func TestMyViewRosterEdits(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1020", "EMP-1009")
	d := time.Now().AddDate(0, 0, 10).Format("2006-01-02")
	if s, _ := c.do("POST", "/api/roster/cell", "EMP-1020", "Employee", map[string]any{"employeeId": "EMP-1020", "date": d, "code": "WO", "why": "Employee request"}); s != 403 {
		t.Fatalf("My view changed the roster: %d", s)
	}
	if s, _ := c.do("POST", "/api/roster/pattern", "EMP-1020", "Employee", map[string]any{"pattern": "fixed"}); s != 403 {
		t.Fatalf("My view applied a pattern: %d", s)
	}
	if s, _ := c.sendFile("/api/bulk/roster/check", "EMP-1020", "Employee", "roster.csv", []byte("Employee ID,Date,Code,Reason\nEMP-1020,"+d+",WO,\n")); s != 403 {
		t.Fatalf("My view uploaded a roster: %d", s)
	}
	// The manager's own My view cannot either; their Line Manager view can.
	if s, _ := c.do("POST", "/api/roster/cell", "EMP-1009", "Employee", map[string]any{"employeeId": "EMP-1020", "date": d, "code": "WO", "why": "Employee request"}); s != 403 {
		t.Fatalf("manager's My view changed the roster: %d", s)
	}
	if s, r := c.do("POST", "/api/roster/cell", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1020", "date": d, "code": "WO", "why": "Employee request"}); s != 200 {
		t.Fatalf("manager view: %d %v", s, r)
	}
}

func TestMyViewDirectory(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1020", "EMP-1009")
	ctx := context.Background()
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1024"}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": store.DateOf(time.Now().AddDate(0, 0, -5))}}); err != nil {
		t.Fatal(err)
	}
	s, d := c.do("GET", "/api/me/directory", "EMP-1020", "Employee", nil)
	if s != 200 || d["department"] != "F&B" {
		t.Fatalf("%d %v", s, d)
	}
	nodes := mbList(d["nodes"])
	// The sample's emails are made from the employee code (EMP-1020@mail.in); the email itself is shown on purpose
	// (mydirectory_test.go checks the rest with work-style emails).
	noMail := []map[string]any{}
	for _, x := range nodes {
		m := map[string]any{}
		for k, v := range mbMap(x) {
			if k != "email" {
				m[k] = v
			}
		}
		noMail = append(noMail, m)
	}
	raw, _ := json.Marshal(noMail)
	if strings.Contains(string(raw), "EMP-") {
		t.Fatalf("directory sends employee codes: %s", raw)
	}
	allowed := map[string]bool{"key": true, "ref": true, "name": true, "designation": true, "location": true, "email": true, "manager": true, "outside": true, "me": true}
	for _, x := range nodes {
		for k := range mbMap(x) {
			if !allowed[k] {
				t.Fatalf("directory sends %q: %v", k, x)
			}
		}
	}
	names := map[string]map[string]any{}
	for _, x := range nodes {
		m := mbMap(x)
		names[m["name"].(string)] = m
	}
	fb, err := st.DB.C("department").Distinct(ctx, "_id", bson.M{"name": "F&B"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := st.DB.C("employee").Find(ctx, bson.M{"department_id": bson.M{"$in": fb}})
	if err != nil {
		t.Fatal(err)
	}
	inDept := 0
	for _, e := range want {
		n, id, status := e["name"].(string), e["id"].(string), e["status"].(string)
		if status == "Active" {
			inDept++
			if names[n] == nil || names[n]["outside"] == true {
				t.Fatalf("%s (%s) missing or marked outside: %s", n, id, raw)
			}
		}
	}
	left, mgr, top := mbName(ctx, st, "EMP-1024"), mbName(ctx, st, "EMP-1009"), mbName(ctx, st, "EMP-1008")
	if names[left] != nil {
		t.Fatalf("someone who left is listed: %s", raw)
	}
	// Managers from other departments (Front Office, Operations) who manage F&B people are the tops, greyed.
	for _, n := range []string{mgr, top} {
		m := names[n]
		if m == nil || m["outside"] != true || m["manager"] != 0.0 {
			t.Fatalf("outside manager %s: %v", n, m)
		}
	}
	var self map[string]any
	for _, x := range nodes {
		if mbMap(x)["me"] == true {
			self = mbMap(x)
		}
	}
	if self == nil || self["manager"] != names[mgr]["key"] {
		t.Fatalf("caller not under their manager: %v", self)
	}
	if len(nodes) != inDept+2 {
		t.Fatalf("want %d in the department and 2 outside managers, got %d", inDept, len(nodes))
	}
	// Another person's view of the same endpoint is their own department (Front Office for EMP-1009), in any role.
	_, o := c.do("GET", "/api/me/directory", "EMP-1009", "Line Manager", nil)
	if o["department"] != "Front Office" {
		t.Fatalf("manager's directory: %v", o["department"])
	}
}
