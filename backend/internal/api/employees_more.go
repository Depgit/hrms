package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// ---------- Employee code series (HR Admin) ----------

type codeSeries struct {
	Key    string `json:"key"`
	Prefix string `json:"prefix"`
	Pad    int    `json:"pad"`
	Next   int    `json:"next"`
	Max    int    `json:"max"` // highest number already used with this prefix
}

type codeSettings struct {
	ByType bool         `json:"byType"`
	Rejoin string       `json:"rejoin"`
	Series []codeSeries `json:"series"`
	// NoEmpID: employment types that never get an Employee ID; they keep their own series' number from joining
	// (hiring.go). nil on a save leaves it as it is.
	NoEmpID []string `json:"noEmpId"`
}

var rePrefix = regexp.MustCompile(`^[A-Za-z0-9\-_]{0,8}$`) // no "/" (PPL-092)

var employeesMoreDigits = regexp.MustCompile(`^[0-9]{1,9}$`)

// maxUsed is the highest number already used after prefix, so a series can never hand out a used code: the codes
// that start with the prefix (any case; in the prefix, as in SQL LIKE, _ stands for any one character) and go on
// with 1 to 9 digits only.
func maxUsed(ctx context.Context, q store.Q, prefix string) int {
	codes, err := q.C("employee").Find(ctx, bson.M{"_id": employeesILike(prefix + "%")}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return 0
	}
	n, skip := 0, len([]rune(prefix))
	for _, x := range codes {
		id := []rune(str(x["id"]))
		if len(id) < skip {
			continue
		}
		if rest := string(id[skip:]); employeesMoreDigits.MatchString(rest) {
			if v, _ := strconv.Atoi(rest); v > n {
				n = v
			}
		}
	}
	return n
}

func (s *Server) codeSettingsGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.directory", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	out := codeSettings{Rejoin: "same", Series: []codeSeries{}, NoEmpID: []string{}}
	if set, _ := s.st.DB.C("employee_code_settings").FindOne(ctx, bson.M{"_id": 1}); set != nil {
		out.ByType, out.Rejoin, out.NoEmpID = set["by_type"] == true, str(set["rejoin"]), strs(set["no_emp_id"])
	}
	rows, err := s.st.DB.C("employee_code_series").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		out.Series = append(out.Series, codeSeries{Key: str(x["key"]), Prefix: str(x["prefix"]), Pad: employeesInt(x["pad"]), Next: employeesInt(x["next_no"])})
	}
	// The series for everyone first, then by key.
	sort.Slice(out.Series, func(i, j int) bool {
		a, b := out.Series[i], out.Series[j]
		if (a.Key == "All") != (b.Key == "All") {
			return a.Key == "All"
		}
		return a.Key < b.Key
	})
	for i := range out.Series {
		out.Series[i].Max = maxUsed(ctx, s.st.DB, out.Series[i].Prefix)
	}
	return out, nil
}

func (s *Server) codeSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin can change employee code rules")
	}
	var b codeSettings
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Rejoin != "same" && b.Rejoin != "new" {
		return nil, bad("Choose what happens when someone rejoins")
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		for _, x := range b.Series {
			label := x.Key
			if label == "All" {
				label = "everyone"
			}
			if !rePrefix.MatchString(x.Prefix) {
				return bad("Prefix can have letters, numbers and - _ only, up to 8 characters")
			}
			if x.Pad < 3 || x.Pad > 8 {
				return bad("Digits must be between 3 and 8")
			}
			mx := maxUsed(ctx, tx, x.Prefix)
			if x.Key == "All" { // a Retainer's RET- codes share these numbers (employeesPrefixes)
				mx = max(mx, maxUsed(ctx, tx, employeesRetPrefix(ctx, tx, x.Prefix)))
			}
			if x.Next <= mx {
				return bad("Next number for %s must be above %d, the highest code already used with that prefix", label, mx)
			}
			n, err := tx.C("employee_code_series").Update(ctx, bson.M{"key": x.Key}, bson.M{"$set": bson.M{"prefix": x.Prefix, "pad": x.Pad, "next_no": x.Next}})
			if err != nil {
				return err
			}
			if n == 0 {
				return bad("Unknown series %s", x.Key)
			}
		}
		set := bson.M{"by_type": b.ByType, "rejoin": b.Rejoin}
		if b.NoEmpID != nil {
			l := []string{}
			for _, t := range b.NoEmpID {
				if !containsStr([]string{"Permanent", "Retainer", "Contractual"}, t) {
					return bad("Unknown employment type %s", t)
				}
				if ok, err := tx.C("employee_code_series").Exists(ctx, bson.M{"key": t}); err != nil || !ok {
					if err == nil {
						err = bad("%s has no series of its own for the numbers it gives instead of an Employee ID", t)
					}
					return err
				}
				if !containsStr(l, t) {
					l = append(l, t)
				}
			}
			set["no_emp_id"] = l
		}
		if _, err := tx.C("employee_code_settings").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Employee code settings updated", "employee_code_settings", "1", nil, b)
	})
}

