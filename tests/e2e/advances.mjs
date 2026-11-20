// Salary advances in the browser (advances.go, owner's request of 12 Nov 2026):
//   1. an employee (EMP-1020) asks for an advance from My pay › Salary advance, with the rules and the projected
//      recovery on the form (the full amount in the first month; the wish for more months goes in the reason);
//   2. the approvers approve it in turn, the first (usually the manager) setting 3 instalments (owner, 14 Nov 2026); Payroll finds it from Employee dues (Advances first, with the line pointing
//      to Approvals), changes the recovery at its own step, and approves: the advance reaches the pay lines;
//   3. Payroll downloads the opening advances template, fills it, uploads it, checks the preview and confirms.
// Run on a throwaway database (a fresh `hrms seed -sample`, see README.md).
//   HRMS_WEB=http://localhost:5194 SHOTS=/some/dir npm run advances
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-advances-shots');
const DL = fs.mkdtempSync(path.join(os.tmpdir(), 'hrms-advances-dl-'));
fs.mkdirSync(SHOTS, { recursive: true });
const PW = 'Saltstayz#2026';
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const ymd = (d = new Date()) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const addYm = (ym, n) => { const d = new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1 + n, 1); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`; };
const ymLabel = (ym) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 15).toLocaleString('en-IN', { month: 'long', year: 'numeric' });

// ---------- API ----------
const tokens = {};
const raw = (p, b) => fetch(BASE + '/api' + p, { method: 'POST', body: JSON.stringify(b) }).then((r) => r.json());
async function token(id) {
  if (tokens[id]) return tokens[id];
  let r = await raw('/auth/login', { employeeId: id, password: id === 'EMP-1002' || id === 'EMP-1020' ? 'pass1234' : PW });
  if (!r.token) {
    const o = await raw('/auth/otp', { employeeId: id, purpose: 'first' });
    const v = await raw('/auth/otp/verify', { employeeId: id, code: o.devOtp });
    r = await raw('/auth/password', { employeeId: id, password: PW, verifyToken: v?.verifyToken });
  }
  return (tokens[id] = r.token);
}
async function api(who, role, p, body, method) {
  const res = await fetch(BASE + '/api' + p, {
    method: method || (body !== undefined ? 'POST' : 'GET'),
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${await token(who)}`, 'X-Role': role },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(`${method || 'POST'} ${p} as ${role}: ${res.status} ${data.error} ${JSON.stringify(data.details || '')}`);
  return data;
}

