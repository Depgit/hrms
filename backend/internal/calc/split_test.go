package calc

import "testing"

// ITM-28 (user decision 13 Nov 2026): a gross revised from 15 Nov pays 14 days at the old gross and 16 at the new.
func TestSplitGross(t *testing.T) {
	if g := SplitGross("2026-11", 56500, 60000, "2026-11-15"); g != Round2((56500*14+60000*16)/30.0) {
		t.Fatalf("15 Nov split: %v", g)
	}
	if g := SplitGross("2026-11", 56500, 60000, "2026-11-01"); g != 60000 {
		t.Fatalf("1st: %v", g)
	}
	if g := SplitGross("2026-11", 0, 60000, "2026-11-15"); g != 60000 {
		t.Fatalf("no earlier gross: %v", g)
	}
	if g := SplitGross("2026-12", 56500, 60000, "2026-11-15"); g != 60000 {
		t.Fatalf("other month: %v", g)
	}
	if g := SplitGross("2026-10", 31000, 62000, "2026-10-31"); g != 32000 {
		t.Fatalf("31 Oct: %v", g)
	}
}
