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

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Round 2 server gaps behind the Dashboard, Employees, Employee file, Work policies, Masters and
// Nominations pages (prototype parity). Every rule of the existing handlers is kept: rights, data
// scope, masking of confidential groups, and nobody acting on their own record.

// ---------- Dashboard ----------

// dashPayStatus is the open payroll month and where it stands. Every role sees the card
// (prototype dsOverview: "Payroll, <month>" with S.run.status).
func (s *Server) dashPayStatus(ctx context.Context) map[string]any {
	ym := openYm(ctx, s.st.DB)
	st := "Open"
	if m, err := loadPayMonth(ctx, s.st.DB, ym, false); err == nil && m != nil {
		st = m.Status
	}
	return map[string]any{"ym": ym, "label": ymLabel(ym), "status": st}
}

// dashRecent is "Recent activity": the whole audit log for roles that manage access, otherwise only
// activity on employees (and on their requests) inside the caller's dashboard scope.
func (s *Server) dashRecent(ctx context.Context, c *Ctx) []store.Row {
	last8 := options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(8).SetProjection(bson.M{"_id": 0, "action": 1, "actor": 1, "at": 1})
	if c.Level("acc.roles") >= 1 || c.Level("acc.users") >= 1 {
		rows, err := s.st.DB.C("audit_log").Find(ctx, nil, last8)
		if err != nil {
			return []store.Row{}
		}
		return rows
	}
	sc := access.Scope{Access: "self", EmployeeID: c.EmployeeID}
	if c.Level("dash.overview") >= 1 && c.Level("emp.directory") >= 1 {
		sc = c.Scope
	}
	rows, err := ui2PeopleRecent(ctx, s.st.DB, sc)
	if err != nil {
		return []store.Row{}
	}
	return rows
}

// ui2PeopleRecent is the last 8 entries of the past 180 days about an employee in scope (with their department), or
// about a request whose subject (employee, offer or candidate, with a department) is in scope, newest first.
//
// Postgres answered this with two EXISTS semi-joins. Joining every audit row of the window until 8 match is far too
// slow when few match (a narrow scope), so for a limited scope the employees and requests in scope are worked out
// first and the audit log is read once by (entity, entity_id), like myviewRecent. For everyone (no department
// limit) nearly every row matches, so the newest rows are read in batches and checked a batch at a time.
func ui2PeopleRecent(ctx context.Context, q store.Q, sc access.Scope) ([]store.Row, error) {
	scope, err := employeesScopeAt(ctx, q, sc, "", "_id") // the same fields name the person in employee, offer and candidate
	if err != nil {
		return nil, err
	}
	depts, err := q.C("department").Distinct(ctx, "_id", bson.M{})
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().AddDate(0, 0, -180)
	last8 := options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(8).SetProjection(bson.M{"_id": 0, "action": 1, "actor": 1, "at": 1})
	if sc.Access != "all" || len(sc.Departments) > 0 {
		emps, err := ui2PeopleRecentIn(ctx, q, "employee", scope, depts, nil)
		if err != nil {
			return nil, err
		}
		reqs, err := ui2PeopleRecentRequests(ctx, q, scope, depts, nil)
		if err != nil {
			return nil, err
		}
		return q.C("audit_log").Find(ctx, bson.M{"at": bson.M{"$gt": cutoff}, "$or": bson.A{
			bson.M{"entity": "employee", "entity_id": bson.M{"$in": emps}},
			bson.M{"entity": "approval_request", "entity_id": bson.M{"$in": reqs}},
		}}, last8)
	}
	out := []store.Row{}
	filter := bson.M{"at": bson.M{"$gt": cutoff}, "entity": bson.M{"$in": bson.A{"employee", "approval_request"}}}
	for size := int64(32); ; size = min(size*2, 2048) {
		batch, err := q.C("audit_log").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(size).
			SetProjection(bson.M{"_id": 1, "action": 1, "actor": 1, "at": 1, "entity": 1, "entity_id": 1}))
		if err != nil {
			return nil, err
		}
		var empIDs, reqIDs []any
		for _, r := range batch {
			if r["entity"] == "employee" {
				empIDs = append(empIDs, r["entity_id"])
			} else {
				reqIDs = append(reqIDs, r["entity_id"])
			}
		}
		emps, reqs := []any{}, []any{}
		if len(empIDs) > 0 {
			if emps, err = ui2PeopleRecentIn(ctx, q, "employee", scope, depts, empIDs); err != nil {
				return nil, err
			}
		}
		if len(reqIDs) > 0 {
			if reqs, err = ui2PeopleRecentRequests(ctx, q, scope, depts, reqIDs); err != nil {
				return nil, err
			}
		}
		ok := map[string]bool{}
		for _, id := range emps {
			ok["employee\x00"+str(id)] = true
		}
		for _, id := range reqs {
			ok["approval_request\x00"+str(id)] = true
		}
		for _, r := range batch {
			if ok[str(r["entity"])+"\x00"+str(r["entity_id"])] {
				out = append(out, store.Row{"action": r["action"], "actor": r["actor"], "at": r["at"]})
				if len(out) == 8 {
					return out, nil
				}
			}
		}
		if int64(len(batch)) < size {
			return out, nil
		}
		filter["_id"] = bson.M{"$lt": batch[len(batch)-1]["id"]}
	}
}

