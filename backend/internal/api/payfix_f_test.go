package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Fixes from the payroll test (fork F), user decisions of 13 Nov 2026.

func pfDaysIn(ym string) int {
	t, _ := time.Parse("2006-01", ym)
	return t.AddDate(0, 1, -1).Day()
}

// ITM-28: a gross revised from the 15th of the open month pays the old gross for the 14 days before and the new
// gross from the 15th; the date is kept on the revision.
func TestPayfixMidMonthGrossSplit(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1011")
	open := pdOpenYm(t, c)
	if _, err := st.DB.C("gross_history").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("gross_history").Insert(ctx, bson.M{"employee_id": "EMP-1020", "from_month": "2020-01", "gross": 56500.0, "reason": "Test", "created_by": "Test"}); err != nil {
		t.Fatal(err)
	}
	csv := "Employee ID,New Gross,Effective From\nEMP-1020,60000," + open + "-15\n"
	if s, r := c.sendFile("/api/bulk/gross/apply", "EMP-1002", "HR Admin", "gross.csv", []byte(csv)); s != 200 || num(r["ok"]) != 1 {
		t.Fatalf("apply: %d %v", s, r)
	}
	h := apiDoc(st, "gross_history", bson.M{"employee_id": "EMP-1020", "from_month": open})
	if h == nil || h["from_date"] != open+"-15" {
		t.Fatalf("revision kept no date: %v", h)
	}
	c.pcCloseAll(t, open)
	if s, r := c.do("POST", "/api/payroll/"+open+"/dry-run", "EMP-1011", "Payroll", nil); s != 200 {
		t.Fatalf("dry run: %d %v", s, r)
	}
	row := apiDoc(st, "payroll_row", bson.M{"ym": open, "employee_id": "EMP-1020"})
	if row == nil {
		t.Fatal("no row")
	}
	dim := float64(pfDaysIn(open))
	want := float64(int64((56500*14+60000*(dim-14))/dim*100+0.5)) / 100
	if g := apiFloat(row["data"].(map[string]any)["gross"]); g != want {
		t.Fatalf("gross for the month %v, want %v (14 days at 56,500, the rest at 60,000)", g, want)
	}
}

// ITM-28: a back-dated revision from the 15th of a paid month raises the arrear only for the days from the 15th.
func TestPayfixMidMonthGrossArrear(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	c.signIn("EMP-1002")
	c.signIn("EMP-1011")
	open := pdOpenYm(t, c)
	prev := addMonths(open, -1)
	if _, err := st.DB.C("gross_history").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("payroll_row").DeleteMany(ctx, bson.M{"employee_id": "EMP-1020", "ym": prev}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.C("gross_history").Insert(ctx, bson.M{"employee_id": "EMP-1020", "from_month": "2020-01", "gross": 50000.0, "reason": "Test", "created_by": "Test"}); err != nil {
		t.Fatal(err)
	}
	dim := pfDaysIn(prev)
	newGross := 50000 + dim*100 // 100 a day more: the arrear is 100 x the days from the 15th
	csv := fmt.Sprintf("Employee ID,New Gross,Effective From\nEMP-1020,%d,%s-15\n", newGross, prev)
	s, r := c.sendFile("/api/bulk/gross/check", "EMP-1002", "HR Admin", "gross.csv", []byte(csv))
	if s != 200 {
		t.Fatalf("check: %d %v", s, r)
	}
	want := fmt.Sprintf("arrear of ₹%s for 1 month", commas((dim-14)*100))
	if txt := fmt.Sprint(r); !strings.Contains(txt, want) || !strings.Contains(txt, "split by days") {
		t.Fatalf("expected %q and the split note: %s", want, txt)
	}
}

func commas(n int) string {
	s := fmt.Sprint(n)
	if len(s) > 3 {
		s = s[:len(s)-3] + "," + s[len(s)-3:]
	}
	return s
}
