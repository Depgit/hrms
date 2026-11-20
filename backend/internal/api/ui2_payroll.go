package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Round 2 payroll gaps (prototype E1): one person's payslip before it is published, runs that
// went stale, statutory files by city and the list of files created, tax reminders, the
// declaration export and item limits, deductor details, the pay day, and structure mapping
// candidates.

// decodeLoose reads an optional JSON body: an empty body leaves v as it is.
func decodeLoose(r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return bad("Could not read the request")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bad("Check the fields: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

// ---------- Payslip preview for one employee ----------

// payslipPreviewEmp is GET /api/payslip-preview?employeeId=EMP-1001&ym=2026-10 (ym defaults to the
// open month). The answer has the shape of a published payslip (GET /api/payslips/{ym}/{emp}):
// {ym, label, snap, setup, logo} plus preview and employeeId. Before the month is published the
// snap comes from the dry-run row, or is worked out live when the person has no row yet (preview
// true); once published it is the published payslip (preview false). Payroll (pay.run) and roles
// that see salaries (emp.salary) may open it for people in their scope; salary, PAN and bank
// figures follow the role's masking for roles without payroll rights.
func (s *Server) payslipPreviewEmp(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("emp.salary") < 1 {
		return nil, forbidden("")
	}
	ctx, q := r.Context(), r.URL.Query()
	id := strings.ToUpper(strings.TrimSpace(q.Get("employeeId")))
	e, err := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.D{{Key: "type", Value: 1}}))
	if err != nil || e == nil {
		return nil, notFound("Employee not found")
	}
	typ := str(e["type"])
	if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
		return nil, err
	}
	ym := q.Get("ym")
	if ym == "" {
		ym = openYm(ctx, s.st.DB)
	}
	if !isYm(ym) {
		return nil, bad("Month must look like 2026-10")
	}
	if !calc.RulesFor(calc.EmploymentType(typ)).Docs {
		return nil, bad("%s staff get no payslip", typ)
	}
	setup, logo, _ := loadSlipSetup(ctx, s.st.DB)
	out := map[string]any{"ym": ym, "label": ymLabel(ym), "setup": setup, "logo": logo, "employeeId": id, "preview": true}
	if published, err := s.st.DB.C("payslip").FindOne(ctx, bson.M{"ym": ym, "employee_id": id}); err == nil && published != nil {
		out["snap"], out["preview"] = published["snap"], false
	} else {
		row, err := s.slipRow(ctx, ym, id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, notFound(fmt.Sprintf("%s is not in the payroll for %s", id, ymLabel(ym)))
		}
		co, _ := companyInfo(ctx, s.st.DB)
		out["snap"] = slipSnap(ctx, s.st.DB, ym, row, co)
	}
	if snap, masked := maskSlip(c, id, out["snap"]); masked {
		out["snap"], out["masked"] = snap, true
	}
	return out, nil
}

// slipRow is the person's row in the month: the stored dry-run row, else computed live.
func (s *Server) slipRow(ctx context.Context, ym, id string) (*PayRowOut, error) {
	stored, err := s.st.DB.C("payroll_row").FindOne(ctx, bson.M{"ym": ym, "employee_id": id}, options.FindOne().SetProjection(bson.D{{Key: "data", Value: 1}}))
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return s.previewRow(ctx, s.st.DB, ym, id)
	}
	o := &PayRowOut{}
	return o, payengineJSON(stored["data"], o)
}

// maskSlip applies the role's masking to a payslip for callers without payroll rights.
func maskSlip(c *Ctx, owner string, snap any) (any, bool) {
	if owner == c.EmployeeID {
		return snap, false
	}
	// Running payroll shows the amounts; government IDs and bank still follow the viewer's groups (a CHRO with IDs
	// and bank Hidden sees no PAN, UAN or account, SET-098).
	full := func(g string) bool { return c.Sensitive[g] == access.Full }
	payRun := c.Level("pay.run") >= 1
	if (full("salary") || payRun) && full("govId") && full("bank") {
		return snap, false
	}
	m := toAny(snap)
	if !full("salary") && !payRun {
		for _, k := range []string{"earn", "ded", "er"} {
			if l, ok := m[k].([]any); ok {
				for _, x := range l {
					if line, ok := x.(map[string]any); ok {
						line["amount"] = c.Show("salary", line["amount"], owner)
					}
				}
			}
		}
		for _, k := range []string{"totE", "totD", "net"} {
			m[k] = c.Show("salary", m[k], owner)
		}
		m["words"] = ""
	}
	if e, ok := m["emp"].(map[string]any); ok {
		e["pan"], e["uan"], e["bank"] = c.Show("govId", e["pan"], owner), c.Show("govId", e["uan"], owner), c.Show("bank", e["bank"], owner)
	}
	return m, true
}

