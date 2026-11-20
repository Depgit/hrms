package api

import (
	"context"
	"math"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Form 16 (spec 16): Part A (employer, PAN and TAN, period, quarterly receipts, challans) and
// Part B (the computation), for permanent staff. Provisional during the year, final after the
// March run and the Q4 filing; released to employees by a payroll approver.

func (s *Server) form16Data(ctx context.Context, pc *payCtx, e *emp, fy string) (map[string]any, error) {
	f := pc.figureFor(e, fy)
	if f == nil {
		return nil, nil
	}
	q := s.st.DB
	d := pc.tax.declOf(e.ID)
	old := d.Regime == "Old"
	co, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1},
		options.FindOne().SetProjection(compFields("legal_name", "address", "tan", "pan", "responsible_name", "responsible_desig")))
	if co != nil {
		co["place"] = companyPlace(ctx, q, str(co["address"])) // "Place: Gurugram · Date: …"
	}
	g, _ := q.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": e.ID}, options.FindOne().SetProjection(compFields("pan")))
	pan := str(g["pan"])
	paid, err := form16Paid(ctx, q, e.ID, f.First, f.Last)
	if err != nil {
		return nil, err
	}
	rets, _ := q.C("tds_return").Find(ctx, bson.M{"fy": fy, "form": "24Q"}, options.Find().SetProjection(compFields("quarter", "receipt")))
	deps, _ := q.C("tds_deposit").Find(ctx, bson.M{"kind": "s"}, options.Find().SetProjection(compFields("ym", "bsr", "challan", "date")))
	dep := map[string]store.Row{}
	for _, x := range deps {
		dep[x["ym"].(string)] = x
	}
	quarters := []map[string]any{}
	challans := []map[string]any{}
	done := 0.0
	for k := 1; k <= 4; k++ {
		qr := map[string]any{"quarter": k, "paid": 0.0, "tds": 0.0, "receipt": ""}
		for _, rt := range rets {
			if rt["quarter"] == int32(k) {
				qr["receipt"] = rt["receipt"]
			}
		}
		for _, m := range calc.FYMonths(fy) {
			if calc.Quarter(m) != k {
				continue
			}
			qr["paid"] = qr["paid"].(float64) + paid[m]
			if t := pc.tax.hist[e.ID][m]; t > 0 {
				qr["tds"] = qr["tds"].(float64) + t
				done += t
				c := map[string]any{"ym": m, "tds": t}
				if x := dep[m]; x != nil {
					c["bsr"], c["challan"], c["date"] = x["bsr"], x["challan"], x["date"]
				}
				challans = append(challans, c)
			}
		}
		qr["paid"] = math.Round(qr["paid"].(float64))
		quarters = append(quarters, qr)
	}
	est := f.Est
	sd, taxable := 75000.0, est.NewTaxable
	if old {
		sd, taxable = 50000, est.OldTaxable
	}
	gross := math.Round(f.Gross * float64(f.Months))
	hra := 0.0
	if old {
		hra = math.Round(est.HraExempt)
	}
	vi := []map[string]any{}
	if old {
		for _, g := range []string{"80C", "80CCD(1B)", "80D", "80E", "80G", "80TTA", "24(b)", "LTA"} {
			v := f.Groups[g]
			if g == "80C" {
				v += f.PFYear
			}
			if c := calc.SectionCaps[g]; c > 0 {
				v = math.Min(v, c)
			}
			if v > 0 {
				vi = append(vi, map[string]any{"section": calc.TaxGroupNames[g], "amount": math.Round(v)})
			}
		}
	}
	base := math.Round(f.Annual / 1.04)
	months := []string{}
	for m := f.First; m <= f.Last; m = nextYm(m) {
		months = append(months, m)
	}
	return map[string]any{
		"cert": "F16-" + fy + "-" + e.ID, "fy": fy, "basis": f.Basis, "ay": itoa(atoiSafe(fy[:4])+1) + "-" + itoa((atoiSafe(fy[:4])+2)%100), "regime": d.Regime,
		"employer": co, "employee": map[string]any{"id": e.ID, "name": e.Name, "pan": pan, "desig": e.Desig, "location": e.Location},
		"period": []string{f.First, f.Last}, "quarters": quarters, "challans": challans,
		"partB": map[string]any{"gross": gross, "hra": hra, "standard": sd, "salaryIncome": math.Max(0, gross-hra-sd), "other": d.Other, "prevIncome": d.PrevIncome,
			"vi": vi, "viTotal": map[bool]float64{true: math.Round(est.OldDeductions), false: 0}[old], "taxable": math.Round(taxable), "tax": base,
			"cess": f.Annual - base, "total": f.Annual, "done": done, "prev": d.PrevTDS, "balance": f.Annual - d.PrevTDS - done},
		"months": months,
	}, nil
}