// ui2PeopleRecentIn is the ids of documents of coll (employee, offer or candidate) in scope whose department exists
// (… JOIN department d ON d.id=e.department_id AND scope), among the given ids when among is not nil.
func ui2PeopleRecentIn(ctx context.Context, q store.Q, coll string, scope bson.M, depts []any, among []any) ([]any, error) {
	conds := []bson.M{scope, {"department_id": bson.M{"$in": depts}}}
	if among != nil {
		conds = append(conds, bson.M{"_id": bson.M{"$in": among}})
	}
	return q.C(coll).Distinct(ctx, "_id", employeesAnd(conds...))
}

// ui2PeopleRecentRequests is the ids of approval requests whose subject (the approval_subject rows with id
// COALESCE(employee_id, offer_id, candidate_id)) is in scope with a department, among the given ids when among is
// not nil.
func ui2PeopleRecentRequests(ctx context.Context, q store.Q, scope bson.M, depts []any, among []any) ([]any, error) {
	var reqs []store.Row
	var sids []any
	if among != nil {
		var err error
		if reqs, err = q.C("approval_request").Find(ctx, bson.M{"_id": bson.M{"$in": among}},
			options.Find().SetProjection(bson.M{"_id": 1, "employee_id": 1, "offer_id": 1, "candidate_id": 1})); err != nil {
			return nil, err
		}
		for _, r := range reqs {
			if sid := approvalSubjectID(r); sid != "" {
				sids = append(sids, sid)
			}
		}
		if len(sids) == 0 {
			return []any{}, nil
		}
	}
	// The subjects in scope: the view is a UNION ALL, so a match in any of its branches counts, as EXISTS did.
	subjects := map[string]bool{}
	var list []any
	for _, coll := range []string{"employee", "offer", "candidate"} {
		ids, err := ui2PeopleRecentIn(ctx, q, coll, scope, depts, sids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if k := str(id); !subjects[k] {
				subjects[k] = true
				list = append(list, id)
			}
		}
	}
	out := []any{}
	if among != nil {
		for _, r := range reqs {
			if subjects[approvalSubjectID(r)] {
				out = append(out, r["id"])
			}
		}
		return out, nil
	}
	if len(list) == 0 {
		return out, nil
	}
	in := bson.M{"$in": list}
	for _, f := range []bson.M{
		{"employee_id": in},
		{"employee_id": nil, "offer_id": in},
		{"employee_id": nil, "offer_id": nil, "candidate_id": in},
	} {
		ids, err := q.C("approval_request").Distinct(ctx, "_id", f)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return out, nil
}

// dashPayOpen is the open payroll month as computed so far, with the dashboard filters (prototype
// dsPayroll): people, gross, net and cost, gross by city and by type, and what blocks the run.
func (s *Server) dashPayOpen(ctx context.Context, c *Ctx, f map[string]string) (map[string]any, error) {
	ym := openYm(ctx, s.st.DB)
	m, err := loadPayMonth(ctx, s.st.DB, ym, false)
	if err != nil {
		return nil, err
	}
	all, err := loadRows(ctx, s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	rows := filterRows(c, all, f)
	var earned, net, ctc float64
	holds, missing, carry := 0, 0, 0
	byCity, byType := map[string]float64{}, map[string]float64{}
	for _, o := range rows {
		er := o.R.ErPF + o.R.Admin + o.R.Edli + o.R.ErESI + o.R.Med + o.R.LwfEr
		earned += o.TotalEarn
		net += o.Net
		ctc += o.TotalEarn + er
		byCity[o.City] += o.TotalEarn
		byType[o.Type] += o.TotalEarn
		if o.Hold {
			holds++
		}
		if o.Missing > 0 {
			missing++
		}
		if o.Carry > 0 {
			carry++
		}
	}
	k := func(v float64) int64 { return int64(math.Round(v / 1000)) }
	names := []string{}
	for n := range byCity {
		names = append(names, n)
	}
	sort.Strings(names)
	cities := []map[string]any{}
	for _, n := range names {
		if v := k(byCity[n]); v != 0 {
			cities = append(cities, map[string]any{"label": n, "value": v})
		}
	}
	types := []map[string]any{}
	for _, t := range []string{"Permanent", "Contractual", "Retainer"} {
		types = append(types, map[string]any{"label": t, "value": k(byType[t])})
	}
	return map[string]any{"ym": ym, "status": m.Status, "people": len(rows), "earned": calc.Round2(earned), "net": calc.Round2(net), "ctc": calc.Round2(ctc),
		"byCity": cities, "byType": types, "holds": holds, "missing": missing, "carry": carry}, nil
}

// dashAttendance is the Attendance tab (prototype dsAttendance) for this month so far, for the active
// employees in the caller's scope that match the city, department and type filters. Like the prototype, every
// role that has the dashboard sees it: it returns counts and shares only, never a person's records.
func (s *Server) dashAttendance(ctx context.Context, c *Ctx, city, dept, typ string) (any, error) {
	if err := c.Need("dash.overview", 1); err != nil {
		return nil, err
	}
	t := today()
	ym := t[:7]
	from, to, err := monthRange(ym)
	if err != nil {
		return nil, err
	}
	filter := bson.M{"status": "Active"}
	if typ != "" {
		filter["type"] = typ
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, filter, func(e *emp) bool { return (city == "" || e.CityID == city) && (dept == "" || e.Dept == dept) })
	if err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	tot := map[string]int{"Present": 0, "Late": 0, "Half day": 0, "Missing punch": 0, "No punch": 0}
	byLoc := map[string][2]int{}
	onLeave := 0
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		v := byLoc[e.Location]
		for d := from; d <= to && d <= t; d = policy.AddDays(d, 1) {
			if d < e.DOJ || (e.LWD != nil && d > *e.LWD && e.Status != "Active") {
				continue
			}
			if m.leaveOn(e, d, false) != nil {
				continue
			}
			st, _ := m.status(e, p, d)
			key := summaryKey(st)
			if key == "" {
				continue
			}
			tot[key]++
			if key == "Late" {
				v[0]++
			}
			if key == "Present" || key == "Late" || key == "Half day" {
				v[1]++
			}
		}
		byLoc[e.Location] = v
		if m.leaveOn(e, t, false) != nil {
			onLeave++
		}
	}
	late := []map[string]any{}
	for l, v := range byLoc {
		pct := 0
		if v[1] > 0 {
			pct = int(math.Round(float64(v[0]) / float64(v[1]) * 100))
		}
		late = append(late, map[string]any{"label": l, "value": pct})
	}
	sort.Slice(late, func(i, j int) bool {
		if late[i]["value"].(int) != late[j]["value"].(int) {
			return late[i]["value"].(int) > late[j]["value"].(int)
		}
		return late[i]["label"].(string) < late[j]["label"].(string)
	})
	if len(late) > 6 {
		late = late[:6]
	}
	idl := ids(emps)
	out := map[string]any{"ym": ym, "counts": tot, "workDays": tot["Present"] + tot["Late"] + tot["Half day"], "onLeave": onLeave, "lateByLocation": late, "people": len(emps)}
	open := map[string]int64{"regularize": 0, "leave": 0, "swap": 0}
	if counts, err := s.st.DB.C("approval_request").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "status", Value: "Pending"}, {Key: "employee_id", Value: bson.D{{Key: "$in", Value: idl}}},
			{Key: "wf", Value: bson.D{{Key: "$in", Value: bson.A{"regularize", "leave", "swap"}}}}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$wf"}, {Key: "n", Value: bson.D{{Key: "$sum", Value: int64(1)}}}}}},
	}); err == nil {
		for _, x := range counts {
			open[str(x["id"])], _ = x["n"].(int64)
		}
	}
	out["open"] = open
	// Roster changes for the people in scope, for every role that sees the tab (prototype dsAttendance).
	{
		month := bson.M{"date": bson.M{"$gte": from, "$lte": to}, "by_dev": false, "employee_id": bson.M{"$in": idl}}
		n, _ := s.st.DB.C("roster_log").Count(ctx, month)
		nl, _ := s.st.DB.C("roster_log").Count(ctx, employeesAnd(month, bson.M{"late": true}))
		// A property is flagged when its late share passes the limit of its work policy (roster discipline, 30% by default).
		groups, err := ui2PeopleRosterGroups(ctx, s.st.DB, month)
		if err != nil {
			return nil, err
		}
		byID := map[string]*emp{}
		for _, e := range emps {
			byID[e.ID] = e
		}
		alerts := []store.Row{}
		for _, x := range groups {
			limit := 30
			if e := byID[fmt.Sprint(x["emp"])]; e != nil {
				if p, _ := ts.policyFor(e); p.S.RS.Alert > 0 {
					limit = p.S.RS.Alert
				}
			}
			if pct, _ := x["pct"].(int32); int(pct) > limit {
				delete(x, "emp")
				x["limit"] = limit
				alerts = append(alerts, x)
			}
		}
		out["roster"] = map[string]any{"changes": n, "late": nl, "alerts": alerts}
	}
	return out, nil
}

