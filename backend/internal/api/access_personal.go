package api

// Access additions (workforce plan module 2, 17 Nov 2026): personal rights on top of roles, data scope by state,
// city or cluster, and the "who can see what" report of one person.
//
//   - A personal right is a grant (the person gets the right at that level in every role view) or a block (the
//     right is cut to that level, 0 = none, in every role view), with an optional last day and a reason. Effective
//     rights = the role view's rights + grants − blocks. HR Admin always has every right and cannot be blocked.
//   - A role mapping's data access may be clusters, cities or states (area_ids). The locations inside them are
//     worked out on every request, so a location added to a covered cluster is covered at once.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

const adminRole = "HR Admin"

// isAreaAccess: a mapping whose places are clusters, cities or states.
func isAreaAccess(a string) bool {
	return a == store.AccessClusters || a == store.AccessCities || a == store.AccessStates
}

// areaColl is the master collection the ids of an area access name.
func areaColl(a string) string {
	return map[string]string{store.AccessClusters: "cluster", store.AccessCities: "city", store.AccessStates: "state"}[a]
}

// areaLocations lists the locations inside the given clusters, cities or states as they are now (every status, as
// a list of location ids may hold an inactive one).
func areaLocations(ctx context.Context, q store.Q, a string, ids []string) ([]string, error) {
	out := []string{}
	if len(ids) == 0 {
		return out, nil
	}
	in := func(l []string) bson.M { return bson.M{"$in": append([]string{}, l...)} }
	clusters := ids
	switch a {
	case store.AccessStates:
		cities, err := q.C("city").Distinct(ctx, "_id", bson.M{"state_id": in(ids)})
		if err != nil {
			return nil, err
		}
		ks, err := q.C("cluster").Distinct(ctx, "_id", bson.M{"city_id": bson.M{"$in": cities}})
		if err != nil {
			return nil, err
		}
		clusters = accessAnyStrs(ks)
	case store.AccessCities:
		ks, err := q.C("cluster").Distinct(ctx, "_id", bson.M{"city_id": in(ids)})
		if err != nil {
			return nil, err
		}
		clusters = accessAnyStrs(ks)
	case store.AccessClusters:
	default:
		return out, nil
	}
	if len(clusters) == 0 {
		return out, nil
	}
	locs, err := q.C("location").Find(ctx, bson.M{"cluster_id": in(clusters)}, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, err
	}
	for _, l := range locs {
		out = append(out, str(l["id"]))
	}
	return out, nil
}

