// Uniform sizes in the browser (uniform.go, owner's request of 13 Nov 2026):
//   1. HR Admin sets an employee's uniform sizes on the employee record (Employees › record › Uniform sizes);
//   2. issues uniform from Employee dues › Uniform: the employee's sizes are shown and pre-fill each item's size by its
//      size type, and the issue records the sizes;
//   3. the Uniform sizes summary counts active employees by size, lists those with none, and downloads as Excel;
//   4. Setup: the refund basis switches between "after the last deduction" and "months of employment", and the
//      issue form says the rule;
//   5. the employee sees the recovery and the expected refund month in My view › Payroll.
// Run on a throwaway database (a fresh `hrms seed -sample`, see README.md).
//   HRMS_WEB=http://localhost:5194 SHOTS=/some/dir npm run uniform
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-uniform-shots');
const DL = fs.mkdtempSync(path.join(os.tmpdir(), 'hrms-uniform-dl-'));
fs.mkdirSync(SHOTS, { recursive: true });
const PW = 'Saltstayz#2026';
const EMP = 'EMP-1020';
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
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
const cdp = await page.createCDPSession();
await cdp.send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: DL, eventsEnabled: true });
const shot = async (name) => { await page.screenshot({ path: path.join(SHOTS, name + '.png'), fullPage: true }); console.log('     screenshot', name); };
const text = () => page.evaluate(() => document.body.innerText);
const until = async (fn, what, ms = 8000) => { const t0 = Date.now(); while (Date.now() - t0 < ms) { if (await fn().catch(() => false)) return true; await wait(150); } check(false, what + ' (timed out)'); return false; };
const has = (t, ms) => until(async () => (await text()).includes(t), `the page shows “${t}”`, ms);
const clickText = (sel, t) => page.evaluate((sel, t) => { const b = [...document.querySelectorAll(sel)].filter((x) => !x.disabled).find((x) => x.textContent.trim().startsWith(t)); b?.click(); return !!b; }, sel, t);
/** Sets a control so React sees the change: by a .field caption, or by a CSS selector when `sel` starts with "css:". */
const fill = (label, value) => page.evaluate((label, value) => {
  let el;
  if (label.startsWith('css:')) el = document.querySelector(label.slice(4));
  else {
    const f = [...document.querySelectorAll('label.field, div.field')].reverse().find((l) => l.querySelector('span')?.textContent.trim().startsWith(label));
    el = f?.querySelector('input:not([type=file]), select, textarea');
  }
  if (!el) return false;
  const proto = el.tagName === 'SELECT' ? HTMLSelectElement.prototype : el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, value);
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return true;
}, label, value);
const valueOf = (sel) => page.$eval(sel, (el) => el.value).catch(() => null);
async function signIn(id, role) {
  const t = await token(id);
  await page.goto(BASE + '/api/health');
  await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role);
}
const tab = async (name) => { await until(() => clickText('.tabs .tab', name), `the ${name} tab`); };

// Stock for the issue, in the store EMP-1020 is served from (their HRBP's, else the central store).
const setup0 = await admin('/inventory/setup');
check(setup0.refundBasis === 'deduction' && setup0.rule === '6 months after the last deduction', `default refund rule: ${setup0.rule}`);
const item = (name) => setup0.items.find((i) => i.name === name && i.kind === 'uniform');
const [shirt, trousers, shoes] = [item('Shirt'), item('Trousers'), item('Shoes')];
check(shirt?.size_type === 'shirt' && trousers?.size_type === 'trouser' && shoes?.size_type === 'shoes', 'Shirt, Trousers and Shoes are tagged by size type');
const held = await admin(`/inventory/employee/${EMP}`);
const lines = [{ itemId: shirt.id, qty: 10 }, { itemId: trousers.id, qty: 10 }, { itemId: shoes.id, qty: 10 }];
await admin('/inventory/stock/add', { to: 'central', lines, note: 'e2e uniform' });
if (held.store !== 'central') await admin('/inventory/stock/transfer', { from: 'central', to: held.store, lines });

