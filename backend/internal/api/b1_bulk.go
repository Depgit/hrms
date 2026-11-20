package api

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Bulk update additions of 14 Nov 2026 (owner's requests):
//
//   - Employee master update gets Role, Skill (job role and skill masters), Medical (the monthly medical insurance
//     amount, b1_employee.go) and Accommodation (the deduction Salary breakup sets). All optional columns: an older
//     file without them is still read; blank keeps the current value.
//   - Personal details gets 1st Child and 2nd Child (after Father or Spouse Name) and Existing UAN?.
//   - Opening optional holiday balances: merged into Opening leave balances on 17 Nov 2026 (column Optional Holiday
//     Balance, bulkOpening in bulk.go).
//   - ESIC and medical cards (cards): a ZIP of card files named by Employee ID and card.

func init() {
	for _, t := range bulkTemplates {
		switch t.Key {
		case "upd":
			t.Cols = insertBefore(t.Cols, "Reason for change", "Role", "Skill", "Medical", "Accommodation")
			t.Optional = append(t.Optional, "Role", "Skill", "Medical", "Accommodation")
			t.lists["Role"], t.lists["Skill"] = bulkMaster(store.KindJobRole, ""), bulkMaster(store.KindSkill, "")
			t.Hint += " Role and Skill must be active masters; an employee who has none yet needs both when the location, department, designation or grade changes. Medical is the monthly medical insurance (₹) the company pays for the employee, replacing the salary structure's rate (Standard goes back to the structure's rule); Accommodation is the monthly accommodation deduction for permanent staff, as in Salary breakup. Medical and Accommodation need the salary right."
			t.row = bulkUpdB1
			attCodeWire(t, false) // realtime_bulk.go
		case "pers":
			t.Cols = insertAfter(t.Cols, "Father or Spouse Name", "Spouse Name", "1st Child", "2nd Child")
			t.Cols = append(t.Cols, "UAN", "Existing UAN?")
			t.Optional = append(t.Optional, "Spouse Name", "1st Child", "2nd Child", "UAN", "Existing UAN?")
			t.lists["Existing UAN?"] = bulkFixed("Yes", "No")
			t.Hint += " Spouse Name, 1st Child and 2nd Child are the family the medical insurance covers. UAN is the 12-digit PF number. Existing UAN? is Yes when the employee already has a UAN (on the record or in the UAN column). UAN and Existing UAN? need the government ID right."
		}
	}
	bulkTemplates = append(bulkTemplates,
		&bulkTemplate{Key: "cards", Name: "ESIC and medical cards", Zip: true, Cols: []string{"Employee ID", "Card (ESIC or MEDICAL)", "File (PDF, JPG or PNG)"},
			Hint: "Upload one ZIP file with the cards, each file named by the Employee ID and the card: EMP-1007_ESIC.pdf, EMP-1007_MEDICAL.pdf (EMP-1007-esic.jpg works too). PDF, JPG or PNG, each 5 MB at most, the ZIP 100 MB at most. Check lists the matched, unmatched and invalid files; Apply stores each matched file on the employee's record as an ESIC card or Medical card. A new card replaces the current one, which stays in the document history.",
			// Whoever can upload employee documents (canFiles: employee details at Edit with the government ID right).
			allow: func(c *Ctx) bool { return c.Level("emp.directory") >= 2 && c.Level("emp.govid") >= 1 },
			row: func(*Server, context.Context, store.Q, *Ctx, bulkRow, bool) (bulkResult, error) {
				return bulkResult{}, bad("Upload the cards as a ZIP file")
			}},
	)
}

// insertBefore puts cols just before the column named at (at the end when it is not there).
func insertBefore(l []string, at string, cols ...string) []string {
	for i, x := range l {
		if x == at {
			return append(append(append([]string{}, l[:i]...), cols...), l[i:]...)
		}
	}
	return append(append([]string{}, l...), cols...)
}

// insertAfter puts cols just after the column named at (at the end when it is not there).
func insertAfter(l []string, at string, cols ...string) []string {
	for i, x := range l {
		if x == at {
			return append(append(append([]string{}, l[:i+1]...), cols...), l[i+1:]...)
		}
	}
	return append(append([]string{}, l...), cols...)
}

// bulkUpdB1 is the Employee master update with Role, Skill, Medical and Accommodation: those columns are checked
// first (a row with a problem writes nothing), then the rest of the row as before (bulkUpd), then they are written
// with the row's Reason for change and Effective From. Role and Skill change like the other job fields (scheduled
// for a future date); the pay columns apply at once and are recorded with the date.
func bulkUpdB1(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	if res, refused := bulkUpdRoleSkill(ctx, tx, c, r); refused { // a job change needs Role and Skill (hirefix.go)
		return res, nil
	}
	role, skill := strings.TrimSpace(r.V["Role"]), strings.TrimSpace(r.V["Skill"])
	if role == "" && skill == "" && strings.TrimSpace(r.V["Medical"]) == "" && strings.TrimSpace(r.V["Accommodation"]) == "" {
		return bulkUpd(s, ctx, tx, c, r, apply)
	}
	var pre bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &pre)
	if !ok || e["status"] != "Active" {
		return bulkUpd(s, ctx, tx, c, r, apply) // says why, as before
	}
	cur, _ := tx.C("employee").FindOne(ctx, bson.M{"_id": pre.ID})
	job := map[string]string{}
	for _, x := range []struct{ col, key, kind, field string }{{"Role", "jobRole", store.KindJobRole, "job_role"}, {"Skill", "skill", store.KindSkill, "skill"}} {
		v := strings.TrimSpace(r.V[x.col])
		if v == "" {
			continue
		}
		_, name, ok := jobMasterLookup(x.kind)(ctx, tx, v)
		if !ok {
			pre.Errors = append(pre.Errors, fmt.Sprintf("%s “%s” is not an active %s", x.col, v, strings.ToLower(x.col)))
			continue
		}
		if name != str(cur[x.field]) {
			job[x.key] = name
		}
	}
	pay := bulkUpdPayRead(ctx, tx, c, r, e, &pre)
	if len(pre.Errors) > 0 {
		return pre, nil
	}
	mine := len(job) > 0 || pay.any()
	if mine && (r.V["Reason for change"] == "" || r.V["Effective From"] == "") {
		pre.Errors = append(pre.Errors, "Reason for change and Effective From are needed for a change")
		return pre, nil
	}
	var reason, eff string
	if mine {
		var err error
		if reason, eff, err = updReasonEff(ctx, tx, r, fmt.Sprint(e["doj"])); err != nil {
			return pre, err
		}
	}
	res, err := bulkUpd(s, ctx, tx, c, r, apply)
	if err != nil || len(res.Errors) > 0 || !mine {
		return res, err
	}
	if len(job) > 0 {
		changes, err := ApplyJobChange(ctx, tx, c, res.ID, JobChange{Changes: job, Reason: reason, EffectiveFrom: eff}, "Bulk update")
		if err != nil {
			return res, err
		}
		for _, x := range changes {
			res.Changes = append(res.Changes, bulkChange{x.Field, x.From, x.To, dateNote(eff)})
		}
	}
	if err := bulkUpdPayApply(ctx, tx, c, res.ID, str(e["type"]), pay, reason, eff, &res); err != nil {
		return res, err
	}
	if len(res.Changes) > 0 {
		res.Eff, res.Reason = eff, reason
	}
	return res, nil
}

