package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// roleAdd creates a custom role with the rights of an existing one.
func (s *Server) roleAdd(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.roles", 2); err != nil {
		return nil, err
	}
	var b struct {
		Name        string `json:"name"`
		CopyFrom    string `json:"copyFrom"`
		Description string `json:"description"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len(b.Name) > 60 {
		return nil, bad("Name the role")
	}
	ctx := r.Context()
	if accessRoleTaken(ctx, s.st.DB, b.Name) {
		return nil, bad("A role with that name exists")
	}
	if b.Description == "" {
		b.Description = "Custom role"
	}
	return map[string]string{"name": b.Name}, s.st.Tx(ctx, func(tx store.Q) error {
		// The new role downloads files when the role it copies does (always, when it copies none).
		canDownload := true
		from, err := tx.C("role").FindOne(ctx, bson.M{"name": b.CopyFrom})
		if err != nil {
			return err
		}
		if from == nil && b.CopyFrom != "" { // an unknown role to copy would give a role with no rights (AUTH-102)
			return bad("Pick the role to copy from")
		}
		if from != nil {
			canDownload, _ = from["can_download"].(bool)
		}
		if _, err := tx.C("role").Insert(ctx, bson.M{"name": b.Name, "description": b.Description, "can_download": canDownload}); err != nil {
			return err
		}
		// Its rights and confidential-data modes start as a copy.
		for _, cp := range []struct{ coll, key, val string }{{"role_right", "right_id", "level"}, {"role_sensitive", "grp", "mode"}, {store.RoleReportColl, "report_id", "on"}} {
			rows, err := tx.C(cp.coll).Find(ctx, bson.M{"role_name": b.CopyFrom})
			if err != nil {
				return err
			}
			docs := make([]any, len(rows))
			for i, x := range rows {
				docs[i] = bson.M{"role_name": b.Name, cp.key: x[cp.key], cp.val: x[cp.val]}
			}
			if err := tx.C(cp.coll).InsertMany(ctx, docs); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, fmt.Sprintf("Role “%s” created from %s", b.Name, b.CopyFrom), "role", b.Name, nil, b)
	})
}

// rolePatch switches a role on or off and edits its description. Built-in roles stay on.
func (s *Server) rolePatch(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.roles", 2); err != nil {
		return nil, err
	}
	var b struct {
		Active      *bool   `json:"active"`
		Description *string `json:"description"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	name, ctx := r.PathValue("name"), r.Context()
	role, err := s.st.DB.C("role").FindOne(ctx, bson.M{"name": name})
	if err != nil || role == nil {
		return nil, notFound("Role not found")
	}
	if b.Active != nil && !*b.Active && role["system"] == true {
		return nil, bad("%s is a built-in role and cannot be switched off", name)
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		action := "Role “" + name + "” updated"
		if b.Active != nil {
			if _, err := tx.C("role").Update(ctx, bson.M{"name": name}, bson.M{"$set": bson.M{"active": *b.Active}}); err != nil {
				return err
			}
			action = map[bool]string{true: "Role “" + name + "” activated", false: "Role “" + name + "” switched off"}[*b.Active]
		}
		if b.Description != nil {
			if _, err := tx.C("role").Update(ctx, bson.M{"name": name}, bson.M{"$set": bson.M{"description": strings.TrimSpace(*b.Description)}}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, action, "role", name, map[string]any{"active": role["active"], "description": role["description"]}, b)
	})
}

