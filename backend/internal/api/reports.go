package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// Reports (spec 20, prototype REPS). Every report respects data scope; viewing needs rep.reports,
// downloading needs rep.reports at Edit and the role's download right. Each report takes the
// prototype's filters (month or range, city, department, type, status, a grouping, and its own);
// without a range a report covers all dates. Columns, order and values follow the prototype; the
// run functions are in ui3_reports.go.

// WF, RS and DS are the report's own filters (Extra): workflow, request status, and a decision, type,
// nomination or status, as in the prototype (x: wf, rs, ds). Late picks late roster changes only.
type repFilter struct{ From, To, YM, City, Dept, Type, Status, Group, WF, RS, DS, Late string }

// repExtra is a report's own filter: query key, label and options (value, label). Load lists more options from
// the database.
type repExtra struct {
	K, L string
	O    [][2]string
	Load func(ctx context.Context, q store.Q) ([][2]string, error)
}

type report struct {
	ID, Group, Name, Hint string
	Filters               []string
	Extra                 []repExtra
	Need                  func(c *Ctx) bool
	Run                   func(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error)
	// Money names the columns shown in rupees; Sum the columns added up in a closing Total row (the
	// prototype's total rows, part of the download too).
	Money, Sum []string
	// Mask names confidential columns with their group (salary, bank, govId, medical), masked for the caller;
	// "personal" columns are blank without the personal details right.
	Mask map[string]string
	// Note is a line shown under the table (attrition, roster alerts).
	Note func(s *Server, ctx context.Context, c *Ctx, f repFilter, rows [][]any) string
}

// ---------- The employees of a report ----------

// repEmp is one employee of a report with the masters every report joins: department, designation, location,
// cluster and city (their names, and the city's id for the city filter).
type repEmp struct {
	E                              store.Row // the employee document
	Dept, Desig, Loc, City, CityID string
}

func (x repEmp) id() string { return str(x.E["id"]) }

// repJoin is the employee side of every report: employee e JOIN department d JOIN designation g JOIN location l
// JOIN cluster k JOIN city ci, inside the caller's data scope and the report's filters (city, department,
// employment type, status — "On notice" being the active people serving notice), plus extra, a condition on the
// employee document (nil for none). Employees come in code order; one missing a master is left out, as the inner
// joins did.
func repJoin(ctx context.Context, q store.Q, c *Ctx, f repFilter, extra any) ([]repEmp, error) {
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	all := bson.A{scope}
	if f.Type != "" {
		all = append(all, bson.D{{Key: "type", Value: f.Type}})
	}
	if f.Status == "On notice" { // still active, serving notice
		all = append(all, bson.D{{Key: "exit_status", Value: "On notice"}})
	} else if f.Status != "" {
		all = append(all, bson.D{{Key: "status", Value: f.Status}})
	}
	if extra != nil {
		all = append(all, extra)
	}
	rows, err := q.C("employee").Find(ctx, bson.D{{Key: "$and", Value: all}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	list, err := repJoinRows(ctx, q, rows)
	if err != nil {
		return nil, err
	}
	out := list[:0]
	for _, x := range list {
		if (f.City == "" || x.CityID == f.City) && (f.Dept == "" || x.Dept == f.Dept) {
			out = append(out, x)
		}
	}
	return out, nil
}

// repJoinRows joins employee documents with their department, designation, location, cluster and city, keeping
// their order and leaving out anyone missing one of them.
func repJoinRows(ctx context.Context, q store.Q, emps []store.Row) ([]repEmp, error) {
	depts, err := repByID(ctx, q, "department", repCol(emps, "department_id"))
	if err != nil {
		return nil, err
	}
	desigs, err := repByID(ctx, q, "designation", repCol(emps, "designation_id"))
	if err != nil {
		return nil, err
	}
	pl, err := repPlacesOf(ctx, q, emps)
	if err != nil {
		return nil, err
	}
	out := make([]repEmp, 0, len(emps))
	for _, e := range emps {
		d, g := depts[str(e["department_id"])], desigs[str(e["designation_id"])]
		l, ci := pl.of(e)
		if d == nil || g == nil || ci == nil {
			continue
		}
		out = append(out, repEmp{E: e, Dept: str(d["name"]), Desig: str(g["name"]), Loc: str(l["name"]), City: str(ci["name"]), CityID: str(ci["id"])})
	}
	return out, nil
}

// repPlaces are the locations, clusters and cities of a set of employees, by id.
type repPlaces struct{ locs, clusters, cities map[string]store.Row }

func repPlacesOf(ctx context.Context, q store.Q, emps []store.Row) (*repPlaces, error) {
	p := &repPlaces{}
	var err error
	if p.locs, err = repByID(ctx, q, "location", repCol(emps, "location_id")); err != nil {
		return nil, err
	}
	if p.clusters, err = repByID(ctx, q, "cluster", repMapCol(p.locs, "cluster_id")); err != nil {
		return nil, err
	}
	if p.cities, err = repByID(ctx, q, "city", repMapCol(p.clusters, "city_id")); err != nil {
		return nil, err
	}
	return p, nil
}

// of is an employee's location and city (location JOIN cluster JOIN city); both nil when the chain is broken.
func (p *repPlaces) of(e store.Row) (loc, city store.Row) {
	l := p.locs[str(e["location_id"])]
	if l == nil {
		return nil, nil
	}
	k := p.clusters[str(l["cluster_id"])]
	if k == nil {
		return nil, nil
	}
	ci := p.cities[str(k["city_id"])]
	if ci == nil {
		return nil, nil
	}
	return l, ci
}

// repByID loads the documents of coll with these ids (text ids), keyed by id.
func repByID(ctx context.Context, q store.Q, coll string, ids []string) (map[string]store.Row, error) {
	out := map[string]store.Row{}
	seen, uniq := map[string]bool{}, []string{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return out, nil
	}
	rows, err := q.C(coll).Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: uniq}}}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[str(r["id"])] = r
	}
	return out, nil
}

