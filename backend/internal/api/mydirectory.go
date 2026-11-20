package api

// My view › Employee directory (owner, 12 Nov 2026): everyone in the company with their email, departments with
// their headcount, and any department as an org chart by reporting manager. Only active people are listed, each with
// name, designation, department, location and the one email on file (employee_personal.email); never the employee
// code, mobile, pay, birth date or ID. A person is named across these answers by `ref`, a keyed hash of the
// employee ID that cannot be turned back into the code.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// dirRef is the directory's handle for a person: HMAC-SHA256 of the employee ID under a key derived from the
// server's secret, 16 characters. The same person has the same ref in the people list and in the chart.
func (s *Server) dirRef(id string) string {
	k := hmac.New(sha256.New, s.drive.key)
	k.Write([]byte("directory-ref"))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:16]
}

// dirCaller is the caller's department and whether they are active: only active employees see the directory.
func (s *Server) dirCaller(ctx context.Context, c *Ctx) (dept string, active bool, err error) {
	me, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.M{"department_id": 1, "status": 1}))
	if err != nil || me == nil {
		return "", false, err
	}
	return str(me["department_id"]), me["status"] == "Active", nil
}

// dirEmails is the email on file for each of the given employees (blank ones left out).
func dirEmails(ctx context.Context, q store.Q, ids []any) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"employee_id": 1, "email": 1}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if e := strings.TrimSpace(str(r["email"])); e != "" {
			out[str(r["employee_id"])] = e
		}
	}
	return out, nil
}

// dirAllNames maps every document of a small master collection to its name.
func dirAllNames(ctx context.Context, q store.Q, coll string) (map[string]string, error) {
	rows, err := q.C(coll).Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[str(r["id"])] = str(r["name"])
	}
	return out, nil
}

// dirPerson is one active employee as the directory knows them. id never leaves the server.
type dirPerson struct {
	id, name, deptID, dept, desig, locID, loc, email string
	words                                            []string // lower-case words of the searchable fields
	text                                             string   // the searchable fields, lower case, joined
}

// dirPeople is every active employee with their department, designation, location and email, by name then id.
func dirPeople(ctx context.Context, q store.Q) ([]*dirPerson, error) {
	emps, err := q.C("employee").Find(ctx, bson.M{"status": "Active"}, options.Find().SetProjection(bson.M{"name": 1, "department_id": 1, "designation_id": 1, "location_id": 1}))
	if err != nil {
		return nil, err
	}
	names := map[string]map[string]string{}
	for _, coll := range []string{"department", "designation", "location"} {
		if names[coll], err = dirAllNames(ctx, q, coll); err != nil {
			return nil, err
		}
	}
	ids := make([]any, 0, len(emps))
	for _, e := range emps {
		ids = append(ids, e["id"])
	}
	emails, err := dirEmails(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*dirPerson, 0, len(emps))
	for _, e := range emps {
		id := str(e["id"])
		p := &dirPerson{id: id, name: str(e["name"]), deptID: str(e["department_id"]), locID: str(e["location_id"]), email: emails[id]}
		p.dept, p.desig, p.loc = names["department"][p.deptID], names["designation"][str(e["designation_id"])], names["location"][p.locID]
		p.text = strings.ToLower(strings.Join([]string{p.name, p.desig, p.dept, p.loc, p.email}, " "))
		p.words = dirWords(p.text)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].id < out[j].id
	})
	return out, nil
}

// ---------- approximate search ----------

// dirWords splits lower-case text into words of letters and digits ("ravi.k@saltstayz.com" → ravi, k, saltstayz, com).
func dirWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// dirDist is the optimal string alignment distance between a and b (insert, delete, change or swap two neighbours),
// stopping early once it exceeds max.
func dirDist(a, b []rune, max int) int {
	if d := len(a) - len(b); d > max || -d > max {
		return max + 1
	}
	prev2, prev, cur := make([]int, len(b)+1), make([]int, len(b)+1), make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				v = min(v, prev2[j-2]+1)
			}
			cur[j] = v
			best = min(best, v)
		}
		if best > max {
			return max + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(b)]
}

