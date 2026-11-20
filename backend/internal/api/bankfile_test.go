package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The ICICI bank upload and the full salary register (bankfile.go, 14 Nov 2026).

var bfICICICols = []string{"PYMT_PROD_TYPE_CODE", "PYMT_MODE", "DEBIT_ACC_NO", "BNF_NAME", "BENE_ACC_NO", "BENE_IFSC", "AMOUNT", "CREDIT_NARR",
	"PYMT_DATE", "MOBILE_NUM", "EMAIL_ID", "REMARK", "REF_NO"}

// bfBook downloads a workbook and opens it, failing on anything but 200.
func bfBook(t *testing.T, c *client, path, emp, role string) (*excelize.File, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.tokens[emp])
	req.Header.Set("X-Role", role)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("%s: %d %s", path, res.StatusCode, body)
	}
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_, params, _ := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
	return f, params["filename"]
}

// asJSON is a stored value as JSON text, for looking into audit entries.
func asJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// bfSheet is a sheet as rows of cells, padded to the longest row.
func bfSheet(t *testing.T, f *excelize.File, sh string) [][]string {
	t.Helper()
	rows, err := f.GetRows(sh)
	if err != nil {
		t.Fatalf("sheet %s: %v", sh, err)
	}
	w := 0
	for _, r := range rows {
		w = max(w, len(r))
	}
	for i := range rows {
		for len(rows[i]) < w {
			rows[i] = append(rows[i], "")
		}
	}
	return rows
}

func bfCol(head []string, name string) int {
	for i, h := range head {
		if h == name {
			return i
		}
	}
	return -1
}

func bfNum(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		t.Fatalf("not a number: %q", s)
	}
	return v
}

