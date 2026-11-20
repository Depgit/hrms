import { useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api } from '../api';
import { Loading, Pager, Pill, Tabs, toast, useLoad } from '../components/ui';

/** My view › Employee directory (owner, 12 Nov 2026). Active people only, each with name, designation, department,
 *  location and the email on file; never the employee code, mobile, pay, birth date or ID. A person is named by `ref`
 *  (the same in the people list and in a chart), never by their code.
 *  - Everyone: the whole company, searched on the server (words in any order, partial words, small typos), filtered by
 *    department and location, 50 at a time. Tapping a person opens their department's chart centred on them.
 *  - Departments: each department with its headcount, the employee's own first and highlighted. Tapping one opens its
 *    org chart: "Around me" (manager, peers, reports; any card re-centres it) or the whole department as a tree.
 *  Where the employee is (tab, search, filters, page, open department) is kept in the address, so the phone's back
 *  button steps back through it. */

type Group = { id: string; name: string; count: number; mine?: boolean };
type Meta = { mine: string; total: number; departments: Group[]; locations: Group[] };
type Row = { ref: string; name: string; designation: string; department: string; dept: string; location: string; email?: string };
type People = { rows: Row[]; total: number; page: number; size: number; pages: number };
/** One person in a department's org chart (GET /api/me/directory?dept=): `key` numbers the people for this answer
 *  only; `manager` is the reporting manager's key (0 for none). */
type P = { key: number; ref: string; name: string; designation: string; location: string; email?: string; manager: number; outside?: boolean; me?: boolean };

/** Copies text to the clipboard (with the older way for browsers that refuse the clipboard API on plain http). */
async function copyText(s: string) {
  try {
    await navigator.clipboard.writeText(s);
  } catch {
    const t = document.createElement('textarea');
    t.value = s; t.style.position = 'fixed'; t.style.opacity = '0';
    document.body.appendChild(t); t.select();
    const ok = document.execCommand('copy');
    t.remove();
    if (!ok) { toast('Copy is not available here'); return; }
  }
  toast('Email copied');
}

/** The email as a mail link with a Copy button; a dash when there is none on file. */
function Mail({ email, compact }: { email?: string; compact?: boolean }) {
  if (!email) return <span className="mute">—</span>;
  return <span className="row" style={{ gap: 6, flexWrap: 'nowrap', minWidth: 0 }} onClick={(e) => e.stopPropagation()}>
    <a href={`mailto:${email}`} style={{ color: 'var(--accent)', fontWeight: 600, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0, fontSize: compact ? 12.5 : undefined }}>{email}</a>
    <button type="button" className="btn sm" aria-label={`Copy ${email}`} onClick={() => copyText(email)}>Copy</button>
  </span>;
}

function useNarrow(px = 760) {
  const q = `(max-width: ${px}px)`;
  const [n, setN] = useState(() => typeof window !== 'undefined' && window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q), f = () => setN(m.matches);
    m.addEventListener('change', f);
    return () => m.removeEventListener('change', f);
  }, [q]);
  return n;
}

export default function Directory() {
  const [sp, setSp] = useSearchParams();
  const get = (k: string) => sp.get(k) || '';
  /** Changes some address parameters (blank removes one); `replace` for typing, so back does not step per letter. */
  const nav = (patch: Record<string, string>, replace = false) => setSp((cur) => {
    const n = new URLSearchParams(cur);
    Object.entries(patch).forEach(([k, v]) => (v ? n.set(k, v) : n.delete(k)));
    return n;
  }, { replace });
  const meta = useLoad(() => api<Meta>('/me/directory/departments'), []);
  const tab = get('tab') === 'depts' ? 'depts' : 'all';
  const dept = get('dept');
  const open = (id: string, ref = '') => nav({ dept: id, p: ref });
  if (!meta.data) return <Loading error={meta.error} />;
  if (!meta.data.total) return <div className="card"><p className="mute" style={{ margin: 0 }}>The directory shows the colleagues of active employees.</p></div>;
  const m = meta.data, mine = m.departments.find((d) => d.mine);
  return <>
    <Tabs tabs={[['all', 'Everyone'], ['depts', 'Departments']]} value={tab} onChange={(t) => nav({ tab: t === 'depts' ? 'depts' : '', dept: '', p: '' })} />
    {dept ? <Chart key={dept} id={dept} focusRef={get('p')} back={tab === 'all' ? 'Everyone' : 'All departments'} onBack={() => nav({ dept: '', p: '' })} />
      : tab === 'all' ? <Everyone m={m} mine={mine} get={get} nav={nav} open={open} />
        : <Departments m={m} open={open} />}
  </>;
}

