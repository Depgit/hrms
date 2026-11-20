package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/saltstayz/hrms/internal/store"
)

// The change reasons and the attendance mode the app writes itself can be renamed in Masters: the app finds them by
// a fixed key and writes the new name from then on (owner's request, 11 Nov 2026).
func TestMasterAppNamesRenamable(t *testing.T) {
	c, _, st := setupFull(t)
	ctx := context.Background()
	for _, e := range []string{"EMP-1002", "EMP-1011", "EMP-1004"} {
		c.signIn(e)
	}
	id := func(kind, usedFor, name string) string {
		t.Helper()
		var use any = usedFor
		if usedFor == "" { // no use: null or empty
			use = bson.M{"$in": bson.A{nil, ""}}
		}
		r, err := st.DB.C("simple_master").FindOne(ctx, bson.M{"kind": kind, "used_for": use, "name": name})
		if err != nil || r == nil {
			t.Fatalf("%s %s %s: %v (found %v)", kind, usedFor, name, err, r)
		}
		return fmt.Sprint(r["id"])
	}
	rename := func(kind, usedFor, from, to string) {
		t.Helper()
		if s, r := c.do("PATCH", "/api/masters/"+kind+"/"+id(kind, usedFor, from), "EMP-1002", "HR Admin", map[string]any{"name": to}); s != 200 {
			t.Fatalf("rename %s to %s: %d %v", from, to, s, r)
		}
	}

	// Every listed item has its key after seeding.
	keyed, err := st.DB.C("simple_master").Count(ctx, bson.M{"sys_key": bson.M{"$ne": nil}})
	if err != nil || keyed != int64(len(store.SysKeys)) {
		t.Fatalf("keyed items %d (%v), want %d", keyed, err, len(store.SysKeys))
	}

	rename("change_reason", "Employee update", "Transfer", "Transfer of place")
	rename("change_reason", "Deduction", "Excess payment recovery", "Overpayment recovery")
	rename("change_reason", "Hold", "Absent without notice", "Absent, no notice")
	rename("change_reason", "Separation", "Terminated", "Dismissed")
	rename("attendance_mode", "", "Biometric", "Biometric punch")

	t.Run("the list says what the app uses it for, without the key", func(t *testing.T) {
		_, l := c.do("GET", "/api/masters/change_reason", "EMP-1002", "HR Admin", nil)
		found := false
		for _, x := range l["list"].([]any) {
			m := x.(map[string]any)
			if _, has := m["sys_key"]; has {
				t.Fatalf("key sent to the screen: %v", m)
			}
			if m["name"] == "Transfer of place" {
				found = strings.Contains(fmt.Sprint(m["appUse"]), "approved transfers") && m["fixed"] == nil
			}
		}
		if !found {
			t.Fatal("renamed Transfer not marked as used by the app")
		}
		if s, r := c.do("PATCH", "/api/masters/change_reason/"+id("change_reason", "Employee update", "Transfer of place"), "EMP-1002", "HR Admin",
			map[string]any{"used_for": "Salary"}); s != 400 || !strings.Contains(fmt.Sprint(r["error"]), "cannot change") {
			t.Fatalf("moving it to another use: %d %v", s, r)
		}
	})

	t.Run("the app writes the new names", func(t *testing.T) {
		for key, want := range map[string]string{"reason.transfer": "Transfer of place", "reason.excess_recovery": "Overpayment recovery",
			"reason.absent_hold": "Absent, no notice", "sep:Termination": "Dismissed", "mode.default": "Biometric punch", "sep:Resignation": "Resigned"} {
			if got := store.SysName(ctx, st.DB, key); got != want {
				t.Fatalf("%s: %q, want %q", key, got, want)
			}
		}
		left, _ := st.DB.C("employee").Count(ctx, bson.M{"attendance_mode": "Biometric"})
		if left != 0 {
			t.Fatalf("%d employees still on the old mode name", left)
		}
	})

	t.Run("a recovery of excess pay under its new name needs no hearing", func(t *testing.T) {
		_, m := c.do("GET", "/api/pay-items/deductions", "EMP-1011", "Payroll", nil)
		if m["noHearingType"] != "Overpayment recovery" {
			t.Fatalf("deductions meta %v", m["noHearingType"])
		}
		if s, r := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 500, "instalments": 1, "dedType": "Overpayment recovery", "note": "Salary paid twice in error"}}); s != 200 {
			t.Fatalf("recovery without a hearing: %d %v", s, r)
		}
		if s, _ := c.do("POST", "/api/approvals", "EMP-1011", "Payroll", map[string]any{"wf": "p_ded", "employeeId": "EMP-1020",
			"payload": map[string]any{"amount": 500, "instalments": 1, "dedType": "Cash shortage", "note": "Shortage at the till on 3 Oct"}}); s != 400 {
			t.Fatal("other deductions still need a hearing")
		}
	})

	t.Run("absconding holds the salary, and a termination records the reason, under the new names", func(t *testing.T) {
		_, h := c.do("GET", "/api/holds", "EMP-1011", "Payroll", nil)
		if h["absentReason"] != "Absent, no notice" {
			t.Fatalf("holds meta %v", h["absentReason"])
		}
		if s, a := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1022", "type": "Absconding", "note": "Not reachable on phone since Monday"}); s != 200 {
			t.Fatalf("absconding: %d %v", s, a)
		}
		hold, err := st.DB.C("salary_hold").FindOne(ctx, bson.M{"employee_id": "EMP-1022", "status": "Active"})
		if reason, _ := hold["reason"].(string); err != nil || reason != "Absent, no notice" {
			t.Fatalf("hold reason %q %v", reason, err)
		}
		s, r := c.do("POST", "/api/separations", "EMP-1004", "HRBP", map[string]any{"employeeId": "EMP-1016", "type": "Termination", "reason": "Misconduct",
			"noticeOption": "pay", "note": "Repeated cash shortages after two warnings"})
		if s != 200 || r["requestId"] == nil {
			t.Fatalf("termination: %d %v", s, r)
		}
		c.approveAll(t, r["requestId"].(string))
		emp, err := st.DB.C("employee").FindOne(ctx, bson.M{"_id": "EMP-1016"})
		if exit, _ := emp["exit_reason"].(string); err != nil || emp == nil || !strings.HasPrefix(exit, "Dismissed") {
			t.Fatalf("exit reason %q %v", exit, err)
		}
	})
}
