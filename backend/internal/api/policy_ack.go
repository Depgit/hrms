package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Company policy acknowledgement (Work policies › Company policies; My view › Dashboard › Compliance).
//
// HR Admin uploads a policy document (PDF) with a title, version, effective date and whom it applies to,
// and says whether new joiners must sign it. Publishing makes it due for everyone it applies to; a new
// version makes it due again; retiring ends it. Each employee downloads the policy, signs it and uploads
// the signed copy, which goes to the reporting manager through the approval engine (workflow p_ack). The
// manager approves it or returns it with a comment; HR Admin and the HRBP can act on it too.
//
// A person's status for a policy is that of the newest signed copy's request: Approved is Signed, Pending
// is Waiting for manager, Rejected is Returned, and anything else (nothing sent, or cancelled) is Not
// submitted. While any applicable policy in force by the end of the pay month is not Signed when the
// payroll run is made (dry run onwards), the salary is held for that month with a salary hold placed by
// the run (source PACK). Once everything is approved the hold is released: the person is paid in the run
// if it is still open, otherwise the held months are paid in the open month as every released hold is.

const ackHoldSource = "PACK"

func (s *Server) policyAckRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/company-policies", s.a(s.cpList))
	m.HandleFunc("POST /api/company-policies", s.a(s.cpCreate))
	m.HandleFunc("GET /api/company-policies/{id}", s.a(s.cpGet))
	m.HandleFunc("PUT /api/company-policies/{id}", s.a(s.cpSave))
	m.HandleFunc("POST /api/company-policies/{id}/publish", s.a(s.cpPublish))
	m.HandleFunc("POST /api/company-policies/{id}/version", s.a(s.cpVersion))
	m.HandleFunc("POST /api/company-policies/{id}/retire", s.a(s.cpRetire))
	m.HandleFunc("GET /api/me/policies", s.a(s.myPolicies))
	m.HandleFunc("POST /api/me/policies/{version}/submit", s.a(s.myPolicySubmit))
	s.finalizers["p_ack"] = finalizeAck
}

// ---------- Who a policy is due for, and where each person stands ----------

// ackPerson is an active employee a policy may apply to, with the names of their department, location and
// reporting manager ("" without one).
type ackPerson struct {
	E                  store.Row
	Dept, Loc, Manager string
	Cluster, City      string // ids of the location's cluster and city (ackItems fills them)
}

// ackPlaces fills each person's cluster and city from their location, for audiences by cluster or city.
func ackPlaces(ctx context.Context, q store.Q, people []ackPerson) error {
	locs, err := q.C("location").Find(ctx, nil, options.Find().SetProjection(bson.M{"cluster_id": 1}))
	if err != nil {
		return err
	}
	ks, err := q.C("cluster").Find(ctx, nil, options.Find().SetProjection(bson.M{"city_id": 1}))
	if err != nil {
		return err
	}
	locCluster, clusterCity := map[string]string{}, map[string]string{}
	for _, l := range locs {
		locCluster[str(l["id"])] = str(l["cluster_id"])
	}
	for _, k := range ks {
		clusterCity[str(k["id"])] = str(k["city_id"])
	}
	for i := range people {
		people[i].Cluster = locCluster[str(people[i].E["location_id"])]
		people[i].City = clusterCity[people[i].Cluster]
	}
	return nil
}

// ackPeople loads the active employees matching filter (a filter on employee documents, nil for everyone) in
// code order, with their department and location (someone missing either is left out, as the joins did) and
// their manager's name.
func ackPeople(ctx context.Context, q store.Q, filter any) ([]ackPerson, error) {
	f := bson.D{{Key: "status", Value: "Active"}}
	if filter != nil {
		f = bson.D{{Key: "$and", Value: bson.A{f, filter}}}
	}
	emps, err := q.C("employee").Find(ctx, f, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	depts, err := repByID(ctx, q, "department", repCol(emps, "department_id"))
	if err != nil {
		return nil, err
	}
	locs, err := repByID(ctx, q, "location", repCol(emps, "location_id"))
	if err != nil {
		return nil, err
	}
	managers, err := repNames(ctx, q, repCol(emps, "manager_id"))
	if err != nil {
		return nil, err
	}
	out := make([]ackPerson, 0, len(emps))
	for _, e := range emps {
		d, l := depts[str(e["department_id"])], locs[str(e["location_id"])]
		if d == nil || l == nil {
			continue
		}
		out = append(out, ackPerson{E: e, Dept: str(d["name"]), Loc: str(l["name"]), Manager: managers[str(e["manager_id"])]})
	}
	return out, nil
}

// ackItem is one (person, policy) pair that is due: the policy in its current version, and the person's newest
// signed copy of that version, its request and the last decision on it (nil when there is none).
type ackItem struct {
	P       ackPerson
	Pol, V  store.Row
	A, R, H store.Row
}

// status is the person's status for the current version, from the newest signed copy's request: Approved is
// Signed, Pending is Waiting for manager, Rejected is Returned, anything else Not submitted.
func (x ackItem) status() string {
	switch str(x.R["status"]) {
	case "Approved":
		return "Signed"
	case "Pending":
		return "Waiting for manager"
	case "Rejected":
		return "Returned"
	}
	return "Not submitted"
}

// decided is the last decision's field k when the request ended that way (nil otherwise).
func (x ackItem) decided(reqStatus, k string) any {
	if str(x.R["status"]) != reqStatus {
		return nil
	}
	return x.H[k]
}

// row is the item as the policy lists show it.
func (x ackItem) row() store.Row {
	return store.Row{"employee_id": x.P.E["id"], "name": x.P.E["name"], "department": x.P.Dept, "location": x.P.Loc, "manager": x.P.Manager,
		"policy_id": x.Pol["id"], "title": x.Pol["title"], "version_id": x.V["id"], "version": x.V["version"], "effective_from": x.V["effective_from"],
		"policy_doc": x.V["document_id"], "signed_doc": x.A["document_id"], "submitted_at": x.A["submitted_at"], "request_id": x.A["request_id"], "status": x.status(),
		"comment": x.decided("Rejected", "note"), "comment_by": x.decided("Rejected", "by_name"), "approved_by": x.decided("Approved", "by_name"),
		"approved_at": x.decided("Approved", "at")}
}

// ackApplies: the published policy pol (current version v) applies to the person: everyone, or the chosen
// departments, places and employment types (an empty list allows all; places are locations, clusters and cities,
// and being in any one of them is enough), or a named employee; and new joiners only when the policy says so
// (otherwise people who joined by the day it was published, in India).
func ackApplies(pol, v store.Row, p ackPerson) bool {
	aud, _ := pol["audience"].(map[string]any)
	if all, _ := aud["all"].(bool); !all {
		named := !ackEmpty(aud["employees"]) && ackListed(aud["employees"], str(p.E["id"]))
		group := !ackEmpty(aud["departments"]) || !ackEmpty(aud["types"]) || !ackPlacesEmpty(aud)
		inGroup := group && ackListed(aud["departments"], p.Dept) && ackListed(aud["types"], str(p.E["type"])) && ackInPlace(aud, p)
		if !named && !inGroup && (group || !ackEmpty(aud["employees"])) { // nothing chosen at all reads as everyone, as before
			return false
		}
	}
	if pol["new_joiners"] == true {
		return true
	}
	pub, ok := v["published_at"].(time.Time)
	return ok && str(p.E["doj"]) <= pub.In(ist).Format("2006-01-02")
}

// ackEmpty: the audience list is missing or empty.
func ackEmpty(l any) bool {
	xs, _ := l.([]any)
	return len(xs) == 0
}

func ackPlacesEmpty(aud map[string]any) bool {
	return ackEmpty(aud["locations"]) && ackEmpty(aud["clusters"]) && ackEmpty(aud["cities"])
}

// ackInPlace: no places chosen, or the person's location, cluster or city is one of them.
func ackInPlace(aud map[string]any, p ackPerson) bool {
	if ackPlacesEmpty(aud) {
		return true
	}
	return (!ackEmpty(aud["locations"]) && ackListed(aud["locations"], str(p.E["location_id"]))) ||
		(!ackEmpty(aud["clusters"]) && ackListed(aud["clusters"], p.Cluster)) ||
		(!ackEmpty(aud["cities"]) && ackListed(aud["cities"], p.City))
}

// ackListed: an empty (or missing) audience list allows everyone; otherwise v must be on it.
func ackListed(l any, v string) bool {
	xs, _ := l.([]any)
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if t, ok := repJSONText(x); ok && t == v {
			return true
		}
	}
	return false
}