// repCol is the text values of field k in rows.
func repCol(rows []store.Row, k string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, str(r[k]))
	}
	return out
}

func repMapCol(m map[string]store.Row, k string) []string {
	out := make([]string, 0, len(m))
	for _, r := range m {
		out = append(out, str(r[k]))
	}
	return out
}

// repEmpIDs are the employees' codes, in order.
func repEmpIDs(list []repEmp) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = x.id()
	}
	return out
}

// repIndex is the list by employee code.
func repIndex(list []repEmp) map[string]repEmp {
	out := make(map[string]repEmp, len(list))
	for _, x := range list {
		out[x.id()] = x
	}
	return out
}

// repNames are employees' names by code (the reporting manager's, a raiser's, …).
func repNames(ctx context.Context, q store.Q, ids []string) (map[string]string, error) {
	rows, err := repByID(ctx, q, "employee", ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for id, r := range rows {
		out[id] = str(r["name"])
	}
	return out, nil
}

// repManagers is each employee's reporting manager by name ("" without one), by employee code.
func repManagers(ctx context.Context, q store.Q, list []repEmp) (map[string]string, error) {
	mids := make([]string, 0, len(list))
	for _, x := range list {
		mids = append(mids, str(x.E["manager_id"]))
	}
	names, err := repNames(ctx, q, mids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, x := range list {
		out[x.id()] = names[str(x.E["manager_id"])]
	}
	return out, nil
}

// ---------- Cell values ----------

// cell is how a report shows a stored value: a timestamp as date and time in India (a bare date when it is
// midnight), a list joined with commas, an object as sorted "key value" pairs; anything else as it is.
func cell(v any) any {
	switch x := v.(type) {
	case time.Time:
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 {
			return x.Format("2006-01-02")
		}
		return x.In(ist).Format("2006-01-02 15:04")
	case []any:
		s := []string{}
		for _, e := range x {
			s = append(s, fmt.Sprint(cell(e)))
		}
		return strings.Join(s, ", ")
	case map[string]any:
		b := []string{}
		for k, e := range x {
			b = append(b, fmt.Sprintf("%s %v", k, e))
		}
		sort.Strings(b)
		return strings.Join(b, ", ")
	}
	return v
}

// repDateIST is a timestamp's date in India (… AT TIME ZONE 'Asia/Kolkata')::date), nil for none.
func repDateIST(v any) any {
	t, ok := v.(time.Time)
	if !ok {
		return nil
	}
	return t.In(ist).Format("2006-01-02")
}

// repDayStart is the first instant of a date in the business zone, as a date compared with a timestamp was.
func repDayStart(d string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", d, store.Loc())
	return t
}

// repISTRange is the time range of the dates from..to in India: a timestamp whose Indian date lies between them.
func repISTRange(from, to string) bson.D {
	a, _ := time.ParseInLocation("2006-01-02", from, ist)
	b, _ := time.ParseInLocation("2006-01-02", to, ist)
	return bson.D{{Key: "$gte", Value: a}, {Key: "$lt", Value: b.AddDate(0, 0, 1)}}
}

func lvl(right string, n int) func(c *Ctx) bool {
	return func(c *Ctx) bool { return c.Level(right) >= n }
}
func hrOnly(c *Ctx) bool {
	return containsStr([]string{"HR Admin", "HRBP", "HRBP Corporate", "CHRO"}, c.Role)
}

// Shared column sets for masking: confidential columns by heading.
var (
	repPayMask = map[string]string{"Total earnings": "salary", "PF": "salary", "ESI": "salary", "TDS": "salary", "Total deductions": "salary", "Rounding off": "salary", "Net pay": "salary"}
	repStatCol = []string{"Employee PF", "Employer PF", "EPFO admin", "Employee ESI", "Employer ESI", "LWF employee", "LWF employer", "TDS", "Medical insurance"}
	repTdsCol  = []string{"Yearly tax", "Deducted this year", "TDS for the month", "Still to deduct"}
)

func salaryCols(cols ...string) map[string]string {
	m := map[string]string{}
	for _, c := range cols {
		m[c] = "salary"
	}
	return m
}

var reports []report

func init() {
	all := []string{"city", "dept", "type"}
	with := func(a ...string) []string { return a }
	reports = []report{
		// ---- Workforce ----
		{ID: "headcount", Group: "Workforce", Name: "Headcount summary", Hint: "Count of people by city, department, type, grade or gender",
			Filters: with("group", "city", "dept", "type"), Run: repHeadcount, Sum: []string{"Active", "Pre-boarding", "On notice (in active)", "Inactive", "Total"}},
		{ID: "emp_master", Group: "Workforce", Name: "Employee master list", Hint: "Every employee with job, location and reporting details",
			Filters: append(all, "status"), Run: repMaster},
		{ID: "joiners", Group: "Workforce", Name: "New joiners", Hint: "People who joined in the period, with the details the medical insurer, EPF and ESIC need",
			Filters: append([]string{"range"}, all...), Run: repJoiners, Money: []string{"Monthly gross"}, Mask: repJoinerMask()},
		{ID: "exits", Group: "Workforce", Name: "Exits and attrition", Hint: "Last working day in the period, with tenure and reason", Filters: append([]string{"range"}, all...),
			Run: repExits, Note: repExitsNote},
		{ID: "on_notice", Group: "Workforce", Name: "Employees on notice", Hint: "Resignations in progress with last working day", Filters: all, Run: repNotice},
		{ID: "probation", Group: "Workforce", Name: "Probation and confirmation due", Hint: "Probation end date comes from each person’s work policy and grade",
			Filters: append([]string{"range"}, all...), Run: repProbation},
		{ID: "birthdays", Group: "Workforce", Name: "Birthdays and work anniversaries", Hint: "For one month", Filters: with("month", "city", "dept"), Run: repBirthdays},
		{ID: "nominations", Group: "Workforce", Name: "Nomination status", Hint: "Every employee and nomination, with status and due date", Filters: with("city", "dept"),
			Extra: []repExtra{{K: "ds", L: "Nomination", O: [][2]string{{"", "All"}}, Load: repSchemeOptions},
				{K: "rs", L: "Status", O: [][2]string{{"", "All"}, {"Not submitted", "Not submitted"}, {"Submitted", "Waiting for Payroll"}, {"Verified", "Verified"}, {"Returned", "Returned"}}}},
			Run: repNominations},
		{ID: "changes", Group: "Workforce", Name: "Employee change history", Hint: "Every change to an employee record with reason, effective date and who made it",
			Filters: with("range", "city", "dept"), Extra: []repExtra{{K: "ds", L: "Status", O: [][2]string{{"", "All"}, {"Applied", "Applied"}, {"Scheduled", "Scheduled"}}}},
			Run: repChanges},
		{ID: "separations", Group: "Workforce", Name: "Separations", Hint: "Every exit with type, stage, clearance and full and final", Filters: all,
			Need: lvl("emp.sep", 1), Run: repSeparations},
		{ID: "exit_interviews", Group: "Workforce", Name: "Exit interviews", Hint: "Reasons for leaving and ratings, to spot patterns by location or manager",
			Filters: with("city", "dept"), Need: func(c *Ctx) bool { return hrOnly(c) && c.Level("emp.sep") >= 1 }, Run: repExitInterviews},

		// ---- Attendance and roster ----
		{ID: "att_summary", Group: "Attendance and roster", Name: "Monthly attendance summary", Hint: "Present, late, half day and missing punches with loss of pay days",
			Filters: with("month", "city", "dept", "type"), Run: repAttMonth(false)},
		{ID: "late", Group: "Attendance and roster", Name: "Late coming", Hint: "Most late arrivals first", Filters: with("month", "city", "dept"), Run: repAttMonth(true)},
		{ID: "roster_late", Group: "Attendance and roster", Name: "Roster discipline", Hint: "Roster changes made close to the shift, with reason and who changed them",
			Filters: with("month", "city", "dept"), Extra: []repExtra{{K: "late", L: "Changes", O: [][2]string{{"", "All changes"}, {"late", "Late changes only"}}}},
			Run: repRosterChanges, Note: repRosterNote},
		{ID: "leave_bal", Group: "Attendance and roster", Name: "Leave balances", Hint: "Casual, privilege and sick leave as on today", Filters: all, Run: repLeave},
		{ID: "deviations", Group: "Attendance and roster", Name: "Roster deviations", Hint: "Planned shift against actual punches, with the manager’s decision",
			Filters: with("range", "city", "dept"), Extra: []repExtra{{K: "ds", L: "Decision", O: [][2]string{{"", "All"}, {"Pending", "Waiting"}, {"OK", "OK"}, {"Not OK", "Not OK"}}}},
			Run: repDeviations},
		{ID: "disc_score", Group: "Attendance and roster", Name: "Employee discipline score", Hint: "Score for the month, lowest first", Filters: with("month", "city", "dept"), Run: repDiscScore},
		{ID: "mgr_score", Group: "Attendance and roster", Name: "Manager planning score", Hint: "How well each manager’s rosters match how the team really works",
			Filters: with("month"), Run: repMgrScore},
		{ID: "consequences", Group: "Attendance and roster", Name: "Warning letters and PIPs", Hint: "Every warning letter and improvement plan, with number, approver and result",
			Filters: with("range", "city", "dept"), Extra: []repExtra{{K: "ds", L: "Type", O: [][2]string{{"", "All"}, {"Warning", "Warning letters"}, {"PIP", "PIPs"}}}},
			Run: repConsequences},

		// ---- Payroll ----
		{ID: "register", Group: "Payroll", Name: "Payroll register", Hint: "Month-wise earnings, deductions and net pay for each employee", Filters: with("month", "city", "dept", "type"),
			Need: lvl("pay.run", 1), Run: repRegister, Money: []string{"Total earnings", "PF", "ESI", "TDS", "Total deductions", "Rounding off", "Net pay"},
			Sum: []string{"Paid days", "Total earnings", "PF", "ESI", "TDS", "Total deductions", "Rounding off", "Net pay"}, Mask: repPayMask},
		{ID: "cost", Group: "Payroll", Name: "Payroll cost", Hint: "Gross, employer contributions and total cost by city, department, type or grade",
			Filters: with("month", "group", "city", "dept", "type"), Need: lvl("pay.run", 1), Run: repCost,
			Money: []string{"Gross earnings", "Employer contributions", "Total cost to company"}, Sum: []string{"People paid", "Gross earnings", "Employer contributions", "Total cost to company"},
			Mask: salaryCols("Gross earnings", "Employer contributions", "Total cost to company")},
		{ID: "statutory", Group: "Payroll", Name: "Statutory summary", Hint: "PF, ESI, labour welfare fund and TDS to pay, by city", Filters: with("month", "dept", "type"),
			Need: lvl("pay.run", 1), Run: repStatutory, Money: repStatCol, Sum: repStatCol, Mask: salaryCols(repStatCol...)},
		{ID: "bank", Group: "Payroll", Name: "Bank advice", Hint: "Who gets paid how much, to which account", Filters: with("month", "city", "type"), Need: lvl("pay.export", 2),
			Run: repBank, Note: repBankNote, Money: []string{"Net pay"}, Sum: []string{"Net pay"}, Mask: map[string]string{"Bank account": "bank", "IFSC": "bank", "Net pay": "salary"}},
		{ID: "pay_lines", Group: "Payroll", Name: "Arrears, recoveries and claims", Hint: "Extra pay lines for the month", Filters: with("month", "city"), Need: lvl("pay.run", 1),
			Run: repLedger, Money: []string{"Amount"}, Mask: salaryCols("Amount")},
		{ID: "retainer", Group: "Payroll", Name: "Retainer and contractual payments", Hint: "People paid without payslips or leave", Filters: with("month", "city", "dept"),
			Need: lvl("pay.run", 1), Run: repRetainer, Money: []string{"Amount", "TDS", "Net payment"}, Sum: []string{"Paid days", "Amount", "TDS", "Net payment"},
			Mask: salaryCols("Amount", "TDS", "Net payment")},
		{ID: "holds", Group: "Payroll", Name: "Salary holds", Hint: "Every salary hold with reason, held amount and release", Filters: with("city", "dept"), Run: repHolds,
			Mask: salaryCols("Held (₹)")},
		{ID: "deductions", Group: "Payroll", Name: "Manual deductions", Hint: "Cash shortage, damage and other deductions with approval and balance", Filters: with("city", "dept"),
			Run: repDeductions, Mask: salaryCols("Amount (₹)")},
		{ID: "tds", Group: "Payroll", Name: "TDS on salary", Hint: "Yearly tax, tax deducted so far and TDS for the month, permanent staff and retainers",
			Filters: with("month", "city", "dept", "type"), Need: lvl("pay.run", 1), Run: repTds, Money: repTdsCol, Sum: repTdsCol, Mask: salaryCols(repTdsCol...)},

		// ---- Approvals ----
		{ID: "approvals", Group: "Approvals", Name: "Approval requests", Hint: "Every request with its stage and result", Filters: with("city"),
			Extra: []repExtra{{K: "wf", L: "Workflow", O: append([][2]string{{"", "All workflows"}}, reportWorkflows...)},
				{K: "rs", L: "Status", O: [][2]string{{"", "All"}, {"Pending", "Pending"}, {"Approved", "Approved"}, {"Rejected", "Rejected"}}}},
			Run: repApprovals},
		{ID: "workload", Group: "Approvals", Name: "Approver workload", Hint: "Pending requests grouped by who must act next", Filters: []string{}, Run: repWorkload},

		// ---- Compliance and audit ----
		{ID: "missing", Group: "Compliance and audit", Name: "Missing details and IDs", Hint: "Pending PAN, Aadhaar, bank, UAN, ESIC number, date of birth, mobile",
			Filters: all, Run: repMissing},
		{ID: "policy_ack", Group: "Compliance and audit", Name: "Company policy acknowledgement", Hint: "Every published company policy for each person it applies to, with status and approval",
			Filters: all, Extra: repPolicyAckStatus, Run: repPolicyAck},
		{ID: "masters", Group: "Compliance and audit", Name: "Master data and usage", Hint: "All masters including inactive ones, with active employees still using them",
			Filters: []string{}, Need: func(c *Ctx) bool { return c.Level("mas.org") >= 1 || c.Level("mas.job") >= 1 }, Run: repMasters},
		{ID: "activity", Group: "Compliance and audit", Name: "Activity log", Hint: "Recent changes made in the system", Filters: []string{},
			Need: func(c *Ctx) bool { return c.Level("acc.roles") >= 1 || c.Role == "HR Admin" }, Run: repActivity},

		// ---- Inventory ----
		{ID: "inv_moves", Group: "Inventory", Name: "Inventory: opening, additions, issues and closing",
			Hint: "Uniform and assets by store for one month. Closing is matched with the serial numbers on hand.", Filters: with("month"), Need: lvl("emp.assets", 1),
			Run: repInventory, Money: []string{"Unit price", "Closing value"}},
		{ID: "inv_deductions", Group: "Inventory", Name: "Inventory issued to employees and salary deduction",
			Hint: "Each issue and return in the month, with the recovery from salary where it applies", Filters: with("month", "city"), Need: lvl("emp.assets", 1),
			Run: repInvEmployees, Money: []string{"Value", "Deducted in this month"}, Mask: salaryCols("Deducted in this month")},
	}
}

func reportByID(id string) *report {
	for i := range reports {
		if reports[i].ID == id {
			return &reports[i]
		}
	}
	return nil
}

func (s *Server) reportList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("rep.reports", 1); err != nil {
		return nil, err
	}
	off, err := reportsOff(r.Context(), s.st.DB, c.Role) // reports the role is not given (report_access.go)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, rp := range reports {
		if (rp.Need != nil && !rp.Need(c)) || off[rp.ID] {
			continue
		}
		out = append(out, map[string]any{"id": rp.ID, "group": rp.Group, "name": rp.Name, "hint": rp.Hint, "filters": rp.Filters, "extra": repExtras(r.Context(), s.st.DB, rp.Extra)})
	}
	return map[string]any{"reports": out, "canDownload": c.Level("rep.reports") >= 2 && c.CanDownload}, nil
}

