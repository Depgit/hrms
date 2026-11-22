package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// Person is a resolved approver.
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var unassigned = Person{Name: approval.Unassigned}

var locationRoles = map[string]bool{"Property Manager": true, "HRBP": true, "Cluster Manager": true, "HRBP Corporate": true, "Operations Head": true, "City Head": true}

type subject struct {
	ID, Name, Grade, LocationID, Department string
	Corporate                               bool
	ManagerID, FunctionalManagerID          *string
}

// ---------- Small helpers for the approvals and separation files ----------

// approvalsJSON decodes a stored jsonb value (a map, a list or a scalar) into a Go value, as pgx scanned jsonb.
func approvalsJSON(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// approvalsStrPtr is a nullable text column as *string.
func approvalsStrPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// approvalsNullIf is the old NULLIF of a text with the empty string: an empty string is stored as null.
func approvalsNullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// approvalsInt reads an integer column (int16, int32 or int64 in a Row) as an int.
func approvalsInt(v any) int {
	switch x := v.(type) {
	case int16:
		return int(x)
	case int32:
		return int(x)
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}

// approvalsByID loads the documents of a collection whose id is in ids, keyed by id: the batch form of a JOIN on
// the id. Empty and repeated ids are asked once; ids that match nothing are absent from the map.
func approvalsByID[K comparable](ctx context.Context, q store.Q, coll string, ids []K) (map[K]store.Row, error) {
	out := map[K]store.Row{}
	var zero K
	seen := map[K]bool{}
	uniq := []K{}
	for _, id := range ids {
		if id != zero && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return out, nil
	}
	rows, err := q.C(coll).Find(ctx, bson.M{"_id": bson.M{"$in": uniq}})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if id, ok := r["id"].(K); ok {
			out[id] = r
		}
	}
	return out, nil
}

// approvalsScopeValue is the caller's data scope (the old access.Scope.SQL over `approval_subject|employee e JOIN
// department d`) for one employee-like record and its department's name, with SQL's three values: true, false, or
// nil where the comparison met a NULL column.
func approvalsScopeValue(sc access.Scope, e store.Row, deptName string) any {
	cmp := func(col, want string) any {
		v, ok := e[col].(string)
		if !ok {
			return nil
		}
		return v == want
	}
	var base any
	switch sc.Access {
	case "all":
		base = true
	case "locs":
		if v, ok := e["location_id"].(string); ok {
			base = containsStr(sc.Locations, v)
		}
	case "team":
		base = cmp("manager_id", sc.EmployeeID)
	default:
		base = cmp("id", sc.EmployeeID)
	}
	if len(sc.Departments) == 0 {
		return base
	}
	switch {
	case base == false || !containsStr(sc.Departments, deptName):
		return false
	case base == nil:
		return nil
	}
	return true
}

// approvalsInScope: the record is inside the caller's data scope (a NULL comparison is outside, as in a WHERE).
func approvalsInScope(sc access.Scope, e store.Row, deptName string) bool {
	return approvalsScopeValue(sc, e, deptName) == true
}

// ---------- Subjects and approvers ----------

// loadSubject reads who a request is about (an employee, an offer or a shortlisted candidate: the old
// approval_subject view) with their department. A subject without a department is not found, as the JOIN had it.
func loadSubject(ctx context.Context, q store.Q, id string) (*subject, error) {
	e, err := approvalSubjectOf(ctx, q, id)
	if err != nil || e == nil {
		return nil, notFound("Employee not found")
	}
	d, err := q.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]})
	if err != nil || d == nil {
		return nil, notFound("Employee not found")
	}
	s := &subject{ManagerID: approvalsStrPtr(e["manager_id"]), FunctionalManagerID: approvalsStrPtr(e["functional_manager_id"])}
	var ok [6]bool
	s.ID, ok[0] = e["id"].(string)
	s.Name, ok[1] = e["name"].(string)
	s.Grade, ok[2] = e["grade"].(string)
	s.LocationID, ok[3] = e["location_id"].(string)
	s.Department, ok[4] = d["name"].(string)
	s.Corporate, ok[5] = d["corporate"].(bool)
	for _, b := range ok {
		if !b { // a NULL where the old scan needed a value
			return nil, notFound("Employee not found")
		}
	}
	return s, nil
}

// isOfferID: an offer raised before stages (OFR-1001) or a candidate's offer (OI-0001).
func isOfferID(id string) bool { return strings.HasPrefix(id, "OFR-") || strings.HasPrefix(id, "OI-") }

// isCandidateID: a shortlisted candidate (SI-0001), the subject of the shortlist and interview requests.
func isCandidateID(id string) bool { return strings.HasPrefix(id, "SI-") }

