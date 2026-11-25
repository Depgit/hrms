package api

// PF for new joiners and the UAN check (owner's requests of 14 Nov 2026).
//
// PF exemption ("Gross > ₹25,000 and no UAN"; ₹20,000 until 19 Nov 2026): a joiner whose monthly gross is above the
// company's threshold (company_setting.pf_exempt_gross, default ₹25,000, Payroll settings) and who answered that they have no existing UAN
// (the employee field existing_uan, false; missing means unknown) is PF exempt: no employee or employer PF, EPS, EDLI
// or EPFO admin (calc.Input.PfExempt), and they are left out of the ECR and the PF enrolment file. The flag is stored on
// employee_pay (pf_exempt with its reason, date and who), shown on the employee's pay section, and decided by
// syncPfExempt: when the payroll is calculated (computeRows, for everyone in the run) and every 5 minutes by the
// maintenance sweep (joiners, salaries set, existing_uan answered). It is decided again while the person has no PF
// history (no earlier payroll row with PF): existing employees who were paid PF are never changed, and anyone with
// existing_uan true or a UAN on record is never exempt. Payroll or HR Admin can override it with a reason (manual: the
// automatic decision then leaves it alone, until set back to automatic); audited. With Basic at ₹15,000 or less the
// law may still require PF: a warning on the employee and in the payroll checklist, never a block.
//
// UAN check: in a payroll run, someone who is PF-applicable (PF worked out above 0, not exempt) with no UAN on record
// has the salary held, with the salary hold of Salary holds (placeHold, reason "UAN not provided", source "no-uan"),
// placed by System when the payroll is calculated. Once a UAN is on record (or the person is exempt, or the check is
// switched off), the next calculation releases it, and every held month already run is paid in the open month as
// "Held salary released, <month>" (releaseHold), as for the bank holds. Switchable (company_setting.uan_hold, default
// on). The Bank verification closure lists them under "UAN not provided" (not pending).
//
//	GET  /api/payroll/pf-settings        {rules, canEdit}
//	PUT  /api/payroll/pf-settings        {exemptGross?, uanHold?} (pay.setup 2)
//	GET  /api/employees/{id}/pf-exempt   the employee's PF status, the rule's answer and the Basic warning
//	PUT  /api/employees/{id}/pf-exempt   {mode: exempt | member | auto, reason} (Payroll at Edit, or HR Admin)
//	PUT  /api/employees/{id}/pf-choice   {choice: member | optout, reason} (Payroll at Edit, or HR Admin)
//
// PF choice (owner, 19 Nov 2026): a joiner with no existing UAN and a monthly gross above the threshold (now ₹25,000 by
// default) answers "Do you want PF deducted from your salary?" on the joining form. Yes is "member": a PF member on the
// normal structure. No is "optout": PF exempt (the rule above), and the whole fixed gross is paid as Basic (no HRA, no
// special allowance: employee_pay.all_basic, calc.Input.AllBasic) from all_basic_from. The choice is stored on
// employee_pay.pf_choice with who and when; HR changes it on the PF card (reason, audited, from the open payroll month).
// Nobody without the "optout" choice is paid any differently: no choice keeps the rule as it was.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	uanHoldWhy    = "UAN not provided"
	uanHoldSource = "no-uan"
	// pfBasicLimit: with Basic at or below it, PF may still be required by law for an exempt joiner.
	pfBasicLimit = 15000.0
)

// ---------- Settings ----------

type pfRules struct {
	ExemptGross float64 `json:"exemptGross"` // a joiner with no UAN and a gross above this is PF exempt
	UanHold     bool    `json:"uanHold"`     // hold the salary of a PF member with no UAN on record
}

func loadPfRules(ctx context.Context, q store.Q) pfRules {
	r := pfRules{ExemptGross: store.DefaultPfExemptGross, UanHold: store.DefaultUanHold}
	cs, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"pf_exempt_gross": 1, "uan_hold": 1}))
	if cs != nil {
		if v := cs["pf_exempt_gross"]; v != nil {
			r.ExemptGross = payengineFloat(v)
		}
		if v, ok := cs["uan_hold"].(bool); ok {
			r.UanHold = v
		}
	}
	return r
}

// pfSettings is GET /api/payroll/pf-settings.
func (s *Server) pfSettings(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.setup") < 1 && c.Level("pay.claims") < 1 && c.Role != "HR Admin" {
		return nil, forbidden("")
	}
	return map[string]any{"rules": loadPfRules(r.Context(), s.st.DB), "canEdit": c.Level("pay.setup") >= 2}, nil
}

