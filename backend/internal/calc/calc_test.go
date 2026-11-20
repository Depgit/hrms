package calc

import (
	"testing"
)

// Golden values from spec 22.3 (prototype calc, structure v1), with two statutory corrections: the member's PF
// shares are to the rupee (RUN-57: 1260.06 → 1260 at ₹21,001) and EDLI is on wages up to ₹15,000 (REAL-40: 125 → 75).
var cols = []string{"gross", "basic", "hra", "sa", "pfBase", "eePF", "erPF", "admin", "edli", "eeESI", "erESI", "med", "bonus", "lwfEe", "lwfEr", "tds", "grat", "plEnc", "ded", "net", "ctc"}

func row(r Result) []float64 {
	return []float64{r.Gross, r.Basic, r.Hra, r.Sa, r.PfBase, r.EePF, r.ErPF, r.Admin, r.Edli, r.EeESI, r.ErESI, r.Med, r.Bonus, r.LwfEe, r.LwfEr, r.Tds, r.Grat, r.PlEnc, r.Ded, r.Net, r.CTC}
}

var golden = []struct {
	label string
	typ   EmploymentType
	lwf   string
	rows  [][]float64
}{
	{"Gurugram", Permanent, "Haryana", [][]float64{
		{12000, 6000, 3000, 3000, 6000, 720, 720, 30, 30, 90, 390, 0, 499.8, 24, 48, 0, 288.6, 250, 834, 11665.8, 14256.4},
		{15000, 7500, 3750, 3750, 7500, 900, 900, 37.5, 37.5, 113, 488, 0, 583.1, 30, 60, 0, 360.75, 312.5, 1043, 14540.1, 17779.35},
		{20000, 10000, 5000, 5000, 10000, 1200, 1200, 50, 50, 150, 650, 0, 583.1, 34, 68, 0, 481, 416.67, 1384, 19199.1, 23498.77},
		{21000, 10500, 5250, 5250, 10500, 1260, 1260, 52.5, 52.5, 158, 683, 0, 583.1, 34, 68, 0, 505.05, 437.5, 1452, 20131.1, 24641.65},
		{21001, 10500.5, 5250.25, 5250.25, 10500.5, 1260, 1260, 52.5, 52.5, 0, 0, 500, 0, 34, 68, 0, 505.07, 437.52, 1294, 19707, 23876.6},
		{25000, 12500, 6250, 6250, 12500, 1500, 1500, 62.5, 62.5, 0, 0, 500, 0, 34, 68, 0, 601.25, 520.83, 1534, 23466, 28315.08},
		{30000, 15000, 7500, 7500, 15000, 1800, 1800, 75, 75, 0, 0, 500, 0, 34, 68, 0, 721.5, 625, 1834, 28166, 33864.5},
		{50000, 25000, 12500, 12500, 25000, 3000, 3000, 125, 75, 0, 0, 500, 0, 34, 68, 0, 1202.5, 1041.67, 3034, 46966, 56012.17},
		{60000, 30000, 15000, 15000, 25000, 3000, 3000, 125, 75, 0, 0, 500, 0, 34, 68, 0, 1443, 1250, 3034, 56966, 66461},
		{100000, 50000, 25000, 25000, 25000, 3000, 3000, 125, 75, 0, 0, 500, 0, 34, 68, 0, 2405, 2083.33, 3034, 96966, 108256.33},
	}},
	{"Delhi", Permanent, "Delhi", [][]float64{
		{20000, 10000, 5000, 5000, 10000, 1200, 1200, 50, 50, 150, 650, 0, 583.1, 0.13, 0.38, 0, 481, 416.67, 1350.13, 19232.97, 23431.14},
		{30000, 15000, 7500, 7500, 15000, 1800, 1800, 75, 75, 0, 0, 500, 0, 0.13, 0.38, 0, 721.5, 625, 1800.13, 28199.88, 33796.88},
	}},
	{"Mumbai", Permanent, "", [][]float64{
		{20000, 10000, 5000, 5000, 10000, 1200, 1200, 50, 50, 150, 650, 0, 583.1, 0, 0, 0, 481, 416.67, 1350, 19233.1, 23430.77},
		{30000, 15000, 7500, 7500, 15000, 1800, 1800, 75, 75, 0, 0, 500, 0, 0, 0, 0, 721.5, 625, 1800, 28200, 33796.5},
	}},
	{"Mumbai", Contractual, "", [][]float64{
		{20000, 10000, 5000, 5000, 10000, 0, 0, 0, 0, 0, 0, 0, 583.1, 0, 0, 0, 0, 0, 0, 20583.1, 20583.1},
		{30000, 15000, 7500, 7500, 15000, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 30000, 30000},
	}},
	// A retainer shows one Retainer fee (Result.Fee), no Basic, HRA or Special allowance (18 Nov 2026); the rest as before.
	{"Mumbai", Retainer, "", [][]float64{
		{20000, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2000, 0, 0, 2000, 18000, 20000},
		{30000, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3000, 0, 0, 3000, 27000, 30000},
	}},
}

