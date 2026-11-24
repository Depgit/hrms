package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// ---------- Inbox counts and settings ----------

func (s *Server) approvalCounts(r *http.Request, c *Ctx) (any, error) {
	out := map[string]int{}
	for _, tab := range []string{"mine", "open", "done", "info"} {
		rows, err := s.apprUIQueue(r.Context(), c, tab, r.URL.Query().Get("wf"))
		if err != nil {
			return nil, err
		}
		out[tab] = len(rows)
	}
	return out, nil
}

// approvalSettings is read by everyone signed in: the request dialogs show the turnaround time.
func (s *Server) approvalSettings(r *http.Request, c *Ctx) (any, error) {
	return map[string]int{"slaHours": slaHours(r.Context(), s.st.DB)}, nil
}

func (s *Server) approvalSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("appr.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		SLAHours int `json:"slaHours"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.SLAHours < 1 || b.SLAHours > 720 {
		return nil, bad("Hours must be between 1 and 720")
	}
	return nil, s.st.Tx(r.Context(), func(tx store.Q) error {
		if _, err := tx.C("approval_setting").Update(r.Context(), bson.M{"_id": 1}, bson.M{"$set": bson.M{"sla_hours": b.SLAHours}}); err != nil {
			return err
		}
		return audit(r.Context(), tx, c, fmt.Sprintf("Approval response time set to %d hours", b.SLAHours), "approval_setting", "1", nil, b)
	})
}

// approvalEscalateNow simulates the response time passing for one request (HR Admin only).
func (s *Server) approvalEscalateNow(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin can do this")
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		req, err := loadRequest(ctx, tx, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		if req.Status != "Pending" {
			return bad("Only a pending request can be escalated")
		}
		mode, err := noResponseMode(ctx, tx, req)
		if err != nil {
			return err
		}
		changed, err := s.noResponse(ctx, tx, req, mode, slaHours(ctx, tx))
		if err != nil {
			return err
		}
		msg := map[string]string{"escalate": "Escalated", "auto": "Auto-approved", "wait": "This workflow keeps waiting and reminds"}[mode]
		if !changed && mode != "wait" {
			msg = "This stage has no escalation role, so it keeps waiting"
		}
		out = map[string]any{"result": msg}
		return audit(ctx, tx, c, "Simulated no response", "approval_request", req.ID, nil, nil)
	})
	return out, err
}

// approvalColumns are the roles a flow can use as approver columns: every engine stage plus
// active roles that can hold approvals (not HR Admin, Employee, Finance or MIS Executive).
func approvalColumns(ctx context.Context, q store.Q) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, st := range approval.StageOrder {
		seen[st] = true
		out = append(out, st)
	}
	rows, err := q.C("role").Find(ctx, bson.M{"active": true, "name": bson.M{"$nin": []string{"HR Admin", "Employee", "Finance", "MIS Executive"}}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if n := str(row["name"]); !seen[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *Server) approvalColumnsGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("appr.queue", 1); err != nil {
		return nil, err
	}
	return approvalColumns(r.Context(), s.st.DB)
}

// ---------- Transfers ----------

// approvalsMoreDeptIDs are the ids of the departments with a name.
func approvalsMoreDeptIDs(ctx context.Context, q store.Q, name string) ([]any, error) {
	return q.C("department").Distinct(ctx, "_id", bson.M{"name": name})
}

// checkTransfer validates a transfer request before it is raised.
func checkTransfer(ctx context.Context, q store.Q, employeeID string, p *requestPayload) error {
	e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": employeeID})
	if e == nil {
		return notFound("Employee not found")
	}
	d, _ := q.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]})
	status, ok1 := e["status"].(string)
	locID, ok2 := e["location_id"].(string)
	if d == nil || !ok1 || !ok2 {
		return notFound("Employee not found")
	}
	dept := str(d["name"])
	if status != "Active" {
		return bad("Only active employees can be transferred")
	}
	if e["exit_status"] == "On notice" {
		return bad("An employee serving notice cannot be transferred")
	}
	if ok, _ := q.C("location").Exists(ctx, bson.M{"_id": p.ToLocation, "status": "Active"}); !ok {
		return bad("Pick an active location to move to")
	}
	if !isDate(p.Effective) || p.Effective <= today() {
		return bad("The effective date must be in the future")
	}
	if p.ToDepartment == dept {
		p.ToDepartment = ""
	}
	if p.ToDepartment != "" {
		if ok, _ := q.C("department").Exists(ctx, bson.M{"name": p.ToDepartment, "status": "Active"}); !ok {
			return bad("Department “%s” is not active", p.ToDepartment)
		}
		depts, err := approvalsMoreDeptIDs(ctx, q, p.ToDepartment)
		if err != nil {
			return err
		}
		if ok, _ := q.C("designation").Exists(ctx, bson.M{"department_id": bson.M{"$in": depts}, "name": p.ToDesignation, "status": "Active"}); !ok {
			return bad("Pick the new designation in %s", p.ToDepartment)
		}
		// The designation must be open to the employee's grade (a NULL grade matches no list).
		open := false
		if grade, ok := e["grade"].(string); ok {
			open, _ = q.C("designation").Exists(ctx, bson.M{"department_id": bson.M{"$in": depts}, "name": p.ToDesignation, "grades": grade})
		}
		if !open {
			return bad("%s is not open to the employee's grade", p.ToDesignation)
		}
	}
	if p.ToLocation == locID && p.ToDepartment == "" {
		return bad("That is the employee's current location and department")
	}
	if p.ToManager != "" {
		ok := false
		if p.ToManager != employeeID {
			ok, _ = q.C("employee").Exists(ctx, bson.M{"_id": p.ToManager, "status": "Active"})
		}
		if !ok {
			return bad("The receiving manager must be another active employee")
		}
	}
	if ok, _ := q.C("approval_request").Exists(ctx, bson.M{"wf": "transfer", "employee_id": employeeID, "status": "Pending"}); ok {
		return bad("A transfer is already pending for this employee")
	}
	return nil
}

