package api_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Round 2 gaps of the Dashboard, Employees, Employee file, Work policies, Masters and Nominations pages.

func u7list(v any) []any { l, _ := v.([]any); return l }
func u7map(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func u7today() string { return time.Now().Format("2006-01-02") }

func TestUI2PeopleDashboard(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1008", "EMP-1011", "EMP-1013")
	// Activity in the property manager's scope (EMP-1020, Sector 29 Hub) and outside it (EMP-1016, Saket).
	for _, id := range []string{"EMP-1020", "EMP-1016"} {
		if s, r := c.do("PATCH", "/api/employees/"+id+"/job", "EMP-1002", "HR Admin", map[string]any{"changes": map[string]string{"shift": "M"}, "reason": "Data correction", "effectiveFrom": u7today()}); s != 200 {
			t.Fatalf("job change %s: %v", id, r)
		}
	}

	t.Run("overview: payroll month and status for every role", func(t *testing.T) {
		for _, who := range [][2]string{{"EMP-1002", "HR Admin"}, {"EMP-1008", "Property Manager"}, {"EMP-1008", "Employee"}} {
			s, d := c.do("GET", "/api/dashboard", who[0], who[1], nil)
			if s != 200 {
				t.Fatalf("%v: %v", who, d)
			}
			p := u7map(d["payroll"])
			if p["ym"] == nil || p["status"] == nil || p["status"] == "" {
				t.Fatalf("%v: payroll %v", who, p)
			}
		}
	})

	t.Run("overview: recent activity is scoped for roles that do not manage access", func(t *testing.T) {
		_, d := c.do("GET", "/api/dashboard", "EMP-1008", "Property Manager", nil)
		recent := u7list(d["recent"])
		if len(recent) == 0 {
			t.Fatal("property manager sees no activity in scope")
		}
		// Both changes are "Changed job details"; the scoped list has only the one in scope.
		n := 0
		for _, x := range recent {
			if u7map(x)["action"] == "Changed job details" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("want 1 in-scope job change, got %d: %v", n, recent)
		}
		_, a := c.do("GET", "/api/dashboard", "EMP-1002", "HR Admin", nil)
		n = 0
		for _, x := range u7list(a["recent"]) {
			if u7map(x)["action"] == "Changed job details" {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("HR Admin should see the whole log, got %d", n)
		}
	})

	t.Run("payroll tab: open month totals by city and type, filters honoured", func(t *testing.T) {
		s, d := c.do("GET", "/api/dashboard?tab=pay", "EMP-1011", "Payroll", nil)
		if s != 200 {
			t.Fatal(d)
		}
		o := u7map(d["open"])
		if o["ym"] == nil || len(u7list(o["byType"])) != 3 || o["byCity"] == nil {
			t.Fatalf("open month block: %v", o)
		}
		if s, _ := c.do("GET", "/api/dashboard?tab=pay", "EMP-1008", "Property Manager", nil); s != 403 {
			t.Fatalf("property manager reads payroll figures: %d", s)
		}
		_, f := c.do("GET", "/api/dashboard?tab=pay&type=Retainer", "EMP-1011", "Payroll", nil)
		for _, x := range u7list(u7map(f["open"])["byType"]) {
			if m := u7map(x); m["label"] != "Retainer" && num(m["value"]) != 0 {
				t.Fatalf("type filter not applied: %v", m)
			}
		}
	})

	t.Run("attendance tab: figures computed on the server with filters", func(t *testing.T) {
		s, d := c.do("GET", "/api/dashboard?tab=att", "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatal(d)
		}
		for _, k := range []string{"counts", "lateByLocation", "open", "roster"} {
			if d[k] == nil {
				t.Fatalf("missing %s: %v", k, d)
			}
		}
		all := num(d["people"])
		_, f := c.do("GET", "/api/dashboard?tab=att&type=Retainer&department=Housekeeping", "EMP-1002", "HR Admin", nil)
		if num(f["people"]) >= all || num(f["people"]) == 0 {
			t.Fatalf("filters not applied: %v of %v", f["people"], all)
		}
		_, p := c.do("GET", "/api/dashboard?tab=att", "EMP-1008", "Property Manager", nil)
		if num(p["people"]) >= all {
			t.Fatalf("property manager sees beyond scope: %v of %v", p["people"], all)
		}
		// My view: only the person's own attendance.
		if s, own := c.do("GET", "/api/dashboard?tab=att", "EMP-1008", "Employee", nil); s != 200 || num(own["people"]) != 1 {
			t.Fatalf("employee view: %d %v", s, own["people"])
		}
		// Every dashboard role sees the tab, as in the prototype, even without attendance records (Finance Head).
		if s, fh := c.do("GET", "/api/dashboard?tab=att", "EMP-1013", "Finance Head", nil); s != 200 || num(fh["people"]) != all {
			t.Fatalf("finance head: %d %v of %v", s, fh["people"], all)
		}
	})
}

func TestUI2PeopleEmployeeRecord(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1008", "EMP-1020")
	s, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
	if s != 200 {
		t.Fatal(e)
	}
	p := u7map(e["policy"])
	if p["name"] == nil || p["name"] == "" || p["via"] == nil || p["optHol"] == nil || p["cl"] == nil || p["pl"] == nil {
		t.Fatalf("policy: %v", p)
	}
	if num(p["probMonths"]) > 0 && p["probEnds"] == nil {
		t.Fatalf("probation end missing: %v", p)
	}
	if e["state"] != "Haryana" || e["region"] != "North" {
		t.Fatalf("state and region: %v %v", e["state"], e["region"])
	}
	if e["structure"] == nil || e["structure"] == "" || len(u7list(e["structureHistory"])) == 0 {
		t.Fatalf("structure: %v %v", e["structure"], e["structureHistory"])
	}
	bh := u7list(e["bankHistory"])
	if len(bh) == 0 || !strings.HasPrefix(fmt.Sprint(u7map(bh[0])["account_no"]), "5010") {
		t.Fatalf("bank history for HR Admin (full): %v", bh)
	}
	// A role without salary or bank rights still sees the policy and the structure name, not the bank history.
	_, pm := c.do("GET", "/api/employees/EMP-1020", "EMP-1008", "Property Manager", nil)
	if pm["salary"] != nil || pm["bankHistory"] != nil {
		t.Fatalf("property manager sees salary or bank: %v %v", pm["salary"], pm["bankHistory"])
	}
	if pm["structure"] == nil || pm["policy"] == nil {
		t.Fatalf("property manager misses structure or policy: %v", pm)
	}
	// The employee sees their own bank history.
	_, own := c.do("GET", "/api/employees/EMP-1020", "EMP-1020", "Employee", nil)
	if len(u7list(own["bankHistory"])) == 0 {
		t.Fatal("own bank history missing")
	}
	// Change requests in the payroll file carry the asker's name.
	if s, r := c.do("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_change", "employeeId": "EMP-1020",
		"payload": map[string]any{"kind": "Personal", "mobile": "9876543210"}}); s != 200 {
		t.Fatal(r)
	}
	_, f := c.do("GET", "/api/employees/EMP-1020/file", "EMP-1002", "HR Admin", nil)
	found := false
	for _, q := range u7list(f["requests"]) {
		if m := u7map(q); m["wf"] == "p_change" && m["raised_by_name"] != nil && m["raised_by_name"] != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("raised_by_name missing on change requests: %v", f["requests"])
	}
}

func TestUI2PeopleReactivateRejoin(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1004")
	inactive := func(id, lwd string) {
		t.Helper()
		if s, r := c.do("POST", "/api/employees/"+id+"/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": lwd, "reason": "Resigned"}); s != 200 {
			t.Fatalf("inactive %s: %v", id, r)
		}
	}
	t.Run("undo inactive restores the record and keeps the history", func(t *testing.T) {
		inactive("EMP-1020", u7today())
		if s, _ := c.do("POST", "/api/employees/EMP-1020/reactivate", "EMP-1004", "HRBP", nil); s != 403 {
			t.Fatalf("HRBP without emp.status edit reactivated: %d", s)
		}
		if s, r := c.do("POST", "/api/employees/EMP-1020/reactivate", "EMP-1002", "HR Admin", nil); s != 200 {
			t.Fatal(r)
		}
		_, e := c.do("GET", "/api/employees/EMP-1020", "EMP-1002", "HR Admin", nil)
		if e["status"] != "Active" || e["lwd"] != nil || e["exit_reason"] != nil {
			t.Fatalf("not restored: %v %v %v", e["status"], e["lwd"], e["exit_reason"])
		}
		_, h := c.do("GET", "/api/employees/EMP-1020/history", "EMP-1002", "HR Admin", nil)
		top := u7map(u7list(h["list"])[0])
		if top["field"] != "Status" || top["new_value"] != "Active" || top["reason"] != "Marked inactive by mistake" {
			t.Fatalf("history: %v", top)
		}
		if s, _ := c.do("POST", "/api/employees/EMP-1020/reactivate", "EMP-1002", "HR Admin", nil); s != 400 {
			t.Fatalf("reactivating an active person: %d", s)
		}
		if s, _ := c.do("POST", "/api/employees/EMP-1002/reactivate", "EMP-1002", "HR Admin", nil); s != 403 {
			t.Fatalf("own record: %d", s)
		}
	})

	t.Run("rejoin starts a shortlist with the code choice; undo is then refused", func(t *testing.T) {
		yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		inactive("EMP-1026", yesterday)
		_, e := c.do("GET", "/api/employees/EMP-1026", "EMP-1002", "HR Admin", nil)
		rj := u7map(e["rejoin"])
		if rj == nil || rj["pending"] != false || rj["rehire"] != "Yes" {
			t.Fatalf("rejoin info: %v", rj)
		}
		if s, r := c.do("POST", "/api/employees/EMP-1026/rejoin", "EMP-1002", "HR Admin", map[string]any{}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "shortlist") {
			t.Fatalf("no direct rejoin offer: %d %v", s, r)
		}
		s, pre := c.do("GET", "/api/candidates/rejoin/EMP-1026", "EMP-1002", "HR Admin", nil)
		if s != 200 || u7map(pre["data"])["rejoinOf"] != "EMP-1026" || len(pre["experience"].([]any)) != 1 {
			t.Fatalf("prefill: %d %v", s, pre)
		}
		data := u7map(pre["data"])
		data["codeMode"], data["ctc"], data["dob"], data["gender"] = "new", 252000, "1995-01-01", "Male"
		if data["manager"] == nil {
			data["manager"] = "EMP-1009"
		}
		if data["phone"] == nil {
			data["phone"] = "9876506001"
		}
		if data["email"] == nil {
			data["email"] = "rejoin.1026@example.com"
		}
		body := map[string]any{"data": data, "experience": pre["experience"]}
		s, r := c.do("POST", "/api/candidates", "EMP-1002", "HR Admin", body)
		if s != 200 {
			t.Fatal(r)
		}
		_, x := c.do("GET", "/api/candidates/"+fmt.Sprint(r["id"]), "EMP-1002", "HR Admin", nil)
		if x["rejoin_of"] != "EMP-1026" || x["codeMode"] != "new" {
			t.Fatalf("candidate: %v %v", x["rejoin_of"], x["codeMode"])
		}
		if s, _ := c.do("POST", "/api/candidates", "EMP-1002", "HR Admin", body); s != 400 {
			t.Fatalf("second rejoin while one is open: %d", s)
		}
		if s, _ := c.do("POST", "/api/employees/EMP-1026/reactivate", "EMP-1002", "HR Admin", nil); s != 400 {
			t.Fatalf("undo while a rejoin is open: %d", s)
		}
		_, e = c.do("GET", "/api/employees/EMP-1026", "EMP-1002", "HR Admin", nil)
		if u7map(e["rejoin"])["pending"] != true {
			t.Fatalf("pending flag: %v", e["rejoin"])
		}
		if s, _ := c.do("POST", "/api/employees/EMP-1026/rejoin", "EMP-1004", "HRBP", map[string]any{}); s != 400 && s != 403 {
			t.Fatalf("HRBP rejoin: %d", s)
		}
	})
}

