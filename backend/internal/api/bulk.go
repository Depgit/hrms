package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
	"github.com/xuri/excelize/v2"
)

// Bulk update (spec 9): download a template with drop-downs → fill offline → upload Excel or CSV →
// every row is checked and shows old → new → apply. Valid rows apply; rows with problems are listed.

type bulkTemplate struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Cols  []string `json:"cols"`
	Hint  string   `json:"hint"`
	Data  bool     `json:"data"` // "Download current data" is offered
	allow func(c *Ctx) bool
	lists map[string]bulkList // column → the drop-down values
	row   func(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error)

	// Optional columns: in the template, but a file without them is still read (columns added later).
	Optional []string `json:"optional,omitempty"`
	// Zip: the upload is a ZIP of files, not a sheet (ESIC and medical cards, b1_bulk.go).
	Zip bool `json:"zip,omitempty"`

	// Note: a line worked out for the caller when the list is read (the opening templates give the HRMS's first
	// deduction month); note makes it.
	Note string `json:"note,omitempty"`
	note func(ctx context.Context, q store.Q) string
	// how: the lines of the template's "How to fill" sheet after the hint (columns, an example, what happens).
	how func(ctx context.Context, q store.Q) []string
	// current: the template's "current data" rows, for a template outside bulkCurrent's list.
	current func(ctx context.Context, s *Server, c *Ctx) ([]string, [][]any, error)
	// key: what makes two rows of a file the same row (the later one is refused), for a template outside bulkRun's
	// list (bulk_polmap.go: the Department and Location pair).
	key func(v map[string]string) string
	// fill: "Download template" already carries the current rows (bulk_apprnames.go: the missing names to fill in).
	fill bool
}

// bulkList reads the values of a template column's drop-down.
type bulkList func(ctx context.Context, q store.Q) ([]string, error)

// bulkFixed is a drop-down of fixed values.
func bulkFixed(vals ...string) bulkList {
	return func(context.Context, store.Q) ([]string, error) { return vals, nil }
}

// bulkQuery is a drop-down of one field of the matching documents, in the order sort gives, then the extra values.
func bulkQuery(coll, field string, filter bson.D, sort bson.D, extra ...string) bulkList {
	return func(ctx context.Context, q store.Q) ([]string, error) {
		o := options.Find()
		if sort != nil {
			o.SetSort(sort)
		}
		vals, err := column(ctx, q, coll, field, filter, o)
		return append(vals, extra...), err
	}
}

// bulkActive is the names (or another field) of a master's active entries, in order.
func bulkActive(coll, field string) bulkList {
	return bulkQuery(coll, field, bson.D{{Key: "status", Value: "Active"}}, bson.D{{Key: field, Value: 1}})
}

// bulkDistinct is the distinct values of a field of the matching documents, in order.
func bulkDistinct(coll, field string, filter bson.D) bulkList {
	return func(ctx context.Context, q store.Q) ([]string, error) {
		vals, err := q.C(coll).Distinct(ctx, field, filter)
		if err != nil {
			return nil, err
		}
		out := []string{}
		for _, v := range vals {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out, nil
	}
}

// bulkMaster is the active entries of a simple master kind (and use), by name.
func bulkMaster(kind, usedFor string) bulkList {
	f := bson.D{{Key: "kind", Value: kind}}
	if usedFor != "" {
		f = append(f, bson.E{Key: "used_for", Value: usedFor})
	}
	return bulkQuery("simple_master", "name", append(f, bson.E{Key: "status", Value: "Active"}), bson.D{{Key: "name", Value: 1}})
}

// bulkReasons is the active change reasons for a use (Employee update, Separation, Salary), by name.
func bulkReasons(usedFor string) bulkList { return bulkMaster("change_reason", usedFor) }

type bulkRow struct {
	Line int
	V    map[string]string
}

type bulkChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
	Note  string `json:"note,omitempty"`
}

