package api

// One employee's own deviation allowances (owner, 24 Nov 2026: "HR admin dega us employee ko"). Early check-in, Late
// check-in and Early check-out are set per work policy (dev_allow.go); HR can also give them to one employee without a
// separate policy. Opened from the employee's name on the roster grid.
//
// Each rule has three choices: use the policy's rule (the default; it takes the policy's action too), On with the
// employee's own "Minutes allowed", "Times allowed in a month" and Action for a day beyond them (all given by HR,
// nothing prefilled; On refused without all three, the same limits as the policy: checkDevAllow), or Off for this
// employee. The rules carry an "Applies from" date and are kept as versions
// (emp_dev_allow): the engine uses, for each date, the version in effect on it laid over the policy's rules
// (attend.DevIn.Rules); a rule's count runs on through the calendar month when a version starts in it. A date inside
// an approved or published payroll month is refused (paidMonthMsg). A version saved on a date that already has one
// replaces it. Every save is audited.
//
// Right: editing work policies (pol.policies at Edit), the right that sets the same allowances on a policy: an
// allowance changes late marks and loss of pay, and giving it person by person must not be wider than giving it to a
// whole policy. Viewing: pol.policies, ros.roster or att.records at View. Both within the caller's data scope.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// ownDev is one version of an employee's own deviation allowances.
type ownDev struct {
	ID, From, Note, By, At string
	R                      policy.DevOwnRules
}

// loadOwnDev reads every employee's versions, oldest first.
func loadOwnDev(ctx context.Context, q store.Q) (map[string][]ownDev, error) {
	rows, err := q.C(store.EmpDevAllowColl).Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "effective_from", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := map[string][]ownDev{}
	for _, r := range rows {
		v := ownDev{ID: str(r["id"]), From: str(r["effective_from"]), Note: str(r["note"]), By: str(r["by_name"]), At: timectxStamp(r["created_at"], "02 Jan 2006 15:04")}
		if err := timectxDecode(r["rules"], &v.R); err != nil {
			return nil, fmt.Errorf("deviation allowances of %s: %w", str(r["employee_id"]), err)
		}
		id := str(r["employee_id"])
		out[id] = append(out[id], v)
	}
	return out, nil
}

// ownDevOn is the employee's own version in effect on d (nil when none).
func (t *timeSet) ownDevOn(id, d string) *ownDev {
	var v *ownDev
	for i := range t.own[id] {
		if t.own[id][i].From <= d {
			v = &t.own[id][i]
		}
	}
	return v
}

// devRulesOn are the allowances in force for the employee on d when they are their own (nil: the policy's).
func (t *timeSet) devRulesOn(e *emp, p *workPolicy, d string) *policy.DevRules {
	v := t.ownDevOn(e.ID, d)
	if v == nil {
		return nil
	}
	r := v.R.Over(p.S.Dev)
	return &r
}

// devMaybe: an allowance may be in force for the employee on some date: their policy has one, or a version of their
// own rules switches one on.
func (t *timeSet) devMaybe(e *emp, p *workPolicy) bool {
	if p.S.Dev.Any() {
		return true
	}
	for _, v := range t.own[e.ID] {
		if v.R.Over(p.S.Dev).Any() {
			return true
		}
	}
	return false
}

// ownDevOnAny: some employee has an allowance of their own switched on.
func (t *timeSet) ownDevOnAny() bool {
	for _, vs := range t.own {
		for _, v := range vs {
			for _, x := range []policy.DevOwn{v.R.EarlyIn, v.R.LateIn, v.R.EarlyOut} {
				if x.Use == policy.DevUseOn {
					return true
				}
			}
		}
	}
	return false
}

// ---------- Routes ----------

var devOwnLabels = []struct{ key, label string }{{"earlyIn", "Early check-in"}, {"lateIn", "Late check-in"}, {"earlyOut", "Early check-out"}}

func devOwnOf(r *policy.DevOwnRules, key string) *policy.DevOwn {
	switch key {
	case "earlyIn":
		return &r.EarlyIn
	case "lateIn":
		return &r.LateIn
	}
	return &r.EarlyOut
}

// devOwnText is the rules in words: "Early check-in: the policy's; Late check-in: on, 120 min, 3 times a month; …".
func devOwnText(r policy.DevOwnRules) string {
	parts := []string{}
	for _, x := range devOwnLabels {
		o := devOwnOf(&r, x.key)
		switch o.Use {
		case policy.DevUseOn:
			parts = append(parts, fmt.Sprintf("%s: on, %d min, %d times a month, beyond: %s", x.label, o.Mins, o.N, devActWords(o.Act)))
		case policy.DevUseOff:
			parts = append(parts, x.label+": off")
		default:
			parts = append(parts, x.label+": the policy's")
		}
	}
	return strings.Join(parts, "; ")
}

// devActWords is an action as the admin picks it: "No action", "Absent", "Half day until regularized" …
func devActWords(act string) string {
	if act == policy.DevActNone || act == "" {
		return "No action"
	}
	if act == policy.DevActAbsent {
		return "Absent"
	}
	return attend.ActStatus(act)
}

func canSeeDevOwn(c *Ctx) error {
	if c.Level("pol.policies") < 1 && c.Level("ros.roster") < 1 && c.Level("att.records") < 1 {
		return forbidden("")
	}
	return nil
}

