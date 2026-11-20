package api

// Master update (Masters › Master update): one Excel workbook with a sheet per master the caller can change.
// Download it with the current data (or blank), fill it offline, upload it once: every row is checked with the
// same rules as the Masters screen (saveMaster), the result is shown sheet by sheet, then applied. Sheets are read
// in dependency order, so a region added on Regions can be used by a new state on States in the same file. A
// missing sheet or row never retires or changes anything.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/mdm"
	"github.com/saltstayz/hrms/internal/store"
)

type mbCol struct {
	Key   string // name, parent, flow, grades, code, start, end, break, locs, used_for, status, since, reason, id
	Head  string
	Width float64
	List  string // a drop-down from the hidden Lists sheet
	Warn  bool   // a value not in the drop-down warns instead of stopping (a parent added in the same file)
	Read  bool   // shown on download only: never read on upload (a location's CMS property)
}

type mbSheet struct {
	Key, Sheet, Right string
	Cols              []mbCol // the master's own columns, between Name and Status
}

// mbSheets in dependency order (the order they are read).
var mbSheets = []mbSheet{
	{Key: "region", Sheet: "Regions", Right: "mas.org"},
	{Key: "state", Sheet: "States", Right: "mas.org", Cols: []mbCol{{Key: "parent", Head: "Region", Width: 22, List: "region", Warn: true}}},
	{Key: "city", Sheet: "Cities", Right: "mas.org", Cols: []mbCol{{Key: "parent", Head: "State", Width: 24, List: "state", Warn: true}}},
	{Key: "cluster", Sheet: "Clusters", Right: "mas.org", Cols: []mbCol{{Key: "parent", Head: "City", Width: 28, List: "city", Warn: true}}},
	{Key: "location", Sheet: "Locations", Right: "mas.org", Cols: []mbCol{{Key: "parent", Head: "Cluster", Width: 30, List: "cluster", Warn: true},
		{Key: "cms", Head: "CMS property (read-only)", Width: 22, Read: true}}},
	{Key: "department", Sheet: "Departments", Right: "mas.job", Cols: []mbCol{{Key: "flow", Head: "Approval flow", Width: 16, List: "flow"}}},
	{Key: "grade", Sheet: "Grades", Right: "mas.job"},
	{Key: "designation", Sheet: "Designations", Right: "mas.job", Cols: []mbCol{{Key: "parent", Head: "Department", Width: 22, List: "department", Warn: true},
		{Key: "grades", Head: "Grades (comma separated)", Width: 26}}},
	{Key: "shift", Sheet: "Shifts", Right: "mas.job", Cols: []mbCol{{Key: "code", Head: "Code", Width: 10}, {Key: "start", Head: "Start (HH:MM)", Width: 14},
		{Key: "end", Head: "End (HH:MM)", Width: 14}, {Key: "break", Head: "Break minutes", Width: 14},
		{Key: "locs", Head: "Locations (All locations, or names separated by ;)", Width: 48}}},
	{Key: "qualification", Sheet: "Qualifications", Right: "mas.job"},
	{Key: "employment_type", Sheet: "Employment types", Right: "mas.job"},
	{Key: "attendance_mode", Sheet: "Attendance modes", Right: "mas.job"},
	{Key: "change_reason", Sheet: "Change reasons", Right: "mas.job", Cols: []mbCol{{Key: "used_for", Head: "Used for", Width: 18, List: "used_for"}}},
}

const mbIDHead = "Row ID (leave as is; blank for a new item)"

func (sh mbSheet) columns() []mbCol {
	out := []mbCol{{Key: "name", Head: "Name", Width: 30}}
	out = append(out, sh.Cols...)
	return append(out, mbCol{Key: "status", Head: "Status", Width: 11, List: "status"}, mbCol{Key: "since", Head: "Inactive from", Width: 14},
		mbCol{Key: "reason", Head: "Inactive reason", Width: 26, List: "reason", Warn: true}, mbCol{Key: "id", Head: mbIDHead, Width: 40})
}

// headKey reads a header cell: "Start (HH:MM)" is start, "Row ID (leave …)" is id.
func (sh mbSheet) headKey(h string) string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.Index(s, " ("); i > 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	for _, c := range sh.columns() {
		if norm(c.Head) == norm(h) {
			return c.Key
		}
	}
	return ""
}

// readOnly says whether the column key is shown on download only (ignored on upload).
func (sh mbSheet) readOnly(k string) bool {
	for _, c := range sh.Cols {
		if c.Key == k {
			return c.Read
		}
	}
	return false
}

func rightName(r string) string {
	return map[string]string{"mas.org": "Masters: organisation", "mas.job": "Masters: job"}[r]
}

func canEditMasters(c *Ctx) bool { return c.Level("mas.org") >= 2 || c.Level("mas.job") >= 2 }

// ---------- Names: an item's name, or "Name (Parent)" where its name repeats ----------

type mbItem struct{ ID, Name, Parent, Status string }

// mbItemMasters are the masters a parent column (or a shift's Locations) names.
var mbItemMasters = []string{"region", "state", "city", "cluster", "location", "department"}

// mbItems lists the items of master t that match filter, in master order, with the name of each one's parent (""
// for a region or a department). Like the join it stands for, an item whose parent is missing is left out.
func mbItems(ctx context.Context, q store.Q, t string, filter bson.M) ([]mbItem, error) {
	p, hasParent := parentOf[t]
	fields := bson.M{"name": 1, "status": 1}
	if hasParent {
		fields[p.field] = 1
	}
	rows, err := q.C(masters[t].table).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(fields))
	if err != nil {
		return nil, err
	}
	parents := map[string]string{}
	if hasParent && len(rows) > 0 {
		ids := make([]any, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r[p.field])
		}
		l, err := q.C(masters[p.master].table).Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(bson.M{"name": 1}))
		if err != nil {
			return nil, err
		}
		for _, x := range l {
			parents[str(x["id"])] = str(x["name"])
		}
	}
	out := make([]mbItem, 0, len(rows))
	for _, r := range rows {
		x := mbItem{ID: str(r["id"]), Name: str(r["name"]), Status: str(r["status"])}
		if hasParent {
			pn, ok := parents[str(r[p.field])]
			if !ok {
				continue
			}
			x.Parent = pn
		}
		out = append(out, x)
	}
	return out, nil
}

