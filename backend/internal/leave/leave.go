// Package leave holds leave plans (spec 10.2) and the pure leave rules: leave year, balance,
// which dates count, and the notice and back-dating windows. Ported from the prototype
// (leaveYear, leaveBalance, ltCover, winErrorsBase).
package leave

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/saltstayz/hrms/internal/policy"
)

// TypeCfg is one leave type inside a plan (prototype ltDefault).
type TypeCfg struct {
	On       bool     `json:"on"`
	Types    []string `json:"types"`  // employment types
	Gender   string   `json:"gender"` // all | Female | Male
	MinSvc   int      `json:"minSvc"` // months of service
	InProb   bool     `json:"inProb"`
	Days     float64  `json:"days"`
	Credit   string   `json:"credit"`            // monthly | upfront; a type in an LV.Earn rule is earned only by paid days
	PerDays  int      `json:"perDays,omitempty"` // read only: the 24 Nov per-type shape (Credit "worked"), see Normalize
	From     string   `json:"from"`              // doj | prob: when accrual starts
	Catchup  bool     `json:"catchup"`
	CF       float64  `json:"cf"`
	Encash   bool     `json:"encash"`
	Times    int      `json:"times"` // event types: how many times in service (0 = no limit)
	Adv      int      `json:"adv"`   // days of advance notice
	Back     int      `json:"back"`  // days it can be applied after the date
	AdvOnly  bool     `json:"advOnly"`
	Offs     string   `json:"offs"` // exclude | sandwich | include
	MaxReq   float64  `json:"maxReq"`
	MaxMonth float64  `json:"maxMonth"`
	DocAfter float64  `json:"docAfter"`
	DocLabel string   `json:"docLabel"`
	OnNotice string   `json:"onNotice"` // allow | restrict
	ApplyBy  string   `json:"applyBy"`  // self | hr
	Blk      bool     `json:"blk"`      // blackout periods apply
	Grp      string   `json:"grp"`      // approval matrix: C (casual and sick) or P (privilege and other)
}

type LV struct {
	Year    string  `json:"year"` // cal | fin | anniv
	Prorate bool    `json:"prorate"`
	Half    bool    `json:"half"`
	Neg     float64 `json:"neg"`
	PlEnc   bool    `json:"plEnc"`
	CoValid int     `json:"coValid"` // days a comp-off credit stays usable; 0 = 90
	// Short leave (owner, 20 Nov 2026): a window of up to ShortH hours on one day, ShortN times a month; no balance.
	// Zero values read as the defaults (2 a month, 2 hours each); ShortOff switches it off for the plan.
	ShortOff bool    `json:"shortOff,omitempty"`
	ShortN   int     `json:"shortN,omitempty"`
	ShortH   float64 `json:"shortH,omitempty"`
	// Earn by working days (owner, 24 Nov 2026): the plan's rules; a type in a rule is earned only this way, on paid days.
	Earn []EarnRule `json:"earn,omitempty"`
}

// EarnRule is one "Earn by working days" rule: every time the employee completes Days paid days (owner, 24 Nov 2026,
// evening: "Working days means Paid days for leaves"), each listed leave type is credited its own Days from the next
// day. Both numbers are the admin's; there are no defaults.
type EarnRule struct {
	Days  float64    `json:"days"`
	Types []EarnType `json:"types"`
}

// EarnType is a leave type in a rule and the days of it one block earns.
type EarnType struct {
	Type string  `json:"type"`
	Days float64 `json:"days"`
}

// Block is one leave type's share of an Earn by working days rule: Per days of leave for every Every paid days.
type Block struct {
	Every float64 `json:"every"`
	Per   float64 `json:"per"`
}

// BlockFor is the rule the type name is earned by, and whether it is in one. A type saved with the per-type shape of
// the morning of 24 Nov (Credit "worked", 1 day for every PerDays days worked, blank 20) reads as its own rule.
func (lv LV) BlockFor(name string, c TypeCfg) (Block, bool) {
	for _, r := range lv.Earn {
		for _, t := range r.Types {
			if t.Type == name {
				return Block{Every: r.Days, Per: t.Days}, true
			}
		}
	}
	if c.Credit == creditWorkedOld {
		return Block{Every: float64(oldEvery(c)), Per: 1}, true
	}
	return Block{}, false
}

