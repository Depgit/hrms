package api_test

// Payroll test fixes, part E (user decisions 13 Nov): net pay in whole rupees, the same figure everywhere.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// RUN-53, ITM-59, RUN-66, REAL-37, RUN-90: after a dry run every stored net is whole rupees, each row foots with
// its rounding off, and the payslip's figure and words are that same net (no paise).
func TestFixENetWholeRupees(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	signAll(c, "EMP-1002", "EMP-1011", "EMP-1012")
	setFirstYm(t, c, "")
	_, state := c.do("GET", "/api/payroll", "EMP-1011", "Payroll", nil)
	open := state["openYm"].(string)
	c.pcCloseAll(t, open)
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatal(r)
	}
	rows, err := st.DB.C("payroll_row").Find(ctx, bson.M{"ym": open})
	if err != nil || len(rows) == 0 {
		t.Fatalf("no rows: %v", err)
	}
	paise, rounded := 0, 0
	for _, r := range rows {
		net := apiFloat(r["net"])
		if net != math.Round(net) {
			paise++
		}
		d, _ := r["data"].(map[string]any)
		if d == nil {
			d, _ = r["data"].(bson.M)
		}
		te, td, ro, carry := apiFloat(d["totalEarn"]), apiFloat(d["totalDed"]), apiFloat(d["roundOff"]), apiFloat(d["carry"])
		if ro != 0 {
			rounded++
		}
		if carry == 0 && math.Abs(te-td+ro-net) > 0.001 {
			t.Fatalf("%v: %v − %v + %v ≠ %v", r["employee_id"], te, td, ro, net)
		}
	}
	if paise > 0 {
		t.Fatalf("%d of %d nets carry paise", paise, len(rows))
	}
	if rounded == 0 {
		t.Log("no row needed rounding in this sample")
	}
	// The payslip shows the stored net, in figures and words.
	pick := rows[0]
	for _, r := range rows {
		if apiFloat(r["net"]) > 0 && (pick == nil || apiFloat(pick["net"]) <= 0) {
			pick = r
		}
	}
	id := fmt.Sprint(pick["employee_id"])
	s, pv := c.do("GET", "/api/payslip-preview?employeeId="+id+"&ym="+open, "EMP-1011", "Payroll", nil)
	if s != 200 {
		t.Fatal(pv)
	}
	snap := pv["snap"].(map[string]any)
	if apiFloat(snap["net"]) != apiFloat(pick["net"]) || strings.Contains(fmt.Sprint(snap["words"]), "Paise") {
		t.Fatalf("payslip net %v words %q, stored %v", snap["net"], snap["words"], pick["net"])
	}
	te, td, ro := apiFloat(snap["totE"]), apiFloat(snap["totD"]), apiFloat(snap["roundOff"])
	if math.Abs(te-td+ro-apiFloat(snap["net"])) > 0.001 {
		t.Fatalf("payslip does not foot: %v − %v + %v ≠ %v", te, td, ro, snap["net"])
	}
}
