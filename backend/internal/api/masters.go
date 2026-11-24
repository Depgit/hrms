package api

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/mdm"
	"github.com/saltstayz/hrms/internal/store"
)

// Field kinds: text, uuid, int, num, bool, texts, uuids, time, enum.
type field struct {
	name     string
	kind     string
	required bool
	enum     []string
	lbl      string // the name shown in messages (the column of the Master update workbook)
}

// label is the field's name in messages: "Region", "Start", "Break minutes".
func (f field) label() string {
	if f.lbl != "" {
		return f.lbl
	}
	return strings.ToUpper(f.name[:1]) + strings.ReplaceAll(f.name[1:], "_", " ")
}

type master struct {
	table  string
	kind   string // simple_master kind
	right  string
	fields []field
	list   mastersLister // the Masters screen's list
	impact mastersImpact // counts active employees still using an item (nil: none can)
	label  string        // one item, in messages: "region", "change reason"
}

// mastersImpact counts the active employees still using the item id.
type mastersImpact func(ctx context.Context, q store.Q, id string) (int64, error)

// mastersLister reads the items of a master's collection that match, in order, as the Masters screen lists them.
type mastersLister func(ctx context.Context, q store.Q, table string, match, order bson.D) ([]store.Row, error)

// mastersJoin adds a parent's name (and more) to every listed item, as the list's JOIN did: the parent is the
// document of collection from whose id is in the field local (of the item, or of an earlier parent: "_k.city_id"),
// joined as `as`; set holds the keys it adds.
type mastersJoin struct {
	from, local, as string
	set             bson.D
}

// mastersList lists every column of the items plus the joined parents' keys. Like an inner JOIN, an item whose
// parent is missing is left out.
func mastersList(joins ...mastersJoin) mastersLister {
	return func(ctx context.Context, q store.Q, table string, match, order bson.D) ([]store.Row, error) {
		pipe := bson.A{bson.D{{Key: "$match", Value: match}}}
		var drop bson.A
		for _, j := range joins {
			pipe = append(pipe,
				bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: j.from}, {Key: "localField", Value: j.local}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: j.as}}}},
				bson.D{{Key: "$unwind", Value: "$" + j.as}},
				bson.D{{Key: "$set", Value: j.set}})
			drop = append(drop, j.as)
		}
		if len(drop) > 0 {
			pipe = append(pipe, bson.D{{Key: "$unset", Value: drop}})
		}
		pipe = append(pipe, bson.D{{Key: "$sort", Value: order}})
		return q.C(table).Aggregate(ctx, pipe)
	}
}

// mastersShiftList lists shifts with their times as HH:MM and their versions (Set shift): effective_from is the
// version in effect today, next_from a version waiting for its date, versions how many there are (1 for a shift
// written before versions).
func mastersShiftList(ctx context.Context, q store.Q, table string, match, order bson.D) ([]store.Row, error) {
	shifts, err := q.C(table).Find(ctx, match, options.Find().SetSort(order))
	if err != nil {
		return nil, err
	}
	day := store.Today()
	vers, err := q.C("shift_version").Aggregate(ctx, bson.A{
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$shift_id"},
			{Key: "cur", Value: bson.D{{Key: "$max", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$lte", Value: bson.A{"$effective_from", day}}}, "$effective_from", nil}}}}}},
			{Key: "nxt", Value: bson.D{{Key: "$min", Value: bson.D{{Key: "$cond", Value: bson.A{bson.D{{Key: "$gt", Value: bson.A{"$effective_from", day}}}, "$effective_from", nil}}}}}},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: int64(1)}}}}}},
	})
	if err != nil {
		return nil, err
	}
	byShift := map[string]store.Row{}
	for _, v := range vers {
		byShift[str(v["id"])] = v
	}
	applies, pairs, err := shiftApplies(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]store.Row, 0, len(shifts))
	for _, x := range shifts {
		row := store.Row{"effective_from": nil, "next_from": nil, "versions": int64(1)}
		for _, k := range []string{"id", "code", "name", "start_time", "end_time", "start2_time", "end2_time", "break_minutes", "status", "inactive_since",
			"inactive_reason", "created_at", "rules"} {
			row[k] = x[k]
		}
		// Where it applies: its switched-on Department + Location rows (shift_maps.go), as text and as id pairs ("" = all).
		row["applies"], row["maps"] = applies[str(x["id"])], pairs[str(x["id"])]
		if row["applies"] == nil {
			row["applies"], row["maps"] = []string{}, []map[string]string{}
		}
		if v, ok := byShift[str(x["id"])]; ok {
			row["effective_from"], row["next_from"], row["versions"] = v["cur"], v["nxt"], v["n"]
		}
		out = append(out, row)
	}
	return out, nil
}

