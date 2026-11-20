package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/api"
)

// A long upload is worked in several transactions (MongoDB ends one after 60 s); with the budget cut to nothing
// every row gets its own, and the upload must behave exactly as one: results in line order, repeated rows refused,
// one audit entry, a check keeping nothing and an apply keeping every good row.
func TestBulkUploadSplitsTransactions(t *testing.T) {
	c, _, st := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1020")
	ctx := context.Background()
	defer api.SetBulkTxBudget(time.Nanosecond)()

	d1, d2, d3 := uiDay(-6), uiDay(-5), uiDay(-4)
	csv := "Employee ID,Date,In time,Out time\n" +
		"EMP-1020," + d1 + ",06:00,14:00\n" +
		"EMP-1020," + d2 + ",06:05,14:05\n" +
		"EMP-1020," + d1 + ",07:00,15:00\n" + // repeated
		"EMP-1020," + uiDay(2) + ",06:00,14:00\n" + // future
		"EMP-1020," + d3 + ",06:10,\n"
	days := func() int64 {
		n, err := st.DB.C("attendance_day").Count(ctx, bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "source", Value: "MIS upload"},
			{Key: "date", Value: bson.D{{Key: "$in", Value: bson.A{d1, d2, d3}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	audits := func() int64 {
		n, _ := st.DB.C("audit_log").Count(ctx, bson.D{{Key: "entity", Value: "bulk"}, {Key: "entity_id", Value: "att"}})
		return n
	}
	before := audits()

	s, chk := c.sendFile("/api/bulk/att/check", "EMP-1002", "HR Admin", "att.csv", []byte(csv))
	if s != 200 || num(chk["ok"]) != 3 || chk["applied"] != false {
		t.Fatal("check", s, chk)
	}
	res := chk["results"].([]any)
	if len(res) != 5 {
		t.Fatal("one result per row", res)
	}
	for i, x := range res {
		if num(x.(map[string]any)["line"]) != float64(i+2) {
			t.Fatal("results in line order", res)
		}
	}
	if e := fmt.Sprint(res[2].(map[string]any)["errors"]); e != "[Appears again (first on line 2). Keep one row each]" {
		t.Fatal("repeated row", e)
	}
	if e := fmt.Sprint(res[3].(map[string]any)["errors"]); e != "[Date is in the future]" {
		t.Fatal("future row", e)
	}
	if n := days(); n != 0 {
		t.Fatal("a check keeps nothing", n)
	}
	if audits() != before {
		t.Fatal("a check writes no audit")
	}

	s, ap := c.sendFile("/api/bulk/att/apply", "EMP-1002", "HR Admin", "att.csv", []byte(csv))
	if s != 200 || num(ap["ok"]) != 3 || ap["applied"] != true || len(ap["results"].([]any)) != 5 {
		t.Fatal("apply", s, ap)
	}
	if n := days(); n != 3 {
		t.Fatal("every good row is kept", n)
	}
	if audits() != before+1 {
		t.Fatal("one audit entry for the upload")
	}
	if a := apiDoc(st, "audit_log", bson.D{{Key: "entity", Value: "bulk"}, {Key: "entity_id", Value: "att"}}); a == nil {
		t.Fatal("audit")
	}
	n, _ := st.DB.C("audit_log").Count(ctx, bson.D{{Key: "entity", Value: "bulk"}, {Key: "entity_id", Value: "att"}, {Key: "action", Value: "Bulk update “Attendance upload” applied: 3 rows"}})
	if n != 1 {
		var all []string
		rows, _ := st.DB.C("audit_log").Find(ctx, bson.D{{Key: "entity", Value: "bulk"}})
		for _, r := range rows {
			all = append(all, fmt.Sprint(r["action"]))
		}
		t.Fatal("the audit counts the rows of every transaction", all)
	}
	// Applying again changes nothing; the rows after the first see what the first ones wrote.
	if _, ap = c.sendFile("/api/bulk/att/apply", "EMP-1002", "HR Admin", "att.csv", []byte(csv)); num(ap["ok"]) != 0 {
		t.Fatal("already applied", ap)
	}

	// Roster rows of one employee across transactions: each later row sees the earlier ones.
	// Two days that are not Sundays (a Sunday is already a weekly off, so WO would change nothing).
	var wd []string
	for n := 20; len(wd) < 2; n++ {
		if d, _ := time.Parse("2006-01-02", uiDay(n)); d.Weekday() != time.Sunday {
			wd = append(wd, uiDay(n))
		}
	}
	r1, r2 := wd[0], wd[1]
	long := "Employee ID,Date,Code,Reason\nEMP-1020," + r1 + ",WO,\nEMP-1020," + r2 + ",WO,\n"
	if s, ap = c.sendFile("/api/bulk/roster/apply", "EMP-1002", "HR Admin", "long.csv", []byte(long)); s != 200 || len(ap["results"].([]any)) != 2 {
		t.Fatal("roster", s, ap)
	}
	for _, d := range []string{r1, r2} {
		if code := uiField(ctx, st, "roster_entry", bson.D{{Key: "employee_id", Value: "EMP-1020"}, {Key: "date", Value: d}}, "code"); code != "WO" {
			t.Fatal("roster applied", d, code, ap)
		}
	}
}
