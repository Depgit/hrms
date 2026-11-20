package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Punch machines as one pool per cluster (workforce plan, module 7; decided 16 Nov 2026). Each machine (T304F
// Mini) holds about 1,000 people. The machines of a cluster are one shared store: a cluster with 10 properties (10
// machines) holds about 10,000 people, a person can punch at any machine of their cluster, and the records are
// collected later. So a move between properties of one cluster needs nothing; a move to another cluster needs
// enrolment there and removal from the old one.
//
//   - Machines: "Import machines from the portal" reads the Realtime portal's machine list (internal/realtime,
//     MachineList.aspx, read only) and creates or updates attendance_device rows (vendor Realtime) by serial number.
//     HR maps each machine to a property (location); its cluster is the property's. A machine's capacity is its own
//     or the default (1,000). Where the portal's branch or the machine's name matches a property's name, the page
//     suggests it.
//   - The board: per cluster, capacity (its active machines), people (active employees, Joining-ID records
//     included, whose property is in it), free and % used: a warning from 90%, full at 100%.
//   - Onboarding and transfers into a full cluster are refused (409, details.kind "cluster_full") unless the request
//     says to go ahead (?machineOverride=1 or the X-Machine-Override: 1 header), which is audited (machineGate).
//     A cluster with no machines mapped is not checked.
//   - Where each record is stored (mpPlan): the person's own property's machine first, then the next machine of the
//     cluster with space. Deterministic (earliest joiner first) and worked out afresh on every read.
//   - The to-do list: the difference between where each active employee works now and the HRMS's record of who is
//     enrolled on which cluster (machine_enrol): a joiner "enrol in cluster X (machine Y)", an exit "remove from
//     cluster X", a move to another cluster "enrol in B, remove from A"; a move inside a cluster creates nothing.
//     HR ticks a task done, or it is ticked when the portal's employee list is fetched (Match portal codes) and shows
//     the person's Attendance code in a branch of that cluster (enrol), or no longer in one (remove).
//   - Online/offline: the portal's machine list has no such column today (17 Nov 2026); when it appears it is read
//     and shown. No alerts.
//
// Settings and the last import and check are company_setting.machine_pool.

const (
	mpDefCap   = 1000
	mpMaxCap   = 100000
	mpWarnPct  = 90
	mpDoneDays = 30
)

// mpFetchMachines downloads and reads the portal's machine list. Tests replace it.
var mpFetchMachines = func(ctx context.Context, cfg realtime.Config) ([]realtime.Machine, error) {
	c, err := realtime.New(cfg)
	if err != nil {
		return nil, err
	}
	b, err := c.MachineList(ctx)
	if err != nil {
		return nil, err
	}
	return realtime.ParseMachines(b)
}

// ---------- Settings ----------

func mpSettings(ctx context.Context, q store.Q) (bson.M, error) {
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"machine_pool": 1}))
	if err != nil {
		return nil, err
	}
	out := bson.M{}
	if r != nil {
		switch m := r["machine_pool"].(type) {
		case map[string]any:
			for k, v := range m {
				out[k] = v
			}
		case bson.M:
			for k, v := range m {
				out[k] = v
			}
		}
	}
	if n := timectxInt(out["default_capacity"]); n <= 0 || n > mpMaxCap {
		out["default_capacity"] = mpDefCap
	}
	return out, nil
}

func mpSet(ctx context.Context, q store.Q, set bson.M) error {
	cur, err := mpSettings(ctx, q)
	if err != nil {
		return err
	}
	for k, v := range set {
		cur[k] = v
	}
	_, err = q.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"machine_pool": map[string]any(cur)}})
	return err
}

// ---------- The pool ----------

type mpMachine struct {
	ID                                        int
	Name, Serial, Model, Mode, Branch, Vendor string
	LocID, LocName, ClusterID                 string
	Cap                                       int
	CapSet, Active, Gone                      bool
	Online                                    *bool
	PortalStatus                              string
	SeenAt                                    any
}

type mpPerson struct{ ID, Name, LocID, DOJ, Code string }

type mpCluster struct {
	ID, Name, City string
	Machines       []*mpMachine // active machines mapped to a property of the cluster, in plan order
	People         []mpPerson   // active employees whose property is in the cluster, earliest joiner first
	Cap            int
}

type mpPool struct {
	defCap      int
	settings    bson.M
	machines    []*mpMachine
	clusters    map[string]*mpCluster
	order       []string // cluster ids by name
	locCluster  map[string]string
	locName     map[string]string
	locs        []store.Row
	people      map[string]mpPerson
	assign      map[string]*mpMachine // employee → the machine their record is planned on
	used        map[int]int
	unplaced    map[string][]string // cluster → employees with no space left
	clusterName map[string]string
}

func (p *mpPool) cname(id string) string {
	if id == "" {
		return ""
	}
	if n := p.clusterName[id]; n != "" {
		return n
	}
	return "an unknown cluster"
}

