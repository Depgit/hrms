package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Blank form soft copies of nomination documents (owner, 24 Nov 2026): HR Admin attaches a blank form (PDF, Word or
// an image) to a document row of a nomination ("Signed nomination form" → Form 2.pdf), replaces or removes it, from
// Nominations › Nomination policy. The employee downloads it next to that document when submitting, fills it and
// uploads it as before. The file is a company document (owner_kind "company", owner_id "nomination/<scheme id>",
// doc_key the document row's id), so Google Drive keeps it in the Company folder. Removing marks it replaced
// (nothing is deleted). Who sees it: canNomBlank.

// nomBlank is the blank form attached to a nomination document (nomDoc.Blank).
type nomBlank struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	At   any    `json:"at"`
}

const (
	ctypeDoc  = "application/msword"
	ctypeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func nomBlankOwner(scheme int) string { return fmt.Sprintf("nomination/%d", scheme) }

// nomBlanks puts the current blank forms on the schemes' documents.
func nomBlanks(ctx context.Context, q store.Q, schemes []nomScheme) error {
	owners := make([]string, len(schemes))
	for i, sc := range schemes {
		owners[i] = nomBlankOwner(sc.ID)
	}
	if len(owners) == 0 {
		return nil
	}
	rows, err := q.C("document").Find(ctx, bson.M{"owner_kind": "company", "owner_id": bson.M{"$in": owners}, "replaced_at": nil},
		options.Find().SetProjection(bson.D{{Key: "owner_id", Value: 1}, {Key: "doc_key", Value: 1}, {Key: "file_name", Value: 1}, {Key: "size_bytes", Value: 1}, {Key: "uploaded_at", Value: 1}}))
	if err != nil {
		return err
	}
	by := map[string]*nomBlank{}
	for _, r := range rows {
		by[str(r["owner_id"])+"|"+str(r["doc_key"])] = &nomBlank{ID: str(r["id"]), Name: str(r["file_name"]), Size: int64(toInt(r["size_bytes"])), At: r["uploaded_at"]}
	}
	for i := range schemes {
		for j := range schemes[i].Docs {
			schemes[i].Docs[j].Blank = by[nomBlankOwner(schemes[i].ID)+"|"+schemes[i].Docs[j].ID]
		}
	}
	return nil
}

// canNomBlank: HR Admin attaches blank forms (the nomination policy is theirs); HR Admin, Payroll (pay.claims) and
// every active employee a nomination on the form applies to may download them.
func (s *Server) canNomBlank(ctx context.Context, c *Ctx, owner string, write bool) error {
	if c.Role == "HR Admin" {
		return nil
	}
	if write {
		return forbidden("Only HR Admin sets the nomination policy")
	}
	if c.Level("pay.claims") >= 1 {
		return nil
	}
	_, id, _ := strings.Cut(owner, "/")
	sc, err := s.st.DB.C("nomination_scheme").FindOne(ctx, nominationsIDFilter(id))
	if err != nil {
		return err
	}
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": c.EmployeeID}, options.FindOne().SetProjection(bson.D{{Key: "type", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return err
	}
	if sc != nil && e != nil && sc["on_form"] == true && e["status"] == "Active" && containsStr(strs(sc["types"]), str(e["type"])) {
		return nil
	}
	return forbidden("This nomination does not apply to you")
}

// nomSchemeDoc finds a nomination and one of its document rows (switched off ones too: the form stays attached).
func nomSchemeDoc(ctx context.Context, q store.Q, id, doc string) (store.Row, nomDoc, error) {
	sc, err := q.C("nomination_scheme").FindOne(ctx, nominationsIDFilter(id))
	if err != nil {
		return nil, nomDoc{}, err
	}
	if sc == nil {
		return nil, nomDoc{}, notFound("Nomination not found")
	}
	var docs []nomDoc
	if err := employeesJSONInto(sc["docs"], &docs); err != nil {
		return nil, nomDoc{}, err
	}
	for _, d := range docs {
		if d.ID == doc {
			return sc, d, nil
		}
	}
	return nil, nomDoc{}, notFound("Save the nomination first: this document is not in it yet")
}

