package api

// Import employees and the later-change templates (owner, 17 Nov 2026). Import employees is the complete one-time
// sheet for new people: job, personal, bank and ID details, Attendance code, opening CL/PL/SL balances and uniform
// sizes. Employee master update, Personal details, Uniform sizes and Opening leave balances are only for later
// changes, so they never ask again for what the import gave: their "Download current data" is each employee's
// current values (imported ones included), and a value uploaded unchanged is no change (no reason or date needed).
// Personal details and Uniform sizes already listed their current data (bulk.go, uniform.go); this file adds it to
// Employee master update and Opening leave balances, and says in each template's help which template is for what.

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// bulkWhich is said once in each template's help.
const bulkWhich = " Import employees is for new people, once: the full record with opening CL/PL/SL balances, uniform sizes, personal, bank and ID details and Attendance code. Employee master update, Personal details, Uniform sizes and Opening leave balances are for later changes: download the current data (it already holds what the import gave) and change only what changed."

func init() {
	for _, t := range bulkTemplates {
		switch t.Key {
		case "new", "upd", "pers", "uni", "lvopen":
			t.Hint += bulkWhich
		}
		switch t.Key {
		case "upd":
			t.Data, t.current = true, updCurrent
		case "lvopen":
			t.Data, t.current = true, lvOpenCurrent
		}
	}
}

// updValues is each employee's current Employee master update values by column, as the download lists them and as
// bulkUpd compares an uploaded row with (equal is no change). Medical and Accommodation only with pay (the salary
// right at Edit, which they need), and only for the types that have them.
func updValues(ctx context.Context, q store.Q, emps []*emp, pay bool) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	if len(emps) == 0 {
		return out, nil
	}
	rows, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids(emps)}}, options.Find().SetProjection(bson.M{
		"functional_manager_id": 1, "manager_id": 1, "attendance_mode": 1, "job_role": 1, "skill": 1, "attendance_code": 1}))
	if err != nil {
		return nil, err
	}
	raw := map[string]store.Row{}
	for _, r := range rows {
		raw[str(r["id"])] = r
	}
	cityIDs := []string{}
	for _, e := range emps {
		cityIDs = append(cityIDs, e.CityID)
	}
	cities, err := timectxByID(ctx, q, "city", cityIDs, "name")
	if err != nil {
		return nil, err
	}
	ts, _ := timeSetFor(ctx, q)
	for _, e := range emps {
		r := raw[e.ID]
		v := map[string]string{"Employee ID": e.ID, "Work City": str(cities[e.CityID]["name"]), "Reporting Location": e.Location, "Department": e.Dept,
			"Designation": e.Desig, "Grade": e.Grade, "Shift": e.Shift, "Reporting Manager L1": str(r["manager_id"]), "Functional Manager L2": str(r["functional_manager_id"]),
			"Attendance Mode": str(r["attendance_mode"]), "Role": str(r["job_role"]), "Skill": str(r["skill"]), attCodeCol: str(r["attendance_code"])}
		if ts != nil {
			if p, _ := ts.policyFor(e); p != nil {
				v["Work Policy"] = p.Name
			}
		}
		if pay {
			t := calc.RulesFor(calc.EmploymentType(e.Type))
			p := payFor(ctx, q, e.ID, e.Type)
			if t.Statutory {
				v["Medical"] = "Standard"
				if p.Medical != nil {
					v["Medical"] = strconv.FormatFloat(*p.Medical, 'f', -1, 64)
				}
			}
			if t.Ded == "all" {
				v["Accommodation"] = strconv.FormatFloat(p.Accom, 'f', -1, 64)
			}
		}
		out[e.ID] = v
	}
	return out, nil
}

