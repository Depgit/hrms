package api

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/access"
	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

// Two payroll exports (owner, 14 Nov 2026), next to the bank advice and the register (payroll.go), which stay as
// they are:
//
//   - The ICICI bank upload: ICICI's bulk payment file (13 columns, one sheet), for an approved or published month,
//     paying exactly the people the bank advice pays (not on hold, net above 0). People whose account number, IFSC
//     or name cannot go to the bank are left out and listed on a second sheet "Not included" (ICICI reads only the
//     first) and by GET /api/payroll/{ym}/icici-upload, which the pages show before the download.
//   - The full salary register: everything about each person's salary for the month in one sheet, from the dry run
//     on (marked DRAFT before approval), with a totals row; IDs, bank and amounts follow the downloader's masking.
//
// Both need the register's rule (Export payroll files at Edit and the role's download right) and are audited.

var (
	reDebitAcc = regexp.MustCompile(`^\d{9,18}$`)
	reBankIFSC = regexp.MustCompile(`^[A-Z]{4}0[A-Z0-9]{6}$`)
	reBankAcc  = regexp.MustCompile(`^\d{1,32}$`)
	reTenDigit = regexp.MustCompile(`^\d{10}$`)
)

// istZone is India Standard Time: the bank file's payment date is today in India whatever zone the server runs in.
var istZone = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return l
	}
	return time.FixedZone("IST", 5*3600+1800)
}()

// iciciDebitAcc is the company's ICICI debit account from the settings, else the default the owner gave.
func iciciDebitAcc(set store.Row) string {
	if v := strings.TrimSpace(str(set["icici_debit_acc"])); v != "" {
		return v
	}
	return store.DefaultICICIDebitAcc
}

// bookOut is a workbook download: file name, the workbook, the rows it holds and what else the audit records.
type bookOut struct {
	name  string
	f     *excelize.File
	rows  int
	extra map[string]any
}

// book serves a workbook built by fn with the checks and audit of sheet (xlsx.go): signed in, the role may
// download, every download in the audit log.
func (s *Server) book(fn func(r *http.Request, c *Ctx) (*bookOut, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.authenticate(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if !c.CanDownload {
			writeErr(w, forbidden("Your role cannot download files"))
			return
		}
		b, err := fn(r, c)
		if err != nil {
			writeErr(w, err)
			return
		}
		defer b.f.Close()
		after := map[string]any{"path": r.URL.Path, "query": r.URL.RawQuery, "format": "xlsx", "rows": b.rows}
		for k, v := range b.extra {
			after[k] = v
		}
		_ = audit(r.Context(), s.st.DB, c, "Downloaded "+b.name, "download", b.name, nil, after)
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", `attachment; filename="`+b.name+`"; filename*=UTF-8''`+url.PathEscape(b.name))
		_ = b.f.Write(w)
	}
}

// bankAcct is the account a person is paid into in a month.
type bankAcct struct{ Bank, Acc, IFSC string }

// monthAccounts is each person's account in force in month ym: the latest that started by then (as the bank
// advice reads it).
func (s *Server) monthAccounts(r *http.Request, ids []string, ym string) (map[string]bankAcct, error) {
	list, err := s.st.DB.C("employee_bank").Find(r.Context(), bson.M{"employee_id": bson.M{"$in": ids}, "from_month": bson.M{"$lte": ym}},
		options.Find().SetSort(bson.D{{Key: "employee_id", Value: 1}, {Key: "from_month", Value: -1}, {Key: "_id", Value: -1}}))
	if err != nil {
		return nil, err
	}
	out := map[string]bankAcct{}
	for _, b := range list {
		id := str(b["employee_id"])
		if _, seen := out[id]; !seen {
			out[id] = bankAcct{str(b["bank_name"]), str(b["account_no"]), str(b["ifsc"])}
		}
	}
	return out, nil
}

// ---------- ICICI bank upload ----------

var iciciCols = []string{"PYMT_PROD_TYPE_CODE", "PYMT_MODE", "DEBIT_ACC_NO", "BNF_NAME", "BENE_ACC_NO", "BENE_IFSC", "AMOUNT", "CREDIT_NARR",
	"PYMT_DATE", "MOBILE_NUM", "EMAIL_ID", "REMARK", "REF_NO"}

// iciciName is a beneficiary name as ICICI takes it: letters and spaces only (dots, hyphens, commas, slashes and
// underscores part words; any other character is dropped), single spaces, trimmed, at most 500 characters.
func iciciName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
			b.WriteRune(r)
		case unicode.IsSpace(r) || strings.ContainsRune(".-_,/", r):
			b.WriteByte(' ')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > 500 {
		out = strings.TrimSpace(out[:500])
	}
	return out
}

