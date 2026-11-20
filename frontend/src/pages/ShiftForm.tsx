import { useEffect, useState, type ReactNode } from 'react';
import { ApiError, api, fmtD, today } from '../api';
import { Loading, Modal, Pill, toast } from '../components/ui';

// Set shift (Masters › Shifts): a shift's timings and attendance rules from an effective date, drawn section by
// section after the client's "Set Shifts" screen. Every rule is optional: left empty it follows the employee's
// work policy, shown under the field. Saving writes a version from the effective date; earlier days keep theirs.

type Adj = { on: boolean; hours?: string };
type Incl = { wo?: boolean; hol?: boolean; optHol?: boolean };
export type Rules = {
  nextDay?: boolean; startFrom?: string; endFrom?: string; addMissingOut?: boolean; forcedOut?: string; earlyInMax?: string; lateOutMax?: string;
  officeHours?: string; shiftIn?: string; singlePunch?: string; maxHours?: string; basedOn?: string; absentBelow?: string; halfBelow?: string;
  fullAbove?: string; weeklyAdj?: Adj; monthlyAdj?: Adj; shortfall?: Incl;
};
type Form = { code: string; name: string; effectiveFrom: string; startTime: string; endTime: string; breakMinutes: string; locMode: 'all' | 'some'; locIds: string[]; rules: Rules };

const mins = (t?: string) => { const m = /^(\d{1,2}):(\d{2})$/.exec(t || ''); return m ? +m[1] * 60 + +m[2] : null; };
const crosses = (a: string, b: string) => { const x = mins(a), y = mins(b); return x != null && y != null && y <= x; };
const DUR = /^\d{1,2}:[0-5]\d$/, DUR3 = /^\d{1,3}:[0-5]\d$/, TIME = /^([01]\d|2[0-3]):[0-5]\d$/;
const CHOICES: Record<string, [string, string][]> = {
  startFrom: [['shift', 'Shift start time'], ['punch', 'In punch']],
  endFrom: [['shift', 'Shift end time'], ['punch', 'Out punch']],
  shiftIn: [['current', 'Current day'], ['previous', 'Previous day'], ['next', 'Next day']],
  singlePunch: [['absent', 'Absent full day'], ['half', 'Present half day'], ['present', 'Present full day']],
  basedOn: [['hours', 'Hours'], ['shift', 'Shift'], ['punch', 'Punch']],
};
const choice = (k: string, v?: string) => CHOICES[k]?.find((c) => c[0] === v)?.[1] ?? v ?? '';
const yn = (v?: boolean) => (v ? 'Yes' : 'No');

/** Each rule's label and how its value reads (in the summary and the version history). */
const LABELS: [keyof Rules, string, (v: any) => string][] = [
  ['nextDay', 'Ends next day', yn], ['startFrom', 'Start from', (v) => choice('startFrom', v)], ['endFrom', 'End from', (v) => choice('endFrom', v)],
  ['addMissingOut', 'Add missing out', yn], ['forcedOut', 'Forced out', String], ['earlyInMax', 'Early in max', String], ['lateOutMax', 'Late out max', String],
  ['officeHours', 'Office hours', String], ['shiftIn', 'Counted on', (v) => choice('shiftIn', v)], ['singlePunch', 'Single punch', (v) => choice('singlePunch', v)],
  ['maxHours', 'Max hours', String], ['basedOn', 'Based on', (v) => choice('basedOn', v)], ['absentBelow', 'Absent <', String], ['halfBelow', 'Half day <', String],
  ['fullAbove', 'Full day ≥', String], ['weeklyAdj', 'Weekly adjustment', (v: Adj) => v?.on ? v.hours || 'on' : 'off'],
  ['monthlyAdj', 'Monthly adjustment', (v: Adj) => v?.on ? v.hours || 'on' : 'off'],
  ['shortfall', 'Shortfall includes', (v: Incl) => [v?.wo && 'weekly off', v?.hol && 'holiday', v?.optHol && 'optional holiday'].filter(Boolean).join(', ') || 'none'],
];

