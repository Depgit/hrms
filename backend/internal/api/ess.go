package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Employee self-service ("My pay and profile").

var exitReasons = []string{"Better opportunity", "Higher studies", "Relocation", "Personal or health", "Compensation", "Other"}
var ecRelations = []string{"Spouse", "Father", "Mother", "Son", "Daughter", "Brother", "Sister", "Friend", "Other relative"}

type notice struct {
	Days        int    `json:"noticeDays"`
	InProbation bool   `json:"inProbation"`
	LWD         string `json:"lwd"`
	CanResign   bool   `json:"canResign"`
	Why         string `json:"why,omitempty"`
	ExitStatus  string `json:"exitStatus,omitempty"`
	CurrentLWD  string `json:"currentLwd,omitempty"`
	Policy      string `json:"policy,omitempty"` // the work policy the notice comes from
}

// noticeFor works out the notice period and whether the employee can start a resignation today.
func noticeFor(ctx context.Context, q store.Q, employeeID string) (notice, error) {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": employeeID}, options.FindOne().SetProjection(bson.M{"status": 1, "exit_status": 1, "lwd": 1}))
	if err != nil || e == nil {
		return notice{}, notFound("Employee not found")
	}
	status := str(e["status"])
	n := notice{CanResign: true}
	days, inProb, err := noticeDaysFor(ctx, q, employeeID)
	if err != nil {
		return n, err
	}
	n.Days, n.InProbation = days, inProb
	n.Policy = sepPolicyName(ctx, q, employeeID)
	n.LWD = time.Now().AddDate(0, 0, n.Days).Format("2006-01-02")
	n.ExitStatus = str(e["exit_status"])
	n.CurrentLWD = str(e["lwd"])
	waiting := bson.M{"wf": "resign", "employee_id": employeeID, "status": "Pending"}
	pending, err := q.C("approval_request").Exists(ctx, bson.M{"$and": bson.A{waiting, bson.M{"$nor": bson.A{hrExitHiddenFilter("")}}}})
	if err != nil {
		return n, err
	}
	// An exit HR started is not shown to the person until approved (myrequests.go), so this does not name it.
	hrExit, err := q.C("approval_request").Exists(ctx, bson.M{"$and": bson.A{waiting, hrExitHiddenFilter("")}})
	if err != nil {
		return n, err
	}
	switch {
	case status != "Active":
		n.CanResign, n.Why = false, "Only active employees can resign"
	case n.ExitStatus == "On notice":
		n.CanResign, n.Why = false, "Your resignation is already accepted. Last working day "+n.CurrentLWD
	case pending:
		n.CanResign, n.Why = false, "A resignation is already waiting for approval"
	case hrExit:
		n.CanResign, n.Why = false, "Your resignation cannot be sent right now. Please speak to HR"
	}
	return n, nil
}

func checkPersonalChange(ctx context.Context, q store.Q, employeeID string, p *requestPayload) error {
	p.Mobile, p.Email, p.ECMobile = strings.TrimSpace(p.Mobile), strings.TrimSpace(p.Email), strings.TrimSpace(p.ECMobile)
	if p.Mobile == "" && p.Email == "" && strings.TrimSpace(p.Address) == "" && p.ECName == "" && p.ECMobile == "" {
		return bad("Change at least one detail")
	}
	if p.Mobile != "" && !reMobile.MatchString(p.Mobile) {
		return bad("Mobile must be 10 digits")
	}
	if p.Email != "" && !reEmail.MatchString(p.Email) {
		return bad("Email is not valid")
	}
	if p.ECName != "" || p.ECMobile != "" || p.ECRelation != "" {
		if len(strings.TrimSpace(p.ECName)) < 2 {
			return bad("Enter the emergency contact name")
		}
		if !containsStr(ecRelations, p.ECRelation) {
			return bad("Choose the relation of the emergency contact")
		}
		if !reMobile.MatchString(p.ECMobile) {
			return bad("Emergency contact mobile must be 10 digits")
		}
		var own string
		if p, _ := q.C("employee_personal").FindOne(ctx, bson.M{"employee_id": employeeID}, options.FindOne().SetProjection(bson.M{"mobile": 1})); p != nil {
			own = str(p["mobile"])
		}
		if p.ECMobile == own || p.ECMobile == p.Mobile {
			return bad("The emergency contact must have a different number from yours")
		}
	}
	return nil
}

// myRequests lists the caller's own requests of one workflow, newest first.
func (s *Server) myRequests(r *http.Request, c *Ctx) (any, error) {
	wf := r.URL.Query().Get("wf")
	if wf == "all" { // My view › Dashboard › My requests (myrequests.go)
		return s.myRequestsAll(r, c)
	}
	if !workflowKeys[wf] {
		return nil, bad("Unknown workflow")
	}
	rows, err := s.st.DB.C("approval_request").Find(r.Context(),
		bson.M{"employee_id": c.EmployeeID, "wf": wf, "$nor": bson.A{hrExitHiddenFilter("")}},
		options.Find().
			SetProjection(bson.M{"wf": 1, "facts": 1, "payload": 1, "chain": 1, "step": 1, "status": 1, "created_at": 1}).
			SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		row["waiting_on"] = myrequestsWaitingOn(row)
	}
	apprUIWaitingNames(r.Context(), s.st.DB, rows)
	return rows, nil
}

// mySeparation gives the notice period for the caller, or for an employee in scope when HR raises it.
func (s *Server) mySeparation(r *http.Request, c *Ctx) (any, error) {
	id := c.EmployeeID
	if v := r.URL.Query().Get("employeeId"); v != "" && v != id {
		if c.Role != "HR Admin" && c.Role != "HRBP" && c.Role != "HRBP Corporate" {
			return nil, forbidden("A resignation is raised by the employee or by HR")
		}
		if err := c.MustInScope(r.Context(), s.st.DB, v); err != nil {
			return nil, err
		}
		id = v
	}
	n, err := noticeFor(r.Context(), s.st.DB, id)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"notice": n, "reasons": exitReasonsFor(c.Role)}
	if r.URL.Query().Get("employeeId") == "" {
		out["case"] = s.mySepCase(r.Context(), c)
	}
	return out, nil
}

func (s *Server) lookups(r *http.Request, c *Ctx) (any, error) {
	return map[string]any{"exitReasons": exitReasonsFor(c.Role), "ecRelations": ecRelations}, nil
}
