package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Full and final (spec 18.2, prototype fnfCompute). The final month's salary is paid in the
// normal run for the LWD month; the settlement adds leave encashment, gratuity, notice pay or
// recovery, and brings forward recoveries and earnings scheduled after the last working day.

type fnfLine struct {
	Key    string   `json:"key"`
	Kind   string   `json:"kind"` // earn | ded | info
	Label  string   `json:"label"`
	Amount float64  `json:"amount"`
	Note   string   `json:"note,omitempty"`
	Src    []int64  `json:"src,omitempty"`   // ledger lines brought forward (voided on approval)
	Carry  []string `json:"carry,omitempty"` // carry-overs settled on approval
}

var reFnfRecovery = regexp.MustCompile(`^(ADV|DED|UNI|AST)-`)

func (s *Server) fnfCompute(ctx context.Context, q store.Q, id string) (*emp, []fnfLine, error) {
	e, err := loadEmp(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	if e.LWD == nil {
		return nil, nil, bad("%s has no last working day", e.Name)
	}
	lwd, fm := *e.LWD, (*e.LWD)[:7]
	gross, _ := grossFor(ctx, q, id, fm)
	_, st := structureFor(ctx, q, id, lwd)
	basic := math.Round(calc.Calc(calc.Input{Type: calc.EmploymentType(e.Type), Gross: gross, Structure: &st, AllBasic: payFor(ctx, q, id, e.Type).allBasicIn(fm)}).Basic)
	L := []fnfLine{}
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	pol, _ := ts.policyFor(e)
	// Leave encashment when the leave plan's Privilege leave is set to "Encashment on exit" (the per-type setting
	// on the Leave plans page; the older plan-level switch, never shown, also counts) and the employment type
	// encashes PL: basic / 26 x balance at LWD (floored to half a day).
	if pl := ts.plan(pol); pl != nil && calc.RulesFor(calc.EmploymentType(e.Type)).PlEnc && leaveTypeOK(e.Type, "Privilege leave") {
		if cfg := ts.typeCfg(pol, "Privilege leave"); cfg != nil && cfg.On && (cfg.Encash || pl.LV.PlEnc) {
			ap, pe, err := takenOf(ctx, q, id, "Privilege leave")
			if err == nil {
				b := leave.Compute(*cfg, pl.LV, pol.S.ProbMonths(e.Grade), e.DOJ, lwd, openingOf(ctx, q, id, "Privilege leave", lwd), ap, pe)
				if days, amt := calc.LeaveEncashment(basic, b.Bal); amt > 0 {
					L = append(L, fnfLine{Key: "enc", Kind: "earn", Label: fmt.Sprintf("Leave encashment, %v PL days", days), Amount: amt, Note: fmt.Sprintf("Basic %s ÷ 26 × %v", calc.Inr(basic), days)})
				}
			}
		}
	}
	// The exit reason, and the latest separation case that was not cancelled (type, notice option and days).
	var reason, sepType, noticeOpt string
	var noticeDays int
	if x, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(compFields("exit_reason"))); x != nil {
		reason = str(x["exit_reason"])
		sep, _ := q.C("separation").FindOne(ctx, bson.M{"employee_id": id, "status": bson.M{"$ne": "Cancelled"}},
			options.FindOne().SetSort(compOrder("-started_at", "-_id")).SetProjection(compFields("type", "notice_option", "notice_days")))
		sepType, noticeOpt = str(sep["type"]), str(sep["notice_option"])
		if n, ok := sep["notice_days"].(int32); ok {
			noticeDays = int(n)
		}
	}
	death := strings.Contains(strings.ToLower(reason), "death") || strings.Contains(strings.ToLower(reason), "disab")
	if yrs, amt, ok := calc.Gratuity(basic, e.DOJ, lwd, death, e.Type == "Permanent"); ok {
		L = append(L, fnfLine{Key: "grat", Kind: "earn", Label: fmt.Sprintf("Gratuity, %d year%s", yrs, map[bool]string{true: "", false: "s"}[yrs == 1]), Amount: amt,
			Note: fmt.Sprintf("Basic %s × 15 ÷ 26 × %d", calc.Inr(basic), yrs)})
	}
	// Notice: shortfall already deducted (information); pay in lieu (termination); notice not served (absconding).
	rows, err := q.C("ledger").Find(ctx, bson.M{"employee_id": id, "void": false, "source_id": "NTC-" + id}, options.Find().SetSort(compOrder("_id")))
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		L = append(L, fnfLine{Key: "ntc", Kind: "info", Label: r["label"].(string), Amount: r["amount"].(float64), Note: "Already deducted in " + ymLabel(r["ym"].(string)) + " pay"})
	}
	if sepType == "Termination" && noticeOpt == "pay" && noticeDays > 0 {
		L = append(L, fnfLine{Key: "nip", Kind: "earn", Label: fmt.Sprintf("Notice pay in lieu, %d days", noticeDays), Amount: calc.NoticePay(gross, noticeDays), Note: fmt.Sprintf("Monthly gross %s ÷ 30 × %d", calc.Inr(math.Round(gross)), noticeDays)})
	}
	if sepType == "Absconding" && pol.S.Ex.Recover && noticeDays > 0 {
		L = append(L, fnfLine{Key: "nns", Kind: "ded", Label: fmt.Sprintf("Notice not served, %d days", noticeDays), Amount: calc.NoticePay(gross, noticeDays), Note: fmt.Sprintf("Monthly gross %s ÷ 30 × %d", calc.Inr(math.Round(gross)), noticeDays)})
	}
	// Months already paid to the employee: an approved or published run with a row for them. Their pay lines
	// were in that row (and any shortfall became a carry-over), so they are not brought forward again (EXT-57);
	// salary paid in them for days after the LWD (a back-dated exit) is recovered (EXT-48).
	paid, err := fnfPaidRows(ctx, q, id, fm)
	if err != nil {
		return nil, nil, err
	}
	if len(paid) > 0 {
		cutoff := cutoffSetting(ctx, q)
		for _, o := range paid {
			if days, amt := fnfExcessPaid(o, lwd, cutoff); amt > 0 {
				L = append(L, fnfLine{Key: "excess", Kind: "ded", Label: fmt.Sprintf("Salary paid after the last working day, %s", ymLabel(o.ym)), Amount: amt,
					Note: fmt.Sprintf("%v of %v paid days fall after %s: earned %s ÷ %v × %v", days, o.Paid, longDate(lwd), calc.Inr(o.Earned), o.Paid, days)})
			}
		}
	}
	// Recoveries scheduled after the LWD month are brought forward; so are approved earnings.
	led, err := q.C("ledger").Find(ctx, bson.M{"employee_id": id, "void": false, "ym": bson.M{"$gt": fm}}, options.Find().SetSort(compOrder("ym", "_id")))
	if err != nil {
		return nil, nil, err
	}
	{
		keep := led[:0]
		for _, l := range led {
			if _, done := paid[str(l["ym"])]; !done {
				keep = append(keep, l)
			}
		}
		led = keep
	}
	rec := fnfLine{Key: "rec", Kind: "ded"}
	fut := fnfLine{Key: "earn2", Kind: "earn"}
	labels := []string{}
	for _, l := range led {
		src := str(l["source_id"])
		if l["kind"] == "ded" && reFnfRecovery.MatchString(src) {
			rec.Amount += l["amount"].(float64)
			rec.Src = append(rec.Src, l["id"].(int64))
			if lb := strings.SplitN(l["label"].(string), " (", 2)[0]; !containsStr(labels, lb) {
				labels = append(labels, lb)
			}
		}
		// A uniform refund due after the exit is not paid (uniform.go: policy, the recovery stands): an info line below.
		if l["kind"] == "earn" && !strings.HasPrefix(src, "NTC") && !strings.HasPrefix(src, "FNF") && !strings.HasPrefix(src, "UNI-") {
			fut.Amount += l["amount"].(float64)
			fut.Src = append(fut.Src, l["id"].(int64))
		}
	}
	uni, err := fnfUniformRefund(ctx, q, id, lwd)
	if err != nil {
		return nil, nil, err
	}
	if len(rec.Src) > 0 {
		rec.Label = "Outstanding recoveries (" + strings.Join(labels, ", ") + ")"
		rec.Note = fmt.Sprintf("%d scheduled instalments brought forward", len(rec.Src))
		L = append(L, rec)
	}
	if h, _ := q.C("salary_hold").FindOne(ctx, bson.M{"employee_id": id, "status": "Active"}); h != nil {
		amt, _ := heldAmount(ctx, q, id, h["from_ym"].(string))
		L = append(L, fnfLine{Key: "hold", Kind: "info", Label: "Salary on hold (" + h["reason"].(string) + ")", Amount: amt, Note: "Not part of this settlement. Release the hold in Salary holds to pay it"})
	}
	if cs, _ := q.C("carry_over").Find(ctx, bson.M{"employee_id": id, "settled": false}, options.Find().SetSort(compOrder("_id"))); len(cs) > 0 {
		cl := fnfLine{Key: "carry", Kind: "ded", Label: "Recoveries carried over from earlier months"}
		ms := []string{}
		for _, x := range cs {
			cl.Amount += x["amount"].(float64)
			cl.Carry = append(cl.Carry, x["key"].(string))
			ms = append(ms, ymLabel(x["ym"].(string)))
		}
		cl.Note = strings.Join(ms, ", ")
		L = append(L, cl)
	}
	if len(fut.Src) > 0 {
		fut.Label = "Approved items scheduled after exit (claims, arrears)"
		fut.Note = fmt.Sprintf("%d lines brought forward", len(fut.Src))
		L = append(L, fut)
	}
	if uni != nil {
		L = append(L, *uni)
	}
	// Variable pay of a designed structure: the amount HR enters for the current cycle (paycomp.go).
	L = append(L, fnfVariableLines(ctx, q, e)...)
	for i := range L {
		L[i].Amount = calc.Round2(L[i].Amount)
	}
	return e, L, nil
}

