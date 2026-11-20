package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Separation (spec 18): one case per exit, through one approval flow named Separation.
// Show-cause (absconding) → Approval → Notice → Clearance → Full and final → Letters → Closed.

var sepTypes = []string{"Resignation", "Termination", "Absconding", "Contract end", "Retirement", "Death"}

// sepReasonName is the Separation change reason recorded for an exit type (Masters › Change reasons; HR may rename it).
func sepReasonName(ctx context.Context, q store.Q, t string) string {
	return store.SysName(ctx, q, "sep:"+t)
}

var termReasons = []string{"Performance", "Misconduct", "Policy violation", "Failed probation", "Redundancy or restructuring", "Other"}
var exitQs = []string{"Manager", "Work and shifts", "Pay and benefits", "Growth", "Workplace and team"}
var exitMain = []string{"Better pay", "Career growth", "Manager or team", "Work hours or shifts", "Relocation", "Higher studies", "Health or family", "Other"}
var sepHR = []string{"HR Admin", "HRBP", "HRBP Corporate"}

// The absconding rules and the exit types that need an exit interview are settings (sepCfg, ui2_separation.go).

type sepCase struct {
	ID, EmployeeID, Type, Status, GivenOn, Reason, Note, StartedBy string
	LWD, NoticeOption, RequestID, HoldID, Rehire, CloseNote        *string
	NoticeDays                                                     int
	AbsentDays                                                     *int
	ShowCause                                                      []map[string]any
	ReqStatus                                                      *string
	EmpStatus                                                      string
	EmpExit                                                        *string
	EmpLWD                                                         *string
}

