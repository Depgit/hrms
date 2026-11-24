// Package policy holds the work-policy settings (spec 10.1) with their defaults, and the pure rules
// that follow from them: weekly offs, holidays, the default roster code, notice and windows.
// The JSON names follow the prototype (polX / POLD) so stored settings stay readable.
package policy

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type Notice struct {
	Prob int `json:"prob"`
	Conf int `json:"conf"`
	// Grade holds the per-grade overrides: {"prob": notice days in probation, "conf": notice days after
	// confirmation, "probM": probation months}. A key that is missing uses the policy default.
	Grade map[string]map[string]int `json:"grade"`
}

type Blackout struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
	On   bool   `json:"on"`
}

type Settings struct {
	Desc     string  `json:"desc"`
	Sun      string  `json:"sun"`     // all | none
	Sat      string  `json:"sat"`     // none | 2,4 | 1,3 | all
	OffMode  string  `json:"offMode"` // fixed | rotational (the roster decides)
	Grace    float64 `json:"grace"`
	FullH    float64 `json:"fullH"`
	HalfH    float64 `json:"halfH"`
	OptHol   int     `json:"optHol"`
	Prob     int     `json:"prob"` // months
	Notice   Notice  `json:"notice"`
	RegCount string  `json:"regCount"` // separate | combined
	WO       struct {
		Min    int `json:"min"`
		Consec int `json:"consec"`
	} `json:"wo"`
	Hrs struct {
		Week   float64 `json:"week"`
		Spread float64 `json:"spread"`
		Brk    float64 `json:"brk"`
	} `json:"hrs"`
	Shift struct {
		// Rest and Change are no longer used: rest is att.minRest and the shift-change notice is rs.lead
		// (Shifts and rest card). They stay readable in stored settings.
		// The Night shift card (nightFrom, nightTo, nightAllow, women) was removed with the night shift allowance (owner,
		// 24 Nov 2026): stored policies that still carry the keys read without them.
		Rest   float64 `json:"rest"`
		Change int     `json:"change"`
	} `json:"shift"`
	Hol struct {
		Prob  bool   `json:"prob"`
		OnOff string `json:"onOff"`
	} `json:"hol"`
	Att struct {
		Mode    string  `json:"mode"`
		Geo     int     `json:"geo"`
		Selfie  bool    `json:"selfie"`
		LateN   int     `json:"lateN"`
		LateAct string  `json:"lateAct"` // half | warn | none
		Early   int     `json:"early"`
		Absent  string  `json:"absent"`  // lop | lopnow | present
		Missing string  `json:"missing"` // half | absent | present
		Night   string  `json:"night"`
		AutoOut string  `json:"autoOut"` // none | end
		MinRest float64 `json:"minRest"` // hours
		RestAct string  `json:"restAct"` // block | warn
		MaxSpan float64 `json:"maxSpan"` // hours
		OD      bool    `json:"od"`
		WFH     bool    `json:"wfh"`
		WFHDays int     `json:"wfhDays"`
		// The punch window (owner, 24 Nov 2026; was on each shift): a punch earlier than the shift start minus EarlyInMax,
		// or later than its end plus LateOutMax, is not this day's. Decimal hours, 0 to 12; 0 = no limit.
		EarlyInMax float64 `json:"earlyInMax"`
		LateOutMax float64 `json:"lateOutMax"`
	} `json:"att"`
	RG struct {
		Reason bool `json:"reason"`
		Mgr    bool `json:"mgr"`
	} `json:"rg"`
	RR struct {
		Back   int  `json:"back"`
		Cutoff bool `json:"cutoff"`
		Extra  int  `json:"extra"`
		Max    int  `json:"max"`
	} `json:"rr"`
	OffCap struct {
		On  bool `json:"on"`
		Max int  `json:"max"`
		Sun bool `json:"sun"`
		Sat bool `json:"sat"`
		Hol bool `json:"hol"`
	} `json:"offCap"`
	Ex struct {
		Ext      bool   `json:"ext"`
		ExtMax   int    `json:"extMax"`
		Review   bool   `json:"review"`
		Auto     bool   `json:"auto"`
		Buyout   string `json:"buyout"`
		Recover  bool   `json:"recover"`
		Leave    string `json:"leave"`
		Withdraw int    `json:"withdraw"`
		FnF      int    `json:"fnf"`
		ExitInt  bool   `json:"exitInt"`
		Waive    string `json:"waive"`
	} `json:"ex"`
	RS struct {
		Lead       int    `json:"lead"`
		Reason     bool   `json:"reason"`
		Past       string `json:"past"` // lock | free
		Alert      int    `json:"alert"`
		DevTol     int    `json:"devTol"`
		Pts        int    `json:"pts"`
		LeaveBelow int    `json:"leaveBelow"` // leave is refused while the month's discipline score is below this (0 = off)
		DevWin     int    `json:"devWin"`
		MgrAlert   int    `json:"mgrAlert"`
		EarlyIn    int    `json:"earlyIn"`
		EarlyCount string `json:"earlyCount"` // no | yes
		OutTol     int    `json:"outTol"`
		DevStart   bool   `json:"devStart"`
	} `json:"rs"`
	// Claims: whether employees on the policy raise their own reimbursement claims. A policy stored before
	// this setting has no "claims" key and keeps the default, Yes.
	Claims struct {
		On bool `json:"on"`
	} `json:"claims"`
	Blackouts []Blackout `json:"blackouts"`
	Dev       DevRules   `json:"dev"` // deviation allowances (dev.go); every rule off until the admin sets it
}

