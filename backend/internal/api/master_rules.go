package api

// The rules for adding and changing a master item, in one place: the Masters screen (Add, Edit, the switch) and
// the Master update workbook both go through saveMaster, retireMaster and reactivateMaster.
//
// Some masters are stored by NAME elsewhere (an employee's grade, a work policy mapped to a department, a role
// mapping's departments …). A rename rewrites every current reference in the same transaction (renameRefs); audit
// and history text keep the name they were written with. An employment type's name, a shift's code and the change
// reasons the app writes itself keep a fixed key, so they can be renamed but not moved to another use (appUse).

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/mdm"
	"github.com/saltstayz/hrms/internal/store"
)

// parentOf is the master an item belongs to, and the column that holds it.
var parentOf = map[string]struct{ field, master string }{
	"state": {"region_id", "region"}, "city": {"state_id", "state"}, "cluster": {"city_id", "city"},
	"location": {"cluster_id", "cluster"}, "designation": {"department_id", "department"},
}

// fixedName says why a simple master's name cannot change, or "" when it can.
func fixedName(kind string) string {
	if kind == "employment_type" {
		return "An employment type's name cannot change: pay rules and employee records use Permanent, Retainer and Contractual"
	}
	return ""
}

// appUse: a change reason or attendance mode the app writes itself has a fixed key (store.SysKeys), so it can be
// renamed like any other item; only what it is used for cannot change. "" for an ordinary item.
func appUse(x store.Row) string {
	key := str(x["sys_key"])
	if key == "" {
		return ""
	}
	return fmt.Sprintf("The app uses “%s” itself: %s. You can rename it; what it is used for cannot change", str(x["name"]), store.SysKeyWhy(key))
}

func conflict(msg string, a ...any) error {
	return &apiError{Status: 409, Message: fmt.Sprintf(msg, a...)}
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// masterRulesNoCase matches the whole of a text field ignoring case: master names are unique whatever their case.
func masterRulesNoCase(s string) bson.M {
	return bson.M{"$regex": "^" + regexp.QuoteMeta(s) + `\z`, "$options": "i"}
}

// masterRow reads one item of a master, locked for the change (a simple master only within its kind).
func masterRow(ctx context.Context, q store.Q, m master, id string) (store.Row, error) {
	if !reUUID.MatchString(id) {
		return nil, nil
	}
	filter := bson.M{"_id": id}
	if m.kind != "" {
		filter["kind"] = m.kind
	}
	return q.C(m.table).Lock(ctx, filter)
}

// impactCount is how many active employees use an item (shown before it is made inactive, or moved).
func impactCount(ctx context.Context, q store.Q, t, id string) (int64, error) {
	m, ok := masters[t]
	if !ok || m.impact == nil {
		return 0, nil
	}
	return m.impact(ctx, q, id)
}

func num64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	in := map[string]int{}
	for _, x := range a {
		in[x]++
	}
	for _, x := range b {
		if in[x] == 0 {
			return false
		}
		in[x]--
	}
	return true
}

// sameValue compares a stored value with a validated one (a time as HH:MM, an id as text, a list as a set).
func sameValue(cur, v any) bool {
	switch x := v.(type) {
	case nil:
		return cur == nil
	case string:
		return cur != nil && fmt.Sprint(cur) == x
	case float64:
		f, ok := num64(cur)
		return ok && f == x
	case bool:
		b, ok := cur.(bool)
		return ok && b == x
	case []string:
		return cur != nil && sameSet(strs(cur), x)
	}
	return false
}

// masterSaved is what saveMaster did.
type masterSaved struct {
	Before, After store.Row
	Refs          map[string]int64 // references to the old name rewritten, by place (renameRefs)
	Moved         int64            // active employees whose place moves with the item (a location to another cluster …)
	Changed       []string         // the fields that changed
}

