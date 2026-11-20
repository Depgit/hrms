// Joining verification and bulk approval as Payroll (EMP-1011): view a document, correct a detail, download the
// bulk approval file, fill it in (a decision with a correction, a correction only, a decision, a skip), upload, check
// the preview, confirm, tick one more and Approve selected, then check the requests went on to the Payroll Manager.
// Run on a throwaway database (see README.md). It creates four joiners waiting on Payroll first.
//   HRMS_WEB=http://localhost:5198 SHOTS=/some/dir npm run verify
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';

const BASE = process.env.HRMS_WEB || 'http://localhost:5173';
const SHOTS = process.env.SHOTS || path.join(os.tmpdir(), 'hrms-verify-shots');
const DL = fs.mkdtempSync(path.join(os.tmpdir(), 'hrms-verify-dl-'));
fs.mkdirSync(SHOTS, { recursive: true });
const PW = 'Saltstayz#2026';
const fails = [];
const check = (ok, what) => { console.log(`${ok ? 'ok  ' : 'FAIL'} ${what}`); if (!ok) fails.push(what); };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const ymd = (d = new Date()) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

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
const PDF = '%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n';
async function upload(si, key) {
  const fd = new FormData();
  fd.append('ownerKind', 'candidate'); fd.append('ownerId', si); fd.append('docKey', key);
  fd.append('file', new Blob([PDF], { type: 'application/pdf' }), key + '.pdf');
  const res = await fetch(BASE + '/api/files', { method: 'POST', headers: { Authorization: `Bearer ${await token('EMP-1004')}`, 'X-Role': 'HRBP' }, body: fd });
  if (!res.ok) throw new Error(`upload ${key}: ${res.status} ${(await res.json()).error}`);
}
async function approveAll(id) {
  for (let i = 0; i < 10; i++) {
    const r = await api('EMP-1002', 'HR Admin', `/approvals/${id}/approve`, { note: 'Agreed' });
    if (r.status === 'Approved') return;
  }
  throw new Error(id + ' still pending');
}

