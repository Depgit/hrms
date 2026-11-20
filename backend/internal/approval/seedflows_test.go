package approval

import (
	"math"
	"reflect"
	"testing"
)

// The seeded lists of the workflows decided by a number route as the sheet's grid wherever the number falls in one
// of the sheet's ranges. Where it falls between two ranges (half a day, ₹5,000.50), the grid asked nobody; the list
// routes it as the next range up. So no leave or amount is approved automatically for falling in a gap.
func TestSeedFlowsMeet(t *testing.T) {
	seeded, grids := SeedFlows(), SeedMatrices()
	compared, gaps := 0, 0
	for key, flows := range rangeFlows() {
		for flow := range flows {
			m, rs := grids[key][flow], seeded[key][flow]
			if _, err := Check(rs.Rules, func(string) bool { return true }); err != nil {
				t.Fatalf("%s/%s cannot be saved: %v", key, flow, err)
			}
			if err := CheckFits(key, rs.Rules); err != nil {
				t.Fatalf("%s/%s: %v", key, flow, err)
			}
			kind := "amount"
			if factWF(key) == "leave" {
				kind = "days"
			}
			value := func(f Facts) float64 {
				if kind == "amount" {
					return math.Abs(f.Amt)
				}
				if f.Roll != 0 {
					return math.Max(f.Roll, 0)
				}
				return math.Max(f.Days, 0)
			}
			for _, fc := range proofFacts(key, m) {
				for rn, res := range proofResolvers {
					want := fc
					v := value(fc)
					inRange := false
					next := math.Inf(1)
					for _, row := range m.Rows {
						if row.C.K != kind {
							continue
						}
						if inRangeOf(v, row.C) {
							inRange = true
						}
						if l := lo(row.C); l >= v && l < next {
							next = l
						}
					}
					if !inRange {
						gaps++
						if math.IsInf(next, 1) {
							t.Fatalf("%s/%s: %v is above every range", key, flow, v)
						}
						if kind == "amount" {
							want.Amt = next
						} else {
							want.Roll, want.Days = next, 0
						}
					}
					old, now := buildMatrix(m, want, res), Build(rs, fc, res)
					compared++
					if !reflect.DeepEqual(old.Chain, now.Chain) || !reflect.DeepEqual(old.Notify, now.Notify) {
						t.Errorf("%s/%s %s, facts %+v (in a range: %v): sheet %v %v, list %v %v", key, flow, rn, fc, inRange, old.Chain, old.Notify, now.Chain, now.Notify)
					}
					if len(now.Chain) == 0 {
						t.Errorf("%s/%s, facts %+v: nobody approves", key, flow, fc)
					}
				}
			}
		}
	}
	t.Logf("%d routes compared, %d of them in a gap of the sheet", compared, gaps)
	if gaps == 0 || compared < 1000 {
		t.Fatal("too few comparisons")
	}
}

func inRangeOf(v float64, c Condition) bool { return inRange(v, c.Min, c.Max) }

// A stored list is upgraded only while it is exactly the list seeded before.
func TestSeedFlowUpgrade(t *testing.T) {
	old := MustConvert("p_claim", SeedMatrices()["p_claim"][Operations])
	got, ok := SeedFlowUpgrade("p_claim", Operations, old.Rules)
	if !ok || !reflect.DeepEqual(got, SeedFlows()["p_claim"][Operations]) {
		t.Fatalf("untouched list: %v %+v", ok, got)
	}
	edited := append([]Rule{}, old.Rules...)
	edited[0].Approvers = []string{LM}
	if _, ok := SeedFlowUpgrade("p_claim", Operations, edited); ok {
		t.Fatal("an edited list was upgraded")
	}
	if _, ok := SeedFlowUpgrade("transfer", Operations, MustConvert("transfer", SeedMatrices()["transfer"][Operations]).Rules); ok {
		t.Fatal("a list without ranges was upgraded")
	}
}
