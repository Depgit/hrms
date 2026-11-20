package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/store"
)

// Payroll cycle (spec 15.1): the checklist (payroll_closure.go, in place of Month close), pilot, dry run with
// edits, run checks, verify, approve, publish. Rows live in payroll_row; every step is audited.

// payInline is the largest run computed while the request waits; larger runs go to a job.
var payInline = 2000

// SetPayInline changes that limit (tests use it to exercise the background job).
func SetPayInline(n int) { payInline = n }

type payMonth struct {
	YM          string          `json:"ym"`
	Status      string          `json:"status"`
	LockedAt    *time.Time      `json:"lockedAt"`
	LockedBy    *string         `json:"lockedBy"`
	Pilot       []string        `json:"pilot"`
	Reminded    map[string]any  `json:"reminded"`
	ComputedAt  *time.Time      `json:"computedAt"`
	ComputedBy  *string         `json:"computedBy"`
	VerifiedBy  *string         `json:"verifiedBy"`
	ApprovedBy  *string         `json:"approvedBy"`
	ApprovedAt  *time.Time      `json:"approvedAt"`
	PublishedBy *string         `json:"publishedBy"`
	PublishedAt *time.Time      `json:"publishedAt"`
	Totals      json.RawMessage `json:"totals"`
	Job         *string         `json:"job"`
	JobError    *string         `json:"jobError"`
	Stale       bool            `json:"stale"` // pay lines, holds or edits changed after the last calculation (set with the checklist)
}

// loadPayMonth reads a payroll month (an Open month with nothing set when it has no record yet); with lock, it
// is locked for the rest of the transaction (FOR UPDATE).
func loadPayMonth(ctx context.Context, q store.Q, ym string, lock bool) (*payMonth, error) {
	m := &payMonth{YM: ym, Status: "Open", Reminded: map[string]any{}, Totals: json.RawMessage("{}")}
	var r store.Row
	var err error
	if lock {
		r, err = q.C("payroll_month").Lock(ctx, bson.M{"ym": ym})
	} else {
		r, err = q.C("payroll_month").FindOne(ctx, bson.M{"ym": ym})
	}
	if err != nil || r == nil {
		return m, err
	}
	m.Status = str(r["status"])
	m.LockedAt, m.LockedBy = payrollTime(r["locked_at"]), payrollText(r["locked_by"])
	m.Pilot = payengineStrings(r["pilot"])
	if rem, ok := r["reminded"].(map[string]any); ok {
		m.Reminded = rem
	}
	m.ComputedAt, m.ComputedBy, m.VerifiedBy = payrollTime(r["computed_at"]), payrollText(r["computed_by"]), payrollText(r["verified_by"])
	m.ApprovedBy, m.ApprovedAt = payrollText(r["approved_by"]), payrollTime(r["approved_at"])
	m.PublishedBy, m.PublishedAt = payrollText(r["published_by"]), payrollTime(r["published_at"])
	if m.Totals, err = json.Marshal(r["totals"]); err != nil {
		return m, err
	}
	m.Job, m.JobError = payrollText(r["job"]), payrollText(r["job_error"])
	return m, nil
}