func hasLeft(ctx context.Context, q store.Q, id string) bool {
	if id == "" {
		return false
	}
	ok, _ := q.C("employee").Exists(ctx, bson.M{"_id": id, "status": "Inactive"})
	return ok
}

// activeManager walks up the reporting line past anyone who has left.
func activeManager(ctx context.Context, q store.Q, start *string) *Person {
	id := start
	for i := 0; id != nil && *id != "" && i < 6; i++ {
		e, err := q.C("employee").FindOne(ctx, bson.M{"_id": *id}, options.FindOne().SetProjection(bson.M{"name": 1, "status": 1, "manager_id": 1}))
		if err != nil || e == nil {
			return nil
		}
		p := Person{ID: str(e["id"]), Name: str(e["name"])}
		if e["status"] != "Inactive" {
			return &p
		}
		id = approvalsStrPtr(e["manager_id"])
	}
	return nil
}

// resolvePerson finds who holds a stage's role for a request, from the role mapping. A
// location-level mapping beats an all-locations one. Line Manager comes from the record.
func resolvePerson(ctx context.Context, q store.Q, role string, s *subject, f approval.Facts) Person {
	switch role {
	case "Line Manager":
		if p := activeManager(ctx, q, s.ManagerID); p != nil {
			return *p
		}
		return resolvePerson(ctx, q, "HRBP", s, f) // manager gone and nobody above: the HRBP
	case "Functional Manager":
		if p := activeManager(ctx, q, s.FunctionalManagerID); p != nil {
			return *p
		}
		return unassigned
	case "Receiving Manager":
		if f.ToManager != "" {
			if p := activeManager(ctx, q, &f.ToManager); p != nil {
				return *p
			}
		}
		return unassigned
	case "Receiving Property Manager", "Receiving Cluster Manager", "Receiving Function Head":
		t := *s
		if f.ToLocation != "" {
			t.LocationID = f.ToLocation
		}
		if f.ToDepartment != "" {
			t.Department = f.ToDepartment
			if d, _ := q.C("department").FindOne(ctx, bson.M{"name": f.ToDepartment}); d != nil {
				if corp, ok := d["corporate"].(bool); ok {
					t.Corporate = corp
				}
			}
		}
		return resolvePerson(ctx, q, approval.StageRole(role), &t, approval.Facts{})
	}
	maps, err := loadMappings(ctx, q, role)
	if err != nil {
		return unassigned
	}
	return pickHolder(role, s, maps)
}

// loadFlow reads a workflow's rule list for a flow (leave: its leave group's list) and whether its approval
// hierarchy is off (the tick cleared, 21 Nov 2026: a new request goes to nobody and is approved at once).
func loadFlow(ctx context.Context, q store.Q, wf string, flow approval.Flow, leaveGroup string) (approval.RuleSet, bool, error) {
	var rs approval.RuleSet
	row, err := q.C(store.ApprovalFlowColl).FindOne(ctx, bson.M{"wf": approval.MatrixKey(wf, leaveGroup), "flow": string(flow)})
	if err != nil || row == nil || approvalsJSON(row["rules"], &rs.Rules) != nil {
		return rs, false, bad("No approval flow is set up for %s", wf)
	}
	return rs, row["hierarchy_off"] == true, nil
}

// skipRoles are the HR roles that can skip approval (of a flow whose approval hierarchy is off). Employees and
// managers never can.
var skipRoles = []string{"HR Admin", "HRBP", "HRBP Corporate", "CHRO"}

// mayCanSkip says whether the caller's role lets them skip approval of a request raised by raisedBy: HR Admin, an
// HR role with Approval flows at Edit, or the HR person who raised it. The flow must allow it too (skipApprovalTx).
func mayCanSkip(c *Ctx, raisedBy string) bool {
	if c == nil || !containsStr(skipRoles, c.Role) {
		return false
	}
	return c.Role == "HR Admin" || c.Level("appr.setup") >= 2 || (raisedBy != "" && raisedBy == c.EmployeeID)
}

// flowSkippable says whether a request's flow has its approval hierarchy off, so HR may skip approval of a request
// raised before the tick was cleared and still waiting (a new one is approved at once).
func flowSkippable(ctx context.Context, q store.Q, r *Request) bool {
	_, off, err := loadFlow(ctx, q, r.WF, approval.Flow(r.Flow), r.Facts.LeaveGroup)
	return err == nil && off
}

