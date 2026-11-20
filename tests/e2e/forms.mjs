// Fills in and submits the main forms as an employee and as HR Admin, with real uploads, and checks each outcome.
import puppeteer from 'puppeteer-core';
import fs from 'fs';
import os from 'os';
import path from 'path';
const BASE = process.env.HRMS_WEB || 'http://localhost:5173', DIR = new URL('./fixtures', import.meta.url).pathname;
const browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: 'new' });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
// Downloads are saved to a temporary folder, and each one's name and state are tracked.
const DLDIR = fs.mkdtempSync(path.join(os.tmpdir(), 'hrms-forms-')), downloads = {};
const cdp = await browser.target().createCDPSession();
await cdp.send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: DLDIR, eventsEnabled: true });
cdp.on('Browser.downloadWillBegin', (e) => { downloads[e.guid] = { name: e.suggestedFilename, state: 'inProgress' }; });
cdp.on('Browser.downloadProgress', (e) => { if (downloads[e.guid]) downloads[e.guid].state = e.state; });
const results = [];
let errs = [], api4 = [];
let promptValue = 'Deep test note';
page.on('dialog', (d) => d.accept(d.type() === 'prompt' ? promptValue : undefined));
page.on('pageerror', (e) => errs.push('PAGE ERROR ' + e.message.split('\n')[0]));
page.on('response', async (r) => {
  if (!r.url().includes('/api/')) return;
  const s = r.status(), what = `${s} ${r.request().method()} ${r.url().replace(BASE, '')}`;
  if (s >= 500) errs.push(what);
  else if (s >= 400) { let b = ''; try { b = (await r.text()).slice(0, 160); } catch {} api4.push(`${what} ${b}`); }
});
// In every page: the toasts shown (so a step reads what its own actions said), the topmost dialog, and a value setter React notices.
await page.evaluateOnNewDocument(() => {
  window.__toasts = [];
  let cur = '';
  new MutationObserver(() => { const t = document.getElementById('toast')?.textContent || ''; if (t !== cur) { cur = t; if (t) window.__toasts.push(t); } })
    .observe(document, { childList: true, subtree: true, characterData: true });
  // Work inside the topmost dialog (.ov > .dlg, drawers .ov.right > .dlg) when one is open.
  window.__sc = () => [...document.querySelectorAll('.ov .dlg')].pop() || document;
  // The control of a .field whose caption is `label` (exact, else starting with it).
  window.__field = (label) => {
    const fs = [...__sc().querySelectorAll('label.field, div.field')];
    const f = fs.find((l) => l.querySelector('span')?.textContent.trim() === label) || fs.find((l) => l.querySelector('span')?.textContent.trim().startsWith(label));
    return f?.querySelector('input:not([type=file]):not([type=checkbox]), select, textarea') || null;
  };
  // Types into a people picker (components/PersonPicker, an input with role=combobox) and clicks the option whose
  // employee code is `id`, else whose code is the text, else the first whose text holds every word of the text.
  window.__pick = async (el, text, id) => {
    if (!el) return false;
    __set(el, text);
    const want = (id || text).trim(), words = text.toLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean);
    for (let i = 0; i < 60; i++) {
      await new Promise((r) => setTimeout(r, 150));
      const os = [...(document.getElementById(el.getAttribute('aria-controls'))?.querySelectorAll('[role=option]') ?? [])].filter((o) => o.dataset.id);
      const o = os.find((o) => o.dataset.id === want) || (!id && os.find((o) => words.every((w) => o.textContent.toLowerCase().includes(w))));
      if (o) { o.click(); return true; }
    }
    return false;
  };
  window.__set = (el, v) => {
    if (el.tagName === 'SELECT') {
      const o = [...el.options].find((o) => o.value === v || o.textContent.trim() === v) || [...el.options].find((o) => o.textContent.trim().startsWith(v));
      if (!o) return false;
      Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value').set.call(el, o.value);
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    }
    const proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, v);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
    return true;
  };
});
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const settle = async () => { await page.waitForNetworkIdle({ idleTime: 350, timeout: 15000 }).catch(() => {}); };
/** Waits until fn (run in the page) returns something truthy, and returns it; '' after the timeout. */
const until = async (fn, args = [], ms = 8000) => { const t0 = Date.now(); for (;;) { const v = await page.evaluate(fn, ...args).catch(() => ''); if (v || Date.now() - t0 > ms) return v || ''; await wait(150); } };
const loaded = () => until(() => !document.querySelector('.loading'), [], 15000);
const login = async (id) => (await (await fetch(BASE + '/api/auth/login', { method: 'POST', body: JSON.stringify({ employeeId: id, password: id === 'EMP-1002' || id === 'EMP-1020' ? 'pass1234' : 'Saltstayz#2026' }) })).json()).token;
const go = async (path) => { await page.goto(BASE + path); await settle(); await loaded(); };
const as = async (id, role, path) => { const t = await login(id); await page.goto(BASE + '/api/health'); await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role); await go(path); };
/** Clicks a button in the topmost dialog (else the page) by its text: exact, or starting with it, or matching a RegExp. */
const click = async (text, exact = false) => {
  const re = text instanceof RegExp ? [text.source, text.flags] : null;
  const r = await page.evaluate((text, exact, re) => {
    const m = re ? ((t) => new RegExp(re[0], re[1]).test(t)) : (t) => t === text || (!exact && t.startsWith(text));
    const all = [...__sc().querySelectorAll('button')], bs = all.filter((b) => !b.disabled);
    const b = (!re && bs.find((x) => x.textContent.trim() === text)) || bs.find((x) => m(x.textContent.trim()));
    if (b) { b.click(); return 'ok'; }
    const off = all.find((x) => m(x.textContent.trim()));
    return off ? `disabled${off.title ? ': ' + off.title : ''}` : 'missing';
  }, re ? '' : text, exact, re);
  if (r !== 'ok') throw new Error(`no button: ${text} (${r})`);
  await wait(200); await settle();
};
/** Clicks a button in the table row that contains every one of `parts`. */
const clickRow = async (parts, text, exact = true) => {
  const ok = await page.evaluate((parts, text, exact) => {
    const tr = [...__sc().querySelectorAll('tr')].find((x) => parts.every((p) => x.textContent.includes(p)));
    const b = tr && [...tr.querySelectorAll('button')].find((x) => !x.disabled && (exact ? x.textContent.trim() === text : x.textContent.trim().startsWith(text)));
    if (b) b.click(); return !!b;
  }, parts, text, exact);
  if (!ok) throw new Error(`no button "${text}" in the row with ${parts.join(', ')}`);
  await wait(200); await settle();
};
const fill = async (label, value) => {
  const ok = await page.evaluate(async (label, value) => { const el = __field(label); return !el ? 'no field' : el.getAttribute('role') === 'combobox' ? ((await __pick(el, value)) ? 'ok' : 'no option') : __set(el, value) ? 'ok' : 'no option'; }, label, value);
  if (ok !== 'ok') throw new Error(`${ok}: ${label}${ok === 'no option' ? ' = ' + value : ''}`);
  await wait(150);
};
/** Focuses an element handle and tabs away, so an onBlur save runs. */
const blur = async (h) => { await h.focus(); await page.keyboard.press('Tab'); await wait(200); await settle(); };
const fieldHandle = async (label) => { const h = (await page.evaluateHandle((label) => __field(label), label)).asElement(); if (!h) throw new Error('no field: ' + label); return h; };
/** Fills a field that saves when it loses focus. */
const fillBlur = async (label, value) => { await fill(label, value); await blur(await fieldHandle(label)); };
/** The nth input of the table row whose first cell starts with `first`. */
const rowInput = async (first, nth = 0, root = '') => {
  const h = (await page.evaluateHandle((first, nth, root) => {
    const tr = [...(root ? document.querySelector(root) : __sc()).querySelectorAll('tr')].find((x) => x.cells[0]?.textContent.trim().startsWith(first));
    return tr ? [...tr.querySelectorAll('input:not([type=checkbox])')][nth] : null;
  }, first, nth, root)).asElement();
  if (!h) throw new Error(`no input in the row ${first}`);
  return h;
};
const setValue = async (h, v) => { await h.evaluate((el, v) => __set(el, v), v); await wait(150); };
/** Type and pick: types `q` into a people picker and picks the person with code `id`. The picker is the field
 *  captioned `label`, or the dialog's (else the page's) employee search. */
const pickPerson = async (q, id, label = '') => {
  const r = await page.evaluate(async (q, id, label) => {
    const el = label ? __field(label) : [...__sc().querySelectorAll('input[role=combobox]')].find((x) => (x.placeholder || '').startsWith('Search employee')) || __sc().querySelector('input[role=combobox]');
    if (!el || el.getAttribute('role') !== 'combobox') return 'no employee picker';
    return (await __pick(el, q, id)) ? 'ok' : `no ${id} in the picker for "${q}"`;
  }, q, id, label);
  if (r !== 'ok') throw new Error(r);
  await wait(300); await settle();
};
const pickEmp = (q, id) => pickPerson(q, id);
const upload = async (file, nth = 0) => {
  const h = (await page.evaluateHandle((n) => [...__sc().querySelectorAll('input[type=file]')][n] || null, nth)).asElement();
  if (!h) throw new Error('no file input');
  await h.uploadFile(DIR + '/' + file);
  await wait(300); await settle();
};
/** The last toast shown since the step began (waits for one). */
const toast = async (ms = 8000) => { await until(() => window.__toasts.length > 0, [], ms); await wait(300); return page.evaluate(() => window.__toasts[window.__toasts.length - 1] || ''); };
const freshToasts = () => page.evaluate(() => { window.__toasts = []; });
/** Waits for the visible toast to go, so the next one is seen even when it says the same thing. */
const toastGone = async () => { await until(() => !document.getElementById('toast'), [], 5000); await freshToasts(); };
const noteBad = () => page.evaluate(() => [...__sc().querySelectorAll('.note.bad')].map((n) => n.textContent).join(' | '));
const tab = async (t) => {
  const ok = await page.evaluate((t) => { const b = [...document.querySelectorAll('.tabs .tab')].find((b) => b.textContent.trim() === t) || [...document.querySelectorAll('.tabs .tab')].find((b) => b.textContent.trim().startsWith(t)); b?.click(); return !!b; }, t);
  if (!ok) throw new Error('no tab: ' + t);
  await wait(150); await settle(); await loaded();
};
/** Segment buttons (.seg), used for sub-tabs and filters. */
const seg = async (t) => {
  const ok = await page.evaluate((t) => { const b = [...document.querySelectorAll('.seg button')].find((b) => b.textContent.trim().startsWith(t)); b?.click(); return !!b; }, t);
  if (!ok) throw new Error('no segment: ' + t);
  await wait(150); await settle(); await loaded();
};
/** Waits for a table row (in the topmost dialog, else the page) that contains every one of `parts`; returns its text. */
const row = (parts, ms = 8000) => until((parts) => [...__sc().querySelectorAll('tr')].find((x) => parts.every((p) => x.textContent.includes(p)))?.textContent.replace(/\s+/g, ' ').trim() || '', [parts], ms);
const needRow = async (parts, ms) => { const r = await row(parts, ms); if (!r) throw new Error('not listed: ' + parts.join(' + ')); return r; };
const dlgText = () => until(() => document.querySelector('.ov .dlg')?.innerText || '');
/** Runs `act`, which downloads through the API (`part` of the URL), and waits for the browser to save the file. */
const download = async (part, act) => {
  const before = new Set(Object.keys(downloads));
  const [res] = await Promise.all([page.waitForResponse((r) => r.url().includes(part), { timeout: 20000 }), act()]);
  if (res.status() !== 200) throw new Error(`download ${part} gave ${res.status()}`);
  for (let i = 0; i < 100; i++) {
    const d = Object.entries(downloads).find(([g, x]) => !before.has(g) && x.state !== 'inProgress');
    if (d) {
      const f = path.join(DLDIR, d[1].name), size = fs.existsSync(f) ? fs.statSync(f).size : 0;
      if (d[1].state !== 'completed' || !size) throw new Error(`download ${d[1].name} ${d[1].state}, ${size} bytes`);
      return `downloaded ${d[1].name} (${size} bytes)`;
    }
    await wait(100);
  }
  throw new Error(`download ${part}: no file saved`);
};
async function step(name, fn, expect) {
  errs = []; api4 = [];
  await freshToasts().catch(() => {});
  let detail = '', ok = true;
  try {
    const r = await fn();
    detail = r ?? (await toast());
    if (expect && !new RegExp(expect, 'i').test(detail)) { ok = false; detail = `expected /${expect}/, got "${detail}" ${await noteBad()}`; }
  } catch (e) { ok = false; detail = e.message + ' ' + (await noteBad().catch(() => '')); }
  if (errs.length) { ok = false; detail += ' ' + errs.join('; '); }
  if (!ok && api4.length) detail += ' · API: ' + api4.join('; ');
  results.push({ name, ok, detail: String(detail).slice(0, 400) });
  console.log(ok ? 'PASS' : 'FAIL', name, '·', String(detail).slice(0, 300));
  await page.keyboard.press('Escape'); await wait(250);
  for (let i = 0; i < 30 && (await page.evaluate(() => !!document.getElementById('toast')).catch(() => false)); i++) await wait(150);
}
// Local dates (the app works in local time).
const ymd = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const monthsAgo = (n) => { const d = new Date(); d.setDate(1); d.setMonth(d.getMonth() - n); return ymd(d).slice(0, 7); };
const todayS = ymd(new Date()), lastYm = monthsAgo(1), prevYm = monthsAgo(2);

