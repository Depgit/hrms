package api

// Opening balances for staff moved onto the HRMS mid-year (owner's request of 13 Nov 2026): Bulk update templates for
// uniform and salary advances whose recovery started under the old system.
//
// The first HRMS deduction month (firstHrmsYm) is the month the HRMS starts recovering: the open payroll month, or
// the next one once its Deduction closure is closed (as for the opening advances of advances.go). An instalment before
// it was recovered outside the HRMS and gets no pay line; an instalment in it or later becomes a pay line of the
// kind the app already uses.
//
//   - Opening uniform (openuni): a uniform_issue marked opening, with no store and no stock movement, its
//     "Uniform recovery" lines from the first HRMS month and the "Uniform deposit refund" by the refund rule, counted
//     from the real last instalment. A refund due before the first HRMS month means it was settled under the old
//     system: the row is refused. A leaver (inactive, or a last working day before the refund month) gets no refund,
//     as uniformRefundCancel does on leaving.
//   - Opening advances (openadv): the advance, its months and its closing month; what was deducted before the first
//     HRMS month is worked out, and the balance is loaded with addOpeningAdvance, exactly as the opening advances
//     upload of Employee dues › Advances does, with the company's advance limits.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/saltstayz/hrms/internal/calc"
	"github.com/saltstayz/hrms/internal/store"
)

const (
	openUniMaxAmount = 100000
	openUniMaxBack   = 24 // months back an issued month may be
)

var (
	openUniCols = []string{"Employee ID", "Issued month", "Uniform amount (₹)", "Instalments", "Items / note", "Recover"}
	openAdvCols = []string{"Employee ID", "Advance amount (₹)", "Advance given month", "Deduction start month", "Monthly deduction (₹)", "Deduction closing month",
		"Reference / reason"}
)

func init() {
	bulkTemplates = append(bulkTemplates,
		&bulkTemplate{Key: "openuni", Name: "Opening uniform", Cols: openUniCols, Optional: []string{"Instalments", "Items / note", "Recover"},
			Hint: "Uniform given to existing staff before the HRMS, whose recovery may have started under the old system. The instalments run from the issued month: those before the HRMS's first deduction month count as recovered before the HRMS; the rest become Uniform recovery deductions. The full amount is refunded by the refund rule in Employee dues › Setup. Recover No, or staff who are not permanent: the issue is recorded only. The same employee and month is loaded once, so uploading the file again is safe.",
			// Whoever issues uniform with recovery (Employee dues at Edit), or HR Admin.
			allow: func(c *Ctx) bool { return c.Level("emp.assets") >= 2 || c.Role == "HR Admin" },
			lists: map[string]bulkList{"Instalments": bulkFixed("1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"), "Recover": bulkFixed("Yes", "No")},
			note:  openingNote, how: openUniHow, row: bulkOpenUni},
		&bulkTemplate{Key: "openadv", Name: "Opening advances", Cols: openAdvCols, Optional: []string{"Reference / reason"},
			Hint:  "Salary advances given before the HRMS and still being recovered. What was deducted before the HRMS's first deduction month (the monthly deduction × the months from the start month) counts as recovered; the balance is deducted from then on at the monthly amount, the last month taking the remainder. The closing month must match the amount and the monthly deduction. Each row becomes an opening advance, as in Employee dues › Advances.",
			allow: func(c *Ctx) bool { return openingAllowed(c) == nil },
			note:  openingNote, how: openAdvHow, row: bulkOpenAdv})
}

// firstHrmsYm is the first HRMS deduction month: the open payroll month, or the next one once its Deduction closure
// is closed.
func firstHrmsYm(ctx context.Context, q store.Q) string {
	return payLineMonth(ctx, q, "deduction", openYm(ctx, q))
}

func openingNote(ctx context.Context, q store.Q) string {
	f := firstHrmsYm(ctx, q)
	return fmt.Sprintf("The HRMS's first deduction month is %s: instalments before it count as recovered before the HRMS.", ymLabel(f))
}

