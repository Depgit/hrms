package api

import (
	"fmt"
	"strings"
	"testing"
)

// The owner's example (13 Nov 2026): uniform worth ₹5,000 given in September 2026, recovered in 2 instalments; the
// HRMS starts deducting in October 2026, so only ₹2,500 is deducted, from October salary, and the ₹5,000 refund
// follows the refund rule from October.
func TestOpeningUniformPlanOwnersCase(t *testing.T) {
	us := uniformSetting{FirstPct: 50, RefundAfter: 6, RefundBasis: refundByDeduction, ServiceMonths: 6}
	for _, n := range []int{2, 0} { // 2 instalments, or blank (Setup's 50% / 50%)
		p := openingUniformPlan(us, "2026-09", 5000, n, "2024-04-15", "2026-10")
		if fmt.Sprint(p.Before) != "[{2026-09 2500}]" || fmt.Sprint(p.After) != "[{2026-10 2500}]" || p.Refund != "2027-04" {
			t.Fatalf("n=%d: before %v after %v refund %s", n, p.Before, p.After, p.Refund)
		}
	}
	// After 12 months of employment instead: joined 10 March 2026, complete in March 2027.
	svc := us
	svc.RefundBasis, svc.ServiceMonths = refundByService, 12
	if p := openingUniformPlan(svc, "2026-09", 5000, 2, "2026-03-10", "2026-10"); p.Refund != "2027-03" {
		t.Fatalf("service basis: %s", p.Refund)
	}
	// Joined long ago: the month after the last deduction.
	if p := openingUniformPlan(svc, "2026-09", 5000, 2, "2020-01-01", "2026-10"); p.Refund != "2026-11" {
		t.Fatalf("service basis, long served: %s", p.Refund)
	}
	// Equal split to the paisa, the remainder in the last instalment.
	if p := openingUniformPlan(us, "2026-09", 1000, 3, "", "2026-10"); fmt.Sprint(p.Parts) != "[{2026-09 333.33} {2026-10 333.33} {2026-11 333.34}]" || p.Refund != "2027-05" {
		t.Fatalf("thirds: %v %s", p.Parts, p.Refund)
	}
	if p := openingUniformPlan(us, "2026-01", 100, 12, "", "2026-10"); len(p.Parts) != 12 || p.Parts[0].amt != 8.33 || p.Parts[11].amt != 8.37 || len(p.Before) != 9 {
		t.Fatalf("twelfths: %v", p.Parts)
	}
	// Recovered and refunded before the HRMS: the refund month is before the first HRMS month.
	if p := openingUniformPlan(us, "2025-06", 1200, 2, "", "2026-10"); p.Refund != "2026-01" || len(p.After) != 0 {
		t.Fatalf("settled before: %v", p)
	}
	// Setup's split with everything in the first instalment.
	full := us
	full.FirstPct = 100
	if p := openingUniformPlan(full, "2026-10", 900, 0, "", "2026-10"); fmt.Sprint(p.Parts) != "[{2026-10 900}]" || p.Refund != "2027-04" {
		t.Fatalf("100%%: %v", p)
	}
}

func TestOpeningUniformParse(t *testing.T) {
	for _, m := range []string{"2026-09", "Sep-2026", "sep-2026", "09/2026", "9/2026", "September 2026", "2026-09-01", "46266"} { // 46266: 1 Sep 2026 as an Excel day number
		in, er := openUniParse(map[string]string{"Issued month": m, "Uniform amount (₹)": "₹5,000", "Instalments": "2"}, "2026-11")
		if len(er) > 0 || in.ym != "2026-09" || in.amount != 5000 || in.instalments != 2 || !in.recover {
			t.Fatalf("%s: %v %v", m, in, er)
		}
	}
	for v, want := range map[string]string{"": "Give the issued month", "2026-12": "future", "2024-10": "more than 24 months back", "Sept 26": "must be a month"} {
		if _, er := openUniParse(map[string]string{"Issued month": v, "Uniform amount (₹)": "100"}, "2026-11"); !strings.Contains(fmt.Sprint(er), want) {
			t.Fatalf("%q: %v", v, er)
		}
	}
	if _, er := openUniParse(map[string]string{"Issued month": "2024-11", "Uniform amount (₹)": "100"}, "2026-11"); len(er) > 0 {
		t.Fatalf("24 months back is allowed: %v", er)
	}
	for _, a := range []string{"0", "100001", "abc", ""} {
		if _, er := openUniParse(map[string]string{"Issued month": "2026-09", "Uniform amount (₹)": a}, "2026-11"); !strings.Contains(fmt.Sprint(er), "1 to 1,00,000") {
			t.Fatalf("amount %q: %v", a, er)
		}
	}
	for _, n := range []string{"0", "13", "1.5", "x"} {
		if _, er := openUniParse(map[string]string{"Issued month": "2026-09", "Uniform amount (₹)": "100", "Instalments": n}, "2026-11"); !strings.Contains(fmt.Sprint(er), "1 to 12") {
			t.Fatalf("instalments %q: %v", n, er)
		}
	}
	if in, _ := openUniParse(map[string]string{"Issued month": "2026-09", "Uniform amount (₹)": "100", "Recover": "no"}, "2026-11"); in.recover {
		t.Fatal("Recover No")
	}
}

