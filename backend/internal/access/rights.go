// Package access holds roles, sub-rights, sensitive-field masking, data scope and sign-in rules (spec 6).
package access

import "strings"

var Roles = []string{
	"HR Admin", "HRBP", "HRBP Corporate", "Line Manager", "Property Manager", "Cluster Manager", "City Head",
	"Operations Head", "Function Head", "Payroll", "Payroll Manager", "CHRO", "Finance Head", "Finance", "MIS Executive", "Employee",
}

// Assignable reports whether a role can be mapped. Employee and Line Manager come from the employee record.
func Assignable(role string) bool {
	if role == "Employee" || role == "Line Manager" {
		return false
	}
	for _, r := range Roles {
		if r == role {
			return true
		}
	}
	return false
}

type Right struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Confidential bool   `json:"confidential"`
}

// Rights catalogue. Levels: 0 No access, 1 View, 2 Edit.
var Rights = []Right{
	{"dash.overview", "Dashboard", false}, {"emp.directory", "Employee directory and job details", false},
	{"emp.personal", "Personal details", true}, {"emp.govid", "Aadhaar and PAN", true}, {"emp.bank", "Bank details", true},
	{"emp.salary", "Salary and CTC", true}, {"emp.medical", "Medical and nominations", true}, {"emp.status", "Mark inactive or reactivate", false},
	{"emp.export", "Export employee data", false}, {"emp.assets", "Employee dues", false}, {"emp.sep", "Separation", false},
	{"onb.pipeline", "Offers and joining details", false}, {"onb.raise", "Raise a new offer", false}, {"onb.setup", "Onboarding setup", false},
	{"comp.bands", "Pay bands", true}, {"comp.budget", "Budgets", false}, {"comp.structure", "Salary structures and LWF", true}, {"comp.approve", "Publish pay changes", true},
	{"appr.queue", "Requests and approvals", false}, {"appr.raise", "Raise requests for others", false}, {"appr.setup", "Approval flows", false},
	{"att.records", "Attendance records and upload", false}, {"att.setup", "Punch machine setup", false}, {"rep.reports", "Reports (Edit = download)", false},
	{"ros.roster", "Shifts and rosters", false}, {"ros.publish", "Approve and publish rosters", false},
	{"pay.run", "Payroll run", true}, {"pay.approve", "Approve payroll", true}, {"pay.setup", "Payroll settings", true}, {"pay.export", "Export payroll files", true}, {"pay.claims", "Claims, arrears and changes", true},
	{"pol.policies", "Work policies", false}, {"mas.org", "Masters: organisation", false}, {"mas.job", "Masters: job", false},
	{"bulk.update", "Bulk update", false}, {"acc.roles", "Roles and rights", false}, {"acc.users", "Role mapping", false}, {"ess.me", "My pay and profile", false},
	// Sending employees to the Realtime portal (create, update, mark left; HR confirms each send). HR Admin only by default.
	{"att.portal", "Realtime portal: send employees", false},
	// Staff accommodation (workforce plan, module 4): units, leases and costs; who stays where. HR Admin only by default.
	{"hst.setup", "Accommodation: units, leases and costs", false}, {"hst.allot", "Accommodation: check-in and check-out", false},
	// Cost and budget console (workforce plan, module 6): view the month's cost; edit the budgets. HR Admin only by default.
	{"cost.view", "Cost and budget", true}, {"cost.budget", "Cost and budget: budgets", false},
}

func KnownRight(id string) bool {
	for _, r := range Rights {
		if r.ID == id {
			return true
		}
	}
	return false
}