// accessPerson shows exactly what one person gets in one role view: how many employees they
// see, confidential data, pages, actions, the workflows they approve and where they are named.
func (s *Server) accessPerson(r *http.Request, c *Ctx) (any, error) {
	if c.Level("acc.roles") < 1 && c.Level("acc.users") < 1 {
		return nil, forbidden("")
	}
	ctx, id := r.Context(), r.PathValue("id")
	pc, err := loadCtx(ctx, s.st.DB, id, r.URL.Query().Get("role"))
	if err != nil {
		return nil, err
	}
	// Everyone still employed, and those of them inside this person's scope.
	working := bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}}
	total, err := s.st.DB.C("employee").Count(ctx, working)
	if err != nil {
		return nil, err
	}
	scope, err := pc.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	visible, err := s.st.DB.C("employee").Count(ctx, bson.D{{Key: "$and", Value: bson.A{working, scope}}})
	if err != nil {
		return nil, err
	}
	var actions []string
	for _, a := range [][2]string{{"onb.raise", "Raise offers"}, {"appr.raise", "Raise leave, transfer and resignation for others"},
		{"emp.status", "Mark employees inactive"}, {"emp.export", "Export employee data"}, {"comp.approve", "Publish pay changes"},
		{"pay.approve", "Approve payroll"}, {"pay.export", "Export payroll files"}, {"ros.publish", "Approve and publish rosters"},
		{"appr.setup", "Change approval flows"}, {"acc.users", "Change users and access"}} {
		if pc.Level(a[0]) >= 2 {
			actions = append(actions, a[1])
		}
	}
	// Workflows where this role approves in a rule in use.
	approves := []string{}
	flows, err := loadFlows(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	for _, fl := range flows {
		wf, flow := fl.WF, fl.Flow
		hit := false
		for i, rule := range fl.Rules {
			for _, role := range rule.Approvers {
				if fl.inUse(i) && approval.StageRole(role) == pc.Role {
					hit = true
				}
			}
		}
		if hit {
			name := workflowName(wf)
			if strings.HasPrefix(wf, "leave") {
				name = "Leave (" + strings.TrimPrefix(wf, "leave") + ")"
			}
			if flow == "Corporate" {
				name += " (corporate)"
			}
			approves = append(approves, name)
		}
	}
	named := []string{}
	reports, _ := s.st.DB.C("employee").Count(ctx, bson.M{"status": bson.M{"$ne": "Inactive"},
		"$or": bson.A{bson.M{"manager_id": id}, bson.M{"functional_manager_id": id}}})
	if reports > 0 {
		named = append(named, fmt.Sprintf("Reporting manager of %d employee%s", reports, map[bool]string{true: "", false: "s"}[reports == 1]))
	}
	if d, err := buildDirectory(ctx, s.st.DB); err == nil {
		for _, l := range d.Locations {
			for role, p := range l.People {
				if p.ID == id {
					named = append(named, role+" for "+l.Name)
				}
			}
		}
		for _, dp := range d.Departments {
			for role, p := range dp.People {
				if p.ID == id {
					named = append(named, role+" for "+dp.Name)
				}
			}
		}
		for role, p := range d.Company {
			if p.ID == id {
				named = append(named, role+" (company-wide)")
			}
		}
	}
	conf := map[string]int{}
	for _, k := range []string{"emp.salary", "emp.govid", "emp.bank", "emp.personal", "emp.medical", "pay.run"} {
		conf[k] = pc.Level(k)
	}
	signIn, _ := s.st.DB.C("auth_credential").Exists(ctx, bson.M{"employee_id": id})
	return map[string]any{
		"employeeId": pc.EmployeeID, "name": pc.Name, "role": pc.Role, "roles": pc.Roles, "access": pc.Access,
		"locations": len(pc.Scope.Locations), "departments": pc.Scope.Departments,
		"total": int(total), "visible": int(visible), "rights": pc.Rights, "confidential": conf, "sensitive": pc.Sensitive,
		"actions": actions, "approves": approves, "named": named, "canDownload": pc.CanDownload, "signedUp": signIn,
	}, nil
}

// accessRoleTaken reports whether a role of this name exists in any letter case.
func accessRoleTaken(ctx context.Context, q store.Q, name string) bool {
	names, _ := q.C("role").Distinct(ctx, "name", nil)
	for _, n := range names {
		if s, _ := n.(string); strings.ToLower(s) == strings.ToLower(name) {
			return true
		}
	}
	return false
}