// ackItems lists every (person, policy) pair that is due: each published policy (only policyID when given) in
// its current version, for each of the people it applies to, with the newest signed copy, its request and the
// last decision. Pairs come by person, then policy id.
func ackItems(ctx context.Context, q store.Q, people []ackPerson, policyID string) ([]ackItem, error) {
	out := []ackItem{}
	if len(people) == 0 {
		return out, nil
	}
	if err := ackPlaces(ctx, q, people); err != nil {
		return nil, err
	}
	pf := bson.M{"status": "Published"}
	if policyID != "" {
		pf["_id"] = policyID
	}
	pols, err := q.C("company_policy").Find(ctx, pf, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	vers, err := ackVersions(ctx, q, pols)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		for _, pol := range pols {
			if v := vers[repInt64(pol["current_version"])]; v != nil && pol["current_version"] != nil && ackApplies(pol, v, p) {
				out = append(out, ackItem{P: p, Pol: pol, V: v})
			}
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	// The newest signed copy of each person for each version.
	vids, emps := []any{}, []string{}
	for _, v := range vers {
		vids = append(vids, v["id"])
	}
	for _, p := range people {
		emps = append(emps, str(p.E["id"]))
	}
	acks, err := q.C("policy_ack").Find(ctx, bson.D{{Key: "version_id", Value: bson.D{{Key: "$in", Value: vids}}}, {Key: "employee_id", Value: repIn(emps)}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}))
	if err != nil {
		return nil, err
	}
	newest := map[string]store.Row{}
	reqIDs := []string{}
	for _, a := range acks {
		k := fmt.Sprint(a["version_id"]) + "|" + str(a["employee_id"])
		if newest[k] == nil {
			newest[k] = a
			reqIDs = append(reqIDs, str(a["request_id"]))
		}
	}
	reqs, err := repByID(ctx, q, "approval_request", reqIDs)
	if err != nil {
		return nil, err
	}
	// The last approval or return of each request.
	hist, err := q.C("approval_history").Find(ctx, bson.D{{Key: "request_id", Value: repIn(reqIDs)}, {Key: "action", Value: bson.D{{Key: "$in", Value: bson.A{"Approved", "Rejected"}}}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}))
	if err != nil {
		return nil, err
	}
	last := map[string]store.Row{}
	for _, h := range hist {
		if id := str(h["request_id"]); last[id] == nil {
			last[id] = h
		}
	}
	for i := range out {
		x := &out[i]
		if a := newest[fmt.Sprint(x.V["id"])+"|"+str(x.P.E["id"])]; a != nil {
			x.A, x.R, x.H = a, reqs[str(a["request_id"])], last[str(a["request_id"])]
		}
	}
	return out, nil
}

// ackVersions loads the current versions of these policies, by id.
func ackVersions(ctx context.Context, q store.Q, pols []store.Row) (map[int64]store.Row, error) {
	ids := []any{}
	for _, p := range pols {
		if v := p["current_version"]; v != nil {
			ids = append(ids, v)
		}
	}
	out := map[int64]store.Row{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.C("company_policy_version").Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	for _, v := range rows {
		out[repInt64(v["id"])] = v
	}
	return out, nil
}

// ackItemsOf lists the pairs due for the active employees matching filter (one policy when policyID is given).
func ackItemsOf(ctx context.Context, q store.Q, filter any, policyID string) ([]ackItem, error) {
	people, err := ackPeople(ctx, q, filter)
	if err != nil {
		return nil, err
	}
	return ackItems(ctx, q, people, policyID)
}

// ackStatusRank orders the people of a policy: Returned, Not submitted, Waiting for manager, then Signed.
func ackStatusRank(st string) int {
	switch st {
	case "Returned":
		return 0
	case "Not submitted":
		return 1
	case "Waiting for manager":
		return 2
	}
	return 3
}

// ackNull stores an empty text as null (the old NULLIF).
func ackNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type cpAudience struct {
	All         bool     `json:"all"`
	Departments []string `json:"departments,omitempty"`
	Locations   []string `json:"locations,omitempty"`
	Types       []string `json:"types,omitempty"`
	Clusters    []string `json:"clusters,omitempty"`  // cluster ids (17 Nov 2026)
	Cities      []string `json:"cities,omitempty"`    // city ids
	Employees   []string `json:"employees,omitempty"` // named employees, on top of the groups above
}

func (a *cpAudience) check(ctx context.Context, q store.Q) error {
	if a.All {
		a.Departments, a.Locations, a.Types, a.Clusters, a.Cities, a.Employees = nil, nil, nil, nil, nil, nil
		return nil
	}
	if len(a.Departments)+len(a.Locations)+len(a.Types)+len(a.Clusters)+len(a.Cities)+len(a.Employees) == 0 {
		return bad("Choose whom the policy applies to: everyone, or departments, locations or employment types")
	}
	for _, x := range []struct {
		coll string
		ids  []string
	}{{"cluster", a.Clusters}, {"city", a.Cities}, {"employee", a.Employees}} {
		for _, id := range x.ids {
			if ok, _ := q.C(x.coll).Exists(ctx, bson.M{"_id": id}); !ok {
				return bad("A chosen %s is not on record", x.coll)
			}
		}
	}
	for _, n := range a.Departments {
		if ok, _ := q.C("department").Exists(ctx, bson.M{"name": n}); !ok {
			return bad("Department %s is not in the masters", n)
		}
	}
	for _, id := range a.Locations {
		if ok, _ := q.C("location").Exists(ctx, bson.M{"_id": id}); !ok {
			return bad("A chosen location is not in the masters")
		}
	}
	for _, t := range a.Types {
		if !containsStr([]string{"Permanent", "Retainer", "Contractual"}, t) {
			return bad("Employment type must be Permanent, Retainer or Contractual")
		}
	}
	return nil
}

type cpBody struct {
	Title         string      `json:"title"`
	Version       string      `json:"version"`
	EffectiveFrom string      `json:"effectiveFrom"`
	Audience      *cpAudience `json:"audience"`
	NewJoiners    *bool       `json:"newJoiners"`
	DocID         string      `json:"docId"`
	Note          string      `json:"note"`
	Publish       bool        `json:"publish"`
}

// ---------- Setup (HR Admin) ----------

func (s *Server) cpList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 1); err != nil {
		return nil, err
	}
	ctx, q := r.Context(), s.st.DB
	list, err := q.C("company_policy").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	vers, err := ackVersions(ctx, q, list)
	if err != nil {
		return nil, err
	}
	all, err := q.C("company_policy_version").Find(ctx, bson.M{"policy_id": bson.M{"$in": repCol(list, "id")}}, options.Find().SetProjection(bson.D{{Key: "policy_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	nvers := map[string]int64{}
	for _, v := range all {
		nvers[str(v["policy_id"])]++
	}
	// Published first, then drafts, then retired; newest first within each.
	rank := map[string]int{"Published": 0, "Draft": 1}
	sort.SliceStable(list, func(i, j int) bool {
		ri, ok := rank[str(list[i]["status"])]
		if !ok {
			ri = 2
		}
		rj, ok := rank[str(list[j]["status"])]
		if !ok {
			rj = 2
		}
		if ri != rj {
			return ri < rj
		}
		ai, _ := list[i]["created_at"].(time.Time)
		aj, _ := list[j]["created_at"].(time.Time)
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return str(list[i]["id"]) < str(list[j]["id"])
	})
	counts, err := s.ackCounts(ctx, c, "")
	if err != nil {
		return nil, err
	}
	pols := []store.Row{}
	for _, p := range list {
		var v store.Row
		if p["current_version"] != nil {
			v = vers[repInt64(p["current_version"])]
		}
		pols = append(pols, store.Row{"id": p["id"], "title": p["title"], "status": p["status"], "audience": p["audience"], "new_joiners": p["new_joiners"],
			"created_by": p["created_by"], "created_at": p["created_at"], "retired_by": p["retired_by"], "retired_at": p["retired_at"], "retire_note": p["retire_note"],
			"version_id": v["id"], "version": v["version"], "effective_from": v["effective_from"], "policy_doc": v["document_id"], "published_at": v["published_at"],
			"published_by": v["published_by"], "versions": nvers[str(p["id"])], "counts": counts[str(p["id"])]})
	}
	return map[string]any{"policies": pols, "canEdit": c.Level("pol.policies") >= 2}, nil
}

// ackCounts counts people by status for each published policy (or one), within the caller's scope.
func (s *Server) ackCounts(ctx context.Context, c *Ctx, policyID string) (map[string]map[string]int, error) {
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	items, err := ackItemsOf(ctx, s.st.DB, scope, policyID)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int{}
	for _, x := range items {
		id := str(x.Pol["id"])
		if out[id] == nil {
			out[id] = map[string]int{"Signed": 0, "Waiting for manager": 0, "Returned": 0, "Not submitted": 0, "total": 0}
		}
		out[id][x.status()]++
		out[id]["total"]++
	}
	return out, nil
}

func (s *Server) cpGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 1); err != nil {
		return nil, err
	}
	ctx, id, q := r.Context(), r.PathValue("id"), s.st.DB
	p, err := q.C("company_policy").FindOne(ctx, bson.M{"_id": id})
	if err != nil || p == nil {
		return nil, orNotFound(err)
	}
	var v store.Row
	if p["current_version"] != nil {
		if v, err = q.C("company_policy_version").FindOne(ctx, bson.M{"_id": p["current_version"]}); err != nil {
			return nil, err
		}
	}
	p["version"], p["effective_from"], p["policy_doc"], p["published_at"] = v["version"], v["effective_from"], v["document_id"], v["published_at"]
	p["versions"], err = q.C("company_policy_version").Find(ctx, bson.M{"policy_id": id}, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).
		SetProjection(bson.D{{Key: "policy_id", Value: 0}}))
	if err != nil {
		return nil, err
	}
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	items, err := ackItemsOf(ctx, q, scope, id)
	if err != nil {
		return nil, err
	}
	st := r.URL.Query().Get("status")
	people := []ackItem{}
	for _, x := range items {
		if st == "" || x.status() == st {
			people = append(people, x)
		}
	}
	// Returned first, then not submitted, waiting and signed; by name within each.
	sort.SliceStable(people, func(i, j int) bool {
		if a, b := ackStatusRank(people[i].status()), ackStatusRank(people[j].status()); a != b {
			return a < b
		}
		return str(people[i].P.E["name"]) < str(people[j].P.E["name"])
	})
	if len(people) > 2000 {
		people = people[:2000]
	}
	rows := make([]store.Row, len(people))
	for i, x := range people {
		rows[i] = x.row()
	}
	p["people"] = rows
	counts, err := s.ackCounts(ctx, c, id)
	if err != nil {
		return nil, err
	}
	p["counts"] = counts[id]
	p["canEdit"] = c.Level("pol.policies") >= 2
	return p, nil
}

// claimPolicyDoc checks an uploaded policy file (a PDF uploaded under a DRAFT- key) and gives it to a version.
func claimPolicyDoc(ctx context.Context, tx store.Q, docID string, version int64) error {
	d, err := tx.C("document").FindOne(ctx, bson.M{"_id": docID, "owner_kind": "policy", "replaced_at": nil})
	if err != nil || d == nil {
		return bad("Upload the policy document")
	}
	if str(d["content_type"]) != "application/pdf" {
		return bad("The policy document must be a PDF")
	}
	if !draftOwner(str(d["owner_id"])) {
		return bad("This document already belongs to a policy version. Upload the file again")
	}
	_, err = tx.C("document").Update(ctx, bson.M{"_id": docID}, bson.M{"$set": bson.M{"owner_id": strconv.FormatInt(version, 10)}})
	return err
}

// cpTitleTaken reports whether a policy that is not retired (other than except) already has this title in any
// letter case.
func cpTitleTaken(ctx context.Context, q store.Q, title, except string) (bool, error) {
	f := bson.M{"status": bson.M{"$ne": "Retired"}}
	if except != "" {
		f["_id"] = bson.M{"$ne": except}
	}
	rows, err := q.C("company_policy").Find(ctx, f, options.Find().SetProjection(bson.D{{Key: "title", Value: 1}}))
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if strings.ToLower(str(r["title"])) == strings.ToLower(title) {
			return true, nil
		}
	}
	return false, nil
}