/** Short summary of a shift's rules for the list ("Policy rules" when none is set). */
export const rulesSummary = (r?: Rules | null): string[] => {
  const out: string[] = [];
  for (const [k, l, f] of LABELS) {
    const v = r?.[k];
    if (v === undefined || v === null || v === '' || (k === 'nextDay' && v === false)) continue;
    out.push(k === 'nextDay' ? 'Ends next day' : `${l} ${f(v)}`);
  }
  return out;
};

/** What changed from the version before (timings and rules). */
function changes(v: any, prev: any): string[] {
  if (!prev) return ['First version'];
  const out: string[] = [];
  if (v.startTime !== prev.startTime) out.push(`Start ${prev.startTime} → ${v.startTime}`);
  if (v.endTime !== prev.endTime) out.push(`End ${prev.endTime} → ${v.endTime}`);
  if (v.breakMinutes !== prev.breakMinutes) out.push(`Break ${prev.breakMinutes} → ${v.breakMinutes} min`);
  for (const [k, l, f] of LABELS) {
    const a = prev.rules?.[k], b = v.rules?.[k];
    if (JSON.stringify(a ?? null) === JSON.stringify(b ?? null)) continue;
    out.push(`${l}: ${a === undefined ? 'policy' : f(a)} → ${b === undefined ? 'policy' : f(b)}`);
  }
  return out.length ? out : ['No change'];
}

const blank = (): Form => ({ code: '', name: '', effectiveFrom: today(), startTime: '09:00', endTime: '18:00', breakMinutes: '30', locMode: 'all', locIds: [], rules: {} });
const formOf = (v: any): Form => {
  const c = v.current, s = v.shift;
  // The OT rule and "Actual hours without break" were removed (8 Nov 2026); an older version may still carry them.
  const { otAbove: _ot, withoutBreak: _wb, ...rules } = c.rules || {};
  return { code: s.code, name: s.name, effectiveFrom: today(), startTime: c.startTime, endTime: c.endTime, breakMinutes: String(c.breakMinutes ?? 0),
    locMode: s.locationIds ? 'some' : 'all', locIds: s.locationIds || [], rules };
};

/** What the field components need from the form. */
type Ctx = { r: Rules; pol: Record<string, string>; err: { msg: string; field?: string } | null; edit: boolean; setR: (k: keyof Rules, v: any) => void };

/** One field: label, control, help, and the policy value it falls back to with a way back to it. */
function Box({ c, k, label, req, help, children, wide, noPol }: { c: Ctx; k: string; label: string; req?: boolean; help?: ReactNode; children: ReactNode; wide?: boolean; noPol?: boolean }) {
  const isSet = (c.r as any)[k] !== undefined, e = c.err?.field === k ? c.err.msg : '';
  return <div className={`field${e ? ' err' : ''}`} style={wide ? { gridColumn: '1/-1' } : undefined}>
    <span>{label}{req ? ' *' : ''}</span>
    {children}
    {help && <small>{help}</small>}
    {!noPol && <small>{isSet
      ? <span className="row" style={{ gap: 6 }}>Work policy: {c.pol[k] ?? '—'}{c.edit && <button type="button" className="btn sm" style={{ padding: '1px 8px', fontSize: 11 }} onClick={() => c.setR(k as keyof Rules, undefined)}>Use work policy</button>}</span>
      : <>Policy: <b>{c.pol[k] ?? '—'}</b></>}</small>}
    {e && <small style={{ color: 'var(--bad)' }} role="alert">{e}</small>}
  </div>;
}

function Radios({ k, opts, value, onPick }: { k: string; opts: [string, string][]; value?: string; onPick: (v: string) => void }) {
  return <div className="row" style={{ gap: 14 }}>{opts.map(([v, l]) => <label key={v} className="chkl"><input type="radio" name={`sf-${k}`} checked={value === v} onChange={() => onPick(v)} />{l}</label>)}</div>;
}

/** A duration (hours and minutes) rule. */
function Dur({ c, k, three }: { c: Ctx; k: keyof Rules; three?: boolean }) {
  return <input type="text" inputMode="numeric" placeholder={three ? 'HHH:MM' : 'HH:MM'} value={(c.r as any)[k] ?? ''} onChange={(e) => c.setR(k, e.target.value.trim())} style={{ maxWidth: 160 }} />;
}

