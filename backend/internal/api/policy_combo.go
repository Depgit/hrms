package api

// Combined work policy mappings (owner, 24 Nov 2026): a department together with one place, for example the People
// Ops department at the Gurgaon office. Stored as level dept_location / dept_cluster / dept_city / dept_state with
// ref "<department name>|<place id>" (store/schema_policycombo.go). A combined mapping ranks after an employee
// mapping and before every single level; among combined mappings the narrower place wins (policyLevels).

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// comboPlace is the place level of each combined level.
var comboPlace = map[string]string{"dept_location": "location", "dept_cluster": "cluster", "dept_city": "city", "dept_state": "state"}

// comboRef is the ref of a combined mapping.
func comboRef(dept, place string) string { return dept + "|" + place }

// splitCombo reads a combined ref into the department name and the place id (the place id never holds a "|").
func splitCombo(ref string) (dept, place string, ok bool) {
	i := strings.LastIndex(ref, "|")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// comboLevelRef is the employee's value at a combined level ("" when the level is not combined or a part is missing).
func (e *emp) comboLevelRef(level string) string {
	pl, ok := comboPlace[level]
	if !ok {
		return ""
	}
	d, p := e.Dept, e.levelRef(pl)
	if d == "" || p == "" {
		return ""
	}
	return comboRef(d, p)
}

// comboRefExists: the department (by name) and the place (by id) of a combined ref both exist.
func comboRefExists(ctx context.Context, q store.Q, level, ref string) bool {
	pl, ok := comboPlace[level]
	if !ok {
		return false
	}
	d, p, ok := splitCombo(ref)
	if !ok {
		return false
	}
	okD, err := q.C("department").Exists(ctx, bson.M{"name": d})
	if err != nil {
		return true // a failed read does not block the save
	}
	okP, err := q.C(pl).Exists(ctx, bson.M{"_id": p})
	return err != nil || (okD && okP)
}

// renameComboDept renames a department inside the combined mappings that carry it; it returns how many changed.
func renameComboDept(ctx context.Context, q store.Q, old, name string) (int64, error) {
	wm := q.C("work_policy_map")
	rows, err := wm.Find(ctx, bson.M{"level": bson.M{"$in": store.ComboLevels}})
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range rows {
		d, p, ok := splitCombo(str(r["ref"]))
		if !ok || d != old {
			continue
		}
		if _, err := wm.Update(ctx, bson.M{"_id": r["id"]}, bson.M{"$set": bson.M{"ref": comboRef(name, p)}}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
