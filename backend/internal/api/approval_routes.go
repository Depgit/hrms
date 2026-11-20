package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

var leaveGroups = map[string]string{"Casual leave": "C", "Sick leave": "C"}

type requestPayload struct {
	// leave
	LType string `json:"ltype,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	// regularize
	Date  string `json:"date,omitempty"`
	RType string `json:"rtype,omitempty"`
	In    string `json:"in,omitempty"`
	Out   string `json:"out,omitempty"`
	// payroll items
	Amount    float64 `json:"amount,omitempty"`
	ClaimType string  `json:"claimType,omitempty"`
	Kind      string  `json:"kind,omitempty"` // p_change: Bank | Personal
	BankName  string  `json:"bankName,omitempty"`
	AccountNo string  `json:"accountNo,omitempty"`
	IFSC      string  `json:"ifsc,omitempty"`
	PayMonth  string  `json:"payMonth,omitempty"`
	// advances, deductions, arrears, claims (spec 15.2)
	Instalments int    `json:"instalments,omitempty"`
	StartYm     string `json:"startYm,omitempty"`
	DedType     string `json:"dedType,omitempty"`
	Heard       string `json:"heard,omitempty"`
	ForYm       string `json:"forYm,omitempty"`
	ToYm        string `json:"toYm,omitempty"`
	ArrearType  string `json:"arrearType,omitempty"`
	SlipText    string `json:"slipText,omitempty"`
	BillDate    string `json:"billDate,omitempty"`
	// separation: the last working day by notice, and an early release's shortfall (waive | recover)
	NoticeLWD string `json:"noticeLwd,omitempty"`
	Shortfall string `json:"shortfall,omitempty"`
	// separation and transfer
	ExitType      string   `json:"exitType,omitempty"`
	Waive         bool     `json:"waive,omitempty"`
	TKind         string   `json:"tkind,omitempty"`
	ToLocation    string   `json:"toLocation,omitempty"`
	ToDepartment  string   `json:"toDepartment,omitempty"`
	ToManager     string   `json:"toManager,omitempty"`
	ToDesignation string   `json:"toDesignation,omitempty"`
	Effective     string   `json:"effective,omitempty"`
	Half          bool     `json:"half,omitempty"`          // leave: a half day
	ScoreOverride string   `json:"scoreOverride,omitempty"` // leave: HR's reason for allowing it below the discipline score
	Doc           string   `json:"doc,omitempty"`           // leave: supporting document id
	Cov           []string `json:"cov,omitempty"`           // leave: the dates that count, set by the server
	OfferID       string   `json:"offerId,omitempty"`       // verify: the offer whose joining details are checked
	Note          string   `json:"note,omitempty"`
	// p_change, kind Personal
	Mobile     string `json:"mobile,omitempty"`
	Email      string `json:"email,omitempty"`
	Address    string `json:"address,omitempty"`
	ECName     string `json:"ecName,omitempty"`
	ECRelation string `json:"ecRelation,omitempty"`
	ECMobile   string `json:"ecMobile,omitempty"`
	// resign: the last working day and notice are set by the server, never by the client
	Reason     string `json:"reason,omitempty"`
	LWD        string `json:"lwd,omitempty"`
	NoticeDays int    `json:"noticeDays,omitempty"`
	// resign while on notice (prototype nrForm rkind): change | withdraw, the case and the date before
	RKind   string `json:"rkind,omitempty"`
	CaseID  string `json:"caseId,omitempty"`
	FromLWD string `json:"fromLwd,omitempty"`
}

// factsFor derives condition facts on the server; clients never supply grade, flow or counts.
// regCountCombined: the employee's work policy counts No Punch and All Other regularizations together.
func regCountCombined(ctx context.Context, q store.Q, employeeID string) bool {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return false
	}
	e, err := loadEmp(ctx, q, employeeID)
	if err != nil {
		return false
	}
	pol, _ := ts.policyFor(e)
	return pol != nil && pol.S.RegCount == "combined"
}

func factsFor(ctx context.Context, q store.Q, wf, employeeID string, p *requestPayload) (approval.Facts, error) {
	f := approval.Facts{}
	switch wf {
	case "leave":
		return leaveFacts(ctx, q, employeeID, p)
	case "regularize":
		if !isDate(p.Date) || p.Date > today() {
			return f, bad("Regularization is only for past or current dates")
		}
		if p.RType != "No Punch" && p.RType != "All Other" {
			return f, bad("Choose No Punch or All Other")
		}
		// Regularizations of this kind already pending or approved in the month; with the work policy's counting
		// "combined" both kinds count together (spec 7.2, regCount; TIME-278).
		filter := bson.M{"wf": "regularize", "employee_id": employeeID, "payload.date": bson.M{"$regex": "^" + regexp.QuoteMeta(p.Date[:7])},
			"payload.rtype": p.RType, "status": bson.M{"$in": []string{"Pending", "Approved"}}}
		if regCountCombined(ctx, q, employeeID) {
			delete(filter, "payload.rtype")
		}
		n, err := q.C("approval_request").Count(ctx, filter)
		if err != nil {
			return f, err
		}
		f.RType, f.Cnt = p.RType, float64(n)+1
	case "p_claim", "p_adv", "p_ded", "p_arrear", "p_fnf":
		if (p.Amount == 0 && wf != "p_fnf") || (p.Amount < 0 && wf != "p_arrear" && wf != "p_fnf") {
			return f, bad("Enter the amount")
		}
		f.Amt, f.CType = math.Abs(p.Amount), p.ClaimType
	case "p_change":
		if p.Kind != "Bank" && p.Kind != "Personal" {
			return f, bad("Change must be Bank or Personal")
		}
		if p.Kind == "Bank" && (!reAcct.MatchString(p.AccountNo) || !reIFSC.MatchString(strings.ToUpper(p.IFSC)) || p.BankName == "") {
			return f, bad("Check the bank name, account number and IFSC")
		}
		if p.Kind == "Personal" {
			if err := checkPersonalChange(ctx, q, employeeID, p); err != nil {
				return f, err
			}
		}
		f.CKind = p.Kind
	case "resign":
		f.SKind, f.Waive = p.ExitType, p.Waive
		if f.SKind == "" {
			f.SKind = "Resignation"
		}
		if f.SKind == "Resignation" {
			n, err := noticeFor(ctx, q, employeeID)
			if err != nil {
				return f, err
			}
			// The last working day follows from the notice period and cannot be chosen (spec 18.1).
			p.NoticeDays, p.LWD = n.Days, n.LWD
			if p.Reason == "" {
				p.Reason = "Better opportunity"
			}
		}
	case "transfer":
		f.ToLocation, f.ToDepartment, f.ToManager = p.ToLocation, p.ToDepartment, p.ToManager
		if p.ToLocation != "" {
			// Within city, cross city (same state), cross state (same region) or cross region.
			var from *approvalRoutesPlace
			if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": employeeID}); e != nil {
				from = approvalRoutesPlaceOf(ctx, q, e["location_id"])
			}
			to := approvalRoutesPlaceOf(ctx, q, p.ToLocation)
			if from == nil || to == nil {
				return f, bad("Pick a location to move to")
			}
			switch {
			case from.city == to.city:
				f.TKind = "Within city"
			case from.state == to.state:
				f.TKind = "Cross city"
			case from.region == to.region:
				f.TKind = "Cross state"
			default:
				f.TKind = "Cross region"
			}
		}
	}
	return f, nil
}

// approvalRoutesPlace is where a location sits: its city, the city's state and the state's region.
type approvalRoutesPlace struct{ city, state, region string }

// approvalRoutesPlaceOf follows a location up to its region; nil when a link is missing.
func approvalRoutesPlaceOf(ctx context.Context, q store.Q, locationID any) *approvalRoutesPlace {
	up := func(coll string, id any, field string) any {
		if id == nil {
			return nil
		}
		row, _ := q.C(coll).FindOne(ctx, bson.M{"_id": id})
		if row == nil {
			return nil
		}
		return row[field]
	}
	city := up("cluster", up("location", locationID, "cluster_id"), "city_id")
	state := up("city", city, "state_id")
	region := up("state", state, "region_id")
	if city == nil || state == nil || region == nil {
		return nil
	}
	return &approvalRoutesPlace{str(city), str(state), str(region)}
}

var workflowKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range approval.Workflows {
		m[w[0]] = true
	}
	return m
}()

type raiseBody struct {
	WF         string         `json:"wf"`
	EmployeeID string         `json:"employeeId"`
	Payload    requestPayload `json:"payload"`
}

func (s *Server) approvalSimulate(r *http.Request, c *Ctx) (any, error) {
	var b raiseBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.EmployeeID == "" {
		b.EmployeeID = c.EmployeeID
	}
	// Anyone may preview the route of their own request; other people's need the approvals view.
	if b.EmployeeID != c.EmployeeID {
		if err := c.Need("appr.queue", 1); err != nil {
			return nil, err
		}
	}
	// An improvement plan is raised from consequence management, but its route can be checked (MX.p_pip).
	if !workflowKeys[b.WF] && b.WF != "p_pip" {
		return nil, bad("Unknown workflow")
	}
	if b.WF == "shortlist" || b.WF == "interview" {
		return nil, bad("A shortlist's route is shown on the shortlist form in Onboarding")
	}
	var f approval.Facts
	var probs []string
	var err error
	if b.WF == "resign" && resignKind(&b.Payload) {
		f, probs, err = s.resignKindFacts(r.Context(), b.EmployeeID, &b.Payload)
	} else {
		f, err = factsFor(r.Context(), s.st.DB, b.WF, b.EmployeeID, &b.Payload)
	}
	if err != nil {
		return nil, err
	}
	plan, err := planChain(r.Context(), s.st.DB, b.WF, b.EmployeeID, f)
	if err != nil {
		return nil, err
	}
	out, err := s.apprUISimulate(r.Context(), c, &b, f, plan)
	if out != nil {
		out.Problems = append(out.Problems, probs...)
	}
	return out, err
}

func (s *Server) approvalRaise(r *http.Request, c *Ctx) (any, error) {
	var b raiseBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !workflowKeys[b.WF] {
		return nil, bad("Unknown workflow")
	}
	if b.WF == "joining" || b.WF == "verify" || b.WF == "shortlist" || b.WF == "interview" {
		return nil, bad("Shortlists, offers and joining verification are raised from Onboarding")
	}
	if b.WF == "p_ack" {
		return nil, bad("A signed policy is sent from your list of company policies")
	}
	if b.EmployeeID == "" {
		b.EmployeeID = c.EmployeeID
	}
	// Someone who has left raises no requests of their own (spec 6.4: payslips, tax, Form 16, F&F and own details
	// only; AUTH-051), except a reimbursement claim within 15 days of the last day (spec 15.2). HR and Payroll can
	// still raise items about them.
	if b.EmployeeID == c.EmployeeID && b.WF != "p_claim" { // a claim has its own rule: up to 15 days after the last day
		if e, _ := s.st.DB.C("employee").FindOne(r.Context(), bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"status": 1})); e != nil && e["status"] == "Inactive" {
			return nil, forbidden("You have left the company, so requests are closed. Ask HR if something needs to change")
		}
	}
	if b.WF == "transfer" {
		if c.Role == "Employee" {
			return nil, forbidden("A transfer is raised by a manager or HR")
		}
		if err := c.Need("appr.raise", 2); err != nil {
			return nil, err
		}
		if err := c.MustInScope(r.Context(), s.st.DB, b.EmployeeID); err != nil {
			return nil, err
		}
		if err := checkTransfer(r.Context(), s.st.DB, b.EmployeeID, &b.Payload); err != nil {
			return nil, err
		}
	}
	if b.WF == "regularize" && b.EmployeeID != c.EmployeeID {
		if err := c.Need("appr.raise", 2); err != nil {
			return nil, err
		}
		// "Manager can regularize for the team" in the employee's work policy (spec 10.1; TIME-280). HR still can.
		if !containsStr([]string{"HR Admin", "HRBP", "HRBP Corporate"}, c.Role) {
			if ts, err := loadTimeSet(r.Context(), s.st.DB); err == nil {
				if e, err := loadEmp(r.Context(), s.st.DB, b.EmployeeID); err == nil {
					if pol, _ := ts.policyFor(e); pol != nil && !pol.S.RG.Mgr {
						return nil, forbidden(e.Name + "'s work policy does not let managers regularize for the team. The employee raises it, or HR")
					}
				}
			}
		}
	}
	if b.WF == "resign" && resignKind(&b.Payload) {
		return s.resignKindRaise(r.Context(), c, &b) // change the last working day, or withdraw
	}
	if b.WF == "resign" {
		if b.Payload.RKind != "" && b.Payload.RKind != "new" {
			return nil, bad("Choose New resignation, Change last working day or Withdraw resignation")
		}
		if b.Payload.ExitType != "" && b.Payload.ExitType != "Resignation" {
			return nil, bad("Termination and other exits are started from Separation")
		}
		if b.EmployeeID != c.EmployeeID && c.Role != "HR Admin" && c.Role != "HRBP" && c.Role != "HRBP Corporate" {
			return nil, forbidden("A resignation is raised by the employee or by HR")
		}
		n, err := noticeFor(r.Context(), s.st.DB, b.EmployeeID)
		if err != nil {
			return nil, err
		}
		if !n.CanResign {
			return nil, bad("%s", n.Why)
		}
		if !containsStr(exitReasonsFor(c.Role), b.Payload.Reason) {
			return nil, bad("Choose a reason for leaving")
		}
		if b.EmployeeID == c.EmployeeID {
			// The employee's own resignation asks for no waiver: an earlier last day is a Change last working day
			// request, so a waiver sent with it is ignored and does not change the route (HIRE-422).
			b.Payload.Waive = false
		}
	}
	// An employee asks for their own salary advance (advances.go): the company's limits, months set by the server.
	if b.WF == "p_adv" && b.EmployeeID == c.EmployeeID {
		return s.raiseSelfAdvance(r.Context(), c, &b)
	}
	// Advances, deductions and arrears are raised by Payroll or HR for an employee (spec 15.2).
	// An arrear is raised by Payroll only (spec 7.1: "Payroll (or auto from revision)"; payroll test ITM-25).
	payDesk := (b.WF == "p_adv" || b.WF == "p_ded" || b.WF == "p_arrear") && (c.Level("pay.claims") >= 2 || (b.WF != "p_arrear" && c.Level("emp.assets") >= 2))
	if b.WF == "p_arrear" && !payDesk {
		return nil, forbidden("Arrears are raised by Payroll")
	}
	if (b.WF == "p_adv" || b.WF == "p_ded") && !payDesk {
		return nil, forbidden("Advances, deductions and arrears are raised by Payroll or HR")
	}
	if payDesk {
		if b.EmployeeID == c.EmployeeID {
			return nil, forbidden("Raise this for another employee; your own goes through your HR")
		}
		if err := c.MustInScope(r.Context(), s.st.DB, b.EmployeeID); err != nil {
			return nil, err
		}
	} else if b.EmployeeID != c.EmployeeID {
		if err := c.Need("appr.raise", 2); err != nil {
			return nil, err
		}
	}
	f, err := factsFor(r.Context(), s.st.DB, b.WF, b.EmployeeID, &b.Payload)
	if err != nil {
		return nil, err
	}
	// Eligibility, balance and window rules (spec 11.7, 12), checked as the person raising it.
	var problems []string
	switch b.WF {
	case "leave":
		problems = leaveErrors(r.Context(), s.st.DB, c, b.EmployeeID, &b.Payload, f.Days)
	case "regularize":
		problems = regErrors(r.Context(), s.st.DB, b.EmployeeID, &b.Payload)
	case "p_adv", "p_ded", "p_arrear", "p_claim":
		problems = payItemErrors(r.Context(), s.st.DB, c, b.WF, b.EmployeeID, &b.Payload)
	}
	if len(problems) > 0 {
		return nil, badD(problems[0], problems)
	}
	if b.WF == "resign" {
		// A resignation opens its separation case with the approval (spec 18).
		var out map[string]any
		err := s.st.Tx(r.Context(), func(tx store.Q) error {
			res, err := s.startSeparation(r.Context(), tx, c, sepStart{EmployeeID: b.EmployeeID, Type: "Resignation", Reason: b.Payload.Reason, Note: b.Payload.Note, Waive: b.Payload.Waive})
			if err != nil {
				return err
			}
			out = map[string]any{"id": res["requestId"], "case": res["id"]}
			return nil
		})
		return out, err
	}
	if b.WF == "leave" && b.Payload.ScoreOverride != "" {
		// Allowed below the discipline score by HR (checked in leaveErrors): the request and the audit entry together.
		var out map[string]any
		err := s.st.Tx(r.Context(), func(tx store.Q) error {
			var err error
			if out, err = s.createRequestTx(r.Context(), tx, c, b.WF, b.EmployeeID, f, b.Payload); err != nil {
				return err
			}
			return audit(r.Context(), tx, c, fmt.Sprintf("Leave %v allowed for %s below the discipline score: %s", out["id"], b.EmployeeID, b.Payload.ScoreOverride),
				"approval_request", fmt.Sprint(out["id"]), nil, map[string]any{"employeeId": b.EmployeeID, "reason": b.Payload.ScoreOverride, "ltype": b.Payload.LType, "from": b.Payload.From, "to": b.Payload.To})
		})
		return out, err
	}
	return s.CreateRequest(r.Context(), c, b.WF, b.EmployeeID, f, b.Payload)
}

// approvalQueue lists requests for a tab: mine (waiting on me), open, done, info.
func (s *Server) approvalQueue(r *http.Request, c *Ctx) (any, error) {
	return s.apprUIQueue(r.Context(), c, r.URL.Query().Get("tab"), r.URL.Query().Get("wf"))
}

// queue lists requests for one inbox tab, filtered by workflow when wfFilter is set.
func (s *Server) queue(ctx context.Context, c *Ctx, tab, wfFilter string) ([]store.Row, error) {
	if tab == "" {
		tab = "mine"
	}
	q := s.st.DB
	// Which requests: the tab's status, the workflow, and never an exit HR started about the caller that is not
	// approved yet (myrequests.go).
	which := bson.A{bson.M{"$nor": bson.A{bson.M{"$and": bson.A{bson.M{"employee_id": c.EmployeeID}, hrExitHiddenFilter("")}}}}}
	switch tab {
	case "done":
		which = append(which, bson.M{"status": bson.M{"$ne": "Pending"}})
	case "info": // requests the role is informed of, open or completed (prototype nm)
	default:
		which = append(which, bson.M{"status": "Pending"})
	}
	if c.Role == "Employee" { // My view never shows candidates
		which = append(which, bson.M{"candidate_id": nil, "offer_id": nil})
	}
	// One request type, or a comma-separated group of them (Payroll view's Payroll and Joining approvals tabs).
	if wfs := splitWF(wfFilter); len(wfs) == 1 {
		which = append(which, bson.M{"wf": wfs[0]})
	} else if len(wfs) > 1 {
		which = append(which, bson.M{"wf": bson.M{"$in": wfs}})
	}
	// Whose: in the caller's data scope (with the approvals view), about the caller or raised by them. "mine" also
	// needs pending items outside scope (receiving side, named approver); canAct decides.
	visible := c.Level("appr.queue") >= 1
	whose := bson.A{bson.M{"employee_id": c.EmployeeID}, bson.M{"raised_by": c.EmployeeID}}
	if visible {
		f, err := c.ScopeFilter(ctx, q, "_e.")
		if err != nil {
			return nil, err
		}
		whose = append(whose, f)
	}
	if tab == "mine" {
		whose = append(whose, bson.M{"status": "Pending"})
	}
	pipeline := bson.A{bson.D{{Key: "$match", Value: bson.M{"$and": which}}}}
	for _, st := range approvalSubjectDeptStages("_e", "_d") { // the subject and its department, both required
		pipeline = append(pipeline, st)
	}
	pipeline = append(pipeline, bson.D{{Key: "$match", Value: bson.M{"$or": whose}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}},
		bson.D{{Key: "$limit", Value: 500}})
	rows, err := q.C("approval_request").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, row := range rows {
		// The request's columns plus employee_name, department and in_scope, as the list always had them.
		e, _ := row["_e"].(map[string]any)
		d, _ := row["_d"].(map[string]any)
		delete(row, "_e")
		delete(row, "_d")
		row["employee_name"], row["department"], row["in_scope"] = e["name"], d["name"], false
		if visible {
			row["in_scope"] = approvalsScopeValue(c.Scope, e, str(d["name"]))
		}
		req, err := loadRequest(ctx, q, row["id"].(string), false)
		if err != nil {
			continue
		}
		mine := canAct(ctx, q, c, req)
		if tab == "mine" && (!mine || (req.WF == "p_ack" && !ackNamed(ctx, q, c, req))) {
			continue
		}
		if tab == "info" {
			informed := false
			for _, n := range asList(row["notify"]) {
				if m, ok := n.(map[string]any); ok && m["role"] == c.Role {
					informed = true
				}
			}
			if !informed {
				continue
			}
		}
		row["canAct"] = mine
		if req.Status == "Pending" && req.Step < len(req.Chain) {
			row["waitingOn"] = req.Chain[req.Step]
		}
		maskRequestPayload(c, row)
		out = append(out, row)
	}
	return out, nil
}

// splitWF reads the request type filter: empty for every type, else one type or a comma-separated list.
func splitWF(wf string) []string {
	out := []string{}
	for _, k := range strings.Split(wf, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func (s *Server) approvalGet(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	if row, ok, err := s.apprUISynthetic(ctx, c, r.PathValue("id")); ok { // improvement plans and tax proofs
		return row, err
	}
	req, err := loadRequest(ctx, s.st.DB, r.PathValue("id"), false)
	if err != nil {
		return nil, err
	}
	if c.Role == "Employee" && (isCandidateID(req.EmployeeID) || isOfferID(req.EmployeeID)) {
		return nil, forbidden("Candidates are not shown in My view") // switch to the role view that handles onboarding
	}
	if req.EmployeeID == c.EmployeeID && req.RaisedBy != c.EmployeeID && hrExitHidden(req) { // myrequests.go
		return nil, notFound("Request not found")
	}
	mine := canAct(ctx, s.st.DB, c, req)
	if !mine && req.EmployeeID != c.EmployeeID && req.RaisedBy != c.EmployeeID {
		if err := c.Need("appr.queue", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, req.EmployeeID); err != nil {
			return nil, err
		}
	}
	// The request's columns and the name of who it is about (nil when there is nobody: loadSubject then refuses).
	row, err := s.st.DB.C("approval_request").FindOne(ctx, bson.M{"_id": req.ID})
	if err != nil {
		return nil, err
	}
	if row != nil {
		e, err := approvalSubjectOf(ctx, s.st.DB, approvalSubjectID(row))
		if err != nil {
			return nil, err
		}
		if e == nil {
			row = nil
		} else {
			row["employee_name"] = e["name"]
		}
	}
	subj, err := loadSubject(ctx, s.st.DB, req.EmployeeID)
	if err != nil {
		return nil, err
	}
	people := []map[string]string{}
	for _, st := range req.Chain {
		people = append(people, map[string]string{"stage": st, "person": resolvePerson(ctx, s.st.DB, st, subj, req.Facts).Name})
	}
	row["people"] = people
	row["canAct"] = mine
	row["canCancel"] = req.Status == "Pending" && mayCancel(c, req) // myrequests.go
	row["canSkip"] = canSkip(ctx, s.st.DB, c, req)                  // HR's "Skip approval" (approvals.go)
	if req.Status == "Pending" && req.Step < len(req.Chain) {
		// Why the viewer has no Approve button, and whether the waiting stage can be decided at all.
		switch {
		case req.EmployeeID == c.EmployeeID:
			row["whyNot"] = "This request is about you, so others decide it."
		case req.RaisedBy == c.EmployeeID:
			row["whyNot"] = "You raised this request, so someone else decides it. Nobody approves a request they raised, including with the HR Admin override."
		}
		if why := blockedFn(ctx, s.st.DB, req, subj)(req.Chain[req.Step], req.Chain[:req.Step]); why != "" {
			row["stuck"] = why + ". Nobody else can decide it: map someone to the role in Access, or reject it."
		}
	}
	if req.WF == "leave" {
		// Team cover: who else in the department at this property is off on the same dates.
		p := payloadOf(req)
		off, err := apprUIColleagueLeaves(ctx, s.st.DB, req.EmployeeID, req.ID, p.From, p.To, false)
		if err != nil {
			off = nil
		}
		row["teamOff"] = off
		row["team"] = apprUITeamSize(ctx, s.st.DB, req.EmployeeID)
	}
	s.apprUIDetail(ctx, c, req, row)
	s.apprUI3Detail(ctx, req, row)
	maskRequestPayload(c, row)
	hist, err := s.st.DB.C("approval_history").Find(ctx, bson.M{"request_id": req.ID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	maskHistoryIDs(c, req.EmployeeID, hist)
	row["history"] = hist
	return row, err
}

func (s *Server) approvalDecide(action string) authedFn {
	return func(r *http.Request, c *Ctx) (any, error) {
		var b struct {
			Note        string `json:"note"`
			Instalments *int   `json:"instalments"` // a salary advance: the monthly instalments the approver sets (advances.go)
		}
		if r.ContentLength > 0 {
			if err := decode(r, &b); err != nil {
				return nil, err
			}
		}
		if req, err := loadRequest(r.Context(), s.st.DB, r.PathValue("id"), false); err == nil && req.WF == "interview" {
			return nil, bad("Record the interview with Selected or Not selected, with the date and your feedback")
		}
		if b.Instalments != nil && action == "approve" {
			var out map[string]any
			err := s.st.Tx(r.Context(), func(tx store.Q) error {
				if err := advanceSetInstalments(r.Context(), tx, c, r.PathValue("id"), *b.Instalments); err != nil {
					return err
				}
				var err error
				out, err = s.decideTx(r.Context(), tx, c, r.PathValue("id"), action, strings.TrimSpace(b.Note))
				return err
			})
			return out, err
		}
		return s.Decide(r.Context(), c, r.PathValue("id"), action, strings.TrimSpace(b.Note))
	}
}

// approvalSkip is HR's "Skip approval" on a request already waiting: approved at once with the reason, when its
// flow's approval hierarchy is off (skipApprovalTx says who may).
func (s *Server) approvalSkip(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var status string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		req, err := loadRequest(ctx, tx, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		if req.EmployeeID != c.EmployeeID && req.RaisedBy != c.EmployeeID && c.Role != "HR Admin" {
			if err := c.MustInScope(ctx, tx, req.EmployeeID); err != nil {
				return err
			}
		}
		if err := s.skipApprovalTx(ctx, tx, c, req, b.Reason); err != nil {
			return err
		}
		status = req.Status
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": status}, nil
}

// approvalSkipCheck tells a raise form whether to offer "Skip approval" next to "Send for approval": the caller is
// an HR role (who raises it may skip it) and the request type's flow has its approval hierarchy off. With a subject
// (employee, candidate or offer) its own flow decides; without one, any flow of the type; leave reads its group.
func (s *Server) approvalSkipCheck(r *http.Request, c *Ctx) (any, error) {
	ctx, q := r.Context(), r.URL.Query()
	wf, subj, group := q.Get("wf"), q.Get("subject"), q.Get("leaveGroup")
	if !mayCanSkip(c, c.EmployeeID) || wf == "" || subj == c.EmployeeID {
		return map[string]any{"skip": false}, nil
	}
	keys := []string{approval.MatrixKey(wf, group)}
	if wf == "leave" && group == "" {
		keys = []string{"leaveC", "leaveP"}
	}
	f := bson.M{"wf": bson.M{"$in": keys}, "hierarchy_off": true}
	if subj != "" {
		sj, err := loadSubject(ctx, s.st.DB, subj)
		if err != nil {
			return map[string]any{"skip": false}, nil
		}
		f["flow"] = string(approval.Operations)
		if sj.Corporate {
			f["flow"] = string(approval.Corporate)
		}
	}
	ok, err := s.st.DB.C(store.ApprovalFlowColl).Exists(ctx, f)
	if err != nil {
		return nil, err
	}
	return map[string]any{"skip": ok}, nil
}

func (s *Server) approvalCancel(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		req, err := loadRequest(ctx, tx, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		if req.Status != "Pending" {
			return bad("Only a request that is not yet approved can be cancelled")
		}
		if !mayCancel(c, req) { // myrequests.go
			if req.EmployeeID == c.EmployeeID {
				return forbidden(raisedByOther(ctx, tx, req.RaisedBy))
			}
			return forbidden("Only the person who raised it can cancel it")
		}
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": req.ID}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
			return err
		}
		if err := history(ctx, tx, req.ID, "", "Cancelled", c.Name, "Cancelled before approval"); err != nil {
			return err
		}
		if err := candidateRequestClosed(ctx, tx, c, req, "Cancelled", ""); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Cancelled request", "approval_request", req.ID, nil, nil)
	})
}

// ---------- Approval flow editor (rule lists) ----------

// flowsAll lists every rule list (approval_flow), by workflow key and flow.
func (s *Server) flowsAll(r *http.Request, c *Ctx) (any, error) {
	// The Employee view reads the flows too ("Check a flow", all flows at a glance), as in the prototype.
	if c.Level("appr.queue") < 1 && c.Role != "Employee" {
		return nil, forbidden("")
	}
	rows, err := s.st.DB.C(store.ApprovalFlowColl).Find(r.Context(), nil, options.Find().SetSort(bson.D{{Key: "wf", Value: 1}, {Key: "flow", Value: 1}}))
	if err != nil {
		return nil, err
	}
	// A flow whose approval hierarchy is off says how many of its requests wait (HR may skip their approval).
	for _, x := range rows {
		if x["hierarchy_off"] == true {
			n, err := s.st.DB.C("approval_request").Count(r.Context(), flowWaitingFilter(str(x["wf"]), str(x["flow"])))
			if err != nil {
				return nil, err
			}
			x["waiting"] = n
		}
	}
	return rows, nil
}

// flowWaitingFilter finds the requests of one rule list (workflow key and flow) still waiting for approval. Leave
// has two lists: casual and sick (C, also requests with no group) and privilege and other (P).
func flowWaitingFilter(key, flow string) bson.M {
	m := bson.M{"status": "Pending", "flow": flow, "wf": key}
	switch key {
	case "leaveC":
		m["wf"], m["facts.leaveGroup"] = "leave", bson.M{"$ne": "P"}
	case "leaveP":
		m["wf"], m["facts.leaveGroup"] = "leave", "P"
	}
	return m
}

// flowHierarchy ticks or clears a flow's "Approval hierarchy required" (every request type, the interview too).
// Cleared, requests still go to the approvers as usual, but HR may skip approval with a reason (skipApprovalTx), when
// raising one or on one waiting; the rules and approvers stay as they are. The answer says how many wait.
func (s *Server) flowHierarchy(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("appr.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		On *bool `json:"on"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.On == nil {
		return nil, bad("Say whether the approval hierarchy is on")
	}
	ctx, wf, flow := r.Context(), r.PathValue("wf"), r.PathValue("flow")
	var waiting int64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before, err := tx.C(store.ApprovalFlowColl).FindOne(ctx, bson.M{"wf": wf, "flow": flow})
		if err != nil || before == nil {
			return orNotFound(err)
		}
		if _, err := tx.C(store.ApprovalFlowColl).Update(ctx, bson.M{"wf": wf, "flow": flow},
			bson.M{"$set": bson.M{"hierarchy_off": !*b.On, "updated_by": c.Name, "updated_at": tx.Now()}}); err != nil {
			return err
		}
		if waiting, err = tx.C("approval_request").Count(ctx, flowWaitingFilter(wf, flow)); err != nil {
			return err
		}
		action := "Approval hierarchy switched on"
		if !*b.On {
			action = "Approval hierarchy switched off: HR can skip approval"
		}
		return audit(ctx, tx, c, action, store.ApprovalFlowColl, wf+"/"+flow, map[string]any{"hierarchy": before["hierarchy_off"] != true},
			map[string]any{"hierarchy": *b.On, "waiting": waiting})
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "hierarchy": *b.On, "waiting": waiting}, nil
}

