package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/leave"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Work policies, leave plans, holiday calendars and exceptions (spec 10). Changes apply from the
// next request or roster; nothing already approved is rewritten. Every change is kept as history.

func canPolicies(c *Ctx, edit bool) error {
	if edit {
		return c.Need("pol.policies", 2)
	}
	if c.Level("pol.policies") < 1 && c.Level("ros.roster") < 1 && c.Level("pay.run") < 1 {
		return forbidden("")
	}
	return nil
}

// flatten turns nested settings into path -> value, for the change history.
func flatten(prefix string, v any, out map[string]any) {
	if m, ok := v.(map[string]any); ok {
		for k, x := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, x, out)
		}
		return
	}
	out[prefix] = v
}

func diff(before, after any) []map[string]any {
	toMap := func(v any) map[string]any {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out := map[string]any{}
		flatten("", m, out)
		return out
	}
	a, b := toMap(before), toMap(after)
	keys := []string{}
	for k := range b {
		if !reflect.DeepEqual(a[k], b[k]) {
			keys = append(keys, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := []map[string]any{}
	for _, k := range keys {
		out = append(out, map[string]any{"field": k, "from": a[k], "to": b[k]})
	}
	return out
}

func configHistory(ctx context.Context, tx store.Q, c *Ctx, entity, id string, changes []map[string]any) error {
	if len(changes) == 0 {
		return nil
	}
	_, err := tx.C("config_history").Insert(ctx, bson.M{"entity": entity, "entity_id": id, "changes": changes, "by_name": c.Name + " (" + c.Role + ")"})
	return err
}

// policiesText is a nullable text column as a *string (nil for NULL).
func policiesText(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// policiesJSON reads a JSON column into a Go value through its JSON form.
func policiesJSON(v any, into any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// policiesSerial is a numeric id given as text in a URL; false unless the text is the number written plainly, as
// ids are shown.
func policiesSerial(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && strconv.FormatInt(n, 10) == s
}

// policiesScope is the caller's data scope (access.Scope) over an exception joined with its employee (_e) and the
// employee's department (_d).
func policiesScope(c *Ctx) bson.M {
	sc := c.Scope
	var m bson.M
	switch sc.Access {
	case "all":
		m = bson.M{}
	case "locs":
		locs := sc.Locations
		if locs == nil {
			locs = []string{}
		}
		m = bson.M{"_e.location_id": bson.M{"$in": locs}}
	case "team":
		m = bson.M{"_e.manager_id": sc.EmployeeID}
	default:
		m = bson.M{"_e._id": sc.EmployeeID}
	}
	if len(sc.Departments) > 0 {
		m["_d.name"] = bson.M{"$in": sc.Departments}
	}
	return m
}

// ---------- Policies ----------

type policySummary struct {
	*workPolicy
	WeekOff   string `json:"weekOff"`
	PlanName  string `json:"planName"`
	CalName   string `json:"calendarName"`
	Employees int    `json:"employees"`
}

func (s *Server) policyList(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	emps, err := loadEmps(ctx, s.st.DB, bson.M{"status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		count[p.ID]++
	}
	cals := map[string]string{}
	rows, _ := s.st.DB.C("holiday_calendar").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1}))
	for _, x := range rows {
		cals[x["id"].(string)] = x["name"].(string)
	}
	out := []policySummary{}
	for _, p := range ts.all {
		ps := policySummary{workPolicy: p, WeekOff: p.S.WeekOffText(), Employees: count[p.ID]}
		if pl := ts.plan(p); pl != nil {
			ps.PlanName = pl.Name
		}
		if p.CalID != nil {
			ps.CalName = cals[*p.CalID]
		}
		out = append(out, ps)
	}
	return out, nil
}

func (s *Server) policyGet(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	for _, p := range ts.all {
		if p.ID == r.PathValue("id") {
			hist, _ := s.st.DB.C("config_history").Find(ctx, bson.M{"entity": "work_policy", "entity_id": p.ID},
				options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(100).
					SetProjection(bson.M{"_id": 0, "changes": 1, "by_name": 1, "at": 1, "effective_from": 1}))
			grades := s.policyCounts(ctx, ts, p)
			return map[string]any{"policy": p, "history": hist, "defaults": policy.Defaults(), "gradeCounts": grades}, nil
		}
	}
	return nil, notFound("Policy not found")
}

func (s *Server) policyCreate(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Name     string `json:"name"`
		CopyFrom string `json:"copyFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len(b.Name) > 80 {
		return nil, bad("Name the policy")
	}
	ctx := r.Context()
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		wp := tx.C("work_policy")
		// The new policy comes last, with the settings, leave plan, calendar and calendar map of the one it copies.
		doc := bson.M{"name": b.Name, "settings": map[string]any{}, "leave_plan_id": nil, "calendar_id": nil, "cal_map": map[string]any{}}
		src, err := wp.FindOne(ctx, bson.M{"_id": b.CopyFrom})
		if err != nil {
			return err
		}
		if src != nil {
			doc["settings"], doc["leave_plan_id"], doc["calendar_id"], doc["cal_map"] = src["settings"], src["leave_plan_id"], src["calendar_id"], src["cal_map"]
		}
		last, err := wp.FindOne(ctx, nil, options.FindOne().SetSort(bson.D{{Key: "sort", Value: -1}}).SetProjection(bson.M{"sort": 1}))
		if err != nil {
			return err
		}
		sort := int64(1)
		if last != nil {
			n, _ := last["sort"].(int32)
			sort = int64(n) + 1
		}
		doc["sort"] = sort
		newID, err := wp.Insert(ctx, doc)
		if err != nil {
			return err
		}
		id = str(newID)
		return audit(ctx, tx, c, "Work policy “"+b.Name+"” created", "work_policy", id, nil, b)
	})
	return map[string]string{"id": id}, err
}

func (s *Server) policySave(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Name      string            `json:"name"`
		Settings  policy.Settings   `json:"-"`
		Raw       json.RawMessage   `json:"settings"`
		LeavePlan *string           `json:"leavePlanId"`
		Calendar  *string           `json:"calendarId"`
		CalMap    map[string]string `json:"calMap"`
		Active    *bool             `json:"active"`
		MakeFirst bool              `json:"makeDefault"`
		Effective string            `json:"effectiveFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	// A client that does not send the reimbursement switch keeps its default (Yes), as a stored policy does.
	// Settings that were removed (overtime, comp-off, break, …) are ignored, as they are in stored policies,
	// so a page opened before the change still saves.
	b.Settings.Claims.On = true
	if len(b.Raw) > 0 {
		if err := json.Unmarshal(b.Raw, &b.Settings); err != nil {
			return nil, bad("Check the settings: %s", strings.TrimPrefix(err.Error(), "json: "))
		}
	}
	if err := checkSettings(b.Settings); err != nil {
		return nil, err
	}
	if err := checkEffective(b.Effective); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		wp := tx.C("work_policy")
		row, err := wp.Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		var calMap map[string]string
		if row == nil || policiesJSON(row["cal_map"], &calMap) != nil {
			return notFound("Policy not found")
		}
		name, plan, cal := str(row["name"]), policiesText(row["leave_plan_id"]), policiesText(row["calendar_id"])
		raw, _ := json.Marshal(row["settings"])
		old, _ := policy.Parse(raw)
		if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
			b.Name = name
		}
		if b.CalMap == nil {
			b.CalMap = map[string]string{}
		}
		if _, err := wp.Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"name": b.Name, "settings": b.Settings, "leave_plan_id": b.LeavePlan,
			"calendar_id": b.Calendar, "cal_map": b.CalMap}}); err != nil {
			return err
		}
		if b.Active != nil {
			st := map[bool]string{true: "Active", false: "Inactive"}[*b.Active]
			if !*b.Active {
				others, _ := wp.Count(ctx, bson.M{"status": "Active", "_id": bson.M{"$ne": id}})
				if others == 0 {
					return bad("Keep at least one active policy")
				}
			}
			if _, err := wp.Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": st}}); err != nil {
				return err
			}
		}
		if b.MakeFirst { // this policy first, every other one a place down
			if _, err := wp.UpdateMany(ctx, bson.M{"_id": bson.M{"$ne": id}}, bson.M{"$inc": bson.M{"sort": 1}}); err != nil {
				return err
			}
			if _, err := wp.Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"sort": 0}}); err != nil {
				return err
			}
		}
		ch := diff(map[string]any{"name": name, "settings": old, "leavePlanId": plan, "calendarId": cal, "calMap": calMap},
			map[string]any{"name": b.Name, "settings": b.Settings, "leavePlanId": b.LeavePlan, "calendarId": b.Calendar, "calMap": b.CalMap})
		if b.Active != nil {
			ch = append(ch, map[string]any{"field": "status", "to": map[bool]string{true: "Active", false: "Inactive"}[*b.Active]})
		}
		if err := configHistoryAt(ctx, tx, c, "work_policy", id, ch, b.Effective); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Work policy “%s” changed (%d setting%s)", b.Name, len(ch), map[bool]string{true: "", false: "s"}[len(ch) == 1]), "work_policy", id, nil, ch)
	})
}

