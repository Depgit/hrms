package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Pay items (spec 15.2): salary advances, deductions, arrears, claims and salary holds. Each
// one that needs approval goes through the approval engine; its finalizer writes ledger lines.

var claimTypes = []string{"Travel", "Mobile and internet", "Other"}

// arrearTypes are the reasons offered for an arrear (the prototype's list; older reasons in legacyArrearTypes).
var arrearTypes = []string{"Pay rate correction", "Attendance or leave correction", "Short payment in earlier month", "Joining or exit adjustment", "Other"}

func addLedger(ctx context.Context, tx store.Q, emp, ym, kind, label string, amt float64, src, reason string) error {
	ym = joinPayMonth(ctx, tx, emp, ym) // never before the joining month, where there is no row to pay it (REAL-19)
	_, err := tx.C("ledger").Insert(ctx, bson.M{"employee_id": emp, "ym": ym, "kind": kind, "label": label, "amount": calc.Round2(amt),
		"source_id": src, "reason": payitemsNull(reason)})
	return err
}

// payitemsNull stores an empty text as null (the old NULLIF).
func payitemsNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func maxYm(a, b string) string {
	if a > b {
		return a
	}
	return b
}

// payitemsReasonOn: name is an active change reason of the master used for this kind of pay item.
func payitemsReasonOn(ctx context.Context, q store.Q, usedFor, name string) bool {
	ok, _ := q.C("simple_master").Exists(ctx, bson.M{"kind": "change_reason", "used_for": usedFor, "status": "Active", "name": name})
	return ok
}

// payitemsReasons lists the active change reasons used for this kind of pay item, in master order.
func payitemsReasons(ctx context.Context, q store.Q, usedFor string) ([]store.Row, error) {
	return q.C("simple_master").Find(ctx, bson.M{"kind": "change_reason", "used_for": usedFor, "status": "Active"},
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "name", Value: 1}}))
}

