// Bulk update for staff moved onto the HRMS mid-year (bulk_opening.go, owner's request of 13 Nov 2026), in the browser:
//   1. the template picker: templates grouped (Employees, Pay and dues, Leave and time), wrapped, no sideways scroll,
//      ?tpl= opens a template;
//   2. Opening uniform: the owner's case (₹5,000 given the month before the HRMS, 2 instalments) and others pasted,
//      checked (recovered before the HRMS, the HRMS deductions, the refund month, refused rows), applied, and shown on
//      the employee's Uniform card with the month "recovered before HRMS";
//   3. Opening advances: checked (what was recovered, the balance, a wrong closing month) and applied;
//   4. Opening leave balances and Uniform sizes are there for HR Admin;
//   5. the page at phone width (390 px) with no sideways scroll.
// Run on a throwaway database (a fresh `hrms seed -sample`, see README.md).
//   HRMS_WEB=http://localhost:5194 SHOTS=/some/dir npm run bulkopen
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-bulkopen-shots');
fs.mkdirSync(SHOTS, { recursive: true });
const PW = 'Saltstayz#2026';
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const ymAdd = (ym, n) => { const d = new Date(Date.UTC(+ym.slice(0, 4), +ym.slice(5, 7) - 1 + n, 1)); return d.toISOString().slice(0, 7); };
const MON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const short = (ym) => `${MON[+ym.slice(5, 7) - 1]} ${ym.slice(0, 4)}`;
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
const admin = (p, body, method) => api('EMP-1002', 'HR Admin', p, body, method);