// mastersPlaceImpact counts the active employees whose location is under an organisation item, following the
// chain of collections from the item down to the locations ({collection, field holding the item above}).
func mastersPlaceImpact(chain ...[2]string) mastersImpact {
	return func(ctx context.Context, q store.Q, id string) (int64, error) {
		ids := []any{id}
		for _, step := range chain {
			l, err := q.C(step[0]).Distinct(ctx, "_id", bson.M{step[1]: bson.M{"$in": ids}})
			if err != nil {
				return 0, err
			}
			ids = l
		}
		return q.C("employee").Count(ctx, bson.M{"location_id": bson.M{"$in": ids}, "status": "Active"})
	}
}

// mastersIDImpact counts the active employees holding the item's id in field.
func mastersIDImpact(field string) mastersImpact {
	return func(ctx context.Context, q store.Q, id string) (int64, error) {
		return q.C("employee").Count(ctx, bson.M{field: id, "status": "Active"})
	}
}

// mastersNameImpact counts the active employees holding the item's value of col (its name, a shift's code) in field.
func mastersNameImpact(table, col, field string) mastersImpact {
	return func(ctx context.Context, q store.Q, id string) (int64, error) {
		x, err := q.C(table).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{col: 1}))
		if err != nil || x == nil {
			return 0, err
		}
		return q.C("employee").Count(ctx, bson.M{field: x[col], "status": "Active"})
	}
}