/** Everyone: the company's active people, searched, filtered and paged on the server. */
function Everyone({ m, mine, get, nav, open }: { m: Meta; mine?: Group; get: (k: string) => string; nav: (p: Record<string, string>, replace?: boolean) => void; open: (id: string, ref?: string) => void }) {
  const narrow = useNarrow();
  const [q, setQ] = useState(get('q'));
  const fd = get('fd'), fl = get('fl'), page = Math.max(1, +get('pg') || 1);
  // Search as the employee types, a moment after the last key.
  useEffect(() => {
    const t = setTimeout(() => { if (q.trim() !== get('q')) nav({ q: q.trim(), pg: '' }, true); }, 250);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q]);
  const qs = new URLSearchParams(Object.entries({ q: get('q'), dept: fd, loc: fl, page: String(page) }).filter(([, v]) => v)).toString();
  const { data, error } = useLoad(() => api<People>(`/me/directory/people?${qs}`), [qs]);
  const filtered = !!(get('q') || fd || fl);
  return <>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ flexWrap: 'wrap', gap: 10, marginBottom: 12 }}>
        <div><h2 style={{ margin: 0, fontSize: 18 }}>Everyone</h2><div className="mute" style={{ fontSize: 13 }}>{m.total} active {m.total === 1 ? 'person' : 'people'} in {m.departments.length} departments</div></div>
        {mine && <button className="btn" onClick={() => open(mine.id)}>My department: <b style={{ marginLeft: 4 }}>{mine.name}</b> ›</button>}
      </div>
      <div className="row" style={{ gap: 10, flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <label className="field" style={{ flex: '2 1 260px' }}><span>Search</span>
          <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name, designation, department, location or email" aria-label="Search the directory" /></label>
        <label className="field" style={{ flex: '1 1 160px' }}><span>Department</span>
          <select value={fd} onChange={(e) => nav({ fd: e.target.value, pg: '' })}><option value="">All departments</option>{m.departments.map((d) => <option key={d.id} value={d.id}>{d.name} ({d.count})</option>)}</select></label>
        <label className="field" style={{ flex: '1 1 160px' }}><span>Location</span>
          <select value={fl} onChange={(e) => nav({ fl: e.target.value, pg: '' })}><option value="">All locations</option>{m.locations.map((d) => <option key={d.id} value={d.id}>{d.name} ({d.count})</option>)}</select></label>
        {filtered && <button className="btn sm" style={{ marginBottom: 4 }} onClick={() => { setQ(''); nav({ q: '', fd: '', fl: '', pg: '' }); }}>Clear</button>}
      </div>
    </div>
    {error ? <div className="note bad">{error}</div> : !data ? <Loading /> : <div className="card">
      <div className="mute" style={{ fontSize: 12.5, marginBottom: 10 }}>{data.total ? `${data.total} ${data.total === 1 ? 'person' : 'people'}${filtered ? ' match' : ''} · tap a person to see their department's chart` : ''}</div>
      {!data.rows.length ? <p className="mute" style={{ margin: 0 }}>No one matches. Check the spelling or clear a filter.</p>
        : narrow ? <div className="grid">{data.rows.map((r) => <PersonCard key={r.ref} r={r} open={open} />)}</div>
          : <div className="tw"><table>
            <thead><tr><th>Name</th><th>Designation</th><th>Department</th><th>Location</th><th>Email</th></tr></thead>
            <tbody>{data.rows.map((r) => <tr key={r.ref} className="click" onClick={() => open(r.dept, r.ref)} title={`Show ${r.name} in the ${r.department} chart`}>
              <td><b>{r.name}</b></td><td>{r.designation}</td>
              <td><button type="button" onClick={(e) => { e.stopPropagation(); open(r.dept); }} style={{ background: 'none', border: 0, padding: 0, font: 'inherit', color: 'var(--accent)', cursor: 'pointer', fontWeight: 600 }}>{r.department}</button></td>
              <td>{r.location}</td><td style={{ maxWidth: 300 }}><Mail email={r.email} /></td>
            </tr>)}</tbody>
          </table></div>}
      {data.pages > 1 && <div style={{ marginTop: 12 }}><Pager page={data.page} pages={data.pages} total={data.total} size={data.size} onChange={(p) => { nav({ pg: p > 1 ? String(p) : '' }); window.scrollTo(0, 0); }} /></div>}
    </div>}
  </>;
}

/** A person in the Everyone list on a phone: tap the card for the chart, the email to write, Copy to copy it. */
function PersonCard({ r, open }: { r: Row; open: (id: string, ref?: string) => void }) {
  return <div role="button" tabIndex={0} onClick={() => open(r.dept, r.ref)} onKeyDown={(e) => e.key === 'Enter' && open(r.dept, r.ref)}
    style={{ border: '1px solid var(--line)', borderRadius: 'var(--r)', padding: '10px 12px', cursor: 'pointer', background: 'var(--surface)', minWidth: 0 }}>
    <b style={{ fontSize: 14 }}>{r.name}</b>
    <div className="ink2" style={{ fontSize: 12.5 }}>{r.designation}</div>
    <div className="mute" style={{ fontSize: 12, marginBottom: 6 }}>{r.department} · {r.location}</div>
    <Mail email={r.email} compact />
  </div>;
}

/** Departments with their headcount; the employee's own first, highlighted. */
function Departments({ m, open }: { m: Meta; open: (id: string) => void }) {
  const l = [...m.departments.filter((d) => d.mine), ...m.departments.filter((d) => !d.mine)];
  return <div className="grid g4">{l.map((d) => <button key={d.id} onClick={() => open(d.id)} className="card"
    style={{ textAlign: 'left', font: 'inherit', color: 'inherit', cursor: 'pointer', borderColor: d.mine ? 'var(--accent)' : undefined, background: d.mine ? 'var(--accent-soft)' : undefined }}>
    <div className="row sp" style={{ gap: 6, alignItems: 'flex-start' }}><b style={{ fontSize: 14.5 }}>{d.name}</b>{d.mine && <Pill k="info">Your department</Pill>}</div>
    <div className="mute" style={{ fontSize: 13, marginTop: 4 }}>{d.count} {d.count === 1 ? 'person' : 'people'}</div>
  </button>)}</div>;
}

type Tree = { by: Map<number, P>; kids: Map<number, P[]> };

/** A department's org chart, centred on `focusRef` (else the employee, else the top of the department). */
function Chart({ id, focusRef, back, onBack }: { id: string; focusRef: string; back: string; onBack: () => void }) {
  const { data, error } = useLoad(() => api<{ department: string; mine: boolean; nodes: P[] }>(`/me/directory?dept=${encodeURIComponent(id)}`), [id]);
  const [focus, setFocus] = useState(0);
  const [mode, setMode] = useState<'me' | 'all'>('me');
  const [q, setQ] = useState('');
  const t = useMemo(() => {
    const nodes = data?.nodes ?? [], by = new Map(nodes.map((n) => [n.key, n])), kids = new Map<number, P[]>();
    nodes.forEach((n) => { if (n.manager && by.has(n.manager)) kids.set(n.manager, [...(kids.get(n.manager) ?? []), n]); });
    const roots = nodes.filter((n) => !n.manager || !by.has(n.manager));
    // The top of the department: someone in it whose manager is outside it (or who has none), with the most reports.
    const tops = nodes.filter((n) => !n.outside && (!n.manager || by.get(n.manager)?.outside));
    const top = tops.sort((a, b) => (kids.get(b.key)?.length ?? 0) - (kids.get(a.key)?.length ?? 0))[0];
    const start = nodes.find((n) => focusRef && n.ref === focusRef) ?? nodes.find((n) => n.me) ?? top ?? nodes[0];
    return { nodes, by, kids, roots, start };
  }, [data, focusRef]);
  const backLink = <button className="btn sm" onClick={onBack}>← {back}</button>;
  if (!data) return <>{backLink}<div style={{ marginTop: 12 }}><Loading error={error} /></div></>;
  const inDept = t.nodes.filter((n) => !n.outside).length;
  if (!t.start) return <><div style={{ marginBottom: 12 }}>{backLink}</div><div className="card"><h2 style={{ margin: 0, fontSize: 18 }}>{data.department}</h2><p className="mute" style={{ margin: '6px 0 0' }}>No active people in this department.</p></div></>;
  const f = t.by.get(focus) ?? t.start;
  const ql = q.trim().toLowerCase();
  const found = ql ? t.nodes.filter((n) => `${n.name} ${n.designation} ${n.location} ${n.email ?? ''}`.toLowerCase().includes(ql)).slice(0, 12) : [];
  const go = (n: P) => { setFocus(n.key); setMode('me'); setQ(''); };
  return <>
    <div style={{ marginBottom: 12 }}>{backLink}</div>
    <div className="card" style={{ marginBottom: 14 }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
      <div><h2 style={{ margin: 0, fontSize: 18, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>{data.department}{data.mine && <Pill k="info">Your department</Pill>}</h2>
        <div className="mute" style={{ fontSize: 13 }}>{inDept} active {inDept === 1 ? 'person' : 'people'}{data.mine ? ' in your department' : ''}</div></div>
      <div className="row" style={{ gap: 10, flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <label className="field" style={{ minWidth: 220 }}><span>Search this department</span><input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name, designation or email" /></label>
        <div className="seg">{([['me', data.mine ? 'Around me' : 'Team view'], ['all', 'Whole department']] as const).map(([k, l]) => <button key={k} className={mode === k ? 'on' : ''} onClick={() => { setMode(k); if (k === 'me') setFocus(0); }}>{l}</button>)}</div>
      </div></div>
      {ql && <div style={{ marginTop: 12 }}>{found.length ? <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>{found.map((n) => <button key={n.key} className="btn sm" onClick={() => go(n)}>{n.name} <span className="mute">· {n.designation}</span></button>)}</div>
        : <p className="mute" style={{ margin: 0, fontSize: 13 }}>No one in {data.department} matches.</p>}</div>}
    </div>
    {mode === 'me' ? <Around f={f} t={t} go={go} /> : <div className="card"><div className="tw">{t.roots.map((r) => <Branch key={r.key} n={r} t={t} go={go} depth={0} />)}</div></div>}
  </>;
}

/** A person's card: name, designation, location and email. Greyed for a manager from another department. */
function Card({ n, on, go, t }: { n: P; on?: boolean; go: (n: P) => void; t: Tree }) {
  const r = t.kids.get(n.key)?.length ?? 0;
  return <button onClick={() => go(n)} title={n.outside ? 'Manages people in this department; works in another department' : `Show ${n.name}'s team`}
    style={{ textAlign: 'left', font: 'inherit', color: 'inherit', cursor: 'pointer', background: on ? 'var(--accent-soft)' : 'var(--surface)', border: `1px solid ${on ? 'var(--accent)' : 'var(--line)'}`,
      borderRadius: 'var(--r)', padding: '10px 12px', width: 220, maxWidth: '100%', opacity: n.outside ? 0.6 : 1 }}>
    <div className="row sp" style={{ gap: 6, alignItems: 'flex-start' }}><b style={{ fontSize: 13.5, lineHeight: 1.3 }}>{n.name}</b>{n.me && <Pill k="info">You</Pill>}</div>
    <div className="ink2" style={{ fontSize: 12.5, marginTop: 2 }}>{n.designation}</div>
    <div className="mute" style={{ fontSize: 12 }}>{n.location}</div>
    {n.email && <div className="mute" style={{ fontSize: 11.5, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{n.email}</div>}
    {(n.outside || r > 0) && <div className="mute" style={{ fontSize: 11.5, marginTop: 4 }}>{n.outside ? 'Other department' : ''}{n.outside && r > 0 ? ' · ' : ''}{r > 0 ? `${r} report${r === 1 ? '' : 's'}` : ''}</div>}
  </button>;
}

const Row = ({ children }: { children: React.ReactNode }) => <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, justifyContent: 'center' }}>{children}</div>;
const Link = () => <div style={{ width: 1, height: 18, background: 'var(--line)', margin: '6px auto' }} />;
const Label = ({ children }: { children: React.ReactNode }) => <div className="mute" style={{ fontSize: 12, textAlign: 'center', marginBottom: 8 }}>{children}</div>;

/** The chart centred on one person: their manager on top, them with their peers, and their reports below; under the
 *  chart, the selected person's email to write to or copy. */
function Around({ f, t, go }: { f: P; t: Tree; go: (n: P) => void }) {
  const mgr = f.manager ? t.by.get(f.manager) : undefined, up = mgr?.manager ? t.by.get(mgr.manager) : undefined;
  const peers = mgr ? t.kids.get(mgr.key) ?? [f] : [f];
  const reps = t.kids.get(f.key) ?? [];
  return <div className="card">
    {up && <div style={{ textAlign: 'center', marginBottom: 10 }}><button className="btn sm" onClick={() => go(mgr!)}>Up to {up.name}</button></div>}
    {mgr && <><Label>Reporting manager</Label><Row><Card n={mgr} go={go} t={t} /></Row><Link /></>}
    <Label>{mgr ? (peers.length > 1 ? `${f.me ? 'You and your peers' : `${f.name} and peers`}` : f.me ? 'You' : f.name) : 'Top of the department'}</Label>
    <Row>{peers.map((n) => <Card key={n.key} n={n} on={n.key === f.key} go={go} t={t} />)}</Row>
    {reps.length > 0 && <><Link /><Label>{f.me ? 'Your reports' : `Reports to ${f.name}`}</Label><Row>{reps.map((n) => <Card key={n.key} n={n} go={go} t={t} />)}</Row></>}
    {!mgr && !reps.length && peers.length === 1 && <p className="mute" style={{ textAlign: 'center', fontSize: 13, margin: '12px 0 0' }}>No reporting lines on record.</p>}
    <div className="row" style={{ justifyContent: 'center', gap: 8, flexWrap: 'wrap', marginTop: 14, paddingTop: 12, borderTop: '1px solid var(--line)', fontSize: 13 }}>
      <b>{f.name}</b><Mail email={f.email} />
    </div>
  </div>;
}

/** The whole department as an indented tree by reporting manager. */
function Branch({ n, t, go, depth }: { n: P; t: Tree; go: (n: P) => void; depth: number }) {
  const [open, setOpen] = useState(depth < 2);
  const k = t.kids.get(n.key) ?? [];
  return <div style={{ marginLeft: depth ? 18 : 0, borderLeft: depth ? '1px solid var(--line)' : 'none', paddingLeft: depth ? 10 : 0 }}>
    <div className="row" style={{ gap: 8, padding: '5px 0', alignItems: 'center', opacity: n.outside ? 0.6 : 1 }}>
      {k.length ? <button className="btn sm" aria-label={open ? 'Collapse' : 'Expand'} onClick={() => setOpen(!open)} style={{ minWidth: 30 }}>{open ? '−' : '+'}</button> : <span style={{ width: 30, flex: 'none', display: 'inline-block' }} />}
      <button onClick={() => go(n)} style={{ background: 'none', border: 0, padding: 0, font: 'inherit', color: 'inherit', cursor: 'pointer', textAlign: 'left' }}>
        <b>{n.name}</b>{n.me && <> <Pill k="info">You</Pill></>} <span className="ink2" style={{ fontSize: 12.5 }}>{n.designation}</span> <span className="mute" style={{ fontSize: 12 }}>· {n.location}{n.outside ? ' · Other department' : ''}{k.length ? ` · ${k.length} report${k.length === 1 ? '' : 's'}` : ''}</span></button>
      {n.email && <a href={`mailto:${n.email}`} className="mute" style={{ fontSize: 12, whiteSpace: 'nowrap' }}>{n.email}</a>}
    </div>
    {open && k.map((c) => <Branch key={c.key} n={c} t={t} go={go} depth={depth + 1} />)}
  </div>;
}
