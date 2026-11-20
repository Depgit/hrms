package leave

import "testing"

// Opening leave balances (Bulk update › Opening leave balances, owner's check of 13 Nov 2026): the balance given "as
// at" a date includes all leave accrued up to that month; with monthly credit, Days/12 is added from the next month.
func TestOpeningBalanceAccrual(t *testing.T) {
	cl := Default(KindBalance)
	cl.Days = 12 // CL 12 a year: 1 a month
	lv := LV{Year: "cal", Prorate: true}
	open := &Opening{"2026-10-31", 8}
	for _, c := range []struct {
		on   string
		want float64
	}{
		{"2026-10-31", 8},  // the As at date itself
		{"2026-11-01", 9},  // November's credit
		{"2026-11-15", 9},  // 15 Nov: 9
		{"2026-11-30", 9},  // still November
		{"2026-12-01", 10}, // 1 Dec: 10
		{"2026-12-31", 10}, // the end of the leave year
	} {
		if b := Compute(cl, lv, 6, "2020-01-01", c.on, open, nil, nil); b.Credit != c.want || b.Bal != c.want {
			t.Fatalf("on %s: want %v, got %+v", c.on, c.want, b)
		}
	}

	// As at 1 October: October's credit is in the balance given, so accrual still starts in November.
	oct1 := &Opening{"2026-10-01", 8}
	for on, want := range map[string]float64{"2026-10-20": 8, "2026-11-15": 9, "2026-12-01": 10} {
		if b := Compute(cl, lv, 6, "2020-01-01", on, oct1, nil, nil); b.Credit != want {
			t.Fatalf("as at 1 Oct, on %s: want %v, got %+v", on, want, b)
		}
	}

	// Leave taken before the As at date is in the balance already; leave from it on is taken from the balance.
	b := Compute(cl, lv, 6, "2020-01-01", "2026-11-15", open, []Taken{{"2026-10-10", 2}, {"2026-11-05", 1}}, []Taken{{"2026-11-20", 0.5}})
	if b.Credit != 9 || b.Used != 1 || b.Pending != 0.5 || b.Bal != 7.5 {
		t.Fatalf("leave around the opening: %+v", b)
	}

	// Credited upfront: nothing more after the opening in its leave year.
	up := cl
	up.Credit = "upfront"
	for _, on := range []string{"2026-10-31", "2026-11-15", "2026-12-01", "2026-12-31"} {
		if b := Compute(up, lv, 6, "2020-01-01", on, open, nil, nil); b.Credit != 8 {
			t.Fatalf("upfront, on %s: want 8, got %+v", on, b)
		}
	}

	// The next leave year is worked out by the plan again (12 upfront; 1 a month).
	if b := Compute(up, lv, 6, "2020-01-01", "2027-01-15", open, nil, nil); b.Credit != 12 {
		t.Fatalf("upfront, next year: %+v", b)
	}
	if b := Compute(cl, lv, 6, "2020-01-01", "2027-02-15", open, nil, nil); b.Credit != 2 {
		t.Fatalf("monthly, next year: %+v", b)
	}

	// A plan crediting after probation keeps that rule after the opening: no credit for months ending before it.
	pl := cl
	pl.From = "prob"
	if b := Compute(pl, lv, 6, "2026-08-10", "2026-12-01", &Opening{"2026-10-31", 0}, nil, nil); b.Credit != 0 {
		t.Fatalf("in probation until February 2027: %+v", b)
	}
}
