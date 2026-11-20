package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/store"
)

// Joining verification (wf "verify", JV-…) from Approvals: Payroll and the Payroll Manager open every joining
// document, correct a joining detail or replace a document while the request waits on them, before they approve.
//
//	GET  /api/approvals/{id}/verify-docs        the joining documents of the request (who may see the request)
//	GET  /api/approvals/{id}/verify-docs/{doc}  one of those files (who may see the request)
//	POST /api/approvals/{id}/verify-docs        replace or add one (who can act on the request now)
//	PUT  /api/approvals/{id}/verify-details     correct joining details (who can act on the request now)
//
// "May see" is the rule of GET /api/approvals/{id}: whoever can act on it now, the person it is about, whoever raised
// it, or a role with the approvals view whose data scope covers the employee. Files are served only when they belong
// to this request's offer (the candidate SI-… or the offer OFR-…), so the request never opens anyone else's file.

// verifyCase is a joining verification with its offer (as onboardingOffers lists it) and where its documents are kept.
type verifyCase struct {
	req       *Request
	offer     store.Row
	kind, own string // document owner: candidate SI-… or offer OFR-…
}

// verifyLoad reads a joining verification and its offer. act asks for "can act now"; otherwise "may see".
func (s *Server) verifyLoad(ctx context.Context, q store.Q, c *Ctx, id string, lock, act bool) (*verifyCase, error) {
	req, err := loadRequest(ctx, q, id, lock)
	if err != nil {
		return nil, err
	}
	if req.WF != "verify" {
		return nil, notFound("Request not found")
	}
	if act {
		if !canAct(ctx, q, c, req) {
			if req.Status != "Pending" {
				return nil, bad("This joining verification is already %s", strings.ToLower(req.Status))
			}
			if req.EmployeeID == c.EmployeeID || req.RaisedBy == c.EmployeeID {
				return nil, forbidden("You cannot decide your own request")
			}
			return nil, forbidden("This request is not waiting on you")
		}
	} else if err := verifyMaySee(ctx, q, c, req); err != nil {
		return nil, err
	}
	raw, err := q.C("offer").FindOne(ctx, bson.M{"verify_request_id": req.ID})
	if err != nil {
		return nil, err
	}
	offerID := str(raw["id"])
	if raw == nil {
		offerID = payloadOf(req).OfferID
	}
	if offerID == "" {
		return nil, notFound("This verification has no joining details") // imported staff: nothing to check here
	}
	rows, err := onboardingOffers(ctx, q, bson.D{{Key: "_id", Value: offerID}}, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, notFound("Offer not found")
	}
	vc := &verifyCase{req: req, offer: rows[0]}
	vc.kind, vc.own = offerDocOwner(vc.offer)
	return vc, nil
}

// verifyDocsMaySee: the joining documents (copies of ID cards and bank proof) go to whoever acts on the
// verification, the person it is about or who sent it, and otherwise only to a viewer with the government ID right:
// a Line Manager who sees the request in the team queue does not get them (HIRE-092, HIRE-093).
func verifyDocsMaySee(ctx context.Context, q store.Q, c *Ctx, req *Request) error {
	if canAct(ctx, q, c, req) || req.EmployeeID == c.EmployeeID || req.RaisedBy == c.EmployeeID || c.Level("emp.govid") >= 1 {
		return nil
	}
	return forbidden("Copies of ID documents need the government ID right")
}

// verifyMaySee is GET /api/approvals/{id}'s rule.
func verifyMaySee(ctx context.Context, q store.Q, c *Ctx, req *Request) error {
	if canAct(ctx, q, c, req) || req.EmployeeID == c.EmployeeID || req.RaisedBy == c.EmployeeID {
		return nil
	}
	if err := c.Need("appr.queue", 1); err != nil {
		return err
	}
	return c.MustInScope(ctx, q, req.EmployeeID)
}

// verifyDocRow is one joining document of a verification.
type verifyDocRow struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	State      string `json:"state"` // Mandatory | Optional | Other (on file, not asked by Onboarding setup)
	Mandatory  bool   `json:"mandatory"`
	Uploaded   bool   `json:"uploaded"`
	ID         any    `json:"id"`
	FileName   any    `json:"file_name"`
	UploadedBy any    `json:"uploaded_by"`
	UploadedAt any    `json:"uploaded_at"`
	SizeBytes  any    `json:"size_bytes"`
}

