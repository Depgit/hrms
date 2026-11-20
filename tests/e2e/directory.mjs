// My view › Employee directory in the browser (mydirectory.go, Directory.tsx; owner's request of 12 Nov 2026):
//   1. Everyone: the whole company with email (mail link and Copy), searched with words in any order and a typo,
//      filtered by department; no employee code column;
//   2. tapping a person opens their department's chart centred on them; back returns to the same search;
//   3. Departments: headcounts, the employee's own first and highlighted; tapping another opens its chart, Whole
//      department shows the tree;
//   4. the same on a phone (390 wide): cards instead of the table.
// Run on a throwaway database (a fresh `hrms seed -sample`, see README.md).
//   HRMS_WEB=http://localhost:5197 SHOTS=/some/dir npm run directory
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-directory-shots');
fs.mkdirSync(SHOTS, { recursive: true });
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

const login = await fetch(BASE + '/api/auth/login', { method: 'POST', body: JSON.stringify({ employeeId: 'EMP-1020', password: 'pass1234' }) }).then((r) => r.json());
if (!login.token) throw new Error('EMP-1020 could not sign in with the seeded password: run on a fresh `hrms seed -sample`');

const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
const problems = [];
page.on('pageerror', (e) => problems.push(`page error: ${e.message.split('\n')[0]}`));
page.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 400) problems.push(`${r.status()} ${r.url().replace(BASE, '')}`); });
await page.setViewport({ width: 1440, height: 900 });
await page.goto(BASE + '/api/health');
await page.evaluate((t) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', 'Employee'); }, login.token);

const text = () => page.evaluate(() => document.querySelector('.main')?.innerText ?? document.body.innerText);
const rows = () => page.evaluate(() => [...document.querySelectorAll('.main table tbody tr')].map((r) => [...r.cells].map((c) => c.innerText.trim())));
const until = async (fn, what, ms = 6000) => {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (await fn()) return true; await wait(150); }
  check(false, `timed out: ${what}`);
  return false;
};
const search = async (q) => {
  await page.$eval('.main input[type=search]', (el) => { el.focus(); el.select(); });
  await page.keyboard.press('Backspace');
  if (q) await page.type('.main input[type=search]', q);
  await wait(900);
};
const clickText = (sel, t) => page.evaluate((sel, t) => { const b = [...document.querySelectorAll(sel)].find((x) => x.textContent.trim().startsWith(t)); b?.click(); return !!b; }, sel, t);
const shot = (n) => page.screenshot({ path: path.join(SHOTS, `${n}.png`), fullPage: true });

// 1. Everyone
await page.goto(BASE + '/directory');
await until(async () => (await rows()).length > 0, 'the Everyone table');
const head = await page.evaluate(() => [...document.querySelectorAll('.main table thead th')].map((h) => h.textContent.trim()));
check(JSON.stringify(head) === JSON.stringify(['Name', 'Designation', 'Department', 'Location', 'Email']), `columns are Name, Designation, Department, Location, Email (${head})`);
const all = await rows();
check(all.length >= 40, `everyone listed (${all.length} on the first page)`);
const mails = await page.$$eval('.main a[href^="mailto:"]', (l) => l.length);
check(mails >= all.length - 2, `emails shown as mail links (${mails})`);
check(await page.$$eval('.main button[aria-label^="Copy "]', (l) => l.length) === mails, 'a Copy button by every email');
const tabs = await page.$$eval('.tabs .tab', (l) => l.map((b) => b.textContent.trim()));
check(tabs.join() === 'Everyone,Departments', `tabs Everyone and Departments (${tabs})`);

await search('kulkarni meera');
let r = await rows();
check(r[0]?.[0] === 'Meera Kulkarni', `words in any order find Meera Kulkarni (${r[0]?.[0]})`);
await search('merea kulkarn');
r = await rows();
check(r[0]?.[0] === 'Meera Kulkarni', `a typo and a partial word still find her (${r[0]?.[0]})`);
check(new URL(page.url()).searchParams.get('q') === 'merea kulkarn', 'the search is kept in the address');
await shot('everyone-search-desktop');

await search('');
await page.select('.main select', await page.$eval('.main select', (s) => [...s.options].find((o) => o.text.startsWith('Front Office'))?.value || ''));
await wait(900);
r = await rows();
check(r.length > 0 && r.every((x) => x[2] === 'Front Office'), `the department filter shows only Front Office (${r.length})`);
await page.select('.main select', '');
await wait(700);
await shot('everyone-desktop');

