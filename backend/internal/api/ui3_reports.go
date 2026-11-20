package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Reports as the prototype builds them (REPS): every report returns the prototype's columns in its
// order, with the values it computes, from the data the server holds. Rows stay inside the caller's
// data scope and the report's filters (repJoin); confidential columns are masked in runReport.

// ---------- Shared helpers ----------

// repIDs lists the employees in the caller's scope that match the report's filters and extra (a
// condition on the employee document, nil for none), in code order.
func repIDs(ctx context.Context, q store.Q, c *Ctx, f repFilter, extra any) ([]string, error) {
	list, err := repJoin(ctx, q, c, f, extra)
	if err != nil {
		return nil, err
	}
	return repEmpIDs(list), nil
}

// repPerson is what a report shows of a person beyond the employee record in emp: city and
// reporting manager by name.
type repPerson struct{ City, Manager string }

// repPeople is the city (through location and cluster) and reporting manager of each employee; one whose
// location, cluster or city is missing is left out.
func repPeople(ctx context.Context, q store.Q, ids []string) (map[string]repPerson, error) {
	out := map[string]repPerson{}
	if len(ids) == 0 {
		return out, nil
	}
	emps, err := q.C("employee").Find(ctx, bson.D{{Key: "_id", Value: repIn(ids)}},
		options.Find().SetProjection(bson.D{{Key: "location_id", Value: 1}, {Key: "manager_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	pl, err := repPlacesOf(ctx, q, emps)
	if err != nil {
		return nil, err
	}
	names, err := repNames(ctx, q, repCol(emps, "manager_id"))
	if err != nil {
		return nil, err
	}
	for _, e := range emps {
		if _, ci := pl.of(e); ci != nil {
			out[str(e["id"])] = repPerson{City: str(ci["name"]), Manager: names[str(e["manager_id"])]}
		}
	}
	return out, nil
}

// repEmps loads the employees of a report, in code order.
func repEmps(ctx context.Context, q store.Q, ids []string) ([]*emp, error) {
	return loadEmps(ctx, q, bson.D{{Key: "_id", Value: repIn(ids)}})
}

// repIn is {$in: ids} (an empty list matches nothing).
func repIn(ids []string) bson.D {
	if ids == nil {
		ids = []string{}
	}
	return bson.D{{Key: "$in", Value: ids}}
}

// repInt64 reads a stored whole number.
func repInt64(v any) int64 {
	switch x := v.(type) {
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// repJSONText is jsonb ->> 'key': a JSON string as it is, any other value as JSON text; false for a missing key
// or JSON null (SQL NULL).
func repJSONText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

// repJSONFloat is (jsonb ->> 'key')::float8 (or ::numeric): nil for NULL, an invalid value error (22P02) for text
// that is not a number, as the cast failed.
func repJSONFloat(v any) (any, error) {
	s, ok := repJSONText(v)
	if !ok {
		return nil, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil, &store.Error{Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type numeric: %q", s)}
	}
	return f, nil
}

// repJSONInt is (jsonb ->> 'key')::int: nil for NULL, an invalid value error (22P02) for text that is not a
// whole number.
func repJSONInt(v any) (any, error) {
	s, ok := repJSONText(v)
	if !ok {
		return nil, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return nil, &store.Error{Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type integer: %q", s)}
	}
	return int32(n), nil
}

// ---------- Workforce ----------

// repMaster is the employee master list, with each person's work policy.
func repMaster(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Status", "Date of joining", "City", "Location", "Department", "Designation", "Grade", "Reporting manager", "Shift",
		"Work policy", "Attendance mode", "Exit status"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	mgr, err := repManagers(ctx, q, list)
	if err != nil {
		return nil, nil, err
	}
	rows := [][]any{}
	for _, x := range list {
		e := x.E
		rows = append(rows, []any{e["id"], e["name"], e["type"], e["status"], e["doj"], x.City, x.Loc, x.Dept, x.Desig, e["grade"], mgr[x.id()], e["shift_code"],
			"", str(e["attendance_mode"]), str(e["exit_status"])})
	}
	if len(rows) == 0 {
		return cols, rows, nil
	}
	emps, err := repEmps(ctx, q, repEmpIDs(list))
	if err != nil {
		return nil, nil, err
	}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	pol := map[string]string{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		pol[e.ID] = p.Name
	}
	for _, r := range rows {
		r[12] = pol[r[0].(string)]
	}
	return cols, rows, nil
}

// repMonthlyGross is each joiner's monthly gross: the latest salary from this month or the joining month,
// whichever is later (nothing without one).
func repMonthlyGross(ctx context.Context, q store.Q, list []repEmp) (map[string]any, error) {
	hist, err := q.C("gross_history").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}}))
	if err != nil {
		return nil, err
	}
	by := map[string][]store.Row{}
	for _, h := range hist {
		by[str(h["employee_id"])] = append(by[str(h["employee_id"])], h)
	}
	out := map[string]any{}
	now := today()[:7]
	for _, x := range list {
		upTo := maxYm(now, str(x.E["doj"])[:7])
		for _, h := range by[x.id()] {
			if str(h["from_month"]) <= upTo {
				out[x.id()] = h["gross"]
				break
			}
		}
	}
	return out, nil
}

// repExits: last working day in the range, tenure in whole months (prototype monthsBetween).
func repExits(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Department", "City", "Date of joining", "Last working day", "Tenure (months)", "Reason", "Status"}
	list, err := repJoin(ctx, s.st.DB, c, f, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: f.From}, {Key: "$lte", Value: f.To}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "status", Value: "Inactive"}}, bson.D{{Key: "exit_status", Value: bson.D{{Key: "$ne", Value: nil}}}}}}})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return str(list[i].E["lwd"]) < str(list[j].E["lwd"]) })
	out := [][]any{}
	for _, x := range list {
		e := x.E
		out = append(out, []any{e["id"], e["name"], e["type"], x.Dept, x.City, e["doj"], e["lwd"], repTenure(str(e["doj"]), str(e["lwd"])), str(e["exit_reason"]), e["status"]})
	}
	return cols, out, nil
}

// repTenure is the whole months from joining to the last working day (never below 0).
func repTenure(doj, lwd string) int32 {
	a, b := mustISO(doj), mustISO(lwd)
	m := (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
	if b.Day() < a.Day() {
		m--
	}
	return int32(max(0, m))
}

// repExitsNote is the attrition line under the exits: exits against the people employed in the period.
func repExitsNote(s *Server, ctx context.Context, c *Ctx, f repFilter, rows [][]any) string {
	f.Status = ""
	employed, _ := repJoin(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}},
		{Key: "doj", Value: bson.D{{Key: "$lte", Value: f.To}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "lwd", Value: nil}}, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: f.From}}}}}}})
	hc := int64(len(employed))
	n := len(rows)
	days := max(1, policy.DayDiff(f.From, f.To)+1)
	pct, ann := 0.0, 0.0
	if hc > 0 {
		pct = float64(n) / float64(hc) * 100
		ann = pct * 365 / float64(days)
	}
	return fmt.Sprintf("%d exit%s against %d people in the period. Attrition %.1f%% for the period, about %.1f%% annualised.", n, map[bool]string{true: "", false: "s"}[n == 1], hc, pct, ann)
}

// repNotice: people serving notice, with the days left to their last working day.
func repNotice(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Department", "City", "Resigned on", "Last working day", "Days left", "Reason"}
	list, err := repJoin(ctx, s.st.DB, c, f, bson.D{{Key: "exit_status", Value: "On notice"}})
	if err != nil {
		return nil, nil, err
	}
	t := today()
	out := [][]any{}
	for _, x := range list {
		e := x.E
		var left any
		if lwd := str(e["lwd"]); lwd != "" {
			left = int32(policy.DayDiff(t, lwd))
		}
		out = append(out, []any{e["id"], e["name"], e["type"], x.Dept, x.City, e["resign_at"], e["lwd"], left, str(e["exit_reason"])})
	}
	return cols, out, nil
}

// repProbation: probation ending in the range, from each person's work policy.
func repProbation(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Department", "City", "Date of joining", "Probation (months)", "Probation ends", "Days from today", "Reporting manager"}
	ids, err := repIDs(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}})
	if err != nil {
		return nil, nil, err
	}
	list, err := repEmps(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	pp, err := repPeople(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	t := today()
	out := [][]any{}
	for _, e := range list {
		p, _ := ts.policyFor(e)
		end := p.S.ProbEnd(e.Grade, e.DOJ) // the grade's probation when it is set, else the policy's
		if end == "" || end < f.From || end > f.To {
			continue
		}
		out = append(out, []any{e.ID, e.Name, e.Type, e.Dept, pp[e.ID].City, e.DOJ, p.S.ProbMonths(e.Grade), end, policy.DayDiff(t, end), pp[e.ID].Manager})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][7].(string) < out[j][7].(string) })
	return cols, out, nil
}

