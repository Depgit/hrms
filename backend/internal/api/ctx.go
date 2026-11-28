package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

// Ctx is the signed-in caller: who they are, the role view they chose, and what that role allows.
type Ctx struct {
	EmployeeID  string            `json:"employeeId"`
	Name        string            `json:"name"`
	Roles       []string          `json:"roles"`
	Role        string            `json:"role"`
	Scope       access.Scope      `json:"-"`
	Access      string            `json:"access"`
	Rights      map[string]int    `json:"rights"`
	Sensitive   map[string]string `json:"sensitive"`
	CanDownload bool              `json:"canDownload"`
	// SkipReason is HR's reason to skip approval of the request this call raises (?skipApproval=…, approvals.go
	// skipApprovalTx); Skip false sends it to the approvers as usual.
	Skip       bool   `json:"-"`
	SkipReason string `json:"-"`
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func strs(v any) []string {
	out := []string{}
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// loadCtx builds the caller's context. Everyone is an Employee; Line Manager is derived
// from having reports; other roles come from the role mapping. The view is chosen by wantRole.
func loadCtx(ctx context.Context, q store.Q, employeeID, wantRole string) (*Ctx, error) {
	emp, err := q.C("employee").FindOne(ctx, bson.M{"_id": employeeID})
	if err != nil {
		return nil, err
	}
	if emp == nil {
		return nil, unauthorized("Sign in again")
	}
	maps, err := ctxRoleMaps(ctx, q, employeeID)
	if err != nil {
		return nil, err
	}
	isMgr, err := q.C("employee").Exists(ctx, bson.M{"manager_id": employeeID, "status": bson.M{"$ne": "Inactive"}})
	if err != nil {
		return nil, err
	}
	roles := []string{"Employee"}
	if emp["status"] != "Inactive" { // someone who has left reaches only their own self-service
		if isMgr {
			roles = append(roles, "Line Manager")
		}
		for _, m := range maps {
			roles = append(roles, m["role_name"].(string))
		}
	}
	c := &Ctx{EmployeeID: employeeID, Name: emp["name"].(string), Roles: roles, Role: "Employee", Rights: map[string]int{}, Sensitive: map[string]string{}}
	for _, r := range roles {
		if r == wantRole {
			c.Role = r
		}
	}
	c.Scope = access.Scope{Access: "self", EmployeeID: employeeID}
	switch c.Role {
	case "Employee":
	case "Line Manager":
		// Two levels (owner, 27 Nov 2026): the direct reports and their reports. ?team=direct narrows to the first
		// (authenticate).
		c.Scope.Access = "team"
		direct, err := q.C("employee").Distinct(ctx, "_id", bson.M{"manager_id": employeeID, "status": bson.M{"$ne": "Inactive"}})
		if err != nil {
			return nil, err
		}
		c.Scope.Managers = []string{employeeID}
		for _, d := range direct {
			if id, ok := d.(string); ok && id != employeeID {
				c.Scope.Managers = append(c.Scope.Managers, id)
			}
		}
	default:
		for _, m := range maps {
			if m["role_name"] == c.Role {
				// A mapping by clusters, cities or states covers the locations inside them as they are now.
				acc, locs, err := mapLocations(ctx, q, m)
				if err != nil {
					return nil, err
				}
				c.Scope.Access, c.Scope.Locations = acc, locs
				c.Scope.Departments = strs(m["departments"])
			}
		}
	}
	c.Access = c.Scope.Access
	rights, err := q.C("role_right").Find(ctx, bson.M{"role_name": c.Role})
	if err != nil {
		return nil, err
	}
	for _, r := range rights {
		c.Rights[r["right_id"].(string)] = int(r["level"].(int16))
	}
	if err := applyPersonalRights(ctx, q, c); err != nil { // personal grants and blocks; HR Admin has every right
		return nil, err
	}
	for k, v := range access.DefaultSensitive[c.Role] {
		c.Sensitive[k] = v
	}
	sens, _ := q.C("role_sensitive").Find(ctx, bson.M{"role_name": c.Role})
	for _, r := range sens {
		c.Sensitive[r["grp"].(string)] = r["mode"].(string)
	}
	if role, _ := q.C("role").FindOne(ctx, bson.M{"name": c.Role}); role != nil {
		c.CanDownload, _ = role["can_download"].(bool)
	}
	return c, nil
}

// ctxRoleMaps are the person's active mappings to active roles. They come in the role table's stored order, the
// order the old join of role and user_role_map returned them in, which is the order of the role views.
func ctxRoleMaps(ctx context.Context, q store.Q, employeeID string) ([]store.Row, error) {
	maps, err := q.C("user_role_map").Find(ctx, bson.M{"employee_id": employeeID, "active": true})
	if err != nil || len(maps) == 0 {
		return nil, err
	}
	byRole := map[string]store.Row{}
	names := bson.A{}
	for _, m := range maps {
		byRole[m["role_name"].(string)] = m
		names = append(names, m["role_name"])
	}
	roles, err := q.C("role").Find(ctx, bson.M{"name": bson.M{"$in": names}, "active": true}, ctxStoredOrder())
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, r := range roles {
		if m, ok := byRole[r["name"].(string)]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// ctxStoredOrder reads documents in the order they were stored, as a PostgreSQL table scan returned rows where a
// query had no ORDER BY (or tied on it).
func ctxStoredOrder() *options.FindOptionsBuilder {
	return options.Find().SetSort(bson.D{{Key: "$natural", Value: 1}})
}

// authenticate resolves the Bearer session; the role view comes from the X-Role header.
func (s *Server) authenticate(r *http.Request) (*Ctx, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return nil, unauthorized("Sign in first")
	}
	sess, err := s.st.DB.C("auth_session").FindOne(r.Context(), bson.M{"token_hash": sha(tok), "expires_at": bson.M{"$gt": time.Now()}})
	if err != nil || sess == nil {
		return nil, unauthorized("Your session has ended. Sign in again")
	}
	emp, _ := sess["employee_id"].(string)
	// Every request checks the person may still sign in (spec 6.5.7): someone moved back to Pre-boarding loses
	// the session opened before (AUTH-010).
	if e, err := s.st.DB.C("employee").FindOne(r.Context(), bson.M{"_id": emp}, options.FindOne().SetProjection(bson.M{"status": 1})); err == nil && e != nil && e["status"] == "Pre-boarding" {
		_, _ = s.st.DB.C("auth_session").DeleteMany(r.Context(), bson.M{"employee_id": emp})
		return nil, unauthorized("Your account opens on your joining date.")
	}
	syncTypeRules(r.Context(), s.st.DB) // rules by employment type, as stored
	c, err := loadCtx(r.Context(), s.st.DB, emp, r.Header.Get("X-Role"))
	// "Direct employees" filter of a manager (team scope): only the first level (owner, 27 Nov 2026).
	if err == nil && c.Scope.Access == "team" && (r.URL.Query().Get("team") == "direct" || r.Header.Get("X-Team") == "direct") {
		c.Scope.Managers = []string{c.EmployeeID}
	}
	return c, err
}

func (c *Ctx) Level(right string) int { return c.Rights[right] }

func (c *Ctx) Need(right string, min int) error {
	if c.Rights[right] < min {
		return forbidden("")
	}
	return nil
}

// Show applies Full / Masked / Hidden to a confidential value; own data is always in full.
func (c *Ctx) Show(group string, v any, owner string) any {
	if owner == c.EmployeeID {
		return v
	}
	mode := c.Sensitive[group]
	if mode == "" {
		mode = access.Hidden
	}
	return access.MaskGroup(group, v, mode)
}

// InScope reports whether employeeID — an employee, an offer or a shortlisted candidate (the approval subjects) —
// is inside the caller's data scope. Like the old join with department, a subject whose department does not exist
// is outside every scope.
func (c *Ctx) InScope(ctx context.Context, q store.Q, employeeID string) (bool, error) {
	e, err := approvalSubjectOf(ctx, q, employeeID)
	if err != nil || e == nil {
		return false, err
	}
	d, err := q.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]})
	if err != nil || d == nil {
		return false, err
	}
	return c.Scope.Allows(access.ScopedEmployee{ID: employeeID, LocationID: str(e["location_id"]), Department: str(d["name"]), ManagerID: str(e["manager_id"])}), nil
}

