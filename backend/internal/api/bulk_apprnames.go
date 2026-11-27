package api

// Bulk update "Approver names" (owner, 26 Nov 2026: "Approver names missing … and 196 more" cannot be filled one by
// one). Opened from the red "Approver names missing" box on Approvals › Approval flow, and listed in Bulk update.
//
// The template is the missing names themselves: one row per gap the box lists (buildDirectory's gaps: a role at a
// property, the Function Head of a department, the HRBP of a corporate department, a company-wide role), the same for
// every request type, so a slot several flows use is one row. Needed By says which flows use the role. Employee is a
// drop-down of the people "Add names" offers (Access › Role mapping: every active employee), written "Name (EMP-…)".
//
// A name is stored the way Role mapping stores it: one mapping per person and role (user_role_map).
//   - A person without that role gets a new mapping: a property → Selected locations with that property; a
//     department → All locations with that department (Function Head, corporate HRBP); a company-wide role → All
//     locations (as the seed maps them).
//   - A person who holds the role by selected locations gets the property added; one who holds it by department gets
//     the department added. A switched-off mapping, a mapping by cluster, city or state, or a property HRBP asked for a
//     corporate department (or the other way round) is refused: change those in Access › Role mapping.
//   - The same checks as Role mapping: an active employee, an assignable role, no overlap with another holder. A slot
//     someone else holds by now is refused; the same person again is no change. A blank Employee is skipped.
//
// Right: Role mapping at Edit (acc.users), as for Map a role. Each change is audited like Map a role / Edit.

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	slotProperty    = "Property"
	slotDept        = "Department"
	slotCorpDept    = "Corporate department"
	slotCompany     = "Company"
	slotCompanyCode = "COMPANY"
)

var apprNameCols = []string{"Role", "Level", "Place", "Place Code", "Where", "Needed By", "Employee"}

func init() {
	bulkTemplates = append(bulkTemplates, &bulkTemplate{Key: "apprnames", Name: "Approver names", Cols: apprNameCols,
		Optional: []string{"Place", "Where", "Needed By"}, fill: true,
		Hint:  "The approver names missing in the approval flows, one row each (a role at a property, a department's Function Head, a corporate department's HRBP, a company-wide role). Pick the person in Employee and upload the file; a blank Employee is skipped. The name is saved as on Access › Role mapping: a person who already holds the role by selected locations or departments gets this place added, anyone else gets a new mapping (Selected locations for a property, All locations for a department or a company-wide role). A role mapping is also data access: the person sees the records of the places it covers.",
		allow: func(c *Ctx) bool { return c.Level("acc.users") >= 2 },
		lists: map[string]bulkList{"Employee": apprNameEmployees},
		key: func(v map[string]string) string {
			return strings.TrimSpace(v["Role"]) + "|" + strings.TrimSpace(v["Level"]) + "|" + strings.TrimSpace(v["Place Code"])
		},
		how:     apprNameHow,
		current: apprNameCurrent,
		row:     bulkApprName})
}

func apprNameHow(context.Context, store.Q) []string {
	return []string{
		"## Columns",
		"Role, Level, Place and Place Code: the missing name. Keep them as downloaded. Where and Needed By are for reading only.",
		"Employee: pick the person from the drop-down (Name (Employee ID)). An Employee ID alone also works. Leave it blank to skip the row.",
		"## What happens",
		"Check lists each name it would set; Apply saves the rows without problems, in file order. A person picked for several properties of the same role gets one mapping covering them all.",
		"Refused: a person who is not active, a slot someone else holds by now, a place that is no longer active, an overlap with another holder, and a mapping that has to be changed by hand on Access › Role mapping (switched off, by cluster, city or state, or a property HRBP for a corporate department).",
	}
}

