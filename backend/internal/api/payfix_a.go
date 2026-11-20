package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Payroll run integrity: a paid month stays as it was paid, approval approves what was verified, one person does
// not both prepare and approve a run, and a salary held for a bank change is paid once the account is verified.

// payMonthOf is the payroll month whose attendance window holds date (the calendar month, or with the 26th–25th
// cut-off the next month from the 26th).
func payMonthOf(date, cutoff string) string {
	if cutoff == "26-25" && len(date) == 10 && date[8:] >= "26" {
		return nextYm(date[:7])
	}
	return date[:7]
}

// payMonthsBetween lists the payroll months whose windows touch the dates from..to.
func payMonthsBetween(from, to, cutoff string) []string {
	if !isDate(from) || !isDate(to) || to < from {
		return nil
	}
	out := []string{}
	for ym := payMonthOf(from, cutoff); ym <= payMonthOf(to, cutoff); ym = nextYm(ym) {
		out = append(out, ym)
	}
	return out
}

// paidMonthMsg refuses a change to attendance or leave on dates of a payroll month already approved or published
// ("" when none is). Spec 15.1: after the lock, corrections are paid as arrears; an exception opens a locked month
// for a correction only until the month is approved, so a paid month never changes under the payslip.
func paidMonthMsg(ctx context.Context, q store.Q, from, to string) string {
	months := payMonthsBetween(from, to, cutoffSetting(ctx, q))
	if len(months) == 0 {
		return ""
	}
	m, _ := q.C("payroll_month").FindOne(ctx, bson.M{"ym": bson.M{"$in": months}, "status": bson.M{"$in": bson.A{"Approved", "Published"}}},
		options.FindOne().SetSort(bson.D{{Key: "ym", Value: 1}}))
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%s is already paid. Raise an arrear or recovery in the open month instead", ymLabel(str(m["ym"])))
}

// lockedMonthOf is the payroll month of date when its attendance is locked by payroll ("" when it is not).
func lockedMonthOf(ctx context.Context, q store.Q, date string) string {
	ym := payMonthOf(date, cutoffSetting(ctx, q))
	if ok, _ := q.C("payroll_month").Exists(ctx, bson.D{{Key: "ym", Value: ym}, {Key: "$or", Value: shiftsLocked}}); ok {
		return ym
	}
	return ""
}

// requestDates are the dates a leave or regularization request covers ("" when it covers none).
func requestDates(r *Request) (string, string) {
	p := payloadOf(r)
	switch r.WF {
	case "leave":
		return p.From, p.To
	case "regularize":
		return p.Date, p.Date
	}
	return "", ""
}

// ---------- Stale run: what changed after the rows were calculated ----------

// staleEntities are the setups whose change moves pay (policies, calendars, leave plans, structures, shifts, LWF).
var staleEntities = bson.A{"work_policy", "holiday_calendar", "leave_plan", "leave_type", "salary_structure", "shift", "lwf_rule", "comp_type_rule", "pf_exempt", "pf_settings"}

// payStaleMore reports a change after since that the stored rows of month m do not include: a gross revision for the
// month, attendance written for its window, a leave, regularization or bank change decided, or a pay setup changed.
func payStaleMore(ctx context.Context, q store.Q, m *payMonth, since time.Time) bool {
	who := any(bson.M{"$ne": nil})
	if len(m.Pilot) > 0 {
		who = bson.M{"$in": m.Pilot}
	}
	from, to := window(m.YM, cutoffSetting(ctx, q))
	if ok, _ := q.C("gross_history").Exists(ctx, bson.M{"employee_id": who, "from_month": bson.M{"$lte": m.YM}, "created_at": bson.M{"$gt": since}}); ok {
		return true
	}
	if ok, _ := q.C("attendance_day").Exists(ctx, bson.M{"employee_id": who, "date": bson.M{"$gte": from, "$lte": to}, "at": bson.M{"$gt": since}}); ok {
		return true
	}
	if ok, _ := q.C("audit_log").Exists(ctx, bson.M{"entity": bson.M{"$in": staleEntities}, "at": bson.M{"$gt": since}}); ok {
		return true
	}
	decided, err := q.C("approval_history").Distinct(ctx, "request_id", bson.M{"at": bson.M{"$gt": since}, "action": bson.M{"$in": bson.A{"Approved", "Applied", "Rejected", "Cancelled", "Corrected"}}})
	if err != nil || len(decided) == 0 {
		return false
	}
	ok, _ := q.C("approval_request").Exists(ctx, bson.M{"_id": bson.M{"$in": decided}, "status": bson.M{"$ne": "Pending"}, "employee_id": who, "$or": bson.A{
		bson.M{"wf": "p_change", "payload.kind": "Bank"},
		bson.M{"wf": "regularize", "payload.date": bson.M{"$gte": from, "$lte": to}},
		bson.M{"wf": "leave", "payload.from": bson.M{"$lte": to}, "payload.to": bson.M{"$gte": from}},
	}})
	return ok
}

