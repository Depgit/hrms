package api

import "testing"

// The people picker's approximate matching: word order, partial words, the code with or without its prefix,
// initials, small typos, and the order of the results.
func TestPeopleMatch(t *testing.T) {
	rk := newPMPerson("EMP-1023", "Rakesh Kumar", "Property Manager", "Operations", "Gurugram – Sector 29 Hub", "Gurugram", "rakesh.kumar@saltstayz.com")
	sr := newPMPerson("EMP-1002", "Sandeep Rawat", "HR Manager", "Human Resources", "Corporate Office", "Delhi", "")
	ak := newPMPerson("EMP-1009", "Anil Kumar Kapoor", "Line Manager", "Operations", "Sector 29 Hub", "Gurugram", "")
	fits := func(q string, p *pmPerson) bool { return pmScore(q, p) > 0 }
	for _, c := range []struct {
		q    string
		p    *pmPerson
		want bool
	}{
		{"rakesh kumar", rk, true},
		{"kumar rakesh", rk, true},   // any order
		{"rak", rk, true},            // start of a word
		{"kum rak", rk, true},        // partial words in any order
		{"akes", rk, true},           // inside a word
		{"1023", rk, true},           // code without prefix
		{"emp1023", rk, true},        // code without the dash
		{"EMP-1023", rk, true},       // code as written
		{"102", rk, true},            // start of the code
		{"rk", rk, true},             // initials
		{"rakhes", rk, true},         // one swap
		{"raksh", rk, true},          // one letter missing
		{"rakesh kumaar", rk, true},  // a typo in the second word
		{"gurugram", rk, true},       // location / city
		{"property", rk, true},       // designation
		{"operations rak", rk, true}, // department and name
		{"saltstayz", rk, true},      // email (when given)
		{"suresh", rk, false},        // a different name
		{"rk", sr, false},            // wrong initials
		{"sr", sr, true},             // initials
		{"ak", ak, true},             // first and last initials of three names
		{"akk", ak, true},            // all initials
		{"ra", ak, false},            // not the start of any word, no initials
		{"rakesh zz", rk, false},     // every word must fit
		{"rkesh", rk, true},          // one letter missing at the start part
		{"rsakesh", rk, true},        // one extra letter
		{"rxkxsh", rk, false},        // two edits on a 6-letter word is too many
		{"kumaar", ak, true},         // typo
		{"sandep", sr, true},         // typo, missing letter
		{"humn resources", sr, true}, // typo in a department word
		{"ressources", sr, true},     // typo in a long word (10 letters, 2 allowed)
		{"xyzzy", sr, false},
	} {
		if got := fits(c.q, c.p); got != c.want {
			t.Errorf("%q vs %s: got %v, want %v (score %d)", c.q, c.p.name, got, c.want, pmScore(c.q, c.p))
		}
	}
	// Exact beats start beats inside beats typo.
	ex, pre, in, typo := pmWordScore("kumar", "kumar"), pmWordScore("kum", "kumar"), pmWordScore("uma", "kumar"), pmWordScore("kumra", "kumar")
	if !(ex > pre && pre > in && in > typo && typo > 0) {
		t.Errorf("ranks: exact %d, prefix %d, contains %d, typo %d", ex, pre, in, typo)
	}
	// Order: the exact name first, then a start, then a typo.
	people := []*pmPerson{
		newPMPerson("EMP-1", "Ravi Kumaran", "", "", "", "", ""),
		newPMPerson("EMP-2", "Ravi Kumar", "", "", "", "", ""),
		newPMPerson("EMP-3", "Ravi Kumra", "", "", "", "", ""),
		newPMPerson("EMP-4", "Sunita Rao", "", "", "", "", ""),
	}
	hits := pmRank("ravi kumar", people)
	if len(hits) != 3 || hits[0].id != "EMP-2" || hits[1].id != "EMP-1" || hits[2].id != "EMP-3" {
		t.Errorf("order: %+v", hits)
	}
	// The code typed in full puts that person first.
	if h := pmRank("emp-4", people); len(h) == 0 || h[0].id != "EMP-4" {
		t.Errorf("code first: %+v", h)
	}
}
