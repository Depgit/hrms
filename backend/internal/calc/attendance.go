package calc

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Daily attendance status and monthly loss of pay (spec 11.3 and 11.4).

var hhmmRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

// ToMin parses "HH:MM" into minutes from midnight; ok is false for bad input.
func ToMin(t string) (int, bool) {
	m := hhmmRe.FindStringSubmatch(strings.TrimSpace(t))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	if h > 23 || mi > 59 {
		return 0, false
	}
	return h*60 + mi, true
}

func HHMM(m int) string {
	x := ((m % 1440) + 1440) % 1440
	return fmt.Sprintf("%02d:%02d", x/60, x%60)
}

type AttPolicy struct {
	Grace       int     `json:"grace"`
	FullH       float64 `json:"fullH"`
	HalfH       float64 `json:"halfH"`
	AutoOut     string  `json:"autoOut"` // "none" | "end"
	EarlyIn     int     `json:"earlyIn"`
	EarlyCount  bool    `json:"earlyCount"`
	LateN       int     `json:"lateN"`
	LateAct     string  `json:"lateAct"` // "half" | "warn" | "none"
	Missing     string  `json:"missing"` // "half" | "absent" | "present"
	Absent      string  `json:"absent"`  // "lop" | "lopnow" | "present"
	RegBackDays int     `json:"regBackDays"`
}

var DefaultAttPolicy = AttPolicy{Grace: 15, FullH: 8, HalfH: 4, AutoOut: "none", EarlyIn: 30,
	LateN: 3, LateAct: "half", Missing: "half", Absent: "lop", RegBackDays: 7}

const (
	Present     = "Present"
	Late        = "Late"
	HalfDay     = "Half day"
	AbsentShort = "Absent (short hours)"
	NoPunch     = "No punch"
	MissingIn   = "Missing in punch"
	MissingOut  = "Missing out punch"
	WeekOff     = "Week off"
	Holiday     = "Holiday"
	OutsideDay  = "Outside"
	UpcomingDay = "Upcoming"
)

// NetHours: early time inside the allowance is left out (unless counted). No break is taken off.
func NetHours(in, out int, shiftStart *int, p AttPolicy) float64 {
	a, b := in, out
	if b <= a {
		b += 1440
	}
	if shiftStart != nil && !p.EarlyCount {
		sg := ((a - *shiftStart + 2160) % 1440) - 720
		if sg < 0 && -sg <= p.EarlyIn {
			a += -sg
		}
	}
	return math.Max(0, float64(b-a)/60)
}

type DayInput struct {
	In, Out    string // "" when missing
	ShiftStart *int
	ShiftEnd   *int
	Off        string // "", "WO" or "PH"
	// LateStart overrides the late check start (a Shift deviation uses the actual shift);
	// set LateStartSet with a nil LateStart to disable lateness.
	LateStart    *int
	LateStartSet bool
	Policy       *AttPolicy
}

func DayStatus(d DayInput) string {
	p := DefaultAttPolicy
	if d.Policy != nil {
		p = *d.Policy
	}
	if d.In == "" && d.Out == "" {
		switch d.Off {
		case "WO":
			return WeekOff
		case "PH":
			return Holiday
		}
		return NoPunch
	}
	if d.In == "" {
		return MissingIn
	}
	a, _ := ToMin(d.In)
	var b int
	if d.Out == "" {
		if p.AutoOut == "end" && d.ShiftEnd != nil {
			b = *d.ShiftEnd
		} else {
			return MissingOut
		}
	} else {
		b, _ = ToMin(d.Out)
	}
	h := NetHours(a, b, d.ShiftStart, p)
	if h < p.HalfH {
		return AbsentShort
	}
	if h < p.FullH {
		return HalfDay
	}
	ls := d.ShiftStart
	if d.LateStartSet {
		ls = d.LateStart
	}
	if ls != nil && a > *ls+p.Grace {
		return Late
	}
	return Present
}

type MonthDay struct {
	Status  string
	Leave   string // "", "paid" or "lop"
	AgeDays int    // days between this date and today
}

type MonthResult struct {
	LOP      float64        `json:"lop"`
	Outside  int            `json:"outside"`
	Counts   map[string]int `json:"counts"`
	LateOver int            `json:"lateOver"`
}

// MonthLOP works out loss of pay from per-day statuses (prototype `finalAtt`).
func MonthLOP(days []MonthDay, p AttPolicy, payDays int) MonthResult {
	c := map[string]int{}
	lop, outside := 0.0, 0
	for _, d := range days {
		if d.Status == OutsideDay {
			outside++
			continue
		}
		if d.Leave != "" {
			if d.Leave == "lop" {
				c["Loss of pay leave"]++
				lop++
			} else {
				c["Paid leave"]++
			}
			continue
		}
		c[d.Status]++
		switch d.Status {
		case HalfDay:
			lop += .5
		case AbsentShort:
			lop++
		case NoPunch:
			if p.Absent == "lopnow" || (p.Absent == "lop" && d.AgeDays > p.RegBackDays) {
				lop++
			}
		case MissingIn, MissingOut:
			switch p.Missing {
			case "half":
				lop += .5
			case "absent":
				lop++
			}
		}
	}
	over := c[Late] - p.LateN
	if over < 0 {
		over = 0
	}
	if p.LateAct == "half" {
		lop += float64(over) * .5
	}
	return MonthResult{math.Min(lop, float64(payDays)), outside, c, over}
}
