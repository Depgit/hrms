package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/policy"
	"github.com/saltstayz/hrms/internal/store"
)

// Income tax (spec 16): declarations and proofs in windows set by Payroll, proof review,
// finalisation, TDS deposits, quarterly returns 24Q / 26Q and Form 16.

func fyParam(ctx context.Context, q store.Q, r *http.Request) string {
	if fy := r.URL.Query().Get("fy"); regexp.MustCompile(`^\d{4}-\d{2}$`).MatchString(fy) {
		return fy
	}
	return calc.FY(openYm(ctx, q))
}

// fyBase selects everyone employed at some point in the financial year: not pre-boarding, joined by its last
// day, and not gone before its first (a filter on employee documents).
func fyBase(fy string) bson.M {
	ms := calc.FYMonths(fy)
	return bson.M{"status": bson.M{"$ne": "Pre-boarding"}, "doj": bson.M{"$lte": lastDay(ms[11])},
		"$or": bson.A{bson.M{"lwd": nil}, bson.M{"lwd": bson.M{"$gte": ms[0] + "-01"}}}}
}

// taxPermanentInScope selects the permanent staff in the caller's scope (the Payroll tax and Form 16 lists).
func taxPermanentInScope(ctx context.Context, q store.Q, c *Ctx) (bson.M, error) {
	f, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	return bson.M{"$and": bson.A{f, bson.M{"type": "Permanent"}}}, nil
}

