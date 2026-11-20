package approval

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Converting a grid (Matrix) into a rule list (RuleSet) that routes every request exactly as the grid did.
//
// A grid combined every matching row, so its route depends only on WHICH rows match. Every condition compares one
// fact with fixed values (a range's ends, a list of grades, an exit type …), so the facts fall into finitely many
// regions inside which every row matches or not as a whole: for a number, each end value and each stretch between
// two end values (whole numbers between the ends for the count of regularizations, which is always a whole
// number); for a text, each value named and one value named nowhere; for a yes/no fact, yes and no. One sample
// fact per region, combined over the facts the grid uses, gives the cells: the grid's route in a cell is its route
// for every request in that region.
//
// The rule list starts as one rule per set of matching rows that occurs (its conditions are those rows' conditions,
// ANDed), the largest sets first, and Otherwise (the rows with no condition) last: the first rule that fits a
// request is then the one for exactly the rows it matches. It is then made small: a rule is dropped, two rules with
// the same route are joined into one (a wider range, more grades) or a condition is dropped, each only when every
// cell still gets the grid's route; finally rules that can never both fit are put in a readable order. Every step
// is checked on all cells, and the result is checked once more, so a list that routes differently is never
// returned.

// kindOrder is the order of the conditions in a rule and in its label.
var kindOrder = []string{"skind", "ckind", "ctype", "tkind", "grades", "cnt", "days", "amount", "pct", "budget", "bandx", "waive"}

// Kinds lists every condition kind a rule may use ("always" is a rule without conditions).
var Kinds = append([]string{}, kindOrder...)

func kindRank(k string) int {
	for i, x := range kindOrder {
		if x == k {
			return i
		}
	}
	return len(kindOrder)
}

// Known values of the text conditions, in the order they are offered.
var (
	TransferKinds = []string{"Within city", "Cross city", "Cross state", "Cross region"}
	ChangeKinds   = []string{"Bank", "Personal"}
	ExitTypes     = []string{"Resignation", "Termination", "Absconding", "Contract end", "Retirement", "Death"}
	PunchTypes    = []string{"No Punch", "All Other", "Any"}
)

// factWF is the workflow on the facts of a grid's requests (both leave grids are leave).
func factWF(key string) string {
	if strings.HasPrefix(key, "leave") {
		return "leave"
	}
	return key
}

func fp(v float64) *float64 { return &v }

// lo is a range's start (none: from 0).
func lo(c Condition) float64 {
	if c.Min == nil {
		return 0
	}
	return *c.Min
}

// cleanCond keeps only what a condition's kind reads.
func cleanCond(c Condition) Condition {
	out := Condition{K: c.K}
	switch c.K {
	case "days", "amount", "pct", "cnt":
		if c.Min != nil {
			out.Min = fp(*c.Min)
		}
		if c.Max != nil {
			out.Max = fp(*c.Max)
		}
		if c.K == "cnt" {
			out.RT = c.RT
		}
	case "grades":
		out.List = append([]string{}, c.List...)
	case "tkind", "ctype", "ckind", "skind":
		out.Val = c.Val
	}
	return out
}

func knownKind(k string) bool { return k == "always" || kindRank(k) < len(kindOrder) }

// rowConds are a grid row's conditions as rule conditions: its exit type (separation) and its condition.
func rowConds(wf string, r Row) []Condition {
	var out []Condition
	if r.SK != "" && wf == "resign" {
		out = append(out, Condition{K: "skind", Val: r.SK})
	}
	if r.C.K != "always" {
		out = append(out, cleanCond(r.C))
	}
	return out
}

// condKey is a condition's canonical text, for comparing conditions.
func condKey(c Condition) string {
	n := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatFloat(*p, 'g', -1, 64)
	}
	l := append([]string{}, c.List...)
	sort.Strings(l)
	return c.K + "|" + n(c.Min) + "|" + n(c.Max) + "|" + c.RT + "|" + c.Val + "|" + strings.Join(l, "\x1f")
}

func whenKey(w []Condition) string {
	ks := make([]string, len(w))
	for i, c := range w {
		ks[i] = condKey(c)
	}
	sort.Strings(ks)
	return strings.Join(ks, "\x1e")
}