// dirTokenScore is how well one query word fits a person's words: 0 a word starts with it, 1 it is inside a word,
// 2 a word (or the start of one) is within a small typo of it (one edit for 4 to 7 letters, two from 8); -1 no fit.
func dirTokenScore(t string, words []string) int {
	best := -1
	for _, w := range words {
		if strings.HasPrefix(w, t) {
			return 0
		}
		if best != 1 && strings.Contains(w, t) {
			best = 1
		}
	}
	if best >= 0 {
		return best
	}
	tr := []rune(t)
	if len(tr) < 4 {
		return -1
	}
	k := 1
	if len(tr) >= 8 {
		k = 2
	}
	for _, w := range words {
		wr := []rune(w)
		// The whole word, or its start (the query may be a partial word with a typo in it).
		for l := len(tr) - k; l <= len(tr)+k; l++ {
			if l >= 1 && l <= len(wr) && dirDist(tr, wr[:l], k) <= k {
				return 2
			}
		}
	}
	return -1
}

// dirMatch scores a person against a query: every query word of 2 or more characters must fit one of their words, in
// any order. Lower is better; ok is false when a word fits nothing. The whole query found as typed scores best.
func dirMatch(query string, p *dirPerson) (score int, ok bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0, true
	}
	if strings.Contains(p.text, q) {
		return -1, true
	}
	// Single letters ("f&b" → f, b; an initial in an email) would fit nearly everyone: they count only as part of
	// the query as typed.
	toks := []string{}
	for _, t := range dirWords(q) {
		if len([]rune(t)) >= 2 {
			toks = append(toks, t)
		}
	}
	if len(toks) == 0 {
		return 0, false
	}
	for _, t := range toks {
		s := dirTokenScore(t, p.words)
		if s < 0 {
			return 0, false
		}
		score += s
	}
	return score, true
}

// ---------- handlers ----------

// dirRow is one person in the people list: only these fields are ever sent.
type dirRow struct {
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	Designation string `json:"designation"`
	Department  string `json:"department"`
	Dept        string `json:"dept"` // the department's id, to open its chart
	Location    string `json:"location"`
	Email       string `json:"email,omitempty"`
}

// myDirectoryPeople is GET /api/me/directory/people?q=&dept=&loc=&page=&size=: the active people of the company,
// searched (name, designation, department, location, email; words in any order, partial words, small typos) and
// filtered by department and location ids, best match first (then by name), a page at a time (50, up to 100).
func (s *Server) myDirectoryPeople(r *http.Request, c *Ctx) (any, error) {
	ctx, qv := r.Context(), r.URL.Query()
	page, _ := strconv.Atoi(qv.Get("page"))
	size, _ := strconv.Atoi(qv.Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 50
	}
	out := map[string]any{"rows": []dirRow{}, "total": 0, "page": page, "size": size, "pages": 1}
	_, active, err := s.dirCaller(ctx, c)
	if err != nil || !active {
		return out, err
	}
	all, err := dirPeople(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	dept, loc, query := qv.Get("dept"), qv.Get("loc"), qv.Get("q")
	type hit struct {
		p     *dirPerson
		score int
	}
	hits := []hit{}
	for _, p := range all {
		if (dept != "" && p.deptID != dept) || (loc != "" && p.locID != loc) {
			continue
		}
		if sc, ok := dirMatch(query, p); ok {
			hits = append(hits, hit{p, sc})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score < hits[j].score }) // people are already by name
	total := len(hits)
	pages := max(1, (total+size-1)/size)
	rows := []dirRow{}
	for _, h := range hits[min((page-1)*size, total):min(page*size, total)] {
		p := h.p
		rows = append(rows, dirRow{Ref: s.dirRef(p.id), Name: p.name, Designation: p.desig, Department: p.dept, Dept: p.deptID, Location: p.loc, Email: p.email})
	}
	out["rows"], out["total"], out["pages"] = rows, total, pages
	return out, nil
}

// myDirectoryDepartments is GET /api/me/directory/departments: the departments and locations that have active
// people, each with its headcount, by name, and the caller's own department (`mine`).
func (s *Server) myDirectoryDepartments(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	type group struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Count int    `json:"count"`
		Mine  bool   `json:"mine,omitempty"`
	}
	mine, active, err := s.dirCaller(ctx, c)
	out := map[string]any{"mine": mine, "total": 0, "departments": []group{}, "locations": []group{}}
	if err != nil || !active {
		return out, err
	}
	all, err := dirPeople(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	count := func(key func(*dirPerson) (string, string)) []group {
		by := map[string]*group{}
		for _, p := range all {
			id, name := key(p)
			if by[id] == nil {
				by[id] = &group{ID: id, Name: name, Mine: id == mine}
			}
			by[id].Count++
		}
		l := []group{}
		for _, g := range by {
			l = append(l, *g)
		}
		sort.Slice(l, func(i, j int) bool {
			if l[i].Name != l[j].Name {
				return l[i].Name < l[j].Name
			}
			return l[i].ID < l[j].ID
		})
		return l
	}
	out["departments"] = count(func(p *dirPerson) (string, string) { return p.deptID, p.dept })
	out["locations"] = count(func(p *dirPerson) (string, string) { return p.locID, p.loc })
	out["total"] = len(all)
	return out, nil
}

// myDirectory is GET /api/me/directory?dept=<department id>: the active people in a department (the caller's own
// by default) as an org chart (tree by reporting manager), with name, designation, location and email. Managers
// outside the department who manage someone in it are included as the tree's top, marked outside. People are keyed
// by a number for this answer only, and carry the directory's ref, so no employee code or ID is sent.
func (s *Server) myDirectory(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	mine, active, err := s.dirCaller(ctx, c)
	if err != nil {
		return nil, err
	}
	id := r.URL.Query().Get("dept")
	if id == "" {
		id = mine
	}
	var d store.Row
	if id != "" {
		if d, err = s.st.DB.C("department").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1})); err != nil {
			return nil, err
		}
	}
	if d == nil {
		return nil, notFound("Department not found")
	}
	type node struct {
		Key         int    `json:"key"`
		Ref         string `json:"ref"`
		Name        string `json:"name"`
		Designation string `json:"designation"`
		Location    string `json:"location"`
		Email       string `json:"email,omitempty"`
		Manager     int    `json:"manager"`
		Outside     bool   `json:"outside,omitempty"`
		Me          bool   `json:"me,omitempty"`
	}
	out := map[string]any{"department": str(d["name"]), "id": id, "mine": id == mine, "nodes": []node{}}
	if !active {
		return out, nil
	}
	rows, err := myviewOrgChart(ctx, s.st.DB, id)
	if err != nil {
		return nil, err
	}
	key, ids := map[string]int{}, make([]any, 0, len(rows))
	for i, x := range rows {
		key[str(x["id"])] = i + 1
		ids = append(ids, x["id"])
	}
	emails, err := dirEmails(ctx, s.st.DB, ids)
	if err != nil {
		return nil, err
	}
	nodes := []node{}
	for i, x := range rows {
		eid := str(x["id"])
		nodes = append(nodes, node{Key: i + 1, Ref: s.dirRef(eid), Name: str(x["name"]), Designation: str(x["designation"]), Location: str(x["location"]),
			Email: emails[eid], Manager: key[str(x["manager_id"])], Outside: x["outside"] == true, Me: eid == c.EmployeeID})
	}
	out["nodes"] = nodes
	return out, nil
}