// payItemErrors checks an advance, deduction, arrear or claim before it is raised.
func payItemErrors(ctx context.Context, q store.Q, c *Ctx, wf, empID string, p *requestPayload) []string {
	er := []string{}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return []string{"Employee not found"}
	}
	open := openYm(ctx, q)
	p.Note = strings.TrimSpace(p.Note)
	switch wf {
	case "p_adv":
		if e.Type != "Permanent" || e.Status != "Active" {
			return []string{"Salary advances are for active permanent staff only"}
		}
		if mx := loadAdvanceRules(ctx, q).MaxInstalments; p.Instalments < 1 || p.Instalments > mx {
			er = append(er, fmt.Sprintf("Recover the advance in 1 to %d instalments", mx))
		}
		if p.PayMonth == "" {
			p.PayMonth = open
		}
		if !isYm(p.PayMonth) || p.PayMonth < open || p.PayMonth > nextYm(open) {
			er = append(er, "Pay the advance with the open payroll month or the next one")
		}
		if p.StartYm == "" {
			p.StartYm = nextYm(p.PayMonth)
		}
		if !isYm(p.StartYm) || p.StartYm <= p.PayMonth {
			er = append(er, "Recovery starts after the month the advance is paid")
		}
		if len(p.Note) < 3 {
			er = append(er, "Give the reason for the advance")
		}
	case "p_ded":
		if e.Type == "Contractual" {
			return []string{"Contractual staff are paid without deductions. Recover this another way"}
		}
		if !payitemsReasonOn(ctx, q, "Deduction", p.DedType) {
			er = append(er, "Choose the type of deduction")
		}
		if p.Instalments < 1 {
			p.Instalments = 1
		}
		if p.StartYm == "" {
			p.StartYm = open
		}
		if !isYm(p.StartYm) || p.StartYm < open {
			er = append(er, "Start the deduction in the open payroll month or later")
		}
		// For people who have left it starts after the last working day month, in one go.
		if e.Status == "Inactive" && e.LWD != nil {
			p.StartYm, p.Instalments = maxYm(p.StartYm, nextYm((*e.LWD)[:7])), 1
		}
		if len(p.Note) < 5 {
			er = append(er, "Write what happened, with the date and the amount involved")
		}
		if p.DedType != store.SysName(ctx, q, "reason.excess_recovery") && p.Heard != "Yes" {
			er = append(er, "Under the Payment of Wages Act the employee must be given a chance to explain before this kind of deduction. Record that they were heard")
		}
		// Proof is optional; when given it is a file uploaded for this employee: on the employee's file, or on
		// one of their requests (owner_id "<employee>/…").
		if p.Doc != "" {
			if ok, _ := q.C("document").Exists(ctx, bson.M{"_id": p.Doc, "$or": bson.A{
				bson.M{"owner_kind": "approval", "owner_id": bson.M{"$regex": "^" + regexp.QuoteMeta(empID) + "(/|$)"}},
				bson.M{"owner_kind": "employee", "owner_id": empID},
			}}); !ok {
				er = append(er, "Attach the proof again")
			}
		}
	case "p_arrear":
		if !isYm(p.ForYm) || p.ForYm >= open {
			er = append(er, "An arrear is for a month already paid, before "+ymLabel(open))
		}
		if p.ToYm == "" {
			p.ToYm = p.ForYm
		}
		if !isYm(p.ToYm) || p.ToYm < p.ForYm || p.ToYm >= open {
			er = append(er, "The arrear months must end before the open month")
		}
		if p.PayMonth == "" {
			p.PayMonth = open
		}
		if !isYm(p.PayMonth) || p.PayMonth < open {
			er = append(er, "Pay the arrear in the open month or later")
		}
		if !arrearTypeOK(p.ArrearType) {
			er = append(er, "Choose the reason for the arrear")
		}
		if p.SlipText = strings.TrimSpace(p.SlipText); len(p.SlipText) < 3 {
			er = append(er, "Write the text the payslip will show")
		}
	case "p_claim":
		// The work policy can switch off employees' own claims; Payroll or HR can still raise one for them.
		if empID == c.EmployeeID && !claimsOn(ctx, q, empID) {
			return []string{"Your work policy does not allow reimbursement claims. Ask Payroll or HR if you need one raised"}
		}
		if !containsStr(claimTypes, p.ClaimType) {
			er = append(er, "Choose the claim type: "+strings.Join(claimTypes, ", "))
		}
		if !isDate(p.BillDate) || p.BillDate > today() {
			er = append(er, "Give the bill date (today or earlier)")
		}
		if p.Doc == "" {
			er = append(er, "Attach the bill")
		} else if ok, _ := q.C("document").Exists(ctx, bson.M{"_id": p.Doc, "owner_kind": "employee", "owner_id": empID}); !ok {
			er = append(er, "Attach the bill again")
		} else if prev, _ := q.C("approval_request").FindOne(ctx, bson.M{"wf": "p_claim", "payload.doc": p.Doc, "status": bson.M{"$in": bson.A{"Pending", "Approved"}}}); prev != nil {
			// The same bill on a second claim would be paid twice (payroll test ITM-32).
			er = append(er, fmt.Sprintf("This bill is already on claim %s (%s). Attach a different bill", str(prev["id"]), strings.ToLower(str(prev["status"]))))
		}
		// Claims are allowed up to 15 days after the last working day.
		if e.LWD != nil && today() > policy.AddDays(*e.LWD, 15) {
			er = append(er, "Claims close 15 days after the last working day ("+longDate(*e.LWD)+")")
		}
	}
	return er
}

// claimsOn: the employee's work policy lets them claim reimbursements ("Employees can claim reimbursements").
func claimsOn(ctx context.Context, q store.Q, empID string) bool {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return true
	}
	e, err := loadEmp(ctx, q, empID)
	if err != nil {
		return true
	}
	p, _ := ts.policyFor(e)
	return p.S.Claims.On
}

func payloadOf(r *Request) requestPayload {
	var p requestPayload
	_ = json.Unmarshal(r.Payload, &p)
	return p
}

