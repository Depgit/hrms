// Thin API client. The session token and the chosen role view travel with every request;
// the server enforces rights and data scope, the UI only hides what the role cannot use.

export class ApiError extends Error {
  status: number;
  details?: unknown;
  constructor(status: number, message: string, details?: unknown) {
    super(message);
    this.status = status;
    this.details = details;
  }
}

const TOKEN = 'hrms.token';
const ROLE = 'hrms.role';
export const session = {
  get token() { return localStorage.getItem(TOKEN) || ''; },
  set token(v: string) { v ? localStorage.setItem(TOKEN, v) : localStorage.removeItem(TOKEN); },
  get role() { return localStorage.getItem(ROLE) || 'Employee'; },
  set role(v: string) { localStorage.setItem(ROLE, v); },
};

let onUnauthorized = () => {};
export const setUnauthorizedHandler = (fn: () => void) => { onUnauthorized = fn; };

// Requests in flight, so the page can show that something is loading (BusyBar).
let inFlight = 0;
const busySubs = new Set<(n: number) => void>();
export const onBusy = (fn: (n: number) => void) => { busySubs.add(fn); return () => { busySubs.delete(fn); }; };
const busy = (d: number) => { inFlight += d; busySubs.forEach((f) => f(inFlight)); };
async function tracked(url: string, init: RequestInit) {
  busy(1);
  try {
    const res = await fetch(url, init);
    return { res, data: await res.json().catch(() => ({})) };
  } finally { busy(-1); }
}

export async function api<T = any>(path: string, opts: { method?: string; body?: unknown } = {}): Promise<T> {
  const { res, data } = await tracked('/api' + path, {
    method: opts.method || (opts.body !== undefined ? 'POST' : 'GET'),
    headers: {
      'Content-Type': 'application/json',
      ...(session.token ? { Authorization: `Bearer ${session.token}` } : {}),
      'X-Role': session.role,
    },
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });
  if (!res.ok) {
    if (res.status === 401 && session.token) onUnauthorized();
    // A joiner or transfer into a cluster whose punch machines are full (backend machines.go): ask, then go ahead.
    if (res.status === 409 && (data.details as any)?.kind === 'cluster_full' && !path.includes('machineOverride=1') &&
      window.confirm(`${data.error}\n\nGo ahead anyway? This is recorded in the audit log.`)) {
      return api<T>(path + (path.includes('?') ? '&' : '?') + 'machineOverride=1', opts);
    }
    throw new ApiError(res.status, data.error || 'Something went wrong', data.details);
  }
  return data as T;
}

export const inr = (n: unknown) => (typeof n === 'number' ? '₹' + Math.round(n).toLocaleString('en-IN') : n == null ? '—' : String(n));
export const fmtD = (s?: string | null) => {
  if (!s) return '—';
  const d = /^\d{4}-\d{2}-\d{2}$/.test(s) ? new Date(+s.slice(0, 4), +s.slice(5, 7) - 1, +s.slice(8, 10)) : new Date(s);
  return isNaN(d.getTime()) ? s : d.toLocaleDateString('en-IN', { day: '2-digit', month: 'short', year: 'numeric' });
};

/** POST a file as multipart form data (documents, bulk uploads). */
export async function postForm<T = any>(path: string, fields: Record<string, string>, file: File): Promise<T> {
  const fd = new FormData();
  Object.entries(fields).forEach(([k, v]) => fd.append(k, v));
  fd.append('file', file);
  const { res, data } = await tracked('/api' + path, { method: 'POST', headers: { Authorization: `Bearer ${session.token}`, 'X-Role': session.role }, body: fd });
  if (!res.ok) throw new ApiError(res.status, data.error || 'Upload failed', data.details);
  return data as T;
}

export const uploadDoc = (ownerKind: string, ownerId: string, docKey: string, file: File) => postForm('/files', { ownerKind, ownerId, docKey }, file);

/** Open a stored document in a new tab (the token travels in a header, so a plain link cannot be used). */
export async function openDoc(id: string) {
  const res = await fetch(`/api/files/${id}`, { headers: { Authorization: `Bearer ${session.token}`, 'X-Role': session.role } });
  if (!res.ok) throw new ApiError(res.status, (await res.json().catch(() => ({}))).error || 'Could not open the file');
  window.open(URL.createObjectURL(await res.blob()), '_blank');
}

/** A date as YYYY-MM-DD in the browser's time zone. Never toISOString().slice(0, 10): that is the
 *  UTC date, a day early in India before 05:30 and for any local midnight. */
export const ymd = (d: Date = new Date()) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
export const today = () => ymd();
/** YYYY-MM-DD shifted by n days (negative for earlier). */
export const addDays = (s: string, n: number) => ymd(new Date(+s.slice(0, 4), +s.slice(5, 7) - 1, +s.slice(8, 10) + n));
/** YYYY-MM of a date, or of YYYY-MM shifted by n months. */
export const ymOf = (d: Date = new Date()) => ymd(d).slice(0, 7);
export const addMonths = (ym: string, n: number) => ymOf(new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1 + n, 1));

/** The month of yesterday: attendance views open on it, so the 1st of a month still shows a full month. */
export const recentYm = () => addDays(today(), -1).slice(0, 7);