// canSkip: the request waits for approval, its flow lets HR skip, and the caller may (never on a request about them).
func canSkip(ctx context.Context, q store.Q, c *Ctx, r *Request) bool {
	return r.Status == "Pending" && r.EmployeeID != c.EmployeeID && mayCanSkip(c, r.RaisedBy) && flowSkippable(ctx, q, r)
}

// AutoNote is the history line of a request approved at once because its flow's approval hierarchy is off.
const AutoNote = "Approved automatically: approval hierarchy is off"

// autoApprove approves a request with no approvers: the history says why when its flow's hierarchy is off.
func (s *Server) autoApprove(ctx context.Context, tx store.Q, r *Request, off bool) error {
	if off {
		if err := history(ctx, tx, r.ID, "", "Approved", "System", AutoNote); err != nil {
			return err
		}
	}
	return s.finalize(ctx, tx, r)
}

// SkipNote is the history line of a request HR approved without waiting for its approvers.
func SkipNote(name, reason string) string { return "Approval skipped by " + name + ": " + reason }

// skipApprovalTx approves a waiting request at once without its approvers: HR's reason is required, the flow's
// approval hierarchy must be off, and the caller an HR role allowed to skip (mayCanSkip). A history line and an
// audit entry, the pending stages close (nothing waits on anyone), then the usual finalize, as after a real approval.
func (s *Server) skipApprovalTx(ctx context.Context, tx store.Q, c *Ctx, r *Request, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return bad("Give a reason to skip approval")
	}
	if r.Status != "Pending" {
		return bad("Only a request waiting for approval can be skipped")
	}
	if !mayCanSkip(c, r.RaisedBy) {
		return forbidden("Only HR can skip approval")
	}
	if r.EmployeeID == c.EmployeeID {
		return forbidden("You cannot skip approval of a request about you")
	}
	if !flowSkippable(ctx, tx, r) {
		return bad("Approval cannot be skipped: the approval hierarchy of this flow is required")
	}
	stage := ""
	if r.Step < len(r.Chain) {
		stage = r.Chain[r.Step]
	}
	note := SkipNote(c.Name, reason)
	if err := history(ctx, tx, r.ID, stage, "Approved", c.Name, note); err != nil {
		return err
	}
	r.Step = len(r.Chain)
	if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"step": r.Step, "stage_since": tx.Now()}}); err != nil {
		return err
	}
	if err := audit(ctx, tx, c, "Approval skipped", "approval_request", r.ID, nil, map[string]any{"wf": r.WF, "flow": r.Flow, "reason": reason, "status": "Approved"}); err != nil {
		return err
	}
	return s.finalize(ctx, tx, r)
}

// storedFlow is one saved rule list.
type storedFlow struct {
	WF    string
	Flow  string
	Rules []approval.Rule
	Off   bool // approval hierarchy off: new requests are approved at once (the rules are kept for when it is ticked again)
}