var (
	reUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	reTime = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

// masters lists every master: its collection, the fields the Masters screen and the workbook may write, how it is
// listed (with the names of its parents) and how its impact is counted.
var masters = map[string]master{
	"region": {table: "region", right: "mas.org", label: "region", fields: []field{{name: "name", kind: "text", required: true}}, list: mastersList(),
		impact: mastersPlaceImpact([2]string{"state", "region_id"}, [2]string{"city", "state_id"}, [2]string{"cluster", "city_id"}, [2]string{"location", "cluster_id"})},
	"state": {table: "state", right: "mas.org", label: "state", fields: []field{{name: "name", kind: "text", required: true}, {name: "region_id", kind: "uuid", required: true, lbl: "Region"}},
		list:   mastersList(mastersJoin{"region", "region_id", "_r", bson.D{{Key: "region", Value: "$_r.name"}}}),
		impact: mastersPlaceImpact([2]string{"city", "state_id"}, [2]string{"cluster", "city_id"}, [2]string{"location", "cluster_id"})},
	"city": {table: "city", right: "mas.org", label: "city", fields: []field{{name: "name", kind: "text", required: true}, {name: "state_id", kind: "uuid", required: true, lbl: "State"}, {name: "headcount_budget", kind: "int"}},
		list:   mastersList(mastersJoin{"state", "state_id", "_s", bson.D{{Key: "state", Value: "$_s.name"}}}),
		impact: mastersPlaceImpact([2]string{"cluster", "city_id"}, [2]string{"location", "cluster_id"})},
	"cluster": {table: "cluster", right: "mas.org", label: "cluster", fields: []field{{name: "name", kind: "text", required: true}, {name: "city_id", kind: "uuid", required: true, lbl: "City"}},
		list:   mastersList(mastersJoin{"city", "city_id", "_c", bson.D{{Key: "city", Value: "$_c.name"}}}),
		impact: mastersPlaceImpact([2]string{"location", "cluster_id"})},
	"location": {table: "location", right: "mas.org", label: "location", fields: []field{{name: "name", kind: "text", required: true}, {name: "cluster_id", kind: "uuid", required: true, lbl: "Cluster"}, {name: "headcount_budget", kind: "int"}, {name: "payroll_budget", kind: "num"},
		{name: "property_id", kind: "text", lbl: "CMS property"}}, // master data SOP: set once, from the CMS (mdm.CheckLocation)
		list: mastersList(mastersJoin{"cluster", "cluster_id", "_k", bson.D{{Key: "cluster", Value: "$_k.name"}}},
			mastersJoin{"city", "_k.city_id", "_c", bson.D{{Key: "city", Value: "$_c.name"}, {Key: "city_id", Value: "$_c._id"}}},
			mastersJoin{"state", "_c.state_id", "_s", bson.D{{Key: "state", Value: "$_s.name"}}}),
		impact: mastersPlaceImpact()},
	"department": {table: "department", right: "mas.job", label: "department", fields: []field{{name: "name", kind: "text", required: true}, {name: "corporate", kind: "bool", lbl: "Approval flow"}},
		list: mastersList(), impact: mastersIDImpact("department_id")},
	"designation": {table: "designation", right: "mas.job", label: "designation", fields: []field{{name: "name", kind: "text", required: true}, {name: "department_id", kind: "uuid", required: true, lbl: "Department"}, {name: "grades", kind: "texts", required: true}},
		list:   mastersList(mastersJoin{"department", "department_id", "_d", bson.D{{Key: "department", Value: "$_d.name"}}}),
		impact: mastersIDImpact("designation_id")},
	"grade": {table: "grade", right: "mas.job", label: "grade", fields: []field{{name: "name", kind: "text", required: true}}, list: mastersList(),
		impact: mastersNameImpact("grade", "name", "grade")},
	"shift": {table: "shift", right: "mas.job", label: "shift", fields: []field{{name: "code", kind: "text", required: true}, {name: "name", kind: "text", required: true}, {name: "start_time", kind: "time", required: true, lbl: "Start"},
		{name: "end_time", kind: "time", required: true, lbl: "End"}, {name: "break_minutes", kind: "int"},
		{name: "start2_time", kind: "time", lbl: "Second part start"}, {name: "end2_time", kind: "time", lbl: "Second part end"}},
		list:   mastersShiftList,
		impact: mastersNameImpact("shift", "code", "shift_code")},
	"qualification": {table: "simple_master", kind: "qualification", right: "mas.job", label: "qualification", fields: []field{{name: "name", kind: "text", required: true}}, list: mastersList()},
	"employment_type": {table: "simple_master", kind: "employment_type", right: "mas.job", label: "employment type", fields: []field{{name: "name", kind: "text", required: true}}, list: mastersList(),
		impact: mastersNameImpact("simple_master", "name", "type")},
	"attendance_mode": {table: "simple_master", kind: "attendance_mode", right: "mas.job", label: "attendance mode", fields: []field{{name: "name", kind: "text", required: true}}, list: mastersList(),
		impact: mastersNameImpact("simple_master", "name", "attendance_mode")},
	"change_reason": {table: "simple_master", kind: "change_reason", right: "mas.job", label: "change reason", fields: []field{{name: "name", kind: "text", required: true},
		{name: "used_for", kind: "enum", required: true, enum: reasonUses}},
		list: mastersList()},
}

// reasonUses are what a change reason is used for, in the Masters screen's order.
var reasonUses = []string{"Employee update", "Salary", "Separation", "Master data", "Hold", "Deduction"}

func masterCfg(t string) (master, error) {
	m, ok := masters[t]
	if !ok {
		return m, notFound("Unknown master")
	}
	return m, nil
}

// validate checks a body against the master's fields; create requires the required fields. The Masters
// screen (Add, Edit) and the Master update workbook both go through it, then through checkMaster.
func (m master) validate(body map[string]any, create bool) (map[string]any, error) {
	known := map[string]field{}
	for _, f := range m.fields {
		known[f.name] = f
		if _, ok := body[f.name]; create && f.required && !ok {
			return nil, bad("%s is required", f.label())
		}
	}
	out := map[string]any{}
	for k, v := range body {
		f, ok := known[k]
		if !ok {
			return nil, bad("Unknown field %s", k)
		}
		if s, _ := v.(string); f.kind == "time" && !f.required && (v == nil || strings.TrimSpace(s) == "") {
			out[k] = nil // an optional time cleared (a split shift's second part)
			continue
		}
		switch f.kind {
		case "text", "enum", "time":
			s, ok := v.(string)
			s = strings.TrimSpace(s)
			if !ok {
				return nil, bad("%s must be text", f.label())
			}
			if f.required && s == "" {
				return nil, bad("%s is required", f.label())
			}
			if len([]rune(s)) > 120 {
				return nil, bad("%s is too long: 120 characters at most", f.label())
			}
			if f.kind == "time" && !reTime.MatchString(s) {
				return nil, bad("%s must be a time as HH:MM, for example 09:30", f.label())
			}
			if f.kind == "enum" {
				found := false
				for _, e := range f.enum {
					found = found || e == s
				}
				if !found {
					return nil, bad("%s must be one of: %s", f.label(), strings.Join(f.enum, ", "))
				}
			}
			if k == "code" {
				s = strings.ToUpper(s)
			}
			out[k] = s
		case "uuid":
			s, _ := v.(string)
			if !reUUID.MatchString(s) {
				return nil, bad("Pick a %s", strings.ToLower(f.label()))
			}
			out[k] = s
		case "int", "num":
			n, ok := v.(float64)
			if !ok || n < 0 || (f.kind == "int" && n != float64(int64(n))) {
				return nil, bad("%s must be a %snumber, 0 or more", f.label(), map[bool]string{true: "whole ", false: ""}[f.kind == "int"])
			}
			out[k] = n
		case "bool":
			b, ok := v.(bool)
			if !ok {
				return nil, bad("%s must be true or false", f.label())
			}
			out[k] = b
		case "texts", "uuids":
			if v == nil && f.kind == "uuids" {
				out[k] = nil // NULL = all locations
				continue
			}
			l, ok := v.([]any)
			if !ok || (f.required && len(l) == 0) {
				return nil, bad("%s: pick at least one", f.label())
			}
			ss := make([]string, 0, len(l))
			for _, x := range l {
				s, _ := x.(string)
				s = strings.TrimSpace(s)
				if f.kind == "uuids" && !reUUID.MatchString(s) {
					return nil, bad("%s must hold ids", f.label())
				}
				if s == "" {
					return nil, bad("%s has an empty value", f.label())
				}
				ss = append(ss, s)
			}
			out[k] = ss
		}
	}
	return out, nil
}

func (s *Server) masterTypes(r *http.Request, c *Ctx) (any, error) {
	keys := make([]string, 0, len(masters))
	for k := range masters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// Masters are readable by signed-in users (forms need them); only Active items are offered for new selections.
func (s *Server) masterList(r *http.Request, c *Ctx) (any, error) {
	m, err := masterCfg(r.PathValue("type"))
	if err != nil {
		return nil, err
	}
	match := bson.D{}
	if m.kind != "" {
		match = append(match, bson.E{Key: "kind", Value: m.kind})
	}
	if st := r.URL.Query().Get("status"); st == "Active" || st == "Inactive" {
		match = append(match, bson.E{Key: "status", Value: st})
	}
	// Master order (the order items were created, as the prototype lists them); ?order=name sorts by name.
	order := bson.D{{Key: "seq", Value: 1}}
	if r.URL.Query().Get("order") == "name" {
		order = bson.D{{Key: "name", Value: 1}, {Key: "seq", Value: 1}}
	}
	rows, err := m.list(r.Context(), s.st.DB, m.table, match, order)
	// An employment type's name cannot change; Edit says why instead of offering it. An item the app writes itself
	// can be renamed, but not moved to another use.
	for _, x := range rows {
		if why := fixedName(m.kind); why != "" {
			x["fixed"] = why
		}
		if why := appUse(x); why != "" {
			x["appUse"] = why
		}
		delete(x, "sys_key")
		// Payroll and headcount budgets are for the Budgets right (or the masters editors), not every signed-in
		// person who reads the names (PPL-126).
		if c.Level("comp.budget") < 1 && c.Level("mas.org") < 1 {
			delete(x, "payroll_budget")
			delete(x, "headcount_budget")
		}
	}
	if err == nil && m.table == "location" { // a location linked to a CMS property: its name and status come from the CMS
		err = mdm.LocationFlags(r.Context(), s.st.DB, rows)
	}
	return rows, err
}

func (s *Server) masterCreate(r *http.Request, c *Ctx) (any, error) {
	t := r.PathValue("type")
	m, err := masterCfg(t)
	if err != nil {
		return nil, err
	}
	if err := c.Need(m.right, 2); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	vals, err := m.validate(body, true)
	if err != nil {
		return nil, err
	}
	var row store.Row
	err = s.st.Tx(r.Context(), func(tx store.Q) error {
		res, err := saveMaster(r.Context(), tx, c, t, "", vals)
		row = res.After
		return err
	})
	return row, err
}

// masterUpdate is Edit on the Masters screen: the same rules as Add and the workbook, and a rename
// rewrites every current reference to the old name (renameRefs).
func (s *Server) masterUpdate(r *http.Request, c *Ctx) (any, error) {
	t := r.PathValue("type")
	m, err := masterCfg(t)
	if err != nil {
		return nil, err
	}
	if err := c.Need(m.right, 2); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	vals, err := m.validate(body, false)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, bad("Nothing to change")
	}
	var row store.Row
	err = s.st.Tx(r.Context(), func(tx store.Q) error {
		res, err := saveMaster(r.Context(), tx, c, t, r.PathValue("id"), vals)
		if err != nil {
			return err
		}
		row = res.After
		if len(res.Refs) > 0 {
			row["referencesUpdated"] = res.Refs
		}
		if res.Moved > 0 {
			row["activeEmployeesMoved"] = res.Moved
		}
		return nil
	})
	return row, err
}

// masterInactive retires an item from a date with a reason. Nothing is ever deleted; an
// in-use item can be retired and the count of active employees still using it is returned.
func (s *Server) masterInactive(r *http.Request, c *Ctx) (any, error) {
	t := r.PathValue("type")
	m, err := masterCfg(t)
	if err != nil {
		return nil, err
	}
	if err := c.Need(m.right, 2); err != nil {
		return nil, err
	}
	var b struct {
		Since  string `json:"since"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	var out store.Row
	err = s.st.Tx(r.Context(), func(tx store.Q) error {
		var err error
		out, _, err = retireMaster(r.Context(), tx, c, t, r.PathValue("id"), b.Since, b.Reason)
		return err
	})
	return out, err
}

func (s *Server) masterReactivate(r *http.Request, c *Ctx) (any, error) {
	t := r.PathValue("type")
	m, err := masterCfg(t)
	if err != nil {
		return nil, err
	}
	if err := c.Need(m.right, 2); err != nil {
		return nil, err
	}
	var row store.Row
	err = s.st.Tx(r.Context(), func(tx store.Q) error {
		var err error
		row, err = reactivateMaster(r.Context(), tx, c, t, r.PathValue("id"))
		return err
	})
	return row, err
}
