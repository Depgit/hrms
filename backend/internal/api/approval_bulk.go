package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/approval"
	"github.com/saltstayz/hrms/internal/store"
)

// Bulk approval for the Payroll view's Payroll approvals and Joining approvals tabs (Approvals.tsx PAY_GROUP and
// JOIN_GROUP). Payroll downloads every request of the tab waiting on them as Excel, writes Approve or Return (with a
// comment) against each, uploads it, sees a preview and confirms. Ticking rows on the screen and "Approve selected"
// uses the same apply step. Every decision goes through decideTx, the code behind the Approve and Reject buttons,
// one request per transaction, and counts only for the caller's own stage: the next stage still follows.
//
//	GET  /api/approvals/bulk/export?group=pay|join  the Excel file (roles that may download)
//	POST /api/approvals/bulk/preview?group=pay|join the filled file (multipart "file"): what would happen, nothing changes
//	POST /api/approvals/bulk/apply {group, source, items: [{id, decision, comment, stage}]}: decide them
//
// Nothing in the file is trusted: each row is checked again at preview and again at apply.

// bulkGroups are the request types of each tab. Tax proofs (p_tax) are reviewed on the tax screen, so they are not
// decided in bulk.
var bulkGroups = map[string][]string{
	"pay":  {"regularize", "leave", "p_claim", "p_adv", "p_ded", "p_arrear", "p_fnf", "p_change"},
	"join": {"joining", "verify"},
}

const (
	bulkMaxRows  = 1000
	bulkMaxBytes = 5 << 20
	bulkSheet    = "Approvals"
	bulkExported = "Exported values" // very hidden: the joining details as downloaded
)

func bulkGroupWFs(g string) ([]string, error) {
	wfs, ok := bulkGroups[g]
	if !ok {
		return nil, bad("Say which tab: Payroll approvals or Joining approvals")
	}
	return wfs, nil
}

var bulkTabName = map[string]string{"pay": "Payroll approvals", "join": "Joining approvals"}

// bulkWaiting lists the requests of the tab's types waiting on the caller (the inbox's "To approve"), oldest first,
// at most bulkMaxRows. The inbox's own list stops at 500 open requests; this one reads them all.
func (s *Server) bulkWaiting(ctx context.Context, c *Ctx, wfs []string) ([]*Request, []store.Row, bool, error) {
	q := s.st.DB
	filter := bson.M{"status": "Pending", "wf": bson.M{"$in": wfs}}
	if c.Role == "Employee" {
		filter["candidate_id"], filter["offer_id"] = nil, nil
	}
	rows, err := q.C("approval_request").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, false, err
	}
	reqs, kept := []*Request{}, []store.Row{}
	for _, row := range rows {
		req, err := loadRequest(ctx, q, str(row["id"]), false)
		if err != nil || !canAct(ctx, q, c, req) {
			continue
		}
		if len(reqs) == bulkMaxRows {
			return reqs, kept, true, nil
		}
		reqs, kept = append(reqs, req), append(kept, row)
	}
	return reqs, kept, false, nil
}

// bulkDay is a date as the screens write it: 11 Nov 2026.
func bulkDay(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.In(store.Loc()).Format("02 Jan 2006")
	case string:
		if d, err := time.Parse("2006-01-02", t); err == nil {
			return d.Format("02 Jan 2006")
		}
		return t
	}
	return ""
}

func bulkPlural(n float64, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%s %ss", strings.TrimSuffix(fmt.Sprintf("%.1f", n), ".0"), one)
}

// bulkSummary is what a request asks, in a line (the screen's title of the request).
func bulkSummary(req *Request, offer store.Row, verifyLine string) string {
	p, f := payloadOf(req), req.Facts
	switch req.WF {
	case "leave":
		days := f.Days
		if days == 0 {
			days = 1
		}
		return fmt.Sprintf("%s, %s · %s to %s", p.LType, bulkPlural(days, "day"), bulkDay(p.From), bulkDay(p.To))
	case "regularize":
		rt := p.RType
		if rt == "" {
			rt = "All Other"
		}
		return fmt.Sprintf("Regularize %s · %s · %s · %s to %s", bulkDay(p.Date), rt, or(p.Reason, "—"), or(p.In, "—"), or(p.Out, "—"))
	case "p_claim":
		s := fmt.Sprintf("%s claim, %s", p.ClaimType, rupees(p.Amount))
		if p.BillDate != "" {
			s += " · bill dated " + bulkDay(p.BillDate)
		}
		return s
	case "p_adv":
		s := "Advance " + rupees(p.Amount)
		if p.Instalments > 0 {
			s += fmt.Sprintf(" · %d instalments", p.Instalments)
		}
		return s
	case "p_ded":
		n := max(1, p.Instalments)
		return fmt.Sprintf("%s, %s · %s", or(p.DedType, "Deduction"), rupees(p.Amount), bulkPlural(float64(n), "instalment"))
	case "p_arrear":
		kind, amt := "Arrear", p.Amount
		if amt < 0 {
			kind, amt = "Recovery", -amt
		}
		s := kind + " " + rupees(amt)
		if isYm(p.ForYm) {
			s += " · for " + ymLabel(p.ForYm)
		}
		return s
	case "p_fnf":
		s := "Full and final, " + rupees(p.Amount)
		if p.LWD != "" {
			s += " · last working day " + bulkDay(p.LWD)
		}
		return s
	case "p_change":
		if p.Kind == "Bank" {
			return "Bank account change"
		}
		return "Personal details change"
	case "joining":
		if offer == nil {
			return "Offer approval"
		}
		pre := ""
		if offer["rejoin_of"] != nil {
			pre = "Rejoin · "
		}
		return fmt.Sprintf("%sOffer · %s · %s · %s · joining %s", pre, str(offer["designation"]), str(offer["city"]), str(offer["type"]), bulkDay(offer["doj"]))
	case "verify":
		return "Joining verification" + verifyLine
	}
	return workflowName(req.WF)
}

