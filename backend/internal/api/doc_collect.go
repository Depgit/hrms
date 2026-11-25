package api

// Documents from existing employees (owner's request of 12 Nov 2026): "when we onboard an existing employee, the
// employee has the option to upload his documents within 90 days, so we have proper documentation".
//
// Who: every active employee whose record did not come from the joining flow (no offer or candidate made it: they
// were imported or added directly) and who misses at least one mandatory document of Form setup's list (documents
// asked at offer release or joining, for their employment type and department; Mandatory at either stage counts as
// mandatory). They get a window (store.DocWindowColl) that starts the day the feature first sees them: today for
// everyone on file at the first start, the import date for an HR Admin import, the sweep's day for anyone added
// later. Joiners never get one: they upload through the joiner's link.
//
// The employee (My view › My documents, GET/POST /api/me/documents) sees the list with Mandatory / Optional, each
// one's state (missing, uploaded and waiting for HR's check, verified, or sent back with HR's reason) and the days
// left, and uploads, views and replaces their own documents while the window is open; a verified document cannot
// be replaced, and a document Form setup gives to HR is uploaded by HR. Uploads go on their own record
// (owner_kind employee) through the joiner's checks (uploadFile: PDF, JPG or PNG by content, 5 MB, name trimmed;
// downloads are sent with nosniff by fileGet). After the window, self-upload is closed until HR extends it.
//
// HR (Employees › Document collection, /api/doc-collection…): HR Admin and the HRBP within their data scope act
// (emp.directory and emp.govid at Edit); roles that may view employee documents (both at View) read. HR verifies
// or sends back with a reason (the employee is told), extends the window by N days for the selected people (a
// window that has ended reopens from today), sends a reminder, uploads on the employee's behalf (shown as
// "Uploaded by HR") and downloads the tracker as Excel. Nobody checks or extends their own.
//
// Reminders (DocSweepArgs, every 15 minutes and on start, on the job queue): a bell message, and an email when
// email is set up, to the employee when the window opens and at 30, 15, 7 and 1 days left; when the window ends
// with mandatory documents missing, a bell message to their HRBP (HR Admin when nobody holds HRBP for them). Each
// is sent once: the key is added to the window's reminders in the same transaction as the message, only if absent.
// The day reminders and the end message carry the due date in their key, so a window HR extends gets its own.
//
// Audit: every upload (storeDoc), verify, send back, extend, reopen, reminder and setting change; file names and
// document names only, never a document's contents.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/store"
)

func (s *Server) docCollectRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/me/documents", s.a(s.myDocuments))
	m.HandleFunc("POST /api/me/documents", s.a(s.myDocumentUpload))
	m.HandleFunc("GET /api/doc-collection", s.a(s.docTracker))
	m.HandleFunc("GET /api/doc-collection/export", s.docTrackerExport)
	m.HandleFunc("GET /api/doc-collection/settings", s.a(s.docSettingsGet))
	m.HandleFunc("PUT /api/doc-collection/settings", s.a(s.docSettingsPut))
	m.HandleFunc("POST /api/doc-collection/extend", s.a(s.docExtend))
	m.HandleFunc("POST /api/doc-collection/remind", s.a(s.docRemind))
	m.HandleFunc("GET /api/doc-collection/{id}", s.a(s.docEmployee))
	m.HandleFunc("POST /api/doc-collection/{id}/files", s.a(s.docHRUpload))
	m.HandleFunc("POST /api/doc-collection/{id}/review", s.a(s.docReview))
}

// ---------- Settings ----------

type docSettings struct {
	Days int  `json:"days"`
	On   bool `json:"on"`
}

func loadDocSettings(ctx context.Context, q store.Q) docSettings {
	set := docSettings{Days: store.DefaultDocWindowDays, On: store.DefaultDocWindowOn}
	if x, _ := onboardingOne(ctx, q, "onb_setting", 1); x != nil {
		if x["doc_window_days"] != nil {
			if d := toInt(x["doc_window_days"]); d > 0 {
				set.Days = d
			}
		}
		if on, ok := x["doc_window_on"].(bool); ok {
			set.On = on
		}
	}
	return set
}

func (s *Server) docSettingsGet(r *http.Request, c *Ctx) (any, error) {
	if c.Level("onb.setup") < 1 && docAccess(c, false) != nil {
		return nil, forbidden("")
	}
	return loadDocSettings(r.Context(), s.st.DB), nil
}

func (s *Server) docSettingsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("onb.setup", 2); err != nil {
		return nil, err
	}
	var b docSettings
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Days < 1 || b.Days > 365 {
		return nil, bad("The window must be between 1 and 365 days")
	}
	ctx := r.Context()
	err := s.st.Tx(ctx, func(tx store.Q) error {
		before := loadDocSettings(ctx, tx)
		if _, err := tx.C("onb_setting").Upsert(ctx, bson.M{"_id": 1}, bson.M{"doc_window_days": b.Days, "doc_window_on": b.On,
			"updated_by": c.Name, "updated_at": tx.Now()}, nil); err != nil {
			return err
		}
		state := "off"
		if b.On {
			state = "on"
		}
		return audit(ctx, tx, c, fmt.Sprintf("Document collection for existing employees %s, %d days", state, b.Days), "onb_setting", "1", before, b)
	})
	if err != nil {
		return nil, err
	}
	if b.On { // anyone without a window gets one from today
		if _, err := s.DocSweep(ctx); err != nil {
			log.Printf("document collection sweep: %v", err)
		}
	}
	return b, nil
}

