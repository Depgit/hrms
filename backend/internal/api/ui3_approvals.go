package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Round 3 of the Approvals, Shift and roster and Bulk update pages (prototype nrForm for resignations,
// consAppRole/consAppName, MX.p_pip, makeWfReq "Roster changed", TPL.upd): who approves an
// improvement plan, a change or withdrawal of an accepted resignation, the roster change behind a
// regularization, the reasons HR may record, and the Work Policy column of the employee master update.

// ---------- Reasons for leaving ----------

// hrExitReasons is the prototype's EXITR. The Employee view does not offer "Death or permanent
// disability" (prototype nrForm filters it out); HR may record it.
var hrExitReasons = []string{"Better opportunity", "Higher studies", "Relocation", "Personal or health", "Compensation", "Death or permanent disability", "Other"}

// exitReasonsFor is the reason list a role view sees and may send.
func exitReasonsFor(role string) []string {
	if role == "Employee" {
		return exitReasons
	}
	return hrExitReasons
}

// ---------- Improvement plans: who approves ----------

// pipStageOK says whether a matrix stage can hold an improvement plan: a role a person holds, not the
// reporting line or a receiving side.
func pipStageOK(stage string) bool {
	return approval.StageRole(stage) != "Line Manager" && !strings.HasPrefix(stage, "Receiving")
}

// pipApprover is the role that approves an improvement plan for an employee, and who holds it. The
// role is the first approver in the plan's approval flow (p_pip, seeded as the prototype's note says:
// HRBP, or HRBP Corporate for corporate staff); the default applies when the flow names no role.
func pipApprover(ctx context.Context, q store.Q, empID string) (string, Person, error) {
	s, err := loadSubject(ctx, q, empID)
	if err != nil {
		return "", unassigned, err
	}
	role := consApprover(s.Corporate)
	if p, err := planFor(ctx, q, "p_pip", s, approval.Facts{}); err == nil {
		for _, st := range p.Chain {
			if pipStageOK(st) {
				role = st
				break
			}
		}
	}
	return role, resolvePerson(ctx, q, role, s, approval.Facts{}), nil
}

// consApproverGet names who would approve an improvement plan for an employee, before it is raised
// (prototype: "A PIP goes to HRBP (name) for approval").
func (s *Server) consApproverGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.URL.Query().Get("employeeId")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	role, p, err := pipApprover(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"role": role, "name": p.Name, "named": p.ID != ""}, nil
}

// ---------- Change or withdraw an accepted resignation (prototype nrForm rkind) ----------

func resignKind(p *requestPayload) bool { return p.RKind == "change" || p.RKind == "withdraw" }

// withdrawDays is how many days after it was given a resignation can be withdrawn: the work
// policy's setting, 7 when it has none (as the separation case shows it).
func withdrawDays(ctx context.Context, q store.Q, empID string) int {
	days := 7
	if ts, err := loadTimeSet(ctx, q); err == nil {
		if e, err := loadEmp(ctx, q, empID); err == nil {
			if p, _ := ts.policyFor(e); p.S.Ex.Withdraw > 0 {
				days = p.S.Ex.Withdraw
			}
		}
	}
	return days
}

// resignKindAllowed: the employee may withdraw their own resignation; changing the last working day,
// and raising either for someone else, is for HR (prototype nrForm: the Employee view only withdraws).
func (s *Server) resignKindAllowed(ctx context.Context, c *Ctx, empID string, p *requestPayload) error {
	if p.RKind == "change" && c.Role == "Employee" {
		return forbidden("You can withdraw your resignation. HR changes the last working day")
	}
	if empID == c.EmployeeID {
		return nil
	}
	if c.Role != "HR Admin" && c.Role != "HRBP" && c.Role != "HRBP Corporate" {
		return forbidden("A resignation is raised by the employee or by HR")
	}
	return c.MustInScope(ctx, s.st.DB, empID)
}

