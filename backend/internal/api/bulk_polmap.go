package api

// Bulk update "Work policy mapping" (owner, 24 Nov 2026): Department + Location mappings of work policies, one pair a
// row. Columns Work Policy, Department, Location, Active (Yes / No, blank Yes) and Move (Yes / No, blank No).
//
//   - Active Yes: the pair already on that policy is no change; on another policy it is an error ("Already applied
//     to …") unless Move is Yes, when it moves (switched off there, on here; history on both, audited); on no policy
//     it is switched on (or added) here.
//   - Active No: the pair is switched off on that policy (an error when it is not on there). Never deleted.
//   - An unknown policy, department or location is an error; the same pair twice in a file is refused on the later row.
//
// A bulk file cannot ask questions: people the pair covers who get another policy through a different level follow
// the order of the levels (Department + Location comes before Location and Department). Each row says how many
// people's work policy it changes. Right: editing work policies (pol.policies at Edit).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

var polMapCols = []string{"Work Policy", "Department", "Location", "Active", "Move"}

func init() {
	bulkTemplates = append(bulkTemplates, &bulkTemplate{Key: "polmap", Name: "Work policy mapping", Data: true, Cols: polMapCols, Optional: []string{"Active", "Move"},
		Hint:  "Department + Location mappings of work policies, one pair a row. Active Yes (or blank) puts the pair on the work policy; a pair already on another policy is refused unless Move is Yes, which moves it here. Active No switches the pair off on that policy (it must be on there). A pair is on one policy at a time and appears once in a file. People the pair covers follow the order of the levels: Department + Location comes before the location or the department alone. Each row says how many people's work policy changes. Download the current data for the pairs mapped now.",
		allow: func(c *Ctx) bool { return c.Level("pol.policies") >= 2 },
		lists: map[string]bulkList{"Work Policy": bulkQuery("work_policy", "name", bson.D{{Key: "status", Value: "Active"}}, bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}}),
			"Department": bulkActive("department", "name"), "Location": bulkActive("location", "name"), "Active": bulkFixed("Yes", "No"), "Move": bulkFixed("Yes", "No")},
		key:     func(v map[string]string) string { return v["Department"] + " at " + v["Location"] },
		how:     polMapHow,
		current: polMapCurrent,
		row:     bulkPolMap})
}

func polMapHow(context.Context, store.Q) []string {
	return []string{
		"## Columns",
		"Work Policy: the policy's name. Department and Location: the pair it applies to (People Ops at Gurgaon office).",
		"Active: Yes or blank puts the pair on the policy; No switches it off there.",
		"Move: Yes moves a pair that is on another policy to this one; blank or No refuses it (\"Already applied to …\").",
		"## Example",
		"Corporate office – 5.5 day | People Ops | Gurgaon office | Yes | (blank)",
		"## What happens",
		"Check lists each row's change and how many people's work policy it changes; Apply saves the rows without problems. Each policy's History shows the change.",
	}
}

// polMapCurrent is the pairs mapped now (switched on), by policy, department and location.
func polMapCurrent(ctx context.Context, s *Server, c *Ctx) ([]string, [][]any, error) {
	q := s.st.DB
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	locs := map[string]string{}
	rows, err := q.C("location").Find(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, l := range rows {
		locs[str(l["id"])] = str(l["name"])
	}
	out := [][]any{}
	for _, p := range ts.all {
		for _, m := range p.Maps {
			if !m.On || m.Level != "dept_location" {
				continue
			}
			if d, l, ok := splitCombo(m.Ref); ok {
				out = append(out, []any{p.Name, d, or(locs[l], l), "Yes", ""})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := fmt.Sprint(out[i][0], out[i][1], out[i][2]), fmt.Sprint(out[j][0], out[j][1], out[j][2])
		return a < b
	})
	return polMapCols, out, nil
}

// bulkYN reads a Yes / No column (blank gives def).
func bulkYN(v string, def bool) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return def, true
	case "yes", "y":
		return true, true
	case "no", "n":
		return false, true
	}
	return false, false
}

