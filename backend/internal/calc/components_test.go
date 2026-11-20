package calc

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// TestLegacyComponentsSameAsClassic: the converted component template gives exactly (bit for bit) the result of the
// classic calculation, for many grosses, rates, types, months, variable overrides and proration, both converted on
// the fly and stored (JSON round trip, as migration 014 writes it).
func TestLegacyComponentsSameAsClassic(t *testing.T) {
	rng := rand.New(rand.NewSource(14))
	lwf := LwfSeed["Haryana"]
	types := []EmploymentType{Permanent, Contractual, Retainer}
	n := 0
	for i := 0; i < 20000; i++ {
		S := DefaultStructure
		S.BasicPct = []float64{50, 40, 45.5, 60, 33.33, 80, 100}[rng.Intn(7)]
		S.HraPct = []float64{50, 40, 0, 60, 37.5, 100}[rng.Intn(6)]
		S.VarPct = []float64{0, 10, 12.5, 20, 33.3}[rng.Intn(5)]
		S.VarCycle = []int{1, 3, 6, 12}[rng.Intn(4)]
		S.PfWage = []string{"basic", "basicSa"}[rng.Intn(2)]
		S.PfCap = rng.Intn(2) == 0
		full := math.Round(rng.Float64()*120000*100) / 100
		if rng.Intn(5) == 0 {
			full = float64(5000 + rng.Intn(30000))
		}
		earned := full
		if rng.Intn(2) == 0 {
			earned = full * float64(rng.Intn(31)) / 31
		}
		in := Input{Type: types[rng.Intn(3)], Gross: earned, EligGross: &full, Month: rng.Intn(13), JoinMonth: rng.Intn(13), Lwf: &lwf,
			Meal: float64(rng.Intn(3) * 500), EsiKeep: rng.Intn(4) == 0, PfExempt: rng.Intn(6) == 0, Inc: float64(rng.Intn(2) * 1000)}
		if rng.Intn(3) == 0 {
			v := []float64{0, 5, 15, 25}[rng.Intn(4)]
			in.VarPct = &v
		}
		if rng.Intn(3) == 0 {
			c := []int{0, 1, 3, 6, 12}[rng.Intn(5)]
			in.VarCycle = &c
		}
		if rng.Intn(4) == 0 {
			tot := float64(rng.Intn(20000))
			in.VarTotal = &tot
		}
		classic := in
		classic.Classic = true
		classic.Structure = &S
		want := Calc(classic)

		onTheFly := in
		onTheFly.Structure = &S
		if got := Calc(onTheFly); !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d converted on the fly differs:\n got %+v\nwant %+v", i, got, want)
		}
		stored := S
		stored.Comps = LegacyComponents(S)
		b, _ := json.Marshal(stored)
		var back Structure
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		withComps := in
		withComps.Structure = &back
		if got := Calc(withComps); !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d stored template differs:\n got %+v\nwant %+v", i, got, want)
		}
		n++
	}
	// The spec's golden values still hold through the components (TestGoldenValues runs the live path too).
	if n == 0 {
		t.Fatal("no cases")
	}
}

// The plan's example: CTC ₹35,000 = fixed ₹20,000 (Basic 50% of fixed, HRA 40% of Basic, the rest special
// allowance) + variable ₹15,000, paid as the amount HR enters.
func designedExample() Structure {
	S := DefaultStructure
	S.Designed, S.VarPct, S.VarCycle = true, 0, 1
	S.Comps = []Component{
		{Code: "basic", Name: "Basic", Kind: CompFixed, Side: SideEarn, Role: RoleBasic, Method: MPctGross, Value: 50, Taxable: true, PF: true, ESI: true, Bonus: true, Payslip: true},
		{Code: "hra", Name: "HRA", Kind: CompFixed, Side: SideEarn, Role: RoleHRA, Method: MPctBasic, Value: 40, Taxable: true, ESI: true, Payslip: true},
		{Code: "conv", Name: "Conveyance", Kind: CompFixed, Side: SideEarn, Method: MAmount, Value: 1600, Max: 1600, ESI: true, Payslip: true},
		{Code: "sa", Name: "Special allowance", Kind: CompFixed, Side: SideEarn, Method: MRemainder, Taxable: true, ESI: true, Payslip: true},
		{Code: "canteen", Name: "Canteen", Kind: CompFixed, Side: SideDed, Method: MAmount, Value: 300, Payslip: true},
		{Code: "perf", Name: "Performance pay", Kind: CompVariable, Side: SideEarn, Method: MRemainder, Cycle: 3, Taxable: true, Payslip: true},
	}
	return S
}