// mpLoad reads the machines, properties, clusters and active employees and works out the plan.
func mpLoad(ctx context.Context, q store.Q) (*mpPool, error) {
	set, err := mpSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	p := &mpPool{defCap: timectxInt(set["default_capacity"]), settings: set, clusters: map[string]*mpCluster{}, locCluster: map[string]string{},
		locName: map[string]string{}, people: map[string]mpPerson{}, clusterName: map[string]string{}}
	cities := map[string]string{}
	cs, err := q.C("city").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		cities[str(c["id"])] = str(c["name"])
	}
	cls, err := q.C("cluster").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1, "city_id": 1}))
	if err != nil {
		return nil, err
	}
	for _, c := range cls {
		id := str(c["id"])
		p.clusters[id] = &mpCluster{ID: id, Name: str(c["name"]), City: cities[str(c["city_id"])]}
		p.clusterName[id] = str(c["name"])
		p.order = append(p.order, id)
	}
	sort.SliceStable(p.order, func(i, j int) bool { return p.clusters[p.order[i]].Name < p.clusters[p.order[j]].Name })
	if p.locs, err = q.C("location").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1, "cluster_id": 1, "status": 1}).SetSort(bson.D{{Key: "name", Value: 1}})); err != nil {
		return nil, err
	}
	for _, l := range p.locs {
		p.locCluster[str(l["id"])] = str(l["cluster_id"])
		p.locName[str(l["id"])] = str(l["name"])
	}
	devs, err := q.C("attendance_device").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		m := &mpMachine{ID: timectxInt(d["id"]), Name: str(d["name"]), Serial: str(d["serial"]), Model: str(d["model"]), Mode: str(d["io_mode"]),
			Branch: str(d["portal_branch"]), Vendor: str(d["vendor"]), LocID: str(d["location_id"]), Active: d["status"] == "Active",
			Gone: d["portal_gone"] == true, PortalStatus: str(d["portal_status"]), SeenAt: d["portal_seen_at"]}
		if b, ok := d["online"].(bool); ok {
			m.Online = &b
		}
		m.LocName, m.ClusterID = p.locName[m.LocID], p.locCluster[m.LocID]
		m.Cap, m.CapSet = p.defCap, d["capacity"] != nil
		if m.CapSet {
			m.Cap = timectxInt(d["capacity"])
		}
		p.machines = append(p.machines, m)
		if cl := p.clusters[m.ClusterID]; cl != nil && m.Active {
			cl.Machines = append(cl.Machines, m)
			cl.Cap += m.Cap
		}
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"status": "Active"}, options.Find().SetProjection(bson.M{"name": 1, "location_id": 1, "doj": 1, "attendance_code": 1}))
	if err != nil {
		return nil, err
	}
	for _, e := range emps {
		pe := mpPerson{ID: str(e["id"]), Name: str(e["name"]), LocID: str(e["location_id"]), DOJ: str(e["doj"]), Code: str(e["attendance_code"])}
		p.people[pe.ID] = pe
		if cl := p.clusters[p.locCluster[pe.LocID]]; cl != nil {
			cl.People = append(cl.People, pe)
		}
	}
	p.assign, p.used, p.unplaced = map[string]*mpMachine{}, map[int]int{}, map[string][]string{}
	for _, cl := range p.clusters {
		a, u, left := mpPlan(cl.Machines, cl.People)
		for k, v := range a {
			p.assign[k] = v
		}
		for k, v := range u {
			p.used[k] = v
		}
		p.unplaced[cl.ID] = left
	}
	return p, nil
}

// mpPlan places each person's record on a machine of their cluster: their own property's machines first (in
// order), then, for whoever is left, the next machine of the cluster with space, going round from just after their
// property's machines. Machines are taken by property name, machine name and id; people earliest joiner first, so
// the plan is the same every time for the same data and a joiner never moves an earlier person. It sorts both
// lists in place and returns who is on which machine, how full each is, and who did not fit.
func mpPlan(ms []*mpMachine, people []mpPerson) (map[string]*mpMachine, map[int]int, []string) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].LocName != ms[j].LocName {
			return ms[i].LocName < ms[j].LocName
		}
		if ms[i].Name != ms[j].Name {
			return ms[i].Name < ms[j].Name
		}
		return ms[i].ID < ms[j].ID
	})
	sort.SliceStable(people, func(i, j int) bool {
		if people[i].DOJ != people[j].DOJ {
			return people[i].DOJ < people[j].DOJ
		}
		return people[i].ID < people[j].ID
	})
	assign, used := map[string]*mpMachine{}, map[int]int{}
	var rest []mpPerson
	for _, pe := range people {
		placed := false
		for _, m := range ms {
			if m.LocID == pe.LocID && used[m.ID] < m.Cap {
				assign[pe.ID], placed = m, true
				used[m.ID]++
				break
			}
		}
		if !placed {
			rest = append(rest, pe)
		}
	}
	var left []string
	for _, pe := range rest {
		start := 0
		for i, m := range ms {
			if m.LocID == pe.LocID {
				start = i + 1
			}
		}
		placed := false
		for k := 0; k < len(ms) && !placed; k++ {
			m := ms[(start+k)%len(ms)]
			if used[m.ID] < m.Cap {
				assign[pe.ID], placed = m, true
				used[m.ID]++
			}
		}
		if !placed {
			left = append(left, pe.ID)
		}
	}
	return assign, used, left
}

// mpLevel is how full a cluster is: none (no machines), ok, warn (from 90%), full (no free place).
func mpLevel(capacity, people int) string {
	switch {
	case capacity <= 0:
		return "none"
	case people >= capacity:
		return "full"
	case people*100 >= capacity*mpWarnPct:
		return "warn"
	}
	return "ok"
}

func mpPct(capacity, people int) int {
	if capacity <= 0 {
		return 0
	}
	return (people*100 + capacity/2) / capacity
}

