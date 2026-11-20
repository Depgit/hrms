package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Dashboard tabs. Every count covers the same people: the caller's scope and the dashboard's filters (dashScope).

// dashScope is who the dashboard counts: the caller's data scope (only the caller, without the dashboard and
// directory rights) narrowed by the City, Department and Type filters — what the old `base` and `scope` SQL with
// its arguments selected from employee e JOIN department d JOIN location l JOIN cluster k JOIN city c. Every
// employee has a department and a location, and every location a cluster and a city, so those joins kept everyone.
// The dispatcher (dashboard, salary_routes.go) builds it with dashScopeOf; dashGroup, dashStatusCounts and
// dashRequests give the Overview's own counts.
type dashScope struct {
	emp  bson.D // over employee documents
	subj bson.D // the same conditions on the approval subject embedded by approvalSubjectStages("e")
}

// dashScopeOf builds the dashboard's scope. city is a city id, dept a department name and typ an employment type;
// "" leaves a filter out.
func (s *Server) dashScopeOf(ctx context.Context, c *Ctx, city, dept, typ string) (dashScope, error) {
	q := s.st.DB
	var locIDs, deptIDs []any
	var err error
	if city != "" { // the locations of the city's clusters
		clusters, err := q.C("cluster").Distinct(ctx, "_id", bson.M{"city_id": city})
		if err != nil {
			return dashScope{}, err
		}
		if locIDs, err = q.C("location").Distinct(ctx, "_id", bson.M{"cluster_id": bson.M{"$in": clusters}}); err != nil {
			return dashScope{}, err
		}
	}
	if dept != "" {
		if deptIDs, err = q.C("department").Distinct(ctx, "_id", bson.M{"name": dept}); err != nil {
			return dashScope{}, err
		}
	}
	build := func(prefix string) (bson.D, error) {
		and := bson.A{}
		if c.Level("dash.overview") >= 1 && c.Level("emp.directory") >= 1 {
			f, err := c.ScopeFilter(ctx, q, prefix)
			if err != nil {
				return nil, err
			}
			and = append(and, f)
		} else {
			id := "_id"
			if prefix != "" {
				id = prefix + "id"
			}
			and = append(and, bson.D{{Key: id, Value: c.EmployeeID}})
		}
		if locIDs != nil {
			and = append(and, bson.D{{Key: prefix + "location_id", Value: bson.D{{Key: "$in", Value: locIDs}}}})
		}
		if deptIDs != nil {
			and = append(and, bson.D{{Key: prefix + "department_id", Value: bson.D{{Key: "$in", Value: deptIDs}}}})
		}
		if typ != "" {
			and = append(and, bson.D{{Key: prefix + "type", Value: typ}})
		}
		return bson.D{{Key: "$and", Value: and}}, nil
	}
	var sc dashScope
	if sc.emp, err = build(""); err != nil {
		return dashScope{}, err
	}
	if sc.subj, err = build("e."); err != nil {
		return dashScope{}, err
	}
	return sc, nil
}

// stillEmployed is the scope's employees whose status is not Inactive.
func (sc dashScope) stillEmployed() bson.D {
	return bson.D{{Key: "$and", Value: bson.A{sc.emp, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}}}}}
}

// dashEmps are the employees matching filter (dashScope.emp or narrower) with the fields a tab needs (and id).
func dashEmps(ctx context.Context, q store.Q, filter bson.D, fields ...string) ([]store.Row, error) {
	proj := bson.D{}
	for _, f := range fields {
		proj = append(proj, bson.E{Key: f, Value: 1})
	}
	return q.C("employee").Find(ctx, filter, options.Find().SetProjection(proj))
}

// dashStatusCounts counts the scope's employees by status (the Overview's headline numbers).
func dashStatusCounts(ctx context.Context, q store.Q, sc dashScope) (active, inactive, preboarding int64, err error) {
	emps, err := dashEmps(ctx, q, sc.emp, "status")
	for _, e := range emps {
		switch e["status"] {
		case "Active":
			active++
		case "Inactive":
			inactive++
		case "Pre-boarding":
			preboarding++
		}
	}
	return active, inactive, preboarding, err
}

