package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/store"
)

// Documents: one index row per file in `document`; the bytes are staged in s.files and then kept in the
// company's Google Drive (drive.go), which deletes the staged copy once the upload is confirmed. Nothing is
// deleted: a newer upload for the same owner and document marks the earlier one replaced.

const maxUpload = 5 << 20 // 5 MB

var allowedTypes = map[string]string{"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png"}

// draftOwner is a client-made key for documents uploaded before an offer is saved.
func draftOwner(id string) bool {
	return strings.HasPrefix(id, "DRAFT-") && len(id) >= 14 && len(id) <= 64
}

// canFiles checks access to the documents of one owner. write is upload; otherwise view.
func (s *Server) canFiles(ctx context.Context, c *Ctx, kind, owner string, write bool) error {
	switch kind {
	case "offer":
		if draftOwner(owner) {
			if c.Level("onb.raise") < 2 {
				return forbidden("")
			}
			// Before the offer is saved, only the person who uploaded can see the files.
			if !write {
				byMe := bson.M{"$regex": regexp.QuoteMeta("("+c.EmployeeID+")") + "$"} // uploaded_by is "Name (EMP-…)"
				if ok, _ := s.st.DB.C("document").Exists(ctx, bson.M{"owner_kind": "offer", "owner_id": owner, "uploaded_by": bson.M{"$not": byMe}}); ok {
					return forbidden("")
				}
			}
			return nil
		}
		ok, _ := s.st.DB.C("offer").Exists(ctx, bson.M{"_id": owner})
		if !ok {
			return notFound("Offer not found")
		}
		need := 1
		if write {
			need = 2
		}
		// Onboarding roles in scope; Payroll sees every document while verifying.
		if c.Level("onb.pipeline") < need && c.Level("onb.raise") < need && (write || c.Level("appr.queue") < 1) {
			return forbidden("")
		}
		return c.MustInScope(ctx, s.st.DB, owner)
	case "employee":
		if owner == c.EmployeeID {
			return nil
		}
		need := 1
		if write {
			need = 2
		}
		// Copies of ID cards are as confidential as the numbers on them.
		if c.Level("emp.directory") < need || c.Level("emp.govid") < 1 {
			return forbidden("")
		}
		return c.MustInScope(ctx, s.st.DB, owner)
	case "nomination":
		// owner is "<employee id>/<scheme id>"
		emp, _, _ := strings.Cut(owner, "/")
		if emp == c.EmployeeID {
			return nil
		}
		if write || c.Level("pay.claims") < 1 {
			return forbidden("")
		}
		return c.MustInScope(ctx, s.st.DB, emp)
	case "company":
		// The payslip logo: everyone sees it on their payslip; payroll setup changes it.
		if write && c.Level("pay.setup") < 2 {
			return forbidden("")
		}
		return nil
	case "approval":
		// Proof attached to a pay request (a deduction): owner is "<employee id>/<key>". Whoever
		// raises deductions uploads it (never for themselves); pay viewers and approvers see it.
		return s.canApprovalFile(ctx, c, owner, write)
	case "policy", "policy_ack":
		// Company policy documents and signed copies (policy_ack.go).
		return s.canPolicyFile(ctx, c, kind, owner, write)
	case "candidate":
		// A candidate's documents at every stage, owner SI-0001 (onboarding_stages.go).
		return s.canCandidateFiles(ctx, c, owner, write)
	case "hst_lease":
		// An accommodation lease's documents (agreement, landlord ID …), owner the lease id (hst.go).
		need := 1
		if write {
			need = 2
		}
		if c.Level("hst.setup") < need {
			return forbidden("")
		}
		l, err := s.st.DB.C("hst_lease").FindOne(ctx, bson.M{"_id": owner})
		if err != nil {
			return err
		}
		if l == nil {
			return notFound("Lease not found")
		}
		_, err = hstUnitIn(ctx, s.st.DB, c, str(l["unit_id"]))
		return err
	}
	return bad("Unknown document owner")
}

// idDocKeys are the onboarding documents that copy an identity or bank document (Aadhaar and PAN cards, address
// proof, UAN passbook, a cancelled cheque or bank proof).
var idDocKeys = map[string]bool{"aadhaar": true, "pan": true, "addr": true, "form11": true, "cheque": true, "bank": true, "bankProof": true, "passbook": true, "passport": true}

// idDocAllowed: a candidate's or joiner's copy of an ID card is as confidential as the number on it, so a reader
// without the government ID right (a hiring or line manager) gets the résumé and the rest, not these (HIRE-093).
// The employee's own files and employee records are checked by canFiles.
func idDocAllowed(c *Ctx, kind, key string) error {
	if (kind == "candidate" || kind == "offer") && idDocKeys[key] && c.Level("emp.govid") < 1 && c.Level("onb.raise") < 1 && c.Level("onb.pipeline") < 1 {
		return forbidden("Copies of ID documents need the government ID right")
	}
	return nil
}