// pfSettingsPut is PUT /api/payroll/pf-settings {exemptGross?, uanHold?}: only the fields sent change. Audited.
func (s *Server) pfSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		ExemptGross *float64 `json:"exemptGross"`
		UanHold     *bool    `json:"uanHold"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.ExemptGross == nil && b.UanHold == nil {
		return nil, bad("Nothing to save")
	}
	if b.ExemptGross != nil && (math.IsNaN(*b.ExemptGross) || *b.ExemptGross < 1000 || *b.ExemptGross > 1000000) {
		return nil, bad("The gross for the PF exemption is ₹1,000 to ₹10,00,000")
	}
	ctx := r.Context()
	var after pfRules
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before := loadPfRules(ctx, tx)
		set := bson.M{"updated_at": tx.Now()}
		after = before
		if b.ExemptGross != nil {
			after.ExemptGross = calc.Round2(*b.ExemptGross)
			set["pf_exempt_gross"] = after.ExemptGross
		}
		if b.UanHold != nil {
			after.UanHold = *b.UanHold
			set["uan_hold"] = after.UanHold
		}
		if after == before {
			return bad("Nothing changed")
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "PF settings updated", "pf_settings", "1", before, after)
	})
	return map[string]any{"rules": after}, err
}

// ---------- The exemption ----------

// existingUanVal reads the employee field existing_uan: true or false, also "Yes"/"No" text; nil when not answered.
func existingUanVal(v any) *bool {
	t, f := true, false
	switch x := v.(type) {
	case bool:
		if x {
			return &t
		}
		return &f
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "yes", "true", "y":
			return &t
		case "no", "false", "n":
			return &f
		}
	}
	return nil
}

// validUan: a 12-digit UAN on record.
func validUan(v any) bool { return reUAN.MatchString(statIDClean(str(v))) }

// pfFacts is what the exemption is decided on, for one employee.
type pfFacts struct {
	ID, Name, Type, Status, DOJ string
	Existing                    *bool   // existing_uan (nil: not answered)
	HasUan                      bool    // a valid UAN on record
	Gross, Basic                float64 // the monthly gross (the open month, or the joining month for a later joiner) and its Basic
	History                     bool    // paid PF in an earlier payroll month
	Choice                      string  // employee_pay.pf_choice: member, optout or "" (not asked)
	Pay                         store.Row
}

func pfFactsOf(ctx context.Context, q store.Q, id string) (*pfFacts, error) {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1, "type": 1, "status": 1, "doj": 1, "existing_uan": 1}))
	if err != nil || e == nil {
		return nil, notFound("Employee not found")
	}
	f := &pfFacts{ID: id, Name: str(e["name"]), Type: str(e["type"]), Status: str(e["status"]), DOJ: str(e["doj"]), Existing: existingUanVal(e["existing_uan"])}
	if g, _ := q.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id}); g != nil {
		f.HasUan = validUan(g["uan"])
		if f.Existing == nil {
			f.Existing = existingUanVal(g["existing_uan"])
		}
	}
	open := openYm(ctx, q)
	ym := open
	if len(f.DOJ) >= 7 && f.DOJ[:7] > ym {
		ym = f.DOJ[:7]
	}
	f.Gross, _ = grossFor(ctx, q, id, ym)
	f.Pay, _ = q.C("employee_pay").FindOne(ctx, bson.M{"employee_id": id})
	f.Choice = str(f.Pay["pf_choice"])
	_, st := structureFor(ctx, q, id, ym+"-01")
	f.Basic = calc.Round2(calc.Calc(calc.Input{Type: calc.EmploymentType(f.Type), Gross: f.Gross, Structure: &st, AllBasic: payOptsOf(f.Pay).allBasicIn(ym), AllIn: allInOn(ctx, q, ym)}).Basic)
	f.History, _ = q.C("payroll_row").Exists(ctx, bson.M{"employee_id": id, "ym": bson.M{"$lt": open}, "data.r.eePF": bson.M{"$gt": 0}})
	return f, nil
}

// payOptsOf reads the PF exemption and the "all Basic" flag of an employee_pay row (nil: neither).
func payOptsOf(r store.Row) payOptions {
	return payOptions{PfExempt: r["pf_exempt"] == true, AllBasic: r["all_basic"] == true, AllBasicFrom: str(r["all_basic_from"])}
}

// pfChoiceOpen: the PF choice may be made for these facts (no UAN, existing UAN answered No, gross above the
// threshold, PF applies to the type); "" when it may, else why not.
func pfChoiceOpen(f *pfFacts, rules pfRules) string {
	switch {
	case !calc.RulesFor(calc.EmploymentType(f.Type)).Statutory:
		return fmt.Sprintf("PF does not apply to %s staff", f.Type)
	case f.HasUan || (f.Existing != nil && *f.Existing):
		return "Has an existing UAN: stays a PF member"
	case f.Existing == nil:
		return "Whether they have an existing UAN is not recorded: record Existing UAN? first"
	case f.Gross <= rules.ExemptGross:
		return fmt.Sprintf("Monthly gross %s is not above %s: PF applies", calc.Inr(f.Gross), calc.Inr(rules.ExemptGross))
	}
	return ""
}

// pfRule is the automatic answer for the facts: exempt or not, and why.
func pfRule(f *pfFacts, rules pfRules) (bool, string) {
	switch {
	case !calc.RulesFor(calc.EmploymentType(f.Type)).Statutory:
		return false, fmt.Sprintf("PF does not apply to %s staff", f.Type)
	case f.HasUan:
		return false, "Existing PF member: a UAN is on record"
	case f.Existing == nil:
		return false, "Not exempt: whether they have an existing UAN is not recorded"
	case *f.Existing:
		return false, "Existing PF member: has an existing UAN"
	case f.Gross <= rules.ExemptGross:
		return false, fmt.Sprintf("Not exempt: monthly gross %s is not above %s", calc.Inr(f.Gross), calc.Inr(rules.ExemptGross))
	case f.Choice == store.PfChoiceMember:
		return false, "PF member by choice: wants PF deducted from the salary"
	case f.Choice == store.PfChoiceOptOut:
		return true, fmt.Sprintf("Chose no PF deduction: monthly gross %s is above %s and there is no existing UAN; paid as all Basic", calc.Inr(f.Gross), calc.Inr(rules.ExemptGross))
	}
	return true, fmt.Sprintf("Monthly gross %s is above %s and there is no existing UAN", calc.Inr(f.Gross), calc.Inr(rules.ExemptGross))
}

// pfBasicWarning is the legal warning for an exempt person whose Basic is ₹15,000 or less ("" when there is none).
func pfBasicWarning(exempt bool, basic float64) string {
	if !exempt || basic <= 0 || basic > pfBasicLimit {
		return ""
	}
	return fmt.Sprintf("PF exempt with Basic %s (₹15,000 or less): the law may still require PF. Check before the payroll run.", calc.Inr(basic))
}

// syncPfExempt decides the exemption of each employee again, unless it was set by hand, the person has left, or they
// were already paid PF (no change after the fact). A change is stored with its reason and audited as System.
func syncPfExempt(ctx context.Context, tx store.Q, ids []string) (int, error) {
	rules := loadPfRules(ctx, tx)
	n := 0
	for _, id := range ids {
		f, err := pfFactsOf(ctx, tx, id)
		if err != nil {
			continue
		}
		if f.Status == "Inactive" || (f.Pay != nil && f.Pay["pf_exempt_manual"] == true) || f.History {
			continue
		}
		want, why := pfRule(f, rules)
		cur := f.Pay != nil && f.Pay["pf_exempt"] == true
		if want == cur && (!want || str(f.Pay["pf_exempt_reason"]) == why) {
			continue
		}
		if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"pf_exempt": want, "pf_exempt_reason": why, "pf_exempt_at": tx.Now(),
			"pf_exempt_by": "System", "pf_exempt_manual": false}, nil); err != nil {
			return n, err
		}
		what := "PF exemption decided: exempt"
		if !want {
			what = "PF exemption removed"
		}
		if err := audit(ctx, tx, nil, what, "pf_exempt", id, map[string]any{"exempt": cur}, map[string]any{"exempt": want, "reason": why}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// pfCandidates are the people the exemption may concern: not left, and with existing_uan answered (on the employee or
// the government IDs), or exempt automatically now (so a raised gross or a UAN added takes it away). within, when not
// nil, narrows them to those ids. Everyone else is a member and stays one, so the rule is not worked out for them.
func pfCandidates(ctx context.Context, q store.Q, within []string) ([]string, error) {
	seen := map[string]bool{}
	ids := []string{}
	add := func(vals []any) {
		for _, v := range vals {
			if id := str(v); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	narrow := func(f bson.M, key string) bson.M {
		if within != nil {
			f[key] = bson.M{"$in": within}
		}
		return f
	}
	a, err := q.C("employee").Distinct(ctx, "_id", narrow(bson.M{"existing_uan": bson.M{"$ne": nil}, "status": bson.M{"$ne": "Inactive"}}, "_id"))
	if err != nil {
		return nil, err
	}
	add(a)
	b, err := q.C("employee_gov_id").Distinct(ctx, "employee_id", narrow(bson.M{"existing_uan": bson.M{"$ne": nil}}, "employee_id"))
	if err != nil {
		return nil, err
	}
	add(b)
	x, err := q.C("employee_pay").Distinct(ctx, "employee_id", narrow(bson.M{"pf_exempt": true, "pf_exempt_manual": bson.M{"$ne": true}}, "employee_id"))
	if err != nil {
		return nil, err
	}
	add(x)
	sort.Strings(ids)
	return ids, nil
}

// PfExemptSweep decides the exemption for everyone it may concern (pfCandidates). Run by the maintenance sweep.
func (s *Server) PfExemptSweep(ctx context.Context) (int, error) {
	ids, err := pfCandidates(ctx, s.st.DB, nil)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	n := 0
	err = s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		n, err = syncPfExempt(ctx, tx, ids)
		return err
	})
	return n, err
}

// syncPfExemptRun decides the exemption for the people paid in month ym (or the pilot) before their rows are calculated.
func syncPfExemptRun(ctx context.Context, tx store.Q, ym string, pilot []string) error {
	eligible, err := payEligible(ctx, tx, ym)
	if err != nil {
		return err
	}
	inRun, err := tx.C("employee").Distinct(ctx, "_id", payengineAnd(eligible, payrollPilotFilter(pilot)))
	if err != nil {
		return err
	}
	within := make([]string, 0, len(inRun))
	for _, id := range inRun {
		within = append(within, str(id))
	}
	ids, err := pfCandidates(ctx, tx, within)
	if err != nil || len(ids) == 0 {
		return err
	}
	_, err = syncPfExempt(ctx, tx, ids)
	return err
}

// pfCanEdit: Payroll (run rights at Edit) or HR Admin may override the exemption.
func pfCanEdit(c *Ctx) bool { return c.Level("pay.run") >= 2 || c.Role == "HR Admin" }

// pfExemptGet is GET /api/employees/{id}/pf-exempt.
func (s *Server) pfExemptGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("emp.salary") < 1 && c.Level("pay.run") < 1 && c.Role != "HR Admin" {
		return nil, forbidden("")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	return pfStatus(ctx, s.st.DB, c, id)
}

func pfStatus(ctx context.Context, q store.Q, c *Ctx, id string) (map[string]any, error) {
	f, err := pfFactsOf(ctx, q, id)
	if err != nil {
		return nil, err
	}
	rules := loadPfRules(ctx, q)
	ruleExempt, ruleWhy := pfRule(f, rules)
	exempt := f.Pay != nil && f.Pay["pf_exempt"] == true
	out := map[string]any{"employeeId": id, "exempt": exempt, "manual": f.Pay != nil && f.Pay["pf_exempt_manual"] == true, "reason": nil, "by": nil, "at": nil,
		"rule": map[string]any{"exempt": ruleExempt, "reason": ruleWhy}, "existingUan": f.Existing, "hasUan": f.HasUan, "history": f.History,
		"statutory": calc.RulesFor(calc.EmploymentType(f.Type)).Statutory, "threshold": rules.ExemptGross, "uanHold": rules.UanHold,
		"warning": pfBasicWarning(exempt, f.Basic), "canEdit": pfCanEdit(c) && f.Status != "Inactive"}
	if f.Pay != nil && f.Pay["pf_exempt_at"] != nil {
		out["reason"], out["by"], out["at"] = str(f.Pay["pf_exempt_reason"]), str(f.Pay["pf_exempt_by"]), payengineStamp(f.Pay["pf_exempt_at"], "02 Jan 2006 15:04")
	}
	// The PF choice (owner, 19 Nov 2026): what was chosen, by whom, and whether the "all Basic" pay is in force.
	po := payOptsOf(f.Pay)
	choice := map[string]any{"value": f.Choice, "by": nil, "at": nil, "reason": nil, "allBasic": po.allBasicIn(openYm(ctx, q)), "from": po.AllBasicFrom,
		"open": pfChoiceOpen(f, rules) == "", "why": pfChoiceOpen(f, rules)}
	if f.Pay != nil && f.Pay["pf_choice_at"] != nil {
		choice["by"], choice["at"], choice["reason"] = str(f.Pay["pf_choice_by"]), payengineStamp(f.Pay["pf_choice_at"], "02 Jan 2006 15:04"), str(f.Pay["pf_choice_reason"])
	}
	out["choice"] = choice
	if f.Choice == store.PfChoiceOptOut && calc.RulesFor(calc.EmploymentType(f.Type)).Statutory && f.Gross > 0 && f.Gross <= rules.ExemptGross {
		out["retainer"] = pfRetainerNote(rules.ExemptGross) // chose no PF on a gross up to the threshold (pf_offer.go)
	}
	if c.Show("salary", 1, id) != nil {
		out["gross"], out["basic"] = f.Gross, f.Basic
	}
	return out, nil
}

// pfExemptPut is PUT /api/employees/{id}/pf-exempt {mode, reason}: exempt or member by hand (the automatic decision
// then leaves it alone), or auto (the rule decides again). A reason of at least 5 characters; audited.
func (s *Server) pfExemptPut(r *http.Request, c *Ctx) (any, error) {
	if !pfCanEdit(c) {
		return nil, forbidden("Payroll or HR Admin sets the PF exemption")
	}
	var b struct {
		Mode   string `json:"mode"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if b.Mode != "exempt" && b.Mode != "member" && b.Mode != "auto" {
		return nil, bad("Choose PF exempt, PF member or decided by the rule")
	}
	if len(b.Reason) < 5 {
		return nil, bad("Give the reason for the change")
	}
	ctx, id := r.Context(), r.PathValue("id")
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if err := c.MustInScope(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": id}); err != nil {
			return err
		}
		f, err := pfFactsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if f.Status == "Inactive" {
			return bad("The PF exemption cannot change after the employee has left")
		}
		if !calc.RulesFor(calc.EmploymentType(f.Type)).Statutory {
			return bad("PF does not apply to %s staff", f.Type)
		}
		before := map[string]any{"exempt": f.Pay != nil && f.Pay["pf_exempt"] == true, "manual": f.Pay != nil && f.Pay["pf_exempt_manual"] == true}
		if b.Mode == "auto" {
			if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"pf_exempt_manual": false}, nil); err != nil {
				return err
			}
			want, why := pfRule(f, loadPfRules(ctx, tx))
			if f.History { // paid PF already: the rule does not change it after the fact
				want, why = before["exempt"].(bool), "Decided by the rule; already paid PF, so it stays as it is"
			}
			if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"pf_exempt": want, "pf_exempt_reason": why, "pf_exempt_at": tx.Now(),
				"pf_exempt_by": c.Name + " (back to the rule: " + b.Reason + ")"}, nil); err != nil {
				return err
			}
		} else {
			want := b.Mode == "exempt"
			why := "Set by hand: PF member. " + b.Reason
			if want {
				why = "Set by hand: PF exempt. " + b.Reason
			}
			if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"pf_exempt": want, "pf_exempt_reason": why, "pf_exempt_at": tx.Now(),
				"pf_exempt_by": c.Name, "pf_exempt_manual": true}, nil); err != nil {
				return err
			}
		}
		if out, err = pfStatus(ctx, tx, c, id); err != nil {
			return err
		}
		return audit(ctx, tx, c, "PF exemption changed by hand", "pf_exempt", id, before, map[string]any{"mode": b.Mode, "exempt": out["exempt"], "reason": b.Reason})
	})
	return out, err
}