// ---------- What an employee is asked for, and what they have ----------

// docNeed is one document of Form setup's list for an employee.
type docNeed struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Mandatory bool   `json:"mandatory"`
	ByHR      bool   `json:"byHR"` // Form setup gives it to HR: the employee cannot upload it
}

// docNeeds is the list for an employment type and department: every document switched on, for that type and
// department, asked at offer release or at joining; Mandatory at either stage makes it mandatory.
func docNeeds(ds []onbDoc, typ, dept string) []docNeed {
	out := []docNeed{}
	for _, d := range ds {
		if (d.Join > 0 || d.Offer > 0) && d.applies(typ, dept) {
			out = append(out, docNeed{Key: d.Key, Label: d.Label, Mandatory: max(d.Join, d.Offer) == 2, ByHR: d.FilledBy == store.FilledByHR})
		}
	}
	return out
}

// currentDocs are the current documents on the records of these employees (their own, and those gathered while
// they were a candidate), by employee and document key; the newest wins when two stages hold the same key.
func currentDocs(ctx context.Context, q store.Q, ids []string) (map[string]map[string]store.Row, error) {
	out := map[string]map[string]store.Row{}
	for start := 0; start < len(ids); start += 1000 {
		chunk := ids[start:min(start+1000, len(ids))]
		rows, err := q.C("document").Find(ctx, bson.M{"replaced_at": nil, "$or": bson.A{bson.M{"employee_id": bson.M{"$in": chunk}},
			bson.M{"owner_kind": "employee", "owner_id": bson.M{"$in": chunk}}}},
			options.Find().SetSort(bson.D{{Key: "uploaded_at", Value: 1}, {Key: "_id", Value: 1}}))
		if err != nil {
			return nil, err
		}
		for _, d := range rows {
			emp := str(d["employee_id"])
			if d["owner_kind"] == "employee" {
				emp = str(d["owner_id"])
			}
			if out[emp] == nil {
				out[emp] = map[string]store.Row{}
			}
			out[emp][str(d["doc_key"])] = d
		}
	}
	return out, nil
}

// docItem is one document as the employee and HR see it.
type docItem struct {
	docNeed
	Status     string         `json:"status"` // missing | uploaded (waiting for HR's check) | verified | returned
	File       map[string]any `json:"file"`
	Reason     string         `json:"reason,omitempty"`
	ReviewedBy string         `json:"reviewedBy,omitempty"`
	ReviewedAt any            `json:"reviewedAt,omitempty"`
	CanUpload  bool           `json:"canUpload"`
}

// docCount counts an employee's documents: of the mandatory ones, how many are in (uploaded or verified, not
// sent back), verified and missing (never uploaded, or sent back); of all, how many wait for HR and were sent back.
type docCount struct {
	Mandatory int `json:"mandatory"`
	Done      int `json:"done"`
	Verified  int `json:"verified"`
	Missing   int `json:"missing"`
	ToCheck   int `json:"toCheck"`
	Returned  int `json:"returned"`
}

// docItems pairs the list with what is on record.
func docItems(needs []docNeed, have map[string]store.Row, emp string) ([]docItem, docCount, []string) {
	items := make([]docItem, 0, len(needs))
	var cnt docCount
	var missing []string
	for _, n := range needs {
		it := docItem{docNeed: n, Status: "missing"}
		if d := have[n.Key]; d != nil {
			by := str(d["uploaded_by"])
			it.File = map[string]any{"id": d["id"], "name": d["file_name"], "uploadedAt": d["uploaded_at"], "uploadedBy": by,
				"byHR": !strings.HasSuffix(by, "("+emp+")")}
			switch str(d["review_state"]) {
			case store.DocVerified:
				it.Status = "verified"
			case store.DocReturned:
				it.Status = "returned"
				it.Reason = str(d["review_note"])
			default:
				it.Status = "uploaded"
			}
			if d["review_at"] != nil {
				it.ReviewedBy, it.ReviewedAt = str(d["review_by"]), d["review_at"]
			}
		}
		switch it.Status {
		case "uploaded":
			cnt.ToCheck++
		case "returned":
			cnt.Returned++
		}
		if n.Mandatory {
			cnt.Mandatory++
			switch it.Status {
			case "uploaded", "verified":
				cnt.Done++
				if it.Status == "verified" {
					cnt.Verified++
				}
			default:
				cnt.Missing++
				missing = append(missing, n.Label)
			}
		}
		items = append(items, it)
	}
	return items, cnt, missing
}

// ---------- The window ----------

// docWin is a window as the screens show it. Due is the last day to upload.
type docWin struct {
	Start       string `json:"start"`
	Due         string `json:"due"`
	DaysLeft    int    `json:"daysLeft"`
	OverdueDays int    `json:"overdueDays"`
	Open        bool   `json:"open"`
	ExtraDays   int    `json:"extraDays"`
	Source      string `json:"source"`
}