// ---------- A tiny .xlsx editor (zip + sheet XML), so the test needs no spreadsheet package ----------
function unzip(buf) {
  let e = buf.length - 22;
  while (e >= 0 && buf.readUInt32LE(e) !== 0x06054b50) e--;
  const n = buf.readUInt16LE(e + 10);
  let p = buf.readUInt32LE(e + 16);
  const out = [];
  for (let i = 0; i < n; i++) {
    const method = buf.readUInt16LE(p + 10), csize = buf.readUInt32LE(p + 20), nl = buf.readUInt16LE(p + 28), xl = buf.readUInt16LE(p + 30), cl = buf.readUInt16LE(p + 32);
    const off = buf.readUInt32LE(p + 42), name = buf.toString('utf8', p + 46, p + 46 + nl);
    const lnl = buf.readUInt16LE(off + 26), lxl = buf.readUInt16LE(off + 28), start = off + 30 + lnl + lxl;
    const data = buf.subarray(start, start + csize);
    out.push({ name, data: method === 8 ? zlib.inflateRawSync(data) : Buffer.from(data) });
    p += 46 + nl + xl + cl;
  }
  return out;
}
function zip(files) {
  const locals = [], centrals = [];
  let off = 0;
  for (const f of files) {
    const name = Buffer.from(f.name), comp = zlib.deflateRawSync(f.data), crc = zlib.crc32(f.data);
    const h = Buffer.alloc(30);
    h.writeUInt32LE(0x04034b50, 0); h.writeUInt16LE(20, 4); h.writeUInt16LE(8, 8); h.writeUInt32LE(crc, 14); h.writeUInt32LE(comp.length, 18); h.writeUInt32LE(f.data.length, 22); h.writeUInt16LE(name.length, 26);
    const c = Buffer.alloc(46);
    c.writeUInt32LE(0x02014b50, 0); c.writeUInt16LE(20, 4); c.writeUInt16LE(20, 6); c.writeUInt16LE(8, 10); c.writeUInt32LE(crc, 16); c.writeUInt32LE(comp.length, 20); c.writeUInt32LE(f.data.length, 24);
    c.writeUInt16LE(name.length, 28); c.writeUInt32LE(off, 42);
    locals.push(h, name, comp); centrals.push(c, name);
    off += 30 + name.length + comp.length;
  }
  const cd = Buffer.concat(centrals), end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0); end.writeUInt16LE(files.length, 8); end.writeUInt16LE(files.length, 10); end.writeUInt32LE(cd.length, 12); end.writeUInt32LE(off, 16);
  return Buffer.concat([...locals, cd, end]);
}
const xmlText = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const unXml = (s) => s.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&apos;/g, "'").replace(/&amp;/g, '&');
/** Opens a sheet by name: sheets, get(ref), set(ref, text), save() → Buffer. */
function openSheet(buf, sheetName) {
  const files = unzip(buf);
  const wb = files.find((f) => f.name === 'xl/workbook.xml').data.toString();
  const rels = files.find((f) => f.name === 'xl/_rels/workbook.xml.rels').data.toString();
  const sheets = [...wb.matchAll(/<sheet[^>]*name="([^"]+)"/g)].map((m) => unXml(m[1]));
  const rid = new RegExp(`<sheet[^>]*name="${sheetName}"[^>]*r:id="([^"]+)"`).exec(wb)[1];
  const target = new RegExp(`<Relationship[^>]*Id="${rid}"[^>]*Target="([^"]+)"`).exec(rels)?.[1] ?? new RegExp(`<Relationship[^>]*Target="([^"]+)"[^>]*Id="${rid}"`).exec(rels)[1];
  const sheet = files.find((f) => f.name === 'xl/' + target.replace(/^\/?xl\//, ''));
  const ssf = files.find((f) => f.name === 'xl/sharedStrings.xml');
  const ss = ssf ? [...ssf.data.toString().matchAll(/<si>([\s\S]*?)<\/si>/g)].map((m) => unXml([...m[1].matchAll(/<t[^>]*>([\s\S]*?)<\/t>/g)].map((t) => t[1]).join(''))) : [];
  let xml = sheet.data.toString();
  const cellRe = (ref) => new RegExp(`<c r="${ref}"([^>]*?)(?:/>|>([\\s\\S]*?)</c>)`);
  return {
    sheets,
    get(ref) {
      const m = cellRe(ref).exec(xml); if (!m) return '';
      const attrs = m[1], inner = m[2] || '';
      if (/t="s"/.test(attrs)) return ss[+/<v>(\d+)<\/v>/.exec(inner)[1]];
      if (/t="inlineStr"/.test(attrs)) return unXml(/<t[^>]*>([\s\S]*?)<\/t>/.exec(inner)?.[1] ?? '');
      return unXml(/<v>([\s\S]*?)<\/v>/.exec(inner)?.[1] ?? '');
    },
    set(ref, text) {
      const m = cellRe(ref).exec(xml);
      if (!m) throw new Error('no cell ' + ref);
      const s = /s="(\d+)"/.exec(m[1]);
      xml = xml.replace(m[0], `<c r="${ref}"${s ? ` s="${s[1]}"` : ''} t="inlineStr"><is><t>${xmlText(text)}</t></is></c>`);
    },
    save() { sheet.data = Buffer.from(xml); return zip(files); },
  };
}

// ---------- Browser ----------
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
const errors = [];
page.on('pageerror', (e) => errors.push('PAGE ERROR ' + e.message.split('\n')[0]));
page.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 500) errors.push(`${r.status()} ${r.url()}`); });
const cdp = await page.createCDPSession();
await cdp.send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: DL, eventsEnabled: true });
const shot = async (name) => { await page.screenshot({ path: path.join(SHOTS, name + '.png'), fullPage: true }); console.log('     screenshot', name); };
const text = () => page.evaluate(() => document.body.innerText);
const until = async (fn, what, ms = 8000) => { const t0 = Date.now(); while (Date.now() - t0 < ms) { if (await fn().catch(() => false)) return true; await wait(150); } check(false, what + ' (timed out)'); return false; };
const has = (t) => until(async () => (await text()).includes(t), `the page shows “${t}”`);
const clickText = (sel, t) => page.evaluate((sel, t) => { const b = [...document.querySelectorAll(sel)].filter((x) => !x.disabled).find((x) => x.textContent.trim().startsWith(t)); b?.click(); return !!b; }, sel, t);
/** Sets a .field control by its caption, so React sees the change. */
const fill = (label, value) => page.evaluate((label, value) => {
  const f = [...document.querySelectorAll('label.field, div.field')].find((l) => l.querySelector('span')?.textContent.trim().startsWith(label));
  const el = f?.querySelector('input:not([type=file]), select, textarea');
  if (!el) return false;
  const proto = el.tagName === 'SELECT' ? HTMLSelectElement.prototype : el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return true;
}, label, value);
async function signIn(id, role) {
  const t = await token(id);
  await page.goto(BASE + '/api/health');
  await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role);
}