// taxInt reads a count from an aggregation ($sum gives int32 or int64).
func taxInt(v any) int {
	switch x := v.(type) {
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// taxTime is a nullable timestamp read from the store.
func taxTime(v any) *time.Time {
	if t, ok := v.(time.Time); ok {
		return &t
	}
	return nil
}

// taxText is a nullable text read from the store.
func taxText(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func lastDay(ym string) string { return ymTime(ym).AddDate(0, 1, -1).Format("2006-01-02") }

// taxView is one person's tax page: declaration, proofs, both regimes, TDS so far.
func (s *Server) taxView(ctx context.Context, id, fy string) (map[string]any, error) {
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, openYm(ctx, s.st.DB), false, fyBase(fy), bson.M{"_id": id})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("Not employed in this financial year")
	}
	e := list[0]
	d := pc.tax.declOf(id)
	proofs, err := taxProofs(ctx, s.st.DB, id, fy)
	if err != nil {
		return nil, err
	}
	hist := []map[string]any{}
	for _, m := range calc.FYMonths(fy) {
		if v, ok := pc.tax.hist[id][m]; ok {
			hist = append(hist, map[string]any{"ym": m, "amount": v})
		}
	}
	var monthNow float64
	f := pc.figureFor(e, fy)
	if f != nil {
		monthNow = monthTds(f, d, pc.tax.hist[id], openYm(ctx, s.st.DB))
	}
	return map[string]any{"employee": map[string]any{"id": e.ID, "name": e.Name, "type": e.Type, "city": pc.cities[e.CityID]}, "fy": fy, "applies": salaryTdsType(e.Type),
		"windows": taxWindowsFor(ctx, s.st.DB, fy), "items": taxItemsList(ctx, s.st.DB), "groups": calc.TaxGroupNames, "caps": calc.SectionCaps, "declaration": d,
		"proofs": proofs, "figure": f, "tds": hist, "tdsThisMonth": monthNow, "regimeLock": loadRegimeLock(ctx, s.st.DB, id, fy)}, nil
}

// taxProofs is a person's proofs for the year in upload order. at is the upload time as the prototype shows it
// ("20 Oct, 10:20"); file_name is the uploaded file's name ("" when the document is gone).
func taxProofs(ctx context.Context, q store.Q, id, fy string) ([]store.Row, error) {
	list, err := q.C("tax_proof").Find(ctx, bson.M{"employee_id": id, "fy": fy}, options.Find().SetSort(compOrder("_id")))
	if err != nil {
		return nil, err
	}
	docIDs := make([]any, 0, len(list))
	for _, p := range list {
		docIDs = append(docIDs, p["doc"])
	}
	docs, err := q.C("document").Find(ctx, bson.M{"_id": bson.M{"$in": docIDs}}, options.Find().SetProjection(compFields("id", "file_name")))
	if err != nil {
		return nil, err
	}
	files := compRowsByID(docs)
	out := make([]store.Row, 0, len(list))
	for _, p := range list {
		at := ""
		if t, ok := p["at"].(time.Time); ok {
			at = t.In(store.Loc()).Format("02 Jan, 15:04")
		}
		out = append(out, store.Row{"id": p["id"], "item": p["item"], "doc": p["doc"], "amount": p["amount"], "remark": p["remark"], "status": p["status"],
			"approved": p["approved"], "note": p["note"], "at": at, "file_name": str(files[str(p["doc"])]["file_name"]), "reviewed_by": p["reviewed_by"]})
	}
	return out, nil
}

// ---------- The regime, chosen once a financial year ----------

// regimeLock is a person's regime choice for the year. It is locked once the declaration is saved, and the
// choice opens again with the next financial year (1 April). Payroll or HR Admin can unlock it for corrections.
type regimeLock struct {
	Locked     bool       `json:"locked"`
	At         *time.Time `json:"at"`
	NextFrom   string     `json:"nextFrom"` // the first day of the next financial year
	UnlockedAt *time.Time `json:"unlockedAt"`
	UnlockedBy *string    `json:"unlockedBy"`
	Reason     *string    `json:"reason"`
}

func loadRegimeLock(ctx context.Context, q store.Q, id, fy string) regimeLock {
	l := regimeLock{NextFrom: fyNextStart(fy)}
	if d, _ := q.C("tax_declaration").FindOne(ctx, bson.M{"employee_id": id, "fy": fy},
		options.FindOne().SetProjection(compFields("regime_locked_at", "regime_unlocked_at", "regime_unlocked_by", "regime_unlock_reason"))); d != nil {
		l.At, l.UnlockedAt, l.UnlockedBy, l.Reason = taxTime(d["regime_locked_at"]), taxTime(d["regime_unlocked_at"]), taxText(d["regime_unlocked_by"]), taxText(d["regime_unlock_reason"])
	}
	l.Locked = l.At != nil
	return l
}

// fyNextStart is 1 April after the financial year: 2027-04-01 for 2026-27.
func fyNextStart(fy string) string {
	y, _ := strconv.Atoi(fy[:4])
	return fmt.Sprintf("%d-04-01", y+1)
}

// regimeLockedMsg: "You chose the new regime for FY 2026-27. You can choose again from 1 April 2027."
func regimeLockedMsg(regime, fy string) string {
	t, _ := time.Parse("2006-01-02", fyNextStart(fy))
	return fmt.Sprintf("You chose the %s regime for FY %s. You can choose again from %s.", strings.ToLower(regime), fy, t.Format("2 January 2006"))
}

// taxRegimeUnlock lets Payroll or HR Admin open one person's regime choice again for the year, with a reason.
// The employee then chooses again (while the declaration window is open), and the new choice locks again.
func (s *Server) taxRegimeUnlock(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.claims") < 2 && c.Level("pay.run") < 2 {
		return nil, forbidden("Only Payroll or HR Admin can unlock a regime choice")
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Reason = strings.TrimSpace(b.Reason); len(b.Reason) < 3 {
		return nil, bad("Give the reason for unlocking the regime choice")
	}
	ctx, id := r.Context(), r.PathValue("id")
	if id == c.EmployeeID {
		return nil, forbidden("You cannot unlock your own regime choice")
	}
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	fy := fyParam(ctx, s.st.DB, r)
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		cur, err := tx.C("tax_declaration").Lock(ctx, bson.M{"employee_id": id, "fy": fy})
		if err != nil {
			return err
		}
		if cur == nil {
			return bad("No regime has been chosen for FY %s yet", fy)
		}
		regime, locked, final := str(cur["regime"]), taxTime(cur["regime_locked_at"]), taxTime(cur["final_at"])
		if final != nil {
			return bad("Tax for FY %s is finalised", fy)
		}
		if locked == nil {
			return bad("The regime choice for FY %s is already open", fy)
		}
		if _, err := tx.C("tax_declaration").Update(ctx, bson.M{"employee_id": id, "fy": fy}, bson.M{"$set": bson.M{"regime_locked_at": nil,
			"regime_unlocked_at": tx.Now(), "regime_unlocked_by": c.Name, "regime_unlock_reason": b.Reason, "updated_at": tx.Now()}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Tax regime choice unlocked for FY "+fy, "tax_declaration", id+"/"+fy,
			map[string]any{"regime": regime, "lockedAt": locked}, map[string]any{"reason": b.Reason})
	})
}

func (s *Server) taxMe(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	return s.taxView(ctx, c.EmployeeID, fyParam(ctx, s.st.DB, r))
}

var rePANx = regexp.MustCompile(`^[A-Z]{5}\d{4}[A-Z]$`)

