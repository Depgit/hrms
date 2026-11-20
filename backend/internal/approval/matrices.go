package approval

import (
	"encoding/json"
	"fmt"
)

// Seed grids from the Approval_flow sheet (spec 7.3), as the grids they were written as; the database is seeded with
// them converted to rule lists (SeedFlows), editable after seeding.

const (
	LM  = "Line Manager"
	PM  = "Property Manager"
	HB  = "HRBP"
	CH  = "HRBP Corporate"
	CM  = "Cluster Manager"
	FH  = "Function Head"
	OH  = "Operations Head"
	CO  = "CHRO"
	RM  = "Receiving Manager"
	RP  = "Receiving Property Manager"
	RC  = "Receiving Cluster Manager"
	PY  = "Payroll"
	PYM = "Payroll Manager"
	FIN = "Finance Head"
)

type seeder struct{ n int }

func (s *seeder) row(label string, c Condition, approve []string, notify []string, sk ...string) Row {
	s.n++
	cells := map[string]string{}
	for _, r := range approve {
		cells[r] = "A"
	}
	for _, r := range notify {
		cells[r] = "N"
	}
	r := Row{ID: fmt.Sprintf("r%d", s.n), Label: label, C: c, On: true, Cells: cells}
	if len(sk) > 0 {
		r.SK = sk[0]
	}
	return r
}

func f(v float64) *float64 { return &v }

var always = Condition{K: "always"}

func days(min float64, max *float64) Condition { return Condition{K: "days", Min: f(min), Max: max} }
func grades(l ...string) Condition             { return Condition{K: "grades", List: l} }
func cnt(rt string, min float64, max *float64) Condition {
	return Condition{K: "cnt", RT: rt, Min: f(min), Max: max}
}
func amt(min float64, max *float64) Condition { return Condition{K: "amount", Min: f(min), Max: max} }
func sk(v string) Condition                   { return Condition{K: "skind", Val: v} }
func r(x ...string) []string                  { return x }

// Workflows lists every workflow key and its display name.
var Workflows = [][2]string{
	{"joining", "Offer approval"}, {"verify", "Joining verification"}, {"leave", "Leave"}, {"regularize", "Attendance regularization"},
	{"transfer", "Employee transfer"}, {"resign", "Separation"}, {"p_claim", "Reimbursement claim"}, {"p_adv", "Salary advance"},
	{"p_ded", "Salary deduction"}, {"p_arrear", "Arrear"}, {"p_change", "Bank or profile change"}, {"p_fnf", "Full and final"},
	{"p_ack", "Company policy acknowledgement"}, // its flow is seeded by migration 024
	{"shortlist", "Shortlist approval"},         // onboarding stages: seeded by migration 028
	{"interview", "Interview"},                  // the hiring manager's interview (migration 028), not an editable route
}