// loadFlows reads every rule list, by workflow key and flow.
func loadFlows(ctx context.Context, q store.Q) ([]storedFlow, error) {
	rows, err := q.C(store.ApprovalFlowColl).Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "wf", Value: 1}, {Key: "flow", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]storedFlow, 0, len(rows))
	for _, x := range rows {
		f := storedFlow{WF: str(x["wf"]), Flow: str(x["flow"]), Off: x["hierarchy_off"] == true}
		if err := approvalsJSON(x["rules"], &f.Rules); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// inUse says whether a rule can decide a request: switched on, or the last rule (Otherwise, always used). A flow
// whose approval hierarchy is off keeps its rules, but planFor sends its requests to nobody.
func (f storedFlow) inUse(i int) bool { return f.Rules[i].On || i == len(f.Rules)-1 }

// StagePerson is a planned stage with the named person and their state.
type StagePerson struct {
	Stage string `json:"stage"`
	Person
	State string `json:"state"`
}

type Plan struct {
	Flow   approval.Flow     `json:"flow"`
	Chain  []string          `json:"chain"`
	People []StagePerson     `json:"people"`
	Notify []approval.Notify `json:"notify"`
	Why    []string          `json:"why"`
	// Rule is the name of the rule that decided ("Otherwise" when none above it fitted). Why is what is saved on
	// the request: that rule's name (none for Otherwise) and then what added approvers beyond it.
	Rule string `json:"rule"`
	// HierarchyOff: the flow's approval hierarchy is off. The request goes to nobody and is approved at once.
	HierarchyOff bool `json:"hierarchyOff,omitempty"`
}

// planChain works out who approves, in order, with named people (simulator and create).
func planChain(ctx context.Context, q store.Q, wf, employeeID string, f approval.Facts) (*Plan, error) {
	s, err := loadSubject(ctx, q, employeeID)
	if err != nil {
		return nil, err
	}
	return planFor(ctx, q, wf, s, f)
}

// planFor plans the chain for a subject that may not be saved yet (an offer being checked).
// personKey tells people apart on a route: the employee id, or the name when nobody is named (Unassigned).
func personKey(p Person) string {
	if p.ID != "" {
		return p.ID
	}
	return p.Name
}

func planFor(ctx context.Context, q store.Q, wf string, s *subject, f approval.Facts) (*Plan, error) {
	employeeID := s.ID
	flow := approval.Operations
	if s.Corporate {
		flow = approval.Corporate
	}
	m, off, err := loadFlow(ctx, q, wf, flow, f.LeaveGroup)
	if err != nil {
		return nil, err
	}
	f.WF, f.Grade = wf, s.Grade
	cache := map[string]Person{}
	person := func(role string) Person {
		if p, ok := cache[role]; ok {
			return p
		}
		p := resolvePerson(ctx, q, role, s, f)
		cache[role] = p
		return p
	}
	c := approval.BuildKeyed(m, f, func(role string) string { return person(role).Name }, func(role string) string { return personKey(person(role)) })
	rule := ""
	if i := approval.Pick(m, f); i >= 0 {
		rule = m.Rules[i].Label
		if i == len(m.Rules)-1 {
			rule = approval.OtherwiseLabel
		}
	}
	// Moving into a corporate department adds its Function Head and the CHRO (spec 7.3).
	if wf == "transfer" && f.ToDepartment != "" && f.ToDepartment != s.Department {
		if d, err := q.C("department").FindOne(ctx, bson.M{"name": f.ToDepartment}); err == nil && d != nil && d["corporate"] == true {
			for _, st := range []string{"Receiving Function Head", "CHRO"} {
				if !containsStr(c.Chain, st) {
					c.Chain = append(c.Chain, st)
				}
			}
			c.Why = append(c.Why, "Moving into a corporate department")
		}
	}
	// A rehire marked "Only with CHRO approval" adds the CHRO (spec 8.7).
	if wf == "joining" && f.RehireCHRO && !containsStr(c.Chain, "CHRO") {
		c.Chain = append(c.Chain, "CHRO")
		c.Why = append(c.Why, "Rehire needs CHRO approval")
	}
	// "Approval hierarchy required" unticked (21 Nov 2026): no approvers at all; the request is approved at once.
	if off {
		c.Chain, c.Why = []string{}, []string{AutoNote}
	}
	p := &Plan{Flow: flow, Chain: c.Chain, Notify: c.Notify, Why: c.Why, Rule: rule, People: []StagePerson{}, HierarchyOff: off}
	for _, st := range c.Chain {
		pp := person(st)
		state := "Ready"
		switch {
		case pp.ID == "":
			state = "No name set"
		case pp.ID == employeeID:
			state = "Is the employee"
		case hasLeft(ctx, q, pp.ID):
			state = "Has left"
		}
		p.People = append(p.People, StagePerson{st, pp, state})
	}
	return p, nil
}

// Finalizer runs when the last approver approves; it may return a note for the history.
type Finalizer func(ctx context.Context, tx store.Q, req *Request) (string, error)

type Request struct {
	ID         string          `json:"id"`
	WF         string          `json:"wf"`
	EmployeeID string          `json:"employee_id"`
	RaisedBy   string          `json:"raised_by"`
	Flow       string          `json:"flow"`
	Facts      approval.Facts  `json:"facts"`
	Payload    json.RawMessage `json:"payload"`
	Chain      []string        `json:"chain"`
	Step       int             `json:"step"`
	Status     string          `json:"status"`
	StageSince time.Time       `json:"stage_since"`
}

// loadRequest reads one approval request; lock takes it FOR UPDATE inside a transaction.
func loadRequest(ctx context.Context, q store.Q, id string, lock bool) (*Request, error) {
	var row store.Row
	var err error
	if lock {
		row, err = q.C("approval_request").Lock(ctx, bson.M{"_id": id})
	} else {
		row, err = q.C("approval_request").FindOne(ctx, bson.M{"_id": id})
	}
	if store.Transient(err) {
		return nil, err // another transaction holds the request: Tx runs this again once it commits (FOR UPDATE waited)
	}
	if err != nil || row == nil {
		return nil, notFound("Request not found")
	}
	if row["employee_id"] == nil && row["offer_id"] == nil && row["candidate_id"] == nil {
		return nil, notFound("Request not found") // nobody it is about: the old COALESCE scanned NULL and failed
	}
	r := &Request{ID: str(row["id"]), WF: str(row["wf"]), EmployeeID: approvalSubjectID(row), RaisedBy: str(row["raised_by"]),
		Flow: str(row["flow"]), Step: approvalsInt(row["step"]), Status: str(row["status"])}
	r.StageSince, _ = row["stage_since"].(time.Time)
	if r.Payload, err = json.Marshal(row["payload"]); err != nil {
		return nil, notFound("Request not found")
	}
	if approvalsJSON(row["facts"], &r.Facts) != nil || approvalsJSON(row["chain"], &r.Chain) != nil {
		return nil, notFound("Request not found")
	}
	return r, nil
}

// history adds a line to a request's history (an empty stage or note is stored as null).
func history(ctx context.Context, q store.Q, reqID, stage, action, by, note string) error {
	_, err := q.C("approval_history").Insert(ctx, bson.M{"request_id": reqID, "stage": approvalsNullIf(stage), "action": action, "by_name": by, "note": approvalsNullIf(note)})
	return err
}

func (s *Server) finalize(ctx context.Context, tx store.Q, r *Request) error {
	// Leave or a regularization on a month already paid is not approved into it (REAL-49): reject it and raise an arrear.
	if from, to := requestDates(r); from != "" {
		if msg := paidMonthMsg(ctx, tx, from, to); msg != "" {
			return bad("%s", msg)
		}
	}
	r.Status = "Approved"
	if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"status": "Approved"}}); err != nil {
		return err
	}
	if fn := s.finalizers[r.WF]; fn != nil {
		note, err := fn(ctx, tx, r)
		if err != nil {
			return err
		}
		if note != "" {
			return history(ctx, tx, r.ID, "", "Applied", "System", note)
		}
	}
	return nil
}

