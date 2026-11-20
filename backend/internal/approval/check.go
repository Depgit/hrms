package approval

import (
	"errors"
	"fmt"
	"strings"
)

// Check validates a rule list before it is saved and returns it tidied: the last rule is Otherwise (no conditions,
// always on), every other rule has at least one condition and a name (named from its conditions when blank), each
// condition is complete, approvers and people told are roles that may take part (allowed), nobody is listed twice
// in a rule nor both approves and is told, and every rule has an id. The messages are for HR.
func Check(rules []Rule, allowed func(role string) bool) ([]Rule, error) {
	if len(rules) == 0 {
		return nil, errors.New("A flow needs at least the Otherwise rule")
	}
	out := make([]Rule, len(rules))
	ids := map[string]bool{}
	for i, r := range rules {
		last := i == len(rules)-1
		r.Label = strings.TrimSpace(r.Label)
		name := r.Label
		if name == "" {
			name = fmt.Sprintf("rule %d", i+1)
		}
		if last {
			if len(r.When) > 0 {
				return nil, errors.New("The last rule must be Otherwise, with no conditions")
			}
			r.Label, r.On, name = OtherwiseLabel, true, OtherwiseLabel
		} else if len(r.When) == 0 {
			return nil, fmt.Errorf("Rule %d (“%s”) has no condition. Add one, or delete the rule", i+1, name)
		}
		kinds := map[string]bool{}
		when := make([]Condition, len(r.When))
		for j, c := range r.When {
			if err := checkCond(c); err != nil {
				return nil, fmt.Errorf("Rule %d (“%s”): %s", i+1, name, err.Error())
			}
			if kinds[c.K] {
				return nil, fmt.Errorf("Rule %d (“%s”) checks %s twice. Keep one condition of each kind", i+1, name, KindNames[c.K])
			}
			kinds[c.K] = true
			when[j] = cleanCond(c)
		}
		r.When = when
		if r.Label == "" {
			r.Label = RuleLabel(r.When)
		}
		seen := map[string]bool{}
		for _, role := range r.Approvers {
			if !allowed(role) {
				return nil, fmt.Errorf("%s cannot approve requests", role)
			}
			if seen[role] {
				return nil, fmt.Errorf("Rule %d (“%s”) lists %s twice as an approver", i+1, name, role)
			}
			seen[role] = true
		}
		told := map[string]bool{}
		for _, role := range r.Notify {
			if !allowed(role) {
				return nil, fmt.Errorf("%s cannot be told about requests", role)
			}
			if seen[role] {
				return nil, fmt.Errorf("Rule %d (“%s”): %s cannot both approve and be told. Keep one", i+1, name, role)
			}
			if told[role] {
				return nil, fmt.Errorf("Rule %d (“%s”) lists %s twice under Also tell", i+1, name, role)
			}
			told[role] = true
		}
		if r.Approvers == nil {
			r.Approvers = []string{}
		}
		if r.Notify == nil {
			r.Notify = []string{}
		}
		out[i] = r
	}
	// Ids: kept when unique, else given.
	n := 0
	for i := range out {
		if id := strings.TrimSpace(out[i].ID); id != "" && !ids[id] {
			out[i].ID = id
			ids[id] = true
			continue
		}
		for {
			n++
			if id := fmt.Sprintf("r%d", n); !ids[id] {
				out[i].ID = id
				ids[id] = true
				break
			}
		}
	}
	return out, nil
}

// KindNames names what each condition kind checks.
var KindNames = map[string]string{"days": "leave days", "grades": "the grade", "cnt": "requests this month", "budget": "above budget",
	"waive": "notice waiver", "tkind": "the transfer type", "amount": "the amount", "pct": "the pay change", "bandx": "the pay band",
	"ctype": "the claim type", "ckind": "the change", "skind": "the exit type"}