func TestUI2PeopleNominations(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1020", "EMP-1008")
	_, sc := c.do("GET", "/api/nominations/schemes", "EMP-1002", "HR Admin", nil)
	var gi map[string]any
	for _, x := range u7list(sc["list"]) {
		if m := u7map(x); m["name"] == "Group insurance beneficiary" {
			gi = m
		}
	}
	if gi == nil || gi["verify"] != true {
		t.Fatalf("scheme: %v", gi)
	}
	for _, d := range u7list(gi["docs"]) {
		if u7map(d)["on"] != true {
			t.Fatalf("documents default to on: %v", d)
		}
	}
	// Final when sent, with the required nominee document switched off.
	gi["verify"] = false
	for _, d := range u7list(gi["docs"]) {
		u7map(d)["on"] = false
	}
	id := fmt.Sprint(gi["id"])
	if s, r := c.do("PUT", "/api/nominations/schemes/"+id, "EMP-1002", "HR Admin", gi); s != 200 {
		t.Fatal(r)
	}
	_, sc = c.do("GET", "/api/nominations/schemes", "EMP-1002", "HR Admin", nil)
	for _, x := range u7list(sc["list"]) {
		if m := u7map(x); fmt.Sprint(m["id"]) == id && (m["verify"] != false || u7map(u7list(m["docs"])[0])["on"] != false) {
			t.Fatalf("not stored: %v", m)
		}
	}
	people := []map[string]any{{"name": "Sita Devi", "rel": "Spouse", "dob": "1990-02-01", "share": 100, "guardian": ""}}
	if s, r := c.do("POST", "/api/me/nominations/"+id, "EMP-1020", "Employee", map[string]any{"people": people}); s != 200 {
		t.Fatal(r)
	}
	s, l := c.do("GET", "/api/employees/EMP-1020/nominations", "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatal(l)
	}
	var item map[string]any
	for _, x := range u7list(l["list"]) {
		if m := u7map(x); fmt.Sprint(u7map(m["scheme"])["id"]) == id {
			item = m
		}
	}
	if item == nil || item["state"] != "Verified" || num(u7map(item["current"])["ver"]) != 1 || u7map(item["current"])["decided_by"] != "Automatic" {
		t.Fatalf("final when sent: %v", item)
	}
	// A second version: both are listed, the newest is current.
	people[0]["name"] = "Sita Kumari"
	if s, r := c.do("POST", "/api/me/nominations/"+id, "EMP-1020", "Employee", map[string]any{"people": people}); s != 200 {
		t.Fatal(r)
	}
	_, l = c.do("GET", "/api/employees/EMP-1020/nominations", "EMP-1011", "Payroll", nil)
	for _, x := range u7list(l["list"]) {
		if m := u7map(x); fmt.Sprint(u7map(m["scheme"])["id"]) == id {
			if len(u7list(m["versions"])) != 2 || num(u7map(m["current"])["ver"]) != 2 {
				t.Fatalf("versions: %v", m)
			}
		}
	}
	if s, _ := c.do("GET", "/api/employees/EMP-1020/nominations", "EMP-1008", "Property Manager", nil); s != 403 {
		t.Fatalf("property manager reads nominations: %d", s)
	}
	if s, _ := c.do("GET", "/api/employees/EMP-1020/nominations", "EMP-1020", "Employee", nil); s != 200 {
		t.Fatalf("own nominations: %d", s)
	}

	// Payroll's lists: department and version on submissions; department, joining date and reminders on pending rows.
	if s, r := c.do("POST", "/api/me/nominations/1", "EMP-1020", "Employee", map[string]any{"people": people}); s != 400 {
		t.Fatalf("EPF needs documents: %d %v", s, r)
	}
	_, q := c.do("GET", "/api/nominations", "EMP-1011", "Payroll", nil)
	var pend map[string]any
	for _, x := range u7list(q["pending"]) {
		if m := u7map(x); m["id"] == "EMP-1020" && num(m["scheme_id"]) == 1 {
			pend = m
		}
	}
	if pend == nil || pend["department"] == nil || pend["doj"] == nil || pend["reminded"] != nil {
		t.Fatalf("pending row: %v", pend)
	}
	if s, _ := c.do("POST", "/api/nominations/remind", "EMP-1008", "Property Manager", map[string]any{"employeeId": "EMP-1020", "schemeId": 1}); s != 403 {
		t.Fatalf("property manager reminds: %d", s)
	}
	if s, r := c.do("POST", "/api/nominations/remind", "EMP-1011", "Payroll", map[string]any{"employeeId": "EMP-1020", "schemeId": 1}); s != 200 || r["reminded"] != u7today() {
		t.Fatalf("remind: %d %v", s, r)
	}
	_, q = c.do("GET", "/api/nominations", "EMP-1011", "Payroll", nil)
	for _, x := range u7list(q["pending"]) {
		if m := u7map(x); m["id"] == "EMP-1020" && num(m["scheme_id"]) == 1 && m["reminded"] != u7today() {
			t.Fatalf("reminder not shown: %v", m)
		}
	}
	// Back to verified by Payroll: a submission waits, and carries its version and department.
	gi["verify"] = true
	if s, r := c.do("PUT", "/api/nominations/schemes/"+id, "EMP-1002", "HR Admin", gi); s != 200 {
		t.Fatal(r)
	}
	people[0]["name"] = "Sita Rani"
	if s, r := c.do("POST", "/api/me/nominations/"+id, "EMP-1020", "Employee", map[string]any{"people": people}); s != 200 {
		t.Fatal(r)
	}
	_, q = c.do("GET", "/api/nominations", "EMP-1011", "Payroll", nil)
	ok := false
	for _, x := range u7list(q["submitted"]) {
		if m := u7map(x); m["employee_id"] == "EMP-1020" && fmt.Sprint(m["scheme_id"]) == id {
			ok = num(m["version"]) == 3 && m["department"] == "F&B"
		}
	}
	if !ok {
		t.Fatalf("submitted row: %v", q["submitted"])
	}
}