// saveMaster adds (id "") or changes one item under the shared rules, with the rename cascade and, for a shift
// timing, a version from today; it is audited as the Masters screen does. vals come from master.validate; on a
// change only the values that differ are written.
func saveMaster(ctx context.Context, tx store.Q, c *Ctx, t, id string, vals map[string]any) (masterSaved, error) {
	var res masterSaved
	m, err := masterCfg(t)
	if err != nil {
		return res, err
	}
	if id != "" {
		if res.Before, err = masterRow(ctx, tx, m, id); err != nil {
			return res, err
		}
		if res.Before == nil {
			return res, notFound("")
		}
		for k, v := range vals {
			if sameValue(res.Before[k], v) {
				delete(vals, k)
			}
		}
		if len(vals) == 0 {
			res.After = res.Before
			return res, nil
		}
	}
	if err := checkMaster(ctx, tx, t, m, res.Before, vals); err != nil {
		return res, err
	}
	if res.Before != nil && len(vals) == 0 {
		res.After = res.Before
		return res, nil
	}
	for k := range vals {
		res.Changed = append(res.Changed, k)
	}
	if res.Before == nil {
		if m.kind == "employment_type" { // pay rules exist for the three built-in types only (spec 8.1, 13; PPL-133)
			return res, bad("Employment types are fixed: Permanent, Retainer and Contractual, each with its own pay rules")
		}
		if m.kind != "" {
			vals["kind"] = m.kind
		}
		newID, err := tx.C(m.table).Insert(ctx, bson.M(vals))
		if err != nil {
			return res, err
		}
		if res.After, err = tx.C(m.table).FindOne(ctx, bson.M{"_id": newID}); err != nil {
			return res, err
		}
		if t == "shift" { // its first version (Set shift)
			if err := ensureBaseVersion(ctx, tx, fmt.Sprint(res.After["id"])); err != nil {
				return res, err
			}
		}
		return res, audit(ctx, tx, c, "Added "+t, m.table, fmt.Sprint(res.After["id"]), nil, res.After)
	}
	_, st := vals["start_time"]
	_, en := vals["end_time"]
	_, br := vals["break_minutes"]
	timing := t == "shift" && (st || en || br)
	if timing { // keep the old timings as the version before today
		if err := ensureBaseVersion(ctx, tx, id); err != nil {
			return res, err
		}
	}
	if res.After, err = tx.C(m.table).FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M(vals)}); err != nil {
		return res, err
	}
	if timing { // a timing changed here applies from today, as a shift version (Set shift)
		// Today's version takes the shift's new timings; a version added today keeps its attendance rules.
		now := tx.Now()
		if _, err := tx.C("shift_version").Upsert(ctx, bson.M{"shift_id": id, "effective_from": store.DateOf(now)},
			bson.M{"start_time": res.After["start_time"], "end_time": res.After["end_time"], "break_minutes": res.After["break_minutes"],
				"created_by": c.Name + " (" + c.EmployeeID + ")", "created_at": now, "note": "Changed in the shift list"},
			bson.M{"rules": res.After["rules"]}); err != nil {
			return res, err
		}
	}
	if n, ok := vals["name"].(string); ok {
		if res.Refs, err = renameRefs(ctx, tx, c, t, res.Before, n); err != nil {
			return res, err
		}
	}
	if p, ok := parentOf[t]; ok && t != "designation" {
		if _, moved := vals[p.field]; moved {
			if res.Moved, err = impactCount(ctx, tx, t, id); err != nil {
				return res, err
			}
		}
	}
	after := store.Row{}
	for k, v := range res.After {
		after[k] = v
	}
	if len(res.Refs) > 0 {
		after["references_updated"] = res.Refs
	}
	return res, audit(ctx, tx, c, "Changed "+t, m.table, id, res.Before, after)
}