// ---------- Maker-checker ----------

// approverMadeRun refuses approval by the person who verified the run or edited one of its rows (spec 15.1: the
// Payroll Manager approves what Payroll prepared).
func approverMadeRun(ctx context.Context, q store.Q, c *Ctx, ym string) error {
	me := "(" + c.EmployeeID + ")"
	verified, _ := q.C("audit_log").FindOne(ctx, bson.M{"entity": "payroll_month", "entity_id": ym, "action": "Payroll " + ymLabel(ym) + ": verified"},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}))
	if verified != nil && strings.HasSuffix(str(verified["actor"]), me) {
		return forbidden("You verified " + ymLabel(ym) + ". Another payroll approver must approve it")
	}
	if edited, _ := q.C("audit_log").Exists(ctx, bson.M{"entity": "payroll_override", "entity_id": bson.M{"$regex": "^" + ym + "/"},
		"actor": bson.M{"$regex": "\\(" + c.EmployeeID + "\\)$"}}); edited {
		return forbidden("You edited rows of " + ymLabel(ym) + ". Another payroll approver must approve it")
	}
	return nil
}

// ---------- Salary held for a bank change ----------

// bankHoldWhy is why the pay engine holds a row while a new bank account waits for verification.
const bankHoldWhy = "New bank account waiting for verification"

// bankHoldSource marks the salary holds placed for it, released once no bank change is pending.
const bankHoldSource = "bank-change"

// syncBankHolds keeps a salary hold for everyone whose run salary was held for a bank change: a hold is placed
// from the first such month that is not paid yet (so the Salary holds page lists it), and released, paying every
// held month in the open month as "Held salary released", once the person has no bank change pending (verified,
// or rejected and paid to the account on record).
func syncBankHolds(ctx context.Context, tx store.Q) error {
	if err := releaseNoBankHolds(ctx, tx); err != nil {
		return err
	}
	months, err := payitemsRunMonths(ctx, tx, bson.M{"$ne": ""})
	if err != nil || len(months) == 0 {
		return err
	}
	rows, err := tx.C("payroll_row").Find(ctx, bson.M{"ym": bson.M{"$in": months}, "hold": true, "data.holdWhy": bankHoldWhy},
		options.Find().SetSort(bson.D{{Key: "ym", Value: 1}}).SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "ym", Value: 1}}))
	if err != nil {
		return err
	}
	first := map[string]string{}
	for _, r := range rows {
		id, ym := str(r["employee_id"]), str(r["ym"])
		if first[id] != "" {
			continue
		}
		// Already paid: released by an earlier hold.
		if paid, _ := tx.C("ledger").Exists(ctx, bson.M{"employee_id": id, "label": "Held salary released, " + ymLabel(ym), "void": false}); paid {
			continue
		}
		first[id] = ym
	}
	ids := make([]string, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ok, _ := tx.C("salary_hold").Exists(ctx, bson.M{"employee_id": id, "status": "Active"}); ok {
			continue
		}
		if _, err := placeHold(ctx, tx, "System", id, bankHoldWhy, "Salary for "+ymLabel(first[id])+" was held while a new bank account waited for verification. "+
			"Released automatically once the bank change is decided.", first[id], bankHoldSource); err != nil {
			return err
		}
	}
	holds, err := tx.C("salary_hold").Find(ctx, bson.M{"status": "Active", "source_id": bankHoldSource})
	if err != nil {
		return err
	}
	for _, h := range holds {
		emp := str(h["employee_id"])
		if pending, _ := tx.C("approval_request").Exists(ctx, bson.M{"wf": "p_change", "status": "Pending", "payload.kind": "Bank", "employee_id": emp}); pending {
			continue
		}
		note := "Bank change decided"
		if b, _ := tx.C("employee_bank").FindOne(ctx, bson.M{"employee_id": emp}, options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}})); b != nil {
			acct := str(b["account_no"])
			if len(acct) > 4 {
				acct = acct[len(acct)-4:]
			}
			note = "Bank account ending " + acct + " in use"
		}
		if _, err := releaseHold(ctx, tx, "System", str(h["id"]), note); err != nil {
			return err
		}
	}
	return nil
}