// Without a range a report covers all dates.
const repAllFrom, repAllTo = "1900-01-01", "2999-12-31"

func repParams(r *http.Request) repFilter {
	q := r.URL.Query()
	f := repFilter{From: q.Get("from"), To: q.Get("to"), YM: q.Get("ym"), City: q.Get("city"), Dept: q.Get("dept"), Type: q.Get("type"), Status: q.Get("status"), Group: q.Get("group"),
		WF: q.Get("wf"), RS: q.Get("rs"), DS: q.Get("ds"), Late: q.Get("late")}
	if !isDate(f.From) {
		f.From = repAllFrom
	}
	if !isDate(f.To) {
		f.To = repAllTo
	}
	if !isYm(f.YM) {
		f.YM = addYm(today()[:7], -1)
	}
	f.YM += "-01"
	return f
}

// repResult is a report run for a caller: columns, rows and the total row, masked, and the note.
type repResult struct {
	rp   *report
	cols []string
	rows [][]any
	sum  []any
	note string
}

func (s *Server) runReport(r *http.Request, c *Ctx) (*repResult, error) {
	if err := c.Need("rep.reports", 1); err != nil {
		return nil, err
	}
	rp := reportByID(r.PathValue("id"))
	if rp == nil || (rp.Need != nil && !rp.Need(c)) {
		return nil, notFound("Report not found")
	}
	if err := reportTicked(r.Context(), s.st.DB, c, rp.ID); err != nil { // the role's report ticks (report_access.go)
		return nil, err
	}
	ctx, f := r.Context(), repParams(r)
	cols, rows, err := rp.Run(s, ctx, c, f)
	if err != nil {
		return nil, err
	}
	out := &repResult{rp: rp, cols: cols, rows: rows}
	if len(rp.Sum) > 0 && len(rows) > 0 {
		out.sum = repTotal(cols, rows, rp.Sum)
	}
	if rp.Note != nil {
		out.note = rp.Note(s, ctx, c, f, rows)
	}
	repMask(c, rp.Mask, cols, rows, out.sum)
	return out, nil
}

