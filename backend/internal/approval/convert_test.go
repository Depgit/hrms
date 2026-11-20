package approval

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The proof that converting a grid keeps every route: for every seeded grid and a set of hand-edited ones, the old
// way (buildMatrix: every matching row combined, column order) and the new way (Build: the first rule that fits)
// are compared on a grid of facts built here independently of the converter: every end value of every range, ±1
// and ±0.5 around it, 0 and negatives; every grade, transfer type, claim type, change and exit type named plus one
// named nowhere and none; yes and no; leave days given as the 30-day total or as the request's days; for an offer,
// every role as the person who raised it. Each fact is routed with resolvers that give everyone a different person,
// that give two roles the same person, and that leave roles unnamed. The routes (approvers and the people told,
// with names) must be identical. Why differs on purpose (the rule that fits, not every matching row).

// resolvers used for every comparison.
var proofResolvers = map[string]func(string) string{
	"distinct": func(r string) string { return "P:" + r },
	"shared": func(r string) string { // two roles, one person (a manager who is also the property manager …)
		switch r {
		case PM:
			return "P:" + LM
		case CH, CO:
			return "P:HRC"
		case RC:
			return "P:" + CM
		case PYM:
			return "P:" + PY
		case FH:
			return "P:" + HB
		}
		return "P:" + r
	},
	"unnamed": func(r string) string { // nobody named for some roles: those stages are kept, each one
		switch r {
		case CM, CH, RP, RC, FIN:
			return Unassigned
		case OH, CO:
			return ""
		}
		return "P:" + r
	},
	"everyone the same": func(string) string { return "P:one" },
}

func proofNums(bounds []float64, abs bool) []float64 {
	seen := map[float64]bool{}
	var out []float64
	add := func(v float64) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, b := range append([]float64{0}, bounds...) {
		for _, d := range []float64{-1, -0.5, 0, 0.5, 1} {
			add(b + d)
			if abs {
				add(-(b + d))
			}
		}
	}
	sort.Float64s(out)
	return out
}