// dashGroup is the Overview's headcount by "city", "type" or "department": rows {label, active, total}, the most
// active first (ties by label). Known divergence, pending the owner's decision: the PostgreSQL build's SQL was
// `GROUP BY 1 ORDER BY 2 DESC` with no tie-breaker, so tied groups came out in HashAggregate order (where each
// label fell in the hash table), which cannot practically be reproduced; ties here are alphabetical instead.
func dashGroup(ctx context.Context, q store.Q, sc dashScope, by string) ([]store.Row, error) {
	emps, err := dashEmps(ctx, q, sc.emp, "status", "type", "department_id", "location_id")
	if err != nil {
		return nil, err
	}
	label := func(e store.Row) any { return e["type"] }
	switch by {
	case "department":
		depts, err := ctxIndex(ctx, q, "department", "_id", ctxValues(emps, "department_id"))
		if err != nil {
			return nil, err
		}
		label = func(e store.Row) any { return depts[str(e["department_id"])]["name"] }
	case "city":
		locs, err := ctxIndex(ctx, q, "location", "_id", ctxValues(emps, "location_id"))
		if err != nil {
			return nil, err
		}
		locRows := []store.Row{}
		for _, l := range locs {
			locRows = append(locRows, l)
		}
		clusters, err := ctxIndex(ctx, q, "cluster", "_id", ctxValues(locRows, "cluster_id"))
		if err != nil {
			return nil, err
		}
		clusterRows := []store.Row{}
		for _, k := range clusters {
			clusterRows = append(clusterRows, k)
		}
		cities, err := ctxIndex(ctx, q, "city", "_id", ctxValues(clusterRows, "city_id"))
		if err != nil {
			return nil, err
		}
		label = func(e store.Row) any {
			return cities[str(clusters[str(locs[str(e["location_id"])]["cluster_id"])]["city_id"])]["name"]
		}
	}
	groups, order := map[string]store.Row{}, []string{}
	for _, e := range emps {
		l := label(e)
		k := fmt.Sprint(l)
		g := groups[k]
		if g == nil {
			g = store.Row{"label": l, "active": int64(0), "total": int64(0)}
			groups[k] = g
			order = append(order, k)
		}
		if e["status"] == "Active" {
			g["active"] = g["active"].(int64) + 1
		}
		g["total"] = g["total"].(int64) + 1
	}
	out := make([]store.Row, len(order))
	for i, k := range order {
		out[i] = groups[k]
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i]["active"].(int64), out[j]["active"].(int64); a != b {
			return a > b
		}
		return fmt.Sprint(out[i]["label"]) < fmt.Sprint(out[j]["label"])
	})
	return out, nil
}