// ymShort is "Oct 2026".
func ymShort(ym string) string { return ymTime(ym).Format("Jan 2006") }

// partsText is the instalments month by month: "Oct 2026 ₹2,500 · Nov 2026 ₹2,500"; more than 12 are given as runs
// of the same amount ("Nov 2026 to Sep 2027: 11 × ₹5,000").
func partsText(parts []uniPart) string {
	out := []string{}
	if len(parts) <= 12 {
		for _, p := range parts {
			out = append(out, ymShort(p.ym)+" "+calc.Inr(p.amt))
		}
		return strings.Join(out, " · ")
	}
	for i := 0; i < len(parts); {
		j := i
		for j+1 < len(parts) && parts[j+1].amt == parts[i].amt {
			j++
		}
		if j == i {
			out = append(out, ymShort(parts[i].ym)+" "+calc.Inr(parts[i].amt))
		} else {
			out = append(out, fmt.Sprintf("%s to %s: %d × %s", ymShort(parts[i].ym), ymShort(parts[j].ym), j-i+1, calc.Inr(parts[i].amt)))
		}
		i = j + 1
	}
	return strings.Join(out, " · ")
}

func partsSum(parts []uniPart) float64 {
	t := 0.0
	for _, p := range parts {
		t += p.amt
	}
	return calc.Round2(t)
}

// ---------- Opening uniform ----------

// openUniPlan is an opening uniform issue's recovery: every instalment from the issued month, split into those
// recovered before the HRMS (Before) and the HRMS deductions (After), and the refund month.
type openUniPlan struct {
	Parts, Before, After []uniPart
	Refund               string
}

// openingUniformPlan works out the recovery of uniform worth amount issued in month issued. n instalments (1 to 12)
// split the amount equally to the paisa, the last one taking the remainder; n 0 uses Setup's split (uniformPlan:
// firstPct% in the issued month, the rest the next). The refund month follows the refund rule from the real last
// instalment. first is the first HRMS deduction month.
func openingUniformPlan(us uniformSetting, issued string, amount float64, n int, doj, first string) openUniPlan {
	var p openUniPlan
	if n > 0 {
		paise := int64(math.Round(amount * 100))
		each := paise / int64(n)
		for i := 0; i < n; i++ {
			amt := each
			if i == n-1 {
				amt = paise - each*int64(n-1)
			}
			p.Parts = append(p.Parts, uniPart{addYm(issued, i), float64(amt) / 100})
		}
	} else {
		p.Parts, _ = uniformPlan(us, issued, amount, doj)
	}
	if len(p.Parts) == 0 { // nothing to split (an amount of 0 is refused before this)
		return p
	}
	for _, x := range p.Parts {
		if x.ym < first {
			p.Before = append(p.Before, x)
		} else {
			p.After = append(p.After, x)
		}
	}
	p.Refund = uniformRefundYm(us, p.Parts[len(p.Parts)-1].ym, doj)
	return p
}

// openingBefore reads an opening issue's before_hrms: the instalments recovered before the HRMS.
func openingBefore(v any) []uniPart {
	l, _ := v.([]any)
	out := []uniPart{}
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			out = append(out, uniPart{str(m["ym"]), repFloat(m["amount"])})
		}
	}
	return out
}

// openUniIn is one row of the Opening uniform file as read.
type openUniIn struct {
	ym          string
	amount      float64
	instalments int // 0: Setup's split
	note        string
	recover     bool
}