// checkSettings keeps numbers in sensible ranges; everything else is a choice from a list.
func checkSettings(p policy.Settings) error {
	type rng struct {
		label    string
		v        float64
		min, max float64
	}
	for _, x := range []rng{{"Full-day hours", p.FullH, 1, 24}, {"Half-day hours", p.HalfH, 0, p.FullH}, {"Late-mark grace", p.Grace, 0, 240},
		{"Probation", float64(p.Prob), 0, 24}, {"Notice in probation", float64(p.Notice.Prob), 0, 365}, {"Notice after confirmation", float64(p.Notice.Conf), 0, 365},
		{"Regularization window", float64(p.RR.Back), 0, 90}, {"Off cap", float64(p.OffCap.Max), 0, 31}, {"Late marks allowed", float64(p.Att.LateN), 0, 31},
		{"Minimum rest", p.Att.MinRest, 0, 24}, {"Longest punch span", p.Att.MaxSpan, 0, 48}, {"Roster lead days", float64(p.RS.Lead), 0, 60},
		{"Deviation tolerance", float64(p.RS.DevTol), 0, 720}, {"Points per Not OK", float64(p.RS.Pts), 0, 100},
		{"Leave not allowed below the discipline score", float64(p.RS.LeaveBelow), 0, 100}} {
		if x.v < x.min || x.v > x.max {
			return bad("%s must be between %v and %v", x.label, x.min, x.max)
		}
	}
	for g, row := range p.Notice.Grade {
		for k, v := range row {
			switch {
			case k == "probM" && (v < 0 || v > 24):
				return bad("Probation for %s must be between 0 and 24 months", g)
			case (k == "prob" || k == "conf") && (v < 0 || v > 365):
				return bad("Notice for %s must be between 0 and 365 days", g)
			}
		}
	}
	// Every choice is one of its list (TIME-065 to TIME-071); blank keeps the default a stored policy falls back to.
	for _, ch := range []struct {
		label, v string
		ok       []string
	}{{"Sundays", p.Sun, []string{"all", "none"}}, {"Saturdays", p.Sat, []string{"none", "2,4", "1,3", "all"}}, {"Weekly off decided by", p.OffMode, []string{"fixed", "rotational"}},
		{"Counting for approval levels", p.RegCount, []string{"separate", "combined"}}, {"Holiday on a weekly off", p.Hol.OnOff, []string{"once", "extra"}},
		{"Allowed ways to punch", p.Att.Mode, []string{"any", "bio", "app", "both"}}, {"After the late-mark limit", p.Att.LateAct, []string{"half", "warn", "none"}},
		{"No punch at all counts as", p.Att.Absent, []string{"lop", "lopnow", "present"}}, {"A missing punch counts as", p.Att.Missing, []string{"half", "absent", "present"}},
		{"Night shift counts on", p.Att.Night, []string{"start", "end"}}, {"If out punch is missing", p.Att.AutoOut, []string{"none", "end"}},
		{"When a roster breaks the rest gap", p.Att.RestAct, []string{"block", "warn"}}, {"Dates that have already passed", p.RS.Past, []string{"lock", "free"}},
		{"Time worked before the planned start", p.RS.EarlyCount, []string{"no", "yes"}}, {"Notice buy-out", p.Ex.Buyout, []string{"no", "approve", "yes"}},
		{"Leave to serve notice", p.Ex.Leave, []string{"no", "hr", "yes"}}, {"Notice waiver approved by", p.Ex.Waive, []string{"HR Admin", "CHRO", "Function Head"}}} {
		if ch.v != "" && !containsStr(ch.ok, ch.v) {
			return bad("%s: “%s” is not one of the choices (%s)", ch.label, ch.v, strings.Join(ch.ok, ", "))
		}
	}
	for _, x := range []rng{{"Work-from-home days a month", float64(p.Att.WFHDays), 0, 31}, {"Early-out grace", float64(p.Att.Early), 0, 240},
		{"Geo-fence radius", float64(p.Att.Geo), 0, 100000}, {"Optional holidays", float64(p.OptHol), 0, 31}} {
		if x.v < x.min || x.v > x.max {
			return bad("%s must be between %v and %v", x.label, x.min, x.max)
		}
	}
	if p.Att.MaxSpan > 0 && p.Att.MaxSpan < 8 {
		return bad("Keep the longest punch span at 8 hours or more, or 0 to switch it off")
	}
	for _, b := range p.Blackouts {
		if !isDate(b.From) || !isDate(b.To) || b.To < b.From || strings.TrimSpace(b.Name) == "" {
			return bad("Every blackout period needs a name and a from and to date")
		}
	}
	return nil
}

