import { useEffect, useRef, useState, type ReactNode } from 'react';
import { onBusy, session } from '../api';

export const Pill = ({ children, k = 'n' }: { children: ReactNode; k?: 'ok' | 'warn' | 'bad' | 'info' | 'n' }) => <span className={`pill ${k}`}>{children}</span>;

export const statusPill = (s: string) =>
  <Pill k={s === 'Active' || s === 'Approved' ? 'ok' : s === 'Rejected' ? 'bad' : s === 'Pending' || s === 'Pre-boarding' ? 'warn' : 'n'}>{s}</Pill>;

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return <label className="field"><span>{label}</span>{children}</label>;
}

export function Drawer({ onClose, children }: { onClose: () => void; children: ReactNode }) {
  useEffect(() => {
    const k = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, [onClose]);
  return <div className="ov right" onClick={(e) => e.target === e.currentTarget && onClose()}><div className="dlg" role="dialog">{children}</div></div>;
}

export function Bars({ rows }: { rows: { label: string; value: number }[] }) {
  const mx = Math.max(1, ...rows.map((r) => r.value));
  return <>{rows.map((r) => <div className="bar" key={r.label}><span>{r.label}</span><i><b style={{ width: `${(r.value / mx) * 100}%` }} /></i><span className="mono">{r.value}</span></div>)}</>;
}

let toastSet: (m: string) => void = () => {};
export const toast = (m: string) => toastSet(m);
export function Toaster() {
  const [m, setM] = useState('');
  useEffect(() => { toastSet = (x) => { setM(x); setTimeout(() => setM(''), 2600); }; }, []);
  return m ? <div id="toast" role="status">{m}</div> : null;
}

/** Load data from the API with loading and error state; `reload` refetches. */
export function useLoad<T>(fn: () => Promise<T>, deps: unknown[]) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState('');
  const [n, setN] = useState(0);
  useEffect(() => {
    let live = true;
    setError('');
    fn().then((d) => live && setData(d)).catch((e) => live && setError(e.message));
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, n]);
  return { data, error, reload: () => setN((x) => x + 1) };
}

/** Centered dialog (prototype `.ov` / `.dlg`); `wide` for tables. Escape or a click outside closes it. */
export function Modal({ onClose, wide, children }: { onClose: () => void; wide?: boolean; children: ReactNode }) {
  useEffect(() => {
    const k = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, [onClose]);
  return <div className="ov" onClick={(e) => e.target === e.currentTarget && onClose()}><div className={`dlg${wide ? ' wide' : ''}`} role="dialog">{children}</div></div>;
}

/** Password input with a show / hide toggle. */
export function PasswordInput(props: { value: string; onChange: (v: string) => void; autoFocus?: boolean }) {
  const [show, setShow] = useState(false);
  return <div className="pw"><input type={show ? 'text' : 'password'} autoFocus={props.autoFocus} value={props.value} onChange={(e) => props.onChange(e.target.value)} />
    <button type="button" className="btn sm" onClick={() => setShow(!show)} aria-label={show ? 'Hide password' : 'Show password'}>{show ? 'Hide' : 'Show'}</button></div>;
}

/** Download a file from the API (the session token travels in the header, so a plain link cannot be used). */
export async function download(path: string, fallbackName: string) {
  const res = await fetch('/api' + path, { headers: { Authorization: `Bearer ${session.token}`, 'X-Role': session.role } });
  if (!res.ok) {
    const d = await res.json().catch(() => ({}));
    throw new Error(d.error || 'Download failed');
  }
  const name = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') || '')?.[1] || fallbackName;
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement('a');
  a.href = url; a.download = name; a.click();
  URL.revokeObjectURL(url);
}

/** A row of tabs. When the row is wider than the screen it scrolls sideways, and the selected tab is scrolled into
 *  view, so it is never cut off at the edge. */
export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: [T, string][]; value: T; onChange: (t: T) => void }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const box = ref.current, on = box?.querySelector<HTMLElement>('.tab.on');
    if (!box || !on || box.scrollWidth <= box.clientWidth) return;
    const b = box.getBoundingClientRect(), r = on.getBoundingClientRect();
    if (r.left < b.left) box.scrollLeft -= b.left - r.left + 24;
    else if (r.right > b.right) box.scrollLeft += r.right - b.right + 24;
  }, [value, tabs.length]);
  return <div className="tabs" ref={ref}>{tabs.map(([k, l]) => <button key={k} className={`tab ${value === k ? 'on' : ''}`} onClick={() => onChange(k)}>{l}</button>)}</div>;
}

