import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { api, fmtD, recentYm, addDays, addMonths, today, postForm, session } from '../api';
import { useAuth } from '../auth';
import { Chain, Field, Loading, Modal, Pill, download, toast, useLoad } from '../components/ui';
import PersonPicker from '../components/PersonPicker';
import { SkipApprovalButton } from '../components/SkipApproval';
import RealtimeMatch from './RealtimeMatch';
import AttAlerts from './AttAlerts';
import MachinePool from './MachinePool';
import PortalSend from './PortalSend';
import CalledToWork from './CalledToWork';

type K = 'ok' | 'warn' | 'bad' | 'info' | 'n';
type T = 'sum' | 'rec' | 'exc' | 'call' | 'mis' | 'api';
type View = 'emp' | 'date' | 'cal';
export const REG_REASONS = ['Forgot to punch', 'Biometric or device issue', 'Field or site duty', 'Work from home', 'Roster changed', 'Other'];
const tone = (t: string) => (t || 'n') as K;
const wd = (d: string) => new Date(d + 'T00:00:00').toLocaleDateString('en-IN', { weekday: 'short' });
export const ymLabel = (ym: string) => new Date(ym + '-15T00:00:00').toLocaleString('en-IN', { month: 'long', year: 'numeric' });
/** Hours between an in and an out time, past midnight when the out is earlier (prototype attHours). */
export const hrs = (i?: string, o?: string) => {
  const m = (s?: string) => { const x = /^(\d{1,2}):(\d{2})/.exec(s || ''); return x ? +x[1] * 60 + +x[2] : null; };
  const a = m(i), b = m(o);
  if (a == null || b == null) return '—';
  return (((b <= a ? b + 1440 : b) - a) / 60).toFixed(1) + ' h';
};

// ---------- Shared data: the employees in the caller's scope, day records, masters ----------

const cache = new Map<string, { at: number; p: Promise<any> }>();
/** GET with a short cache, keyed by the session and role view, for lists several tabs share. */
export function cached<R = any>(path: string, ttl = 300000): Promise<R> {
  const k = `${session.token.slice(0, 10)}|${session.role}|${path}`;
  const hit = cache.get(k);
  if (hit && Date.now() - hit.at < ttl) return hit.p;
  const p = api<R>(path);
  p.catch(() => cache.delete(k));
  cache.set(k, { at: Date.now(), p });
  return p;
}
export const forget = (prefix: string) => { for (const k of [...cache.keys()]) if (k.split('|')[2].startsWith(prefix)) cache.delete(k); };

export type ScopeEmp = { id: string; name: string; type: string; department: string; corporate: boolean; designation: string; location_id: string; location: string; city: string; city_id: string; shift_code: string; doj: string };
/** Every active employee in the caller's data access (the prototype's scoped()). */
export async function scopeEmps(): Promise<ScopeEmp[]> {
  // A role without the employee directory (an employee's own view) sees only themselves, as the prototype's
  // scoped() does for access 'self'.
  if ((session.role || 'Employee') === 'Employee' || !(await canList())) {
    const me = await cached('/me');
    const e = me?.ctx?.employeeId ? await cached(`/employees/${me.ctx.employeeId}`) : null;
    return e && e.status === 'Active' ? [e as ScopeEmp] : [];
  }
  const first = await cached('/employees?status=Active&size=200&page=1');
  const pages = Math.ceil(first.total / 200);
  const rest = await Promise.all(Array.from({ length: Math.max(0, pages - 1) }, (_, i) => cached(`/employees?status=Active&size=200&page=${i + 2}`)));
  return [first, ...rest].flatMap((d) => d.rows);
}

/** Whether the current role view may list employees (the directory right). */
const canList = () => cached('/me').then((m: any) => (m?.ctx?.rights?.['emp.directory'] ?? 0) >= 1).catch(() => false);

/** Run fn over xs, n at a time. */
export async function each<X, R>(xs: X[], n: number, fn: (x: X) => Promise<R>): Promise<R[]> {
  const out: R[] = new Array(xs.length);
  let i = 0;
  await Promise.all(Array.from({ length: Math.min(n, xs.length) }, async () => { while (i < xs.length) { const j = i++; out[j] = await fn(xs[j]); } }));
  return out;
}
/** Every employee's day category over a range, in one request (the prototype's attDay for each day). */
type Sum = { days: string[]; ix: Map<string, number>; k: Map<string, string>; legend: Record<string, string> };
const summaryOf = (from: string, to: string): Promise<Sum> => cached<any>(`/attendance/summary?from=${from}&to=${to}`, 60000)
  .then((r) => ({ days: r.days, ix: new Map<string, number>(r.days.map((d: string, i: number) => [d, i])), k: new Map<string, string>(r.rows.map((x: any) => [x.id, x.k])), legend: r.legend }));
/** The category of one employee on one date, or null outside employment and in the future. */
const catOf = (s: Sum, id: string, d: string): string | null => { const i = s.ix.get(d), k = s.k.get(id); return i == null || !k ? null : s.legend[k[i]] ?? null; };

const ATCAT = ['Present', 'Late', 'Half day', 'Absent', 'Missing punch', 'On leave', 'Week off', 'Holiday'];
const ATKIND: Record<string, K> = { Present: 'ok', Late: 'warn', 'Half day': 'warn', Absent: 'bad', 'Missing punch': 'warn', 'On leave': 'info', 'Week off': 'n', Holiday: 'n' };
const ATLET: Record<string, string> = { Present: 'P', Late: 'LT', 'Half day': 'HD', Absent: 'AB', 'Missing punch': 'MP', 'On leave': 'LV', 'Week off': 'WO', Holiday: 'PH' };
/** The prototype's day category (attDay) from a day's status. */
function attCat(r?: { status?: string; tone?: string } | null): string | null {
  const s = r?.status?.replace(/ until regularized$/, ''); // a deviation allowance's action: "Half day until regularized" is a half day
  if (!s || s === 'Upcoming' || s === 'Before joining' || s === 'After last working day') return null;
  if (r!.tone === 'info') return 'On leave';
  if (['Present', 'Late', 'Half day', 'Week off', 'Holiday'].includes(s)) return s;
  if (/Missing/.test(s)) return 'Missing punch';
  return 'Absent';
}
function tally(ids: string[], dates: string[], s: Sum) {
  const c: Record<string, number> = Object.fromEntries(ATCAT.map((k) => [k, 0]));
  ids.forEach((id) => dates.forEach((d) => { const k = catOf(s, id, d); if (k) c[k]++; }));
  const w = c.Present + c.Late + c['Half day'], den = w + c.Absent + c['Missing punch'];
  return { c, rate: den ? Math.round((w / den) * 1000) / 10 : null };
}
const monthOpts = () => Array.from({ length: 12 }, (_, i) => addMonths(today().slice(0, 7), -i)).map((ym) => <option key={ym} value={ym}>{ymLabel(ym)}</option>);
const datesOf = (ym: string) => { const n = new Date(+ym.slice(0, 4), +ym.slice(5, 7), 0).getDate(); return Array.from({ length: n }, (_, i) => `${ym}-${String(i + 1).padStart(2, '0')}`); };

type Filt = { city: string; loc: string; dept: string; type: string; q: string };
const F0: Filt = { city: '', loc: '', dept: '', type: '', q: '' };
const match = (f: Filt) => (e: ScopeEmp) => (!f.city || e.city_id === f.city) && (!f.loc || e.location_id === f.loc) && (!f.dept || e.department === f.dept) && (!f.type || e.type === f.type)
  && (!f.q.trim() || e.name.toLowerCase().includes(f.q.trim().toLowerCase()) || e.id.toLowerCase().includes(f.q.trim().toLowerCase()));