// ---------------- Employee ----------------
await as('EMP-1020', 'Employee', '/me');
await step('Employee: submit a claim with the bill', async () => {
  await tab('Reimbursements'); await fill('Type', 'Travel'); await fill('Amount', '1250'); await fill('Note', 'Cab to client site'); await upload('bill.pdf'); await click('Submit claim');
  const t = await toast(); await needRow(['Travel', '₹1,250', 'Pending']); return t + ' · listed as pending'; }, '^Sent to Payroll · listed');
// The new UI (and the prototype) has no "Save as draft": the declaration is kept on the screen until it is submitted.
await step('Employee: tax declaration filled in, old regime (no draft save in the new UI)', async () => {
  await tab('Tax declaration');
  await page.evaluate(() => [...document.querySelectorAll('label.chkl')].find((l) => l.textContent.trim() === 'Old tax regime')?.querySelector('input')?.click()); await wait(300);
  await setValue(await rowInput('Public Provident Fund'), '50000'); await setValue(await rowInput('Health insurance: self'), '18000');
  return await until(() => { const t = document.body.innerText; return t.includes('You have changes that are not submitted.') && t.includes('You have chosen the old regime') ? 'old regime chosen, deductions entered, changes not submitted' : ''; });
}, 'not submitted');
await step('Employee: tax declaration submitted', async () => {
  await tab('Tax declaration');
  await page.evaluate(() => { const i = [...document.querySelectorAll('label.chkl')].find((l) => l.textContent.trim() === 'Old tax regime')?.querySelector('input'); if (i && !i.checked) i.click(); }); await wait(300);
  await setValue(await rowInput('Public Provident Fund'), '60000'); await click('Submit declaration'); const t = await toast();
  // Saved on the server: reload the page and read the declaration back.
  await go('/me?tab=tax');
  const back = await until(() => { const v = (n) => [...document.querySelectorAll('tr')].find((x) => x.cells[0]?.textContent.trim().startsWith(n))?.querySelector('input')?.value;
    const st = [...document.querySelectorAll('.card .pill')][0]?.textContent; return v('Public Provident Fund') === '60000' && v('Health insurance: self') === '18000' ? `${st}, PPF 60000, health 18000 kept` : ''; });
  return `${t} · ${back || 'not saved'}`; }, '^Declaration submitted · Declared, PPF 60000, health 18000 kept');
await step('Employee: apply for leave (window rules shown if refused)', async () => {
  await tab('My leave'); await click('Apply for leave'); const d = new Date(Date.now() + 20 * 864e5); while ([0, 6].includes(d.getDay())) d.setDate(d.getDate() + 1); const iso = ymd(d);
  await fill('Leave type', 'Privilege leave'); await fill('From', iso); await fill('To', iso); await fill('Reason', 'Family function'); await settle(); await click('Send for approval');
  const t = await toast(4000); if (!t) return 'refused: ' + (await noteBad());
  await needRow(['Privilege leave', 'Pending']); return t + ' · listed'; }, 'sent for approval · listed|refused: \\S');
await step('Employee: personal details change', async () => { await tab('My details'); await fill('What do you want to change?', 'Personal'); await fill('Mobile', '9876512345'); await click('Send to Payroll');
  const t = await toast(); const l = await until(() => [...document.querySelectorAll('ul.log li')].find((x) => x.textContent.startsWith('Personal') && x.textContent.includes('Pending'))?.textContent || '');
  return `${t} · ${l ? 'request listed' : 'not listed'}`; }, '^Sent to Payroll · request listed');
// Resignation and the exit card are on My view › Separation (/my-exit), not on My pay and profile.
await step('Employee: start resignation', async () => { await go('/my-exit'); await click('Start resignation'); await fill('Reason for leaving', 'Relocation'); await click('Submit resignation'); }, '^Submitted. Last working day');
await step('Employee: exit card opens the case', async () => { await go('/my-exit');
  return await until(() => { const c = [...document.querySelectorAll('.card')].find((x) => x.querySelector('h3')?.textContent.trim() === 'My exit'); return c ? c.innerText.replace(/\s+/g, ' ') : ''; }); }, 'My exit.*Resignation · last working day');
// Approvals is not in My view (9 Nov): each request starts from its own page and is followed in Dashboard › My requests.
await step('Employee: no Approvals in My view (the address opens the Dashboard)', async () => {
  const menu = await page.evaluate(() => [...document.querySelectorAll('aside a.nav')].map((a) => a.getAttribute('href')));
  await go('/approvals'); const at = await page.evaluate(() => location.pathname);
  return `${menu.includes('/approvals') ? 'Approvals in the menu' : 'not in the menu'} · /approvals opens ${at === '/' ? 'the Dashboard' : at}`; }, '^not in the menu · /approvals opens the Dashboard$');
await step('Employee: regularize a day from My attendance (window rules shown if refused)', async () => {
  await go('/me?tab=att');
  if (!(await until(() => !!document.querySelector('table tbody tr')))) throw new Error('no attendance register');
  const n = await page.evaluate(() => { const bs = [...document.querySelectorAll('tbody button')].filter((b) => b.textContent.trim() === 'Regularize'); bs[bs.length - 1]?.click(); return bs.length; });
  if (!n || !(await until(() => document.querySelector('.ov .dlg h2')?.textContent === 'New attendance regularization request'))) return 'no day to regularize';
  await fill('Reason', 'Forgot to punch'); await settle(); await click('Send for approval');
  const t = await toast(4000); if (!t) return 'refused: ' + (await noteBad()); return t; }, 'sent for approval|refused: \\S|no day to regularize');
await step('Employee: My requests on the Dashboard, with who each waits on', async () => { await go('/');
  const r = await until(() => { const c = document.getElementById('my-requests'); if (!c) return '';
    const rows = [...c.querySelectorAll('.mreq')].map((x) => x.innerText.replace(/\s+/g, ' '));
    const cl = rows.find((x) => x.startsWith('Reimbursement claim')), ch = rows.find((x) => x.startsWith('Bank or profile change')), rs = rows.find((x) => x.startsWith('Separation'));
    return cl && ch && rs ? `${rows.length} requests · claim ${/Waiting on \S/.test(cl) ? 'waiting on someone' : 'no approver shown'} · details change ${/Cancel/.test(ch) ? 'can be cancelled' : 'no Cancel'} · resignation listed` : ''; });
  return r || 'not listed'; }, '^\\d+ requests · claim waiting on someone · details change can be cancelled · resignation listed');
await step('Employee: open a request from My requests (detail and history, read-only)', async () => { await go('/');
  const ok = await until(() => { const r = [...document.querySelectorAll('#my-requests .mreq')].find((x) => x.innerText.startsWith('Reimbursement claim')); const b = r && [...r.querySelectorAll('button')].find((x) => x.textContent === 'Open'); b?.click(); return !!b; });
  if (!ok) throw new Error('no claim in My requests');
  const d = await until(() => { const t = document.querySelector('.ov .dlg')?.innerText || ''; return t.includes('Approval progress') && !document.querySelector('.ov .dlg .loading') ? t : ''; });
  const approve = await page.evaluate(() => [...document.querySelectorAll('.ov .dlg button')].some((b) => /^(Approve|Reject)/.test(b.textContent.trim())));
  return d ? `detail shown with approval progress · ${approve ? 'Approve shown' : 'no Approve or Reject'}` : 'no detail'; }, '^detail shown with approval progress · no Approve or Reject$');
await step('Employee: cancel a request from My requests', async () => { await go('/');
  const ok = await until(() => { const r = [...document.querySelectorAll('#my-requests .mreq')].find((x) => x.innerText.startsWith('Bank or profile change') && /Pending/.test(x.innerText)); const b = r && [...r.querySelectorAll('button')].find((x) => x.textContent === 'Cancel'); b?.click(); return !!b; });
  if (!ok) throw new Error('no pending details change with Cancel');
  const t = await toast(); await seg('Decided');
  const r = await until(() => [...document.querySelectorAll('#my-requests .mreq')].some((x) => x.innerText.startsWith('Bank or profile change') && /Cancelled/.test(x.innerText)));
  return `${t} · ${r ? 'shown Cancelled' : 'not shown cancelled'}`; }, '^Request cancelled · shown Cancelled$');