// verifyDocs lists the documents asked at offer or joining for the offer's type and department (the stricter of
// the two states), each with its current file, then any other current file of the owner (a résumé, say).
func verifyDocs(ctx context.Context, q store.Q, o store.Row, ds []onbDoc, kind, owner string) ([]verifyDocRow, int, int, error) {
	docs, err := docsOf(ctx, q, kind, owner)
	if err != nil {
		return nil, 0, 0, err
	}
	typ, dept := str(o["type"]), str(o["department"])
	out, done, total, seen := []verifyDocRow{}, 0, 0, map[string]bool{}
	add := func(key, label, state string) {
		row := verifyDocRow{Key: key, Label: label, State: state, Mandatory: state == "Mandatory"}
		if d := docs[key]; d != nil {
			row.Uploaded, row.ID, row.FileName, row.UploadedBy, row.UploadedAt, row.SizeBytes = true, d["id"], d["file_name"], d["uploaded_by"], d["uploaded_at"], d["size_bytes"]
		}
		if row.Mandatory {
			total++
			if row.Uploaded {
				done++
			}
		}
		seen[key] = true
		out = append(out, row)
	}
	for _, d := range ds {
		st := max(d.Join, d.Offer)
		if st == 0 || !d.applies(typ, dept) {
			continue
		}
		add(d.Key, d.Label, map[int]string{1: "Optional", 2: "Mandatory"}[st])
	}
	labels := map[string]string{"resume": "Résumé"}
	for _, d := range ds {
		labels[d.Key] = d.Label
	}
	extra := []string{}
	for k := range docs {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		l := labels[k]
		if l == "" {
			l = k
		}
		add(k, l, "Other")
	}
	return out, done, total, nil
}

// verifyDocList: GET /api/approvals/{id}/verify-docs: the documents, and the fields a verifier may correct.
func (s *Server) verifyDocList(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	vc, err := s.verifyLoad(ctx, s.st.DB, c, r.PathValue("id"), false, false)
	if err != nil {
		return nil, err
	}
	if err := verifyDocsMaySee(ctx, s.st.DB, c, vc.req); err != nil {
		return nil, err
	}
	fs, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	list, done, total, err := verifyDocs(ctx, s.st.DB, vc.offer, ds, vc.kind, vc.own)
	if err != nil {
		return nil, err
	}
	// fields: what Edit details may change (the UAN shows once an existing UAN is available).
	return map[string]any{"owner": map[string]string{"kind": vc.kind, "id": vc.own}, "docs": list, "mandatoryDone": done, "mandatoryTotal": total,
		"canReplace": canAct(ctx, s.st.DB, c, vc.req), "fields": verifyEditable(fs, str(vc.offer["type"]))}, nil
}