// Defaults are the prototype's mkPol and POLD values.
func Defaults() Settings {
	var s Settings
	s.Sun, s.Sat, s.OffMode = "all", "none", "fixed"
	s.Grace, s.FullH, s.HalfH, s.OptHol, s.Prob = 15, 8, 4, 2, 6
	s.Notice = Notice{Prob: 15, Conf: 60, Grade: map[string]map[string]int{}}
	s.RegCount = "separate"
	s.WO.Min, s.WO.Consec = 1, 6
	s.Hrs.Week, s.Hrs.Spread, s.Hrs.Brk = 48, 10.5, 30
	s.Shift.Rest, s.Shift.Change = 11, 2
	s.Hol.Prob, s.Hol.OnOff = true, "once"
	a := &s.Att
	a.Mode, a.Geo, a.LateN, a.LateAct, a.Early, a.Absent, a.Missing, a.Night, a.AutoOut = "any", 100, 3, "half", 10, "lop", "half", "start", "none"
	a.MinRest, a.RestAct, a.MaxSpan, a.OD = 6, "block", 16, true
	s.RG.Reason, s.RG.Mgr = true, true
	s.RR.Back, s.RR.Cutoff, s.RR.Extra = 7, true, 3
	s.OffCap.On, s.OffCap.Max, s.OffCap.Sun, s.OffCap.Sat, s.OffCap.Hol = true, 7, true, true, true
	x := &s.Ex
	x.Ext, x.ExtMax, x.Review, x.Buyout, x.Recover, x.Leave, x.Withdraw, x.FnF, x.ExitInt, x.Waive = true, 3, true, "approve", true, "no", 7, 45, true, "HR Admin"
	r := &s.RS
	r.Lead, r.Reason, r.Past, r.Alert, r.DevTol, r.Pts, r.DevWin, r.MgrAlert = 2, true, "lock", 30, 60, 5, 3, 15
	r.EarlyIn, r.EarlyCount, r.DevStart = 30, "no", true
	s.Claims.On = true
	s.Blackouts = []Blackout{}
	return s
}

// Parse merges stored settings over the defaults; keys that are not stored keep their default, and keys
// of settings that were removed (overtime, comp-off, holiday work, break, late half day, extra-hours, rest and
// off-day deviations, split shifts) are ignored.
func Parse(raw []byte) (Settings, error) {
	s := Defaults()
	if len(raw) == 0 {
		return s, nil
	}
	err := json.Unmarshal(raw, &s)
	if s.Notice.Grade == nil {
		s.Notice.Grade = map[string]map[string]int{}
	}
	if s.Blackouts == nil {
		s.Blackouts = []Blackout{}
	}
	return s, err
}

// ---------- Dates ----------

