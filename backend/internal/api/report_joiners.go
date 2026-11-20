package api

// Reports › New joiners (test sheet #14): the month's joiners with what the medical insurer and the EPF and ESIC
// portals need, one row per person: personal details, IDs, bank, monthly gross, and the PF nominees and medical
// family members in repeated columns (PF nominee 1, PF nominee 1 relation, …, as many as the most anyone has).
// Confidential columns follow the viewer's rights and Full / Masked / Hidden modes like every report: IDs as
// govId, bank account and IFSC as bank, the gross as salary, nominee names and dates of birth as medical (as the
// nominations page masks them), personal details blank without the personal details right.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// repJoinerMaxPeople is the most people a nomination can hold (nomination_scheme.max_people).
const repJoinerMaxPeople = 10

// repJoinerMask is the joiners report's confidential columns by group.
func repJoinerMask() map[string]string {
	m := map[string]string{"Monthly gross": "salary", "Aadhaar": "govId", "PAN": "govId", "UAN": "govId", "ESIC number": "govId",
		"Bank account": "bank", "IFSC": "bank", "Father or spouse name": "personal", "Date of birth": "personal", "Mobile": "personal", "Email": "personal"}
	for i := 1; i <= repJoinerMaxPeople; i++ {
		m[fmt.Sprintf("PF nominee %d", i)] = "medical"
		m[fmt.Sprintf("PF nominee %d date of birth", i)] = "medical"
		m[fmt.Sprintf("Medical member %d", i)] = "medical"
		m[fmt.Sprintf("Medical member %d date of birth", i)] = "medical"
	}
	return m
}

// repJoiners: joiners in the range, by date of joining; the monthly gross only for a role with the salary right
// (prototype salOk).
func repJoiners(s *Server, ctx context.Context, c *Ctx, f repFilter) ([]string, [][]any, error) {
	cols := []string{"Code", "Name", "Type", "Date of joining", "City", "Location", "Department", "Designation", "Grade", "Reporting manager",
		"Father or spouse name", "Date of birth", "Gender", "Mobile", "Email", "Aadhaar", "PAN", "UAN", "Existing UAN?", "ESIC number",
		"Bank name", "Bank account", "IFSC"}
	withGross := c.Level("emp.salary") >= 1
	if withGross {
		cols = append(cols, "Monthly gross")
	}
	q := s.st.DB
	list, err := repJoin(ctx, q, c, f, bson.D{{Key: "doj", Value: bson.D{{Key: "$gte", Value: f.From}, {Key: "$lte", Value: f.To}}}})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return str(list[i].E["doj"]) < str(list[j].E["doj"]) })
	mgr, err := repManagers(ctx, q, list)
	if err != nil {
		return nil, nil, err
	}
	gross := map[string]any{}
	if withGross && len(list) > 0 {
		if gross, err = repMonthlyGross(ctx, q, list); err != nil {
			return nil, nil, err
		}
	}
	ids := repEmpIDs(list)
	one := func(coll string) (map[string]store.Row, error) {
		out := map[string]store.Row{}
		if len(ids) == 0 {
			return out, nil
		}
		rows, err := q.C(coll).Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}})
		for _, r := range rows {
			out[str(r["employee_id"])] = r
		}
		return out, err
	}
	pers, err := one("employee_personal")
	if err != nil {
		return nil, nil, err
	}
	gov, err := one("employee_gov_id")
	if err != nil {
		return nil, nil, err
	}
	bank, err := repJoinerBanks(ctx, q, ids)
	if err != nil {
		return nil, nil, err
	}
	pf, err := repJoinerPeople(ctx, q, ids, "provident|epf")
	if err != nil {
		return nil, nil, err
	}
	med, err := repJoinerPeople(ctx, q, ids, "medical")
	if err != nil {
		return nil, nil, err
	}
	// One medical plan (owner, 19 Nov 2026): employee + spouse + up to 2 children. Without a medical nomination the
	// family on the personal details (spouse, 1st and 2nd child) are the members.
	for _, id := range ids {
		if len(med[id]) == 0 {
			med[id] = repJoinerFamily(pers[id])
		}
	}
	nPF, nMed := 1, 1
	for _, id := range ids {
		nPF, nMed = max(nPF, min(len(pf[id]), repJoinerMaxPeople)), max(nMed, min(len(med[id]), repJoinerMaxPeople))
	}
	for i := 1; i <= nPF; i++ {
		cols = append(cols, fmt.Sprintf("PF nominee %d", i), fmt.Sprintf("PF nominee %d relation", i), fmt.Sprintf("PF nominee %d date of birth", i), fmt.Sprintf("PF nominee %d share %%", i))
	}
	for i := 1; i <= nMed; i++ {
		cols = append(cols, fmt.Sprintf("Medical member %d", i), fmt.Sprintf("Medical member %d relation", i), fmt.Sprintf("Medical member %d date of birth", i))
	}
	blank := func(v any) any {
		if v == nil || str(v) == "" {
			return nil
		}
		return v
	}
	out := [][]any{}
	for _, x := range list {
		e, id := x.E, x.id()
		p, g, b := pers[id], gov[id], bank[id]
		row := []any{e["id"], e["name"], e["type"], e["doj"], x.City, x.Loc, x.Dept, x.Desig, e["grade"], mgr[id],
			blank(p["father_spouse"]), blank(cell(p["dob"])), blank(p["gender"]), blank(p["mobile"]), blank(p["email"]),
			blank(g["aadhaar"]), blank(g["pan"]), blank(statIDClean(str(g["uan"]))), blank(yesNo(e["existing_uan"])), blank(statIDClean(str(g["esic"]))),
			blank(b["bank_name"]), blank(b["account_no"]), blank(b["ifsc"])}
		if withGross {
			row = append(row, gross[id])
		}
		for i := 0; i < nPF; i++ {
			if i < len(pf[id]) {
				n := pf[id][i]
				row = append(row, blank(n["name"]), blank(n["rel"]), blank(n["dob"]), n["share"])
			} else {
				row = append(row, nil, nil, nil, nil)
			}
		}
		for i := 0; i < nMed; i++ {
			if i < len(med[id]) {
				n := med[id][i]
				row = append(row, blank(n["name"]), blank(n["rel"]), blank(n["dob"]))
			} else {
				row = append(row, nil, nil, nil)
			}
		}
		out = append(out, row)
	}
	return cols, out, nil
}

