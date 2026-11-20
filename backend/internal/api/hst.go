package api

// Staff accommodation (workforce plan, module 4; the owner's decisions of 16–17 Nov 2026). The company rents flats,
// houses and PGs (units) for its staff:
//
//   - Setup (hst.setup): units with their leases (history kept, documents attached), fixed monthly items and the
//     variable costs HR enters each month (electricity, water, Wi-Fi, housekeeping, other).
//   - Allocation (hst.allot): check-in and check-out per bed, with the full history per person and per unit. A bed is
//     never double-booked and a person never has two stays at once. "Assign accommodation" suggests the units of the
//     person's cluster with a free bed, the ones serving their property first, matching gender.
//   - Cost split (decided 17 Nov, internal/hst): by default a unit's month cost is divided equally among everyone who
//     stayed in it at any time in the month; the setting "Split by days stayed" (off) divides each day's cost among
//     the people staying that day instead. A month (or, by days, a day) with nobody in the unit is empty-unit cost,
//     carried by the unit's home property.
//   - Not payroll (decided 17 Nov): the split is the company's own cost calculation only. Nothing of it reaches the
//     payslip, salary register, payroll sheet, TDS or any payroll total, and a change here never makes a payroll
//     run stale.
//
// This is a different thing from employee_pay.accommodation, the manual monthly deduction HR uses for the minimum
// wage top-up, which is untouched and stays on the payslip and salary sheet as before.
//
// The cost console (module 6) reads HstMonthCost.

import (
	"context"
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
	"github.com/saltstayz/hrms/internal/hst"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// routesHst registers the accommodation routes (server.go calls it).
func (s *Server) routesHst(m *http.ServeMux) {
	m.HandleFunc("GET /api/hst/setup", s.a(s.hstSetupGet))
	m.HandleFunc("PUT /api/hst/settings", s.a(s.hstSettingsPut))
	m.HandleFunc("GET /api/hst/units", s.a(s.hstUnitList))
	m.HandleFunc("POST /api/hst/units", s.a(s.hstUnitSave))
	m.HandleFunc("GET /api/hst/units/{id}", s.a(s.hstUnitGet))
	m.HandleFunc("PUT /api/hst/units/{id}", s.a(s.hstUnitSave))
	m.HandleFunc("POST /api/hst/units/{id}/leases", s.a(s.hstLeaseSave))
	m.HandleFunc("PUT /api/hst/leases/{id}", s.a(s.hstLeaseSave))
	m.HandleFunc("PUT /api/hst/units/{id}/costs/{ym}", s.a(s.hstCostSave))
	m.HandleFunc("GET /api/hst/suggest", s.a(s.hstSuggest))
	m.HandleFunc("GET /api/hst/stays", s.a(s.hstStays))
	m.HandleFunc("POST /api/hst/stays", s.a(s.hstCheckIn))
	m.HandleFunc("PUT /api/hst/stays/{id}", s.a(s.hstStayEdit))
	m.HandleFunc("POST /api/hst/stays/{id}/checkout", s.a(s.hstCheckOut))
	m.HandleFunc("GET /api/hst/attention", s.a(s.hstAttention))
	m.HandleFunc("GET /api/hst/board", s.a(s.hstBoard))
	m.HandleFunc("GET /api/hst/board.xlsx", s.sheet(s.hstBoardSheet))
	m.HandleFunc("GET /api/hst/split", s.a(s.hstSplitGet))
	m.HandleFunc("GET /api/hst/waiting", s.a(s.hstWaiting))            // hst_auto.go
	m.HandleFunc("POST /api/hst/waiting/{id}/drop", s.a(s.hstWaitDrop)) // hst_auto.go
	m.HandleFunc("GET /api/hst/clusters", s.a(s.hstAccomClusters))      // hst_auto.go
}

// ---------- Records ----------

type hstItem struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
}

type hstUnitRec struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Address   string    `json:"address"`
	ClusterID string    `json:"clusterId"`
	Near      []string  `json:"near"`
	Gender    string    `json:"gender"`
	Beds      int       `json:"beds"`
	Landlord  string    `json:"landlord"`
	Phone     string    `json:"landlordPhone"`
	Status    string    `json:"status"`
	Fixed     []hstItem `json:"fixed"`
	Note      string    `json:"note"`
}

func hstUnitOf(r store.Row) hstUnitRec {
	u := hstUnitRec{ID: str(r["id"]), Name: str(r["name"]), Kind: str(r["kind"]), Address: str(r["address"]), ClusterID: str(r["cluster_id"]),
		Near: payengineStrings(r["near_ids"]), Gender: str(r["gender"]), Beds: int(payengineFloat(r["beds"])), Landlord: str(r["landlord"]),
		Phone: str(r["landlord_phone"]), Status: str(r["status"]), Note: str(r["note"]), Fixed: []hstItem{}}
	if u.Near == nil {
		u.Near = []string{}
	}
	_ = payengineJSON(r["fixed_items"], &u.Fixed)
	return u
}

// home is the property that carries the unit's empty-unit cost: the first it serves.
func (u hstUnitRec) home() string {
	if len(u.Near) > 0 {
		return u.Near[0]
	}
	return ""
}

func (u hstUnitRec) fixedTotal() float64 {
	t := 0.0
	for _, x := range u.Fixed {
		t += x.Amount
	}
	return calc.Round2(t)
}