/** Shown in place of a page or tab while its data loads, or the error when it failed. */
export function Loading({ error }: { error?: string }) {
  if (error) return <div className="note bad" role="alert">{error}</div>;
  return <div className="loading" role="status" aria-live="polite"><i aria-hidden="true" />Loading…</div>;
}

/** A thin bar across the top while any request takes longer than a moment. */
export function BusyBar() {
  const [on, setOn] = useState(false);
  useEffect(() => {
    let t: ReturnType<typeof setTimeout> | undefined;
    const off = onBusy((n) => { clearTimeout(t); if (n > 0) t = setTimeout(() => setOn(true), 250); else setOn(false); });
    return () => { clearTimeout(t); off(); };
  }, []);
  return on ? <div className="busybar" role="progressbar" aria-label="Loading" /> : null;
}

export function Pager({ page, pages, total, size, onChange }: { page: number; pages: number; total?: number; size: number; onChange: (p: number) => void }) {
  return <div className="row pager">
    <button className="btn sm" disabled={page <= 1} onClick={() => onChange(page - 1)}>← Previous</button>
    <span className="mute" style={{ fontSize: 12.5 }}>{total != null ? `${(page - 1) * size + 1}–${Math.min(page * size, total)} of ${total}` : `Page ${page}`}</span>
    <button className="btn sm" disabled={page >= pages} onClick={() => onChange(page + 1)}>Next →</button>
  </div>;
}

/** A long list shown a page at a time; back to the first page when the list changes length. */
export function usePage<T>(rows: T[] | null | undefined, size = 100) {
  const [page, setPage] = useState(1);
  const total = rows?.length ?? 0, pages = Math.max(1, Math.ceil(total / size));
  useEffect(() => setPage(1), [total]);
  return { rows: rows?.slice((page - 1) * size, page * size), pager: total > size ? <Pager page={page} pages={pages} total={total} size={size} onChange={setPage} /> : null };
}

/** On / off switch (prototype `.sw`). */
export const Switch = ({ on, onChange, disabled, label }: { on: boolean; onChange?: () => void; disabled?: boolean; label?: string }) =>
  <button type="button" className={`sw${on ? ' on' : ''}`} disabled={disabled} onClick={onChange} aria-label={label || 'Toggle'} aria-pressed={on} />;

/** Approval route as pills joined by arrows (prototype `.chain` / `.step`): done, current, rejected, upcoming. */
export function Chain({ steps, step = -1, status }: { steps: string[]; step?: number; status?: string }) {
  return <div className="chain">{steps.map((c, i) => {
    const done = i < step || status === 'Approved', cur = i === step && status === 'Pending', rej = status === 'Rejected' && i === step;
    return <span key={i} className="chain-i">{i > 0 && <span className="arrow">→</span>}<span className={`step${rej ? ' rej' : done ? ' done' : cur ? ' cur' : ''}`}>{c}</span></span>;
  })}</div>;
}

/** Collapsible card with a Show / Hide label (prototype `details.adv`). */
export const Adv = ({ title, open, children }: { title: ReactNode; open?: boolean; children: ReactNode }) =>
  <details className="adv" open={open}><summary>{title}</summary><div>{children}</div></details>;