// iciciMobile is a mobile number as its last 10 digits once +91, spaces, dashes and brackets are taken off (a
// leading 91 or 0 before the 10 digits too); blank when that does not leave exactly 10 digits.
func iciciMobile(s string) string {
	t := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", " ", "").Replace(strings.TrimSpace(s))
	t = strings.TrimPrefix(t, "+91")
	switch {
	case len(t) == 12 && strings.HasPrefix(t, "91"):
		t = t[2:]
	case len(t) == 11 && strings.HasPrefix(t, "0"):
		t = t[1:]
	}
	if !reTenDigit.MatchString(t) {
		return ""
	}
	return t
}

// iciciNarration is the credit narration for a payroll month: "Oct 26 Salary".
func iciciNarration(ym string) string { return ymTime(ym).Format("Jan 06") + " Salary" }

// iciciLine is one payment of the upload.
type iciciLine struct {
	ID, Name, Mode, Acc, IFSC, Mobile, Email string
	Amount                                   float64
}

// iciciSkip is someone the bank advice pays who cannot go into the upload, and why.
type iciciSkip struct {
	EmployeeID string  `json:"employeeId"`
	Name       string  `json:"name"`
	Amount     float64 `json:"-"`
	Reason     string  `json:"reason"`
}

// iciciPlan builds the upload of an approved or published month: the bank advice's people (in the caller's scope,
// not on hold, net above 0) with the figure the register and bank advice show, and the people left out.
func (s *Server) iciciPlan(r *http.Request, c *Ctx) (string, string, []iciciLine, []iciciSkip, error) {
	if !isYm(r.PathValue("ym")) {
		return "", "", nil, nil, bad("Month must look like 2026-10")
	}
	ym, rows, err := s.approvedRows(r, c)
	if err != nil {
		return "", "", nil, nil, err
	}
	ctx := r.Context()
	paid := []*PayRowOut{}
	ids := []string{}
	for _, o := range rows {
		if o.Hold || o.Net <= 0 {
			continue
		}
		paid = append(paid, o)
		ids = append(ids, o.EmployeeID)
	}
	banks, err := s.monthAccounts(r, ids, ym)
	if err != nil {
		return "", "", nil, nil, err
	}
	contact := map[string][2]string{}
	if pl, err := s.st.DB.C("employee_personal").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "mobile", Value: 1}, {Key: "email", Value: 1}})); err != nil {
		return "", "", nil, nil, err
	} else {
		for _, p := range pl {
			contact[str(p["employee_id"])] = [2]string{str(p["mobile"]), str(p["email"])}
		}
	}
	set, err := payslipsCompany(ctx, s.st.DB)
	if err != nil {
		return "", "", nil, nil, err
	}
	lines, skips := []iciciLine{}, []iciciSkip{}
	for _, o := range paid {
		amount := calc.Round2(o.Net)
		b, has := banks[o.EmployeeID]
		acc := strings.ReplaceAll(strings.TrimSpace(b.Acc), " ", "")
		ifsc := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(b.IFSC), " ", ""))
		name := iciciName(o.Name)
		why := []string{}
		switch {
		case !has:
			why = append(why, "No bank account on record")
		case acc == "":
			why = append(why, "Account number missing")
		case !reBankAcc.MatchString(acc):
			why = append(why, "Account number must be 1 to 32 digits, digits only")
		}
		if has {
			switch {
			case ifsc == "":
				why = append(why, "IFSC missing (needed for NEFT)")
			case !reBankIFSC.MatchString(ifsc):
				why = append(why, "IFSC "+ifsc+" is not valid (11 characters, like HDFC0001234)")
			}
		}
		if name == "" {
			why = append(why, "Name has no letters left after removing special characters")
		}
		if len(why) > 0 {
			skips = append(skips, iciciSkip{EmployeeID: o.EmployeeID, Name: o.Name, Amount: amount, Reason: strings.Join(why, "; ")})
			continue
		}
		mode := "NEFT"
		if strings.HasPrefix(ifsc, "ICIC") {
			mode = "FT" // an ICICI account: a transfer within the bank
		}
		email := strings.TrimSpace(contact[o.EmployeeID][1])
		if len(email) > 500 {
			email = ""
		}
		lines = append(lines, iciciLine{ID: o.EmployeeID, Name: name, Mode: mode, Acc: acc, IFSC: ifsc, Mobile: iciciMobile(contact[o.EmployeeID][0]),
			Email: email, Amount: amount})
	}
	return ym, iciciDebitAcc(set), lines, skips, nil
}