type bulkResult struct {
	Line    int          `json:"line"`
	ID      string       `json:"id"`
	Name    string       `json:"name,omitempty"`
	Errors  []string     `json:"errors"`
	Changes []bulkChange `json:"changes"`
	// The row's Effective From and Reason for change, for every change in it (prototype chgCheck c.eff, c.reason).
	Eff    string `json:"eff,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var bulkTemplates = []*bulkTemplate{
	{Key: "upd", Name: "Employee master update",
		Cols:  []string{"Employee ID", "Work City", "Reporting Location", "Department", "Designation", "Grade", "Shift", "Reporting Manager L1", "Functional Manager L2", "Work Policy", "Attendance Mode", "Reason for change", "Effective From"},
		Hint:  "Blank cells keep the current value. Every row that changes something needs a Reason for change and an Effective From date. A past date is recorded as back-dated; a future date is scheduled and applies by itself on that day. Values must match an active master. If the department changes, give the designation too. If the city changes, give the location too. Managers are given by Employee ID.",
		allow: func(c *Ctx) bool { return c.Level("bulk.update") >= 2 && c.Level("emp.directory") >= 2 },
		lists: map[string]bulkList{"Work City": bulkActive("city", "name"), "Reporting Location": bulkActive("location", "name"),
			"Department": bulkActive("department", "name"), "Designation": bulkDistinct("designation", "name", bson.D{{Key: "status", Value: "Active"}}),
			"Grade": bulkActive("grade", "name"), "Shift": bulkActive("shift", "code"),
			"Work Policy":     bulkQuery("work_policy", "name", bson.D{{Key: "status", Value: "Active"}}, bson.D{{Key: "sort", Value: 1}, {Key: "name", Value: 1}}),
			"Attendance Mode": bulkMaster("attendance_mode", ""), "Reason for change": bulkReasons("Employee update")},
		row: bulkUpd},
	{Key: "pers", Name: "Personal details", Data: true,
		Cols:  []string{"Employee ID", "Mobile", "Personal Email", "Date of Birth", "Gender", "Address", "Father or Spouse Name", "Marital Status", "Emergency Contact Name", "Emergency Contact Relation", "Emergency Contact Mobile"},
		Hint:  "Corrects personal details. Blank cells keep the current value. Mobile and emergency contact mobile are 10 digits; the sign-in OTP goes to the mobile. Date of Birth is a date like 1990-04-21. An emergency contact needs a name, a relation and a different number. Each change is kept in the employee's change history.",
		allow: func(c *Ctx) bool { return c.Level("bulk.update") >= 2 && c.Level("emp.personal") >= 2 },
		lists: map[string]bulkList{"Gender": bulkFixed("Male", "Female", "Other", "Prefer not to say"), "Marital Status": bulkFixed("Single", "Married", "Other"),
			"Emergency Contact Relation": bulkFixed("Spouse", "Father", "Mother", "Son", "Daughter", "Brother", "Sister", "Friend", "Other relative")},
		row: bulkPers},
	{Key: "off", Name: "Mark inactive (separation)", Cols: []string{"Employee ID", "Last Working Day", "Reason"},
		Hint:  "Last Working Day is the date the employee becomes inactive. A future date puts the employee on notice until then. Reason must be one of the separation reasons in Masters. Employees are never deleted.",
		allow: func(c *Ctx) bool { return c.Level("bulk.update") >= 2 && c.Level("emp.status") >= 2 },
		lists: map[string]bulkList{"Reason": bulkReasons("Separation")}, row: bulkOff},
	{Key: "sal", Name: "Salary breakup", Data: true,
		Cols: []string{"Employee ID", "Monthly Gross", "Variable Pay %", "Variable Payout Cycle", "Meal Deduction", "Accommodation Deduction", "Salary Structure", "Structure Effective From", "Reason for change", "Effective From",
			salTotalCol, salTargetCol, salModeCol, reimbCol},
		Optional: []string{salTotalCol, salTargetCol, salModeCol, reimbCol},
		Hint:     "Every changed row needs a Reason for change and an Effective From date. " + salCompHint + " A gross change applies from that date: a date after the 1st pays that month split by days (the old gross before it, the new one from it); a past month raises an arrear (or a recovery) for approval, a future month is scheduled. Download the current data first. The calculated columns in the download are for reference and are ignored on upload. Blank cells keep the current value. Variable pay applies to types that earn it; meal and accommodation deductions apply to permanent staff only. Reimbursement (₹) is the Reimbursement a month OUTSIDE the gross, paid on top of it like variable pay (earned by the days paid, not in the PF, ESI or bonus wage); 0 removes it.",
		allow:    func(c *Ctx) bool { return c.Level("bulk.update") >= 2 && c.Level("emp.salary") >= 2 },
		lists: map[string]bulkList{"Variable Payout Cycle": bulkFixed("1", "3", "6", "12"), salModeCol: bulkFixed("%", "₹"), "Salary Structure": bulkActive("salary_structure", "name"),
			"Reason for change": bulkReasons("Salary")},
		row: bulkSal},
	{Key: "gross", Name: "Gross revision", Data: true, Cols: []string{"Employee ID", "New Gross", "Effective From"},
		Hint:  "New Gross applies from Effective From (a date like 2026-11-15 or a month like 2026-11, meaning its 1st; blank means this month). A date after the 1st pays that month split by days: the old gross before it, the new one from it. A past month raises an arrear for the difference, or a recovery if the gross goes down, for approval.",
		allow: func(c *Ctx) bool { return c.Level("bulk.update") >= 2 && c.Level("emp.salary") >= 2 }, row: bulkGross},
	{Key: "band", Name: "Pay bands", Data: true, Cols: []string{"City", "Grade", "Minimum Gross", "Maximum Gross"},
		Hint:  "One row for each city and grade. Download the current bands, change the minimum and maximum, and upload. Blank cells keep the current value. The check shows how many employees fall outside the new band.",
		allow: func(c *Ctx) bool { return c.Level("comp.bands") >= 2 },
		lists: map[string]bulkList{"City": bulkActive("city", "name"), "Grade": bulkActive("grade", "name")},
		row:   bulkBand},
	{Key: "att", Name: "Attendance upload", Cols: []string{"Employee ID", "Date", "In time", "Out time"},
		Hint:  "Use this until the punch machines are connected. One row per employee per day, times as HH:MM. Leave a time blank if it is missing. A row for a day that already has a record replaces it, except a day corrected by an approved regularization, which is kept. An out time earlier than the in time is read as the next morning (night shift).",
		allow: func(c *Ctx) bool { return c.Level("att.records") >= 2 }, row: bulkAtt},
	{Key: "roster", Name: "Roster upload", Cols: []string{"Employee ID", "Date", "Code", "Reason"},
		Hint:  "One row per employee and date. Code is a shift code, WO for a weekly off or PH for a holiday. The same rules as the roster page apply: past dates are locked, changes inside the lead days need a reason, and the rest gap between shifts is checked.",
		allow: func(c *Ctx) bool { return c.Level("ros.roster") >= 2 },
		lists: map[string]bulkList{"Code": bulkQuery("shift", "code", bson.D{{Key: "status", Value: "Active"}}, nil, "WO", "PH"),
			"Reason": bulkFixed("Guest demand or occupancy", "Staff absent or sick", "Employee request", "Event or banquet", "Vacancy cover", "Other")},
		row: bulkRoster},
	{Key: "lvopen", Name: "Opening leave balances", Cols: []string{"Employee ID", "Leave type", "Balance", "As at", optHolCol}, Optional: []string{optHolCol},
		Hint: "The balance an employee has on the As at date, for leave types with a balance, and the optional holidays still to take that calendar year. Give the balance as at the date, including all leave accrued up to that month. From the next month, leave keeps accruing by the leave plan (e.g. CL 12 a year adds 1 a month). Leave taken before the As at date is not counted again. Loading the same employee, type and date again replaces the balance; a later As at date takes over from an earlier one. " +
			"Optional Holiday Balance is what is left of the As at date's calendar year of the optional holidays the work policy gives (0 to 31, halves allowed); from the next calendar year the work policy's number applies again. A blank cell leaves that balance as it is: a row may give a leave type and balance, an optional holiday balance, or both.",
		how:   lvOpenHow,
		allow: func(c *Ctx) bool { return c.Level("pol.policies") >= 2 },
		lists: map[string]bulkList{"Leave type": bulkQuery("leave_type", "name", bson.D{{Key: "kind", Value: "balance"}, {Key: "status", Value: "Active"}}, bson.D{{Key: "sort", Value: 1}})}, row: bulkOpening},
	{Key: "bud", Name: "Budgets", Data: true, Cols: []string{"Level", "Name", "Headcount Budget", budCostCol},
		Hint:  "One row for each city, cluster or location. Level is City, Cluster or Location, and Name is its name. City rows take a headcount budget only; a city's cost budget is the total of its locations. Monthly Cost Budget (CTC) is the total monthly cost to the company: salary (without the meal and accommodation minimum-wage top-ups) plus the employer's PF, ESI and other contributions, as Cost and budget counts it. A cluster with 0 (or never set) uses the total of its locations. Blank cells keep the current value. 0 means no budget. A file with the old column name Monthly Payroll Budget is still read.",
		allow: func(c *Ctx) bool { return c.Level("comp.budget") >= 2 || c.Level("cost.budget") >= 2 },
		lists: map[string]bulkList{"Level": bulkFixed("City", "Cluster", "Location")}, row: bulkBud},
}

func templateFor(c *Ctx, key string) (*bulkTemplate, error) {
	for _, t := range bulkTemplates {
		if t.Key == key {
			if !t.allow(c) {
				return nil, forbidden("")
			}
			return t, nil
		}
	}
	return nil, notFound("Unknown template")
}

func (s *Server) bulkList(r *http.Request, c *Ctx) (any, error) {
	out := []*bulkTemplate{}
	for _, t := range bulkTemplates {
		if t.allow(c) {
			if t.note != nil {
				x := *t
				x.Note = t.note(r.Context(), s.st.DB)
				t = &x
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// ---------- Download: header, drop-downs on a hidden sheet, optional current data ----------

func (s *Server) bulkDownload(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := templateFor(c, r.PathValue("tpl"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !c.CanDownload {
		writeErr(w, forbidden("Your role cannot download files"))
		return
	}
	ctx := r.Context()
	cols, rows := t.Cols, [][]any{}
	if (r.URL.Query().Get("data") == "1" && t.Data) || t.fill {
		if cols, rows, err = s.bulkCurrent(ctx, c, t); err != nil {
			writeErr(w, err)
			return
		}
	}
	f := excelize.NewFile()
	defer f.Close()
	const sheet = "Data"
	_ = f.SetSheetName("Sheet1", sheet)
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1}})
	for i, h := range cols {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, h)
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, col, col, float64(max(14, len(h)+4)))
	}
	end, _ := excelize.ColumnNumberToName(len(cols))
	_ = f.SetCellStyle(sheet, "A1", end+"1", head)
	_ = f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	for i, row := range rows {
		_ = f.SetSheetRow(sheet, fmt.Sprintf("A%d", i+2), &row)
	}
	// Drop-downs for master columns (never for employee code or name), from a hidden sheet.
	listCol := 0
	if len(t.lists) > 0 {
		_, _ = f.NewSheet("Lists")
		for i, h := range cols {
			list, ok := t.lists[h]
			if !ok {
				continue
			}
			vals, err := list(ctx, s.st.DB)
			if err != nil || len(vals) == 0 {
				continue
			}
			listCol++
			lc, _ := excelize.ColumnNumberToName(listCol)
			_ = f.SetCellValue("Lists", lc+"1", h)
			for j, v := range vals {
				_ = f.SetCellValue("Lists", fmt.Sprintf("%s%d", lc, j+2), v)
			}
			dc, _ := excelize.ColumnNumberToName(i + 1)
			dv := excelize.NewDataValidation(true)
			dv.Sqref = fmt.Sprintf("%s2:%s5000", dc, dc)
			dv.SetSqrefDropList(fmt.Sprintf("Lists!$%s$2:$%s$%d", lc, lc, len(vals)+1))
			dv.SetError(excelize.DataValidationErrorStyleStop, "Not in the list", "Choose a value from the list.")
			_ = f.AddDataValidation(sheet, dv)
		}
		_ = f.SetSheetVisible("Lists", false)
	}
	notes, _ := f.NewSheet("How to fill")
	_ = notes
	_ = f.SetCellValue("How to fill", "A1", t.Name)
	_ = f.SetCellValue("How to fill", "A3", t.Hint)
	_ = f.SetColWidth("How to fill", "A", "A", 120)
	if t.how != nil {
		title, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
		bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
		wrap, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
		_ = f.SetCellStyle("How to fill", "A1", "A1", title)
		_ = f.SetCellStyle("How to fill", "A3", "A3", wrap)
		// A line starting with "## " is a heading.
		for i, l := range t.how(ctx, s.st.DB) {
			cell := fmt.Sprintf("A%d", i+5)
			st := wrap
			if h, ok := strings.CutPrefix(l, "## "); ok {
				l, st = h, bold
			}
			_ = f.SetCellValue("How to fill", cell, l)
			_ = f.SetCellStyle("How to fill", cell, cell, st)
		}
	}
	f.SetActiveSheet(0)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.xlsx"`, t.Key, time.Now().Format("2006-01-02")))
	_, _ = w.Write(buf.Bytes())
	_ = audit(ctx, s.st.DB, c, "Downloaded bulk template “"+t.Name+"”", "bulk", t.Key, nil, map[string]any{"rows": len(rows)})
}

// column is one text field of the matching documents of a collection, in the order the options give ("id" is
// the document id).
func column(ctx context.Context, q store.Q, coll, field string, filter any, opts ...options.Lister[options.FindOptions]) ([]string, error) {
	proj := bson.D{{Key: field, Value: 1}}
	if field != "id" && field != "_id" {
		proj = append(proj, bson.E{Key: "_id", Value: 0})
	}
	rows, err := q.C(coll).Find(ctx, filter, append(opts, options.Find().SetProjection(proj))...)
	if err != nil {
		return nil, err
	}
	key := field
	if key == "_id" {
		key = "id"
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		v, ok := r[key].(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s: %v is not text", coll, field, r[key])
		}
		out = append(out, v)
	}
	return out, nil
}