// 2. A person → their department's chart, centred on them; back keeps the search.
await search('merea kulkarn');
await page.click('.main table tbody tr');
await until(async () => (await text()).includes('Whole department'), 'the chart after tapping a person');
let t = await text();
const u = new URL(page.url());
check(!!u.searchParams.get('dept') && !!u.searchParams.get('p'), 'the chart is in the address (department and person)');
const on = await page.evaluate(() => [...document.querySelectorAll('.main .card button')].find((b) => b.style.borderColor.includes('accent') || b.style.border.includes('accent'))?.innerText.split('\n')[0] ?? '');
check(t.includes('People Ops') && on === 'Meera Kulkarni', `the chart is People Ops, centred on Meera Kulkarni (${on})`);
const strip = await page.evaluate(() => { const l = [...document.querySelectorAll('.main .card > .row')].pop(); return l?.innerText ?? ''; });
check(strip.startsWith('Meera Kulkarni') && /@/.test(strip), `her email is under the chart (${strip.replace(/\n/g, ' ')})`);
check(await page.evaluate(() => [...document.querySelectorAll('.main .card .seg button')].map((b) => b.textContent).join()) === 'Team view,Whole department', 'another department offers Team view and Whole department');
await shot('chart-from-person-desktop');
check(await clickText('.main button', '← Everyone'), 'a back link to Everyone');
await until(async () => (await rows()).length > 0, 'Everyone again');
check(await page.$eval('.main input[type=search]', (el) => el.value) === 'merea kulkarn', 'back on Everyone with the same search');
await page.goBack(); await wait(900);
check((await text()).includes('Whole department'), 'the browser back button steps back to the chart');
await page.goForward(); await wait(900);

// 3. Departments
await clickText('.tabs .tab', 'Departments');
await until(async () => (await text()).includes('Your department'), 'the Departments grid');
const cards = await page.$$eval('.main .grid button.card', (l) => l.map((b) => b.innerText.replace(/\n+/g, ' | ')));
check(cards.length >= 5, `departments listed (${cards.length})`);
check(/^F&B \| Your department \| \d+ people$/.test(cards[0] ?? ''), `own department first and highlighted (${cards[0]})`);
check(cards.every((c) => /\d+ (people|person)$/.test(c)), 'every department shows its headcount');
await shot('departments-desktop');
await clickText('.main .grid button.card', 'Front Office');
await until(async () => (await text()).includes('Whole department'), 'the Front Office chart');
t = await text();
check(t.includes('Front Office') && !t.includes('Your department'), 'Front Office chart, not marked as own');
await shot('department-chart-desktop');
await clickText('.main .card .seg button', 'Whole department'); await wait(500);
check((await page.$$eval('.main .card .tw b', (l) => l.length)) > 3, 'Whole department shows the tree');
await shot('department-tree-desktop');
check(await clickText('.main button', '← All departments'), 'a back link to the departments');
await until(async () => (await text()).includes('Your department'), 'the grid again');
await clickText('.main .grid button.card', 'F&B');
await until(async () => (await text()).includes('Around me'), 'own department chart');
t = await text();
check(t.includes('You and your peers') || t.includes('Your reports') || t.includes('You'), 'own department opens around me');
await shot('own-department-desktop');

// 4. Phone
await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
await page.goto(BASE + '/directory'); await wait(1200);
check(await page.$('.main table') === null, 'no table on a phone');
const pc = await page.$$eval('.main div[role=button]', (l) => l.length);
check(pc >= 40, `people as cards on a phone (${pc})`);
const wide = await page.evaluate(() => document.documentElement.scrollWidth);
check(wide <= 390, `no sideways scroll on a phone (${wide}px)`);
await shot('everyone-phone');
await search('meera');
await page.click('.main div[role=button]');
await until(async () => (await text()).includes('Whole department'), 'the chart on a phone');
await shot('chart-phone');
await page.goto(BASE + '/directory?tab=depts'); await wait(1000);
await shot('departments-phone');

check(!problems.length, `no page errors or failed calls${problems.length ? ': ' + [...new Set(problems)].join('; ') : ''}`);
await browser.close();
console.log(`\n${fails.length ? 'FAILED ' + fails.length : 'ALL PASSED'} · screenshots in ${SHOTS}`);
process.exitCode = fails.length ? 1 : 0;