type hstLeaseRec struct {
	ID          string  `json:"id"`
	UnitID      string  `json:"unitId"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	NoticeDays  int     `json:"noticeDays"`
	Rent        float64 `json:"rent"`
	Deposit     float64 `json:"deposit"`
	Maintenance float64 `json:"maintenance"`
	IncPct      float64 `json:"incPct"`
	IncOn       string  `json:"incOn"`
	Note        string  `json:"note"`
}

func hstLeaseOf(r store.Row) hstLeaseRec {
	return hstLeaseRec{ID: str(r["id"]), UnitID: str(r["unit_id"]), Start: str(r["start"]), End: str(r["end"]), NoticeDays: int(payengineFloat(r["notice_days"])),
		Rent: payengineFloat(r["rent"]), Deposit: payengineFloat(r["deposit"]), Maintenance: payengineFloat(r["maintenance"]), IncPct: payengineFloat(r["inc_pct"]),
		IncOn: str(r["inc_on"]), Note: str(r["note"])}
}

func (l hstLeaseRec) lease() hst.Lease {
	return hst.Lease{ID: l.ID, Start: l.Start, End: l.End, Rent: l.Rent, Maintenance: l.Maintenance, IncPct: l.IncPct, IncOn: l.IncOn}
}

type hstStayRec struct {
	ID         string `json:"id"`
	UnitID     string `json:"unitId"`
	EmployeeID string `json:"employeeId"`
	Bed        int    `json:"bed"`
	In         string `json:"checkIn"`
	Out        string `json:"checkOut"`
	Note       string `json:"note"`
	OutReason  string `json:"outReason"`
	By         string `json:"by"`
}

func hstStayOf(r store.Row) hstStayRec {
	return hstStayRec{ID: str(r["id"]), UnitID: str(r["unit_id"]), EmployeeID: str(r["employee_id"]), Bed: int(payengineFloat(r["bed"])), In: str(r["check_in"]),
		Out: str(r["check_out"]), Note: str(r["note"]), OutReason: str(r["out_reason"]), By: str(r["created_by"])}
}

// on reports whether the person is in the unit on date d (the check-out day is free).
func (x hstStayRec) on(d string) bool { return x.In <= d && (x.Out == "" || d < x.Out) }

func hstUnits(ctx context.Context, q store.Q, filter any) ([]hstUnitRec, error) {
	rows, err := q.C(store.HstUnitColl).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]hstUnitRec, 0, len(rows))
	for _, r := range rows {
		out = append(out, hstUnitOf(r))
	}
	return out, nil
}

func hstUnitIDs(units []hstUnitRec) []string {
	ids := make([]string, 0, len(units))
	for _, u := range units {
		ids = append(ids, u.ID)
	}
	return ids
}

func hstLeases(ctx context.Context, q store.Q, unitIDs []string) (map[string][]hstLeaseRec, error) {
	rows, err := q.C(store.HstLeaseColl).Find(ctx, bson.M{"unit_id": bson.M{"$in": unitIDs}}, options.Find().SetSort(bson.D{{Key: "start", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := map[string][]hstLeaseRec{}
	for _, r := range rows {
		l := hstLeaseOf(r)
		out[l.UnitID] = append(out[l.UnitID], l)
	}
	return out, nil
}

// hstStaysOf lists stays matching filter, oldest first.
func hstStaysOf(ctx context.Context, q store.Q, filter any) ([]hstStayRec, error) {
	rows, err := q.C(store.HstStayColl).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "check_in", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]hstStayRec, 0, len(rows))
	for _, r := range rows {
		out = append(out, hstStayOf(r))
	}
	return out, nil
}

// hstOverlapping matches the stays that share a day with [from, to) (to "" = open).
func hstOverlapping(from, to string) bson.M {
	f := bson.M{"$or": bson.A{bson.M{"check_out": nil}, bson.M{"check_out": bson.M{"$gt": from}}}}
	if to != "" {
		f["check_in"] = bson.M{"$lt": to}
	}
	return f
}

func hstNames(ctx context.Context, q store.Q, coll string, ids []string) map[string]string {
	out := map[string]string{}
	rows, _ := timectxByID(ctx, q, coll, ids, "name")
	for id, r := range rows {
		out[id] = str(r["name"])
	}
	return out
}

func hstEmpNames(ctx context.Context, q store.Q, ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	rows, _ := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}}))
	for _, r := range rows {
		out[str(r["id"])] = str(r["name"])
	}
	return out
}

// ---------- Settings ----------

// hstSettings is company_setting.hst.
type hstSettings struct {
	ByDays    bool `json:"byDays"`    // "Split by days stayed" (off: the month cost equally among everyone who stayed)
	LeaseDays int  `json:"leaseDays"` // remind this many days before a lease ends
	IncDays   int  `json:"incDays"`   // remind this many days before a rent increase
	BoardDays int  `json:"boardDays"` // the occupancy board lists leases ending within this many days
}

// method is the split method the settings choose.
func (s hstSettings) method() string {
	if s.ByDays {
		return hst.ByDays
	}
	return hst.Equal
}

func hstDefaults() hstSettings { return hstSettings{LeaseDays: 60, IncDays: 30, BoardDays: 60} }

func loadHstSettings(ctx context.Context, q store.Q) (hstSettings, error) {
	set := hstDefaults()
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"hst": 1}))
	if err != nil || r == nil || r["hst"] == nil {
		return set, err
	}
	_ = payengineJSON(r["hst"], &set)
	d := hstDefaults()
	if set.LeaseDays <= 0 {
		set.LeaseDays = d.LeaseDays
	}
	if set.IncDays <= 0 {
		set.IncDays = d.IncDays
	}
	if set.BoardDays <= 0 {
		set.BoardDays = d.BoardDays
	}
	return set, nil
}

func hstCan(c *Ctx) error {
	if c.Level("hst.setup") < 1 && c.Level("hst.allot") < 1 {
		return forbidden("")
	}
	return nil
}

// hstClusters are the clusters the caller sees (nil = all): those holding a location of a by-location scope.
func hstClusters(ctx context.Context, q store.Q, c *Ctx) (map[string]bool, error) {
	if c.Scope.Access == "all" {
		return nil, nil
	}
	out := map[string]bool{}
	if c.Scope.Access != "locs" || len(c.Scope.Locations) == 0 {
		return out, nil
	}
	ids, err := q.C("location").Distinct(ctx, "cluster_id", bson.M{"_id": bson.M{"$in": c.Scope.Locations}})
	for _, id := range ids {
		out[str(id)] = true
	}
	return out, err
}

func hstUnitFilter(vis map[string]bool) bson.M {
	if vis == nil {
		return bson.M{}
	}
	ids := []string{}
	for id := range vis {
		ids = append(ids, id)
	}
	return bson.M{"cluster_id": bson.M{"$in": ids}}
}

type hstPlace struct {
	clusters  map[string]store.Row // id → name, city_id
	cities    map[string]string
	locations map[string]store.Row // id → name, cluster_id
}

func hstPlaces(ctx context.Context, q store.Q) (*hstPlace, error) {
	p := &hstPlace{clusters: map[string]store.Row{}, cities: map[string]string{}, locations: map[string]store.Row{}}
	cl, err := q.C("cluster").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "city_id", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range cl {
		p.clusters[str(r["id"])] = r
	}
	ci, err := q.C("city").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range ci {
		p.cities[str(r["id"])] = str(r["name"])
	}
	lo, err := q.C("location").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "cluster_id", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range lo {
		p.locations[str(r["id"])] = r
	}
	return p, nil
}

func (p *hstPlace) cluster(id string) string { return str(p.clusters[id]["name"]) }
func (p *hstPlace) city(clusterID string) string {
	return p.cities[str(p.clusters[clusterID]["city_id"])]
}
func (p *hstPlace) location(id string) string { return str(p.locations[id]["name"]) }

func (s *Server) hstSetupGet(r *http.Request, c *Ctx) (any, error) {
	if err := hstCan(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	set, err := loadHstSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	vis, err := hstClusters(ctx, q, c)
	if err != nil {
		return nil, err
	}
	clusters, locations := []map[string]any{}, []map[string]any{}
	for id, r := range p.clusters {
		if vis != nil && !vis[id] {
			continue
		}
		clusters = append(clusters, map[string]any{"id": id, "name": str(r["name"]), "city": p.city(id), "active": str(r["status"]) != "Inactive"})
	}
	for id, r := range p.locations {
		if vis != nil && !vis[str(r["cluster_id"])] {
			continue
		}
		locations = append(locations, map[string]any{"id": id, "name": str(r["name"]), "clusterId": str(r["cluster_id"]), "active": str(r["status"]) != "Inactive"})
	}
	byName := func(l []map[string]any) {
		sort.Slice(l, func(i, j int) bool { return str(l[i]["name"]) < str(l[j]["name"]) })
	}
	byName(clusters)
	byName(locations)
	return map[string]any{"settings": set, "clusters": clusters, "locations": locations, "today": store.Today(), "openYm": openYm(ctx, q),
		"can": map[string]any{"setup": c.Level("hst.setup"), "allot": c.Level("hst.allot")}}, nil
}

func (s *Server) hstSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.setup", 2); err != nil {
		return nil, err
	}
	var b hstSettings
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.LeaseDays < 1 || b.LeaseDays > 365 || b.IncDays < 1 || b.IncDays > 365 || b.BoardDays < 1 || b.BoardDays > 365 {
		return nil, bad("Reminder and board windows must be between 1 and 365 days")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before, err := loadHstSettings(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"hst": b}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Accommodation settings changed", "hst_settings", "1", before, b)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"settings": b}, nil
}

// ---------- The month split: the cost source ----------

// HstMonthCost is the accommodation cost of month ym (YYYY-MM) for the cost console (workforce plan, module 6), split
// by the method the settings choose: each unit's cost, each employee's share (all units together) and each
// property's empty-unit cost (key: location id; "" for a unit that serves no property). Shares and empty cost add
// up to the units' cost. It is a cost figure only, never payroll.
func HstMonthCost(ctx context.Context, q store.Q, ym string) (hst.Month, error) {
	units, err := hstUnits(ctx, q, nil)
	if err != nil {
		return hst.Month{}, err
	}
	return hstSplit(ctx, q, ym, units)
}

func hstSplit(ctx context.Context, q store.Q, ym string, units []hstUnitRec) (hst.Month, error) {
	if !isYm(ym) {
		return hst.Month{}, bad("Month must be YYYY-MM")
	}
	set, err := loadHstSettings(ctx, q)
	if err != nil {
		return hst.Month{}, err
	}
	ids := hstUnitIDs(units)
	leases, err := hstLeases(ctx, q, ids)
	if err != nil {
		return hst.Month{}, err
	}
	costs, err := q.C(store.HstCostColl).Find(ctx, bson.M{"ym": ym, "unit_id": bson.M{"$in": ids}})
	if err != nil {
		return hst.Month{}, err
	}
	variable := map[string]float64{}
	for _, r := range costs {
		variable[str(r["unit_id"])] = payengineFloat(r["total"])
	}
	first, last := ym+"-01", policy.LastDayOf(ym)
	f := hstOverlapping(first, policy.AddDays(last, 1))
	f["unit_id"] = bson.M{"$in": ids}
	stays, err := hstStaysOf(ctx, q, f)
	if err != nil {
		return hst.Month{}, err
	}
	us := make([]hst.Unit, 0, len(units))
	for _, u := range units {
		hu := hst.Unit{ID: u.ID, Home: u.home(), Fixed: u.fixedTotal(), Variable: variable[u.ID]}
		for _, l := range leases[u.ID] {
			hu.Leases = append(hu.Leases, l.lease())
		}
		us = append(us, hu)
	}
	ss := make([]hst.Stay, 0, len(stays))
	for _, x := range stays {
		ss = append(ss, hst.Stay{ID: x.ID, UnitID: x.UnitID, EmployeeID: x.EmployeeID, In: x.In, Out: x.Out})
	}
	return hst.Split(ym, us, ss, set.method()), nil
}

// ---------- Units ----------

type hstUnitOut struct {
	hstUnitRec
	Cluster    string   `json:"cluster"`
	City       string   `json:"city"`
	NearNames  []string `json:"nearNames"`
	Filled     int      `json:"filled"`
	Free       int      `json:"free"`
	RentNow    float64  `json:"rentNow"`
	MonthlyNow float64  `json:"monthlyNow"`
	LeaseEnd   string   `json:"leaseEnd"`
	Leased     bool     `json:"leased"`
}

func hstUnitView(u hstUnitRec, p *hstPlace, leases []hstLeaseRec, stays []hstStayRec, day string) hstUnitOut {
	o := hstUnitOut{hstUnitRec: u, Cluster: p.cluster(u.ClusterID), City: p.city(u.ClusterID), NearNames: []string{}}
	for _, id := range u.Near {
		o.NearNames = append(o.NearNames, p.location(id))
	}
	for _, x := range stays {
		if x.UnitID == u.ID && x.on(day) {
			o.Filled++
		}
	}
	o.Free = max(0, u.Beds-o.Filled)
	hu := hst.Unit{Fixed: u.fixedTotal()}
	for _, l := range leases {
		hu.Leases = append(hu.Leases, l.lease())
	}
	if l := hu.LeaseOn(day); l != nil {
		o.Leased, o.RentNow, o.MonthlyNow, o.LeaseEnd = true, l.RentOn(day), calc.Round2(hu.MonthlyOn(day)), l.End
	}
	return o
}

func (s *Server) hstUnitList(r *http.Request, c *Ctx) (any, error) {
	if err := hstCan(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	vis, err := hstClusters(ctx, q, c)
	if err != nil {
		return nil, err
	}
	units, err := hstUnits(ctx, q, hstUnitFilter(vis))
	if err != nil {
		return nil, err
	}
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	ids := hstUnitIDs(units)
	leases, err := hstLeases(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	day := store.Today()
	f := hstOverlapping(day, policy.AddDays(day, 1))
	f["unit_id"] = bson.M{"$in": ids}
	stays, err := hstStaysOf(ctx, q, f)
	if err != nil {
		return nil, err
	}
	out := make([]hstUnitOut, 0, len(units))
	for _, u := range units {
		out = append(out, hstUnitView(u, p, leases[u.ID], stays, day))
	}
	return map[string]any{"units": out, "today": day}, nil
}

// hstUnitIn checks a unit the caller may see and returns it (nil error) or a 404/403.
func hstUnitIn(ctx context.Context, q store.Q, c *Ctx, id string) (*hstUnitRec, error) {
	row, err := q.C(store.HstUnitColl).FindOne(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, notFound("Accommodation unit not found")
	}
	u := hstUnitOf(row)
	vis, err := hstClusters(ctx, q, c)
	if err != nil {
		return nil, err
	}
	if vis != nil && !vis[u.ClusterID] {
		return nil, forbidden("This unit is outside your access")
	}
	return &u, nil
}

func (s *Server) hstUnitGet(r *http.Request, c *Ctx) (any, error) {
	if err := hstCan(c); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	u, err := hstUnitIn(ctx, q, c, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	ym := r.URL.Query().Get("ym")
	if ym == "" {
		ym = store.Today()[:7]
	}
	if !isYm(ym) {
		return nil, bad("Month must be YYYY-MM")
	}
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	leases, err := hstLeases(ctx, q, []string{u.ID})
	if err != nil {
		return nil, err
	}
	stays, err := hstStaysOf(ctx, q, bson.M{"unit_id": u.ID})
	if err != nil {
		return nil, err
	}
	day := store.Today()
	view := hstUnitView(*u, p, leases[u.ID], stays, day)
	ls := []map[string]any{}
	for i := len(leases[u.ID]) - 1; i >= 0; i-- { // newest first
		l := leases[u.ID][i]
		docs, err := docsOf(ctx, q, "hst_lease", l.ID)
		if err != nil {
			return nil, err
		}
		hl := l.lease()
		ls = append(ls, map[string]any{"lease": l, "docs": docs, "rentNow": hl.RentOn(day), "nextIncrease": hl.NextIncrease(day), "current": hl.InForce(day)})
	}
	who := []string{}
	for _, x := range stays {
		who = append(who, x.EmployeeID)
	}
	names := hstEmpNames(ctx, q, who)
	st := []map[string]any{}
	for i := len(stays) - 1; i >= 0; i-- {
		x := stays[i]
		st = append(st, map[string]any{"stay": x, "name": names[x.EmployeeID], "now": x.on(day)})
	}
	costRows, err := q.C(store.HstCostColl).Find(ctx, bson.M{"unit_id": u.ID}, options.Find().SetSort(bson.D{{Key: "ym", Value: -1}}).SetLimit(24))
	if err != nil {
		return nil, err
	}
	costs := []map[string]any{}
	for _, r := range costRows {
		costs = append(costs, map[string]any{"ym": str(r["ym"]), "items": r["items"], "total": payengineFloat(r["total"]), "by": str(r["by_name"]), "at": r["at"]})
	}
	m, err := hstSplit(ctx, q, ym, []hstUnitRec{*u})
	if err != nil {
		return nil, err
	}
	um := m.Units[u.ID]
	shares := []map[string]any{}
	for e, v := range um.Shares {
		shares = append(shares, map[string]any{"employeeId": e, "name": hstEmpNames(ctx, q, []string{e})[e], "days": um.Days[e], "share": v})
	}
	sort.Slice(shares, func(i, j int) bool { return str(shares[i]["employeeId"]) < str(shares[j]["employeeId"]) })
	return map[string]any{"unit": view, "leases": ls, "stays": st, "costs": costs, "today": day,
		"month": map[string]any{"ym": ym, "method": m.Method, "days": m.Days, "cost": um.Cost, "allocated": um.Allocated, "empty": um.Empty, "emptyDays": um.EmptyDays, "shares": shares,
			"home": p.location(u.home())}}, nil
}

type hstUnitBody struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Address   string    `json:"address"`
	ClusterID string    `json:"clusterId"`
	Near      []string  `json:"near"`
	Gender    string    `json:"gender"`
	Beds      int       `json:"beds"`
	Landlord  string    `json:"landlord"`
	Phone     string    `json:"landlordPhone"`
	Status    string    `json:"status"`
	Fixed     []hstItem `json:"fixed"`
	Note      string    `json:"note"`
}

func (s *Server) hstUnitSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.setup", 2); err != nil {
		return nil, err
	}
	var b hstUnitBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name, b.Address, b.Landlord, b.Phone, b.Note = strings.TrimSpace(b.Name), strings.TrimSpace(b.Address), strings.TrimSpace(b.Landlord), strings.TrimSpace(b.Phone), strings.TrimSpace(b.Note)
	if b.Status == "" {
		b.Status = "Active"
	}
	switch {
	case b.Name == "" || len(b.Name) > 80:
		return nil, bad("Give the unit a name (up to 80 characters)")
	case b.Kind != "Flat" && b.Kind != "House" && b.Kind != "PG":
		return nil, bad("Type must be Flat, House or PG")
	case b.Gender != "Male" && b.Gender != "Female" && b.Gender != "Any":
		return nil, bad("Gender must be Male, Female or Any")
	case b.Beds < 1 || b.Beds > 200:
		return nil, bad("Beds must be between 1 and 200")
	case b.Status != "Active" && b.Status != "Inactive":
		return nil, bad("Status must be Active or Inactive")
	case len(b.Near) == 0:
		return nil, bad("Choose at least one property the unit serves (the first carries its cost when nobody stays)")
	case b.Phone != "" && !reMobile.MatchString(strings.ReplaceAll(b.Phone, " ", "")):
		return nil, bad("Landlord contact must be a 10-digit mobile number")
	}
	if b.Fixed == nil {
		b.Fixed = []hstItem{}
	}
	for i := range b.Fixed {
		b.Fixed[i].Label = strings.TrimSpace(b.Fixed[i].Label)
		if b.Fixed[i].Label == "" || b.Fixed[i].Amount < 0 {
			return nil, bad("Each fixed item needs a name and an amount of 0 or more")
		}
		b.Fixed[i].Amount = calc.Round2(b.Fixed[i].Amount)
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var out string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		cl, err := tx.C("cluster").FindOne(ctx, bson.M{"_id": b.ClusterID})
		if err != nil {
			return err
		}
		if cl == nil {
			return bad("Choose the unit's cluster")
		}
		vis, err := hstClusters(ctx, tx, c)
		if err != nil {
			return err
		}
		if vis != nil && !vis[b.ClusterID] {
			return forbidden("That cluster is outside your access")
		}
		seen := map[string]bool{}
		for _, l := range b.Near {
			loc, err := tx.C("location").FindOne(ctx, bson.M{"_id": l})
			if err != nil {
				return err
			}
			if loc == nil || seen[l] {
				return bad("A property the unit serves is unknown or listed twice")
			}
			seen[l] = true
		}
		dup := bson.M{"name": bson.M{"$regex": "^" + regexpQuote(b.Name) + "$", "$options": "i"}}
		if id != "" {
			dup["_id"] = bson.M{"$ne": id}
		}
		if ok, err := tx.C(store.HstUnitColl).Exists(ctx, dup); err != nil {
			return err
		} else if ok {
			return bad("There is already a unit called %s", b.Name)
		}
		doc := bson.M{"name": b.Name, "kind": b.Kind, "address": nullIfEmpty(b.Address), "cluster_id": b.ClusterID, "near_ids": b.Near, "gender": b.Gender,
			"beds": b.Beds, "landlord": nullIfEmpty(b.Landlord), "landlord_phone": nullIfEmpty(b.Phone), "status": b.Status, "fixed_items": b.Fixed,
			"note": nullIfEmpty(b.Note)}
		if id == "" {
			doc["created_by"] = c.Name + " (" + c.EmployeeID + ")"
			nid, err := tx.C(store.HstUnitColl).Insert(ctx, doc)
			if err != nil {
				return err
			}
			out = fmt.Sprint(nid)
			return audit(ctx, tx, c, "Accommodation unit added: "+b.Name, store.HstUnitColl, out, nil, doc)
		}
		before, err := tx.C(store.HstUnitColl).Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		if before == nil {
			return notFound("Accommodation unit not found")
		}
		if vis != nil && !vis[str(before["cluster_id"])] {
			return forbidden("This unit is outside your access")
		}
		// Beds and status cannot drop below the people staying now or booked later.
		day := store.Today()
		f := hstOverlapping(day, "")
		f["unit_id"] = id
		live, err := hstStaysOf(ctx, tx, f)
		if err != nil {
			return err
		}
		for _, x := range live {
			if x.Bed > b.Beds {
				return bad("Bed %d is taken (check-in %s): check that person out or move them to a lower bed first", x.Bed, longDate(x.In))
			}
		}
		if b.Status == "Inactive" && len(live) > 0 {
			return bad("%d %s staying or booked here: check them out before marking the unit inactive", len(live), map[bool]string{true: "person is", false: "people are"}[len(live) == 1])
		}
		doc["updated_at"] = tx.Now()
		if _, err := tx.C(store.HstUnitColl).Update(ctx, bson.M{"_id": id}, bson.M{"$set": doc}); err != nil {
			return err
		}
		out = id
		return audit(ctx, tx, c, "Accommodation unit changed: "+b.Name, store.HstUnitColl, id, before, doc)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": out}, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------- Leases ----------

type hstLeaseBody struct {
	Start       string  `json:"start"`
	End         string  `json:"end"`
	NoticeDays  int     `json:"noticeDays"`
	Rent        float64 `json:"rent"`
	Deposit     float64 `json:"deposit"`
	Maintenance float64 `json:"maintenance"`
	IncPct      float64 `json:"incPct"`
	IncOn       string  `json:"incOn"`
	Note        string  `json:"note"`
}

// hstLeaseSave adds a lease to a unit (POST /units/{id}/leases) or corrects one (PUT /leases/{id}). Leases are never
// deleted: an ended lease stays as the unit's history.
func (s *Server) hstLeaseSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.setup", 2); err != nil {
		return nil, err
	}
	var b hstLeaseBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	switch {
	case !isDate(b.Start):
		return nil, bad("Give the lease start date")
	case b.End != "" && (!isDate(b.End) || b.End < b.Start):
		return nil, bad("The end date must be on or after the start (leave it empty for an ongoing lease)")
	case b.Rent <= 0:
		return nil, bad("Give the monthly rent")
	case b.Deposit < 0 || b.Maintenance < 0 || b.NoticeDays < 0 || b.NoticeDays > 365:
		return nil, bad("Deposit, maintenance and notice period cannot be negative (notice at most 365 days)")
	case b.IncPct < 0 || b.IncPct > 100:
		return nil, bad("The yearly increase must be between 0 and 100%%")
	case b.IncPct > 0 && (!isDate(b.IncOn) || b.IncOn <= b.Start):
		return nil, bad("Give the date of the first rent increase, after the lease start")
	}
	if b.IncPct == 0 {
		b.IncOn = ""
	}
	ctx := r.Context()
	var out string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		unitID, leaseID := r.PathValue("id"), ""
		var before store.Row
		if strings.HasPrefix(r.URL.Path, "/api/hst/leases/") {
			leaseID = unitID
			var err error
			if before, err = tx.C(store.HstLeaseColl).FindOne(ctx, bson.M{"_id": leaseID}); err != nil {
				return err
			}
			if before == nil {
				return notFound("Lease not found")
			}
			unitID = str(before["unit_id"])
		}
		u, err := hstUnitIn(ctx, tx, c, unitID)
		if err != nil {
			return err
		}
		if _, err := tx.C(store.HstUnitColl).Lock(ctx, bson.M{"_id": u.ID}); err != nil {
			return err
		}
		ls, err := hstLeases(ctx, tx, []string{u.ID})
		if err != nil {
			return err
		}
		nl := hst.Lease{Start: b.Start, End: b.End}
		for _, l := range ls[u.ID] {
			if l.ID != leaseID && hst.LeasesOverlap(nl, l.lease()) {
				end := "ongoing"
				if l.End != "" {
					end = "to " + longDate(l.End)
				}
				return bad("This overlaps the lease from %s (%s). End that lease first: a unit has one lease at a time", longDate(l.Start), end)
			}
		}
		doc := bson.M{"start": b.Start, "end": nullIfEmpty(b.End), "notice_days": b.NoticeDays, "rent": b.Rent, "deposit": b.Deposit, "maintenance": b.Maintenance,
			"inc_pct": b.IncPct, "inc_on": nullIfEmpty(b.IncOn), "note": nullIfEmpty(b.Note)}
		if leaseID == "" {
			doc["unit_id"] = u.ID
			doc["created_by"] = c.Name + " (" + c.EmployeeID + ")"
			nid, err := tx.C(store.HstLeaseColl).Insert(ctx, doc)
			if err != nil {
				return err
			}
			out = fmt.Sprint(nid)
			return audit(ctx, tx, c, "Lease added: "+u.Name+" from "+longDate(b.Start), store.HstLeaseColl, out, nil, doc)
		}
		doc["updated_at"] = tx.Now()
		if _, err := tx.C(store.HstLeaseColl).Update(ctx, bson.M{"_id": leaseID}, bson.M{"$set": doc}); err != nil {
			return err
		}
		out = leaseID
		return audit(ctx, tx, c, "Lease changed: "+u.Name, store.HstLeaseColl, leaseID, before, doc)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": out}, nil
}

// ---------- Month costs ----------

// hstCostKeys are the variable items HR enters each month.
var hstCostKeys = []string{"electricity", "water", "wifi", "housekeeping", "other"}

func (s *Server) hstCostSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.setup", 2); err != nil {
		return nil, err
	}
	ym := r.PathValue("ym")
	if !isYm(ym) {
		return nil, bad("Month must be YYYY-MM")
	}
	var b map[string]float64
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	items := bson.M{}
	total := 0.0
	known := map[string]bool{}
	for _, k := range hstCostKeys {
		known[k] = true
	}
	for k, v := range b {
		if !known[k] {
			return nil, bad("Unknown cost item %s", k)
		}
		if v < 0 || math.IsNaN(v) {
			return nil, bad("Amounts cannot be negative")
		}
		items[k] = calc.Round2(v)
		total += calc.Round2(v)
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		u, err := hstUnitIn(ctx, tx, c, r.PathValue("id"))
		if err != nil {
			return err
		}
		this := bson.M{"unit_id": u.ID, "ym": ym}
		before, err := tx.C(store.HstCostColl).FindOne(ctx, this)
		if err != nil {
			return err
		}
		if _, err := tx.C(store.HstCostColl).Upsert(ctx, this, bson.M{"items": items, "total": calc.Round2(total), "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Accommodation costs for "+ymLabel(ym)+": "+u.Name, store.HstCostColl, u.ID+"/"+ym, before, items)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"total": calc.Round2(total)}, nil
}

// ---------- Allocation ----------

type hstSuggestion struct {
	hstUnitOut
	FreeBeds []int  `json:"freeBeds"`
	Rank     int    `json:"rank"` // position of the person's property among those the unit serves (1 = nearest); 0 = not listed
	Why      string `json:"why"`
}

// hstFreeBeds lists the beds of u with no stay overlapping [from, to) (to "" = open), ignoring stay skip.
func hstFreeBeds(u hstUnitRec, stays []hstStayRec, from, to, skip string) []int {
	taken := map[int]bool{}
	for _, x := range stays {
		if x.UnitID == u.ID && x.ID != skip && hst.Overlaps(x.In, x.Out, from, to) {
			taken[x.Bed] = true
		}
	}
	out := []int{}
	for b := 1; b <= u.Beds; b++ {
		if !taken[b] {
			out = append(out, b)
		}
	}
	return out
}

func hstGenderOK(unit, person string) bool {
	return unit == "Any" || person == "" || unit == person
}

// hstRankOf is the position of the property locID among those unit u serves (1 = nearest); 0 = not listed.
func hstRankOf(u hstUnitRec, locID string) int {
	for i, l := range u.Near {
		if l == locID {
			return i + 1
		}
	}
	return 0
}

// hstBefore orders suggestions: the unit serving the person's property first (by its rank, unlisted last), then
// the most free beds, then the name. The automatic allocation (hst_auto.go) takes the first.
func hstBefore(ri, fi int, ni string, rj, fj int, nj string) bool {
	if ri == 0 {
		ri = 1 << 20
	}
	if rj == 0 {
		rj = 1 << 20
	}
	if ri != rj {
		return ri < rj
	}
	if fi != fj {
		return fi > fj
	}
	return ni < nj
}

func (s *Server) hstSuggest(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	id := r.URL.Query().Get("employeeId")
	day := r.URL.Query().Get("date")
	if day == "" {
		day = store.Today()
	}
	if !isDate(day) {
		return nil, bad("Date must be YYYY-MM-DD")
	}
	if err := c.MustInScope(ctx, q, id); err != nil {
		return nil, err
	}
	list, err := loadEmps(ctx, q, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("Employee not found")
	}
	e := list[0]
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	units, err := hstUnits(ctx, q, bson.M{"cluster_id": e.ClusterID, "status": "Active"})
	if err != nil {
		return nil, err
	}
	ids := hstUnitIDs(units)
	leases, err := hstLeases(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	f := hstOverlapping(day, "")
	f["unit_id"] = bson.M{"$in": ids}
	stays, err := hstStaysOf(ctx, q, f)
	if err != nil {
		return nil, err
	}
	out := []hstSuggestion{}
	other := 0
	for _, u := range units {
		free := hstFreeBeds(u, stays, day, "", "")
		if len(free) == 0 {
			continue
		}
		if !hstGenderOK(u.Gender, e.Gender) {
			other++
			continue
		}
		sg := hstSuggestion{hstUnitOut: hstUnitView(u, p, leases[u.ID], stays, day), FreeBeds: free, Rank: hstRankOf(u, e.LocID)}
		switch {
		case sg.Rank == 1:
			sg.Why = "Serves " + e.Location + " first"
		case sg.Rank > 1:
			sg.Why = fmt.Sprintf("Serves %s (%d of %d properties)", e.Location, sg.Rank, len(u.Near))
		default:
			sg.Why = "In " + p.cluster(e.ClusterID) + "; does not list " + e.Location
		}
		out = append(out, sg)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return hstBefore(out[i].Rank, len(out[i].FreeBeds), out[i].Name, out[j].Rank, len(out[j].FreeBeds), out[j].Name)
	})
	cur, err := hstStaysOf(ctx, q, bson.M{"employee_id": id, "check_out": nil})
	if err != nil {
		return nil, err
	}
	var current any
	if len(cur) > 0 {
		x := cur[len(cur)-1]
		un, _ := q.C(store.HstUnitColl).FindOne(ctx, bson.M{"_id": x.UnitID})
		current = map[string]any{"stay": x, "unit": str(un["name"])}
	}
	return map[string]any{"employee": map[string]any{"id": e.ID, "name": e.Name, "gender": e.Gender, "location": e.Location, "locationId": e.LocID,
		"cluster": p.cluster(e.ClusterID), "city": p.city(e.ClusterID), "status": e.Status}, "date": day, "units": out, "otherGender": other, "current": current}, nil
}

type hstStayBody struct {
	EmployeeID string `json:"employeeId"`
	UnitID     string `json:"unitId"`
	Bed        int    `json:"bed"`
	CheckIn    string `json:"checkIn"`
	CheckOut   string `json:"checkOut"`
	Note       string `json:"note"`
}

// hstBook checks a stay of emp in unit u on bed (0: the first free bed) over [in, out) against the unit's other
// stays and the person's, leaving out stay skip, and returns the bed. The caller holds the unit's and the person's
// locks, so two bookings never pass the check together.
func hstBook(ctx context.Context, tx store.Q, u *hstUnitRec, empID string, bed int, in, out, skip string) (int, error) {
	mine, err := hstStaysOf(ctx, tx, bson.M{"employee_id": empID})
	if err != nil {
		return 0, err
	}
	for _, x := range mine {
		if x.ID != skip && hst.Overlaps(x.In, x.Out, in, out) {
			un, _ := tx.C(store.HstUnitColl).FindOne(ctx, bson.M{"_id": x.UnitID})
			to := "still staying"
			if x.Out != "" {
				to = "until " + longDate(x.Out)
			}
			return 0, bad("%s already stays at %s from %s (%s): check them out there first", empID, str(un["name"]), longDate(x.In), to)
		}
	}
	stays, err := hstStaysOf(ctx, tx, bson.M{"unit_id": u.ID})
	if err != nil {
		return 0, err
	}
	free := hstFreeBeds(*u, stays, in, out, skip)
	if bed == 0 {
		if len(free) == 0 {
			return 0, bad("%s has no free bed from %s", u.Name, longDate(in))
		}
		return free[0], nil
	}
	if bed < 1 || bed > u.Beds {
		return 0, bad("%s has beds 1 to %d", u.Name, u.Beds)
	}
	for _, f := range free {
		if f == bed {
			return bed, nil
		}
	}
	for _, x := range stays {
		if x.ID != skip && x.Bed == bed && hst.Overlaps(x.In, x.Out, in, out) {
			return 0, bad("Bed %d of %s is taken by %s from %s: choose another bed or date", bed, u.Name, x.EmployeeID, longDate(x.In))
		}
	}
	return 0, bad("Bed %d of %s is taken", bed, u.Name)
}

// hstPerson loads and checks the person a stay is for: in the caller's scope, not left, gender matching the unit.
func hstPerson(ctx context.Context, tx store.Q, c *Ctx, id string, u *hstUnitRec) (*emp, error) {
	if err := c.MustInScope(ctx, tx, id); err != nil {
		return nil, err
	}
	list, err := loadEmps(ctx, tx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("Employee not found")
	}
	e := list[0]
	if !hstGenderOK(u.Gender, e.Gender) {
		return nil, bad("%s is for %s staff; %s is recorded as %s", u.Name, strings.ToLower(u.Gender), e.Name, strings.ToLower(e.Gender))
	}
	return e, nil
}

func (s *Server) hstCheckIn(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 2); err != nil {
		return nil, err
	}
	var b hstStayBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	if !isDate(b.CheckIn) {
		return nil, bad("Give the check-in date")
	}
	if b.CheckOut != "" && (!isDate(b.CheckOut) || b.CheckOut <= b.CheckIn) {
		return nil, bad("The check-out date must be after the check-in date")
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		u, err := hstUnitIn(ctx, tx, c, b.UnitID)
		if err != nil {
			return err
		}
		if u.Status != "Active" {
			return bad("%s is inactive", u.Name)
		}
		e, err := hstPerson(ctx, tx, c, b.EmployeeID, u)
		if err != nil {
			return err
		}
		if e.Status == "Inactive" {
			return bad("%s has left", e.Name)
		}
		if _, err := tx.C(store.HstUnitColl).Lock(ctx, bson.M{"_id": u.ID}); err != nil {
			return err
		}
		if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": e.ID}); err != nil {
			return err
		}
		bed, err := hstBook(ctx, tx, u, e.ID, b.Bed, b.CheckIn, b.CheckOut, "")
		if err != nil {
			return err
		}
		doc := bson.M{"unit_id": u.ID, "employee_id": e.ID, "bed": bed, "check_in": b.CheckIn, "check_out": nullIfEmpty(b.CheckOut), "note": nullIfEmpty(b.Note),
			"created_by": c.Name + " (" + c.EmployeeID + ")"}
		id, err := tx.C(store.HstStayColl).Insert(ctx, doc)
		if err != nil {
			return err
		}
		out = map[string]any{"id": fmt.Sprint(id), "bed": bed, "unit": u.Name}
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": e.ID, "hst_wait": bson.M{"$ne": nil}}, bson.M{"$set": bson.M{"hst_wait": nil}}); err != nil {
			return err // assigned by hand: off the "Waiting for a bed" list (hst_auto.go)
		}
		return audit(ctx, tx, c, fmt.Sprintf("Checked in to %s, bed %d, from %s", u.Name, bed, longDate(b.CheckIn)), store.HstStayColl, fmt.Sprint(id), nil, doc)
	})
	return out, err
}

func (s *Server) hstCheckOut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 2); err != nil {
		return nil, err
	}
	var b struct {
		Date   string `json:"date"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if !isDate(b.Date) {
		return nil, bad("Give the check-out date")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		row, err := tx.C(store.HstStayColl).Lock(ctx, bson.M{"_id": r.PathValue("id")})
		if err != nil {
			return err
		}
		if row == nil {
			return notFound("Stay not found")
		}
		x := hstStayOf(row)
		u, err := hstUnitIn(ctx, tx, c, x.UnitID)
		if err != nil {
			return err
		}
		if err := c.MustInScope(ctx, tx, x.EmployeeID); err != nil {
			return err
		}
		switch {
		case x.Out != "":
			return bad("Already checked out on %s", longDate(x.Out))
		case b.Date <= x.In:
			return bad("The check-out date must be after the check-in (%s)", longDate(x.In))
		}
		set := bson.M{"check_out": b.Date, "out_reason": nullIfEmpty(b.Reason), "out_by": c.Name + " (" + c.EmployeeID + ")", "out_at": tx.Now()}
		if _, err := tx.C(store.HstStayColl).Update(ctx, bson.M{"_id": x.ID}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Checked out of "+u.Name+" on "+longDate(b.Date), store.HstStayColl, x.ID, row, set)
	})
	return nil, err
}

