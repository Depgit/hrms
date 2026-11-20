// Load test: 5,053 employees. Times each call; payroll runs go to the background job (over 2,000).
// Load test (spec 22.1 #8): times each call against a database filled by load.sql.
const B = (process.env.HRMS_API || 'http://localhost:8080') + '/api';
const post = (p, b) => fetch(B + p, { method: 'POST', body: JSON.stringify(b) }).then((r) => r.json());
const A = (await post('/auth/login', { employeeId: 'EMP-1002', password: 'pass1234' })).token;
const o = await post('/auth/otp', { employeeId: 'EMP-1012', purpose: 'first' }); const v = await post('/auth/otp/verify', { employeeId: 'EMP-1012', code: o.devOtp });
const PM = (await post('/auth/password', { employeeId: 'EMP-1012', password: 'Saltstayz#2026', verifyToken: v?.verifyToken })).token;
const results = [];
async function timed(name, method, path, tok, role, body) {
  const t0 = performance.now();
  const r = await fetch(B + path, { method, headers: { Authorization: 'Bearer ' + tok, 'X-Role': role, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
  const buf = await r.arrayBuffer();
  const ms = Math.round(performance.now() - t0);
  let info = '';
  try { const j = JSON.parse(new TextDecoder().decode(buf)); info = j.total ?? j.rows?.length ?? j.list?.length ?? j.status ?? j.error ?? ''; } catch { info = `${(buf.byteLength / 1024).toFixed(0)} KB file`; }
  results.push({ name, ms, status: r.status, info: String(info) });
  console.log(`${String(ms).padStart(6)} ms  ${r.status}  ${name}  ${info}`);
  return r.status;
}
const st = await (await fetch(B + '/payroll', { headers: { Authorization: 'Bearer ' + A, 'X-Role': 'HR Admin' } })).json();
const ym = st.openYm;
await timed('Employee list, page 1 of 50', 'GET', '/employees?size=50', A, 'HR Admin');
await timed('Employee search', 'GET', '/employees?size=50&q=Load%20person%2042', A, 'HR Admin');
await timed('Dashboard overview', 'GET', '/dashboard', A, 'HR Admin');
await timed('Attendance records for the month', 'GET', `/attendance?ym=${ym}`, A, 'HR Admin');
// Month close is the run checklist now: five closures, each closed only when nothing is pending (there is no way to
// set items aside), and the dry run needs all five closed. load.sql must leave nothing pending for the dry run to run.
await timed('Run checklist (pending items in five closures)', 'GET', `/payroll/${ym}/checklist`, A, 'HR Admin');
for (const k of ['attendance', 'leave', 'bank', 'claim', 'deduction']) {
  if (k === 'attendance' || k === 'leave') await timed(`Reject waiting at cut-off (${k})`, 'POST', `/payroll/${ym}/closures/${k}/reject-waiting`, A, 'HR Admin', { all: true });
  await timed(`Close ${k}`, 'POST', `/payroll/${ym}/closures/${k}/close`, A, 'HR Admin', {});
}
const t0 = performance.now();
await timed('Dry run request (queued to the job)', 'POST', `/payroll/${ym}/dry-run`, A, 'HR Admin', {});
for (;;) {
  const m = (await (await fetch(B + '/payroll', { headers: { Authorization: 'Bearer ' + A, 'X-Role': 'HR Admin' } })).json()).open;
  if (!m.job && m.status === 'Dry run') break;
  if (m.jobError) { console.log('JOB ERROR', m.jobError); break; }
  await new Promise((r) => setTimeout(r, 500));
}
const dry = Math.round(performance.now() - t0);
results.push({ name: 'Dry run for 5,053 people, end to end (background job)', ms: dry, status: 200, info: '' });
console.log(`${String(dry).padStart(6)} ms  200  Dry run end to end`);
await timed('Payroll rows, page 1', 'GET', `/payroll/${ym}/rows?page=1&size=50`, A, 'HR Admin');
await timed('Payroll rows, filtered by flag', 'GET', `/payroll/${ym}/rows?flag=miss&size=50`, A, 'HR Admin');
await timed('Run checks', 'GET', `/payroll/${ym}/checks`, A, 'HR Admin');
await timed('Payroll sheet download', 'GET', `/payroll/${ym}/sheet.xlsx`, A, 'HR Admin');
await timed('Edit one row', 'PATCH', `/payroll/${ym}/rows/EMP-3001`, A, 'HR Admin', { otherEarn: 500, reason: 'Load test edit' });
const v0 = performance.now();
await timed('Verify request', 'POST', `/payroll/${ym}/verify`, A, 'HR Admin', {});
for (;;) { const m = await (await fetch(B + `/payroll/${ym}/close`, { headers: { Authorization: 'Bearer ' + A, 'X-Role': 'HR Admin' } })).json(); if (!m.month.job && m.month.status === 'Verified') break; await new Promise((r) => setTimeout(r, 500)); }
results.push({ name: 'Verify end to end (recalculates everyone)', ms: Math.round(performance.now() - v0), status: 200, info: '' });
const a0 = performance.now();
await timed('Approve request', 'POST', `/payroll/${ym}/approve`, PM, 'Payroll Manager', {});
for (;;) { const m = await (await fetch(B + `/payroll/${ym}/close`, { headers: { Authorization: 'Bearer ' + A, 'X-Role': 'HR Admin' } })).json(); if (!m.month.job && m.month.status === 'Approved') break; await new Promise((r) => setTimeout(r, 500)); }
results.push({ name: 'Approve end to end (recalculates everyone)', ms: Math.round(performance.now() - a0), status: 200, info: '' });
await timed('Publish (payslips, TDS history, carry-overs)', 'POST', `/payroll/${ym}/publish`, PM, 'Payroll Manager', {});
await timed('Bank advice download', 'GET', `/payroll/${ym}/bank-advice.xlsx`, PM, 'Payroll Manager');
await timed('ECR text file', 'GET', `/statutory/ecr.txt?ym=${ym}`, PM, 'Payroll Manager');
for (const r of ['emp_master', 'headcount', 'register', 'statutory', 'att_summary', 'late', 'leave_bal', 'missing', 'workload'])
  await timed(`Report: ${r}`, 'GET', `/reports/${r}?ym=${ym}`, A, 'HR Admin');
await timed('Report download: employee master (all rows)', 'GET', '/reports/emp_master/sheet', A, 'HR Admin');
await timed('Dashboard payroll tab', 'GET', '/dashboard?tab=pay', A, 'HR Admin');
await timed('Tax list (all permanent staff)', 'GET', '/tax', A, 'HR Admin');
const dev = await (await fetch(B + '/devices', { method: 'POST', headers: { Authorization: 'Bearer ' + A, 'X-Role': 'HR Admin' }, body: JSON.stringify({ name: 'Load gate' }) })).json();
const day = new Date(Date.now() - 864e5).toISOString().slice(0, 10);
const punches = [];
for (let i = 3001; i <= 5500; i++) { punches.push({ employeeId: 'EMP-' + i, time: day + ' 08:5' + (i % 10), direction: 'IN' }, { employeeId: 'EMP-' + i, time: day + ' 18:0' + (i % 10), direction: 'OUT' }); }
const p0 = performance.now();
const pr = await (await fetch(B + '/punches', { method: 'POST', headers: { 'X-Device-Key': dev.key }, body: JSON.stringify({ punches }) })).json();
results.push({ name: '5,000 punches pushed by one device', ms: Math.round(performance.now() - p0), status: 200, info: `accepted ${pr.accepted}` });
console.log(`${String(Math.round(performance.now() - p0)).padStart(6)} ms  punches accepted ${pr.accepted}`);
import('fs').then((fs) => fs.writeFileSync('load-results.json', JSON.stringify(results, null, 1)));
