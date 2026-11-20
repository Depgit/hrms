package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Nominations after joining (spec 19.2): HR Admin sets the schemes, the employee submits,
// Payroll verifies or returns. Documents are stored under owner "<employee>/<scheme>":
// "n<i>:<doc>" for nominee i and "e:<doc>" for the employee's own.

type nomDoc struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Per   string `json:"per"` // nominee | employee
	Req   bool   `json:"req"`
	On    *bool  `json:"on,omitempty"` // switched off documents are kept, not asked; nil = on
}

type nomScheme struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Form      string   `json:"form"`
	Note      string   `json:"note"`
	On        bool     `json:"on"`
	Types     []string `json:"types"`
	Mandatory bool     `json:"mandatory"`
	Max       int      `json:"max"`
	Shares    bool     `json:"shares"`
	Relations []string `json:"relations"`
	Guardian  bool     `json:"guardian"`
	DueDays   int      `json:"dueDays"`
	Docs      []nomDoc `json:"docs"`
	Verify    *bool    `json:"verify"` // Payroll checks and verifies; false = final when sent (nil on save = true)
}

type nominee struct {
	Name     string  `json:"name"`
	Rel      string  `json:"rel"`
	DOB      string  `json:"dob"`
	Share    float64 `json:"share"`
	Guardian string  `json:"guardian"`
}

var nomRelations = []string{"Spouse", "Son", "Daughter", "Father", "Mother", "Brother", "Sister", "Other"}

func loadSchemes(ctx context.Context, q store.Q, onlyOn bool) ([]nomScheme, error) {
	filter := bson.M{}
	if onlyOn {
		filter["on_form"] = true
	}
	rows, err := q.C("nomination_scheme").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := []nomScheme{}
	for _, r := range rows {
		sc := nomScheme{ID: employeesInt(r["id"]), Name: str(r["name"]), Form: str(r["form"]), Note: str(r["note"]), On: r["on_form"] == true,
			Types: strs(r["types"]), Mandatory: r["mandatory"] == true, Max: employeesInt(r["max_people"]), Shares: r["shares"] == true,
			Relations: strs(r["relations"]), Guardian: r["guardian"] == true, DueDays: employeesInt(r["due_days"])}
		verify := r["verify"] == true
		sc.Verify = &verify
		if err := employeesJSONInto(r["docs"], &sc.Docs); err != nil {
			return nil, err
		}
		for i := range sc.Docs {
			on := docOn(sc.Docs[i])
			sc.Docs[i].On = &on
		}
		out = append(out, sc)
	}
	return out, nil
}

// nominationsIDFilter matches the document whose integer id reads as id (WHERE id::text=$1): anything but the
// plain number matches nothing.
func nominationsIDFilter(id string) bson.M {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != id {
		return bson.M{"_id": bson.M{"$exists": false}}
	}
	return bson.M{"_id": n}
}

// nominationsRank orders an employee's submissions for one scheme: the one waiting for Payroll first, then the
// verified one, then the latest (ORDER BY status='Submitted' DESC, status='Verified' DESC, id DESC).
func nominationsRank(l []store.Row) {
	rank := func(r store.Row) int {
		switch r["status"] {
		case "Submitted":
			return 2
		case "Verified":
			return 1
		}
		return 0
	}
	sort.SliceStable(l, func(i, j int) bool {
		if a, b := rank(l[i]), rank(l[j]); a != b {
			return a > b
		}
		return employeesInt(l[i]["id"]) > employeesInt(l[j]["id"])
	})
}

// nominationsByScheme loads submissions (filter on nomination documents) grouped by employee and scheme, each
// group in nominationsRank order.
func nominationsByScheme(ctx context.Context, q store.Q, filter bson.M) (map[string][]store.Row, error) {
	rows, err := q.C("nomination").Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := map[string][]store.Row{}
	for _, r := range rows {
		k := nominationsKey(str(r["employee_id"]), employeesInt(r["scheme_id"]))
		out[k] = append(out[k], r)
	}
	for _, l := range out {
		nominationsRank(l)
	}
	return out, nil
}

func nominationsKey(emp string, scheme int) string { return emp + "/" + strconv.Itoa(scheme) }

func (s *Server) nomSchemes(r *http.Request, c *Ctx) (any, error) {
	return loadSchemes(r.Context(), s.st.DB, c.Role != "HR Admin")
}

