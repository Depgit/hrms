package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// The employee record's additions of 14 Nov 2026: job role and skill (b1_masters.go), children's names, "Existing
// UAN?", the PAN check (b1_pan.go), the monthly medical insurance amount and the opening optional holiday balance.

// existing_uan: the joiner already has a UAN (true) or not (false); missing means unknown. Builder 2's PF rules read
// it; this file only stores and shows it.

// uanFlag reads "Yes"/"No" (any case; Y/N, true/false) into a bool; ok is false for anything else.
func uanFlag(v string) (val bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "y", "true":
		return true, true
	case "no", "n", "false":
		return false, true
	}
	return false, false
}

func yesNo(v any) string {
	switch v {
	case true:
		return "Yes"
	case false:
		return "No"
	}
	return ""
}

// applyOfferExtras copies what the joining form gives beyond the personal details: "Existing UAN?", the PF choice and the PAN
// check of the PAN being copied (the check is kept only for the same PAN).
func applyOfferExtras(ctx context.Context, tx store.Q, empID string, o store.Row, d map[string]string) error {
	if v, ok := uanFlag(d["uanAvail"]); ok {
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": empID}, bson.M{"$set": bson.M{"existing_uan": v}}); err != nil {
			return err
		}
	}
	if err := applyJoinPfChoice(ctx, tx, empID, str(o["candidate_id"]), d); err != nil { // "Do you want PF deducted?" (pfuan.go, pf_offer.go)
		return err
	}
	pc, _ := o["pan_check"].(map[string]any)
	pan := strings.ToUpper(strings.TrimSpace(d["pan"]))
	if pc == nil || pan == "" || str(pc["pan"]) != pan {
		return nil
	}
	_, err := tx.C("employee_gov_id").Update(ctx, bson.M{"employee_id": empID, "pan": pan}, bson.M{"$set": bson.M{"pan_status": pc["status"],
		"pan_name": employeesNullIf(str(pc["name"])), "pan_checked_at": pc["at"], "pan_checked_by": pc["by"], "pan_checked": pan}})
	return err
}

// employeeB1 adds to the employee record: the PAN check and Existing UAN? next to the statutory IDs (for viewers
// of government IDs), and the opening optional holiday balance.
func (s *Server) employeeB1(ctx context.Context, c *Ctx, e store.Row, id string, own bool) {
	q := s.st.DB
	if own || c.Level("emp.govid") >= 1 {
		if g, _ := q.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id}); g != nil {
			if gv, ok := e["govId"].(store.Row); ok && gv != nil {
				gv["panCheck"] = panView(g)
			}
		}
		x, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"existing_uan": 1}))
		e["existingUan"] = yesNo(x["existing_uan"])
	}
	if o := optHolOpeningOf(ctx, q, id, today()); o != nil {
		e["optHolOpening"] = o
	}
}

// ---------- Medical insurance a month, per employee ----------

// withMedical is the structure with the employee's monthly medical insurance in place of the structure's rate and
// cap (nil: the structure as it is). The amount is an employer cost: ESI or medical cover, never both, as before
// (calc decides who is covered); 0 means no medical insurance. The month's medical is the amount, at most the
// gross earned.
func withMedical(st *calc.Structure, amt *float64) *calc.Structure {
	if amt == nil || st == nil {
		return st
	}
	x := *st
	x.MedOn, x.MedPct, x.MedMax = *amt > 0, 100, *amt
	return &x
}

// ---------- Bulk update › Employee master update: Role, Skill, Medical, Accommodation ----------

// updPayCols are the Employee master update's pay columns: Medical (₹ a month, "Standard" for the structure's
// rule) and Accommodation (the deduction the Salary breakup template sets). They need the salary right.
type updPay struct {
	medical, accom *float64
	medStd         bool
}

