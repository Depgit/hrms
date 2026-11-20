package api

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/store"
)

// What the Approvals page shows beyond the core engine (prototype vAppr, reqSub, payItems,
// payItemRows, nrPrev): the subject's city, employment type and designation, the raiser's name,
// improvement plans and tax proofs in the inbox, pay-item rows for full and final and deductions,
// and an employee's preview of their own route.

// ---------- Subject details on request rows ----------

// apprUIAddSubject adds city, type, designation, department, location and raised_by_name to request
// rows (list and detail). A full and final without the last working day in its payload gets it
// from the stored settlement. A request whose subject is gone is left as it is.
func apprUIAddSubject(ctx context.Context, q store.Q, rows []store.Row) error {
	ids := []string{}
	for _, r := range rows {
		if id, ok := r["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	reqs, err := approvalsByID(ctx, q, "approval_request", ids)
	if err != nil {
		return err
	}
	var subjIDs, empIDs, offerIDs, candIDs, raisers []string
	for _, r := range reqs {
		subjIDs = append(subjIDs, approvalSubjectID(r))
		empIDs, offerIDs, candIDs = append(empIDs, str(r["employee_id"])), append(offerIDs, str(r["offer_id"])), append(candIDs, str(r["candidate_id"]))
		raisers = append(raisers, str(r["raised_by"]))
	}
	subjects, err := approvalSubjectsOf(ctx, q, subjIDs)
	if err != nil {
		return err
	}
	// The subject's department and place (all optional), the designation of the employee, offer or candidate the
	// request names, who raised it and the settlement of a full and final.
	var deptIDs, locIDs []string
	for _, e := range subjects {
		deptIDs, locIDs = append(deptIDs, str(e["department_id"])), append(locIDs, str(e["location_id"]))
	}
	depts, err := approvalsByID(ctx, q, "department", deptIDs)
	if err != nil {
		return err
	}
	place, err := apprUIPlaces(ctx, q, locIDs)
	if err != nil {
		return err
	}
	emps, err := approvalsByID(ctx, q, "employee", append(empIDs, raisers...))
	if err != nil {
		return err
	}
	offers, err := approvalsByID(ctx, q, "offer", offerIDs)
	if err != nil {
		return err
	}
	cands, err := approvalsByID(ctx, q, "candidate", candIDs)
	if err != nil {
		return err
	}
	var desigIDs []string
	for _, m := range []map[string]store.Row{emps, offers, cands} {
		for _, x := range m {
			desigIDs = append(desigIDs, str(x["designation_id"]))
		}
	}
	desigs, err := approvalsByID(ctx, q, "designation", desigIDs)
	if err != nil {
		return err
	}
	fnfs, err := approvalsByID(ctx, q, "fnf", ids)
	if err != nil {
		return err
	}
	// desigOf is the designation's name for the record of id in m, or nil (a LEFT JOIN that found nothing).
	desigOf := func(m map[string]store.Row, id any) any {
		if x := m[str(id)]; x != nil {
			if g := desigs[str(x["designation_id"])]; g != nil {
				return g["name"]
			}
		}
		return nil
	}
	orBlank := func(v any) any {
		if v == nil {
			return ""
		}
		return v
	}
	for _, row := range rows {
		r := reqs[str(row["id"])]
		if r == nil {
			continue
		}
		e := subjects[approvalSubjectID(r)]
		if e == nil {
			continue
		}
		pl := place[str(e["location_id"])]
		designation := desigOf(emps, r["employee_id"])
		if designation == nil {
			designation = desigOf(offers, r["offer_id"])
		}
		if designation == nil {
			designation = desigOf(cands, r["candidate_id"])
		}
		raisedBy := r["raised_by"]
		if rb := emps[str(r["raised_by"])]; rb != nil && rb["name"] != nil {
			raisedBy = rb["name"]
		}
		var dept any
		if d := depts[str(e["department_id"])]; d != nil {
			dept = d["name"]
		}
		row["city"], row["type"], row["designation"], row["location"], row["raised_by_name"] = orBlank(pl.city), orBlank(e["type"]), orBlank(designation), orBlank(pl.location), raisedBy
		if str(row["department"]) == "" {
			row["department"] = orBlank(dept)
		}
		if f := fnfs[str(r["id"])]; f != nil {
			if lwd := str(f["lwd"]); lwd != "" {
				if p, ok := row["payload"].(map[string]any); ok && str(p["lwd"]) == "" {
					p["lwd"] = lwd
				}
			}
		}
	}
	return nil
}

// apprUIPlace is a location's name and its city's name, each nil when a link is missing (LEFT JOINs).
type apprUIPlace struct{ location, city any }

// apprUIPlaces names the locations and their cities (location → cluster → city).
func apprUIPlaces(ctx context.Context, q store.Q, locIDs []string) (map[string]apprUIPlace, error) {
	locs, err := approvalsByID(ctx, q, "location", locIDs)
	if err != nil {
		return nil, err
	}
	var clusterIDs []string
	for _, l := range locs {
		clusterIDs = append(clusterIDs, str(l["cluster_id"]))
	}
	clusters, err := approvalsByID(ctx, q, "cluster", clusterIDs)
	if err != nil {
		return nil, err
	}
	var cityIDs []string
	for _, k := range clusters {
		cityIDs = append(cityIDs, str(k["city_id"]))
	}
	cities, err := approvalsByID(ctx, q, "city", cityIDs)
	if err != nil {
		return nil, err
	}
	out := map[string]apprUIPlace{}
	for id, l := range locs {
		p := apprUIPlace{location: l["name"]}
		if k := clusters[str(l["cluster_id"])]; k != nil {
			if c := cities[str(k["city_id"])]; c != nil {
				p.city = c["name"]
			}
		}
		out[id] = p
	}
	return out, nil
}

// apprUIPerson is an employee with their department, designation, location and city, all required (the inner
// JOINs of the improvement plan and tax proof lists).
type apprUIPerson struct {
	emp                                     store.Row
	department, designation, location, city any
}

// apprUIPeople loads the employees of ids that have all of department, designation, location, cluster and city.
func apprUIPeople(ctx context.Context, q store.Q, ids []string) (map[string]*apprUIPerson, error) {
	emps, err := approvalsByID(ctx, q, "employee", ids)
	if err != nil {
		return nil, err
	}
	var deptIDs, desigIDs, locIDs []string
	for _, e := range emps {
		deptIDs, desigIDs, locIDs = append(deptIDs, str(e["department_id"])), append(desigIDs, str(e["designation_id"])), append(locIDs, str(e["location_id"]))
	}
	depts, err := approvalsByID(ctx, q, "department", deptIDs)
	if err != nil {
		return nil, err
	}
	desigs, err := approvalsByID(ctx, q, "designation", desigIDs)
	if err != nil {
		return nil, err
	}
	place, err := apprUIPlaces(ctx, q, locIDs)
	if err != nil {
		return nil, err
	}
	out := map[string]*apprUIPerson{}
	for id, e := range emps {
		d, g := depts[str(e["department_id"])], desigs[str(e["designation_id"])]
		pl, ok := place[str(e["location_id"])]
		if d == nil || g == nil || !ok || pl.city == nil {
			continue
		}
		out[id] = &apprUIPerson{emp: e, department: d["name"], designation: g["name"], location: pl.location, city: pl.city}
	}
	return out, nil
}

// ---------- The inbox: requests plus improvement plans and tax proofs ----------

// apprUIQueue is the list the Approvals page shows for a tab: approval requests with the
// subject's details, plus improvement plans waiting for HR and employees' tax proofs waiting
// for Payroll (prototype payItems adds both). They are never "for information".
func (s *Server) apprUIQueue(ctx context.Context, c *Ctx, tab, wf string) ([]store.Row, error) {
	if tab == "" {
		tab = "mine"
	}
	rows, err := s.queue(ctx, c, tab, wf)
	if err != nil {
		return nil, err
	}
	if err := apprUIAddSubject(ctx, s.st.DB, rows); err != nil {
		return nil, err
	}
	wfs := splitWF(wf)
	has := func(k string) bool { return len(wfs) == 0 || slices.Contains(wfs, k) }
	if tab == "info" || (!has("p_pip") && !has("p_tax")) {
		return rows, nil
	}
	extra := []store.Row{}
	if has("p_pip") {
		l, err := s.apprUIPips(ctx, c, "")
		if err != nil {
			return nil, err
		}
		extra = append(extra, l...)
	}
	if has("p_tax") {
		l, err := s.apprUITax(ctx, c, "")
		if err != nil {
			return nil, err
		}
		extra = append(extra, l...)
	}
	added := false
	for _, x := range extra {
		pending := x["status"] == "Pending"
		switch tab {
		case "mine":
			if x["canAct"] != true {
				continue
			}
		case "open":
			if !pending {
				continue
			}
		case "done":
			if pending {
				continue
			}
		}
		rows = append(rows, x)
		added = true
	}
	if added {
		at := func(r store.Row) time.Time { t, _ := r["created_at"].(time.Time); return t }
		sort.SliceStable(rows, func(i, j int) bool { return at(rows[i]).After(at(rows[j])) })
	}
	return rows, nil
}

// apprUIPipStatus maps an improvement plan's state to the inbox status.
func apprUIPipStatus(st string) string {
	switch st {
	case "Pending approval":
		return "Pending"
	case "Rejected":
		return "Rejected"
	case "Withdrawn":
		return "Cancelled"
	}
	return "Approved" // Active, Completed
}

// apprUIPips lists improvement plans (consequence management, spec 11.6) as inbox items "PIP-<id>",
// for people who can see them: approvals or roster rights (not the Employee view), or the approving
// role, within scope.
// One plan when id is set.
func (s *Server) apprUIPips(ctx context.Context, c *Ctx, id string) ([]store.Row, error) {
	filter := bson.M{"kind": "PIP"}
	if id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != id {
			return []store.Row{}, nil // the plan's number as text is never this
		}
		filter["_id"] = n
	}
	plans, err := s.st.DB.C("consequence").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}))
	if err != nil {
		return nil, err
	}
	empIDs := []string{}
	for _, x := range plans {
		empIDs = append(empIDs, str(x["employee_id"]))
	}
	people, err := apprUIPeople(ctx, s.st.DB, empIDs)
	if err != nil {
		return nil, err
	}
	// The newest 300 plans of people in the caller's data scope, with the person's details.
	list := []store.Row{}
	for _, x := range plans {
		p := people[str(x["employee_id"])]
		if p == nil || !approvalsInScope(c.Scope, p.emp, str(p.department)) {
			continue
		}
		x["pip_status"], x["employee_name"], x["department"], x["city"], x["type"], x["designation"], x["location"] =
			x["status"], p.emp["name"], p.department, p.city, p.emp["type"], p.designation, p.location
		if list = append(list, x); len(list) == 300 {
			break
		}
	}
	// Every employee can read their own roster (ros.roster 1), so the Employee view never lists plans.
	rights := c.Role == "HR Admin" || c.Level("appr.queue") >= 1 || (c.Level("ros.roster") >= 1 && c.Role != "Employee")
	out := []store.Row{}
	for _, x := range list {
		role := str(x["approver_role"])
		if role == "" {
			role = "HRBP"
		}
		if !rights && c.Role != role {
			continue
		}
		emp := str(x["employee_id"])
		st := apprUIPipStatus(str(x["pip_status"]))
		days := fmt.Sprint(x["days"])
		at, _ := time.ParseInLocation("2006-01-02", str(x["issued_at"]), time.Local)
		step := 0
		if st == "Approved" {
			step = 1
		}
		row := store.Row{"id": fmt.Sprintf("PIP-%v", x["id"]), "consId": x["id"], "wf": "p_pip", "employee_id": emp, "employee_name": x["employee_name"],
			"department": x["department"], "city": x["city"], "type": x["type"], "designation": x["designation"], "location": x["location"],
			"raised_by": nil, "raised_by_name": x["issued_by"], "status": st, "step": step, "chain": []string{role},
			"notify": []any{}, "why": []string{"PIP needs HR approval"}, "facts": map[string]any{}, "created_at": at,
			"payload": map[string]any{"days": x["days"], "reason": x["reason"], "details": x["details"], "issuedBy": x["issued_by"], "issuedRole": x["issued_role"],
				"from": x["from_date"], "to": x["to_date"], "outcome": x["outcome"]},
			"sub": "PIP · " + days + " days"}
		row["canAct"] = st == "Pending" && emp != c.EmployeeID && (c.Role == "HR Admin" || c.Role == role)
		if st == "Pending" {
			row["waitingOn"] = role
		}
		out = append(out, row)
	}
	return out, nil
}