// ---------- Page ----------

export default function Attendance() {
  const { can, me } = useAuth();
  // My view (the Employee role view): the person's own records only, with no filters or employee picker.
  const my = me?.ctx.role === 'Employee';
  const [tab, setTab] = useState<T>('sum');
  const [f, setF] = useState<Filt>(F0);
  const [view, setView] = useState<View>('emp');
  const [ym, setYm] = useState(recentYm());
  const [emp, setEmp] = useState('');
  const dir = useLoad(scopeEmps, []);
  const up = can('att.records', 2), cf = can('att.setup');
  const list = useMemo(() => (dir.data ?? []).filter(match(f)), [dir.data, f]);
  // With exactly one person in scope the employee picker is not needed: that person is chosen and the picker hidden.
  const solo = dir.data?.length === 1 ? dir.data[0].id : '';
  const pick = solo || emp;
  const tabs: [T, string][] = [['sum', 'Summary'], ['rec', 'Records'], ['exc', 'Missing punches'], ...(!my ? [['call', 'Called to work'] as [T, string]] : []), ...(up ? [['mis', 'MIS upload'] as [T, string]] : []), ...(cf ? [['api', 'Punch machine setup'] as [T, string]] : [])];
  const head = <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}</button>)}</div>;
  if (tab === 'mis' && up) return <>{head}<Mis /></>;
  if (tab === 'call' && !my) return <>{head}{!dir.data ? <Loading error={dir.error} /> : <CalledToWork list={dir.data} />}</>;
  if (tab === 'api' && cf) return <>{head}<Devices /></>;
  const bar = (extra?: ReactNode) => (my ? (extra ? <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>{extra}</div></div> : null) : <FilterBar f={f} setF={setF} extra={extra} />);
  const body = !dir.data ? <Loading error={dir.error} />
    : tab === 'sum' ? <Summary list={list} onCal={(id) => { setEmp(id); setView('cal'); setTab('rec'); }} />
    : tab === 'exc' ? <Missing list={list} ym={ym} />
    : view === 'date' ? (my ? <PeriodByDate /> : <ByDate list={list} />)
    : view === 'cal' ? <Calendar list={list} ym={ym} setYm={setYm} emp={pick} setEmp={setEmp} solo={!!solo} /> : <ByEmp list={list} ym={ym} setYm={setYm} emp={pick} setEmp={setEmp} solo={!!solo} />;
  return <>
    {head}
    {tab === 'rec' && <div className="seg" style={{ marginBottom: 14 }}>{([['emp', 'By employee'], ['date', 'By date'], ['cal', 'Calendar']] as [View, string][]).map(([k, l]) => <button key={k} className={view === k ? 'on' : ''} onClick={() => setView(k)}>{l}</button>)}</div>}
    {!(my && tab === 'rec') && bar(tab === 'exc' ? <Field label="Month"><select value={ym} onChange={(e) => setYm(e.target.value)}>{monthOpts()}</select></Field> : undefined)}
    {body}
  </>;
}