// payrollICICICheck is GET /api/payroll/{ym}/icici-upload: what the upload holds and who is left out, shown before
// the download.
func (s *Server) payrollICICICheck(r *http.Request, c *Ctx) (any, error) {
	ym, debit, lines, skips, err := s.iciciPlan(r, c)
	if err != nil {
		return nil, err
	}
	total := 0.0
	ft := 0
	for _, l := range lines {
		total += l.Amount
		if l.Mode == "FT" {
			ft++
		}
	}
	return map[string]any{"ym": ym, "file": iciciFileName(ym), "rows": len(lines), "ft": ft, "neft": len(lines) - ft, "amount": calc.Round2(total),
		"debitAcc": debit, "narration": iciciNarration(ym), "notIncluded": skips}, nil
}

func iciciFileName(ym string) string {
	return "ICICI-bank-upload-" + ymTime(ym).Format("Jan-2006") + ".xlsx"
}

// payrollICICI is GET /api/payroll/{ym}/icici-upload.xlsx: ICICI's bulk payment upload. The first sheet holds the
// header and the payments only; account, IFSC, mobile, date (and every other text column) are text cells, the
// amount a number with 2 decimals.
func (s *Server) payrollICICI(r *http.Request, c *Ctx) (*bookOut, error) {
	ym, debit, lines, skips, err := s.iciciPlan(r, c)
	if err != nil {
		return nil, err
	}
	f := excelize.NewFile()
	const sh = "Sheet1"
	text, _ := f.NewStyle(&excelize.Style{NumFmt: 49}) // "@": Excel keeps what is typed as text
	money, _ := f.NewStyle(&excelize.Style{NumFmt: 2}) // 0.00
	head, _ := f.NewStyle(&excelize.Style{NumFmt: 49, Font: &excelize.Font{Bold: true}})
	widths := []float64{22, 11, 16, 30, 22, 14, 12, 16, 12, 13, 30, 10, 12}
	for i := range iciciCols {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sh, col, col, widths[i])
		st := text
		if iciciCols[i] == "AMOUNT" {
			st = money
		}
		_ = f.SetColStyle(sh, col, st)
	}
	cell := func(col, row int) string { n, _ := excelize.CoordinatesToCellName(col, row); return n }
	for i, h := range iciciCols {
		_ = f.SetCellStr(sh, cell(i+1, 1), h)
	}
	end, _ := excelize.ColumnNumberToName(len(iciciCols))
	_ = f.SetCellStyle(sh, "A1", end+"1", head)
	narr, date := iciciNarration(ym), time.Now().In(istZone).Format("02-01-2006")
	for i, l := range lines {
		row := i + 2
		vals := []string{"PAB_VENDOR", l.Mode, debit, l.Name, l.Acc, l.IFSC, "", narr, date, l.Mobile, l.Email, "", l.ID}
		for j, v := range vals {
			if iciciCols[j] == "AMOUNT" {
				_ = f.SetCellFloat(sh, cell(j+1, row), l.Amount, 2, 64)
				_ = f.SetCellStyle(sh, cell(j+1, row), cell(j+1, row), money)
				continue
			}
			_ = f.SetCellStr(sh, cell(j+1, row), v)
			_ = f.SetCellStyle(sh, cell(j+1, row), cell(j+1, row), text)
		}
	}
	if len(skips) > 0 { // ICICI uploads the first sheet only; this one is for Payroll to fix and download again
		const ni = "Not included"
		_, _ = f.NewSheet(ni)
		bold, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
		for i, h := range []string{"Emp ID", "Name", "Amount", "Reason"} {
			_ = f.SetCellStr(ni, cell(i+1, 1), h)
		}
		_ = f.SetCellStyle(ni, "A1", "D1", bold)
		_ = f.SetColWidth(ni, "A", "A", 12)
		_ = f.SetColWidth(ni, "B", "B", 28)
		_ = f.SetColWidth(ni, "C", "C", 12)
		_ = f.SetColWidth(ni, "D", "D", 70)
		for i, k := range skips {
			_ = f.SetCellStr(ni, cell(1, i+2), k.EmployeeID)
			_ = f.SetCellStr(ni, cell(2, i+2), k.Name)
			_ = f.SetCellFloat(ni, cell(3, i+2), k.Amount, 2, 64)
			_ = f.SetCellStyle(ni, cell(3, i+2), cell(3, i+2), money)
			_ = f.SetCellStr(ni, cell(4, i+2), k.Reason)
		}
	}
	f.SetActiveSheet(0)
	left := make([]string, 0, len(skips))
	for _, k := range skips {
		left = append(left, k.EmployeeID)
	}
	return &bookOut{name: iciciFileName(ym), f: f, rows: len(lines), extra: map[string]any{"notIncluded": left}}, nil
}