func (s *Server) taxSave(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Regime       string             `json:"regime"`
		Items        map[string]float64 `json:"items"`
		LandlordName string             `json:"landlordName"`
		LandlordPAN  string             `json:"landlordPan"`
		OtherIncome  float64            `json:"otherIncome"`
		PrevIncome   float64            `json:"prevIncome"`
		PrevTDS      float64            `json:"prevTds"`
		Submit       bool               `json:"submit"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	typ := taxEmployeeType(ctx, s.st.DB, c.EmployeeID)
	if !salaryTdsType(typ) {
		return nil, bad("%s staff do not declare tax with the company", typ)
	}
	w := taxWindowsFor(ctx, s.st.DB, fy)
	if !w.DeclOpen {
		return nil, bad("The declaration window is closed (%s to %s)", longDate(w.DeclFrom), longDate(w.DeclTo))
	}
	if b.Regime != "New" && b.Regime != "Old" {
		return nil, bad("Choose the new or the old regime")
	}
	known := map[string]bool{}
	for _, it := range calc.TaxItems {
		known[it.ID] = true
	}
	items := map[string]float64{}
	for k, v := range b.Items {
		if !known[k] {
			return nil, bad("Unknown declaration line %s", k)
		}
		if v < 0 || math.IsNaN(v) {
			return nil, bad("Amounts cannot be negative")
		}
		if v > 0 {
			items[k] = calc.Round2(v)
		}
	}
	if b.OtherIncome < 0 || b.PrevIncome < 0 || b.PrevTDS < 0 {
		return nil, bad("Amounts cannot be negative")
	}
	b.LandlordPAN = strings.ToUpper(strings.TrimSpace(b.LandlordPAN))
	if items["hra"] > 0 && strings.TrimSpace(b.LandlordName) == "" {
		return nil, bad("Give the landlord's name for the rent paid")
	}
	if items["hra"] > 100000 && !rePANx.MatchString(b.LandlordPAN) {
		return nil, bad("Rent above ₹1,00,000 a year needs the landlord's PAN")
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		key := bson.M{"employee_id": c.EmployeeID, "fy": fy}
		cur, err := tx.C("tax_declaration").Lock(ctx, key)
		if err != nil {
			return err
		}
		if cur != nil && cur["final_at"] != nil {
			return bad("Your tax for %s is finalised", fy)
		}
		// The regime is chosen once a financial year: saving the declaration locks it (Payroll can unlock it).
		if cur != nil && cur["regime_locked_at"] != nil && b.Regime != str(cur["regime"]) {
			return bad("%s", regimeLockedMsg(str(cur["regime"]), fy))
		}
		status := "Draft"
		if b.Submit {
			status = "Declared"
		}
		now := tx.Now()
		var submitted any
		if status == "Declared" {
			submitted = now
		}
		set := bson.M{"regime": b.Regime, "status": status, "items": items, "landlord_name": strings.TrimSpace(b.LandlordName), "landlord_pan": b.LandlordPAN,
			"other_income": b.OtherIncome, "prev_income": b.PrevIncome, "prev_tds": b.PrevTDS, "submitted_at": submitted, "regime_locked_at": now, "updated_at": now}
		if cur != nil {
			// A declaration once submitted stays submitted; the first submission and the lock keep their times.
			if cur["status"] == "Declared" {
				set["status"] = "Declared"
			}
			if cur["submitted_at"] != nil {
				set["submitted_at"] = cur["submitted_at"]
			}
			if cur["regime_locked_at"] != nil {
				set["regime_locked_at"] = cur["regime_locked_at"]
			}
		}
		if _, err := tx.C("tax_declaration").Upsert(ctx, key, set, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Tax declaration saved", "tax_declaration", c.EmployeeID+"/"+fy, nil, b)
	})
}

// taxEmployeeType is an employee's type ("" when there is no such employee).
func taxEmployeeType(ctx context.Context, q store.Q, id string) string {
	e, _ := q.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(compFields("type")))
	return str(e["type"])
}

// taxFinalised reports whether a person's tax for the year is finalised.
func taxFinalised(ctx context.Context, q store.Q, id, fy string) (bool, error) {
	d, err := q.C("tax_declaration").FindOne(ctx, bson.M{"employee_id": id, "fy": fy}, options.FindOne().SetProjection(compFields("final_at")))
	return d != nil && d["final_at"] != nil, err
}

func (s *Server) taxProofAdd(r *http.Request, c *Ctx) (any, error) {
	var b struct {
		Item   string  `json:"item"`
		Doc    string  `json:"doc"`
		Amount float64 `json:"amount"`
		Remark string  `json:"remark"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	w := taxWindowsFor(ctx, s.st.DB, fy)
	if !w.ProofOpen {
		return nil, bad("The proof window is closed (%s to %s)", longDate(w.ProofFrom), longDate(w.ProofTo))
	}
	ok := false
	for _, it := range calc.TaxItems {
		ok = ok || it.ID == b.Item
	}
	if !ok {
		return nil, bad("Choose what the proof is for")
	}
	if b.Amount <= 0 {
		return nil, bad("Enter the amount the proof shows")
	}
	if found, _ := s.st.DB.C("document").Exists(ctx, bson.M{"_id": b.Doc, "owner_kind": "employee", "owner_id": c.EmployeeID}); !found {
		return nil, bad("Upload the proof document")
	}
	var id int64
	err := s.st.Tx(ctx, func(tx store.Q) error {
		final, err := taxFinalised(ctx, tx, c.EmployeeID, fy)
		if err != nil {
			return err
		}
		if final {
			return bad("Your tax for %s is finalised", fy)
		}
		// One document is one proof: sent twice it could count twice (SET-127).
		if dup, _ := tx.C("tax_proof").Exists(ctx, bson.M{"employee_id": c.EmployeeID, "fy": fy, "doc": b.Doc, "status": bson.M{"$nin": bson.A{"Withdrawn", "Rejected"}}}); dup {
			return bad("This document is already sent as a proof. Withdraw that one first, or attach a different document")
		}
		newID, err := tx.C("tax_proof").Insert(ctx, bson.M{"employee_id": c.EmployeeID, "fy": fy, "item": b.Item, "doc": b.Doc, "amount": b.Amount,
			"remark": strings.TrimSpace(b.Remark)})
		if err != nil {
			return err
		}
		id, _ = newID.(int64)
		return audit(ctx, tx, c, "Tax proof uploaded", "tax_proof", strconv.FormatInt(id, 10), nil, b)
	})
	return map[string]int64{"id": id}, err
}