// ui2PeopleRosterGroups groups roster changes by location and department (key "<location> · <department>", null
// when the change has no department) with at least 5 changes: n changes, late of them late, pct the late share
// rounded, and emp the lowest employee code among them; the highest share first.
func ui2PeopleRosterGroups(ctx context.Context, q store.Q, month bson.M) ([]store.Row, error) {
	rows, err := q.C("roster_log").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: month}},
		employeesLookup("location", "location_id", "l"), employeesUnwind("l"),
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "$concat", Value: bson.A{"$l.name", " · ", "$department"}}}},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: int64(1)}}},
			{Key: "late", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{"$late", int64(1), int64(0)}}}}}},
			{Key: "emp", Value: bson.D{{Key: "$min", Value: "$employee_id"}}},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "n", Value: bson.D{{Key: "$gte", Value: 5}}}}}},
	})
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, x := range rows {
		n, _ := x["n"].(int64)
		late, _ := x["late"].(int64)
		out = append(out, store.Row{"key": x["id"], "n": n, "late": late, "pct": int32(math.Round(100 * float64(late) / float64(n))), "emp": x["emp"]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i]["pct"] != out[j]["pct"] {
			return out[i]["pct"].(int32) > out[j]["pct"].(int32)
		}
		return str(out[i]["key"]) < str(out[j]["key"])
	})
	return out, nil
}

