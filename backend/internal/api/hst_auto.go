package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Accommodation from the offer (user, 19 Nov 2026). The offer form and the release ask "Company accommodation:
// Yes / No"; Yes needs a cluster that has accommodation units (offer.data.accom = "Yes"/"No",
// offer.data.accomCluster = the cluster id). No pay effect: accommodation is outside payroll.
//
// When the person is marked joined (hireJoin) the HRMS allocates them by itself, in the same transaction: the first
// free bed of the cluster's active units matching their gender, in the suggestion order (the unit serving their
// property first, then the most free beds), check-in on the actual joining date, audited, with the stay note and a
// timeline line "Allocated automatically from the offer". With no free bed they go on the "Waiting for a bed" list
// (employee.hst_wait: cluster_id, since, offer_id) and HR (hst.allot) gets a bell; the maintenance sweep allocates
// them when a bed frees up in that cluster (check-in that day). HR may also check them in by hand, which takes them
// off the list, or drop them from it.

const hstAutoNote = "Allocated automatically from the offer"

// accomTag is the offer's accommodation tag: yes and the cluster.
func accomTag(o store.Row) (bool, string) {
	d := rowData(o)
	return d["accom"] == "Yes", d["accomCluster"]
}

// accomProblems checks the offer form's accommodation answer (data accom, accomCluster): No when not given; Yes
// needs a cluster with an active accommodation unit.
func accomProblems(ctx context.Context, q store.Q, d map[string]string, errs *[]string) error {
	switch d["accom"] {
	case "Yes":
	case "", "No":
		d["accom"] = "No"
		delete(d, "accomCluster")
		return nil
	default:
		*errs = append(*errs, "Company accommodation is Yes or No")
		return nil
	}
	if d["accomCluster"] == "" {
		*errs = append(*errs, "Choose the cluster for the company accommodation")
		return nil
	}
	ok, err := q.C(store.HstUnitColl).Exists(ctx, bson.M{"cluster_id": d["accomCluster"], "status": "Active"})
	if err != nil {
		return err
	}
	if !ok {
		*errs = append(*errs, "The cluster chosen for the accommodation has no accommodation units")
	}
	return nil
}

// accomLine is the offer breakup's line for the tag ("" when No).
func accomLine(ctx context.Context, q store.Q, o store.Row) string {
	yes, cl := accomTag(o)
	if !yes {
		return ""
	}
	name := cl
	if r, _ := q.C("cluster").FindOne(ctx, bson.M{"_id": cl}); r != nil {
		name = str(r["name"])
	}
	return "Company accommodation: Yes, in " + name + " (provided by the company; no change to pay)"
}

