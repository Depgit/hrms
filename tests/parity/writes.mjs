// The deterministic write script: the important flows end to end over HTTP, with the request bodies the Go
// integration tests use (backend/internal/api/*_test.go). Every call, reads included, is recorded under a step name,
// so a flow that goes differently on another build shows up call by call. Ids and dates come from the responses and
// from today, never from randomness, so two runs on the same data and the same day send the same requests.
//
// The sample organisation (seed.SampleOrg): EMP-1002 HR Admin, EMP-1004 HRBP (north), EMP-1008 Property Manager,
// EMP-1009 Line Manager (Sector 29 Hub), EMP-1010 Function Head (Sales), EMP-1011 Payroll, EMP-1012 Payroll Manager,
// EMP-1014 contractual staff and EMP-1020 permanent staff reporting to EMP-1009.

const HR = ['EMP-1002', 'HR Admin'];
const HRBP = ['EMP-1004', 'HRBP'];
const LM = ['EMP-1009', 'Line Manager'];
const PM = ['EMP-1008', 'Property Manager'];
const PAY = ['EMP-1011', 'Payroll'];
const PAYM = ['EMP-1012', 'Payroll Manager'];
const EMP = ['EMP-1020', 'Employee'];
const CON = ['EMP-1014', 'Employee'];
const PDF = '%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n';

