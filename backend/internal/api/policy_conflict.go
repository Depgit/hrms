package api

// Policy conflict check (workforce plan module 5, decided 16 Nov 2026). Before a work policy mapping is saved, the
// people it would cover who already get a different policy through another mapping are shown, grouped by that policy
// and mapping ("Operations – 6 day is already applied here through the Operations department to 42 people"). HR
// then chooses:
//
//   - replace: those people move to the new policy;
//   - keep: they stay on the policy they have; the new mapping covers only the rest;
//   - pick: the people named in move go to the new policy, the others stay.
//
// The choice is stored as employee-level mappings marked exception (an employee mapping is tried first), so the
// outcome never depends on the order of the levels. It applies at once: mappings carry no dates, and a person's
// policy is worked out for whole months. The save is refused while there are conflicts and no choice.
//
// Not conflicts: an employee-level mapping (it is itself the explicit choice for that person); people on the
// default policy (no mapping matches them); and people whose mapping is the same one moving from another policy
// (a location, department and so on belongs to one policy at a time; since 24 Nov 2026 only Edit › move to another
// policy moves one, policy_maps.go: mapping it again elsewhere is refused): those are listed as moves.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// polMatch is one switched-on mapping of an active policy that matches an employee.
type polMatch struct {
	P *workPolicy
	M polMap
}

// policyMatches lists every mapping of an active policy that matches e, in the order policyFor tries them: by
// level, then by the policies' order. The first one is the policy the person gets.
func (t *timeSet) policyMatches(e *emp) []polMatch {
	out := []polMatch{}
	for _, lv := range policyLevels {
		ref := e.levelRef(lv)
		if ref == "" {
			continue
		}
		for _, p := range t.pols {
			for _, m := range p.Maps {
				if m.On && m.Level == lv && m.Ref == ref {
					out = append(out, polMatch{p, m})
				}
			}
		}
	}
	return out
}

// refNames names the value of a mapping: the employee, location, cluster, city or state behind an id; the others
// store the name itself.
type refNames struct {
	ctx   context.Context
	q     store.Q
	cache map[string]string
}

func newRefNames(ctx context.Context, q store.Q) *refNames {
	return &refNames{ctx: ctx, q: q, cache: map[string]string{}}
}

func (n *refNames) name(level, ref string) string {
	switch level {
	case "employee", "location", "cluster", "city", "state":
	default:
		if pl, ok := comboPlace[level]; ok { // a department at a place: "People Ops at Gurgaon office"
			if d, p, ok := splitCombo(ref); ok {
				return d + " at " + n.name(pl, p)
			}
		}
		return ref
	}
	k := level + "|" + ref
	if v, ok := n.cache[k]; ok {
		return v
	}
	v := ref
	if x, err := n.q.C(level).FindOne(n.ctx, bson.M{"_id": ref}, options.FindOne().SetProjection(bson.M{"name": 1})); err == nil && x != nil {
		v = str(x["name"])
		if level == "employee" {
			v += " (" + ref + ")"
		}
	}
	n.cache[k] = v
	return v
}

// label is a mapping in words: "the Operations department", "the location Gurugram – Sector 29 Hub".
func (n *refNames) label(m polMap) string {
	nm := n.name(m.Level, m.Ref)
	switch m.Level {
	case "department":
		return "the " + nm + " department"
	case "employee":
		if m.Exception {
			return "an exception for " + nm
		}
		return "a mapping for " + nm
	case "type":
		return nm + " employment type"
	}
	if pl, ok := comboPlace[m.Level]; ok { // "the People Ops department at location Gurgaon office"
		if d, p, ok := splitCombo(m.Ref); ok {
			return "the " + d + " department at " + strings.ToLower(levelNames[pl]) + " " + n.name(pl, p)
		}
	}
	return strings.ToLower(levelNames[m.Level]) + " " + nm
}

type conflictPerson struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Department string `json:"department"`
	Location   string `json:"location"`
}

// conflictGroup is the people who get one other policy through one mapping.
type conflictGroup struct {
	PolicyID string           `json:"policyId"`
	Policy   string           `json:"policy"`
	Level    string           `json:"level"`
	Ref      string           `json:"ref"`
	Through  string           `json:"through"`
	Count    int              `json:"count"`
	Text     string           `json:"text"`
	People   []conflictPerson `json:"people"`
}

type conflictResult struct {
	Policy    string          `json:"policy"`
	Conflicts []conflictGroup `json:"conflicts"`
	Moves     []conflictGroup `json:"moves"`
	People    int             `json:"people"`    // people in a conflict
	Newly     int             `json:"newly"`     // people the new mappings give this policy with no conflict
	Message   string          `json:"message"`   // the conflicts in one sentence
	ApplyFrom string          `json:"applyFrom"` // when a choice takes effect: today (mappings carry no dates)
	who       map[string]polMatch
}

