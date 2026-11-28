package api_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Round 2, onboarding and compensation (gaps B 1-9) and access, reports and bulk (gaps G, A's import).

// Rules by employment type are stored and the payroll engine reads them; versions switch off and on;
// the active count per version; the offer template.
func TestUI2CompStructureAndRules(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1001")

	_, comp := c.do("GET", "/api/comp", "EMP-1002", "HR Admin", nil)
	types := comp["types"].(map[string]any)
	if types["Contractual"].(map[string]any)["statutory"] != false || types["Permanent"].(map[string]any)["ded"] != "all" {
		t.Fatalf("stored rules start as the old constants: %v", types)
	}
	v1 := comp["structures"].([]any)[0].(map[string]any)
	active := float64(apiCount(st, "employee", bson.M{"status": "Active"}))
	if v1["active"] != active {
		t.Fatalf("every active employee is on the default version: %v of %v", v1["active"], active)
	}

	t.Run("rules by employment type", func(t *testing.T) {
		on := map[string]any{"statutory": true, "lwf": false, "bonus": true, "grat": false, "plEnc": false, "variable": true, "ded": "none", "leave": false, "docs": false}
		if s, _ := c.do("PUT", "/api/comp/types/Contractual", "EMP-1004", "HRBP", on); s != 403 {
			t.Fatal("only comp.structure Edit changes the rules")
		}
		bad := map[string]any{"statutory": true, "ded": "some"}
		if s, _ := c.do("PUT", "/api/comp/types/Contractual", "EMP-1002", "HR Admin", bad); s != 400 {
			t.Fatal("deductions are all, tds or none")
		}
		if s, _ := c.do("PUT", "/api/comp/types/Intern", "EMP-1002", "HR Admin", on); s != 404 {
			t.Fatal("unknown type")
		}
		_, open := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
		ym := open["openYm"].(string)
		before := c.row(t, ym, "EMP-1014") // contractual
		if num(before["r"].(map[string]any)["eePF"]) != 0 {
			t.Fatal("no PF for contractual staff by default")
		}
		s, r := c.do("PUT", "/api/comp/types/Contractual", "EMP-1002", "HR Admin", on)
		if s != 200 || r["types"].(map[string]any)["Contractual"].(map[string]any)["statutory"] != true {
			t.Fatal(r)
		}
		var stored string // the stored rule as text
		if rules, _ := apiDoc(st, "comp_type_rule", bson.M{"type": "Contractual"})["rules"].(map[string]any); rules["statutory"] != nil {
			stored = fmt.Sprint(rules["statutory"])
		}
		if stored != "true" {
			t.Fatal("rules are stored")
		}
		after := c.row(t, ym, "EMP-1014")
		if num(after["r"].(map[string]any)["eePF"]) <= 0 {
			t.Fatalf("the payroll engine reads the stored rules: %v", after["r"])
		}
		_, pv := c.do("POST", "/api/salary/preview", "EMP-1002", "HR Admin", map[string]any{"type": "Contractual", "gross": 15000})
		if num(pv["breakup"].(map[string]any)["eePF"]) <= 0 || pv["rules"].(map[string]any)["statutory"] != true {
			t.Fatalf("salary preview uses the stored rules: %v", pv)
		}
		hist := apiCount(st, "config_history", bson.M{"entity": "comp_type_rule", "entity_id": "Contractual"})
		if hist != 1 {
			t.Fatal("the change is in the history")
		}
		on["statutory"] = false
		if s, _ := c.do("PUT", "/api/comp/types/Contractual", "EMP-1002", "HR Admin", on); s != 200 {
			t.Fatal("back to the default")
		}
	})

	t.Run("switch a version off and on", func(t *testing.T) {
		id := v1["id"].(string)
		if s, r := c.do("POST", "/api/comp/structures/"+id+"/status", "EMP-1002", "HR Admin", map[string]any{"on": false}); s != 400 || !strings.Contains(r["error"].(string), "default") {
			t.Fatal("the default version stays on", r)
		}
		_, r := c.do("POST", "/api/comp/structures", "EMP-1002", "HR Admin", map[string]any{"name": "Version 2", "copyFrom": id})
		v2 := r["id"].(string)
		if s, _ := c.do("POST", "/api/comp/structures/"+v2+"/status", "EMP-1004", "HRBP", map[string]any{"on": false}); s != 403 {
			t.Fatal("needs comp.structure Edit")
		}
		if s, r := c.do("POST", "/api/comp/structures/"+v2+"/status", "EMP-1002", "HR Admin", map[string]any{"on": false}); s != 200 || r["status"] != "Inactive" {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/comp/structures/"+v2+"/status", "EMP-1002", "HR Admin", map[string]any{"on": true}); s != 200 || r["status"] != "Active" {
			t.Fatal(r)
		}
		if s, r := c.do("POST", "/api/comp/structure-map", "EMP-1002", "HR Admin", map[string]any{"structureId": v2, "fromDate": today(), "ids": []string{"EMP-1020"}}); s != 200 {
			t.Fatal(r)
		}
		_, comp := c.do("GET", "/api/comp", "EMP-1002", "HR Admin", nil)
		for _, x := range comp["structures"].([]any) {
			m := x.(map[string]any)
			if m["id"] == v2 && m["active"] != 1.0 {
				t.Fatalf("1 active employee on Version 2: %v", m)
			}
		}
		s, r := c.do("POST", "/api/comp/structures/"+v2+"/status", "EMP-1002", "HR Admin", map[string]any{"on": false})
		if s != 400 || r["error"] != "1 employees are still on Version 2. Map them to another version first" {
			t.Fatal(r)
		}
	})

	t.Run("offer letter template", func(t *testing.T) {
		tpl := "Dear {{name}}, probation {{probation}}."
		if s, _ := c.do("PUT", "/api/comp/offer-template", "EMP-1004", "HRBP", map[string]string{"template": tpl}); s != 403 {
			t.Fatal("needs comp.structure Edit")
		}
		if s, _ := c.do("PUT", "/api/comp/offer-template", "EMP-1002", "HR Admin", map[string]string{"template": "  "}); s != 400 {
			t.Fatal("cannot be empty")
		}
		if s, r := c.do("PUT", "/api/comp/offer-template", "EMP-1002", "HR Admin", map[string]string{"template": tpl}); s != 200 {
			t.Fatal(r)
		}
		if _, co := c.do("GET", "/api/settings/company", "EMP-1001", "CHRO", nil); co["offerTemplate"] != tpl {
			t.Fatal(co)
		}
	})
}

// The work policy and LWF note on the offer check, documents asked from the candidate, the
// {{probation}} merge field, offers readable for the letter preview, and the prototype's rights.
func TestUI2CompOffers(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1001", "EMP-1014", "EMP-1003", "EMP-1013")
	var city, hub string
	if l := apiDoc(st, "location", bson.M{"name": "Gurugram – Sector 29 Hub"}); l != nil {
		if k := apiDoc(st, "cluster", bson.M{"_id": l["cluster_id"]}); k != nil {
			hub, _ = l["id"].(string)
			city, _ = k["city_id"].(string)
		}
	}
	data := map[string]any{"first": "Riya", "last": "Sharma", "type": "Permanent", "doj": today(), "dept": "Front Office", "desig": "Front Office Supervisor",
		"grade": "G3", "city": city, "loc": hub, "gross": 26000, "phone": "9876501234", "email": "riya.sharma@example.com", "jobRole": "Front desk", "jobSkill": "Guest service",
		"pfWant": "Yes"}
	draft := "DRAFT-ui2comp-0001"

	_, chk := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": data, "draft": draft})
	pol, _ := chk["policy"].(map[string]any)
	if pol == nil || pol["name"] == "" || pol["via"] == "" || pol["probation"] != 6.0 || pol["weekOff"] == "" || !strings.HasPrefix(pol["pl"].(string), "PL ") {
		t.Fatalf("policy %v", chk["policy"])
	}
	if chk["lwfNote"] == nil || chk["rules"].(map[string]any)["statutory"] != true {
		t.Fatalf("LWF note and type rules: %v %v", chk["lwfNote"], chk["rules"])
	}
	// Retainers cannot apply for leave, so PL does not apply to them.
	ret := map[string]any{}
	for k, v := range data {
		ret[k] = v
	}
	ret["type"] = "Retainer"
	if _, r := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": ret}); r["policy"].(map[string]any)["pl"] != "PL not applicable" {
		t.Fatal(r["policy"])
	}

	t.Run("ask candidate", func(t *testing.T) {
		base := "/api/offers/draft/" + draft + "/docs/"
		if s, _ := c.do("POST", base+"photo/request", "EMP-1014", "Employee", nil); s != 403 {
			t.Fatal("onboarding roles only")
		}
		if s, _ := c.do("POST", base+"nosuchdoc/request", "EMP-1004", "HRBP", nil); s != 400 {
			t.Fatal("unknown document")
		}
		if s, _ := c.do("POST", "/api/offers/draft/OFR-1/docs/photo/request", "EMP-1004", "HRBP", nil); s != 400 {
			t.Fatal("drafts only")
		}
		if s, r := c.do("POST", base+"photo/request", "EMP-1004", "HRBP", nil); s != 200 || r["asked"].(map[string]any)["photo"] != true {
			t.Fatal(r)
		}
		if s, _ := c.do("POST", base+"photo/request", "EMP-1002", "HR Admin", nil); s != 403 {
			t.Fatal("a draft belongs to the person who started it")
		}
		c.do("POST", base+"pan/request", "EMP-1004", "HRBP", nil)
		if s, r := c.do("POST", base+"pan/cancel", "EMP-1004", "HRBP", nil); s != 200 || r["asked"].(map[string]any)["pan"] != nil {
			t.Fatal(r)
		}
		_, r := c.do("POST", "/api/offers/check", "EMP-1004", "HRBP", map[string]any{"data": data, "draft": draft})
		if a := r["asked"].(map[string]any); a["photo"] != true || len(a) != 1 {
			t.Fatalf("the check returns what was asked: %v", a)
		}
	})

	// An offer is raised for a selected candidate; what is asked from them stays with the candidate (SI-…).
	c.signIn("EMP-1009")
	si := selectCandidate(t, c, shortlistBody(st, "Riya", "Sharma", "9876501234"))
	if s, r := c.do("POST", "/api/offers/draft/"+si+"/docs/photo/request", "EMP-1004", "HRBP", nil); s != 200 || r["asked"].(map[string]any)["photo"] != true {
		t.Fatal(r)
	}
	if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, "aadhaar"); s != 200 {
		t.Fatal(r)
	}
	s, r := c.do("POST", "/api/offers", "EMP-1004", "HRBP", map[string]any{"data": data, "candidate": si})
	if s != 200 {
		t.Fatal(r)
	}
	offer := r["id"].(string)
	_, o := c.do("GET", "/api/offers/"+offer, "EMP-1004", "HRBP", nil)
	if o["asked"].(map[string]any)["photo"] != true {
		t.Fatalf("the request is on the offer: %v", o["asked"])
	}
	if !strings.Contains(o["letter"].(string), "probation of 6 months") {
		t.Fatalf("{{probation}} comes from the work policy: %s", o["letter"])
	}

	t.Run("rights as in the prototype", func(t *testing.T) {
		_, roles := c.do("GET", "/api/roles", "EMP-1002", "HR Admin", nil)
		want := map[string]map[string]float64{"HRBP Corporate": {"comp.structure": 1}, "CHRO": {"comp.structure": 1, "comp.budget": 1}, "Finance Head": {"comp.structure": 1, "comp.budget": 1}}
		for _, x := range roles["list"].([]any) {
			m := x.(map[string]any)
			for k, v := range want[m["name"].(string)] {
				if m["rights"].(map[string]any)[k] != v {
					t.Fatalf("%s %s = %v", m["name"], k, m["rights"].(map[string]any)[k])
				}
			}
		}
	})

	t.Run("offers readable for the letter preview", func(t *testing.T) {
		if s, l := c.do("GET", "/api/offers", "EMP-1001", "CHRO", nil); s != 200 || len(l["list"].([]any)) != 1 {
			t.Fatal("CHRO reads the offers with comp.structure", s, l)
		}
		if s, _ := c.do("GET", "/api/offers", "EMP-1014", "Employee", nil); s != 403 {
			t.Fatal("employees do not")
		}
		// A role whose only way in is comp.structure sees the letter, not the candidate's personal details.
		if s, r := c.do("PUT", "/api/roles/Finance Head/rights", "EMP-1002", "HR Admin", map[string]any{"rights": map[string]int{"appr.queue": 0}}); s != 200 {
			t.Fatal(r)
		}
		s, p := c.do("GET", "/api/offers/"+offer, "EMP-1013", "Finance Head", nil)
		if s != 200 || !strings.Contains(p["letter"].(string), "Riya Sharma") || p["data"] != nil || p["docs"] != nil {
			t.Fatalf("preview only: %d %v", s, p)
		}
	})

	t.Run("migration raises a right only where the owner never set it", func(t *testing.T) {
		level := func(role, right string) (n int) {
			if lv, ok := apiDoc(st, "role_right", bson.M{"role_name": role, "right_id": right})["level"].(int16); ok {
				n = int(lv)
			}
			return
		}
		rights := st.DB.C("role_right")
		_, _ = rights.UpdateMany(ctx, bson.M{"role_name": "CHRO", "right_id": bson.M{"$in": bson.A{"comp.structure", "comp.budget"}}}, bson.M{"$set": bson.M{"level": 0}})
		_, _ = rights.DeleteMany(ctx, bson.M{"role_name": "HRBP Corporate", "right_id": "comp.structure"})
		// The owner set CHRO comp.budget to 0 in Access control.
		if s, r := c.do("PUT", "/api/roles/CHRO/rights", "EMP-1002", "HR Admin", map[string]any{"rights": map[string]int{"comp.budget": 0}}); s != 200 {
			t.Fatal(r)
		}
		_, _ = rights.UpdateMany(ctx, bson.M{"role_name": "Finance Head", "right_id": "comp.structure"}, bson.M{"$set": bson.M{"level": 2}})
		if err := ui2CompRaiseRights(ctx, st); err != nil {
			t.Fatal(err)
		}
		if level("CHRO", "comp.structure") != 1 || level("HRBP Corporate", "comp.structure") != 1 {
			t.Fatal("a seeded 0 and a missing row are raised")
		}
		if level("CHRO", "comp.budget") != 0 || level("Finance Head", "comp.structure") != 2 {
			t.Fatal("a level the owner set is never lowered or overwritten")
		}
	})
}