const state = await api('EMP-1011', 'Payroll', '/payroll');
const open = state.openYm;
console.log('open payroll month:', open);

// 1. The employee asks for an advance.
await signIn('EMP-1020', 'Employee');
await page.goto(BASE + '/me?tab=adv');
await has('Request a salary advance');
const form = await api('EMP-1020', 'Employee', '/me/advance');
check(form.eligible === true && form.maxAmount > 0, `EMP-1020 may ask, up to ${form.maxAmount}`);
const rules = await page.$eval('[data-testid="adv-rules"]', (el) => el.innerText).catch(() => '');
check(rules.includes('You can ask for up to') && rules.includes('One advance at a time') && rules.includes(`recovered in full from your ${ymLabel(form.startYm)} salary`) && rules.includes('Your approver sets the instalments'), 'the rules are on the form');
check(!(await fill('Recover in', '3')), 'the employee does not pick the instalments');
await fill('Amount', '6000');
await fill('Reason and recovery wish', 'Medical bills for my mother, please deduct in 3 months');
await until(async () => (await page.$eval('[data-testid="adv-projection"]', (el) => el.innerText).catch(() => '')).includes('One deduction of ₹6,000'), 'the projected recovery: one deduction of ₹6,000');
await until(async () => (await text()).includes('Approval route'), 'the approval route is shown');
await shot('1440-employee-advance-form');
await clickText('button', 'Send for approval');
await until(async () => (await text()).includes('Pending'), 'the advance is listed as pending');
const mine = (await api('EMP-1020', 'Employee', '/me/requests?wf=all')).filter((r) => r.wf === 'p_adv');
const adv = mine[0]?.id;
check(!!adv && mine[0].status === 'Pending' && mine[0].canCancel === true, `${adv} is pending and the employee can cancel it`);
check(mine[0]?.payload.payMonth === form.payMonth && mine[0]?.payload.startYm === form.startYm, 'the server set the pay month and the first recovery month');
check(mine[0]?.payload.instalments === 1, 'stored with 1 instalment (the full amount in the first month)');
const again = await api('EMP-1020', 'Employee', '/me/advance');
check(again.eligible === false && String(again.why).includes(adv), 'one at a time: the form says the open advance is waiting');
await page.goto(BASE + '/');
await until(async () => (await text()).includes('My requests'), 'My view dashboard');
await until(async () => (await text()).includes(adv), 'My requests lists the advance', 12000);

