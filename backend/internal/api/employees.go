package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// ---------- Query helpers shared by the employee pages (employees*.go, nominations.go, ui2_people.go) ----------

// employeesScope is the caller's data scope (access.Scope) as a filter on employee documents: everyone, the
// locations of a "locs" role, the caller's reports ("team") or the caller ("self"), and, for a role limited to some
// departments, those departments (named in the scope, matched by their ids).
func employeesScope(ctx context.Context, q store.Q, sc access.Scope) (bson.M, error) {
	return employeesScopeAt(ctx, q, sc, "", "_id")
}

// employeesScopeAt is employeesScope for a person's fields at prefix ("" for the queried documents themselves, or
// "e" / "e." for a document a $lookup put in e), whose id is the field idField under it ("_id" in employee
// documents, "id" in approval subjects).
func employeesScopeAt(ctx context.Context, q store.Q, sc access.Scope, prefix, idField string) (bson.M, error) {
	prefix = myrequestsPath(prefix)
	f := bson.M{}
	switch sc.Access {
	case "all":
	case "locs":
		f[prefix+"location_id"] = bson.M{"$in": append([]string{}, sc.Locations...)}
	case "team":
		f[prefix+"manager_id"] = sc.EmployeeID
	default:
		f[prefix+idField] = sc.EmployeeID
	}
	if len(sc.Departments) > 0 {
		depts, err := q.C("department").Distinct(ctx, "_id", bson.M{"name": bson.M{"$in": sc.Departments}})
		if err != nil {
			return nil, err
		}
		f[prefix+"department_id"] = bson.M{"$in": depts}
	}
	return f, nil
}

// employeesAnd joins filters that may name the same fields.
func employeesAnd(conds ...bson.M) bson.M {
	if len(conds) == 1 {
		return conds[0]
	}
	l := bson.A{}
	for _, c := range conds {
		l = append(l, c)
	}
	return bson.M{"$and": l}
}

// employeesFold matches a whole value regardless of case, as lower(col)=lower(v) did.
func employeesFold(v string) bson.M {
	return bson.M{"$regex": "^" + regexp.QuoteMeta(v) + "$", "$options": "i"}
}

// employeesILike is col ILIKE pattern: % is any run of characters, _ any one character and a backslash makes the
// next character literal; letters match in either case.
func employeesILike(pattern string) bson.M {
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '%':
			b.WriteString(".*")
		case r == '_':
			b.WriteString(".")
		case r == '\\' && i+1 < len(rs):
			i++
			b.WriteString(regexp.QuoteMeta(string(rs[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return bson.M{"$regex": b.String(), "$options": "is"}
}

// employeesNullIf is NULLIF(v, ""): a blank value is stored as null.
func employeesNullIf(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// employeesInt reads a stored integer (int32, int64, or a JSON number) as an int.
func employeesInt(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case int16:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}

// employeesJSONInto decodes a stored jsonb value into dst through JSON, as reading the column always did (fields
// missing from the JSON keep dst's values).
func employeesJSONInto(v any, dst any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// employeesLookup joins the document of collection from whose _id is the value at localField, as the array as.
func employeesLookup(from, localField, as string) bson.D {
	return bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: from}, {Key: "localField", Value: localField},
		{Key: "foreignField", Value: "_id"}, {Key: "as", Value: as}}}}
}

// employeesUnwind keeps the joined document at path and drops rows without one, as an inner JOIN did.
func employeesUnwind(path string) bson.D { return bson.D{{Key: "$unwind", Value: "$" + path}} }

// employeesFirst is the first value of a LEFT JOINed array field, or null when nothing joined.
func employeesFirst(path string) bson.D {
	return bson.D{{Key: "$ifNull", Value: bson.A{bson.D{{Key: "$first", Value: "$" + path}}, nil}}}
}

// employeesDirectoryStages turn employee documents into directory rows: the person with their department,
// designation, location, cluster and city (all required), and their L1 and L2 managers (when set).
func employeesDirectoryStages() []bson.D {
	return []bson.D{
		employeesLookup("department", "department_id", "d"), employeesUnwind("d"),
		employeesLookup("designation", "designation_id", "g"), employeesUnwind("g"),
		employeesLookup("location", "location_id", "l"), employeesUnwind("l"),
		employeesLookup("cluster", "l.cluster_id", "k"), employeesUnwind("k"),
		employeesLookup("city", "k.city_id", "c"), employeesUnwind("c"),
		employeesLookup("employee", "manager_id", "m"),
		employeesLookup("employee", "functional_manager_id", "fm"),
		{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0}, {Key: "id", Value: "$_id"}, {Key: "name", Value: 1}, {Key: "type", Value: 1}, {Key: "doj", Value: 1},
			{Key: "status", Value: 1}, {Key: "exit_status", Value: 1}, {Key: "lwd", Value: 1}, {Key: "exit_reason", Value: 1}, {Key: "grade", Value: 1},
			{Key: "shift_code", Value: 1}, {Key: "attendance_mode", Value: 1}, {Key: "job_role", Value: 1}, {Key: "skill", Value: 1},
			{Key: "department", Value: "$d.name"}, {Key: "corporate", Value: "$d.corporate"}, {Key: "designation", Value: "$g.name"},
			{Key: "location_id", Value: "$l._id"}, {Key: "location", Value: "$l.name"}, {Key: "cluster", Value: "$k.name"},
			{Key: "city", Value: "$c.name"}, {Key: "city_id", Value: "$c._id"},
			{Key: "manager_id", Value: employeesFirst("m._id")}, {Key: "manager", Value: employeesFirst("m.name")},
			{Key: "functional_manager_id", Value: employeesFirst("fm._id")}, {Key: "functional_manager", Value: employeesFirst("fm.name")},
		}}},
	}
}

// employeesDirectory runs a filter over employees, in id order, through the directory stages.
func employeesDirectory(ctx context.Context, q store.Q, filter bson.M, skip, limit int64) ([]store.Row, error) {
	p := []bson.D{{{Key: "$match", Value: filter}}, {{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}}}
	if skip > 0 {
		p = append(p, bson.D{{Key: "$skip", Value: skip}})
	}
	if limit > 0 {
		p = append(p, bson.D{{Key: "$limit", Value: limit}})
	}
	return q.C("employee").Aggregate(ctx, append(p, employeesDirectoryStages()...))
}