// fnfPayMonth is the payroll month that pays the settlement: the latest that pays within 45 days of the LWD by
// the pay schedule's pay day, never before the open month (spec 18.2).
func fnfPayMonth(ctx context.Context, q store.Q, lwd string) string {
	var payDay string
	if cs, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"pay_day": 1})); cs != nil {
		payDay = str(cs["pay_day"])
	}
	return calc.FnfPayMonth(lwd, openYm(ctx, q), payDay)
}

// fnfPaidRow is a stored payroll row of a month already approved or published.
type fnfPaidRow struct {
	ym string
	PayRowOut
}

// fnfPaidRows are the employee's rows in approved or published months from the LWD month on, by month.
func fnfPaidRows(ctx context.Context, q store.Q, id, fromYm string) (map[string]*fnfPaidRow, error) {
	yms, err := q.C("payroll_month").Distinct(ctx, "ym", bson.M{"ym": bson.M{"$gte": fromYm}, "status": bson.M{"$in": bson.A{"Approved", "Published"}}})
	if err != nil || len(yms) == 0 {
		return nil, err
	}
	rows, err := q.C("payroll_row").Find(ctx, bson.M{"employee_id": id, "ym": bson.M{"$in": yms}}, options.Find().SetSort(compOrder("ym")))
	if err != nil {
		return nil, err
	}
	out := map[string]*fnfPaidRow{}
	for _, r := range rows {
		o := &fnfPaidRow{ym: str(r["ym"])}
		if err := payengineJSON(r["data"], &o.PayRowOut); err != nil {
			return nil, err
		}
		out[o.ym] = o
	}
	return out, nil
}