// mbReasons are the active change reasons for Master data, in master order: what an item can be retired with.
func mbReasons(ctx context.Context, q store.Q) ([]string, error) {
	rows, err := q.C("simple_master").Find(ctx, bson.M{"kind": "change_reason", "used_for": "Master data", "status": "Active"},
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, str(r["name"]))
	}
	return out, nil
}

// mbDisplay names every item as it is written in a parent column: the name, or "Name (Parent)" where it repeats.
func mbDisplay(items []mbItem) map[string]string {
	n := map[string]int{}
	for _, x := range items {
		n[strings.ToLower(x.Name)]++
	}
	out := map[string]string{}
	for _, x := range items {
		out[x.ID] = x.Name
		if n[strings.ToLower(x.Name)] > 1 && x.Parent != "" {
			out[x.ID] = x.Name + " (" + x.Parent + ")"
		}
	}
	return out
}

// ---------- Download ----------

func (s *Server) masterBookDownload(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !canEditMasters(c) {
		writeErr(w, forbidden(""))
		return
	}
	if !c.CanDownload {
		writeErr(w, forbidden("Your role cannot download files"))
		return
	}
	ctx, data := r.Context(), r.URL.Query().Get("data") == "1"
	f, sheets, n, err := s.masterBook(ctx, c, data)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		writeErr(w, err)
		return
	}
	name := "master-update-format"
	if data {
		name = "master-update"
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.xlsx"`, name, time.Now().Format("2006-01-02")))
	_, _ = w.Write(buf.Bytes())
	what := map[bool]string{true: "with current data", false: "blank format"}[data]
	_ = audit(ctx, s.st.DB, c, "Downloaded the Master update workbook ("+what+")", "master_update", "", nil, map[string]any{"sheets": sheets, "rows": n})
}

// masterBook builds the workbook: "How to fill", one sheet per master the caller can change, a hidden "Lists".
func (s *Server) masterBook(ctx context.Context, c *Ctx, data bool) (*excelize.File, []string, int, error) {
	q := s.st.DB
	items, disp := map[string][]mbItem{}, map[string]map[string]string{}
	for _, t := range mbItemMasters {
		l, err := mbItems(ctx, q, t, bson.M{})
		if err != nil {
			return nil, nil, 0, err
		}
		items[t], disp[t] = l, mbDisplay(l)
	}
	lists := map[string][]string{"status": {"Active", "Inactive"}, "flow": {"Operations", "Corporate"}, "used_for": reasonUses}
	for t, l := range items {
		for _, x := range l {
			if x.Status == "Active" {
				lists[t] = append(lists[t], disp[t][x.ID])
			}
		}
	}
	reasons, err := mbReasons(ctx, q)
	if err != nil {
		return nil, nil, 0, err
	}
	lists["reason"] = reasons

	f := excelize.NewFile()
	const how = "How to fill"
	_ = f.SetSheetName("Sheet1", how)
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center"}})
	idHead, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "4B5563"}, Fill: excelize.Fill{Type: "pattern", Color: []string{"D1D5DB"}, Pattern: 1}})
	idCell, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "6B7280"}, Fill: excelize.Fill{Type: "pattern", Color: []string{"F3F4F6"}, Pattern: 1}, NumFmt: 49})
	text, _ := f.NewStyle(&excelize.Style{NumFmt: 49}) // "@": typed dates, times and codes stay as typed

	// The drop-down lists go on a hidden "Lists" sheet, last; each list gets a column the first time it is used.
	listAt, listOrder := map[string]string{}, []string{}
	listRange := func(key string) string {
		if r, ok := listAt[key]; ok {
			return r
		}
		vals := lists[key]
		if len(vals) == 0 {
			return ""
		}
		listOrder = append(listOrder, key)
		lc, _ := excelize.ColumnNumberToName(len(listOrder))
		listAt[key] = fmt.Sprintf("Lists!$%s$2:$%s$%d", lc, lc, len(vals)+1)
		return listAt[key]
	}

	sheets, total, first := []string{}, 0, -1
	for _, sh := range mbSheets {
		if c.Level(sh.Right) < 2 {
			continue
		}
		idx, err := f.NewSheet(sh.Sheet)
		if err != nil {
			return nil, nil, 0, err
		}
		if first < 0 {
			first = idx
		}
		sheets = append(sheets, sh.Sheet)
		cols := sh.columns()
		for i, col := range cols {
			cn, _ := excelize.ColumnNumberToName(i + 1)
			_ = f.SetColWidth(sh.Sheet, cn, cn, col.Width)
			switch {
			case col.Key == "id" || col.Read:
				_ = f.SetColStyle(sh.Sheet, cn, idCell)
			case col.Key != "break":
				_ = f.SetColStyle(sh.Sheet, cn, text)
			}
			_ = f.SetCellValue(sh.Sheet, cn+"1", col.Head)
			st := head
			if col.Key == "id" || col.Read {
				st = idHead
			}
			_ = f.SetCellStyle(sh.Sheet, cn+"1", cn+"1", st)
			if col.List == "" {
				continue
			}
			rng := listRange(col.List)
			if rng == "" {
				continue
			}
			dv := excelize.NewDataValidation(true)
			dv.Sqref = fmt.Sprintf("%s2:%s5001", cn, cn)
			dv.SetSqrefDropList(rng)
			if col.Warn {
				dv.SetError(excelize.DataValidationErrorStyleWarning, "Not in the list", "Not in the list of active items. Keep it only if it is added in this file (on an earlier sheet), or it is a reason added on Change reasons.")
			} else {
				dv.SetError(excelize.DataValidationErrorStyleStop, "Not in the list", "Choose a value from the list.")
			}
			_ = f.AddDataValidation(sh.Sheet, dv)
		}
		_ = f.SetPanes(sh.Sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
		if !data {
			continue
		}
		rows, err := s.masterBookRows(ctx, sh, disp)
		if err != nil {
			return nil, nil, 0, err
		}
		for i, row := range rows {
			_ = f.SetSheetRow(sh.Sheet, fmt.Sprintf("A%d", i+2), &row)
		}
		total += len(rows)
	}
	_, _ = f.NewSheet("Lists")
	for i, key := range listOrder {
		lc, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellValue("Lists", lc+"1", key)
		for j, v := range lists[key] {
			_ = f.SetCellValue("Lists", fmt.Sprintf("%s%d", lc, j+2), v)
		}
	}
	_ = f.SetSheetVisible("Lists", false)
	howTo(f, how, sheets, len(sheets) < len(mbSheets))
	if first >= 0 {
		f.SetActiveSheet(first)
	}
	return f, sheets, total, nil
}