// A joiner taken from shortlist to a joining verification waiting on Payroll (as onboarding_stages_test.go does).
async function joiner(n, last, place) {
  const first = 'Verify', phone = String(9753100000 + n), today = ymd();
  const email = `verify.${last.toLowerCase()}${Date.now() % 100000}@example.com`;
  const sl = await api('EMP-1004', 'HRBP', '/candidates', {
    data: { first, last, phone, email, dob: '1996-04-12', gender: 'Female', addr: 'Sector 14, Gurugram', ctc: 300000, city: place.city, loc: place.loc,
      dept: 'Front Office', desig: 'Front Office Supervisor', manager: 'EMP-1009' },
    experience: [{ company: 'Hotel Orchid', designation: 'Front Desk Associate', from: '2021-06', to: '2024-03' }],
  });
  await approveAll(sl.request.id);
  await api('EMP-1009', 'Line Manager', `/candidates/${sl.id}/interview`, { date: today, result: 'Selected', feedback: 'Good with guests' });
  await upload(sl.id, 'aadhaar');
  const of = await api('EMP-1004', 'HRBP', '/offers', { candidate: sl.id, data: { first, last, type: 'Permanent', doj: today, dept: 'Front Office',
    desig: 'Front Office Supervisor', grade: 'G3', city: place.city, loc: place.loc, gross: 26000, phone, email, jobRole: 'Front desk', jobSkill: 'Guest service' } });
  await approveAll(of.request.id);
  await api('EMP-1004', 'HRBP', `/offers/${of.id}/release`, {});
  await api('EMP-1004', 'HRBP', `/offers/${of.id}/accept`, {});
  await api('EMP-1004', 'HRBP', `/candidates/${of.id}/joined`, { date: today });
  const pan = `VERFY${String(1000 + n)}K`;
  await api('EMP-1004', 'HRBP', `/offers/${of.id}/joining`, { data: { gender: 'Female', dob: '1996-04-12', aadhaar: `98761234${String(1000 + n)}`, pan, uanAvail: 'No',
    bankName: 'HDFC Bank', acct: `5010012345${String(10 + n)}`, ifsc: 'HDFC0001234', shift: 'G', attMode: 'Biometric', l1: 'EMP-1009' } });
  for (const k of ['pan', 'photo', 'police', 'bankProof']) await upload(sl.id, k);
  const v = await api('EMP-1004', 'HRBP', `/offers/${of.id}/send`, {});
  return { jv: v.id, oi: of.id, si: sl.id, name: `${first} ${last}`, pan };
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
const colName = (n) => { let s = ''; for (; n > 0; n = Math.floor((n - 1) / 26)) s = String.fromCharCode(65 + ((n - 1) % 26)) + s; return s; };
/** Opens the Approvals sheet: get(id, heading), set(id, heading, text), save() → Buffer. */
function openBook(buf) {
  const files = unzip(buf);
  const wb = files.find((f) => f.name === 'xl/workbook.xml').data.toString();
  const rels = files.find((f) => f.name === 'xl/_rels/workbook.xml.rels').data.toString();
  const rid = new RegExp('<sheet[^>]*name="Approvals"[^>]*r:id="([^"]+)"').exec(wb)[1];
  const target = new RegExp(`<Relationship[^>]*Id="${rid}"[^>]*Target="([^"]+)"`).exec(rels)?.[1] ?? new RegExp(`<Relationship[^>]*Target="([^"]+)"[^>]*Id="${rid}"`).exec(rels)[1];
  const sheet = files.find((f) => f.name === 'xl/' + target.replace(/^\/?xl\//, ''));
  const ssf = files.find((f) => f.name === 'xl/sharedStrings.xml');
  const ss = ssf ? [...ssf.data.toString().matchAll(/<si>([\s\S]*?)<\/si>/g)].map((m) => unXml([...m[1].matchAll(/<t[^>]*>([\s\S]*?)<\/t>/g)].map((t) => t[1]).join(''))) : [];
  let xml = sheet.data.toString();
  const cellRe = (ref) => new RegExp(`<c r="${ref}"([^>]*?)(?:/>|>([\\s\\S]*?)</c>)`);
  const value = (ref) => {
    const m = cellRe(ref).exec(xml); if (!m) return '';
    const attrs = m[1], inner = m[2] || '';
    if (/t="s"/.test(attrs)) return ss[+/<v>(\d+)<\/v>/.exec(inner)[1]];
    if (/t="inlineStr"/.test(attrs)) return unXml(/<t[^>]*>([\s\S]*?)<\/t>/.exec(inner)?.[1] ?? '');
    return unXml(/<v>([\s\S]*?)<\/v>/.exec(inner)?.[1] ?? '');
  };
  const heads = {};
  for (let c = 1; c < 80; c++) { const h = value(colName(c) + '1'); if (h) heads[h] = colName(c); }
  const rows = {};
  for (let r = 2; r < 2000; r++) { const id = value('A' + r); if (!id) break; rows[id] = r; }
  return {
    heads, rows,
    get: (id, h) => value(heads[h] + rows[id]),
    set(id, h, text) {
      const ref = heads[h] + rows[id];
      const m = cellRe(ref).exec(xml);
      if (!m) throw new Error('no cell ' + ref);
      const s = /s="(\d+)"/.exec(m[1]);
      xml = xml.replace(m[0], `<c r="${ref}"${s ? ` s="${s[1]}"` : ''} t="inlineStr"><is><t>${xmlText(text)}</t></is></c>`);
    },
    save() { sheet.data = Buffer.from(xml); return zip(files); },
  };
}

// ---------- Run ----------
const locs = await api('EMP-1002', 'HR Admin', '/masters/location?status=Active');
const hub = locs.find((l) => l.name === 'Gurugram – Sector 29 Hub');
const place = { loc: hub.id, city: hub.city_id };
const J = [];
for (const [i, last] of ['Anand', 'Bose', 'Chopra', 'Dutta'].entries()) J.push(await joiner(i + 1, last, place));
console.log('joining verifications:', J.map((j) => j.jv).join(', '));

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
const until = async (fn, what, ms = 8000) => { const t0 = Date.now(); while (Date.now() - t0 < ms) { if (await fn()) return true; await wait(150); } check(false, what + ' (timed out)'); return false; };
const clickText = (sel, t) => page.evaluate((sel, t) => { const b = [...document.querySelectorAll(sel)].find((x) => x.textContent.trim().startsWith(t)); b?.click(); return !!b; }, sel, t);

async function signIn(id, role) {
  const t = await token(id);
  await page.goto(BASE + '/api/health');
  await page.evaluate((t, role) => { localStorage.setItem('hrms.token', t); localStorage.setItem('hrms.role', role); }, t, role);
}
async function openJoin() {
  await page.goto(BASE + '/approvals?tab=join');
  await until(async () => (await text()).includes('Download for bulk approval'), 'Joining approvals shows the bulk bar');
}
const openRow = async (name) => { await clickText('.irow', name); await until(async () => (await text()).includes('Details to verify') && (await text()).includes(name), `${name} opens`); };

await signIn('EMP-1011', 'Payroll');
await openJoin();
check((await page.$$('input[type=checkbox][aria-label^="Select "]')).length >= 4, 'tick boxes on the requests');

// 1. View a document.
await openRow(J[0].name);
await until(async () => (await text()).includes('mandatory uploaded'), 'documents listed');
const docText = await text();
check(docText.includes('PAN card') || docText.includes('PAN'), 'the PAN card is listed');
const newTab = new Promise((res) => browser.once('targetcreated', res));
await clickText('button', 'View');
const tgt = await Promise.race([newTab, wait(6000).then(() => null)]);
await wait(800);
check(!!tgt && /^blob:|about:blank/.test(tgt.url()), `View opens the file in a new tab (${tgt?.url().slice(0, 30)})`);
if (tgt) { const p2 = await tgt.page().catch(() => null); if (p2) { await wait(500); check(p2.url().startsWith('blob:'), 'the new tab shows the file'); await p2.close(); } }
await page.bringToFront();
await shot('1440-verify-detail');

// 2. Edit a detail and save.
await clickText('button', 'Edit details');
await until(async () => !!(await page.$('input[aria-label="PAN"]')), 'Edit details turns the rows into controls');
await page.$eval('input[aria-label="PAN"]', (el) => { el.select(); });
await page.type('input[aria-label="PAN"]', 'BADPAN');
await clickText('button', 'Save');
await until(async () => (await text()).includes('PAN format looks wrong'), 'a wrong PAN is refused');
await page.click('input[aria-label="PAN"]', { clickCount: 3 });
await page.type('input[aria-label="PAN"]', 'VERFY9999Z');
await shot('1440-verify-edit');
await clickText('button', 'Save');
await until(async () => (await text()).includes('Details corrected by Sunita Rao: PAN'), 'the correction is in the history');
const d0 = await api('EMP-1011', 'Payroll', `/offers/${J[0].oi}`);
check(d0.data.pan === 'VERFY9999Z', 'the PAN is saved on the joining details');

// 3. Download the Excel file.
for (const f of fs.readdirSync(DL)) fs.unlinkSync(path.join(DL, f));
await clickText('button', 'Download for bulk approval');
let xlsx = '';
await until(async () => { xlsx = fs.readdirSync(DL).find((f) => f.endsWith('.xlsx')) || ''; return !!xlsx; }, 'the Excel file downloads', 15000);
const book = openBook(fs.readFileSync(path.join(DL, xlsx)));
check(J.every((j) => book.rows[j.jv]), 'every joiner waiting on Payroll is in the file');
for (const h of ['Request ID', 'Request type', 'Employee or joiner', 'Department', 'Location', 'Details', 'Raised on', 'Waiting on', 'PAN', 'Aadhaar number', 'Bank name', 'Documents', 'Decision', 'Comment']) check(!!book.heads[h], `column ${h}`);
check(book.get(J[0].jv, 'PAN') === 'VERFY9999Z', 'the file has the corrected PAN');
check(book.get(J[1].jv, 'Aadhaar number').startsWith('••••'), 'Aadhaar is masked');
check(/^\d+ of \d+ mandatory uploaded/.test(book.get(J[1].jv, 'Documents')), 'Documents says how many are uploaded');

// 4. Fill it in: approve; approve with a correction; a correction only; Return without a comment (skipped).
book.set(J[0].jv, 'Decision', 'Approve');
book.set(J[1].jv, 'Decision', 'Approve');
book.set(J[1].jv, 'Bank name', 'Kotak Mahindra Bank');
book.set(J[2].jv, 'IFSC code', 'KKBK0000123');
book.set(J[3].jv, 'Decision', 'Return');
const filled = path.join(DL, 'filled.xlsx');
fs.writeFileSync(filled, book.save());

// 5. Upload: the preview.
const input = await page.$('input[type=file][accept=".xlsx"]');
await input.uploadFile(filled);
await until(async () => (await text()).includes('Check before you confirm'), 'the preview opens');
const pv = await text();
check(pv.includes('Will approve: 2'), 'preview: will approve 2');
check(pv.includes('Corrections only: 1'), 'preview: corrections only 1');
check(pv.includes('Skipped: 1') && pv.includes('Return needs a comment'), 'preview: Return without a comment is skipped');
check(pv.includes('Bank name: HDFC Bank → Kotak Mahindra Bank'), 'preview lists the correction');
await shot('1440-bulk-preview');
const before = await api('EMP-1011', 'Payroll', `/approvals/${J[0].jv}`);
check(before.step === 0, 'nothing changed before Confirm');

// 6. Confirm.
await clickText('.dlg button, button', 'Confirm:');
await until(async () => (await text()).includes('Approved: 2'), 'the result: 2 approved');
check((await text()).includes('Corrected: 1'), 'the result: 1 corrected');
check((await text()).includes('Next: Payroll Manager'), 'approved requests go on to the Payroll Manager');
await shot('1440-bulk-result');
await clickText('button', 'Close');
await wait(500);

// 7. Tick the corrected one and Approve selected.
await until(async () => !!(await page.$(`input[aria-label="Select ${J[2].name} ${J[2].jv}"]`)), 'the corrected request is still waiting on Payroll');
await page.click(`input[aria-label="Select ${J[2].name} ${J[2].jv}"]`);
await clickText('button', 'Approve selected (1)');
await until(async () => (await text()).includes('Approve 1 request?'), 'Approve selected asks first');
await clickText('button', 'Approve 1');
await until(async () => (await text()).includes('Approved: 1'), 'Approve selected approves it');
await clickText('button', 'Close');

// 8. The Payroll Manager has them now; the skipped one is still with Payroll.
const pm = await api('EMP-1012', 'Payroll Manager', '/approvals?tab=mine&wf=joining,verify');
for (const j of J.slice(0, 3)) check(pm.some((r) => r.id === j.jv), `${j.jv} waits on the Payroll Manager`);
const still = await api('EMP-1011', 'Payroll', `/approvals/${J[3].jv}`);
check(still.status === 'Pending' && still.step === 0, `${J[3].jv} is still with Payroll`);
const o1 = await api('EMP-1011', 'Payroll', `/offers/${J[1].oi}`), o2 = await api('EMP-1011', 'Payroll', `/offers/${J[2].oi}`);
check(o1.data.bankName === 'Kotak Mahindra Bank' && o2.data.ifsc === 'KKBK0000123', 'the corrections from the file are saved');

// 9. Phone width.
await page.setViewport({ width: 390, height: 844 });
await openJoin();
await shot('390-joining-approvals');
await openRow(J[3].name);
await shot('390-verify-detail');
await signIn('EMP-1012', 'Payroll Manager');
await page.setViewport({ width: 1440, height: 900 });
await page.goto(BASE + `/approvals?id=${J[0].jv}`);
await until(async () => (await text()).includes('Details to verify'), 'the Payroll Manager opens a verification');
await shot('1440-payroll-manager-detail');

await browser.close();
for (const e of [...new Set(errors)]) check(false, e);
console.log(`\n${fails.length ? 'FAILED ' + fails.length : 'ALL PASSED'} · screenshots in ${SHOTS}`);
process.exitCode = fails.length ? 1 : 0;
