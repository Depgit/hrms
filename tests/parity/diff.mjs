#!/usr/bin/env node
// Compare two parity captures call by call:
//
//   node diff.mjs <golden-dir> <actual-dir> [--details 12] [--only <regex>] [--json <file>] [--quiet]
//
// Each call (phase | role or step | METHOD path, normalized) is one of:
//   identical         same status and the same normalized body
//   order             same content, but an array is in another order (the API promises the same order: a finding)
//   different         same status, different content: a path-level diff follows
//                     (missing = in the golden capture only, extra = in the actual one only, null-vs-missing,
//                      changed = another value, length = arrays of different length)
//   status            the HTTP status differs (the body diff follows too)
//   only-golden / only-actual   the call was made in one run only (the write script took another branch)
// Exit code 0 when every call is identical, 1 otherwise, 2 on bad input.
import { readFileSync, existsSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { RecordNormalizer, loadKnownIds, runWindow } from './lib/normalize.mjs';
import { RULES } from './rules.mjs';

const argv = process.argv.slice(2);
const FLAGS = new Set(['quiet']); // options without a value
const pos = [];
const opts = {};
for (let i = 0; i < argv.length; i++) {
  if (!argv[i].startsWith('--')) { pos.push(argv[i]); continue; }
  const k = argv[i].slice(2);
  opts[k] = FLAGS.has(k) ? true : argv[++i];
}
const opt = (k, d) => (k in opts ? opts[k] : d);
if (pos.length !== 2) {
  console.error('usage: node diff.mjs <golden-dir> <actual-dir> [--details N] [--only <regex>] [--json <file>] [--quiet]');
  process.exit(2);
}
const [A_DIR, B_DIR] = pos;
const DETAILS = Number(opt('details', 12));
const ONLY = opt('only') ? new RegExp(opt('only')) : null;
const QUIET = opt('quiet', false) === true;

function load(dir) {
  const f = join(dir, 'records.jsonl');
  if (!existsSync(f)) { console.error(`no records.jsonl in ${dir}`); process.exit(2); }
  const known = loadKnownIds(existsSync(join(dir, 'known-ids.txt')) ? join(dir, 'known-ids.txt') : opt('known-ids'));
  const meta = existsSync(join(dir, 'meta.json')) ? JSON.parse(readFileSync(join(dir, 'meta.json'), 'utf8')) : {};
  const rn = new RecordNormalizer(known, runWindow(meta));
  const entries = [];
  for (const line of readFileSync(f, 'utf8').split('\n')) if (line) entries.push(rn.add(JSON.parse(line)));
  return { entries, meta };
}

// ---- rules ----
const ruleKey = (p) => p.replace(/\[\d+\]/g, '[]');
function applyRules(entry) {
  const rules = RULES.filter((r) => r.key.test(entry.key));
  if (!rules.length) return entry.body;
  const walk = (v, path) => {
    const rp = ruleKey(path);
    for (const r of rules) if (r.path.test(rp)) {
      if (r.action === 'ignore') return '<volatile>';
      if (r.action === 'unordered' && Array.isArray(v)) v = sortCanon(v);
    }
    if (Array.isArray(v)) return v.map((x, i) => walk(x, `${path}[${i}]`));
    if (v && typeof v === 'object') return Object.fromEntries(Object.keys(v).map((k) => [k, walk(v[k], `${path}.${k}`)]));
    return v;
  };
  return walk(entry.body, '$');
}

// ---- comparison ----
const canon = (v) => JSON.stringify(v, (k, x) => (x && typeof x === 'object' && !Array.isArray(x) ? Object.fromEntries(Object.keys(x).sort().map((key) => [key, x[key]])) : x));
function sortCanon(arr) { return arr.map((x) => [canon(deepSort(x)), x]).sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0)).map(([, x]) => x); }
function deepSort(v) {
  if (Array.isArray(v)) return sortCanon(v.map(deepSort));
  if (v && typeof v === 'object') return Object.fromEntries(Object.keys(v).sort().map((k) => [k, deepSort(v[k])]));
  return v;
}
const brief = (v) => { const s = JSON.stringify(v); return s === undefined ? '(missing)' : s.length > 90 ? s.slice(0, 87) + '…' : s; };
const isObj = (v) => v && typeof v === 'object' && !Array.isArray(v);

function diffValues(a, b, path, out) {
  if (out.length > 200) return;
  if (canon(a) === canon(b)) return;
  if (isObj(a) && isObj(b)) {
    for (const k of [...new Set([...Object.keys(a), ...Object.keys(b)])].sort()) {
      const p = `${path}.${k}`;
      const inA = k in a, inB = k in b;
      if (inA && !inB) out.push(a[k] === null ? { kind: 'null-vs-missing', path: p, a: 'null', b: '(missing)' } : { kind: 'missing', path: p, a: brief(a[k]) });
      else if (!inA && inB) out.push(b[k] === null ? { kind: 'null-vs-missing', path: p, a: '(missing)', b: 'null' } : { kind: 'extra', path: p, b: brief(b[k]) });
      else diffValues(a[k], b[k], p, out);
    }
  } else if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) out.push({ kind: 'length', path, a: a.length, b: b.length });
    const n = Math.min(a.length, b.length);
    for (let i = 0; i < n; i++) diffValues(a[i], b[i], `${path}[${i}]`, out);
    for (let i = n; i < a.length; i++) out.push({ kind: 'missing', path: `${path}[${i}]`, a: brief(a[i]) });
    for (let i = n; i < b.length; i++) out.push({ kind: 'extra', path: `${path}[${i}]`, b: brief(b[i]) });
  } else {
    out.push({ kind: 'changed', path, a: brief(a), b: brief(b) });
  }
}