// ---------- A run that went stale ----------

// payStale reports whether pay lines (claims, arrears, recoveries), salary holds or Payroll's
// edits changed after the month was last calculated, so the run needs a recalculation.
func payStale(ctx context.Context, q store.Q, m *payMonth) bool {
	if m == nil || m.ComputedAt == nil || m.Status == "Approved" || m.Status == "Published" {
		return false
	}
	since := *m.ComputedAt
	changed := bson.A{bson.M{"created_at": bson.M{"$gt": since}}, bson.M{"voided_at": bson.M{"$gt": since}}}
	lines := bson.M{"ym": m.YM, "$or": changed}
	held := bson.M{"from_ym": bson.M{"$lte": m.YM}, "$or": bson.A{bson.M{"at": bson.M{"$gt": since}}, bson.M{"released_at": bson.M{"$gt": since}}, bson.M{"extended_at": bson.M{"$gt": since}}}}
	if len(m.Pilot) > 0 { // while a pilot runs, only the pilot's lines and holds count
		lines["employee_id"] = bson.M{"$in": m.Pilot}
		held["employee_id"] = bson.M{"$in": m.Pilot}
	}
	if ok, _ := q.C("ledger").Exists(ctx, lines); ok {
		return true
	}
	// An edit saved after its row was last calculated.
	if edits, _ := q.C("payroll_override").Find(ctx, bson.M{"ym": m.YM}); len(edits) > 0 {
		who := make([]any, 0, len(edits))
		for _, o := range edits {
			who = append(who, o["employee_id"])
		}
		rows, _ := q.C("payroll_row").Find(ctx, bson.M{"ym": m.YM, "employee_id": bson.M{"$in": who}},
			options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "computed_at", Value: 1}}))
		computed := map[string]time.Time{}
		for _, r := range rows {
			computed[str(r["employee_id"])], _ = r["computed_at"].(time.Time)
		}
		for _, o := range edits {
			at, _ := o["at"].(time.Time)
			if c, ok := computed[str(o["employee_id"])]; ok && at.After(c) {
				return true
			}
		}
	}
	if ok, _ := q.C("salary_hold").Exists(ctx, held); ok {
		return true
	}
	// Gross, attendance, leave, regularization, bank changes and pay setups (payfix_a.go).
	return payStaleMore(ctx, q, m, since)
}

// ---------- Statutory files: the list of files created ----------

// statMask applies the role's masking to a statutory file preview (Aadhaar, PAN, UAN and IP
// numbers, bank details, wages), as the prototype's file builder does.
func statMask(c *Ctx, file string, rows []statRow) []statRow {
	group := map[string]string{"UAN": "govId", "Aadhaar": "govId", "PAN": "govId", "IP number": "govId", "ESIC number": "govId",
		"Bank": "bank", "Account number": "bank", "Bank account": "bank", "IFSC": "bank",
		"EPF wage": "salary", "Monthly wages": "salary", "Gross wages": "salary", "EPF wages": "salary", "EPS wages": "salary", "EDLI wages": "salary",
		"EE EPF": "salary", "EPS": "salary", "ER EPF difference": "salary", "Total wages": "salary", "EPF contribution (employee)": "salary",
		"EPS contribution (employer)": "salary", "EPF difference (employer)": "salary", "Total monthly wages": "salary"}
	cols := statFiles[file].Cols
	for i := range rows {
		owner := strings.SplitN(rows[i].Key, "|", 2)[0]
		cells := append([]any{}, rows[i].Cells...)
		for j, v := range cells {
			if j < len(cols) && group[cols[j]] != "" && v != "" && v != nil {
				cells[j] = c.Show(group[cols[j]], v, owner)
			}
		}
		rows[i].Cells = cells
	}
	return rows
}

