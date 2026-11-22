package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// Onboarding in stages (owner's process, 11 Nov 2026). Every new hire starts with a shortlist and keeps one number:
//
//	Shortlisting ID SI-0001: HR fills in personal details, work experience, existing CTC and location; the hiring
//	  manager, then the department's Function Head approve it (wf shortlist, editable in Approval routes).
//	Interview: the hiring manager records Selected or Not selected, with the date and feedback (wf interview, an
//	  approval-engine step in their Approvals with those two buttons; it never escalates to anyone else).
//	Offer ID OI-0001: HR completes today's offer form, prefilled from the shortlist → today's offer approval (wf
//	  joining, pay band and budget checks) → release → accept or decline. No employee record is made here.
//	Joining ID JI-0001: HR marks the candidate joined (the actual date) or not joined; joining details and documents.
//	Employee ID: the next code of the series, once the mandatory joining documents and details are in, or at joining
//	  (Onboarding setup). It does what offer approval used to do: the employee record as Pre-boarding, pay,
//	  personal, bank, and the documents' employee_id. Payroll verification then makes them active, as before.
//
// A candidate closed at any stage (rejected, withdrawn, not selected, declined, not joined) never uses an Employee ID.
// Offers raised before stages (OFR-1001) have no candidate and keep their own flow in onboarding.go.

var reCandID = regexp.MustCompile(`^(SI|OI|JI)-(\d{1,9})$`)

// candidateNo reads the number of SI-0001, OI-0001 or JI-0001.
func candidateNo(id string) (int, bool) {
	m := reCandID.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(id)))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[2])
	return n, err == nil && n > 0
}

// stageID is the number with a stage's prefix: SI-0001, OI-0001, JI-0001 (four digits, more past 9999).
func stageID(prefix string, no int) string { return fmt.Sprintf("%s-%04d", prefix, no) }