// updCurrent is Employee master update's current data: the active employees in scope, one row each.
func updCurrent(ctx context.Context, s *Server, c *Ctx) ([]string, [][]any, error) {
	q := s.st.DB
	t, err := templateFor(c, "upd")
	if err != nil {
		return nil, nil, err
	}
	emps, err := scopedEmps(ctx, q, c, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	vals, err := updValues(ctx, q, emps, c.Level("emp.salary") >= 2)
	if err != nil {
		return nil, nil, err
	}
	out := [][]any{}
	for _, e := range emps {
		row := []any{}
		for _, col := range t.Cols {
			row = append(row, vals[e.ID][col])
		}
		out = append(out, append(row, e.Name))
	}
	return append(append([]string{}, t.Cols...), "Name"), out, nil
}

// updUnchanged drops from a row's job changes (column → value) those equal to the employee's current value, so the
// current data uploaded as it is needs no reason or date.
func updUnchanged(ctx context.Context, tx store.Q, id string, ch map[string]string) {
	if len(ch) == 0 {
		return
	}
	emps, err := loadEmps(ctx, tx, bson.M{"_id": id})
	if err != nil || len(emps) == 0 {
		return
	}
	cur, err := updValues(ctx, tx, emps, false)
	if err != nil {
		return
	}
	col := map[string]string{"location": "Reporting Location", "department": "Department", "designation": "Designation", "grade": "Grade", "shift": "Shift",
		"manager": "Reporting Manager L1", "functionalManager": "Functional Manager L2", "attendanceMode": "Attendance Mode"}
	for k, v := range ch {
		if c, ok := col[k]; ok && strings.EqualFold(strings.TrimSpace(v), cur[id][c]) {
			delete(ch, k)
		}
	}
	// A designation is named with its department; a location with its city: keep the pair when one of them changes.
	if _, ok := ch["department"]; ok && cur[id]["Designation"] != "" {
		if _, has := ch["designation"]; !has {
			ch["designation"] = cur[id]["Designation"]
		}
	}
}

// lvOpenCurrent is Opening leave balances' current data: each active employee in scope with the latest opening of
// each leave type (as loaded by Import employees or this template) and the latest optional holiday opening, on the
// row with the same As at date when there is one, else on a row of its own. An employee with none gets a row with
// only the Employee ID (blank: nothing changes).
func lvOpenCurrent(ctx context.Context, s *Server, c *Ctx) ([]string, [][]any, error) {
	q := s.st.DB
	t, err := templateFor(c, "lvopen")
	if err != nil {
		return nil, nil, err
	}
	emps, err := scopedEmps(ctx, q, c, bson.D{{Key: "status", Value: "Active"}})
	if err != nil {
		return nil, nil, err
	}
	all := ids(emps)
	latest := func(coll string, key func(store.Row) string) (map[string]map[string]store.Row, error) {
		rows, err := q.C(coll).Find(ctx, bson.M{"employee_id": bson.M{"$in": all}}, options.Find().SetSort(bson.D{{Key: "at", Value: 1}}))
		if err != nil {
			return nil, err
		}
		out := map[string]map[string]store.Row{}
		for _, r := range rows {
			id := str(r["employee_id"])
			if out[id] == nil {
				out[id] = map[string]store.Row{}
			}
			out[id][key(r)] = r // the later date wins
		}
		return out, nil
	}
	lv, err := latest("leave_opening", func(r store.Row) string { return str(r["leave_type"]) })
	if err != nil {
		return nil, nil, err
	}
	oh, err := latest(store.OptHolOpeningColl, func(store.Row) string { return "" })
	if err != nil {
		return nil, nil, err
	}
	cols := append(append([]string{}, t.Cols...), "Name")
	row := func(id, lt string, bal any, at string, opt any, name string) []any {
		v := map[string]any{"Employee ID": id, "Leave type": lt, "Balance": bal, "As at": at, optHolCol: opt, "Name": name}
		out := []any{}
		for _, c := range cols {
			if x, ok := v[c]; ok && x != nil {
				out = append(out, x)
			} else {
				out = append(out, "")
			}
		}
		return out
	}
	out := [][]any{}
	for _, e := range emps {
		types := []string{}
		for lt := range lv[e.ID] {
			types = append(types, lt)
		}
		sort.Strings(types)
		var opt store.Row
		if x := oh[e.ID][""]; x != nil {
			opt = x
		}
		n := 0
		for _, lt := range types {
			x := lv[e.ID][lt]
			var o any
			if opt != nil && str(opt["at"]) == str(x["at"]) {
				o, opt = timectxNum(opt["balance"]), nil
			}
			out = append(out, row(e.ID, lt, timectxNum(x["balance"]), str(x["at"]), o, e.Name))
			n++
		}
		if opt != nil {
			out = append(out, row(e.ID, "", nil, str(opt["at"]), timectxNum(opt["balance"]), e.Name))
			n++
		}
		if n == 0 {
			out = append(out, row(e.ID, "", nil, "", nil, e.Name))
		}
	}
	return cols, out, nil
}
