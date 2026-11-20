package api_test

// Documents from existing employees (doc_collect.go): who gets a window, the employee uploading only their own
// listed documents while it is open, the window closing, HR extending, verifying and sending back, reminders sent
// once, and the scope of HR's tracker.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
	"github.com/saltstayz/hrms/internal/store"
)

// docUpload posts a small PDF as docKey to path (multipart, as the web app does).
func docUpload(c *client, emp, role, path, key string) (int, map[string]any) {
	c.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("docKey", key)
	f, _ := w.CreateFormFile("file", key+".pdf")
	_, _ = f.Write([]byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n"))
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
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

func docSweep(t *testing.T, app *api.Server) map[string]int {
	t.Helper()
	n, err := app.DocSweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// docList is the docs of a /me/documents or /doc-collection/{id} answer, by key.
func docList(b map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	l, _ := b["docs"].([]any)
	for _, x := range l {
		d := x.(map[string]any)
		out[d["key"].(string)] = d
	}
	return out
}

// docMandatory is the keys of the mandatory documents in a list answer.
func docMandatory(b map[string]any) []string {
	var out []string
	l, _ := b["docs"].([]any)
	for _, x := range l {
		d := x.(map[string]any)
		if d["mandatory"] == true {
			out = append(out, d["key"].(string))
		}
	}
	return out
}

// setStart moves an employee's window to start n days before today.
func setStart(t *testing.T, st *store.Store, emp string, daysAgo int) {
	t.Helper()
	start := docDate(-daysAgo)
	if _, err := st.DB.C(store.DocWindowColl).Update(context.Background(), bson.M{"employee_id": emp}, bson.M{"$set": bson.M{"start": start}}); err != nil {
		t.Fatal(err)
	}
}

func docDate(days int) string {
	t, _ := time.Parse("2006-01-02", store.Today())
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// docGet is a GET that must answer 200.
func docGet(c *client, path, emp, role string) map[string]any {
	c.t.Helper()
	s, b := c.do("GET", path, emp, role, nil)
	if s != 200 {
		c.t.Fatalf("GET %s: %d %v", path, s, b)
	}
	return b
}

func TestDocCollectEligibility(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	// EMP-1016 came through the joining flow (an offer made the record); EMP-1022 has every mandatory document;
	// EMP-1046 has left.
	if _, err := st.DB.C("offer").Raw().InsertOne(ctx, bson.M{"_id": "OFR-9901", "employee_id": "EMP-1016"}); err != nil {
		t.Fatal(err)
	}
	_, d22 := c.do("GET", "/api/doc-collection/EMP-1022", "EMP-1002", "HR Admin", nil)
	if d22["window"] != nil {
		t.Fatalf("no window before the sweep: %v", d22)
	}
	for _, k := range docMandatory(d22) {
		if s, b := docUpload(c, "EMP-1002", "HR Admin", "/api/doc-collection/EMP-1022/files", k); s != 200 {
			t.Fatalf("HR upload %s: %d %v", k, s, b)
		}
	}
	if _, err := st.DB.C("employee").Raw().UpdateOne(ctx, bson.M{"_id": "EMP-1046"}, bson.M{"$set": bson.M{"status": "Inactive"}}); err != nil {
		t.Fatal(err)
	}
	n := docSweep(t, app)
	if n["opened"] == 0 {
		t.Fatalf("sweep opened nothing: %v", n)
	}
	w := apiDoc(st, store.DocWindowColl, bson.M{"employee_id": "EMP-1020"})
	if w == nil || w["start"] != store.Today() || w["source"] != "startup" {
		t.Fatalf("EMP-1020 window: %v", w)
	}
	for _, id := range []string{"EMP-1016", "EMP-1022", "EMP-1046"} {
		if apiDoc(st, store.DocWindowColl, bson.M{"employee_id": id}) != nil {
			t.Errorf("%s must not get a window", id)
		}
	}
	// Running again opens nothing more and sends nothing twice.
	before := apiCount(st, "notice", bson.M{"kind": "documents"})
	if n2 := docSweep(t, app); n2["opened"] != 0 || n2["employee"] != 0 {
		t.Fatalf("second sweep: %v", n2)
	}
	if after := apiCount(st, "notice", bson.M{"kind": "documents"}); after != before {
		t.Fatalf("messages %d → %d on a second sweep", before, after)
	}
	if k := apiCount(st, "notice", bson.M{"kind": "documents", "to_employee_id": "EMP-1020"}); k != 1 {
		t.Fatalf("EMP-1020 has %d opening messages, want 1", k)
	}
	// An HR Admin import opens a window dated today.
	e := apiDoc(st, "employee", bson.M{"_id": "EMP-1020"})
	name := func(coll string, id any) string { return fmt.Sprint(apiDoc(st, coll, bson.M{"_id": id})["name"]) }
	row := map[string]any{"name": "Docs Import Person", "type": "Permanent", "doj": "2024-01-05", "location": name("location", e["location_id"]),
		"department": name("department", e["department_id"]), "designation": name("designation", e["designation_id"]), "grade": e["grade"],
		"shift": e["shift_code"], "gross": 17000, "code": "DOC-0001"}
	if s, b := c.do("POST", "/api/employees/import", "EMP-1002", "HR Admin", map[string]any{"rows": []any{row}, "apply": true}); s != 200 {
		t.Fatalf("import: %d %v", s, b)
	}
	if w := apiDoc(st, store.DocWindowColl, bson.M{"employee_id": "DOC-0001"}); w == nil || w["source"] != "import" || w["start"] != store.Today() {
		t.Fatalf("imported window: %v", w)
	}
	// Switched off: nothing opens and nobody can upload.
	if s, b := c.do("PUT", "/api/doc-collection/settings", "EMP-1002", "HR Admin", map[string]any{"days": 60, "on": false}); s != 200 {
		t.Fatalf("settings: %d %v", s, b)
	}
	c.signIn("EMP-1020")
	_, me := c.do("GET", "/api/me/documents", "EMP-1020", "Employee", nil)
	if win := me["window"].(map[string]any); win["open"] != false {
		t.Fatalf("window open while switched off: %v", win)
	}
	if s, _ := c.do("PUT", "/api/doc-collection/settings", "EMP-1020", "Employee", map[string]any{"days": 60, "on": true}); s != 403 {
		t.Fatalf("an employee changed the settings: %d", s)
	}
}

func TestDocCollectEmployeeAndHR(t *testing.T) {
	c, app, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1004", "EMP-1011", "EMP-1018", "EMP-1020"} {
		c.signIn(e)
	}
	docSweep(t, app)
	_, me := c.do("GET", "/api/me/documents", "EMP-1020", "Employee", nil)
	win := me["window"].(map[string]any)
	if win["open"] != true || int(win["daysLeft"].(float64)) != 90 {
		t.Fatalf("window: %v", win)
	}
	mand := docMandatory(me)
	if len(mand) < 2 {
		t.Fatalf("mandatory documents: %v", mand)
	}
	k1, k2 := mand[0], mand[1]
	// Own listed documents only.
	if s, b := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", k1); s != 200 {
		t.Fatalf("upload %s: %d %v", k1, s, b)
	}
	if s, _ := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", "not-a-document"); s != 400 {
		t.Fatalf("a key off the list: %d", s)
	}
	if s, _ := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", k2); s != 400 {
		t.Fatalf("own Form setup document through /api/files: %d", s)
	}
	if s, _ := c.upload("EMP-1020", "Employee", "employee", "EMP-1018", k2); s != 403 {
		t.Fatalf("upload to someone else's record: %d", s)
	}
	if s, _ := c.upload("EMP-1020", "Employee", "employee", "EMP-1020", "Claim bill"); s != 200 {
		t.Fatalf("a claim bill must still upload: %d", s)
	}
	d := apiDoc(st, "document", bson.M{"owner_id": "EMP-1020", "doc_key": k1, "replaced_at": nil})
	if d == nil || d["owner_kind"] != "employee" {
		t.Fatalf("stored on the employee's record: %v", d)
	}
	if docList(docGet(c, "/api/me/documents", "EMP-1020", "Employee"))[k1]["status"] != "uploaded" {
		t.Fatal("status uploaded")
	}
	// A document Form setup gives to HR.
	if _, err := st.DB.C("onb_document").Update(ctx, bson.M{"key": k2}, bson.M{"$set": bson.M{"filled_by": "hr"}}); err != nil {
		t.Fatal(err)
	}
	if s, _ := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", k2); s != 400 {
		t.Fatalf("an HR document uploaded by the employee: %d", s)
	}
	if _, err := st.DB.C("onb_document").Update(ctx, bson.M{"key": k2}, bson.M{"$set": bson.M{"filled_by": "joiner"}}); err != nil {
		t.Fatal(err)
	}
	// Scope: another employee, Payroll (read only), the HRBP.
	if s, _ := c.do("GET", "/api/doc-collection/EMP-1020", "EMP-1018", "Employee", nil); s != 403 {
		t.Fatalf("another employee read EMP-1020's documents: %d", s)
	}
	if s, _ := c.do("POST", "/api/doc-collection/EMP-1020/review", "EMP-1018", "Employee", map[string]any{"docKey": k1, "action": "verify"}); s != 403 {
		t.Fatalf("another employee verified: %d", s)
	}
	if s, _ := c.do("GET", "/api/doc-collection", "EMP-1018", "Employee", nil); s != 403 {
		t.Fatalf("an employee read the tracker: %d", s)
	}
	if s, b := c.do("GET", "/api/doc-collection/EMP-1020", "EMP-1011", "Payroll", nil); s != 200 || b["canEdit"] != false {
		t.Fatalf("Payroll reads: %d %v", s, b["canEdit"])
	}
	if s, _ := c.do("POST", "/api/doc-collection/EMP-1020/review", "EMP-1011", "Payroll", map[string]any{"docKey": k1, "action": "verify"}); s != 403 {
		t.Fatalf("Payroll verified: %d", s)
	}
	if s, _ := c.do("POST", "/api/doc-collection/EMP-1002/review", "EMP-1002", "HR Admin", map[string]any{"docKey": k1, "action": "verify"}); s != 403 {
		t.Fatalf("HR checked their own: %d", s)
	}
	_, all := c.do("GET", "/api/doc-collection", "EMP-1002", "HR Admin", nil)
	_, hrbp := c.do("GET", "/api/doc-collection", "EMP-1004", "HRBP", nil)
	ids := func(b map[string]any) map[string]bool {
		m := map[string]bool{}
		for _, r := range b["rows"].([]any) {
			m[r.(map[string]any)["id"].(string)] = true
		}
		return m
	}
	allIDs, hrbpIDs := ids(all), ids(hrbp)
	if len(hrbpIDs) == 0 || len(hrbpIDs) >= len(allIDs) {
		t.Fatalf("HRBP sees %d of %d", len(hrbpIDs), len(allIDs))
	}
	outside := ""
	for id := range allIDs {
		if !hrbpIDs[id] {
			outside = id
			break
		}
	}
	if s, _ := c.do("GET", "/api/doc-collection/"+outside, "EMP-1004", "HRBP", nil); s != 403 {
		t.Fatalf("HRBP opened %s outside their scope: %d", outside, s)
	}
	if s, _ := c.do("POST", "/api/doc-collection/"+outside+"/review", "EMP-1004", "HRBP", map[string]any{"docKey": k1, "action": "verify"}); s != 403 {
		t.Fatalf("HRBP verified outside their scope: %d", s)
	}
	if _, b := c.do("POST", "/api/doc-collection/extend", "EMP-1004", "HRBP", map[string]any{"ids": []string{outside}, "days": 5}); len(b["done"].([]any)) != 0 {
		t.Fatalf("HRBP extended outside their scope: %v", b)
	}
	// Verify: a verified document cannot be replaced by the employee.
	if s, b := c.do("POST", "/api/doc-collection/EMP-1020/review", "EMP-1002", "HR Admin", map[string]any{"docKey": k1, "action": "verify"}); s != 200 {
		t.Fatalf("verify: %d %v", s, b)
	}
	if s, _ := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", k1); s != 400 {
		t.Fatalf("replaced a verified document: %d", s)
	}
	// Send back: a reason is needed; the employee is told and uploads again.
	if s, b := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", k2); s != 200 {
		t.Fatalf("upload %s: %d %v", k2, s, b)
	}
	if s, _ := c.do("POST", "/api/doc-collection/EMP-1020/review", "EMP-1002", "HR Admin", map[string]any{"docKey": k2, "action": "return"}); s != 400 {
		t.Fatalf("send back without a reason: %d", s)
	}
	if s, b := c.do("POST", "/api/doc-collection/EMP-1020/review", "EMP-1002", "HR Admin", map[string]any{"docKey": k2, "action": "return", "reason": "The photo is blurred"}); s != 200 {
		t.Fatalf("send back: %d %v", s, b)
	}
	it := docList(docGet(c, "/api/me/documents", "EMP-1020", "Employee"))[k2]
	if it["status"] != "returned" || it["reason"] != "The photo is blurred" || it["canUpload"] != true {
		t.Fatalf("sent back: %v", it)
	}
	if apiCount(st, "notice", bson.M{"to_employee_id": "EMP-1020", "text": bson.M{"$regex": "sent back"}}) != 1 {
		t.Fatal("the employee is told")
	}
	if s, _ := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", k2); s != 200 {
		t.Fatalf("upload again: %d", s)
	}
	if docList(docGet(c, "/api/me/documents", "EMP-1020", "Employee"))[k2]["status"] != "uploaded" {
		t.Fatal("a new upload waits for HR again")
	}
	// The window closes.
	setStart(t, st, "EMP-1020", 100)
	_, me = c.do("GET", "/api/me/documents", "EMP-1020", "Employee", nil)
	win = me["window"].(map[string]any)
	if win["open"] != false || int(win["overdueDays"].(float64)) != 10 {
		t.Fatalf("closed window: %v", win)
	}
	if s, b := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", mand[len(mand)-1]); s != 400 || !strings.Contains(fmt.Sprint(b["error"]), "Ask HR to reopen") {
		t.Fatalf("upload after the window: %d %v", s, b)
	}
	_, ov := c.do("GET", "/api/doc-collection?filter=overdue", "EMP-1002", "HR Admin", nil)
	if !ids(ov)["EMP-1020"] {
		t.Fatal("EMP-1020 is in Overdue")
	}
	// HR extends: an ended window reopens from today.
	_, ext := c.do("POST", "/api/doc-collection/extend", "EMP-1002", "HR Admin", map[string]any{"ids": []string{"EMP-1020"}, "days": 5})
	if done := ext["done"].([]any); len(done) != 1 || done[0].(map[string]any)["due"] != docDate(5) || done[0].(map[string]any)["reopened"] != true {
		t.Fatalf("extend: %v", ext)
	}
	if s, b := docUpload(c, "EMP-1020", "Employee", "/api/me/documents", mand[len(mand)-1]); s != 200 {
		t.Fatalf("upload after extend: %d %v", s, b)
	}
	// HR uploads on the employee's behalf.
	if s, b := docUpload(c, "EMP-1002", "HR Admin", "/api/doc-collection/EMP-1020/files", k1); s != 200 {
		t.Fatalf("HR upload: %d %v", s, b)
	}
	if f := docList(docGet(c, "/api/doc-collection/EMP-1020", "EMP-1002", "HR Admin"))[k1]["file"].(map[string]any); f["byHR"] != true {
		t.Fatalf("marked uploaded by HR: %v", f)
	}
	// Audit: no document contents, only names.
	for _, a := range []string{"Verified ", "Sent back ", "Reopened document upload", "Uploaded "} {
		if apiCount(st, "audit_log", bson.M{"action": bson.M{"$regex": "^" + a}}) == 0 {
			t.Errorf("audit %q missing", a)
		}
	}
	// The Excel tracker.
	req, _ := http.NewRequest("GET", c.base+"/api/doc-collection/export", nil)
	req.Header.Set("Authorization", "Bearer "+c.tokens["EMP-1002"])
	req.Header.Set("X-Role", "HR Admin")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "spreadsheetml") {
		t.Fatalf("export: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestDocCollectReminders(t *testing.T) {
	c, app, st := setupFull(t)
	c.signIn("EMP-1002")
	docSweep(t, app)
	count := func(emp, re string) int {
		return apiCount(st, "notice", bson.M{"to_employee_id": emp, "kind": "documents", "text": bson.M{"$regex": re}})
	}
	// 7 days left: one reminder, however often the sweep runs.
	setStart(t, st, "EMP-1020", 83)
	docSweep(t, app)
	docSweep(t, app)
	if n := count("EMP-1020", "^Reminder: 7 days left"); n != 1 {
		t.Fatalf("7-day reminders: %d", n)
	}
	// 1 day left.
	setStart(t, st, "EMP-1020", 89)
	docSweep(t, app)
	docSweep(t, app)
	if n := count("EMP-1020", "^Reminder: 1 day left"); n != 1 {
		t.Fatalf("1-day reminders: %d", n)
	}
	// Ended with documents missing: their HRBP (or HR Admin) is told once.
	setStart(t, st, "EMP-1020", 91)
	before := apiCount(st, "notice", bson.M{"kind": "documents", "text": bson.M{"$regex": "^Document upload closed for .*EMP-1020"}})
	docSweep(t, app)
	docSweep(t, app)
	after := apiCount(st, "notice", bson.M{"kind": "documents", "text": bson.M{"$regex": "^Document upload closed for .*EMP-1020"}})
	if before != 0 || after < 1 {
		t.Fatalf("closing message: %d → %d", before, after)
	}
	n := apiDoc(st, "notice", bson.M{"kind": "documents", "text": bson.M{"$regex": "^Document upload closed for .*EMP-1020"}})
	if n["view"] != "HRBP" && n["view"] != "HR Admin" {
		t.Fatalf("closing message goes to HR: %v", n)
	}
	docSweep(t, app)
	if again := apiCount(st, "notice", bson.M{"kind": "documents", "text": bson.M{"$regex": "^Document upload closed for .*EMP-1020"}}); again != after {
		t.Fatalf("closing message repeated: %d → %d", after, again)
	}
	// A reminder by hand: once a day.
	_, b := c.do("POST", "/api/doc-collection/remind", "EMP-1002", "HR Admin", map[string]any{"ids": []string{"EMP-1018", "EMP-1018"}})
	if len(b["sent"].([]any)) != 1 {
		t.Fatalf("remind: %v", b)
	}
	_, b = c.do("POST", "/api/doc-collection/remind", "EMP-1002", "HR Admin", map[string]any{"ids": []string{"EMP-1018"}})
	if len(b["sent"].([]any)) != 0 || len(b["skipped"].([]any)) != 1 {
		t.Fatalf("second reminder the same day: %v", b)
	}
}