// masterBookRows are a sheet's current items, active and inactive, in master order.
func (s *Server) masterBookRows(ctx context.Context, sh mbSheet, disp map[string]map[string]string) ([][]any, error) {
	m := masters[sh.Key]
	filter := bson.M{}
	if m.kind != "" {
		filter["kind"] = m.kind
	}
	items, err := s.st.DB.C(m.table).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
	if err != nil {
		return nil, err
	}
	locName := map[string]string{}
	if sh.Key == "shift" {
		l, err := s.st.DB.C("location").Find(ctx, nil, options.Find().SetProjection(bson.M{"name": 1}))
		if err != nil {
			return nil, err
		}
		for _, x := range l {
			locName[str(x["id"])] = str(x["name"])
		}
	}
	out := [][]any{}
	for _, x := range items {
		row := []any{}
		for _, col := range sh.columns() {
			var v any = ""
			switch col.Key {
			case "name", "code", "used_for", "status":
				v = str(x[col.Key])
			case "parent":
				p := parentOf[sh.Key]
				v = disp[p.master][str(x[p.field])]
			case "flow":
				v = map[bool]string{true: "Corporate", false: "Operations"}[x["corporate"] == true]
			case "grades":
				v = strings.Join(strs(x["grades"]), ", ")
			case "start":
				v = str(x["start_time"])
			case "end":
				v = str(x["end_time"])
			case "break":
				v, _ = num64(x["break_minutes"])
			case "locs":
				if x["location_ids"] == nil {
					v = "All locations"
				} else {
					names := []string{}
					for _, id := range strs(x["location_ids"]) {
						names = append(names, locName[id])
					}
					v = strings.Join(names, "; ")
				}
			case "since":
				v = str(x["inactive_since"])
			case "reason":
				v = str(x["inactive_reason"])
			case "id":
				v = str(x["id"])
			case "cms":
				v = str(x["property_id"])
			}
			row = append(row, v)
		}
		out = append(out, row)
	}
	return out, nil
}

func howTo(f *excelize.File, sheet string, sheets []string, partial bool) {
	title, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	wrap, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	lines := [][2]string{
		{"t", "Master update"},
		{"", "One workbook for every master you can change. Fill it in, then upload it in Masters › Master update. Nothing changes until the file is checked and you apply it."},
		{"", ""},
		{"b", "How it works"},
		{"", "• Each sheet is one master: " + strings.Join(sheets, ", ") + ". Sheets are read in this order, so a region added on Regions can be used by a new state on States in the same file."},
		{"", "• A sheet you leave out, an empty sheet or a row you delete changes nothing. Nothing is ever deleted: set Status to Inactive to retire an item. Blank rows are skipped."},
		{"", "• Row ID: leave it as it is. A row with a Row ID changes that item, its name too. A new item has no Row ID. A row without a Row ID whose name already exists (in the same state, city or department, where names repeat) changes that item."},
		{"", "• A blank cell keeps the current value of an existing item. A new item needs its name and its parent (a state needs its region, and so on)."},
		{"", "• Parents are given by name. Where a name repeats (a city in two states, a cluster in two cities, a designation in two departments), write “Name (Parent)”, for example “Aurangabad (Bihar)”. The drop-downs list active items; you can also type one that is added on an earlier sheet of this file."},
		{"", "• Status is Active or Inactive. To retire an item set Inactive, an Inactive reason from the Master data reasons (Masters › Change reasons) and an Inactive from date, today or later (blank means today). A future date retires it on that day. Setting an inactive item back to Active reactivates it."},
		{"", "• A rename keeps everything that uses the name working: employee records, work policy mappings, pay bands, the grades of designations, role mappings, offers and approvals waiting for a decision are updated with it."},
		{"", "• These cannot change: an employment type's name and a shift's code. The change reasons the app writes itself (Transfer, Excess payment recovery, Absent without notice, the separation reasons) and the attendance mode Biometric can be renamed, but what they are used for cannot change."},
		{"", "• A designation cannot move to another department while active employees hold it: move them first with Bulk update, or add a new designation. Moving a location, cluster, city or state is allowed; its employees move with it."},
		{"", "• A location linked to a CMS property takes its name and status from the CMS: rename or reactivate it in the CMS. Link a location in Masters › Locations. With “Properties come from the CMS” on, a new location is added there by picking its CMS property, not in this workbook."},
		{"", "• A shift's new timing applies from today as a new version (earlier days keep the old timing). Its attendance rules are set in Masters › Shifts › Set shift."},
		{"", "• Not in this workbook: city and location budgets (Bulk update › Budgets), shift attendance rules (Set shift) and company details."},
		{"", "• Upload this Excel file (.xlsx). A CSV file holds one sheet only, so it cannot carry every master. Up to 5,000 rows and 10 MB."},
		{"", ""},
		{"b", "Columns"},
		{"", "Every sheet: Name, the master's own columns, then Status, Inactive from, Inactive reason and Row ID (grey, last)."},
		{"", "Regions: Name. States: Region. Cities: State. Clusters: City. Locations: Cluster, and CMS property (read-only, ignored on upload)."},
		{"", "Departments: Approval flow, Operations or Corporate. Grades: Name."},
		{"", "Designations: Department, and Grades separated by commas (at least one; each an active grade)."},
		{"", "Shifts: Code (cannot change), Start and End as 24-hour HH:MM, Break minutes, and Locations: “All locations” or location names separated by “;”."},
		{"", "Qualifications, Employment types, Attendance modes, Roles, Skills: Name. Change reasons: Used for, one of " + strings.Join(reasonUses, ", ") + "."},
	}
	if partial {
		lines = append(lines, [2]string{"", ""}, [2]string{"", "Only the masters you can change are in this workbook."})
	}
	_ = f.SetColWidth(sheet, "A", "A", 130)
	for i, l := range lines {
		cell := fmt.Sprintf("A%d", i+1)
		_ = f.SetCellValue(sheet, cell, l[1])
		st := wrap
		switch l[0] {
		case "t":
			st = title
		case "b":
			st = bold
		}
		_ = f.SetCellStyle(sheet, cell, cell, st)
	}
}