// 1. HR Admin sets the sizes on the employee record.
await signIn('EMP-1002', 'HR Admin');
await page.goto(BASE + '/employees');
await page.waitForSelector('input[placeholder="Search name or employee ID"]');
await page.type('input[placeholder="Search name or employee ID"]', EMP);
await until(() => page.evaluate((id) => { const r = [...document.querySelectorAll('tr.click')].find((x) => x.textContent.includes(id)); r?.click(); return !!r; }, EMP), `${EMP} in the list`);
await until(async () => !!(await page.$('[data-testid="uniform-block"]')), 'the record shows Uniform sizes', 12000);
check((await text()).includes('No sizes on record yet'), 'no sizes on record yet');
await clickText('[data-testid="uniform-block"] button', 'Add sizes');
await has('Uniform sizes: ');
check(await fill('Shirt / T-shirt', 'M') && await fill('Trouser waist', '32') && await fill('Trouser length', '30') && await fill('Shoe size', '8'), 'the four size drop-downs');
await shot('1440-employee-sizes-dialog');
await clickText('.dlg button', 'Save sizes');
await until(async () => (await page.$eval('[data-testid="uniform-block"]', (el) => el.innerText).catch(() => '')).includes('32 in'), 'the record shows the sizes');
const mine = await admin(`/employees/${EMP}/uniform`);
check(mine.sizes.shirt === 'M' && mine.sizes.waist === '32' && mine.sizes.length === '30' && mine.sizes.shoe === '8', 'the sizes are saved');
const hist = await admin(`/employees/${EMP}/history`);
check(hist.filter((h) => String(h.field).startsWith('Uniform:')).length === 4, 'each size is in the change history');
await shot('1440-employee-record-sizes');

// 2. Issue uniform: the sizes pre-fill each item.
await page.goto(BASE + '/dues?tab=uni');
const PICK = 'input[role=combobox][aria-label="Employee"]';
await page.waitForSelector(PICK);
await page.click(PICK);
await page.type(PICK, EMP, { delay: 30 });
await until(() => page.evaluate((id) => { const o = [...document.querySelectorAll('[role=option]')].find((x) => x.dataset.id === id); o?.dispatchEvent(new MouseEvent('mousedown', { bubbles: true })); o?.click(); return !!o; }, EMP), 'pick the employee');
await until(async () => (await page.$eval('[data-testid="issue-sizes"]', (el) => el.innerText).catch(() => '')).includes('Shirt / T-shirt M'), 'the issue form shows the sizes on record');
await until(async () => (await valueOf('select[aria-label="Shirt size"]')) === 'M', 'Shirt pre-filled with M');
check((await valueOf('select[aria-label="Trousers waist"]')) === '32' && (await valueOf('select[aria-label="Trousers length"]')) === '30', 'Trousers pre-filled with waist 32 and length 30');
check((await valueOf('select[aria-label="Shoes size"]')) === '8', 'Shoes pre-filled with UK 8');
// Quantities: 2 shirts (one changed to L at issue below is a separate line), 1 trousers, 1 shoes, nothing else.
for (const i of setup0.items.filter((x) => x.kind === 'uniform' && x.status === 'Active')) await fill(`css:input[aria-label="${i.name} quantity"]`, '0');
await fill('css:input[aria-label="Shirt quantity"]', '2');
await fill('css:input[aria-label="Trousers quantity"]', '1');
await fill('css:input[aria-label="Shoes quantity"]', '1');
await fill('css:select[aria-label="Shoes size"]', '9'); // the size can be changed at issue
check((await text()).includes('Refunded in full 6 months after the last deduction'), 'the issue form says the refund rule');
await shot('1440-issue-uniform-sizes');
await clickText('button', 'Issue and schedule recovery');
let issue;
await until(async () => { issue = (await admin(`/inventory/uniform-issues?employeeId=${EMP}`))[0]; return !!issue; }, 'the issue is recorded', 12000);
const got = Object.fromEntries((issue?.lines ?? []).map((l) => [l.name, `${l.qty}:${l.size ?? ''}`]));
check(got.Shirt === '2:M' && got.Trousers === '1:32/30' && got.Shoes === '1:9' && Object.keys(got).length === 3, `issued by size: ${JSON.stringify(got)}`);
check(issue?.refundStatus === 'scheduled' && !!issue?.refundYm && issue?.refundRule === '6 months after the last deduction', `refund scheduled for ${issue?.refundYm}`);
await until(async () => (await text()).includes('2 × Shirt (M)'), 'the issue list shows the sizes');
await shot('1440-uniform-issues');