// policyRefExists: a mapping's value names a real record: an employee, place or grade by its id or name as the
// level stores it, or one of the employment types.
func policyRefExists(ctx context.Context, q store.Q, level, ref string) bool {
	var f bson.M
	coll := level
	switch level {
	case "employee", "location", "cluster", "city", "state":
		f = bson.M{"_id": ref}
	case "department", "designation", "grade":
		f = bson.M{"name": ref}
	case "type":
		return containsStr(empTypes, ref)
	default:
		return false
	}
	ok, err := q.C(coll).Exists(ctx, f)
	return err != nil || ok // a failed read does not block the save
}

func (s *Server) policyMaps(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Maps       []polMap          `json:"maps"`
		Resolution *policyResolution `json:"resolution"` // HR's choice when the save meets conflicts (policy_conflict.go)
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	for _, m := range b.Maps {
		if levelNames[m.Level] == "" || strings.TrimSpace(m.Ref) == "" {
			return nil, bad("Every mapping needs a level and a value")
		}
		if !policyRefExists(ctx, s.st.DB, m.Level, m.Ref) { // TIME-084
			return nil, bad("%s “%s” does not exist", levelNames[m.Level], m.Ref)
		}
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		// People the new mappings would cover who already get another policy: refused until HR chooses.
		ts, err := loadTimeSet(ctx, tx)
		if err != nil {
			return err
		}
		cf, err := policyConflicts(ctx, tx, ts, id, b.Maps)
		if err != nil {
			return err
		}
		if len(cf.Conflicts) > 0 && b.Resolution == nil {
			return conflictRefusal(cf)
		}
		// Mappings are switched off, never deleted, so the history of who had which policy stays.
		wm := tx.C("work_policy_map")
		cur, err := wm.Find(ctx, bson.M{"policy_id": id}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.M{"level": 1, "ref": 1, "on_map": 1, "exception": 1}))
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, m := range b.Maps {
			want[m.Level+"|"+m.Ref] = m.On
		}
		have := map[string]bool{}
		changes := []map[string]any{}
		for _, x := range cur {
			k := x["level"].(string) + "|" + x["ref"].(string)
			have[k] = true
			on, keep := want[k]
			if !keep && x["exception"] == true { // an exception the conflict check saved stays until switched off by name
				continue
			}
			on = on && keep
			if on != x["on_map"].(bool) {
				if _, err := wm.Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"on_map": on}}); err != nil {
					return err
				}
				changes = append(changes, map[string]any{"field": "applies to " + k, "to": map[bool]string{true: "on", false: "off"}[on]})
				if on {
					if err := moveMapOff(ctx, tx, c, id, x["level"].(string), x["ref"].(string)); err != nil {
						return err
					}
				}
			}
		}
		for _, m := range b.Maps {
			k := m.Level + "|" + m.Ref
			if !have[k] && m.On {
				if _, err := wm.Insert(ctx, bson.M{"policy_id": id, "level": m.Level, "ref": m.Ref, "created_by": c.Name}); err != nil {
					return err
				}
				changes = append(changes, map[string]any{"field": "applies to " + k, "to": "on"})
				// A mapping (a person, location, department, …) belongs to one policy at a time.
				if err := moveMapOff(ctx, tx, c, id, m.Level, m.Ref); err != nil {
					return err
				}
			}
		}
		if len(cf.Conflicts) > 0 { // HR's choice, as employee-level exceptions
			byPol, err := applyResolution(ctx, tx, c, id, cf, b.Resolution)
			if err != nil {
				return err
			}
			for pid, ch := range byPol {
				if pid == id {
					changes = append(changes, ch...)
					continue
				}
				if err := configHistory(ctx, tx, c, "work_policy", pid, ch); err != nil {
					return err
				}
			}
			if err := audit(ctx, tx, c, "Policy conflict resolved: "+b.Resolution.Mode, "work_policy", id, cf.Conflicts, b.Resolution); err != nil {
				return err
			}
		}
		if err := configHistory(ctx, tx, c, "work_policy", id, changes); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Work policy mapping changed", "work_policy", id, nil, changes)
	})
}