func TestGoldenValues(t *testing.T) {
	for _, g := range golden {
		var lwf *LwfRule
		if g.lwf != "" {
			r := LwfSeed[g.lwf]
			lwf = &r
		}
		for _, exp := range g.rows {
			got := row(Calc(Input{Type: g.typ, Gross: exp[0], Lwf: lwf}))
			for i, c := range cols {
				if Round2(got[i]) != exp[i] {
					t.Errorf("%s %s gross %.0f: %s = %v, want %v", g.label, g.typ, exp[0], c, Round2(got[i]), exp[i])
				}
			}
		}
	}
}

func TestDelhiLwfMonths(t *testing.T) {
	r := LwfSeed["Delhi"]
	if v := Calc(Input{Type: Permanent, Gross: 30000, Lwf: &r, Month: 6}).LwfEe; v != 0.75 {
		t.Fatalf("June LWF = %v", v)
	}
	if v := Calc(Input{Type: Permanent, Gross: 30000, Lwf: &r, Month: 7}).LwfEe; v != 0 {
		t.Fatalf("July LWF = %v", v)
	}
}

func TestVariablePayout(t *testing.T) {
	pct, cyc, total := 10.0, 3, 7161.0
	base := Input{Type: Permanent, Gross: 30000, VarPct: &pct, VarCycle: &cyc, JoinMonth: 10}
	in := base
	in.Month = 11
	if v := Calc(in).VarPay; v != 0 {
		t.Fatalf("Nov payout %v", v)
	}
	in.Month, in.VarTotal = 12, &total
	if v := Calc(in).VarPay; v != 7161 {
		t.Fatalf("Dec payout %v", v)
	}
	in = base
	in.Month = 3
	if v := Calc(in).VarPay; v != 9000 {
		t.Fatalf("Mar payout %v", v)
	}
}

func TestPayDaysAndEsiPeriod(t *testing.T) {
	if PayDays("2026-02", "calendar") != 28 || PayDays("2026-10", "calendar") != 31 || PayDays("2026-10", "26") != 26 {
		t.Fatal("pay days")
	}
	if !EsiKeep("2026-12", "2024-01", func(ym string) float64 {
		if ym < "2026-12" {
			return 20000
		}
		return 23000
	}, 21000) {
		t.Fatal("ESI should continue to period end")
	}
	if EsiKeep("2026-11", "2026-11", func(string) float64 { return 23000 }, 21000) {
		t.Fatal("joiner above ceiling is never covered")
	}
	if r := Calc(Input{Type: Permanent, Gross: 23000, EsiKeep: true}); !r.EsiOn || r.EeESI != 173 {
		t.Fatalf("esi keep: %+v", r)
	}
}

func ip(v int) *int { return &v }

func TestDayStatus(t *testing.T) {
	nine, six := ip(540), ip(1080)
	cases := []struct {
		in, out string
		s, e    *int
		want    string
	}{
		{"09:10", "18:00", nine, six, Present},
		{"09:20", "18:00", nine, six, Late},
		{"09:00", "15:00", nine, six, HalfDay},
		{"09:00", "12:00", nine, six, AbsentShort},
		{"09:00", "", nine, six, MissingOut},
		{"22:00", "07:00", ip(1320), ip(360), Present}, // night shift crosses midnight
		{"13:00", "23:00", ip(840), ip(1320), Present}, // 60 min early is not late
	}
	for _, c := range cases {
		if got := DayStatus(DayInput{In: c.in, Out: c.out, ShiftStart: c.s, ShiftEnd: c.e}); got != c.want {
			t.Errorf("%s-%s: %s, want %s", c.in, c.out, got, c.want)
		}
	}
	if DayStatus(DayInput{Off: "WO"}) != WeekOff {
		t.Error("week off")
	}
}

func TestMonthLOP(t *testing.T) {
	days := []MonthDay{{Status: HalfDay, AgeDays: 20}, {Status: NoPunch, AgeDays: 20}, {Status: NoPunch, AgeDays: 2}, {Status: MissingOut, AgeDays: 10}}
	for i := 0; i < 5; i++ {
		days = append(days, MonthDay{Status: Late, AgeDays: 5})
	}
	// 0.5 half day + 1 old no punch + 0.5 missing + (5-3) late x 0.5
	if r := MonthLOP(days, DefaultAttPolicy, 31); r.LOP != 3 {
		t.Fatalf("lop = %v", r.LOP)
	}
}

func TestTax(t *testing.T) {
	if r := TaxEstimate(TaxInput{SalaryYear: 1200000, BasicYear: 600000, HraYear: 300000, EmployeePfYear: 21600}); r.NewTax != 0 {
		t.Fatalf("rebate: %v", r.NewTax)
	}
	if r := TaxEstimate(TaxInput{SalaryYear: 1285000}); r.NewTax != 10400 {
		t.Fatalf("marginal relief: %v", r.NewTax)
	}
	if MonthlyTds(60000, 30000, 0, 6) != 5000 {
		t.Fatal("monthly tds")
	}
}