func (s *Server) nomSchemeSave(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin sets the nomination policy")
	}
	var b nomScheme
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	switch {
	case b.Name == "":
		return nil, bad("Name the nomination")
	case b.Max < 1 || b.Max > 10:
		return nil, bad("Allow between 1 and 10 people")
	case b.DueDays < 1 || b.DueDays > 180:
		return nil, bad("Due days after joining must be between 1 and 180")
	case len(b.Relations) == 0:
		return nil, bad("Allow at least one relation")
	case len(b.Types) == 0:
		return nil, bad("Choose at least one employment type")
	}
	for _, rel := range b.Relations {
		if !containsStr(nomRelations, rel) {
			return nil, bad("Unknown relation %s", rel)
		}
	}
	for _, t := range b.Types {
		if !containsStr(empTypes, t) {
			return nil, bad("Unknown employment type %s", t)
		}
	}
	for i := range b.Docs {
		b.Docs[i].Label = strings.TrimSpace(b.Docs[i].Label)
		if b.Docs[i].Label == "" || (b.Docs[i].Per != "nominee" && b.Docs[i].Per != "employee") {
			return nil, bad("Every document needs a name and who gives it (nominee or employee)")
		}
		if b.Docs[i].ID == "" {
			b.Docs[i].ID = "d" + randomHex(3)
		}
		on := docOn(b.Docs[i])
		b.Docs[i].On = &on
	}
	if b.Verify == nil {
		v := true
		b.Verify = &v
	}
	if b.Docs == nil {
		b.Docs = []nomDoc{}
	}
	ctx, id := r.Context(), r.PathValue("id")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		doc := bson.M{"name": b.Name, "form": b.Form, "note": b.Note, "on_form": b.On, "types": b.Types, "mandatory": b.Mandatory, "max_people": b.Max,
			"shares": b.Shares, "relations": b.Relations, "guardian": b.Guardian, "due_days": b.DueDays, "docs": b.Docs, "verify": *b.Verify}
		var err error
		if id == "" {
			_, err = tx.C("nomination_scheme").Insert(ctx, doc)
		} else {
			_, err = tx.C("nomination_scheme").Update(ctx, nominationsIDFilter(id), bson.M{"$set": doc})
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, c, "Nomination policy “"+b.Name+"” saved", "nomination_scheme", id, nil, b)
	})
}

func ageOn(dob, on string) int {
	a, err1 := time.Parse("2006-01-02", dob)
	b, err2 := time.Parse("2006-01-02", on)
	if err1 != nil || err2 != nil {
		return 99
	}
	y := b.Year() - a.Year()
	if b.YearDay() < a.YearDay() {
		y--
	}
	return y
}

// nomProblems mirrors the prototype's nmErrors.
func nomProblems(sc nomScheme, people []nominee, docs map[string]store.Row) []string {
	er := []string{}
	if len(people) == 0 {
		return []string{"Add at least one person"}
	}
	if len(people) > sc.Max {
		er = append(er, fmt.Sprintf("At most %d %s can be added", sc.Max, map[bool]string{true: "person", false: "people"}[sc.Max == 1]))
	}
	seen, tot := map[string]bool{}, 0.0
	for i, p := range people {
		n := i + 1
		if len(strings.TrimSpace(p.Name)) < 2 {
			er = append(er, fmt.Sprintf("Person %d: enter the full name", n))
		}
		if !containsStr(sc.Relations, p.Rel) {
			er = append(er, fmt.Sprintf("Person %d: %s is not allowed here. Allowed: %s", n, or(p.Rel, "relation"), strings.Join(sc.Relations, ", ")))
		}
		if !isDate(p.DOB) || p.DOB > today() {
			er = append(er, fmt.Sprintf("Person %d: enter a valid date of birth", n))
		}
		k := strings.ToLower(strings.TrimSpace(p.Name)) + "|" + p.Rel
		if seen[k] {
			er = append(er, fmt.Sprintf("Person %d: same name and relation is added twice", n))
		}
		seen[k] = true
		if sc.Shares {
			if p.Share <= 0 || p.Share > 100 {
				er = append(er, fmt.Sprintf("Person %d: share must be between 1 and 100", n))
			}
			tot += p.Share
		}
		if sc.Guardian && isDate(p.DOB) && ageOn(p.DOB, today()) < 18 && strings.TrimSpace(p.Guardian) == "" {
			er = append(er, fmt.Sprintf("Person %d is under 18. Enter the name of an adult guardian", n))
		}
		for _, d := range sc.Docs {
			if d.Req && docOn(d) && d.Per == "nominee" && docs[fmt.Sprintf("n%d:%s", i, d.ID)] == nil {
				er = append(er, fmt.Sprintf("Person %d: attach %s", n, d.Label))
			}
		}
	}
	if sc.Shares && math.Round(tot*100) != 10000 {
		er = append(er, fmt.Sprintf("Shares add up to %s%%. They must add up to 100%%", strconv.FormatFloat(tot, 'f', -1, 64)))
	}
	for _, d := range sc.Docs {
		if d.Req && docOn(d) && d.Per == "employee" && docs["e:"+d.ID] == nil {
			er = append(er, "Attach "+d.Label)
		}
	}
	return er
}

