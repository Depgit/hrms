package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Salary advances, beyond what Payroll or HR raise for an employee (payitems.go, owner's request of 12 Nov 2026):
//
//   - The employee asks for one themselves (My pay › Salary advance): amount, instalments (1 to 12) and the reason.
//     It is paid with the open payroll month (the next one once Deduction closure is closed) and recovered from the
//     month after. Active permanent staff only, as today; the company's limits (advanceRules) apply. It goes through
//     the p_adv approval rules like any advance, and the employee may cancel it while it waits.
//   - Payroll, at its own approval step, may change the instalments, the pay month and the first recovery month
//     before approving (PUT /api/approvals/{id}/advance-terms), in the request's history and the audit log.
//   - Opening advances: advances given before the HRMS went live and still being recovered, loaded from Excel
//     (template, preview, confirm). Each good row becomes an approved advance marked opening, with no "Salary
//     advance" earning line (it was paid before go-live) and recovery lines for the balance.
//
//	GET  /api/me/advance                  the caller's own form: eligibility, limits, open advance, months
//	GET  /api/advances/settings           the limits;  PUT the same {maxMultiple?, oneOpen?} (pay.setup 2)
//	PUT  /api/approvals/{id}/advance-terms {instalments, payMonth, startYm}
//	GET  /api/advances/opening/template   the Excel template
//	POST /api/advances/opening/preview    the filled file (multipart "file"): what would happen, nothing changes
//	POST /api/advances/opening/apply      the same file: every row checked again, good rows added

// ---------- The company's limits ----------

// advanceRules are the limits on an employee's own advance request (Payroll › Pay schedule and components).
type advanceRules struct {
	MaxMultiple    float64 `json:"maxMultiple"`    // the most one may ask for, × monthly gross; 0: no limit
	OneOpen        bool    `json:"oneOpen"`        // one advance open at a time
	MinMonths      int     `json:"minMonths"`      // months of service before an employee may ask (owner, 14 Nov 2026)
	MaxInstalments int     `json:"maxInstalments"` // the most monthly instalments an advance is recovered in
}

func loadAdvanceRules(ctx context.Context, q store.Q) advanceRules {
	r := advanceRules{MaxMultiple: store.DefaultAdvMaxMultiple, OneOpen: store.DefaultAdvOneOpen, MinMonths: store.DefaultAdvMinMonths, MaxInstalments: store.DefaultAdvMaxInstalments}
	cs, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"adv_max_multiple": 1, "adv_one_open": 1,
		"adv_min_months": 1, "adv_max_instalments": 1}))
	if cs != nil {
		if v := payengineIntPtr(cs["adv_min_months"]); v != nil {
			r.MinMonths = *v
		}
		if v := payengineIntPtr(cs["adv_max_instalments"]); v != nil && *v >= 1 {
			r.MaxInstalments = *v
		}
		if v := cs["adv_max_multiple"]; v != nil {
			r.MaxMultiple = payengineFloat(v)
		}
		if v, ok := cs["adv_one_open"].(bool); ok {
			r.OneOpen = v
		}
	}
	return r
}

// advanceSettings is GET /api/advances/settings.
func (s *Server) advanceSettings(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.setup") < 1 && c.Level("pay.claims") < 1 {
		return nil, forbidden("")
	}
	return map[string]any{"rules": loadAdvanceRules(r.Context(), s.st.DB), "canEdit": c.Level("pay.setup") >= 2}, nil
}

// advanceSettingsPut is PUT /api/advances/settings {maxMultiple?, oneOpen?, minMonths?, maxInstalments?}: only the fields
// sent change.
func (s *Server) advanceSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		MaxMultiple    *float64 `json:"maxMultiple"`
		OneOpen        *bool    `json:"oneOpen"`
		MinMonths      *int     `json:"minMonths"`
		MaxInstalments *int     `json:"maxInstalments"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.MaxMultiple == nil && b.OneOpen == nil && b.MinMonths == nil && b.MaxInstalments == nil {
		return nil, bad("Nothing to save")
	}
	if b.MinMonths != nil && (*b.MinMonths < 0 || *b.MinMonths > 36) {
		return nil, bad("The service needed is 0 to 36 months")
	}
	if b.MaxInstalments != nil && (*b.MaxInstalments < 1 || *b.MaxInstalments > 24) {
		return nil, bad("The most instalments is 1 to 24")
	}
	if b.MaxMultiple != nil && (*b.MaxMultiple < 0 || *b.MaxMultiple > 12 || math.IsNaN(*b.MaxMultiple)) {
		return nil, bad("The most an employee can ask for is 0 (no limit) to 12 times the monthly gross")
	}
	ctx := r.Context()
	var after advanceRules
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before := loadAdvanceRules(ctx, tx)
		set := bson.M{"updated_at": tx.Now()}
		after = before
		if b.MaxMultiple != nil {
			after.MaxMultiple = calc.Round2(*b.MaxMultiple)
			set["adv_max_multiple"] = after.MaxMultiple
		}
		if b.OneOpen != nil {
			after.OneOpen = *b.OneOpen
			set["adv_one_open"] = after.OneOpen
		}
		if b.MinMonths != nil {
			after.MinMonths = *b.MinMonths
			set["adv_min_months"] = after.MinMonths
		}
		if b.MaxInstalments != nil {
			after.MaxInstalments = *b.MaxInstalments
			set["adv_max_instalments"] = after.MaxInstalments
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Salary advance limits updated", "company_setting", "1", before, after)
	})
	return map[string]any{"rules": after}, err
}

// ---------- The employee's own request ----------

// advanceOpen is an employee's advance still open: waiting for approval, or approved with recovery lines (not void)
// from the open month on. Empty id when there is none.
type advanceOpen struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"`
	Balance float64 `json:"balance"`
}