func toInt(v any) int {
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

// onboardingStagesCandidates lists candidates (in sort order, at most limit; 0 for all) as the pipeline and the
// candidate view read them: the candidate with the names of their department, designation, place, hiring manager,
// raiser and interviewer, the status of their shortlist and interview requests, and their offer (one per candidate)
// with its approval and Payroll verification status. Offer and request columns are null when there is none. A
// candidate whose department, designation, place, hiring manager or raiser is missing is left out, as the old
// inner joins did.
func onboardingStagesCandidates(ctx context.Context, q store.Q, filter bson.D, sort bson.D, limit int64) ([]store.Row, error) {
	opts := options.Find().SetSort(sort)
	if limit > 0 {
		opts.SetLimit(limit)
	}
	cands, err := q.C("candidate").Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return cands, nil
	}
	var ids, deptIDs, desigIDs, locIDs, people, reqIDs []string
	for _, x := range cands {
		ids, deptIDs, desigIDs, locIDs = append(ids, str(x["id"])), append(deptIDs, str(x["department_id"])), append(desigIDs, str(x["designation_id"])), append(locIDs, str(x["location_id"]))
		people = append(people, str(x["manager_id"]), str(x["raised_by"]), str(x["interviewed_by"]))
		reqIDs = append(reqIDs, str(x["shortlist_request_id"]), str(x["interview_request_id"]))
	}
	offers := map[string]store.Row{}
	rows, err := q.C("offer").Find(ctx, bson.D{{Key: "candidate_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, err
	}
	for _, o := range rows {
		offers[str(o["candidate_id"])] = o
		reqIDs = append(reqIDs, str(o["request_id"]), str(o["verify_request_id"]))
	}
	depts, err := onboardingByID(ctx, q, "department", deptIDs...)
	if err != nil {
		return nil, err
	}
	desigs, err := onboardingByID(ctx, q, "designation", desigIDs...)
	if err != nil {
		return nil, err
	}
	places, err := onboardingPlaces(ctx, q, locIDs)
	if err != nil {
		return nil, err
	}
	emps, err := onboardingByID(ctx, q, "employee", people...)
	if err != nil {
		return nil, err
	}
	reqs, err := onboardingByID(ctx, q, "approval_request", reqIDs...)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, x := range cands {
		d, g, p := depts[str(x["department_id"])], desigs[str(x["designation_id"])], places[str(x["location_id"])]
		m, rb, ib := emps[str(x["manager_id"])], emps[str(x["raised_by"])], emps[str(x["interviewed_by"])]
		if d == nil || g == nil || p.loc == nil || m == nil || rb == nil {
			continue
		}
		o := offers[str(x["id"])]
		sr, ir := reqs[str(x["shortlist_request_id"])], reqs[str(x["interview_request_id"])]
		r, v := reqs[str(onboardingCol(o, "request_id"))], reqs[str(onboardingCol(o, "verify_request_id"))]
		out = append(out, store.Row{"id": x["id"], "no": x["no"], "name": x["name"], "data": x["data"], "existing_ctc": x["existing_ctc"], "stage": x["stage"],
			"employee_id": x["employee_id"], "rejoin_of": x["rejoin_of"], "created_at": x["created_at"],
			"department_id": x["department_id"], "department": d["name"], "corporate": d["corporate"], "designation_id": x["designation_id"], "designation": g["name"],
			"location_id": x["location_id"], "location": p.loc["name"], "city_id": p.city["id"], "city": p.city["name"],
			"manager_id": x["manager_id"], "manager_name": m["name"], "raised_by": x["raised_by"], "raised_by_name": rb["name"], "raised_role": x["raised_role"],
			"shortlist_request_id": x["shortlist_request_id"], "interview_request_id": x["interview_request_id"], "interview_date": x["interview_date"],
			"interview_result": x["interview_result"], "interview_feedback": x["interview_feedback"], "interviewed_by": x["interviewed_by"],
			"interviewed_by_name": onboardingCol(ib, "name"), "interviewed_at": x["interviewed_at"], "joined_on": x["joined_on"], "joined_at": x["joined_at"],
			"joined_by": x["joined_by"], "employee_created_at": x["employee_created_at"], "closed_kind": x["closed_kind"], "closed_reason": x["closed_reason"],
			"closed_at": x["closed_at"], "closed_by": x["closed_by"],
			"offer_no": x["offer_no"], "offer_version": x["offer_version"], "waiting_window": x["waiting"], "bgv": x["bgv"], "ji_at": x["ji_at"], "approved_at": x["approved_at"],
			"expires_on": onboardingCol(o, "expires_on"), "query_at": onboardingCol(o, "query_at"), "query_text": onboardingCol(o, "query_text"), "lapsed_at": onboardingCol(o, "lapsed_at"),
			"shortlist_status": onboardingCol(sr, "status"), "interview_status": onboardingCol(ir, "status"),
			"offer_id": onboardingCol(o, "id"), "type": onboardingCol(o, "type"), "doj": onboardingCol(o, "doj"), "gross": onboardingCol(o, "gross"),
			"grade": onboardingCol(o, "grade"), "offer_data": onboardingCol(o, "data"), "released_at": onboardingCol(o, "released_at"),
			"accepted_at": onboardingCol(o, "accepted_at"), "declined_at": onboardingCol(o, "declined_at"), "verified_at": onboardingCol(o, "verified_at"),
			"offer_request_id": onboardingCol(o, "request_id"), "offer_status": onboardingCol(r, "status"),
			"verify_request_id": onboardingCol(o, "verify_request_id"), "verify_status": onboardingCol(v, "status")})
	}
	return out, nil
}

// currentID is the ID of the stage the candidate is at: their record's id once it exists (the Joining ID JI-…, then
// the Employee ID), else JI after joining, the Offer ID once issued (at acceptance; earlier for candidates selected
// before 17 Nov 2026), or SI.
func currentID(x store.Row) string {
	if e := str(x["employee_id"]); e != "" {
		return e
	}
	no := toInt(x["no"])
	switch {
	case x["joined_on"] != nil:
		return stageID("JI", no)
	case str(x["offer_no"]) != "":
		return str(x["offer_no"])
	}
	return stageID("SI", no)
}

// bucket is the pipeline column: Shortlisted, Interview, Offer, Joined (documents pending), With Payroll, Done, Closed.
func bucket(x store.Row) string {
	if x["stage"] != "Closed" && x["waiting_window"] != nil { // no-show or stopped coming (hiring.go)
		return "Waiting"
	}
	if x["stage"] == "Joined" {
		switch {
		case x["ji_at"] != nil && x["approved_at"] == nil && x["verified_at"] != nil: // verified; the Employee ID waits (background check)
			return "Joined"
		case x["verified_at"] != nil:
			return "Done"
		case x["verify_status"] == "Pending":
			return "With Payroll"
		}
	}
	return str(x["stage"])
}

var pipelineBuckets = []string{"Shortlisted", "Interview", "Offer", "Waiting", "Joined", "With Payroll", "Done", "Closed"}

// waitingOn is the stage and the person a pending request waits on.
func waitingOn(ctx context.Context, q store.Q, reqID string) (string, string) {
	if reqID == "" {
		return "", ""
	}
	req, err := loadRequest(ctx, q, reqID, false)
	if err != nil || req.Status != "Pending" || req.Step >= len(req.Chain) {
		return "", ""
	}
	sub, err := loadSubject(ctx, q, req.EmployeeID)
	if err != nil {
		return req.Chain[req.Step], ""
	}
	return req.Chain[req.Step], resolvePerson(ctx, q, req.Chain[req.Step], sub, req.Facts).Name
}

func withName(stage, name string) string {
	if name == "" || name == approval.Unassigned {
		return stage
	}
	return name + " (" + stage + ")"
}

// joinProgress counts the mandatory joining details and documents of a candidate's offer.
func joinProgress(fs []onbField, ds []onbDoc, x store.Row, docs map[string]store.Row) (int, int) {
	if x["offer_id"] == nil {
		return 0, 0
	}
	o := store.Row{"type": str(x["type"]), "department": str(x["department"])}
	return progress(fs, ds, o, rowData(store.Row{"data": x["offer_data"]}), docs, "join")
}

// joinedProgress puts the mandatory joining items done and due on the row (as "joining") and returns them.
func joinedProgress(ctx context.Context, q store.Q, x store.Row, fs []onbField, ds []onbDoc) (int, int) {
	if j, ok := x["joining"].(map[string]int); ok {
		return j["done"], j["total"]
	}
	docs, _ := docsOf(ctx, q, "candidate", str(x["id"]))
	done, total := joinProgress(fs, ds, x, docs)
	x["joining"] = map[string]int{"done": done, "total": total}
	return done, total
}

// describe adds what the pipeline shows: the current ID, the column, a status line and who it waits on.
func describe(ctx context.Context, q store.Q, x store.Row, fs []onbField, ds []onbDoc) {
	x["current_id"] = currentID(x)
	b := bucket(x)
	x["bucket"] = b
	status, waiting := "", ""
	if x["stage"] == "Joined" { // joined, with Payroll or done: the joining details and documents
		joinedProgress(ctx, q, x, fs, ds)
	}
	switch b {
	case "Shortlisted", "Interview":
		req, label := str(x["shortlist_request_id"]), "Shortlist approval"
		if b == "Interview" {
			req, label = str(x["interview_request_id"]), "Interview"
		}
		st, who := waitingOn(ctx, q, req)
		if st == "Line Manager" { // the hiring manager named on the shortlist
			st = "hiring manager"
		}
		status, waiting = label, withName(st, who)
	case "Offer":
		switch {
		case x["offer_id"] == nil:
			status, waiting = "Selected. Offer to raise", "HR"
		case x["offer_status"] == "Pending":
			st, who := waitingOn(ctx, q, str(x["offer_request_id"]))
			status, waiting = "Offer approval", withName(st, who)
		case x["offer_status"] == "Rejected" || x["offer_status"] == "Cancelled":
			status, waiting = "Offer not approved. Revise it or close", "HR"
		case x["released_at"] == nil:
			status, waiting = fmt.Sprintf("Offer v%d approved. Release it", max(1, toInt(x["offer_version"]))), "HR"
		case x["query_at"] != nil && x["accepted_at"] == nil:
			status, waiting = fmt.Sprintf("Question about the breakup on v%d. Revise the offer", max(1, toInt(x["offer_version"]))), "HR"
		case x["accepted_at"] == nil:
			status, waiting = fmt.Sprintf("Offer v%d sent", max(1, toInt(x["offer_version"]))), "Candidate"
			if e := str(x["expires_on"]); e != "" {
				status += ", answer due by " + longDate(e)
			}
		default:
			status, waiting = fmt.Sprintf("Offer accepted (%s). Joining on %s", currentID(x), longDate(str(x["doj"]))), "HR"
		}
	case "Waiting":
		if w := waitOf(x["waiting_window"], today()); w != nil {
			status, waiting = waitLabel(w), "HR"
		}
	case "Joined":
		fsd, tot := joinedProgress(ctx, q, x, fs, ds)
		if x["ji_at"] != nil { // a normal employee on the Joining ID until every step is approved (hiring.go)
			switch {
			case x["verify_status"] == "Rejected":
				status, waiting = "Returned by Payroll", "HR"
			case x["verified_at"] != nil:
				status, waiting = "Verified by Payroll. The Employee ID waits for a clear background check", "HR"
			case fsd < tot:
				status, waiting = fmt.Sprintf("On %s, paid from %s. Documents pending, %d of %d mandatory", x["employee_id"], longDate(str(x["joined_on"])), fsd, tot), "HR"
			default:
				status, waiting = fmt.Sprintf("On %s. Joining details complete: send to Payroll", x["employee_id"]), "HR"
			}
			break
		}
		switch {
		case x["verify_status"] == "Rejected":
			status, waiting = "Returned by Payroll", "HR"
		case str(x["employee_id"]) != "":
			status, waiting = str(x["employee_id"])+" created. Send to Payroll", "HR"
		default:
			status, waiting = fmt.Sprintf("Documents pending, %d of %d mandatory", fsd, tot), "HR"
		}
	case "With Payroll":
		st, who := waitingOn(ctx, q, str(x["verify_request_id"]))
		status, waiting = "Payroll verification", withName(st, who)
	case "Done":
		status = "Verified by Payroll"
		if x["approved_at"] != nil {
			status = "Approved: Employee ID " + str(x["employee_id"])
		}
	case "Closed":
		status = str(x["closed_kind"])
		if r := str(x["closed_reason"]); r != "" {
			status += ": " + r
		}
	}
	x["status"], x["waiting"] = status, waiting
}

// ---------- Access ----------

// canSeeCandidates: onboarding roles see the pipeline; approvers (appr.queue) read the candidates whose requests they
// decide. Both within their data scope (location, department, team).
func canSeeCandidates(c *Ctx) bool {
	return c.Role != "Employee" && (c.Level("onb.pipeline") >= 1 || c.Level("onb.raise") >= 1 || c.Level("appr.queue") >= 1)
}

func onbEditor(c *Ctx) bool { return c.Level("onb.raise") >= 2 || c.Level("onb.pipeline") >= 2 }

// loadCandidate finds a candidate by SI, OI or JI and checks the caller's scope.
func (s *Server) loadCandidate(ctx context.Context, q store.Q, c *Ctx, id string, lock bool) (store.Row, error) {
	no, ok := candidateNo(id)
	if !ok { // the candidate's record id (EMP-…, a contract number) or an old Joining ID kept as an alias (hiring.go)
		id = strings.ToUpper(strings.TrimSpace(id))
		x, _ := q.C("candidate").FindOne(ctx, bson.D{{Key: "employee_id", Value: id}})
		if x == nil {
			if e := store.EmployeeByAlias(ctx, q, id); e != "" {
				x, _ = q.C("candidate").FindOne(ctx, bson.D{{Key: "employee_id", Value: e}})
			}
		}
		if x == nil {
			return nil, notFound("Candidate not found")
		}
		no = toInt(x["no"])
	}
	byNo := bson.D{{Key: "no", Value: no}}
	if lock {
		if _, err := q.C("candidate").Lock(ctx, byNo); err != nil {
			return nil, err
		}
	}
	rows, err := onboardingStagesCandidates(ctx, q, byNo, bson.D{{Key: "no", Value: 1}}, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, notFound("Candidate not found")
	}
	x := rows[0]
	if c != nil {
		if !canSeeCandidates(c) {
			return nil, forbidden("")
		}
		if err := candidateAccess(ctx, q, c, str(x["id"])); err != nil {
			return nil, err
		}
	}
	return x, nil
}

// candidateAccess: the candidate is inside the caller's data scope, or the caller is their hiring manager or decides
// one of their requests now (a hiring manager deciding from a role view whose scope does not cover the place).
func candidateAccess(ctx context.Context, q store.Q, c *Ctx, id string) error {
	ok, err := c.InScope(ctx, q, id)
	if err != nil || ok {
		return err
	}
	x, err := onboardingOne(ctx, q, "candidate", id)
	if err != nil || x == nil {
		return notFound("Candidate not found")
	}
	if str(x["manager_id"]) == c.EmployeeID && c.Role != "Employee" {
		return nil
	}
	o, err := q.C("offer").FindOne(ctx, bson.D{{Key: "candidate_id", Value: id}})
	if err != nil {
		return notFound("Candidate not found")
	}
	// Their shortlist, interview and offer requests.
	for _, r := range []any{x["shortlist_request_id"], x["interview_request_id"], onboardingCol(o, "request_id")} {
		if r == nil {
			continue
		}
		if req, err := loadRequest(ctx, q, str(r), false); err == nil && canAct(ctx, q, c, req) {
			return nil
		}
	}
	return forbidden("This candidate is outside your access")
}

// canCandidateFiles: documents of a candidate (owner_id = the SI id) at every stage. Onboarding roles upload; they and
// the approvers of the candidate read them, within scope.
func (s *Server) canCandidateFiles(ctx context.Context, c *Ctx, owner string, write bool) error {
	x, err := onboardingOne(ctx, s.st.DB, "candidate", owner)
	if err != nil || x == nil {
		return notFound("Candidate not found")
	}
	stage := x["stage"]
	if c.Role == "Employee" {
		return forbidden("")
	}
	if write {
		if !onbEditor(c) {
			return forbidden("")
		}
		if stage == "Closed" {
			return bad("This candidate is closed")
		}
	} else if !canSeeCandidates(c) {
		return forbidden("")
	}
	if write {
		return c.MustInScope(ctx, s.st.DB, owner)
	}
	return candidateAccess(ctx, s.st.DB, c, owner)
}

// ---------- Settings ----------

func employeeIDAt(ctx context.Context, q store.Q) string {
	if x, _ := onboardingOne(ctx, q, "onb_setting", 1); x != nil {
		return str(x["employee_id_at"])
	}
	return "documents"
}

func (s *Server) onbSettingsGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.pipeline") < 1 && c.Level("onb.raise") < 1 && c.Level("onb.setup") < 1 {
		return nil, forbidden("")
	}
	return map[string]any{"employeeIdAt": employeeIDAt(r.Context(), s.st.DB), "hiring": loadHireSettings(r.Context(), s.st.DB)}, nil
}

