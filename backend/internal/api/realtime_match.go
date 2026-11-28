package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/realtime"
	"github.com/saltstayz/hrms/internal/store"
)

// Matching portal codes (owner, 16 Nov 2026): HR should not type ~2,000 Attendance codes by hand. "Fetch employee
// list" downloads the Realtime portal's list of active employees (internal/realtime EmployeeList: name, codes,
// phone, dates of birth and joining, department, designation, branch) and proposes, for each HRMS employee who is
// not inactive and has no Attendance code, the portal code that is the same person. Nothing is applied by itself:
// HR ticks the proposals and applies them, or downloads them in the Employee master update format to edit and
// upload through Bulk update.
//
// Codes only (owner, 28 Nov 2026): many people share a name, so names, similar names, mobiles and dates never decide.
//   - Sure: the only free portal employee whose code (EmpCode, else CardNo) is the number of the employee's Employee
//     ID, or of a Joining ID they had before the Employee ID (alias), ignoring leading zeros: EMP-3201 / JI-3201 ↔
//     003201.
//   - Check: that number is on more than one portal employee, or proposed for two HRMS employees; HR picks.
//   - None: no portal code has that number. HR gives the code by Bulk update (Employee master update).
// The portal's name is shown next to the code for HR to look at; it is never used to choose.
//
// A portal code already given to an HRMS employee is never proposed, and a code proposed for two people is shown
// as Check for both. Only what the proposals need is kept (code, card, name, department, designation, branch, date
// of joining); the portal's phone numbers and dates of birth are used for matching and not stored. The result is
// company_setting.realtime_match.

const (
	rmLevelSure   = "Sure"
	rmLevelLikely = "Likely"
	rmLevelCheck  = "Check"
	rmLevelNone   = "None"
	rmMaxCands    = 5
	rmMaxApply    = 3000
	rmStaleAfter  = 20 * time.Minute
)

var rmLevels = []string{rmLevelSure, rmLevelLikely, rmLevelCheck, rmLevelNone}

// rtFetchEmployees downloads and reads the portal's employee list. Tests replace it.
var rtFetchEmployees = func(ctx context.Context, cfg realtime.Config) ([]realtime.Employee, error) {
	c, err := realtime.New(cfg)
	if err != nil {
		return nil, err
	}
	b, err := c.EmployeeList(ctx)
	if err != nil {
		return nil, err
	}
	return realtime.ParseEmployees(b)
}

// ---------- Matching (pure) ----------

// rmHR is an HRMS employee to find a portal code for.
type rmHR struct {
	ID, Name, Dept, Desig, Loc, DOJ string   // name and the rest are shown, never matched on
	Aliases                         []string // Joining IDs the record had before its Employee ID
}

// rmIDNumber is the number in an Employee or Joining ID without leading zeros ("" when it has no digits): the
// portal code the HRMS gives (ptIDCode pads it to 6 digits).
func rmIDNumber(id string) string {
	d := reDigitsOnly(id)
	if d == "" {
		return ""
	}
	return rtNorm(d)
}

// rmCand is a portal employee as shown with a proposal.
type rmCand struct {
	Code   string `json:"code" bson:"code"`
	Card   string `json:"card" bson:"card"`
	Name   string `json:"name" bson:"name"`
	Dept   string `json:"dept" bson:"dept"`
	Desig  string `json:"desig" bson:"desig"`
	Branch string `json:"branch" bson:"branch"`
	DOJ    string `json:"doj" bson:"doj"`
	Why    string `json:"why,omitempty" bson:"why,omitempty"`
}

// rmProp is the proposal for one HRMS employee.
type rmProp struct {
	EmployeeID string   `json:"employeeId" bson:"employeeId"`
	Name       string   `json:"name" bson:"name"`
	Dept       string   `json:"dept" bson:"dept"`
	Level      string   `json:"level" bson:"level"`
	Reason     string   `json:"reason" bson:"reason"`
	Pick       *rmCand  `json:"pick,omitempty" bson:"pick,omitempty"` // Sure and Likely
	Cands      []rmCand `json:"cands,omitempty" bson:"cands,omitempty"`
}

type rmPlan struct {
	Props    []rmProp // one per HRMS employee, in the order given
	Leftover []rmCand // portal employees neither used nor proposed nor a candidate
	Used     int      // portal employees whose code an HRMS employee already has
}

