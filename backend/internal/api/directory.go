package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// Approver directory: who holds each approving role, per property, department and company.
// One function (pickHolder) decides for the approval engine, the Approver names tab and the health check.

type mapping struct {
	role, id, name, access string
	locs, depts            []string
	left                   bool
}

// loadMappings returns the active role mappings of active roles for one role ("" = all roles), oldest first.
// Holders who have left are kept so the engine can see "has left" and skip their stage; people not joined yet are
// not. Mappings made at the same moment (the seed, one import) keep the order they were stored in, as the old
// ORDER BY created_at over a table scan did: that order breaks ties between holders.
func loadMappings(ctx context.Context, q store.Q, role string) ([]mapping, error) {
	f := bson.M{"active": true}
	if role != "" {
		f["role_name"] = role
	}
	rows, err := q.C("user_role_map").Find(ctx, f, ctxStoredOrder())
	if err != nil {
		return nil, err
	}
	activeRoles, err := q.C("role").Distinct(ctx, "name", bson.M{"active": true})
	if err != nil {
		return nil, err
	}
	roleOn := map[any]bool{}
	for _, r := range activeRoles {
		roleOn[r] = true
	}
	emps, err := ctxIndex(ctx, q, "employee", "_id", ctxValues(rows, "employee_id"))
	if err != nil {
		return nil, err
	}
	type made struct {
		m  mapping
		at time.Time
	}
	list := []made{}
	for _, x := range rows {
		e := emps[str(x["employee_id"])]
		if e == nil || e["status"] == "Pre-boarding" || !roleOn[x["role_name"]] {
			continue
		}
		at, _ := x["created_at"].(time.Time)
		acc, locs, err := mapLocations(ctx, q, x) // clusters, cities or states: the locations inside them now
		if err != nil {
			return nil, err
		}
		list = append(list, made{mapping{role: str(x["role_name"]), id: str(x["employee_id"]), name: str(e["name"]), access: acc,
			locs: locs, depts: strs(x["departments"]), left: e["status"] == "Inactive"}, at})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	out := make([]mapping, len(list))
	for i, x := range list {
		out[i] = x.m
	}
	return out, nil
}

// pickHolder: a location-level mapping beats an all-locations one, and an active holder always
// beats one who has left.
func pickHolder(role string, s *subject, maps []mapping) Person {
	c := holderCands(role, s, maps)
	if len(c) == 0 {
		return unassigned
	}
	return c[0].p
}

// holderCand is a person who could hold a role for a subject; a lower rank wins, and a holder who
// has left ranks 10 lower.
type holderCand struct {
	p    Person
	rank int
}

// holderCands lists the candidates for a role, best first (pickHolder takes the first; the
// Approver names tab reports a tie for the first place as an overlap).
func holderCands(role string, s *subject, maps []mapping) []holderCand {
	var c []holderCand
	add := func(m mapping, rank int) {
		if m.left {
			rank += 10
		}
		c = append(c, holderCand{Person{m.id, m.name}, rank})
	}
	for _, m := range maps {
		if m.role != role {
			continue
		}
		switch {
		case role == "HRBP" && s.Corporate && containsStr(m.depts, s.Department):
			add(m, 0)
		case locationRoles[role]:
			if len(m.depts) > 0 {
				continue
			}
			if m.access == "locs" && containsStr(m.locs, s.LocationID) {
				add(m, 1)
			} else if m.access == "all" {
				add(m, 2)
			}
		case role == "Function Head":
			if containsStr(m.depts, s.Department) {
				add(m, 1)
			} else if len(m.depts) == 0 && m.access == "all" {
				add(m, 2)
			}
		default: // company-wide roles: CHRO, Payroll, Payroll Manager, Finance Head
			rank := 2
			if m.access == "all" {
				rank = 1
			}
			add(m, rank)
		}
	}
	sort.SliceStable(c, func(i, j int) bool { return c[i].rank < c[j].rank })
	return c
}

// overlapNote says when two or more active people tie for a role at a place, and who is used
// (prototype syncApprovers: "Property Manager for X: A and B are both mapped. A is used.").
func overlapNote(label string, c []holderCand) string {
	if len(c) < 2 || c[0].rank >= 10 {
		return ""
	}
	names, seen := []string{}, map[string]bool{}
	for _, x := range c {
		if x.rank != c[0].rank || seen[x.p.ID] {
			continue
		}
		seen[x.p.ID] = true
		names = append(names, x.p.Name)
	}
	if len(names) < 2 {
		return ""
	}
	return label + ": " + strings.Join(names, " and ") + " are both mapped. " + names[0] + " is used."
}

var (
	locationRoleList = []string{"Property Manager", "HRBP", "Cluster Manager", "HRBP Corporate", "Operations Head"}
	companyRoleList  = []string{"CHRO", "Payroll", "Payroll Manager", "Finance Head"}
)

type namedPlace struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Path   string            `json:"path,omitempty"`
	Corp   bool              `json:"corporate,omitempty"`
	People map[string]Person `json:"people"`
}