// bulkUpdPayRead checks the Medical and Accommodation cells of a row (blank keeps the current value).
func bulkUpdPayRead(ctx context.Context, tx store.Q, c *Ctx, r bulkRow, e store.Row, res *bulkResult) updPay {
	var p updPay
	med, acc := strings.TrimSpace(r.V["Medical"]), strings.TrimSpace(r.V["Accommodation"])
	if med == "" && acc == "" {
		return p
	}
	if c.Level("emp.salary") < 2 {
		res.Errors = append(res.Errors, "Medical and Accommodation need the right to change salaries")
		return p
	}
	typ := str(e["type"])
	t := calc.RulesFor(calc.EmploymentType(typ))
	cur := payFor(ctx, tx, res.ID, typ)
	num := func(col, v string) *float64 {
		f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64)
		if err != nil || f < 0 || f > 1e6 {
			res.Errors = append(res.Errors, col+" must be an amount in rupees, 0 or more")
			return nil
		}
		f = calc.Round2(f)
		return &f
	}
	if strings.EqualFold(med, "Standard") {
		if cur.Medical != nil {
			p.medStd = true
		}
	} else if med != "" {
		if !t.Statutory {
			res.Errors = append(res.Errors, typ+" staff have no medical insurance")
		} else if v := num("Medical", med); v != nil && (cur.Medical == nil || *cur.Medical != *v) {
			p.medical = v
		}
	}
	if acc != "" {
		if t.Ded != "all" {
			res.Errors = append(res.Errors, typ+" staff have no accommodation deduction")
		} else if v := num("Accommodation", acc); v != nil && *v != cur.Accom {
			p.accom = v
		}
	}
	return p
}

func (p updPay) any() bool { return p.medical != nil || p.accom != nil || p.medStd }

// bulkUpdPayApply writes the pay columns of a checked row with its reason and date, each in the change history.
func bulkUpdPayApply(ctx context.Context, tx store.Q, c *Ctx, id, typ string, p updPay, reason, eff string, res *bulkResult) error {
	cur := payFor(ctx, tx, id, typ)
	set := bson.M{}
	medText := func(v *float64) string {
		if v == nil {
			return "Standard"
		}
		return rupees(*v)
	}
	changes := []bulkChange{}
	if p.medical != nil || p.medStd {
		var next *float64
		if p.medical != nil {
			next = p.medical
		}
		set["medical"] = next
		changes = append(changes, bulkChange{"Medical insurance (₹ a month)", medText(cur.Medical), medText(next), ""})
	}
	if p.accom != nil {
		set["accommodation"] = *p.accom
		changes = append(changes, bulkChange{"Accommodation deduction", rupees(cur.Accom), rupees(*p.accom), ""})
	}
	if len(set) == 0 {
		return nil
	}
	if _, err := tx.C("employee_pay").Upsert(ctx, bson.D{{Key: "employee_id", Value: id}}, set, nil); err != nil {
		return err
	}
	for _, x := range changes {
		if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": x.Field, "old_value": x.From, "new_value": x.To, "reason": reason,
			"effective_from": eff, "source": "Bulk update", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
			return err
		}
		res.Changes = append(res.Changes, x)
	}
	return audit(ctx, tx, c, "Pay options changed", "employee", id, nil, changes)
}

// ---------- Bulk update › Personal details: Existing UAN? ----------

// bulkPersUANCheck reads the Existing UAN? cell of a Personal details row: Yes needs a UAN on record, and the
// column needs the government ID right at Edit. It returns the value to write (nil: none, or no change).
func bulkPersUANCheck(ctx context.Context, tx store.Q, c *Ctx, r bulkRow, res *bulkResult) *bool {
	v := strings.TrimSpace(r.V["Existing UAN?"])
	if v == "" {
		return nil
	}
	flag, ok := uanFlag(v)
	if !ok {
		res.Errors = append(res.Errors, "Existing UAN? is Yes or No")
		return nil
	}
	if c.Level("emp.govid") < 2 {
		res.Errors = append(res.Errors, "Existing UAN? needs the right to change government IDs")
		return nil
	}
	if flag && !reUAN.MatchString(statIDClean(r.V["UAN"])) { // a UAN in the same row counts
		g, _ := tx.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": res.ID})
		if statIDClean(str(g["uan"])) == "" {
			res.Errors = append(res.Errors, "Existing UAN? Yes needs the UAN: add the UAN to the record first")
			return nil
		}
	}
	e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": res.ID}, options.FindOne().SetProjection(bson.M{"existing_uan": 1}))
	if e != nil && e["existing_uan"] == flag {
		return nil
	}
	return &flag
}