// repBirthdays: birthdays and work anniversaries in the month, by date.
func repBirthdays(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	dob := map[string]string{}
	if len(list) > 0 {
		pers, err := q.C("employee_personal").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}},
			options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "dob", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		for _, p := range pers {
			dob[str(p["employee_id"])] = str(p["dob"])
		}
	}
	y, mm := f.YM[:4], f.YM[5:7]
	out := [][]any{}
	for _, x := range list {
		id, name := x.E["id"], x.E["name"]
		if d := dob[x.id()]; len(d) == 10 && d[5:7] == mm {
			out = append(out, []any{"Birthday", id, name, x.Dept, x.City, y + d[4:], ""})
		}
		if doj := str(x.E["doj"]); len(doj) == 10 && doj[5:7] == mm && doj[:4] < y {
			out = append(out, []any{"Work anniversary", id, name, x.Dept, x.City, y + doj[4:], fmt.Sprintf("%d years", atoiSafe(y)-atoiSafe(doj[:4]))})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][5].(string) < out[j][5].(string) })
	return []string{"Event", "Code", "Name", "Department", "City", "Date", "Years"}, out, nil
}

// repChanges: every change to an employee record effective in the range (the date it takes effect, which every
// change has), newest first, optionally only Applied or Scheduled ones.
func repChanges(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Effective", "Code", "Name", "City", "Department", "Field", "Old value", "New value", "Reason", "Done by", "Done on", "Status"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	filter := bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}, {Key: "effective_from", Value: bson.D{{Key: "$gte", Value: f.From}, {Key: "$lte", Value: f.To}}}}
	if f.DS != "" {
		filter = append(filter, bson.E{Key: "state", Value: f.DS})
	}
	changes, err := q.C("employee_change").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "effective_from", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, x := range changes {
		e := idx[str(x["employee_id"])]
		out = append(out, []any{x["effective_from"], e.E["id"], e.E["name"], e.City, e.Dept, x["field"], str(x["old_value"]), str(x["new_value"]), str(x["reason"]),
			str(x["changed_by"]), cell(x["changed_at"]), x["state"]})
	}
	return cols, out, nil
}

// repNominations: every active employee and nomination that applies to their type; the current
// nomination is the one waiting for Payroll, else the verified one, else the latest (prototype nmCur).
func repNominations(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Location", "Department", "Nomination", "Needed", "Due", "Status", "Version", "Sent", "Verified"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	schemes, err := q.C("nomination_scheme").Find(ctx, bson.M{"on_form": true}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	noms, err := q.C("nomination").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	sent := map[string][]store.Row{} // employee|scheme → nominations, oldest first
	for _, n := range noms {
		k := str(n["employee_id"]) + "|" + fmt.Sprint(n["scheme_id"])
		sent[k] = append(sent[k], n)
	}
	label := map[string]string{"Submitted": "Waiting for Payroll", "Returned": "Returned to you", "Superseded": "Replaced"}
	out := [][]any{}
	for _, x := range list {
		e := x.E
		for _, sc := range schemes {
			if !repHas(sc["types"], str(e["type"])) || (f.DS != "" && fmt.Sprint(sc["id"]) != f.DS) {
				continue
			}
			n, ver := repCurrentNomination(sent[x.id()+"|"+fmt.Sprint(sc["id"])])
			status := "Not submitted"
			if n != nil {
				status = str(n["status"])
			}
			if f.RS != "" && status != f.RS {
				continue
			}
			needed := "Optional"
			if sc["mandatory"] == true {
				needed = "Mandatory"
			}
			var version, at, verified any
			if n != nil {
				version, at = ver, cell(n["submitted_at"])
				if status == "Verified" {
					verified = cell(n["decided_at"])
				}
			}
			out = append(out, []any{e["id"], e["name"], e["type"], x.Loc, x.Dept, sc["name"], needed, policy.AddDays(str(e["doj"]), int(repInt64(sc["due_days"]))),
				or(label[status], status), version, at, verified})
		}
	}
	return cols, out, nil
}

// repCurrentNomination picks the current one of a person's nominations for a scheme (oldest first): the newest
// waiting for Payroll, else the newest verified, else the newest; with its version (its place in the list).
func repCurrentNomination(l []store.Row) (store.Row, int64) {
	for _, want := range []string{"Submitted", "Verified", ""} {
		for i := len(l) - 1; i >= 0; i-- {
			if want == "" || str(l[i]["status"]) == want {
				return l[i], int64(i + 1)
			}
		}
	}
	return nil, 0
}

// repHas reports whether a stored list (text[]) holds v.
func repHas(l any, v string) bool {
	xs, _ := l.([]any)
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// repSeparations: every case with its stage, the clearance of each owner, the exit interview and the
// full and final (prototype seps), in the order the cases were started.
func repSeparations(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Case", "Code", "Name", "Location", "Department", "Type", "Reason", "Started", "Last working day", "Stage",
		"Manager clearance", "HRBP clearance", "Finance clearance", "Exit interview", "Rehire", "Full and final"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	seps, err := q.C("separation").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}},
		options.Find().SetSort(bson.D{{Key: "started_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	cases, err := sepCasesOf(ctx, q, seps)
	if err != nil {
		return nil, nil, err
	}
	if len(cases) == 0 {
		return cols, [][]any{}, nil
	}
	started := map[string]any{}
	for _, x := range seps {
		started[str(x["id"])] = repDateIST(x["started_at"])
	}
	caseIDs, empIDs := []string{}, []string{}
	for _, sc := range cases {
		caseIDs, empIDs = append(caseIDs, sc.ID), append(empIDs, sc.EmployeeID)
	}
	done, err := q.C("exit_interview").Distinct(ctx, "sep_id", bson.D{{Key: "sep_id", Value: repIn(caseIDs)}})
	if err != nil {
		return nil, nil, err
	}
	interviewed := map[string]bool{}
	for _, id := range done {
		interviewed[str(id)] = true
	}
	// The latest full and final of each person that was not rejected: "status amount".
	fnfs, err := q.C("fnf").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(empIDs)}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "Rejected"}}}},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		return nil, nil, err
	}
	fnf := map[string]string{}
	for _, x := range fnfs {
		if id := str(x["employee_id"]); fnf[id] == "" {
			net, _ := x["net"].(float64)
			fnf[id] = str(x["status"]) + " " + strconv.FormatInt(int64(math.Round(net)), 10)
		}
	}
	out := [][]any{}
	for _, sc := range cases {
		e := idx[sc.EmployeeID]
		st, _ := s.stage(ctx, q, sc)
		if st == "Notice" {
			st = "Notice period"
		}
		items, err := clearanceOf(ctx, q, sc)
		if err != nil {
			return nil, nil, err
		}
		open := map[string]bool{}
		for _, it := range items {
			if !it.Done {
				open[it.Owner] = true
			}
		}
		clear := func(owner string) string {
			if open[owner] {
				return "Open"
			}
			return "Done"
		}
		exit := "Not needed"
		if interviewed[sc.ID] {
			exit = "Done"
		} else if exitNeededQ(ctx, q, sc.Type) {
			exit = "Pending"
		}
		out = append(out, []any{sc.ID, sc.EmployeeID, e.E["name"], e.Loc, e.Dept, sc.Type, sc.Reason, started[sc.ID], sc.lwd(), st,
			clear("Reporting manager"), clear("HRBP"), clear("Finance"), exit, sv(sc.Rehire), fnf[sc.EmployeeID]})
	}
	return cols, out, nil
}