// verifyDocGet: GET /api/approvals/{id}/verify-docs/{doc} streams one file of this verification's owner.
func (s *Server) verifyDocGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx := r.Context()
	vc, err := s.verifyLoad(ctx, s.st.DB, c, r.PathValue("id"), false, false)
	if err == nil {
		err = verifyDocsMaySee(ctx, s.st.DB, c, vc.req)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	doc, err := s.st.DB.C("document").FindOne(ctx, bson.M{"_id": r.PathValue("doc"), "owner_kind": vc.kind, "owner_id": vc.own})
	if err != nil || doc == nil {
		writeErr(w, notFound("Document not found"))
		return
	}
	name, ctype, skey, driveID := str(doc["file_name"]), str(doc["content_type"]), str(doc["storage_key"]), str(doc["drive_file_id"])
	if driveID != "" {
		s.driveStream(w, r, driveID, name, ctype)
		return
	}
	f, err := s.files.Open(ctx, skey)
	if err != nil {
		if d, _ := s.st.DB.C("document").FindOne(ctx, bson.M{"_id": doc["id"]}); d != nil && str(d["drive_file_id"]) != "" {
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

// verifyDocUpload: POST /api/approvals/{id}/verify-docs (multipart: docKey, file) replaces a joining document of
// this verification through the normal upload path (storeDoc); the earlier file is kept, marked replaced.
func (s *Server) verifyDocUpload(r *http.Request, c *Ctx) (any, error) {
	key, err := uploadForm(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	vc, err := s.verifyLoad(ctx, s.st.DB, c, r.PathValue("id"), false, true)
	if err != nil {
		return nil, err
	}
	_, ds, err := loadSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	list, _, _, err := verifyDocs(ctx, s.st.DB, vc.offer, ds, vc.kind, vc.own)
	if err != nil {
		return nil, err
	}
	var doc *verifyDocRow
	for i := range list {
		if list[i].Key == key {
			doc = &list[i]
		}
	}
	if doc == nil {
		return nil, bad("That document is not asked for this joiner")
	}
	f, err := uploadFile(r)
	if err != nil {
		return nil, err
	}
	row, err := s.storeDoc(ctx, c, vc.kind, vc.own, key, f, c.Name+" ("+c.EmployeeID+")")
	if err != nil {
		return nil, err
	}
	if vc.kind == "candidate" { // as an upload in Onboarding: the last mandatory document may create the Employee ID
		if note := s.candidateDocAdded(ctx, c, vc.own); note != "" {
			row["note"] = note
		}
	}
	what := "added"
	if doc.Uploaded {
		what = "replaced"
	}
	err = s.st.Tx(ctx, func(tx store.Q) error {
		return history(ctx, tx, vc.req.ID, "", "Corrected", c.Name, fmt.Sprintf("Document %s by %s: %s (%s)", what, c.Name, doc.Label, f.name))
	})
	return row, err
}

// verifyMaskKeys are shown with only the last four characters in the history and in the bulk Excel file.
var verifyMaskKeys = map[string]bool{"aadhaar": true, "acct": true}

// verifyShow is a joining detail as people read it: masked numbers, a manager as "Name (EMP-…)", "(blank)".
func verifyShow(key, v string, names map[string]string) string {
	if v == "" {
		return "(blank)"
	}
	if verifyMaskKeys[key] {
		return fmt.Sprint(access.Mask(strings.ReplaceAll(v, " ", ""), access.Masked))
	}
	if n := names[v]; n != "" && (key == "l1" || key == "l2") {
		return n + " (" + v + ")"
	}
	return v
}

// verifyEditable are the joining fields a verifier may correct: those asked at joining for the offer's type
// (the UAN only when an existing UAN is available, decided on the values being saved).
func verifyEditable(fs []onbField, typ string) []onbField {
	out := []onbField{}
	for _, f := range fs {
		if f.Join > 0 && f.On && containsStr(f.Types, typ) && f.Kind != "flow" && f.Kind != "policy" {
			out = append(out, f)
		}
	}
	return out
}

// verifyCheck validates the joining details after the change: the formats the joining save checks (checkFormat),
// every mandatory detail filled, the UAN when one exists, a listed value for a choice, a date for a date, and an
// existing employee, shift, attendance mode or qualification for those choices.
// joinMasterErrors checks a joining detail that names a master record: a manager (an active employee), a shift, an
// attendance mode or a qualification.
func joinMasterErrors(ctx context.Context, q store.Q, f onbField, v string) []string {
	switch {
	case f.Key == "l1" || f.Key == "l2":
		if ok, _ := q.C("employee").Exists(ctx, bson.M{"_id": v, "status": bson.M{"$nin": bson.A{"Inactive", "Pre-boarding"}}}); !ok {
			return []string{f.Label + ": " + v + " is not an active employee"}
		}
	case f.Key == "shift":
		if ok, _ := q.C("shift").Exists(ctx, bson.M{"code": v}); !ok {
			return []string{f.Label + ": no shift with the code " + v}
		}
	case f.Key == "attMode" || f.Key == "qual":
		kind := map[string]string{"attMode": "attendance_mode", "qual": "qualification"}[f.Key]
		if ok, _ := q.C("simple_master").Exists(ctx, bson.M{"kind": kind, "name": v}); !ok {
			return []string{f.Label + ": " + v + " is not in the list"}
		}
	}
	return nil
}

func verifyCheck(ctx context.Context, q store.Q, fields []onbField, typ string, next map[string]string, changed map[string]bool) []string {
	errs := []string{}
	vals := map[string]string{}
	for _, f := range fields {
		vals[f.Key] = next[f.Key]
	}
	checkFormat(vals, &errs)
	for _, f := range fields {
		v := next[f.Key]
		if v == "" {
			if f.Join == 2 && f.applies(typ, next) {
				errs = append(errs, f.Label+" is required")
			}
			continue
		}
		if !changed[f.Key] {
			continue
		}
		switch {
		case f.Kind == "date" && !isDate(v):
			errs = append(errs, f.Label+" must be a date")
		case f.Kind == "sel" && f.Options != nil && strings.TrimSpace(*f.Options) != "":
			ok := false
			for _, o := range strings.Split(*f.Options, ",") {
				ok = ok || strings.TrimSpace(o) == v
			}
			if !ok {
				errs = append(errs, f.Label+": choose one of "+strings.Join(strings.Split(*f.Options, ","), ", "))
			}
		default:
			errs = append(errs, joinMasterErrors(ctx, q, f, v)...)
		}
	}
	if next["uanAvail"] == "Yes" && next["uan"] == "" && !containsStr(errs, "UAN is required") {
		errs = append(errs, "UAN is required when an existing UAN is available")
	}
	return dedupe(errs)
}

// verifyPlanned is a correction worked out but not yet written: the editable fields, the values before and after,
// and the changed keys in the setup's order.
type verifyPlanned struct {
	fields    []onbField
	byKey     map[string]onbField
	old, next map[string]string
	keys      []string
}

// verifyPlan checks a correction of a verification's joining details (in: field key → new value) and works it out.
// Problems come back as one 400 with every message in details.
func verifyPlan(ctx context.Context, q store.Q, vc *verifyCase, in map[string]string) (*verifyPlanned, error) {
	typ := str(vc.offer["type"])
	fs, _, err := loadSetup(ctx, q)
	if err != nil {
		return nil, err
	}
	p := &verifyPlanned{fields: verifyEditable(fs, typ), byKey: map[string]onbField{}, old: rowData(vc.offer), next: map[string]string{}}
	for _, f := range p.fields {
		p.byKey[f.Key] = f
	}
	label := map[string]string{}
	for _, f := range fs {
		label[f.Key] = f.Label
	}
	for k, v := range p.old {
		p.next[k] = v
	}
	refused := []string{}
	for k, v := range in {
		if _, ok := p.byKey[k]; !ok {
			refused = append(refused, or(label[k], k)+" cannot be changed while verifying")
			continue
		}
		if k == "pan" || k == "ifsc" {
			v = strings.ToUpper(v)
		}
		p.next[k] = v
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, badD(refused[0], refused)
	}
	changed := map[string]bool{}
	for _, f := range p.fields {
		if _, sent := in[f.Key]; sent && p.next[f.Key] != p.old[f.Key] {
			changed[f.Key] = true
			p.keys = append(p.keys, f.Key)
		}
	}
	if errs := verifyCheck(ctx, q, p.fields, typ, p.next, changed); len(errs) > 0 {
		return nil, badD(errs[0], errs)
	}
	return p, nil
}

// verifyNames are the names of the managers named in a correction, for showing them.
func verifyNames(ctx context.Context, q store.Q, p *verifyPlanned) map[string]string {
	names := map[string]string{}
	for _, k := range []string{"l1", "l2"} {
		for _, v := range []string{p.old[k], p.next[k]} {
			if v != "" && names[v] == "" {
				if e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": v}); e != nil {
					names[v] = str(e["name"])
				}
			}
		}
	}
	return names
}

// verifyChanges are a correction as people read it: field, before and after, with numbers masked.
func verifyChanges(ctx context.Context, q store.Q, p *verifyPlanned) []bulkChange {
	names := verifyNames(ctx, q, p)
	out := []bulkChange{}
	for _, k := range p.keys {
		out = append(out, bulkChange{Field: p.byKey[k].Label, From: verifyShow(k, p.old[k], names), To: verifyShow(k, p.next[k], names)})
	}
	return out
}

// verifyApply writes a planned correction inside tx: the offer's joining details (as the joining save writes them),
// the employee record made for the joiner (verifyPropagate), a line in the request's history and the audit log.
func (s *Server) verifyApply(ctx context.Context, tx store.Q, c *Ctx, vc *verifyCase, p *verifyPlanned, via string) (map[string]any, error) {
	if len(p.keys) == 0 {
		return map[string]any{"changed": []string{}}, nil
	}
	o := vc.offer
	set := bson.D{}
	before, after := map[string]string{}, map[string]string{}
	for _, k := range p.keys {
		set = append(set, bson.E{Key: "data." + k, Value: p.next[k]})
		before[k], after[k] = p.old[k], p.next[k]
		if verifyMaskKeys[k] { // the append-only audit log keeps no full Aadhaar or account number (HIRE-219)
			before[k], after[k] = verifyShow(k, p.old[k], nil), verifyShow(k, p.next[k], nil)
		}
	}
	if _, err := tx.C("offer").Update(ctx, bson.D{{Key: "_id", Value: o["id"]}}, bson.D{{Key: "$set", Value: set}}); err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, k := range p.keys {
		changed[k] = true
	}
	emp := str(o["employee_id"])
	if emp != "" {
		if err := verifyPropagate(ctx, tx, emp, p.old, p.next, changed, vc.req.ID); err != nil {
			return nil, err
		}
	}
	parts, labels := []string{}, []string{}
	for _, ch := range verifyChanges(ctx, tx, p) {
		labels = append(labels, ch.Field)
		parts = append(parts, fmt.Sprintf("%s %s → %s", ch.Field, ch.From, ch.To))
	}
	note := "Details corrected by " + c.Name + via + ": " + strings.Join(parts, "; ")
	if err := history(ctx, tx, vc.req.ID, "", "Corrected", c.Name, note); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, c, "Joining details corrected while verifying "+str(o["candidate_name"])+via+": "+strings.Join(labels, ", "), "approval_request", vc.req.ID,
		map[string]any{"offer": o["id"], "employee": emp, "values": before}, map[string]any{"offer": o["id"], "employee": emp, "values": after}); err != nil {
		return nil, err
	}
	return map[string]any{"changed": labels, "note": note, "employeeUpdated": emp != ""}, nil
}

// verifyDetailsPut: PUT /api/approvals/{id}/verify-details {data: {key: value}} corrects joining details while the
// verification waits on the caller. The offer's joining details change as the joining save changes them; the
// employee record made for the joiner gets the personal, ID and bank details as the joining propagates them
// (applyPersonal). The request's history and the audit log record the change.
func (s *Server) verifyDetailsPut(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Data map[string]any `json:"data"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	in := cleanData(b.Data)
	if len(in) == 0 {
		return nil, bad("Nothing to save")
	}
	ctx := r.Context()
	var out map[string]any
	err := s.st.Tx(ctx, func(tx store.Q) error {
		out = nil // the transaction may run again
		vc, err := s.verifyLoad(ctx, tx, c, r.PathValue("id"), true, true)
		if err != nil {
			return err
		}
		p, err := verifyPlan(ctx, tx, vc, in)
		if err != nil {
			return err
		}
		out, err = s.verifyApply(ctx, tx, c, vc, p, "")
		return err
	})
	return out, err
}

// verifyPropagate puts corrected details on the employee record made for the joiner, the way the joining puts them
// there (applyPersonal: personal, ID and bank). A corrected bank account replaces the one taken from the joining
// details (same employee, account and IFSC, from the joining month) rather than adding a second account.
func verifyPropagate(ctx context.Context, tx store.Q, emp string, old, next map[string]string, changed map[string]bool, reqID string) error {
	e, err := onboardingOne(ctx, tx, "employee", emp)
	if err != nil {
		return err
	}
	if e == nil {
		return nil
	}
	doj := str(e["doj"])
	if len(doj) < 7 {
		return nil
	}
	if (changed["acct"] || changed["ifsc"] || changed["bankName"]) && old["acct"] != "" && old["ifsc"] != "" &&
		next["acct"] != "" && next["ifsc"] != "" && next["bankName"] != "" {
		if _, err := tx.C("employee_bank").Update(ctx, bson.D{{Key: "employee_id", Value: emp}, {Key: "account_no", Value: old["acct"]},
			{Key: "ifsc", Value: strings.ToUpper(old["ifsc"])}, {Key: "from_month", Value: doj[:7]}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "bank_name", Value: next["bankName"]}, {Key: "account_no", Value: next["acct"]}, {Key: "ifsc", Value: strings.ToUpper(next["ifsc"])}}}}); err != nil {
			return err
		}
	}
	return applyPersonal(ctx, tx, emp, doj, next, "Verification "+reqID)
}
