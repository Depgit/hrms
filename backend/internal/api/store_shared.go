package api

// Shared database helpers (owned by the store foundation): the old approval_subject VIEW as Go.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// The approval_subject VIEW (migrations 003 and 028) said who a request is about: an employee, an offer (the
// candidate whose offer is being approved) or a shortlisted candidate. Its columns, in order:
var approvalSubjectColumns = []string{"id", "name", "type", "grade", "location_id", "department_id", "manager_id", "functional_manager_id", "status"}

// The view's three branches as $project stages over employee, offer and candidate documents.
var (
	approvalSubjectEmployee = bson.D{{Key: "_id", Value: 0}, {Key: "id", Value: "$_id"}, {Key: "name", Value: 1}, {Key: "type", Value: 1},
		{Key: "grade", Value: 1}, {Key: "location_id", Value: 1}, {Key: "department_id", Value: 1}, {Key: "manager_id", Value: 1},
		{Key: "functional_manager_id", Value: 1}, {Key: "status", Value: 1}}
	approvalSubjectOffer = bson.D{{Key: "_id", Value: 0}, {Key: "id", Value: "$_id"}, {Key: "name", Value: "$candidate_name"}, {Key: "type", Value: 1},
		{Key: "grade", Value: 1}, {Key: "location_id", Value: 1}, {Key: "department_id", Value: 1}, {Key: "manager_id", Value: 1},
		{Key: "functional_manager_id", Value: 1}, {Key: "status", Value: bson.D{{Key: "$literal", Value: "Candidate"}}}}
	approvalSubjectCandidate = bson.D{{Key: "_id", Value: 0}, {Key: "id", Value: "$_id"}, {Key: "name", Value: 1}, {Key: "type", Value: bson.D{{Key: "$literal", Value: nil}}},
		{Key: "grade", Value: bson.D{{Key: "$literal", Value: ""}}}, {Key: "location_id", Value: 1}, {Key: "department_id", Value: 1}, {Key: "manager_id", Value: 1},
		{Key: "functional_manager_id", Value: bson.D{{Key: "$literal", Value: nil}}}, {Key: "status", Value: bson.D{{Key: "$literal", Value: "Candidate"}}}}
)

// approvalSubjectID is COALESCE(r.employee_id, r.offer_id, r.candidate_id) of an approval_request row: the id the
// view was joined on.
func approvalSubjectID(r store.Row) string {
	for _, k := range []string{"employee_id", "offer_id", "candidate_id"} {
		if s, ok := r[k].(string); ok {
			return s
		}
	}
	return ""
}

// approvalSubjectsOf returns the view's rows for these ids, keyed by id (an id with no employee, offer or candidate
// is absent). Each Row has exactly the view's columns. Up to three queries (employee, then offer, then candidate,
// for the ids still missing): $unionWith is not allowed inside a transaction.
func approvalSubjectsOf(ctx context.Context, q store.Q, ids []string) (map[string]store.Row, error) {
	out := map[string]store.Row{}
	left := ids
	for _, b := range []struct {
		coll string
		proj bson.D
	}{{"employee", approvalSubjectEmployee}, {"offer", approvalSubjectOffer}, {"candidate", approvalSubjectCandidate}} {
		if len(left) == 0 {
			break
		}
		rows, err := q.C(b.coll).Aggregate(ctx, bson.A{
			bson.D{{Key: "$match", Value: bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: left}}}}}},
			bson.D{{Key: "$project", Value: b.proj}},
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if id, _ := r["id"].(string); id != "" {
				if _, seen := out[id]; !seen { // UNION ALL order: employee, then offer, then candidate
					out[id] = r
				}
			}
		}
		var rest []string
		for _, id := range left {
			if _, ok := out[id]; !ok {
				rest = append(rest, id)
			}
		}
		left = rest
	}
	return out, nil
}

// approvalSubjectOf is SELECT * FROM approval_subject WHERE id=$1: the subject's Row, or nil when there is none.
func approvalSubjectOf(ctx context.Context, q store.Q, id string) (store.Row, error) {
	m, err := approvalSubjectsOf(ctx, q, []string{id})
	if err != nil {
		return nil, err
	}
	return m[id], nil
}

// approvalSubjectStages are aggregation stages for a pipeline over approval_request that add the request's subject
// (the view's columns, with `id`) as the embedded document `as`, and — like the old
// JOIN approval_subject e ON e.id=COALESCE(r.employee_id, r.offer_id, r.candidate_id) — drop requests without one.
func approvalSubjectStages(as string) []bson.D {
	lookup := func(from, tmp string, proj bson.D) bson.D {
		return bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: from}, {Key: "localField", Value: "_subject_id"},
			{Key: "foreignField", Value: "_id"}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$project", Value: proj}}}}, {Key: "as", Value: tmp}}}}
	}
	return []bson.D{
		{{Key: "$set", Value: bson.D{{Key: "_subject_id", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$employee_id", "$offer_id", "$candidate_id"}}}}}}},
		lookup("employee", "_subject_e", approvalSubjectEmployee),
		lookup("offer", "_subject_o", approvalSubjectOffer),
		lookup("candidate", "_subject_c", approvalSubjectCandidate),
		{{Key: "$set", Value: bson.D{{Key: as, Value: bson.D{{Key: "$first", Value: bson.D{{Key: "$concatArrays", Value: bson.A{"$_subject_e", "$_subject_o", "$_subject_c"}}}}}}}}},
		{{Key: "$match", Value: bson.D{{Key: as, Value: bson.D{{Key: "$exists", Value: true}}}}}},
		{{Key: "$unset", Value: bson.A{"_subject_id", "_subject_e", "_subject_o", "_subject_c"}}},
	}
}

// approvalSubjectDeptStages is approvalSubjectStages plus the subject's department (the whole department document,
// `_id` included) as `deptAs`, dropping requests whose subject has no department, like
// … JOIN department d ON d.id=e.department_id.
func approvalSubjectDeptStages(as, deptAs string) []bson.D {
	return append(approvalSubjectStages(as),
		bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "department"}, {Key: "localField", Value: as + ".department_id"},
			{Key: "foreignField", Value: "_id"}, {Key: "as", Value: deptAs}}}},
		bson.D{{Key: "$unwind", Value: "$" + deptAs}},
	)
}