func openAdvanceOf(ctx context.Context, q store.Q, emp, open string) (*advanceOpen, error) {
	reqs, err := q.C("approval_request").Find(ctx, bson.M{"employee_id": emp, "wf": "p_adv", "status": bson.M{"$in": bson.A{"Pending", "Approved"}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetProjection(bson.M{"status": 1}))
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if r["status"] == "Pending" {
			return &advanceOpen{ID: str(r["id"]), Status: "Pending"}, nil
		}
		lines, err := q.C("ledger").Find(ctx, bson.M{"source_id": r["id"], "kind": "ded", "void": false, "ym": bson.M{"$gte": open}},
			options.Find().SetProjection(bson.M{"amount": 1}))
		if err != nil {
			return nil, err
		}
		if len(lines) > 0 {
			bal := 0.0
			for _, l := range lines {
				bal += payengineFloat(l["amount"])
			}
			return &advanceOpen{ID: str(r["id"]), Status: "Approved", Balance: calc.Round2(bal)}, nil
		}
	}
	return nil, nil
}

// advanceMax is the most the employee may ask for under the rules: 0 with no limit; -1 when there is a limit but
// no monthly gross on record to work it out from.
func advanceMax(rules advanceRules, gross float64) float64 {
	if rules.MaxMultiple <= 0 {
		return 0
	}
	if gross <= 0 {
		return -1
	}
	return math.Floor(gross * rules.MaxMultiple)
}

// multipleText is "1 ×", "1.5 ×".
func multipleText(m float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(m, 'f', 2, 64), "0"), ".0") + " ×"
}

// selfAdvanceMonths sets the months of an employee's own advance: paid with the open payroll month, or the next one
// once its Deduction closure is closed (as the advance's lines would land), and recovered from the month after, in full
// (one instalment): the employee writes a wish for more months in the reason, and the approver sets the instalments
// when approving (owner's decision of 14 Nov 2026).
func selfAdvanceMonths(ctx context.Context, q store.Q, p *requestPayload) {
	p.PayMonth = payLineMonth(ctx, q, "deduction", openYm(ctx, q))
	p.StartYm = nextYm(p.PayMonth)
	p.Instalments = 1
}

// serviceFrom is the day an employee who joined on doj completes months of service.
func serviceFrom(doj string, months int) string {
	t, err := time.Parse("2006-01-02", doj)
	if err != nil {
		return doj
	}
	return t.AddDate(0, months, 0).Format("2006-01-02")
}

// selfAdvanceWhyNot is why an employee may not ask for an advance themselves ("" when they may): active permanent
// staff with at least the company's months of service (default 3).
func selfAdvanceWhyNot(e *emp, rules advanceRules) string {
	if e.Type != "Permanent" {
		return fmt.Sprintf("Salary advances are for permanent employees only (you are %s)", e.Type)
	}
	if e.Status != "Active" {
		return "Salary advances are for active permanent staff only"
	}
	if from := serviceFrom(e.DOJ, rules.MinMonths); rules.MinMonths > 0 && today() < from {
		return fmt.Sprintf("You can ask for a salary advance after %d months' service, from %s (you joined on %s)", rules.MinMonths, longDate(from), longDate(e.DOJ))
	}
	return ""
}

// selfAdvanceErrors checks who may ask (selfAdvanceWhyNot) and the company's limits on an employee's own advance
// (before payItemErrors, so the first reason given is the clearest).
func selfAdvanceErrors(ctx context.Context, q store.Q, emp string, p *requestPayload) []string {
	er := []string{}
	open := openYm(ctx, q)
	rules := loadAdvanceRules(ctx, q)
	if e, err := loadEmp(ctx, q, emp); err == nil {
		if why := selfAdvanceWhyNot(e, rules); why != "" {
			return []string{why}
		}
	}
	gross, _ := grossFor(ctx, q, emp, open)
	switch mx := advanceMax(rules, gross); {
	case mx < 0:
		er = append(er, "Your monthly gross is not on record, so the limit cannot be worked out. Ask Payroll")
	case mx > 0 && p.Amount > mx:
		er = append(er, fmt.Sprintf("You can ask for up to %s (%s your monthly gross of %s)", calc.Inr(mx), multipleText(rules.MaxMultiple), calc.Inr(gross)))
	}
	if rules.OneOpen {
		o, err := openAdvanceOf(ctx, q, emp, open)
		switch {
		case err != nil:
			er = append(er, "Your open advances could not be checked. Try again")
		case o != nil && o.Status == "Pending":
			er = append(er, fmt.Sprintf("You already have an advance waiting for approval (%s). Only one advance can be open at a time", o.ID))
		case o != nil:
			er = append(er, fmt.Sprintf("You already have an advance being recovered (%s, %s still to recover). Only one advance can be open at a time", o.ID, calc.Inr(o.Balance)))
		}
	}
	return er
}

