package api

import "testing"

// The directory's approximate search: words in any order, partial words, small typos from 4 letters.
func TestDirMatch(t *testing.T) {
	p := &dirPerson{name: "Sandeep Rawat", desig: "HR Manager", dept: "Human Resources", loc: "Gurugram – Sector 29 Hub", email: "sandeep.rawat@saltstayz.com"}
	p.text = "sandeep rawat hr manager human resources gurugram – sector 29 hub sandeep.rawat@saltstayz.com"
	p.words = dirWords(p.text)
	for _, c := range []struct {
		q  string
		ok bool
	}{
		{"", true},
		{"Sandeep Rawat", true},
		{"rawat sandeep", true},      // any order
		{"san raw", true},            // partial words
		{"andee", true},              // inside a word
		{"sandep", true},             // a letter missing
		{"sandeep rwaat", true},      // two letters swapped
		{"sundeep", true},            // a letter changed
		{"gurgaon", false},           // two changes in 7 letters: too far
		{"gurugarm sec", true},       // swapped letters, and a partial word
		{"resourcse", true},          // 9 letters: up to two edits
		{"humn", true},               // 4 letters, one missing (start of "human")
		{"rwt", false},               // under 4 letters: no typos
		{"sandeep.rawat@salt", true}, // part of the email as typed
		{"saltstayz.com", true},
		{"manager hub 29", true},
		{"priya", false},
		{"sandeep priya", false}, // every word must fit
	} {
		if _, ok := dirMatch(c.q, p); ok != c.ok {
			t.Errorf("%q: match %v, want %v", c.q, ok, c.ok)
		}
	}
	// Better fits score lower: the start of a word, then inside a word, then a typo.
	if s0, s1, s2 := dirTokenScore("rawat", p.words), dirTokenScore("awat", p.words), dirTokenScore("rawet", p.words); s0 != 0 || s1 != 1 || s2 != 2 {
		t.Errorf("scores %d %d %d", s0, s1, s2)
	}
	if whole, words := dirScoreOrFar(dirMatch("rawat hr", p)), dirScoreOrFar(dirMatch("hr rawat", p)); whole >= words {
		t.Errorf("the query as typed should score best: %d, words %d", whole, words)
	}
	if d := dirDist([]rune("abcd"), []rune("abdc"), 1); d != 1 {
		t.Errorf("swap distance %d", d)
	}
}

func dirScoreOrFar(score int, ok bool) int {
	if !ok {
		return 1 << 20
	}
	return score
}

// Single letters count only inside the query as typed: "F&B" is not "a word starting with f and one with b".
func TestDirMatchSingleLetters(t *testing.T) {
	mk := func(text string) *dirPerson { return &dirPerson{text: text, words: dirWords(text)} }
	if _, ok := dirMatch("F&B", mk("asha front office bell captain")); ok {
		t.Error("F&B matched Front Office / Bell")
	}
	if _, ok := dirMatch("F&B", mk("asha f&b steward")); !ok {
		t.Error("F&B did not match F&B")
	}
	if _, ok := dirMatch("ravi k", mk("ravi kumar ravi.k@x.com")); !ok {
		t.Error("ravi k")
	}
}