type directoryView struct {
	Locations   []namedPlace      `json:"locations"`
	Departments []namedPlace      `json:"departments"`
	Company     map[string]Person `json:"company"`
	Gaps        []string          `json:"gaps"`
	Left        []string          `json:"left"`
	Overlaps    []string          `json:"overlaps"` // two people mapped to the same role and place
}

// usedRoles are the roles that approve in at least one rule in use.
func usedRoles(ctx context.Context, q store.Q) (map[string]bool, error) {
	flows, err := loadFlows(ctx, q)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, fl := range flows {
		for i, r := range fl.Rules {
			if !fl.inUse(i) {
				continue
			}
			for _, role := range r.Approvers {
				used[approval.StageRole(role)] = true
			}
		}
	}
	return used, nil
}

func buildDirectory(ctx context.Context, q store.Q) (*directoryView, error) {
	maps, err := loadMappings(ctx, q, "")
	if err != nil {
		return nil, err
	}
	used, err := usedRoles(ctx, q)
	if err != nil {
		return nil, err
	}
	d := &directoryView{Company: map[string]Person{}, Gaps: []string{}, Left: []string{}, Overlaps: []string{}}
	overlap := func(label string, c []holderCand) {
		if n := overlapNote(label, c); n != "" {
			d.Overlaps = append(d.Overlaps, n)
		}
	}
	leftNames := map[string]bool{}
	note := func(p Person, label string) {
		for _, m := range maps {
			if m.id == p.ID && m.left && !leftNames[p.Name] {
				leftNames[p.Name] = true
				d.Left = append(d.Left, p.Name+" ("+label+")")
			}
		}
	}
	locs, err := dirActiveLocations(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, l := range locs {
		np := namedPlace{ID: l["id"].(string), Name: l["name"].(string), Path: l["path"].(string), People: map[string]Person{}}
		for _, role := range locationRoleList {
			p := pickHolder(role, &subject{LocationID: np.ID}, maps)
			overlap(role+" for "+np.Name, holderCands(role, &subject{LocationID: np.ID}, maps))
			np.People[role] = p
			if p.ID == "" && used[role] {
				d.Gaps = append(d.Gaps, role+" for "+np.Name)
			}
			note(p, role)
		}
		d.Locations = append(d.Locations, np)
	}
	depts, err := q.C("department").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, dp := range depts {
		np := namedPlace{ID: dp["id"].(string), Name: dp["name"].(string), Corp: dp["corporate"].(bool), People: map[string]Person{}}
		fh := pickHolder("Function Head", &subject{Department: np.Name, Corporate: np.Corp}, maps)
		overlap("Function Head for "+np.Name, holderCands("Function Head", &subject{Department: np.Name, Corporate: np.Corp}, maps))
		np.People["Function Head"] = fh
		if fh.ID == "" && used["Function Head"] {
			d.Gaps = append(d.Gaps, "Function Head for "+np.Name)
		}
		note(fh, "Function Head")
		if np.Corp {
			h := unassigned
			corp := []holderCand{}
			for _, m := range maps {
				if m.role == "HRBP" && containsStr(m.depts, np.Name) {
					if h.ID == "" {
						h = Person{m.id, m.name}
					}
					if !m.left {
						corp = append(corp, holderCand{Person{m.id, m.name}, 1})
					}
				}
			}
			overlap("HRBP for "+np.Name, corp)
			np.People["HRBP"] = h
			if h.ID == "" && used["HRBP"] {
				d.Gaps = append(d.Gaps, "HRBP for "+np.Name)
			}
		}
		d.Departments = append(d.Departments, np)
	}
	for _, role := range companyRoleList {
		p := pickHolder(role, &subject{}, maps)
		overlap(role, holderCands(role, &subject{}, maps))
		d.Company[role] = p
		if p.ID == "" && used[role] {
			d.Gaps = append(d.Gaps, role)
		}
		note(p, role)
	}
	return d, nil
}

// dirActiveLocations are the active locations with their place in the hierarchy ("Cluster › City › State ›
// Region"), ordered by city and name. A location whose cluster, city, state or region is missing is left out.
func dirActiveLocations(ctx context.Context, q store.Q) ([]store.Row, error) {
	locs, err := q.C("location").Find(ctx, bson.M{"status": "Active"})
	if err != nil {
		return nil, err
	}
	names := map[string]map[string]store.Row{}
	for _, coll := range []string{"cluster", "city", "state", "region"} {
		rows, err := q.C(coll).Find(ctx, nil)
		if err != nil {
			return nil, err
		}
		names[coll] = map[string]store.Row{}
		for _, x := range rows {
			names[coll][x["id"].(string)] = x
		}
	}
	type place struct {
		row  store.Row
		city string
	}
	list := []place{}
	for _, l := range locs {
		k := names["cluster"][str(l["cluster_id"])]
		if k == nil {
			continue
		}
		c := names["city"][str(k["city_id"])]
		if c == nil {
			continue
		}
		st := names["state"][str(c["state_id"])]
		if st == nil {
			continue
		}
		rg := names["region"][str(st["region_id"])]
		if rg == nil {
			continue
		}
		path := str(k["name"]) + " › " + str(c["name"]) + " › " + str(st["name"]) + " › " + str(rg["name"])
		list = append(list, place{store.Row{"id": l["id"], "name": l["name"], "path": path}, str(c["name"])})
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.city != b.city {
			return a.city < b.city
		}
		if an, bn := str(a.row["name"]), str(b.row["name"]); an != bn {
			return an < bn
		}
		return str(a.row["id"]) < str(b.row["id"])
	})
	out := make([]store.Row, len(list))
	for i, p := range list {
		out[i] = p.row
	}
	return out, nil
}

