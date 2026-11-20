package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// dirFields are the only fields a directory answer may carry for a person.
var (
	dirPeopleFields = map[string]bool{"ref": true, "name": true, "designation": true, "department": true, "dept": true, "location": true, "email": true}
	dirChartFields  = map[string]bool{"key": true, "ref": true, "name": true, "designation": true, "location": true, "email": true, "manager": true, "outside": true, "me": true}
)

// dirNoLeak fails when a person in rows has a field outside allowed, or the answer holds an employee code, a
// mobile number or a birth date of anyone.
func dirNoLeak(t *testing.T, st *store.Store, rows []any, allowed map[string]bool) {
	t.Helper()
	for _, x := range rows {
		for k := range mbMap(x) {
			if !allowed[k] {
				t.Fatalf("directory sends %q: %v", k, x)
			}
		}
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "EMP-") {
		t.Fatalf("directory sends employee codes: %s", raw)
	}
	pers, err := st.DB.C("employee_personal").Find(context.Background(), bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pers {
		for _, k := range []string{"mobile", "dob"} {
			if v, _ := p[k].(string); v != "" && strings.Contains(string(raw), v) {
				t.Fatalf("directory sends a %s (%s)", k, v)
			}
		}
	}
}

// dirGet calls a directory endpoint as an employee in My view.
func dirGet(t *testing.T, c *client, path string) map[string]any {
	t.Helper()
	s, d := c.do("GET", path, "EMP-1020", "Employee", nil)
	if s != 200 {
		t.Fatalf("%s: %d %v", path, s, d)
	}
	return d
}

// dirSetup: the sample organisation with work-style emails (the sample's own contain the employee code), one person
// without an email, one who left and one not yet joined.
func dirSetup(t *testing.T) (*client, *store.Store, map[string]store.Row) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1020")
	ctx := context.Background()
	emps, err := st.DB.C("employee").Find(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.Row{}
	for _, e := range emps {
		id := e["id"].(string)
		byID[id] = e
		mail := strings.ToLower(strings.ReplaceAll(e["name"].(string), " ", ".")) + "@saltstayz.com"
		if _, err := st.DB.C("employee_personal").Update(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"email": mail}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.C("employee_personal").Update(ctx, bson.M{"employee_id": "EMP-1021"}, bson.M{"$set": bson.M{"email": ""}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1024"}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": store.DateOf(time.Now().AddDate(0, 0, -5))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1025"}, bson.M{"$set": bson.M{"status": "Pre-boarding"}}); err != nil {
		t.Fatal(err)
	}
	byID["EMP-1024"]["status"], byID["EMP-1025"]["status"] = "Inactive", "Pre-boarding"
	return c, st, byID
}

func dirNames(rows []any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, x := range rows {
		m := mbMap(x)
		out[m["name"].(string)] = m
	}
	return out
}

func TestMyDirectoryPeople(t *testing.T) {
	c, st, emps := dirSetup(t)
	active := 0
	for _, e := range emps {
		if e["status"] == "Active" {
			active++
		}
	}
	d := dirGet(t, c, "/api/me/directory/people?size=100")
	rows := mbList(d["rows"])
	if d["total"] != float64(active) || len(rows) != active || d["pages"] != 1.0 {
		t.Fatalf("want %d active people, got total %v, %d rows", active, d["total"], len(rows))
	}
	dirNoLeak(t, st, rows, dirPeopleFields)
	names := dirNames(rows)
	// Active only: nobody who left or has not joined yet.
	for _, id := range []string{"EMP-1024", "EMP-1025"} {
		if names[emps[id]["name"].(string)] != nil {
			t.Fatalf("%s (%s) is listed", id, emps[id]["status"])
		}
	}
	self := names[emps["EMP-1020"]["name"].(string)]
	want := strings.ToLower(strings.ReplaceAll(emps["EMP-1020"]["name"].(string), " ", ".")) + "@saltstayz.com"
	if self == nil || self["email"] != want || self["department"] != "F&B" || self["designation"] == "" || self["location"] == "" || self["dept"] == "" || len(self["ref"].(string)) != 16 {
		t.Fatalf("own row: %v", self)
	}
	if r := names[emps["EMP-1021"]["name"].(string)]; r == nil || r["email"] != nil {
		t.Fatalf("someone without an email: %v", r)
	}
	// Sorted by name when there is no search.
	for i := 1; i < len(rows); i++ {
		if mbMap(rows[i-1])["name"].(string) > mbMap(rows[i])["name"].(string) {
			t.Fatalf("not by name at %d", i)
		}
	}

	search := func(q string, extra ...string) []any {
		t.Helper()
		return mbList(dirGet(t, c, "/api/me/directory/people?size=100&q="+url.QueryEscape(q)+strings.Join(extra, ""))["rows"])
	}
	target := emps["EMP-1004"]["name"].(string)
	if target != "Meera Kulkarni" {
		t.Fatalf("sample name changed: %q", target)
	}
	for _, q := range []string{"Meera Kulkarni", "kulkarni meera", "mee kul", "merea", "kulkanri", "KULKARNY meera", "kulkarni@salt", "meera people ops"} {
		l := search(q)
		if len(l) == 0 || mbMap(l[0])["name"] != target {
			t.Fatalf("search %q: want %s first, got %v", q, target, l)
		}
	}
	if l := search("zee"); len(l) != 0 { // "mee" with a typo: under 4 letters there is no typo allowance
		t.Fatalf("a 3-letter typo matched: %v", l)
	}
	if l := search("zzqx"); len(l) != 0 {
		t.Fatalf("nonsense matched: %v", l)
	}
	// Department and location words find everyone there.
	fb := search("F&B")
	inFB := 0
	for _, e := range emps {
		if e["status"] == "Active" && e["department_id"] == self["dept"] {
			inFB++
		}
	}
	for _, x := range fb {
		if mbMap(x)["department"] != "F&B" && !strings.Contains(mbMap(x)["designation"].(string), "F&B") {
			t.Fatalf("F&B search: %v", x)
		}
	}
	if len(fb) < inFB {
		t.Fatalf("F&B search found %d of %d", len(fb), inFB)
	}

	// Filters: department and location ids.
	byDept := search("", "&dept="+self["dept"].(string))
	if len(byDept) != inFB {
		t.Fatalf("dept filter: %d, want %d", len(byDept), inFB)
	}
	for _, x := range byDept {
		if mbMap(x)["department"] != "F&B" {
			t.Fatalf("dept filter let in %v", x)
		}
	}
	loc := emps["EMP-1020"]["location_id"].(string)
	inLoc := 0
	for _, e := range emps {
		if e["status"] == "Active" && e["location_id"] == loc {
			inLoc++
		}
	}
	if l := search("", "&loc="+loc); len(l) != inLoc || inLoc == 0 {
		t.Fatalf("location filter: %d, want %d", len(l), inLoc)
	}
	if l := search("meera", "&dept="+self["dept"].(string)); len(l) != 0 {
		t.Fatalf("search ignored the department filter: %v", l)
	}

	// Paging: 10 at a time, no one twice, the last page partial, past the end empty.
	seen := map[string]bool{}
	pages := (active + 9) / 10
	for p := 1; p <= pages; p++ {
		d := dirGet(t, c, fmt.Sprintf("/api/me/directory/people?size=10&page=%d", p))
		l := mbList(d["rows"])
		if d["pages"] != float64(pages) || d["page"] != float64(p) || (p < pages && len(l) != 10) || (p == pages && len(l) != active-10*(pages-1)) {
			t.Fatalf("page %d: %v rows %d", p, d, len(l))
		}
		for _, x := range l {
			r := mbMap(x)["ref"].(string)
			if seen[r] {
				t.Fatalf("listed twice: %v", x)
			}
			seen[r] = true
		}
	}
	if len(seen) != active {
		t.Fatalf("paged through %d of %d", len(seen), active)
	}
	if l := mbList(dirGet(t, c, fmt.Sprintf("/api/me/directory/people?size=10&page=%d", pages+1))["rows"]); len(l) != 0 {
		t.Fatalf("past the end: %v", l)
	}
	if d := dirGet(t, c, "/api/me/directory/people?size=5000"); d["size"] != 50.0 {
		t.Fatalf("size not capped: %v", d["size"])
	}

	// Signed-in only.
	for _, p := range []string{"/api/me/directory/people", "/api/me/directory/departments", "/api/me/directory"} {
		if s, _ := c.do("GET", p, "", "", nil); s != 401 {
			t.Fatalf("%s without sign-in: %d", p, s)
		}
	}
}

func TestMyDirectoryDepartmentsAndChart(t *testing.T) {
	c, st, emps := dirSetup(t)
	ctx := context.Background()
	d := dirGet(t, c, "/api/me/directory/departments")
	depts := mbList(d["departments"])
	want := map[string]int{}
	active := 0
	for _, e := range emps {
		if e["status"] == "Active" {
			want[e["department_id"].(string)]++
			active++
		}
	}
	sum, mine := 0, ""
	for _, x := range depts {
		m := mbMap(x)
		for k := range m {
			if k != "id" && k != "name" && k != "count" && k != "mine" {
				t.Fatalf("department sends %q", k)
			}
		}
		if int(m["count"].(float64)) != want[m["id"].(string)] {
			t.Fatalf("%s: %v people, want %d", m["name"], m["count"], want[m["id"].(string)])
		}
		sum += int(m["count"].(float64))
		if m["mine"] == true {
			mine = m["name"].(string)
		}
	}
	if len(depts) != len(want) || sum != active || d["total"] != float64(active) || mine != "F&B" || d["mine"] != emps["EMP-1020"]["department_id"] {
		t.Fatalf("departments: %d of %d, %d people of %d, mine %q", len(depts), len(want), sum, active, mine)
	}
	lsum := 0
	for _, x := range mbList(d["locations"]) {
		lsum += int(mbMap(x)["count"].(float64))
	}
	if lsum != active {
		t.Fatalf("locations add up to %d of %d", lsum, active)
	}

	// Another department's chart, by id: same fields, no codes, the people of that department, refs as in the list.
	fo, err := st.DB.C("department").FindOne(ctx, bson.M{"name": "Front Office"})
	if err != nil || fo == nil {
		t.Fatal("no Front Office", err)
	}
	ch := dirGet(t, c, "/api/me/directory?dept="+fo["id"].(string))
	nodes := mbList(ch["nodes"])
	if ch["department"] != "Front Office" || ch["mine"] != false || ch["id"] != fo["id"] {
		t.Fatalf("chart: %v", ch)
	}
	dirNoLeak(t, st, nodes, dirChartFields)
	inFO := 0
	for _, e := range emps {
		if e["status"] == "Active" && e["department_id"] == fo["id"] {
			inFO++
		}
	}
	outside := 0
	for _, x := range nodes {
		m := mbMap(x)
		if m["me"] == true {
			t.Fatalf("caller marked in another department: %v", m)
		}
		if m["outside"] == true {
			outside++
		}
	}
	if len(nodes)-outside != inFO || inFO == 0 {
		t.Fatalf("Front Office chart: %d inside, want %d", len(nodes)-outside, inFO)
	}
	people := dirNames(mbList(dirGet(t, c, "/api/me/directory/people?size=100&dept="+fo["id"].(string))["rows"]))
	for _, x := range nodes {
		m := mbMap(x)
		if m["outside"] == true {
			continue
		}
		p := people[m["name"].(string)]
		if p == nil || p["ref"] != m["ref"] || p["email"] != m["email"] {
			t.Fatalf("chart and list disagree on %v: %v", m, p)
		}
	}
	// No dept: the caller's own, marked mine and with the caller in it. An unknown one: 404.
	own := dirGet(t, c, "/api/me/directory")
	if own["department"] != "F&B" || own["mine"] != true {
		t.Fatalf("own chart: %v", own)
	}
	found := false
	for _, x := range mbList(own["nodes"]) {
		found = found || mbMap(x)["me"] == true
	}
	if !found {
		t.Fatal("caller missing from their own chart")
	}
	if s, _ := c.do("GET", "/api/me/directory?dept=nope", "EMP-1020", "Employee", nil); s != 404 {
		t.Fatalf("unknown department: %d", s)
	}
}

// With 2,000 more people the list still answers quickly, a page at a time, and a typo still finds the person.
func TestMyDirectoryPeopleAtScale(t *testing.T) {
	c, st, emps := dirSetup(t)
	ctx := context.Background()
	src := emps["EMP-1020"]
	var es, ps []any
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("EMP-%d", 50000+i)
		e := bson.M{}
		for k, v := range src {
			if k != "id" {
				e[k] = v
			}
		}
		e["_id"], e["name"] = id, fmt.Sprintf("Person %04d Load", i)
		es = append(es, e)
		ps = append(ps, bson.M{"employee_id": id, "email": fmt.Sprintf("person%04d@saltstayz.com", i), "mobile": fmt.Sprintf("97%08d", i)})
	}
	es = append(es, bson.M{"_id": "EMP-59999", "name": "Harshvardhan Kulshreshtha", "type": src["type"], "doj": src["doj"], "department_id": src["department_id"],
		"designation_id": src["designation_id"], "grade": src["grade"], "location_id": src["location_id"], "shift_code": src["shift_code"], "status": "Active"})
	if _, err := st.DB.C("employee").Raw().InsertMany(ctx, es); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("employee_personal").Raw().InsertMany(ctx, ps); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d := dirGet(t, c, "/api/me/directory/people?q="+url.QueryEscape("kulshresta harsh"))
	took := time.Since(start)
	l := mbList(d["rows"])
	if len(l) == 0 || mbMap(l[0])["name"] != "Harshvardhan Kulshreshtha" {
		t.Fatalf("typo search at scale: %v", l)
	}
	if took > 3*time.Second {
		t.Fatalf("search over 2,000+ people took %s", took)
	}
	d = dirGet(t, c, "/api/me/directory/people?page=3")
	if len(mbList(d["rows"])) != 50 || d["total"].(float64) < 2000 {
		t.Fatalf("page 3 of %v: %d rows", d["total"], len(mbList(d["rows"])))
	}
	t.Logf("typo search over %v people: %s", d["total"], took)
}