// checkCond says what is missing or wrong in one condition.
func checkCond(c Condition) error {
	rng := func() error {
		if (c.Min != nil && *c.Min < 0) || (c.Max != nil && *c.Max < 0) {
			return errors.New("the numbers cannot be below 0")
		}
		if c.Max != nil && *c.Max < lo(c) {
			return errors.New("the upper number cannot be below the lower one")
		}
		return nil
	}
	pick := func(what string, l []string) error {
		if !contains(l, c.Val) {
			return fmt.Errorf("pick %s (%s)", what, strings.Join(l, ", "))
		}
		return nil
	}
	switch c.K {
	case "days", "amount", "pct":
		return rng()
	case "cnt":
		if !contains(PunchTypes, c.RT) {
			return errors.New("pick No Punch, All Other or Any")
		}
		return rng()
	case "grades":
		if len(c.List) == 0 {
			return errors.New("pick at least one grade")
		}
		for _, g := range c.List {
			if strings.TrimSpace(g) == "" {
				return errors.New("a grade has no name")
			}
		}
		return nil
	case "budget", "waive", "bandx":
		return nil
	case "tkind":
		return pick("a transfer type", TransferKinds)
	case "ckind":
		return pick("Bank or Personal", ChangeKinds)
	case "skind":
		return pick("an exit type", ExitTypes)
	case "ctype":
		if strings.TrimSpace(c.Val) == "" {
			return errors.New("pick a claim type")
		}
		return nil
	}
	return errors.New("pick what the condition checks")
}

// kindsFor lists the condition kinds each workflow's requests carry (the editor's COND_OK): a condition of another
// kind could never hold for them. Both leave lists (leaveC, leaveP) are leave.
var kindsFor = map[string][]string{
	"shortlist": {}, "interview": {}, "verify": {}, "joining": {"budget"}, "leave": {"days", "grades"}, "regularize": {"cnt", "grades"},
	"transfer": {"grades", "tkind"}, "resign": {"grades", "waive", "skind"}, "p_claim": {"amount", "ctype", "grades"},
	"p_adv": {"amount", "grades"}, "p_arrear": {"amount"}, "p_ded": {"amount", "grades"}, "p_change": {"ckind"}, "p_fnf": {"amount"},
	"p_pip": {"amount"}, "p_ack": {"grades"},
}

// KindFits says whether a rule of a workflow (key: its workflow key, leaveC and leaveP for leave) may check a
// condition kind. Workflows not listed may check any kind.
func KindFits(key, kind string) bool {
	l, ok := kindsFor[factWF(key)]
	return !ok || contains(l, kind)
}

// RoleFits says whether a role may take part in a workflow's route: the receiving side (Receiving Manager,
// Receiving Property Manager …) exists only for a transfer.
func RoleFits(key, role string) bool {
	return factWF(key) == "transfer" || !strings.HasPrefix(role, "Receiving ")
}

var kindNouns = map[string]string{"days": "leave days", "grades": "grade", "cnt": "count of requests", "budget": "budget",
	"waive": "notice waiver", "tkind": "transfer type", "amount": "amount", "pct": "pay change", "bandx": "pay band",
	"ctype": "claim type", "ckind": "bank or personal change", "skind": "exit type"}

// CheckFits says, for HR, what in a rule list does not apply to the workflow (key): a condition its requests do
// not carry, or a role that only takes part in a transfer. nil when everything fits.
func CheckFits(key string, rules []Rule) error {
	for i, r := range rules {
		name := r.Label
		if i == len(rules)-1 {
			name = OtherwiseLabel
		}
		for _, c := range r.When {
			if !KindFits(key, c.K) {
				what := kindNouns[c.K]
				if what == "" {
					what = c.K
				}
				return fmt.Errorf("Rule %d (“%s”): this request type has no %s to check. Remove that condition", i+1, name, what)
			}
		}
		for _, role := range append(append([]string{}, r.Approvers...), r.Notify...) {
			if !RoleFits(key, role) {
				return fmt.Errorf("Rule %d (“%s”): %s only takes part in an employee transfer. Remove it", i+1, name, role)
			}
		}
	}
	return nil
}