func finalizeClaim(ctx context.Context, tx store.Q, r *Request) (string, error) {
	p := payloadOf(r)
	ym := payLineMonth(ctx, tx, "claim", openYm(ctx, tx)) // the next month once Reimbursement closure is closed
	ym = joinPayMonth(ctx, tx, r.EmployeeID, ym)
	return fmt.Sprintf("Added to %s pay lines.", ymLabel(ym)), addLedger(ctx, tx, r.EmployeeID, ym, "earn", "Reimbursement: "+p.ClaimType, p.Amount, r.ID, p.Note)
}

func finalizeArrear(ctx context.Context, tx store.Q, r *Request) (string, error) {
	p := payloadOf(r)
	open := openYm(ctx, tx)
	if p.Amount < 0 { // a recovery lowers pay: the next month once Deduction closure is closed
		open = payLineMonth(ctx, tx, "deduction", open)
	}
	ym := maxYm(p.PayMonth, open)
	label := "Arrear"
	if p.ForYm != "" {
		label += " for " + ymTime(p.ForYm).Format("Jan 2006")
		if p.ToYm != "" && p.ToYm != p.ForYm {
			label += " to " + ymTime(p.ToYm).Format("Jan 2006")
		}
	}
	kind := "earn"
	if p.Amount < 0 {
		kind, label = "ded", strings.Replace(label, "Arrear", "Arrear recovery", 1)
	}
	text := p.SlipText
	if text == "" {
		text = p.Note
	}
	return fmt.Sprintf("Added to %s pay lines.", ymLabel(ym)), addLedger(ctx, tx, r.EmployeeID, ym, kind, label, math.Abs(p.Amount), r.ID, text)
}