// ---------- Upload: check, then apply ----------

type mbRowRes struct {
	Line    int          `json:"line"`
	Name    string       `json:"name"`
	Kind    string       `json:"kind"` // new | changed | same | error
	Errors  []string     `json:"errors"`
	Changes []bulkChange `json:"changes"`
}

type mbSheetRes struct {
	Key     string     `json:"key"`
	Sheet   string     `json:"sheet"`
	New     int        `json:"new"`
	Changed int        `json:"changed"`
	Same    int        `json:"same"`
	Errors  int        `json:"errors"`
	Note    string     `json:"note,omitempty"`
	Refused bool       `json:"refused,omitempty"`
	Rows    []mbRowRes `json:"rows"` // rows with changes or errors; rows with no change are only counted
}

// readWorkbook reads the uploaded Excel file (10 MB at most). A CSV holds one sheet, so it is refused.
func readWorkbook(r *http.Request) (*excelize.File, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 11<<20)
	if err := r.ParseMultipartForm(11 << 20); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			return nil, bad("The file must be 10 MB or smaller")
		}
		return nil, bad("Choose the Excel workbook")
	}
	f, h, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose the Excel workbook")
	}
	defer f.Close()
	if h.Size > 10<<20 {
		return nil, bad("The file must be 10 MB or smaller")
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte("PK")) {
		return nil, bad("Upload the Excel workbook (.xlsx) from Download. A CSV file holds one sheet only, so it cannot carry every master")
	}
	x, err := excelize.OpenReader(bytes.NewReader(b), excelize.Options{RawCellValue: true})
	if err != nil {
		return nil, bad("That Excel file could not be read")
	}
	return x, nil
}

type mbIn struct {
	sh   *mbSheet
	rows [][]string
}

func (s *Server) masterBookRun(r *http.Request, c *Ctx) (any, error) {
	if !canEditMasters(c) {
		return nil, forbidden("")
	}
	apply := r.PathValue("action") == "apply"
	if !apply && r.PathValue("action") != "check" {
		return nil, notFound("")
	}
	x, err := readWorkbook(r)
	if err != nil {
		return nil, err
	}
	defer x.Close()
	byName := map[string]string{}
	for _, n := range x.GetSheetList() {
		byName[strings.ToLower(strings.TrimSpace(n))] = n
	}
	ins, missing, total := []mbIn{}, []string{}, 0
	for i := range mbSheets {
		sh := &mbSheets[i]
		n, ok := byName[strings.ToLower(sh.Sheet)]
		if !ok {
			missing = append(missing, sh.Sheet)
			continue
		}
		rows, err := x.GetRows(n, excelize.Options{RawCellValue: true})
		if err != nil {
			return nil, bad("The sheet %s could not be read", n)
		}
		ins = append(ins, mbIn{sh, rows})
		if c.Level(sh.Right) >= 2 && len(rows) > 1 {
			for _, rec := range rows[1:] {
				if strings.TrimSpace(strings.Join(rec, "")) != "" {
					total++
				}
			}
		}
	}
	if len(ins) == 0 {
		return nil, bad("No master sheets found. Download the format again: each sheet is named after its master, like Regions or Cities")
	}
	if total > 5000 {
		return nil, bad("Upload at most 5,000 rows at a time (this file has %d)", total)
	}
	// Every row is read and written in one transaction, so later rows see what earlier ones did; a check rolls it
	// all back at the end. A row that a database rule stops after it started writing cannot be undone on its own
	// (MongoDB has no savepoints): it is noted in undo and the upload runs again from the start, that row stopping
	// before it writes (see masterBookRow).
	ctx := r.Context()
	undo := map[string]string{}
	var out map[string]any
	for {
		err = s.st.Tx(ctx, func(tx store.Q) error {
			var err error
			if out, err = s.masterBookSheets(ctx, tx, c, ins, apply, undo); err != nil {
				return err
			}
			if !apply {
				return errMbCheckOnly
			}
			return nil
		})
		if !errors.Is(err, errMbAgain) {
			break
		}
	}
	if err != nil && !errors.Is(err, errMbCheckOnly) {
		return nil, err
	}
	out["missing"] = missing
	return out, nil
}

var (
	errMbCheckOnly = errors.New("master update: checked only, nothing is kept")
	errMbAgain     = errors.New("master update: a row failed after it started writing; the upload runs again without it")
)