// flowPut saves a workflow's rule list for a flow (approval.Check says what a valid list is) and its no-response
// setting, with an audit entry. It returns the rules as saved (named and with ids).
func (s *Server) flowPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("appr.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Rules      []approval.Rule `json:"rules"`
		NoResponse string          `json:"noResponse"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	wf, flow := r.PathValue("wf"), r.PathValue("flow")
	if wf == "interview" {
		return nil, bad("The interview is always taken by the hiring manager, so it has no route to change")
	}
	allowed, err := approvalColumns(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	rules, err := approval.Check(b.Rules, func(role string) bool { return containsStr(allowed, role) })
	if err != nil {
		return nil, bad("%s", err.Error())
	}
	// Only the conditions this request type carries, and the receiving side only on a transfer.
	if err := approval.CheckFits(wf, rules); err != nil {
		return nil, bad("%s", err.Error())
	}
	for i, rule := range rules {
		for _, cond := range rule.When {
			if cond.K == "ctype" && !containsStr(claimTypes, cond.Val) {
				return nil, bad("Rule %d (“%s”): pick a claim type (%s)", i+1, rule.Label, strings.Join(claimTypes, ", "))
			}
		}
	}
	if b.NoResponse != "" && b.NoResponse != "escalate" && b.NoResponse != "auto" && b.NoResponse != "wait" {
		return nil, bad("If no response must be escalate, auto or wait")
	}
	b.Rules = rules
	err = s.st.Tx(r.Context(), func(tx store.Q) error {
		before, err := tx.C(store.ApprovalFlowColl).FindOne(r.Context(), bson.M{"wf": wf, "flow": flow})
		if err != nil || before == nil {
			return orNotFound(err)
		}
		set := bson.M{"rules": rules, "updated_by": c.Name, "updated_at": tx.Now()}
		if b.NoResponse != "" { // blank keeps the flow's setting
			set["no_response"] = b.NoResponse
		}
		if _, err := tx.C(store.ApprovalFlowColl).Update(r.Context(), bson.M{"wf": wf, "flow": flow}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(r.Context(), tx, c, "Changed approval flow", store.ApprovalFlowColl, wf+"/"+flow, before, b)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "rules": rules}, nil
}

// flowTestOut is which rule decides a sample request and the route it gives.
type flowTestOut struct {
	Rule      int               `json:"rule"` // the rule's number in the list, from 1
	Label     string            `json:"label"`
	Otherwise bool              `json:"otherwise"`
	Chain     []string          `json:"chain"`
	Notify    []approval.Notify `json:"notify"`
	People    []StagePerson     `json:"people,omitempty"` // with an employee: who each stage is
	// HierarchyOff: the flow's approval hierarchy is off: the route below applies, and HR can skip approval.
	HierarchyOff bool `json:"hierarchyOff,omitempty"`
}

// flowTest runs sample facts (grade, leave days in 30 days, count and type, amount, exit type …) through a saved
// rule list: which rule decides and the approvers in order. Without an employee the route is in roles; with one,
// the people are named (and a person on two stages is asked once, as on a real request).
func (s *Server) flowTest(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("appr.setup", 1); err != nil {
		return nil, err
	}
	var b struct {
		Facts      approval.Facts `json:"facts"`
		EmployeeID string         `json:"employeeId"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, key, flow := r.Context(), r.PathValue("wf"), approval.Flow(r.PathValue("flow"))
	if ok, err := s.st.DB.C(store.ApprovalFlowColl).Exists(ctx, bson.M{"wf": key, "flow": string(flow)}); err != nil || !ok {
		return nil, orNotFound(err)
	}
	rs, off, err := loadFlow(ctx, s.st.DB, key, flow, "")
	if err != nil {
		return nil, err
	}
	f := b.Facts
	f.WF = key
	if strings.HasPrefix(key, "leave") {
		f.WF = "leave"
	}
	resolve := func(role string) string { return role }
	var subj *subject
	cache := map[string]Person{}
	if b.EmployeeID != "" {
		if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
			return nil, err
		}
		if subj, err = loadSubject(ctx, s.st.DB, b.EmployeeID); err != nil {
			return nil, err
		}
		if f.Grade == "" {
			f.Grade = subj.Grade
		}
		resolve = func(role string) string {
			p, ok := cache[role]
			if !ok {
				p = resolvePerson(ctx, s.st.DB, role, subj, f)
				cache[role] = p
			}
			return p.Name
		}
	}
	i := approval.Pick(rs, f)
	if i < 0 {
		return nil, bad("This flow has no rules")
	}
	ch := approval.Build(rs, f, resolve)
	if subj != nil { // two different people with the same name are both asked, as on a real request
		ch = approval.BuildKeyed(rs, f, resolve, func(role string) string { resolve(role); return personKey(cache[role]) })
	}
	out := &flowTestOut{Rule: i + 1, Label: rs.Rules[i].Label, Otherwise: i == len(rs.Rules)-1, Chain: ch.Chain, Notify: ch.Notify, HierarchyOff: off}
	if subj != nil {
		out.People = []StagePerson{}
		for _, st := range ch.Chain {
			resolve(st)
			pp, state := cache[st], "Ready"
			switch {
			case pp.ID == "":
				state = "No name set"
			case pp.ID == subj.ID:
				state = "Is the employee"
			case hasLeft(ctx, s.st.DB, pp.ID):
				state = "Has left"
			}
			out.People = append(out.People, StagePerson{st, pp, state})
		}
	}
	return out, nil
}