// proofFacts is the grid of facts for one grid (key is its workflow key).
func proofFacts(key string, m Matrix) []Facts {
	wf := factWF(key)
	bounds := map[string][]float64{}
	vals := map[string][]string{}
	used := map[string]bool{}
	for _, r := range m.Rows {
		c := r.C
		used[c.K] = true
		if c.Min != nil {
			bounds[c.K] = append(bounds[c.K], *c.Min)
		}
		if c.Max != nil {
			bounds[c.K] = append(bounds[c.K], *c.Max)
		}
		vals[c.K] = append(vals[c.K], c.List...)
		if c.Val != "" {
			vals[c.K] = append(vals[c.K], c.Val)
		}
		if c.RT != "" {
			vals["rt"] = append(vals["rt"], c.RT)
		}
		if r.SK != "" {
			used["skind"] = true
		}
	}
	if wf == "resign" {
		used["skind"], used["waive"] = true, true
	}
	type axis []func(*Facts)
	var axes []axis
	texts := func(k string, extra ...string) []string {
		return append(append(append([]string{}, vals[k]...), extra...), "", "Nowhere named")
	}
	strAxis := func(l []string, set func(*Facts, string)) axis {
		a := axis{}
		for _, v := range l {
			v := v
			a = append(a, func(f *Facts) { set(f, v) })
		}
		return a
	}
	numAxis := func(l []float64, set func(*Facts, float64)) axis {
		a := axis{}
		for _, v := range l {
			v := v
			a = append(a, func(f *Facts) { set(f, v) })
		}
		return a
	}
	boolAxis := func(set func(*Facts, bool)) axis {
		return axis{func(f *Facts) { set(f, false) }, func(f *Facts) { set(f, true) }}
	}
	if used["days"] {
		ns := proofNums(bounds["days"], false)
		a := numAxis(ns, func(f *Facts, v float64) { f.Roll = v })
		a = append(a, numAxis(ns, func(f *Facts, v float64) { f.Roll, f.Days = 0, v })...) // the request's own days
		axes = append(axes, a)
	}
	if used["grades"] {
		axes = append(axes, strAxis(texts("grades", "G1", "G6", "G7"), func(f *Facts, v string) { f.Grade = v }))
	}
	if used["cnt"] {
		ns := []float64{}
		for _, n := range proofNums(bounds["cnt"], false) {
			if n == float64(int(n)) { // the count of requests is a whole number
				ns = append(ns, n)
			}
		}
		axes = append(axes, numAxis(ns, func(f *Facts, v float64) { f.Cnt = v }))
		axes = append(axes, strAxis(texts("rt", "No Punch", "All Other", "Any"), func(f *Facts, v string) { f.RType = v }))
	}
	if used["amount"] {
		axes = append(axes, numAxis(proofNums(bounds["amount"], true), func(f *Facts, v float64) { f.Amt = v }))
	}
	if used["pct"] {
		axes = append(axes, numAxis(proofNums(bounds["pct"], true), func(f *Facts, v float64) { f.Pct = v }))
	}
	if used["tkind"] {
		axes = append(axes, strAxis(texts("tkind", TransferKinds...), func(f *Facts, v string) { f.TKind = v }))
	}
	if used["ctype"] {
		axes = append(axes, strAxis(texts("ctype", "Travel", "Other"), func(f *Facts, v string) { f.CType = v }))
	}
	if used["ckind"] {
		axes = append(axes, strAxis(texts("ckind", ChangeKinds...), func(f *Facts, v string) { f.CKind = v }))
	}
	if used["skind"] {
		axes = append(axes, strAxis(texts("skind", ExitTypes...), func(f *Facts, v string) { f.SKind = v }))
	}
	if used["budget"] || wf == "joining" {
		axes = append(axes, boolAxis(func(f *Facts, v bool) { f.AboveBudget = v }))
	}
	if used["waive"] {
		axes = append(axes, boolAxis(func(f *Facts, v bool) { f.Waive = v }))
	}
	if used["bandx"] {
		axes = append(axes, boolAxis(func(f *Facts, v bool) { f.BandX = v }))
	}
	if wf == "joining" { // the person who raised an offer does not approve it again
		axes = append(axes, strAxis(append([]string{""}, m.Cols...), func(f *Facts, v string) { f.InitiatorRole = v }))
	}
	out := []Facts{{WF: wf}}
	for _, a := range axes {
		next := make([]Facts, 0, len(out)*len(a))
		for _, f := range out {
			for _, set := range a {
				g := f
				set(&g)
				next = append(next, g)
			}
		}
		out = next
	}
	return out
}

