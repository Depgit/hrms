package api

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Owner's test sheet of 18 Nov 2026 (hiring fixes):
//
//   #2/#3 Role and Skill are required wherever the department is: Import employees, the offer form (also checked
//   here, not only by Form setup), the joining details of an offer that has none yet (an offer made before they
//   were asked), and a job change of an employee (Employee master update or the job edit) when the employee has
//   none yet. Existing employees without them are not blocked until their job is edited.
//
//   #22 Every employee has Shortlisting, Offer and Joining IDs: a hire keeps the candidate's number; Import employees
//   (and everyone else without a candidate, migration 025) takes the Employee code's digits (24 Nov 2026,
//   store/schema_hirecode.go). Shown on the employee file and the status timeline (employee.hire_no, hire_digits).
//
//   #26 The offer letter can be released at joining: by department and by employment type (Form setup › Hiring).
//   The offer still goes through approval, is released (the answer is due as usual) and HR records Accepted, but the
//   letter is held (not generated or shown) until Mark joined releases it, audited.

// ---------- #2/#3 Role and Skill ----------

// jobMasterRequired adds "Role is required" / "Skill is required" for an offer without them.
func jobMasterRequired(d map[string]string, errs *[]string) {
	for _, kind := range []string{store.KindJobRole, store.KindSkill} {
		if strings.TrimSpace(d[jobMasterKey[kind]]) == "" {
			*errs = append(*errs, jobMasterLabel[kind]+" is required")
		}
	}
}

// jobMasterMissing names what an offer still lacks of Role and Skill ("" when both are there).
func jobMasterMissing(d map[string]string) string {
	var l []string
	jobMasterRequired(d, &l)
	return strings.Join(l, ", ")
}

// roleSkillForJob says what a job change needs when the employee has no Role or Skill yet and the change does not
// give them: "" when nothing is missing.
func roleSkillForJob(cur store.Row, role, skill string) string {
	var miss []string
	if strings.TrimSpace(role) == "" && str(cur["job_role"]) == "" {
		miss = append(miss, "Role")
	}
	if strings.TrimSpace(skill) == "" && str(cur["skill"]) == "" {
		miss = append(miss, "Skill")
	}
	if len(miss) == 0 {
		return ""
	}
	return strings.Join(miss, " and ") + " " + map[bool]string{true: "is", false: "are"}[len(miss) == 1] + " required when the job changes (this employee has none yet)"
}

// coreJobColumns are the job fields whose change needs Role and Skill.
var coreJobColumns = map[string]bool{"department_id": true, "designation_id": true, "location_id": true, "grade": true}

// bulkUpdRoleSkill checks an Employee master update row: a change of location, department, designation or grade
// of an employee without Role or Skill must give them. It returns the row's result and true when the row is refused.
func bulkUpdRoleSkill(ctx context.Context, tx store.Q, c *Ctx, r bulkRow) (bulkResult, bool) {
	var pre bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &pre)
	if !ok || e["status"] != "Active" {
		return pre, false // the row's own check says why
	}
	ch := map[string]string{}
	for col, key := range map[string]string{"Reporting Location": "location", "Department": "department", "Designation": "designation", "Grade": "grade"} {
		if v := strings.TrimSpace(r.V[col]); v != "" {
			ch[key] = v
		}
	}
	updUnchanged(ctx, tx, pre.ID, ch) // the current values uploaded as they are: no change
	if len(ch) == 0 {
		return pre, false
	}
	cur, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": pre.ID})
	if msg := roleSkillForJob(cur, r.V["Role"], r.V["Skill"]); msg != "" {
		pre.Errors = append(pre.Errors, msg+": give them in the Role and Skill columns")
		return pre, true
	}
	return pre, false
}

