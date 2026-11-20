package api

// Approximate people search for GET /api/employees?q= (the people picker and the employee list's search box).
// The same scoring runs in the browser over lists a page already has (frontend/src/components/personMatch.ts), so a
// name is found the same way in every picker:
//
//   - every word of the query must fit one of the person's words, in any order: name, employee code (with or without
//     its prefix: "1023", "emp1023", "EMP-1023"), designation, department, location and city, and the email when the
//     caller may see personal details (emp.personal);
//   - a query word fits a word exactly, as its start, inside it (3 letters or more), or within a small typo (one edit
//     for 4 to 7 letters, two from 8; a swap of two neighbours is one edit), in that order of rank;
//   - a single query word also fits the initials of the name ("rk" → Rakesh Kumar).
//
// Data scope and the other filters are applied first, by the database, exactly as before; the search only orders and
// narrows what the caller may already list. The rows returned are the same directory rows as without q.

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Ranks of one query word against one person word. Every rank is far above the next, so a better kind of fit always
// wins; name and code words add a little over the other fields.
const (
	pmExact    = 400
	pmPrefix   = 300
	pmContains = 200
	pmTypo     = 100
	pmNameBump = 20
	// pmFewHits: when the plain word search finds fewer people than this, the typo-tolerant pass looks at everyone in
	// the caller's scope (the client shows the best 20).
	pmFewHits = 20
)

// pmPerson is one person as the search sees them.
type pmPerson struct {
	id, name                        string
	nameWords, otherWords, initials []string
	nameText                        string
	desig, dept, loc, city, email   string
}

// pmWords splits text into lower-case words of letters and digits ("ravi.k@saltstayz.com" → ravi, k, saltstayz, com).
func pmWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// newPMPerson prepares a person for scoring. The code adds its parts and the parts joined ("EMP-1023" → emp, 1023,
// emp1023) so it is found with or without the prefix and the dash.
func newPMPerson(id, name, desig, dept, loc, city, email string) *pmPerson {
	p := &pmPerson{id: id, name: name, desig: desig, dept: dept, loc: loc, city: city, email: email}
	p.nameWords = pmWords(name)
	code := pmWords(id)
	p.nameWords = append(p.nameWords, code...)
	if len(code) > 1 {
		p.nameWords = append(p.nameWords, strings.Join(code, ""))
	}
	for _, f := range []string{desig, dept, loc, city, email} {
		p.otherWords = append(p.otherWords, pmWords(f)...)
	}
	var ini strings.Builder
	for _, w := range pmWords(name) {
		if r := []rune(w); len(r) > 0 && unicode.IsLetter(r[0]) {
			ini.WriteRune(r[0])
		}
	}
	if s := ini.String(); len([]rune(s)) >= 2 {
		r := []rune(s)
		p.initials = []string{s}
		if len(r) > 2 { // first and last name only ("Anil Kumar Kapoor" → akk and ak)
			p.initials = append(p.initials, string([]rune{r[0], r[len(r)-1]}))
		}
	}
	p.nameText = strings.Join(pmWords(name), " ")
	return p
}