// approvalsMoreShiftRuns: the shift applies to the department (id) at the location (its rows, shift_maps.go).
func approvalsMoreShiftRuns(ctx context.Context, q store.Q, sh store.Row, dept, loc string) bool {
	ok, _ := shiftRowFor(ctx, q, str(sh["id"]), dept, loc)
	return ok
}

// finalizeTransfer schedules (or applies) the move through the same path as any job change,
// so it lands in the change history and future-dated moves apply on their date.
func finalizeTransfer(ctx context.Context, tx store.Q, r *Request) (string, error) {
	var p requestPayload
	_ = json.Unmarshal(r.Payload, &p)
	ch := map[string]string{"location": p.ToLocation}
	if p.ToDepartment != "" {
		ch["department"], ch["designation"] = p.ToDepartment, p.ToDesignation
	}
	if p.ToManager != "" {
		ch["manager"] = p.ToManager
	}
	// A shift that does not apply to the new department and location moves to the first one that does (G first, then by
	// code).
	if e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": r.EmployeeID}); e != nil && e["shift_code"] != nil {
		dept := str(e["department_id"])
		if p.ToDepartment != "" {
			if d, _ := tx.C("department").FindOne(ctx, bson.M{"name": p.ToDepartment}); d != nil {
				dept = str(d["id"])
			}
		}
		if sh, _ := tx.C("shift").FindOne(ctx, bson.M{"code": e["shift_code"]}); sh != nil && !approvalsMoreShiftRuns(ctx, tx, sh, dept, p.ToLocation) {
			alts, err := tx.C("shift").Find(ctx, bson.M{"status": "Active"})
			if err == nil {
				codes := []string{}
				for _, a := range alts {
					if approvalsMoreShiftRuns(ctx, tx, a, dept, p.ToLocation) {
						codes = append(codes, str(a["code"]))
					}
				}
				sort.Slice(codes, func(i, j int) bool {
					if gi, gj := codes[i] == "G", codes[j] == "G"; gi != gj {
						return gi
					}
					return codes[i] < codes[j]
				})
				if len(codes) > 0 {
					ch["shift"] = codes[0]
				}
			}
		}
	}
	sys := &Ctx{Name: "Approval " + r.ID, Role: "System", EmployeeID: "system"}
	changes, err := ApplyJobChange(ctx, tx, sys, r.EmployeeID, JobChange{Changes: ch, Reason: store.SysName(ctx, tx, "reason.transfer"), EffectiveFrom: p.Effective}, "Transfer "+r.ID)
	if err != nil {
		return "", err
	}
	parts := []string{}
	for _, c := range changes {
		parts = append(parts, c.Field+" to "+c.To)
	}
	return fmt.Sprintf("Applies from %s: %s.", p.Effective, strings.Join(parts, ", ")), nil
}

// ---------- Reporting managers who have left ----------

type orphanGroup struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	LWD     *string  `json:"lwd"`
	Reports []Person `json:"reports"`
}