// form16Paid is the pay credited (total earnings) in each month from first to last whose payroll is approved.
func form16Paid(ctx context.Context, q store.Q, emp, first, last string) (map[string]float64, error) {
	months, err := q.C("payroll_month").Distinct(ctx, "ym", bson.M{"ym": bson.M{"$gte": first, "$lte": last}, "status": bson.M{"$in": bson.A{"Approved", "Published"}}})
	if err != nil {
		return nil, err
	}
	rows, err := q.C("payroll_row").Find(ctx, bson.M{"employee_id": emp, "ym": bson.M{"$in": months}}, options.Find().SetProjection(compFields("ym", "data.totalEarn")))
	if err != nil {
		return nil, err
	}
	paid := map[string]float64{}
	for _, x := range rows {
		data, _ := x["data"].(map[string]any)
		paid[str(x["ym"])], _ = data["totalEarn"].(float64) // 0 when the row has no total
	}
	return paid, nil
}

func (s *Server) form16Release(ctx context.Context, fy string) (bool, bool) {
	x, err := s.st.DB.C("form16_release").FindOne(ctx, bson.M{"fy": fy}, options.FindOne().SetProjection(compFields("final")))
	if err != nil || x == nil {
		return false, false
	}
	final, _ := x["final"].(bool)
	return true, final
}

func (s *Server) form16List(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	who, err := taxPermanentInScope(ctx, s.st.DB, c)
	if err != nil {
		return nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, openYm(ctx, s.st.DB), false, fyBase(fy), who)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range list {
		f := pc.figureFor(e, fy)
		if f == nil {
			continue
		}
		done := 0.0
		for _, v := range pc.tax.hist[e.ID] {
			done += v
		}
		d := pc.tax.declOf(e.ID)
		out = append(out, map[string]any{"id": e.ID, "name": e.Name, "regime": d.Regime, "annual": f.Annual, "done": done, "first": f.First, "last": f.Last,
			"prevTds": d.PrevTDS, "toDeduct": math.Max(0, f.Annual-done-d.PrevTDS), "basis": f.Basis})
	}
	rel, final := s.form16Release(ctx, fy)
	var by *string // "Name, 12 Apr 2027"
	if x, _ := s.st.DB.C("form16_release").FindOne(ctx, bson.M{"fy": fy}); x != nil && x["by_name"] != nil && x["at"] != nil {
		v := str(x["by_name"]) + ", " + compDay(x["at"]).(string)
		by = &v
	}
	return map[string]any{"fy": fy, "rows": out, "released": rel, "final": final, "releasedBy": by, "ready": companyTaxReady(ctx, s.st.DB),
		"canRelease": c.Level("pay.approve") >= 2, "canExport": c.Level("pay.export") >= 2}, nil
}

