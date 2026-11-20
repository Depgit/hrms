package hst

import "testing"

// A 30-day month (November 2026) so the decided examples come out in whole rupees.
const nov = "2026-11"

func unit12k() Unit {
	return Unit{ID: "U1", Home: "P1", Leases: []Lease{{ID: "L1", Start: "2026-01-01", Rent: 12000}}}
}

// Equal per person (the default): the month cost ÷ everyone who stayed in the unit at any time in the month.
func TestSplitEqualDecidedExamples(t *testing.T) {
	u := unit12k()
	// 1 person all month = 12k.
	m := Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01"}}, Equal)
	if m.Method != Equal || m.Shares["A"] != 12000 || m.Units["U1"].Empty != 0 || m.Units["U1"].Cost != 12000 {
		t.Fatalf("one person: %+v", m)
	}
	// 2 people = 6k each; 4 people = 3k each.
	two := []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01"}, {UnitID: "U1", EmployeeID: "B", In: "2026-11-01"}}
	if m := Split(nov, []Unit{u}, two, Equal); m.Shares["A"] != 6000 || m.Shares["B"] != 6000 {
		t.Fatalf("two people: %v", m.Shares)
	}
	four := append(two, Stay{UnitID: "U1", EmployeeID: "C", In: "2026-11-01"}, Stay{UnitID: "U1", EmployeeID: "D", In: "2026-09-01", Out: "2026-12-05"})
	if m := Split(nov, []Unit{u}, four, Equal); m.Shares["A"] != 3000 || m.Shares["B"] != 3000 || m.Shares["C"] != 3000 || m.Shares["D"] != 3000 {
		t.Fatalf("four people: %v", m.Shares)
	}
	// An unknown or empty method is equal.
	if m := Split(nov, []Unit{u}, two, ""); m.Method != Equal || m.Shares["B"] != 6000 {
		t.Fatalf("default method: %+v", m)
	}
}

// Equal mode counts anyone who stayed on any day, however short: B arriving on the 16th still pays half, and a unit
// empty for part of the month has no empty-unit cost.
func TestSplitEqualPartMonth(t *testing.T) {
	u := unit12k()
	m := Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01"}, {UnitID: "U1", EmployeeID: "B", In: "2026-11-16"}}, Equal)
	if m.Shares["A"] != 6000 || m.Shares["B"] != 6000 || m.Units["U1"].Days["B"] != 15 {
		t.Fatalf("arrival on the 16th: %v %v", m.Shares, m.Units["U1"].Days)
	}
	m = Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "B", In: "2026-11-16"}}, Equal)
	if m.Shares["B"] != 12000 || len(m.Empty) != 0 || m.Units["U1"].Empty != 0 || m.Units["U1"].EmptyDays != 15 {
		t.Fatalf("alone from the 16th: %v %v %+v", m.Shares, m.Empty, m.Units["U1"])
	}
	// One person who left on the 2nd (one day) and one who came on the 30th: half each.
	m = Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01", Out: "2026-11-02"}, {UnitID: "U1", EmployeeID: "B", In: "2026-11-30"}}, Equal)
	if m.Shares["A"] != 6000 || m.Shares["B"] != 6000 || len(m.Empty) != 0 {
		t.Fatalf("one day each: %v %v", m.Shares, m.Empty)
	}
	// Checked out on the 1st: not in the unit this month, so the month is empty.
	m = Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01", Out: "2026-11-01"}}, Equal)
	if len(m.Shares) != 0 || m.Empty["P1"] != 12000 {
		t.Fatalf("left on the 1st: %v %v", m.Shares, m.Empty)
	}
	// Three people: shares to the paisa that add up to the cost exactly (the paisa left goes to the first).
	u.Leases[0].Rent = 10000
	m = Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01"}, {UnitID: "U1", EmployeeID: "B", In: "2026-10-01"}, {UnitID: "U1", EmployeeID: "C", In: "2026-10-01"}}, Equal)
	um := m.Units["U1"]
	if m.Shares["A"] != 3333.34 || m.Shares["B"] != 3333.33 || m.Shares["C"] != 3333.33 || um.Empty != 0 || um.Allocated != 10000 {
		t.Fatalf("three people: %v %+v", m.Shares, um)
	}
}

