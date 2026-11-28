package api_test

// Blank form soft copies of nomination documents (nom_blank.go): only HR Admin attaches, replaces or removes one;
// HR Admin, Payroll and the people a nomination applies to download it, nobody else.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const nbPDF = "%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n"

// nbBlank is the blank form on document doc of the employee's nomination 1 (EPF), nil when none.
func nbBlank(t *testing.T, c *client, emp, doc string) map[string]any {
	t.Helper()
	_, mine := c.do("GET", "/api/me/nominations", emp, "Employee", nil)
	for _, x := range mine["list"].([]any) {
		sc := x.(map[string]any)["scheme"].(map[string]any)
		if fmt.Sprint(sc["id"]) != "1" {
			continue
		}
		for _, d := range sc["docs"].([]any) {
			if m := d.(map[string]any); m["id"] == doc {
				b, _ := m["blank"].(map[string]any)
				return b
			}
		}
	}
	t.Fatalf("no document %s on nomination 1 for %s", doc, emp)
	return nil
}

func TestNomBlankForm(t *testing.T) {
	c, _, st := setupFull(t)
	other := apiDoc(st, "employee", bson.M{"status": "Active", "type": bson.M{"$in": bson.A{"Contractual", "Retainer"}}})
	if other == nil {
		t.Fatal("no contractual or retainer employee in the sample")
	}
	outsider := fmt.Sprint(other["id"])
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1016", outsider)
	path := "/api/nominations/schemes/1/blank/form"
	// Only HR Admin attaches; the document row must exist; the generic upload is not a way round.
	if s, r := c.sendFile(path, "EMP-1011", "Payroll", "Form 2.pdf", []byte(nbPDF)); s != 403 {
		t.Fatalf("Payroll attached: %d %v", s, r)
	}
	if s, _ := c.sendFile("/api/nominations/schemes/1/blank/nope", "EMP-1002", "HR Admin", "Form 2.pdf", []byte(nbPDF)); s != 404 {
		t.Fatalf("unknown document row: %d", s)
	}
	if s, _ := c.sendFile(path, "EMP-1002", "HR Admin", "Form 2.exe", []byte("MZ\x90\x00binary")); s != 400 {
		t.Fatalf("not a document: %d", s)
	}
	if s, _ := c.upload("EMP-1002", "HR Admin", "company", "nomination/1", "form"); s != 400 {
		t.Fatalf("generic upload: %d", s)
	}
	s, r := c.sendFile(path, "EMP-1002", "HR Admin", "Form 2.pdf", []byte(nbPDF))
	if s != 200 || r["name"] != "Form 2.pdf" {
		t.Fatalf("attach: %d %v", s, r)
	}
	// Replaced by a Word file: one current form, the employee sees the new one.
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("word/document.xml")
	_, _ = w.Write([]byte("<w:document/>"))
	_ = z.Close()
	if s, r = c.sendFile(path, "EMP-1002", "HR Admin", "Form 2.docx", buf.Bytes()); s != 200 {
		t.Fatalf("replace: %d %v", s, r)
	}
	id := fmt.Sprint(r["id"])
	if b := nbBlank(t, c, "EMP-1016", "form"); b == nil || b["id"] != id || b["name"] != "Form 2.docx" {
		t.Fatalf("employee view: %v", b)
	}
	// Download: the person it applies to, Payroll and HR Admin; not someone it does not apply to.
	for _, who := range [][2]string{{"EMP-1016", "Employee"}, {"EMP-1011", "Payroll"}, {"EMP-1002", "HR Admin"}} {
		if s, _ := c.getRaw("/api/files/"+id, who[0], who[1]); s != 200 {
			t.Fatalf("%s download: %d", who[1], s)
		}
	}
	if s, _ := c.getRaw("/api/files/"+id, outsider, "Employee"); s != 403 {
		t.Fatalf("%s (%v) downloaded a form that does not apply: %d", outsider, other["type"], s)
	}
	if s, _ := c.getRaw("/api/files?ownerKind=company&ownerId=nomination/1", outsider, "Employee"); s != 403 {
		t.Fatalf("list: %d", s)
	}
	// Remove: HR Admin only; marked replaced, nothing deleted; audited.
	if s, _ := c.do("DELETE", path, "EMP-1011", "Payroll", nil); s != 403 {
		t.Fatalf("Payroll removed: %d", s)
	}
	if s, r := c.do("DELETE", path, "EMP-1002", "HR Admin", nil); s != 200 {
		t.Fatalf("remove: %d %v", s, r)
	}
	if b := nbBlank(t, c, "EMP-1016", "form"); b != nil {
		t.Fatalf("still shown: %v", b)
	}
	if n := apiCount(st, "document", bson.M{"owner_kind": "company", "owner_id": "nomination/1"}); n != 2 {
		t.Fatalf("documents kept: %d", n)
	}
	if n := apiCount(st, "audit_log", bson.M{"action": bson.M{"$regex": "^Blank form (attached|replaced|removed)"}}); n != 3 {
		t.Fatalf("audit: %d", n)
	}
}