// ---------- Full salary register ----------

// regRow is one person's row of the full register with what it is built from.
type regRow struct {
	o    *PayRowOut
	f    payFigures
	gov  store.Row
	bank bankAcct
	ctc  float64
	earn map[string]float64 // pay lines by kind (earnKind)
	ded  map[string]float64 // by dedKind
}

// earnKind sorts an earning pay line into its register column.
func earnKind(l calc.Line) string {
	switch {
	case strings.HasPrefix(l.Label, "Arrear"):
		return "arrear"
	case strings.HasPrefix(l.Label, "Reimbursement"):
		return "reimb"
	case l.Label == uniRefundLabel:
		return "unirefund"
	case strings.HasPrefix(l.Label, "Held salary released"):
		return "held"
	case l.Label == "Salary advance":
		return "advance"
	case l.Label == incentiveLabel:
		return "incentive"
	}
	return "other"
}

// dedKind sorts a recovery pay line into its register column.
func dedKind(l calc.Line) string {
	switch {
	case strings.HasPrefix(l.Label, "Advance recovery"):
		return "advance"
	case strings.HasPrefix(l.Label, "Uniform recovery"):
		return "uniform"
	case strings.HasPrefix(l.Label, "Asset ") || strings.HasPrefix(l.Source, "AST-"):
		return "asset"
	}
	return "other"
}

// regCol is a column of the full register: heading, sensitive group ("" none), kind (t text, d date, n number,
// m money) and the value.
type regCol struct {
	H     string
	Group string
	Kind  byte
	V     func(x *regRow) any
}