// bulkVerifyInfo is what the Excel shows of a joining verification: its details and its documents.
type bulkVerifyInfo struct {
	values map[string]string // field key → value as shown
	docs   string
	line   string // " · 18 of 20 mandatory items complete"
}

// bulkExport: GET /api/approvals/bulk/export?group=pay|join.
func (s *Server) bulkExport(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !c.CanDownload {
		writeErr(w, forbidden("Your role cannot download files"))
		return
	}
	if err := bulkNotMyView(c); err != nil {
		writeErr(w, err)
		return
	}
	group := r.URL.Query().Get("group")
	wfs, err := bulkGroupWFs(group)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx := r.Context()
	f, n, err := s.bulkBook(ctx, c, group, wfs)
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
	name := fmt.Sprintf("%s-%s.xlsx", strings.ReplaceAll(strings.ToLower(bulkTabName[group]), " ", "-"), time.Now().In(store.Loc()).Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"; filename*=UTF-8''`+url.PathEscape(name))
	_, _ = w.Write(buf.Bytes())
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Downloaded %d requests for bulk approval (%s)", n, bulkTabName[group]), "approval_bulk", "", nil, map[string]any{"group": group, "rows": n})
}

// bulkColumns are the file's columns before the joining details; bulkInputs come last.
var (
	bulkColumns = []string{"Request ID", "Request type", "Employee or joiner", "Employee ID", "Department", "Location", "Details", "Raised on", "Waiting on"}
	bulkInputs  = []string{"Decision", "Comment"}
)

// bulkBook builds the workbook: "Approvals" (one row per request waiting on the caller) and "How to fill".
func (s *Server) bulkBook(ctx context.Context, c *Ctx, group string, wfs []string) (*excelize.File, int, error) {
	q := s.st.DB
	reqs, rows, truncated, err := s.bulkWaiting(ctx, c, wfs)
	if err != nil {
		return nil, 0, err
	}
	// Names, department and location as on the list.
	subjIDs := []string{}
	for _, r := range reqs {
		subjIDs = append(subjIDs, r.EmployeeID)
	}
	subjects, err := approvalSubjectsOf(ctx, q, subjIDs)
	if err != nil {
		return nil, 0, err
	}
	if err := apprUIAddSubject(ctx, q, rows); err != nil {
		return nil, 0, err
	}
	// Offers behind offer approvals and joining verifications.
	offerIDs, verifyIDs := []string{}, []string{}
	for _, r := range reqs {
		switch r.WF {
		case "joining":
			offerIDs = append(offerIDs, r.EmployeeID)
		case "verify":
			verifyIDs = append(verifyIDs, r.ID)
		}
	}
	offers := map[string]store.Row{}
	if len(offerIDs)+len(verifyIDs) > 0 {
		l, err := onboardingOffers(ctx, q, bson.D{{Key: "$or", Value: bson.A{bson.M{"_id": bson.M{"$in": offerIDs}}, bson.M{"verify_request_id": bson.M{"$in": verifyIDs}}}}}, 0)
		if err != nil {
			return nil, 0, err
		}
		for _, o := range l {
			offers[str(o["id"])] = o
			if v := str(o["verify_id"]); v != "" {
				offers[v] = o
			}
		}
	}
	fs, ds, err := loadSetup(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	// Joining details: one column per field asked at joining for any of the joiners in the file, in setup order.
	verify := map[string]*bulkVerifyInfo{}
	fieldCols := []onbField{}
	if len(verifyIDs) > 0 {
		used := map[string]bool{}
		names := map[string]string{}
		for _, id := range verifyIDs {
			if o := offers[id]; o != nil {
				d := rowData(o)
				for _, k := range []string{"l1", "l2"} {
					if d[k] != "" {
						names[d[k]] = ""
					}
				}
			}
		}
		if len(names) > 0 {
			ids := []string{}
			for k := range names {
				ids = append(ids, k)
			}
			if es, err := q.C("employee").Find(ctx, bson.M{"_id": bson.M{"$in": ids}}); err == nil {
				for _, e := range es {
					names[str(e["id"])] = str(e["name"])
				}
			}
		}
		for _, id := range verifyIDs {
			o := offers[id]
			if o == nil {
				continue
			}
			d, typ := rowData(o), str(o["type"])
			info := &bulkVerifyInfo{values: map[string]string{}}
			for _, f := range verifyEditable(fs, typ) {
				if !f.applies(typ, d) {
					continue
				}
				used[f.Key] = true
				if v := d[f.Key]; v != "" {
					info.values[f.Key] = verifyShow(f.Key, v, names)
				}
			}
			kind, owner := offerDocOwner(o)
			list, done, total, err := verifyDocs(ctx, q, o, ds, kind, owner)
			if err != nil {
				return nil, 0, err
			}
			missing, optional := []string{}, 0
			for _, x := range list {
				if x.Mandatory && !x.Uploaded {
					missing = append(missing, x.Label)
				}
				if !x.Mandatory && x.Uploaded {
					optional++
				}
			}
			info.docs = fmt.Sprintf("%d of %d mandatory uploaded", done, total)
			if len(missing) > 0 {
				info.docs += ". Missing: " + strings.Join(missing, ", ")
			}
			if optional > 0 {
				info.docs += fmt.Sprintf(". %d other uploaded", optional)
			}
			docs, _ := docsOf(ctx, q, kind, owner)
			jd, jt := progress(fs, ds, o, d, docs, "join")
			info.line = fmt.Sprintf(" · %d of %d mandatory items complete", jd, jt)
			verify[id] = info
		}
		for _, f := range fs {
			if used[f.Key] {
				fieldCols = append(fieldCols, f)
			}
		}
	}

	cols := append([]string{}, bulkColumns...)
	for _, f := range fieldCols {
		cols = append(cols, f.Label)
	}
	if len(verifyIDs) > 0 {
		cols = append(cols, "Documents")
	}
	cols = append(cols, bulkInputs...)

	f := excelize.NewFile()
	_ = f.SetSheetName("Sheet1", bulkSheet)
	head, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}, Border: bulkBorder("9CA3AF")})
	inHead, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"FDE68A"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}, Border: bulkBorder("9CA3AF")})
	cell, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{Vertical: "top", WrapText: true}, NumFmt: 49, Border: bulkBorder("E5E7EB")})
	inCell, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{Vertical: "top", WrapText: true}, NumFmt: 49,
		Fill: excelize.Fill{Type: "pattern", Color: []string{"FFFBEB"}, Pattern: 1}, Border: bulkBorder("D1D5DB")})
	widths := map[string]float64{"Request ID": 13, "Request type": 20, "Employee or joiner": 24, "Employee ID": 12, "Department": 16, "Location": 24,
		"Details": 46, "Raised on": 13, "Waiting on": 16, "Documents": 40, "Decision": 13, "Comment": 36}
	for i, h := range cols {
		cn, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetCellValue(bulkSheet, cn+"1", h)
		wd, ok := widths[h]
		if !ok {
			wd = float64(max(14, min(30, len(h)+2)))
		}
		_ = f.SetColWidth(bulkSheet, cn, cn, wd)
	}
	last, _ := excelize.ColumnNumberToName(len(cols))
	firstIn, _ := excelize.ColumnNumberToName(len(cols) - 1)
	_ = f.SetCellStyle(bulkSheet, "A1", last+"1", head)
	_ = f.SetCellStyle(bulkSheet, firstIn+"1", last+"1", inHead)
	_ = f.SetRowHeight(bulkSheet, 1, 32)
	for i, req := range reqs {
		row := rows[i]
		name := ""
		if e := subjects[req.EmployeeID]; e != nil {
			name = str(e["name"])
		}
		var info *bulkVerifyInfo
		var offer store.Row
		switch req.WF {
		case "verify":
			info, offer = verify[req.ID], offers[req.ID]
		case "joining":
			offer = offers[req.EmployeeID]
		}
		line := ""
		if info != nil {
			line = info.line
		}
		vals := []any{req.ID, workflowName(req.WF), name, req.EmployeeID, str(row["department"]), str(row["location"]), bulkSummary(req, offer, line),
			bulkDay(row["created_at"]), req.Chain[req.Step]}
		for _, fc := range fieldCols {
			v := ""
			if info != nil {
				v = info.values[fc.Key]
			}
			vals = append(vals, v)
		}
		if len(verifyIDs) > 0 {
			d := ""
			if info != nil {
				d = info.docs
			}
			vals = append(vals, d)
		}
		vals = append(vals, "", "")
		_ = f.SetSheetRow(bulkSheet, fmt.Sprintf("A%d", i+2), &vals)
	}
	end := max(2, len(reqs)+1)
	_ = f.SetCellStyle(bulkSheet, "A2", fmt.Sprintf("%s%d", last, end), cell)
	_ = f.SetCellStyle(bulkSheet, fmt.Sprintf("%s2", firstIn), fmt.Sprintf("%s%d", last, end), inCell)
	if len(fieldCols) > 0 { // joining details: editable, in blue
		a, _ := excelize.ColumnNumberToName(len(bulkColumns) + 1)
		z, _ := excelize.ColumnNumberToName(len(bulkColumns) + len(fieldCols))
		edHead, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"BFDBFE"}, Pattern: 1},
			Alignment: &excelize.Alignment{Vertical: "center", WrapText: true}, Border: bulkBorder("9CA3AF")})
		edCell, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{Vertical: "top", WrapText: true}, NumFmt: 49,
			Fill: excelize.Fill{Type: "pattern", Color: []string{"EFF6FF"}, Pattern: 1}, Border: bulkBorder("D1D5DB")})
		_ = f.SetCellStyle(bulkSheet, a+"1", z+"1", edHead)
		for i, req := range reqs {
			if req.WF == "verify" {
				_ = f.SetCellStyle(bulkSheet, fmt.Sprintf("%s%d", a, i+2), fmt.Sprintf("%s%d", z, i+2), edCell)
			}
		}
		// What was exported, to tell a correction from an unchanged (or masked) cell on upload.
		_, _ = f.NewSheet(bulkExported)
		keys, labels := []any{"Request ID"}, []any{"Column"}
		for _, fc := range fieldCols {
			keys, labels = append(keys, fc.Key), append(labels, fc.Label)
		}
		_ = f.SetSheetRow(bulkExported, "A1", &keys)
		_ = f.SetSheetRow(bulkExported, "A2", &labels)
		n := 3
		for _, req := range reqs {
			info := verify[req.ID]
			if req.WF != "verify" || info == nil {
				continue
			}
			vals := []any{req.ID}
			for _, fc := range fieldCols {
				vals = append(vals, info.values[fc.Key])
			}
			_ = f.SetSheetRow(bulkExported, fmt.Sprintf("A%d", n), &vals)
			n++
		}
		_ = f.SetSheetVisible(bulkExported, false, true)
	}
	dv := excelize.NewDataValidation(true)
	dv.Sqref = fmt.Sprintf("%s2:%s%d", firstIn, firstIn, end)
	_ = dv.SetDropList([]string{"Approve", "Return"})
	dv.SetError(excelize.DataValidationErrorStyleStop, "Approve or Return", "Choose Approve or Return, or leave it blank to decide later.")
	dv.SetInput("Decision", "Approve or Return. Blank: decide later. Return needs a comment.")
	_ = f.AddDataValidation(bulkSheet, dv)
	_ = f.SetPanes(bulkSheet, &excelize.Panes{Freeze: true, XSplit: 3, YSplit: 1, TopLeftCell: "D2", ActivePane: "bottomRight",
		Selection: []excelize.Selection{{SQRef: "D2", ActiveCell: "D2", Pane: "bottomRight"}}})
	_ = f.AutoFilter(bulkSheet, fmt.Sprintf("A1:%s%d", last, end), nil)

	how := "How to fill"
	_, _ = f.NewSheet(how)
	bulkHowTo(f, how, group, len(reqs), truncated)
	f.SetActiveSheet(0)
	return f, len(reqs), nil
}