func checkVersion(b *cpBody) error {
	if b.Version = strings.TrimSpace(b.Version); b.Version == "" || len(b.Version) > 20 {
		return bad("Enter the version, for example 1.0")
	}
	if !isDate(b.EffectiveFrom) {
		return bad("Enter the effective date")
	}
	if b.DocID == "" {
		return bad("Upload the policy document")
	}
	return nil
}

func (s *Server) cpCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 2); err != nil {
		return nil, err
	}
	var b cpBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if b.Title = strings.TrimSpace(b.Title); len(b.Title) < 3 || len(b.Title) > 120 {
		return nil, bad("Enter the policy title")
	}
	if err := checkVersion(&b); err != nil {
		return nil, err
	}
	if b.Audience == nil {
		b.Audience = &cpAudience{All: true}
	}
	if err := b.Audience.check(ctx, s.st.DB); err != nil {
		return nil, err
	}
	joiners := b.NewJoiners == nil || *b.NewJoiners
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if ok, _ := cpTitleTaken(ctx, tx, b.Title, ""); ok {
			return bad("A policy called %s already exists. Add a new version to it instead", b.Title)
		}
		n, err := store.NextSeq(ctx, tx, "company_policy_seq")
		if err != nil {
			return err
		}
		id = fmt.Sprintf("CP-%d", n)
		if _, err := tx.C("company_policy").Insert(ctx, bson.M{"_id": id, "title": b.Title, "audience": b.Audience, "new_joiners": joiners, "created_by": c.Name}); err != nil {
			return err
		}
		v, err := tx.C("company_policy_version").Insert(ctx, bson.M{"policy_id": id, "version": b.Version, "effective_from": b.EffectiveFrom, "document_id": b.DocID,
			"note": ackNull(strings.TrimSpace(b.Note)), "created_by": c.Name})
		if err != nil {
			return bad("Upload the policy document")
		}
		ver := repInt64(v)
		if err := claimPolicyDoc(ctx, tx, b.DocID, ver); err != nil {
			return err
		}
		if _, err := tx.C("company_policy").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"current_version": ver}}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, "Added company policy "+b.Title, "company_policy", id, nil, b); err != nil {
			return err
		}
		if b.Publish {
			return publishPolicy(ctx, tx, c, id)
		}
		return nil
	})
	return map[string]string{"id": id}, err
}