func (s *Server) taxProofWithdraw(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	var n int64
	if pid, ok := compIntID(r.PathValue("id")); ok {
		var err error
		if n, err = s.st.DB.C("tax_proof").Update(ctx, bson.M{"_id": pid, "employee_id": c.EmployeeID, "status": "Pending"}, bson.M{"$set": bson.M{"status": "Withdrawn"}}); err != nil {
			return nil, err
		}
	}
	if n == 0 {
		return nil, bad("Only your own proof waiting for review can be withdrawn")
	}
	return nil, nil
}

// ---------- Payroll side ----------

func (s *Server) canTax(c *Ctx, min int) error {
	if c.Level("pay.run") >= min || c.Level("pay.claims") >= min {
		return nil
	}
	return forbidden("")
}

func declStatus(d *taxDecl, pending, reviewed int) string {
	switch {
	case d.Final != nil:
		return "Finalised"
	case d.Status == "Draft" && len(d.Items) == 0:
		return "Not declared"
	case d.Status == "Draft":
		return "Not submitted"
	case pending > 0:
		return "Proofs to review"
	case reviewed > 0:
		return "Proofs reviewed"
	}
	return "Declared"
}

// taxList is Payroll's view of every permanent employee's declaration in the year.
func (s *Server) taxList(r *http.Request, c *Ctx) (any, error) {
	if err := s.canTax(c, 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	who, err := taxPermanentInScope(ctx, s.st.DB, c)
	if err != nil {
		return nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, openYm(ctx, s.st.DB), false, fyBase(fy), who)
	if err != nil {
		return nil, err
	}
	// Proofs per person: waiting for review, and reviewed (approved or rejected).
	counts := map[string][2]int{}
	rows, err := s.st.DB.C("tax_proof").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"fy": fy}},
		bson.M{"$group": bson.M{"_id": "$employee_id",
			"pending":  bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$status", "Pending"}}, 1, 0}}},
			"reviewed": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$in": bson.A{"$status", bson.A{"Approved", "Rejected"}}}, 1, 0}}}}},
	})
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		counts[str(x["id"])] = [2]int{taxInt(x["pending"]), taxInt(x["reviewed"])}
	}
	out := []map[string]any{}
	stat := map[string]int{}
	for _, e := range list {
		d := pc.tax.declOf(e.ID)
		f := pc.figureFor(e, fy)
		st := declStatus(d, counts[e.ID][0], counts[e.ID][1])
		stat[st]++
		declared := 0.0
		for _, v := range d.Items {
			declared += v
		}
		paid := 0.0
		for _, v := range pc.tax.hist[e.ID] {
			paid += v
		}
		row := map[string]any{"id": e.ID, "name": e.Name, "dept": e.Dept, "location": e.Location, "regime": d.Regime, "status": st, "declared": declared,
			"pending": counts[e.ID][0], "tdsPaid": paid}
		if f != nil {
			row["annual"], row["newTax"], row["oldTax"] = f.Annual, f.Est.NewTax, f.Est.OldTax
			// Deductions counted (after limits, with PF and the HRA exemption) on the declaration and on approved proofs.
			row["declaredDed"] = math.Round(f.Declared.OldDeductions + f.Declared.HraExempt)
			row["approved"] = math.Round(f.Approved.OldDeductions + f.Approved.HraExempt)
			row["diff"] = calc.Round2(func() float64 {
				if d.Regime == "Old" {
					return f.Approved.OldTax - f.Declared.OldTax
				}
				return f.Approved.NewTax - f.Declared.NewTax
			}())
		}
		if q := r.URL.Query().Get("status"); q != "" && q != st {
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return map[string]any{"fy": fy, "rows": out, "counts": stat, "windows": taxWindowsFor(ctx, s.st.DB, fy),
		"canReview": c.Level("pay.claims") >= 2 || c.Level("pay.run") >= 2, "canFinalise": c.Level("pay.approve") >= 2, "canSetup": c.Level("pay.setup") >= 2}, nil
}

func (s *Server) taxEmployee(r *http.Request, c *Ctx) (any, error) {
	if err := s.canTax(c, 1); err != nil {
		return nil, err
	}
	ctx, id := r.Context(), r.PathValue("id")
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	v, err := s.taxView(ctx, id, fyParam(ctx, s.st.DB, r))
	if v != nil {
		v["canUnlock"] = (c.Level("pay.claims") >= 2 || c.Level("pay.run") >= 2) && id != c.EmployeeID
	}
	return v, err
}

func (s *Server) taxProofDecide(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.claims") < 2 && c.Level("pay.run") < 2 {
		return nil, forbidden("")
	}
	var b struct {
		Amount float64 `json:"amount"`
		Note   string  `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	ctx, id, act := r.Context(), r.PathValue("id"), r.PathValue("action")
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		pid, ok := compIntID(id)
		var cur store.Row
		if ok {
			var err error
			if cur, err = tx.C("tax_proof").Lock(ctx, bson.M{"_id": pid}); err != nil {
				return err
			}
		}
		if cur == nil {
			return notFound("Proof not found")
		}
		emp, status := str(cur["employee_id"]), str(cur["status"])
		amount, _ := cur["amount"].(float64)
		if emp == c.EmployeeID {
			return forbidden("You cannot review your own proof")
		}
		if err := c.MustInScope(ctx, tx, emp); err != nil {
			return err
		}
		if status != "Pending" {
			return bad("This proof is already %s", strings.ToLower(status))
		}
		switch act {
		case "approve":
			if b.Amount <= 0 {
				return bad("Enter the approved amount")
			}
			set := bson.M{"status": "Approved", "approved": math.Min(b.Amount, amount), "reviewed_by": c.Name, "reviewed_at": tx.Now()}
			if note := strings.TrimSpace(b.Note); note != "" { // a blank note keeps the one there
				set["note"] = note
			}
			if _, err := tx.C("tax_proof").Update(ctx, bson.M{"_id": pid}, bson.M{"$set": set}); err != nil {
				return err
			}
		case "reject":
			if strings.TrimSpace(b.Note) == "" {
				return bad("Give a reason to reject")
			}
			if _, err := tx.C("tax_proof").Update(ctx, bson.M{"_id": pid}, bson.M{"$set": bson.M{"status": "Rejected", "note": strings.TrimSpace(b.Note),
				"reviewed_by": c.Name, "reviewed_at": tx.Now()}}); err != nil {
				return err
			}
		default:
			return notFound("")
		}
		return audit(ctx, tx, c, "Tax proof "+act+"d", "tax_proof", id, nil, b)
	})
}

func (s *Server) taxWindowsPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		DeclFrom   string `json:"declFrom"`
		DeclTo     string `json:"declTo"`
		DeclForce  *bool  `json:"declForce"`
		ProofFrom  string `json:"proofFrom"`
		ProofTo    string `json:"proofTo"`
		ProofForce *bool  `json:"proofForce"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	for _, d := range []string{b.DeclFrom, b.DeclTo, b.ProofFrom, b.ProofTo} {
		if !isDate(d) {
			return nil, bad("Give every window's from and to dates")
		}
	}
	if b.DeclTo < b.DeclFrom || b.ProofTo < b.ProofFrom {
		return nil, bad("A window must end after it starts")
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	// The windows sit in or near their financial year: from a year before it starts (a declaration can open early)
	// to three months after it ends (SET-125).
	if ms := calc.FYMonths(fy); len(ms) == 12 {
		lo, hi := policy.AddDays(ms[0]+"-01", -365), policy.AddDays(policy.LastDayOf(ms[11]), 92)
		for _, d := range []string{b.DeclFrom, b.DeclTo, b.ProofFrom, b.ProofTo} {
			if d < lo || d > hi {
				return nil, bad("Windows for %s must fall between %s and %s", fy, longDate(lo), longDate(hi))
			}
		}
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("tax_window").Upsert(ctx, bson.M{"fy": fy}, bson.M{"decl_from": b.DeclFrom, "decl_to": b.DeclTo, "decl_force": b.DeclForce,
			"proof_from": b.ProofFrom, "proof_to": b.ProofTo, "proof_force": b.ProofForce, "updated_by": c.Name, "updated_at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Tax windows changed", "tax_window", fy, nil, b)
	})
}

// taxFinalise: after the proof window, only approved amounts count; the difference in tax is
// spread over the months left by the monthly TDS (spec 16).
func (s *Server) taxFinalise(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.approve", 2); err != nil {
		return nil, forbidden("Only a payroll approver can finalise")
	}
	var b struct {
		Force  bool   `json:"force"`
		Reason string `json:"reason"`
	}
	if err := decodeLoose(r, &b); err != nil {
		return nil, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	w := taxWindowsFor(ctx, s.st.DB, fy)
	if w.ProofOpen {
		return nil, bad("Close the proof window before finalising")
	}
	if w.Finalised != nil {
		return nil, bad("Tax for %s is already finalised", fy)
	}
	// Finalising drops every declaration to its approved proofs, so it waits for the proof window's end date. Before
	// then it needs an explicit early finalisation with a reason, kept in the audit (RUN-87).
	early := today() <= w.ProofTo
	if early && !b.Force {
		return nil, bad("The proof window runs to %s. Finalise after it ends, or confirm finalising early and give the reason", longDate(w.ProofTo))
	}
	if early && len(b.Reason) < 3 {
		return nil, bad("Give the reason for finalising before the proof window ends")
	}
	n, total := 0, 0.0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		n, total = 0, 0 // the transaction may run again
		pc, list, err := loadPayCtxOpt(ctx, tx, openYm(ctx, tx), false, fyBase(fy), bson.M{"type": "Permanent"})
		if err != nil {
			return err
		}
		for _, e := range list {
			d := pc.tax.decl[e.ID]
			if d == nil || d.Status != "Declared" || d.Final != nil {
				continue
			}
			f := pc.figureFor(e, fy)
			diff := 0.0
			if f != nil {
				diff = f.Approved.NewTax - f.Declared.NewTax
				if d.Regime == "Old" {
					diff = f.Approved.OldTax - f.Declared.OldTax
				}
			}
			if _, err := tx.C("tax_declaration").Update(ctx, bson.M{"employee_id": e.ID, "fy": fy}, bson.M{"$set": bson.M{"final_at": tx.Now(), "final_diff": calc.Round2(diff)}}); err != nil {
				return err
			}
			n++
			total += math.Max(0, diff)
		}
		if _, err := tx.C("tax_window").Upsert(ctx, bson.M{"fy": fy}, bson.M{"finalised_at": tx.Now(), "finalised_by": c.Name}, bson.M{"updated_by": c.Name}); err != nil {
			return err
		}
		detail := map[string]any{"employees": n, "extra": total}
		if early {
			detail["early"], detail["reason"] = true, b.Reason
		}
		return audit(ctx, tx, c, "Tax finalised for "+fy, "tax_window", fy, nil, detail)
	})
	return map[string]any{"employees": n, "extra": calc.Round2(total)}, err
}