// apprNameEmployees is the Employee drop-down: every active employee, as the Role mapping picker offers them.
func apprNameEmployees(ctx context.Context, q store.Q) ([]string, error) {
	rows, err := q.C("employee").Find(ctx, bson.D{{Key: "status", Value: "Active"}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, e := range rows {
		out = append(out, str(e["name"])+" ("+str(e["id"])+")")
	}
	return out, nil
}

// apprNeededBy is, for each approving role, the flows whose rules in use name it ("Joining (Operations), …").
func apprNeededBy(ctx context.Context, q store.Q) (map[string]string, error) {
	flows, err := loadFlows(ctx, q)
	if err != nil {
		return nil, err
	}
	lists, seen := map[string][]string{}, map[string]bool{}
	for _, fl := range flows {
		label := healthWF(fl.WF) + " (" + fl.Flow + ")"
		for i, r := range fl.Rules {
			if !fl.inUse(i) {
				continue
			}
			for _, st := range r.Approvers {
				role := approval.StageRole(st)
				if k := role + "|" + label; !seen[k] {
					seen[k] = true
					lists[role] = append(lists[role], label)
				}
			}
		}
	}
	out := map[string]string{}
	for k, v := range lists {
		out[k] = strings.Join(v, ", ")
	}
	return out, nil
}

// apprNameCurrent is the template's rows: the gaps of the approver directory, in its order.
func apprNameCurrent(ctx context.Context, s *Server, _ *Ctx) ([]string, [][]any, error) {
	d, err := buildDirectory(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	need, err := apprNeededBy(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	out := make([][]any, 0, len(d.slots))
	for _, x := range d.slots {
		out = append(out, []any{x.Role, x.Level, x.Place, x.PlaceID, x.Path, need[x.Role], ""})
	}
	return apprNameCols, out, nil
}

// apprPick returns the value of list that v names, ignoring case ("" when none).
func apprPick(v string, list ...string) string {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(v), x) {
			return x
		}
	}
	return ""
}

// apprEmpID reads "Name (EMP-0001)" or a bare Employee ID.
func apprEmpID(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.LastIndex(v, "("); i >= 0 && strings.HasSuffix(v, ")") {
		v = v[i+1 : len(v)-1]
	}
	return strings.ToUpper(strings.TrimSpace(v))
}

func bulkApprName(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	roleIn, levelIn := strings.TrimSpace(r.V["Role"]), strings.TrimSpace(r.V["Level"])
	code, placeIn, empIn := strings.TrimSpace(r.V["Place Code"]), strings.TrimSpace(r.V["Place"]), strings.TrimSpace(r.V["Employee"])
	res := bulkResult{ID: or(roleIn, "(blank)"), Name: or(placeIn, code)}
	if empIn == "" {
		return res, nil // nobody picked: skipped
	}
	sl, msg, err := apprSlotFind(ctx, tx, roleIn, levelIn, code, placeIn)
	if err != nil || msg != "" {
		if msg != "" {
			res.Errors = append(res.Errors, msg)
		}
		return res, err
	}
	res.ID, res.Name = sl.Role, sl.Label
	e, msg, err := apprSlotPerson(ctx, tx, apprEmpID(empIn), empIn)
	if err != nil || msg != "" {
		if msg != "" {
			res.Errors = append(res.Errors, msg)
		}
		return res, err
	}
	ch, msg, err := apprSlotSet(ctx, tx, c, sl, e, "Bulk update: approver names")
	if msg != "" {
		res.Errors = append(res.Errors, msg)
	}
	if ch != nil {
		res.Changes = append(res.Changes, *ch)
	}
	return res, err
}

// apprSlot is one approver name of the directory: a role at a property, a department, a corporate department or the
// company, resolved against the masters.
type apprSlot struct {
	Role, Level, PlaceID, Place, Label string
	Corp                               bool
}

// apprSlotFind resolves a role, level and place (its code, else its name) as the Approver names tab lists them.
// msg is a refusal for the person (the row or the cell), err a failure.
func apprSlotFind(ctx context.Context, tx store.Q, roleIn, levelIn, code, placeIn string) (sl apprSlot, msg string, err error) {
	level := apprPick(levelIn, slotProperty, slotDept, slotCorpDept, slotCompany)
	var role string
	switch level {
	case slotProperty:
		role = apprPick(roleIn, locationRoleList...)
	case slotDept:
		role = apprPick(roleIn, "Function Head")
	case slotCorpDept:
		role = apprPick(roleIn, "HRBP")
	case slotCompany:
		role = apprPick(roleIn, companyRoleList...)
	default:
		return sl, "Level must be Property, Department, Corporate department or Company. Keep the row as downloaded", nil
	}
	if role == "" {
		return sl, fmt.Sprintf("“%s” is not an approver role at the %s level. Keep the row as downloaded", or(roleIn, "(blank)"), strings.ToLower(level)), nil
	}
	sl.Role, sl.Level = role, level
	switch level {
	case slotProperty:
		loc, err := bulkFindCI(ctx, tx, "location", "_id", code, nil, "name", "status")
		if err == nil && loc == nil && code == "" && placeIn != "" {
			loc, err = bulkFindCI(ctx, tx, "location", "name", placeIn, nil, "name", "status")
		}
		if err != nil {
			return sl, "", err
		}
		if loc == nil || str(loc["status"]) != "Active" {
			return sl, fmt.Sprintf("Property “%s” not found or not active", or(placeIn, code)), nil
		}
		sl.PlaceID, sl.Place = str(loc["id"]), str(loc["name"])
	case slotDept, slotCorpDept:
		dp, err := bulkFindCI(ctx, tx, "department", "_id", code, nil, "name", "status", "corporate")
		if err == nil && dp == nil && code == "" && placeIn != "" {
			dp, err = bulkFindCI(ctx, tx, "department", "name", placeIn, nil, "name", "status", "corporate")
		}
		if err != nil {
			return sl, "", err
		}
		if dp == nil || str(dp["status"]) != "Active" {
			return sl, fmt.Sprintf("Department “%s” not found or not active", or(placeIn, code)), nil
		}
		sl.PlaceID, sl.Place, sl.Corp = str(dp["id"]), str(dp["name"]), dp["corporate"] == true
		if level == slotCorpDept && !sl.Corp {
			return sl, fmt.Sprintf("%s is not a corporate department: its HRBP is the property HRBP", sl.Place), nil
		}
	default:
		sl.PlaceID, sl.Place = slotCompanyCode, "the whole company"
	}
	sl.Label = role + " for " + sl.Place
	if level == slotCompany {
		sl.Label = role
	}
	return sl, "", nil
}

// apprSlotPerson finds the employee picked (by Employee ID); typed is what the person wrote, for the refusal.
func apprSlotPerson(ctx context.Context, tx store.Q, id, typed string) (store.Row, string, error) {
	e, err := bulkFindCI(ctx, tx, "employee", "_id", id, nil, "name", "status")
	if err != nil {
		return nil, "", err
	}
	if e == nil || id == "" {
		return nil, fmt.Sprintf("Employee “%s” not found. Pick a name from the list", typed), nil
	}
	return e, "", nil
}

// apprSlotHolder is who holds the slot now, as the Approver names tab decides it.
func apprSlotHolder(ctx context.Context, tx store.Q, sl apprSlot) (Person, error) {
	maps, err := loadMappings(ctx, tx, sl.Role)
	if err != nil {
		return unassigned, err
	}
	switch sl.Level {
	case slotProperty:
		return pickHolder(sl.Role, &subject{LocationID: sl.PlaceID}, maps), nil
	case slotDept:
		return pickHolder(sl.Role, &subject{Department: sl.Place, Corporate: sl.Corp}, maps), nil
	case slotCorpDept:
		for _, m := range maps {
			if containsStr(m.depts, sl.Place) {
				return Person{m.id, m.name}, nil
			}
		}
		return unassigned, nil
	}
	return pickHolder(sl.Role, &subject{}, maps), nil
}

// apprSlotSet names e (an employee row) for an empty slot, the way Role mapping stores it: the person's mapping of the
// role gets the place added, or a new mapping is made. A slot someone else holds is refused (msg); the same person
// again is no change (nil). how names the screen in the audit ("Bulk update: approver names", "Approver names").
func apprSlotSet(ctx context.Context, tx store.Q, c *Ctx, sl apprSlot, e store.Row, how string) (*bulkChange, string, error) {
	role, level, placeID, place, label := sl.Role, sl.Level, sl.PlaceID, sl.Place, sl.Label
	empID, name := str(e["id"]), str(e["name"])
	who := name + " (" + empID + ")"
	if str(e["status"]) != "Active" {
		return nil, who + " is not active. Only active employees can hold a role", nil
	}
	if err := roleAssignable(ctx, tx, role); err != nil {
		return nil, "", err
	}
	holder, err := apprSlotHolder(ctx, tx, sl)
	if err != nil {
		return nil, "", err
	}
	if holder.ID == empID {
		return nil, "", nil // named already
	}
	if holder.ID != "" {
		return nil, fmt.Sprintf("%s is %s now. Change it on Access › Role mapping", label, holder.Name+" ("+holder.ID+")"), nil
	}
	// The person's mapping of the role: added to, or a new one.
	cur, err := tx.C("user_role_map").FindOne(ctx, bson.M{"employee_id": empID, "role_name": role})
	if err != nil {
		return nil, "", err
	}
	byHand := func(why string) (*bulkChange, string, error) {
		return nil, who + " " + why + ". Change it on Access › Role mapping", nil
	}
	var m roleMap
	note := ""
	if cur == nil {
		m = roleMap{EmployeeID: empID, Role: role, Access: "all", LocationIDs: []string{}, Departments: []string{}}
		switch level {
		case slotProperty:
			m.Access, m.LocationIDs, note = "locs", []string{placeID}, "New "+role+" mapping: Selected locations ("+place+")"
		case slotDept, slotCorpDept:
			m.Departments, note = []string{place}, "New "+role+" mapping: All locations, department "+place
		default:
			note = "New " + role + " mapping: All locations"
		}
	} else {
		if cur["active"] != true {
			return byHand("has a switched-off " + role + " mapping")
		}
		m = accessRoleMapOf(cur)
		switch level {
		case slotProperty:
			if len(m.Departments) > 0 {
				return byHand("holds " + role + " for departments, not for properties")
			}
			if m.Access != "locs" {
				return byHand("holds " + role + " by " + strings.TrimSuffix(m.Access, "s") + ", and " + place + " is outside it")
			}
			m.LocationIDs = append(append([]string{}, m.LocationIDs...), placeID)
		case slotDept, slotCorpDept:
			if level == slotCorpDept && len(m.Departments) == 0 {
				return byHand("is HRBP for properties; a corporate department's HRBP is mapped by department")
			}
			m.Departments = append(append([]string{}, m.Departments...), place)
		default:
			return nil, "", nil // an active company-wide mapping makes them the holder already
		}
		note = "Added to " + name + "'s " + role + " mapping"
	}
	ov, err := overlaps(ctx, tx, m)
	if err != nil {
		return nil, "", err
	}
	if len(ov) > 0 {
		return nil, "Overlap not allowed. " + ov[0], nil
	}
	ch := &bulkChange{Field: label, From: "Not mapped", To: who, Note: note}
	if cur == nil { // as Map a role (roleMapCreate)
		doc := bson.M{"employee_id": empID, "role_name": role, "access": m.Access, "location_ids": m.LocationIDs, "departments": m.Departments, "created_by": c.Name}
		mid, err := tx.C("user_role_map").Insert(ctx, doc)
		if err != nil {
			return nil, "", err
		}
		row, err := tx.C("user_role_map").FindOne(ctx, bson.M{"_id": mid})
		if err != nil {
			return nil, "", err
		}
		accessStripArea(row)
		return ch, "", audit(ctx, tx, c, "Mapped "+name+" as "+role+" ("+how+")", "user_role_map", fmt.Sprint(row["id"]), nil, row)
	}
	// As Edit on Role mapping (roleMapUpdate).
	set := bson.M{"access": m.Access, "location_ids": m.LocationIDs, "departments": m.Departments, "active": true, "role_name": m.Role}
	row, err := tx.C("user_role_map").FindOneAndUpdate(ctx, bson.M{"_id": m.ID}, bson.M{"$set": set})
	if err != nil {
		return nil, "", err
	}
	accessStripArea(cur)
	accessStripArea(row)
	return ch, "", audit(ctx, tx, c, "Changed role mapping ("+how+")", "user_role_map", m.ID, cur, row)
}