func mpMachineLabel(m *mpMachine) string {
	if m == nil {
		return ""
	}
	if m.Serial != "" {
		return m.Name + " (" + m.Serial + ")"
	}
	return m.Name
}

// ---------- The capacity check (onboarding and transfers) ----------

// MachineCheck is how full a cluster would be with one more person.
type MachineCheck struct {
	ClusterID string `json:"clusterId"`
	Cluster   string `json:"cluster"`
	Capacity  int    `json:"capacity"`
	People    int    `json:"people"` // now, without the person checked
	After     int    `json:"after"`
	Free      int    `json:"free"` // after
	Pct       int    `json:"pct"`  // after
	Level     string `json:"level"`
	Message   string `json:"message,omitempty"`
}

// machineCheck works out the cluster of property locID with employee empID in it (counted once, whether or not they
// are there already). nil when the property is unknown.
func machineCheck(ctx context.Context, q store.Q, empID, locID string) (*MachineCheck, error) {
	loc, err := q.C("location").FindOne(ctx, bson.M{"_id": locID}, options.FindOne().SetProjection(bson.M{"cluster_id": 1}))
	if err != nil || loc == nil {
		return nil, err
	}
	clID := str(loc["cluster_id"])
	locIDs, err := column(ctx, q, "location", "id", bson.M{"cluster_id": clID})
	if err != nil {
		return nil, err
	}
	set, err := mpSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	devs, err := q.C("attendance_device").Find(ctx, bson.M{"location_id": bson.M{"$in": locIDs}, "status": "Active"}, options.Find().SetProjection(bson.M{"capacity": 1}))
	if err != nil {
		return nil, err
	}
	out := &MachineCheck{ClusterID: clID}
	for _, d := range devs {
		if d["capacity"] != nil {
			out.Capacity += timectxInt(d["capacity"])
		} else {
			out.Capacity += timectxInt(set["default_capacity"])
		}
	}
	n, err := q.C("employee").Count(ctx, bson.M{"status": "Active", "location_id": bson.M{"$in": locIDs}, "_id": bson.M{"$ne": empID}})
	if err != nil {
		return nil, err
	}
	if cl, _ := q.C("cluster").FindOne(ctx, bson.M{"_id": clID}, options.FindOne().SetProjection(bson.M{"name": 1})); cl != nil {
		out.Cluster = str(cl["name"])
	}
	out.People, out.After = int(n), int(n)+1
	out.Free, out.Pct = out.Capacity-out.After, mpPct(out.Capacity, out.After)
	switch {
	case out.Capacity <= 0:
		out.Level = "none"
	case out.After > out.Capacity:
		out.Level = "full"
		out.Message = fmt.Sprintf("%s is full: its punch machines hold %d people and %d already work there. Add a machine or remove leavers from the machines first", out.Cluster, out.Capacity, out.People)
	case out.After*100 >= out.Capacity*mpWarnPct:
		out.Level = "warn"
		out.Message = fmt.Sprintf("%s's punch machines will be %d%% full (%d of %d)", out.Cluster, out.Pct, out.After, out.Capacity)
	default:
		out.Level = "ok"
	}
	return out, nil
}

type mpOverrideKey struct{}

// machineOverrideReq marks a request that says to go ahead into a full cluster (?machineOverride=1 or the
// X-Machine-Override: 1 header). Called for every signed-in request (http.go).
func machineOverrideReq(r *http.Request) *http.Request {
	if r.URL.Query().Get("machineOverride") == "1" || r.Header.Get("X-Machine-Override") == "1" {
		return r.WithContext(context.WithValue(r.Context(), mpOverrideKey{}, true))
	}
	return r
}

func machineOverride(ctx context.Context) bool { v, _ := ctx.Value(mpOverrideKey{}).(bool); return v }

// machineGate refuses to put employee empID on property toLoc when that is in another cluster than fromLoc ("" for a
// joiner) and the cluster's machines are full, unless the request says to go ahead, which is audited. A cluster
// without machines is not checked.
func machineGate(ctx context.Context, q store.Q, c *Ctx, empID, fromLoc, toLoc string) error {
	if toLoc == "" || toLoc == fromLoc {
		return nil
	}
	chk, err := machineCheck(ctx, q, empID, toLoc)
	if err != nil || chk == nil || chk.Level != "full" {
		return err
	}
	if fromLoc != "" {
		if l, err := q.C("location").FindOne(ctx, bson.M{"_id": fromLoc}, options.FindOne().SetProjection(bson.M{"cluster_id": 1})); err != nil {
			return err
		} else if l != nil && str(l["cluster_id"]) == chk.ClusterID {
			return nil // a move inside the cluster
		}
	}
	if machineOverride(ctx) {
		return audit(ctx, q, c, fmt.Sprintf("Put into %s although its punch machines are full (%d of %d)", chk.Cluster, chk.After, chk.Capacity), "employee", empID, nil, chk)
	}
	return &apiError{Status: 409, Message: chk.Message + ".", Details: map[string]any{"kind": "cluster_full", "check": chk}}
}

// machineGateJoin is machineGate for a record just made at joining: its own property.
func machineGateJoin(ctx context.Context, q store.Q, c *Ctx, empID string) error {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": empID}, options.FindOne().SetProjection(bson.M{"location_id": 1}))
	if err != nil || e == nil {
		return err
	}
	return machineGate(ctx, q, c, empID, "", str(e["location_id"]))
}