// bulkCurrent is the "download current data" export: the template columns first, then reference columns.
func (s *Server) bulkCurrent(ctx context.Context, c *Ctx, t *bulkTemplate) ([]string, [][]any, error) {
	q := s.st.DB
	switch t.Key {
	case "gross", "sal":
		emps, err := scopedEmps(ctx, q, c, bson.D{{Key: "status", Value: "Active"}})
		if err != nil {
			return nil, nil, err
		}
		cityIDs := []string{}
		for _, e := range emps {
			cityIDs = append(cityIDs, e.CityID)
		}
		cities, err := timectxByID(ctx, q, "city", cityIDs, "name")
		if err != nil {
			return nil, nil, err
		}
		city := func(e *emp) any { return cities[e.CityID]["name"] }
		ym, now := today()[:7], today()
		out := [][]any{}
		if t.Key == "gross" {
			for _, e := range emps {
				g, _ := grossFor(ctx, q, e.ID, ym)
				out = append(out, []any{e.ID, g, "", e.Name, e.Grade, city(e), g})
			}
			return []string{"Employee ID", "New Gross", "Effective From", "Name", "Grade", "City", "Current Gross"}, out, nil
		}
		cols := append(append([]string{}, t.Cols...), "Name", "Employment Type", "Grade", "City", "Location", "Structure now", "Basic", "HRA", "Special Allowance", "Reimbursement (outside the gross)", "Bonus",
			"Variable Pay (payout)", "Total Earnings", "Employee PF", "Employee ESI", "Employee LWF", "TDS", "Total Deductions", "Net Pay", "Employer PF", "EPFO Admin",
			"Employer ESI", "Employer LWF", "Medical Insurance", "Gratuity", "Leave Encashment Provision", "Monthly CTC", "Payout Target for the Cycle")
		// The variable payouts due in the open month (bulk_varpay.go): the first on the person's row, any more on rows of
		// their own with only the Employee ID, the payout columns, the name and the cycle's target.
		pays, payNames := map[string][][]any{}, map[string]string{}
		if varPayAllowed(c) {
			if pays, payNames, err = salPayoutRows(ctx, s, c); err != nil {
				return nil, nil, err
			}
		}
		payRow := func(id, name string, p []any) []any {
			row := make([]any, len(cols))
			for i := range row {
				row[i] = ""
			}
			row[0], row[len(t.Cols)] = id, name
			copy(row[len(t.Cols)-len(payCols):], p[:len(payCols)])
			row[len(cols)-1] = p[len(payCols)]
			return row
		}
		if !salAllowed(c) { // payouts only: no salary data
			ids := []string{}
			for id := range pays {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				for _, p := range pays[id] {
					out = append(out, payRow(id, payNames[id], p))
				}
			}
			return cols, out, nil
		}
		allIn := allInOn(ctx, q, ym)
		for _, e := range emps {
			id, typ := e.ID, e.Type
			g, _ := grossFor(ctx, q, id, ym)
			name, st := structureFor(ctx, q, id, now)
			p := payFor(ctx, q, id, typ)
			res := calc.Calc(calc.Input{Type: calc.EmploymentType(typ), Gross: g, Structure: withMedical(&st, p.Medical), VarPct: p.VarPct, VarCycle: p.VarCycle, Meal: p.Meal, Accom: p.Accom,
				Lwf: lwfFor(ctx, q, e.LocID, now), EmpAmts: p.CompAmts, AllIn: allIn})
			R := func(v float64) any { return int64(v + 0.5) }
			var vp, vc any = "", ""
			if p.VarPct != nil {
				vp = *p.VarPct
			}
			if p.VarCycle != nil {
				vc = *p.VarCycle
			}
			var total, target, tmode any = "", "", ""
			if st.Designed { // variable pay as a target (paycomp.go): total pay, the target and how it is given
				in := calc.Input{Type: calc.EmploymentType(typ), Gross: g, Structure: withMedical(&st, p.Medical), Meal: p.Meal, Accom: p.Accom,
					Lwf: lwfFor(ctx, q, e.LocID, now), EmpAmts: p.CompAmts, AllIn: allIn}
				target, tmode = 0.0, "₹"
				if t := targetAt(loadTargets(ctx, q, id), ym); t != nil {
					t.apply(&in)
					if t.Mode == "pct" {
						target, tmode = t.Pct, "%"
					} else {
						target = t.Total
					}
				} else {
					zero := 0.0
					in.VarTarget = &zero
				}
				res = calc.Calc(in)
				total = calc.Round2(g + varMonthly(res))
				vp, vc = "", ""
			}
			first := []any{"", "", "", "", ""}
			if l := pays[id]; len(l) > 0 {
				first = l[0]
			}
			out = append(out, append(append([]any{id, g, vp, vc, nz(p.Meal), nz(p.Accom), "", "", "", "", total, target, tmode, nz(reimbOf(p))}, first[:len(payCols)]...),
				e.Name, typ, e.Grade, city(e), e.Location, name,
				R(res.Basic), R(res.Hra), R(res.Sa), R(res.Reimb), R(res.Bonus), R(res.VarPay), R(res.TotalEarn), R(res.EePF), R(res.EeESI), R(res.LwfEe), R(res.Tds), R(res.Ded), R(res.Net),
				R(res.ErPF), R(res.Admin), R(res.ErESI), R(res.LwfEr), R(res.Med), R(res.Grat), R(res.PlEnc), R(res.CTC), first[len(payCols)]))
			for i := 1; i < len(pays[id]); i++ {
				out = append(out, payRow(id, e.Name, pays[id][i]))
			}
		}
		return cols, out, nil
	case "uni":
		return bulkUniCurrent(ctx, q, c, t)
	case "pers":
		emps, err := scopedEmps(ctx, q, c, bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Inactive"}}}})
		if err != nil {
			return nil, nil, err
		}
		pers, err := q.C("employee_personal").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids(emps)}}}})
		if err != nil {
			return nil, nil, err
		}
		byEmp := map[string]store.Row{}
		for _, p := range pers {
			byEmp[str(p["employee_id"])] = p
		}
		uanOf := bulkUANFlags(ctx, q, ids(emps)) // Existing UAN? (b1_employee.go)
		uanNo := map[string]string{}             // the UAN itself, for those who may see government IDs
		if c.Level("emp.govid") >= 1 {
			if gs, err := q.C("employee_gov_id").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: ids(emps)}}}}); err == nil {
				for _, g := range gs {
					uanNo[str(g["employee_id"])] = statIDClean(str(g["uan"]))
				}
			}
		}
		out := [][]any{}
		for _, e := range emps {
			p := byEmp[e.ID] // blank when there are no personal details
			v := func(k string) string { return str(p[k]) }
			out = append(out, []any{e.ID, v("mobile"), v("email"), v("dob"), v("gender"), v("address"), v("father_spouse"), v("spouse"), v("child1"), v("child2"), v("marital"), v("ec_name"),
				v("ec_relation"), v("ec_mobile"), uanNo[e.ID], uanOf[e.ID], e.Name, e.Status})
		}
		return append(append([]string{}, t.Cols...), "Name", "Status"), out, nil
	case "band":
		// Every active city with every active grade, and the band where there is one.
		cities, err := q.C("city").Find(ctx, bson.D{{Key: "status", Value: "Active"}}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}))
		if err != nil {
			return t.Cols, [][]any{}, err
		}
		grades, err := column(ctx, q, "grade", "name", bson.D{{Key: "status", Value: "Active"}}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
		if err != nil {
			return t.Cols, [][]any{}, err
		}
		bands, err := q.C("pay_band").Find(ctx, nil)
		if err != nil {
			return t.Cols, [][]any{}, err
		}
		band := map[[2]string]store.Row{}
		for _, b := range bands {
			band[[2]string{str(b["city_id"]), str(b["grade"])}] = b
		}
		out, label := [][]any{}, bulkCityLabels(ctx, q, cities)
		for _, ci := range cities {
			for _, g := range grades {
				b := band[[2]string{str(ci["id"]), g}]
				out = append(out, []any{label[str(ci["id"])], g, b["min_gross"], b["max_gross"]})
			}
		}
		return t.Cols, out, nil
	case "bud":
		out := [][]any{}
		if c.Scope.Access == "all" {
			rows, err := q.C("city").Find(ctx, bson.D{{Key: "status", Value: "Active"}}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}))
			if err != nil {
				return nil, nil, err
			}
			label := bulkCityLabels(ctx, q, rows)
			for _, r := range rows {
				out = append(out, []any{"City", label[str(r["id"])], r["headcount_budget"], ""})
			}
			cls, err := q.C("cluster").Find(ctx, bson.D{{Key: "status", Value: "Active"}}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}))
			if err != nil {
				return nil, nil, err
			}
			for _, r := range cls { // a cluster without its own figure uses its locations' total: shown blank
				out = append(out, []any{"Cluster", r["name"], r["headcount_budget"], r["cost_budget"]})
			}
		}
		f := bson.D{{Key: "status", Value: "Active"}}
		if c.Scope.Access == "locs" {
			f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: append([]string{}, c.Scope.Locations...)}}})
		}
		rows, err := q.C("location").Find(ctx, f, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
		for _, r := range rows {
			out = append(out, []any{"Location", r["name"], r["headcount_budget"], r["payroll_budget"]})
		}
		return t.Cols, out, err
	}
	if t.current != nil { // a template that lists its own current data (bulk_varpay.go)
		return t.current(ctx, s, c)
	}
	return t.Cols, nil, nil
}

func nz(v float64) any {
	if v == 0 {
		return ""
	}
	return v
}

// ---------- Upload: parse, check row by row, apply ----------

func readUpload(r *http.Request) ([][]string, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 10<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			return nil, bad("The file must be 10 MB or smaller")
		}
		return nil, bad("Choose an Excel or CSV file") // no file sent (RUN-40)
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose an Excel or CSV file")
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(b, []byte("PK")) {
		x, err := excelize.OpenReader(bytes.NewReader(b))
		if err != nil {
			return nil, bad("That Excel file could not be read")
		}
		defer x.Close()
		sheet := x.GetSheetName(0)
		rows, err := x.GetRows(sheet)
		if err != nil {
			return nil, bad("That Excel file could not be read")
		}
		xlsxDateCells(x, sheet, rows)
		return rows, nil
	}
	cr := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))))
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, bad("That CSV file could not be read: %v", err)
	}
	return rows, nil
}

