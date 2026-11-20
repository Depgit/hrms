// Package approval is the single approval engine for every workflow (spec section 7).
// Workflows differ only by the facts on a request and by a finalize handler.
package approval

import "math"

type Flow string

const (
	Operations Flow = "Operations"
	Corporate  Flow = "Corporate"
)

// Condition kinds: always, days, grades, cnt, budget, waive, tkind, amount, pct, bandx, ctype, ckind, skind.
type Condition struct {
	K    string   `json:"k"`
	Min  *float64 `json:"min,omitempty"` // nil = from 0
	Max  *float64 `json:"max,omitempty"` // nil = no limit
	List []string `json:"list,omitempty"`
	RT   string   `json:"rt,omitempty"` // cnt: "No Punch" | "All Other" | "Any"
	Val  string   `json:"val,omitempty"`
}

// Facts are what conditions are evaluated against. The server derives them; clients never supply grade or counts.
type Facts struct {
	WF            string  `json:"wf"`
	Grade         string  `json:"grade,omitempty"`
	Roll          float64 `json:"roll,omitempty"` // leave days in the last 30 days, including this request
	Days          float64 `json:"days,omitempty"`
	RType         string  `json:"rtype,omitempty"`
	Cnt           float64 `json:"cnt,omitempty"`
	AboveBudget   bool    `json:"aboveBudget,omitempty"`
	Waive         bool    `json:"waive,omitempty"`
	TKind         string  `json:"tkind,omitempty"`
	Amt           float64 `json:"amt,omitempty"`
	Pct           float64 `json:"pct,omitempty"`
	BandX         bool    `json:"bandx,omitempty"`
	CType         string  `json:"ctype,omitempty"`
	CKind         string  `json:"ckind,omitempty"`
	SKind         string  `json:"skind,omitempty"`
	LeaveGroup    string  `json:"leaveGroup,omitempty"` // "C" or "P"
	InitiatorRole string  `json:"initiatorRole,omitempty"`
	ToLocation    string  `json:"toLocation,omitempty"`
	ToDepartment  string  `json:"toDepartment,omitempty"`
	ToManager     string  `json:"toManager,omitempty"`
	RehireCHRO    bool    `json:"rehireChro,omitempty"`
}

func inRange(n float64, min, max *float64) bool {
	lo := 0.0
	if min != nil {
		lo = *min
	}
	return n >= lo && (max == nil || n <= *max)
}