func TestSettlement(t *testing.T) {
	if _, _, ok := Gratuity(15000, "2020-01-01", "2024-06-01", false, true); ok {
		t.Fatal("under 4.75 years")
	}
	if y, a, ok := Gratuity(15000, "2020-01-01", "2024-11-01", false, true); !ok || y != 5 || a != 43269 {
		t.Fatalf("gratuity %d %v", y, a)
	}
	if d, a := LeaveEncashment(15000, 7.8); d != 7.5 || a != 4327 {
		t.Fatalf("encash %v %v", d, a)
	}
	if FnfPayMonth("2026-11-23", "2026-10", "Last working day") != "2026-12" {
		t.Fatal("fnf month")
	}
	if v := Credited(LeaveCredit{Days: 12, Credit: "monthly", From: "doj"}, "2026-01-10", 6, "2026-01-01", "2026-03-15", true); v != 3 {
		t.Fatalf("CL credit %v", v)
	}
	if v := Credited(LeaveCredit{Days: 15, Credit: "monthly", From: "prob"}, "2026-01-10", 6, "2026-01-01", "2026-08-15", true); v != 2.5 {
		t.Fatalf("PL credit %v", v)
	}
}

func TestTaxHelpers(t *testing.T) {
	if FY("2026-10") != "2026-27" || FY("2027-03") != "2026-27" || FY("2027-04") != "2027-28" {
		t.Fatal("financial year")
	}
	if m := FYMonths("2026-27"); len(m) != 12 || m[0] != "2026-04" || m[11] != "2027-03" {
		t.Fatal(m)
	}
	if Quarter("2026-05") != 1 || Quarter("2026-12") != 3 || Quarter("2027-02") != 4 {
		t.Fatal("quarter")
	}
	g, rent := GroupDeductions(map[string]float64{"c_ppf": 100000, "c_elss": 80000, "d_self": 40000, "hra": 240000})
	if g["80C"] != 180000 || g["80D"] != 25000 || rent != 240000 {
		t.Fatal(g, rent)
	}
	// 80C over the limit is capped in the estimate; employee PF counts under 80C.
	r := TaxEstimate(TaxInput{SalaryYear: 1200000, BasicYear: 600000, HraYear: 300000, EmployeePfYear: 21600, RentYear: 240000, Metro: true, Deductions: g})
	if r.OldDeductions != 175000 || r.HraExempt != 180000 {
		t.Fatalf("%+v", r)
	}
}

func TestEsiKeepReplacesMedical(t *testing.T) {
	r := Calc(Input{Type: Permanent, Gross: 25000, EsiKeep: true})
	if !r.EsiOn || r.Med != 0 || r.EeESI != 188 {
		t.Fatalf("ESI kept: on %v, medical %v, ESI %v", r.EsiOn, r.Med, r.EeESI)
	}
	if r := Calc(Input{Type: Permanent, Gross: 25000}); r.EsiOn || r.Med != 500 {
		t.Fatalf("above the ceiling: medical %v", r.Med)
	}
}

// The owner's example (14 Nov 2026): quarterly variable pay, joined in May 2026: paid in July and October 2026,
// January and April 2027, and in no other month; the joining month is month 1 of the cycle.
func TestVariablePayoutOwnCycleFromJoining(t *testing.T) {
	pct, cyc := 10.0, 3
	paid := map[int]bool{7: true, 10: true, 1: true, 4: true}
	for m := 1; m <= 12; m++ {
		got := Calc(Input{Type: Permanent, Gross: 30000, VarPct: &pct, VarCycle: &cyc, JoinMonth: 5, Month: m}).VarPay
		if (got > 0) != paid[m] {
			t.Errorf("month %d: variable paid %v, want paid %v", m, got, paid[m])
		}
	}
	// The amount is the existing rule: the cycle's three months (pro rata accruals when given).
	if v := Calc(Input{Type: Permanent, Gross: 30000, VarPct: &pct, VarCycle: &cyc, JoinMonth: 5, Month: 7}).VarPay; v != 9000 {
		t.Errorf("July payout %v, want 9000", v)
	}
}

// A PF exempt joiner (owner, 14 Nov 2026): no employee or employer PF, EPS, EDLI or admin; ESI and the rest unchanged.
func TestPfExemptCalc(t *testing.T) {
	a := Calc(Input{Type: Permanent, Gross: 21000})
	b := Calc(Input{Type: Permanent, Gross: 21000, PfExempt: true})
	if a.EePF == 0 || a.ErPF == 0 || a.Edli == 0 || a.Admin == 0 {
		t.Fatalf("member pays PF: %+v", a)
	}
	if b.EePF != 0 || b.ErPF != 0 || b.Eps != 0 || b.ErEpf != 0 || b.Edli != 0 || b.Admin != 0 || !b.PfExempt {
		t.Fatalf("exempt: %+v", b)
	}
	if b.EeESI != a.EeESI || b.Bonus != a.Bonus || b.Net != a.Net+a.EePF {
		t.Fatalf("only PF changes: %+v vs %+v", b, a)
	}
	if c := Calc(Input{Type: Contractual, Gross: 21000, PfExempt: true}); c.PfExempt {
		t.Fatal("no PF flag where PF never applies")
	}
}