// repExitInterviews: the interview of every case, one column per rated area (prototype EXQ), in the order the
// cases were started.
func repExitInterviews(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Location", "Department", "Reporting manager", "Last working day", "Main reason", "Would recommend"}
	cols = append(append(cols, exitQs...), "Comments")
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	mgr, err := repManagers(ctx, q, list)
	if err != nil {
		return nil, nil, err
	}
	seps, err := q.C("separation").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}},
		options.Find().SetSort(bson.D{{Key: "started_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	ivs, err := q.C("exit_interview").Find(ctx, bson.D{{Key: "sep_id", Value: repIn(repCol(seps, "id"))}})
	if err != nil {
		return nil, nil, err
	}
	byCase := map[string]store.Row{}
	for _, x := range ivs {
		byCase[str(x["sep_id"])] = x
	}
	out := [][]any{}
	for _, sp := range seps {
		x := byCase[str(sp["id"])]
		if x == nil {
			continue
		}
		e := idx[str(sp["employee_id"])].E
		// The employee's last working day once they are on notice or gone, else the case's.
		lwd := sp["lwd"]
		if (e["status"] == "Inactive" || e["exit_status"] == "On notice") && e["lwd"] != nil {
			lwd = e["lwd"]
		}
		row := []any{e["id"], e["name"], idx[str(e["id"])].Loc, idx[str(e["id"])].Dept, mgr[str(e["id"])], lwd, x["main_reason"], str(x["recommend"])}
		ratings, _ := x["ratings"].(map[string]any)
		for _, area := range exitQs {
			v, err := repJSONFloat(ratings[area])
			if err != nil {
				return nil, nil, err
			}
			row = append(row, v)
		}
		out = append(out, append(row, str(x["comments"])))
	}
	return cols, out, nil
}

// ---------- Attendance and roster ----------

// repAttMonth is the monthly attendance summary, or the late coming report (active people with late
// days, most first, with late days as a share of the days worked).
func repAttMonth(late bool) func(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	return func(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
		ym := f.YM[:7]
		ids, err := repIDs(ctx, s.st.DB, c, f, nil)
		if err != nil {
			return nil, nil, err
		}
		pc, list, err := loadPayCtx(ctx, s.st.DB, ym, bson.D{{Key: "_id", Value: repIn(ids)}})
		if err != nil {
			return nil, nil, err
		}
		pp := map[string]repPerson{}
		if late {
			if pp, err = repPeople(ctx, s.st.DB, ids); err != nil {
				return nil, nil, err
			}
		}
		out := [][]any{}
		for _, e := range list {
			p, _ := pc.ts.policyFor(e)
			mo := pc.md.span(e, p, pc.from, pc.to)
			if late {
				n := mo.Counts["Late"]
				if e.Status != "Active" || n == 0 {
					continue
				}
				share := "0%"
				if w := mo.Counts["Present"] + n + mo.Counts["Half day"]; w > 0 {
					share = fmt.Sprintf("%d%%", int(math.Round(float64(n)/float64(w)*100)))
				}
				out = append(out, []any{e.ID, e.Name, e.Location, e.Dept, e.Shift, n, share, pp[e.ID].Manager})
				continue
			}
			out = append(out, []any{e.ID, e.Name, pc.cities[e.CityID], e.Dept, mo.Counts["Present"], mo.Counts["Late"], mo.Counts["Half day"],
				mo.Counts["Missing punch"], mo.Counts["No punch"], mo.LOP, mo.Outside})
		}
		if late {
			sort.SliceStable(out, func(i, j int) bool { return out[i][5].(int) > out[j][5].(int) })
			return []string{"Code", "Name", "Location", "Department", "Shift", "Late days", "Late share of working days", "Reporting manager"}, out, nil
		}
		return []string{"Code", "Name", "City", "Department", "Present", "Late", "Half day", "Missing punch", "No punch", "Loss of pay days", "Days after last working day"}, out, nil
	}
}

// repLeave: casual, privilege and sick leave balance and use as on today ("n/a" where a type does
// not apply to the person).
func repLeave(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Department", "City", "CL balance", "CL used", "PL balance", "PL used", "SL balance", "SL used"}
	ids, err := repIDs(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	list, err := repEmps(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	pp, err := repPeople(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	on := today()
	out := [][]any{}
	for _, e := range list {
		p, _ := ts.policyFor(e)
		pl := ts.plan(p)
		if pl == nil {
			continue
		}
		row, some := []any{e.ID, e.Name, e.Dept, pp[e.ID].City}, false
		for _, name := range []string{"Casual leave", "Privilege leave", "Sick leave"} {
			cfg := ts.typeCfg(p, name)
			lt, ok := ts.types[name]
			if !ok || !typeAvail(cfg, lt, e) || lt.Kind != leave.KindBalance {
				row = append(row, "n/a", 0)
				continue
			}
			ap, pe, err := takenOf(ctx, s.st.DB, e.ID, name)
			if err != nil {
				return nil, nil, err
			}
			b := leave.Compute(*cfg, pl.LV, p.S.ProbMonths(e.Grade), e.DOJ, on, openingOf(ctx, s.st.DB, e.ID, name, on), ap, pe)
			row, some = append(row, b.Bal, b.Used), true
		}
		// The prototype's canLeave: the employment type takes leave, or some leave type applies.
		if !some && !calc.RulesFor(calc.EmploymentType(e.Type)).Leave {
			continue
		}
		out = append(out, row)
	}
	return cols, out, nil
}

// repRosterChanges: roster changes in the month (not the regularizations from deviations), late
// ones only on request, with whether each was late; the place and department are those recorded with
// the change, else the employee's.
func repRosterChanges(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Date", "Code", "Name", "Location", "Department", "From shift", "To shift", "Changed by", "Timing", "Reason"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	ym := f.YM[:7]
	filter := bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}, {Key: "date", Value: bson.D{{Key: "$gte", Value: ym + "-01"}, {Key: "$lt", Value: nextYm(ym) + "-01"}}},
		{Key: "by_dev", Value: false}}
	if f.Late != "" {
		filter = append(filter, bson.E{Key: "late", Value: true})
	}
	logs, err := q.C("roster_log").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "date", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	places, err := repByID(ctx, q, "location", repCol(logs, "location_id"))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, rl := range logs {
		e := idx[str(rl["employee_id"])]
		loc, dept := e.Loc, e.Dept
		if l := places[str(rl["location_id"])]; l != nil {
			loc = str(l["name"])
		}
		if d, ok := rl["department"].(string); ok {
			dept = d
		}
		timing := "In time"
		if rl["late"] == true {
			timing = "Late"
		}
		out = append(out, []any{rl["date"], e.E["id"], e.E["name"], loc, dept, str(rl["from_code"]), rl["to_code"], str(rl["by_name"]), timing, str(rl["why"])})
	}
	return cols, out, nil
}

// repRosterNote names the places above their late change limit (prototype rosAlerts).
func repRosterNote(s *Server, ctx context.Context, c *Ctx, f repFilter, rows [][]any) string {
	al, err := s.rosterAlerts(ctx, c, f.YM[:7])
	if err != nil || len(al) == 0 {
		return "No location is above its late change limit."
	}
	l := []string{}
	for _, a := range al {
		l = append(l, fmt.Sprintf("%v %v%% late (limit %v%%)", a["key"], a["pct"], a["limit"]))
	}
	return "Alerts: " + strings.Join(l, "; ")
}

// repDeviations: planned shift against actual punches with the decision, Pending while no one has
// decided (prototype devList). Days before the first attendance on record are not looked at.
func repDeviations(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Date", "Code", "Name", "Location", "Department", "Reporting manager", "Planned", "Actual", "In", "Out", "Hours", "What differs", "Decision", "Decided by", "Note"}
	ids, err := repIDs(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}})
	if err != nil || len(ids) == 0 {
		return cols, [][]any{}, err
	}
	from, to := f.From, f.To
	if t := today(); to > t {
		to = t
	}
	// The first day on record for these people: their earliest attendance or deviation decision.
	first := ""
	for _, coll := range []string{"attendance_day", "deviation_decision"} {
		r, err := s.st.DB.C(coll).FindOne(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}},
			options.FindOne().SetSort(bson.D{{Key: "date", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "date", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		if d := str(r["date"]); d != "" && (first == "" || d < first) {
			first = d
		}
	}
	if first == "" {
		return cols, [][]any{}, nil
	}
	if from < first {
		from = first
	}
	if from > to {
		return cols, [][]any{}, nil
	}
	list, err := repEmps(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids, from, to)
	if err != nil {
		return nil, nil, err
	}
	pp, err := repPeople(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, e := range list {
		p, _ := ts.policyFor(e)
		start := from
		if e.DOJ > start {
			start = e.DOJ
		}
		for d := start; d <= to; d = policy.AddDays(d, 1) {
			v, x := m.deviation(e, p, d)
			if v == nil {
				continue
			}
			st, by, note := "Pending", "", ""
			if x != nil {
				st, by, note = x.Status, x.By, x.Note
			}
			if f.DS != "" && f.DS != st {
				continue
			}
			actual := v.Actual
			if actual == "Custom" {
				actual = "Other time"
			}
			out = append(out, []any{d, e.ID, e.Name, e.Location, e.Dept, pp[e.ID].Manager, v.Planned, actual, v.In, v.Out, v.Hours, strings.Join(v.Kinds, ", "), st, by, note})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0].(string) > out[j][0].(string)
		}
		return out[i][2].(string) < out[j][2].(string)
	})
	return cols, out, nil
}

// repScore is one person's month for the discipline scores (prototype discScore).
type repScore struct {
	Days, Dev, OK, No, Pend int
	discScore
}

func repScores(ctx context.Context, q store.Q, ts *timeSet, list []*emp, ym string) (map[string]repScore, error) {
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, q, ts, ids(list), from, to)
	if err != nil {
		return nil, err
	}
	t := today()
	out := map[string]repScore{}
	for _, e := range list {
		p, _ := ts.policyFor(e)
		var r repScore
		for d := from; d <= to && d <= t; d = policy.AddDays(d, 1) {
			if d < e.DOJ || (e.LWD != nil && d > *e.LWD) {
				continue
			}
			if c := m.planned(e, p, d).Code; c != "WO" && c != "PH" && c != "L" {
				r.Days++
			}
			v, x := m.deviation(e, p, d)
			if v == nil {
				continue
			}
			r.Dev++
			switch {
			case x == nil:
				r.Pend++
			case x.Status == "OK":
				r.OK++
			default:
				r.No++
			}
		}
		r.discScore = scoreFor(e, p, ym, r.No)
		out[e.ID] = r
	}
	return out, nil
}

// repDiscScore: each active person's score for the month, lowest first.
func repDiscScore(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Location", "Department", "Reporting manager", "Rostered days", "Deviations", "OK", "Not OK", "Waiting", "Started at", "Score", "Band"}
	ids, err := repIDs(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	list, err := repEmps(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	sc, err := repScores(ctx, s.st.DB, ts, list, f.YM[:7])
	if err != nil {
		return nil, nil, err
	}
	pp, err := repPeople(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, e := range list {
		r := sc[e.ID]
		start := any(r.Start)
		if r.Note != "" {
			start = fmt.Sprintf("%d (%s)", r.Start, r.Note)
		}
		out = append(out, []any{e.ID, e.Name, e.Location, e.Dept, pp[e.ID].Manager, r.Days, r.Dev, r.OK, r.No, r.Pend, start, r.Score, r.Band})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i][11] != out[j][11] {
			return out[i][11].(int) < out[j][11].(int)
		}
		return out[i][6].(int) > out[j][6].(int)
	})
	return cols, out, nil
}

// repMgrScore: how well each manager's rosters match how the team works: deviation share of
// rostered days and late roster changes, flagged above the policy's limit (prototype mgrScores).
func repMgrScore(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Manager", "Team", "Rostered days", "Deviations", "Deviation share %", "Marked OK", "Marked Not OK", "Waiting", "Late roster changes", "All roster changes", "Score", "Flag"}
	ym := f.YM[:7]
	ids, err := repIDs(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}, {Key: "manager_id", Value: bson.D{{Key: "$ne", Value: nil}}}})
	if err != nil {
		return nil, nil, err
	}
	list, err := repEmps(ctx, s.st.DB, ids)
	if err != nil {
		return nil, nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	sc, err := repScores(ctx, s.st.DB, ts, list, ym)
	if err != nil {
		return nil, nil, err
	}
	// Each person's roster changes in the month (not regularizations), and how many were late.
	changes := map[string][2]int{}
	logs, err := s.st.DB.C("roster_log").Find(ctx, bson.D{{Key: "date", Value: bson.D{{Key: "$gte", Value: ym + "-01"}, {Key: "$lt", Value: nextYm(ym) + "-01"}}},
		{Key: "by_dev", Value: false}, {Key: "employee_id", Value: repIn(ids)}}, options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "late", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	for _, x := range logs {
		n := changes[str(x["employee_id"])]
		n[0]++
		if x["late"] == true {
			n[1]++
		}
		changes[str(x["employee_id"])] = n
	}
	type mgr struct {
		ID                                           string
		Team, Days, Dev, OK, No, Pend, Chg, Late, Al int
	}
	byID, order := map[string]*mgr{}, []*mgr{}
	for _, e := range list {
		g := byID[*e.ManagerID]
		if g == nil {
			p, _ := ts.policyFor(e)
			g = &mgr{ID: *e.ManagerID, Al: p.S.RS.MgrAlert}
			byID[g.ID], order = g, append(order, g)
		}
		r := sc[e.ID]
		g.Team++
		g.Days += r.Days
		g.Dev += r.Dev
		g.OK += r.OK
		g.No += r.No
		g.Pend += r.Pend
		g.Chg += changes[e.ID][0]
		g.Late += changes[e.ID][1]
	}
	mids := []string{}
	for _, g := range order {
		mids = append(mids, g.ID)
	}
	names, err := repNames(ctx, s.st.DB, mids)
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, g := range order {
		share := 0.0
		if g.Days > 0 {
			share = math.Round(float64(g.Dev)/float64(g.Days)*1000) / 10
		}
		lateShare := 0.0
		if g.Chg > 0 {
			lateShare = math.Round(float64(g.Late) / float64(g.Chg) * 100)
		}
		flag := ""
		if share > float64(g.Al) {
			flag = "Roster not matching the team"
		}
		score := int(math.Max(0, math.Round(100-2*share-lateShare/2)))
		out = append(out, []any{names[g.ID], g.Team, g.Days, g.Dev, share, g.OK, g.No, g.Pend, g.Late, g.Chg, score, flag})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][10].(int) < out[j][10].(int) })
	return cols, out, nil
}