// employeesReason is the active change reason for usedFor named like name (any case), as stored.
func employeesReason(ctx context.Context, q store.Q, usedFor, name string) (string, bool) {
	r, err := q.C("simple_master").FindOne(ctx, bson.M{"kind": "change_reason", "used_for": usedFor, "name": employeesFold(name), "status": "Active"})
	if err != nil || r == nil {
		return "", false
	}
	return str(r["name"]), true
}

// ---------- Employee directory ----------

func (s *Server) employeeList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.directory", 1); err != nil {
		return nil, err
	}
	ctx, qv := r.Context(), r.URL.Query()
	scope, err := employeesScope(ctx, s.st.DB, c.Scope)
	if err != nil {
		return nil, err
	}
	conds := []bson.M{scope}
	query := strings.TrimSpace(qv.Get("q"))
	// City, location and department are given by city id, location id and department name.
	if v := qv.Get("city"); v != "" {
		clusters, err := s.st.DB.C("cluster").Distinct(ctx, "_id", bson.M{"city_id": v})
		if err != nil {
			return nil, err
		}
		locs, err := s.st.DB.C("location").Distinct(ctx, "_id", bson.M{"cluster_id": bson.M{"$in": clusters}})
		if err != nil {
			return nil, err
		}
		conds = append(conds, bson.M{"location_id": bson.M{"$in": locs}})
	}
	if v := qv.Get("location"); v != "" {
		conds = append(conds, bson.M{"location_id": v})
	}
	if v := qv.Get("department"); v != "" {
		depts, err := s.st.DB.C("department").Distinct(ctx, "_id", bson.M{"name": v})
		if err != nil {
			return nil, err
		}
		conds = append(conds, bson.M{"department_id": bson.M{"$in": depts}})
	}
	for _, key := range []string{"type", "status"} {
		if v := qv.Get(key); v != "" {
			conds = append(conds, bson.M{key: v})
		}
	}
	page, _ := strconv.Atoi(qv.Get("page"))
	size, _ := strconv.Atoi(qv.Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 200 {
		size = 25
	}
	filter := employeesAnd(conds...)
	if query != "" && len(pmWords(query)) == 0 {
		query = "" // only punctuation (".*", "-", "@"): no words to look for, so the list is unfiltered (PPL-014)
	}
	if query != "" {
		// Approximate search (peoplesearch.go): best match first, within the same scope and filters.
		ids, err := employeesSearch(ctx, s.st.DB, filter, query, c.Level("emp.personal") >= 1)
		if err != nil {
			return nil, err
		}
		from, to := min((page-1)*size, len(ids)), min(page*size, len(ids))
		rows, err := employeesDirectory(ctx, s.st.DB, bson.M{"_id": bson.M{"$in": ids[from:to]}}, 0, 0)
		if err != nil {
			return nil, err
		}
		at := make(map[string]int, to-from)
		for i, id := range ids[from:to] {
			at[id] = i
		}
		sort.SliceStable(rows, func(i, j int) bool { return at[str(rows[i]["id"])] < at[str(rows[j]["id"])] })
		return map[string]any{"total": len(ids), "page": page, "size": size, "rows": rows}, nil
	}
	// Every employee has its department, designation and location (foreign keys), so the directory's joins keep them
	// all and the count is of the employees themselves.
	total, err := s.st.DB.C("employee").Count(ctx, filter)
	if err != nil {
		return nil, err
	}
	rows, err := employeesDirectory(ctx, s.st.DB, filter, int64((page-1)*size), int64(size))
	if err != nil {
		return nil, err
	}
	return map[string]any{"total": total, "page": page, "size": size, "rows": rows}, nil
}

// grossFor returns the gross effective in a YYYY-MM month.
func grossFor(ctx context.Context, q store.Q, id, ym string) (float64, bool) {
	g, err := q.C("gross_history").FindOne(ctx, bson.M{"employee_id": id, "from_month": bson.M{"$lte": ym}},
		options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}}).SetProjection(bson.M{"gross": 1}))
	if err != nil || g == nil {
		return 0, false
	}
	v, ok := g["gross"].(float64)
	return v, ok
}

// structureFor returns the salary structure mapped to an employee on a date (default when unmapped).
func structureFor(ctx context.Context, q store.Q, id, date string) (string, calc.Structure) {
	var name string
	st := calc.DefaultStructure
	var s store.Row
	m, err := q.C("structure_map").FindOne(ctx, bson.M{"employee_id": id, "from_date": bson.M{"$lte": date}},
		options.FindOne().SetSort(bson.D{{Key: "from_date", Value: -1}}))
	if err == nil && m != nil {
		s, err = q.C("salary_structure").FindOne(ctx, bson.M{"_id": m["structure_id"]})
	}
	if err != nil || s == nil || employeesJSONInto(s["rates"], &st) != nil {
		if s, _ = q.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}); s != nil {
			name = str(s["name"])
			_ = employeesJSONInto(s["rates"], &st)
		}
		return name, st
	}
	return str(s["name"]), st
}

// lwfFor returns the LWF rule of the state an employee's location sits in.
func lwfFor(ctx context.Context, q store.Q, locationID, date string) *calc.LwfRule {
	city := employeesCityOf(ctx, q, locationID)
	if city == nil {
		return nil
	}
	w, err := q.C("lwf_rule").FindOne(ctx, bson.M{"state_id": city["state_id"], "effective_from": bson.M{"$lte": date}},
		options.FindOne().SetSort(bson.D{{Key: "effective_from", Value: -1}}))
	if err != nil || w == nil {
		return nil
	}
	var rule calc.LwfRule
	if employeesJSONInto(w["rule"], &rule) != nil {
		return nil
	}
	return &rule
}

// employeesCityOf is the city a location sits in (location → cluster → city), or nil.
func employeesCityOf(ctx context.Context, q store.Q, locationID any) store.Row {
	l, _ := q.C("location").FindOne(ctx, bson.M{"_id": locationID})
	if l == nil {
		return nil
	}
	k, _ := q.C("cluster").FindOne(ctx, bson.M{"_id": l["cluster_id"]})
	if k == nil {
		return nil
	}
	ci, _ := q.C("city").FindOne(ctx, bson.M{"_id": k["city_id"]})
	return ci
}