// loadPolicyForEdit reads a policy FOR UPDATE: its id, title, status, audience, new joiner rule and current version.
func loadPolicyForEdit(ctx context.Context, tx store.Q, id string) (store.Row, error) {
	p, err := tx.C("company_policy").Lock(ctx, bson.M{"_id": id})
	if err != nil || p == nil {
		return nil, orNotFound(err)
	}
	return store.Row{"id": p["id"], "title": p["title"], "status": p["status"], "audience": p["audience"], "new_joiners": p["new_joiners"],
		"current_version": p["current_version"]}, nil
}

// cpSave edits a policy. A draft can change everything; a published policy its title, whom it applies to
// and the new joiner rule (the document changes with a new version). Retired policies are read only.
func (s *Server) cpSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 2); err != nil {
		return nil, err
	}
	var b cpBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		p, err := loadPolicyForEdit(ctx, tx, id)
		if err != nil {
			return err
		}
		if p["status"] == "Retired" {
			return bad("A retired policy cannot be changed")
		}
		if b.Title = strings.TrimSpace(b.Title); b.Title != "" {
			if len(b.Title) < 3 || len(b.Title) > 120 {
				return bad("Enter the policy title")
			}
			if ok, _ := cpTitleTaken(ctx, tx, b.Title, id); ok {
				return bad("A policy called %s already exists", b.Title)
			}
		} else {
			b.Title = p["title"].(string)
		}
		aud := p["audience"]
		if b.Audience != nil {
			if err := b.Audience.check(ctx, tx); err != nil {
				return err
			}
			aud = b.Audience
		}
		joiners := p["new_joiners"].(bool)
		if b.NewJoiners != nil {
			joiners = *b.NewJoiners
		}
		if _, err := tx.C("company_policy").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"title": b.Title, "audience": jsonOrNil(aud), "new_joiners": joiners}}); err != nil {
			return err
		}
		if p["status"] == "Draft" && (b.Version != "" || b.EffectiveFrom != "" || b.DocID != "") {
			ver := p["current_version"].(int64)
			cur, _ := tx.C("company_policy_version").FindOne(ctx, bson.M{"_id": ver})
			if b.Version == "" {
				b.Version = str(cur["version"])
			}
			if b.EffectiveFrom == "" {
				b.EffectiveFrom = str(cur["effective_from"])
			}
			newDoc := b.DocID != "" && b.DocID != str(cur["document_id"])
			if b.DocID == "" {
				b.DocID = str(cur["document_id"])
			}
			if err := checkVersion(&b); err != nil {
				return err
			}
			if newDoc {
				if err := claimPolicyDoc(ctx, tx, b.DocID, ver); err != nil {
					return err
				}
			}
			if _, err := tx.C("company_policy_version").Update(ctx, bson.M{"_id": ver}, bson.M{"$set": bson.M{"version": b.Version, "effective_from": b.EffectiveFrom,
				"document_id": b.DocID}}); err != nil {
				return err
			}
		}
		if err := audit(ctx, tx, c, "Changed company policy "+b.Title, "company_policy", id, p, b); err != nil {
			return err
		}
		// Whom it applies to may have changed: a hold for a policy that no longer applies is released.
		return releaseClearedAckHolds(ctx, tx, "")
	})
}