// repConsequences: every warning letter and improvement plan issued in the range, with where it stands.
func repConsequences(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Date", "Code", "Name", "Location", "Department", "Type", "Number", "PIP days", "Reason", "Issued by", "Approver role", "Decided by", "Plan from", "Plan to",
		"Status", "Result"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	filter := bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}, {Key: "issued_at", Value: bson.D{{Key: "$gte", Value: f.From}, {Key: "$lte", Value: f.To}}}}
	if f.DS != "" {
		filter = append(filter, bson.E{Key: "kind", Value: f.DS})
	}
	xs, err := q.C("consequence").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	t := today()
	out := [][]any{}
	for _, x := range xs {
		e := idx[str(x["employee_id"])]
		var days any
		if n := repInt64(x["days"]); n != 0 {
			days = x["days"]
		}
		status := str(x["status"])
		switch {
		case status == "Active" && x["to_date"] != nil && str(x["to_date"]) < t:
			status = "Review due"
		case status == "Pending approval":
			status = "Waiting for " + or(str(x["approver_role"]), "approval")
		case status == "Completed":
			if x["outcome"] == "Improved" {
				status = "Completed, improved"
			} else {
				status = "Completed, not improved"
			}
		}
		out = append(out, []any{x["issued_at"], e.E["id"], e.E["name"], e.Loc, e.Dept, x["kind"], x["seq"], days, x["reason"], str(x["issued_by"]), str(x["approver_role"]),
			str(x["decided_by"]), x["from_date"], x["to_date"], status, str(x["outcome"])})
	}
	return cols, out, nil
}

// ---------- Payroll ----------

// repPayRows are the month's stored pay rows in the caller's scope that match the filters.
func repPayRows(ctx context.Context, q store.Q, c *Ctx, f repFilter) ([]*PayRowOut, error) {
	all, err := loadRows(ctx, q, f.YM[:7])
	if err != nil {
		return nil, err
	}
	return filterRows(c, all, map[string]string{"city": f.City, "dept": f.Dept, "type": f.Type}), nil
}

// erContrib is the employer's contributions on a pay row: PF (with EPFO admin and EDLI), ESI,
// medical insurance and LWF (prototype payroll cost).
func erContrib(o *PayRowOut) float64 {
	r := o.R
	return r.ErPF + r.Admin + r.Edli + r.ErESI + r.Med + r.LwfEr
}