func (s *Server) bulkRun(r *http.Request, c *Ctx) (any, error) {
	t, err := templateFor(c, r.PathValue("tpl"))
	if err != nil {
		return nil, err
	}
	apply := r.PathValue("action") == "apply"
	if !apply && r.PathValue("action") != "check" {
		return nil, notFound("")
	}
	raw, err := readUpload(r)
	if err != nil {
		return nil, err
	}
	if len(raw) < 2 {
		return nil, bad("No data rows found. Keep the header row and add one row per change.")
	}
	head := make([]string, len(raw[0]))
	for i, h := range raw[0] {
		head[i] = strings.TrimSpace(h)
		if t.Key == "bud" && head[i] == budCostColOld { // the column's name before 18 Nov 2026
			head[i] = budCostCol
		}
	}
	if wide := rosterWideTemplate(t, head); wide != nil {
		t = wide // the roster download's layout: one column per date
	}
	miss := []string{}
	for _, col := range t.Cols {
		if t.Key == "sal" && containsStr(head, payAmtCol) && col != "Employee ID" {
			continue // a payouts-only Salary breakup file (bulk_varpay.go) needs no salary columns
		}
		if !containsStr(head, col) && !containsStr(t.Optional, col) {
			miss = append(miss, col)
		}
	}
	if len(miss) > 0 {
		return nil, bad("Missing columns: %s. Download the template again.", strings.Join(miss, ", "))
	}
	if len(raw) > 5001 {
		return nil, bad("Upload at most 5,000 rows at a time")
	}
	// Rows are worked in order, in as few transactions as possible: one for the whole upload when it takes less
	// than bulkTxBudget (almost always), otherwise one per stretch of about that long, because MongoDB ends a
	// transaction after transactionLifetimeLimitSeconds (60 s) and the driver re-runs a failed one from the start
	// (PostgreSQL ran any upload as one transaction with a savepoint per row). A row is checked before it writes
	// anything (MongoDB has no savepoints), so a row with a problem leaves no trace. A check's transactions are
	// rolled back, an apply's commit; every row sees the rows before it (in a long check only those in its own
	// stretch, so a check starts a new stretch only between two employees when it can).
	type bulkItem struct {
		row   bulkRow
		dup   *bulkResult // set for a repeated row, which is not worked
		group string      // the employee, so that a check keeps an employee's rows together
	}
	items, seen := []bulkItem{}, map[string]int{}
	for i, rec := range raw[1:] {
		v := map[string]string{}
		blank := true
		for j, h := range head {
			if j < len(rec) {
				v[h] = strings.TrimSpace(rec[j])
				if v[h] != "" {
					blank = false
				}
			}
		}
		if blank {
			continue
		}
		row := bulkRow{Line: i + 2, V: v}
		key := v[t.Cols[0]] + "|" + v[t.Cols[1]]
		if t.Key != "band" && t.Key != "bud" && t.Key != "att" && t.Key != "roster" && t.Key != "lvopen" && t.Key != "openuni" {
			key = v["Employee ID"]
		}
		// New employees: repeated codes and PANs are checked row by row. Opening advances: an employee may have more
		// than one (when the company allows it); a repeat is caught by the same checks as a reload.
		if t.Key == "new" || t.Key == "openadv" {
			key = strconv.Itoa(row.Line)
		}
		if t.Key == "sal" { // a person's second due variable component is a row of its own (bulk_varpay.go)
			key = v["Employee ID"] + "|" + v[payMonthCol] + "|" + v[payCompCol]
		}
		if t.Key == "incentive" { // several incentives a person; the same month and remark twice is the repeat (bulk_incentive.go)
			key = incentiveRowKey(v)
		}
		if t.key != nil {
			key = t.key(v)
		}
		it := bulkItem{row: row, group: strings.ToUpper(v["Employee ID"])}
		if first, dup := seen[strings.ToUpper(key)]; dup {
			it.dup = &bulkResult{Line: row.Line, ID: key, Errors: []string{fmt.Sprintf("Appears again (first on line %d). Keep one row each", first)}, Changes: []bulkChange{}}
		} else {
			seen[strings.ToUpper(key)] = row.Line
		}
		items = append(items, it)
	}
	ctx := context.WithValue(r.Context(), tsKey{}, &tsHolder{}) // the time set is read once per upload
	results, ok := []bulkResult{}, 0
	for next := 0; ; {
		var part []bulkResult
		var partOK, end int
		err := s.st.Tx(r.Context(), func(tx store.Q) error {
			part, partOK, end = []bulkResult{}, 0, next // the transaction may run again
			began := time.Now()
			for end < len(items) {
				if took := time.Since(began); end > next && took >= bulkTxBudget &&
					(apply || items[end].group == "" || items[end].group != items[end-1].group || took >= 2*bulkTxBudget) {
					break // the rest goes in the next transaction
				}
				it := items[end]
				end++
				if it.dup != nil {
					part = append(part, *it.dup)
					continue
				}
				res, err := t.row(s, ctx, tx, c, it.row, apply)
				res.Line = it.row.Line
				if res.Errors == nil {
					res.Errors = []string{}
				}
				if res.Changes == nil {
					res.Changes = []bulkChange{}
				}
				if err != nil {
					if ae, isAPI := err.(*apiError); isAPI {
						res.Errors = append(res.Errors, ae.Message)
					} else {
						return err
					}
				} else if len(res.Errors) == 0 && len(res.Changes) > 0 {
					partOK++
				}
				part = append(part, res)
			}
			if !apply {
				return errBulkCheck
			}
			if end < len(items) {
				return nil
			}
			n := ok + partOK // the audit goes in with the last rows
			return audit(ctx, tx, c, fmt.Sprintf("Bulk update “%s” applied: %d row%s", t.Name, n, map[bool]string{true: "", false: "s"}[n == 1]), "bulk", t.Key, nil, map[string]any{"rows": n})
		})
		if err != nil && !errors.Is(err, errBulkCheck) {
			return nil, err
		}
		results, ok, next = append(results, part...), ok+partOK, end
		if next >= len(items) {
			break
		}
	}
	return map[string]any{"applied": apply, "ok": ok, "results": results}, nil
}

// bulkTxBudget is how long one transaction of an upload works rows before the rest go into the next one: well
// inside MongoDB's 60 s transaction lifetime. A check may run to twice this to keep an employee's rows together.
var bulkTxBudget = 20 * time.Second

// errBulkCheck ends a check's transaction without committing it.
var errBulkCheck = errors.New("bulk check: nothing is kept")

// bulkEmployee loads the employee of a row and checks it is in scope.
func bulkEmployee(ctx context.Context, tx store.Q, c *Ctx, r bulkRow, res *bulkResult) (store.Row, bool) {
	id := strings.ToUpper(r.V["Employee ID"])
	res.ID = id
	e, _ := bulkFindCI(ctx, tx, "employee", "_id", id, nil, "name", "type", "status", "doj", "grade", "location_id")
	if e != nil {
		e = store.Row{"id": e["id"], "name": e["name"], "type": e["type"], "status": e["status"], "doj": e["doj"], "grade": e["grade"], "loc": e["location_id"]}
	}
	if e == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("Employee %s not found", or(r.V["Employee ID"], "(blank)")))
		return nil, false
	}
	res.ID, res.Name = e["id"].(string), e["name"].(string)
	if ok, _ := c.InScope(ctx, tx, res.ID); !ok {
		res.Errors = append(res.Errors, res.ID+" is outside your access")
		return nil, false
	}
	return e, true
}

// bulkFindCI finds the first document whose field equals v ignoring case and matches the
// rest of filter, with the fields named. An exact match is tried first, as the stored values mostly are.
func bulkFindCI(ctx context.Context, q store.Q, coll, field, v string, filter bson.D, fields ...string) (store.Row, error) {
	proj := bson.D{}
	for _, f := range fields {
		proj = append(proj, bson.E{Key: f, Value: 1})
	}
	o := options.FindOne().SetProjection(proj)
	if r, err := q.C(coll).FindOne(ctx, append(bson.D{{Key: field, Value: v}}, filter...), o); err != nil || r != nil {
		return r, err
	}
	return q.C(coll).FindOne(ctx, append(bson.D{{Key: field, Value: bulkCI(v)}}, filter...), o)
}

// bulkCI matches a text equal to v ignoring case.
func bulkCI(v string) bson.D {
	return bson.D{{Key: "$regex", Value: "^" + regexp.QuoteMeta(v) + "$"}, {Key: "$options", Value: "i"}}
}

// bulkCityOf is the city of a location ("" when it is not known).
func bulkCityOf(ctx context.Context, q store.Q, locID any) string {
	l, err := q.C("location").FindOne(ctx, bson.D{{Key: "_id", Value: locID}}, options.FindOne().SetProjection(bson.D{{Key: "cluster_id", Value: 1}}))
	if err != nil || l == nil {
		return ""
	}
	k, err := q.C("cluster").FindOne(ctx, bson.D{{Key: "_id", Value: l["cluster_id"]}}, options.FindOne().SetProjection(bson.D{{Key: "city_id", Value: 1}}))
	if err != nil || k == nil {
		return ""
	}
	return str(k["city_id"])
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func bulkUpd(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	if e["status"] != "Active" {
		res.Errors = append(res.Errors, fmt.Sprintf("%s is %s. Only active employees can be updated", res.Name, strings.ToLower(e["status"].(string))))
		return res, nil
	}
	ch := map[string]string{}
	for col, key := range map[string]string{"Reporting Location": "location", "Department": "department", "Designation": "designation", "Grade": "grade", "Shift": "shift",
		"Reporting Manager L1": "manager", "Functional Manager L2": "functionalManager", "Attendance Mode": "attendanceMode"} {
		if v := r.V[col]; v != "" {
			ch[key] = v
		}
	}
	if city := r.V["Work City"]; city != "" {
		curCity := bulkCityOf(ctx, tx, e["loc"])
		ci, _ := bulkFindCI(ctx, tx, "city", "name", city, bson.D{{Key: "status", Value: "Active"}})
		if ci == nil {
			res.Errors = append(res.Errors, fmt.Sprintf("Work City “%s” is not an active city", city))
			return res, nil
		}
		cityID := str(ci["id"])
		if cityID != curCity && ch["location"] == "" {
			res.Errors = append(res.Errors, "Reporting Location is required when the city changes")
			return res, nil
		}
		if loc := ch["location"]; loc != "" {
			// A location of that name in one of the city's clusters.
			clusters, _ := tx.C("cluster").Distinct(ctx, "_id", bson.D{{Key: "city_id", Value: cityID}})
			if l, _ := bulkFindCI(ctx, tx, "location", "name", loc, bson.D{{Key: "cluster_id", Value: bson.D{{Key: "$in", Value: clusters}}}}); l == nil {
				res.Errors = append(res.Errors, fmt.Sprintf("Location “%s” is not in %s", loc, city))
				return res, nil
			}
		}
	}
	updUnchanged(ctx, tx, res.ID, ch) // the current data uploaded as it is: no change (bulk_current.go)
	// Work Policy: an active policy other than the employee's puts them on it (prototype TPL.upd).
	pol := bulkPolicy(ctx, tx, res.ID, r.V["Work Policy"], &res)
	if len(res.Errors) > 0 {
		return res, nil
	}
	if len(ch) == 0 && pol == nil {
		return res, nil
	}
	if r.V["Reason for change"] == "" || r.V["Effective From"] == "" {
		res.Errors = append(res.Errors, "Reason for change and Effective From are needed for a change")
		return res, nil
	}
	reason, eff, err := updReasonEff(ctx, tx, r, fmt.Sprint(e["doj"]))
	if err != nil {
		return res, err
	}
	var changes []change
	if len(ch) > 0 {
		if changes, err = ApplyJobChange(ctx, tx, c, res.ID, JobChange{Changes: ch, Reason: reason, EffectiveFrom: eff}, "Bulk update"); err != nil {
			return res, err
		}
	}
	note := dateNote(eff)
	for _, x := range changes {
		res.Changes = append(res.Changes, bulkChange{x.Field, x.From, x.To, note})
	}
	if pol != nil {
		if err := applyPolicyChange(ctx, tx, c, res.ID, pol, reason, eff, "Bulk update"); err != nil {
			return res, err
		}
		res.Changes = append(res.Changes, bulkChange{"Work Policy", pol.from, pol.to, note})
	}
	if len(res.Changes) > 0 {
		res.Eff, res.Reason = eff, reason
	}
	return res, nil
}

// excelDate accepts 2026-11-01, 01-11-2026, 01/11/2026 and Excel serial numbers.
// xlsxDateCells replaces the displayed text of every date cell with the date as yyyy-mm-dd (PPL-091, PPL-155):
// GetRows gives what Excel shows, which for a date depends on the cell's format (13-Nov-26, 11/13/26 in the US
// default …). The cell's own value, a day number, is exact.
func xlsxDateCells(x *excelize.File, sheet string, rows [][]string) {
	raw, err := x.GetRows(sheet, excelize.Options{RawCellValue: true})
	if err != nil {
		return
	}
	dateStyle := map[int]bool{}
	for i, row := range rows {
		if i >= len(raw) {
			break
		}
		for j, v := range row {
			if j >= len(raw[i]) || raw[i][j] == v || isDate(v) {
				continue
			}
			n, err := strconv.ParseFloat(strings.TrimSpace(raw[i][j]), 64)
			if err != nil || n < 1 || n > 2958465 {
				continue
			}
			cell, err := excelize.CoordinatesToCellName(j+1, i+1)
			if err != nil {
				continue
			}
			st, err := x.GetCellStyle(sheet, cell)
			if err != nil {
				continue
			}
			is, seen := dateStyle[st]
			if !seen {
				is = xlsxIsDateStyle(x, st)
				dateStyle[st] = is
			}
			if is {
				rows[i][j] = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n)).Format("2006-01-02")
			}
		}
	}
}