// fnfExcessPaid is the salary a paid row gave for days after the last working day: the days of its pay window
// after the LWD that the row counted as employed (up to the LWD it was run with), on the row's pay-days basis
// and at most its paid days, priced at its earned pay per paid day.
func fnfExcessPaid(o *fnfPaidRow, lwd, cutoff string) (days, amount float64) {
	from, to := window(o.ym, cutoff)
	start, end := maxStr(from, policy.AddDays(lwd, 1)), to
	if o.LWD != "" && o.LWD < end {
		end = o.LWD // days after the LWD the row was run with were already left out
	}
	if end < start || o.Paid <= 0 || o.Days <= 0 {
		return 0, 0
	}
	cal := float64(policy.DayDiff(start, end) + 1)
	days = calc.Round2(math.Min(o.Paid, calc.OutsideOnBasis(o.Days, cal, policy.DayDiff(from, to)+1)))
	return days, calc.Round2(o.Earned / o.Paid * days)
}

func maxStr(a, b string) string {
	if a > b {
		return a
	}
	return b
}

func fnfNet(L []fnfLine) float64 {
	n := 0.0
	for _, l := range L {
		switch l.Kind {
		case "earn":
			n += l.Amount
		case "ded":
			n -= l.Amount
		}
	}
	return math.Round(n)
}