// sepCasesOf turns separation documents into cases, in the order given: each with its employee's status, exit
// status and last working day (an inner join: a case whose employee is missing is left out) and the status of its
// approval request (nil when it has none). sepFind, sepFindOne and sepLock read cases through it.
func sepCasesOf(ctx context.Context, q store.Q, seps []store.Row) ([]*sepCase, error) {
	var empIDs, reqIDs []string
	for _, x := range seps {
		empIDs, reqIDs = append(empIDs, str(x["employee_id"])), append(reqIDs, str(x["request_id"]))
	}
	emps, err := approvalsByID(ctx, q, "employee", empIDs)
	if err != nil {
		return nil, err
	}
	reqs, err := approvalsByID(ctx, q, "approval_request", reqIDs)
	if err != nil {
		return nil, err
	}
	out := make([]*sepCase, 0, len(seps))
	for _, x := range seps {
		e := emps[str(x["employee_id"])]
		if e == nil {
			continue
		}
		c := &sepCase{ID: str(x["id"]), EmployeeID: str(x["employee_id"]), Type: str(x["type"]), Status: str(x["status"]), GivenOn: str(x["given_on"]),
			Reason: str(x["reason"]), Note: str(x["note"]), StartedBy: str(x["started_by"]),
			LWD: approvalsStrPtr(x["lwd"]), NoticeOption: approvalsStrPtr(x["notice_option"]), RequestID: approvalsStrPtr(x["request_id"]),
			HoldID: approvalsStrPtr(x["hold_id"]), Rehire: approvalsStrPtr(x["rehire"]), CloseNote: approvalsStrPtr(x["close_note"]),
			NoticeDays: approvalsInt(x["notice_days"]), EmpStatus: str(e["status"]), EmpExit: approvalsStrPtr(e["exit_status"]), EmpLWD: approvalsStrPtr(e["lwd"])}
		if x["absent_days"] != nil {
			n := approvalsInt(x["absent_days"])
			c.AbsentDays = &n
		}
		if r := reqs[str(x["request_id"])]; r != nil {
			c.ReqStatus = approvalsStrPtr(r["status"])
		}
		if err := approvalsJSON(x["show_cause"], &c.ShowCause); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// sepFind returns the cases whose separation document matches filter, in the order the options sort them.
func sepFind(ctx context.Context, q store.Q, filter any, opts ...options.Lister[options.FindOptions]) ([]*sepCase, error) {
	seps, err := q.C("separation").Find(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}
	return sepCasesOf(ctx, q, seps)
}

// sepFindOne is the first case sepFind returns, or store.ErrNoRows when there is none.
func sepFindOne(ctx context.Context, q store.Q, filter any, opts ...options.Lister[options.FindOptions]) (*sepCase, error) {
	cases, err := sepFind(ctx, q, filter, opts...)
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, store.ErrNoRows
	}
	return cases[0], nil
}

// sepLock reads a case FOR UPDATE (of the separation) inside a transaction; store.ErrNoRows when there is none.
func sepLock(ctx context.Context, tx store.Q, id string) (*sepCase, error) {
	x, err := tx.C("separation").Lock(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if x == nil {
		return nil, store.ErrNoRows
	}
	cases, err := sepCasesOf(ctx, tx, []store.Row{x})
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, store.ErrNoRows
	}
	return cases[0], nil
}

// sepNewestFirst sorts cases by when they started, newest first.
var sepNewestFirst = options.Find().SetSort(bson.D{{Key: "started_at", Value: -1}, {Key: "_id", Value: -1}})

func loadSep(ctx context.Context, q store.Q, id string) (*sepCase, error) {
	c, err := sepFindOne(ctx, q, bson.M{"_id": id})
	if err != nil {
		return nil, notFound("Separation case not found")
	}
	return c, nil
}

// sepDay is a timestamp as to_char(at,'DD Mon YYYY') wrote it in the business zone ("05 Nov 2026"), nil for none.
func sepDay(v any) any {
	t, ok := v.(time.Time)
	if !ok {
		return nil
	}
	return t.In(store.Loc()).Format("02 Jan 2006")
}

// sepIssuedAssets are the assets still issued to an employee (id, item name, serial, issued_at), in the order they
// were stored; an asset whose item is missing is left out, as the JOIN did.
func sepIssuedAssets(ctx context.Context, q store.Q, empID string) ([]store.Row, error) {
	assets, err := q.C("asset").Find(ctx, bson.M{"employee_id": empID, "status": "Issued"}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	itemIDs := []int32{}
	for _, a := range assets {
		if id, ok := a["item_id"].(int32); ok {
			itemIDs = append(itemIDs, id)
		}
	}
	items, err := approvalsByID(ctx, q, "inv_item", itemIDs)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, a := range assets {
		id, _ := a["item_id"].(int32)
		if it := items[id]; it != nil {
			out = append(out, store.Row{"id": a["id"], "name": it["name"], "serial": a["serial"], "issued_at": a["issued_at"]})
		}
	}
	return out, nil
}

// lwd is the case's last working day: the employee's once on notice, else the case's.
func (c *sepCase) lwd() string {
	if c.EmpLWD != nil && (c.EmpStatus == "Inactive" || (c.EmpExit != nil && *c.EmpExit == "On notice")) {
		return *c.EmpLWD
	}
	if c.LWD != nil {
		return *c.LWD
	}
	return ""
}

func letterNeeded(t string) string {
	switch t {
	case "Termination":
		return "Termination letter"
	case "Resignation", "Contract end", "Retirement":
		return "Relieving letter"
	}
	return ""
}

type clearItem struct {
	ID    int    `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
	Auto  string `json:"auto,omitempty"`
	Done  bool   `json:"done"`
	By    string `json:"by,omitempty"`
	At    string `json:"at,omitempty"`
	Why   string `json:"why,omitempty"`
}

// sepClearanceRead is the checklist read once for a whole list of cases (GET /api/separations), so stage and
// sepWaitingForMe do not read clearance_item and clearance again for every row (the old query was one LEFT JOIN
// per case). clearanceOf uses it, through the request context, for the cases it holds.
type sepClearanceRead struct {
	items  []store.Row            // the active items, by sort and id
	signed map[string][]store.Row // the clearance documents of each case loaded (an entry, maybe empty, per case)
}

type sepClearanceKey struct{}

func activeClearanceItems(ctx context.Context, q store.Q) ([]store.Row, error) {
	return q.C("clearance_item").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "_id", Value: 1}}))
}

// withSepClearance reads the checklist of the given cases in two queries and returns a context that carries it.
func withSepClearance(ctx context.Context, q store.Q, cases []*sepCase) (context.Context, error) {
	items, err := activeClearanceItems(ctx, q)
	if err != nil {
		return ctx, err
	}
	cr := &sepClearanceRead{items: items, signed: map[string][]store.Row{}}
	ids := []string{}
	for _, sc := range cases {
		cr.signed[sc.ID] = []store.Row{}
		ids = append(ids, sc.ID)
	}
	if len(ids) > 0 {
		signed, err := q.C("clearance").Find(ctx, bson.M{"sep_id": bson.M{"$in": ids}})
		if err != nil {
			return ctx, err
		}
		for _, x := range signed {
			id := str(x["sep_id"])
			cr.signed[id] = append(cr.signed[id], x)
		}
	}
	return context.WithValue(ctx, sepClearanceKey{}, cr), nil
}

// clearanceOf lists the active items with their state; automatic ones follow the records.
func clearanceOf(ctx context.Context, q store.Q, c *sepCase) ([]clearItem, error) {
	var items, signed []store.Row
	if cr, ok := ctx.Value(sepClearanceKey{}).(*sepClearanceRead); ok && cr.signed[c.ID] != nil {
		items, signed = cr.items, cr.signed[c.ID]
	} else {
		var err error
		if items, err = activeClearanceItems(ctx, q); err != nil {
			return nil, err
		}
		if signed, err = q.C("clearance").Find(ctx, bson.M{"sep_id": c.ID}); err != nil {
			return nil, err
		}
	}
	tick := map[int]store.Row{}
	for _, x := range signed {
		tick[approvalsInt(x["item_id"])] = x
	}
	out := []clearItem{}
	for _, row := range items {
		it := clearItem{ID: approvalsInt(row["id"]), Owner: str(row["owner"]), Name: str(row["name"]), Auto: str(row["auto"])}
		if x := tick[it.ID]; x != nil {
			at, _ := x["at"].(time.Time)
			it.Done, it.By, it.At = true, str(x["by_name"]), at.In(store.Loc()).Format("02 Jan 2006")
		}
		out = append(out, it)
	}
	for i := range out {
		switch out[i].Auto {
		case "assets":
			n64, _ := q.C("asset").Count(ctx, bson.M{"employee_id": c.EmployeeID, "status": "Issued"})
			u64, _ := q.C("uniform_issue").Count(ctx, bson.M{"employee_id": c.EmployeeID, "returned": false, "opening": bson.M{"$ne": true}}) // an opening issue has no items to return
			n, u := int(n64), int(u64)
			out[i].Done = n == 0
			out[i].Why = "From the asset records"
			if n > 0 {
				out[i].Why = fmt.Sprintf("%d asset%s still issued", n, map[bool]string{true: "", false: "s"}[n == 1])
			}
			if u > 0 {
				out[i].Why += fmt.Sprintf(" · %d uniform issue%s not returned (returns do not reverse recovery)", u, map[bool]string{true: "", false: "s"}[u == 1])
			}
		case "exit":
			if !exitNeededQ(ctx, q, c.Type) {
				out[i].Done, out[i].Why = true, "Not needed for "+strings.ToLower(c.Type)
			} else {
				ok, _ := q.C("exit_interview").Exists(ctx, bson.M{"sep_id": c.ID})
				out[i].Done, out[i].Why = ok, "From the exit interview"
			}
		}
	}
	return out, nil
}

// stage works out where a case is.
func (s *Server) stage(ctx context.Context, q store.Q, c *sepCase) (string, string) {
	if c.Status == "Cancelled" {
		if c.CloseNote != nil && strings.HasPrefix(*c.CloseNote, "Withdrawn") {
			return "Withdrawn", ""
		}
		return "Cancelled", ""
	}
	if c.Status == "Closed" {
		return "Closed", ""
	}
	if c.RequestID == nil {
		if c.Type == "Absconding" {
			n, cfg := len(c.ShowCause), sepCfg(ctx, q)
			if n < cfg.AbsLetters {
				return "Show-cause", fmt.Sprintf("HRBP sends letter %d of %d", n+1, cfg.AbsLetters)
			}
			last, _ := c.ShowCause[n-1]["at"].(string)
			if w := cfg.AbsWait - policy.DayDiff(last, today()); w > 0 {
				return "Show-cause", fmt.Sprintf("Waiting %d more day%s for a reply", w, map[bool]string{true: "", false: "s"}[w == 1])
			}
			return "Show-cause", "HRBP sends it for approval"
		}
		return "Approval", ""
	}
	switch *c.ReqStatus {
	case "Rejected":
		return "Rejected", ""
	case "Cancelled":
		return "Withdrawn", ""
	case "Pending":
		return "Approval", "Waiting for approvers"
	}
	if c.Type == "Resignation" && c.EmpStatus != "Inactive" && (c.EmpExit == nil || *c.EmpExit != "On notice") {
		return "Withdrawn", ""
	}
	lwd := c.lwd()
	items, _ := clearanceOf(ctx, q, c)
	pend := []string{}
	for _, it := range items {
		if !it.Done && !containsStr(pend, it.Owner) {
			pend = append(pend, it.Owner)
		}
	}
	if lwd > today() {
		n := policy.DayDiff(today(), lwd)
		next := fmt.Sprintf("%d day%s to the last working day", n, map[bool]string{true: "", false: "s"}[n == 1])
		if len(pend) > 0 {
			next += " · clearance open with " + strings.Join(pend, ", ")
		} else {
			next += " · clearance done"
		}
		return "Notice", next
	}
	if len(pend) > 0 {
		return "Clearance", "Waiting for " + strings.Join(pend, ", ")
	}
	var fst *string
	if f, _ := q.C("fnf").FindOne(ctx, bson.M{"employee_id": c.EmployeeID, "status": bson.M{"$in": []string{"Pending approval", "Approved"}}},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}})); f != nil {
		fst = approvalsStrPtr(f["status"])
	}
	if fst == nil {
		return "Full and final", "Payroll prepares the full and final"
	}
	if *fst != "Approved" {
		return "Full and final", "Full and final waiting for approval"
	}
	if ln := letterNeeded(c.Type); ln != "" {
		if ok, _ := q.C("sep_letter").Exists(ctx, bson.M{"sep_id": c.ID, "kind": ln}); !ok {
			return "Letters", "HRBP issues the " + strings.ToLower(ln)
		}
	}
	return "Ready to close", "HR closes the case with the rehire decision"
}

func (s *Server) canSepView(ctx context.Context, c *Ctx, sc *sepCase) error {
	if sc.EmployeeID == c.EmployeeID {
		if sepHidden(sc) { // an exit HR started is not shown to the person until approved (myrequests.go)
			return notFound("Separation case not found")
		}
		return nil
	}
	if c.Level("emp.sep") >= 1 {
		return c.MustInScope(ctx, s.st.DB, sc.EmployeeID)
	}
	if ok, _ := s.st.DB.C("employee").Exists(ctx, bson.M{"_id": sc.EmployeeID, "manager_id": c.EmployeeID}); ok {
		return nil // the reporting manager signs the clearance
	}
	return forbidden("")
}

func isSepHR(c *Ctx) bool { return containsStr(sepHR, c.Role) && c.Level("emp.sep") >= 2 }

// ---------- List and detail ----------

func (s *Server) sepList(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	q := s.st.DB
	// The newest 500 cases of the caller's reports, and with the separation right of everyone in their data scope;
	// never one about the caller that is hidden from them (myrequests.go). One aggregation, filtered and limited in
	// the database like the old `sepSQL JOIN department d WHERE … ORDER BY s.started_at DESC LIMIT 500`.
	who := bson.D{{Key: "e.manager_id", Value: c.EmployeeID}}
	if c.Level("emp.sep") >= 1 {
		scope, err := c.ScopeFilter(ctx, q, "e.")
		if err != nil {
			return nil, err
		}
		who = bson.D{{Key: "$or", Value: bson.A{who, scope}}}
		if len(scope) == 0 { // all data: everyone
			who = bson.D{}
		}
	}
	hidden := sepHiddenFilter("", "r")
	hidden["employee_id"] = c.EmployeeID
	lookup := func(from, local, as string) bson.D {
		return bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: from}, {Key: "localField", Value: local}, {Key: "foreignField", Value: "_id"}, {Key: "as", Value: as}}}}
	}
	seps, err := q.C("separation").Aggregate(ctx, []bson.D{
		{{Key: "$sort", Value: bson.D{{Key: "started_at", Value: -1}, {Key: "_id", Value: -1}}}}, // sepNewestFirst
		lookup("employee", "employee_id", "e"),
		{{Key: "$unwind", Value: "$e"}}, // JOIN employee e
		{{Key: "$set", Value: bson.D{{Key: "e.id", Value: "$e._id"}}}},
		lookup("department", "e.department_id", "d"),
		{{Key: "$unwind", Value: "$d"}}, // JOIN department d
		lookup("approval_request", "request_id", "r"),
		{{Key: "$unwind", Value: bson.D{{Key: "path", Value: "$r"}, {Key: "preserveNullAndEmptyArrays", Value: true}}}}, // LEFT JOIN approval_request r
		{{Key: "$match", Value: bson.D{{Key: "$and", Value: bson.A{who, bson.D{{Key: "$nor", Value: bson.A{hidden}}}}}}}},
		{{Key: "$limit", Value: 500}},
		{{Key: "$unset", Value: bson.A{"e", "d", "r"}}},
	})
	if err != nil {
		return nil, err
	}
	cases, err := sepCasesOf(ctx, q, seps)
	if err != nil {
		return nil, err
	}
	// The employee, location, department and designation of every row, read in batches (the old sepListRow joined
	// them per case).
	ids := []string{}
	for _, sc := range cases {
		ids = append(ids, sc.EmployeeID)
	}
	emps, err := approvalsByID(ctx, q, "employee", ids)
	if err != nil {
		return nil, err
	}
	var locIDs, deptIDs, desigIDs []string
	for _, e := range emps {
		locIDs, deptIDs, desigIDs = append(locIDs, str(e["location_id"])), append(deptIDs, str(e["department_id"])), append(desigIDs, str(e["designation_id"]))
	}
	locs, err := approvalsByID(ctx, q, "location", locIDs)
	if err != nil {
		return nil, err
	}
	depts, err := approvalsByID(ctx, q, "department", deptIDs)
	if err != nil {
		return nil, err
	}
	desigs, err := approvalsByID(ctx, q, "designation", desigIDs)
	if err != nil {
		return nil, err
	}
	if ctx, err = withSepClearance(ctx, q, cases); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	counts := map[string]int{}
	for _, sc := range cases {
		st, next := s.stage(ctx, s.st.DB, sc)
		counts[st]++
		if q := r.URL.Query().Get("stage"); q != "" && q != st {
			continue
		}
		// The employee with their location, department and designation (all blank when one is missing).
		var person sepListPerson
		if e := emps[sc.EmployeeID]; e != nil {
			l, d, g := locs[str(e["location_id"])], depts[str(e["department_id"])], desigs[str(e["designation_id"])]
			if l != nil && d != nil && g != nil {
				person = sepListPerson{name: str(e["name"]), loc: str(l["name"]), dept: str(d["name"]), desig: str(g["name"]), mgr: approvalsStrPtr(e["manager_id"])}
			}
		}
		out = append(out, s.sepListRow(ctx, c, sc, st, next, person)) // with designation, manager, waitingForMe
	}
	return map[string]any{"cases": out, "counts": counts, "types": sepTypes, "termReasons": termReasons, "canStart": isSepHR(c)}, nil
}

func (s *Server) sepGet(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	sc, err := loadSep(ctx, s.st.DB, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if err := s.canSepView(ctx, c, sc); err != nil {
		return nil, err
	}
	return s.sepDetail(ctx, c, sc)
}

func (s *Server) sepDetail(ctx context.Context, c *Ctx, sc *sepCase) (map[string]any, error) {
	q := s.st.DB
	st, next := s.stage(ctx, q, sc)
	var emp store.Row // the directory row (employees.go), nil when the employee's details are incomplete
	if rows, err := employeesDirectory(ctx, q, bson.M{"_id": sc.EmployeeID}, 0, 1); err == nil && len(rows) > 0 {
		emp = rows[0]
	}
	items, err := clearanceOf(ctx, q, sc)
	if err != nil {
		return nil, err
	}
	isMgr := emp != nil && emp["manager_id"] == c.EmployeeID
	canSign := map[string]bool{
		"Reporting manager": isMgr || c.Role == "HR Admin",
		"HRBP":              containsStr([]string{"HR Admin", "HRBP", "HRBP Corporate"}, c.Role) && c.Level("emp.sep") >= 1,
		"Finance":           containsStr([]string{"HR Admin", "Finance", "Finance Head", "Payroll", "Payroll Manager"}, c.Role) && c.Level("emp.sep") >= 1,
	}
	if sc.EmployeeID == c.EmployeeID {
		canSign = map[string]bool{}
	}
	out := map[string]any{"id": sc.ID, "type": sc.Type, "stage": st, "next": next, "givenOn": sc.GivenOn, "lwd": sc.lwd(), "noticeDays": sc.NoticeDays,
		"noticeOption": sc.NoticeOption, "reason": sc.Reason, "startedBy": sc.StartedBy, "requestId": sc.RequestID, "rehire": sc.Rehire, "closeNote": sc.CloseNote,
		"showCause": sc.ShowCause, "absentDays": sc.AbsentDays, "employee": emp, "clearance": items, "canSign": canSign,
		"open": st != "Closed" && st != "Cancelled" && st != "Withdrawn" && st != "Rejected", "isHR": isSepHR(c), "isMe": sc.EmployeeID == c.EmployeeID,
		"exitNeeded": exitNeededQ(ctx, q, sc.Type), "letterNeeded": letterNeeded(sc.Type), "exitQs": exitQs, "exitMain": exitMain}
	// Whether the work policy recovers notice not served (absconding: "N days not served, recovered in the full and final").
	if e, err := loadEmp(ctx, q, sc.EmployeeID); err == nil {
		if ts, err := loadTimeSet(ctx, q); err == nil {
			if pol, _ := ts.policyFor(e); pol != nil {
				out["noticeRecover"] = pol.S.Ex.Recover
			}
		}
	}
	// The note is visible to HR; the exit interview only to HR Admin, HRBP, HRBP Corporate, CHRO and the employee.
	if c.Level("emp.sep") >= 2 {
		out["note"] = sc.Note
	}
	if containsStr([]string{"HR Admin", "HRBP", "HRBP Corporate", "CHRO"}, c.Role) || sc.EmployeeID == c.EmployeeID {
		var x store.Row
		if v, _ := q.C("exit_interview").FindOne(ctx, bson.M{"sep_id": sc.ID}); v != nil {
			x = store.Row{"main_reason": v["main_reason"], "ratings": v["ratings"], "recommend": v["recommend"], "comments": v["comments"],
				"by_name": v["by_name"], "at": sepDay(v["at"])}
		}
		out["exitInterview"] = x
		out["canInterview"] = true
	}
	var letters []store.Row
	if l, err := q.C("sep_letter").Find(ctx, bson.M{"sep_id": sc.ID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}})); err == nil {
		letters = []store.Row{}
		for _, x := range l {
			letters = append(letters, store.Row{"id": x["id"], "kind": x["kind"], "by_name": x["by_name"], "at": sepDay(x["at"])})
		}
	}
	out["letters"] = letters
	var fnf store.Row
	if f, _ := q.C("fnf").FindOne(ctx, bson.M{"employee_id": sc.EmployeeID, "status": bson.M{"$ne": "Cancelled"}},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}})); f != nil {
		fnf = store.Row{"id": f["id"], "pay_ym": f["pay_ym"], "net": f["net"], "status": f["status"], "lines": f["lines"]}
		// Amounts (gratuity and encashment reveal basic pay) only for the employee and roles that see salary in
		// full; others see the lines with the amounts masked or hidden (ACC-09).
		if sc.EmployeeID != c.EmployeeID && c.Sensitive["salary"] != access.Full {
			fnf["net"] = c.Show("salary", f["net"], sc.EmployeeID)
			var lines []map[string]any
			if compJSONInto(f["lines"], &lines) == nil {
				for _, l := range lines {
					l["amount"] = c.Show("salary", l["amount"], sc.EmployeeID)
					delete(l, "note") // the note shows the basic pay
				}
				fnf["lines"] = lines
			} else {
				fnf["lines"] = nil
			}
		}
		if f["receivable"] != nil {
			fnf["receivable"] = f["receivable"]
			if sc.EmployeeID != c.EmployeeID && c.Sensitive["salary"] != access.Full {
				fnf["receivable"] = nil
			}
		}
	}
	out["fnf"] = fnf
	out["assets"], _ = sepIssuedAssets(ctx, q, sc.EmployeeID)
	if sc.RequestID != nil {
		if req, err := loadRequest(ctx, q, *sc.RequestID, false); err == nil {
			p := payloadOf(req)
			step := ""
			if req.Status == "Pending" && req.Step < len(req.Chain) {
				step = req.Chain[req.Step]
			}
			out["request"] = map[string]any{"id": req.ID, "status": req.Status, "chain": req.Chain, "step": step, "lwd": p.LWD, "shortfall": p.Shortfall,
				"canAct": canAct(ctx, q, c, req), "noticeLwd": p.NoticeLWD}
		}
	}
	if sc.Type == "Resignation" {
		days := 7
		if ts, err := loadTimeSet(ctx, q); err == nil {
			if e, err := loadEmp(ctx, q, sc.EmployeeID); err == nil {
				p, _ := ts.policyFor(e)
				if p.S.Ex.Withdraw > 0 {
					days = p.S.Ex.Withdraw
				}
			}
		}
		out["withdrawBy"] = policy.AddDays(sc.GivenOn, days)
	}
	s.sepDetailMore(ctx, c, sc, out) // history, named approvers, HRBP, closed by, terms set by
	return out, nil
}

// ---------- Start ----------

type sepStart struct {
	EmployeeID   string `json:"employeeId"`
	Type         string `json:"type"`
	GivenOn      string `json:"givenOn"`
	LWD          string `json:"lwd"`
	NoticeOption string `json:"noticeOption"`
	Reason       string `json:"reason"`
	Note         string `json:"note"`
	Waive        bool   `json:"waive"`
}

// absentRun counts the working days in a row, up to yesterday, with no punch, leave or regularization.
func absentRun(ctx context.Context, q store.Q, e *emp) int {
	n, _ := absentRunFrom(ctx, q, e)
	return n
}

// absentRunFrom is absentRun with the last working day before it (EXT-20): the last day with a punch, leave or
// regularization before the run, walking back over the weekly offs and holidays inside it (those are skipped,
// not counted). When the run reaches the start of the 30 days looked at, or the joining date, it is the day
// before the run's first day; with no run it is "".
func absentRunFrom(ctx context.Context, q store.Q, e *emp) (n int, lastDay string) {
	ts, err := loadTimeSet(ctx, q)
	if err != nil {
		return 0, ""
	}
	p, _ := ts.policyFor(e)
	from := policy.AddDays(today(), -30)
	m, err := loadMonth(ctx, q, ts, []string{e.ID}, from, today())
	if err != nil {
		return 0, ""
	}
	first := ""
	for d := policy.AddDays(today(), -1); d >= from && d >= e.DOJ; d = policy.AddDays(d, -1) {
		present := m.leaveOn(e, d, true) != nil || m.regs[e.ID][d] != ""
		if !present {
			st, _ := m.status(e, p, d)
			if st == "Week off" || st == "Holiday" {
				continue
			}
			present = st != "No punch" && !strings.HasPrefix(st, "Absent")
		}
		if present {
			if n == 0 {
				return 0, ""
			}
			return n, d
		}
		n, first = n+1, d
	}
	if n == 0 {
		return 0, ""
	}
	return n, policy.AddDays(first, -1)
}

func (s *Server) sepCreate(r *http.Request, c *Ctx) (any, error) {
	var b sepStart
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if !isSepHR(c) {
		return nil, forbidden("HR Admin, HRBP or HRBP Corporate start a separation; an employee starts only a resignation")
	}
	if err := c.MustInScope(ctx, s.st.DB, b.EmployeeID); err != nil {
		return nil, err
	}
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		out, err = s.startSeparation(ctx, tx, c, b)
		return err
	})
	return out, err
}

// startSeparation checks and opens a case, and raises the Separation approval (absconding waits
// for the show-cause letters).
func (s *Server) startSeparation(ctx context.Context, tx store.Q, c *Ctx, b sepStart) (map[string]any, error) {
	if !containsStr(sepTypes, b.Type) {
		return nil, bad("Choose the exit type")
	}
	e, err := loadEmp(ctx, tx, b.EmployeeID)
	if err != nil {
		return nil, err
	}
	if e.Status != "Active" {
		return nil, bad("%s is %s. Only an active employee can be separated", e.Name, strings.ToLower(e.Status))
	}
	if e.Exit != nil && *e.Exit == "On notice" {
		return nil, bad("%s is already on notice", e.Name)
	}
	if open, _ := tx.C("separation").FindOne(ctx, bson.M{"employee_id": e.ID, "status": "Open"}); open != nil {
		if cases, _ := sepCasesOf(ctx, tx, []store.Row{open}); len(cases) > 0 && sepHidden(cases[0]) && e.ID == c.EmployeeID {
			return nil, bad("Your resignation cannot be sent right now. Please speak to HR") // a case HR started, not shown to them yet
		}
		return nil, bad("%s already has an open separation case", e.Name)
	}
	if b.GivenOn == "" {
		b.GivenOn = today()
	}
	if !isDate(b.GivenOn) || b.GivenOn > today() {
		return nil, bad("The date it was given cannot be in the future")
	}
	b.Note, b.Reason = strings.TrimSpace(b.Note), strings.TrimSpace(b.Reason)
	days, _, err := noticeDaysFor(ctx, tx, e.ID)
	if err != nil {
		return nil, err
	}
	var absent *int
	payload := requestPayload{ExitType: b.Type, Reason: b.Reason, Note: b.Note, Waive: b.Waive, NoticeDays: days}
	switch b.Type {
	case "Resignation":
		if !containsStr(hrExitReasons, b.Reason) { // the employee's own list is checked when they raise it
			return nil, bad("Choose a reason for leaving")
		}
		// The last working day follows from the notice period and the date given; not editable.
		b.LWD = policy.AddDays(b.GivenOn, days)
	case "Termination":
		if len(b.Note) < 10 {
			return nil, bad("A termination needs a note explaining why")
		}
		if !containsStr(termReasons, b.Reason) {
			return nil, bad("Choose the reason for termination")
		}
		switch b.NoticeOption {
		case "serve":
			b.LWD = policy.AddDays(today(), days)
		case "pay", "none":
			if b.LWD == "" {
				b.LWD = today()
			}
		default:
			return nil, bad("Choose pay in lieu of notice, serve the notice, or no notice (misconduct)")
		}
		if b.LWD < today() {
			return nil, bad("The last working day cannot be in the past. For a past exit use Mark inactive")
		}
	case "Absconding":
		n, last := absentRunFrom(ctx, tx, e)
		rule := sepCfg(ctx, tx).AbsDays
		absent = &n
		if n < rule && len(b.Note) < 10 {
			return nil, bad("%s has %d absent day%s in a row (the rule is %d). Add a note explaining the case", e.Name, n, map[bool]string{true: "", false: "s"}[n == 1], rule)
		}
		if b.LWD == "" { // the last day at work before the absent run (today when there is no run)
			b.LWD = today()
			if last != "" {
				b.LWD = last
			}
		}
		if b.Reason == "" {
			b.Reason = store.SysName(ctx, tx, "reason.absent_hold")
		}
	default:
		if !isDate(b.LWD) {
			return nil, bad("Give the last working day")
		}
		if b.Type != "Death" && b.LWD < today() {
			return nil, bad("The last working day cannot be in the past. For a past exit use Mark inactive")
		}
		if b.Reason == "" {
			b.Reason = sepReasonName(ctx, tx, b.Type)
		}
	}
	payload.LWD, payload.NoticeLWD = b.LWD, policy.AddDays(b.GivenOn, days)
	n, err := store.NextSeq(ctx, tx, "separation_seq")
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("SEP-%d", n)
	if _, err := tx.C("separation").Insert(ctx, bson.M{"_id": id, "employee_id": e.ID, "type": b.Type, "given_on": b.GivenOn, "lwd": b.LWD, "notice_days": days,
		"notice_option": approvalsNullIf(b.NoticeOption), "reason": b.Reason, "note": b.Note, "absent_days": absent, "started_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return nil, err
	}
	out := map[string]any{"id": id}
	if b.Type == "Absconding" {
		// Salary is held while the case runs; reporting back releases it.
		hid, err := placeHold(ctx, tx, c.Name, e.ID, store.SysName(ctx, tx, "reason.absent_hold"), "Absconding case "+id, openYm(ctx, tx), id)
		if err != nil {
			return nil, err
		}
		if _, err := tx.C("separation").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"hold_id": hid}}); err != nil {
			return nil, err
		}
	} else {
		req, err := s.raiseSep(ctx, tx, c, id, e.ID, payload)
		if err != nil {
			return nil, err
		}
		out["requestId"] = req["id"]
	}
	return out, audit(ctx, tx, c, b.Type+" started", "separation", id, nil, b)
}

func (s *Server) raiseSep(ctx context.Context, tx store.Q, c *Ctx, id, empID string, p requestPayload) (map[string]any, error) {
	lwd, days := p.LWD, p.NoticeDays
	f, err := factsFor(ctx, tx, "resign", empID, &p)
	if err != nil {
		return nil, err
	}
	p.LWD, p.NoticeDays = lwd, days // from the date given, not from today
	if p.ExitType != "Resignation" {
		f.SKind = p.ExitType
	}
	req, err := s.createRequestTx(ctx, tx, c, "resign", empID, f, p)
	if err != nil {
		return nil, err
	}
	_, err = tx.C("separation").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"request_id": req["id"]}})
	return req, err
}

// sepClose ends a case (Closed or Cancelled) with who ended it, when, and the note (none when blank).
func sepClose(ctx context.Context, tx store.Q, id, status, by, note string) error {
	_, err := tx.C("separation").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": status, "closed_by": by, "closed_at": tx.Now(), "close_note": approvalsNullIf(note)}})
	return err
}

// ---------- Actions on a case ----------

func (s *Server) sepAction(r *http.Request, c *Ctx) (any, error) {
	ctx, act := r.Context(), r.PathValue("action")
	var b struct {
		LWD       string         `json:"lwd"`
		Shortfall string         `json:"shortfall"`
		Note      string         `json:"note"`
		Rehire    string         `json:"rehire"`
		Kind      string         `json:"kind"`
		Main      string         `json:"mainReason"`
		Ratings   map[string]int `json:"ratings"`
		Recommend string         `json:"recommend"`
		Comments  string         `json:"comments"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &b); err != nil {
			return nil, err
		}
	}
	b.Note = strings.TrimSpace(b.Note)
	var out any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sc, err := sepLock(ctx, tx, r.PathValue("id"))
		if err != nil {
			return notFound("Separation case not found")
		}
		if err := s.canSepView(ctx, c, sc); err != nil {
			return err
		}
		st, _ := s.stage(ctx, tx, sc)
		hrOnly := func() error {
			if !isSepHR(c) || sc.EmployeeID == c.EmployeeID {
				return forbidden("Only HR can do this")
			}
			return nil
		}
		switch act {
		// "terms" (an approver's earlier last working day) and "rehire" have their own routes: sepTerms, sepRehire.
		case "withdraw":
			if sc.Type != "Resignation" {
				return bad("Only a resignation can be withdrawn")
			}
			if sc.EmployeeID != c.EmployeeID && !isSepHR(c) {
				return forbidden("")
			}
			if st != "Approval" && st != "Notice" {
				return bad("This resignation can no longer be withdrawn")
			}
			// On notice the employee's withdrawal goes to the approvers as a Withdraw resignation request; only HR
			// withdraws it directly (HIRE-444).
			if st == "Notice" && !isSepHR(c) {
				return bad("While on notice, a withdrawal goes to the approvers: raise Withdraw resignation")
			}
			det, _ := s.sepDetail(ctx, c, sc)
			if by, _ := det["withdrawBy"].(string); by != "" && today() > by {
				return bad("A resignation can be withdrawn up to %s, 7 days from the day it was given", longDate(by))
			}
			// The same steps as an approved withdrawal request (ui3_approvals.go).
			if err := withdrawSepCase(ctx, tx, sc, c.Name, ""); err != nil {
				return err
			}
			if err := voidNoticeRecovery(ctx, tx, sc.EmployeeID, "Resignation withdrawn by "+c.Name); err != nil {
				return err
			}
		case "cancel":
			if err := hrOnly(); err != nil {
				return err
			}
			if st != "Approval" && st != "Show-cause" {
				return bad("Only a case still waiting for approval can be cancelled")
			}
			if b.Note == "" {
				return bad("Give the reason for cancelling")
			}
			if sc.RequestID != nil && *sc.ReqStatus == "Pending" {
				if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": *sc.RequestID}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
					return err
				}
			}
			if sc.HoldID != nil {
				if _, err := releaseHold(ctx, tx, c.Name, *sc.HoldID, "Case cancelled"); err != nil && !strings.Contains(err.Error(), "already") {
					return err
				}
			}
			if err := sepClose(ctx, tx, sc.ID, "Cancelled", c.Name, "Cancelled: "+b.Note); err != nil {
				return err
			}
		case "show-cause":
			if err := hrOnly(); err != nil {
				return err
			}
			if sc.Type != "Absconding" || sc.RequestID != nil {
				return bad("Show-cause letters are for absconding cases before approval")
			}
			n, cfg := len(sc.ShowCause), sepCfg(ctx, tx)
			if n >= cfg.AbsLetters {
				return bad("All %d show-cause letters are sent. Send the case for approval", cfg.AbsLetters)
			}
			if n > 0 {
				last, _ := sc.ShowCause[n-1]["at"].(string)
				if policy.DayDiff(last, today()) < cfg.AbsWait {
					return bad("Wait %d days after the last letter", cfg.AbsWait)
				}
			}
			body, err := s.letterBody(ctx, tx, sc, "Show-cause notice", n+1)
			if err != nil {
				return err
			}
			if _, err := tx.C("sep_letter").Insert(ctx, bson.M{"sep_id": sc.ID, "kind": "Show-cause notice", "body": body, "by_name": c.Name}); err != nil {
				return err
			}
			sc.ShowCause = append(sc.ShowCause, map[string]any{"n": n + 1, "at": today(), "by": c.Name})
			if _, err := tx.C("separation").Update(ctx, bson.M{"_id": sc.ID}, bson.M{"$set": bson.M{"show_cause": sc.ShowCause}}); err != nil {
				return err
			}
		case "send":
			if err := hrOnly(); err != nil {
				return err
			}
			if sc.Type != "Absconding" || sc.RequestID != nil {
				return bad("Only an absconding case waits to be sent")
			}
			if _, next := s.stage(ctx, tx, sc); next != "HRBP sends it for approval" {
				return bad("%s first", next)
			}
			days, _, _ := noticeDaysFor(ctx, tx, sc.EmployeeID)
			if _, err := s.raiseSep(ctx, tx, c, sc.ID, sc.EmployeeID, requestPayload{ExitType: "Absconding", LWD: *sc.LWD, Reason: sc.Reason, Note: sc.Note, NoticeDays: days, NoticeLWD: *sc.LWD}); err != nil {
				return err
			}
		case "reported-back":
			if err := hrOnly(); err != nil {
				return err
			}
			if sc.Type != "Absconding" || (st != "Show-cause" && st != "Approval") {
				return bad("Only an absconding case before approval ends this way")
			}
			if sc.RequestID != nil && *sc.ReqStatus == "Pending" {
				if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": *sc.RequestID}, bson.M{"$set": bson.M{"status": "Cancelled"}}); err != nil {
					return err
				}
			}
			if sc.HoldID != nil {
				if _, err := releaseHold(ctx, tx, c.Name, *sc.HoldID, "Employee reported back"); err != nil && !strings.Contains(err.Error(), "already") {
					return err
				}
			}
			if err := sepClose(ctx, tx, sc.ID, "Cancelled", c.Name, "Employee reported back"); err != nil {
				return err
			}
		case "lwd":
			if err := hrOnly(); err != nil {
				return err
			}
			if st != "Notice" {
				return bad("The last working day can be changed during the notice period")
			}
			if !isDate(b.LWD) || b.LWD < today() || len(b.Note) < 5 {
				return bad("Give the new last working day (today or later) and the reason")
			}
			if lim := policy.AddDays(today(), 365); b.LWD > lim { // a sanity bound: a typo is not decades of notice (HIRE-498)
				return bad("The last working day can be at most a year away (%s)", longDate(lim))
			}
			if _, err := tx.C("employee").Update(ctx, bson.M{"_id": sc.EmployeeID}, bson.M{"$set": bson.M{"lwd": b.LWD}}); err != nil {
				return err
			}
			// The case's note gains a line (the case is locked, so its note is the stored one).
			note := sc.Note + fmt.Sprintf("\nLast working day changed to %s: %s", b.LWD, b.Note)
			if _, err := tx.C("separation").Update(ctx, bson.M{"_id": sc.ID}, bson.M{"$set": bson.M{"lwd": b.LWD, "note": note}}); err != nil {
				return err
			}
			if err := sepEvent(ctx, tx, c, sc.ID, "lwd", fmt.Sprintf("Last working day changed from %s to %s: %s", longDate(sc.lwd()), longDate(b.LWD), b.Note)); err != nil {
				return err
			}
		case "interview":
			if sc.EmployeeID != c.EmployeeID && !isSepHR(c) {
				return forbidden("")
			}
			// Required for the types in the settings; HR may still hold one for any exit (prototype: "Optional for <type>").
			if !exitNeededQ(ctx, tx, sc.Type) && sc.EmployeeID == c.EmployeeID {
				return bad("No exit interview is needed for %s", strings.ToLower(sc.Type))
			}
			if !containsStr(exitMain, b.Main) || (b.Recommend != "Yes" && b.Recommend != "Maybe" && b.Recommend != "No") {
				return bad("Choose the main reason and whether they would recommend working here")
			}
			for _, q := range exitQs {
				if v := b.Ratings[q]; v < 1 || v > 5 {
					return bad("Rate %s from 1 to 5", strings.ToLower(q))
				}
			}
			// One interview per case: a second one replaces the first.
			if _, err := tx.C("exit_interview").Upsert(ctx, bson.M{"sep_id": sc.ID}, bson.M{"main_reason": b.Main, "ratings": b.Ratings, "recommend": b.Recommend,
				"comments": strings.TrimSpace(b.Comments), "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
				return err
			}
		case "letter":
			if err := hrOnly(); err != nil {
				return err
			}
			if err := s.issueLetter(ctx, tx, c, sc, st, b.Kind); err != nil {
				return err
			}
		case "close":
			if err := hrOnly(); err != nil {
				return err
			}
			if st != "Ready to close" {
				return bad("The case is at %s", strings.ToLower(st))
			}
			if !containsStr([]string{"Eligible", "Not eligible", "Only with CHRO approval"}, b.Rehire) {
				return bad("Decide whether this person can be rehired")
			}
			if _, err := tx.C("employee").Update(ctx, bson.M{"_id": sc.EmployeeID}, bson.M{"$set": bson.M{"rehire": b.Rehire}}); err != nil {
				return err
			}
			if _, err := tx.C("separation").Update(ctx, bson.M{"_id": sc.ID}, bson.M{"$set": bson.M{"rehire": b.Rehire}}); err != nil {
				return err
			}
			if err := sepClose(ctx, tx, sc.ID, "Closed", c.Name, b.Note); err != nil {
				return err
			}
		default:
			return notFound("")
		}
		out = map[string]string{"ok": act}
		return audit(ctx, tx, c, "Separation "+act, "separation", sc.ID, nil, b)
	})
	return out, err
}