// ---------- Pay options: variable pay and deductions, by employment type ----------

type payOptions struct {
	VarPct   *float64 `json:"varPct"`
	VarCycle *int     `json:"varCycle"`
	Meal     float64  `json:"meal"`
	Accom    float64  `json:"accommodation"`
	// Medical is the medical insurance ₹ a month (nil: the structure's rule; b1_employee.go).
	Medical  *float64 `json:"medical"`
	PfExempt bool     `json:"pfExempt,omitempty"` // a joiner exempt from PF (pfuan.go)
	VarStart string   `json:"varStart,omitempty"` // the month variable pay started, when after joining
	// AllBasic and AllBasicFrom: the whole fixed gross paid as Basic from that month (the "no PF" choice, pfuan.go).
	AllBasic     bool   `json:"allBasic,omitempty"`
	AllBasicFrom string `json:"allBasicFrom,omitempty"`
	// CompAmts: the person's own ₹ a month for the per-employee pay components (calc.MPerEmp), by code; "reimb" is the
	// Reimbursement, outside the gross (reimb.go).
	CompAmts map[string]float64 `json:"compAmts,omitempty"`
	// TdsPct is a retainer's own TDS, % of gross (employee_pay.tds_pct; nil: the structure's "Retainer TDS"; 25 Nov 2026).
	TdsPct *float64 `json:"tdsPct"`
}

// checkTdsPct refuses a TDS % outside 0–50 (the structure's own limits, compensation.go).
func checkTdsPct(v *float64) error {
	if v != nil && (*v < 0 || *v > 50) {
		return bad("TDS must be between 0 and 50%% of gross")
	}
	return nil
}

// allBasicIn: the whole fixed gross is paid as Basic in month ym ("optout" PF choice, pfuan.go), only while PF exempt.
func (p payOptions) allBasicIn(ym string) bool {
	return p.AllBasic && p.PfExempt && ym >= p.AllBasicFrom
}

// payFor reads an employee's pay options, keeping only what the employment type allows.
func payFor(ctx context.Context, q store.Q, id, empType string) payOptions {
	var p payOptions
	if r, _ := q.C("employee_pay").FindOne(ctx, bson.M{"employee_id": id}); r != nil {
		if v, ok := r["var_pct"].(float64); ok {
			p.VarPct = &v
		}
		if v, ok := r["var_cycle"].(int32); ok {
			n := int(v)
			p.VarCycle = &n
		}
		p.Meal, _ = r["meal"].(float64)
		p.Accom, _ = r["accommodation"].(float64)
		if v, ok := r["medical"].(float64); ok {
			p.Medical = &v
		}
		p.PfExempt = r["pf_exempt"] == true
		p.VarStart = str(r["var_start"])
		p.AllBasic, p.AllBasicFrom = r["all_basic"] == true, str(r["all_basic_from"])
		p.CompAmts = byCompOf(r["comp_amts"])
		if v, ok := r["tds_pct"].(float64); ok {
			p.TdsPct = &v
		}
	}
	t := calc.RulesFor(calc.EmploymentType(empType))
	if !t.Variable {
		p.VarPct, p.VarCycle = nil, nil
	}
	if t.Ded != "all" {
		p.Meal, p.Accom = 0, 0
	}
	if !t.Statutory {
		p.Medical = nil
	}
	if t.Ded != "tds" {
		p.TdsPct = nil
	}
	return p
}

