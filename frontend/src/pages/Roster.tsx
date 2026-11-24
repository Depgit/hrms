import { useMemo, useState } from 'react';
import { api, fmtD, recentYm, addDays, addMonths, today, postForm } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Modal, Pill, download, toast, useLoad } from '../components/ui';
import { scopeEmps, ymLabel, type ScopeEmp } from './Attendance';
import { letterFile } from './MyTime';
import { CellPicker, DevOwnModal } from './RosterCell';

type T = 'grid' | 'dev' | 'disc' | 'bulk' | 'cons';
type K = 'ok' | 'warn' | 'bad' | 'info' | 'n';
type Sel = { city: string; loc: string; dept: string };
const ROSR = ['Guest demand or occupancy', 'Staff absent or sick', 'Employee request', 'Event or banquet', 'Vacancy cover', 'Other'];
const dlab = (d: string) => new Date(d + 'T00:00:00').toLocaleDateString('en-IN', { weekday: 'short', day: 'numeric', month: 'short' });
const monday = (d: string) => addDays(d, -((new Date(d + 'T00:00:00').getDay() + 6) % 7));
const scoreK = (s: number): K => (s >= 90 ? 'ok' : s >= 70 ? 'warn' : 'bad');
/** The discipline band from the server (it scales with a part month's start). */
const bandK = (b?: string): K => (b === 'Good' ? 'ok' : b === 'Watch' ? 'warn' : 'bad');
const uniq = <X,>(xs: X[], key: (x: X) => string) => [...new Map(xs.map((x) => [key(x), x])).values()];
/** Roster alert (rosAlertHtml): property · department pairs over the late-change limit of their work policy. */
const Alert = ({ a }: { a?: any[] }) => (a?.length ? <div className="note bad" style={{ marginBottom: 14 }}><b>Roster alert:</b> {a.map((x) => `${x.key} has ${x.pct}% late changes this month (limit ${x.limit ?? 30}%, ${x.n} changes)`).join('; ')}. Review the discipline report.</div> : null);
/** The roster query for the city, property (blank for all) and department chosen. */
const rosQ = (s: Sel) => `city=${s.city}&location=${s.loc}&department=${encodeURIComponent(s.dept)}`;

/** Whether this role view may change the roster (edit level of the right). Never in My view, the employee's own
 *  self-service, whatever the Employee role's rights say; the server checks the right on every change. */
function useRosEdit(right = 'ros.roster') {
  const { can, me } = useAuth();
  return me?.ctx.role !== 'Employee' && can(right, 2);
}

/** The cities, properties and departments the caller can roster (rosScope). */
function rosScope(dir: ScopeEmp[], s: Sel) {
  const by = (a: string, b: string) => (a < b ? -1 : 1);
  const cities = uniq(dir, (e) => e.city_id).map((e) => ({ id: e.city_id, name: e.city })).sort((a, b) => by(a.name, b.name));
  const depts = [...new Set(dir.map((e) => e.department))].sort(by);
  const city = cities.some((c) => c.id === s.city) ? s.city : cities[0]?.id ?? '';
  // First visit: the first department that has people in the chosen city, so the grid does not open empty.
  const dept = depts.includes(s.dept) ? s.dept : depts.find((d) => dir.some((e) => e.city_id === city && e.department === d)) ?? depts[0] ?? '';
  const locs = uniq(dir.filter((e) => e.city_id === city && e.department === dept), (e) => e.location_id).map((e) => ({ id: e.location_id, name: e.location })).sort((a, b) => by(a.name, b.name));
  const loc = locs.some((l) => l.id === s.loc) ? s.loc : '';
  const emps = dir.filter((e) => e.city_id === city && e.department === dept && (!loc || e.location_id === loc));
  return { cities, depts, locs, sel: { city, dept, loc }, emps };
}