func (s *Server) fileUpload(r *http.Request, c *Ctx) (any, error) {
	key, err := uploadForm(r)
	if err != nil {
		return nil, err
	}
	kind, owner := r.FormValue("ownerKind"), r.FormValue("ownerId")
	ctx := r.Context()
	if err := s.canFiles(ctx, c, kind, owner, true); err != nil {
		return nil, err
	}
	// A candidate's or joiner's document is one Form setup lists, or the résumé (HIRE-073).
	if kind == "candidate" || kind == "offer" {
		if ok, err := s.st.DB.C("onb_document").Exists(ctx, bson.M{"key": key}); err != nil {
			return nil, err
		} else if !ok && key != "resume" && key != "bgvReport" { // bgvReport: the background check report (hiring.go)
			return nil, bad("Unknown document %s: choose one listed in Form setup", key)
		}
	}
	// The employee's own Form setup documents go through My documents, which checks the window, the list and that
	// a verified one is not replaced (doc_collect.go). Bills, proofs and other own uploads are not on that list.
	if kind == "employee" && owner == c.EmployeeID {
		if ok, err := s.st.DB.C("onb_document").Exists(ctx, bson.M{"key": key}); err != nil {
			return nil, err
		} else if ok {
			return nil, bad("Upload this document from My documents")
		}
	}
	f, err := uploadFile(r)
	if err != nil {
		return nil, err
	}
	row, err := s.storeDoc(ctx, c, kind, owner, key, f, c.Name+" ("+c.EmployeeID+")")
	if err == nil && kind == "candidate" { // the last mandatory joining document creates the Employee ID
		if note := s.candidateDocAdded(ctx, c, owner); note != "" {
			row["note"] = note
		}
	}
	return row, err
}

// uploadForm reads a multipart upload (5 MB at most) and its document key.
func uploadForm(r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxUpload+64<<10)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		return "", bad("The file must be 5 MB or smaller")
	}
	key := strings.TrimSpace(r.FormValue("docKey"))
	if key == "" || len(key) > 80 {
		return "", bad("Say which document this is")
	}
	return key, nil
}

// upload is a file checked against the limits: not empty, 5 MB at most, a PDF, JPG or PNG by its content.
type upload struct {
	name, ctype, ext string
	data             []byte
}

// uploadFile reads the file of a parsed upload (uploadForm) and checks it.
func uploadFile(r *http.Request) (*upload, error) {
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, bad("Choose a file to upload")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, bad("The file is empty")
	}
	if len(data) > maxUpload {
		return nil, bad("The file must be 5 MB or smaller")
	}
	ctype := http.DetectContentType(data)
	ext, ok := allowedTypes[ctype]
	if !ok {
		return nil, bad("Upload a PDF, JPG or PNG file")
	}
	name := filepath.Base(hdr.Filename)
	if len(name) > 120 {
		name = name[:120]
	}
	return &upload{name: name, ctype: ctype, ext: ext, data: data}, nil
}

// storeDoc stages the file, records it as the owner's current document of that key (an earlier one is marked
// replaced), audits it as actor and queues it for Google Drive. by is the "uploaded by" shown with the file.
func (s *Server) storeDoc(ctx context.Context, actor *Ctx, kind, owner, key string, f *upload, by string) (store.Row, error) {
	var emp *string
	switch kind {
	case "employee":
		emp = &owner
	case "offer", "candidate": // the Employee ID made for the offer or candidate, if any
		if o, _ := s.st.DB.C(kind).FindOne(ctx, bson.M{"_id": owner}); o != nil {
			if e, ok := o["employee_id"].(string); ok {
				emp = &e
			}
		}
	case "nomination":
		e, _, _ := strings.Cut(owner, "/")
		emp = &e
	}
	skey := fmt.Sprintf("%s/%s/%s-%s%s", kind, safePath(owner), time.Now().UTC().Format("20060102T150405"), randomHex(6), f.ext)
	if err := s.files.Put(ctx, skey, bytes.NewReader(f.data)); err != nil {
		return nil, err
	}
	var row store.Row
	err := s.st.Tx(ctx, func(tx store.Q) error {
		docs := tx.C("document")
		if _, err := docs.UpdateMany(ctx, bson.M{"owner_kind": kind, "owner_id": owner, "doc_key": key, "replaced_at": nil}, bson.M{"$set": bson.M{"replaced_at": tx.Now()}}); err != nil {
			return err
		}
		id, err := docs.Insert(ctx, bson.M{"owner_kind": kind, "owner_id": owner, "employee_id": emp, "doc_key": key, "file_name": f.name,
			"content_type": f.ctype, "size_bytes": len(f.data), "storage_key": skey, "uploaded_by": by})
		if err != nil {
			return err
		}
		d, err := docs.FindOne(ctx, bson.M{"_id": id})
		if err != nil {
			return err
		}
		row = store.Row{}
		for _, k := range []string{"id", "doc_key", "file_name", "content_type", "size_bytes", "uploaded_at"} {
			row[k] = d[k]
		}
		return audit(ctx, tx, actor, "Uploaded "+f.name, "document", row["id"].(string), nil, map[string]any{"owner": kind + " " + owner, "doc": key})
	})
	if err == nil {
		s.driveQueue(ctx, row["id"].(string)) // to Google Drive in the background
	}
	return row, err
}