func bulkBorder(color string) []excelize.Border {
	out := []excelize.Border{}
	for _, t := range []string{"left", "right", "top", "bottom"} {
		out = append(out, excelize.Border{Type: t, Color: color, Style: 1})
	}
	return out
}

func bulkHowTo(f *excelize.File, sheet, group string, n int, truncated bool) {
	title, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
	bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	wrap, _ := f.NewStyle(&excelize.Style{Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	tab := bulkTabName[group]
	lines := [][2]string{
		{"t", "Bulk approval: " + tab},
		{"", fmt.Sprintf("This file lists the %d requests on the %s tab that were waiting on you when you downloaded it (%s).", n, tab, time.Now().In(store.Loc()).Format("02 Jan 2006, 3:04 pm"))},
		{"", ""},
		{"b", "How to fill"},
		{"", "1. On the Approvals sheet, check each row. For a joining verification the joining details and the documents are on the same row; open the request in the app to see the files."},
		{"", "2. In Decision, pick Approve or Return. Leave it blank to decide later: that row is skipped."},
		{"", "3. Return needs a comment saying what to fix. A comment with Approve is optional and is saved with the approval."},
		{"", "4. Do not change Request ID or Waiting on. The grey columns are only for reading; changes there are ignored."},
		{"", ""},
		{"b", "Correcting joining details (blue columns, joining verifications only)"},
		{"", "• Type the right value over a wrong one. Any blue cell that differs from what was downloaded is a correction. Cells you leave as they are change nothing."},
		{"", "• Aadhaar and account numbers are shown masked (only the last four digits). A masked cell left as it is changes nothing. To change one, type the full new number."},
		{"", "• A manager is shown as “Name (EMP-1009)”: to change it, type the new manager's Employee ID, or “Name (EMP-…)”. Dates are YYYY-MM-DD. A choice must be one of its listed values."},
		{"", "• Corrections are checked like Edit details in the app (Aadhaar 12 digits, PAN ABCDE1234F, IFSC, 10-digit mobile, past date of birth, mandatory details filled). A row with a correction that is not valid is skipped as a whole, with the reason; fix it and upload again."},
		{"", "• Corrections are saved before the row's decision, with the request's history, the audit log and the employee record updated as Edit details does. A row with corrections and no decision only saves the corrections (the request must be waiting on you)."},
		{"", "5. Save the file and upload it in Approvals › " + tab + " › Upload decisions. You first see a preview of what will happen. Nothing changes until you confirm."},
		{"", ""},
		{"b", "What happens"},
		{"", "• Approve counts only for your own stage. If another stage follows (for example the Payroll Manager), the request moves on to it, exactly as with the Approve button."},
		{"", "• Return sends the request back with your comment, exactly as with the Reject button."},
		{"", "• Each row is checked again when you upload and again when you confirm. A row is skipped when the decision is blank, the request is no longer waiting on you, it was already decided after you downloaded the file, the Request ID is unknown, it is not a request of this tab, or Return has no comment."},
		{"", "• Each request is decided on its own: one that fails does not stop the others. You see how many were approved, returned and skipped, and why."},
		{"", fmt.Sprintf("• Up to %d rows and 5 MB per file. Upload this Excel file (.xlsx).", bulkMaxRows)},
	}
	if truncated {
		lines = append(lines, [2]string{"", ""}, [2]string{"b", fmt.Sprintf("More than %d requests are waiting on you: this file has the oldest %d. Download again after uploading it for the rest.", bulkMaxRows, bulkMaxRows)})
	}
	_ = f.SetColWidth(sheet, "A", "A", 120)
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

// ---------- Preview and apply ----------

// bulkLine is one row of the file (or one ticked request) and what happens to it.
type bulkLine struct {
	Line     int          `json:"line,omitempty"` // row in the Excel file
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Decision string       `json:"decision"` // approve | return | "" (as given)
	Comment  string       `json:"comment"`
	Stage    string       `json:"stage"`             // the stage the decision is for (Waiting on, as downloaded)
	Changes  []bulkChange `json:"changes,omitempty"` // corrections of joining details, masked
	Outcome  string       `json:"outcome"`           // preview: approve | return | correct | skip; apply: approved | returned | corrected | skipped | failed
	Reason   string       `json:"reason,omitempty"`
	Next     string       `json:"next,omitempty"` // after an approval: the next stage, or "Fully approved"

	cells map[string]string // joining detail columns of the row: field key → cell
	shown map[string]string // the same details as downloaded (nil when the file has no record of them)
	data  map[string]string // the corrections: field key → new value
}

// bulkDecision reads a decision cell: approve, return, "" for blank, or "?" for anything else.
func bulkDecision(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return ""
	case "approve", "approved", "yes":
		return "approve"
	case "return", "returned", "reject", "rejected", "send back":
		return "return"
	}
	return "?"
}

var bulkEmpRef = regexp.MustCompile(`\(\s*([A-Za-z0-9-]+)\s*\)\s*$`)

// bulkCorrections are the joining detail cells of a row that differ from what was downloaded (or, when the file has
// no record of that, from what is stored now): field key → the new value. A masked number left as it is changes
// nothing; a masked cell that was edited is refused (type the full number).
func bulkCorrections(ctx context.Context, q store.Q, req *Request, ln *bulkLine, labels map[string]string) (map[string]string, string) {
	if len(ln.cells) == 0 {
		return nil, ""
	}
	if req.WF != "verify" {
		for _, v := range ln.cells {
			if v != "" {
				return nil, "Details can be corrected only on a joining verification"
			}
		}
		return nil, ""
	}
	shown := ln.shown
	if shown == nil {
		shown = map[string]string{}
		if o, _ := q.C("offer").FindOne(ctx, bson.M{"verify_request_id": req.ID}); o != nil {
			d := rowData(o)
			names := map[string]string{}
			for _, k := range []string{"l1", "l2"} {
				if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": d[k]}); d[k] != "" && e != nil {
					names[d[k]] = str(e["name"])
				}
			}
			for k, v := range d {
				if v != "" {
					shown[k] = verifyShow(k, v, names)
				}
			}
		}
	}
	out := map[string]string{}
	for k, cell := range ln.cells {
		if cell == shown[k] {
			continue
		}
		if verifyMaskKeys[k] && strings.Contains(cell, "•") {
			return nil, "Type the full new " + or(labels[k], k) + ", not the masked one"
		}
		v := cell
		switch k {
		case "l1", "l2":
			if m := bulkEmpRef.FindStringSubmatch(v); m != nil {
				v = m[1]
			}
			v = strings.ToUpper(v)
		case "aadhaar", "acct":
			v = strings.ReplaceAll(v, " ", "")
		}
		out[k] = v
	}
	return out, ""
}

// bulkJudge decides what happens to one line now: approve, return, correct or skip with the reason. seen catches a
// request listed twice. It reads the request as it is now; apply repeats this check, and decideTx checks once more.
// bulkMaySee: the caller may open the request (as GET /api/approvals/{id} allows): it waits on them, is about them
// or raised by them, or is in their approvals scope.
func bulkMaySee(ctx context.Context, q store.Q, c *Ctx, r *Request) bool {
	if r.EmployeeID == c.EmployeeID || r.RaisedBy == c.EmployeeID || canAct(ctx, q, c, r) {
		return true
	}
	if c.Level("appr.queue") < 1 {
		return false
	}
	ok, _ := c.InScope(ctx, q, r.EmployeeID)
	return ok
}

// bulkNotMyView refuses bulk approval in My view: an employee decides nothing in bulk (AUTH-080).
func bulkNotMyView(c *Ctx) error {
	if c.Role == "Employee" {
		return forbidden("Bulk approval is for approver roles")
	}
	return nil
}

func (s *Server) bulkJudge(ctx context.Context, q store.Q, c *Ctx, wfs []string, ln *bulkLine, seen map[string]bool, labels map[string]string) {
	skip := func(why string) { ln.Outcome, ln.Reason, ln.Changes, ln.data = "skip", why, nil, nil }
	ln.ID = strings.TrimSpace(ln.ID)
	ln.Comment = strings.TrimSpace(ln.Comment)
	ln.Stage = strings.TrimSpace(ln.Stage)
	dec := bulkDecision(ln.Decision)
	var req *Request
	if ln.ID != "" {
		// A request the caller may not open reads as unknown: no name, type or stage of someone else's request
		// (AUTH-083).
		if r, err := loadRequest(ctx, q, ln.ID, false); err == nil && bulkMaySee(ctx, q, c, r) {
			req = r
			ln.Type = workflowName(r.WF)
			if e, _ := approvalSubjectOf(ctx, q, r.EmployeeID); e != nil {
				ln.Name = str(e["name"])
			}
		}
	}
	switch {
	case dec == "?":
		skip("Decision must be Approve or Return")
		return
	case ln.ID == "":
		skip(map[bool]string{true: "No decision", false: "No Request ID"}[dec == ""])
		return
	case seen[ln.ID]:
		skip("Listed twice: only the first row counts")
		return
	}
	seen[ln.ID] = true
	ln.Decision = dec
	if req == nil {
		skip(map[bool]string{true: "No decision", false: "Unknown Request ID"}[dec == ""])
		return
	}
	if !slices.Contains(wfs, req.WF) {
		skip("Not a request of this tab (" + workflowName(req.WF) + ")")
		return
	}
	corr, problem := bulkCorrections(ctx, q, req, ln, labels)
	if problem != "" {
		skip(problem)
		return
	}
	if dec == "" && len(corr) == 0 {
		skip("No decision")
		return
	}
	if req.Status != "Pending" {
		skip("Already " + strings.ToLower(req.Status) + " since the download")
		return
	}
	if !canAct(ctx, q, c, req) {
		switch {
		case req.EmployeeID == c.EmployeeID || req.RaisedBy == c.EmployeeID:
			skip("You cannot decide your own request")
		case req.Step < len(req.Chain):
			skip("Not waiting on you any more: now with " + req.Chain[req.Step])
		default:
			skip("Not waiting on you any more")
		}
		return
	}
	stage := req.Chain[req.Step]
	if ln.Stage != "" && !strings.EqualFold(ln.Stage, stage) {
		skip("Moved on since the download: now at " + stage + ". Download again to decide it")
		return
	}
	ln.Stage = stage
	if dec == "return" && ln.Comment == "" {
		skip("Return needs a comment")
		return
	}
	if dec == "approve" && c.Role == "HR Admin" && approval.StageRole(stage) != "HR Admin" && ln.Comment == "" {
		skip("Add a comment: you are approving on behalf of " + stage)
		return
	}
	if len(corr) > 0 {
		vc, err := s.verifyLoad(ctx, q, c, req.ID, false, true)
		if err != nil {
			skip(bulkErrText(err))
			return
		}
		p, err := verifyPlan(ctx, q, vc, corr)
		if err != nil {
			skip("Correction not valid: " + strings.Join(bulkErrList(err), "; "))
			return
		}
		if len(p.keys) == 0 && dec == "" {
			skip("No decision")
			return
		}
		ln.Changes, ln.data = verifyChanges(ctx, q, p), corr
	}
	ln.Outcome = dec
	if dec == "" {
		ln.Outcome = "correct"
	}
}

// bulkFieldLabels maps the labels of the joining fields to their keys (the detail columns' headings), and back.
func bulkFieldLabels(fs []onbField) (map[string]string, map[string]string) {
	byLabel, byKey := map[string]string{}, map[string]string{}
	for _, f := range fs {
		byLabel[strings.ToLower(strings.TrimSpace(f.Label))] = f.Key
		byKey[f.Key] = f.Label
	}
	return byLabel, byKey
}

// bulkReadFile reads the uploaded Excel file: the sheet with Request ID and Decision columns, one line per filled row,
// with the joining detail columns and, from the hidden "Exported values" sheet, those details as downloaded.
func bulkReadFile(r *http.Request, fs []onbField) ([]bulkLine, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, bulkMaxBytes+64<<10)
	if err := r.ParseMultipartForm(bulkMaxBytes); err != nil {
		return nil, bad("The file must be 5 MB or smaller")
	}
	file, h, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose the Excel file")
	}
	defer file.Close()
	if h.Size > bulkMaxBytes {
		return nil, bad("The file must be 5 MB or smaller")
	}
	b, err := io.ReadAll(io.LimitReader(file, bulkMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > bulkMaxBytes {
		return nil, bad("The file must be 5 MB or smaller")
	}
	if !bytes.HasPrefix(b, []byte("PK")) {
		return nil, bad("Upload the Excel file (.xlsx) from Download for bulk approval")
	}
	x, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		return nil, bad("That Excel file could not be read")
	}
	defer x.Close()
	names := x.GetSheetList()
	byLabel, _ := bulkFieldLabels(fs)
	// The details as downloaded: row 1 the keys, row 2 the column headings, then one row per request.
	var shown map[string]map[string]string
	if slices.Contains(names, bulkExported) {
		if rows, err := x.GetRows(bulkExported); err == nil && len(rows) >= 2 {
			shown = map[string]map[string]string{}
			keys := rows[0]
			for i, l := range rows[1] {
				if i > 0 && i < len(keys) && strings.TrimSpace(keys[i]) != "" {
					byLabel[strings.ToLower(strings.TrimSpace(l))] = strings.TrimSpace(keys[i])
				}
			}
			for _, rec := range rows[2:] {
				if len(rec) == 0 || strings.TrimSpace(rec[0]) == "" {
					continue
				}
				m := map[string]string{}
				for i := 1; i < len(keys); i++ {
					v := ""
					if i < len(rec) {
						v = strings.TrimSpace(rec[i])
					}
					m[strings.TrimSpace(keys[i])] = v
				}
				shown[strings.TrimSpace(rec[0])] = m
			}
		}
	}
	if i := slices.Index(names, bulkSheet); i > 0 { // the Approvals sheet first
		names = append([]string{bulkSheet}, append(names[:i:i], names[i+1:]...)...)
	}
	for _, n := range names {
		if n == bulkExported {
			continue
		}
		rows, err := x.GetRows(n)
		if err != nil || len(rows) == 0 {
			continue
		}
		col := map[string]int{}
		detail := map[int]string{} // column → field key
		for i, h := range rows[0] {
			hl := strings.ToLower(strings.TrimSpace(h))
			col[hl] = i
			if i >= len(bulkColumns) && byLabel[hl] != "" {
				detail[i] = byLabel[hl]
			}
		}
		_, ok1 := col["request id"]
		_, ok2 := col["decision"]
		if !ok1 || !ok2 {
			continue
		}
		get := func(rec []string, k string) string {
			if i, ok := col[k]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		out := []bulkLine{}
		for i, rec := range rows[1:] {
			if strings.TrimSpace(strings.Join(rec, "")) == "" {
				continue
			}
			if len(out) == bulkMaxRows {
				return nil, bad("Up to %d rows at a time. Split the file and upload the rest after", bulkMaxRows)
			}
			ln := bulkLine{Line: i + 2, ID: get(rec, "request id"), Decision: get(rec, "decision"), Comment: get(rec, "comment"), Stage: get(rec, "waiting on")}
			if len(detail) > 0 {
				ln.cells = map[string]string{}
				for ci, k := range detail {
					v := ""
					if ci < len(rec) {
						v = strings.TrimSpace(rec[ci])
					}
					ln.cells[k] = v
				}
				if shown != nil {
					ln.shown = shown[ln.ID]
					if ln.shown == nil {
						ln.shown = map[string]string{}
					}
				}
			}
			out = append(out, ln)
		}
		return out, nil
	}
	return nil, bad("No sheet with the columns Request ID and Decision. Download the file again from Download for bulk approval")
}

// bulkCounts counts outcomes, and the rows whose corrections are (to be) saved.
func bulkCounts(lines []bulkLine) map[string]int {
	out := map[string]int{"approve": 0, "return": 0, "correct": 0, "skip": 0, "approved": 0, "returned": 0, "corrected": 0, "skipped": 0, "failed": 0, "corrections": 0}
	for _, l := range lines {
		out[l.Outcome]++
		if len(l.Changes) > 0 && l.Outcome != "skip" && l.Outcome != "skipped" && l.Outcome != "failed" {
			out["corrections"]++
		}
	}
	return out
}

// bulkPreview: POST /api/approvals/bulk/preview?group=… with the filled file. Nothing changes.
func (s *Server) bulkPreview(r *http.Request, c *Ctx) (any, error) {
	if err := bulkNotMyView(c); err != nil {
		return nil, err
	}
	group := r.URL.Query().Get("group")
	wfs, err := bulkGroupWFs(group)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	fs, _, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	lines, err := bulkReadFile(r, fs)
	if err != nil {
		return nil, err
	}
	_, labels := bulkFieldLabels(fs)
	seen := map[string]bool{}
	for i := range lines {
		s.bulkJudge(ctx, s.st.DB, c, wfs, &lines[i], seen, labels)
	}
	n := bulkCounts(lines)
	return map[string]any{"rows": lines, "approve": n["approve"], "return": n["return"], "correct": n["correct"], "skip": n["skip"],
		"corrections": n["corrections"], "total": len(lines)}, nil
}

type bulkApplyBody struct {
	Group  string `json:"group"`
	Source string `json:"source"` // select (ticked on screen) | excel
	Items  []struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
		Stage    string `json:"stage"`
		Line     int    `json:"line"`
	} `json:"items"`
}

