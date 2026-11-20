import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent } from 'react';
import { api } from '../api';
import { rankPeople } from './personMatch';

/** A person the picker can show: an employee row from /api/employees, or any list a page has (managers, users). */
export type Person = {
  id: string; name: string;
  /** The code shown next to the name when the id is not it (a role mapping's employee). */
  code?: string;
  designation?: string; department?: string; location?: string; city?: string; email?: string; type?: string; status?: string;
  /** The muted line under the name, when the default (designation · department · location) does not fit. */
  detail?: string;
  [k: string]: any;
};

const SHOW = 20;

/** Type-to-search people picker that looks like a dropdown (an accessible combobox).
 *
 *  Two modes:
 *  - `people` given: picks from that list (managers, approvers, users), matched in the browser;
 *  - otherwise: searches every employee in the caller's data access on the server (GET /api/employees?q=), with
 *    `search` added to the query (for example "&status=Active").
 *  Matching is approximate on both sides (words in any order, start of a word, initials, small typos), best first.
 *  `value` is the chosen id ('' for none, or the pinned `empty` option); `onChange` gets the id and the person. */
export default function PersonPicker({ value, onChange, people, search = '', selected, empty, placeholder, exclude, accept, detail, disabled, required, label, style, noMatch }: {
  value: string;
  onChange: (id: string, person: Person | null) => void;
  people?: Person[] | null;
  search?: string;
  /** The chosen person when the page already knows them (server mode), so their name shows without a lookup. */
  selected?: Person | null;
  /** A pinned first option that means "nobody chosen" (value ''), like "All employees" or "Keep current manager". */
  empty?: string;
  placeholder?: string;
  exclude?: string[];
  accept?: (p: Person) => boolean;
  detail?: (p: Person) => string;
  disabled?: boolean;
  /** Someone must stay chosen: no clear button. */
  required?: boolean;
  label?: string;
  style?: CSSProperties;
  noMatch?: string;
}) {
  const client = people !== undefined;
  const uid = useId().replace(/:/g, '');
  const listId = `pp-${uid}`;
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState('');
  const [typed, setTyped] = useState(false);
  const [active, setActive] = useState(0);
  const [known, setKnown] = useState<Person | null>(null);
  const [srv, setSrv] = useState<{ key: string; rows: Person[]; total: number } | null>(null);
  const [srvErr, setSrvErr] = useState('');
  const input = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const t = typed ? q.trim() : '';
  const keep = (p: Person) => !(exclude ?? []).includes(p.id) && (!accept || accept(p));

  // Server mode: fetch the matches for what is typed (200 ms after the last key), only while the list is open.
  const key = `${t}|${search}`;
  useEffect(() => {
    if (client || !open) return;
    let live = true;
    const go = () => api(`/employees?size=${SHOW}${t ? `&q=${encodeURIComponent(t)}` : ''}${search}`)
      .then((d) => { if (live) { setSrv({ key, rows: d.rows as Person[], total: d.total as number }); setSrvErr(''); } })
      .catch((e) => { if (live) { setSrv({ key, rows: [], total: 0 }); setSrvErr(e.message); } });
    const timer = setTimeout(go, t ? 200 : 0);
    return () => { live = false; clearTimeout(timer); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, open, key]);

  // The chosen person: from the list, what the page passed, the last one picked, or (server mode) looked up once.
  const chosen = useMemo(() => {
    if (!value) return null;
    if (client) return people?.find((p) => p.id === value) ?? null;
    if (selected?.id === value) return selected;
    return known?.id === value ? known : null;
  }, [value, client, people, selected, known]);
  useEffect(() => {
    if (client || !value || chosen) return;
    let live = true;
    api(`/employees?size=5&q=${encodeURIComponent(value)}${search}`)
      .then((d) => { const p = (d.rows as Person[]).find((x) => x.id === value); if (live && p) setKnown(p); }).catch(() => {});
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, value, !!chosen]);

  const { rows, total, loading } = useMemo(() => {
    if (client) {
      const all = (people ?? []).filter(keep);
      const hits = t ? rankPeople(t, all) : all;
      return { rows: hits.slice(0, SHOW), total: hits.length, loading: people === null };
    }
    const fresh = srv?.key === key;
    const r = fresh ? srv!.rows.filter(keep) : [];
    return { rows: r, total: fresh ? srv!.total - (srv!.rows.length - r.length) : 0, loading: !fresh };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, people, t, srv, key, exclude?.join(','), accept]);

  // Options: the pinned empty option first, then the matches.
  const opts: (Person | null)[] = [...(empty !== undefined ? [null] : []), ...rows];
  useEffect(() => { setActive(empty !== undefined && !t ? 0 : empty !== undefined && rows.length ? 1 : 0); }, [t, rows.length, empty]);
  useEffect(() => {
    if (!open) return;
    listRef.current?.querySelector<HTMLElement>(`#${listId}-${active}`)?.scrollIntoView({ block: 'nearest' });
  }, [active, open, listId]);

  // The list is placed with fixed positioning under the input (above it when there is no room below), so a table
  // that scrolls sideways or a card's edge never cuts it off.
  const [pos, setPos] = useState<CSSProperties>({});
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const r = input.current?.getBoundingClientRect();
      if (!r) return;
      const vh = window.innerHeight, vw = window.innerWidth, below = vh - r.bottom - 8, above = r.top - 8;
      const width = Math.min(Math.max(r.width, 300), vw - 16), left = Math.max(8, Math.min(r.left, vw - width - 8));
      const up = below < 220 && above > below;
      setPos({ position: 'fixed', left, width, right: 'auto', maxHeight: Math.max(120, Math.min(340, (up ? above : below) - 4)),
        ...(up ? { top: 'auto', bottom: vh - r.top + 4 } : { top: r.bottom + 4, bottom: 'auto' }) });
    };
    place();
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => { window.removeEventListener('scroll', place, true); window.removeEventListener('resize', place); };
  }, [open]);

  const code = (p: Person) => p.code ?? p.id;
  const shown = chosen ? `${chosen.name} · ${code(chosen)}` : value ? value : '';
  const line = (p: Person) => detail ? detail(p) : p.detail ?? [p.designation, p.department, p.location].filter(Boolean).join(' · ');

  const close = () => { setOpen(false); setTyped(false); setQ(''); };
  const pick = (p: Person | null) => {
    if (p) setKnown(p);
    onChange(p ? p.id : '', p);
    close();
  };
  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!open) { setOpen(true); return; }
      const n = opts.length;
      if (n) setActive((a) => (a + (e.key === 'ArrowDown' ? 1 : n - 1)) % n);
    } else if (e.key === 'Enter') {
      if (open && opts.length && active < opts.length) { e.preventDefault(); pick(opts[active]); }
    } else if (e.key === 'Escape') {
      if (open) { e.preventDefault(); e.stopPropagation(); close(); }
    } else if (e.key === 'Tab') close();
  };

  const more = total > rows.length;
  return <div className={`pp${open ? ' open' : ''}${disabled ? ' dis' : ''}`} style={style} data-value={value}>
    <input ref={input} type="text" role="combobox" aria-expanded={open} aria-controls={listId} aria-autocomplete="list" aria-label={label}
      aria-activedescendant={open && opts.length ? `${listId}-${active}` : undefined} autoComplete="off" spellCheck={false} disabled={disabled}
      placeholder={placeholder ?? (empty ?? (client ? 'Type a name or code' : 'Search by name, code or place'))}
      value={typed ? q : shown}
      onFocus={(e) => { setOpen(true); e.currentTarget.select(); }}
      onClick={() => setOpen(true)}
      onBlur={() => close()}
      onChange={(e) => { setQ(e.target.value); setTyped(true); setOpen(true); }}
      onKeyDown={onKey} />
    {value && !disabled && !required && <button type="button" className="pp-x" aria-label="Clear" title="Clear" tabIndex={-1}
      onMouseDown={(e) => e.preventDefault()} onClick={() => { onChange('', null); close(); input.current?.focus(); }}>×</button>}
    {(!value || required) && !disabled && <span className="pp-caret" aria-hidden="true"><svg width="12" height="12" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"><path d="M3 4.5 6 7.5 9 4.5" /></svg></span>}
    {open && <ul className="pp-list" role="listbox" id={listId} ref={listRef} style={pos} aria-label={label ? `${label} matches` : 'Matches'}
      onMouseDown={(e) => e.preventDefault()}>
      {opts.map((p, i) => p === null
        ? <li key="__empty" id={`${listId}-${i}`} role="option" aria-selected={!value} className={`pp-o pin${i === active ? ' on' : ''}`}
          data-id="" onMouseEnter={() => setActive(i)} onClick={() => pick(null)}>{empty}</li>
        : <li key={p.id} id={`${listId}-${i}`} role="option" aria-selected={p.id === value} className={`pp-o${i === active ? ' on' : ''}`}
          data-id={p.id} onMouseEnter={() => setActive(i)} onClick={() => pick(p)}>
          <div className="pp-n"><b>{p.name}</b> <span className="mono">{code(p)}</span></div>
          {line(p) && <div className="pp-d">{line(p)}</div>}
        </li>)}
      {loading && !rows.length && <li className="pp-s" role="presentation"><i aria-hidden="true" />Searching…</li>}
      {!loading && !rows.length && <li className="pp-s" role="presentation">{srvErr || (t ? (noMatch ?? 'No match in your access') : 'Nobody to pick')}</li>}
      {!loading && more && <li className="pp-s" role="presentation">Showing {rows.length} of {total}. Keep typing to narrow.</li>}
    </ul>}
  </div>;
}