// taxFinaliseReopen undoes a finalisation (the Payroll Manager, with a reason, audited): declarations count again
// as declared and employees can declare and submit proofs while the windows are open. Months already paid keep
// their TDS; the months left spread the tax again (RUN-87).
func (s *Server) taxFinaliseReopen(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.approve", 2); err != nil {
		return nil, forbidden("Only a payroll approver can reopen the finalisation")
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := decodeLoose(r, &b); err != nil {
		return nil, err
	}
	if b.Reason = strings.TrimSpace(b.Reason); len(b.Reason) < 3 {
		return nil, bad("Give the reason for reopening")
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	if taxWindowsFor(ctx, s.st.DB, fy).Finalised == nil {
		return nil, bad("Tax for %s is not finalised", fy)
	}
	n := 0
	err := s.st.Tx(ctx, func(tx store.Q) error {
		res, err := tx.C("tax_declaration").UpdateMany(ctx, bson.M{"fy": fy, "final_at": bson.M{"$ne": nil}}, bson.M{"$unset": bson.M{"final_at": "", "final_diff": ""}})
		if err != nil {
			return err
		}
		n = int(res)
		if _, err := tx.C("tax_window").Update(ctx, bson.M{"fy": fy}, bson.M{"$unset": bson.M{"finalised_at": "", "finalised_by": ""},
			"$set": bson.M{"reopened_at": tx.Now(), "reopened_by": c.Name, "reopen_reason": b.Reason}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Tax finalisation reopened for "+fy, "tax_window", fy, nil, map[string]any{"employees": n, "reason": b.Reason})
	})
	return map[string]any{"employees": n}, err
}

// ---------- TDS deposits and quarterly returns ----------

func (s *Server) tdsOverview(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	ms := calc.FYMonths(fy)
	type mrow struct {
		YM, Kind, Due, Status string
		People                int
		Amount                float64
		Deposit               store.Row
	}
	// People and TDS per month and section (s = salary, Section 192; f = fees, 194J).
	sums := map[string][2]float64{}
	rows, err := s.st.DB.C("tds_history").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"ym": bson.M{"$gte": ms[0], "$lte": ms[11]}}},
		bson.M{"$group": bson.M{"_id": bson.M{"ym": "$ym", "section": "$section"}, "n": bson.M{"$sum": 1}, "v": bson.M{"$sum": "$amount"},
			"o": bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$source", "opening"}}, 1, 0}}}}},
	})
	if err != nil {
		return nil, err
	}
	opening := map[string]bool{} // months loaded as opening TDS (deducted and deposited before go-live)
	for _, x := range rows {
		g, _ := x["id"].(map[string]any)
		v, _ := x["v"].(float64)
		k := map[string]string{"192": "s", "194J": "f"}[str(g["section"])] + str(g["ym"])
		sums[k] = [2]float64{float64(taxInt(x["n"])), compRound2(v)}
		opening[k] = taxInt(x["o"]) > 0 && taxInt(x["o"]) == taxInt(x["n"])
	}
	deps, _ := s.st.DB.C("tds_deposit").Find(ctx, bson.M{"ym": bson.M{"$gte": ms[0], "$lte": ms[11]}},
		options.Find().SetProjection(compFields("kind", "ym", "date", "bsr", "challan", "by_name")))
	dep := map[string]store.Row{}
	for _, d := range deps {
		dep[d["kind"].(string)+d["ym"].(string)] = d
	}
	out := []map[string]any{}
	for _, kind := range []string{"s", "f"} {
		for _, m := range ms {
			x := sums[kind+m]
			if x[1] == 0 && dep[kind+m] == nil {
				continue
			}
			st := "Due"
			switch {
			case dep[kind+m] != nil:
				st = "Deposited"
			case opening[kind+m]:
				st = "Before go-live"
			case x[1] == 0:
				st = "Nil"
			case today() > tdsDue(m):
				st = "Overdue"
			}
			out = append(out, map[string]any{"kind": kind, "ym": m, "people": int(x[0]), "amount": calc.Round2(x[1]), "due": tdsDue(m), "status": st, "deposit": dep[kind+m]})
		}
	}
	rets, _ := s.st.DB.C("tds_return").Find(ctx, bson.M{"fy": fy}, options.Find().SetProjection(compFields("form", "quarter", "receipt", "by_name")))
	quarters := []map[string]any{}
	for _, form := range []string{"24Q", "26Q"} {
		kind := map[string]string{"24Q": "s", "26Q": "f"}[form]
		for q := 1; q <= 4; q++ {
			amt := 0.0
			for _, m := range ms {
				if calc.Quarter(m) == q {
					amt += sums[kind+m][1]
				}
			}
			row := map[string]any{"form": form, "quarter": q, "amount": calc.Round2(amt)}
			for _, rt := range rets {
				if rt["form"] == form && rt["quarter"] == int32(q) {
					row["receipt"], row["by"] = rt["receipt"], rt["by_name"]
				}
			}
			quarters = append(quarters, row)
		}
	}
	co, _ := s.st.DB.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(compFields("tan", "pan", "legal_name", "responsible_name", "responsible_desig")))
	return map[string]any{"fy": fy, "months": out, "quarters": quarters, "deductor": co, "canEdit": c.Level("pay.approve") >= 2, "canExport": c.Level("pay.export") >= 2}, nil
}

