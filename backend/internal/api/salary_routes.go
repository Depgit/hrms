package api

import (
	"context"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

func (s *Server) structures(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 1 && c.Level("onb.raise") < 2 && c.Level("pay.run") < 1 {
		return nil, forbidden("")
	}
	return s.st.DB.C("salary_structure").Find(r.Context(), nil, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
}

// salaryPreview is the salary calculator: HR enters gross only; the breakup comes from the structure.
func (s *Server) salaryPreview(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.raise") < 2 && c.Level("comp.bands") < 1 && c.Level("pay.run") < 1 {
		return nil, forbidden("")
	}
	var b struct {
		Type        string   `json:"type"`
		LocationID  string   `json:"locationId"`
		Grade       string   `json:"grade"`
		Gross       float64  `json:"gross"`
		StructureID string   `json:"structureId"`
		Month       int      `json:"month"`
		JoinMonth   int      `json:"joinMonth"`
		VarPct      *float64 `json:"varPct"`
		VarCycle    *int     `json:"varCycle"`
		Meal        float64  `json:"meal"`
		Accom       float64  `json:"accom"`
		Inc         float64  `json:"inc"`
		VarAmt      *float64 `json:"varAmt"`  // variable pay as a target: ₹ a month (varMode "amount")
		VarMode     string   `json:"varMode"` // "pct": the target is varPct % of the fixed gross
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	t := calc.EmploymentType(b.Type)
	if _, ok := calc.DefaultTypeRules[t]; !ok {
		return nil, bad("Employment type must be Permanent, Retainer or Contractual")
	}
	if b.Gross <= 0 {
		return nil, bad("Enter the monthly gross")
	}
	// Deductions and income are never negative, as an employee's pay options refuse (SET-038).
	if b.Meal < 0 || b.Accom < 0 || b.Inc < 0 || (b.VarPct != nil && (*b.VarPct < 0 || *b.VarPct > 100)) {
		return nil, bad("Meal, accommodation and other income cannot be negative, and variable pay is 0 to 100%%")
	}
	ctx := r.Context()
	structs := s.st.DB.C("salary_structure")
	st := calc.DefaultStructure // the rates the structure sets replace these
	if b.StructureID != "" {
		x, err := structs.FindOne(ctx, bson.M{"_id": b.StructureID})
		if err != nil || x == nil || payengineJSON(x["rates"], &st) != nil {
			return nil, bad("Unknown salary structure")
		}
	} else if x, _ := structs.FindOne(ctx, bson.M{"is_default": true}); x != nil {
		_ = payengineJSON(x["rates"], &st)
	}
	var lwf *calc.LwfRule
	if b.LocationID != "" {
		lwf = lwfFor(ctx, s.st.DB, b.LocationID, today())
	}
	in := calc.Input{Type: t, Gross: b.Gross, Structure: &st, Lwf: lwf, Month: b.Month, JoinMonth: b.JoinMonth, VarPct: b.VarPct, VarCycle: b.VarCycle, Meal: b.Meal, Accom: b.Accom, Inc: b.Inc}
	switch {
	case st.Designed && b.VarMode == "pct" && b.VarPct != nil:
		in.VarTargetPct = b.VarPct
	case st.Designed && b.VarAmt != nil && *b.VarAmt >= 0:
		in.VarTarget, in.CTC = b.VarAmt, b.Gross+*b.VarAmt
	}
	res := calc.Calc(in)
	out := map[string]any{"breakup": res, "ctcYear": calc.Round2(res.CTC * 12), "band": nil, "lwfNote": lwfNoteText(lwf), "rules": calc.RulesFor(t)}
	if st.Designed { // the labour-code wage check, for a structure made from components (paycomp.go)
		out["wage"] = wageNote(res)
	}
	if b.LocationID != "" && b.Grade != "" {
		if band := salaryRoutesBand(ctx, s.st.DB, b.LocationID, b.Grade); band != nil {
			pos := "within"
			if b.Gross < band["min_gross"].(float64) {
				pos = "below"
			} else if b.Gross > band["max_gross"].(float64) {
				pos = "above"
			}
			band["position"] = pos
			out["band"] = band
		}
	}
	return out, nil
}

// salaryRoutesBand is the pay band of a grade in the city of a location ({min_gross, max_gross}), nil when there is none.
func salaryRoutesBand(ctx context.Context, q store.Q, locationID, grade string) store.Row {
	loc, _ := q.C("location").FindOne(ctx, bson.M{"_id": locationID})
	if loc == nil {
		return nil
	}
	cluster, _ := q.C("cluster").FindOne(ctx, bson.M{"_id": loc["cluster_id"]})
	if cluster == nil {
		return nil
	}
	band, _ := q.C("pay_band").FindOne(ctx, bson.M{"city_id": cluster["city_id"], "grade": grade},
		options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "min_gross", Value: 1}, {Key: "max_gross", Value: 1}}))
	return band
}