// dashOverview adds the month's movement, attrition, tenure and the "Needs attention" list.
func (s *Server) dashOverview(ctx context.Context, c *Ctx, sc dashScope, mine int) (map[string]any, error) {
	emps, err := dashEmps(ctx, s.st.DB, sc.emp, "doj", "lwd", "status", "exit_status", "manager_id", "functional_manager_id")
	if err != nil {
		return nil, err
	}
	now := store.Today()
	yearAgo := policy.AddDays(now, -365)
	var joined, left, notice, exits12, actv int64
	var tenureDays int
	working := []store.Row{}
	for _, e := range emps {
		doj, lwd, status := str(e["doj"]), str(e["lwd"]), str(e["status"])
		if dashPart(doj, 0, 7) == now[:7] {
			joined++
		}
		if status == "Inactive" {
			if dashPart(lwd, 0, 7) == now[:7] {
				left++
			}
			if lwd != "" && lwd >= yearAgo && lwd <= now {
				exits12++
			}
			continue
		}
		working = append(working, e)
		actv++
		tenureDays += policy.DayDiff(doj, now)
		if e["exit_status"] == "On notice" {
			notice++
		}
	}
	tenure := 0.0 // average years of service of the people still employed
	if actv > 0 {
		tenure = float64(tenureDays) / 365.0 / float64(actv)
	}
	attrition := 0.0
	if actv > 0 && exits12 > 0 {
		attrition = math.Round(float64(exits12)/(float64(actv)+float64(exits12)/2)*1000) / 10
	}
	active := bson.M{"status": "Active"}
	cities, _ := s.st.DB.C("city").Count(ctx, active)
	clusters, _ := s.st.DB.C("cluster").Count(ctx, active)
	regions, _ := s.st.DB.C("region").Count(ctx, active)

	type alert struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Page   string `json:"page"`
	}
	alerts := []alert{}
	plural := func(n int64, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	// The prototype's "Needs attention" list, in its order: roster discipline, deviations, improvement plans,
	// nominations, a manager who left, approvals, missing details, confirmations due, exits. Each count is the same
	// the linked page shows (the same handlers or queries).
	if c.Level("ros.roster") >= 1 {
		if ra, err := s.rosterAlerts(ctx, c, today()[:7]); err == nil && len(ra) > 0 {
			alerts = append(alerts, alert{"Roster", plural(int64(len(ra)), "location is", "locations are") + " above the late change limit", "/roster"})
		}
		if rq, err := http.NewRequestWithContext(ctx, "GET", "/?status=Pending", nil); err == nil {
			if d, err := s.deviationList(rq, c); err == nil {
				if n := d.(map[string]any)["pending"].(int); n > 0 {
					alerts = append(alerts, alert{"Roster deviations", plural(int64(n), "planned against actual difference", "planned against actual differences") + " waiting for a decision", "/roster"})
				}
			}
		}
		if pips, err := s.apprUIQueue(ctx, c, "mine", "p_pip"); err == nil && len(pips) > 0 {
			alerts = append(alerts, alert{"PIP approvals", plural(int64(len(pips)), "improvement plan", "improvement plans") + " waiting for your decision", "/roster"})
		}
	}
	if c.Level("pay.claims") >= 1 {
		if rq, err := http.NewRequestWithContext(ctx, "GET", "/", nil); err == nil {
			if q, err := s.nominationQueue(rq, c); err == nil {
				m := q.(map[string]any)
				waiting, overdue := len(m["submitted"].([]store.Row)), 0
				for _, p := range m["pending"].([]store.Row) {
					if p["overdue"] == true {
						overdue++
					}
				}
				if waiting > 0 || overdue > 0 {
					t := fmt.Sprintf("%d waiting to be verified", waiting)
					if overdue > 0 {
						t += fmt.Sprintf(", %d overdue from employees", overdue)
					}
					alerts = append(alerts, alert{"Nominations", t, "/payroll?tab=claims&sub=nom"})
				}
			}
		}
	}
	if c.Role == "HR Admin" || c.Level("emp.directory") >= 2 {
		if orphans := dashOrphans(ctx, s.st.DB, working); orphans > 0 {
			alerts = append(alerts, alert{"Reporting manager", plural(orphans, "employee reports", "employees report") + " to a manager who has left", "/approvals"})
		}
	}
	if mine > 0 {
		alerts = append(alerts, alert{"Approvals", fmt.Sprintf("%d waiting on %s", mine, c.Role), "/approvals"})
	}
	if c.Level("emp.directory") >= 1 {
		if missing := dashMissingDetails(ctx, s.st.DB, working); missing > 0 {
			alerts = append(alerts, alert{"Details", plural(missing, "person", "people") + " with PAN, Aadhaar, bank or salary pending", "/employees"})
		}
	}
	if c.Level("emp.directory") >= 1 {
		// Probation ends within 30 days, from each person's work policy and grade.
		if ts, err := loadTimeSet(ctx, s.st.DB); err == nil {
			if emps, err := scopedEmps(ctx, s.st.DB, c, bson.M{"status": "Active"}); err == nil {
				var due int64
				t := today()
				for _, e := range emps {
					p, _ := ts.policyFor(e)
					if p == nil {
						continue
					}
					if end := p.S.ProbEnd(e.Grade, e.DOJ); end != "" && end >= t && policy.DayDiff(t, end) <= 30 {
						due++
					}
				}
				if due > 0 {
					alerts = append(alerts, alert{"Probation", plural(due, "confirmation", "confirmations") + " due in 30 days", "/reports"})
				}
			}
		}
	}
	if t, page := s.ackAlert(ctx, c); t != "" { // company policies (policy_ack.go)
		alerts = append(alerts, alert{"Company policies", t, page})
	}
	if notice > 0 {
		alerts = append(alerts, alert{"Exits", fmt.Sprintf("%d on notice", notice), "/employees"})
	}
	return map[string]any{"joinedMonth": joined, "leftMonth": left, "onNotice": notice, "attrition": attrition,
		"avgTenure": math.Round(tenure*10) / 10, "cities": cities, "clusters": clusters, "regions": regions, "alerts": alerts}, nil
}