var reBSR, reChallan = regexp.MustCompile(`^\d{7}$`), regexp.MustCompile(`^\d{5}$`)

func (s *Server) tdsDepositPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.approve", 2); err != nil {
		return nil, err
	}
	var b struct {
		Date    string `json:"date"`
		BSR     string `json:"bsr"`
		Challan string `json:"challan"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	kind, ym := r.PathValue("kind"), r.PathValue("ym")
	if (kind != "s" && kind != "f") || !isYm(ym) {
		return nil, notFound("")
	}
	if !isDate(b.Date) || !reBSR.MatchString(b.BSR) || !reChallan.MatchString(b.Challan) {
		return nil, bad("Enter the date, a 7 digit BSR code and a 5 digit challan number")
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("tds_deposit").Upsert(ctx, bson.M{"kind": kind, "ym": ym}, bson.M{"date": b.Date, "bsr": b.BSR, "challan": b.Challan, "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, "TDS deposit recorded", "tds_deposit", kind+ym, nil, b)
	})
}

func (s *Server) tdsReturnPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.approve", 2); err != nil {
		return nil, err
	}
	var b struct {
		Receipt string `json:"receipt"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	form := r.PathValue("form")
	q, _ := strconv.Atoi(r.PathValue("q"))
	if (form != "24Q" && form != "26Q") || q < 1 || q > 4 {
		return nil, notFound("")
	}
	if b.Receipt = strings.TrimSpace(b.Receipt); len(b.Receipt) < 6 {
		return nil, bad("Enter the receipt number from the filing")
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		if _, err := tx.C("tds_return").Upsert(ctx, bson.M{"fy": fy, "form": form, "quarter": q}, bson.M{"receipt": b.Receipt, "by_name": c.Name, "at": tx.Now()}, nil); err != nil {
			return err
		}
		return audit(ctx, tx, c, form+" receipt recorded", "tds_return", fmt.Sprintf("%s/%s/Q%d", fy, form, q), nil, b)
	})
}