func rmCandOf(p realtime.Employee, why string) rmCand {
	return rmCand{Code: p.Code, Card: p.Card, Name: p.Name, Dept: p.Dept, Desig: p.Desig, Branch: p.Branch, DOJ: p.DOJ, Why: why}
}

// rmCodeOf is the code to give an employee for a portal row: its EmpCode, else its CardNo.
func rmCodeOf(p realtime.Employee) string {
	if strings.TrimSpace(p.Code) != "" {
		return strings.TrimSpace(p.Code)
	}
	return strings.TrimSpace(p.Card)
}

// rmPlanFor proposes a portal code for each HRMS employee, by code only: the portal employee whose code (or card)
// is the number of the employee's Employee ID or earlier Joining ID. used holds the normalised codes HRMS employees
// already have.
func rmPlanFor(hr []rmHR, portal []realtime.Employee, used map[string]bool) rmPlan {
	var plan rmPlan
	free := make([]realtime.Employee, 0, len(portal))
	for _, p := range portal {
		if used[rtNorm(p.Code)] || used[rtNorm(p.Card)] {
			plan.Used++
			continue
		}
		free = append(free, p)
	}
	byNo := map[string][]int{} // normalised code or card → free portal rows
	for i, p := range free {
		seen := map[string]bool{}
		for _, k := range []string{rtNorm(p.Code), rtNorm(p.Card)} {
			if k != "" && !seen[k] {
				seen[k] = true
				byNo[k] = append(byNo[k], i)
			}
		}
	}
	touched := map[int]bool{} // portal rows proposed or listed as a candidate
	pickOf := map[int][]int{} // portal row → proposals that picked it
	for _, e := range hr {
		pr := rmProp{EmployeeID: e.ID, Name: e.Name, Dept: e.Dept, Level: rmLevelNone,
			Reason: "No portal code is this Employee ID's number. Give the code by Bulk update › Employee master update"}
		var hits []int
		var via []string
		seen := map[int]bool{}
		for _, id := range append([]string{e.ID}, e.Aliases...) {
			n := rmIDNumber(id)
			if n == "" {
				continue
			}
			for _, i := range byNo[n] {
				if !seen[i] {
					seen[i] = true
					hits = append(hits, i)
					via = append(via, id)
				}
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			i := hits[0]
			c := rmCandOf(free[i], "")
			pr.Level, pr.Reason, pr.Pick = rmLevelSure, "Portal code "+rmCodeOf(free[i])+" is the number of "+via[0], &c
			touched[i] = true
			pickOf[i] = append(pickOf[i], len(plan.Props))
		default:
			pr.Level, pr.Reason = rmLevelCheck, fmt.Sprintf("%d portal employees have this number as code or card", len(hits))
			for k, i := range hits {
				if k == rmMaxCands {
					break
				}
				pr.Cands = append(pr.Cands, rmCandOf(free[i], "number of "+via[k]))
				touched[i] = true
			}
		}
		plan.Props = append(plan.Props, pr)
	}
	// A portal code proposed for two people: both become Check, with it as their candidate.
	for i, ps := range pickOf {
		if len(ps) < 2 {
			continue
		}
		for _, k := range ps {
			pr := &plan.Props[k]
			var others []string
			for _, o := range ps {
				if o != k {
					others = append(others, plan.Props[o].Name+" ("+plan.Props[o].EmployeeID+")")
				}
			}
			c := rmCandOf(free[i], pr.Reason)
			pr.Level, pr.Reason, pr.Pick = rmLevelCheck, "Proposed for "+strings.Join(others, ", ")+" too", nil
			pr.Cands = append([]rmCand{c}, pr.Cands...)
		}
	}
	for i, p := range free {
		if !touched[i] {
			plan.Leftover = append(plan.Leftover, rmCandOf(p, ""))
		}
	}
	return plan
}

// ---------- A run ----------

func rmLoad(ctx context.Context, q store.Q) (bson.M, error) {
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(bson.M{"realtime_match": 1}))
	if err != nil || r == nil {
		return bson.M{}, err
	}
	switch m := r["realtime_match"].(type) {
	case map[string]any:
		return bson.M(m), nil
	case bson.M:
		return m, nil
	}
	return bson.M{}, nil
}

func rmSet(ctx context.Context, q store.Q, set bson.M) error {
	// The field starts as null (the registry adds it that way), and a dotted $set cannot go inside a null.
	if _, err := q.C("company_setting").Update(ctx, bson.M{"_id": 1, "realtime_match": bson.M{"$not": bson.M{"$type": "object"}}},
		bson.M{"$set": bson.M{"realtime_match": bson.M{}}}); err != nil {
		return err
	}
	s := bson.M{}
	for k, v := range set {
		s["realtime_match."+k] = v
	}
	_, err := q.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": s})
	return err
}