function FilterBar({ f, setF, extra }: { f: Filt; setF: (f: Filt) => void; extra?: ReactNode }) {
  const cities = useLoad(() => cached<any[]>('/masters/city?status=Active'), []);
  const locs = useLoad(() => cached<any[]>('/masters/location?status=Active'), []);
  const depts = useLoad(() => cached<any[]>('/masters/department?status=Active'), []);
  const set = (k: keyof Filt, v: string) => setF({ ...f, [k]: v, ...(k === 'city' ? { loc: '' } : {}) });
  return <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
    <Field label="City"><select value={f.city} onChange={(e) => set('city', e.target.value)}><option value="">All cities</option>{cities.data?.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select></Field>
    <Field label="Location"><select value={f.loc} onChange={(e) => set('loc', e.target.value)}><option value="">All locations</option>{locs.data?.filter((l) => !f.city || l.city_id === f.city).map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select></Field>
    <Field label="Department"><select value={f.dept} onChange={(e) => set('dept', e.target.value)}><option value="">All departments</option>{depts.data?.map((x) => <option key={x.id}>{x.name}</option>)}</select></Field>
    <Field label="Type"><select value={f.type} onChange={(e) => set('type', e.target.value)}><option value="">All types</option>{['Permanent', 'Retainer', 'Contractual'].map((x) => <option key={x}>{x}</option>)}</select></Field>
    <Field label="Search"><input type="text" value={f.q} onChange={(e) => set('q', e.target.value)} placeholder="Name or code" /></Field>
    {extra}<button className="btn sm" onClick={() => setF(F0)}>Clear filters</button></div></div>;
}

// ---------- Summary ----------

function Summary({ list, onCal }: { list: ScopeEmp[]; onCal: (id: string) => void }) {
  const y = addDays(today(), -1);
  const mtd = useMemo(() => { const o: string[] = []; for (let d = today().slice(0, 8) + '01'; d < today(); d = addDays(d, 1)) o.push(d); return o; }, []);
  const days = useLoad(() => summaryOf(mtd.length ? mtd[0] : y, y), [y, mtd.length]);
  const [grp, setGrp] = useState<'loc' | 'dept' | 'city'>('loc');
  if (!days.data) return <Loading error={days.error} />;
  const D = days.data, ids = list.map((e) => e.id), cy = tally(ids, [y], D), cm = tally(ids, mtd, D);
  const dn = new Date(y + 'T00:00:00').toLocaleDateString('en-IN', { weekday: 'long', day: 'numeric', month: 'short' });
  const kpi = (t: ReturnType<typeof tally>, title: string, sub: string) => <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>{title}</h3><span className="mute" style={{ fontSize: 12.5 }}>{sub}</span></div>
    <div className="grid g4" style={{ gap: 10 }}>{ATCAT.map((k) => <div key={k} className="card stat" style={{ margin: 0 }}><div className="l">{k}</div><div className="n">{t.c[k]}</div></div>)}
      <div className="card stat" style={{ margin: 0 }}><div className="l">Attendance rate</div><div className="n">{t.rate == null ? '—' : t.rate + '%'}</div></div></div></div>;
  const key = (e: ScopeEmp) => (grp === 'loc' ? e.location : grp === 'dept' ? e.department : e.city);
  const groups: Record<string, string[]> = {};
  list.forEach((e) => (groups[key(e)] = groups[key(e)] || []).push(e.id));
  const gl = grp === 'loc' ? 'Location' : grp === 'dept' ? 'Department' : 'City';
  const worst = list.map((e) => { let a = 0, m = 0, l = 0; mtd.forEach((d) => { const k = catOf(D, e.id, d); if (k === 'Absent') a++; else if (k === 'Missing punch') m++; else if (k === 'Late') l++; }); return { e, a, m, l }; })
    .filter((x) => x.a || x.m || x.l).sort((x, z) => (z.a * 3 + z.m * 2 + z.l) - (x.a * 3 + x.m * 2 + x.l)).slice(0, 8);
  return <>
    {kpi(cy, 'Yesterday', `${dn} · ${list.length} employees`)}
    {kpi(cm, 'Month to date', mtd.length ? `${fmtD(mtd[0])} to ${fmtD(mtd[mtd.length - 1])}` : 'No complete day yet')}
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>By {gl.toLowerCase()}</h3><select value={grp} onChange={(e) => setGrp(e.target.value as any)}><option value="loc">Location</option><option value="dept">Department</option><option value="city">City</option></select></div>
      <div className="tw"><table><thead><tr><th>{gl}</th><th className="num">Employees</th><th className="num">Present yesterday</th><th className="num">Half day</th><th className="num">Absent</th><th className="num">Missing</th><th className="num">On leave</th><th className="num">Rate yesterday</th><th className="num">Rate MTD</th><th className="num">Absent days MTD</th></tr></thead><tbody>
        {Object.entries(groups).sort((a, b) => (a[0] < b[0] ? -1 : 1)).map(([k, g]) => { const a = tally(g, [y], D), b = tally(g, mtd, D); return <tr key={k}><td><b>{k}</b></td><td className="num">{g.length}</td><td className="num">{a.c.Present + a.c.Late}</td><td className="num">{a.c['Half day']}</td>
          <td className="num">{a.c.Absent ? <b style={{ color: 'var(--bad)' }}>{a.c.Absent}</b> : 0}</td><td className="num">{a.c['Missing punch']}</td><td className="num">{a.c['On leave']}</td><td className="num">{a.rate == null ? '—' : a.rate + '%'}</td><td className="num">{b.rate == null ? '—' : b.rate + '%'}</td><td className="num">{b.c.Absent}</td></tr>; })}
        {!list.length && <tr><td colSpan={10} className="mute">No employees match the filters.</td></tr>}
      </tbody></table></div></div>
    <div className="card"><h3>Needs attention this month</h3><p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Most absent days, missing punches and late marks so far this month, for the filtered employees.</p>
      <div className="tw"><table><thead><tr><th>Employee</th><th>Location</th><th className="num">Absent</th><th className="num">Missing punch</th><th className="num">Late</th><th></th></tr></thead><tbody>
        {worst.map((x) => <tr key={x.e.id}><td><b>{x.e.name}</b><div className="mute" style={{ fontSize: 12 }}>{x.e.id} · {x.e.department}</div></td><td>{x.e.location}</td><td className="num">{x.a}</td><td className="num">{x.m}</td><td className="num">{x.l}</td><td><button className="btn sm" onClick={() => onCal(x.e.id)}>Calendar</button></td></tr>)}
        {!worst.length && <tr><td colSpan={6} className="mute">Nothing to flag.</td></tr>}
      </tbody></table></div></div>
  </>;
}

// ---------- Records: by employee, by date, calendar ----------

const EmpSel = ({ list, emp, setEmp, all, hide }: { list: ScopeEmp[]; emp: string; setEmp: (s: string) => void; all: string; hide?: boolean }) => hide ? null :
  <Field label="Employee"><PersonPicker label="Employee" people={list} value={list.some((e) => e.id === emp) ? emp : ''} onChange={(id) => setEmp(id)} empty={all} style={{ minWidth: 260 }} /></Field>;
/** One employee's month (finalAtt): days up to today, from joining, with leave shown as On leave. */
const monthOf = (id: string, ym: string) => api(`/attendance/month/${id}?ym=${ym}`);
const shownDays = (m: any) => (m?.month.days ?? []).filter((d: any) => d.date <= today() && !['Upcoming', 'Before joining', 'After last working day'].includes(d.status));
/** The month's called-to-work days and deviation allowances (owner, 24 Nov 2026), as pills when there are any. */
export const extraPills = (c: Record<string, number>) => (['Called to work', 'Late check-in allowed', 'Early check-in allowed', 'Early check-out allowed', 'Beyond the deviation allowance',
  'Absent until regularized', 'Half day until regularized', 'Present until regularized'] as const)
  .filter((k) => c[k]).map((k) => <Pill key={k} k={k.startsWith('Beyond') || k.endsWith('regularized') ? 'warn' : 'info'}>{c[k]} {k.toLowerCase()}</Pill>);

function ByEmp({ list, ym, setYm, emp, setEmp, solo }: { list: ScopeEmp[]; ym: string; setYm: (s: string) => void; emp: string; setEmp: (s: string) => void; solo?: boolean }) {
  const one = list.find((e) => e.id === emp);
  const all = useLoad(() => (one ? Promise.resolve(null) : cached<any[]>(`/attendance?ym=${ym}`, 60000)), [ym, !!one]);
  const det = useLoad(() => (one ? monthOf(one.id, ym) : Promise.resolve(null)), [ym, one?.id]);
  const ids = new Set(list.map((e) => e.id)), rows = (all.data ?? []).filter((r) => ids.has(r.id));
  const c = det.data?.month.counts ?? {};
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
      <Field label="Month"><select value={ym} onChange={(e) => setYm(e.target.value)}>{monthOpts()}</select></Field><EmpSel list={list} emp={emp} setEmp={setEmp} all="All employees" hide={solo} /></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Source: punch machines, mobile app and MIS uploads. A later upload replaces the same day.</p></div>
    {!one && (!all.data ? <Loading error={all.error} /> : <div className="card"><div className="tw"><table><thead><tr><th>Employee</th><th>Location</th><th className="num">Present</th><th className="num">Late</th><th className="num">Half day</th><th className="num">Missing punch</th><th className="num">No punch</th><th></th></tr></thead><tbody>
      {rows.slice(0, 150).map((r) => <tr key={r.id}><td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.id} · {r.type}</div></td><td>{r.location}</td>
        <td className="num">{r.counts.Present || 0}</td><td className="num">{r.counts.Late || 0}</td><td className="num">{r.counts['Half day'] || 0}</td>
        <td className="num">{r.counts['Missing punch'] ? <b style={{ color: 'var(--warn)' }}>{r.counts['Missing punch']}</b> : 0}</td><td className="num">{r.counts['No punch'] || 0}</td>
        <td><button className="btn sm" onClick={() => setEmp(r.id)}>Open</button></td></tr>)}
      {!rows.length && <tr><td colSpan={8} className="mute">No employees match the filters.</td></tr>}
    </tbody></table></div>{rows.length > 150 && <p className="mute" style={{ fontSize: 12 }}>Showing 150 of {rows.length}.</p>}</div>)}
    {one && (!det.data ? <Loading error={det.error} /> : <>
      <div className="row" style={{ gap: 8, marginBottom: 12 }}><Pill k="ok">{c.Present || 0} present</Pill><Pill k="warn">{c.Late || 0} late</Pill><Pill k="warn">{c['Half day'] || 0} half day</Pill>
        <Pill k={c['Missing punch'] ? 'warn' : 'n'}>{c['Missing punch'] || 0} missing punch</Pill><Pill k={c['No punch'] ? 'bad' : 'n'}>{c['No punch'] || 0} no punch</Pill>{extraPills(c)}</div>
      <div className="card"><div className="tw"><table><thead><tr><th>Date</th><th>Day</th><th>In</th><th>Out</th><th className="num">Hours</th><th>Source</th><th>Status</th><th>Note</th></tr></thead><tbody>
        {shownDays(det.data).reverse().map((d: any) => { const lv = attCat(d) === 'On leave'; return <tr key={d.date}><td>{fmtD(d.date)}</td><td className="mute">{wd(d.date)}</td><td className="mono">{d.in || '—'}</td><td className="mono">{d.out || '—'}</td>
          <td className="num">{hrs(d.in, d.out)}</td><td className="mute">{d.src || '—'}</td><td><Pill k={lv ? 'info' : tone(d.tone)}>{lv ? 'On leave' : d.status}</Pill>{d.called && <> <Pill k="info">Called to work</Pill></>}</td>
          <td className="mute" style={{ fontSize: 12 }}>{d.note || ''}</td></tr>; })}
      </tbody></table></div></div>
    </>)}
  </>;
}

function ByDate({ list }: { list: ScopeEmp[] }) {
  const [d, setD] = useState(addDays(today(), -1));
  const [st, setSt] = useState('');
  const { data, error } = useLoad(() => cached(`/attendance/day?date=${d}`, 60000), [d]);
  const by = new Map(list.map((e) => [e.id, e]));
  const rows = (data?.rows ?? []).filter((r: any) => by.has(r.id)).map((r: any) => ({ r, k: (r.cat as string | undefined) ?? attCat(r) })).filter((x: any) => x.k);
  const cnt: Record<string, number> = {};
  rows.forEach((x: any) => (cnt[x.k] = (cnt[x.k] || 0) + 1));
  const shown = rows.filter((x: any) => !st || x.k === st);
  const move = (n: number) => { const x = addDays(d, n); setD(x > today() ? today() : x); };
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
      <Field label="Date"><input type="date" value={d} max={today()} onChange={(e) => e.target.value && setD(e.target.value)} /></Field>
      <button className="btn sm" onClick={() => move(-1)}>Previous day</button><button className="btn sm" onClick={() => move(1)}>Next day</button></div>
      <div className="row" style={{ flexWrap: 'wrap', gap: 8, marginTop: 12 }}><button className={`btn sm ${st ? '' : 'primary'}`} onClick={() => setSt('')}>All {rows.length}</button>
        {ATCAT.filter((k) => cnt[k]).map((k) => <button key={k} className={`btn sm ${st === k ? 'primary' : ''}`} onClick={() => setSt(k)}>{k} {cnt[k]}</button>)}</div></div>
    {!data ? <Loading error={error} /> : <div className="card"><div className="tw"><table><thead><tr><th>Employee</th><th>Location</th><th>Department</th><th>Shift</th><th>In</th><th>Out</th><th className="num">Hours</th><th>Source</th><th>Status</th></tr></thead><tbody>
      {shown.slice(0, 200).map(({ r, k }: any) => <tr key={r.id}><td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.id}</div></td><td>{r.location}</td><td>{r.department}</td><td className="mono">{r.planned}</td>
        <td className="mono">{r.in || '—'}</td><td className="mono">{r.out || '—'}</td><td className="num">{hrs(r.in, r.out)}</td><td className="mute">{r.src || '—'}</td><td><Pill k={ATKIND[k]}>{k}</Pill></td></tr>)}
      {!shown.length && <tr><td colSpan={9} className="mute">No records.</td></tr>}
    </tbody></table></div>{shown.length > 200 && <p className="mute" style={{ fontSize: 12 }}>Showing 200 of {shown.length}. Use the filters to narrow down.</p>}</div>}
  </>;
}