// repIdx are the positions of the named columns.
func repIdx(cols, names []string) []int {
	out := []int{}
	for i, c := range cols {
		if containsStr(names, c) {
			out = append(out, i)
		}
	}
	return out
}

func repNum(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// repTotal is the Total row: the named columns added up, the rest blank.
func repTotal(cols []string, rows [][]any, names []string) []any {
	tot := make([]any, len(cols))
	for i := range tot {
		tot[i] = ""
	}
	tot[0] = "Total"
	for _, i := range repIdx(cols, names) {
		t := 0.0
		for _, r := range rows {
			if v, ok := repNum(r[i]); ok {
				t += v
			}
		}
		tot[i] = math.Round(t*100) / 100
	}
	return tot
}

// repMask applies the caller's rights and sensitive modes to confidential columns: without the
// salary or payroll right salary values are blank (as before), without the bank or payroll export
// right bank values are blank; otherwise Full / Masked / Hidden as for the role. Own values stay in
// full. The Total row is masked like the rows (no owner).
func repMask(c *Ctx, mask map[string]string, cols []string, rows [][]any, sum []any) {
	if len(mask) == 0 {
		return
	}
	own := -1
	for i, h := range cols {
		if h == "Code" || h == "Employee code" {
			own = i
			break
		}
	}
	for i, h := range cols {
		g := mask[h]
		if g == "" {
			continue
		}
		for _, r := range rows {
			owner := ""
			if own >= 0 {
				owner = str(r[own])
			}
			r[i] = repMaskCell(c, g, r[i], owner)
		}
		if sum != nil && sum[i] != "" {
			sum[i] = repMaskCell(c, g, sum[i], "")
		}
	}
}

func repMaskCell(c *Ctx, group string, v any, owner string) any {
	if v == nil || v == "" || (owner != "" && owner == c.EmployeeID) {
		return v
	}
	switch group {
	case "personal": // date of birth, mobile, email, father or spouse name: the personal details right
		if c.Level("emp.personal") < 1 {
			return nil
		}
		return v
	case "salary":
		if c.Level("emp.salary") < 1 && c.Level("pay.run") < 1 {
			return nil
		}
	case "bank":
		if c.Level("emp.bank") < 1 && c.Level("pay.export") < 1 {
			return nil
		}
		// "Bank name account number": only the number is confidential.
		if s, ok := v.(string); ok {
			if i := strings.LastIndex(s, " "); i > 0 && strings.ContainsAny(s[i+1:], "0123456789") {
				if m := c.Show(group, s[i+1:], owner); m != nil {
					return s[:i] + " " + str(m)
				}
				return nil
			}
		}
	}
	return c.Show(group, v, owner)
}

func (s *Server) reportGet(r *http.Request, c *Ctx) (any, error) {
	res, err := s.runReport(r, c)
	if err != nil {
		return nil, err
	}
	rows, total := res.rows, len(res.rows)
	if total > 500 {
		rows = rows[:500]
	}
	out := map[string]any{"name": res.rp.Name, "cols": res.cols, "rows": rows, "total": total, "money": repIdx(res.cols, res.rp.Money)}
	if res.sum != nil {
		out["sum"] = res.sum
	}
	if res.note != "" {
		out["note"] = res.note
	}
	return out, nil
}

func (s *Server) reportSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := c.Need("rep.reports", 2); err != nil {
		return "", nil, nil, forbidden("Downloading reports needs the report download right")
	}
	res, err := s.runReport(r, c)
	if err != nil {
		return "", nil, nil, err
	}
	rows := res.rows
	if res.sum != nil {
		rows = append(rows, res.sum)
	}
	_ = audit(r.Context(), s.st.DB, c, "Downloaded report "+res.rp.Name, "report", res.rp.ID, nil, r.URL.RawQuery)
	return res.rp.ID + "-" + today() + ".xlsx", res.cols, rows, nil
}