// ---------- Opening optional holiday balances ----------

// Optional holidays are not a leave type: the work policy says how many a year each employee may choose from the
// holiday calendar (settings.optHol), and they are taken as leave. So the opening balance is its own small
// collection (opt_hol_opening, employee and As at date), loaded the way Opening leave balances loads leave_opening:
// the same employee and date replaces, a later date takes over. It holds what is left of the calendar year of the
// As at date; the next calendar year starts again at the policy's number.

// optHolOpeningOf is the latest opening optional holiday balance on or before the date: {at, balance, left}, left
// being the balance while the date is in the same calendar year (nil after it). nil when none was loaded.
func optHolOpeningOf(ctx context.Context, q store.Q, empID, on string) map[string]any {
	r, err := q.C(store.OptHolOpeningColl).FindOne(ctx, bson.M{"employee_id": empID, "at": bson.M{"$lte": on}},
		options.FindOne().SetSort(bson.D{{Key: "at", Value: -1}}))
	if err != nil || r == nil {
		return nil
	}
	at, bal := str(r["at"]), timectxNum(r["balance"])
	out := map[string]any{"at": at, "balance": bal, "left": nil}
	if len(on) >= 4 && len(at) >= 4 && at[:4] == on[:4] {
		out["left"] = bal
	}
	return out
}

