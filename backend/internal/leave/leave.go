// Package leave holds leave plans (spec 10.2) and the pure leave rules: leave year, balance,
// which dates count, and the notice and back-dating windows. Ported from the prototype
// (leaveYear, leaveBalance, ltCover, winErrorsBase).
package leave

import (
	"fmt"
	"math"
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
	Credit   string   `json:"credit"` // monthly | upfront
	From     string   `json:"from"`   // doj | prob: when accrual starts
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
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

// Compute is leaveBalance for one balance type on a date.
func Compute(c TypeCfg, lv LV, probMonths int, doj, on string, open *Opening, approved, pending []Taken) Balance {
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
	monthly := c.Credit != "upfront"
	var op *Opening
	if open != nil {
		if oa := policy.D(open.At); !oa.Before(ys) && !oa.After(ye) {
			op = open
		}
	}
	credit := 0.0
	switch {
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
		prev := Compute(c, lv, probMonths, doj, ys.AddDate(0, 0, -1).Format("2006-01-02"), open, approved, nil)
		carry = r2(math.Min(math.Max(prev.Bal, 0), c.CF))
		credit += carry
	}
	in := func(t Taken) bool {
		f := policy.D(t.From)
		return !f.Before(ys) && !f.After(ye) && (op == nil || t.From >= op.At)
	}
	b := Balance{On: true, Credit: r2(credit), Carry: carry, Neg: lv.Neg, InProbBlock: !c.InProb && at.Before(pe)}
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