func (s *Server) clearanceSign(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	var b struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 {
		if err := decode(r, &b); err != nil {
			return nil, err
		}
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		sc, err := loadSep(ctx, tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		det, err := s.sepDetail(ctx, c, sc)
		if err != nil {
			return err
		}
		st := det["stage"].(string)
		if st != "Notice" && st != "Clearance" {
			return bad("Clearance opens once the separation is approved")
		}
		item, _ := strconv.Atoi(r.PathValue("item"))
		it, _ := tx.C("clearance_item").FindOne(ctx, bson.M{"_id": item, "status": "Active"})
		if it == nil {
			return notFound("Clearance item not found")
		}
		owner, auto := str(it["owner"]), str(it["auto"])
		if auto != "" {
			return bad("This item follows the records and cannot be ticked")
		}
		if !det["canSign"].(map[string]bool)[owner] {
			return forbidden(owner + " signs this item")
		}
		// Only for a leaver inside the signer's data scope; the reporting manager signs for their own report
		// (HIRE-489).
		if mgr, _ := tx.C("employee").Exists(ctx, bson.M{"_id": sc.EmployeeID, "manager_id": c.EmployeeID}); !mgr {
			if err := c.MustInScope(ctx, tx, sc.EmployeeID); err != nil {
				return err
			}
		}
		// Signed once: a second tick keeps the first.
		if _, err := tx.C("clearance").Upsert(ctx, bson.M{"sep_id": sc.ID, "item_id": item}, nil,
			bson.M{"by_name": c.Name + " (" + c.Role + ")", "note": approvalsNullIf(b.Note)}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Clearance signed", "clearance", sc.ID+"/"+strconv.Itoa(item), nil, nil)
	})
}

// ---------- Letters ----------

func (s *Server) letterBody(ctx context.Context, q store.Q, sc *sepCase, kind string, n int) (string, error) {
	w, err := ui2SepWho(ctx, q, sc.EmployeeID)
	if err != nil {
		return "", err
	}
	name, desig, dept, loc, doj, company, addr := w.name, w.desig, w.dept, w.loc, w.doj, w.company, w.addr
	lwd := sc.lwd()
	head := fmt.Sprintf("%s\n%s\n\nDate: %s\n\nTo: %s (%s)\n%s, %s, %s\n\n", company, addr, longDate(today()), name, sc.EmployeeID, desig, dept, loc)
	switch kind {
	case "Show-cause notice":
		return head + fmt.Sprintf("Subject: Show-cause notice %d for unauthorised absence\n\nYou have been absent from work without information since %s. You are asked to report to work, or explain your absence in writing, within %d days of this letter. If we do not hear from you, the company will treat this as abandonment of employment and act under its policies.\n\nHR Department", n, longDate(policy.AddDays(lwd, 1)), sepCfg(ctx, q).AbsWait), nil
	case "Termination letter":
		opt := map[string]string{"pay": "You will be paid in lieu of the notice period in your full and final settlement.", "serve": "You are required to serve the notice period.", "none": "No notice period applies."}[sv(sc.NoticeOption)]
		return head + fmt.Sprintf("Subject: Termination of employment\n\nThis is to inform you that your employment with %s is terminated, with your last working day being %s. Reason: %s. %s\n\nPlease complete the exit clearance and return all company property. Your full and final settlement will follow the company's policy.\n\nHR Department", company, longDate(lwd), sc.Reason, opt), nil
	case "Relieving letter":
		return head + fmt.Sprintf("Subject: Relieving letter\n\nThis is to confirm that %s, %s in %s, has been relieved from the services of %s at the close of business on %s, after serving from %s. All dues have been settled in the full and final settlement.\n\nWe wish them the very best.\n\nHR Department", name, desig, dept, company, longDate(lwd), longDate(doj)), nil
	case "Experience letter":
		return head + fmt.Sprintf("TO WHOM IT MAY CONCERN\n\nThis is to certify that %s worked with %s as %s in the %s department at %s from %s to %s.\n\nDuring this period their conduct was good. We wish them success in the future.\n\nHR Department", name, company, desig, dept, loc, longDate(doj), longDate(lwd)), nil
	case "No dues certificate":
		return head + noDuesText(ctx, q, sc.EmployeeID, name, desig, dept, company, doj, lwd), nil
	}
	return "", bad("Unknown letter")
}

func (s *Server) issueLetter(ctx context.Context, tx store.Q, c *Ctx, sc *sepCase, st, kind string) error {
	switch kind {
	case "Termination letter":
		if sc.Type != "Termination" || (st == "Approval" || st == "Rejected") {
			return bad("A termination letter follows an approved termination")
		}
	case "Relieving letter", "Experience letter":
		if !relievingFor(sc.Type) {
			return bad("%s is for resignation, contract end and retirement", kind)
		}
		if st != "Letters" && st != "Ready to close" {
			return bad("%s follows the approved full and final", kind)
		}
	case "No dues certificate":
		if st == "Approval" || st == "Notice" || st == "Clearance" || st == "Show-cause" {
			return bad("A no dues certificate follows the completed clearance")
		}
		if err := fnfOwedBlocks(ctx, tx, sc.EmployeeID); err != nil {
			return err
		}
	default:
		return bad("Choose a letter")
	}
	body, err := s.letterBody(ctx, tx, sc, kind, 0)
	if err != nil {
		return err
	}
	_, err = tx.C("sep_letter").Insert(ctx, bson.M{"sep_id": sc.ID, "kind": kind, "body": body, "by_name": c.Name})
	return err
}

func (s *Server) sepLetter(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	sc, err := loadSep(ctx, s.st.DB, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if err := s.canSepView(ctx, c, sc); err != nil {
		return nil, err
	}
	lid := r.PathValue("lid")
	n, perr := strconv.ParseInt(lid, 10, 64)
	if perr != nil || strconv.FormatInt(n, 10) != lid {
		return nil, notFound("") // no letter's number reads like this
	}
	l, err := s.st.DB.C("sep_letter").FindOne(ctx, bson.M{"sep_id": sc.ID, "_id": n})
	if err != nil || l == nil {
		return nil, orNotFound(err)
	}
	return store.Row{"kind": l["kind"], "body": l["body"], "by_name": l["by_name"], "at": sepDay(l["at"])}, nil
}

// finalizeSeparation runs when the Separation approval completes: the employee goes on notice
// (or straight out, for a past day), and a recovered resignation shortfall is deducted.
func finalizeSeparation(ctx context.Context, tx store.Q, r *Request) (string, error) {
	p := payloadOf(r)
	if p.LWD == "" {
		return "", nil
	}
	t := p.ExitType
	if t == "" {
		t = "Resignation"
	}
	reason := p.Reason
	if t != "Resignation" || reason == "" {
		reason = sepReasonName(ctx, tx, t) + map[bool]string{true: ": " + p.Reason, false: ""}[p.Reason != "" && t != "Resignation"]
	}
	// The resignation date is the day the request was raised.
	var resignAt any
	if req, _ := tx.C("approval_request").FindOne(ctx, bson.M{"_id": r.ID}); req != nil {
		if t, ok := req["created_at"].(time.Time); ok {
			resignAt = store.DateOf(t)
		}
	}
	// A termination with notice to serve puts the employee on notice from approval (spec 18.1): the last working
	// day is the approval date plus the notice days, unless the approver set another date (EXT-59).
	if t == "Termination" {
		if sep, _ := tx.C("separation").FindOne(ctx, bson.M{"request_id": r.ID}); sep != nil && str(sep["notice_option"]) == "serve" && str(sep["lwd"]) == p.LWD {
			days := 0
			switch n := sep["notice_days"].(type) {
			case int32:
				days = int(n)
			case int64:
				days = int(n)
			case int:
				days = n
			}
			if lwd := policy.AddDays(today(), days); days > 0 && lwd > p.LWD {
				p.LWD = lwd
				if _, err := tx.C("separation").Update(ctx, bson.M{"_id": sep["_id"]}, bson.M{"$set": bson.M{"lwd": lwd}}); err != nil {
					return "", err
				}
			}
		}
	}
	if _, err := tx.C("employee").Update(ctx, bson.M{"_id": r.EmployeeID, "status": "Active"},
		bson.M{"$set": bson.M{"exit_status": "On notice", "lwd": p.LWD, "exit_reason": reason, "resign_at": resignAt}}); err != nil {
		return "", err
	}
	msg := "On notice. Last working day " + p.LWD + "."
	if t == "Resignation" && p.Shortfall == "recover" && p.NoticeLWD > p.LWD {
		short := policy.DayDiff(p.LWD, p.NoticeLWD)
		ym := p.LWD[:7]
		gross, _ := grossFor(ctx, tx, r.EmployeeID, ym)
		amt := calc.Round2(gross / float64(calc.PayDays(ym, "calendar")) * float64(short))
		if amt > 0 {
			if err := addLedger(ctx, tx, r.EmployeeID, maxYm(ym, openYm(ctx, tx)), "ded", fmt.Sprintf("Notice period shortfall (%d days)", short), amt, "NTC-"+r.EmployeeID, "Released early, shortfall recovered"); err != nil {
				return "", err
			}
			msg += fmt.Sprintf(" Notice shortfall of %d days recovered.", short)
		}
	}
	return msg, nil
}

// mySeparationCase is the employee's open case (exit card).
func (s *Server) mySepCase(ctx context.Context, c *Ctx) map[string]any {
	cases, err := sepFind(ctx, s.st.DB, bson.M{"employee_id": c.EmployeeID}, sepNewestFirst)
	if err != nil {
		return nil
	}
	var sc *sepCase // the newest case not hidden from them (myrequests.go)
	for _, x := range cases {
		if !sepHidden(x) {
			sc = x
			break
		}
	}
	if sc == nil {
		return nil
	}
	d, err := s.sepDetail(ctx, c, sc)
	if err != nil {
		return nil
	}
	return d
}