// ---------- Employee record ----------

// employeeExtras adds to GET /api/employees/{id}: state and region, the work policy that applies and
// why (with probation, optional holidays, casual and privilege leave), the salary structure name and
// its history (not a salary figure, so shown to every role that can see the person), the bank account
// history under the bank masking rule, and, for someone who has left, what a rejoin needs to know.
func (s *Server) employeeExtras(ctx context.Context, c *Ctx, e store.Row, id string) {
	own := id == c.EmployeeID
	q := s.st.DB
	var state, region string
	if ci := employeesCityOf(ctx, q, e["location_id"]); ci != nil {
		if st, _ := q.C("state").FindOne(ctx, bson.M{"_id": ci["state_id"]}); st != nil {
			if rg, _ := q.C("region").FindOne(ctx, bson.M{"_id": st["region_id"]}); rg != nil {
				state, region = str(st["name"]), str(rg["name"])
			}
		}
	}
	e["state"], e["region"] = state, region
	if ts, err := loadTimeSet(ctx, q); err == nil {
		if x, err := loadEmp(ctx, q, id); err == nil {
			p, via := ts.policyFor(x)
			// Probation by the person's grade when the policy sets it, else the policy's.
			pol := map[string]any{"id": p.ID, "name": p.Name, "via": via, "probMonths": p.S.ProbMonths(x.Grade), "probEnds": nil, "optHol": p.S.OptHol}
			if end := p.S.ProbEnd(x.Grade, x.DOJ); end != "" && isDate(x.DOJ) {
				pol["probEnds"] = end
			}
			lt := func(n string) map[string]any {
				cfg := ts.typeCfg(p, n)
				if t, ok := ts.types[n]; !ok || t.Status != "Active" || cfg == nil || !cfg.On || !containsStr(cfg.Types, x.Type) || !leaveTypeOK(x.Type, n) {
					return map[string]any{"on": false}
				}
				return map[string]any{"on": true, "days": cfg.Days, "from": cfg.From, "credit": cfg.Credit}
			}
			pol["cl"], pol["pl"] = lt("Casual leave"), lt("Privilege leave")
			if pl := ts.plan(p); pl != nil {
				pol["leavePlan"] = pl.Name
			}
			e["policy"] = pol
		}
	}
	name, std := structureFor(ctx, q, id, today())
	e["structure"] = name
	// The structure's standard rates (not confidential: the same for everyone on the version), so the drawer can say
	// "5% of gross, paid every month" to roles that cannot open the salary structures.
	e["structureStd"] = map[string]any{"varPct": std.VarPct, "varCycle": std.VarCycle, "tdsRate": std.TdsRate}
	e["structureHistory"] = ui2PeopleStructureHistory(ctx, q, id)
	if own || c.Level("emp.bank") >= 1 {
		rows, err := q.C("employee_bank").Find(ctx, bson.M{"employee_id": id}, options.Find().
			SetSort(bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}).
			SetProjection(bson.M{"_id": 0, "bank_name": 1, "account_no": 1, "ifsc": 1, "from_month": 1, "created_by": 1, "created_at": 1}))
		if err != nil {
			rows = []store.Row{}
		}
		for _, b := range rows {
			b["account_no"], b["ifsc"] = c.Show("bank", b["account_no"], id), c.Show("bank", b["ifsc"], id)
		}
		e["bankHistory"] = rows
	}
	if e["status"] == "Inactive" {
		before, _ := q.C("employee_service").Count(ctx, bson.M{"employee_id": id})
		pending := openRejoin(ctx, q, id) != "" // a shortlist on its way, or a rejoin offer raised before stages
		var linked *string                      // the code they rejoined on, the latest when more than one
		if n, _ := q.C("employee").FindOne(ctx, bson.M{"prev_code": id}, options.FindOne().
			SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetProjection(bson.M{"_id": 1})); n != nil {
			code := str(n["id"])
			linked = &code
		}
		rh := "Yes"
		if x, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"rehire": 1})); x != nil && str(x["rehire"]) != "" {
			rh = str(x["rehire"])
		}
		e["rejoin"] = map[string]any{"before": before, "pending": pending, "rehire": rh, "newCode": linked}
	}
}