// hstAutoAllot books emp a bed in cluster from day, or (first, at joining) puts them on the waiting list. It returns
// the unit and bed ("" when there is no bed). The caller runs it inside a transaction.
func hstAutoAllot(ctx context.Context, tx store.Q, empID, cluster, day, offerID string, first bool) (string, int, error) {
	list, err := loadEmps(ctx, tx, bson.M{"_id": empID})
	if err != nil || len(list) == 0 {
		return "", 0, err
	}
	e := list[0]
	if open, err := tx.C(store.HstStayColl).Exists(ctx, bson.M{"employee_id": empID, "check_out": nil}); err != nil || open {
		return "", 0, err // already staying somewhere
	}
	units, err := hstUnits(ctx, tx, bson.M{"cluster_id": cluster, "status": "Active"})
	if err != nil {
		return "", 0, err
	}
	f := hstOverlapping(day, "")
	f["unit_id"] = bson.M{"$in": hstUnitIDs(units)}
	stays, err := hstStaysOf(ctx, tx, f)
	if err != nil {
		return "", 0, err
	}
	type cand struct {
		u    hstUnitRec
		rank int
		free int
	}
	cands := []cand{}
	for _, u := range units {
		if n := len(hstFreeBeds(u, stays, day, "", "")); n > 0 && hstGenderOK(u.Gender, e.Gender) {
			cands = append(cands, cand{u, hstRankOf(u, e.LocID), n})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return hstBefore(cands[i].rank, cands[i].free, cands[i].u.Name, cands[j].rank, cands[j].free, cands[j].u.Name)
	})
	for _, cd := range cands {
		u := cd.u
		if _, err := tx.C(store.HstUnitColl).Lock(ctx, bson.M{"_id": u.ID}); err != nil {
			return "", 0, err
		}
		if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": e.ID}); err != nil {
			return "", 0, err
		}
		bed, err := hstBook(ctx, tx, &u, e.ID, 0, day, "", "")
		if err != nil {
			continue // taken meanwhile: the next unit
		}
		doc := bson.M{"unit_id": u.ID, "employee_id": e.ID, "bed": bed, "check_in": day, "check_out": nil, "note": hstAutoNote, "created_by": ptAutoBy}
		id, err := tx.C(store.HstStayColl).Insert(ctx, doc)
		if err != nil {
			return "", 0, err
		}
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": e.ID}, bson.M{"$set": bson.M{"hst_wait": nil}}); err != nil {
			return "", 0, err
		}
		if err := audit(ctx, tx, nil, fmt.Sprintf("%s (%s) checked in to %s, bed %d, from %s: %s", e.Name, e.ID, u.Name, bed, longDate(day), hstAutoNote),
			store.HstStayColl, fmt.Sprint(id), nil, doc); err != nil {
			return "", 0, err
		}
		if err := lifeEvent(ctx, tx, nil, lifeEv{Subject: "employee", Emp: e.ID, Action: hstAutoNote, To: fmt.Sprintf("%s, bed %d", u.Name, bed), Date: day,
			Reason: "Company accommodation tagged on the offer"}); err != nil {
			return "", 0, err
		}
		return u.Name, bed, nil
	}
	if !first {
		return "", 0, nil // still waiting (the sweep)
	}
	// No free bed: the waiting list, and a bell to HR.
	wait := map[string]any{"cluster_id": cluster, "since": day, "offer_id": nullIfEmpty(offerID)}
	if _, err := tx.C("employee").Update(ctx, bson.M{"_id": e.ID}, bson.M{"$set": bson.M{"hst_wait": wait}}); err != nil {
		return "", 0, err
	}
	cname := cluster
	if r, _ := tx.C("cluster").FindOne(ctx, bson.M{"_id": cluster}); r != nil {
		cname = str(r["name"])
	}
	if err := audit(ctx, tx, nil, fmt.Sprintf("%s (%s) waits for a bed in %s: no free bed on %s", e.Name, e.ID, cname, longDate(day)), "employee", e.ID, nil, wait); err != nil {
		return "", 0, err
	}
	if err := lifeEvent(ctx, tx, nil, lifeEv{Subject: "employee", Emp: e.ID, Action: "Waiting for a bed", To: cname, Date: day,
		Reason: "Company accommodation tagged on the offer; no free bed in the cluster"}); err != nil {
		return "", 0, err
	}
	to, err := hstRecipients(ctx, tx, "hst.allot")
	if err != nil {
		return "", 0, err
	}
	for who, view := range to {
		if _, err := noticeOnce(ctx, tx, "hst:wait:"+e.ID+":"+day+":"+who, who, fmt.Sprintf("%s (%s) joined with company accommodation in %s, but no bed is free. They are on the waiting list and get the next free bed there.",
			e.Name, e.ID, cname), "/accommodation?tab=waiting", view); err != nil {
			return "", 0, err
		}
	}
	return "", 0, nil
}

// HstWaitSweep gives the people waiting for a bed one as soon as their cluster has one (the maintenance job). It
// returns how many it allocated.
func (s *Server) HstWaitSweep(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.st.DB.C("employee").Find(ctx, bson.M{"hst_wait": bson.M{"$ne": nil}}, options.Find().SetSort(bson.D{{Key: "hst_wait.since", Value: 1}, {Key: "_id", Value: 1}}).
		SetProjection(bson.M{"hst_wait": 1, "status": 1}))
	if err != nil {
		return 0, err
	}
	day, n := store.DateOf(now), 0
	for _, r := range rows {
		w := asMap(r["hst_wait"])
		id := str(r["id"])
		err := s.st.Tx(ctx, func(tx store.Q) error {
			if str(r["status"]) == "Inactive" { // left before a bed came
				_, err := tx.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"hst_wait": nil}})
				return err
			}
			from := str(w["since"])
			if from < day {
				from = day
			}
			unit, _, err := hstAutoAllot(ctx, tx, id, str(w["cluster_id"]), from, str(w["offer_id"]), false)
			if err == nil && unit != "" {
				n++
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *Server) hstWaitSweepLogged(ctx context.Context) {
	if _, err := s.HstWaitSweep(ctx, time.Now()); err != nil {
		log.Printf("accommodation waiting list: %v", err)
	}
}

// hstWaiting is the "Waiting for a bed" list.
func (s *Server) hstWaiting(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	q := s.st.DB
	rows, err := q.C("employee").Find(ctx, bson.M{"hst_wait": bson.M{"$ne": nil}}, options.Find().SetSort(bson.D{{Key: "hst_wait.since", Value: 1}}).
		SetProjection(bson.M{"hst_wait": 1}))
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
	out := []map[string]any{}
	for _, r := range rows {
		w := asMap(r["hst_wait"])
		cl := str(w["cluster_id"])
		if vis != nil && !vis[cl] {
			continue
		}
		list, err := loadEmps(ctx, q, bson.M{"_id": r["id"]})
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			continue
		}
		e := list[0]
		out = append(out, map[string]any{"employeeId": e.ID, "name": e.Name, "gender": e.Gender, "location": e.Location, "clusterId": cl, "cluster": p.cluster(cl),
			"city": p.city(cl), "since": w["since"], "offerId": w["offer_id"]})
	}
	return map[string]any{"rows": out, "canEdit": c.Level("hst.allot") >= 2}, nil
}