// repJoinerFamily is the spouse and children on a person's personal details, as medical members (name, rel).
func repJoinerFamily(p store.Row) []map[string]any {
	out := []map[string]any{}
	for _, x := range [][2]string{{"spouse", "Spouse"}, {"child1", "Child"}, {"child2", "Child"}} {
		if v := strings.TrimSpace(str(p[x[0]])); v != "" {
			out = append(out, map[string]any{"name": v, "rel": x[1]})
		}
	}
	return out
}

// repJoinerBanks is each person's bank account in force this month (the latest that started by now, else the
// first one still to start).
func repJoinerBanks(ctx context.Context, q store.Q, ids []string) (map[string]store.Row, error) {
	out := map[string]store.Row{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.C("employee_bank").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}, {Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	now := today()[:7]
	for _, r := range rows {
		// Latest first: an account still to start gives way to the next one down.
		if cur, ok := out[str(r["employee_id"])]; !ok || str(cur["from_month"]) > now {
			out[str(r["employee_id"])] = r
		}
	}
	return out, nil
}

// repJoinerPeople are the people of each person's latest nomination (not returned) under the schemes whose name
// matches pattern: name, rel, dob, share.
func repJoinerPeople(ctx context.Context, q store.Q, ids []string, pattern string) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	if len(ids) == 0 {
		return out, nil
	}
	schemes, err := q.C("nomination_scheme").Distinct(ctx, "_id", bson.M{"name": bson.M{"$regex": pattern, "$options": "i"}})
	if err != nil || len(schemes) == 0 {
		return out, err
	}
	rows, err := q.C("nomination").Find(ctx, bson.D{{Key: "employee_id", Value: repIn(ids)}, {Key: "scheme_id", Value: bson.M{"$in": schemes}},
		{Key: "status", Value: bson.M{"$ne": "Returned"}}}, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := str(r["employee_id"])
		if _, done := out[id]; done {
			continue
		}
		list := []map[string]any{}
		if l, ok := r["people"].([]any); ok {
			for _, x := range l {
				if m, ok := x.(map[string]any); ok {
					list = append(list, m)
				}
			}
		}
		out[id] = list
	}
	return out, nil
}
