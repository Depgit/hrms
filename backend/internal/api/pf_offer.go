package api

// PF on the offer (owner, 26 Nov 2026). HR records the candidate's answer "Does the employee want PF? Yes / No" on
// the offer form (offer.data.pfWant, required for a type PF applies to, no default; hidden and ignored for a Retainer
// or any type without PF):
//
//   - No, offered gross above company_setting.pf_exempt_gross: the offer's breakup and letter are the whole gross as
//     Basic with no PF (employee or employer), as calc.Input AllBasic + PfExempt; at Mark joined the employee gets
//     pf_choice optout, all_basic and PF exempt (applyJoinPfChoice).
//   - No, gross up to the limit: the offer is refused (check, raise and release) with pfRetainerMsg.
//   - Yes: the normal structure; under the all-inclusive gross both PF shares come out of the salary (calc/allin.go).
//
// The offer's answer is copied to data.pfChoice, the joining form's question "Do you want PF deducted from your
// salary?", which is then asked whenever the offer said No (and the joiner has no existing UAN), or as before (no
// UAN, gross above the limit), prefilled with the offer's answer. The joining form's answer wins. A joiner's No with
// a gross up to the limit is not blocked: it is recorded, HR gets a bell and the PF card says the type must change to
// Retainer (PF still applies by the rule until then). Every answer and change is audited and on the timeline.

import (
	"context"
	"fmt"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// pfAnswer is the PF answer in force on an offer's data (member, optout or ""): the joining form's (pfChoice), else
// the offer's (pfWant).
func pfAnswer(d map[string]string) string {
	if v := pfChoiceWord(d["pfChoice"]); v != "" {
		return v
	}
	return pfChoiceWord(d["pfWant"])
}

// pfYesNo is the form word for a choice: Yes (member), No (optout), "" otherwise.
func pfYesNo(choice string) string {
	switch pfChoiceWord(choice) {
	case store.PfChoiceMember:
		return "Yes"
	case store.PfChoiceOptOut:
		return "No"
	}
	return ""
}

// pfWantText is the timeline wording of an answer.
func pfWantText(v string) string {
	switch pfChoiceWord(v) {
	case store.PfChoiceMember:
		return "Yes: PF deducted (Basic, HRA, special allowance)"
	case store.PfChoiceOptOut:
		return "No: no PF, the whole gross as Basic"
	}
	return ""
}

// pfOfferNo: the offer said No and the joiner has not declared an existing UAN (the joining form then asks again).
func pfOfferNo(d map[string]string) bool {
	return pfChoiceWord(d["pfWant"]) == store.PfChoiceOptOut && d["uanAvail"] != "Yes"
}

// offerPfOptOut: the offer's pay is the whole gross as Basic with no PF (the answer in force is No, no existing UAN,
// the gross above the limit).
func offerPfOptOut(d map[string]string, gross, limit float64) bool {
	return pfAnswer(d) == store.PfChoiceOptOut && d["uanAvail"] != "Yes" && limit > 0 && gross > limit
}

// pfRetainerMsg refuses an offer whose candidate does not want PF on a gross up to the limit.
func pfRetainerMsg(limit float64) string {
	return fmt.Sprintf("Gross is up to %s: an employee who does not want PF must be a Retainer. Change the employment type to Retainer.", calc.Inr(limit))
}

// offerPfProblems adds the PF answer's problems for an offer of type typ and monthly gross.
func offerPfProblems(ctx context.Context, q store.Q, typ string, gross float64, d map[string]string, errs *[]string) {
	if typ == "" || !calc.RulesFor(calc.EmploymentType(typ)).Statutory {
		return
	}
	switch pfChoiceWord(d["pfWant"]) {
	case "":
		*errs = append(*errs, "Does the employee want PF? Choose Yes or No")
	case store.PfChoiceOptOut:
		if lim := loadPfRules(ctx, q).ExemptGross; gross > 0 && gross <= lim {
			*errs = append(*errs, pfRetainerMsg(lim))
		}
	}
}

// offerPfData sets the offer's answer as saved: "Yes"/"No" in pfWant and copied to pfChoice (the joining form's
// question, prefilled); nothing for a type without PF.
func offerPfData(typ string, d map[string]string) {
	if !calc.RulesFor(calc.EmploymentType(typ)).Statutory {
		delete(d, "pfWant")
		delete(d, "pfChoice")
		return
	}
	if w := pfYesNo(d["pfWant"]); w != "" {
		d["pfWant"], d["pfChoice"] = w, w
	}
}

// offerPfRelease refuses releasing an offer whose candidate does not want PF on a gross up to the limit.
func offerPfRelease(ctx context.Context, q store.Q, o store.Row) error {
	if !calc.RulesFor(calc.EmploymentType(str(o["type"]))).Statutory || pfChoiceWord(rowData(o)["pfWant"]) != store.PfChoiceOptOut {
		return nil
	}
	g, _ := o["gross"].(float64)
	if lim := loadPfRules(ctx, q).ExemptGross; g > 0 && g <= lim {
		return bad("%s", pfRetainerMsg(lim))
	}
	return nil
}

// offerPfEvent puts the offer's answer on the candidate's timeline: the first one, or a change in a revised offer.
func offerPfEvent(ctx context.Context, tx store.Q, c *Ctx, cand string, prev store.Row, d map[string]string) error {
	w := d["pfWant"]
	if w == "" {
		return nil
	}
	was := ""
	if prev != nil {
		was = rowData(prev)["pfWant"]
	}
	if was == w {
		return nil
	}
	ev := lifeEv{Cand: cand, Action: "PF choice on the offer", To: pfWantText(w)}
	if was != "" {
		ev.Action, ev.From = "PF choice changed on the offer", pfWantText(was)
	}
	return lifeEvent(ctx, tx, c, ev)
}

// joinPfEvent records an answer to "Do you want PF deducted?" given at joining (where: "the joining form" for the
// joiner, "the joining details" for HR) when it differs from the one on the offer's data: audited, and on the
// candidate's timeline.
func joinPfEvent(ctx context.Context, tx store.Q, c *Ctx, o store.Row, next, where string) error {
	d := rowData(o)
	was, now := pfChoiceWord(d["pfChoice"]), pfChoiceWord(next)
	if now == "" || now == was {
		return nil
	}
	reason := ""
	if w := pfChoiceWord(d["pfWant"]); w != "" && w != now {
		reason = "The offer said " + pfYesNo(w)
	}
	action := "PF choice on " + where
	if was != "" {
		action = "PF choice changed on " + where
	}
	if err := audit(ctx, tx, c, action, "offer", str(o["id"]), map[string]any{"choice": pfYesNo(was)}, map[string]any{"choice": pfYesNo(now), "offer": pfYesNo(d["pfWant"])}); err != nil {
		return err
	}
	cand := str(o["candidate_id"])
	if cand == "" {
		return nil
	}
	return lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: action, From: pfWantText(was), To: pfWantText(now), Reason: reason})
}