/** My view › Records › By date: the employee's own days over a period, one row per date, in the By date columns. */
function PeriodByDate() {
  const t = today(), first = t.slice(0, 8) + '01';
  const [p, setP] = useState({ from: first, to: t });
  const [st, setSt] = useState('');
  const { data, error } = useLoad(() => api(`/attendance/day?from=${p.from}&to=${p.to}`), [p.from, p.to]);
  const rows = (data?.rows ?? []).map((r: any) => ({ r, k: (r.cat as string | undefined) ?? attCat(r) })).filter((x: any) => x.k);
  const cnt: Record<string, number> = {};
  rows.forEach((x: any) => (cnt[x.k] = (cnt[x.k] || 0) + 1));
  const shown = rows.filter((x: any) => !st || x.k === st);
  const set = (k: 'from' | 'to', v: string) => { if (!v) return; const n = { ...p, [k]: v }; if (n.from > n.to) { if (k === 'from') n.to = v; else n.from = v; } setP(n); setSt(''); };
  const last = addMonths(t.slice(0, 7), -1);
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
      <Field label="From"><input type="date" value={p.from} max={t} onChange={(e) => set('from', e.target.value)} /></Field>
      <Field label="To"><input type="date" value={p.to} max={t} onChange={(e) => set('to', e.target.value)} /></Field>
      <button className="btn sm" onClick={() => { setP({ from: first, to: t }); setSt(''); }}>This month</button>
      <button className="btn sm" onClick={() => { setP({ from: last + '-01', to: addDays(first, -1) }); setSt(''); }}>Last month</button></div>
      <div className="row" style={{ flexWrap: 'wrap', gap: 8, marginTop: 12 }}><button className={`btn sm ${st ? '' : 'primary'}`} onClick={() => setSt('')}>All {rows.length}</button>
        {ATCAT.filter((k) => cnt[k]).map((k) => <button key={k} className={`btn sm ${st === k ? 'primary' : ''}`} onClick={() => setSt(k)}>{k} {cnt[k]}</button>)}</div></div>
    {!data ? <Loading error={error} /> : <div className="card"><div className="tw"><table><thead><tr><th>Date</th><th>Location</th><th>Department</th><th>Shift</th><th>In</th><th>Out</th><th className="num">Hours</th><th>Source</th><th>Status</th></tr></thead><tbody>
      {shown.map(({ r, k }: any) => <tr key={r.date}><td style={{ whiteSpace: 'nowrap' }}><b>{fmtD(r.date)}</b><div className="mute" style={{ fontSize: 12 }}>{wd(r.date)}</div></td><td>{r.location}</td><td>{r.department}</td><td className="mono">{r.planned}</td>
        <td className="mono">{r.in || '—'}</td><td className="mono">{r.out || '—'}</td><td className="num">{hrs(r.in, r.out)}</td><td className="mute">{r.src || '—'}</td><td><Pill k={ATKIND[k]}>{k}</Pill></td></tr>)}
      {!shown.length && <tr><td colSpan={9} className="mute">No records.</td></tr>}
    </tbody></table></div></div>}
  </>;
}

function Calendar({ list, ym, setYm, emp, setEmp, solo }: { list: ScopeEmp[]; ym: string; setYm: (s: string) => void; emp: string; setEmp: (s: string) => void; solo?: boolean }) {
  const one = list.find((e) => e.id === emp);
  const days = datesOf(ym), past = days.filter((d) => d <= today());
  const det = useLoad(() => (one ? monthOf(one.id, ym) : Promise.resolve(null)), [ym, one?.id]);
  const grid = useLoad(() => (one || !past.length ? Promise.resolve(null) : summaryOf(past[0], past[past.length - 1])), [ym, !!one]);
  const head = <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
    <Field label="Month"><select value={ym} onChange={(e) => setYm(e.target.value)}>{monthOpts()}</select></Field><EmpSel list={list} emp={emp} setEmp={setEmp} all="All employees (grid)" hide={solo} /></div></div>;
  if (one) {
    if (!det.data) return <>{head}<Loading error={det.error} /></>;
    const by = new Map<string, any>(det.data.month.days.map((d: any) => [d.date, d]));
    const c: Record<string, number> = {};
    let w = 0, den = 0;
    past.forEach((d) => { const k = attCat(by.get(d)); if (k) { c[k] = (c[k] || 0) + 1; if (['Present', 'Late', 'Half day'].includes(k)) { w++; den++; } else if (k === 'Absent' || k === 'Missing punch') den++; } });
    const rate = den ? Math.round((w / den) * 1000) / 10 : null;
    const first = (new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).getDay() + 6) % 7, cells: string[] = [...Array(first).fill(''), ...days];
    while (cells.length % 7) cells.push('');
    return <>{head}
      <div className="row" style={{ gap: 8, marginBottom: 12 }}>{ATCAT.filter((k) => c[k]).map((k) => <Pill key={k} k={ATKIND[k]}>{c[k]} {k.toLowerCase()}</Pill>)}{rate != null && <Pill k="info">Attendance rate {rate}%</Pill>}</div>
      <div className="card"><div style={{ display: 'grid', gridTemplateColumns: 'repeat(7,minmax(0,1fr))', gap: 6 }}>
        {['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'].map((x) => <div key={x} className="mute" style={{ fontSize: 12, textAlign: 'center' }}>{x}</div>)}
        {cells.map((d, i) => { if (!d) return <div key={'b' + i} />; const r = by.get(d), k = d > today() ? null : attCat(r); return <div key={d} style={{ border: '1px solid var(--line)', borderRadius: 8, padding: 6, minHeight: 78, fontSize: 12 }}>
          <div style={{ fontWeight: 600 }}>{+d.slice(8)}</div>
          {k ? <><div style={{ margin: '4px 0' }}><Pill k={ATKIND[k]}>{k}</Pill></div>{(r?.in || r?.out) && <div className="mono mute" style={{ fontSize: 11 }}>{r.in || '—'} – {r.out || '—'}</div>}</> : <div className="mute" style={{ fontSize: 11 }}>{d > today() ? '' : '—'}</div>}</div>; })}
      </div></div></>;
  }
  const es = list.slice(0, 60);
  return <>{head}
    {!grid.data && past.length ? <Loading error={grid.error} /> : <div className="card"><div className="row sp" style={{ marginBottom: 10 }}><span className="mute" style={{ fontSize: 12.5 }}>{ATCAT.map((k) => ATLET[k] + ' = ' + k).join(' · ')}</span></div>
      <div className="tw"><table className="roster"><thead><tr><th style={{ textAlign: 'left' }}>Employee</th>{days.map((d) => <th key={d} style={{ fontSize: 11 }}>{+d.slice(8)}</th>)}</tr></thead><tbody>
        {es.map((e) => <tr key={e.id}><td className="nm"><b>{e.name}</b><div className="mute" style={{ fontSize: 11 }}>{e.id}</div></td>
          {days.map((d) => { const k = d > today() || !grid.data ? null : catOf(grid.data, e.id, d); return <td key={d} style={{ padding: 2 }}>{k && <span className={`pill ${ATKIND[k]}`} title={k} style={{ fontSize: 10, padding: '2px 4px' }}>{ATLET[k]}</span>}</td>; })}</tr>)}
      </tbody></table></div>{list.length > 60 && <p className="mute" style={{ fontSize: 12 }}>Showing 60 of {list.length}. Use the filters to narrow down.</p>}</div>}
  </>;
}

