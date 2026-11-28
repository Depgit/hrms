package attend

import "testing"

// Where a shift applies (owner, 24 Nov 2026): a row matches when its department and its location each match or are
// All (""); a shift from the master with no switched-on row is offered to nobody; nil rows (a timing that is no
// master shift) apply everywhere.
func TestShiftRunsFor(t *testing.T) {
	sh := Shift{Code: "N", Maps: []ShiftMap{{Dept: "FO", Loc: "GGN"}, {Dept: "", Loc: "NOI"}, {Dept: "HK", Loc: ""}}}
	cases := []struct {
		dept, loc string
		want      bool
	}{
		{"FO", "GGN", true},  // Front Office at Gurgaon
		{"HK", "GGN", true},  // Housekeeping at all locations
		{"FB", "NOI", true},  // all departments at Noida
		{"FO", "NOI", true},  // all departments at Noida
		{"FB", "GGN", false}, // no row for F&B at Gurgaon
		{"FO", "BLR", false}, // Front Office only at Gurgaon
	}
	for _, c := range cases {
		if got := sh.RunsFor(c.dept, c.loc); got != c.want {
			t.Errorf("%s at %s: %v, want %v", c.dept, c.loc, got, c.want)
		}
	}
	if !sh.RunsAt("GGN") || !sh.RunsAt("BLR") { // BLR through Housekeeping at all locations
		t.Error("RunsAt: some department at the location")
	}
	if (Shift{Maps: []ShiftMap{{Dept: "FO", Loc: "GGN"}}}).RunsAt("BLR") {
		t.Error("RunsAt: no row at BLR")
	}
	if everywhere := (Shift{Maps: []ShiftMap{{}}}); !everywhere.RunsFor("any", "where") {
		t.Error("All departments at all locations")
	}
	if nobody := (Shift{Maps: []ShiftMap{}}); nobody.RunsFor("FO", "GGN") || nobody.RunsAt("GGN") {
		t.Error("no switched-on row: offered to nobody")
	}
	if free := (Shift{}); !free.RunsFor("FO", "GGN") {
		t.Error("nil rows: everywhere")
	}
}