var prefixes = map[string]string{"joining": "REQ", "verify": "JV", "leave": "LV", "regularize": "AR", "transfer": "TR", "resign": "RS",
	"p_claim": "CLM", "p_adv": "ADV", "p_ded": "DED", "p_arrear": "ARR", "p_change": "PR", "p_fnf": "FNF", "p_ack": "PA", "shortlist": "SL", "interview": "INT"}

// CreateRequest builds the chain, stores the request, skips vacant stages and finalizes when no approval is needed.
func (s *Server) CreateRequest(ctx context.Context, c *Ctx, wf, employeeID string, f approval.Facts, payload any) (map[string]any, error) {
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		out, err = s.createRequestTx(ctx, tx, c, wf, employeeID, f, payload)
		return err
	})
	return out, err
}

// createRequestTx is CreateRequest inside the caller's transaction, so a request can be raised
// together with the record it is about (an offer, a transfer).
func (s *Server) createRequestTx(ctx context.Context, tx store.Q, c *Ctx, wf, employeeID string, f approval.Facts, payload any) (map[string]any, error) {
	var out map[string]any
	err := func() error {
		if c.Role != "HR Admin" && employeeID != c.EmployeeID {
			if err := c.MustInScope(ctx, tx, employeeID); err != nil {
				return err
			}
		}
		if wf == "joining" {
			f.InitiatorRole = c.Role
		}
		p, err := planChain(ctx, tx, wf, employeeID, f)
		if err != nil {
			return err
		}
		n, err := store.NextSeq(ctx, tx, "approval_request_seq")
		if err != nil {
			return err
		}
		pre := prefixes[wf]
		if pre == "" {
			pre = "RQ"
		}
		id := fmt.Sprintf("%s-%d", pre, n)
		f.WF = wf
		// An offer or a shortlist is about a candidate who has no employee record yet.
		empCol, offerCol, candCol := &employeeID, (*string)(nil), (*string)(nil)
		if isOfferID(employeeID) {
			empCol, offerCol = nil, &employeeID
		} else if isCandidateID(employeeID) {
			empCol, candCol = nil, &employeeID
		}
		if _, err := tx.C("approval_request").Insert(ctx, bson.M{"_id": id, "wf": wf, "employee_id": empCol, "offer_id": offerCol, "candidate_id": candCol,
			"raised_by": c.EmployeeID, "flow": string(p.Flow), "facts": f, "payload": jsonOrNil(payload), "chain": p.Chain, "notify": p.Notify, "why": p.Why}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, "Raised "+strings.ToLower(workflowName(wf))+" request", "approval_request", id, nil, map[string]any{"facts": f, "payload": payload}); err != nil {
			return err
		}
		req, err := loadRequest(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := skipVacant(ctx, tx, req); err != nil {
			return err
		}
		if c != nil && c.Skip && req.Status == "Pending" && len(p.Chain) > 0 { // HR's "Skip approval" when raising it
			if err := s.skipApprovalTx(ctx, tx, c, req, c.SkipReason); err != nil {
				return err
			}
		} else if len(p.Chain) == 0 {
			if err := s.autoApprove(ctx, tx, req, p.HierarchyOff); err != nil {
				return err
			}
		}
		out = map[string]any{"id": id, "status": req.Status, "plan": p}
		return nil
	}()
	return out, err
}

// blockedFn says why a stage of a request cannot be decided, or "" when it can: nobody is
// named, or the approver has left, raised the request, is the person it is about, or already
// holds an earlier stage (a person is asked once).
func blockedFn(ctx context.Context, q store.Q, r *Request, s *subject) func(stage string, earlier []string) string {
	cache := map[string]Person{}
	person := func(stage string) Person {
		if p, ok := cache[stage]; ok {
			return p
		}
		p := resolvePerson(ctx, q, stage, s, r.Facts)
		cache[stage] = p
		return p
	}
	return func(stage string, earlier []string) string {
		p := person(stage)
		switch {
		case p.ID == "":
			return "No one is named for " + stage
		case p.ID == r.RaisedBy:
			return p.Name + " (" + stage + ") raised this request"
		case p.ID == r.EmployeeID:
			return p.Name + " (" + stage + ") is the person this request is about"
		case hasLeft(ctx, q, p.ID):
			return p.Name + " (" + stage + ") has left"
		}
		for _, st := range earlier {
			if st != stage && person(st).ID == p.ID {
				return p.Name + " is also " + st + " and is asked once"
			}
		}
		return ""
	}
}

// skipVacant moves past stages nobody can decide (see blockedFn): to the next stage, or at
// the last stage to its escalation role, then to the fallback roles. Each move is in the history.
func skipVacant(ctx context.Context, tx store.Q, r *Request) error {
	if r.Status != "Pending" || r.Step >= len(r.Chain) {
		return nil
	}
	s, err := loadSubject(ctx, tx, r.EmployeeID)
	if err != nil {
		return err
	}
	before, step := strings.Join(r.Chain, "|"), r.Step
	chain, next, notes := approval.RouteVacant(append([]string{}, r.Chain...), r.Step, blockedFn(ctx, tx, r, s))
	if next == step && strings.Join(chain, "|") == before {
		return nil // nothing moved: the sweep does not repeat the same note
	}
	r.Chain, r.Step = chain, next
	for _, n := range notes {
		if err := history(ctx, tx, r.ID, "", "Skipped", "System", n); err != nil {
			return err
		}
	}
	_, err = tx.C("approval_request").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"chain": r.Chain, "step": r.Step, "stage_since": tx.Now()}})
	return err
}