func TestUI2PeoplePolicies(t *testing.T) {
	c := setup(t)
	signAll(c, "EMP-1002", "EMP-1011")
	_, list := c.do("GET", "/api/policies", "EMP-1002", "HR Admin", nil)
	pols := u7list(list["list"])
	if len(pols) < 2 {
		t.Fatalf("need two policies: %v", pols)
	}
	a, b := u7map(pols[0]), u7map(pols[1])
	aid, bid := fmt.Sprint(a["id"]), fmt.Sprint(b["id"])

	t.Run("effective date is kept with the change", func(t *testing.T) {
		_, g := c.do("GET", "/api/policies/"+bid, "EMP-1002", "HR Admin", nil)
		p := u7map(g["policy"])
		p["name"] = fmt.Sprint(p["name"]) + " (edited)"
		eff := time.Now().AddDate(0, 1, 0).Format("2006-01-02")
		body := map[string]any{"name": p["name"], "settings": p["settings"], "leavePlanId": p["leavePlanId"], "calendarId": p["calendarId"], "calMap": p["calMap"], "effectiveFrom": eff}
		if s, r := c.do("PUT", "/api/policies/"+bid, "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		body["effectiveFrom"] = "soon"
		if s, _ := c.do("PUT", "/api/policies/"+bid, "EMP-1002", "HR Admin", body); s != 400 {
			t.Fatalf("bad date accepted: %d", s)
		}
		_, g = c.do("GET", "/api/policies/"+bid, "EMP-1002", "HR Admin", nil)
		if h := u7list(g["history"]); len(h) == 0 || u7map(h[0])["effective_from"] != eff {
			t.Fatalf("history: %v", g["history"])
		}
	})

	t.Run("a mapping on another policy is refused; Edit moves it; counts per mapping and grade", func(t *testing.T) {
		put := func(id string, maps []map[string]any) {
			t.Helper()
			// Contract staff in Housekeeping get their policy through the employment type: keep them there (policy
			// conflict check, policy_conflict.go).
			if s, r := c.do("PUT", "/api/policies/"+id+"/maps", "EMP-1002", "HR Admin", map[string]any{"maps": maps, "resolution": map[string]any{"mode": "keep"}}); s != 200 {
				t.Fatal(r)
			}
		}
		put(aid, []map[string]any{{"level": "department", "ref": "Housekeeping", "on": true}})
		// Mapping it again on B is refused (owner, 24 Nov 2026): it moves only by Edit › Work policy.
		hkB := map[string]any{"maps": []map[string]any{{"level": "department", "ref": "Housekeeping", "on": true}}, "resolution": map[string]any{"mode": "keep"}}
		if s, r := c.do("PUT", "/api/policies/"+bid+"/maps", "EMP-1002", "HR Admin", hkB); s != 409 || !strings.HasPrefix(fmt.Sprint(r["error"]), "Already applied to ") {
			t.Fatalf("the same mapping on a second policy: %d %v", s, r)
		}
		_, ga := c.do("GET", "/api/policies/"+aid, "EMP-1002", "HR Admin", nil)
		for _, m := range u7list(u7map(ga["policy"])["maps"]) {
			if x := u7map(m); x["ref"] == "Housekeeping" && x["on"] == true {
				body := map[string]any{"policyId": bid, "resolution": map[string]any{"mode": "keep"}}
				if s, r := c.do("PUT", fmt.Sprintf("/api/policies/%s/maps/%v", aid, x["id"]), "EMP-1002", "HR Admin", body); s != 200 {
					t.Fatalf("edit › move: %d %v", s, r)
				}
			}
		}
		_, ga = c.do("GET", "/api/policies/"+aid, "EMP-1002", "HR Admin", nil)
		for _, m := range u7list(u7map(ga["policy"])["maps"]) {
			if x := u7map(m); x["ref"] == "Housekeeping" && x["on"] != false {
				t.Fatalf("not moved off policy A: %v", x)
			}
		}
		_, gb := c.do("GET", "/api/policies/"+bid, "EMP-1002", "HR Admin", nil)
		var hk map[string]any
		for _, m := range u7list(u7map(gb["policy"])["maps"]) {
			if x := u7map(m); x["ref"] == "Housekeeping" {
				hk = x
			}
		}
		if hk == nil || hk["on"] != true || num(hk["employees"]) == 0 {
			t.Fatalf("mapping count: %v", hk)
		}
		gc := u7map(gb["gradeCounts"])
		tot := 0.0
		for _, v := range gc {
			tot += num(v)
		}
		if tot < num(hk["employees"]) {
			t.Fatalf("grade counts %v below mapping count %v", gc, hk["employees"])
		}
	})

	t.Run("coverage: the full matrix and the default rows with grade and city", func(t *testing.T) {
		s, cv := c.do("GET", "/api/policies/coverage", "EMP-1002", "HR Admin", nil)
		if s != 200 {
			t.Fatal(cv)
		}
		sum := 0.0
		for _, vias := range u7map(cv["matrix"]) {
			for _, n := range u7map(vias) {
				sum += num(n)
			}
		}
		if sum != num(cv["total"]) || sum == 0 {
			t.Fatalf("matrix sums to %v, total %v", sum, cv["total"])
		}
		if num(cv["defaultTotal"]) != float64(len(u7list(cv["defaults"]))) && len(u7list(cv["defaults"])) < 500 {
			t.Fatalf("defaults: %v of %v", len(u7list(cv["defaults"])), cv["defaultTotal"])
		}
		for _, x := range u7list(cv["defaults"]) {
			if m := u7map(x); m["grade"] == nil || m["city"] == nil || m["city"] == "" || m["via"] != "Default" {
				t.Fatalf("default row: %v", m)
			}
		}
	})

	t.Run("leave plan history, leave type inactive and copy", func(t *testing.T) {
		_, lp := c.do("GET", "/api/leave-plans", "EMP-1002", "HR Admin", nil)
		plan := u7map(u7map(u7list(lp["plans"])[0])["plan"])
		pid := fmt.Sprint(plan["id"])
		eff := time.Now().AddDate(0, 0, 3).Format("2006-01-02")
		body := map[string]any{"name": plan["name"], "description": "Edited in test", "lv": plan["lv"], "lrx": plan["lrx"], "types": plan["types"], "effectiveFrom": eff}
		if s, r := c.do("PUT", "/api/leave-plans/"+pid, "EMP-1002", "HR Admin", body); s != 200 {
			t.Fatal(r)
		}
		s, h := c.do("GET", "/api/leave-plans/"+pid+"/history", "EMP-1002", "HR Admin", nil)
		if s != 200 || len(u7list(h["list"])) == 0 || u7map(u7list(h["list"])[0])["effective_from"] != eff {
			t.Fatalf("plan history: %v", h)
		}
		if s, _ := c.do("POST", "/api/leave-types/Sick%20leave/inactive", "EMP-1011", "Payroll", nil); s != 403 {
			t.Fatalf("payroll (view only) made a leave type inactive: %d", s)
		}
		s, r := c.do("POST", "/api/leave-types/"+url.PathEscape("Sick leave")+"/inactive", "EMP-1002", "HR Admin", nil)
		if s != 200 || r["status"] != "Inactive" {
			t.Fatalf("inactive: %d %v", s, r)
		}
		if s, _ := c.do("POST", "/api/leave-types/Sick%20leave/inactive", "EMP-1002", "HR Admin", nil); s != 400 {
			t.Fatalf("twice: %d", s)
		}
		_, lp = c.do("GET", "/api/leave-plans", "EMP-1002", "HR Admin", nil)
		for _, x := range u7list(lp["types"]) {
			if m := u7map(x); m["name"] == "Sick leave" && m["status"] != "Inactive" {
				t.Fatalf("status not stored: %v", m)
			}
		}
		if s, r := c.do("POST", "/api/leave-types/Sick%20leave/reactivate", "EMP-1002", "HR Admin", nil); s != 200 || r["status"] != "Active" {
			t.Fatalf("reactivate: %d %v", s, r)
		}
		if s, _ := c.do("POST", "/api/leave-types/Nope/inactive", "EMP-1002", "HR Admin", nil); s != 404 {
			t.Fatalf("unknown type: %d", s)
		}
		if s, r := c.do("POST", "/api/leave-types", "EMP-1002", "HR Admin", map[string]any{"name": "Study leave", "kind": "balance", "paid": true, "copyFrom": "Casual leave"}); s != 200 {
			t.Fatal(r)
		}
		_, lp = c.do("GET", "/api/leave-plans", "EMP-1002", "HR Admin", nil)
		for _, x := range u7list(lp["plans"]) {
			pl := u7map(u7map(x)["plan"])
			types := u7map(pl["types"])
			cl, st := u7map(types["Casual leave"]), u7map(types["Study leave"])
			if cl == nil {
				continue
			}
			if st == nil || st["on"] != false || num(st["days"]) != num(cl["days"]) || fmt.Sprint(st["types"]) != fmt.Sprint(cl["types"]) {
				t.Fatalf("copy in %v: %v vs %v", pl["name"], st, cl)
			}
		}
		if s, _ := c.do("POST", "/api/leave-types", "EMP-1002", "HR Admin", map[string]any{"name": "Odd leave", "kind": "event", "paid": true, "copyFrom": "Nope"}); s != 400 {
			t.Fatalf("copy from unknown type: %d", s)
		}
	})
}

func TestUI2PeopleMasters(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	_, sh := c.do("GET", "/api/masters/shift", "EMP-1002", "HR Admin", nil)
	var g map[string]any
	for _, x := range u7list(sh["list"]) {
		if m := u7map(x); m["code"] == "G" {
			g = m
		}
	}
	if g == nil || g["created_at"] == nil {
		t.Fatalf("shift created_at: %v", g)
	}
	active, _ := st.DB.C("employee").Count(context.Background(), bson.M{"shift_code": "G", "status": "Active"})
	want := int(active)
	s, r := c.do("GET", "/api/masters/shift/"+fmt.Sprint(g["id"])+"/impact", "EMP-1002", "HR Admin", nil)
	if s != 200 || int(num(r["n"])) != want || want == 0 {
		t.Fatalf("impact: %d %v want %d", s, r, want)
	}
	_, et := c.do("GET", "/api/masters/employment_type", "EMP-1002", "HR Admin", nil)
	for _, x := range u7list(et["list"]) {
		if m := u7map(x); m["name"] == "Permanent" {
			if _, r := c.do("GET", "/api/masters/employment_type/"+fmt.Sprint(m["id"])+"/impact", "EMP-1002", "HR Admin", nil); num(r["n"]) == 0 {
				t.Fatalf("employment type impact: %v", r)
			}
		}
	}
	if s, _ := c.do("GET", "/api/masters/shift/"+fmt.Sprint(g["id"])+"/impact", "EMP-1020", "Employee", nil); s != 403 {
		t.Fatalf("employee reads impact: %d", s)
	}
	if s, _ := c.do("GET", "/api/masters/shift/00000000-0000-0000-0000-000000000000/impact", "EMP-1002", "HR Admin", nil); s != 404 {
		t.Fatalf("unknown id: %d", s)
	}
}