// publishPolicy publishes a draft; its current version is published now unless it already was.
func publishPolicy(ctx context.Context, tx store.Q, c *Ctx, id string) error {
	if _, err := tx.C("company_policy").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Published"}}); err != nil {
		return err
	}
	p, err := tx.C("company_policy").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.D{{Key: "current_version", Value: 1}}))
	if err != nil {
		return err
	}
	if p != nil && p["current_version"] != nil {
		if _, err := tx.C("company_policy_version").Update(ctx, bson.M{"_id": p["current_version"], "published_at": nil},
			bson.M{"$set": bson.M{"published_at": tx.Now(), "published_by": c.Name}}); err != nil {
			return err
		}
	}
	return audit(ctx, tx, c, "Published company policy", "company_policy", id, "Draft", "Published")
}

func (s *Server) cpPublish(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		p, err := loadPolicyForEdit(ctx, tx, id)
		if err != nil {
			return err
		}
		if p["status"] != "Draft" {
			return bad("Only a draft can be published")
		}
		return publishPolicy(ctx, tx, c, id)
	})
}

// cancelAcks cancels the signed copies still waiting for a decision on a policy's versions other than keep.
func cancelAcks(ctx context.Context, tx store.Q, policyID string, keep int64, why string) error {
	vids, err := tx.C("company_policy_version").Distinct(ctx, "_id", bson.M{"policy_id": policyID, "_id": bson.M{"$ne": keep}})
	if err != nil || len(vids) == 0 {
		return err
	}
	reqIDs, err := tx.C("policy_ack").Distinct(ctx, "request_id", bson.M{"version_id": bson.M{"$in": vids}})
	if err != nil || len(reqIDs) == 0 {
		return err
	}
	rows, err := tx.C("approval_request").Find(ctx, bson.M{"_id": bson.M{"$in": reqIDs}, "status": "Pending"}, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return err
	}
	for _, x := range rows {
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
			return err
		}
		if err := history(ctx, tx, x["id"].(string), "", "Cancelled", "System", why); err != nil {
			return err
		}
	}
	return nil
}

// cpVersion publishes a new version of a published policy: it is due again for everyone it applies to.
func (s *Server) cpVersion(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 2); err != nil {
		return nil, err
	}
	var b cpBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := checkVersion(&b); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	var ver int64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		p, err := loadPolicyForEdit(ctx, tx, id)
		if err != nil {
			return err
		}
		if p["status"] != "Published" {
			return bad("A new version is added to a published policy. Change the draft instead")
		}
		olds, err := tx.C("company_policy_version").Find(ctx, bson.M{"policy_id": id}, options.Find().SetProjection(bson.D{{Key: "version", Value: 1}}))
		if err != nil {
			return err
		}
		for _, o := range olds {
			if strings.ToLower(str(o["version"])) == strings.ToLower(b.Version) {
				return bad("Version %s already exists", b.Version)
			}
		}
		v, err := tx.C("company_policy_version").Insert(ctx, bson.M{"policy_id": id, "version": b.Version, "effective_from": b.EffectiveFrom, "document_id": b.DocID,
			"note": ackNull(strings.TrimSpace(b.Note)), "created_by": c.Name, "published_by": c.Name, "published_at": tx.Now()})
		if err != nil {
			return bad("Upload the policy document")
		}
		ver = repInt64(v)
		if err := claimPolicyDoc(ctx, tx, b.DocID, ver); err != nil {
			return err
		}
		if _, err := tx.C("company_policy").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"current_version": ver}}); err != nil {
			return err
		}
		if err := cancelAcks(ctx, tx, id, ver, "Version "+b.Version+" of the policy was published, so this copy is no longer needed"); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Published version "+b.Version+" of company policy "+p["title"].(string), "company_policy", id, nil, b)
	})
	return map[string]any{"id": id, "version": ver}, err
}

func (s *Server) cpRetire(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pol.policies", 2); err != nil {
		return nil, err
	}
	var b struct {
		Note string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Note = strings.TrimSpace(b.Note); len(b.Note) < 5 {
		return nil, bad("Write why the policy is retired")
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		p, err := loadPolicyForEdit(ctx, tx, id)
		if err != nil {
			return err
		}
		if p["status"] == "Retired" {
			return bad("This policy is already retired")
		}
		if _, err := tx.C("company_policy").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Retired", "retired_by": c.Name, "retired_at": tx.Now(),
			"retire_note": b.Note}}); err != nil {
			return err
		}
		if err := cancelAcks(ctx, tx, id, -1, "The policy was retired"); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, "Retired company policy "+p["title"].(string), "company_policy", id, p["status"], map[string]string{"status": "Retired", "note": b.Note}); err != nil {
			return err
		}
		return releaseClearedAckHolds(ctx, tx, "")
	})
}

// ---------- The employee's own list (My view) ----------

// holdYmFor is the payroll month a pending policy holds: the open month, or the month it takes effect.
func holdYmFor(open, effective string) string {
	if len(effective) >= 7 && effective[:7] > open {
		return effective[:7]
	}
	return open
}

func (s *Server) myPolicies(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	items, err := ackItemsOf(ctx, s.st.DB, bson.M{"_id": c.EmployeeID}, "")
	if err != nil {
		return nil, err
	}
	// Pending first, then by the date each takes effect and title.
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if sa, sb := a.status() == "Signed", b.status() == "Signed"; sa != sb {
			return sb
		}
		if ea, eb := str(a.V["effective_from"]), str(b.V["effective_from"]); ea != eb {
			return ea < eb
		}
		return str(a.Pol["title"]) < str(b.Pol["title"])
	})
	rows := make([]store.Row, len(items))
	for i, x := range items {
		rows[i] = x.row()
	}
	open := openYm(ctx, s.st.DB)
	pending, holdYm := 0, ""
	for _, x := range rows {
		delete(x, "manager")
		if x["status"] == "Signed" {
			continue
		}
		pending++
		ym := holdYmFor(open, str(x["effective_from"]))
		x["holdYm"], x["holdLabel"] = ym, ymLabel(ym)
		if holdYm == "" || ym < holdYm {
			holdYm = ym
		}
		if x["status"] == "Waiting for manager" {
			x["approver"] = s.waitingOn(ctx, s.st.DB, str(x["request_id"]))
		}
	}
	out := map[string]any{"items": rows, "pending": pending, "openYm": open}
	if holdYm != "" {
		out["holdYm"], out["holdLabel"] = holdYm, ymLabel(holdYm)
	}
	if h, _ := s.st.DB.C("salary_hold").FindOne(ctx, bson.M{"employee_id": c.EmployeeID, "status": "Active", "source_id": ackHoldSource}); h != nil {
		out["heldSince"] = ymLabel(h["from_ym"].(string))
	}
	return out, nil
}