// finishBlockedLast completes a request stuck at its last stage with nobody left to escalate to, when the only
// people who could decide it are its raiser or someone who already approved an earlier stage (an HR Admin raising
// leave for someone whose line manager is the CHRO: payroll test REAL-15). Nobody approves a request they raised and
// a person is asked once, so the stage is treated as vacant; with no stage after it, the approvals already given by
// others complete the request. A stage nobody holds, or held by someone who left, still waits (for HR Admin).
func (s *Server) finishBlockedLast(ctx context.Context, tx store.Q, r *Request) (bool, error) {
	if r.Status != "Pending" || r.Step >= len(r.Chain) || r.Step == 0 {
		return false, nil
	}
	sub, err := loadSubject(ctx, tx, r.EmployeeID)
	if err != nil {
		return false, nil
	}
	why := blockedFn(ctx, tx, r, sub)(r.Chain[r.Step], r.Chain[:r.Step])
	if !strings.HasSuffix(why, "raised this request") && !strings.HasSuffix(why, "and is asked once") {
		return false, nil
	}
	if ok, err := tx.C("approval_history").Exists(ctx, bson.M{"request_id": r.ID, "action": "Approved"}); err != nil || !ok {
		return false, err
	}
	if err := history(ctx, tx, r.ID, "", "Skipped", "System", why+". Nobody else can decide it, so the approvals already given complete the request"); err != nil {
		return false, err
	}
	r.Step = len(r.Chain)
	if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"step": r.Step, "stage_since": tx.Now()}}); err != nil {
		return false, err
	}
	return true, s.finalize(ctx, tx, r)
}