// EvalCond says whether one condition holds for the facts.
func EvalCond(c Condition, f Facts) bool {
	switch c.K {
	case "always":
		return true
	case "days":
		n := f.Roll
		if n == 0 {
			n = f.Days
		}
		return inRange(math.Max(n, 0), c.Min, c.Max) // days are never below 0
	case "cnt":
		return (c.RT == "Any" || c.RT == f.RType) && inRange(math.Max(f.Cnt, 0), c.Min, c.Max)
	case "grades":
		for _, g := range c.List {
			if g == f.Grade {
				return true
			}
		}
		return false
	case "budget":
		return f.AboveBudget
	case "waive":
		return f.Waive
	case "tkind":
		return f.TKind == c.Val
	case "amount":
		return inRange(math.Abs(f.Amt), c.Min, c.Max)
	case "pct":
		return inRange(math.Abs(f.Pct), c.Min, c.Max)
	case "bandx":
		return f.BandX
	case "ctype":
		return f.CType == c.Val
	case "ckind":
		return f.CKind == c.Val
	case "skind":
		return orDefault(f.SKind, "Resignation") == c.Val
	}
	return false
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type Notify struct {
	Role   string `json:"role"`
	Person string `json:"person"`
}

type Chain struct {
	Chain  []string `json:"chain"`
	Notify []Notify `json:"notify"`
	Why    []string `json:"why"`
}

const Unassigned = "Unassigned"

// Rule is one line of a workflow's route: when every condition in When holds (AND), the request goes to Approvers,
// one after the other, and Notify are told. A rule with no conditions always fits.
type Rule struct {
	ID        string      `json:"id"`
	Label     string      `json:"label"`
	On        bool        `json:"on"` // a rule switched off is skipped (the last rule, Otherwise, is always used)
	When      []Condition `json:"when"`
	Approvers []string    `json:"approvers"`
	Notify    []string    `json:"notify"`
}

// RuleSet is the route of one workflow and flow: rules checked from the top, the first that fits decides. The
// last rule is "Otherwise" (no conditions), so every request has a route.
type RuleSet struct {
	Rules []Rule `json:"rules"`
}

// OtherwiseLabel names the last rule.
const OtherwiseLabel = "Otherwise"

// Fits says whether every condition of the rule holds for the facts.
func (r Rule) Fits(f Facts) bool {
	for _, c := range r.When {
		if !EvalCond(c, f) {
			return false
		}
	}
	return true
}

// Pick returns the index of the rule that decides: the first switched-on rule whose conditions all hold, and the
// last rule (Otherwise) when none does. -1 when there are no rules.
func Pick(s RuleSet, f Facts) int {
	for i, r := range s.Rules {
		if i == len(s.Rules)-1 || (r.On && r.Fits(f)) {
			return i
		}
	}
	return -1
}

// Build routes a request: the rule that decides (Pick) gives the approvers, in its order, and the people told.
// Why names that rule (nothing for Otherwise, like the old "every request" rows). A person on two stages is asked
// once; a role that approves is not also told (Approve beats Notify); the person who raised an offer does not
// approve it again (joining). An empty chain means nobody needs to approve.
func Build(s RuleSet, f Facts, resolve func(role string) string) Chain {
	return BuildKeyed(s, f, resolve, resolve)
}

// BuildKeyed is Build with a separate key telling who a stage's person is (the employee id): two different people
// with the same name are both asked (ESS-230). resolve still gives the names of the people told.
func BuildKeyed(s RuleSet, f Facts, resolve, key func(role string) string) Chain {
	out := Chain{Chain: []string{}, Notify: []Notify{}, Why: []string{}}
	i := Pick(s, f)
	if i < 0 {
		return out
	}
	r := s.Rules[i]
	if len(r.When) > 0 {
		out.Why = append(out.Why, r.Label)
	}
	out.Chain, out.Notify = routeKeyed(r.Approvers, r.Notify, f, resolve, key)
	return out
}

// route turns a rule's approvers and notify list into the chain and the people told.
func route(approvers, notify []string, f Facts, resolve func(role string) string) ([]string, []Notify) {
	return routeKeyed(approvers, notify, f, resolve, resolve)
}

func routeKeyed(approvers, notify []string, f Facts, resolve, key func(role string) string) ([]string, []Notify) {
	chain, told := []string{}, []Notify{}
	A, seen := map[string]bool{}, map[string]bool{}
	for _, role := range approvers {
		if A[role] {
			continue
		}
		A[role] = true
		if f.WF == "joining" && f.InitiatorRole == role {
			continue
		}
		p := key(role)
		if p != Unassigned && p != "" && seen[p] {
			continue
		}
		seen[p] = true
		chain = append(chain, role)
	}
	N := map[string]bool{}
	for _, role := range notify {
		if A[role] || N[role] {
			continue
		}
		N[role] = true
		told = append(told, Notify{role, resolve(role)})
	}
	return chain, told
}

type Stage struct {
	SLAHours int
	Esc      string
}

// StageSLA is each stage's turnaround and escalation role (spec 7.2).
var StageSLA = map[string]Stage{
	"Line Manager": {24, "HRBP"}, "Functional Manager": {24, "HRBP"}, "Receiving Manager": {24, "HRBP"},
	"Receiving Property Manager": {24, "Receiving Cluster Manager"}, "Receiving Cluster Manager": {24, ""},
	"Receiving Function Head": {24, "CHRO"}, "Property Manager": {24, "Cluster Manager"}, "Function Head": {24, "CHRO"},
	"HRBP": {24, "HRBP Corporate"}, "Cluster Manager": {24, "Operations Head"}, "Operations Head": {48, ""},
	"HRBP Corporate": {24, "CHRO"}, "Payroll": {48, "Payroll Manager"}, "Payroll Manager": {48, ""},
	"CHRO": {48, ""}, "Finance Head": {48, "CHRO"},
}

// StageOrder lists the stages in the order the flow editor offers them.
var StageOrder = []string{"Line Manager", "Functional Manager", "Property Manager", "Cluster Manager", "City Head", "Operations Head", "Function Head",
	"HRBP", "HRBP Corporate", "Receiving Manager", "Receiving Property Manager", "Receiving Cluster Manager", "Receiving Function Head",
	"Payroll", "Payroll Manager", "CHRO", "Finance Head"}

func SLA(stage string) Stage {
	if s, ok := StageSLA[stage]; ok {
		return s
	}
	return Stage{24, ""}
}

// StageRole maps receiving stages to the role behind them.
func StageRole(stage string) string {
	switch stage {
	case "Functional Manager", "Receiving Manager":
		return "Line Manager"
	case "Receiving Property Manager":
		return "Property Manager"
	case "Receiving Cluster Manager":
		return "Cluster Manager"
	case "Receiving Function Head":
		return "Function Head"
	}
	return stage
}

// Fallback decides a request when a blocked last stage has no escalation role left to try.
var Fallback = []string{"HR Admin", "CHRO"}

// RouteVacant moves past stages nobody can decide. blocked says why a stage cannot be decided
// (its approver has left, nobody is named, or the approver raised the request, is the person it
// is about or already approved an earlier stage), or "" when it can. Such a stage moves to the
// next stage; at the last stage it goes to the escalation role, then to the fallback roles.
// Mutates chain and returns the step and the notes for the history.
func RouteVacant(chain []string, step int, blocked func(stage string, earlier []string) string) ([]string, int, []string) {
	var notes []string
	tried := map[string]bool{}
	for guard := 0; step < len(chain) && guard < 12; guard++ {
		stage := chain[step]
		tried[stage] = true
		why := blocked(stage, chain[:step])
		if why == "" {
			break
		}
		if step+1 < len(chain) {
			notes = append(notes, why+". Moved to "+chain[step+1])
			step++
			continue
		}
		next := ""
		for _, cand := range append([]string{SLA(stage).Esc}, Fallback...) {
			if cand != "" && !tried[cand] {
				next = cand
				break
			}
		}
		if next == "" {
			notes = append(notes, why+". Nobody else can decide it")
			break
		}
		notes = append(notes, why+". Escalated to "+next)
		chain[step] = next
	}
	return chain, step, notes
}

// MatrixKey: leave uses the C (casual and sick) or P (privilege and other) rules.
func MatrixKey(wf, leaveGroup string) string {
	if wf == "leave" {
		return "leave" + orDefault(leaveGroup, "C")
	}
	return wf
}
