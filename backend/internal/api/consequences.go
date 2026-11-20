package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Consequence management (spec 11.6): warning letters issued by the line manager, and
// improvement plans of 7, 15 or 30 days approved by HRBP (operations) or HRBP Corporate.

var consReasons = []string{"Roster discipline (Not OK deviations)", "Attendance and punctuality", "Conduct", "Performance", "Other"}

func consApprover(corporate bool) string {
	if corporate {
		return "HRBP Corporate"
	}
	return "HRBP"
}

// consState adds "Review due" for an active plan whose end date has passed.
func consState(row store.Row) {
	if row["status"] == "Active" {
		if to, _ := row["to_date"].(string); to != "" && to < today() {
			row["state"] = "Review due"
			return
		}
	}
	row["state"] = row["status"]
}

func (s *Server) consList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	// The latest 500 letters and plans of people in scope.
	filter := bson.M{}
	if len(scope) > 0 {
		ids, err := s.st.DB.C("employee").Distinct(ctx, "_id", scope)
		if err != nil {
			return nil, err
		}
		filter["employee_id"] = bson.M{"$in": ids}
	}
	list, err := s.st.DB.C("consequence").Find(ctx, filter, options.Find().SetSort(compOrder("-_id")).SetLimit(500))
	if err != nil {
		return nil, err
	}
	rows, err := consWithEmployee(ctx, s.st.DB, list)
	if err != nil {
		return nil, err
	}
	subs := map[string]*subject{}
	for _, x := range rows {
		consState(x)
		x["canDecide"] = x["status"] == "Pending approval" && x["employee_id"] != c.EmployeeID && (c.Role == "HR Admin" || c.Role == x["approver_role"])
		// The person who holds the approving role for this employee, from the role mapping, for every
		// plan (prototype consAppName under the role).
		if role, _ := x["approver_role"].(string); role != "" {
			emp := x["employee_id"].(string)
			if subs[emp] == nil {
				subs[emp], _ = loadSubject(ctx, s.st.DB, emp)
			}
			if sub := subs[emp]; sub != nil {
				x["approver_name"] = resolvePerson(ctx, s.st.DB, role, sub, approval.Facts{}).Name
			}
		}
	}
	// Not OK deviations this month with the discipline score (the month's start, 100 or pro-rata, less Not OK ×
	// the policy's points).
	// Suggestions are the people among them with no open plan.
	notOK, _ := consNotOK(ctx, s.st.DB, scope, today()[:7])
	sug, scores := []store.Row{}, map[string]any{}
	if len(notOK) > 0 {
		ts, err := loadTimeSet(ctx, s.st.DB)
		if err != nil {
			return nil, err
		}
		for _, x := range notOK {
			id, no := x["id"].(string), int(x["not_ok"].(int64))
			sc := discScore{Start: 100, Score: attend.Score(100, no, policy.Defaults().RS.Pts)}
			if e, err := loadEmp(ctx, s.st.DB, id); err == nil {
				p, _ := ts.policyFor(e)
				sc = scoreFor(e, p, today()[:7], no)
			}
			score := sc.Score
			scores[id] = map[string]any{"no": no, "score": score, "start": sc.Start, "startNote": sc.Note}
			if open, _ := x["open_pip"].(bool); !open && len(sug) < 20 {
				sug = append(sug, store.Row{"id": id, "name": x["name"], "not_ok": no, "score": score})
			}
		}
	}
	return map[string]any{"rows": rows, "suggest": sug, "scores": scores, "reasons": consReasons}, nil
}