// releaseNoBankHolds releases the salary holds placed from the payroll checklist for having no bank account on record
// (holdNoBank) once the person has an account on record in the open month (added, or a bank change verified) and no
// bank change waits: every held month already run is paid in the open month as "Held salary released, <month>"
// (releaseHold, as for a bank change). A hold released before its month is run simply stops holding it.
func releaseNoBankHolds(ctx context.Context, tx store.Q) error {
	holds, err := tx.C("salary_hold").Find(ctx, bson.M{"status": "Active", "source_id": noBankHoldSource}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil || len(holds) == 0 {
		return err
	}
	open := openYm(ctx, tx)
	for _, h := range holds {
		emp := str(h["employee_id"])
		if pending, _ := tx.C("approval_request").Exists(ctx, bson.M{"wf": "p_change", "status": "Pending", "payload.kind": "Bank", "employee_id": emp}); pending {
			continue // paid once the change is decided
		}
		b, err := tx.C("employee_bank").FindOne(ctx, bson.M{"employee_id": emp, "from_month": bson.M{"$lte": open}},
			options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}))
		if err != nil {
			return err
		}
		if b == nil {
			continue
		}
		acct := str(b["account_no"])
		if len(acct) > 4 {
			acct = acct[len(acct)-4:]
		}
		if _, err := releaseHold(ctx, tx, "System", str(h["id"]), "Bank account ending "+acct+" on record"); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Pay lines before joining ----------

// joinPayMonth is the month a pay line for emp can be paid in: ym, or the joining month when the person joins later
// (a person has no payroll row before joining, so a line there would never be paid).
func joinPayMonth(ctx context.Context, q store.Q, emp, ym string) string {
	e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": emp}, options.FindOne().SetProjection(bson.D{{Key: "doj", Value: 1}}))
	if d := str(e["doj"]); len(d) >= 7 && d[:7] > ym {
		return d[:7]
	}
	return ym
}

// movePreJoinLines moves the open lines of month ym that belong to people who join after it into their joining
// month (lines approved before this was checked). Run months are never touched.
func movePreJoinLines(ctx context.Context, tx store.Q, ym string) error {
	ids, err := tx.C("ledger").Distinct(ctx, "employee_id", bson.M{"ym": ym, "void": false})
	if err != nil || len(ids) == 0 {
		return err
	}
	late, err := tx.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids}, "doj": bson.M{"$gt": policy.LastDayOf(ym)}},
		options.Find().SetProjection(bson.D{{Key: "doj", Value: 1}}))
	if err != nil {
		return err
	}
	for _, e := range late {
		if _, err := tx.C("ledger").UpdateMany(ctx, bson.M{"ym": ym, "void": false, "employee_id": e["id"]}, bson.M{"$set": bson.M{"ym": str(e["doj"])[:7]}}); err != nil {
			return err
		}
	}
	return nil
}

// linesOutsideRun names the people with pay lines in month ym but no row in its run (empty when there are none).
func linesOutsideRun(ctx context.Context, q store.Q, ym string) ([]string, error) {
	inRun, err := q.C("payroll_row").Distinct(ctx, "employee_id", bson.M{"ym": ym})
	if err != nil {
		return nil, err
	}
	out, err := q.C("ledger").Distinct(ctx, "employee_id", bson.M{"ym": ym, "void": false, "employee_id": bson.M{"$nin": inRun}})
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, id := range out {
		e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.D{{Key: "name", Value: 1}}))
		names = append(names, strings.TrimSpace(str(e["name"])+" ("+str(id)+")"))
	}
	sort.Strings(names)
	return names, nil
}

// cutoffChangeErr refuses a cut-off change once a month has been paid (the next window would overlap days already
// paid or skip some) or while the open month is closed or run.
func cutoffChangeErr(ctx context.Context, q store.Q) error {
	if paid, _ := q.C("payroll_month").Exists(ctx, bson.M{"status": bson.M{"$in": bson.A{"Approved", "Published"}}}); paid {
		return bad("The cut-off cannot change once a month has been paid: the next window would overlap days already paid or leave days out")
	}
	if m, _ := loadPayMonth(ctx, q, openYm(ctx, q), false); m.Status != "Open" || m.LockedAt != nil {
		return bad("Change the cut-off before the open month is closed or run")
	}
	return nil
}

// ---------- TDS of a month approved but not yet published ----------

// addApprovedTds counts the Section 192 TDS of the financial year's months before ym that are approved but not yet
// published (their TDS history is written at publish), so the next month does not deduct it again (RUN-85).
func addApprovedTds(ctx context.Context, q store.Q, td *taxData, fy, ym string, ids []string) error {
	months, err := q.C("payroll_month").Distinct(ctx, "ym", bson.M{"status": "Approved", "ym": bson.M{"$lt": ym}})
	if err != nil || len(months) == 0 {
		return err
	}
	in := bson.A{}
	for _, m := range months {
		if calc.FY(str(m)) == fy {
			in = append(in, m)
		}
	}
	if len(in) == 0 {
		return nil
	}
	rows, err := q.C("payroll_row").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"ym": bson.M{"$in": in}, "employee_id": bson.M{"$in": ids}}},
		bson.M{"$project": bson.M{"_id": 0, "employee_id": 1, "ym": 1, "stds": "$data.stds"}},
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		id, m := str(r["employee_id"]), str(r["ym"])
		v := payengineFloat(r["stds"])
		if v <= 0 {
			continue
		}
		if td.hist[id] == nil {
			td.hist[id] = map[string]float64{}
		}
		if _, ok := td.hist[id][m]; !ok {
			td.hist[id][m] = calc.Round2(v)
		}
	}
	return nil
}