func finalizeDeduction(ctx context.Context, tx store.Q, r *Request) (string, error) {
	p := payloadOf(r)
	start := maxYm(p.StartYm, payLineMonth(ctx, tx, "deduction", openYm(ctx, tx))) // the next month once Deduction closure is closed
	n := max(1, p.Instalments)
	label := p.DedType
	if label == "" {
		label = "Deduction"
	}
	for i, amt := range calc.Instalments(p.Amount, n) {
		l := label
		if n > 1 {
			l += fmt.Sprintf(" (%d of %d)", i+1, n)
		}
		if err := addLedger(ctx, tx, r.EmployeeID, addYm(start, i), "ded", l, amt, r.ID, p.Note); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Deducted from %s in %d instalment%s.", ymLabel(start), n, map[bool]string{true: "", false: "s"}[n == 1]), nil
}

func finalizeAdvance(ctx context.Context, tx store.Q, r *Request) (string, error) {
	p := payloadOf(r)
	pay := maxYm(p.PayMonth, payLineMonth(ctx, tx, "deduction", openYm(ctx, tx))) // the next month once Deduction closure is closed
	start := maxYm(p.StartYm, nextYm(pay))
	n := max(1, p.Instalments)
	if err := addLedger(ctx, tx, r.EmployeeID, pay, "earn", "Salary advance", p.Amount, r.ID, p.Note); err != nil {
		return "", err
	}
	for i, amt := range calc.Instalments(p.Amount, n) {
		if err := addLedger(ctx, tx, r.EmployeeID, addYm(start, i), "ded", fmt.Sprintf("Advance recovery (%d of %d)", i+1, n), amt, r.ID, p.Note); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Paid with %s; %d recovery lines from %s.", ymLabel(pay), n, ymLabel(start)), nil
}

// ---------- Lists for the Employee dues and Payroll pages ----------

func (s *Server) canDues(c *Ctx, min int) error {
	if c.Level("pay.claims") >= min || c.Level("emp.assets") >= min {
		return nil
	}
	return forbidden("")
}

// payitemsEmployeeStages join the employee each document is about (field) as `e` — its id, name, type, status
// and the fields a scope names — dropping documents without one (JOIN employee e; every employee has a
// department, so JOIN department d kept them all), and keep those inside scope (c.ScopeFilter with prefix "e.").
func payitemsEmployeeStages(field string, scope bson.D) bson.A {
	return bson.A{
		bson.M{"$lookup": bson.M{"from": "employee", "localField": field, "foreignField": "_id", "as": "e", "pipeline": bson.A{
			bson.M{"$project": bson.M{"_id": 0, "id": "$_id", "name": 1, "type": 1, "status": 1, "location_id": 1, "department_id": 1, "manager_id": 1}},
		}}},
		bson.M{"$unwind": "$e"},
		bson.M{"$match": scope},
	}
}

// payItemList lists one kind of pay item with its ledger lines (advances, deductions, arrears, claims, changes).
func (s *Server) payItemList(r *http.Request, c *Ctx) (any, error) {
	// Payroll viewers (pay.run) read pay items too; changing them needs pay.claims or emp.assets.
	if err := s.canDues(c, 1); err != nil && c.Level("pay.run") < 1 {
		return nil, err
	}
	wf := map[string]string{"advances": "p_adv", "deductions": "p_ded", "arrears": "p_arrear", "claims": "p_claim", "changes": "p_change"}[r.PathValue("kind")]
	if wf == "" {
		return nil, notFound("")
	}
	ctx := r.Context()
	scope, err := c.ScopeFilter(ctx, s.st.DB, "e.")
	if err != nil {
		return nil, err
	}
	pipe := append(bson.A{bson.M{"$match": bson.M{"wf": wf}}}, payitemsEmployeeStages("employee_id", scope)...)
	pipe = append(pipe,
		bson.M{"$sort": bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}},
		bson.M{"$limit": 500},
		// Who acted first on the request, who raised it, and the waiver of a deduction, if any.
		bson.M{"$lookup": bson.M{"from": "approval_history", "localField": "_id", "foreignField": "request_id", "as": "h",
			"pipeline": bson.A{bson.M{"$sort": bson.M{"_id": 1}}, bson.M{"$limit": 1}}}},
		bson.M{"$lookup": bson.M{"from": "employee", "localField": "raised_by", "foreignField": "_id", "as": "rb"}},
		bson.M{"$lookup": bson.M{"from": "deduction_waiver", "localField": "_id", "foreignField": "request_id", "as": "w"}},
		bson.M{"$project": bson.M{"_id": 0, "id": "$_id", "employee_id": 1, "name": "$e.name", "type": "$e.type", "payload": 1, "status": 1, "chain": 1,
			"step": 1, "created_at": 1, "first_action": bson.M{"$first": "$h.by_name"}, "raised_by_name": bson.M{"$first": "$rb.name"},
			"waived": bson.M{"$first": "$w.amount"}, "waive_note": bson.M{"$first": "$w.note"}, "waived_by": bson.M{"$first": "$w.by_name"},
			"waived_at": bson.M{"$first": "$w.at"}}},
	)
	rows, err := s.st.DB.C("approval_request").Aggregate(ctx, pipe)
	if err != nil {
		return nil, err
	}
	payengineFill(rows, "first_action", "raised_by_name", "waived", "waive_note", "waived_by", "waived_at")
	reqIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		row["created"] = payengineStamp(row["created_at"], "02 Jan 2006")
		row["waived_at"] = payengineStamp(row["waived_at"], "02 Jan 2006")
		delete(row, "created_at")
		reqIDs = append(reqIDs, str(row["id"]))
	}
	// Each item's ledger lines, by month.
	lines, _ := s.st.DB.C("ledger").Find(ctx, bson.M{"source_id": bson.M{"$in": reqIDs}}, options.Find().
		SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}).
		SetProjection(bson.D{{Key: "source_id", Value: 1}, {Key: "ym", Value: 1}, {Key: "kind", Value: 1}, {Key: "label", Value: 1},
			{Key: "amount", Value: 1}, {Key: "void", Value: 1}, {Key: "void_reason", Value: 1}}))
	bySource := map[string][]store.Row{}
	for _, l := range lines {
		src := str(l["source_id"])
		delete(l, "source_id")
		if l["void_reason"] == nil {
			l["void_reason"] = ""
		}
		bySource[src] = append(bySource[src], l)
	}
	open := openYm(ctx, s.st.DB)
	for _, row := range rows {
		lines := bySource[str(row["id"])]
		if lines == nil {
			lines = []store.Row{}
		}
		bal := 0.0
		for _, l := range lines {
			if l["kind"] == "ded" && l["void"] == false && l["ym"].(string) >= open {
				bal += l["amount"].(float64)
			}
		}
		row["lines"], row["balance"] = lines, calc.Round2(bal)
		if st := row["status"]; st == "Pending" {
			chain, _ := row["chain"].([]any)
			step := int(row["step"].(int32))
			if step < len(chain) {
				row["waitingOn"] = chain[step]
			}
		}
	}
	ded, _ := payitemsReasons(ctx, s.st.DB, "Deduction") // master order
	dedTypes := []string{}
	for _, d := range ded {
		dedTypes = append(dedTypes, d["name"].(string))
	}
	return map[string]any{"items": rows, "openYm": open, "claimTypes": claimTypes, "arrearTypes": arrearTypes, "dedTypes": dedTypes, "noHearingType": store.SysName(ctx, s.st.DB, "reason.excess_recovery")}, nil // a recovery of excess pay needs no hearing
}