// consWithEmployee adds to each letter (all its columns) the employee's name, location, department (and whether
// it is corporate) and designation; letters whose employee or masters are missing are dropped, as the joins did.
func consWithEmployee(ctx context.Context, q store.Q, list []store.Row) ([]store.Row, error) {
	ids := []any{}
	for _, x := range list {
		ids = append(ids, x["employee_id"])
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(compFields("id", "name", "department_id", "designation_id", "location_id")))
	if err != nil {
		return nil, err
	}
	byID := compRowsByID(emps)
	master := func(coll string, field string) (map[string]store.Row, error) {
		want := []any{}
		for _, e := range emps {
			want = append(want, e[field])
		}
		rows, err := q.C(coll).Find(ctx, bson.M{"_id": bson.M{"$in": want}})
		return compRowsByID(rows), err
	}
	depts, err := master("department", "department_id")
	if err != nil {
		return nil, err
	}
	desigs, err := master("designation", "designation_id")
	if err != nil {
		return nil, err
	}
	locs, err := master("location", "location_id")
	if err != nil {
		return nil, err
	}
	out := make([]store.Row, 0, len(list))
	for _, x := range list {
		e := byID[str(x["employee_id"])]
		if e == nil {
			continue
		}
		d, g, l := depts[str(e["department_id"])], desigs[str(e["designation_id"])], locs[str(e["location_id"])]
		if d == nil || g == nil || l == nil {
			continue
		}
		x["name"], x["location"], x["corporate"], x["department"], x["designation"] = e["name"], l["name"], d["corporate"], d["name"], g["name"]
		out = append(out, x)
	}
	return out, nil
}

// consNotOK is, for people in scope with Not OK deviations in month ym, the count and whether they have an open
// improvement plan; most deviations first, then by employee code.
func consNotOK(ctx context.Context, q store.Q, scope bson.D, ym string) ([]store.Row, error) {
	counts, err := q.C("deviation_decision").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"status": "Not OK", "date": bson.M{"$gte": ym + "-01", "$lt": nextYm(ym) + "-01"}}},
		bson.M{"$group": bson.M{"_id": "$employee_id", "n": bson.M{"$sum": int64(1)}}},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(counts))
	for i, x := range counts {
		ids[i] = str(x["id"])
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"$and": bson.A{scope, bson.M{"_id": bson.M{"$in": ids}}}}, options.Find().SetProjection(compFields("id", "name", "department_id")))
	if err != nil {
		return nil, err
	}
	depts, err := q.C("department").Distinct(ctx, "_id", bson.M{})
	if err != nil {
		return nil, err
	}
	hasDept := map[string]bool{}
	for _, d := range depts {
		hasDept[str(d)] = true
	}
	byID := map[string]store.Row{} // the people in scope (with a department)
	for _, e := range emps {
		if hasDept[str(e["department_id"])] {
			byID[str(e["id"])] = e
		}
	}
	open, err := q.C("consequence").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}, "kind": "PIP", "status": bson.M{"$in": bson.A{"Pending approval", "Active"}}})
	if err != nil {
		return nil, err
	}
	hasOpen := map[string]bool{}
	for _, id := range open {
		hasOpen[str(id)] = true
	}
	out := []store.Row{}
	for _, x := range counts {
		id := str(x["id"])
		if e := byID[id]; e != nil {
			out = append(out, store.Row{"id": id, "name": e["name"], "not_ok": x["n"], "open_pip": hasOpen[id]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i]["not_ok"].(int64), out[j]["not_ok"].(int64)
		if a != b {
			return a > b
		}
		return str(out[i]["id"]) < str(out[j]["id"])
	})
	return out, nil
}

