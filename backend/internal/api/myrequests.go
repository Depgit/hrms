package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// My view › Dashboard › My requests. Approvals is not in My view: each request starts from its own page (leave,
// attendance, claims, details, separation) and the employee follows them all here. The list is the requests about
// the caller, whoever raised them; offer approval and joining verification belong to Onboarding and are left out.
//
// The hidden-exit rule below is shared with ess.go, myview.go, approval_routes.go and separation.go. It comes in
// two forms: a Go check on a loaded request or case (hrExitHidden, sepHidden), and a MongoDB filter for queries
// (hrExitHiddenFilter, sepHiddenFilter). The filters match the hidden ones; to leave them out, wrap the filter in
// $nor, which is also how a condition is negated inside a pipeline's $match:
//
//	approval_request filter:  bson.M{"employee_id": id, "$nor": bson.A{hrExitHiddenFilter("")}}
//	request embedded at "r":  bson.M{"$match": bson.M{"$nor": bson.A{hrExitHiddenFilter("r")}}}
//	separation + its request: bson.M{"$match": bson.M{"$nor": bson.A{sepHiddenFilter("", "r")}}}
//
// A prefix is the path of the document the fields belong to: "" for the documents being queried themselves, or the
// field a $lookup (+ $unwind) put the request or case in, written "r" or "r." alike. Combine a filter with other
// conditions on the same fields (status, wf, type) through $and, as its keys would otherwise clash.

// myRequestWFs are the workflows My requests shows: all but the onboarding approvals, which are HR's.
var myRequestWFs = bson.M{"$nin": bson.A{"joining", "verify"}}

// An exit HR starts about someone (termination, absconding, contract end, retirement) is not shown to that person
// until it is approved (owner's decision, 11 Nov 2026): not in My requests or its count, an approvals queue, the
// request itself, the separations list or their Separation page. One rejected or cancelled never is. Their own
// resignation, and later changes to an approved exit, are shown as before.
//
// hrExitHiddenFilter matches such requests: wf resign with an exit type that is set and is not Resignation, a new
// exit (rkind blank or new) and not yet Approved. A missing or null payload field counts as blank.
func hrExitHiddenFilter(prefix string) bson.M {
	p := myrequestsPath(prefix)
	return bson.M{
		p + "wf":               "resign",
		p + "payload.exitType": bson.M{"$nin": bson.A{nil, "", "Resignation"}},
		p + "payload.rkind":    bson.M{"$in": bson.A{nil, "", "new"}},
		p + "status":           bson.M{"$ne": "Approved"},
	}
}

func hrExitHidden(req *Request) bool {
	p := payloadOf(req)
	return req.WF == "resign" && p.ExitType != "" && p.ExitType != "Resignation" && (p.RKind == "" || p.RKind == "new") && req.Status != "Approved"
}

// sepHiddenFilter is the same rule for a separation case (fields at sepPrefix) and its request (fields at
// reqPrefix, absent when the case has none): an exit HR started that is not yet approved, including an absconding
// case still at show-cause (no request yet).
func sepHiddenFilter(sepPrefix, reqPrefix string) bson.M {
	return bson.M{
		myrequestsPath(sepPrefix) + "type":   bson.M{"$ne": "Resignation"},
		myrequestsPath(reqPrefix) + "status": bson.M{"$ne": "Approved"},
	}
}

func sepHidden(sc *sepCase) bool {
	return sc.Type != "Resignation" && (sc.ReqStatus == nil || *sc.ReqStatus != "Approved")
}

// myrequestsPath turns a document prefix ("", "r" or "r.") into the start of a field path.
func myrequestsPath(prefix string) string {
	if prefix == "" || strings.HasSuffix(prefix, ".") {
		return prefix
	}
	return prefix + "."
}

// myrequestsWaitingOn is who a pending request waits on: the chain's entry at the current step (chain->>step), or
// nil when the request is not pending or the step is past the chain.
func myrequestsWaitingOn(r store.Row) any {
	if r["status"] != "Pending" {
		return nil
	}
	chain, _ := r["chain"].([]any)
	step, ok := r["step"].(int32)
	if !ok {
		return nil
	}
	i := int(step)
	if i < 0 { // jsonb counts a negative index from the end
		i += len(chain)
	}
	if i < 0 || i >= len(chain) || chain[i] == nil {
		return nil
	}
	if s, ok := chain[i].(string); ok {
		return s
	}
	b, _ := json.Marshal(chain[i]) // ->> gives other JSON values as their text
	return string(b)
}