func (s *Server) employeePayPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.salary", 2); err != nil {
		return nil, err
	}
	id, ctx := r.PathValue("id"), r.Context()
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	var b struct {
		payOptions
		Reason string `json:"reason"`
		// The variable pay target (17 Nov 2026): VarMode "pct" (% of the fixed gross) or "amount" (₹ a month), VarVal the
		// figure (nil: unchanged). On a structure still on percentage rates only "pct" is taken, as the Variable pay %.
		VarMode string   `json:"varMode"`
		VarVal  *float64 `json:"varVal"`
		// Reimb is the Reimbursement (outside the gross), ₹ a month (nil: unchanged; reimb.go).
		Reimb *float64 `json:"reimb"`
		// TdsSet: the request sets the retainer's own TDS % (TdsPct; null = back to the structure's rate). Without it
		// the TDS % is unchanged.
		TdsSet bool `json:"tdsSet"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil || e == nil {
		return nil, notFound("Employee not found")
	}
	empType, status := str(e["type"]), str(e["status"])
	if status == "Inactive" {
		return nil, bad("Pay options cannot change after the employee has left")
	}
	t := calc.RulesFor(calc.EmploymentType(empType))
	cname, cst := structureFor(ctx, s.st.DB, id, openYm(ctx, s.st.DB)+"-01")
	targetMode := cst.Designed
	if targetMode {
		old := payFor(ctx, s.st.DB, id, empType)
		b.VarPct, b.VarCycle = old.VarPct, old.VarCycle // variable pay is the target below
	} else if b.VarVal != nil {
		if b.VarMode == "amount" {
			return nil, bad("%s pays variable as a %% of gross: give the target in %%", cname)
		}
		b.VarPct = b.VarVal
	}
	if !t.Variable && (b.VarVal != nil && *b.VarVal > 0) {
		return nil, bad("%s staff do not earn variable pay", empType)
	}
	if !t.Variable && (b.VarPct != nil || b.VarCycle != nil) {
		return nil, bad("%s staff do not earn variable pay", empType)
	}
	if t.Ded != "all" && (b.Meal != 0 || b.Accom != 0) {
		return nil, bad("Meal and accommodation deductions apply to permanent staff only")
	}
	if b.VarPct != nil && (*b.VarPct < 0 || *b.VarPct > 100) {
		return nil, bad("Variable pay must be between 0 and 100 percent")
	}
	if b.VarCycle != nil && !containsInt([]int{1, 3, 6, 12}, *b.VarCycle) {
		return nil, bad("Variable payout is every 1, 3, 6 or 12 months")
	}
	if b.Meal < 0 || b.Accom < 0 {
		return nil, bad("Deductions cannot be negative")
	}
	if b.TdsSet && b.TdsPct != nil && t.Ded != "tds" {
		return nil, bad("%s staff have no TDS on fees: it is for types whose deduction is TDS (Retainer)", empType)
	}
	if err := checkTdsPct(b.TdsPct); b.TdsSet && err != nil {
		return nil, err
	}
	if b.Reimb != nil {
		if err := reimbCheckFor(ctx, s.st.DB, id, empType, openYm(ctx, s.st.DB)+"-01", *b.Reimb); err != nil {
			return nil, err
		}
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		reason, ok := employeesReason(ctx, tx, "Salary", b.Reason)
		if !ok {
			return bad("Choose a reason for the change")
		}
		old := payFor(ctx, tx, id, empType)
		if !b.TdsSet {
			b.TdsPct = old.TdsPct
		}
		if b.Reimb != nil {
			if _, err := setReimb(ctx, tx, c, id, reimbOf(old), *b.Reimb, reason, today(), "Manual"); err != nil {
				return err
			}
		}
		set := bson.M{"var_pct": b.VarPct, "var_cycle": b.VarCycle, "meal": b.Meal, "accommodation": b.Accom}
		if b.TdsSet {
			set["tds_pct"] = b.TdsPct
		}
		if !sameFloatPtr(old.VarPct, b.VarPct) { // a new % applies from the open month on (RUN-31)
			set["var_from"], set["var_prev_pct"] = openYm(ctx, tx), old.VarPct
			varStartSet(ctx, tx, id, old.VarPct, b.VarPct, openYm(ctx, tx), set)
		}
		if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, set, nil); err != nil {
			return err
		}
		if targetMode && t.Variable && b.VarVal != nil {
			from := openYm(ctx, tx)
			if d := str(e["doj"]); len(d) >= 7 && d[:7] > from {
				from = d[:7]
			}
			cur := targetAt(loadTargets(ctx, tx, id), from)
			mode := b.VarMode
			if mode != "pct" {
				mode = "amount"
			}
			if cur == nil || cur.Mode != mode || (mode == "pct" && cur.Pct != *b.VarVal) || (mode != "pct" && cur.Total != *b.VarVal) {
				if _, err := setVarTarget(ctx, tx, c, id, from, mode, *b.VarVal, nil, reason, "Manual"); err != nil {
					return err
				}
			}
		}
		fmtP := func(v *float64) string {
			if v == nil {
				return "Standard"
			}
			return fmt.Sprint(*v) + "%"
		}
		fmtT := func(v *float64) string {
			if v == nil {
				return "Structure rate"
			}
			return fmt.Sprint(*v) + "% of gross"
		}
		fmtC := func(v *int) string {
			if v == nil {
				return "Standard"
			}
			return fmt.Sprintf("Every %d months", *v)
		}
		for _, ch := range [][3]string{
			{"Variable pay %", fmtP(old.VarPct), fmtP(b.VarPct)}, {"Variable payout cycle", fmtC(old.VarCycle), fmtC(b.VarCycle)},
			{"Meal deduction", inr(old.Meal), inr(b.Meal)}, {"Accommodation deduction", inr(old.Accom), inr(b.Accom)},
			{"TDS %", fmtT(old.TdsPct), fmtT(b.TdsPct)},
		} {
			if ch[1] == ch[2] {
				continue
			}
			if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": ch[0], "old_value": ch[1], "new_value": ch[2], "reason": reason,
				"effective_from": today(), "source": "Manual", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Pay options changed", "employee", id, old, b.payOptions)
	})
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func inr(v float64) string { return "₹" + strings.TrimSuffix(fmt.Sprintf("%.2f", v), ".00") }

// ---------- Download employee details as CSV ----------

type exportSection struct {
	key, label, right string
	cols              []string
}

var exportSections = []exportSection{
	{"emp", "Job and location", "emp.directory", []string{"Department", "Designation", "Grade", "City", "State", "Region", "Cluster", "Location", "Shift", "Attendance mode", "Approval flow", "Reporting manager L1", "Functional manager L2", "Role", "Skill"}},
	{"personal", "Personal details", "emp.personal", []string{"Mobile", "Email", "Date of birth", "Gender", "1st child name", "2nd child name"}},
	{"stat", "Statutory IDs", "emp.govid", []string{"PAN", "Aadhaar", "UAN", "ESIC no", "PAN check", "Existing UAN?"}},
	{"bank", "Bank", "emp.bank", []string{"Bank (current)", "Account number", "IFSC", "Bank effective from"}},
	{"salary", "Salary structure and pay", "emp.salary", []string{"Monthly gross", "Salary structure", "Structure effective from", "Basic", "HRA", "Special allowance", "Reimbursement", "Variable pay %", "Variable payout cycle (months)", "Meal deduction", "Accommodation deduction", "PF employee", "ESI employee", "CTC a month", "Tax regime", "Medical insurance (₹ a month)"}},
	{"policy", "Work policy and leave", "emp.directory", []string{"Work policy", "Mapped through", "Probation ends", "Weekly off", "Casual leave days", "Privilege leave days", "Sick leave days", "Notice period (days)", "Holiday calendar"}},
	{"appr", "Approvers", "emp.directory", []string{"Leave approvers", "Attendance regularization approvers", "Resignation approvers", "Claim approvers (up to ₹5,000)", "Advance approvers (up to ₹20,000)"}},
	{"hist", "Service history", "emp.directory", []string{"Times rejoined", "First joining date", "Previous code", "Earlier service"}},
	{"uniform", "Uniform sizes", "emp.directory", []string{"Shirt / T-shirt size", "Trouser waist", "Trouser length", "Shoe size (UK)"}},
}

// exportAllowed lists the groups a role may download. HRBP downloads job and location only.
func exportAllowed(c *Ctx) []exportSection {
	out := []exportSection{}
	for _, x := range exportSections {
		if c.Level(x.right) >= 1 && (c.Role != "HRBP" || x.key == "emp") {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) exportOptions(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.export", 2); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, x := range exportAllowed(c) {
		out = append(out, map[string]any{"key": x.key, "label": x.label, "count": len(x.cols)})
	}
	return map[string]any{"sections": out, "total": len(exportSections), "canDownload": c.CanDownload, "limited": c.Role == "HRBP"}, nil
}

// employeesMoreExportStages add to employee documents what a download row shows: the job and place (department,
// designation, location up to region, all required), the L1 and L2 managers, personal details and statutory IDs
// (when there are any) and the bank account in use this month (the latest from this month or before).
func employeesMoreExportStages(ym string) []bson.D {
	return []bson.D{
		employeesLookup("department", "department_id", "d"), employeesUnwind("d"),
		employeesLookup("designation", "designation_id", "g"), employeesUnwind("g"),
		employeesLookup("location", "location_id", "l"), employeesUnwind("l"),
		employeesLookup("cluster", "l.cluster_id", "k"), employeesUnwind("k"),
		employeesLookup("city", "k.city_id", "c"), employeesUnwind("c"),
		employeesLookup("state", "c.state_id", "st"), employeesUnwind("st"),
		employeesLookup("region", "st.region_id", "rg"), employeesUnwind("rg"),
		employeesLookup("employee", "manager_id", "m"),
		employeesLookup("employee", "functional_manager_id", "fm"),
		{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "employee_personal"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "employee_id"}, {Key: "as", Value: "p"}}}},
		{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "employee_gov_id"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "employee_id"}, {Key: "as", Value: "gi"}}}},
		{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "employee_bank"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "employee_id"},
			{Key: "pipeline", Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{{Key: "from_month", Value: bson.D{{Key: "$lte", Value: ym}}}}}},
				bson.D{{Key: "$sort", Value: bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}}},
				bson.D{{Key: "$limit", Value: 1}},
			}}, {Key: "as", Value: "b"}}}},
		{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0}, {Key: "id", Value: "$_id"}, {Key: "name", Value: 1}, {Key: "status", Value: 1}, {Key: "type", Value: 1}, {Key: "doj", Value: 1},
			{Key: "lwd", Value: 1}, {Key: "exit_reason", Value: 1}, {Key: "location_id", Value: "$l._id"},
			{Key: "department", Value: "$d.name"}, {Key: "designation", Value: "$g.name"}, {Key: "grade", Value: 1}, {Key: "city", Value: "$c.name"},
			{Key: "state", Value: "$st.name"}, {Key: "region", Value: "$rg.name"}, {Key: "cluster", Value: "$k.name"}, {Key: "location", Value: "$l.name"},
			{Key: "shift_code", Value: 1}, {Key: "attendance_mode", Value: 1}, {Key: "corporate", Value: "$d.corporate"},
			{Key: "manager", Value: employeesFirst("m.name")}, {Key: "fmanager", Value: employeesFirst("fm.name")},
			{Key: "mobile", Value: employeesFirst("p.mobile")}, {Key: "email", Value: employeesFirst("p.email")}, {Key: "dob", Value: employeesFirst("p.dob")},
			{Key: "gender", Value: employeesFirst("p.gender")}, {Key: "pan", Value: employeesFirst("gi.pan")}, {Key: "aadhaar", Value: employeesFirst("gi.aadhaar")},
			{Key: "uan", Value: employeesFirst("gi.uan")}, {Key: "esic", Value: employeesFirst("gi.esic")},
			{Key: "pan_status", Value: employeesFirst("gi.pan_status")}, {Key: "pan_checked", Value: employeesFirst("gi.pan_checked")},
			{Key: "child1", Value: employeesFirst("p.child1")}, {Key: "child2", Value: employeesFirst("p.child2")},
			{Key: "job_role", Value: 1}, {Key: "skill", Value: 1}, {Key: "existing_uan", Value: 1},
			{Key: "bank_name", Value: employeesFirst("b.bank_name")}, {Key: "account_no", Value: employeesFirst("b.account_no")},
			{Key: "ifsc", Value: employeesFirst("b.ifsc")}, {Key: "from_month", Value: employeesFirst("b.from_month")}, {Key: "prev_code", Value: 1},
		}}},
	}
}

// employeeExport streams one CSV row per employee in scope. Confidential values follow the
// role's Full / Masked / Hidden setting; groups the role cannot see are not offered.
func (s *Server) employeeExport(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := c.Need("emp.export", 2); err != nil {
		writeErr(w, err)
		return
	}
	if !c.CanDownload {
		writeErr(w, forbidden("Your role cannot download files"))
		return
	}
	qv, ctx := r.URL.Query(), r.Context()
	want := map[string]bool{}
	for _, k := range strings.Split(qv.Get("sections"), ",") {
		want[k] = true
	}
	secs := []exportSection{}
	for _, x := range exportAllowed(c) {
		if want[x.key] {
			secs = append(secs, x)
		}
	}
	scope, err := employeesScope(ctx, s.st.DB, c.Scope)
	if err != nil {
		writeErr(w, err)
		return
	}
	conds := []bson.M{scope}
	for _, key := range []string{"status", "type"} {
		if v := qv.Get(key); v != "" {
			conds = append(conds, bson.M{key: v})
		}
	}
	pipeline := append([]bson.D{{{Key: "$match", Value: employeesAnd(conds...)}}, {{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}}},
		employeesMoreExportStages(today()[:7])...)
	rows, err := s.st.DB.C("employee").Aggregate(ctx, pipeline)
	if err != nil {
		writeErr(w, err)
		return
	}
	head := []string{"Employee code", "Name", "Status", "Employment type", "Date of joining", "Last working day", "Exit reason"}
	for _, x := range secs {
		head = append(head, x.cols...)
	}
	date := today()
	var sizes map[string]map[string]string // uniform sizes, read once when the group is asked for (uniform.go)
	for _, x := range secs {
		if x.key == "uniform" {
			all := make([]string, 0, len(rows))
			for _, e := range rows {
				all = append(all, fmt.Sprint(e["id"]))
			}
			if sizes, err = sizesOf(ctx, s.st.DB, all); err != nil {
				writeErr(w, err)
				return
			}
		}
	}
	xc, err := newExportCtx(ctx, s.st.DB)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="employees-%s.csv"`, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	_ = cw.Write(head)
	for _, e := range rows {
		id := e["id"].(string)
		str := func(k string) string {
			if v := e[k]; v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
		show := func(group, k string) string {
			v := c.Show(group, e[k], id)
			if v == nil {
				if e[k] == nil {
					return ""
				}
				return "Hidden"
			}
			return fmt.Sprint(v)
		}
		line := []string{id, str("name"), str("status"), str("type"), str("doj"), str("lwd"), str("exit_reason")}
		for _, x := range secs {
			switch x.key {
			case "emp":
				flow := "Operations"
				if v, _ := e["corporate"].(bool); v {
					flow = "Corporate"
				}
				line = append(line, str("department"), str("designation"), str("grade"), str("city"), str("state"), str("region"), str("cluster"), str("location"),
					str("shift_code"), str("attendance_mode"), flow, str("manager"), str("fmanager"), str("job_role"), str("skill"))
			case "personal":
				// Personal details are not a masked group: the section is there only for roles with the personal
				// details right, who see them in full (PPL-103).
				line = append(line, str("mobile"), str("email"), str("dob"), str("gender"), str("child1"), str("child2"))
			case "stat":
				pc := ""
				if v := panView(store.Row{"pan": e["pan"], "pan_status": e["pan_status"], "pan_checked": e["pan_checked"]}); v != nil {
					pc = fmt.Sprint(v["status"])
				}
				line = append(line, show("govId", "pan"), show("govId", "aadhaar"), show("govId", "uan"), show("govId", "esic"), pc, yesNo(e["existing_uan"]))
			case "bank":
				line = append(line, show("bank", "bank_name"), show("bank", "account_no"), show("bank", "ifsc"), str("from_month"))
			case "salary":
				gross, ok := grossFor(ctx, s.st.DB, id, date[:7])
				if !ok {
					line = append(line, make([]string, len(x.cols))...)
					break
				}
				name, st := structureFor(ctx, s.st.DB, id, date)
				p := payFor(ctx, s.st.DB, id, str("type"))
				in := calc.Input{Type: calc.EmploymentType(str("type")), Gross: gross, Structure: withMedical(&st, p.Medical), VarPct: p.VarPct, VarCycle: p.VarCycle,
					Meal: p.Meal, Accom: p.Accom, Lwf: lwfFor(ctx, s.st.DB, str("location_id"), date), EmpAmts: p.CompAmts, AllIn: allInOn(ctx, s.st.DB, date[:7])}
				varTargetInput(ctx, s.st.DB, id, date[:7], st, &in) // a structure made from components (paycomp.go)
				res := calc.Calc(in)
				money := func(v float64) string {
					if m := c.Show("salary", calc.Round2(v), id); m != nil {
						return fmt.Sprint(m)
					}
					return "Hidden"
				}
				vp, vc := "", ""
				if p.VarPct != nil {
					vp = fmt.Sprint(*p.VarPct)
				}
				if p.VarCycle != nil {
					vc = fmt.Sprint(*p.VarCycle)
				}
				from, regime := salaryExtra(ctx, s.st.DB, id, xc.fy)
				line = append(line, money(gross), name, from, money(res.Basic), money(res.Hra), money(res.Sa), money(res.Reimb), vp, vc,
					fmt.Sprint(p.Meal), fmt.Sprint(p.Accom), money(res.EePF), money(res.EeESI), money(res.CTC), regime, money(res.Med))
			case "policy":
				em, err := loadEmp(ctx, s.st.DB, id)
				if err != nil {
					line = append(line, make([]string, len(x.cols))...)
					break
				}
				line = append(line, xc.policyCols(ctx, s.st.DB, em, calc.RulesFor(calc.EmploymentType(str("type"))).Leave)...)
			case "appr":
				line = append(line, xc.apprCols(ctx, s.st.DB, id)...)
			case "hist":
				var prev *string
				if v, ok := e["prev_code"].(string); ok && v != "" {
					prev = &v
				}
				line = append(line, histCols(ctx, s.st.DB, id, str("doj"), prev)...)
			case "uniform":
				m := sizes[id]
				line = append(line, m["shirt"], m["waist"], m["length"], m["shoe"])
			}
		}
		for i, v := range line { // a name like =1+2 is text, not a formula, when the file opens in Excel (PPL-110)
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				line[i] = csvCell(v)
			}
		}
		_ = cw.Write(line)
	}
	cw.Flush()
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Downloaded employee details (%d rows)", len(rows)), "employee", "export", nil, map[string]any{"sections": qv.Get("sections")})
}

// sameFloatPtr: both unset, or both set to the same value.
func sameFloatPtr(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
