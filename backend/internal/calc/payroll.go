package calc

import (
	"math"
	"reflect"
	"strconv"
	"strings"
)

// The month's pay row for one person (spec 14 "Per month", prototype payRow), plus the
// helpers the payroll cycle needs: instalment plans, version blending, run checks, amount in words.

// Line is one pay ledger line (spec 15.2).
type Line struct {
	ID     int64   `json:"id,omitempty"`
	Kind   string  `json:"kind"` // earn | ded
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
	Reason string  `json:"reason,omitempty"`
	Source string  `json:"source,omitempty"`
}

// RowIn is everything a pay row needs; Calc runs the salary engine for an earned gross,
// with the variable payout total when one is due.
type RowIn struct {
	Days         int      // pay-days basis for the month
	LOP          float64  // loss of pay days from final attendance
	Outside      float64  // days before joining or after the last working day
	DaysN        int      // days in the attendance window
	PaidOverride *float64 // paid days entered by Payroll
	Gross        float64  // monthly gross for the month
	Calc         func(earned float64, varTotal *float64) Result
	PrevVariable []float64 // accruals of the earlier months of the variable cycle (zero before joining)
	Lines        []Line
	OtherEarn    float64
	OtherDed     float64
	SalaryTds    float64 // Section 192, from the tax module
}

type Row struct {
	Days      int     `json:"days"`
	Paid      float64 `json:"paid"`
	LOP       float64 `json:"lop"`
	Outside   float64 `json:"outside"`
	OutsideP  float64 `json:"outsideP,omitempty"` // the days outside employment on the pay-days basis
	Gross     float64 `json:"gross"`
	Earned    float64 `json:"earned"`
	R         Result  `json:"r"`
	Add       []Line  `json:"add"`
	Ded       []Line  `json:"ded"`
	OE        float64 `json:"oe"`
	OD        float64 `json:"od"`
	STds      float64 `json:"stds"`
	TotalEarn float64 `json:"totalEarn"`
	TotalDed  float64 `json:"totalDed"`
	Net       float64 `json:"net"`
	RoundOff  float64 `json:"roundOff,omitempty"` // net − (total earnings − total deductions): net is paid in whole rupees
	Carry     float64 `json:"carry"`              // a negative remainder, recovered next month
}

func sumLines(l []Line) float64 {
	t := 0.0
	for _, x := range l {
		t += x.Amount
	}
	return t
}

// PaidDays: Payroll's figure when entered, else the basis less loss of pay and the days
// outside employment (scaled to the basis).
func PaidDays(days int, lop, outside float64, daysN int, override *float64) float64 {
	if override != nil {
		return math.Max(0, math.Min(float64(days), *override))
	}
	outp := OutsideOnBasis(days, outside, daysN)
	return math.Max(0, float64(days)-lop-outp)
}

// OutsideOnBasis is the days outside employment scaled to the pay-days basis, as PaidDays counts them.
func OutsideOnBasis(days int, outside float64, daysN int) float64 {
	if daysN <= 0 {
		daysN = days
	}
	if daysN <= 0 {
		return 0
	}
	return Round2(outside * float64(days) / float64(daysN))
}

// OutsideDays is the row's days before joining or after the last working day on the pay-days basis (rows saved
// before this was kept: the window's count, at most the basis).
func (r Row) OutsideDays() float64 {
	if r.OutsideP > 0 || r.Outside == 0 {
		return r.OutsideP
	}
	return math.Min(r.Outside, float64(r.Days))
}

func PayRow(in RowIn) Row {
	paid := PaidDays(in.Days, in.LOP, in.Outside, in.DaysN, in.PaidOverride)
	earned := 0.0
	if in.Days > 0 {
		earned = in.Gross * paid / float64(in.Days)
	}
	r := in.Calc(earned, nil)
	// The variable payout is pro rata: each month of the cycle counts by the days actually paid.
	if r.VarPay > 0 && r.VarCycle > 1 {
		tot := r.Variable
		for _, v := range in.PrevVariable {
			tot += v
		}
		r = in.Calc(earned, &tot)
	}
	row := Row{Days: in.Days, Paid: paid, LOP: in.LOP, Outside: in.Outside, OutsideP: OutsideOnBasis(in.Days, in.Outside, in.DaysN), Gross: in.Gross, Earned: Round2(earned), R: r,
		Add: []Line{}, Ded: []Line{}, OE: in.OtherEarn, OD: in.OtherDed, STds: in.SalaryTds}
	for _, l := range in.Lines {
		if l.Kind == "earn" {
			row.Add = append(row.Add, l)
		} else {
			row.Ded = append(row.Ded, l)
		}
	}
	add, ded := sumLines(row.Add), sumLines(row.Ded)
	row.TotalEarn = Round2(r.TotalEarn + add + in.OtherEarn)
	row.TotalDed = Round2(r.Ded + ded + in.OtherDed + in.SalaryTds)
	// Net pay is the totals' difference rounded to the nearest rupee (user decision 13 Nov); the rounding is kept as
	// RoundOff so register, payslip, bank advice and words show one figure and foot (ITM-59). A negative remainder is
	// carried to next month as it is.
	raw := Round2(row.TotalEarn - row.TotalDed)
	if raw < 0 {
		row.Carry = -raw
	} else {
		row.Net = math.Round(raw)
		row.RoundOff = Round2(row.Net - raw)
	}
	return row
}