// devOwnGet is the employee's own rules: the versions, the one in effect today and the policy's rules beside them.
func (s *Server) devOwnGet(r *http.Request, c *Ctx) (any, error) {
	if err := canSeeDevOwn(c); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	p, _ := ts.policyFor(e)
	t := today()
	cur := ts.ownDevOn(e.ID, t)
	versions := []map[string]any{}
	for i := len(ts.own[e.ID]) - 1; i >= 0; i-- { // newest first
		v := ts.own[e.ID][i]
		versions = append(versions, map[string]any{"id": v.ID, "from": v.From, "rules": v.R, "text": devOwnText(v.R), "note": v.Note, "by": v.By, "at": v.At,
			"current": cur != nil && cur.ID == v.ID, "future": v.From > t})
	}
	eff := p.S.Dev
	if x := ts.devRulesOn(e, p, t); x != nil {
		eff = *x
	}
	out := map[string]any{"employee": map[string]any{"id": e.ID, "name": e.Name, "policy": p.Name}, "policyRules": p.S.Dev, "versions": versions,
		"effective": eff, "canEdit": c.Level("pol.policies") >= 2, "today": t}
	if cur != nil {
		out["current"] = cur.R
	}
	return out, nil
}

// devOwnSave saves a version of the employee's own rules from a date.
func (s *Server) devOwnSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 2); err != nil {
		return nil, err
	}
	var b struct {
		EffectiveFrom string        `json:"effectiveFrom"`
		EarlyIn       policy.DevOwn `json:"earlyIn"`
		LateIn        policy.DevOwn `json:"lateIn"`
		EarlyOut      policy.DevOwn `json:"earlyOut"`
		Note          string        `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	if !isDate(b.EffectiveFrom) {
		return nil, bad("Give the date the rules apply from")
	}
	rules := policy.DevOwnRules{EarlyIn: b.EarlyIn, LateIn: b.LateIn, EarlyOut: b.EarlyOut}
	if err := checkDevOwn(&rules); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	if len(b.Note) > 500 {
		return nil, bad("Keep the note under 500 characters")
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	saved := false
	err = s.st.Tx(ctx, func(tx store.Q) error {
		saved = false
		if msg := paidMonthMsg(ctx, tx, b.EffectiveFrom, b.EffectiveFrom); msg != "" {
			return bad("Applies from %s: %s", longDate(b.EffectiveFrom), msg)
		}
		key := bson.M{"employee_id": e.ID, "effective_from": b.EffectiveFrom}
		old, err := tx.C(store.EmpDevAllowColl).FindOne(ctx, key)
		if err != nil {
			return err
		}
		// The version in effect on the date before this save: nothing to do when the rules are the same.
		prev, err := tx.C(store.EmpDevAllowColl).FindOne(ctx, bson.M{"employee_id": e.ID, "effective_from": bson.M{"$lte": b.EffectiveFrom}},
			options.FindOne().SetSort(bson.D{{Key: "effective_from", Value: -1}}))
		if err != nil {
			return err
		}
		if prev != nil {
			var pr policy.DevOwnRules
			if timectxDecode(prev["rules"], &pr) == nil && pr == rules && (old == nil || str(old["note"]) == b.Note) {
				return nil
			}
		} else if !rules.Own() {
			return nil // "use the policy's rule" for all three, with nothing of their own before: nothing changes
		}
		doc := bson.M{"rules": rules, "note": timectxNullIf(b.Note), "by_id": strPtr(c.EmployeeID), "by_name": c.Name, "created_at": tx.Now()}
		what := "set"
		if old != nil {
			what = "replaced"
			if _, err := tx.C(store.EmpDevAllowColl).Update(ctx, bson.M{"_id": old["id"]}, bson.M{"$set": doc}); err != nil {
				return err
			}
		} else {
			doc["employee_id"], doc["effective_from"] = e.ID, b.EffectiveFrom
			if _, err := tx.C(store.EmpDevAllowColl).Insert(ctx, doc); err != nil {
				return err
			}
		}
		saved = true
		return audit(ctx, tx, c, fmt.Sprintf("Deviation allowances of %s (%s) %s from %s: %s", e.Name, e.ID, what, longDate(b.EffectiveFrom), devOwnText(rules)),
			store.EmpDevAllowColl, e.ID, old, doc)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"saved": saved}, nil
}

// checkDevOwn refuses a choice that is not one of the three, and an On rule without both numbers and an action or
// out of the policy's limits; the numbers and action of a rule that is not On are dropped.
func checkDevOwn(r *policy.DevOwnRules) error {
	asRules := policy.DevRules{}
	for _, x := range devOwnLabels {
		o := devOwnOf(r, x.key)
		o.Use = strings.TrimSpace(o.Use)
		switch o.Use {
		case "", policy.DevUsePolicy:
			o.Use, o.Mins, o.N, o.Act = policy.DevUsePolicy, 0, 0, ""
		case policy.DevUseOff:
			o.Mins, o.N, o.Act = 0, 0, ""
		case policy.DevUseOn:
		default:
			return bad("%s: choose Use the policy's rule, On or Off", x.label)
		}
		rr := policy.DevRule{On: o.Use == policy.DevUseOn, Mins: o.Mins, N: o.N, Act: o.Act}
		switch x.key {
		case "earlyIn":
			asRules.EarlyIn = rr
		case "lateIn":
			asRules.LateIn = rr
		default:
			asRules.EarlyOut = rr
		}
	}
	return checkDevAllow(asRules)
}