// ---------------- HR Admin: approvals ----------------
await as('EMP-1002', 'HR Admin', '/approvals');
for (let i = 0; i < 4; i++) {
  await step(`HR Admin: approve inbox item ${i + 1} with a comment`, async () => {
    await go('/approvals');
    const who = await page.evaluate(() => { const b = document.querySelector('button.irow'); b?.click(); return b ? b.innerText.replace(/\s+/g, ' ') : ''; });
    if (!who) return 'inbox empty';
    const id = await until(() => !document.querySelector('.inbox .loading') && document.querySelector('.inbox .card .ink2')?.textContent.split(' · ')[0]);
    const c = await until(() => { const i = [...document.querySelectorAll('input')].find((x) => (x.placeholder || '').startsWith('Comment')); return i ? __set(i, 'Checked in the UI test') : false; });
    if (!c) throw new Error(`${id}: no comment box (cannot act)`);
    await click(/^Approve( \(admin override\))?$/); return `${await toast()} · ${id} · ${who}`;
  }, 'approved|inbox empty');
}

// The approval flow is a list of rules per request type and flow: the first rule that fits decides. HR tests a request,
// adds a rule, names its approver, moves it to the top, switches it off and deletes it; each change saves at once.
const ariaClick = async (label) => { const ok = await page.evaluate((l) => { const b = document.querySelector(`button[aria-label="${l}"]`); if (b && !b.disabled) b.click(); return !!b && !b.disabled; }, label); if (!ok) throw new Error('no button ' + label); await wait(200); await settle(); };
const flowTest = async (grade, days) => { await fill('Grade', grade); await fill('Leave days in the last 30 days', days); await click('Test', true);
  await wait(300); return until(() => document.getElementById('flow-test-result')?.innerText.replace(/\s+/g, ' ').trim() || ''); };
const saved = async () => { const t = await toast(); await toastGone(); return t; };
await step('Approval flow: rules, Test a request, add, move, switch off and delete a rule', async () => {
  await go('/approvals?tab=rules'); await tab('Leave'); await seg('Privilege and other leave');
  const names = () => until(() => [...document.querySelectorAll('#flow-rules .flow-rule')].map((x) => x.innerText.split('\n')[0].trim()).join(' | '));
  const before = await names();
  if (!/^1\. Grade G3–G4 \| 2\. Grade G5–G6 \|.* \| Otherwise$/.test(before)) throw new Error('rules shown: ' + before);
  const t1 = await flowTest('G3', '7');
  if (!/^Rule 1 decides: Grade G3–G4 Line Manager → Property Manager → HRBP → Cluster Manager Also told: HRBP Corporate, Function Head, Operations Head$/.test(t1)) throw new Error('test G3: ' + t1);
  await toastGone();
  await page.evaluate(() => __set(document.querySelector('select[aria-label="Add rule: what it checks"]'), 'Grade is'));
  await click('Add rule', true); const added = await saved();
  await page.evaluate(() => __set(document.querySelectorAll('select[aria-label="Role to add"]')[0], 'CHRO')); await click('Add', true); const named = await saved();
  while (await page.evaluate(() => !document.querySelector('button[aria-label="Move “Grade G1” up"]')?.disabled)) { await ariaClick('Move “Grade G1” up'); await saved(); }
  const t2 = await flowTest('G1', '7');
  if (t2 !== 'Rule 1 decides: Grade G1 CHRO') throw new Error('after moving to the top: ' + t2);
  await ariaClick('Use “Grade G1”'); await saved();
  const t3 = await flowTest('G1', '7');
  if (!/^Rule 5 decides: Up to 10 days in 30 days/.test(t3)) throw new Error('switched off: ' + t3);
  await ariaClick('Delete “Grade G1”'); const del = await saved();
  await go('/approvals?tab=rules'); await tab('Leave'); await seg('Privilege and other leave');
  const after = await names();
  if (after !== before) throw new Error(`after delete: ${after}`);
  await tab('Check a flow');
  const glance = await until(() => document.body.innerText.includes('Each flow is a list of rules') && document.body.innerText.includes('All flows at a glance'));
  return `${added} · ${named} · rule 1 decides after moving · skipped when off · ${del} · list as before${glance ? ' · glance shown' : ''}`;
}, '^Rule added, switched off. It switches on when you pick its first approver · Saved. The rule is now switched on · rule 1 decides after moving · skipped when off · Rule deleted · list as before · glance shown$');

// ---------------- HR Admin: payroll ----------------
await go('/payroll');
// Payslip design has no Save button any more: each field saves when it loses focus, and the logo as soon as it is uploaded.
await step('Payslip setup: codes, footer and logo', async () => { await tab('Payslip design'); const said = [];
  for (const [l, v] of [['Company PAN', 'AAACS1234B'], ['TAN', 'DELS12345A'], ['Printed at the bottom of every payslip', 'Generated by Saltstayz HRMS']]) { await toastGone(); await fillBlur(l, v); said.push(await toast()); }
  await toastGone(); await upload('logo.png'); said.push(await toast());
  if (!(await until(() => !!document.querySelector('img[alt=Logo]')))) throw new Error('logo not shown after upload');
  const t = said.every((x) => x === 'Saved') ? 'Saved' : said.join(' | ');
  await go('/payroll'); await tab('Payslip design');
  const kept = await until(() => { const f = __field; return f('Company PAN')?.value === 'AAACS1234B' && f('TAN')?.value === 'DELS12345A' && f('Printed at the bottom')?.value === 'Generated by Saltstayz HRMS' && !!document.querySelector('img[alt=Logo]'); });
  return `${t} · ${kept ? 'PAN, TAN, footer and logo kept' : 'not kept after reload'}`; }, '^Saved · PAN, TAN, footer and logo kept');
// Month close is gone: the checklist at the top of Payroll run has five closures, and the dry run waits for all of them.
const closureRow = (name) => page.evaluate((n) => { const b = [...document.querySelectorAll('button[aria-expanded]')].find((x) => x.textContent.includes(n)); b?.click(); return !!b; }, name);
await step('Payroll run checklist: the dry run waits for it', async () => { await go('/payroll'); await tab('Payroll run');
  const dry = await page.evaluate(() => { const b = [...document.querySelectorAll('button')].find((x) => x.textContent.trim() === 'Run dry run'); return b ? `${b.disabled ? 'disabled' : 'enabled'}: ${b.title}` : 'no button'; });
  const t = await until(() => document.body.innerText.includes('Before you run payroll') && document.body.innerText.includes('of 5 closed') ? 'checklist shown' : '');
  return `${t} · Run dry run ${dry}`; }, '^checklist shown · Run dry run disabled: Close the checklist first');
// The checklist has no shortcut: each pending item clears only when the person it waits on acts. The setup below does
// through the API what employees and approvers would do in the app (about 100 days in a fresh seed), then the
// checklist is used on screen: Not raised and Waiting listed, a message sent, each closure closed once it is clear.
const apiAs = async (tok, role, method, path, body) => {
  const r = await fetch(BASE + '/api' + path, { method, headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + tok, 'X-Role': role }, body: body ? JSON.stringify(body) : undefined });
  const x = await r.json().catch(() => ({})); if (!r.ok) throw new Error(`${method} ${path} ${r.status}: ${x.error || ''}`); return x;
};
const tokens = {};
/** A token for anyone: the password when the account is active, else activated with the development OTP. */
const tokenFor = async (id) => {
  if (tokens[id]) return tokens[id];
  let t = await login(id).catch(() => '');
  if (!t) {
    const post = async (p, b) => (await fetch(BASE + '/api' + p, { method: 'POST', body: JSON.stringify(b) })).json();
    const o = await post('/auth/otp', { employeeId: id, purpose: 'first' }); const v = await post('/auth/otp/verify', { employeeId: id, code: o.devOtp });
    t = (await post('/auth/password', { employeeId: id, password: 'Saltstayz#2026', verifyToken: v?.verifyToken })).token;
  }
  if (!t) throw new Error('cannot sign in as ' + id);
  return (tokens[id] = t);
};
const STAGE_ROLE = { 'Functional Manager': 'Line Manager', 'Receiving Manager': 'Line Manager', 'Receiving Property Manager': 'Property Manager', 'Receiving Cluster Manager': 'Cluster Manager', 'Receiving Function Head': 'Function Head' };
let payYm = '', keptFor = '';
const checklist = async () => apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', `/payroll/${payYm}/checklist`);
/** Each employee raises a regularization for every day of theirs listed as not raised (HR opens an exception for the
 *  cycle, as the regularization window has passed). */
const raiseDays = async (keep) => {
  const x = await checklist(), att = x.closures.find((c) => c.key === 'attendance'), by = {};
  for (const it of att.items) if (it.group === 'Not raised' && it.date && it.employeeId !== keep) (by[it.employeeId] ||= []).push(it);
  const A = await tokenFor('EMP-1002');
  let n = 0;
  for (const [emp, items] of Object.entries(by)) {
    await apiAs(A, 'HR Admin', 'POST', '/exceptions', { employeeId: emp, kind: 'regularize', from: x.from, to: x.to, until: todayS, reason: 'Payroll cut-off: days to regularize' });
    const t = await tokenFor(emp);
    for (const it of items) {
      await apiAs(t, 'Employee', 'POST', '/approvals', { wf: 'regularize', payload: { date: it.date, rtype: it.what.startsWith('Missing punch') ? 'All Other' : 'No Punch', in: '09:00', out: '18:00', reason: 'Forgot to punch' } });
      n++;
    }
  }
  return n;
};
const rolesOf = {};
const approveAs = async (id, role, req) => { try { await apiAs(await tokenFor(id), role, 'POST', `/approvals/${req}/approve`, { note: 'Checked before the payroll run' }); return true; } catch { return false; } };
/** Every waiting request in the checklist is decided by the person it waits on, stage by stage, in the stage's role
 *  view when they hold it, else another view they hold (a manager who left falls back to the HRBP, for example). When
 *  nobody in place can act (no one mapped, or the approver is the person it is about), HR Admin's override decides,
 *  as the app allows. */