// policyResolve says which policy applies to one employee and why (the coverage check).
func (s *Server) policyCoverage(r *http.Request, c *Ctx) (any, error) {
	return s.policyCoverageAll(r, c)
}

// ---------- Leave plans and types ----------

func (s *Server) leavePlans(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	ts, err := loadTimeSet(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	used := map[string][]string{}
	for _, p := range ts.all {
		if p.PlanID != nil {
			used[*p.PlanID] = append(used[*p.PlanID], p.Name)
		}
	}
	plans := []map[string]any{}
	for _, p := range ts.plans {
		plans = append(plans, map[string]any{"plan": p, "policies": used[p.ID]})
	}
	// Master order: the order the plans were created (name order breaks a tie).
	ord := leavePlanOrder(r.Context(), s.st.DB)
	sort.Slice(plans, func(i, j int) bool {
		a, b := plans[i]["plan"].(*leavePlan), plans[j]["plan"].(*leavePlan)
		if ord[a.ID] != ord[b.ID] {
			return ord[a.ID] < ord[b.ID]
		}
		return a.Name < b.Name
	})
	types := []leaveType{}
	for _, n := range ts.order {
		types = append(types, ts.types[n])
	}
	return map[string]any{"plans": plans, "types": types}, nil
}

func (s *Server) leavePlanCreate(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Name     string `json:"name"`
		CopyFrom string `json:"copyFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
		return nil, bad("Name the leave plan")
	}
	ctx := r.Context()
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		lp := tx.C("leave_plan")
		src, err := lp.FindOne(ctx, bson.M{"_id": b.CopyFrom}, options.FindOne().SetProjection(bson.M{"lv": 1, "lrx": 1, "types": 1}))
		if err != nil {
			return err
		}
		var doc bson.M
		if src != nil { // a copy of another plan, with an empty description
			doc = bson.M{"name": b.Name, "description": "", "lv": src["lv"], "lrx": src["lrx"], "types": src["types"]}
		} else { // the standard settings, every active leave type switched off
			types := map[string]leave.TypeCfg{}
			rows, _ := tx.C("leave_type").Find(ctx, bson.M{"status": "Active"}, options.Find().SetProjection(bson.M{"name": 1, "kind": 1}))
			for _, x := range rows {
				t := leave.Default(x["kind"].(string))
				t.On = false
				types[x["name"].(string)] = t
			}
			doc = bson.M{"name": b.Name, "lv": leave.LV{Year: "cal", Prorate: true, Half: true}, "lrx": leave.LRX{StaffOn: true, StaffPct: 20, Club: "ok"}, "types": types}
		}
		newID, err := lp.Insert(ctx, doc)
		if err != nil {
			return err
		}
		id = str(newID)
		return audit(ctx, tx, c, "Leave plan “"+b.Name+"” created", "leave_plan", id, nil, b)
	})
	return map[string]string{"id": id}, err
}

func (s *Server) leavePlanSave(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Name        string                   `json:"name"`
		Description string                   `json:"description"`
		LV          leave.LV                 `json:"lv"`
		LRX         leave.LRX                `json:"lrx"`
		Types       map[string]leave.TypeCfg `json:"types"`
		Active      *bool                    `json:"active"`
		Effective   string                   `json:"effectiveFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := checkEffective(b.Effective); err != nil {
		return nil, err
	}
	if !containsStr([]string{"cal", "fin", "anniv"}, b.LV.Year) {
		return nil, bad("Choose the leave year")
	}
	for n, t := range b.Types {
		if t.Days < 0 || t.Days > 366 || t.Adv < 0 || t.Back < 0 || t.MaxReq < 0 {
			return nil, bad("%s: days and windows cannot be negative", n)
		}
		if !containsStr([]string{"exclude", "sandwich", "include"}, t.Offs) || (t.Grp != "C" && t.Grp != "P") {
			return nil, bad("%s: choose how offs count and the approval group", n)
		}
		if t.On && len(t.Types) == 0 {
			return nil, bad("%s: choose at least one employment type", n)
		}
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		lp := tx.C("leave_plan")
		row, err := lp.Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		var old leavePlan
		if row == nil || policiesJSON(row["lv"], &old.LV) != nil || policiesJSON(row["lrx"], &old.LRX) != nil || policiesJSON(row["types"], &old.Types) != nil {
			return notFound("Leave plan not found")
		}
		old.Name, old.Description = str(row["name"]), str(row["description"])
		for n := range b.Types {
			if ok, _ := tx.C("leave_type").Exists(ctx, bson.M{"name": n}); !ok {
				return bad("Unknown leave type %s", n)
			}
		}
		if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
			b.Name = old.Name
		}
		if _, err := lp.Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"name": b.Name, "description": b.Description, "lv": b.LV, "lrx": b.LRX, "types": b.Types},
			"$inc": bson.M{"version": 1}}); err != nil {
			return err
		}
		if b.Active != nil {
			if _, err := lp.Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": map[bool]string{true: "Active", false: "Inactive"}[*b.Active]}}); err != nil {
				return err
			}
		}
		ch := diff(map[string]any{"name": old.Name, "description": old.Description, "lv": old.LV, "lrx": old.LRX, "types": old.Types},
			map[string]any{"name": b.Name, "description": b.Description, "lv": b.LV, "lrx": b.LRX, "types": b.Types})
		if err := configHistoryAt(ctx, tx, c, "leave_plan", id, ch, b.Effective); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Leave plan “"+b.Name+"” changed", "leave_plan", id, nil, ch)
	})
}