// xlsxIsDateStyle: the style's number format shows a date (Excel's built-in date formats, or a custom one with day,
// month or year codes outside quotes).
func xlsxIsDateStyle(x *excelize.File, idx int) bool {
	s, err := x.GetStyle(idx)
	if err != nil || s == nil {
		return false
	}
	if s.CustomNumFmt != nil && *s.CustomNumFmt != "" {
		f, quoted := strings.ToLower(*s.CustomNumFmt), false
		for _, r := range f {
			switch {
			case r == '"':
				quoted = !quoted
			case !quoted && (r == 'd' || r == 'y'):
				return true
			}
		}
		return false
	}
	n := s.NumFmt
	return (n >= 14 && n <= 17) || n == 22 || (n >= 27 && n <= 36) || (n >= 50 && n <= 58)
}

func excelDate(v string) string {
	v = strings.TrimSpace(v)
	// Day first, as dates are written in India (a two-digit year too: 12-06-24 is 12 June 2024, PPL-091). A real
	// date cell arrives as yyyy-mm-dd (xlsxDateCells), whatever its display format.
	for _, f := range []string{"2006-01-02", "02-01-2006", "02/01/2006", "2/1/2006", "2-1-2006", "02-01-06", "02/01/06", "2/1/06", "2006/01/02",
		"02-Jan-2006", "2-Jan-2006", "02-Jan-06", "2-Jan-06", "02 Jan 2006", "2 Jan 2006", "02/Jan/2006"} {
		if t, err := time.Parse(f, v); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 20000 && n < 80000 {
		return time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n)).Format("2006-01-02")
	}
	return v
}

func dateNote(d string) string {
	switch {
	case d > today():
		return "scheduled from " + longDate(d)
	case d < today():
		return "back-dated to " + longDate(d)
	}
	return "from today"
}

func bulkOff(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	lwd := excelDate(r.V["Last Working Day"])
	var reason string
	switch {
	case e["status"] == "Inactive":
		res.Errors = append(res.Errors, "Already inactive")
	case !isDate(lwd):
		res.Errors = append(res.Errors, "Last Working Day is required (a date like 2026-11-30)")
	case lwd < e["doj"].(string):
		res.Errors = append(res.Errors, "Last Working Day is before the joining date")
	case !bulkReason(ctx, tx, "Separation", r.V["Reason"], &reason):
		reasons, _ := bulkReasons("Separation")(ctx, tx)
		res.Errors = append(res.Errors, fmt.Sprintf("Reason “%s” is not a separation reason. Choose one of: %s", r.V["Reason"], strings.Join(reasons, ", ")))
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	to := "Inactive from " + longDate(lwd)
	if lwd > today() {
		to = "On notice until " + longDate(lwd)
		if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: res.ID}}, bson.M{"$set": bson.M{"exit_status": "On notice", "lwd": lwd, "exit_reason": reason}}); err != nil {
			return res, err
		}
	} else if _, err := tx.C("employee").Update(ctx, bson.D{{Key: "_id", Value: res.ID}},
		bson.M{"$set": bson.M{"status": "Inactive", "lwd": lwd, "exit_reason": reason, "exit_status": nil}}); err != nil {
		return res, err
	} else if _, err := uniformRefundCancel(ctx, tx, res.ID, lwd); err != nil { // policy: no uniform refund after leaving
		return res, err
	}
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": res.ID, "field": "Status", "old_value": e["status"], "new_value": to, "reason": reason,
		"effective_from": lwd, "source": "Bulk update", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return res, err
	}
	res.Changes = append(res.Changes, bulkChange{"Status", e["status"].(string), to, reason})
	return res, nil
}

// bulkReason finds an active change reason for a use by name, ignoring case, and gives it as the master spells it.
func bulkReason(ctx context.Context, q store.Q, usedFor, v string, out *string) bool {
	r, err := bulkFindCI(ctx, q, "simple_master", "name", v, bson.D{{Key: "kind", Value: "change_reason"}, {Key: "used_for", Value: usedFor}, {Key: "status", Value: "Active"}}, "name")
	if err != nil || r == nil {
		return false
	}
	*out = str(r["name"])
	return true
}

// monthsBetween counts whole payroll months from ym up to (not including) to.
func monthsBetween(ym, to string) int {
	a, err1 := time.Parse("2006-01", ym)
	b, err2 := time.Parse("2006-01", to)
	if err1 != nil || err2 != nil || !a.Before(b) {
		return 0
	}
	return (b.Year()-a.Year())*12 + int(b.Month()-a.Month())
}

// applyGross writes a gross from a date (YYYY-MM-DD; a bare month YYYY-MM means its 1st). A date after the 1st
// splits that month by days: the earlier gross before it, the new one from it (user decision 13 Nov 2026, ITM-28).
// A past month raises an arrear (or a recovery) for the difference over the months already paid, through the
// approval engine (spec 9, applyGross).
func (s *Server) applyGross(ctx context.Context, tx store.Q, c *Ctx, id, eff string, gross float64, reason string, apply bool, res *bulkResult) error {
	ym, date := eff[:7], eff
	if len(eff) == 7 {
		date = eff + "-01"
	}
	open := openYm(ctx, tx)
	old, _ := grossFor(ctx, tx, id, ym)
	if d := grossDateOf(ctx, tx, id, ym); old == gross && (d == "" || d == date) {
		return nil
	}
	note := "from " + ym
	if date[8:] != "01" {
		note = "from " + date + " (split by days in " + ym + ")"
	}
	switch {
	case ym > open:
		note += " (scheduled)"
	case ym < open:
		amt, n := revisionArrear(ctx, tx, id, date, open, gross)
		kind, wf := "arrear", "p_arrear"
		if amt < 0 {
			kind, wf = "recovery", "p_ded"
		}
		note += fmt.Sprintf(" (back-dated: %s of %s for %d month%s, for approval)", kind, rupees(abs(amt)), n, map[bool]string{true: "", false: "s"}[n == 1])
		if apply && amt != 0 {
			why := fmt.Sprintf("Salary revision effective %s, %d month%s", map[bool]string{true: ym, false: date}[date[8:] == "01"], n, map[bool]string{true: "", false: "s"}[n == 1])
			if amt < 0 {
				why = "Salary reduction, recovery of excess paid: " + why
			}
			payload := map[string]any{"amount": abs(amt), "note": why, "forYm": ym, "toYm": addYm(open, -1), "payMonth": open, "arrearType": "Salary revision", "slipText": why}
			if amt < 0 {
				payload = map[string]any{"amount": abs(amt), "note": why, "dedType": store.SysName(ctx, tx, "reason.excess_recovery"), "instalments": 1, "startYm": open}
			}
			if _, err := s.createRequestTx(ctx, tx, c, wf, id, approval.Facts{Amt: abs(amt)}, payload); err != nil {
				return err
			}
		}
	}
	if _, err := tx.C("gross_history").Upsert(ctx, bson.D{{Key: "employee_id", Value: id}, {Key: "from_month", Value: ym}},
		bson.M{"gross": gross, "old_gross": old, "from_date": date, "reason": reason, "created_by": c.Name + " (" + c.Role + ")"}, nil); err != nil {
		return err
	}
	if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": id, "field": "Monthly gross", "old_value": rupees(old), "new_value": rupees(gross), "reason": reason,
		"effective_from": date, "source": "Bulk update", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
		return err
	}
	res.Changes = append(res.Changes, bulkChange{"Gross", rupees(old), rupees(gross), note})
	return nil
}

// revisionArrear is the difference a gross written from date (YYYY-MM-DD) makes to the months already paid (its
// month up to the month before open), month by month (payroll test ITM-89): for each month, the gross it now has (the
// new one, unless a later revision already on record still applies) minus the gross actually paid (the stored payroll
// row, else the history as it stood). The month of a date after the 1st has the earlier gross for the days before it
// and the new one from it (ITM-28). It returns the total and the number of months that change.
func revisionArrear(ctx context.Context, q store.Q, id, date, open string, gross float64) (float64, int) {
	ym := date[:7]
	paid := map[string]float64{}
	if rows, err := q.C("payroll_row").Find(ctx, bson.M{"employee_id": id, "ym": bson.M{"$gte": ym, "$lt": open}},
		options.Find().SetProjection(bson.M{"ym": 1, "data.gross": 1})); err == nil {
		for _, r := range rows {
			if d, ok := r["data"].(map[string]any); ok {
				if g, ok := d["gross"].(float64); ok && g > 0 {
					paid[str(r["ym"])] = g
				}
			}
		}
	}
	total, n := 0.0, 0
	for m := ym; m < open; m = addYm(m, 1) {
		now, _ := grossFor(ctx, q, id, m) // the history as it stands, before this change is written
		was, ok := paid[m]
		if !ok {
			was = now
		}
		if later, _ := q.C("gross_history").Exists(ctx, bson.M{"employee_id": id, "from_month": bson.M{"$gt": ym, "$lte": m}}); !later {
			now = gross // the new gross applies to this month; a later revision on record keeps its own months
			if m == ym {
				before, _ := grossFor(ctx, q, id, addYm(ym, -1)) // the gross in force on the 1st
				now = calc.SplitGross(ym, before, gross, date)
			}
		}
		if d := calc.Round2(now - was); d != 0 {
			total += d
			n++
		}
	}
	return calc.Round2(total), n
}