// hstStayEdit corrects a stay's dates, bed or note (an entry made wrongly). The same checks as a check-in apply.
func (s *Server) hstStayEdit(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 2); err != nil {
		return nil, err
	}
	var b hstStayBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Note = strings.TrimSpace(b.Note)
	if !isDate(b.CheckIn) {
		return nil, bad("Give the check-in date")
	}
	if b.CheckOut != "" && (!isDate(b.CheckOut) || b.CheckOut <= b.CheckIn) {
		return nil, bad("The check-out date must be after the check-in date")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		row, err := tx.C(store.HstStayColl).Lock(ctx, bson.M{"_id": r.PathValue("id")})
		if err != nil {
			return err
		}
		if row == nil {
			return notFound("Stay not found")
		}
		x := hstStayOf(row)
		u, err := hstUnitIn(ctx, tx, c, x.UnitID)
		if err != nil {
			return err
		}
		if _, err := hstPerson(ctx, tx, c, x.EmployeeID, u); err != nil {
			return err
		}
		if _, err := tx.C(store.HstUnitColl).Lock(ctx, bson.M{"_id": u.ID}); err != nil {
			return err
		}
		if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": x.EmployeeID}); err != nil {
			return err
		}
		bed := b.Bed
		if bed == 0 {
			bed = x.Bed
		}
		if bed, err = hstBook(ctx, tx, u, x.EmployeeID, bed, b.CheckIn, b.CheckOut, x.ID); err != nil {
			return err
		}
		set := bson.M{"bed": bed, "check_in": b.CheckIn, "check_out": nullIfEmpty(b.CheckOut), "note": nullIfEmpty(b.Note)}
		if _, err := tx.C(store.HstStayColl).Update(ctx, bson.M{"_id": x.ID}, bson.M{"$set": set}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Stay corrected: "+x.EmployeeID+" at "+u.Name, store.HstStayColl, x.ID, row, set)
	})
	return nil, err
}

