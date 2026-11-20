package api

// Monthly incentives (owner's request of 14 Nov 2026): Bulk update › Incentives. Each row (Employee ID, Month, Amount,
// Remark) becomes an earning pay line "Incentive" in that month, with the remark as the line's reason, so it shows on
// the payslip, the payroll sheet and register, the full salary register's Incentive column and Upcoming pay lines.
//
//   - Month: the open payroll month or later (at most 12 months ahead). A month already paid is closed; the open month
//     is locked for new lines once its Reimbursement or Deduction closure is closed or the run is verified, as other
//     pay lines then go into the next month: the row is refused and names the first month that is still open.
//   - Duplicates: the same employee, month and remark is refused (also twice in one file), so uploading again is safe.
//   - Statutory treatment: taxable salary (it is in the pay row's total earnings, which the tax pages and Form 16
//     count, and the month's TDS projection counts the year's incentives, tds.go). Not PF or ESI wages: like bonus,
//     variable pay and every other pay line, PF is on Basic and ESI on the earned gross only (spec 14 "Variable is not
//     in PF wage and currently not in ESI wage"; the spec says nothing about incentives).
//   - Who: Payroll (pay items at Edit: claims at Edit) or HR Admin.

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	incentiveLabel  = "Incentive"
	incentiveSource = "BULK-INC"
	incentiveMax    = 1000000
)

var incentiveCols = []string{"Employee ID", "Month", "Amount (₹)", "Remark"}

func init() {
	bulkTemplates = append(bulkTemplates, &bulkTemplate{Key: "incentive", Name: "Incentives", Cols: incentiveCols, Optional: []string{"Remark"},
		Hint: "One row per incentive: Employee ID, the month it is paid in (2026-11 or Nov-2026), the amount and a remark. Each row becomes an earning \"Incentive\" on that month's payslip. " +
			"The month must be the open payroll month or later; a month already paid, or the open month once its pay lines are closed, is refused. The same employee, month and remark is loaded once, so uploading the file again is safe. Taxable; not PF or ESI wages.",
		allow: func(c *Ctx) bool { return c.Level("pay.claims") >= 2 || c.Role == "HR Admin" },
		note:  incentiveNote, how: incentiveHow, row: bulkIncentive})
}

// incentiveFirstYm is the first month an incentive can go into: the open month, or the next one once the open month's
// Reimbursement or Deduction closure is closed or its run is verified (why says which).
func incentiveFirstYm(ctx context.Context, q store.Q) (open, first, why string) {
	open = openYm(ctx, q)
	first = open
	if closed, _ := q.C("payroll_closure").Find(ctx, bson.M{"ym": open, "closure": bson.M{"$in": bson.A{"claim", "deduction"}}, "closed_at": bson.M{"$ne": nil}}); len(closed) > 0 {
		first, why = nextYm(open), closureDefs[str(closed[0]["closure"])].Title+" is closed"
	}
	if m, _ := q.C("payroll_month").FindOne(ctx, bson.M{"ym": open}); m != nil && str(m["status"]) == "Verified" {
		first, why = nextYm(open), "the run is verified"
	}
	return
}

// incentiveRowKey is the key that finds the same incentive twice in one file: the employee, the month and the remark.
func incentiveRowKey(v map[string]string) string {
	ym := strings.TrimSpace(v["Month"])
	if m, ok := openMonth(ym); ok {
		ym = m
	}
	return strings.TrimSpace(v["Employee ID"]) + "|" + ym + "|" + strings.Join(strings.Fields(v["Remark"]), " ")
}

func incentiveNote(ctx context.Context, q store.Q) string {
	open, first, why := incentiveFirstYm(ctx, q)
	if first != open {
		return fmt.Sprintf("Incentives go into %s or later: %s's pay lines are locked (%s).", ymLabel(first), ymLabel(open), why)
	}
	return fmt.Sprintf("Incentives go into the open payroll month, %s, or later.", ymLabel(open))
}

