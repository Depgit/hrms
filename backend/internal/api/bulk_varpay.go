package api

// Variable payouts in Bulk update › Salary breakup (pay components, paycomp.go; 17 Nov 2026). The separate template
// "Variable payouts" was merged into Salary breakup on the owner's request (17 Nov 2026): the sheet gains four optional
// columns (Payout Month, Payout Component, Variable Payout (₹), Payout Note). A row may change the salary, give a
// variable payout, or both; a blank Variable Payout (₹) gives no payout. A payout is the variable amount to pay a person
// for a component of a structure made from components, in a month the component's payout is due. It is what
// Payroll › Variable pay enters one by one: stored per person, month and component (loading the same row again
// replaces the amount), paid in that month's run. A due component with no amount is not paid and is listed in the
// payroll checks.
//
// Each part keeps its rights and rules: salary columns need Bulk update and Salary at Edit (as before); payouts need
// Payroll (run or claims at Edit) or HR Admin. The payout is checked before anything in the row is written, so a row
// with a problem in either part writes nothing.

import (
	"context"
	"fmt"
	"strings"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	payMonthCol = "Payout Month"
	payCompCol  = "Payout Component"
	payAmtCol   = "Variable Payout (₹)"
	payNoteCol  = "Payout Note"
)

var payCols = []string{payMonthCol, payCompCol, payAmtCol, payNoteCol}

// salAllowed is the Salary breakup's own right (salary columns).
func salAllowed(c *Ctx) bool { return c.Level("bulk.update") >= 2 && c.Level("emp.salary") >= 2 }

func init() {
	for _, t := range bulkTemplates {
		if t.Key != "sal" {
			continue
		}
		t.Cols = append(t.Cols, payCols...)
		t.Optional = append(t.Optional, payCols...)
		t.allow = func(c *Ctx) bool { return salAllowed(c) || varPayAllowed(c) }
		t.Hint += " Variable payouts: give Payout Month, Payout Component and Variable Payout (₹) (and a Payout Note if you like) for the variable amount to pay in a month a component of the person's structure is due; a blank Variable Payout (₹) pays nothing new. A row may change the salary, give a payout, or both. Payouts need Payroll (run or claims at Edit) or HR Admin."
		t.how = salPayHow
		t.row = bulkSalPay
	}
}

// salColsGiven: any salary column of the row filled (the columns before the payout ones, Employee ID aside).
func salColsGiven(t *bulkTemplate, r bulkRow) bool {
	for _, col := range t.Cols {
		if col != "Employee ID" && !containsStr(payCols, col) && strings.TrimSpace(r.V[col]) != "" {
			return true
		}
	}
	return false
}

// bulkSalPay is a Salary breakup row: the salary part (bulkSal) and the variable payout part.
func bulkSalPay(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	t, _ := templateFor(c, "sal")
	sal := t != nil && salColsGiven(t, r)
	if strings.TrimSpace(r.V[payAmtCol]) == "" {
		if salAllowed(c) {
			return bulkSal(s, ctx, tx, c, r, apply)
		}
		var res bulkResult
		if _, ok := bulkEmployee(ctx, tx, c, r, &res); ok && sal {
			res.Errors = append(res.Errors, "Salary columns need Bulk update and Salary at Edit: leave them blank")
		}
		return res, nil
	}
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	if !varPayAllowed(c) {
		res.Errors = append(res.Errors, "Variable payouts need Payroll (run or claims at Edit) or HR Admin")
		return res, nil
	}
	if sal && !salAllowed(c) {
		res.Errors = append(res.Errors, "Salary columns need Bulk update and Salary at Edit: leave them blank")
		return res, nil
	}
	p, ok := varPayRead(ctx, tx, r, e, &res)
	if !ok {
		return res, nil
	}
	if sal {
		sres, err := bulkSal(s, ctx, tx, c, r, apply)
		if err != nil || len(sres.Errors) > 0 {
			return sres, err
		}
		res = sres
	}
	var before, beforeNote string
	if old, _ := tx.C(store.VarPayoutColl).FindOne(ctx, map[string]any{"employee_id": res.ID, "ym": p.ym, "code": p.code}); old != nil {
		before, beforeNote = calc.Inr(payengineFloat(old["amount"])), str(old["note"])
	}
	if p.note == "" {
		p.note = beforeNote // a blank Payout Note keeps the note (the download leaves it blank)
	}
	if before == calc.Inr(p.amt) && beforeNote == p.note {
		return res, nil // the same amount again: unchanged
	}
	if _, err := setVarPayout(ctx, tx, c, res.ID, p.ym, p.code, &p.amt, p.note); err != nil {
		if ae, ok := err.(*apiError); ok {
			res.Errors = append(res.Errors, ae.Message)
			return res, nil
		}
		return res, err
	}
	res.Changes = append(res.Changes, bulkChange{Field: p.name + " in " + ymLabel(p.ym), From: before, To: calc.Inr(p.amt), Note: p.note})
	return res, nil
}

type varPayLine struct {
	ym, code, name, note string
	amt                  float64
}