// grossDateOf is the date the revision recorded for month ym applies from ("" when there is none; the 1st when it
// was written before dates were kept).
func grossDateOf(ctx context.Context, q store.Q, id, ym string) string {
	g, err := q.C("gross_history").FindOne(ctx, bson.M{"employee_id": id, "from_month": ym}, options.FindOne().SetProjection(bson.M{"from_date": 1}))
	if err != nil || g == nil {
		return ""
	}
	if d := str(g["from_date"]); isDate(d) {
		return d
	}
	return ym + "-01"
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// month reads "2026-11-01", "2026-11" or blank (this month).
func month(v string) (string, bool) {
	v = excelDate(v)
	switch {
	case v == "":
		return today()[:7], true
	case isDate(v):
		return v[:7], true
	case len(v) == 7 && isDate(v+"-01"):
		return v, true
	}
	return "", false
}

func bulkGross(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	g, err := strconv.ParseFloat(r.V["New Gross"], 64)
	ym, okM := month(r.V["Effective From"])
	eff := ym // a date keeps its day: the month is split by days (ITM-28)
	if d := excelDate(r.V["Effective From"]); isDate(d) {
		eff = d
	}
	switch {
	case err != nil || g <= 0:
		res.Errors = append(res.Errors, "New Gross must be a number above zero")
	case e["status"] != "Active":
		res.Errors = append(res.Errors, "Employee is not active")
	case !okM:
		res.Errors = append(res.Errors, "Effective From must be a date like 2026-11-01 or a month like 2026-11")
	case ym < e["doj"].(string)[:7] || (len(eff) == 10 && eff < e["doj"].(string)):
		res.Errors = append(res.Errors, "Effective From is before the joining date")
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	if err := s.applyGross(ctx, tx, c, res.ID, eff, g, "Gross revision", apply, &res); err != nil {
		return res, err
	}
	city := bulkCityOf(ctx, tx, e["loc"])
	if b := bandOf(ctx, tx, city, e["grade"].(string), g); b != nil && b.Position != "within" && len(res.Changes) > 0 {
		res.Changes[len(res.Changes)-1].Note += fmt.Sprintf(" · %s the band %s to %s", b.Position, rupees(b.Min), rupees(b.Max))
	}
	return res, nil
}

func bulkSal(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	if e["status"] != "Active" {
		res.Errors = append(res.Errors, "Employee is not active")
		return res, nil
	}
	typ := e["type"].(string)
	t := calc.RulesFor(calc.EmploymentType(typ))
	num := func(col string, min float64) *float64 {
		v := r.V[col]
		if v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < min {
			res.Errors = append(res.Errors, fmt.Sprintf("%s must be a number of %v or more", col, min))
			return nil
		}
		return &f
	}
	gross, vp, vc, meal, accom := num("Monthly Gross", 1), num("Variable Pay %", 0), num("Variable Payout Cycle", 1), num("Meal Deduction", 0), num("Accommodation Deduction", 0)
	// Pay by total (fixed + variable) and the variable target of a structure made from components (paycomp.go).
	vt, vmode, tp, curTarget, designed := salCompCols(ctx, tx, res.ID, typ, r, num, gross, vp, &res)
	if tp != nil && gross == nil {
		gross = tp
	}
	// A value equal to the current one is no change, so the downloaded data uploaded unchanged needs no Effective
	// From or reason (PPL-150).
	same := func(a *float64, b float64) bool { return a != nil && math.Abs(*a-b) < 0.005 }
	if g, ok := grossFor(ctx, tx, res.ID, today()[:7]); ok && same(gross, g) {
		gross = nil
	}
	curPay := payFor(ctx, tx, res.ID, typ)
	if curPay.VarPct != nil && same(vp, *curPay.VarPct) {
		vp = nil
	}
	if curPay.VarCycle != nil && same(vc, float64(*curPay.VarCycle)) {
		vc = nil
	}
	if same(meal, curPay.Meal) {
		meal = nil
	}
	if same(accom, curPay.Accom) {
		accom = nil
	}
	reimb := num(reimbCol, 0)
	if same(reimb, reimbOf(curPay)) {
		reimb = nil
	}
	if (vp != nil || vc != nil) && !t.Variable {
		res.Errors = append(res.Errors, typ+" staff do not earn variable pay")
	}
	if vc != nil && !containsInt([]int{1, 3, 6, 12}, int(*vc)) {
		res.Errors = append(res.Errors, "Variable Payout Cycle is 1, 3, 6 or 12 months")
	}
	if (meal != nil || accom != nil) && t.Ded != "all" {
		res.Errors = append(res.Errors, typ+" staff have no meal or accommodation deduction")
	}
	var structID, structName string
	if v := r.V["Salary Structure"]; v != "" {
		if st, _ := bulkFindCI(ctx, tx, "salary_structure", "name", v, bson.D{{Key: "status", Value: "Active"}}, "name"); st != nil {
			structID, structName = str(st["id"]), str(st["name"])
		}
		if structID == "" {
			res.Errors = append(res.Errors, fmt.Sprintf("Salary Structure “%s” is not an active structure", v))
		} else if !isDate(excelDate(r.V["Structure Effective From"])) {
			res.Errors = append(res.Errors, "Structure Effective From is needed as a date like 2026-11-01")
		}
	}
	if vt != nil && curTarget != nil && vmode == curTarget.Mode && ((vmode == "pct" && math.Abs(*vt-curTarget.Pct) < 0.005) || (vmode != "pct" && math.Abs(*vt-curTarget.Total) < 0.005)) {
		vt = nil
	}
	if vt != nil && curTarget == nil && *vt == 0 {
		vt = nil
	}
	if vt != nil {
		switch {
		case !designed:
			res.Errors = append(res.Errors, salTargetCol+" is for a structure that pays variable as a target; this structure pays variable as Variable Pay %")
		case !t.Variable:
			res.Errors = append(res.Errors, typ+" staff do not earn variable pay")
		}
	}
	if designed && (vp != nil || vc != nil) {
		res.Errors = append(res.Errors, "Variable Pay % and Variable Payout Cycle are for a percentage structure: give "+salTargetCol+" and "+salModeCol+" (% or ₹) instead")
	}
	payChange := gross != nil || vp != nil || vc != nil || meal != nil || accom != nil || vt != nil || reimb != nil
	eff := excelDate(r.V["Effective From"])
	var reason string
	if payChange {
		if !isDate(eff) {
			res.Errors = append(res.Errors, "Effective From is needed as a date like 2026-11-01")
		} else if eff < e["doj"].(string) {
			res.Errors = append(res.Errors, "Effective From is before the joining date")
		}
		if !bulkReason(ctx, tx, "Salary", r.V["Reason for change"], &reason) {
			res.Errors = append(res.Errors, fmt.Sprintf("Reason for change “%s” is not a salary reason", r.V["Reason for change"]))
		}
	}
	if reimb != nil { // the Reimbursement is outside the gross (25 Nov 2026): the structure must carry it, no retainer
		on := eff
		if !isDate(on) {
			on = today()
		}
		if err := reimbCheckFor(ctx, tx, res.ID, typ, on, *reimb); err != nil {
			if ae, ok := err.(*apiError); ok {
				res.Errors = append(res.Errors, ae.Message)
			} else {
				return res, err
			}
		}
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	if gross != nil {
		if err := s.applyGross(ctx, tx, c, res.ID, eff, *gross, reason, apply, &res); err != nil {
			return res, err
		}
	}
	if reimb != nil {
		ch, err := setReimb(ctx, tx, c, res.ID, reimbOf(curPay), *reimb, reason, eff, "Bulk update")
		if err != nil {
			return res, err
		}
		if ch != nil {
			res.Changes = append(res.Changes, *ch)
		}
	}
	if vp != nil || vc != nil || meal != nil || accom != nil {
		cur := payFor(ctx, tx, res.ID, typ)
		next := cur
		if vp != nil {
			next.VarPct = vp
		}
		if vc != nil {
			n := int(*vc)
			next.VarCycle = &n
		}
		if meal != nil {
			next.Meal = *meal
		}
		if accom != nil {
			next.Accom = *accom
		}
		set := bson.M{"var_pct": next.VarPct, "var_cycle": next.VarCycle, "meal": next.Meal, "accommodation": next.Accom}
		if !sameFloatPtr(cur.VarPct, next.VarPct) { // the new % applies from Effective From, not back into the cycle (RUN-31)
			set["var_from"], set["var_prev_pct"] = eff[:7], cur.VarPct
			varStartSet(ctx, tx, res.ID, cur.VarPct, next.VarPct, eff[:7], set) // the payout cycle starts with it (varpay.go)
		}
		if _, err := tx.C("employee_pay").Upsert(ctx, bson.D{{Key: "employee_id", Value: res.ID}}, set, nil); err != nil {
			return res, err
		}
		show := func(p *float64, suffix string) string {
			if p == nil {
				return "Standard"
			}
			return strconv.FormatFloat(*p, 'f', -1, 64) + suffix
		}
		cyc := func(p *int) string {
			if p == nil {
				return "Standard"
			}
			return fmt.Sprintf("Every %d months", *p)
		}
		for _, x := range []bulkChange{{"Variable pay %", show(cur.VarPct, "%"), show(next.VarPct, "%"), ""}, {"Variable payout cycle", cyc(cur.VarCycle), cyc(next.VarCycle), ""},
			{"Meal deduction", rupees(cur.Meal), rupees(next.Meal), ""}, {"Accommodation deduction", rupees(cur.Accom), rupees(next.Accom), ""}} {
			if x.From == x.To {
				continue
			}
			if _, err := tx.C("employee_change").Insert(ctx, bson.M{"employee_id": res.ID, "field": x.Field, "old_value": x.From, "new_value": x.To, "reason": reason,
				"effective_from": eff, "source": "Bulk update", "changed_by": c.Name + " (" + c.Role + ")"}); err != nil {
				return res, err
			}
			res.Changes = append(res.Changes, x)
		}
	}
	if structID != "" {
		from := excelDate(r.V["Structure Effective From"])
		cur, _ := structureFor(ctx, tx, res.ID, from)
		if cur != structName {
			if _, err := tx.C("structure_map").Upsert(ctx, bson.D{{Key: "employee_id", Value: res.ID}, {Key: "from_date", Value: from}}, bson.M{"structure_id": structID}, nil); err != nil {
				return res, err
			}
			res.Changes = append(res.Changes, bulkChange{"Salary structure", cur, structName, "from " + longDate(from)})
		}
	}
	if vt != nil {
		if open := openYm(ctx, tx); eff[:7] < open {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: a variable target applies from the open month (%s) or later", salTargetCol, ymLabel(open)))
			return res, nil
		}
		ch, err := setVarTarget(ctx, tx, c, res.ID, eff[:7], vmode, *vt, nil, reason, "Bulk update")
		if err != nil {
			if ae, ok := err.(*apiError); ok {
				res.Errors = append(res.Errors, ae.Message)
				return res, nil
			}
			return res, err
		}
		res.Changes = append(res.Changes, *ch)
	}
	return res, nil
}

func bulkBand(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	res := bulkResult{ID: r.V["City"] + " · " + r.V["Grade"]}
	var cityID string
	if ci, why := bulkFindCity(ctx, tx, r.V["City"]); ci != nil {
		cityID = str(ci["id"])
	} else if why != "" {
		res.Errors = append(res.Errors, why)
	} else {
		res.Errors = append(res.Errors, fmt.Sprintf("City “%s” is not an active city", r.V["City"]))
	}
	if ok, _ := tx.C("grade").Exists(ctx, bson.D{{Key: "name", Value: r.V["Grade"]}, {Key: "status", Value: "Active"}}); !ok {
		res.Errors = append(res.Errors, fmt.Sprintf("Grade “%s” is not an active grade", r.V["Grade"]))
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	var curMin, curMax float64
	b, _ := tx.C("pay_band").FindOne(ctx, bson.D{{Key: "city_id", Value: cityID}, {Key: "grade", Value: r.V["Grade"]}})
	has := b != nil
	if has {
		curMin, curMax = timectxNum(b["min_gross"]), timectxNum(b["max_gross"])
	}
	if !has && r.V["Minimum Gross"] == "" && r.V["Maximum Gross"] == "" {
		return res, nil // a city and grade with no band, left blank: nothing to do (PPL-156)
	}
	mn, mx := curMin, curMax
	for col, p := range map[string]*float64{"Minimum Gross": &mn, "Maximum Gross": &mx} {
		if v := r.V[col]; v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				res.Errors = append(res.Errors, col+" must be a number above zero")
				continue
			}
			*p = f
		}
	}
	if len(res.Errors) == 0 && (mn <= 0 || mx <= 0) {
		res.Errors = append(res.Errors, "A new band needs both a minimum and a maximum")
	}
	if len(res.Errors) == 0 && mx < mn {
		res.Errors = append(res.Errors, "Maximum Gross is below Minimum Gross")
	}
	if len(res.Errors) > 0 || (has && mn == curMin && mx == curMax) {
		return res, nil
	}
	if _, err := tx.C("pay_band").Upsert(ctx, bson.D{{Key: "city_id", Value: cityID}, {Key: "grade", Value: r.V["Grade"]}}, bson.M{"min_gross": mn, "max_gross": mx}, nil); err != nil {
		return res, err
	}
	outside := bulkOutsideBand(ctx, tx, cityID, r.V["Grade"], mn, mx)
	from := "No band"
	if has {
		from = rupees(curMin) + " to " + rupees(curMax)
	}
	res.Changes = append(res.Changes, bulkChange{"Pay band", from, rupees(mn) + " to " + rupees(mx), fmt.Sprintf("%d employee%s outside the new band", outside, map[bool]string{true: "", false: "s"}[outside == 1])})
	return res, audit(ctx, tx, c, "Pay band "+res.ID+" set to "+rupees(mn)+" to "+rupees(mx), "pay_band", res.ID, nil, nil)
}

// bulkOutsideBand counts the active employees of a city and grade whose gross this month is outside min to max
// (people without a gross are not counted).
func bulkOutsideBand(ctx context.Context, tx store.Q, cityID, grade string, mn, mx float64) int {
	clusters, err := tx.C("cluster").Distinct(ctx, "_id", bson.D{{Key: "city_id", Value: cityID}})
	if err != nil {
		return 0
	}
	locs, err := tx.C("location").Distinct(ctx, "_id", bson.D{{Key: "cluster_id", Value: bson.D{{Key: "$in", Value: clusters}}}})
	if err != nil {
		return 0
	}
	emps, err := column(ctx, tx, "employee", "id", bson.D{{Key: "location_id", Value: bson.D{{Key: "$in", Value: locs}}}, {Key: "grade", Value: grade}, {Key: "status", Value: "Active"}})
	if err != nil {
		return 0
	}
	// Each one's latest gross from this month or before.
	hist, err := tx.C("gross_history").Find(ctx, bson.D{{Key: "employee_id", Value: bson.D{{Key: "$in", Value: emps}}}, {Key: "from_month", Value: bson.D{{Key: "$lte", Value: store.DateOf(tx.Now())[:7]}}}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}}).SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "gross", Value: 1}}))
	if err != nil {
		return 0
	}
	n, done := 0, map[string]bool{}
	for _, h := range hist {
		if id := str(h["employee_id"]); !done[id] {
			done[id] = true
			if g := timectxNum(h["gross"]); g < mn || g > mx {
				n++
			}
		}
	}
	return n
}