func bulkPolMap(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	res := bulkResult{ID: r.V["Department"] + " at " + r.V["Location"], Name: r.V["Work Policy"]}
	pol, err := bulkFindCI(ctx, tx, "work_policy", "name", r.V["Work Policy"], nil, "name", "status")
	if err != nil {
		return res, err
	}
	dept, err := bulkFindCI(ctx, tx, "department", "name", r.V["Department"], nil, "name")
	if err != nil {
		return res, err
	}
	loc, err := bulkFindCI(ctx, tx, "location", "name", r.V["Location"], nil, "name")
	if err != nil {
		return res, err
	}
	if pol == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("Work policy “%s” not found", or(r.V["Work Policy"], "(blank)")))
	}
	if dept == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("Department “%s” not found", or(r.V["Department"], "(blank)")))
	}
	if loc == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("Location “%s” not found", or(r.V["Location"], "(blank)")))
	}
	active, okA := bulkYN(r.V["Active"], true)
	move, okM := bulkYN(r.V["Move"], false)
	if !okA {
		res.Errors = append(res.Errors, "Active must be Yes or No")
	}
	if !okM {
		res.Errors = append(res.Errors, "Move must be Yes or No")
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	pid, pname := str(pol["id"]), str(pol["name"])
	res.ID, res.Name = str(dept["name"])+" at "+str(loc["name"]), pname
	if active && str(pol["status"]) != "Active" {
		res.Errors = append(res.Errors, fmt.Sprintf("Work policy “%s” is inactive", pname))
		return res, nil
	}
	const level = "dept_location"
	ref := comboRef(str(dept["name"]), str(loc["id"]))
	ts, err := loadTimeSet(ctx, tx) // fresh: it sees the rows before this one
	if err != nil {
		return res, err
	}
	var here, other *workPolicy
	onHere := false
	for _, p := range ts.all {
		if p.ID == pid {
			here = p
		}
		for _, m := range p.Maps {
			if m.On && m.Level == level && m.Ref == ref {
				if p.ID == pid {
					onHere = true
				} else if other == nil {
					other = p
				}
			}
		}
	}
	if here == nil {
		return res, notFound("Policy not found")
	}
	switch {
	case active && onHere:
		return res, nil // already on this policy: no change
	case active && other != nil && !move:
		res.Errors = append(res.Errors, fmt.Sprintf("Already applied to %s. Put Move = Yes to move it to %s, or switch it off there", other.Name, pname))
		return res, nil
	case !active && !onHere:
		why := fmt.Sprintf("Not applied to %s, so there is nothing to switch off", pname)
		if other != nil {
			why = fmt.Sprintf("Not applied to %s (it is applied to %s)", pname, other.Name)
		}
		res.Errors = append(res.Errors, why)
		return res, nil
	}
	// The people of the pair and the policy each gets now; after the change, worked out on the same time set.
	emps, err := loadEmps(ctx, tx, bson.M{"status": bson.M{"$ne": "Inactive"}, "location_id": loc["id"], "department_id": dept["id"]})
	if err != nil {
		return res, err
	}
	before := map[string]string{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		before[e.ID] = p.ID
	}
	setOn := func(p *workPolicy, on bool) {
		found := false
		for i := range p.Maps {
			if p.Maps[i].Level == level && p.Maps[i].Ref == ref {
				p.Maps[i].On, found = on, true
			}
		}
		if !found && on {
			p.Maps = append(p.Maps, polMap{Level: level, Ref: ref, On: true})
		}
	}
	wm := tx.C("work_policy_map")
	hist := map[string][]map[string]any{}
	field := "applies to " + level + "|" + ref
	if active {
		if other != nil { // Move = Yes: off on the policy that has it
			if _, err := wm.UpdateMany(ctx, bson.M{"policy_id": other.ID, "level": level, "ref": ref, "on_map": true}, bson.M{"$set": bson.M{"on_map": false}}); err != nil {
				return res, err
			}
			setOn(other, false)
			mv := map[string]any{"field": "mapping moved", "from": other.Name, "to": pname, "note": res.ID + " (Bulk update)"}
			hist[other.ID] = append(hist[other.ID], map[string]any{"field": field, "from": "on", "to": "off"}, mv)
			hist[pid] = append(hist[pid], mv)
			res.Changes = append(res.Changes, bulkChange{Field: "Work policy", From: other.Name, To: pname})
		} else {
			res.Changes = append(res.Changes, bulkChange{Field: "Applies to " + pname, From: "Off", To: "On"})
		}
		x, err := wm.FindOne(ctx, bson.M{"policy_id": pid, "level": level, "ref": ref})
		if err != nil {
			return res, err
		}
		if x != nil {
			_, err = wm.Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"on_map": true}})
		} else {
			_, err = wm.Insert(ctx, bson.M{"policy_id": pid, "level": level, "ref": ref, "created_by": c.Name})
		}
		if err != nil {
			return res, err
		}
		setOn(here, true)
		hist[pid] = append([]map[string]any{{"field": field, "from": "off", "to": "on"}}, hist[pid]...)
	} else {
		if _, err := wm.UpdateMany(ctx, bson.M{"policy_id": pid, "level": level, "ref": ref, "on_map": true}, bson.M{"$set": bson.M{"on_map": false}}); err != nil {
			return res, err
		}
		setOn(here, false)
		hist[pid] = append(hist[pid], map[string]any{"field": field, "from": "on", "to": "off"})
		res.Changes = append(res.Changes, bulkChange{Field: "Applies to " + pname, From: "On", To: "Off"})
	}
	n := 0
	for _, e := range emps {
		if p, _ := ts.policyFor(e); p.ID != before[e.ID] {
			n++
		}
	}
	res.Changes[0].Note = fmt.Sprintf("%d %s work policy changes", n, map[bool]string{true: "person's", false: "people's"}[n == 1])
	pids := []string{}
	for p := range hist {
		pids = append(pids, p)
	}
	sort.Strings(pids)
	for _, p := range pids {
		if err := configHistory(ctx, tx, c, "work_policy", p, hist[p]); err != nil {
			return res, err
		}
	}
	return res, audit(ctx, tx, c, "Work policy mapping (Bulk update): "+res.ID, "work_policy", pid, nil,
		map[string]any{"policy": pname, "department": dept["name"], "location": loc["name"], "active": active, "move": move, "people": n})
}