// raiseSelfAdvance stores an employee's own advance. The employee is locked first, so two requests sent together
// cannot both pass the one-at-a-time rule.
func (s *Server) raiseSelfAdvance(ctx context.Context, c *Ctx, b *raiseBody) (any, error) {
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": b.EmployeeID}); err != nil {
			return err
		}
		f, err := factsFor(ctx, tx, b.WF, b.EmployeeID, &b.Payload)
		if err != nil {
			return err
		}
		selfAdvanceMonths(ctx, tx, &b.Payload)
		problems := selfAdvanceErrors(ctx, tx, b.EmployeeID, &b.Payload) // who may ask first: the clearest reason
		if len(problems) == 0 {
			problems = payItemErrors(ctx, tx, c, b.WF, b.EmployeeID, &b.Payload)
		}
		if len(problems) > 0 {
			return badD(problems[0], problems)
		}
		out, err = s.createRequestTx(ctx, tx, c, b.WF, b.EmployeeID, f, b.Payload)
		return err
	})
	return out, err
}

// myAdvance is GET /api/me/advance: what the caller's own advance form shows. eligible says whether they may ask
// now; why, when not. maxAmount 0 means no limit.
func (s *Server) myAdvance(r *http.Request, c *Ctx) (any, error) {
	ctx, q := r.Context(), s.st.DB
	open := openYm(ctx, q)
	rules := loadAdvanceRules(ctx, q)
	var p requestPayload
	selfAdvanceMonths(ctx, q, &p)
	out := map[string]any{"rules": rules, "openYm": open, "payMonth": p.PayMonth, "startYm": p.StartYm, "maxInstalments": rules.MaxInstalments, "instalments": 1,
		"eligible": false, "why": "", "gross": 0.0, "maxAmount": 0.0, "open": nil}
	e, err := loadEmp(ctx, q, c.EmployeeID)
	if err != nil {
		out["why"] = "Your employee record could not be read. Ask HR"
		return out, nil
	}
	gross, _ := grossFor(ctx, q, c.EmployeeID, open)
	mx := advanceMax(rules, gross)
	out["gross"], out["maxAmount"] = gross, math.Max(mx, 0)
	o, err := openAdvanceOf(ctx, q, c.EmployeeID, open)
	if err != nil {
		return nil, err
	}
	if o != nil {
		out["open"] = o
	}
	why := selfAdvanceWhyNot(e, rules)
	switch {
	case why != "":
		out["why"] = why
	case mx < 0:
		out["why"] = "Your monthly gross is not on record, so the limit cannot be worked out. Ask Payroll"
	case rules.OneOpen && o != nil && o.Status == "Pending":
		out["why"] = fmt.Sprintf("Your advance %s is waiting for approval. Only one advance can be open at a time", o.ID)
	case rules.OneOpen && o != nil:
		out["why"] = fmt.Sprintf("Your advance %s still has %s to recover. Only one advance can be open at a time", o.ID, calc.Inr(o.Balance))
	default:
		out["eligible"] = true
	}
	return out, nil
}

// ---------- Payroll changes the recovery at its approval step ----------

// advanceTermsLine is "4 instalments from January 2027, paid with December 2026".
func advanceTermsLine(n int, pay, start string) string {
	return fmt.Sprintf("%d instalment%s from %s, paid with %s", n, map[bool]string{true: "", false: "s"}[n == 1], ymLabel(start), ymLabel(pay))
}