// Blend combines calculations for parts of a month (a structure version change mid-month),
// each weighted by its share of the days. Flags and the variable cycle come from the last part.
func Blend(parts []Result, weights []float64) Result {
	if len(parts) == 1 {
		return parts[0]
	}
	var out Result
	ov := reflect.ValueOf(&out).Elem()
	for i, p := range parts {
		pv := reflect.ValueOf(p)
		for f := 0; f < pv.NumField(); f++ {
			switch pv.Field(f).Kind() {
			case reflect.Float64:
				ov.Field(f).SetFloat(ov.Field(f).Float() + pv.Field(f).Float()*weights[i])
			default:
				ov.Field(f).Set(pv.Field(f))
			}
		}
	}
	out.Comps = blendComps(parts, weights)
	return out
}

// blendComps weights each component's amounts by the parts' shares of the month (a component missing from a part
// counts nothing there); the order is the parts' order of first appearance, the names and flags the last part's.
func blendComps(parts []Result, weights []float64) []CompLine {
	var out []CompLine
	at := map[string]int{}
	for i, p := range parts {
		for _, l := range p.Comps {
			k := l.Kind + "|" + l.Side + "|" + l.Code
			j, ok := at[k]
			if !ok {
				j = len(out)
				at[k] = j
				out = append(out, CompLine{})
			}
			a, t, acc := out[j].Amount, out[j].Target, out[j].Accrued
			out[j] = l
			out[j].Amount, out[j].Target, out[j].Accrued = a+l.Amount*weights[i], t+l.Target*weights[i], acc+l.Accrued*weights[i]
		}
	}
	return out
}

// Instalments splits an amount into n equal whole-rupee parts; the last takes the rounding.
func Instalments(amount float64, n int) []float64 {
	if n < 1 {
		n = 1
	}
	each := math.Floor(amount / float64(n))
	out := make([]float64, n)
	for i := range out {
		out[i] = each
	}
	out[n-1] = Round2(amount - each*float64(n-1))
	return out
}