// employeeGet returns one employee; each confidential group is shown Full / Masked / Hidden by role.
func (s *Server) employeeGet(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	own := id == c.EmployeeID
	if !own {
		if err := c.Need("emp.directory", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	rows, err := employeesDirectory(ctx, s.st.DB, bson.M{"_id": id}, 0, 1)
	if err != nil || len(rows) == 0 {
		return nil, orNotFound(err)
	}
	e := rows[0]
	e["hireIds"] = hireIDsFor(ctx, s.st.DB, id) // SI-/OI-/JI- (hirefix.go)
	if own || c.Level("emp.personal") >= 1 {
		e["personal"], _ = s.st.DB.C("employee_personal").FindOne(ctx, bson.M{"employee_id": id}, options.FindOne().SetProjection(bson.M{"_id": 0,
			"dob": 1, "gender": 1, "mobile": 1, "email": 1, "address": 1, "father_spouse": 1, "marital": 1, "ec_name": 1, "ec_relation": 1, "ec_mobile": 1, "child1": 1, "child2": 1, "spouse": 1}))
	}
	if own || c.Level("emp.govid") >= 1 {
		g, _ := s.st.DB.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id}, options.FindOne().SetProjection(bson.M{"_id": 0,
			"aadhaar": 1, "pan": 1, "uan": 1, "esic": 1}))
		for k, v := range g {
			g[k] = c.Show("govId", v, id)
		}
		e["govId"] = g
	}
	if own || c.Level("emp.bank") >= 1 {
		// The account in use: the latest one effective this month or before.
		b, _ := s.st.DB.C("employee_bank").FindOne(ctx, bson.M{"employee_id": id, "from_month": bson.M{"$lte": today()[:7]}},
			options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}).
				SetProjection(bson.M{"_id": 0, "bank_name": 1, "account_no": 1, "ifsc": 1, "from_month": 1}))
		if b != nil {
			b["account_no"], b["ifsc"] = c.Show("bank", b["account_no"], id), c.Show("bank", b["ifsc"], id)
		}
		e["bank"] = b
	}
	if own || c.Level("emp.salary") >= 1 {
		gross, ok := grossFor(ctx, s.st.DB, id, today()[:7])
		name, st := structureFor(ctx, s.st.DB, id, today())
		p := payFor(ctx, s.st.DB, id, e["type"].(string))
		pv := p
		pv.CompAmts = nil // the Reimbursement is shown below, under the salary mask
		sal := map[string]any{"structure": name, "gross": nil, "ctc": nil, "net": nil, "pay": pv, "reimb": c.Show("salary", reimbOf(p), id),
			"varPayout": varPayoutInfo(ctx, s.st.DB, id, e["type"].(string), str(e["doj"]), p, st)} // the person's own cycle (varpay.go)
		if ok {
			in := calc.Input{Type: calc.EmploymentType(e["type"].(string)), Gross: gross, Structure: withMedical(&st, p.Medical), VarPct: p.VarPct, VarCycle: p.VarCycle,
				Meal: p.Meal, Accom: p.Accom, Lwf: lwfFor(ctx, s.st.DB, e["location_id"].(string), today()), PfExempt: p.PfExempt, AllBasic: p.allBasicIn(today()[:7]),
				EmpAmts: p.CompAmts}
			hasTarget := varTargetInput(ctx, s.st.DB, id, today()[:7], st, &in) // variable pay as a target (paycomp.go)
			res := calc.Calc(in)
			if st.Designed {
				sal["designed"] = true
				if hasTarget {
					t := targetAt(loadTargets(ctx, s.st.DB, id), today()[:7])
					sal["varTarget"] = c.Show("salary", varMonthly(res), id)
					sal["varMode"], sal["varPct"] = t.Mode, c.Show("salary", t.Pct, id)
				}
				for _, x := range calc.ComponentsOf(st) {
					if x.Kind == calc.CompVariable {
						sal["varCycle"] = x.Cycle
					}
				}
			}
			sal["gross"] = c.Show("salary", gross, id)
			sal["ctc"] = c.Show("salary", calc.Round2(res.CTC), id)
			sal["net"] = c.Show("salary", calc.Round2(res.Net), id)
		}
		e["salary"] = sal
	}
	s.employeeExtras(ctx, c, e, id)
	s.employeeB1(ctx, c, e, id, own) // PAN check, Existing UAN?, opening optional holidays (b1_employee.go)
	return e, nil
}

func orNotFound(err error) error {
	if err != nil {
		return err
	}
	return notFound("")
}