// RerouteOpen re-checks every open request, so one waiting on someone who has since left,
// lost the role, or cannot decide it moves on (run by the maintenance job).
func (s *Server) RerouteOpen(ctx context.Context) (int, error) {
	rows, err := s.st.DB.C("approval_request").Find(ctx, bson.M{"status": "Pending"},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}).SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		moved := false // set by the last attempt of the transaction, which may run more than once
		err := s.st.Tx(ctx, func(tx store.Q) error {
			moved = false
			r, err := loadRequest(ctx, tx, row["id"].(string), true)
			if err != nil || r.Status != "Pending" {
				return err
			}
			step, chain := r.Step, strings.Join(r.Chain, "|")
			if err := skipVacant(ctx, tx, r); err != nil {
				return err
			}
			if done, err := s.finishBlockedLast(ctx, tx, r); err != nil || done {
				moved = done
				return err
			}
			moved = r.Step != step || strings.Join(r.Chain, "|") != chain
			return nil
		})
		if moved {
			n++
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// canAct: the named person, or a holder of the stage's role whose scope covers the employee,
// or HR Admin (override). Never the requester or the person the request is about.
func canAct(ctx context.Context, q store.Q, c *Ctx, r *Request) bool {
	if r.WF == "interview" {
		return interviewer(ctx, q, c, r)
	}
	if r.Status != "Pending" || r.Step >= len(r.Chain) || r.EmployeeID == c.EmployeeID || r.RaisedBy == c.EmployeeID {
		return false
	}
	if c.Role == "HR Admin" || ackDeputy(ctx, q, c, r) {
		return true
	}
	stage := r.Chain[r.Step]
	s, err := loadSubject(ctx, q, r.EmployeeID)
	if err != nil {
		return false
	}
	// The person named for the stage decides it from whichever view is active: a manager covering for one who
	// left is named as Line Manager without holding that role (payroll test REAL-61).
	if resolvePerson(ctx, q, stage, s, r.Facts).ID == c.EmployeeID {
		return true
	}
	payItem := len(r.WF) > 2 && r.WF[:2] == "p_" && c.Level("pay.claims") >= 2
	if c.Level("appr.queue") < 2 && !payItem {
		return false
	}
	if approval.StageRole(stage) != c.Role {
		return false
	}
	if len(stage) > 9 && stage[:9] == "Receiving" {
		return true
	}
	ok, _ := c.InScope(ctx, q, r.EmployeeID)
	return ok
}

// Decide approves or rejects the current stage.
func (s *Server) Decide(ctx context.Context, c *Ctx, id, action, note string) (map[string]any, error) {
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		out, err = s.decideTx(ctx, tx, c, id, action, note)
		return err
	})
	return out, err
}

// decideTx is Decide inside the caller's transaction (the interview records its result with the decision).
func (s *Server) decideTx(ctx context.Context, tx store.Q, c *Ctx, id, action, note string) (map[string]any, error) {
	var out map[string]any
	err := func() error {
		r, err := loadRequest(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if !canAct(ctx, tx, c, r) {
			if r.EmployeeID == c.EmployeeID || r.RaisedBy == c.EmployeeID {
				return forbidden("You cannot decide your own request")
			}
			return forbidden("This request is not waiting on you")
		}
		stage := r.Chain[r.Step]
		if action == "reject" {
			if note == "" {
				return bad("Add a comment to reject")
			}
			if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": "Rejected"}}); err != nil {
				return err
			}
			out = map[string]any{"status": "Rejected"}
			if err := history(ctx, tx, id, stage, "Rejected", c.Name, note); err != nil {
				return err
			}
			if err := candidateRequestClosed(ctx, tx, c, r, "Rejected", note); err != nil {
				return err
			}
			return audit(ctx, tx, c, "Rejected request", "approval_request", id, nil, nil)
		}
		override := c.Role == "HR Admin" && approval.StageRole(stage) != "HR Admin" && r.WF != "interview" // the interview is the hiring manager's own
		by := c.Name
		if override {
			if note == "" {
				return bad("Add a comment: you are approving on behalf of %s", stage)
			}
			by = c.Name + " (HR Admin, on behalf of " + stage + ")"
		} else {
			by = ackOnBehalf(ctx, tx, c, r, stage) // an HRBP deciding a policy acknowledgement
		}
		if err := history(ctx, tx, id, stage, "Approved", by, note); err != nil {
			return err
		}
		r.Step++
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"step": r.Step, "stage_since": tx.Now()}}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, "Approved request stage", "approval_request", id, nil, map[string]string{"stage": stage}); err != nil {
			return err
		}
		if r.Step >= len(r.Chain) {
			out = map[string]any{"status": "Approved"}
			return s.finalize(ctx, tx, r)
		}
		if err := skipVacant(ctx, tx, r); err != nil {
			return err
		}
		if done, err := s.finishBlockedLast(ctx, tx, r); err != nil || done {
			out = map[string]any{"status": r.Status}
			return err
		}
		out = map[string]any{"status": "Pending", "next": r.Chain[r.Step]}
		return nil
	}()
	return out, err
}