func TestDesignedStructure(t *testing.T) {
	S := designedExample()
	vt := 15000.0
	r := Calc(Input{Type: Permanent, Gross: 20000, Structure: &S, VarTarget: &vt, Month: 6})
	if r.Basic != 10000 || r.Hra != 4000 || r.Sa != 6000 || r.CompDed != 300 {
		t.Fatalf("basic %v hra %v others %v ded %v", r.Basic, r.Hra, r.Sa, r.CompDed)
	}
	if r.Variable != 15000 || r.VarPay != 0 || r.VarCycle != 1 {
		t.Fatalf("variable %v paid %v cycle %v", r.Variable, r.VarPay, r.VarCycle)
	}
	if r.NonTax != 1600 {
		t.Fatalf("non-taxable %v", r.NonTax)
	}
	// ESI wage leaves out nothing here (every fixed earning is ESI wage); PF on Basic.
	if r.PfBase != 10000 || r.EePF != 1200 {
		t.Fatalf("pf base %v ee %v", r.PfBase, r.EePF)
	}
	// Entered payout: paid in the month, in total earnings.
	r2 := Calc(Input{Type: Permanent, Gross: 20000, Structure: &S, VarTarget: &vt, Month: 6, VarPaid: map[string]float64{"perf": 40000}})
	if r2.VarPay != 40000 || r2.TotalEarn != r.TotalEarn+40000 {
		t.Fatalf("payout %v total %v", r2.VarPay, r2.TotalEarn)
	}
	// Prorated: half the days paid halves every fixed earning and the accrual; the fixed deduction stays.
	full := 20000.0
	h := Calc(Input{Type: Permanent, Gross: 10000, EligGross: &full, Structure: &S, VarTarget: &vt})
	if h.Basic != 5000 || h.Sa != 3000 || h.Variable != 7500 || h.CompDed != 300 {
		t.Fatalf("prorated basic %v others %v variable %v ded %v", h.Basic, h.Sa, h.Variable, h.CompDed)
	}
	if pct, ok := WageShare(r); ok || pct != 28.57 {
		t.Fatalf("wage share %v %v", pct, ok)
	}
	// Contractual: no component deductions (deductions none), no variable when the type has none.
	c := Calc(Input{Type: Retainer, Gross: 20000, Structure: &S, VarTarget: &vt, VarPaid: map[string]float64{"perf": 100}})
	if c.CompDed != 0 || c.VarPay != 0 || c.Variable != 0 {
		t.Fatalf("retainer %v %v %v", c.CompDed, c.VarPay, c.Variable)
	}
}

func TestVarDueAndCaps(t *testing.T) {
	// Paid when a cycle is complete, counted from the joining month (owner, 18 Nov 2026): joined May 2026.
	due := func(cycle int) []string {
		var out []string
		for k := 0; k < 24; k++ {
			ym := fmt.Sprintf("%04d-%02d", 2026+(4+k)/12, (4+k)%12+1) // May 2026 to April 2028
			if VarDue(ym, "2026-05", cycle) {
				out = append(out, ym)
			}
		}
		return out
	}
	for cycle, want := range map[int]string{
		3:  "2026-07 2026-10 2027-01 2027-04 2027-07 2027-10 2028-01 2028-04",
		6:  "2026-10 2027-04 2027-10 2028-04",
		12: "2027-04 2028-04",
	} {
		if got := strings.Join(due(cycle), " "); got != want {
			t.Fatalf("cycle %d from May: %s, want %s", cycle, got, want)
		}
	}
	if len(due(1)) != 24 || VarDue("2026-04", "2026-05", 1) || VarDue("2026-04", "2026-05", 12) {
		t.Fatal("monthly: every month from joining, never before")
	}
	S := DefaultStructure
	S.Designed = true
	S.Comps = []Component{
		{Code: "basic", Name: "Basic", Kind: CompFixed, Side: SideEarn, Role: RoleBasic, Method: MPctCTC, Value: 40, Min: 12000, PF: true, ESI: true, Taxable: true},
		{Code: "sa", Name: "Special", Kind: CompFixed, Side: SideEarn, Method: MRemainder, ESI: true, Taxable: true},
		{Code: "a", Name: "Target A", Kind: CompVariable, Side: SideEarn, Method: MAmount, Value: 1000, Cycle: 12},
		{Code: "b", Name: "Target B", Kind: CompVariable, Side: SideEarn, Method: MAmount, Value: 3000, Cycle: 12},
	}
	r := Calc(Input{Type: Permanent, Gross: 25000, Structure: &S, CTC: 25000})
	if r.Basic != 12000 || r.Sa != 13000 {
		t.Fatalf("min cap: basic %v special %v", r.Basic, r.Sa)
	}
	// A total target without a balancing component is shared in proportion; a component's own target stays.
	tot := 8000.0
	r = Calc(Input{Type: Permanent, Gross: 25000, Structure: &S, VarTarget: &tot})
	if r.Variable != 8000 || r.Comps[2].Target != 2000 || r.Comps[3].Target != 6000 {
		t.Fatalf("shared target %v %+v", r.Variable, r.Comps)
	}
	r = Calc(Input{Type: Permanent, Gross: 25000, Structure: &S, VarTarget: &tot, VarTargets: map[string]float64{"a": 500}})
	if r.Comps[2].Target != 500 || r.Comps[3].Target != 7500 {
		t.Fatalf("own target %+v", r.Comps)
	}
	// A mid-month change blends component amounts by days.
	b := Blend([]Result{r, r}, []float64{0.25, 0.75})
	if len(b.Comps) != 4 || b.Comps[0].Amount != r.Comps[0].Amount {
		t.Fatalf("blend %+v", b.Comps)
	}
}