func (s *Server) leaveTypeAdd(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Paid     bool   `json:"paid"`
		CopyFrom string `json:"copyFrom"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	if len(b.Name) < 2 || len(b.Name) > 40 || !containsStr([]string{leave.KindBalance, leave.KindNone, leave.KindEvent}, b.Kind) {
		return nil, bad("Name the leave type and choose how it is counted")
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		lt := tx.C("leave_type")
		last, err := lt.FindOne(ctx, nil, options.FindOne().SetSort(bson.D{{Key: "sort", Value: -1}}).SetProjection(bson.M{"sort": 1}))
		if err != nil {
			return err
		}
		var sort any // the new type goes last (max(sort)+1, NULL when there is no type yet)
		if last != nil {
			n, _ := last["sort"].(int32)
			sort = int64(n) + 1
		}
		if _, err := lt.Insert(ctx, bson.M{"name": b.Name, "kind": b.Kind, "paid": b.Paid, "sort": sort}); err != nil {
			return err
		}
		// Every plan gets the new type, switched off until someone sets it up, with the settings of
		// the type it copies (in that plan) or the standard defaults.
		t := leave.Default(b.Kind)
		t.On = false
		if b.CopyFrom != "" {
			if ok, _ := lt.Exists(ctx, bson.M{"name": b.CopyFrom}); !ok {
				return bad("Unknown leave type %s to copy from", b.CopyFrom)
			}
		}
		if err := leaveTypeCopy(ctx, tx, b.Name, b.CopyFrom, t); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Leave type “"+b.Name+"” added", "leave_type", b.Name, nil, b)
	})
}

// ---------- Holiday calendars ----------

func (s *Server) calendars(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	ctx := r.Context()
	cals, err := s.st.DB.C("holiday_calendar").Find(ctx, nil, // in master order
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.M{"name": 1, "status": 1}))
	if err != nil {
		return nil, err
	}
	hol, err := s.st.DB.C("holiday").Aggregate(ctx, bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "date", Value: 1}, {Key: "_id", Value: 1}}}},
		bson.D{{Key: "$project", Value: bson.D{{Key: "cal", Value: "$calendar_id"}, {Key: "date", Value: 1}, {Key: "name", Value: 1}, {Key: "kind", Value: 1}, {Key: "on_list", Value: 1}}}},
	})
	if err != nil {
		return nil, err
	}
	for _, x := range cals {
		l := []store.Row{}
		for _, h := range hol {
			if h["cal"] == x["id"] {
				l = append(l, h)
			}
		}
		x["holidays"] = l
	}
	return cals, nil
}

func (s *Server) calendarSave(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Name     string `json:"name"`
		CopyFrom string `json:"copyFrom"`
		Holiday  *struct {
			ID   int64  `json:"id"`
			Date string `json:"date"`
			Name string `json:"name"`
			Kind string `json:"kind"`
			On   *bool  `json:"on"`
		} `json:"holiday"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	var out string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if id == "" { // a new calendar, optionally copying another's holidays
			if b.Name = strings.TrimSpace(b.Name); b.Name == "" {
				return bad("Name the calendar")
			}
			newID, err := tx.C("holiday_calendar").Insert(ctx, bson.M{"name": b.Name})
			if err != nil {
				return err
			}
			out = str(newID)
			src, err := tx.C("holiday").Find(ctx, bson.M{"calendar_id": b.CopyFrom}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
			if err != nil {
				return err
			}
			copies := make([]any, 0, len(src))
			for _, h := range src {
				copies = append(copies, bson.M{"calendar_id": out, "date": h["date"], "name": h["name"], "kind": h["kind"], "on_list": h["on_list"]})
			}
			if err := tx.C("holiday").InsertMany(ctx, copies); err != nil {
				return err
			}
			return audit(ctx, tx, c, "Holiday calendar “"+b.Name+"” created", "holiday_calendar", out, nil, b)
		}
		if h := b.Holiday; h != nil {
			if h.ID == 0 {
				if !isDate(h.Date) || strings.TrimSpace(h.Name) == "" || !containsStr([]string{"National", "Regional", "Optional"}, h.Kind) {
					return bad("Give the holiday a date, a name and a kind")
				}
				// One holiday a date on a calendar (TIME-098).
				if dup, _ := tx.C("holiday").FindOne(ctx, bson.M{"calendar_id": id, "date": h.Date}); dup != nil {
					return bad("%s is already a holiday on this calendar (%s)", longDate(h.Date), str(dup["name"]))
				}
				if _, err := tx.C("holiday").Insert(ctx, bson.M{"calendar_id": id, "date": h.Date, "name": strings.TrimSpace(h.Name), "kind": h.Kind}); err != nil {
					return err
				}
			} else if h.On != nil {
				if _, err := tx.C("holiday").Update(ctx, bson.M{"_id": h.ID, "calendar_id": id}, bson.M{"$set": bson.M{"on_list": *h.On}}); err != nil {
					return err
				}
			}
			if err := configHistory(ctx, tx, c, "holiday_calendar", id, []map[string]any{{"field": "holiday " + h.Date + " " + h.Name, "to": h}}); err != nil {
				return err
			}
			return audit(ctx, tx, c, "Holiday list changed", "holiday_calendar", id, nil, h)
		}
		if b.Name = strings.TrimSpace(b.Name); b.Name != "" {
			if _, err := tx.C("holiday_calendar").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"name": b.Name}}); err != nil {
				return err
			}
		}
		return nil
	})
	return map[string]string{"id": out}, err
}