// resignKindFacts checks a change or withdrawal (prototype nrErrors) and fills the payload from the
// records: the open case, its type, the current and new last working day, the notice and the reason.
// It returns the facts for the approval flow and the problems that stop it being raised.
func (s *Server) resignKindFacts(ctx context.Context, empID string, p *requestPayload) (approval.Facts, []string, error) {
	q := s.st.DB
	f := approval.Facts{WF: "resign", SKind: "Resignation"}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return f, nil, err
	}
	if e.Status != "Active" {
		return f, []string{fmt.Sprintf("%s is %s. Requests can only be raised for active employees", e.Name, strings.ToLower(e.Status))}, nil
	}
	onNotice := e.Exit != nil && *e.Exit == "On notice"
	cur := ""
	if e.LWD != nil {
		cur = *e.LWD
	}
	var sc *sepCase
	if x, err := sepFindOne(ctx, q, bson.M{"employee_id": empID, "status": "Open"}, sepNewestFirst); err == nil {
		sc = x
	}
	p.ExitType, p.FromLWD, p.CaseID = "Resignation", cur, ""
	given := today()
	if sc != nil {
		p.CaseID, p.ExitType, f.SKind, given = sc.ID, sc.Type, sc.Type, sc.GivenOn
		p.NoticeDays = sc.NoticeDays
		if p.Reason == "" {
			p.Reason = sc.Reason
		}
	} else {
		p.NoticeDays, _, _ = noticeDaysFor(ctx, q, empID)
		if x, _ := q.C("employee").FindOne(ctx, bson.M{"_id": empID}); x != nil && x["resign_at"] != nil {
			given = str(x["resign_at"])
		}
	}
	noticeLwd := policy.AddDays(given, p.NoticeDays)
	probs := []string{}
	if pending, _ := q.C("approval_request").Exists(ctx, bson.M{"wf": "resign", "employee_id": empID, "status": "Pending"}); pending {
		probs = append(probs, "A resignation request is already pending for this employee")
	}
	stage := ""
	if sc != nil {
		stage, _ = s.stage(ctx, q, sc)
	}
	switch p.RKind {
	case "withdraw":
		p.LWD, p.Waive = cur, false
		switch {
		case !onNotice:
			probs = append(probs, "There is no accepted resignation to withdraw")
		case sc == nil || sc.Type != "Resignation":
			probs = append(probs, "Only a resignation can be withdrawn")
		case stage != "Notice":
			probs = append(probs, "This resignation can no longer be withdrawn")
		default:
			if lim, since := withdrawDays(ctx, q, empID), policy.DayDiff(sc.GivenOn, today()); since > lim {
				probs = append(probs, fmt.Sprintf("Resignation can be withdrawn within %d days of the day it was given. The resignation was given %d days ago", lim, since))
			}
		}
	case "change":
		f.Waive = p.Waive
		switch {
		case !onNotice:
			probs = append(probs, "Only an employee serving notice can have the last working day changed")
		case sc != nil && stage != "Notice":
			probs = append(probs, "The last working day can be changed during the notice period")
		case !isDate(p.LWD):
			probs = append(probs, "Give the new last working day")
		case p.LWD < today():
			probs = append(probs, "The last working day cannot be in the past")
		case p.LWD > policy.AddDays(today(), 365):
			probs = append(probs, "The last working day can be at most a year away")
		case p.LWD == cur:
			probs = append(probs, "That is already the last working day")
		case !p.Waive && p.LWD < noticeLwd:
			probs = append(probs, fmt.Sprintf("Policy notice is %d days. Choose %s or later, or request a waiver", p.NoticeDays, longDate(noticeLwd)))
		}
	}
	return f, probs, nil
}

// resignKindRaise raises a change or withdrawal of an accepted resignation for approval. It goes
// through the employee's separation flow; approving it moves or withdraws the case (finalizeResign).
func (s *Server) resignKindRaise(ctx context.Context, c *Ctx, b *raiseBody) (any, error) {
	if err := s.resignKindAllowed(ctx, c, b.EmployeeID, &b.Payload); err != nil {
		return nil, err
	}
	if b.Payload.Reason != "" && !containsStr(exitReasonsFor(c.Role), b.Payload.Reason) {
		return nil, bad("Choose a reason for leaving")
	}
	f, probs, err := s.resignKindFacts(ctx, b.EmployeeID, &b.Payload)
	if err != nil {
		return nil, err
	}
	if len(probs) > 0 {
		return nil, badD(probs[0], probs)
	}
	return s.CreateRequest(ctx, c, "resign", b.EmployeeID, f, b.Payload)
}

