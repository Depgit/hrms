package api

import "testing"

// Combined mappings (owner, 24 Nov 2026): Individual > Department + place (narrower place first) > Location >
// Cluster > City > State > Department > Designation > Grade > Employment type; a switched-off mapping is ignored.
func TestPolicyComboPrecedence(t *testing.T) {
	on := func(level, ref string) polMap { return polMap{Level: level, Ref: ref, On: true} }
	pol := func(id string, maps ...polMap) *workPolicy {
		return &workPolicy{ID: id, Name: id, Status: "Active", Maps: maps}
	}
	e := &emp{ID: "EMP-1", Dept: "People Ops", LocID: "loc1", ClusterID: "cl1", CityID: "ci1", StateID: "st1", Type: "Permanent", Grade: "G3", Desig: "Executive"}
	def := pol("default")
	loc := pol("location", on("location", "loc1"))
	dept := pol("department", on("department", "People Ops"))
	state := pol("state", on("state", "st1"))
	cmbState := pol("dept+state", on("dept_state", "People Ops|st1"))
	cmbCity := pol("dept+city", on("dept_city", "People Ops|ci1"))
	cmbCluster := pol("dept+cluster", on("dept_cluster", "People Ops|cl1"))
	cmbLoc := pol("dept+location", on("dept_location", "People Ops|loc1"))
	ind := pol("individual", on("employee", "EMP-1"))
	ts := &timeSet{}
	get := func(x *emp, ps ...*workPolicy) (string, string) {
		ts.pols = append([]*workPolicy{def}, ps...)
		p, via := ts.policyFor(x)
		return p.ID, via
	}
	for _, c := range []struct {
		name     string
		ps       []*workPolicy
		want     string
		wantVia  string
		employee *emp
	}{
		{"a combo beats a single location and a single department", []*workPolicy{loc, dept, cmbState}, "dept+state", "Department + State", e},
		{"department + location beats department + cluster", []*workPolicy{cmbCluster, cmbLoc}, "dept+location", "Department + Location", e},
		{"department + cluster beats department + city", []*workPolicy{cmbCity, cmbCluster}, "dept+cluster", "Department + Cluster", e},
		{"department + city beats department + state", []*workPolicy{cmbState, cmbCity, state}, "dept+city", "Department + City", e},
		{"the individual beats every combo", []*workPolicy{cmbLoc, cmbCluster, ind}, "individual", "Employee", e},
		{"a switched-off combo is ignored", []*workPolicy{pol("off", polMap{Level: "dept_location", Ref: "People Ops|loc1"}), loc}, "location", "Location", e},
		{"another department at the place does not match", []*workPolicy{cmbLoc, dept}, "default", "Default",
			&emp{ID: "EMP-2", Dept: "Sales", LocID: "loc1", ClusterID: "cl1", CityID: "ci1", StateID: "st1"}},
		{"the department elsewhere does not match the combo", []*workPolicy{cmbLoc, dept}, "department", "Department",
			&emp{ID: "EMP-3", Dept: "People Ops", LocID: "loc2", ClusterID: "cl2", CityID: "ci2", StateID: "st2"}},
		{"a missing place never matches a combo", []*workPolicy{cmbLoc, cmbCluster, dept}, "department", "Department",
			&emp{ID: "EMP-4", Dept: "People Ops", CityID: "ci9"}},
	} {
		if id, via := get(c.employee, c.ps...); id != c.want || via != c.wantVia {
			t.Errorf("%s: got %s through %s, want %s through %s", c.name, id, via, c.want, c.wantVia)
		}
	}
	// The conflict check and the overlap report see the combo first, then the single levels.
	ts.pols = []*workPolicy{def, dept, loc, cmbLoc}
	ms := ts.policyMatches(e)
	if len(ms) != 3 || ms[0].P.ID != "dept+location" || ms[1].P.ID != "location" || ms[2].P.ID != "department" {
		t.Fatalf("matches %v", ms)
	}
	if !mapMatches(e, on("dept_city", "People Ops|ci1")) || mapMatches(e, on("dept_city", "Sales|ci1")) {
		t.Fatal("mapMatches on a combo")
	}
	if d, p, ok := splitCombo("R&D | Labs|loc-9"); !ok || d != "R&D | Labs" || p != "loc-9" {
		t.Fatalf("split %q %q %v", d, p, ok)
	}
	for _, x := range []struct {
		h    float64
		want string
	}{{8, "8 h"}, {8.5, "8 h 30 min"}, {9.75, "9 h 45 min"}, {500.0 / 60, "8 h 20 min"}} {
		if got := hmWords(x.h); got != x.want {
			t.Errorf("hmWords(%v) = %q, want %q", x.h, got, x.want)
		}
	}
}