func (s *Server) employeeHistory(r *http.Request, c *Ctx) (any, error) {
	id := r.PathValue("id")
	if id != c.EmployeeID {
		if err := c.Need("emp.directory", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(r.Context(), s.st.DB, id); err != nil {
			return nil, err
		}
	}
	rows, err := s.st.DB.C("employee_change").Find(r.Context(), bson.M{"employee_id": id}, options.Find().
		SetProjection(bson.M{"_id": 0, "field": 1, "old_value": 1, "new_value": 1, "reason": 1, "effective_from": 1, "state": 1, "source": 1, "changed_by": 1, "changed_at": 1}).
		SetSort(bson.D{{Key: "effective_from", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil || id == c.EmployeeID {
		return rows, err
	}
	return historyForViewer(c, id, rows), nil
}

// historySalaryFields are the change history's pay rows (gross revisions, pay options).
var historySalaryFields = map[string]bool{"Monthly gross": true, "Variable pay %": true, "Variable payout cycle": true, "Meal deduction": true,
	"Accommodation deduction": true, "Salary structure": true, "Tax regime": true}

// historyForViewer keeps someone else's change history within the viewer's rights (PPL-035, PPL-063, PPL-152):
// personal rows (mobile, email, date of birth, address …) need the personal details right; pay rows follow the
// salary group (Masked shows the change without the amounts, Hidden leaves the row out).
func historyForViewer(c *Ctx, id string, rows []store.Row) []store.Row {
	personal := map[string]bool{}
	for _, pc := range personalCols {
		personal[pc.label] = true
	}
	out := make([]store.Row, 0, len(rows))
	for _, h := range rows {
		f := str(h["field"])
		switch {
		case personal[f] && c.Level("emp.personal") < 1:
			continue
		case historySalaryFields[f]:
			mode := c.Sensitive["salary"]
			if mode == "" || mode == access.Hidden || c.Level("emp.salary") < 1 {
				continue
			}
			if mode == access.Masked {
				h["old_value"], h["new_value"] = c.Show("salary", h["old_value"], id), c.Show("salary", h["new_value"], id)
			}
		}
		out = append(out, h)
	}
	return out
}

// employeesReportsUpTo: walking up from's reporting line (L1) reaches target.
func employeesReportsUpTo(ctx context.Context, q store.Q, from, target string) bool {
	seen := map[string]bool{}
	for cur := from; cur != "" && !seen[cur] && len(seen) < 200; {
		if cur == target {
			return true
		}
		seen[cur] = true
		e, err := q.C("employee").FindOne(ctx, bson.M{"_id": cur}, options.FindOne().SetProjection(bson.M{"manager_id": 1}))
		if err != nil || e == nil {
			return false
		}
		cur = str(e["manager_id"])
	}
	return false
}

// ---------- Job changes: predefined reason + effective date; past = back-dated, future = scheduled ----------

type JobChange struct {
	Changes       map[string]string `json:"changes"`
	Reason        string            `json:"reason"`
	EffectiveFrom string            `json:"effectiveFrom"`
}

// jobField is one changeable job field: the employee column it is stored in, its label, how a given value is
// found among the active master values (lookup gives the id to store and the name to show; nil for designation,
// which depends on the department) and how a stored id is named for the history (oldName; nil: shown as stored).
type jobField struct {
	column, label string
	lookup        func(ctx context.Context, q store.Q, v string) (id, name string, ok bool)
	oldName       func(ctx context.Context, q store.Q, id string) (string, bool)
}

// employeesPick finds one document and returns two of its fields (keys as in a Row: "id" for _id).
func employeesPick(ctx context.Context, q store.Q, coll string, filter bson.M, idKey, nameKey string) (string, string, bool) {
	r, err := q.C(coll).FindOne(ctx, filter)
	if err != nil || r == nil {
		return "", "", false
	}
	return str(r[idKey]), str(r[nameKey]), true
}

// employeesActiveByIDOrName finds an active master value by its id or its name in any case.
func employeesActiveByIDOrName(coll string) func(context.Context, store.Q, string) (string, string, bool) {
	return func(ctx context.Context, q store.Q, v string) (string, string, bool) {
		return employeesPick(ctx, q, coll, bson.M{"$or": bson.A{bson.M{"_id": v}, bson.M{"name": employeesFold(v)}}, "status": "Active"}, "id", "name")
	}
}

// employeesNameOf names a document by its id.
func employeesNameOf(coll string) func(context.Context, store.Q, string) (string, bool) {
	return func(ctx context.Context, q store.Q, id string) (string, bool) {
		_, name, ok := employeesPick(ctx, q, coll, bson.M{"_id": id}, "id", "name")
		return name, ok
	}
}

// employeesActiveManager finds an active employee by code in any case.
func employeesActiveManager(ctx context.Context, q store.Q, v string) (string, string, bool) {
	return employeesPick(ctx, q, "employee", bson.M{"_id": employeesFold(v), "status": "Active"}, "id", "name")
}

var jobFields = map[string]jobField{
	"location":    {"location_id", "Location", employeesActiveByIDOrName("location"), employeesNameOf("location")},
	"department":  {"department_id", "Department", employeesActiveByIDOrName("department"), employeesNameOf("department")},
	"designation": {"designation_id", "Designation", nil, employeesNameOf("designation")},
	"grade": {"grade", "Grade", func(ctx context.Context, q store.Q, v string) (string, string, bool) {
		return employeesPick(ctx, q, "grade", bson.M{"name": v, "status": "Active"}, "name", "name")
	}, nil},
	"shift": {"shift_code", "Shift", func(ctx context.Context, q store.Q, v string) (string, string, bool) {
		return employeesPick(ctx, q, "shift", bson.M{"code": strings.ToUpper(v), "status": "Active"}, "code", "code")
	}, nil},
	"manager":           {"manager_id", "Reporting manager (L1)", employeesActiveManager, employeesNameOf("employee")},
	"functionalManager": {"functional_manager_id", "Functional manager (L2)", employeesActiveManager, employeesNameOf("employee")},
	"attendanceMode": {"attendance_mode", "Attendance mode", func(ctx context.Context, q store.Q, v string) (string, string, bool) {
		return employeesPick(ctx, q, "simple_master", bson.M{"kind": "attendance_mode", "name": v, "status": "Active"}, "name", "name")
	}, nil},
}

type change struct{ Field, From, To string }

// ApplyJobChange validates and applies (or schedules) a job change; shared by single edits and bulk update.
func ApplyJobChange(ctx context.Context, tx store.Q, c *Ctx, id string, b JobChange, source string) ([]change, error) {
	e, err := tx.C("employee").Lock(ctx, bson.M{"_id": id})
	if err != nil || e == nil {
		return nil, orNotFound(err)
	}
	if e["status"] != "Active" {
		return nil, bad("%s is %s. Only active employees can be updated", e["name"], strings.ToLower(e["status"].(string)))
	}
	reason, ok := employeesReason(ctx, tx, "Employee update", b.Reason)
	if !ok {
		return nil, bad("Reason “%s” is not in the list of employee update reasons", b.Reason)
	}
	if !isDate(b.EffectiveFrom) {
		return nil, bad("Effective From must be a date like 2026-11-01")
	}
	if b.EffectiveFrom < e["doj"].(string) {
		return nil, bad("Effective From is before the joining date")
	}
	deptID := e["department_id"].(string)
	if v, ok := b.Changes["department"]; ok {
		did, _, ok := jobFields["department"].lookup(ctx, tx, v)
		if !ok {
			return nil, bad("Department “%s” is not an active department", v)
		}
		if did != deptID && b.Changes["designation"] == "" {
			return nil, bad("Designation is required when the department changes")
		}
		deptID = did
	}
	type pending struct {
		f               jobField
		from, to, value string
	}
	var ch []pending
	for k, v := range b.Changes {
		f, ok := jobFields[k]
		if !ok {
			return nil, bad("Unknown field %s", k)
		}
		var newID, newName string
		if k == "designation" {
			desig, _ := tx.C("designation").FindOne(ctx, bson.M{"$or": bson.A{bson.M{"_id": v}, bson.M{"name": employeesFold(v)}}, "department_id": deptID, "status": "Active"})
			if desig == nil {
				return nil, bad("Designation “%s” does not exist under this department", v)
			}
			newID, newName = str(desig["id"]), str(desig["name"])
			grade := e["grade"].(string)
			if g := b.Changes["grade"]; g != "" {
				grade = g
			}
			if !containsStr(strs(desig["grades"]), grade) {
				return nil, bad("Grade %s is not mapped to %s", grade, newName)
			}
		} else if newID, newName, ok = f.lookup(ctx, tx, v); !ok {
			return nil, bad("%s “%s” is not an active value", f.label, v)
		}
		if k == "grade" && b.Changes["designation"] == "" && newID != str(e["grade"]) {
			// A grade alone must be one the current designation offers, as when the designation changes (PPL-048).
			if desig, _ := tx.C("designation").FindOne(ctx, bson.M{"_id": e["designation_id"]}); desig != nil && !containsStr(strs(desig["grades"]), newID) {
				return nil, bad("Grade %s is not mapped to %s", newName, str(desig["name"]))
			}
		}
		if k == "manager" && newID == id {
			return nil, bad("An employee cannot report to themselves")
		}
		if k == "manager" || k == "functionalManager" {
			// No reporting loop: the new manager may not report, directly or up the line, to this employee (PPL-049).
			if employeesReportsUpTo(ctx, tx, newID, id) {
				return nil, bad("%s reports to %s, directly or up the line, so this would make a reporting loop", newName, e["name"])
			}
		}
		cur := fmt.Sprint(e[f.column])
		if e[f.column] == nil {
			cur = ""
		}
		if cur == newID {
			continue
		}
		from := cur
		if f.oldName != nil && cur != "" {
			if n, ok := f.oldName(ctx, tx, cur); ok {
				from = n
			}
		}
		ch = append(ch, pending{f, from, newName, newID})
	}
	// A job edit of someone without Role or Skill gives them (hirefix.go); Bulk update checks its rows itself.
	if source == "Manual" {
		core, role, skill := false, "", ""
		for _, x := range ch {
			core = core || coreJobColumns[x.f.column]
			switch x.f.column {
			case "job_role":
				role = x.value
			case "skill":
				skill = x.value
			}
		}
		if msg := roleSkillForJob(e, role, skill); core && msg != "" {
			return nil, bad("%s", msg)
		}
	}
	// A shift can only be given to a department and location it applies to (its rows, shift_maps.go). Checked when the
	// shift, the location or the department changes: someone already on a shift their mapping no longer allows keeps it.
	shiftCode, locID, moved := e["shift_code"].(string), e["location_id"].(string), false
	for _, x := range ch {
		switch x.f.column {
		case "shift_code":
			shiftCode, moved = x.value, true
		case "location_id":
			locID, moved = x.value, true
		case "department_id":
			moved = true
		}
	}
	if sh, err := tx.C("shift").FindOne(ctx, bson.M{"code": shiftCode}); err == nil && sh != nil && moved {
		if ok, err := shiftRowFor(ctx, tx, str(sh["id"]), deptID, locID); err != nil {
			return nil, err
		} else if !ok {
			return nil, bad("%s", shiftNotForMsg(shiftCode, mbNameOf(ctx, tx, "department", deptID), mbNameOf(ctx, tx, "location", locID)))
		}
	}
	if err := machineGate(ctx, tx, c, id, e["location_id"].(string), locID); err != nil { // into a full punch-machine cluster (machines.go)
		return nil, err
	}
	future := b.EffectiveFrom > today()
	state := "Applied"
	if future {
		state = "Scheduled"
	}
	out := []change{}
	for _, x := range ch {
		if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": x.f.label, "old_value": x.from, "new_value": x.to,
			"reason": reason, "effective_from": b.EffectiveFrom, "state": state, "source": source, "apply_column": x.f.column, "apply_value": x.value,
			"changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
			return nil, err
		}
		if !future {
			if _, err := tx.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{x.f.column: x.value}}); err != nil {
				return nil, err
			}
		}
		out = append(out, change{x.f.label, x.from, x.to})
	}
	action := "Changed job details"
	if future {
		action = "Scheduled job change"
	}
	return out, audit(ctx, tx, c, action, "employee", id, nil, map[string]any{"changes": out, "effectiveFrom": b.EffectiveFrom, "reason": reason})
}

func containsStr(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) employeeJob(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.directory", 2); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if err := c.MustInScope(r.Context(), s.st.DB, id); err != nil {
		return nil, err
	}
	var b JobChange
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	var out []change
	err := s.st.Tx(r.Context(), func(tx store.Q) error {
		var err error
		out, err = ApplyJobChange(r.Context(), tx, c, id, b, "Manual")
		return err
	})
	return map[string]any{"changes": out}, err
}

func (s *Server) employeeInactive(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.status", 2); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if err := c.MustInScope(r.Context(), s.st.DB, id); err != nil {
		return nil, err
	}
	var b struct {
		LWD    string `json:"lwd"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !isDate(b.LWD) {
		return nil, bad("Give the last working day")
	}
	if b.LWD > today() {
		return nil, bad("For a future date, start a separation so notice and approvals apply")
	}
	return nil, s.st.Tx(r.Context(), func(tx store.Q) error {
		emp, err := tx.C("employee").Lock(r.Context(), bson.M{"_id": id})
		if err != nil || emp == nil {
			return orNotFound(err)
		}
		e := store.Row{"status": emp["status"], "doj": emp["doj"]} // what the audit records as before
		if e["status"] == "Inactive" {
			return bad("Already inactive")
		}
		if b.LWD < e["doj"].(string) {
			return bad("Last working day cannot be before the joining date")
		}
		reason, ok := employeesReason(r.Context(), tx, "Separation", b.Reason)
		if !ok {
			return bad("“%s” is not a separation reason", b.Reason)
		}
		if _, err := tx.C("employee").Update(r.Context(), bson.M{"_id": id},
			bson.M{"$set": bson.M{"status": "Inactive", "lwd": b.LWD, "exit_reason": reason, "exit_status": nil}}); err != nil {
			return err
		}
		if _, err := uniformRefundCancel(r.Context(), tx, id, b.LWD); err != nil { // policy: no uniform refund after leaving
			return err
		}
		if _, err := tx.C("employee_change").Insert(r.Context(), bson.M{"employee_id": id, "field": "Status", "old_value": "Active", "new_value": "Inactive from " + b.LWD,
			"reason": reason, "effective_from": b.LWD, "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
			return err
		}
		return audit(r.Context(), tx, c, "Marked employee inactive", "employee", id, e, map[string]any{"lwd": b.LWD, "reason": reason})
	})
}

// ApplyScheduled makes scheduled changes effective, ends employment after the last working
// day and activates verified joiners on their joining date. Run by the daily job.
func ApplyScheduled(ctx context.Context, st *store.Store) (int, error) {
	t := store.Today()
	due, err := st.DB.C("employee_change").Find(ctx, bson.M{"state": "Scheduled", "effective_from": bson.M{"$lte": t}, "apply_column": bson.M{"$ne": nil}},
		options.Find().SetSort(bson.D{{Key: "effective_from", Value: 1}, {Key: "_id", Value: 1}}).
			SetProjection(bson.M{"employee_id": 1, "apply_column": 1, "apply_value": 1}))
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		err := st.Tx(ctx, func(tx store.Q) error {
			if _, err := tx.C("employee").Update(ctx, bson.M{"_id": d["employee_id"]}, bson.M{"$set": bson.M{d["apply_column"].(string): d["apply_value"]}}); err != nil {
				return err
			}
			_, err := tx.C("employee_change").Update(ctx, bson.M{"_id": d["id"]}, bson.M{"$set": bson.M{"state": "Applied"}})
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	if _, err := st.DB.C("employee").UpdateMany(ctx, bson.M{"status": "Active", "lwd": bson.M{"$ne": nil, "$lt": t}}, bson.M{"$set": bson.M{"status": "Inactive"}}); err != nil {
		return 0, err
	}
	if _, err := st.DB.C("employee").UpdateMany(ctx, bson.M{"status": "Pre-boarding", "verified": true, "doj": bson.M{"$lte": t}}, bson.M{"$set": bson.M{"status": "Active"}}); err != nil {
		return len(due), err
	}
	// Leavers whose last working day has passed get no uniform refund scheduled after it (uniform.go).
	_, err = sweepUniformRefunds(ctx, st)
	return len(due), err
}

// ---------- Employee codes: never reused; the next number only moves up ----------

// employeesCodeKey is the code series employees of a type take theirs from: one for all, or one per type.
func employeesCodeKey(ctx context.Context, q store.Q, empType string) string {
	if set, _ := q.C("employee_code_settings").FindOne(ctx, bson.M{"_id": 1}); set != nil && set["by_type"] == true {
		return empType
	}
	return "All"
}

func NextEmployeeCode(ctx context.Context, tx store.Q, empType string) (string, error) {
	key := employeesCodeKey(ctx, tx, empType)
	series, err := tx.C("employee_code_series").Lock(ctx, bson.M{"key": key})
	if err != nil {
		return "", err // e.g. an aborted or timed-out transaction: not a missing series
	}
	if series == nil {
		return "", bad("No employee code series for %s", key)
	}
	prefix, pad, n := str(series["prefix"]), employeesInt(series["pad"]), employeesInt(series["next_no"])
	for {
		code := fmt.Sprintf("%s%0*d", prefix, pad, n)
		n++
		taken, err := tx.C("employee").Exists(ctx, bson.M{"_id": employeesFold(code)})
		if err != nil {
			return "", err
		}
		if !taken {
			_, err := tx.C("employee_code_series").Update(ctx, bson.M{"key": key}, bson.M{"$set": bson.M{"next_no": n}})
			return code, err
		}
	}
}

func (s *Server) nextCode(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.directory", 1); err != nil {
		return nil, err
	}
	key := employeesCodeKey(r.Context(), s.st.DB, r.URL.Query().Get("type"))
	series, err := s.st.DB.C("employee_code_series").FindOne(r.Context(), bson.M{"key": key})
	if err != nil || series == nil {
		return map[string]any{"next": nil}, nil
	}
	return map[string]any{"next": fmt.Sprintf("%s%0*d", str(series["prefix"]), employeesInt(series["pad"]), employeesInt(series["next_no"]))}, nil
}

// ---------- Import existing employees (HR Admin only), checked row by row before anything is created ----------

type ImportRow struct {
	Code        string  `json:"code"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	DOJ         string  `json:"doj"`
	Location    string  `json:"location"`
	Department  string  `json:"department"`
	Designation string  `json:"designation"`
	Grade       string  `json:"grade"`
	Shift       string  `json:"shift"`
	ManagerID   string  `json:"managerId"`
	Gross       float64 `json:"gross"`
	Mobile      string  `json:"mobile"`
	Email       string  `json:"email"`
	DOB         string  `json:"dob"`
	Gender      string  `json:"gender"`
	PAN         string  `json:"pan"`
	Aadhaar     string  `json:"aadhaar"`
	UAN         string  `json:"uan"`
	ESIC        string  `json:"esic"`
	BankName    string  `json:"bankName"`
	AccountNo   string  `json:"accountNo"`
	IFSC        string  `json:"ifsc"`
	// Uniform sizes (optional, uniform.go): on the size lists.
	Shirt  string `json:"shirt"`
	Waist  string `json:"waist"`
	Length string `json:"length"`
	Shoe   string `json:"shoe"`
}

var (
	reCode    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-.]{0,19}$`) // no "/": the code is part of every record's web address (PPL-092)
	rePAN     = regexp.MustCompile(`^[A-Z]{5}\d{4}[A-Z]$`)
	reAadhaar = regexp.MustCompile(`^\d{12}$`)
	reMobile  = regexp.MustCompile(`^\d{10}$`)
	reIFSC    = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`)
	reAcct    = regexp.MustCompile(`^\d{6,18}$`)
	reEmail   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

