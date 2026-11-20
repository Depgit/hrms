package api_test

// Joining verification from Approvals (verify_edit.go) and bulk approval by Excel or ticks (approval_bulk.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// pendingVerify takes a new candidate through shortlist, interview, offer, joining, details and documents to a joining
// verification waiting on Payroll (EMP-1011), then the Payroll Manager (EMP-1012). It returns the JV id, the Employee
// ID and the candidate (SI-…).
func pendingVerify(t *testing.T, c *client, st *store.Store, n int) (vid, emp, si string) {
	t.Helper()
	first, last := "Joiner", []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel", "India", "Juliet"}[n]
	phone := fmt.Sprintf("98765%05d", 10000+n)
	si = selectCandidate(t, c, shortlistBody(st, first, last, phone))
	oi := approvedOffer(t, c, st, si, first, last, phone)
	c.do("POST", "/api/offers/"+oi+"/release", "EMP-1004", "HRBP", nil)
	c.do("POST", "/api/offers/"+oi+"/accept", "EMP-1004", "HRBP", nil)
	if s, r := c.do("POST", "/api/candidates/"+oi+"/joined", "EMP-1004", "HRBP", map[string]any{"date": today()}); s != 200 {
		t.Fatalf("joined: %v", r)
	}
	pan := fmt.Sprintf("ABCDE%04dF", 1000+n*11)
	if s, r := c.do("POST", "/api/offers/"+oi+"/joining", "EMP-1004", "HRBP", map[string]any{"data": joinData(pan)}); s != 200 {
		t.Fatalf("joining details: %v", r)
	}
	for _, k := range []string{"pan", "photo", "police", "bankProof"} {
		if s, r := c.upload("EMP-1004", "HRBP", "candidate", si, k); s != 200 {
			t.Fatalf("%s: %v", k, r)
		}
	}
	s, v := c.do("POST", "/api/offers/"+oi+"/send", "EMP-1004", "HRBP", nil)
	if s != 200 {
		t.Fatalf("send to Payroll: %v", v)
	}
	vid = v["id"].(string)
	o := apiDoc(st, "offer", bson.M{"_id": oi})
	return vid, fmt.Sprint(o["employee_id"]), si
}