// deductorPut saves TAN, PAN and the person responsible (deductorSave in ui2_payroll.go).
func (s *Server) deductorPut(r *http.Request, c *Ctx) (any, error) { return s.deductorSave(r, c) }

func companyTaxReady(ctx context.Context, q store.Q) bool {
	co, _ := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, options.FindOne().SetProjection(compFields("tan", "pan")))
	return str(co["tan"]) != "" && str(co["pan"]) != ""
}

// tdsReturnSheet is the 24Q (salary) or 26Q (fees) data for a quarter.
func (s *Server) tdsReturnSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := c.Need("pay.export", 2); err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	if !companyTaxReady(ctx, s.st.DB) {
		return "", nil, nil, bad("Add the company TAN and PAN in Payslip setup first")
	}
	form := r.PathValue("form")
	q, _ := strconv.Atoi(r.PathValue("q"))
	if (form != "24Q" && form != "26Q") || q < 1 || q > 4 {
		return "", nil, nil, notFound("")
	}
	fy := fyParam(ctx, s.st.DB, r)
	sec := map[string]string{"24Q": "192", "26Q": "194J"}[form]
	kind := map[string]string{"24Q": "s", "26Q": "f"}[form]
	months := []string{}
	for _, m := range calc.FYMonths(fy) {
		if calc.Quarter(m) == q {
			months = append(months, m)
		}
	}
	out, err := taxReturnRows(ctx, s.st.DB, c, form, sec, kind, fy, months)
	if err != nil {
		return "", nil, nil, err
	}
	return fmt.Sprintf("%s-%s-Q%d.xlsx", form, fy, q), []string{"Employee code", "Name", "PAN", "Section", map[string]string{"24Q": "Regime", "26Q": "Type"}[form], "Month",
		"Amount paid or credited", "Tax deducted", "Date of deduction", "Challan serial number", "BSR code", "Date of deposit"}, out, nil
}