// Check is one payroll run check (spec 15.1 #4).
type Check struct {
	Title  string `json:"title"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// CheckRow is what the run checks need about one row.
type CheckRow struct {
	Name     string
	Net      float64
	Prev     float64 // last month's net; 0 when not paid last month
	Hold     bool
	Edited   bool
	Adjusted bool
	Joined   bool
}

// MovedMore reports a change of more than 15% against last month.
func MovedMore(net, prev float64) bool { return prev > 0 && math.Abs(net-prev)/prev > 0.15 }

func names(l []string) string {
	if len(l) <= 6 {
		return strings.Join(l, "; ")
	}
	return strings.Join(l[:6], "; ") + " and " + itoa(len(l)-6) + " more"
}

func itoa(n int) string { return strconv.Itoa(n) }

func formatFloat(v float64, prec int) string { return strconv.FormatFloat(v, 'f', prec, 64) }

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

func pad2(n int64) string {
	if n < 10 {
		return "0" + formatInt(n)
	}
	return formatInt(n)
}

// RunChecks compares the run with last month: total net within 5%, people moving more than
// 15%, zero net, holds, ledger lines, manual changes and joiners.
// LineTotals are the month's pay lines (arrears, claims, recoveries): those of the people in
// the run, and those of people outside it while a pilot runs.
type LineTotals struct {
	N         int
	Earn, Ded float64
	OutN      int
	OutEarn   float64
	OutDed    float64
	OutWho    []string // the people with those lines (named in the check)
	Pilot     bool
}

func RunChecks(rows []CheckRow, lt LineTotals) []Check {
	lines, lineEarn, lineDed := lt.N, lt.Earn, lt.Ded
	var moved, zero, held, joined []string
	edits, adj := 0, 0
	cur, prev := 0.0, 0.0
	for _, r := range rows {
		cur += r.Net
		prev += r.Prev
		if MovedMore(r.Net, r.Prev) {
			moved = append(moved, r.Name+" "+Inr(r.Prev)+" → "+Inr(r.Net))
		}
		if r.Net <= 0 {
			zero = append(zero, r.Name)
		}
		if r.Hold {
			held = append(held, r.Name)
		}
		if r.Joined {
			joined = append(joined, r.Name)
		}
		if r.Edited {
			edits++
		}
		if r.Adjusted {
			adj++
		}
	}
	pc := 0.0
	if prev > 0 {
		pc = math.Round((cur-prev)/prev*1000) / 10
	}
	sign := ""
	if pc > 0 {
		sign = "+"
	}
	none := func(l []string) string {
		if len(l) == 0 {
			return "None"
		}
		return names(l)
	}
	first := Check{"Net pay compared with last month", math.Abs(pc) <= 5, Inr(math.Round(cur)) + " against " + Inr(math.Round(prev)) + " (" + sign + trimFloat(pc) + "%)"}
	if prev == 0 {
		first = Check{"Net pay compared with last month", true, Inr(math.Round(cur)) + ". No earlier run to compare with"}
	}
	out := []Check{
		first,
		{"Employees whose net pay moved more than 15%", len(moved) == 0, none(moved)},
		{"Zero or negative net pay", len(zero) == 0, none(zero)},
		{"Salary on hold", len(held) == 0, func() string {
			if len(held) == 0 {
				return "None"
			}
			return itoa(len(held)) + ": " + names(held)
		}()},
		{"Arrears, claims and recoveries in this month", lt.OutN == 0 || lt.Pilot, func() string {
			t := "None"
			if lines > 0 {
				t = nLines(lines) + " · additions " + Inr(lineEarn) + " · deductions " + Inr(lineDed)
			}
			out := nLines(lt.OutN) + " (additions " + Inr(lt.OutEarn) + " · deductions " + Inr(lt.OutDed) + ")"
			if lt.OutN > 0 && lt.Pilot {
				t += ". Not in this pilot: " + out + " for people outside it; the full dry run pays them"
			} else if lt.OutN > 0 {
				t += ". Not in the run: " + out + " for people without a row"
				if len(lt.OutWho) > 0 {
					t += " (" + strings.Join(lt.OutWho, ", ") + ")"
				}
				t += "; run the dry run again, or move or cancel those lines"
			}
			return t
		}()},
		{"Manual changes made by Payroll", edits == 0 && adj == 0, func() string {
			if edits == 0 && adj == 0 {
				return "None"
			}
			return itoa(edits) + " paid-days edits, " + itoa(adj) + " adjustments"
		}()},
		{"New joiners this month", true, none(joined)},
	}
	return out
}

func nLines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return itoa(n) + " lines"
}

func trimFloat(v float64) string {
	s := strings.TrimRight(strings.TrimRight(formatFloat(v, 1), "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// Inr formats a rupee amount the Indian way: ₹1,23,456 (paise shown only when present).
func Inr(v float64) string {
	neg := v < 0
	v = math.Abs(Round2(v))
	whole := int64(v)
	paise := int64(math.Round((v - float64(whole)) * 100))
	s := groupIndian(whole)
	if paise > 0 {
		s += "." + pad2(paise)
	}
	if neg {
		return "-₹" + s
	}
	return "₹" + s
}

func groupIndian(n int64) string {
	s := formatInt(n)
	if len(s) <= 3 {
		return s
	}
	head, tail := s[:len(s)-3], s[len(s)-3:]
	var parts []string
	for len(head) > 2 {
		parts = append([]string{head[len(head)-2:]}, parts...)
		head = head[:len(head)-2]
	}
	if head != "" {
		parts = append([]string{head}, parts...)
	}
	return strings.Join(parts, ",") + "," + tail
}

var ones = []string{"", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten", "Eleven", "Twelve", "Thirteen",
	"Fourteen", "Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen"}
var tens = []string{"", "", "Twenty", "Thirty", "Forty", "Fifty", "Sixty", "Seventy", "Eighty", "Ninety"}

func two(x int64) string {
	if x < 20 {
		return ones[x]
	}
	if x%10 == 0 {
		return tens[x/10]
	}
	return tens[x/10] + " " + ones[x%10]
}

func three(x int64) string {
	s := ""
	if x > 99 {
		s = ones[x/100] + " Hundred"
		if x%100 > 0 {
			s += " "
		}
	}
	if x%100 > 0 {
		s += two(x % 100)
	}
	return s
}

// RupeeWords writes an amount in Indian words: "Twenty Eight Thousand One Hundred Sixty Six", with the paise
// when there are any ("... Sixty Six and Sixty Eight Paise"), so the words match the figure paid (RUN-66).
func RupeeWords(v float64) string {
	v = Round2(math.Max(0, v))
	n := int64(v)
	paise := int64(math.Round((v - float64(n)) * 100))
	if paise == 100 {
		n, paise = n+1, 0
	}
	words := "Zero"
	if n > 0 {
		var out []string
		for _, p := range []struct {
			v int64
			l string
		}{{10000000, "Crore"}, {100000, "Lakh"}, {1000, "Thousand"}} {
			if n >= p.v {
				out = append(out, three(n/p.v)+" "+p.l)
				n %= p.v
			}
		}
		if n > 0 {
			out = append(out, three(n))
		}
		words = strings.Join(out, " ")
	}
	if paise > 0 {
		words += " and " + two(paise) + " Paise"
	}
	return words
}