func bfSetBank(t *testing.T, c *client, emp, acc, ifsc string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.st.DB.C("employee_bank").DeleteMany(ctx, bson.M{"employee_id": emp}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.st.DB.C("employee_bank").Insert(ctx, bson.M{"employee_id": emp, "bank_name": "Test Bank", "account_no": acc, "ifsc": ifsc, "from_month": "2020-01", "created_by": "Test seed"}); err != nil {
		t.Fatal(err)
	}
}

func TestBankFileICICIAndFullRegister(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1012")
	ym := pdOpenYm(t, c)

	// The people: an ICICI account with leading zeros (FT), a name with special characters and a mobile written with
	// +91, spaces and dashes, an IFSC that is not valid, an account with letters, someone with no account, someone
	// held, someone whose net is 0, and someone paid a held salary released this month.
	const (
		ft, named, badIFSC, badAcc, noAcc, held, zero, released = "EMP-1016", "EMP-1020", "EMP-1022", "EMP-1046", "EMP-1030", "EMP-1026", "EMP-1031", "EMP-1032"
	)
	bfSetBank(t, c, ft, "000123456789", "ICIC0001234")
	bfSetBank(t, c, named, "50100012345678", "hdfc0001234")
	bfSetBank(t, c, badIFSC, "50100099998888", "HDFC1234")
	bfSetBank(t, c, badAcc, "5010-00AB-1234", "HDFC0001234")
	if _, err := st.DB.C("employee").Update(ctx, bson.M{"_id": named}, bson.M{"$set": bson.M{"name": "Ravi K. D'Souza-Rao (Jr)"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("employee_personal").Update(ctx, bson.M{"employee_id": named}, bson.M{"$set": bson.M{"mobile": "+91 98765-43210", "email": "ravi.dsouza@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("employee_personal").Update(ctx, bson.M{"employee_id": ft}, bson.M{"$set": bson.M{"mobile": "12345"}}); err != nil {
		t.Fatal(err)
	}
	if s, r := c.do("POST", "/api/holds", "EMP-1011", "Payroll", map[string]any{"employeeId": held, "reason": "Bank or KYC issue", "note": "Account frozen by the bank", "fromYm": ym}); s != 200 {
		t.Fatal(r)
	}
	relLabel := "Held salary released, " + mustDate(ymAdd(ym, -1)+"-01").Format("January 2006")
	if _, err := st.DB.C("ledger").Insert(ctx, bson.M{"employee_id": released, "ym": ym, "kind": "earn", "label": relLabel, "amount": 4321.0, "source_id": "HLD-TEST", "reason": "Hold released: test"}); err != nil {
		t.Fatal(err)
	}

	// The debit account setting: Pay schedule, Payroll settings at Edit, 9 to 18 digits, audited.
	_, sch := c.do("GET", "/api/payroll/schedule", "EMP-1011", "Payroll", nil)
	if sch["iciciDebitAcc"] != "38605005062" {
		t.Fatalf("default debit account: %v", sch["iciciDebitAcc"])
	}
	for _, v := range []string{"12345678", "1234567890123456789", "3860500506A", "38605 005062"} {
		if s, r := c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"iciciDebitAcc": v}); s != 400 {
			t.Fatalf("debit account %q: %d %v", v, s, r)
		}
	}
	if s, _ := c.do("PUT", "/api/payroll/schedule", "EMP-1004", "HRBP", map[string]any{"iciciDebitAcc": "123456789012"}); s != 403 {
		t.Fatalf("HRBP changing the debit account: %d", s)
	}
	const debit = "003860500506"
	if s, r := c.do("PUT", "/api/payroll/schedule", "EMP-1011", "Payroll", map[string]any{"iciciDebitAcc": " " + debit + " "}); s != 200 {
		t.Fatalf("save debit account: %d %v", s, r)
	}
	if _, sch = c.do("GET", "/api/payroll/schedule", "EMP-1011", "Payroll", nil); sch["iciciDebitAcc"] != debit {
		t.Fatalf("saved debit account: %v", sch["iciciDebitAcc"])
	}
	if a := apiDoc(st, "audit_log", bson.M{"action": "Bank upload debit account updated", "entity": "company_setting"}); a == nil ||
		!strings.Contains(asJSON(a["before"]), "38605005062") || !strings.Contains(asJSON(a["after"]), debit) {
		t.Fatalf("debit account change not audited: %v", a)
	}

	// The run, up to the dry run; one person's paid days set to 0, so the net is 0.
	c.pcCloseAll(t, ym)
	// The checklist helper gives everyone without an account one; this person has none again (as after a bank
	// closure closed with the account removed later).
	if _, err := st.DB.C("employee_bank").DeleteMany(ctx, bson.M{"employee_id": noAcc}); err != nil {
		t.Fatal(err)
	}
	pfaMust(t, c, ym, "dry-run", "EMP-1011", "Payroll")
	zp := 0.0
	if s, r := c.do("PATCH", "/api/payroll/"+ym+"/rows/"+zero, "EMP-1011", "Payroll", map[string]any{"paidDays": zp, "reason": "No work this month"}); s != 200 {
		t.Fatalf("paid days 0: %d %v", s, r)
	}
	if n := num(c.row(t, ym, zero)["net"]); n > 0 {
		t.Fatalf("%s net %v, want 0", zero, n)
	}

	// Before approval: no bank upload; the full register is a draft.
	icici := "/api/payroll/" + ym + "/icici-upload.xlsx"
	full := "/api/payroll/" + ym + "/full-register.xlsx"
	label := mustDate(ym + "-01").Format("January 2006")
	for _, step := range []string{"", "verify"} {
		if step != "" {
			pfaMust(t, c, ym, step, "EMP-1011", "Payroll")
		}
		if s, b := c.getRaw(icici, "EMP-1012", "Payroll Manager"); s != 400 || !strings.Contains(string(b), "not approved") {
			t.Fatalf("ICICI upload before approval (%s): %d %s", step, s, b)
		}
		if s, r := c.do("GET", "/api/payroll/"+ym+"/icici-upload", "EMP-1012", "Payroll Manager", nil); s != 400 {
			t.Fatalf("ICICI check before approval: %d %v", s, r)
		}
		f, name := bfBook(t, c, full, "EMP-1012", "Payroll Manager")
		if !strings.Contains(name, "DRAFT") || name != "full-salary-register-"+mustDate(ym+"-01").Format("Jan-2006")+"-DRAFT.xlsx" {
			t.Fatalf("draft file name %q", name)
		}
		bfSample(t, f, name)
		rows := bfSheet(t, f, "Full register")
		if want := "DRAFT: " + label + " not yet approved; figures may change"; rows[0][0] != want {
			t.Fatalf("draft line %q, want %q", rows[0][0], want)
		}
		if rows[1][0] != "Emp ID" {
			t.Fatalf("header not under the draft line: %v", rows[1][:3])
		}
	}
	if s, _ := c.getRaw("/api/payroll/"+ymAdd(ym, 1)+"/full-register.xlsx", "EMP-1012", "Payroll Manager"); s != 400 {
		t.Fatalf("full register of a month not run: %d", s)
	}
	if s, _ := c.getRaw(full, "EMP-1004", "HRBP"); s != 403 {
		t.Fatalf("HRBP downloading the full register: %d", s)
	}
	pfaMust(t, c, ym, "approve", "EMP-1012", "Payroll Manager")

	// ---- ICICI upload ----
	_, chk := c.do("GET", "/api/payroll/"+ym+"/icici-upload", "EMP-1012", "Payroll Manager", nil)
	f, name := bfBook(t, c, icici, "EMP-1012", "Payroll Manager")
	if want := "ICICI-bank-upload-" + mustDate(ym+"-01").Format("Jan-2006") + ".xlsx"; name != want {
		t.Fatalf("file name %q, want %q", name, want)
	}
	bfSample(t, f, name)
	sheets := f.GetSheetList()
	if len(sheets) != 2 || sheets[1] != "Not included" {
		t.Fatalf("sheets %v", sheets)
	}
	rows := bfSheet(t, f, sheets[0])
	if strings.Join(rows[0], "|") != strings.Join(bfICICICols, "|") {
		t.Fatalf("headers %v", rows[0])
	}
	if len(rows)-1 != int(num(chk["rows"])) {
		t.Fatalf("first sheet has %d data rows, check says %v: only header and payments", len(rows)-1, chk["rows"])
	}
	byID := map[string][]string{}
	for i, r := range rows[1:] {
		byID[r[12]] = r
		for j := range r { // every cell but the amount is text; the amount a number with 2 decimals
			at, _ := excelize.CoordinatesToCellName(j+1, i+2)
			typ, _ := f.GetCellType(sheets[0], at)
			if bfICICICols[j] == "AMOUNT" {
				if typ == excelize.CellTypeSharedString || typ == excelize.CellTypeInlineString {
					t.Fatalf("%s amount stored as text", r[12])
				}
				continue
			}
			if r[j] != "" && typ != excelize.CellTypeSharedString && typ != excelize.CellTypeInlineString {
				t.Fatalf("%s %s stored as type %v, want text", r[12], bfICICICols[j], typ)
			}
		}
	}
	today := time.Now().In(mustIST()).Format("02-01-2006")
	narr := mustDate(ym+"-01").Format("Jan 06") + " Salary"
	for id, r := range byID {
		if r[0] != "PAB_VENDOR" || r[2] != debit || r[7] != narr || r[8] != today || r[11] != "" || r[12] != id {
			t.Fatalf("%s: fixed columns %v", id, r)
		}
		if len(r[7]) > 30 {
			t.Fatalf("narration too long: %q", r[7])
		}
	}
	if r := byID[ft]; r == nil || r[1] != "FT" || r[4] != "000123456789" || r[5] != "ICIC0001234" || r[9] != "" {
		t.Fatalf("ICICI account (FT, leading zeros, bad mobile blank): %v", r)
	}
	if r := byID[named]; r == nil || r[1] != "NEFT" || r[3] != "Ravi K D Souza Rao Jr" && r[3] != "Ravi K DSouza Rao Jr" || r[5] != "HDFC0001234" ||
		r[9] != "9876543210" || r[10] != "ravi.dsouza@example.com" || r[4] != "50100012345678" {
		t.Fatalf("NEFT row with name, mobile and email cleaned: %v", r)
	}
	if r := byID[named]; strings.ContainsAny(r[3], ".'-()") || strings.Contains(r[3], "  ") {
		t.Fatalf("name not cleaned: %q", r[3])
	}
	for _, id := range []string{held, zero, badIFSC, badAcc, noAcc} {
		if byID[id] != nil {
			t.Fatalf("%s should not be in the upload", id)
		}
	}
	rel := c.row(t, ym, released)
	if r := byID[released]; r == nil || bfNum(t, r[6]) != num(rel["net"]) {
		t.Fatalf("held salary released is part of the month's net: %v, net %v", r, rel["net"])
	}
	if cellV, _ := f.GetCellValue(sheets[0], "G2"); !strings.Contains(cellV, ".") {
		t.Fatalf("amount not shown with 2 decimals: %q", cellV)
	}
	// Not included: name, code and reason, in the file and in the check.
	ni := bfSheet(t, f, "Not included")
	if strings.Join(ni[0], "|") != "Emp ID|Name|Amount|Reason" {
		t.Fatalf("Not included header %v", ni[0])
	}
	reasons := map[string]string{}
	for _, r := range ni[1:] {
		reasons[r[0]] = r[3]
	}
	for id, want := range map[string]string{badIFSC: "IFSC HDFC1234 is not valid", badAcc: "Account number must be 1 to 32 digits"} {
		if !strings.Contains(reasons[id], want) {
			t.Fatalf("%s reason %q, want %q", id, reasons[id], want)
		}
	}
	if len(chk["notIncluded"].([]any)) != len(ni)-1 {
		t.Fatalf("check lists %d, sheet %d", len(chk["notIncluded"].([]any)), len(ni)-1)
	}
	for _, v := range chk["notIncluded"].([]any) {
		m := v.(map[string]any)
		if m["employeeId"] == "" || m["name"] == "" || m["reason"] == "" || m["amount"] != nil {
			t.Fatalf("check item %v", m)
		}
	}
	// Exactly the bank advice's people, with its amounts.
	bf, _ := bfBook(t, c, "/api/payroll/"+ym+"/bank-advice.xlsx", "EMP-1012", "Payroll Manager")
	advice := map[string]float64{}
	for _, r := range bfSheet(t, bf, "Data")[1:] {
		advice[r[0]] = bfNum(t, r[5])
	}
	if len(advice) != len(byID)+len(ni)-1 {
		t.Fatalf("bank advice pays %d, upload %d + not included %d", len(advice), len(byID), len(ni)-1)
	}
	for id, r := range byID {
		if a, ok := advice[id]; !ok || a != bfNum(t, r[6]) {
			t.Fatalf("%s: upload %s, bank advice %v (%v)", id, r[6], a, ok)
		}
	}
	for _, r := range ni[1:] {
		if _, ok := advice[r[0]]; !ok {
			t.Fatalf("%s is not paid by the bank advice", r[0])
		}
	}
	if apiDoc(st, "audit_log", bson.M{"action": "Downloaded " + name, "entity": "download"}) == nil {
		t.Fatal("ICICI download not audited")
	}

	// ---- Full register, approved ----
	f, name = bfBook(t, c, full, "EMP-1012", "Payroll Manager")
	if strings.Contains(name, "DRAFT") {
		t.Fatalf("approved month named %q", name)
	}
	bfSample(t, f, name)
	reg := bfSheet(t, f, "Full register")
	head := reg[0]
	if head[0] != "Emp ID" {
		t.Fatalf("approved register starts with %v", head[:3])
	}
	for _, h := range []string{"PAN", "UAN", "ESIC no", "Account number", "IFSC", "CTC a month", "Basic", "HRA", "Held salary released", "Total earnings",
		"Employee PF", "TDS", "Advance recovery", "Total deductions", "Rounding off", "Net pay", "Employer EPS", "Total employer cost",
		"Cost to company for the month", "On hold", "Hold reason", "Remarks"} {
		if bfCol(head, h) < 0 {
			t.Fatalf("no column %s", h)
		}
	}
	body, total := reg[1:len(reg)-1], reg[len(reg)-1]
	if total[0] != "Total" {
		t.Fatalf("last row %v", total[:3])
	}
	// Totals equal the sum of the rows; each row adds up.
	firstMoney, lastMoney := bfCol(head, "Monthly gross (structure)"), bfCol(head, "Cost to company for the month")
	for j := firstMoney; j <= lastMoney; j++ {
		sum := 0.0
		for _, r := range body {
			if r[j] != "" {
				sum += bfNum(t, r[j])
			}
		}
		if math.Abs(sum-bfNum(t, total[j])) > 0.005 {
			t.Fatalf("%s: total %s, rows add up to %.2f", head[j], total[j], sum)
		}
	}
	sumCols := func(r []string, from, to string) float64 {
		s := 0.0
		for j := bfCol(head, from); j <= bfCol(head, to); j++ {
			if r[j] != "" {
				s += bfNum(t, r[j])
			}
		}
		return s
	}
	for _, r := range body {
		tot := func(h string) float64 { return bfNum(t, r[bfCol(head, h)]) }
		if math.Abs(sumCols(r, "Basic", "Other earnings")-tot("Total earnings")) > 0.02 {
			t.Fatalf("%s: earnings do not add up to the total", r[0])
		}
		if math.Abs(sumCols(r, "Employee PF", "Other recoveries")-tot("Total deductions")) > 0.02 {
			t.Fatalf("%s: deductions do not add up to the total", r[0])
		}
		if r[bfCol(head, "Net pay")] != "" && tot("Net pay") > 0 && math.Abs(tot("Total earnings")-tot("Total deductions")+tot("Rounding off")-tot("Net pay")) > 0.02 {
			t.Fatalf("%s: net does not foot", r[0])
		}
		if math.Abs(sumCols(r, "Employer EPF", "Gratuity provision")-tot("Total employer cost")) > 0.02 {
			t.Fatalf("%s: employer cost does not add up", r[0])
		}
	}
	// Net pay equals the old register's for every row.
	of, _ := bfBook(t, c, "/api/payroll/"+ym+"/register.xlsx", "EMP-1012", "Payroll Manager")
	old := bfSheet(t, of, "Data")
	oldNet := map[string]string{}
	for _, r := range old[1:] {
		oldNet[r[0]] = r[bfCol(old[0], "Net pay")]
	}
	if len(oldNet) != len(body) {
		t.Fatalf("register has %d rows, full register %d", len(oldNet), len(body))
	}
	for _, r := range body {
		if bfNum(t, oldNet[r[0]]) != bfNum(t, r[bfCol(head, "Net pay")]) {
			t.Fatalf("%s: net %s, register %s", r[0], r[bfCol(head, "Net pay")], oldNet[r[0]])
		}
	}
	find := func(rows [][]string, id string) []string {
		for _, r := range rows {
			if r[0] == id {
				return r
			}
		}
		t.Fatalf("%s not in the register", id)
		return nil
	}
	if r := find(body, held); r[bfCol(head, "On hold")] != "Yes" || !strings.Contains(r[bfCol(head, "Hold reason")], "Bank or KYC issue") {
		t.Fatalf("held row: %v %v", r[bfCol(head, "On hold")], r[bfCol(head, "Hold reason")])
	}
	if r := find(body, released); bfNum(t, r[bfCol(head, "Held salary released")]) != 4321 {
		t.Fatalf("held salary released column: %v", r[bfCol(head, "Held salary released")])
	}
	if r := find(body, zero); r[bfCol(head, "Remarks")] != "No work this month" {
		t.Fatalf("remarks: %q", r[bfCol(head, "Remarks")])
	}
	if r := find(body, ft); r[bfCol(head, "Account number")] != "000123456789" {
		t.Fatalf("account in full for Payroll Manager: %q", r[bfCol(head, "Account number")])
	}
	pan := apiDoc(st, "employee_gov_id", bson.M{"employee_id": named})
	if r := find(body, named); pan != nil && asStr(pan["pan"]) != "" && r[bfCol(head, "PAN")] != asStr(pan["pan"]) {
		t.Fatalf("PAN in full: %q", r[bfCol(head, "PAN")])
	}

	// Masking: a role that sees salaries masked and government IDs hidden.
	for grp, mode := range map[string]string{"salary": "Masked", "govId": "Hidden", "bank": "Masked"} {
		_, _ = st.DB.C("role_sensitive").DeleteMany(ctx, bson.M{"role_name": "Payroll", "grp": grp})
		if _, err := st.DB.C("role_sensitive").Insert(ctx, bson.M{"role_name": "Payroll", "grp": grp, "mode": mode}); err != nil {
			t.Fatal(err)
		}
	}
	f, _ = bfBook(t, c, full, "EMP-1011", "Payroll")
	mreg := bfSheet(t, f, "Full register")
	mhead, mbody, mtotal := mreg[0], mreg[1:len(mreg)-1], mreg[len(mreg)-1]
	for _, r := range mbody {
		if r[0] == "EMP-1011" { // own row in full, as everywhere
			continue
		}
		for _, h := range []string{"Net pay", "Basic", "Total earnings", "CTC a month", "Employer EPF"} {
			if v := r[bfCol(mhead, h)]; v != "₹ ••••" {
				t.Fatalf("%s %s masked as %q", r[0], h, v)
			}
		}
		for _, h := range []string{"PAN", "UAN", "ESIC no"} {
			if v := r[bfCol(mhead, h)]; v != "" {
				t.Fatalf("%s %s hidden, got %q", r[0], h, v)
			}
		}
		if v := r[bfCol(mhead, "Account number")]; v != "" && (!strings.HasPrefix(v, "•") || strings.ContainsAny(v[:len(v)-4], "0123456789")) {
			t.Fatalf("%s account masked as %q", r[0], v)
		}
		if bfNum(t, r[bfCol(mhead, "Paid days")]) < 0 {
			t.Fatal("days stay")
		}
	}
	if v := mtotal[bfCol(mhead, "Net pay")]; v != "₹ ••••" {
		t.Fatalf("masked total %q", v)
	}
}

// bfSample saves a downloaded workbook in HRMS_EXPORT_SAMPLES when it is set, so the owner can open the files the
// test made.
func bfSample(t *testing.T, f *excelize.File, name string) {
	t.Helper()
	dir := os.Getenv("HRMS_EXPORT_SAMPLES")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveAs(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

// mustIST is India Standard Time for the expected payment date.
func mustIST() *time.Location {
	if l, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return l
	}
	return time.FixedZone("IST", 5*3600+1800)
}