// statRecord remembers a downloaded statutory file: which file, period, city and rows, by whom.
func statRecord(ctx context.Context, q store.Q, c *Ctx, file, from, to, ym, city string, skip bool, keys []string) error {
	_, err := q.C("statutory_file").Insert(ctx, bson.M{"file": file, "from_date": payitemsNull(from), "to_date": payitemsNull(to), "ym": payitemsNull(ym),
		"city_id": payitemsNull(city), "skip_sent": skip, "rows": len(keys), "keys": keys, "by_name": c.Name, "by_role": c.Role})
	return err
}

// statPeriod is how the screen names a file's period: the month, or from_to_to.
func statPeriod(from, to, ym string) string {
	if ym != "" {
		return ym
	}
	return from + "_to_" + to
}

// statHistory is GET /api/statutory/history: the files created so far, newest first.
func (s *Server) statHistory(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.export") < 2 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	rows, err := s.st.DB.C("statutory_file").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(50))
	if err != nil {
		return nil, err
	}
	cities := map[string]string{} // the city of a file for one city
	if l, err := s.st.DB.C("city").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}})); err == nil {
		for _, c := range l {
			cities[str(c["id"])] = str(c["name"])
		}
	}
	out := []map[string]any{}
	for _, x := range rows {
		file := x["file"].(string)
		from, to, ym, city := str(x["from_date"]), str(x["to_date"]), str(x["ym"]), str(x["city_id"])
		out = append(out, map[string]any{"id": x["id"], "file": file, "name": statFiles[file].Name, "from": from, "to": to, "ym": ym,
			"period": statPeriod(from, to, ym), "cityId": city, "city": cities[city], "skipSent": x["skip_sent"],
			"rows": x["rows"], "by": x["by_name"], "role": x["by_role"], "at": payengineStamp(x["at"], "02 Jan 2006 15:04")})
	}
	return map[string]any{"files": out, "canDownload": c.Level("pay.export") >= 2 && c.CanDownload}, nil
}

// statHistorySheet is GET /api/statutory/history/{id}/sheet: the same file again, with the rows it held.
func (s *Server) statHistorySheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := c.Need("pay.export", 2); err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	// The id as the list gives it (a whole number written plainly).
	var f store.Row
	if n, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil && strconv.FormatInt(n, 10) == r.PathValue("id") {
		f, _ = s.st.DB.C("statutory_file").FindOne(ctx, bson.M{"_id": n})
	}
	if f == nil {
		return "", nil, nil, notFound("That file is not on the list")
	}
	file, from, to, ym, city := str(f["file"]), str(f["from_date"]), str(f["to_date"]), str(f["ym"]), str(f["city_id"])
	keys := payengineStrings(f["keys"])
	if _, ok := statFiles[file]; !ok {
		return "", nil, nil, notFound("")
	}
	rows, err := s.statRows(ctx, c, file, from, to, ym, city)
	if err != nil {
		return "", nil, nil, err
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	out := [][]any{}
	for _, x := range rows {
		if want[x.Key] {
			out = append(out, x.Cells)
		}
	}
	_ = audit(ctx, s.st.DB, c, "Downloaded "+statFiles[file].Name+" again", "statutory_file", r.PathValue("id"), nil, map[string]any{"rows": len(out)})
	return file + "-" + strings.ReplaceAll(statPeriod(from, to, ym), "_", "-") + ".xlsx", statFiles[file].Cols, out, nil
}

// ---------- Deductor details ----------