// A converted structure (migration 017): a variable target in % of the fixed gross or in ₹ gives the monthly target,
// the payout suggestion (target × the cycle's months) and nothing paid until HR enters it; fixed pay is unchanged.
func TestVarTargetPctAndAmount(t *testing.T) {
	S := DefaultStructure
	S.VarCycle = 3
	before := Calc(Input{Type: Permanent, Gross: 30000, Structure: &S})
	C := S
	C.Comps, C.Designed, C.Converted = TargetComponents(S), true, true
	pct, amt := 11.11, 5000.0
	for _, tc := range []struct {
		name string
		in   Input
		want float64
	}{
		{"pct", Input{VarTargetPct: &pct}, 3333},
		{"amount", Input{VarTarget: &amt}, 5000},
	} {
		in := tc.in
		in.Type, in.Gross, in.Structure = Permanent, 30000, &C
		r := Calc(in)
		var line *CompLine
		for i := range r.Comps {
			if r.Comps[i].Kind == CompVariable {
				line = &r.Comps[i]
			}
		}
		if line == nil || Round2(line.Target) != tc.want || line.Cycle != 3 || Round2(line.Target*float64(line.Cycle)) != Round2(tc.want*3) {
			t.Fatalf("%s: variable line %+v, want target %v on a 3-month cycle", tc.name, line, tc.want)
		}
		if r.VarPay != 0 || Round2(r.Variable) != tc.want {
			t.Fatalf("%s: paid %v (want 0 until entered), accrued %v", tc.name, r.VarPay, r.Variable)
		}
		if r.Basic != before.Basic || r.Hra != before.Hra || r.Sa != before.Sa || r.EePF != before.EePF || r.EeESI != before.EeESI || r.Net != before.Net {
			t.Fatalf("%s: fixed pay changed: %+v vs %+v", tc.name, r, before)
		}
		in.VarPaid = map[string]float64{"var": 9000}
		if r := Calc(in); r.VarPay != 9000 {
			t.Fatalf("%s: entered 9000, paid %v", tc.name, r.VarPay)
		}
	}
}

// A retainer shows the retainer value only: one fee line, no Basic, HRA or Special allowance and no fixed component
// lines, with every amount and the net exactly as before (TDS under 194J on the gross).
func TestRetainerFeeOnly(t *testing.T) {
	S := DefaultStructure
	S.Comps = TargetComponents(S)
	S.Designed = true
	for _, st := range []*Structure{nil, &S} {
		in := Input{Type: Retainer, Gross: 30000, Structure: st}
		r := Calc(in)
		in.Type = Contractual // the same arithmetic without the retainer display
		rules := RulesFor(Retainer)
		in.Rules = &rules
		was := Calc(in)
		if r.Fee != 30000 || r.Basic != 0 || r.Hra != 0 || r.Sa != 0 || r.EePF != 0 || r.EeESI != 0 {
			t.Fatalf("retainer: fee %v basic %v hra %v sa %v pf %v esi %v", r.Fee, r.Basic, r.Hra, r.Sa, r.EePF, r.EeESI)
		}
		for _, l := range r.Comps {
			if l.Kind == CompFixed && l.Side == SideEarn {
				t.Fatalf("fixed component line %s on a retainer", l.Name)
			}
		}
		if r.Net != was.Net || r.Tds != was.Tds || r.TotalEarn != was.TotalEarn || r.CTC != was.CTC || was.Basic == 0 {
			t.Fatalf("net %v → %v, tds %v → %v, ctc %v → %v", was.Net, r.Net, was.Tds, r.Tds, was.CTC, r.CTC)
		}
	}
}