func repRegister(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	rows, err := repPayRows(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, o := range rows {
		hold := ""
		if o.Hold {
			hold = "On hold"
		}
		// The payroll register's figures, so a row foots: earnings − deductions + rounding off = net pay (ITM-59).
		fg := figuresOf(o)
		out = append(out, []any{o.EmployeeID, o.Name, o.Type, o.City, o.Paid, fg.TotE, fg.PF, fg.ESI, fg.Tds,
			fg.TotD, fg.RoundOff, fg.Net, hold})
	}
	return []string{"Code", "Name", "Type", "City", "Paid days", "Total earnings", "PF", "ESI", "TDS", "Total deductions", "Rounding off", "Net pay", "Bank"}, out, nil
}

// repCost: the month's cost by city, location, department, type, grade or gender, highest first.
func repCost(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	rows, err := repPayRows(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, nil, err
	}
	gender := map[string]string{}
	if f.Group == "gender" && len(rows) > 0 {
		ids := make([]string, len(rows))
		for i, o := range rows {
			ids[i] = o.EmployeeID
		}
		gr, err := s.st.DB.C("employee_personal").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}},
			options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "gender", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		for _, g := range gr {
			gender[str(g["employee_id"])] = str(g["gender"])
		}
	}
	key := func(o *PayRowOut) string {
		switch f.Group {
		case "location", "loc":
			return o.Location
		case "department", "dept":
			return o.Dept
		case "type":
			return o.Type
		case "grade":
			return o.Grade
		case "gender":
			return or(gender[o.EmployeeID], "Not recorded")
		}
		return o.City
	}
	type agg struct {
		K         string
		N         int
		Earn, Emp float64
	}
	by, order := map[string]*agg{}, []*agg{}
	for _, o := range rows {
		k := key(o)
		a := by[k]
		if a == nil {
			a = &agg{K: k}
			by[k], order = a, append(order, a)
		}
		a.N++
		a.Earn += o.TotalEarn
		a.Emp += erContrib(o)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Earn+order[i].Emp > order[j].Earn+order[j].Emp })
	out := [][]any{}
	for _, a := range order {
		out = append(out, []any{a.K, a.N, math.Round(a.Earn), math.Round(a.Emp), math.Round(a.Earn + a.Emp)})
	}
	return []string{"Group", "People paid", "Gross earnings", "Employer contributions", "Total cost to company"}, out, nil
}

// repStatutory: PF, ESI, LWF, TDS and medical insurance to pay, by city. EPFO admin includes EDLI.
func repStatutory(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	rows, err := repPayRows(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, nil, err
	}
	by, order := map[string][]float64{}, []string{}
	for _, o := range rows {
		v := by[o.City]
		if v == nil {
			v = make([]float64, 9)
			order = append(order, o.City)
		}
		r := o.R
		for i, x := range []float64{r.EePF, r.ErPF, r.Admin + r.Edli, r.EeESI, r.ErESI, r.LwfEe, r.LwfEr, r.Tds + o.STds, r.Med} {
			v[i] += x
		}
		by[o.City] = v
	}
	out := [][]any{}
	for _, ct := range order {
		row := []any{ct}
		for _, x := range by[ct] {
			row = append(row, math.Round(x))
		}
		out = append(out, row)
	}
	return []string{"City", "Employee PF", "Employer PF", "EPFO admin", "Employee ESI", "Employer ESI", "LWF employee", "LWF employer", "TDS", "Medical insurance"}, out, nil
}

// repBank: net pay by account (bank name and account number) for an approved month, held salaries left out.
func repBank(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Bank account", "IFSC", "Net pay"}
	ym := f.YM[:7]
	if m, err := loadPayMonth(ctx, s.st.DB, ym, false); err != nil || m == nil || (m.Status != "Approved" && m.Status != "Published") {
		return cols, [][]any{}, err
	}
	rows, err := repPayRows(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, len(rows))
	for i, o := range rows {
		ids[i] = o.EmployeeID
	}
	// Each person's account in force for the month: the latest from this month or earlier.
	bl, err := s.st.DB.C("employee_bank").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}, {Key: "from_month", Value: bson.D{{Key: "$lte", Value: ym}}}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		return nil, nil, err
	}
	banks := map[string][2]string{}
	for _, b := range bl {
		id := str(b["employee_id"])
		if _, seen := banks[id]; !seen {
			banks[id] = [2]string{strings.Trim(str(b["bank_name"])+" "+str(b["account_no"]), " "), str(b["ifsc"])}
		}
	}
	out := [][]any{}
	for _, o := range rows {
		if o.Hold || o.Net <= 0 {
			continue
		}
		b := banks[o.EmployeeID]
		out = append(out, []any{o.EmployeeID, o.Name, b[0], b[1], round0(o.Net)})
	}
	return cols, out, nil
}

// repBankNote says why the bank advice is empty before the month is approved.
func repBankNote(s *Server, ctx context.Context, c *Ctx, f repFilter, rows [][]any) string {
	ym := f.YM[:7]
	if m, err := loadPayMonth(ctx, s.st.DB, ym, false); err == nil && m != nil && (m.Status == "Approved" || m.Status == "Published") {
		return ""
	}
	return "The bank advice is ready once the payroll for " + ymLabel(ym) + " is approved."
}

// repLedger: the month's extra pay lines (not voided), in the order they were added.
func repLedger(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Kind", "Item", "Amount", "Source", "Reason"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	lines, err := q.C("ledger").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}, {Key: "void", Value: false}, {Key: "ym", Value: f.YM[:7]}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, x := range lines {
		e := idx[str(x["employee_id"])]
		kind := "Deduction"
		if x["kind"] == "earn" {
			kind = "Earning"
		}
		out = append(out, []any{e.E["id"], e.E["name"], kind, x["label"], x["amount"], str(x["source_id"]), str(x["reason"])})
	}
	return cols, out, nil
}

// repRetainer: people paid without payslips or leave (every type but permanent).
func repRetainer(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	rows, err := repPayRows(ctx, s.st.DB, c, f)
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, o := range rows {
		if o.Type != "Permanent" {
			out = append(out, []any{o.EmployeeID, o.Name, o.Type, o.City, o.Paid, round0(o.TotalEarn), round0(o.R.Tds + o.STds), round0(o.Net)})
		}
	}
	return []string{"Code", "Name", "Type", "City", "Paid days", "Amount", "TDS", "Net payment"}, out, nil
}

// repDeductions: every manual deduction request, in the order raised, with its details from the request.
func repDeductions(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Deduction", "Code", "Name", "Type", "Amount (₹)", "Instalments", "First month", "Status", "Employee heard", "Raised by", "Raised on", "Details"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	reqs, err := q.C("approval_request").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}, {Key: "wf", Value: "p_ded"}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	raisers, err := repNames(ctx, q, repCol(reqs, "raised_by"))
	if err != nil {
		return nil, nil, err
	}
	text := func(p map[string]any, k string) string { s, _ := repJSONText(p[k]); return s }
	out := [][]any{}
	for _, r := range reqs {
		e := idx[str(r["employee_id"])]
		p, _ := r["payload"].(map[string]any)
		amount, err := repJSONFloat(p["amount"])
		if err != nil {
			return nil, nil, err
		}
		inst, err := repJSONInt(p["instalments"])
		if err != nil {
			return nil, nil, err
		}
		by, ok := raisers[str(r["raised_by"])]
		if !ok {
			by = str(r["raised_by"])
		}
		out = append(out, []any{r["id"], e.E["id"], e.E["name"], text(p, "dedType"), amount, inst, text(p, "startYm"), r["status"], text(p, "heard"), by,
			repDateIST(r["created_at"]), text(p, "note")})
	}
	return cols, out, nil
}