// proofEdited are grids as HR may have edited them: rows switched off, rows added, columns reordered, rows that only
// notify, cells for roles that are not columns, exit types on other rows, overlapping ranges, every condition kind.
func proofEdited() map[string]map[Flow]Matrix {
	seed := SeedMatrices()
	s := &seeder{n: 1000}
	rev := func(l []string) []string {
		out := append([]string{}, l...)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}
	off := func(rs []Row, i int) []Row {
		out := append([]Row{}, rs...)
		out[i].On = false
		return out
	}
	lp := seed["leaveP"][Operations]
	rg := seed["regularize"][Operations]
	rs := seed["resign"][Operations]
	tr := seed["transfer"][Corporate]
	notifyOnly := s.row("Tell the cluster for 3 or 4", cnt("Any", 3, f(4)), nil, r(OH, CH))
	notifyOnly.Cells["Not a column"] = "A"
	waiverAll := s.row("Waiver, any exit", Condition{K: "waive"}, r(FH), r(CO))
	// The old editor saved a cleared "From" as no start and accepted a typed negative one: both, on overlapping rows.
	lc := seed["leaveC"][Operations]
	lcRows := append([]Row{}, lc.Rows...)
	lcRows[0].C = Condition{K: "days", Min: f(-1), Max: f(1)}
	lcRows[1].C = Condition{K: "days", Max: f(2)}
	lcRows[1].Cells = map[string]string{LM: "A", PM: "A", HB: "A"}
	return map[string]map[Flow]Matrix{
		"leaveC": {
			Operations: {lc.Cols, lcRows},
			Corporate: {r(LM, PM), []Row{s.row("Short leave", Condition{K: "days", Min: f(-1), Max: f(5)}, r(LM), nil),
				s.row("Up to 3", Condition{K: "days", Max: f(3)}, r(PM), nil), s.row("From -2", Condition{K: "days", Min: f(-2)}, nil, r(HB))}},
		},
		"p_adv": {
			Operations: {r(LM, HB, PY, CO), []Row{s.row("From -5 to 100", amt(-5, f(100)), r(LM), nil), s.row("Up to 50", Condition{K: "amount", Max: f(50)}, r(HB), nil),
				s.row("From -1", Condition{K: "amount", Min: f(-1)}, nil, r(CO)), s.row("Up to -3", Condition{K: "amount", Max: f(-3)}, r(PY), nil)}},
			Corporate: {r(LM, HB, PY), []Row{s.row("Any, from -2 to 3", cnt("Any", -2, f(3)), r(LM), nil), s.row("No punch, up to 5", Condition{K: "cnt", RT: "No Punch", Max: f(5)}, r(HB), nil),
				s.row("Pay change from -10", Condition{K: "pct", Min: f(-10), Max: f(4)}, r(PY), nil), s.row("Pay change up to 2", Condition{K: "pct", Max: f(2)}, nil, r(LM))}},
		},
		"leaveP": {
			Operations: {rev(lp.Cols), append(off(lp.Rows, 3), s.row("Over 20 days", days(20, nil), r(CO), r(FH)), s.row("Half day", days(0.5, f(0.5)), nil, r(CH)))},
			Corporate: {lp.Cols, []Row{s.row("Up to 2 days", Condition{K: "days", Max: f(2)}, r(LM), nil), s.row("2.5 to 4", days(2.5, f(4)), r(PM, LM), r(HB)),
				s.row("Every request", always, nil, r(HB)), s.row("Grade G1", grades("G1"), r(HB), r(LM))}},
		},
		"regularize": {
			Operations: {append([]string{OH}, rg.Cols[:len(rg.Cols)-1]...), append(off(rg.Rows, 0), notifyOnly)},
			Corporate:  {rg.Cols, []Row{s.row("Any, up to 3", Condition{K: "cnt", RT: "Any", Max: f(3)}, r(LM), nil), s.row("No punch, 2 and above", cnt("No Punch", 2, nil), r(HB), nil), s.row("Grade G2", grades("G2"), nil, r(CM))}},
		},
		"resign": {
			Operations: {rev(rs.Cols), append(off(rs.Rows, 1), s.row("Every termination", always, r(FH), nil, "Termination"), waiverAll,
				s.row("Retirement, twice", sk("Retirement"), r(OH), nil, "Retirement"), s.row("Death, grade G5", grades("G5"), r(LM), nil, "Death"))},
			Corporate: {rs.Cols, []Row{s.row("Grade G1 to G3", grades("G1", "G2", "G3"), r(LM, FH), nil), s.row("Grade G3 to G6, resignation", grades("G3", "G4", "G5", "G6"), r(HB), r(LM), "Resignation"),
				s.row("Termination", sk("Termination"), r(CO), r(FH))}},
		},
		"p_claim": {
			Operations: {r(LM, PY, PYM, FIN, HB), []Row{s.row("Up to ₹10,000", amt(0, f(10000)), r(LM), nil), s.row("₹5,000 and above", amt(5000, nil), r(PY), nil),
				s.row("Travel", Condition{K: "ctype", Val: "Travel"}, r(FIN), r(HB)), s.row("Grade G1", grades("G1"), nil, r(PYM)), s.row("Grade G1 or G2", grades("G1", "G2"), r(PYM), nil)}},
			Corporate: {r(LM, HB, CO), []Row{s.row("Pay change up to 10%", Condition{K: "pct", Max: f(10)}, r(LM), nil), s.row("Pay change above 10%", Condition{K: "pct", Min: f(10.01)}, r(LM, HB), nil),
				s.row("Outside the band", Condition{K: "bandx"}, r(CO), nil), s.row("Above budget", Condition{K: "budget"}, r(HB, CO), r(LM))}},
		},
		"transfer": {
			Operations: {append(r(RM), tr.Cols...), append(append([]Row{}, tr.Rows...), s.row("Cross state", Condition{K: "tkind", Val: "Cross state"}, r(OH), nil),
				s.row("Cross region", Condition{K: "tkind", Val: "Cross region"}, r(OH, CO), nil))},
			Corporate: {tr.Cols, off(off(off(off(tr.Rows, 0), 1), 2), 3)}, // every row switched off
		},
		"joining": {
			Operations: {r(CH, HB, CM), []Row{s.row("Every offer", always, r(HB), nil), s.row("Every offer, told", always, nil, r(HB, CM)), s.row("Above budget", Condition{K: "budget"}, r(CM, CH), nil),
				s.row("Exit type on an offer", always, r(CO), nil, "Termination")}}, // an exit type outside separation is not checked
		},
		"p_change": {
			Operations: {r(PY, HB), []Row{s.row("Bank", Condition{K: "ckind", Val: "Bank"}, r(PY), nil), s.row("Bank, told", Condition{K: "ckind", Val: "Bank"}, nil, r(HB)),
				s.row("Exit type in a change", sk("Resignation"), r(HB), nil)}}, // outside separation every request counts as a resignation
		},
	}
}