// dashOrphans counts the people (still employed) whose reporting or functional manager has left.
func dashOrphans(ctx context.Context, q store.Q, working []store.Row) int64 {
	mgrs := append(ctxValues(working, "manager_id"), ctxValues(working, "functional_manager_id")...)
	if len(mgrs) == 0 {
		return 0
	}
	gone, _ := q.C("employee").Distinct(ctx, "_id", bson.M{"_id": bson.M{"$in": mgrs}, "status": "Inactive"})
	left := map[any]bool{}
	for _, id := range gone {
		left[id] = true
	}
	var n int64
	for _, e := range working {
		if (e["manager_id"] != nil && left[e["manager_id"]]) || (e["functional_manager_id"] != nil && left[e["functional_manager_id"]]) {
			n++
		}
	}
	return n
}

// dashMissingDetails counts the people (still employed) without both PAN and Aadhaar on record, without a bank
// account, or without a monthly gross (imported without a salary, owner 14 Nov 2026: set it with Salary breakup).
func dashMissingDetails(ctx context.Context, q store.Q, working []store.Row) int64 {
	ids := ctxValues(working, "id")
	if len(ids) == 0 {
		return 0
	}
	filled := bson.M{"$nin": bson.A{nil, ""}}
	withIDs, _ := q.C("employee_gov_id").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}, "pan": filled, "aadhaar": filled})
	withBank, _ := q.C("employee_bank").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}})
	withGross, _ := q.C("gross_history").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}, "gross": bson.M{"$gt": 0}})
	ok := map[any]int{}
	for _, id := range withIDs {
		ok[id]++
	}
	for _, id := range withBank {
		ok[id]++
	}
	for _, id := range withGross {
		ok[id]++
	}
	var n int64
	for _, id := range ids {
		if ok[id] < 3 {
			n++
		}
	}
	return n
}

// dashPart is d[from:to] of a YYYY-MM-DD date: the year (0, 4), year and month (0, 7) or month (5, 7); "" for a
// missing date.
func dashPart(d string, from, to int) string {
	if len(d) < to {
		return ""
	}
	return d[from:to]
}

type bar struct {
	Label string `json:"label"`
	N     int64  `json:"n"`
}

