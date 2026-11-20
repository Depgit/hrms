package api

import "testing"

// ITM-106: the number of opening-advance recoveries counts a small remainder as one more month.
func TestRecoveryMonths(t *testing.T) {
	for _, x := range []struct {
		bal, mon float64
		want     int
	}{{60001, 1000, 61}, {60000, 1000, 60}, {5004, 2500, 3}, {18000, 5000, 4}, {10000, 3000, 4}, {100, 1000, 1}, {0.01, 1000, 1}} {
		if got := recoveryMonths(x.bal, x.mon); got != x.want {
			t.Errorf("recoveryMonths(%v, %v) = %d, want %d", x.bal, x.mon, got, x.want)
		}
	}
}
