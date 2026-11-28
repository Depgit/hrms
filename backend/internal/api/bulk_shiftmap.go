package api

// Bulk update "Shift mapping" (owner, 24 Nov 2026: "Shift applies to Location and Department, and also add bulk upload
// in that"): where each shift applies, one Department + Location row a line (shift_maps.go). Columns Shift Code,
// Department, Location (blank or "All" = all of them) and Active (Yes / No, blank Yes). Opened from Masters › Shifts ›
// Bulk upload, and listed in Bulk update.
//
//   - Active Yes: a row the shift has switched on is no change; one switched off is switched on; a new pair is added.
//   - Active No: the shift's row is switched off (an error when the shift does not have it; already off is no change).
//     The shift's last switched-on row is refused, as on Set shift. Nothing is deleted.
//   - An unknown shift, department or location is an error; switching on needs an active shift, department and
//     location. The same row twice in a file is refused on the later line.
//
// Each change goes into the shift's mapping history and the audit, and says how many people on the shift its rows no
// longer cover (they keep their roster; a new assignment is refused). Right: changing shifts (mas.job at Edit).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

var shiftMapCols = []string{"Shift Code", "Department", "Location", "Active"}

func init() {
	active := bson.D{{Key: "status", Value: "Active"}}
	bulkTemplates = append(bulkTemplates, &bulkTemplate{Key: "shiftmap", Name: "Shift mapping", Data: true, Cols: shiftMapCols, Optional: []string{"Active"},
		Hint:  "Where each shift applies, one Department + Location row a line. A blank or All Department means all departments, a blank or All Location all locations (All departments at Noida; Front Office at all locations). Active Yes (or blank) adds the row to the shift, or switches it on; No switches it off (the shift must have it). A row appears once in a file, and a shift keeps at least one row switched on. People already on a shift that no row covers any more keep it: their roster stays, a new assignment of the shift to them is refused. Download the current data for the rows switched on now.",
		allow: func(c *Ctx) bool { return c.Level("mas.job") >= 2 },
		lists: map[string]bulkList{"Shift Code": bulkQuery("shift", "code", active, bson.D{{Key: "code", Value: 1}}),
			"Department": bulkQuery("department", "name", active, bson.D{{Key: "name", Value: 1}}, "All"),
			"Location":   bulkQuery("location", "name", active, bson.D{{Key: "name", Value: 1}}, "All"), "Active": bulkFixed("Yes", "No")},
		key:     shiftMapKey,
		how:     shiftMapHow,
		current: shiftMapCurrent,
		row:     bulkShiftMap})
}

// shiftMapAll: a blank or "All" department or location means all of them.
func shiftMapAll(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.EqualFold(v, "All")
}

// shiftMapKey is what makes two lines of a file the same row: the shift and the pair.
func shiftMapKey(v map[string]string) string {
	side := func(x string) string {
		if shiftMapAll(x) {
			return "all"
		}
		return strings.TrimSpace(x)
	}
	return strings.TrimSpace(v["Shift Code"]) + "|" + side(v["Department"]) + "|" + side(v["Location"])
}

func shiftMapHow(context.Context, store.Q) []string {
	return []string{
		"## Columns",
		"Shift Code: the shift's code (G, N2). Department and Location: the pair it applies to; blank or All means all of them.",
		"Active: Yes or blank adds the row to the shift or switches it on; No switches it off.",
		"## Example",
		"N | Front Office | Gurgaon office | Yes",
		"G | All | Noida | Yes",
		"G | All | All | No",
		"## What happens",
		"Check lists each row's change and how many people on the shift its rows no longer cover; Apply saves the rows without problems, in file order (add the new rows before switching off the old one). Each shift's mapping history on Set shift shows the change.",
	}
}