// dashBars turns counts into bars, largest first (ties by label), or by label when byLabel. Ties by label are the
// same known divergence as in dashGroup (byGender, byWorkflow, byStage were `GROUP BY 1 ORDER BY 2 DESC` in the
// PostgreSQL build, ties in hash order).
func dashBars(counts map[string]int64, byLabel bool) []bar {
	out := []bar{}
	for k, n := range counts {
		out = append(out, bar{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if !byLabel && out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// dashWorkforce: six-month joiners, exits and month-end headcount; grade, gender, tenure, largest locations.
func (s *Server) dashWorkforce(ctx context.Context, sc dashScope) (any, error) {
	type month struct {
		Label     string `json:"label"`
		Joined    int64  `json:"joined"`
		Left      int64  `json:"left"`
		Headcount int64  `json:"headcount"`
	}
	q := s.st.DB
	emps, err := dashEmps(ctx, q, sc.emp, "doj", "lwd", "status", "grade", "location_id")
	if err != nil {
		return nil, err
	}
	trend := []month{}
	now := time.Now()
	for i := 5; i >= 0; i-- {
		start := time.Date(now.Year(), now.Month()-time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, -1)
		from, to := start.Format("2006-01-02"), end.Format("2006-01-02")
		m := month{Label: start.Format("Jan 06")}
		for _, e := range emps {
			doj, lwd, inactive := str(e["doj"]), str(e["lwd"]), e["status"] == "Inactive"
			if doj >= from && doj <= to {
				m.Joined++
			}
			if inactive && lwd != "" && lwd >= from && lwd <= to {
				m.Left++
			}
			if doj <= to && (lwd == "" || lwd > to || !inactive) { // on the books at the month's end
				m.Headcount++
			}
		}
		trend = append(trend, m)
	}
	// The rest counts people still employed.
	working := []store.Row{}
	for _, e := range emps {
		if e["status"] != "Inactive" {
			working = append(working, e)
		}
	}
	personal, _ := ctxIndex(ctx, q, "employee_personal", "employee_id", ctxValues(working, "id"))
	locs, _ := ctxIndex(ctx, q, "location", "_id", ctxValues(working, "location_id"))
	day := store.Today()
	var birthdays, anniversaries int64
	byGrade, byGender, byTenure, byLoc := map[string]int64{}, map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, e := range working {
		doj, p := str(e["doj"]), personal[str(e["id"])]
		if dashPart(str(p["dob"]), 5, 7) == day[5:7] {
			birthdays++
		}
		if dashPart(doj, 5, 7) == day[5:7] && dashPart(doj, 0, 4) < day[:4] {
			anniversaries++
		}
		byGrade[str(e["grade"])]++
		gender := str(p["gender"])
		if gender == "" {
			gender = "Not recorded"
		}
		byGender[gender]++
		switch days := policy.DayDiff(doj, day); {
		case days < 365:
			byTenure["1 Under 1 year"]++
		case days < 730:
			byTenure["2 1 to 2 years"]++
		case days < 1095:
			byTenure["3 2 to 3 years"]++
		default:
			byTenure["4 3 years and more"]++
		}
		if l := locs[str(e["location_id"])]; l != nil {
			byLoc[str(l["name"])]++
		}
	}
	tenure := dashBars(byTenure, true)
	for i := range tenure {
		tenure[i].Label = tenure[i].Label[2:] // drop the sort key
	}
	largest := dashBars(byLoc, false)
	if len(largest) > 8 {
		largest = largest[:8]
	}
	return map[string]any{
		"trend":         trend,
		"byGrade":       dashBars(byGrade, true),
		"byGender":      dashBars(byGender, false),
		"byTenure":      tenure,
		"byLocation":    largest,
		"birthdays":     birthdays,
		"anniversaries": anniversaries,
	}, nil
}

// dashRequests counts the approval requests about people (employees, offers, shortlisted candidates) in the scope:
// by status, and the pending ones by workflow and by the role they wait on.
func dashRequests(ctx context.Context, q store.Q, sc dashScope) (byStatus, pendingWF, pendingStage map[string]int64, err error) {
	pipe := bson.A{}
	for _, st := range approvalSubjectStages("e") {
		pipe = append(pipe, st)
	}
	pipe = append(pipe,
		bson.D{{Key: "$match", Value: sc.subj}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "status", Value: "$status"}, {Key: "wf", Value: "$wf"}, {Key: "stage", Value: bson.D{{Key: "$arrayElemAt", Value: bson.A{"$chain", "$step"}}}}}},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: int64(1)}}}}}})
	groups, err := q.C("approval_request").Aggregate(ctx, pipe)
	if err != nil {
		return nil, nil, nil, err
	}
	byStatus, pendingWF, pendingStage = map[string]int64{}, map[string]int64{}, map[string]int64{}
	for _, g := range groups {
		k, _ := g["id"].(map[string]any)
		n, _ := g["n"].(int64)
		byStatus[str(k["status"])] += n
		if k["status"] != "Pending" {
			continue
		}
		pendingWF[str(k["wf"])] += n
		if stage, ok := k["stage"].(string); ok { // a request past its last stage waits on nobody
			pendingStage[stage] += n
		}
	}
	return byStatus, pendingWF, pendingStage, nil
}