// ---------- Missing punches ----------

function Missing({ list, ym }: { list: ScopeEmp[]; ym: string }) {
  const { can, me } = useAuth();
  // In My view the employee regularizes their own days here too (Approvals is not in My view).
  const mine = (id: string) => me?.ctx.role === 'Employee' && id === me.ctx.employeeId;
  const [reg, setReg] = useState<any>(null);
  const { data, error, reload } = useLoad(() => api<any[]>(`/attendance/issues?ym=${ym}`), [ym]);
  const ids = new Set(list.map((e) => e.id)), rows = (data ?? []).filter((r) => ids.has(r.id));
  if (!data) return <Loading error={error} />;
  return <>
    <div className="card"><p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>{rows.length} days need attention</p><div className="tw"><table><thead><tr><th>Employee</th><th>Date</th><th>In</th><th>Out</th><th>Source</th><th>Issue</th><th></th></tr></thead><tbody>
      {rows.slice(0, 80).map((r) => <tr key={r.id + r.date}><td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.id}</div></td><td>{fmtD(r.date)}</td>
        <td className="mono">{r.in || '—'}</td><td className="mono">{r.out || '—'}</td><td className="mute">{r.src || '—'}</td><td><Pill k={tone(r.tone)}>{r.status}</Pill></td>
        <td>{r.regPending ? <Pill k="info">Request waiting for approval</Pill> : (can('appr.raise', 2) || mine(r.id)) && <button className="btn sm" onClick={() => setReg({ employeeId: r.id, name: r.name, date: r.date, rec: r })}>Regularize</button>}</td></tr>)}
      {!rows.length && <tr><td colSpan={7} className="mute">No missing punches.</td></tr>}
    </tbody></table></div>{rows.length > 80 && <p className="mute" style={{ fontSize: 12 }}>Showing the latest 80 of {rows.length}.</p>}</div>
    {reg && <RegularizeModal {...reg} emps={list} onClose={() => { setReg(null); reload(); }} />}
  </>;
}

// ---------- Regularization request (prototype nrModal 'regularize') ----------

/** Raise a regularization for someone (or yourself) for one date (the prototype's new request dialog for
 * 'regularize'). `emps` is the list the employee can be picked from; without it only the given person is offered. */
export function RegularizeModal({ employeeId, name, date, rec, emps, onClose }: { employeeId: string; name: string; date: string; rec?: any; emps?: { id: string; name: string }[]; onClose: () => void }) {
  const { me } = useAuth();
  const [emp, setEmp] = useState(employeeId);
  const [v, setV] = useState({ date, rtype: rec?.in && rec?.out ? 'All Other' : 'No Punch', in: '09:00', out: '18:00', reason: REG_REASONS[0], note: '' });
  const [err, setErr] = useState<string[]>([]);
  const [route, setRoute] = useState<any>(null);
  const self = emp === me?.ctx.employeeId;
  const body = { wf: 'regularize', employeeId: self ? undefined : emp, payload: v };
  const sys = useLoad(() => (v.date === date && emp === employeeId ? Promise.resolve(rec ?? null) : api(`/attendance/month/${self ? 'me' : emp}?ym=${v.date.slice(0, 7)}`).then((m) => m.month.days.find((d: any) => d.date === v.date) ?? null)), [v.date, emp]);
  useEffect(() => {
    // Everyone sees the route of their own request; a role that may not preview it gets a 403 and no route block.
    setErr([]);
    const t = setTimeout(() => api('/approvals/simulate', { body: { ...body, employeeId: emp } }).then(setRoute).catch(() => setRoute(null)), 250);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(v), emp]);
  const send = async (skip = '') => {
    setErr([]);
    try { const r = await api('/approvals' + skip, { body }); toast(skip ? `${r.id} approved: approval skipped` : `${r.id} sent for approval`); onClose(); } catch (x: any) { setErr((x.details as string[]) || [x.message]); }
  };
  const set = (k: keyof typeof v, x: string) => setV({ ...v, [k]: x });
  const s = sys.data;
  const people = emps?.length ? emps : [{ id: employeeId, name }];
  // What would stop the request (nrErrors): the server's checks shown as the form changes, or the refusal after sending.
  const problems: string[] = err.length ? err : route?.problems ?? [];
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>New attendance regularization request</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div style={{ marginTop: 12 }}><div className="form">
      <Field label="Employee"><PersonPicker label="Employee" people={people} value={emp} onChange={(id) => id && setEmp(id)} required /></Field>
      <Field label="Date"><input type="date" value={v.date} max={today()} onChange={(e) => e.target.value && set('date', e.target.value)} /></Field>
      <Field label="Reason"><select value={v.reason} onChange={(e) => set('reason', e.target.value)}>{REG_REASONS.map((r) => <option key={r}>{r}</option>)}</select></Field>
      <Field label="What needs correcting"><select value={v.rtype} onChange={(e) => set('rtype', e.target.value)}><option value="No Punch">No punch (missing in or out)</option><option value="All Other">Late, early or wrong time</option></select></Field>
      <Field label="In time"><input type="time" value={v.in} onChange={(e) => set('in', e.target.value)} /></Field>
      <Field label="Out time"><input type="time" value={v.out} onChange={(e) => set('out', e.target.value)} /></Field>
      <label className="field full"><span>Note</span><input type="text" value={v.note} onChange={(e) => set('note', e.target.value)} /></label>
    </div></div>
    <div className="card" style={{ marginTop: 14 }}>
      <h3>System record for {fmtD(v.date)}</h3>
      <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>In <b>{s?.in || '—'}</b> · Out <b>{s?.out || '—'}</b> · {s?.src || 'No data'} {s?.status && <Pill k={tone(s.tone)}>{s.status}</Pill>}</p>
      {route && <><h3>Approval route</h3>
        {route.chain?.length ? <Chain steps={route.chain} /> : <span className="mute">No approver is needed. It is approved straight away.</span>}
        {route.people?.length > 0 && <div className="mute" style={{ fontSize: 12, marginTop: 6 }}>{route.people.map((x: any) => `${x.stage}: ${x.name}`).join(' · ')}</div>}
        {route.notify?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Will be informed: {route.notify.map((n: any) => `${n.role} (${n.person})`).join(', ')}</p>}
        {route.why?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Added because: {route.why.join(', ')}</p>}</>}
      {problems.length > 0 && <div className="note bad" style={{ marginTop: 8 }}><ul style={{ margin: 0, paddingLeft: 18 }}>{problems.map((x) => <li key={x}>{x}</li>)}</ul></div>}
    </div>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><SkipApprovalButton wf="regularize" subject={emp} onSkip={(q) => send(q)} /><button className="btn primary" onClick={() => send()}>Send for approval</button></div>
  </Modal>;
}