// shiftMapCurrent is the rows switched on now, of the active shifts, by shift code, department and location.
func shiftMapCurrent(ctx context.Context, s *Server, c *Ctx) ([]string, [][]any, error) {
	q := s.st.DB
	shifts, err := q.C("shift").Find(ctx, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	code := map[string]string{}
	for _, sh := range shifts {
		code[str(sh["id"])] = str(sh["code"])
	}
	names, err := loadShiftMapNames(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.C(store.ShiftMapColl).Find(ctx, bson.D{{Key: "on_map", Value: true}})
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, r := range rows {
		k, ok := code[str(r["shift_id"])]
		if !ok {
			continue
		}
		d, l := "All", "All"
		if id := str(r["department_id"]); id != "" {
			d = names.deptName(id)
		}
		if id := str(r["location_id"]); id != "" {
			l = names.locName(id)
		}
		out = append(out, []any{k, d, l, "Yes"})
	}
	key := func(x []any) string { return fmt.Sprintf("%s|%s|%s", x[0], x[1], x[2]) }
	sort.SliceStable(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return shiftMapCols, out, nil
}

func bulkShiftMap(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	code := strings.ToUpper(strings.TrimSpace(r.V["Shift Code"]))
	deptIn, locIn := strings.TrimSpace(r.V["Department"]), strings.TrimSpace(r.V["Location"])
	res := bulkResult{ID: or(code, "(blank)"), Name: or(deptIn, "All") + " at " + or(locIn, "All")}
	sh, err := bulkFindCI(ctx, tx, "shift", "code", code, nil, "code", "status")
	if err != nil {
		return res, err
	}
	if sh == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("Shift “%s” not found", or(code, "(blank)")))
	}
	var dept, loc store.Row
	if !shiftMapAll(deptIn) {
		if dept, err = bulkFindCI(ctx, tx, "department", "name", deptIn, nil, "name", "status"); err != nil {
			return res, err
		}
		if dept == nil {
			res.Errors = append(res.Errors, fmt.Sprintf("Department “%s” not found. Leave it blank or write All for all departments", deptIn))
		}
	}
	if !shiftMapAll(locIn) {
		if loc, err = bulkFindCI(ctx, tx, "location", "name", locIn, nil, "name", "status"); err != nil {
			return res, err
		}
		if loc == nil {
			res.Errors = append(res.Errors, fmt.Sprintf("Location “%s” not found. Leave it blank or write All for all locations", locIn))
		}
	}
	active, okA := bulkYN(r.V["Active"], true)
	if !okA {
		res.Errors = append(res.Errors, "Active must be Yes or No")
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	shiftID, code := str(sh["id"]), str(sh["code"])
	in := shiftMapIn{DepartmentID: str(dept["id"]), LocationID: str(loc["id"])}
	names, err := loadShiftMapNames(ctx, tx)
	if err != nil {
		return res, err
	}
	label := names.label(in.DepartmentID, in.LocationID)
	res.ID, res.Name = code, label
	if active {
		if str(sh["status"]) != "Active" {
			res.Errors = append(res.Errors, fmt.Sprintf("Shift %s is inactive", code))
		}
		for _, x := range []struct {
			row  store.Row
			what string
		}{{dept, "Department"}, {loc, "Location"}} {
			if x.row != nil && str(x.row["status"]) != "Active" {
				res.Errors = append(res.Errors, fmt.Sprintf("%s “%s” is inactive", x.what, str(x.row["name"])))
			}
		}
		if len(res.Errors) > 0 {
			return res, nil
		}
	}
	row, err := tx.C(store.ShiftMapColl).FindOne(ctx, shiftPairFilter(shiftID, in))
	if err != nil {
		return res, err
	}
	on := row != nil && row["on_map"] == true
	switch {
	case active && on, !active && row != nil && !on:
		return res, nil // as it is
	case !active && row == nil:
		res.Errors = append(res.Errors, fmt.Sprintf("%s is not on shift %s, so there is nothing to switch off", label, code))
		return res, nil
	case !active:
		if err := lastOnRefusal(ctx, tx, shiftID, str(row["id"])); err != nil {
			return res, err
		}
	}
	by, field := c.Name+" ("+c.EmployeeID+")", "Applies to: "+label
	from, to := "Off", "On"
	switch {
	case row == nil:
		from = ""
		if _, err := tx.C(store.ShiftMapColl).Insert(ctx, bson.M{"shift_id": shiftID, "department_id": timectxNullIf(in.DepartmentID), "location_id": timectxNullIf(in.LocationID),
			"created_by": by}); err != nil {
			return res, err
		}
	default:
		if !active {
			from, to = "On", "Off"
		}
		if _, err := tx.C(store.ShiftMapColl).Update(ctx, bson.D{{Key: "_id", Value: row["id"]}}, bson.M{"$set": bson.M{"on_map": active, "changed_by": by, "changed_at": tx.Now()}}); err != nil {
			return res, err
		}
	}
	n, err := shiftOutside(ctx, tx, shiftID, code)
	if err != nil {
		return res, err
	}
	note := outsideNote(n)
	res.Changes = append(res.Changes, bulkChange{Field: "Shift " + code + " applies to " + label, From: or(from, "—"), To: to, Note: note})
	if err := configHistory(ctx, tx, c, "shift", shiftID, []map[string]any{{"field": field, "from": from, "to": to, "note": strings.TrimSpace("Bulk update. " + note)}}); err != nil {
		return res, err
	}
	return res, audit(ctx, tx, c, "Shift mapping (Bulk update): "+code+" "+label, "shift", shiftID, nil,
		map[string]any{"shift": code, "applies to": label, "active": active, "outside": n})
}