// 2. The approvers approve it in turn, up to Payroll.
const STAGE_ROLE = { 'Functional Manager': 'Line Manager' };
for (let i = 0; i < 8; i++) {
  const r = await api('EMP-1002', 'HR Admin', `/approvals/${adv}`);
  if (r.status !== 'Pending') break;
  const stage = r.chain[r.step];
  if (stage === 'Payroll') break;
  const person = r.people[r.step]?.person;
  const hits = person ? (await api('EMP-1002', 'HR Admin', `/employees?q=${encodeURIComponent(person)}&size=5`)).rows : [];
  const who = hits.find((e) => e.name === person)?.id;
  let done = false;
  // The first approver reads the wish in the reason and sets 3 monthly instalments.
  const body = i === 0 ? { note: 'Agreed', instalments: 3 } : { note: 'Agreed' };
  if (who) {
    try { await api(who, STAGE_ROLE[stage] || stage, `/approvals/${adv}/approve`, body); done = true; console.log(`     ${stage}: approved by ${person} (${who})`); } catch (e) { console.log('     ' + e.message); }
  }
  if (!done) { await api('EMP-1002', 'HR Admin', `/approvals/${adv}/approve`, { ...body, note: `On behalf of ${stage}` }); console.log(`     ${stage}: approved by HR Admin on behalf`); }
}
const set3 = await api('EMP-1002', 'HR Admin', `/approvals/${adv}`);
check(set3.payload.instalments === 3 && set3.history.some((h) => String(h.note).includes('when approving: 3 monthly instalments')), 'the first approver set 3 instalments, in the history');
const atPay = await api('EMP-1002', 'HR Admin', `/approvals/${adv}`);
check(atPay.status === 'Pending' && atPay.chain[atPay.step] === 'Payroll', `${adv} waits on Payroll`);

// Payroll: Employee dues opens on Advances, with the line pointing to Approvals.
await signIn('EMP-1011', 'Payroll');
await page.goto(BASE + '/dues');
await until(async () => (await page.$eval('.tabs .tab.on', (el) => el.textContent.trim()).catch(() => '')) === 'Advances', 'Employee dues opens on Advances for Payroll');
const firstTab = await page.$eval('.tabs .tab', (el) => el.textContent.trim()).catch(() => '');
check(firstTab === 'Advances', 'Advances is the first tab for Payroll');
await until(async () => (await page.$eval('[data-testid="adv-waiting"]', (el) => el.innerText).catch(() => '')).includes('1 advance request is waiting for approval'), 'the line says one advance request is waiting');
check((await text()).includes('Approvals › Payroll approvals'), 'the line points to Approvals › Payroll approvals');
await shot('1440-payroll-dues-advances');
await clickText('a', 'Approvals › Payroll approvals');
await until(async () => page.url().includes('/approvals?tab=pay'), 'the link opens Payroll approvals');
await page.goto(BASE + `/approvals?id=${adv}`);
await until(async () => !!(await page.$('[data-testid="adv-terms"]')), 'the request shows Change recovery for Payroll');
await clickText('button', 'Change recovery');
await fill('Instalments', '4');
await shot('1440-payroll-change-recovery');
await clickText('button', 'Save the change');
await has('Recovery changed by Sunita Rao (Payroll): 3 instalments');
const changed = await api('EMP-1011', 'Payroll', `/approvals/${adv}`);
check(changed.payload.instalments === 4, 'the advance now has 4 instalments');
await clickText('button', 'Approve');
await until(async () => (await api('EMP-1011', 'Payroll', `/approvals/${adv}`)).status !== 'Pending' || (await api('EMP-1011', 'Payroll', `/approvals/${adv}`)).chain[(await api('EMP-1011', 'Payroll', `/approvals/${adv}`)).step] !== 'Payroll', 'Payroll approves');
// Larger advances have more stages after Payroll; approve any that are left as HR Admin.
for (let i = 0; i < 4; i++) {
  const r = await api('EMP-1002', 'HR Admin', `/approvals/${adv}`);
  if (r.status !== 'Pending') break;
  await api('EMP-1002', 'HR Admin', `/approvals/${adv}/approve`, { note: 'Agreed' });
}
const list = await api('EMP-1011', 'Payroll', '/pay-items/advances');
const item = list.items.find((a) => a.id === adv);
const earn = item?.lines.filter((l) => l.kind === 'earn') ?? [], ded = item?.lines.filter((l) => l.kind === 'ded') ?? [];
check(item?.status === 'Approved', `${adv} is approved`);
check(earn.length === 1 && earn[0].amount === 6000, 'the advance is paid as one Salary advance line');
check(ded.length === 4 && ded.reduce((s, l) => s + l.amount, 0) === 6000 && ded[0].ym === addYm(earn[0]?.ym ?? open, 1), 'four recovery lines from the month after');