// hstStays is a person's stay history (the employee file), newest first, with what needs doing.
func (s *Server) hstStays(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	id := r.URL.Query().Get("employeeId")
	if err := c.MustInScope(ctx, q, id); err != nil {
		return nil, err
	}
	stays, err := hstStaysOf(ctx, q, bson.M{"employee_id": id})
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, x := range stays {
		ids = append(ids, x.UnitID)
	}
	names := hstNames(ctx, q, store.HstUnitColl, ids)
	day := store.Today()
	out := []map[string]any{}
	for i := len(stays) - 1; i >= 0; i-- {
		x := stays[i]
		out = append(out, map[string]any{"stay": x, "unit": names[x.UnitID], "now": x.on(day)})
	}
	att, err := hstAttentionList(ctx, q, bson.M{"employee_id": id})
	if err != nil {
		return nil, err
	}
	return map[string]any{"stays": out, "attention": att, "canEdit": c.Level("hst.allot") >= 2, "today": day}, nil
}

// hstAttn is an open stay HR should check out: the person left or is leaving, or moved out of the unit's cluster.
type hstAttn struct {
	Stay     hstStayRec `json:"stay"`
	Unit     string     `json:"unit"`
	Name     string     `json:"name"`
	Kind     string     `json:"kind"` // exit | transfer
	Why      string     `json:"why"`
	Suggest  string     `json:"suggest"` // a check-out date to start from
	Location string     `json:"location"`
}

