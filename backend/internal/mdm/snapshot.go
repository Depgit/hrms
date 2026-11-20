package mdm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// The published employee snapshot, schema v1. Data minimisation (SOP §3): only what consumers need to show who
// approved or did something and where they work — no personal contact data, salary, bank or government ids.
//
//	employee_code    HRMS employee code (the identifier; it never changes)
//	name             full name
//	status           Pre-boarding | Active | Inactive
//	exit_status      the separation's state, or null
//	employment_type  Permanent | Retainer | Contractual
//	designation      designation name, or null
//	department       department name, or null
//	grade            grade name
//	location         HRMS location name, or null
//	property_id      the location's CMS property_id, or null while the location is not linked
//	manager_code     the reporting manager's employee_code, or null
//	doj              date of joining, YYYY-MM-DD
//	lwd              last working day, YYYY-MM-DD, or null
//
// Every value is a string or null.

// SnapshotFields lists the snapshot's keys in the order the documentation shows them.
var SnapshotFields = []string{"employee_code", "name", "status", "exit_status", "employment_type", "designation", "department",
	"grade", "location", "property_id", "manager_code", "doj", "lwd"}

// employeeFields are the employee fields the snapshot reads: a change to any other field never changes it.
var employeeFields = map[string]bool{"name": true, "type": true, "status": true, "exit_status": true, "designation_id": true,
	"department_id": true, "grade": true, "location_id": true, "manager_id": true, "doj": true, "lwd": true}

// snapshots computes the v1 snapshot of each of these employees that exists, by code.
func snapshots(ctx context.Context, q store.Q, codes []string) (map[string]map[string]any, error) {
	proj := bson.M{}
	for f := range employeeFields {
		proj[f] = 1
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": codes}}, options.Find().SetProjection(proj))
	if err != nil {
		return nil, err
	}
	ids := func(field string) []any {
		seen, out := map[string]bool{}, []any{}
		for _, e := range emps {
			if v := str(e[field]); v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		return out
	}
	names := func(coll, field string, extra ...string) (map[string]store.Row, error) {
		out := map[string]store.Row{}
		l := ids(field)
		if len(l) == 0 {
			return out, nil
		}
		p := bson.M{"name": 1}
		for _, x := range extra {
			p[x] = 1
		}
		rows, err := q.C(coll).Find(ctx, bson.M{"_id": bson.M{"$in": l}}, options.Find().SetProjection(p))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[str(r["id"])] = r
		}
		return out, nil
	}
	desig, err := names("designation", "designation_id")
	if err != nil {
		return nil, err
	}
	dept, err := names("department", "department_id")
	if err != nil {
		return nil, err
	}
	loc, err := names("location", "location_id", "property_id")
	if err != nil {
		return nil, err
	}
	nameOf := func(m map[string]store.Row, id any) any {
		if r, ok := m[str(id)]; ok {
			return strOrNil(r["name"])
		}
		return nil
	}
	out := make(map[string]map[string]any, len(emps))
	for _, e := range emps {
		code := str(e["id"])
		var pid any
		if l, ok := loc[str(e["location_id"])]; ok {
			pid = strOrNil(l["property_id"])
		}
		out[code] = map[string]any{
			"employee_code":   code,
			"name":            strOrNil(e["name"]),
			"status":          strOrNil(e["status"]),
			"exit_status":     strOrNil(e["exit_status"]),
			"employment_type": strOrNil(e["type"]),
			"designation":     nameOf(desig, e["designation_id"]),
			"department":      nameOf(dept, e["department_id"]),
			"grade":           strOrNil(e["grade"]),
			"location":        nameOf(loc, e["location_id"]),
			"property_id":     pid,
			"manager_code":    strOrNil(e["manager_id"]),
			"doj":             strOrNil(e["doj"]),
			"lwd":             strOrNil(e["lwd"]),
		}
	}
	return out, nil
}

// Canonical is the canonical JSON of a value: object keys sorted, no whitespace, UTF-8, no HTML escaping. The
// snapshot hash is the SHA-256 of the snapshot's canonical JSON.
func Canonical(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("mdm: canonical JSON: %v", err))
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Hash is the hex SHA-256 of a snapshot's canonical JSON.
func Hash(data map[string]any) string {
	h := sha256.Sum256(Canonical(data))
	return hex.EncodeToString(h[:])
}

// eventTypeOf is the type of the event that publishes cur after prev (nil: never published).
func eventTypeOf(prev, cur map[string]any) string {
	if prev == nil {
		return EmployeeCreated
	}
	was, now := str(prev["status"]), str(cur["status"])
	switch {
	case was != "Inactive" && now == "Inactive":
		return EmployeeSeparated
	case was == "Inactive" && now != "Inactive":
		return EmployeeReactivated
	}
	return EmployeeUpdated
}

func uniqSorted(l []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(l))
	for _, x := range l {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