// bulkApply: POST /api/approvals/bulk/apply {group, source, items} decides the ticked requests (no corrections).
func (s *Server) bulkApply(r *http.Request, c *Ctx) (any, error) {
	if err := bulkNotMyView(c); err != nil {
		return nil, err
	}
	var b bulkApplyBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if _, err := bulkGroupWFs(b.Group); err != nil {
		return nil, err
	}
	if b.Source != "excel" {
		b.Source = "select"
	}
	if len(b.Items) == 0 {
		return nil, bad("Nothing to apply")
	}
	if len(b.Items) > bulkMaxRows {
		return nil, bad("Up to %d requests at a time", bulkMaxRows)
	}
	lines := make([]bulkLine, len(b.Items))
	for i, it := range b.Items {
		lines[i] = bulkLine{Line: it.Line, ID: it.ID, Decision: it.Decision, Comment: it.Comment, Stage: it.Stage}
	}
	return s.bulkDecideRun(r.Context(), c, b.Group, b.Source, lines, nil)
}

// bulkApplyFile: POST /api/approvals/bulk/apply-file?group=… with the same filled file as the preview: corrections
// and decisions.
func (s *Server) bulkApplyFile(r *http.Request, c *Ctx) (any, error) {
	if err := bulkNotMyView(c); err != nil {
		return nil, err
	}
	group := r.URL.Query().Get("group")
	if _, err := bulkGroupWFs(group); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fs, _, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	lines, err := bulkReadFile(r, fs)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, bad("The file has no rows")
	}
	_, labels := bulkFieldLabels(fs)
	return s.bulkDecideRun(ctx, c, group, "excel", lines, labels)
}