// ---------- Exceptions (spec 10.2) ----------

func canExceptions(c *Ctx) error {
	// Spec 10.2: Payroll opens an exception for one employee and date range (payroll test RUN-13 / REAL-12).
	if c.Role == "HR Admin" || c.Role == "Payroll Manager" || c.Level("pay.approve") >= 2 || c.Level("pay.run") >= 2 {
		return nil
	}
	return forbidden("Only Payroll, the Payroll Manager or HR Admin can open exceptions")
}

func (s *Server) exceptionList(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && canExceptions(c) != nil {
		return nil, forbidden("")
	}
	// The newest 500 in the caller's scope, each with the employee's name.
	return s.st.DB.C("time_exception").Aggregate(r.Context(), bson.A{
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: -1}}}},
		bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "employee"}, {Key: "localField", Value: "employee_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "_e"}}}},
		bson.D{{Key: "$unwind", Value: "$_e"}},
		bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "department"}, {Key: "localField", Value: "_e.department_id"}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: "_d"}}}},
		bson.D{{Key: "$unwind", Value: "$_d"}},
		bson.D{{Key: "$match", Value: policiesScope(c)}},
		bson.D{{Key: "$limit", Value: 500}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "$_e.name"}}}},
		bson.D{{Key: "$unset", Value: bson.A{"_e", "_d"}}},
	})
}

