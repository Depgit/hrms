package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// The time set: policies, plans, calendars and shifts, loaded once per request. Everything about
// rosters, attendance and leave is computed from it in memory (spec 10-12).

type polMap struct {
	ID    int64  `json:"id"`
	Level string `json:"level"`
	Ref   string `json:"ref"`
	On    bool   `json:"on"`
	// Employees on the policy through this mapping (GET /api/policies/{id} only).
	Employees *int `json:"employees,omitempty"`
	// An employee-level mapping saved by the policy conflict check, and why (policy_conflict.go).
	Exception bool   `json:"exception,omitempty"`
	Note      string `json:"note,omitempty"`
}

type workPolicy struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Sort   int               `json:"sort"`
	S      policy.Settings   `json:"settings"`
	PlanID *string           `json:"leavePlanId"`
	CalID  *string           `json:"calendarId"`
	CalMap map[string]string `json:"calMap"`
	Maps   []polMap          `json:"maps"`
}

type leavePlan struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Status      string                   `json:"status"`
	LV          leave.LV                 `json:"lv"`
	LRX         leave.LRX                `json:"lrx"`
	Types       map[string]leave.TypeCfg `json:"types"`
	Version     int                      `json:"version"`
}

type leaveType struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Paid    bool   `json:"paid"`
	Builtin bool   `json:"builtin"`
	Status  string `json:"status"`
}

type timeSet struct {
	pols   []*workPolicy // active, in order; the first is the default
	all    []*workPolicy
	plans  map[string]*leavePlan
	cals   map[string]policy.Holidays
	opts   map[string]map[string]bool // optional holidays by calendar
	shifts map[string]attend.Shift    // active shifts as they stand today
	list   []attend.Shift
	vers   map[string][]shiftVer // each active shift's versions, oldest first
	types  map[string]leaveType
	order  []string            // leave type names in catalogue order
	own    map[string][]ownDev // employees' own deviation allowances, oldest version first (dev_own.go)
}

// shiftVer is a shift's timings and attendance rules from a date (Set shift).
type shiftVer struct {
	from  string
	sh    attend.Shift
	brk   int
	rules *attend.Rules // nil = no rules: the work policy decides
}

// parseRules reads a stored rules object; empty rules come back nil.
func parseRules(raw []byte) *attend.Rules {
	if len(raw) == 0 {
		return nil
	}
	r := &attend.Rules{} // the removed rules of an older version are not read (attend.Rules)
	if json.Unmarshal(raw, r) != nil {
		return nil
	}
	if r.Empty() {
		return nil
	}
	return r
}

// shiftOn is an active shift with the version in effect on date d (before its first version, the first).
func (t *timeSet) shiftOn(code, d string) (attend.Shift, *attend.Rules, bool) {
	vs := t.vers[code]
	if len(vs) == 0 {
		sh, ok := t.shifts[code]
		return sh, nil, ok
	}
	v := &vs[0]
	for i := 1; i < len(vs) && vs[i].from <= d; i++ {
		v = &vs[i]
	}
	return v.sh, v.rules, true
}

