package api

import (
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/saltstayz/hrms/internal/jobs"
	"github.com/saltstayz/hrms/internal/mail"
	"github.com/saltstayz/hrms/internal/mdm"
	"github.com/saltstayz/hrms/internal/storage"
	"github.com/saltstayz/hrms/internal/store"
)

type Server struct {
	st         *store.Store
	jobs       *jobs.Client // background job queue (jobs.go); nil until StartJobs
	devOTP     bool
	sms        SMSSender
	files      storage.Store
	finalizers map[string]Finalizer
	drive      *driveService // Google Drive document storage (drive.go)
	mdm        *mdm.Service  // master data sync, SOP-ARCH-MDM-01 (mdm_routes.go)
	mail       mail.Sender   // the company mailbox (joining_link.go); nil when email is not set up
	appURL     string        // the web address used in links sent by email
	joinGuard  *joinLimiter  // rate limits of the public joiner pages (joining_link.go)
	authGuard  *authLimiter  // sign-in and OTP limits per address (auth_limit.go)
	trusted    []*net.IPNet  // proxies whose X-Forwarded-For is believed (TRUSTED_PROXIES)
}

type Options struct {
	DevOTP bool          // return the OTP in the response (development only)
	SMS    SMSSender     // nil = no SMS provider configured
	Files  storage.Store // uploaded documents; defaults to ./data/uploads on local disk
	// SecretKey (32 bytes, see SecretKey) encrypts the stored Google refresh token; nil = from APP_SECRET, else a
	// key for this process only (tests).
	SecretKey []byte
	// Mail sends the joiner's link and code (SMTP_* settings, or the development echo); nil = email not set up.
	Mail mail.Sender
	// AppURL is the web app's address for links in emails (APP_URL); default http://localhost:5173.
	AppURL string
	// TrustedProxies are the addresses or ranges of the proxies in front of the API (TRUSTED_PROXIES), whose
	// X-Forwarded-For names the caller for the joiner pages' rate limits; nil = the machine itself (loopback), an
	// empty list = none.
	TrustedProxies []string
}

func New(st *store.Store, o Options) *Server {
	if o.Files == nil {
		o.Files = storage.Local{Root: "data/uploads"}
	}
	if o.AppURL == "" {
		o.AppURL = "http://localhost:5173"
	}
	if o.TrustedProxies == nil {
		o.TrustedProxies = defaultTrustedProxies
	}
	trusted, err := parseProxies(o.TrustedProxies)
	if err != nil {
		panic(err) // main checks TRUSTED_PROXIES with ParseTrustedProxies first
	}
	s := &Server{st: st, devOTP: o.DevOTP, sms: o.SMS, files: o.Files, finalizers: map[string]Finalizer{}, drive: newDriveService(o.SecretKey),
		mail: o.Mail, appURL: strings.TrimRight(o.AppURL, "/"), joinGuard: newJoinLimiter(), authGuard: newAuthLimiter(o.DevOTP), trusted: trusted}
	s.registerFinalizers()
	s.mdmInit()
	return s
}