var reNameState = regexp.MustCompile(`^(.+?)\s*\(([^()]+)\)$`)

// bulkCityLabels names each city as the templates write it: its name, or "Name (State)" when another active city
// in another state has the same name (PPL-156).
func bulkCityLabels(ctx context.Context, q store.Q, cities []store.Row) map[string]string {
	seen, stateIDs := map[string]int{}, []string{}
	for _, r := range cities {
		seen[strings.ToLower(str(r["name"]))]++
		stateIDs = append(stateIDs, str(r["state_id"]))
	}
	states, _ := timectxByID(ctx, q, "state", stateIDs, "name")
	out := map[string]string{}
	for _, r := range cities {
		name := str(r["name"])
		if seen[strings.ToLower(name)] > 1 {
			name += " (" + str(states[str(r["state_id"])]["name"]) + ")"
		}
		out[str(r["id"])] = name
	}
	return out
}

// bulkFindCity finds an active city by its template label: the name, or "Name (State)". A name two active cities
// share is refused with the labels to use.
func bulkFindCity(ctx context.Context, q store.Q, label string) (store.Row, string) {
	label = strings.TrimSpace(label)
	all, _ := q.C("city").Find(ctx, bson.D{{Key: "name", Value: bulkCI(label)}, {Key: "status", Value: "Active"}})
	if len(all) == 1 {
		return all[0], ""
	}
	if len(all) > 1 {
		return nil, fmt.Sprintf("%d cities are named %s: write the city as %s (State)", len(all), label, label)
	}
	if m := reNameState.FindStringSubmatch(label); m != nil {
		if st, _ := bulkFindCI(ctx, q, "state", "name", m[2], nil); st != nil {
			x, _ := bulkFindCI(ctx, q, "city", "name", m[1], bson.D{{Key: "status", Value: "Active"}, {Key: "state_id", Value: st["id"]}})
			return x, ""
		}
	}
	return nil, ""
}

func bulkBud(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	res := bulkResult{ID: r.V["Level"] + " " + r.V["Name"]}
	lvl := strings.ToLower(r.V["Level"])
	if lvl != "city" && lvl != "location" && lvl != "cluster" {
		res.Errors = append(res.Errors, "Level must be City, Cluster or Location")
		return res, nil
	}
	if lvl == "cluster" {
		return bulkBudCluster(ctx, tx, c, r, res)
	}
	table := map[string]string{"city": "city", "location": "location"}[lvl]
	var x store.Row
	if lvl == "city" {
		var why string
		if x, why = bulkFindCity(ctx, tx, r.V["Name"]); why != "" {
			res.Errors = append(res.Errors, why)
			return res, nil
		}
	} else {
		x, _ = bulkFindCI(ctx, tx, table, "name", r.V["Name"], bson.D{{Key: "status", Value: "Active"}})
	}
	if x == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s “%s” is not active", r.V["Level"], r.V["Name"]))
		return res, nil
	}
	id, hc, pb := str(x["id"]), timectxInt64(x["headcount_budget"]), 0.0
	if lvl == "location" {
		pb = timectxNum(x["payroll_budget"])
	}
	// HRBP sees and sets only the budgets of mapped properties.
	if c.Scope.Access != "all" && (lvl == "city" || !containsStr(c.Scope.Locations, id)) {
		res.Errors = append(res.Errors, res.ID+" is outside your access")
		return res, nil
	}
	if v := r.V[budCostCol]; v != "" && lvl == "city" {
		res.Errors = append(res.Errors, "A city cost budget is the total of its locations. Leave it blank for a city row")
		return res, nil
	}
	// Every column is checked before anything is written: a row with a problem changes nothing.
	set := bson.M{}
	for col, cur := range map[string]float64{"Headcount Budget": float64(hc), budCostCol: pb} {
		v := r.V[col]
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			res.Errors = append(res.Errors, col+" must be zero or more")
			continue
		}
		if col == "Headcount Budget" && f != math.Trunc(f) { // named on its row, not a failed upload (SET-069)
			res.Errors = append(res.Errors, "Headcount Budget must be a whole number")
			continue
		}
		if f == cur {
			continue
		}
		var val any = f
		if col == "Headcount Budget" {
			val = int64(f)
		}
		set[map[string]string{"Headcount Budget": "headcount_budget", budCostCol: "payroll_budget"}[col]] = val
		show := func(x float64) string {
			if col == "Headcount Budget" {
				return strconv.FormatFloat(x, 'f', -1, 64)
			}
			return rupees(x)
		}
		res.Changes = append(res.Changes, bulkChange{col, show(cur), show(f), ""})
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	for dbcol, f := range set {
		if _, err := tx.C(table).Update(ctx, bson.D{{Key: "_id", Value: id}}, bson.M{"$set": bson.M{dbcol: f}}); err != nil {
			return res, err
		}
	}
	if len(res.Changes) > 0 {
		return res, audit(ctx, tx, c, "Budget updated for "+res.ID, table, id, nil, nil)
	}
	return res, nil
}