func TestConvertKeepsEveryRoute(t *testing.T) {
	type grid struct {
		name string
		key  string
		m    Matrix
	}
	var grids []grid
	for _, set := range []struct {
		name string
		ms   map[string]map[Flow]Matrix
	}{{"seeded", SeedMatrices()}, {"edited", proofEdited()}} {
		keys := []string{}
		for k := range set.ms {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, fl := range []Flow{Operations, Corporate} {
				if m, ok := set.ms[k][fl]; ok {
					grids = append(grids, grid{set.name + " " + k + "/" + string(fl), k, m})
				}
			}
		}
	}
	compared, sizes := 0, []string{}
	for _, g := range grids {
		rs, err := Convert(g.key, g.m)
		if err != nil {
			t.Fatalf("%s: %v", g.name, err)
		}
		if _, err := Check(rs.Rules, func(string) bool { return true }); err != nil {
			t.Errorf("%s: the converted rules cannot be saved: %v", g.name, err)
		}
		if last := rs.Rules[len(rs.Rules)-1]; last.Label != OtherwiseLabel || len(last.When) != 0 {
			t.Errorf("%s: last rule %+v", g.name, last)
		}
		on := 0
		for _, r := range g.m.Rows {
			if r.On {
				on++
			}
		}
		sizes = append(sizes, fmt.Sprintf("%s: %d rows on → %d rules", g.name, on, len(rs.Rules)))
		facts := proofFacts(g.key, g.m)
		diffs := 0
		for _, f := range facts {
			for rn, res := range proofResolvers {
				old, now := buildMatrix(g.m, f, res), Build(rs, f, res)
				compared++
				if !reflect.DeepEqual(old.Chain, now.Chain) || !reflect.DeepEqual(old.Notify, now.Notify) {
					diffs++
					if diffs <= 3 {
						t.Errorf("%s, %s, facts %+v:\n  grid  %v %v\n  rules %v %v (rule %q)", g.name, rn, f, old.Chain, old.Notify, now.Chain, now.Notify, now.Why)
					}
				}
			}
		}
		if diffs > 0 {
			t.Errorf("%s: %d differences in %d comparisons", g.name, diffs, len(facts)*len(proofResolvers))
		}
	}
	t.Logf("%d grids, %d routes compared, every one the same\n%s", len(grids), compared, strings.Join(sizes, "\n"))
	if compared < 10000 {
		t.Fatalf("only %d comparisons", compared)
	}
}

