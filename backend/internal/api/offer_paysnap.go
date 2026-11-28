package api

// The offer's breakup as it was (owner, 27 Nov 2026): HR opens an offer version (or its line on the timeline) later
// and sees the breakup decided for that gross: Basic, HRA, allowances, deductions, employer cost, CTC. Each version
// keeps it in offer_version.pay, written when the version is raised and again when it is sent (what the candidate
// got). Versions from before carry none: they are worked out again from the version's own gross and details, marked
// "worked out again", because the structure or rates may have changed since.

import (
	"context"
	"encoding/json"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// offerPaySnap works out the breakup of an offer (type, gross, location, joining date, data) with the structure in
// use, as a plain map for offer_version.pay.
func offerPaySnap(ctx context.Context, q store.Q, o store.Row) map[string]any {
	d := rowData(o)
	if m, ok := o["data"].(map[string]string); ok { // an offer being raised: the cleaned form data
		d = m
	}
	gross, _ := o["gross"].(float64)
	j := offerJob{Type: str(o["type"]), Gross: gross, LocID: str(o["location_id"]), DOJ: str(o["doj"])}
	if j.LocID == "" {
		j.LocID = d["loc"]
	}
	res := offerPay(ctx, q, j, d)
	which := bson.D{{Key: "is_default", Value: true}}
	if d["structId"] != "" {
		which = bson.D{{Key: "_id", Value: d["structId"]}}
	}
	out := map[string]any{"breakup": res, "ctcYear": calc.Round2(res.CTC * 12), "rules": calc.RulesFor(calc.EmploymentType(j.Type)),
		"at": time.Now().UTC().Format(time.RFC3339)}
	if x, _ := q.C("salary_structure").FindOne(ctx, which); x != nil {
		out["rates"], out["structure"] = x["rates"], x["name"]
	}
	b, _ := json.Marshal(out)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// offerVersionPay is a version's breakup: the one kept, else worked out again (live: true) from its own details.
func offerVersionPay(ctx context.Context, q store.Q, v store.Row, offerLoc string) map[string]any {
	if p, ok := v["pay"].(map[string]any); ok && p["breakup"] != nil {
		return p
	}
	gross, _ := v["gross"].(float64)
	if gross <= 0 || str(v["type"]) == "" {
		return nil
	}
	p := offerPaySnap(ctx, q, store.Row{"type": v["type"], "gross": gross, "doj": v["doj"], "data": v["data"], "location_id": offerLoc})
	p["live"] = true
	return p
}