// masterBookSheets reads the sheets' rows in order inside tx, checking and writing each one; with apply the summary is
// audited. It returns the result the screen shows (without the missing sheets).
func (s *Server) masterBookSheets(ctx context.Context, tx store.Q, c *Ctx, ins []mbIn, apply bool, undo map[string]string) (map[string]any, error) {
	st := &mbState{alias: map[string]map[string]string{}, undo: undo}
	sheets, changes, errs := []mbSheetRes{}, 0, 0
	for _, in := range ins {
		res := mbSheetRes{Key: in.sh.Key, Sheet: in.sh.Sheet, Rows: []mbRowRes{}}
		if c.Level(in.sh.Right) < 2 {
			res.Refused, res.Note = true, "Not read: changing these masters needs the "+rightName(in.sh.Right)+" right. Nothing on this sheet changes."
			sheets = append(sheets, res)
			continue
		}
		if len(in.rows) == 0 {
			res.Note = "Empty: nothing changes."
			sheets = append(sheets, res)
			continue
		}
		cols := map[string]int{}
		for j, h := range in.rows[0] {
			if k := in.sh.headKey(h); k != "" && !in.sh.readOnly(k) {
				if _, dup := cols[k]; !dup {
					cols[k] = j
				}
			}
		}
		if _, ok := cols["name"]; !ok {
			res.Note, res.Errors = "The Name column is missing, so the sheet was not read. Download the format again.", 1
			errs++
			sheets = append(sheets, res)
			continue
		}
		seen := map[string]int{}
		for i, rec := range in.rows[1:] {
			v, blank := map[string]string{}, true
			for k, j := range cols {
				if j < len(rec) {
					v[k] = strings.TrimSpace(rec[j])
					blank = blank && v[k] == ""
				}
			}
			if blank {
				continue
			}
			row, err := s.masterBookRow(ctx, tx, c, in.sh, i+2, v, st, seen)
			if err != nil {
				return nil, err
			}
			switch row.Kind {
			case "new":
				res.New++
			case "changed":
				res.Changed++
			case "same":
				res.Same++
			default:
				res.Errors++
			}
			if row.Kind != "same" {
				res.Rows = append(res.Rows, row)
			}
		}
		changes += res.New + res.Changed
		errs += res.Errors
		sheets = append(sheets, res)
	}
	if apply {
		counts := map[string]any{}
		for _, x := range sheets {
			if x.New+x.Changed > 0 {
				counts[x.Sheet] = map[string]int{"new": x.New, "changed": x.Changed}
			}
		}
		if err := audit(ctx, tx, c, fmt.Sprintf("Master update applied: %d change%s", changes, plural(changes)), "master_update", "", nil, counts); err != nil {
			return nil, err
		}
	}
	return map[string]any{"applied": apply, "changes": changes, "errors": errs, "sheets": sheets}, nil
}

// mbState carries what earlier rows of the file did: items renamed in it are still found by their old name, so a
// downloaded workbook whose region is renamed on Regions keeps its states pointing at it.
type mbState struct {
	alias map[string]map[string]string // master → lower(old name or "old (parent)") → id
	// undo holds the rows (mbUndoKey) that a database rule stopped after they had started writing, with their error,
	// from earlier runs of this upload: in this run they stop before writing anything.
	undo map[string]string
}

func mbUndoKey(sh *mbSheet, line int) string { return sh.Key + "|" + strconv.Itoa(line) }

// masterBookRow checks and writes one row. Rule errors are the row's errors, and a row with errors leaves no trace:
// its rules are all checked before it writes anything. A database rule (a duplicate, a value the database refuses)
// can still stop a row half-way, and MongoDB has no savepoint to undo just that row: the row is then noted in
// st.undo and errMbAgain makes the upload run again, with that row stopping before it writes.
func (s *Server) masterBookRow(ctx context.Context, tx store.Q, c *Ctx, sh *mbSheet, line int, v map[string]string, st *mbState, seen map[string]int) (mbRowRes, error) {
	res, alias, err := s.mbRow(ctx, tx, c, sh, line, v, st, seen)
	res.Line, res.Name = line, or(res.Name, v["name"])
	if res.Errors == nil {
		res.Errors = []string{}
	}
	if res.Changes == nil {
		res.Changes = []bulkChange{}
	}
	if err != nil {
		var ae *apiError
		code, _ := store.ErrCode(err)
		var msg string
		switch {
		case errors.As(err, &ae):
			msg = ae.Message
		case code == "23505":
			msg = "That already exists"
		case strings.HasPrefix(code, "23") || strings.HasPrefix(code, "22"):
			msg = "A value is not valid"
		default:
			return res, err
		}
		if ae == nil { // a database rule: part of the row may be written
			key := mbUndoKey(sh, line)
			if _, again := st.undo[key]; again {
				return res, err
			}
			st.undo[key] = msg
			return res, errMbAgain
		}
		res.Errors = append(res.Errors, msg)
		res.Kind, res.Changes = "error", []bulkChange{}
		return res, nil
	}
	if len(res.Errors) > 0 {
		res.Kind, res.Changes = "error", []bulkChange{}
		return res, nil
	}
	for t, l := range alias {
		if st.alias[t] == nil {
			st.alias[t] = map[string]string{}
		}
		for k, id := range l {
			st.alias[t][k] = id
		}
	}
	return res, nil
}