const decideAll = async () => {
  let n = 0, over = 0;
  for (let round = 0; round < 8; round++) {
    const x = await checklist(), waiting = x.closures.flatMap((c) => c.items.filter((it) => it.requestId && !it.late));
    if (!waiting.length) return `${n} decisions${over ? ` (${over} by HR Admin override)` : ''}`;
    for (const it of waiting) {
      let ok = false;
      if (it.waitsOnId) {
        const want = STAGE_ROLE[it.waitsAs] || it.waitsAs;
        rolesOf[it.waitsOnId] ||= (await apiAs(await tokenFor(it.waitsOnId), 'Employee', 'GET', '/me')).ctx.roles;
        for (const r of [want, ...rolesOf[it.waitsOnId].filter((r) => r !== want)].filter((r) => r !== 'Employee' && rolesOf[it.waitsOnId].includes(r))) {
          if ((ok = await approveAs(it.waitsOnId, r, it.requestId))) break;
        }
      }
      if (!ok && (ok = await approveAs('EMP-1002', 'HR Admin', it.requestId))) over++;
      if (!ok) throw new Error(`${it.requestId} (${it.what}) cannot be decided by ${it.waitsOn}`);
      n++;
    }
  }
  throw new Error('requests still waiting after 8 rounds');
};
await step('Payroll run checklist: employees raise regularizations for their missed days (API setup)', async () => {
  payYm = (await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', '/payroll')).openYm;
  const x = await checklist(), first = x.closures[0].items.find((it) => it.group === 'Not raised');
  keptFor = first?.employeeId || '';
  return `${await raiseDays(keptFor)} raised, ${keptFor} left not raised`; }, '^[1-9]\\d* raised, EMP-\\d+ left not raised');
await step('Payroll run checklist: a closure shows Not raised and Waiting, and a message goes out', async () => { await go('/payroll'); await tab('Payroll run');
  if (!(await closureRow('Attendance closure'))) throw new Error('no Attendance closure row');
  const groups = await until(() => /Not raised \(\d+\)/.test(document.body.innerText) && /Waiting for approval \(\d+\)/.test(document.body.innerText) ? 'Not raised and Waiting listed' : '');
  await click('Send message', true); const box = await until(() => [...document.querySelectorAll('.ov .dlg textarea')].map((x) => x.value).join(' ')); await click('Send message', true); const t = await toast();
  const m = await until(() => [...document.querySelectorAll('td')].some((x) => x.textContent.includes(' · Messaged ')) ? 'shown as messaged' : '');
  const close = await page.evaluate(() => { const b = [...document.querySelectorAll('button')].find((x) => x.textContent.trim() === 'Close'); return b ? (b.disabled ? 'Close disabled' : 'Close enabled') : 'no Close'; });
  await closureRow('Attendance closure');
  return `${groups || 'groups not shown'} · ${t} · ${box.includes('Payroll for') ? 'default text' : 'no default text'} · ${m || 'not shown'} · ${close}`;
}, '^Not raised and Waiting listed · Message sent · default text · shown as messaged · Close disabled$');
await step('Payroll run checklist: the rest raised, every request decided by its approver (API setup)', async () => {
  const raised = await raiseDays(''); const decided = await decideAll();
  const x = await checklist(), left = x.closures.reduce((a, c) => a + c.pending, 0);
  return `${raised} more raised · ${decided} · ${left} pending`; }, '^[1-9]\\d* more raised · [1-9]\\d* decisions.* · 0 pending$');
// Items that never clear without the employee: a joiner imported with no bank account, who missed every day of the
// month and punched in once without punching out. Payroll marks the missing punch as half a day of loss of pay, the
// absences as full days, and holds the salary for the month; then every closure can close.
let joiner = '';
await step('Payroll run checklist: a joiner with no bank account and a missing punch (API setup)', async () => {
  const A = await tokenFor('EMP-1002');
  const k = (await apiAs(A, 'HR Admin', 'GET', '/employees?q=EMP-1020&size=5')).rows[0];
  const row = { name: 'Checklist Joiner', type: 'Permanent', doj: payYm + '-01', location: k.location, department: k.department, designation: k.designation, grade: k.grade, shift: k.shift_code,
    gross: 18000, mobile: '9876509911', email: 'checklist.joiner@example.com', dob: '1996-04-04', gender: 'Male', pan: 'ABCDE9911F', aadhaar: '556677889911' };
  const imp = await apiAs(A, 'HR Admin', 'POST', '/employees/import', { rows: [row], apply: true });
  joiner = imp.results?.[0]?.code || '';
  if (!joiner) throw new Error('import: ' + JSON.stringify(imp.results));
  const days = (await checklist()).closures.find((c) => c.key === 'attendance').items.filter((it) => it.employeeId === joiner && it.group === 'Not raised').map((it) => it.date).sort();
  const fd = new FormData();
  fd.append('file', new Blob([`Employee ID,Date,In time,Out time\n${joiner},${days[0]},09:00,\n`], { type: 'text/csv' }), 'att.csv');
  const up = await fetch(BASE + '/api/bulk/att/apply', { method: 'POST', headers: { Authorization: 'Bearer ' + A, 'X-Role': 'HR Admin' }, body: fd });
  if (!up.ok) throw new Error('attendance upload ' + up.status);
  const x = await checklist(), mine = (k) => x.closures.find((c) => c.key === k).items.filter((it) => it.employeeId === joiner && it.group === 'Not raised');
  const att = mine('attendance'), punch = att.filter((it) => it.lopHalf).length;
  return `${joiner} · ${att.length - punch} absences, ${punch} missing punch · ${mine('bank').filter((it) => it.canHold).length} with no bank account`;
}, '^EMP-\\d+ · [1-9]\\d* absences, 1 missing punch · 1 with no bank account$');
await step('Payroll run checklist: Mark as LOP, a missing punch as half a day and the absences as full days', async () => { await go('/payroll'); await tab('Payroll run');
  if (!(await closureRow('Attendance closure'))) throw new Error('no Attendance closure row');
  await until(() => /Not raised \(\d+\)/.test(document.body.innerText));
  await toastGone(); await clickRow([joiner, 'Missing punch'], 'Mark as LOP');
  const choice = await until(() => { const d = document.querySelector('.ov .dlg'); return d && [...d.querySelectorAll('button')].some((b) => b.textContent.trim() === 'Half day') && [...d.querySelectorAll('button')].some((b) => b.textContent.trim() === 'Full day') ? 'Full day or Half day' : ''; });
  await click('Half day', true); await fill('Reason', 'Punched in only, no reply by the cut-off'); await click('Mark as loss of pay', true); const one = await toast();
  await toastGone(); await click('Mark all absences as loss of pay'); await fill('Reason', 'Absent, nothing raised by the cut-off'); await click('Mark as loss of pay', true); const all = await toast();
  const x = await checklist(), cl = x.closures.find((c) => c.key === 'attendance');
  const A = await tokenFor('EMP-1002'), m = await apiAs(A, 'HR Admin', 'GET', `/attendance/month/${joiner}?ym=${payYm}`);
  const half = m.month.days.filter((d) => d.code === 'LOP' && d.lop === 0.5).length;
  await closureRow('Attendance closure');
  return `${choice || 'no choice'} · ${one} · ${all} · ${cl.pending} pending · ${half} half day on the calendar`;
}, '^Full day or Half day · 1 day marked as loss of pay \\(1 as a half day\\) · [1-9]\\d* days? marked as loss of pay · 0 pending · 1 half day on the calendar$');
await step('Payroll run checklist: Hold salary for no bank account, shown On hold', async () => { await go('/payroll'); await tab('Payroll run');
  if (!(await closureRow('Bank verification closure'))) throw new Error('no Bank verification closure row');
  await until(() => document.body.innerText.includes('No bank account on record'));
  const all = await page.evaluate(() => [...document.querySelectorAll('button')].some((b) => /^Hold all \(1\)$/.test(b.textContent.trim())) ? 'Hold all (1)' : 'no Hold all');
  await toastGone(); await clickRow([joiner, 'No bank account on record'], 'Hold salary');
  const pre = await until(() => document.querySelector('.ov .dlg textarea')?.value || '');
  await click('Hold salary', true); const t = await toast();
  const held = await until((j) => /On hold \(1\)/.test(document.body.innerText) && [...document.querySelectorAll('tr')].some((r) => r.textContent.includes(j) && r.textContent.includes('HLD-')) ? 'On hold (1)' : '', [joiner]);
  const x = await checklist(), bank = x.closures.find((c) => c.key === 'bank');
  await closureRow('Bank verification closure');
  return `${all} · prefilled "${pre}" · ${t} · ${held || 'not under On hold'} · ${bank.status}`;
}, '^Hold all \\(1\\) · prefilled "No bank account on record" · Salary put on hold · On hold \\(1\\) · Done$');
await step('Payroll run checklist: each closure closes once clear', async () => { const said = [];
  await go('/payroll'); await tab('Payroll run');
  for (const name of ['Attendance closure', 'Leave closure', 'Bank verification closure', 'Reimbursement closure', 'Deduction closure']) {
    if (!(await closureRow(name))) throw new Error('no row: ' + name);
    await wait(200); await click('Close', true); said.push(await toast()); await toastGone();
    await closureRow(name);
  }
  const all = await until(() => document.body.innerText.includes('5 of 5 closed') ? 'all five closed' : '');
  return `${said.filter((x) => / closed$/.test(x)).length} closed · ${all || 'not all closed'}`; }, '^5 closed · all five closed$');
await step('Payroll run: dry run', async () => { await tab('Payroll run'); await click('Run dry run'); const t = await toast();
  // A big run goes to the background: refresh until it is done.
  for (let i = 0; i < 40 && (await page.evaluate(() => /Calculating .* in the background/.test(document.body.innerText))); i++) { await wait(1500); await click('Refresh', true).catch(() => {}); }
  const st = await until(() => document.querySelector('.chain .step.cur')?.textContent === 'Dry run' && document.querySelectorAll('.tw table tbody tr').length > 1 ? 'stage Dry run, sheet filled' : '');
  return `${t} · ${st || 'no dry run sheet'}`; }, '(Dry run ready|background).* · stage Dry run, sheet filled');
await step('Payroll run: edit a row with a reason', async () => {
  const s = (await page.evaluateHandle(() => [...document.querySelectorAll('input[type=search]')].find((x) => x.placeholder.startsWith('Search name or ID')))).asElement();
  if (!s) throw new Error('no payroll sheet search');
  await setValue(s, 'EMP-1020'); await s.focus(); await page.keyboard.press('Enter'); await settle();
  await needRow(['EMP-1020', 'Adjust']); await clickRow(['EMP-1020'], 'Adjust');
  await fill('Other earning', '600'); await fill('Reason', 'Festival bonus agreed by HRBP'); await click('Save', true); const t = await toast();
  const r = await row(['EMP-1020', 'Adjusted']); return `${t} · ${r ? 'row shows Adjusted' : 'row not adjusted'}`; }, '^Saved · row shows Adjusted');
await step('Payroll run: verify', async () => { await go('/payroll'); await tab('Payroll run'); await click('Mark verified'); }, 'verified');
// Maker-checker (13 Nov 2026): whoever verified the run, or edited a row in it, cannot approve it. HR Admin made and
// verified the run above, so the Payroll Manager approves and publishes it, and HR Admin carries on afterwards.
const switchTo = async (id, role, path) => { const t = await tokenFor(id); await page.goto(BASE + '/api/health'); await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role); await go(path); };
await step('Payroll run: approve', async () => { await switchTo('EMP-1012', 'Payroll Manager', '/payroll'); await tab('Payroll run'); const mo = await page.evaluate(() => document.querySelector('.chain .mute')?.textContent || ''); await click('Approve and update register'); const t = await toast();
  const now = mo && (await row([mo, '₹'], 4000));
  await go('/payroll'); await tab('Payroll run'); const later = mo && (await row([mo, '₹']));
  return `${t} · ${mo} ${now ? 'in the register' : 'not in the register on screen'}${now ? '' : later ? ' (shown after a reload)' : ', nor after a reload'}`; }, 'Register updated · .+ in the register$');