// repHolds: every hold with the amount held (months run while it is active) or paid out on release.
func repHolds(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Hold", "Code", "Name", "Reason", "Details", "From", "Held (₹)", "Status", "Raised by", "Raised on", "Released by", "Released on", "Release note"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	holds, err := q.C("salary_hold").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}},
		options.Find().SetSort(bson.D{{Key: "at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	rows := [][]any{}
	for _, h := range holds {
		e := idx[str(h["employee_id"])]
		// What the release paid out: the amounts of the paid list.
		paid := 0.0
		if l, ok := h["paid"].([]any); ok {
			for _, p := range l {
				m, _ := p.(map[string]any)
				v, err := repJSONFloat(m["amount"])
				if err != nil {
					return nil, nil, err
				}
				if x, ok := v.(float64); ok {
					paid += x
				}
			}
		}
		rows = append(rows, []any{h["id"], e.E["id"], e.E["name"], h["reason"], str(h["note"]), h["from_ym"], paid, h["status"], str(h["by_name"]), repDateIST(h["at"]),
			str(h["released_by"]), repDateIST(h["released_at"]), str(h["release_note"])})
	}
	for _, r := range rows {
		if r[7] == "Active" {
			amt, err := heldAmount(ctx, q, r[1].(string), strings.TrimSpace(str(r[5])))
			if err != nil {
				return nil, nil, err
			}
			r[6] = math.Round(amt)
		}
	}
	return cols, rows, nil
}

// repTds: Section 192 for salaried staff (yearly tax on the regime, deducted so far, this month and
// still to deduct) and Section 194J for retainers (deducted so far and this month).
func repTds(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Section", "Regime", "Yearly tax", "Deducted this year", "TDS for the month", "Still to deduct"}
	ym := f.YM[:7]
	fy := calc.FY(ym)
	ids, err := repIDs(ctx, s.st.DB, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, ym, false, fyBase(fy), bson.D{{Key: "_id", Value: repIn(ids)}})
	if err != nil {
		return nil, nil, err
	}
	months := calc.FYMonths(fy)
	fee := map[string]map[string]float64{}
	fr, err := s.st.DB.C("tds_history").Find(ctx, bson.D{{Key: "section", Value: "194J"}, {Key: "ym", Value: bson.D{{Key: "$gte", Value: months[0]}, {Key: "$lte", Value: months[11]}}},
		{Key: "employee_id", Value: repIn(ids)}})
	if err != nil {
		return nil, nil, err
	}
	for _, x := range fr {
		id := str(x["employee_id"])
		if fee[id] == nil {
			fee[id] = map[string]float64{}
		}
		fee[id][strings.TrimSpace(str(x["ym"]))] = x["amount"].(float64)
	}
	// This month's figures not yet in the TDS history come from the month's pay rows.
	run := map[string]*PayRowOut{}
	if rows, err := loadRows(ctx, s.st.DB, ym); err == nil {
		for _, o := range rows {
			run[o.EmployeeID] = o
		}
	}
	m, err := loadPayMonth(ctx, s.st.DB, ym, false)
	if err != nil {
		return nil, nil, err
	}
	closed := (m != nil && m.Status == "Published") || ym < openYm(ctx, s.st.DB)
	out := [][]any{}
	for _, e := range list {
		switch {
		case salaryTdsType(e.Type):
			fig := pc.figureFor(e, fy)
			if fig == nil {
				continue
			}
			d := pc.tax.declOf(e.ID)
			hist := pc.tax.hist[e.ID]
			done := 0.0
			for _, v := range hist {
				done += v
			}
			month, ok := hist[ym]
			if !ok && !closed {
				month = monthTds(fig, d, hist, ym)
			}
			out = append(out, []any{e.ID, e.Name, e.Type, "192", d.Regime, math.Round(fig.Annual), math.Round(done), math.Round(month), math.Max(0, math.Round(fig.Annual-done-d.PrevTDS))})
		case calc.RulesFor(calc.EmploymentType(e.Type)).Ded == "tds":
			done := 0.0
			for _, v := range fee[e.ID] {
				done += v
			}
			month, ok := fee[e.ID][ym]
			if o := run[e.ID]; !ok && !closed && o != nil {
				month = o.R.Tds
			}
			out = append(out, []any{e.ID, e.Name, e.Type, "194J", "—", nil, math.Round(done), math.Round(month), nil})
		}
	}
	return cols, out, nil
}

// ---------- Approvals ----------

// repReq is an approval request with its subject (an employee, an offer or a shortlisted candidate: the old
// approval_subject view) and the names of the subject's department and city.
type repReq struct {
	R, E       store.Row
	Dept, City string
}

// repRequests lists the approval requests matching filter, in sort order, joined with their subject and the
// subject's department, location, cluster and city (a request missing one is left out, as the inner joins did),
// inside the caller's data scope and the report's filters on the subject.
func repRequests(ctx context.Context, q store.Q, c *Ctx, f repFilter, filter bson.D, order bson.D) ([]repReq, error) {
	if f.Status == "On notice" {
		// The subject has no exit status: the old query failed on this filter.
		return nil, fmt.Errorf("column e.exit_status does not exist")
	}
	reqs, err := q.C("approval_request").Find(ctx, filter, options.Find().SetSort(order))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(reqs))
	for _, r := range reqs {
		ids = append(ids, approvalSubjectID(r))
	}
	subs, err := approvalSubjectsOf(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	subRows := make([]store.Row, 0, len(subs))
	for _, e := range subs {
		subRows = append(subRows, e)
	}
	depts, err := repByID(ctx, q, "department", repCol(subRows, "department_id"))
	if err != nil {
		return nil, err
	}
	pl, err := repPlacesOf(ctx, q, subRows)
	if err != nil {
		return nil, err
	}
	out := []repReq{}
	for _, r := range reqs {
		e := subs[approvalSubjectID(r)]
		if e == nil {
			continue
		}
		d := depts[str(e["department_id"])]
		_, ci := pl.of(e)
		if d == nil || ci == nil {
			continue
		}
		dept := str(d["name"])
		if !c.Scope.Allows(access.ScopedEmployee{ID: str(e["id"]), LocationID: str(e["location_id"]), Department: dept, ManagerID: str(e["manager_id"])}) {
			continue
		}
		if (f.City != "" && str(ci["id"]) != f.City) || (f.Dept != "" && dept != f.Dept) || (f.Type != "" && str(e["type"]) != f.Type) ||
			(f.Status != "" && str(e["status"]) != f.Status) {
			continue
		}
		out = append(out, repReq{R: r, E: e, Dept: dept, City: str(ci["name"])})
	}
	return out, nil
}

// repStageRole is the role of the stage a request waits on (chain[step]), false when there is none.
func repStageRole(r store.Row) (string, bool) {
	chain, _ := r["chain"].([]any)
	step := int(repInt64(r["step"]))
	if step < 0 || step >= len(chain) {
		return "", false
	}
	return repJSONText(chain[step])
}

// repApprovals: every request raised in the range (joining requests by the candidate's place) with its stage and
// result, newest first.
func repApprovals(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Request", "Workflow", "Employee", "Department", "City", "Status", "Waiting on", "Stage", "Raised"}
	filter := bson.D{{Key: "created_at", Value: repISTRange(f.From, f.To)}}
	if f.WF != "" {
		filter = append(filter, bson.E{Key: "wf", Value: f.WF})
	}
	if f.RS != "" {
		filter = append(filter, bson.E{Key: "status", Value: f.RS})
	}
	list, err := repRequests(ctx, s.st.DB, c, f, filter, bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: 1}})
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, x := range list {
		r := x.R
		waiting := ""
		if r["status"] == "Pending" {
			waiting, _ = repStageRole(r)
		}
		stage := ""
		if chain, _ := r["chain"].([]any); len(chain) > 0 {
			stage = fmt.Sprintf("%d/%d", repInt64(r["step"]), len(chain))
		}
		out = append(out, []any{r["id"], repWfLabel(str(r["wf"])), x.E["name"], x.Dept, x.City, r["status"], waiting, stage, cell(r["created_at"])})
	}
	return cols, out, nil
}

// repWorkload: pending requests by the stage they wait on, with how many of each workflow.
func repWorkload(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	list, err := repRequests(ctx, s.st.DB, c, f, bson.D{{Key: "status", Value: "Pending"}}, bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}})
	if err != nil {
		return nil, nil, err
	}
	type grp struct {
		Stage string
		N     int
		By    map[string]int
		Order []string
	}
	by, order := map[string]*grp{}, []*grp{}
	for _, x := range list {
		st, ok := repStageRole(x.R)
		if !ok {
			st = "—"
		}
		wf := repWfLabel(str(x.R["wf"]))
		g := by[st]
		if g == nil {
			g = &grp{Stage: st, By: map[string]int{}}
			by[st], order = g, append(order, g)
		}
		if g.By[wf] == 0 {
			g.Order = append(g.Order, wf)
		}
		g.By[wf]++
		g.N++
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].N > order[j].N })
	out := [][]any{}
	for _, g := range order {
		parts := []string{}
		for _, wf := range g.Order {
			parts = append(parts, fmt.Sprintf("%s %d", wf, g.By[wf]))
		}
		out = append(out, []any{g.Stage, g.N, strings.Join(parts, ", ")})
	}
	return []string{"Waiting on", "Pending requests", "Of which"}, out, nil
}

// ---------- Compliance and audit ----------

