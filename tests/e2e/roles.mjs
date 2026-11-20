// Every role opens every page and tab; reports page errors, server errors and calls the role may not make.
import puppeteer from 'puppeteer-core';
const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const PW = 'Saltstayz#2026';
const post = (p, b) => fetch(BASE + '/api' + p, { method: 'POST', body: JSON.stringify(b) }).then((r) => r.json());
async function token(id) {
  let r = await post('/auth/login', { employeeId: id, password: id === 'EMP-1002' || id === 'EMP-1020' ? 'pass1234' : PW });
  if (r.token) return r.token;
  const o = await post('/auth/otp', { employeeId: id, purpose: 'first' });
  const v = await post('/auth/otp/verify', { employeeId: id, code: o.devOtp });
  r = await post('/auth/password', { employeeId: id, password: PW, verifyToken: v?.verifyToken });
  return r.token;
}
const people = [['EMP-1020', 'Employee'], ['EMP-1009', 'Line Manager'], ['EMP-1008', 'Property Manager'], ['EMP-1007', 'Cluster Manager'], ['EMP-1006', 'City Head'],
  ['EMP-1005', 'Operations Head'], ['EMP-1004', 'HRBP'], ['EMP-1003', 'HRBP Corporate'], ['EMP-1011', 'Payroll'], ['EMP-1012', 'Payroll Manager'], ['EMP-1013', 'Finance Head'],
  ['EMP-1010', 'Function Head'], ['EMP-1001', 'CHRO'], ['EMP-1002', 'HR Admin']];
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
const findings = [];
let where = '';
page.on('pageerror', (e) => findings.push(`${where}: PAGE ERROR ${e.message.split('\n')[0]}`));
page.on('response', async (r) => {
  const u = r.url();
  if (!u.includes('/api/')) return;
  const s = r.status();
  if (s >= 500 || s === 403 || s === 404) {
    let body = ''; try { body = (await r.text()).slice(0, 120); } catch {}
    findings.push(`${where}: ${s} ${r.request().method()} ${u.replace(BASE, '')} ${body}`);
  }
});
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
for (const [id, role] of people) {
  const t = await token(id);
  await page.goto(BASE + '/api/health');
  await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role);
  await page.goto(BASE + '/'); await wait(1200);
  const links = await page.evaluate(() => [...document.querySelectorAll('aside a.nav')].map((a) => a.getAttribute('href')));
  // My view has no Approvals: each request starts from its own page and is followed in Dashboard › My requests.
  // Every role view keeps Approvals.
  if (role === 'Employee') {
    where = `${role} (My view)`;
    if (links.includes('/approvals')) findings.push(`${where}: Approvals is in the menu`);
    await page.goto(BASE + '/approvals'); await wait(1200);
    const at = await page.evaluate(() => location.pathname);
    if (at !== '/') findings.push(`${where}: /approvals stayed on ${at} instead of going to the Dashboard`);
    if (!(await page.evaluate(() => !!document.getElementById('my-requests')))) findings.push(`${where}: no My requests on the Dashboard`);
  } else if (!links.includes('/approvals')) findings.push(`${role}: Approvals is missing from the menu`);
  for (const href of links) {
    where = `${role} ${href}`;
    await page.goto(BASE + href); await wait(900);
    // every tab on the page, and nested tabs once
    const tabs = await page.evaluate(() => [...document.querySelectorAll('.tabs .tab')].map((b) => b.textContent.trim()));
    for (const tb of tabs) {
      where = `${role} ${href} › ${tb}`;
      await page.evaluate((tb) => [...document.querySelectorAll('.tabs .tab')].find((b) => b.textContent.trim() === tb)?.click(), tb); await wait(700);
      const inner = await page.evaluate(() => { const l = document.querySelectorAll('.tabs'); return l.length > 1 ? [...l[1].querySelectorAll('.tab')].map((b) => b.textContent.trim()) : []; });
      for (const it of inner) {
        where = `${role} ${href} › ${tb} › ${it}`;
        await page.evaluate((it) => [...document.querySelectorAll('.tabs')[1]?.querySelectorAll('.tab') ?? []].find((b) => b.textContent.trim() === it)?.click(), it); await wait(600);
      }
    }
  }
  console.log('done', role, links.length, 'pages');
}
await browser.close();
console.log('\nFINDINGS', findings.length);
process.exitCode = findings.length ? 1 : 0;
for (const f of [...new Set(findings)]) console.log(f);