func (s *Server) onbSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		EmployeeIDAt string        `json:"employeeIdAt"`
		Hiring       *hireSettings `json:"hiring"` // the hiring lifecycle's settings (hiring.go)
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if b.Hiring != nil {
		return s.hireSettingsPut(ctx, c, *b.Hiring)
	}
	if b.EmployeeIDAt != "documents" && b.EmployeeIDAt != "joining" {
		return nil, bad("Choose when the Employee ID is created")
	}
	return map[string]any{"employeeIdAt": b.EmployeeIDAt}, s.st.Tx(ctx, func(tx store.Q) error {
		before := employeeIDAt(ctx, tx)
		if _, err := tx.C("onb_setting").Update(ctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "employee_id_at", Value: b.EmployeeIDAt},
			{Key: "updated_by", Value: c.Name}, {Key: "updated_at", Value: tx.Now()}}}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Employee ID is created "+map[string]string{"documents": "when the mandatory joining documents are complete", "joining": "at joining"}[b.EmployeeIDAt],
			"onb_setting", "1", map[string]string{"employeeIdAt": before}, b)
	})
}

// ---------- Stage 1: Shortlist ----------

type expRow struct {
	Company     string `json:"company"`
	Designation string `json:"designation"`
	From        string `json:"from"` // YYYY-MM
	To          string `json:"to"`   // YYYY-MM, blank while still there
}

type shortlistBody struct {
	Data       map[string]any `json:"data"`
	Experience []expRow       `json:"experience"`
}

type shortlist struct {
	d        map[string]string
	job      offerJob
	ctc      float64
	exp      []expRow
	manager  string
	codeMode string
}

var (
	reYM     = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)
	personal = []string{"first", "last", "phone", "email", "dob", "gender", "addr"} // genders: personal.go
)

// canApprove: the hiring manager decides the shortlist and the interview in their Approvals, so they must have a
// role view with approvals: a team of their own (Line Manager) or a mapped role that decides requests.
func canApprove(ctx context.Context, q store.Q, emp string) bool {
	if ok, _ := q.C("employee").Exists(ctx, onboardingStagesReportsTo(emp)); ok {
		return true
	}
	roles, err := onboardingStagesApproverRoles(ctx, q, true)
	if err != nil || len(roles) == 0 {
		return false
	}
	ok, _ := q.C("user_role_map").Exists(ctx, bson.D{{Key: "employee_id", Value: emp}, {Key: "active", Value: true}, {Key: "role_name", Value: bson.D{{Key: "$in", Value: roles}}}})
	return ok
}

// onboardingStagesReportsTo matches the people on someone's team (their reports who have not left).
func onboardingStagesReportsTo(emp string) bson.D {
	return bson.D{{Key: "manager_id", Value: emp}, {Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}}
}

// onboardingStagesApproverRoles are the roles that decide requests (appr.queue at level 2 or more); activeOnly keeps
// only roles switched on.
func onboardingStagesApproverRoles(ctx context.Context, q store.Q, activeOnly bool) ([]any, error) {
	roles, err := q.C("role_right").Distinct(ctx, "role_name", bson.D{{Key: "right_id", Value: "appr.queue"}, {Key: "level", Value: bson.D{{Key: "$gte", Value: 2}}}})
	if err != nil || !activeOnly || len(roles) == 0 {
		return roles, err
	}
	return q.C("role").Distinct(ctx, "name", bson.D{{Key: "name", Value: bson.D{{Key: "$in", Value: roles}}}, {Key: "active", Value: true}})
}

// shortlistProblems checks the shortlist form and resolves its job, place and hiring manager.
func (s *Server) shortlistProblems(ctx context.Context, q store.Q, c *Ctx, b shortlistBody) (shortlist, []string) {
	d := cleanData(b.Data)
	errs := []string{}
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	for _, k := range []string{"first", "last"} {
		if d[k] == "" {
			add("%s is required", map[string]string{"first": "First name", "last": "Last name"}[k])
		} else if len(d[k]) > 60 {
			add("Keep names under 60 characters")
		}
	}
	d["phone"] = strings.ReplaceAll(d["phone"], " ", "")
	switch {
	case d["phone"] == "":
		add("Mobile number is required")
	case !reMobile.MatchString(d["phone"]):
		add("Mobile number must be 10 digits")
	}
	switch {
	case d["email"] == "":
		add("Personal email is required")
	case !reEmail.MatchString(d["email"]):
		add("Personal email looks invalid")
	}
	switch {
	case d["dob"] == "":
		add("Date of birth is required")
	case !isDate(d["dob"]) || d["dob"] >= today():
		add("Date of birth must be a past date")
	case d["dob"] > time.Now().AddDate(-18, 0, 0).Format("2006-01-02"):
		add("The candidate must be at least 18 years old")
	}
	if d["gender"] == "" {
		add("Gender is required")
	} else if !containsStr(genders, d["gender"]) {
		add("Choose a gender from the list")
	}
	sl := shortlist{d: d}
	if v := d["ctc"]; v == "" {
		add("Existing CTC is required (0 for a fresher)")
	} else if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 || f > 1e9 {
		add("Existing CTC must be a yearly amount in rupees")
	} else {
		sl.ctc = f
	}
	if len(b.Experience) > 15 {
		add("Add up to 15 work experience rows")
	}
	month := today()[:7]
	for i, e := range b.Experience {
		e.Company, e.Designation, e.From, e.To = strings.TrimSpace(e.Company), strings.TrimSpace(e.Designation), strings.TrimSpace(e.From), strings.TrimSpace(e.To)
		if e.Company == "" && e.Designation == "" && e.From == "" && e.To == "" {
			continue
		}
		n := i + 1
		switch {
		case e.Company == "" || e.Designation == "":
			add("Work experience row %d: enter the company and the designation", n)
		case len(e.Company) > 100 || len(e.Designation) > 100:
			add("Work experience row %d: keep the company and designation under 100 characters", n)
		case !reYM.MatchString(e.From):
			add("Work experience row %d: enter the month it started", n)
		case e.To != "" && !reYM.MatchString(e.To):
			add("Work experience row %d: the end month looks wrong", n)
		case e.From > month || (e.To != "" && e.To > month):
			add("Work experience row %d: months cannot be in the future", n)
		case e.To != "" && e.To < e.From:
			add("Work experience row %d: it ends before it starts", n)
		default:
			sl.exp = append(sl.exp, e)
		}
	}
	for k, l := range map[string]string{"city": "Work city", "loc": "Reporting location", "dept": "Department", "desig": "Designation"} {
		if d[k] == "" {
			add("%s is required", l)
		}
	}
	// The job and place resolve as on the offer form; the type is asked at the offer.
	jd := map[string]string{"type": "Permanent", "dept": d["dept"], "desig": d["desig"], "city": d["city"], "loc": d["loc"], "rejoinOf": d["rejoinOf"]}
	sl.job = resolveJob(ctx, q, jd, &errs)
	sl.job.Name = strings.TrimSpace(d["first"] + " " + d["last"])
	sl.manager = d["manager"]
	switch {
	case sl.manager == "":
		add("Hiring manager is required")
	case sl.manager == c.EmployeeID:
		add("The hiring manager approves the shortlist and takes the interview, so someone else raises it")
	default:
		if ok, _ := q.C("employee").Exists(ctx, bson.D{{Key: "_id", Value: sl.manager}, {Key: "status", Value: "Active"}}); !ok {
			add("The hiring manager must be an active employee")
		} else if !canApprove(ctx, q, sl.manager) {
			add("Choose a hiring manager who manages a team or holds an approver role, so the shortlist and interview reach them")
		}
	}
	if sl.job.RejoinOf != "" {
		sl.codeMode = d["codeMode"]
		if sl.codeMode == "" {
			settings, _ := onboardingOne(ctx, q, "employee_code_settings", 1)
			sl.codeMode = str(onboardingCol(settings, "rejoin"))
		}
		if sl.codeMode != "same" && sl.codeMode != "new" {
			add("Choose the employee code for the rejoin")
		}
		if open := openRejoin(ctx, q, sl.job.RejoinOf); open != "" {
			add("%s for %s is already open", open, sl.job.RejoinOf)
		}
	}
	if d["phone"] != "" {
		// Someone still in the pipeline (not closed, no Employee ID yet) with this mobile number.
		if x := onboardingStagesOpenCandidate(ctx, q, bson.E{Key: "data.phone", Value: d["phone"]}); x != nil {
			add("%s (%s) is already in the pipeline with this mobile number", x["id"], x["name"])
		}
	}
	return sl, dedupe(errs)
}