func dateAdd(d string, n int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// daysFrom is the number of days from a to b (negative when b is earlier).
func daysFrom(a, b string) int {
	ta, err1 := time.Parse("2006-01-02", a)
	tb, err2 := time.Parse("2006-01-02", b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(tb.Sub(ta).Hours() / 24)
}

func windowOf(w store.Row, set docSettings, today string) *docWin {
	if w == nil {
		return nil
	}
	start, extra := str(w["start"]), toInt(w["extra_days"])
	due := dateAdd(start, set.Days+extra)
	left := daysFrom(today, due)
	v := &docWin{Start: start, Due: due, DaysLeft: left, Open: set.On && left >= 0, ExtraDays: extra, Source: str(w["source"])}
	if left < 0 {
		v.OverdueDays = -left
	}
	return v
}

// docStatus is the tracker's status: Complete (every mandatory document in and verified), To check (all in, some
// waiting for HR), Overdue (missing after the window) or Open.
func docStatus(cnt docCount, w *docWin) string {
	switch {
	case cnt.Missing == 0 && cnt.Done == cnt.Verified:
		return "Complete"
	case cnt.Missing == 0:
		return "To check"
	case w != nil && w.DaysLeft < 0:
		return "Overdue"
	}
	return "Open"
}

// docPerson is what the document screens need of an employee.
type docPerson struct {
	ID, Name, Type, DeptID, Dept, LocID, Status string
}

func loadDocPerson(ctx context.Context, q store.Q, id string) (*docPerson, error) {
	e, err := q.C("employee").FindOne(ctx, bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, notFound("Employee not found")
	}
	p := &docPerson{ID: id, Name: str(e["name"]), Type: str(e["type"]), DeptID: str(e["department_id"]), LocID: str(e["location_id"]), Status: str(e["status"])}
	if d, _ := q.C("department").FindOne(ctx, bson.M{"_id": e["department_id"]}); d != nil {
		p.Dept = str(d["name"])
	}
	return p, nil
}

// docDetail is one employee's list, counts and window. hr: as HR sees it (HR may upload any document, at any time).
func (s *Server) docDetail(ctx context.Context, p *docPerson, hr bool) (map[string]any, []docNeed, map[string]store.Row, error) {
	q := s.st.DB
	_, ds, err := loadSetup(ctx, q)
	if err != nil {
		return nil, nil, nil, err
	}
	needs := docNeeds(ds, p.Type, p.Dept)
	all, err := currentDocs(ctx, q, []string{p.ID})
	if err != nil {
		return nil, nil, nil, err
	}
	have := all[p.ID]
	wrow, err := q.C(store.DocWindowColl).FindOne(ctx, bson.M{"employee_id": p.ID})
	if err != nil {
		return nil, nil, nil, err
	}
	set := loadDocSettings(ctx, q)
	win := windowOf(wrow, set, store.Today())
	items, cnt, _ := docItems(needs, have, p.ID)
	for i := range items {
		if hr {
			items[i].CanUpload = true
		} else {
			items[i].CanUpload = win != nil && win.Open && !items[i].ByHR && items[i].Status != "verified"
		}
	}
	var winOut any
	if win != nil {
		winOut = win
	}
	return map[string]any{"employee": map[string]any{"id": p.ID, "name": p.Name, "type": p.Type, "department": p.Dept},
		"window": winOut, "docs": items, "counts": cnt, "status": docStatus(cnt, win), "settings": set}, needs, have, nil
}

// ---------- My view › My documents ----------

func (s *Server) myDocuments(r *http.Request, c *Ctx) (any, error) {
	p, err := loadDocPerson(r.Context(), s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	out, _, _, err := s.docDetail(r.Context(), p, false)
	return out, err
}

// myDocumentUpload: the employee uploads one of their own documents while their window is open.
func (s *Server) myDocumentUpload(r *http.Request, c *Ctx) (any, error) {
	key, err := uploadForm(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	p, err := loadDocPerson(ctx, s.st.DB, c.EmployeeID)
	if err != nil {
		return nil, err
	}
	detail, _, _, err := s.docDetail(ctx, p, false)
	if err != nil {
		return nil, err
	}
	var it *docItem
	for _, x := range detail["docs"].([]docItem) {
		if x.Key == key {
			it = &x
			break
		}
	}
	switch {
	case it == nil:
		return nil, bad("This document is not on your list")
	case it.ByHR:
		return nil, bad("HR adds this document to your record")
	case detail["window"] == nil:
		return nil, bad("HR collects your documents. Ask HR if one is missing.")
	case !detail["window"].(*docWin).Open:
		return nil, bad("Document upload is closed. Ask HR to reopen it.")
	case it.Status == "verified":
		return nil, bad("HR has verified this document, so it cannot be replaced")
	}
	f, err := uploadFile(r)
	if err != nil {
		return nil, err
	}
	row, err := s.storeDoc(ctx, c, "employee", c.EmployeeID, key, f, c.Name+" ("+c.EmployeeID+")")
	if err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "id": row["id"], "fileName": row["file_name"], "uploadedAt": row["uploaded_at"]}, nil
}

// ---------- HR: Employees › Document collection ----------

// docAccess: HR's tracker. View needs Employee directory and Aadhaar and PAN at View (as seeing someone's
// documents does); acting needs both at Edit. Never from My view.
func docAccess(c *Ctx, write bool) error {
	need := 1
	if write {
		need = 2
	}
	if c.Role == "Employee" || c.Level("emp.directory") < need || c.Level("emp.govid") < need {
		return forbidden("")
	}
	return nil
}

// docHRPerson checks HR's access to one employee's documents and loads them. Nobody acts on their own.
func (s *Server) docHRPerson(ctx context.Context, c *Ctx, id string, write bool) (*docPerson, error) {
	if err := docAccess(c, write); err != nil {
		return nil, err
	}
	if write && id == c.EmployeeID {
		return nil, forbidden("You cannot act on your own documents here. Use My view › My documents.")
	}
	if err := s.canFiles(ctx, c, "employee", id, write); err != nil {
		return nil, err
	}
	return loadDocPerson(ctx, s.st.DB, id)
}

func (s *Server) docEmployee(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	p, err := s.docHRPerson(ctx, c, r.PathValue("id"), false)
	if err != nil {
		return nil, err
	}
	out, _, _, err := s.docDetail(ctx, p, true)
	if err != nil {
		return nil, err
	}
	out["canEdit"] = docAccess(c, true) == nil && p.ID != c.EmployeeID
	if !out["canEdit"].(bool) {
		items := out["docs"].([]docItem)
		for i := range items {
			items[i].CanUpload = false
		}
	}
	return out, nil
}

// docRow is one line of the tracker.
type docRow struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Department  string   `json:"department"`
	Location    string   `json:"location"`
	Start       string   `json:"start"`
	Due         string   `json:"due"`
	DaysLeft    int      `json:"daysLeft"`
	OverdueDays int      `json:"overdueDays"`
	ExtraDays   int      `json:"extraDays"`
	Status      string   `json:"status"`
	MissingDocs []string `json:"missingDocs"`
	docCount
}

// docRows is the tracker: every active employee with a window, inside the caller's scope, overdue first, then by
// days left and name.
func (s *Server) docRows(ctx context.Context, c *Ctx) ([]docRow, error) {
	q := s.st.DB
	wins, err := q.C(store.DocWindowColl).Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	if len(wins) == 0 {
		return []docRow{}, nil
	}
	byEmp := map[string]store.Row{}
	ids := bson.A{}
	for _, w := range wins {
		byEmp[str(w["employee_id"])] = w
		ids = append(ids, w["employee_id"])
	}
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	filter := bson.D{{Key: "$and", Value: bson.A{bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "status", Value: "Active"}}, scope}}}
	emps, err := q.C("employee").Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	depts, err := ctxIndex(ctx, q, "department", "_id", ctxValues(emps, "department_id"))
	if err != nil {
		return nil, err
	}
	locs, err := ctxIndex(ctx, q, "location", "_id", ctxValues(emps, "location_id"))
	if err != nil {
		return nil, err
	}
	empIDs := make([]string, 0, len(emps))
	for _, e := range emps {
		empIDs = append(empIDs, str(e["id"]))
	}
	have, err := currentDocs(ctx, q, empIDs)
	if err != nil {
		return nil, err
	}
	_, ds, err := loadSetup(ctx, q)
	if err != nil {
		return nil, err
	}
	set, today := loadDocSettings(ctx, q), store.Today()
	out := make([]docRow, 0, len(emps))
	for _, e := range emps {
		id := str(e["id"])
		dept := str(depts[str(e["department_id"])]["name"])
		_, cnt, missing := docItems(docNeeds(ds, str(e["type"]), dept), have[id], id)
		w := windowOf(byEmp[id], set, today)
		if missing == nil {
			missing = []string{}
		}
		out = append(out, docRow{ID: id, Name: str(e["name"]), Department: dept, Location: str(locs[str(e["location_id"])]["name"]),
			Start: w.Start, Due: w.Due, DaysLeft: w.DaysLeft, OverdueDays: w.OverdueDays, ExtraDays: w.ExtraDays,
			Status: docStatus(cnt, w), MissingDocs: missing, docCount: cnt})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ra, rb := docRank(a), docRank(b)
		if ra != rb {
			return ra < rb
		}
		if a.DaysLeft != b.DaysLeft {
			return a.DaysLeft < b.DaysLeft
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return out, nil
}

// docRank orders the tracker: overdue, open, to check, complete.
func docRank(r docRow) int {
	return map[string]int{"Overdue": 0, "Open": 1, "To check": 2, "Complete": 3}[r.Status]
}

// docFilter: all, overdue, due7 (missing documents and at most 7 days left), tocheck, complete.
func docFilter(rows []docRow, f string) ([]docRow, error) {
	keep := func(r docRow) bool { return true }
	switch f {
	case "", "all":
	case "overdue":
		keep = func(r docRow) bool { return r.Status == "Overdue" }
	case "due7":
		keep = func(r docRow) bool { return r.Missing > 0 && r.DaysLeft >= 0 && r.DaysLeft <= 7 }
	case "tocheck":
		keep = func(r docRow) bool { return r.ToCheck > 0 }
	case "complete":
		keep = func(r docRow) bool { return r.Status == "Complete" }
	default:
		return nil, bad("Unknown filter")
	}
	out := []docRow{}
	for _, r := range rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Server) docTracker(r *http.Request, c *Ctx) (any, error) {
	if err := docAccess(c, false); err != nil {
		return nil, err
	}
	ctx := r.Context()
	rows, err := s.docRows(ctx, c)
	if err != nil {
		return nil, err
	}
	totals := map[string]int{"all": len(rows)}
	for _, f := range []string{"overdue", "due7", "tocheck", "complete"} {
		l, _ := docFilter(rows, f)
		totals[f] = len(l)
	}
	shown, err := docFilter(rows, r.URL.Query().Get("filter"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"rows": shown, "totals": totals, "settings": loadDocSettings(ctx, s.st.DB), "canEdit": docAccess(c, true) == nil}, nil
}

// docTrackerExport downloads the tracker (with the screen's filter) as an Excel file.
func (s *Server) docTrackerExport(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := docAccess(c, false); err != nil {
		writeErr(w, err)
		return
	}
	if !c.CanDownload {
		writeErr(w, forbidden("Your role cannot download files"))
		return
	}
	ctx := r.Context()
	rows, err := s.docRows(ctx, c)
	if err == nil {
		rows, err = docFilter(rows, r.URL.Query().Get("filter"))
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	f := excelize.NewFile()
	defer f.Close()
	const sheet = "Document collection"
	_ = f.SetSheetName("Sheet1", sheet)
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}})
	cols := []string{"Employee ID", "Name", "Department", "Location", "Window started", "Last day to upload", "Days left", "Overdue days",
		"Mandatory", "Uploaded", "Verified", "Waiting for HR check", "Sent back", "Missing mandatory documents", "Status"}
	widths := []float64{14, 26, 18, 26, 14, 16, 10, 12, 11, 10, 10, 14, 10, 44, 12}
	for i, h := range cols {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(sheet, cell, h)
		cn, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sheet, cn, cn, widths[i])
	}
	last, _ := excelize.ColumnNumberToName(len(cols))
	_ = f.SetCellStyle(sheet, "A1", last+"1", head)
	for i, x := range rows {
		left := any(x.DaysLeft)
		if x.DaysLeft < 0 {
			left = 0
		}
		vals := []any{x.ID, x.Name, x.Department, x.Location, x.Start, x.Due, left, x.OverdueDays, x.Mandatory, x.Done, x.Verified, x.ToCheck, x.Returned,
			strings.Join(x.MissingDocs, ", "), x.Status}
		for j, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(j+1, i+2)
			_ = f.SetCellValue(sheet, cell, v)
		}
	}
	_ = f.SetPanes(sheet, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		writeErr(w, err)
		return
	}
	name := "document-collection-" + store.Today() + ".xlsx"
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	_, _ = w.Write(buf.Bytes())
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Downloaded the document collection tracker (%d employees)", len(rows)), "doc_window", "", nil,
		map[string]any{"filter": r.URL.Query().Get("filter"), "rows": len(rows)})
}

