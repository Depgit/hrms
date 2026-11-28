// The GET sweep: every GET route of the API (backend/internal/api, route table in server.go and the *Routes
// helpers), each with the concrete URLs the harness calls. Path parameters come from the list responses the sweep
// has already received (HR Admin, who sees everything, runs first); query variants are the ones the web app sends
// (grep frontend/src for `api(` calls). capture.mjs checks that every GET pattern in the route table has an entry
// here and fails when one is missing, so a new route cannot be skipped silently.
//
// An entry is [pattern, urls(c)] where c is the SweepCtx below. Entries run in this order; one that needs ids from a
// list must come after the list's entry. A URL may be an object { url, auth: false } for a call without a token.

const enc = encodeURIComponent;
const uniq = (l) => [...new Set(l)];

// Employees every per-person route is called for: the named users of the sample organisation and a few staff of
// each employment type (EMP-1014 contractual, EMP-1015 retainer, EMP-1016/1020 permanent), plus everyone created
// during the run. 'me' where the web app uses it.
const SAMPLE = ['EMP-1001', 'EMP-1002', 'EMP-1004', 'EMP-1008', 'EMP-1009', 'EMP-1011', 'EMP-1014', 'EMP-1015', 'EMP-1016', 'EMP-1020', 'EMP-1022', 'EMP-1046'];
const CAP = 30; // most ids of one kind called per route

export class SweepCtx {
  constructor({ norm, today, base }) {
    this.norm = norm; // the live normalizer: ids sort by their normalized form, so new ids sort the same in every run
    this.today = today;
    this.bodies = new Map(); // url → body of the first role's response
    this.base = base; // { employees: Set } from the sweep before the writes (undefined in that sweep)
    this.extra = { files: new Set() }; // ids learned outside the sweep (uploads in the write script)
  }
  body(url) { return this.bodies.get(url); }
  arr(url, prop) {
    const b = this.body(url);
    const l = prop ? b?.[prop] : b;
    return Array.isArray(l) ? l : [];
  }
  /** sort orders ids deterministically: numbers numerically, strings by their normalized form. */
  sort(ids) {
    const l = uniq(ids.filter((x) => x !== null && x !== undefined && x !== ''));
    const k = (x) => (typeof x === 'number' ? x.toString().padStart(12, '0') : this.norm.str(String(x)));
    return l.sort((a, b) => (k(a) < k(b) ? -1 : k(a) > k(b) ? 1 : 0)).slice(0, CAP);
  }
  get employees() { return this.arr('/api/employees?size=500', 'rows'); }
  get empIds() { return this.employees.map((e) => e.id); }
  get sample() {
    const all = new Set(this.empIds);
    const fresh = this.base ? this.empIds.filter((id) => !this.base.employees.has(id)) : [];
    return uniq([...SAMPLE.filter((id) => all.has(id) || all.size === 0), ...this.sort(fresh)]);
  }
  get inactive() {
    const l = this.employees.filter((e) => e.status === 'Inactive').map((e) => e.id);
    return l.length ? this.sort(l) : ['EMP-1020'];
  }
  get ym() { return this.today.slice(0, 7); }
  get prevYm() { const [y, m] = this.ym.split('-').map(Number); return m === 1 ? `${y - 1}-12` : `${y}-${String(m - 1).padStart(2, '0')}`; }
  get openYm() { return this.body('/api/payroll')?.openYm || this.prevYm; }
  get months() { return uniq([this.openYm, this.prevYm, this.ym]).sort(); }
  get fy() { return this.body('/api/tax/me')?.figure?.fy || this.body('/api/tax/me')?.fy || ''; }
  addDays(n) {
    const d = new Date(this.today + 'T12:00:00Z');
    d.setUTCDate(d.getUTCDate() + n);
    return d.toISOString().slice(0, 10);
  }
  get hub() { return this.arr('/api/masters/location').find((l) => l.name === 'Gurugram – Sector 29 Hub')?.id || ''; }
  get gurugram() { return this.arr('/api/masters/city').find((l) => l.name === 'Gurugram')?.id || ''; }
  get masterTypes() { const l = this.arr('/api/masters'); return l.length ? l : ['attendance_mode', 'change_reason', 'city', 'cluster', 'department', 'designation', 'employment_type', 'grade', 'location', 'qualification', 'region', 'shift', 'state']; }
  get approvalIds() {
    const ids = [];
    for (const tab of ['open', 'done', 'info']) for (const r of this.arr(`/api/approvals?tab=${tab}&wf=`)) ids.push(r.id);
    return uniq(ids);
  }
  get fileIds() {
    const ids = [...this.extra.files];
    for (const id of this.sample) for (const d of this.arr(`/api/employees/${id}/documents`)) ids.push(d.id);
    return this.sort(ids);
  }
}

const perEmp = (path, q = '') => (c) => c.sample.map((id) => `${path.replace('{id}', id)}${q}`);