func hstAttentionList(ctx context.Context, q store.Q, filter bson.M) ([]hstAttn, error) {
	filter["check_out"] = nil
	stays, err := hstStaysOf(ctx, q, filter)
	if err != nil || len(stays) == 0 {
		return []hstAttn{}, err
	}
	who, unitIDs := []any{}, []string{}
	for _, x := range stays {
		who = append(who, x.EmployeeID)
		unitIDs = append(unitIDs, x.UnitID)
	}
	emps, err := loadEmps(ctx, q, bson.M{"_id": bson.M{"$in": who}})
	if err != nil {
		return nil, err
	}
	byID := map[string]*emp{}
	for _, e := range emps {
		byID[e.ID] = e
	}
	units, err := hstUnits(ctx, q, bson.M{"_id": bson.M{"$in": unitIDs}})
	if err != nil {
		return nil, err
	}
	unitBy := map[string]hstUnitRec{}
	for _, u := range units {
		unitBy[u.ID] = u
	}
	today := store.Today()
	out := []hstAttn{}
	for _, x := range stays {
		e, u := byID[x.EmployeeID], unitBy[x.UnitID]
		if e == nil {
			continue
		}
		a := hstAttn{Stay: x, Unit: u.Name, Name: e.Name, Location: e.Location}
		switch {
		case e.Status == "Inactive":
			a.Kind, a.Why, a.Suggest = "exit", "Has left", today
			if e.LWD != nil {
				a.Why, a.Suggest = "Left on "+longDate(*e.LWD), policy.AddDays(*e.LWD, 1)
			}
		case e.LWD != nil && *e.LWD != "":
			a.Kind, a.Why, a.Suggest = "exit", "Leaving: last working day "+longDate(*e.LWD), policy.AddDays(*e.LWD, 1)
		case e.ClusterID != u.ClusterID:
			a.Kind, a.Why, a.Suggest = "transfer", "Moved to "+e.Location+", outside this unit's cluster", today
		default:
			continue
		}
		if a.Suggest <= x.In {
			a.Suggest = policy.AddDays(x.In, 1)
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Server) hstAttention(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	vis, err := hstClusters(ctx, q, c)
	if err != nil {
		return nil, err
	}
	f := bson.M{}
	if vis != nil {
		units, err := hstUnits(ctx, q, hstUnitFilter(vis))
		if err != nil {
			return nil, err
		}
		f["unit_id"] = bson.M{"$in": hstUnitIDs(units)}
	}
	list, err := hstAttentionList(ctx, q, f)
	if err != nil {
		return nil, err
	}
	return map[string]any{"list": list}, nil
}

// ---------- Occupancy board ----------

type hstBoardRow struct {
	City       string  `json:"city"`
	Cluster    string  `json:"cluster"`
	ClusterID  string  `json:"clusterId"`
	Units      int     `json:"units"`
	Beds       int     `json:"beds"`
	Filled     int     `json:"filled"`
	Free       int     `json:"free"`
	FreeMale   int     `json:"freeMale"`
	FreeFemale int     `json:"freeFemale"`
	FreeAny    int     `json:"freeAny"`
	Cost       float64 `json:"cost"`
	Allocated  float64 `json:"allocated"`
	Empty      float64 `json:"empty"`
	Ending     int     `json:"ending"`
}

type hstEnding struct {
	UnitID     string `json:"unitId"`
	Unit       string `json:"unit"`
	Cluster    string `json:"cluster"`
	City       string `json:"city"`
	End        string `json:"end"`
	DaysLeft   int    `json:"daysLeft"`
	NoticeDays int    `json:"noticeDays"`
	NoticeBy   string `json:"noticeBy"`
}

func (s *Server) hstBoardData(ctx context.Context, c *Ctx, ym, day string) ([]hstBoardRow, hstBoardRow, []hstEnding, error) {
	q := s.st.DB
	var total hstBoardRow
	vis, err := hstClusters(ctx, q, c)
	if err != nil {
		return nil, total, nil, err
	}
	units, err := hstUnits(ctx, q, hstUnitFilter(vis))
	if err != nil {
		return nil, total, nil, err
	}
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, total, nil, err
	}
	set, err := loadHstSettings(ctx, q)
	if err != nil {
		return nil, total, nil, err
	}
	m, err := hstSplit(ctx, q, ym, units)
	if err != nil {
		return nil, total, nil, err
	}
	ids := hstUnitIDs(units)
	leases, err := hstLeases(ctx, q, ids)
	if err != nil {
		return nil, total, nil, err
	}
	f := hstOverlapping(day, policy.AddDays(day, 1))
	f["unit_id"] = bson.M{"$in": ids}
	stays, err := hstStaysOf(ctx, q, f)
	if err != nil {
		return nil, total, nil, err
	}
	rows := map[string]*hstBoardRow{}
	ending := []hstEnding{}
	horizon := policy.AddDays(day, set.BoardDays)
	for _, u := range units {
		br := rows[u.ClusterID]
		if br == nil {
			br = &hstBoardRow{City: p.city(u.ClusterID), Cluster: p.cluster(u.ClusterID), ClusterID: u.ClusterID}
			rows[u.ClusterID] = br
		}
		um := m.Units[u.ID]
		br.Cost += um.Cost
		br.Allocated += um.Allocated
		br.Empty += um.Empty
		// A lease ending within the window with no lease after it.
		ls := leases[u.ID]
		for i, l := range ls {
			if l.End == "" || l.End < day || l.End > horizon || i < len(ls)-1 {
				continue
			}
			e := hstEnding{UnitID: u.ID, Unit: u.Name, Cluster: br.Cluster, City: br.City, End: l.End, DaysLeft: policy.DayDiff(day, l.End), NoticeDays: l.NoticeDays}
			if l.NoticeDays > 0 {
				e.NoticeBy = policy.AddDays(l.End, -l.NoticeDays)
			}
			ending = append(ending, e)
			br.Ending++
		}
		if u.Status != "Active" {
			continue
		}
		br.Units++
		br.Beds += u.Beds
		v := hstUnitView(u, p, ls, stays, day)
		br.Filled += v.Filled
		br.Free += v.Free
		switch u.Gender {
		case "Male":
			br.FreeMale += v.Free
		case "Female":
			br.FreeFemale += v.Free
		default:
			br.FreeAny += v.Free
		}
	}
	out := make([]hstBoardRow, 0, len(rows))
	for _, br := range rows {
		br.Cost, br.Allocated, br.Empty = calc.Round2(br.Cost), calc.Round2(br.Allocated), calc.Round2(br.Empty)
		out = append(out, *br)
		total.Units += br.Units
		total.Beds += br.Beds
		total.Filled += br.Filled
		total.Free += br.Free
		total.FreeMale += br.FreeMale
		total.FreeFemale += br.FreeFemale
		total.FreeAny += br.FreeAny
		total.Cost += br.Cost
		total.Allocated += br.Allocated
		total.Empty += br.Empty
		total.Ending += br.Ending
	}
	total.Cost, total.Allocated, total.Empty = calc.Round2(total.Cost), calc.Round2(total.Allocated), calc.Round2(total.Empty)
	sort.Slice(out, func(i, j int) bool {
		if out[i].City != out[j].City {
			return out[i].City < out[j].City
		}
		return out[i].Cluster < out[j].Cluster
	})
	sort.Slice(ending, func(i, j int) bool { return ending[i].End < ending[j].End })
	return out, total, ending, nil
}

func hstBoardArgs(r *http.Request) (string, string, error) {
	ym, day := r.URL.Query().Get("ym"), store.Today()
	if ym == "" {
		ym = day[:7]
	}
	if !isYm(ym) {
		return "", "", bad("Month must be YYYY-MM")
	}
	return ym, day, nil
}

func (s *Server) hstBoard(r *http.Request, c *Ctx) (any, error) {
	if err := hstCan(c); err != nil {
		return nil, err
	}
	ym, day, err := hstBoardArgs(r)
	if err != nil {
		return nil, err
	}
	rows, total, ending, err := s.hstBoardData(r.Context(), c, ym, day)
	if err != nil {
		return nil, err
	}
	set, _ := loadHstSettings(r.Context(), s.st.DB)
	return map[string]any{"ym": ym, "on": day, "rows": rows, "total": total, "ending": ending, "boardDays": set.BoardDays}, nil
}

func (s *Server) hstBoardSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := hstCan(c); err != nil {
		return "", nil, nil, err
	}
	ym, day, err := hstBoardArgs(r)
	if err != nil {
		return "", nil, nil, err
	}
	rows, total, ending, err := s.hstBoardData(r.Context(), c, ym, day)
	if err != nil {
		return "", nil, nil, err
	}
	cols := []string{"City", "Cluster", "Units", "Beds", "Filled", "Free", "Free (male)", "Free (female)", "Free (any)", "Month cost", "Allocated to staff",
		"Empty-unit cost", "Leases ending soon", "Next lease end"}
	next := map[string]string{}
	for _, e := range ending {
		if next[e.Cluster+"|"+e.City] == "" {
			next[e.Cluster+"|"+e.City] = e.End
		}
	}
	out := [][]any{}
	line := func(b hstBoardRow, end string) []any {
		return []any{b.City, b.Cluster, b.Units, b.Beds, b.Filled, b.Free, b.FreeMale, b.FreeFemale, b.FreeAny, b.Cost, b.Allocated, b.Empty, b.Ending, end}
	}
	for _, b := range rows {
		out = append(out, line(b, next[b.Cluster+"|"+b.City]))
	}
	total.City, total.Cluster = "Total", ""
	out = append(out, line(total, ""))
	return "accommodation-occupancy-" + ym + ".xlsx", cols, out, nil
}

