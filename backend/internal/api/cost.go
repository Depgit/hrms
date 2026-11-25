package api

// The cost and budget console (workforce plan, module 6; decisions of 17 Nov 2026). One page that answers "what do
// we spend" for the whole company or any state, city, cluster or property, with a drill-down to each person. The
// company has no costs besides salary and accommodation today, so there is no expense ledger.
//
//   - Lines: salary paid (earnings: fixed, variable, incentives, arrears and claims; the bonus is shown with the
//     company contributions; the meal and accommodation deductions of employee_pay are left out: HR raises the
//     gross by them and deducts them back to meet the minimum wage, so they are no company cost — owner, 17 Nov), company contributions (employer PF, EPFO admin and EDLI, ESI, LWF, gratuity, bonus,
//     on the all-inclusive gross only admin, gratuity, PL encashment, bonus inside G and medical: allin.go,
//     medical insurance) and accommodation (each person's share, plus the empty-unit cost on the property the unit
//     serves; hst.go HstMonthCost). Total cost, cost per head, budget, difference and % used, red when over.
//   - Actual or projected: an approved or published month reads its payroll rows; the open month (and later ones)
//     is projected from the current salary structures and headcount (a full month for everyone paid in it, with the
//     pay lines already entered), labelled "Projected". An earlier month with no approved run shows nothing.
//   - Frozen: when a month is published its figures are written to cost_snapshot (people, empty-unit cost, place
//     names and budgets as they were), and the page for that month reads the snapshot from then on. A month
//     published before this build is frozen by the maintenance sweep.
//   - Budgets: per property the existing location fields (payroll_budget is the monthly cost budget,
//     headcount_budget the seats); per cluster its own figures when set, else the total of its properties.
//     Cities, states and the company add up their clusters.
//   - Headcount plan: per property the seats against people in post (active, Joining IDs included), offers in flight
//     (accepted and not joined yet, and pre-boarding records) and offers still in approval. The offer budget check
//     (onboarding.go budgetOver) counts the same numbers (seatsUsed).
//
// Rights: cost.view (View) to see the page, cost.budget (Edit) to change budgets; HR Admin has both. The page is
// limited to the viewer's properties (a role mapped to properties, clusters, cities or states sees those only).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// routesCost registers the cost console routes (server.go calls it).
func (s *Server) routesCost(m *http.ServeMux) {
	m.HandleFunc("GET /api/cost", s.a(s.costGet))
	m.HandleFunc("GET /api/cost/people", s.a(s.costPeople))
	m.HandleFunc("GET /api/cost/trend", s.a(s.costTrend))
	m.HandleFunc("GET /api/cost/headcount", s.a(s.costHeadcount))
	m.HandleFunc("GET /api/cost/export.xlsx", s.sheet(s.costSheet))
	m.HandleFunc("PUT /api/cost/budget/{kind}/{id}", s.a(s.costBudgetSave))
}

// ---------- The month's figures ----------

// costLine is one person's cost in a month.
type costLine struct {
	EmployeeID string  `json:"employeeId"`
	Name       string  `json:"name"`
	LocID      string  `json:"locId"`
	Desig      string  `json:"desig,omitempty"`
	Type       string  `json:"type,omitempty"`
	Paid       bool    `json:"paid"`            // on the payroll (false: an accommodation share only)
	Salary     float64 `json:"salary"`          // earnings less the bonus, the minimum-wage top-ups and the Reimbursement
	Reimb      float64 `json:"reimb,omitempty"` // the Reimbursement, outside the gross (reimb.go; 25 Nov 2026)
	TopUp      float64 `json:"topUp"`           // the meal and accommodation deductions: a minimum-wage top-up, not company cost
	PF         float64 `json:"pf"`              // employer PF
	Admin      float64 `json:"admin"`           // EPFO admin and EDLI
	ESI        float64 `json:"esi"`
	LWF        float64 `json:"lwf"`
	Grat       float64 `json:"grat"`
	PlEnc      float64 `json:"plEnc,omitempty"` // the PL encashment provision: a company cost on the all-inclusive gross (allin.go)
	Bonus      float64 `json:"bonus"`
	Med        float64 `json:"med"`
	Accom      float64 `json:"accom"` // the person's share of accommodation
}

func (l costLine) contrib() float64 {
	return l.PF + l.Admin + l.ESI + l.LWF + l.Grat + l.PlEnc + l.Bonus + l.Med
}

// costPlace is a property with its cluster, city and state, and its budgets.
type costPlace struct {
	Name      string  `json:"name"`
	ClusterID string  `json:"clusterId"`
	Cluster   string  `json:"cluster"`
	CityID    string  `json:"cityId"`
	City      string  `json:"city"`
	StateID   string  `json:"stateId"`
	State     string  `json:"state"`
	Active    bool    `json:"active"`
	HC        int     `json:"hc"`   // headcount budget (0 = none)
	Cost      float64 `json:"cost"` // monthly cost budget (0 = none)
}