// onboardingStagesOpenCandidate is the first candidate (by number) matching cond who is still in the pipeline: not
// closed and without an Employee ID. nil when there is none.
func onboardingStagesOpenCandidate(ctx context.Context, q store.Q, cond bson.E) store.Row {
	// On a Joining ID and not yet approved counts as in the pipeline too (hiring.go).
	x, _ := q.C("candidate").FindOne(ctx, bson.D{cond, {Key: "stage", Value: bson.D{{Key: "$ne", Value: "Closed"}}}, {Key: "approved_at", Value: nil},
		{Key: "$or", Value: bson.A{bson.D{{Key: "employee_id", Value: nil}}, bson.D{{Key: "ji_at", Value: bson.D{{Key: "$ne", Value: nil}}}}}}},
		options.FindOne().SetSort(bson.D{{Key: "no", Value: 1}}))
	return x
}

// openRejoin names an open shortlist, offer or rejoin request for someone who has left, or "".
func openRejoin(ctx context.Context, q store.Q, emp string) string {
	if x := onboardingStagesOpenCandidate(ctx, q, bson.E{Key: "rejoin_of", Value: emp}); x != nil {
		return "Candidate " + str(x["id"])
	}
	// A rejoin offer raised before stages that is still waiting for approval.
	offers, err := q.C("offer").Find(ctx, bson.D{{Key: "rejoin_of", Value: emp}, {Key: "request_id", Value: bson.D{{Key: "$ne", Value: nil}}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil || len(offers) == 0 {
		return ""
	}
	reqIDs := make([]any, 0, len(offers))
	for _, o := range offers {
		reqIDs = append(reqIDs, o["request_id"])
	}
	pending, err := q.C("approval_request").Distinct(ctx, "_id", bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: reqIDs}}}, {Key: "status", Value: "Pending"}})
	if err != nil {
		return ""
	}
	for _, o := range offers {
		for _, p := range pending {
			if p == o["request_id"] {
				return "Rejoin offer " + str(o["id"])
			}
		}
	}
	return ""
}

func (s *Server) shortlistSubject(sl shortlist) *subject {
	m := sl.manager
	return &subject{ID: "SI-new", Name: sl.job.Name, LocationID: sl.job.LocID, Department: sl.job.Dept, Corporate: sl.job.Corporate, ManagerID: &m}
}

// candidateCheck is the shortlist form's live check: what to fix and the approval route.
func (s *Server) candidateCheck(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	var b shortlistBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	sl, errs := s.shortlistProblems(ctx, s.st.DB, c, b)
	out := map[string]any{"errors": errs}
	if sl.job.LocID != "" && sl.job.DeptID != "" && sl.manager != "" {
		if p, err := planFor(ctx, s.st.DB, "shortlist", s.shortlistSubject(sl), approval.Facts{}); err == nil {
			out["plan"] = p
		}
	}
	out["next"] = stageID("SI", int(onboardingStagesPeekSeq(ctx, s.st.DB, "candidate_seq")))
	return out, nil
}

// onboardingStagesPeekSeq is the number a sequence gives next, without taking it: one after the last handed out (the
// counters collection), or the sequence's start while none has been.
func onboardingStagesPeekSeq(ctx context.Context, q store.Q, name string) int64 {
	if x, err := q.C("counters").FindOne(ctx, bson.D{{Key: "_id", Value: name}}); err == nil && x != nil {
		return int64(toInt(x["value"])) + 1
	}
	for _, sq := range store.Sequences() {
		if sq.Name == name {
			return sq.Start
		}
	}
	return 1
}

// candidateCreate saves the shortlist as SI-0001 and sends it for approval (wf shortlist).
func (s *Server) candidateCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	var b shortlistBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	sl, errs := s.shortlistProblems(ctx, s.st.DB, c, b)
	if len(errs) > 0 {
		return nil, badD(fmt.Sprintf("%d thing%s to fix before sending", len(errs), map[bool]string{true: "", false: "s"}[len(errs) == 1]), errs)
	}
	// A shortlist is raised for a place inside the caller's scope (before a number is taken).
	if c.Role != "HR Admin" && !c.Scope.Allows(access.ScopedEmployee{ID: "SI-new", LocationID: sl.job.LocID, Department: sl.job.Dept, ManagerID: sl.manager}) {
		return nil, forbidden("This location is outside your access")
	}
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n, err := store.NextSeq(ctx, tx, "candidate_seq")
		if err != nil {
			return err
		}
		id := stageID("SI", int(n))
		data := map[string]any{}
		for _, k := range personal {
			if v := sl.d[k]; v != "" {
				data[k] = v
			}
		}
		exp := sl.exp
		if exp == nil {
			exp = []expRow{}
		}
		data["experience"] = exp
		if sl.codeMode != "" {
			data["codeMode"] = sl.codeMode
		}
		if _, err := tx.C("candidate").Insert(ctx, bson.D{{Key: "_id", Value: id}, {Key: "no", Value: n}, {Key: "name", Value: sl.job.Name}, {Key: "data", Value: data},
			{Key: "existing_ctc", Value: sl.ctc}, {Key: "department_id", Value: sl.job.DeptID}, {Key: "designation_id", Value: sl.job.DesigID},
			{Key: "location_id", Value: sl.job.LocID}, {Key: "manager_id", Value: sl.manager}, {Key: "rejoin_of", Value: strPtr(sl.job.RejoinOf)},
			{Key: "raised_by", Value: c.EmployeeID}, {Key: "raised_role", Value: c.Role}}); err != nil {
			return err
		}
		req, err := s.createRequestTx(ctx, tx, c, "shortlist", id, approval.Facts{}, map[string]string{"candidateId": id})
		if err != nil {
			return err
		}
		if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{{Key: "$set", Value: bson.D{{Key: "shortlist_request_id", Value: req["id"]}}}}); err != nil {
			return err
		}
		out = map[string]any{"id": id, "request": req}
		if err := lifeEvent(ctx, tx, c, lifeEv{Cand: id, Action: "Shortlisted", To: "Shortlisted", Reason: sl.job.Desig + ", " + sl.job.Loc}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Shortlisted "+sl.job.Name+" as "+id, "candidate", id, nil, map[string]any{"department": sl.job.Dept, "designation": sl.job.Desig,
			"location": sl.job.Loc, "hiringManager": sl.manager, "rejoinOf": sl.job.RejoinOf})
	})
	return out, err
}