// ---------- The to-do list ----------

type mpTodo struct {
	EmployeeID string `json:"employeeId"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // Enrol, Move, Remove
	Task       string `json:"task"`
	Location   string `json:"location,omitempty"`
	ToID       string `json:"toId,omitempty"`
	To         string `json:"to,omitempty"`
	Machine    string `json:"machine,omitempty"`
	FromID     string `json:"fromId,omitempty"`
	From       string `json:"from,omitempty"`
	Code       string `json:"code,omitempty"`
	Since      string `json:"since,omitempty"` // date of joining, or last working day
	Note       string `json:"note,omitempty"`  // the last portal check
}

var mpKindOrder = map[string]int{"Enrol": 0, "Move": 1, "Remove": 2}

// mpTodos is what has to be done on the machines: every active employee whose cluster is not the one they are
// recorded on, and every employee recorded on a cluster who is no longer active.
func mpTodos(ctx context.Context, q store.Q, p *mpPool) ([]mpTodo, error) {
	recs, err := q.C(store.MachineEnrolColl).Find(ctx, bson.M{"cluster_id": bson.M{"$ne": nil}}, options.Find().SetProjection(bson.M{"employee_id": 1, "cluster_id": 1}))
	if err != nil {
		return nil, err
	}
	rec := map[string]string{}
	var gone []string
	for _, r := range recs {
		id := str(r["employee_id"])
		rec[id] = str(r["cluster_id"])
		if _, ok := p.people[id]; !ok {
			gone = append(gone, id)
		}
	}
	notes := map[string]string{}
	if v, ok := p.settings["check"].(map[string]any); ok {
		if n, ok := v["notes"].(map[string]any); ok {
			for k, x := range n {
				notes[k] = str(x)
			}
		}
	}
	out := []mpTodo{}
	for _, pe := range p.people {
		want, have := p.locCluster[pe.LocID], rec[pe.ID]
		if want == "" || want == have {
			continue
		}
		t := mpTodo{EmployeeID: pe.ID, Name: pe.Name, Location: p.locName[pe.LocID], ToID: want, To: p.cname(want), Machine: mpMachineLabel(p.assign[pe.ID]),
			Code: pe.Code, Since: pe.DOJ, Note: notes[pe.ID]}
		where := "cluster " + t.To
		if t.Machine != "" {
			where += " (machine " + t.Machine + ")"
		} else if len(p.clusters[want].Machines) == 0 {
			where += " (no machine mapped to the cluster yet)"
		} else {
			where += " (the cluster's machines are full)"
		}
		if have == "" {
			t.Kind, t.Task = "Enrol", "Enrol in "+where
		} else {
			t.Kind, t.FromID, t.From = "Move", have, p.cname(have)
			t.Task = "Enrol in " + where + ", remove from cluster " + t.From
		}
		out = append(out, t)
	}
	if len(gone) > 0 {
		es, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": gone}}, options.Find().SetProjection(bson.M{"name": 1, "location_id": 1, "lwd": 1, "attendance_code": 1}))
		if err != nil {
			return nil, err
		}
		byID := map[string]store.Row{}
		for _, e := range es {
			byID[str(e["id"])] = e
		}
		for _, id := range gone {
			e := byID[id]
			t := mpTodo{EmployeeID: id, Name: id, Kind: "Remove", FromID: rec[id], From: p.cname(rec[id]), Note: notes[id]}
			if e != nil {
				t.Name, t.Location, t.Since, t.Code = str(e["name"]), p.locName[str(e["location_id"])], str(e["lwd"]), str(e["attendance_code"])
			}
			t.Task = "Remove from cluster " + t.From
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if mpKindOrder[a.Kind] != mpKindOrder[b.Kind] {
			return mpKindOrder[a.Kind] < mpKindOrder[b.Kind]
		}
		if a.To+a.From != b.To+b.From {
			return a.To+a.From < b.To+b.From
		}
		return a.Name+a.EmployeeID < b.Name+b.EmployeeID
	})
	return out, nil
}

// mpDone records a to-do as done: the person is now on the new cluster (enrol, move) or on none (remove).
func mpDone(ctx context.Context, q store.Q, t mpTodo, how, by string) error {
	now := time.Now()
	task := map[string]any{"kind": t.Kind, "task": t.Task, "from": timectxNullIf(t.From), "to": timectxNullIf(t.To), "machine": timectxNullIf(t.Machine),
		"at": now, "by": by, "how": how}
	var cl any
	if t.Kind != "Remove" {
		cl = t.ToID
	}
	_, err := q.C(store.MachineEnrolColl).Upsert(ctx, bson.M{"employee_id": t.EmployeeID},
		bson.M{"cluster_id": cl, "since": now, "how": how, "task": task, "task_at": now, "updated_at": now}, nil)
	return err
}

// ---------- Checking the to-dos against the portal's employee list ----------

// mpNormBranch is a portal branch name compared without case and extra spaces.
func mpNormBranch(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// machineVerify ticks the to-dos the portal's employee list shows done: the person's Attendance code is in a
// branch whose machines are mapped to the cluster they are to be enrolled on (enrol), and in none mapped to the
// cluster they are to be removed from (remove); a move needs both. It notes why the rest are not done yet.
func (s *Server) machineVerify(ctx context.Context, portal []realtime.Employee, by string) (map[string]any, error) {
	q := s.st.DB
	p, err := mpLoad(ctx, q)
	if err != nil {
		return nil, err
	}
	todos, err := mpTodos(ctx, q, p)
	if err != nil {
		return nil, err
	}
	branchCl := map[string]map[string]bool{}
	for _, m := range p.machines {
		if m.Branch != "" && m.ClusterID != "" {
			b := mpNormBranch(m.Branch)
			if branchCl[b] == nil {
				branchCl[b] = map[string]bool{}
			}
			branchCl[b][m.ClusterID] = true
		}
	}
	codeCl := map[string]map[string]bool{} // normalised code → clusters it is in
	unmapped := map[string]bool{}
	for _, e := range portal {
		cls, ok := branchCl[mpNormBranch(e.Branch)]
		if !ok && e.Branch != "" {
			unmapped[e.Branch] = true
		}
		for _, k := range []string{rtNorm(e.Code), rtNorm(e.Card)} {
			if k == "" {
				continue
			}
			if codeCl[k] == nil {
				codeCl[k] = map[string]bool{}
			}
			for c := range cls {
				codeCl[k][c] = true
			}
		}
	}
	done, notes := 0, map[string]any{}
	for _, t := range todos {
		code := rtNorm(t.Code)
		if code == "" {
			notes[t.EmployeeID] = "No Attendance code: tick it by hand"
			continue
		}
		in := codeCl[code]
		enrolled := t.Kind == "Remove" || in[t.ToID]
		removed := t.Kind == "Enrol" || !in[t.FromID]
		if enrolled && removed {
			if err := mpDone(ctx, q, t, "portal", by); err != nil {
				return nil, err
			}
			done++
			continue
		}
		switch {
		case !enrolled && !removed:
			notes[t.EmployeeID] = fmt.Sprintf("Code %s is not yet in a branch of %s and is still in %s", t.Code, t.To, t.From)
		case !enrolled:
			notes[t.EmployeeID] = fmt.Sprintf("Code %s is not yet in a branch of %s", t.Code, t.To)
		default:
			notes[t.EmployeeID] = fmt.Sprintf("Code %s is still in a branch of %s", t.Code, t.From)
		}
	}
	ub := make([]string, 0, len(unmapped))
	for b := range unmapped {
		ub = append(ub, b)
	}
	sort.Strings(ub)
	res := map[string]any{"at": time.Now(), "by": by, "portal": len(portal), "checked": len(todos), "done": done, "notes": notes, "unmappedBranches": ub}
	if err := mpSet(ctx, q, bson.M{"check": res}); err != nil {
		return nil, err
	}
	if done > 0 {
		_ = audit(ctx, q, nil, fmt.Sprintf("Punch machine to-dos checked against the portal's employee list: %d done", done), "company_setting", "1", nil, map[string]any{"done": done, "by": by})
	}
	return res, nil
}

// ---------- Suggested property for a machine ----------

var mpNoise = map[string]bool{"saltstayz": true, "salt": true, "stayz": true, "pause": true, "the": true, "hotel": true, "hotels": true, "cluster": true}

func mpTokens(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if !mpNoise[w] {
			out = append(out, w)
		}
	}
	return out
}

func mpSubset(a, b []string) bool {
	if len(a) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, w := range b {
		have[w] = true
	}
	for _, w := range a {
		if !have[w] {
			return false
		}
	}
	return true
}

// mpHint suggests the property of a machine whose portal branch or name matches exactly one active property's name
// (every word of one is in the other, "SaltStayz" and the like left out), or else the cluster whose name matches.
func mpHint(p *mpPool, m *mpMachine) map[string]any {
	keys := [][]string{mpTokens(m.Branch), mpTokens(m.Name)}
	for _, k := range keys {
		var hit []store.Row
		for _, l := range p.locs {
			if l["status"] != "Active" {
				continue
			}
			lt := mpTokens(str(l["name"]))
			if mpSubset(k, lt) || mpSubset(lt, k) {
				hit = append(hit, l)
			}
		}
		if len(hit) == 1 {
			id := str(hit[0]["id"])
			return map[string]any{"locationId": id, "location": p.locName[id], "cluster": p.cname(p.locCluster[id])}
		}
	}
	for _, k := range keys {
		var hit []string
		for _, id := range p.order {
			ct := mpTokens(p.clusters[id].Name)
			for i := 0; i < len(ct); i++ { // "Gurugram Cluster 1" → gurugram
				if ct[i] != "" && unicode.IsDigit(rune(ct[i][0])) {
					ct = append(ct[:i], ct[i+1:]...)
					i--
				}
			}
			if mpSubset(ct, k) {
				hit = append(hit, id)
			}
		}
		if len(hit) == 1 {
			return map[string]any{"clusterId": hit[0], "cluster": p.cname(hit[0])}
		}
	}
	return nil
}

// ---------- Handlers ----------

func (s *Server) machineRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/attendance/machines", s.a(s.machinesBoard))
	m.HandleFunc("GET /api/attendance/machines/todo", s.a(s.machinesTodo))
	m.HandleFunc("GET /api/attendance/machines/todo.xlsx", s.sheet(s.machinesTodoSheet))
	m.HandleFunc("GET /api/attendance/machines/plan", s.a(s.machinesPlan))
	m.HandleFunc("GET /api/attendance/machines/check", s.a(s.machinesCheck))
	m.HandleFunc("POST /api/attendance/machines/import", s.a(s.machinesImport))
	m.HandleFunc("POST /api/attendance/machines/settings", s.a(s.machinesSettings))
	m.HandleFunc("POST /api/attendance/machines/use-hints", s.a(s.machinesUseHints))
	m.HandleFunc("POST /api/attendance/machines/baseline", s.a(s.machinesBaseline))
	m.HandleFunc("POST /api/attendance/machines/todo/done", s.a(s.machinesTodoDone))
	m.HandleFunc("POST /api/attendance/machines/{id}/map", s.a(s.machinesMap))
}

func (s *Server) machinesBoard(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	p, err := mpLoad(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	todos, err := mpTodos(ctx, s.st.DB, p)
	if err != nil {
		return nil, err
	}
	open := map[string]int{}
	for _, t := range todos {
		open[t.ToID]++
		if t.FromID != "" {
			open[t.FromID]++
		}
	}
	clusters := []map[string]any{}
	for _, id := range p.order {
		cl := p.clusters[id]
		on, off, unk := 0, 0, 0
		for _, m := range cl.Machines {
			switch {
			case m.Online == nil:
				unk++
			case *m.Online:
				on++
			default:
				off++
			}
		}
		n := len(cl.People)
		noMachine := map[string]int{} // properties of the cluster without a machine: their people overflow
		has := map[string]bool{}
		for _, m := range cl.Machines {
			has[m.LocID] = true
		}
		for _, pe := range cl.People {
			if !has[pe.LocID] {
				noMachine[p.locName[pe.LocID]]++
			}
		}
		clusters = append(clusters, map[string]any{"id": id, "name": cl.Name, "city": cl.City, "capacity": cl.Cap, "people": n, "free": cl.Cap - n,
			"pct": mpPct(cl.Cap, n), "level": mpLevel(cl.Cap, n), "machines": len(cl.Machines), "online": on, "offline": off, "unknown": unk,
			"todo": open[id], "unplaced": len(p.unplaced[id]), "noMachine": noMachine})
	}
	machines := []map[string]any{}
	unmapped := 0
	for _, m := range p.machines {
		x := map[string]any{"id": m.ID, "name": m.Name, "serial": m.Serial, "model": m.Model, "mode": m.Mode, "branch": m.Branch, "vendor": m.Vendor,
			"status": map[bool]string{true: "Active", false: "Inactive"}[m.Active], "locationId": timectxNullIf(m.LocID), "location": timectxNullIf(m.LocName),
			"clusterId": timectxNullIf(m.ClusterID), "cluster": timectxNullIf(p.cname(m.ClusterID)), "capacity": m.Cap, "capacitySet": m.CapSet,
			"planned": p.used[m.ID], "online": m.Online, "portalStatus": timectxNullIf(m.PortalStatus), "gone": m.Gone, "seenAt": m.SeenAt}
		if m.LocID == "" {
			if m.Active {
				unmapped++
			}
			x["hint"] = mpHint(p, m)
		}
		machines = append(machines, x)
	}
	enrolled, err := s.st.DB.C(store.MachineEnrolColl).Count(ctx, bson.M{"cluster_id": bson.M{"$ne": nil}})
	if err != nil {
		return nil, err
	}
	_, envOK := rtEnv()
	return map[string]any{"envSet": envOK, "defaultCapacity": p.defCap, "warnPct": mpWarnPct, "clusters": clusters, "machines": machines,
		"unmapped": unmapped, "people": len(p.people), "enrolled": enrolled, "todo": len(todos), "lastImport": p.settings["import"], "lastCheck": mpCheckSummary(p.settings)}, nil
}

// mpCheckSummary is the last portal check without its per-person notes.
func mpCheckSummary(set bson.M) any {
	v, ok := set["check"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	for k, x := range v {
		if k != "notes" {
			out[k] = x
		}
	}
	return out
}

func (s *Server) machinesTodoList(ctx context.Context) ([]mpTodo, *mpPool, error) {
	p, err := mpLoad(ctx, s.st.DB)
	if err != nil {
		return nil, nil, err
	}
	t, err := mpTodos(ctx, s.st.DB, p)
	return t, p, err
}

func (s *Server) machinesTodo(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	todos, p, err := s.machinesTodoList(ctx)
	if err != nil {
		return nil, err
	}
	if k := r.URL.Query().Get("kind"); k != "" && k != "All" {
		f := []mpTodo{}
		for _, t := range todos {
			if t.Kind == k {
				f = append(f, t)
			}
		}
		todos = f
	}
	recs, err := s.st.DB.C(store.MachineEnrolColl).Find(ctx, bson.M{"task_at": bson.M{"$gte": time.Now().AddDate(0, 0, -mpDoneDays)}},
		options.Find().SetSort(bson.D{{Key: "task_at", Value: -1}}).SetLimit(300))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, x := range recs {
		ids = append(ids, str(x["employee_id"]))
	}
	names, err := timectxByID(ctx, s.st.DB, "employee", ids, "name")
	if err != nil {
		return nil, err
	}
	done := []map[string]any{}
	for _, x := range recs {
		t, _ := x["task"].(map[string]any)
		if t == nil {
			continue
		}
		id := str(x["employee_id"])
		done = append(done, map[string]any{"employeeId": id, "name": str(names[id]["name"]), "kind": t["kind"], "task": t["task"], "by": t["by"], "how": t["how"], "at": x["task_at"]})
	}
	return map[string]any{"open": todos, "done": done, "lastCheck": mpCheckSummary(p.settings)}, nil
}

func (s *Server) machinesTodoSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return "", nil, nil, err
	}
	todos, _, err := s.machinesTodoList(r.Context())
	if err != nil {
		return "", nil, nil, err
	}
	rows := [][]any{}
	for _, t := range todos {
		rows = append(rows, []any{t.Kind, t.EmployeeID, t.Name, t.Code, t.Location, t.To, t.Machine, t.From, t.Since, t.Task, t.Note})
	}
	return "punch-machine-todo", []string{"Task", "Employee ID", "Name", "Attendance code", "Property", "Enrol in cluster", "Machine", "Remove from cluster",
		"Joined / last day", "What to do", "Last portal check"}, rows, nil
}

func (s *Server) machinesPlan(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	p, err := mpLoad(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	cl := p.clusters[r.URL.Query().Get("clusterId")]
	if cl == nil {
		return nil, notFound("Choose a cluster")
	}
	ms := []map[string]any{}
	for _, m := range cl.Machines {
		ms = append(ms, map[string]any{"id": m.ID, "name": m.Name, "serial": m.Serial, "location": m.LocName, "capacity": m.Cap, "planned": p.used[m.ID]})
	}
	people := []map[string]any{}
	for _, pe := range cl.People {
		m := p.assign[pe.ID]
		x := map[string]any{"employeeId": pe.ID, "name": pe.Name, "location": p.locName[pe.LocID], "doj": pe.DOJ, "machine": nil, "overflow": false}
		if m != nil {
			x["machine"], x["overflow"] = mpMachineLabel(m), m.LocID != pe.LocID
		}
		people = append(people, x)
	}
	return map[string]any{"cluster": cl.Name, "capacity": cl.Cap, "people": people, "machines": ms, "unplaced": len(p.unplaced[cl.ID])}, nil
}

func (s *Server) machinesCheck(r *http.Request, c *Ctx) (any, error) {
	if c.Level("att.setup") < 1 && c.Level("emp.directory") < 1 && c.Level("onb.pipeline") < 1 {
		return nil, forbidden("")
	}
	q := r.URL.Query()
	chk, err := machineCheck(r.Context(), s.st.DB, q.Get("employeeId"), q.Get("locationId"))
	if err != nil {
		return nil, err
	}
	if chk == nil {
		return nil, notFound("No such property")
	}
	return chk, nil
}

// machinesImport reads the portal's machine list and creates or updates a device per serial number.
func (s *Server) machinesImport(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	cfg, ok := rtEnv()
	if !ok {
		return nil, bad("Add REALTIME_URL, REALTIME_USER and REALTIME_PASS to the server's environment first")
	}
	ctx := r.Context()
	list, err := mpFetchMachines(ctx, cfg)
	if err != nil {
		return nil, bad("The portal's machine list could not be read: %v", err)
	}
	if len(list) == 0 {
		return nil, bad("The portal's machine list came back empty")
	}
	now := time.Now()
	added, updated := 0, 0
	err = s.st.Tx(ctx, func(tx store.Q) error {
		added, updated = 0, 0
		devs, err := tx.C("attendance_device").Find(ctx, nil, options.Find().SetProjection(bson.M{"serial": 1, "vendor": 1}))
		if err != nil {
			return err
		}
		bySerial := map[string]int{}
		for _, d := range devs {
			if k := strings.ToUpper(strings.TrimSpace(str(d["serial"]))); k != "" {
				bySerial[k] = timectxInt(d["id"])
			}
		}
		seen := map[int]bool{}
		for _, m := range list {
			set := bson.M{"model": timectxNullIf(m.Model), "machine_no": timectxNullIf(m.No), "io_mode": timectxNullIf(m.Mode), "portal_branch": timectxNullIf(m.Branch),
				"portal_status": timectxNullIf(m.Status), "portal_seen_at": now, "portal_gone": false}
			if m.Online != nil {
				set["online"] = *m.Online
			} else {
				set["online"] = nil
			}
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = m.Serial
			}
			if id, ok := bySerial[strings.ToUpper(m.Serial)]; ok {
				if seen[id] {
					continue
				}
				seen[id] = true
				set["name"] = name
				if _, err := tx.C("attendance_device").Update(ctx, bson.M{"_id": id}, bson.M{"$set": set}); err != nil {
					return err
				}
				updated++
				continue
			}
			key := randomHex(24) // never shown: a portal machine sends its punches to the portal, not here
			set["name"], set["vendor"], set["serial"], set["key_hash"], set["key_hint"], set["created_by"] = name, "Realtime", m.Serial, hashKey(key), key[len(key)-4:], c.Name
			v, err := tx.C("attendance_device").Insert(ctx, set)
			if err != nil {
				return err
			}
			seen[timectxInt(v)] = true
			added++
		}
		// Portal machines the list no longer shows.
		ids := []int{}
		for id := range seen {
			ids = append(ids, id)
		}
		if _, err := tx.C("attendance_device").UpdateMany(ctx, bson.M{"vendor": "Realtime", "portal_seen_at": bson.M{"$ne": nil}, "_id": bson.M{"$nin": ids}},
			bson.M{"$set": bson.M{"portal_gone": true}}); err != nil {
			return err
		}
		res := map[string]any{"at": now, "by": c.Name, "machines": len(list), "added": added, "updated": updated}
		if err := mpSet(ctx, tx, bson.M{"import": res}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Punch machines imported from the portal: %d new, %d updated", added, updated), "attendance_device", "", nil, res)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"machines": len(list), "added": added, "updated": updated}, nil
}

func (s *Server) machinesSettings(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		DefaultCapacity int `json:"defaultCapacity"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.DefaultCapacity < 1 || b.DefaultCapacity > mpMaxCap {
		return nil, bad("A machine holds from 1 to %d people", mpMaxCap)
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if err := mpSet(ctx, tx, bson.M{"default_capacity": b.DefaultCapacity}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Punch machines hold %d people each by default", b.DefaultCapacity), "company_setting", "1", nil, b)
	})
}