func rmRunning(m bson.M) map[string]any {
	r, _ := m["running"].(map[string]any)
	if r == nil {
		if b, ok := m["running"].(bson.M); ok {
			r = b
		}
	}
	if r == nil {
		return nil
	}
	if t, ok := r["since"].(time.Time); ok && time.Since(t) > rmStaleAfter {
		return nil
	}
	return r
}

// rmHRMS is every HRMS employee who is not inactive, with the details matching uses, and the codes already given.
func rmHRMS(ctx context.Context, q store.Q) ([]rmHR, map[string]bool, error) {
	emps, err := q.C("employee").Find(ctx, bson.M{"status": bson.M{"$ne": "Inactive"}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetProjection(bson.M{"_id": 1, "name": 1, "doj": 1, "department_id": 1, "designation_id": 1, "location_id": 1, "attendance_code": 1, "alias_ids": 1}))
	if err != nil {
		return nil, nil, err
	}
	used, err := rmUsed(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	names := map[string]map[string]string{}
	for _, coll := range []string{"department", "designation", "location"} {
		rows, err := q.C(coll).Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"_id": 1, "name": 1}))
		if err != nil {
			return nil, nil, err
		}
		names[coll] = map[string]string{}
		for _, r := range rows {
			names[coll][str(r["id"])] = str(r["name"])
		}
	}
	var ids []string
	for _, e := range emps {
		if str(e["attendance_code"]) == "" {
			ids = append(ids, str(e["id"]))
		}
	}
	var out []rmHR
	for _, e := range emps {
		id := str(e["id"])
		if str(e["attendance_code"]) != "" {
			continue
		}
		out = append(out, rmHR{ID: id, Name: str(e["name"]), Dept: names["department"][str(e["department_id"])], Desig: names["designation"][str(e["designation_id"])],
			Loc: names["location"][str(e["location_id"])], DOJ: str(e["doj"]), Aliases: strs(e["alias_ids"])})
	}
	return out, used, nil
}

// rmUsed is every Attendance code already given (normalised), whoever has it.
func rmUsed(ctx context.Context, q store.Q) (map[string]bool, error) {
	used := map[string]bool{}
	all, err := q.C("employee").Find(ctx, bson.M{"attendance_code": bson.M{"$nin": bson.A{nil, ""}}}, options.Find().SetProjection(bson.M{"attendance_code": 1}))
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		used[rtNorm(str(e["attendance_code"]))] = true
	}
	return used, nil
}

func reDigitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rmRun fetches the portal's employee list and stores fresh proposals.
func (s *Server) rmRun(ctx context.Context, by string) error {
	fail := func(err error) error {
		_ = rmSet(context.WithoutCancel(ctx), s.st.DB, bson.M{"running": nil, "error": err.Error(), "error_at": time.Now()})
		return err
	}
	cfg, ok := rtEnv()
	if !ok {
		return fail(fmt.Errorf("the portal account is not set: add REALTIME_URL, REALTIME_USER and REALTIME_PASS to the server's environment (backend/.env) and restart"))
	}
	portal, err := rtFetchEmployees(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	if len(portal) == 0 {
		return fail(fmt.Errorf("the portal's employee list came back empty"))
	}
	hr, used, err := rmHRMS(ctx, s.st.DB)
	if err != nil {
		return fail(err)
	}
	s.machineVerifyAfterFetch(ctx, portal, by) // tick the punch-machine to-dos the list shows done (machines.go)
	plan := rmPlanFor(hr, portal, used)
	codes := make([]string, 0, len(portal)) // every portal code, so Apply accepts only real ones
	for _, p := range portal {
		codes = append(codes, rmCodeOf(p))
	}
	props := make([]any, len(plan.Props))
	for i, p := range plan.Props {
		props[i] = p
	}
	left := make([]any, len(plan.Leftover))
	for i, p := range plan.Leftover {
		left[i] = p
	}
	return rmSet(ctx, s.st.DB, bson.M{"running": nil, "error": nil, "error_at": nil, "fetched_at": time.Now(), "by": by,
		"portal": len(portal), "used": plan.Used, "hr": len(hr), "codes": codes, "props": props, "leftover": left, "by_code": true})
}

// RealtimeMatchArgs is a "Fetch employee list" run.
type RealtimeMatchArgs struct {
	By string `json:"by"`
}

func (RealtimeMatchArgs) Kind() string { return "realtime_match" }

func (RealtimeMatchArgs) InsertOpts() jobs.InsertOpts { return jobs.InsertOpts{MaxAttempts: 1} }

type realtimeMatchWorker struct {
	jobs.WorkerDefaults[RealtimeMatchArgs]
	s *Server
}

func (w *realtimeMatchWorker) Timeout(*jobs.Job[RealtimeMatchArgs]) time.Duration {
	return 15 * time.Minute
}

func (w *realtimeMatchWorker) Work(ctx context.Context, j *jobs.Job[RealtimeMatchArgs]) error {
	if err := w.s.rmRun(ctx, j.Args.By); err != nil {
		log.Printf("realtime portal employee list: %v", err)
	}
	return nil
}

// RealtimeMatchRun fetches and matches at once (tests, and the command line) and returns a one-line summary.
func (s *Server) RealtimeMatchRun(ctx context.Context, by string) (string, error) {
	if err := s.rmRun(ctx, by); err != nil {
		return "", err
	}
	m, err := rmLoad(ctx, s.st.DB)
	if err != nil {
		return "", err
	}
	props, left, err := rmCurrent(ctx, s.st.DB, m)
	if err != nil {
		return "", err
	}
	n := map[string]int{}
	for _, p := range props {
		n[p.Level]++
	}
	return fmt.Sprintf("%v portal employees (%v already have their code), %v HRMS employees without a code: %d Sure, %d Likely, %d Check, %d None; %d portal employees match nobody",
		m["portal"], m["used"], m["hr"], n[rmLevelSure], n[rmLevelLikely], n[rmLevelCheck], n[rmLevelNone], len(left)), nil
}

// ---------- Reading the stored result ----------

func rmDecodeProps(v any) []rmProp {
	var out []rmProp
	arr, _ := v.(bson.A)
	if arr == nil {
		if a, ok := v.([]any); ok {
			arr = a
		}
	}
	for _, x := range arr {
		b, err := bson.Marshal(x)
		if err != nil {
			continue
		}
		var p rmProp
		if bson.Unmarshal(b, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func rmDecodeCands(v any) []rmCand {
	var out []rmCand
	arr, _ := v.(bson.A)
	if arr == nil {
		if a, ok := v.([]any); ok {
			arr = a
		}
	}
	for _, x := range arr {
		b, err := bson.Marshal(x)
		if err != nil {
			continue
		}
		var p rmCand
		if bson.Unmarshal(b, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// rmCurrent is the stored proposals as they stand now: people who have since been given a code, or left, are
// dropped, and codes given since are no longer proposed (a Sure or Likely whose code is gone becomes None, or
// Check when other candidates remain).
func rmCurrent(ctx context.Context, q store.Q, m bson.M) ([]rmProp, []rmCand, error) {
	if m["by_code"] != true { // a list from before 28 Nov 2026 was matched by names: never offered, fetch again
		return nil, nil, nil
	}
	props, left := rmDecodeProps(m["props"]), rmDecodeCands(m["leftover"])
	if len(props) == 0 && len(left) == 0 {
		return props, left, nil
	}
	used, err := rmUsed(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	open := map[string]bool{}
	emps, err := q.C("employee").Find(ctx, bson.M{"status": bson.M{"$ne": "Inactive"}, "attendance_code": bson.M{"$in": bson.A{nil, ""}}}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, nil, err
	}
	for _, e := range emps {
		open[str(e["id"])] = true
	}
	taken := func(c rmCand) bool { return used[rtNorm(c.Code)] || used[rtNorm(c.Card)] }
	out := props[:0]
	for _, p := range props {
		if !open[p.EmployeeID] {
			continue
		}
		if p.Pick != nil && taken(*p.Pick) {
			p.Pick = nil
			p.Level, p.Reason = rmLevelNone, "The proposed portal code has since been given to someone else"
		}
		cs := p.Cands[:0]
		for _, c := range p.Cands {
			if !taken(c) {
				cs = append(cs, c)
			}
		}
		p.Cands = cs
		if p.Level == rmLevelCheck && len(p.Cands) == 0 {
			p.Level, p.Reason = rmLevelNone, "Every candidate's code has since been given to someone else"
		}
		out = append(out, p)
	}
	l := left[:0]
	for _, c := range left {
		if !taken(c) {
			l = append(l, c)
		}
	}
	return out, l, nil
}

// ---------- Routes ----------

// realtimeMatchGet: the latest proposals, filtered by level and a search, a page at a time.
func (s *Server) realtimeMatchGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	m, err := rmLoad(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	props, left, err := rmCurrent(ctx, s.st.DB, m)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, l := range rmLevels {
		counts[l] = 0
	}
	for _, p := range props {
		counts[p.Level]++
	}
	qv := r.URL.Query()
	level, search := qv.Get("level"), strings.ToLower(strings.TrimSpace(qv.Get("q")))
	page, _ := strconv.Atoi(qv.Get("page"))
	size, _ := strconv.Atoi(qv.Get("size"))
	page, size = max(page, 1), min(max(size, 1), 500)
	if qv.Get("size") == "" {
		size = 50
	}
	var rows []rmProp
	for _, p := range props {
		if level != "" && level != "All" && p.Level != level {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(p.Name+" "+p.EmployeeID), search) {
			continue
		}
		rows = append(rows, p)
	}
	total := len(rows)
	from := min((page-1)*size, total)
	rows = rows[from:min(from+size, total)]
	if rows == nil {
		rows = []rmProp{}
	}
	_, envOK := rtEnv()
	return map[string]any{"envSet": envOK, "running": rmRunning(m), "fetchedAt": m["fetched_at"], "by": m["by"], "error": m["error"], "errorAt": m["error_at"],
		"portal": m["portal"], "used": m["used"], "counts": counts, "leftover": len(left), "total": total, "page": page, "size": size, "rows": rows}, nil
}

// realtimeMatchStart fetches the portal's employee list in the background.
func (s *Server) realtimeMatchStart(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	if _, ok := rtEnv(); !ok {
		return nil, bad("Add REALTIME_URL, REALTIME_USER and REALTIME_PASS to the server's environment first")
	}
	ctx := r.Context()
	m, err := rmLoad(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	if rmRunning(m) != nil {
		return nil, bad("The employee list is already being fetched. Wait for it to finish")
	}
	if err := rmSet(ctx, s.st.DB, bson.M{"running": bson.M{"since": time.Now(), "by": c.Name}}); err != nil {
		return nil, err
	}
	_ = audit(ctx, s.st.DB, c, "Employee list fetched from the Realtime portal to match Attendance codes", "company_setting", "1", nil, nil)
	if s.jobs != nil {
		if _, err := s.jobs.Insert(context.WithoutCancel(ctx), RealtimeMatchArgs{By: c.Name}, nil); err != nil {
			return nil, err
		}
	} else {
		go func() { _ = s.rmRun(context.WithoutCancel(ctx), c.Name) }()
	}
	return map[string]any{"started": true}, nil
}

type rmApplyRes struct {
	EmployeeID string `json:"employeeId"`
	Name       string `json:"name"`
	Code       string `json:"code"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// realtimeMatchApply gives the ticked employees their portal codes. Each pair is checked like the Attendance code
// column of Bulk update (letters and digits, nobody else has it), and the code must be one of the portal's.
func (s *Server) realtimeMatchApply(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Pairs []struct {
			EmployeeID string `json:"employeeId"`
			Code       string `json:"code"`
		} `json:"pairs"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.Pairs) == 0 {
		return nil, bad("Tick at least one proposal")
	}
	if len(b.Pairs) > rmMaxApply {
		return nil, bad("Apply at most %d at a time", rmMaxApply)
	}
	ctx := r.Context()
	m, err := rmLoad(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	portal := map[string]string{} // normalised code or card → the code to store
	if arr, ok := m["codes"].(bson.A); ok {
		for _, x := range arr {
			portal[rtNorm(str(x))] = str(x)
		}
	}
	for _, p := range rmDecodeProps(m["props"]) { // cards too
		for _, cd := range append(p.Cands, func() []rmCand {
			if p.Pick != nil {
				return []rmCand{*p.Pick}
			}
			return nil
		}()...) {
			portal[rtNorm(cd.Card)] = rmCodeOf(realtime.Employee{Code: cd.Code, Card: cd.Card})
		}
	}
	for _, cd := range rmDecodeCands(m["leftover"]) {
		portal[rtNorm(cd.Card)] = rmCodeOf(realtime.Employee{Code: cd.Code, Card: cd.Card})
	}
	out := make([]rmApplyRes, 0, len(b.Pairs))
	seenEmp, seenCode := map[string]bool{}, map[string]bool{}
	applied := 0
	const chunk = 200
	for start := 0; start < len(b.Pairs); start += chunk {
		part := b.Pairs[start:min(start+chunk, len(b.Pairs))]
		var res []rmApplyRes
		n := 0
		err := s.st.Tx(ctx, func(tx store.Q) error {
			res, n = res[:0], 0
			for _, p := range part {
				id, code := strings.ToUpper(strings.TrimSpace(p.EmployeeID)), strings.TrimSpace(p.Code)
				rr := rmApplyRes{EmployeeID: id, Code: code}
				e, err := tx.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1, "status": 1, "attendance_code": 1}))
				if err != nil {
					return err
				}
				norm := rtNorm(code)
				canon, isPortal := portal[norm]
				switch {
				case e == nil:
					rr.Error = "No such employee"
				case str(e["status"]) == "Inactive":
					rr.Error = "The employee is inactive"
				case str(e["attendance_code"]) != "":
					rr.Error = "Already has Attendance code " + str(e["attendance_code"]) + ". Change it through Bulk update › Employee master update"
				case code == "":
					rr.Error = "No code given"
				case !isPortal:
					rr.Error = "Code " + code + " is not in the portal's employee list. Fetch the list again"
				case seenEmp[id]:
					rr.Error = "The employee appears twice in this request"
				case seenCode[norm]:
					rr.Error = "Code " + code + " is given to two employees in this request"
				}
				if e != nil {
					rr.Name = str(e["name"])
				}
				if rr.Error == "" {
					msg, err := attCodeProblem(ctx, tx, canon, id)
					if err != nil {
						return err
					}
					rr.Error = msg
				}
				if rr.Error == "" {
					if _, err := tx.C("employee").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"attendance_code": canon}}); err != nil {
						return err
					}
					if err := audit(ctx, tx, c, "Attendance code "+canon+" given from the Realtime portal match", "employee", id,
						map[string]any{"attendance_code": nil}, map[string]any{"attendance_code": canon}); err != nil {
						return err
					}
					rr.OK, rr.Code = true, canon
					n++
				}
				seenEmp[id] = true
				if norm != "" {
					seenCode[norm] = true
				}
				res = append(res, rr)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		applied += n
		out = append(out, res...)
	}
	return map[string]any{"applied": applied, "failed": len(out) - applied, "results": out}, nil
}

// realtimeMatchSheet: the proposals in the Employee master update format (Employee ID and Attendance code filled,
// everything else blank, which keeps the current values), or the portal employees with no HRMS match.
func (s *Server) realtimeMatchSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	m, err := rmLoad(ctx, s.st.DB)
	if err != nil {
		return "", nil, nil, err
	}
	props, left, err := rmCurrent(ctx, s.st.DB, m)
	if err != nil {
		return "", nil, nil, err
	}
	day := time.Now().In(ist).Format("2006-01-02")
	if r.URL.Query().Get("what") == "portal" {
		cols := []string{"Portal code", "Card number", "Name in the portal", "Department", "Designation", "Branch", "Date of joining"}
		rows := make([][]any, 0, len(left))
		for _, p := range left {
			rows = append(rows, []any{p.Code, p.Card, p.Name, p.Dept, p.Desig, p.Branch, p.DOJ})
		}
		return "portal-employees-not-in-hrms-" + day + ".xlsx", cols, rows, nil
	}
	var cols []string
	for _, t := range bulkTemplates {
		if t.Key == "upd" {
			cols = t.Cols
		}
	}
	idI, codeI := -1, -1
	for i, h := range cols {
		switch h {
		case "Employee ID":
			idI = i
		case attCodeCol:
			codeI = i
		}
	}
	if idI < 0 || codeI < 0 {
		return "", nil, nil, fmt.Errorf("the Employee master update template has no Attendance code column")
	}
	want := map[string]bool{}
	for _, l := range strings.Split(or(r.URL.Query().Get("levels"), rmLevelSure+","+rmLevelLikely), ",") {
		want[strings.TrimSpace(l)] = true
	}
	var rows [][]any
	for _, p := range props {
		if p.Pick == nil || !want[p.Level] {
			continue
		}
		row := make([]any, len(cols))
		for i := range row {
			row[i] = ""
		}
		row[idI], row[codeI] = p.EmployeeID, rmCodeOf(realtime.Employee{Code: p.Pick.Code, Card: p.Pick.Card})
		rows = append(rows, row)
	}
	return "attendance-codes-" + day + ".xlsx", cols, rows, nil
}
