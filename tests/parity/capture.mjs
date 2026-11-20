#!/usr/bin/env node
// Parity capture: signs in one user per role, calls every GET route for every role, runs the write script
// (writes.mjs) over HTTP, and calls every GET route again. Every request and response is recorded, in order, in
// <out>/records.jsonl; <out>/meta.json describes the run. Compare two captures with diff.mjs.
//
//   node capture.mjs --base http://localhost:8092 --out <dir> --known-ids <file> [--api-dir <backend/internal/api>]
//
// The API must run on a fresh copy of the parity source database (see README.md), with OTP_DEV_ECHO=true.
import { createWriteStream, mkdirSync, readFileSync, readdirSync, writeFileSync, copyFileSync, existsSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Api, PASSWORD, STARTING_PASSWORD } from './lib/http.mjs';
import { RecordNormalizer, loadKnownIds } from './lib/normalize.mjs';
import { ROUTES, SweepCtx } from './routes.mjs';
import { runWrites } from './writes.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const args = Object.fromEntries(process.argv.slice(2).reduce((acc, a, i, l) => (a.startsWith('--') ? [...acc, [a.slice(2), l[i + 1]?.startsWith('--') || l[i + 1] === undefined ? 'true' : l[i + 1]]] : acc), []));
const BASE = args.base || 'http://localhost:8092';
const OUT = args.out;
const API_DIR = resolve(args['api-dir'] || join(here, '../../backend/internal/api'));
const ZONE = args.zone || process.env.APP_TZ || 'Asia/Kolkata';
if (!OUT) {
  console.error('usage: node capture.mjs --base http://localhost:8092 --out <dir> --known-ids <file> [--api-dir <dir>] [--skip-writes]');
  process.exit(2);
}

// The people the sample organisation signs in with (tests/e2e/roles.mjs): one per role. HR Admin goes first: its
// responses give the ids every other role's sweep uses.
const PEOPLE = [['EMP-1002', 'HR Admin'], ['EMP-1020', 'Employee'], ['EMP-1009', 'Line Manager'], ['EMP-1008', 'Property Manager'], ['EMP-1007', 'Cluster Manager'],
  ['EMP-1006', 'City Head'], ['EMP-1005', 'Operations Head'], ['EMP-1004', 'HRBP'], ['EMP-1003', 'HRBP Corporate'], ['EMP-1011', 'Payroll'], ['EMP-1012', 'Payroll Manager'],
  ['EMP-1013', 'Finance Head'], ['EMP-1010', 'Function Head'], ['EMP-1001', 'CHRO']];
const STARTING = new Set(['EMP-1002', 'EMP-1020']); // seed.StartingLogins
const EXTRA_ACTORS = ['EMP-1014']; // signed in for the write script only (a contractual employee)