// varPayRead checks a row's payout columns without writing anything: the checks setVarPayout makes, and the
// component by its name or code (blank when the structure has one variable component).
func varPayRead(ctx context.Context, tx store.Q, r bulkRow, e store.Row, res *bulkResult) (varPayLine, bool) {
	var p varPayLine
	if str(e["status"]) == "Pre-boarding" {
		res.Errors = append(res.Errors, res.Name+" is pre-boarding (not joined yet)")
		return p, false
	}
	if !calc.RulesFor(calc.EmploymentType(str(e["type"]))).Variable {
		res.Errors = append(res.Errors, fmt.Sprintf("%s staff do not earn variable pay", e["type"]))
		return p, false
	}
	ym, okM := openMonth(strings.TrimSpace(r.V[payMonthCol]))
	if !okM {
		res.Errors = append(res.Errors, payMonthCol+" must be a month, like 2026-12 or Dec-2026")
	}
	amt, okA := openAmount(r.V[payAmtCol])
	if !okA || amt < 0 || amt > 1e7 {
		res.Errors = append(res.Errors, payAmtCol+" must be a number, 0 or more (at most ₹1,00,00,000)")
	}
	note := strings.Join(strings.Fields(r.V[payNoteCol]), " ")
	if len([]rune(note)) > 200 {
		res.Errors = append(res.Errors, payNoteCol+" is longer than 200 characters")
	}
	if note != "" && strings.ContainsAny(note[:1], "=+-@") {
		res.Errors = append(res.Errors, payNoteCol+" cannot start with = + - or @")
	}
	if len(res.Errors) > 0 {
		return p, false
	}
	_, st := structureFor(ctx, tx, res.ID, ym+"-01")
	want := strings.ToLower(strings.TrimSpace(r.V[payCompCol]))
	var comp *calc.Component
	vars := 0
	if st.Designed {
		for _, x := range calc.ComponentsOf(st) {
			if x.Kind != calc.CompVariable || x.Method == calc.MLegacyVar {
				continue
			}
			vars++
			if want == "" || strings.ToLower(x.Name) == want || strings.ToLower(x.Code) == want {
				x := x
				comp = &x
			}
		}
	}
	switch {
	case want == "" && vars > 1:
		res.Errors = append(res.Errors, fmt.Sprintf("%s's salary structure has %d variable components: give the %s", res.Name, vars, payCompCol))
	case comp == nil:
		res.Errors = append(res.Errors, fmt.Sprintf("“%s” is not a variable component of %s's salary structure", r.V[payCompCol], res.Name))
	case !calc.VarDue(ym, varAnchorFor(ctx, tx, res.ID), comp.Cycle):
		res.Errors = append(res.Errors, fmt.Sprintf("%s is not due in %s: it is paid %s", comp.Name, ymLabel(ym), varDueWord(varAnchorFor(ctx, tx, res.ID), ym, comp.Cycle)))
	case ym < openYm(ctx, tx):
		open := openYm(ctx, tx)
		res.Errors = append(res.Errors, fmt.Sprintf("%s is already paid and closed. Enter amounts for %s or later", ymLabel(ym), ymLabel(open)))
	case amt > 0:
		if msg := varNoTarget(ctx, tx, res.ID, res.Name, ym, *comp); msg != "" {
			res.Errors = append(res.Errors, msg)
		}
	}
	if len(res.Errors) > 0 {
		return p, false
	}
	return varPayLine{ym: ym, code: comp.Code, name: comp.Name, note: note, amt: amt}, true
}

// salPayoutRows is the payouts due in the open payroll month for the current-data download, by employee: one
// [month, component, amount entered, note] line per due component, and the target for the cycle.
func salPayoutRows(ctx context.Context, s *Server, c *Ctx) (map[string][][]any, map[string]string, error) {
	q := s.st.DB
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, nil, err
	}
	ym := openYm(ctx, q)
	rows, err := varDueRows(ctx, q, ym, scope)
	if err != nil {
		return nil, nil, err
	}
	out, names := map[string][][]any{}, map[string]string{}
	for _, x := range rows {
		id := str(x["employeeId"])
		names[id] = str(x["name"])
		for _, l := range x["comps"].([]map[string]any) {
			if l["due"] != true || l["noTarget"] == true {
				continue
			}
			amt := any("")
			if l["paid"] != nil {
				amt = l["paid"]
			}
			out[id] = append(out[id], []any{ym, l["name"], amt, "", l["cycleTarget"]})
		}
	}
	return out, names, nil
}

func salPayHow(ctx context.Context, q store.Q) []string {
	open := openYm(ctx, q)
	return []string{
		"## Variable payouts (optional columns)",
		"Payout Month: the payroll month the amount is paid in, as 2026-12, Dec-2026 or 12/2026. The open payroll month is " + ymLabel(open) + ".",
		"Payout Component: the variable component's name or code, as in the person's salary structure (Compensation › Pay components). Blank works when the structure has one variable component.",
		"Variable Payout (₹): the amount to pay, 0 or more. 0 records that nothing is paid this cycle (the payroll check then counts it as entered). Blank: no payout in this row.",
		"Payout Note: optional, up to 200 characters, for example \"Q3 rating 4/5\".",
		"A row may change the salary, give a payout, or both. For a second due component of the same person, add a row with only the Employee ID and the payout columns.",
		"",
		"## When a component is due",
		"The cycle is counted from the person's joining month (or the month their variable pay started) and the payout falls when a cycle is complete. Monthly components every month; quarterly every 3 months (joined in May: July, October, January, April); half-yearly every 6 months (joined in May: October, April); yearly every 12 months (joined in May: April). A payout for a month the component is not due in is refused, and so is a payout for someone with no variable target (none or 0).",
		"",
		"## What happens",
		"1. Download the current data: the payout columns are filled for each component due in " + ymLabel(open) + " (one row each), with its target for the cycle in the last column. Fill Variable Payout (₹) and upload, then Check file.",
		"2. Apply stores the amounts. They are paid in that month's run (run the dry run again) as earnings on the payslip, by the component's name, and count as taxable salary when the component is taxable. Payroll › Variable pay shows them.",
		"3. A due component with no amount is not paid; the payroll checks list who, before approval.",
	}
}