// nomBlankUpload attaches or replaces the blank form of a nomination document (POST multipart "file").
func (s *Server) nomBlankUpload(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin sets the nomination policy")
	}
	ctx := r.Context()
	sc, d, err := nomSchemeDoc(ctx, s.st.DB, r.PathValue("id"), r.PathValue("doc"))
	if err != nil {
		return nil, err
	}
	f, err := blankFormFile(r)
	if err != nil {
		return nil, err
	}
	owner := nomBlankOwner(toInt(sc["id"]))
	had, _ := s.st.DB.C("document").Exists(ctx, bson.M{"owner_kind": "company", "owner_id": owner, "doc_key": d.ID, "replaced_at": nil})
	row, err := s.storeDoc(ctx, c, "company", owner, d.ID, f, c.Name+" ("+c.EmployeeID+")")
	if err != nil {
		return nil, err
	}
	verb := map[bool]string{true: "replaced", false: "attached"}[had]
	if err := s.st.Tx(ctx, func(tx store.Q) error {
		return audit(ctx, tx, c, fmt.Sprintf("Blank form %s for “%s” (%s): %s", verb, d.Label, str(sc["name"]), f.name), "nomination_scheme", fmt.Sprint(sc["id"]), nil,
			map[string]any{"doc": d.ID, "file": f.name, "document": row["id"]})
	}); err != nil {
		return nil, err
	}
	return nomBlank{ID: str(row["id"]), Name: str(row["file_name"]), Size: int64(len(f.data)), At: row["uploaded_at"]}, nil
}

// nomBlankRemove takes the blank form off a nomination document (marked replaced, never deleted).
func (s *Server) nomBlankRemove(r *http.Request, c *Ctx) (any, error) {
	if c.Role != "HR Admin" {
		return nil, forbidden("Only HR Admin sets the nomination policy")
	}
	ctx := r.Context()
	sc, d, err := nomSchemeDoc(ctx, s.st.DB, r.PathValue("id"), r.PathValue("doc"))
	if err != nil {
		return nil, err
	}
	owner := nomBlankOwner(toInt(sc["id"]))
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		cur, err := tx.C("document").FindOne(ctx, bson.M{"owner_kind": "company", "owner_id": owner, "doc_key": d.ID, "replaced_at": nil})
		if err != nil {
			return err
		}
		if cur == nil {
			return notFound("No blank form is attached to this document")
		}
		if _, err := tx.C("document").UpdateMany(ctx, bson.M{"owner_kind": "company", "owner_id": owner, "doc_key": d.ID, "replaced_at": nil}, bson.M{"$set": bson.M{"replaced_at": tx.Now()}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, fmt.Sprintf("Blank form removed from “%s” (%s): %s", d.Label, str(sc["name"]), str(cur["file_name"])), "nomination_scheme", fmt.Sprint(sc["id"]),
			map[string]any{"doc": d.ID, "file": cur["file_name"], "document": cur["id"]}, nil)
	})
}

// blankFormFile reads a blank form upload: 5 MB at most, a PDF, JPG, PNG or a Word document (.doc, .docx) by its
// content.
func blankFormFile(r *http.Request) (*upload, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxUpload+64<<10)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		return nil, bad("The file must be 5 MB or smaller")
	}
	fh, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose a file to upload")
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, maxUpload+1))
	if err != nil {
		return nil, err
	}
	switch {
	case len(data) == 0:
		return nil, bad("The file is empty")
	case len(data) > maxUpload:
		return nil, bad("The file must be 5 MB or smaller")
	}
	name := filepath.Base(hdr.Filename)
	if len(name) > 120 {
		name = name[:120]
	}
	ext := strings.ToLower(filepath.Ext(name))
	ctype := http.DetectContentType(data)
	if e, ok := allowedTypes[ctype]; ok {
		return &upload{name: name, ctype: ctype, ext: e, data: data}, nil
	}
	if ext == ".doc" && bytes.HasPrefix(data, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}) {
		return &upload{name: name, ctype: ctypeDoc, ext: ".doc", data: data}, nil
	}
	if ext == ".docx" && bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		if z, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
			for _, f := range z.File {
				if f.Name == "word/document.xml" {
					return &upload{name: name, ctype: ctypeDocx, ext: ".docx", data: data}, nil
				}
			}
		}
	}
	return nil, bad("Upload a PDF, Word (.doc, .docx), JPG or PNG file")
}