// deductorSave is PUT /api/tds/deductor {tan, pan, rp, rpd} (name and desig are accepted for rp
// and rpd): the deductor details on Tax and Form 16, saved by a payroll approver (pay.approve 2)
// without touching the rest of the payslip setup. Fields left out keep their value.
func (s *Server) deductorSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.approve", 2); err != nil {
		return nil, err
	}
	var b struct {
		TAN   *string `json:"tan"`
		PAN   *string `json:"pan"`
		RP    *string `json:"rp"`
		RPD   *string `json:"rpd"`
		Name  *string `json:"name"`
		Desig *string `json:"desig"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.RP == nil {
		b.RP = b.Name
	}
	if b.RPD == nil {
		b.RPD = b.Desig
	}
	set := bson.M{}
	add := func(col string, v *string, upper bool, re interface{ MatchString(string) bool }, msg string) error {
		if v == nil {
			return nil
		}
		x := strings.TrimSpace(*v)
		if upper {
			x = strings.ToUpper(x)
		}
		if re != nil && x != "" && !re.MatchString(x) {
			return bad("%s", msg)
		}
		set[col] = x
		return nil
	}
	if err := add("tan", b.TAN, true, reTAN, "TAN format looks wrong (ABCD12345E)"); err != nil {
		return nil, err
	}
	if err := add("pan", b.PAN, true, rePAN, "Company PAN format looks wrong (ABCDE1234F)"); err != nil {
		return nil, err
	}
	if err := add("responsible_name", b.RP, false, nil, ""); err != nil {
		return nil, err
	}
	if err := add("responsible_desig", b.RPD, false, nil, ""); err != nil {
		return nil, err
	}
	if len(set) == 0 {
		return nil, bad("Nothing to save")
	}
	ctx := r.Context()
	deductor := options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 0}, {Key: "tan", Value: 1}, {Key: "pan", Value: 1},
		{Key: "responsible_name", Value: 1}, {Key: "responsible_desig", Value: 1}})
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		before, _ := tx.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, deductor)
		set["updated_at"] = tx.Now()
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": set}); err != nil {
			return err
		}
		after, _ := tx.C("company_setting").FindOne(ctx, bson.M{"_id": 1}, deductor)
		return audit(ctx, tx, c, "Deductor details updated", "company_setting", "1", before, after)
	})
}

// ---------- Tax declarations: reminders, export, items and limits ----------

// taxItemOut is a declaration item with Payroll's limit and on/off.
type taxItemOut struct {
	calc.TaxItem
	Section    string  `json:"section"`
	DefaultCap float64 `json:"defaultCap"`
	Active     bool    `json:"active"`
}

// loadTaxItems lists every declaration item with the limit and on/off Payroll set.
func loadTaxItems(ctx context.Context, q store.Q) ([]taxItemOut, error) {
	set := map[string]struct {
		cap float64
		on  bool
	}{}
	rows, err := q.C("tax_item_setting").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		set[str(r["id"])] = struct {
			cap float64
			on  bool
		}{payengineFloat(r["cap"]), r["active"] == true}
	}
	out := make([]taxItemOut, 0, len(calc.TaxItems))
	for _, it := range calc.TaxItems {
		o := taxItemOut{TaxItem: it, Section: calc.TaxGroupNames[it.Group], DefaultCap: it.Cap, Active: true}
		if v, ok := set[it.ID]; ok && it.ID != "hra" {
			o.Cap, o.Active = v.cap, v.on
		}
		out = append(out, o)
	}
	return out, nil
}

// countedTaxItems are the items the tax estimate counts: those switched on, with their limits.
func countedTaxItems(l []taxItemOut) []calc.TaxItem {
	out := []calc.TaxItem{}
	for _, it := range l {
		if it.Active || it.ID == "hra" {
			out = append(out, it.TaxItem)
		}
	}
	return out
}

// taxItemsGet is GET /api/tax/items: the "Declaration items and limits" panel.
func (s *Server) taxItemsGet(r *http.Request, c *Ctx) (any, error) {
	if err := s.canTax(c, 1); err != nil {
		return nil, err
	}
	l, err := loadTaxItems(r.Context(), s.st.DB)
	if err != nil {
		return nil, err
	}
	out := []taxItemOut{}
	for _, it := range l {
		if it.ID != "hra" { // rent is declared with the landlord, not as an item with a limit
			out = append(out, it)
		}
	}
	return map[string]any{"items": out, "canEdit": c.Level("pay.claims") >= 2 || c.Level("pay.setup") >= 2}, nil
}

// taxItemsPut is PUT /api/tax/items {items: [{id, cap, active}]}: change limits (0 = only the
// section limit) and switch items on or off. Items are never deleted; amounts already declared
// stay on record, and an item switched off is no longer counted in the tax estimate.
func (s *Server) taxItemsPut(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.claims") < 2 && c.Level("pay.setup") < 2 {
		return nil, forbidden("")
	}
	var b struct {
		Items []struct {
			ID     string   `json:"id"`
			Cap    *float64 `json:"cap"`
			Active *bool    `json:"active"`
		} `json:"items"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.Items) == 0 {
		return nil, bad("Nothing to save")
	}
	ctx := r.Context()
	cur, err := loadTaxItems(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	byID := map[string]taxItemOut{}
	for _, it := range cur {
		byID[it.ID] = it
	}
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		for _, x := range b.Items {
			it, ok := byID[x.ID]
			if !ok || x.ID == "hra" {
				return bad("Unknown declaration item %s", x.ID)
			}
			after := it
			if x.Cap != nil {
				if *x.Cap < 0 || math.IsNaN(*x.Cap) || *x.Cap > 1e9 {
					return bad("A limit must be zero or more")
				}
				// Never above the statutory limit, nor without one where the section has none (SET-136): 80D
				// self is at most 25,000 by law.
				if it.DefaultCap > 0 && (*x.Cap > it.DefaultCap || (*x.Cap == 0 && calc.SectionCaps[it.Group] == 0)) {
					return bad("%s: the limit can be at most %s, the limit in law", it.Name, rupees(it.DefaultCap))
				}
				after.Cap = calc.Round2(*x.Cap)
			}
			if x.Active != nil {
				after.Active = *x.Active
			}
			if _, err := tx.C("tax_item_setting").Upsert(ctx, bson.M{"_id": x.ID}, bson.M{"cap": after.Cap, "active": after.Active, "by_name": c.Name,
				"at": tx.Now()}, nil); err != nil {
				return err
			}
			if err := audit(ctx, tx, c, "Tax declaration item changed: "+it.Name, "tax_item_setting", x.ID,
				map[string]any{"cap": it.Cap, "active": it.Active}, map[string]any{"cap": after.Cap, "active": after.Active}); err != nil {
				return err
			}
		}
		return nil
	})
}