type importResult struct {
	Line   int      `json:"line"`
	Code   string   `json:"code,omitempty"`
	Errors []string `json:"errors"`
}

// employeesMemo caches a lookup by its key: the masters do not change while one import runs.
func employeesMemo[T any](f func(string) T) func(string) T {
	m := map[string]T{}
	return func(k string) T {
		v, ok := m[k]
		if !ok {
			v = f(k)
			m[k] = v
		}
		return v
	}
}

// employeesFound is a master value an import row names: its id, the designation's grades, and whether it was found.
type employeesFound struct {
	id     string
	grades []string
	ok     bool
}

func (s *Server) employeeImport(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin can import employees")
	}
	var b struct {
		Rows  []ImportRow `json:"rows"`
		Apply bool        `json:"apply"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.Rows) == 0 || len(b.Rows) > 5000 {
		return nil, bad("Send between 1 and 5,000 rows")
	}
	ctx := r.Context()
	var results []importResult
	failed := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		results, failed = nil, 0 // the transaction may run again
		// The employee records written, for their document windows (doc_collect.go).
		var created []bson.M
		seenCode, seenPAN := map[string]bool{}, map[string]bool{}
		var defaultStructure string
		if d, _ := tx.C("salary_structure").FindOne(ctx, bson.M{"is_default": true}); d != nil {
			defaultStructure = str(d["id"])
		}
		find := func(coll string, filter bson.M) employeesFound {
			x, err := tx.C(coll).FindOne(ctx, filter)
			if err != nil || x == nil {
				return employeesFound{}
			}
			return employeesFound{id: str(x["id"]), grades: strs(x["grades"]), ok: true}
		}
		location := employeesMemo(func(name string) employeesFound {
			return find("location", bson.M{"name": employeesFold(name), "status": "Active"})
		})
		department := employeesMemo(func(name string) employeesFound {
			return find("department", bson.M{"name": employeesFold(name), "status": "Active"})
		})
		designation := employeesMemo(func(key string) employeesFound {
			name, dept, _ := strings.Cut(key, "\x00")
			return find("designation", bson.M{"name": employeesFold(name), "department_id": dept, "status": "Active"})
		})
		shift := employeesMemo(func(code string) employeesFound {
			return find("shift", bson.M{"code": strings.ToUpper(code), "status": "Active"})
		})
		// Every employee id, loaded once: as stored for the manager check (id=$1) and upper-cased for the code
		// checks (upper(id)=upper($1)), which a per-row case-insensitive match could only answer by scanning the
		// collection. Rows this import adds join both sets as they are added, as each INSERT was visible to the
		// next row's checks.
		idRows, err := tx.C("employee").Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"_id": 1}))
		if err != nil {
			return err
		}
		empIDs, takenUpper := make(map[string]bool, len(idRows)), make(map[string]bool, len(idRows))
		for _, r := range idRows {
			id := str(r["id"])
			empIDs[id], takenUpper[strings.ToUpper(id)] = true, true
		}
		// The PANs on file among the ones the rows carry, in one read (employee_gov_id.pan has no index).
		panOnFile := map[string]bool{}
		var pans []string
		for _, x := range b.Rows {
			if p := strings.ToUpper(strings.TrimSpace(x.PAN)); rePAN.MatchString(p) {
				pans = append(pans, p)
			}
		}
		if len(pans) > 0 {
			got, err := tx.C("employee_gov_id").Distinct(ctx, "pan", bson.M{"pan": bson.M{"$in": pans}})
			if err != nil {
				return err
			}
			for _, p := range got {
				panOnFile[str(p)] = true
			}
		}
		// Employee codes are handed out in memory, as NextEmployeeCode does row by row: each series is locked once
		// (FOR UPDATE) and its next_no written once at the end.
		codeKey := employeesMemo(func(typ string) string { return employeesCodeKey(ctx, tx, typ) })
		type codeSeries struct {
			prefix    string
			pad, next int
		}
		series := map[string]*codeSeries{}
		var seriesKeys []string
		nextCode := func(typ string) (string, error) {
			key := codeKey(typ)
			sr := series[key]
			if sr == nil {
				row, err := tx.C("employee_code_series").Lock(ctx, bson.M{"key": key})
				if err != nil {
					return "", err
				}
				if row == nil {
					return "", bad("No employee code series for %s", key)
				}
				sr = &codeSeries{prefix: str(row["prefix"]), pad: employeesInt(row["pad"]), next: employeesInt(row["next_no"])}
				series[key] = sr
				seriesKeys = append(seriesKeys, key)
			}
			for {
				code := fmt.Sprintf("%s%0*d", sr.prefix, sr.pad, sr.next)
				sr.next++
				if !takenUpper[strings.ToUpper(code)] {
					return code, nil
				}
			}
		}
		// The documents of the rows to create, written per collection in one InsertMany each (employee first, for
		// the foreign keys of the others).
		insertOrder := []string{"employee", "employee_personal", "employee_gov_id", "gross_history", "employee_bank", "structure_map", store.UniformSizeColl}
		pending := map[string][]any{}
		flush := func() error {
			for _, coll := range insertOrder {
				if err := tx.C(coll).InsertMany(ctx, pending[coll]); err != nil {
					return err
				}
				pending[coll] = nil
			}
			return nil
		}
		for i, x := range b.Rows {
			var errs []string
			add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
			x.PAN, x.IFSC = strings.ToUpper(strings.TrimSpace(x.PAN)), strings.ToUpper(strings.TrimSpace(x.IFSC))
			if x.Code != "" && !reCode.MatchString(x.Code) {
				add("Employee Code can have letters, numbers and - _ . only, up to 20 characters")
			}
			if strings.TrimSpace(x.Name) == "" {
				add("Full Name is required")
			}
			if x.Type != "Permanent" && x.Type != "Retainer" && x.Type != "Contractual" {
				add("Employment Type must be Permanent, Retainer or Contractual")
			}
			if !isDate(x.DOJ) {
				add("Date of Joining must be a date like 2023-06-12")
			}
			if x.Gross <= 0 {
				add("Monthly Gross must be a number above 0")
			}
			var locID, deptID, desID string
			if l := location(x.Location); !l.ok {
				add("Location “%s” not in master", x.Location)
			} else {
				locID = l.id
			}
			if d := department(x.Department); !d.ok {
				add("Department “%s” not in master", x.Department)
			} else {
				deptID = d.id
				g := designation(x.Designation + "\x00" + deptID)
				desID = g.id
				if !g.ok {
					add("Designation “%s” does not exist under %s", x.Designation, x.Department)
				} else if !containsStr(g.grades, x.Grade) {
					add("Grade %s is not mapped to %s", x.Grade, x.Designation)
				}
			}
			if sh := shift(x.Shift); !sh.ok {
				add("Shift “%s” not in master", x.Shift)
			} else if locID != "" && deptID != "" {
				if ok, err := shiftRowFor(ctx, tx, sh.id, deptID, locID); err != nil {
					return err
				} else if !ok {
					add("%s", shiftNotForMsg(strings.ToUpper(x.Shift), x.Department, x.Location))
				}
			}
			if x.ManagerID != "" && !empIDs[x.ManagerID] {
				add("Reporting manager %s not found", x.ManagerID)
			}
			if x.Code != "" {
				up := strings.ToUpper(x.Code)
				if takenUpper[up] || seenCode[up] {
					add("Code %s already exists", x.Code)
				}
				seenCode[up] = true
			}
			if x.PAN != "" {
				if !rePAN.MatchString(x.PAN) {
					add("PAN is not in the right format")
				} else if panOnFile[x.PAN] || seenPAN[x.PAN] {
					add("PAN %s is already used by another employee", x.PAN)
				}
				seenPAN[x.PAN] = true
			}
			if x.Aadhaar != "" && !reAadhaar.MatchString(strings.ReplaceAll(x.Aadhaar, " ", "")) {
				add("Aadhaar must be 12 digits")
			}
			// UAN and ESIC: "No", "NA" and the like mean none; otherwise the right number of digits (REAL-39).
			x.UAN, x.ESIC = statIDClean(x.UAN), statIDClean(x.ESIC)
			if x.UAN != "" && !reUAN.MatchString(x.UAN) {
				add("UAN must be 12 digits")
			}
			if x.ESIC != "" && !reESIC.MatchString(x.ESIC) {
				add("ESIC number must be 10 or 17 digits")
			}
			if x.Mobile != "" && !reMobile.MatchString(x.Mobile) {
				add("Mobile must be 10 digits")
			}
			if x.Email != "" && !reEmail.MatchString(x.Email) {
				add("Email is not valid")
			}
			if x.DOB != "" && !isDate(x.DOB) {
				add("Date of Birth must be a valid date")
			}
			if x.AccountNo != "" && (!reAcct.MatchString(x.AccountNo) || !reIFSC.MatchString(x.IFSC)) {
				add("Account number must be 6 to 18 digits with a valid IFSC")
			}
			sizes, serr := importSizes(ctx, tx, map[string]string{"shirt": x.Shirt, "waist": x.Waist, "length": x.Length, "shoe": x.Shoe})
			errs = append(errs, serr...)
			res := importResult{Line: i + 2, Code: x.Code, Errors: errs}
			if len(errs) > 0 {
				failed++
			}
			if len(errs) == 0 && !b.Apply && x.Code != "" {
				// A valid row's code can be a later row's manager, in the preview as when applying (PPL-098).
				empIDs[x.Code], takenUpper[strings.ToUpper(x.Code)] = true, true
			}
			if len(errs) == 0 && b.Apply {
				id := x.Code
				if id == "" {
					var err error
					if id, err = nextCode(x.Type); err != nil {
						if ferr := flush(); ferr != nil {
							return ferr // the rows before this one were already written, and their error came first
						}
						return err
					}
				}
				empIDs[id], takenUpper[strings.ToUpper(id)] = true, true
				res.Code = id
				docs := []struct {
					coll string
					doc  bson.M
				}{
					{"employee", bson.M{"_id": id, "name": x.Name, "type": x.Type, "doj": x.DOJ, "department_id": deptID, "designation_id": desID, "grade": x.Grade,
						"location_id": locID, "shift_code": strings.ToUpper(x.Shift), "manager_id": employeesNullIf(x.ManagerID), "status": "Active", "verified": true}},
					{"employee_personal", bson.M{"employee_id": id, "dob": employeesNullIf(x.DOB), "gender": employeesNullIf(x.Gender),
						"mobile": employeesNullIf(x.Mobile), "email": employeesNullIf(x.Email)}},
					{"employee_gov_id", bson.M{"employee_id": id, "pan": employeesNullIf(x.PAN), "aadhaar": employeesNullIf(strings.ReplaceAll(x.Aadhaar, " ", "")),
						"uan": employeesNullIf(x.UAN), "esic": employeesNullIf(x.ESIC)}},
					{"gross_history", bson.M{"employee_id": id, "from_month": x.DOJ[:7], "gross": x.Gross, "reason": "Imported", "created_by": c.Name}},
				}
				if x.AccountNo != "" {
					docs = append(docs, struct {
						coll string
						doc  bson.M
					}{"employee_bank", bson.M{"employee_id": id, "bank_name": or(x.BankName, "Bank"), "account_no": x.AccountNo, "ifsc": x.IFSC,
						"from_month": x.DOJ[:7], "created_by": c.Name}})
				}
				if defaultStructure != "" {
					docs = append(docs, struct {
						coll string
						doc  bson.M
					}{"structure_map", bson.M{"employee_id": id, "from_date": "1900-01-01", "structure_id": defaultStructure}})
				}
				if d := sizesDoc(id, sizes, c.Name+" ("+c.Role+")", tx.Now()); d != nil {
					docs = append(docs, struct {
						coll string
						doc  bson.M
					}{store.UniformSizeColl, d})
				}
				for _, d := range docs {
					pending[d.coll] = append(pending[d.coll], d.doc)
					if d.coll == "employee" {
						created = append(created, d.doc)
					}
				}
			}
			results = append(results, res)
		}
		if b.Apply {
			// The rows are written even when some failed, as each valid row's INSERTs were: a write error still
			// wins over the list of row errors.
			if err := flush(); err != nil {
				return err
			}
			for _, key := range seriesKeys {
				if _, err := tx.C("employee_code_series").Update(ctx, bson.M{"key": key}, bson.M{"$set": bson.M{"next_no": series[key].next}}); err != nil {
					return err
				}
			}
		}
		if b.Apply && failed > 0 {
			return badD(fmt.Sprintf("%d row(s) have errors. Nothing was imported. Fix them and try again.", failed), results)
		}
		if b.Apply {
			// Imported people may upload their missing documents within the window, from today (doc_collect.go).
			if err := docWindowsForImport(ctx, tx, created); err != nil {
				return err
			}
			return audit(ctx, tx, c, fmt.Sprintf("Imported %d employees", len(results)), "employee", "", nil, nil)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"applied": b.Apply, "ok": len(results) - failed, "failed": failed, "results": results}, nil
}

// employeesTime parses a stored YYYY-MM-DD date as a time at midnight UTC, as dates were always read.
func employeesTime(v any) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", str(v))
	return t, err == nil
}