// form16Get returns one Form 16: the employee's own once released, or any in scope for payroll.
func (s *Server) form16Get(r *http.Request, c *Ctx) (any, error) {
	ctx, id := r.Context(), r.PathValue("id")
	if id == "me" {
		id = c.EmployeeID
	}
	fy := fyParam(ctx, s.st.DB, r)
	rel, final := s.form16Release(ctx, fy)
	if id == c.EmployeeID && c.Level("pay.run") < 1 {
		if !rel {
			return nil, notFound("Form 16 for " + fy + " is not released yet")
		}
	} else {
		if err := c.Need("pay.run", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, openYm(ctx, s.st.DB), false, fyBase(fy), bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("No Form 16 for this person in " + fy)
	}
	d, err := s.form16Data(ctx, pc, list[0], fy)
	if err != nil || d == nil {
		return nil, orNotFound(err)
	}
	d["released"], d["final"] = rel, final
	return d, nil
}

// form16All is every Form 16 in scope, for one printable file.
func (s *Server) form16All(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.export", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if !companyTaxReady(ctx, s.st.DB) {
		return nil, bad("Add the company TAN and PAN in Payslip setup first")
	}
	fy := fyParam(ctx, s.st.DB, r)
	who, err := taxPermanentInScope(ctx, s.st.DB, c)
	if err != nil {
		return nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, openYm(ctx, s.st.DB), false, fyBase(fy), who)
	if err != nil {
		return nil, err
	}
	rel, final := s.form16Release(ctx, fy)
	out := []map[string]any{}
	for _, e := range list {
		d, err := s.form16Data(ctx, pc, e, fy)
		if err != nil {
			return nil, err
		}
		if d != nil {
			d["released"], d["final"] = rel, final
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *Server) form16ReleasePost(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.approve", 2); err != nil {
		return nil, forbidden("Form 16 is released by a payroll approver")
	}
	var b struct {
		On *bool `json:"on"` // false withdraws a release (the switch turned off); default true
	}
	if err := decodeLoose(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	if b.On != nil && !*b.On {
		fy := fyParam(ctx, s.st.DB, r)
		return map[string]bool{"released": false, "final": false}, s.st.Tx(ctx, func(tx store.Q) error {
			if _, err := tx.C("form16_release").Delete(ctx, bson.M{"fy": fy}); err != nil {
				return err
			}
			return audit(ctx, tx, c, "Form 16 withdrawn from employees for "+fy, "form16_release", fy, nil, nil)
		})
	}
	if !companyTaxReady(ctx, s.st.DB) {
		return nil, bad("Add the company TAN and PAN in Payslip setup first")
	}
	fy := fyParam(ctx, s.st.DB, r)
	ms := calc.FYMonths(fy)
	// Final once March is paid and the Q4 24Q return is filed; provisional before that.
	var marchDone, q4 bool
	marchDone, _ = s.st.DB.C("payroll_month").Exists(ctx, bson.M{"ym": ms[11], "status": "Published"})
	q4, _ = s.st.DB.C("tds_return").Exists(ctx, bson.M{"fy": fy, "form": "24Q", "quarter": 4})
	final := marchDone && q4
	err := s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("form16_release").Upsert(ctx, bson.M{"fy": fy}, bson.M{"final": final, "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Form 16 released for "+fy, "form16_release", fy, nil, map[string]bool{"final": final})
	})
	return map[string]bool{"final": final, "released": true}, err
}

// myForm16 lists the years with a released Form 16 for the caller.
func (s *Server) myForm16(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	if !salaryTdsType(taxEmployeeType(ctx, s.st.DB, c.EmployeeID)) {
		return map[string]any{"applies": false, "years": []any{}}, nil
	}
	rel, err := s.st.DB.C("form16_release").Find(ctx, bson.M{}, options.Find().SetSort(compOrder("-fy")))
	if err != nil {
		return map[string]any{"applies": true, "years": []store.Row(nil)}, err
	}
	years := make([]store.Row, 0, len(rel))
	for _, x := range rel {
		years = append(years, store.Row{"fy": x["fy"], "final": x["final"], "at": compDay(x["at"])})
	}
	return map[string]any{"applies": true, "years": years}, nil
}
