package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Opening TDS (REAL-32, RUN-94): the salary TDS (Section 192) already deducted in the months of the financial year
// before the first payroll run on this system. It is loaded from an Excel or CSV file (Emp ID, Month, TDS deducted),
// checked first and then saved as tds_history rows marked "opening". The monthly TDS then spreads only the tax still
// due over the months left, instead of the whole year's tax. Months with an approved or published run, and the open
// month or later, are refused: their TDS comes from payroll.

var openingTdsCols = []string{"Emp ID", "Name", "Month", "TDS deducted"}

// openingMonth reads a month as 2026-04, Apr 2026, April 2026, 04/2026, a date (2026-04-30, 30/04/2026) or an Excel
// date serial.
func openingMonth(v string) (string, bool) {
	v = strings.TrimSpace(v)
	for _, f := range []string{"2006-01", "Jan 2006", "January 2006", "Jan-2006", "January-2006", "01/2006", "1/2006", "2006-01-02", "02/01/2006", "2/1/2006", "Jan-06", "01-02-2006"} {
		if t, err := time.Parse(f, v); err == nil {
			return t.Format("2006-01"), true
		}
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 20000 && n < 80000 { // an Excel date
		return time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n)).Format("2006-01"), true
	}
	return "", false
}

// openingLocked are the months whose TDS comes from payroll: those with an approved or published run.
func openingLocked(ctx context.Context, q store.Q) (map[string]bool, error) {
	ms, err := q.C("payroll_month").Distinct(ctx, "ym", bson.M{"status": bson.M{"$in": bson.A{"Approved", "Published"}}})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range ms {
		out[str(m)] = true
	}
	return out, nil
}

// tdsOpeningList is the opening TDS on record for the year, by person and month.
func (s *Server) tdsOpeningList(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	ms := calc.FYMonths(fy)
	rows, err := s.st.DB.C("tds_history").Find(ctx, bson.M{"source": "opening", "section": "192", "ym": bson.M{"$gte": ms[0], "$lte": ms[11]}},
		options.Find().SetSort(compOrder("employee_id", "ym")))
	if err != nil {
		return nil, err
	}
	names, err := openingNames(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	out, total := []map[string]any{}, 0.0
	for _, x := range rows {
		a, _ := x["amount"].(float64)
		total += a
		out = append(out, map[string]any{"id": x["employee_id"], "name": names[str(x["employee_id"])], "ym": x["ym"], "amount": a, "by": x["by_name"]})
	}
	return map[string]any{"fy": fy, "openYm": openYm(ctx, s.st.DB), "rows": out, "total": calc.Round2(total), "canEdit": c.Level("pay.run") >= 2}, nil
}

func openingNames(ctx context.Context, q store.Q) (map[string]string, error) {
	emps, err := q.C("employee").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("id", "name", "type", "status")))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range emps {
		out[str(e["id"])] = str(e["name"])
	}
	return out, nil
}

// tdsOpeningTemplate is the file to fill: one row per permanent employee and month of the year before the open
// payroll month that has no run, with the TDS on record (blank when none).
func (s *Server) tdsOpeningTemplate(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := c.Need("pay.run", 2); err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	open := openYm(ctx, s.st.DB)
	locked, err := openingLocked(ctx, s.st.DB)
	if err != nil {
		return "", nil, nil, err
	}
	emps, err := s.st.DB.C("employee").Find(ctx, bson.M{"type": "Permanent", "status": bson.M{"$ne": "Pre-boarding"}}, options.Find().SetSort(compOrder("_id")))
	if err != nil {
		return "", nil, nil, err
	}
	have := map[string]float64{}
	hist, _ := s.st.DB.C("tds_history").Find(ctx, bson.M{"section": "192", "source": "opening"})
	for _, h := range hist {
		have[str(h["employee_id"])+str(h["ym"])], _ = h["amount"].(float64)
	}
	out := [][]any{}
	for _, e := range emps {
		id := str(e["id"])
		for _, m := range calc.FYMonths(fy) {
			if m >= open || locked[m] || m < str(e["doj"])[:7] || (e["lwd"] != nil && str(e["lwd"]) != "" && m > str(e["lwd"])[:7]) {
				continue
			}
			var amt any = ""
			if v, ok := have[id+m]; ok {
				amt = v
			}
			out = append(out, []any{id, e["name"], m, amt})
		}
	}
	return "opening-tds-" + fy + ".xlsx", openingTdsCols, out, nil
}