function AdjBox({ c, k, label, help }: { c: Ctx; k: 'weeklyAdj' | 'monthlyAdj'; label: string; help: string }) {
  const v = c.r[k];
  return <Box c={c} k={k} label={label} help={help}>
    <div className="row" style={{ gap: 8 }}>
      <label className="chkl"><input type="checkbox" aria-label={label} checked={!!v?.on} onChange={(e) => c.setR(k, e.target.checked ? { on: true, hours: v?.hours || '' } : undefined)} />On</label>
      <input type="text" placeholder="HHH:MM" aria-label={label + ' hours'} disabled={!v?.on} value={v?.hours ?? ''} onChange={(e) => c.setR(k, { on: true, hours: e.target.value.trim() })} style={{ maxWidth: 120 }} />
    </div>
  </Box>;
}

/** Active locations grouped by city, as checkboxes. */
function LocChecks({ locs, sel, set }: { locs: any[]; sel: string[]; set: (v: string[]) => void }) {
  const cities = [...new Set(locs.map((l) => l.city))];
  return <>{cities.map((c) => <div key={c}>
    <div className="mute" style={{ fontSize: 11, fontWeight: 700, textTransform: 'uppercase', letterSpacing: '.05em' }}>{c}</div>
    {locs.filter((l) => l.city === c).map((l) => <label key={l.id} className="chkl"><input type="checkbox" checked={sel.includes(l.id)} onChange={(e) => set(e.target.checked ? [...sel, l.id] : sel.filter((x) => x !== l.id))} />{l.name}</label>)}
  </div>)}</>;
}