// Role mapping (email, city, role on edit), reports (location grouping, On notice, own filters, holds
// and deductions with scope only, CSV) and the Import employees bulk template.
func TestUI2CompAccessReportsBulk(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1008", "EMP-1011", "EMP-1012")

	t.Run("role mapping", func(t *testing.T) {
		_, l := c.do("GET", "/api/role-map", "EMP-1002", "HR Admin", nil)
		var hrbpc, chro map[string]any
		for _, x := range l["list"].([]any) {
			m := x.(map[string]any)
			if _, ok := m["email"]; !ok || m["city"] == nil {
				t.Fatalf("email and city: %v", m)
			}
			switch m["role_name"] {
			case "HRBP Corporate":
				hrbpc = m
			case "CHRO":
				chro = m
			}
		}
		id := hrbpc["id"].(string)
		if s, _ := c.do("PATCH", "/api/role-map/"+id, "EMP-1002", "HR Admin", map[string]any{"role": "Employee"}); s != 400 {
			t.Fatal("Employee is not assignable")
		}
		if s, r := c.do("PATCH", "/api/role-map/"+id, "EMP-1002", "HR Admin", map[string]any{"role": "CHRO"}); s != 400 || !strings.Contains(r["error"].(string), "Overlap") {
			t.Fatal("one CHRO for the company", r)
		}
		if s, r := c.do("PATCH", "/api/role-map/"+chro["id"].(string), "EMP-1002", "HR Admin", map[string]any{"role": "HRBP Corporate"}); s != 400 || !strings.Contains(r["error"].(string), "Overlap") {
			t.Fatal("HRBP Corporate places are held", r)
		}
		if s, r := c.do("PATCH", "/api/role-map/"+id, "EMP-1002", "HR Admin", map[string]any{"role": "MIS Executive"}); s != 200 || r["role_name"] != "MIS Executive" {
			t.Fatal(r)
		}
	})

	t.Run("reports", func(t *testing.T) {
		_, hc := c.do("GET", "/api/reports/headcount?group=location", "EMP-1002", "HR Admin", nil)
		cols := hc["cols"].([]any)
		if strings.Join(toStrs(cols), "|") != "Group|Active|Pre-boarding|On notice (in active)|Inactive|Total" {
			t.Fatal(cols)
		}
		locIDs, _ := st.DB.C("employee").Distinct(ctx, "location_id", bson.M{"location_id": bson.M{"$ne": nil}})
		locs := len(locIDs)
		if len(hc["rows"].([]any)) != locs {
			t.Fatal("one row per location")
		}
		_, _ = st.DB.C("employee").Update(ctx, bson.M{"_id": "EMP-1020"}, bson.M{"$set": bson.M{"exit_status": "On notice",
			"lwd": time.Now().In(store.Loc()).AddDate(0, 0, 20).Format("2006-01-02")}}) // 20 days from today
		_, n := c.do("GET", "/api/reports/emp_master?status=On%20notice", "EMP-1002", "HR Admin", nil)
		if rows := n["rows"].([]any); len(rows) != 1 || rows[0].([]any)[0] != "EMP-1020" {
			t.Fatal(rows)
		}
		_, cost := c.do("GET", "/api/reports/cost?group=location", "EMP-1011", "Payroll", nil)
		// The prototype's payroll cost: one Group column whatever the grouping, rows by location.
		if cost["cols"].([]any)[0] != "Group" || cost["cols"].([]any)[1] != "People paid" {
			t.Fatal(cost)
		}
		for _, r := range cost["rows"].([]any) {
			if n := apiCount(st, "location", bson.M{"name": r.([]any)[0]}); n != 1 {
				t.Fatalf("grouped by location: %v", r)
			}
		}

		// Own filters: listed with their options, and applied.
		_, list := c.do("GET", "/api/reports", "EMP-1002", "HR Admin", nil)
		extra := map[string]int{}
		for _, x := range list["reports"].([]any) {
			m := x.(map[string]any)
			extra[m["id"].(string)] = len(m["extra"].([]any))
		}
		if extra["approvals"] != 2 || extra["deviations"] != 1 || extra["consequences"] != 1 || extra["nominations"] != 2 || extra["changes"] != 1 || extra["headcount"] != 0 {
			t.Fatal(extra)
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 900, "instalments": 1, "dedType": "Cash shortage", "note": "Till short", "heard": "Yes"}}); s != 200 {
			t.Fatal(r)
		}
		from := "from=2020-01-01&to=" + today()
		_, ap := c.do("GET", "/api/reports/approvals?wf=p_ded&rs=Pending&"+from, "EMP-1002", "HR Admin", nil)
		rows := ap["rows"].([]any)
		if len(rows) != 1 || rows[0].([]any)[1] != "Salary deduction" || rows[0].([]any)[5] != "Pending" {
			t.Fatal(ap)
		}
		if _, none := c.do("GET", "/api/reports/approvals?wf=p_ded&rs=Rejected&"+from, "EMP-1002", "HR Admin", nil); len(none["rows"].([]any)) != 0 {
			t.Fatal("status filter")
		}
		if _, nm := c.do("GET", "/api/reports/nominations?rs=Verified", "EMP-1002", "HR Admin", nil); len(nm["rows"].([]any)) != 0 {
			t.Fatal("no nomination is verified yet")
		}
		for _, q := range []string{"deviations?ds=OK&" + from, "consequences?ds=PIP&" + from, "changes?ds=Scheduled&" + from} {
			if s, r := c.do("GET", "/api/reports/"+q, "EMP-1002", "HR Admin", nil); s != 200 {
				t.Fatal(q, r)
			}
		}

		// Salary holds and manual deductions: any reports role, within its scope; amounts hidden without salary rights.
		if s, r := c.do("GET", "/api/reports/holds", "EMP-1008", "Property Manager", nil); s != 200 {
			t.Fatal(r)
		}
		_, d := c.do("GET", "/api/reports/deductions?"+from, "EMP-1008", "Property Manager", nil)
		if rows := d["rows"].([]any); len(rows) != 1 || rows[0].([]any)[4] != nil {
			t.Fatalf("in scope, amount hidden: %v", d)
		}
		if _, d := c.do("GET", "/api/reports/deductions?"+from, "EMP-1004", "HRBP", nil); len(d["rows"].([]any)) != 1 {
			t.Fatal("HRBP sees it")
		}
		s, csv := c.getRaw("/api/reports/headcount/sheet?group=location&format=csv", "EMP-1002", "HR Admin")
		if s != 200 || !bytes.HasPrefix(csv, []byte("\xef\xbb\xbfGroup,Active,Pre-boarding,On notice (in active),Inactive,Total\n")) {
			t.Fatalf("csv %d %q", s, csv)
		}
		if s, x := c.getRaw("/api/reports/headcount/sheet", "EMP-1002", "HR Admin"); s != 200 || !bytes.HasPrefix(x, []byte("PK")) {
			t.Fatal("Excel stays the default")
		}
	})

	t.Run("import employees", func(t *testing.T) {
		head := "Employee Code,Full Name,Employment Type,Date of Joining,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Monthly Gross,Mobile,Email,Date of Birth,Gender,PAN,Aadhaar,UAN,ESIC No,Bank Name,Account Number,IFSC,Status,Last Working Day,Opening CL Balance,Opening PL Balance,Opening SL Balance"
		file := head + "\n" +
			"SLT-00417,Ravi Kumar,Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,24500,9810011223,,,Male,ABCDE1234Z,,,,HDFC Bank,50100234567891,HDFC0001234,Active,,3,10,\n" +
			",Neha Singh,Permanent,03/02/2025,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,Ravi Kumar,,,,24500,,,,Female,,,,,,,,,,,,\n" +
			"slt-00417,Sunil Das,Permanent,2023-06-12,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,Unknown Person,,,,24500,,,,,ABCDE1234Z,,,,,,,,,,,\n" +
			",Old Hand,Retainer,2022-01-01,Gurugram,Gurugram – Sector 29 Hub,Front Office,Front Office Supervisor,G3,G,EMP-1009,,,,30000,,,,,,,,,,,,Inactive,2024-03-31,,,\n" +
			",Bad Row,Intern,2022-13-01,Atlantis,Nowhere,Front Office,Chef,G9,Z,,,No such policy,,0,123,x@,,,,12,,,,,,Gone,,-1,,\n"
		file = withRoleSkill(file) // Role and Skill are required (hirefix.go)
		_, l := c.do("GET", "/api/bulk", "EMP-1004", "HRBP", nil)
		for _, x := range l["list"].([]any) {
			if x.(map[string]any)["key"] == "new" {
				t.Fatal("HR Admin only")
			}
		}
		if s, _ := c.sendFile("/api/bulk/new/check", "EMP-1004", "HRBP", "new.csv", []byte(file)); s != 403 {
			t.Fatal("HR Admin only")
		}
		s, r := c.sendFile("/api/bulk/new/check", "EMP-1002", "HR Admin", "new.csv", []byte(file))
		if s != 200 || r["ok"] != 3.0 {
			t.Fatal(s, r)
		}
		res := r["results"].([]any)
		errs := func(i int) string { return strings.Join(toStrs(res[i].(map[string]any)["errors"].([]any)), "; ") }
		if res[1].(map[string]any)["id"] != "(system code)" || errs(1) != "" {
			t.Fatal("a manager added earlier in the file is found by name", res[1])
		}
		if e := errs(2); !strings.Contains(e, "appears twice in the file") || !strings.Contains(e, "“Unknown Person” unknown") || !strings.Contains(e, "PAN ABCDE1234Z is already used") {
			t.Fatal(e)
		}
		for _, want := range []string{"Employment Type “Intern” not in master", "Date of Joining must be like", "Work City “Atlantis” not in master", "Reporting Location “Nowhere” not in master",
			"Designation “Chef” does not exist under Front Office", "Grade “G9” not in master", "Shift “Z” not in master", "Reporting Manager L1 is required", "Work Policy “No such policy” not found",
			"Mobile must be 10 digits", "Email is not valid", "Aadhaar must be 12 digits", "Status must be Active or Inactive", "Opening CL Balance must be a number"} {
			if !strings.Contains(errs(4), want) {
				t.Fatalf("missing %q in %s", want, errs(4))
			}
		}
		if n := apiCount(st, "employee", bson.M{"name": bson.M{"$in": bson.A{"Ravi Kumar", "Neha Singh", "Old Hand"}}}); n != 0 {
			t.Fatal("Check creates nothing")
		}
		s, r = c.sendFile("/api/bulk/new/apply", "EMP-1002", "HR Admin", "new.csv", []byte(file))
		if s != 200 || r["ok"] != 3.0 {
			t.Fatal(r)
		}
		var status, mgr, l2, lwd string
		var cl, pl float64
		var maps int
		if e := apiDoc(st, "employee", bson.M{"_id": "SLT-00417"}); e != nil {
			status, _ = e["status"].(string)
			mgr, _ = e["manager_id"].(string)
			l2, _ = e["functional_manager_id"].(string)
			maps = apiCount(st, "structure_map", bson.M{"employee_id": "SLT-00417"})
			cl = apiFloat(apiDoc(st, "leave_opening", bson.M{"employee_id": "SLT-00417", "leave_type": "Casual leave"})["balance"])
			pl = apiFloat(apiDoc(st, "leave_opening", bson.M{"employee_id": "SLT-00417", "leave_type": "Privilege leave"})["balance"])
		}
		if status != "Active" || mgr != "EMP-1009" || l2 != "EMP-1009" || maps != 1 || cl != 3 || pl != 10 {
			t.Fatal(status, mgr, l2, maps, cl, pl)
		}
		neha := apiDoc(st, "employee", bson.M{"name": "Neha Singh"})
		mgr, _ = neha["manager_id"].(string)
		lwd, _ = neha["doj"].(string)
		if mgr != "SLT-00417" || lwd != "2025-02-03" {
			t.Fatal("manager by name and a dd/mm/yyyy date", mgr, lwd)
		}
		old := apiDoc(st, "employee", bson.M{"name": "Old Hand"})
		status, _ = old["status"].(string)
		lwd, _ = old["lwd"].(string)
		if status != "Inactive" || lwd != "2024-03-31" {
			t.Fatal(status, lwd)
		}
	})
}

