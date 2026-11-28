package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/attend"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
	"github.com/xuri/excelize/v2"
)

// Roster, attendance and punch-machine pieces the prototype shows (vRos, vAtt, vMis, vApi):
// a pattern applied to everyone on the roster, each day's attendance category for a range, the
// roster download with one column per date (and its upload), recent attendance uploads, and the
// settings for pulling punches from the office server.

// ---------- Roster: who is shown ----------

// rosterEmps lists the active employees in the caller's scope at a property, or in a whole city,
// optionally in one department (prototype rosterEmps). Ordered by employee ID.
func rosterEmps(ctx context.Context, q store.Q, c *Ctx, loc, city, dept string) ([]*emp, error) {
	if loc == "" && city == "" {
		return nil, bad("Choose a city or a property")
	}
	f := bson.D{{Key: "status", Value: "Active"}}
	if loc != "" {
		f = append(f, bson.E{Key: "location_id", Value: loc})
	}
	return scopedEmps(ctx, q, c, f, ui2AttendanceIn(city, dept))
}

// ui2AttendanceIn keeps employees of a city and a department name ("" for any).
func ui2AttendanceIn(city, dept string) func(*emp) bool {
	return func(e *emp) bool { return (city == "" || e.CityID == city) && (dept == "" || e.Dept == dept) }
}

// codesAt is the roster codes usable at any of the locations (for some department): the shifts mapped there, then WO
// and PH. One employee's codes are codesFor (shift_maps.go).
func (t *timeSet) codesAt(locs ...string) []string {
	out := []string{}
	for _, sh := range t.list {
		for _, l := range locs {
			if sh.RunsAt(l) {
				out = append(out, sh.Code)
				break
			}
		}
	}
	return append(out, "WO", "PH")
}

func weekDays(wk string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = policy.AddDays(wk, i)
	}
	return out
}

// ---------- Apply a pattern to everyone shown (prototype rpat) ----------

var rosterPatterns = map[string]string{"fixed": "Reset to work policy defaults", "rot": "Rotate Morning, Evening, Night weekly", "6on1": "Six on, one off (staggered off day)"}

