package api_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/seed"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

// deep3Str is a stored text value ("" for null or another type).
func deep3Str(v any) string { s, _ := v.(string); return s }

// TestDeepEndpointSweep calls every endpoint the flow tests do not reach, checks the effect and
// one refusal for each.
func TestDeepEndpointSweep(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1008", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1020")
	tt := t // the running subtest; helpers fail it, not the parent
	ok := func(method, path, emp, role string, body any) map[string]any {
		tt.Helper()
		s, r := c.do(method, path, emp, role, body)
		if s != 200 {
			tt.Fatalf("%s %s as %s: %d %v", method, path, role, s, r)
		}
		return r
	}
	no := func(want int, method, path, emp, role string, body any) {
		tt.Helper()
		if s, r := c.do(method, path, emp, role, body); s != want {
			tt.Fatalf("%s %s as %s: %d, want %d (%v)", method, path, role, s, want, r)
		}
	}
	list := func(r map[string]any) []any { l, _ := r["list"].([]any); return l }
	A := func(method, path string, body any) map[string]any {
		tt.Helper()
		return ok(method, path, "EMP-1002", "HR Admin", body)
	}
	run := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) { tt = t; f(t) })
	}

	run("access control", func(t *testing.T) {
		if len(list(A("GET", "/api/rights", nil))) < 20 {
			t.Fatal("rights catalogue")
		}
		roles := list(A("GET", "/api/roles", nil))
		if len(roles) < 15 {
			t.Fatal("roles")
		}
		ok("PUT", "/api/roles/Finance/rights", "EMP-1002", "HR Admin", map[string]any{"rights": map[string]any{"rep.reports": 2}, "canDownload": true})
		lv := 0
		if v, ok := apiDoc(st, "role_right", bson.M{"role_name": "Finance", "right_id": "rep.reports"})["level"].(int16); ok {
			lv = int(v)
		}
		if lv != 2 {
			t.Fatal("role rights saved")
		}
		no(403, "PUT", "/api/roles/Finance/rights", "EMP-1004", "HRBP", map[string]any{"rights": map[string]any{}})
		maps := list(A("GET", "/api/role-map", nil))
		var hrbp map[string]any
		for _, m := range maps {
			if x := m.(map[string]any); x["role_name"] == "HRBP" {
				hrbp = x
			}
		}
		locs := hrbp["location_ids"].([]any)
		ids := []string{}
		for _, l := range locs[:1] {
			ids = append(ids, l.(string))
		}
		ok("PATCH", "/api/role-map/"+hrbp["id"].(string), "EMP-1002", "HR Admin", map[string]any{"employeeId": "EMP-1004", "role": "HRBP", "access": "locs", "locationIds": ids})
		_, e := c.do("GET", "/api/employees?size=100", "EMP-1004", "HRBP", nil)
		for _, x := range e["rows"].([]any) {
			if x.(map[string]any)["location_id"] != ids[0] {
				t.Fatal("narrowed HRBP mapping limits the list")
			}
		}
		if len(list(A("GET", "/api/audit", nil))) == 0 {
			t.Fatal("audit log")
		}
		no(403, "GET", "/api/audit", "EMP-1020", "Employee", nil)
	})

	run("approvals: queue, simulate, cancel, matrix", func(t *testing.T) {
		sim := A("POST", "/api/approvals/simulate", map[string]any{"wf": "p_adv", "employeeId": "EMP-1020", "payload": map[string]any{"amount": 30000}})
		if len(sim["people"].([]any)) == 0 {
			t.Fatal("simulation lists approvers")
		}
		_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Bill")
		r := ok("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_claim", "payload": map[string]any{"amount": 400, "claimType": "Other", "billDate": today(), "doc": doc["id"]}})
		id := r["id"].(string)
		for _, tab := range []string{"mine", "open", "done", "info"} {
			ok("GET", "/api/approvals?tab="+tab, "EMP-1009", "Line Manager", nil)
		}
		mine := list(ok("GET", "/api/approvals?tab=mine", "EMP-1009", "Line Manager", nil))
		found := false
		for _, x := range mine {
			found = found || x.(map[string]any)["id"] == id
		}
		if !found {
			t.Fatal("the line manager's inbox has the claim")
		}
		// A comma-separated group of request types (Payroll view's Payroll and Joining approvals tabs).
		inGroup := func(wf string) (bool, []string) {
			var wfs []string
			has := false
			for _, x := range list(ok("GET", "/api/approvals?tab=open&wf="+wf, "EMP-1009", "Line Manager", nil)) {
				m := x.(map[string]any)
				wfs = append(wfs, m["wf"].(string))
				has = has || m["id"] == id
			}
			return has, wfs
		}
		if has, wfs := inGroup("regularize,leave,p_claim,p_adv"); !has || len(wfs) == 0 {
			t.Fatalf("the pay group lists the claim: %v", wfs)
		} else {
			for _, w := range wfs {
				if !strings.Contains("regularize,leave,p_claim,p_adv", w) {
					t.Fatalf("the pay group lists a %s", w)
				}
			}
		}
		if has, wfs := inGroup("joining,verify"); has {
			t.Fatalf("the joining group lists the claim: %v", wfs)
		}
		no(403, "POST", "/api/approvals/"+id+"/cancel", "EMP-1009", "Line Manager", nil)
		ok("POST", "/api/approvals/"+id+"/cancel", "EMP-1020", "Employee", nil)
		no(400, "POST", "/api/approvals/"+id+"/cancel", "EMP-1020", "Employee", nil)
		mx := list(A("GET", "/api/approvals/flows", nil))
		var m map[string]any
		for _, x := range mx {
			if y := x.(map[string]any); y["wf"] == "p_claim" && y["flow"] == "Operations" {
				m = y
			}
		}
		rules := m["rules"].([]any)
		rules[0].(map[string]any)["label"] = "Every claim (edited)"
		A("PUT", "/api/approvals/flows/p_claim/Operations", map[string]any{"rules": rules})
		no(400, "PUT", "/api/approvals/flows/p_claim/Operations", "EMP-1002", "HR Admin", map[string]any{"rules": []any{}})
		if len(list(A("GET", "/api/approvals/columns", nil))) == 0 || A("GET", "/api/approvals/approvers", nil) == nil || A("GET", "/api/approvals/health", nil) == nil {
			t.Fatal("approver setup lists")
		}
	})

	run("masters", func(t *testing.T) {
		types := A("GET", "/api/masters", nil)
		if len(types) == 0 {
			t.Fatal("master types")
		}
		g := A("POST", "/api/masters/grade", map[string]any{"name": "G7"})
		gid := fmt.Sprint(g["id"])
		no(409, "POST", "/api/masters/grade", "EMP-1002", "HR Admin", map[string]any{"name": "G7"})
		no(400, "POST", "/api/masters/grade", "EMP-1002", "HR Admin", map[string]any{"name": ""})
		A("PATCH", "/api/masters/grade/"+gid, map[string]any{"name": "G7 senior"})
		no(400, "POST", "/api/masters/grade/"+gid+"/inactive", "EMP-1002", "HR Admin", map[string]any{"since": today()})
		A("POST", "/api/masters/grade/"+gid+"/inactive", map[string]any{"since": today(), "reason": "Merged into G6"})
		status := deep3Str(apiDoc(st, "grade", bson.M{"_id": gid})["status"])
		if status != "Inactive" {
			t.Fatal("grade made inactive")
		}
		A("POST", "/api/masters/grade/"+gid+"/reactivate", nil)
		no(403, "POST", "/api/masters/grade", "EMP-1020", "Employee", map[string]any{"name": "G8"})
	})

	run("employees: job change, history, import, files", func(t *testing.T) {
		// A grade alone must be one the designation offers (PPL-048): pick another of its grades.
		e20 := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
		newGrade := ""
		for _, g := range apiDoc(st, "designation", bson.M{"_id": e20["designation_id"]})["grades"].([]any) {
			if g != e20["grade"] {
				newGrade = g.(string)
			}
		}
		if newGrade == "" { // a designation with one grade: offer a second one for the test
			newGrade = map[bool]string{true: "G2", false: "G1"}[e20["grade"] != "G2"]
			if _, err := st.DB.C("designation").Update(context.Background(), bson.M{"_id": e20["designation_id"]}, bson.M{"$push": bson.M{"grades": newGrade}}); err != nil {
				t.Fatal(err)
			}
		}
		hireFixRoleSkill(t, st, "EMP-1020") // a job change needs Role and Skill on the record (hirefix.go)
		r := A("PATCH", "/api/employees/EMP-1020/job", map[string]any{"changes": map[string]any{"grade": newGrade}, "reason": "Promotion", "effectiveFrom": today()})
		if len(r["changes"].([]any)) != 1 {
			t.Fatal(r)
		}
		no(400, "PATCH", "/api/employees/EMP-1020/job", "EMP-1002", "HR Admin", map[string]any{"changes": map[string]any{"grade": "G9"}, "reason": "Promotion", "effectiveFrom": today()})
		h := list(A("GET", "/api/employees/EMP-1020/history", nil))
		if len(h) == 0 {
			t.Fatal("history records the change")
		}
		e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
		loc := deep3Str(apiDoc(st, "location", bson.M{"_id": e["location_id"]})["name"])
		dept := deep3Str(apiDoc(st, "department", bson.M{"_id": e["department_id"]})["name"])
		desig := deep3Str(apiDoc(st, "designation", bson.M{"_id": e["designation_id"]})["name"])
		row := map[string]any{"name": "Imported Person", "type": "Permanent", "doj": today(), "location": loc, "department": dept, "designation": desig, "grade": "G1", "shift": "G",
			"gross": 17000, "mobile": "9876501234", "email": "imported@example.com", "dob": "1995-05-05", "gender": "Female", "pan": "ABCDE1234F", "aadhaar": "123412341234",
			"bankName": "HDFC Bank", "accountNo": "123456789012", "ifsc": "HDFC0001234"}
		chk := A("POST", "/api/employees/import", map[string]any{"rows": []any{row}})
		if fmt.Sprint(chk["failed"]) != "0" {
			t.Fatal(chk)
		}
		A("POST", "/api/employees/import", map[string]any{"rows": []any{row}, "apply": true})
		n := apiCount(st, "employee", bson.M{"name": "Imported Person"})
		if n != 1 {
			t.Fatal("import applied")
		}
		no(403, "POST", "/api/employees/import", "EMP-1004", "HRBP", map[string]any{"rows": []any{row}})
		_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "PAN card")
		if files := ok("GET", "/api/files?ownerKind=employee&ownerId=EMP-1020", "EMP-1020", "Employee", nil); files["PAN card"] == nil {
			t.Fatal("file list")
		}
		if s, raw := c.getRaw("/api/files/"+doc["id"].(string), "EMP-1020", "Employee"); s != 200 || !strings.HasPrefix(string(raw), "%PDF") {
			t.Fatal("file download", s)
		}
		if s, _ := c.getRaw("/api/files/"+doc["id"].(string), "EMP-1009", "Line Manager"); s != 403 {
			t.Fatalf("a line manager cannot open ID copies: %d", s)
		}
		ok("GET", "/api/employees/EMP-1020/documents", "EMP-1002", "HR Admin", nil)
	})

	run("self-service", func(t *testing.T) {
		ok("GET", "/api/lookups", "EMP-1020", "Employee", nil)
		ok("GET", "/api/me/requests?wf=p_claim", "EMP-1020", "Employee", nil)
		no(400, "GET", "/api/me/requests", "EMP-1020", "Employee", nil)
		if sep := ok("GET", "/api/me/separation", "EMP-1020", "Employee", nil); sep["notice"] == nil {
			t.Fatal("notice details")
		}
		no(400, "POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Personal", "mobile": "12"}})
		ok("POST", "/api/approvals", "EMP-1020", "Employee", map[string]any{"wf": "p_change", "payload": map[string]any{"kind": "Personal", "mobile": "9876543210"}})
		ok("GET", "/api/me/discipline", "EMP-1020", "Employee", nil)
		ok("GET", "/api/me/form16", "EMP-1020", "Employee", nil)
	})

	run("onboarding setup, company, nominations", func(t *testing.T) {
		set := A("GET", "/api/onboarding/setup", nil)
		f := set["fields"].([]any)[0].(map[string]any)
		key := fmt.Sprint(f["key"])
		label := "Renamed field"
		A("PATCH", "/api/onboarding/setup/fields/"+key, map[string]any{"label": label})
		added := A("POST", "/api/onboarding/setup/docs", map[string]any{"label": "Police verification", "offer": 0, "join": 1})
		if added == nil {
			t.Fatal("document added")
		}
		no(403, "POST", "/api/onboarding/setup/docs", "EMP-1020", "Employee", map[string]any{"label": "x"})
		ok("GET", "/api/offers", "EMP-1004", "HRBP", nil)
		no(403, "GET", "/api/offers", "EMP-1020", "Employee", nil)
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1029"}, bson.M{"$set": bson.M{"status": "Inactive", "lwd": store.DateOf(time.Now().AddDate(0, 0, -30)), "rehire": "Eligible"}})
		if pre := A("GET", "/api/offers/rejoin/EMP-1029", nil); pre == nil {
			t.Fatal("rejoin prefill")
		}
		co := A("GET", "/api/settings/company", nil)
		co["address"] = "Plot 4, Sector 29, Gurugram"
		A("PUT", "/api/settings/company", co)
		no(403, "PUT", "/api/settings/company", "EMP-1004", "HRBP", co)
		schemes := list(A("GET", "/api/nominations/schemes", nil))
		sc := schemes[0].(map[string]any)
		sc["dueDays"] = 20.0
		A("PUT", fmt.Sprintf("/api/nominations/schemes/%v", sc["id"]), sc)
		no(403, "PUT", fmt.Sprintf("/api/nominations/schemes/%v", sc["id"]), "EMP-1011", "Payroll", sc)
	})

	run("work policies, plans, calendars, exceptions", func(t *testing.T) {
		pols := A("GET", "/api/policies", nil)["list"].([]any)
		p := pols[0].(map[string]any)
		pid := p["id"].(string)
		full := A("GET", "/api/policies/"+pid, nil)["policy"].(map[string]any)
		settings := full["settings"].(map[string]any)
		settings["grace"] = 12.0
		A("PUT", "/api/policies/"+pid, map[string]any{"name": full["name"], "settings": settings, "leavePlanId": full["leavePlanId"], "calendarId": full["calendarId"]})
		settings["fullH"] = -1.0
		no(400, "PUT", "/api/policies/"+pid, "EMP-1002", "HR Admin", map[string]any{"name": full["name"], "settings": settings})
		np := A("POST", "/api/policies", map[string]any{"name": "Night operations", "copyFrom": pid})
		A("PUT", "/api/policies/"+np["id"].(string)+"/maps", map[string]any{"maps": []any{map[string]any{"level": "employee", "ref": "EMP-1020", "on": true}}})
		cov := A("GET", "/api/policies/coverage", nil)
		for _, x := range cov["rows"].([]any) {
			if m := x.(map[string]any); m["id"] == "EMP-1020" && m["policy"] != "Night operations" {
				t.Fatalf("employee map wins: %v", m["policy"])
			}
		}
		plans := A("GET", "/api/leave-plans", nil)
		pl := list(plans)
		if len(pl) == 0 {
			if l, ok := plans["plans"].([]any); ok {
				pl = l
			}
		}
		lp := pl[0].(map[string]any)["plan"].(map[string]any)
		created := A("POST", "/api/leave-plans", map[string]any{"name": "Copy plan", "copyFrom": lp["id"]})
		A("PUT", "/api/leave-plans/"+created["id"].(string), map[string]any{"name": "Copy plan", "description": "edited", "lv": lp["lv"], "lrx": lp["lrx"], "types": lp["types"]})
		A("POST", "/api/leave-types", map[string]any{"name": "Bereavement leave", "kind": "event", "paid": true})
		no(409, "POST", "/api/leave-types", "EMP-1002", "HR Admin", map[string]any{"name": "Bereavement leave", "kind": "event", "paid": true})
		cals := A("GET", "/api/calendars", nil)
		cl := list(cals)
		if len(cl) == 0 {
			cl, _ = cals["calendars"].([]any)
		}
		calID := cl[0].(map[string]any)["id"].(string)
		A("PUT", "/api/calendars/"+calID, map[string]any{"holiday": map[string]any{"date": time.Now().Format("2006") + "-12-31", "name": "Year end", "kind": "Regional"}})
		A("POST", "/api/exceptions", map[string]any{"employeeId": "EMP-1020", "kind": "leave", "from": today(), "to": today(), "until": today(), "reason": "Sweep"})
		exs := list(A("GET", "/api/exceptions", nil))
		if len(exs) == 0 {
			t.Fatal("exceptions listed")
		}
		A("POST", fmt.Sprintf("/api/exceptions/%v/revoke", exs[0].(map[string]any)["id"]), nil)
		no(403, "POST", "/api/exceptions", "EMP-1008", "Property Manager", map[string]any{"employeeId": "EMP-1020", "kind": "leave", "from": today(), "to": today(), "until": today(), "reason": "x"})
	})

	run("attendance and roster views", func(t *testing.T) {
		ym := time.Now().AddDate(0, 0, -1).Format("2006-01")
		ok("GET", "/api/attendance/issues?ym="+ym, "EMP-1008", "Property Manager", nil)
		ok("GET", "/api/attendance/day?date="+time.Now().AddDate(0, 0, -1).Format("2006-01-02"), "EMP-1008", "Property Manager", nil)
		ok("GET", "/api/roster/discipline", "EMP-1008", "Property Manager", nil)
		ok("GET", "/api/consequences", "EMP-1008", "Property Manager", nil)
		// An employee sees only their own missing punches.
		for _, x := range list(ok("GET", "/api/attendance/issues?ym="+ym, "EMP-1020", "Employee", nil)) {
			if x.(map[string]any)["id"] != "EMP-1020" {
				t.Fatal("an employee sees someone else's attendance")
			}
		}
	})

	run("bulk templates", func(t *testing.T) {
		if len(list(A("GET", "/api/bulk", nil))) < 9 {
			t.Fatal("templates")
		}
		if s, _ := c.getRaw("/api/bulk/sal/download?data=1", "EMP-1002", "HR Admin"); s != 200 {
			t.Fatal("current data download", s)
		}
		day := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
		runs := []struct{ tpl, csv string }{
			{"att", "Employee ID,Date,In time,Out time\nEMP-1020," + day + ",06:05,14:10\n"},
			{"roster", "Employee ID,Date,Code,Reason\nEMP-1020," + time.Now().AddDate(0, 0, 10).Format("2006-01-02") + ",E,Employee request\n"},
			{"lvopen", "Employee ID,Leave type,Balance,As at\nEMP-1020,Privilege leave,6," + time.Now().Format("2006") + "-01-01\n"},
			{"bud", "Level,Name,Headcount Budget,Monthly Payroll Budget\nLocation," + "Gurugram – Sector 29 Hub" + ",60,1500000\n"},
			{"sal", "Employee ID,Monthly Gross,Variable Pay %,Variable Payout Cycle,Meal Deduction,Accommodation Deduction,Salary Structure,Structure Effective From,Reason for change,Effective From\nEMP-1026,,,,500,,,,Policy change," + today() + "\n"},
			{"off", "Employee ID,Last Working Day,Reason\nEMP-1030," + time.Now().AddDate(0, 0, 20).Format("2006-01-02") + ",Contract ended\n"},
		}
		for _, x := range runs {
			_, r := c.sendFile("/api/bulk/"+x.tpl+"/apply", "EMP-1002", "HR Admin", x.tpl+".csv", []byte(x.csv))
			if num(r["ok"]) != 1 {
				t.Fatalf("bulk %s: %v", x.tpl, r)
			}
		}
		att := apiDoc(st, "attendance_day", bson.M{"employee_id": "EMP-1020", "date": day})
		in, src := deep3Str(att["in_time"]), deep3Str(att["source"])
		meal := apiFloat(apiDoc(st, "employee_pay", bson.M{"employee_id": "EMP-1026"})["meal"])
		var exit *string
		if v, ok := apiDoc(st, "employee", bson.M{"_id": "EMP-1030"})["exit_status"].(string); ok {
			exit = &v
		}
		if in != "06:05" || src != "MIS upload" || meal != 500 || exit == nil {
			t.Fatalf("bulk effects: in %s src %s meal %v exit %v", in, src, meal, exit)
		}
	})

	run("compensation", func(t *testing.T) {
		comp := A("GET", "/api/comp", nil)
		v2 := A("POST", "/api/comp/structures", map[string]any{"name": "Version 2"})
		A("POST", "/api/comp/structures/"+v2["id"].(string)+"/default", nil)
		def := deep3Str(apiDoc(st, "salary_structure", bson.M{"is_default": true})["name"])
		if def != "Version 2" {
			t.Fatal("default structure")
		}
		A("POST", "/api/comp/structure-map", map[string]any{"structureId": v2["id"], "fromDate": today(), "type": "Retainer"})
		if len(list(A("GET", "/api/comp/structures/"+v2["id"].(string)+"/maps", nil))) == 0 {
			t.Fatal("maps listed")
		}
		band := comp["bands"].([]any)[0].(map[string]any)
		A("PUT", "/api/comp/bands", map[string]any{"cityId": band["city_id"], "grade": band["grade"], "min": 15000, "max": 30000})
		no(400, "PUT", "/api/comp/bands", "EMP-1002", "HR Admin", map[string]any{"cityId": band["city_id"], "grade": band["grade"], "min": 30000, "max": 15000})
		var other string
		for _, b := range comp["bands"].([]any) {
			if m := b.(map[string]any); m["city_id"] != band["city_id"] {
				other = m["city_id"].(string)
			}
		}
		if r := A("POST", "/api/comp/bands/copy", map[string]any{"from": band["city_id"], "to": other, "factor": 1.1}); num(r["copied"]) == 0 {
			t.Fatal("bands copied")
		}
		state := comp["lwf"].([]any)[0].(map[string]any)
		A("PUT", "/api/comp/lwf/"+state["state_id"].(string), map[string]any{"effectiveFrom": today(), "rule": map[string]any{"on": true, "ee": map[string]any{"mode": "fixed", "val": 10}, "er": map[string]any{"mode": "fixed", "val": 20}, "freq": "monthly"}})
		no(400, "PUT", "/api/comp/lwf/"+state["state_id"].(string), "EMP-1002", "HR Admin", map[string]any{"effectiveFrom": today(), "rule": map[string]any{"on": true, "freq": "half", "ee": map[string]any{"mode": "fixed"}, "er": map[string]any{"mode": "fixed"}}})
		loc := comp["budgets"].([]any)[0].(map[string]any)
		A("PUT", "/api/comp/budgets/location/"+loc["id"].(string), map[string]any{"headcount": 40, "payroll": 900000})
		// An HRBP edits only mapped properties.
		var outside string
		if m := apiDoc(st, "user_role_map", bson.M{"employee_id": "EMP-1004", "role_name": "HRBP"}); m != nil {
			outside = deep3Str(apiDoc(st, "location", bson.M{"_id": bson.M{"$nin": m["location_ids"]}})["id"])
		}
		_, _ = st.DB.C("role_right").UpdateMany(ctx, bson.M{"role_name": "HRBP", "right_id": "comp.budget"}, bson.M{"$set": bson.M{"level": 2}})
		no(403, "PUT", "/api/comp/budgets/location/"+outside, "EMP-1004", "HRBP", map[string]any{"headcount": 1})
		ok("GET", "/api/salary/structures", "EMP-1011", "Payroll", nil)
	})

	run("payroll lists, sheets and setup", func(t *testing.T) {
		_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
		open := state["openYm"].(string)
		cl := ok("GET", "/api/payroll/"+open+"/checklist", "EMP-1011", "Payroll", nil)
		// Month close's "send to MIS and all approvers" is the checklist's Message all now.
		if msgs, _ := pcClosure(cl, "attendance")["messages"].([]any); len(msgs) > 0 {
			m := msgs[0].(map[string]any)
			ok("POST", "/api/payroll/"+open+"/closures/attendance/message", "EMP-1011", "Payroll",
				map[string]any{"messages": []any{map[string]any{"to": m["to"], "refs": m["refs"], "text": m["text"]}}})
		}
		for _, k := range []string{"advances", "deductions", "arrears", "claims", "changes"} {
			ok("GET", "/api/pay-items/"+k, "EMP-1011", "Payroll", nil)
		}
		no(404, "GET", "/api/pay-items/bogus", "EMP-1011", "Payroll", nil)
		for _, p := range []string{"/api/payroll/upcoming.xlsx", "/api/payroll/" + open + "/attendance.xlsx"} {
			if s, _ := c.getRaw(p, "EMP-1011", "Payroll"); s != 200 {
				t.Fatalf("%s %d", p, s)
			}
		}
		c.closeAndPublish(t, open)
		if len(list(ok("GET", "/api/payroll/register", "EMP-1011", "Payroll", nil))) != 1 {
			t.Fatal("register")
		}
		if s, _ := c.getRaw("/api/payroll/"+open+"/register.xlsx", "EMP-1012", "Payroll Manager"); s != 200 {
			t.Fatal("register sheet")
		}
		ok("GET", "/api/payslip-setup", "EMP-1011", "Payroll", nil)
		if pv := ok("GET", "/api/payslip-preview", "EMP-1011", "Payroll", nil); pv["snap"] == nil {
			t.Fatal("payslip preview")
		}
		no(403, "PUT", "/api/payslip-setup", "EMP-1020", "Employee", map[string]any{})
	})

	run("tax and form 16 extras", func(t *testing.T) {
		setFirstYm(t, c, "")
		_, me := c.do("GET", "/api/tax/me", "EMP-1020", "Employee", nil)
		w := me["windows"].(map[string]any)
		yes := true
		A("PUT", "/api/tax/windows", map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": yes})
		_, doc := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Rent receipt")
		p := ok("POST", "/api/tax/me/proofs", "EMP-1020", "Employee", map[string]any{"item": "hra", "doc": doc["id"], "amount": 60000})
		ok("POST", fmt.Sprintf("/api/tax/me/proofs/%v/withdraw", p["id"]), "EMP-1020", "Employee", nil)
		no(400, "POST", fmt.Sprintf("/api/tax/me/proofs/%v/withdraw", p["id"]), "EMP-1020", "Employee", nil)
		fy := me["fy"].(string)
		ok("PUT", "/api/tds/return/24Q/1?fy="+fy, "EMP-1012", "Payroll Manager", map[string]any{"receipt": "QRCPT100001"})
		no(400, "PUT", "/api/tds/return/24Q/1?fy="+fy, "EMP-1012", "Payroll Manager", map[string]any{"receipt": "1"})
		ok("PUT", "/api/tds/deductor", "EMP-1012", "Payroll Manager", map[string]any{"name": "Deepak Arora", "desig": "Payroll Manager"})
		ok("POST", "/api/form16/release?fy="+fy, "EMP-1012", "Payroll Manager", nil)
		if len(list(ok("GET", "/api/form16/all?fy="+fy, "EMP-1012", "Payroll Manager", nil))) == 0 {
			t.Fatal("print all Form 16")
		}
		if y := ok("GET", "/api/me/form16", "EMP-1020", "Employee", nil); len(y["years"].([]any)) == 0 {
			t.Fatal("my Form 16 years")
		}
	})

	run("separation list, letters, lost asset, devices", func(t *testing.T) {
		r := ok("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Contract end", "lwd": time.Now().AddDate(0, 0, 5).Format("2006-01-02")})
		if len(ok("GET", "/api/separations", "EMP-1004", "HRBP", nil)["cases"].([]any)) == 0 {
			t.Fatal("separation list")
		}
		ok("POST", "/api/separations/"+r["id"].(string)+"/cancel", "EMP-1004", "HRBP", map[string]any{"note": "Contract extended"})
		r = ok("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1026", "type": "Termination", "reason": "Performance", "noticeOption": "serve", "note": "Did not meet the improvement plan"})
		c.approveAll(t, r["requestId"].(string))
		ok("POST", "/api/separations/"+r["id"].(string)+"/letter", "EMP-1004", "HRBP", map[string]any{"kind": "Termination letter"})
		d := ok("GET", "/api/separations/"+r["id"].(string), "EMP-1004", "HRBP", nil)
		lid := d["letters"].([]any)[0].(map[string]any)["id"]
		l := ok("GET", fmt.Sprintf("/api/separations/%s/letters/%v", r["id"], lid), "EMP-1004", "HRBP", nil)
		if !strings.Contains(l["body"].(string), "terminated") {
			t.Fatal("termination letter text")
		}
		if me := ok("GET", "/api/me/separation", "EMP-1020", "Employee", nil); me["case"] != nil {
			t.Fatal("EMP-1020 has no case")
		}
		inv := A("GET", "/api/inventory", nil)
		var laptop float64
		for _, i := range inv["items"].([]any) {
			if m := i.(map[string]any); m["name"] == "Laptop" {
				laptop = num(m["id"])
			}
		}
		A("POST", "/api/inventory/stock/add", map[string]any{"to": "central", "lines": []any{map[string]any{"itemId": laptop, "serials": []string{"LP-9"}}}})
		aid, _ := apiDoc(st, "asset", bson.M{"serial": "LP-9"})["id"].(int64)
		A("POST", "/api/inventory/issue", map[string]any{"store": "central", "employeeId": "EMP-1020", "assetId": aid})
		A("POST", fmt.Sprintf("/api/inventory/assets/%d/lost", aid), map[string]any{"note": "Stolen from the bus", "recover": 5000})
		ded := apiFloat(apiDoc(st, "ledger", bson.M{"source_id": fmt.Sprintf("AST-%d", aid)})["amount"])
		if ded != 5000 {
			t.Fatalf("lost asset recovery %v", ded)
		}
		dev := A("POST", "/api/devices", map[string]any{"name": "Gate 2"})
		k := A("POST", fmt.Sprintf("/api/devices/%v/key", dev["id"]), nil)
		if k["key"] == dev["key"] {
			t.Fatal("a new key replaces the old one")
		}
		A("POST", fmt.Sprintf("/api/devices/%v/inactive", dev["id"]), map[string]any{"reason": "Replaced"})
		no(403, "POST", "/api/devices", "EMP-1011", "Payroll", map[string]any{"name": "x"})
	})

	run("sign out", func(t *testing.T) {
		ok("POST", "/api/auth/logout", "EMP-1009", "Line Manager", nil)
		no(401, "GET", "/api/me", "EMP-1009", "Line Manager", nil)
	})
}

// TestSeedAndStorage covers the seed's starting logins and sample punches, and the local file store.
func TestSeedAndStorage(t *testing.T) {
	_, _, st := setupFull(t)
	ctx := context.Background()
	if _, err := seed.StartingLogins(ctx, st); err != nil {
		t.Fatal(err)
	}
	n := apiCount(st, "auth_credential", bson.M{"employee_id": bson.M{"$in": bson.A{"EMP-1002", "EMP-1020"}}})
	if n != 2 {
		t.Fatal("starting logins")
	}
	if _, err := seed.SampleAttendance(ctx, st); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fs := storage.Local{Root: dir}
	if err := fs.Put(ctx, "a/b.pdf", bytes.NewReader([]byte("%PDF-1"))); err != nil {
		t.Fatal(err)
	}
	rc, err := fs.Open(ctx, "a/b.pdf")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	_, _ = rc.Read(buf)
	rc.Close()
	if string(buf) != "%PDF-1" {
		t.Fatal(string(buf))
	}
	if err := fs.Put(ctx, "../escape", bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("paths cannot leave the root")
	}
}