// pmDist is the optimal string alignment distance between a and b (insert, delete, change, or swap two neighbours),
// giving up (max+1) once it is above max.
func pmDist(a, b []rune, max int) int {
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

// pmTypos is how many edits a query word of n letters may have: none below 4, one for 4 to 7, two from 8.
func pmTypos(n int) int {
	switch {
	case n >= 8:
		return 2
	case n >= 4:
		return 1
	}
	return 0
}

// pmWordScore is how well query word t fits word w: exact, start, inside (3 letters or more) or a small typo of the
// word or of its start (the query may be a partly typed word with a typo in it); 0 when it does not fit.
func pmWordScore(t, w string) int {
	switch {
	case t == w:
		return pmExact
	case strings.HasPrefix(w, t):
		return pmPrefix
	case len(t) >= 3 && strings.Contains(w, t):
		return pmContains
	}
	tr, wr := []rune(t), []rune(w)
	k := pmTypos(len(tr))
	if k == 0 {
		return 0
	}
	best := k + 1
	for l := len(tr) - k; l <= len(tr)+k; l++ {
		if l >= 1 && l <= len(wr) {
			if d := pmDist(tr, wr[:l], k); d < best {
				best = d
			}
		}
	}
	if best > k {
		return 0
	}
	return pmTypo - 10*best
}

// pmTokenScore is the best fit of query word t over the person's words (name and code words a little higher).
func pmTokenScore(t string, p *pmPerson) int {
	best := 0
	for _, w := range p.nameWords {
		if s := pmWordScore(t, w); s > 0 && s+pmNameBump > best {
			best = s + pmNameBump
		}
	}
	for _, w := range p.otherWords {
		if s := pmWordScore(t, w); s > best {
			best = s
		}
	}
	return best
}

// pmScore scores a person against a query: 0 when they do not fit. Every query word must fit (any order); a single
// word may instead fit the name's initials; the name starting with the whole query, or the code typed in full, adds
// to the score.
func pmScore(query string, p *pmPerson) int {
	toks := pmWords(query)
	if len(toks) == 0 {
		return 0
	}
	total := 0
	for _, t := range toks {
		s := pmTokenScore(t, p)
		if s == 0 {
			total = 0
			break
		}
		total += s
	}
	if len(toks) == 1 && len([]rune(toks[0])) >= 2 {
		for i, ini := range p.initials {
			s := 0
			switch {
			case ini == toks[0]:
				s = pmExact - 50
			case i == 0 && strings.HasPrefix(ini, toks[0]):
				s = pmPrefix - 50
			}
			if s > total {
				total = s
			}
		}
	}
	if total == 0 {
		return 0
	}
	q := strings.Join(toks, " ")
	if strings.HasPrefix(p.nameText, q) {
		total += 50
	}
	if strings.Join(pmWords(p.id), "") == strings.Join(toks, "") {
		total += 500
	}
	return total
}

// pmHit is a person that fits, with their score.
type pmHit struct {
	id, name string
	score    int
}

// pmRank keeps the people that fit the query, best first (then by name and code).
func pmRank(query string, people []*pmPerson) []pmHit {
	out := []pmHit{}
	for _, p := range people {
		if s := pmScore(query, p); s > 0 {
			out = append(out, pmHit{p.id, p.name, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if an, bn := strings.ToLower(a.name), strings.ToLower(b.name); an != bn {
			return an < bn
		}
		return a.id < b.id
	})
	return out
}

// pmNames maps each document of a small master collection to its name (and keeps the documents for parents).
func pmNames(ctx context.Context, q store.Q, coll string, fields ...string) (map[string]string, map[string]store.Row, error) {
	proj := bson.M{"name": 1}
	for _, f := range fields {
		proj[f] = 1
	}
	rows, err := q.C(coll).Find(ctx, bson.M{}, options.Find().SetProjection(proj))
	if err != nil {
		return nil, nil, err
	}
	names, docs := make(map[string]string, len(rows)), make(map[string]store.Row, len(rows))
	for _, r := range rows {
		names[str(r["id"])], docs[str(r["id"])] = str(r["name"]), r
	}
	return names, docs, nil
}

// pmIDsNamed are the ids of the master documents whose name has the query word t in it.
func pmIDsNamed(names map[string]string, t string) []string {
	out := []string{}
	for id, n := range names {
		if strings.Contains(strings.ToLower(n), t) {
			out = append(out, id)
		}
	}
	return out
}

// employeesSearch is the people search over the employees matching filter (the caller's scope and the list's
// filters): the ids that fit q, best first. withEmail lets the email count (the caller sees personal details).
func employeesSearch(ctx context.Context, q store.Q, filter bson.M, query string, withEmail bool) ([]string, error) {
	toks := pmWords(query)
	if len(toks) == 0 {
		return nil, nil
	}
	depts, _, err := pmNames(ctx, q, "department")
	if err != nil {
		return nil, err
	}
	desigs, _, err := pmNames(ctx, q, "designation")
	if err != nil {
		return nil, err
	}
	locs, locDocs, err := pmNames(ctx, q, "location", "cluster_id")
	if err != nil {
		return nil, err
	}
	_, clusterDocs, err := pmNames(ctx, q, "cluster", "city_id")
	if err != nil {
		return nil, err
	}
	cities, _, err := pmNames(ctx, q, "city")
	if err != nil {
		return nil, err
	}
	cityOf := func(locID string) string {
		if l := locDocs[locID]; l != nil {
			if k := clusterDocs[str(l["cluster_id"])]; k != nil {
				return cities[str(k["city_id"])]
			}
		}
		return ""
	}
	proj := options.Find().SetProjection(bson.M{"name": 1, "designation_id": 1, "department_id": 1, "location_id": 1})
	load := func(f bson.M) ([]*pmPerson, error) {
		rows, err := q.C("employee").Find(ctx, f, proj)
		if err != nil {
			return nil, err
		}
		emails := map[string]string{}
		if withEmail && len(rows) > 0 {
			ids := make([]string, len(rows))
			for i, r := range rows {
				ids[i] = str(r["id"])
			}
			ps, err := q.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"employee_id": 1, "email": 1}))
			if err != nil {
				return nil, err
			}
			for _, p := range ps {
				emails[str(p["employee_id"])] = str(p["email"])
			}
		}
		out := make([]*pmPerson, len(rows))
		for i, r := range rows {
			id, loc := str(r["id"]), str(r["location_id"])
			out[i] = newPMPerson(id, str(r["name"]), desigs[str(r["designation_id"])], depts[str(r["department_id"])], locs[loc], cityOf(loc), emails[id])
		}
		return out, nil
	}
	// First pass: each query word found as typed in one of the fields (the database narrows, the scoring orders).
	conds := []bson.M{filter}
	for _, t := range toks {
		re := bson.M{"$regex": regexp.QuoteMeta(t), "$options": "i"}
		or := bson.A{bson.M{"name": re}, bson.M{"_id": re},
			bson.M{"designation_id": bson.M{"$in": pmIDsNamed(desigs, t)}}, bson.M{"department_id": bson.M{"$in": pmIDsNamed(depts, t)}},
			bson.M{"location_id": bson.M{"$in": pmIDsNamed(locs, t)}}}
		var inCity []string
		for id := range locs {
			if strings.Contains(strings.ToLower(cityOf(id)), t) {
				inCity = append(inCity, id)
			}
		}
		if len(inCity) > 0 {
			or = append(or, bson.M{"location_id": bson.M{"$in": inCity}})
		}
		if withEmail {
			ids, err := q.C("employee_personal").Distinct(ctx, "employee_id", bson.M{"email": re})
			if err != nil {
				return nil, err
			}
			or = append(or, bson.M{"_id": bson.M{"$in": ids}})
		}
		// The code without its dash ("emp1023") is checked by the scoring; let such a word through here.
		if strings.IndexFunc(t, unicode.IsDigit) > 0 {
			or = append(or, bson.M{"_id": bson.M{"$regex": "^" + regexp.QuoteMeta(strings.TrimLeftFunc(t, unicode.IsLetter)), "$options": "i"}},
				bson.M{"_id": bson.M{"$regex": regexp.QuoteMeta(strings.TrimLeftFunc(t, unicode.IsLetter)) + "$", "$options": "i"}})
		}
		conds = append(conds, bson.M{"$or": or})
	}
	people, err := load(employeesAnd(conds...))
	if err != nil {
		return nil, err
	}
	hits := pmRank(query, people)
	// Few or no plain hits: look at everyone in scope for initials and typos.
	if len(hits) < pmFewHits {
		if people, err = load(filter); err != nil {
			return nil, err
		}
		hits = pmRank(query, people)
	}
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	// An old Joining ID finds the record under its Employee ID (hiring.go).
	if qt := strings.ToUpper(strings.TrimSpace(query)); reJIAlias.MatchString(qt) {
		if a := store.EmployeeByAlias(ctx, q, qt); a != "" && !containsStr(ids, a) {
			if ok, _ := q.C("employee").Exists(ctx, employeesAnd(filter, bson.M{"_id": a})); ok {
				ids = append([]string{a}, ids...)
			}
		}
	}
	return ids, nil
}