// waitingOn names who must act on a request now ("" when it is not pending).
func (s *Server) waitingOn(ctx context.Context, q store.Q, id string) string {
	req, err := loadRequest(ctx, q, id, false)
	if err != nil || req.Status != "Pending" || req.Step >= len(req.Chain) {
		return ""
	}
	sub, err := loadSubject(ctx, q, req.EmployeeID)
	if err != nil {
		return ""
	}
	return resolvePerson(ctx, q, req.Chain[req.Step], sub, req.Facts).Name
}

type ackPayload struct {
	PolicyID      string `json:"policyId"`
	PolicyTitle   string `json:"policyTitle"`
	Version       string `json:"version"`
	VersionID     int64  `json:"versionId"`
	EffectiveFrom string `json:"effectiveFrom"`
	PolicyDoc     string `json:"policyDoc"`
	SignedDoc     string `json:"signedDoc"`
	Note          string `json:"note,omitempty"`
}

// myPolicySubmit sends the employee's signed copy (uploaded as a policy_ack document) for approval.
func (s *Server) myPolicySubmit(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		DocID string `json:"docId"`
		Note  string `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ver, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		return nil, notFound("Policy not found")
	}
	ctx := r.Context()
	var out map[string]any
	err = s.st.Tx(ctx, func(tx store.Q) error {
		out = nil
		// One submission at a time per person and version.
		if err := store.AdvisoryLock(ctx, tx, "p_ack:"+c.EmployeeID+":"+strconv.FormatInt(ver, 10)); err != nil {
			return err
		}
		items, err := ackItemsOf(ctx, tx, bson.M{"_id": c.EmployeeID}, "")
		if err != nil {
			return err
		}
		var row store.Row
		for _, x := range items {
			if repInt64(x.V["id"]) == ver {
				row = x.row()
				break
			}
		}
		if row == nil {
			return notFound("This policy does not apply to you, or a newer version has been published")
		}
		switch row["status"] {
		case "Signed":
			return bad("You have already acknowledged this policy")
		case "Waiting for manager":
			return bad("Your signed copy is waiting for your manager. Cancel it in Approvals to send another")
		}
		ok, err := tx.C("document").Exists(ctx, bson.M{"_id": b.DocID, "owner_kind": "policy_ack", "owner_id": c.EmployeeID + "/" + strconv.FormatInt(ver, 10), "replaced_at": nil})
		if err != nil || !ok {
			return bad("Upload the signed copy first")
		}
		p := ackPayload{PolicyID: str(row["policy_id"]), PolicyTitle: str(row["title"]), Version: str(row["version"]), VersionID: ver,
			EffectiveFrom: str(row["effective_from"]), PolicyDoc: str(row["policy_doc"]), SignedDoc: b.DocID, Note: strings.TrimSpace(b.Note)}
		res, err := s.createRequestTx(ctx, tx, c, "p_ack", c.EmployeeID, approval.Facts{}, p)
		if err != nil {
			return err
		}
		if _, err := tx.C("policy_ack").Insert(ctx, bson.M{"version_id": ver, "employee_id": c.EmployeeID, "document_id": b.DocID, "request_id": res["id"]}); err != nil {
			return err
		}
		// The signed copy is part of the employee's record.
		if _, err := tx.C("document").Update(ctx, bson.M{"_id": b.DocID}, bson.M{"$set": bson.M{"employee_id": c.EmployeeID}}); err != nil {
			return err
		}
		if res["status"] == "Approved" { // no approval needed: the hold goes now
			if err := releaseClearedAckHolds(ctx, tx, c.EmployeeID); err != nil {
				return err
			}
		}
		out = res
		return nil
	})
	return out, err
}

// finalizeAck runs when the signed copy is approved: with nothing else pending, the hold is released.
func finalizeAck(ctx context.Context, tx store.Q, r *Request) (string, error) {
	ackHold := bson.M{"employee_id": r.EmployeeID, "status": "Active", "source_id": ackHoldSource}
	held, _ := tx.C("salary_hold").Exists(ctx, ackHold)
	if err := releaseClearedAckHolds(ctx, tx, r.EmployeeID); err != nil {
		return "", err
	}
	still, _ := tx.C("salary_hold").Exists(ctx, ackHold)
	switch {
	case held && !still:
		return "Policy acknowledged. Every company policy is approved, so the salary hold is released.", nil
	case held:
		return "Policy acknowledged. The salary stays held: another company policy is still pending.", nil
	}
	return "Policy acknowledged.", nil
}

// ---------- Salary hold ----------

// ackPendingTitles lists, per employee, the applicable policies not yet Signed that are in force by
// monthEnd, in title order. people narrows the employees (a filter on employee documents).
func ackPendingTitles(ctx context.Context, q store.Q, monthEnd string, people any) (map[string][]string, error) {
	items, err := ackItemsOf(ctx, q, people, "")
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, x := range items {
		if str(x.V["effective_from"]) <= monthEnd && x.status() != "Signed" {
			id := str(x.P.E["id"])
			out[id] = append(out[id], str(x.Pol["title"]))
		}
	}
	for _, l := range out {
		sort.Strings(l)
	}
	return out, nil
}

func ackHoldReason(titles []string) string {
	return "Company policy not acknowledged: " + strings.Join(titles, ", ")
}

// syncAckHolds runs with every payroll calculation (dry run onwards), before the rows are computed: anyone
// in the run (paid in the month, and in the pilot when there is one) with a policy pending is held from this
// month, and a hold placed this way for someone whose policies are now all approved is released. A person
// already held for another reason stays on that hold.
func syncAckHolds(ctx context.Context, tx store.Q, ym string, pilot []string) error {
	published, err := tx.C("company_policy").Exists(ctx, bson.M{"status": "Published"})
	if err != nil {
		return err
	}
	placed, err := tx.C("salary_hold").Exists(ctx, bson.M{"status": "Active", "source_id": ackHoldSource})
	if err != nil || (!published && !placed) {
		return err
	}
	last := policy.LastDayOf(ym)
	eligible, err := payEligible(ctx, tx, ym)
	if err != nil {
		return err
	}
	inRun := any(eligible)
	if len(pilot) > 0 {
		inRun = bson.D{{Key: "$and", Value: bson.A{eligible, bson.M{"_id": bson.M{"$in": pilot}}}}}
	}
	pend, err := ackPendingTitles(ctx, tx, last, inRun)
	if err != nil {
		return err
	}
	holds, err := tx.C("salary_hold").Find(ctx, bson.M{"status": "Active"})
	if err != nil {
		return err
	}
	held := map[string]store.Row{}
	for _, h := range holds {
		held[h["employee_id"].(string)] = h
	}
	ids := make([]string, 0, len(pend))
	for id := range pend {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		reason := ackHoldReason(pend[id])
		h := held[id]
		switch {
		case h == nil:
			if _, err := placeHold(ctx, tx, "System", id, reason, "Placed by the payroll run for "+ymLabel(ym)+". Released automatically once every company policy is approved.", ym, ackHoldSource); err != nil {
				return err
			}
		case str(h["source_id"]) == ackHoldSource && h["reason"] != reason:
			if _, err := tx.C("salary_hold").Update(ctx, bson.M{"_id": h["id"]}, bson.M{"$set": bson.M{"reason": reason}}); err != nil {
				return err
			}
		}
	}
	// Holds placed by an earlier run for people in this run with nothing pending any more.
	stale, err := tx.C("salary_hold").Find(ctx, bson.M{"status": "Active", "source_id": ackHoldSource, "employee_id": bson.M{"$nin": ids}})
	if err != nil || len(stale) == 0 {
		return err
	}
	inRunIDs, err := tx.C("employee").Distinct(ctx, "_id", bson.D{{Key: "$and", Value: bson.A{inRun, bson.M{"_id": bson.M{"$in": repCol(stale, "employee_id")}}}}})
	if err != nil {
		return err
	}
	running := map[string]bool{}
	for _, id := range inRunIDs {
		running[str(id)] = true
	}
	for _, x := range stale {
		if !running[str(x["employee_id"])] {
			continue
		}
		if _, err := releaseHold(ctx, tx, "System", x["id"].(string), "Every company policy is approved"); err != nil {
			return err
		}
	}
	return nil
}

// releaseClearedAckHolds releases holds placed for company policies (for one person, or everyone) once
// nothing is pending for the open payroll month.
func releaseClearedAckHolds(ctx context.Context, tx store.Q, emp string) error {
	f := bson.M{"status": "Active", "source_id": ackHoldSource}
	if emp != "" {
		f["employee_id"] = emp
	}
	holds, err := tx.C("salary_hold").Find(ctx, f)
	if err != nil || len(holds) == 0 {
		return err
	}
	pend, err := ackPendingTitles(ctx, tx, policy.LastDayOf(openYm(ctx, tx)), bson.M{"_id": bson.M{"$in": repCol(holds, "employee_id")}})
	if err != nil {
		return err
	}
	for _, h := range holds {
		e := h["employee_id"].(string)
		if len(pend[e]) > 0 {
			if _, err := tx.C("salary_hold").Update(ctx, bson.M{"_id": h["id"]}, bson.M{"$set": bson.M{"reason": ackHoldReason(pend[e])}}); err != nil {
				return err
			}
			continue
		}
		if _, err := releaseHold(ctx, tx, "System", h["id"].(string), "Every company policy is approved"); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Employee file, payroll checks, dashboard, report ----------

// ackReadiness is the "Company policies" item of the employee file for a pay month (nil when none applies).
func (s *Server) ackReadiness(ctx context.Context, id, ym string) *readyItem {
	items, err := ackItemsOf(ctx, s.st.DB, bson.M{"_id": id}, "")
	if err != nil || len(items) == 0 {
		return nil
	}
	rows := make([]store.Row, len(items))
	for i, x := range items {
		rows[i] = x.row()
	}
	last := policy.LastDayOf(ym)
	who, waiting, mine, later := []string{}, 0, 0, 0
	for _, x := range rows {
		if x["status"] == "Signed" {
			continue
		}
		if str(x["effective_from"]) > last {
			later++
			continue
		}
		if x["status"] == "Waiting for manager" {
			waiting++
			if n := s.waitingOn(ctx, s.st.DB, str(x["request_id"])); n != "" && !containsStr(who, n) {
				who = append(who, n)
			}
		} else {
			mine++
		}
	}
	it := &readyItem{Key: "policies", Title: "Company policies", State: "ready", Text: fmt.Sprintf("All %d acknowledged", len(rows)-later), Owner: "Employee"}
	if later > 0 {
		it.Text += fmt.Sprintf(". %d more in force after %s", later, ymLabel(ym))
	}
	if n := waiting + mine; n > 0 {
		if mine > 0 {
			who = append(who, "the employee")
		}
		it.State = "hold"
		it.Text = fmt.Sprintf("%d pending, with %s. Salary for %s is held until approved", n, joinAnd(who), ymLabel(ym))
		it.Owner = map[bool]string{true: "Manager", false: "Employee"}[mine == 0]
	}
	return it
}

func joinAnd(l []string) string {
	if len(l) <= 1 {
		return strings.Join(l, "")
	}
	return strings.Join(l[:len(l)-1], ", ") + " and " + l[len(l)-1]
}

// ackRunChecks is the payroll run check for the people in the run, once company policies are in use.
func (s *Server) ackRunChecks(ctx context.Context, ym string, ids []string) []calc.Check {
	if ok, _ := s.st.DB.C("company_policy").Exists(ctx, bson.M{"status": "Published"}); !ok {
		return nil
	}
	ck := calc.Check{Title: "Company policies acknowledged", OK: true, Detail: "None pending"}
	pend, err := ackPendingTitles(ctx, s.st.DB, policy.LastDayOf(ym), bson.D{{Key: "_id", Value: repIn(ids)}})
	if err != nil || len(pend) == 0 {
		return []calc.Check{ck}
	}
	names := []string{}
	if rows, err := s.st.DB.C("employee").Find(ctx, bson.D{{Key: "_id", Value: repIn(keysOf(pend))}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetProjection(bson.D{{Key: "name", Value: 1}})); err == nil {
		for _, r := range rows {
			names = append(names, r["name"].(string))
		}
	}
	more := ""
	if len(names) > 5 {
		more = fmt.Sprintf(" and %d more", len(names)-5)
		names = names[:5]
	}
	ck.OK = false
	ck.Detail = fmt.Sprintf("%d held: %s%s", len(pend), strings.Join(names, "; "), more)
	return []calc.Check{ck}
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ackAlert is the dashboard's "Needs attention" line for people in the caller's scope (manager and HR views).
func (s *Server) ackAlert(ctx context.Context, c *Ctx) (string, string) {
	if c.Role == "Employee" || (c.Level("pol.policies") < 1 && c.Level("appr.queue") < 1 && c.Level("emp.directory") < 1) {
		return "", ""
	}
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return "", ""
	}
	items, err := ackItemsOf(ctx, s.st.DB, scope, "")
	if err != nil {
		return "", ""
	}
	pending := map[string]bool{}
	for _, x := range items {
		if x.status() != "Signed" {
			pending[str(x.P.E["id"])] = true
		}
	}
	n := len(pending)
	if n == 0 {
		return "", ""
	}
	page := "/approvals"
	if c.Level("pol.policies") >= 1 {
		page = "/policies?tab=cp"
	}
	if n == 1 {
		return "1 person has company policies pending", page
	}
	return fmt.Sprintf("%d people have company policies pending", n), page
}

// repPolicyAck: every applicable published policy for each person, with status and who approved it.
func repPolicyAck(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Department", "Location", "Policy", "Version", "Status", "Submitted on", "Approved by", "Approved on"}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	people := make([]ackPerson, len(list))
	for i, x := range list {
		people[i] = ackPerson{E: x.E, Dept: x.Dept, Loc: x.Loc}
	}
	items, err := ackItems(ctx, q, people, "")
	if err != nil {
		return nil, nil, err
	}
	shown := []ackItem{}
	for _, x := range items {
		if f.DS == "" || x.status() == f.DS {
			shown = append(shown, x)
		}
	}
	// By name, code and policy title.
	sort.SliceStable(shown, func(i, j int) bool {
		a, b := shown[i], shown[j]
		if na, nb := str(a.P.E["name"]), str(b.P.E["name"]); na != nb {
			return na < nb
		}
		if ia, ib := str(a.P.E["id"]), str(b.P.E["id"]); ia != ib {
			return ia < ib
		}
		return str(a.Pol["title"]) < str(b.Pol["title"])
	})
	out := [][]any{}
	for _, x := range shown {
		approvedBy := any("")
		if str(x.R["status"]) == "Approved" {
			approvedBy = x.H["by_name"]
		}
		out = append(out, []any{x.P.E["id"], x.P.E["name"], x.P.Dept, x.P.Loc, x.Pol["title"], x.V["version"], x.status(), repDateIST(x.A["submitted_at"]), approvedBy,
			repDateIST(x.decided("Approved", "at"))})
	}
	return cols, out, nil
}

var repPolicyAckStatus = []repExtra{{K: "ds", L: "Status", O: [][2]string{{"", "All"}, {"Signed", "Signed"}, {"Waiting for manager", "Waiting for manager"}, {"Returned", "Returned"}, {"Not submitted", "Not submitted"}}}}

// ---------- Documents ----------

// canPolicyFile: 'policy' files (owner = a version id, or a DRAFT- key before saving) are uploaded by whoever
// manages work policies; a published version is open to everyone signed in, a draft to policy viewers.
// 'policy_ack' files (owner "<employee id>/<version id>") are uploaded by the employee for themselves, for a
// policy that applies to them, and seen by the employee, their approvers, HR Admin, and approval or policy
// viewers whose scope covers the employee.
func (s *Server) canPolicyFile(ctx context.Context, c *Ctx, kind, owner string, write bool) error {
	if kind == "policy" {
		if write {
			if !draftOwner(owner) {
				return bad("Upload the policy document before saving the policy")
			}
			return c.Need("pol.policies", 2)
		}
		if draftOwner(owner) {
			return c.Need("pol.policies", 2)
		}
		if n, ok := invNumericID(owner); ok {
			if published, _ := s.st.DB.C("company_policy_version").Exists(ctx, bson.M{"_id": n, "published_at": bson.M{"$ne": nil}}); published {
				return nil
			}
		}
		return c.Need("pol.policies", 1)
	}
	emp, ver, _ := strings.Cut(owner, "/")
	if emp == "" || ver == "" || len(owner) > 80 {
		return bad("Say which policy the signed copy is for")
	}
	if write {
		if emp != c.EmployeeID {
			return forbidden("Upload your own signed copy")
		}
		items, _ := ackItemsOf(ctx, s.st.DB, bson.M{"_id": emp}, "")
		for _, x := range items {
			if fmt.Sprint(x.V["id"]) == ver {
				return nil
			}
		}
		return bad("This policy does not apply to you, or a newer version has been published")
	}
	if emp == c.EmployeeID || c.Role == "HR Admin" {
		return nil
	}
	if c.Level("appr.queue") >= 1 || c.Level("pol.policies") >= 1 {
		if ok, _ := c.InScope(ctx, s.st.DB, emp); ok {
			return nil
		}
	}
	// The named approver of the employee's acknowledgement, wherever their scope is.
	reqs, _ := s.st.DB.C("approval_request").Find(ctx, bson.M{"wf": "p_ack", "employee_id": emp, "status": "Pending"}, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	for _, x := range reqs {
		if req, err := loadRequest(ctx, s.st.DB, x["id"].(string), false); err == nil && canAct(ctx, s.st.DB, c, req) {
			return nil
		}
	}
	return forbidden("")
}

// ---------- Approvals: who else can act ----------

// ackDeputy: HRBPs (and HR Admin, who may act on anything) can decide an acknowledgement in their scope,
// besides the reporting manager. Their inbox still shows only what is named to them; they act from Open.
func ackDeputy(ctx context.Context, q store.Q, c *Ctx, r *Request) bool {
	if r.WF != "p_ack" || (c.Role != "HRBP" && c.Role != "HRBP Corporate") || c.Level("appr.queue") < 2 {
		return false
	}
	ok, _ := c.InScope(ctx, q, r.EmployeeID)
	return ok
}

// ackNamed reports whether the caller is the person named for the request's current stage.
func ackNamed(ctx context.Context, q store.Q, c *Ctx, r *Request) bool {
	if r.Step >= len(r.Chain) {
		return false
	}
	sub, err := loadSubject(ctx, q, r.EmployeeID)
	if err != nil {
		return false
	}
	return resolvePerson(ctx, q, r.Chain[r.Step], sub, r.Facts).ID == c.EmployeeID
}

// ackOnBehalf is how an HRBP's decision is recorded when they are not the person named for the stage.
func ackOnBehalf(ctx context.Context, q store.Q, c *Ctx, r *Request, stage string) string {
	if r.WF != "p_ack" || c.Role == "HR Admin" || approval.StageRole(stage) == c.Role || ackNamed(ctx, q, c, r) {
		return c.Name
	}
	return c.Name + " (" + c.Role + ", on behalf of " + stage + ")"
}