// fnfList: everyone on notice or gone, with their settlement state (Payroll, Full and final).
func (s *Server) fnfList(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.claims") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	rows, err := fnfLeavers(ctx, s.st.DB, c)
	if err != nil {
		return nil, err
	}
	s.fnfEstimates(ctx, rows) // estNet for rows not yet prepared
	return map[string]any{"rows": rows, "openYm": openYm(ctx, s.st.DB), "canPrepare": c.Level("pay.claims") >= 2 || c.Level("pay.run") >= 2}, nil
}

// fnfLeavers are the people in scope on notice or gone (with a last working day), latest LWD first (300 at most):
// their location, latest settlement that was not cancelled (shown as Rejected when its request was rejected) and
// the assets they still hold.
func fnfLeavers(ctx context.Context, q store.Q, c *Ctx) ([]store.Row, error) {
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"$and": bson.A{scope, bson.M{"lwd": bson.M{"$ne": nil},
		"$or": bson.A{bson.M{"exit_status": "On notice"}, bson.M{"status": "Inactive"}}}}},
		options.Find().SetSort(compOrder("-lwd", "_id")).SetLimit(300))
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(emps))
	locIDs := make([]any, len(emps))
	for i, e := range emps {
		ids[i], locIDs[i] = str(e["id"]), e["location_id"]
	}
	locs, err := q.C("location").Find(ctx, bson.M{"_id": bson.M{"$in": locIDs}}, options.Find().SetProjection(compFields("id", "name")))
	if err != nil {
		return nil, err
	}
	locName := compRowsByID(locs)
	fnfs, err := q.C("fnf").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}, "status": bson.M{"$ne": "Cancelled"}},
		options.Find().SetSort(compOrder("employee_id", "-at", "-_id")).SetProjection(compFields("id", "employee_id", "status", "net", "pay_ym", "receivable")))
	if err != nil {
		return nil, err
	}
	latest := map[string]store.Row{}
	reqIDs := []string{}
	for _, f := range fnfs {
		if _, seen := latest[str(f["employee_id"])]; !seen {
			latest[str(f["employee_id"])] = f
			reqIDs = append(reqIDs, str(f["id"]))
		}
	}
	rejected, err := q.C("approval_request").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": reqIDs}, "status": "Rejected"})
	if err != nil {
		return nil, err
	}
	isRejected := map[string]bool{}
	for _, id := range rejected {
		isRejected[str(id)] = true
	}
	assets, err := q.C("asset").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"employee_id": bson.M{"$in": ids}, "status": "Issued"}},
		bson.M{"$group": bson.M{"_id": "$employee_id", "n": bson.M{"$sum": int64(1)}}},
	})
	if err != nil {
		return nil, err
	}
	held := map[string]int64{}
	for _, a := range assets {
		held[str(a["id"])], _ = a["n"].(int64)
	}
	rows := make([]store.Row, 0, len(emps))
	for _, e := range emps {
		l := locName[str(e["location_id"])]
		if l == nil { // an employee without a location is left out
			continue
		}
		id := str(e["id"])
		f := latest[id] // the settlement columns are null when none was prepared
		x := store.Row{"id": e["id"], "name": e["name"], "type": e["type"], "status": e["status"], "exit_status": e["exit_status"], "lwd": e["lwd"],
			"exit_reason": e["exit_reason"], "location": l["name"], "fnf_id": f["id"], "fnf_status": f["status"], "net": f["net"], "pay_ym": f["pay_ym"], "receivable": f["receivable"],
			"assets": held[id]}
		if x["fnf_status"] == "Pending approval" && isRejected[str(f["id"])] {
			x["fnf_status"] = "Rejected"
		}
		rows = append(rows, x)
	}
	return rows, nil
}