await step('Payroll run: publish payslips', async () => { await click('Publish payslips'); }, 'published');
await switchTo('EMP-1002', 'HR Admin', '/payroll');
await step('Upcoming pay lines: open and download', async () => { await tab('Upcoming pay lines');
  if (!(await until(() => [...document.querySelectorAll('h3')].some((h) => h.textContent === 'Next six months')))) throw new Error('Next six months not shown');
  return await download('/payroll/upcoming.xlsx', () => click('Download all lines')); }, 'downloaded');
await step('Claims and changes: add an arrear', async () => { await tab('Claims and changes'); await seg('Arrears'); await pickEmp('Karan', 'EMP-1020'); await fill('Arrear for month', prevYm); await fill('Amount', '2200');
  await fill('Reason type', 'Short payment in earlier month'); await fill('Details shown on the payslip', 'Night allowance for two months'); await click('Send for approval'); const t = await toast();
  return `${t} · ${(await row(['Night allowance for two months', 'Pending approval'])) ? 'listed' : 'not listed'}`; }, 'sent for approval.* · listed');
// Declaration and proof windows are now under Claims and changes, Tax declarations, with "Open now" / "Close now".
await step('Tax: windows saved', async () => { await tab('Claims and changes'); await seg('Tax declarations'); await clickRow(['Proof upload'], 'Open now'); const t = await toast();
  return `${t} · ${(await row(['Proof upload', 'Open', 'set by hand'])) ? 'proof upload Open' : 'proof upload not open'}`; }, '^Saved · proof upload Open');
await step('Tax: review a declaration', async () => { await clickRow(['EMP-1020'], 'Review');
  return await until(() => { const d = document.querySelector('.ov .dlg'); return d?.querySelector('h2') && !d.querySelector('.loading') ? `${d.querySelector('h2').textContent} · ${d.querySelector('.pill')?.textContent}${d.innerText.includes('₹60,000') ? ' · PPF ₹60,000' : ''}` : ''; }); }, 'Karan Khan: tax proofs · Old regime · PPF ₹60,000');
// Deductor details are inline fields on Tax and Form 16, saved as each one loses focus.
await step('TDS: deductor details saved', async () => { await go('/payroll'); await tab('Tax and Form 16'); await fillBlur('Person responsible for deduction', 'Deepak Arora'); const t = await toast();
  await toastGone(); await fillBlur('Designation', 'Payroll Manager'); await toast();
  await go('/payroll'); await tab('Tax and Form 16');
  const kept = await until(() => { const f = __field; return f('Person responsible for deduction')?.value === 'Deepak Arora' && f('Designation')?.value === 'Payroll Manager'; });
  return `${t} · ${kept ? 'name and designation kept' : 'not kept after reload'}`; }, '^Deductor details updated · name and designation kept');
// Release is now a switch, "Employees can see their Form 16".
await step('Form 16: release to employees', async () => {
  const r = await page.evaluate(() => { const s = document.querySelector('button.sw[aria-label="Employees can see their Form 16"]'); if (!s) return 'no switch'; if (s.disabled) return 'switch disabled'; if (s.getAttribute('aria-pressed') === 'true') return 'already on'; s.click(); return 'ok'; });
  if (r !== 'ok') throw new Error('Form 16 release: ' + r);
  await settle(); return await toast(); }, 'released to employees');
await step('Form 16: view one', async () => { await clickRow(['EMP-1002'], 'View');
  return await until(() => { const t = document.querySelector('.ov .dlg')?.innerText || ''; return t.includes('Part A') && t.includes('Part B') ? 'Part A and Part B shown' : ''; }); }, 'Part B');
await step('Statutory: ECR preview and download', async () => { await go('/payroll'); await tab('Statutory and insurance files'); await fill('File', 'PF contribution file (ECR)'); await fill('Payroll month', lastYm);
  await click('Prepare file');
  const n = await until(() => [...document.querySelectorAll('.card .mute')].map((x) => x.textContent).find((x) => /· \d+ rows?$/.test(x)) || '');
  if (!n) throw new Error('no file prepared');
  return `${n} · ${await download('/statutory/ecr/sheet', () => click('Download CSV'))}`; }, '· [1-9]\\d* rows · downloaded');
await step('Full and final: list opens', async () => { await go('/payroll'); await tab('Full and final');
  return await until(() => { const c = [...document.querySelectorAll('.card')].find((x) => x.querySelector('h3')?.textContent === 'Full and final settlement'); return c?.querySelector('.tw table') ? `listed (${c.querySelectorAll('tbody tr').length} rows)` : ''; }); }, 'listed');

// ---------------- Employee dues and inventory ----------------
await go('/dues');
// Stock is added to the central store from the Inventory tab, uniform by quantity and assets by serial number.
await step('Inventory: add stock with serials', async () => { await tab('Inventory'); await click('Add stock'); await until(() => !document.querySelector('.ov .dlg .loading'));
  await setValue(await rowInput('Shirt'), '12');
  await page.evaluate(() => { const tr = [...__sc().querySelectorAll('tr')].find((x) => x.cells[0]?.querySelector('b')?.textContent === 'Laptop'); const d = tr?.querySelector('details'); if (d) d.open = true; });
  const ok = await page.evaluate(() => { const tr = [...__sc().querySelectorAll('tr')].find((x) => x.cells[0]?.querySelector('b')?.textContent === 'Laptop'); const t = tr?.querySelector('textarea'); return t ? __set(t, 'LP-501\nLP-502') : false; });
  if (!ok) throw new Error('no serial numbers box for Laptop');
  await click('Add to central store'); const t = await toast();
  return `${t} · ${(await row(['Laptop', 'LP-501, LP-502'])) ? 'serials in the movements' : 'serials not listed'}`; }, '14 units added to the central store · serials in the movements');