// ---------- Headcount ----------

// repHeadcount counts people by city (default), location, department, type, grade or gender (blank as "Not
// recorded"), largest group first.
func repHeadcount(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	gender := map[string]string{}
	if f.Group == "gender" && len(list) > 0 {
		pers, err := q.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": repEmpIDs(list)}},
			options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "gender", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		for _, p := range pers {
			gender[str(p["employee_id"])] = str(p["gender"])
		}
	}
	key := func(x repEmp) string {
		switch f.Group {
		case "location", "loc":
			return x.Loc
		case "department", "dept":
			return x.Dept
		case "type":
			return str(x.E["type"])
		case "grade":
			return str(x.E["grade"])
		case "gender":
			return or(gender[x.id()], "Not recorded")
		}
		return x.City
	}
	type grp struct {
		key                                  string
		active, pre, notice, inactive, total int64
	}
	by, order := map[string]*grp{}, []*grp{}
	for _, x := range list {
		k := key(x)
		g := by[k]
		if g == nil {
			g = &grp{key: k}
			by[k], order = g, append(order, g)
		}
		switch str(x.E["status"]) {
		case "Active":
			g.active++
			if str(x.E["exit_status"]) == "On notice" {
				g.notice++
			}
		case "Pre-boarding":
			g.pre++
		case "Inactive":
			g.inactive++
		}
		g.total++
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].total != order[j].total {
			return order[i].total > order[j].total
		}
		return order[i].key < order[j].key
	})
	out := [][]any{}
	for _, g := range order {
		out = append(out, []any{g.key, g.active, g.pre, g.notice, g.inactive, g.total})
	}
	return []string{"Group", "Active", "Pre-boarding", "On notice (in active)", "Inactive", "Total"}, out, nil
}