// ---------- The PF choice ----------

// pfChoiceWord reads the joining form's answer to "Do you want PF deducted from your salary?": Yes is member, No is
// optout ("" for anything else).
func pfChoiceWord(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "y", store.PfChoiceMember:
		return store.PfChoiceMember
	case "no", "n", store.PfChoiceOptOut:
		return store.PfChoiceOptOut
	}
	return ""
}

// setPfChoice stores the PF choice with who, when and why, and the "all Basic" flag (optout) from month from.
func setPfChoice(ctx context.Context, tx store.Q, id, choice, by, reason, from string) error {
	_, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"pf_choice": choice, "pf_choice_by": by, "pf_choice_at": tx.Now(),
		"pf_choice_reason": reason, "all_basic": choice == store.PfChoiceOptOut, "all_basic_from": from}, nil)
	return err
}

// pfChoiceFrom is the first month a choice made now applies to: the open payroll month, or the joining month when later.
func pfChoiceFrom(ctx context.Context, q store.Q, doj string) string {
	from := openYm(ctx, q)
	if len(doj) >= 7 && doj[:7] > from {
		from = doj[:7]
	}
	return from
}

// applyJoinPfChoice records the PF answer in force (the joining form's, else the offer's; pf_offer.go) on the
// employee when it was asked (no existing UAN and a gross above the threshold, or the offer said No) and decides the
// exemption again. An unchanged answer changes nothing. A No on a gross up to the threshold is recorded (PF still
// applies by the rule) and HR is told the type must be Retainer.
func applyJoinPfChoice(ctx context.Context, tx store.Q, empID, cand string, d map[string]string) error {
	choice := pfAnswer(d)
	rules := loadPfRules(ctx, tx)
	if choice == "" || !(pfChoiceAsked(d, rules.ExemptGross) || pfOfferNo(d)) {
		return nil
	}
	e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": empID}, options.FindOne().SetProjection(bson.M{"doj": 1, "type": 1}))
	if !calc.RulesFor(calc.EmploymentType(str(e["type"]))).Statutory { // a Retainer: no PF, the answer is ignored
		return nil
	}
	p, _ := tx.C("employee_pay").FindOne(ctx, bson.M{"employee_id": empID}, options.FindOne().SetProjection(bson.M{"pf_choice": 1}))
	was := str(p["pf_choice"])
	if was == choice {
		return nil
	}
	by, why := pfAnswerSource(d)
	if err := setPfChoice(ctx, tx, empID, choice, by, why, pfChoiceFrom(ctx, tx, str(e["doj"]))); err != nil {
		return err
	}
	if err := audit(ctx, tx, nil, "PF choice recorded from the "+strings.ToLower(by), "pf_exempt", empID, map[string]any{"choice": was}, map[string]any{"choice": choice, "reason": why}); err != nil {
		return err
	}
	if err := lifeEvent(ctx, tx, nil, lifeEv{Subject: "employee", Cand: cand, Emp: empID, Action: "PF choice recorded", From: pfWantText(was), To: pfWantText(choice), Reason: why}); err != nil {
		return err
	}
	if _, err := syncPfExempt(ctx, tx, []string{empID}); err != nil {
		return err
	}
	if choice != store.PfChoiceOptOut {
		return nil
	}
	f, err := pfFactsOf(ctx, tx, empID)
	if err != nil || f.Gross <= 0 || f.Gross > rules.ExemptGross {
		return err
	}
	return pfRetainerAlert(ctx, tx, f, rules.ExemptGross)
}

