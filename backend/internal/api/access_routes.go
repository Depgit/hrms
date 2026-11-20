package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

func (s *Server) rightsCatalogue(r *http.Request, c *Ctx) (any, error) { return access.Rights, nil }

func (s *Server) rolesList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.roles", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	roles, err := s.st.DB.C("role").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	// Each role with its rights {right: level} and confidential-data modes {group: mode}, as JSON objects.
	rights, err := accessByRole(ctx, s.st.DB, "role_right", "right_id", "level")
	if err != nil {
		return nil, err
	}
	sens, err := accessByRole(ctx, s.st.DB, "role_sensitive", "grp", "mode")
	if err != nil {
		return nil, err
	}
	for _, x := range roles {
		name := x["name"].(string)
		x["rights"], x["sensitive"] = map[string]any{}, map[string]any{}
		if m := rights[name]; m != nil {
			x["rights"] = m
		}
		if m := sens[name]; m != nil {
			x["sensitive"] = m
		}
	}
	return roles, nil
}

// accessByRole collects one per-role table as {role: {key: value}}. Numbers are float64, as JSON objects built by
// the database (jsonb_object_agg) read back.
func accessByRole(ctx context.Context, q store.Q, coll, key, val string) (map[string]map[string]any, error) {
	rows, err := q.C(coll).Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, x := range rows {
		role := x["role_name"].(string)
		if out[role] == nil {
			out[role] = map[string]any{}
		}
		v := x[val]
		if n, ok := v.(int16); ok {
			v = float64(n)
		}
		out[role][x[key].(string)] = v
	}
	return out, nil
}

