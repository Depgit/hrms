package access

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case *string:
		if x == nil {
			return ""
		}
		return *x
	case float64:
		return fmt.Sprintf("%.0f", x)
	}
	return fmt.Sprint(v)
}

// Scope decides whose records a caller sees (spec 6.4). Enforced on the server for every list.
type Scope struct {
	Access      string   // all | locs | team | self
	Locations   []string // location ids for "locs"
	Departments []string // optional department-name limit
	EmployeeID  string   // the caller (team = their reports, self = themselves)
}

type ScopedEmployee struct{ ID, LocationID, Department, ManagerID string }

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func (s Scope) Allows(e ScopedEmployee) bool {
	var ok bool
	switch s.Access {
	case "all":
		ok = true
	case "locs":
		ok = contains(s.Locations, e.LocationID)
	case "team":
		ok = e.ManagerID != "" && e.ManagerID == s.EmployeeID
	default:
		ok = e.ID == s.EmployeeID
	}
	return ok && (len(s.Departments) == 0 || contains(s.Departments, e.Department))
}

// SQL builds the equivalent filter over `employee e JOIN department d`, appending to args.
func (s Scope) SQL(args *[]any, e, d string) string {
	p := func(v any) string { *args = append(*args, v); return fmt.Sprintf("$%d", len(*args)) }
	var base string
	switch s.Access {
	case "all":
		base = "TRUE"
	case "locs":
		base = fmt.Sprintf("%s.location_id = ANY(%s::uuid[])", e, p(s.Locations))
	case "team":
		base = fmt.Sprintf("%s.manager_id = %s", e, p(s.EmployeeID))
	default:
		base = fmt.Sprintf("%s.id = %s", e, p(s.EmployeeID))
	}
	if len(s.Departments) > 0 {
		return fmt.Sprintf("(%s AND %s.name = ANY(%s::text[]))", base, d, p(s.Departments))
	}
	return "(" + base + ")"
}

// ---------- Sign-in rules (spec 6.5) ----------

var digits = regexp.MustCompile(`\D`)

// NormaliseEmployeeID turns "1001" into "EMP-1001"; anything else is upper-cased.
func NormaliseEmployeeID(raw string) string {
	s := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsDigit(r) }) < 0 {
		return "EMP-" + s
	}
	return s
}

func MaskMobile(m string) string {
	d := digits.ReplaceAllString(m, "")
	if len(d) < 4 {
		return ""
	}
	n := len(d) - 4
	if n < 2 {
		n = 2
	}
	return d[:2] + strings.Repeat("*", n) + d[len(d)-2:]
}

// PasswordProblems lists the rules a password breaks.
func PasswordProblems(p, employeeID, mobile string) []string {
	var out []string
	has := func(f func(rune) bool) bool { return strings.IndexFunc(p, f) >= 0 }
	if len([]rune(p)) < 8 {
		out = append(out, "At least 8 characters")
	}
	if !has(unicode.IsUpper) {
		out = append(out, "An upper case letter")
	}
	if !has(unicode.IsLower) {
		out = append(out, "A lower case letter")
	}
	if !has(unicode.IsDigit) {
		out = append(out, "A number")
	}
	if !has(func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		out = append(out, "A special character")
	}
	m := digits.ReplaceAllString(mobile, "")
	if strings.Contains(strings.ToUpper(p), strings.ToUpper(employeeID)) || (len(m) >= 6 && strings.Contains(p, m[len(m)-6:])) {
		out = append(out, "Not your Employee ID or mobile number")
	}
	return out
}