func lv(extra map[string]int) map[string]int {
	m := map[string]int{"ess.me": 2, "dash.overview": 1}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// with copies m with one right set to a level.
func with(m map[string]int, id string, level int) map[string]int {
	out := map[string]int{id: level}
	for k, v := range m {
		if k != id {
			out[k] = v
		}
	}
	return out
}

func all2() map[string]int {
	m := map[string]int{}
	for _, r := range Rights {
		m[r.ID] = 2
	}
	return m
}

var payrollRights = map[string]int{"rep.reports": 2, "emp.directory": 1, "emp.personal": 1, "emp.govid": 1, "emp.bank": 1, "emp.salary": 1, "emp.export": 2, "emp.assets": 2, "emp.sep": 1,
	"comp.bands": 2, "comp.budget": 2, "comp.structure": 2, "comp.approve": 2, "appr.queue": 2, "att.records": 1,
	"pay.run": 2, "pay.approve": 2, "pay.setup": 2, "pay.export": 2, "pay.claims": 2}

// DefaultRights per role (seed); editable in Access control. They follow the prototype's role definitions
// (only HR Admin manages work policies and masters; Finance, Finance Head and CHRO view employee dues and
// claims), except where the spec or a control differs: Payroll runs payroll and the Payroll Manager approves
// it, and Finance and Finance Head do not download by default (spec 6.3).
var DefaultRights = map[string]map[string]int{
	"HR Admin": all2(),
	"HRBP": lv(map[string]int{"rep.reports": 2, "emp.directory": 2, "emp.personal": 2, "emp.govid": 2, "emp.export": 2, "emp.assets": 2, "emp.sep": 2,
		"onb.pipeline": 2, "onb.raise": 2, "appr.queue": 2, "appr.raise": 2, "att.records": 2, "ros.roster": 2, "ros.publish": 2, "comp.budget": 1}),
	"HRBP Corporate": lv(map[string]int{"rep.reports": 2, "emp.directory": 2, "emp.salary": 1, "emp.export": 2, "emp.assets": 2, "emp.sep": 2, "onb.pipeline": 2, "onb.raise": 2,
		"comp.bands": 1, "comp.budget": 1, "comp.structure": 1, "comp.approve": 2, "appr.queue": 2, "appr.raise": 2, "att.records": 1}),
	"Line Manager":     lv(map[string]int{"emp.directory": 1, "appr.queue": 2, "appr.raise": 2, "att.records": 1, "ros.roster": 2}),
	"Property Manager": lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.sep": 1, "appr.queue": 2, "appr.raise": 2, "att.records": 1, "ros.roster": 2, "ros.publish": 2}),
	"Cluster Manager":  lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.sep": 1, "appr.queue": 2, "att.records": 1, "ros.roster": 2, "ros.publish": 2}),
	"City Head":        lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.sep": 1, "appr.queue": 2, "att.records": 1, "ros.roster": 2, "ros.publish": 2}),
	"Operations Head":  lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.salary": 1, "emp.sep": 1, "appr.queue": 2, "att.records": 1, "ros.roster": 1}),
	"Function Head":    lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.sep": 1, "appr.queue": 2, "appr.raise": 2, "att.records": 1, "ros.roster": 1}),
	"Payroll":          lv(with(payrollRights, "pay.approve", 1)), // approve and publish belong to the Payroll Manager
	"Payroll Manager":  lv(payrollRights),
	"CHRO":             lv(map[string]int{"rep.reports": 2, "emp.directory": 1, "emp.salary": 1, "emp.export": 2, "emp.sep": 1, "comp.bands": 1, "comp.budget": 1, "comp.structure": 1, "comp.approve": 2, "appr.queue": 2, "att.records": 1, "pay.run": 1, "pay.approve": 2, "pay.claims": 1, "emp.assets": 1}),
	"Finance Head":     lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.salary": 1, "emp.bank": 1, "emp.sep": 1, "comp.bands": 1, "comp.budget": 1, "comp.structure": 1, "appr.queue": 2, "pay.run": 1, "pay.claims": 1, "emp.assets": 1}),
	"Finance":          lv(map[string]int{"rep.reports": 1, "emp.directory": 1, "emp.salary": 1, "emp.bank": 1, "emp.sep": 1, "comp.bands": 1, "comp.budget": 1, "comp.structure": 1, "pay.run": 1, "pay.claims": 1, "emp.assets": 1}),
	"MIS Executive":    lv(map[string]int{"att.records": 2}),
	"Employee":         lv(map[string]int{"att.records": 1, "ros.roster": 1}), // own attendance and roster
}

// Sensitive groups and modes.
const (
	Full   = "Full"
	Masked = "Masked"
	Hidden = "Hidden"
)

var SensitiveGroups = []string{"salary", "govId", "bank", "medical"}

func s4(salary, gov, bank, med string) map[string]string {
	return map[string]string{"salary": salary, "govId": gov, "bank": bank, "medical": med}
}

// DefaultSensitive is the seed matrix (spec 6.3).
var DefaultSensitive = map[string]map[string]string{
	"HR Admin": s4(Full, Full, Full, Full), "HRBP": s4(Masked, Full, Masked, Masked), "HRBP Corporate": s4(Full, Masked, Masked, Masked),
	"Line Manager": s4(Hidden, Hidden, Hidden, Hidden), "Property Manager": s4(Hidden, Hidden, Hidden, Hidden),
	"Cluster Manager": s4(Masked, Hidden, Hidden, Hidden), "City Head": s4(Masked, Hidden, Hidden, Hidden), "Function Head": s4(Masked, Hidden, Hidden, Hidden),
	"Operations Head": s4(Full, Hidden, Hidden, Hidden), "Payroll": s4(Full, Full, Full, Masked), "Payroll Manager": s4(Full, Full, Full, Masked),
	"CHRO": s4(Full, Hidden, Hidden, Hidden), "Finance Head": s4(Full, Masked, Full, Hidden), "Finance": s4(Full, Masked, Full, Hidden),
	"MIS Executive": s4(Hidden, Hidden, Hidden, Hidden), "Employee": s4(Hidden, Hidden, Hidden, Hidden),
}

// NoDownloadByDefault: download allowed for every role except these (spec 6.3).
var NoDownloadByDefault = map[string]bool{"Operations Head": true, "Finance": true, "Finance Head": true}

// MaskedMoney is how a masked salary figure reads: nothing of the amount is shown (ACC-24). Showing the last
// four digits, as for account and ID numbers, would give away any salary under a lakh.
const MaskedMoney = "₹ ••••"

// MaskGroup applies Full / Masked / Hidden for a sensitive group: salary figures mask completely, every
// other group (government IDs, bank, medical) keeps the last four characters as Mask does.
func MaskGroup(group string, v any, mode string) any {
	if group == "salary" && mode == Masked && v != nil && strings.TrimSpace(toString(v)) != "" {
		return MaskedMoney
	}
	return Mask(v, mode)
}

// Mask applies Full / Masked / Hidden to a value. Hidden returns nil.
func Mask(v any, mode string) any {
	if v == nil || mode == Full {
		return v
	}
	if mode == Hidden {
		return nil
	}
	s := strings.TrimSpace(toString(v))
	if s == "" {
		return v
	}
	if len(s) <= 4 {
		return "••••"
	}
	n := len(s) - 4
	if n > 8 {
		n = 8
	}
	return strings.Repeat("•", n) + s[len(s)-4:]
}

// Each confidential group's view right follows the sensitive matrix: a role that sees a group
// Full or Masked can view it (if it can see the directory at all); Hidden means no access.
var groupRight = map[string]string{"salary": "emp.salary", "govId": "emp.govid", "bank": "emp.bank", "medical": "emp.medical"}

func init() {
	for role, modes := range DefaultSensitive {
		r := DefaultRights[role]
		if r["emp.directory"] < 1 {
			continue
		}
		for g, mode := range modes {
			if mode != Hidden && r[groupRight[g]] < 1 {
				r[groupRight[g]] = 1
			}
		}
		if modes["govId"] != Hidden && r["emp.personal"] < 1 {
			r["emp.personal"] = 1
		}
	}
}

// RoleDescriptions is what each built-in role is for, shown under Roles and rights.
var RoleDescriptions = map[string]string{
	"HR Admin":         "Configures the system and manages all employee data",
	"HRBP":             "Runs hiring and employee records for a cluster of 5 to 6 properties",
	"Line Manager":     "Raises requests and builds rosters for own team",
	"Property Manager": "Lead for one property. Approves team requests and rosters",
	"Cluster Manager":  "Runs a cluster of 5 to 6 properties in a city",
	"City Head":        "Runs all clusters in a city. Reports to the Operations Head",
	"Operations Head":  "Heads operations across cities in a region",
	"Function Head":    "Heads a function. Approves for operations and corporate teams in the department",
	"HRBP Corporate":   "Approves cost for new hires. HRBPs report to this role",
	"Payroll":          "Corporate payroll team. Verifies joining details and runs payroll",
	"Payroll Manager":  "Payroll head. Gives the second approval on joining details and runs payroll",
	"CHRO":             "Final approver for corporate flow and senior exceptions",
	"Finance Head":     "CFO. Final approver on large payments, full and final settlements and arrears",
	"Finance":          "Reviews cost and reports",
	"Employee":         "Self-service: own leave, regularization and resignation requests",
	"MIS Executive":    "Reads reports and dashboards",
}