// newMapsOf are the mappings a save switches on for policy id that are not on now (employee-level ones aside).
func newMapsOf(ts *timeSet, id string, maps []polMap) []polMap {
	on := map[string]bool{}
	for _, p := range ts.all {
		if p.ID != id {
			continue
		}
		for _, m := range p.Maps {
			if m.On {
				on[m.Level+"|"+m.Ref] = true
			}
		}
	}
	out := []polMap{}
	for _, m := range maps {
		if m.On && m.Level != "employee" && !on[m.Level+"|"+m.Ref] {
			out = append(out, m)
		}
	}
	return out
}

// policyConflicts works out who the new mappings of policy id would cover that already get a different policy.
func policyConflicts(ctx context.Context, q store.Q, ts *timeSet, id string, maps []polMap) (*conflictResult, error) {
	res := &conflictResult{Conflicts: []conflictGroup{}, Moves: []conflictGroup{}, ApplyFrom: today(), who: map[string]polMatch{}}
	var target *workPolicy
	for _, p := range ts.all {
		if p.ID == id {
			target = p
		}
	}
	if target == nil {
		return nil, notFound("Policy not found")
	}
	res.Policy = target.Name
	fresh := newMapsOf(ts, id, maps)
	if target.Status != "Active" || len(fresh) == 0 {
		return res, nil
	}
	emps, err := loadEmps(ctx, q, bson.M{"status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return nil, err
	}
	names := newRefNames(ctx, q)
	conf, moves := map[string]*conflictGroup{}, map[string]*conflictGroup{}
	var confOrder, moveOrder []string
	add := func(into map[string]*conflictGroup, order *[]string, k string, mt polMatch, e *emp) {
		g := into[k]
		if g == nil {
			g = &conflictGroup{PolicyID: mt.P.ID, Policy: mt.P.Name, Level: mt.M.Level, Ref: mt.M.Ref, Through: names.label(mt.M), People: []conflictPerson{}}
			into[k] = g
			*order = append(*order, k)
		}
		g.Count++
		g.People = append(g.People, conflictPerson{e.ID, e.Name, e.Dept, e.Location})
	}
	for _, e := range emps {
		var hit *polMap
		for i := range fresh {
			if mapMatches(e, fresh[i]) {
				hit = &fresh[i]
				break
			}
		}
		if hit == nil {
			continue
		}
		ms := ts.policyMatches(e)
		if len(ms) == 0 || ms[0].P.ID == id {
			res.Newly++
			continue
		}
		cur := ms[0]
		k := cur.P.ID + "|" + cur.M.Level + "|" + cur.M.Ref
		same := false
		for _, f := range fresh {
			if f.Level == cur.M.Level && f.Ref == cur.M.Ref {
				same = true
			}
		}
		if same {
			add(moves, &moveOrder, k, cur, e)
			continue
		}
		add(conf, &confOrder, k, cur, e)
		res.who[e.ID] = cur
	}
	plural := func(n int) string {
		return fmt.Sprintf("%d %s", n, map[bool]string{true: "person", false: "people"}[n == 1])
	}
	parts := []string{}
	for _, k := range confOrder {
		g := conf[k]
		g.Text = fmt.Sprintf("%s is already applied here (through %s) to %s", g.Policy, g.Through, plural(g.Count))
		res.Conflicts = append(res.Conflicts, *g)
		res.People += g.Count
		parts = append(parts, g.Text)
	}
	for _, k := range moveOrder {
		g := moves[k]
		g.Text = fmt.Sprintf("%s moves from %s to %s (%s)", strings.ToUpper(g.Through[:1])+g.Through[1:], g.Policy, target.Name, plural(g.Count))
		res.Moves = append(res.Moves, *g)
	}
	if len(parts) > 0 {
		res.Message = strings.Join(parts, ". ") + "."
	}
	return res, nil
}

// policyResolution is HR's choice for the conflicts of a save.
type policyResolution struct {
	Mode string   `json:"mode"` // replace | keep | pick
	Move []string `json:"move"` // pick: the people who move to the new policy
}

// policyMapsPreview: POST /api/policies/{id}/maps/preview {maps}: the conflicts the save would meet.
func (s *Server) policyMapsPreview(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, true); err != nil {
		return nil, err
	}
	var b struct {
		Maps []polMap `json:"maps"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if err := ts.mapDupCheck(r.PathValue("id"), b.Maps); err != nil { // already applied elsewhere: refused before any question
		return nil, err
	}
	return policyConflicts(ctx, s.st.DB, ts, r.PathValue("id"), b.Maps)
}

// conflictRefusal is the error of a save that meets conflicts without a choice.
func conflictRefusal(cf *conflictResult) error {
	return &apiError{Status: 409, Message: cf.Message + " Choose Replace, Keep or Pick people, or cancel.", Details: cf}
}

// setEmployeeMap switches on (or adds) the employee-level mapping of one person to policy pid as an exception with
// a note, and switches the person's employee mapping off on every other policy.
func setEmployeeMap(ctx context.Context, tx store.Q, c *Ctx, pid, empID, note string) error {
	wm := tx.C("work_policy_map")
	x, err := wm.FindOne(ctx, bson.M{"policy_id": pid, "level": "employee", "ref": empID})
	if err != nil {
		return err
	}
	if x != nil {
		if _, err := wm.Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"on_map": true, "exception": true, "note": note}}); err != nil {
			return err
		}
	} else if _, err := wm.Insert(ctx, bson.M{"policy_id": pid, "level": "employee", "ref": empID, "exception": true, "note": note, "created_by": c.Name}); err != nil {
		return err
	}
	return moveMapOff(ctx, tx, c, pid, "employee", empID)
}

// applyResolution stores HR's choice as employee-level exceptions and returns the change lines per policy.
func applyResolution(ctx context.Context, tx store.Q, c *Ctx, id string, cf *conflictResult, res *policyResolution) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	if len(cf.who) == 0 {
		return out, nil
	}
	move := map[string]bool{}
	switch res.Mode {
	case "replace":
		for e := range cf.who {
			move[e] = true
		}
	case "keep":
	case "pick":
		for _, e := range res.Move {
			if _, ok := cf.who[e]; !ok {
				return nil, bad("%s is not one of the people in conflict", e)
			}
			move[e] = true
		}
	default:
		return nil, bad("Choose Replace, Keep or Pick people")
	}
	ids := make([]string, 0, len(cf.who))
	for e := range cf.who {
		ids = append(ids, e)
	}
	sort.Strings(ids)
	day := time.Now().In(ist).Format("02 Jan 2006")
	moved, kept := map[string]int{}, map[string]int{}
	for _, e := range ids {
		old := cf.who[e]
		if move[e] {
			note := fmt.Sprintf("Moved from %s to %s on %s (policy conflict: Replace)", old.P.Name, cf.Policy, day)
			if err := setEmployeeMap(ctx, tx, c, id, e, note); err != nil {
				return nil, err
			}
			moved[old.P.ID]++
			continue
		}
		if old.M.Level == "employee" { // already an explicit mapping for this person: nothing to add
			kept[old.P.ID]++
			continue
		}
		note := fmt.Sprintf("Kept on %s when %s was mapped on %s (policy conflict: Keep)", old.P.Name, cf.Policy, day)
		if err := setEmployeeMap(ctx, tx, c, old.P.ID, e, note); err != nil {
			return nil, err
		}
		kept[old.P.ID]++
	}
	for pid, n := range moved {
		out[id] = append(out[id], map[string]any{"field": "exceptions", "to": fmt.Sprintf("%d moved here from another policy", n)})
		out[pid] = append(out[pid], map[string]any{"field": "exceptions", "to": fmt.Sprintf("%d moved to %s", n, cf.Policy)})
	}
	for pid, n := range kept {
		out[pid] = append(out[pid], map[string]any{"field": "exceptions", "to": fmt.Sprintf("%d kept here (not moved to %s)", n, cf.Policy)})
	}
	return out, nil
}

// ---------- Overlap report and effective policy ----------

type overlapRow struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Department string           `json:"department"`
	Location   string           `json:"location"`
	Policy     string           `json:"policy"`
	Through    string           `json:"through"`
	Matches    []map[string]any `json:"matches"`
}

// policyOverlaps: everyone in scope matched by two or more switched-on mappings that point to different policies.
func (s *Server) policyOverlapRows(ctx context.Context, c *Ctx) ([]overlapRow, error) {
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, bson.M{"status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return nil, err
	}
	names := newRefNames(ctx, s.st.DB)
	out := []overlapRow{}
	for _, e := range emps {
		ms := ts.policyMatches(e)
		pols := map[string]bool{}
		for _, m := range ms {
			pols[m.P.ID] = true
		}
		if len(pols) < 2 {
			continue
		}
		row := overlapRow{ID: e.ID, Name: e.Name, Department: e.Dept, Location: e.Location, Policy: ms[0].P.Name, Through: names.label(ms[0].M), Matches: []map[string]any{}}
		for i, m := range ms {
			row.Matches = append(row.Matches, map[string]any{"policyId": m.P.ID, "policy": m.P.Name, "level": m.M.Level, "through": names.label(m.M), "wins": i == 0})
		}
		out = append(out, row)
	}
	return out, nil
}

// policyOverlaps: GET /api/policies/overlaps.
func (s *Server) policyOverlaps(r *http.Request, c *Ctx) (any, error) {
	if err := canPolicies(c, false); err != nil {
		return nil, err
	}
	rows, err := s.policyOverlapRows(r.Context(), c)
	if err != nil {
		return nil, err
	}
	return map[string]any{"rows": rows, "total": len(rows)}, nil
}

// policyOverlapsSheet: GET /api/policies/overlaps.xlsx, the overlap report as a file.
func (s *Server) policyOverlapsSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := canPolicies(c, false); err != nil {
		return "", nil, nil, err
	}
	list, err := s.policyOverlapRows(r.Context(), c)
	if err != nil {
		return "", nil, nil, err
	}
	cols := []string{"Employee ID", "Name", "Department", "Location", "Policy they get", "Through", "Also matched by"}
	rows := [][]any{}
	for _, x := range list {
		also := []string{}
		for _, m := range x.Matches[1:] {
			also = append(also, fmt.Sprintf("%s (through %s)", m["policy"], m["through"]))
		}
		rows = append(rows, []any{x.ID, x.Name, x.Department, x.Location, x.Policy, x.Through, strings.Join(also, "; ")})
	}
	return "policy-overlaps-" + today() + ".xlsx", cols, rows, nil
}

// employeePolicy: GET /api/employees/{id}/policy: the work policy the person gets and why, and every other
// mapping that matches them.
func (s *Server) employeePolicy(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	if id != c.EmployeeID && c.Level("pol.policies") < 1 {
		if err := c.Need("emp.directory", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	e, err := loadEmp(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	p, via := ts.policyFor(e)
	names := newRefNames(ctx, s.st.DB)
	out := map[string]any{"policyId": p.ID, "policy": p.Name, "via": via, "exception": false, "note": "", "others": []map[string]any{}}
	if pl := ts.plan(p); pl != nil {
		out["leavePlan"] = pl.Name
	}
	ms := ts.policyMatches(e)
	if len(ms) == 0 {
		out["why"] = fmt.Sprintf("No mapping matches %s, so they get the first active policy, %s, by default.", e.Name, p.Name)
		return out, nil
	}
	w := ms[0]
	out["level"], out["through"], out["exception"], out["note"] = w.M.Level, names.label(w.M), w.M.Exception, w.M.Note
	if w.M.Exception {
		out["why"] = fmt.Sprintf("%s is set for %s as an exception. %s", p.Name, e.Name, w.M.Note)
	} else {
		out["why"] = fmt.Sprintf("%s applies through %s.", p.Name, names.label(w.M))
	}
	others := []map[string]any{}
	for _, m := range ms[1:] {
		x := map[string]any{"policyId": m.P.ID, "policy": m.P.Name, "level": m.M.Level, "through": names.label(m.M)}
		if m.P.ID != p.ID {
			x["overridden"] = true
		}
		others = append(others, x)
	}
	out["others"] = others
	return out, nil
}

func (s *Server) accessPolicyRoutes(m *http.ServeMux) {
	// Personal rights and "who can see what" (access_personal.go).
	m.HandleFunc("GET /api/user-rights", s.a(s.userRightsList))
	m.HandleFunc("POST /api/user-rights", s.a(s.userRightAdd))
	m.HandleFunc("DELETE /api/user-rights/{id}", s.a(s.userRightEnd))
	m.HandleFunc("GET /api/access/person/{id}/effective", s.a(s.accessEffective))
	m.HandleFunc("GET /api/access/person/{id}/effective.xlsx", s.sheet(s.accessEffectiveSheet))
	// Policy conflict check, overlap report and each employee's effective policy (policy_conflict.go).
	m.HandleFunc("POST /api/policies/{id}/maps/preview", s.a(s.policyMapsPreview))
	// Edit one mapping: its value and/or the policy it is on (policy_maps.go).
	m.HandleFunc("POST /api/policies/{id}/maps/{mapId}/preview", s.a(s.policyMapEditPreview))
	m.HandleFunc("PUT /api/policies/{id}/maps/{mapId}", s.a(s.policyMapEdit))
	m.HandleFunc("GET /api/policies/overlaps", s.a(s.policyOverlaps))
	m.HandleFunc("GET /api/policies/overlaps.xlsx", s.sheet(s.policyOverlapsSheet))
	m.HandleFunc("GET /api/employees/{id}/policy", s.a(s.employeePolicy))
}