// machinesMap maps a machine to a property and sets its capacity (null: the default).
func (s *Server) machinesMap(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		LocationID *string `json:"locationId"`
		Capacity   *int    `json:"capacity"`
		Default    bool    `json:"defaultCapacity"` // true: use the default capacity
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), devicesID(r.PathValue("id"))
	set := bson.M{}
	if b.LocationID != nil {
		if *b.LocationID == "" {
			set["location_id"] = nil
		} else {
			ok, err := s.st.DB.C("location").Exists(ctx, bson.M{"_id": *b.LocationID, "status": "Active"})
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, bad("Choose an active property")
			}
			set["location_id"] = *b.LocationID
		}
	}
	switch {
	case b.Default:
		set["capacity"] = nil
	case b.Capacity != nil:
		if *b.Capacity < 0 || *b.Capacity > mpMaxCap {
			return nil, bad("A machine holds from 0 to %d people", mpMaxCap)
		}
		set["capacity"] = *b.Capacity
	}
	if len(set) == 0 {
		return nil, bad("Nothing to change")
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		before, err := tx.C("attendance_device").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1, "location_id": 1, "capacity": 1}))
		if err != nil || before == nil {
			return orNotFound(err)
		}
		if _, err := tx.C("attendance_device").Update(ctx, bson.M{"_id": id}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Punch machine mapped: "+str(before["name"]), "attendance_device", fmt.Sprint(id),
			map[string]any{"locationId": before["location_id"], "capacity": before["capacity"]}, set)
	})
}