const today = new Intl.DateTimeFormat('en-CA', { timeZone: ZONE, year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date());

mkdirSync(OUT, { recursive: true });
const out = createWriteStream(join(OUT, 'records.jsonl'));
const api = new Api(BASE);
const known = loadKnownIds(args['known-ids']);
const live = new RecordNormalizer(known);
let seq = 0;
const counts = {}; // phase → role/step group → n
const started = new Date();

function sanitizeReq(path, opts) {
  if (opts.form) {
    return { form: opts.form.fields || {}, file: opts.form.name, bytes: Buffer.byteLength(opts.form.content) };
  }
  if (opts.body === undefined) return undefined;
  if (path.startsWith('/api/auth/') && opts.body && typeof opts.body === 'object' && 'code' in opts.body) return { ...opts.body, code: '<otp>' };
  return opts.body;
}

/** rec sends one request and records it. meta: { phase, role, step } — role for sweeps, step for scripted calls. */
async function rec(meta, method, path, opts = {}) {
  const res = await api.call(method, path, opts);
  const r = {
    seq: ++seq, phase: meta.phase, role: meta.role, step: meta.step, emp: opts.emp, method, path, req: sanitizeReq(path, opts),
    status: res.status, ct: res.contentType, disposition: res.disposition, kind: res.kind, body: res.body,
  };
  out.write(JSON.stringify(r) + '\n');
  live.add(r);
  const group = meta.step ? meta.step.split('.')[0] : meta.role || '-';
  counts[meta.phase] ??= {};
  counts[meta.phase][group] = (counts[meta.phase][group] || 0) + 1;
  if (res.status >= 500) console.warn(`  ${res.status} ${method} ${path} (${meta.role || meta.step})`);
  return res;
}

// ---- the route table: every GET pattern must have an entry in routes.mjs ----
function routeTable(dir) {
  const pats = new Set();
  for (const f of readdirSync(dir)) {
    if (!f.endsWith('.go') || f.endsWith('_test.go')) continue;
    for (const m of readFileSync(join(dir, f), 'utf8').matchAll(/HandleFunc\("GET (\/[^"]+)"/g)) pats.add('GET ' + m[1]);
  }
  return pats;
}
const table = existsSync(API_DIR) ? routeTable(API_DIR) : new Set();
const covered = new Set(ROUTES.map(([p]) => p));
// /api/mdm/* (the master-data integration added with the MongoDB move) has no Postgres counterpart to compare
// with; its own tests live in backend/internal/mdm and mdm_lock_test.go. /api/join/* (the new joiner's public pages,
// added 11 Nov 2026) needs a link token made during a run, never signs in, and has no Postgres counterpart either;
// it is tested in backend/internal/api/joining_link_test.go and tests/e2e/forms.mjs.
const uncovered = [...table].filter((p) => !covered.has(p) && !p.startsWith('GET /api/mdm/') && !p.startsWith('GET /api/join/')).sort();
const unknown = [...covered].filter((p) => table.size && !table.has(p)).sort();
if (!table.size) console.warn(`route table not found in ${API_DIR}; coverage not checked`);
if (uncovered.length && args['allow-uncovered'] !== 'true') {
  console.error(`GET routes with no entry in routes.mjs:\n  ${uncovered.join('\n  ')}`);
  process.exit(2);
}

// ---- wait for the API ----
for (let i = 0; ; i++) {
  try { const r = await fetch(BASE + '/api/health'); if (r.ok) break; } catch {}
  if (i > 60) { console.error('API not reachable at ' + BASE); process.exit(2); }
  await new Promise((r) => setTimeout(r, 500));
}

// ---- sign in: one person per role ----
async function signIn(emp) {
  const m = { phase: 'signin', step: `signin.${emp}` };
  if (STARTING.has(emp)) {
    const r = await rec(m, 'POST', '/api/auth/login', { body: { employeeId: emp, password: STARTING_PASSWORD } });
    if (r.body?.token) { api.tokens.set(emp, r.body.token); return true; }
  }
  const o = await rec(m, 'POST', '/api/auth/otp', { body: { employeeId: emp, purpose: 'first' } });
  const v = await rec(m, 'POST', '/api/auth/otp/verify', { body: { employeeId: emp, code: o.body?.devOtp || '' } });
  const p = await rec(m, 'POST', '/api/auth/password', { body: { employeeId: emp, password: PASSWORD, verifyToken: v.body?.verifyToken } });
  if (p.body?.token) { api.tokens.set(emp, p.body.token); return true; }
  console.warn(`  could not sign in ${emp}: ${JSON.stringify(p.body)}`);
  return false;
}

console.log(`capture ${BASE} → ${OUT} (today ${today}, ${table.size} GET routes in the table)`);
const people = [...PEOPLE];
await signIn('EMP-1002');
// Roles beyond the sample list: the first person mapped to it. A role nobody holds in the sample organisation
// (Finance, MIS Executive) is given, through Role mapping, to one of the corporate staff, so every role is swept.
const roles = (await rec({ phase: 'signin', step: 'signin.roles' }, 'GET', '/api/roles', { emp: 'EMP-1002', role: 'HR Admin' })).body;
const rmap = (await rec({ phase: 'signin', step: 'signin.roles' }, 'GET', '/api/role-map', { emp: 'EMP-1002', role: 'HR Admin' })).body;
const spare = ['EMP-1047', 'EMP-1048', 'EMP-1050', 'EMP-1051', 'EMP-1052', 'EMP-1053'];
for (const r of (Array.isArray(roles) ? roles : []).slice().sort((a, b) => (a.name < b.name ? -1 : 1))) {
  if (!r.active || people.some(([, role]) => role === r.name)) continue;
  let m = (Array.isArray(rmap) ? rmap : []).filter((x) => x.role_name === r.name && x.active).map((x) => x.employee_id).sort()[0];
  if (!m && spare.length) {
    const emp = spare.shift();
    const res = await rec({ phase: 'signin', step: 'signin.map' }, 'POST', '/api/role-map', { emp: 'EMP-1002', role: 'HR Admin', body: { employeeId: emp, role: r.name, access: 'all' } });
    if (res.status === 200) m = emp;
  }
  if (m) people.push([m, r.name]);
  else console.warn(`  role ${r.name} has nobody mapped; not swept`);
}
for (const emp of [...new Set([...people.map(([e]) => e), ...EXTRA_ACTORS])]) {
  if (!api.tokens.has(emp)) await signIn(emp);
}
// Each person's own view of who they are, in the role they are swept as.
const signedIn = people.filter(([e]) => api.tokens.has(e));
for (const [emp, role] of signedIn) {
  const me = await rec({ phase: 'signin', step: 'signin.me' }, 'GET', '/api/me', { emp, role });
  if (me.body?.ctx?.role !== role) console.warn(`  ${emp} is not ${role} (got ${me.body?.ctx?.role})`);
}

// ---- the GET sweep ----
async function sweep(phase, base, extra) {
  const ctx = new SweepCtx({ norm: live.norm, today, base });
  if (extra) for (const id of extra.files) ctx.extra.files.add(id);
  const [first, ...rest] = signedIn;
  const plan = [];
  const problems = [];
  for (const [pattern, fn] of ROUTES) {
    let urls = [];
    try { urls = fn(ctx); } catch (e) { problems.push(`${pattern}: ${e.message}`); }
    if (!urls.length) problems.push(`${pattern}: no URL (no ids to call it with)`);
    for (const u of urls) {
      const { url, auth = true } = typeof u === 'string' ? { url: u } : u;
      plan.push({ pattern, url, auth });
      const res = auth
        ? await rec({ phase, role: first[1] }, 'GET', url, { emp: first[0], role: first[1] })
        : await rec({ phase, role: '(no sign-in)' }, 'GET', url, {});
      if (auth && !ctx.bodies.has(url)) ctx.bodies.set(url, res.body);
    }
  }
  for (const [emp, role] of rest) {
    for (const p of plan) if (p.auth) await rec({ phase, role }, 'GET', p.url, { emp, role });
    process.stdout.write(`  ${phase}: ${role} done\n`);
  }
  return { ctx, plan, problems };
}

const before = await sweep('get-before');
console.log(`get-before: ${before.plan.length} URLs per role`);
const base = { employees: new Set(before.ctx.empIds) };

const writeNotes = [];
const extra = { files: new Set() };
if (args['skip-writes'] !== 'true') {
  await runWrites({
    api, today, rec: (step, method, path, opts) => rec({ phase: 'write', step }, method, path, opts), files: extra.files, log: (s) => { writeNotes.push(s); console.log('  ' + s); },
  });
  console.log(`writes: ${Object.values(counts.write || {}).reduce((a, b) => a + b, 0)} calls`);
}

const after = await sweep('get-after', base, extra);
console.log(`get-after: ${after.plan.length} URLs per role`);
await new Promise((r) => out.end(r));

if (args['known-ids']) copyFileSync(args['known-ids'], join(OUT, 'known-ids.txt'));
const meta = {
  base: BASE, today, zone: ZONE, started: started.toISOString(), finished: new Date().toISOString(), records: seq,
  people: signedIn.map(([emp, role]) => ({ emp, role })), routeTable: table.size, uncovered, unknownPatterns: unknown,
  urlsPerRole: { 'get-before': before.plan.length, 'get-after': after.plan.length },
  sweepProblems: { 'get-before': before.problems, 'get-after': after.problems }, counts, writeNotes,
};
writeFileSync(join(OUT, 'meta.json'), JSON.stringify(meta, null, 2));
console.log(`${seq} calls recorded in ${((Date.now() - started) / 1000).toFixed(0)}s`);
for (const [phase, g] of Object.entries(counts)) console.log(`  ${phase}: ${Object.values(g).reduce((a, b) => a + b, 0)} (${Object.entries(g).map(([k, v]) => `${k} ${v}`).join(', ')})`);
if (before.problems.length || after.problems.length) console.log(`sweep notes:\n  ${[...before.problems, ...after.problems].join('\n  ')}`);
