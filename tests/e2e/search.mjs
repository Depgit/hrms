// The people picker (components/PersonPicker): type-to-search with approximate matches, in the browser.
//   1. Employee file (server search): a typo ("kapor") and initials ("ak") find Anil Kapoor; the keyboard picks
//      (↓, Enter) and the file opens; nothing found says so; Escape closes the list.
//   2. Attendance › Records (a list the page has, matched in the browser): "All employees" is pinned first, a typo in
//      a surname ("bajja") finds Rakesh Bajaj, and picking shows his month.
//   3. Phone width (390 px) and dark mode: the list fits the screen (no sideways scroll).
// It only reads, so it can share a seed with roles.mjs. Run on a throwaway database (a fresh `hrms seed -sample`).
//   HRMS_WEB=http://localhost:5194 SHOTS=/some/dir npm run search
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-search-shots');
fs.mkdirSync(SHOTS, { recursive: true });
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'light' }]);
const errors = [];
page.on('pageerror', (e) => errors.push('PAGE ERROR ' + e.message.split('\n')[0]));
page.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 500) errors.push(`${r.status()} ${r.url()}`); });
const shot = async (name) => { await page.screenshot({ path: path.join(SHOTS, name + '.png') }); console.log('     screenshot', name); };
const until = async (fn, ms = 8000) => { const t0 = Date.now(); for (;;) { const v = await page.evaluate(fn).catch(() => ''); if (v || Date.now() - t0 > ms) return v || ''; await wait(150); } };
/** The open list of a picker: its options' codes, in order (the pinned empty option as ''). */
const options = (sel) => page.evaluate((sel) => {
  const el = document.querySelector(sel), ul = el && document.getElementById(el.getAttribute('aria-controls'));
  return ul ? [...ul.querySelectorAll('[role=option]')].map((o) => o.dataset.id) : null;
}, sel);
const listText = (sel) => page.evaluate((sel) => { const el = document.querySelector(sel); return document.getElementById(el?.getAttribute('aria-controls'))?.textContent || ''; }, sel);
/** Types like a person: clears the box, then types the text key by key. */
const typeIn = async (sel, text) => {
  await page.click(sel, { clickCount: 3 });
  await page.keyboard.press('Backspace');
  await page.type(sel, text, { delay: 40 });
};
/** Waits for the picker's list to settle (the 200 ms pause and the answer) and returns the codes shown. */
const settled = async (sel) => {
  let last = null;
  for (let i = 0; i < 40; i++) {
    await wait(150);
    const busy = (await listText(sel)).includes('Searching');
    const o = await options(sel);
    if (!busy && o && JSON.stringify(o) === JSON.stringify(last)) return o;
    last = o;
  }
  return last ?? [];
};

const login = async (id, pw, role) => {
  const r = await fetch(BASE + '/api/auth/login', { method: 'POST', body: JSON.stringify({ employeeId: id, password: pw }) }).then((x) => x.json());
  await page.goto(BASE + '/api/health');
  await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, r.token, role);
};
await login('EMP-1002', 'pass1234', 'HR Admin');

// ---------- 1. Employee file: server search ----------
const FILE = 'input[role=combobox][aria-label="Employee"]';
await page.goto(BASE + '/file');
await page.waitForSelector(FILE);
await typeIn(FILE, 'kapor');
let o = await settled(FILE);
check(o[0] === 'EMP-1009', `typo "kapor" puts Anil Kapoor (EMP-1009) first: ${o.slice(0, 3).join(', ')}`);
check(await page.$eval(FILE, (el) => el.getAttribute('aria-expanded') === 'true' && !!el.getAttribute('aria-activedescendant')), 'combobox is expanded with an active option');
await shot('desktop-typo-kapor');
await typeIn(FILE, 'ak');
o = await settled(FILE);
check(o.includes('EMP-1009'), `initials "ak" find Anil Kapoor: ${o.slice(0, 5).join(', ')}`);
await shot('desktop-initials-ak');
await typeIn(FILE, 'kapoor anil');
o = await settled(FILE);
check(o[0] === 'EMP-1009', 'words in any order ("kapoor anil")');
await typeIn(FILE, '1008');
o = await settled(FILE);
check(o[0] === 'EMP-1008', 'the code without its prefix ("1008") finds EMP-1008');
// Keyboard: the first option is active; ↓ then ↑ comes back to it, Enter picks it and the file opens.
await typeIn(FILE, 'rakesh bajaj');
o = await settled(FILE);
check(o[0] === 'EMP-1008', '"rakesh bajaj" finds EMP-1008 first');
await page.keyboard.press('ArrowDown'); await page.keyboard.press('ArrowUp'); await page.keyboard.press('Enter');
check(!!(await until(() => (document.querySelector('h2')?.textContent || '').startsWith('Rakesh Bajaj'), 10000)), 'Enter picks the active option and the file opens');
check((await page.$eval(FILE, (el) => el.value)).includes('EMP-1008'), 'the box shows the chosen person (name · code)');
await typeIn(FILE, 'zzqqxx');
await settled(FILE);
check((await listText(FILE)).includes('No match in your access'), 'nothing found says "No match in your access"');
await page.keyboard.press('Escape');
check(await page.$eval(FILE, (el) => el.getAttribute('aria-expanded') === 'false'), 'Escape closes the list');

