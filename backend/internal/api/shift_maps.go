package api

// Where a shift applies (owner, 24 Nov 2026: "Shift applies to Location and Department, and also add bulk upload in
// that"): a list of Department + Location rows on the shift (store/schema_shiftmap.go), either side "All". A shift is
// offered to, and valid for, an employee when one of its switched-on rows matches their department and location:
// roster codes and the cell picker, the roster upload, the shift pickers of Onboarding and joining verification, an
// employee's job, Bulk update and Master update, and every other check of a shift's validity.
//
// Rows are added, switched on or off, and removed on Set shift (POST/PUT/DELETE /api/shifts/{id}/maps…) or by Bulk
// update › Shift mapping (bulk_shiftmap.go). Every change is audited and goes into the shift's mapping history
// (config_history, entity "shift"), shown on Set shift. A row is not versioned by date: it takes effect at once, as the
// shift's locations did. The same pair twice on a shift is refused, and the last switched-on row cannot be switched
// off or removed (a shift without one would be offered to nobody: make the shift inactive instead).
//
// People already on a shift that their mapping no longer allows keep it: their past and planned roster cells and
// their default shift do not change (timectx.go baseShift). A new assignment of the shift to them is refused.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/store"
)

// shiftMapsOn are the switched-on rows of the given shifts (store rows with an id), by shift id. A shift without one
// gets an empty list: it is offered to nobody.
func shiftMapsOn(ctx context.Context, q store.Q, shifts []store.Row) (map[string][]attend.ShiftMap, error) {
	out := map[string][]attend.ShiftMap{}
	ids := []string{}
	for _, r := range shifts {
		id := str(r["id"])
		out[id] = []attend.ShiftMap{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.C(store.ShiftMapColl).Find(ctx, bson.D{{Key: "shift_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "on_map", Value: true}},
		options.Find().SetProjection(bson.D{{Key: "shift_id", Value: 1}, {Key: "department_id", Value: 1}, {Key: "location_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		id := str(m["shift_id"])
		out[id] = append(out[id], attend.ShiftMap{Dept: str(m["department_id"]), Loc: str(m["location_id"])})
	}
	return out, nil
}

// shiftRowFor says whether the stored shift (a row with an id) has a switched-on row for the department and location.
func shiftRowFor(ctx context.Context, q store.Q, shiftID, deptID, locID string) (bool, error) {
	return q.C(store.ShiftMapColl).Exists(ctx, bson.D{{Key: "shift_id", Value: shiftID}, {Key: "on_map", Value: true},
		{Key: "department_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, deptID}}}}, {Key: "location_id", Value: bson.D{{Key: "$in", Value: bson.A{nil, locID}}}}})
}

// shiftNotForMsg is the refusal of a shift its mapping does not allow for the department and location (names).
func shiftNotForMsg(code, dept, loc string) string {
	return fmt.Sprintf("Shift %s does not apply to %s at %s. Choose a shift mapped to the department and location, or add the row on the shift (Masters › Shifts)", code, or(dept, "this department"), or(loc, "this location"))
}

// listFor are the active shifts offered to the employee.
func (t *timeSet) listFor(e *emp) []attend.Shift {
	out := []attend.Shift{}
	for _, sh := range t.list {
		if sh.RunsFor(e.DeptID, e.LocID) {
			out = append(out, sh)
		}
	}
	return out
}

// codesFor are the roster codes the employee can be given: the shifts offered to them, then WO and PH.
func (t *timeSet) codesFor(e *emp) []string {
	out := []string{}
	for _, sh := range t.listFor(e) {
		out = append(out, sh.Code)
	}
	return append(out, "WO", "PH")
}

// shiftFor says whether the active shift code is offered to the employee.
func (t *timeSet) shiftFor(code string, e *emp) bool {
	sh, ok := t.shifts[code]
	return ok && sh.RunsFor(e.DeptID, e.LocID)
}

// ---------- Rows as shown ----------

// shiftMapNames reads department and location names (all of them: a row may name an inactive one).
type shiftMapNames struct{ dept, loc map[string]string }

func loadShiftMapNames(ctx context.Context, q store.Q) (shiftMapNames, error) {
	n := shiftMapNames{dept: map[string]string{}, loc: map[string]string{}}
	for coll, into := range map[string]map[string]string{"department": n.dept, "location": n.loc} {
		rows, err := q.C(coll).Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}}))
		if err != nil {
			return n, err
		}
		for _, r := range rows {
			into[str(r["id"])] = str(r["name"])
		}
	}
	return n, nil
}

func (n shiftMapNames) deptName(id string) string {
	if id == "" {
		return "All departments"
	}
	return or(n.dept[id], id)
}

func (n shiftMapNames) locName(id string) string {
	if id == "" {
		return "all locations"
	}
	return or(n.loc[id], id)
}

// label reads a row: "Front Office at Gurgaon office", "All departments at all locations".
func (n shiftMapNames) label(dept, loc string) string {
	return n.deptName(dept) + " at " + n.locName(loc)
}

// shiftMapRows are the shift's rows for Set shift: switched-on first, then by department and location.
func shiftMapRows(ctx context.Context, q store.Q, shiftID string, names shiftMapNames) ([]map[string]any, error) {
	rows, err := q.C(store.ShiftMapColl).Find(ctx, bson.D{{Key: "shift_id", Value: shiftID}})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range rows {
		d, l := str(r["department_id"]), str(r["location_id"])
		out = append(out, map[string]any{"id": r["id"], "departmentId": d, "department": names.deptName(d), "locationId": l, "location": or(map[bool]string{true: "All locations"}[l == ""], names.locName(l)),
			"label": names.label(d, l), "on": r["on_map"] == true, "createdBy": str(r["created_by"]), "createdAt": r["created_at"], "changedBy": str(r["changed_by"]), "changedAt": r["changed_at"]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i]["on"].(bool), out[j]["on"].(bool); a != b {
			return a
		}
		return fmt.Sprint(out[i]["label"]) < fmt.Sprint(out[j]["label"])
	})
	return out, nil
}

// shiftMapHistory is the shift's mapping history, newest first.
func shiftMapHistory(ctx context.Context, q store.Q, shiftID string) ([]store.Row, error) {
	rows, err := q.C("config_history").Find(ctx, bson.D{{Key: "entity", Value: "shift"}, {Key: "entity_id", Value: shiftID}},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(200))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, r := range rows {
		out = append(out, store.Row{"at": r["at"], "by": r["by_name"], "changes": r["changes"]})
	}
	return out, nil
}

// shiftOutside counts the active employees on the shift (code) whom its switched-on rows (shiftID) do not cover.
func shiftOutside(ctx context.Context, q store.Q, shiftID, code string) (int, error) {
	emps, err := q.C("employee").Find(ctx, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}, {Key: "shift_code", Value: code}},
		options.Find().SetProjection(bson.D{{Key: "department_id", Value: 1}, {Key: "location_id", Value: 1}}))
	if err != nil || len(emps) == 0 {
		return 0, err
	}
	on, err := shiftMapsOn(ctx, q, []store.Row{{"id": shiftID}})
	if err != nil {
		return 0, err
	}
	sh := attend.Shift{Maps: on[shiftID]}
	n := 0
	for _, e := range emps {
		if !sh.RunsFor(str(e["department_id"]), str(e["location_id"])) {
			n++
		}
	}
	return n, nil
}

// outsideNote says how many people on the shift its rows no longer cover ("" for none).
func outsideNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s on this shift %s outside its mapping now: their roster stays, a new assignment of the shift to them is refused",
		n, map[bool]string{true: "person", false: "people"}[n == 1], map[bool]string{true: "is", false: "are"}[n == 1])
}

// ---------- Changes ----------

type shiftMapIn struct {
	DepartmentID string `json:"departmentId"` // "" = all departments
	LocationID   string `json:"locationId"`   // "" = all locations
}

// checkShiftMapPair checks a pair to switch on: an active department and an active location, or "" for all.
func checkShiftMapPair(ctx context.Context, q store.Q, in shiftMapIn) error {
	for _, x := range []struct{ coll, id, label string }{{"department", in.DepartmentID, "Department"}, {"location", in.LocationID, "Location"}} {
		if x.id == "" {
			continue
		}
		r, err := q.C(x.coll).FindOne(ctx, bson.D{{Key: "_id", Value: x.id}}, options.FindOne().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "status", Value: 1}}))
		if err != nil {
			return err
		}
		if r == nil {
			return bad("%s not found. Pick one from the list, or All", x.label)
		}
		if str(r["status"]) != "Active" {
			return bad("%s %s is inactive. Pick an active one, or All", x.label, str(r["name"]))
		}
	}
	return nil
}