// rosterPattern sets the week for everyone shown: back to the policy defaults, Morning / Evening /
// Night in rotation, or six days on and one off with the off day staggered. Each change goes
// through the same rules as a single cell (past dates locked, late changes need a reason, minimum
// rest); changes the rules refuse are counted, not made. Approved leave days are left alone.
func (s *Server) rosterPattern(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("ros.roster", 2); err != nil {
		return nil, err
	}
	var b struct {
		Location   string `json:"location"`
		City       string `json:"city"`
		Department string `json:"department"`
		Week       string `json:"week"`
		Pattern    string `json:"pattern"`
		Why        string `json:"why"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if rosterPatterns[b.Pattern] == "" {
		return nil, bad("Choose a pattern")
	}
	if !isDate(b.Week) {
		return nil, bad("Choose the week")
	}
	ctx := r.Context()
	emps, err := rosterEmps(ctx, s.st.DB, c, b.Location, b.City, b.Department)
	if err != nil {
		return nil, err
	}
	if len(emps) == 0 {
		return nil, bad("No active employees in this city and department for your scope")
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	wk := weekStart(b.Week)
	days := weekDays(wk, 7)
	applied, blocked := 0, 0
	err = s.st.Tx(ctx, func(tx store.Q) error {
		applied, blocked = 0, 0 // the transaction may run again
		m, err := loadMonth(ctx, tx, ts, ids(emps), days[0], days[6])
		if err != nil {
			return err
		}
		for k, e := range emps {
			p, _ := ts.policyFor(e)
			hol := ts.holidays(p, e.CityID)
			ops := []string{}
			for _, code := range []string{"M", "E", "N"} {
				if ts.shiftFor(code, e) {
					ops = append(ops, code)
				}
			}
			want := map[string]string{}
			for i, d := range days {
				if d < e.DOJ || (e.LWD != nil && d > *e.LWD) || m.planned(e, p, d).Code == "L" {
					continue // outside employment, or approved leave: nothing to plan
				}
				def := p.S.DefCode(e.ID, ts.baseShift(e), d, hol)
				tgt := def
				if b.Pattern != "fixed" && len(ops) > 0 && def != "PH" {
					switch {
					case b.Pattern == "rot" && def != "WO":
						half := 0
						if i >= 3 {
							half = 1
						}
						tgt = ops[(k+half)%len(ops)]
					case b.Pattern == "6on1" && i == k%7:
						tgt = "WO"
					case b.Pattern == "6on1" && def == "WO":
						tgt = ts.baseShift(e)
					}
				}
				if tgt != m.rosterCode(e, p, d) {
					want[d] = tgt
				}
			}
			// A change can be refused for the rest gap with the day after as it still is; that day
			// changes too, so refused days are tried again until a pass changes nothing more.
			for progress := true; progress && len(want) > 0; {
				progress = false
				for _, d := range days {
					tgt, ok := want[d]
					if !ok {
						continue
					}
					if _, _, err := s.rosterSet(ctx, tx, c, ts, e, d, tgt, b.Why, ""); err != nil {
						var ae *apiError
						if !errors.As(err, &ae) {
							return err
						}
						continue // the rules refused it before anything was written
					}
					delete(want, d)
					applied++
					progress = true
				}
			}
			blocked += len(want)
		}
		if applied == 0 {
			return nil
		}
		return audit(ctx, tx, c, fmt.Sprintf("Roster pattern “%s” applied for the week of %s: %d change%s for %d employees", rosterPatterns[b.Pattern], longDate(wk), applied,
			map[bool]string{true: "", false: "s"}[applied == 1], len(emps)), "roster", or(b.Location, b.City), nil, b)
	})
	return map[string]any{"applied": applied, "blocked": blocked, "employees": len(emps), "week": wk}, err
}

// ---------- Attendance categories for a range (prototype attDay) ----------

// attCats are the day categories of the attendance summary, with the letter each has in the
// compact rows of /attendance/summary.
var attCats = []struct{ Name, Letter string }{{"Present", "P"}, {"Late", "L"}, {"Half day", "H"}, {"Absent", "A"}, {"Missing punch", "M"},
	{"On leave", "V"}, {"Week off", "W"}, {"Holiday", "O"}}

// attCategory is the prototype's attDay: "" before joining, after leaving, or in the future;
// On leave for approved paid leave with no punches; otherwise from the day's status.
func (m *monthData) attCategory(e *emp, p *workPolicy, d string) string {
	if d > m.today || d < e.DOJ || (e.LWD != nil && d > *e.LWD && e.Status != "Active") {
		return ""
	}
	if l := m.leaveOn(e, d, false); l != nil && !m.unpaid(l.Type) {
		if rec := m.rec(e, p, d); rec == nil || rec.Empty() {
			return "On leave"
		}
	}
	st, _ := m.status(e, p, d)
	st = attend.BaseStatus(st) // "Present until regularized" (a deviation allowance's action) is present
	switch {
	case st == "Present", st == "Late", st == "Half day", st == "Week off", st == "Holiday":
		return st
	case strings.Contains(st, "Missing"):
		return "Missing punch"
	}
	return "Absent"
}

// attendanceSummary returns each employee's category for every day of a range, in one pass (one
// set of queries for the whole range, not one per day). A row's "k" has one letter a day, in the
// order of "days"; "-" is a day outside employment or still to come.
func (s *Server) attendanceSummary(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.records", 1); err != nil {
		return nil, err
	}
	ctx, qv := r.Context(), r.URL.Query()
	to := or(qv.Get("to"), policy.AddDays(today(), -1))
	from := or(qv.Get("from"), to[:min(7, len(to))]+"-01")
	if !isDate(from) || !isDate(to) || to < from || policy.DayDiff(from, to) > 62 {
		return nil, bad("Choose a range of up to two months")
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	// Everyone employed at some point of the range (joined by its end, not left before its start, or still
	// active), optionally at a property, in a city, in a department or of an employment type.
	f := bson.D{{Key: "status", Value: bson.D{{Key: "$ne", Value: "Pre-boarding"}}}, {Key: "doj", Value: bson.D{{Key: "$lte", Value: to}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "lwd", Value: nil}}, bson.D{{Key: "lwd", Value: bson.D{{Key: "$gte", Value: from}}}}, bson.D{{Key: "status", Value: "Active"}}}}}
	if v := qv.Get("location"); v != "" {
		f = append(f, bson.E{Key: "location_id", Value: v})
	}
	if v := qv.Get("type"); v != "" {
		f = append(f, bson.E{Key: "type", Value: v})
	}
	emps, err := scopedEmps(ctx, s.st.DB, c, f, ui2AttendanceIn(qv.Get("city"), qv.Get("department")))
	if err != nil {
		return nil, err
	}
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), from, to)
	if err != nil {
		return nil, err
	}
	days := []string{}
	for d := from; d <= to; d = policy.AddDays(d, 1) {
		days = append(days, d)
	}
	letter, legend := map[string]byte{}, map[string]string{}
	for _, x := range attCats {
		letter[x.Name], legend[x.Letter] = x.Letter[0], x.Name
	}
	rows := make([]map[string]string, 0, len(emps))
	k := make([]byte, len(days))
	for _, e := range emps {
		p, _ := ts.policyFor(e)
		for i, d := range days {
			k[i] = '-'
			if cat := m.attCategory(e, p, d); cat != "" {
				k[i] = letter[cat]
			}
		}
		rows = append(rows, map[string]string{"id": e.ID, "name": e.Name, "k": string(k)})
	}
	return map[string]any{"from": from, "to": to, "days": days, "legend": legend, "rows": rows}, nil
}

// ---------- Roster download: one column per date (prototype rbDownload) ----------

const rosterWhyCol = "Reason for late changes"

// rosterDownload is the roster for a city or property and department as Excel: one row per
// employee, one column per date holding the current code (L for approved leave), drop-downs of the
// codes valid at each employee's property, and a last column for the reason of late changes.
// Without a city, property or department it is the plain template of the Bulk update page.
func (s *Server) rosterDownload(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	// department: one or more (repeated); none, or "*", is every department in the city / property (owner, 27 Nov 2026).
	loc, city := qv.Get("location"), qv.Get("city")
	depts := map[string]bool{}
	for _, d := range qv["department"] {
		if d = strings.TrimSpace(d); d == "*" {
			depts = map[string]bool{}
			break
		} else if d != "" {
			depts[d] = true
		}
	}
	if loc == "" && city == "" && len(depts) == 0 {
		r.SetPathValue("tpl", "roster")
		s.bulkDownload(w, r)
		return
	}
	fail := func(err error) { writeErr(w, err) }
	c, err := s.authenticate(r)
	if err != nil {
		fail(err)
		return
	}
	if err := c.Need("ros.roster", 1); err != nil {
		fail(err)
		return
	}
	if !c.CanDownload {
		fail(forbidden("Your role cannot download files"))
		return
	}
	from := or(qv.Get("from"), weekStart(today()))
	n, _ := strconv.Atoi(or(qv.Get("days"), "14"))
	if to := qv.Get("to"); to != "" { // From … To (inclusive) instead of a number of days
		if !isDate(to) || !isDate(from) || to < from {
			fail(bad("The To date must be on or after the From date"))
			return
		}
		n = policy.DayDiff(from, to) + 1
	}
	if !isDate(from) || n < 1 || n > 62 {
		fail(bad("Choose a start date and up to 62 days"))
		return
	}
	ctx := r.Context()
	emps, err := rosterEmps(ctx, s.st.DB, c, loc, city, "")
	if err != nil {
		fail(err)
		return
	}
	if len(depts) > 0 {
		keep := emps[:0]
		for _, e := range emps {
			if depts[e.Dept] {
				keep = append(keep, e)
			}
		}
		emps = keep
	}
	if len(emps) == 0 {
		fail(bad("No employees match the city, property and department chosen"))
		return
	}
	ts, err := loadTimeSet(ctx, s.st.DB)
	if err != nil {
		fail(err)
		return
	}
	days := weekDays(from, n)
	m, err := loadMonth(ctx, s.st.DB, ts, ids(emps), days[0], days[n-1])
	if err != nil {
		fail(err)
		return
	}
	// Rows grouped by property and department, so each pair's drop-down (the shifts mapped to it) covers one block of rows.
	sort.SliceStable(emps, func(i, j int) bool {
		if emps[i].Location != emps[j].Location {
			return emps[i].Location < emps[j].Location
		}
		if emps[i].Dept != emps[j].Dept {
			return emps[i].Dept < emps[j].Dept
		}
		return emps[i].ID < emps[j].ID
	})
	head := append(append([]string{"Employee ID", "Name", "Property", "Department"}, days...), rosterWhyCol)
	f := excelize.NewFile()
	defer f.Close()
	const sh = "Roster"
	_ = f.SetSheetName("Sheet1", sh)
	hs, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1}})
	for i, h := range head {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sh, cell, h)
		col, _ := excelize.ColumnNumberToName(i + 1)
		width := 11.0
		switch {
		case i == 1:
			width = 26
		case i == 2:
			width = 30
		case i == 0 || i == 3:
			width = 16
		case h == rosterWhyCol:
			width = 30
		}
		_ = f.SetColWidth(sh, col, col, width)
	}
	last, _ := excelize.ColumnNumberToName(len(head))
	_ = f.SetCellStyle(sh, "A1", last+"1", hs)
	_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, XSplit: 2, YSplit: 1, TopLeftCell: "C2", ActivePane: "bottomRight"})
	type block struct{ first, last int }
	blocks, order := map[string]*block{}, []string{}
	for i, e := range emps {
		p, _ := ts.policyFor(e)
		row := []any{e.ID, e.Name, e.Location, e.Dept}
		for _, d := range days {
			row = append(row, m.planned(e, p, d).Code)
		}
		row = append(row, nil)
		_ = f.SetSheetRow(sh, fmt.Sprintf("A%d", i+2), &row)
		if b := blocks[e.LocID+"|"+e.DeptID]; b != nil {
			b.last = i + 2
		} else {
			blocks[e.LocID+"|"+e.DeptID], order = &block{i + 2, i + 2}, append(order, e.LocID+"|"+e.DeptID)
		}
	}
	// Drop-downs from a hidden sheet: the reasons, then the codes valid at each property.
	_, _ = f.NewSheet("Lists")
	list := func(col int, title string, vals []string) string {
		lc, _ := excelize.ColumnNumberToName(col)
		_ = f.SetCellValue("Lists", lc+"1", title)
		for j, v := range vals {
			_ = f.SetCellValue("Lists", fmt.Sprintf("%s%d", lc, j+2), v)
		}
		return fmt.Sprintf("Lists!$%s$2:$%s$%d", lc, lc, len(vals)+1)
	}
	firstDay, _ := excelize.ColumnNumberToName(5)
	lastDay, _ := excelize.ColumnNumberToName(4 + n)
	for i, key := range order {
		b := blocks[key]
		var first *emp
		for _, e := range emps {
			if e.LocID+"|"+e.DeptID == key {
				first = e
				break
			}
		}
		dv := excelize.NewDataValidation(true)
		dv.Sqref = fmt.Sprintf("%s%d:%s%d", firstDay, b.first, lastDay, b.last)
		dv.SetSqrefDropList(list(i+2, first.Dept+" at "+first.Location, ts.codesFor(first)))
		dv.SetError(excelize.DataValidationErrorStyleStop, "Not a code for this department and property", "Choose a shift mapped to the department and property, WO or PH.")
		_ = f.AddDataValidation(sh, dv)
	}
	dv := excelize.NewDataValidation(true)
	dv.Sqref = fmt.Sprintf("%s2:%s%d", last, last, len(emps)+1)
	dv.SetSqrefDropList(list(1, rosterWhyCol, rosterReasons))
	_ = f.AddDataValidation(sh, dv)
	_ = f.SetSheetVisible("Lists", false)
	// How to fill (prototype notes).
	// The city named, or the city of the property.
	cityID := city
	if city == "" {
		if l, _ := s.st.DB.C("location").FindOne(ctx, bson.D{{Key: "_id", Value: loc}}); l != nil {
			if k, _ := s.st.DB.C("cluster").FindOne(ctx, bson.D{{Key: "_id", Value: l["cluster_id"]}}); k != nil {
				cityID = str(k["city_id"])
			}
		}
	}
	var place string
	if cityID != "" {
		if x, _ := s.st.DB.C("city").FindOne(ctx, bson.D{{Key: "_id", Value: cityID}}); x != nil {
			place = str(x["name"])
		}
	}
	if loc != "" {
		place += ", " + emps[0].Location
	}
	if len(depts) > 0 {
		ds := make([]string, 0, len(depts))
		for d := range depts {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		place += ", " + strings.Join(ds, ", ")
	} else {
		place += ", all departments"
	}
	notes := []string{
		fmt.Sprintf("Roster for %s. %s to %s.", strings.TrimPrefix(place, ", "), longDate(days[0]), longDate(days[n-1])),
		"Each date column holds the shift code for that day: a shift code, WO for weekly off or PH for public holiday. Change only the cells you need.",
		"L means approved leave. It cannot be changed here. Cancel the leave instead.",
		"A shift can only be used at a property it is mapped to. A change inside the lock window before the date is a late change and needs a reason in the last column.",
		"Past dates are not changed by this upload unless the work policy allows it (Past dates in the roster bulk upload).",
	}
	_, _ = f.NewSheet("How to fill")
	_ = f.SetCellValue("How to fill", "A1", "Roster upload")
	for i, t := range notes {
		_ = f.SetCellValue("How to fill", fmt.Sprintf("A%d", i+3), t)
	}
	_ = f.SetColWidth("How to fill", "A", "A", 120)
	f.SetActiveSheet(0)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		fail(err)
		return
	}
	name := fmt.Sprintf("roster-%s-%ddays.xlsx", days[0], n)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(buf.Bytes())
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Downloaded the roster for %s, %s to %s", strings.TrimPrefix(place, ", "), longDate(days[0]), longDate(days[n-1])), "roster", or(loc, city), nil,
		map[string]any{"employees": len(emps), "days": n})
}

// ---------- Roster upload: the layout above (one column per date) ----------

// rosterDateCol reports whether a heading is a date, and which.
func rosterDateCol(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if h == "" || strings.EqualFold(h, "Date") {
		return "", false
	}
	d := excelDate(h)
	return d, isDate(d)
}

// rosterWideTemplate switches a roster upload to the one-column-per-date layout when its headings
// have an Employee ID and date columns but no Date and Code columns. Nil otherwise.
func rosterWideTemplate(t *bulkTemplate, head []string) *bulkTemplate {
	if t.Key != "roster" || !containsStr(head, "Employee ID") || (containsStr(head, "Date") && containsStr(head, "Code")) {
		return nil
	}
	for _, h := range head {
		if _, ok := rosterDateCol(h); ok {
			return &bulkTemplate{Key: "roster-wide", Name: t.Name, Hint: t.Hint, allow: t.allow, Cols: []string{"Employee ID", h}, row: bulkRosterWide}
		}
	}
	return nil
}

// bulkRosterWide checks and applies one employee's row of the wide roster (prototype rbCheck):
// every date cell that differs from the roster is a change under the roster rules. A row with any
// problem changes nothing.
func bulkRosterWide(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	if _, ok := bulkEmployee(ctx, tx, c, r, &res); !ok {
		return res, nil
	}
	e, err := loadEmp(ctx, tx, res.ID)
	if err != nil {
		return res, err
	}
	if e.Status != "Active" {
		res.Errors = append(res.Errors, e.Name+" is not active")
		return res, nil
	}
	type cell struct{ d, v string }
	cells, why := []cell{}, ""
	for h, v := range r.V {
		if d, ok := rosterDateCol(h); ok {
			if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
				cells = append(cells, cell{d, v})
			}
		} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "reason") {
			why = strings.TrimSpace(v)
		}
	}
	if len(cells) == 0 {
		return res, nil
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].d < cells[j].d })
	ts, err := timeSetFor(ctx, tx)
	if err != nil {
		return res, err
	}
	p, _ := ts.policyFor(e)
	m, err := loadMonth(ctx, tx, ts, []string{e.ID}, cells[0].d, cells[len(cells)-1].d)
	if err != nil {
		return res, err
	}
	codes := ts.codesFor(e)
	// Every cell is checked first, against the roster as the cells before it in the row leave it (ov); a row with
	// any problem changes nothing. Then the changes are made.
	ov := *m
	ov.roster = map[string]map[string]rosterCell{e.ID: {}}
	for d, x := range m.roster[e.ID] {
		ov.roster[e.ID][d] = x
	}
	type change struct{ d, code string }
	var todo []change
	for _, x := range cells {
		lab := longDate(x.d)
		cur := m.planned(e, p, x.d).Code
		if x.v == cur {
			continue
		}
		lead := policy.DayDiff(today(), x.d)
		switch {
		case cur == "L":
			res.Errors = append(res.Errors, lab+": approved leave, cancel the leave instead")
		case x.v == "L":
			res.Errors = append(res.Errors, lab+": leave is set by a leave request")
		case !containsStr(codes, x.v):
			res.Errors = append(res.Errors, fmt.Sprintf("%s: “%s” is not a shift mapped to %s at %s", lab, x.v, e.Dept, e.Location))
		case x.d < e.DOJ || (e.LWD != nil && x.d > *e.LWD):
			res.Errors = append(res.Errors, lab+": outside the employment dates")
		case lead < 0 && p.S.RS.Past == "lock":
			res.Errors = append(res.Errors, lab+": date has passed and is locked")
		case lead < 0 && p.S.RS.BulkPast != "same": // work policy › Past dates in the roster bulk upload
			res.Errors = append(res.Errors, lab+": date has passed. The bulk upload does not change past dates (work policy)")
		case lead < p.S.RS.Lead && p.S.RS.Reason && why == "":
			res.Errors = append(res.Errors, lab+": late change, add a reason in the last column")
		default:
			was, warn, late, err := rosterCheck(ts, e, x.d, x.v, why, "", func() (*monthData, error) { return &ov, nil })
			if err != nil {
				var ae *apiError
				if !errors.As(err, &ae) {
					return res, err
				}
				res.Errors = append(res.Errors, lab+": "+strings.TrimPrefix(ae.Message, "Not changed. "))
				continue
			}
			note := lab
			if late {
				note += " · late change"
			}
			if warn != "" {
				note += " · " + warn
			}
			res.Changes = append(res.Changes, bulkChange{"Roster", cur, x.v, note})
			if was != x.v {
				todo = append(todo, change{x.d, x.v})
				ov.rosterOverlay(e, p, x.d, x.v)
			}
		}
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	for _, x := range todo {
		if _, _, err := s.rosterSet(ctx, tx, c, ts, e, x.d, x.code, why, ""); err != nil {
			return res, fmt.Errorf("roster upload, %s: %w", longDate(x.d), err) // checked above: only a database error gets here
		}
	}
	return res, nil
}

// ---------- Recent attendance uploads ----------

// attUploadLog lists the latest applied attendance uploads (prototype vMis "Recent uploads").
func (s *Server) attUploadLog(r *http.Request, c *Ctx) (any, error) {
	if _, err := templateFor(c, "att"); err != nil {
		return nil, err
	}
	rows, err := s.st.DB.C("audit_log").Find(r.Context(), bson.D{{Key: "entity", Value: "bulk"}, {Key: "entity_id", Value: "att"}, {Key: "action", Value: bson.D{{Key: "$regex", Value: "^Bulk update"}}}},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(20).
			SetProjection(bson.D{{Key: "actor", Value: 1}, {Key: "actor_role", Value: 1}, {Key: "at", Value: 1}, {Key: "after.rows", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := []store.Row{}
	for _, x := range rows {
		after, _ := x["after"].(map[string]any)
		out = append(out, store.Row{"by": x["actor"], "role": str(x["actor_role"]), "at": x["at"], "rows": int32(timectxNum(after["rows"]))})
	}
	return out, nil
}

// ---------- Punch machines: pull from the office server's API (prototype vApi) ----------

type pullConfig struct {
	URL    string  `json:"url"`
	Method string  `json:"method"`
	Auth   string  `json:"auth"`
	Key    *string `json:"key,omitempty"` // write-only: never returned
	Since  string  `json:"since"`
	Every  int     `json:"every"`
	EmpF   string  `json:"empF"`
	TimeF  string  `json:"timeF"`
	DirF   string  `json:"dirF"`
	InV    string  `json:"inV"`
	OutV   string  `json:"outV"`
}

var pullAuths = []string{"API key", "Bearer token", "Username and password", "None"}

func loadPull(ctx context.Context, q store.Q) (pullConfig, string, store.Row, error) {
	var p pullConfig
	r, err := q.C("attendance_pull_config").FindOne(ctx, bson.D{{Key: "_id", Value: 1}})
	if err != nil {
		return p, "", nil, err
	}
	if r == nil {
		p = pullConfig{Method: "GET", Auth: "API key", Since: "from", Every: 15, EmpF: "emp_code", TimeF: "punch_time", DirF: "punch_type", InV: "IN", OutV: "OUT"}
		return p, "", nil, nil
	}
	p = pullConfig{URL: str(r["url"]), Method: str(r["method"]), Auth: str(r["auth"]), Since: str(r["since_param"]), Every: timectxInt(r["every_min"]),
		EmpF: str(r["emp_field"]), TimeF: str(r["time_field"]), DirF: str(r["dir_field"]), InV: str(r["in_value"]), OutV: str(r["out_value"])}
	meta := store.Row{}
	for _, k := range []string{"tested_at", "test_ok", "test_note", "updated_by", "updated_at", "last_pull_at", "last_pull_ok", "last_pull_note", "last_pull_count"} {
		meta[k] = r[k]
	}
	return p, str(r["secret"]), meta, nil
}

func (s *Server) pullConfigGet(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 1); err != nil {
		return nil, err
	}
	p, secret, meta, err := loadPull(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"url": p.URL, "method": p.Method, "auth": p.Auth, "since": p.Since, "every": p.Every, "empF": p.EmpF, "timeF": p.TimeF,
		"dirF": p.DirF, "inV": p.InV, "outV": p.OutV, "keySet": secret != "", "auths": pullAuths}
	for k, v := range meta {
		out[k] = v
	}
	return out, nil
}

// checkPull trims and validates the settings.
func checkPull(p *pullConfig) error {
	for _, f := range []*string{&p.URL, &p.Method, &p.Auth, &p.Since, &p.EmpF, &p.TimeF, &p.DirF, &p.InV, &p.OutV} {
		*f = strings.TrimSpace(*f)
	}
	p.Method = strings.ToUpper(or(p.Method, "GET"))
	switch {
	case p.URL != "" && !validPullURL(p.URL):
		return bad("Give the server API address as http:// or https:// with the server name or IP")
	case p.Method != "GET" && p.Method != "POST":
		return bad("Request method is GET or POST")
	case !containsStr(pullAuths, p.Auth):
		return bad("Choose how the server authenticates")
	case p.Every < 1 || p.Every > 1440:
		return bad("Pull every 1 to 1,440 minutes")
	case p.EmpF == "" || p.TimeF == "":
		return bad("Give the employee code field and the punch time field")
	case p.DirF != "" && (p.InV == "" || p.OutV == ""):
		return bad("Give the values that mean IN and OUT")
	}
	return nil
}

func pullHost(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return strings.ToLower(u.Host)
	}
	return ""
}

func validPullURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil
}

func (s *Server) pullConfigPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	var b pullConfig
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := checkPull(&b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	key, keep := "", true
	if b.Key != nil && strings.TrimSpace(*b.Key) != "" {
		key, keep = strings.TrimSpace(*b.Key), false
	}
	if b.Auth == "None" {
		key, keep = "", false
	}
	b.Key = nil
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		set := bson.M{"url": b.URL, "method": b.Method, "auth": b.Auth, "since_param": b.Since, "every_min": b.Every, "emp_field": b.EmpF, "time_field": b.TimeF,
			"dir_field": b.DirF, "in_value": b.InV, "out_value": b.OutV, "updated_by": c.Name, "updated_at": tx.Now()}
		old, err := tx.C("attendance_pull_config").Lock(ctx, bson.D{{Key: "_id", Value: 1}})
		if err != nil {
			return err
		}
		if old == nil { // the first save: the key as given (blank when none was entered)
			set["secret"] = key
			_, err = tx.C("attendance_pull_config").Upsert(ctx, bson.D{{Key: "_id", Value: 1}}, set, nil)
		} else {
			if !keep {
				set["secret"] = key
			}
			if old["url"] != b.URL { // the last test was of another address
				set["tested_at"], set["test_ok"], set["test_note"] = nil, nil, nil
			}
			_, err = tx.C("attendance_pull_config").Update(ctx, bson.D{{Key: "_id", Value: 1}}, bson.M{"$set": set})
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, c, "Punch machine API settings saved", "attendance_pull_config", "1", nil, map[string]any{"settings": b, "keyChanged": !keep})
	})
}

// pullDialer refuses loopback and link-local addresses (the server's own ports, cloud metadata).
// PULL_ALLOW_LOCAL=1 lifts it, for tests against a local stand-in server.
var pullDialer = &net.Dialer{Timeout: 5 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
	if os.Getenv("PULL_ALLOW_LOCAL") == "1" {
		return nil
	}
	host, _, _ := net.SplitHostPort(address)
	if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return errPullBlocked
	}
	return nil
}}

var errPullBlocked = errors.New("blocked address")

// pullErr describes a failed request in words, without the address, the key or the URL.
func pullErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.Is(err, errPullBlocked):
		return "This address is not allowed: it points at this server or a local-only address"
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err):
		return "No answer within 5 seconds. Check the address, the port and that the server is reachable from the internet"
	case errors.As(err, &dns):
		return "The server name could not be found"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "The server refused the connection. Check the port and that the API service is running"
	case errors.As(err, &op):
		return "Could not connect to the server"
	case strings.Contains(err.Error(), "tls") || strings.Contains(err.Error(), "certificate"):
		return "The secure (https) connection failed. Check the certificate or use the address the vendor gave"
	}
	return "Could not reach the server"
}

// pullConfigTest calls the server API once with a short timeout and reports what came back: the
// status, the content type, the size, how many records it holds and whether the mapped fields
// are there. The form's unsaved values are tested when sent; a blank key uses the saved one.
// Nothing secret is returned: no key, no URL, no headers.
func (s *Server) pullConfigTest(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("att.setup", 2); err != nil {
		return nil, err
	}
	ctx := r.Context()
	saved, secret, _, err := loadPull(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	p := saved
	raw, _ := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if len(bytes.TrimSpace(raw)) > 0 {
		var b pullConfig
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			return nil, bad("Check the fields: %s", strings.TrimPrefix(err.Error(), "json: "))
		}
		switch {
		case b.Key != nil && strings.TrimSpace(*b.Key) != "":
			secret = strings.TrimSpace(*b.Key)
		case pullHost(b.URL) != pullHost(saved.URL):
			secret = "" // the saved key only ever goes to the saved server
		}
		b.Key, p = nil, b
		if err := checkPull(&p); err != nil {
			return nil, err
		}
	}
	if p.URL == "" {
		return nil, bad("Give the server API address first")
	}
	if !validPullURL(p.URL) {
		return nil, bad("Give the server API address as http:// or https:// with the server name or IP")
	}
	since := time.Now().In(ist).Add(-time.Hour).Format("2006-01-02 15:04:05")
	out := map[string]any{"ok": false, "since": since}
	note := ""
	if p.Auth != "None" && secret == "" {
		out["warning"] = "The request was sent without a key or token. Enter it in Key or token (a saved key is only sent to the saved address)"
	}
	status, ctype, data, err := pullDo(ctx, p, secret, since, 1<<20)
	if err != nil {
		note = pullErr(err)
	} else {
		out["status"], out["contentType"], out["bytes"] = status, ctype, len(data)
		switch {
		case status == 401 || status == 403:
			note = fmt.Sprintf("The server answered %d: the key, token or password was not accepted", status)
		case status >= 300 && status < 400:
			note = fmt.Sprintf("The server answered %d and points elsewhere. Use the final address", status)
		case status >= 400:
			note = fmt.Sprintf("The server answered %d %s", status, http.StatusText(status))
		default:
			recs, ok := pullRecords(data)
			if !ok {
				note = "Connected, but the answer is not JSON. Ask the vendor for a JSON response"
				break
			}
			mapped, sample := 0, map[string]string(nil)
			for _, x := range recs {
				emp, t := fmt.Sprint(x[p.EmpF]), fmt.Sprint(x[p.TimeF])
				if x[p.EmpF] == nil || x[p.TimeF] == nil || emp == "" || t == "" {
					continue
				}
				mapped++
				if sample == nil {
					dir := ""
					if p.DirF != "" && x[p.DirF] != nil {
						switch v := fmt.Sprint(x[p.DirF]); v {
						case p.InV:
							dir = "IN"
						case p.OutV:
							dir = "OUT"
						default:
							dir = "Not recognised (" + v + ")"
						}
					}
					sample = map[string]string{"employee": emp, "time": t, "direction": dir}
				}
			}
			out["records"], out["mapped"], out["ok"] = len(recs), mapped, true
			if sample != nil {
				out["sample"] = sample
			}
			switch {
			case len(recs) == 0:
				note = "Connected. No punches in the last hour"
			case mapped == 0:
				note = fmt.Sprintf("Connected. %d records came back, but none has the fields %s and %s. Check the field mapping", len(recs), p.EmpF, p.TimeF)
				out["ok"] = false
			default:
				note = fmt.Sprintf("Connected. %d punch%s in the last hour, %d with the mapped fields", len(recs), map[bool]string{true: "", false: "es"}[len(recs) == 1], mapped)
			}
		}
	}
	out["note"] = note
	if p.URL == saved.URL {
		_, _ = s.st.DB.C("attendance_pull_config").Update(ctx, bson.D{{Key: "_id", Value: 1}}, bson.M{"$set": bson.M{"tested_at": time.Now(), "test_ok": out["ok"], "test_note": note}})
	}
	_ = audit(ctx, s.st.DB, c, "Punch machine API connection tested: "+note, "attendance_pull_config", "1", nil, nil)
	return out, nil
}

// pullRecords finds the punch records in a JSON answer: a list, or the first list inside an object.
func pullRecords(data []byte) ([]map[string]any, bool) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, false
	}
	asRecs := func(l []any) []map[string]any {
		out := []map[string]any{}
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	switch x := v.(type) {
	case []any:
		return asRecs(x), true
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if l, ok := x[k].([]any); ok {
				return asRecs(l), true
			}
		}
		return []map[string]any{}, true
	}
	return nil, false
}