func bulkIncentive(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	er := []string{}
	status, doj := str(e["status"]), str(e["doj"])
	if status == "Pre-boarding" {
		res.Errors = append(res.Errors, res.Name+" is pre-boarding (not joined yet)")
		return res, nil
	}
	ym := ""
	if v := strings.TrimSpace(r.V["Month"]); v == "" {
		er = append(er, "Give the month, like 2026-11 or Nov-2026")
	} else if m, ok := openMonth(v); !ok {
		er = append(er, "Month must be a month, like 2026-11 or Nov-2026")
	} else {
		ym = m
	}
	amt, okA := openAmount(r.V["Amount (₹)"])
	if !okA || amt <= 0 || amt > incentiveMax {
		er = append(er, "Amount must be a number above 0, at most ₹10,00,000")
	}
	remark := strings.Join(strings.Fields(r.V["Remark"]), " ")
	if len([]rune(remark)) > 200 {
		er = append(er, "Remark is longer than 200 characters")
	}
	if remark != "" && strings.ContainsAny(remark[:1], "=+-@") {
		er = append(er, "Remark cannot start with = + - or @")
	}
	if ym != "" {
		open, first, why := incentiveFirstYm(ctx, tx)
		switch {
		case ym < open:
			er = append(er, fmt.Sprintf("%s is already paid and closed. Incentives go in the open payroll month (%s) or later", ymLabel(ym), ymLabel(open)))
		case ym < first:
			er = append(er, fmt.Sprintf("%s is locked for new pay lines (%s). Use %s or later", ymLabel(ym), why, ymLabel(first)))
		case ym > addYm(open, 12):
			er = append(er, fmt.Sprintf("%s is more than 12 months after the open month (%s)", ymLabel(ym), ymLabel(open)))
		}
		if len(doj) >= 7 && ym < doj[:7] {
			er = append(er, fmt.Sprintf("%s is before the joining month (%s)", ymLabel(ym), ymLabel(doj[:7])))
		}
		if status == "Inactive" {
			if x, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": res.ID}); x != nil && str(x["lwd"]) != "" && str(x["lwd"]) < ym+"-01" {
				er = append(er, fmt.Sprintf("%s left on %s, before %s. Pay it in the full and final instead", res.Name, longDate(str(x["lwd"])), ymLabel(ym)))
			}
		}
	}
	if len(er) == 0 {
		dup, err := tx.C("ledger").FindOne(ctx, bson.M{"employee_id": res.ID, "ym": ym, "kind": "earn", "label": incentiveLabel, "void": false, "reason": payitemsNull(remark)})
		if err != nil {
			return res, err
		}
		if dup != nil {
			what := "with the remark \"" + remark + "\""
			if remark == "" {
				what = "with no remark"
			}
			er = append(er, fmt.Sprintf("Already loaded: an incentive of %s for %s in %s %s", calc.Inr(payengineFloat(dup["amount"])), res.ID, ymLabel(ym), what))
		}
	}
	if len(er) > 0 {
		res.Errors = er
		return res, nil
	}
	if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": res.ID}); err != nil {
		return res, err
	}
	if err := addLedger(ctx, tx, res.ID, ym, "earn", incentiveLabel, amt, incentiveSource, remark); err != nil {
		return res, err
	}
	res.Changes = append(res.Changes, bulkChange{Field: incentiveLabel, From: "", To: calc.Inr(amt) + " in " + ymLabel(ym), Note: remark})
	if apply {
		if err := audit(ctx, tx, c, "Incentive added", "ledger", res.ID+"/"+ym, nil, map[string]any{"employeeId": res.ID, "ym": ym, "amount": amt, "remark": remark}); err != nil {
			return res, err
		}
	}
	return res, nil
}

func incentiveHow(ctx context.Context, q store.Q) []string {
	open, first, _ := incentiveFirstYm(ctx, q)
	return []string{
		"## Columns",
		"Employee ID: as in the HRMS, for example EMP-1020. The employee must have joined (not Pre-boarding).",
		"Month: the payroll month the incentive is paid in, as 2026-11, Nov-2026 or 11/2026 (an Excel date works too).",
		"Amount (₹): the incentive, above 0 and at most 10,00,000.",
		"Remark: optional, up to 200 characters, for example \"Sales target Q3\". Shown next to the line in the payroll sheet and Upcoming pay lines.",
		"",
		"## Months",
		"The open payroll month is now " + ymLabel(open) + "; incentives go into " + ymLabel(first) + " or later.",
		"A month already paid is closed. The open month is locked for new pay lines once its Reimbursement or Deduction closure is closed or its run is verified; the check then gives the next month.",
		"",
		"## Tax, PF and ESI",
		"An incentive is taxable salary: it counts in the year's salary for TDS and Form 16. It is not PF or ESI wages, like bonus and variable pay: PF is on Basic and ESI on the earned gross.",
		"",
		"## Example",
		"EMP-1020 | " + first + " | 5000 | Sales target Q3",
		"Adds an earning \"Incentive\" of ₹5,000 to EMP-1020's " + ymLabel(first) + " payslip, with the remark Sales target Q3.",
		"",
		"## What happens",
		"1. Upload the file in Bulk update › Incentives and press Check file. Each row shows the incentive it adds, or why it is refused. Nothing changes until you apply.",
		"2. A row is refused when the Employee ID is unknown, outside your access or pre-boarding; the month is already paid, locked, before the joining month, after the last working day or more than 12 months ahead; the amount is not a number above 0; or the same employee, month and remark is already loaded (also twice in the file), so uploading the file again is safe.",
		"3. Apply adds every good row as an \"Incentive\" earning in its month: on the payslip, the payroll sheet, the full salary register (Incentive column) and Upcoming pay lines. A wrong amount is put right with Payroll's adjustment on the person's row in the run (Other deductions or Other earnings).",
		"Up to 5,000 rows a file. Excel (.xlsx) or CSV.",
	}
}