// openUniParse reads a row's cells and lists what is wrong with them on their own. now is today's month.
func openUniParse(v map[string]string, now string) (openUniIn, []string) {
	in, er := openUniIn{recover: true}, []string{}
	switch m := strings.TrimSpace(v["Issued month"]); {
	case m == "":
		er = append(er, "Give the issued month, like 2026-09")
	default:
		ym, ok := openMonth(m)
		switch {
		case !ok:
			er = append(er, "Issued month must be a month, like 2026-09, Sep-2026 or 09/2026")
		case ym > now:
			er = append(er, "Issued month is in the future")
		case ym < addYm(now, -openUniMaxBack):
			er = append(er, fmt.Sprintf("Issued month is more than %d months back (the earliest is %s)", openUniMaxBack, ymLabel(addYm(now, -openUniMaxBack))))
		default:
			in.ym = ym
		}
	}
	if a, ok := openAmount(v["Uniform amount (₹)"]); !ok || a < 1 || a > openUniMaxAmount {
		er = append(er, "Uniform amount must be a number from 1 to 1,00,000")
	} else {
		in.amount = a
	}
	if s := strings.TrimSpace(v["Instalments"]); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) || f < 1 || f > 12 {
			er = append(er, "Instalments must be a whole number from 1 to 12, or blank for the Setup split")
		} else {
			in.instalments = int(f)
		}
	}
	in.note = strings.Join(strings.Fields(v["Items / note"]), " ")
	if len([]rune(in.note)) > 200 {
		er = append(er, "Items / note is longer than 200 characters")
	}
	switch strings.ToLower(strings.TrimSpace(v["Recover"])) {
	case "", "yes", "y", "true", "1":
	case "no", "n", "false", "0":
		in.recover = false
	default:
		er = append(er, "Recover must be Yes or No")
	}
	return in, er
}