// costClusterBudget is a cluster's own budgets (nil = the total of its properties).
type costClusterBudget struct {
	HC   *int     `json:"hc"`
	Cost *float64 `json:"cost"`
}

// costBook is a month's figures: what a snapshot freezes.
type costBook struct {
	YM       string                       `json:"ym"`
	Mode     string                       `json:"mode"` // actual | projected | none
	People   []costLine                   `json:"people"`
	Empty    map[string]float64           `json:"empty"` // property → empty-unit cost ("" = a unit serving no property)
	Places   map[string]costPlace         `json:"places"`
	Clusters map[string]costClusterBudget `json:"clusters"`
}

// costPlaces loads every property with its place names and budgets, and the clusters' own budgets.
func costPlaces(ctx context.Context, q store.Q) (map[string]costPlace, map[string]costClusterBudget, error) {
	names := func(coll string) (map[string]store.Row, error) {
		rows, err := q.C(coll).Find(ctx, bson.M{})
		if err != nil {
			return nil, err
		}
		out := map[string]store.Row{}
		for _, r := range rows {
			out[str(r["id"])] = r
		}
		return out, nil
	}
	states, err := names("state")
	if err != nil {
		return nil, nil, err
	}
	cities, err := names("city")
	if err != nil {
		return nil, nil, err
	}
	clusters, err := names("cluster")
	if err != nil {
		return nil, nil, err
	}
	locs, err := q.C("location").Find(ctx, bson.M{})
	if err != nil {
		return nil, nil, err
	}
	places := map[string]costPlace{}
	for _, l := range locs {
		cl := clusters[str(l["cluster_id"])]
		ct := cities[str(cl["city_id"])]
		stt := states[str(ct["state_id"])]
		places[str(l["id"])] = costPlace{Name: str(l["name"]), ClusterID: str(l["cluster_id"]), Cluster: str(cl["name"]), CityID: str(cl["city_id"]), City: str(ct["name"]),
			StateID: str(ct["state_id"]), State: str(stt["name"]), Active: l["status"] == "Active", HC: toInt(l["headcount_budget"]), Cost: payengineFloat(l["payroll_budget"])}
	}
	cb := map[string]costClusterBudget{}
	for id, cl := range clusters {
		var b costClusterBudget
		if cl["headcount_budget"] != nil {
			n := toInt(cl["headcount_budget"])
			b.HC = &n
		}
		b.Cost = payengineFloatPtr(cl["cost_budget"])
		if b.HC != nil || b.Cost != nil {
			cb[id] = b
		}
	}
	return places, cb, nil
}

// costLineOf is the cost of one pay row. On the all-inclusive gross (allin.go) the employer's PF, ESI, EDLI and LWF
// are deducted from the employee (inside the salary paid, still remitted to the authorities), so the company's own
// contributions are the admin charges, gratuity, the PL encashment provision and medical insurance.
func costLineOf(o *PayRowOut) costLine {
	r := o.R
	if r.AllIn {
		return costLine{EmployeeID: o.EmployeeID, Name: o.Name, LocID: o.LocID, Desig: o.Desig, Type: o.Type, Paid: true,
			Salary: o.TotalEarn - r.Bonus - r.Meal - r.Accom - r.Reimb, Reimb: r.Reimb, TopUp: r.Meal + r.Accom, Admin: r.Admin, Grat: r.Grat, PlEnc: r.PlEnc, Bonus: r.Bonus, Med: r.Med}
	}
	return costLine{EmployeeID: o.EmployeeID, Name: o.Name, LocID: o.LocID, Desig: o.Desig, Type: o.Type, Paid: true,
		Salary: o.TotalEarn - r.Bonus - r.Meal - r.Accom - r.Reimb, Reimb: r.Reimb, TopUp: r.Meal + r.Accom, PF: r.ErPF, Admin: r.Admin + r.Edli, ESI: r.ErESI, LWF: r.LwfEr, Grat: r.Grat, Bonus: r.Bonus, Med: r.Med}
}

// costNewBook is a book for month ym with the places loaded.
func costNewBook(ctx context.Context, q store.Q, ym, mode string) (*costBook, error) {
	places, cb, err := costPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	return &costBook{YM: ym, Mode: mode, People: []costLine{}, Empty: map[string]float64{}, Places: places, Clusters: cb}, nil
}