export const ROUTES = [
  // basics and the lists other entries take ids from
  ['GET /api/health', () => [{ url: '/api/health', auth: false }]],
  ['GET /api/me', () => ['/api/me']],
  ['GET /api/lookups', () => ['/api/lookups']],
  ['GET /api/payroll', () => ['/api/payroll']],
  ['GET /api/tax/me', () => ['/api/tax/me']],
  ['GET /api/employees', (c) => ['/api/employees?size=500', '/api/employees?status=Active&size=200', '/api/employees?status=Active&size=50', '/api/employees?status=Inactive&size=50',
    '/api/employees?q=Karan&size=8', '/api/employees?status=Active&q=EMP-10&size=8', '/api/employees?page=2&size=20', `/api/employees?city=${c.gurugram}&size=50`,
    '/api/employees?department=Front%20Office&type=Permanent&size=50']],
  ['GET /api/masters', () => ['/api/masters']],
  ['GET /api/masters/{type}', (c) => c.masterTypes.flatMap((t) => [`/api/masters/${t}`, `/api/masters/${t}?status=Active`])],
  ['GET /api/masters/{type}/{id}/impact', (c) => c.masterTypes.flatMap((t) => c.sort(c.arr(`/api/masters/${t}`).map((x) => x.id)).slice(0, 3).map((id) => `/api/masters/${t}/${enc(id)}/impact`))],
  ['GET /api/shifts/{id}', (c) => [...c.sort(c.arr('/api/masters/shift').map((x) => x.id)), 'new'].map((id) => `/api/shifts/${enc(id)}`)],
  ['GET /api/master-update/download', () => ['/api/master-update/download', '/api/master-update/download?data=1']],

  // dashboards and my view
  ['GET /api/dashboard', () => ['', 'wf', 'ap', 'cmp', 'pay', 'att'].map((t) => `/api/dashboard${t ? '?tab=' + t : ''}`)],
  ['GET /api/me/dashboard', () => ['ov', 'pay', 'att', 'cmp'].map((t) => `/api/me/dashboard?tab=${t}`)],
  ['GET /api/me/requests', () => ['all', 'leave', 'regularize', 'p_change', 'p_claim', 'resign'].map((w) => `/api/me/requests?wf=${w}`)],
  ['GET /api/me/separation', () => ['/api/me/separation', '/api/me/separation?employeeId=EMP-1020']],
  ['GET /api/me/directory', (c) => ['/api/me/directory', ...c.arr('/api/masters/department').filter((d) => d.name === 'Front Office').map((d) => `/api/me/directory?dept=${enc(d.id)}`)]],
  // My view › Employee directory: everyone with email, departments with headcount (mydirectory.go, 12 Nov 2026; no
  // Postgres counterpart: 404 there, and the chart above gains ref, email, id and mine). Each person's `ref` is keyed
  // by the server's secret (APP_SECRET, else random per process), so it differs between runs without APP_SECRET.
  // Tested in mydirectory_test.go and tests/e2e/directory.mjs.
  ['GET /api/me/directory/people', (c) => ['/api/me/directory/people', '/api/me/directory/people?q=kulkarni%20meera', '/api/me/directory/people?q=merea&page=1',
    '/api/me/directory/people?page=2', ...c.arr('/api/masters/department').filter((d) => d.name === 'Front Office').map((d) => `/api/me/directory/people?dept=${enc(d.id)}`)]],
  ['GET /api/me/directory/departments', () => ['/api/me/directory/departments']],
  ['GET /api/me/discipline', (c) => c.months.map((m) => `/api/me/discipline?ym=${m}`)],
  ['GET /api/notices', () => ['/api/notices']],

  // employees
  ['GET /api/employees/next-code', () => ['/api/employees/next-code', '/api/employees/next-code?type=Permanent', '/api/employees/next-code?type=Contractual']],
  ['GET /api/employees/export', () => ['/api/employees/export?sections=emp', '/api/employees/export?sections=emp,bank,salary', '/api/employees/export?sections=emp,personal,govid,bank,salary']],
  ['GET /api/employees/export/options', () => ['/api/employees/export/options']],
  ['GET /api/employee-codes', () => ['/api/employee-codes']],
  ['GET /api/employees/{id}', perEmp('/api/employees/{id}')],
  ['GET /api/employees/{id}/history', perEmp('/api/employees/{id}/history')],
  ['GET /api/employees/{id}/documents', perEmp('/api/employees/{id}/documents')],
  ['GET /api/employees/{id}/file', (c) => c.sample.flatMap((id) => [`/api/employees/${id}/file`, `/api/employees/${id}/file?ym=${c.openYm}`])],
  ['GET /api/employees/{id}/nominations', perEmp('/api/employees/{id}/nominations')],
  ['GET /api/employees/{id}/pf-exempt', perEmp('/api/employees/{id}/pf-exempt')],

  // access
  ['GET /api/rights', () => ['/api/rights']],
  ['GET /api/roles', () => ['/api/roles']],
  ['GET /api/role-map', () => ['/api/role-map']],
  ['GET /api/access/person/{id}', (c) => uniq([...c.arr('/api/role-map').map((m) => `/api/access/person/${m.employee_id}?role=${enc(m.role_name)}`), '/api/access/person/EMP-1009?role=Line%20Manager', '/api/access/person/EMP-1020?role=Employee'])],
  ['GET /api/audit', () => ['/api/audit', '/api/audit?page=2']],
  // Personal rights and "who can see what" (access_personal.go, 17 Nov 2026; no Postgres counterpart: 404 there).
  ['GET /api/user-rights', () => ['/api/user-rights', '/api/user-rights?all=1', '/api/user-rights?employee=EMP-1009']],
  ['GET /api/access/person/{id}/effective', (c) => uniq([...c.arr('/api/role-map').map((m) => `/api/access/person/${m.employee_id}/effective`), '/api/access/person/EMP-1009/effective', '/api/access/person/EMP-1020/effective'])],
  ['GET /api/access/person/{id}/effective.xlsx', () => ['/api/access/person/EMP-1002/effective.xlsx']],
  // Which report each role sees (report_access.go, 18 Nov 2026; no Postgres counterpart: 404 there).
  ['GET /api/access/reports', () => ['/api/access/reports?role=HRBP', '/api/access/reports?role=HR%20Admin', '/api/access/reports']],

  // approvals
  ['GET /api/approvals', () => ['mine', 'open', 'done', 'info'].map((t) => `/api/approvals?tab=${t}&wf=`).concat(['/api/approvals', '/api/approvals?tab=mine&wf=leave', '/api/approvals?tab=open&wf=p_ack'])],
  ['GET /api/approvals/counts', () => ['/api/approvals/counts?wf=', '/api/approvals/counts?wf=leave']],
  ['GET /api/approvals/flows', () => ['/api/approvals/flows']], // approval rule lists (11 Nov 2026; the Postgres build had /api/approvals/matrix, grids)
  ['GET /api/approvals/matrix', () => ['/api/approvals/matrix']], // the Postgres build's grids; gone since the rule lists (404 in the MongoDB build: intended)
  ['GET /api/approvals/settings', () => ['/api/approvals/settings']],
  ['GET /api/approvals/approvers', () => ['/api/approvals/approvers']],
  ['GET /api/approvals/health', () => ['/api/approvals/health']],
  ['GET /api/approvals/columns', () => ['/api/approvals/columns']],
  ['GET /api/approvals/skip-check', () => ['/api/approvals/skip-check?wf=p_claim', '/api/approvals/skip-check?wf=leave']], // HR's "Skip approval" offer (19 Nov 2026)
  ['GET /api/approvals/{id}', (c) => c.sort(c.approvalIds).map((id) => `/api/approvals/${enc(id)}`)],
  // Payroll's verification tools (verify_edit.go, approval_bulk.go, 11 Nov 2026; no Postgres counterpart: 404 there).
  // Tested in verify_bulk_test.go and tests/e2e/verify.mjs. The export is an .xlsx with today's date in it.
  ['GET /api/approvals/{id}/verify-docs', (c) => c.sort(c.approvalIds).slice(0, 5).map((id) => `/api/approvals/${enc(id)}/verify-docs`)],
  ['GET /api/approvals/{id}/verify-docs/{doc}', () => ['/api/approvals/JV-0000/verify-docs/photo']],
  ['GET /api/approvals/bulk/export', () => ['/api/approvals/bulk/export?group=pay', '/api/approvals/bulk/export?group=join']],

  // onboarding
  ['GET /api/onboarding/setup', () => ['/api/onboarding/setup']],
  ['GET /api/onboarding/settings', () => ['/api/onboarding/settings']],
  ['GET /api/candidates', () => ['/api/candidates', '/api/candidates?stage=Done']],
  ['GET /api/candidates/managers', () => ['/api/candidates/managers']],
  ['GET /api/candidates/{id}', (c) => c.sort(c.arr('/api/candidates', 'rows').flatMap((r) => [r.id, r.current_id])).map((id) => `/api/candidates/${enc(id)}`)],
  ['GET /api/candidates/rejoin/{id}', (c) => c.inactive.slice(0, 3).map((id) => `/api/candidates/rejoin/${id}`)],
  ['GET /api/offers', () => ['/api/offers', '/api/offers?legacy=1']],
  ['GET /api/offers/{id}', (c) => c.sort([...c.arr('/api/offers'), ...c.arr('/api/offers?legacy=1')].map((o) => o.id)).map((id) => `/api/offers/${enc(id)}`)],
  ['GET /api/offers/rejoin/{id}', (c) => c.inactive.slice(0, 3).map((id) => `/api/offers/rejoin/${id}`)],
  // The joiner's link status per offer (joining_link.go, 11 Nov 2026; no Postgres counterpart: a 404 there).
  ['GET /api/joiner-link/{id}', (c) => c.sort([...c.arr('/api/offers'), ...c.arr('/api/offers?legacy=1')].map((o) => o.id)).map((id) => `/api/joiner-link/${enc(id)}`)],
  ['GET /api/files', () => ['/api/files?ownerKind=employee&ownerId=EMP-1020', '/api/files?ownerKind=employee&ownerId=EMP-1002', '/api/files?ownerKind=candidate&ownerId=SI-0001']],
  ['GET /api/files/{id}', (c) => c.fileIds.map((id) => `/api/files/${enc(id)}`)],
  ['GET /api/settings/company', () => ['/api/settings/company']],
  // PAN verification's provider setting (b1_pan.go, 14 Nov 2026; HR Admin only; no Postgres counterpart: 404 there).
  ['GET /api/settings/pan-verify', () => ['/api/settings/pan-verify']],

  // nominations, policies, leave
  ['GET /api/nominations/schemes', () => ['/api/nominations/schemes']],
  ['GET /api/me/nominations', () => ['/api/me/nominations']],
  // Documents from existing employees: My documents and HR's Document collection (doc_collect.go, 12 Nov 2026; no
  // Postgres counterpart: 404 there). Tested in doc_collect_test.go and tests/e2e/docs.mjs. The export is an .xlsx
  // with today's date in it; windows open on the first start, so the bell (/api/notices) differs from Postgres too.
  ['GET /api/me/documents', () => ['/api/me/documents']],
  ['GET /api/doc-collection', () => ['all', 'overdue', 'due7', 'tocheck', 'complete'].map((f) => `/api/doc-collection?filter=${f}`)],
  ['GET /api/doc-collection/export', () => ['/api/doc-collection/export', '/api/doc-collection/export?filter=overdue']],
  ['GET /api/doc-collection/settings', () => ['/api/doc-collection/settings']],
  ['GET /api/doc-collection/{id}', (c) => c.sample.slice(0, 6).map((id) => `/api/doc-collection/${enc(id)}`)],
  ['GET /api/nominations', () => ['/api/nominations']],
  ['GET /api/policies', () => ['/api/policies']],
  ['GET /api/policies/coverage', () => ['/api/policies/coverage', '/api/policies/coverage?q=Karan']],
  // Policy conflict check: overlap report and each employee's effective policy (policy_conflict.go, 17 Nov 2026;
  // no Postgres counterpart: 404 there).
  ['GET /api/policies/overlaps', () => ['/api/policies/overlaps']],
  ['GET /api/policies/overlaps.xlsx', () => ['/api/policies/overlaps.xlsx']],
  ['GET /api/employees/{id}/policy', perEmp('/api/employees/{id}/policy')],
  ['GET /api/policies/{id}', (c) => c.sort(c.arr('/api/policies').map((p) => p.id)).map((id) => `/api/policies/${enc(id)}`)],
  ['GET /api/leave-plans', () => ['/api/leave-plans']],
  ['GET /api/leave-plans/{id}/history', (c) => c.sort(c.arr('/api/leave-plans', 'plans').map((p) => p.plan?.id ?? p.id)).map((id) => `/api/leave-plans/${enc(id)}/history`)],
  ['GET /api/calendars', () => ['/api/calendars']],
  ['GET /api/exceptions', () => ['/api/exceptions']],
  ['GET /api/leave/balances/{id}', (c) => ['me', ...c.sample].map((id) => `/api/leave/balances/${id}`).concat([`/api/leave/balances/me?on=${c.addDays(7)}`])],

  // roster and attendance
  ['GET /api/roster', (c) => [`/api/roster?location=${c.hub}&department=F%26B&week=${c.addDays(1)}`, `/api/roster?location=${c.hub}&week=${c.today}`, `/api/roster?city=${c.gurugram}&week=${c.addDays(-7)}`]],
  ['GET /api/roster/discipline', (c) => c.months.map((m) => `/api/roster/discipline?ym=${m}`)],
  ['GET /api/attendance', (c) => [`/api/attendance?ym=${c.ym}`, `/api/attendance?ym=${c.prevYm}&location=${c.hub}`, ...['EMP-1020', 'EMP-1014', 'EMP-1009'].map((e) => `/api/attendance?ym=${c.ym}&employee=${e}`), `/api/attendance?ym=${c.prevYm}&employee=EMP-1020`]],
  ['GET /api/attendance/issues', (c) => c.months.map((m) => `/api/attendance/issues?ym=${m}`)],
  ['GET /api/attendance/day', (c) => ['/api/attendance/day', `/api/attendance/day?date=${c.addDays(-2)}&location=${c.hub}`, `/api/attendance/day?from=${c.addDays(-7)}&to=${c.today}`]],
  ['GET /api/attendance/month/{id}', (c) => ['me', ...c.sample].flatMap((id) => [`/api/attendance/month/${id}?ym=${c.ym}`, `/api/attendance/month/${id}?ym=${c.prevYm}`])],
  ['GET /api/attendance/summary', (c) => [`/api/attendance/summary?from=${c.ym}-01&to=${c.today}`, `/api/attendance/summary?from=${c.prevYm}-01&to=${c.addDays(-1)}&city=${c.gurugram}&department=Front%20Office&type=Permanent`]],
  ['GET /api/attendance/pull-config', () => ['/api/attendance/pull-config']],
  ['GET /api/attendance/realtime', () => ['/api/attendance/realtime']],
  ['GET /api/attendance/alerts', () => ['/api/attendance/alerts']],
  ['GET /api/attendance/called', (c) => c.months.map((m) => `/api/attendance/called?ym=${m}`)],
  ['GET /api/attendance/dev-allow/{id}', perEmp('/api/attendance/dev-allow/{id}')],
  ['GET /api/attendance/realtime/match', () => ['/api/attendance/realtime/match', '/api/attendance/realtime/match?level=All&q=emp&page=1&size=20']],
  ['GET /api/attendance/realtime/match/download', () => ['/api/attendance/realtime/match/download?format=csv', '/api/attendance/realtime/match/download?what=portal&format=csv']],
  ['GET /api/attendance/machines', () => ['/api/attendance/machines']],
  ['GET /api/attendance/portal', () => ['/api/attendance/portal']],
  ['GET /api/attendance/portal/map', () => ['/api/attendance/portal/map']],
  ['GET /api/attendance/machines/todo', () => ['/api/attendance/machines/todo', '/api/attendance/machines/todo?kind=Enrol', '/api/attendance/machines/todo?kind=Remove']],
  ['GET /api/attendance/machines/todo.xlsx', () => ['/api/attendance/machines/todo.xlsx?format=csv']],
  ['GET /api/attendance/machines/plan', (c) => uniq([...c.arr('/api/attendance/machines', 'clusters').slice(0, 3).map((x) => `/api/attendance/machines/plan?clusterId=${enc(x.id)}`), '/api/attendance/machines/plan?clusterId=none'])],
  ['GET /api/attendance/machines/check', (c) => [`/api/attendance/machines/check?locationId=${enc(c.hub || 'none')}`]],
  ['GET /api/bulk/roster/download', (c) => [`/api/bulk/roster/download?location=${c.hub}&department=Front%20Office&from=${c.addDays(1)}&days=7`, `/api/bulk/roster/download?location=${c.hub}`]],
  ['GET /api/bulk/att/log', () => ['/api/bulk/att/log']],
  ['GET /api/deviations', (c) => [`/api/deviations?from=${c.addDays(-14)}&to=${c.today}&status=Pending`, `/api/deviations?from=${c.addDays(-30)}&to=${c.today}`]],
  ['GET /api/consequences', () => ['/api/consequences']],
  ['GET /api/consequences/approver', () => ['/api/consequences/approver?employeeId=EMP-1020', '/api/consequences/approver?employeeId=EMP-1014']],
  ['GET /api/consequences/{id}/letter', (c) => c.sort(c.arr('/api/consequences', 'rows').map((r) => r.id)).map((id) => `/api/consequences/${enc(id)}/letter`)],

  // bulk update
  ['GET /api/bulk', () => ['/api/bulk']],
  ['GET /api/bulk/{tpl}/download', (c) => c.arr('/api/bulk').flatMap((t) => [`/api/bulk/${t.key}/download`, ...(t.data ? [`/api/bulk/${t.key}/download?data=1`] : [])])],
  ['GET /api/managers/left', () => ['/api/managers/left']],

  // payroll
  ['GET /api/payroll/register', () => ['/api/payroll/register']],
  ['GET /api/payroll/upcoming', () => ['/api/payroll/upcoming']],
  ['GET /api/payroll/upcoming.xlsx', () => ['/api/payroll/upcoming.xlsx']],
  ['GET /api/payroll/schedule', () => ['/api/payroll/schedule']],
  ['GET /api/payroll/{ym}/checklist', (c) => c.months.map((m) => `/api/payroll/${m}/checklist`)],
  ['GET /api/payroll/{ym}/attendance.xlsx', (c) => c.months.map((m) => `/api/payroll/${m}/attendance.xlsx`)],
  ['GET /api/payroll/{ym}/rows', (c) => c.months.flatMap((m) => [`/api/payroll/${m}/rows`, `/api/payroll/${m}/rows?size=500`, `/api/payroll/${m}/rows?q=EMP-1020`])],
  ['GET /api/payroll/{ym}/rows/{emp}', (c) => c.months.flatMap((m) => ['EMP-1020', 'EMP-1014', 'EMP-1002', 'EMP-1016'].map((e) => `/api/payroll/${m}/rows/${e}`))],
  ['GET /api/payroll/{ym}/checks', (c) => c.months.map((m) => `/api/payroll/${m}/checks`)],
  ['GET /api/payroll/{ym}/cost', (c) => c.months.map((m) => `/api/payroll/${m}/cost`)],
  ['GET /api/payroll/{ym}/sheet.xlsx', (c) => c.months.map((m) => `/api/payroll/${m}/sheet.xlsx`)],
  ['GET /api/payroll/{ym}/bank-advice.xlsx', (c) => c.months.map((m) => `/api/payroll/${m}/bank-advice.xlsx`)],
  ['GET /api/payroll/{ym}/register.xlsx', (c) => c.months.map((m) => `/api/payroll/${m}/register.xlsx`)],
  // ICICI's bulk payment upload (with its check) and the full salary register (bankfile.go, 14 Nov 2026; no Postgres counterpart: 404 there).
  ['GET /api/payroll/{ym}/icici-upload', (c) => c.months.map((m) => `/api/payroll/${m}/icici-upload`)],
  ['GET /api/payroll/pf-settings', () => ['/api/payroll/pf-settings']],
  ['GET /api/payroll/{ym}/icici-upload.xlsx', (c) => c.months.map((m) => `/api/payroll/${m}/icici-upload.xlsx`)],
  ['GET /api/payroll/{ym}/full-register.xlsx', (c) => c.months.map((m) => `/api/payroll/${m}/full-register.xlsx`)],
  ['GET /api/payslip-setup', () => ['/api/payslip-setup']],
  ['GET /api/payslip-preview', (c) => ['/api/payslip-preview', '/api/payslip-preview?employeeId=EMP-1020', `/api/payslip-preview?employeeId=EMP-1016&ym=${c.openYm}`]],
  ['GET /api/payslips', () => ['/api/payslips', '/api/payslips?employeeId=EMP-1020', '/api/payslips?employeeId=EMP-1014']],
  ['GET /api/payslips/{ym}/{emp}', (c) => c.months.flatMap((m) => ['EMP-1020', 'EMP-1014', 'EMP-1002'].map((e) => `/api/payslips/${m}/${e}`))],
  ['GET /api/pay-items/{kind}', () => ['advances', 'deductions', 'arrears', 'claims', 'changes', 'other'].map((k) => `/api/pay-items/${k}`)],
  ['GET /api/holds', () => ['/api/holds']],
  // Salary advances asked for by employees and opening advances (advances.go, 12 Nov 2026; no Postgres counterpart: 404 there).
  ['GET /api/me/advance', () => ['/api/me/advance']],
  ['GET /api/advances/settings', () => ['/api/advances/settings']],
  ['GET /api/advances/opening/template', () => ['/api/advances/opening/template']],
  ['GET /api/holds/absent', () => ['/api/holds/absent', '/api/holds/absent?days=3']],

  // compensation
  ['GET /api/comp', () => ['/api/comp']],
  // The all-inclusive gross from a month (allin.go, 26 Nov 2026; no Postgres counterpart: 404 there).
  ['GET /api/comp/all-in', () => ['/api/comp/all-in']],
  ['GET /api/salary/structures', () => ['/api/salary/structures']],
  ['GET /api/comp/structures/{id}/maps', (c) => c.sort(c.arr('/api/salary/structures').map((s) => s.id)).map((id) => `/api/comp/structures/${enc(id)}/maps`)],
  ['GET /api/comp/structure-map/candidates', (c) => [`/api/comp/structure-map/candidates?date=${c.today}`, `/api/comp/structure-map/candidates?date=${c.today}&city=${c.gurugram}&type=Permanent`]],
  // Pay components (paycomp.go, 17 Nov 2026; no Postgres counterpart: 404 there): the component library, each version's
  // components (converted from the rates for the versions made before), and Payroll › Variable pay. salary_structure
  // rates gain comps (migration 014); payroll rows of designed structures gain r.comps, r.compDed, r.nonTax and
  // varMissing; the payroll sheet gains two last columns only then. Bulk update › Salary breakup gains two optional
  // columns (Total Pay (Fixed + Variable), Variable Target) and (17 Nov) the variable payout columns, which replaced the template Variable payouts (varpay). Tested in paycomp_test.go and calc/components_test.go.
  ['GET /api/comp/components', () => ['/api/comp/components']],
  ['GET /api/comp/structures/{id}/components', (c) => c.sort(c.arr('/api/salary/structures').map((s) => s.id)).map((id) => `/api/comp/structures/${enc(id)}/components`)],
  ['GET /api/pay/variable', (c) => ['/api/pay/variable', `/api/pay/variable?ym=${c.ym}`]],

  // tax and statutory
  ['GET /api/tax', () => ['/api/tax', '/api/tax?status=Submitted']],
  ['GET /api/tax/employee/{id}', perEmp('/api/tax/employee/{id}')],
  ['GET /api/tax/declarations.xlsx', () => ['/api/tax/declarations.xlsx']],
  ['GET /api/tax/items', () => ['/api/tax/items']],
  ['GET /api/tds', () => ['/api/tds']],
  // Opening TDS before go-live (tds_opening.go, 13 Nov 2026; no Postgres counterpart: 404 there).
  ['GET /api/tds/opening', () => ['/api/tds/opening']],
  ['GET /api/tds/opening/template.xlsx', () => ['/api/tds/opening/template.xlsx']],
  ['GET /api/tds/return/{form}/{q}/sheet', (c) => [1, 2, 3, 4].map((q) => `/api/tds/return/24Q/${q}/sheet?fy=${c.fy}`)],
  ['GET /api/form16', (c) => ['/api/form16', `/api/form16?fy=${c.fy}`]],
  ['GET /api/form16/all', (c) => [`/api/form16/all?fy=${c.fy}`]],
  ['GET /api/form16/{id}', (c) => ['me', 'EMP-1002', 'EMP-1020'].map((id) => `/api/form16/${id}?fy=${c.fy}`)],
  ['GET /api/me/form16', (c) => ['/api/me/form16', `/api/me/form16?fy=${c.fy}`]],
  ['GET /api/statutory/ecr.txt', (c) => c.months.map((m) => `/api/statutory/ecr.txt?ym=${m}`)],
  ['GET /api/statutory/{file}', (c) => [...['insurance', 'pf', 'esi'].map((f) => `/api/statutory/${f}?from=${c.prevYm}-01&to=${c.today}`), ...['ecr', 'esic'].flatMap((f) => c.months.map((m) => `/api/statutory/${f}?ym=${m}`))]],
  ['GET /api/statutory/{file}/sheet', (c) => [...['insurance', 'pf', 'esi'].map((f) => `/api/statutory/${f}/sheet?from=${c.prevYm}-01&to=${c.today}`), ...['ecr', 'esic'].map((f) => `/api/statutory/${f}/sheet?ym=${c.openYm}`)]],
  ['GET /api/statutory/history', () => ['/api/statutory/history']],
  ['GET /api/statutory/history/{id}/sheet', (c) => c.sort(c.arr('/api/statutory/history', 'files').map((f) => f.id)).map((id) => `/api/statutory/history/${enc(id)}/sheet`)],

  // separation and full and final
  ['GET /api/separations', () => ['/api/separations']],
  ['GET /api/separations/settings', () => ['/api/separations/settings']],
  ['GET /api/separations/absent', () => ['/api/separations/absent?employeeId=EMP-1022', '/api/separations/absent?employeeId=EMP-1020']],
  ['GET /api/separations/{id}', (c) => c.sort(c.arr('/api/separations', 'cases').map((s) => s.id)).map((id) => `/api/separations/${enc(id)}`)],
  ['GET /api/separations/{id}/letters/{lid}', (c) => c.sort(c.arr('/api/separations', 'cases').map((s) => s.id)).flatMap((id) => c.sort((c.body(`/api/separations/${enc(id)}`)?.letters || []).map((l) => l.id)).map((lid) => `/api/separations/${enc(id)}/letters/${enc(lid)}`))],
  ['GET /api/fnf', () => ['/api/fnf']],
  ['GET /api/fnf/{id}', (c) => c.sort([...c.arr('/api/fnf', 'rows').map((r) => r.employee_id ?? r.id), 'EMP-1020']).map((id) => `/api/fnf/${enc(id)}`)],
  ['GET /api/fnf/{id}/letter', (c) => c.sort([...c.arr('/api/fnf', 'rows').map((r) => r.employee_id ?? r.id), 'EMP-1020']).map((id) => `/api/fnf/${enc(id)}/letter`)],
  ['GET /api/fnf/{id}/no-dues', (c) => c.sort([...c.arr('/api/fnf', 'rows').map((r) => r.employee_id ?? r.id), 'EMP-1020']).map((id) => `/api/fnf/${enc(id)}/no-dues`)],

  // inventory
  ['GET /api/inventory', (c) => ['/api/inventory', '/api/inventory?store=central', '/api/inventory?store=hrbp%3AEMP-1004', `/api/inventory?ym=${c.ym}`, `/api/inventory?ym=${c.prevYm}&store=central`]],
  ['GET /api/inventory/employee/{id}', perEmp('/api/inventory/employee/{id}')],
  ['GET /api/inventory/uniform-issues', () => ['/api/inventory/uniform-issues', '/api/inventory/uniform-issues?employeeId=EMP-1020']],
  ['GET /api/inventory/setup', () => ['/api/inventory/setup']],
  ['GET /api/inventory/kits', () => ['/api/inventory/kits']],
  // Uniform sizes (uniform.go, 13 Nov 2026; no Postgres counterpart: 404 there): the summary for ordering with its Excel
  // file, and each employee's sizes with their uniform's recovery and refund. Setup, the uniform issues and My view's
  // pay tab gain keys (refundBasis, serviceMonths, rule, sizes, size_type; refundStatus, refundRule, recovered,
  // toRecover; uniform). Tested in uniform_test.go and tests/e2e/uniform.mjs.
  ['GET /api/inventory/sizes', (c) => ['/api/inventory/sizes', ...c.arr('/api/masters/department').filter((d) => d.name === 'Front Office').map((d) => `/api/inventory/sizes?dept=${enc(d.name)}`)]],
  ['GET /api/inventory/sizes/sheet', () => ['/api/inventory/sizes/sheet']],
  ['GET /api/employees/{id}/uniform', perEmp('/api/employees/{id}/uniform')],
  ['GET /api/employees/{id}/portal', perEmp('/api/employees/{id}/portal')],
  // Hiring lifecycle (hiring.go, 17 Nov 2026; no Postgres counterpart: 404 there): the employee's stage and status
  // timeline. The candidate view, offers and onboarding settings gain keys (offerVersions, waitingWindow, timeline,
  // hiring, offer_no…); the drop-off report is listed under /api/reports. Tested in hiring_test.go.
  ['GET /api/employees/{id}/timeline', perEmp('/api/employees/{id}/timeline')],
  // Staff accommodation (hst.go, 17 Nov 2026; no Postgres counterpart: 404 there): setup, units, stays, suggestions,
  // check-outs to do, the occupancy board and its Excel file, and the month's cost split. Tested in hst_test.go.
  ['GET /api/hst/setup', () => ['/api/hst/setup']],
  ['GET /api/hst/units', () => ['/api/hst/units']],
  ['GET /api/hst/units/{id}', (c) => c.sort(c.arr('/api/hst/units', 'units').map((u) => u.id)).flatMap((id) => [`/api/hst/units/${enc(id)}`, `/api/hst/units/${enc(id)}?ym=${c.ym}`])],
  ['GET /api/hst/suggest', (c) => c.sample.slice(0, 3).map((id) => `/api/hst/suggest?employeeId=${enc(id)}`)],
  ['GET /api/hst/stays', (c) => c.sample.map((id) => `/api/hst/stays?employeeId=${enc(id)}`)],
  ['GET /api/hst/attention', () => ['/api/hst/attention']],
  ['GET /api/hst/board', (c) => ['/api/hst/board', `/api/hst/board?ym=${c.ym}`]],
  ['GET /api/hst/board.xlsx', (c) => [`/api/hst/board.xlsx?ym=${c.ym}&format=csv`]],
  // Waiting for a bed and the offer form's accommodation clusters (hst_auto.go, 19 Nov 2026). Tested in hst_auto_test.go.
  ['GET /api/hst/waiting', () => ['/api/hst/waiting']],
  ['GET /api/hst/clusters', () => ['/api/hst/clusters']],
  ['GET /api/hst/split', (c) => ['/api/hst/split', `/api/hst/split?ym=${c.ym}`]],
  // Cost and budget console (cost.go, 17 Nov 2026; no Postgres counterpart: 404 there): the month's cost tree, the
  // people of a node, the six-month trend, the headcount plan and the Excel file. Tested in cost_test.go.
  ['GET /api/cost', (c) => ['/api/cost', ...c.months.map((m) => `/api/cost?ym=${m}`)]],
  ['GET /api/cost/people', (c) => [`/api/cost/people?ym=${c.prevYm}`, ...(c.body(`/api/cost?ym=${c.prevYm}`)?.tree?.children ?? []).slice(0, 2).map((n) => `/api/cost/people?ym=${c.prevYm}&kind=${n.kind}&id=${enc(n.id)}`)]],
  ['GET /api/cost/trend', (c) => ['/api/cost/trend', `/api/cost/trend?ym=${c.prevYm}`]],
  ['GET /api/cost/headcount', () => ['/api/cost/headcount']],
  ['GET /api/cost/export.xlsx', (c) => [`/api/cost/export.xlsx?ym=${c.prevYm}&format=csv`]],

  // reports and devices
  ['GET /api/reports', () => ['/api/reports']],
  ['GET /api/reports/{id}', (c) => c.arr('/api/reports', 'reports').map((r) => `/api/reports/${enc(r.id)}`)],
  ['GET /api/reports/{id}/sheet', (c) => c.arr('/api/reports', 'reports').map((r) => `/api/reports/${enc(r.id)}/sheet`)],
  ['GET /api/devices', () => ['/api/devices']],

  // company policies (acknowledgement)
  ['GET /api/company-policies', () => ['/api/company-policies']],
  ['GET /api/company-policies/{id}', (c) => c.sort(c.arr('/api/company-policies', 'policies').map((p) => p.id)).flatMap((id) => [`/api/company-policies/${enc(id)}`, `/api/company-policies/${enc(id)}?status=Not%20submitted`])],
  ['GET /api/me/policies', () => ['/api/me/policies']],

  // Google Drive (not connected in a test run: the status and the callback's refusal)
  ['GET /api/drive', () => ['/api/drive']],
  ['GET /api/drive/callback', () => [{ url: '/api/drive/callback', auth: false }, { url: '/api/drive/callback?state=x&code=y', auth: false }]],
];
