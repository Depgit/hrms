// Documents from existing employees (backend doc_collect.go), in the browser: the employee (EMP-1020, imported
// with the sample organisation, so their window is open from the first start) uploads two documents from My view ›
// My documents; HR Admin opens Employees › Document collection, verifies one and sends the other back with a
// reason; the employee sees the reason, cannot replace the verified one and uploads the other again; HR extends
// the window and the employee sees the new last day. Run on a throwaway database (see README.md).
//   HRMS_WEB=http://localhost:5196 SHOTS=/some/dir npm run docs
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-docs-shots');
const DL = fs.mkdtempSync(path.join(os.tmpdir(), 'hrms-docs-dl-'));
fs.mkdirSync(SHOTS, { recursive: true });
const PW = 'Saltstayz#2026';
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const ymd = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const fmtD = (s) => new Date(+s.slice(0, 4), +s.slice(5, 7) - 1, +s.slice(8, 10)).toLocaleDateString('en-IN', { day: '2-digit', month: 'short', year: 'numeric' });

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
  if (!res.ok) throw new Error(`${method || 'POST'} ${p} as ${role}: ${res.status} ${data.error}`);
  return data;
}
const PDF = path.join(DL, 'scan.pdf');
fs.writeFileSync(PDF, '%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n');

// ---------- Browser ----------
const EMP = 'EMP-1020';
const start = await api(EMP, 'Employee', '/me/documents');
check(start.window?.open === true, `the window is open for ${EMP} (${start.window?.daysLeft} days left)`);
const mand = start.docs.filter((d) => d.mandatory && !d.byHR);
check(mand.length >= 2, 'at least two mandatory documents to upload');
const [A, B] = mand;
const name = start.employee.name;

const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
const errors = [];
page.on('pageerror', (e) => errors.push('PAGE ERROR ' + e.message.split('\n')[0]));
page.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 500) errors.push(`${r.status()} ${r.url()}`); });
const cdp = await page.createCDPSession();
await cdp.send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: DL, eventsEnabled: true });
const shot = async (n) => { await page.screenshot({ path: path.join(SHOTS, n + '.png'), fullPage: true }); console.log('     screenshot', n); };
const text = () => page.evaluate(() => document.body.innerText);
const until = async (fn, what, ms = 8000) => { const t0 = Date.now(); while (Date.now() - t0 < ms) { if (await fn()) return true; await wait(150); } check(false, what + ' (timed out)'); return false; };
const clickText = (sel, t) => page.evaluate((sel, t) => { const b = [...document.querySelectorAll(sel)].find((x) => x.textContent.trim().startsWith(t)); b?.click(); return !!b; }, sel, t);
/** Clicks a button whose text starts with t in the table row that names label. */
const clickInRow = (label, t) => page.evaluate((label, t) => {
  const tr = [...document.querySelectorAll('tr')].find((r) => r.querySelector('td b')?.textContent.trim() === label);
  const b = tr && [...tr.querySelectorAll('button')].find((x) => x.textContent.trim().startsWith(t));
  b?.click(); return !!b;
}, label, t);
const rowText = (label) => page.evaluate((label) => [...document.querySelectorAll('tr')].find((r) => r.querySelector('td b')?.textContent.trim() === label)?.innerText ?? '', label);
async function signIn(id, role) {
  const t = await token(id);
  await page.goto(BASE + '/api/health');
  await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role);
}
async function uploadIn(label) {
  const input = await page.$(`input[aria-label="Upload ${label}"]`);
  if (!input) { check(false, `an upload button for ${label}`); return; }
  await input.uploadFile(PDF);
  await until(async () => (await rowText(label)).includes('waiting for HR check'), `${label} is uploaded and waits for HR`);
}

// 1. The employee: the dashboard card, then My documents.
await signIn(EMP, 'Employee');
await page.goto(BASE + '/');
await until(async () => (await text()).includes('Upload your documents'), 'the My view dashboard asks for the documents');
await page.goto(BASE + '/my-documents');
await until(async () => (await text()).includes('days left') && (await text()).includes(A.label), 'My documents lists the documents and the days left');
const t1 = await text();
check(t1.includes('Mandatory') && t1.includes('Optional'), 'mandatory and optional are marked');
await shot('1440-my-documents-empty');
await uploadIn(A.label);
await uploadIn(B.label);
check((await text()).includes(`2 of ${start.counts.mandatory}`), `the count says 2 of ${start.counts.mandatory}`);
await shot('1440-my-documents-uploaded');

