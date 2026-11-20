package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// Personal details (spec 22 API: PATCH sections separately). HR corrects a person's mobile,
// email, birth date, address and emergency contact directly; an employee asks for the same
// change from My details and it goes for approval. The mobile is where the sign-in OTP goes.

var (
	genders  = []string{"Male", "Female", "Other", "Prefer not to say"}
	maritals = []string{"Single", "Married", "Other"}
)

// personalIn: nil keeps the current value, "" clears it (never for the mobile).
type personalIn struct {
	Mobile       *string `json:"mobile"`
	Email        *string `json:"email"`
	DOB          *string `json:"dob"`
	Gender       *string `json:"gender"`
	Address      *string `json:"address"`
	FatherSpouse *string `json:"fatherSpouse"`
	Marital      *string `json:"marital"`
	ECName       *string `json:"ecName"`
	ECRelation   *string `json:"ecRelation"`
	ECMobile     *string `json:"ecMobile"`
	Child1       *string `json:"child1"` // the first child's name (owner, 14 Nov 2026)
	Child2       *string `json:"child2"` // the second child's name
	Spouse       *string `json:"spouse"` // the spouse's name (owner, 19 Nov 2026: the medical cover)
}

var personalCols = []struct{ col, label string }{
	{"mobile", "Mobile"}, {"email", "Personal email"}, {"dob", "Date of birth"}, {"gender", "Gender"}, {"address", "Address"},
	{"father_spouse", "Father or spouse name"}, {"marital", "Marital status"},
	{"ec_name", "Emergency contact name"}, {"ec_relation", "Emergency contact relation"}, {"ec_mobile", "Emergency contact mobile"},
	{"child1", "1st child name"}, {"child2", "2nd child name"}, {"spouse", "Spouse name"},
}

func (p personalIn) values() map[string]*string {
	return map[string]*string{"mobile": p.Mobile, "email": p.Email, "dob": p.DOB, "gender": p.Gender, "address": p.Address, "father_spouse": p.FatherSpouse,
		"marital": p.Marital, "ec_name": p.ECName, "ec_relation": p.ECRelation, "ec_mobile": p.ECMobile, "child1": p.Child1, "child2": p.Child2, "spouse": p.Spouse}
}

// savePersonal checks the new values against the current ones and saves the changes, each in
// the change history. It returns the problems instead when there are any.
// personalMobileOwner is another employee still with the company (not Inactive) whose mobile is m, or "".
func personalMobileOwner(ctx context.Context, q store.Q, m, except string) string {
	rows, err := q.C("employee_personal").Find(ctx, bson.M{"mobile": m, "employee_id": bson.M{"$ne": except}})
	if err != nil {
		return ""
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if ok, _ := q.C("employee").Exists(ctx, bson.M{"_id": id, "status": bson.M{"$ne": "Inactive"}}); ok {
			return id
		}
	}
	return ""
}