// fnfAssetsHeld counts the assets still issued to an employee.
func fnfAssetsHeld(ctx context.Context, q store.Q, id string) int {
	n, _ := q.C("asset").Count(ctx, bson.M{"employee_id": id, "status": "Issued"})
	return int(n)
}

func (s *Server) fnfPreview(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.claims") < 1 {
		return nil, forbidden("")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	e, L, err := s.fnfCompute(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	assets := fnfAssetsHeld(ctx, s.st.DB, id)
	return map[string]any{"employee": map[string]any{"id": e.ID, "name": e.Name, "doj": e.DOJ, "lwd": *e.LWD, "type": e.Type}, "lines": L, "net": fnfNet(L),
		"payYm": fnfPayMonth(ctx, s.st.DB, *e.LWD), "assetsPending": assets, "assets": issuedAssets(ctx, s.st.DB, id)}, nil
}

// fnfPrepare sends the settlement for approval (p_fnf, by net amount).
func (s *Server) fnfPrepare(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.claims") < 2 && c.Level("pay.run") < 2 {
		return nil, forbidden("Payroll prepares the full and final")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		out = nil // the transaction may run again
		e, L, err := s.fnfCompute(ctx, tx, id)
		if err != nil {
			return err
		}
		if e.LWD == nil || (e.Exit == nil && e.Status != "Inactive") {
			return bad("%s is not leaving", e.Name)
		}
		if assets := fnfAssetsHeld(ctx, tx, id); assets > 0 {
			return bad("Clearance pending: %d asset%s not returned", assets, map[bool]string{true: "", false: "s"}[assets == 1])
		}
		// When a separation case exists, its clearance must be complete (spec 18.1).
		open, err := tx.C("separation").FindOne(ctx, bson.M{"employee_id": id, "status": "Open"}, options.FindOne().SetProjection(compFields("id")))
		if err != nil {
			return err
		}
		var sep *string // the open case the settlement belongs to
		if open != nil {
			sep = taxText(open["id"])
			if sc, err := loadSep(ctx, tx, *sep); err == nil {
				items, _ := clearanceOf(ctx, tx, sc)
				for _, it := range items {
					if !it.Done {
						return bad("Clearance is not complete: %s (%s)", it.Name, it.Owner)
					}
				}
				out = map[string]any{"case": sc.ID}
			}
		}
		waiting, err := tx.C("fnf").Distinct(ctx, "_id", bson.M{"employee_id": id, "status": "Pending approval"})
		if err != nil {
			return err
		}
		if ok, _ := tx.C("approval_request").Exists(ctx, bson.M{"_id": bson.M{"$in": waiting}, "status": "Pending"}); ok {
			return bad("A full and final is already waiting for approval")
		}
		if ok, _ := tx.C("fnf").Exists(ctx, bson.M{"employee_id": id, "status": "Approved"}); ok {
			return bad("The full and final is already approved")
		}
		net := fnfNet(L)
		pay := fnfPayMonth(ctx, tx, *e.LWD)
		req, err := s.createRequestTx(ctx, tx, c, "p_fnf", id, approval.Facts{Amt: math.Abs(net)}, requestPayload{Amount: net, PayMonth: pay, LWD: *e.LWD, Note: "Full and final settlement"})
		if err != nil {
			return err
		}
		// A settlement still waiting is replaced by this one.
		if _, err := tx.C("fnf").UpdateMany(ctx, bson.M{"employee_id": id, "status": "Pending approval"}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
			return err
		}
		if _, err := tx.C("fnf").Insert(ctx, bson.M{"_id": req["id"], "sep_id": sep, "employee_id": id, "lwd": *e.LWD, "pay_ym": pay, "lines": L, "net": net, "by_name": c.Name}); err != nil {
			return err
		}
		if out == nil {
			out = map[string]any{}
		}
		out["id"], out["net"], out["payYm"] = req["id"], net, pay
		return audit(ctx, tx, c, "Full and final sent for approval", "fnf", req["id"].(string), nil, map[string]any{"net": net})
	})
	return out, err
}