// 3. The Uniform sizes summary.
await tab('Uniform sizes');
await until(async () => !!(await page.$('[data-testid="size-groups"]')), 'the summary opens', 12000);
const sum = await admin('/inventory/sizes');
const shirtM = sum.groups.find((g) => g.key === 'shirt').rows.find((r) => r.size === 'M').count;
check(sum.recorded >= 1 && shirtM >= 1 && sum.missing.length === sum.total - sum.recorded, `summary: ${sum.recorded} of ${sum.total} with sizes, ${shirtM} in M`);
check((await text()).includes(`No sizes on record (${sum.missing.length})`), 'the summary lists those with no sizes');
await shot('1440-uniform-sizes-summary');
for (const f of fs.readdirSync(DL)) fs.unlinkSync(path.join(DL, f));
await clickText('button', 'Download Excel');
let xlsx = '';
await until(async () => { xlsx = fs.readdirSync(DL).find((f) => f.endsWith('.xlsx')) || ''; return !!xlsx; }, 'the summary downloads as Excel', 15000);
check(xlsx && fs.readFileSync(path.join(DL, xlsx)).subarray(0, 2).toString() === 'PK', `downloaded ${xlsx}`);
const loc = sum.locations[0];
await fill('css:select[aria-label="Location"]', loc.id);
await until(async () => (await text()).includes('Trousers by waist and length') && (await admin(`/inventory/sizes?loc=${encodeURIComponent(loc.id)}`)).total < sum.total, `narrowed to ${loc.name}`);

// 4. Setup: switch the refund basis.
await tab('Setup');
await has('Refund basis');
check(await fill('Refund basis', 'service'), 'choose months of employment');
await until(async () => (await admin('/inventory/setup')).refundBasis === 'service', 'the basis is saved');
await until(async () => (await page.$eval('[data-testid="refund-rule"]', (el) => el.innerText).catch(() => '')).includes('after 6 months of employment'), 'the rule says months of employment');
await fill('Refund after (months of employment)', '12');
await page.evaluate(() => document.activeElement?.blur());
await page.evaluate(() => [...document.querySelectorAll('label.field input')].find((x) => x.closest('label')?.textContent.includes('months of employment'))?.dispatchEvent(new FocusEvent('focusout', { bubbles: true })));
await until(async () => (await admin('/inventory/setup')).serviceMonths === 12, '12 months saved');
check((await page.$eval('select[aria-label="Shirt size by"]', (el) => el.value).catch(() => '')) === 'shirt', 'Setup shows each item’s size type');
await shot('1440-setup-refund-basis');
await tab('Uniform');
await has('Refunded in full after 12 months of employment');
await shot('1440-issue-form-employment-rule');
await tab('Setup');
await has('Refund basis');
await fill('Refund basis', 'deduction');
await until(async () => (await admin('/inventory/setup')).refundBasis === 'deduction', 'back to months after the last deduction');
check((await admin(`/inventory/uniform-issues?employeeId=${EMP}`))[0]?.refundYm === issue?.refundYm, 'the issue made before keeps its refund month');

// 5. The employee sees the recovery and the expected refund.
await signIn(EMP, 'Employee');
await page.goto(BASE + '/');
await until(() => clickText('.tabs .tab', 'Payroll'), 'My view › Payroll');
await until(async () => !!(await page.$('[data-testid="my-uniform"]')), 'My uniform is shown', 12000);
const my = await page.$eval('[data-testid="my-uniform"]', (el) => el.innerText);
check(my.includes('Expected refund') && my.includes(ymLabel(issue.refundYm)) && my.includes('to be recovered from salary'), 'My view shows the recovery and the expected refund month');
check(my.includes('Trouser waist 32 in'), 'My view shows the sizes on record');
await shot('1440-my-uniform');
await page.setViewport({ width: 390, height: 844 });
await page.reload();
await until(() => clickText('.tabs .tab', 'Payroll'), 'My view › Payroll at phone width');
await until(async () => !!(await page.$('[data-testid="my-uniform"]')), 'My uniform at phone width', 12000);
await shot('390-my-uniform');
const wide = await page.evaluate(() => document.documentElement.scrollWidth);
check(wide <= 400, `no sideways scroll at 390 px (${wide})`);

await browser.close();
for (const e of [...new Set(errors)]) check(false, e);
console.log(`\n${fails.length ? 'FAILED ' + fails.length : 'ALL PASSED'} · screenshots in ${SHOTS}`);
process.exitCode = fails.length ? 1 : 0;
