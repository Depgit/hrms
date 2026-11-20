package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
// Levels:
//   - Sure: the only portal employee with the same mobile (the number used by one person on each side) and the
//     name or a date agrees; or the only one with the same name and at least one more thing agrees (date of birth,
//     date of joining, department, designation, location).
//   - Likely: the same mobile but a different name; the same name and nothing else to go by; one of several with
//     the name whose date of birth or joining agrees; or the only similar name (spelling, word order, initials)
//     with the department or a date agreeing.
//   - Check: several candidates; HR picks one.
//   - None: no candidate (the person may have no portal code yet).
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
	ID, Name, Dept, Desig, Loc, Mobile, DOB, DOJ string
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

var rmHonorifics = map[string]bool{"mr": true, "mrs": true, "ms": true, "miss": true, "dr": true, "shri": true, "sri": true, "smt": true,
	"kumari": true, "km": true, "kum": true}

// rmTokens is a name's words in lower case, without punctuation and honorifics; nil for a name with no letters
// (the portal lists some people under their card number).
func rmTokens(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if !rmHonorifics[w] {
			out = append(out, w)
		}
	}
	return out
}

// rmSameName: the same words (order does not matter).
func rmSameName(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, " ") == strings.Join(y, " ")
}

// rmSimilarName: one name's words all in the other (at least two of them, or one and the other's initial), or a
// small spelling difference (Vikas Kumar / Vikash Kumar).
func rmSimilarName(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if rmSameName(a, b) {
		return true
	}
	small, big := a, b
	if len(small) > len(big) {
		small, big = big, small
	}
	in := map[string]bool{}
	for _, w := range big {
		in[w] = true
	}
	shared := 0
	for _, w := range small {
		if in[w] {
			shared++
		}
	}
	if shared == len(small) && shared >= 2 {
		return true
	}
	x, y := strings.Join(a, " "), strings.Join(b, " ")
	if len(x) >= 6 && len(y) >= 6 {
		d := rmLev(x, y)
		if n := max(len(x), len(y)); float64(n-d)/float64(n) >= 0.85 {
			return true
		}
	}
	return false
}

func rmLev(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// rmText compares masters loosely: lower case letters and digits only; equal, or one contains the other (4+).
func rmText(a, b string, drop ...string) bool {
	norm := func(s string) string {
		var sb strings.Builder
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			skip := false
			for _, d := range drop {
				if w == d {
					skip = true
				}
			}
			if !skip {
				sb.WriteString(w)
			}
		}
		return sb.String()
	}
	x, y := norm(a), norm(b)
	if x == "" || y == "" {
		return false
	}
	return x == y || (len(x) >= 4 && len(y) >= 4 && (strings.Contains(x, y) || strings.Contains(y, x)))
}

// rmAgree lists what else agrees between an HRMS employee and a portal employee.
func rmAgree(e rmHR, p realtime.Employee) []string {
	var out []string
	if e.DOB != "" && e.DOB == p.DOB {
		out = append(out, "date of birth")
	}
	if e.DOJ != "" && e.DOJ == p.DOJ {
		out = append(out, "date of joining")
	}
	if rmText(e.Dept, p.Dept) {
		out = append(out, "department")
	}
	if rmText(e.Desig, p.Desig) {
		out = append(out, "designation")
	}
	if rmText(e.Loc, p.Branch, "saltstayz", "pause", "office") {
		out = append(out, "location")
	}
	return out
}