// bulkOpenUni is one row of the Opening uniform template.
func bulkOpenUni(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	e, ok := bulkEmployee(ctx, tx, c, r, &res)
	if !ok {
		return res, nil
	}
	status, typ, doj := str(e["status"]), str(e["type"]), str(e["doj"])
	if status == "Pre-boarding" {
		res.Errors = append(res.Errors, res.Name+" is pre-boarding (not joined yet). Opening uniform is for staff already working")
		return res, nil
	}
	in, er := openUniParse(r.V, today()[:7])
	if in.ym != "" && isDate(doj) && in.ym < doj[:7] {
		er = append(er, fmt.Sprintf("Issued month is before the joining month (%s)", ymLabel(doj[:7])))
	}
	if in.ym != "" {
		prev, err := tx.C("uniform_issue").FindOne(ctx, bson.M{"employee_id": res.ID, "opening": true, "issued_ym": in.ym}, options.FindOne().SetProjection(bson.M{"_id": 1}))
		if err != nil {
			return res, err
		}
		if prev != nil {
			er = append(er, fmt.Sprintf("Already loaded as %s (opening uniform for this employee issued in %s)", str(prev["id"]), ymLabel(in.ym)))
		}
	}
	if len(er) > 0 {
		res.Errors = er
		return res, nil
	}
	first := firstHrmsYm(ctx, tx)
	us := loadUniformSetting(ctx, tx)
	recover := in.recover && typ == "Permanent"
	var plan openUniPlan
	if recover {
		plan = openingUniformPlan(us, in.ym, in.amount, in.instalments, doj, first)
		if plan.Refund < first {
			res.Errors = append(res.Errors, fmt.Sprintf("Refund was due in %s, before the HRMS's first deduction month (%s): already refunded before the HRMS; nothing to load",
				ymLabel(plan.Refund), ymLabel(first)))
			return res, nil
		}
	}
	emp, err := tx.C("employee").FindOne(ctx, bson.M{"_id": res.ID}, options.FindOne().SetProjection(bson.M{"lwd": 1}))
	if err != nil {
		return res, err
	}
	lwd := str(emp["lwd"])
	seq, err := store.NextSeq(ctx, tx, "uniform_seq")
	if err != nil {
		return res, err
	}
	id := fmt.Sprintf("UNI-%d", seq)
	doc := bson.M{"_id": id, "employee_id": res.ID, "store_id": nil, "lines": []uniformLine{}, "amount": in.amount, "recover": recover, "by_name": c.Name,
		"issued_on": in.ym + "-01", "refund_rule": nil, "opening": true, "issued_ym": in.ym, "instalments": nil, "note": payitemsNull(in.note), "before_hrms": nil,
		"first_ym": first}
	if in.instalments > 0 {
		doc["instalments"] = in.instalments
	}
	issued := calc.Inr(in.amount) + " in " + ymLabel(in.ym)
	if !recover {
		why := "no salary recovery: Recover is No"
		if in.recover {
			why = "no salary recovery for " + strings.ToLower(typ) + " staff (permanent staff only)"
		}
		if _, err := tx.C("uniform_issue").Insert(ctx, doc); err != nil {
			return res, err
		}
		res.Changes = append(res.Changes, bulkChange{Field: "Uniform issued", To: issued + ", recorded only", Note: why})
		return res, audit(ctx, tx, c, "Opening uniform loaded", "uniform_issue", id, nil, doc)
	}
	before := []map[string]any{}
	for _, p := range plan.Before {
		before = append(before, map[string]any{"ym": p.ym, "amount": p.amt})
	}
	doc["before_hrms"], doc["refund_rule"] = before, refundRuleText(us)
	if _, err := tx.C("uniform_issue").Insert(ctx, doc); err != nil {
		return res, err
	}
	n := len(plan.Parts)
	for i, p := range plan.Parts {
		if p.ym < first {
			continue // recovered before the HRMS
		}
		label := "Uniform recovery"
		if n > 1 {
			label += fmt.Sprintf(" (%d of %d)", i+1, n)
		}
		if err := addLedger(ctx, tx, res.ID, p.ym, "ded", label, p.amt, id, "Opening uniform issued in "+ymLabel(in.ym)); err != nil {
			return res, err
		}
	}
	if err := addLedger(ctx, tx, res.ID, plan.Refund, "earn", uniRefundLabel, in.amount, id, "Refund of uniform recovered earlier"); err != nil {
		return res, err
	}
	// Policy: the refund is paid only to staff still employed in its month; a leaver's is cancelled as on leaving.
	leaver := status == "Inactive" || isDate(lwd) && lwd[:7] < plan.Refund
	if leaver {
		why := uniRefundVoid + " (inactive)"
		if isDate(lwd) {
			why = fmt.Sprintf("%s (last working day %s)", uniRefundVoid, longDate(lwd))
		}
		if _, err := tx.C("ledger").UpdateMany(ctx, bson.M{"source_id": id, "kind": "earn", "void": false},
			bson.M{"$set": bson.M{"void": true, "void_reason": why, "voided_at": tx.Now()}}); err != nil {
			return res, err
		}
	}
	split := fmt.Sprintf("%d instalments", n)
	switch {
	case n == 1:
		split = "1 instalment"
	case in.instalments == 0:
		split += fmt.Sprintf(" (Setup split %s%%)", strconv.FormatFloat(us.FirstPct, 'f', -1, 64))
	}
	res.Changes = append(res.Changes, bulkChange{Field: "Uniform issued", To: issued + ", " + split, Note: in.note})
	if len(plan.Before) > 0 {
		res.Changes = append(res.Changes, bulkChange{Field: "Recovered before HRMS", To: calc.Inr(partsSum(plan.Before)) + ": " + partsText(plan.Before)})
	} else {
		res.Changes = append(res.Changes, bulkChange{Field: "Recovered before HRMS", To: "nothing"})
	}
	if len(plan.After) > 0 {
		res.Changes = append(res.Changes, bulkChange{Field: "HRMS deductions", To: partsText(plan.After)})
	} else {
		res.Changes = append(res.Changes, bulkChange{Field: "HRMS deductions", To: "none: all recovered before the HRMS"})
	}
	if leaver {
		left := "inactive"
		if isDate(lwd) {
			left = "last working day " + longDate(lwd)
		}
		res.Changes = append(res.Changes, bulkChange{Field: "Refund", To: fmt.Sprintf("not paid (%s was due in %s)", calc.Inr(in.amount), ymLabel(plan.Refund)),
			Note: "left before the refund month: " + left})
	} else {
		res.Changes = append(res.Changes, bulkChange{Field: "Refund", To: calc.Inr(in.amount) + " in " + ymLabel(plan.Refund), Note: refundRuleText(us)})
	}
	return res, audit(ctx, tx, c, "Opening uniform loaded", "uniform_issue", id, nil, map[string]any{"issue": doc, "refundYm": plan.Refund, "refundCancelled": leaver})
}

