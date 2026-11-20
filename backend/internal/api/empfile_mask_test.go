package api_test

import "testing"

// The employee file opens for every directory role, but pay figures reach only roles that see salary in full.
func TestEmployeeFileMasking(t *testing.T) {
	c, _, _ := setupFull(t)
	signAll(c, "EMP-1002", "EMP-1004", "EMP-1011", "EMP-1009")
	for _, x := range []struct {
		emp, role string
		pay       bool
	}{{"EMP-1011", "Payroll", true}, {"EMP-1002", "HR Admin", true}, {"EMP-1004", "HRBP", false}, {"EMP-1009", "Line Manager", false}} {
		s, r := c.do("GET", "/api/employees/EMP-1020/file", x.emp, x.role, nil)
		if s != 200 {
			t.Fatalf("%s opens the file: %d %v", x.role, s, r)
		}
		if items, _ := r["items"].([]any); len(items) == 0 {
			t.Errorf("%s sees the readiness items", x.role)
		}
		hasRow := r["row"] != nil
		lines, _ := r["lines"].([]any)
		slips, _ := r["payslips"].([]any)
		if hasRow != x.pay || (!x.pay && (len(lines) > 0 || len(slips) > 0)) {
			t.Errorf("%s: pay figures shown=%v, want %v (lines %d, payslips %d)", x.role, hasRow, x.pay, len(lines), len(slips))
		}
	}
}