function compare(ea, eb) {
  if (!ea) return { cat: 'only-actual' };
  if (!eb) return { cat: 'only-golden' };
  const a = applyRules(ea), b = applyRules(eb);
  const meta = [];
  if (ea.ct !== eb.ct) meta.push({ kind: 'changed', path: '(content-type)', a: ea.ct, b: eb.ct });
  if ((ea.disposition ?? null) !== (eb.disposition ?? null)) meta.push({ kind: 'changed', path: '(content-disposition)', a: brief(ea.disposition), b: brief(eb.disposition) });
  const same = canon(a) === canon(b);
  if (ea.status !== eb.status) {
    const d = [...meta];
    if (!same) diffValues(a, b, '$', d);
    return { cat: 'status', status: [ea.status, eb.status], diffs: d };
  }
  if (same && !meta.length) return { cat: 'identical' };
  if (!meta.length && canon(deepSort(a)) === canon(deepSort(b))) {
    // where the order differs: the first arrays whose element order is not the same
    const d = [];
    const walk = (x, y, path) => {
      if (d.length >= DETAILS || canon(x) === canon(y)) return;
      if (Array.isArray(x) && Array.isArray(y)) {
        // elements compared with their own nested arrays sorted: if the sequences differ, this array is reordered
        const ia = x.map((v) => canon(deepSort(v)));
        const ib = y.map((v) => canon(deepSort(v)));
        const at = ia.findIndex((v, i) => v !== ib[i]);
        if (at >= 0) { d.push({ kind: 'order', path, a: `${x.length} items`, b: `first out of place at [${at}]` }); return; }
        x.forEach((v, i) => walk(v, y[i], `${path}[${i}]`));
      } else if (isObj(x) && isObj(y)) for (const k of Object.keys(x)) walk(x[k], y[k], `${path}.${k}`);
    };
    walk(a, b, '$');
    return { cat: 'order', diffs: d };
  }
  const d = [...meta];
  diffValues(a, b, '$', d);
  return { cat: 'different', diffs: d };
}

// ---- run ----
const A = load(A_DIR), B = load(B_DIR);
const byKey = (l) => new Map(l.map((e) => [e.key, e]));
const ma = byKey(A.entries), mb = byKey(B.entries);
const keys = [...A.entries.map((e) => e.key), ...B.entries.filter((e) => !ma.has(e.key)).map((e) => e.key)].filter((k) => !ONLY || ONLY.test(k));

const CATS = ['identical', 'order', 'different', 'status', 'only-golden', 'only-actual'];
const total = Object.fromEntries(CATS.map((c) => [c, 0]));
const byPhase = {};
const byGroup = {};
const report = [];
for (const k of keys) {
  const ea = ma.get(k), eb = mb.get(k);
  const r = compare(ea, eb);
  total[r.cat]++;
  const e = ea || eb;
  const ph = e.phase;
  byPhase[ph] ??= Object.fromEntries(CATS.map((c) => [c, 0]));
  byPhase[ph][r.cat]++;
  const g = `${ph} | ${e.step ? e.step.split('.')[0] : e.role}`;
  byGroup[g] ??= Object.fromEntries(CATS.map((c) => [c, 0]));
  byGroup[g][r.cat]++;
  if (r.cat !== 'identical') report.push({ key: k, ...r });
}

if (!QUIET) {
  for (const r of report) {
    const head = r.cat === 'status' ? `[status ${r.status[0]} → ${r.status[1]}]` : `[${r.cat}]`;
    console.log(`${head} ${r.key}`);
    for (const d of (r.diffs || []).slice(0, DETAILS)) {
      if (d.kind === 'missing') console.log(`    missing  ${d.path}: ${d.a}`);
      else if (d.kind === 'extra') console.log(`    extra    ${d.path}: ${d.b}`);
      else console.log(`    ${d.kind.padEnd(8)} ${d.path}: ${d.a} → ${d.b}`);
    }
    if ((r.diffs || []).length > DETAILS) console.log(`    … ${r.diffs.length - DETAILS} more`);
  }
  console.log('');
}
const line = (o) => CATS.filter((c) => o[c]).map((c) => `${c} ${o[c]}`).join(', ');
console.log(`golden ${A_DIR} (${A.entries.length} calls${A.meta.today ? ', ' + A.meta.today : ''})`);
console.log(`actual ${B_DIR} (${B.entries.length} calls${B.meta.today ? ', ' + B.meta.today : ''})`);
if (A.meta.today && B.meta.today && A.meta.today !== B.meta.today) console.log(`WARNING: the captures were made on different days (${A.meta.today}, ${B.meta.today}); dates will differ`);
console.log('by phase:');
for (const [ph, o] of Object.entries(byPhase)) console.log(`  ${ph.padEnd(11)} ${line(o)}`);
const bad = Object.entries(byGroup).filter(([, o]) => CATS.slice(1).some((c) => o[c]));
if (bad.length) {
  console.log('groups with differences:');
  for (const [g, o] of bad) console.log(`  ${g.padEnd(40)} ${line(o)}`);
}
console.log(`TOTAL ${keys.length} calls: ${line(total)}`);
if (opt('json')) writeFileSync(opt('json'), JSON.stringify({ golden: A_DIR, actual: B_DIR, total, byPhase, byGroup, report }, null, 1));
process.exit(keys.length && total.identical === keys.length ? 0 : 1);