// docHRUpload: HR uploads a document of the list on the employee's behalf (shown as "Uploaded by HR").
func (s *Server) docHRUpload(r *http.Request, c *Ctx) (any, error) {
	key, err := uploadForm(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	p, err := s.docHRPerson(ctx, c, r.PathValue("id"), true)
	if err != nil {
		return nil, err
	}
	_, needs, _, err := s.docDetail(ctx, p, true)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, n := range needs {
		ok = ok || n.Key == key
	}
	if !ok {
		return nil, bad("This document is not on the employee's list")
	}
	f, err := uploadFile(r)
	if err != nil {
		return nil, err
	}
	row, err := s.storeDoc(ctx, c, "employee", p.ID, key, f, c.Name+" ("+c.EmployeeID+")")
	if err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "id": row["id"], "fileName": row["file_name"], "uploadedAt": row["uploaded_at"]}, nil
}

// docReview: HR verifies a document, or sends it back with a reason (the employee is told and may upload again).
func (s *Server) docReview(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		DocKey string `json:"docKey"`
		Action string `json:"action"` // verify | return
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	switch b.Action {
	case "verify":
		b.Reason = ""
	case "return":
		if b.Reason == "" {
			return nil, bad("Say why the document is sent back")
		}
		if len([]rune(b.Reason)) > 300 {
			return nil, bad("Keep the reason to 300 characters")
		}
	default:
		return nil, bad("Choose Verify or Send back")
	}
	ctx := r.Context()
	p, err := s.docHRPerson(ctx, c, r.PathValue("id"), true)
	if err != nil {
		return nil, err
	}
	detail, needs, have, err := s.docDetail(ctx, p, true)
	if err != nil {
		return nil, err
	}
	var need *docNeed
	for i := range needs {
		if needs[i].Key == b.DocKey {
			need = &needs[i]
		}
	}
	if need == nil {
		return nil, bad("This document is not on the employee's list")
	}
	d := have[b.DocKey]
	if d == nil {
		return nil, bad("Nothing is uploaded for %s", need.Label)
	}
	state, action := store.DocVerified, "Verified "
	if b.Action == "return" {
		state, action = store.DocReturned, "Sent back "
	}
	win, _ := detail["window"].(*docWin)
	err = s.st.Tx(ctx, func(tx store.Q) error {
		n, err := tx.C("document").Update(ctx, bson.M{"_id": d["id"], "replaced_at": nil}, bson.M{"$set": bson.M{"review_state": state,
			"review_note": strPtr(b.Reason), "review_by": c.Name, "review_at": tx.Now()}})
		if err != nil {
			return err
		}
		if n == 0 {
			return bad("The document was replaced just now. Open it again.")
		}
		if b.Action == "return" {
			text := fmt.Sprintf("HR sent back your %s: %s. Please upload it again in My documents.", need.Label, b.Reason)
			if win == nil || !win.Open {
				text = fmt.Sprintf("HR sent back your %s: %s. HR will reopen the upload for you.", need.Label, b.Reason)
			}
			if err := docNotice(ctx, tx, p.ID, c.EmployeeID, text, "/my-documents", "Employee"); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, action+need.Label+" of "+p.Name+" ("+p.ID+")", "document", str(d["id"]), nil,
			map[string]any{"employee": p.ID, "doc": b.DocKey, "file": d["file_name"], "reason": strPtr(b.Reason)})
	})
	if err != nil {
		return nil, err
	}
	if b.Action == "return" {
		s.docMail(ctx, p.ID, "A document was sent back", fmt.Sprintf("HR sent back your %s: %s.\n\nPlease upload it again in the HRMS: My documents.", need.Label, b.Reason))
	}
	return map[string]any{"ok": true, "status": map[string]string{store.DocVerified: "verified", store.DocReturned: "returned"}[state],
		"windowOpen": win != nil && win.Open}, nil
}