// An advance example: ₹30,000 given in June 2026, deducted ₹5,000 a month from July to December 2026; the HRMS starts
// in October 2026, so ₹15,000 was recovered before it and ₹15,000 is left for October to December.
func TestOpeningAdvancePlan(t *testing.T) {
	row := func(amount, given, start, monthly, closing string) map[string]string {
		return map[string]string{"Advance amount (₹)": amount, "Advance given month": given, "Deduction start month": start, "Monthly deduction (₹)": monthly,
			"Deduction closing month": closing}
	}
	p, er := openingAdvancePlan(row("30000", "2026-06", "Jul-2026", "5000", "12/2026"), "2026-10", "2026-11")
	if len(er) > 0 || p.N != 6 || p.Before != 3 || p.Recovered != 15000 || p.Balance != 15000 || p.HrmsStart != "2026-10" || p.HrmsN != 3 || p.Last != 5000 {
		t.Fatalf("example: %+v %v", p, er)
	}
	if got := partsText(p.instalments(p.Before, p.N)); got != "Oct 2026 ₹5,000 · Nov 2026 ₹5,000 · Dec 2026 ₹5,000" {
		t.Fatal(got)
	}
	// The last instalment takes the remainder: 12,000 at 5,000 is 5,000, 5,000 and 2,000.
	p, er = openingAdvancePlan(row("12000", "2026-07", "2026-08", "5000", "2026-10"), "2026-10", "2026-11")
	if len(er) > 0 || p.N != 3 || p.Recovered != 10000 || p.Balance != 2000 || p.Last != 2000 || p.HrmsN != 1 {
		t.Fatalf("remainder: %+v %v", p, er)
	}
	// Starting after the first HRMS month: nothing recovered yet.
	p, er = openingAdvancePlan(row("4000", "2026-10", "2026-11", "2000", "2026-12"), "2026-10", "2026-11")
	if len(er) > 0 || p.Before != 0 || p.Recovered != 0 || p.Balance != 4000 || p.HrmsStart != "2026-11" {
		t.Fatalf("later start: %+v %v", p, er)
	}
	for _, c := range []struct {
		r    map[string]string
		want string
	}{
		{row("30000", "2026-06", "2026-07", "5000", "2026-11"), "Deduction closing month should be December 2026: ₹30,000 at ₹5,000 a month is 6 instalments from July 2026. The file says November 2026"},
		{row("12000", "2026-07", "2026-08", "5000", "2026-09"), "should be October 2026: ₹12,000 at ₹5,000 a month is 3 instalments from August 2026, the last ₹2,000"},
		{row("6000", "2026-04", "2026-05", "2000", "2026-07"), "Nothing left to recover"},
		{row("6000", "2026-09", "2026-08", "2000", "2026-10"), "before the month the advance was given"},
		{row("6000", "2026-12", "2026-12", "2000", "2027-02"), "Advance given month is in the future"},
		{row("100000", "2026-09", "2026-10", "100", "2026-10"), "the most is 60"},
		{row("0", "2026-09", "2026-10", "100", "2026-10"), "Advance amount must be a number above 0"},
		{row("1000", "2026-09", "2026-10", "", "2026-10"), "Monthly deduction must be a number above 0"},
		{row("1000", "", "2026-10", "100", "2026-10"), "Give the advance given month"},
	} {
		if _, er := openingAdvancePlan(c.r, "2026-10", "2026-11"); !strings.Contains(fmt.Sprint(er), c.want) {
			t.Fatalf("%v: want %q, got %v", c.r, c.want, er)
		}
	}
}

func TestPartsTextRuns(t *testing.T) {
	parts := []uniPart{}
	for i := 0; i < 14; i++ {
		parts = append(parts, uniPart{addYm("2026-10", i), 1000})
	}
	parts[13].amt = 500
	if got := partsText(parts); got != "Oct 2026 to Oct 2027: 13 × ₹1,000 · Nov 2027 ₹500" {
		t.Fatal(got)
	}
}
