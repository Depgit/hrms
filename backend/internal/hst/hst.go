// Package hst works out what staff accommodation costs and who carries it (workforce plan, module 4; decided
// 16–17 Nov 2026). It is pure: the api package loads units, leases, month costs and stays and calls it.
//
// A unit's cost for a day is its month cost ÷ the days in the month, where the month cost is the rent and
// maintenance of the lease in force that day (with its yearly increases), the unit's other fixed items while it is
// leased, and the variable items HR entered for the month. The unit's month cost is then shared (decided 17 Nov):
//
//   - Equal (the default): equally among everyone who stayed in the unit on any day of the month (₹12,000 with one
//     person = ₹12,000; two = ₹6,000 each; four = ₹3,000 each). Only a month with nobody in the unit at all is
//     "empty unit cost".
//   - ByDays (a setting, off by default): each day's cost equally among the people staying that day (check-in ≤ day <
//     check-out); a day with nobody in the unit is empty-unit cost.
//
// Empty-unit cost is carried by the unit's home property (the first of the properties it serves). This is the
// company's own cost calculation only: it is never part of payroll.
package hst

import (
	"math"
	"sort"
	"time"
)

// Lease is a unit's lease. End "" is ongoing. IncOn is the first yearly increase (repeats every year on that date).
type Lease struct {
	ID          string
	Start, End  string
	Rent        float64
	Maintenance float64
	IncPct      float64
	IncOn       string
}

// Unit is one flat, house or PG with what its month cost needs.
type Unit struct {
	ID       string
	Home     string  // the property (location id) that carries its empty-unit cost; "" when none is set
	Fixed    float64 // other fixed monthly items, while a lease is in force
	Variable float64 // the month's variable items (electricity, water, Wi-Fi, housekeeping, other)
	Leases   []Lease
}

// Stay is a person in a unit from In (inclusive) to Out (exclusive; "" = staying).
type Stay struct {
	ID, UnitID, EmployeeID string
	In, Out                string
}

// Split methods.
const (
	Equal  = "equal" // the month cost equally among everyone who stayed in the month (default)
	ByDays = "days"  // each day's cost among the people staying that day
)

// Round2 rounds to the paisa.
func Round2(v float64) float64 { return math.Round(v*100) / 100 }

const day = "2006-01-02"

func parse(s string) time.Time { t, _ := time.Parse(day, s); return t }

// AddDays adds n days to a YYYY-MM-DD date.
func AddDays(s string, n int) string { return parse(s).AddDate(0, 0, n).Format(day) }

// DaysBetween is the number of days from a to b (b − a).
func DaysBetween(a, b string) int { return int(math.Round(parse(b).Sub(parse(a)).Hours() / 24)) }

// MonthDays lists the dates of a YYYY-MM month.
func MonthDays(ym string) []string {
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return nil
	}
	out := []string{}
	for d := t; d.Month() == t.Month(); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(day))
	}
	return out
}

// InForce reports whether the lease covers date d.
func (l Lease) InForce(d string) bool { return l.Start <= d && (l.End == "" || d <= l.End) }

// Increases is how many yearly increases have applied by date d (0 without an increase).
func (l Lease) Increases(d string) int {
	if l.IncPct == 0 || l.IncOn == "" || d < l.IncOn {
		return 0
	}
	on, at := parse(l.IncOn), parse(d)
	n := at.Year() - on.Year()
	if on.AddDate(n, 0, 0).After(at) {
		n--
	}
	return n + 1
}

// RentOn is the monthly rent in force on date d (0 outside the lease): the rent raised by IncPct at each yearly
// increase, to the paisa.
func (l Lease) RentOn(d string) float64 {
	if !l.InForce(d) {
		return 0
	}
	r := l.Rent
	for i := 0; i < l.Increases(d); i++ {
		r = Round2(r * (1 + l.IncPct/100))
	}
	return r
}

// NextIncrease is the first increase date after date d ("" when there is none or the lease has ended by then).
func (l Lease) NextIncrease(d string) string {
	if l.IncPct == 0 || l.IncOn == "" {
		return ""
	}
	// After k increases the next one is k years after the first (before the first, k is 0: the first itself).
	next := parse(l.IncOn).AddDate(l.Increases(d), 0, 0).Format(day)
	if l.End != "" && next > l.End {
		return ""
	}
	return next
}

// LeaseOn is the lease in force on date d, or nil.
func (u Unit) LeaseOn(d string) *Lease {
	for i := range u.Leases {
		if u.Leases[i].InForce(d) {
			return &u.Leases[i]
		}
	}
	return nil
}

// MonthlyOn is the unit's monthly running cost as it stands on date d (rent + maintenance + fixed items of the
// lease in force; no variable items).
func (u Unit) MonthlyOn(d string) float64 {
	l := u.LeaseOn(d)
	if l == nil {
		return 0
	}
	return l.RentOn(d) + l.Maintenance + u.Fixed
}