func loadTimeSet(ctx context.Context, q store.Q) (*timeSet, error) {
	t := &timeSet{plans: map[string]*leavePlan{}, cals: map[string]policy.Holidays{}, opts: map[string]map[string]bool{}, shifts: map[string]attend.Shift{},
		vers: map[string][]shiftVer{}, types: map[string]leaveType{}}
	pols, err := q.C("work_policy").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	byID := map[string]*workPolicy{}
	for _, r := range pols {
		p := &workPolicy{ID: str(r["id"]), Name: str(r["name"]), Status: str(r["status"]), Sort: timectxInt(r["sort"]), Maps: []polMap{},
			PlanID: timectxStrPtr(r["leave_plan_id"]), CalID: timectxStrPtr(r["calendar_id"])}
		if p.S, err = policy.Parse(timectxJSON(r["settings"])); err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Name, err)
		}
		if err := timectxDecode(r["cal_map"], &p.CalMap); err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Name, err)
		}
		if p.CalMap == nil {
			p.CalMap = map[string]string{}
		}
		t.all = append(t.all, p)
		byID[p.ID] = p
		if p.Status == "Active" {
			t.pols = append(t.pols, p)
		}
	}
	maps, err := q.C("work_policy_map").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range maps {
		m := polMap{ID: timectxInt64(r["id"]), Level: str(r["level"]), Ref: str(r["ref"]), On: r["on_map"] == true, Exception: r["exception"] == true, Note: str(r["note"])}
		if p := byID[str(r["policy_id"])]; p != nil {
			p.Maps = append(p.Maps, m)
		}
	}
	plans, err := q.C("leave_plan").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range plans {
		p := &leavePlan{ID: str(r["id"]), Name: str(r["name"]), Description: str(r["description"]), Status: str(r["status"]), Version: timectxInt(r["version"])}
		for _, x := range []struct {
			v   any
			out any
		}{{r["lv"], &p.LV}, {r["lrx"], &p.LRX}, {r["types"], &p.Types}} {
			if err := timectxDecode(x.v, x.out); err != nil {
				return nil, fmt.Errorf("leave plan %s: %w", p.Name, err)
			}
		}
		leave.Normalize(&p.LV, p.Types) // a plan saved with the per-type "By days worked" of 24 Nov reads as a rule
		t.plans[p.ID] = p
	}
	// Holidays on the list, of calendars that exist, in the order they were added.
	cals, err := q.C("holiday_calendar").Distinct(ctx, "_id", nil)
	if err != nil {
		return nil, err
	}
	hols, err := q.C("holiday").Find(ctx, bson.D{{Key: "on_list", Value: true}, {Key: "calendar_id", Value: bson.D{{Key: "$in", Value: cals}}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range hols {
		cal, d, n := str(r["calendar_id"]), str(r["date"]), str(r["name"])
		if r["kind"] == "Optional" { // optional holidays are taken as leave; kept for the shift rules' shortfall inclusions
			if t.opts[cal] == nil {
				t.opts[cal] = map[string]bool{}
			}
			t.opts[cal][d] = true
			continue
		}
		if t.cals[cal] == nil {
			t.cals[cal] = policy.Holidays{}
		}
		t.cals[cal][d] = n
	}
	shifts, err := q.C("shift").Find(ctx, bson.D{{Key: "status", Value: "Active"}}, options.Find().SetSort(bson.D{{Key: "code", Value: 1}}))
	if err != nil {
		return nil, err
	}
	rowVer := map[string]shiftVer{}
	codeOf := map[string]string{} // active shift id → code
	shiftIDs := []string{}
	applies, err := shiftMapsOn(ctx, q, shifts) // where each shift applies (shift_maps.go)
	if err != nil {
		return nil, err
	}
	for _, r := range shifts {
		code := str(r["code"])
		sh := shiftOf(code, str(r["start_time"]), str(r["end_time"]), str(r["start2_time"]), str(r["end2_time"]), applies[str(r["id"])])
		t.shifts[code] = sh
		t.list = append(t.list, sh)
		rowVer[code] = shiftVer{from: "2000-01-01", sh: sh, brk: timectxInt(r["break_minutes"]), rules: parseRules(timectxJSON(r["rules"]))}
		codeOf[str(r["id"])] = code
		shiftIDs = append(shiftIDs, str(r["id"]))
	}
	// The active shifts' versions, oldest first.
	vers, err := q.C("shift_version").Find(ctx, bson.D{{Key: "shift_id", Value: bson.D{{Key: "$in", Value: shiftIDs}}}},
		options.Find().SetSort(bson.D{{Key: "effective_from", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range vers {
		code := codeOf[str(r["shift_id"])]
		sh := shiftOf(code, str(r["start_time"]), str(r["end_time"]), str(r["start2_time"]), str(r["end2_time"]), t.shifts[code].Maps)
		t.vers[code] = append(t.vers[code], shiftVer{from: str(r["effective_from"]), sh: sh, brk: timectxInt(r["break_minutes"]), rules: parseRules(timectxJSON(r["rules"]))})
	}
	for code, v := range rowVer { // a shift written without versions (older seed, direct writes) stands as it is
		if len(t.vers[code]) == 0 {
			t.vers[code] = []shiftVer{v}
		}
	}
	types, err := q.C("leave_type").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range types {
		lt := leaveType{Name: str(r["name"]), Kind: str(r["kind"]), Paid: r["paid"] == true, Builtin: r["builtin"] == true, Status: str(r["status"])}
		t.types[lt.Name] = lt
		t.order = append(t.order, lt.Name)
	}
	if t.own, err = loadOwnDev(ctx, q); err != nil {
		return nil, err
	}
	return t, nil
}

// emp is what the time rules need about an employee.
type emp struct {
	ID, Name, Type, Grade, Status, DOJ, LocID, ClusterID, CityID, StateID string
	Dept, DeptID, Desig, Shift, Gender, Location                          string
	LWD, Exit, ManagerID                                                  *string
	Corporate                                                             bool
}

// loadEmps lists the employees matching filter (a filter on employee documents; nil for everyone), ordered by
// employee ID, with their department, designation, location, cluster, city and state (employees missing one of
// those are left out) and their gender from the personal details (blank when there are none).
// keep, when given, narrows the list further on the joined values (for example the city or the department name).
func loadEmps(ctx context.Context, q store.Q, filter any, keep ...func(*emp) bool) ([]*emp, error) {
	rows, err := q.C("employee").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.D{
		{Key: "name", Value: 1}, {Key: "type", Value: 1}, {Key: "grade", Value: 1}, {Key: "status", Value: 1}, {Key: "doj", Value: 1},
		{Key: "location_id", Value: 1}, {Key: "department_id", Value: 1}, {Key: "designation_id", Value: 1}, {Key: "shift_code", Value: 1},
		{Key: "lwd", Value: 1}, {Key: "exit_status", Value: 1}, {Key: "manager_id", Value: 1}}))
	if err != nil || len(rows) == 0 {
		return []*emp{}, err
	}
	col := func(k string) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, str(r[k]))
		}
		return out
	}
	depts, err := timectxByID(ctx, q, "department", col("department_id"), "name", "corporate")
	if err != nil {
		return nil, err
	}
	desigs, err := timectxByID(ctx, q, "designation", col("designation_id"), "name")
	if err != nil {
		return nil, err
	}
	locs, err := timectxByID(ctx, q, "location", col("location_id"), "name", "cluster_id")
	if err != nil {
		return nil, err
	}
	clusterIDs := []string{}
	for _, l := range locs {
		clusterIDs = append(clusterIDs, str(l["cluster_id"]))
	}
	clusters, err := timectxByID(ctx, q, "cluster", clusterIDs, "city_id")
	if err != nil {
		return nil, err
	}
	cityIDs := []string{}
	for _, k := range clusters {
		cityIDs = append(cityIDs, str(k["city_id"]))
	}
	cities, err := timectxByID(ctx, q, "city", cityIDs, "state_id")
	if err != nil {
		return nil, err
	}
	gender := map[string]string{}
	pers, err := q.C("employee_personal").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: col("id")}}}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "gender", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, p := range pers {
		gender[str(p["employee_id"])] = str(p["gender"])
	}
	out := make([]*emp, 0, len(rows))