// apprUITax lists employees with tax proofs waiting for review in the open financial year as inbox
// items "TAX-<employee>" (prototype payItems p_tax). They are reviewed on the tax screen, so they
// stay pending until every proof is decided. One employee when emp is set.
func (s *Server) apprUITax(ctx context.Context, c *Ctx, emp string) ([]store.Row, error) {
	if s.canTax(c, 1) != nil {
		return nil, nil
	}
	fy := calc.FY(openYm(ctx, s.st.DB))
	filter := bson.M{"status": "Pending", "fy": fy}
	if emp != "" {
		filter["employee_id"] = emp
	}
	proofs, err := s.st.DB.C("tax_proof").Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	// One item per employee: how many proofs wait and since when.
	type waiting struct {
		n  int64
		at time.Time
	}
	by := map[string]*waiting{}
	ids := []string{}
	for _, p := range proofs {
		id := str(p["employee_id"])
		at, _ := p["at"].(time.Time)
		if w := by[id]; w != nil {
			w.n++
			if at.Before(w.at) {
				w.at = at
			}
			continue
		}
		by[id] = &waiting{1, at}
		ids = append(ids, id)
	}
	people, err := apprUIPeople(ctx, s.st.DB, ids)
	if err != nil {
		return nil, err
	}
	decls, err := s.st.DB.C("tax_declaration").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}, "fy": fy})
	if err != nil {
		return nil, err
	}
	regimeOf := map[string]any{}
	for _, d := range decls {
		regimeOf[str(d["employee_id"])] = d["regime"]
	}
	list := []store.Row{}
	for _, id := range ids {
		p := people[id]
		if p == nil || !approvalsInScope(c.Scope, p.emp, str(p.department)) {
			continue
		}
		rg := regimeOf[id]
		if rg == nil {
			rg = "New"
		}
		list = append(list, store.Row{"employee_id": id, "n": by[id].n, "at": by[id].at, "regime": rg, "employee_name": p.emp["name"], "department": p.department,
			"city": p.city, "type": p.emp["type"], "designation": p.designation, "location": p.location})
	}
	// ORDER BY min(p.at) DESC: the employee whose oldest waiting proof is newest comes first.
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i]["at"].(time.Time), list[j]["at"].(time.Time)
		if !a.Equal(b) {
			return a.After(b)
		}
		return str(list[i]["employee_id"]) < str(list[j]["employee_id"])
	})
	review := c.Level("pay.claims") >= 2 || c.Level("pay.run") >= 2
	out := []store.Row{}
	for _, x := range list {
		id := str(x["employee_id"])
		n := fmt.Sprint(x["n"])
		plural := map[bool]string{true: "", false: "s"}[n == "1"]
		regime := strings.ToLower(str(x["regime"])) + " regime"
		out = append(out, store.Row{"id": "TAX-" + id, "wf": "p_tax", "employee_id": id, "employee_name": x["employee_name"],
			"department": x["department"], "city": x["city"], "type": x["type"], "designation": x["designation"], "location": x["location"],
			"raised_by": nil, "raised_by_name": x["employee_name"], "status": "Pending", "step": 0, "chain": []string{"Payroll"}, "waitingOn": "Payroll",
			"notify": []any{}, "why": []string{}, "facts": map[string]any{}, "created_at": x["at"], "fy": fy,
			"payload": map[string]any{"proofs": x["n"], "regime": x["regime"]},
			"sub":     n + " proof" + plural + " to review", "title": n + " tax proof" + plural, "meta": regime,
			"canAct": review && id != c.EmployeeID})
	}
	return out, nil
}