// TestAllBasicOptOut (owner, 19 Nov 2026): a PF exempt person who chose no PF is paid the whole fixed gross as Basic,
// HRA and special allowance 0, no employee or employer PF; without the flag the normal structure is untouched.
func TestAllBasicOptOut(t *testing.T) {
	S := designedExample()
	vt := 15000.0
	for _, st := range []*Structure{nil, &S} {
		for _, classic := range []bool{false, true} {
			if classic && st != nil {
				continue
			}
			in := Input{Type: Permanent, Gross: 30000, Structure: st, Month: 6, Classic: classic}
			if st != nil {
				in.VarTarget = &vt
			}
			normal := Calc(in)
			in.PfExempt, in.AllBasic = true, true
			r := Calc(in)
			if r.Basic != 30000 || r.Hra != 0 || r.Sa != 0 {
				t.Fatalf("structure %v classic %v: basic %v hra %v sa %v", st != nil, classic, r.Basic, r.Hra, r.Sa)
			}
			if r.EePF != 0 || r.ErPF != 0 || r.Eps != 0 || r.Edli != 0 || r.Admin != 0 || !r.PfExempt {
				t.Fatalf("PF not 0: %+v", r)
			}
			if r.Gross != normal.Gross || r.Variable != normal.Variable || r.EsiOn != normal.EsiOn || r.LwfEe != normal.LwfEe || r.CompDed != normal.CompDed {
				t.Fatalf("other rules changed: %+v vs %+v", r, normal)
			}
			if r.Net != normal.Net+normal.EePF-r.EePF {
				t.Fatalf("net %v, normal %v with PF %v", r.Net, normal.Net, normal.EePF)
			}
			// Opted in (no flag): exactly the normal structure.
			in.PfExempt, in.AllBasic = false, false
			if again := Calc(in); !reflect.DeepEqual(again, normal) {
				t.Fatalf("opt-in changed: %+v vs %+v", again, normal)
			}
			if st == nil && (normal.Basic != 15000 || normal.Hra != 7500 || normal.Sa != 7500 || normal.EePF == 0) {
				t.Fatalf("normal structure %+v", normal)
			}
		}
	}
	// Designed: one Basic line for the fixed pay, the deduction and variable lines kept.
	r := Calc(Input{Type: Permanent, Gross: 30000, Structure: &S, VarTarget: &vt, PfExempt: true, AllBasic: true})
	fixed := 0
	for _, l := range r.Comps {
		if l.Kind == CompFixed && l.Side == SideEarn {
			fixed++
			if l.Amount != 30000 {
				t.Fatalf("basic line %+v", l)
			}
		}
	}
	if fixed != 1 || r.NonTax != 0 {
		t.Fatalf("fixed lines %d non-taxable %v: %+v", fixed, r.NonTax, r.Comps)
	}
}

// Reimbursement (owner, 20 Nov 2026): a per-employee ₹ amount inside the fixed gross. ₹3,000 on a gross of ₹30,000 on
// Version 1 gives Basic 15,000, HRA 7,500, Reimbursement 3,000 and Special allowance 4,500; PF stays on Basic only, the
// gross is unchanged, and with no amount the result is bit for bit what it was.
func TestReimbursementInFixedPay(t *testing.T) {
	S := DefaultStructure
	S.Comps, S.Designed, S.Converted = TargetComponents(S), true, true
	old := DefaultStructure
	old.Comps, old.Designed, old.Converted = TargetComponents(old), true, true
	old.Comps = append(old.Comps[:2:2], old.Comps[3:]...) // the template before the Reimbursement was added
	for _, st := range []*Structure{nil, &S} {
		r := Calc(Input{Type: Permanent, Gross: 30000, Structure: st, EmpAmts: map[string]float64{ReimbCode: 3000}})
		if r.Gross != 30000 || r.Basic != 15000 || r.Hra != 7500 || r.Reimb != 3000 || r.Sa != 4500 {
			t.Fatalf("breakup gross %v basic %v hra %v reimb %v sa %v", r.Gross, r.Basic, r.Hra, r.Reimb, r.Sa)
		}
		if r.PfBase != 15000 || r.EePF != 1800 || r.ErPF != 1800 {
			t.Fatalf("PF wage %v, PF %v / %v: want Basic only", r.PfBase, r.EePF, r.ErPF)
		}
		if st != nil {
			found := false
			for _, l := range r.Comps {
				found = found || (l.Code == ReimbCode && l.Amount == 3000 && l.Taxable && l.Payslip)
			}
			if !found {
				t.Fatalf("no Reimbursement line: %+v", r.Comps)
			}
		}
	}
	// ESI wage leaves the Reimbursement out (a gross within the ESI ceiling).
	r := Calc(Input{Type: Permanent, Gross: 20000, Structure: &S, EmpAmts: map[string]float64{ReimbCode: 2000}})
	if r.EeESI != 135 { // 0.75% of 18,000
		t.Fatalf("ESI %v, want 135 on 18,000", r.EeESI)
	}
	for _, g := range []float64{12000, 30000, 87654.32} {
		a := Calc(Input{Type: Permanent, Gross: g, Structure: &S})
		b := Calc(Input{Type: Permanent, Gross: g, Structure: &old})
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("gross %v: no amount changed the result\n%+v\n%+v", g, a, b)
		}
	}
}