// ---------- Opening advances ----------

// openAdvPlan is an opening advance worked out from the file: the instalments in all from the start month, those
// before the first HRMS month (recovered), and the balance with its HRMS instalments.
type openAdvPlan struct {
	Amount, Monthly       float64
	Given, Start, Closing string
	N                     int     // instalments in all
	Last                  float64 // the last instalment (the remainder)
	Before                int     // instalments before the first HRMS month
	Recovered, Balance    float64
	HrmsStart             string // the first HRMS instalment's month
	HrmsN                 int
}

// openingAdvancePlan checks an opening advance and works it out. first is the first HRMS deduction month, now
// today's month. The closing month must be start + ceil(amount ÷ monthly) − 1.
func openingAdvancePlan(v map[string]string, first, now string) (openAdvPlan, []string) {
	var p openAdvPlan
	er := []string{}
	var okA, okM bool
	if p.Amount, okA = openAmount(v["Advance amount (₹)"]); !okA || p.Amount <= 0 {
		okA = false
		er = append(er, "Advance amount must be a number above 0")
	}
	if p.Monthly, okM = openAmount(v["Monthly deduction (₹)"]); !okM || p.Monthly <= 0 {
		okM = false
		er = append(er, "Monthly deduction must be a number above 0")
	}
	month := func(col string) string {
		s := strings.TrimSpace(v[col])
		if s == "" {
			er = append(er, "Give the "+strings.ToLower(col[:1])+col[1:]+", like 2026-09")
			return ""
		}
		ym, ok := openMonth(s)
		if !ok {
			er = append(er, col+" must be a month, like 2026-09, Sep-2026 or 09/2026")
			return ""
		}
		return ym
	}
	p.Given, p.Start, p.Closing = month("Advance given month"), month("Deduction start month"), month("Deduction closing month")
	if p.Given != "" && p.Given > now {
		er = append(er, "Advance given month is in the future")
	}
	if p.Given != "" && p.Start != "" && p.Start < p.Given {
		er = append(er, fmt.Sprintf("Deduction start month (%s) is before the month the advance was given (%s)", ymLabel(p.Start), ymLabel(p.Given)))
	}
	if !okA || !okM || p.Start == "" {
		return p, er
	}
	p.N = recoveryMonths(p.Amount, p.Monthly)
	if p.N > openMaxMonths {
		return p, append(er, fmt.Sprintf("Recovery would take %d months; the most is %d. Check the monthly deduction", p.N, openMaxMonths))
	}
	p.Last = calc.Round2(p.Amount - p.Monthly*float64(p.N-1))
	want := addYm(p.Start, p.N-1)
	if p.Closing != "" && p.Closing != want {
		last := ""
		if p.Last != p.Monthly {
			last = fmt.Sprintf(", the last %s", calc.Inr(p.Last))
		}
		er = append(er, fmt.Sprintf("Deduction closing month should be %s: %s at %s a month is %d instalment%s from %s%s. The file says %s",
			ymLabel(want), calc.Inr(p.Amount), calc.Inr(p.Monthly), p.N, map[bool]string{true: "", false: "s"}[p.N == 1], ymLabel(p.Start), last, ymLabel(p.Closing)))
	}
	p.Before = min(monthsBetween(p.Start, first), p.N)
	p.Recovered = calc.Round2(p.Monthly * float64(p.Before))
	if p.Before == p.N {
		p.Recovered = p.Amount
	}
	p.Balance = calc.Round2(p.Amount - p.Recovered)
	if p.Balance <= 0 {
		er = append(er, fmt.Sprintf("Nothing left to recover: the last deduction (%s) is before the HRMS's first deduction month (%s)", ymLabel(want), ymLabel(first)))
		return p, er
	}
	p.HrmsStart, p.HrmsN = addYm(p.Start, p.Before), p.N-p.Before
	return p, er
}