func TestConvertLabels(t *testing.T) {
	eq(t, RuleLabel([]Condition{days(6, f(10)), grades("G4", "G3")}), "Grade G3–G4, 6 to 10 days in 30 days")
	eq(t, RuleLabel([]Condition{{K: "waive"}, grades("G1", "G2", "G5"), sk("Resignation")}), "Resignation, grade G1–G2, G5, notice waiver requested")
	eq(t, RuleLabel([]Condition{amt(0, f(100000))}), "Up to ₹1,00,000")
	eq(t, RuleLabel([]Condition{amt(500001, nil)}), "₹5,00,001 or more")
	eq(t, RuleLabel([]Condition{cnt("No Punch", 1, f(1))}), "1 no-punch request this month")
	eq(t, RuleLabel([]Condition{cnt("Any", 2, f(5))}), "2 to 5 requests this month")
	eq(t, RuleLabel([]Condition{days(1, f(1))}), "1 day in 30 days")
	eq(t, RuleLabel([]Condition{{K: "tkind", Val: "Cross city"}}), "Cross city transfer")
	eq(t, RuleLabel(nil), OtherwiseLabel)
}

func TestCheck(t *testing.T) {
	all := func(string) bool { return true }
	g := []Condition{grades("G1")}
	bad := map[string][]Rule{
		"A flow needs at least the Otherwise rule":                                                                {},
		"The last rule must be Otherwise, with no conditions":                                                     {{When: g}},
		"Rule 1 (“x”) has no condition. Add one, or delete the rule":                                              {{Label: "x"}, {}},
		"Rule 1 (“x”): pick at least one grade":                                                                   {{Label: "x", When: []Condition{{K: "grades"}}}, {}},
		"Rule 1 (“x”): the upper number cannot be below the lower one":                                            {{Label: "x", When: []Condition{days(5, f(2))}}, {}},
		"Rule 1 (“x”): pick an exit type (Resignation, Termination, Absconding, Contract end, Retirement, Death)": {{Label: "x", When: []Condition{sk("Fired")}}, {}},
		"Rule 1 (“x”) checks the grade twice. Keep one condition of each kind":                                    {{Label: "x", When: []Condition{grades("G1"), grades("G2")}}, {}},
		"Rule 1 (“x”) lists HRBP twice as an approver":                                                            {{Label: "x", When: g, Approvers: r(HB, LM, HB)}, {}},
		"Rule 2 (“Otherwise”): HRBP cannot both approve and be told. Keep one":                                    {{Label: "x", When: g}, {Approvers: r(HB), Notify: r(HB)}},
		"Rule 1 (“x”): pick what the condition checks":                                                            {{Label: "x", When: []Condition{{K: "always"}}}, {}},
	}
	for want, rules := range bad {
		if _, err := Check(rules, all); err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	if _, err := Check([]Rule{{Approvers: r("Nobody")}}, func(r string) bool { return r != "Nobody" }); err == nil || err.Error() != "Nobody cannot approve requests" {
		t.Error(err)
	}
	out, err := Check([]Rule{{ID: "a", When: []Condition{days(1, nil)}, Approvers: r(LM)}, {ID: "a", Label: "anything", On: false}}, all)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, out[0].Label, "1 or more days in 30 days") // named from its conditions
	eq(t, out[1].Label, OtherwiseLabel)
	eq(t, out[1].On, true)
	eq(t, out[1].ID != "a", true) // ids are unique
	eq(t, out[1].Approvers, []string{})
}

// The comparison above does find a difference when there is one.
func TestProofCatchesADifference(t *testing.T) {
	m := SeedMatrices()["leaveP"][Operations]
	rs := MustConvert("leaveP", m)
	broken := []RuleSet{{Rules: append([]Rule{}, rs.Rules...)}, {Rules: append([]Rule{}, rs.Rules...)}, {Rules: append([]Rule{}, rs.Rules[1:]...)}}
	broken[0].Rules[0].Approvers = []string{PM, LM, HB, CM} // two steps the other way round
	broken[1].Rules[3].When = []Condition{days(6, nil)}     // 10.5 days in 30 days now needs approval
	for i, b := range broken {
		diffs := 0
		for _, f := range proofFacts("leaveP", m) {
			if !reflect.DeepEqual(buildMatrix(m, f, proofResolvers["distinct"]).Chain, Build(b, f, proofResolvers["distinct"]).Chain) {
				diffs++
			}
		}
		if diffs == 0 {
			t.Errorf("broken list %d: no difference found", i)
		}
	}
}