func D(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func ISO(t time.Time) string { return t.Format("2006-01-02") }

func AddDays(s string, n int) string { return ISO(D(s).AddDate(0, 0, n)) }

// DayDiff is b - a in days.
func DayDiff(a, b string) int { return int(D(b).Sub(D(a)).Hours() / 24) }

func LastDayOf(ym string) string {
	t, _ := time.Parse("2006-01", ym)
	return ISO(t.AddDate(0, 1, -1))
}

// IDNum is the trailing number of an employee code; it spreads rotational offs across the team.
func IDNum(id string) int {
	i := len(id)
	for i > 0 && id[i-1] >= '0' && id[i-1] <= '9' {
		i--
	}
	if n, err := strconv.Atoi(id[i:]); err == nil {
		return n
	}
	h := 0
	for _, c := range id {
		h = (h*31 + int(c)) % 100003
	}
	return h
}

// ---------- Weekly offs, holidays, default roster ----------

// IsWO: a fixed weekly off from the policy (rotational policies leave it to the roster).
func (s Settings) IsWO(date string) bool {
	if s.OffMode == "rotational" {
		return false
	}
	t := D(date)
	switch t.Weekday() {
	case time.Sunday:
		return s.Sun == "all"
	case time.Saturday:
		n := (t.Day() + 6) / 7
		return s.Sat == "all" || (s.Sat == "2,4" && (n == 2 || n == 4)) || (s.Sat == "1,3" && (n == 1 || n == 3))
	}
	return false
}

// Holidays: date -> name, public holidays only (optional holidays are taken as leave).
type Holidays map[string]string

// DefCode is the roster cell when nobody has changed it (prototype defCode).
func (s Settings) DefCode(empID, base, date string, hol Holidays) string {
	if s.OffMode == "rotational" && (int(D(date).Weekday())+6)%7 == IDNum(empID)%6 {
		return "WO"
	}
	if s.IsWO(date) {
		return "WO"
	}
	if _, ok := hol[date]; ok {
		return "PH"
	}
	return base
}

// NoticeDays: the grade override wins over the policy default.
func (s Settings) NoticeDays(grade string, inProbation bool) int {
	k := "conf"
	if inProbation {
		k = "prob"
	}
	if g, ok := s.Notice.Grade[grade]; ok {
		if v, ok := g[k]; ok && v > 0 {
			return v
		}
	}
	if inProbation {
		return s.Notice.Prob
	}
	return s.Notice.Conf
}

// ProbMonths is the probation for a grade: the grade's months when set (0 = no probation), else the policy's.
func (s Settings) ProbMonths(grade string) int {
	if g, ok := s.Notice.Grade[grade]; ok {
		if v, ok := g["probM"]; ok && v >= 0 {
			return v
		}
	}
	return s.Prob
}

// ProbEnd is the day probation ends for someone of the grade who joined on doj ("" when there is none).
func (s Settings) ProbEnd(grade, doj string) string {
	m := s.ProbMonths(grade)
	if m <= 0 || doj == "" {
		return ""
	}
	return ISO(D(doj).AddDate(0, m, 0))
}

// InProbation on a date, for someone of the grade who joined on doj.
func (s Settings) InProbation(grade, doj, on string) bool {
	end := s.ProbEnd(grade, doj)
	return end != "" && on < end
}

// RegDeadline is the last day a regularization for date can be raised (spec 11.7).
func (s Settings) RegDeadline(date string) (byAge, byCycle, last string) {
	byAge = AddDays(date, s.RR.Back)
	last = byAge
	if s.RR.Cutoff {
		byCycle = AddDays(LastDayOf(date[:7]), s.RR.Extra)
		if byCycle < last {
			last = byCycle
		}
	}
	return
}

// MonthOffs counts the offs in a month for the off cap (holiday on an off counts once).
func (s Settings) MonthOffs(ym string, code func(date string) string) (total int, over bool) {
	sun, sat, hol := 0, 0, 0
	for d := ym + "-01"; d[:7] == ym; d = AddDays(d, 1) {
		c := code(d)
		wd := D(d).Weekday()
		switch c {
		case "WO":
			switch wd {
			case time.Sunday:
				sun++
			case time.Saturday:
				sat++
			default:
				hol++
			}
		case "PH":
			hol++
		}
	}
	c := s.OffCap
	if c.Sun {
		total += sun
	}
	if c.Sat {
		total += sat
	}
	if c.Hol {
		total += hol
	}
	return total, c.On && total > c.Max
}

// WeekOffText describes the weekly off for summaries.
func (s Settings) WeekOffText() string {
	if s.OffMode == "rotational" {
		return "Weekly off set on the roster"
	}
	parts := []string{}
	if s.Sun == "all" {
		parts = append(parts, "Sundays")
	}
	switch s.Sat {
	case "all":
		parts = append(parts, "all Saturdays")
	case "2,4":
		parts = append(parts, "2nd and 4th Saturdays")
	case "1,3":
		parts = append(parts, "1st and 3rd Saturdays")
	}
	if len(parts) == 0 {
		return "No fixed weekly off"
	}
	return strings.Join(parts, " and ") + " off"
}