// instalments lists the plan's instalments from instalment from (0-based) on.
func (p openAdvPlan) instalments(from, to int) []uniPart {
	out := []uniPart{}
	for i := from; i < to; i++ {
		amt := p.Monthly
		if i == p.N-1 {
			amt = p.Last
		}
		out = append(out, uniPart{addYm(p.Start, i), amt})
	}
	return out
}

// bulkOpenAdv is one row of the Opening advances template.
func bulkOpenAdv(s *Server, ctx context.Context, tx store.Q, c *Ctx, r bulkRow, apply bool) (bulkResult, error) {
	var res bulkResult
	if _, ok := bulkEmployee(ctx, tx, c, r, &res); !ok {
		return res, nil
	}
	first := firstHrmsYm(ctx, tx)
	p, er := openingAdvancePlan(r.V, first, today()[:7])
	ref := strings.Join(strings.Fields(r.V["Reference / reason"]), " ")
	if len([]rune(ref)) > 200 {
		er = append(er, "Reference / reason is longer than 200 characters")
	}
	if ref == "" && p.Given != "" {
		ref = "Opening advance given " + ymLabel(p.Given) // also the key that stops the same advance loading twice
	}
	if len(er) > 0 {
		res.Errors = er
		return res, nil
	}
	if _, err := tx.C("employee").Lock(ctx, bson.M{"_id": res.ID}); err != nil {
		return res, err
	}
	ln := &openLine{Line: r.Line, EmployeeID: res.ID, AdvanceDate: p.Given, Original: p.Amount, Balance: p.Balance, Monthly: p.Monthly, StartYm: p.HrmsStart,
		Reference: ref, Months: p.HrmsN, Last: p.Last, plan: &p}
	// No company limits (one open at a time, a multiple of the gross): the advance was already given under the old
	// system, so refusing it would only lose its recovery. Same as the opening upload of Employee dues › Advances.
	er = openJudgeDB(ctx, tx, c, ln)
	if len(er) > 0 {
		res.Errors = er
		return res, nil
	}
	id, err := s.addOpeningAdvance(ctx, tx, c, ln)
	if err != nil {
		return res, err
	}
	all := fmt.Sprintf("%d × %s", p.N, calc.Inr(p.Monthly))
	if p.Last != p.Monthly {
		all = fmt.Sprintf("%d × %s, then %s", p.N-1, calc.Inr(p.Monthly), calc.Inr(p.Last))
	}
	if p.N == 1 {
		all = calc.Inr(p.Last)
	}
	res.Changes = append(res.Changes, bulkChange{Field: "Advance", To: fmt.Sprintf("%s given %s: %s, %s to %s", calc.Inr(p.Amount), ymLabel(p.Given), all, ymShort(p.Start), ymShort(addYm(p.Start, p.N-1))),
		Note: ref})
	if p.Before > 0 {
		res.Changes = append(res.Changes, bulkChange{Field: "Recovered before HRMS", To: calc.Inr(p.Recovered) + ": " + partsText(p.instalments(0, p.Before))})
	} else {
		res.Changes = append(res.Changes, bulkChange{Field: "Recovered before HRMS", To: "nothing"})
	}
	res.Changes = append(res.Changes, bulkChange{Field: "HRMS deductions", To: partsText(p.instalments(p.Before, p.N))},
		bulkChange{Field: "Balance to recover", To: calc.Inr(p.Balance), Note: map[bool]string{true: id, false: ""}[apply]})
	return res, nil
}

// ---------- "How to fill" ----------