// approvalsMoreReports are the employees still working whose L1 or L2 is one of the ids, inside the caller's data
// scope (their department must exist, as the old JOIN required).
func approvalsMoreReports(ctx context.Context, q store.Q, c *Ctx, filter bson.M) ([]store.Row, error) {
	emps, err := q.C("employee").Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	deptIDs := make([]string, 0, len(emps))
	for _, e := range emps {
		deptIDs = append(deptIDs, str(e["department_id"]))
	}
	depts, err := approvalsByID(ctx, q, "department", deptIDs)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, e := range emps {
		if d := depts[str(e["department_id"])]; d != nil && approvalsInScope(c.Scope, e, str(d["name"])) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Server) managersLeft(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" && c.Level("emp.directory") < 2 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	gone, err := s.st.DB.C("employee").Find(ctx, bson.M{"status": "Inactive"}, options.Find().SetProjection(bson.M{"name": 1, "lwd": 1}))
	if err != nil {
		return nil, err
	}
	left := map[string]store.Row{}
	ids := []string{}
	for _, m := range gone {
		left[str(m["id"])] = m
		ids = append(ids, str(m["id"]))
	}
	out := []*orphanGroup{}
	if len(ids) == 0 {
		return out, nil
	}
	reports, err := approvalsMoreReports(ctx, s.st.DB, c, bson.M{"status": bson.M{"$ne": "Inactive"},
		"$or": bson.A{bson.M{"manager_id": bson.M{"$in": ids}}, bson.M{"functional_manager_id": bson.M{"$in": ids}}}})
	if err != nil {
		return nil, err
	}
	// One line per report and departed manager (a person who is both L1 and L2 counts once), by manager then report name.
	type line struct{ mid, mname, lwd, id, name string }
	lines := []line{}
	for _, e := range reports {
		seen := map[string]bool{}
		for _, col := range []string{"manager_id", "functional_manager_id"} {
			mid := str(e[col])
			m := left[mid]
			if m == nil || seen[mid] {
				continue
			}
			seen[mid] = true
			lines = append(lines, line{mid, str(m["name"]), str(m["lwd"]), str(e["id"]), str(e["name"])})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if a.mname != b.mname {
			return a.mname < b.mname
		}
		if a.name != b.name {
			return a.name < b.name
		}
		if a.mid != b.mid {
			return a.mid < b.mid
		}
		return a.id < b.id
	})
	idx := map[string]*orphanGroup{}
	for _, l := range lines {
		g := idx[l.mid]
		if g == nil {
			lwd := l.lwd // "" when the departed manager has no last working day
			g = &orphanGroup{ID: l.mid, Name: l.mname, LWD: &lwd}
			idx[l.mid] = g
			out = append(out, g)
		}
		g.Reports = append(g.Reports, Person{l.id, l.name})
	}
	return out, nil
}

// managersReassign moves everyone in scope who reports to a departed manager to a new one.
// Open requests follow automatically, because the Line Manager stage reads the current record.
func (s *Server) managersReassign(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" && c.Level("emp.directory") < 2 {
		return nil, forbidden("")
	}
	var b struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	// Only the reports of a manager who has left: an active manager's team changes through a job change with a reason
	// and an effective date (ESS-229, PPL-075).
	if ok, _ := s.st.DB.C("employee").Exists(ctx, bson.M{"_id": b.From, "status": "Inactive"}); !ok {
		return nil, bad("%s has not left. Change an active manager's team through a job change, with a reason and date", or(b.From, "The manager"))
	}
	if ok, _ := s.st.DB.C("employee").Exists(ctx, bson.M{"_id": b.To, "status": "Active"}); !ok {
		return nil, bad("Choose an active employee as the new manager")
	}
	n := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n = 0 // the transaction may run more than once
		rows, err := approvalsMoreReports(ctx, tx, c, bson.M{"status": bson.M{"$ne": "Inactive"}, "_id": bson.M{"$ne": b.To},
			"$or": bson.A{bson.M{"manager_id": b.From}, bson.M{"functional_manager_id": b.From}}})
		if err != nil {
			return err
		}
		var oldName, newName string
		if from, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": b.From}); from != nil {
			oldName = str(from["name"])
			if to, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": b.To}); to != nil {
				newName = str(to["name"])
			}
		}
		today := store.DateOf(tx.Now())
		for _, row := range rows {
			for _, f := range []struct{ col, label string }{{"manager_id", "Reporting manager (L1)"}, {"functional_manager_id", "Functional manager (L2)"}} {
				if row[f.col] != b.From {
					continue
				}
				if _, err := tx.C("employee").Update(ctx, bson.M{"_id": row["id"]}, bson.M{"$set": bson.M{f.col: b.To}}); err != nil {
					return err
				}
				if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": row["id"], "field": f.label, "old_value": oldName, "new_value": newName,
					"reason": "Reporting line change", "effective_from": today, "source": "Manager left", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
					return err
				}
			}
			n++
		}
		return audit(ctx, tx, c, fmt.Sprintf("Reassigned %d reports of %s to %s", n, oldName, newName), "employee", b.From, nil, b)
	})
	return map[string]int{"moved": n}, err
}
