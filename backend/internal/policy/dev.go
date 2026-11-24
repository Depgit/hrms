package policy

// Deviation allowances (owner, 24 Nov 2026: "Deviation logics. Minutes and after x number of count. Early check in,
// late check in, early checkout … option to enable or disable"). Each rule lets a check-in or check-out differ from
// the planned shift by up to Mins minutes on up to N days of a calendar month; beyond that the admin is informed and
// the rule's Action decides the day (No action: the usual rules; Absent; Half day or Present until regularized).
// Both numbers are entered by the HR admin: nothing is prefilled ("by default kuch nahi … ye dono value admin dega,
// automatic nahi"). Every rule is off on every policy until it is turned on, so attendance and pay stay as they are.
// A person-specific value is a policy mapped to that person (the Individual mapping), or the person's own rules
// below (DevOwnRules), which HR gives from the roster without a separate policy.

// DevRule is one deviation allowance.
type DevRule struct {
	On   bool `json:"on"`
	Mins int  `json:"mins"` // minutes allowed (0 = not set)
	N    int  `json:"n"`    // times allowed in a calendar month (0 = not set)
	// Act is what a day beyond the allowance gets (owner, 24 Nov 2026): DevActNone, DevActAbsent, DevActHalf or
	// DevActPresent. No default: "" until the admin picks one, and a rule is not saved On without it. A rule saved On
	// before the Action existed reads as No action, the behaviour it had (Action).
	Act string `json:"act,omitempty"`
}

// The actions of a day beyond the allowance (DevRule.Act).
const (
	DevActNone    = "none"    // the usual rules (late mark, the late-mark limit, hours) and the bell
	DevActAbsent  = "absent"  // Absent, loss of pay 1, until a regularization of the date is approved
	DevActHalf    = "half"    // Half day until regularized: loss of pay 0.5 (as the missing-punch option)
	DevActPresent = "present" // Present until regularized: no loss of pay, flagged for regularization
)

// DevActs are the actions in the order the admin sees them.
var DevActs = []string{DevActNone, DevActAbsent, DevActHalf, DevActPresent}

// Active: the rule is on with both numbers set.
func (r DevRule) Active() bool { return r.On && r.Mins > 0 && r.N > 0 }

// Action is the action of a day beyond the allowance: "" for No action, and for a rule that has none yet (read as No
// action, so nothing saved before the Action existed changes).
func (r DevRule) Action() string {
	switch r.Act {
	case DevActAbsent, DevActHalf, DevActPresent:
		return r.Act
	}
	return ""
}

// DevRules are the three allowances of a work policy.
type DevRules struct {
	EarlyIn  DevRule `json:"earlyIn"`  // a check-in before the shift start
	LateIn   DevRule `json:"lateIn"`   // a check-in after the shift start and the late-mark grace
	EarlyOut DevRule `json:"earlyOut"` // a check-out before the shift end and the early-out grace
}

// Any: at least one allowance is in force.
func (d DevRules) Any() bool { return d.EarlyIn.Active() || d.LateIn.Active() || d.EarlyOut.Active() }

// One employee's own deviation allowances (owner, 24 Nov 2026: "HR admin dega us employee ko"): HR gives a rule to one
// person without a separate policy. Each rule either uses the work policy's rule (the default), is On with the
// person's own minutes and times (both typed by HR, the same limits as the policy), or is Off for the person. Kept as
// versions by "applies from" date (emp_dev_allow).

// Use values of an employee's own rule.
const (
	DevUsePolicy = "policy"
	DevUseOn     = "on"
	DevUseOff    = "off"
)

// DevOwn is one employee's own setting of one allowance. "Use the policy's rule" takes the policy's action too.
type DevOwn struct {
	Use  string `json:"use"`            // policy | on | off ("" = policy)
	Mins int    `json:"mins,omitempty"` // with on: minutes allowed
	N    int    `json:"n,omitempty"`    // with on: times allowed in a calendar month
	Act  string `json:"act,omitempty"`  // with on: the action beyond the allowance (DevRule.Act)
}

// Rule is the employee's rule over the policy's p.
func (o DevOwn) Rule(p DevRule) DevRule {
	switch o.Use {
	case DevUseOn:
		return DevRule{On: true, Mins: o.Mins, N: o.N, Act: o.Act}
	case DevUseOff:
		return DevRule{}
	}
	return p
}

// DevOwnRules are one employee's three settings.
type DevOwnRules struct {
	EarlyIn  DevOwn `json:"earlyIn"`
	LateIn   DevOwn `json:"lateIn"`
	EarlyOut DevOwn `json:"earlyOut"`
}

// Over lays the employee's settings over the policy's rules.
func (o DevOwnRules) Over(p DevRules) DevRules {
	return DevRules{EarlyIn: o.EarlyIn.Rule(p.EarlyIn), LateIn: o.LateIn.Rule(p.LateIn), EarlyOut: o.EarlyOut.Rule(p.EarlyOut)}
}

// Own: some rule is the employee's own (on or off), not the policy's.
func (o DevOwnRules) Own() bool {
	for _, x := range []DevOwn{o.EarlyIn, o.LateIn, o.EarlyOut} {
		if x.Use == DevUseOn || x.Use == DevUseOff {
			return true
		}
	}
	return false
}