func safePath(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// docsOf lists the current documents of an owner, keyed by document.
func docsOf(ctx context.Context, q store.Q, kind, owner string) (map[string]store.Row, error) {
	rows, err := q.C("document").Find(ctx, bson.M{"owner_kind": kind, "owner_id": owner, "replaced_at": nil}, options.Find().
		SetProjection(bson.D{{Key: "doc_key", Value: 1}, {Key: "file_name", Value: 1}, {Key: "content_type", Value: 1}, {Key: "size_bytes", Value: 1},
			{Key: "uploaded_by", Value: 1}, {Key: "uploaded_at", Value: 1}}).
		SetSort(bson.D{{Key: "uploaded_at", Value: 1}, {Key: "_id", Value: 1}}))
	out := map[string]store.Row{}
	for _, r := range rows {
		out[r["doc_key"].(string)] = r
	}
	return out, err
}

func (s *Server) fileList(r *http.Request, c *Ctx) (any, error) {
	kind, owner := r.URL.Query().Get("ownerKind"), r.URL.Query().Get("ownerId")
	if err := s.canFiles(r.Context(), c, kind, owner, false); err != nil {
		return nil, err
	}
	return docsOf(r.Context(), s.st.DB, kind, owner)
}

// fileGet streams one document after checking access to its owner.
func (s *Server) fileGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx := r.Context()
	doc, err := s.st.DB.C("document").FindOne(ctx, bson.M{"_id": r.PathValue("id")})
	if err != nil || doc == nil {
		writeErr(w, notFound("Document not found"))
		return
	}
	kind, owner, name, ctype, skey, driveID := str(doc["owner_kind"]), str(doc["owner_id"]), str(doc["file_name"]), str(doc["content_type"]), str(doc["storage_key"]), str(doc["drive_file_id"])
	var emp *string
	if e, ok := doc["employee_id"].(string); ok {
		emp = &e
	}
	if err := s.canFiles(ctx, c, kind, owner, false); err != nil {
		// Documents gathered at onboarding also belong to the employee's record.
		if emp == nil || s.canFiles(ctx, c, "employee", *emp, false) != nil {
			writeErr(w, err)
			return
		}
	}
	if err := idDocAllowed(c, kind, str(doc["doc_key"])); err != nil {
		writeErr(w, err)
		return
	}
	if driveID != "" { // in Google Drive: streamed through the app
		s.driveStream(w, r, driveID, name, ctype)
		return
	}
	f, err := s.files.Open(ctx, skey)
	if err != nil {
		// Uploaded to Drive just now, between reading the row and opening the staged copy?
		if d, _ := s.st.DB.C("document").FindOne(ctx, bson.M{"_id": r.PathValue("id")}); d != nil && str(d["drive_file_id"]) != "" {
			s.driveStream(w, r, str(d["drive_file_id"]), name, ctype)
			return
		}
		writeErr(w, notFound("The file is missing from storage"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, f)
}

// employeeDocs lists every current document on an employee's record, whatever stage it came from.
func (s *Server) employeeDocs(r *http.Request, c *Ctx) (any, error) {
	id := r.PathValue("id")
	if err := s.canFiles(r.Context(), c, "employee", id, false); err != nil {
		return nil, err
	}
	return filesOfEmployee(r.Context(), s.st.DB, id)
}

// filesOfEmployee lists the current documents of an employee, oldest first, each labelled with the onboarding
// document's name when it came from an offer or candidate (a résumé is "Résumé"; otherwise the document key).
func filesOfEmployee(ctx context.Context, q store.Q, id string) ([]store.Row, error) {
	docs, err := q.C("document").Find(ctx, bson.M{"replaced_at": nil, "$or": bson.A{bson.M{"employee_id": id}, bson.M{"owner_kind": "employee", "owner_id": id}}},
		options.Find().SetSort(bson.D{{Key: "uploaded_at", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	labels, err := ctxIndex(ctx, q, "onb_document", "key", ctxValues(docs, "doc_key"))
	if err != nil {
		return nil, err
	}
	out := make([]store.Row, len(docs))
	for i, d := range docs {
		label := d["doc_key"]
		// Onboarding documents, and the ESIC and medical cards HR loads onto the record (b1_bulk.go), by their label.
		if od := labels[str(d["doc_key"])]; od != nil && (d["owner_kind"] == "offer" || d["owner_kind"] == "candidate" || store.CardDocKey(str(d["doc_key"]))) {
			label = od["label"]
		} else if d["doc_key"] == "resume" {
			label = "Résumé"
		}
		out[i] = store.Row{"label": label}
		for _, k := range []string{"id", "owner_kind", "doc_key", "file_name", "content_type", "size_bytes", "uploaded_by", "uploaded_at"} {
			out[i][k] = d[k]
		}
	}
	return out, nil
}