// taxReturnRows are the return's lines: every TDS deduction of the section in the quarter's months for people in
// the caller's scope, by month and employee, with the PAN, the pay credited that month, the regime (24Q) or type
// (26Q), and the month's challan when deposited.
func taxReturnRows(ctx context.Context, q store.Q, c *Ctx, form, sec, kind, fy string, months []string) ([][]any, error) {
	hist, err := q.C("tds_history").Find(ctx, bson.M{"ym": bson.M{"$in": months}, "section": sec}, options.Find().SetSort(compOrder("ym", "employee_id")))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, t := range hist {
		ids = append(ids, str(t["employee_id"]))
	}
	scope, err := c.ScopeFilter(ctx, q, "")
	if err != nil {
		return nil, err
	}
	emps, err := q.C("employee").Find(ctx, bson.M{"$and": bson.A{scope, bson.M{"_id": bson.M{"$in": ids}}}}, options.Find().SetProjection(compFields("id", "name", "type", "department_id")))
	if err != nil {
		return nil, err
	}
	depts, err := q.C("department").Distinct(ctx, "_id", bson.M{})
	if err != nil {
		return nil, err
	}
	hasDept := map[string]bool{}
	for _, d := range depts {
		hasDept[str(d)] = true
	}
	byID := map[string]store.Row{} // the people in scope (with a department)
	for _, e := range emps {
		if hasDept[str(e["department_id"])] {
			byID[str(e["id"])] = e
		}
	}
	pans := map[string]string{}
	govs, err := q.C("employee_gov_id").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}}, options.Find().SetProjection(compFields("employee_id", "pan")))
	if err != nil {
		return nil, err
	}
	for _, g := range govs {
		pans[str(g["employee_id"])] = str(g["pan"])
	}
	paid := map[string]float64{} // ym|employee → the month's total earnings on the payroll row
	prs, err := q.C("payroll_row").Find(ctx, bson.M{"ym": bson.M{"$in": months}, "employee_id": bson.M{"$in": ids}},
		options.Find().SetProjection(compFields("ym", "employee_id", "data.totalEarn")))
	if err != nil {
		return nil, err
	}
	for _, p := range prs {
		data, _ := p["data"].(map[string]any)
		paid[str(p["ym"])+"|"+str(p["employee_id"])], _ = data["totalEarn"].(float64)
	}
	regimes := map[string]string{}
	decls, err := q.C("tax_declaration").Find(ctx, bson.M{"fy": fy, "employee_id": bson.M{"$in": ids}}, options.Find().SetProjection(compFields("employee_id", "regime")))
	if err != nil {
		return nil, err
	}
	for _, d := range decls {
		regimes[str(d["employee_id"])] = str(d["regime"])
	}
	deps, err := q.C("tds_deposit").Find(ctx, bson.M{"kind": kind, "ym": bson.M{"$in": months}})
	if err != nil {
		return nil, err
	}
	dep := map[string]store.Row{}
	for _, d := range deps {
		dep[str(d["ym"])] = d
	}
	out := [][]any{}
	for _, t := range hist {
		e := byID[str(t["employee_id"])]
		if e == nil {
			continue
		}
		ym := str(t["ym"])
		mode := any(or(regimes[str(e["id"])], "New"))
		if form == "26Q" {
			mode = e["type"]
		}
		d := dep[ym] // nil cells when the month is not deposited yet
		out = append(out, []any{t["employee_id"], e["name"], pans[str(e["id"])], sec, mode, ym, math.Round(paid[ym+"|"+str(e["id"])]), t["amount"], lastDay(ym),
			d["challan"], d["bsr"], d["date"]})
	}
	return out, nil
}