// machinesUseHints maps every active machine without a property to its suggested property.
func (s *Server) machinesUseHints(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	n := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n = 0
		p, err := mpLoad(ctx, tx)
		if err != nil {
			return err
		}
		done := []map[string]any{}
		for _, m := range p.machines {
			if m.LocID != "" || !m.Active {
				continue
			}
			h := mpHint(p, m)
			if h == nil || h["locationId"] == nil {
				continue
			}
			if _, err := tx.C("attendance_device").Update(ctx, bson.M{"_id": m.ID, "location_id": nil}, bson.M{"$set": bson.M{"location_id": h["locationId"]}}); err != nil {
				return err
			}
			done = append(done, map[string]any{"machine": m.Name, "serial": m.Serial, "location": h["location"]})
			n++
		}
		if n == 0 {
			return nil
		}
		return audit(ctx, tx, c, fmt.Sprintf("%d punch machines mapped to their suggested property", n), "attendance_device", "", nil, done)
	})
	return map[string]any{"mapped": n}, err
}

// machinesBaseline records every active employee not yet recorded as enrolled on their property's cluster (they
// punch today), so that the to-do list starts from now.
func (s *Server) machinesBaseline(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	n := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		if n, err = store.MachineBaseline(ctx, tx, time.Now()); err != nil || n == 0 {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("%d active employees recorded as enrolled on their cluster's punch machines", n), "machine_enrol", "", nil, nil)
	})
	return map[string]any{"recorded": n}, err
}