// checkMaster holds the rules beyond the field formats: names the app uses, unique names, active parents,
// grades, a designation's department, a shift's code and timing. before is nil for a new item; vals holds only
// what changes.
func checkMaster(ctx context.Context, tx store.Q, t string, m master, before store.Row, vals map[string]any) error {
	if t == "location" { // the masters lock of the master data SOP: CMS properties (internal/mdm)
		if err := mdm.CheckLocation(ctx, tx, before, vals); err != nil {
			return mdmErr(err)
		}
	}
	cur := func(k string) any {
		if v, ok := vals[k]; ok {
			return v
		}
		if before != nil {
			return before[k]
		}
		return nil
	}
	id := "00000000-0000-0000-0000-000000000000"
	if before != nil {
		id = fmt.Sprint(before["id"])
	}
	_, renamed := vals["name"]
	if before != nil && m.kind != "" {
		_, use := vals["used_for"]
		if why := fixedName(m.kind); why != "" && (renamed || use) {
			return bad("%s", why)
		}
		if why := appUse(before); why != "" && use {
			return bad("%s", why)
		}
	}
	if t == "shift" {
		if code, ok := vals["code"].(string); ok {
			if before != nil { // vals holds only what differs
				return bad("The code of a shift cannot change. Add a new shift instead")
			}
			if why := shiftCodeProblem(code); why != "" {
				return bad("%s", why)
			}
			if dup, err := tx.C("shift").Exists(ctx, bson.M{"code": masterRulesNoCase(code)}); err != nil || dup {
				return orErr(err, conflict("A shift with code %s already exists", code))
			}
		}
		if err := checkShiftTiming(before, vals); err != nil {
			return err
		}
		if ids, ok := vals["location_ids"].([]string); ok {
			if len(ids) == 0 {
				return bad("Pick at least one location, or all locations")
			}
			n, err := tx.C("location").Count(ctx, bson.M{"_id": bson.M{"$in": ids}})
			if err != nil || n != int64(len(uniq(ids))) {
				return orErr(err, bad("A location of the shift does not exist"))
			}
		}
	}
	// The parent must exist and be active when it is set or changed.
	p, hasParent := parentOf[t]
	_, parentChanged := vals[p.field]
	var parentName string
	if hasParent {
		pm := masters[p.master]
		pr, err := tx.C(pm.table).FindOne(ctx, bson.M{"_id": cur(p.field)}, options.FindOne().SetProjection(bson.M{"status": 1, "name": 1}))
		if err != nil {
			return err
		}
		if pr == nil {
			return bad("That %s does not exist", pm.label)
		}
		parentName = str(pr["name"])
		if parentChanged && str(pr["status"]) != "Active" {
			return bad("%s “%s” is inactive. Pick an active one", capital(pm.label), parentName)
		}
	}
	_, useChanged := vals["used_for"]
	if renamed || parentChanged || useChanged {
		name := str(cur("name"))
		var dup bool
		var err error
		where := ""
		switch {
		case t == "shift": // a shift is known by its code; names may repeat
		case m.kind != "":
			// the same name for the same use (no use matches no use)
			dup, err = tx.C("simple_master").Exists(ctx, bson.M{"kind": m.kind, "name": masterRulesNoCase(name), "used_for": cur("used_for"), "_id": bson.M{"$ne": id}})
			if u := str(cur("used_for")); u != "" {
				where = " for " + u
			}
		case hasParent && t != "state" && t != "location":
			dup, err = tx.C(m.table).Exists(ctx, bson.M{"name": masterRulesNoCase(name), p.field: cur(p.field), "_id": bson.M{"$ne": id}})
			where = " in " + parentName
		default:
			dup, err = tx.C(m.table).Exists(ctx, bson.M{"name": masterRulesNoCase(name), "_id": bson.M{"$ne": id}})
		}
		if err != nil {
			return err
		}
		if dup {
			return conflict("A %s named “%s” already exists%s", m.label, name, where)
		}
	}
	if t == "designation" {
		if g, ok := vals["grades"].([]string); ok {
			had := []string{}
			if before != nil {
				had = strs(before["grades"])
			}
			seen, out := map[string]bool{}, []string{}
			for _, x := range g {
				gr, err := masterRulesNamed(ctx, tx, "grade", nil, x)
				if err != nil {
					return err
				}
				if gr == nil {
					return bad("Grade %s is not in Grades", x)
				}
				name := str(gr["name"])
				if str(gr["status"]) != "Active" && !containsStr(had, name) {
					return bad("Grade %s is inactive. Pick an active grade", name)
				}
				if !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
			vals["grades"] = out
			if before != nil && sameValue(before["grades"], out) {
				delete(vals, "grades")
			}
		}
		if before != nil && parentChanged {
			var was string
			if d, _ := tx.C("department").FindOne(ctx, bson.M{"_id": before["department_id"]}, options.FindOne().SetProjection(bson.M{"name": 1})); d != nil {
				was = str(d["name"])
			}
			n, other, err := masterRulesHolders(ctx, tx, id, was, str(before["name"]))
			if err != nil {
				return err
			}
			if n > 0 {
				return bad("%d active employee%s hold this designation in %s; move them first with Bulk update, or add a new designation", n, plural(int(n)), was)
			}
			if other > 0 {
				return bad("%d joining employee%s, open offers or pending transfers use this designation in %s; finish or move them first, or add a new designation", other, plural(int(other)), was)
			}
		}
	}
	return nil
}

// masterRulesNamed is the item of table that matches filter and is named x whatever the case: the one written
// exactly as x first, else the first in master order; nil when there is none.
func masterRulesNamed(ctx context.Context, q store.Q, table string, filter bson.M, x string) (store.Row, error) {
	f := bson.M{"name": masterRulesNoCase(x)}
	for k, v := range filter {
		f[k] = v
	}
	l, err := q.C(table).Find(ctx, f, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
	if err != nil || len(l) == 0 {
		return nil, err
	}
	for _, r := range l {
		if str(r["name"]) == x {
			return r, nil
		}
	}
	return l[0], nil
}

// masterRulesHolders counts who still uses a designation (id, named name in department dept): its active employees,
// and the people on their way to it: employees before their first day, open offers (not joined, not declined, their
// approval neither rejected nor cancelled) and transfers to it waiting for approval.
func masterRulesHolders(ctx context.Context, q store.Q, id, dept, name string) (int64, int64, error) {
	emp := q.C("employee")
	active, err := emp.Count(ctx, bson.M{"designation_id": id, "status": "Active"})
	if err != nil {
		return 0, 0, err
	}
	joining, err := emp.Count(ctx, bson.M{"designation_id": id, "status": "Pre-boarding"})
	if err != nil {
		return 0, 0, err
	}
	offers, err := q.C("offer").Find(ctx, bson.M{"designation_id": id, "employee_id": nil, "declined_at": nil}, options.Find().SetProjection(bson.M{"request_id": 1}))
	if err != nil {
		return 0, 0, err
	}
	var reqIDs []any
	for _, o := range offers {
		if o["request_id"] != nil {
			reqIDs = append(reqIDs, o["request_id"])
		}
	}
	status := map[string]string{}
	if len(reqIDs) > 0 {
		reqs, err := q.C("approval_request").Find(ctx, bson.M{"_id": bson.M{"$in": reqIDs}}, options.Find().SetProjection(bson.M{"status": 1}))
		if err != nil {
			return 0, 0, err
		}
		for _, r := range reqs {
			status[str(r["id"])] = str(r["status"])
		}
	}
	for _, o := range offers {
		st, ok := status[str(o["request_id"])]
		if !ok { // no approval request (yet) counts as pending
			st = "Pending"
		}
		if st != "Rejected" && st != "Cancelled" {
			joining++
		}
	}
	moves, err := q.C("approval_request").Count(ctx, bson.M{"wf": "transfer", "status": "Pending", "payload.toDepartment": dept, "payload.toDesignation": name})
	if err != nil {
		return 0, 0, err
	}
	return active, joining + moves, nil
}

// checkShiftTiming: start and end differ, the break is shorter than the shift, and a shift whose rules say it ends
// the next day (or not) keeps times that agree (Set shift owns the rules).
func checkShiftTiming(before store.Row, vals map[string]any) error {
	_, a := vals["start_time"]
	_, b := vals["end_time"]
	_, c := vals["break_minutes"]
	if !a && !b && !c {
		return nil
	}
	get := func(k string) any {
		if v, ok := vals[k]; ok {
			return v
		}
		if before != nil {
			return before[k]
		}
		return nil
	}
	st, _ := attend.Min(str(get("start_time")))
	en, _ := attend.Min(str(get("end_time")))
	if st == en {
		return bad("Start and end cannot be the same")
	}
	length := en - st
	if length <= 0 {
		length += 1440
	}
	if brk, _ := num64(get("break_minutes")); brk >= float64(length) {
		return bad("Break minutes must be shorter than the shift")
	}
	if before != nil {
		if r, ok := before["rules"].(map[string]any); ok {
			if nd, ok := r["nextDay"].(bool); ok {
				if nd && en > st {
					return bad("This shift's rules say it ends the next day, so the end must be at or before the start. Change the timing in Set shift")
				}
				if !nd && en <= st {
					return bad("This shift's rules say it ends the same day, so the end must be after the start. Change the timing in Set shift")
				}
			}
		}
	}
	return nil
}

func uniq(l []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, x := range l {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func orErr(err, other error) error {
	if err != nil {
		return err
	}
	return other
}

// renameRefs rewrites the current references to a master that other records hold by NAME, after a rename from
// before["name"] to name. Audit and history text (employee_change of the past, roster_log, payslips, the audit log)
// keep the name they were written with. It returns the count changed by place.
func renameRefs(ctx context.Context, tx store.Q, c *Ctx, t string, before store.Row, name string) (map[string]int64, error) {
	old, id := str(before["name"]), fmt.Sprint(before["id"])
	out := map[string]int64{}
	var err error
	// run makes one change and counts the records it touched under key; after an error nothing more runs.
	run := func(key string, change func() (int64, error)) {
		if err != nil {
			return
		}
		n, e := change()
		if e != nil {
			err = fmt.Errorf("rename %s (%s): %w", t, key, e)
			return
		}
		if n > 0 {
			out[key] += n
		}
	}
	// rename sets field (a column, or a key inside a JSON column: "data.grade") to the new name in every record of
	// coll where it holds the old name and filter matches.
	rename := func(coll string, filter bson.M, field string) func() (int64, error) {
		return func() (int64, error) {
			f := bson.M{field: old}
			for k, v := range filter {
				f[k] = v
			}
			return tx.C(coll).UpdateMany(ctx, f, bson.M{"$set": bson.M{field: name}})
		}
	}
	// replaceIn replaces the old name in a list of names (an array column), keeping its order.
	replaceIn := func(coll, pk, field string) func() (int64, error) {
		return func() (int64, error) { return masterRulesReplaceIn(ctx, tx, coll, pk, field, old, name) }
	}
	// A job change waiting for its date shows the new name (and applies it, where the value is the name).
	scheduled := func(col, value string, byName bool) {
		if byName {
			run("scheduled job changes", func() (int64, error) {
				ec := tx.C("employee_change")
				if _, err := ec.UpdateMany(ctx, bson.M{"state": "Scheduled", "apply_column": col, "apply_value": old, "new_value": old},
					bson.M{"$set": bson.M{"new_value": name}}); err != nil {
					return 0, err
				}
				return ec.UpdateMany(ctx, bson.M{"state": "Scheduled", "apply_column": col, "apply_value": old}, bson.M{"$set": bson.M{"apply_value": name}})
			})
			return
		}
		run("scheduled job changes", rename("employee_change", bson.M{"state": "Scheduled", "apply_column": col, "apply_value": value}, "new_value"))
	}
	switch t {
	case "grade":
		run("employee records", rename("employee", nil, "grade"))
		run("offers", func() (int64, error) { // the offer's grade and the grade in its form data, counted once per offer
			ids, err := tx.C("offer").Distinct(ctx, "_id", bson.M{"$or": bson.A{bson.M{"grade": old}, bson.M{"data.grade": old}}})
			if err != nil || len(ids) == 0 {
				return 0, err
			}
			if _, err := rename("offer", nil, "grade")(); err != nil {
				return 0, err
			}
			if _, err := rename("offer", nil, "data.grade")(); err != nil {
				return 0, err
			}
			return int64(len(ids)), nil
		})
		run("pay bands", rename("pay_band", nil, "grade"))
		run("designations", replaceIn("designation", "id", "grades"))
		run("work policy mappings", rename("work_policy_map", bson.M{"level": "grade"}, "ref"))
		// Probation and notice by grade (work policy settings.notice.grade, keyed by the grade's name).
		run("work policy grade settings", func() (int64, error) { return masterRulesNoticeGrades(ctx, tx, old, name) })
		// Approval rules "Grade G3 and G4" list the grades by name (the rule's name is the admin's text and is kept).
		run("approval rules", func() (int64, error) { return masterRulesApprovalGrades(ctx, tx, old, name) })
		scheduled("grade", "", true)
		run("pending approvals", rename("approval_request", bson.M{"status": "Pending"}, "facts.grade"))
	case "department":
		run("work policy mappings", rename("work_policy_map", bson.M{"level": "department"}, "ref"))
		run("role mappings", replaceIn("user_role_map", "id", "departments"))
		run("onboarding documents", replaceIn("onb_document", "key", "depts"))
		run("company policies", func() (int64, error) { return masterRulesAudience(ctx, tx, old, name) })
		run("offers", rename("offer", nil, "data.dept"))
		run("roster publications", rename("roster_publish", nil, "department"))
		run("pending approvals", func() (int64, error) { // the transfer's department, and the facts shown with it
			if _, err := tx.C("approval_request").UpdateMany(ctx, bson.M{"status": "Pending", "payload.toDepartment": old, "facts.toDepartment": old},
				bson.M{"$set": bson.M{"facts.toDepartment": name}}); err != nil {
				return 0, err
			}
			return rename("approval_request", bson.M{"status": "Pending"}, "payload.toDepartment")()
		})
		scheduled("department_id", id, false)
	case "designation":
		dept, e := masterRulesDeptOf(ctx, tx, id)
		if e != nil {
			return nil, e
		}
		// Designation names repeat across departments, and a policy is mapped to the name. While another
		// designation still has the old name, its mapping stays and the new name gets the same policies.
		var shared bool
		if shared, err = tx.C("designation").Exists(ctx, bson.M{"name": old, "_id": bson.M{"$ne": id}}); err != nil {
			return nil, err
		}
		if shared {
			run("work policy mappings", func() (int64, error) { return masterRulesShareMaps(ctx, tx, old, name, c.Name) })
		} else {
			run("work policy mappings", rename("work_policy_map", bson.M{"level": "designation"}, "ref"))
		}
		run("offers", rename("offer", bson.M{"$or": bson.A{bson.M{"designation_id": id}, bson.M{"data.dept": dept}}}, "data.desig"))
		run("pending approvals", rename("approval_request", bson.M{"status": "Pending", "payload.toDepartment": dept}, "payload.toDesignation"))
		scheduled("designation_id", id, false)
	case "location":
		scheduled("location_id", id, false)
	case "attendance_mode":
		run("employee records", rename("employee", nil, "attendance_mode"))
		run("offers", rename("offer", nil, "data.attMode"))
		scheduled("attendance_mode", "", true)
	case "qualification":
		run("offers", rename("offer", nil, "data.qual"))
	case "job_role", "skill": // employees and offers hold them by name (b1_masters.go)
		col, key := jobMasterCol[t], jobMasterKey[t]
		run("employee records", rename("employee", nil, col))
		run("offers", rename("offer", nil, "data."+key))
		scheduled(col, "", true)
	case "change_reason":
		if str(before["used_for"]) == "Deduction" { // a deduction waiting for approval is booked under its type's name
			run("pending approvals", rename("approval_request", bson.M{"status": "Pending", "wf": "p_ded"}, "payload.dedType"))
		}
	}
	return out, err
}

// masterRulesReplaceIn replaces old by name in the list of names in field (an array column) of every record of coll
// that holds it, keeping the order. pk is the record's key ("id", or "key" for onb_document).
// It returns how many records changed.
func masterRulesReplaceIn(ctx context.Context, q store.Q, coll, pk, field, old, name string) (int64, error) {
	rows, err := q.C(coll).Find(ctx, bson.M{field: old}, options.Find().SetProjection(bson.M{pk: 1, field: 1}))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		l, _ := r[field].([]any)
		for i, x := range l {
			if x == old {
				l[i] = name
			}
		}
		if _, err := q.C(coll).Update(ctx, bson.M{pk: r[pk]}, bson.M{"$set": bson.M{field: l}}); err != nil {
			return 0, err
		}
	}
	return int64(len(rows)), nil
}

// masterRulesNoticeGrades moves the probation and notice a work policy keeps for a grade (settings.notice.grade,
// an object keyed by the grade's name) to the grade's new name. It returns how many policies changed.
func masterRulesNoticeGrades(ctx context.Context, q store.Q, old, name string) (int64, error) {
	rows, err := q.C("work_policy").Find(ctx, bson.M{"settings.notice.grade": bson.M{"$type": "object"}}, options.Find().SetProjection(bson.M{"settings.notice.grade": 1}))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range rows {
		set, _ := r["settings"].(map[string]any)
		notice, _ := set["notice"].(map[string]any)
		grades, _ := notice["grade"].(map[string]any)
		v, ok := grades[old]
		if !ok {
			continue
		}
		delete(grades, old)
		grades[name] = v
		if _, err := q.C("work_policy").Update(ctx, bson.M{"_id": r["id"]}, bson.M{"$set": bson.M{"settings.notice.grade": grades}}); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// masterRulesApprovalGrades renames a grade in the approval rules that list grades by name: a condition
// {"k": "grades", "list": [...]} of a rule. It returns how many rule lists changed.
func masterRulesApprovalGrades(ctx context.Context, q store.Q, old, name string) (int64, error) {
	rows, err := q.C(store.ApprovalFlowColl).Find(ctx, bson.M{"rules": bson.M{"$type": "array"}}, options.Find().SetProjection(bson.M{"wf": 1, "flow": 1, "rules": 1}))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, m := range rows {
		rules, _ := m["rules"].([]any)
		changed := false
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			when, _ := rule["when"].([]any)
			for _, c := range when {
				cond, _ := c.(map[string]any)
				list, ok := cond["list"].([]any)
				if !ok || cond["k"] != "grades" || !masterRulesHas(list, old) {
					continue
				}
				for i, g := range list {
					if g == old {
						list[i] = name
					}
				}
				changed = true
			}
		}
		if !changed {
			continue
		}
		if _, err := q.C(store.ApprovalFlowColl).Update(ctx, bson.M{"wf": m["wf"], "flow": m["flow"]}, bson.M{"$set": bson.M{"rules": rules}}); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// masterRulesHas says whether a JSON list holds the text s.
func masterRulesHas(l []any, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// masterRulesAudience renames a department in the audience of company policies (audience.departments, a list of
// names). It returns how many policies changed.
func masterRulesAudience(ctx context.Context, q store.Q, old, name string) (int64, error) {
	rows, err := q.C("company_policy").Find(ctx, bson.M{"audience.departments": old}, options.Find().SetProjection(bson.M{"audience.departments": 1}))
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range rows {
		aud, _ := r["audience"].(map[string]any)
		deps, ok := aud["departments"].([]any)
		if !ok || !masterRulesHas(deps, old) {
			continue
		}
		for i, d := range deps {
			if d == old {
				deps[i] = name
			}
		}
		if _, err := q.C("company_policy").Update(ctx, bson.M{"_id": r["id"]}, bson.M{"$set": bson.M{"audience.departments": deps}}); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// masterRulesShareMaps gives a designation's new name the work policies mapped (on) to its old name, which another
// designation still has; a policy already mapped to the new name is left as it is. It returns the mappings added.
func masterRulesShareMaps(ctx context.Context, q store.Q, old, name, by string) (int64, error) {
	wm := q.C("work_policy_map")
	pids, err := wm.Distinct(ctx, "policy_id", bson.M{"level": "designation", "ref": old, "on_map": true})
	if err != nil {
		return 0, err
	}
	docs := []any{}
	for _, p := range pids {
		has, err := wm.Exists(ctx, bson.M{"policy_id": p, "level": "designation", "ref": name, "on_map": true})
		if err != nil {
			return 0, err
		}
		if !has {
			docs = append(docs, bson.M{"policy_id": p, "level": "designation", "ref": name, "created_by": by})
		}
	}
	if err := wm.InsertMany(ctx, docs); err != nil {
		return 0, err
	}
	return int64(len(docs)), nil
}

// masterRulesDeptOf is the name of the department a designation is in.
func masterRulesDeptOf(ctx context.Context, q store.Q, id string) (string, error) {
	g, err := q.C("designation").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"department_id": 1}))
	if err != nil || g == nil {
		return "", orErr(err, store.ErrNoRows)
	}
	d, err := q.C("department").FindOne(ctx, bson.M{"_id": g["department_id"]}, options.FindOne().SetProjection(bson.M{"name": 1}))
	if err != nil || d == nil {
		return "", orErr(err, store.ErrNoRows)
	}
	return str(d["name"]), nil
}

// retireMaster marks an item inactive from a date (today or later) with a reason. Nothing is ever deleted; an item
// still in use can be retired, and the active employees still using it are counted.
func retireMaster(ctx context.Context, tx store.Q, c *Ctx, t, id, since, reason string) (store.Row, int64, error) {
	m, err := masterCfg(t)
	if err != nil {
		return nil, 0, err
	}
	if since == "" {
		since = today()
	}
	if !isDate(since) || since < today() {
		return nil, 0, bad("Inactive from must be today or a future date")
	}
	if len(strings.TrimSpace(reason)) < 3 {
		return nil, 0, bad("Give a reason")
	}
	before, err := masterRow(ctx, tx, m, id)
	if err != nil || before == nil {
		return nil, 0, orErr(err, notFound(""))
	}
	// Items the app writes itself (a separation reason, the default attendance mode …) and the three built-in
	// employment types, whose pay rules the app knows, stay active (PPL-127).
	if key := str(before["sys_key"]); key != "" {
		return nil, 0, bad("“%s” cannot be made inactive: %s. Rename it if the wording should change", str(before["name"]), store.SysKeyWhy(key))
	}
	if m.kind == "employment_type" && containsStr(empTypes, str(before["name"])) {
		return nil, 0, bad("“%s” is a built-in employment type with its own pay rules and cannot be made inactive", str(before["name"]))
	}
	n, err := impactCount(ctx, tx, t, id)
	if err != nil {
		return nil, 0, err
	}
	// The item is inactive at once when the date has come; a later date only schedules it.
	set := bson.M{"inactive_since": since, "inactive_reason": strings.TrimSpace(reason)}
	if since <= store.DateOf(tx.Now()) {
		set["status"] = "Inactive"
	}
	out, err := tx.C(m.table).FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	if err != nil {
		return nil, 0, err
	}
	out["activeEmployeesUsingIt"] = n
	return out, n, audit(ctx, tx, c, "Marked "+t+" inactive", m.table, id, before, out)
}

// reactivateMaster makes an item active again (or cancels a retirement waiting for its date).
func reactivateMaster(ctx context.Context, tx store.Q, c *Ctx, t, id string) (store.Row, error) {
	m, err := masterCfg(t)
	if err != nil {
		return nil, err
	}
	before, err := masterRow(ctx, tx, m, id)
	if err != nil || before == nil {
		return nil, orErr(err, notFound(""))
	}
	if t == "location" { // a location whose CMS property is inactive is reactivated in the CMS
		if err := mdm.CheckReactivate(ctx, tx, before); err != nil {
			return nil, mdmErr(err)
		}
	}
	row, err := tx.C(m.table).FindOneAndUpdate(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Active", "inactive_since": nil, "inactive_reason": nil}})
	if err != nil {
		return nil, err
	}
	return row, audit(ctx, tx, c, "Reactivated "+t, m.table, id, nil, nil)
}