// joinRoleSkillFields are Role and Skill asked in the joining details of an offer that has none yet (made before
// they were asked on the offer form).
func joinRoleSkillFields(ctx context.Context, q store.Q, typ string, d map[string]string) []onbField {
	var out []onbField
	for _, kind := range []string{store.KindJobRole, store.KindSkill} {
		key := jobMasterKey[kind]
		if strings.TrimSpace(d[key]) != "" {
			continue
		}
		out = append(out, onbField{Key: key, Label: jobMasterLabel[kind], Section: "Job", Kind: "sel", Options: jobMasterOptions(ctx, q, key), On: true, Sys: true,
			Join: 2, Types: []string{typ}, FilledBy: store.FilledByHR})
	}
	return out
}

// joinRoleSkill reads Role and Skill from HR's joining details when the offer has none yet: the names to store, or
// the problems. The employee record made at joining (a Joining ID) gets them too.
func joinRoleSkill(ctx context.Context, tx store.Q, o store.Row, in map[string]string) (bson.D, []string) {
	d := rowData(o)
	set, errs := bson.D{}, []string{}
	for _, kind := range []string{store.KindJobRole, store.KindSkill} {
		key := jobMasterKey[kind]
		v, ok := in[key]
		if !ok || strings.TrimSpace(d[key]) != "" {
			continue
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		if _, name, ok := jobMasterLookup(kind)(ctx, tx, v); ok {
			set = append(set, bson.E{Key: "data." + key, Value: name})
		} else {
			errs = append(errs, fmt.Sprintf("%s “%s” is not an active %s in Masters", jobMasterLabel[kind], v, strings.ToLower(jobMasterLabel[kind])))
		}
	}
	return set, errs
}

// joinRoleSkillEmployee puts the offer's Role and Skill on the employee record made at joining when it has none.
func joinRoleSkillEmployee(ctx context.Context, tx store.Q, o store.Row) error {
	emp := str(o["employee_id"])
	if cand := str(o["candidate_id"]); emp == "" && cand != "" {
		if x, _ := onboardingOne(ctx, tx, "candidate", cand); x != nil {
			emp = str(x["employee_id"])
		}
	}
	if emp == "" {
		return nil
	}
	d := rowData(o)
	for _, kind := range []string{store.KindJobRole, store.KindSkill} {
		v := strings.TrimSpace(d[jobMasterKey[kind]])
		if v == "" {
			continue
		}
		col := jobMasterCol[kind]
		if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: emp}, {Key: col, Value: nil}}, bson.D{{Key: "$set", Value: bson.D{{Key: col, Value: v}}}}); err != nil {
			return err
		}
	}
	return nil
}

// ---------- #22 Shortlisting, Offer and Joining IDs of every employee ----------