var regCols = func() []regCol {
	t := func(h, g string, v func(x *regRow) any) regCol { return regCol{h, g, 't', v} }
	m := func(h string, v func(x *regRow) float64) regCol {
		return regCol{h, "salary", 'm', func(x *regRow) any { return calc.Round2(v(x)) }}
	}
	n := func(h string, v func(x *regRow) float64) regCol {
		return regCol{h, "", 'n', func(x *regRow) any { return calc.Round2(v(x)) }}
	}
	yes := func(b bool) string {
		if b {
			return "Yes"
		}
		return "No"
	}
	return []regCol{
		// Employee
		t("Emp ID", "", func(x *regRow) any { return x.o.EmployeeID }),
		t("Name", "", func(x *regRow) any { return x.o.Name }),
		t("Designation", "", func(x *regRow) any { return x.o.Desig }),
		t("Department", "", func(x *regRow) any { return x.o.Dept }),
		t("Grade", "", func(x *regRow) any { return x.o.Grade }),
		t("Employment type", "", func(x *regRow) any { return x.o.Type }),
		t("City", "", func(x *regRow) any { return x.o.City }),
		t("Location", "", func(x *regRow) any { return x.o.Location }),
		{"Date of joining", "", 'd', func(x *regRow) any { return x.o.DOJ }},
		{"Last working day", "", 'd', func(x *regRow) any { return x.o.LWD }},
		t("PAN", "govId", func(x *regRow) any { return str(x.gov["pan"]) }),
		t("UAN", "govId", func(x *regRow) any { return str(x.gov["uan"]) }),
		t("ESIC no", "govId", func(x *regRow) any { return str(x.gov["esic"]) }),
		t("Bank", "bank", func(x *regRow) any { return x.bank.Bank }),
		t("Account number", "bank", func(x *regRow) any { return x.bank.Acc }),
		t("IFSC", "bank", func(x *regRow) any { return x.bank.IFSC }),
		// Days
		n("Days in month", func(x *regRow) float64 { return float64(x.o.Days) }),
		n("Paid days", func(x *regRow) float64 { return x.o.Paid }),
		n("LOP days", func(x *regRow) float64 { return x.f.LOP }),
		n("Days outside employment", func(x *regRow) float64 { return x.f.Outside }),
		// Salary
		m("Monthly gross (structure)", func(x *regRow) float64 { return x.o.Gross }),
		m("CTC a month", func(x *regRow) float64 { return x.ctc }),
		// Earnings
		m("Basic", func(x *regRow) float64 { return x.f.Basic }),
		m("HRA", func(x *regRow) float64 { return x.f.Hra }),
		m("Reimbursement (fixed pay)", func(x *regRow) float64 { return x.f.Reimb }),
		m("Special allowance", func(x *regRow) float64 { return x.f.Sa }),
		m("Bonus", func(x *regRow) float64 { return x.f.Bonus }),
		m("Variable pay", func(x *regRow) float64 { return x.f.VarPay }),
		m("Incentive", func(x *regRow) float64 { return x.earn["incentive"] }),
		m("Arrears", func(x *regRow) float64 { return x.earn["arrear"] }),
		m("Reimbursements", func(x *regRow) float64 { return x.earn["reimb"] }),
		m("Salary advance", func(x *regRow) float64 { return x.earn["advance"] }),
		m("Uniform refund", func(x *regRow) float64 { return x.earn["unirefund"] }),
		m("Held salary released", func(x *regRow) float64 { return x.earn["held"] }),
		m("Other earnings", func(x *regRow) float64 { return x.earn["other"] + x.f.OE }),
		m("Total earnings", func(x *regRow) float64 { return x.f.TotE }),
		// Deductions
		m("Employee PF", func(x *regRow) float64 { return x.f.PF }),
		m("Employee ESI", func(x *regRow) float64 { return x.f.ESI }),
		m("LWF (employee)", func(x *regRow) float64 { return x.f.LWF }),
		m("TDS", func(x *regRow) float64 { return x.f.Tds }),
		m("Meal", func(x *regRow) float64 { return x.o.R.Meal }),
		m("Accommodation", func(x *regRow) float64 { return x.f.MealAcc - calc.Round2(x.o.R.Meal) }),
		m("Advance recovery", func(x *regRow) float64 { return x.ded["advance"] }),
		m("Uniform recovery", func(x *regRow) float64 { return x.ded["uniform"] }),
		m("Asset recovery", func(x *regRow) float64 { return x.ded["asset"] }),
		m("Other recoveries", func(x *regRow) float64 { return x.ded["other"] + x.f.OD }),
		m("Total deductions", func(x *regRow) float64 { return x.f.TotD }),
		m("Rounding off", func(x *regRow) float64 { return x.f.RoundOff }),
		m("Net pay", func(x *regRow) float64 { return x.f.Net }),
		// Employer
		m("Employer EPF", func(x *regRow) float64 { return x.o.R.ErEpf }),
		m("Employer EPS", func(x *regRow) float64 { return x.o.R.Eps }),
		m("EDLI", func(x *regRow) float64 { return x.o.R.Edli }),
		m("EPFO admin charges", func(x *regRow) float64 { return x.o.R.Admin }),
		m("Employer ESI", func(x *regRow) float64 { return x.o.R.ErESI }),
		m("Employer LWF", func(x *regRow) float64 { return x.o.R.LwfEr }),
		m("Medical insurance", func(x *regRow) float64 { return x.o.R.Med }),
		m("Gratuity provision", func(x *regRow) float64 { return x.o.R.Grat }),
		m("Total employer cost", func(x *regRow) float64 { return x.o.R.EmployerCost }),
		m("Cost to company for the month", func(x *regRow) float64 { return x.f.TotE + x.o.R.EmployerCost }),
		// Status
		t("On hold", "", func(x *regRow) any { return yes(x.o.Hold) }),
		t("Hold reason", "", func(x *regRow) any { return x.o.HoldWhy }),
		t("Remarks", "", func(x *regRow) any { return x.o.Reason }),
	}
}()