// finalizeResign runs when a resignation request is fully approved: a withdrawal ends the case, a
// change moves the last working day, anything else is the separation approval itself.
func finalizeResign(ctx context.Context, tx store.Q, r *Request) (string, error) {
	p := payloadOf(r)
	switch p.RKind {
	case "withdraw":
		return finalizeResignWithdraw(ctx, tx, r, p)
	case "change":
		return finalizeResignChange(ctx, tx, r, p)
	}
	return finalizeSeparation(ctx, tx, r)
}

func finalizeResignWithdraw(ctx context.Context, tx store.Q, r *Request, p requestPayload) (string, error) {
	sc, err := sepLock(ctx, tx, p.CaseID)
	if err != nil || sc.Status != "Open" {
		return "", bad("The resignation case is no longer open, so there is nothing to withdraw. Reject this request")
	}
	// Whose resignation, and who withdraws it: the raiser's name, or their id when there is no such employee.
	var name, by string
	if e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": r.EmployeeID}); e != nil {
		name, by = str(e["name"]), r.RaisedBy
		if rb, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": r.RaisedBy}); rb != nil && rb["name"] != nil {
			by = str(rb["name"])
		}
	}
	if err := withdrawSepCase(ctx, tx, sc, by, r.ID); err != nil {
		return "", err
	}
	if err := voidNoticeRecovery(ctx, tx, r.EmployeeID, "Resignation withdrawn ("+r.ID+")"); err != nil {
		return "", err
	}
	return name + "'s resignation is withdrawn. Employment continues.", nil
}

func finalizeResignChange(ctx context.Context, tx store.Q, r *Request, p requestPayload) (string, error) {
	e, err := tx.C("employee").Lock(ctx, bson.M{"_id": r.EmployeeID, "status": "Active", "exit_status": "On notice"})
	if err != nil || e == nil {
		return "", bad("The employee is no longer serving notice. Reject this request")
	}
	name := str(e["name"])
	from := p.FromLWD
	if cur, ok := e["lwd"].(string); ok {
		from = cur
	}
	if _, err := tx.C("employee").Update(ctx, bson.M{"_id": r.EmployeeID}, bson.M{"$set": bson.M{"lwd": p.LWD}}); err != nil {
		return "", err
	}
	if p.CaseID != "" {
		if _, err := tx.C("separation").Update(ctx, bson.M{"_id": p.CaseID, "status": "Open"}, bson.M{"$set": bson.M{"lwd": p.LWD}}); err != nil {
			return "", err
		}
		sys := &Ctx{Name: "Approval " + r.ID, Role: "System", EmployeeID: "system"}
		if err := sepEvent(ctx, tx, sys, p.CaseID, "lwd", fmt.Sprintf("Last working day changed from %s to %s, approved as %s", longDate(from), longDate(p.LWD), r.ID)); err != nil {
			return "", err
		}
	}
	// The notice is served in full or waived, so a shortfall recovered for an earlier date goes.
	if err := voidNoticeRecovery(ctx, tx, r.EmployeeID, "Last working day changed ("+r.ID+")"); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s's last working day changed from %s to %s.", name, longDate(from), longDate(p.LWD)), nil
}

// withdrawSepCase ends a resignation case as withdrawn (POST /api/separations/{id}/withdraw, and an
// approved withdrawal request): a resignation still waiting for approval is cancelled, an accepted one
// takes the employee off notice. Any other change or withdrawal request still waiting about the case is cancelled
// with it (HIRE-445); except is the request being approved, if any.
func withdrawSepCase(ctx context.Context, tx store.Q, sc *sepCase, by, except string) error {
	open, err := tx.C("approval_request").Find(ctx, bson.M{"wf": "resign", "status": "Pending", "payload.caseId": sc.ID, "_id": bson.M{"$ne": except}})
	if err != nil {
		return err
	}
	for _, o := range open {
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": o["id"]}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
			return err
		}
		if err := history(ctx, tx, str(o["id"]), "", "Cancelled", by, "Resignation withdrawn"); err != nil {
			return err
		}
	}
	if sc.RequestID != nil && sc.ReqStatus != nil && *sc.ReqStatus == "Pending" {
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": *sc.RequestID}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
			return err
		}
		if err := history(ctx, tx, *sc.RequestID, "", "Cancelled", by, "Resignation withdrawn"); err != nil {
			return err
		}
	} else if _, err := tx.C("employee").Update(ctx, bson.M{"_id": sc.EmployeeID, "status": "Active"},
		bson.M{"$set": bson.M{"exit_status": nil, "lwd": nil, "exit_reason": nil, "resign_at": nil}}); err != nil {
		return err
	} else if err := uniformRefundRestore(ctx, tx, sc.EmployeeID); err != nil { // an opening uniform's refund cancelled for the notice is due again
		return err
	}
	return sepClose(ctx, tx, sc.ID, "Cancelled", by, "Withdrawn by "+by)
}