// The "Split by days stayed" option: each day's cost among the people staying that day.
func TestSplitByDaysArrival(t *testing.T) {
	u := unit12k()
	// 1, 2 and 4 people all month come out the same as equal mode.
	four := []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01"}, {UnitID: "U1", EmployeeID: "B", In: "2026-11-01"},
		{UnitID: "U1", EmployeeID: "C", In: "2026-11-01"}, {UnitID: "U1", EmployeeID: "D", In: "2026-09-01", Out: "2026-12-05"}}
	if m := Split(nov, []Unit{u}, four, ByDays); m.Method != ByDays || m.Shares["A"] != 3000 || m.Shares["D"] != 3000 {
		t.Fatalf("four people by days: %+v", m)
	}
	// Alone until the 15th, then B arrives on the 16th: days 1–15 A pays 400 a day, 16–30 they share 400.
	m := Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01"}, {UnitID: "U1", EmployeeID: "B", In: "2026-11-16"}}, ByDays)
	if m.Shares["B"] != 3000 || m.Shares["A"] != 9000 || m.Units["U1"].Days["B"] != 15 {
		t.Fatalf("arrival on the 16th: %v %v", m.Shares, m.Units["U1"].Days)
	}
	// Only B, arriving on the 16th: half the month is theirs, the first half is empty-unit cost for the property.
	m = Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "B", In: "2026-11-16"}}, ByDays)
	if m.Shares["B"] != 6000 || m.Empty["P1"] != 6000 || m.Units["U1"].EmptyDays != 15 {
		t.Fatalf("alone from the 16th: %v %v", m.Shares, m.Empty)
	}
	// The check-out day is not charged: out on the 11th = 10 days.
	m = Split(nov, []Unit{u}, []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-10-01", Out: "2026-11-11"}}, ByDays)
	if m.Shares["A"] != 4000 || m.Empty["P1"] != 8000 {
		t.Fatalf("check-out on the 11th: %v %v", m.Shares, m.Empty)
	}
}

func TestSplitEmptyUnitAndCosts(t *testing.T) {
	u := unit12k()
	u.Leases[0].Maintenance = 1000
	u.Fixed = 500
	u.Variable = 1500 // electricity, water …
	// A month with nobody in the unit: the whole month cost is empty-unit cost for its home property, both methods.
	for _, method := range []string{Equal, ByDays} {
		m := Split(nov, []Unit{u}, nil, method)
		if m.Units["U1"].Cost != 15000 || m.Empty["P1"] != 15000 || len(m.Shares) != 0 || m.Units["U1"].EmptyDays != 30 {
			t.Fatalf("empty unit (%s): %+v", method, m)
		}
	}
	// A 31-day month: amounts to the paisa, and cost = allocated + empty exactly, both methods.
	stays := []Stay{{UnitID: "U1", EmployeeID: "A", In: "2026-12-10"}, {UnitID: "U1", EmployeeID: "B", In: "2026-12-03", Out: "2026-12-20"}}
	for _, method := range []string{Equal, ByDays} {
		um := Split("2026-12", []Unit{u}, stays, method).Units["U1"]
		if Round2(um.Allocated+um.Empty) != um.Cost || um.Cost != 15000 {
			t.Fatalf("does not foot (%s): %+v", method, um)
		}
	}
	if m := Split("2026-12", []Unit{u}, stays, Equal); m.Shares["A"] != 7500 || m.Shares["B"] != 7500 || len(m.Empty) != 0 {
		t.Fatalf("equal, 31 days: %v %v", m.Shares, m.Empty)
	}
}

func TestLeaseRentAndIncrease(t *testing.T) {
	l := Lease{Start: "2025-04-01", Rent: 10000, IncPct: 5, IncOn: "2026-04-01"}
	cases := map[string]float64{"2025-03-31": 0, "2025-04-01": 10000, "2026-03-31": 10000, "2026-04-01": 10500, "2027-04-01": 11025}
	for d, want := range cases {
		if got := l.RentOn(d); got != want {
			t.Fatalf("rent on %s = %v, want %v", d, got, want)
		}
	}
	if n := l.NextIncrease("2026-01-01"); n != "2026-04-01" {
		t.Fatalf("next increase %s", n)
	}
	if n := l.NextIncrease("2026-04-01"); n != "2027-04-01" {
		t.Fatalf("next increase after the first %s", n)
	}
	// A mid-month increase is charged day by day: 15 days at 10000 and 15 at 10500 in a 30-day month.
	u := Unit{ID: "U", Leases: []Lease{{Start: "2026-01-01", Rent: 10000, IncPct: 5, IncOn: "2026-11-16"}}}
	if c := Split(nov, []Unit{u}, nil, Equal).Units["U"].Cost; c != 10250 {
		t.Fatalf("mid-month increase cost %v", c)
	}
	// No lease after it ends: an ended lease costs nothing.
	u = Unit{ID: "U", Leases: []Lease{{Start: "2026-01-01", End: "2026-11-15", Rent: 12000}}}
	if c := Split(nov, []Unit{u}, nil, Equal).Units["U"].Cost; c != 6000 {
		t.Fatalf("ended lease cost %v", c)
	}
}

func TestOverlaps(t *testing.T) {
	if Overlaps("2026-11-01", "2026-11-10", "2026-11-10", "") {
		t.Fatal("check-out day is free for the next person")
	}
	if !Overlaps("2026-11-01", "", "2026-12-01", "2026-12-05") {
		t.Fatal("open stay overlaps a later one")
	}
	if !LeasesOverlap(Lease{Start: "2026-01-01", End: "2026-06-30"}, Lease{Start: "2026-06-30"}) {
		t.Fatal("leases sharing the end day overlap")
	}
	if LeasesOverlap(Lease{Start: "2026-01-01", End: "2026-06-30"}, Lease{Start: "2026-07-01"}) {
		t.Fatal("back-to-back leases do not overlap")
	}
}