func (c *Ctx) MustInScope(ctx context.Context, q store.Q, employeeID string) error {
	ok, err := c.InScope(ctx, q, employeeID)
	if err != nil {
		return err
	}
	if !ok {
		return forbidden("This employee is outside your access")
	}
	return nil
}

// ScopeFilter is the caller's data scope as a query filter — what c.Scope.SQL(&args, "e", "d") added to a query
// over `employee e JOIN department d`: all, the mapped locations, the caller's reports (team) or the caller alone
// (self), and the department limit as the ids of the departments with those names. prefix "" is for employee,
// offer or candidate documents (id in `_id`); a prefix such as "e." is for a subject embedded by
// approvalSubjectStages("e") (id in `e.id`). An empty filter matches everything; combine it with other conditions
// through $and when they may name the same fields.
func (c *Ctx) ScopeFilter(ctx context.Context, q store.Q, prefix string) (bson.D, error) {
	sc := c.Scope
	idField := "_id"
	if prefix != "" {
		idField = prefix + "id"
	}
	f := bson.D{}
	switch sc.Access {
	case "all":
	case "locs":
		f = append(f, bson.E{Key: prefix + "location_id", Value: bson.D{{Key: "$in", Value: append([]string{}, sc.Locations...)}}})
	case "team":
		f = append(f, bson.E{Key: prefix + "manager_id", Value: bson.D{{Key: "$in", Value: sc.TeamManagers()}}})
	default:
		f = append(f, bson.E{Key: idField, Value: sc.EmployeeID})
	}
	if len(sc.Departments) > 0 {
		ids, err := q.C("department").Distinct(ctx, "_id", bson.M{"name": bson.M{"$in": sc.Departments}})
		if err != nil {
			return nil, err
		}
		f = append(f, bson.E{Key: prefix + "department_id", Value: bson.D{{Key: "$in", Value: ids}}})
	}
	return f, nil
}