// mbRow reads one row into the Masters screen's body, finds the item it is about and applies it.
func (s *Server) mbRow(ctx context.Context, tx store.Q, c *Ctx, sh *mbSheet, line int, v map[string]string, st *mbState, seen map[string]int) (mbRowRes, map[string]map[string]string, error) {
	t, m := sh.Key, masters[sh.Key]
	res := mbRowRes{Name: v["name"]}
	fail := func(msg string, a ...any) (mbRowRes, map[string]map[string]string, error) {
		res.Errors = append(res.Errors, fmt.Sprintf(msg, a...))
		return res, nil, nil
	}
	name := v["name"]
	if name == "" {
		return fail("Name is required")
	}
	p, hasParent := parentOf[t]
	pm := masters[p.master]
	// The item: by Row ID; else by its name (under the same parent where names repeat); else a new one.
	var cur store.Row
	var err error
	if id := strings.ToLower(v["id"]); id != "" {
		if cur, err = masterRow(ctx, tx, m, id); err != nil {
			return res, nil, err
		}
		if cur == nil {
			return fail("Row ID %s is not a %s here. Leave it blank to add a new item", v["id"], m.label)
		}
	}
	var parentID string
	if hasParent && v["parent"] != "" {
		id, msg, err := st.resolve(ctx, tx, p.master, v["parent"])
		if err != nil {
			return res, nil, err
		}
		if msg != "" {
			return fail("%s", msg)
		}
		parentID = id
	}
	code := strings.ToUpper(v["code"])
	usedFor := v["used_for"]
	for _, u := range reasonUses {
		if strings.EqualFold(u, usedFor) {
			usedFor = u
		}
	}
	if cur == nil {
		var msg string
		if cur, msg, err = mbFind(ctx, tx, t, name, parentID, usedFor, code); err != nil {
			return res, nil, err
		}
		if msg != "" {
			return fail("%s", msg)
		}
	}
	if cur == nil && hasParent && parentID == "" {
		return fail("%s is required for a new %s", capital(pm.label), m.label)
	}
	if cur == nil && t == "shift" && code == "" {
		return fail("Code is required for a new shift")
	}
	key := "id:" + str(cur["id"])
	if cur == nil {
		key = strings.ToLower("new:" + name + "|" + parentID + "|" + usedFor + "|" + code)
	}
	if first, ok := seen[key]; ok {
		return fail("Appears again, first on line %d. Keep one row for each item", first)
	}
	seen[key] = line

	// The row as the Masters screen's body; master.validate and saveMaster apply the same rules.
	body := map[string]any{"name": name}
	if parentID != "" {
		body[p.field] = parentID
	}
	switch t {
	case "department":
		switch f := strings.TrimSuffix(strings.ToLower(v["flow"]), " flow"); f {
		case "":
		case "corporate", "operations":
			body["corporate"] = f == "corporate"
		default:
			return fail("Approval flow must be Operations or Corporate")
		}
	case "designation":
		if g := v["grades"]; g != "" {
			l := []any{}
			for _, x := range strings.FieldsFunc(g, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
				if x = strings.TrimSpace(x); x == "" {
					continue
				}
				// A grade renamed on the Grades sheet of this file is still found by its old name.
				if id, ok := st.alias["grade"][strings.ToLower(x)]; ok {
					if there, err := tx.C("grade").Exists(ctx, bson.M{"name": masterRulesNoCase(x)}); err != nil {
						return res, nil, err
					} else if !there {
						if gr, _ := tx.C("grade").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1})); gr != nil {
							x = str(gr["name"])
						}
					}
				}
				l = append(l, x)
			}
			body["grades"] = l
		}
	case "shift":
		if code != "" {
			body["code"] = code
		}
		for k, f := range map[string]string{"start": "start_time", "end": "end_time"} {
			if v[k] == "" {
				continue
			}
			hm, ok := clockTime(v[k])
			if !ok {
				return fail("%s must be a time as HH:MM, for example 09:30", map[string]string{"start": "Start", "end": "End"}[k])
			}
			body[f] = hm
		}
		if b := v["break"]; b != "" {
			n, err := strconv.ParseFloat(b, 64)
			if err != nil {
				return fail("Break minutes must be a whole number, 0 or more")
			}
			body["break_minutes"] = n
		}
		if l := v["locs"]; l != "" {
			if strings.EqualFold(l, "All locations") || strings.EqualFold(l, "All") {
				body["location_ids"] = nil
			} else {
				ids := []any{}
				for _, x := range strings.Split(l, ";") {
					if x = strings.TrimSpace(x); x == "" {
						continue
					}
					id, msg, err := st.resolve(ctx, tx, "location", x)
					if err != nil {
						return res, nil, err
					}
					if msg != "" {
						return fail("%s", msg)
					}
					ids = append(ids, id)
				}
				body["location_ids"] = ids
			}
		}
	case "change_reason":
		if usedFor != "" {
			body["used_for"] = usedFor
		}
	}
	vals, err := m.validate(body, cur == nil)
	if err != nil {
		return res, nil, err
	}

	// Status: blank keeps it; Inactive retires the item from a date (blank = today) with a Master data reason;
	// Active reactivates an inactive item, or cancels a retirement waiting for its date.
	curStatus, curSince, curReason := "Active", "", ""
	if cur != nil {
		curStatus, curSince, curReason = str(cur["status"]), str(cur["inactive_since"]), str(cur["inactive_reason"])
	}
	status := ""
	switch strings.ToLower(v["status"]) {
	case "":
	case "active":
		status = "Active"
	case "inactive":
		status = "Inactive"
	default:
		return fail("Status must be Active or Inactive")
	}
	since, reason := "", v["reason"]
	if v["since"] != "" {
		if since = excelDate(v["since"]); !isDate(since) {
			return fail("Inactive from must be a date like 2026-12-01")
		}
	}
	var retire, reactivate bool
	otherReason := reason != "" && !strings.EqualFold(reason, curReason)
	switch {
	case status == "Inactive" && cur == nil:
		return fail("A new item starts Active. Leave Status blank, or add it and retire it later")
	case status == "Inactive" && curStatus == "Inactive":
		if (since != "" && since != curSince) || otherReason {
			return fail("Already inactive since %s. To change the date or the reason, make it Active first", longDate(curSince))
		}
	case status == "Inactive":
		if since == "" {
			since = today()
		}
		retire = since != curSince || otherReason
	case status == "Active" && curStatus == "Inactive":
		reactivate = true
	case status == "" && curStatus == "Inactive":
		if (since != "" && since != curSince) || otherReason {
			return fail("Already inactive since %s. To change the date or the reason, make it Active first", longDate(curSince))
		}
	case status == "Active" && since == "" && reason == "":
		reactivate = curSince != "" // a retirement waiting for its date is cancelled
	case status == "Active" || (status == "" && (since != "" || reason != "")):
		if since == curSince && !otherReason {
			break // as it is
		}
		if since == "" {
			return fail("Give the Inactive from date, or clear the Inactive reason")
		}
		if since <= today() {
			return fail("Status is Active but Inactive from is %s. Set Status to Inactive to retire it from that day, or give a future date", longDate(since))
		}
		retire = true
	}
	if retire {
		if since < today() {
			return fail("Inactive from must be today or a future date")
		}
		canon, err := tx.C("simple_master").FindOne(ctx, bson.M{"kind": "change_reason", "used_for": "Master data", "status": "Active", "name": masterRulesNoCase(reason)},
			options.FindOne().SetSort(bson.D{{Key: "seq", Value: 1}}).SetProjection(bson.M{"name": 1}))
		if err != nil {
			return res, nil, err
		}
		if canon == nil {
			l, _ := mbReasons(ctx, tx)
			if reason == "" {
				return fail("Give the Inactive reason, one of: %s", strings.Join(l, ", "))
			}
			return fail("Inactive reason “%s” is not a Master data reason. Choose one of: %s", reason, strings.Join(l, ", "))
		}
		reason = str(canon["name"])
		if len(strings.TrimSpace(reason)) < 3 { // retireMaster's own rule, checked before anything is written
			return fail("Give a reason")
		}
	}
	// A location whose CMS property is inactive is reactivated in the CMS: refused before the row writes anything.
	if reactivate && t == "location" && cur != nil {
		if err := mdm.CheckReactivate(ctx, tx, cur); err != nil {
			return res, nil, mdmErr(err)
		}
	}
	// A row a database rule stopped half-way in an earlier run of this upload stops here, before it writes.
	if msg, ok := st.undo[mbUndoKey(sh, line)]; ok {
		return fail("%s", msg)
	}

	// Write: add or change, then retire or reactivate.
	id := str(cur["id"])
	var saved masterSaved
	if cur == nil || len(vals) > 0 {
		if saved, err = saveMaster(ctx, tx, c, t, id, vals); err != nil {
			return res, nil, err
		}
		id = str(saved.After["id"])
	}
	if cur == nil {
		res.Changes = append(res.Changes, bulkChange{Field: "Added"})
		seen["id:"+id] = line // a later row with the same name is about this new item
	} else {
		ch, err := mbDescribe(ctx, tx, t, m, saved)
		if err != nil {
			return res, nil, err
		}
		res.Changes = append(res.Changes, ch...)
	}
	if reactivate {
		if _, err := reactivateMaster(ctx, tx, c, t, id); err != nil {
			return res, nil, err
		}
		if curStatus == "Inactive" {
			res.Changes = append(res.Changes, bulkChange{Field: "Reactivated"})
		} else {
			res.Changes = append(res.Changes, bulkChange{Field: "Retirement on " + longDate(curSince) + " cancelled"})
		}
	}
	if retire {
		_, n, err := retireMaster(ctx, tx, c, t, id, since, reason)
		if err != nil {
			return res, nil, err
		}
		what := "Marked inactive from " + longDate(since)
		if since > today() {
			what = "Inactive from " + longDate(since) + " (scheduled)"
		}
		note := reason
		if n > 0 {
			note += fmt.Sprintf(" · %d active employee%s still use%s it", n, plural(int(n)), map[bool]string{true: "s", false: ""}[n == 1])
		}
		res.Changes = append(res.Changes, bulkChange{Field: what, Note: note})
	}
	switch {
	case cur == nil:
		res.Kind = "new"
	case len(res.Changes) > 0:
		res.Kind = "changed"
	default:
		res.Kind = "same"
	}
	// A renamed item is still found by its old name further on in the file.
	var alias map[string]map[string]string
	if cur != nil && saved.After != nil && str(saved.After["name"]) != str(cur["name"]) {
		old := strings.ToLower(str(cur["name"]))
		alias = map[string]map[string]string{t: {old: id}}
		if hasParent {
			pn := mbNameOf(ctx, tx, pm.table, cur[p.field])
			alias[t][old+" ("+strings.ToLower(pn)+")"] = id
		}
	}
	return res, alias, nil
}