// ui2PeopleStructureHistory is the salary structures mapped to an employee, oldest first: name and from date.
func ui2PeopleStructureHistory(ctx context.Context, q store.Q, id string) []store.Row {
	out := []store.Row{}
	maps, err := q.C("structure_map").Find(ctx, bson.M{"employee_id": id}, options.Find().SetSort(bson.D{{Key: "from_date", Value: 1}}))
	if err != nil || len(maps) == 0 {
		return out
	}
	sids := []any{}
	for _, m := range maps {
		sids = append(sids, m["structure_id"])
	}
	structs, err := q.C("salary_structure").Find(ctx, bson.M{"_id": bson.M{"$in": sids}}, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return out
	}
	name := map[string]any{}
	for _, x := range structs {
		name[str(x["id"])] = x["name"]
	}
	for _, m := range maps {
		if n, ok := name[str(m["structure_id"])]; ok {
			out = append(out, store.Row{"name": n, "from_date": m["from_date"]})
		}
	}
	return out
}

// employeeReactivate is the prototype's "Undo inactive": only for someone marked inactive by mistake.
// A settled or settling exit, or a rejoin already raised, needs the Rejoin flow instead.
func (s *Server) employeeReactivate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.status", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if id == c.EmployeeID {
		return nil, forbidden("Nobody can reactivate their own record")
	}
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &b); err != nil {
			return nil, err
		}
	}
	reason := strings.TrimSpace(b.Reason)
	if reason == "" {
		reason = "Marked inactive by mistake"
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		full, err := tx.C("employee").Lock(ctx, bson.M{"_id": id})
		if err != nil || full == nil {
			return orNotFound(err)
		}
		e := store.Row{"name": full["name"], "status": full["status"], "lwd": full["lwd"], "exit_reason": full["exit_reason"]} // what the audit records as before
		if e["status"] != "Inactive" {
			return bad("%s is not inactive", e["name"])
		}
		if ok, _ := tx.C("fnf").Exists(ctx, bson.M{"employee_id": id, "status": bson.M{"$in": bson.A{"Pending approval", "Approved"}}}); ok {
			return bad("A full and final settlement is pending or approved for %s. Use Rejoin instead", e["name"])
		}
		if ok, _ := tx.C("employee").Exists(ctx, bson.M{"prev_code": id}); ok {
			return bad("%s has already rejoined on a new code", e["name"])
		}
		if open := openRejoin(ctx, tx, id); open != "" {
			return bad("%s is open for %s. Close it first, or let it finish", open, e["name"])
		}
		status := "Active" // or Pre-boarding again when the joining date is still ahead
		if str(full["doj"]) > today() {
			status = "Pre-boarding"
		}
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": id},
			bson.M{"$set": bson.M{"status": status, "lwd": nil, "exit_reason": nil, "exit_status": nil, "resign_at": nil}}); err != nil {
			return err
		}
		if err := uniformRefundRestore(ctx, tx, id); err != nil { // the uniform refund cancelled on leaving is due again
			return err
		}
		old := "Inactive"
		if l, _ := e["lwd"].(string); l != "" {
			old = "Inactive from " + l
		}
		if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": "Status", "old_value": old, "new_value": "Active", "reason": reason,
			"effective_from": today(), "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Reactivated employee (undo inactive)", "employee", id, e, map[string]any{"reason": reason})
	})
}