// machinesTodoDone ticks to-dos done by hand.
func (s *Server) machinesTodoDone(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeIDs []string `json:"employeeIds"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.EmployeeIDs) == 0 {
		return nil, bad("Tick at least one task")
	}
	ctx := r.Context()
	n := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n = 0
		p, err := mpLoad(ctx, tx)
		if err != nil {
			return err
		}
		todos, err := mpTodos(ctx, tx, p)
		if err != nil {
			return err
		}
		by := map[string]mpTodo{}
		for _, t := range todos {
			by[t.EmployeeID] = t
		}
		done := []string{}
		for _, id := range b.EmployeeIDs {
			t, ok := by[id]
			if !ok {
				continue // done already, or nothing to do
			}
			if err := mpDone(ctx, tx, t, "hand", c.Name); err != nil {
				return err
			}
			done = append(done, t.EmployeeID+": "+t.Task)
			n++
		}
		if n == 0 {
			return bad("Nothing to tick: these tasks are done already")
		}
		return audit(ctx, tx, c, fmt.Sprintf("%d punch machine to-dos ticked done", n), "machine_enrol", "", nil, done)
	})
	return map[string]any{"done": n}, err
}

// machineVerifyAfterFetch checks the to-dos when the portal's employee list has been fetched (realtime_match.go).
// A failure is logged; it never stops the matching.
func (s *Server) machineVerifyAfterFetch(ctx context.Context, portal []realtime.Employee, by string) {
	if _, err := s.machineVerify(ctx, portal, by); err != nil {
		log.Printf("punch machine to-dos: portal check: %v", err)
	}
}