// 2. HR Admin: Employees › Document collection.
await signIn('EMP-1002', 'HR Admin');
await page.goto(BASE + '/employees');
await until(async () => (await text()).includes('Document collection'), 'Employees has a Document collection tab');
await clickText('.tab', 'Document collection');
await until(async () => (await text()).includes(name), `the tracker lists ${name}`);
check((await rowText(name)).includes(`2 of ${start.counts.mandatory}`), 'the tracker shows 2 uploaded');
check((await text()).includes('Overdue') && (await text()).includes('Due in 7 days') && (await text()).includes('Complete'), 'the filters are there');
await shot('1440-hr-tracker');
await page.evaluate((n) => [...document.querySelectorAll('tr.click')].find((r) => r.innerText.includes(n))?.click(), name);
await until(async () => (await text()).includes('mandatory uploaded') && !!(await page.$('.dlg')), 'the employee\'s documents open');
// Verify A.
check(await clickInRow(A.label, 'Verify'), `Verify on ${A.label}`);
await until(async () => (await rowText(A.label)).includes('Verified'), `${A.label} is verified`);
// Send back B with a reason.
check(await clickInRow(B.label, 'Send back'), `Send back on ${B.label}`);
await until(async () => !!(await page.$('textarea[aria-label="Reason"]')), 'Send back asks for a reason');
await page.type('textarea[aria-label="Reason"]', 'The scan is cut off; upload the full page');
await clickText('.dlg .btn.primary', 'Send back');
await until(async () => (await rowText(B.label)).includes('Sent back'), `${B.label} is sent back`);
check((await rowText(B.label)).includes('The scan is cut off'), 'the reason shows');
await shot('1440-hr-review');
await clickText('.dlg button', 'Close');
await wait(400);

// 3. The employee: the reason, no Replace on the verified one, upload again.
await signIn(EMP, 'Employee');
await page.goto(BASE + '/my-documents');
await until(async () => (await rowText(B.label)).includes('Sent back'), 'the employee sees it was sent back');
check((await rowText(B.label)).includes('The scan is cut off'), 'the employee sees the reason');
check((await rowText(A.label)).includes('Verified') && !(await page.$(`input[aria-label="Upload ${A.label}"]`)), 'a verified document cannot be replaced');
const bell = await api(EMP, 'Employee', '/notices');
check(bell.notices.some((n) => n.text.includes('sent back your')), 'the bell has the message');
await shot('1440-my-documents-sent-back');
await uploadIn(B.label);
check(!(await rowText(B.label)).includes('Sent back'), 'the new upload replaces the sent-back one');

// 4. HR extends the window for the employee.
await signIn('EMP-1002', 'HR Admin');
await page.goto(BASE + '/employees?tab=docs');
await until(async () => (await text()).includes(name), 'the tracker opens from the link');
const before = await api('EMP-1002', 'HR Admin', `/doc-collection/${EMP}`);
await page.click(`input[aria-label="Select ${name}"]`);
await clickText('button', 'Extend (1)');
await until(async () => !!(await page.$('input[aria-label="Extra days"]')), 'Extend asks for the days');
await page.click('input[aria-label="Extra days"]', { clickCount: 3 });
await page.type('input[aria-label="Extra days"]', '10');
await shot('1440-hr-extend');
await clickText('.dlg .btn.primary', 'Extend by 10');
await until(async () => (await rowText(name)).includes('+10 days'), 'the tracker shows the 10 extra days');
const after = await api('EMP-1002', 'HR Admin', `/doc-collection/${EMP}`);
const d0 = new Date(+before.window.due.slice(0, 4), +before.window.due.slice(5, 7) - 1, +before.window.due.slice(8, 10) + 10);
check(after.window.due === ymd(d0), `the last day moved by 10 days (${before.window.due} → ${after.window.due})`);
// Excel.
for (const f of fs.readdirSync(DL)) if (f.endsWith('.xlsx')) fs.unlinkSync(path.join(DL, f));
await clickText('button', 'Export to Excel');
let xlsx = '';
await until(async () => { xlsx = fs.readdirSync(DL).find((f) => f.endsWith('.xlsx')) || ''; return !!xlsx; }, 'the tracker downloads as Excel', 15000);
check(xlsx && fs.statSync(path.join(DL, xlsx)).size > 2000, `Excel file ${xlsx}`);

// 5. The employee sees the new last day.
await signIn(EMP, 'Employee');
await page.goto(BASE + '/my-documents');
await until(async () => (await text()).includes(fmtD(after.window.due)), 'the employee sees the new last day');
await page.setViewport({ width: 390, height: 844 });
await page.reload();
await until(async () => (await text()).includes(A.label), 'My documents on a phone');
await shot('390-my-documents');
const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
check(overflow <= 1, `no sideways scroll on a phone (${overflow}px)`);

await browser.close();
check(!errors.length, 'no page errors or server errors' + (errors.length ? ': ' + errors.slice(0, 5).join('; ') : ''));
console.log(`\n${fails.length ? fails.length + ' FAILED' : 'all passed'}; screenshots in ${SHOTS}`);
if (fails.length) process.exitCode = 1;