// audit writes to the append-only audit log.
func audit(ctx context.Context, q store.Q, c *Ctx, action, entity, entityID string, before, after any) error {
	actor, role := "System", ""
	if c != nil {
		actor, role = c.Name+" ("+c.EmployeeID+")", c.Role
	}
	_, err := q.C("audit_log").Insert(ctx, bson.M{"actor": actor, "actor_role": ctxNullIfEmpty(role), "action": action,
		"entity": ctxNullIfEmpty(entity), "entity_id": ctxNullIfEmpty(entityID), "before": jsonOrNil(before), "after": jsonOrNil(after)})
	return err
}

// ctxNullIfEmpty stores an empty text as null (the old NULLIF).
func ctxNullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ctxIndex loads the documents of coll whose field is one of vals, keyed by that field's value as text (`_id`
// keys by id): the lookup side of a join. The field must be unique in coll.
func ctxIndex(ctx context.Context, q store.Q, coll, field string, vals []any) (map[string]store.Row, error) {
	out := map[string]store.Row{}
	if len(vals) == 0 {
		return out, nil
	}
	rows, err := q.C(coll).Find(ctx, bson.M{field: bson.M{"$in": vals}})
	if err != nil {
		return nil, err
	}
	key := field
	if field == "_id" {
		key = "id"
	}
	for _, r := range rows {
		out[fmt.Sprint(r[key])] = r
	}
	return out, nil
}

// ctxValues are the distinct non-null values of field in rows, in first-seen order (the ids to join on).
func ctxValues(rows []store.Row, field string) []any {
	out, seen := []any{}, map[string]bool{}
	for _, r := range rows {
		if v := r[field]; v != nil && !seen[fmt.Sprint(v)] {
			seen[fmt.Sprint(v)] = true
			out = append(out, v)
		}
	}
	return out
}

// MustInScopeLocation checks a property against the caller's data scope.
func (c *Ctx) MustInScopeLocation(id string) error {
	if c.Scope.Access == "all" || (c.Scope.Access == "locs" && containsStr(c.Scope.Locations, id)) {
		return nil
	}
	return forbidden("That property is outside your access")
}