// ---------- ESIC and medical cards: a ZIP of files named by Employee ID ----------

const (
	cardsZipMax     = 100 << 20 // the ZIP
	cardsFilesMax   = 2000      // entries in it
	cardsTotalMax   = 600 << 20 // all files unpacked
	cardsRatioMax   = 200       // unpacked ÷ packed, per file: more is treated as a zip bomb
	cardsNameMaxLen = 180
)

var reCardName = regexp.MustCompile(`(?i)^(.+)[_-](esic|medical)\.(pdf|jpe?g|png)$`)

// cardKinds: the suffix → the document key and its label.
var cardKinds = map[string][2]string{"ESIC": {store.DocESICCard, "ESIC card"}, "MEDICAL": {store.DocMedicalCard, "Medical card"}}

type cardFile struct {
	name     string // as in the ZIP
	empID    string
	kind     string // ESIC or MEDICAL
	file     *upload
	problem  string
	unknown  bool // no such employee, or outside the caller's access
	empName  string
	replaces string
}

// cardsRead opens the uploaded ZIP and checks every file in it: a safe name, a known employee in scope, the card
// suffix, the type and size. It never writes a file to disk.
func (s *Server) cardsRead(r *http.Request, c *Ctx) ([]*cardFile, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, cardsZipMax+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			return nil, bad("The ZIP must be 100 MB or smaller")
		}
		return nil, bad("Choose a ZIP file")
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose a ZIP file")
	}
	defer f.Close()
	if hdr.Size > cardsZipMax {
		return nil, bad("The ZIP must be 100 MB or smaller")
	}
	ra, ok := f.(io.ReaderAt)
	if !ok {
		return nil, bad("That ZIP file could not be read")
	}
	zr, err := zip.NewReader(ra, hdr.Size)
	if errors.Is(err, zip.ErrInsecurePath) && zr != nil {
		err = nil // each unsafe name is refused on its own row (cardsUnsafe)
	}
	if err != nil {
		return nil, bad("That is not a ZIP file. Put the cards in one ZIP file and upload it")
	}
	if len(zr.File) > cardsFilesMax {
		return nil, bad("The ZIP has %d files: upload at most %d at a time", len(zr.File), cardsFilesMax)
	}
	ctx := r.Context()
	out, seen, total := []*cardFile{}, map[string]string{}, int64(0)
	for _, zf := range zr.File {
		name := zf.Name
		if zf.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			continue
		}
		base := path.Base(strings.ReplaceAll(name, "\\", "/"))
		// Files a Mac or Windows adds to a ZIP are not cards: left out without a word.
		if strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, "._") || base == ".DS_Store" || strings.EqualFold(base, "Thumbs.db") {
			continue
		}
		cf := &cardFile{name: name}
		out = append(out, cf)
		if p := cardsUnsafe(name); p != "" {
			cf.problem = p
			continue
		}
		m := reCardName.FindStringSubmatch(base)
		if m == nil {
			cf.problem = "Name it by Employee ID and card, like EMP-1007_ESIC.pdf or EMP-1007_MEDICAL.pdf"
			continue
		}
		cf.empID, cf.kind = strings.ToUpper(strings.TrimSpace(m[1])), strings.ToUpper(m[2])
		if zf.UncompressedSize64 > maxUpload {
			cf.problem = "Larger than 5 MB"
			continue
		}
		if zf.CompressedSize64 > 0 && zf.UncompressedSize64/zf.CompressedSize64 > cardsRatioMax {
			cf.problem = "Packed too tightly to be a card (refused as unsafe)"
			continue
		}
		data, err := cardsUnpack(zf)
		if err != nil {
			cf.problem = err.Error()
			continue
		}
		if total += int64(len(data)); total > cardsTotalMax {
			return nil, bad("The files in the ZIP add up to more than 600 MB unpacked: upload them in parts")
		}
		ctype := http.DetectContentType(data)
		ext, ok := allowedTypes[ctype]
		if !ok {
			cf.problem = "Not a PDF, JPG or PNG file"
			continue
		}
		up := base
		if len(up) > cardsNameMaxLen {
			up = up[:cardsNameMaxLen]
		}
		cf.file = &upload{name: up, ctype: ctype, ext: ext, data: data}
		e, _ := bulkFindCI(ctx, s.st.DB, "employee", "_id", cf.empID, nil, "name", "status")
		if e == nil {
			cf.unknown, cf.problem = true, "No employee "+cf.empID
			continue
		}
		cf.empID, cf.empName = str(e["id"]), str(e["name"])
		if err := s.canFiles(ctx, c, "employee", cf.empID, true); err != nil {
			cf.unknown, cf.problem = true, cf.empID+" is outside your access"
			continue
		}
		k := cf.empID + "|" + cf.kind
		if first, dup := seen[k]; dup {
			cf.problem = "A second " + cardKinds[cf.kind][1] + " for " + cf.empID + " (first: " + first + "). Keep one"
			continue
		}
		seen[k] = name
		if cur, _ := s.st.DB.C("document").FindOne(ctx, bson.M{"owner_kind": "employee", "owner_id": cf.empID, "doc_key": cardKinds[cf.kind][0], "replaced_at": nil}); cur != nil {
			cf.replaces = str(cur["file_name"])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// cardsUnsafe says why a name in the ZIP is refused (path traversal, an absolute path, a drive or control
// characters), or "".
func cardsUnsafe(name string) string {
	n := strings.ReplaceAll(name, "\\", "/")
	switch {
	case strings.HasPrefix(n, "/") || (len(n) > 1 && n[1] == ':'):
		return "Unsafe path in the ZIP (absolute): refused"
	case strings.ContainsAny(n, "\x00\r\n"):
		return "Unsafe file name: refused"
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "Unsafe path in the ZIP (..): refused"
		}
	}
	if len(name) > 400 {
		return "File name too long"
	}
	return ""
}