export async function runWrites({ today, rec, files, log }) {
  const day = (n) => { const d = new Date(today + 'T12:00:00Z'); d.setUTCDate(d.getUTCDate() + n); return d.toISOString().slice(0, 10); };
  const addMonths = (ym, n) => { const [y, m] = ym.split('-').map(Number); const t = y * 12 + (m - 1) + n; return `${Math.floor(t / 12)}-${String((t % 12) + 1).padStart(2, '0')}`; };
  const call = (step, [emp, role], method, path, body) => rec(step, method, path, { emp, role, body });
  const get = (step, who, path) => call(step, who, 'GET', path);
  const upload = async (step, [emp, role], kind, owner, key) => {
    const r = await rec(step, 'POST', '/api/files', { emp, role, form: { fields: { ownerKind: kind, ownerId: owner, docKey: key }, name: key.replace(/[^a-zA-Z0-9]/g, '_') + '.pdf', content: PDF, type: 'application/pdf' } });
    if (r.body?.id) files.add(r.body.id);
    return r;
  };
  const sendFile = (step, [emp, role], path, name, content) => rec(step, 'POST', path, { emp, role, form: { name, content, type: 'text/csv' } });
  const id = (r, k = 'id') => (r?.body && r.body[k] != null ? String(r.body[k]) : 'MISSING');
  // approveAll moves a request to its end as HR Admin (an override needs a note), like the Go tests' approveAll.
  const approveAll = async (step, reqId, note = 'Parity run') => {
    for (let i = 0; i < 10; i++) {
      const d = await get(`${step}.status`, HR, `/api/approvals/${reqId}`);
      if (d.body?.status !== 'Pending') return d.body?.status;
      const a = await call(`${step}.approve`, HR, 'POST', `/api/approvals/${reqId}/approve`, { note });
      if (a.status !== 200) return a.body?.error;
    }
    return 'still pending';
  };

  // ---- lookups the flows need (recorded like every other call) ----
  const locs = (await get('setup.locations', HR, '/api/masters/location?status=Active')).body || [];
  const hubLoc = locs.find((l) => l.name === 'Gurugram – Sector 29 Hub') || {};
  const golf = locs.find((l) => l.name === 'Gurugram – Golf Course Rd')?.id || 'MISSING';
  const hub = hubLoc.id || 'MISSING', city = hubLoc.city_id || 'MISSING';
  const depts = (await get('setup.departments', HR, '/api/masters/department?status=Active')).body || [];
  const frontOffice = depts.find((d) => d.name === 'Front Office')?.id || 'MISSING';
  const shifts = Object.fromEntries(((await get('setup.shifts', HR, '/api/masters/shift?status=Active')).body || []).map((s) => [s.code, s]));
  const staff = Object.fromEntries((((await get('setup.employees', HR, '/api/employees?size=500')).body || {}).rows || []).map((e) => [e.id, e]));
  const open = (await get('setup.payroll', PAY, '/api/payroll')).body?.openYm || addMonths(today.slice(0, 7), -1);

  // ---- masters: create, rename, retire, bring back ----
  {
    const g = await call('master.grade.create', HR, 'POST', '/api/masters/grade', { name: 'G7' });
    await call('master.grade.duplicate', HR, 'POST', '/api/masters/grade', { name: 'G7' });
    await call('master.grade.empty', HR, 'POST', '/api/masters/grade', { name: '' });
    const gid = id(g);
    await call('master.grade.rename', HR, 'PATCH', `/api/masters/grade/${gid}`, { name: 'G7 senior' });
    await call('master.grade.inactive-no-reason', HR, 'POST', `/api/masters/grade/${gid}/inactive`, { since: today });
    await call('master.grade.inactive', HR, 'POST', `/api/masters/grade/${gid}/inactive`, { since: today, reason: 'Merged into G6' });
    await call('master.grade.reactivate', HR, 'POST', `/api/masters/grade/${gid}/reactivate`);
    await call('master.grade.by-employee', EMP, 'POST', '/api/masters/grade', { name: 'G8' });
    const d = await call('master.designation.create', HR, 'POST', '/api/masters/designation', { name: 'Night Auditor', department_id: frontOffice, grades: ['G2'] });
    await call('master.designation.rename', HR, 'PATCH', `/api/masters/designation/${id(d)}`, { name: 'Night Audit Associate' });
    await get('master.designation.impact', HR, `/api/masters/designation/${id(d)}/impact`);
    await call('master.department.create', HR, 'POST', '/api/masters/department', { name: 'Spa', corporate: false });
  }

  // ---- settings ----
  await call('settings.sla.refused', LM, 'PUT', '/api/approvals/settings', { slaHours: 48 });
  await call('settings.sla.zero', HR, 'PUT', '/api/approvals/settings', { slaHours: 0 });
  await call('settings.sla', HR, 'PUT', '/api/approvals/settings', { slaHours: 48 });
  // the company TAN and PAN (TestTaxAndStatutory), so the 24Q data and Form 16 pages have something to show
  const slip = { setup: { accent: '#0f6e6e', footer: 'Computer generated', showEmployer: true }, cin: '', pan: 'AAACS1234B', tan: 'DELS12345A', pfCode: '', esiCode: '', cutoff: 'calendar', firstYm: '' };
  await call('settings.payslip-by-payroll', PAY, 'PUT', '/api/payslip-setup', slip);
  await call('settings.payslip', HR, 'PUT', '/api/payslip-setup', slip);

  // ---- leave: apply → Line Manager → Property Manager (TestEndToEnd) ----
  let tue = new Date(today + 'T12:00:00Z');
  do { tue.setUTCDate(tue.getUTCDate() + 1); } while (tue.getUTCDay() !== 1);
  tue.setUTCDate(tue.getUTCDate() + 1);
  const t0 = tue.toISOString().slice(0, 10);
  const plus = (n) => { const d = new Date(t0 + 'T12:00:00Z'); d.setUTCDate(d.getUTCDate() + n); return d.toISOString().slice(0, 10); };
  {
    await call('leave.contractual', CON, 'POST', '/api/approvals', { wf: 'leave', payload: { ltype: 'Casual leave', from: t0, to: plus(1) } });
    const r = await call('leave.apply', EMP, 'POST', '/api/approvals', { wf: 'leave', payload: { ltype: 'Casual leave', from: t0, to: plus(1) } });
    const lid = id(r);
    await call('leave.own-approve', EMP, 'POST', `/api/approvals/${lid}/approve`);
    await call('leave.pm-first', PM, 'POST', `/api/approvals/${lid}/approve`);
    await call('leave.lm-approve', LM, 'POST', `/api/approvals/${lid}/approve`);
    await call('leave.reject-no-comment', PM, 'POST', `/api/approvals/${lid}/reject`, {});
    await call('leave.pm-approve', PM, 'POST', `/api/approvals/${lid}/approve`);
    await get('leave.detail', EMP, `/api/approvals/${lid}`);
    // a second request cancelled by the employee, a third escalated by HR Admin and left waiting
    const c2 = await call('leave.apply2', EMP, 'POST', '/api/approvals', { wf: 'leave', payload: { ltype: 'Casual leave', from: plus(9), to: plus(9) } });
    await call('leave.cancel-by-lm', LM, 'POST', `/api/approvals/${id(c2)}/cancel`);
    await call('leave.cancel', EMP, 'POST', `/api/approvals/${id(c2)}/cancel`);
    await call('leave.cancel-again', EMP, 'POST', `/api/approvals/${id(c2)}/cancel`);
    const c3 = await call('leave.apply3', EMP, 'POST', '/api/approvals', { wf: 'leave', payload: { ltype: 'Casual leave', from: plus(14), to: plus(14) } });
    await call('leave.escalate-by-lm', LM, 'POST', `/api/approvals/${id(c3)}/escalate`);
    await call('leave.escalate', HR, 'POST', `/api/approvals/${id(c3)}/escalate`);
    await get('leave.balance', EMP, '/api/leave/balances/me');
  }

  // ---- regularization: open days of the last week (the window is 7 days) of two of EMP-1009's reports ----
  {
    let n = 0;
    for (const who of [EMP, CON]) {
      const a = await get('reg.days', LM, `/api/attendance?ym=${today.slice(0, 7)}&employee=${who[0]}`);
      const prev = await get('reg.days', LM, `/api/attendance?ym=${day(-7).slice(0, 7)}&employee=${who[0]}`);
      const openDays = [...(a.body?.days || []), ...(day(-7).slice(0, 7) !== today.slice(0, 7) ? prev.body?.days || [] : [])]
        .filter((d) => d.date < today && d.date >= day(-7) && /^(Missing|No punch|Absent)/.test(d.status || ''))
        .sort((x, y) => (x.date < y.date ? -1 : 1));
      const sh = shifts[staff[who[0]]?.shift_code] || { start_time: '09:00', end_time: '18:00' };
      log(`regularization: ${openDays.length} open days of ${who[0]} in the last week`);
      for (const d of openDays) {
        const rtype = d.status === 'No punch' || d.status === 'Absent' ? 'No Punch' : 'All Other';
        const r = await call('reg.raise', who, 'POST', '/api/approvals', { wf: 'regularize', payload: { date: d.date, rtype, in: d.in || sh.start_time, out: d.out || sh.end_time, reason: rtype === 'No Punch' ? 'Biometric or device issue' : 'Forgot to punch' } });
        const rid = id(r);
        if (n === 0) { // the chain as it is: Line Manager, then Property Manager
          await call('reg.pm-first', PM, 'POST', `/api/approvals/${rid}/approve`);
          await call('reg.lm-approve', LM, 'POST', `/api/approvals/${rid}/approve`);
          await call('reg.pm-approve', PM, 'POST', `/api/approvals/${rid}/approve`);
          await approveAll('reg.rest', rid);
        } else if (n === 1) {
          await call('reg.lm-reject-no-note', LM, 'POST', `/api/approvals/${rid}/reject`, {});
          await call('reg.lm-reject', LM, 'POST', `/api/approvals/${rid}/reject`, { note: 'No proof of attendance' });
        } else {
          await approveAll('reg.hr', rid);
        }
        n++;
      }
    }
    await call('reg.roster-changed', EMP, 'POST', '/api/approvals', { wf: 'regularize', payload: { date: day(-2), rtype: 'No Punch', in: '06:00', out: '14:00', reason: 'Roster changed' } });
    await call('reg.outside-window', EMP, 'POST', '/api/approvals', { wf: 'regularize', payload: { date: day(-40), rtype: 'No Punch', in: '06:00', out: '14:00', reason: 'Forgot to punch' } });
    await call('reg.exception', PAYM, 'POST', '/api/exceptions', { employeeId: EMP[0], kind: 'regularize', from: day(-40), to: day(-40), until: day(3), reason: 'Device fault' });
    const ex = await call('reg.with-exception', EMP, 'POST', '/api/approvals', { wf: 'regularize', payload: { date: day(-40), rtype: 'No Punch', in: '06:00', out: '14:00', reason: 'Biometric or device issue' } });
    await approveAll('reg.with-exception', id(ex));
    await get('reg.month', EMP, `/api/attendance/month/me?ym=${day(-40).slice(0, 7)}`);
  }

  // ---- claim with its bill (TestEndToEnd, TestPayroll) ----
  {
    await call('claim.no-bill', CON, 'POST', '/api/approvals', { wf: 'p_claim', payload: { amount: 3000, claimType: 'Travel', billDate: today } });
    const doc = await upload('claim.bill', EMP, 'employee', EMP[0], 'Bill');
    const r = await call('claim.raise', EMP, 'POST', '/api/approvals', { wf: 'p_claim', payload: { amount: 800, claimType: 'Mobile and internet', billDate: today, doc: id(doc) } });
    await call('claim.override-no-note', HR, 'POST', `/api/approvals/${id(r)}/approve`, {});
    await approveAll('claim', id(r));
  }

  // ---- personal details change (fixes_test, myrequests_test) ----
  {
    await call('pchange.bad-mobile', EMP, 'POST', '/api/approvals', { wf: 'p_change', payload: { kind: 'Personal', mobile: '12' } });
    const r = await call('pchange.raise', EMP, 'POST', '/api/approvals', { wf: 'p_change', payload: { kind: 'Personal', email: 'karan@example.com', note: 'New email' } });
    await approveAll('pchange', id(r));
    await get('pchange.employee', EMP, `/api/employees/${EMP[0]}`);
  }

  // ---- transfer (TestApprovalExtras) ----
  {
    const eff = addMonths(today.slice(0, 7), 1) + today.slice(7);
    const tr = { wf: 'transfer', employeeId: CON[0], payload: { toLocation: golf, effective: eff } };
    await call('transfer.by-employee', CON, 'POST', '/api/approvals', tr);
    await call('transfer.past', HR, 'POST', '/api/approvals', { ...tr, payload: { toLocation: golf, effective: '2020-01-01' } });
    const r = await call('transfer.raise', LM, 'POST', '/api/approvals', tr);
    await call('transfer.second', LM, 'POST', '/api/approvals', tr);
    await approveAll('transfer', id(r), 'ok');
    await get('transfer.history', HR, `/api/employees/${CON[0]}/history`);
  }

  // ---- consequence: a performance plan approved by the HRBP ----
  {
    const r = await call('cons.issue', LM, 'POST', '/api/consequences', { employeeId: EMP[0], kind: 'PIP', days: 15, reason: 'Attendance and punctuality', details: 'Late on five days this month.' });
    await call('cons.approve-by-lm', LM, 'POST', `/api/consequences/${id(r)}/approve`);
    await call('cons.approve', HRBP, 'POST', `/api/consequences/${id(r)}/approve`, { note: 'ok' });
    await get('cons.letter', EMP, `/api/consequences/${id(r)}/letter`);
  }

  // ---- roster changes (TestTimeAndAttendance) ----
  {
    await call('roster.no-reason', LM, 'POST', '/api/roster/cell', { employeeId: EMP[0], date: day(1), code: 'E' });
    await call('roster.cell', LM, 'POST', '/api/roster/cell', { employeeId: EMP[0], date: day(1), code: 'E', why: 'Staff absent or sick' });
    await call('roster.past', LM, 'POST', '/api/roster/cell', { employeeId: EMP[0], date: day(-3), code: 'E', why: 'x' });
    await call('roster.no-rest', LM, 'POST', '/api/roster/cell', { employeeId: EMP[0], date: day(9), code: 'N' });
    await call('roster.publish-by-lm', LM, 'POST', '/api/roster/publish', { location: hub, department: 'F&B', week: day(1) });
    await call('roster.publish', HR, 'POST', '/api/roster/publish', { location: hub, department: 'F&B', week: day(1) });
    await get('roster.week', LM, `/api/roster?location=${hub}&department=F%26B&week=${day(1)}`);
  }

  // ---- punch machine: a device and its punches (TestReportsAndConnector) ----
  {
    const dev = await call('device.add', HR, 'POST', '/api/devices', { name: 'Sector 29 front desk', vendor: 'eSSL' });
    const key = dev.body?.key || 'MISSING';
    const y = day(-1);
    const body = { punches: [{ employeeId: 'emp-1021', time: y + ' 09:02', direction: 'IN' }, { employeeId: 'EMP-1021', time: y + ' 18:05', direction: 'OUT' }, { employeeId: 'EMP-9999', time: y + ' 09:00' }] };
    await rec('device.wrong-key', 'POST', '/api/punches', { headers: { 'X-Device-Key': 'wrong' }, body });
    await rec('device.punches', 'POST', '/api/punches', { headers: { 'X-Device-Key': key }, body });
    await rec('device.punches-again', 'POST', '/api/punches', { headers: { 'X-Device-Key': key }, body });
    await get('device.day', HR, `/api/attendance?ym=${y.slice(0, 7)}&employee=EMP-1021`);
  }

  // ---- deviations: the first pending one of the last two weeks marked OK ----
  {
    const l = await get('dev.list', HR, `/api/deviations?from=${day(-14)}&to=${today}&status=Pending`);
    const rows = (l.body?.rows || []).slice().sort((a, b) => `${a.employeeId ?? a.employee_id}${a.date}`.localeCompare(`${b.employeeId ?? b.employee_id}${b.date}`));
    const first = rows[0];
    if (first) {
      const item = { employeeId: first.employeeId ?? first.employee_id, date: first.date };
      await call('dev.decide-by-lm', LM, 'POST', '/api/deviations/decide', { items: [item], status: 'OK' });
      await call('dev.decide', HR, 'POST', '/api/deviations/decide', { items: [item], status: 'OK', note: 'covering breakfast' });
    }
    log(`deviations: ${rows.length} pending`);
  }

  // ---- onboarding: shortlist → interview → offer → joined → documents → Employee ID → Payroll (onboarding_stages_test) ----
  {
    const sl = { data: { first: 'Riya', last: 'Sharma', phone: '9876501234', email: 'riya.sharma@example.com', dob: '1996-04-12', gender: 'Female', addr: 'Sector 14, Gurugram', ctc: 300000, city, loc: hub, dept: 'Front Office', desig: 'Front Office Supervisor', manager: LM[0] },
      experience: [{ company: 'Hotel Orchid', designation: 'Front Desk Associate', from: '2021-06', to: '2024-03' }] };
    await call('onb.check', HRBP, 'POST', '/api/candidates/check', sl);
    await call('onb.check-bad', HRBP, 'POST', '/api/candidates/check', { data: { first: 'X', phone: '12', dob: '2015-01-01', manager: 'EMP-1004' }, experience: [{ company: 'A', designation: 'B', from: '2025-05', to: '2024-01' }] });
    await call('onb.shortlist-by-employee', CON, 'POST', '/api/candidates', sl);
    const s = await call('onb.shortlist', HRBP, 'POST', '/api/candidates', sl);
    const si = id(s);
    await upload('onb.resume', HRBP, 'candidate', si, 'resume');
    await call('onb.shortlist-again', HRBP, 'POST', '/api/candidates', sl);
    await approveAll('onb.shortlist', s.body?.request?.id ?? 'MISSING', 'Agreed');
    await get('onb.after-shortlist', HRBP, `/api/candidates/${si}`);
    await call('onb.interview-no-feedback', LM, 'POST', `/api/candidates/${si}/interview`, { date: today, result: 'Selected' });
    const iv = await call('onb.interview', LM, 'POST', `/api/candidates/${si}/interview`, { date: today, result: 'Selected', feedback: 'Calm and clear with guests' });
    const oid = iv.body?.offerId ?? iv.body?.candidateId ?? 'MISSING'; // the Offer ID now comes at acceptance (hiring.go)
    await get('onb.offer-stage', HRBP, `/api/candidates/${oid}`);
    const od = { first: 'Riya', last: 'Sharma', type: 'Permanent', doj: today, dept: 'Front Office', desig: 'Front Office Supervisor', grade: 'G3', city, loc: hub, gross: 26000, phone: '9876501234', email: 'riya.sharma@example.com', pfWant: 'Yes' };
    await call('onb.offer-without-candidate', HRBP, 'POST', '/api/offers', { data: od });
    await call('onb.offer-check', HRBP, 'POST', '/api/offers/check', { data: od, candidate: si });
    await upload('onb.aadhaar', HRBP, 'candidate', si, 'aadhaar');
    const o = await call('onb.offer', HRBP, 'POST', '/api/offers', { data: od, candidate: si });
    const offer = id(o);
    await approveAll('onb.offer', o.body?.request?.id ?? 'MISSING', 'Agreed');
    await call('onb.accept-before-release', HRBP, 'POST', `/api/offers/${offer}/accept`);
    await call('onb.release', HRBP, 'POST', `/api/offers/${offer}/release`);
    await call('onb.accept', HRBP, 'POST', `/api/offers/${offer}/accept`);
    await call('onb.joined-future', HRBP, 'POST', `/api/candidates/${si}/joined`, { date: day(1) });
    await call('onb.joined', HRBP, 'POST', `/api/candidates/${si}/joined`, { date: today });
    await call('onb.send-early', HRBP, 'POST', `/api/offers/${offer}/send`);
    await call('onb.joining-bad', HRBP, 'POST', `/api/offers/${offer}/joining`, { data: { aadhaar: '123' } });
    await call('onb.joining', HRBP, 'POST', `/api/offers/${offer}/joining`, { data: { gender: 'Female', dob: '1996-04-12', aadhaar: '123412341234', pan: 'abcde1234f', uanAvail: 'No', bankName: 'HDFC Bank', acct: '50100123456789', ifsc: 'HDFC0001234', shift: 'G', attMode: 'Biometric', l1: LM[0] } });
    for (const k of ['pan', 'photo', 'police', 'bankProof']) await upload('onb.doc', HRBP, 'candidate', si, k);
    const v = await call('onb.send', HRBP, 'POST', `/api/offers/${offer}/send`);
    const vid = id(v);
    await call('onb.verify-payroll', PAY, 'POST', `/api/approvals/${vid}/approve`);
    await call('onb.verify-manager', PAYM, 'POST', `/api/approvals/${vid}/approve`);
    const x = await get('onb.done', HRBP, `/api/candidates/${si}`);
    await get('onb.offer-detail', HRBP, `/api/offers/${offer}`);
    log(`onboarding: ${si} → ${oid} → ${x.body?.current_id ?? '?'} (${x.body?.bucket ?? '?'})`);
    // a second candidate the hiring manager rejects
    const s2 = await call('onb.shortlist2', HRBP, 'POST', '/api/candidates', { ...sl, data: { ...sl.data, first: 'Asha', last: 'Rao', phone: '9876502001', email: 'asha.rao@example.com' } });
    await call('onb.reject2', LM, 'POST', `/api/approvals/${s2.body?.request?.id ?? 'MISSING'}/reject`, { note: 'Not enough experience' });
  }

  // ---- inventory: stock in, to the HRBP's store, uniform and a laptop issued (TestSeparationAndInventory) ----
  const hrbpStore = 'hrbp:EMP-1004';
  let laptopAsset = 'MISSING';
  {
    const inv = await get('inv.items', HR, '/api/inventory?store=central');
    const items = Object.fromEntries((inv.body?.items || []).map((i) => [i.name, i.id]));
    const shirt = items.Shirt ?? 0, laptop = items.Laptop ?? 0;
    await call('inv.add-by-hrbp', HRBP, 'POST', '/api/inventory/stock/add', { to: hrbpStore, lines: [{ itemId: shirt, qty: 5 }] });
    await call('inv.add', HR, 'POST', '/api/inventory/stock/add', { to: 'central', lines: [{ itemId: shirt, qty: 10 }, { itemId: laptop, serials: ['lp-1', 'LP-2'] }] });
    await call('inv.transfer', HR, 'POST', '/api/inventory/stock/transfer', { from: 'central', to: hrbpStore, lines: [{ itemId: shirt, qty: 4 }, { itemId: laptop, serials: ['LP-1'] }] });
    await call('inv.issue-too-many', HRBP, 'POST', '/api/inventory/issue', { store: hrbpStore, employeeId: EMP[0], lines: [{ itemId: shirt, qty: 5 }] });
    await call('inv.issue-uniform', HRBP, 'POST', '/api/inventory/issue', { store: hrbpStore, employeeId: EMP[0], lines: [{ itemId: shirt, qty: 2 }] });
    const st = await get('inv.store', HRBP, `/api/inventory?store=${encodeURIComponent(hrbpStore)}`);
    laptopAsset = (st.body?.assets || []).find((a) => a.serial === 'LP-1')?.id ?? 'MISSING';
    await call('inv.issue-laptop', HRBP, 'POST', '/api/inventory/issue', { store: hrbpStore, employeeId: EMP[0], assetId: laptopAsset });
    await get('inv.employee', HRBP, `/api/inventory/employee/${EMP[0]}`);
  }

  // ---- pay items: advance, deduction, arrear (TestPayroll) and a salary hold ----
  let adv = 'MISSING';
  {
    // 13 instalments: refused by both builds (the Postgres build 403, an employee could not ask; since 12 Nov 2026 the
    // employee may ask for their own advance, so the MongoDB build answers 400 for the instalments). A valid body would
    // create an advance only in one build and move every later id.
    await call('pay.adv-by-employee', EMP, 'POST', '/api/approvals', { wf: 'p_adv', employeeId: EMP[0], payload: { amount: 5000, instalments: 13, note: 'self' } });
    await call('pay.adv-contractual', PAY, 'POST', '/api/approvals', { wf: 'p_adv', employeeId: CON[0], payload: { amount: 5000, instalments: 2, note: 'test' } });
    const a = await call('pay.adv', PAY, 'POST', '/api/approvals', { wf: 'p_adv', employeeId: EMP[0], payload: { amount: 12000, instalments: 4, payMonth: open, startYm: addMonths(open, 1), note: 'Medical emergency' } });
    adv = id(a);
    await approveAll('pay.adv', adv, 'payroll test');
    await call('pay.ded-not-heard', PAY, 'POST', '/api/approvals', { wf: 'p_ded', employeeId: EMP[0], payload: { amount: 6000, instalments: 3, dedType: 'Cash shortage', note: 'Shortage at the till on 3 Oct' } });
    const d = await call('pay.ded', PAY, 'POST', '/api/approvals', { wf: 'p_ded', employeeId: EMP[0], payload: { amount: 6000, instalments: 3, dedType: 'Cash shortage', note: 'Shortage at the till on 3 Oct', heard: 'Yes' } });
    await approveAll('pay.ded', id(d), 'payroll test');
    const ar = await call('pay.arrear', PAY, 'POST', '/api/approvals', { wf: 'p_arrear', employeeId: EMP[0], payload: { amount: 3500, forYm: addMonths(open, -1), arrearType: 'Missed payment', slipText: 'Shift allowance not paid' } });
    await approveAll('pay.arrear', id(ar), 'payroll test');
    await get('pay.upcoming', PAY, '/api/payroll/upcoming');
    await call('hold.no-note', PAY, 'POST', '/api/holds', { employeeId: 'EMP-1016', reason: 'Bank or KYC issue', note: 'x' });
    const h = await call('hold.add', PAY, 'POST', '/api/holds', { employeeId: 'EMP-1016', reason: 'Bank or KYC issue', note: 'Account closed by the bank' });
    await get('hold.file', PAY, '/api/employees/EMP-1016/file');
    await call('hold.release', PAY, 'POST', `/api/holds/${id(h)}/release`, { note: 'New account verified' });
  }

  // ---- bulk update: employee master changes (TestBulkUpdate) ----
  {
    const upd = 'Employee ID,Work City,Reporting Location,Department,Designation,Grade,Shift,Reporting Manager L1,Functional Manager L2,Work Policy,Attendance Mode,Reason for change,Effective From\n' +
      `EMP-1018,,,,,,,EMP-1009,,,,Reporting line change,${today}\n` +
      `EMP-1017,,,,,G9,,,,,,Promotion,${today}\n` +
      `EMP-9999,,,,,,G,,,,,Transfer,${today}\n`;
    await sendFile('bulk.upd-check', HR, '/api/bulk/upd/check', 'upd.csv', upd);
    await sendFile('bulk.upd-apply', HR, '/api/bulk/upd/apply', 'upd.csv', upd);
    const band = 'City,Grade,Minimum Gross,Maximum Gross\nGurugram,G1,15000,9000\nGurugram,G1,15000,22000\n';
    await sendFile('bulk.band-apply', HR, '/api/bulk/band/apply', 'band.csv', band);
    await sendFile('bulk.band-by-employee', CON, '/api/bulk/band/check', 'band.csv', band);
  }

  // ---- payroll run: clear the checklist, close it, dry run, edit, verify, approve, publish ----
  {
    const cl = await get('run.checklist', PAY, `/api/payroll/${open}/checklist`);
    const closures = cl.body?.closures || [];
    const att = closures.find((c) => c.key === 'attendance');
    // The days nobody raised: the attendance HR uploads for them (Bulk update › Attendance), the shift's times.
    const rows = (att?.items || []).filter((it) => it.group === 'Not raised' && it.date)
      .map((it) => { const sh = shifts[staff[it.employeeId]?.shift_code] || { start_time: '09:00', end_time: '18:00' }; return `${it.employeeId},${it.date},${sh.start_time},${sh.end_time}`; })
      .sort();
    log(`payroll ${open}: ${rows.length} attendance days uploaded to clear the checklist`);
    const csv = 'Employee ID,Date,In time,Out time\n' + rows.join('\n') + '\n';
    await sendFile('run.att-check', HR, '/api/bulk/att/check', 'att.csv', csv);
    await sendFile('run.att-apply', HR, '/api/bulk/att/apply', 'att.csv', csv);
    for (const k of ['attendance', 'leave']) {
      if ((closures.find((c) => c.key === k)?.waiting || 0) > 0) await call(`run.reject-waiting`, PAY, 'POST', `/api/payroll/${open}/closures/${k}/reject-waiting`, { all: true });
    }
    const cl2 = await get('run.checklist2', PAY, `/api/payroll/${open}/checklist`);
    for (const c of cl2.body?.closures || []) {
      if (['bank', 'claim', 'deduction'].includes(c.key) && c.status !== 'Closed') {
        for (const it of c.items || []) if (it.requestId) await approveAll('run.clear', it.requestId, 'Checked');
      }
    }
    await call('run.dry-run-by-employee', EMP, 'POST', `/api/payroll/${open}/dry-run`);
    await call('run.dry-run-early', PAY, 'POST', `/api/payroll/${open}/dry-run`);
    for (const k of ['attendance', 'leave', 'bank', 'claim', 'deduction']) await call(`run.close`, PAY, 'POST', `/api/payroll/${open}/closures/${k}/close`);
    await get('run.checklist-closed', PAY, `/api/payroll/${open}/checklist`);
    await call('run.reg-locked', EMP, 'POST', '/api/approvals', { wf: 'regularize', payload: { date: open + '-15', rtype: 'No Punch', in: '09:00', out: '18:00', reason: 'Forgot to punch' } });
    await call('run.dry-run', PAY, 'POST', `/api/payroll/${open}/dry-run`);
    await get('run.rows', PAY, `/api/payroll/${open}/rows?q=EMP-1020`);
    await call('run.edit-no-reason', PAY, 'PATCH', `/api/payroll/${open}/rows/EMP-1020`, { paidDays: 20 });
    await call('run.edit', PAY, 'PATCH', `/api/payroll/${open}/rows/EMP-1020`, { paidDays: 20, otherEarn: 500, reason: 'Late joining on site' });
    await get('run.row', PAY, `/api/payroll/${open}/rows/EMP-1020`);
    await call('run.reopen', PAYM, 'POST', `/api/payroll/${open}/closures/attendance/reopen`, { reason: 'Late punches to check' });
    await call('run.verify-reopened', PAY, 'POST', `/api/payroll/${open}/verify`);
    await call('run.reclose', PAY, 'POST', `/api/payroll/${open}/closures/attendance/close`);
    await get('run.checks', PAY, `/api/payroll/${open}/checks`);
    await call('run.verify', PAY, 'POST', `/api/payroll/${open}/verify`);
    await call('run.approve-by-payroll', PAY, 'POST', `/api/payroll/${open}/approve`);
    await call('run.approve', PAYM, 'POST', `/api/payroll/${open}/approve`);
    await call('run.publish', PAYM, 'POST', `/api/payroll/${open}/publish`);
    await get('run.state', PAY, '/api/payroll');
    await get('run.payslips', EMP, '/api/payslips');
    await get('run.payslip', EMP, `/api/payslips/${open}/EMP-1020`);
    await get('run.payslip-contractual', CON, `/api/payslips/${open}/EMP-1014`);
    await get('run.payslip-other', CON, `/api/payslips/${open}/EMP-1020`);
    await call('run.adv-skip', PAY, 'POST', `/api/advances/${adv}/skip`);
    await get('run.upcoming', PAY, '/api/payroll/upcoming');
  }

  // ---- company policy acknowledgement (policy_ack_test) ----
  {
    const doc = await upload('policy.doc', HR, 'policy', 'DRAFT-0123456789ab', 'document');
    await call('policy.by-employee', EMP, 'POST', '/api/company-policies', { title: 'Code of conduct', version: '1.0', effectiveFrom: '2026-01-01', docId: id(doc) });
    await call('policy.no-audience', HR, 'POST', '/api/company-policies', { title: 'Code of conduct', version: '1.0', effectiveFrom: '2026-01-01', audience: { all: false }, docId: id(doc) });
    const p = await call('policy.create', HR, 'POST', '/api/company-policies', { title: 'Code of conduct', version: '1.0', effectiveFrom: '2026-01-01', audience: { all: true }, newJoiners: true, docId: id(doc) });
    const pid = id(p);
    await get('policy.draft-mine', EMP, '/api/me/policies');
    await call('policy.publish', HR, 'POST', `/api/company-policies/${pid}/publish`);
    const m = await get('policy.mine', EMP, '/api/me/policies');
    const it = (m.body?.items || [])[0] || {};
    const ver = it.version_id != null ? String(it.version_id) : 'MISSING';
    if (it.policy_doc) files.add(it.policy_doc);
    await call('policy.submit-policy-file', EMP, 'POST', `/api/me/policies/${ver}/submit`, { docId: it.policy_doc ?? 'MISSING' });
    const signed = await upload('policy.signed', EMP, 'policy_ack', `${EMP[0]}/${ver}`, 'signed');
    const s = await call('policy.submit', EMP, 'POST', `/api/me/policies/${ver}/submit`, { docId: id(signed) });
    await call('policy.submit-again', EMP, 'POST', `/api/me/policies/${ver}/submit`, { docId: id(signed) });
    await call('policy.lm-approve', LM, 'POST', `/api/approvals/${id(s)}/approve`, { note: 'Signed copy checked' });
    await get('policy.after', EMP, '/api/me/policies');
    await get('policy.detail', HR, `/api/company-policies/${pid}`);
  }

  // ---- nominations (TestNominations) ----
  {
    const mine = await get('nom.mine', EMP, '/api/me/nominations');
    const sid = String((mine.body || [])[0]?.scheme?.id ?? 'MISSING');
    const people = [{ name: 'Asha Verma', rel: 'Spouse', dob: '1995-02-01', share: 60 }, { name: 'Kabir Verma', rel: 'Son', dob: '2020-03-01', share: 30 }];
    await call('nom.incomplete', EMP, 'POST', `/api/me/nominations/${sid}`, { people });
    people[0].share = 70; people[1].guardian = 'Asha Verma';
    for (const k of ['n0:aadhaar', 'n1:aadhaar', 'e:form']) await upload('nom.doc', EMP, 'nomination', `${EMP[0]}/${sid}`, k);
    await call('nom.submit', EMP, 'POST', `/api/me/nominations/${sid}`, { people });
    await call('nom.submit-again', EMP, 'POST', `/api/me/nominations/${sid}`, { people });
    const q = await get('nom.queue', PAY, '/api/nominations');
    const nid = String((q.body?.submitted || [])[0]?.id ?? 'MISSING');
    await call('nom.return-no-note', PAY, 'POST', `/api/nominations/${nid}/return`, {});
    await call('nom.verify', PAY, 'POST', `/api/nominations/${nid}/verify`);
  }

  // ---- tax declaration and proof (TestTaxAndStatutory, first half) ----
  {
    const t = await get('tax.me', ['EMP-1002', 'Employee'], '/api/tax/me');
    const w = t.body?.windows || {};
    await call('tax.windows', HR, 'PUT', '/api/tax/windows', { declFrom: w.declFrom, declTo: w.declTo, declForce: true, proofFrom: w.proofFrom, proofTo: w.proofTo, proofForce: true });
    await call('tax.contractual', CON, 'PUT', '/api/tax/me', { regime: 'New', items: {} });
    const decl = { regime: 'Old', items: { c_ppf: 150000, d_self: 25000, hra: 240000 }, submit: true };
    await call('tax.no-landlord', ['EMP-1002', 'Employee'], 'PUT', '/api/tax/me', decl);
    await call('tax.declare', ['EMP-1002', 'Employee'], 'PUT', '/api/tax/me', { ...decl, landlordName: 'Ramesh Kapoor', landlordPan: 'ABCPK1234F' });
    const doc = await upload('tax.proof-doc', ['EMP-1002', 'Employee'], 'employee', 'EMP-1002', 'PPF passbook');
    const pf = await call('tax.proof', ['EMP-1002', 'Employee'], 'POST', '/api/tax/me/proofs', { item: 'c_ppf', doc: id(doc), amount: 150000, remark: 'FY deposit' });
    await call('tax.proof-approve', PAY, 'POST', `/api/tax/proofs/${id(pf)}/approve`, { amount: 100000 });
    await get('tax.list', PAY, '/api/tax');
  }

  // ---- resignation → terms → approval → clearance → assets back → exit interview (TestSeparationAndInventory) ----
  {
    const r = await call('resign.raise', EMP, 'POST', '/api/approvals', { wf: 'resign', payload: { reason: 'Relocation' } });
    const req = id(r), sep = id(r, 'case');
    const d = await get('resign.case', EMP, `/api/separations/${sep}`);
    const notice = d.body?.request?.noticeLwd || today;
    const shift = (n) => { const x = new Date(notice + 'T12:00:00Z'); x.setUTCDate(x.getUTCDate() + n); return x.toISOString().slice(0, 10); };
    await call('resign.terms-later', LM, 'POST', `/api/separations/${sep}/terms`, { lwd: shift(3), shortfall: 'waive' });
    await call('resign.terms', LM, 'POST', `/api/separations/${sep}/terms`, { lwd: shift(-10), shortfall: 'recover' });
    await approveAll('resign', req, 'separation test');
    const c = await get('resign.notice', LM, `/api/separations/${sep}`);
    const items = (c.body?.clearance || []).filter((x) => x.auto == null);
    for (const [owner, who] of [['Reporting manager', EMP], ['Reporting manager', LM], ['HRBP', HRBP], ['Finance', PAY]]) {
      for (const it of items.filter((x) => x.owner === owner)) await call('resign.clearance', who, 'POST', `/api/separations/${sep}/clearance/${it.id}`);
    }
    await call('resign.fnf-early', PAY, 'POST', '/api/fnf/EMP-1020');
    await call('resign.laptop-back', HRBP, 'POST', '/api/inventory/return', { store: hrbpStore, assetId: laptopAsset });
    await call('resign.interview', EMP, 'POST', `/api/separations/${sep}/interview`, { mainReason: 'Relocation', ratings: { Manager: 4, 'Work and shifts': 3, 'Pay and benefits': 2, Growth: 3, 'Workplace and team': 4 }, recommend: 'Yes' });
    await get('resign.after', HRBP, `/api/separations/${sep}`);
    await get('resign.fnf-preview', PAY, '/api/fnf/EMP-1020');
    await call('resign.close-early', HRBP, 'POST', `/api/separations/${sep}/close`, { rehire: 'Eligible' });
  }

  // ---- HR-raised exits: termination, absconding, a resignation withdrawn ----
  {
    await call('term.no-note', HRBP, 'POST', '/api/separations', { employeeId: 'EMP-1016', type: 'Termination', reason: 'Misconduct', noticeOption: 'pay' });
    const t = await call('term.raise', HRBP, 'POST', '/api/separations', { employeeId: 'EMP-1016', type: 'Termination', reason: 'Misconduct', noticeOption: 'pay', note: 'Repeated cash shortages after two warnings' });
    await get('term.hidden-from-employee', ['EMP-1020', 'Employee'], '/api/me/separation');
    await approveAll('term', t.body?.requestId ?? 'MISSING', 'separation test');
    await call('term.by-employee', EMP, 'POST', '/api/separations', { employeeId: 'EMP-1018', type: 'Termination' });
    const a = await call('abscond.raise', HRBP, 'POST', '/api/separations', { employeeId: 'EMP-1022', type: 'Absconding', note: 'Not reachable on phone since Monday' });
    await call('abscond.show-cause', HRBP, 'POST', `/api/separations/${id(a)}/show-cause`);
    await call('abscond.show-cause-again', HRBP, 'POST', `/api/separations/${id(a)}/show-cause`);
    await call('abscond.reported-back', HRBP, 'POST', `/api/separations/${id(a)}/reported-back`);
    const w = await call('withdraw.raise', HR, 'POST', '/api/separations', { employeeId: 'EMP-1017', type: 'Resignation', reason: 'Higher studies' });
    await call('withdraw', HR, 'POST', `/api/separations/${id(w)}/withdraw`);
    await get('withdraw.case', HR, `/api/separations/${id(w)}`);
  }
}