// raw is a GET that returns the body as bytes.
func (c *client) raw(path, emp, role string) (int, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// replaceDoc posts a document to a verification.
func (c *client) replaceDoc(vid, emp, role, key string) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("docKey", key)
	f, _ := w.CreateFormFile("file", key+"-new.pdf")
	_, _ = f.Write([]byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n"))
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+"/api/approvals/"+vid+"/verify-docs", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func histNotes(st *store.Store, vid string) string {
	rows, _ := st.DB.C("approval_history").Find(context.Background(), bson.M{"request_id": vid})
	out := []string{}
	for _, r := range rows {
		out = append(out, fmt.Sprint(r["note"]))
	}
	return strings.Join(out, " | ")
}

func TestVerifyDocsAndEdit(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1014")
	vid, emp, si := pendingVerify(t, c, st, 0)
	vid2, _, si2 := pendingVerify(t, c, st, 1)
	_ = vid2

	// Documents: the verifier and the next approver see the list and open files; an unrelated employee does not.
	s, l := c.do("GET", "/api/approvals/"+vid+"/verify-docs", "EMP-1011", "Payroll", nil)
	if s != 200 || l["canReplace"] != true || l["mandatoryDone"] != l["mandatoryTotal"] || apiFloat(l["mandatoryTotal"]) < 4 {
		t.Fatalf("docs as Payroll: %d %v", s, l)
	}
	var panDoc string
	for _, d := range l["docs"].([]any) {
		x := d.(map[string]any)
		if x["key"] == "pan" {
			panDoc = fmt.Sprint(x["id"])
			if x["state"] != "Mandatory" || x["uploaded"] != true || x["file_name"] != "pan.pdf" {
				t.Fatalf("pan row: %v", x)
			}
		}
	}
	if s, b := c.raw("/api/approvals/"+vid+"/verify-docs/"+panDoc, "EMP-1011", "Payroll"); s != 200 || !bytes.HasPrefix(b, []byte("%PDF")) {
		t.Fatalf("Payroll opens the PAN card: %d %s", s, b)
	}
	if s, l := c.do("GET", "/api/approvals/"+vid+"/verify-docs", "EMP-1012", "Payroll Manager", nil); s != 200 || l["canReplace"] != false {
		t.Fatalf("the Payroll Manager sees, cannot replace yet: %d %v", s, l)
	}
	if s, _ := c.do("GET", "/api/approvals/"+vid+"/verify-docs", "EMP-1014", "Employee", nil); s != 403 {
		t.Fatalf("an unrelated employee lists the documents: %d", s)
	}
	if s, _ := c.raw("/api/approvals/"+vid+"/verify-docs/"+panDoc, "EMP-1014", "Employee"); s != 403 {
		t.Fatalf("an unrelated employee opens a document: %d", s)
	}
	other := apiDoc(st, "document", bson.M{"owner_id": si2, "doc_key": "pan", "replaced_at": nil})
	if s, _ := c.raw("/api/approvals/"+vid+"/verify-docs/"+fmt.Sprint(other["id"]), "EMP-1011", "Payroll"); s != 404 {
		t.Fatalf("another joiner's file through this request: %d", s)
	}
	// Replace: only while it waits on you.
	if s, _ := c.replaceDoc(vid, "EMP-1012", "Payroll Manager", "pan"); s != 403 {
		t.Fatalf("the Payroll Manager replaced before their step: %d", s)
	}
	if s, r := c.replaceDoc(vid, "EMP-1011", "Payroll", "pan"); s != 200 || r["file_name"] != "pan-new.pdf" {
		t.Fatalf("replace: %d %v", s, r)
	}
	if n := apiCount(st, "document", bson.M{"owner_id": si, "doc_key": "pan", "replaced_at": nil}); n != 1 {
		t.Fatalf("current PAN cards: %d", n)
	}
	if h := histNotes(st, vid); !strings.Contains(h, "Document replaced by Sunita Rao: PAN") {
		t.Fatalf("history: %s", h)
	}

	// Edit details: refused when not your step, and for others.
	edit := func(emp, role string, data map[string]any) (int, map[string]any) {
		return c.do("PUT", "/api/approvals/"+vid+"/verify-details", emp, role, map[string]any{"data": data})
	}
	if s, _ := edit("EMP-1012", "Payroll Manager", map[string]any{"pan": "ABCDE5555F"}); s != 403 {
		t.Fatalf("Payroll Manager before their step: %d", s)
	}
	if s, _ := edit("EMP-1014", "Employee", map[string]any{"pan": "ABCDE5555F"}); s != 403 {
		t.Fatalf("an employee edits: %d", s)
	}
	for _, x := range []struct {
		data map[string]any
		want string
	}{
		{map[string]any{"pan": "BAD"}, "PAN format looks wrong"},
		{map[string]any{"aadhaar": ""}, "Aadhaar number is required"},
		{map[string]any{"ifsc": "XX"}, "IFSC format looks wrong"},
		{map[string]any{"acct": "12"}, "Account number must be 6 to 18 digits"},
		{map[string]any{"dob": "2999-01-01"}, "Date of birth must be a past date"},
		{map[string]any{"gender": "Robot"}, "Gender: choose one of"},
		{map[string]any{"l1": "EMP-9999"}, "not an active employee"},
		{map[string]any{"uanAvail": "Yes"}, "UAN is required"},
		{map[string]any{"gross": 99999}, "cannot be changed while verifying"},
	} {
		s, r := edit("EMP-1011", "Payroll", x.data)
		if s != 400 || !strings.Contains(fmt.Sprint(r["error"], r["details"]), x.want) {
			t.Fatalf("%v: %d %v", x.data, s, r)
		}
	}
	oldBank := apiDoc(st, "employee_bank", bson.M{"employee_id": emp})
	s, r := edit("EMP-1011", "Payroll", map[string]any{"pan": "abcde5555f", "bankName": "ICICI Bank", "acct": "123456789012", "ifsc": "ICIC0001234", "gender": "Female"})
	if s != 200 || fmt.Sprint(r["changed"]) != "[PAN Bank name Account number IFSC code]" || r["employeeUpdated"] != true {
		t.Fatalf("edit: %d %v", s, r)
	}
	o := apiDoc(st, "offer", bson.M{"verify_request_id": vid})
	if d := o["data"].(map[string]any); d["pan"] != "ABCDE5555F" || d["acct"] != "123456789012" {
		t.Fatalf("offer data: %v", d)
	}
	if g := apiDoc(st, "employee_gov_id", bson.M{"employee_id": emp}); g["pan"] != "ABCDE5555F" {
		t.Fatalf("employee PAN: %v", g)
	}
	if n := apiCount(st, "employee_bank", bson.M{"employee_id": emp}); n != 1 {
		t.Fatalf("bank accounts: %d", n)
	}
	if b := apiDoc(st, "employee_bank", bson.M{"employee_id": emp}); b["account_no"] != "123456789012" || b["ifsc"] != "ICIC0001234" || b["bank_name"] != "ICICI Bank" || b["id"] != oldBank["id"] {
		t.Fatalf("bank: %v (was %v)", b, oldBank)
	}
	h := histNotes(st, vid)
	if !strings.Contains(h, "Details corrected by Sunita Rao: PAN ABCDE1000F → ABCDE5555F; Bank name HDFC Bank → ICICI Bank; Account number") ||
		!strings.Contains(h, "••••••••9012") || strings.Contains(h, "123456789012") {
		t.Fatalf("history: %s", h)
	}
	a := apiDoc(st, "audit_log", bson.M{"entity_id": vid, "action": bson.M{"$regex": "^Joining details corrected"}})
	if a == nil || !strings.Contains(fmt.Sprint(a["before"]), "ABCDE1000F") || !strings.Contains(fmt.Sprint(a["after"]), "ABCDE5555F") {
		t.Fatalf("audit: %v", a)
	}
	// After Payroll approves, the Payroll Manager may edit; Payroll no longer.
	if _, a := c.do("POST", "/api/approvals/"+vid+"/approve", "EMP-1011", "Payroll", nil); a["next"] != "Payroll Manager" {
		t.Fatal(a)
	}
	if s, _ := edit("EMP-1011", "Payroll", map[string]any{"bankName": "SBI"}); s != 403 {
		t.Fatalf("Payroll after their step: %d", s)
	}
	if s, r := edit("EMP-1012", "Payroll Manager", map[string]any{"bankName": "State Bank of India"}); s != 200 {
		t.Fatalf("Payroll Manager at their step: %d %v", s, r)
	}
	c.do("POST", "/api/approvals/"+vid+"/reject", "EMP-1012", "Payroll Manager", map[string]string{"note": "Bank proof unclear"})
	if s, r := edit("EMP-1012", "Payroll Manager", map[string]any{"bankName": "SBI"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "already rejected") {
		t.Fatalf("not pending: %d %v", s, r)
	}
}

// bulkFile is an exported workbook to fill in: cell(id, heading) and set(id, heading, value).
type bulkFile struct {
	t    *testing.T
	f    *excelize.File
	cols map[string]int
	rows map[string]int
	n    int
}

func openBulk(t *testing.T, b []byte) *bulkFile {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.GetRows("Approvals")
	if err != nil {
		t.Fatal(err)
	}
	bf := &bulkFile{t: t, f: f, cols: map[string]int{}, rows: map[string]int{}, n: len(rows)}
	for i, h := range rows[0] {
		bf.cols[h] = i + 1
	}
	for i, r := range rows[1:] {
		bf.rows[r[0]] = i + 2
	}
	return bf
}

func (b *bulkFile) cell(id, col string) string {
	cn, _ := excelize.CoordinatesToCellName(b.cols[col], b.rows[id])
	v, _ := b.f.GetCellValue("Approvals", cn)
	return v
}

func (b *bulkFile) set(id, col, v string) {
	row := b.rows[id]
	if row == 0 { // a new row at the end
		b.n++
		row = b.n
		b.rows[id] = row
		cn, _ := excelize.CoordinatesToCellName(1, row)
		_ = b.f.SetCellValue("Approvals", cn, id)
	}
	cn, _ := excelize.CoordinatesToCellName(b.cols[col], row)
	_ = b.f.SetCellValue("Approvals", cn, v)
}

func (b *bulkFile) bytes() []byte {
	var buf bytes.Buffer
	if err := b.f.Write(&buf); err != nil {
		b.t.Fatal(err)
	}
	return buf.Bytes()
}

// outcomes maps request id → outcome: reason, from a preview or apply result.
func outcomes(r map[string]any) map[string]string {
	out := map[string]string{}
	for _, x := range r["rows"].([]any) {
		m := x.(map[string]any)
		k := fmt.Sprint(m["id"])
		if _, dup := out[k]; dup {
			k += "#2"
		}
		out[k] = fmt.Sprint(m["outcome"], ": ", m["reason"])
	}
	return out
}

func TestBulkApprovals(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1009", "EMP-1011", "EMP-1012", "EMP-1014")
	v := make([]string, 10)
	emps := make([]string, 10)
	for i := 1; i <= 9; i++ {
		v[i], emps[i], _ = pendingVerify(t, c, st, i)
	}

	// Export: only what waits on you, with the joining details, masked numbers and documents.
	if s, _ := c.raw("/api/approvals/bulk/export?group=other", "EMP-1011", "Payroll"); s != 400 {
		t.Fatalf("unknown group: %d", s)
	}
	s, xb := c.raw("/api/approvals/bulk/export?group=join", "EMP-1011", "Payroll")
	if s != 200 {
		t.Fatalf("export: %d %s", s, xb)
	}
	bf := openBulk(t, xb)
	for _, h := range []string{"Request ID", "Request type", "Employee or joiner", "Department", "Location", "Details", "Raised on", "Waiting on", "PAN", "Aadhaar number", "Account number", "Documents", "Decision", "Comment"} {
		if bf.cols[h] == 0 {
			t.Fatalf("no column %q: %v", h, bf.cols)
		}
	}
	if len(bf.rows) != 9 {
		t.Fatalf("rows: %v", bf.rows)
	}
	if bf.cell(v[1], "Waiting on") != "Payroll" || bf.cell(v[1], "Request type") != "Joining verification" || !strings.HasPrefix(bf.cell(v[1], "Aadhaar number"), "••••") ||
		!docsComplete(bf.cell(v[1], "Documents")) || bf.cell(v[1], "Reporting manager (L1)") != "Anil Kapoor (EMP-1009)" {
		t.Fatalf("row: %q %q %q %q %q", bf.cell(v[1], "Waiting on"), bf.cell(v[1], "Request type"), bf.cell(v[1], "Aadhaar number"), bf.cell(v[1], "Documents"), bf.cell(v[1], "Reporting manager (L1)"))
	}
	if sheets := bf.f.GetSheetList(); !strings.Contains(strings.Join(sheets, ","), "How to fill") || !strings.Contains(strings.Join(sheets, ","), "Exported values") {
		t.Fatalf("sheets: %v", sheets)
	}
	if s, xb := c.raw("/api/approvals/bulk/export?group=join", "EMP-1012", "Payroll Manager"); s != 200 || len(openBulk(t, xb).rows) != 0 {
		t.Fatal("nothing waits on the Payroll Manager yet")
	}
	if s, xb := c.raw("/api/approvals/bulk/export?group=pay", "EMP-1011", "Payroll"); s != 200 || openBulk(t, xb).rows[v[1]] != 0 {
		t.Fatal("joining verifications are not on Payroll approvals")
	}

	// Fill it in.
	bf.set(v[1], "Decision", "Approve")
	bf.set(v[1], "PAN", "ABCDE7777F") // a correction with Approve; the masked Aadhaar is left as it is
	bf.set(v[2], "Decision", "Return")
	bf.set(v[2], "Comment", "Police verification is unreadable")
	bf.set(v[3], "Decision", "Return")     // no comment
	bf.set(v[4], "Bank name", "Axis Bank") // a correction only
	bf.set(v[5], "Decision", "Approve")
	bf.set(v[5], "IFSC code", "BAD") // an invalid correction
	// v[6] left blank
	bf.set(v[7], "Decision", "Approve") // approved on screen after the download
	bf.set(v[8], "Decision", "Approve") // returned on screen after the download
	bf.set(v[9], "Decision", "Approve")
	bf.set(v[9], "Account number", "••••1234") // a masked cell edited
	bf.set("JV-99999", "Decision", "Approve")
	file := bf.bytes()
	dup := openBulk(t, file)
	dup.set(v[2]+" ", "Decision", "Return") // the same request twice (spaces are trimmed)
	file = dup.bytes()
	c.do("POST", "/api/approvals/"+v[7]+"/approve", "EMP-1011", "Payroll", nil)
	c.do("POST", "/api/approvals/"+v[8]+"/reject", "EMP-1011", "Payroll", map[string]string{"note": "Wrong person"})

	s, p := c.sendFile("/api/approvals/bulk/preview?group=join", "EMP-1011", "Payroll", "joining.xlsx", file)
	if s != 200 {
		t.Fatalf("preview: %d %v", s, p)
	}
	o := outcomes(p)
	want := map[string]string{
		v[1]: "approve: <nil>", v[2]: "return: <nil>", v[3]: "skip: Return needs a comment", v[4]: "correct: <nil>", v[5]: "skip: Correction not valid: IFSC format looks wrong",
		v[6]: "skip: No decision", v[7]: "skip: Not waiting on you any more: now with Payroll Manager", v[8]: "skip: Already rejected since the download",
		v[9]: "skip: Type the full new Account number, not the masked one", "JV-99999": "skip: Unknown Request ID", v[2] + "#2": "skip: Listed twice: only the first row counts",
	}
	for k, w := range want {
		if o[k] != w {
			t.Errorf("preview %s: %q, want %q", k, o[k], w)
		}
	}
	if p["approve"] != 1.0 || p["return"] != 1.0 || p["correct"] != 1.0 || p["skip"] != 8.0 || p["corrections"] != 2.0 {
		t.Fatalf("preview counts: %v", p)
	}
	for _, x := range p["rows"].([]any) {
		m := x.(map[string]any)
		if m["id"] == v[1] && fmt.Sprint(m["changes"]) != "[map[field:PAN from:ABCDE1011F to:ABCDE7777F]]" {
			t.Fatalf("v1 changes: %v", m["changes"])
		}
	}
	before1 := apiDoc(st, "offer", bson.M{"verify_request_id": v[1]})["data"].(map[string]any)
	if before1["pan"] != "ABCDE1011F" {
		t.Fatal("the preview changed nothing")
	}
	// Another tab's types.
	_, pp := c.sendFile("/api/approvals/bulk/preview?group=pay", "EMP-1011", "Payroll", "joining.xlsx", file)
	if o := outcomes(pp); o[v[1]] != "skip: Not a request of this tab (Joining verification)" {
		t.Fatalf("pay tab: %v", o[v[1]])
	}

	// Apply the same file.
	s, a := c.sendFile("/api/approvals/bulk/apply-file?group=join", "EMP-1011", "Payroll", "joining.xlsx", file)
	if s != 200 || a["approved"] != 1.0 || a["returned"] != 1.0 || a["corrected"] != 1.0 || a["skipped"] != 8.0 || a["failed"] != 0.0 {
		t.Fatalf("apply: %d %v", s, a)
	}
	r1 := apiDoc(st, "approval_request", bson.M{"_id": v[1]})
	if r1["status"] != "Pending" || apiFloat(r1["step"]) != 1 {
		t.Fatalf("v1 moves on to the Payroll Manager: %v %v", r1["status"], r1["step"])
	}
	if d := apiDoc(st, "offer", bson.M{"verify_request_id": v[1]})["data"].(map[string]any); d["pan"] != "ABCDE7777F" || d["aadhaar"] != before1["aadhaar"] {
		t.Fatalf("v1 data: %v", d)
	}
	if g := apiDoc(st, "employee_gov_id", bson.M{"employee_id": emps[1]}); g["pan"] != "ABCDE7777F" {
		t.Fatalf("v1 employee PAN: %v", g)
	}
	if h := histNotes(st, v[1]); !strings.Contains(h, "Details corrected by Sunita Rao (bulk approval file): PAN ABCDE1011F → ABCDE7777F") {
		t.Fatalf("v1 history: %s", h)
	}
	if r := apiDoc(st, "approval_request", bson.M{"_id": v[2]}); r["status"] != "Rejected" {
		t.Fatalf("v2: %v", r["status"])
	}
	r4 := apiDoc(st, "approval_request", bson.M{"_id": v[4]})
	if r4["status"] != "Pending" || apiFloat(r4["step"]) != 0 || apiDoc(st, "offer", bson.M{"verify_request_id": v[4]})["data"].(map[string]any)["bankName"] != "Axis Bank" {
		t.Fatalf("v4 corrected only: %v %v", r4["status"], r4["step"])
	}
	if r := apiDoc(st, "approval_request", bson.M{"_id": v[5]}); apiFloat(r["step"]) != 0 || apiDoc(st, "offer", bson.M{"verify_request_id": v[5]})["data"].(map[string]any)["ifsc"] != "HDFC0001234" {
		t.Fatal("v5 untouched")
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Bulk approval from Excel \\(Joining approvals\\): 1 approved, 1 returned, 2 with corrections, 8 skipped, 0 failed"}}); a == nil {
		t.Fatal("bulk run audited")
	}

	// Re-checked at apply time: decided on screen between the preview and the confirm.
	f2 := openBulk(t, xb2(t, c))
	f2.set(v[6], "Decision", "Approve")
	b2 := f2.bytes()
	if _, p := c.sendFile("/api/approvals/bulk/preview?group=join", "EMP-1011", "Payroll", "j.xlsx", b2); p["approve"] != 1.0 {
		t.Fatalf("preview 2: %v", p)
	}
	c.do("POST", "/api/approvals/"+v[6]+"/reject", "EMP-1011", "Payroll", map[string]string{"note": "Not yet"})
	if _, a := c.sendFile("/api/approvals/bulk/apply-file?group=join", "EMP-1011", "Payroll", "j.xlsx", b2); a["approved"] != 0.0 || outcomes(a)[v[6]] != "skipped: Already rejected since the download" {
		t.Fatalf("apply 2: %v", a)
	}

	// Ticked on screen: approves only your own stage; the next stage follows; Return needs a comment.
	sel := func(emp, role string, items ...map[string]any) map[string]any {
		_, r := c.do("POST", "/api/approvals/bulk/apply", emp, role, map[string]any{"group": "join", "source": "select", "items": items})
		return r
	}
	r := sel("EMP-1011", "Payroll", map[string]any{"id": v[4], "decision": "approve", "stage": "Payroll"}, map[string]any{"id": v[1], "decision": "approve", "stage": "Payroll"},
		map[string]any{"id": v[3], "decision": "return"})
	o = outcomes(r)
	if r["approved"] != 1.0 || o[v[1]] != "skipped: Not waiting on you any more: now with Payroll Manager" || o[v[3]] != "skipped: Return needs a comment" {
		t.Fatalf("select as Payroll: %v", r)
	}
	if r4 := apiDoc(st, "approval_request", bson.M{"_id": v[4]}); r4["status"] != "Pending" || apiFloat(r4["step"]) != 1 {
		t.Fatalf("v4 at the Payroll Manager: %v", r4)
	}
	r = sel("EMP-1012", "Payroll Manager", map[string]any{"id": v[1], "decision": "approve", "stage": "Payroll Manager"}, map[string]any{"id": v[4], "decision": "approve", "stage": "Payroll Manager"})
	if r["approved"] != 2.0 {
		t.Fatalf("select as Payroll Manager: %v", r)
	}
	for _, x := range r["rows"].([]any) {
		if m := x.(map[string]any); m["next"] != "Fully approved" {
			t.Fatalf("fully approved: %v", m)
		}
	}
	if e := apiDoc(st, "employee", bson.M{"_id": emps[1]}); e["status"] != "Active" || e["verified"] != true {
		t.Fatalf("v1's employee is active: %v %v", e["status"], e["verified"])
	}
	if s, _ := c.do("POST", "/api/approvals/bulk/apply", "EMP-1011", "Payroll", map[string]any{"group": "join", "items": []any{}}); s != 400 {
		t.Fatal("nothing to apply")
	}
}

// xb2 downloads Payroll's joining file again.
func xb2(t *testing.T, c *client) []byte {
	s, b := c.raw("/api/approvals/bulk/export?group=join", "EMP-1011", "Payroll")
	if s != 200 {
		t.Fatalf("export: %d", s)
	}
	return b
}

// docsComplete: "N of N mandatory uploaded…".
func docsComplete(s string) bool {
	var a, b int
	_, err := fmt.Sscanf(s, "%d of %d mandatory uploaded", &a, &b)
	return err == nil && a == b && a >= 4
}
