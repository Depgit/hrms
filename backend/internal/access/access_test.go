package access

import "testing"

func TestScope(t *testing.T) {
	e := ScopedEmployee{ID: "EMP-1", LocationID: "L1", Department: "F&B", ManagerID: "EMP-9"}
	cases := []struct {
		s    Scope
		want bool
	}{
		{Scope{Access: "locs", Locations: []string{"L1"}}, true},
		{Scope{Access: "locs", Locations: []string{"L1"}, Departments: []string{"Sales"}}, false},
		{Scope{Access: "team", EmployeeID: "EMP-9"}, true},
		{Scope{Access: "self", EmployeeID: "EMP-2"}, false},
		{Scope{Access: "all"}, true},
	}
	for i, c := range cases {
		if c.s.Allows(e) != c.want {
			t.Errorf("case %d", i)
		}
	}
	var args []any
	if got := (Scope{Access: "locs", Locations: []string{"L1"}, Departments: []string{"F&B"}}).SQL(&args, "e", "d"); got != "(e.location_id = ANY($1::uuid[]) AND d.name = ANY($2::text[]))" || len(args) != 2 {
		t.Fatal(got)
	}
}

func TestSignIn(t *testing.T) {
	if NormaliseEmployeeID(" 1001 ") != "EMP-1001" || NormaliseEmployeeID("prm-00012") != "PRM-00012" {
		t.Fatal("normalise")
	}
	if MaskMobile("9810012345") != "98******45" {
		t.Fatal(MaskMobile("9810012345"))
	}
	if p := PasswordProblems("Good#Pass1", "EMP-1001", "9810012345"); len(p) != 0 {
		t.Fatal(p)
	}
	if p := PasswordProblems("xEmp-1001#a", "EMP-1001", ""); len(p) != 1 || p[0] != "Not your Employee ID or mobile number" {
		t.Fatal(p)
	}
}

func TestMask(t *testing.T) {
	if Mask("ABCPX1234K", Masked) != "••••••234K" || Mask("x", Hidden) != nil || Mask("ABC", Full) != "ABC" {
		t.Fatal(Mask("ABCPX1234K", Masked))
	}
	for _, r := range Roles {
		if DefaultRights[r] == nil || DefaultSensitive[r] == nil {
			t.Fatal("missing defaults for", r)
		}
	}
}

func TestRightsFollowSensitiveMatrix(t *testing.T) {
	if DefaultRights["HRBP"]["emp.bank"] != 1 || DefaultRights["HRBP"]["emp.salary"] != 1 || DefaultRights["Cluster Manager"]["emp.salary"] != 1 {
		t.Fatal("masked groups must be viewable")
	}
	if DefaultRights["Line Manager"]["emp.salary"] != 0 || DefaultRights["Property Manager"]["emp.bank"] != 0 {
		t.Fatal("hidden groups must stay hidden")
	}
	if DefaultRights["HRBP"]["emp.govid"] != 2 {
		t.Fatal("explicit edit right kept")
	}
}

// ACC-24: a masked salary shows nothing of the amount; account and ID numbers keep their last four.
func TestMaskSalaryFully(t *testing.T) {
	if got := MaskGroup("salary", 16673, Masked); got != MaskedMoney {
		t.Fatalf("salary masked as %v", got)
	}
	if got := MaskGroup("salary", "15063.4", Masked); got != MaskedMoney {
		t.Fatalf("salary masked as %v", got)
	}
	if got := MaskGroup("bank", "123456786540", Masked); got != "••••••••6540" {
		t.Fatalf("bank masked as %v", got)
	}
	if MaskGroup("salary", 100, Hidden) != nil || MaskGroup("salary", 100, Full) != 100 {
		t.Fatal("hidden/full")
	}
}