// apprUISynthetic answers GET /api/approvals/{id} for an improvement plan or a tax proof item.
// ok is false when the id is an ordinary approval request.
func (s *Server) apprUISynthetic(ctx context.Context, c *Ctx, id string) (store.Row, bool, error) {
	var list []store.Row
	var err error
	switch {
	case strings.HasPrefix(id, "PIP-"):
		list, err = s.apprUIPips(ctx, c, strings.TrimPrefix(id, "PIP-"))
	case strings.HasPrefix(id, "TAX-"):
		list, err = s.apprUITax(ctx, c, strings.TrimPrefix(id, "TAX-"))
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if len(list) == 0 {
		return nil, true, notFound("Request not found")
	}
	row := list[0]
	stage := row["chain"].([]string)[0]
	subj, err := loadSubject(ctx, s.st.DB, str(row["employee_id"]))
	if err != nil {
		return nil, true, err
	}
	person := resolvePerson(ctx, s.st.DB, stage, subj, approval.Facts{})
	row["people"] = []map[string]string{{"stage": stage, "person": person.Name}}
	hist := []store.Row{}
	if row["wf"] == "p_pip" {
		var h []map[string]any
		if raw, ok := s.pipHistory(ctx, row["consId"]); ok {
			h = raw
		}
		for i, e := range h {
			what, note := str(e["what"]), str(e["note"])
			by := str(e["by"])
			hr := store.Row{"id": i + 1, "at": e["at"], "by_name": by, "note": note}
			switch {
			case what == "approve":
				hr["stage"], hr["action"] = stage, "Approved"
			case what == "reject":
				hr["stage"], hr["action"] = stage, "Rejected"
			case what == "withdraw":
				hr["action"], hr["note"] = "Withdrawn", strings.TrimSpace("Withdrawn by "+by+". "+note)
			default:
				if what != "" {
					what = strings.ToUpper(what[:1]) + what[1:]
				}
				hr["action"], hr["note"] = "Updated", strings.TrimSpace(what+" by "+by+". "+note)
			}
			hist = append(hist, hr)
		}
		if p, _ := row["payload"].(map[string]any); p != nil && row["status"] == "Approved" {
			hist = append(hist, store.Row{"id": len(hist) + 1, "action": "Applied", "by_name": "System", "note": "PIP started.", "at": p["from"]})
		}
		if row["status"] == "Pending" && row["employee_id"] == c.EmployeeID {
			row["whyNot"] = "This request is about you, so others decide it."
		}
	}
	row["history"] = hist
	return row, true, nil
}

// pipHistory reads an improvement plan's decision history.
func (s *Server) pipHistory(ctx context.Context, id any) ([]map[string]any, bool) {
	row, err := s.st.DB.C("consequence").FindOne(ctx, bson.M{"_id": id})
	if err != nil || row == nil || row["history"] == nil {
		return nil, false
	}
	var h []map[string]any
	if approvalsJSON(row["history"], &h) != nil {
		return nil, false
	}
	return h, true
}

// ---------- Request detail extras ----------

// apprUIDetail adds what the detail panel shows beyond the engine: the subject's details, the
// separation case of a resignation, the settlement lines of a full and final, the instalment
// schedule of a deduction, and team cover on the day before and after a leave.
func (s *Server) apprUIDetail(ctx context.Context, c *Ctx, req *Request, row store.Row) {
	q := s.st.DB
	_ = apprUIAddSubject(ctx, q, []store.Row{row})
	p := payloadOf(req)
	switch req.WF {
	case "resign":
		if sep, _ := q.C("separation").FindOne(ctx, bson.M{"request_id": req.ID},
			options.FindOne().SetSort(bson.D{{Key: "started_at", Value: -1}, {Key: "_id", Value: -1}})); sep != nil {
			row["caseId"] = sep["id"]
		}
	case "p_fnf":
		if f, _ := q.C("fnf").FindOne(ctx, bson.M{"_id": req.ID}); f != nil {
			if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": f["employee_id"]}); e != nil {
				var lines []fnfLine
				_ = approvalsJSON(f["lines"], &lines)
				out := []map[string]any{}
				for _, l := range lines {
					out = append(out, map[string]any{"key": l.Key, "kind": l.Kind, "label": l.Label, "amount": l.Amount, "note": l.Note})
				}
				row["fnf"] = map[string]any{"lwd": f["lwd"], "doj": e["doj"], "payYm": f["pay_ym"], "net": f["net"], "lines": out}
			}
		}
	case "p_ded":
		sched := []map[string]any{}
		if req.Status == "Approved" {
			l, _ := q.C("ledger").Find(ctx, bson.M{"source_id": req.ID, "kind": "ded", "void": false},
				options.Find().SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}))
			for _, x := range l {
				sched = append(sched, map[string]any{"ym": strings.TrimSpace(str(x["ym"])), "amount": x["amount"]})
			}
		} else {
			start := maxYm(p.StartYm, openYm(ctx, q))
			for i, amt := range calc.Instalments(p.Amount, max(1, p.Instalments)) {
				sched = append(sched, map[string]any{"ym": addYm(start, i), "amount": amt})
			}
		}
		row["schedule"] = sched
	case "leave":
		// Team cover over the leave and the day either side (prototype coverHtml).
		if isDate(p.From) && isDate(p.To) {
			if l, err := apprUITeamOff(ctx, q, req.EmployeeID, req.ID, p.From, p.To); err == nil {
				row["teamOff"] = l
			}
		}
	}
}