// regCTC is each person's CTC a month as the employee record shows it: the salary engine on the full monthly
// gross with the structure, variable pay, meal and accommodation and labour welfare fund of the month.
func (s *Server) regCTC(r *http.Request, ym string, rows []*PayRowOut) (map[string]float64, error) {
	ctx, q := r.Context(), s.st.DB
	ids := make([]string, 0, len(rows))
	for _, o := range rows {
		ids = append(ids, o.EmployeeID)
	}
	emps, err := loadEmps(ctx, q, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	state := map[string]string{}
	for _, e := range emps {
		state[e.ID] = e.StateID
	}
	structs, err := q.C("salary_structure").Find(ctx, nil)
	if err != nil {
		return nil, err
	}
	byName := map[string]calc.Structure{}
	def := calc.DefaultStructure
	for _, x := range structs {
		var st calc.Structure
		_ = payengineJSON(x["rates"], &st)
		byName[str(x["name"])] = st
		if x["is_default"] == true {
			def = st
		}
	}
	pays, err := q.C("employee_pay").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, err
	}
	pay := map[string]store.Row{}
	for _, p := range pays {
		pay[str(p["employee_id"])] = p
	}
	rules, err := q.C("lwf_rule").Find(ctx, bson.M{"effective_from": bson.M{"$lte": lastDayOfYm(ym)}},
		options.Find().SetSort(bson.D{{Key: "state_id", Value: 1}, {Key: "effective_from", Value: -1}}))
	if err != nil {
		return nil, err
	}
	lwf := map[string]*calc.LwfRule{}
	for _, x := range rules {
		if _, seen := lwf[str(x["state_id"])]; !seen {
			var rule calc.LwfRule
			_ = payengineJSON(x["rule"], &rule)
			lwf[str(x["state_id"])] = &rule
		}
	}
	out := map[string]float64{}
	for _, o := range rows {
		st, ok := byName[o.Structure]
		if !ok {
			st = def
		}
		p := pay[o.EmployeeID]
		varPct := payengineFloatPtr(p["var_pct"])
		if from := str(p["var_from"]); from != "" && ym < from {
			varPct = payengineFloatPtr(p["var_prev_pct"])
		}
		in := calc.Input{Type: calc.EmploymentType(o.Type), Gross: o.Gross, Structure: &st, VarPct: varPct, VarCycle: payengineIntPtr(p["var_cycle"]),
			Meal: payengineFloat(p["meal"]), Accom: payengineFloat(p["accommodation"]), Lwf: lwf[state[o.EmployeeID]], EmpAmts: byCompOf(p["comp_amts"])}
		varTargetInput(ctx, q, o.EmployeeID, ym, st, &in) // a structure made from components (paycomp.go)
		res := calc.Calc(in)
		out[o.EmployeeID] = calc.Round2(res.CTC)
	}
	return out, nil
}

func lastDayOfYm(ym string) string { return ymTime(ym).AddDate(0, 1, -1).Format("2006-01-02") }

// fullRegisterName is the file name: full-salary-register-Oct-2026.xlsx, with -DRAFT before approval.
func fullRegisterName(ym string, draft bool) string {
	n := "full-salary-register-" + ymTime(ym).Format("Jan-2006")
	if draft {
		n += "-DRAFT"
	}
	return n + ".xlsx"
}