export default function ShiftForm({ id, locs, edit, onBack }: { id: string | null; locs: any[]; edit: boolean; onBack: (changed: boolean) => void }) {
  const [sid, setSid] = useState(id);
  const [view, setView] = useState<any>(null);
  const [loadErr, setLoadErr] = useState('');
  const [f, setF] = useState<Form | null>(null);
  const [err, setErr] = useState<{ msg: string; field?: string } | null>(null);
  const [ask, setAsk] = useState<{ from: string; note: string } | null>(null);
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    let live = true;
    api(`/shifts/${sid ?? 'new'}`).then((v) => { if (!live) return; setView(v); setF(v.shift ? formOf(v) : blank()); }).catch((e) => live && setLoadErr(e.message));
    return () => { live = false; };
  }, [sid]);
  if (loadErr) return <div className="note bad">{loadErr}</div>;
  if (!view || !f) return <Loading />;

  const pd = view.policyDefaults || { text: {}, value: {} };
  const P: Record<string, string> = pd.text || {};
  const r = f.rules;
  const set = (patch: Partial<Form>) => { setF({ ...f, ...patch }); setErr(null); };
  const setR = (k: keyof Rules, v: any) => { const x = { ...r }; if (v === undefined || v === '') delete x[k]; else (x as any)[k] = v; set({ rules: x }); };
  const nextDay = r.nextDay ?? crosses(f.startTime, f.endTime);
  // Fallbacks that depend on this shift's own values.
  const pol: Record<string, string> = {
    ...P, forcedOut: `Shift end (${f.endTime || '—'})`, officeHours: `Full day rule (${r.fullAbove || P.fullAbove})`,
  };

  const c: Ctx = { r, pol, err, edit, setR };
  /** Checks on screen before the server's own (the server says the same and more). */
  const check = (): { msg: string; field?: string } | null => {
    if (!f.name.trim()) return { field: 'name', msg: 'Enter the shift name' };
    if (!sid && !/^[A-Za-z0-9][A-Za-z0-9-]{0,9}$/.test(f.code.trim())) return { field: 'code', msg: 'Enter a short code: letters and digits, up to 10' };
    if (!TIME.test(f.startTime)) return { field: 'startTime', msg: 'Start time must be HH:MM' };
    if (!TIME.test(f.endTime)) return { field: 'endTime', msg: 'End time must be HH:MM' };
    if (f.locMode === 'some' && !f.locIds.length) return { field: 'locationIds', msg: 'Pick at least one location' };
    for (const k of ['earlyInMax', 'lateOutMax', 'officeHours', 'maxHours', 'absentBelow', 'halfBelow', 'fullAbove'] as (keyof Rules)[]) {
      const v = r[k] as string | undefined;
      if (v && !DUR.test(v)) return { field: k, msg: 'Enter hours and minutes, HH:MM, for example 4:30' };
    }
    if (r.forcedOut && !TIME.test(r.forcedOut)) return { field: 'forcedOut', msg: 'Forced time out must be a time, HH:MM' };
    for (const k of ['weeklyAdj', 'monthlyAdj'] as const) if (r[k]?.on && !DUR3.test(r[k]!.hours || '')) return { field: k, msg: 'Enter the hours it can cover, HHH:MM, for example 4:00' };
    return null;
  };
  const save = async () => {
    const a = ask!;
    const body = { code: f.code.trim(), name: f.name.trim(), effectiveFrom: a.from, startTime: f.startTime, endTime: f.endTime, breakMinutes: +f.breakMinutes || 0,
      locationIds: f.locMode === 'all' ? null : f.locIds, rules: { ...r, nextDay }, note: a.note.trim() };
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

  const vers: any[] = view.versions || [];
  const t = today();
  return <>
    <div className="row sp" style={{ marginBottom: 14 }}>
      <div><h2 style={{ margin: 0 }}>{sid ? `Set shift: ${view.shift?.name ?? f.name}` : 'Add shift'}{sid && <span className="mono mute" style={{ fontSize: 14, marginLeft: 8 }}>{view.shift?.code}</span>}</h2>
        <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>
          {sid ? <>In effect today: {view.current?.startTime}–{view.current?.endTime}, from {view.current?.effectiveFrom === '2000-01-01' ? 'the start' : fmtD(view.current?.effectiveFrom)}. </> : null}
          Empty rules follow the work policy{pd.policy ? <> (<b>{pd.policy}</b>{pd.employees ? `, ${pd.employees} on this shift` : ''})</> : null}.</div></div>
      <div className="row"><button className="btn" onClick={() => onBack(saved)}>Back to shifts</button>{edit && <button className="btn primary" onClick={askSave}>Save</button>}</div>
    </div>
    {!edit && <div className="note" style={{ marginBottom: 14 }}>You can view this shift. Changing it needs the Masters: job right.</div>}
    {err && <div className="note bad" role="alert" style={{ marginBottom: 14 }}>{err.msg}</div>}
    <fieldset disabled={!edit} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>
      <div className="card" style={{ marginBottom: 14 }}><h3>Shift</h3>
        <div className="form">
          <Box c={c} k="name" label="Shift name" req noPol><input type="text" value={f.name} placeholder="Shift name" onChange={(e) => set({ name: e.target.value })} /></Box>
          <Box c={c} k="code" label="Code" req noPol help={sid ? 'The code cannot change.' : 'Used on the roster, for example N2.'}><input type="text" value={f.code} disabled={!!sid} onChange={(e) => set({ code: e.target.value.toUpperCase() })} /></Box>
          <Box c={c} k="effectiveFrom" label="Shift effective date" req noPol help={view.lockedUntil ? `Attendance is locked by payroll up to ${fmtD(view.lockedUntil)}.` : 'Days before this date keep the earlier version.'}>
            <input type="date" value={f.effectiveFrom} onChange={(e) => set({ effectiveFrom: e.target.value })} /></Box>
          <Box c={c} k="startTime" label="Start time" req noPol><input type="time" value={f.startTime} onChange={(e) => set({ startTime: e.target.value })} /></Box>
          <Box c={c} k="endTime" label="End time" req noPol><input type="time" value={f.endTime} onChange={(e) => set({ endTime: e.target.value })} /></Box>
          <Box c={c} k="nextDay" label="Shift ending next day" noPol help={nextDay ? 'Yes: the day is built from the punches between the start and the next morning\'s end (4:00 either side unless the punch limits below are set).' : 'Set from the times. Yes needs an end at or before the start.'}>
            <Radios k="nextDay" opts={[['yes', 'Yes'], ['no', 'No']]} value={nextDay ? 'yes' : 'no'} onPick={(v) => setR('nextDay', v === 'yes')} /></Box>
          <Box c={c} k="breakMinutes" label="Break minutes" noPol help="Shown on the shift for information. No break is taken off the hours worked: they run from the in punch to the out punch."><input type="number" min={0} value={f.breakMinutes} onChange={(e) => set({ breakMinutes: e.target.value })} /></Box>
          <Box c={c} k="locationIds" label="Locations" noPol wide help="Forms and rosters show only shifts valid for the location.">
            <select value={f.locMode} onChange={(e) => set({ locMode: e.target.value as 'all' | 'some' })} style={{ maxWidth: 260 }}><option value="all">All locations</option><option value="some">Selected locations only</option></select>
            {f.locMode === 'some' && <div className="chkgrp"><LocChecks locs={locs} sel={f.locIds} set={(ids) => set({ locIds: ids })} /></div>}
          </Box>
        </div>
      </div>

      <div className="card" style={{ marginBottom: 14 }}><h3>Punch rules</h3>
        <div className="form">
          <Box c={c} k="startFrom" label="Consider shift start from" help="Shift start time: hours count from the shift start, so arriving early adds nothing. In punch: from the in punch.">
            <Radios k="startFrom" opts={CHOICES.startFrom} value={r.startFrom} onPick={(v) => setR('startFrom', v)} /></Box>
          <Box c={c} k="endFrom" label="Consider shift end from" help="Shift end time: hours stop at the shift end, so staying late adds nothing. Out punch: to the out punch.">
            <Radios k="endFrom" opts={CHOICES.endFrom} value={r.endFrom} onPick={(v) => setR('endFrom', v)} /></Box>
          <Box c={c} k="addMissingOut" label="Add missing last out entry" help="When the out punch is missing, the day gets an out time (the forced time out, else the shift end) instead of Missing out punch.">
            <Radios k="addMissingOut" opts={[['yes', 'Yes'], ['no', 'No']]} value={r.addMissingOut === undefined ? undefined : r.addMissingOut ? 'yes' : 'no'} onPick={(v) => setR('addMissingOut', v === 'yes')} /></Box>
          <Box c={c} k="forcedOut" label="Forced time out" help="The out time used by Add missing last out entry.">
            <input type="time" value={r.forcedOut ?? ''} onChange={(e) => setR('forcedOut', e.target.value)} style={{ maxWidth: 160 }} /></Box>
          <Box c={c} k="earlyInMax" label="Maximum early in punch allowed" help="A punch earlier than the shift start minus this is not taken as this shift's in punch. Setting it builds the day from the shift's punch window (4:00 when not set).">
            <Dur c={c} k="earlyInMax" /></Box>
          <Box c={c} k="lateOutMax" label="Maximum late out punch allowed" help="A punch later than the shift end plus this is not taken as this shift's out punch (4:00 when not set and the day uses a window).">
            <Dur c={c} k="lateOutMax" /></Box>
          <Box c={c} k="officeHours" label="Office hour time" help="The hours a day of this shift should have. Short and extra hours are measured against it."><Dur c={c} k="officeHours" /></Box>
          <Box c={c} k="shiftIn" label="Consider shift in" help={<>Select if attendance needs to be considered in current day. Previous day: counted on the day before it starts (a 00:30–08:30 duty on the evening's date). Next day: on the day after it starts.</>}>
            <Radios k="shiftIn" opts={CHOICES.shiftIn} value={r.shiftIn} onPick={(v) => setR('shiftIn', v)} /></Box>
        </div>
      </div>

      <div className="card" style={{ marginBottom: 14 }}><h3>Single punch rules</h3>
        <div className="form">
          <Box c={c} k="singlePunch" label="Mark 'Single punch' as" wide help="A day with only one punch (and no forced out) counts as this. The day still shows the missing punch, so it can be regularized.">
            <Radios k="singlePunch" opts={CHOICES.singlePunch} value={r.singlePunch} onPick={(v) => setR('singlePunch', v)} /></Box>
        </div>
      </div>

      <div className="card" style={{ marginBottom: 14 }}><h3>Attendance rules</h3>
        <div className="form">
          <Box c={c} k="maxHours" label="Max Hrs:mins" help="Hours counted for a day are capped at this."><Dur c={c} k="maxHours" /></Box>
          <Box c={c} k="basedOn" label="Attendance based on" help="Hours: worked hours against the rules below. Shift: only hours inside the shift count. Punch: any in punch on a working day makes it a full day; the hour rules are not used.">
            <select value={r.basedOn ?? ''} onChange={(e) => setR('basedOn', e.target.value)}><option value="">Select attendance based on</option>{CHOICES.basedOn.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></Box>
          <Box c={c} k="absentBelow" label="Absent rule <" help="Worked less than this: Absent."><Dur c={c} k="absentBelow" /></Box>
          <Box c={c} k="halfBelow" label="Half day rule <" help="Worked less than this (and not absent): Half day."><Dur c={c} k="halfBelow" /></Box>
          <Box c={c} k="fullAbove" label="Full day rule >" help="Worked this or more: a full day. Between the half day and full day rules the day is Short hours: present, with the missing hours covered by the adjustments below, else a half day.">
            <Dur c={c} k="fullAbove" /></Box>
          <AdjBox c={c} k="weeklyAdj" label="Weekly adjustment" help="Extra hours in the same Monday–Sunday week cover Short hours days, oldest first, up to this many hours a week. A day not covered becomes a half day." />
          <AdjBox c={c} k="monthlyAdj" label="Monthly adjustment" help="The same over the month, after the weekly adjustment, up to this many hours a month." />
          <Box c={c} k="shortfall" label="Inclusion in shortfall hours" help="Hours worked on these days also count as extra hours for the adjustments.">
            {([['wo', 'Weekly-off working hours'], ['hol', 'Holiday working hours'], ['optHol', 'Optional holiday working hours']] as [keyof Incl, string][]).map(([k, l]) =>
              <label key={k} className="chkl"><input type="checkbox" checked={!!r.shortfall?.[k]} onChange={(e) => { const s = { ...(r.shortfall || {}), [k]: e.target.checked || undefined }; setR('shortfall', Object.values(s).some(Boolean) ? s : undefined); }} />{l}</label>)}</Box>
        </div>
        <p className="mute" style={{ fontSize: 12, margin: '12px 0 0' }}>Not here yet: Consider NA, Configurable attendance calculation and IS consider first entry in last entry out. The app always uses the first in and the last out.</p>
      </div>
    </fieldset>

    {sid && <div className="card"><h3>Version history</h3>
      <div className="tw"><table>
        <thead><tr><th>Effective from</th><th>Timings</th><th>What changed</th><th>By</th><th>Saved</th><th>Note</th></tr></thead>
        <tbody>{vers.map((v, i) => <tr key={v.id ?? i}>
          <td className="mono">{v.effectiveFrom === '2000-01-01' ? 'The start' : fmtD(v.effectiveFrom)}{' '}{v.current ? <Pill k="ok">In effect</Pill> : v.effectiveFrom > t ? <Pill k="warn">Upcoming</Pill> : null}</td>
          <td className="mono">{v.startTime}–{v.endTime}{v.breakMinutes ? ` · ${v.breakMinutes} min break` : ''}</td>
          <td className="ink2" style={{ fontSize: 12.5 }}>{changes(v, vers[i + 1]).map((c, j) => <div key={j}>{c}</div>)}</td>
          <td>{v.createdBy || '—'}</td><td className="mute">{v.createdAt ? fmtD(v.createdAt) : '—'}</td><td className="ink2">{v.note || '—'}</td>
        </tr>)}</tbody>
      </table></div>
    </div>}

    {ask && <Modal onClose={() => setAsk(null)}>
      <h2>Save {f.name || 'the shift'}</h2>
      <p className="ink2">The timings and rules apply from this date. Days before it keep the earlier version. A date in a month whose attendance is locked by payroll is refused.</p>
      <div className="grid g2" style={{ margin: '10px 0' }}>
        <label className="field"><span>Effective from</span><input type="date" value={ask.from} onChange={(e) => setAsk({ ...ask, from: e.target.value })} /></label>
        <label className="field"><span>Note for the history (optional)</span><input type="text" value={ask.note} onChange={(e) => setAsk({ ...ask, note: e.target.value })} /></label>
      </div>
      <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={save} disabled={!ask.from}>Save from {ask.from ? fmtD(ask.from) : '…'}</button><button className="btn" onClick={() => setAsk(null)}>Cancel</button></div>
    </Modal>}
  </>;
}