// repMissing: what each person still has to give: PAN, Aadhaar, bank, date of birth and mobile, the
// UAN where PF applies and the ESIC number where ESI applies (from their salary), most missing first.
func repMissing(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Department", "City", "Status", "Missing", "Count"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}})
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return cols, [][]any{}, nil
	}
	ids := repEmpIDs(list)
	byEmp := func(coll string) (map[string]store.Row, error) {
		rows, err := q.C(coll).Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}})
		if err != nil {
			return nil, err
		}
		out := map[string]store.Row{}
		for _, r := range rows {
			out[str(r["employee_id"])] = r
		}
		return out, nil
	}
	gov, err := byEmp("employee_gov_id")
	if err != nil {
		return nil, nil, err
	}
	pers, err := byEmp("employee_personal")
	if err != nil {
		return nil, nil, err
	}
	withAcct, err := q.C("employee_bank").Distinct(ctx, "employee_id", bson.D{{Key: "employee_id", Value: repIn(ids)}, {Key: "account_no", Value: bson.D{{Key: "$nin", Value: bson.A{nil, ""}}}}})
	if err != nil {
		return nil, nil, err
	}
	bank := map[string]bool{}
	for _, id := range withAcct {
		bank[str(id)] = true
	}
	ym := today()[:7]
	pc, _, err := loadPayCtxOpt(ctx, q, ym, false, nil, bson.D{{Key: "_id", Value: repIn(ids)}})
	if err != nil {
		return nil, nil, err
	}
	none := func(v any) bool { x := strings.TrimSpace(str(v)); return x == "" || strings.EqualFold(x, "Pending") }
	out := [][]any{}
	for _, x := range list {
		id, e, g, p := x.id(), x.E, gov[x.id()], pers[x.id()]
		m := []string{}
		if none(g["pan"]) {
			m = append(m, "PAN")
		}
		if none(g["aadhaar"]) {
			m = append(m, "Aadhaar")
		}
		if !bank[id] {
			m = append(m, "Bank")
		}
		if p["dob"] == nil {
			m = append(m, "Date of birth")
		}
		if none(p["mobile"]) {
			m = append(m, "Mobile")
		}
		gross := pc.grossAt(id, ym)
		if gross == 0 {
			gross = pc.grossAt(id, str(e["doj"])[:7])
		}
		res := calc.Calc(calc.Input{Type: calc.EmploymentType(str(e["type"])), Gross: gross, Structure: pc.structure(pc.structureOn(id, ym+"-01")), PfExempt: pc.pay[id].PfExempt})
		if res.EePF > 0 && !reUAN.MatchString(statIDClean(str(g["uan"]))) { // blank, "No" or not 12 digits (REAL-39)
			m = append(m, "UAN (PF applies)")
		}
		if res.EsiOn && !reESIC.MatchString(statIDClean(str(g["esic"]))) {
			m = append(m, "ESIC number (ESI applies)")
		}
		if len(m) > 0 {
			out = append(out, []any{id, e["name"], e["type"], x.Dept, x.City, e["status"], strings.Join(m, ", "), len(m)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i][7].(int) > out[j][7].(int) })
	return cols, out, nil
}