func openUniHow(ctx context.Context, q store.Q) []string {
	first, us := firstHrmsYm(ctx, q), loadUniformSetting(ctx, q)
	pct := strconv.FormatFloat(us.FirstPct, 'f', -1, 64)
	return []string{
		"## Columns",
		"Employee ID: as in the HRMS, for example EMP-1020. The employee must have joined (not Pre-boarding).",
		"Issued month: the month the uniform was given, as 2026-09, Sep-2026 or 09/2026 (an Excel date works too). Not in the future and not more than 24 months back.",
		"Uniform amount (₹): the value of the uniform, from 1 to 1,00,000.",
		"Instalments: optional, 1 to 12. The amount is split equally to the paisa, month by month from the issued month, the last instalment taking the remainder. Blank: the split in Employee dues › Setup (now " + pct + "% in the issued month, the rest the next month).",
		"Items / note: optional, for example \"2 shirts, 1 trousers\". Shown on the employee's Uniform card.",
		"Recover: Yes (the default when blank) or No. No records the issue only: no salary deduction and no refund. Uniform is recovered from permanent staff only; for other staff the issue is recorded only.",
		"",
		"## How the months are treated",
		"The first HRMS deduction month is now " + ymLabel(first) + " (the open payroll month, or the next one once its Deduction closure is closed).",
		"Instalments before it were recovered under the old system: no pay line is made, and the employee's Uniform card shows them as \"recovered before HRMS\".",
		"Instalments in it or later become \"Uniform recovery\" deductions on the payslip.",
		"The refund (\"Uniform deposit refund\", the full amount) is scheduled by the refund rule in Employee dues › Setup, now " + refundRuleText(us) + ", counted from the real last instalment. A row whose refund month is before the first HRMS month is refused: it was already refunded before the HRMS.",
		"Someone inactive, or whose last working day is before the refund month, gets no refund, as for any leaver; the full and final shows \"Uniform deposit refund not paid\".",
		"",
		"## Example (the owner's case)",
		"EMP-1020 | 2026-09 | 5000 | 2 | Shirt and trousers | Yes",
		"Uniform worth ₹5,000 given in September 2026, recovered in 2 instalments of ₹2,500: September and October 2026. The HRMS starts deducting in October 2026, so September's ₹2,500 counts as recovered before the HRMS and only ₹2,500 is deducted, from October salary. The ₹5,000 refund is paid by the refund rule: with 6 months after the last deduction, in April 2027.",
		"",
		"## What happens",
		"1. Upload the file in Bulk update › Opening uniform and press Check file. Each row shows what was recovered before the HRMS, the HRMS deductions month by month and the refund month, or why it is refused. Nothing changes until you apply.",
		"2. A row is refused when the Employee ID is unknown, outside your access or pre-boarding; the amount is not 1 to 1,00,000; the month is in the future, more than 24 months back or before the joining month; the instalments are not 1 to 12; the refund was already due before the HRMS; or the same employee and issued month is already loaded (so uploading the file again is safe).",
		"3. Apply adds every good row: a uniform issue marked Opening balance, with no store and no stock movement, shown on the employee's Uniform card with its schedule, what is recovered and the refund.",
		"Up to 5,000 rows a file. Excel (.xlsx) or CSV.",
	}
}

