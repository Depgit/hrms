package api

// Which report each role sees (test sheet #21). Access control › Roles and rights lists every report, grouped as on
// the Reports page, with a tick per role (store.RoleReportColl). A report without a tick is left out of the role's
// report list, and running or downloading it is refused (403). No document means ticked, so a new report or a new
// role keeps the old behaviour. HR Admin always sees every report. A report still needs the rights it always needed
// (the Reports right, the payroll right for payroll reports, …): the tick only takes reports away. Role level only:
// personal rights (access_personal.go) do not grant or block single reports.

import (
	"context"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

func init() {
	store.ReportIDs = func() []string {
		out := []string{}
		for _, rp := range allReports() {
			out = append(out, rp.ID)
		}
		return out
	}
}

// allReports is every report, the hiring drop-off report included before its routes add it to the list.
func allReports() []report {
	out := append([]report{}, reports...)
	if reportByID(hireDropoffReport.ID) == nil {
		out = append(out, hireDropoffReport)
	}
	return out
}

// reportsOff are the reports the caller's role has unticked (none for HR Admin).
func reportsOff(ctx context.Context, q store.Q, role string) (map[string]bool, error) {
	off := map[string]bool{}
	if role == adminRole {
		return off, nil
	}
	rows, err := q.C(store.RoleReportColl).Find(ctx, bson.M{"role_name": role, "on": false})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		off[str(r["report_id"])] = true
	}
	return off, nil
}

// reportTicked refuses a report the caller's role is not given.
func reportTicked(ctx context.Context, q store.Q, c *Ctx, id string) error {
	off, err := reportsOff(ctx, q, c.Role)
	if err != nil {
		return err
	}
	if off[id] {
		return forbidden("This report is not given to your role")
	}
	return nil
}

// roleCtx is a role's rights as a caller of that role has them (without personal rights), to tell which reports
// the role's rights allow.
func roleCtx(ctx context.Context, q store.Q, role string) (*Ctx, error) {
	c := &Ctx{Role: role, Rights: map[string]int{}, Sensitive: map[string]string{}}
	if role == adminRole {
		for _, r := range access.Rights {
			c.Rights[r.ID] = 2
		}
		return c, nil
	}
	rows, err := q.C("role_right").Find(ctx, bson.M{"role_name": role})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		c.Rights[str(r["right_id"])] = rowLevel(r["level"])
	}
	return c, nil
}

// roleReports is GET /api/access/reports?role=: every report grouped as on the Reports page, each with the role's
// tick (on) and whether the role's rights allow it (rights: false says the tick alone does not show it).
func (s *Server) roleReports(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.roles", 1); err != nil {
		return nil, err
	}
	ctx, q := r.Context(), s.st.DB
	role := r.URL.Query().Get("role")
	if role == "" {
		return nil, bad("Pick a role")
	}
	if ok, err := q.C("role").Exists(ctx, bson.M{"name": role}); err != nil {
		return nil, err
	} else if !ok {
		return nil, notFound("Role not found")
	}
	off, err := reportsOff(ctx, q, role)
	if err != nil {
		return nil, err
	}
	rc, err := roleCtx(ctx, q, role)
	if err != nil {
		return nil, err
	}
	type item struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Hint   string `json:"hint"`
		On     bool   `json:"on"`
		Rights bool   `json:"rights"`
	}
	type group struct {
		Group   string `json:"group"`
		Reports []item `json:"reports"`
	}
	groups := []*group{}
	byName := map[string]*group{}
	for _, rp := range allReports() {
		g := byName[rp.Group]
		if g == nil {
			g = &group{Group: rp.Group}
			byName[rp.Group] = g
			groups = append(groups, g)
		}
		g.Reports = append(g.Reports, item{ID: rp.ID, Name: rp.Name, Hint: rp.Hint, On: !off[rp.ID],
			Rights: rc.Level("rep.reports") >= 1 && (rp.Need == nil || rp.Need(rc))})
	}
	return map[string]any{"role": role, "locked": role == adminRole, "groups": groups}, nil
}

// roleReportsPut is PUT /api/roles/{name}/reports {reports: {id: true|false}}: ticks or unticks reports for a role.
func (s *Server) roleReportsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("acc.roles", 2); err != nil {
		return nil, err
	}
	name := r.PathValue("name")
	if name == adminRole {
		return nil, bad("HR Admin always sees every report")
	}
	var b struct {
		Reports map[string]bool `json:"reports"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.Reports) == 0 {
		return nil, bad("Pick a report")
	}
	known := map[string]bool{}
	for _, rp := range allReports() {
		known[rp.ID] = true
	}
	ids := make([]string, 0, len(b.Reports))
	for id := range b.Reports {
		if !known[id] {
			return nil, bad("Unknown report %s", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if ok, err := tx.C("role").Exists(ctx, bson.M{"name": name}); err != nil {
			return err
		} else if !ok {
			return notFound("Role not found")
		}
		off, err := reportsOff(ctx, tx, name)
		if err != nil {
			return err
		}
		before, after := map[string]bool{}, map[string]bool{}
		now := time.Now()
		for _, id := range ids {
			before[id], after[id] = !off[id], b.Reports[id]
			if _, err := tx.C(store.RoleReportColl).Upsert(ctx, bson.M{"role_name": name, "report_id": id},
				bson.M{"on": b.Reports[id], "updated_by": c.EmployeeID, "updated_at": now}, nil); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Reports of role “"+name+"” changed", "role", name, map[string]any{"reports": before}, map[string]any{"reports": after})
	})
}

func (s *Server) reportAccessRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/access/reports", s.a(s.roleReports))
	m.HandleFunc("PUT /api/roles/{name}/reports", s.a(s.roleReportsPut))
}