// finalizeFnf writes the settlement to the ledger and voids the lines it brought forward.
func finalizeFnf(ctx context.Context, tx store.Q, r *Request) (string, error) {
	var lines []fnfLine
	f, err := tx.C("fnf").FindOne(ctx, bson.M{"_id": r.ID}, options.FindOne().SetProjection(compFields("lines", "pay_ym", "net")))
	if err != nil {
		return "", err
	}
	if f == nil || compJSONInto(f["lines"], &lines) != nil {
		return "", nil // an older request without a stored settlement
	}
	pay := str(f["pay_ym"])
	pay = maxYm(pay, openYm(ctx, tx))
	// An employee who owes money on settlement (net below zero) is not run through payroll, where the row would
	// only net to zero and carry the rest over for someone who has left (EXT-42): the brought-forward lines
	// and carry-overs are closed into the settlement, nothing is written to pay, and the amount stays
	// recoverable from the employee until Payroll records it recovered or waived.
	net, _ := f["net"].(float64)
	owed := net < 0
	for _, l := range lines {
		if l.Key == "unirefund" { // uniform refunds the leaver does not get: cancelled
			for _, id := range l.Src {
				if _, err := tx.C("ledger").Update(ctx, bson.M{"_id": id, "void": false}, bson.M{"$set": bson.M{"void": true,
					"void_reason": uniRefundVoid + " (full and final " + r.ID + ")", "voided_at": tx.Now()}}); err != nil {
					return "", err
				}
			}
			continue
		}
		if l.Kind == "info" || l.Amount == 0 {
			continue
		}
		for _, id := range l.Src {
			if _, err := tx.C("ledger").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"void": true, "void_reason": "Brought forward to full and final " + r.ID, "voided_at": tx.Now()}}); err != nil {
				return "", err
			}
		}
		for _, k := range l.Carry {
			if _, err := tx.C("carry_over").UpdateMany(ctx, bson.M{"key": k}, bson.M{"$set": bson.M{"settled": true, "settled_in": r.ID}}); err != nil {
				return "", err
			}
		}
		if owed {
			continue
		}
		if err := addLedger(ctx, tx, r.EmployeeID, pay, l.Kind, l.Label, l.Amount, r.ID, "Full and final"); err != nil {
			return "", err
		}
	}
	if owed {
		rec := bson.M{"amount": -net, "status": "Open"}
		if _, err := tx.C("fnf").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"status": "Approved", "pay_ym": pay, "receivable": rec}}); err != nil {
			return "", err
		}
		return fmt.Sprintf("The employee owes %s on settlement: nothing is paid. Record the recovery (or a waiver) in Full and final.", calc.Inr(-net)), nil
	}
	if _, err := tx.C("fnf").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"status": "Approved", "pay_ym": pay}}); err != nil {
		return "", err
	}
	return "Settlement added to " + ymLabel(pay) + " pay lines.", nil
}