type nomState struct {
	Scheme  nomScheme  `json:"scheme"`
	State   string     `json:"state"` // Not submitted, Waiting for Payroll, Verified, Returned
	Due     string     `json:"due"`
	Overdue bool       `json:"overdue"`
	Current *store.Row `json:"current"`
}

// nomStates lists every scheme that applies to an employee with its state.
func nomStates(ctx context.Context, q store.Q, empID string) ([]nomState, error) {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": empID})
	if err != nil || e == nil {
		return nil, notFound("Employee not found")
	}
	typ, doj := str(e["type"]), str(e["doj"])
	schemes, err := loadSchemes(ctx, q, true)
	if err != nil {
		return nil, err
	}
	subs, err := nominationsByScheme(ctx, q, bson.M{"employee_id": empID})
	if err != nil {
		return nil, err
	}
	out := []nomState{}
	for _, sc := range schemes {
		if !containsStr(sc.Types, typ) {
			continue
		}
		st := nomState{Scheme: sc, State: "Not submitted"}
		if t, err := time.Parse("2006-01-02", doj); err == nil {
			st.Due = t.AddDate(0, 0, sc.DueDays).Format("2006-01-02")
		}
		// The submission waiting for Payroll wins, then the verified one, then the latest.
		if l := subs[nominationsKey(empID, sc.ID)]; len(l) > 0 {
			cur := store.Row{}
			for _, k := range []string{"id", "status", "people", "submitted_at", "decided_by", "decided_at", "note"} {
				cur[k] = l[0][k]
			}
			st.Current = &cur
			st.State = map[string]string{"Submitted": "Waiting for Payroll", "Verified": "Verified", "Returned": "Returned"}[cur["status"].(string)]
		}
		st.Overdue = st.State != "Verified" && st.State != "Waiting for Payroll" && sc.Mandatory && st.Due != "" && st.Due < today()
		out = append(out, st)
	}
	return out, nil
}

func (s *Server) myNominations(r *http.Request, c *Ctx) (any, error) {
	return nomStates(r.Context(), s.st.DB, c.EmployeeID)
}

