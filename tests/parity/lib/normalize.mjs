// Normalization: what is allowed to differ between two runs of the same script on the same data.
//
//  - ISO date-times (created_at, decided_at, …) → "<ts>" plus the zone they were written in ("<ts+05:30>", "<tsZ>"),
//    so a value that moved to another zone still shows up. Plain dates (YYYY-MM-DD) are kept: both runs happen on
//    the same day.
//  - uuids that existed in the source database (known-ids) are kept as they are; a uuid created during the run is
//    replaced by a label given at its first appearance: "<new:R.n>", where R identifies the call it first appeared in
//    (a short hash of that call's key) and n counts the new ids first seen in that call. A difference in one call
//    therefore cannot shift the labels of every later call. Inside arrays the labels are given in a canonical
//    element order, so the same elements in another order still get the same labels.
//  - secrets: session tokens, OTPs, device keys → fixed placeholders.
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

export const UUID_RE = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi;
// 2026-11-11T12:20:41.425513+05:30, 2026-11-11T06:50:41Z, 2026-11-11 12:20:41.4+05:30 (Postgres text), 2026-11-11T12:20:41
const TS_RE = /\b(\d{4}-\d{2}-\d{2})(?:T(\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?)|[ ](\d{2}:\d{2}:\d{2}(?:\.\d+)?)(?=[+-]\d{2}|Z))(Z|[+-]\d{2}(?::?\d{2})?)?(?![\d:.])/g;
const TOKEN_RE = /^[A-Za-z0-9_-]{43}$/; // 32 random bytes, base64url (auth.go newSession)
// Date-times formatted for people: "11 Nov 2026 12:37", "11 Nov 2026, 12:37", "11 Nov, 12:37", "2026-11-11 12:37".
// Only those inside the run's own time window are masked (they are "now"); any other one is data and is compared.
const MONTHS = { Jan: 1, Feb: 2, Mar: 3, Apr: 4, May: 5, Jun: 6, Jul: 7, Aug: 8, Sep: 9, Oct: 10, Nov: 11, Dec: 12 };
const HUMAN_RE = /\b(\d{1,2}) (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)(?: (\d{4}))?,? (\d{2}):(\d{2})\b|\b(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2})(?::\d{2})?(?![\d.+:Z-])/g;
// Keys whose string values are random per run. "key" only when it looks random (a device key), not "attendance".
const SECRET_KEYS = new Set(['token', 'devOtp', 'key', 'key_hint']);

export function loadKnownIds(file) {
  if (!file) return new Set();
  return new Set(readFileSync(file, 'utf8').split(/\s+/).filter(Boolean).map((s) => s.toLowerCase()));
}

const shortHash = (s) => createHash('sha1').update(s).digest('hex').slice(0, 6);

export class Normalizer {
  /** window: { date: 'YYYY-MM-DD', from: 'HH:MM', to: 'HH:MM' } — when the run happened, in the business zone. */
  constructor(knownIds, window) {
    this.known = knownIds instanceof Set ? knownIds : new Set(knownIds || []);
    this.window = window;
    this.labels = new Map(); // new uuid → label
    this.scope = 'x';
    this.n = 0;
  }

  /** begin starts a new call: labels given from now on carry this call's key. */
  begin(callKey) {
    this.scope = shortHash(callKey);
    this.n = 0;
  }

  isNew(uuid) { return !this.known.has(uuid.toLowerCase()); }

  label(uuid) {
    const u = uuid.toLowerCase();
    if (this.known.has(u)) return uuid;
    let l = this.labels.get(u);
    if (!l) {
      l = `<new:${this.scope}.${++this.n}>`;
      this.labels.set(u, l);
    }
    return l;
  }

  /** str normalizes one string: timestamps, new uuids, tokens. */
  str(s, key) {
    if (key && SECRET_KEYS.has(key) && (key !== 'key' || /^[A-Za-z0-9_-]{24,}$/.test(s))) {
      return `<${key}>`;
    }
    if (TOKEN_RE.test(s) && /[A-Z]/.test(s) && /[a-z]/.test(s) && /\d/.test(s)) return '<token>';
    let out = s.replace(TS_RE, (_m, _d, _t1, _t2, zone) => `<ts${zone ?? ''}>`);
    if (this.window) out = out.replace(HUMAN_RE, (m, d, mon, y, hh, mm, Y, M, D, h2, m2) => {
      const date = Y ? `${Y}-${M}-${D}` : `${y || this.window.date.slice(0, 4)}-${String(MONTHS[mon]).padStart(2, '0')}-${String(d).padStart(2, '0')}`;
      const hm = Y ? `${h2}:${m2}` : `${hh}:${mm}`;
      return date === this.window.date && hm >= this.window.from && hm <= this.window.to ? '<now>' : m;
    });
    out = out.replace(UUID_RE, (u) => this.label(u));
    return out;
  }