// payrollText is a nullable text column as *string.
func payrollText(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// payrollTime is a nullable timestamp column as *time.Time.
func payrollTime(v any) *time.Time {
	if t, ok := v.(time.Time); ok {
		return &t
	}
	return nil
}

// ensurePayMonth creates the month's record when it has none, and locks it for the transaction.
// Only the open month gets a new record: a stray month never becomes the open one (RUN-12).
func ensurePayMonth(ctx context.Context, tx store.Q, ym string) (*payMonth, error) {
	if has, err := tx.C("payroll_month").Exists(ctx, bson.M{"ym": ym}); err != nil {
		return nil, err
	} else if !has {
		if open := openYm(ctx, tx); ym != open {
			return nil, bad("Only the open payroll month (%s) can be run", ymLabel(open))
		}
	}
	if _, err := tx.C("payroll_month").Upsert(ctx, bson.M{"ym": ym}, nil, nil); err != nil {
		return nil, err
	}
	return loadPayMonth(ctx, tx, ym, true)
}

func (s *Server) payYm(r *http.Request, c *Ctx, min int) (string, error) {
	if err := c.Need("pay.run", min); err != nil {
		return "", err
	}
	ym := r.PathValue("ym")
	if !isYm(ym) {
		return "", bad("Month must look like 2026-10")
	}
	return ym, nil
}

// payrollState: the open month, the month waiting to be published, and recent months.
func (s *Server) payrollState(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	open := openYm(ctx, s.st.DB)
	cur, err := loadPayMonth(ctx, s.st.DB, open, false)
	if err != nil {
		return nil, err
	}
	cur.Stale = payStale(ctx, s.st.DB, cur)
	months, err := s.st.DB.C("payroll_month").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "ym", Value: -1}}).SetLimit(24).
		SetProjection(bson.D{{Key: "ym", Value: 1}, {Key: "status", Value: 1}, {Key: "totals", Value: 1}, {Key: "approved_by", Value: 1},
			{Key: "published_by", Value: 1}, {Key: "published_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, m := range months {
		m["published_at"] = payengineStamp(m["published_at"], "02 Jan 2006 15:04")
	}
	var waiting *payMonth
	for _, m := range months {
		if m["status"] == "Approved" {
			waiting, _ = loadPayMonth(ctx, s.st.DB, m["ym"].(string), false)
			break
		}
	}
	return map[string]any{"openYm": open, "open": cur, "waiting": waiting, "months": months, "cutoff": cutoffSetting(ctx, s.st.DB),
		"canRun": c.Level("pay.run") >= 2, "canApprove": c.Level("pay.approve") >= 2, "flags": payFlags}, nil
}

// ---------- Pending requests (the employee file) ----------

type pendingItem struct {
	ID, WF, EmployeeID, Name, Stage, Approver, ApproverID, When string
}

// pendingFor lists leave and regularization requests still pending for the month's window, of the employees in
// pilot (an empty list: everyone's; a nil list matched no one, as the SQL's cardinality(NULL) did).
func (s *Server) pendingFor(ctx context.Context, q store.Q, ym string, pilot []string) ([]pendingItem, error) {
	out := []pendingItem{}
	if pilot == nil {
		return out, nil
	}
	from, to := window(ym, cutoffSetting(ctx, q))
	who := bson.M{"$ne": nil} // requests about an employee
	if len(pilot) > 0 {
		who = bson.M{"$in": pilot}
	}
	rows, err := q.C("approval_request").Find(ctx, bson.M{"status": "Pending", "employee_id": who, "$or": bson.A{
		bson.M{"wf": "regularize", "payload.date": bson.M{"$gte": from, "$lte": to}},
		bson.M{"wf": "leave", "payload.from": bson.M{"$lte": to}, "payload.to": bson.M{"$gte": from}},
	}}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		req, err := loadRequest(ctx, q, row["id"].(string), false)
		if err != nil || req.Step >= len(req.Chain) {
			continue
		}
		sub, err := loadSubject(ctx, q, req.EmployeeID)
		if err != nil {
			continue
		}
		stage := req.Chain[req.Step]
		p := resolvePerson(ctx, q, stage, sub, req.Facts)
		var payload requestPayload
		_ = json.Unmarshal(req.Payload, &payload)
		when := payload.Date
		if req.WF == "leave" {
			when = payload.From + " to " + payload.To
		}
		out = append(out, pendingItem{req.ID, req.WF, req.EmployeeID, sub.Name, stage, p.Name, p.ID, when})
	}
	return out, nil
}

// payrollPilotFilter narrows a run to its pilot (nil when there is none: everyone).
func payrollPilotFilter(pilot []string) any {
	if len(pilot) == 0 {
		return nil
	}
	return bson.M{"_id": bson.M{"$in": pilot}}
}

// payrollAttendance is the attendance summary download for the month.
func (s *Server) payrollAttendance(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return "", nil, nil, err
	}
	pc, list, err := loadPayCtx(ctx, s.st.DB, ym, scope)
	if err != nil {
		return "", nil, nil, err
	}
	kinds := []string{"Present", "Late", "Half day", "Missing punch", "No punch", "Week off", "Holiday", "Paid leave", "Loss of pay leave"}
	cols := append([]string{"Emp ID", "Name", "Type", "Department", "Location"}, kinds...)
	cols = append(cols, "Loss of pay days", "Days outside employment", "Paid days")
	out := [][]any{}
	for _, e := range list {
		p, _ := pc.ts.policyFor(e)
		mo := pc.md.span(e, p, pc.from, pc.to)
		row := []any{e.ID, e.Name, e.Type, e.Dept, e.Location}
		for _, k := range kinds {
			row = append(row, mo.Counts[k])
		}
		days := calc.PayDays(ym, "calendar")
		row = append(row, mo.LOP, mo.Outside, calc.PaidDays(days, mo.LOP, float64(mo.Outside), mo.DaysN, nil))
		out = append(out, row)
	}
	return "attendance-summary-" + ym + ".xlsx", cols, out, nil
}

// ---------- Pilot, dry run, verify, approve, publish ----------

func (s *Server) payrollPilot(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 2)
	if err != nil {
		return nil, err
	}
	var b struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		m, err := ensurePayMonth(ctx, tx, ym)
		if err != nil {
			return err
		}
		if m.Status != "Open" && m.Status != "Dry run" {
			return bad("The pilot is chosen before the run is verified")
		}
		var pilot []string
		for _, id := range b.IDs {
			if id = strings.TrimSpace(strings.ToUpper(id)); id != "" && !containsStr(pilot, id) {
				pilot = append(pilot, id)
			}
		}
		if len(pilot) > 0 {
			n, err := tx.C("employee").Count(ctx, bson.M{"_id": bson.M{"$in": pilot}})
			if err != nil || int(n) != len(pilot) {
				return bad("Some employee IDs in the pilot are not known")
			}
		}
		if _, err := tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"pilot": pilot}}); err != nil {
			return err
		}
		what := "Cleared the payroll pilot"
		if len(pilot) > 0 {
			what = fmt.Sprintf("Set a payroll pilot of %d people", len(pilot))
		}
		return audit(ctx, tx, c, what, "payroll_month", ym, nil, pilot)
	})
}

// PayrollRunArgs computes a large run in the background.
type PayrollRunArgs struct {
	YM     string `json:"ym"`
	Action string `json:"action"` // Dry run | Verified | Approved
	By     string `json:"by"`
}

func (PayrollRunArgs) Kind() string { return "payroll_run" }

type payrollRunWorker struct {
	jobs.WorkerDefaults[PayrollRunArgs]
	s *Server
}

func (w *payrollRunWorker) Work(ctx context.Context, j *jobs.Job[PayrollRunArgs]) error {
	err := w.s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := w.s.computeRows(ctx, tx, j.Args.YM, j.Args.By); err != nil {
			return err
		}
		_, err := tx.C("payroll_month").Update(ctx, bson.M{"ym": j.Args.YM}, bson.M{"$set": bson.M{"status": j.Args.Action, "job": nil, "job_error": nil}})
		return err
	})
	if err != nil {
		_, _ = w.s.st.DB.C("payroll_month").Update(ctx, bson.M{"ym": j.Args.YM}, bson.M{"$set": bson.M{"job": nil, "job_error": err.Error()}})
	}
	return err
}

