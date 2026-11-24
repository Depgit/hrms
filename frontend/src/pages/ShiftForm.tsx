import { useEffect, useState, type ReactNode } from 'react';
import { ApiError, api, fmtD, today } from '../api';
import { Loading, Modal, Pill, Switch, toast } from '../components/ui';

// Set shift (Masters › Shifts): a shift's timings from an effective date, and where it applies. Saving the timings
// writes a version from the effective date; earlier days keep theirs. A shift may have a second part (a split shift).
// The punch rules, the single punch rule (24 Nov 2026) and the attendance rules card (owner, 24 Nov 2026) were removed:
// the work policy decides them. Where it applies is a list of Department + Location rows (either may be All), changed
// row by row (Add, switch on or off, Remove) and kept in the mapping history.

/** What a shift version keeps beyond its timings: only whether it ends the next day (set from the times). */
export type Rules = { nextDay?: boolean };
/** Shift rules that were removed (8 and 24 Nov 2026); an older version may still carry them. */
const REMOVED_RULES = ['otAbove', 'withoutBreak', 'startFrom', 'endFrom', 'addMissingOut', 'forcedOut', 'earlyInMax', 'lateOutMax', 'officeHours', 'shiftIn', 'singlePunch',
  'maxHours', 'basedOn', 'absentBelow', 'halfBelow', 'fullAbove', 'weeklyAdj', 'monthlyAdj', 'shortfall'];
type Pair = { departmentId: string; locationId: string };
type Form = { code: string; name: string; effectiveFrom: string; startTime: string; endTime: string; start2Time: string; end2Time: string; breakMinutes: string; maps: Pair[]; rules: Rules };

/** A shift's timings as text: "09:00–18:00", or "06:00–10:00 + 15:00–19:00" for a split shift. */
export const timingText = (start?: string, end?: string, start2?: string | null, end2?: string | null) =>
  `${start ?? ''}–${end ?? ''}${start2 && end2 ? ` + ${start2}–${end2}` : ''}`;

/** Whether a shift of the master list (its switched-on rows, maps) is offered to someone of the department at the location
 *  (ids; "" when not chosen yet, which only a row for All matches). The server checks the same. */
export const shiftOffered = (s: { maps?: Pair[] }, deptId: string, locId: string) =>
  (s.maps ?? []).some((m) => (!m.departmentId || m.departmentId === deptId) && (!m.locationId || m.locationId === locId));

const mins = (t?: string) => { const m = /^(\d{1,2}):(\d{2})$/.exec(t || ''); return m ? +m[1] * 60 + +m[2] : null; };
const crosses = (a: string, b: string) => { const x = mins(a), y = mins(b); return x != null && y != null && y <= x; };
const TIME = /^([01]\d|2[0-3]):[0-5]\d$/;

/** What changed from the version before (timings). */
function changes(v: any, prev: any): string[] {
  if (!prev) return ['First version'];
  const out: string[] = [];
  if (v.startTime !== prev.startTime) out.push(`Start ${prev.startTime} → ${v.startTime}`);
  if (v.endTime !== prev.endTime) out.push(`End ${prev.endTime} → ${v.endTime}`);
  const p2 = (x: any) => (x.start2Time && x.end2Time ? `${x.start2Time}–${x.end2Time}` : 'none');
  if (p2(v) !== p2(prev)) out.push(`Second part ${p2(prev)} → ${p2(v)}`);
  if (v.breakMinutes !== prev.breakMinutes) out.push(`Break ${prev.breakMinutes} → ${v.breakMinutes} min`);
  if (!!v.rules?.nextDay !== !!prev.rules?.nextDay) out.push(`Ends next day: ${prev.rules?.nextDay ? 'Yes' : 'No'} → ${v.rules?.nextDay ? 'Yes' : 'No'}`);
  return out.length ? out : ['No change'];
}

const blank = (): Form => ({ code: '', name: '', effectiveFrom: today(), startTime: '09:00', endTime: '18:00', start2Time: '', end2Time: '', breakMinutes: '30', maps: [], rules: {} });
const formOf = (v: any): Form => {
  const c = v.current, s = v.shift;
  // Removed rules (REMOVED_RULES) are left out: the server no longer accepts them.
  const rules: Record<string, any> = { ...(c.rules || {}) };
  for (const k of REMOVED_RULES) delete rules[k];
  return { code: s.code, name: s.name, effectiveFrom: today(), startTime: c.startTime, endTime: c.endTime, start2Time: c.start2Time ?? '', end2Time: c.end2Time ?? '',
    breakMinutes: String(c.breakMinutes ?? 0), maps: [], rules: rules as Rules };
};