// taxRemind is POST /api/tax/remind: remind the permanent staff in scope who have not submitted a
// declaration for the year (while the declaration window is open). The reminder is logged.
func (s *Server) taxRemind(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.claims") < 2 && c.Level("pay.run") < 2 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	if w := taxWindowsFor(ctx, s.st.DB, fy); !w.DeclOpen {
		return nil, bad("The declaration window is closed (%s to %s)", longDate(w.DeclFrom), longDate(w.DeclTo))
	}
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	// Active permanent staff in scope without a submitted (or finalised) declaration for the year, by employee ID.
	declared, err := s.st.DB.C("tax_declaration").Distinct(ctx, "employee_id", bson.M{"fy": fy,
		"$or": bson.A{bson.M{"status": "Declared"}, bson.M{"final_at": bson.M{"$ne": nil}}}})
	if err != nil {
		return nil, err
	}
	list, err := s.st.DB.C("employee").Find(ctx, payengineAnd(bson.M{"status": "Active", "type": "Permanent", "_id": bson.M{"$nin": declared}}, scope),
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	ids := idsOf(list)
	if len(ids) == 0 {
		return nil, bad("Everyone in your view has declared")
	}
	var at time.Time
	err = s.st.Tx(ctx, func(tx store.Q) error {
		at = tx.Now()
		if _, err := tx.C("tax_reminder").Insert(ctx, bson.M{"fy": fy, "employees": ids, "by_name": c.Name, "at": at}); err != nil {
			return err
		}
		// Each person gets the reminder under the bell (SET-134).
		for _, id := range ids {
			if _, err := tx.C("notice").Insert(ctx, bson.D{{Key: "to_employee_id", Value: id}, {Key: "from_employee_id", Value: strPtr(c.EmployeeID)},
				{Key: "kind", Value: "tax"}, {Key: "text", Value: taxRemindText(fy)}, {Key: "link", Value: "/me?tab=tax"}, {Key: "view", Value: "Employee"},
				{Key: "ref", Value: []string{}}}); err != nil {
				return err
			}
		}
		return audit(ctx, tx, c, fmt.Sprintf("Reminder sent to %d employees who have not declared tax", len(ids)), "tax_reminder", fy, nil, map[string]any{"count": len(ids)})
	})
	return map[string]any{"count": len(ids), "fy": fy, "at": at.Format("02 Jan 2006 15:04")}, err
}