func (s *Server) roleRights(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.roles", 2); err != nil {
		return nil, err
	}
	name := r.PathValue("name")
	if name == "HR Admin" {
		return nil, bad("HR Admin always keeps full rights so the system cannot be locked out")
	}
	var b struct {
		Rights      map[string]int    `json:"rights"`
		Sensitive   map[string]string `json:"sensitive"`
		CanDownload *bool             `json:"canDownload"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return nil, s.st.Tx(r.Context(), func(tx store.Q) error {
		role, _ := tx.C("role").FindOne(r.Context(), bson.M{"name": name})
		if role == nil {
			return notFound("Role not found")
		}
		// What the changed rights, modes and download were before, for the audit log (AUTH-095).
		before := map[string]any{}
		if len(b.Rights) > 0 {
			was := map[string]int{}
			for k := range b.Rights {
				was[k] = 0
			}
			rows, _ := tx.C("role_right").Find(r.Context(), bson.M{"role_name": name})
			for _, x := range rows {
				if _, ok := was[str(x["right_id"])]; ok {
					n, _ := x["level"].(int16)
					was[str(x["right_id"])] = int(n)
				}
			}
			before["rights"] = was
		}
		if len(b.Sensitive) > 0 {
			was := map[string]string{}
			for g := range b.Sensitive {
				was[g] = access.DefaultSensitive[name][g]
			}
			rows, _ := tx.C("role_sensitive").Find(r.Context(), bson.M{"role_name": name})
			for _, x := range rows {
				if _, ok := was[str(x["grp"])]; ok {
					was[str(x["grp"])] = str(x["mode"])
				}
			}
			before["sensitive"] = was
		}
		if b.CanDownload != nil {
			before["canDownload"] = role["can_download"]
		}
		for k, v := range b.Rights {
			if !access.KnownRight(k) || v < 0 || v > 2 {
				return bad("Unknown right %s or level %d", k, v)
			}
			if _, err := tx.C("role_right").Upsert(r.Context(), bson.M{"role_name": name, "right_id": k}, bson.M{"level": v}, nil); err != nil {
				return err
			}
		}
		for g, m := range b.Sensitive {
			if m != access.Full && m != access.Masked && m != access.Hidden {
				return bad("Mode must be Full, Masked or Hidden")
			}
			if _, err := tx.C("role_sensitive").Upsert(r.Context(), bson.M{"role_name": name, "grp": g}, bson.M{"mode": m}, nil); err != nil {
				return err
			}
		}
		if b.CanDownload != nil {
			if _, err := tx.C("role").Update(r.Context(), bson.M{"name": name}, bson.M{"$set": bson.M{"can_download": *b.CanDownload}}); err != nil {
				return err
			}
		}
		return audit(r.Context(), tx, c, "Changed role rights", "role", name, before, b)
	})
}

// ---------- Role mapping: one holder per place or department for a role (spec 6.2) ----------

type roleMap struct {
	ID          string   `json:"id"`
	EmployeeID  string   `json:"employeeId"`
	Role        string   `json:"role"`
	Access      string   `json:"access"`
	LocationIDs []string `json:"locationIds"`
	Departments []string `json:"departments"`
	AreaIDs     []string `json:"areaIds,omitempty"` // clusters, cities or states for those accesses
	Active      *bool    `json:"active,omitempty"`
}

// units are the places a mapping holds a role for; nil means the role may overlap (it does not approve).
func units(ctx context.Context, q store.Q, m roleMap) ([]string, error) {
	hasDept := len(m.Departments) > 0
	prefix := func(p string, l []string) []string {
		out := make([]string, len(l))
		for i, x := range l {
			out[i] = p + x
		}
		return out
	}
	// col lists one field of the active masters of coll (every row, duplicates included).
	col := func(coll, field string) ([]string, error) {
		rows, err := q.C(coll).Find(ctx, bson.M{"status": "Active"}, options.Find().SetProjection(bson.M{field: 1}))
		if err != nil {
			return nil, err
		}
		out := make([]string, len(rows))
		for i, x := range rows {
			out[i] = x[field].(string)
		}
		return out, nil
	}
	switch {
	case locationRoles[m.Role] && !(m.Role == "HRBP" && hasDept):
		ids := m.LocationIDs
		if m.Access == "all" {
			var err error
			if ids, err = col("location", "id"); err != nil {
				return nil, err
			}
		} else if isAreaAccess(m.Access) { // the locations inside the clusters, cities or states now
			var err error
			if ids, err = areaLocations(ctx, q, m.Access, m.AreaIDs); err != nil {
				return nil, err
			}
		}
		return prefix("loc|", ids), nil
	case m.Role == "Function Head":
		d := m.Departments
		if !hasDept {
			var err error
			if d, err = col("department", "name"); err != nil {
				return nil, err
			}
		}
		return prefix("dept|", d), nil
	case m.Role == "HRBP":
		return prefix("hdept|", m.Departments), nil
	case m.Role == "CHRO" || m.Role == "Payroll" || m.Role == "Payroll Manager" || m.Role == "Finance Head":
		return []string{"co|" + m.Role}, nil
	}
	return nil, nil
}

func overlaps(ctx context.Context, q store.Q, m roleMap) ([]string, error) {
	mine, err := units(ctx, q, m)
	if err != nil || len(mine) == 0 {
		return nil, err
	}
	set := map[string]bool{}
	for _, u := range mine {
		set[u] = true
	}
	// Other people's active mappings to the same role, for active employees, in the employees' stored order (the
	// order the old join gave).
	maps, err := q.C("user_role_map").Find(ctx, bson.M{"active": true, "role_name": m.Role, "employee_id": bson.M{"$ne": m.EmployeeID}, "_id": bson.M{"$ne": m.ID}})
	if err != nil {
		return nil, err
	}
	byEmp := map[string][]store.Row{}
	for _, x := range maps {
		byEmp[x["employee_id"].(string)] = append(byEmp[x["employee_id"].(string)], x)
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ctxValues(maps, "employee_id")}, "status": "Active"}, ctxStoredOrder())
	if err != nil {
		return nil, err
	}
	type other struct {
		roleMap
		name string
	}
	others := []other{}
	for _, e := range emps {
		for _, x := range byEmp[e["id"].(string)] {
			others = append(others, other{accessRoleMapOf(x), e["name"].(string)})
		}
	}
	var out []string
	for _, o := range others {
		u, err := units(ctx, q, o.roleMap)
		if err != nil {
			return nil, err
		}
		hit := 0
		for _, x := range u {
			if set[x] {
				hit++
			}
		}
		if hit > 0 {
			out = append(out, fmt.Sprintf("%s already holds %s for %d of the places you picked", o.name, m.Role, hit))
		}
	}
	return out, nil
}

func (s *Server) roleMapList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.users", 1); err != nil {
		return nil, err
	}
	return accessMapList(r.Context(), s.st.DB, c.Level("emp.personal") >= 1)
}

// accessMapList is every role mapping with the person's name, department and city, and their personal email for
// callers who may see personal details (else null), ordered by name and role.
func accessMapList(ctx context.Context, q store.Q, withEmail bool) ([]store.Row, error) {
	maps, err := q.C("user_role_map").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	emps, err := ctxIndex(ctx, q, "employee", "_id", ctxValues(maps, "employee_id"))
	if err != nil {
		return nil, err
	}
	empRows := make([]store.Row, 0, len(emps))
	for _, e := range emps {
		empRows = append(empRows, e)
	}
	depts, err := ctxIndex(ctx, q, "department", "_id", ctxValues(empRows, "department_id"))
	if err != nil {
		return nil, err
	}
	locs, err := ctxIndex(ctx, q, "location", "_id", ctxValues(empRows, "location_id"))
	if err != nil {
		return nil, err
	}
	clusters, err := q.C("cluster").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	cities, err := q.C("city").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	clusterCity, cityName := map[string]any{}, map[string]any{}
	for _, k := range clusters {
		clusterCity[k["id"].(string)] = k["city_id"]
	}
	for _, ci := range cities {
		cityName[ci["id"].(string)] = ci["name"]
	}
	personal, err := ctxIndex(ctx, q, "employee_personal", "employee_id", ctxValues(empRows, "id"))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, m := range maps {
		e := emps[str(m["employee_id"])]
		if e == nil {
			continue
		}
		d, l := depts[str(e["department_id"])], locs[str(e["location_id"])]
		if d == nil || l == nil {
			continue
		}
		city, ok := cityName[str(clusterCity[str(l["cluster_id"])])]
		if !ok {
			continue
		}
		accessStripArea(m)
		m["name"], m["department"], m["city"], m["email"] = e["name"], d["name"], city, nil
		if p := personal[str(e["id"])]; withEmail && p != nil {
			m["email"] = p["email"]
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a["name"] != b["name"] {
			return a["name"].(string) < b["name"].(string)
		}
		if a["role_name"] != b["role_name"] {
			return a["role_name"].(string) < b["role_name"].(string)
		}
		return a["id"].(string) < b["id"].(string)
	})
	return out, nil
}

// roleAssignable: a built-in role other than Employee and Line Manager (which come from the employee record), or an
// active role added under Roles and rights (AUTH-103).
func roleAssignable(ctx context.Context, q store.Q, role string) error {
	if role == "Employee" || role == "Line Manager" {
		return bad("Employee and Line Manager come from the employee record and cannot be assigned")
	}
	if access.Assignable(role) {
		return nil
	}
	if ok, err := q.C("role").Exists(ctx, bson.M{"name": role, "active": true}); err != nil {
		return err
	} else if !ok {
		return bad("Pick a role from Roles and rights")
	}
	return nil
}

// accessRoleMapOf reads a stored mapping.
func accessRoleMapOf(x store.Row) roleMap {
	return roleMap{ID: str(x["id"]), EmployeeID: str(x["employee_id"]), Role: str(x["role_name"]), Access: str(x["access"]),
		LocationIDs: strs(x["location_ids"]), Departments: strs(x["departments"]), AreaIDs: strs(x["area_ids"])}
}

func (s *Server) roleMapCreate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.users", 2); err != nil {
		return nil, err
	}
	var m roleMap
	if err := decode(r, &m); err != nil {
		return nil, err
	}
	if err := roleAssignable(r.Context(), s.st.DB, m.Role); err != nil {
		return nil, err
	}
	if m.Access != "all" && m.Access != "locs" && !isAreaAccess(m.Access) {
		return nil, bad("Data access must be all, locs, clusters, cities or states")
	}
	if m.Access == "locs" && len(m.LocationIDs) == 0 {
		return nil, bad("Pick at least one location")
	}
	if m.Access != "locs" {
		m.LocationIDs = []string{}
	}
	if !isAreaAccess(m.Access) {
		m.AreaIDs = nil
	} else if err := checkArea(r.Context(), s.st.DB, m.Access, m.AreaIDs); err != nil {
		return nil, err
	}
	if m.Role == adminRole {
		if err := adminChangeCheck(r.Context(), s.st.DB, c, m.EmployeeID, false); err != nil {
			return nil, err
		}
	}
	if m.Role == "Function Head" && len(m.Departments) == 0 {
		return nil, bad("Pick the departments this Function Head leads")
	}
	if m.Departments == nil {
		m.Departments = []string{}
	}
	var row store.Row
	err := s.st.Tx(r.Context(), func(tx store.Q) error {
		e, err := tx.C("employee").FindOne(r.Context(), bson.M{"_id": m.EmployeeID})
		if err != nil {
			return err
		}
		if e == nil {
			return notFound("Employee not found")
		}
		name := e["name"].(string)
		if e["status"] != "Active" {
			return bad("Only active employees can hold a role")
		}
		ov, err := overlaps(r.Context(), tx, m)
		if err != nil {
			return err
		}
		if len(ov) > 0 {
			return badD("Overlap not allowed. "+ov[0]+". Switch that mapping off or choose other places.", ov)
		}
		doc := bson.M{"employee_id": m.EmployeeID, "role_name": m.Role, "access": m.Access,
			"location_ids": m.LocationIDs, "departments": m.Departments, "created_by": c.Name}
		if m.AreaIDs != nil {
			doc["area_ids"] = m.AreaIDs
		}
		id, err := tx.C("user_role_map").Insert(r.Context(), doc)
		if err != nil {
			return err
		}
		if row, err = tx.C("user_role_map").FindOne(r.Context(), bson.M{"_id": id}); err != nil {
			return err
		}
		accessStripArea(row)
		return audit(r.Context(), tx, c, "Mapped "+name+" as "+m.Role, "user_role_map", fmt.Sprint(row["id"]), nil, row)
	})
	return row, err
}

func (s *Server) roleMapUpdate(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.users", 2); err != nil {
		return nil, err
	}
	var b roleMap
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	var row store.Row
	err := s.st.Tx(r.Context(), func(tx store.Q) error {
		x, err := tx.C("user_role_map").Lock(r.Context(), bson.M{"_id": r.PathValue("id")})
		if err != nil {
			return err
		}
		if x == nil {
			return notFound("")
		}
		cur, active := accessRoleMapOf(x), x["active"] == true
		if b.Access != "" {
			cur.Access = b.Access
		}
		if b.LocationIDs != nil {
			cur.LocationIDs = b.LocationIDs
		}
		if b.Departments != nil {
			cur.Departments = b.Departments
		}
		if b.AreaIDs != nil {
			cur.AreaIDs = b.AreaIDs
		}
		if b.Active != nil {
			active = *b.Active
		}
		// Only an HR Admin takes HR Admin away (switched off or changed to another role), and never from the last one.
		if cur.Role == adminRole && x["active"] == true && (!active || (b.Role != "" && b.Role != cur.Role)) {
			if err := adminChangeCheck(r.Context(), tx, c, cur.EmployeeID, true); err != nil {
				return err
			}
		}
		if (b.Role == adminRole && cur.Role != adminRole) || (cur.Role == adminRole && active && x["active"] != true) {
			if err := adminChangeCheck(r.Context(), tx, c, cur.EmployeeID, false); err != nil {
				return err
			}
		}
		if b.Role != "" && b.Role != cur.Role { // the role can change on edit, with the same checks as a new mapping
			if err := roleAssignable(r.Context(), tx, b.Role); err != nil {
				return err
			}
			if ok, _ := tx.C("user_role_map").Exists(r.Context(), bson.M{"employee_id": cur.EmployeeID, "role_name": b.Role, "_id": bson.M{"$ne": cur.ID}}); ok {
				return bad("This person already has the %s role. Edit that mapping instead", b.Role)
			}
			cur.Role = b.Role
			if cur.Role == "Function Head" && len(cur.Departments) == 0 {
				return bad("Pick the departments this Function Head leads")
			}
		}
		if cur.Access != "all" && cur.Access != "locs" && !isAreaAccess(cur.Access) {
			return bad("Data access must be all, locs, clusters, cities or states")
		}
		if cur.Access != "locs" {
			cur.LocationIDs = []string{}
		} else if len(cur.LocationIDs) == 0 {
			return bad("Pick at least one location")
		}
		if !isAreaAccess(cur.Access) {
			cur.AreaIDs = nil
		} else if err := checkArea(r.Context(), tx, cur.Access, cur.AreaIDs); err != nil {
			return err
		}
		if active {
			ov, err := overlaps(r.Context(), tx, cur)
			if err != nil {
				return err
			}
			if len(ov) > 0 {
				return badD("Overlap not allowed. "+ov[0]+".", ov)
			}
		}
		set := bson.M{"access": cur.Access, "location_ids": cur.LocationIDs, "departments": cur.Departments, "active": active, "role_name": cur.Role}
		if cur.AreaIDs != nil || x["area_ids"] != nil {
			set["area_ids"] = cur.AreaIDs
		}
		if row, err = tx.C("user_role_map").FindOneAndUpdate(r.Context(), bson.M{"_id": cur.ID}, bson.M{"$set": set}); err != nil {
			return err
		}
		accessStripArea(x)
		accessStripArea(row)
		return audit(r.Context(), tx, c, "Changed role mapping", "user_role_map", cur.ID, x, row)
	})
	return row, err
}

func (s *Server) auditLog(r *http.Request, c *Ctx) (any, error) {
	if c.Level("acc.roles") < 1 && c.Level("acc.users") < 1 {
		return nil, forbidden("")
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	return s.st.DB.C("audit_log").Find(r.Context(), nil, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetSkip(int64((page-1)*100)).SetLimit(100))
}