// dashApprovals: pending, approved, rejected and the approval rate for requests in scope.
func (s *Server) dashApprovals(ctx context.Context, c *Ctx, sc dashScope) (any, error) {
	byStatus, pendingWF, pendingStage, _ := dashRequests(ctx, s.st.DB, sc)
	pend, appr, rej := byStatus["Pending"], byStatus["Approved"], byStatus["Rejected"]
	byWf := dashBars(pendingWF, false)
	for i := range byWf {
		byWf[i].Label = workflowName(byWf[i].Label)
	}
	rate := 0.0
	if appr+rej > 0 {
		rate = math.Round(float64(appr) / float64(appr+rej) * 100)
	}
	mine, _ := s.queue(ctx, c, "mine", "")
	return map[string]any{"pending": pend, "approved": appr, "rejected": rej, "rate": rate, "mine": len(mine), "byWorkflow": byWf,
		"byStage": dashBars(pendingStage, false)}, nil
}

// dashCompliance: missing statutory details and master data still used after being made inactive.
func (s *Server) dashCompliance(ctx context.Context, sc dashScope) (any, error) {
	q := s.st.DB
	working, _ := dashEmps(ctx, q, sc.stillEmployed(), "type", "doj", "status")
	ids := ctxValues(working, "id")
	gov, _ := ctxIndex(ctx, q, "employee_gov_id", "employee_id", ids)
	personal, _ := ctxIndex(ctx, q, "employee_personal", "employee_id", ids)
	banked := map[any]bool{}
	if l, err := q.C("employee_bank").Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$in": ids}}); err == nil {
		for _, id := range l {
			banked[id] = true
		}
	}
	gross := dashLatestGross(ctx, q, ids)
	day := store.Today()
	weekAgo := policy.AddDays(day, -7)
	var pan, aadhaar, bank, uan, esi, dob, joiners, pre int64
	for _, e := range working {
		id := str(e["id"])
		g, permanent := gov[id], e["type"] == "Permanent"
		if str(g["pan"]) == "" {
			pan++
		}
		if str(g["aadhaar"]) == "" {
			aadhaar++
		}
		if !banked[id] {
			bank++
		}
		if permanent && statIDClean(str(g["uan"])) == "" {
			uan++
		}
		if permanent && statIDClean(str(g["esic"])) == "" && gross[id] <= 21000 { // within the ESI wage limit
			esi++
		}
		if p := personal[id]; p == nil || p["dob"] == nil {
			dob++
		}
		if doj := str(e["doj"]); doj >= weekAgo && doj <= day {
			joiners++
		}
		if e["status"] == "Pre-boarding" {
			pre++
		}
	}
	// Masters made inactive that people still employed use (counted over everyone, not the scope).
	var stale int64
	for _, m := range []struct{ coll, key, empField string }{
		{"location", "_id", "location_id"}, {"department", "_id", "department_id"}, {"designation", "_id", "designation_id"},
		{"grade", "name", "grade"}, {"shift", "code", "shift_code"}} {
		stale += dashStaleMasters(ctx, q, m.coll, m.key, m.empField)
	}
	return map[string]any{"pan": pan, "aadhaar": aadhaar, "bank": bank, "uan": uan, "esi": esi, "dob": dob,
		"joinersWeek": joiners, "preboarding": pre, "staleMasters": stale}, nil
}

// dashLatestGross is each person's latest monthly gross (people without a gross history are left out: 0).
func dashLatestGross(ctx context.Context, q store.Q, ids []any) map[string]float64 {
	out := map[string]float64{}
	if len(ids) == 0 {
		return out
	}
	rows, _ := q.C("gross_history").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$employee_id"}, {Key: "gross", Value: bson.D{{Key: "$first", Value: "$gross"}}}}}},
	})
	for _, r := range rows {
		g, _ := r["gross"].(float64)
		out[str(r["id"])] = g
	}
	return out
}