// Text is the block as people say it, "1 day for every 7 paid days", with ", up to 8 days a year" when capDays
// (the type's Days a year) is set.
func (b Block) Text(capDays float64) string {
	s := fmt.Sprintf("%s for every %s", dayWord(b.Per, "day"), dayWord(b.Every, "paid day"))
	if capDays > 0 {
		s += fmt.Sprintf(", up to %s a year", dayWord(capDays, "day"))
	}
	return s
}

func dayWord(v float64, w string) string {
	if v == 1 {
		return "1 " + w
	}
	return strconv.FormatFloat(v, 'f', -1, 64) + " " + w + "s"
}

// creditWorkedOld is the per-type Credit value of the morning of 24 Nov, replaced by the plan's Earn rules.
const creditWorkedOld = "worked"

func oldEvery(c TypeCfg) int {
	if c.PerDays <= 0 {
		return 20
	}
	return c.PerDays
}

// Normalize turns a plan saved with the per-type shape (Credit "worked" with PerDays) into an Earn by working days
// rule of its own (PerDays paid days → 1 day; Days a year stays its cap) and clears the per-type fields, so the
// plan editor shows the rule and the next save stores the new shape. Plans without it are left as they are.
func Normalize(lv *LV, types map[string]TypeCfg) {
	names := make([]string, 0, len(types))
	for n := range types {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := types[n]
		if c.Credit != creditWorkedOld && c.PerDays == 0 {
			continue
		}
		if _, in := lv.BlockFor(n, TypeCfg{}); c.Credit == creditWorkedOld && !in {
			lv.Earn = append(lv.Earn, EarnRule{Days: float64(oldEvery(c)), Types: []EarnType{{Type: n, Days: 1}}})
		}
		if c.Credit == creditWorkedOld {
			c.Credit = CreditMonthly
		}
		c.PerDays = 0
		types[n] = c
	}
}

// halves: a whole or half number.
func halves(v float64) bool { return v*2 == math.Round(v*2) }

// EarnProblem is what is wrong with the plan's Earn by working days rules, "" when nothing is. kind gives a leave
// type's kind (balance, event, none).
func EarnProblem(lv LV, types map[string]TypeCfg, kind func(string) string) string {
	seen := map[string]float64{}
	for i, r := range lv.Earn {
		if r.Days <= 0 || r.Days > 366 || !halves(r.Days) {
			return fmt.Sprintf("Earn by working days, rule %d: enter the paid days, more than 0 and up to 366, in whole or half days", i+1)
		}
		if len(r.Types) == 0 {
			return fmt.Sprintf("Earn by working days, rule for every %v paid days: add at least one leave type", r.Days)
		}
		for _, t := range r.Types {
			if t.Type == "" {
				return fmt.Sprintf("Earn by working days, rule for every %v paid days: choose the leave type of each row", r.Days)
			}
			if _, ok := types[t.Type]; !ok || kind(t.Type) != KindBalance {
				return fmt.Sprintf("Earn by working days: %s is not a leave type with a balance in this plan", t.Type)
			}
			if t.Days <= 0 || t.Days > 366 || !halves(t.Days) {
				return fmt.Sprintf("Earn by working days, %s: enter the days earned, more than 0, in whole or half days", t.Type)
			}
			if w, dup := seen[t.Type]; dup {
				return fmt.Sprintf("%s is already in the rule for every %v paid days. A leave type can be in only one rule", t.Type, w)
			}
			seen[t.Type] = r.Days
		}
	}
	return ""
}

// ShortLeaveType is the name short leave requests carry (payload.ltype); it is not a leave type with a balance.
const ShortLeaveType = "Short leave"

// Short is the plan's short leave rule: allowed, how many a month and the most hours each.
func (lv LV) Short() (on bool, perMonth int, maxH float64) {
	perMonth, maxH = lv.ShortN, lv.ShortH
	if perMonth <= 0 {
		perMonth = 2
	}
	if maxH <= 0 {
		maxH = 2
	}
	return !lv.ShortOff, perMonth, maxH
}