// myviewOrgChart is the active people of a department (with their manager) and, at the top, the active managers
// from outside it who manage someone in it (marked outside, without a manager), each with name, designation and
// location: outside first, then by name and id.
func myviewOrgChart(ctx context.Context, q store.Q, deptID any) ([]store.Row, error) {
	proj := options.Find().SetProjection(bson.M{"name": 1, "manager_id": 1, "designation_id": 1, "location_id": 1})
	dep, err := q.C("employee").Find(ctx, bson.M{"status": "Active", "department_id": deptID}, proj)
	if err != nil {
		return nil, err
	}
	inDep, managers := []any{}, []any{}
	for _, e := range dep {
		e["outside"] = false
		inDep = append(inDep, e["id"])
		if e["manager_id"] != nil {
			managers = append(managers, e["manager_id"])
		}
	}
	top, err := q.C("employee").Find(ctx, bson.M{"status": "Active", "$and": bson.A{bson.M{"_id": bson.M{"$in": managers}}, bson.M{"_id": bson.M{"$nin": inDep}}}}, proj)
	if err != nil {
		return nil, err
	}
	for _, e := range top {
		e["outside"], e["manager_id"] = true, nil
	}
	all := append(dep, top...)
	var desigIDs, locIDs []any
	for _, e := range all {
		desigIDs, locIDs = append(desigIDs, e["designation_id"]), append(locIDs, e["location_id"])
	}
	desigs, err := myviewNames(ctx, q, "designation", desigIDs)
	if err != nil {
		return nil, err
	}
	locs, err := myviewNames(ctx, q, "location", locIDs)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, e := range all {
		g, ok1 := desigs[str(e["designation_id"])]
		l, ok2 := locs[str(e["location_id"])]
		if ok1 && ok2 {
			out = append(out, store.Row{"id": e["id"], "manager_id": e["manager_id"], "outside": e["outside"], "name": e["name"], "designation": g, "location": l})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a["outside"] != b["outside"] {
			return a["outside"] == true
		}
		if str(a["name"]) != str(b["name"]) {
			return str(a["name"]) < str(b["name"])
		}
		return str(a["id"]) < str(b["id"])
	})
	return out, nil
}