func taxRemindText(fy string) string {
	return "Reminder: declare your tax for " + fy + " in My pay and profile › Tax declaration, so the right TDS is deducted."
}

// taxSheet is GET /api/tax/declarations.xlsx: every declaration in scope for the year, with the
// prototype's columns.
func (s *Server) taxSheet(r *http.Request, c *Ctx) (string, []string, [][]any, error) {
	if err := s.canTax(c, 1); err != nil {
		return "", nil, nil, err
	}
	ctx := r.Context()
	fy := fyParam(ctx, s.st.DB, r)
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return "", nil, nil, err
	}
	pc, list, err := loadPayCtxOpt(ctx, s.st.DB, openYm(ctx, s.st.DB), false, fyBase(fy), payengineAnd(scope, bson.M{"type": "Permanent"}))
	if err != nil {
		return "", nil, nil, err
	}
	// Each person's proofs for the year: pending, and reviewed (approved or rejected).
	counts := map[string][2]int{}
	rows, err := s.st.DB.C("tax_proof").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"fy": fy}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$employee_id"},
			{Key: "pending", Value: bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$status", "Pending"}}, 1, 0}}}},
			{Key: "reviewed", Value: bson.M{"$sum": bson.M{"$cond": bson.A{bson.M{"$in": bson.A{"$status", bson.A{"Approved", "Rejected"}}}, 1, 0}}}}}}},
	})
	if err != nil {
		return "", nil, nil, err
	}
	for _, r := range rows {
		counts[str(r["id"])] = [2]int{int(payengineFloat(r["pending"])), int(payengineFloat(r["reviewed"]))}
	}
	out := [][]any{}
	for _, e := range list {
		d := pc.tax.decl[e.ID]
		if d == nil {
			continue
		}
		f := pc.figureFor(e, fy)
		var dd, ad, dt, at, diff float64
		if f != nil {
			dd, ad = f.Declared.OldDeductions+f.Declared.HraExempt, f.Approved.OldDeductions+f.Approved.HraExempt
			dt, at = f.Declared.NewTax, f.Approved.NewTax
			if d.Regime == "Old" {
				dt, at, diff = f.Declared.OldTax, f.Approved.OldTax, math.Max(0, f.Approved.OldTax-f.Declared.OldTax)
			}
		}
		out = append(out, []any{e.ID, e.Name, d.Regime, declStatus(d, counts[e.ID][0], counts[e.ID][1]), math.Round(dd), math.Round(ad), d.Items["hra"],
			d.PrevIncome, d.PrevTDS, d.Other, dt, at, diff})
	}
	return "tax-declarations-" + fy + ".xlsx", []string{"Emp ID", "Name", "Regime", "Status", "Declared deductions", "Approved deductions", "Rent", "Previous income",
		"Previous TDS", "Other income", "Tax on declaration", "Tax on approved proofs", "To recover"}, out, nil
}

// ---------- Pay schedule: cut-off, first month, pay day ----------

var payDays = []string{"Last working day", "1st of next month", "5th of next month"}