// ShortErrors are the short leave rules for a window from–to (HH:MM, minutes after midnight) when taken already holds
// the short leaves pending or approved in the same month.
func ShortErrors(lv LV, from, to int, taken int) []string {
	on, n, maxH := lv.Short()
	if !on {
		return []string{"Short leave is not allowed in this leave plan"}
	}
	er := []string{}
	switch {
	case to <= from:
		er = append(er, "The to-time must be after the from-time")
	case float64(to-from) > maxH*60+1e-9:
		er = append(er, fmt.Sprintf("Short leave is at most %s at a time. This request is %s", hoursWord(maxH*60), hoursWord(float64(to-from))))
	}
	if taken >= n {
		er = append(er, fmt.Sprintf("Short leave is allowed %d time%s a month and %d %s already taken or waiting this month", n, plural(n), taken,
			map[bool]string{true: "is", false: "are"}[taken == 1]))
	}
	return er
}

// hoursWord is minutes as people say them: "2 hours", "1 hour 30 minutes".
func hoursWord(m float64) string {
	h, mm := int(m)/60, int(m)%60
	switch {
	case mm == 0:
		return fmt.Sprintf("%d hour%s", h, plural(h))
	case h == 0:
		return fmt.Sprintf("%d minutes", mm)
	}
	return fmt.Sprintf("%d hour%s %d minutes", h, plural(h), mm)
}

type LRX struct {
	StaffOn  bool   `json:"staffOn"`
	StaffPct int    `json:"staffPct"`
	Sandwich bool   `json:"sandwich"`
	Club     string `json:"club"` // ok | no
}

// How a balance type is credited (TypeCfg.Credit) when it is in no Earn by working days rule. Anything else reads as
// monthly.
const (
	CreditMonthly = "monthly"
	CreditUpfront = "upfront"
)

// CreditProblem is what is wrong with a type's credit setting, "" when nothing is (after Normalize).
func (c TypeCfg) CreditProblem() string {
	if c.Credit != "" && c.Credit != CreditMonthly && c.Credit != CreditUpfront {
		return "choose Monthly or Upfront for the year"
	}
	return ""
}

// Kind of a leave type in the catalogue.
const (
	KindBalance = "balance"
	KindNone    = "none"
	KindEvent   = "event"
)

// Default settings for a type of a kind (prototype ltDefault).
func Default(kind string) TypeCfg {
	days := 0.0
	switch kind {
	case KindEvent:
		days = 5
	case KindBalance:
		days = 6
	}
	return TypeCfg{On: true, Types: []string{"Permanent"}, Gender: "all", InProb: true, Days: days, Credit: "monthly", From: "doj", Back: 7,
		Offs: "exclude", DocLabel: "Supporting document", OnNotice: "allow", ApplyBy: "self", Blk: true, Grp: "P"}
}

// ---------- Leave year and balance ----------

func monthsDiff(a, b time.Time) int { return (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month()) }
func firstOf(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC) }
func endOf(t time.Time) time.Time   { return firstOf(t).AddDate(0, 1, -1) }