// bulkPersUANNoCheck reads the UAN cell of a Personal details row (owner, 19 Nov 2026): 12 digits, the government
// ID right at Edit. It returns the UAN to write ("" for none, or no change).
func bulkPersUANNoCheck(ctx context.Context, tx store.Q, c *Ctx, r bulkRow, res *bulkResult) string {
	v := strings.TrimSpace(r.V["UAN"])
	if v == "" {
		return ""
	}
	v = strings.Join(strings.Fields(v), "")
	if !reUAN.MatchString(v) {
		res.Errors = append(res.Errors, "UAN must be 12 digits")
		return ""
	}
	if c.Level("emp.govid") < 2 {
		res.Errors = append(res.Errors, "UAN needs the right to change government IDs")
		return ""
	}
	if g, _ := tx.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": res.ID}, options.FindOne().SetProjection(bson.M{"uan": 1})); g != nil && statIDClean(str(g["uan"])) == v {
		return ""
	}
	return v
}

// bulkPersUANNoApply writes a checked UAN, in the change history (masked to the last 4 digits) and the audit log.
func bulkPersUANNoApply(ctx context.Context, tx store.Q, c *Ctx, id, uan string, res *bulkResult) error {
	g, _ := tx.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": id}, options.FindOne().SetProjection(bson.M{"uan": 1}))
	old := statIDClean(str(g["uan"]))
	if _, err := tx.C("employee_gov_id").Upsert(ctx, bson.M{"employee_id": id}, bson.M{"uan": uan}, nil); err != nil {
		return err
	}
	mask := func(v string) string {
		if len(v) < 4 {
			return or(v, "None")
		}
		return "ending " + v[len(v)-4:]
	}
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": "UAN", "old_value": mask(old), "new_value": mask(uan),
		"reason": "Correction", "effective_from": today(), "source": "Bulk update", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return err
	}
	res.Changes = append(res.Changes, bulkChange{Field: "UAN", From: mask(old), To: mask(uan)})
	return audit(ctx, tx, c, "UAN changed", "employee", id, map[string]any{"uan": mask(old)}, map[string]any{"uan": mask(uan)})
}

// bulkPersUANApply writes a checked Existing UAN?, in the change history and the audit log.
func bulkPersUANApply(ctx context.Context, tx store.Q, c *Ctx, id string, flag bool, res *bulkResult) error {
	e, err := tx.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"existing_uan": 1}))
	if err != nil || e == nil {
		return orNotFound(err)
	}
	if _, err := tx.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"existing_uan": flag}}); err != nil {
		return err
	}
	from := or(yesNo(e["existing_uan"]), "Not known")
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": "Existing UAN?", "old_value": from, "new_value": yesNo(flag),
		"reason": "Correction", "effective_from": today(), "source": "Bulk update", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return err
	}
	res.Changes = append(res.Changes, bulkChange{Field: "Existing UAN?", From: from, To: yesNo(flag)})
	return audit(ctx, tx, c, "Existing UAN? changed", "employee", id, map[string]any{"existingUan": e["existing_uan"]}, map[string]any{"existingUan": flag})
}

// importUAN reads the optional Existing UAN? column of an employee import: Yes needs the UAN in the same row.
func importUAN(v, uan string, add func(string, ...any)) any {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	flag, ok := uanFlag(v)
	if !ok {
		add("Existing UAN? is Yes or No")
		return nil
	}
	if flag && uan == "" {
		add("UAN is required when Existing UAN? is Yes")
	}
	return flag
}

// fmtMoneyPtr shows an optional amount ("" when unset).
func fmtMoneyPtr(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(*v)
}

// bulkUANFlags is each employee's Existing UAN? as Yes, No or "" (unknown), for the Personal details download.
func bulkUANFlags(ctx context.Context, q store.Q, ids []string) map[string]string {
	out := map[string]string{}
	rows, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids}, "existing_uan": bson.M{"$ne": nil}},
		options.Find().SetProjection(bson.M{"existing_uan": 1}))
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[str(r["id"])] = yesNo(r["existing_uan"])
	}
	return out
}