// computeRows recomputes every row of the month (or the pilot) and stores them.
func (s *Server) computeRows(ctx context.Context, tx store.Q, ym, by string) (int, error) {
	m, err := loadPayMonth(ctx, tx, ym, false)
	if err != nil {
		return 0, err
	}
	// A salary held from the checklist for having no bank account is released first once an account is on record,
	// so a pending company policy can hold the month in its place.
	if err := releaseNoBankHolds(ctx, tx); err != nil {
		return 0, err
	}
	// PF for new joiners: the exemption is decided again for people with no PF history, and a salary held for a missing
	// UAN is released once a UAN is on record (pfuan.go).
	if err := syncPfExemptRun(ctx, tx, ym, m.Pilot); err != nil {
		return 0, err
	}
	if err := releaseUanHolds(ctx, tx); err != nil {
		return 0, err
	}
	// Company policies still pending hold the salary for the month (policy_ack.go).
	if err := syncAckHolds(ctx, tx, ym, m.Pilot); err != nil {
		return 0, err
	}
	// PF applies and no UAN is on record: the salary is held (pfuan.go).
	if err := placeUanHolds(ctx, tx, ym, m.Pilot); err != nil {
		return 0, err
	}
	// Salary held for a bank change since decided is paid in this month; lines of later joiners move to their joining month.
	if err := syncBankHolds(ctx, tx); err != nil {
		return 0, err
	}
	if err := movePreJoinLines(ctx, tx, ym); err != nil {
		return 0, err
	}
	pc, list, err := loadPayCtx(ctx, tx, ym, payrollPilotFilter(m.Pilot))
	if err != nil {
		return 0, err
	}
	if _, err := tx.C("payroll_row").DeleteMany(ctx, bson.M{"ym": ym}); err != nil {
		return 0, err
	}
	now := time.Now()
	docs := make([]any, 0, len(list))
	for _, e := range list {
		o := pc.row(e)
		data, _ := json.Marshal(o)
		docs = append(docs, bson.M{"ym": ym, "employee_id": e.ID, "net": calc.Round2(o.Net), "hold": o.Hold, "flags": o.Flags, "data": data, "computed_at": now})
	}
	if err := tx.C("payroll_row").InsertMany(ctx, docs); err != nil {
		return 0, err
	}
	tot, err := payTotals(ctx, tx, ym)
	if err != nil {
		return 0, err
	}
	_, err = tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"computed_at": tx.Now(), "computed_by": by, "totals": tot}})
	return len(list), err
}

// payTotals sums the month's stored rows: people and held salaries, the net paid and held, gross and earned.
// Money is summed and then rounded to the paisa, as the numeric sums were.
func payTotals(ctx context.Context, q store.Q, ym string) (map[string]any, error) {
	var n, held int
	var net, heldNet, gross, earned float64
	sums, err := q.C("payroll_row").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"ym": ym}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil},
			{Key: "people", Value: bson.M{"$sum": int64(1)}},
			{Key: "held", Value: bson.M{"$sum": bson.M{"$cond": bson.A{"$hold", int64(1), int64(0)}}}},
			{Key: "paid", Value: bson.M{"$sum": bson.M{"$cond": bson.A{"$hold", 0, "$net"}}}},
			{Key: "heldNet", Value: bson.M{"$sum": bson.M{"$cond": bson.A{"$hold", "$net", 0}}}},
			{Key: "gross", Value: bson.M{"$sum": "$data.gross"}},
			{Key: "earned", Value: bson.M{"$sum": "$data.earned"}}}}},
	})
	if err == nil && len(sums) > 0 {
		t := sums[0]
		n, held = int(payengineFloat(t["people"])), int(payengineFloat(t["held"]))
		net, heldNet, gross, earned = payengineFloat(t["paid"]), payengineFloat(t["heldNet"]), payengineFloat(t["gross"]), payengineFloat(t["earned"])
	}
	return map[string]any{"employees": n, "held": held, "net": calc.Round2(net), "heldNet": calc.Round2(heldNet), "gross": calc.Round2(gross), "earned": calc.Round2(earned)}, err
}

// payrollStep moves the month along: dry-run, verify, back, approve, publish.
func (s *Server) payrollStep(r *http.Request, c *Ctx) (any, error) {
	step := r.PathValue("step")
	need := map[string]string{"dry-run": "pay.run", "verify": "pay.run", "back": "pay.run", "approve": "pay.approve", "publish": "pay.approve"}[step]
	if need == "" {
		return nil, notFound("")
	}
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	if err := c.Need(need, 2); err != nil {
		if need == "pay.approve" {
			return nil, forbidden("Approve and publish need the Payroll Manager")
		}
		return nil, err
	}
	ym := r.PathValue("ym")
	if !isYm(ym) {
		return nil, bad("Month must look like 2026-10")
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		m, err := ensurePayMonth(ctx, tx, ym)
		if err != nil {
			return err
		}
		if m.Job != nil {
			return bad("%s is still being calculated", ymLabel(ym))
		}
		want := map[string][]string{"dry-run": {"Open", "Dry run"}, "verify": {"Dry run"}, "back": {"Verified"}, "approve": {"Verified"}, "publish": {"Approved"}}[step]
		if !containsStr(want, m.Status) {
			return bad("%s is %s; this step is not available now", ymLabel(ym), strings.ToLower(m.Status))
		}
		if step != "publish" && ym != openYm(ctx, tx) {
			return bad("Only the open payroll month (%s) can be run", ymLabel(openYm(ctx, tx)))
		}
		next := map[string]string{"dry-run": "Dry run", "verify": "Verified", "back": "Dry run", "approve": "Approved", "publish": "Published"}[step]
		// Salary is processed only once the checklist's five closures are closed (the attendance lock among them).
		if step != "back" {
			if err := closureGate(ctx, tx, ym); err != nil {
				return err
			}
		}
		if (step == "verify" || step == "approve") && len(m.Pilot) > 0 {
			return bad("Clear the pilot and run everyone before verifying")
		}
		// Approval approves the verified rows as they are (never recalculated), by someone who did not prepare them.
		if step == "approve" {
			if err := approverMadeRun(ctx, tx, c, ym); err != nil {
				return err
			}
			if payStale(ctx, tx, m) {
				return bad("Something changed after %s was calculated. Send it back to the dry run, run it again and verify before approving", ymLabel(ym))
			}
		}
		if step == "dry-run" || step == "verify" {
			eligible, err := payEligible(ctx, tx, ym)
			if err != nil {
				return err
			}
			n64, err := tx.C("employee").Count(ctx, eligible)
			if err != nil {
				return err
			}
			n := int(n64)
			if n > payInline && s.jobs != nil {
				if _, err := s.Enqueue(ctx, tx, PayrollRunArgs{YM: ym, Action: next, By: c.Name}); err != nil {
					return err
				}
				if _, err := tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"job": next, "job_error": nil}}); err != nil {
					return err
				}
				out = map[string]any{"queued": true, "employees": n}
				return audit(ctx, tx, c, "Payroll "+step+" started in the background", "payroll_month", ym, nil, n)
			}
			if _, err := s.computeRows(ctx, tx, ym, c.Name); err != nil {
				return err
			}
		}
		// Pay lines of people without a row would never be paid: they are fixed before verifying (REAL-19).
		if step == "verify" {
			if out, err := linesOutsideRun(ctx, tx, ym); err != nil {
				return err
			} else if len(out) > 0 {
				return bad("Pay lines of %s are in %s but they have no row in the run. Move or cancel those lines first", strings.Join(out, ", "), ymLabel(ym))
			}
		}
		if step == "publish" {
			if err := s.publishMonth(ctx, tx, ym); err != nil {
				return err
			}
		}
		// The new status, and who verified, approved or published it and when.
		set := bson.M{"status": next}
		if col := map[string]string{"verify": "verified", "approve": "approved", "publish": "published"}[step]; col != "" {
			set[col+"_by"], set[col+"_at"] = c.Name, tx.Now()
		}
		if _, err := tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": set}); err != nil {
			return err
		}
		// A salary held for a bank change waits on Salary holds, paid once the account is verified (RUN-77).
		if step == "approve" {
			if err := syncBankHolds(ctx, tx); err != nil {
				return err
			}
		}
		tot, _ := payTotals(ctx, tx, ym)
		out = map[string]any{"status": next, "totals": tot}
		return audit(ctx, tx, c, "Payroll "+ymLabel(ym)+": "+strings.ToLower(next), "payroll_month", ym, m.Status, next)
	})
	// A published month's cost figures are frozen for the cost console (cost.go), after the publish has committed.
	if err == nil && step == "publish" {
		s.costFreezeLogged(ctx, ym, c.Name)
	}
	return out, err
}