func (s *Server) exceptionAdd(r *http.Request, c *Ctx) (any, error) {
	if err := canExceptions(c); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeID string `json:"employeeId"`
		Kind       string `json:"kind"`
		From       string `json:"from"`
		To         string `json:"to"`
		Until      string `json:"until"`
		Reason     string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	switch {
	case b.Kind != "leave" && b.Kind != "regularize":
		return nil, bad("Choose leave or regularization")
	case !isDate(b.From) || !isDate(b.To) || b.To < b.From:
		return nil, bad("Give the dates the exception covers")
	case !isDate(b.Until) || b.Until < today():
		return nil, bad("Give the last day the exception can be used")
	case strings.TrimSpace(b.Reason) == "":
		return nil, bad("Give a reason")
	}
	ctx := r.Context()
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		id, err := tx.C("time_exception").Insert(ctx, bson.M{"employee_id": b.EmployeeID, "kind": b.Kind, "from_date": b.From, "to_date": b.To, "until": b.Until,
			"reason": strings.TrimSpace(b.Reason), "created_by": c.Name + " (" + c.Role + ")"})
		if err != nil {
			return err
		}
		return audit(ctx, tx, c, "Exception opened for "+b.EmployeeID, "time_exception", fmt.Sprint(id), nil, b)
	})
}

func (s *Server) exceptionRevoke(r *http.Request, c *Ctx) (any, error) {
	if err := canExceptions(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		var n int64
		if id, ok := policiesSerial(r.PathValue("id")); ok {
			var err error
			if n, err = tx.C("time_exception").Update(ctx, bson.M{"_id": id, "status": "Active"},
				bson.M{"$set": bson.M{"status": "Revoked", "revoked_by": c.Name, "revoked_at": tx.Now()}}); err != nil {
				return err
			}
		}
		if n == 0 {
			return bad("Only an active exception can be revoked")
		}
		return audit(ctx, tx, c, "Exception revoked", "time_exception", r.PathValue("id"), nil, nil)
	})
}

// exceptionOpen: an active exception covers these dates (the window rules are skipped).
func exceptionOpen(ctx context.Context, q store.Q, empID, kind, from, to string) bool {
	ok, _ := q.C("time_exception").Exists(ctx, bson.M{"employee_id": empID, "kind": kind, "status": "Active", "until": bson.M{"$gte": store.DateOf(q.Now())},
		"from_date": bson.M{"$lte": from}, "to_date": bson.M{"$gte": to}})
	return ok
}
