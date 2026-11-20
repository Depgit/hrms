package api_test

// Round 3 payroll gaps (prototype): statutory file columns (with the ECR upload text unchanged),
// the place on Form 16, the proof's file name and time, master order for hold reasons, deduction
// types, masters, calendars and leave plans, the prototype's arrear reasons, improvement plans
// waiting on Upcoming pay lines, and assets added to the central store by Payroll.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

func u3strs(v any) []string {
	out := []string{}
	for _, x := range u7list(v) {
		if m, ok := x.(map[string]any); ok {
			out = append(out, fmt.Sprint(m["name"]))
		} else {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

func u3eq(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

func u3cols(t *testing.T, r map[string]any, want ...string) {
	t.Helper()
	if got := u3strs(r["cols"]); !u3eq(got, want) {
		t.Fatalf("columns\n got %q\nwant %q", got, want)
	}
	for _, x := range u7list(r["rows"]) {
		if n := len(u7list(u7map(x)["cells"])); n != len(want) {
			t.Fatalf("a row has %d cells for %d columns: %v", n, len(want), x)
		}
	}
}

// u3cells is the cells of the row with this action for this employee.
func u3cells(r map[string]any, action, id string) []any {
	for _, x := range u7list(r["rows"]) {
		cells := u7list(u7map(x)["cells"])
		if len(cells) > 1 && cells[0] == action && cells[1] == id {
			return cells
		}
	}
	return nil
}

func TestUI3PayrollStatutory(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1001", "EMP-1002", "EMP-1011", "EMP-1012")
	setFirstYm(t, c, "")
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Government ids made from the employee number ("EMP-1020" gives UAN 100100001020, ESIC 3100001020, Aadhaar 4321 8765 1020).
	for _, id := range []string{"EMP-1020", "EMP-1002", "EMP-1026"} {
		n := id[4:]
		must(st.DB.C("employee_gov_id").Update(ctx, bson.M{"employee_id": id}, bson.M{"$set": bson.M{"uan": "10010000" + n, "esic": "310000" + n, "aadhaar": "4321 8765 " + n}}))
	}
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.closeAndPublish(t, open)

	t.Run("range files: columns and cells as the prototype builds them", func(t *testing.T) {
		from, to := time.Now().AddDate(0, 0, -7).Format("2006-01-02"), today()
		joined := store.DateOf(time.Now().AddDate(0, 0, -2)) // two days ago
		joiners := bson.M{"$in": bson.A{"EMP-1003", "EMP-1026"}}
		must(st.DB.C("employee").UpdateMany(ctx, bson.M{"_id": joiners}, bson.M{"$set": bson.M{"doj": joined}}))
		must(st.DB.C("gross_history").UpdateMany(ctx, bson.M{"employee_id": joiners}, bson.M{"$set": bson.M{"from_month": joined[:7]}}))
		must(st.DB.C("gross_history").UpdateMany(ctx, bson.M{"employee_id": "EMP-1026"}, bson.M{"$set": bson.M{"gross": 18000}}))
		personal := bson.M{"father_spouse": "Ramesh Kumar", "marital": "Married", "mobile": "9876500026", "email": "e1026@example.com"}
		must(st.DB.C("employee_personal").Upsert(ctx, bson.M{"employee_id": "EMP-1026"}, personal, nil))
		must(st.DB.C("employee_personal").UpdateMany(ctx, bson.M{"employee_id": "EMP-1003"}, bson.M{"$set": bson.M{"father_spouse": nil, "marital": nil}}))
		// A joiner with no UAN yet (the sample staff have one since 14 Nov 2026): "New UAN to be generated".
		must(st.DB.C("employee_gov_id").Update(ctx, bson.M{"employee_id": "EMP-1003"}, bson.M{"$set": bson.M{"uan": nil}}))
		must(st.DB.C("employee_bank").Insert(ctx, bson.M{"employee_id": "EMP-1026", "from_month": joined[:7], "bank_name": "State Bank of India",
			"account_no": "30011026", "ifsc": "SBIN0000001"}))
		if s, r := c.do("POST", "/api/employees/EMP-1005/inactive", "EMP-1002", "HR Admin", map[string]string{"lwd": time.Now().AddDate(0, 0, -1).Format("2006-01-02"), "reason": "Resigned"}); s != 200 {
			t.Fatal(r)
		}
		q := "?from=" + from + "&to=" + to

		_, ins := c.do("GET", "/api/statutory/insurance"+q, "EMP-1012", "Payroll Manager", nil)
		u3cols(t, ins, "Action", "Employee code", "Name", "Date of birth", "Gender", "Cover type", "Cover start (joining date)", "Cover end (last working day)", "Mobile", "Email", "City", "Department")
		if add := u3cells(ins, "Add", "EMP-1003"); add == nil || add[6] != time.Now().AddDate(0, 0, -2).Format("2006-01-02") || add[7] != "" {
			t.Fatalf("insurance add: cover starts on joining, no end: %v", add)
		}
		if rm := u3cells(ins, "Remove", "EMP-1005"); rm == nil || rm[7] != time.Now().AddDate(0, 0, -1).Format("2006-01-02") {
			t.Fatalf("insurance removal ends on the last working day: %v", rm)
		}

		_, pf := c.do("GET", "/api/statutory/pf"+q, "EMP-1012", "Payroll Manager", nil)
		u3cols(t, pf, "Action", "Employee code", "Name", "Father or spouse name", "Date of birth", "Gender", "Date of joining", "UAN", "UAN status", "Aadhaar", "PAN",
			"Bank account", "IFSC", "EPF wages", "Date of exit", "Reason for exit", "Mobile", "Email", "City")
		en := u3cells(pf, "Enrol", "EMP-1026")
		if en == nil || en[3] != "Ramesh Kumar" || en[7] != "100100001026" || en[8] != "Existing UAN, link to establishment" || en[9] != "4321 8765 1026" ||
			en[11] != "30011026" || en[12] != "SBIN0000001" || num(en[13]) <= 0 || en[14] != "" || en[15] != "" || en[16] != "9876500026" || en[17] != "e1026@example.com" || en[18] == "" {
			t.Fatalf("PF enrolment row: %v", en)
		}
		if en3 := u3cells(pf, "Enrol", "EMP-1003"); en3 == nil || en3[3] != "To collect" || en3[8] != "New UAN to be generated" {
			t.Fatalf("PF: father or spouse to collect, new UAN: %v", en3)
		}
		if ex := u3cells(pf, "Exit", "EMP-1005"); ex == nil || ex[14] != time.Now().AddDate(0, 0, -1).Format("2006-01-02") || ex[15] != "1 - Cessation of service" {
			t.Fatalf("PF exit: %v", ex)
		}

		_, esi := c.do("GET", "/api/statutory/esi"+q, "EMP-1012", "Payroll Manager", nil)
		u3cols(t, esi, "Action", "Employee code", "Name", "Father or husband name", "Date of birth", "Gender", "Marital status", "Date of joining", "ESIC number", "IP status",
			"Aadhaar", "Monthly wages", "Dispensary city", "Date of exit", "Reason for exit", "Mobile")
		ei := u3cells(esi, "Enrol", "EMP-1026")
		if ei == nil || ei[3] != "Ramesh Kumar" || ei[6] != "Married" || ei[8] != "3100001026" || ei[9] != "Existing IP, tag to employer" || ei[10] != "4321 8765 1026" ||
			num(ei[11]) != 18000 || ei[12] == "" || ei[13] != "" || ei[14] != "" || ei[15] != "9876500026" {
			t.Fatalf("ESI enrolment row: %v", ei)
		}

		// Masking as before: CHRO previews with government ids and bank details hidden, wages in full.
		_, chro := c.do("GET", "/api/statutory/pf"+q, "EMP-1001", "CHRO", nil)
		ce := u3cells(chro, "Enrol", "EMP-1026")
		if ce == nil || ce[7] != nil || ce[9] != nil || ce[11] != nil || ce[12] != nil || ce[13] != en[13] || ce[3] != "Ramesh Kumar" {
			t.Fatalf("CHRO's preview keeps the masking: %v", ce)
		}
		_, chroEsi := c.do("GET", "/api/statutory/esi"+q, "EMP-1001", "CHRO", nil)
		if ci := u3cells(chroEsi, "Enrol", "EMP-1026"); ci == nil || ci[8] != nil || ci[10] != nil || ci[11] != ei[11] {
			t.Fatalf("CHRO's ESI preview keeps the masking: %v", ci)
		}
		// The download (pay.export) carries the new headings too.
		s, raw := c.getRaw("/api/statutory/pf/sheet"+q, "EMP-1012", "Payroll Manager")
		if s != 200 {
			t.Fatal(s, string(raw))
		}
		if rows := sheetRows(t, raw); len(rows) < 2 || !u3eq(rows[0], u3strs(pf["cols"])) {
			t.Fatalf("download headings %v", rows[0])
		}
	})

	t.Run("month files: headings, and the ECR upload text is unchanged", func(t *testing.T) {
		_, ecr := c.do("GET", "/api/statutory/ecr?ym="+open, "EMP-1012", "Payroll Manager", nil)
		u3cols(t, ecr, "UAN", "Member name", "Gross wages", "EPF wages", "EPS wages", "EDLI wages", "EPF contribution (employee)", "EPS contribution (employer)",
			"EPF difference (employer)", "NCP days", "Refund of advances", "Remark")
		_, esic := c.do("GET", "/api/statutory/esic?ym="+open, "EMP-1012", "Payroll Manager", nil)
		u3cols(t, esic, "IP number", "IP name", "Days for which wages paid", "Total monthly wages", "Reason code for zero days", "Last working day (if left)", "Remark")

		s, txt := c.getRaw("/api/statutory/ecr.txt?ym="+open, "EMP-1012", "Payroll Manager")
		if s != 200 {
			t.Fatal(s, string(txt))
		}
		// EPFO's ECR: 11 fields joined by #~#, one line per member with a UAN, in EPFO's order:
		// UAN, name, gross, EPF wages, EPS wages, EDLI wages, EE share, EPS share, ER difference, NCP days, refund.
		lines := strings.Split(strings.TrimSuffix(string(txt), "\n"), "\n")
		want := []string{}
		for _, x := range u7list(ecr["rows"]) {
			m := u7map(x)
			if m["missing"] != nil && m["missing"] != "" {
				continue
			}
			parts := []string{}
			for _, v := range u7list(m["cells"])[:11] {
				parts = append(parts, strings.TrimSuffix(fmt.Sprint(v), ".00"))
			}
			want = append(want, strings.Join(parts, "#~#"))
		}
		if len(lines) == 0 || !u3eq(lines, want) {
			t.Fatalf("ECR text\n got %q\nwant %q", lines, want)
		}
		var line string
		for _, l := range lines {
			if n := len(strings.Split(l, "#~#")); n != 11 {
				t.Fatalf("an ECR line has %d fields: %q", n, l)
			}
			if strings.HasPrefix(l, "100100001020#~#") {
				line = l
			}
		}
		row := c.row(t, open, "EMP-1020")
		r := row["r"].(map[string]any)
		// Gross wages without reimbursements or bonus; EPS and EDLI wages capped at ₹15,000 (REAL-40, REAL-41); NCP days
		// without the days outside employment (REAL-42).
		epsWage := math.Min(math.Round(num(r["pfBase"])), 15000)
		eps := math.Min(math.Round(epsWage*0.0833), math.Round(num(r["erPF"])))
		exp := fmt.Sprintf("100100001020#~#Karan Khan#~#%v#~#%v#~#%v#~#%v#~#%v#~#%v#~#%v#~#%v#~#0", math.Round(num(r["gross"])+num(r["varPay"])), math.Round(num(r["pfBase"])), epsWage,
			epsWage, math.Round(num(r["eePF"])), eps, math.Round(num(r["erPF"]))-eps, math.Max(0, math.Round(num(row["days"])-num(row["paid"])-num(row["outsideP"]))))
		if line != exp {
			t.Fatalf("ECR line\n got %s\nwant %s", line, exp)
		}
	})
}

func TestUI3PayrollForm16AndProofs(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	setFirstYm(t, c, "")

	t.Run("Form 16 names the place of the registered office", func(t *testing.T) {
		_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		fy := fmt.Sprint(tax["fy"])
		place := func(addr string) any {
			t.Helper()
			if _, err := st.DB.C("company_setting").UpdateMany(ctx, nil, bson.M{"$set": bson.M{"address": addr}}); err != nil {
				t.Fatal(err)
			}
			s, f := c.do("GET", "/api/form16/EMP-1002?fy="+fy, "EMP-1012", "Payroll Manager", nil)
			if s != 200 {
				t.Fatalf("form 16: %d %v", s, f)
			}
			return u7map(f["employer"])["place"]
		}
		if p := place("Plot 12, Sector 29, Gurugram, Haryana 122001"); p != "Gurugram" {
			t.Fatalf("place %v", p)
		}
		if p := place("4th floor, Brigade Road, Bengaluru 560001"); p != "Bengaluru" {
			t.Fatalf("place %v", p)
		}
		if p := place("Survey 41, Baner Road, Pune, Maharashtra 411045"); p != "Pune" { // a city not in the masters
			t.Fatalf("place %v", p)
		}
		if p := place("Registered office address (edit in Masters, Company)"); p != "" {
			t.Fatalf("no place before the address is filled in: %v", p)
		}
	})

	t.Run("a proof shows its file name and the time it was uploaded", func(t *testing.T) {
		_, tax := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		w := tax["windows"].(map[string]any)
		yes := true
		win := map[string]any{"declFrom": w["declFrom"], "declTo": w["declTo"], "declForce": yes, "proofFrom": w["proofFrom"], "proofTo": w["proofTo"], "proofForce": yes}
		if s, r := c.do("PUT", "/api/tax/windows", "EMP-1002", "HR Admin", win); s != 200 {
			t.Fatal(r)
		}
		decl := map[string]any{"regime": "Old", "items": map[string]any{"c_ppf": 150000}, "submit": true}
		if s, r := c.do("PUT", "/api/tax/me", "EMP-1002", "Employee", decl); s != 200 {
			t.Fatal(r)
		}
		_, doc := c.upload("EMP-1002", "Employee", "employee", "EMP-1002", "PPF passbook")
		if s, r := c.do("POST", "/api/tax/me/proofs", "EMP-1002", "Employee", map[string]any{"item": "c_ppf", "doc": doc["id"], "amount": 150000, "remark": "FY deposit"}); s != 200 {
			t.Fatal(r)
		}
		at := regexp.MustCompile(`^\d{2} [A-Z][a-z]{2}, \d{2}:\d{2}$`)
		check := func(r map[string]any) {
			t.Helper()
			ps := u7list(r["proofs"])
			if len(ps) != 1 {
				t.Fatalf("proofs %v", r["proofs"])
			}
			p := u7map(ps[0])
			if p["file_name"] != "PPF passbook.pdf" || !at.MatchString(fmt.Sprint(p["at"])) || !strings.HasPrefix(fmt.Sprint(p["at"]), time.Now().Format("02 Jan")) {
				t.Fatalf("proof %v", p)
			}
		}
		_, me := c.do("GET", "/api/tax/me", "EMP-1002", "Employee", nil)
		check(me)
		s, emp := c.do("GET", "/api/tax/employee/EMP-1002", "EMP-1011", "Payroll", nil)
		if s != 200 {
			t.Fatal(emp)
		}
		check(emp)
	})
}

func TestUI3PayrollMasterOrder(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1011")
	get := func(path, emp, role string) map[string]any {
		t.Helper()
		s, r := c.do("GET", path, emp, role, nil)
		if s != 200 {
			t.Fatalf("%s: %d %v", path, s, r)
		}
		return r
	}

	t.Run("hold reasons and deduction types", func(t *testing.T) {
		holds := u3strs(get("/api/holds", "EMP-1011", "Payroll")["reasons"])
		if want := []string{"Absent without notice", "Pending clearance or handover", "Investigation or disciplinary case", "Bank or KYC issue", "Asset not returned", "Other"}; !u3eq(holds, want) {
			t.Fatalf("hold reasons %q", holds)
		}
		ded := u3strs(get("/api/pay-items/deductions", "EMP-1011", "Payroll")["dedTypes"])
		if want := []string{"Cash shortage", "Damage or loss of property", "Penalty as per policy", "Notice period shortfall", "Excess payment recovery", "Other recovery"}; !u3eq(ded, want) {
			t.Fatalf("deduction types %q", ded)
		}
		// A new reason goes to the end of the list, whatever its name.
		if s, r := c.do("POST", "/api/masters/change_reason", "EMP-1002", "HR Admin", map[string]any{"name": "Advance reason", "used_for": "Hold"}); s != 200 {
			t.Fatal(r)
		}
		holds = u3strs(get("/api/holds", "EMP-1011", "Payroll")["reasons"])
		if holds[len(holds)-1] != "Advance reason" || holds[0] != "Absent without notice" {
			t.Fatalf("new hold reason %q", holds)
		}
	})

	t.Run("masters in master order; order=name for callers that want names", func(t *testing.T) {
		cities := u3strs(get("/api/masters/city", "EMP-1002", "HR Admin")["list"])
		if want := []string{"Gurugram", "Delhi", "Bengaluru", "Hyderabad", "Mumbai"}; !u3eq(cities, want) {
			t.Fatalf("cities %q", cities)
		}
		var state string
		for _, x := range u7list(get("/api/masters/state", "EMP-1002", "HR Admin")["list"]) {
			if u7map(x)["name"] == "Haryana" {
				state = fmt.Sprint(u7map(x)["id"])
			}
		}
		if s, r := c.do("POST", "/api/masters/city", "EMP-1002", "HR Admin", map[string]any{"name": "Ambala", "state_id": state}); s != 200 {
			t.Fatal(r)
		}
		cities = u3strs(get("/api/masters/city?status=Active", "EMP-1002", "HR Admin")["list"])
		if want := []string{"Gurugram", "Delhi", "Bengaluru", "Hyderabad", "Mumbai", "Ambala"}; !u3eq(cities, want) {
			t.Fatalf("active cities %q", cities)
		}
		byName := u3strs(get("/api/masters/city?order=name", "EMP-1002", "HR Admin")["list"])
		if want := []string{"Ambala", "Bengaluru", "Delhi", "Gurugram", "Hyderabad", "Mumbai"}; !u3eq(byName, want) {
			t.Fatalf("cities by name %q", byName)
		}
		types := u3strs(get("/api/masters/employment_type", "EMP-1002", "HR Admin")["list"])
		if want := []string{"Permanent", "Retainer", "Contractual"}; !u3eq(types, want) {
			t.Fatalf("employment types %q", types)
		}
	})

	t.Run("calendars and leave plans", func(t *testing.T) {
		cals := u3strs(get("/api/calendars", "EMP-1002", "HR Admin")["list"])
		if want := []string{"NCR 2026", "Karnataka 2026", "Telangana 2026", "Maharashtra 2026"}; !u3eq(cals, want) {
			t.Fatalf("calendars %q", cals)
		}
		plans := func() []string {
			out := []string{}
			for _, x := range u7list(get("/api/leave-plans", "EMP-1002", "HR Admin")["plans"]) {
				out = append(out, fmt.Sprint(u7map(u7map(x)["plan"])["name"]))
			}
			return out
		}
		if want := []string{"Standard leave plan", "Property leadership leave plan", "No leave credit"}; !u3eq(plans(), want) {
			t.Fatalf("leave plans %q", plans())
		}
		if s, r := c.do("POST", "/api/leave-plans", "EMP-1002", "HR Admin", map[string]any{"name": "A new plan"}); s != 200 {
			t.Fatal(r)
		}
		if got := plans(); got[len(got)-1] != "A new plan" || got[0] != "Standard leave plan" {
			t.Fatalf("a new plan goes last: %q", got)
		}
	})
}

func TestUI3PayrollArrearsPipsAssets(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1013", "EMP-1020")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)

	t.Run("arrear reasons are the prototype's; older reasons still read and pay", func(t *testing.T) {
		_, meta := c.do("GET", "/api/pay-items/arrears", "EMP-1011", "Payroll", nil)
		if got, want := u3strs(meta["arrearTypes"]), []string{"Pay rate correction", "Attendance or leave correction", "Short payment in earlier month", "Joining or exit adjustment", "Other"}; !u3eq(got, want) {
			t.Fatalf("arrear reasons %q", got)
		}
		raise := func(typ string, amt float64) (int, map[string]any) {
			return c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_arrear", "employeeId": "EMP-1020",
				"payload": map[string]any{"amount": amt, "forYm": ymAdd(open, -1), "arrearType": typ, "slipText": "Shift allowance not paid in " + ymAdd(open, -1)}})
		}
		if s, r := raise("Pay rate correction", 1500); s != 200 {
			t.Fatal(r)
		}
		if s, _ := raise("Bonus", 1500); s != 400 {
			t.Fatal("an unknown reason is refused")
		}
		// A request raised with an older reason (an open form, the bulk gross change) is kept as it is.
		s, old := raise("Missed payment", 2500)
		if s != 200 {
			t.Fatal(old)
		}
		id := fmt.Sprint(old["id"])
		c.approveAll(t, id)
		_, list := c.do("GET", "/api/pay-items/arrears", "EMP-1011", "Payroll", nil)
		item := findRow(list["items"], "id", id)
		if item == nil || u7map(item["payload"])["arrearType"] != "Missed payment" || item["status"] != "Approved" {
			t.Fatalf("older arrear %v", item)
		}
		var amt float64
		lines, _ := st.DB.C("ledger").Find(ctx, bson.M{"source_id": id, "kind": "earn", "void": false})
		for _, l := range lines {
			amt += l["amount"].(float64)
		}
		if amt = math.Round(amt*100) / 100; amt != 2500 { // summed to the paisa, as the numeric sum was
			t.Fatalf("the older arrear is paid: %v", amt)
		}
	})

	t.Run("improvement plans waiting for approval on Upcoming pay lines", func(t *testing.T) {
		s, r := c.do("POST", "/api/consequences", "EMP-1009", "Line Manager", map[string]any{"employeeId": "EMP-1020", "kind": "PIP", "days": 15, "reason": "Attendance and punctuality", "details": "Late on five days this month."})
		if s != 200 {
			t.Fatal(r)
		}
		pip := func(emp, role string) map[string]any {
			t.Helper()
			s, up := c.do("GET", "/api/payroll/upcoming", emp, role, nil)
			if s != 200 {
				t.Fatalf("%s upcoming: %d %v", role, s, up)
			}
			for _, x := range u7list(up["pending"]) {
				if m := u7map(x); m["wf"] == "p_pip" && m["employee_id"] == "EMP-1020" {
					return m
				}
			}
			return nil
		}
		p := pip("EMP-1011", "Payroll")
		if p == nil || p["sub"] != "PIP · 15 days" || p["name"] != "Karan Khan" || num(u7map(p["payload"])["days"]) != 15 || !strings.HasPrefix(fmt.Sprint(p["id"]), "PIP-") {
			t.Fatalf("pending PIP %v", p)
		}
		if pip("EMP-1013", "Finance Head") == nil {
			t.Fatal("Finance Head sees the plan waiting, as in Approvals")
		}
		if s, _ := c.do("GET", "/api/payroll/upcoming", "EMP-1020", "Employee", nil); s != 403 {
			t.Fatal("employees do not see Upcoming pay lines")
		}
		if _, err := st.DB.C("consequence").UpdateMany(ctx, bson.M{"employee_id": "EMP-1020", "kind": "PIP"}, bson.M{"$set": bson.M{"status": "Active"}}); err != nil {
			t.Fatal(err)
		}
		if pip("EMP-1011", "Payroll") != nil {
			t.Fatal("an approved plan is no longer waiting")
		}
	})

	t.Run("Payroll adds assets to the central store, not uniform stock", func(t *testing.T) {
		_, inv := c.do("GET", "/api/inventory", "EMP-1011", "Payroll", nil)
		if inv["canAddAsset"] != true || inv["canCentral"] != false {
			t.Fatalf("Payroll flags %v %v", inv["canAddAsset"], inv["canCentral"])
		}
		var asset, uniform any
		for _, x := range u7list(inv["items"]) {
			m := u7map(x)
			if m["kind"] == "asset" && asset == nil {
				asset = m["id"]
			}
			if m["kind"] != "asset" && uniform == nil {
				uniform = m["id"]
			}
		}
		if asset == nil || uniform == nil {
			t.Fatalf("items %v", inv["items"])
		}
		add := func(emp, role, to string, item any, serial string) int {
			s, _ := c.do("POST", "/api/inventory/stock/add", emp, role, map[string]any{"to": to, "note": "Purchase",
				"lines": []any{map[string]any{"itemId": item, "serials": []string{serial}, "qty": 1, "name": "Model " + serial}}})
			return s
		}
		if s := add("EMP-1011", "Payroll", "central", asset, "P3-SN-1"); s != 200 {
			t.Fatalf("Payroll adds an asset: %d", s)
		}
		if s := add("EMP-1012", "Payroll Manager", "central", asset, "P3-SN-2"); s != 200 {
			t.Fatalf("Payroll Manager adds an asset: %d", s)
		}
		n, _ := st.DB.C("asset").Count(ctx, bson.M{"serial": bson.M{"$in": bson.A{"P3-SN-1", "P3-SN-2"}}, "store_id": "central", "status": "Available"})
		if n != 2 {
			t.Fatalf("assets in the central store: %d", n)
		}
		if s := add("EMP-1011", "Payroll", "central", uniform, "P3-SN-3"); s != 403 {
			t.Fatalf("uniform stock stays with HRBP Corporate: %d", s)
		}
		if s := add("EMP-1011", "Payroll", "hrbp:EMP-1004", asset, "P3-SN-4"); s != 403 {
			t.Fatalf("Payroll adds to the central store only: %d", s)
		}
		if s, _ := c.do("POST", "/api/inventory/stock/transfer", "EMP-1011", "Payroll", map[string]any{"from": "central", "to": "hrbp:EMP-1004",
			"lines": []any{map[string]any{"itemId": asset, "serials": []string{"P3-SN-1"}}}}); s != 403 {
			t.Fatalf("transfers stay with HRBP Corporate: %d", s)
		}
		if _, h := c.do("GET", "/api/inventory", "EMP-1004", "HRBP", nil); h["canAddAsset"] != false {
			t.Fatal("an HRBP gets assets by transfer")
		}
		if s := add("EMP-1004", "HRBP", "hrbp:EMP-1004", asset, "P3-SN-5"); s != 403 {
			t.Fatalf("HRBP: %d", s)
		}
	})
}