// mbFind is the existing item a row without a Row ID is about: the same name (under the same parent where names
// repeat; a shift by its code; a change reason for the same use). "" message and nil row for a new item.
func mbFind(ctx context.Context, q store.Q, t, name, parentID, usedFor, code string) (store.Row, string, error) {
	m := masters[t]
	var rows []store.Row
	var err error
	p, hasParent := parentOf[t]
	bySeq := options.Find().SetSort(bson.D{{Key: "seq", Value: 1}})
	switch {
	case t == "shift":
		if code == "" {
			return nil, "", nil
		}
		rows, err = q.C("shift").Find(ctx, bson.M{"code": masterRulesNoCase(code)}, bySeq)
	case m.kind != "" && usedFor != "":
		rows, err = q.C("simple_master").Find(ctx, bson.M{"kind": m.kind, "name": masterRulesNoCase(name), "used_for": usedFor}, bySeq)
	case m.kind != "":
		rows, err = q.C("simple_master").Find(ctx, bson.M{"kind": m.kind, "name": masterRulesNoCase(name)}, bySeq)
	case hasParent && parentID != "" && t != "state" && t != "location":
		rows, err = q.C(m.table).Find(ctx, bson.M{"name": masterRulesNoCase(name), p.field: parentID}, bySeq)
	default:
		rows, err = q.C(m.table).Find(ctx, bson.M{"name": masterRulesNoCase(name)}, bySeq)
	}
	if err != nil || len(rows) == 0 {
		return nil, "", err
	}
	if len(rows) > 1 {
		exact := []store.Row{}
		for _, r := range rows {
			if str(r["name"]) == name {
				exact = append(exact, r)
			}
		}
		if len(exact) == 1 {
			return exact[0], "", nil
		}
		if m.kind != "" {
			return nil, fmt.Sprintf("“%s” is a reason for more than one use. Give Used for, or the Row ID", name), nil
		}
		return nil, fmt.Sprintf("“%s” is in more than one %s. Give the %s, or the Row ID", name, masters[p.master].label, capital(masters[p.master].label)), nil
	}
	return rows[0], "", nil
}

