package leave

import "testing"

// EXT-08: the unused balance at the end of a leave year is carried into the next, up to the type's carry
// forward limit; an imported opening balance replaces everything before it.
func TestCarryForward(t *testing.T) {
	pl := TypeCfg{On: true, Days: 15, Credit: "monthly", From: "doj", CF: 30}
	lv := LV{Year: "cal"}
	// Joined 1 Jan 2024: 15 days a year, 5 used in 2024, none in 2025; on 30 Nov 2026 the 2026 credit is 13.75.
	used := []Taken{{From: "2024-06-10", Days: 5}}
	b := Compute(pl, lv, 0, "2024-01-01", "2026-11-30", nil, used, nil)
	// 2024 closes at 10, 2025 at 10 + 15 = 25, carried in full; 2026: 25 + 13.75.
	if b.Bal != 38.75 || b.Carry != 25 {
		t.Fatalf("carry: %+v", b)
	}
	pl.CF = 20
	if b = Compute(pl, lv, 0, "2024-01-01", "2026-11-30", nil, used, nil); b.Bal != 33.75 || b.Carry != 20 {
		t.Fatalf("carry capped at the limit: %+v", b)
	}
	pl.CF = 0
	if b = Compute(pl, lv, 0, "2024-01-01", "2026-11-30", nil, used, nil); b.Bal != 13.75 || b.Carry != 0 {
		t.Fatalf("no carry forward: %+v", b)
	}
	// An opening balance loaded in 2025 is the starting point: 2025 closes at 6 + 15 x 9/12 = 17.25 (Apr to Dec).
	pl.CF = 30
	op := &Opening{At: "2025-03-31", Balance: 6}
	if b = Compute(pl, lv, 0, "2024-01-01", "2026-11-30", op, used, nil); b.Bal != 31 || b.Carry != 17.25 {
		t.Fatalf("opening then carry: %+v", b)
	}
	// Inside the opening's own year nothing is carried.
	if b = Compute(pl, lv, 0, "2024-01-01", "2025-05-31", op, used, nil); b.Carry != 0 || b.Bal != 8.5 {
		t.Fatalf("opening year: %+v", b)
	}
}