// importHireNo gives an imported employee SI-/OI-/JI- IDs from the Employee code (EMP-000004 → SI-000004, owner's
// rule of 24 Nov 2026), or the next number of the candidate series when that number is taken or the code has none,
// with a timeline entry (store.HireNoFromCode).
func importHireNo(ctx context.Context, tx store.Q, c *Ctx, id, doj string) (int, error) {
	if err := store.AdvisoryLock(ctx, tx, "hire_no"); err != nil { // two imports at once never take one number
		return 0, err
	}
	n, digits, why, err := store.HireNoFromCode(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if err := store.SetHireNo(ctx, tx, id, n, digits); err != nil {
		return 0, err
	}
	return n, store.HireIDsEvent(ctx, tx, id, n, digits, doj, store.HireReason("Imported with Import employees", why), c.Name, c.EmployeeID)
}

// hireIDsFor is an employee's Shortlisting, Offer and Joining IDs (nil when they have none yet). via is "candidate"
// (a hire through Onboarding), "code" (from the Employee code) or "import" (a number of the candidate series).
func hireIDsFor(ctx context.Context, q store.Q, id string) map[string]any {
	e, _ := onboardingOne(ctx, q, "employee", id)
	if e == nil {
		return nil
	}
	no, digits, via := toInt(e["hire_no"]), str(e["hire_digits"]), "import"
	if digits != "" {
		via = "code"
	}
	if x, _ := q.C("candidate").FindOne(ctx, bson.D{{Key: "employee_id", Value: id}}); x != nil {
		no, digits, via = toInt(x["no"]), "", "candidate"
	}
	if no <= 0 {
		return nil
	}
	si, oi, ji := store.HireIDsOf(no, digits)
	return map[string]any{"no": no, "si": si, "oi": oi, "ji": ji, "via": via}
}

// ---------- #26 Offer letter released at joining ----------

// letterAtJoining says whether the rule holds an offer's letter until joining.
func letterAtJoining(h hireSettings, dept, typ string) bool {
	return containsStr(h.LetterJoinDepts, dept) || containsStr(h.LetterJoinTypes, typ)
}

// letterRuleCheck checks the rule's departments and employment types against the masters.
func letterRuleCheck(ctx context.Context, q store.Q, h hireSettings) error {
	for _, d := range h.LetterJoinDepts {
		if ok, _ := q.C("department").Exists(ctx, bson.M{"name": d}); !ok {
			return bad("Department “%s” is not in Masters", d)
		}
	}
	for _, t := range h.LetterJoinTypes {
		if containsStr(empTypes, t) {
			continue
		}
		if ok, _ := q.C("simple_master").Exists(ctx, bson.M{"kind": "employment_type", "name": t}); !ok {
			return bad("Unknown employment type %s", t)
		}
	}
	return nil
}

// letterView adds the letter rule to an offer read: letterAt ("joining" or "offer"), letterHeld (the letter is not
// out yet: not generated), letterReleasedAt and the rule in words. A legacy offer (no candidate) has no Mark joined,
// so it always releases with the offer.
func letterView(ctx context.Context, q store.Q, o store.Row) {
	at := "offer"
	if str(o["candidate_id"]) != "" {
		if o["at_joining"] == true { // an offer at joining: the letter is given at Mark joined (offer_at_joining.go)
			at = "joining"
		} else if o["released_at"] != nil {
			if o["letter_at"] == "joining" {
				at = "joining"
			}
		} else if letterAtJoining(loadHireSettings(ctx, q), str(o["department"]), str(o["type"])) {
			at = "joining"
		}
	}
	held := at == "joining" && o["letter_released_at"] == nil
	o["letterAt"], o["letterHeld"], o["letterReleasedAt"] = at, held, o["letter_released_at"]
	if at == "joining" && o["at_joining"] == true {
		o["letterRule"] = "Offer at joining: approved as usual and never released before joining. When HR marks the candidate joined, the offer is accepted (joining is the acceptance) and the letter is generated and given."
	} else if at == "joining" {
		o["letterRule"] = fmt.Sprintf("Offer letter released at joining (%s, %s): approve, release and record Accepted as usual; the letter is generated and given to the candidate when HR marks them joined.", o["department"], o["type"])
	}
	if held {
		o["letter"] = ""
	}
}

// releaseLetterAtJoining releases a held offer letter at Mark joined (audited, on the timeline).
func releaseLetterAtJoining(ctx context.Context, tx store.Q, c *Ctx, o store.Row, cand, date string) error {
	if o["letter_at"] != "joining" || o["letter_released_at"] != nil {
		return nil
	}
	if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "letter_released_at", Value: tx.Now()}}}}); err != nil {
		return err
	}
	why := "Held until joining by the offer letter rule (Form setup › Hiring)"
	if o["at_joining"] == true {
		why = "Offer at joining: the letter is given when they join"
	}
	if err := lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Action: "Offer letter released at joining", Date: date, Reason: why}); err != nil {
		return err
	}
	return audit(ctx, tx, c, "Offer letter released at joining for "+str(o["candidate_name"]), "offer", str(o["id"]), map[string]any{"letterAt": "joining"}, map[string]any{"releasedOn": date})
}