// pfChoicePut is PUT /api/employees/{id}/pf-choice {choice: member | optout, reason}: HR changes the PF choice
// (Payroll at Edit or HR Admin, a reason of at least 5 characters, audited). It applies from the open payroll month:
// optout makes the person PF exempt and paid as all Basic; member makes them a PF member on the normal structure.
// The exemption goes back to the rule (any setting by hand is replaced by the choice).
func (s *Server) pfChoicePut(r *http.Request, c *Ctx) (any, error) {
	if !pfCanEdit(c) {
		return nil, forbidden("Payroll or HR Admin changes the PF choice")
	}
	var b struct {
		Choice string `json:"choice"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if b.Choice != store.PfChoiceMember && b.Choice != store.PfChoiceOptOut {
		return nil, bad("Choose PF deducted (member) or no PF (all Basic)")
	}
	if len(b.Reason) < 5 {
		return nil, bad("Give the reason for the change")
	}
	ctx, id := r.Context(), r.PathValue("id")
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if err := c.MustInScope(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": id}); err != nil {
			return err
		}
		f, err := pfFactsOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if f.Status == "Inactive" {
			return bad("The PF choice cannot change after the employee has left")
		}
		rules := loadPfRules(ctx, tx)
		// Back to PF from a No recorded outside the usual case (a gross up to the threshold; pf_offer.go) is allowed.
		if why := pfChoiceOpen(f, rules); why != "" && !(b.Choice == store.PfChoiceMember && f.Choice == store.PfChoiceOptOut) {
			return bad("%s", why)
		}
		if b.Choice == store.PfChoiceOptOut && f.History {
			return bad("Already paid PF in an earlier month: a PF member cannot stop PF")
		}
		if f.Choice == b.Choice {
			return bad("Nothing changed")
		}
		before := map[string]any{"choice": f.Choice, "exempt": f.Pay != nil && f.Pay["pf_exempt"] == true, "allBasic": payOptsOf(f.Pay).AllBasic}
		if err := setPfChoice(ctx, tx, id, b.Choice, c.Name, b.Reason, pfChoiceFrom(ctx, tx, f.DOJ)); err != nil {
			return err
		}
		f.Choice = b.Choice
		want, why := pfRule(f, rules)
		if _, err := tx.C("employee_pay").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"pf_exempt": want, "pf_exempt_reason": why, "pf_exempt_at": tx.Now(),
			"pf_exempt_by": c.Name + " (PF choice: " + b.Reason + ")", "pf_exempt_manual": false}, nil); err != nil {
			return err
		}
		if out, err = pfStatus(ctx, tx, c, id); err != nil {
			return err
		}
		if err := lifeEvent(ctx, tx, c, lifeEv{Subject: "employee", Emp: id, Action: "PF choice changed", From: pfWantText(str(before["choice"])), To: pfWantText(b.Choice),
			Reason: b.Reason}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "PF choice changed", "pf_exempt", id, before, map[string]any{"choice": b.Choice, "exempt": want, "reason": b.Reason})
	})
	return out, err
}

// ---------- Who is PF-applicable, in bulk ----------

// pfFigure is one person's PF position in a month.
type pfFigure struct {
	Gross, Basic, EePF float64
	Exempt             bool
	HasUan             bool
}

// pfFigures works out, for the people listed, the month's gross and Basic, the PF on it (0 for exempt or non-statutory
// people), the exemption and whether a UAN is on record: a few queries for everyone.
func pfFigures(ctx context.Context, q store.Q, ym string, list []*emp) (map[string]*pfFigure, error) {
	out := map[string]*pfFigure{}
	if len(list) == 0 {
		return out, nil
	}
	ids := ids(list)
	in := bson.M{"$in": ids}
	hist := map[string][]grossPt{}
	gross, err := q.C("gross_history").Find(ctx, bson.M{"employee_id": in}, options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range gross {
		id := str(r["employee_id"])
		hist[id] = append(hist[id], grossPt{From: str(r["from_month"]), Date: str(r["from_date"]), Gross: payengineFloat(r["gross"])})
	}
	structs := map[string]calc.Structure{}
	def := ""
	sts, err := q.C("salary_structure").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range sts {
		var st calc.Structure
		_ = payengineJSON(r["rates"], &st)
		structs[str(r["id"])] = st
		if r["is_default"] == true {
			def = str(r["id"])
		}
	}
	smap := map[string]string{}
	first := ym + "-01"
	maps, err := q.C("structure_map").Find(ctx, bson.M{"employee_id": in, "from_date": bson.M{"$lte": first}}, options.Find().SetSort(bson.D{{Key: "from_date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range maps {
		smap[str(r["employee_id"])] = str(r["structure_id"])
	}
	exempt, allBasic := map[string]bool{}, map[string]bool{}
	pays, err := q.C("employee_pay").Find(ctx, bson.M{"employee_id": in, "pf_exempt": true})
	if err != nil {
		return nil, err
	}
	for _, r := range pays {
		exempt[str(r["employee_id"])] = true
		allBasic[str(r["employee_id"])] = payOptsOf(r).allBasicIn(ym)
	}
	uan := map[string]bool{}
	govs, err := q.C("employee_gov_id").Find(ctx, bson.M{"employee_id": in}, options.Find().SetProjection(bson.M{"employee_id": 1, "uan": 1}))
	if err != nil {
		return nil, err
	}
	for _, g := range govs {
		uan[str(g["employee_id"])] = validUan(g["uan"])
	}
	allIn := allInOn(ctx, q, ym) // the all-inclusive gross (allin.go)
	for _, e := range list {
		g := grossInMonth(hist[e.ID], ym)
		sid := smap[e.ID]
		if sid == "" {
			sid = def
		}
		st, ok := structs[sid]
		if !ok {
			st = calc.DefaultStructure
		}
		res := calc.Calc(calc.Input{Type: calc.EmploymentType(e.Type), Gross: g, Structure: &st, PfExempt: exempt[e.ID], AllBasic: allBasic[e.ID], AllIn: allIn})
		out[e.ID] = &pfFigure{Gross: g, Basic: calc.Round2(res.Basic), EePF: res.EePF, Exempt: exempt[e.ID], HasUan: uan[e.ID]}
	}
	return out, nil
}

// uanMissingIn lists the people of list who pay PF in month ym but have no UAN on record, in list order.
func uanMissingIn(ctx context.Context, q store.Q, ym string, list []*emp) ([]*emp, error) {
	figs, err := pfFigures(ctx, q, ym, list)
	if err != nil {
		return nil, err
	}
	out := []*emp{}
	for _, e := range list {
		if f := figs[e.ID]; f != nil && f.EePF > 0 && !f.HasUan {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---------- The UAN hold ----------

// releaseUanHolds releases the holds placed for a missing UAN once a UAN is on record, the person is PF exempt, or the
// check is switched off: every held month already run is paid in the open month as "Held salary released, <month>".
func releaseUanHolds(ctx context.Context, tx store.Q) error {
	holds, err := tx.C("salary_hold").Find(ctx, bson.M{"status": "Active", "source_id": uanHoldSource}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil || len(holds) == 0 {
		return err
	}
	on := loadPfRules(ctx, tx).UanHold
	for _, h := range holds {
		emp := str(h["employee_id"])
		note := ""
		g, _ := tx.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": emp}, options.FindOne().SetProjection(bson.M{"uan": 1}))
		p, _ := tx.C("employee_pay").FindOne(ctx, bson.M{"employee_id": emp}, options.FindOne().SetProjection(bson.M{"pf_exempt": 1}))
		switch {
		case g != nil && validUan(g["uan"]):
			u := statIDClean(str(g["uan"]))
			note = "UAN ending " + u[len(u)-4:] + " on record"
		case p != nil && p["pf_exempt"] == true:
			note = "PF exempt: no UAN needed"
		case !on:
			note = "The UAN check is switched off"
		default:
			continue
		}
		if _, err := releaseHold(ctx, tx, "System", str(h["id"]), note); err != nil {
			return err
		}
	}
	return nil
}

// placeUanHolds holds, from month ym, the salary of everyone in the run (or the pilot) who pays PF with no UAN on
// record and has no hold yet. Nothing while the check is switched off.
func placeUanHolds(ctx context.Context, tx store.Q, ym string, pilot []string) error {
	if !loadPfRules(ctx, tx).UanHold {
		return nil
	}
	eligible, err := payEligible(ctx, tx, ym)
	if err != nil {
		return err
	}
	list, err := loadEmps(ctx, tx, payengineAnd(eligible, payrollPilotFilter(pilot)))
	if err != nil {
		return err
	}
	missing, err := uanMissingIn(ctx, tx, ym, list)
	if err != nil {
		return err
	}
	for _, e := range missing {
		if held, _ := tx.C("salary_hold").Exists(ctx, bson.M{"employee_id": e.ID, "status": "Active"}); held {
			continue // already held; the next run holds it for the UAN if that hold is released first
		}
		if _, err := placeHold(ctx, tx, "System", e.ID, uanHoldWhy, "Placed by the payroll run for "+ymLabel(ym)+": PF applies and no UAN is on record. "+
			"Released automatically at the next payroll calculation once the UAN is entered.", ym, uanHoldSource); err != nil {
			return err
		}
	}
	return nil
}

// uanChecklist lists, for the Bank verification closure, the people in the run who pay PF with no UAN on record: their
// salary is (or will be, at the calculation) held for it. Not pending: they do not stop the closure from closing.
func uanChecklist(ctx context.Context, q store.Q, ym string, list []*emp) ([]*closureItem, error) {
	missing, err := uanMissingIn(ctx, q, ym, list)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	on := loadPfRules(ctx, q).UanHold
	holds, err := q.C("salary_hold").Find(ctx, bson.M{"status": "Active", "employee_id": bson.M{"$in": ids(missing)}})
	if err != nil {
		return nil, err
	}
	holdOf := map[string]store.Row{}
	for _, h := range holds {
		holdOf[str(h["employee_id"])] = h
	}
	out := []*closureItem{}
	for _, e := range missing {
		it := &closureItem{closure: "bank", key: e.ID + "/uan", Group: uanHoldWhy, EmployeeID: e.ID, Name: e.Name, What: "UAN not provided (PF applies)",
			WaitsOn: e.Name, WaitsOnID: e.ID, WaitsAs: "Employee"}
		h := holdOf[e.ID]
		switch {
		case h != nil:
			it.HoldID, it.When = str(h["id"]), holdMonths(str(h["from_ym"]), str(h["to_ym"]))
			it.Note = "Salary on hold: " + str(h["reason"]) + ". Released at the next payroll calculation once a UAN is entered."
		case on:
			it.Note = "The salary is held when payroll is calculated, and released at the next calculation once a UAN is entered."
		default:
			it.Note = "The UAN check is switched off in Payroll settings: the salary is not held."
		}
		it.Ref = ym + ":bank:" + it.key
		out = append(out, it)
	}
	return out, nil
}

// pfBasicWarnings is the checklist's warning about PF exempt people in the run whose Basic is ₹15,000 or less.
func pfBasicWarnings(ctx context.Context, q store.Q, ym string, list []*emp) (string, error) {
	figs, err := pfFigures(ctx, q, ym, list)
	if err != nil {
		return "", err
	}
	names := []string{}
	for _, e := range list {
		if f := figs[e.ID]; f != nil && pfBasicWarning(f.Exempt, f.Basic) != "" {
			names = append(names, e.Name+" ("+e.ID+")")
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	shown := names
	if len(shown) > 8 {
		shown = append(append([]string{}, names[:8]...), fmt.Sprintf("%d more", len(names)-8))
	}
	return fmt.Sprintf("PF exempt with Basic ≤ ₹15,000: check %s. The law may still require PF for them; the exemption is on each employee's pay section (Payroll or HR Admin can change it).",
		strings.Join(shown, ", ")), nil
}