// paySchedule is GET /api/payroll/schedule: the pay schedule settings.
func (s *Server) paySchedule(r *http.Request, c *Ctx) (any, error) {
	if c.Level("pay.run") < 1 && c.Level("pay.setup") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	set, err := payslipsCompany(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	cutoff, first, payDay := str(set["cutoff"]), payrollText(set["first_ym"]), str(set["pay_day"])
	started, _ := s.st.DB.C("payroll_month").Exists(ctx, nil)
	return map[string]any{"frequency": "Monthly", "cutoff": cutoff, "firstYm": first, "payDay": payDay, "payDays": payDays, "started": started,
		"openYm": openYm(ctx, s.st.DB), "canEdit": c.Level("pay.setup") >= 2, "iciciDebitAcc": iciciDebitAcc(set)}, nil
}

// payScheduleSave is PUT /api/payroll/schedule {cutoff?, firstYm?, payDay?}: only the fields sent
// change; the rest of the payslip setup is left alone.
func (s *Server) payScheduleSave(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Cutoff        *string `json:"cutoff"`
		FirstYm       *string `json:"firstYm"`
		PayDay        *string `json:"payDay"`
		ICICIDebitAcc *string `json:"iciciDebitAcc,omitempty"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.ICICIDebitAcc != nil {
		v := strings.TrimSpace(*b.ICICIDebitAcc)
		b.ICICIDebitAcc = &v
	}
	switch {
	case b.Cutoff == nil && b.FirstYm == nil && b.PayDay == nil && b.ICICIDebitAcc == nil:
		return nil, bad("Nothing to save")
	case b.ICICIDebitAcc != nil && !reDebitAcc.MatchString(*b.ICICIDebitAcc):
		return nil, bad("The ICICI debit account number must be 9 to 18 digits, digits only")
	case b.Cutoff != nil && *b.Cutoff != "calendar" && *b.Cutoff != "26-25":
		return nil, bad("Cut-off must be the calendar month or 26th to 25th")
	case b.FirstYm != nil && *b.FirstYm != "" && !isYm(*b.FirstYm):
		return nil, bad("First payroll month must look like 2026-10")
	case b.PayDay != nil && !containsStr(payDays, *b.PayDay):
		return nil, bad("Salary is paid on the last working day, the 1st or the 5th of the next month")
	}
	ctx := r.Context()
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		cur, _ := tx.C("company_setting").Lock(ctx, bson.M{"_id": 1})
		var before store.Row
		if cur != nil {
			before = store.Row{"cutoff": cur["cutoff"], "first_ym": cur["first_ym"], "pay_day": cur["pay_day"]}
			if b.ICICIDebitAcc != nil {
				before["icici_debit_acc"] = iciciDebitAcc(cur)
			}
		}
		set := bson.M{"updated_at": tx.Now()}
		if b.Cutoff != nil && *b.Cutoff != or(str(before["cutoff"]), "calendar") {
			if err := cutoffChangeErr(ctx, tx); err != nil {
				return err
			}
			set["cutoff"] = *b.Cutoff
		}
		if b.FirstYm != nil && *b.FirstYm != strings.TrimSpace(str(before["first_ym"])) {
			if started, _ := tx.C("payroll_month").Exists(ctx, nil); started {
				return bad("The first payroll month cannot change once payroll has run")
			}
			set["first_ym"] = payitemsNull(*b.FirstYm)
		}
		if b.PayDay != nil {
			set["pay_day"] = *b.PayDay
		}
		if b.ICICIDebitAcc != nil {
			set["icici_debit_acc"] = *b.ICICIDebitAcc
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": set}); err != nil {
			return err
		}
		action := "Pay schedule updated"
		if b.ICICIDebitAcc != nil && b.Cutoff == nil && b.FirstYm == nil && b.PayDay == nil {
			action = "Bank upload debit account updated"
		}
		return audit(ctx, tx, c, action, "company_setting", "1", before, b)
	})
}

// ---------- Structure mapping candidates ----------

// structureCandidates is GET /api/comp/structure-map/candidates?from=&date=&city=&type=: the
// active employees in scope on version `from` (default: the default version) on `date` (default
// today), with gross and their mapping history; the number of employees on each version today;
// and the recent mapping log.
func (s *Server) structureCandidates(r *http.Request, c *Ctx) (any, error) {
	if c.Level("comp.structure") < 1 && c.Level("pay.setup") < 1 && c.Level("pay.run") < 1 {
		return nil, forbidden("")
	}
	ctx, qv := r.Context(), r.URL.Query()
	date := qv.Get("date")
	if date == "" {
		date = today()
	}
	if !isDate(date) {
		return nil, bad("Give the date as 2026-11-01")
	}
	structs, err := s.st.DB.C("salary_structure").Find(ctx, nil, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "name", Value: 1}}).
		SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "is_default", Value: 1}, {Key: "status", Value: 1}}))
	if err != nil {
		return nil, err
	}
	def := ""
	names := map[string]string{} // structure names by id
	for _, x := range structs {
		if x["is_default"] == true {
			def = x["id"].(string)
		}
		names[str(x["id"])] = str(x["name"])
	}
	from := qv.Get("from")
	if from == "" {
		from = def
	}
	// The active employees in scope, by employee ID, with their city.
	scope, err := c.ScopeFilter(ctx, s.st.DB, "")
	if err != nil {
		return nil, err
	}
	people, err := loadEmps(ctx, s.st.DB, payengineAnd(bson.M{"status": "Active"}, scope))
	if err != nil {
		return nil, err
	}
	cities := map[string]string{}
	if l, err := s.st.DB.C("city").Find(ctx, nil, options.Find().SetProjection(bson.D{{Key: "name", Value: 1}})); err == nil {
		for _, x := range l {
			cities[str(x["id"])] = str(x["name"])
		}
	}
	// Their structure mappings and gross history, oldest first.
	maps, err := s.st.DB.C("structure_map").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids(people)}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	mapsOf := map[string][]store.Row{}
	for _, m := range maps {
		mapsOf[str(m["employee_id"])] = append(mapsOf[str(m["employee_id"])], m)
	}
	grossRows, err := s.st.DB.C("gross_history").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids(people)}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: 1}}).
			SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: 1}, {Key: "gross", Value: 1}}))
	if err != nil {
		return nil, err
	}
	grossOf := map[string][]store.Row{}
	for _, g := range grossRows {
		grossOf[str(g["employee_id"])] = append(grossOf[str(g["employee_id"])], g)
	}
	// on is the structure mapped on a date (the latest mapping from then or before), else the default.
	on := func(id, d string) string {
		sid := def
		for _, m := range mapsOf[id] {
			if str(m["from_date"]) <= d {
				sid = str(m["structure_id"])
			}
		}
		return sid
	}
	// grossOn is the gross of the month (the latest revision from then or before), else the first on record.
	grossOn := func(id, ym string) any {
		h := grossOf[id]
		if len(h) == 0 {
			return nil
		}
		g := h[0]["gross"]
		for _, x := range h {
			if str(x["from_month"]) <= ym {
				g = x["gross"]
			}
		}
		return g
	}
	city, typ := qv.Get("city"), qv.Get("type")
	onToday := map[string]int{}
	cands := []map[string]any{}
	for _, p := range people {
		onToday[on(p.ID, today())]++
		if on(p.ID, date) != from || (city != "" && p.CityID != city) || (typ != "" && p.Type != typ) {
			continue
		}
		var gross any
		if g := grossOn(p.ID, date[:7]); g != nil {
			gross = c.Show("salary", g, p.ID)
		}
		history := []map[string]any{}
		for _, m := range mapsOf[p.ID] {
			if n, ok := names[str(m["structure_id"])]; ok {
				history = append(history, map[string]any{"from": m["from_date"], "structureId": m["structure_id"], "structure": n})
			}
		}
		cands = append(cands, map[string]any{"id": p.ID, "name": p.Name, "type": p.Type, "cityId": p.CityID, "city": cities[p.CityID], "gross": gross,
			"history": history})
	}
	vers := []map[string]any{}
	for _, x := range structs {
		vers = append(vers, map[string]any{"id": x["id"], "name": x["name"], "isDefault": x["is_default"], "status": x["status"], "today": onToday[x["id"].(string)]})
	}
	// The recent mapping log, newest first.
	logs, err := s.st.DB.C("audit_log").Find(ctx, bson.M{"action": "Mapped people to a salary structure"},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(30))
	if err != nil {
		return nil, err
	}
	log := []map[string]any{}
	for _, l := range logs {
		a, _ := l["after"].(map[string]any)
		toName := str(a["toName"])
		if toName == "" {
			toName = names[str(l["entity_id"])]
		}
		log = append(log, map[string]any{"at": payengineStamp(l["at"], "02 Jan 2006 15:04"), "by": l["actor"], "role": str(l["actor_role"]), "count": a["count"],
			"date": a["from"], "fromName": a["fromName"], "toName": toName})
	}
	return map[string]any{"from": from, "date": date, "default": def, "versions": vers, "rows": cands, "log": log,
		"canApply": c.Level("comp.structure") >= 2 || c.Level("pay.run") >= 2}, nil
}

// taxItemsList is every declaration item with Payroll's limit and on/off (built-in ones if unreadable).
func taxItemsList(ctx context.Context, q store.Q) any {
	l, err := loadTaxItems(ctx, q)
	if err != nil {
		return calc.TaxItems
	}
	return l
}