// dashStaleMasters counts the inactive masters of coll (matched on key) that an employee still employed uses.
func dashStaleMasters(ctx context.Context, q store.Q, coll, key, empField string) int64 {
	key2 := key
	if key == "_id" {
		key2 = "id"
	}
	inactive, err := q.C(coll).Find(ctx, bson.M{"status": "Inactive"}, options.Find().SetProjection(bson.M{key: 1}))
	if err != nil || len(inactive) == 0 {
		return 0
	}
	used, _ := q.C("employee").Distinct(ctx, empField, bson.M{"status": bson.M{"$ne": "Inactive"}, empField: bson.M{"$in": ctxValues(inactive, key2)}})
	inUse := map[any]bool{}
	for _, v := range used {
		inUse[v] = true
	}
	var n int64
	for _, m := range inactive {
		if inUse[m[key2]] {
			n++
		}
	}
	return n
}

// dashPayroll is the Payroll tab: the last runs, the latest month's cost and statutory totals,
// and what is waiting (holds, pay items, full and final).
func (s *Server) dashPayroll(ctx context.Context, c *Ctx, f map[string]string) (any, error) {
	q := s.st.DB
	months, err := q.C("payroll_month").Find(ctx, bson.M{"status": bson.M{"$in": bson.A{"Approved", "Published"}}}, options.Find().
		SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "ym", Value: 1}, {Key: "status", Value: 1}, {Key: "totals", Value: 1}}).
		SetSort(bson.D{{Key: "ym", Value: -1}}).SetLimit(12))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"months": months, "openYm": openYm(ctx, q)}
	if len(months) > 0 {
		ym := months[0]["ym"].(string)
		rows, err := loadRows(ctx, q, ym)
		if err != nil {
			return nil, err
		}
		rows = filterRows(c, rows, f)
		var pfEe, pfEr, esiEe, esiEr, lwf, tds, employer, earned float64
		byCity := map[string]float64{}
		for _, o := range rows {
			r := o.R
			pfEe += r.EePF
			pfEr += r.ErPF + r.Admin + r.Edli
			esiEe += r.EeESI
			esiEr += r.ErESI
			lwf += r.LwfEe + r.LwfEr
			tds += o.STds + r.Tds
			employer += r.EmployerCost
			earned += o.TotalEarn
			byCity[o.City] += o.TotalEarn + r.EmployerCost
		}
		cities := []map[string]any{}
		for k, v := range byCity {
			cities = append(cities, map[string]any{"label": k, "value": calc.Round2(v)})
		}
		out["latest"] = map[string]any{"ym": ym, "people": len(rows), "earned": calc.Round2(earned), "employer": calc.Round2(employer),
			"pf": calc.Round2(pfEe + pfEr), "esi": calc.Round2(esiEe + esiEr), "lwf": calc.Round2(lwf), "tds": calc.Round2(tds), "byCity": cities}
	}
	holds, _ := q.C("salary_hold").Count(ctx, bson.M{"status": "Active"})
	items, _ := q.C("approval_request").Count(ctx, bson.M{"status": "Pending", "wf": bson.M{"$regex": "^p_"}}) // pay items
	// Full and final due: people on notice, and those who left in the last 60 days without an approved settlement.
	settled, _ := q.C("fnf").Distinct(ctx, "employee_id", bson.M{"status": "Approved"})
	fnf, _ := q.C("employee").Count(ctx, bson.M{"$or": bson.A{
		bson.M{"exit_status": "On notice"},
		bson.M{"status": "Inactive", "lwd": bson.M{"$gt": policy.AddDays(store.Today(), -60)}, "_id": bson.M{"$nin": settled}}}})
	out["holds"], out["pendingItems"], out["fnfDue"] = holds, items, fnf
	return out, nil
}