next:
	for _, r := range rows {
		d, g, l := depts[str(r["department_id"])], desigs[str(r["designation_id"])], locs[str(r["location_id"])]
		if d == nil || g == nil || l == nil {
			continue
		}
		k := clusters[str(l["cluster_id"])]
		if k == nil {
			continue
		}
		c := cities[str(k["city_id"])]
		if c == nil {
			continue
		}
		e := &emp{ID: str(r["id"]), Name: str(r["name"]), Type: str(r["type"]), Grade: str(r["grade"]), Status: str(r["status"]), DOJ: str(r["doj"]),
			LocID: str(r["location_id"]), ClusterID: str(l["cluster_id"]), CityID: str(k["city_id"]), StateID: str(c["state_id"]),
			DeptID: str(r["department_id"]), Dept: str(d["name"]), Desig: str(g["name"]), Shift: str(r["shift_code"]), Gender: gender[str(r["id"])], Location: str(l["name"]),
			LWD: timectxStrPtr(r["lwd"]), Exit: timectxStrPtr(r["exit_status"]), ManagerID: timectxStrPtr(r["manager_id"]), Corporate: d["corporate"] == true}
		for _, k := range keep {
			if !k(e) {
				continue next
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func loadEmp(ctx context.Context, q store.Q, id string) (*emp, error) {
	l, err := loadEmps(ctx, q, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, notFound("Employee not found")
	}
	return l[0], nil
}

var levelNames = map[string]string{"employee": "Employee", "location": "Location", "cluster": "Cluster", "department": "Department", "designation": "Designation",
	"grade": "Grade", "type": "Employment type", "city": "City", "state": "State",
	"dept_location": "Department + Location", "dept_cluster": "Department + Cluster", "dept_city": "Department + City", "dept_state": "Department + State"}

// policyLevels is the order in which a person's mappings are tried when they match more than one work policy:
// the individual, then a department together with a place (policy_combo.go) from the narrowest place to the widest,
// then the place alone from the narrowest (location) to the widest (state), then department, designation, grade and
// employment type (owner, 24 Nov 2026).
var policyLevels = []string{"employee", "dept_location", "dept_cluster", "dept_city", "dept_state", "location", "cluster", "city", "state",
	"department", "designation", "grade", "type"}

// levelRef is the employee's value at a mapping level ("" for an unknown level).
func (e *emp) levelRef(level string) string {
	switch level {
	case "employee":
		return e.ID
	case "location":
		return e.LocID
	case "cluster":
		return e.ClusterID
	case "city":
		return e.CityID
	case "state":
		return e.StateID
	case "department":
		return e.Dept
	case "designation":
		return e.Desig
	case "grade":
		return e.Grade
	case "type":
		return e.Type
	}
	return e.comboLevelRef(level)
}

// policyFor resolves the policy (spec 10): the first level in policyLevels with a mapping that matches,
// else the first active policy.
func (t *timeSet) policyFor(e *emp) (*workPolicy, string) {
	for _, lv := range policyLevels {
		ref := e.levelRef(lv)
		if ref == "" {
			continue
		}
		for _, p := range t.pols {
			for _, m := range p.Maps {
				if m.On && m.Level == lv && m.Ref == ref {
					return p, levelNames[lv]
				}
			}
		}
	}
	if len(t.pols) == 0 {
		return &workPolicy{Name: "Default", S: policy.Defaults(), CalMap: map[string]string{}}, "Default"
	}
	return t.pols[0], "Default"
}

// holidays of the policy's calendar for the employee's city (a city calendar overrides the default).
func (t *timeSet) holidays(p *workPolicy, cityID string) policy.Holidays {
	if id, ok := p.CalMap[cityID]; ok {
		return t.cals[id]
	}
	if p.CalID != nil {
		return t.cals[*p.CalID]
	}
	return nil
}

// optHolidays are the optional holidays of the policy's calendar for the employee's city.
func (t *timeSet) optHolidays(p *workPolicy, cityID string) map[string]bool {
	if id, ok := p.CalMap[cityID]; ok {
		return t.opts[id]
	}
	if p.CalID != nil {
		return t.opts[*p.CalID]
	}
	return nil
}

func (t *timeSet) plan(p *workPolicy) *leavePlan {
	if p.PlanID == nil {
		return nil
	}
	return t.plans[*p.PlanID]
}

// typeCfg is the leave type's settings in the employee's plan (nil when the plan has no such type).
func (t *timeSet) typeCfg(p *workPolicy, name string) *leave.TypeCfg {
	pl := t.plan(p)
	if pl == nil {
		return nil
	}
	if c, ok := pl.Types[name]; ok {
		return &c
	}
	return nil
}

// baseShift is the employee's shift when it is an active shift, else G. Whether the shift's mapping still allows them
// does not matter here (owner, 24 Nov 2026): their past and planned days keep their shift; only a new assignment is
// refused.
func (t *timeSet) baseShift(e *emp) string {
	if _, ok := t.shifts[e.Shift]; ok {
		return e.Shift
	}
	return "G"
}

// ---------- Month data: roster changes, punches, leave and decisions for a set of employees ----------

type rosterCell struct {
	Code, Why, From, By, At string
	Late, Dev               bool
	Other                   *attend.Shift // "Other time for this day" (code OT): the day's own timings (roster_day.go)
}

type leaveSpan struct {
	ID, Type, From, To, Status string
	Days                       float64
	Half                       bool   // a half-day leave (one date)
	HalfOf                     string // which half is leave: first or second
	Cov                        map[string]bool
}

type decision struct {
	Status, By, At, Note, Planned, Actual string
	Kinds                                 []string
}

type monthData struct {
	ts     *timeSet
	roster map[string]map[string]rosterCell
	raw    map[string]map[string]*attend.Raw
	leaves map[string][]leaveSpan // approved and pending
	decide map[string]map[string]decision
	regs   map[string]map[string]string // pending regularization request id by date
	shorts map[string]map[string][]shortSpan
	today  string
	calls  map[string]map[string]*callDoc       // called to work, by employee and date (work_call.go)
	dev    map[string]map[string]*attend.DevDay // deviation allowances, by employee|month and date (dev_allow.go)
}

// shortSpan is a short leave on a date (short_leave.go): its window in minutes and its status (Approved or Pending).
type shortSpan struct {
	ID, Status string
	attend.Window
}

// loadMonth loads roster, punches (from the day before from), leave and decisions for the employees.
func loadMonth(ctx context.Context, q store.Q, ts *timeSet, ids []string, from, to string) (*monthData, error) {
	m := &monthData{ts: ts, roster: map[string]map[string]rosterCell{}, raw: map[string]map[string]*attend.Raw{},
		leaves: map[string][]leaveSpan{}, decide: map[string]map[string]decision{}, regs: map[string]map[string]string{}, shorts: map[string]map[string][]shortSpan{},
		today: today()}
	if ids == nil {
		ids = []string{}
	}
	pre := policy.AddDays(from, -1)
	if ts.anyDev() { // the deviation allowances count from the 1st of the month (dev_allow.go)
		pre = min(pre, policy.AddDays(from[:7]+"-01", -1))
	}
	who := bson.E{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}
	rows, err := q.C("roster_entry").Find(ctx, bson.D{who, {Key: "date", Value: timectxBetween(pre, policy.AddDays(to, 1))}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if m.roster[id] == nil {
			m.roster[id] = map[string]rosterCell{}
		}
		m.roster[id][str(r["date"])] = rosterCell{Code: str(r["code"]), Why: str(r["why"]), From: str(r["from_code"]), By: str(r["by_name"]),
			At: timectxStamp(r["at"], "02 Jan 2006 15:04"), Late: r["late"] == true, Dev: r["by_dev"] == true, Other: otherOfRow(r)}
	}
	rows, err = q.C("attendance_day").Find(ctx, bson.D{who, {Key: "date", Value: timectxBetween(pre, to)}},
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "employee_id", Value: 1}, {Key: "date", Value: 1}, {Key: "in_time", Value: 1},
			{Key: "out_time", Value: 1}, {Key: "out_date", Value: 1}, {Key: "source", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if m.raw[id] == nil {
			m.raw[id] = map[string]*attend.Raw{}
		}
		m.raw[id][str(r["date"])] = &attend.Raw{In: str(r["in_time"]), Out: str(r["out_time"]), OutDate: str(r["out_date"]), Src: str(r["source"])}
	}
	// Leave approved or waiting that touches the range; "cov" lists the dates a request counts (none: every date).
	// A short leave (payload.short) is a window of one day, not a day of leave: loaded apart below.
	rows, err = q.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "leave"}, {Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{"Approved", "Pending"}}}},
		who, {Key: "payload.to", Value: bson.D{{Key: "$gte", Value: pre}}}, {Key: "payload.from", Value: bson.D{{Key: "$lte", Value: to}}},
		{Key: "payload.short", Value: bson.D{{Key: "$ne", Value: true}}}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "status", Value: 1}, {Key: "payload.ltype", Value: 1}, {Key: "payload.from", Value: 1},
			{Key: "payload.to", Value: 1}, {Key: "payload.cov", Value: 1}, {Key: "payload.half", Value: 1}, {Key: "payload.halfOf", Value: 1}, {Key: "facts.days", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		pl, _ := r["payload"].(map[string]any)
		fa, _ := r["facts"].(map[string]any)
		l := leaveSpan{ID: str(r["id"]), Type: str(pl["ltype"]), From: str(pl["from"]), To: str(pl["to"]), Status: str(r["status"]), Days: timectxNum(fa["days"]),
			Half: pl["half"] == true, HalfOf: str(pl["halfOf"])}
		if cov := timectxStrs(pl["cov"]); len(cov) > 0 {
			l.Cov = map[string]bool{}
			for _, d := range cov {
				l.Cov[d] = true
			}
		}
		id := str(r["employee_id"])
		m.leaves[id] = append(m.leaves[id], l)
	}
	if err := m.loadShorts(ctx, q, who, pre, to); err != nil {
		return nil, err
	}
	if err := m.loadCalls(ctx, q, who, pre, to); err != nil {
		return nil, err
	}
	rows, err = q.C("deviation_decision").Find(ctx, bson.D{who, {Key: "date", Value: timectxBetween(from, to)}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if m.decide[id] == nil {
			m.decide[id] = map[string]decision{}
		}
		kinds := timectxStrs(r["kinds"])
		if kinds == nil {
			kinds = []string{}
		}
		m.decide[id][str(r["date"])] = decision{Status: str(r["status"]), By: str(r["by_name"]), At: timectxStamp(r["at"], "02 Jan 2006"), Note: str(r["note"]),
			Planned: str(r["planned"]), Actual: str(r["actual"]), Kinds: kinds}
	}
	rows, err = q.C("approval_request").Find(ctx, bson.D{{Key: "wf", Value: "regularize"}, {Key: "status", Value: "Pending"}, who,
		{Key: "payload.date", Value: timectxBetween(from, to)}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "payload.date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		pl, _ := r["payload"].(map[string]any)
		if m.regs[id] == nil {
			m.regs[id] = map[string]string{}
		}
		m.regs[id][str(pl["date"])] = str(r["id"])
	}
	return m, nil
}

func (l leaveSpan) covers(d string) bool {
	if l.Cov != nil {
		return l.Cov[d]
	}
	return d >= l.From && d <= l.To
}

// leaveOn is the approved (or, with pending, also pending) leave on a date.
func (m *monthData) leaveOn(e *emp, d string, pending bool) *leaveSpan {
	for i, l := range m.leaves[e.ID] {
		if (l.Status == "Approved" || (pending && l.Status == "Pending")) && l.covers(d) {
			return &m.leaves[e.ID][i]
		}
	}
	return nil
}

func (m *monthData) unpaid(t string) bool {
	lt, ok := m.ts.types[t]
	return ok && !lt.Paid
}

// rosterCode is the roster cell before leave: a stored change, else the policy default.
func (m *monthData) rosterCode(e *emp, p *workPolicy, d string) string {
	if c, ok := m.roster[e.ID][d]; ok {
		return c.Code
	}
	return p.S.DefCode(e.ID, m.ts.baseShift(e), d, m.ts.holidays(p, e.CityID))
}

// planned is the day's plan: approved paid leave shows as L (plannedFor). A shift comes with the version
// (timings and attendance rules) in effect on the date.
func (m *monthData) planned(e *emp, p *workPolicy, d string) attend.Plan {
	if l := m.leaveOn(e, d, false); l != nil && !m.unpaid(l.Type) {
		return attend.Plan{Code: "L"}
	}
	return m.shiftPlan(e, p, d)
}

// shiftPlan is the day's roster plan whatever leave there is, with the approved short leave windows.
func (m *monthData) shiftPlan(e *emp, p *workPolicy, d string) attend.Plan {
	c := m.rosterCode(e, p, d)
	if x, ok := m.roster[e.ID][d]; ok && x.Other != nil && c == otherCode { // other time for this day: a shift of its own (roster_day.go)
		sh := *x.Other
		return attend.Plan{Code: c, Shift: &sh, Rules: otherRules(sh), Short: m.shortWindows(e.ID, d)}
	}
	if rosterCodes[c] { // a week off, holiday or leave, even if an old shift took the same code (TIME-042)
		return attend.Plan{Code: c}
	}
	if s, r, ok := m.ts.shiftOn(c, d); ok {
		return attend.Plan{Code: c, Shift: &s, Rules: r, Short: m.shortWindows(e.ID, d)}
	}
	return attend.Plan{Code: c}
}

func (m *monthData) rec(e *emp, p *workPolicy, d string) *attend.Rec {
	return attend.Day(m.raw[e.ID][policy.AddDays(d, -1)], m.raw[e.ID][d], d, p.S.Att.MaxSpan)
}

// deviation is the computed deviation with any decision already taken.
func (m *monthData) deviation(e *emp, p *workPolicy, d string) (*attend.Deviation, *decision) {
	if d > m.today {
		return nil, nil
	}
	al := m.devOn(e, p, d).Allowed() // what the deviation allowances cover is no roster deviation (dev_allow.go)
	plan, _, _ := al.Apply(m.planned(e, p, d), nil, false)
	v := al.Trim(attend.Deviate(p.S, plan, m.rec(e, p, d), d, e.LocID, m.ts.listFor(e)))
	if x, ok := m.decide[e.ID][d]; ok {
		if v == nil {
			v = &attend.Deviation{Planned: x.Planned, Actual: x.Actual, Kinds: x.Kinds}
		}
		return v, &x
	}
	return v, nil
}

// status is the day's attendance status, with the late mark following a Shift deviation.
func (m *monthData) status(e *emp, p *workPolicy, d string) (string, string) {
	if m.calledWorked(e, p, d) { // called to work on a holiday or leave day and worked (work_call.go)
		return "Present", "ok"
	}
	v, _ := m.deviation(e, p, d)
	ls, ov := attend.LateStartFor(v, m.ts.list)
	// The deviation allowance, or the action of a rule beyond it (dev_allow.go).
	return m.devOn(e, p, d).Status(p.S, m.planned(e, p, d), m.rec(e, p, d), ls, ov)
}

// month runs FinalMonth for one employee and calendar month.
func (m *monthData) month(e *emp, p *workPolicy, ym string) attend.Month {
	return m.span(e, p, ym+"-01", policy.LastDayOf(ym))
}

// span runs FinalMonth over an attendance window (a calendar month, or 26th to 25th).
func (m *monthData) span(e *emp, p *workPolicy, from, to string) attend.Month {
	days := []attend.DayIn{}
	opt := m.ts.optHolidays(p, e.CityID)
	for d := from; d <= to; d = policy.AddDays(d, 1) {
		in := attend.DayIn{Date: d, Before: d < e.DOJ, Future: d > m.today, AgeDays: policy.DayDiff(d, m.today), OptHol: opt[d]}
		if e.LWD != nil && d > *e.LWD && (e.Status == "Inactive" || (e.Exit != nil && *e.Exit == "On notice")) {
			in.After = true
		}
		if l := m.leaveOn(e, d, false); l != nil {
			in.Leave, in.Unpaid, in.Half, in.HalfOf = l.Type, m.unpaid(l.Type), l.Half, l.HalfOf
			if l.Half && !in.Before && !in.After {
				in.Rec = m.rec(e, p, d) // the other half: worked when punched, by the shift's half-day rule (RUN-15)
				in.Plan = m.shiftPlan(e, p, d)
			}
		}
		if !in.Before && !in.After && in.Leave == "" && !in.Future {
			in.Plan, in.Rec = m.planned(e, p, d), m.rec(e, p, d)
			v, _ := m.deviation(e, p, d)
			in.LateFrom, in.Override = attend.LateStartFor(v, m.ts.list)
			in.Dev = m.devOn(e, p, d)
		}
		if !in.Before && !in.After && !in.Future && m.callIn(e, p, d) { // called to work (work_call.go)
			in.Called, in.CallPlan, in.Rec = true, m.callPlan(e, p, d), m.rec(e, p, d)
		}
		days = append(days, in)
	}
	return attend.FinalMonth(p.S, days)
}

// ---------- Small helpers ----------

func ids(l []*emp) []string {
	out := make([]string, len(l))
	for i, e := range l {
		out[i] = e.ID
	}
	return out
}

func monthRange(ym string) (string, string, error) {
	if len(ym) != 7 || !isDate(ym+"-01") {
		return "", "", bad("Month must look like 2026-11")
	}
	return ym + "-01", policy.LastDayOf(ym), nil
}

// scopedEmps lists employees in the caller's scope (Scope: locations, team or self, and the department limit)
// that match filter (on employee documents) and every keep.
func scopedEmps(ctx context.Context, q store.Q, c *Ctx, filter any, keep ...func(*emp) bool) ([]*emp, error) {
	f := any(timectxScopeBase(c.Scope))
	if filter != nil {
		f = bson.D{{Key: "$and", Value: bson.A{f, filter}}}
	}
	all := append([]func(*emp) bool{func(e *emp) bool { return timectxAllows(c.Scope, e) }}, keep...)
	return loadEmps(ctx, q, f, all...)
}

// ---------- Database helpers of the time area ----------

// timectxScopeBase narrows employee documents to the caller's access: everyone, the mapped locations, the
// caller's team or the caller (the department limit is checked on the loaded rows by timectxAllows).
func timectxScopeBase(s access.Scope) bson.D {
	switch s.Access {
	case "all":
		return bson.D{}
	case "locs":
		return bson.D{{Key: "location_id", Value: bson.D{{Key: "$in", Value: append([]string{}, s.Locations...)}}}}
	case "team":
		return bson.D{{Key: "manager_id", Value: s.EmployeeID}}
	}
	return bson.D{{Key: "_id", Value: s.EmployeeID}}
}

// timectxAllows is the caller's data scope over a loaded employee (its location, manager, id and department).
func timectxAllows(s access.Scope, e *emp) bool {
	mgr := ""
	if e.ManagerID != nil {
		mgr = *e.ManagerID
	}
	return s.Allows(access.ScopedEmployee{ID: e.ID, LocationID: e.LocID, Department: e.Dept, ManagerID: mgr})
}

// timectxInScope is the set of the given employees inside the caller's data scope (and with a department), for
// lists of records about employees that keep only those in the scope.
func timectxInScope(ctx context.Context, q store.Q, c *Ctx, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.C("employee").Find(ctx, bson.D{{Key: "$and", Value: bson.A{timectxScopeBase(c.Scope), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}}}},
		options.Find().SetProjection(bson.D{{Key: "location_id", Value: 1}, {Key: "manager_id", Value: 1}, {Key: "department_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	deptIDs := []string{}
	for _, r := range rows {
		deptIDs = append(deptIDs, str(r["department_id"]))
	}
	depts, err := timectxByID(ctx, q, "department", deptIDs, "name")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		d := depts[str(r["department_id"])]
		if d == nil {
			continue
		}
		e := &emp{ID: str(r["id"]), LocID: str(r["location_id"]), Dept: str(d["name"]), ManagerID: timectxStrPtr(r["manager_id"])}
		if timectxAllows(c.Scope, e) {
			out[e.ID] = true
		}
	}
	return out, nil
}

// timectxByID loads the documents of a collection with these ids (only the fields named), keyed by id.
func timectxByID(ctx context.Context, q store.Q, coll string, ids []string, fields ...string) (map[string]store.Row, error) {
	seen, uniq := map[string]bool{}, []string{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	out := map[string]store.Row{}
	if len(uniq) == 0 {
		return out, nil
	}
	proj := bson.D{}
	for _, f := range fields {
		proj = append(proj, bson.E{Key: f, Value: 1})
	}
	rows, err := q.C(coll).Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: uniq}}}}, options.Find().SetProjection(proj))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[str(r["id"])] = r
	}
	return out, nil
}