// costAddAccom adds the month's accommodation split: each share on the person's line (a person off the payroll gets a
// line of their own, at their property) and the empty-unit cost per property.
func costAddAccom(ctx context.Context, q store.Q, b *costBook) error {
	m, err := HstMonthCost(ctx, q, b.YM)
	if err != nil {
		return err
	}
	at := map[string]int{}
	for i, l := range b.People {
		at[l.EmployeeID] = i
	}
	missing := []string{}
	for id, share := range m.Shares {
		if i, ok := at[id]; ok {
			b.People[i].Accom += share
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		rows, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": missing}}, options.Find().SetProjection(compFields("name", "location_id", "type")))
		if err != nil {
			return err
		}
		found := map[string]store.Row{}
		for _, r := range rows {
			found[str(r["id"])] = r
		}
		for _, id := range missing {
			e := found[id]
			b.People = append(b.People, costLine{EmployeeID: id, Name: str(e["name"]), LocID: str(e["location_id"]), Type: str(e["type"]), Accom: m.Shares[id]})
		}
	}
	for loc, v := range m.Empty {
		b.Empty[loc] += v
	}
	sort.SliceStable(b.People, func(i, j int) bool { return b.People[i].EmployeeID < b.People[j].EmployeeID })
	return nil
}

// costActualBook is an approved or published month from its payroll rows.
func costActualBook(ctx context.Context, q store.Q, ym string) (*costBook, error) {
	b, err := costNewBook(ctx, q, ym, "actual")
	if err != nil {
		return nil, err
	}
	rows, err := loadRows(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	for _, o := range rows {
		b.People = append(b.People, costLineOf(o))
	}
	return b, costAddAccom(ctx, q, b)
}

// costProjectedBook projects month ym from the current salary structures and headcount: everyone paid in the month,
// for the full month, with the pay lines already entered for it (incentives, arrears, claims).
func costProjectedBook(ctx context.Context, q store.Q, ym string) (*costBook, error) {
	b, err := costNewBook(ctx, q, ym, "projected")
	if err != nil {
		return nil, err
	}
	eligible, err := payEligible(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, q, ym, false, eligible, nil)
	if err != nil {
		return nil, err
	}
	for _, e := range list {
		b.People = append(b.People, costLineOf(pc.row(e)))
	}
	return b, costAddAccom(ctx, q, b)
}

// costMeta is how a month's figures were made.
type costMeta struct {
	YM       string     `json:"ym"`
	OpenYm   string     `json:"openYm"`
	Mode     string     `json:"mode"`   // actual | projected | none
	Status   string     `json:"status"` // the payroll month's status
	Frozen   bool       `json:"frozen"`
	FrozenAt *time.Time `json:"frozenAt,omitempty"`
	FrozenBy string     `json:"frozenBy,omitempty"`
}

// costBookFor is month ym's figures: the snapshot when frozen, the payroll rows when approved or published, a
// projection for the open month and later, nothing for an earlier month without an approved run.
func costBookFor(ctx context.Context, q store.Q, ym string) (*costBook, costMeta, error) {
	meta := costMeta{YM: ym, OpenYm: openYm(ctx, q)}
	pm, err := loadPayMonth(ctx, q, ym, false)
	if err != nil {
		return nil, meta, err
	}
	meta.Status = pm.Status
	snap, err := q.C(store.CostSnapshotColl).FindOne(ctx, bson.M{"ym": ym})
	if err != nil {
		return nil, meta, err
	}
	if snap != nil {
		b := &costBook{}
		if err := payengineJSON(snap["data"], b); err != nil {
			return nil, meta, err
		}
		if t, ok := snap["frozen_at"].(time.Time); ok {
			meta.FrozenAt = &t
		}
		meta.Mode, meta.Frozen, meta.FrozenBy = "actual", true, str(snap["frozen_by"])
		return b, meta, nil
	}
	var b *costBook
	switch {
	case pm.Status == "Approved" || pm.Status == "Published":
		b, err = costActualBook(ctx, q, ym)
	case ym >= meta.OpenYm:
		b, err = costProjectedBook(ctx, q, ym)
	default:
		b, err = costNewBook(ctx, q, ym, "none")
	}
	if err != nil {
		return nil, meta, err
	}
	meta.Mode = b.Mode
	return b, meta, nil
}

// costFreeze writes month ym's snapshot from its payroll rows, once: a month already frozen stays as it is. It
// reports whether it wrote one.
func costFreeze(ctx context.Context, q store.Q, ym, by string) (bool, error) {
	if ok, err := q.C(store.CostSnapshotColl).Exists(ctx, bson.M{"ym": ym}); err != nil || ok {
		return false, err
	}
	b, err := costActualBook(ctx, q, ym)
	if err != nil {
		return false, err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	if _, err := q.C(store.CostSnapshotColl).Insert(ctx, bson.M{"ym": ym, "frozen_by": by, "frozen_at": q.Now(), "data": data}); err != nil {
		return false, err
	}
	return true, nil
}

// CostFreezeSweep freezes every published month that has no snapshot yet (months published before the console, or
// a freeze that failed at publishing). It returns how many it froze.
func CostFreezeSweep(ctx context.Context, q store.Q) (int, error) {
	months, err := q.C("payroll_month").Distinct(ctx, "ym", bson.M{"status": "Published"})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range months {
		ok, err := costFreeze(ctx, q, str(m), "System")
		if err != nil {
			return n, fmt.Errorf("%s: %w", str(m), err)
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// costFreezeLogged freezes a month just published, after its transaction: a failure is logged and the maintenance
// sweep tries again (publishing payroll never waits on the cost console).
func (s *Server) costFreezeLogged(ctx context.Context, ym, by string) {
	if _, err := costFreeze(ctx, s.st.DB, ym, by); err != nil {
		log.Printf("cost snapshot %s: %v", ym, err)
	}
}

// costSweepLogged runs CostFreezeSweep from the maintenance job.
func (s *Server) costSweepLogged(ctx context.Context) {
	if _, err := CostFreezeSweep(ctx, s.st.DB); err != nil {
		log.Printf("cost snapshots: %v", err)
	}
}

// ---------- The tree ----------

// costNode is one row of the console: the company, a state, city, cluster or property.
type costNode struct {
	Kind       string      `json:"kind"` // company | state | city | cluster | property
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Active     bool        `json:"active"`
	People     int         `json:"people"`
	Salary     float64     `json:"salary"`
	TopUp      float64     `json:"topUp"` // minimum-wage top-ups left out of the salary
	Reimb      float64     `json:"reimb"` // the Reimbursement, outside the gross
	PF         float64     `json:"pf"`
	Admin      float64     `json:"admin"`
	ESI        float64     `json:"esi"`
	LWF        float64     `json:"lwf"`
	Grat       float64     `json:"grat"`
	PlEnc      float64     `json:"plEnc,omitempty"` // the PL encashment provision (all-inclusive gross only)
	Bonus      float64     `json:"bonus"`
	Med        float64     `json:"med"`
	Contrib    float64     `json:"contrib"`
	AccomShare float64     `json:"accomShare"`
	AccomEmpty float64     `json:"accomEmpty"`
	Accom      float64     `json:"accom"`
	Total      float64     `json:"total"`
	PerHead    float64     `json:"perHead"`
	Budget     float64     `json:"budget"`
	BudgetOwn  *float64    `json:"budgetOwn,omitempty"` // a cluster's own cost budget
	HCBudget   int         `json:"hcBudget"`
	HCOwn      *int        `json:"hcOwn,omitempty"` // a cluster's own headcount budget
	Diff       float64     `json:"diff"`            // budget − total (negative when over)
	Pct        float64     `json:"pct"`             // % of the budget used
	Over       bool        `json:"over"`
	Children   []*costNode `json:"children,omitempty"`
}

func (n *costNode) addLine(l costLine) {
	if l.Paid {
		n.People++
	}
	n.Salary += l.Salary
	n.Reimb += l.Reimb
	n.TopUp += l.TopUp
	n.PF += l.PF
	n.Admin += l.Admin
	n.ESI += l.ESI
	n.LWF += l.LWF
	n.Grat += l.Grat
	n.PlEnc += l.PlEnc
	n.Bonus += l.Bonus
	n.Med += l.Med
	n.AccomShare += l.Accom
}

func (n *costNode) addNode(c *costNode) {
	n.People += c.People
	n.Salary += c.Salary
	n.Reimb += c.Reimb
	n.TopUp += c.TopUp
	n.PF += c.PF
	n.Admin += c.Admin
	n.ESI += c.ESI
	n.LWF += c.LWF
	n.Grat += c.Grat
	n.PlEnc += c.PlEnc
	n.Bonus += c.Bonus
	n.Med += c.Med
	n.AccomShare += c.AccomShare
	n.AccomEmpty += c.AccomEmpty
	n.Budget += c.Budget
	n.HCBudget += c.HCBudget
}

// finish works out the totals, rounds the figures and orders the children by name (the company's unplaced row last).
func (n *costNode) finish() {
	r := calc.Round2
	n.Contrib = r(n.PF + n.Admin + n.ESI + n.LWF + n.Grat + n.PlEnc + n.Bonus + n.Med)
	n.Accom = r(n.AccomShare + n.AccomEmpty)
	n.Total = r(n.Salary + n.Reimb + n.Contrib + n.Accom)
	for _, p := range []*float64{&n.Salary, &n.Reimb, &n.TopUp, &n.PF, &n.Admin, &n.ESI, &n.LWF, &n.Grat, &n.PlEnc, &n.Bonus, &n.Med, &n.AccomShare, &n.AccomEmpty, &n.Budget} {
		*p = r(*p)
	}
	if n.People > 0 {
		n.PerHead = r(n.Total / float64(n.People))
	}
	n.Diff, n.Pct, n.Over = 0, 0, false
	if n.Budget > 0 {
		n.Diff = r(n.Budget - n.Total)
		n.Pct = math.Round(n.Total/n.Budget*1000) / 10
		n.Over = n.Total > n.Budget
	}
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if (a.ID == "") != (b.ID == "") {
			return b.ID == ""
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// costInScope reports whether the viewer's data scope covers a property ("" = cost tied to no property: the whole
// company's view only).
func costInScope(c *Ctx, loc string) bool {
	return c.Scope.Access == "all" || (loc != "" && c.Scope.Access == "locs" && containsStr(c.Scope.Locations, loc))
}

// costCan checks the right and that the viewer's role covers properties.
func costCan(c *Ctx) error {
	if err := c.Need("cost.view", 1); err != nil {
		return err
	}
	if c.Scope.Access != "all" && c.Scope.Access != "locs" {
		return forbidden("The cost console needs a role view with access to properties")
	}
	return nil
}

// costTree builds the console's rows for the viewer: company › state › city › cluster › property, every property
// of the viewer's scope that is active or has a cost in the month.
func costTree(b *costBook, c *Ctx) *costNode {
	props := map[string]*costNode{}
	prop := func(loc string) *costNode {
		if _, ok := b.Places[loc]; !ok {
			loc = ""
		}
		if !costInScope(c, loc) {
			return nil
		}
		n := props[loc]
		if n == nil {
			pl := b.Places[loc]
			n = &costNode{Kind: "property", ID: loc, Name: pl.Name, Active: pl.Active, Budget: pl.Cost, HCBudget: pl.HC}
			if loc == "" {
				n.Name, n.Active = "Not tied to a property", true
			}
			props[loc] = n
		}
		return n
	}
	for _, l := range b.People {
		if n := prop(l.LocID); n != nil {
			n.addLine(l)
		}
	}
	for loc, v := range b.Empty {
		if n := prop(loc); n != nil {
			n.AccomEmpty += v
		}
	}
	for loc, pl := range b.Places {
		if pl.Active {
			prop(loc)
		}
	}
	company := &costNode{Kind: "company", Name: "Company", Active: true}
	clusters, cities, states := map[string]*costNode{}, map[string]*costNode{}, map[string]*costNode{}
	for loc, n := range props {
		n.finish()
		if loc == "" {
			company.Children = append(company.Children, n)
			continue
		}
		pl := b.Places[loc]
		st := states[pl.StateID]
		if st == nil {
			st = &costNode{Kind: "state", ID: pl.StateID, Name: pl.State, Active: true}
			states[pl.StateID] = st
			company.Children = append(company.Children, st)
		}
		ct := cities[pl.CityID]
		if ct == nil {
			ct = &costNode{Kind: "city", ID: pl.CityID, Name: pl.City, Active: true}
			cities[pl.CityID] = ct
			st.Children = append(st.Children, ct)
		}
		cl := clusters[pl.ClusterID]
		if cl == nil {
			cl = &costNode{Kind: "cluster", ID: pl.ClusterID, Name: pl.Cluster, Active: true}
			clusters[pl.ClusterID] = cl
			ct.Children = append(ct.Children, cl)
		}
		cl.Children = append(cl.Children, n)
	}
	for id, cl := range clusters {
		for _, p := range cl.Children {
			cl.addNode(p)
		}
		own := b.Clusters[id]
		if own.Cost != nil && *own.Cost > 0 {
			cl.Budget, cl.BudgetOwn = *own.Cost, own.Cost
		}
		if own.HC != nil && *own.HC > 0 {
			cl.HCBudget, cl.HCOwn = *own.HC, own.HC
		}
		cl.finish()
	}
	for _, ct := range cities {
		for _, x := range ct.Children {
			ct.addNode(x)
		}
		ct.finish()
	}
	for _, st := range states {
		for _, x := range st.Children {
			st.addNode(x)
		}
		st.finish()
	}
	for _, x := range company.Children {
		company.addNode(x)
	}
	company.finish()
	return company
}

// costFind is the node of kind and id in the tree (the company for kind "company" or "").
func costFind(n *costNode, kind, id string) *costNode {
	if kind == "" || kind == "company" {
		return n
	}
	if n.Kind == kind && n.ID == id {
		return n
	}
	for _, x := range n.Children {
		if f := costFind(x, kind, id); f != nil {
			return f
		}
	}
	return nil
}

// costYm is the month asked for (?ym=), by default the open payroll month.
func costYm(ctx context.Context, q store.Q, r *http.Request) (string, error) {
	ym := r.URL.Query().Get("ym")
	if ym == "" {
		return openYm(ctx, q), nil
	}
	if !isYm(ym) {
		return "", bad("Month must look like 2026-10")
	}
	if ym > addYm(openYm(ctx, q), 12) {
		return "", bad("Pick a month at most a year after the open payroll month")
	}
	return ym, nil
}

// costGet is the console for a month: the tree, how it was made, and what the viewer can do.
func (s *Server) costGet(r *http.Request, c *Ctx) (any, error) {
	if err := costCan(c); err != nil {
		return nil, err
	}
	ctx, q := r.Context(), s.st.DB
	ym, err := costYm(ctx, q, r)
	if err != nil {
		return nil, err
	}
	b, meta, err := costBookFor(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	return map[string]any{"meta": meta, "tree": costTree(b, c), "canBudget": c.Level("cost.budget") >= 2, "canPeople": costSeesPeople(c),
		"scopeAll": c.Scope.Access == "all"}, nil
}

// costSeesPeople: the per-person drill-down shows each person's salary, so it needs salary figures in full.
func costSeesPeople(c *Ctx) bool { return c.Sensitive["salary"] == "Full" }

// costPeople is the drill-down: each person of a node (?kind=&id=) with their cost lines.
func (s *Server) costPeople(r *http.Request, c *Ctx) (any, error) {
	if err := costCan(c); err != nil {
		return nil, err
	}
	if !costSeesPeople(c) {
		return nil, forbidden("Each person's cost needs salary figures in full")
	}
	ctx, q := r.Context(), s.st.DB
	ym, err := costYm(ctx, q, r)
	if err != nil {
		return nil, err
	}
	kind, id := r.URL.Query().Get("kind"), r.URL.Query().Get("id")
	if !containsStr([]string{"", "company", "state", "city", "cluster", "property"}, kind) {
		return nil, bad("Unknown level")
	}
	b, meta, err := costBookFor(ctx, q, ym)
	if err != nil {
		return nil, err
	}
	type row struct {
		costLine
		Location string  `json:"location"`
		Contrib  float64 `json:"contrib"`
		Total    float64 `json:"total"`
	}
	out := []row{}
	for _, l := range b.People {
		pl, placed := b.Places[l.LocID]
		loc := l.LocID
		if !placed {
			loc = ""
		}
		if !costInScope(c, loc) {
			continue
		}
		in := false
		switch kind {
		case "", "company":
			in = true
		case "property":
			in = loc == id
		case "cluster":
			in = placed && pl.ClusterID == id
		case "city":
			in = placed && pl.CityID == id
		case "state":
			in = placed && pl.StateID == id
		}
		if !in {
			continue
		}
		x := row{costLine: l, Location: pl.Name}
		rd := calc.Round2
		x.Salary, x.Reimb, x.TopUp, x.PF, x.Admin, x.ESI, x.LWF, x.Grat, x.Bonus, x.Med, x.Accom = rd(l.Salary), rd(l.Reimb), rd(l.TopUp), rd(l.PF), rd(l.Admin), rd(l.ESI), rd(l.LWF), rd(l.Grat), rd(l.Bonus), rd(l.Med), rd(l.Accom)
		x.PlEnc = rd(l.PlEnc)
		x.Contrib = rd(l.contrib())
		x.Total = rd(l.Salary + l.Reimb + l.contrib() + l.Accom)
		out = append(out, x)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return map[string]any{"meta": meta, "people": out}, nil
}

// costTrend is a node's figures for the six months to ?ym (oldest first).
func (s *Server) costTrend(r *http.Request, c *Ctx) (any, error) {
	if err := costCan(c); err != nil {
		return nil, err
	}
	ctx, q := r.Context(), s.st.DB
	ym, err := costYm(ctx, q, r)
	if err != nil {
		return nil, err
	}
	kind, id := r.URL.Query().Get("kind"), r.URL.Query().Get("id")
	out := []map[string]any{}
	for i := 5; i >= 0; i-- {
		m := addYm(ym, -i)
		b, meta, err := costBookFor(ctx, q, m)
		if err != nil {
			return nil, err
		}
		n := costFind(costTree(b, c), kind, id)
		if n == nil {
			n = &costNode{}
		}
		out = append(out, map[string]any{"ym": m, "mode": meta.Mode, "frozen": meta.Frozen, "people": n.People, "salary": n.Salary, "reimb": n.Reimb, "contrib": n.Contrib,
			"accom": n.Accom, "total": n.Total, "perHead": n.PerHead, "budget": n.Budget, "over": n.Over})
	}
	return map[string]any{"months": out}, nil
}

// costSheet is the console's view as Excel: one row per node, the level shown by indentation.
func (s *Server) costSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := costCan(c); err != nil {
		return "", nil, nil, err
	}
	ctx, q := r.Context(), s.st.DB
	ym, err := costYm(ctx, q, r)
	if err != nil {
		return "", nil, nil, err
	}
	b, meta, err := costBookFor(ctx, q, ym)
	if err != nil {
		return "", nil, nil, err
	}
	basis := map[string]string{"actual": "Actual", "projected": "Projected", "none": "No approved payroll"}[meta.Mode]
	if meta.Frozen && meta.FrozenAt != nil {
		basis = "Actual, frozen on " + meta.FrozenAt.In(store.Loc()).Format("02 Jan 2006")
	}
	rows := [][]any{}
	level := map[string]string{"company": "Company", "state": "State", "city": "City", "cluster": "Cluster", "property": "Property"}
	var walk func(n *costNode, depth int)
	walk = func(n *costNode, depth int) {
		budget, diff, pct := any(""), any(""), any("")
		if n.Budget > 0 {
			budget, diff, pct = n.Budget, n.Diff, n.Pct
		}
		rows = append(rows, []any{ymLabel(ym), basis, level[n.Kind], strings.Repeat("  ", depth) + n.Name, n.People, n.Salary, n.Reimb, n.TopUp, n.PF, n.Admin, n.ESI, n.LWF, n.Grat, n.PlEnc, n.Bonus, n.Med,
			n.Contrib, n.AccomShare, n.AccomEmpty, n.Accom, n.Total, n.PerHead, budget, diff, pct, map[bool]string{true: "Over budget"}[n.Over]})
		for _, x := range n.Children {
			walk(x, depth+1)
		}
	}
	walk(costTree(b, c), 0)
	return "cost-" + ym + ".xlsx", []string{"Month", "Basis", "Level", "Name", "People", "Salary paid", "Reimbursement (outside the gross)", "Minimum-wage top-ups (excluded)", "Employer PF", "EPFO admin and EDLI", "Employer ESI", "LWF (employer)",
		"Gratuity", "PL encashment provision", "Bonus", "Medical insurance", "Company contributions", "Accommodation (shares)", "Accommodation (empty units)", "Accommodation", "Total cost",
		"Cost per head", "Budget", "Difference", "% of budget used", "Over"}, rows, nil
}

// ---------- Budgets ----------

// costBudgetSave sets a property's or a cluster's budgets (PUT /api/cost/budget/{location|cluster}/{id}): headcount
// and monthly cost. For a property they are the location's own fields (also on Compensation › Budgets); a cluster's
// figure can be cleared (null) to go back to the total of its properties.
func (s *Server) costBudgetSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("cost.budget", 2); err != nil {
		return nil, err
	}
	var b struct {
		Headcount      *int     `json:"headcount"`
		Cost           *float64 `json:"cost"`
		ClearHeadcount bool     `json:"clearHeadcount"`
		ClearCost      bool     `json:"clearCost"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if (b.Headcount != nil && *b.Headcount < 0) || (b.Cost != nil && *b.Cost < 0) {
		return nil, bad("Budgets cannot be negative")
	}
	if b.Cost != nil && *b.Cost > 1e12 {
		return nil, bad("That budget is too large")
	}
	ctx, kind, id := r.Context(), r.PathValue("kind"), r.PathValue("id")
	set, unset := bson.M{}, bson.M{}
	switch kind {
	case "location":
		if err := c.MustInScopeLocation(id); err != nil {
			return nil, err
		}
		if b.ClearHeadcount || b.ClearCost {
			return nil, bad("A property's budget is a number; 0 means no budget")
		}
		if b.Headcount != nil {
			set["headcount_budget"] = int32(*b.Headcount)
		}
		if b.Cost != nil {
			set["payroll_budget"] = calc.Round2(*b.Cost)
		}
	case "cluster":
		if c.Scope.Access != "all" {
			locs, err := s.st.DB.C("location").Distinct(ctx, "_id", bson.M{"cluster_id": id, "status": "Active"})
			if err != nil {
				return nil, err
			}
			for _, l := range locs {
				if !costInScope(c, str(l)) {
					return nil, forbidden("Part of that cluster is outside your access")
				}
			}
			if len(locs) == 0 {
				return nil, forbidden("")
			}
		}
		if b.Headcount != nil {
			set["headcount_budget"] = int32(*b.Headcount)
		} else if b.ClearHeadcount {
			unset["headcount_budget"] = nil
		}
		if b.Cost != nil {
			set["cost_budget"] = calc.Round2(*b.Cost)
		} else if b.ClearCost {
			unset["cost_budget"] = nil
		}
		for k := range unset {
			set[k] = nil
		}
	default:
		return nil, bad("Budgets are kept per property (location) and per cluster")
	}
	if len(set) == 0 {
		return nil, bad("Nothing to change")
	}
	fields := []string{"headcount_budget", "payroll_budget"}
	if kind == "cluster" {
		fields = []string{"headcount_budget", "cost_budget"}
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		before, err := tx.C(kind).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(compFields(fields...)))
		if err != nil {
			return err
		}
		if before == nil {
			return notFound("")
		}
		if _, err := tx.C(kind).Update(ctx, bson.M{"_id": id}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Budget changed (cost console)", kind, id, before, set)
	})
}

// ---------- Headcount plan ----------

// seatUse is a property's seats in use: people in post (active, Joining IDs included), offers in flight (accepted and
// not joined yet, and pre-boarding records) and offers still waiting for approval.
type seatUse struct{ InPost, InFlight, Awaiting int64 }

func (u *seatUse) taken() int64 {
	if u == nil {
		return 0
	}
	return u.InPost + u.InFlight + u.Awaiting
}

// seatsUsed counts the seats in use per property (locs nil = every property), leaving out one offer (the offer being
// checked). The headcount plan and the offer budget check both count with it.
func seatsUsed(ctx context.Context, q store.Q, locs []any, exceptOffer string) (map[string]*seatUse, error) {
	out := map[string]*seatUse{}
	get := func(loc string) *seatUse {
		u := out[loc]
		if u == nil {
			u = &seatUse{}
			out[loc] = u
		}
		return u
	}
	ef := bson.M{"status": bson.M{"$in": bson.A{"Active", "Pre-boarding"}}}
	of := bson.M{"employee_id": nil, "declined_at": nil, "lapsed_at": nil}
	if locs != nil {
		ef["location_id"] = bson.M{"$in": locs}
		of["location_id"] = bson.M{"$in": locs}
	}
	emps, err := q.C("employee").Find(ctx, ef, options.Find().SetProjection(compFields("location_id", "status")))
	if err != nil {
		return nil, err
	}
	for _, e := range emps {
		if e["status"] == "Active" {
			get(str(e["location_id"])).InPost++
		} else {
			get(str(e["location_id"])).InFlight++
		}
	}
	offers, err := q.C("offer").Find(ctx, of, options.Find().SetProjection(compFields("location_id", "request_id", "accepted_at", "candidate_id")))
	if err != nil || len(offers) == 0 {
		return out, err
	}
	reqIDs, candIDs := bson.A{}, bson.A{}
	for _, o := range offers {
		if o["request_id"] != nil {
			reqIDs = append(reqIDs, o["request_id"])
		}
		if o["candidate_id"] != nil {
			candIDs = append(candIDs, o["candidate_id"])
		}
	}
	pending := map[string]bool{}
	if len(reqIDs) > 0 {
		ids, err := q.C("approval_request").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": reqIDs}, "status": "Pending"})
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			pending[str(id)] = true
		}
	}
	stage := map[string]string{}
	if len(candIDs) > 0 {
		cs, err := q.C("candidate").Find(ctx, bson.M{"_id": bson.M{"$in": candIDs}}, options.Find().SetProjection(compFields("stage")))
		if err != nil {
			return nil, err
		}
		for _, x := range cs {
			stage[str(x["id"])] = str(x["stage"])
		}
	}
	for _, o := range offers {
		if exceptOffer != "" && str(o["id"]) == exceptOffer {
			continue
		}
		loc := str(o["location_id"])
		switch {
		case o["accepted_at"] != nil:
			// A candidate who joined, withdrew or was closed is no longer an offer in flight.
			if cid := str(o["candidate_id"]); cid != "" && stage[cid] != "Offer" {
				continue
			}
			get(loc).InFlight++
		case o["request_id"] != nil && pending[str(o["request_id"])]:
			get(loc).Awaiting++
		}
	}
	return out, nil
}

// costHeadcount is the headcount plan: per cluster and property of the viewer's scope, the seats budgeted against
// people in post, offers in flight and offers in approval.
func (s *Server) costHeadcount(r *http.Request, c *Ctx) (any, error) {
	if err := costCan(c); err != nil {
		return nil, err
	}
	ctx, q := r.Context(), s.st.DB
	places, cb, err := costPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	use, err := seatsUsed(ctx, q, nil, "")
	if err != nil {
		return nil, err
	}
	type prop struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Seats    int    `json:"seats"`
		InPost   int64  `json:"inPost"`
		InFlight int64  `json:"inFlight"`
		Awaiting int64  `json:"awaiting"`
		Free     *int64 `json:"free"` // null when no seats are budgeted
		Over     bool   `json:"over"`
	}
	type cluster struct {
		prop
		City       string `json:"city"`
		State      string `json:"state"`
		Own        bool   `json:"own"`
		Properties []prop `json:"properties"`
	}
	fill := func(p *prop) {
		if p.Seats > 0 {
			f := int64(p.Seats) - p.InPost - p.InFlight - p.Awaiting
			p.Free, p.Over = &f, f < 0
		}
	}
	byCl := map[string]*cluster{}
	order := []*cluster{}
	for loc, pl := range places {
		u := use[loc]
		if !costInScope(c, loc) || (!pl.Active && u.taken() == 0) {
			continue
		}
		if u == nil {
			u = &seatUse{}
		}
		p := prop{ID: loc, Name: pl.Name, Seats: pl.HC, InPost: u.InPost, InFlight: u.InFlight, Awaiting: u.Awaiting}
		fill(&p)
		cl := byCl[pl.ClusterID]
		if cl == nil {
			cl = &cluster{prop: prop{ID: pl.ClusterID, Name: pl.Cluster}, City: pl.City, State: pl.State}
			byCl[pl.ClusterID] = cl
			order = append(order, cl)
		}
		cl.Properties = append(cl.Properties, p)
		cl.Seats += p.Seats
		cl.InPost, cl.InFlight, cl.Awaiting = cl.InPost+p.InPost, cl.InFlight+p.InFlight, cl.Awaiting+p.Awaiting
	}
	for _, cl := range order {
		if own := cb[cl.ID].HC; own != nil && *own > 0 {
			cl.Seats, cl.Own = *own, true
		}
		fill(&cl.prop)
		sort.SliceStable(cl.Properties, func(i, j int) bool { return cl.Properties[i].Name < cl.Properties[j].Name })
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].City != order[j].City {
			return order[i].City < order[j].City
		}
		return order[i].Name < order[j].Name
	})
	return map[string]any{"clusters": order, "canBudget": c.Level("cost.budget") >= 2}, nil
}