export default function Roster() {
  const [tab, setTab] = useState<T>('grid');
  const [sel, setSel] = useState<Sel>({ city: '', loc: '', dept: '' });
  const [week, setWeek] = useState(monday(today()));
  const dir = useLoad(scopeEmps, []);
  const devs = useLoad(() => api(`/deviations?from=${addDays(today(), -14)}&to=${today()}&status=Pending`).then((d) => d.pending as number).catch(() => 0), [tab]);
  const cons = useLoad(() => api('/consequences').catch(() => null), [tab]);
  const consN = (cons.data?.rows ?? []).filter((x: any) => x.canDecide).length;
  const bulkOn = useRosEdit();
  const T = (k: T, l: string, n?: number) => <button key={k} className={`tab ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}{n ? <> <span className="badge">{n}</span></> : null}</button>;
  const sc = dir.data ? rosScope(dir.data, sel) : null;
  const pick = (k: keyof Sel, v: string) => setSel({ ...(sc?.sel ?? sel), [k]: v, ...(k !== 'loc' ? { loc: '' } : {}) });
  const filters = sc && <>
    <Field label="City"><select value={sc.sel.city} onChange={(e) => pick('city', e.target.value)}>{sc.cities.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
    <Field label="Property"><select value={sc.sel.loc} onChange={(e) => pick('loc', e.target.value)}><option value="">All properties</option>{sc.locs.map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</select></Field>
    <Field label="Department"><select value={sc.sel.dept} onChange={(e) => pick('dept', e.target.value)}>{sc.depts.map((d) => <option key={d}>{d}</option>)}</select></Field>
  </>;
  return <>
    <div className="tabs">{T('grid', 'Roster')}{T('dev', 'Deviations', devs.data ?? 0)}{T('disc', 'Discipline report')}{bulkOn && T('bulk', 'Bulk upload')}{T('cons', 'Consequences', consN)}</div>
    {tab === 'dev' && <Deviations onChange={devs.reload} />}
    {tab === 'disc' && <Discipline />}
    {tab === 'cons' && <Consequences onChange={cons.reload} />}
    {(tab === 'grid' || (tab === 'bulk' && bulkOn)) && (!sc ? <Loading error={dir.error} />
      : tab === 'grid' ? <Grid sc={sc} filters={filters} week={week} setWeek={setWeek} />
      : <Bulk sc={sc} filters={filters} week={week} />)}
  </>;
}

// ---------- Weekly roster ----------

/** A deviation marker on a roster cell (devTag / devTip): the shift worked, or − for leaving early. (A + is an extra-hours
 * deviation recorded before 8 Nov 2026; extra hours are no longer a deviation.) */
const devTag = (v?: any) => {
  if (!v) return null;
  const t = v.kinds.includes('Shift') || v.planned !== v.actual ? (v.actual === 'Custom' ? '?' : v.actual) : v.kinds.includes('Early exit') ? '−' : '+';
  const tip = `Planned ${v.planned}, actual ${v.actual === 'Custom' ? 'a different time' : v.actual}. In ${v.in || '—'}, out ${v.out || '—'}, ${v.hours} h. ${v.kinds.join(', ')}. ${v.status === 'Pending' ? 'Waiting for the manager' : 'Marked ' + v.status}`;
  return <i className="dv" title={tip}>{v.status === 'Not OK' ? '✕' : '→'}{t}</i>;
};

/** What a roster cell shows: an active call to work shows its shift with a mark (the day stays a holiday or leave). */
const shownCode = (c: any) => (c.call?.active ? c.call.shift : c.code);
const cellTip = (c: any) => {
  const k = c.call;
  const call = k && (k.active
    ? `Called to work (${k.kind === 'holiday' ? 'holiday' : k.leave}): ${k.shift}${k.time ? ' ' + k.time : ''}. ${k.reason}. By ${k.by}, ${k.at}`
    : 'A call to work on this date does nothing now: the day is no longer a holiday or leave day');
  return [call, c.time && !k?.active && `${c.code === 'OT' ? 'Other time' : c.code} ${c.time}`, c.leave && c.code !== 'L' && `${c.leave} (approved leave)`, c.tip,
    c.pending && `${c.pending} waiting for approval`].filter(Boolean).join('. ');
};

function Grid({ sc, filters, week, setWeek }: { sc: ReturnType<typeof rosScope>; filters: any; week: string; setWeek: (w: string) => void }) {
  const { can, me } = useAuth();
  const canE = useRosEdit(), canPub = useRosEdit('ros.publish');
  const canCall = me?.ctx.role !== 'Employee' && can('att.records', 2);
  const [pick, setPick] = useState<{ row: any; i: number } | null>(null);
  const [devFor, setDevFor] = useState('');
  const [why, setWhy] = useState(ROSR[0]);
  const [min, setMin] = useState(2);
  const [pat, setPat] = useState('fixed');
  const [busy, setBusy] = useState('');
  const { data: rs, error, reload } = useLoad(() => api(`/roster?${rosQ(sc.sel)}&week=${week}`), [rosQ(sc.sel), week]);
  const days: string[] = rs?.days ?? Array.from({ length: 7 }, (_, i) => addDays(week, i));
  const rows: any[] = rs?.rows ?? [];
  const pub = !!rs?.published;
  const lead = rs?.rules.lead ?? 2;
  const shifts = ((rs?.codes ?? []) as string[]).filter((c) => c !== 'WO' && c !== 'PH');
  // On duty: a shift or other time, or called to work (an active call on a holiday or leave day).
  const cover = days.map((_: string, i: number) => rows.filter((r: any) => !['WO', 'L', 'PH'].includes(shownCode(r.cells[i]))).length);
  const publish = async () => {
    try {
      const r = await Promise.all((rs?.locations ?? []).filter((l: any) => !l.published).map((l: any) => api('/roster/publish', { body: { location: l.id, department: sc.sel.dept, week: rs.week } })));
      toast(r.some((x) => x.late) ? 'Roster published, marked late' : 'Roster published'); reload();
    } catch (e: any) { toast(e.message); }
  };
  /** Apply pattern to everyone shown (rpat), through the same rules as a single change. */
  const pattern = async () => {
    setBusy('pattern');
    try {
      const r = await api('/roster/pattern', { body: { city: sc.sel.city, location: sc.sel.loc, department: sc.sel.dept, week, pattern: pat, why } });
      toast(r.blocked ? `${r.applied} changes made, ${r.blocked} dates not changed (locked, or less than the minimum rest between shifts)` : `${r.applied} changes made for ${r.employees} employees`); reload();
    } catch (x: any) { toast(x.message); } finally { setBusy(''); }
  };
  return <>
    <Alert a={rs?.alerts} />
    <div className="card" style={{ marginBottom: 14 }}><div className="row sp"><div><b>Roster rules</b><div className="mute" style={{ fontSize: 12.5 }}>The roster is final {lead} days before each date. Changes after that are late changes and need a reason. Attendance, late marks and loss of pay follow this roster, so a changed roster needs no regularization.</div></div>
      {canE && <label className="field" style={{ minWidth: 230 }}><span>Reason for late changes</span><select value={why} onChange={(e) => setWhy(e.target.value)}>{(rs?.reasons ?? ROSR).map((x: string) => <option key={x}>{x}</option>)}</select></label>}</div></div>
    <div className="card" style={{ marginBottom: 14 }}><div className="row">{filters}
      <Field label="Minimum on duty a day"><input type="number" value={min} onChange={(e) => setMin(+e.target.value)} style={{ width: 90 }} /></Field>
      {canE && <label className="field"><span>Apply pattern to everyone shown</span><div className="row"><select id="rpat" value={pat} onChange={(e) => setPat(e.target.value)}><option value="fixed">Reset to work policy defaults</option><option value="rot">Rotate Morning, Evening, Night weekly</option><option value="6on1">Six on, one off (staggered off day)</option></select><button className="btn" disabled={!!busy || !rows.length} onClick={pattern}>Apply</button></div></label>}</div></div>
    {error && <div className="note bad" style={{ marginBottom: 14 }}>{error}</div>}
    <div className="card"><div className="row sp" style={{ marginBottom: 10 }}><div className="row"><button className="btn sm" onClick={() => setWeek(addDays(week, -7))}>Previous week</button><b>{dlab(days[0])} to {dlab(days[6])}</b><button className="btn sm" onClick={() => setWeek(addDays(week, 7))}>Next week</button>
      <span className="mute">{rows.length} employees {pub ? <Pill k="ok">Published</Pill> : <Pill k="warn">Draft</Pill>}</span></div>
      {canPub && <button className="btn primary" disabled={pub || !rs?.locations?.length} onClick={publish}>Publish roster</button>}</div>
      {!rs && !error ? <Loading /> : <div className="tw"><table className="roster"><thead><tr><th style={{ textAlign: 'left' }}>Employee</th>{days.map((d: string) => <th key={d}>{dlab(d)}</th>)}<th style={{ textAlign: 'left' }}>Checks</th></tr></thead>
        <tbody>{rows.map((row: any) => <tr key={row.id}><td className="nm"><button className="linkb" title="Deviation allowances of this employee" onClick={() => setDevFor(row.id)}><b>{row.name}</b></button>
          <div className="mute" style={{ fontSize: 11 }}>{row.id} · {row.policy}{row.devOwn ? ' · own deviation rules' : ''}</div></td>
          {row.cells.map((c: any, i: number) => <td key={i}><button className={`cell ${shownCode(c)}${c.call?.active ? ' called' : ''}${c.changed ? (c.late ? ' chgL' : ' chg') : ''}`} title={cellTip(c)}
            disabled={(!canE && !canCall) || busy === row.id + days[i]} onClick={() => setPick({ row, i })}>{shownCode(c)}{c.call?.active && <i className="cw">*</i>}{devTag(c.dev)}</button></td>)}
          <td style={{ textAlign: 'left' }}>{row.warnings.length ? row.warnings.map((w: string) => <Pill key={w} k="warn">{w}</Pill>) : <Pill k="ok">OK</Pill>}</td></tr>)}
          {!rows.length && <tr><td colSpan={9} className="mute" style={{ textAlign: 'left' }}>No active employees in this city and department for your scope.</td></tr>}</tbody>
        <tfoot><tr><td className="nm mute">On duty</td>{cover.map((n: number, i: number) => <td key={i}><span className={`pill ${n < min ? 'bad' : 'ok'}`}>{n}</span></td>)}<td /></tr>
          {shifts.map((code) => <tr key={code}><td className="nm mute">{code} shift{rs?.timings?.[code] ? <div className="mono" style={{ fontSize: 11 }}>{rs.timings[code]}</div> : null}</td>{days.map((_: string, i: number) => <td key={i} className="mono mute">{rows.filter((r: any) => shownCode(r.cells[i]) === code).length}</td>)}<td /></tr>)}
          {rows.some((r: any) => r.cells.some((c: any) => c.code === 'OT')) && <tr><td className="nm mute">Other time</td>{days.map((_: string, i: number) => <td key={i} className="mono mute">{rows.filter((r: any) => r.cells[i].code === 'OT').length}</td>)}<td /></tr>}</tfoot></table></div>}
      <p className="mute" style={{ fontSize: 12, margin: '12px 0 0' }}>WO is weekly off and PH is public holiday (optional holidays are not auto-filled, since each employee picks theirs), both filled from each employee's work policy and its city holiday calendar. L is leave. OT is other time for that day only. A shift with * is a call to work on a holiday or leave day. Click a cell to change it; click a name for that employee's deviation allowances. Only shifts mapped to the employee's department and location can be assigned.</p></div>
    {pick && <CellPicker row={pick.row} cell={pick.row.cells[pick.i]} date={days[pick.i]} timings={rs?.timings ?? {}} why={why} canRoster={canE} canCall={canCall}
      onClose={(changed) => { setPick(null); if (changed) reload(); }} />}
    {devFor && <DevOwnModal id={devFor} onClose={() => { setDevFor(''); reload(); }} />}
  </>;
}

// ---------- Deviations ----------

function Deviations({ onChange }: { onChange: () => void }) {
  const ed = useRosEdit('ros.publish');
  const [f, setF] = useState({ from: addDays(today(), -6), to: today(), st: 'Pending', q: '' });
  const [sel, setSel] = useState<Record<string, boolean>>({});
  const [note, setNote] = useState('');
  const { data, error, reload } = useLoad(() => api(`/deviations?from=${f.from}&to=${f.to}`), [f.from, f.to]);
  const all: any[] = data?.rows ?? [], ql = f.q.trim().toLowerCase();
  const rows = all.filter((x) => (!f.st || x.status === f.st) && (!ql || `${x.name} ${x.employeeId}`.toLowerCase().includes(ql)));
  const cnt = (s: string) => all.filter((x) => x.status === s).length;
  const keys = Object.keys(sel).filter((k) => sel[k]);
  const set = (k: string, v: string) => { setF({ ...f, [k]: v }); setSel({}); };
  const quick = (k: string) => { const t = today(); if (k === 'week') setF({ ...f, from: addDays(t, -6), to: t }); else if (k === 'month') setF({ ...f, from: t.slice(0, 8) + '01', to: t }); else { const p = addMonths(t.slice(0, 7), -1); setF({ ...f, from: p + '-01', to: addDays(t.slice(0, 8) + '01', -1) }); } setSel({}); };
  const decide = async (status: string, items = keys) => {
    try {
      const r = await api('/deviations/decide', { body: { items: items.map((k) => { const [employeeId, date] = k.split('|'); return { employeeId, date }; }), status, note } });
      toast(`${r.done} marked ${status}${r.skipped ? `, ${r.skipped} skipped` : ''}`); setSel({}); setNote(''); reload(); onChange();
    } catch (x: any) { toast(x.message); }
  };
  const pend = ed && f.st === 'Pending';
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 10 }}><div><b>Planned against actual</b><div className="mute" style={{ fontSize: 12.5 }}>Attendance is worked out from the first punch in and the last punch out, so pay follows the hours. Where the shift worked is different from the roster, the manager says whether it was agreed. This is separate from attendance regularization.</div></div></div>
      <div className="form" style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', marginTop: 12 }}>
        <Field label="From"><input type="date" value={f.from} onChange={(e) => e.target.value && set('from', e.target.value)} /></Field>
        <Field label="To"><input type="date" value={f.to} onChange={(e) => e.target.value && set('to', e.target.value)} /></Field>
        <Field label="Decision"><select value={f.st} onChange={(e) => set('st', e.target.value)}><option value="Pending">Waiting for a decision</option><option value="OK">Marked OK</option><option value="Not OK">Marked Not OK</option><option value="">All</option></select></Field>
        <Field label="Search"><input type="search" value={f.q} onChange={(e) => set('q', e.target.value)} placeholder="Name or ID" /></Field></div>
      <div className="row" style={{ gap: 8, marginTop: 10 }}><button className="btn" onClick={() => quick('week')}>Last 7 days</button><button className="btn" onClick={() => quick('month')}>This month</button><button className="btn" onClick={() => quick('last')}>Last month</button></div></div>
    <div className="grid g4" style={{ marginBottom: 14 }}><div className="card stat"><div className="l">Waiting for a decision</div><div className="n">{cnt('Pending')}</div></div><div className="card stat"><div className="l">Marked OK</div><div className="n">{cnt('OK')}</div></div><div className="card stat"><div className="l">Marked Not OK</div><div className="n">{cnt('Not OK')}</div></div><div className="card stat"><div className="l">In this view</div><div className="n">{rows.length}</div></div></div>
    <div className="card"><div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap', gap: 8 }}><b>{rows.length} deviation{rows.length === 1 ? '' : 's'}</b>
      {pend && rows.length > 0 && <div className="row" style={{ gap: 8 }}><input placeholder="Note, optional" style={{ minWidth: 180, width: 'auto' }} value={note} onChange={(e) => setNote(e.target.value)} />
        <button className="btn" onClick={() => setSel(Object.fromEntries(rows.slice(0, 100).map((x) => [x.employeeId + '|' + x.date, true])))}>Select all</button>
        <button className="btn primary" disabled={!keys.length} onClick={() => decide('OK')}>Mark OK ({keys.length})</button><button className="btn bad" disabled={!keys.length} onClick={() => decide('Not OK')}>Mark Not OK ({keys.length})</button></div>}</div>
      {!data ? <Loading error={error} /> : <div className="tw"><table><thead><tr>{pend && <th />}<th>Employee</th><th>Date</th><th>Planned</th><th>Actual</th><th>In · Out</th><th className="num">Hours</th><th>What differs</th><th>Decision</th><th /></tr></thead><tbody>
        {rows.slice(0, 100).map((x) => { const k = x.employeeId + '|' + x.date, v = x.dev; return <tr key={k}>
          {pend && <td><input type="checkbox" checked={!!sel[k]} onChange={(e) => setSel({ ...sel, [k]: e.target.checked })} /></td>}
          <td><b>{x.name}</b><div className="mute" style={{ fontSize: 12 }}>{x.employeeId} · {x.location}</div></td><td>{fmtD(x.date)}</td><td><b>{v.planned}</b></td><td><b>{v.actual === 'Custom' ? 'Other time' : v.actual}</b></td>
          <td className="mono">{v.in || '—'} · {v.out || '—'}</td><td className="num">{v.hours}{v.planH ? <span className="mute"> of {Math.round(v.planH * 10) / 10}</span> : null}</td>
          <td>{v.kinds.map((y: string) => <Pill key={y} k="warn">{y}</Pill>)}</td>
          <td>{x.status === 'Pending' ? <Pill k={x.overdue ? 'bad' : 'warn'}>{x.overdue ? 'Overdue' : 'Waiting'}</Pill> : <Pill k={x.status === 'OK' ? 'ok' : 'bad'}>{x.status}</Pill>}
            {x.status !== 'Pending' && <div className="mute" style={{ fontSize: 11.5 }}>{[x.by, x.at && fmtD(String(x.at).slice(0, 10)), x.note].filter(Boolean).join(' · ')}</div>}</td>
          <td>{ed && x.status === 'Pending' && <div className="row" style={{ gap: 6 }}><button className="btn sm" onClick={() => decide('OK', [k])}>OK</button><button className="btn sm bad" onClick={() => decide('Not OK', [k])}>Not OK</button></div>}</td></tr>; })}
        {!rows.length && <tr><td colSpan={10} className="mute" style={{ padding: 22, textAlign: 'center' }}>Nothing here for these dates.</td></tr>}
      </tbody></table></div>}
      {rows.length > 100 && <div className="mute" style={{ fontSize: 12, marginTop: 8 }}>Showing the first 100 of {rows.length}. Narrow the dates or search by name.</div>}
      <p className="mute" style={{ fontSize: 12, margin: '12px 0 0' }}><b>OK</b> means the manager agreed. The roster is updated to the shift actually worked, and no points are lost. <b>Not OK</b> keeps the roster as planned, pay still follows the hours worked, and the employee loses discipline points.</p></div>
  </>;
}

// ---------- Discipline report ----------

function Discipline() {
  const [ym, setYm] = useState(recentYm());
  const { data: d, error } = useLoad(() => api(`/roster/discipline?ym=${ym}`), [ym]);
  const tbl = (rows: any[], head: string) => <div className="tw"><table><thead><tr><th>{head}</th><th className="num">Changes</th><th className="num">Late</th><th className="num">Late %</th><th className="num">Employees</th><th>Main reason</th></tr></thead><tbody>
    {rows.map((x) => { const p = Math.round((x.late / x.n) * 100); return <tr key={x.key}><td><b>{x.key}</b></td><td className="num">{x.n}</td><td className="num">{x.late}</td><td className="num"><Pill k={p >= 50 ? 'bad' : p >= 25 ? 'warn' : 'ok'}>{p}%</Pill></td><td className="num">{x.people}</td><td className="ink2">{x.reason || '—'}</td></tr>; })}
    {!rows.length && <tr><td colSpan={6} className="mute">No roster changes this month.</td></tr>}</tbody></table></div>;
  const people = (d?.people ?? []).filter((p: any) => p.dev > 0).sort((a: any, b: any) => a.score - b.score || b.dev - a.dev).slice(0, 15);
  return <>
    <Alert a={d?.alerts} />
    <div className="row" style={{ marginBottom: 12 }}><Field label="Month"><input type="month" value={ym} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field></div>
    {!d ? <Loading error={error} /> : <>
      <div className="grid g4" style={{ marginBottom: 14 }}>
        <div className="card stat"><div className="l">Roster changes</div><div className="n">{d.changes}</div></div>
        <div className="card stat"><div className="l">Late changes</div><div className="n">{d.late} <span className="mute" style={{ fontSize: 13 }}>{d.changes ? Math.round((d.late / d.changes) * 100) : 0}%</span></div></div>
        <div className="card stat"><div className="l">Rosters published late</div><div className="n">{d.pubLate}</div></div>
        <div className="card stat"><div className="l">Regularizations caused by roster</div><div className="n">{d.regRoster} <span className="mute" style={{ fontSize: 13 }}>of {d.regAll}</span></div></div>
      </div>
      <div className="card" style={{ marginBottom: 14 }}><h3>By property and department</h3>{tbl(d.byPlace, 'Property · department')}</div>
      <div className="card" style={{ marginBottom: 14 }}><h3>By who made the change</h3>{tbl(d.byWho, 'Role')}</div>
      <div className="card"><h3>How to read this</h3><p className="ink2" style={{ fontSize: 13, margin: 0 }}>A high late-change share on one property means the roster is not being planned ahead, or the property is short-staffed. Fix it at the source: a larger bench, better demand forecast or a different shift pattern. Employees are not asked to regularize for these days, because attendance follows the changed roster.</p></div>
      <div className="card" style={{ marginTop: 14 }}><h3>Manager planning score</h3><div className="tw"><table><thead><tr><th>Manager</th><th className="num">Team</th><th className="num">Rostered days</th><th className="num">Deviations</th><th className="num">Deviation share</th><th className="num">Marked OK</th><th className="num">Late roster changes</th><th className="num">Score</th><th /></tr></thead><tbody>
        {d.managers.map((m: any) => <tr key={m.id}><td><b>{m.name}</b></td><td className="num">{m.team}</td><td className="num">{m.days}</td><td className="num">{m.dev}</td><td className="num">{m.share}%</td><td className="num">{m.ok}</td><td className="num">{m.late} of {m.changes}</td>
          <td className="num"><Pill k={scoreK(m.planning)}>{m.planning}</Pill></td><td>{m.flag && <Pill k="bad">Roster not matching the team</Pill>}</td></tr>)}
        {!d.managers.length && <tr><td colSpan={9} className="mute">No data for this month.</td></tr>}
      </tbody></table></div><p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Score = 100 − 2 × deviation share − half the share of late roster changes. A manager who marks many deviations OK is not planning rosters that match how the team really works.</p></div>
      <div className="card" style={{ marginTop: 14 }}><h3>Employee discipline score, lowest first</h3><div className="tw"><table><thead><tr><th>Employee</th><th>Property</th><th className="num">Deviations</th><th className="num">OK</th><th className="num">Not OK</th><th className="num">Waiting</th><th className="num">Score</th></tr></thead><tbody>
        {people.map((p: any) => <tr key={p.id}><td><b>{p.name}</b><div className="mute" style={{ fontSize: 12 }}>{p.id}</div></td><td>{p.location}{p.department ? ` · ${p.department}` : ''}</td><td className="num">{p.dev}</td><td className="num">{p.ok}</td><td className="num">{p.no}</td><td className="num">{p.pending}</td><td className="num"><Pill k={bandK(p.band)}>{p.score}</Pill>{p.start < 100 && <div className="mute" style={{ fontSize: 11.5 }}>Started at {p.start}{p.startNote ? ` (${p.startNote})` : ''}</div>}</td></tr>)}
        {!people.length && <tr><td colSpan={7} className="mute">No deviations this month.</td></tr>}
      </tbody></table></div><p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Everyone starts a month at 100 and loses points for each deviation the manager marks Not OK (set in the work policy). Someone employed for only part of the month starts at 100 × days employed ÷ days in the month, and the Good and Watch bands scale with it. Deviations waiting for a decision do not cost points yet.</p></div>
    </>}
  </>;
}

// ---------- Bulk upload: download the roster for a period, change it offline, upload it back ----------

function Bulk({ sc, filters, week }: { sc: ReturnType<typeof rosScope>; filters: any; week: string }) {
  const canE = useRosEdit();
  const [from, setFrom] = useState(week);
  const [n, setN] = useState('14');
  const [file, setFile] = useState<File | null>(null);
  const [lines, setLines] = useState(0);
  const [res, setRes] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const ds = Array.from({ length: +n }, (_, i) => addDays(from, i));
  /** rbDownload: one column per date with the current codes, drop-downs and a reason column, built by the server. */
  const dl = async () => {
    if (!sc.emps.length) { toast('No employees match the city, property and department chosen'); return; }
    setBusy(true);
    try { await download(`/bulk/roster/download?${rosQ(sc.sel)}&from=${from}&days=${n}`, `roster-${from}-${ds.length}days.xlsx`); }
    catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const run = async (action: 'check' | 'apply') => {
    if (!file) return;
    setBusy(true);
    try {
      const r = await postForm(`/bulk/roster/${action}`, {}, file);
      if (action === 'apply') { toast(`${r.ok} change${r.ok === 1 ? '' : 's'} applied`); setRes(null); setFile(null); setLines(0); } else setRes(r);
    } catch (x: any) { toast(x.message); setRes(null); } finally { setBusy(false); }
  };
  const pick = async (f?: File) => { if (!f) return; setFile(f); setRes(null); setLines(/\.csv$/i.test(f.name) ? (await f.text()).trim().split(/\r?\n/).length - 1 : 0); };
  // One line per employee: the changes found, or the problems.
  const by = new Map<string, { line: number; id: string; name: string; err: string[]; ch: any[] }>();
  (res?.results ?? []).forEach((r: any) => { const g = by.get(r.id) ?? { line: r.line, id: r.id, name: r.name, err: [] as string[], ch: [] as any[] }; g.err.push(...r.errors); g.ch.push(...r.changes); by.set(r.id, g); });
  const G = [...by.values()], ok = G.filter((x) => !x.err.length && x.ch.length), late = ok.reduce((a, x) => a + x.ch.filter((c) => /late change/.test(c.note || '')).length, 0);
  const nch = ok.reduce((a, x) => a + x.ch.length, 0);
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Download the roster for a city, property and department as an Excel file with drop-downs, change the cells you need, and upload it back. Every change follows the same rules as the roster screen: shifts must be mapped to the department and property, late changes need a reason, past dates are locked.</div>
    <div className="card" style={{ marginBottom: 14 }}><h3>1. Choose what to download</h3><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>{filters}
      <Field label="From"><input type="date" value={from} onChange={(e) => e.target.value && setFrom(e.target.value)} /></Field>
      <Field label="Days"><select value={n} onChange={(e) => setN(e.target.value)}><option value="7">7 days</option><option value="14">14 days</option><option value="31">31 days</option></select></Field></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>{sc.emps.length} employees, {fmtD(ds[0])} to {fmtD(ds[ds.length - 1])}</p>
      <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={busy} onClick={dl}>Download roster template (Excel)</button></div></div>
    {canE ? <div className="card"><h3>2. Upload the filled file</h3><div className="row" style={{ flexWrap: 'wrap', gap: 8 }}>
      <label className="btn">Choose Excel or CSV file<input type="file" accept=".xlsx,.csv,text/csv" hidden onChange={(e) => { pick(e.target.files?.[0]); e.target.value = ''; }} /></label>
      <button className="btn primary" disabled={!file || busy} onClick={() => run('check')}>Check file</button>
      {res && ok.length > 0 && <button className="btn primary" disabled={busy} onClick={() => run('apply')}>Apply {nch} changes</button>}
      {file && <span className="mute" style={{ fontSize: 12.5 }}>{lines ? `${lines} rows loaded` : file.name}</span>}</div>
      {res && <div style={{ marginTop: 14 }}><div className="row" style={{ gap: 10, marginBottom: 8, flexWrap: 'wrap' }}><Pill k="ok">{ok.length} employees with changes</Pill><Pill k="n">{G.filter((x) => !x.err.length && !x.ch.length).length} unchanged</Pill>
        {G.some((x) => x.err.length) && <Pill k="bad">{G.filter((x) => x.err.length).length} with errors</Pill>}{late > 0 && <Pill k="warn">{late} late changes</Pill>}</div>
        <div className="tw"><table><thead><tr><th>Line</th><th>Employee</th><th>Result</th></tr></thead><tbody>
          {G.filter((x) => x.err.length || x.ch.length).slice(0, 150).map((x) => <tr key={x.id + x.line} style={{ verticalAlign: 'top' }}><td className="mono">{x.line}</td><td><b>{x.name || x.id}</b> <span className="mono mute">{x.id}</span></td>
            <td>{x.err.length ? x.err.map((m, i) => <div key={i}><Pill k="bad">{m}</Pill></div>) : <>{x.ch.slice(0, 12).map((c, i) => <div key={i} style={{ fontSize: 12.5 }}>{(c.note || '').split(' · ')[0]}: {c.from} → <b>{c.to}</b></div>)}{x.ch.length > 12 && <div className="mute" style={{ fontSize: 12 }}>and {x.ch.length - 12} more</div>}</>}</td></tr>)}
          {!G.some((x) => x.err.length || x.ch.length) && <tr><td colSpan={3} className="mute">No changes found in the file.</td></tr>}
        </tbody></table></div></div>}</div>
      : <div className="card"><p className="mute" style={{ margin: 0 }}>You can download a roster but not upload changes.</p></div>}
  </>;
}

// ---------- Consequences: warning letters and improvement plans ----------

const consLabel = (x: any) => (x.kind === 'PIP' ? `PIP ${x.seq} · ${x.days} days` : `Warning letter ${x.seq}`);
const consState = (x: any): [string, K] => (x.state === 'Review due' ? ['Review due', 'warn']
  : ({ Issued: ['Issued', 'info'], 'Pending approval': ['Waiting for ' + (x.approver_role || 'approval'), 'warn'], Active: ['Active', 'info'],
    Completed: [x.outcome === 'Improved' ? 'Completed, improved' : 'Completed, not improved', x.outcome === 'Improved' ? 'ok' : 'bad'], Rejected: ['Rejected', 'bad'], Withdrawn: ['Withdrawn', 'n'] } as Record<string, [string, K]>)[x.status] ?? [x.status, 'n']);
const HIST: Record<string, string> = { approve: 'Approved', reject: 'Rejected', withdraw: 'Withdrawn' };

function Consequences({ onChange }: { onChange: () => void }) {
  const { me } = useAuth();
  const ed = useRosEdit(), self = me?.ctx.employeeId;
  const { data, error, reload } = useLoad(() => api('/consequences'), []);
  const dir = useLoad(scopeEmps, []);
  const ym = today().slice(0, 7);
  const [q, setQ] = useState({ iq: '', q: '', kind: '', st: '' });
  const [form, setForm] = useState<any>(null);
  const [open, setOpen] = useState<any>(null);
  const rows: any[] = data?.rows ?? [];
  const emp = useMemo(() => new Map((dir.data ?? []).map((e) => [e.id, e])), [dir.data]);
  /** This month's Not OK count and discipline score, for the people with any Not OK. */
  const score = useMemo(() => new Map(Object.entries((data?.scores ?? {}) as Record<string, any>)), [data]);
  const live = (id: string) => rows.filter((x) => x.employee_id === id && !['Rejected', 'Withdrawn'].includes(x.status));
  const hist = (id: string) => { const n = live(id), a = n.filter((x) => x.kind === 'Warning').length, b = n.filter((x) => x.kind === 'PIP').length; return `${a} warning${a === 1 ? '' : 's'}, ${b} PIP${b === 1 ? '' : 's'}`; };
  const iq = q.iq.trim().toLowerCase();
  const found = ed && iq.length >= 2 ? (dir.data ?? []).filter((e) => e.id !== self && `${e.name} ${e.id}`.toLowerCase().includes(iq)).slice(0, 8) : [];
  const sug = ed ? (data?.suggest ?? []).filter((s: any) => s.id !== self).slice(0, 8) : [];
  const ql = q.q.trim().toLowerCase();
  const list = rows.filter((x) => (!q.st || (q.st === 'Due' ? x.state === 'Review due' : x.status === q.st)) && (!q.kind || x.kind === q.kind) && (!ql || `${x.name} ${x.employee_id}`.toLowerCase().includes(ql)));
  const mine = rows.filter((x) => x.canDecide).length;
  const issue = (id: string, name: string) => setForm({ id, name, kind: 'Warning', days: 7, reason: data?.reasons?.[0] ?? '', details: '' });
  const done = () => { reload(); onChange(); };
  if (!data) return <Loading error={error} />;
  return <>
    <div className="grid g4" style={{ marginBottom: 14 }}>
      <div className="card stat"><div className="l">Warning letters this month</div><div className="n">{rows.filter((x) => x.kind === 'Warning' && String(x.issued_at).slice(0, 7) === ym).length}</div></div>
      <div className="card stat"><div className="l">PIPs waiting for approval</div><div className="n">{rows.filter((x) => x.status === 'Pending approval').length}</div></div>
      <div className="card stat"><div className="l">PIPs running</div><div className="n">{rows.filter((x) => x.status === 'Active' && x.state !== 'Review due').length}</div></div>
      <div className="card stat"><div className="l">PIPs to review</div><div className="n">{rows.filter((x) => x.state === 'Review due').length}</div></div></div>
    {ed && <div className="card" style={{ marginBottom: 14 }}><div><b>Issue a warning letter or PIP</b><div className="mute" style={{ fontSize: 12.5 }}>The line manager issues. A warning letter is final once issued. A PIP of 7, 15 or 30 days is approved by the HRBP for property teams and by Corporate HR for corporate teams. Each type is numbered 1, 2, 3 as it repeats.</div></div>
      <div className="form" style={{ gridTemplateColumns: 'minmax(200px,1fr)', marginTop: 10 }}><Field label="Find employee"><input value={q.iq} onChange={(e) => setQ({ ...q, iq: e.target.value })} placeholder="Type a name or code" /></Field></div>
      {found.length > 0 && <div className="tw"><table><thead><tr><th>Employee</th><th>Department</th><th>On record</th><th /></tr></thead><tbody>{found.map((e) => <tr key={e.id}><td>{e.name} <span className="mono mute">{e.id}</span></td><td>{e.department}</td><td>{hist(e.id)}</td><td><button className="btn" onClick={() => issue(e.id, e.name)}>Issue</button></td></tr>)}</tbody></table></div>}
      {sug.length > 0 && <div style={{ marginTop: 12 }}><div className="mute" style={{ fontSize: 12.5, marginBottom: 6 }}>Marked Not OK in {ymLabel(ym)}</div><div className="tw"><table><thead><tr><th>Employee</th><th className="num">Not OK</th><th className="num">Score</th><th>On record</th><th /></tr></thead><tbody>
        {sug.map((s: any) => <tr key={s.id}><td>{s.name} <span className="mono mute">{s.id}</span></td><td className="num">{s.not_ok}</td><td className="num">{s.score}{score.get(s.id)?.start < 100 && <div className="mute" style={{ fontSize: 11.5 }}>Started at {score.get(s.id).start}{score.get(s.id).startNote ? ` (${score.get(s.id).startNote})` : ''}</div>}</td><td>{hist(s.id)}</td><td><button className="btn" onClick={() => issue(s.id, s.name)}>Issue</button></td></tr>)}</tbody></table></div></div>}</div>}
    <div className="card"><div className="row sp" style={{ flexWrap: 'wrap', gap: 10, marginBottom: 10 }}><b>{list.length} record{list.length === 1 ? '' : 's'}{mine > 0 && <> · <span className="mute">{mine} for you to decide</span></>}</b>
      <div className="row" style={{ gap: 8 }}><input value={q.q} onChange={(e) => setQ({ ...q, q: e.target.value })} placeholder="Search" style={{ minWidth: 150, width: 'auto' }} />
        <select value={q.kind} onChange={(e) => setQ({ ...q, kind: e.target.value })}><option value="">All types</option><option>Warning</option><option>PIP</option></select>
        <select value={q.st} onChange={(e) => setQ({ ...q, st: e.target.value })}>{[['', 'All status'], ['Issued', 'Issued'], ['Pending approval', 'Waiting for approval'], ['Active', 'Running'], ['Due', 'Review due'], ['Completed', 'Completed'], ['Rejected', 'Rejected'], ['Withdrawn', 'Withdrawn']].map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></div></div>
      {list.length ? <div className="tw"><table><thead><tr><th>Employee</th><th>Type</th><th>Reason</th><th>Issued</th><th>Approver</th><th>Status</th><th /></tr></thead><tbody>
        {list.slice(0, 100).map((x) => { const st = consState(x); return <tr key={x.id}><td>{x.name} <span className="mono mute">{x.employee_id}</span></td><td>{consLabel(x)}</td><td>{x.reason}</td>
          <td>{fmtD(String(x.issued_at).slice(0, 10))}<div className="mute" style={{ fontSize: 12 }}>{x.issued_by}</div></td>
          <td>{x.kind === 'PIP' ? <>{x.approver_role}{x.approver_name && <div className="mute" style={{ fontSize: 12 }}>{x.approver_name}</div>}</> : <span className="mute">Not needed</span>}</td>
          <td><Pill k={st[1]}>{st[0]}</Pill>{x.from_date && <div className="mute" style={{ fontSize: 12 }}>{fmtD(x.from_date)} to {fmtD(x.to_date)}</div>}</td>
          <td style={{ whiteSpace: 'nowrap' }}><button className="btn" onClick={() => setOpen(x)}>Open</button></td></tr>; })}
      </tbody></table></div> : <p className="mute">Nothing here yet.</p>}</div>
    {form && <IssueForm f={form} setF={setForm} e={emp.get(form.id)} live={live(form.id)} sc={score.get(form.id)} reasons={data.reasons} onDone={() => { setForm(null); done(); }} />}
    {open && <ConsOpen x={open} e={emp.get(open.employee_id)} edit={ed && open.employee_id !== self} onClose={() => setOpen(null)} onDone={() => { setOpen(null); done(); }} />}
  </>;
}

function IssueForm({ f, setF, e, live, sc, reasons, onDone }: { f: any; setF: (f: any) => void; e?: ScopeEmp; live: any[]; sc?: any; reasons: string[]; onDone: () => void }) {
  const pw = live.filter((x) => x.kind === 'Warning').length + 1, pp = live.filter((x) => x.kind === 'PIP').length + 1;
  const op = live.find((x) => x.kind === 'PIP' && ['Pending approval', 'Active'].includes(x.status));
  // Who approves a plan for this employee, known before it is raised (prototype consAppRole, consAppName).
  const ap = useLoad(() => api<{ role: string; name: string }>(`/consequences/approver?employeeId=${encodeURIComponent(f.id)}`).catch(() => null), [f.id]);
  const role = ap.data?.role ?? (e?.corporate ? 'HRBP Corporate' : 'HRBP');
  const apName = ap.data?.name && ap.data.name !== 'Unassigned' ? ` (${ap.data.name})` : '';
  const close = () => setF(null);
  const save = async () => {
    if (f.details.trim().length < 10) { toast('Write what happened and what is expected'); return; }
    if (f.kind === 'PIP' && op) { toast('A PIP is already open for this employee'); return; }
    try {
      await api('/consequences', { body: { employeeId: f.id, kind: f.kind, days: +f.days, reason: f.reason, details: f.details } });
      toast(f.kind === 'PIP' ? `PIP ${pp} · ${f.days} days sent to ${role} for approval` : `Warning letter ${pw} issued`); onDone();
    } catch (x: any) { toast(x.message); }
  };
  return <Modal wide onClose={close}>
    <div className="row sp" style={{ marginBottom: 10 }}><h2 style={{ margin: 0 }}>Issue to {f.name}</h2><button className="btn" onClick={close}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>{e ? `${e.designation}, ${e.department}. ` : ''}This month: {sc?.no ?? 0} Not OK, discipline score {sc?.score ?? 100}{sc?.start < 100 ? ` (started at ${sc.start}${sc.startNote ? `, ${sc.startNote}` : ''})` : ''}. Next warning letter is number {pw}. Next PIP is number {pp}.</p>
    {op && <div className="note bad" style={{ marginBottom: 10 }}>A PIP is already {op.status === 'Active' ? 'running' : 'waiting for approval'} for this employee. Close it before issuing another one.</div>}
    <div className="form">
      <Field label="Type"><select value={f.kind} onChange={(x) => setF({ ...f, kind: x.target.value })}><option value="Warning">Warning letter {pw}</option><option value="PIP" disabled={!!op}>PIP {pp}</option></select></Field>
      <Field label="PIP length"><select value={f.days} onChange={(x) => setF({ ...f, days: +x.target.value })}>{[7, 15, 30].map((d) => <option key={d} value={d}>{d} days</option>)}</select></Field>
      <label className="field" style={{ gridColumn: '1/-1' }}><span>Reason</span><select value={f.reason} onChange={(x) => setF({ ...f, reason: x.target.value })}>{reasons.map((r) => <option key={r}>{r}</option>)}</select></label>
      <label className="field" style={{ gridColumn: '1/-1' }}><span>What happened and what is expected</span><textarea rows={4} value={f.details} onChange={(x) => setF({ ...f, details: x.target.value })} placeholder="Write the facts, dates and the improvement expected" /></label>
    </div>
    <p className="mute" style={{ fontSize: 12.5 }}>A PIP goes to {role}{apName} for approval. A warning letter needs no approval. Length applies to a PIP only.</p>
    <div className="row" style={{ gap: 8, marginTop: 10 }}><button className="btn primary" onClick={save}>Issue</button></div>
  </Modal>;
}

function ConsOpen({ x, e, edit, onClose, onDone }: { x: any; e?: ScopeEmp; edit: boolean; onClose: () => void; onDone: () => void }) {
  const [note, setNote] = useState('');
  const [out, setOut] = useState('Improved');
  const [cl, setCl] = useState('');
  const st = consState(x);
  const act = async (action: string, body: any, msg: string) => { try { await api(`/consequences/${x.id}/${action}`, { body }); toast(msg); onDone(); } catch (err: any) { toast(err.message); } };
  const decide = (ok: boolean) => { if (!ok && !note.trim()) { toast('Add a note to reject'); return; } act(ok ? 'approve' : 'reject', { note }, ok ? 'PIP approved and started' : 'PIP rejected'); };
  const withdraw = () => { const r = prompt('Why is it withdrawn?'); if (r?.trim()) act('withdraw', { note: r }, 'Withdrawn. The record stays for audit'); };
  const history = (x.history ?? []) as any[];
  const closeNote = history.find((h) => /^completed/.test(String(h.what)))?.note;
  return <Modal wide onClose={onClose}>
    <div className="row sp" style={{ marginBottom: 10 }}><div><h2 style={{ margin: 0 }}>{consLabel(x)}</h2><div className="mute">{x.name} · {x.employee_id}{x.department ?? e?.department ? ` · ${x.department ?? e?.department}` : ''}</div></div><div className="row" style={{ gap: 8 }}><Pill k={st[1]}>{st[0]}</Pill><button className="btn" onClick={onClose}>Close</button></div></div>
    <p><b>Reason:</b> {x.reason}</p><p style={{ whiteSpace: 'pre-wrap' }}>{x.details}</p>
    {x.from_date && <p><b>Plan period:</b> {fmtD(x.from_date)} to {fmtD(x.to_date)}</p>}{x.outcome && <p><b>Result:</b> {x.outcome}{closeNote ? '. ' + closeNote : ''}</p>}
    <ul className="mute" style={{ fontSize: 12.5 }}><li>{fmtD(String(x.issued_at).slice(0, 10))}: issued by {x.issued_by} ({x.issued_role})</li>
      {history.map((h, i) => <li key={i}>{fmtD(h.at)}: {HIST[h.what] ?? String(h.what).replace(/^completed/, 'Closed')} by {h.by}{h.note ? '. ' + h.note : ''}</li>)}</ul>
    {x.canDecide && <><div className="field" style={{ marginTop: 10 }}><span>Note (needed to reject)</span><input value={note} onChange={(ev) => setNote(ev.target.value)} /></div>
      <div className="row" style={{ gap: 8, marginTop: 10 }}><button className="btn primary" onClick={() => decide(true)}>Approve</button><button className="btn" onClick={() => decide(false)}>Reject</button></div></>}
    {x.status === 'Active' && edit && <><div className="form" style={{ marginTop: 10 }}><Field label="Result of the PIP"><select value={out} onChange={(ev) => setOut(ev.target.value)}><option>Improved</option><option>Not improved</option></select></Field><Field label="Note"><input value={cl} onChange={(ev) => setCl(ev.target.value)} /></Field></div>
      <div className="row" style={{ gap: 8, marginTop: 10 }}><button className="btn primary" onClick={() => act('complete', { outcome: out, note: cl }, 'PIP closed')}>Close the PIP</button></div></>}
    <div className="row" style={{ gap: 8, marginTop: 10 }}>{x.status !== 'Pending approval' && x.status !== 'Rejected' && <button className="btn" onClick={() => letterFile(x.id, `${x.kind === 'PIP' ? 'pip' : 'warning'}-${x.employee_id}-${x.seq}.html`)}>Download letter</button>}
      {['Issued', 'Pending approval'].includes(x.status) && edit && <button className="btn" onClick={withdraw}>Withdraw</button>}</div>
  </Modal>;
}