// candidateRequestClosed closes the candidate when the shortlist is rejected or withdrawn, or the interview says
// Not selected (called by the engine when a request is rejected or cancelled).
func candidateRequestClosed(ctx context.Context, tx store.Q, c *Ctx, r *Request, status, note string) error {
	kind := ""
	switch {
	case r.WF == "shortlist" && status == "Rejected":
		kind = "Rejected"
	case r.WF == "interview" && status == "Rejected":
		kind = "Not selected"
	case (r.WF == "shortlist" || r.WF == "interview") && status == "Cancelled":
		kind, note = "Withdrawn", "Withdrawn by "+c.Name
	default:
		return nil
	}
	n, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: r.EmployeeID}, {Key: "stage", Value: bson.D{{Key: "$ne", Value: "Closed"}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Closed"}, {Key: "closed_kind", Value: kind}, {Key: "closed_reason", Value: strPtr(note)},
			{Key: "closed_at", Value: tx.Now()}, {Key: "closed_by", Value: c.Name}}}})
	if err != nil || n == 0 {
		return err
	}
	return lifeEvent(ctx, tx, c, lifeEv{Cand: r.EmployeeID, Action: "Closed: " + kind, From: map[string]string{"shortlist": "Shortlisted", "interview": "Interview"}[r.WF], To: "Closed", Reason: note})
}

// sendNotice puts an in-app message in someone's bell (notices.go). ref makes it done once the request is decided.
func sendNotice(ctx context.Context, tx store.Q, to, from, text, link, view string, ref []string) error {
	if to == "" {
		return nil
	}
	if ref == nil {
		ref = []string{}
	}
	_, err := tx.C("notice").Insert(ctx, bson.D{{Key: "to_employee_id", Value: to}, {Key: "from_employee_id", Value: strPtr(from)}, {Key: "kind", Value: "onboarding"},
		{Key: "text", Value: text}, {Key: "link", Value: link}, {Key: "view", Value: view}, {Key: "ref", Value: ref}})
	return err
}

// approverView is the role view in which someone decides requests: Line Manager when they have a team, else a
// mapped role that decides requests.
func approverView(ctx context.Context, q store.Q, emp string) string {
	if ok, _ := q.C("employee").Exists(ctx, onboardingStagesReportsTo(emp)); ok {
		return "Line Manager"
	}
	roles, err := onboardingStagesApproverRoles(ctx, q, false)
	if err != nil || len(roles) == 0 {
		return ""
	}
	// Their first such mapping.
	m, _ := q.C("user_role_map").FindOne(ctx, bson.D{{Key: "employee_id", Value: emp}, {Key: "active", Value: true}, {Key: "role_name", Value: bson.D{{Key: "$in", Value: roles}}}},
		options.FindOne().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	return str(onboardingCol(m, "role_name"))
}

// systemRequestTx raises a request the system starts (the interview after an approved shortlist), as the person who
// raised the candidate.
func (s *Server) systemRequestTx(ctx context.Context, tx store.Q, wf, subjectID, raisedBy string, payload any) (*Request, error) {
	p, err := planChain(ctx, tx, wf, subjectID, approval.Facts{})
	if err != nil {
		return nil, err
	}
	n, err := store.NextSeq(ctx, tx, "approval_request_seq")
	if err != nil {
		return nil, err
	}
	pre := prefixes[wf]
	if pre == "" {
		pre = "RQ"
	}
	id := fmt.Sprintf("%s-%d", pre, n)
	f := approval.Facts{WF: wf}
	if _, err := tx.C("approval_request").Insert(ctx, bson.D{{Key: "_id", Value: id}, {Key: "wf", Value: wf}, {Key: "candidate_id", Value: subjectID},
		{Key: "raised_by", Value: raisedBy}, {Key: "flow", Value: string(p.Flow)}, {Key: "facts", Value: f}, {Key: "payload", Value: jsonOrNil(payload)},
		{Key: "chain", Value: p.Chain}, {Key: "notify", Value: p.Notify}, {Key: "why", Value: p.Why}}); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, nil, "Raised "+strings.ToLower(workflowName(wf))+" request", "approval_request", id, nil, map[string]any{"payload": payload}); err != nil {
		return nil, err
	}
	req, err := loadRequest(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if err := skipVacant(ctx, tx, req); err != nil {
		return nil, err
	}
	if len(req.Chain) == 0 {
		return req, s.autoApprove(ctx, tx, req, p.HierarchyOff)
	}
	return req, nil
}

// finalizeShortlist: the hiring manager and the Function Head approved; the interview now waits on the hiring manager.
func (s *Server) finalizeShortlist(ctx context.Context, tx store.Q, r *Request) (string, error) {
	x, err := tx.C("candidate").FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: r.EmployeeID}, {Key: "stage", Value: "Shortlisted"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Interview"}}}})
	if err == nil && x == nil {
		err = store.ErrNoRows
	}
	if err != nil {
		return "", orNotFound(err)
	}
	name, raisedBy := str(x["name"]), str(x["raised_by"])
	if err := lifeEvent(ctx, tx, nil, lifeEv{Cand: r.EmployeeID, Action: "Shortlist approved", From: "Shortlisted", To: "Interview", Reason: "Approval " + r.ID}); err != nil {
		return "", err
	}
	req, err := s.systemRequestTx(ctx, tx, "interview", r.EmployeeID, raisedBy, map[string]string{"candidateId": r.EmployeeID})
	if err != nil {
		return "", err
	}
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: r.EmployeeID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "interview_request_id", Value: req.ID}}}}); err != nil {
		return "", err
	}
	if req.Status == "Approved" { // the interview flow's approval hierarchy is off: selected at once
		return fmt.Sprintf("Shortlist approved. No interview approval needed (%s): selected, raise the offer.", req.ID), nil
	}
	who := ""
	if req.Status == "Pending" && req.Step < len(req.Chain) {
		sub, err := loadSubject(ctx, tx, r.EmployeeID)
		if err == nil {
			p := resolvePerson(ctx, tx, req.Chain[req.Step], sub, req.Facts)
			who = p.Name
			text := fmt.Sprintf("Interview waiting for you: %s (%s). Record Selected or Not selected in Approvals.", name, r.EmployeeID)
			if err := sendNotice(ctx, tx, p.ID, raisedBy, text, "/approvals?id="+req.ID, approverView(ctx, tx, p.ID), []string{":interview:" + req.ID}); err != nil {
				return "", err
			}
		}
	}
	if who == "" {
		return fmt.Sprintf("Shortlist approved. The interview %s is next.", req.ID), nil
	}
	return fmt.Sprintf("Shortlist approved. Interview with %s (%s).", who, req.ID), nil
}

// finalizeInterview: the hiring manager selected the candidate; the Offer ID is issued and HR raises the offer.
func (s *Server) finalizeInterview(ctx context.Context, tx store.Q, r *Request) (string, error) {
	x, err := tx.C("candidate").FindOne(ctx, bson.D{{Key: "_id", Value: r.EmployeeID}, {Key: "stage", Value: "Interview"}})
	if err == nil && x == nil {
		err = store.ErrNoRows
	}
	if err != nil {
		return "", orNotFound(err)
	}
	// Selected, unless the interview recorded a result already.
	result := x["interview_result"]
	if result == nil {
		result = "Selected"
	}
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: r.EmployeeID}, {Key: "stage", Value: "Interview"}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Offer"}, {Key: "interview_result", Value: result}}}}); err != nil {
		return "", err
	}
	name, raisedBy, role := str(x["name"]), str(x["raised_by"]), str(x["raised_role"])
	// No Offer ID yet: it is issued when the candidate accepts (hiring.go). The offer is a version under SI-.
	text := fmt.Sprintf("%s was selected at the interview (%s). Raise the offer from Onboarding.", name, r.EmployeeID)
	if err := sendNotice(ctx, tx, raisedBy, "", text, "/onboarding?id="+r.EmployeeID, role, nil); err != nil {
		return "", err
	}
	if err := lifeEvent(ctx, tx, nil, lifeEv{Cand: r.EmployeeID, Action: "Selected at the interview", From: "Interview", To: "Selected", Reason: str(x["interview_feedback"])}); err != nil {
		return "", err
	}
	return "Selected. HR raises the offer; the Offer ID is issued when the candidate accepts.", nil
}