// resolve finds an item named in a parent column (or a shift's Locations): its name; "Name (Parent)" where the
// name repeats; or a name it had before it was renamed earlier in this file.
func (st *mbState) resolve(ctx context.Context, q store.Q, t, text string) (string, string, error) {
	m := masters[t]
	p, hasParent := parentOf[t]
	pm := masters[p.master]
	rows, err := mbItems(ctx, q, t, bson.M{"name": masterRulesNoCase(text)})
	if err != nil {
		return "", "", err
	}
	if len(rows) == 1 {
		return rows[0].ID, "", nil
	}
	if len(rows) > 1 {
		opts, exact := []string{}, []string{}
		for _, r := range rows {
			if r.Name == text {
				exact = append(exact, r.ID)
			}
			opts = append(opts, "“"+r.Name+" ("+r.Parent+")”")
		}
		if len(exact) == 1 && !hasParent { // names differing only by case
			return exact[0], "", nil
		}
		if !hasParent {
			return "", fmt.Sprintf("%s “%s” matches more than one %s. Write the name exactly as it is in %s", capital(m.label), text, m.label, mbSheetOf(t)), nil
		}
		return "", fmt.Sprintf("%s “%s” is in more than one %s. Write it as %s", capital(m.label), text, pm.label, strings.Join(opts, " or ")), nil
	}
	// "Name (Parent)": the parent is found the same way (it may itself have been renamed in this file).
	if i := strings.LastIndex(text, " ("); hasParent && i > 0 && strings.HasSuffix(text, ")") {
		n, pt := strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+2:len(text)-1])
		if pid, msg, err := st.resolve(ctx, q, p.master, pt); err != nil {
			return "", "", err
		} else if msg == "" {
			x, err := masterRulesNamed(ctx, q, m.table, bson.M{p.field: pid}, n)
			if err != nil {
				return "", "", err
			}
			if x != nil {
				return str(x["id"]), "", nil
			}
		}
	}
	if id, ok := st.alias[t][strings.ToLower(text)]; ok {
		return id, "", nil
	}
	return "", fmt.Sprintf("%s “%s” is not in %s", capital(m.label), text, mbSheetOf(t)), nil
}

func mbSheetOf(t string) string {
	for _, sh := range mbSheets {
		if sh.Key == t {
			return sh.Sheet
		}
	}
	return t
}

// mbDescribe lists what saveMaster changed, as the check shows it: "Name: A → B", "Region: North → East".
func mbDescribe(ctx context.Context, q store.Q, t string, m master, saved masterSaved) ([]bulkChange, error) {
	if len(saved.Changed) == 0 {
		return nil, nil
	}
	order := map[string]int{}
	for i, f := range m.fields {
		order[f.name] = i
	}
	ch := append([]string{}, saved.Changed...)
	sort.Slice(ch, func(i, j int) bool {
		a, b := order[ch[i]], order[ch[j]]
		if ch[i] == "name" {
			a = -1
		}
		if ch[j] == "name" {
			b = -1
		}
		return a < b
	})
	label := map[string]string{}
	for _, f := range m.fields {
		label[f.name] = f.label()
	}
	locNames := func(v any) (string, error) {
		if v == nil {
			return "All locations", nil
		}
		l, err := q.C("location").Find(ctx, bson.M{"_id": bson.M{"$in": strs(v)}},
			options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetProjection(bson.M{"name": 1}))
		names := make([]string, 0, len(l))
		for _, x := range l {
			names = append(names, str(x["name"]))
		}
		return strings.Join(names, "; "), err
	}
	out := []bulkChange{}
	for _, k := range ch {
		from, to := saved.Before[k], saved.After[k]
		c := bulkChange{Field: label[k]}
		switch {
		case k == "name" && len(saved.Refs) > 0:
			c.From, c.To = str(from), str(to)
			parts := []string{}
			keys := make([]string, 0, len(saved.Refs))
			for x := range saved.Refs {
				keys = append(keys, x)
			}
			sort.Strings(keys)
			for _, x := range keys {
				parts = append(parts, fmt.Sprintf("%d %s", saved.Refs[x], x))
			}
			c.Note = "also updated: " + strings.Join(parts, ", ")
		case strings.HasSuffix(k, "_id"):
			pm := masters[parentOf[t].master]
			c.From, c.To = mbNameOf(ctx, q, pm.table, from), mbNameOf(ctx, q, pm.table, to)
			if saved.Moved > 0 {
				c.Note = fmt.Sprintf("its %d active employee%s move with it", saved.Moved, plural(int(saved.Moved)))
			}
		case k == "corporate":
			c.From, c.To = map[bool]string{true: "Corporate", false: "Operations"}[from == true], map[bool]string{true: "Corporate", false: "Operations"}[to == true]
		case k == "grades":
			c.From, c.To = strings.Join(strs(from), ", "), strings.Join(strs(to), ", ")
		case k == "location_ids":
			var err error
			if c.From, err = locNames(from); err != nil {
				return nil, err
			}
			if c.To, err = locNames(to); err != nil {
				return nil, err
			}
		case k == "start_time" || k == "end_time" || k == "break_minutes":
			c.From, c.To, c.Note = fmt.Sprint(from), fmt.Sprint(to), "from today, as a new shift version"
		default:
			c.From, c.To = "—", fmt.Sprint(to)
			if from != nil {
				c.From = fmt.Sprint(from)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// mbNameOf is the name of the item id of table ("" when there is none).
func mbNameOf(ctx context.Context, q store.Q, table string, id any) string {
	x, _ := q.C(table).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{"name": 1}))
	return str(x["name"])
}

// clockTime reads a time typed in Excel: 09:30, 9:30, 21:30:00, 9:30 PM, or Excel's fraction of a day.
func clockTime(v string) (string, bool) {
	v = strings.ToUpper(strings.TrimSpace(v))
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f < 1 {
		m := int(math.Round(f * 1440))
		return fmt.Sprintf("%02d:%02d", m/60%24, m%60), true
	}
	for _, l := range []string{"15:04", "15:04:05", "3:04 PM", "3:04PM", "3:04:05 PM", "15.04"} {
		if t, err := time.Parse(l, v); err == nil {
			return t.Format("15:04"), true
		}
	}
	return "", false
}
