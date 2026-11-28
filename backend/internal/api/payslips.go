package api

import (
	"context"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Payslips (spec 15.1 #6): a snapshot written when the month is published, shown with the
// payslip setup (company name, address, logo, accent, codes, footer). Retainer and
// Contractual staff get no payslip document.

type payslipSetup struct {
	Accent       string `json:"accent"`
	Footer       string `json:"footer"`
	ShowEmployer bool   `json:"showEmployer"`
	Website      string `json:"website,omitempty"`
	Email        string `json:"email,omitempty"` // company email and phone in the header
	Phone        string `json:"phone,omitempty"`
	ShowIds      bool   `json:"showIds"` // CIN, PAN, TAN, PF and ESI codes in the header (on unless turned off)
}

type companyFacts struct {
	Name, LegalName, Address, PAN, TAN, CIN, PFCode, ESICode string
}

func companyInfo(ctx context.Context, q store.Q) (companyFacts, error) {
	var c companyFacts
	r, err := payslipsCompany(ctx, q)
	if err != nil {
		return c, err
	}
	c = companyFacts{Name: str(r["name"]), LegalName: str(r["legal_name"]), Address: str(r["address"]), PAN: str(r["pan"]), TAN: str(r["tan"]),
		CIN: str(r["cin"]), PFCode: str(r["pf_code"]), ESICode: str(r["esi_code"])}
	return c, nil
}

// payslipsCompany reads the company settings (the one company_setting record, id 1); ErrNoRows when there is none.
func payslipsCompany(ctx context.Context, q store.Q) (store.Row, error) {
	r, err := q.C("company_setting").FindOne(ctx, bson.M{"_id": 1})
	if err == nil && r == nil {
		err = store.ErrNoRows
	}
	return r, err
}

func maskTail(v string, keep int) string {
	if len(v) <= keep {
		return v
	}
	return strings.Repeat("X", len(v)-keep) + v[len(v)-keep:]
}

type slipLine struct {
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
	Note   string  `json:"note,omitempty"`
}

// payFigures are a row's figures as the register and payslip show them: to the paisa, earnings adding up to the
// stored total earnings, deductions to the stored total deductions, and the stored net (what the bank advice and a
// hold release pay). Rounding left over from the parts goes into the special allowance and the tax line (ITM-59).
type payFigures struct {
	Basic, Hra, Sa, Bonus, VarPay, Add, OE, TotE              float64
	Fee                                                       float64 // Retainer: the retainer fee in place of Basic, HRA and Special allowance
	Reimb                                                     float64 // the Reimbursement, outside the gross (reimb.go)
	PF, ESI, LWF, MealAcc, Tds, Tds194, Tds192, Rec, OD, TotD float64
	ErDed                                                     float64 // all-inclusive gross: the employer's PF, ESI, EDLI and LWF deducted (allin.go)
	Net, RoundOff, LOP, Outside                               float64
	CompDed                                                   float64 // deductions of a designed structure's components
}

func figuresOf(o *PayRowOut) payFigures {
	r2 := calc.Round2
	c := o.R
	sum := func(l []calc.Line) float64 {
		t := 0.0
		for _, x := range l {
			t += x.Amount
		}
		return r2(t)
	}
	f := payFigures{Basic: r2(c.Basic), Hra: r2(c.Hra), Bonus: r2(c.Bonus), VarPay: r2(c.VarPay), Add: sum(o.Add), OE: r2(o.OE), TotE: r2(o.TotalEarn),
		PF: r2(c.EePF), ESI: r2(c.EeESI), LWF: r2(c.LwfEe), MealAcc: r2(c.Meal + c.Accom), Tds194: r2(c.Tds), Tds192: r2(o.STds), Rec: sum(o.Ded), OD: r2(o.OD),
		TotD: r2(o.TotalDed), Net: r2(o.Net), CompDed: r2(c.CompDed), Reimb: r2(c.Reimb),
		ErDed: r2(r2(c.ErPF) + r2(c.ErESI) + r2(c.Edli) + r2(c.LwfEr))}
	if !c.AllIn {
		f.ErDed = 0
	}
	f.Sa = r2(f.TotE - f.Basic - f.Hra - f.Bonus - f.VarPay - f.Add - f.OE)
	if f.Reimb != 0 { // r2(x − 0) is x: a row without one is unchanged to the paisa
		f.Sa = r2(f.Sa - f.Reimb)
	}
	if c.Sa == 0 && math.Abs(f.Sa) < 0.05 { // no special allowance: a paisa or two of rounding is not shown as one
		f.Sa = 0
	}
	if c.Fee != 0 { // a retainer: one fee line, which takes the rounding (calc retainerOnly)
		f.Fee, f.Sa = r2(f.TotE-f.Bonus-f.VarPay-f.Add-f.OE), 0
	}
	rest := r2(f.TotD - f.PF - f.ESI - f.LWF - f.MealAcc - f.Tds194 - f.Tds192 - f.Rec - f.OD)
	if f.CompDed != 0 {
		rest = r2(rest - f.CompDed)
	}
	if f.ErDed != 0 {
		rest = r2(rest - f.ErDed)
	}
	switch {
	case rest == 0:
	case f.Tds194 != 0:
		f.Tds194 = r2(f.Tds194 + rest)
	case f.Tds192 != 0:
		f.Tds192 = r2(f.Tds192 + rest)
	case f.LWF != 0:
		f.LWF = r2(f.LWF + rest)
	}
	f.Tds = r2(f.Tds194 + f.Tds192)
	// Net pay is in whole rupees (user decision 13 Nov): the rounding off makes the row foot. A row with a negative
	// remainder pays nothing and carries it; a row saved before the rule has no rounding.
	if o.Carry == 0 {
		f.RoundOff = r2(f.Net - (f.TotE - f.TotD))
	}
	// Loss of pay apart from the days before joining or after the last working day (RUN-65).
	f.Outside = o.OutsideDays()
	f.LOP = math.Max(0, r2(float64(o.Days)-o.Paid-f.Outside))
	return f
}

// slipSnap is what the payslip shows; stored so a later change never rewrites a published payslip.
func slipSnap(ctx context.Context, q store.Q, ym string, o *PayRowOut, co companyFacts) map[string]any {
	E, D, ER := []slipLine{}, []slipLine{}, []slipLine{}
	nz := func(l *[]slipLine, label string, v float64, note string) {
		if v != 0 {
			*l = append(*l, slipLine{label, calc.Round2(v), note})
		}
	}
	c := o.R
	f := figuresOf(o)
	r2 := calc.Round2
	// The all-inclusive gross (allin.go): the bonus is a line inside the gross; variable pay is paid every month on top.
	bonusLabel, bonusNote, varNote := "Bonus", "", ""
	if c.AllIn {
		bonusLabel, bonusNote, varNote = "Statutory bonus", "Part of the gross", "Pro-rata, additional"
	}
	// The statutory bonus inside the gross comes right after the fixed earnings (owner, 26 Nov 2026); else as before.
	bonusFirst := func() {
		if c.AllIn {
			nz(&E, bonusLabel, f.Bonus, bonusNote)
		}
	}
	bonusLater := func() {
		if !c.AllIn {
			nz(&E, bonusLabel, f.Bonus, bonusNote)
		}
	}
	var compDed []slipLine
	nz(&E, "Retainer fee", f.Fee, "")
	if len(c.Comps) > 0 { // a designed structure: its components, by name
		fixed, varLines, dedLines := compSlipLines(o, f)
		for _, l := range fixed {
			nz(&E, l.Label, l.Amount, l.Note)
		}
		bonusFirst()
		nz(&E, "Reimbursement", f.Reimb, "Outside the gross")
		bonusLater()
		for _, l := range varLines {
			if l.Note == "" {
				l.Note = varNote
			}
			nz(&E, l.Label, l.Amount, l.Note)
		}
		compDed = dedLines
	} else {
		nz(&E, "Basic", f.Basic, "")
		nz(&E, "House rent allowance", f.Hra, "")
		nz(&E, "Special allowance", f.Sa, "")
		bonusFirst()
		nz(&E, "Reimbursement", f.Reimb, "Outside the gross")
		bonusLater()
		nz(&E, "Variable pay", f.VarPay, varNote)
	}
	for _, x := range o.Add {
		nz(&E, x.Label, x.Amount, x.Reason)
	}
	nz(&E, "Other earning", f.OE, o.Reason)
	nz(&D, "Provident fund", f.PF, "")
	nz(&D, "ESI", f.ESI, "")
	nz(&D, "Labour welfare fund", f.LWF, "")
	if c.AllIn { // the employer's shares, deducted from the employee (all-inclusive gross, allin.go)
		erLWF := r2(f.ErDed - r2(c.ErPF) - r2(c.ErESI) - r2(c.Edli))
		nz(&D, "Employer PF (incl. EPS)", r2(c.ErPF), "")
		nz(&D, "Employer ESI", r2(c.ErESI), "")
		nz(&D, "EDLI", r2(c.Edli), "")
		nz(&D, "Employer LWF", erLWF, "")
	}
	nz(&D, "Meal", c.Meal, "")
	nz(&D, "Accommodation", c.Accom, "")
	for _, l := range compDed {
		nz(&D, l.Label, l.Amount, l.Note)
	}
	nz(&D, "TDS", f.Tds194, tdsNote(c.TdsRate))
	nz(&D, "Income tax (TDS)", f.Tds192, "Section 192")
	for _, x := range o.Ded {
		nz(&D, x.Label, x.Amount, x.Reason)
	}
	nz(&D, "Other deduction", f.OD, o.Reason)
	if c.AllIn { // the company bears only the admin charges (and gratuity, PL encashment, medical: provisions)
		nz(&ER, "EPFO admin charges", c.Admin, "")
	} else {
		nz(&ER, "Employer provident fund", c.ErPF+c.Admin+c.Edli, "")
		nz(&ER, "Employer ESI", c.ErESI, "")
		nz(&ER, "Employer labour welfare fund", c.LwfEr, "")
	}
	var pan, uan string
	if g, _ := q.C("employee_gov_id").FindOne(ctx, bson.M{"employee_id": o.EmployeeID}); g != nil {
		pan, uan = str(g["pan"]), str(g["uan"])
	}
	// The account in force in the month (the latest that started by then), else the latest on record.
	var bank, acct string
	latest := options.FindOne().SetSort(bson.D{{Key: "from_month", Value: -1}, {Key: "_id", Value: -1}})
	if b, _ := q.C("employee_bank").FindOne(ctx, bson.M{"employee_id": o.EmployeeID, "from_month": bson.M{"$lte": ym}}, latest); b != nil {
		bank, acct = str(b["bank_name"]), str(b["account_no"])
	}
	if bank == "" {
		if b, _ := q.C("employee_bank").FindOne(ctx, bson.M{"employee_id": o.EmployeeID}, latest); b != nil {
			bank, acct = str(b["bank_name"]), str(b["account_no"])
		}
	}
	bankText := ""
	if bank != "" {
		bankText = bank + " " + maskTail(acct, 4)
	}
	snap := map[string]any{
		"days": o.Days, "paid": o.Paid, "lop": f.LOP, "outside": f.Outside, "earn": E, "ded": D, "er": ER,
		"totE": f.TotE, "totD": f.TotD, "roundOff": f.RoundOff, "net": f.Net, "words": "Rupees " + calc.RupeeWords(f.Net) + " only",
		"emp": map[string]any{"id": o.EmployeeID, "name": o.Name, "desig": o.Desig, "dept": o.Dept, "city": o.City, "location": o.Location, "doj": o.DOJ,
			"pan": maskTail(pan, 4), "uan": uan, "bank": bankText, "type": o.Type},
		"company": map[string]any{"name": co.Name, "legalName": co.LegalName, "address": co.Address, "pan": co.PAN, "tan": co.TAN, "cin": co.CIN, "pf": co.PFCode, "esi": co.ESICode},
	}
	if c.AllIn { // the gross G offered, earned this month, and the employer's shares deducted from it
		snap["allIn"] = map[string]any{"offered": calc.Round2(c.OfferedFull), "gross": calc.Round2(c.Gross), "erDed": f.ErDed}
	}
	return snap
}

func loadSlipSetup(ctx context.Context, q store.Q) (payslipSetup, *string, error) {
	p := payslipSetup{ShowIds: true} // a setup saved before the switch existed shows the codes
	r, err := payslipsCompany(ctx, q)
	if err != nil {
		return p, nil, err
	}
	_ = payengineJSON(r["payslip"], &p)
	return p, payrollText(r["logo_doc"]), nil
}

var reHex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// reCIN is a Corporate Identity Number: L or U, 5 digits (industry), 2 letters (state), 4 digits (year), 3 letters
// (company type), 6 digits.
var reCIN = regexp.MustCompile(`^[LU]\d{5}[A-Z]{2}\d{4}[A-Z]{3}\d{6}$`)

// payslipSetupGet returns the payslip setup with the company's codes (spec 15.1 #6).
func (s *Server) payslipSetupGet(r *http.Request, c *Ctx) (any, error) {
	// Payroll setup is for the pay roles; everyone else gets the header through their own payslip (SET-084).
	if c.Level("pay.run") < 1 && c.Level("pay.setup") < 1 {
		return nil, forbidden("")
	}
	ctx := r.Context()
	p, logo, err := loadSlipSetup(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	co, err := companyInfo(ctx, s.st.DB)
	if err != nil {
		return nil, err
	}
	var cutoff string
	var first *string
	if r, _ := payslipsCompany(ctx, s.st.DB); r != nil {
		cutoff, first = str(r["cutoff"]), payrollText(r["first_ym"])
	}
	started, _ := s.st.DB.C("payroll_month").Exists(ctx, nil)
	return map[string]any{"setup": p, "logo": logo, "company": co, "cutoff": cutoff, "firstYm": first, "openYm": openYm(ctx, s.st.DB),
		"canEdit": c.Level("pay.setup") >= 2, "started": started}, nil
}

func (s *Server) payslipSetupPut(r *http.Request, c *Ctx) (any, error) {
	if err := c.Need("pay.setup", 2); err != nil {
		return nil, err
	}
	var b struct {
		Setup   payslipSetup `json:"setup"`
		Logo    *string      `json:"logo"`
		CIN     string       `json:"cin"`
		PAN     string       `json:"pan"`
		TAN     string       `json:"tan"`
		PFCode  string       `json:"pfCode"`
		ESICode string       `json:"esiCode"`
		Cutoff  string       `json:"cutoff"`
		FirstYm string       `json:"firstYm"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	b.PAN, b.TAN, b.CIN = strings.ToUpper(strings.TrimSpace(b.PAN)), strings.ToUpper(strings.TrimSpace(b.TAN)), strings.ToUpper(strings.TrimSpace(b.CIN))
	switch {
	case !reHex.MatchString(b.Setup.Accent):
		return nil, bad("Accent colour must look like #0f6e6e")
	case b.PAN != "" && !rePAN.MatchString(b.PAN):
		return nil, bad("Company PAN format looks wrong (ABCDE1234F)")
	case b.TAN != "" && !reTAN.MatchString(b.TAN):
		return nil, bad("TAN format looks wrong (ABCD12345E)")
	case b.CIN != "" && !reCIN.MatchString(b.CIN): // SET-087
		return nil, bad("CIN format looks wrong: 21 characters like U55101DL2019PTC123456")
	case b.Cutoff != "calendar" && b.Cutoff != "26-25":
		return nil, bad("Cut-off must be the calendar month or 26th to 25th")
	case b.FirstYm != "" && !isYm(b.FirstYm):
		return nil, bad("First payroll month must look like 2026-10")
	case len(b.Setup.Footer) > 300:
		return nil, bad("Keep the footer under 300 characters")
	}
	if err := cleanSlipContact(&b.Setup); err != nil {
		return nil, err
	}
	ctx := r.Context()
	var logo any // the uploaded logo (none when left empty)
	if b.Logo != nil && *b.Logo != "" {
		d, err := s.st.DB.C("document").FindOne(ctx, bson.M{"_id": *b.Logo, "owner_kind": "company"})
		if err != nil || d == nil {
			return nil, bad("Upload the logo first")
		}
		if size := int64(payengineFloat(d["size_bytes"])); size > 500<<10 || !strings.HasPrefix(str(d["content_type"]), "image/") {
			return nil, bad("The logo must be a JPG or PNG of 500 KB or less")
		}
		logo = *b.Logo
	}
	started, _ := s.st.DB.C("payroll_month").Exists(ctx, nil)
	return nil, s.st.Tx(ctx, func(tx store.Q) error {
		cur, _ := tx.C("company_setting").FindOne(ctx, bson.M{"_id": 1})
		var before store.Row
		if cur != nil {
			before = store.Row{}
			for _, k := range []string{"payslip", "logo_doc", "cin", "pan", "tan", "pf_code", "esi_code", "cutoff", "first_ym"} {
				before[k] = cur[k]
			}
		}
		if or(str(cur["cutoff"]), "calendar") != or(b.Cutoff, "calendar") {
			if err := cutoffChangeErr(ctx, tx); err != nil { // ACC-17: not once a month is paid
				return err
			}
		}
		// The first payroll month is set only before payroll has run; afterwards it stays as it is.
		first := any(nil)
		if b.FirstYm != "" {
			if started {
				first = cur["first_ym"]
			} else {
				first = b.FirstYm
			}
		}
		if _, err := tx.C("company_setting").Update(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"payslip": b.Setup, "logo_doc": logo, "cin": b.CIN,
			"pan": b.PAN, "tan": b.TAN, "pf_code": strings.TrimSpace(b.PFCode), "esi_code": strings.TrimSpace(b.ESICode), "cutoff": b.Cutoff,
			"first_ym": first, "updated_at": tx.Now()}}); err != nil {
			return err
		}
		return audit(ctx, tx, c, "Payslip and payroll setup updated", "company_setting", "1", before, b)
	})
}

// myPayslips lists the caller's published payslips (or another person's, for payroll).
func (s *Server) payslipList(r *http.Request, c *Ctx) (any, error) {
	ctx := r.Context()
	id := r.URL.Query().Get("employeeId")
	if id == "" {
		id = c.EmployeeID
	}
	if id != c.EmployeeID {
		if err := c.Need("pay.run", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	var typ string
	if e, _ := s.st.DB.C("employee").FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.D{{Key: "type", Value: 1}})); e != nil {
		typ = str(e["type"])
	}
	rows, err := s.st.DB.C("payslip").Find(ctx, bson.M{"employee_id": id}, options.Find().SetSort(bson.D{{Key: "ym", Value: -1}}).
		SetProjection(bson.D{{Key: "ym", Value: 1}, {Key: "net", Value: 1}, {Key: "published_at", Value: 1}}))
	for _, r := range rows {
		r["published_at"] = payengineStamp(r["published_at"], "02 Jan 2006")
	}
	return map[string]any{"slips": rows, "docs": calc.RulesFor(calc.EmploymentType(typ)).Docs, "type": typ}, err
}

func (s *Server) payslipGet(r *http.Request, c *Ctx) (any, error) {
	ctx, ym, id := r.Context(), r.PathValue("ym"), r.PathValue("emp")
	if id != c.EmployeeID {
		if err := c.Need("pay.run", 1); err != nil {
			return nil, err
		}
		if err := c.MustInScope(ctx, s.st.DB, id); err != nil {
			return nil, err
		}
	}
	slip, err := s.st.DB.C("payslip").FindOne(ctx, bson.M{"ym": ym, "employee_id": id})
	if err != nil || slip == nil {
		return nil, notFound("No payslip for that month")
	}
	snap := slip["snap"]
	p, logo, _ := loadSlipSetup(ctx, s.st.DB)
	out := map[string]any{"ym": ym, "label": ymLabel(ym), "snap": snap, "setup": p, "logo": logo}
	if m, masked := maskSlip(c, id, snap); masked { // IDs and bank follow the viewer's groups (SET-098)
		out["snap"], out["masked"] = m, true
	}
	return out, nil
}

// payslipPreview shows the payslip as it will look, from a computed row (setup screen).
func (s *Server) payslipPreview(r *http.Request, c *Ctx) (any, error) {
	if r.URL.Query().Get("employeeId") != "" { // one person's payslip for a month (ui2_payroll.go)
		return s.payslipPreviewEmp(r, c)
	}
	if err := c.Need("pay.run", 1); err != nil {
		return nil, err
	}
	ctx := r.Context()
	ym := openYm(ctx, s.st.DB)
	// The first permanent employee in the month's dry run, else the first active permanent employee in scope.
	var id string
	if r, _ := s.st.DB.C("payroll_row").FindOne(ctx, bson.M{"ym": ym, "data.type": "Permanent"},
		options.FindOne().SetSort(bson.D{{Key: "employee_id", Value: 1}}).SetProjection(bson.D{{Key: "employee_id", Value: 1}})); r != nil {
		id = str(r["employee_id"])
	} else if l, err := scopedEmps(ctx, s.st.DB, c, bson.M{"status": "Active", "type": "Permanent"}); err == nil && len(l) > 0 {
		id = l[0].ID
	}
	if id == "" {
		return nil, bad("No permanent employee to preview a payslip for")
	}
	row, err := s.previewRow(ctx, s.st.DB, ym, id)
	if err != nil || row == nil {
		return nil, bad("No permanent employee to preview a payslip for")
	}
	co, _ := companyInfo(ctx, s.st.DB)
	p, logo, _ := loadSlipSetup(ctx, s.st.DB)
	out := map[string]any{"ym": ym, "label": ymLabel(ym), "snap": slipSnap(ctx, s.st.DB, ym, row, co), "setup": p, "logo": logo, "preview": true}
	if m, masked := maskSlip(c, id, out["snap"]); masked { // a real person's payslip, masked by the viewer's groups (SET-099)
		out["snap"], out["masked"] = m, true
	}
	return out, nil
}

// tdsNote is the payslip note of a retainer's TDS: the % of gross used (a row saved before 25 Nov 2026 has none).
func tdsNote(rate float64) string {
	if rate <= 0 {
		return "% of gross"
	}
	return strconv.FormatFloat(rate, 'f', -1, 64) + "% of gross"
}

// compSlipLines are a designed structure's payslip lines, to the paisa: the fixed earnings (the balancing component
// takes the rounding so they add up to Basic + HRA + the rest, as figuresOf has them; components not shown on the
// payslip are one "Other allowances" line), the variable amounts paid (adding up to the variable paid) and the
// component deductions (adding up to f.CompDed).
func compSlipLines(o *PayRowOut, f payFigures) (fixed, variable, ded []slipLine) {
	r2 := calc.Round2
	shown, other := 0.0, 0.0
	rem := -1
	for _, l := range o.R.Comps {
		if l.Kind != calc.CompFixed || l.Side != calc.SideEarn || l.Extra { // the Reimbursement is its own line (outside the gross)
			continue
		}
		v := r2(l.Amount)
		if !l.Payslip {
			other = r2(other + v)
			continue
		}
		fixed = append(fixed, slipLine{Label: l.Name, Amount: v})
		shown = r2(shown + v)
		if rem < 0 || l.Code == "sa" {
			rem = len(fixed) - 1
		}
	}
	if other != 0 {
		fixed = append(fixed, slipLine{Label: "Other allowances", Amount: other})
	}
	if diff := r2(f.Basic + f.Hra + f.Sa - shown - other); diff != 0 && rem >= 0 {
		fixed[rem].Amount = r2(fixed[rem].Amount + diff)
	}
	paid := 0.0
	for _, l := range o.R.Comps {
		if l.Kind == calc.CompVariable && r2(l.Amount) != 0 {
			variable = append(variable, slipLine{Label: l.Name, Amount: r2(l.Amount)})
			paid = r2(paid + r2(l.Amount))
		}
	}
	if diff := r2(f.VarPay - paid); diff != 0 {
		if len(variable) > 0 {
			variable[len(variable)-1].Amount = r2(variable[len(variable)-1].Amount + diff)
		} else {
			variable = append(variable, slipLine{Label: "Variable pay", Amount: diff})
		}
	}
	dsum, hidden := 0.0, 0.0
	for _, l := range o.R.Comps {
		if l.Side != calc.SideDed || r2(l.Amount) == 0 {
			continue
		}
		if !l.Payslip {
			hidden = r2(hidden + r2(l.Amount))
			continue
		}
		ded = append(ded, slipLine{Label: l.Name, Amount: r2(l.Amount)})
		dsum = r2(dsum + r2(l.Amount))
	}
	if hidden != 0 {
		ded = append(ded, slipLine{Label: "Other salary deductions", Amount: hidden})
	}
	if diff := r2(f.CompDed - dsum - hidden); diff != 0 && len(ded) > 0 {
		ded[len(ded)-1].Amount = r2(ded[len(ded)-1].Amount + diff)
	}
	return fixed, variable, ded
}