// cardsUnpack reads one file of the ZIP, at most 5 MB whatever its header claims.
func cardsUnpack(zf *zip.File) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, errors.New("Could not be unpacked")
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxUpload+1))
	if err != nil {
		return nil, errors.New("Could not be unpacked")
	}
	if len(data) > maxUpload {
		return nil, errors.New("Larger than 5 MB")
	}
	if len(data) == 0 {
		return nil, errors.New("The file is empty")
	}
	return data, nil
}

// cardsRun is POST /api/bulk/cards/{check|apply}: check lists every file of the ZIP as matched, unmatched or
// invalid; apply stores the matched ones (each through the document store, so a replaced card stays in history).
func (s *Server) cardsRun(r *http.Request, c *Ctx) (any, error) {
	t, err := templateFor(c, "cards")
	if err != nil {
		return nil, err
	}
	apply := r.PathValue("action") == "apply"
	if !apply && r.PathValue("action") != "check" {
		return nil, notFound("")
	}
	files, err := s.cardsRead(r, c)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, bad("The ZIP has no files")
	}
	ctx := r.Context()
	results, ok := []bulkResult{}, 0
	matched, unmatched, invalid := 0, 0, 0
	by := c.Name + " (" + c.EmployeeID + ")"
	for i, f := range files {
		res := bulkResult{Line: i + 1, ID: f.empID, Name: f.name, Errors: []string{}, Changes: []bulkChange{}}
		if f.empName != "" {
			res.Name = f.empName + " · " + f.name
		}
		switch {
		case f.unknown:
			unmatched++
			res.Errors = append(res.Errors, "Unmatched: "+f.problem)
		case f.problem != "":
			invalid++
			res.Errors = append(res.Errors, "Invalid: "+f.problem)
		default:
			matched++
			label := cardKinds[f.kind][1]
			note := "new"
			if f.replaces != "" {
				note = "replaces the current card, which stays in the history"
			}
			res.Changes = append(res.Changes, bulkChange{Field: label, From: or(f.replaces, "—"), To: f.file.name, Note: note})
			if apply {
				if _, err := s.storeDoc(ctx, c, "employee", f.empID, cardKinds[f.kind][0], f.file, by); err != nil {
					return nil, err
				}
			}
			ok++
		}
		results = append(results, res)
	}
	if apply {
		_ = audit(ctx, s.st.DB, c, fmt.Sprintf("Bulk update “%s” applied: %d file%s", t.Name, ok, map[bool]string{true: "", false: "s"}[ok == 1]), "bulk", t.Key, nil,
			map[string]any{"files": ok, "unmatched": unmatched, "invalid": invalid})
	}
	return map[string]any{"applied": apply, "ok": ok, "results": results, "summary": map[string]int{"matched": matched, "unmatched": unmatched, "invalid": invalid}}, nil
}