func (s *Server) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", s.h(func(r *http.Request) (any, error) { return map[string]string{"status": "ok"}, nil }))

	// Sign-in: Employee ID + OTP to the registered mobile, then password.
	m.HandleFunc("POST /api/auth/otp", s.h(s.otpSend))
	m.HandleFunc("POST /api/auth/otp/verify", s.h(s.otpVerify))
	m.HandleFunc("POST /api/auth/password", s.h(s.setPassword))
	m.HandleFunc("POST /api/auth/login", s.h(s.login))
	m.HandleFunc("POST /api/auth/logout", s.a(s.logout))
	m.HandleFunc("GET /api/me", s.a(s.me))
	m.HandleFunc("GET /api/dashboard", s.a(s.dashboard))
	m.HandleFunc("GET /api/me/requests", s.a(s.myRequests))
	m.HandleFunc("GET /api/me/separation", s.a(s.mySeparation))
	m.HandleFunc("GET /api/me/dashboard", s.a(s.myDashboard)) // My view: own figures (myview.go)
	m.HandleFunc("GET /api/me/directory", s.a(s.myDirectory)) // My view: a department's org chart (mydirectory.go)
	m.HandleFunc("GET /api/me/advance", s.a(s.myAdvance))     // My pay › Salary advance (advances.go)
	m.HandleFunc("GET /api/lookups", s.a(s.lookups))
	// My view › Employee directory: everyone with email (searched, paged) and departments with headcount.
	m.HandleFunc("GET /api/me/directory/people", s.a(s.myDirectoryPeople))
	m.HandleFunc("GET /api/me/directory/departments", s.a(s.myDirectoryDepartments))

	// Masters: no delete; inactive with a date and a reason.
	m.HandleFunc("GET /api/masters", s.a(s.masterTypes))
	m.HandleFunc("GET /api/masters/{type}", s.a(s.masterList))
	m.HandleFunc("POST /api/masters/{type}", s.a(s.masterCreate))
	m.HandleFunc("PATCH /api/masters/{type}/{id}", s.a(s.masterUpdate))
	m.HandleFunc("POST /api/masters/{type}/{id}/inactive", s.a(s.masterInactive))
	m.HandleFunc("POST /api/masters/{type}/{id}/reactivate", s.a(s.masterReactivate))
	// Master update: one workbook, a sheet per master (master_book.go).
	m.HandleFunc("GET /api/master-update/download", s.masterBookDownload)
	m.HandleFunc("POST /api/master-update/{action}", s.a(s.masterBookRun))
	// Set shift: timings and attendance rules by effective date (Masters › Shifts).
	m.HandleFunc("GET /api/shifts/{id}", s.a(s.shiftGet))
	m.HandleFunc("PUT /api/shifts/{id}", s.a(s.shiftPut))
	m.HandleFunc("POST /api/shifts", s.a(s.shiftCreate))
	m.HandleFunc("POST /api/shifts/{id}/preview", s.a(s.shiftPreview))

	// Employees.
	m.HandleFunc("GET /api/employees", s.a(s.employeeList))
	m.HandleFunc("GET /api/employees/next-code", s.a(s.nextCode))
	m.HandleFunc("GET /api/employees/export", s.employeeExport)
	m.HandleFunc("GET /api/employees/export/options", s.a(s.exportOptions))
	m.HandleFunc("GET /api/employee-codes", s.a(s.codeSettingsGet))
	m.HandleFunc("PUT /api/employee-codes", s.a(s.codeSettingsPut))
	m.HandleFunc("PUT /api/employees/{id}/pay", s.a(s.employeePayPut))
	m.HandleFunc("POST /api/employees/import", s.a(s.employeeImport))
	m.HandleFunc("GET /api/employees/{id}", s.a(s.employeeGet))
	m.HandleFunc("GET /api/employees/{id}/history", s.a(s.employeeHistory))
	m.HandleFunc("PATCH /api/employees/{id}/job", s.a(s.employeeJob))
	m.HandleFunc("PATCH /api/employees/{id}/personal", s.a(s.employeePersonal))
	m.HandleFunc("POST /api/employees/{id}/inactive", s.a(s.employeeInactive))

	// Access control.
	m.HandleFunc("GET /api/rights", s.a(s.rightsCatalogue))
	m.HandleFunc("GET /api/roles", s.a(s.rolesList))
	m.HandleFunc("POST /api/roles", s.a(s.roleAdd))
	m.HandleFunc("PATCH /api/roles/{name}", s.a(s.rolePatch))
	m.HandleFunc("PUT /api/roles/{name}/rights", s.a(s.roleRights))
	m.HandleFunc("GET /api/access/person/{id}", s.a(s.accessPerson))
	m.HandleFunc("GET /api/role-map", s.a(s.roleMapList))
	m.HandleFunc("POST /api/role-map", s.a(s.roleMapCreate))
	m.HandleFunc("PATCH /api/role-map/{id}", s.a(s.roleMapUpdate))
	m.HandleFunc("GET /api/audit", s.a(s.auditLog))

	// Approvals: one engine for every workflow.
	m.HandleFunc("GET /api/approvals", s.a(s.approvalQueue))
	m.HandleFunc("POST /api/approvals", s.a(s.approvalRaise))
	m.HandleFunc("POST /api/approvals/simulate", s.a(s.approvalSimulate))
	m.HandleFunc("GET /api/approvals/flows", s.a(s.flowsAll))
	m.HandleFunc("PUT /api/approvals/flows/{wf}/{flow}", s.a(s.flowPut))
	m.HandleFunc("POST /api/approvals/flows/{wf}/{flow}/test", s.a(s.flowTest))
	m.HandleFunc("PUT /api/approvals/flows/{wf}/{flow}/hierarchy", s.a(s.flowHierarchy))
	m.HandleFunc("GET /api/approvals/counts", s.a(s.approvalCounts))
	m.HandleFunc("GET /api/approvals/settings", s.a(s.approvalSettings))
	m.HandleFunc("PUT /api/approvals/settings", s.a(s.approvalSettingsPut))
	m.HandleFunc("GET /api/approvals/approvers", s.a(s.approverNames))
	m.HandleFunc("GET /api/approvals/health", s.a(s.approvalHealth))
	m.HandleFunc("GET /api/approvals/columns", s.a(s.approvalColumnsGet))
	m.HandleFunc("POST /api/approvals/{id}/escalate", s.a(s.approvalEscalateNow))
	// Joining verification: documents and corrections (verify_edit.go); bulk approval by Excel or ticks (approval_bulk.go).
	m.HandleFunc("GET /api/approvals/{id}/verify-docs", s.a(s.verifyDocList))
	m.HandleFunc("GET /api/approvals/{id}/verify-docs/{doc}", s.verifyDocGet)
	m.HandleFunc("POST /api/approvals/{id}/verify-docs", s.a(s.verifyDocUpload))
	m.HandleFunc("PUT /api/approvals/{id}/verify-details", s.a(s.verifyDetailsPut))
	m.HandleFunc("PUT /api/approvals/{id}/advance-terms", s.a(s.advanceTerms)) // Payroll changes an advance's recovery (advances.go)
	// PF for new joiners: the exemption and the UAN check (pfuan.go, 14 Nov 2026).
	m.HandleFunc("GET /api/payroll/pf-settings", s.a(s.pfSettings))
	m.HandleFunc("PUT /api/payroll/pf-settings", s.a(s.pfSettingsPut))
	m.HandleFunc("GET /api/employees/{id}/pf-exempt", s.a(s.pfExemptGet))
	m.HandleFunc("PUT /api/employees/{id}/pf-exempt", s.a(s.pfExemptPut))
	m.HandleFunc("PUT /api/employees/{id}/pf-choice", s.a(s.pfChoicePut))
	m.HandleFunc("GET /api/approvals/bulk/export", s.bulkExport)
	m.HandleFunc("POST /api/approvals/bulk/preview", s.a(s.bulkPreview))
	m.HandleFunc("POST /api/approvals/bulk/apply", s.a(s.bulkApply))
	m.HandleFunc("POST /api/approvals/bulk/apply-file", s.a(s.bulkApplyFile))
	// Onboarding (spec 8) and documents.
	m.HandleFunc("GET /api/onboarding/setup", s.a(s.onbSetupGet))
	m.HandleFunc("PATCH /api/onboarding/setup/{kind}/{key}", s.a(s.onbSetupPatch))
	m.HandleFunc("POST /api/onboarding/setup/{kind}", s.a(s.onbSetupAdd))
	m.HandleFunc("POST /api/offers/check", s.a(s.offerCheck))
	m.HandleFunc("POST /api/offers", s.a(s.offerCreate))
	m.HandleFunc("GET /api/offers", s.a(s.offerList))
	m.HandleFunc("GET /api/offers/rejoin/{id}", s.a(s.rejoinPrefill))
	m.HandleFunc("GET /api/offers/{id}", s.a(s.offerGet))
	m.HandleFunc("POST /api/offers/{id}/joining", s.a(s.offerJoining))
	m.HandleFunc("POST /api/offers/{id}/send", s.a(s.offerSend))
	m.HandleFunc("POST /api/offers/{id}/{step}", s.a(s.offerStep))
	m.HandleFunc("POST /api/files", s.a(s.fileUpload))
	m.HandleFunc("GET /api/files", s.a(s.fileList))
	m.HandleFunc("GET /api/files/{id}", s.fileGet)
	m.HandleFunc("GET /api/employees/{id}/documents", s.a(s.employeeDocs))
	m.HandleFunc("GET /api/settings/company", s.a(s.companyGet))
	m.HandleFunc("PUT /api/settings/company", s.a(s.companyPut))
	// Nominations (spec 19.2).
	m.HandleFunc("GET /api/nominations/schemes", s.a(s.nomSchemes))
	m.HandleFunc("POST /api/nominations/schemes", s.a(s.nomSchemeSave))
	m.HandleFunc("PUT /api/nominations/schemes/{id}", s.a(s.nomSchemeSave))
	m.HandleFunc("GET /api/me/nominations", s.a(s.myNominations))
	m.HandleFunc("POST /api/me/nominations/{scheme}", s.a(s.nominationSubmit))
	m.HandleFunc("GET /api/nominations", s.a(s.nominationQueue))
	m.HandleFunc("POST /api/nominations/{id}/{action}", s.a(s.nominationDecide))
	// Work policies, leave plans, holiday calendars, exceptions (spec 10).
	m.HandleFunc("GET /api/policies", s.a(s.policyList))
	m.HandleFunc("POST /api/policies", s.a(s.policyCreate))
	m.HandleFunc("GET /api/policies/coverage", s.a(s.policyCoverage))
	m.HandleFunc("GET /api/policies/{id}", s.a(s.policyGet))
	m.HandleFunc("PUT /api/policies/{id}", s.a(s.policySave))
	m.HandleFunc("PUT /api/policies/{id}/maps", s.a(s.policyMaps))
	m.HandleFunc("GET /api/leave-plans", s.a(s.leavePlans))
	m.HandleFunc("POST /api/leave-plans", s.a(s.leavePlanCreate))
	m.HandleFunc("PUT /api/leave-plans/{id}", s.a(s.leavePlanSave))
	m.HandleFunc("POST /api/leave-types", s.a(s.leaveTypeAdd))
	m.HandleFunc("GET /api/calendars", s.a(s.calendars))
	m.HandleFunc("POST /api/calendars", s.a(s.calendarSave))
	m.HandleFunc("PUT /api/calendars/{id}", s.a(s.calendarSave))
	m.HandleFunc("GET /api/exceptions", s.a(s.exceptionList))
	m.HandleFunc("POST /api/exceptions", s.a(s.exceptionAdd))
	m.HandleFunc("POST /api/exceptions/{id}/revoke", s.a(s.exceptionRevoke))
	m.HandleFunc("GET /api/leave/balances/{id}", s.a(s.leaveBalances))
	// Roster, attendance, deviations, consequences (spec 11).
	m.HandleFunc("GET /api/roster", s.a(s.rosterGet))
	m.HandleFunc("POST /api/roster/cell", s.a(s.rosterCell))
	m.HandleFunc("POST /api/roster/publish", s.a(s.rosterPublish))
	m.HandleFunc("GET /api/roster/discipline", s.a(s.discipline))
	m.HandleFunc("GET /api/attendance", s.a(s.attendanceList))
	m.HandleFunc("GET /api/attendance/issues", s.a(s.attendanceIssues))
	m.HandleFunc("GET /api/attendance/day", s.a(s.attendanceDay))
	m.HandleFunc("GET /api/attendance/month/{id}", s.a(s.attendanceMonth))
	m.HandleFunc("POST /api/roster/pattern", s.a(s.rosterPattern))
	m.HandleFunc("GET /api/attendance/summary", s.a(s.attendanceSummary))
	m.HandleFunc("GET /api/attendance/pull-config", s.a(s.pullConfigGet))
	m.HandleFunc("PUT /api/attendance/pull-config", s.a(s.pullConfigPut))
	m.HandleFunc("POST /api/attendance/pull-config/test", s.a(s.pullConfigTest))
	m.HandleFunc("POST /api/attendance/pull-config/pull", s.a(s.pullNow))
	m.HandleFunc("GET /api/attendance/realtime", s.a(s.realtimeGet))
	m.HandleFunc("PUT /api/attendance/realtime", s.a(s.realtimePut))
	m.HandleFunc("POST /api/attendance/realtime/pull", s.a(s.realtimePull))
	m.HandleFunc("GET /api/attendance/realtime/match", s.a(s.realtimeMatchGet))
	m.HandleFunc("POST /api/attendance/realtime/match", s.a(s.realtimeMatchStart))
	m.HandleFunc("POST /api/attendance/realtime/match/apply", s.a(s.realtimeMatchApply))
	m.HandleFunc("GET /api/attendance/realtime/match/download", s.sheet(s.realtimeMatchSheet))
	m.HandleFunc("GET /api/attendance/alerts", s.a(s.attAlertsGet))
	m.HandleFunc("PUT /api/attendance/alerts", s.a(s.attAlertsPut))
	m.HandleFunc("POST /api/attendance/alerts/test", s.a(s.attAlertsTest))
	m.HandleFunc("GET /api/bulk/roster/download", s.rosterDownload)
	m.HandleFunc("GET /api/bulk/att/log", s.a(s.attUploadLog))
	m.HandleFunc("GET /api/deviations", s.a(s.deviationList))
	m.HandleFunc("POST /api/deviations/decide", s.a(s.deviationDecide))
	m.HandleFunc("GET /api/me/discipline", s.a(s.myDiscipline))
	m.HandleFunc("GET /api/consequences", s.a(s.consList))
	m.HandleFunc("GET /api/consequences/approver", s.a(s.consApproverGet))
	m.HandleFunc("POST /api/consequences", s.a(s.consIssue))
	m.HandleFunc("GET /api/consequences/{id}/letter", s.a(s.consLetter))
	m.HandleFunc("POST /api/consequences/{id}/{action}", s.a(s.consAction))
	// Bulk update (spec 9).
	m.HandleFunc("GET /api/bulk", s.a(s.bulkList))
	m.HandleFunc("GET /api/bulk/{tpl}/download", s.bulkDownload)
	m.HandleFunc("POST /api/bulk/{tpl}/{action}", s.a(s.bulkRun))
	s.routesB1(m)   // PAN verification, ESIC and medical cards (b1_routes.go)
	s.routesHst(m)  // staff accommodation: units, leases, stays, cost split, occupancy board (hst.go)
	s.routesCost(m) // cost and budget console: month cost tree, people, trend, headcount plan, budgets (cost.go)
	m.HandleFunc("GET /api/managers/left", s.a(s.managersLeft))
	m.HandleFunc("POST /api/managers/reassign", s.a(s.managersReassign))
	m.HandleFunc("GET /api/approvals/{id}", s.a(s.approvalGet))
	m.HandleFunc("POST /api/approvals/{id}/approve", s.a(s.approvalDecide("approve")))
	m.HandleFunc("POST /api/approvals/{id}/reject", s.a(s.approvalDecide("reject")))
	m.HandleFunc("POST /api/approvals/{id}/cancel", s.a(s.approvalCancel))
	m.HandleFunc("POST /api/approvals/{id}/skip", s.a(s.approvalSkip))      // HR's "Skip approval" (hierarchy off)
	m.HandleFunc("GET /api/approvals/skip-check", s.a(s.approvalSkipCheck)) // offer "Skip approval" on a raise form

	// Compensation.
	m.HandleFunc("GET /api/payroll", s.a(s.payrollState))
	m.HandleFunc("GET /api/payroll/register", s.a(s.payrollRegister))
	m.HandleFunc("GET /api/payroll/upcoming", s.a(s.upcoming))
	m.HandleFunc("GET /api/payroll/upcoming.xlsx", s.sheet(s.upcomingSheet))
	m.HandleFunc("GET /api/payroll/schedule", s.a(s.paySchedule))
	m.HandleFunc("PUT /api/payroll/schedule", s.a(s.payScheduleSave))
	// The run checklist (payroll_closure.go) replaces Month close: its lock, cut-off and reminders are closures now.
	m.HandleFunc("GET /api/payroll/{ym}/checklist", s.a(s.checklistGet))
	m.HandleFunc("POST /api/payroll/{ym}/closures/{key}/{action}", s.a(s.closureAct))
	// In-app messages (notices.go): each person's own, in any view.
	m.HandleFunc("GET /api/notices", s.a(s.noticeList))
	m.HandleFunc("POST /api/notices/read-all", s.a(s.noticeReadAll))
	m.HandleFunc("POST /api/notices/{id}/read", s.a(s.noticeMark("read")))
	m.HandleFunc("POST /api/notices/{id}/dismiss", s.a(s.noticeMark("dismiss")))
	m.HandleFunc("GET /api/payroll/{ym}/attendance.xlsx", s.sheet(s.payrollAttendance))
	m.HandleFunc("PUT /api/payroll/{ym}/pilot", s.a(s.payrollPilot))
	m.HandleFunc("GET /api/payroll/{ym}/rows", s.a(s.payrollRows))
	m.HandleFunc("GET /api/payroll/{ym}/rows/{emp}", s.a(s.payrollRow))
	m.HandleFunc("PATCH /api/payroll/{ym}/rows/{emp}", s.a(s.payrollEdit))
	m.HandleFunc("GET /api/payroll/{ym}/checks", s.a(s.payrollChecks))
	m.HandleFunc("GET /api/payroll/{ym}/cost", s.a(s.payrollCost))
	m.HandleFunc("GET /api/payroll/{ym}/sheet.xlsx", s.sheet(s.payrollSheet))
	m.HandleFunc("POST /api/payroll/{ym}/sheet/{action}", s.a(s.payrollUpload))
	m.HandleFunc("GET /api/payroll/{ym}/bank-advice.xlsx", s.sheet(s.payrollBank))
	m.HandleFunc("GET /api/payroll/{ym}/register.xlsx", s.sheet(s.payrollRegisterSheet))
	// ICICI's bulk payment upload and the full salary register (bankfile.go, 14 Nov 2026).
	m.HandleFunc("GET /api/payroll/{ym}/icici-upload", s.a(s.payrollICICICheck))
	m.HandleFunc("GET /api/payroll/{ym}/icici-upload.xlsx", s.book(s.payrollICICI))
	m.HandleFunc("GET /api/payroll/{ym}/full-register.xlsx", s.book(s.payrollFullRegister))
	m.HandleFunc("POST /api/payroll/{ym}/{step}", s.a(s.payrollStep))
	m.HandleFunc("GET /api/payslip-setup", s.a(s.payslipSetupGet))
	m.HandleFunc("PUT /api/payslip-setup", s.a(s.payslipSetupPut))
	m.HandleFunc("GET /api/payslip-preview", s.a(s.payslipPreview))
	m.HandleFunc("GET /api/payslips", s.a(s.payslipList))
	m.HandleFunc("GET /api/payslips/{ym}/{emp}", s.a(s.payslipGet))
	m.HandleFunc("GET /api/pay-items/{kind}", s.a(s.payItemList))
	m.HandleFunc("POST /api/advances/{id}/{action}", s.a(s.advanceAction))
	// Salary advance limits and opening advances from Excel (advances.go).
	m.HandleFunc("GET /api/advances/settings", s.a(s.advanceSettings))
	m.HandleFunc("PUT /api/advances/settings", s.a(s.advanceSettingsPut))
	m.HandleFunc("GET /api/advances/opening/template", s.openingTemplate)
	m.HandleFunc("POST /api/advances/opening/preview", s.a(s.openingPreview))
	m.HandleFunc("POST /api/advances/opening/apply", s.a(s.openingApply))
	m.HandleFunc("GET /api/holds", s.a(s.holdList))
	m.HandleFunc("POST /api/holds", s.a(s.holdAdd))
	m.HandleFunc("POST /api/holds/{id}/release", s.a(s.holdRelease))

	m.HandleFunc("GET /api/comp", s.a(s.compGet))
	m.HandleFunc("POST /api/comp/structures", s.a(s.structureCreate))
	m.HandleFunc("PUT /api/comp/structures/{id}", s.a(s.structureSave))
	m.HandleFunc("POST /api/comp/structures/{id}/default", s.a(s.structureDefault))
	m.HandleFunc("GET /api/comp/structures/{id}/maps", s.a(s.structureMaps))
	m.HandleFunc("POST /api/comp/structure-map", s.a(s.structureMapAdd))
	m.HandleFunc("GET /api/comp/structure-map/candidates", s.a(s.structureCandidates))
	m.HandleFunc("PUT /api/comp/bands", s.a(s.bandSave))
	m.HandleFunc("POST /api/comp/bands/copy", s.a(s.bandCopy))
	m.HandleFunc("PUT /api/comp/lwf/{state}", s.a(s.lwfSave))
	m.HandleFunc("PUT /api/comp/budgets/{kind}/{id}", s.a(s.budgetSave))
	m.HandleFunc("PUT /api/comp/types/{type}", s.a(s.typeRulesPut))
	m.HandleFunc("POST /api/comp/structures/{id}/status", s.a(s.structureStatus))
	m.HandleFunc("PUT /api/comp/offer-template", s.a(s.offerTemplatePut))
	m.HandleFunc("POST /api/offers/draft/{draft}/docs/{key}/{action}", s.a(s.draftDocAsk))
	m.HandleFunc("GET /api/employees/{id}/file", s.a(s.employeeFile))

	m.HandleFunc("GET /api/tax/me", s.a(s.taxMe))
	m.HandleFunc("PUT /api/tax/me", s.a(s.taxSave))
	m.HandleFunc("POST /api/tax/me/proofs", s.a(s.taxProofAdd))
	m.HandleFunc("POST /api/tax/me/proofs/{id}/withdraw", s.a(s.taxProofWithdraw))
	m.HandleFunc("GET /api/tax", s.a(s.taxList))
	m.HandleFunc("GET /api/tax/employee/{id}", s.a(s.taxEmployee))
	m.HandleFunc("POST /api/tax/employee/{id}/regime-unlock", s.a(s.taxRegimeUnlock))
	m.HandleFunc("POST /api/tax/proofs/{id}/{action}", s.a(s.taxProofDecide))
	m.HandleFunc("PUT /api/tax/windows", s.a(s.taxWindowsPut))
	m.HandleFunc("POST /api/tax/finalise", s.a(s.taxFinalise))
	m.HandleFunc("POST /api/tax/finalise/reopen", s.a(s.taxFinaliseReopen))
	m.HandleFunc("POST /api/tax/remind", s.a(s.taxRemind))
	m.HandleFunc("GET /api/tax/declarations.xlsx", s.sheet(s.taxSheet))
	m.HandleFunc("GET /api/tax/items", s.a(s.taxItemsGet))
	m.HandleFunc("PUT /api/tax/items", s.a(s.taxItemsPut))
	m.HandleFunc("GET /api/tds", s.a(s.tdsOverview))
	m.HandleFunc("GET /api/tds/opening", s.a(s.tdsOpeningList))
	m.HandleFunc("GET /api/tds/opening/template.xlsx", s.sheet(s.tdsOpeningTemplate))
	m.HandleFunc("POST /api/tds/opening/{action}", s.a(s.tdsOpeningUpload))
	m.HandleFunc("PUT /api/tds/deposit/{kind}/{ym}", s.a(s.tdsDepositPut))
	m.HandleFunc("PUT /api/tds/return/{form}/{q}", s.a(s.tdsReturnPut))
	m.HandleFunc("GET /api/tds/return/{form}/{q}/sheet", s.sheet(s.tdsReturnSheet))
	m.HandleFunc("PUT /api/tds/deductor", s.a(s.deductorPut))
	m.HandleFunc("GET /api/form16", s.a(s.form16List))
	m.HandleFunc("GET /api/form16/all", s.a(s.form16All))
	m.HandleFunc("POST /api/form16/release", s.a(s.form16ReleasePost))
	m.HandleFunc("GET /api/form16/{id}", s.a(s.form16Get))
	m.HandleFunc("GET /api/me/form16", s.a(s.myForm16))
	m.HandleFunc("GET /api/statutory/ecr.txt", s.textFile(s.ecrText))
	m.HandleFunc("GET /api/statutory/{file}", s.a(s.statPreview))
	m.HandleFunc("GET /api/statutory/{file}/sheet", s.sheet(s.statSheet))
	m.HandleFunc("GET /api/statutory/history", s.a(s.statHistory))
	m.HandleFunc("GET /api/statutory/history/{id}/sheet", s.sheet(s.statHistorySheet))

	m.HandleFunc("GET /api/separations", s.a(s.sepList))
	m.HandleFunc("POST /api/separations", s.a(s.sepCreate))
	m.HandleFunc("GET /api/separations/{id}", s.a(s.sepGet))
	m.HandleFunc("GET /api/separations/{id}/letters/{lid}", s.a(s.sepLetter))
	m.HandleFunc("POST /api/separations/{id}/clearance/{item}", s.a(s.clearanceSign))
	m.HandleFunc("POST /api/separations/{id}/{action}", s.a(s.sepAction))
	m.HandleFunc("GET /api/separations/settings", s.a(s.sepSettingsGet))
	m.HandleFunc("PUT /api/separations/settings", s.a(s.sepSettingsPut))
	m.HandleFunc("POST /api/separations/clearance-items", s.a(s.sepItemAdd))
	m.HandleFunc("PUT /api/separations/clearance-items/{id}", s.a(s.sepItemPut))
	m.HandleFunc("GET /api/separations/absent", s.a(s.sepAbsent))
	m.HandleFunc("POST /api/separations/{id}/rehire", s.a(s.sepRehire))
	m.HandleFunc("POST /api/separations/{id}/terms", s.a(s.sepTerms))
	m.HandleFunc("GET /api/fnf", s.a(s.fnfList))
	m.HandleFunc("GET /api/fnf/{id}", s.a(s.fnfPreview))
	m.HandleFunc("POST /api/fnf/{id}", s.a(s.fnfPrepare))
	m.HandleFunc("GET /api/fnf/{id}/letter", s.a(s.fnfLetter))
	m.HandleFunc("GET /api/fnf/{id}/no-dues", s.a(s.fnfNoDues))
	m.HandleFunc("POST /api/fnf/{id}/receivable", s.a(s.fnfReceivable))
	m.HandleFunc("GET /api/inventory", s.a(s.invGet))
	m.HandleFunc("POST /api/inventory/stock/{kind}", s.a(s.invAddOrTransfer))
	m.HandleFunc("POST /api/inventory/issue", s.a(s.invIssue))
	m.HandleFunc("POST /api/inventory/return", s.a(s.invReturn))
	m.HandleFunc("POST /api/inventory/assets/{id}/lost", s.a(s.invLost))
	m.HandleFunc("GET /api/inventory/employee/{id}", s.a(s.invEmployee))
	m.HandleFunc("POST /api/inventory/stock/writeoff", s.a(s.invWriteOff))
	m.HandleFunc("GET /api/inventory/uniform-issues", s.a(s.uniformIssues))
	m.HandleFunc("GET /api/inventory/setup", s.a(s.invSetupGet))
	m.HandleFunc("PUT /api/inventory/setup", s.a(s.invSetupPut))
	m.HandleFunc("POST /api/inventory/items", s.a(s.invItemAdd))
	m.HandleFunc("PATCH /api/inventory/items/{id}", s.a(s.invItemPatch))
	m.HandleFunc("GET /api/inventory/kits", s.a(s.invKits))
	m.HandleFunc("POST /api/inventory/kits", s.a(s.invKitAdd))
	m.HandleFunc("PUT /api/inventory/kits/{id}", s.a(s.invKitPut))
	// Uniform sizes (uniform.go): the size lists, the summary for ordering, and each employee's sizes.
	m.HandleFunc("PUT /api/inventory/size-lists", s.a(s.invSizeListsPut))
	m.HandleFunc("GET /api/inventory/sizes", s.a(s.invSizesGet))
	m.HandleFunc("GET /api/inventory/sizes/sheet", s.invSizesSheet)
	m.HandleFunc("GET /api/employees/{id}/uniform", s.a(s.empUniformGet))
	m.HandleFunc("PUT /api/employees/{id}/uniform", s.a(s.empUniformPut))
	m.HandleFunc("GET /api/holds/absent", s.a(s.holdsAbsent))
	m.HandleFunc("POST /api/pay-items/deductions/{id}/waive", s.a(s.deductionWaive))

	m.HandleFunc("GET /api/reports", s.a(s.reportList))
	m.HandleFunc("GET /api/reports/{id}", s.a(s.reportGet))
	m.HandleFunc("GET /api/reports/{id}/sheet", s.sheet(s.reportSheet))
	s.reportAccessRoutes(m) // which report each role sees (report_access.go)
	m.HandleFunc("GET /api/devices", s.a(s.deviceList))
	m.HandleFunc("POST /api/devices", s.a(s.deviceAdd))
	m.HandleFunc("POST /api/devices/{id}/{action}", s.a(s.deviceAction))
	m.HandleFunc("POST /api/punches", s.punchPush)

	m.HandleFunc("GET /api/salary/structures", s.a(s.structures))
	m.HandleFunc("POST /api/salary/preview", s.a(s.salaryPreview))

	// Round 2 (ui2_people.go): employee file, work policies, nominations, masters.
	m.HandleFunc("POST /api/employees/{id}/reactivate", s.a(s.employeeReactivate))
	m.HandleFunc("POST /api/employees/{id}/rejoin", s.a(s.employeeRejoin))
	m.HandleFunc("GET /api/employees/{id}/nominations", s.a(s.employeeNominations))
	m.HandleFunc("POST /api/nominations/remind", s.a(s.nominationRemind))
	m.HandleFunc("GET /api/leave-plans/{id}/history", s.a(s.leavePlanHistory))
	m.HandleFunc("POST /api/leave-types/{name}/inactive", s.a(s.leaveTypeStatus(false)))
	m.HandleFunc("POST /api/leave-types/{name}/reactivate", s.a(s.leaveTypeStatus(true)))
	m.HandleFunc("GET /api/masters/{type}/{id}/impact", s.a(s.masterImpact))
	s.policyAckRoutes(m) // company policy acknowledgement (policy_ack.go)
	s.driveRoutes(m)     // document storage in Google Drive (drive.go)
	s.candidateRoutes(m) // onboarding stages: SI → OI → JI → Employee ID (onboarding_stages.go)
	s.mdmRoutes(m)       // master data sync with PMS, Purchase Desk, Moneyball and the CMS (mdm_routes.go)
	s.joiningRoutes(m)   // the joiner's self-fill link: HR's side and the public /api/join pages (joining_link.go)
	// Documents from existing employees: My documents and HR's tracker (doc_collect.go).
	s.docCollectRoutes(m)
	s.accessPolicyRoutes(m) // personal rights, who can see what, policy conflicts and overlaps (policy_conflict.go)
	s.machineRoutes(m)      // punch machines as one pool per cluster: board, plan, to-do list (machines.go)
	s.portalWriteRoutes(m)  // send employees to the Realtime portal: create, update, mark left (portal_write.go)
	s.payCompRoutes(m)      // pay components, structures from components, variable payouts (paycomp.go)

	return logRequests(m)
}

// ParseTrustedProxies reads TRUSTED_PROXIES (comma-separated addresses or CIDR ranges; "none" for none; empty for
// the default, the machine itself) into Options.TrustedProxies, refusing what is not an address or a range.
func ParseTrustedProxies(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "":
		return nil, nil
	case "none":
		return []string{}, nil
	}
	list := strings.Split(v, ",")
	if _, err := parseProxies(list); err != nil {
		return nil, err
	}
	return list, nil
}

// reJoinPath is a joiner link in a path: /api/join/{token}…; the token opens someone's joining form, so it is never
// written to the log.
var reJoinPath = regexp.MustCompile(`^/api/join/[^/]+`)

// logPath is a request's path as the log shows it: the joiner's link token masked.
func logPath(p string) string { return reJoinPath.ReplaceAllString(p, "/api/join/•••") }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasSuffix(r.URL.Path, "/health") {
			log.Printf("%s %s %s", r.Method, logPath(r.URL.Path), time.Since(start).Round(time.Millisecond))
		}
	})
}