func toStrs(l []any) []string {
	out := []string{}
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// ui2CompRaiseRights applies what migration 015_ui_comp.sql did to role_right (the SQL migrations became the
// store's reference data, so the rule is applied here): the prototype's rights are added where the row is missing
// (for roles that exist) and raised from 0 only where the owner never set that right in Access control (no
// "Changed role rights" audit entry naming it), so a level the owner set is never lowered or overwritten.
func ui2CompRaiseRights(ctx context.Context, st *store.Store) error {
	want := []struct {
		role, right string
		level       int
	}{{"HRBP Corporate", "comp.structure", 1}, {"CHRO", "comp.structure", 1}, {"CHRO", "comp.budget", 1},
		{"Finance Head", "comp.structure", 1}, {"Finance Head", "comp.budget", 1}}
	return st.Tx(ctx, func(tx store.Q) error {
		for _, w := range want {
			if ok, err := tx.C("role").Exists(ctx, bson.M{"name": w.role}); err != nil || !ok { // only roles that exist
				if err != nil {
					return err
				}
				continue
			}
			key := bson.M{"role_name": w.role, "right_id": w.right}
			cur, err := tx.C("role_right").FindOne(ctx, key)
			if err != nil {
				return err
			}
			if cur == nil {
				if _, err := tx.C("role_right").Insert(ctx, bson.M{"role_name": w.role, "right_id": w.right, "level": w.level}); err != nil {
					return err
				}
				continue
			}
			if lv, _ := cur["level"].(int16); lv != 0 {
				continue
			}
			changes, err := tx.C("audit_log").Find(ctx, bson.M{"action": "Changed role rights", "entity": "role", "entity_id": w.role})
			if err != nil {
				return err
			}
			set := false // an owner's change named this right
			for _, a := range changes {
				after, _ := a["after"].(map[string]any)
				if r, _ := after["rights"].(map[string]any); r[w.right] != nil {
					set = true
				}
			}
			if set {
				continue
			}
			if _, err := tx.C("role_right").Update(ctx, key, bson.M{"$set": bson.M{"level": w.level}}); err != nil {
				return err
			}
		}
		return nil
	})
}