// payrollFullRegister is GET /api/payroll/{ym}/full-register.xlsx: every column of each person's salary for the
// month, from the stored rows of the dry run, verified, approved or published month, in the caller's scope.
func (s *Server) payrollFullRegister(r *http.Request, c *Ctx) (*bookOut, error) {
	if err := c.Need("pay.export", 2); err != nil { // the register's rule (approvedRows)
		return nil, err
	}
	ym := r.PathValue("ym")
	if !isYm(ym) {
		return nil, bad("Month must look like 2026-10")
	}
	ctx := r.Context()
	m, err := loadPayMonth(ctx, s.st.DB, ym, false)
	if err != nil {
		return nil, err
	}
	switch m.Status {
	case "Dry run", "Verified", "Approved", "Published":
	default:
		return nil, bad("%s has no payroll calculated yet. Run the dry run first", ymLabel(ym))
	}
	draft := m.Status == "Dry run" || m.Status == "Verified"
	all, err := loadRows(ctx, s.st.DB, ym)
	if err != nil {
		return nil, err
	}
	rows := filterRows(c, all, map[string]string{})
	ids := make([]string, 0, len(rows))
	for _, o := range rows {
		ids = append(ids, o.EmployeeID)
	}
	banks, err := s.monthAccounts(r, ids, ym)
	if err != nil {
		return nil, err
	}
	gov := map[string]store.Row{}
	if gl, err := s.st.DB.C("employee_gov_id").Find(ctx, bson.M{"employee_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.D{{Key: "employee_id", Value: 1}, {Key: "pan", Value: 1}, {Key: "uan", Value: 1}, {Key: "esic", Value: 1}})); err != nil {
		return nil, err
	} else {
		for _, g := range gl {
			gov[str(g["employee_id"])] = g
		}
	}
	ctc, err := s.regCTC(r, ym, rows)
	if err != nil {
		return nil, err
	}
	data := make([]*regRow, 0, len(rows))
	for _, o := range rows {
		x := &regRow{o: o, f: figuresOf(o), gov: gov[o.EmployeeID], bank: banks[o.EmployeeID], ctc: ctc[o.EmployeeID], earn: map[string]float64{}, ded: map[string]float64{}}
		for _, l := range o.Add {
			x.earn[earnKind(l)] += l.Amount
		}
		for _, l := range o.Ded {
			x.ded[dedKind(l)] += l.Amount
		}
		data = append(data, x)
	}
	f := buildFullRegister(c, ym, draft, data)
	return &bookOut{name: fullRegisterName(ym, draft), f: f, rows: len(data), extra: map[string]any{"status": m.Status}}, nil
}

// buildFullRegister writes the sheet: a DRAFT line first before approval, the frozen header, the rows and a totals
// row. Sensitive columns follow the caller's Full / Masked / Hidden (own row in full, as everywhere); the totals of
// amounts are shown only to a caller who sees salaries in full.
func buildFullRegister(c *Ctx, ym string, draft bool, data []*regRow) *excelize.File {
	cols := regColsFor(data)
	f := excelize.NewFile()
	const sh = "Full register"
	_ = f.SetSheetName("Sheet1", sh)
	cell := func(col, row int) string { n, _ := excelize.CoordinatesToCellName(col, row); return n }
	moneyFmt := "#,##0.00"
	dateFmt := "dd-mmm-yyyy"
	sMoney, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &moneyFmt})
	sDate, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &dateFmt})
	sText, _ := f.NewStyle(&excelize.Style{NumFmt: 49})
	sHead, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, Fill: excelize.Fill{Type: "pattern", Color: []string{"E1E5E8"}, Pattern: 1},
		Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"}})
	sTotal, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}, CustomNumFmt: &moneyFmt, Border: []excelize.Border{{Type: "top", Color: "000000", Style: 1}}})
	sDraft, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Color: "B00020", Size: 12}})
	headRow := 1
	if draft {
		_ = f.SetCellStr(sh, "A1", "DRAFT: "+ymLabel(ym)+" not yet approved; figures may change")
		_ = f.SetCellStyle(sh, "A1", "A1", sDraft)
		headRow = 2
	}
	width := make([]int, len(cols))
	for i, col := range cols {
		_ = f.SetCellStr(sh, cell(i+1, headRow), col.H)
		width[i] = min(len(col.H), 18) + 2
	}
	end, _ := excelize.ColumnNumberToName(len(cols))
	_ = f.SetCellStyle(sh, "A"+itoa(headRow), end+itoa(headRow), sHead)
	_ = f.SetRowHeight(sh, headRow, 32)
	salaryFull := c.Sensitive["salary"] == access.Full
	sums := make([]float64, len(cols))
	for ri, x := range data {
		row := headRow + 1 + ri
		for i, col := range cols {
			v := col.V(x)
			if col.Group != "" {
				if s, ok := v.(string); ok && s == "" {
					v = nil
				}
				v = c.Show(col.Group, v, x.o.EmployeeID)
			}
			at := cell(i+1, row)
			switch val := v.(type) {
			case nil:
			case float64:
				sums[i] += val
				_ = f.SetCellFloat(sh, at, val, 2, 64)
				if col.Kind == 'm' {
					_ = f.SetCellStyle(sh, at, at, sMoney)
				}
				width[i] = max(width[i], len(fmt.Sprintf("%.2f", val))+4)
			case string:
				if col.Kind == 'd' {
					if t, err := time.Parse("2006-01-02", val); err == nil {
						_ = f.SetCellValue(sh, at, t)
						_ = f.SetCellStyle(sh, at, at, sDate)
						width[i] = max(width[i], 13)
						continue
					}
				}
				if val == "" {
					continue
				}
				_ = f.SetCellStr(sh, at, val)
				if col.Group == "govId" || col.Group == "bank" {
					_ = f.SetCellStyle(sh, at, at, sText)
				}
				width[i] = max(width[i], len([]rune(val))+2)
			}
		}
	}
	// Totals: days always; amounts only when the caller sees salaries in full (a masked total gives nothing away).
	tr := headRow + 1 + len(data)
	_ = f.SetCellStr(sh, cell(1, tr), "Total")
	_ = f.SetCellStr(sh, cell(2, tr), fmt.Sprintf("%d employee%s", len(data), map[bool]string{true: "", false: "s"}[len(data) == 1]))
	for i, col := range cols {
		at := cell(i+1, tr)
		switch {
		case col.Kind == 'n' && col.H != "Days in month":
			_ = f.SetCellFloat(sh, at, calc.Round2(sums[i]), 2, 64)
		case col.Kind == 'm' && salaryFull:
			_ = f.SetCellFloat(sh, at, calc.Round2(sums[i]), 2, 64)
		case col.Kind == 'm':
			if v := access.MaskGroup("salary", 1.0, c.Sensitive["salary"]); v != nil {
				_ = f.SetCellValue(sh, at, v)
			}
		}
	}
	_ = f.SetCellStyle(sh, cell(1, tr), cell(len(cols), tr), sTotal)
	for i := range cols {
		col, _ := excelize.ColumnNumberToName(i + 1)
		_ = f.SetColWidth(sh, col, col, float64(max(10, min(42, width[i]))))
	}
	_ = f.SetPanes(sh, &excelize.Panes{Freeze: true, XSplit: 2, YSplit: headRow, TopLeftCell: cell(3, headRow+1), ActivePane: "bottomRight"})
	return f
}