// employeeRejoin used to raise a joining request straight from the record. Since onboarding runs in stages, a
// rejoin starts as a shortlist prefilled from the old record (Onboarding › Shortlist, GET /api/candidates/rejoin).
func (s *Server) employeeRejoin(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	return nil, bad("A rejoin starts with a shortlist: use Rejoin on the record, which opens Onboarding › Shortlist prefilled")
}

// ---------- Employee nominations (Employee file, prototype fNom) ----------

func docOn(d nomDoc) bool { return d.On == nil || *d.On }

// nomDocCount is documents attached against documents needed (switched-on, required ones).
func nomDocCount(sc nomScheme, people int, files map[string]store.Row) (int, int) {
	have, need := 0, 0
	for _, d := range sc.Docs {
		if !docOn(d) || !d.Req {
			continue
		}
		if d.Per == "employee" {
			need++
			if files["e:"+d.ID] != nil {
				have++
			}
			continue
		}
		for i := 0; i < people; i++ {
			need++
			if files[fmt.Sprintf("n%d:%s", i, d.ID)] != nil {
				have++
			}
		}
	}
	return have, need
}

// employeeNominations lists every nomination that applies to one employee with all its versions.
// Payroll (claims) and the person see nominees in full; roles with medical rights see them masked.
func (s *Server) employeeNominations(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	own := id == c.EmployeeID
	full := own || c.Level("pay.claims") >= 1
	if !own {
		if c.Level("pay.claims") < 1 && c.Level("emp.medical") < 1 {
			return nil, forbidden("Your role cannot see nominations")
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	states, err := nomStates(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.DB.C("nomination").Find(ctx, bson.M{"employee_id": id}, options.Find().SetSort(bson.D{{Key: "scheme_id", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	mask := func(people any) any {
		l, ok := people.([]any)
		if full || !ok {
			return people
		}
		out := []any{}
		for _, x := range l {
			p, _ := x.(map[string]any)
			q := map[string]any{}
			for k, v := range p {
				q[k] = v
			}
			for _, k := range []string{"name", "dob", "guardian"} {
				q[k] = c.Show("medical", p[k], id)
			}
			out = append(out, q)
		}
		return out
	}
	out := []map[string]any{}
	for _, st := range states {
		vers := []map[string]any{}
		var cur map[string]any
		for _, x := range rows {
			if int(x["scheme_id"].(int32)) != st.Scheme.ID {
				continue
			}
			v := map[string]any{"id": x["id"], "ver": len(vers) + 1, "status": x["status"], "submitted_at": x["submitted_at"], "decided_by": x["decided_by"],
				"decided_at": x["decided_at"], "note": x["note"], "people": mask(x["people"])}
			vers = append(vers, v)
			if st.Current != nil && (*st.Current)["id"] == x["id"] {
				cur = v
			}
		}
		item := map[string]any{"scheme": st.Scheme, "state": st.State, "due": st.Due, "overdue": st.Overdue, "current": cur, "versions": vers}
		if cur != nil {
			files, _ := docsOf(ctx, s.st.DB, "nomination", fmt.Sprintf("%s/%d", id, st.Scheme.ID))
			n := 0
			if l, ok := cur["people"].([]any); ok {
				n = len(l)
			}
			h, need := nomDocCount(st.Scheme, n, files)
			item["docs"] = map[string]int{"have": h, "need": need}
		}
		out = append(out, item)
	}
	return out, nil
}

// nominationRemind records a reminder to an employee who has not submitted a nomination yet.
func (s *Server) nominationRemind(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeID string `json:"employeeId"`
		SchemeID   int    `json:"schemeId"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	e, _ := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": b.EmployeeID})
	ns, _ := s.st.DB.C("nomination_scheme").FindOne(ctx, bson.M{"_id": b.SchemeID})
	if e == nil || ns == nil {
		return nil, notFound("Employee or nomination not found")
	}
	name, scheme := str(e["name"]), str(ns["name"])
	if e["status"] != "Active" {
		return nil, bad("%s is not active", name)
	}
	var at string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("nomination_reminder").Insert(ctx, bson.M{"employee_id": b.EmployeeID, "scheme_id": b.SchemeID, "by_name": c.Name + " (" + c.Role + ")"}); err != nil {
			return err
		}
		at = store.DateOf(tx.Now()) // the reminder's at
		return audit(ctx, tx, c, "Reminder sent to "+name+" for "+scheme, "nomination_reminder", b.EmployeeID, nil, b)
	})
	return map[string]string{"reminded": at}, err
}

// ---------- Work policies ----------

// configHistoryAt records a change with the date it applies from (blank means today).
func configHistoryAt(ctx context.Context, tx store.Q, c *Ctx, entity, id string, changes []map[string]any, eff string) error {
	if len(changes) == 0 {
		return nil
	}
	if eff == "" {
		eff = today()
	}
	_, err := tx.C("config_history").Insert(ctx, bson.M{"entity": entity, "entity_id": id, "changes": changes, "by_name": c.Name + " (" + c.Role + ")", "effective_from": eff})
	return err
}

func checkEffective(eff string) error {
	if eff != "" && !isDate(eff) {
		return bad("Applies from must be a date like 2026-12-01")
	}
	return nil
}

// mapMatches says whether an employee falls under one policy mapping.
func mapMatches(e *emp, m polMap) bool {
	v := e.levelRef(m.Level)
	return v != "" && v == m.Ref
}

// policyCounts fills maps[].employees (people on this policy through that mapping, for mappings that
// are on) and returns the people on the policy by grade (prototype mapBlock and notice by grade).
func (s *Server) policyCounts(ctx context.Context, ts *timeSet, p *workPolicy) map[string]int {
	grades := map[string]int{}
	emps, err := loadEmps(ctx, s.st.DB, bson.M{"status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return grades
	}
	n := make([]int, len(p.Maps))
	for _, e := range emps {
		if q, _ := ts.policyFor(e); q.ID != p.ID {
			continue
		}
		grades[e.Grade]++
		for i, m := range p.Maps {
			if m.On && mapMatches(e, m) {
				n[i]++
			}
		}
	}
	for i := range p.Maps {
		if p.Maps[i].On {
			v := n[i]
			p.Maps[i].Employees = &v
		}
	}
	return grades
}

// policyCoverageAll: which policy every employee in scope ends up on and through which mapping, as a
// full matrix {policyId: {via: n}}, plus everyone on the default policy with grade and city.
func (s *Server) policyCoverageAll(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	filter := bson.M{"status": bson.M{"$ne": "Inactive"}}
	if q != "" {
		like := employeesILike("%" + q + "%")
		filter["$or"] = bson.A{bson.M{"name": like}, bson.M{"_id": like}}
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, filter)
	if err != nil {
		return nil, err
	}
	cities := map[string]string{}
	if l, err := s.st.DB.C("city").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1})); err == nil {
		for _, x := range l {
			cities[x["id"].(string)] = x["name"].(string)
		}
	}
	matrix := map[string]map[string]int{}
	rows, defs := []map[string]any{}, []map[string]any{}
	defTotal := 0
	for _, e := range emps {
		p, via := ts.policyFor(e)
		if matrix[p.ID] == nil {
			matrix[p.ID] = map[string]int{}
		}
		matrix[p.ID][via]++
		row := map[string]any{"id": e.ID, "name": e.Name, "type": e.Type, "department": e.Dept, "location": e.Location, "grade": e.Grade, "city": cities[e.CityID],
			"policy": p.Name, "policyId": p.ID, "via": via}
		if pl := ts.plan(p); pl != nil {
			row["plan"] = pl.Name
		}
		if len(rows) < 300 {
			rows = append(rows, row)
		}
		if via == "Default" {
			defTotal++
			if len(defs) < 500 {
				defs = append(defs, row)
			}
		}
	}
	return map[string]any{"rows": rows, "total": len(emps), "matrix": matrix, "defaults": defs, "defaultTotal": defTotal}, nil
}

// moveMapOff switches the same mapping off on every other policy: a location, department, grade and
// so on belongs to one policy at a time. Each policy it leaves records the change in its history.
func moveMapOff(ctx context.Context, tx store.Q, c *Ctx, policyID, level, ref string) error {
	others := bson.M{"level": level, "ref": ref, "policy_id": bson.M{"$ne": policyID}, "on_map": true}
	moved, err := tx.C("work_policy_map").Find(ctx, others, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.M{"policy_id": 1}))
	if err != nil {
		return err
	}
	if _, err := tx.C("work_policy_map").UpdateMany(ctx, others, bson.M{"$set": bson.M{"on_map": false}}); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, x := range moved {
		pid := x["policy_id"].(string)
		if seen[pid] {
			continue
		}
		seen[pid] = true
		ch := []map[string]any{{"field": "applies to " + level + "|" + ref, "to": "off"}}
		if err := configHistory(ctx, tx, c, "work_policy", pid, ch); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, "Work policy mapping moved to another policy", "work_policy", pid, nil, ch); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Leave plans and leave types ----------

func (s *Server) leavePlanHistory(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	return s.st.DB.C("config_history").Find(r.Context(), bson.M{"entity": "leave_plan", "entity_id": r.PathValue("id")}, options.Find().
		SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(100).
		SetProjection(bson.M{"_id": 0, "changes": 1, "by_name": 1, "at": 1, "effective_from": 1}))
}

// leaveTypeStatus makes a leave type inactive (it disappears from new requests; pending ones continue)
// or active again.
func (s *Server) leaveTypeStatus(active bool) authedFn {
	return func(r *http.Request, c *Ctx) (any, error) {
		if err := canPolicies(c, true); err != nil {
			return nil, err
		}
		ctx, name := r.Context(), r.PathValue("name")
		st := map[bool]string{true: "Active", false: "Inactive"}[active]
		var pending int64
		err := s.st.Tx(ctx, func(tx store.Q) error {
			lt, err := tx.C("leave_type").Lock(ctx, bson.M{"name": name})
			if err != nil || lt == nil {
				return notFound("Leave type not found")
			}
			if lt["status"] == st {
				return bad("%s is already %s", name, strings.ToLower(st))
			}
			if _, err := tx.C("leave_type").Update(ctx, bson.M{"name": name}, bson.M{"$set": bson.M{"status": st}}); err != nil {
				return err
			}
			pending, _ = tx.C("approval_request").Count(ctx, bson.M{"wf": "leave", "status": "Pending", "payload.ltype": name})
			return audit(ctx, tx, c, "Leave type “"+name+"” made "+strings.ToLower(st), "leave_type", name, nil, map[string]any{"status": st})
		})
		return map[string]any{"name": name, "status": st, "pending": pending}, err
	}
}

// leaveTypeCopy gives a new leave type the settings of another one in every plan (switched off), or def in a plan
// without the other one.
func leaveTypeCopy(ctx context.Context, tx store.Q, name, from string, def leave.TypeCfg) error {
	plans, err := tx.C("leave_plan").Find(ctx, nil, options.Find().SetProjection(bson.M{"types": 1}))
	if err != nil {
		return err
	}
	for _, p := range plans {
		types, _ := p["types"].(map[string]any)
		next := map[string]any{}
		for k, v := range types {
			next[k] = v
		}
		if v, ok := types[from]; ok {
			next[name] = ui2PeopleJSONConcat(v, map[string]any{"on": false})
		} else {
			next[name] = def
		}
		if _, err := tx.C("leave_plan").Update(ctx, bson.M{"_id": p["id"]}, bson.M{"$set": bson.M{"types": next}}); err != nil {
			return err
		}
	}
	return nil
}

// ui2PeopleJSONConcat is jsonb's a || b for an object b: the union of two objects (b's keys win); a non-object a is
// joined with b in an array.
func ui2PeopleJSONConcat(a any, b map[string]any) any {
	switch x := a.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = v
		}
		for k, v := range b {
			out[k] = v
		}
		return out
	case []any:
		return append(append([]any{}, x...), b)
	}
	return []any{a, b}
}

// ---------- Masters ----------

// masterImpact is how many active employees still use a master item (shown before it is made inactive).
func (s *Server) masterImpact(r *http.Request, c *Ctx) (any, error) {
	t := r.PathValue("type")
	m, err := masterCfg(t)
	if err != nil {
		return nil, err
	}
	if err := c.Need(m.right, 1); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if !reUUID.MatchString(id) {
		return nil, notFound("")
	}
	if ok, err := s.st.DB.C(m.table).Exists(ctx, bson.M{"_id": id}); err != nil || !ok {
		return nil, orNotFound(err)
	}
	n, err := impactCount(ctx, s.st.DB, t, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"n": n}, nil
}