// timectxBetween matches a date stored as YYYY-MM-DD from lo to hi, both included (the strings sort as the dates do).
func timectxBetween(lo, hi string) bson.D {
	return bson.D{{Key: "$gte", Value: lo}, {Key: "$lte", Value: hi}}
}

// timectxMonth matches a date inside the month ym.
func timectxMonth(ym string) bson.D { return timectxBetween(ym+"-01", ym+"-31") }

// timectxNullIf stores a blank string as null.
func timectxNullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// timectxStrPtr reads a nullable text value.
func timectxStrPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// timectxStrs reads a text array (or a JSON array of strings); null stays nil.
func timectxStrs(v any) []string {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// timectxInt reads an integer column (int32 as stored for int4).
func timectxInt(v any) int { return int(timectxInt64(v)) }

func timectxInt64(v any) int64 {
	switch x := v.(type) {
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// timectxNum reads a number kept in a JSON document (a number, or a number written as text; 0 when it is missing).
func timectxNum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// timectxStamp formats a timestamp in the business zone; "" for null.
func timectxStamp(v any, layout string) string {
	if t, ok := v.(time.Time); ok {
		return t.In(store.Loc()).Format(layout)
	}
	return ""
}

// timectxJSON is a stored JSON document as JSON text (what the parsers of policies and rules read).
func timectxJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// timectxDecode reads a stored JSON document into a Go value through its JSON form (json tags apply).
func timectxDecode(v any, out any) error {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func sortStrings(l []string) []string { sort.Strings(l); return l }