// bulkDecideRun judges each line again, then saves its corrections and decides it in one transaction of its own:
// verifyApply (the Edit details code) first, then decideTx (the Approve and Reject buttons' code), after checking the
// request is still at the stage it was decided for. A failure is reported and the next line goes on.
func (s *Server) bulkDecideRun(ctx context.Context, c *Ctx, group, source string, lines []bulkLine, labels map[string]string) (any, error) {
	wfs, _ := bulkGroupWFs(group)
	seen := map[string]bool{}
	for i := range lines {
		ln := &lines[i]
		s.bulkJudge(ctx, s.st.DB, c, wfs, ln, seen, labels)
		if ln.Outcome == "skip" {
			ln.Outcome = "skipped"
			continue
		}
		action, stage, corr := map[string]string{"approve": "approve", "return": "reject"}[ln.Outcome], ln.Stage, ln.data
		var out map[string]any
		err := s.st.Tx(ctx, func(tx store.Q) error {
			out = nil
			req, err := loadRequest(ctx, tx, ln.ID, true)
			if err != nil {
				return err
			}
			if req.Status != "Pending" || req.Step >= len(req.Chain) || req.Chain[req.Step] != stage {
				return bad("Decided by someone else just now")
			}
			if len(corr) > 0 {
				vc, err := s.verifyLoad(ctx, tx, c, ln.ID, true, true)
				if err != nil {
					return err
				}
				p, err := verifyPlan(ctx, tx, vc, corr)
				if err != nil {
					return err
				}
				if _, err := s.verifyApply(ctx, tx, c, vc, p, " (bulk approval file)"); err != nil {
					return err
				}
			}
			if action == "" {
				return nil
			}
			out, err = s.decideTx(ctx, tx, c, ln.ID, action, ln.Comment)
			return err
		})
		switch {
		case err != nil:
			ln.Outcome, ln.Reason = "failed", strings.Join(bulkErrList(err), "; ")
		case action == "":
			ln.Outcome = "corrected"
		case action == "reject":
			ln.Outcome = "returned"
		default:
			ln.Outcome = "approved"
			if out["status"] == "Approved" {
				ln.Next = "Fully approved"
			} else {
				ln.Next = str(out["next"])
			}
		}
	}
	n := bulkCounts(lines)
	ids := map[string][]string{}
	for _, l := range lines {
		if l.Outcome != "skipped" {
			ids[l.Outcome] = append(ids[l.Outcome], l.ID)
		}
		if len(l.Changes) > 0 && l.Outcome != "skipped" && l.Outcome != "failed" {
			ids["corrections"] = append(ids["corrections"], l.ID)
		}
	}
	how := map[string]string{"excel": "from Excel", "select": "selected on screen"}[source]
	_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Bulk approval %s (%s): %d approved, %d returned, %d with corrections, %d skipped, %d failed", how, bulkTabName[group],
		n["approved"], n["returned"], n["corrections"], n["skipped"], n["failed"]), "approval_bulk", "", nil,
		map[string]any{"group": group, "source": source, "approved": n["approved"], "returned": n["returned"], "corrected": n["corrected"], "corrections": n["corrections"],
			"skipped": n["skipped"], "failed": n["failed"], "ids": ids})
	return map[string]any{"rows": lines, "approved": n["approved"], "returned": n["returned"], "corrected": n["corrected"], "corrections": n["corrections"],
		"skipped": n["skipped"], "failed": n["failed"]}, nil
}

// bulkErrList is the messages of a refused step (a validation error lists every problem).
func bulkErrList(err error) []string {
	var ae *apiError
	if errors.As(err, &ae) {
		if l, ok := ae.Details.([]string); ok && len(l) > 0 {
			return l
		}
		return []string{ae.Message}
	}
	return []string{bulkErrText(err)}
}

// bulkErrText is the message of a refused decision.
func bulkErrText(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Message
	}
	if code, _ := store.ErrCode(err); code != "" {
		return "A value is not valid (" + code + ")"
	}
	return "Something went wrong"
}