// shiftPairFilter matches the shift's row for the pair (null for "all").
func shiftPairFilter(shiftID string, in shiftMapIn) bson.D {
	return bson.D{{Key: "shift_id", Value: shiftID}, {Key: "department_id", Value: timectxNullIf(in.DepartmentID)}, {Key: "location_id", Value: timectxNullIf(in.LocationID)}}
}

// addShiftMap adds the pair to the shift (switched on), or refuses a pair the shift already has.
func addShiftMap(ctx context.Context, tx store.Q, c *Ctx, shiftID string, in shiftMapIn, names shiftMapNames) error {
	in.DepartmentID, in.LocationID = strings.TrimSpace(in.DepartmentID), strings.TrimSpace(in.LocationID)
	if err := checkShiftMapPair(ctx, tx, in); err != nil {
		return err
	}
	label := names.label(in.DepartmentID, in.LocationID)
	x, err := tx.C(store.ShiftMapColl).FindOne(ctx, shiftPairFilter(shiftID, in))
	if err != nil {
		return err
	}
	if x != nil {
		if x["on_map"] == true {
			return conflict("%s is already on this shift", label)
		}
		return conflict("%s is already on this shift, switched off. Switch it on instead", label)
	}
	if _, err := tx.C(store.ShiftMapColl).Insert(ctx, bson.M{"shift_id": shiftID, "department_id": timectxNullIf(in.DepartmentID), "location_id": timectxNullIf(in.LocationID),
		"created_by": c.Name + " (" + c.EmployeeID + ")"}); err != nil {
		return err
	}
	return nil
}