// repMasters: every master including inactive ones (inactive first), with what it sits under and the
// active employees still using it.
func repMasters(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	q := s.st.DB
	all := func(coll string, filter any) ([]store.Row, error) {
		return q.C(coll).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	}
	active, err := q.C("employee").Find(ctx, bson.M{"status": "Active"}, options.Find().SetProjection(bson.D{{Key: "location_id", Value: 1}, {Key: "department_id", Value: 1},
		{Key: "designation_id", Value: 1}, {Key: "grade", Value: 1}, {Key: "shift_code", Value: 1}, {Key: "type", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	cities, err := all("city", nil)
	if err != nil {
		return nil, nil, err
	}
	clusters, err := all("cluster", nil)
	if err != nil {
		return nil, nil, err
	}
	regions, err := all("region", nil)
	if err != nil {
		return nil, nil, err
	}
	locs, err := all("location", nil)
	if err != nil {
		return nil, nil, err
	}
	depts, err := all("department", nil)
	if err != nil {
		return nil, nil, err
	}
	desigs, err := all("designation", nil)
	if err != nil {
		return nil, nil, err
	}
	grades, err := all("grade", nil)
	if err != nil {
		return nil, nil, err
	}
	shifts, err := all("shift", nil)
	if err != nil {
		return nil, nil, err
	}
	types, err := all("simple_master", bson.M{"kind": "employment_type"})
	if err != nil {
		return nil, nil, err
	}
	byID := func(rows []store.Row) map[string]store.Row {
		m := map[string]store.Row{}
		for _, r := range rows {
			m[str(r["id"])] = r
		}
		return m
	}
	locByID, clusterByID, deptByID := byID(locs), byID(clusters), byID(depts)
	// Active employees using each master: through their location for a cluster and a city.
	n := map[string]int64{}
	for _, e := range active {
		n["location:"+str(e["location_id"])]++
		n["department:"+str(e["department_id"])]++
		n["designation:"+str(e["designation_id"])]++
		n["grade:"+str(e["grade"])]++
		n["shift:"+str(e["shift_code"])]++
		n["type:"+str(e["type"])]++
		if l := locByID[str(e["location_id"])]; l != nil {
			n["cluster:"+str(l["cluster_id"])]++
			if k := clusterByID[str(l["cluster_id"])]; k != nil {
				n["city:"+str(k["city_id"])]++
			}
		}
	}
	type master struct {
		t                  int
		kind, name, status string
		under              string
		using              int64
	}
	list := []master{}
	add := func(t int, kind string, rows []store.Row, under func(store.Row) string, using func(store.Row) int64) {
		for _, x := range rows {
			list = append(list, master{t, kind, str(x["name"]), str(x["status"]), under(x), using(x)})
		}
	}
	none := func(store.Row) string { return "" }
	count := func(key, field string) func(store.Row) int64 {
		return func(x store.Row) int64 { return n[key+":"+str(x[field])] }
	}
	add(1, "City", cities, none, count("city", "id"))
	add(2, "Cluster", clusters, none, count("cluster", "id"))
	add(3, "Region", regions, none, func(store.Row) int64 { return 0 })
	add(4, "Location", locs, func(x store.Row) string { return str(clusterByID[str(x["cluster_id"])]["name"]) }, count("location", "id"))
	add(5, "Department", depts, none, count("department", "id"))
	add(6, "Designation", desigs, func(x store.Row) string { return str(deptByID[str(x["department_id"])]["name"]) }, count("designation", "id"))
	add(7, "Grade", grades, none, count("grade", "name"))
	add(8, "Shift", shifts, none, count("shift", "code"))
	add(9, "Employment type", types, none, count("type", "name"))
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if (a.status == "Inactive") != (b.status == "Inactive") {
			return a.status == "Inactive"
		}
		if a.t != b.t {
			return a.t < b.t
		}
		return a.name < b.name
	})
	out := [][]any{}
	for _, m := range list {
		out = append(out, []any{m.kind, m.name, m.status, m.under, m.using})
	}
	return []string{"Master", "Name", "Status", "Under", "Active employees using it"}, out, nil
}

// repActivity: the audit log, newest first: when, and what happened with the record and who did it.
func repActivity(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	logs, err := s.st.DB.C("audit_log").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(20000).
		SetProjection(bson.D{{Key: "at", Value: 1}, {Key: "action", Value: 1}, {Key: "entity_id", Value: 1}, {Key: "actor", Value: 1}, {Key: "actor_role", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, l := range logs {
		action := str(l["action"])
		what := action
		// A short record id the action does not already name is added.
		if id, ok := l["entity_id"].(string); ok && utf8.RuneCountInString(id) <= 20 && !strings.Contains(action, id) {
			what += " " + id
		}
		what += " · " + str(l["actor"])
		if role, ok := l["actor_role"].(string); ok {
			what += ", " + role
		}
		out = append(out, []any{cell(l["at"]), what})
	}
	return []string{"When", "What happened"}, out, nil
}

// ---------- Inventory ----------

// repInventory: per store and item, the month's opening, each kind of movement and closing; uniform
// items valued at their price; asset closing checked against the serials on hand this month. Items
// shown are the active ones and any with movements in the store up to the month's end.
func repInventory(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Store", "Item", "Opening", "Additions (purchase)", "Transferred in", "Returned by employees", "Issued to employees", "Transferred out", "Written off",
		"Closing", "Unit price", "Closing value", "Serial check"}
	q := s.st.DB
	stores, err := s.storesFor(ctx, q, c)
	if err != nil {
		return nil, nil, err
	}
	ym := f.YM[:7]
	first, next := repDayStart(ym+"-01"), repDayStart(nextYm(ym)+"-01")
	items, err := q.C("inv_item").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "kind", Value: -1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, st := range stores {
		moves, err := q.C("inv_move").Find(ctx, bson.D{{Key: "store_id", Value: st["id"]}, {Key: "at", Value: bson.D{{Key: "$lt", Value: next}}}},
			options.Find().SetProjection(bson.D{{Key: "item_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "qty", Value: 1}, {Key: "at", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		type tally struct {
			moved                            bool
			op, add, tin, ret, iss, tout, wo int64
		}
		by := map[int64]*tally{}
		for _, m := range moves {
			id := repInt64(m["item_id"])
			t := by[id]
			if t == nil {
				t = &tally{}
				by[id] = t
			}
			t.moved = true
			qty := repInt64(m["qty"])
			at, _ := m["at"].(time.Time)
			if at.Before(first) { // the opening: everything before the month, in and out
				if containsStr([]string{"Addition", "Transfer in", "Return"}, str(m["kind"])) {
					t.op += qty
				} else {
					t.op -= qty
				}
				continue
			}
			switch str(m["kind"]) {
			case "Addition":
				t.add += qty
			case "Transfer in":
				t.tin += qty
			case "Return":
				t.ret += qty
			case "Issue":
				t.iss += qty
			case "Transfer out":
				t.tout += qty
			case "Write-off":
				t.wo += qty
			}
		}
		hand, err := invAvailable(ctx, q, str(st["id"]))
		if err != nil {
			return nil, nil, err
		}
		for _, it := range items {
			id := repInt64(it["id"])
			t := by[id]
			if t == nil {
				t = &tally{}
			}
			if it["status"] != "Active" && !t.moved {
				continue
			}
			cl := t.op + t.add + t.tin + t.ret - t.iss - t.tout - t.wo
			if t.op == 0 && t.add == 0 && t.tin == 0 && t.ret == 0 && t.iss == 0 && t.tout == 0 && t.wo == 0 && cl == 0 {
				continue
			}
			var price, value any
			check := ""
			if it["kind"] == "uniform" {
				if p, _ := it["price"].(float64); p > 0 {
					price, value = p, p*float64(cl)
				}
			} else if ym == today()[:7] {
				if h := hand[id]; h == cl {
					check = "Matches"
				} else {
					check = fmt.Sprintf("Check: %d on hand", h)
				}
			} else {
				check = "Past month"
			}
			out = append(out, []any{st["name"], it["name"], t.op, t.add, t.tin, t.ret, t.iss, t.tout, t.wo, cl, price, value, check})
		}
	}
	return cols, out, nil
}

// repInvEmployees: each issue and return in the month to someone in scope, with the uniform's value,
// its salary deduction plan and what was deducted for it this month.
func repInvEmployees(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Date", "Store", "Employee code", "Name", "City", "Movement", "Item", "Qty", "Value", "Salary deduction plan", "Deducted in this month"}
	q := s.st.DB
	stores, err := s.storesFor(ctx, q, c)
	if err != nil {
		return nil, nil, err
	}
	sids, storeName := []string{}, map[string]any{}
	for _, st := range stores {
		sids = append(sids, st["id"].(string))
		storeName[st["id"].(string)] = st["name"]
	}
	ym := f.YM[:7]
	list, err := repJoin(ctx, q, c, f, nil)
	if err != nil {
		return nil, nil, err
	}
	idx := repIndex(list)
	// The month in India, as the movement dates are shown.
	start, _ := time.ParseInLocation("2006-01", ym, ist)
	raw, err := q.C("inv_move").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(repEmpIDs(list))}, {Key: "kind", Value: bson.D{{Key: "$in", Value: bson.A{"Issue", "Return"}}}},
		{Key: "at", Value: bson.D{{Key: "$gte", Value: start}, {Key: "$lt", Value: start.AddDate(0, 1, 0)}}}, {Key: "store_id", Value: repIn(sids)}},
		options.Find().SetSort(bson.D{{Key: "at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, err
	}
	itemIDs := []any{}
	for _, m := range raw {
		itemIDs = append(itemIDs, m["item_id"])
	}
	itemRows, err := q.C("inv_item").Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: itemIDs}}}})
	if err != nil {
		return nil, nil, err
	}
	items := map[int64]store.Row{}
	for _, it := range itemRows {
		items[repInt64(it["id"])] = it
	}
	// Uniform moves recorded before they carried the issue id: the issue made with them (same person, same instant).
	var legacy []string
	for _, m := range raw {
		if str(m["ref"]) == "" && items[repInt64(m["item_id"])]["kind"] == "uniform" {
			legacy = append(legacy, str(m["employee_id"]))
		}
	}
	var issues []store.Row
	if len(legacy) > 0 {
		if issues, err = q.C("uniform_issue").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(legacy)}},
			options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "at", Value: 1}})); err != nil {
			return nil, nil, err
		}
	}
	type move struct {
		date, store, id, name, city, kind, item, ikind, ref any
		price                                               float64
		qty                                                 int32
	}
	moves := []move{}
	for _, m := range raw {
		it := items[repInt64(m["item_id"])]
		x, ok := idx[str(m["employee_id"])]
		if it == nil || !ok {
			continue
		}
		ref := str(m["ref"])
		if ref == "" && it["kind"] == "uniform" {
			at, _ := m["at"].(time.Time)
			for _, u := range issues {
				if ua, _ := u["at"].(time.Time); u["employee_id"] == m["employee_id"] && ua.Equal(at) {
					ref = str(u["id"])
					break
				}
			}
		}
		price, _ := it["price"].(float64)
		moves = append(moves, move{date: repDateIST(m["at"]), store: storeName[str(m["store_id"])], id: x.E["id"], name: x.E["name"], city: x.City, kind: m["kind"],
			item: it["name"], ikind: it["kind"], ref: ref, price: price, qty: int32(repInt64(m["qty"]))})
	}
	refs := []string{}
	for _, m := range moves {
		if r := m.ref.(string); r != "" && !containsStr(refs, r) {
			refs = append(refs, r)
		}
	}
	type uni struct {
		Total  float64
		Sched  []string
		Refund string
	}
	unis := map[string]*uni{}
	deducted := map[string]float64{} // employee|ref → deducted this month
	if len(refs) > 0 {
		ur, err := q.C("uniform_issue").Find(ctx, bson.D{{Key: "_id", Value: repIn(refs)}}, options.Find().SetProjection(bson.D{{Key: "amount", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		for _, u := range ur {
			unis[u["id"].(string)] = &uni{Total: u["amount"].(float64)}
		}
		lines, err := q.C("ledger").Find(ctx, bson.D{{Key: "source_id", Value: repIn(refs)}, {Key: "void", Value: false}},
			options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
		if err != nil {
			return nil, nil, err
		}
		for _, l := range lines {
			src, lym := l["source_id"].(string), strings.TrimSpace(l["ym"].(string))
			if u := unis[src]; u != nil {
				if l["kind"] == "ded" {
					u.Sched = append(u.Sched, ymLabel(lym)+" "+inr0(l["amount"].(float64)))
				} else {
					u.Refund = lym
				}
			}
			if l["kind"] == "ded" && lym == ym {
				deducted[l["employee_id"].(string)+"|"+src] += l["amount"].(float64)
			}
		}
	}
	out := [][]any{}
	for _, m := range moves {
		ref, isU := m.ref.(string), m.ikind == "uniform"
		var val any
		if isU {
			val = m.price * float64(m.qty)
		}
		u := unis[ref]
		plan := "None, unless lost or damaged"
		switch {
		case m.kind == "Return":
			plan = "Return, no change to recovery"
		case isU && (u == nil || len(u.Sched) == 0):
			plan = "Recovery not scheduled"
		case isU:
			plan = strings.Join(u.Sched, ", ")
			if u.Refund != "" {
				plan += "; refund " + ymLabel(u.Refund)
			}
		}
		var ded any
		if m.kind == "Issue" {
			led := deducted[str(m.id)+"|"+ref]
			if isU && u != nil && u.Total > 0 {
				led *= val.(float64) / u.Total
			}
			if led != 0 {
				ded = math.Round(led)
			}
		}
		out = append(out, []any{m.date, m.store, m.id, m.name, m.city, m.kind, m.item, m.qty, val, plan, ded})
	}
	return cols, out, nil
}