// ---------- Finalizers: what happens when the last approver approves ----------

func (s *Server) registerFinalizers() {
	s.finalizers["p_claim"] = finalizeClaim
	s.finalizers["p_arrear"] = finalizeArrear
	s.finalizers["p_ded"] = finalizeDeduction
	s.finalizers["p_adv"] = finalizeAdvance
	s.finalizers["p_change"] = func(ctx context.Context, tx store.Q, r *Request) (string, error) {
		var p requestPayload
		_ = json.Unmarshal(r.Payload, &p)
		if p.Kind != "Bank" {
			// Only the details given change; an insert when the person has no details row yet, so the approved
			// change is never lost.
			set := bson.M{}
			for k, v := range map[string]string{"mobile": p.Mobile, "email": p.Email, "address": p.Address, "ec_name": p.ECName, "ec_relation": p.ECRelation, "ec_mobile": p.ECMobile} {
				if v != "" {
					set[k] = v
				}
			}
			_, err := tx.C("employee_personal").Upsert(ctx, bson.M{"employee_id": r.EmployeeID}, set, nil)
			return "Personal details updated.", err
		}
		if _, err := tx.C("employee_bank").Insert(ctx, bson.M{"employee_id": r.EmployeeID, "bank_name": p.BankName, "account_no": p.AccountNo,
			"ifsc": strings.ToUpper(p.IFSC), "from_month": openYm(ctx, tx), "created_by": "Approval"}); err != nil {
			return "", err
		}
		// Salary held while this account waited is paid in the open month (RUN-77 / ITM-64).
		return "New account used from " + ymLabel(openYm(ctx, tx)) + ".", syncBankHolds(ctx, tx)
	}
	s.finalizers["transfer"] = finalizeTransfer
	s.finalizers["regularize"] = finalizeRegularize
	s.finalizers["joining"] = finalizeOffer
	s.finalizers["shortlist"] = s.finalizeShortlist // onboarding stages (onboarding_stages.go)
	s.finalizers["interview"] = s.finalizeInterview
	s.finalizers["verify"] = finalizeVerify
	s.finalizers["resign"] = finalizeResign // the separation, or a change or withdrawal of it
	s.finalizers["p_fnf"] = finalizeFnf
}