// apprUITeamOff is who else in the department at the same property has leave pending or approved
// from the day before to the day after the dates.
func apprUITeamOff(ctx context.Context, q store.Q, empID, exclude, from, to string) ([]store.Row, error) {
	l, err := apprUIColleagueLeaves(ctx, q, empID, exclude, addDay(from, -1), addDay(to, 1), true)
	for _, x := range l {
		delete(x, "ltype")
	}
	return l, err
}

// apprUIColleagueLeaves are the leave requests, pending or approved, of the employee's colleagues (same location
// and department; with workingOnly, not those who have left) that overlap from–to, other than the request exclude:
// rows id, name, ltype, from, to and status, by start date then name.
func apprUIColleagueLeaves(ctx context.Context, q store.Q, empID, exclude, from, to string, workingOnly bool) ([]store.Row, error) {
	out := []store.Row{}
	me, err := q.C("employee").FindOne(ctx, bson.M{"_id": empID})
	if err != nil || me == nil || me["location_id"] == nil || me["department_id"] == nil {
		return out, err
	}
	f := bson.M{"_id": bson.M{"$ne": me["id"]}, "location_id": me["location_id"], "department_id": me["department_id"]}
	if workingOnly {
		f["status"] = bson.M{"$ne": "Inactive"}
	}
	team, err := q.C("employee").Find(ctx, f, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	names := map[string]any{}
	ids := []string{}
	for _, x := range team {
		names[str(x["id"])] = x["name"]
		ids = append(ids, str(x["id"]))
	}
	if len(ids) == 0 {
		return out, nil
	}
	reqs, err := q.C("approval_request").Find(ctx, bson.M{"wf": "leave", "status": bson.M{"$in": []string{"Pending", "Approved"}}, "_id": bson.M{"$ne": exclude}, "payload.short": bson.M{"$ne": true},
		"employee_id": bson.M{"$in": ids}, "payload.from": bson.M{"$lte": to}, "payload.to": bson.M{"$gte": from}})
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		p, _ := r["payload"].(map[string]any)
		id := str(r["employee_id"])
		out = append(out, store.Row{"id": id, "name": names[id], "ltype": p["ltype"], "from": p["from"], "to": p["to"], "status": r["status"]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := str(out[i]["from"]), str(out[j]["from"]); a != b {
			return a < b
		}
		return str(out[i]["name"]) < str(out[j]["name"])
	})
	return out, nil
}

// apprUITeamSize counts the active people at the employee's location and department, the employee included.
func apprUITeamSize(ctx context.Context, q store.Q, empID string) int {
	me, _ := q.C("employee").FindOne(ctx, bson.M{"_id": empID})
	if me == nil || me["location_id"] == nil || me["department_id"] == nil {
		return 0
	}
	n, _ := q.C("employee").Count(ctx, bson.M{"status": "Active", "location_id": me["location_id"], "department_id": me["department_id"]})
	return int(n)
}

func addDay(d string, n int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// ---------- Route preview ----------

// apprUISim is the route preview with what the request dialog shows next to it.
type apprUISim struct {
	*Plan
	Days       *float64    `json:"days,omitempty"`  // leave: days counted under the type's rule
	Offs       *int        `json:"offs,omitempty"`  // leave: weekly offs and holidays inside the dates
	Range      *int        `json:"range,omitempty"` // leave: calendar days from and to
	Problems   []string    `json:"problems"`        // what would stop the request being raised
	TeamOff    []store.Row `json:"teamOff,omitempty"`
	Team       *int        `json:"team,omitempty"`
	Location   string      `json:"location,omitempty"`
	Department string      `json:"department,omitempty"`
	// leave: why every leave is refused this month (a low discipline score), and whether the caller may allow
	// this one request with a reason.
	ScoreBlock  string `json:"scoreBlock,omitempty"`
	CanOverride bool   `json:"canOverride,omitempty"`
}

// apprUISimulate adds the leave count, team cover and the raise-time problems to a route preview.
// Team cover is only for the caller's own request or an employee in their data scope.
func (s *Server) apprUISimulate(ctx context.Context, c *Ctx, b *raiseBody, f approval.Facts, plan *Plan) (*apprUISim, error) {
	q := s.st.DB
	out := &apprUISim{Plan: plan, Problems: []string{}}
	switch b.WF {
	case "leave":
		days := f.Days
		out.Days = &days
		offs, rng := apprUILeaveOffs(ctx, q, b.EmployeeID, b.Payload.From, b.Payload.To)
		out.Offs, out.Range = &offs, &rng
		out.Problems = append(out.Problems, leaveErrors(ctx, q, c, b.EmployeeID, &b.Payload, f.Days)...)
		if ts, err := loadTimeSet(ctx, q); err == nil {
			if e, err := loadEmp(ctx, q, b.EmployeeID); err == nil {
				pol, _ := ts.policyFor(e)
				if out.ScoreBlock = leaveScoreBlock(ctx, q, e, pol); out.ScoreBlock != "" {
					out.CanOverride = canScoreOverride(ctx, q, c, e.ID) == nil
				}
			}
		}
		mine := b.EmployeeID == c.EmployeeID
		if !mine {
			mine, _ = c.InScope(ctx, q, b.EmployeeID)
		}
		if mine {
			if l, err := apprUITeamOff(ctx, q, b.EmployeeID, "", b.Payload.From, b.Payload.To); err == nil {
				for _, x := range l {
					delete(x, "id")
				}
				out.TeamOff = l
			}
			team := apprUITeamSize(ctx, q, b.EmployeeID)
			out.Team = &team
			if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": b.EmployeeID}); e != nil {
				l, _ := q.C("location").FindOne(ctx, bson.M{"_id": e["location_id"]})
				d, _ := q.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]})
				if l != nil && d != nil {
					out.Location, out.Department = str(l["name"]), str(d["name"])
				}
			}
		}
	case "regularize":
		out.Problems = append(out.Problems, regErrors(ctx, q, b.EmployeeID, &b.Payload)...)
	case "p_adv":
		if b.EmployeeID == c.EmployeeID { // the employee's own advance (advances.go): the checks of sending it
			selfAdvanceMonths(ctx, q, &b.Payload)
			pr := selfAdvanceErrors(ctx, q, b.EmployeeID, &b.Payload)
			if len(pr) == 0 {
				pr = payItemErrors(ctx, q, c, b.WF, b.EmployeeID, &b.Payload)
			}
			out.Problems = append(out.Problems, pr...)
		}
	}
	return out, nil
}