// DayCost is the unit's cost on date d of a month with dim days.
func (u Unit) DayCost(d string, dim int) float64 {
	return (u.MonthlyOn(d) + u.Variable) / float64(dim)
}

// UnitMonth is one unit's month: its cost, each occupant's share and days, and the empty-unit cost.
type UnitMonth struct {
	UnitID    string             `json:"unitId"`
	Home      string             `json:"home"`
	Cost      float64            `json:"cost"`
	Allocated float64            `json:"allocated"`
	Empty     float64            `json:"empty"`
	EmptyDays int                `json:"emptyDays"`
	Shares    map[string]float64 `json:"shares"`
	Days      map[string]int     `json:"days"`
}

// Month is the split of a month across every unit: per unit, per person (all units together) and per property.
type Month struct {
	YM     string               `json:"ym"`
	Method string               `json:"method"` // Equal or ByDays
	Days   int                  `json:"days"`
	Units  map[string]UnitMonth `json:"units"`
	Shares map[string]float64   `json:"shares"` // employee → share of the month
	Empty  map[string]float64   `json:"empty"`  // property (location id) → empty-unit cost; "" = a unit with no property
}

// Split works out month ym for the units and the stays (stays of other units are ignored) by method (Equal unless
// ByDays). Amounts are to the paisa and cost = allocated + empty exactly: by days, each person's share is rounded and
// the unit's empty cost is what is left; in equal mode the paisa left by rounding goes to the first person, and the
// unit's whole cost is empty only when nobody stayed in the month. Days counts each person's days in the unit and
// EmptyDays the days with nobody, in both methods.
func Split(ym string, units []Unit, stays []Stay, method string) Month {
	if method != ByDays {
		method = Equal
	}
	days := MonthDays(ym)
	out := Month{YM: ym, Method: method, Days: len(days), Units: map[string]UnitMonth{}, Shares: map[string]float64{}, Empty: map[string]float64{}}
	if len(days) == 0 {
		return out
	}
	byUnit := map[string][]Stay{}
	for _, s := range stays {
		byUnit[s.UnitID] = append(byUnit[s.UnitID], s)
	}
	for _, u := range units {
		um := UnitMonth{UnitID: u.ID, Home: u.Home, Shares: map[string]float64{}, Days: map[string]int{}}
		raw := map[string]float64{}
		cost := 0.0
		for _, d := range days {
			c := u.DayCost(d, len(days))
			cost += c
			var here []string
			seen := map[string]bool{}
			for _, s := range byUnit[u.ID] {
				if s.In <= d && (s.Out == "" || d < s.Out) && !seen[s.EmployeeID] {
					seen[s.EmployeeID] = true
					here = append(here, s.EmployeeID)
				}
			}
			if len(here) == 0 {
				um.EmptyDays++
				continue
			}
			for _, e := range here {
				raw[e] += c / float64(len(here))
				um.Days[e]++
			}
		}
		um.Cost = Round2(cost)
		ids := make([]string, 0, len(raw))
		for e := range raw {
			ids = append(ids, e)
		}
		sort.Strings(ids)
		shares := map[string]float64{}
		if method == Equal && len(ids) > 0 {
			each := Round2(um.Cost / float64(len(ids)))
			for _, e := range ids {
				shares[e] = each
			}
			shares[ids[0]] = Round2(um.Cost - each*float64(len(ids)-1))
		} else {
			for _, e := range ids {
				shares[e] = Round2(raw[e])
			}
		}
		for _, e := range ids {
			v := shares[e]
			um.Shares[e] = v
			um.Allocated += v
			out.Shares[e] = Round2(out.Shares[e] + v)
		}
		um.Allocated = Round2(um.Allocated)
		um.Empty = Round2(um.Cost - um.Allocated)
		if um.Empty != 0 {
			out.Empty[u.Home] = Round2(out.Empty[u.Home] + um.Empty)
		}
		out.Units[u.ID] = um
	}
	return out
}

// Overlaps reports whether two stays' date ranges [in, out) share a day.
func Overlaps(in1, out1, in2, out2 string) bool {
	end1, end2 := out1, out2
	if end1 == "" {
		end1 = "9999-12-31"
	}
	if end2 == "" {
		end2 = "9999-12-31"
	}
	return in1 < end2 && in2 < end1
}

// LeasesOverlap reports whether two leases [start, end] (end inclusive, "" ongoing) share a day.
func LeasesOverlap(a, b Lease) bool {
	ea, eb := a.End, b.End
	if ea == "" {
		ea = "9999-12-31"
	}
	if eb == "" {
		eb = "9999-12-31"
	}
	return a.Start <= eb && b.Start <= ea
}