func openAdvHow(ctx context.Context, q store.Q) []string {
	first := firstHrmsYm(ctx, q)
	return []string{
		"## Columns",
		"Employee ID: as in the HRMS, for example EMP-1020. The employee must be active and permanent.",
		"Advance amount (₹): the advance given, for example 30000.",
		"Advance given month: the month it was paid, as 2026-06, Jun-2026 or 06/2026. Not in the future.",
		"Deduction start month: the first month it was (or will be) deducted from salary. Not before the given month.",
		"Monthly deduction (₹): deducted each month. The last month takes whatever is left, so it can be smaller.",
		"Deduction closing month: the last deduction month. It must be the start month + (amount ÷ monthly, rounded up) − 1; if it is not, the check gives the month the system expects.",
		"Reference / reason: optional, for example the old voucher number. When blank, \"Opening advance given <month>\" is used. The same employee and reference is loaded only once.",
		"",
		"## How the months are treated",
		"The first HRMS deduction month is now " + ymLabel(first) + ".",
		"Already recovered = the monthly deduction × the months from the start month up to the month before it.",
		"Balance = the advance amount − what was already recovered. It is deducted from the first HRMS month (or the start month, if later) at the monthly amount until the closing month, the last month taking the remainder.",
		"Nothing is added to pay: the advance was paid before the HRMS.",
		"",
		"## Examples",
		"EMP-1020 | 30000 | 2026-06 | 2026-07 | 5000 | 2026-12 | Voucher 2231",
		"6 instalments of ₹5,000, July to December 2026. With October 2026 as the first HRMS month, July, August and September (₹15,000) were recovered before the HRMS; ₹15,000 is left, deducted ₹5,000 in October, November and December 2026.",
		"EMP-1016 | 12000 | 2026-07 | 2026-08 | 5000 | 2026-10 |",
		"3 instalments: ₹5,000 in August and September, ₹2,000 in October. With October 2026 as the first HRMS month, ₹10,000 was recovered and ₹2,000 is deducted in October.",
		"",
		"## What happens",
		"1. Upload the file in Bulk update › Opening advances and press Check file. Each row shows what was recovered before the HRMS, the HRMS deductions month by month and the balance, or why it is refused. Nothing changes until you apply.",
		"2. A row is refused when the Employee ID is unknown, not active or not permanent; nothing is left to recover; the start month is before the given month; the closing month does not match; the given month is in the future; the recovery takes more than 60 months; the same employee and reference is already loaded; or the same employee and reference is already loaded. The company's advance limits (one at a time, a multiple of the gross) are not applied: these advances were already given.",
		"3. Apply adds every good row as an approved advance tagged Opening balance, exactly as Employee dues › Advances › Opening advances does. Skip next and Recover balance work on it; if the employee leaves, the balance is recovered in the full and final.",
		"Up to 5,000 rows a file. Excel (.xlsx) or CSV.",
	}
}

func lvOpenHow(ctx context.Context, q store.Q) []string {
	return []string{
		"## Columns",
		"Employee ID: as in the HRMS, for example EMP-1020.",
		"Leave type: a leave type with a balance (from the drop-down).",
		"Balance: the days the employee has on the As at date.",
		"As at: the date of the balance, for example 2026-10-31. Up to today, and not before joining.",
		"",
		"## How it works",
		"Give the balance as at the date, including all leave accrued up to that month. From the next month, leave keeps accruing by the leave plan (e.g. CL 12 a year adds 1 a month).",
		"Any date in a month works the same: 1 October and 31 October both mean the balance includes October's credit, and accrual starts in November.",
		"Leave credited upfront (all at the start of the leave year) gets nothing more after the As at date until the next leave year.",
		"Leave taken before the As at date is not counted again; leave from that date on is taken from the balance.",
		"",
		"",
		"## Optional holidays",
		"Optional Holiday Balance: the optional holidays the employee can still take in the As at date's calendar year (0 to 31, halves allowed). Optional holidays are not a leave type: the work policy gives each employee a number a year (Policies › Holiday rules), chosen from the holiday calendar. From 1 January of the next year the work policy's number applies again.",
		"A blank cell leaves that balance as it is. Give the optional holiday balance on any one row of the employee (or a row with only Employee ID, As at and Optional Holiday Balance).",
		"",
		"## Example",
		"EMP-1020 | Casual leave | 8 | 2026-10-31 | 1",
		"With casual leave at 12 days a year, credited monthly, the balance is 9 on 15 November 2026 and 10 on 1 December 2026. With 2 optional holidays a year and one already taken in August, 1 is left for the rest of 2026.",
	}
}
