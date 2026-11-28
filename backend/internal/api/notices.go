package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// In-app messages (notice). Payroll sends them from the payroll run checklist to whoever a pending item waits on:
// the employee to raise it, or the approver to decide it (payroll_closure.go). Everyone reads their own, in any
// role view: the bell in the header counts the unread ones, and the Dashboard shows the open ones with Open and
// Dismiss. A message is done once every item it is about is resolved (raised, approved or rejected, or its
// closure closed); that is worked out when the messages are read and when the checklist loads.

// noticeList is the caller's own messages: every open one, and the last 30 days of the rest.
func (s *Server) noticeList(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	s.resolveNotices(ctx, c.EmployeeID)
	rows, err := noticeRows(ctx, s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	unread, open := 0, 0
	for _, n := range rows {
		if n["done"] == false && n["dismissed"] == false {
			open++
			if n["read"] == false {
				unread++
			}
		}
	}
	return map[string]any{"notices": rows, "unread": unread, "open": open}, nil
}

// noticeRows are the 50 messages the list shows: open ones first, then newest first.
func noticeRows(ctx context.Context, q store.Q, to string) ([]store.Row, error) {
	open := bson.D{{Key: "done_at", Value: nil}, {Key: "dismissed_at", Value: nil}}
	since := time.Now().In(store.Loc()).AddDate(0, 0, -30)
	list, err := q.C("notice").Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "to_employee_id", Value: to},
			{Key: "$or", Value: bson.A{open, bson.D{{Key: "created_at", Value: bson.D{{Key: "$gt", Value: since}}}}}}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "_open", Value: bson.D{{Key: "$and", Value: bson.A{
			bson.D{{Key: "$eq", Value: bson.A{"$done_at", nil}}}, bson.D{{Key: "$eq", Value: bson.A{"$dismissed_at", nil}}}}}}}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "_open", Value: -1}, {Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}}},
		bson.D{{Key: "$limit", Value: 50}},
	})
	if err != nil {
		return nil, err
	}
	from, err := ctxIndex(ctx, q, "employee", "_id", ctxValues(list, "from_employee_id"))
	if err != nil {
		return nil, err
	}
	out := make([]store.Row, len(list))
	for i, n := range list {
		at, _ := n["created_at"].(time.Time)
		out[i] = store.Row{"id": n["id"], "kind": n["kind"], "text": n["text"], "link": str(n["link"]), "view": str(n["view"]),
			"from_name": str(from[str(n["from_employee_id"])]["name"]), "at": at.In(store.Loc()).Format("02 Jan 2006 15:04"),
			"read": n["read_at"] != nil, "done": n["done_at"] != nil, "dismissed": n["dismissed_at"] != nil}
	}
	return out, nil
}

// noticeMark marks one of the caller's messages read, or dismisses it from the Dashboard.
func (s *Server) noticeMark(action string) authedFn {
	return func(r *http.Request, c *Ctx) (any, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, notFound("Message not found")
		}
		ctx, notices := r.Context(), s.st.DB.C("notice")
		mine := bson.M{"_id": id, "to_employee_id": c.EmployeeID}
		if ok, err := notices.Exists(ctx, mine); err != nil {
			return nil, err
		} else if !ok {
			return nil, notFound("Message not found")
		}
		// The first read (and dismissal) keeps its time.
		now := time.Now()
		stamps := []string{"read_at"}
		if action == "dismiss" {
			stamps = append(stamps, "dismissed_at")
		}
		for _, f := range stamps {
			if _, err := notices.Update(ctx, bson.M{"_id": id, "to_employee_id": c.EmployeeID, f: nil}, bson.M{"$set": bson.M{f: now}}); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true}, nil
	}
}

