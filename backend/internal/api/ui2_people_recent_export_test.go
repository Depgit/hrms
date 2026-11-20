package api

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

// UI2PeopleRecent is the dashboard's scoped "Recent activity" (tests).
func UI2PeopleRecent(ctx context.Context, q store.Q, sc access.Scope) ([]store.Row, error) {
	return ui2PeopleRecent(ctx, q, sc)
}

// UI2PeopleRecentJoined is the same list worked out the slow, literal way — every audit row of the window joined to
// its employee or request subject, as the two EXISTS of the old SQL read — kept as the reference the fast version is
// compared with (tests).
func UI2PeopleRecentJoined(ctx context.Context, q store.Q, sc access.Scope) ([]store.Row, error) {
	empScope, err := employeesScopeAt(ctx, q, sc, "", "_id")
	if err != nil {
		return nil, err
	}
	subjScope, err := employeesScopeAt(ctx, q, sc, "e.", "id")
	if err != nil {
		return nil, err
	}
	inScope := func(from, as string, stages []bson.D, scope bson.M) bson.D {
		p := bson.A{}
		for _, st := range stages {
			p = append(p, st)
		}
		p = append(p, bson.D{{Key: "$match", Value: scope}}, bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 1}}}})
		return bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: from}, {Key: "localField", Value: "entity_id"}, {Key: "foreignField", Value: "_id"},
			{Key: "pipeline", Value: p}, {Key: "as", Value: as}}}}
	}
	return q.C("audit_log").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "at", Value: bson.D{{Key: "$gt", Value: time.Now().AddDate(0, 0, -180)}}},
			{Key: "entity", Value: bson.D{{Key: "$in", Value: bson.A{"employee", "approval_request"}}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: -1}}}},
		inScope("employee", "_emp", []bson.D{employeesLookup("department", "department_id", "d"), employeesUnwind("d")}, empScope),
		inScope("approval_request", "_req", approvalSubjectDeptStages("e", "d"), subjScope),
		bson.D{{Key: "$match", Value: bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "entity", Value: "employee"}, {Key: "_emp", Value: bson.D{{Key: "$ne", Value: bson.A{}}}}},
			bson.D{{Key: "entity", Value: "approval_request"}, {Key: "_req", Value: bson.D{{Key: "$ne", Value: bson.A{}}}}},
		}}}}},
		bson.D{{Key: "$limit", Value: 8}},
		bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 0}, {Key: "action", Value: 1}, {Key: "actor", Value: 1}, {Key: "at", Value: 1}}}},
	})
}