// conjoin ANDs conditions into at most one per kind, in kind order. false when they can never all hold.
func conjoin(cs []Condition) ([]Condition, bool) {
	byKind := map[string][]Condition{}
	for _, c := range cs {
		byKind[c.K] = append(byKind[c.K], c)
	}
	var out []Condition
	for _, k := range kindOrder {
		l := byKind[k]
		if len(l) == 0 {
			continue
		}
		c := cleanCond(l[0])
		for _, x := range l[1:] {
			switch k {
			case "days", "amount", "pct", "cnt":
				if k == "cnt" {
					switch {
					case x.RT == "Any":
					case c.RT == "Any":
						c.RT = x.RT
					case c.RT != x.RT:
						return nil, false
					}
				}
				// The higher start wins. No start means from 0, so it beats a negative start: a blank "From" and
				// a "From" of -1 together start at 0, not at -1.
				if x.Min != nil || c.Min != nil {
					c.Min = fp(math.Max(lo(c), lo(x)))
				}
				if x.Max != nil && (c.Max == nil || *x.Max < *c.Max) {
					c.Max = fp(*x.Max)
				}
			case "grades":
				keep := []string{}
				for _, g := range c.List {
					if contains(x.List, g) && !contains(keep, g) {
						keep = append(keep, g)
					}
				}
				c.List = keep
			case "tkind", "ctype", "ckind", "skind":
				if c.Val != x.Val {
					return nil, false
				}
			}
		}
		out = append(out, c)
	}
	return out, true
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// outside is a text no condition names.
func outside(vals []string) string {
	s := "~none~"
	for contains(vals, s) {
		s += "~"
	}
	return s
}

func uniqSorted(v []float64) []float64 {
	sort.Float64s(v)
	out := v[:0:0]
	for i, x := range v {
		if i == 0 || x != v[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// realSamples: one number in every region the end values split the number line into (each end value, a number
// between two of them, one below and one above). nonNeg drops samples below 0 (the amount and percent are compared
// as absolute values).
func realSamples(ends []float64, nonNeg bool) []float64 {
	b := uniqSorted(append(append([]float64{}, ends...), 0))
	s := []float64{b[0] - 1}
	for i, x := range b {
		s = append(s, x)
		if i+1 < len(b) {
			s = append(s, (x+b[i+1])/2)
		}
	}
	s = append(s, b[len(b)-1]+1)
	if nonNeg {
		keep := s[:0:0]
		for _, x := range s {
			if x >= 0 {
				keep = append(keep, x)
			}
		}
		s = keep
	}
	return s
}

// intSamples: the same for a whole number (the count of regularizations this month): a whole n is ≥ min exactly from
// ⌈min⌉ and ≤ max exactly below ⌊max⌋+1, so the regions start at those points.
func intSamples(mins, maxs []float64) []float64 {
	p := []float64{0, 1}
	for _, x := range mins {
		p = append(p, math.Ceil(x))
	}
	for _, x := range maxs {
		p = append(p, math.Floor(x)+1)
	}
	p = uniqSorted(p)
	return append([]float64{p[0] - 1}, p...)
}

// converter holds the cells of one grid and the grid's route in each.
type converter struct {
	wf    string
	cols  []string
	rows  []Row
	conds [][]Condition // per row
	cells []Facts
	want  []string // the grid's route per cell (routeKey)
	masks []string // which rows match, per cell
}

func ident(role string) string { return "\x00" + role }

// routeKey is a route (approvers in order, then the people told) as text, for comparing routes.
func routeKey(chain []string, told []Notify) string {
	n := make([]string, len(told))
	for i, x := range told {
		n[i] = x.Role
	}
	return strings.Join(chain, "\x1f") + "\x1e" + strings.Join(n, "\x1f")
}

// numKind says whether a condition kind compares a number. The number is never below 0: leave days and the count
// of requests are not, and the amount and the pay change are compared as absolute values (EvalCond).
func numKind(k string) bool { return k == "days" || k == "amount" || k == "pct" || k == "cnt" }

// normRange writes a grid row's range as a rule may hold it: a start below 0 (the old editor accepted one) is no
// start, since no number is below 0. ok is false when the range holds no number at all (it ends below 0 or below
// its start): such a row never matched.
func normRange(c Condition) (Condition, bool) {
	if !numKind(c.K) {
		return c, true
	}
	if c.Min != nil && *c.Min < 0 {
		c.Min = nil
	}
	if c.Max != nil && *c.Max < lo(c) {
		return c, false
	}
	return c, true
}

func newConverter(key string, m Matrix) (*converter, error) {
	cv := &converter{wf: factWF(key)}
	for _, c := range m.Cols {
		if !contains(cv.cols, c) {
			cv.cols = append(cv.cols, c)
		}
	}
	for _, r := range m.Rows {
		if !r.On || !knownKind(r.C.K) {
			continue
		}
		c, holds := normRange(r.C)
		if !holds {
			continue
		}
		r.C = c
		cv.rows = append(cv.rows, r)
		cv.conds = append(cv.conds, rowConds(cv.wf, r))
	}
	// The values each fact is compared with.
	used := map[string]bool{}
	ends := map[string][]float64{}
	var cntMins, cntMaxs []float64
	vals := map[string][]string{}
	for _, cs := range cv.conds {
		for _, c := range cs {
			used[c.K] = true
			switch c.K {
			case "days", "amount", "pct":
				ends[c.K] = append(ends[c.K], lo(c))
				if c.Max != nil {
					ends[c.K] = append(ends[c.K], *c.Max)
				}
			case "cnt":
				cntMins = append(cntMins, lo(c))
				if c.Max != nil {
					cntMaxs = append(cntMaxs, *c.Max)
				}
				if c.RT != "Any" && !contains(vals["rtype"], c.RT) {
					vals["rtype"] = append(vals["rtype"], c.RT)
				}
			case "grades":
				for _, g := range c.List {
					if !contains(vals["grades"], g) {
						vals["grades"] = append(vals["grades"], g)
					}
				}
			case "tkind", "ctype", "ckind", "skind":
				if !contains(vals[c.K], c.Val) {
					vals[c.K] = append(vals[c.K], c.Val)
				}
			}
		}
	}
	texts := func(k string, extra ...string) []string {
		l := append([]string{}, vals[k]...)
		sort.Strings(l)
		for _, e := range extra {
			if !contains(l, e) {
				l = append(l, e)
			}
		}
		return append(l, outside(l))
	}
	type axis []func(*Facts)
	var axes []axis
	nums := func(s []float64, set func(*Facts, float64)) axis {
		a := axis{}
		for _, v := range s {
			v := v
			a = append(a, func(f *Facts) { set(f, v) })
		}
		return a
	}
	strs := func(s []string, set func(*Facts, string)) axis {
		a := axis{}
		for _, v := range s {
			v := v
			a = append(a, func(f *Facts) { set(f, v) })
		}
		return a
	}
	bools := func(set func(*Facts, bool)) axis {
		return axis{func(f *Facts) { set(f, false) }, func(f *Facts) { set(f, true) }}
	}
	for _, k := range kindOrder {
		if !used[k] {
			continue
		}
		switch k {
		case "days":
			axes = append(axes, nums(realSamples(ends[k], false), func(f *Facts, v float64) { f.Roll = v }))
		case "amount":
			axes = append(axes, nums(realSamples(ends[k], true), func(f *Facts, v float64) { f.Amt = v }))
		case "pct":
			axes = append(axes, nums(realSamples(ends[k], true), func(f *Facts, v float64) { f.Pct = v }))
		case "cnt":
			axes = append(axes, nums(intSamples(cntMins, cntMaxs), func(f *Facts, v float64) { f.Cnt = v }))
			axes = append(axes, strs(texts("rtype"), func(f *Facts, v string) { f.RType = v }))
		case "grades":
			axes = append(axes, strs(texts(k), func(f *Facts, v string) { f.Grade = v }))
		case "tkind":
			axes = append(axes, strs(texts(k), func(f *Facts, v string) { f.TKind = v }))
		case "ctype":
			axes = append(axes, strs(texts(k), func(f *Facts, v string) { f.CType = v }))
		case "ckind":
			axes = append(axes, strs(texts(k), func(f *Facts, v string) { f.CKind = v }))
		case "skind":
			axes = append(axes, strs(texts(k, ""), func(f *Facts, v string) { f.SKind = v }))
		case "budget":
			axes = append(axes, bools(func(f *Facts, v bool) { f.AboveBudget = v }))
		case "bandx":
			axes = append(axes, bools(func(f *Facts, v bool) { f.BandX = v }))
		case "waive":
			axes = append(axes, bools(func(f *Facts, v bool) { f.Waive = v }))
		}
	}
	total := 1
	for _, a := range axes {
		total *= len(a)
		if total > 2_000_000 {
			return nil, fmt.Errorf("too many combinations of conditions to convert")
		}
	}
	idx := make([]int, len(axes))
	grid := m // the route wanted is the stored grid's own, so the tidying above is checked too
	for n := 0; n < total; n++ {
		f := Facts{WF: cv.wf}
		for i, a := range axes {
			a[idx[i]](&f)
		}
		for i := len(idx) - 1; i >= 0; i-- { // next combination
			idx[i]++
			if idx[i] < len(axes[i]) {
				break
			}
			idx[i] = 0
		}
		mask := make([]byte, len(cv.rows))
		for i, r := range cv.rows {
			mask[i] = '0'
			if evalRow(r, f) {
				mask[i] = '1'
			}
		}
		c := buildMatrix(grid, f, ident)
		cv.cells = append(cv.cells, f)
		cv.want = append(cv.want, routeKey(c.Chain, c.Notify))
		cv.masks = append(cv.masks, string(mask))
	}
	return cv, nil
}

// routeOf is the grid's route for a set of matching rows: approvers in column order, the people told (not
// approving) in column order, and everyone with an N cell.
func (cv *converter) routeOf(mask string) (approvers, notify, nAll []string) {
	A, N := map[string]bool{}, map[string]bool{}
	for i, r := range cv.rows {
		if mask[i] != '1' {
			continue
		}
		for role, v := range r.Cells {
			if v == "A" {
				A[role] = true
			} else if v == "N" {
				N[role] = true
			}
		}
	}
	approvers, notify, nAll = []string{}, []string{}, []string{}
	for _, c := range cv.cols {
		if A[c] {
			approvers = append(approvers, c)
		}
		if N[c] {
			nAll = append(nAll, c)
			if !A[c] {
				notify = append(notify, c)
			}
		}
	}
	return
}

// ok says whether the rules give every cell the grid's route.
func (cv *converter) ok(rs []Rule) bool {
	set := RuleSet{Rules: rs}
	for i, f := range cv.cells {
		k := Pick(set, f)
		if k < 0 {
			return false
		}
		chain, told := route(rs[k].Approvers, rs[k].Notify, f, ident)
		if routeKey(chain, told) != cv.want[i] {
			return false
		}
	}
	return true
}

// disjoint says whether no cell fits both rules.
func (cv *converter) disjoint(a, b Rule) bool {
	for _, f := range cv.cells {
		if a.Fits(f) && b.Fits(f) {
			return false
		}
	}
	return true
}

func without(rs []Rule, i int) []Rule {
	out := make([]Rule, 0, len(rs)-1)
	out = append(out, rs[:i]...)
	return append(out, rs[i+1:]...)
}

func sameRoute(a, b Rule) bool {
	return strings.Join(a.Approvers, "\x1f") == strings.Join(b.Approvers, "\x1f") && strings.Join(a.Notify, "\x1f") == strings.Join(b.Notify, "\x1f")
}

// mergeWhen joins two rules' conditions that differ in one condition of the same kind into one wider condition.
func mergeWhen(a, b []Condition) ([]Condition, bool) {
	if len(a) != len(b) {
		return nil, false
	}
	diff := -1
	for i := range a {
		if a[i].K != b[i].K {
			return nil, false
		}
		if condKey(a[i]) != condKey(b[i]) {
			if diff >= 0 {
				return nil, false
			}
			diff = i
		}
	}
	if diff < 0 {
		return nil, false
	}
	x, y := a[diff], b[diff]
	m := cleanCond(x)
	widen := func() {
		if x.Min == nil || y.Min == nil { // no start means from 0 (a negative start is lower still)
			l := math.Min(lo(x), lo(y))
			m.Min = nil
			if lo(x) > 0 || lo(y) > 0 || l < 0 {
				m.Min = fp(l)
			}
		} else {
			m.Min = fp(math.Min(*x.Min, *y.Min))
		}
		if x.Max == nil || y.Max == nil {
			m.Max = nil
		} else {
			m.Max = fp(math.Max(*x.Max, *y.Max))
		}
	}
	switch x.K {
	case "days", "amount", "pct":
		widen()
	case "cnt":
		sameRange := condKey(Condition{K: "cnt", Min: x.Min, Max: x.Max}) == condKey(Condition{K: "cnt", Min: y.Min, Max: y.Max})
		switch {
		case x.RT == y.RT:
			widen()
		case sameRange:
			m.RT = "Any"
		default:
			return nil, false
		}
	case "grades":
		for _, g := range y.List {
			if !contains(m.List, g) {
				m.List = append(m.List, g)
			}
		}
		sort.SliceStable(m.List, func(i, j int) bool { return natLess(m.List[i], m.List[j]) })
	default:
		return nil, false
	}
	out := append([]Condition{}, a...)
	out[diff] = m
	return out, true
}

// natLess orders "G2" before "G10".
func natLess(a, b string) bool {
	split := func(s string) (string, int, bool) {
		i := len(s)
		for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
			i--
		}
		if i == len(s) {
			return s, 0, false
		}
		n, _ := strconv.Atoi(s[i:])
		return s[:i], n, true
	}
	pa, na, oka := split(a)
	pb, nb, okb := split(b)
	if oka && okb && pa == pb {
		return na < nb
	}
	return a < b
}

// compress makes the rule list small, keeping every cell's route.
func (cv *converter) compress(rs []Rule) []Rule {
	for changed := true; changed; {
		changed = false
		// A rule that changes nothing.
		for i := 0; i < len(rs)-1; i++ {
			if c := without(rs, i); cv.ok(c) {
				rs, changed = c, true
				i--
			}
		}
		// A rule whose route Otherwise can take (when no request falls through to Otherwise).
		for i := len(rs) - 2; i >= 0; i-- {
			c := without(rs, i)
			c[len(c)-1].Approvers, c[len(c)-1].Notify = rs[i].Approvers, rs[i].Notify
			if cv.ok(c) {
				rs, changed = c, true
			}
		}
		// Two rules with the same route that differ in one condition.
	merge:
		for i := 0; i < len(rs)-1; i++ {
			for j := i + 1; j < len(rs)-1; j++ {
				if !sameRoute(rs[i], rs[j]) {
					continue
				}
				m, ok := mergeWhen(rs[i].When, rs[j].When)
				if !ok {
					continue
				}
				for _, at := range [][2]int{{i, j}, {j, i}} {
					c := append([]Rule{}, rs...)
					c[at[0]].When = m
					c = without(c, at[1])
					if cv.ok(c) {
						rs, changed = c, true
						break merge
					}
				}
			}
		}
		// A condition that changes nothing (a rule keeps at least one).
		for i := 0; i < len(rs)-1; i++ {
			for k := 0; k < len(rs[i].When) && len(rs[i].When) > 1; k++ {
				c := append([]Rule{}, rs...)
				w := append([]Condition{}, rs[i].When[:k]...)
				c[i].When = append(w, rs[i].When[k+1:]...)
				if cv.ok(c) {
					rs, changed = c, true
					k--
				}
			}
		}
	}
	return rs
}

func indexOr(l []string, s string, d int) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return d
}

// readOrder is the order rules are shown in when their order does not matter: by exit type, change, claim type,
// transfer, grade, then the numbers from low to high.
func readOrder(grades []string) func(a, b Rule) bool {
	vec := func(r Rule) []float64 {
		v := []float64{}
		for _, k := range kindOrder {
			var c *Condition
			for i := range r.When {
				if r.When[i].K == k {
					c = &r.When[i]
				}
			}
			if c == nil {
				v = append(v, 1, 0)
				continue
			}
			x := 0.0
			switch k {
			case "skind":
				x = float64(indexOr(ExitTypes, c.Val, 99))
			case "ckind":
				x = float64(indexOr(ChangeKinds, c.Val, 99))
			case "tkind":
				x = float64(indexOr(TransferKinds, c.Val, 99))
			case "grades":
				x = 999
				for _, g := range c.List {
					x = math.Min(x, float64(indexOr(grades, g, 999)))
				}
			case "cnt":
				x = float64(indexOr(PunchTypes, c.RT, 9))*1e9 + lo(*c)
			case "days", "amount", "pct":
				x = lo(*c)
			}
			v = append(v, 0, x)
		}
		return v
	}
	return func(a, b Rule) bool {
		va, vb := vec(a), vec(b)
		for i := range va {
			if va[i] != vb[i] {
				return va[i] < vb[i]
			}
		}
		return false
	}
}

// Convert turns a stored grid into a rule list that routes every request exactly as the grid did (see the top of
// this file). key is the grid's workflow key (leaveC and leaveP for leave). Rules that match one grid row exactly
// keep that row's label; the others are named from their conditions.
func Convert(key string, m Matrix) (RuleSet, error) {
	cv, err := newConverter(key, m)
	if err != nil {
		return RuleSet{}, err
	}
	// Otherwise: the rows with no condition (they match every request).
	base := make([]byte, len(cv.rows))
	for i := range cv.rows {
		base[i] = '0'
		if len(cv.conds[i]) == 0 {
			base[i] = '1'
		}
	}
	type set struct {
		mask         string
		a, n, nAll   []string
		size         int
		conditions   []Condition
		conditionsOK bool
	}
	seen := map[string]bool{string(base): true}
	var sets []set
	for _, mask := range cv.masks {
		if seen[mask] {
			continue
		}
		seen[mask] = true
		s := set{mask: mask, size: strings.Count(mask, "1")}
		s.a, s.n, s.nAll = cv.routeOf(mask)
		var all []Condition
		for i := range cv.rows {
			if mask[i] == '1' {
				all = append(all, cv.conds[i]...)
			}
		}
		s.conditions, s.conditionsOK = conjoin(all)
		if !s.conditionsOK || len(s.conditions) == 0 {
			return RuleSet{}, fmt.Errorf("rows %s cannot be combined", mask)
		}
		sets = append(sets, s)
	}
	// Larger route first, then more rows: a set always comes before the sets inside it.
	sort.SliceStable(sets, func(i, j int) bool {
		x, y := sets[i], sets[j]
		if len(x.a) != len(y.a) {
			return len(x.a) > len(y.a)
		}
		if len(x.nAll) != len(y.nAll) {
			return len(x.nAll) > len(y.nAll)
		}
		if x.size != y.size {
			return x.size > y.size
		}
		return x.mask > y.mask
	})
	rs := []Rule{}
	for _, s := range sets {
		rs = append(rs, Rule{On: true, When: s.conditions, Approvers: s.a, Notify: s.n})
	}
	a, n, _ := cv.routeOf(string(base))
	rs = append(rs, Rule{On: true, When: []Condition{}, Approvers: a, Notify: n})
	if !cv.ok(rs) {
		return RuleSet{}, fmt.Errorf("the rules do not route as the grid")
	}
	rs = cv.compress(rs)
	// Readable order: swap neighbours that can never both fit.
	var grades []string
	for _, f := range cv.cells {
		if f.Grade != "" && !contains(grades, f.Grade) {
			grades = append(grades, f.Grade)
		}
	}
	sort.SliceStable(grades, func(i, j int) bool { return natLess(grades[i], grades[j]) })
	less := readOrder(grades)
	for swapped := true; swapped; {
		swapped = false
		for i := 0; i+2 < len(rs); i++ {
			if less(rs[i+1], rs[i]) && cv.disjoint(rs[i], rs[i+1]) {
				rs[i], rs[i+1] = rs[i+1], rs[i]
				swapped = true
			}
		}
	}
	if !cv.ok(rs) {
		return RuleSet{}, fmt.Errorf("the rules do not route as the grid")
	}
	// Names: a grid row's own label when the rule is exactly that row.
	labels := map[string]string{}
	for i, r := range cv.rows {
		if len(cv.conds[i]) == 0 || strings.TrimSpace(r.Label) == "" {
			continue
		}
		if w, ok := conjoin(cv.conds[i]); ok {
			if _, has := labels[whenKey(w)]; !has {
				labels[whenKey(w)] = strings.TrimSpace(r.Label)
			}
		}
	}
	for i := range rs {
		rs[i].ID = fmt.Sprintf("r%d", i+1)
		switch {
		case i == len(rs)-1:
			rs[i].ID, rs[i].Label = "otherwise", OtherwiseLabel
		case labels[whenKey(rs[i].When)] != "":
			rs[i].Label = labels[whenKey(rs[i].When)]
		default:
			rs[i].Label = RuleLabel(rs[i].When)
		}
	}
	return RuleSet{Rules: rs}, nil
}

// MustConvert is Convert for grids known to convert (the seeded ones).
func MustConvert(key string, m Matrix) RuleSet {
	rs, err := Convert(key, m)
	if err != nil {
		panic(fmt.Sprintf("approval: converting %s: %v", key, err))
	}
	return rs
}

// ---------- Labels ----------

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// rupees writes an amount the Indian way: ₹1,00,000.
func rupees(v float64) string {
	s := num(math.Abs(v))
	frac := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s, frac = s[:i], s[i:]
	}
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	return "₹" + s + frac
}

// span names a range of n-formatted numbers followed by a unit (one or many): "2 to 5 days", "up to 1 day",
// "6 or more days", "3 days"; anyText when the range has no ends.
func span(c Condition, n func(float64) string, one, many, anyText string) string {
	l := lo(c)
	unit := func(v float64) string {
		if many == "" {
			return ""
		}
		if v == 1 {
			return " " + one
		}
		return " " + many
	}
	switch {
	case c.Max == nil && l <= 0:
		return anyText
	case c.Max == nil:
		return n(l) + " or more" + unit(2)
	case l <= 0:
		return "up to " + n(*c.Max) + unit(*c.Max)
	case l == *c.Max:
		return n(l) + unit(l)
	}
	return n(l) + " to " + n(*c.Max) + unit(2)
}

// gradeList writes grades as runs: "G1–G3, G5".
func gradeList(l []string) string {
	g := append([]string{}, l...)
	sort.SliceStable(g, func(i, j int) bool { return natLess(g[i], g[j]) })
	next := func(a, b string) bool { // b follows a: G3 then G4
		i, j := len(a), len(b)
		for i > 0 && a[i-1] >= '0' && a[i-1] <= '9' {
			i--
		}
		for j > 0 && b[j-1] >= '0' && b[j-1] <= '9' {
			j--
		}
		if i == len(a) || j == len(b) || a[:i] != b[:j] {
			return false
		}
		x, _ := strconv.Atoi(a[i:])
		y, _ := strconv.Atoi(b[j:])
		return y == x+1
	}
	var parts []string
	for i := 0; i < len(g); {
		j := i
		for j+1 < len(g) && next(g[j], g[j+1]) {
			j++
		}
		if j > i {
			parts = append(parts, g[i]+"–"+g[j])
		} else {
			parts = append(parts, g[i])
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// CondLabel describes one condition in plain words.
func CondLabel(c Condition) string {
	switch c.K {
	case "always":
		return "every request"
	case "skind":
		return c.Val
	case "ckind":
		switch c.Val {
		case "Bank":
			return "bank account change"
		case "Personal":
			return "personal details change"
		}
		return "change of " + c.Val
	case "ctype":
		return "claim type " + c.Val
	case "tkind":
		return strings.ToLower(c.Val) + " transfer"
	case "grades":
		return "grade " + gradeList(c.List)
	case "cnt":
		noun := map[string]string{"No Punch": "no-punch ", "All Other": "all-other "}[c.RT]
		return span(c, num, noun+"request this month", noun+"requests this month", "any number of "+noun+"requests this month")
	case "days":
		return span(c, num, "day", "days", "any number of days") + " in 30 days"
	case "amount":
		return span(c, rupees, "", "", "any amount")
	case "pct":
		return "pay change " + span(c, func(v float64) string { return num(v) + "%" }, "", "", "of any size")
	case "budget":
		return "above budget"
	case "bandx":
		return "outside the grade pay band"
	case "waive":
		return "notice waiver requested"
	}
	return c.K
}

// RuleLabel names a rule from its conditions: "Grade G3–G4, 6 to 10 days in 30 days".
func RuleLabel(when []Condition) string {
	if len(when) == 0 {
		return OtherwiseLabel
	}
	w := append([]Condition{}, when...)
	sort.SliceStable(w, func(i, j int) bool { return kindRank(w[i].K) < kindRank(w[j].K) })
	parts := make([]string, len(w))
	for i, c := range w {
		parts[i] = CondLabel(c)
	}
	s := strings.Join(parts, ", ")
	if s != "" && s[0] >= 'a' && s[0] <= 'z' {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}