// advanceAction: skip moves the next instalment to the end; recover collects the balance in one month.
func (s *Server) advanceAction(r *http.Request, c *Ctx) (any, error) {
	if err := s.canDues(c, 2); err != nil {
		return nil, err
	}
	var b struct {
		YM string `json:"ym"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &b); err != nil {
			return nil, err
		}
	}
	ctx, id, act := r.Context(), r.PathValue("id"), r.PathValue("action")
	var msg string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		req, err := loadRequest(ctx, tx, id, true)
		if store.Transient(err) {
			return err
		}
		if err != nil || req.WF != "p_adv" {
			return notFound("Advance not found")
		}
		if req.Status != "Approved" {
			return bad("Only an approved advance has a recovery schedule")
		}
		if err := c.MustInScope(ctx, tx, req.EmployeeID); err != nil {
			return err
		}
		open := openYm(ctx, tx)
		// The recovery instalments still to come, in order.
		left, err := tx.C("ledger").Find(ctx, bson.M{"source_id": id, "kind": "ded", "void": false, "ym": bson.M{"$gte": open}}, options.Find().
			SetSort(bson.D{{Key: "ym", Value: 1}, {Key: "_id", Value: 1}}).
			SetProjection(bson.D{{Key: "ym", Value: 1}, {Key: "label", Value: 1}, {Key: "amount", Value: 1}, {Key: "reason", Value: 1}}))
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return bad("Nothing is left to recover")
		}
		for _, l := range left {
			if l["reason"] == nil {
				l["reason"] = ""
			}
		}
		void := func(lid any, why string) error {
			_, err := tx.C("ledger").Update(ctx, bson.M{"_id": lid}, bson.M{"$set": bson.M{"void": true, "void_reason": why, "voided_at": tx.Now()}})
			return err
		}
		switch act {
		case "skip":
			first, last := left[0], left[len(left)-1]
			to := nextYm(last["ym"].(string))
			if err := void(first["id"], "Moved to "+ymLabel(to)); err != nil {
				return err
			}
			if err := addLedger(ctx, tx, req.EmployeeID, to, "ded", first["label"].(string)+" (moved)", first["amount"].(float64), id, first["reason"].(string)); err != nil {
				return err
			}
			msg = fmt.Sprintf("The %s instalment moved to %s.", ymLabel(first["ym"].(string)), ymLabel(to))
		case "recover":
			if b.YM == "" {
				b.YM = open
			}
			if !isYm(b.YM) || b.YM < open {
				return bad("Recover in the open month or later")
			}
			bal := 0.0
			for _, l := range left {
				bal += l["amount"].(float64)
				if err := void(l["id"], "Balance recovered in "+ymLabel(b.YM)); err != nil {
					return err
				}
			}
			if err := addLedger(ctx, tx, req.EmployeeID, b.YM, "ded", "Advance recovery (balance)", bal, id, left[0]["reason"].(string)); err != nil {
				return err
			}
			msg = fmt.Sprintf("Balance of %s set for %s.", calc.Inr(bal), ymLabel(b.YM))
		default:
			return notFound("")
		}
		return audit(ctx, tx, c, "Advance "+act, "approval_request", id, nil, msg)
	})
	return map[string]string{"message": msg}, err
}

// ---------- Salary holds ----------

func (s *Server) holdList(r *http.Request, c *Ctx) (any, error) {
	if err := s.canDues(c, 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	scope, err := c.ScopeFilter(ctx, s.st.DB, "e.")
	if err != nil {
		return nil, err
	}
	pipe := append(payitemsEmployeeStages("employee_id", scope),
		bson.M{"$sort": bson.D{{Key: "status", Value: 1}, {Key: "at", Value: -1}, {Key: "_id", Value: -1}}},
		bson.M{"$project": bson.M{"_id": 0, "id": "$_id", "employee_id": 1, "name": "$e.name", "reason": 1, "note": 1, "from_ym": 1, "status": 1,
			"by_name": 1, "at": 1, "released_by": 1, "released_at": 1, "release_note": 1, "paid": 1, "emp_status": "$e.status", "to_ym": 1}},
	)
	rows, err := s.st.DB.C("salary_hold").Aggregate(ctx, pipe)
	if err != nil {
		return nil, err
	}
	payengineFill(rows, "released_by", "released_at", "release_note")
	for _, h := range rows {
		h["at"] = payengineStamp(h["at"], "02 Jan 2006")
		h["released_at"] = payengineStamp(h["released_at"], "02 Jan 2006")
		if h["status"] == "Active" {
			amt, _ := heldAmount(ctx, s.st.DB, h["employee_id"].(string), h["from_ym"].(string))
			h["held"] = amt
		}
	}
	reasons, _ := payitemsReasons(ctx, s.st.DB, "Hold") // master order
	return map[string]any{"holds": rows, "reasons": reasons, "openYm": openYm(ctx, s.st.DB), "canEdit": c.Level("pay.claims") >= 2, "absentReason": store.SysName(ctx, s.st.DB, "reason.absent_hold")}, nil
}

// payitemsRunMonths are the payroll months already approved or published whose ym matches cond.
func payitemsRunMonths(ctx context.Context, q store.Q, cond bson.M) ([]any, error) {
	return q.C("payroll_month").Distinct(ctx, "ym", bson.M{"status": bson.M{"$in": bson.A{"Approved", "Published"}}, "ym": cond})
}

// heldAmount adds up the held net of the months already run.
func heldAmount(ctx context.Context, q store.Q, emp, from string) (float64, error) {
	months, err := payitemsRunMonths(ctx, q, bson.M{"$gte": from})
	if err != nil {
		return 0, err
	}
	rows, err := q.C("payroll_row").Find(ctx, bson.M{"employee_id": emp, "ym": bson.M{"$in": months}, "hold": true},
		options.Find().SetProjection(bson.D{{Key: "net", Value: 1}}))
	if err != nil {
		return 0, err
	}
	v := 0.0
	for _, r := range rows {
		v += payengineFloat(r["net"])
	}
	return calc.Round2(v), nil
}

func (s *Server) holdAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeID string `json:"employeeId"`
		Reason     string `json:"reason"`
		Note       string `json:"note"`
		FromYm     string `json:"fromYm"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	if !payitemsReasonOn(ctx, s.st.DB, "Hold", b.Reason) {
		return nil, bad("Choose the reason for the hold")
	}
	if b.Note = strings.TrimSpace(b.Note); len(b.Note) < 5 {
		return nil, bad("Write why the salary is held")
	}
	open := openYm(ctx, s.st.DB)
	if b.FromYm == "" {
		b.FromYm = open
	}
	if !isYm(b.FromYm) || b.FromYm < open {
		return nil, bad("A hold starts in the open month (%s) or later", ymLabel(open))
	}
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		if id, err = placeHold(ctx, tx, c.Name, b.EmployeeID, b.Reason, b.Note, b.FromYm, ""); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Salary put on hold", "salary_hold", id, nil, b)
	})
	return map[string]string{"id": id}, err
}