await step('Inventory: issue uniform with recovery', async () => {
  // Uniform is issued from the employee's store (their HRBP's), so central stock is transferred there first.
  const store = await page.evaluate(() => fetch('/api/inventory/employee/EMP-1020', { headers: { Authorization: 'Bearer ' + localStorage.getItem('hrms.token'), 'X-Role': localStorage.getItem('hrms.role') } }).then((r) => r.json()).then((d) => d.store === 'central' ? '' : d.storeName.replace(/'s store$/, '')));
  if (store) { await tab('Inventory'); await click('Transfer to an HRBP'); await fill('Transfer to', store); await until(() => !__sc().querySelector('.loading'));
    await setValue(await rowInput('Shirt'), '2'); await click('Transfer', true); const tr = await toast(); if (!/transferred/i.test(tr)) throw new Error('transfer to ' + store + ': ' + tr); await toastGone(); }
  await tab('Uniform'); await pickEmp('Karan', 'EMP-1020');
  for (const it of ['Shirt', 'Trousers', 'Apron', 'Name badge', 'Shoes']) { const h = await rowInput(it).catch(() => null); if (h) await setValue(h, it === 'Shirt' ? '2' : '0'); }
  await click('Issue and schedule recovery'); const t = await toast();
  return `${t} · ${(await row(['EMP-1020', '2 × Shirt'])) ? 'listed with recovery' : 'not listed'}`; }, 'Recovery scheduled · listed with recovery');
await step('Inventory: issue an asset', async () => { await tab('Assets'); await clickRow(['LP-501'], 'Issue'); await pickEmp('Karan', 'EMP-1020'); await click('Issue', true); const t = await toast();
  return `${t} · ${(await row(['LP-501', 'Issued', 'EMP-1020'])) ? 'shown with Karan' : 'not shown issued'}`; }, '^Issued · shown with Karan');
// "Take back" is now "Returned" on the asset row, which opens Return from an employee.
await step('Inventory: take an asset back', async () => { await tab('Assets'); await clickRow(['LP-501', 'EMP-1020'], 'Returned');
  const box = await until(() => { const tr = [...__sc().querySelectorAll('tr')].find((x) => x.textContent.includes('LP-501')); const c = tr?.querySelector('input[type=checkbox]'); if (c && !c.checked) c.click(); return !!c; });
  if (!box) throw new Error('LP-501 not in the return dialog');
  await click('Record return'); const t = await toast();
  return `${t} · ${(await row(['LP-501', 'Available'])) ? 'available again' : 'still issued'}`; }, 'Return recorded.* · available again');
// Advances, deductions and salary holds are raised from inline cards (no dialogs).
await step('Dues: give an advance', async () => { await tab('Advances'); await pickEmp('Karan', 'EMP-1020'); await fill('Amount', '6000'); await fill('Recover in', '3 instalments'); await fill('Reason', 'Medical emergency');
  await click('Send for approval'); const t = await toast(); return `${t} · ${(await row(['EMP-1020', 'Medical emergency', 'Pending approval'])) ? 'listed' : 'not listed'}`; }, 'sent for approval.* · listed');
await step('Dues: add a deduction', async () => { await tab('Deductions'); await pickEmp('Karan', 'EMP-1020'); await fill('Type', 'Cash shortage'); await fill('Amount', '1500'); await fill('Employee was heard', 'Yes');
  await fill('What happened', 'Till short by 1,500 on the 3rd'); await click('Send for approval'); const t = await toast();
  return `${t} · ${(await row(['EMP-1020', 'Till short by 1,500', 'Pending approval'])) ? 'listed' : 'not listed'}`; }, 'sent for approval.* · listed');
await step('Dues: put a salary on hold', async () => { await tab('Salary holds'); await pickEmp('Divya', 'EMP-1032'); await fill('Reason', 'Bank or KYC issue'); await fill('Details', 'KYC documents expired');
  await click('Put on hold', true); const t = await toast(); return `${t} · ${(await row(['EMP-1032', 'KYC documents expired', 'Active'])) ? 'listed active' : 'not listed'}`; }, 'on hold · listed active');
await step('Dues: release the hold', async () => { await clickRow(['EMP-1032', 'KYC documents expired', 'Active'], 'Release'); await fill('Reason for releasing', 'KYC renewed'); await click('Release', true);
  const t = await toast(); return `${t} · ${(await row(['EMP-1032', 'Released', 'KYC renewed'])) ? 'shown released' : 'not released'}`; }, '^Hold released · shown released');

// ---------------- Compensation ----------------
await go('/compensation');
let newVer = '';
await step('Compensation: new structure version', async () => { await tab('Salary structure'); await click('New version from'); const t = await toast(); newVer = /^(.+?) created/.exec(t)?.[1] ?? '';
  return `${t} · ${newVer && (await until((v) => [...document.querySelectorAll('.seg button')].some((b) => b.textContent.startsWith(v)), [newVer])) ? 'listed' : 'not listed'}`; }, 'created.* · listed');
// Mapping people to a version moved to Payroll, Structure mapping.
await step('Compensation: map people to a version', async () => { if (!newVer) throw new Error('no new version from the previous step');
  await go('/payroll'); await tab('Structure mapping'); await fill('To version', newVer); await fill('Applicable from', todayS); await fill('Employment type', 'Retainer'); await settle(); await loaded();
  await click('Select all'); await click('Apply to'); return await toast(); }, '^[1-9]\\d* employees mapped');
// Pay bands are edited in place and saved when the field loses focus (no dialog, no toast): read the band back.
await step('Compensation: edit a pay band', async () => { await go('/compensation'); await tab('Pay bands');
  const city = await page.evaluate(() => { const s = [...document.querySelectorAll('label.field')].find((l) => l.querySelector('span')?.textContent === 'City')?.querySelector('select'); return s?.selectedOptions[0]?.textContent || ''; });
  const g = await page.evaluate(() => [...document.querySelectorAll('tr')].find((x) => x.querySelector('input[type=number]'))?.cells[0].textContent.trim() || '');
  await setValue(await rowInput(g, 0), '16000'); await setValue(await rowInput(g, 1), '24000'); await blur(await rowInput(g, 1)); await settle();
  const glance = await until((g, city) => { const tb = [...document.querySelectorAll('.card')].find((c) => c.querySelector('h3')?.textContent === 'All cities at a glance')?.querySelector('table'); if (!tb) return '';
    const col = [...tb.querySelectorAll('th')].findIndex((th) => th.textContent === city), tr = [...tb.querySelectorAll('tbody tr')].find((x) => x.cells[0].textContent.trim() === g);
    const v = tr?.cells[col]?.textContent || ''; return v.includes('16,000') ? v : ''; }, [g, city]);
  return glance ? `Band saved: ${g} in ${city} ${glance}` : `band not saved for ${g} in ${city}`; }, 'Band saved: .*₹16,000 to ₹24,000');
// Labour welfare fund rules are edited in the table and saved when a value loses focus.
await step('Compensation: change an LWF rule', async () => { await tab('Labour welfare fund'); await setValue(await rowInput('Delhi', 0), '1'); await blur(await rowInput('Delhi', 0)); const t = await toast();
  await go('/compensation'); await tab('Labour welfare fund'); const v = await (await rowInput('Delhi', 0)).evaluate((el) => el.value);
  return `${t} · Delhi employee ${v === '1' ? 'now ₹1' : 'still ' + v}`; }, '^Saved · Delhi employee now ₹1');
await step('Compensation: edit a budget', async () => { await tab('Budgets');
  const loc = await page.evaluate(() => [...document.querySelectorAll('tbody tr')].filter((x) => x.querySelector('input')).pop()?.cells[0].childNodes[0].textContent.trim() || '');
  await setValue(await rowInput(loc, 0), '45'); await blur(await rowInput(loc, 0)); const t = await toast();
  await go('/compensation'); await tab('Budgets'); const v = await (await rowInput(loc, 0)).evaluate((el) => el.value);
  return `${t} · ${loc} headcount budget ${v}`; }, '^Saved · .+ headcount budget 45$');

// ---------------- Separation ----------------
await go('/separation');
await step('Separation: start a termination', async () => { await click('Start a separation'); await fill('Type of exit', 'Termination');
  await pickPerson('Divya', 'EMP-1032', 'Employee'); await settle(); await until(() => /notice in policy \d+ days/.test(__sc().textContent));
  await fill('Reason', 'Misconduct'); await fill('Notice period', 'pay'); await fill('Note', 'Repeated cash shortages after written warnings'); await click('Start', true); }, 'Termination sent for approval');
// The case dialog's heading is the employee; the exit type is the pill beside it.
await step('Separation: case view opens', async () => { await go('/separation'); await clickRow(['EMP-1032', 'Termination'], 'Open');
  return await until(() => { const d = document.querySelector('.ov .dlg'); const h = d?.querySelector('h2'); return h ? `${h.textContent} · ${d.querySelector('.pill')?.textContent}` : ''; }); }, 'Divya Patil.* · Termination');
await step('Separation: cancel the case', async () => { await go('/separation'); await clickRow(['EMP-1032', 'Termination'], 'Open'); await until(() => !!document.querySelector('.ov .dlg h2'));
  promptValue = 'Settled after a hearing'; await click('Cancel this exit'); const t = await toast();
  return `${t} · ${(await until(() => [...document.querySelectorAll('.ov .dlg .pill')].some((p) => p.textContent === 'Cancelled'))) ? 'case shows Cancelled' : 'case not cancelled'}`; }, 'Exit cancelled · case shows Cancelled');

// ---------------- Other pages ----------------
await step('Employee file: pick and read', async () => { await go('/file');
  await pickPerson('Karan', 'EMP-1020');
  await settle(); await loaded();
  return await until(() => { const h = document.querySelector('h2')?.textContent || '', r = [...document.querySelectorAll('h3')].find((x) => /^Ready for .* payroll\?$/.test(x.textContent)); return h.startsWith('Karan Khan') && r ? `file shown: ${r.textContent}` : ''; }); }, 'file shown');
// Reports run as soon as one is picked (no "Show" button).
await step('Reports: run three reports', async () => { await go('/reports'); const out = [];
  for (const n of ['Employee master list', 'Headcount summary', 'Payroll register']) {
    if (!(await page.evaluate((n) => { const b = [...document.querySelectorAll('button.nav')].find((b) => b.textContent.trim() === n); b?.click(); return !!b; }, n))) throw new Error('no report: ' + n);
    out.push(n + ': ' + (await until((n) => document.querySelector('.card h2')?.textContent === n && !document.querySelector('.card .loading') && [...document.querySelectorAll('.card .mute')].map((x) => x.textContent).find((x) => /^\d+ records?/.test(x)), [n], 15000) || 'no result'));
  }
  return out.join(', '); }, '^Employee master list: [1-9]\\d* records, Headcount summary: [1-9]\\d* records?, Payroll register: [1-9]\\d* records');
await step('Attendance: add a punch machine', async () => { await go('/attendance'); await tab('Punch machine setup'); await click('Add a machine'); await fill('Name', 'Back gate'); await click('Add', true);
  const k = await until(() => { const d = document.querySelector('.ov .dlg'); return d?.querySelector('h2')?.textContent === 'Device key' && d.querySelector('.mono')?.textContent.length > 8; });
  if (!k) return 'no device key shown';
  await click('Done', true); return `key shown once · ${(await row(['Back gate', 'Active'])) ? 'machine listed' : 'not listed'}`; }, 'key shown once · machine listed');
// Set shift: a rule from a date is saved as a version and listed in the history. The retired Split shift is used, so
// no one's attendance changes; the date is the first of next month, so a locked payroll month never refuses it.
await step('Masters: set a shift\'s half day rule from a date', async () => { await go('/masters?tab=shift'); await clickRow(['Split', 'SPL'], 'Edit');
  if (!(await until(() => document.querySelector('h2')?.textContent.startsWith('Set shift') && !!__field('Half day rule <')))) throw new Error('Set shift form not shown');
  const from = monthsAgo(-1) + '-01';
  await fill('Half day rule <', '5:00'); await click('Save', true); await fill('Effective from', from); await fill('Note for the history', 'Forms test');
  await click('Save from'); const t = await toast();
  const v = await row(['Half day <: policy → 5:00', 'Forms test']);
  return `${t} · ${v ? 'version listed' : 'no version listed'}`; }, '^Saved\\. The shift applies from .* · version listed');
// Edit on every master row: the Add form's fields, prefilled; a rename shows in the list (and everything that
// uses the name follows it on the server).
await step('Masters: rename a region with Edit', async () => { await go('/masters?tab=region'); await clickRow(['West'], 'Edit');
  if (!(await until(() => document.querySelector('.ov .dlg h2')?.textContent === 'Edit “West”'))) throw new Error('no Edit form');
  await fill('Name', 'West India'); await click('Save', true); const t = await toast();
  await tab('States'); const st = await row(['Maharashtra', 'Region: West India']);
  return `${t} · ${(await go('/masters?tab=region'), await row(['West India', 'Active'])) ? 'renamed in the list' : 'not renamed'}${st ? ', its states follow' : ''}`; }, '^Saved · renamed in the list, its states follow');
await step('Masters: Edit a designation: name and grades in one form', async () => { await go('/masters?tab=desig'); await clickRow(['Kitchen Helper'], 'Edit');
  if (!(await until(() => [...document.querySelectorAll('.ov .dlg .field span')].some((x) => x.textContent === 'Grades this designation can be offered')))) throw new Error('no grades in the Edit form');
  await fill('Name', 'Kitchen Assistant');
  await page.evaluate(() => [...document.querySelectorAll('.ov .dlg label.chkl')].find((l) => l.textContent.trim() === 'G2')?.querySelector('input')?.click()); await wait(200);
  await click('Save', true); const t = await toast();
  return `${t} · ${(await row(['Kitchen Assistant', 'Grades: G1, G2'])) ? 'listed with G1, G2' : 'not listed'}`; }, '^Saved · listed with G1, G2');
// Master update: the workbook with the current data, uploaded unchanged, checks with no change and no error.
await step('Masters: Master update, current data downloaded and uploaded unchanged', async () => { await go('/masters'); await tab('Master update');
  const before = new Set(Object.keys(downloads));
  const d = await download('/master-update/download', () => click('Download with current data'));
  const name = Object.entries(downloads).find(([g]) => !before.has(g))?.[1].name;
  const h = (await page.evaluateHandle(() => document.querySelector('input[type=file]'))).asElement();
  if (!h || !name) throw new Error('no file input or no download');
  await h.uploadFile(path.join(DLDIR, name)); await wait(300);
  await click('Check file');
  const res = await until(() => { const c = [...document.querySelectorAll('.card')].find((x) => x.querySelector('h3')?.textContent === 'Check result');
    return c ? `${c.querySelector('.row.sp span')?.textContent} · ${c.querySelectorAll('tbody')[0]?.rows.length} sheets` : ''; }, [], 15000);
  return `${d} · ${res}`; }, '^downloaded master-update-[\\d-]+\\.xlsx .* · 0 changes · 0 rows with errors · 15 sheets$');
// Google Drive document storage, before an OAuth client is set: the card shows the one-time Google Cloud steps and
// the exact redirect URI to register (through the web app's address, so it works behind the dev proxy).
await step('Masters › Company: Google Drive storage card, not connected', async () => { await go('/masters?tab=company');
  const r = await until(() => { const c = document.getElementById('drive-storage'); if (!c) return '';
    const t = c.innerText, pill = c.querySelector('.pill')?.textContent, steps = c.querySelectorAll('ol li').length;
    const uri = [...c.querySelectorAll('.mono')].map((m) => m.textContent).find((x) => x.endsWith('/api/drive/callback'));
    return t.includes('enable the Google Drive API') && t.includes('Internal') ? `${pill.toLowerCase()} · ${steps} setup steps · ${uri}` : ''; });
  if (!r) return 'card not shown';
  return r.endsWith(' · ' + BASE + '/api/drive/callback') ? r.replace(/ · [^ ]+$/, ` · redirect URI on the web address (${BASE}/api/drive/callback)`) : r; },
  '^not connected · 5 setup steps · redirect URI on the web address \\(http\\S+/api/drive/callback\\)$');
await step('Dashboard: every tab renders', async () => { await go('/');
  for (const t of ['Workforce', 'Payroll', 'Attendance', 'Compliance', 'Approvals', 'Overview']) { await tab(t);
    const bad = await page.evaluate((t) => document.querySelector('.tabs .tab.on')?.textContent !== t ? 'not selected' : document.querySelector('.note.bad')?.textContent || '', t);
    if (bad) throw new Error(`${t}: ${bad}`); }
  return 'all tabs'; }, 'all tabs');

// ---------------- Employee again: payslip and Form 16 ----------------
await as('EMP-1020', 'Employee', '/me');
await step('Employee: open the payslip', async () => { await tab('Payslips'); await click('View');
  return await until(() => { const d = document.querySelector('.ov .dlg'); return d?.querySelector('.sl')?.textContent.includes('Net pay for the month') ? `payslip shown: ${d.querySelector('h2')?.textContent}` : ''; }); }, 'payslip shown');
await step('Employee: Form 16 listed after release', async () => { await go('/me'); await tab('Form 16');
  const y = await until(() => [...document.querySelectorAll('.card')].find((c) => /^Form 16, FY /.test(c.querySelector('h3')?.textContent || '') && [...c.querySelectorAll('button')].some((b) => b.textContent === 'View'))?.querySelector('h3').textContent || '');
  if (!y) return 'missing';
  await click('View', true);
  return (await until(() => (document.querySelector('.ov .dlg')?.innerText || '').includes('Part B'))) ? `listed: ${y}, opens with Part B` : `listed: ${y}, does not open`; }, '^listed: .*opens with Part B');

// Withdraw resignation starts from My view › Separation (the Approvals form and route). HR Admin first accepts the
// resignation raised above (through the API, each stage with a comment), so the employee is serving notice.
await step('Employee: withdraw the resignation from Separation', async () => {
  const hr = await login('EMP-1002'), me = await page.evaluate(() => localStorage.getItem('hrms.token'));
  const call = (p, t, role, body) => fetch(BASE + '/api' + p, { method: body ? 'POST' : 'GET', headers: { Authorization: 'Bearer ' + t, 'X-Role': role }, body: body ? JSON.stringify(body) : undefined }).then((r) => r.json());
  const rs = (await call('/me/requests?wf=resign', me, 'Employee')).find((r) => r.status === 'Pending' && (!r.payload.rkind || r.payload.rkind === 'new'));
  for (let i = 0; rs && i < 8; i++) { const d = await call('/approvals/' + rs.id, hr, 'HR Admin'); if (d.status !== 'Pending') break; await call(`/approvals/${rs.id}/approve`, hr, 'HR Admin', { note: 'Accepted in the UI test' }); }
  await go('/my-exit'); await click('Withdraw resignation');
  if (!(await until(() => (document.querySelector('.ov .dlg')?.innerText || '').includes('Approval route')))) throw new Error('no route in the withdraw form: ' + (await noteBad()));
  await fill('Note', 'Staying on after all'); await click('Send for approval'); const t = await toast();
  const l = await until(() => [...document.querySelectorAll('.note')].find((n) => /Withdraw resignation/.test(n.innerText) && /Waiting for \S/.test(n.innerText)) ? 'listed waiting' : '');
  return `${t} · ${l || 'not listed'}`; }, '^Sent for approval · listed waiting$');


// ---------------- Onboarding in stages: SI → hiring manager → Function Head → interview → offer v1 → OI (accepted) → JI → Employee ID ----------------
/** Signs in as anyone (activated with the development OTP when needed) in a role view, and opens a page. */
const asAny = async (id, role, path) => { const t = await tokenFor(id); await page.goto(BASE + '/api/health'); await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role); await go(path); };
/** Sets the input with this aria-label (work experience rows). */
const aria = async (label, v) => { const ok = await page.evaluate((l, v) => { const el = __sc().querySelector(`[aria-label="${l}"]`); return el ? __set(el, v) : false; }, label, v); if (!ok) throw new Error('no input ' + label); await wait(120); };
/** Uploads a fixture into the file input of the table row whose text has `label`. */
const uploadRow = async (label, file) => {
  const h = (await page.evaluateHandle((l) => [...__sc().querySelectorAll('tr')].find((x) => x.cells[0]?.textContent.includes(l))?.querySelector('input[type=file]') || null, label)).asElement();
  if (!h) throw new Error('no upload for ' + label);
  await freshToasts(); await h.uploadFile(DIR + '/' + file); await wait(300); await settle(); return toast();
};
/** Opens the inbox item of a person in Approvals (To approve) whose line has `what`. */
const openItem = async (name, what) => {
  const ok = await until((n, w) => { const b = [...document.querySelectorAll('button.irow')].find((x) => x.innerText.includes(n) && x.innerText.includes(w)); b?.click(); return !!b; }, [name, what]);
  if (!ok) throw new Error(`${what} of ${name} is not waiting in To approve`);
  await wait(300); await settle();
};
const STAGE_PERSON = { 'HRBP Corporate': 'EMP-1003', CHRO: 'EMP-1001', HRBP: 'EMP-1004', 'Cluster Manager': 'EMP-1007', Payroll: 'EMP-1011', 'Payroll Manager': 'EMP-1012', 'Finance Head': 'EMP-1013' };
/** Approves a request stage by stage as the person each stage is mapped to (sample organisation). */
const approveChain = async (id) => {
  const hr = await tokenFor('EMP-1002');
  for (let i = 0; i < 6; i++) {
    const r = await apiAs(hr, 'HR Admin', 'GET', '/approvals/' + id);
    if (r.status !== 'Pending') return r.status;
    const st = r.chain[r.step], who = STAGE_PERSON[st];
    if (!who) throw new Error(`${id}: no one to approve ${st}`);
    await apiAs(await tokenFor(who), st, 'POST', `/approvals/${id}/approve`, { note: 'Checked in the UI test' });
  }
  throw new Error(id + ' still pending');
};
let si = '', emp = '';
/** Opens the candidate (every step ends with Escape, which closes the dialog). */
const openCand = async () => { await go(`/onboarding?id=${si}`); if (!(await until(() => document.querySelector('.ov .dlg .bigid')?.textContent))) throw new Error('the candidate did not open'); };
await as('EMP-1002', 'HR Admin', '/onboarding');
await step('Onboarding: Form setup shows the hiring settings (waiting window, background check, offer expiry)', async () => { await tab('Form setup');
  const v = await until(() => { const w = __field('Waiting window (days)')?.value; return w ? `${w} · ${__field('Background check')?.value} · ${__field('When the window ends')?.value} · ${__field('An offer stays open (days)')?.value}` : ''; });
  const hint = await until(() => /Joining ID \(JI\) and is a normal employee from day 1/.test(document.body.innerText) ? 'hint shown' : '');
  return `${v} · ${hint}`; }, '^7 · true · false · 7 · hint shown$');
await step('Onboarding: shortlist a candidate with work experience and a résumé (SI)', async () => { await tab('Shortlist');
  await fill('First name', 'Meera'); await fill('Last name', 'Joshi'); await fill('Mobile number', '9876507001'); await fill('Personal email ID', 'meera.joshi@example.com');
  await fill('Date of birth', '1997-03-15'); await fill('Gender', 'Female'); await fill('Current address', 'DLF Phase 3, Gurugram');
  await aria('Company 1', 'Hotel Orchid'); await aria('Designation 1', 'Sales Associate'); await aria('From 1', '2021-06'); await aria('To 1', '2024-12');
  await fill('Existing CTC (₹ a year)', '360000'); await fill('Work city', 'Gurugram'); await fill('Reporting location', 'Gurugram – Sector 29 Hub');
  await fill('Department', 'Sales'); await fill('Designation', 'Sales Executive'); await fill('Hiring manager', 'Anil Kapoor');
  await upload('bill.pdf');
  const route = await until(() => { const t = document.querySelector('.split .chain')?.innerText.replace(/\s+/g, ' ').trim() || ''; return t.includes('Function Head') ? t : ''; });
  await click('Send for approval'); const t = await toast(); si = /SI-\d{4}/.exec(t)?.[0] || '';
  const opened = await until(() => document.querySelector('.ov .dlg .bigid')?.textContent || '');
  return `${t} · ${route} · opened ${opened}`; }, '^SI-\\d{4} sent for approval · Line Manager: Anil Kapoor → Function Head: Rohit Khanna · opened SI-\\d{4}$');
await asAny('EMP-1009', 'Line Manager', '/approvals');
await step('Onboarding: the hiring manager approves the shortlist in Approvals', async () => {
  if (!si) throw new Error('no shortlist from the previous step');
  await openItem('Meera Joshi', 'Shortlist approval');
  const shown = await until(() => /Hotel Orchid/.test(document.querySelector('.inbox')?.innerText || '') && /3,60,000/.test(document.querySelector('.inbox')?.innerText || '') ? 'experience and CTC shown' : '');
  await until(() => { const i = [...document.querySelectorAll('input')].find((x) => (x.placeholder || '').startsWith('Comment')); return i ? __set(i, 'Good fit for corporate sales') : false; });
  await click(/^Approve$/); return `${shown} · ${await toast()}`; }, '^experience and CTC shown · Approved. Next: Function Head$');
await asAny('EMP-1010', 'Function Head', '/approvals');
await step('Onboarding: the Function Head approves the shortlist', async () => {
  await openItem('Meera Joshi', 'Shortlist approval');
  await until(() => { const i = [...document.querySelectorAll('input')].find((x) => (x.placeholder || '').startsWith('Comment')); return i ? __set(i, 'Approved for Sales') : false; });
  await click(/^Approve$/); return toast(); }, '^Fully approved. Interview is next.$');
await asAny('EMP-1009', 'Line Manager', '/approvals');
await step('Onboarding: the hiring manager records the interview as Selected (no Offer ID yet)', async () => {
  const bell = (await apiAs(await tokenFor('EMP-1009'), 'Line Manager', 'GET', '/notices')).notices.some((n) => n.text.startsWith('Interview waiting for you: Meera Joshi')) ? 'told' : 'not told';
  await openItem('Meera Joshi', 'Interview');
  if (!(await until(() => [...document.querySelectorAll('button')].some((b) => b.textContent.trim() === 'Not selected')))) throw new Error('no Selected / Not selected buttons');
  await fill('Feedback', 'Clear on the product, good with clients'); await click('Selected', true); return `${await toast()} · ${bell}`; }, '^Meera Joshi selected. HR raises the offer · told$');
await as('EMP-1002', 'HR Admin', '/onboarding');
await step('Onboarding: HR raises offer v1 under the same number, prefilled from the shortlist', async () => {
  await go(`/onboarding?id=${si}`); await until(() => document.querySelector('.ov .dlg .bigid')?.textContent.startsWith('SI-'));
  const told = (await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', '/notices')).notices.some((n) => n.text.includes('Meera Joshi was selected')) ? 'HR told' : 'HR not told';
  await click(/^Raise offer v1$/);
  const pre = await page.evaluate(() => __field('Personal email ID')?.value || '');
  await fill('Date of joining', todayS); await fill('Grade', 'G3'); await fill('Role', 'Sales'); await fill('Skill', 'Accounts'); await fill('Monthly gross (₹)', '32000');
  const up = await uploadRow('Aadhaar card', 'bill.pdf');
  await click('Send offer for approval'); return `${await toast()} · ${pre} · ${up} · ${told}`; }, '^Offer v1 for SI-\\d{4} sent for approval · meera.joshi@example.com · Attached bill.pdf · HR told$');
await step('Onboarding: offer approved by its approvers (API), released, HR records Accepted: the Offer ID (OI), no Employee ID yet', async () => {
  const x = await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', `/candidates/${si}`);
  const st = await approveChain(x.offer_request_id);
  await go(`/onboarding?id=${si}`); await click('Release offer'); await click('Release offer', true); const t1 = await toast(); await toastGone();
  await until(() => [...document.querySelectorAll('.ov .dlg button')].some((b) => b.textContent.trim() === 'Accepted')); await click('Accepted', true); const t2 = await toast();
  const y = await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', `/candidates/${si}`);
  return `${st} · ${t1} · ${t2} · ${y.employee_id ? 'Employee ID ' + y.employee_id : 'no Employee ID'}`; }, '^Approved · Offer released · Meera Joshi accepted the offer. Offer ID OI-\\d{4} · no Employee ID$');
await step('Onboarding: mark joined, the Joining ID has the same number (JI), a normal employee from day 1', async () => { await openCand();
  await fill('Actual joining date', todayS); await click('Mark joined', true); const t = await toast();
  const id = await until(() => { const b = document.querySelector('.ov .dlg .bigid')?.textContent || ''; return b.startsWith('JI-') ? b : ''; });
  return `${t} · ${id}`; }, '^Meera Joshi joined: JI-\\d{4}. Punches and pay count from .+ · JI-\\d{4}$');
// The joiner fills their own details (joining_link.go): HR sends the link, the joiner opens it on a phone, confirms
// the code (the development echo shows it), fills their fields, uploads their documents and submits; HR then sees the
// values marked Joiner, fills HR's fields and saves. The Employee ID waits for every approval (hiring.go).
let joinURL = '';
await step("Onboarding: HR emails the joiner's link (development echo)", async () => {
  await openCand(); await click('Joining details and documents');
  if (!(await until(() => document.querySelector('.ov .dlg .jlink')?.innerText.includes('Not sent')))) throw new Error("no Joiner's link panel");
  await click('Email the link', true); const t = await toast();
  joinURL = await until(() => document.querySelector('.ov .dlg .jlink input[readonly]')?.value || '');
  const st = await until(() => document.querySelector('.ov .dlg .jlink .pill')?.textContent || '');
  return `${t} · ${/\/join\/[A-Za-z0-9_-]{43}$/.test(joinURL) ? 'link shown' : 'no link: ' + joinURL} · ${st}`;
}, '^Link emailed to meera.joshi@example.com · link shown · Sent, not opened yet$');
await step('Joiner: opens the link on a phone, confirms the code, fills the details, uploads documents, submits', async () => {
  if (!joinURL) throw new Error('no link from the previous step');
  const p2 = await browser.newPage();
  await p2.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
  p2.on('pageerror', (e) => errs.push('JOINER PAGE ERROR ' + e.message.split('\n')[0]));
  p2.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 500) errs.push(`${r.status()} ${r.url().replace(BASE, '')}`); });
  const u2 = async (fn, args = [], ms = 10000) => { const t0 = Date.now(); for (;;) { const v = await p2.evaluate(fn, ...args).catch(() => ''); if (v || Date.now() - t0 > ms) return v || ''; await wait(150); } };
  const click2 = async (text) => { if (!(await u2((t) => { const b = [...document.querySelectorAll('button')].find((x) => x.textContent.trim() === t && !x.disabled); b?.click(); return !!b; }, [text]))) throw new Error('joiner page: no button ' + text); await wait(300); };
  const set2 = async (label, v) => {
    const ok = await p2.evaluate((label, v) => {
      const l = [...document.querySelectorAll('label.lgf')].find((x) => x.querySelector('span')?.textContent.replace(/\s*\*\s*$/, '').trim() === label);
      const el = l?.querySelector('input,select'); if (!el) return false;
      Object.getOwnPropertyDescriptor(el.tagName === 'SELECT' ? HTMLSelectElement.prototype : HTMLInputElement.prototype, 'value').set.call(el, v);
      el.dispatchEvent(new Event(el.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true })); return true;
    }, label, v);
    if (!ok) throw new Error('joiner page: no field ' + label); await wait(100);
  };
  const toast2 = () => u2(() => document.getElementById('toast')?.textContent || '');
  const up2 = async (label) => {
    const h = (await p2.evaluateHandle((l) => [...document.querySelectorAll('.jpdoc')].find((x) => x.textContent.includes(l))?.querySelector('input[type=file]') || null, label)).asElement();
    if (!h) throw new Error('joiner page: no upload for ' + label);
    await h.uploadFile(DIR + '/bill.pdf');
    if (!(await u2((l) => [...document.querySelectorAll('.jpdoc')].find((x) => x.textContent.includes(l))?.textContent.includes('Uploaded: bill.pdf'), [label]))) throw new Error(label + ' not uploaded');
  };
  try {
    await p2.goto(BASE + '/join/' + joinURL.split('/join/')[1]);
    await click2('Email me the code');
    const code = await u2(() => document.querySelector('.lgsms b.mono')?.textContent || '');
    if (!/^\d{6}$/.test(code)) throw new Error('no development code shown');
    await p2.evaluate((c) => { const el = document.querySelector('input[autocomplete="one-time-code"]'); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(el, c); el.dispatchEvent(new Event('input', { bubbles: true })); }, code);
    await click2('Continue');
    const hello = await u2(() => (document.querySelector('.jpcard h1')?.textContent || '').startsWith('Hello') ? document.querySelector('.jpcard h1').textContent : '');
    const noPay = await p2.evaluate(() => !/gross|CTC|₹|Anil Kapoor|Reporting manager|Shift/i.test(document.body.innerText)) ? 'no pay or HR fields' : 'shows pay or HR fields';
    for (const [l, v] of [['Aadhaar number', '123412347001'], ['PAN', 'ABCDE7001F'], ['Do you already have a UAN (PF number)?', 'No'], ['Bank name', 'HDFC Bank'], ['Account number', '50100123457001'], ['IFSC code', 'HDFC0001234'], ['Emergency contact name', 'Kiran Joshi'], ['Emergency contact mobile', '9876507002']]) await set2(l, v);
    await click2('Save and continue later'); const saved = await toast2();
    await up2('PAN card'); await up2('Passport-size photo');
    await click2('Submit to HR');
    const refused = await u2(() => document.querySelector('.lgerr')?.innerText.includes('Upload your Cancelled cheque or bank passbook') ? 'refused without the cheque' : '');
    await up2('Cancelled cheque');
    const narrow = await p2.evaluate(() => document.documentElement.scrollWidth <= 390 ? 'fits 390px' : 'wider than 390px: ' + document.documentElement.scrollWidth);
    await click2('Submit to HR');
    const thanks = await u2(() => document.querySelector('.jpdone h1')?.textContent || '');
    return `${hello} · ${noPay} · ${saved.split('.')[0]} · ${refused} · ${narrow} · ${thanks}`;
  } finally { await p2.close(); }
}, '^Hello Meera Joshi · no pay or HR fields · Saved · refused without the cheque · fits 390px · Thank you, Meera$');
let next = '';
await step("Onboarding: HR sees the joiner's values (marked Joiner), fills HR's fields and saves (still on the Joining ID)", async () => {
  next = (await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', '/employees/next-code?type=Permanent')).next;
  const bell = (await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', '/notices')).notices.some((n) => n.text.startsWith('Meera Joshi submitted joining details')) ? 'HR told' : 'HR not told';
  await go('/onboarding'); await tab('Pipeline'); const listed = (await row(['Meera Joshi', 'Joiner submitted'])) ? 'pipeline: Joiner submitted' : 'pipeline: no link status';
  await openCand(); await click('Joining details and documents');
  const seen = await until(() => { const f = [...__sc().querySelectorAll('label.field')].find((l) => l.querySelector('span')?.textContent.startsWith('PAN'));
    return f && f.querySelector('input')?.value === 'ABCDE7001F' && f.querySelector('.jpmark') ? 'PAN ABCDE7001F marked Joiner' : ''; });
  const panel = await until(() => document.querySelector('.ov .dlg .jlink .pill')?.textContent || '');
  await fill('Shift', 'G'); await fill('Attendance mode', 'Biometric');
  await click('Save', true); const t = await toast();
  return `${bell} · ${listed} · ${seen} · ${panel} · ${t}`;
}, '^HR told · pipeline: Joiner submitted · PAN ABCDE7001F marked Joiner · Submitted by the joiner · Joining details saved$');
await step('Onboarding: HR records a clear background check with the report', async () => {
  await openCand(); await until(() => /Background check/.test(document.querySelector('.ov .dlg')?.innerText || ''));
  const h = (await page.evaluateHandle(() => [...document.querySelectorAll('.ov .dlg dl')].find((d) => d.textContent.includes('Background check'))?.querySelector('input[type=file]') || null)).asElement();
  if (!h) throw new Error('no background check upload');
  await freshToasts(); await h.uploadFile(DIR + '/bill.pdf'); await wait(300); await settle(); const up = await toast(); await toastGone();
  await fill('Result', 'Clear'); await fill('Note (vendor', 'Checked by the vendor'); await click('Save the check', true); const t = await toast();
  return `${up} · ${t}`; }, '^Attached bill.pdf · Background check: Clear$');
await step('Onboarding: send to Payroll, verified (API), the trail reads SI → OI → JI → Employee ID', async () => {
  await openCand(); await until(() => [...document.querySelectorAll('.ov .dlg button')].some((b) => b.textContent.trim() === 'Send to Payroll'));
  await click('Send to Payroll', true); const t = await toast();
  const x = await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', `/candidates/${si}`);
  const st = await approveChain(x.verify_request_id);
  emp = (await apiAs(await tokenFor('EMP-1002'), 'HR Admin', 'GET', `/candidates/${si}`)).employee_id || '';
  if (emp !== next) throw new Error(`Employee ID ${emp}, expected ${next} (next in the series)`);
  await page.keyboard.press('Escape'); await go('/onboarding'); await tab('Pipeline'); await seg('Done');
  const r = await needRow([emp, 'Meera Joshi', 'Done']); await clickRow([emp, 'Meera Joshi'], 'Open');
  const trail = await until(() => document.querySelector('.ov .dlg .trail')?.innerText.replace(/\s+/g, ' ').trim() || '');
  return `${t} · ${st} · ${r ? 'listed Done' : ''} · ${trail}`; }, '^Sent to Payroll · Approved · listed Done · SI-\\d{4} Shortlisted .* → OI-\\d{4} Offer accepted .* → JI-\\d{4} Joined .* → EMP-\\d+ Employee ID');

await browser.close();
const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length} passed, ${failed.length} failed`);
for (const f of failed) console.log('FAILED:', f.name, '·', f.detail);
process.exitCode = failed.length ? 1 : 0;
import('fs').then((fs) => fs.writeFileSync('forms-results.json', JSON.stringify(results, null, 1)));