// ---------- 2. Attendance › Records: a list the page has ----------
await page.goto(BASE + '/attendance');
await until(() => [...document.querySelectorAll('.tabs .tab')].some((b) => b.textContent.trim() === 'Records'));
await page.evaluate(() => [...document.querySelectorAll('.tabs .tab')].find((b) => b.textContent.trim() === 'Records')?.click());
const ATT = 'input[role=combobox][aria-label="Employee"]';
await page.waitForSelector(ATT, { timeout: 15000 });
await page.click(ATT);
o = await settled(ATT);
check(o[0] === '' && (await listText(ATT)).startsWith('All employees'), 'the "All employees" option is pinned first');
await typeIn(ATT, 'bajja');
o = await settled(ATT);
check(o.includes('EMP-1008'), `typo "bajja" finds Rakesh Bajaj in the page's list: ${o.slice(0, 4).join(', ')}`);
await typeIn(ATT, 'rb');
o = await settled(ATT);
check(o.includes('EMP-1008'), `initials "rb" find Rakesh Bajaj: ${o.slice(0, 4).join(', ')}`);
await page.evaluate((sel) => { const el = document.querySelector(sel); document.getElementById(el.getAttribute('aria-controls'))?.querySelector('[data-id="EMP-1008"]')?.click(); }, ATT);
check(await page.$eval(ATT, (el) => el.closest('.pp')?.dataset.value === 'EMP-1008'), 'clicking an option picks it');

// ---------- 3. Phone width and dark mode ----------
await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
await page.goto(BASE + '/file');
await page.waitForSelector(FILE);
await typeIn(FILE, 'sandep rawat');
o = await settled(FILE);
check(o[0] === 'EMP-1002', `phone: typo "sandep rawat" finds Sandeep Rawat: ${o.slice(0, 3).join(', ')}`);
const fit = await page.evaluate((sel) => {
  const el = document.querySelector(sel), ul = document.getElementById(el.getAttribute('aria-controls')), r = ul.getBoundingClientRect();
  return { left: r.left, right: r.right, vw: window.innerWidth, sw: document.documentElement.scrollWidth };
}, FILE);
check(fit.left >= 0 && fit.right <= fit.vw && fit.sw <= fit.vw, `phone: the list fits the screen (${Math.round(fit.left)}–${Math.round(fit.right)} of ${fit.vw}, page ${fit.sw})`);
await shot('phone-390-typo');
await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]);
await typeIn(FILE, 'ak');
await settled(FILE);
await shot('phone-390-dark-initials');
await page.setViewport({ width: 1440, height: 900 });
await page.goto(BASE + '/file');
await page.waitForSelector(FILE);
await typeIn(FILE, 'meera kulkrni');
o = await settled(FILE);
check(o[0] === 'EMP-1004', `dark: typo "meera kulkrni" finds Meera Kulkarni first: ${o.slice(0, 3).join(', ')}`);
await shot('desktop-dark-typo');

check(!errors.length, 'no page errors or server errors' + (errors.length ? ': ' + errors.join('; ') : ''));
await browser.close();
console.log(fails.length ? `\n${fails.length} FAILED` : '\nALL PASSED');
process.exit(fails.length ? 1 : 0);