func accessAnyStrs(l []any) []string {
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// mapLocations is a stored mapping's access as the scope uses it: all, or locs with the location ids (an area
// access resolved to its locations now).
func mapLocations(ctx context.Context, q store.Q, m store.Row) (string, []string, error) {
	a := str(m["access"])
	if isAreaAccess(a) {
		l, err := areaLocations(ctx, q, a, strs(m["area_ids"]))
		return store.AccessLocs, l, err
	}
	return a, strs(m["location_ids"]), nil
}

// accessStripArea leaves area_ids out of a mapping that has none, so mappings by locations read as before.
func accessStripArea(m store.Row) store.Row {
	if m != nil && m["area_ids"] == nil {
		delete(m, "area_ids")
	}
	return m
}

// checkArea validates the ids of an area access.
func checkArea(ctx context.Context, q store.Q, a string, ids []string) error {
	if len(ids) == 0 {
		return bad("Pick at least one %s", strings.TrimSuffix(map[string]string{store.AccessClusters: "cluster", store.AccessCities: "city", store.AccessStates: "state"}[a], "s"))
	}
	for _, id := range ids {
		if ok, err := q.C(areaColl(a)).Exists(ctx, bson.M{"_id": id}); err != nil {
			return err
		} else if !ok {
			return bad("A chosen %s is not in the masters", areaColl(a))
		}
	}
	return nil
}

// ---------- Personal rights ----------

type userRight struct {
	ID         string `json:"id"`
	EmployeeID string `json:"employeeId"`
	Right      string `json:"right"`
	Kind       string `json:"kind"`
	Level      int    `json:"level"`
	Until      string `json:"until,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// personalRights are the person's grants and blocks in force today (not ended, last day not passed).
func personalRights(ctx context.Context, q store.Q, employeeID string) ([]store.Row, error) {
	rows, err := q.C(store.UserRightColl).Find(ctx, bson.M{"employee_id": employeeID, "active": true}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	t := today()
	for _, r := range rows {
		if u := str(r["until"]); u != "" && u < t {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func rowLevel(v any) int {
	switch n := v.(type) {
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// applyPersonalRights finishes a context's rights: HR Admin gets every right at Edit; anyone else gets their grants
// added and their blocks applied (a block wins over a role and over a grant).
func applyPersonalRights(ctx context.Context, q store.Q, c *Ctx) error {
	if c.Role == adminRole {
		for _, r := range access.Rights {
			c.Rights[r.ID] = 2
		}
		return nil
	}
	rows, err := personalRights(ctx, q, c.EmployeeID)
	if err != nil {
		return err
	}
	for _, kind := range []string{"grant", "block"} {
		for _, r := range rows {
			if str(r["kind"]) != kind {
				continue
			}
			id, lv := str(r["right_id"]), rowLevel(r["level"])
			if kind == "grant" && lv > c.Rights[id] {
				c.Rights[id] = lv
			}
			if kind == "block" && c.Rights[id] > lv {
				c.Rights[id] = lv
			}
		}
	}
	return nil
}

// isAdmin: the person holds an active HR Admin mapping.
func isAdmin(ctx context.Context, q store.Q, employeeID string) (bool, error) {
	return q.C("user_role_map").Exists(ctx, bson.M{"employee_id": employeeID, "role_name": adminRole, "active": true})
}

// otherActiveAdmins counts active employees other than employeeID who hold an active HR Admin mapping.
func otherActiveAdmins(ctx context.Context, q store.Q, employeeID string) (int64, error) {
	ids, err := q.C("user_role_map").Distinct(ctx, "employee_id", bson.M{"role_name": adminRole, "active": true, "employee_id": bson.M{"$ne": employeeID}})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	return q.C("employee").Count(ctx, bson.M{"_id": bson.M{"$in": ids}, "status": "Active"})
}

// adminChangeCheck: only an HR Admin gives or takes away HR Admin, and the last active HR Admin stays.
func adminChangeCheck(ctx context.Context, q store.Q, c *Ctx, employeeID string, removing bool) error {
	if c.Role != adminRole {
		return forbidden("Only an HR Admin can give or remove the HR Admin role")
	}
	if removing {
		n, err := otherActiveAdmins(ctx, q, employeeID)
		if err != nil {
			return err
		}
		if n == 0 {
			return bad("This is the last active HR Admin. Map another HR Admin first so the system cannot be locked out")
		}
	}
	return nil
}

func rightLabel(id string) string {
	for _, r := range access.Rights {
		if r.ID == id {
			return r.Label
		}
	}
	return id
}

var levelText = map[int]string{0: "No access", 1: "View", 2: "Edit"}

// userRightsList: GET /api/user-rights[?employee=ID]: personal rights in force (and, with ?all=1, ended or past
// ones too), newest first, with the person's name.
func (s *Server) userRightsList(r *http.Request, c *Ctx) (any, error) {
	if c.Level("acc.users") < 1 && c.Level("acc.roles") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	f := bson.M{}
	if e := r.URL.Query().Get("employee"); e != "" {
		f["employee_id"] = e
	}
	if r.URL.Query().Get("all") != "1" {
		f["active"] = true
	}
	rows, err := s.st.DB.C(store.UserRightColl).Find(ctx, f, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	names, err := ctxIndex(ctx, s.st.DB, "employee", "_id", ctxValues(rows, "employee_id"))
	if err != nil {
		return nil, err
	}
	t := today()
	out := []map[string]any{}
	for _, x := range rows {
		lv := rowLevel(x["level"])
		u := str(x["until"])
		state := "In force"
		switch {
		case x["active"] != true:
			state = "Ended"
		case u != "" && u < t:
			state = "Expired"
		}
		out = append(out, map[string]any{"id": x["id"], "employeeId": x["employee_id"], "name": str(names[str(x["employee_id"])]["name"]),
			"right": x["right_id"], "rightLabel": rightLabel(str(x["right_id"])), "kind": x["kind"], "level": lv, "levelText": levelText[lv],
			"until": x["until"], "reason": x["reason"], "state": state, "createdBy": x["created_by"], "createdAt": x["created_at"],
			"endedBy": x["ended_by"], "endedAt": x["ended_at"]})
	}
	return out, nil
}

// userRightAdd: POST /api/user-rights {employeeId, right, kind, level, until, reason}. A grant gives View (1) or Edit
// (2); a block leaves No access (0) or View (1). It replaces the person's grant or block of the same right in force.
// Someone who is not HR Admin can give a right only up to the level they have themselves.
func (s *Server) userRightAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.users", 2); err != nil {
		return nil, err
	}
	var b userRight
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason, b.Until = strings.TrimSpace(b.Reason), strings.TrimSpace(b.Until)
	switch {
	case !access.KnownRight(b.Right):
		return nil, bad("Pick a right")
	case b.Kind != "grant" && b.Kind != "block":
		return nil, bad("Choose to give or block the right")
	case b.Kind == "grant" && (b.Level < 1 || b.Level > 2):
		return nil, bad("A right is given as View or Edit")
	case b.Kind == "block" && (b.Level < 0 || b.Level > 1):
		return nil, bad("A block leaves No access or View")
	case b.Until != "" && (!isDate(b.Until) || b.Until < today()):
		return nil, bad("The last day must be a date from today")
	case len(b.Reason) > 300:
		return nil, bad("Keep the reason under 300 characters")
	}
	if b.Kind == "grant" && c.Role != adminRole && b.Level > c.Level(b.Right) {
		return nil, forbidden("You can give a right only up to the level you have yourself. Ask an HR Admin")
	}
	ctx := r.Context()
	var id string
	err := s.st.Tx(ctx, func(tx store.Q) error {
		e, err := tx.C("employee").FindOne(ctx, bson.M{"_id": b.EmployeeID})
		if err != nil {
			return err
		}
		if e == nil {
			return notFound("Employee not found")
		}
		if b.Kind == "block" {
			if adm, err := isAdmin(ctx, tx, b.EmployeeID); err != nil {
				return err
			} else if adm {
				return bad("%s is an HR Admin. HR Admin always has every right and cannot be blocked", str(e["name"]))
			}
		}
		prev, err := tx.C(store.UserRightColl).Find(ctx, bson.M{"employee_id": b.EmployeeID, "right_id": b.Right, "active": true})
		if err != nil {
			return err
		}
		if _, err := tx.C(store.UserRightColl).UpdateMany(ctx, bson.M{"employee_id": b.EmployeeID, "right_id": b.Right, "active": true},
			bson.M{"$set": bson.M{"active": false, "ended_by": c.Name, "ended_at": tx.Now()}}); err != nil {
			return err
		}
		doc := bson.M{"employee_id": b.EmployeeID, "right_id": b.Right, "kind": b.Kind, "level": b.Level, "until": ctxNullIfEmpty(b.Until),
			"reason": ctxNullIfEmpty(b.Reason), "created_by": c.Name}
		nid, err := tx.C(store.UserRightColl).Insert(ctx, doc)
		if err != nil {
			return err
		}
		id = str(nid)
		verb := map[string]string{"grant": "Gave", "block": "Blocked"}[b.Kind]
		var before any
		if len(prev) > 0 {
			before = prev
		}
		return audit(ctx, tx, c, fmt.Sprintf("%s %s (%s) for %s", verb, rightLabel(b.Right), levelText[b.Level], str(e["name"])), store.UserRightColl, id, before, doc)
	})
	return map[string]string{"id": id}, err
}

// userRightEnd: DELETE /api/user-rights/{id} ends a grant or block (kept as history).
func (s *Server) userRightEnd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.users", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		x, err := tx.C(store.UserRightColl).Lock(ctx, bson.M{"_id": r.PathValue("id")})
		if err != nil {
			return err
		}
		if x == nil {
			return notFound("")
		}
		if x["active"] != true {
			return bad("This personal right has already ended")
		}
		row, err := tx.C(store.UserRightColl).FindOneAndUpdate(ctx, bson.M{"_id": x["id"]}, bson.M{"$set": bson.M{"active": false, "ended_by": c.Name, "ended_at": tx.Now()}})
		if err != nil {
			return err
		}
		name := ""
		if e, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": x["employee_id"]}); e != nil {
			name = str(e["name"])
		}
		verb := map[string]string{"grant": "Ended the personal right", "block": "Lifted the block on"}[str(x["kind"])]
		return audit(ctx, tx, c, fmt.Sprintf("%s %s for %s", verb, rightLabel(str(x["right_id"])), name), store.UserRightColl, str(x["id"]), x, row)
	})
}

// ---------- Who can see what ----------

type effSource struct {
	Kind  string `json:"kind"` // role | grant | block | admin
	Role  string `json:"role,omitempty"`
	Level int    `json:"level"`
	Until string `json:"until,omitempty"`
	Note  string `json:"note,omitempty"`
}

type effRight struct {
	ID           string      `json:"id"`
	Label        string      `json:"label"`
	Confidential bool        `json:"confidential"`
	Level        int         `json:"level"`
	Sources      []effSource `json:"sources"`
	Blocked      bool        `json:"blocked"`
}

type effRole struct {
	Role  string `json:"role"`
	Scope string `json:"scope"`
}

// effective works out a person's rights across all their roles with where each level comes from.
func effective(ctx context.Context, q store.Q, id string) (map[string]any, []effRight, error) {
	pc, err := loadCtx(ctx, q, id, "")
	if err != nil {
		return nil, nil, err
	}
	admin := containsStr(pc.Roles, adminRole)
	byRole := map[string]map[string]int{}
	rows, err := q.C("role_right").Find(ctx, bson.M{"role_name": bson.M{"$in": pc.Roles}})
	if err != nil {
		return nil, nil, err
	}
	for _, x := range rows {
		rn := str(x["role_name"])
		if byRole[rn] == nil {
			byRole[rn] = map[string]int{}
		}
		byRole[rn][str(x["right_id"])] = rowLevel(x["level"])
	}
	personal, err := personalRights(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	// Each role's data scope, in words.
	maps, err := ctxRoleMaps(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	roles := []effRole{}
	for _, r := range pc.Roles {
		sc := "Self only"
		switch r {
		case "Employee":
		case "Line Manager":
			sc = "Own team"
		default:
			for _, m := range maps {
				if str(m["role_name"]) == r {
					sc, err = scopeText(ctx, q, m)
					if err != nil {
						return nil, nil, err
					}
				}
			}
		}
		roles = append(roles, effRole{r, sc})
	}
	out := []effRight{}
	for _, rt := range access.Rights {
		e := effRight{ID: rt.ID, Label: rt.Label, Confidential: rt.Confidential, Sources: []effSource{}}
		if admin {
			e.Level = 2
			e.Sources = append(e.Sources, effSource{Kind: "admin", Role: adminRole, Level: 2, Note: "HR Admin has every right, always"})
		}
		for _, r := range pc.Roles {
			if r == adminRole {
				continue
			}
			if lv := byRole[r][rt.ID]; lv > 0 {
				e.Sources = append(e.Sources, effSource{Kind: "role", Role: r, Level: lv})
				if lv > e.Level {
					e.Level = lv
				}
			}
		}
		for _, p := range personal {
			if str(p["right_id"]) != rt.ID {
				continue
			}
			lv := rowLevel(p["level"])
			src := effSource{Kind: str(p["kind"]), Level: lv, Until: str(p["until"]), Note: str(p["reason"])}
			if src.Kind == "grant" {
				e.Sources = append(e.Sources, src)
				if lv > e.Level {
					e.Level = lv
				}
			} else if !admin {
				e.Sources = append(e.Sources, src)
				if e.Level > lv {
					e.Level, e.Blocked = lv, true
				}
			}
		}
		out = append(out, e)
	}
	head := map[string]any{"employeeId": pc.EmployeeID, "name": pc.Name, "admin": admin, "roles": roles, "personal": len(personal)}
	return head, out, nil
}

// scopeText says in words whose records a mapping covers.
func scopeText(ctx context.Context, q store.Q, m store.Row) (string, error) {
	a := str(m["access"])
	var t string
	switch {
	case a == store.AccessAll:
		t = "All locations"
	case isAreaAccess(a):
		ids := strs(m["area_ids"])
		rows, err := q.C(areaColl(a)).Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"name": 1}))
		if err != nil {
			return "", err
		}
		n := []string{}
		for _, x := range rows {
			n = append(n, str(x["name"]))
		}
		sort.Strings(n)
		t = map[string]string{store.AccessClusters: "Clusters: ", store.AccessCities: "Cities: ", store.AccessStates: "States: "}[a] + strings.Join(n, ", ")
	default:
		l := strs(m["location_ids"])
		t = fmt.Sprintf("%d location%s", len(l), map[bool]string{true: "", false: "s"}[len(l) == 1])
	}
	if d := strs(m["departments"]); len(d) > 0 {
		t += " · " + strings.Join(d, ", ") + " only"
	}
	return t, nil
}

func canSeeAccess(c *Ctx) error {
	if c.Level("acc.roles") < 1 && c.Level("acc.users") < 1 {
		return forbidden("")
	}
	return nil
}

// accessEffective: GET /api/access/person/{id}/effective: the person's rights across all their roles, the level
// they end up with and where it comes from (role, personal grant, block, HR Admin).
func (s *Server) accessEffective(r *http.Request, c *Ctx) (any, error) {
	if err := canSeeAccess(c); err != nil {
		return nil, err
	}
	head, rights, err := effective(r.Context(), s.st.DB, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	head["rights"] = rights
	return head, nil
}

// sourceText is a source in words, for the export.
func sourceText(x effSource) string {
	switch x.Kind {
	case "admin":
		return "HR Admin (every right)"
	case "role":
		return fmt.Sprintf("Role %s (%s)", x.Role, levelText[x.Level])
	}
	t := map[string]string{"grant": "Personal grant", "block": "Blocked to"}[x.Kind] + " (" + levelText[x.Level] + ")"
	if x.Until != "" {
		t += " until " + x.Until
	}
	if x.Note != "" {
		t += ": " + x.Note
	}
	return t
}

// accessEffectiveSheet: GET /api/access/person/{id}/effective.xlsx, the same report as a file.
func (s *Server) accessEffectiveSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := canSeeAccess(c); err != nil {
		return "", nil, nil, err
	}
	head, rights, err := effective(r.Context(), s.st.DB, r.PathValue("id"))
	if err != nil {
		return "", nil, nil, err
	}
	cols := []string{"Employee ID", "Name", "Right", "Confidential", "Effective level", "Where it comes from"}
	rows := [][]any{}
	for _, x := range rights {
		src := []string{}
		for _, s := range x.Sources {
			src = append(src, sourceText(s))
		}
		if len(src) == 0 {
			src = append(src, "No role gives it")
		}
		rows = append(rows, []any{head["employeeId"], head["name"], x.Label, map[bool]string{true: "Yes", false: ""}[x.Confidential], levelText[x.Level], strings.Join(src, "; ")})
	}
	return fmt.Sprintf("access-%s-%s.xlsx", head["employeeId"], time.Now().Format("2006-01-02")), cols, rows, nil
}
