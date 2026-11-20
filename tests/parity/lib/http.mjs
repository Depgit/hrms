// HTTP client for the parity harness: the same headers the web app sends (Bearer token + X-Role), JSON or
// multipart bodies, and a decoded body for every response (JSON, Excel cells, text, or a digest of anything else).
import { createHash } from 'node:crypto';
import { readXlsx } from './xlsx.mjs';

export const PASSWORD = 'Saltstayz#2026'; // what tests/e2e and the Go tests set through the OTP flow
export const STARTING_PASSWORD = 'pass1234'; // seed.StartingLogins (EMP-1002, EMP-1020)

export function decodeBody(buf, contentType) {
  const ct = (contentType || '').toLowerCase();
  if (buf.length === 0) return { kind: 'empty', body: null };
  if (ct.includes('json')) {
    try { return { kind: 'json', body: JSON.parse(buf.toString('utf8')) }; } catch { return { kind: 'text', body: buf.toString('utf8') }; }
  }
  if (ct.includes('spreadsheet') || (buf[0] === 0x50 && buf[1] === 0x4b && !ct.startsWith('text/'))) {
    try { return { kind: 'xlsx', body: readXlsx(buf) }; } catch (e) { /* fall through to a digest */ }
  }
  if (ct.startsWith('text/') || ct.includes('csv')) return { kind: 'text', body: buf.toString('utf8') };
  return { kind: 'binary', body: { bytes: buf.length, sha256: createHash('sha256').update(buf).digest('hex') } };
}

export class Api {
  constructor(base) {
    this.base = base.replace(/\/+$/, '');
    this.tokens = new Map(); // employee id → session token
  }

  /** call sends one request. opts: emp (whose token), role (X-Role), body (JSON), form ({fields, name, content, type}), headers. */
  async call(method, path, opts = {}) {
    const headers = { ...(opts.headers || {}) };
    if (opts.emp) headers.Authorization = 'Bearer ' + (this.tokens.get(opts.emp) || '');
    if (opts.role !== undefined) headers['X-Role'] = opts.role;
    let body;
    if (opts.form) {
      const fd = new FormData();
      for (const [k, v] of Object.entries(opts.form.fields || {})) fd.append(k, v);
      fd.append('file', new Blob([opts.form.content], { type: opts.form.type || 'application/octet-stream' }), opts.form.name);
      body = fd;
    } else if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(opts.body);
    }
    let res, buf;
    for (let attempt = 0; ; attempt++) {
      try {
        res = await fetch(this.base + path, { method, headers, body, redirect: 'manual' });
        buf = Buffer.from(await res.arrayBuffer());
        break;
      } catch (e) {
        if (attempt >= 2 || method !== 'GET') throw e; // only reads are retried on a dropped connection
        await new Promise((r) => setTimeout(r, 300));
      }
    }
    const contentType = res.headers.get('content-type') || '';
    const { kind, body: decoded } = decodeBody(buf, contentType);
    return { status: res.status, contentType, disposition: res.headers.get('content-disposition') || undefined, location: res.headers.get('location') || undefined, kind, body: decoded };
  }
}