func rmHas(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
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

// rmPlanFor proposes a portal code for each HRMS employee. used holds the normalised codes HRMS employees already
// have.
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
	toks := make([][]string, len(free))
	pPhone := map[string]int{}
	for i, p := range free {
		toks[i] = rmTokens(p.Name)
		if p.Phone != "" {
			pPhone[p.Phone]++
		}
	}
	hPhone := map[string]int{}
	for _, e := range hr {
		if e.Mobile != "" {
			hPhone[e.Mobile]++
		}
	}
	touched := map[int]bool{} // portal rows proposed or listed as a candidate
	pickOf := map[int][]int{} // portal row → proposals (Sure/Likely) that picked it
	for _, e := range hr {
		et := rmTokens(e.Name)
		pr := rmProp{EmployeeID: e.ID, Name: e.Name, Dept: e.Dept, Level: rmLevelNone, Reason: "No portal employee with this mobile or a similar name"}
		pick := -1
		var phone, same, similar []int
		for i, p := range free {
			if e.Mobile != "" && p.Phone == e.Mobile && pPhone[p.Phone] == 1 && hPhone[e.Mobile] == 1 {
				phone = append(phone, i)
			}
			if rmSameName(et, toks[i]) {
				same = append(same, i)
			} else if rmSimilarName(et, toks[i]) {
				similar = append(similar, i)
			}
		}
		cands := func(ix []int, why func(int) string) []rmCand {
			var out []rmCand
			for _, i := range ix {
				if len(out) == rmMaxCands {
					break
				}
				out = append(out, rmCandOf(free[i], why(i)))
				touched[i] = true
			}
			return out
		}
		agreeText := func(i int) string {
			if a := rmAgree(e, free[i]); len(a) > 0 {
				return "same " + strings.Join(a, ", ")
			}
			return ""
		}
		switch {
		case len(phone) == 1:
			i := phone[0]
			a := rmAgree(e, free[i])
			pick = i
			if rmSimilarName(et, toks[i]) || rmHas(a, "date of birth") || rmHas(a, "date of joining") {
				pr.Level, pr.Reason = rmLevelSure, "Same mobile"
				if rmSameName(et, toks[i]) {
					pr.Reason += " and name"
				} else if rmSimilarName(et, toks[i]) {
					pr.Reason += ", similar name"
				}
			} else {
				pr.Level, pr.Reason = rmLevelLikely, "Same mobile, but the name differs ("+free[i].Name+")"
			}
			if len(a) > 0 {
				pr.Reason += "; same " + strings.Join(a, ", ")
			}
		case len(same) == 1:
			i := same[0]
			pick = i
			if a := rmAgree(e, free[i]); len(a) > 0 {
				pr.Level, pr.Reason = rmLevelSure, "Same name; same "+strings.Join(a, ", ")
			} else {
				pr.Level, pr.Reason = rmLevelLikely, "Same name only (nothing else to compare)"
			}
		case len(same) > 1:
			var dated []int
			for _, i := range same {
				if a := rmAgree(e, free[i]); rmHas(a, "date of birth") || rmHas(a, "date of joining") {
					dated = append(dated, i)
				}
			}
			if len(dated) == 1 {
				pick = dated[0]
				pr.Level, pr.Reason = rmLevelLikely, fmt.Sprintf("One of %d with this name; same %s", len(same), strings.Join(rmAgree(e, free[dated[0]]), ", "))
			} else {
				pr.Level, pr.Reason = rmLevelCheck, fmt.Sprintf("%d portal employees have this name", len(same))
				pr.Cands = cands(same, agreeText)
			}
		case len(similar) == 1:
			i := similar[0]
			a := rmAgree(e, free[i])
			if rmHas(a, "department") || rmHas(a, "date of birth") || rmHas(a, "date of joining") {
				pick = i
				pr.Level, pr.Reason = rmLevelLikely, "Similar name ("+free[i].Name+"); same "+strings.Join(a, ", ")
			} else {
				pr.Level, pr.Reason = rmLevelCheck, "Similar name only"
				pr.Cands = cands(similar, agreeText)
			}
		case len(similar) > 1:
			pr.Level, pr.Reason = rmLevelCheck, fmt.Sprintf("%d portal employees have a similar name", len(similar))
			// the ones with something else agreeing first
			sort.SliceStable(similar, func(x, y int) bool { return len(rmAgree(e, free[similar[x]])) > len(rmAgree(e, free[similar[y]])) })
			pr.Cands = cands(similar, agreeText)
		}
		if pick >= 0 {
			c := rmCandOf(free[pick], "")
			pr.Pick = &c
			touched[pick] = true
			pickOf[pick] = append(pickOf[pick], len(plan.Props))
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
		SetProjection(bson.M{"_id": 1, "name": 1, "doj": 1, "department_id": 1, "designation_id": 1, "location_id": 1, "attendance_code": 1}))
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
	pers := map[string]store.Row{}
	if len(ids) > 0 {
		rows, err := q.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"employee_id": 1, "mobile": 1, "dob": 1}))
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			pers[str(r["employee_id"])] = r
		}
	}
	var out []rmHR
	for _, e := range emps {
		id := str(e["id"])
		if str(e["attendance_code"]) != "" {
			continue
		}
		p := pers[id]
		mob := reDigitsOnly(str(p["mobile"]))
		if len(mob) > 10 {
			mob = mob[len(mob)-10:]
		} else if len(mob) < 10 {
			mob = ""
		}
		out = append(out, rmHR{ID: id, Name: str(e["name"]), Dept: names["department"][str(e["department_id"])], Desig: names["designation"][str(e["designation_id"])],
			Loc: names["location"][str(e["location_id"])], Mobile: mob, DOB: str(p["dob"]), DOJ: str(e["doj"])})
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
		"portal": len(portal), "used": plan.Used, "hr": len(hr), "codes": codes, "props": props, "leftover": left})
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