  /** assign gives labels to the new uuids in v in a canonical order (arrays sorted by their masked content). */
  assign(v) {
    if (typeof v === 'string') {
      for (const m of v.matchAll(UUID_RE)) this.label(m[0]);
    } else if (Array.isArray(v)) {
      const keyed = v.map((x, i) => [this.mask(x), i]);
      keyed.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : a[1] - b[1]));
      for (const [, i] of keyed) this.assign(v[i]);
    } else if (v && typeof v === 'object') {
      for (const k of Object.keys(v).sort()) {
        for (const m of k.matchAll(UUID_RE)) this.label(m[0]);
        this.assign(v[k]);
      }
    }
  }

  /** mask is a canonical string of v with every new uuid blanked (labels not yet given are unknown here). */
  mask(v) {
    return JSON.stringify(v, (k, x) => (typeof x === 'string' ? x.replace(TS_RE, '<ts>').replace(UUID_RE, (u) => (this.isNew(u) ? '<u>' : u)) : x));
  }

  /** value normalizes any JSON value (labels are given first, in canonical order). */
  value(v) {
    this.assign(v);
    return this.apply(v);
  }

  apply(v, key) {
    if (typeof v === 'string') return this.str(v, key);
    if (Array.isArray(v)) return v.map((x) => this.apply(x));
    if (v && typeof v === 'object') {
      const out = {};
      for (const k of Object.keys(v)) out[this.str(k)] = this.apply(v[k], k);
      return out;
    }
    return v;
  }
}

/** keyOf is a call's comparable identity: phase, role or step, method and the normalized path. */
export function callKey(rec, norm) {
  const who = rec.step ? rec.step : rec.role || '-';
  return `${rec.phase} | ${who} | ${rec.method} ${norm.str(rec.path)}`;
}

/**
 * RecordNormalizer turns raw capture records, fed in capture order, into comparable entries:
 * { key, seq, phase, role, step, method, path, status, ct, disposition, kind, req, body }.
 * Repeated keys get "#2", "#3", …. capture.mjs runs one live (to order new ids the same way in every run) and
 * diff.mjs runs one per capture.
 */
export class RecordNormalizer {
  constructor(knownIds, window) {
    this.norm = new Normalizer(knownIds, window);
    this.seen = new Map();
  }
  add(r) {
    const norm = this.norm;
    // the path and request first: they may carry ids created by earlier calls
    norm.assign(r.path);
    if (r.req !== undefined) norm.assign(r.req);
    let key = callKey(r, norm);
    const n = (this.seen.get(key) || 0) + 1;
    this.seen.set(key, n);
    if (n > 1) key += ` #${n}`;
    norm.begin(key);
    const req = r.req === undefined ? undefined : norm.value(r.req);
    const body = norm.value(r.body);
    return {
      key, seq: r.seq, phase: r.phase, role: r.role, step: r.step, method: r.method, path: norm.str(r.path), status: r.status,
      ct: (r.ct || '').toLowerCase().replace(/\s+/g, ''), disposition: r.disposition === undefined ? undefined : norm.str(r.disposition), kind: r.kind, req, body,
    };
  }
}

/** runWindow is the normalizer window of a capture (meta.json): its start and end, a minute either side. */
export function runWindow(meta) {
  if (!meta?.started || !meta?.finished) return undefined;
  const zone = meta.zone || 'Asia/Kolkata';
  const f = (iso, deltaMin) => {
    const parts = Object.fromEntries(new Intl.DateTimeFormat('en-GB', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
      .formatToParts(new Date(Date.parse(iso) + deltaMin * 60000)).map((p) => [p.type, p.value]));
    return { date: `${parts.year}-${parts.month}-${parts.day}`, hm: `${parts.hour}:${parts.minute}` };
  };
  const a = f(meta.started, -1), b = f(meta.finished, 1);
  return { date: a.date, from: a.hm, to: b.date === a.date ? b.hm : '23:59' };
}

export function normalizeRecords(records, knownIds, window) {
  const rn = new RecordNormalizer(knownIds, window);
  return { entries: records.map((r) => rn.add(r)), norm: rn.norm };
}