// hstSplitGet is the month split per unit and person, and the empty-unit cost per property.
func (s *Server) hstSplitGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.setup", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	ym := r.URL.Query().Get("ym")
	if ym == "" {
		ym = store.Today()[:7]
	}
	vis, err := hstClusters(ctx, q, c)
	if err != nil {
		return nil, err
	}
	units, err := hstUnits(ctx, q, hstUnitFilter(vis))
	if err != nil {
		return nil, err
	}
	m, err := hstSplit(ctx, q, ym, units)
	if err != nil {
		return nil, err
	}
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	who := []string{}
	for e := range m.Shares {
		who = append(who, e)
	}
	names := hstEmpNames(ctx, q, who)
	rows := []map[string]any{}
	for _, u := range units {
		um := m.Units[u.ID]
		people := []map[string]any{}
		for e, v := range um.Shares {
			people = append(people, map[string]any{"employeeId": e, "name": names[e], "days": um.Days[e], "share": v})
		}
		sort.Slice(people, func(i, j int) bool { return str(people[i]["employeeId"]) < str(people[j]["employeeId"]) })
		rows = append(rows, map[string]any{"unitId": u.ID, "unit": u.Name, "cluster": p.cluster(u.ClusterID), "city": p.city(u.ClusterID), "home": p.location(u.home()),
			"cost": um.Cost, "allocated": um.Allocated, "empty": um.Empty, "emptyDays": um.EmptyDays, "people": people})
	}
	empty := []map[string]any{}
	for l, v := range m.Empty {
		empty = append(empty, map[string]any{"locationId": l, "location": p.location(l), "empty": v})
	}
	sort.Slice(empty, func(i, j int) bool { return str(empty[i]["location"]) < str(empty[j]["location"]) })
	return map[string]any{"ym": ym, "method": m.Method, "days": m.Days, "units": rows, "empty": empty}, nil
}