// dashboard returns headcount and pending counts within the caller's scope.
func (s *Server) dashboard(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	qv := r.URL.Query()
	// Who the dashboard counts: the caller's scope (or only the caller) and the City, Department and Type filters.
	sc, err := s.dashScopeOf(ctx, c, qv.Get("city"), qv.Get("department"), qv.Get("type"))
	if err != nil {
		return nil, err
	}
	switch qv.Get("tab") {
	case "wf":
		return s.dashWorkforce(ctx, sc)
	case "ap":
		return s.dashApprovals(ctx, c, sc)
	case "cmp":
		out, err := s.dashCompliance(ctx, sc)
		if m, ok := out.(map[string]any); ok && err == nil && c.Level("pay.run") >= 1 {
			// Statutory follow-ups: TDS deposits past the due date, payroll months not published.
			m["tdsOverdue"] = salaryRoutesTdsOverdue(ctx, s.st.DB)
			unpublished, _ := s.st.DB.C("payroll_month").Count(ctx, bson.M{"status": "Approved"})
			m["unpublished"] = unpublished
		}
		return out, err
	case "pay":
		if err := c.Need("pay.run", 1); err != nil {
			return nil, err
		}
		f := map[string]string{"city": qv.Get("city"), "dept": qv.Get("department"), "type": qv.Get("type")}
		out, err := s.dashPayroll(ctx, c, f)
		if m, ok := out.(map[string]any); ok && err == nil {
			m["open"], err = s.dashPayOpen(ctx, c, f)
		}
		return out, err
	case "att":
		return s.dashAttendance(ctx, c, qv.Get("city"), qv.Get("department"), qv.Get("type"))
	}
	byCity, err := dashGroup(ctx, s.st.DB, sc, "city")
	if err != nil {
		return nil, err
	}
	byType, _ := dashGroup(ctx, s.st.DB, sc, "type")
	byDept, _ := dashGroup(ctx, s.st.DB, sc, "department")
	active, inactive, pre, _ := dashStatusCounts(ctx, s.st.DB, sc)
	byStatus, _, _, _ := dashRequests(ctx, s.st.DB, sc)
	pendingScoped := byStatus["Pending"]
	pending, _ := s.st.DB.C("approval_request").Find(ctx, bson.M{"status": "Pending"}, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	mine := 0
	for _, p := range pending {
		// Counted as the To approve inbox counts: a policy acknowledgement only for the person named on it.
		if req, err := loadRequest(ctx, s.st.DB, p["id"].(string), false); err == nil && canAct(ctx, s.st.DB, c, req) &&
			(req.WF != "p_ack" || ackNamed(ctx, s.st.DB, c, req)) {
			mine++
		}
	}
	recent := s.dashRecent(ctx, c)
	ov, err := s.dashOverview(ctx, c, sc, mine)
	if err != nil {
		return nil, err
	}
	ov["active"], ov["inactive"], ov["preboarding"], ov["pendingAll"], ov["pendingMine"] = active, inactive, pre, pendingScoped, mine
	ov["byCity"], ov["byType"], ov["byDept"], ov["recent"] = byCity, byType, byDept, recent
	ov["payroll"] = s.dashPayStatus(ctx)
	return ov, nil
}

// salaryRoutesTdsOverdue counts the TDS deposits past their due date: each month and section with TDS deducted
// (section 192 is deposited as salary TDS "s", the rest as fees "f") with no deposit recorded, due on 30 April
// for March and on the 7th of the next month otherwise.
func salaryRoutesTdsOverdue(ctx context.Context, q store.Q) int64 {
	deducted, _ := q.C("tds_history").Aggregate(ctx, bson.A{bson.M{"$group": bson.M{"_id": bson.M{"ym": "$ym", "section": "$section"}}}})
	deposits, _ := q.C("tds_deposit").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "kind", Value: 1}, {Key: "ym", Value: 1}}))
	deposited := map[string]bool{}
	for _, d := range deposits {
		deposited[str(d["kind"])+"|"+str(d["ym"])] = true
	}
	today := store.Today()
	var n int64
	for _, t := range deducted {
		k, _ := t["id"].(map[string]any)
		ym, kind := str(k["ym"]), "f"
		if str(k["section"]) == "192" {
			kind = "s"
		}
		if deposited[kind+"|"+ym] || len(ym) != 7 {
			continue
		}
		due := nextYm(ym) + "-07"
		if ym[5:] == "03" {
			due = ym[:4] + "-04-30"
		}
		if due < today {
			n++
		}
	}
	return n
}