type Err = { msg: string; field?: string } | null;

/** One field: label, control, help and its error. */
function Box({ err, k, label, req, help, children, wide }: { err: Err; k: string; label: string; req?: boolean; help?: ReactNode; children: ReactNode; wide?: boolean }) {
  const e = err?.field === k ? err.msg : '';
  return <div className={`field${e ? ' err' : ''}`} style={wide ? { gridColumn: '1/-1' } : undefined}>
    <span>{label}{req ? ' *' : ''}</span>
    {children}
    {help && <small>{help}</small>}
    {e && <small style={{ color: 'var(--bad)' }} role="alert">{e}</small>}
  </div>;
}

function Radios({ k, opts, value, onPick }: { k: string; opts: [string, string][]; value?: string; onPick: (v: string) => void }) {
  return <div className="row" style={{ gap: 14 }}>{opts.map(([v, l]) => <label key={v} className="chkl"><input type="radio" name={`sf-${k}`} checked={value === v} onChange={() => onPick(v)} />{l}</label>)}</div>;
}

/** Department and Location pickers (All, or one active item) with an Add button. */
function PairPicker({ depts, locs, onAdd, busy }: { depts: any[]; locs: any[]; onAdd: (p: Pair) => void; busy?: boolean }) {
  const [p, setP] = useState<Pair | null>(null);
  const cities = [...new Set(locs.map((l) => l.city))];
  return <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'flex-end' }}>
    <label className="field" style={{ minWidth: 200 }}><span>Department</span>
      <select value={p?.departmentId ?? '-'} onChange={(e) => setP({ departmentId: e.target.value, locationId: p?.locationId ?? '-' })}>
        <option value="-" disabled>Choose a department</option>
        <option value="">All departments</option>{depts.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}</select></label>
    <label className="field" style={{ minWidth: 220 }}><span>Location</span>
      <select value={p?.locationId ?? '-'} onChange={(e) => setP({ departmentId: p?.departmentId ?? '-', locationId: e.target.value })}>
        <option value="-" disabled>Choose a location</option>
        <option value="">All locations</option>
        {cities.map((c) => <optgroup key={c} label={c}>{locs.filter((l) => l.city === c).map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</optgroup>)}</select></label>
    <button type="button" className="btn" disabled={busy || !p || p.departmentId === '-' || p.locationId === '-'} onClick={() => { onAdd(p!); setP(null); }}>Add row</button>
  </div>;
}