// ---------- Stage 2: Interview ----------

// interviewer: only the hiring manager (whoever the interview's stage resolves to) records it, in a role view with
// approvals; nobody approves on their behalf.
func interviewer(ctx context.Context, q store.Q, c *Ctx, r *Request) bool {
	if r.Status != "Pending" || r.Step >= len(r.Chain) || c.Role == "Employee" || c.Level("appr.queue") < 2 {
		return false
	}
	sub, err := loadSubject(ctx, q, r.EmployeeID)
	return err == nil && resolvePerson(ctx, q, r.Chain[r.Step], sub, r.Facts).ID == c.EmployeeID
}

func (s *Server) candidateInterview(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Date     string `json:"date"`
		Result   string `json:"result"`
		Feedback string `json:"feedback"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Feedback = strings.TrimSpace(b.Feedback)
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		x, err := s.loadCandidate(ctx, tx, nil, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		if x["stage"] != "Interview" {
			return bad("%s is not waiting for an interview", x["id"])
		}
		req, err := loadRequest(ctx, tx, str(x["interview_request_id"]), true)
		if err != nil {
			return err
		}
		if !interviewer(ctx, tx, c, req) {
			return forbidden("Only the hiring manager records this interview")
		}
		created, _ := x["created_at"].(time.Time)
		switch {
		case !isDate(b.Date):
			return bad("Enter the interview date")
		case b.Date > today():
			return bad("The interview date cannot be in the future")
		case !created.IsZero() && b.Date < created.Format("2006-01-02"):
			return bad("The interview date cannot be before the shortlist, %s", longDate(created.Format("2006-01-02")))
		case b.Result != "Selected" && b.Result != "Not selected":
			return bad("Choose Selected or Not selected")
		case b.Feedback == "":
			return bad("Add your interview feedback")
		case len(b.Feedback) > 1000:
			return bad("Keep the feedback under 1,000 characters")
		}
		if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: x["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "interview_date", Value: b.Date},
			{Key: "interview_result", Value: b.Result}, {Key: "interview_feedback", Value: b.Feedback}, {Key: "interviewed_by", Value: c.EmployeeID},
			{Key: "interviewed_at", Value: tx.Now()}}}}); err != nil {
			return err
		}
		action := "approve"
		if b.Result == "Not selected" {
			action = "reject"
		}
		if out, err = s.decideTx(ctx, tx, c, req.ID, action, b.Feedback); err != nil {
			return err
		}
		out["result"] = b.Result
		if b.Result == "Selected" {
			out["candidateId"] = x["id"]
		}
		return audit(ctx, tx, c, fmt.Sprintf("Interview of %s (%s): %s", x["name"], x["id"], b.Result), "candidate", str(x["id"]), nil, b)
	})
	return out, err
}

// ---------- Stage 3: Joined, Employee ID ----------

// candidateOffer loads the candidate's offer row as finalizeOffer reads it.
func candidateOffer(ctx context.Context, tx store.Q, cand string) (store.Row, error) {
	o, err := onboardingLockOffer(ctx, tx, bson.D{{Key: "candidate_id", Value: cand}})
	if err != nil || o == nil {
		return nil, orNotFound(err)
	}
	return o, nil
}

// createEmployeeID gives a joined candidate the next Employee ID (or reopens a rejoiner's old one), as offer
// approval did before stages: the record as Pre-boarding, pay, personal, bank and the documents.
func createEmployeeID(ctx context.Context, tx store.Q, c *Ctx, cand string) (string, error) {
	// Locked: two uploads finishing at once never make two Employee IDs.
	x, err := tx.C("candidate").Lock(ctx, bson.D{{Key: "_id", Value: cand}})
	if err != nil {
		return "", err // a lock conflict: the transaction runs again
	}
	if x == nil {
		return "", notFound("Candidate not found")
	}
	if emp := str(x["employee_id"]); emp != "" {
		return emp, nil
	}
	name, joined := str(x["name"]), str(x["joined_on"])
	if x["stage"] != "Joined" || joined == "" {
		return "", bad("The Employee ID is created after the candidate joins")
	}
	o, err := candidateOffer(ctx, tx, cand)
	if err != nil {
		return "", err
	}
	ji := stageID("JI", toInt(x["no"]))
	code, _, err := employeeFromOffer(ctx, tx, o, joined, "Joining "+ji)
	if err != nil {
		return "", err
	}
	if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: cand}}, bson.D{{Key: "$set", Value: bson.D{{Key: "employee_id", Value: code},
		{Key: "employee_created_at", Value: tx.Now()}}}}); err != nil {
		return "", err
	}
	if err := lifeEvent(ctx, tx, c, lifeEv{Cand: cand, Emp: code, Action: "Employee ID created: " + code, From: "Joined (" + ji + ")", To: "Pre-boarding"}); err != nil {
		return "", err
	}
	return code, audit(ctx, tx, c, fmt.Sprintf("Employee ID %s created for %s (%s)", code, name, ji), "candidate", cand, nil, map[string]string{"employeeId": code})
}

// maybeEmployeeID creates the Employee ID of a joined candidate once it is due: at joining, or (the default) when the
// mandatory joining details and documents are complete. It returns the new code, or "".
func (s *Server) maybeEmployeeID(ctx context.Context, tx store.Q, c *Ctx, cand string) (string, error) {
	// Joined on a Joining ID (hiring.go): the Employee ID comes with the last approval, not with the documents.
	if x, err := onboardingOne(ctx, tx, "candidate", cand); err != nil || x == nil || x["employee_id"] != nil || x["stage"] != "Joined" || x["ji_at"] != nil {
		return "", nil
	}
	if employeeIDAt(ctx, tx) != "joining" {
		x, err := s.loadCandidate(ctx, tx, nil, cand, false)
		if err != nil {
			return "", err
		}
		fs, ds, err := loadSetup(ctx, tx)
		if err != nil {
			return "", err
		}
		docs, err := docsOf(ctx, tx, "candidate", cand)
		if err != nil {
			return "", err
		}
		if done, total := joinProgress(fs, ds, x, docs); done < total {
			return "", nil
		}
	}
	return createEmployeeID(ctx, tx, c, cand)
}

// candidateDocAdded runs after a document of a candidate is uploaded (files.go): the last mandatory joining document
// creates the Employee ID. It returns a note for the upload, or "".
func (s *Server) candidateDocAdded(ctx context.Context, c *Ctx, cand string) string {
	var code string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		var err error
		code, err = s.maybeEmployeeID(ctx, tx, c, cand)
		return err
	})
	if err != nil || code == "" {
		return ""
	}
	return "All mandatory joining documents are in. Employee ID " + code + " created."
}

func (s *Server) candidateJoined(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	var b struct {
		Date string `json:"date"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		x, err := s.loadCandidate(ctx, tx, c, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		if x["stage"] != "Offer" || x["accepted_at"] == nil || x["declined_at"] != nil {
			return bad("Mark joined once the candidate has accepted the offer")
		}
		if w := waitOf(x["waiting_window"], today()); w != nil && b.Date < w.Since {
			return bad("They were waited for from %s. Enter the day they actually came", longDate(w.Since))
		}
		released, _ := x["released_at"].(time.Time)
		switch {
		case !isDate(b.Date):
			return bad("Enter the date the candidate joined")
		case b.Date > today():
			return bad("The joining date cannot be in the future. Mark joined on the day they join")
		case !released.IsZero() && b.Date < released.Format("2006-01-02"):
			return bad("The joining date cannot be before the offer was released, %s", longDate(released.Format("2006-01-02")))
		}
		if rj := str(x["rejoin_of"]); rj != "" {
			old, _ := onboardingOne(ctx, tx, "employee", rj)
			if lwd := str(onboardingCol(old, "lwd")); lwd != "" && b.Date <= lwd {
				return bad("A rejoining date must be after the last working day, %s", longDate(lwd))
			}
		}
		id := str(x["id"])
		// From the joining day the person is a normal employee on the Joining ID (hiring.go).
		code, err := s.hireJoin(ctx, tx, c, x, b.Date)
		if err != nil {
			return err
		}
		ji := stageID("JI", toInt(x["no"]))
		out = map[string]any{"id": code, "ji": ji}
		return audit(ctx, tx, c, fmt.Sprintf("%s joined on %s: %s", x["name"], longDate(b.Date), code), "candidate", id, nil, b)
	})
	return out, err
}