// 3. Opening advances: template, fill, upload, preview, confirm.
await page.goto(BASE + '/dues?tab=adv');
await has('Opening advances');
for (const f of fs.readdirSync(DL)) fs.unlinkSync(path.join(DL, f));
await clickText('button', 'Download template');
let xlsx = '';
await until(async () => { xlsx = fs.readdirSync(DL).find((f) => f.endsWith('.xlsx')) || ''; return !!xlsx; }, 'the template downloads', 15000);
const book = openSheet(fs.readFileSync(path.join(DL, xlsx)), 'Opening advances');
check(book.sheets.includes('How to fill'), 'the template has How to fill');
check(['Employee ID', 'Employee name', 'Advance date', 'Original amount', 'Balance still to recover', 'Monthly recovery', 'Recovery starts', 'Reference / reason'].every((h, i) => book.get(String.fromCharCode(65 + i) + '1') === h), 'the template has the eight columns');
const perm = (await api('EMP-1002', 'HR Admin', '/employees?type=Permanent&status=Active&size=50')).rows.map((e) => e.id).filter((id) => id !== 'EMP-1020');
const [e1, e2] = perm;
const past = ymd(new Date(Date.now() - 120 * 864e5));
const rowsIn = [
  [e1, 'first', past, '24000', '15000', '5000', open, 'Voucher 101'],
  [e2, 'second', past, '10000', '7000', '2000', addYm(open, 1), 'Register page 7'],
  ['EMP-9999', 'nobody', past, '5000', '3000', '1000', open, 'Voucher 102'],
  [e1, 'first again', past, '24000', '15000', '5000', open, 'voucher 101'],
];
rowsIn.forEach((r, i) => r.forEach((v, j) => book.set(String.fromCharCode(65 + j) + (i + 2), v)));
const filled = path.join(DL, 'opening-filled.xlsx');
fs.writeFileSync(filled, book.save());
const input = await page.$('input[type=file][accept=".xlsx"]');
await input.uploadFile(filled);
await has('Check before you confirm');
const pv = await text();
check(pv.includes('Will add: 2') && pv.includes('Skipped: 2'), 'preview: will add 2, skipped 2');
check(pv.includes('Unknown Employee ID'), 'preview: unknown employee skipped with the reason');
check(pv.includes('Same employee and reference as row 2'), 'preview: the repeated row is skipped');
check(pv.includes('Balance to recover: ₹22,000'), 'preview: the total balance');
await shot('1440-opening-preview');
check(!(await api('EMP-1011', 'Payroll', '/pay-items/advances')).items.some((a) => a.payload.opening), 'nothing is added before Confirm');
await clickText('.dlg button', 'Confirm: add 2');
await has('Added: 2');
await shot('1440-opening-result');
await clickText('.dlg button', 'Close');
await until(async () => (await text()).includes('Opening balance'), 'the Advances list tags them Opening balance');
await shot('1440-advances-list');
const after = (await api('EMP-1011', 'Payroll', '/pay-items/advances')).items.filter((a) => a.payload.opening);
const o1 = after.find((a) => a.employee_id === e1);
check(after.length === 2 && o1 && o1.balance === 15000 && o1.lines.every((l) => l.kind === 'ded') && o1.lines.map((l) => l.amount).join(',') === '5000,5000,5000', 'opening advances: no earning line, the balance in monthly recoveries');

// The employee sees the opening advance in My view.
await signIn(e1, 'Employee');
const myPay = await api(e1, 'Employee', '/me/dashboard?tab=pay');
check(myPay.items.some((x) => x.id === o1?.id && String(x.kind).includes('opening balance') && x.balance === 15000), `${e1} sees the opening advance with its balance in My view`);

// Phone width: the employee's form.
await signIn('EMP-1020', 'Employee');
await page.setViewport({ width: 390, height: 844 });
await page.goto(BASE + '/me?tab=adv');
await has('My advances');
await shot('390-employee-advance');
const wide = await page.evaluate(() => document.documentElement.scrollWidth);
check(wide <= 400, `no sideways scroll at 390 px (${wide})`);

await browser.close();
for (const e of [...new Set(errors)]) check(false, e);
console.log(`\n${fails.length ? 'FAILED ' + fails.length : 'ALL PASSED'} · screenshots in ${SHOTS}`);
process.exitCode = fails.length ? 1 : 0;