func mustISO(d string) time.Time { t, _ := time.Parse("2006-01-02", d); return t }

// ---------- The reports' own filters ----------

// reportWorkflows are the approval workflows by key and name, for the Approval requests report
// (with shift swaps, as in the prototype).
var reportWorkflows = append(append([][2]string{}, approval.Workflows...), [2]string{"swap", "Shift swap"}, [2]string{"p_pip", "Performance improvement plan"},
	[2]string{"p_tax", "Tax proofs"})

// repWfLabel is a workflow's name for its key (the key itself when it is not listed).
func repWfLabel(wf string) string {
	for _, w := range reportWorkflows {
		if w[0] == wf {
			return w[1]
		}
	}
	return wf
}

// repSchemeOptions are the nominations on the form, in id order (the Nomination report's own filter).
func repSchemeOptions(ctx context.Context, q store.Q) ([][2]string, error) {
	rows, err := q.C("nomination_scheme").Find(ctx, bson.M{"on_form": true},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{fmt.Sprint(r["id"]), str(r["name"])})
	}
	return out, nil
}

// repExtras lists a report's own filters with their options for the report list.
func repExtras(ctx context.Context, q store.Q, xs []repExtra) []map[string]any {
	out := []map[string]any{}
	for _, x := range xs {
		o := append([][2]string{}, x.O...)
		if x.Load != nil {
			if more, err := x.Load(ctx, q); err == nil {
				o = append(o, more...)
			}
		}
		out = append(out, map[string]any{"k": x.K, "l": x.L, "o": o})
	}
	return out
}