export default function ShiftForm({ id, locs, depts, edit, onBack }: { id: string | null; locs: any[]; depts: any[]; edit: boolean; onBack: (changed: boolean) => void }) {
  const [sid, setSid] = useState(id);
  const [view, setView] = useState<any>(null);
  const [loadErr, setLoadErr] = useState('');
  const [f, setF] = useState<Form | null>(null);
  const [err, setErr] = useState<Err>(null);
  const [ask, setAsk] = useState<{ from: string; note: string } | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let live = true;
    api(`/shifts/${sid ?? 'new'}`).then((v) => { if (!live) return; setView(v); setF(v.shift ? formOf(v) : blank()); }).catch((e) => live && setLoadErr(e.message));
    return () => { live = false; };
  }, [sid]);
  if (loadErr) return <div className="note bad">{loadErr}</div>;
  if (!view || !f) return <Loading />;

  const pd = view.policyDefaults || {};
  const r = f.rules;
  const set = (patch: Partial<Form>) => { setF({ ...f, ...patch }); setErr(null); };
  const setR = (k: keyof Rules, v: any) => { const x = { ...r }; if (v === undefined || v === '') delete x[k]; else (x as any)[k] = v; set({ rules: x }); };
  const nextDay = r.nextDay ?? crosses(f.startTime, f.endTime);
  const split = !!(f.start2Time || f.end2Time);
  const deptName = (id: string) => (id ? depts.find((d) => d.id === id)?.name ?? id : 'All departments');
  const locName = (id: string) => (id ? locs.find((l) => l.id === id)?.name ?? id : 'all locations');
  const pairLabel = (p: Pair) => `${deptName(p.departmentId)} at ${locName(p.locationId)}`;

  /** Checks on screen before the server's own (the server says the same and more). */
  const check = (): Err => {
    if (!f.name.trim()) return { field: 'name', msg: 'Enter the shift name' };
    if (!sid && !/^[A-Za-z0-9][A-Za-z0-9-]{0,9}$/.test(f.code.trim())) return { field: 'code', msg: 'Enter a short code: letters and digits, up to 10' };
    if (!TIME.test(f.startTime)) return { field: 'startTime', msg: 'Start time must be HH:MM' };
    if (!TIME.test(f.endTime)) return { field: 'endTime', msg: 'End time must be HH:MM' };
    if (split) {
      if (!TIME.test(f.start2Time)) return { field: 'start2Time', msg: 'Second part start must be HH:MM. Leave both second part times empty for a shift in one part' };
      if (!TIME.test(f.end2Time)) return { field: 'end2Time', msg: 'Second part end must be HH:MM. Leave both second part times empty for a shift in one part' };
      if (nextDay || crosses(f.startTime, f.endTime)) return { field: 'end2Time', msg: 'A split shift must start and end on the same day: the first part must end after it starts' };
      if (mins(f.start2Time)! <= mins(f.endTime)!) return { field: 'start2Time', msg: `The second part must start after the first part ends (${f.endTime})` };
      if (mins(f.end2Time)! <= mins(f.start2Time)!) return { field: 'end2Time', msg: 'The second part must end after it starts, on the same day' };
    }
    if (!sid && !f.maps.length) return { field: 'maps', msg: 'Add at least one row: the department and location the shift applies to (either may be All)' };
    return null;
  };
  const save = async () => {
    const a = ask!;
    const body: Record<string, any> = { code: f.code.trim(), name: f.name.trim(), effectiveFrom: a.from, startTime: f.startTime, endTime: f.endTime, start2Time: f.start2Time, end2Time: f.end2Time,
      breakMinutes: +f.breakMinutes || 0, rules: { ...r, nextDay }, note: a.note.trim() };
    if (!sid) body.maps = f.maps;
    try {
      const v = sid ? await api(`/shifts/${sid}`, { method: 'PUT', body }) : await api('/shifts', { body });
      toast(`Saved. The shift applies from ${fmtD(a.from)}`);
      setAsk(null); setSaved(true);
      if (!sid) { setSid(v.shift.id); return; }
      setView(v); setF({ ...formOf(v), effectiveFrom: a.from });
    } catch (x: any) {
      setAsk(null);
      const field = x instanceof ApiError ? (x.details as any)?.field : undefined;
      setErr({ msg: x.message, field });
    }
  };
  const askSave = () => { const e = check(); if (e) { setErr(e); return; } setAsk({ from: f.effectiveFrom || today(), note: '' }); };

  // Rows of a saved shift change at once, each on its own (audited, in the mapping history).
  const rowCall = async (path: string, method: string, body?: any, done?: string) => {
    setBusy(true);
    try {
      const v = await api(`/shifts/${sid}/maps${path}`, { method, body });
      setView(v); setSaved(true);
      toast((done ?? 'Saved') + (v.outsideNow ? `. ${v.outsideNow} ${v.outsideNow === 1 ? 'person' : 'people'} on this shift ${v.outsideNow === 1 ? 'is' : 'are'} outside its rows now: their roster stays, a new assignment is refused` : ''));
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const addLocal = (p: Pair) => {
    if (f.maps.some((m) => m.departmentId === p.departmentId && m.locationId === p.locationId)) { setErr({ field: 'maps', msg: `${pairLabel(p)} is already in the list` }); return; }
    set({ maps: [...f.maps, p] });
  };

  const vers: any[] = view.versions || [];
  const rows: any[] = view.maps || [];
  const hist: any[] = view.mapHistory || [];
  const t = today();
  return <>
    <div className="row sp" style={{ marginBottom: 14 }}>
      <div><h2 style={{ margin: 0 }}>{sid ? `Set shift: ${view.shift?.name ?? f.name}` : 'Add shift'}{sid && <span className="mono mute" style={{ fontSize: 14, marginLeft: 8 }}>{view.shift?.code}</span>}</h2>
        <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>
          {sid ? <>In effect today: {timingText(view.current?.startTime, view.current?.endTime, view.current?.start2Time, view.current?.end2Time)}, from {view.current?.effectiveFrom === '2000-01-01' ? 'the start' : fmtD(view.current?.effectiveFrom)}. </> : null}
          The work policy decides the attendance rules{pd.policy ? <> (<b>{pd.policy}</b>{pd.employees ? `, ${pd.employees} on this shift` : ''})</> : null}.</div></div>
      <div className="row"><button className="btn" onClick={() => onBack(saved)}>Back to shifts</button>{edit && <button className="btn primary" onClick={askSave}>Save</button>}</div>
    </div>
    {!edit && <div className="note" style={{ marginBottom: 14 }}>You can view this shift. Changing it needs the Masters: job right.</div>}
    {err && <div className="note bad" role="alert" style={{ marginBottom: 14 }}>{err.msg}</div>}
    <fieldset disabled={!edit} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>
      <div className="card" style={{ marginBottom: 14 }}><h3>Shift</h3>
        <div className="form">
          <Box err={err} k="name" label="Shift name" req><input type="text" value={f.name} placeholder="Shift name" onChange={(e) => set({ name: e.target.value })} /></Box>
          <Box err={err} k="code" label="Code" req help={sid ? 'The code cannot change.' : 'Used on the roster, for example N2.'}><input type="text" value={f.code} disabled={!!sid} onChange={(e) => set({ code: e.target.value.toUpperCase() })} /></Box>
          <Box err={err} k="effectiveFrom" label="Shift effective date" req help={view.lockedUntil ? `Attendance is locked by payroll up to ${fmtD(view.lockedUntil)}.` : 'Days before this date keep the earlier version.'}>
            <input type="date" value={f.effectiveFrom} onChange={(e) => set({ effectiveFrom: e.target.value })} /></Box>
          <Box err={err} k="startTime" label="Start time" req><input type="time" value={f.startTime} onChange={(e) => set({ startTime: e.target.value })} /></Box>
          <Box err={err} k="endTime" label={split ? 'End time (first part)' : 'End time'} req><input type="time" value={f.endTime} onChange={(e) => set({ endTime: e.target.value })} /></Box>
          <Box err={err} k="start2Time" label="Second part start" help="Optional, for a split shift such as 06:00–10:00 and 15:00–19:00. The gap between the parts is the break and never counts as hours. Both parts are on the same day.">
            <input type="time" value={f.start2Time} onChange={(e) => set({ start2Time: e.target.value })} /></Box>
          <Box err={err} k="end2Time" label="Second part end" help={split ? <>Hours are the time worked in both parts added together. The late mark is measured from the first part's start. <button type="button" className="btn sm" style={{ padding: '1px 8px', fontSize: 11 }} onClick={() => set({ start2Time: '', end2Time: '' })}>No second part</button></> : 'Leave both second part times empty for a shift in one part.'}>
            <input type="time" value={f.end2Time} onChange={(e) => set({ end2Time: e.target.value })} /></Box>
          <Box err={err} k="nextDay" label="Shift ending next day" help={nextDay ? 'Yes: the day is built from the punches between the start and the next morning\'s end (4:00 either side unless the work policy sets a maximum early in or late out punch).' : 'Set from the times. Yes needs an end at or before the start.'}>
            <Radios k="nextDay" opts={[['yes', 'Yes'], ['no', 'No']]} value={nextDay ? 'yes' : 'no'} onPick={(v) => setR('nextDay', v === 'yes')} /></Box>
          <Box err={err} k="breakMinutes" label="Break minutes" help={split ? 'Shown on the shift for information. On a split shift only the gap between the parts is taken off the hours worked.' : 'Shown on the shift for information. No break is taken off the hours worked: they run from the in punch to the out punch.'}><input type="number" min={0} value={f.breakMinutes} onChange={(e) => set({ breakMinutes: e.target.value })} /></Box>
        </div>
      </div>

      <div className="card" style={{ marginBottom: 14 }}><h3>Applies to</h3>
        <p className="mute" style={{ fontSize: 12.5, marginTop: -6 }}>Department + Location rows; either may be All. Forms, rosters and uploads offer the shift to an employee only when a switched-on row matches their department and location. People already on the shift whom no row covers keep it: their roster stays, a new assignment is refused.</p>
        {err?.field === 'maps' && <div className="note bad" role="alert" style={{ marginBottom: 10 }}>{err.msg}</div>}
        {!sid ? <>
          {f.maps.length ? <div className="tw" style={{ marginBottom: 10 }}><table>
            <thead><tr><th>Department</th><th>Location</th><th></th></tr></thead>
            <tbody>{f.maps.map((m, i) => <tr key={i}><td>{deptName(m.departmentId)}</td><td>{m.locationId ? locName(m.locationId) : 'All locations'}</td>
              <td><button type="button" className="btn sm" onClick={() => set({ maps: f.maps.filter((_, j) => j !== i) })}>Remove</button></td></tr>)}</tbody>
          </table></div> : <p className="mute" style={{ fontSize: 12.5 }}>No rows yet. Add at least one.</p>}
          {edit && <PairPicker depts={depts} locs={locs} onAdd={addLocal} />}
        </> : <>
          {view.outside > 0 && <div className="note" style={{ marginBottom: 10 }}>{view.outside} {view.outside === 1 ? 'person' : 'people'} on this shift {view.outside === 1 ? 'is' : 'are'} outside its rows: their roster stays, a new assignment of the shift to them is refused.</div>}
          <div className="tw" style={{ marginBottom: 10 }}><table>
            <thead><tr><th>Department</th><th>Location</th><th>On</th><th>Added</th><th>Last switched</th>{edit && <th></th>}</tr></thead>
            <tbody>{rows.map((m) => <tr key={m.id}>
              <td>{m.department}</td><td>{m.location}</td>
              <td><Switch on={!!m.on} disabled={!edit || busy} label={`${m.label} on`} onChange={() => rowCall(`/${m.id}`, 'PUT', { on: !m.on }, m.on ? 'Switched off' : 'Switched on')} /></td>
              <td className="mute" style={{ fontSize: 12 }}>{m.createdBy || '—'}{m.createdAt ? ` · ${fmtD(m.createdAt)}` : ''}</td>
              <td className="mute" style={{ fontSize: 12 }}>{m.changedBy ? `${m.changedBy} · ${fmtD(m.changedAt)}` : '—'}</td>
              {edit && <td><button type="button" className="btn sm" disabled={busy} onClick={() => { if (window.confirm(`Remove ${m.label} from this shift? The history keeps it.`)) rowCall(`/${m.id}`, 'DELETE', undefined, 'Removed'); }}>Remove</button></td>}
            </tr>)}</tbody>
          </table></div>
          {edit && <PairPicker depts={depts} locs={locs} busy={busy} onAdd={(p) => rowCall('', 'POST', p, `Added ${pairLabel(p)}`)} />}
        </>}
      </div>
    </fieldset>

    {sid && <div className="card" style={{ marginBottom: 14 }}><h3>Version history</h3>
      <div className="tw"><table>
        <thead><tr><th>Effective from</th><th>Timings</th><th>What changed</th><th>By</th><th>Saved</th><th>Note</th></tr></thead>
        <tbody>{vers.map((v, i) => <tr key={v.id ?? i}>
          <td className="mono">{v.effectiveFrom === '2000-01-01' ? 'The start' : fmtD(v.effectiveFrom)}{' '}{v.current ? <Pill k="ok">In effect</Pill> : v.effectiveFrom > t ? <Pill k="warn">Upcoming</Pill> : null}</td>
          <td className="mono">{timingText(v.startTime, v.endTime, v.start2Time, v.end2Time)}{v.breakMinutes ? ` · ${v.breakMinutes} min break` : ''}</td>
          <td className="ink2" style={{ fontSize: 12.5 }}>{changes(v, vers[i + 1]).map((c, j) => <div key={j}>{c}</div>)}</td>
          <td>{v.createdBy || '—'}</td><td className="mute">{v.createdAt ? fmtD(v.createdAt) : '—'}</td><td className="ink2">{v.note || '—'}</td>
        </tr>)}</tbody>
      </table></div>
    </div>}

    {sid && <div className="card"><h3>Applies to: history</h3>
      {!hist.length ? <p className="mute" style={{ margin: 0 }}>No changes yet.</p> : <div className="tw"><table>
        <thead><tr><th>When</th><th>By</th><th>Change</th></tr></thead>
        <tbody>{hist.map((h, i) => <tr key={i}>
          <td className="mute" style={{ whiteSpace: 'nowrap' }}>{h.at ? fmtD(h.at) : '—'}</td><td>{h.by || '—'}</td>
          <td className="ink2" style={{ fontSize: 12.5 }}>{(h.changes || []).map((c: any, j: number) => <div key={j}>{String(c.field || '').replace(/^Applies to: /, '')}: {c.from || 'new'} → {c.to}{c.note ? <span className="mute"> · {c.note}</span> : null}</div>)}</td>
        </tr>)}</tbody>
      </table></div>}
    </div>}

    {ask && <Modal onClose={() => setAsk(null)}>
      <h2>Save {f.name || 'the shift'}</h2>
      <p className="ink2">The timings apply from this date. Days before it keep the earlier version. A date in a month whose attendance is locked by payroll is refused.{!sid ? ' The rows under Applies to take effect at once.' : ''}</p>
      <div className="grid g2" style={{ margin: '10px 0' }}>
        <label className="field"><span>Effective from</span><input type="date" value={ask.from} onChange={(e) => setAsk({ ...ask, from: e.target.value })} /></label>
        <label className="field"><span>Note for the history (optional)</span><input type="text" value={ask.note} onChange={(e) => setAsk({ ...ask, note: e.target.value })} /></label>
      </div>
      <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={save} disabled={!ask.from}>Save from {ask.from ? fmtD(ask.from) : '…'}</button><button className="btn" onClick={() => setAsk(null)}>Cancel</button></div>
    </Modal>}
  </>;
}