// advanceTerms is PUT /api/approvals/{id}/advance-terms {instalments, payMonth, startYm}: whoever may decide the
// advance's Payroll or Payroll Manager stage now changes how it is paid and recovered, before approving it.
func (s *Server) advanceTerms(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Instalments int    `json:"instalments"`
		PayMonth    string `json:"payMonth"`
		StartYm     string `json:"startYm"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	var msg string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		req, err := loadRequest(ctx, tx, id, true)
		if store.Transient(err) {
			return err
		}
		if err != nil || req.WF != "p_adv" {
			return notFound("Advance not found")
		}
		if req.Status != "Pending" || req.Step >= len(req.Chain) {
			return bad("Only an advance waiting for approval can be changed")
		}
		if !canAct(ctx, tx, c, req) {
			return forbidden("This advance is not waiting on you")
		}
		stage := req.Chain[req.Step]
		if role := approval.StageRole(stage); role != "Payroll" && role != "Payroll Manager" {
			return forbidden("The recovery is changed at the Payroll step, before Payroll approves")
		}
		p := payloadOf(req)
		open := openYm(ctx, tx)
		er := []string{}
		if mx := loadAdvanceRules(ctx, tx).MaxInstalments; b.Instalments < 1 || b.Instalments > mx {
			er = append(er, fmt.Sprintf("Recover the advance in 1 to %d instalments", mx))
		}
		if !isYm(b.PayMonth) || b.PayMonth < open || b.PayMonth > nextYm(open) {
			er = append(er, "Pay the advance with the open payroll month or the next one")
		}
		if !isYm(b.StartYm) || b.StartYm <= b.PayMonth || b.StartYm > addYm(b.PayMonth, 6) {
			er = append(er, "Recovery starts after the month the advance is paid, within six months")
		}
		if len(er) > 0 {
			return badD(er[0], er)
		}
		n0 := max(1, p.Instalments)
		pay0, start0 := p.PayMonth, p.StartYm
		if pay0 == "" {
			pay0 = open
		}
		if start0 == "" {
			start0 = nextYm(pay0)
		}
		if b.Instalments == n0 && b.PayMonth == pay0 && b.StartYm == start0 {
			return bad("Nothing changed")
		}
		var raw map[string]any
		if err := json.Unmarshal(req.Payload, &raw); err != nil || raw == nil {
			raw = map[string]any{}
		}
		raw["instalments"], raw["payMonth"], raw["startYm"] = b.Instalments, b.PayMonth, b.StartYm
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"payload": raw}}); err != nil {
			return err
		}
		msg = fmt.Sprintf("Recovery changed by %s (%s): %s → %s.", c.Name, stage, advanceTermsLine(n0, pay0, start0), advanceTermsLine(b.Instalments, b.PayMonth, b.StartYm))
		if err := history(ctx, tx, id, "", "Changed", c.Name, msg); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Advance recovery changed", "approval_request", id,
			map[string]any{"instalments": n0, "payMonth": pay0, "startYm": start0},
			map[string]any{"instalments": b.Instalments, "payMonth": b.PayMonth, "startYm": b.StartYm})
	})
	return map[string]string{"message": msg}, err
}

// ---------- Opening advances from Excel ----------

const (
	openMaxRows   = 2000
	openMaxBytes  = 5 << 20
	openMaxMonths = 60
	openSheet     = "Opening advances"
)

var openColumns = []string{"Employee ID", "Employee name", "Advance date", "Original amount", "Balance still to recover", "Monthly recovery", "Recovery starts", "Reference / reason"}

// openingAllowed: Payroll (claims at Edit) or HR Admin.
func openingAllowed(c *Ctx) error {
	if c.Level("pay.claims") >= 2 || c.Role == "HR Admin" {
		return nil
	}
	return forbidden("Opening advances are loaded by Payroll or HR Admin")
}

// openingTemplate is GET /api/advances/opening/template: the empty sheet with its columns and "How to fill".
func (s *Server) openingTemplate(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err == nil {
		err = openingAllowed(c)
	}
	if err == nil && !c.CanDownload {
		err = forbidden("Your role cannot download files")
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx := r.Context()
	open := openYm(ctx, s.st.DB)
	first := payLineMonth(ctx, s.st.DB, "deduction", open)
	f := excelize.NewFile()
	defer f.Close()
	_ = f.SetSheetName("Sheet1", openSheet)
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"FDE68A"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}, Border: bulkBorder("9CA3AF")})
	ref, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}, Border: bulkBorder("9CA3AF")})
	text, _ := f.NewStyle(&excelize.Style{NumFmt: 49})
	widths := []float64{14, 26, 14, 16, 18, 16, 16, 36}
	for i, h := range openColumns {
		cn, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellValue(openSheet, cn+"1", h)
		st := head
		if i == 1 {
			st = ref // the name is for reference only
		}
		_ = f.SetCellStyle(openSheet, cn+"1", cn+"1", st)
		_ = f.SetColWidth(openSheet, cn, cn, widths[i])
	}
	// Every cell as text, so Excel keeps 2026-12 and dates as typed.
	_ = f.SetCellStyle(openSheet, "A2", fmt.Sprintf("H%d", openMaxRows+1), text)
	_ = f.SetRowHeight(openSheet, 1, 32)
	_ = f.SetPanes(openSheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	how := "How to fill"
	_, _ = f.NewSheet(how)
	title, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	wrap, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	lines := [][2]string{
		{"t", "Opening salary advances"},
		{"", "Use this file for advances given to employees before the HRMS went live that are still being recovered from salary. One row per advance."},
		{"", "The amount was already paid, so nothing is added to pay: only the balance is deducted, month by month, and shown on the payslip."},
		{"", ""},
		{"b", "Columns"},
		{"", "Employee ID: as in the HRMS, for example EMP-1020. The employee must be active and permanent."},
		{"", "Employee name: for your reference only; it is not read."},
		{"", "Advance date: the day the advance was given, as YYYY-MM-DD (for example 2026-08-14), today or earlier."},
		{"", "Original amount: the advance given, in rupees, for example 30000."},
		{"", "Balance still to recover: what is left to deduct from salary. Above 0 and not more than the original amount."},
		{"", "Monthly recovery: the amount deducted each month. The last month takes whatever is left, so it can be smaller."},
		{"", fmt.Sprintf("Recovery starts: the first month to deduct, as YYYY-MM. %s or later (the open payroll month%s).",
			first, map[bool]string{true: " is closed for deductions, so the month after", false: ""}[first != open])},
		{"", "Reference / reason: the old voucher or register number, or why it was given. Needed: the same employee and reference is loaded only once."},
		{"", ""},
		{"b", "Example"},
		{"", "EMP-1020 | Karan Mehta | 2026-08-14 | 30000 | 18000 | 5000 | " + first + " | Voucher 2231, medical"},
		{"", fmt.Sprintf("This deducts 5,000 in %s and the next two months, and 3,000 in the fourth month.", ymLabel(first))},
		{"", ""},
		{"b", "What happens"},
		{"", "1. Upload the file on Employee dues › Advances › Upload opening advances. You first see each row as Will add, or Skipped with the reason. Nothing changes until you confirm."},
		{"", "2. A row is skipped when the Employee ID is unknown, not active or not permanent, the balance is more than the original amount or not above 0, the monthly recovery is not above 0, the recovery would start before the open payroll month, the recovery would take more than 60 months, or the same employee and reference was already loaded or is listed twice in the file."},
		{"", "3. Confirm checks every row again. Each good row becomes an approved advance tagged Opening balance, with its recovery months. A row that fails does not stop the others."},
		{"", "4. Skip next and Recover balance work on these advances like any other. If the employee leaves, the balance is recovered in the full and final."},
		{"", fmt.Sprintf("Up to %d rows and 5 MB per file. Upload this Excel file (.xlsx).", openMaxRows)},
	}
	_ = f.SetColWidth(how, "A", "A", 120)
	for i, l := range lines {
		cell := fmt.Sprintf("A%d", i+1)
		_ = f.SetCellValue(how, cell, l[1])
		st := wrap
		switch l[0] {
		case "t":
			st = title
		case "b":
			st = bold
		}
		_ = f.SetCellStyle(how, cell, cell, st)
	}
	f.SetActiveSheet(0)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		writeErr(w, err)
		return
	}
	name := "opening-advances.xlsx"
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	_, _ = w.Write(buf.Bytes())
}

// openLine is one row of the opening advances file and what happens to it.
type openLine struct {
	Line        int     `json:"line"`
	EmployeeID  string  `json:"employeeId"`
	Name        string  `json:"name"` // as stored (the file's name column is not read)
	AdvanceDate string  `json:"advanceDate"`
	Original    float64 `json:"original"`
	Balance     float64 `json:"balance"`
	Monthly     float64 `json:"monthly"`
	StartYm     string  `json:"startYm"`
	Reference   string  `json:"reference"`
	Months      int     `json:"months"`  // recovery instalments
	Last        float64 `json:"last"`    // the last instalment
	Outcome     string  `json:"outcome"` // preview: add | skip; apply: added | skipped | failed
	Reason      string  `json:"reason,omitempty"`
	ID          string  `json:"id,omitempty"` // the advance created

	cells map[string]string // the row as read
	plan  *openAdvPlan      // from Bulk update › Opening advances: the advance as given, before the HRMS took over
}

// openAmount reads a rupee amount: "30000", "30,000", "₹ 30,000.50". ok false when it is not a number.
func openAmount(v string) (float64, bool) {
	v = strings.NewReplacer("₹", "", ",", "", " ", "", "Rs.", "", "Rs", "", "INR", "").Replace(strings.TrimSpace(v))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return calc.Round2(n), true
}

var reSerial = regexp.MustCompile(`^\d{4,6}(\.\d+)?$`)

// openDate reads a date: YYYY-MM-DD, DD-MM-YYYY, DD/MM/YYYY, "14 Aug 2026", or an Excel date number.
func openDate(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if reSerial.MatchString(v) {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 20000 && n < 80000 {
			if t, err := excelize.ExcelDateToTime(n, false); err == nil {
				return t.Format("2006-01-02"), true
			}
		}
	}
	for _, l := range []string{"2006-01-02", "02-01-2006", "2-1-2006", "02/01/2006", "2/1/2006", "02 Jan 2006", "2 Jan 2006", "02-Jan-2006", "2-Jan-2006", "02 January 2006", "2 January 2006", "2006/01/02"} {
		if t, err := time.Parse(l, v); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	return "", false
}

// openMonth reads a month: YYYY-MM, MM/YYYY, "Dec 2026", "December 2026", a date, or an Excel date number.
func openMonth(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if isYm(v) {
		return v, true
	}
	for _, l := range []string{"01/2006", "1/2006", "01-2006", "1-2006", "Jan 2006", "January 2006", "Jan-2006", "Jan-06", "2006/01"} {
		if t, err := time.Parse(l, v); err == nil {
			return t.Format("2006-01"), true
		}
	}
	if d, ok := openDate(v); ok {
		return d[:7], true
	}
	return "", false
}

// openReadFile reads the uploaded file: the sheet with the Employee ID and Balance still to recover columns.
func openReadFile(r *http.Request) ([]openLine, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, openMaxBytes+64<<10)
	if err := r.ParseMultipartForm(openMaxBytes); err != nil {
		return nil, bad("The file must be 5 MB or smaller")
	}
	file, h, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose the Excel file")
	}
	defer file.Close()
	if h.Size > openMaxBytes {
		return nil, bad("The file must be 5 MB or smaller")
	}
	if !strings.HasSuffix(strings.ToLower(h.Filename), ".xlsx") {
		return nil, bad("Upload the Excel file (.xlsx) made from Download template")
	}
	b, err := io.ReadAll(io.LimitReader(file, openMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > openMaxBytes {
		return nil, bad("The file must be 5 MB or smaller")
	}
	if !bytes.HasPrefix(b, []byte("PK")) {
		return nil, bad("Upload the Excel file (.xlsx) made from Download template")
	}
	x, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		return nil, bad("That Excel file could not be read")
	}
	defer x.Close()
	names := x.GetSheetList()
	if i := slices.Index(names, openSheet); i > 0 {
		names = append([]string{openSheet}, append(names[:i:i], names[i+1:]...)...)
	}
	keys := map[string]string{} // heading → key
	for _, k := range openColumns {
		keys[strings.ToLower(k)] = k
	}
	for _, n := range names {
		rows, err := x.GetRows(n, excelize.Options{RawCellValue: true})
		if err != nil || len(rows) == 0 {
			continue
		}
		col := map[string]int{}
		for i, h := range rows[0] {
			if k, ok := keys[strings.ToLower(strings.TrimSpace(h))]; ok {
				col[k] = i
			}
		}
		if _, ok := col["Employee ID"]; !ok {
			continue
		}
		if _, ok := col["Balance still to recover"]; !ok {
			continue
		}
		out := []openLine{}
		for i, rec := range rows[1:] {
			if strings.TrimSpace(strings.Join(rec, "")) == "" {
				continue
			}
			if len(out) == openMaxRows {
				return nil, bad("Up to %d rows at a time. Split the file and upload the rest after", openMaxRows)
			}
			ln := openLine{Line: i + 2, cells: map[string]string{}}
			for k, ci := range col {
				if ci < len(rec) {
					ln.cells[k] = strings.TrimSpace(rec[ci])
				}
			}
			out = append(out, ln)
		}
		return out, nil
	}
	return nil, bad("No sheet with the columns Employee ID and Balance still to recover. Download the template again")
}

// openKey is the duplicate key of a row: the employee and the reference.
func openKey(emp, ref string) string {
	return strings.ToUpper(emp) + "|" + strings.ToLower(strings.Join(strings.Fields(ref), " "))
}

// openParse reads a row's cells into the line and lists what is wrong with them on their own.
func openParse(ln *openLine, first string) []string {
	er := []string{}
	ln.EmployeeID = strings.ToUpper(ln.cells["Employee ID"])
	ln.Reference = strings.Join(strings.Fields(ln.cells["Reference / reason"]), " ")
	if ln.EmployeeID == "" {
		er = append(er, "No Employee ID")
	}
	if d, ok := openDate(ln.cells["Advance date"]); !ok {
		er = append(er, "Advance date must be a date, as YYYY-MM-DD")
	} else if d > today() {
		er = append(er, "Advance date is in the future")
	} else {
		ln.AdvanceDate = d
	}
	var ok bool
	if ln.Original, ok = openAmount(ln.cells["Original amount"]); !ok || ln.Original <= 0 {
		er = append(er, "Original amount must be a number above 0")
	}
	bal, okB := openAmount(ln.cells["Balance still to recover"])
	ln.Balance = bal
	switch {
	case !okB:
		er = append(er, "Balance still to recover must be a number")
	case bal <= 0:
		er = append(er, "Balance still to recover must be above 0: nothing is left to recover")
	case ok && ln.Original > 0 && bal > ln.Original:
		er = append(er, "Balance is more than the original amount")
	}
	mon, okM := openAmount(ln.cells["Monthly recovery"])
	ln.Monthly = mon
	if !okM || mon <= 0 {
		er = append(er, "Monthly recovery must be a number above 0")
	}
	if ym, ok := openMonth(ln.cells["Recovery starts"]); !ok {
		er = append(er, "Recovery starts must be a month, as YYYY-MM")
	} else if ln.StartYm = ym; ym < first {
		er = append(er, fmt.Sprintf("Recovery starts before the open payroll month: start in %s or later", ymLabel(first)))
	}
	if len([]rune(ln.Reference)) < 3 {
		er = append(er, "Give a reference or reason (for example the old voucher number)")
	} else if len([]rune(ln.Reference)) > 200 {
		er = append(er, "Reference / reason is longer than 200 characters")
	}
	if okB && okM && bal > 0 && mon > 0 {
		ln.Months = recoveryMonths(bal, mon)
		if ln.Months > openMaxMonths {
			er = append(er, fmt.Sprintf("Recovery would take %d months; the most is %d. Raise the monthly recovery", ln.Months, openMaxMonths))
		} else {
			ln.Last = calc.Round2(bal - mon*float64(ln.Months-1))
		}
	}
	return er
}

// openJudgeDB checks a row against what is stored: the employee (known, in the caller's scope, active, permanent)
// and an opening advance already loaded for the same employee and reference.
func openJudgeDB(ctx context.Context, q store.Q, c *Ctx, ln *openLine) []string {
	if ln.EmployeeID == "" {
		return nil
	}
	e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": ln.EmployeeID}, options.FindOne().SetProjection(bson.M{"name": 1, "type": 1, "status": 1}))
	if e == nil {
		return []string{"Unknown Employee ID"}
	}
	if ok, _ := c.InScope(ctx, q, ln.EmployeeID); !ok {
		return []string{"Not in your data access"}
	}
	ln.Name = str(e["name"])
	er := []string{}
	if str(e["status"]) != "Active" {
		er = append(er, "Employee is not active")
	} else if str(e["type"]) != "Permanent" {
		er = append(er, "Salary advances are recovered from permanent staff only ("+str(e["type"])+")")
	}
	if ln.Reference != "" {
		prev, _ := q.C("approval_request").Find(ctx, bson.M{"employee_id": ln.EmployeeID, "wf": "p_adv", "payload.opening": true, "status": bson.M{"$ne": "Cancelled"}},
			options.Find().SetProjection(bson.M{"payload": 1}))
		for _, p := range prev {
			pl, _ := p["payload"].(map[string]any)
			if openKey(ln.EmployeeID, str(pl["reference"])) == openKey(ln.EmployeeID, ln.Reference) {
				er = append(er, "Already loaded as "+str(p["id"])+" (same employee and reference)")
				break
			}
		}
	}
	return er
}

// openJudge decides what happens to every row: add, or skip with the reasons. seen catches a row listed twice.
func openJudge(ctx context.Context, q store.Q, c *Ctx, lines []openLine) {
	first := payLineMonth(ctx, q, "deduction", openYm(ctx, q))
	seen := map[string]int{}
	for i := range lines {
		ln := &lines[i]
		er := openParse(ln, first)
		er = append(er, openJudgeDB(ctx, q, c, ln)...)
		if ln.EmployeeID != "" && len([]rune(ln.Reference)) >= 3 {
			k := openKey(ln.EmployeeID, ln.Reference)
			if at, ok := seen[k]; ok {
				er = append(er, fmt.Sprintf("Same employee and reference as row %d: only the first counts", at))
			} else {
				seen[k] = ln.Line
			}
		}
		ln.Outcome, ln.Reason = "add", ""
		if len(er) > 0 {
			ln.Outcome, ln.Reason = "skip", strings.Join(er, "; ")
		}
	}
}

func openCounts(lines []openLine) map[string]any {
	n := map[string]int{}
	bal := 0.0
	for _, l := range lines {
		n[l.Outcome]++
		if l.Outcome == "add" || l.Outcome == "added" {
			bal += l.Balance
		}
	}
	return map[string]any{"total": len(lines), "add": n["add"], "skip": n["skip"], "added": n["added"], "skipped": n["skipped"], "failed": n["failed"],
		"balance": calc.Round2(bal)}
}

// openingPreview is POST /api/advances/opening/preview: what the file would do. Nothing changes.
func (s *Server) openingPreview(r *http.Request, c *Ctx) (any, error) {
	if err := openingAllowed(c); err != nil {
		return nil, err
	}
	lines, err := openReadFile(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	openJudge(ctx, s.st.DB, c, lines)
	out := openCounts(lines)
	out["rows"] = lines
	out["firstYm"] = payLineMonth(ctx, s.st.DB, "deduction", openYm(ctx, s.st.DB))
	return out, nil
}

// openingApply is POST /api/advances/opening/apply with the same file: every row is judged again, and each good
// row is added in a transaction of its own (the employee locked, the checks repeated inside), so one that fails
// does not stop the rest. The run is audited with its counts and the advances' ids.
func (s *Server) openingApply(r *http.Request, c *Ctx) (any, error) {
	if err := openingAllowed(c); err != nil {
		return nil, err
	}
	lines, err := openReadFile(r)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, bad("The file has no rows")
	}
	ctx := r.Context()
	openJudge(ctx, s.st.DB, c, lines)
	ids := []string{}
	for i := range lines {
		ln := &lines[i]
		if ln.Outcome != "add" {
			ln.Outcome = "skipped"
			continue
		}
		var id string
		err := s.st.Tx(ctx, func(tx store.Q) error {
			id = ""
			if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": ln.EmployeeID}); err != nil {
				return err
			}
			first := payLineMonth(ctx, tx, "deduction", openYm(ctx, tx))
			if er := append(openParse(ln, first), openJudgeDB(ctx, tx, c, ln)...); len(er) > 0 {
				return badD(er[0], er)
			}
			var err error
			id, err = s.addOpeningAdvance(ctx, tx, c, ln)
			return err
		})
		if err != nil {
			ln.Outcome, ln.Reason = "failed", strings.Join(bulkErrList(err), "; ")
			continue
		}
		ln.Outcome, ln.ID = "added", id
		ids = append(ids, id)
	}
	out := openCounts(lines)
	name := ""
	if r.MultipartForm != nil && len(r.MultipartForm.File["file"]) > 0 {
		name = r.MultipartForm.File["file"][0].Filename
	}
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Opening advances uploaded: %d added, %d skipped, %d failed", out["added"], out["skipped"], out["failed"]),
		"advance_opening", "", nil, map[string]any{"file": name, "rows": len(lines), "added": out["added"], "skipped": out["skipped"], "failed": out["failed"], "ids": ids})
	out["rows"] = lines
	return out, nil
}

// addOpeningAdvance stores one opening advance: an approved p_adv request marked opening (no approval chain), its
// history, and the recovery lines for the balance (the monthly amount from the start month, the last line the
// remainder). There is no "Salary advance" earning line: it was paid before go-live.
func (s *Server) addOpeningAdvance(ctx context.Context, tx store.Q, c *Ctx, ln *openLine) (string, error) {
	n, err := store.NextSeq(ctx, tx, "approval_request_seq")
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s-%d", prefixes["p_adv"], n)
	flow := approval.Operations
	if subj, err := loadSubject(ctx, tx, ln.EmployeeID); err == nil && subj.Corporate {
		flow = approval.Corporate
	}
	payload := map[string]any{"amount": ln.Original, "instalments": ln.Months, "startYm": ln.StartYm, "note": ln.Reference,
		"opening": true, "reference": ln.Reference, "advanceDate": ln.AdvanceDate, "balance": ln.Balance, "monthly": ln.Monthly}
	given := "on " + longDate(ln.AdvanceDate)
	if p := ln.plan; p != nil {
		// Bulk update › Opening advances: the advance date is its month, and the months before the HRMS are kept.
		given = "in " + ymLabel(ln.AdvanceDate)
		payload["deductionStartYm"], payload["closingYm"], payload["recoveredBefore"], payload["totalInstalments"] = p.Start, addYm(p.Start, p.N-1), p.Recovered, p.N
	}
	if _, err := tx.C("approval_request").Insert(ctx, bson.M{"_id": id, "wf": "p_adv", "employee_id": ln.EmployeeID, "raised_by": c.EmployeeID,
		"flow": string(flow), "facts": approval.Facts{WF: "p_adv", Amt: ln.Original}, "payload": payload, "chain": []string{}, "status": "Approved"}); err != nil {
		return "", err
	}
	if err := history(ctx, tx, id, "", "Loaded", c.Name, fmt.Sprintf("Opening balance loaded from Excel: advance given %s before the HRMS went live, %s still to recover in %d instalment%s from %s.",
		given, calc.Inr(ln.Balance), ln.Months, map[bool]string{true: "", false: "s"}[ln.Months == 1], ymLabel(ln.StartYm))); err != nil {
		return "", err
	}
	for i := 0; i < ln.Months; i++ {
		amt := ln.Monthly
		if i == ln.Months-1 {
			amt = ln.Last
		}
		if err := addLedger(ctx, tx, ln.EmployeeID, addYm(ln.StartYm, i), "ded", fmt.Sprintf("Advance recovery (%d of %d)", i+1, ln.Months), amt, id, ln.Reference); err != nil {
			return "", err
		}
	}
	return id, nil
}

// recoveryMonths is how many monthly recoveries of mon clear bal, the last one taking what is left: worked in paise,
// so 60,001 at 1,000 is 61 months, not 60 with a larger last one (payroll test ITM-106).
func recoveryMonths(bal, mon float64) int {
	b, m := int64(math.Round(bal*100)), int64(math.Round(mon*100))
	if m <= 0 || b <= 0 {
		return 1
	}
	return int((b + m - 1) / m)
}

// ---------- The approver sets the instalments when approving ----------

// advanceSetInstalments is the approver's choice of instalments on an advance waiting on them, written before the
// approval (POST /api/approvals/{id}/approve {note, instalments}): 1 to the company's most, recorded in the request's
// history with who set it, and audited. Nothing is written when it is the number already on the request.
func advanceSetInstalments(ctx context.Context, tx store.Q, c *Ctx, id string, n int) error {
	req, err := loadRequest(ctx, tx, id, true)
	if store.Transient(err) {
		return err
	}
	if err != nil {
		return notFound("Request not found")
	}
	if req.WF != "p_adv" {
		return bad("Instalments are set only on a salary advance")
	}
	if req.Status != "Pending" || req.Step >= len(req.Chain) {
		return bad("Only an advance waiting for approval can be changed")
	}
	if !canAct(ctx, tx, c, req) {
		return forbidden("This advance is not waiting on you")
	}
	var raw map[string]any
	if err := json.Unmarshal(req.Payload, &raw); err != nil || raw == nil {
		raw = map[string]any{}
	}
	if raw["opening"] == true {
		return bad("An opening advance keeps the recovery it was loaded with")
	}
	if mx := loadAdvanceRules(ctx, tx).MaxInstalments; n < 1 || n > mx {
		return bad("Recover the advance in 1 to %d monthly instalments", mx)
	}
	p := payloadOf(req)
	n0 := max(1, p.Instalments)
	if n == n0 {
		return nil
	}
	raw["instalments"] = n
	raw["instalmentsBy"] = c.Name
	if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"payload": raw}}); err != nil {
		return err
	}
	stage := req.Chain[req.Step]
	start := p.StartYm
	if start == "" {
		start = nextYm(openYm(ctx, tx))
	}
	msg := fmt.Sprintf("Instalments set by %s (%s) when approving: %d monthly instalment%s from %s (was %d).", c.Name, stage, n, map[bool]string{true: "", false: "s"}[n == 1], ymLabel(start), n0)
	if err := history(ctx, tx, id, "", "Changed", c.Name, msg); err != nil {
		return err
	}
	return audit(ctx, tx, c, "Advance instalments set", "approval_request", id, map[string]any{"instalments": n0}, map[string]any{"instalments": n, "stage": stage})
}