func (s *Server) nominationSubmit(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		People []nominee `json:"people"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	id, _ := strconv.Atoi(r.PathValue("scheme"))
	if e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}); err != nil || e == nil || e["status"] != "Active" {
		return nil, bad("Nominations are made by active employees")
	}
	states, err := nomStates(ctx, s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	var sc *nomState
	for i := range states {
		if states[i].Scheme.ID == id {
			sc = &states[i]
		}
	}
	if sc == nil {
		return nil, notFound("This nomination does not apply to you")
	}
	if sc.State == "Waiting for Payroll" {
		return nil, bad("Your last submission is waiting for Payroll. You can change it after it is verified or returned")
	}
	owner := fmt.Sprintf("%s/%d", c.EmployeeID, id)
	docs, err := docsOf(ctx, s.st.DB, "nomination", owner)
	if err != nil {
		return nil, err
	}
	for i := range b.People {
		b.People[i].Name = strings.TrimSpace(b.People[i].Name)
		b.People[i].Guardian = strings.TrimSpace(b.People[i].Guardian)
		if !sc.Scheme.Shares {
			b.People[i].Share = 0
		}
	}
	if er := nomProblems(sc.Scheme, b.People, docs); len(er) > 0 {
		return nil, badD(er[0], er)
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		inserted, err := tx.C("nomination").Insert(ctx, bson.M{"employee_id": c.EmployeeID, "scheme_id": id, "status": "Submitted", "people": b.People})
		if err != nil {
			return err
		}
		nid, _ := inserted.(int64)
		// A nomination Payroll does not verify is final when sent.
		if v := sc.Scheme.Verify; v != nil && !*v {
			if _, err := tx.C("nomination").Update(ctx, bson.M{"_id": nid},
				bson.M{"$set": bson.M{"status": "Verified", "decided_by": "Automatic", "decided_at": tx.Now()}}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Submitted "+sc.Scheme.Name, "nomination", strconv.FormatInt(nid, 10), nil, b.People)
	})
}

// nominationQueue is Payroll's view: submissions to verify and people still to submit.
func (s *Server) nominationQueue(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	scope, err := employeesScope(ctx, s.st.DB, c.Scope)
	if err != nil {
		return nil, err
	}
	schemes, err := s.st.DB.C("nomination_scheme").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	schemeOf := map[int]store.Row{}
	for _, x := range schemes {
		schemeOf[employeesInt(x["id"])] = x
	}
	depts, err := s.st.DB.C("department").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	deptName := map[string]any{}
	for _, d := range depts {
		deptName[str(d["id"])] = d["name"]
	}

	// Submissions waiting for Payroll, oldest first, for people in scope; version is the submission's number among
	// that person's submissions for the scheme.
	waiting, err := s.st.DB.C("nomination").Find(ctx, bson.M{"status": "Submitted"},
		options.Find().SetSort(bson.D{{Key: "submitted_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	empIDs := []string{}
	for _, n := range waiting {
		empIDs = append(empIDs, str(n["employee_id"]))
	}
	emps, err := s.st.DB.C("employee").Find(ctx, employeesAnd(scope, bson.M{"_id": bson.M{"$in": empIDs}}), options.Find().SetProjection(bson.M{"name": 1, "department_id": 1}))
	if err != nil {
		return nil, err
	}
	empOf := map[string]store.Row{}
	for _, e := range emps {
		empOf[str(e["id"])] = e
	}
	all, err := s.st.DB.C("nomination").Find(ctx, bson.M{"employee_id": bson.M{"$in": empIDs}}, options.Find().SetProjection(bson.M{"employee_id": 1, "scheme_id": 1}))
	if err != nil {
		return nil, err
	}
	versions := map[string][]int{} // submission ids per person and scheme
	for _, v := range all {
		k := nominationsKey(str(v["employee_id"]), employeesInt(v["scheme_id"]))
		versions[k] = append(versions[k], employeesInt(v["id"]))
	}
	subs := []store.Row{}
	for _, n := range waiting {
		e, sc := empOf[str(n["employee_id"])], schemeOf[employeesInt(n["scheme_id"])]
		dept, ok := deptName[str(e["department_id"])]
		if e == nil || sc == nil || !ok {
			continue
		}
		var version int64
		for _, v := range versions[nominationsKey(str(n["employee_id"]), employeesInt(n["scheme_id"]))] {
			if v <= employeesInt(n["id"]) {
				version++
			}
		}
		subs = append(subs, store.Row{"id": n["id"], "employee_id": n["employee_id"], "name": e["name"], "department": dept, "scheme_id": n["scheme_id"],
			"scheme": sc["name"], "shares": sc["shares"], "docs": sc["docs"], "people": n["people"], "submitted_at": n["submitted_at"], "version": version})
	}
	for _, x := range subs {
		x["files"], _ = docsOf(ctx, s.st.DB, "nomination", fmt.Sprintf("%s/%d", x["employee_id"], x["scheme_id"]))
	}

	// Mandatory nominations not yet submitted (or returned): every active person in scope of a type a scheme on the
	// form applies to, whose top submission (nominationsRank) is none or a returned one.
	pending := []store.Row{}
	types := []string{}
	mandatory := []store.Row{}
	for _, sc := range schemes {
		if sc["on_form"] == true && sc["mandatory"] == true {
			mandatory = append(mandatory, sc)
			types = append(types, strs(sc["types"])...)
		}
	}
	if len(mandatory) > 0 {
		people, err := s.st.DB.C("employee").Find(ctx, employeesAnd(scope, bson.M{"status": "Active", "type": bson.M{"$in": types}}),
			options.Find().SetProjection(bson.M{"name": 1, "department_id": 1, "doj": 1, "type": 1}))
		if err != nil {
			return nil, err
		}
		ids := []int{}
		for _, sc := range mandatory {
			ids = append(ids, employeesInt(sc["id"]))
		}
		subsOf, err := nominationsByScheme(ctx, s.st.DB, bson.M{"scheme_id": bson.M{"$in": ids}})
		if err != nil {
			return nil, err
		}
		reminders, err := s.st.DB.C("nomination_reminder").Find(ctx, bson.M{"scheme_id": bson.M{"$in": ids}},
			options.Find().SetProjection(bson.M{"employee_id": 1, "scheme_id": 1, "at": 1}))
		if err != nil {
			return nil, err
		}
		reminded := map[string]time.Time{} // the last reminder per person and scheme
		for _, x := range reminders {
			k := nominationsKey(str(x["employee_id"]), employeesInt(x["scheme_id"]))
			if at, ok := x["at"].(time.Time); ok && at.After(reminded[k]) {
				reminded[k] = at
			}
		}
		t := today()
		for _, e := range people {
			dept, ok := deptName[str(e["department_id"])]
			doj, okDate := employeesTime(e["doj"])
			if !ok || !okDate {
				continue
			}
			for _, sc := range mandatory {
				if !containsStr(strs(sc["types"]), str(e["type"])) {
					continue
				}
				k := nominationsKey(str(e["id"]), employeesInt(sc["id"]))
				state := "Not submitted"
				if l := subsOf[k]; len(l) > 0 {
					if l[0]["status"] != "Returned" {
						continue
					}
					state = "Returned"
				}
				due := doj.AddDate(0, 0, employeesInt(sc["due_days"])).Format("2006-01-02")
				var last any
				if at, ok := reminded[k]; ok {
					last = store.DateOf(at)
				}
				pending = append(pending, store.Row{"id": e["id"], "name": e["name"], "department": dept, "doj": e["doj"], "scheme_id": sc["id"],
					"scheme": sc["name"], "state": state, "due": due, "overdue": due < t, "reminded": last})
			}
		}
		// Soonest due first, then by person and scheme.
		sort.SliceStable(pending, func(i, j int) bool {
			a, b := pending[i], pending[j]
			if a["due"] != b["due"] {
				return str(a["due"]) < str(b["due"])
			}
			if a["id"] != b["id"] {
				return str(a["id"]) < str(b["id"])
			}
			return employeesInt(a["scheme_id"]) < employeesInt(b["scheme_id"])
		})
		if len(pending) > 5000 {
			pending = pending[:5000]
		}
	}
	return map[string]any{"submitted": subs, "pending": pending}, nil
}

func (s *Server) nominationDecide(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.claims", 2); err != nil {
		return nil, err
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = decode(r, &b)
	action := r.PathValue("action")
	if action != "verify" && action != "return" {
		return nil, notFound("")
	}
	if action == "return" && strings.TrimSpace(b.Note) == "" {
		return nil, bad("Say what the employee needs to fix")
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		n, err := tx.C("nomination").Lock(ctx, nominationsIDFilter(r.PathValue("id")))
		if err != nil || n == nil {
			return notFound("Nomination not found")
		}
		sc, err := tx.C("nomination_scheme").FindOne(ctx, bson.M{"_id": n["scheme_id"]})
		if err != nil || sc == nil {
			return notFound("Nomination not found")
		}
		emp, status, scheme := str(n["employee_id"]), str(n["status"]), str(sc["name"])
		if status != "Submitted" {
			return bad("This nomination is already %s", strings.ToLower(status))
		}
		if emp == c.EmployeeID {
			return forbidden("Nobody verifies their own nomination")
		}
		if err := c.MustInScope(ctx, tx, emp); err != nil {
			return err
		}
		st := map[string]string{"verify": "Verified", "return": "Returned"}[action]
		if _, err := tx.C("nomination").Update(ctx, bson.M{"_id": n["id"]},
			bson.M{"$set": bson.M{"status": st, "decided_by": c.Name + " (" + c.Role + ")", "decided_at": tx.Now(), "note": employeesNullIf(b.Note)}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, st+" "+scheme+" of "+emp, "nomination", r.PathValue("id"), nil, b)
	})
}