// apprUILeaveOffs counts the weekly offs and holidays on the roster between two dates, and the
// calendar days in the range.
func apprUILeaveOffs(ctx context.Context, q store.Q, empID, from, to string) (offs, rng int) {
	if !isDate(from) || !isDate(to) || to < from {
		return 0, 0
	}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return 0, 0
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return 0, 0
	}
	pol, _ := ts.policyFor(e)
	m, err := loadMonth(ctx, q, ts, []string{e.ID}, from, to)
	if err != nil {
		return 0, 0
	}
	for _, d := range leave.Dates(from, to) {
		rng++
		if code := m.rosterCode(e, pol, d); code == "WO" || code == "PH" {
			offs++
		}
	}
	return offs, rng
}

// ---------- My requests ----------

// apprUIWaitingNames adds the name of the person a pending request waits for (waiting_name).
func apprUIWaitingNames(ctx context.Context, q store.Q, rows []store.Row) {
	subj := map[string]*subject{}
	for _, row := range rows {
		if row["status"] != "Pending" || str(row["waiting_on"]) == "" {
			continue
		}
		req, err := loadRequest(ctx, q, str(row["id"]), false)
		if err != nil || req.Step >= len(req.Chain) {
			continue
		}
		s := subj[req.EmployeeID]
		if s == nil {
			if s, err = loadSubject(ctx, q, req.EmployeeID); err != nil {
				continue
			}
			subj[req.EmployeeID] = s
		}
		row["waiting_name"] = resolvePerson(ctx, q, req.Chain[req.Step], s, req.Facts).Name
	}
}