// slaHours is the one response time for every approval stage (rulings: no turnaround matrices).
func slaHours(ctx context.Context, q store.Q) int {
	h := 24
	if row, _ := q.C("approval_setting").FindOne(ctx, bson.M{"_id": 1}); row != nil {
		if v, ok := row["sla_hours"].(int32); ok {
			h = int(v)
		}
	}
	return h
}

// noResponse applies a workflow's "if nobody acts" rule to the current stage: escalate to the
// stage's escalation role, auto-approve, or keep waiting. It returns whether anything changed.
func (s *Server) noResponse(ctx context.Context, tx store.Q, r *Request, mode string, hours int) (bool, error) {
	stage := r.Chain[r.Step]
	switch mode {
	case "auto":
		if err := history(ctx, tx, r.ID, stage, "Approved", "System", fmt.Sprintf("Auto-approved after %dh without a response", hours)); err != nil {
			return false, err
		}
		r.Step++
		if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"step": r.Step, "stage_since": tx.Now()}}); err != nil {
			return false, err
		}
		if r.Step >= len(r.Chain) {
			return true, s.finalize(ctx, tx, r)
		}
		return true, skipVacant(ctx, tx, r)
	case "wait":
		return false, nil
	}
	// Escalating to the person who did not respond helps nobody: go one role higher.
	subj, err := loadSubject(ctx, tx, r.EmployeeID)
	if err != nil {
		return false, err
	}
	silent := resolvePerson(ctx, tx, stage, subj, r.Facts).ID
	esc := approval.SLA(stage).Esc
	for i := 0; esc != "" && silent != "" && i < 6 && resolvePerson(ctx, tx, esc, subj, r.Facts).ID == silent; i++ {
		esc = approval.SLA(esc).Esc
	}
	if esc == "" {
		return false, nil
	}
	// The escalation role takes this stage; if it was also a later stage, it is asked once.
	chain := []string{}
	for i, st := range r.Chain {
		if i > r.Step && st == esc {
			continue
		}
		chain = append(chain, st)
	}
	chain[r.Step] = esc
	r.Chain = chain
	if _, err := tx.C("approval_request").Update(ctx, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"chain": r.Chain, "stage_since": tx.Now()}}); err != nil {
		return false, err
	}
	if err := history(ctx, tx, r.ID, stage, "Escalated", "System", fmt.Sprintf("No response in %dh, escalated to %s", hours, esc)); err != nil {
		return false, err
	}
	// Nobody named for the escalation role, or they cannot decide it: move on from there.
	return true, skipVacant(ctx, tx, r)
}

// noResponseMode is the workflow's "if nobody acts" setting for a request: the no_response of its rule list for
// the flow (a leave request reads its leave group's list, C when it has none), "escalate" when there is no such
// list.
func noResponseMode(ctx context.Context, q store.Q, r *Request) (string, error) {
	wf := r.WF
	if wf == "leave" {
		g := r.Facts.LeaveGroup
		if g == "" {
			g = "C"
		}
		wf = "leave" + g
	}
	m, err := q.C(store.ApprovalFlowColl).FindOne(ctx, bson.M{"wf": wf, "flow": r.Flow})
	if err != nil {
		return "", err
	}
	if mode, ok := m["no_response"].(string); ok {
		return mode, nil
	}
	return "escalate", nil
}

// SLASweep: overdue stages escalate, auto-approve or keep waiting, per the workflow's setting.
func (s *Server) SLASweep(ctx context.Context) (int, error) {
	hours := slaHours(ctx, s.st.DB)
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := s.st.DB.C("approval_request").Find(ctx, bson.M{"status": "Pending", "stage_since": bson.M{"$lt": cutoff}},
		options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		changed := false // set by the last attempt of the transaction, which may run more than once
		err := s.st.Tx(ctx, func(tx store.Q) error {
			changed = false
			r, err := loadRequest(ctx, tx, row["id"].(string), true)
			if err != nil || r.Status != "Pending" || r.Step >= len(r.Chain) || time.Since(r.StageSince) < time.Duration(hours)*time.Hour {
				return err
			}
			mode, err := noResponseMode(ctx, tx, r)
			if err != nil {
				return err
			}
			changed, err = s.noResponse(ctx, tx, r, mode, hours)
			return err
		})
		if changed {
			n++
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func workflowName(wf string) string {
	for _, w := range approval.Workflows {
		if w[0] == wf {
			return w[1]
		}
	}
	return wf
}