// ---------- MIS upload ----------

const MISH = 'Employee ID,Date,In time,Out time';
const whenT = (at: string) => new Date(at).toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' });

function Mis() {
  const [text, setText] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [res, setRes] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const log = useLoad(() => api<any[]>('/bulk/att/log'), []);
  const src = () => file ?? new File([text], 'attendance.csv', { type: 'text/csv' });
  const run = async (action: 'check' | 'apply') => {
    setBusy(true);
    try {
      const r = await postForm(`/bulk/att/${action}`, {}, src());
      if (action === 'apply') {
        toast(`${r.ok} rows applied`); setText(''); setFile(null); setRes(null); log.reload(); forget('/attendance');
      } else setRes(r);
    } catch (x: any) { toast(x.message); setRes(null); } finally { setBusy(false); }
  };
  const pick = async (f?: File) => {
    if (!f) return;
    setRes(null);
    if (/\.csv$/i.test(f.name)) { setText(await f.text()); setFile(null); } else { setFile(f); setText(''); }
  };
  const lines = text.split(/\r?\n/);
  const cell = (line: number, i: number) => (lines[line - 1] || '').split(',')[i]?.trim() || '';
  const ok = res?.ok ?? 0;
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Use this until the punch machines are connected. Upload one row per employee per day: <span className="mono">{MISH}</span>. Leave a time blank if it is missing. Rows for a day that already has a record replace it.</div>
    <div className="card"><div className="row" style={{ marginBottom: 12 }}>
      <button className="btn" onClick={() => download('/bulk/att/download', 'attendance.xlsx').catch((e) => toast(e.message))}>Download template</button>
      <label className="btn">Choose Excel or CSV file<input type="file" accept=".xlsx,.csv,text/csv" hidden onChange={(e) => { pick(e.target.files?.[0]); e.target.value = ''; }} /></label>
      {file && <span className="mute" style={{ fontSize: 12.5 }}>{file.name}</span>}</div>
      <textarea rows={8} style={{ width: '100%', fontFamily: 'ui-monospace,monospace', fontSize: 12.5 }} placeholder={MISH} value={text} disabled={!!file} onChange={(e) => { setText(e.target.value); setRes(null); }} />
      <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={busy || (!file && !text.trim())} onClick={() => run('check')}>Check rows</button>
        {res && ok > 0 && <button className="btn primary" disabled={busy} onClick={() => run('apply')}>Apply {ok} row{ok > 1 ? 's' : ''}</button>}</div>
      {res && <div className="tw" style={{ marginTop: 14 }}><table><thead><tr><th>#</th><th>Employee</th><th>Date</th><th>In</th><th>Out</th><th>Result</th></tr></thead><tbody>
        {res.results.map((r: any) => { const ch = r.changes[0], io = ch?.to?.split(' to ') ?? []; return <tr key={r.line}><td>{r.line}</td><td>{r.name || '—'} <span className="mute mono">{r.id}</span></td>
          <td>{file ? (ch?.field.replace('Punches on ', '') ?? '') : cell(r.line, 1)}</td><td className="mono">{(file ? io[0] : cell(r.line, 2)) || '—'}</td><td className="mono">{(file ? io[1] : cell(r.line, 3)) || '—'}</td>
          <td>{r.errors.length ? <Pill k="bad">{r.errors.join('. ')}</Pill> : <Pill k="ok">{ch?.note === 'replaces the day' ? 'Replaces existing record' : 'New record'}</Pill>}</td></tr>; })}
      </tbody></table></div>}</div>
    <div className="card" style={{ marginTop: 14 }}><h3>Recent uploads</h3><ul className="log">{(log.data ?? []).map((l, i) => <li key={i}><div>{l.rows} attendance row{l.rows === 1 ? '' : 's'} uploaded by {String(l.by).replace(/ \(EMP-[^)]+\)$/, '')}</div><div className="mute" style={{ fontSize: 12 }}>{whenT(l.at)}</div></li>)}{log.data && !log.data.length && <li className="mute">No uploads yet.</li>}</ul></div>
  </>;
}

// ---------- Punch machine setup (spec 11.2, prototype vApi): pull punches from the office server's API; machines can also push ----------

type Pull = { url: string; method: string; auth: string; key: string; since: string; every: number | string; empF: string; timeF: string; dirF: string; inV: string; outV: string };
const PULL_KEYS = ['url', 'method', 'auth', 'since', 'every', 'empF', 'timeF', 'dirF', 'inV', 'outV'] as const;