// approverNames is the read-only "Approver names" tab: names come from the role mapping.
func (s *Server) approverNames(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("appr.queue", 1); err != nil {
		return nil, err
	}
	return buildDirectory(r.Context(), s.st.DB)
}

// healthItem is one setup check (prototype apprHealth): the affected items, how many there are,
// and the sentence the prototype writes under the check.
type healthItem struct {
	Title  string   `json:"title"`
	OK     bool     `json:"ok"`
	Detail string   `json:"detail,omitempty"`
	Fix    string   `json:"fix,omitempty"` // names (Approver names tab) | users (Role mapping)
	Count  int      `json:"count"`
	Items  []string `json:"items"`
}

// healthWF names a rule list as the prototype's WFN: "Leave, group C" for a leave list.
func healthWF(wf string) string {
	if strings.HasPrefix(wf, "leave") && len(wf) == 6 {
		return "Leave, group " + wf[5:]
	}
	if wf == "p_pip" {
		return "Performance improvement plan"
	}
	return workflowName(wf)
}

// approvalHealth is the setup check on the "Check a flow" tab. The Employee view reads it too, as in
// the prototype, with the affected items cut to the examples the prototype's sentences name.
func (s *Server) approvalHealth(r *http.Request, c *Ctx) (any, error) {
	if c.Level("appr.queue") < 1 && c.Role != "Employee" {
		return nil, forbidden("")
	}
	full := c.Level("appr.queue") >= 1
	ctx := r.Context()
	d, err := buildDirectory(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	first := func(l []string, n int) []string {
		if len(l) > n {
			return l[:n]
		}
		return l
	}
	item := func(title string, l []string, detail func() string, fix string, sample int) healthItem {
		h := healthItem{Title: title, OK: len(l) == 0, Count: len(l), Items: []string{}}
		if len(l) > 0 {
			h.Detail, h.Fix, h.Items = detail(), fix, l
			if !full && sample > 0 {
				h.Items = first(l, sample)
			}
		}
		return h
	}
	pl := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	}
	gaps := d.Gaps
	out := []healthItem{item("Every approver in use has a name", gaps, func() string {
		return fmt.Sprintf("%d missing, for example %s", len(gaps), strings.Join(first(gaps, 2), " and "))
	}, "names", 2)}
	// Everyone named in the directory: who has left, and who cannot sign in yet.
	maps, err := loadMappings(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	gone := map[string]bool{}
	for _, m := range maps {
		if m.left {
			gone[m.id] = true
		}
	}
	named := map[string]string{}
	for _, l := range d.Locations {
		for _, p := range l.People {
			named[p.ID] = p.Name
		}
	}
	for _, dp := range d.Departments {
		for _, p := range dp.People {
			named[p.ID] = p.Name
		}
	}
	for _, p := range d.Company {
		named[p.ID] = p.Name
	}
	delete(named, "")
	ids := []any{}
	for id := range named {
		ids = append(ids, id)
	}
	creds, _ := s.st.DB.C("auth_credential").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}})
	canSignIn := map[any]bool{}
	for _, id := range creds {
		canSignIn[id] = true
	}
	var left, noLogin []string
	for id, name := range named {
		if gone[id] {
			left = append(left, name)
		} else if !canSignIn[id] {
			noLogin = append(noLogin, name)
		}
	}
	sort.Strings(left)
	sort.Strings(noLogin)
	out = append(out,
		item("Nobody named as an approver has left", left, func() string {
			return fmt.Sprintf("%s %s left. Their stages are skipped until you replace them", strings.Join(left, ", "), pl(len(left), "has", "have"))
		}, "names", 0),
		item("Every named approver can sign in", noLogin, func() string {
			return fmt.Sprintf("%d cannot sign in, for example %s", len(noLogin), strings.Join(first(noLogin, 3), ", "))
		}, "users", 3))
	// Rules with conditions but no approver (a request they fit is approved without anyone).
	flows, err := loadFlows(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	empty := []string{}
	for _, fl := range flows {
		for i, rule := range fl.Rules {
			if fl.inUse(i) && len(rule.Approvers) == 0 && len(rule.When) > 0 {
				empty = append(empty, healthWF(fl.WF)+" / "+rule.Label)
			}
		}
	}
	out = append(out, item("Every condition has at least one approver", empty, func() string { return strings.Join(first(empty, 2), "; ") }, "", 2))
	used, err := usedRoles(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	usedList := []any{}
	for role := range used {
		usedList = append(usedList, role)
	}
	// The highest approving right (requests or claims) each role has, and the roles some active employee holds.
	lvl := map[string]int{}
	rights, _ := s.st.DB.C("role_right").Find(ctx, bson.M{"role_name": bson.M{"$in": usedList}, "right_id": bson.M{"$in": bson.A{"appr.queue", "pay.claims"}}})
	for _, x := range rights {
		if n := int(x["level"].(int16)); n > lvl[x["role_name"].(string)] {
			lvl[x["role_name"].(string)] = n
		}
	}
	held := map[string]bool{}
	if maps, err := s.st.DB.C("user_role_map").Find(ctx, bson.M{"role_name": bson.M{"$in": usedList}, "active": true}); err == nil {
		working, _ := s.st.DB.C("employee").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": ctxValues(maps, "employee_id")}, "status": "Active"})
		isWorking := map[any]bool{}
		for _, id := range working {
			isWorking[id] = true
		}
		for _, x := range maps {
			if isWorking[x["employee_id"]] {
				held[x["role_name"].(string)] = true
			}
		}
	}
	var noRight, noUser []string
	payRoles := map[string]bool{"Payroll": true, "Payroll Manager": true, "Finance Head": true, "CHRO": true}
	for role := range used {
		if lvl[role] < 2 && !payRoles[role] {
			noRight = append(noRight, role)
		}
		if role != "Line Manager" && !held[role] {
			noUser = append(noUser, role)
		}
	}
	sort.Strings(noRight)
	sort.Strings(noUser)
	out = append(out,
		item("Every approving role has the right to approve", noRight, func() string {
			return fmt.Sprintf("%s %s in a flow but cannot approve. Fix it in Access control", strings.Join(noRight, ", "), pl(len(noRight), "is", "are"))
		}, "", 0),
		item("Every approving role has at least one user", noUser, func() string { return "No active user with the role: " + strings.Join(noUser, ", ") }, "users", 0))
	return out, nil
}