// regColsFor is the register's columns for these rows: the fixed columns, and for a designed structure's components
// (paycomp.go) a column each: fixed earnings after Special allowance ("Component: …", a breakdown of Basic, HRA and
// Special allowance, which hold their totals), variable components after Variable pay, deductions after
// Accommodation. A month with converted structures only has exactly the fixed columns.
func regColsFor(data []*regRow) []regCol {
	base := regCols
	hasFee := false
	for _, x := range data {
		hasFee = hasFee || x.f.Fee != 0
	}
	if hasFee { // a Retainer fee column after Special allowance, only when a retainer is paid in the month
		base = make([]regCol, 0, len(regCols)+1)
		for _, c := range regCols {
			base = append(base, c)
			if c.H == "Special allowance" {
				base = append(base, regCol{"Retainer fee", "salary", 'm', func(x *regRow) any { return x.f.Fee }})
			}
		}
	}
	type key struct{ kind, side, code string }
	var earn, vars, ded []key
	names := map[key]string{}
	for _, x := range data {
		for _, l := range x.o.R.Comps {
			k := key{l.Kind, l.Side, l.Code}
			if _, ok := names[k]; !ok {
				switch {
				case l.Side == calc.SideDed:
					ded = append(ded, k)
				case l.Kind == calc.CompVariable:
					vars = append(vars, k)
				default:
					earn = append(earn, k)
				}
			}
			names[k] = l.Name
		}
	}
	if len(names) == 0 {
		return base
	}
	col := func(prefix string, k key) regCol {
		return regCol{prefix + names[k], "salary", 'm', func(x *regRow) any {
			t := 0.0
			for _, l := range x.o.R.Comps {
				if l.Kind == k.kind && l.Side == k.side && l.Code == k.code {
					t += l.Amount
				}
			}
			return calc.Round2(t)
		}}
	}
	out := make([]regCol, 0, len(base)+len(names))
	for _, c := range base {
		out = append(out, c)
		switch c.H {
		case "Special allowance":
			for _, k := range earn {
				out = append(out, col("Component: ", k))
			}
		case "Variable pay":
			for _, k := range vars {
				out = append(out, col("Variable: ", k))
			}
		case "Accommodation":
			for _, k := range ded {
				out = append(out, col("", k))
			}
		}
	}
	return out
}