// ---------- Reminders (the maintenance job) ----------

// hstRecipients are the people who get a reminder about right: active HR Admins and the active holders of a role
// with that right at Edit, each with the role view the message opens in.
func hstRecipients(ctx context.Context, q store.Q, right string) (map[string]string, error) {
	roles, err := q.C("role_right").Distinct(ctx, "role_name", bson.M{"right_id": right, "level": bson.M{"$gte": 2}})
	if err != nil {
		return nil, err
	}
	roles = append(roles, adminRole)
	maps, err := q.C("user_role_map").Find(ctx, bson.M{"active": true, "role_name": bson.M{"$in": roles}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	ids := []any{}
	for _, m := range maps {
		ids = append(ids, m["employee_id"])
	}
	live, err := q.C("employee").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": ids}, "status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for _, id := range live {
		ok[str(id)] = true
	}
	out := map[string]string{}
	for _, m := range maps {
		if id := str(m["employee_id"]); ok[id] && (out[id] == "" || str(m["role_name"]) == adminRole) {
			out[id] = str(m["role_name"])
		}
	}
	return out, nil
}

// HstSweep sends the accommodation bell reminders due at now, once each: a lease ending soon (and the last week to
// give notice), a rent increase coming up, and an open stay of someone who left, is leaving or moved out of the
// unit's cluster. It returns how many messages it sent.
func (s *Server) HstSweep(ctx context.Context, now time.Time) (int, error) {
	q := s.st.DB
	set, err := loadHstSettings(ctx, q)
	if err != nil {
		return 0, err
	}
	day := store.DateOf(now)
	units, err := hstUnits(ctx, q, nil)
	if err != nil || len(units) == 0 {
		return 0, err
	}
	unitBy := map[string]hstUnitRec{}
	for _, u := range units {
		unitBy[u.ID] = u
	}
	leases, err := hstLeases(ctx, q, hstUnitIDs(units))
	if err != nil {
		return 0, err
	}
	type msg struct{ key, text, link string }
	setup, allot := []msg{}, []msg{}
	for uid, ls := range leases {
		u := unitBy[uid]
		link := "/accommodation?unit=" + uid
		for i, l := range ls {
			if l.End != "" && l.End >= day && i == len(ls)-1 { // the unit's last lease is ending
				left := policy.DayDiff(day, l.End)
				if left <= set.LeaseDays {
					setup = append(setup, msg{"hst:lease-end:" + l.ID + ":" + l.End, fmt.Sprintf("The lease of %s ends on %s (%d days). Renew it or plan the move-out.", u.Name, longDate(l.End), left), link})
				}
				if l.NoticeDays > 0 && left <= l.NoticeDays+7 && left >= l.NoticeDays {
					setup = append(setup, msg{"hst:lease-notice:" + l.ID + ":" + l.End, fmt.Sprintf("Last week to give the landlord of %s notice: %d days' notice before the lease ends on %s (by %s).",
						u.Name, l.NoticeDays, longDate(l.End), longDate(policy.AddDays(l.End, -l.NoticeDays))), link})
				}
			}
			hl := l.lease()
			if l.End != "" && l.End < day {
				continue
			}
			// NextIncrease is always after day (the first increase is after the lease start).
			if next := hl.NextIncrease(day); next != "" && policy.DayDiff(day, next) <= set.IncDays {
				setup = append(setup, msg{"hst:inc:" + l.ID + ":" + next, fmt.Sprintf("The rent of %s goes up %s%% on %s: %s a month from %s.", u.Name, trimNum(l.IncPct), longDate(next),
					calc.Inr(hl.RentOn(next)), calc.Inr(hl.RentOn(policy.AddDays(next, -1)))), link})
			}
		}
	}
	att, err := hstAttentionList(ctx, q, bson.M{})
	if err != nil {
		return 0, err
	}
	for _, a := range att {
		allot = append(allot, msg{"hst:checkout:" + a.Stay.ID + ":" + a.Kind, fmt.Sprintf("%s (%s) is still checked in at %s. %s: check them out.", a.Name, a.Stay.EmployeeID, a.Unit, a.Why),
			"/accommodation?tab=stays"})
	}
	sent := 0
	for _, g := range []struct {
		right string
		msgs  []msg
	}{{"hst.setup", setup}, {"hst.allot", allot}} {
		if len(g.msgs) == 0 {
			continue
		}
		to, err := hstRecipients(ctx, q, g.right)
		if err != nil {
			return sent, err
		}
		for who, view := range to {
			for _, m := range g.msgs {
				key := m.key + ":" + who
				if done, err := q.C("notice").Exists(ctx, bson.M{"ref": key}); err != nil {
					return sent, err
				} else if done {
					continue
				}
				if _, err := q.C("notice").Insert(ctx, bson.D{{Key: "to_employee_id", Value: who}, {Key: "from_employee_id", Value: nil}, {Key: "kind", Value: "accommodation"},
					{Key: "text", Value: m.text}, {Key: "link", Value: m.link}, {Key: "view", Value: view}, {Key: "ref", Value: []string{key}}}); err != nil {
					return sent, err
				}
				sent++
			}
		}
	}
	return sent, nil
}

// hstSweepLogged runs the reminders from the maintenance job; a failure is logged and does not stop the job.
func (s *Server) hstSweepLogged(ctx context.Context) {
	if _, err := s.HstSweep(ctx, time.Now()); err != nil {
		log.Printf("accommodation reminders: %v", err)
	}
}
