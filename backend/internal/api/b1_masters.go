package api

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Job role and skill masters (owner, 14 Nov 2026). "Role" here is the employee's JOB role (front desk, kitchen …),
// never an access role of internal/access: the master type, simple_master kind and employee field are job_role,
// and the Form setup key is jobRole. Skill is the same kind of master (skill, employee.skill, Form setup jobSkill).
//
// Each is managed in Masters like Qualifications (add, edit, rename, inactive from a date, reactivate) and in the
// Master update workbook (sheets Roles and Skills). An employee holds one of each by name. Both are required on the
// offer form like the department and checked against the active list; existing employees without them stay valid.
// Bulk update › Employee master update and Import employees take them as columns (blank keeps / not given).

// jobMasterCol is the employee field of each master; jobMasterKey its offer form key.
var (
	jobMasterCol = map[string]string{store.KindJobRole: "job_role", store.KindSkill: "skill"}
	jobMasterKey = map[string]string{store.KindJobRole: "jobRole", store.KindSkill: "jobSkill"}
	// jobMasterOfField is the master behind each Form setup field.
	jobMasterOfField = map[string]string{"jobRole": store.KindJobRole, "jobSkill": store.KindSkill}
	jobMasterLabel   = map[string]string{store.KindJobRole: "Role", store.KindSkill: "Skill"}
)

func init() {
	for _, k := range []string{store.KindJobRole, store.KindSkill} {
		masters[k] = master{table: "simple_master", kind: k, right: "mas.job", label: strings.ToLower(jobMasterLabel[k]),
			fields: []field{{name: "name", kind: "text", required: true}}, list: mastersList(), impact: mastersNameImpact("simple_master", "name", jobMasterCol[k])}
	}
	jobFields["jobRole"] = jobField{"job_role", "Role", jobMasterLookup(store.KindJobRole), nil}
	jobFields["skill"] = jobField{"skill", "Skill", jobMasterLookup(store.KindSkill), nil}
	mbSheets = append(mbSheets, mbSheet{Key: store.KindJobRole, Sheet: "Roles", Right: "mas.job"}, mbSheet{Key: store.KindSkill, Sheet: "Skills", Right: "mas.job"})
}

// jobMasterLookup finds an active item of the master by name in any case and gives its name as the master spells it.
func jobMasterLookup(kind string) func(ctx context.Context, q store.Q, v string) (string, string, bool) {
	return func(ctx context.Context, q store.Q, v string) (string, string, bool) {
		return employeesPick(ctx, q, "simple_master", bson.M{"kind": kind, "name": employeesFold(strings.TrimSpace(v)), "status": "Active"}, "name", "name")
	}
}

// jobMasterNames are the active names of a master, in name order.
func jobMasterNames(ctx context.Context, q store.Q, kind string) []string {
	out, _ := column(ctx, q, "simple_master", "name", bson.D{{Key: "kind", Value: kind}, {Key: "status", Value: "Active"}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	return out
}

// jobMasterOptions is the drop-down of a Form setup field backed by a master (nil for any other field).
func jobMasterOptions(ctx context.Context, q store.Q, key string) *string {
	kind := jobMasterOfField[key]
	if kind == "" {
		return nil
	}
	s := strings.Join(jobMasterNames(ctx, q, kind), ",")
	return &s
}

// jobMasterErrors checks the offer's Role and Skill against the active masters (a blank one is left to the
// required check, so an older offer without them still reads).
func jobMasterErrors(ctx context.Context, q store.Q, d map[string]string, add func(string, ...any)) {
	for _, kind := range []string{store.KindJobRole, store.KindSkill} {
		v := strings.TrimSpace(d[jobMasterKey[kind]])
		if v == "" {
			continue
		}
		if _, _, ok := jobMasterLookup(kind)(ctx, q, v); !ok {
			add("%s “%s” is not an active %s in Masters", jobMasterLabel[kind], v, strings.ToLower(jobMasterLabel[kind]))
		}
	}
}

// jobMasterJob is the job role and skill an offer gives the employee record (null when not given).
func jobMasterJob(ctx context.Context, q store.Q, d map[string]string) bson.D {
	out := bson.D{}
	for _, kind := range []string{store.KindJobRole, store.KindSkill} {
		var v any
		if _, name, ok := jobMasterLookup(kind)(ctx, q, d[jobMasterKey[kind]]); ok {
			v = name
		} else if x := strings.TrimSpace(d[jobMasterKey[kind]]); x != "" {
			v = x
		}
		out = append(out, bson.E{Key: jobMasterCol[kind], Value: v})
	}
	return out
}

// importJobMasters reads the Role and Skill columns of an employee import: both required (18 Nov 2026, like the
// department) and each an active master.
func importJobMasters(ctx context.Context, q store.Q, role, skill string, add func(string, ...any)) (any, any) {
	var out [2]any
	for i, x := range [][2]string{{store.KindJobRole, role}, {store.KindSkill, skill}} {
		v := strings.TrimSpace(x[1])
		if v == "" {
			add("%s is required", jobMasterLabel[x[0]])
			continue
		}
		if _, name, ok := jobMasterLookup(x[0])(ctx, q, v); ok {
			out[i] = name
		} else {
			add("%s “%s” is not an active %s in Masters", jobMasterLabel[x[0]], v, strings.ToLower(jobMasterLabel[x[0]]))
		}
	}
	return out[0], out[1]
}

// jobMasterText is a stored name for display ("" for null).
func jobMasterText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