// shiftMapCtx is the shift a mapping change is about, read in the transaction.
func shiftMapCtx(ctx context.Context, tx store.Q, id string) (store.Row, error) {
	sh, err := tx.C("shift").FindOne(ctx, bson.D{{Key: "_id", Value: id}}, options.FindOne().SetProjection(bson.D{{Key: "code", Value: 1}, {Key: "name", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return nil, err
	}
	if sh == nil {
		return nil, notFound("Shift not found")
	}
	return sh, nil
}

// lastOnRefusal refuses switching off or removing the shift's last switched-on row.
func lastOnRefusal(ctx context.Context, tx store.Q, shiftID, rowID string) error {
	n, err := tx.C(store.ShiftMapColl).Count(ctx, bson.D{{Key: "shift_id", Value: shiftID}, {Key: "on_map", Value: true}, {Key: "_id", Value: bson.D{{Key: "$ne", Value: rowID}}}})
	if err != nil {
		return err
	}
	if n == 0 {
		return bad("This is the shift's last row switched on: without one the shift is offered to nobody. Add the new row first, or make the shift inactive")
	}
	return nil
}

// shiftMapAdd: POST /api/shifts/{id}/maps {departmentId, locationId}: the shift applies there too.
func (s *Server) shiftMapAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 2); err != nil {
		return nil, err
	}
	var b shiftMapIn
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sh, err := shiftMapCtx(ctx, tx, id)
		if err != nil {
			return err
		}
		names, err := loadShiftMapNames(ctx, tx)
		if err != nil {
			return err
		}
		if err := addShiftMap(ctx, tx, c, id, b, names); err != nil {
			return err
		}
		label := names.label(strings.TrimSpace(b.DepartmentID), strings.TrimSpace(b.LocationID))
		if err := configHistory(ctx, tx, c, "shift", id, []map[string]any{{"field": "Applies to: " + label, "from": "", "to": "On", "note": "Added"}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Shift mapping added", "shift", id, nil, map[string]any{"shift": sh["code"], "applies to": label})
	})
	if err != nil {
		return nil, err
	}
	return shiftView(ctx, s.st.DB, id)
}

// shiftMapSwitch: PUT /api/shifts/{id}/maps/{mapId} {on}: switches a row on or off (kept either way).
func (s *Server) shiftMapSwitch(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 2); err != nil {
		return nil, err
	}
	var b struct {
		On *bool `json:"on"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.On == nil {
		return nil, bad("Say whether the row is on")
	}
	ctx, id, mid := r.Context(), r.PathValue("id"), r.PathValue("mapId")
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sh, err := shiftMapCtx(ctx, tx, id)
		if err != nil {
			return err
		}
		row, err := tx.C(store.ShiftMapColl).FindOne(ctx, bson.D{{Key: "_id", Value: mid}, {Key: "shift_id", Value: id}})
		if err != nil {
			return err
		}
		if row == nil {
			return notFound("Row not found on this shift")
		}
		if (row["on_map"] == true) == *b.On {
			return nil // as it is
		}
		names, err := loadShiftMapNames(ctx, tx)
		if err != nil {
			return err
		}
		in := shiftMapIn{DepartmentID: str(row["department_id"]), LocationID: str(row["location_id"])}
		if *b.On {
			if err := checkShiftMapPair(ctx, tx, in); err != nil {
				return err
			}
		} else if err := lastOnRefusal(ctx, tx, id, mid); err != nil {
			return err
		}
		if _, err := tx.C(store.ShiftMapColl).Update(ctx, bson.D{{Key: "_id", Value: mid}}, bson.M{"$set": bson.M{"on_map": *b.On,
			"changed_by": c.Name + " (" + c.EmployeeID + ")", "changed_at": tx.Now()}}); err != nil {
			return err
		}
		label := names.label(in.DepartmentID, in.LocationID)
		from, to := map[bool]string{true: "Off", false: "On"}[*b.On], map[bool]string{true: "On", false: "Off"}[*b.On]
		n, err := shiftOutside(ctx, tx, id, str(sh["code"]))
		if err != nil {
			return err
		}
		out = map[string]any{"outside": n}
		if err := configHistory(ctx, tx, c, "shift", id, []map[string]any{{"field": "Applies to: " + label, "from": from, "to": to, "note": outsideNote(n)}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Shift mapping switched "+strings.ToLower(to), "shift", id, map[string]any{"applies to": label, "on": !*b.On},
			map[string]any{"shift": sh["code"], "applies to": label, "on": *b.On, "outside": n})
	})
	if err != nil {
		return nil, err
	}
	v, err := shiftView(ctx, s.st.DB, id)
	if v != nil && out != nil {
		v["outsideNow"] = out["outside"]
	}
	return v, err
}

// shiftMapRemove: DELETE /api/shifts/{id}/maps/{mapId}: removes a row (the history keeps it).
func (s *Server) shiftMapRemove(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("mas.job", 2); err != nil {
		return nil, err
	}
	ctx, id, mid := r.Context(), r.PathValue("id"), r.PathValue("mapId")
	var n int
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sh, err := shiftMapCtx(ctx, tx, id)
		if err != nil {
			return err
		}
		row, err := tx.C(store.ShiftMapColl).FindOne(ctx, bson.D{{Key: "_id", Value: mid}, {Key: "shift_id", Value: id}})
		if err != nil {
			return err
		}
		if row == nil {
			return notFound("Row not found on this shift")
		}
		if row["on_map"] == true {
			if err := lastOnRefusal(ctx, tx, id, mid); err != nil {
				return err
			}
		}
		if _, err := tx.C(store.ShiftMapColl).Delete(ctx, bson.D{{Key: "_id", Value: mid}}); err != nil {
			return err
		}
		names, err := loadShiftMapNames(ctx, tx)
		if err != nil {
			return err
		}
		label := names.label(str(row["department_id"]), str(row["location_id"]))
		if n, err = shiftOutside(ctx, tx, id, str(sh["code"])); err != nil {
			return err
		}
		from := map[bool]string{true: "On", false: "Off"}[row["on_map"] == true]
		if err := configHistory(ctx, tx, c, "shift", id, []map[string]any{{"field": "Applies to: " + label, "from": from, "to": "Removed", "note": outsideNote(n)}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Shift mapping removed", "shift", id, map[string]any{"shift": sh["code"], "applies to": label, "on": row["on_map"] == true},
			map[string]any{"outside": n})
	})
	if err != nil {
		return nil, err
	}
	v, err := shiftView(ctx, s.st.DB, id)
	if v != nil {
		v["outsideNow"] = n
	}
	return v, err
}

// createShiftMaps writes a new shift's rows (Add shift): at least one, no pair twice.
func createShiftMaps(ctx context.Context, tx store.Q, c *Ctx, shiftID string, rows []shiftMapIn) ([]string, error) {
	if len(rows) == 0 {
		return nil, fieldErr("maps", "Add at least one row: the department and location the shift applies to (either may be All)")
	}
	names, err := loadShiftMapNames(ctx, tx)
	if err != nil {
		return nil, err
	}
	labels := []string{}
	for _, in := range rows {
		if err := addShiftMap(ctx, tx, c, shiftID, in, names); err != nil {
			if ae, ok := err.(*apiError); ok {
				return nil, fieldErr("maps", "%s", ae.Message)
			}
			return nil, err
		}
		labels = append(labels, names.label(strings.TrimSpace(in.DepartmentID), strings.TrimSpace(in.LocationID)))
	}
	ch := []map[string]any{}
	for _, l := range labels {
		ch = append(ch, map[string]any{"field": "Applies to: " + l, "from": "", "to": "On", "note": "Added with the shift"})
	}
	return labels, configHistory(ctx, tx, c, "shift", shiftID, ch)
}

// shiftAllEverywhere gives a shift added without rows (the Masters list's generic Add, Master update) the row "All
// departments at all locations", as a shift added there without locations applied everywhere.
func shiftAllEverywhere(ctx context.Context, tx store.Q, c *Ctx, shiftID string) error {
	if has, err := tx.C(store.ShiftMapColl).Exists(ctx, bson.D{{Key: "shift_id", Value: shiftID}}); err != nil || has {
		return err
	}
	if _, err := tx.C(store.ShiftMapColl).Insert(ctx, bson.M{"shift_id": shiftID, "department_id": nil, "location_id": nil, "created_by": c.Name + " (" + c.EmployeeID + ")"}); err != nil {
		return err
	}
	return configHistory(ctx, tx, c, "shift", shiftID, []map[string]any{{"field": "Applies to: All departments at all locations", "from": "", "to": "On", "note": "Added with the shift"}})
}

// shiftApplies are each shift's switched-on rows, by shift id: their labels, and the pairs of ids ("" = all) the
// forms filter their shift pickers with (the Masters list, the Master update workbook).
func shiftApplies(ctx context.Context, q store.Q) (map[string][]string, map[string][]map[string]string, error) {
	names, err := loadShiftMapNames(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.C(store.ShiftMapColl).Find(ctx, bson.D{{Key: "on_map", Value: true}})
	if err != nil {
		return nil, nil, err
	}
	labels, pairs := map[string][]string{}, map[string][]map[string]string{}
	for _, r := range rows {
		id, d, l := str(r["shift_id"]), str(r["department_id"]), str(r["location_id"])
		labels[id] = append(labels[id], names.label(d, l))
		pairs[id] = append(pairs[id], map[string]string{"departmentId": d, "locationId": l})
	}
	for _, l := range labels {
		sort.Strings(l)
	}
	return labels, pairs, nil
}