// placeHold puts a salary on hold from a month (one active hold per person); source names what placed it.
func placeHold(ctx context.Context, tx store.Q, by, emp, reason, note, from, source string) (string, error) {
	if ok, _ := tx.C("salary_hold").Exists(ctx, bson.M{"employee_id": emp, "status": "Active"}); ok {
		return "", bad("This employee already has a hold. Release it first")
	}
	n, err := store.NextSeq(ctx, tx, "salary_hold_seq")
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("HLD-%d", n)
	if _, err := tx.C("salary_hold").Insert(ctx, bson.M{"_id": id, "employee_id": emp, "reason": reason, "note": note, "from_ym": from, "by_name": by,
		"source_id": payitemsNull(source)}); err != nil {
		return "", err
	}
	return id, nil
}

// releaseHold pays every held month that was run as "Held salary released" in the open month.
func releaseHold(ctx context.Context, tx store.Q, by, id, note string) (float64, error) {
	h, err := tx.C("salary_hold").Lock(ctx, bson.M{"_id": id})
	if err != nil || h == nil {
		return 0, notFound("Hold not found")
	}
	emp, from, status := str(h["employee_id"]), str(h["from_ym"]), str(h["status"])
	if status != "Active" {
		return 0, bad("This hold is already released")
	}
	open := openYm(ctx, tx)
	months, err := payitemsRunMonths(ctx, tx, bson.M{"$gte": from, "$lt": open})
	if err != nil {
		return 0, err
	}
	rows, err := tx.C("payroll_row").Find(ctx, bson.M{"employee_id": emp, "ym": bson.M{"$in": months}, "hold": true},
		options.Find().SetSort(bson.D{{Key: "ym", Value: 1}}).SetProjection(bson.D{{Key: "ym", Value: 1}, {Key: "net", Value: 1}}))
	if err != nil {
		return 0, err
	}
	paid, total := []map[string]any{}, 0.0
	for _, row := range rows {
		n := calc.Round2(row["net"].(float64)) // the stored net, as the bank advice pays it (ITM-59)
		if n <= 0 {
			continue
		}
		if err := addLedger(ctx, tx, emp, open, "earn", "Held salary released, "+ymLabel(row["ym"].(string)), n, id, "Hold released: "+note); err != nil {
			return 0, err
		}
		paid = append(paid, map[string]any{"ym": row["ym"], "amount": n})
		total += n
	}
	_, err = tx.C("salary_hold").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Released", "released_by": by, "released_at": tx.Now(),
		"release_note": note, "paid": paid}})
	return total, err
}