// voidNoticeRecovery cancels a notice shortfall recovery not yet paid (prototype noticeRecovery).
func voidNoticeRecovery(ctx context.Context, tx store.Q, empID, why string) error {
	_, err := tx.C("ledger").UpdateMany(ctx, bson.M{"source_id": "NTC-" + empID, "void": false, "ym": bson.M{"$gte": openYm(ctx, tx)}},
		bson.M{"$set": bson.M{"void": true, "void_reason": why, "voided_at": tx.Now()}})
	return err
}

// ---------- Request detail ----------

// apprUI3Detail adds the roster change behind a "Roster changed" regularization (prototype: "Roster
// for this day was changed late by Property Manager: Staff absent or sick"), and the case a change or
// withdrawal of a resignation is about.
func (s *Server) apprUI3Detail(ctx context.Context, req *Request, row store.Row) {
	p := payloadOf(req)
	switch req.WF {
	case "regularize":
		if p.Reason != "Roster changed" || !isDate(p.Date) {
			return
		}
		// The latest change a person made in the roster log, else the roster entry itself.
		var x store.Row
		if l, _ := s.st.DB.C("roster_log").FindOne(ctx, bson.M{"employee_id": req.EmployeeID, "date": p.Date, "by_dev": false},
			options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})); l != nil {
			x = store.Row{"late": l["late"], "by_name": l["by_name"], "why": ui3ApprovalsBlank(l["why"]), "from": ui3ApprovalsBlank(l["from_code"]), "to": l["to_code"], "at": l["at"]}
		} else if e, _ := s.st.DB.C("roster_entry").FindOne(ctx, bson.M{"employee_id": req.EmployeeID, "date": p.Date}); e != nil {
			x = store.Row{"late": e["late"], "by_name": e["by_name"], "why": ui3ApprovalsBlank(e["why"]), "from": ui3ApprovalsBlank(e["from_code"]), "to": e["code"], "at": e["at"]}
		}
		if x != nil {
			name, role := splitBy(str(x["by_name"]))
			if role == "" {
				role = name
			}
			row["rosterChange"] = map[string]any{"late": x["late"], "by": role, "byName": name, "why": x["why"], "from": x["from"], "to": x["to"], "at": x["at"]}
		}
	case "resign":
		if resignKind(&p) && p.CaseID != "" {
			row["caseId"] = p.CaseID
		}
	}
}

// ---------- Bulk: the Work Policy column of the employee master update (prototype TPL.upd) ----------

type polChange struct{ id, from, to string }

// bulkPolicy reads a row's Work Policy cell: blank keeps the policy; otherwise it must be an active
// work policy, and it is a change when it is not the one the employee has now.
func bulkPolicy(ctx context.Context, tx store.Q, empID, v string, res *bulkResult) *polChange {
	if v = strings.TrimSpace(v); v == "" {
		return nil
	}
	var id, name, status string
	if pol := ui3ApprovalsByName(ctx, tx, "work_policy", nil, v); pol != nil {
		id, name, status = str(pol["id"]), str(pol["name"]), str(pol["status"])
	} else {
		res.Errors = append(res.Errors, fmt.Sprintf("Work Policy “%s” not found", v))
		return nil
	}
	if status != "Active" {
		res.Errors = append(res.Errors, fmt.Sprintf("Work Policy “%s” is inactive", name))
		return nil
	}
	ts, err := timeSetFor(ctx, tx)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return nil
	}
	e, err := loadEmp(ctx, tx, empID)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return nil
	}
	if cur, _ := ts.policyFor(e); cur.ID != id {
		return &polChange{id, cur.Name, name}
	}
	return nil
}