// SeedMatrices returns matrix[wfKey][flow].
func SeedMatrices() map[string]map[Flow]Matrix {
	s := &seeder{}
	both := func(cols []string, rows ...Row) map[Flow]Matrix {
		return map[Flow]Matrix{Operations: {cols, rows}, Corporate: {append([]string{}, cols...), append([]Row{}, rows...)}}
	}
	opc := r(LM, PM, HB, CH, CM, FH, OH)
	crc := r(LM, FH, HB, CO)
	return map[string]map[Flow]Matrix{
		"joining": {
			Operations: {r(HB, CM, CH), []Row{s.row("Every offer", always, r(HB, CM), nil), s.row("Above budget", Condition{K: "budget"}, r(HB, CM, CH), nil)}},
			Corporate:  {r(CH, CO), []Row{s.row("Every offer", always, r(CH), nil), s.row("Above budget", Condition{K: "budget"}, r(CH, CO), nil)}},
		},
		"verify": both(r(PY, PYM), s.row("Every joining", always, r(PY, PYM), nil)),
		"leaveC": {
			Operations: {opc, []Row{s.row("1 day in 30 days", days(1, f(1)), r(LM, PM), nil), s.row("2 days in 30 days", days(2, f(2)), r(LM, PM), nil), s.row("3 days and above in 30 days", days(3, nil), r(LM, PM, HB), nil)}},
			Corporate:  {crc, []Row{s.row("1 day in 30 days", days(1, f(1)), r(LM), nil), s.row("2 days in 30 days", days(2, f(2)), r(LM), nil), s.row("3 days and above in 30 days", days(3, nil), r(LM), nil)}},
		},
		"leaveP": {
			Operations: {opc, []Row{
				s.row("Up to 5 days in 30 days", days(1, f(5)), r(LM, PM), nil), s.row("6 to 10 days in 30 days", days(6, f(10)), r(LM, PM, HB, CM), nil),
				s.row("11 days and above in 30 days", days(11, nil), r(LM, PM, HB, CM), nil),
				s.row("Grade G3 and G4", grades("G3", "G4"), r(LM, PM, HB, CM), r(CH, FH, OH)), s.row("Grade G5 and G6", grades("G5", "G6"), r(LM, PM, HB, CM, FH), r(CH, OH))}},
			Corporate: {crc, []Row{
				s.row("Up to 5 days in 30 days", days(1, f(5)), r(LM), nil), s.row("6 to 10 days in 30 days", days(6, f(10)), r(LM), nil),
				s.row("11 days and above in 30 days", days(11, nil), r(LM, FH, HB, CO), nil),
				s.row("Grade G3 and G4", grades("G3", "G4"), r(LM), r(FH, HB, CO)), s.row("Grade G5 and G6", grades("G5", "G6"), r(LM), r(FH, HB, CO))}},
		},
		"regularize": {
			Operations: {opc, []Row{
				s.row("No punch, 1st", cnt("No Punch", 1, f(1)), r(LM, PM), nil), s.row("No punch, 2 to 5", cnt("No Punch", 2, f(5)), r(LM, PM, HB), nil),
				s.row("No punch, 6 and above", cnt("No Punch", 6, nil), r(LM, PM, HB, CM), nil),
				s.row("All other, up to 3", cnt("All Other", 1, f(3)), r(LM, PM), nil), s.row("All other, 4 to 7", cnt("All Other", 4, f(7)), r(LM, PM), nil),
				s.row("All other, 8 and above", cnt("All Other", 8, nil), r(LM, PM, HB), nil),
				s.row("Grade G3 and G4", grades("G3", "G4"), r(LM, PM, HB, CM), r(CH, FH, OH)), s.row("Grade G5 and G6", grades("G5", "G6"), r(LM, PM, HB, CM, FH), r(CH, OH))}},
			Corporate: {crc, []Row{
				s.row("No punch, 1st", cnt("No Punch", 1, f(1)), r(LM), nil), s.row("No punch, 2 to 5", cnt("No Punch", 2, f(5)), r(LM), nil),
				s.row("No punch, 6 and above", cnt("No Punch", 6, nil), r(LM, FH, HB, CO), nil),
				s.row("All other, up to 3", cnt("All Other", 1, f(3)), r(LM), nil), s.row("All other, 4 to 7", cnt("All Other", 4, f(7)), r(LM), nil),
				s.row("All other, 8 and above", cnt("All Other", 8, nil), r(LM, FH, HB, CO), nil),
				s.row("Grade G3 and G4", grades("G3", "G4"), r(LM), r(FH, HB, CO)), s.row("Grade G5 and G6", grades("G5", "G6"), r(LM, FH, HB, CO), nil)}},
		},
		"transfer": {
			Operations: {append(append([]string{}, opc...), RM, RP, RC), []Row{
				s.row("Grade G1 and G2", grades("G1", "G2"), r(LM, PM, HB, CM), r(CH, FH, OH)), s.row("Grade G3 and G4", grades("G3", "G4"), r(LM, PM, HB, CM), r(CH, FH, OH)),
				s.row("Grade G5 and G6", grades("G5", "G6"), r(LM, PM, HB, CM), r(CH, FH, OH)), s.row("Receiving side", always, r(RM, RP, RC), nil)}},
			Corporate: {append(append([]string{}, crc...), RM), []Row{
				s.row("Grade G1 and G2", grades("G1", "G2"), r(LM, FH, HB), r(CO)), s.row("Grade G3 and G4", grades("G3", "G4"), r(LM, FH, HB), r(CO)),
				s.row("Grade G5 and G6", grades("G5", "G6"), r(LM, FH, HB, CO), nil), s.row("Receiving side", always, r(RM), nil)}},
		},
		"resign": {
			Operations: {append(append([]string{}, opc...), CO), []Row{
				s.row("Resignation, grade G1 and G2", grades("G1", "G2"), r(LM, PM), nil, "Resignation"),
				s.row("Resignation, grade G3 and G4", grades("G3", "G4"), r(LM, PM, HB, CM), r(CH, FH, OH), "Resignation"),
				s.row("Resignation, grade G5 and G6", grades("G5", "G6"), r(LM, PM, HB, CM, FH), r(CH, OH), "Resignation"),
				s.row("Resignation, notice waiver requested", Condition{K: "waive"}, r(CM, CH), nil, "Resignation"),
				s.row("Termination", sk("Termination"), r(HB, CM, CH, CO), nil), s.row("Absconding", sk("Absconding"), r(HB, CM, CH), nil),
				s.row("Contract end", sk("Contract end"), r(HB, CH), nil), s.row("Retirement", sk("Retirement"), r(HB, CH), nil),
				s.row("Death", sk("Death"), r(HB, CH), r(CO))}},
			Corporate: {crc, []Row{
				s.row("Resignation, grade G1 and G2", grades("G1", "G2"), r(LM, FH, HB), r(CO), "Resignation"),
				s.row("Resignation, grade G3 and G4", grades("G3", "G4"), r(LM, FH, HB), r(CO), "Resignation"),
				s.row("Resignation, grade G5 and G6", grades("G5", "G6"), r(LM, FH, HB, CO), nil, "Resignation"),
				s.row("Resignation, notice waiver requested", Condition{K: "waive"}, r(CO), nil, "Resignation"),
				s.row("Termination", sk("Termination"), r(FH, HB, CO), nil), s.row("Absconding", sk("Absconding"), r(FH, HB, CO), nil),
				s.row("Contract end", sk("Contract end"), r(HB), nil), s.row("Retirement", sk("Retirement"), r(HB, CO), nil),
				s.row("Death", sk("Death"), r(HB), r(CO))}},
		},
		"p_claim": both(r(LM, PY, PYM, FIN), s.row("Up to ₹5,000", amt(0, f(5000)), r(LM, PY), nil),
			s.row("₹5,001 to ₹25,000", amt(5001, f(25000)), r(LM, PY, PYM), nil), s.row("Above ₹25,000", amt(25001, nil), r(LM, PY, PYM, FIN), nil)),
		"p_adv":    both(r(LM, HB, PY, PYM, CO), s.row("Up to ₹20,000", amt(0, f(20000)), r(LM, HB, PY), nil), s.row("Above ₹20,000", amt(20001, nil), r(LM, HB, PY, PYM, CO), nil)),
		"p_ded":    both(r(LM, HB, PY, PYM, CO), s.row("Up to ₹20,000", amt(0, f(20000)), r(LM, HB, PY), nil), s.row("Above ₹20,000", amt(20001, nil), r(LM, HB, PY, PYM, CO), nil)),
		"p_arrear": both(r(PYM, FIN), s.row("Up to ₹25,000", amt(0, f(25000)), r(PYM), nil), s.row("Above ₹25,000", amt(25001, nil), r(PYM, FIN), nil)),
		"p_change": both(r(PY, HB), s.row("Bank account change", Condition{K: "ckind", Val: "Bank"}, r(PY), nil),
			s.row("Personal details change", Condition{K: "ckind", Val: "Personal"}, r(HB), nil)),
		"p_fnf": both(r(PY, PYM, FIN, CO), s.row("Up to ₹1,00,000", amt(0, f(100000)), r(PY, PYM), nil),
			s.row("₹1,00,001 to ₹5,00,000", amt(100001, f(500000)), r(PY, PYM, FIN), nil), s.row("Above ₹5,00,000", amt(500001, nil), r(PY, PYM, FIN, CO), nil)),
	}
}