// tdsOpeningUpload checks (action check) or saves (action apply) an opening TDS file. Nothing is saved while any
// row has a problem; a TDS of 0 removes an opening amount loaded earlier.
func (s *Server) tdsOpeningUpload(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 2); err != nil {
		return nil, err
	}
	apply := r.PathValue("action") == "apply"
	if !apply && r.PathValue("action") != "check" {
		return nil, notFound("")
	}
	sheet, err := readUpload(r)
	if err != nil {
		return nil, err
	}
	if len(sheet) < 2 {
		return nil, bad("The file has no rows. Download the template, fill in the TDS deducted and upload it")
	}
	col := map[string]int{}
	for i, h := range sheet[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, k := range []string{"emp id", "month", "tds deducted"} {
		if _, ok := col[k]; !ok {
			return nil, bad("The file needs the columns Emp ID, Month and TDS deducted. Download the template")
		}
	}
	cell := func(rec []string, k string) string {
		if i := col[k]; i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	ctx := r.Context()
	open := openYm(ctx, s.st.DB)
	locked, err := openingLocked(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	emps, err := s.st.DB.C("employee").Find(ctx, bson.M{}, options.Find().SetProjection(compFields("id", "name", "type", "doj")))
	if err != nil {
		return nil, err
	}
	byID := map[string]store.Row{}
	for _, e := range emps {
		byID[str(e["id"])] = e
	}
	type line struct {
		Line    int     `json:"line"`
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		YM      string  `json:"ym"`
		Amount  float64 `json:"amount"`
		Problem string  `json:"problem,omitempty"`
	}
	lines, seen, problems, total := []line{}, map[string]int{}, 0, 0.0
	for i, rec := range sheet[1:] {
		id, mv, av := cell(rec, "emp id"), cell(rec, "month"), cell(rec, "tds deducted")
		if id == "" && mv == "" && av == "" {
			continue
		}
		l := line{Line: i + 2, ID: access.NormaliseEmployeeID(id)}
		e := byID[l.ID]
		ym, okM := openingMonth(mv)
		l.YM = ym
		amt, errA := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(av, "₹"), ",", ""), 64)
		l.Amount = calc.Round2(amt)
		switch {
		case e == nil:
			l.Problem = fmt.Sprintf("Employee %s not found", id)
		case !okM:
			l.Problem = "Month must be like 2026-04 or Apr 2026"
		case av == "":
			l.Problem = "Give the TDS deducted (0 to remove an amount loaded earlier)"
		case errA != nil || amt < 0:
			l.Problem = "TDS deducted must be a number, 0 or more"
		case locked[ym]:
			l.Problem = ymLabel(ym) + " has a payroll run; its TDS comes from payroll"
		case ym >= open:
			l.Problem = "Only months before the open payroll month (" + ymLabel(open) + ") can have opening TDS"
		case seen[l.ID+ym] > 0:
			l.Problem = fmt.Sprintf("Also on line %d", seen[l.ID+ym])
		}
		if e != nil {
			l.Name = str(e["name"])
		}
		if l.Problem == "" && e != nil && str(e["doj"]) != "" && ym < str(e["doj"])[:7] {
			l.Problem = "Before the joining date (" + str(e["doj"]) + ")"
		}
		if l.Problem != "" {
			problems++
		} else {
			seen[l.ID+ym] = l.Line
			total += l.Amount
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil, bad("The file has no rows. Download the template, fill in the TDS deducted and upload it")
	}
	out := map[string]any{"rows": lines, "ok": len(lines) - problems, "problems": problems, "total": calc.Round2(total), "applied": false}
	if !apply {
		return out, nil
	}
	if problems > 0 {
		return nil, bad("Fix the %d row%s with problems and upload again. Nothing was saved", problems, map[bool]string{true: "", false: "s"}[problems == 1])
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		for _, l := range lines {
			key := bson.M{"employee_id": l.ID, "ym": l.YM, "section": "192"}
			if l.Amount == 0 {
				if _, err := tx.C("tds_history").DeleteMany(ctx, bson.M{"employee_id": l.ID, "ym": l.YM, "section": "192", "source": "opening"}); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.C("tds_history").Upsert(ctx, key, bson.M{"amount": l.Amount, "source": "opening", "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, "Opening TDS loaded", "tds_history", "opening", nil, map[string]any{"rows": len(lines), "total": calc.Round2(total)})
	})
	if err != nil {
		return nil, err
	}
	out["applied"] = true
	return out, nil
}