func savePersonal(ctx context.Context, tx store.Q, c *Ctx, id string, in personalIn, source string) ([]bulkChange, []string, error) {
	// The current values, "" for a blank one or when the person has no personal details yet.
	saved, err := tx.C("employee_personal").FindOne(ctx, bson.M{"employee_id": id})
	if err != nil {
		return nil, nil, err
	}
	cur, next := map[string]string{}, map[string]string{}
	for _, pc := range personalCols {
		cur[pc.col] = str(saved[pc.col])
		next[pc.col] = cur[pc.col]
	}
	for k, v := range in.values() {
		if v != nil {
			next[k] = strings.TrimSpace(*v)
		}
	}
	next["mobile"], next["ec_mobile"] = strings.ReplaceAll(next["mobile"], " ", ""), strings.ReplaceAll(next["ec_mobile"], " ", "")
	next["email"] = strings.ToLower(next["email"])
	if strings.EqualFold(next["email"], cur["email"]) {
		next["email"] = cur["email"] // the same address in another case is no change (PPL-149)
	}
	var errs []string
	add := func(s string) { errs = append(errs, s) }
	// Length limits (PPL-058): an address is a few lines, a name a few words.
	for _, l := range []struct {
		col, label string
		max        int
	}{{"address", "Address", 500}, {"father_spouse", "Father or spouse name", 100}, {"ec_name", "Emergency contact name", 100}, {"email", "Email", 120},
		{"child1", "1st child name", 100}, {"child2", "2nd child name", 100}, {"spouse", "Spouse name", 100}} {
		if n := len([]rune(next[l.col])); n > l.max && next[l.col] != cur[l.col] {
			add(fmt.Sprintf("%s can be at most %d characters (this one has %d)", l.label, l.max, n))
		}
	}
	if next["mobile"] == "" && cur["mobile"] != "" {
		add("Mobile cannot be removed: the sign-in OTP goes to it")
	} else if next["mobile"] != "" && !reMobile.MatchString(next["mobile"]) {
		add("Mobile must be 10 digits")
	} else if next["mobile"] != "" && next["mobile"] != cur["mobile"] {
		// The mobile receives the sign-in OTP, so it identifies one person (PPL-059).
		if other := personalMobileOwner(ctx, tx, next["mobile"], id); other != "" {
			add("Mobile " + next["mobile"] + " is already used by " + other)
		}
	}
	if v := next["email"]; v != "" && !reEmail.MatchString(v) {
		add("Email is not valid")
	}
	if v := next["dob"]; v != "" && (!isDate(v) || v >= today()) {
		add("Date of birth must be a past date like 1990-04-21")
	} else if v != "" && v > time.Now().AddDate(-14, 0, 0).Format("2006-01-02") {
		add("Date of birth makes the person younger than 14")
	}
	if v := next["gender"]; v != "" && !containsStr(genders, v) {
		add("Gender is one of " + strings.Join(genders, ", "))
	}
	if v := next["marital"]; v != "" && !containsStr(maritals, v) {
		add("Marital status is one of " + strings.Join(maritals, ", "))
	}
	if next["ec_name"] != "" || next["ec_mobile"] != "" || next["ec_relation"] != "" {
		switch {
		case len(next["ec_name"]) < 2:
			add("Give the emergency contact's name")
		case !containsStr(ecRelations, next["ec_relation"]):
			add("Emergency contact relation is one of " + strings.Join(ecRelations, ", "))
		case !reMobile.MatchString(next["ec_mobile"]):
			add("Emergency contact mobile must be 10 digits")
		case next["ec_mobile"] == next["mobile"]:
			add("The emergency contact needs a different number from the employee's")
		}
	}
	if len(errs) > 0 {
		return nil, errs, nil
	}
	changes := []bulkChange{}
	for _, pc := range personalCols {
		if next[pc.col] != cur[pc.col] {
			changes = append(changes, bulkChange{Field: pc.label, From: cur[pc.col], To: next[pc.col]})
		}
	}
	if len(changes) == 0 {
		return changes, nil, nil
	}
	// Every value is written, a blank one as null; the row is created when the person has none yet.
	set := bson.M{}
	for _, pc := range personalCols {
		set[pc.col] = employeesNullIf(next[pc.col])
	}
	if _, err := tx.C("employee_personal").Upsert(ctx, bson.M{"employee_id": id}, set, nil); err != nil {
		return nil, nil, err
	}
	for _, ch := range changes {
		if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": ch.Field, "old_value": ch.From, "new_value": ch.To,
			"reason": "Correction", "effective_from": today(), "source": source, "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
			return nil, nil, err
		}
	}
	return changes, nil, audit(ctx, tx, c, "Personal details changed", "employee", id, nil, changes)
}

func (s *Server) employeePersonal(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("emp.personal", 2); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if id == c.EmployeeID {
		return nil, forbidden("Change your own details from My pay and profile → My details. The change goes for approval.")
	}
	if err := c.MustInScope(r.Context(), s.st.DB, id); err != nil {
		return nil, err
	}
	var b personalIn
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	var out []bulkChange
	err := s.st.Tx(r.Context(), func(tx store.Q) error {
		ch, errs, err := savePersonal(r.Context(), tx, c, id, b, "Manual")
		if err == nil && len(errs) > 0 {
			return bad("%s", strings.Join(errs, ". "))
		}
		out = ch
		return err
	})
	return map[string]any{"changes": out}, err
}

// bulkPers is the Personal details template: blank cells keep the current value.
func bulkPers(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	if _, ok := bulkEmployee(ctx, tx, c, r, &res); !ok {
		return res, nil
	}
	in := personalIn{}
	for col, dst := range map[string]**string{"Mobile": &in.Mobile, "Personal Email": &in.Email, "Gender": &in.Gender, "Address": &in.Address,
		"Father or Spouse Name": &in.FatherSpouse, "Marital Status": &in.Marital, "Emergency Contact Name": &in.ECName,
		"Emergency Contact Relation": &in.ECRelation, "Emergency Contact Mobile": &in.ECMobile, "1st Child": &in.Child1, "2nd Child": &in.Child2,
		"Spouse Name": &in.Spouse} {
		if v := strings.TrimSpace(r.V[col]); v != "" {
			*dst = &v
		}
	}
	if v := excelDate(strings.TrimSpace(r.V["Date of Birth"])); v != "" {
		in.DOB = &v
	}
	uanNo := bulkPersUANNoCheck(ctx, tx, c, r, &res) // the UAN number (b1_employee.go), checked before anything is written
	uan := bulkPersUANCheck(ctx, tx, c, r, &res)     // Existing UAN? (b1_employee.go)
	if len(res.Errors) > 0 {
		return res, nil
	}
	ch, errs, err := savePersonal(ctx, tx, c, res.ID, in, "Bulk update")
	res.Changes, res.Errors = ch, errs
	if err != nil || len(errs) > 0 {
		return res, err
	}
	if uanNo != "" {
		if err := bulkPersUANNoApply(ctx, tx, c, res.ID, uanNo, &res); err != nil {
			return res, err
		}
	}
	if uan == nil {
		return res, nil
	}
	return res, bulkPersUANApply(ctx, tx, c, res.ID, *uan, &res)
}