function Devices() {
  const { can } = useAuth();
  const ed = can('att.setup', 2);
  const d = useLoad(() => api<any[]>('/devices'), []);
  const cfg = useLoad(() => api('/attendance/pull-config'), []);
  const locs = useLoad(() => cached<any[]>('/masters/location?status=Active'), []);
  const [a, setA] = useState<Pull | null>(null);
  const [test, setTest] = useState<any>(null);
  const [busy, setBusy] = useState('');
  const [add, setAdd] = useState<any>(null);
  const [key, setKey] = useState<any>(null);
  useEffect(() => { if (cfg.data && !a) setA({ ...(Object.fromEntries(PULL_KEYS.map((k) => [k, cfg.data[k] ?? ''])) as any), key: '' }); }, [cfg.data, a]);
  const save = async () => { try { setKey(await api('/devices', { body: add })); setAdd(null); d.reload(); } catch (e: any) { toast(e.message); } };
  const act = async (id: number, x: string, body?: any) => { try { const r = await api(`/devices/${id}/${x}`, { body: body ?? {} }); if (r?.key) setKey(r); else toast('Done'); d.reload(); } catch (e: any) { toast(e.message); } };
  const on = (d.data ?? []).filter((x) => x.status === 'Active');
  const pullBody = () => ({ ...Object.fromEntries(PULL_KEYS.map((k) => [k, a![k]])), every: +a!.every || 0, key: a!.key.trim() || undefined });
  const apiSave = async () => {
    setBusy('save');
    try { await api('/attendance/pull-config', { method: 'PUT', body: pullBody() }); toast('Settings saved'); setA({ ...a!, key: '' }); cfg.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const apiTest = async () => {
    setBusy('test');
    try { setTest(await api('/attendance/pull-config/test', { body: pullBody() })); cfg.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const pullNow = async () => {
    setBusy('pull');
    try { const r = await api('/attendance/pull-config/pull', { method: 'POST' }); toast(r.last_pull_ok ? `Pulled: ${r.last_pull_note}` : `Pull failed: ${r.last_pull_note}`); cfg.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const connected = cfg.data?.test_ok === true || cfg.data?.last_pull_ok === true || on.some((x) => x.last_seen);
  const f = (k: keyof Pull, label: string, ph = '', type = 'text') => <Field label={label}><input type={type} value={a ? String(a[k]) : ''} placeholder={ph} disabled={!ed}
    onChange={(e) => setA({ ...a!, [k]: e.target.value })} /></Field>;
  const sel = (k: keyof Pull, label: string, opts: string[]) => <Field label={label}><select value={a ? String(a[k]) : ''} disabled={!ed} onChange={(e) => setA({ ...a!, [k]: e.target.value })}>{opts.map((o) => <option key={o}>{o}</option>)}</select></Field>;
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Your punch machines send data to the server in your office, and that server exposes it through a static API. Once this is set up, the system pulls punches on a schedule and fills the Records tab automatically. MIS upload stays available as a fallback.</div>
    <RealtimePortal ed={ed} />
    <RealtimeMatch ed={ed} />
    <MachinePool ed={ed} />
    {can('att.portal') && <PortalSend />}
    <AttAlerts ed={ed} />
    <div className="grid split"><div className="card"><div className="row sp"><h3 style={{ margin: 0 }}>Connection</h3><Pill k={connected ? 'ok' : 'warn'}>{connected ? 'Connected' : 'Not connected'}</Pill></div>
      {!a ? <Loading error={cfg.error} /> : <>
        <div className="form" style={{ marginTop: 12 }}>{f('url', 'Server API address', 'http://203.0.113.10:8080/api/punches')}{sel('method', 'Request method', ['GET', 'POST'])}
          {sel('auth', 'Authentication', cfg.data?.auths ?? ['API key', 'Bearer token', 'Username and password', 'None'])}{f('key', 'Key or token', cfg.data?.keySet ? 'Saved. Type a new one to replace it' : '', 'password')}
          {f('since', 'Query parameter for “from time”', 'from')}{f('every', 'Pull every (minutes)', '', 'number')}</div>
        <h3 style={{ margin: '16px 0 8px', fontSize: 13 }}>Field mapping</h3>
        <div className="form">{f('empF', 'Employee code field', 'emp_code')}{f('timeF', 'Punch time field', 'punch_time')}{f('dirF', 'In or out field', 'punch_type')}{f('inV', 'Value that means IN', 'IN')}{f('outV', 'Value that means OUT', 'OUT')}</div>
        {ed && <div className="row" style={{ marginTop: 16 }}><button className="btn primary" disabled={!!busy} onClick={apiSave}>Save settings</button><button className="btn" disabled={!!busy || !a.url.trim()} onClick={apiTest}>{busy === 'test' ? 'Testing…' : 'Test connection'}</button>
          {cfg.data?.url && <button className="btn" disabled={!!busy} onClick={pullNow}>{busy === 'pull' ? 'Pulling…' : 'Pull now'}</button>}</div>}
        {cfg.data?.url && <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>{cfg.data.last_pull_at
          ? <>Last pull {whenT(cfg.data.last_pull_at)}: <span style={{ color: cfg.data.last_pull_ok ? undefined : 'var(--bad)' }}>{cfg.data.last_pull_note}</span>. Punches are pulled every {cfg.data.every} minutes.</>
          : `Punches are pulled every ${cfg.data.every} minutes once the address is saved; the first pull asks for the last 24 hours.`}</p>}
        {test ? <div className={`note ${test.ok ? 'ok' : 'bad'}`} style={{ marginTop: 12 }}>{test.note}{test.status ? ` (HTTP ${test.status}${test.contentType ? ', ' + test.contentType : ''}, ${test.bytes} bytes)` : ''}.
          {test.sample && <div style={{ marginTop: 4 }}>First punch: <span className="mono">{test.sample.employee}</span> at <span className="mono">{test.sample.time}</span>{test.sample.direction ? `, ${test.sample.direction}` : ''}</div>}
          {test.warning && <div style={{ marginTop: 4 }}>{test.warning}.</div>}</div>
          : cfg.data?.tested_at && <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Last tested {whenT(cfg.data.tested_at)}: {cfg.data.test_note}</p>}</>}</div>
      <div className="card"><h3>How punches become attendance</h3><ul style={{ margin: 0, paddingLeft: 18, fontSize: 13, lineHeight: 1.7 }}><li>First IN of the day and last OUT become the day’s record. Night shifts that end after midnight stay on the shift’s start date.</li><li>A punch sent twice counts once.</li><li>The machine’s employee code must match the Employee ID.</li><li>A day with a regularization approved keeps the corrected times; later punches do not overwrite it.</li></ul>
        <h3 style={{ margin: '16px 0 8px', fontSize: 13 }}>What we need from your vendor</h3><ul style={{ margin: 0, paddingLeft: 18, fontSize: 13, lineHeight: 1.7 }}><li>API address, method and how it authenticates</li><li>A sample response with a few punches</li><li>How to ask for punches since a time</li><li>Whether the server is reachable from the internet (static IP and open port) or a small agent in the office should push data out instead</li></ul></div></div>
    <div className="card" style={{ marginTop: 14 }}><div className="row sp"><h3 style={{ margin: 0 }}>Machines that push punches</h3><Pill k={on.some((x) => x.last_seen) ? 'ok' : 'n'}>{on.length} active</Pill></div>
      <p className="mute" style={{ fontSize: 12.5 }}>Where the machine, or the vendor's bridge, can send punches itself, add it here. It sends punches to <span className="mono">POST /api/punches</span> with its key in the <span className="mono">X-Device-Key</span> header:
        <span className="mono"> {'{"punches":[{"employeeId":"EMP-1020","time":"2026-11-01 09:02","direction":"IN"}]}'}</span>. Times without a zone are IST.</p>
      <div className="tw"><table><thead><tr><th>Machine</th><th>Location</th><th>Vendor</th><th>Key</th><th className="num">Punches</th><th>Last seen</th><th>Status</th><th></th></tr></thead><tbody>
        {(d.data ?? []).map((x) => <tr key={x.id}><td><b>{x.name}</b> <span className="mono mute">{x.serial}</span></td><td>{x.location ?? '—'}</td><td>{x.vendor || '—'}</td><td className="mono">…{x.key_hint}</td>
          <td className="num">{x.punches}</td><td>{x.last_seen ?? 'Never'}</td><td><Pill k={x.status === 'Active' ? 'ok' : 'n'}>{x.status}</Pill></td>
          <td>{ed && x.status === 'Active' && <div className="row" style={{ gap: 6 }}><button className="btn sm" onClick={() => confirm('Replace the key? The old key stops working at once.') && act(x.id, 'key')}>New key</button>
            <button className="btn sm" onClick={() => { const r = prompt('Why is this machine retired?'); if (r) act(x.id, 'inactive', { reason: r }); }}>Retire</button></div>}</td></tr>)}
        {d.data && !d.data.length && <tr><td colSpan={8} className="mute">No machines yet.</td></tr>}
      </tbody></table></div>
      {ed && <div className="row" style={{ marginTop: 16 }}><button className="btn" onClick={() => setAdd({ name: '', locationId: '', vendor: '', serial: '' })}>Add a machine</button></div>}</div>
    {add && <Modal onClose={() => setAdd(null)}><h2 style={{ marginTop: 0 }}>Add a punch machine</h2>
      <div className="form"><Field label="Name"><input value={add.name} onChange={(e) => setAdd({ ...add, name: e.target.value })} placeholder="Sector 29 front desk" /></Field>
        <Field label="Location"><select value={add.locationId} onChange={(e) => setAdd({ ...add, locationId: e.target.value })}><option value="">Any</option>{(locs.data ?? []).map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</select></Field>
        <Field label="Vendor"><input value={add.vendor} onChange={(e) => setAdd({ ...add, vendor: e.target.value })} /></Field>
        <Field label="Serial number"><input value={add.serial} onChange={(e) => setAdd({ ...add, serial: e.target.value })} /></Field></div>
      <div className="row" style={{ justifyContent: 'flex-end', marginTop: 12 }}><button className="btn primary" onClick={save}>Add</button></div></Modal>}
    {key && <Modal onClose={() => setKey(null)}><h2 style={{ marginTop: 0 }}>Device key</h2><p className="ink2">{key.note}</p>
      <div className="mono" style={{ background: 'var(--soft)', padding: 12, borderRadius: 8, overflowWrap: 'anywhere' }}>{key.key}</div>
      <div className="row" style={{ justifyContent: 'flex-end', marginTop: 12 }}><button className="btn" onClick={() => navigator.clipboard?.writeText(key.key).then(() => toast('Copied'))}>Copy</button><button className="btn primary" onClick={() => setKey(null)}>Done</button></div></Modal>}
  </>;
}

// ---------- Realtime India portal (16 Nov 2026): the office machines report to Realtime's web portal; its raw punch report is read on a schedule ----------

type RtUnmatched = { code: string; card: string; name: string; dept: string; swipes: number };

function RealtimePortal({ ed }: { ed: boolean }) {
  const d = useLoad(() => api('/attendance/realtime'), []);
  const [f, setF] = useState<{ on: boolean; every: string; byId: boolean } | null>(null);
  const [rng, setRng] = useState({ from: today(), to: today() });
  const [busy, setBusy] = useState('');
  const [all, setAll] = useState(false);
  const running = d.data?.running;
  useEffect(() => { if (d.data && !f) setF({ on: !!d.data.on, every: String(d.data.every ?? 30), byId: !!d.data.byId }); }, [d.data, f]);
  useEffect(() => { // while a pull runs, look again every few seconds
    if (!running) return;
    const t = setInterval(d.reload, 4000);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [running]);
  const save = async () => {
    setBusy('save');
    try { await api('/attendance/realtime', { method: 'PUT', body: { on: f!.on, every: +f!.every || 0, byId: f!.byId } }); toast('Settings saved'); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const pull = async () => {
    setBusy('pull');
    try { await api('/attendance/realtime/pull', { body: rng }); toast('Pull started. It takes about a minute a day'); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const last = d.data?.last;
  const list: RtUnmatched[] = last?.unmatched ?? [];
  const shown = all ? list : list.slice(0, 10);
  const pill = !d.data ? null : !d.data.envSet ? <Pill k="warn">Account not set</Pill> : running ? <Pill k="info">Pulling…</Pill>
    : last ? <Pill k={last.ok ? 'ok' : 'bad'}>{last.ok ? 'Working' : 'Last pull failed'}</Pill> : <Pill k="n">Not pulled yet</Pill>;
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Realtime India portal</h3>{pill}</div>
    {!d.data || !f ? <Loading error={d.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5 }}>Your punch machines report to the Realtime attendance portal{d.data.host ? <> at <span className="mono">{d.data.host}</span></> : ''}. The HRMS signs in with the portal account, reads the day’s <b>All Raw Punch Report</b> (every swipe, entry and exit) and stores each swipe as a punch. It only reads the report; it never changes anything in the portal.</p>
      {!d.data.envSet && <div className="note warn" style={{ marginBottom: 12 }}>Add <span className="mono">REALTIME_URL</span>, <span className="mono">REALTIME_USER</span> and <span className="mono">REALTIME_PASS</span> to the server’s environment (backend/.env) and restart the server. The account is kept there only, never in the database.</div>}
      <div className="form">
        <Field label="Pull automatically"><select value={f.on ? 'on' : 'off'} disabled={!ed} onChange={(e) => setF({ ...f, on: e.target.value === 'on' })}><option value="off">Off</option><option value="on">On</option></select></Field>
        <Field label={`Pull every (minutes, ${d.data.minEvery} or more)`}><input type="number" min={d.data.minEvery} value={f.every} disabled={!ed} onChange={(e) => setF({ ...f, every: e.target.value })} /></Field>
        <Field label="Match by Employee ID number"><select value={f.byId ? 'on' : 'off'} disabled={!ed} onChange={(e) => setF({ ...f, byId: e.target.value === 'on' })}><option value="off">Off: Attendance code only</option><option value="on">On: portal 1050 = EMP-1050</option></select></Field>
      </div>
      <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Turn number matching on only if your Employee IDs were made from the portal codes. Otherwise a swipe can land on the wrong person.</p>
      {ed && <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={!!busy} onClick={save}>Save settings</button></div>}
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>When on, today is pulled at each interval, and until 10:00 yesterday too, for swipes that reach the portal late. A swipe already stored counts once.</p>
      {ed && <><h3 style={{ margin: '16px 0 8px', fontSize: 13 }}>Pull now</h3>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <Field label="From"><input type="date" value={rng.from} max={today()} onChange={(e) => setRng({ from: e.target.value, to: rng.to < e.target.value ? e.target.value : rng.to })} /></Field>
          <Field label="To"><input type="date" value={rng.to} min={rng.from} max={today()} onChange={(e) => setRng({ ...rng, to: e.target.value })} /></Field>
          <button className="btn" disabled={!!busy || !!running || !d.data.envSet} onClick={pull}>{running ? 'Pulling…' : busy === 'pull' ? 'Starting…' : 'Pull now'}</button>
        </div>
        <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>Up to {d.data.maxDays} days at a time, for example to load the month so far.</p></>}
      {running && <div className="note" style={{ marginTop: 12 }}>Pulling {running.from === running.to ? fmtD(running.from) : `${fmtD(running.from)} to ${fmtD(running.to)}`}, started by {running.by}…</div>}
      {last && <div className={`note ${last.ok ? 'ok' : 'bad'}`} style={{ marginTop: 12 }}>
        Last pull {whenT(last.at)} ({last.by}, {last.from === last.to ? fmtD(last.from) : `${fmtD(last.from)} to ${fmtD(last.to)}`}): {last.note}.
        {(last.errors ?? []).length > 1 && <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{last.errors.slice(1, 6).map((e: string, i: number) => <li key={i}>{e}</li>)}</ul>}
      </div>}
      {list.length > 0 && <>
        <h3 style={{ margin: '16px 0 4px', fontSize: 13 }}>Portal codes that match no employee ({last.unmatchedCount})</h3>
        <p className="mute" style={{ fontSize: 12, margin: '0 0 8px' }}>Their swipes were not stored. Give each person their portal code in the <b>Attendance code</b> column of Bulk update › Employee master update (or Import employees), then pull again.{f.byId ? ' Without a code, the portal code must equal the number of the Employee ID (1050 for EMP-1050).' : ''}</p>
        <div className="tw"><table><thead><tr><th>Portal code</th><th>Card number</th><th>Name in the portal</th><th>Department</th><th className="num">Swipes</th></tr></thead><tbody>
          {shown.map((u) => <tr key={u.code + u.card}><td className="mono">{u.code}</td><td className="mono">{u.card}</td><td>{u.name || '—'}</td><td>{u.dept || '—'}</td><td className="num">{u.swipes}</td></tr>)}
        </tbody></table></div>
        {list.length > 10 && <button className="btn sm" style={{ marginTop: 8 }} onClick={() => setAll(!all)}>{all ? 'Show fewer' : `Show all ${list.length}${last.unmatchedCount > list.length ? ` (of ${last.unmatchedCount})` : ''}`}</button>}
      </>}
    </>}
  </div>;
}