// bulkBudCluster is a Cluster row of the Budgets template: the cluster's own headcount and monthly cost budget (the
// Monthly Cost Budget (CTC) column). 0 clears the figure, so the cluster uses the total of its locations again.
func bulkBudCluster(ctx context.Context, tx store.Q, c *Ctx, r bulkRow, res bulkResult) (bulkResult, error) {
	if c.Scope.Access != "all" {
		res.Errors = append(res.Errors, res.ID+" is outside your access")
		return res, nil
	}
	all, err := tx.C("cluster").Find(ctx, bson.D{{Key: "name", Value: bulkCI(strings.TrimSpace(r.V["Name"]))}, {Key: "status", Value: "Active"}})
	if err != nil {
		return res, err
	}
	if len(all) != 1 {
		why := fmt.Sprintf("Cluster “%s” is not active", r.V["Name"])
		if len(all) > 1 {
			why = fmt.Sprintf("%d active clusters are named %s: set this one on Cost and budget", len(all), r.V["Name"])
		}
		res.Errors = append(res.Errors, why)
		return res, nil
	}
	x := all[0]
	set := bson.M{}
	for col, dbcol := range map[string]string{"Headcount Budget": "headcount_budget", budCostCol: "cost_budget"} {
		v := r.V[col]
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			res.Errors = append(res.Errors, col+" must be zero or more")
			continue
		}
		if col == "Headcount Budget" && f != math.Trunc(f) {
			res.Errors = append(res.Errors, "Headcount Budget must be a whole number")
			continue
		}
		cur := payengineFloat(x[dbcol])
		if f == cur && (f != 0 || x[dbcol] == nil) {
			continue
		}
		var val any
		if f > 0 {
			if col == "Headcount Budget" {
				val = int32(f)
			} else {
				val = f
			}
		}
		set[dbcol] = val
		show := func(x float64) string {
			if x == 0 {
				return "total of its locations"
			}
			if col == "Headcount Budget" {
				return strconv.FormatFloat(x, 'f', -1, 64)
			}
			return rupees(x)
		}
		res.Changes = append(res.Changes, bulkChange{col, show(cur), show(f), ""})
	}
	if len(res.Errors) > 0 || len(set) == 0 {
		return res, nil
	}
	if _, err := tx.C("cluster").Update(ctx, bson.D{{Key: "_id", Value: x["id"]}}, bson.M{"$set": set}); err != nil {
		return res, err
	}
	return res, audit(ctx, tx, c, "Budget updated for "+res.ID, "cluster", str(x["id"]), nil, set)
}

// The time set is loaded once per upload, not once per row.
type tsKey struct{}
type tsHolder struct{ ts *timeSet }

func timeSetFor(ctx context.Context, q store.Q) (*timeSet, error) {
	if h, ok := ctx.Value(tsKey{}).(*tsHolder); ok {
		if h.ts == nil {
			ts, err := loadTimeSet(ctx, q)
			if err != nil {
				return nil, err
			}
			h.ts = ts
		}
		return h.ts, nil
	}
	return loadTimeSet(ctx, q)
}

func bulkAtt(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	d := excelDate(r.V["Date"])
	in, out := strings.TrimSpace(r.V["In time"]), strings.TrimSpace(r.V["Out time"])
	_, okIn := attend.Min(in)
	_, okOut := attend.Min(out)
	switch {
	case !isDate(d):
		res.Errors = append(res.Errors, "Date must look like 2026-11-01")
	case d > today():
		res.Errors = append(res.Errors, "Date is in the future")
	case d < e["doj"].(string):
		res.Errors = append(res.Errors, "Date is before the joining date")
	case in == "" && out == "":
		res.Errors = append(res.Errors, "Enter an in time, an out time, or both")
	case (in != "" && !okIn) || (out != "" && !okOut):
		res.Errors = append(res.Errors, "Times must look like 09:30")
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	// A paid month's attendance stays as paid; a locked month changes only with an exception (spec 11.7, 15.1).
	if msg := paidMonthMsg(ctx, tx, d, d); msg != "" {
		res.Errors = append(res.Errors, msg)
		return res, nil
	}
	if ym := lockedMonthOf(ctx, tx, d); ym != "" && !exceptionOpen(ctx, tx, res.ID, "regularize", d, d) {
		res.Errors = append(res.Errors, fmt.Sprintf("Attendance for %s is locked by payroll. Ask the Payroll Manager for an exception", ymLabel(ym)))
		return res, nil
	}
	var oldIn, oldOut string
	day := bson.D{{Key: "employee_id", Value: res.ID}, {Key: "date", Value: d}}
	old, err := tx.C("attendance_day").FindOne(ctx, day)
	if err != nil {
		return res, err
	}
	if old != nil {
		oldIn, oldOut = str(old["in_time"]), str(old["out_time"])
		if old["source"] == "Regularized" {
			res.Errors = append(res.Errors, "This day was corrected by an approved regularization and is kept")
			return res, nil
		}
		if oldIn == in && oldOut == out {
			return res, nil
		}
	}
	if _, err := tx.C("attendance_day").Upsert(ctx, day, bson.M{"in_time": timectxNullIf(in), "out_time": timectxNullIf(out), "out_date": nil, "source": "MIS upload",
		"by_name": c.Name + " (" + c.Role + ")", "at": tx.Now()}, nil); err != nil {
		return res, err
	}
	from := "No record"
	if oldIn != "" || oldOut != "" {
		from = or(oldIn, "—") + " to " + or(oldOut, "—")
	}
	res.Changes = append(res.Changes, bulkChange{"Punches on " + longDate(d), from, or(in, "—") + " to " + or(out, "—"), map[bool]string{true: "replaces the day", false: "new day"}[from != "No record"]})
	return res, nil
}

func bulkRoster(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	if _, ok := bulkEmployee(ctx, tx, c, r, &res); !ok {
		return res, nil
	}
	d, code := excelDate(r.V["Date"]), strings.ToUpper(strings.TrimSpace(r.V["Code"]))
	if !isDate(d) || code == "" {
		res.Errors = append(res.Errors, "Give a date like 2026-11-01 and a code")
		return res, nil
	}
	ts, err := timeSetFor(ctx, tx)
	if err != nil {
		return res, err
	}
	e, err := loadEmp(ctx, tx, res.ID)
	if err != nil {
		return res, err
	}
	p, _ := ts.policyFor(e)
	m, err := loadMonth(ctx, tx, ts, []string{e.ID}, d, d)
	if err != nil {
		return res, err
	}
	cur := m.rosterCode(e, p, d)
	if cur != code && d < today() && p.S.RS.Past != "lock" && p.S.RS.BulkPast != "same" { // work policy › Past dates in the roster bulk upload
		res.Errors = append(res.Errors, longDate(d)+": date has passed. The bulk upload does not change past dates (work policy)")
		return res, nil
	}
	warn, late, err := s.rosterSet(ctx, tx, c, ts, e, d, code, r.V["Reason"], "")
	if err != nil {
		return res, err
	}
	if cur != code {
		note := longDate(d)
		if late {
			note += " · late change"
		}
		if warn != "" {
			note += " · " + warn
		}
		res.Changes = append(res.Changes, bulkChange{"Roster", cur, code, note})
	}
	return res, nil
}

// optHolCol is the optional holiday balance column of Opening leave balances (the template "Opening optional holiday
// balances" of 14 Nov 2026, merged into it on 17 Nov 2026).
const optHolCol = "Optional Holiday Balance"

// bulkOpening is a row of Opening leave balances: a leave type's balance, the optional holiday balance, or both, as at
// a date. Both parts are checked before either is written; a blank cell leaves that balance as it is.
func bulkOpening(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	lt, at := strings.TrimSpace(r.V["Leave type"]), excelDate(r.V["As at"])
	balS, ohS := strings.TrimSpace(r.V["Balance"]), strings.TrimSpace(r.V[optHolCol])
	leave, opt := lt != "" || balS != "", ohS != ""
	// A balance equal to the one stored for the employee, type and date is no change (the current data uploaded as
	// it is, bulk_current.go), whatever has changed in the leave setup since.
	stored := func(coll string, key bson.D, v string) bool {
		f, err := strconv.ParseFloat(v, 64)
		x, _ := tx.C(coll).FindOne(ctx, key)
		return err == nil && x != nil && timectxNum(x["balance"]) == f
	}
	if leave && stored("leave_opening", bson.D{{Key: "employee_id", Value: res.ID}, {Key: "leave_type", Value: lt}, {Key: "at", Value: at}}, balS) {
		leave = false
	}
	if opt && stored(store.OptHolOpeningColl, bson.D{{Key: "employee_id", Value: res.ID}, {Key: "at", Value: at}}, ohS) {
		opt = false
	}
	switch {
	case !leave && !opt:
		return res, nil // blank cells (or nothing new): the balances stay as they are
	case !isDate(at) || at > today():
		res.Errors = append(res.Errors, "As at must be a date up to today")
	case at < str(e["doj"]):
		res.Errors = append(res.Errors, "As at is before the joining date")
	}
	bal, err := strconv.ParseFloat(balS, 64)
	if leave {
		switch {
		case !func() bool {
			ok, _ := tx.C("leave_type").Exists(ctx, bson.D{{Key: "name", Value: lt}, {Key: "kind", Value: "balance"}, {Key: "status", Value: "Active"}})
			return ok
		}():
			res.Errors = append(res.Errors, fmt.Sprintf("“%s” is not a leave type with a balance", lt))
		case leaveTypeRefusal(str(e["type"]), lt) != "":
			res.Errors = append(res.Errors, leaveTypeRefusal(str(e["type"]), lt))
		case err != nil || bal < -60 || bal > 400:
			res.Errors = append(res.Errors, "Balance must be a number")
		}
	}
	oh, err := strconv.ParseFloat(ohS, 64)
	if opt {
		switch {
		case !calc.RulesFor(calc.EmploymentType(str(e["type"]))).Leave:
			res.Errors = append(res.Errors, fmt.Sprintf("%s staff have no leave or optional holidays", e["type"]))
		case err != nil || oh < 0 || oh > 31 || oh*2 != float64(int(oh*2)):
			res.Errors = append(res.Errors, optHolCol+" must be a number from 0 to 31 (halves allowed)")
		}
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	if leave {
		if err := openingSet(ctx, tx, c, &res, "leave_opening", bson.D{{Key: "employee_id", Value: res.ID}, {Key: "leave_type", Value: lt}, {Key: "at", Value: at}}, bal, lt+" opening", at); err != nil {
			return res, err
		}
	}
	if opt {
		if err := openingSet(ctx, tx, c, &res, store.OptHolOpeningColl, bson.D{{Key: "employee_id", Value: res.ID}, {Key: "at", Value: at}}, oh, "Optional holidays opening", at); err != nil {
			return res, err
		}
	}
	return res, nil
}

// openingSet stores an opening balance (leave_opening or opt_hol_opening) under its key, noting the change.
func openingSet(ctx context.Context, tx store.Q, c *Ctx, res *bulkResult, coll string, key bson.D, bal float64, field, at string) error {
	var old *float64
	if x, _ := tx.C(coll).FindOne(ctx, key); x != nil {
		b := timectxNum(x["balance"])
		old = &b
	}
	if old != nil && *old == bal {
		return nil
	}
	if _, err := tx.C(coll).Upsert(ctx, key, bson.M{"balance": bal, "created_by": c.Name, "created_at": tx.Now()}, nil); err != nil {
		return err
	}
	from := "—"
	if old != nil {
		from = strconv.FormatFloat(*old, 'f', -1, 64)
	}
	res.Changes = append(res.Changes, bulkChange{field, from, strconv.FormatFloat(bal, 'f', -1, 64), "as at " + longDate(at)})
	return nil
}