func (s *Server) consIssue(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeID string `json:"employeeId"`
		Kind       string `json:"kind"`
		Days       int    `json:"days"`
		Reason     string `json:"reason"`
		Details    string `json:"details"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	switch {
	case b.Kind != "Warning" && b.Kind != "PIP":
		return nil, bad("Choose a warning letter or an improvement plan")
	case b.Kind == "PIP" && !containsInt([]int{7, 15, 30}, b.Days):
		return nil, bad("An improvement plan is 7, 15 or 30 days")
	case !containsStr(consReasons, b.Reason):
		return nil, bad("Choose a reason")
	case len(strings.TrimSpace(b.Details)) < 10:
		return nil, bad("Describe what happened in a sentence or two")
	case b.EmployeeID == c.EmployeeID:
		return nil, forbidden("Nobody issues a letter to themselves")
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	var id int64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		e, err := tx.C("employee").FindOne(ctx, bson.M{"_id": b.EmployeeID}, options.FindOne().SetProjection(compFields("status", "department_id")))
		if err != nil {
			return err
		}
		var d store.Row
		if e != nil {
			if d, err = tx.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]}, options.FindOne().SetProjection(compFields("corporate"))); err != nil {
				return err
			}
		}
		if d == nil {
			return notFound("Employee not found")
		}
		corp, _ := d["corporate"].(bool)
		status := str(e["status"])
		if status != "Active" {
			return bad("Letters are issued to active employees")
		}
		if b.Kind == "PIP" {
			if open, _ := tx.C("consequence").Exists(ctx, bson.M{"employee_id": b.EmployeeID, "kind": "PIP", "status": bson.M{"$in": bson.A{"Pending approval", "Active"}}}); open {
				return bad("This employee already has an open improvement plan")
			}
		}
		// The letter's number: one more than the employee's letters of this kind that stand.
		n, err := tx.C("consequence").Count(ctx, bson.M{"employee_id": b.EmployeeID, "kind": b.Kind, "status": bson.M{"$nin": bson.A{"Rejected", "Withdrawn"}}})
		if err != nil {
			return err
		}
		seq := int(n) + 1
		st, appr := "Issued", ""
		if b.Kind == "PIP" {
			st, appr = "Pending approval", consApprover(corp)
			if role, _, err := pipApprover(ctx, tx, b.EmployeeID); err == nil {
				appr = role // the improvement plan's approval flow
			}
		} else {
			b.Days = 0
		}
		var approver any // NULL for a warning letter
		if appr != "" {
			approver = appr
		}
		newID, err := tx.C("consequence").Insert(ctx, bson.M{"employee_id": b.EmployeeID, "kind": b.Kind, "seq": seq, "days": b.Days, "reason": b.Reason,
			"details": strings.TrimSpace(b.Details), "status": st, "approver_role": approver, "issued_by": c.Name, "issued_role": c.Role})
		if err != nil {
			return err
		}
		id, _ = newID.(int64)
		label := fmt.Sprintf("Warning letter %d", seq)
		if b.Kind == "PIP" {
			label = fmt.Sprintf("Improvement plan %d (%d days)", seq, b.Days)
		}
		return audit(ctx, tx, c, label+" issued to "+b.EmployeeID, "consequence", strconv.FormatInt(id, 10), nil, b)
	})
	return map[string]int64{"id": id}, err
}

func (s *Server) consAction(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Note    string `json:"note"`
		Outcome string `json:"outcome"`
	}
	_ = decode(r, &b)
	ctx, action := r.Context(), r.PathValue("action")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		var x store.Row
		var err error
		if cid, ok := compIntID(r.PathValue("id")); ok {
			if x, err = tx.C("consequence").Lock(ctx, bson.M{"_id": cid}); err != nil {
				return err
			}
		}
		if x == nil {
			return notFound("Not found")
		}
		emp := x["employee_id"].(string)
		if emp == c.EmployeeID {
			return forbidden("Nobody decides their own letter")
		}
		if err := c.MustInScope(ctx, tx, emp); err != nil {
			return err
		}
		hist := map[string]any{"at": today(), "by": c.Name, "role": c.Role, "what": action, "note": b.Note}
		switch action {
		case "approve", "reject":
			if x["status"] != "Pending approval" || (c.Role != "HR Admin" && c.Role != x["approver_role"]) {
				return forbidden("This plan is waiting for " + fmt.Sprint(x["approver_role"]))
			}
			if action == "reject" && strings.TrimSpace(b.Note) == "" {
				return bad("Add a comment to reject")
			}
			if action == "approve" {
				days := int(x["days"].(int32))
				var note any // an empty note is stored as null
				if b.Note != "" {
					note = b.Note
				}
				_, err = tx.C("consequence").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"status": "Active", "from_date": store.DateOf(tx.Now()),
					"to_date": policy.AddDays(today(), days-1), "decided_by": c.Name, "decided_at": tx.Now(), "note": note}, "$push": bson.M{"history": hist}})
			} else {
				_, err = tx.C("consequence").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"status": "Rejected", "decided_by": c.Name, "decided_at": tx.Now(), "note": b.Note},
					"$push": bson.M{"history": hist}})
			}
		case "withdraw":
			if err := c.Need("ros.roster", 2); err != nil {
				return err
			}
			if x["status"] != "Issued" && x["status"] != "Pending approval" && x["status"] != "Active" {
				return bad("Only an open letter or plan can be withdrawn")
			}
			if strings.TrimSpace(b.Note) == "" {
				return bad("Say why it is withdrawn")
			}
			_, err = tx.C("consequence").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"status": "Withdrawn"}, "$push": bson.M{"history": hist}})
		case "complete":
			if err := c.Need("ros.roster", 2); err != nil {
				return err
			}
			if x["status"] != "Active" || x["kind"] != "PIP" {
				return bad("Only an active improvement plan can be completed")
			}
			if b.Outcome != "Improved" && b.Outcome != "Not improved" {
				return bad("Choose Improved or Not improved")
			}
			hist["what"] = "completed: " + b.Outcome
			_, err = tx.C("consequence").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"status": "Completed", "outcome": b.Outcome}, "$push": bson.M{"history": hist}})
		default:
			return notFound("")
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("%s %s %d for %s", map[string]string{"approve": "Approved", "reject": "Rejected", "withdraw": "Withdrew", "complete": "Completed"}[action], map[string]string{"PIP": "improvement plan", "Warning": "warning letter"}[x["kind"].(string)], x["seq"], emp), "consequence", fmt.Sprint(x["id"]), x, b)
	})
}

// consLetter is the letter text (warning letter or improvement plan).
func (s *Server) consLetter(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	var x store.Row
	if cid, ok := compIntID(r.PathValue("id")); ok {
		one, err := s.st.DB.C("consequence").Find(ctx, bson.M{"_id": cid})
		if err != nil {
			return nil, err
		}
		rows, err := consWithEmployee(ctx, s.st.DB, one)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			x = rows[0]
		}
	}
	if x == nil {
		return nil, notFound("")
	}
	if x["employee_id"] != c.EmployeeID {
		if err := c.Need("ros.roster", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, x["employee_id"].(string)); err != nil {
			return nil, err
		}
	}
	co, _ := s.st.DB.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(compFields("legal_name")))
	company := str(co["legal_name"])
	head := fmt.Sprintf("%s\n\nDate: %s\n\nEmployee: %s (%s)\nDesignation: %s, %s\nLocation: %s\n\n", company, longDate(fmt.Sprint(x["issued_at"])), x["name"], x["employee_id"], x["designation"], x["department"], x["location"])
	var title, body string
	if x["kind"] == "PIP" {
		title = fmt.Sprintf("Performance improvement plan %d", x["seq"])
		period := ""
		if f, ok := x["from_date"].(string); ok {
			period = fmt.Sprintf(", from %s to %s", longDate(f), longDate(x["to_date"].(string)))
		}
		approvedBy := "—"
		if v, ok := x["decided_by"].(string); ok && x["status"] != "Rejected" {
			approvedBy = v
		}
		body = fmt.Sprintf("You are placed on a performance improvement plan for %d days%s.\n\nReason: %s\n\n%s\n\nYour manager will review your progress at the end of the plan. If the expected improvement is not seen, further action under company policy may follow.\n\nIssued by: %s\nApproved by: %s",
			x["days"], period, x["reason"], x["details"], x["issued_by"], approvedBy)
	} else {
		title = fmt.Sprintf("Warning letter %d", x["seq"])
		body = fmt.Sprintf("Reason: %s\n\n%s\n\nThis is warning number %d on record. Please make sure this does not happen again. Repeated issues may lead to a performance improvement plan or further action under company policy.\n\nIssued by: %s",
			x["reason"], x["details"], x["seq"], x["issued_by"])
	}
	return map[string]string{"title": title, "text": head + title + "\n\n" + body}, nil
}
