package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// The prototype's "Download all details" groups beyond job, personal, IDs, bank and salary: the work policy and
// leave, the approvers of the common requests, and the service history (XSEC policy, appr, hist).

type exportCtx struct {
	ts     *timeSet
	cals   map[string]string
	routes map[string]string
	fy     string
}

func newExportCtx(ctx context.Context, q store.Q) (*exportCtx, error) {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return nil, err
	}
	x := &exportCtx{ts: ts, cals: map[string]string{}, routes: map[string]string{}, fy: calc.FY(today()[:7])}
	rows, _ := q.C("holiday_calendar").Find(ctx, nil)
	for _, r := range rows {
		x.cals[r["id"].(string)] = r["name"].(string)
	}
	return x, nil
}

// policyCols: Work policy, Mapped through, Probation ends, Weekly off, Casual / Privilege / Sick leave days,
// Notice period (days), Holiday calendar.
func (x *exportCtx) policyCols(ctx context.Context, q store.Q, e *emp, canLeave bool) []string {
	p, via := x.ts.policyFor(e)
	probEnds := p.S.ProbEnd(e.Grade, e.DOJ) // the grade's probation when it is set
	days := func(t string) string {
		if !canLeave {
			return "0"
		}
		if c := x.ts.typeCfg(p, t); c != nil && c.On {
			return trimNum(c.Days)
		}
		return "0"
	}
	notice := ""
	if d, _, err := noticeDaysFor(ctx, q, e.ID); err == nil {
		notice = itoa(d)
	}
	cal := ""
	if id, ok := p.CalMap[e.CityID]; ok {
		cal = x.cals[id]
	} else if p.CalID != nil {
		cal = x.cals[*p.CalID]
	}
	return []string{p.Name, via, probEnds, weekOffLine(p.S), days("Casual leave"), days("Privilege leave"), days("Sick leave"), notice, cal}
}

// apprCols: the approval route of a typical request of each kind, as "Stage: Person > …" (the prototype's export).
// Routes are cached by everything that decides them, so a large export stays quick.
func (x *exportCtx) apprCols(ctx context.Context, q store.Q, id string) []string {
	// The sample leave and regularization fall on the next weekday, so a weekend export still shows a route.
	t := today()
	for d, _ := time.Parse("2006-01-02", t); d.Weekday() == time.Saturday || d.Weekday() == time.Sunday; d, _ = time.Parse("2006-01-02", t) {
		t = policy.AddDays(t, 1)
	}
	reqs := []struct {
		wf string
		p  requestPayload
	}{
		{"leave", requestPayload{LType: "Casual leave", From: t, To: t}},
		{"regularize", requestPayload{Date: t, RType: "No Punch"}},
		{"resign", requestPayload{ExitType: "Resignation"}},
		{"p_claim", requestPayload{Amount: 3000, ClaimType: "Travel"}},
		{"p_adv", requestPayload{Amount: 3000}},
	}
	subj, err := loadSubject(ctx, q, id)
	if err != nil {
		return make([]string, len(reqs))
	}
	out := []string{}
	for _, r := range reqs {
		p := r.p
		f, err := factsFor(ctx, q, r.wf, id, &p)
		if err != nil {
			out = append(out, "")
			continue
		}
		fk, _ := json.Marshal(f)
		mk := func(v *string) string {
			if v == nil {
				return ""
			}
			return *v
		}
		key := strings.Join([]string{r.wf, subj.Grade, subj.LocationID, subj.Department, fmt.Sprint(subj.Corporate), mk(subj.ManagerID), mk(subj.FunctionalManagerID), string(fk)}, "|")
		if v, ok := x.routes[key]; ok {
			out = append(out, v)
			continue
		}
		plan, err := planFor(ctx, q, r.wf, subj, f)
		v := ""
		if err == nil {
			parts := []string{}
			for _, sp := range plan.People {
				parts = append(parts, sp.Stage+": "+sp.Name)
			}
			v = strings.Join(parts, " > ")
		}
		x.routes[key] = v
		out = append(out, v)
	}
	return out
}

// histCols: Times rejoined, First joining date, Previous code, Earlier service.
func histCols(ctx context.Context, q store.Q, id, doj string, prev *string) []string {
	rows, _ := q.C("employee_service").Find(ctx, bson.M{"employee_id": id}, options.Find().SetSort(bson.D{{Key: "doj", Value: 1}, {Key: "_id", Value: 1}}))
	first, earlier := doj, []string{}
	for _, r := range rows {
		if d, _ := r["doj"].(string); d != "" && d < first {
			first = d
		}
		earlier = append(earlier, fmt.Sprintf("%v to %v (%v)", r["doj"], r["lwd"], r["code"]))
	}
	pc := ""
	if prev != nil {
		pc = *prev
	}
	return []string{itoa(len(rows)), first, pc, strings.Join(earlier, "; ")}
}

// salaryExtra: Structure effective from and Tax regime (New unless an Old-regime declaration is on record).
func salaryExtra(ctx context.Context, q store.Q, id, fy string) (string, string) {
	var from, regime string
	// The latest structure in force today (an assignment dated 1990 or earlier is the opening one, not a change).
	if m, _ := q.C("structure_map").FindOne(ctx, bson.M{"employee_id": id, "from_date": bson.M{"$lte": store.Today(), "$gt": "1990-01-01"}},
		options.FindOne().SetSort(bson.D{{Key: "from_date", Value: -1}})); m != nil {
		from = str(m["from_date"])
	}
	if d, _ := q.C("tax_declaration").FindOne(ctx, bson.M{"employee_id": id, "fy": fy}); d != nil {
		regime = str(d["regime"])
	}
	if regime == "" {
		regime = "New"
	}
	return from, regime
}