// myRequestsAll is GET /api/me/requests?wf=all: every request about the caller, newest first, with who it waits
// on (stage and person), since when, who raised it and whether the caller may cancel it.
func (s *Server) myRequestsAll(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	rows, err := s.st.DB.C("approval_request").Find(ctx,
		bson.M{"employee_id": c.EmployeeID, "wf": myRequestWFs, "$nor": bson.A{hrExitHiddenFilter("")}},
		options.Find().
			SetProjection(bson.M{"wf": 1, "facts": 1, "payload": 1, "chain": 1, "step": 1, "status": 1, "created_at": 1, "stage_since": 1, "raised_by": 1}).
			SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).
			SetLimit(300))
	if err != nil {
		return nil, err
	}
	// raised_by_name: the raiser's name, or their id when there is no such employee (the old LEFT JOIN).
	raisers := []string{}
	for _, row := range rows {
		raisers = append(raisers, str(row["raised_by"]))
	}
	names, err := s.st.DB.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": raisers}}, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	nameOf := map[string]any{}
	for _, e := range names {
		nameOf[str(e["id"])] = e["name"]
	}
	for _, row := range rows {
		row["raised_by_name"] = row["raised_by"]
		if n, ok := nameOf[str(row["raised_by"])]; ok && n != nil {
			row["raised_by_name"] = n
		}
		row["waiting_on"] = myrequestsWaitingOn(row)
	}
	apprUIWaitingNames(ctx, s.st.DB, rows)
	for _, row := range rows {
		req := &Request{ID: str(row["id"]), WF: str(row["wf"]), EmployeeID: c.EmployeeID, RaisedBy: str(row["raised_by"]), Status: str(row["status"])}
		req.Payload, _ = json.Marshal(row["payload"])
		row["canCancel"] = req.Status == "Pending" && mayCancel(c, req)
	}
	return rows, nil
}

// myPendingCount is the number behind "My requests waiting" on the My view Dashboard: the same requests as the list.
func myPendingCount(ctx context.Context, q store.Q, emp string) int64 {
	n, _ := q.C("approval_request").Count(ctx, bson.M{"employee_id": emp, "status": "Pending", "wf": myRequestWFs, "$nor": bson.A{hrExitHiddenFilter("")}})
	return n
}

// subjectMayCancel reports whether the employee a request is about may cancel it although someone else raised it:
// only the kinds they raise for themselves (leave, regularization, a claim, a change of their details, their own
// resignation or its withdrawal, an advance they asked for themselves). An advance Payroll or HR raised, a
// deduction, arrear, full and final, transfer, an exit HR started or a change of last working day is cancelled
// only by whoever raised it, or by HR Admin.
func subjectMayCancel(req *Request) bool {
	switch req.WF {
	case "leave", "regularize", "p_claim", "p_change", "p_ack":
		return true
	case "p_adv":
		return req.RaisedBy == req.EmployeeID
	case "resign":
		p := payloadOf(req)
		return (p.ExitType == "" || p.ExitType == "Resignation") && (p.RKind == "" || p.RKind == "new" || p.RKind == "withdraw")
	}
	return false
}

// mayCancel: the person who raised a request, the employee it is about for their own kinds (subjectMayCancel), or
// HR Admin for anyone else's. HR Admin follows the employee's rule for a request about themselves.
func mayCancel(c *Ctx, req *Request) bool {
	if req.RaisedBy == c.EmployeeID {
		return true
	}
	if req.EmployeeID == c.EmployeeID {
		return subjectMayCancel(req)
	}
	return c.Role == "HR Admin"
}

// raisedByOther is the refusal when an employee tries to cancel a request about them that someone else raised.
func raisedByOther(ctx context.Context, q store.Q, by string) string {
	name := by
	if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": by}, options.FindOne().SetProjection(bson.M{"name": 1})); e != nil {
		if n, ok := e["name"].(string); ok {
			name = n
		}
	}
	return "This request was raised by " + name + ", so only they or HR Admin can cancel it"
}