// updReasonEff checks a row's Reason for change and Effective From as ApplyJobChange does, and
// returns the reason as the master spells it and the date.
func updReasonEff(ctx context.Context, tx store.Q, r bulkRow, doj string) (string, string, error) {
	m := ui3ApprovalsByName(ctx, tx, "simple_master", bson.M{"kind": "change_reason", "used_for": "Employee update", "status": "Active"}, r.V["Reason for change"])
	if m == nil {
		return "", "", bad("Reason “%s” is not in the list of employee update reasons", r.V["Reason for change"])
	}
	reason := str(m["name"])
	eff := excelDate(r.V["Effective From"])
	if !isDate(eff) {
		return "", "", bad("Effective From must be a date like 2026-11-01")
	}
	if eff < doj {
		return "", "", bad("Effective From is before the joining date")
	}
	return reason, eff, nil
}

// applyPolicyChange records a work policy change in the employee's change history and applies it now,
// or schedules it for its date (applyScheduledPolicies applies it then).
func applyPolicyChange(ctx context.Context, tx store.Q, c *Ctx, empID string, pc *polChange, reason, eff, source string) error {
	state := "Applied"
	if eff > today() {
		state = "Scheduled"
	}
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": empID, "field": "Work Policy", "old_value": pc.from, "new_value": pc.to, "reason": reason,
		"effective_from": eff, "state": state, "source": source, "apply_value": pc.id, "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return err
	}
	if state == "Applied" {
		if err := mapEmployeePolicy(ctx, tx, c, empID, pc.id); err != nil {
			return err
		}
	}
	return audit(ctx, tx, c, map[bool]string{true: "Scheduled work policy change", false: "Changed work policy"}[state == "Scheduled"], "employee", empID, nil,
		map[string]any{"from": pc.from, "to": pc.to, "effectiveFrom": eff, "reason": reason})
}

// mapEmployeePolicy puts an employee on a work policy by an employee-level mapping and switches the
// employee's mapping off on every other policy (a person belongs to one policy at a time).
func mapEmployeePolicy(ctx context.Context, tx store.Q, c *Ctx, empID, policyID string) error {
	mapping := bson.M{"policy_id": policyID, "level": "employee", "ref": empID}
	if ok, _ := tx.C("work_policy_map").Exists(ctx, mapping); ok {
		if _, err := tx.C("work_policy_map").UpdateMany(ctx, mapping, bson.M{"$set": bson.M{"on_map": true}}); err != nil {
			return err
		}
	} else if _, err := tx.C("work_policy_map").Insert(ctx, bson.M{"policy_id": policyID, "level": "employee", "ref": empID, "created_by": c.Name}); err != nil {
		return err
	}
	if err := configHistory(ctx, tx, c, "work_policy", policyID, []map[string]any{{"field": "applies to employee|" + empID, "to": "on"}}); err != nil {
		return err
	}
	return moveMapOff(ctx, tx, c, policyID, "employee", empID)
}

// applyScheduledPolicies applies work policy changes whose date has come (run by the maintenance job).
func applyScheduledPolicies(ctx context.Context, st *store.Store) (int, error) {
	due, err := st.DB.C("employee_change").Find(ctx, bson.M{"state": "Scheduled", "field": "Work Policy", "apply_column": nil, "apply_value": bson.M{"$ne": nil},
		"effective_from": bson.M{"$lte": store.Today()}}, options.Find().SetSort(bson.D{{Key: "effective_from", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return 0, err
	}
	sys := &Ctx{Name: "Scheduled change", Role: "System", EmployeeID: "system"}
	for _, d := range due {
		err := st.Tx(ctx, func(tx store.Q) error {
			if err := mapEmployeePolicy(ctx, tx, sys, str(d["employee_id"]), str(d["apply_value"])); err != nil {
				return err
			}
			_, err := tx.C("employee_change").Update(ctx, bson.M{"_id": d["id"]}, bson.M{"$set": bson.M{"state": "Applied"}})
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// ui3ApprovalsBlank is the old COALESCE of a text with the empty string: a NULL text reads as an empty one.
func ui3ApprovalsBlank(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// ui3ApprovalsByName is the first document of coll matching filter whose name equals name in any case
// (lower(name)=lower($1)), or nil.
func ui3ApprovalsByName(ctx context.Context, q store.Q, coll string, filter bson.M, name string) store.Row {
	rows, err := q.C(coll).Find(ctx, filter)
	if err != nil {
		return nil
	}
	for _, x := range rows {
		if strings.ToLower(str(x["name"])) == strings.ToLower(name) {
			return x
		}
	}
	return nil
}