// fnfReceivableRec is fnf.receivable: what a leaver owes on an approved settlement that netted below zero.
type fnfReceivableRec struct {
	Amount float64 `json:"amount"`
	Status string  `json:"status"` // Open | Recovered | Waived
	Note   string  `json:"note,omitempty"`
	ByName string  `json:"by_name,omitempty"`
	At     string  `json:"at,omitempty"`
}

// fnfOwed reads a settlement's receivable (nil when it has none).
func fnfOwed(f store.Row) *fnfReceivableRec {
	if f == nil || f["receivable"] == nil {
		return nil
	}
	var r fnfReceivableRec
	if compJSONInto(f["receivable"], &r) != nil {
		return nil
	}
	return &r
}

// fnfOwedBlocks refuses a no dues certificate while the employee still owes money on the settlement.
func fnfOwedBlocks(ctx context.Context, q store.Q, empID string) error {
	fs, err := q.C("fnf").Find(ctx, bson.M{"employee_id": empID, "status": "Approved"})
	if err != nil {
		return err
	}
	for _, f := range fs {
		if r := fnfOwed(f); r != nil && r.Status == "Open" {
			return bad("%s is still owed by the employee on the full and final: record the recovery or waiver first", calc.Inr(r.Amount))
		}
	}
	return nil
}

// fnfReceivable is POST /api/fnf/{id}/receivable {action: recovered | waived, note}: closes the amount an
// employee owes on an approved settlement (EXT-42). Payroll records a recovery (cheque, transfer) with its
// reference; a waiver needs the Payroll Manager level and someone other than the person who prepared the
// settlement. Both are audited; the no dues certificate waits until it is closed.
func (s *Server) fnfReceivable(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	switch {
	case b.Action != "recovered" && b.Action != "waived":
		return nil, bad("Choose recovered or waived")
	case len(b.Note) < 5:
		return nil, bad("Add a note: the payment reference, or why it is waived")
	case b.Action == "recovered" && c.Level("pay.claims") < 2 && c.Level("pay.run") < 2:
		return nil, forbidden("Payroll records a recovery")
	case b.Action == "waived" && c.Level("pay.run") < 2:
		return nil, forbidden("Only the Payroll Manager waives an amount owed")
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		f, err := tx.C("fnf").FindOne(ctx, bson.M{"$or": bson.A{bson.M{"_id": r.PathValue("id")}, bson.M{"employee_id": r.PathValue("id")}}, "status": "Approved"},
			options.FindOne().SetSort(compOrder("-at", "-_id")))
		if err != nil {
			return err
		}
		rec := fnfOwed(f)
		if rec == nil || rec.Status != "Open" {
			return bad("Nothing is owed on an approved full and final")
		}
		fid := str(f["id"])
		if err := c.MustInScope(ctx, tx, str(f["employee_id"])); err != nil {
			return err
		}
		if b.Action == "waived" {
			if req, _ := tx.C("approval_request").FindOne(ctx, bson.M{"_id": fid}); req != nil && str(req["raised_by"]) == c.EmployeeID {
				return forbidden("You prepared this settlement: another Payroll Manager waives it")
			}
		}
		amt := rec.Amount
		status := map[string]string{"recovered": "Recovered", "waived": "Waived"}[b.Action]
		set := bson.M{"receivable": bson.M{"amount": amt, "status": status, "note": b.Note, "by_name": c.Name, "at": today()}}
		if _, err := tx.C("fnf").Update(ctx, bson.M{"_id": fid}, bson.M{"$set": set}); err != nil {
			return err
		}
		out = map[string]any{"id": fid, "status": status, "amount": amt}
		return audit(ctx, tx, c, "Full and final: amount owed "+strings.ToLower(status), "fnf", fid, nil, map[string]any{"amount": amt, "note": b.Note})
	})
	return out, err
}