// hstWaitDrop takes someone off the waiting list (they no longer need a bed).
func (s *Server) hstWaitDrop(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("hst.allot", 2); err != nil {
		return nil, err
	}
	var b struct {
		Reason string `json:"reason"`
	}
	_ = decode(r, &b)
	if strings.TrimSpace(b.Reason) == "" {
		return nil, bad("Give the reason")
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		e, err := tx.C("employee").Lock(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		if e == nil || e["hst_wait"] == nil {
			return bad("%s is not waiting for a bed", id)
		}
		if _, err := tx.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"hst_wait": nil}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("%s (%s) taken off the waiting list for a bed: %s", e["name"], id, strings.TrimSpace(b.Reason)), "employee", id,
			map[string]any{"hst_wait": e["hst_wait"]}, nil)
	})
}

// hstAccomClusters lists the clusters with active accommodation units, with beds and free beds today: the choices
// of the offer form's "Company accommodation" (HR who raise or release offers, or hold an accommodation right).
func (s *Server) hstAccomClusters(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) && c.Level("hst.allot") < 1 && c.Level("hst.setup") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	q := s.st.DB
	units, err := hstUnits(ctx, q, bson.M{"status": "Active"})
	if err != nil {
		return nil, err
	}
	p, err := hstPlaces(ctx, q)
	if err != nil {
		return nil, err
	}
	day := store.Today()
	f := hstOverlapping(day, "")
	f["unit_id"] = bson.M{"$in": hstUnitIDs(units)}
	stays, err := hstStaysOf(ctx, q, f)
	if err != nil {
		return nil, err
	}
	type row struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		City  string `json:"city"`
		Units int    `json:"units"`
		Beds  int    `json:"beds"`
		Free  int    `json:"free"`
	}
	by := map[string]*row{}
	for _, u := range units {
		x := by[u.ClusterID]
		if x == nil {
			x = &row{ID: u.ClusterID, Name: p.cluster(u.ClusterID), City: p.city(u.ClusterID)}
			by[u.ClusterID] = x
		}
		x.Units++
		x.Beds += u.Beds
		x.Free += len(hstFreeBeds(u, stays, day, "", ""))
	}
	out := []*row{}
	for _, x := range by {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]any{"clusters": out}, nil
}

// accomView is the candidate's accommodation tag from the offer (nil when No) and, after joining, the bed or the
// waiting list.
func accomView(ctx context.Context, q store.Q, x store.Row) map[string]any {
	o, err := q.C("offer").FindOne(ctx, bson.M{"candidate_id": x["id"]})
	if err != nil || o == nil {
		return nil
	}
	yes, cl := accomTag(o)
	if !yes {
		return nil
	}
	out := map[string]any{"clusterId": cl, "cluster": cl, "status": "Allocated automatically when marked joined"}
	if r, _ := q.C("cluster").FindOne(ctx, bson.M{"_id": cl}); r != nil {
		out["cluster"] = str(r["name"])
	}
	emp := str(x["employee_id"])
	if emp == "" || x["ji_at"] == nil {
		return out
	}
	if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": emp}); e != nil && e["hst_wait"] != nil {
		out["status"] = "Waiting for a bed since " + longDate(str(asMap(e["hst_wait"])["since"]))
		return out
	}
	stays, _ := hstStaysOf(ctx, q, bson.M{"employee_id": emp, "check_out": nil})
	if len(stays) > 0 {
		st := stays[len(stays)-1]
		un, _ := q.C(store.HstUnitColl).FindOne(ctx, bson.M{"_id": st.UnitID})
		out["status"] = fmt.Sprintf("Staying at %s, bed %d, from %s", str(un["name"]), st.Bed, longDate(st.In))
	} else {
		out["status"] = "Not staying in company accommodation now"
	}
	return out
}