// SeedFlows returns the seeded rule lists, rules[wfKey][flow]: the seed grids (SeedMatrices) converted, except the
// workflows decided by a number of leave days or an amount, whose lists are written out so their ranges meet
// (rangeFlows).
func SeedFlows() map[string]map[Flow]RuleSet {
	out := map[string]map[Flow]RuleSet{}
	for wf, flows := range SeedMatrices() {
		out[wf] = map[Flow]RuleSet{}
		for flow, m := range flows {
			out[wf][flow] = MustConvert(wf, m)
		}
	}
	for wf, flows := range rangeFlows() {
		for flow, rs := range flows {
			out[wf][flow] = rs
		}
	}
	return out
}

// rangeFlows are the seeded lists of the workflows decided by a number. The sheet's ranges ("1 day", "2 days",
// "3 days and above"; "up to ₹5,000", "₹5,001 to ₹25,000", "above ₹25,000") leave gaps: half a day, 1.5 days,
// ₹5,000.50. A request in a gap fitted no row, so nobody approved it. Checked from the top, "up to" rules meet:
// each takes what no rule above it took, and Otherwise takes the top range. Everywhere else they route exactly as
// the sheet (TestSeedFlowsMeet).
func rangeFlows() map[string]map[Flow]RuleSet {
	upTo := func(k string, max float64) Condition { return Condition{K: k, Max: f(max)} }
	rule := func(when []Condition, approve, notify []string) Rule {
		if notify == nil {
			notify = []string{}
		}
		return Rule{Label: RuleLabel(when), On: true, When: when, Approvers: approve, Notify: notify}
	}
	list := func(otherwise []string, rules ...Rule) RuleSet {
		rules = append(rules, Rule{ID: "otherwise", Label: OtherwiseLabel, On: true, When: []Condition{}, Approvers: otherwise, Notify: []string{}})
		for i := range rules[:len(rules)-1] {
			rules[i].ID = fmt.Sprintf("r%d", i+1)
		}
		return RuleSet{Rules: rules}
	}
	both := func(build func() RuleSet) map[Flow]RuleSet {
		return map[Flow]RuleSet{Operations: build(), Corporate: build()}
	}
	days := func(max float64) []Condition { return []Condition{upTo("days", max)} }
	amount := func(max float64) []Condition { return []Condition{upTo("amount", max)} }
	return map[string]map[Flow]RuleSet{
		"leaveC": {
			Operations: list(r(LM, PM, HB), rule(days(1), r(LM, PM), nil), rule(days(2), r(LM, PM), nil)),
			Corporate:  list(r(LM), rule(days(1), r(LM), nil), rule(days(2), r(LM), nil)),
		},
		"leaveP": {
			Operations: list(r(LM, PM, HB, CM),
				rule([]Condition{grades("G3", "G4")}, r(LM, PM, HB, CM), r(CH, FH, OH)), rule([]Condition{grades("G5", "G6")}, r(LM, PM, HB, CM, FH), r(CH, OH)),
				rule(days(5), r(LM, PM), nil), rule(days(10), r(LM, PM, HB, CM), nil)),
			Corporate: list(r(LM, FH, HB, CO),
				rule([]Condition{grades("G3", "G4", "G5", "G6"), upTo("days", 10)}, r(LM), r(FH, HB, CO)),
				rule(days(5), r(LM), nil), rule(days(10), r(LM), nil)),
		},
		"p_claim": both(func() RuleSet {
			return list(r(LM, PY, PYM, FIN), rule(amount(5000), r(LM, PY), nil), rule(amount(25000), r(LM, PY, PYM), nil))
		}),
		"p_adv":    both(func() RuleSet { return list(r(LM, HB, PY, PYM, CO), rule(amount(20000), r(LM, HB, PY), nil)) }),
		"p_ded":    both(func() RuleSet { return list(r(LM, HB, PY, PYM, CO), rule(amount(20000), r(LM, HB, PY), nil)) }),
		"p_arrear": both(func() RuleSet { return list(r(PYM, FIN), rule(amount(25000), r(PYM), nil)) }),
		"p_fnf": both(func() RuleSet {
			return list(r(PY, PYM, FIN, CO), rule(amount(100000), r(PY, PYM), nil), rule(amount(500000), r(PY, PYM, FIN), nil))
		}),
	}
}

// SeedFlowUpgrade gives the seeded list with meeting ranges (rangeFlows) for a stored list that is still exactly
// the list seeded before (the sheet's grid converted, gaps and all), and false for any other list: a list HR has
// changed is never replaced.
func SeedFlowUpgrade(key string, flow Flow, rules []Rule) (RuleSet, bool) {
	next, ok := rangeFlows()[key][flow]
	if !ok {
		return RuleSet{}, false
	}
	m, ok := SeedMatrices()[key][flow]
	if !ok {
		return RuleSet{}, false
	}
	was, err1 := json.Marshal(MustConvert(key, m).Rules)
	is, err2 := json.Marshal(rules)
	if err1 != nil || err2 != nil || string(was) != string(is) {
		return RuleSet{}, false
	}
	return next, true
}