// Year is the leave year that contains on.
func Year(kind, doj, on string) (time.Time, time.Time) {
	d := policy.D(on)
	switch kind {
	case "fin":
		s := d.Year()
		if d.Month() < time.April {
			s--
		}
		return time.Date(s, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(s+1, 3, 31, 0, 0, 0, 0, time.UTC)
	case "anniv":
		j := policy.D(doj)
		s := time.Date(d.Year(), j.Month(), j.Day(), 0, 0, 0, 0, time.UTC)
		if s.After(d) {
			s = s.AddDate(-1, 0, 0)
		}
		return s, s.AddDate(1, 0, -1)
	}
	return time.Date(d.Year(), 1, 1, 0, 0, 0, 0, time.UTC), time.Date(d.Year(), 12, 31, 0, 0, 0, 0, time.UTC)
}

// Opening is an imported balance as at a date; monthly credit continues after it.
type Opening struct {
	At      string
	Balance float64
}

// Taken is one request for the type: its start date and counted days.
type Taken struct {
	From string
	Days float64
}

type Balance struct {
	On          bool    `json:"on"`
	Credit      float64 `json:"credit"`
	Used        float64 `json:"used"`
	Pending     float64 `json:"pending"`
	Bal         float64 `json:"bal"`
	Carry       float64 `json:"carry,omitempty"` // carried forward from the previous leave year (part of Credit)
	Neg         float64 `json:"neg"`
	InProbBlock bool    `json:"inProbBlock"`
	Worked      *Earned `json:"worked,omitempty"` // a type in an Earn by working days rule: how this year's credit was earned
}

// Worked is the attendance a type in an Earn by working days rule is earned on (owner, 24 Nov 2026, evening: "Working
// days means Paid days for leaves"). Days holds the paid days of each date as the attendance engine settles it
// (attend.DayOut.Paid: 1 less the day's loss of pay, so present, late, paid leave, a week off and a holiday 1, a half
// day 0.5, absent and loss of pay 0). Through is the last date that counts (see Through); "" when none does (someone
// not active).
type Worked struct {
	Days    map[string]float64
	Through string
}

// Earned is how an Earn by working days credit was earned in the leave year.
type Earned struct {
	Every   float64       `json:"every"`   // paid days in one block (the rule's number)
	Per     float64       `json:"per"`     // days of this type one block earns
	Days    float64       `json:"days"`    // paid days counted this leave year, up to Through
	Blocks  int           `json:"blocks"`  // whole blocks completed
	Left    float64       `json:"left"`    // paid days counted toward the next block
	ToNext  float64       `json:"toNext"`  // paid days still needed to complete the next block
	Earned  float64       `json:"earned"`  // Blocks x Per, before the cap
	Cap     float64       `json:"cap"`     // Days a year when set (0: no cap); with an opening, the share of the year left
	Credit  float64       `json:"credit"`  // Earned up to Cap: what is credited (an opening balance comes on top)
	Through string        `json:"through"` // paid days counted up to this date ("" when none counts yet)
	Credits []EarnedOn    `json:"credits"` // each credit, from the day after the paid day that completed its block
	Months  []EarnedMonth `json:"months"`  // paid days counted in each month
}

// EarnedOn is one credit of an Earn by working days type: the date it is credited from (the day after the paid day
// that completed the block), the blocks completed that day and the days credited (after the cap).
type EarnedOn struct {
	On     string  `json:"on"`
	Blocks int     `json:"blocks"`
	Credit float64 `json:"credit"`
}

// EarnedMonth is the paid days counted in one month of the leave year.
type EarnedMonth struct {
	Month string  `json:"month"` // 2026-11
	Days  float64 `json:"days"`
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

// blocks is how many whole blocks of every paid days fit in days (both whole or half days: counted in halves).
func blocks(days, every float64) int {
	u := int(math.Round(every * 2))
	if u <= 0 {
		return 0
	}
	return int(math.Round(days*2)) / u
}

// Through is the last date whose paid days count for a balance on the date on (owner, 24 Nov 2026, evening: credited
// as soon as the days are complete, not at the month end): the day before on, and never today or later (a day counts
// once it is over), so a block completed on a day is credited from the next day. Nothing after a leaver's last working
// day lwd counts; a balance on the last working day itself (F&F) counts that day once it has passed.
func Through(on, today, lwd string) string {
	t := min(policy.AddDays(on, -1), policy.AddDays(today, -1))
	if lwd != "" && (lwd < t || (lwd == on && lwd < today)) {
		t = lwd
	}
	return t
}

// earn is the Earn by working days credit of the leave year ys..ye on at: the paid days from the accrual start
// (joining, or the end of probation unless it is back-dated at confirmation) up to wk.Through, counted day by day
// across the leave year, so a block can span months; each time the count reaches a whole block of blk.Every days,
// blk.Per is credited from the next day, the leftover counts toward the next block, and the count starts again with
// the next leave year. Capped at Days a year when it is set. With an opening balance only days after it count, and
// the cap is the share of the year left after it.
func earn(c TypeCfg, blk Block, wk *Worked, ys, ye, dj, pe, at time.Time, op *Opening) Earned {
	e := Earned{Every: blk.Every, Per: blk.Per, Cap: c.Days, ToNext: blk.Every, Credits: []EarnedOn{}, Months: []EarnedMonth{}}
	after := ""
	if op != nil {
		after = op.At
		if c.Days > 0 {
			yr, left := monthsDiff(ys, ye)+1, monthsDiff(firstOf(policy.D(op.At).AddDate(0, 0, 1)), ye)+1
			e.Cap = r2(c.Days * float64(max(0, min(left, yr))) / float64(yr))
		}
	}
	if wk == nil {
		return e
	}
	from := ys
	if dj.After(from) {
		from = dj
	}
	if c.From == "prob" && !(c.Catchup && !pe.After(at)) && pe.After(from) {
		from = pe
	}
	to := ye
	if at.Before(to) {
		to = at
	}
	f := from.Format("2006-01-02")
	thr := min(to.Format("2006-01-02"), wk.Through)
	if thr < f { // also when nothing counts (Through "")
		return e
	}
	e.Through = thr
	dates := make([]string, 0, len(wk.Days))
	for d, w := range wk.Days {
		if d >= f && d <= thr && d > after && w > 0 {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)
	capped := func(v float64) float64 {
		if e.Cap > 0 {
			return math.Min(v, e.Cap)
		}
		return v
	}
	early := "" // back-dated at confirmation: blocks completed in probation are credited when it ends
	if c.From == "prob" && c.Catchup {
		early = pe.Format("2006-01-02")
	}
	got := 0.0
	for _, d := range dates {
		w := wk.Days[d]
		e.Days += w
		if n := len(e.Months); n == 0 || e.Months[n-1].Month != d[:7] {
			e.Months = append(e.Months, EarnedMonth{Month: d[:7]})
		}
		e.Months[len(e.Months)-1].Days += w
		if n := blocks(e.Days, blk.Every); n > e.Blocks {
			if now := capped(float64(n) * blk.Per); now > got {
				x := EarnedOn{On: max(policy.AddDays(d, 1), early), Blocks: n - e.Blocks, Credit: r2(now - got)}
				if k := len(e.Credits) - 1; k >= 0 && e.Credits[k].On == x.On {
					x.Blocks, x.Credit = x.Blocks+e.Credits[k].Blocks, r2(x.Credit+e.Credits[k].Credit)
					e.Credits = e.Credits[:k]
				}
				e.Credits = append(e.Credits, x)
				got = now
			}
			e.Blocks = n
		}
	}
	if blk.Every > 0 {
		e.Left = e.Days - float64(e.Blocks)*blk.Every
		e.ToNext = blk.Every - e.Left
	}
	e.Earned = float64(e.Blocks) * blk.Per
	e.Credit = capped(e.Earned)
	return e
}

// Compute is leaveBalance for one balance type on a date, for a type in no Earn by working days rule (see ComputeWith).
func Compute(c TypeCfg, lv LV, probMonths int, doj, on string, open *Opening, approved, pending []Taken) Balance {
	return ComputeWith("", c, lv, probMonths, doj, on, open, approved, pending, nil)
}

// ComputeWith is the balance of the plan's type name on a date. A type in an Earn by working days rule is credited
// only by its blocks of paid days wk (no monthly or upfront credit); other types ignore wk.
func ComputeWith(name string, c TypeCfg, lv LV, probMonths int, doj, on string, open *Opening, approved, pending []Taken, wk *Worked) Balance {
	if !c.On {
		return Balance{}
	}
	ys, ye := Year(lv.Year, doj, on)
	dj, at := policy.D(doj), policy.D(on)
	pe := dj.AddDate(0, probMonths, 0)
	start := ys
	if dj.After(ys) {
		start = firstOf(dj)
	}
	accrues := func(m time.Time) bool { return c.From == "doj" || !endOf(m).Before(pe) }
	blk, byWork := lv.BlockFor(name, c)
	monthly := c.Credit != CreditUpfront && !byWork
	var op *Opening
	if open != nil {
		if oa := policy.D(open.At); !oa.Before(ys) && !oa.After(ye) {
			op = open
		}
	}
	credit := 0.0
	var earned *Earned
	switch {
	case byWork:
		e := earn(c, blk, wk, ys, ye, dj, pe, at, op)
		earned = &e
		if op != nil {
			credit = op.Balance
		}
		credit += e.Credit
	case op != nil:
		credit = op.Balance
		if monthly {
			s2 := firstOf(policy.D(op.At)).AddDate(0, 1, 0)
			for i, n := 0, max(0, monthsDiff(s2, at)+1); i < n; i++ {
				if m := s2.AddDate(0, i, 0); accrues(m) {
					credit += c.Days / 12
				}
			}
		}
	case !monthly:
		yr := monthsDiff(ys, ye) + 1
		left := monthsDiff(start, ye) + 1
		if lv.Prorate && dj.After(ys) {
			credit = c.Days * float64(min(left, yr)) / float64(yr)
		} else {
			credit = c.Days
		}
	default:
		for i, n := 0, max(0, monthsDiff(start, at))+1; i < n; i++ {
			if m := start.AddDate(0, i, 0); accrues(m) {
				credit += c.Days / 12
			}
		}
		if c.From == "prob" && c.Catchup && pe.After(start) && !pe.After(at) {
			credit += c.Days / 12 * float64(max(0, monthsDiff(start, pe)))
		}
	}
	// Carry forward (spec 10.2): the unused balance at the end of the previous leave year, up to the type's
	// limit, when the employee was already employed then and no imported opening balance falls in this year
	// (an opening is the full balance at its date; years before it are not computed).
	carry := 0.0
	if op == nil && c.CF > 0 && dj.Before(ys) && (open == nil || open.At < ys.Format("2006-01-02")) {
		prev := ComputeWith(name, c, lv, probMonths, doj, ys.AddDate(0, 0, -1).Format("2006-01-02"), open, approved, nil, wk)
		carry = r2(math.Min(math.Max(prev.Bal, 0), c.CF))
		credit += carry
	}
	in := func(t Taken) bool {
		f := policy.D(t.From)
		return !f.Before(ys) && !f.After(ye) && (op == nil || t.From >= op.At)
	}
	b := Balance{On: true, Credit: r2(credit), Carry: carry, Neg: lv.Neg, InProbBlock: !c.InProb && at.Before(pe), Worked: earned}
	for _, t := range approved {
		if in(t) {
			b.Used += t.Days
		}
	}
	for _, t := range pending {
		if in(t) {
			b.Pending += t.Days
		}
	}
	b.Bal = r2(b.Credit - b.Used - b.Pending)
	return b
}

// ---------- Which dates count ----------

// Cover keeps the dates that count as leave once weekly offs and holidays are applied (ltCover).
// exclude: offs are not counted; sandwich: offs between two leave days are counted; include: all.
func Cover(dates []string, off []bool, mode string) []string {
	keep := make([]bool, len(dates))
	switch mode {
	case "exclude":
		for i := range dates {
			keep[i] = !off[i]
		}
	case "sandwich":
		first, last := -1, -1
		for i := range dates {
			if !off[i] {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		for i := range dates {
			keep[i] = first >= 0 && i >= first && i <= last
		}
	default:
		for i := range dates {
			keep[i] = true
		}
	}
	out := []string{}
	for i, d := range dates {
		if keep[i] {
			out = append(out, d)
		}
	}
	return out
}

// Dates lists every date from..to (inclusive), at most 400.
func Dates(from, to string) []string {
	out := []string{}
	for d := from; d <= to && len(out) < 400; d = policy.AddDays(d, 1) {
		out = append(out, d)
	}
	return out
}

// WindowErrors are the notice, back-dating and size rules of a type (winErrorsBase).
func WindowErrors(c TypeCfg, name, today, from string, days float64) []string {
	er := []string{}
	back := c.Back
	if c.AdvOnly {
		back = 0
	}
	lead := policy.DayDiff(today, from)
	if lead < 0 {
		if -lead > back {
			if back > 0 {
				er = append(er, fmt.Sprintf("%s can be applied up to %d day%s after the leave date. Ask Payroll for an exception", name, back, plural(back)))
			} else {
				er = append(er, name+" cannot be back-dated. Ask Payroll for an exception")
			}
		}
	} else if lead < c.Adv {
		er = append(er, fmt.Sprintf("%s needs %d days of advance notice. The earliest start date is %s", name, c.Adv, policy.D(policy.AddDays(today, c.Adv)).Format("02 Jan 2006")))
	}
	if c.MaxReq > 0 && days > c.MaxReq {
		er = append(er, fmt.Sprintf("%s is limited to %v day%s in one request. Split it or use another leave type", name, c.MaxReq, plural(int(c.MaxReq))))
	}
	return er
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