// publishMonth releases payslips, writes TDS history and carries negative nets forward.
func (s *Server) publishMonth(ctx context.Context, tx store.Q, ym string) error {
	rows, err := loadRows(ctx, tx, ym)
	if err != nil {
		return err
	}
	co, err := companyInfo(ctx, tx)
	if err != nil {
		return err
	}
	for _, o := range rows {
		if calc.RulesFor(calc.EmploymentType(o.Type)).Docs {
			// Published again (after a correction), the payslip is replaced and dated now.
			snap := slipSnap(ctx, tx, ym, o, co)
			if _, err := tx.C("payslip").Upsert(ctx, bson.M{"ym": ym, "employee_id": o.EmployeeID},
				bson.M{"snap": snap, "net": o.Net, "published_at": tx.Now()}, nil); err != nil {
				return err
			}
		}
		if t := o.STds + o.R.Tds; t > 0 {
			sec := "192"
			if o.R.Tds > 0 {
				sec = "194J"
			}
			if _, err := tx.C("tds_history").Upsert(ctx, bson.M{"employee_id": o.EmployeeID, "ym": ym, "section": sec},
				bson.M{"amount": calc.Round2(t)}, nil); err != nil {
				return err
			}
		}
		if o.Carry > 0 {
			key := "CAR-" + ym + "-" + o.EmployeeID
			if ok, _ := tx.C("carry_over").Exists(ctx, bson.M{"key": key}); ok {
				continue
			}
			active := o.Status == "Active" && o.LWD == ""
			if _, err := tx.C("carry_over").Insert(ctx, bson.M{"key": key, "employee_id": o.EmployeeID, "ym": ym, "amount": o.Carry, "settled": active,
				"settled_in": map[bool]any{true: nextYm(ym), false: nil}[active]}); err != nil {
				return err
			}
			if active {
				if _, err := tx.C("ledger").Insert(ctx, bson.M{"employee_id": o.EmployeeID, "ym": nextYm(ym), "kind": "ded", "label": "Carried over from " + ymLabel(ym),
					"amount": o.Carry, "source_id": key, "reason": "Recoveries were more than the pay for the month"}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ---------- Rows: list, detail, edits ----------

func filterRows(c *Ctx, rows []*PayRowOut, q map[string]string) []*PayRowOut {
	out := []*PayRowOut{}
	needle := strings.ToLower(strings.TrimSpace(q["q"]))
	for _, o := range rows {
		if !c.Scope.Allows(o.scoped()) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(o.Name+" "+o.EmployeeID), needle) {
			continue
		}
		if (q["city"] != "" && o.CityID != q["city"]) || (q["location"] != "" && o.LocID != q["location"]) || (q["type"] != "" && o.Type != q["type"]) ||
			(q["dept"] != "" && o.Dept != q["dept"]) || (q["flag"] != "" && !containsStr(o.Flags, q["flag"])) {
			continue
		}
		out = append(out, o)
	}
	return out
}

func queryMap(r *http.Request, keys ...string) map[string]string {
	m := map[string]string{}
	for _, k := range keys {
		m[k] = r.URL.Query().Get(k)
	}
	return m
}

func (s *Server) payrollRows(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return nil, err
	}
	all, err := loadRows(r.Context(), s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	scoped := filterRows(c, all, map[string]string{})
	rows := filterRows(c, all, queryMap(r, "q", "city", "location", "type", "dept", "flag"))
	counts := map[string]int{}
	for _, o := range scoped {
		for _, f := range o.Flags {
			counts[f]++
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if size <= 0 || size > 500 {
		size = 50
	}
	if page < 1 {
		page = 1
	}
	// Sums leave out held salaries; held and heldNet count them, for the rows in view.
	var sum struct{ Net, Earned, Ded, Gross, Held, HeldNet float64 }
	for _, o := range rows {
		if o.Hold {
			sum.Held++
			sum.HeldNet += o.Net
			continue
		}
		sum.Net += o.Net
		sum.Earned += o.TotalEarn
		sum.Ded += o.TotalDed
		sum.Gross += o.Gross
	}
	total := len(rows)
	lo, hi := min((page-1)*size, total), min(page*size, total)
	return map[string]any{"rows": rows[lo:hi], "total": total, "page": page, "size": size, "counts": counts, "scoped": len(scoped),
		"sum": map[string]float64{"net": calc.Round2(sum.Net), "earned": calc.Round2(sum.Earned), "ded": calc.Round2(sum.Ded), "gross": calc.Round2(sum.Gross),
			"held": sum.Held, "heldNet": calc.Round2(sum.HeldNet)}}, nil
}

// payrollPreview computes one person's row live (used by the employee file and before a dry run).
func (s *Server) previewRow(ctx context.Context, q store.Q, ym, id string) (*PayRowOut, error) {
	pc, list, err := loadPayCtx(ctx, q, ym, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return pc.row(list[0]), nil
}

func (s *Server) payrollRow(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("emp")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	stored, err := s.st.DB.C("payroll_row").FindOne(ctx, bson.M{"ym": ym, "employee_id": id}, options.FindOne().SetProjection(bson.D{{Key: "data", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var row *PayRowOut
	if stored == nil {
		if row, err = s.previewRow(ctx, s.st.DB, ym, id); err != nil {
			return nil, err
		}
		if row == nil {
			return nil, notFound("Not in this payroll month")
		}
	} else {
		row = &PayRowOut{}
		_ = payengineJSON(stored["data"], row)
	}
	ov, _ := s.st.DB.C("payroll_override").FindOne(ctx, bson.M{"ym": ym, "employee_id": id}, options.FindOne().SetProjection(bson.D{
		{Key: "_id", Value: 0}, {Key: "paid_days", Value: 1}, {Key: "other_earn", Value: 1}, {Key: "other_ded", Value: 1}, {Key: "reason", Value: 1},
		{Key: "by_name", Value: 1}, {Key: "at", Value: 1}}))
	return map[string]any{"row": row, "override": ov}, nil
}

type overrideBody struct {
	PaidDays  *float64 `json:"paidDays"`
	OtherEarn float64  `json:"otherEarn"`
	OtherDed  float64  `json:"otherDed"`
	Reason    string   `json:"reason"`
}

// payrollEditOf is one row's edit for setOverrides.
type payrollEditOf struct {
	id string
	b  overrideBody
}

// setOverride saves Payroll's edit for one row and recomputes it when the dry run exists.
func (s *Server) setOverride(ctx context.Context, tx store.Q, c *Ctx, ym, id string, b overrideBody) error {
	return s.setOverrides(ctx, tx, c, ym, []payrollEditOf{{id: id, b: b}})
}

// setOverrides saves Payroll's edits in order and recomputes the stored rows they touch, with the same result
// as calling setOverride for each in turn. The month is locked once, and the touched rows and the month's totals
// are computed once for the whole set (not once per edit), so a sheet of thousands of rows stays well inside
// MongoDB's transaction time limit (60s by default; Postgres had none).
func (s *Server) setOverrides(ctx context.Context, tx store.Q, c *Ctx, ym string, edits []payrollEditOf) error {
	if len(edits) == 0 {
		return nil
	}
	m, err := ensurePayMonth(ctx, tx, ym)
	if err != nil {
		return err
	}
	if m.Status != "Open" && m.Status != "Dry run" {
		return bad("%s is %s. Send it back to the dry run to edit", ymLabel(ym), strings.ToLower(m.Status))
	}
	ids := []string{} // each person once, in the order of their first edit
	seen := map[string]bool{}
	for _, e := range edits {
		if !seen[e.id] {
			seen[e.id] = true
			ids = append(ids, e.id)
		}
	}
	ofThem := bson.M{"ym": ym, "employee_id": bson.M{"$in": ids}}
	befores := map[string]store.Row{}
	stored, err := tx.C("payroll_override").Find(ctx, ofThem)
	if err != nil {
		return err
	}
	for _, r := range stored {
		befores[str(r["employee_id"])] = r
	}
	days := calc.PayDays(ym, "calendar")
	// Paid days stay within the days employed: the row's basis less the days before joining or after leaving.
	maxPaid := map[string]float64{}
	if pc, list, err := loadPayCtx(ctx, tx, ym, bson.M{"_id": bson.M{"$in": ids}}); err == nil {
		for _, e := range list {
			o := pc.row(e)
			maxPaid[e.ID] = math.Max(0, float64(o.Days)-o.OutsideDays())
		}
	}
	written := map[string]bool{}
	for _, e := range edits {
		id, b := e.id, e.b
		if id == c.EmployeeID {
			return forbidden("You cannot change your own payroll row. Ask another payroll user")
		}
		b.Reason = strings.TrimSpace(b.Reason)
		clear := b.PaidDays == nil && b.OtherEarn == 0 && b.OtherDed == 0
		if !clear && len(b.Reason) < 3 {
			return bad("Give a reason for the change")
		}
		if b.PaidDays != nil && (*b.PaidDays < 0 || *b.PaidDays > float64(days)) {
			return bad("Paid days must be between 0 and %d", days)
		}
		if mp, ok := maxPaid[id]; ok && b.PaidDays != nil && *b.PaidDays > mp+0.001 {
			return bad("Paid days for %s can be at most %v: the days employed in %s", id, calc.Round2(mp), ymLabel(ym))
		}
		if b.OtherEarn < 0 || b.OtherDed < 0 {
			return bad("Enter other earning and other deduction as positive amounts")
		}
		this := bson.M{"ym": ym, "employee_id": id}
		before := befores[id]
		if written[id] { // edited earlier in this set: the record as that edit left it
			before, _ = tx.C("payroll_override").FindOne(ctx, this)
		}
		if clear {
			if _, err := tx.C("payroll_override").Delete(ctx, this); err != nil {
				return err
			}
		} else if _, err := tx.C("payroll_override").Upsert(ctx, this, bson.M{"paid_days": b.PaidDays, "other_earn": b.OtherEarn, "other_ded": b.OtherDed,
			"reason": b.Reason, "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		written[id] = true
		if err := audit(ctx, tx, c, "Edited payroll row", "payroll_override", ym+"/"+id, before, b); err != nil {
			return err
		}
	}
	// The dry run's stored rows of these people are recomputed with their final edits, then the month's totals.
	have, err := tx.C("payroll_row").Find(ctx, ofThem, options.Find().SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "employee_id", Value: 1}}))
	if err != nil || len(have) == 0 {
		return err
	}
	rowIDs := make([]string, 0, len(have))
	for _, r := range have {
		rowIDs = append(rowIDs, str(r["employee_id"]))
	}
	pc, list, err := loadPayCtx(ctx, tx, ym, bson.M{"_id": bson.M{"$in": rowIDs}})
	if err != nil {
		return err
	}
	for _, e := range list {
		row := pc.row(e)
		data, _ := json.Marshal(row)
		if _, err := tx.C("payroll_row").Update(ctx, bson.M{"ym": ym, "employee_id": e.ID}, bson.M{"$set": bson.M{"net": calc.Round2(row.Net), "hold": row.Hold,
			"flags": row.Flags, "data": data, "computed_at": tx.Now()}}); err != nil {
			return err
		}
	}
	tot, _ := payTotals(ctx, tx, ym)
	_, err = tx.C("payroll_month").Update(ctx, bson.M{"ym": ym}, bson.M{"$set": bson.M{"totals": tot}})
	return err
}

func (s *Server) payrollEdit(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 2)
	if err != nil {
		return nil, err
	}
	var b overrideBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("emp")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error { return s.setOverride(ctx, tx, c, ym, id, b) })
}

func (s *Server) payrollChecks(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	rows, err := loadRows(ctx, s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	cr, ids := []calc.CheckRow{}, []string{}
	for _, o := range rows {
		cr = append(cr, calc.CheckRow{Name: o.Name, Net: o.Net, Prev: o.Prev, Hold: o.Hold, Edited: o.Edited, Adjusted: o.OE != 0 || o.OD != 0, Joined: o.DOJ[:7] == ym})
		ids = append(ids, o.EmployeeID)
	}
	// Lines of people in the run, and (while a pilot runs) of the people left out of it.
	var lt calc.LineTotals
	inRun := bson.M{"$in": bson.A{"$employee_id", ids}}
	count := func(in any) bson.M { return bson.M{"$sum": bson.M{"$cond": bson.A{in, int64(1), int64(0)}}} }
	amount := func(in any, kind string) bson.M {
		return bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$and": bson.A{in, bson.M{"$eq": bson.A{"$kind", kind}}}}, "$amount", 0}}}
	}
	out := bson.M{"$not": bson.A{inRun}}
	if sums, err := s.st.DB.C("ledger").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"ym": ym, "void": false}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil},
			{Key: "n", Value: count(inRun)}, {Key: "earn", Value: amount(inRun, "earn")}, {Key: "ded", Value: amount(inRun, "ded")},
			{Key: "outN", Value: count(out)}, {Key: "outEarn", Value: amount(out, "earn")}, {Key: "outDed", Value: amount(out, "ded")}}}},
	}); err == nil && len(sums) > 0 {
		t := sums[0]
		lt.N, lt.OutN = int(payengineFloat(t["n"])), int(payengineFloat(t["outN"]))
		lt.Earn, lt.Ded = calc.Round2(payengineFloat(t["earn"])), calc.Round2(payengineFloat(t["ded"]))
		lt.OutEarn, lt.OutDed = calc.Round2(payengineFloat(t["outEarn"])), calc.Round2(payengineFloat(t["outDed"]))
	}
	if m, err := loadPayMonth(ctx, s.st.DB, ym, false); err == nil {
		lt.Pilot = len(m.Pilot) > 0
	}
	if lt.OutN > 0 && !lt.Pilot {
		lt.OutWho, _ = linesOutsideRun(ctx, s.st.DB, ym)
	}
	return append(append(calc.RunChecks(cr, lt), s.ackRunChecks(ctx, ym, ids)...), varRunChecks(rows)...), nil
}

// ---------- Sheet download and upload ----------

var sheetCols = []string{"Emp ID", "Name", "Type", "Department", "Grade", "City", "Location", "Paid days", "LOP days", "Days outside employment", "Basic", "HRA", "Special allowance", "Bonus",
	"Variable paid", "Arrears and reimbursements", "Other earnings", "Total earnings", "PF", "ESI", "LWF", "Meal and accommodation", "TDS", "Recoveries",
	"Other deductions", "Total deductions", "Rounding off", "Net pay", "Hold", "Adjustment reason", "Arrear and reimbursement details", "Recovery details"}

// sheetCompCols end the payroll sheet and register when a row is on a structure made from components (paycomp.go).
var sheetCompCols = []string{"Component deductions", "Component details"}

// sheetWithComps adds the component columns when some row has components, so a month of converted structures only
// gives exactly the sheet it gave before.
func sheetWithComps(rows []*PayRowOut, out [][]any) ([]string, [][]any) {
	has := false
	for _, o := range rows {
		has = has || len(o.R.Comps) > 0
	}
	if !has {
		return sheetCols, out
	}
	for i, o := range rows {
		out[i] = append(out[i], calc.Round2(o.R.CompDed), compText(o.R.Comps))
	}
	return append(append([]string{}, sheetCols...), sheetCompCols...), out
}

// sheetWithFee adds a "Retainer fee" column after Special allowance when a row is a retainer's (calc retainerOnly),
// so a month without retainers gives exactly the sheet it gave before.
func sheetWithFee(rows []*PayRowOut, cols []string, out [][]any) ([]string, [][]any) {
	at := -1
	for i, c := range cols {
		if c == "Special allowance" {
			at = i + 1
		}
	}
	has := false
	for _, o := range rows {
		has = has || o.R.Fee != 0
	}
	if !has || at < 0 {
		return cols, out
	}
	ins := func(l []any, v any) []any { return append(append(append([]any{}, l[:at]...), v), l[at:]...) }
	for i, o := range rows {
		out[i] = ins(out[i], figuresOf(o).Fee)
	}
	nc := append(append(append([]string{}, cols[:at]...), "Retainer fee"), cols[at:]...)
	return nc, out
}

// sheetWithReimb adds a "Reimbursement" column after HRA when a row has one (reimb.go), so a month without any gives
// exactly the sheet it gave before.
func sheetWithReimb(rows []*PayRowOut, cols []string, out [][]any) ([]string, [][]any) {
	at := -1
	for i, c := range cols {
		if c == "HRA" {
			at = i + 1
		}
	}
	has := false
	for _, o := range rows {
		has = has || o.R.Reimb != 0
	}
	if !has || at < 0 {
		return cols, out
	}
	ins := func(l []any, v any) []any { return append(append(append([]any{}, l[:at]...), v), l[at:]...) }
	for i, o := range rows {
		out[i] = ins(out[i], figuresOf(o).Reimb)
	}
	return append(append(append([]string{}, cols[:at]...), "Reimbursement"), cols[at:]...), out
}

func lineText(l []calc.Line) string {
	out := []string{}
	for _, x := range l {
		t := x.Label
		if x.Reason != "" {
			t += " (" + x.Reason + ")"
		}
		out = append(out, t+" "+trimNum(calc.Round2(x.Amount)))
	}
	return strings.Join(out, "; ")
}

// sheetRow is a row of the payroll sheet and register: the same paise figures as the payslip and bank advice
// (figuresOf), so each row adds up and its net is what the bank is paid (ITM-59).
func sheetRow(o *PayRowOut) []any {
	hold := ""
	if o.Hold {
		hold = "Yes"
	}
	f := figuresOf(o)
	return []any{o.EmployeeID, o.Name, o.Type, o.Dept, o.Grade, o.City, o.Location, o.Paid, f.LOP, f.Outside, f.Basic, f.Hra,
		f.Sa, f.Bonus, f.VarPay, f.Add, f.OE, f.TotE, f.PF, f.ESI, f.LWF,
		f.MealAcc, f.Tds, f.Rec, f.OD, f.TotD, f.RoundOff, f.Net, hold, o.Reason, lineText(o.Add), lineText(o.Ded)}
}

// compText lists a designed structure's components with this month's amounts ("" for a converted structure).
func compText(l []calc.CompLine) string {
	out := []string{}
	for _, x := range l {
		t := x.Name
		switch {
		case x.Side == calc.SideDed:
			t += " (deduction)"
		case x.Kind == calc.CompVariable:
			t += " (variable paid)"
		}
		out = append(out, t+" "+trimNum(calc.Round2(x.Amount)))
	}
	return strings.Join(out, "; ")
}

func (s *Server) payrollSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return "", nil, nil, err
	}
	all, err := loadRows(r.Context(), s.st.DB, ym)
	if err != nil {
		return "", nil, nil, err
	}
	rows := filterRows(c, all, queryMap(r, "q", "city", "location", "type", "dept", "flag"))
	out := [][]any{}
	for _, o := range rows {
		out = append(out, sheetRow(o))
	}
	cols, out := sheetWithComps(rows, out)
	cols, out = sheetWithFee(rows, cols, out)
	cols, out = sheetWithReimb(rows, cols, out)
	return "payroll-sheet-" + ym + ".xlsx", cols, out, nil
}