// ---------- Browser ----------
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }]);
const errors = [];
page.on('pageerror', (e) => errors.push('PAGE ERROR ' + e.message.split('\n')[0]));
page.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 500) errors.push(`${r.status()} ${r.url()}`); });
// A full page is taken with the window made as tall as the page, so the fixed sidebar stays in place.
const shot = async (name, full = true) => {
  const vp = page.viewport();
  if (full) {
    const h = await page.evaluate(() => document.documentElement.scrollHeight);
    if (h > vp.height) { await page.setViewport({ ...vp, height: h }); await wait(250); }
  }
  await page.screenshot({ path: path.join(SHOTS, name + '.png') });
  if (page.viewport().height !== vp.height) await page.setViewport(vp);
  console.log('     screenshot', name);
};
const text = () => page.evaluate(() => document.body.innerText);
const until = async (fn, what, ms = 8000) => { const t0 = Date.now(); while (Date.now() - t0 < ms) { if (await fn().catch(() => false)) return true; await wait(150); } check(false, what + ' (timed out)'); return false; };
const has = (t, ms) => until(async () => (await text()).includes(t), `the page shows “${t}”`, ms);
const clickText = (sel, t) => page.evaluate((sel, t) => { const b = [...document.querySelectorAll(sel)].filter((x) => !x.disabled).find((x) => x.textContent.trim().startsWith(t)); b?.click(); return !!b; }, sel, t);
const fill = (label, value) => page.evaluate((label, value) => {
  const f = [...document.querySelectorAll('label.field, div.field')].reverse().find((l) => l.querySelector('span')?.textContent.trim().startsWith(label));
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
const pickTpl = async (name) => {
  await until(() => clickText('[data-testid="bulk-picker"] .seg button', name), `pick “${name}”`);
  await until(async () => (await page.$eval('[data-testid="bulk-template"] h3', (el) => el.textContent).catch(() => '')) === name, `the ${name} template opens`);
};
const result = () => page.$eval('[data-testid="bulk-result"]', (el) => el.innerText).catch(() => '');
const noSideways = () => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);

// The first HRMS deduction month: the open payroll month on a fresh seed (no Deduction closure closed).
const first = (await api('EMP-1002', 'HR Admin', '/pay-items/deductions')).openYm;
const prev = ymAdd(first, -1);
const refund = ymAdd(first, 6); // 6 months after the last deduction (the first HRMS month)
console.log(`     first HRMS deduction month ${first}`);
// The company allows an advance of up to 3 × the monthly gross (the sample's ₹30,000 example is above 1 ×).
await admin('/advances/settings', { maxMultiple: 3, oneOpen: true }, 'PUT');

// 1. The template picker.
await signIn('EMP-1002', 'HR Admin');
await page.goto(BASE + '/bulk');
await until(async () => !!(await page.$('[data-testid="bulk-picker"]')), 'the picker shows', 12000);
const groups = await page.$$eval('[data-testid="bulk-picker"] .bulk-group', (gs) => gs.map((g) => [g.querySelector('.bulk-glabel').textContent, [...g.querySelectorAll('button')].map((b) => b.textContent)]));
const by = Object.fromEntries(groups);
check(JSON.stringify(Object.keys(by)) === JSON.stringify(['Employees', 'Pay and dues', 'Leave and time']), `groups: ${Object.keys(by).join(', ')}`);
check(by.Employees?.includes('Uniform sizes') && by.Employees?.includes('Import employees'), `Employees: ${by.Employees?.join(', ')}`);
check(by['Pay and dues']?.includes('Opening uniform') && by['Pay and dues']?.includes('Opening advances'), `Pay and dues: ${by['Pay and dues']?.join(', ')}`);
check(by['Leave and time']?.includes('Opening leave balances'), `Leave and time: ${by['Leave and time']?.join(', ')}`);
check(!by['Leave and time']?.includes('Opening optional holiday balances') && !by['Pay and dues']?.includes('Variable payouts'), 'merged templates are not in the picker');
check(!(await page.$('.tabs')), 'no sideways-scrolling tab row');
check(await noSideways() <= 0, 'no sideways scroll at 1440 px');
await shot('01-template-picker', false);

// 2. Opening uniform.
await pickTpl('Opening uniform');
check(page.url().includes('tpl=openuni'), '?tpl= follows the template');
await has(`first deduction month is ${ymLabel(first)}`);
await shot('02-opening-uniform-template', false);
const uni = ['Employee ID,Issued month,Uniform amount (₹),Instalments,Items / note,Recover',
  `EMP-1020,${short(prev).replace(' ', '-')},5000,2,Shirt and trousers,Yes`, // the owner's case
  `EMP-1016,${first},3000,,2 shirts and 1 trousers,`, // Setup split, all in the HRMS
  `EMP-1014,${prev},2000,2,Contract staff kit,`, // not permanent: recorded only
  `EMP-1020,${ymAdd(first, -12)},1200,2,Old kit,`, // refunded before the HRMS
  `EMP-1016,${ymAdd(first, 2)},900,,,`, // future (when the first month is last month)
  `EMP-9999,${prev},1000,,,`].join('\n');
check(await fill('Paste or load CSV', uni), 'paste the opening uniform rows');
await clickText('button', 'Check file');
await until(async () => (await result()).includes('Recovered before HRMS'), 'the check result shows', 12000);
const ur = await result();
check(ur.includes(`Recovered before HRMS: ₹2,500: ${short(prev)} ₹2,500`), 'the owner’s case: September recovered before the HRMS');
check(ur.includes(`HRMS deductions: ${short(first)} ₹2,500`), 'the owner’s case: ₹2,500 deducted in the first HRMS month');
check(ur.includes(`Refund: ₹5,000 in ${ymLabel(refund)}`), `the owner’s case: refund in ${ymLabel(refund)}`);
check(ur.includes('recorded only'), 'contract staff: recorded only');
check(ur.includes('already refunded before the HRMS; nothing to load'), 'refused: already refunded before the HRMS');
check(ur.includes('EMP-9999 not found'), 'refused: unknown employee');
check((await text()).includes('Apply 3 valid rows'), 'three rows ready');
await shot('03-opening-uniform-check');
await clickText('button', 'Apply 3 valid rows');
let card;
await until(async () => { card = await admin('/employees/EMP-1020/uniform'); return card.issues.some((u) => u.opening); }, 'the opening uniform is applied', 12000);
const issue = card?.issues.find((u) => u.opening);
check(issue?.recovered === 2500 && issue?.toRecover === 2500 && issue?.refundYm === refund && issue?.refundStatus === 'scheduled', `stored: ${JSON.stringify({ r: issue?.recovered, t: issue?.toRecover, y: issue?.refundYm })}`);
check(issue?.schedule?.[0]?.before === true && issue?.schedule?.[0]?.ym === prev, 'the schedule starts with the month recovered before the HRMS');

// The employee's Uniform card.
await page.goto(BASE + '/employees');
await page.waitForSelector('input[placeholder="Search name or employee ID"]');
await page.type('input[placeholder="Search name or employee ID"]', 'EMP-1020');
await until(() => page.evaluate(() => { const r = [...document.querySelectorAll('tr.click')].find((x) => x.textContent.includes('EMP-1020')); r?.click(); return !!r; }), 'EMP-1020 in the list');
await until(async () => (await page.$eval('[data-testid="uniform-block"]', (el) => el.innerText).catch(() => '')).includes('recovered before HRMS'), 'the Uniform card shows “recovered before HRMS”', 12000);
const blk = await page.$eval('[data-testid="uniform-block"]', (el) => el.innerText);
check(blk.includes('Opening balance') && blk.includes('Shirt and trousers') && blk.includes(ymLabel(prev)), 'the card shows the opening issue, its note and month');
check(blk.includes(`Expected ₹5,000 in ${ymLabel(refund)}`), 'the card shows the expected refund');
// The same card in the employee payroll file (Employee file › Uniform), at full width.
await page.goto(BASE + '/file?id=EMP-1020');
await until(() => clickText('.tabs .tab', 'Uniform'), 'Employee file › Uniform', 12000);
await until(async () => (await page.$eval('[data-testid="uniform-block"]', (el) => el.innerText).catch(() => '')).includes('recovered before HRMS'), 'the file’s Uniform card', 12000);
check((await page.$eval('[data-testid="uniform-block"]', (el) => el.innerText)).includes('Expected'), 'the file’s card shows the refund column');
await shot('08-employee-uniform-card');

// 3. Opening advances, opened by ?tpl=.
await page.goto(BASE + '/bulk?tpl=openadv');
await until(async () => (await page.$eval('[data-testid="bulk-template"] h3', (el) => el.textContent).catch(() => '')) === 'Opening advances', '?tpl=openadv opens Opening advances', 12000);
await shot('04-opening-advances-template', false);
const adv = ['Employee ID,Advance amount (₹),Advance given month,Deduction start month,Monthly deduction (₹),Deduction closing month,Reference / reason',
  `EMP-1020,30000,${ymAdd(first, -4)},${ymAdd(first, -3)},5000,${ymAdd(first, 2)},Voucher 2231`,
  `EMP-1016,12000,${ymAdd(first, -3)},${ymAdd(first, -2)},5000,${first},`,
  `EMP-1017,10000,${prev},${prev},3000,${ymAdd(first, 1)},Wrong closing month`,
  `EMP-1018,6000,${ymAdd(first, -5)},${ymAdd(first, -4)},2000,${ymAdd(first, -2)},All recovered`,
  `EMP-1014,6000,${prev},${first},2000,${ymAdd(first, 2)},Contract staff`].join('\n');
check(await fill('Paste or load CSV', adv), 'paste the opening advance rows');
await clickText('button', 'Check file');
await until(async () => (await result()).includes('Balance to recover'), 'the advances check result shows', 12000);
const ar = await result();
check(ar.includes(`Recovered before HRMS: ₹15,000: ${short(ymAdd(first, -3))} ₹5,000`), '₹15,000 recovered before the HRMS');
check(ar.includes(`HRMS deductions: ${short(first)} ₹5,000 · ${short(ymAdd(first, 1))} ₹5,000 · ${short(ymAdd(first, 2))} ₹5,000`), 'three HRMS deductions of ₹5,000');
check(ar.includes(`HRMS deductions: ${short(first)} ₹2,000`), 'the remainder: ₹2,000 in the first HRMS month');
check(ar.includes(`Deduction closing month should be ${ymLabel(ymAdd(first, 2))}`), 'a wrong closing month names the month expected');
check(ar.includes('Nothing left to recover'), 'refused: nothing left to recover');
await shot('05-opening-advances-check');
await clickText('button', 'Apply 2 valid rows');
await until(async () => (await api('EMP-1002', 'HR Admin', '/pay-items/advances')).items.filter((a) => a.payload?.opening).length === 2, 'two opening advances are added', 12000);

// 4. Opening leave balances and Uniform sizes (once the "applied" message has gone).
await until(async () => !(await page.$('#toast')), 'the message goes', 6000);
await pickTpl('Opening leave balances');
await has('including all leave accrued up to that month');
await shot('06-opening-leave-balances', false);
await pickTpl('Uniform sizes');
await has('Shirt / T-shirt Size');
await shot('07-uniform-sizes', false);

// 5. Phone width.
await page.setViewport({ width: 390, height: 844 });
await page.goto(BASE + '/bulk?tpl=openuni');
await until(async () => !!(await page.$('[data-testid="bulk-picker"]')), 'the picker at phone width', 12000);
await wait(300);
check(await noSideways() <= 0, `no sideways scroll at 390 px (${await noSideways()})`);
await shot('09-picker-390');
check(await fill('Paste or load CSV', uni.split('\n').slice(0, 3).join('\n').replace(`EMP-1020,${short(prev).replace(' ', '-')}`, `EMP-1020,${ymAdd(first, -3)}`)), 'paste at phone width');
await clickText('button', 'Check file');
await until(async () => (await result()).includes('Recovered before HRMS'), 'the check result at phone width', 12000);
check(await noSideways() <= 0, `no sideways scroll at 390 px with the check result (${await noSideways()})`);
await shot('10-check-390');

await browser.close();
for (const e of [...new Set(errors)]) check(false, e);
console.log(`\n${fails.length ? 'FAILED ' + fails.length : 'ALL PASSED'} · screenshots in ${SHOTS}`);
process.exitCode = fails.length ? 1 : 0;