// noticeReadAll marks all the caller's messages read (the bell's "Mark all read").
func (s *Server) noticeReadAll(r *http.Request, c *Ctx) (any, error) {
	_, err := s.st.DB.C("notice").UpdateMany(r.Context(), bson.M{"to_employee_id": c.EmployeeID, "read_at": nil}, bson.M{"$set": bson.M{"read_at": time.Now()}})
	return map[string]any{"ok": true}, err
}

// resolveNotices marks done the caller's open messages whose items are all resolved.
func (s *Server) resolveNotices(ctx context.Context, to string) {
	notices := s.st.DB.C("notice")
	list, err := notices.Find(ctx, bson.M{"to_employee_id": to, "done_at": nil, "dismissed_at": nil, "ref.0": bson.M{"$exists": true}})
	if err != nil {
		return
	}
	done := []any{}
	for _, x := range list {
		all := true
		for _, ref := range strs(x["ref"]) {
			if !refResolved(ctx, s.st.DB, ref, to, str(x["kind"])) {
				all = false
				break
			}
		}
		if all {
			done = append(done, x["id"])
		}
	}
	if len(done) > 0 {
		_, _ = notices.UpdateMany(ctx, bson.M{"_id": bson.M{"$in": done}, "done_at": nil}, bson.M{"$set": bson.M{"done_at": time.Now()}})
	}
}

// refResolved: the checklist item ref ("<ym>:<closure>:<item>") no longer waits on the message's recipient.
func refResolved(ctx context.Context, q store.Q, ref, to, kind string) bool {
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		return true
	}
	ym, cl, key := parts[0], parts[1], parts[2]
	if closed, _ := q.C("payroll_closure").Exists(ctx, bson.M{"ym": ym, "closure": cl, "closed_at": bson.M{"$ne": nil}}); closed && cl != "bank" {
		return true
	}
	emp, rest, day := strings.Cut(key, "/")
	if !day { // a request: resolved once decided, or (for the approver asked) once it has moved on to someone else
		req, err := loadRequest(ctx, q, key, false)
		if err != nil || req.Status != "Pending" || req.Step >= len(req.Chain) {
			return true
		}
		if kind != "payroll" {
			return false
		}
		sub, err := loadSubject(ctx, q, req.EmployeeID)
		if err != nil {
			return true
		}
		return resolvePerson(ctx, q, req.Chain[req.Step], sub, req.Facts).ID != to
	}
	// Any of these on record resolves the item.
	type check struct {
		coll   string
		filter bson.M
	}
	leaveOn := func(day string) check { // a leave request covering the day, pending or approved
		return check{"approval_request", bson.M{"wf": "leave", "employee_id": emp, "status": bson.M{"$in": bson.A{"Pending", "Approved"}},
			"payload.from": bson.M{"$lte": day}, "payload.to": bson.M{"$gte": day}, "payload.short": bson.M{"$ne": true}}}
	}
	var checks []check
	switch {
	case rest == "bank": // an account in use for the month, or a new one waiting or approved
		checks = []check{{"employee_bank", bson.M{"employee_id": emp, "from_month": bson.M{"$lte": ym}}},
			{"approval_request", bson.M{"wf": "p_change", "employee_id": emp, "payload.kind": "Bank", "status": bson.M{"$in": bson.A{"Pending", "Approved"}}}}}
	case cl == "attendance": // the day regularized (even if rejected), on leave, or punched in and out
		checks = []check{{"approval_request", bson.M{"wf": "regularize", "employee_id": emp, "payload.date": rest, "status": bson.M{"$in": bson.A{"Pending", "Approved", "Rejected"}}}},
			leaveOn(rest),
			{"attendance_day", bson.M{"employee_id": emp, "date": rest, "in_time": bson.M{"$ne": nil}, "out_time": bson.M{"$ne": nil}}}}
	default: // leave
		checks = []check{leaveOn(rest)}
	}
	for _, x := range checks {
		if ok, _ := q.C(x.coll).Exists(ctx, x.filter); ok {
			return true
		}
	}
	return false
}