// docExtend gives the selected people N more days. A window still open moves its last day; one that has ended
// reopens with N days from today.
func (s *Server) docExtend(r *http.Request, c *Ctx) (any, error) {
	if err := docAccess(c, true); err != nil {
		return nil, err
	}
	var b struct {
		IDs  []string `json:"ids"`
		Days int      `json:"days"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Days < 1 || b.Days > 365 {
		return nil, bad("Extend by 1 to 365 days")
	}
	if len(b.IDs) == 0 || len(b.IDs) > 1000 {
		return nil, bad("Choose the employees to extend")
	}
	ctx := r.Context()
	set, today := loadDocSettings(ctx, s.st.DB), store.Today()
	type skip struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	done, skipped := []map[string]any{}, []skip{}
	for _, id := range docIDs(b.IDs) {
		p, err := s.docHRPerson(ctx, c, id, true)
		if err != nil {
			skipped = append(skipped, skip{id, err.Error()})
			continue
		}
		var due string
		var reopened bool
		err = s.st.Tx(ctx, func(tx store.Q) error {
			w, err := tx.C(store.DocWindowColl).Lock(ctx, bson.M{"employee_id": id})
			if err != nil {
				return err
			}
			if w == nil {
				return bad("No document window")
			}
			cur := windowOf(w, set, today)
			base := cur.Due
			reopened = cur.DaysLeft < 0
			if reopened {
				base = today
			}
			due = dateAdd(base, b.Days)
			extra := daysFrom(cur.Start, due) - set.Days
			if extra < 0 {
				extra = 0
			}
			if _, err := tx.C(store.DocWindowColl).Update(ctx, bson.M{"_id": w["id"]}, bson.M{"$set": bson.M{"extra_days": extra,
				"updated_at": tx.Now(), "updated_by": c.Name}}); err != nil {
				return err
			}
			action := fmt.Sprintf("Extended document upload for %s (%s) by %d days, until %s", p.Name, id, b.Days, longDate(due))
			if reopened {
				action = fmt.Sprintf("Reopened document upload for %s (%s) until %s", p.Name, id, longDate(due))
			}
			if err := docNotice(ctx, tx, id, c.EmployeeID, fmt.Sprintf("HR gave you until %s to upload your documents in My documents.", longDate(due)),
				"/my-documents", "Employee"); err != nil {
				return err
			}
			return audit(ctx, tx, c, action, "doc_window", str(w["id"]), map[string]any{"due": cur.Due}, map[string]any{"due": due, "days": b.Days})
		})
		if err != nil {
			skipped = append(skipped, skip{id, err.Error()})
			continue
		}
		done = append(done, map[string]any{"id": id, "due": due, "reopened": reopened})
	}
	return map[string]any{"done": done, "skipped": skipped}, nil
}

func docIDs(l []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, x := range l {
		x = strings.TrimSpace(x)
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// docRemind sends the selected people a reminder now (bell, and email when set up): at most one a day each, and
// only to someone with mandatory documents still missing.
func (s *Server) docRemind(r *http.Request, c *Ctx) (any, error) {
	if err := docAccess(c, true); err != nil {
		return nil, err
	}
	var b struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.IDs) == 0 || len(b.IDs) > 1000 {
		return nil, bad("Choose the employees to remind")
	}
	ctx := r.Context()
	type skip struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	sent, skipped := []string{}, []skip{}
	for _, id := range docIDs(b.IDs) {
		p, err := s.docHRPerson(ctx, c, id, true)
		if err != nil {
			skipped = append(skipped, skip{id, err.Error()})
			continue
		}
		detail, _, _, err := s.docDetail(ctx, p, false)
		if err != nil {
			return nil, err
		}
		cnt, win := detail["counts"].(docCount), detail["window"]
		var text string
		err = s.st.Tx(ctx, func(tx store.Q) error {
			if win == nil {
				return bad("No document window")
			}
			w := win.(*docWin)
			if cnt.Missing == 0 {
				return bad("Nothing mandatory is missing")
			}
			startOfDay, _ := time.ParseInLocation("2006-01-02", store.Today(), store.Loc())
			n, err := tx.C(store.DocWindowColl).Update(ctx, bson.M{"employee_id": id, "$or": bson.A{bson.M{"reminded_at": nil}, bson.M{"reminded_at": bson.M{"$lt": startOfDay}}}},
				bson.M{"$set": bson.M{"reminded_at": tx.Now()}})
			if err != nil {
				return err
			}
			if n == 0 {
				return bad("Already reminded today")
			}
			text = docReminderText(cnt, w)
			if err := docNotice(ctx, tx, id, c.EmployeeID, text, "/my-documents", "Employee"); err != nil {
				return err
			}
			return audit(ctx, tx, c, "Sent a document reminder to "+p.Name+" ("+id+")", "doc_window", id, nil, map[string]any{"missing": cnt.Missing})
		})
		if err != nil {
			skipped = append(skipped, skip{id, err.Error()})
			continue
		}
		s.docMail(ctx, id, "Please upload your documents", text)
		sent = append(sent, id)
	}
	return map[string]any{"sent": sent, "skipped": skipped}, nil
}

func docReminderText(cnt docCount, w *docWin) string {
	if !w.Open {
		return fmt.Sprintf("%d mandatory document(s) are still missing from your record. Upload is closed: ask HR to reopen it.", cnt.Missing)
	}
	left := fmt.Sprintf("%d days left", w.DaysLeft)
	switch w.DaysLeft {
	case 0:
		left = "today is the last day"
	case 1:
		left = "1 day left"
	}
	return fmt.Sprintf("Reminder: %s to upload your documents in My documents (by %s). %d mandatory document(s) still missing.", left, longDate(w.Due), cnt.Missing)
}

// docNotice puts a message in someone's bell. from may be empty (the system).
func docNotice(ctx context.Context, tx store.Q, to, from, text, link, view string) error {
	_, err := tx.C("notice").Insert(ctx, bson.D{{Key: "to_employee_id", Value: to}, {Key: "from_employee_id", Value: strPtr(from)}, {Key: "kind", Value: "documents"},
		{Key: "text", Value: text}, {Key: "link", Value: link}, {Key: "view", Value: view}, {Key: "ref", Value: []string{}}})
	return err
}

// docMail emails the employee when email is set up and they have an address; a failure is only logged.
func (s *Server) docMail(ctx context.Context, emp, subject, text string) {
	if s.mail == nil {
		return
	}
	p, _ := s.st.DB.C("employee_personal").FindOne(ctx, bson.M{"employee_id": emp})
	to := strings.TrimSpace(str(p["email"]))
	if to == "" {
		return
	}
	comp := companyName(ctx, s.st.DB)
	if s.appURL != "" { // every document mail is about My documents: link to it (owner, 26 Nov 2026)
		text += "\n\nOpen My documents: " + strings.TrimRight(s.appURL, "/") + "/my-documents"
	}
	if err := s.mail.Send(ctx, mail.Message{To: to, Subject: comp + ": " + subject, Text: text + "\n\n" + comp + " HR\n"}); err != nil {
		log.Printf("document collection email to %s: %v", emp, err)
	}
}

// ---------- The sweep: windows and reminders ----------

const docSweepEvery = 15 * time.Minute

// docReminderDays are the days left at which the employee is reminded.
var docReminderDays = []int{1, 7, 15, 30}

// DocSweepArgs is the periodic document collection sweep.
type DocSweepArgs struct{}

func (DocSweepArgs) Kind() string { return "doc_collection" }

func (DocSweepArgs) InsertOpts() jobs.InsertOpts {
	return jobs.InsertOpts{UniqueOpts: jobs.UniqueOpts{ByPeriod: docSweepEvery}}
}

type docSweepWorker struct {
	jobs.WorkerDefaults[DocSweepArgs]
	s *Server
}

func (w *docSweepWorker) Work(ctx context.Context, _ *jobs.Job[DocSweepArgs]) error {
	_, err := w.s.DocSweep(ctx)
	return err
}

// DocSweep opens a window for every eligible employee without one and sends the reminders that are due. It
// returns how many windows it opened and messages it sent. Safe to run at any time and from several servers.
func (s *Server) DocSweep(ctx context.Context) (map[string]int, error) {
	out := map[string]int{"opened": 0, "employee": 0, "hrbp": 0}
	q := s.st.DB
	set := loadDocSettings(ctx, q)
	if !set.On {
		return out, nil
	}
	today := store.Today()
	_, ds, err := loadSetup(ctx, q)
	if err != nil {
		return out, err
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"status": "Active"}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return out, err
	}
	depts, err := ctxIndex(ctx, q, "department", "_id", ctxValues(emps, "department_id"))
	if err != nil {
		return out, err
	}
	wins, err := q.C(store.DocWindowColl).Find(ctx, bson.M{})
	if err != nil {
		return out, err
	}
	first := len(wins) == 0
	byEmp := map[string]store.Row{}
	for _, w := range wins {
		byEmp[str(w["employee_id"])] = w
	}
	joined := map[string]bool{} // came through the joining flow
	for _, coll := range []string{"offer", "candidate"} {
		l, err := q.C(coll).Distinct(ctx, "employee_id", bson.M{"employee_id": bson.M{"$ne": nil}})
		if err != nil {
			return out, err
		}
		for _, x := range l {
			joined[str(x)] = true
		}
	}
	ids := make([]string, 0, len(emps))
	for _, e := range emps {
		ids = append(ids, str(e["id"]))
	}
	have, err := currentDocs(ctx, q, ids)
	if err != nil {
		return out, err
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	source := "added"
	if first {
		source = "startup"
	}
	for _, e := range emps {
		id := str(e["id"])
		needs := docNeeds(ds, str(e["type"]), str(depts[str(e["department_id"])]["name"]))
		_, cnt, missing := docItems(needs, have[id], id)
		w := byEmp[id]
		if w == nil {
			if joined[id] || cnt.Missing == 0 {
				continue
			}
			ins, err := q.C(store.DocWindowColl).Upsert(ctx, bson.M{"employee_id": id}, nil, bson.M{"start": today, "source": source})
			if err != nil {
				keep(err)
				continue
			}
			if ins {
				out["opened"]++
			}
			if w, err = q.C(store.DocWindowColl).FindOne(ctx, bson.M{"employee_id": id}); err != nil || w == nil {
				keep(err)
				continue
			}
		}
		if cnt.Missing == 0 {
			continue
		}
		win := windowOf(w, set, today)
		if win.DaysLeft >= 0 {
			th := 0
			for _, d := range docReminderDays {
				if win.DaysLeft <= d {
					th = d
					break
				}
			}
			dayKey := ""
			if th > 0 {
				dayKey = fmt.Sprintf("d%d:%s", th, win.Due)
			}
			if !containsStr(strs(w["reminders"]), "open") {
				text := fmt.Sprintf("Please upload your documents in My documents by %s: %d mandatory document(s) missing (%s).", longDate(win.Due), cnt.Missing, strings.Join(missing, ", "))
				sent, err := s.docOnce(ctx, w, []string{"open", dayKey}, func(tx store.Q) error {
					return docNotice(ctx, tx, id, "", text, "/my-documents", "Employee")
				})
				keep(err)
				if sent {
					out["employee"]++
					s.docMail(ctx, id, "Please upload your documents", text)
				}
				continue
			}
			if dayKey != "" && !containsStr(strs(w["reminders"]), dayKey) {
				text := docReminderText(cnt, win)
				sent, err := s.docOnce(ctx, w, []string{dayKey}, func(tx store.Q) error {
					return docNotice(ctx, tx, id, "", text, "/my-documents", "Employee")
				})
				keep(err)
				if sent {
					out["employee"]++
					s.docMail(ctx, id, "Please upload your documents", text)
				}
			}
			continue
		}
		// Ended with mandatory documents missing: their HRBP is told once.
		endKey := "ended:" + win.Due
		if containsStr(strs(w["reminders"]), endKey) {
			continue
		}
		to, view := docHRBPs(ctx, q, id)
		text := fmt.Sprintf("Document upload closed for %s (%s) on %s with %d mandatory document(s) missing: %s.", str(e["name"]), id, longDate(win.Due),
			cnt.Missing, strings.Join(missing, ", "))
		sent, err := s.docOnce(ctx, w, []string{endKey}, func(tx store.Q) error {
			for i, p := range to {
				if err := docNotice(ctx, tx, p, "", text, "/employees?tab=docs&filter=overdue", view[i]); err != nil {
					return err
				}
			}
			return nil
		})
		keep(err)
		if sent {
			out["hrbp"] += len(to)
		}
	}
	return out, firstErr
}

// docOnce claims the reminder keys on the window (the first key must not be there yet) and, in the same
// transaction, sends the messages; it reports whether this call sent them.
func (s *Server) docOnce(ctx context.Context, w store.Row, keys []string, send func(tx store.Q) error) (bool, error) {
	add := []string{}
	for _, k := range keys {
		if k != "" {
			add = append(add, k)
		}
	}
	sent := false
	err := s.st.Tx(ctx, func(tx store.Q) error {
		sent = false
		n, err := tx.C(store.DocWindowColl).Update(ctx, bson.M{"_id": w["id"], "reminders": bson.M{"$ne": add[0]}},
			bson.M{"$addToSet": bson.M{"reminders": bson.M{"$each": add}}})
		if err != nil || n == 0 {
			return err
		}
		if err := send(tx); err != nil {
			return err
		}
		sent = true
		return nil
	})
	return sent, err
}

// docHRBPs are who hears that an employee's window ended: the HRBP who holds the employee's place, else every
// active HR Admin; with the role view to open the message in.
func docHRBPs(ctx context.Context, q store.Q, emp string) ([]string, []string) {
	if sub, err := loadSubject(ctx, q, emp); err == nil {
		if p := resolvePerson(ctx, q, "HRBP", sub, approval.Facts{}); p.ID != "" {
			return []string{p.ID}, []string{"HRBP"}
		}
	}
	maps, _ := q.C("user_role_map").Find(ctx, bson.M{"role_name": "HR Admin", "active": true})
	var to, view []string
	for _, m := range maps {
		id := str(m["employee_id"])
		if ok, _ := q.C("employee").Exists(ctx, bson.M{"_id": id, "status": bson.M{"$ne": "Inactive"}}); ok && !containsStr(to, id) {
			to, view = append(to, id), append(view, "HR Admin")
		}
	}
	return to, view
}

// docWindowsForImport opens a window, dated today, for every employee an HR Admin import created who misses a
// mandatory document (all of them, unless Form setup asks for none of their type and department). In the import's
// transaction.
func docWindowsForImport(ctx context.Context, tx store.Q, created []bson.M) error {
	if len(created) == 0 || !loadDocSettings(ctx, tx).On {
		return nil
	}
	_, ds, err := loadSetup(ctx, tx)
	if err != nil {
		return err
	}
	depts := map[string]string{}
	docs := []any{}
	for _, e := range created {
		did := str(e["department_id"])
		if _, ok := depts[did]; !ok {
			d, _ := tx.C("department").FindOne(ctx, bson.M{"_id": did})
			depts[did] = str(d["name"])
		}
		mandatory := false
		for _, n := range docNeeds(ds, str(e["type"]), depts[did]) {
			mandatory = mandatory || n.Mandatory
		}
		if mandatory {
			docs = append(docs, bson.M{"employee_id": e["_id"], "start": store.Today(), "source": "import"})
		}
	}
	if len(docs) == 0 {
		return nil
	}
	return tx.C(store.DocWindowColl).InsertMany(ctx, docs)
}