// payrollUpload reads a corrected sheet: Paid days, Other earnings, Other deductions and
// Adjustment reason per Emp ID. check lists the changes; apply saves them.
func (s *Server) payrollUpload(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 2)
	if err != nil {
		return nil, err
	}
	apply := r.PathValue("action") == "apply"
	sheet, err := readUpload(r)
	if err != nil {
		return nil, err
	}
	if len(sheet) < 2 {
		return nil, bad("The sheet has no rows")
	}
	head, data := sheet[0], sheet[1:]
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	for _, k := range []string{"Emp ID", "Paid days", "Other earnings", "Other deductions", "Adjustment reason"} {
		if _, ok := col[k]; !ok {
			return nil, bad("The sheet needs the column “%s”. Download the payroll sheet and edit it", k)
		}
	}
	ctx := r.Context()
	rows, err := loadRows(ctx, s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	byID := map[string]*PayRowOut{}
	for _, o := range rows {
		byID[o.EmployeeID] = o
	}
	type change struct {
		Row     int      `json:"row"`
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		What    []string `json:"what"`
		Problem string   `json:"problem,omitempty"`
	}
	changes := []change{}
	unchanged := 0 // rows in the run that the sheet leaves as they are
	cell := func(rec []string, k string) string {
		if i := col[k]; i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	num := func(v string) (float64, bool) {
		if v == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
		return f, err == nil
	}
	todo := []payrollEditOf{}
	for i, rec := range data {
		id := strings.ToUpper(cell(rec, "Emp ID"))
		if id == "" {
			continue
		}
		ch := change{Row: i + 2, ID: id}
		o := byID[id]
		if o == nil || !c.Scope.Allows(o.scoped()) {
			ch.Problem = "Not in this payroll run"
			changes = append(changes, ch)
			continue
		}
		ch.Name = o.Name
		pd, ok1 := num(cell(rec, "Paid days"))
		oe, ok2 := num(cell(rec, "Other earnings"))
		od, ok3 := num(cell(rec, "Other deductions"))
		if !ok1 || !ok2 || !ok3 {
			ch.Problem = "Paid days, other earnings and other deductions must be numbers"
			changes = append(changes, ch)
			continue
		}
		b := overrideBody{OtherEarn: oe, OtherDed: od, Reason: cell(rec, "Adjustment reason")}
		if math.Abs(pd-o.Paid) > 0.001 {
			b.PaidDays = &pd
			ch.What = append(ch.What, fmt.Sprintf("paid days %v → %v", o.Paid, pd))
		} else if o.Edited {
			p := o.Paid
			b.PaidDays = &p
		}
		if oe != o.OE {
			ch.What = append(ch.What, fmt.Sprintf("other earning %v → %v", o.OE, oe))
		}
		if od != o.OD {
			ch.What = append(ch.What, fmt.Sprintf("other deduction %v → %v", o.OD, od))
		}
		if len(ch.What) == 0 {
			unchanged++
			continue
		}
		maxPaid := math.Max(0, float64(o.Days)-o.OutsideDays()) // the days employed in the month
		if id == c.EmployeeID {
			ch.Problem = "You cannot change your own payroll row"
		} else if len(strings.TrimSpace(b.Reason)) < 3 {
			ch.Problem = "Give the adjustment reason"
		} else if pd < 0 || pd > float64(o.Days) {
			ch.Problem = fmt.Sprintf("Paid days must be between 0 and %d", o.Days)
		} else if b.PaidDays != nil && pd > maxPaid+0.001 {
			ch.Problem = fmt.Sprintf("Paid days can be at most %v: the days employed in the month", calc.Round2(maxPaid))
		}
		changes = append(changes, ch)
		if ch.Problem == "" {
			todo = append(todo, payrollEditOf{id: id, b: b})
		}
	}
	problems := 0
	for _, ch := range changes {
		if ch.Problem != "" {
			problems++
		}
	}
	if apply && problems == 0 {
		err = s.st.Tx(ctx, func(tx store.Q) error { return s.setOverrides(ctx, tx, c, ym, todo) })
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"changes": changes, "problems": problems, "applied": apply && problems == 0, "ready": len(todo), "unchanged": unchanged}, nil
}

// ---------- Register, bank advice, cost ----------

func (s *Server) payrollRegister(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	months, err := s.st.DB.C("payroll_month").Find(r.Context(), bson.M{"status": bson.M{"$in": bson.A{"Approved", "Published"}}},
		options.Find().SetSort(bson.D{{Key: "ym", Value: -1}}).SetProjection(bson.D{{Key: "ym", Value: 1}, {Key: "status", Value: 1}, {Key: "totals", Value: 1},
			{Key: "approved_by", Value: 1}, {Key: "approved_at", Value: 1}, {Key: "published_by", Value: 1}, {Key: "published_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, m := range months {
		m["approved_at"] = payengineStamp(m["approved_at"], "02 Jan 2006 15:04")
		m["published_at"] = payengineStamp(m["published_at"], "02 Jan 2006 15:04")
	}
	return months, nil
}

func (s *Server) approvedRows(r *http.Request, c *Ctx) (string, []*PayRowOut, error) {
	if err := c.Need("pay.export", 2); err != nil {
		return "", nil, err
	}
	ym := r.PathValue("ym")
	m, err := loadPayMonth(r.Context(), s.st.DB, ym, false)
	if err != nil {
		return "", nil, err
	}
	if m.Status != "Approved" && m.Status != "Published" {
		return "", nil, bad("%s is not approved yet", ymLabel(ym))
	}
	rows, err := loadRows(r.Context(), s.st.DB, ym)
	return ym, filterRows(c, rows, map[string]string{}), err
}

// payrollBank is the bank advice: net pay by account, held salaries left out.
func (s *Server) payrollBank(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	ym, rows, err := s.approvedRows(r, c)
	if err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	// Each person's account in force in the month: the latest that started by then.
	who := make([]string, 0, len(rows))
	for _, o := range rows {
		who = append(who, o.EmployeeID)
	}
	accounts, err := s.st.DB.C("employee_bank").Find(ctx, bson.M{"employee_id": bson.M{"$in": who}, "from_month": bson.M{"$lte": ym}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		return "", nil, nil, err
	}
	banks := map[string][3]string{}
	for _, b := range accounts {
		id := str(b["employee_id"])
		if _, seen := banks[id]; !seen {
			banks[id] = [3]string{str(b["bank_name"]), str(b["account_no"]), str(b["ifsc"])}
		}
	}
	out := [][]any{}
	for _, o := range rows {
		if o.Hold || o.Net <= 0 {
			continue
		}
		b := banks[o.EmployeeID]
		out = append(out, []any{o.EmployeeID, o.Name, b[0], b[1], b[2], calc.Round2(o.Net), "Salary " + ymLabel(ym)})
	}
	return "bank-advice-" + ym + ".xlsx", []string{"Emp ID", "Name", "Bank", "Account number", "IFSC", "Amount", "Narration"}, out, nil
}

func (s *Server) payrollRegisterSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	ym, rows, err := s.approvedRows(r, c)
	if err != nil {
		return "", nil, nil, err
	}
	out := [][]any{}
	for _, o := range rows {
		out = append(out, sheetRow(o))
	}
	cols, out := sheetWithComps(rows, out)
	cols, out = sheetWithFee(rows, cols, out)
	cols, out = sheetWithReimb(rows, cols, out)
	return "payroll-register-" + ym + ".xlsx", cols, out, nil
}

type costAgg struct {
	Label    string  `json:"label"`
	People   int     `json:"people"`
	Earned   float64 `json:"earned"`
	Employer float64 `json:"employer"`
	Net      float64 `json:"net"`
	Cost     float64 `json:"cost"`
}

// payrollCost summarises the month's cost by city, department, type and grade.
func (s *Server) payrollCost(r *http.Request, c *Ctx) (any, error) {
	ym, err := s.payYm(r, c, 1)
	if err != nil {
		return nil, err
	}
	rows, err := loadRows(r.Context(), s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	rows = filterRows(c, rows, map[string]string{})
	type agg = costAgg
	out := map[string][]*agg{}
	for _, by := range []string{"city", "dept", "type", "grade"} {
		m := map[string]*agg{}
		for _, o := range rows {
			k := map[string]string{"city": o.City, "dept": o.Dept, "type": o.Type, "grade": o.Grade}[by]
			a := m[k]
			if a == nil {
				a = &agg{Label: k}
				m[k] = a
			}
			a.People++
			a.Earned += o.TotalEarn
			a.Employer += o.R.EmployerCost
			a.Net += o.Net
			a.Cost += o.TotalEarn + o.R.EmployerCost
		}
		l := []*agg{}
		for _, a := range m {
			a.Earned, a.Employer, a.Net, a.Cost = calc.Round2(a.Earned), calc.Round2(a.Employer), calc.Round2(a.Net), calc.Round2(a.Cost)
			l = append(l, a)
		}
		sort.Slice(l, func(i, j int) bool { return l[i].Cost > l[j].Cost })
		out[by] = l
	}
	return out, nil
}