func (s *Server) holdRelease(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 2); err != nil {
		return nil, err
	}
	var b struct {
		Note string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Note = strings.TrimSpace(b.Note); len(b.Note) < 3 {
		return nil, bad("Enter the reason for releasing")
	}
	ctx, id := r.Context(), r.PathValue("id")
	var total float64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		h, err := tx.C("salary_hold").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.D{{Key: "employee_id", Value: 1}}))
		if err != nil || h == nil {
			return notFound("Hold not found")
		}
		if err := c.MustInScope(ctx, tx, str(h["employee_id"])); err != nil {
			return err
		}
		if total, err = releaseHold(ctx, tx, c.Name, id, b.Note); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Salary hold released", "salary_hold", id, nil, map[string]any{"note": b.Note, "paid": total})
	})
	return map[string]any{"paid": total}, err
}

// ---------- Upcoming pay lines (spec 15.2): the next six months of ledger lines ----------

func (s *Server) upcoming(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 1); err != nil {
		if s.canDues(c, 1) != nil {
			return nil, err
		}
	}
	ctx := r.Context()
	open := openYm(ctx, s.st.DB)
	months := []string{}
	for i := 0; i < 6; i++ {
		months = append(months, addYm(open, i))
	}
	scope, err := c.ScopeFilter(ctx, s.st.DB, "e.")
	if err != nil {
		return nil, err
	}
	// The lines of the six months, by month, then name.
	pipe := append(bson.A{bson.M{"$match": bson.M{"void": false, "ym": bson.M{"$gte": open, "$lte": addYm(open, 5)}}}}, payitemsEmployeeStages("employee_id", scope)...)
	pipe = append(pipe,
		bson.M{"$sort": bson.D{{Key: "ym", Value: 1}, {Key: "e.name", Value: 1}, {Key: "_id", Value: 1}}},
		bson.M{"$project": bson.M{"_id": 0, "id": "$_id", "employee_id": 1, "name": "$e.name", "ym": 1, "kind": 1, "label": 1, "amount": 1,
			"reason": bson.M{"$ifNull": bson.A{"$reason", ""}}, "source_id": 1}},
	)
	lines, err := s.st.DB.C("ledger").Aggregate(ctx, pipe)
	if err != nil {
		return nil, err
	}
	payengineFill(lines, "source_id")
	totals := map[string]map[string]float64{}
	for _, m := range months {
		totals[m] = map[string]float64{"earn": 0, "ded": 0, "count": 0}
	}
	for _, l := range lines {
		t := totals[l["ym"].(string)]
		t[l["kind"].(string)] += l["amount"].(float64)
		t["count"]++
	}
	// Pay items waiting for approval, newest first.
	pipe = append(bson.A{bson.M{"$match": bson.M{"status": "Pending", "wf": bson.M{"$regex": "^p_"}}}}, payitemsEmployeeStages("employee_id", scope)...)
	pipe = append(pipe,
		bson.M{"$sort": bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}},
		bson.M{"$limit": 200},
		bson.M{"$project": bson.M{"_id": 0, "id": "$_id", "wf": 1, "employee_id": 1, "name": "$e.name", "payload": 1, "created_at": 1}},
	)
	pending, _ := s.st.DB.C("approval_request").Aggregate(ctx, pipe)
	for _, p := range pending {
		p["created"] = payengineStamp(p["created_at"], "02 Jan 2006")
		delete(p, "created_at")
	}
	pending = append(pending, s.pendingPips(ctx, c)...) // improvement plans waiting, "PIP · N days"
	// Changes ahead: salary revisions and bank accounts from a later month, by month, then name.
	later := bson.M{"$match": bson.M{"from_month": bson.M{"$gt": open}}}
	pipe = append(append(bson.A{later}, payitemsEmployeeStages("employee_id", scope)...),
		bson.M{"$project": bson.M{"_id": 0, "employee_id": 1, "name": "$e.name", "ym": "$from_month", "what": bson.M{"$literal": "Salary revision"},
			"old": "$old_gross", "new": "$gross"}})
	changes, _ := s.st.DB.C("gross_history").Aggregate(ctx, pipe)
	pipe = append(append(bson.A{later}, payitemsEmployeeStages("employee_id", scope)...),
		bson.M{"$project": bson.M{"_id": 0, "employee_id": 1, "name": "$e.name", "ym": "$from_month", "what": bson.M{"$concat": bson.A{"Bank account ", "$bank_name"}},
			"old": bson.M{"$literal": nil}, "new": bson.M{"$literal": nil}}})
	banks, _ := s.st.DB.C("employee_bank").Aggregate(ctx, pipe)
	changes = append(changes, banks...)
	payengineFill(changes, "old", "new")
	sort.SliceStable(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if str(a["ym"]) != str(b["ym"]) {
			return str(a["ym"]) < str(b["ym"])
		}
		return str(a["name"]) < str(b["name"])
	})
	return map[string]any{"months": months, "lines": lines, "totals": totals, "pending": pending, "changes": changes, "openYm": open}, nil
}

func (s *Server) upcomingSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	v, err := s.upcoming(r, c)
	if err != nil {
		return "", nil, nil, err
	}
	m := v.(map[string]any)
	out := [][]any{}
	for _, l := range m["lines"].([]store.Row) {
		kind := "Addition"
		if l["kind"] == "ded" {
			kind = "Deduction"
		}
		out = append(out, []any{l["ym"], l["employee_id"], l["name"], kind, l["label"], l["amount"], l["reason"], l["source_id"]})
	}
	return "upcoming-pay-lines-" + m["openYm"].(string) + ".xlsx", []string{"Month", "Emp ID", "Name", "Type", "Line", "Amount", "Reason", "Source"}, out, nil
}