// pfAnswerSource says where the answer in force came from, for employee_pay.pf_choice_by and the reason.
func pfAnswerSource(d map[string]string) (by, why string) {
	w, now := pfChoiceWord(d["pfWant"]), pfAnswer(d)
	switch {
	case w == "":
		return "Joining form", "Answered on the joining form"
	case w != now:
		return "Joining form", "Answered on the joining form; the offer said " + pfYesNo(w)
	}
	return store.PfChoiceByOffer, "Answered on the offer"
}

// pfRetainerNote is the PF card's line for someone who chose no PF on a gross up to the limit.
func pfRetainerNote(limit float64) string {
	return fmt.Sprintf("Chose no PF, but the monthly gross is up to %s: an employee who does not want PF must be a Retainer. Change the employment type to Retainer. PF applies until then.", calc.Inr(limit))
}

// pfRetainerAlert tells HR (HR Admin and Payroll at Edit) once that a joiner chose no PF on a gross up to the limit.
func pfRetainerAlert(ctx context.Context, tx store.Q, f *pfFacts, limit float64) error {
	to, err := hstRecipients(ctx, tx, "pay.run")
	if err != nil {
		return err
	}
	text := fmt.Sprintf("%s (%s) chose no PF at joining, but the monthly gross is up to %s: an employee who does not want PF must be a Retainer. Change the employment type to Retainer.",
		f.Name, f.ID, calc.Inr(limit))
	for who, view := range to {
		if _, err := noticeOnce(ctx, tx, "pf-retainer:"+f.ID+":"+who, who, text, "/file?id="+f.ID, view); err != nil {
			return err
		}
	}
	return nil
}