// candidateClose: Mark not joined (accepted, never came) or close a candidate whose offer was not raised or not
// approved. Both keep the reason; no Employee ID is used.
func (s *Server) candidateClose(kind string) authedFn {
	return func(r *http.Request, c *Ctx) (any, error) {
		if !onbEditor(c) {
			return nil, forbidden("")
		}
		var b struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		b.Reason = strings.TrimSpace(b.Reason)
		if b.Reason == "" {
			return nil, bad("Give the reason")
		}
		ctx := r.Context()
		return map[string]string{"result": kind}, s.st.Tx(ctx, func(tx store.Q) error {
			x, err := s.loadCandidate(ctx, tx, c, r.PathValue("id"), true)
			if err != nil {
				return err
			}
			if x["stage"] != "Offer" {
				return bad("Only a candidate at the offer stage can be closed here")
			}
			switch kind {
			case "Not joined":
				if x["accepted_at"] == nil || x["declined_at"] != nil {
					return bad("Mark not joined is for a candidate who accepted the offer and did not join")
				}
			default: // Withdrawn
				if st := x["offer_status"]; x["offer_id"] != nil && st != "Rejected" && st != "Cancelled" {
					return bad("The offer is %s. Use Candidate declined, or Mark not joined after acceptance", strings.ToLower(offerStatus(store.Row{"request_status": st,
						"released_at": x["released_at"], "accepted_at": x["accepted_at"], "declined_at": x["declined_at"]})))
				}
			}
			if _, err := tx.C("candidate").Update(ctx, bson.D{{Key: "_id", Value: x["id"]}}, bson.D{{Key: "$set", Value: bson.D{{Key: "stage", Value: "Closed"},
				{Key: "closed_kind", Value: kind}, {Key: "closed_reason", Value: b.Reason}, {Key: "closed_at", Value: tx.Now()}, {Key: "closed_by", Value: c.Name}}}}); err != nil {
				return err
			}
			if err := stopJoinerLink(ctx, tx, str(x["offer_id"]), c.Name+" ("+c.EmployeeID+")", "The candidate was closed: "+kind); err != nil {
				return err
			}
			if err := lifeEvent(ctx, tx, c, lifeEv{Cand: str(x["id"]), Action: "Closed: " + kind, From: hireState(x, nil), To: "Closed", Reason: b.Reason}); err != nil {
				return err
			}
			return audit(ctx, tx, c, fmt.Sprintf("%s (%s) closed: %s", x["name"], currentID(x), kind), "candidate", str(x["id"]), nil, b)
		})
	}
}

// candidateEmployeeID creates a due Employee ID on request (after the setting changed, for example).
func (s *Server) candidateEmployeeID(r *http.Request, c *Ctx) (any, error) {
	if !onbEditor(c) {
		return nil, forbidden("")
	}
	ctx := r.Context()
	var code string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		x, err := s.loadCandidate(ctx, tx, c, r.PathValue("id"), true)
		if err != nil {
			return err
		}
		if x["stage"] != "Joined" {
			return bad("The Employee ID is created after the candidate joins")
		}
		if x["ji_at"] != nil {
			return bad("%s is on %s. The Employee ID is issued when every step is approved: joining details, Payroll verification and the background check", x["name"], x["employee_id"])
		}
		if code = str(x["employee_id"]); code != "" {
			return nil
		}
		if code, err = s.maybeEmployeeID(ctx, tx, c, str(x["id"])); err == nil && code == "" {
			return bad("Complete the mandatory joining details and documents first")
		}
		return err
	})
	return map[string]string{"employeeId": code}, err
}

// ---------- Pipeline and candidate ----------

func (s *Server) candidateList(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.pipeline") < 1 && c.Level("onb.raise") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	where, err := onboardingScope(ctx, s.st.DB, c.Scope)
	if err != nil {
		return nil, err
	}
	rows, err := onboardingStagesCandidates(ctx, s.st.DB, where, bson.D{{Key: "no", Value: -1}}, 1000)
	if err != nil {
		return nil, err
	}
	fs, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	want := r.URL.Query().Get("stage")
	counts := map[string]int{}
	out := []store.Row{}
	for _, x := range rows {
		b := bucket(x)
		counts[b]++
		if want != "" && b != want {
			continue
		}
		describe(ctx, s.st.DB, x, fs, ds)
		for _, k := range []string{"data", "offer_data", "interview_feedback", "waiting_window"} {
			delete(x, k)
		}
		out = append(out, x)
	}
	// The joiner's link, in short (joining_link.go): Link sent, Link opened, Joiner submitted…
	offers := []string{}
	for _, x := range out {
		if id := str(x["offer_id"]); id != "" {
			offers = append(offers, id)
		}
	}
	links := joinerLinkSummaries(ctx, s.st.DB, offers)
	for _, x := range out {
		l := links[str(x["offer_id"])]
		x["joinerLink"] = l
		// While the joiner has the link open, the joining details wait on them (HR still marks the joining).
		if (l == "Link sent" || l == "Link opened") && x["waiting"] == "HR" {
			if x["bucket"] == "Offer" {
				x["waiting"] = "HR to mark joined; the joiner is filling the joining form"
			} else {
				x["waiting"] = "Joiner (filling the joining form)"
			}
		}
	}
	return map[string]any{"rows": out, "counts": counts, "stages": pipelineBuckets}, nil
}

// reqSummary is a request's route with named people and its history, for the candidate view.
func reqSummary(ctx context.Context, q store.Q, id string) store.Row {
	if id == "" {
		return nil
	}
	req, err := loadRequest(ctx, q, id, false)
	if err != nil {
		return nil
	}
	people := []map[string]string{}
	if sub, err := loadSubject(ctx, q, req.EmployeeID); err == nil {
		for _, st := range req.Chain {
			people = append(people, map[string]string{"stage": st, "person": resolvePerson(ctx, q, st, sub, req.Facts).Name})
		}
	}
	hist, _ := q.C("approval_history").Find(ctx, bson.D{{Key: "request_id", Value: id}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "stage", Value: 1}, {Key: "action", Value: 1}, {Key: "by_name", Value: 1}, {Key: "note", Value: 1}, {Key: "at", Value: 1}}))
	return store.Row{"id": req.ID, "wf": req.WF, "status": req.Status, "chain": req.Chain, "step": req.Step, "people": people, "history": hist}
}

func (s *Server) candidateGet(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	x, err := s.loadCandidate(ctx, s.st.DB, c, r.PathValue("id"), false)
	if err != nil {
		return nil, err
	}
	fs, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	describe(ctx, s.st.DB, x, fs, ds)
	id, no := str(x["id"]), toInt(x["no"])
	x["ids"] = map[string]string{"si": id, "oi": str(x["offer_no"]), "ji": stageID("JI", no)}
	// The trail: SI-0001 → OI-0001 (at acceptance) → JI-0001 (at joining) → EMP-1045 (approved), each with when it
	// was reached. A candidate selected before 17 Nov 2026 got the OI- at selection.
	trail := []map[string]any{{"id": id, "label": "Shortlisted", "at": x["created_at"]}}
	if oi := str(x["offer_no"]); oi != "" {
		at := x["accepted_at"]
		if at == nil {
			at = x["interviewed_at"]
		}
		trail = append(trail, map[string]any{"id": oi, "label": "Offer accepted", "at": at})
	}
	if x["joined_on"] != nil {
		jid := stageID("JI", no)
		if e := str(x["employee_id"]); x["ji_at"] != nil && e != "" && !strings.HasPrefix(e, "JI-") && x["approved_at"] == nil {
			jid = e // a contract number, or a rejoiner's old code, from joining
		}
		trail = append(trail, map[string]any{"id": jid, "label": "Joined", "at": x["joined_on"]})
	}
	if e := str(x["employee_id"]); e != "" && (x["ji_at"] == nil || x["approved_at"] != nil) {
		trail = append(trail, map[string]any{"id": e, "label": "Employee ID", "at": x["employee_created_at"]})
	}
	x["trail"] = trail
	x["shortlist"] = reqSummary(ctx, s.st.DB, str(x["shortlist_request_id"]))
	x["interview"] = reqSummary(ctx, s.st.DB, str(x["interview_request_id"]))
	if x["offer_id"] != nil {
		x["offerRequest"] = reqSummary(ctx, s.st.DB, str(x["offer_request_id"]))
		x["verify"] = reqSummary(ctx, s.st.DB, str(x["verify_request_id"]))
	}
	docs, _ := docsOf(ctx, s.st.DB, "candidate", id)
	x["docs"] = docs
	x["employeeIdAt"] = employeeIDAt(ctx, s.st.DB)
	// What the offer form starts from: the shortlist, with today's names of the job and place.
	d := rowData(x)
	pre := map[string]any{"dept": x["department"], "desig": x["designation"], "city": x["city_id"], "loc": x["location_id"], "l1": x["manager_id"]}
	for _, k := range personal {
		if v := d[k]; v != "" {
			pre[k] = v
		}
	}
	if m, ok := x["offer_data"].(map[string]any); ok && x["offer_id"] != nil {
		for k, v := range m { // a revised offer starts from the one sent
			pre[k] = v
		}
	}
	x["prefill"] = pre
	if m, ok := x["data"].(map[string]any); ok {
		x["experience"] = m["experience"]
		x["codeMode"] = m["codeMode"]
	}
	// What the caller can do now.
	edit := onbEditor(c)
	can := map[string]bool{}
	can["raiseOffer"] = edit && c.Level("onb.raise") >= 2 && x["stage"] == "Offer" && (x["offer_id"] == nil || x["offer_status"] == "Rejected" || x["offer_status"] == "Cancelled")
	can["markJoined"] = edit && x["stage"] == "Offer" && x["accepted_at"] != nil && x["declined_at"] == nil
	can["raiseOffer"] = can["raiseOffer"] || (edit && c.Level("onb.raise") >= 2 && x["stage"] == "Offer" && x["query_at"] != nil && x["accepted_at"] == nil && x["declined_at"] == nil)
	can["close"] = edit && x["stage"] == "Offer" && (x["offer_id"] == nil || x["offer_status"] == "Rejected" || x["offer_status"] == "Cancelled")
	can["upload"] = edit && x["stage"] != "Closed"
	if x["stage"] == "Interview" {
		if req, err := loadRequest(ctx, s.st.DB, str(x["interview_request_id"]), false); err == nil {
			can["interview"] = interviewer(ctx, s.st.DB, c, req)
		}
	}
	x["can"] = can
	hireView(ctx, s.st.DB, c, x) // versions, waiting, background check, timeline (hiring.go)
	delete(x, "offer_data")
	return x, nil
}

// candidateManagers lists who can be the hiring manager: active people with a team or an approver role.
func (s *Server) candidateManagers(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.raise") < 1 && c.Level("onb.pipeline") < 1 {
		return nil, forbidden("")
	}
	ctx, q := r.Context(), s.st.DB
	// Who has a team (reports who have not left), and who holds an active approver role (as canApprove).
	managers, err := q.C("employee").Distinct(ctx, "manager_id", bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}})
	if err != nil {
		return nil, err
	}
	ids := []any{}
	for _, m := range managers {
		if m != nil {
			ids = append(ids, m)
		}
	}
	roles, err := onboardingStagesApproverRoles(ctx, q, true)
	if err != nil {
		return nil, err
	}
	if len(roles) > 0 {
		approvers, err := q.C("user_role_map").Distinct(ctx, "employee_id", bson.D{{Key: "active", Value: true}, {Key: "role_name", Value: bson.D{{Key: "$in", Value: roles}}}})
		if err != nil {
			return nil, err
		}
		ids = append(ids, approvers...)
	}
	emps, err := q.C("employee").Find(ctx, bson.D{{Key: "status", Value: "Active"}, {Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(1000))
	if err != nil {
		return nil, err
	}
	var desigIDs, deptIDs, locIDs []string
	for _, e := range emps {
		desigIDs, deptIDs, locIDs = append(desigIDs, str(e["designation_id"])), append(deptIDs, str(e["department_id"])), append(locIDs, str(e["location_id"]))
	}
	desigs, err := onboardingByID(ctx, q, "designation", desigIDs...)
	if err != nil {
		return nil, err
	}
	depts, err := onboardingByID(ctx, q, "department", deptIDs...)
	if err != nil {
		return nil, err
	}
	locs, err := onboardingByID(ctx, q, "location", locIDs...)
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, e := range emps {
		g, d, l := desigs[str(e["designation_id"])], depts[str(e["department_id"])], locs[str(e["location_id"])]
		if g == nil || d == nil || l == nil {
			continue
		}
		out = append(out, store.Row{"id": e["id"], "name": e["name"], "designation": g["name"], "department": d["name"], "location": l["name"]})
	}
	return out, nil
}

// candidateRejoin prefills a shortlist from the record of someone who has left (Rejoin).
func (s *Server) candidateRejoin(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.raise", 2); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	e, err := onboardingLeaver(ctx, s.st.DB, id)
	if err != nil || e == nil {
		return nil, orNotFound(err)
	}
	if e["status"] != "Inactive" {
		return nil, bad("Only someone who has left can rejoin")
	}
	first, last, _ := strings.Cut(str(e["name"]), " ")
	data := map[string]any{"first": first, "last": last, "dept": e["dept"], "desig": e["desig"], "city": e["city"], "loc": e["loc"], "phone": e["mobile"],
		"email": e["email"], "gender": e["gender"], "dob": e["dob"], "addr": e["address"], "rejoinOf": id}
	if e["manager_status"] == "Active" && canApprove(ctx, s.st.DB, str(e["manager_id"])) {
		data["manager"] = e["manager_id"]
	}
	if g, ok := grossFor(ctx, s.st.DB, id, today()[:7]); ok && g > 0 {
		data["ctc"] = calc12(g)
	}
	for k, v := range data {
		if v == nil || v == "" {
			delete(data, k)
		}
	}
	cs, _ := onboardingOne(ctx, s.st.DB, "company_setting", 1)
	settings, _ := onboardingOne(ctx, s.st.DB, "employee_code_settings", 1)
	company, mode := str(onboardingCol(cs, "name")), str(onboardingCol(settings, "rejoin"))
	exp := []expRow{}
	if doj := str(e["doj"]); len(doj) >= 7 {
		to := ""
		if l := str(e["lwd"]); len(l) >= 7 {
			to = l[:7]
		}
		exp = append(exp, expRow{Company: company, Designation: str(e["desig"]), From: doj[:7], To: to})
	}
	rehire, _ := e["rehire"].(string)
	if rehire == "" {
		rehire = "Yes"
	}
	data["codeMode"] = mode
	return map[string]any{"data": data, "experience": exp, "rehire": rehire, "exitReason": e["exit_reason"], "lwd": e["lwd"], "codeDefault": mode,
		"open": openRejoin(ctx, s.st.DB, id)}, nil
}

func calc12(monthly float64) float64 { return float64(int64(monthly*12 + 0.5)) }

// candidateRoutes: the onboarding stages (server.go calls this once).
func (s *Server) candidateRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/candidates", s.a(s.candidateList))
	m.HandleFunc("POST /api/candidates/check", s.a(s.candidateCheck))
	m.HandleFunc("POST /api/candidates", s.a(s.candidateCreate))
	m.HandleFunc("GET /api/candidates/managers", s.a(s.candidateManagers))
	m.HandleFunc("GET /api/candidates/rejoin/{id}", s.a(s.candidateRejoin))
	m.HandleFunc("GET /api/candidates/{id}", s.a(s.candidateGet))
	m.HandleFunc("POST /api/candidates/{id}/interview", s.a(s.candidateInterview))
	m.HandleFunc("POST /api/candidates/{id}/joined", s.a(s.candidateJoined))
	m.HandleFunc("POST /api/candidates/{id}/not-joined", s.a(s.candidateClose("Not joined")))
	m.HandleFunc("POST /api/candidates/{id}/close", s.a(s.candidateClose("Withdrawn")))
	m.HandleFunc("POST /api/candidates/{id}/employee-id", s.a(s.candidateEmployeeID))
	m.HandleFunc("GET /api/onboarding/settings", s.a(s.onbSettingsGet))
	m.HandleFunc("PUT /api/onboarding/settings", s.a(s.onbSettingsPut))
	s.hireRoutes(m) // the hiring lifecycle: joining outcomes, waiting, background check, stage and timeline (hiring.go)
}
