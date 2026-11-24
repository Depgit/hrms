import { useState } from 'react';
import { api, fmtD } from '../api';
import { Field, Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import { DevActSelect, devActText } from './DevAllow';

// Individual roster changes (owner, 24 Nov 2026: "individual roster change allow this as well"). A click on a roster
// cell opens this picker instead of cycling the codes: each shift with its timing, WO, PH, Other time for this day,
// and on a holiday or leave cell Called to work with a shift. The employee's name opens their own deviation
// allowances. The server checks every rule (backend roster_day.go, dev_own.go).

type Time = { start: string; end: string; start2?: string; end2?: string };

const dlab = (d: string) => new Date(d + 'T00:00:00').toLocaleDateString('en-IN', { weekday: 'short', day: 'numeric', month: 'short' });

/** The roster cell picker. */
export function CellPicker({ row, cell, date, timings, why, canRoster, canCall, onClose }: {
  row: any; cell: any; date: string; timings: Record<string, string>; why: string; canRoster: boolean; canCall: boolean; onClose: (changed: boolean) => void;
}) {
  const shifts = (row.codes as string[]).filter((c) => c !== 'WO' && c !== 'PH');
  const call = cell.call?.active ? cell.call : null;
  const paidLeave = cell.code === 'L';
  const callable = !!call || cell.holiday || !!cell.leave;
  const [busy, setBusy] = useState(false);
  const [two, setTwo] = useState(!!cell.other?.start2);
  const [tm, setTm] = useState<Time>({ start: cell.other?.start ?? '', end: cell.other?.end ?? '', start2: cell.other?.start2 ?? '', end2: cell.other?.end2 ?? '' });
  const [shift, setShift] = useState<string>(call?.shift ?? '');
  const [reason, setReason] = useState<string>(call?.reason ?? '');
  const run = async (body: any, msg: string) => {
    setBusy(true);
    try {
      const r = await api('/roster/cell', { body: { employeeId: row.id, date, ...body } });
      toast(r.warning ? 'Warning: ' + r.warning : r.call === 'unchanged' ? 'Nothing changed' : msg); onClose(true);
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const setCode = (code: string) => run({ code, why }, `${row.name}: ${code} on ${fmtD(date)}`);
  const setOther = () => {
    if (!tm.start || !tm.end) { toast('Give the start and the end'); return; }
    if (two && (!tm.start2 || !tm.end2)) { toast('Give the second part, or untick Two parts'); return; }
    run({ code: 'OT', why, time: { start: tm.start, end: tm.end, ...(two ? { start2: tm.start2, end2: tm.end2 } : {}) } }, `${row.name}: other time on ${fmtD(date)}`);
  };
  const callToWork = () => {
    if (!shift) { toast('Choose the shift'); return; }
    if (!call && !reason.trim()) { toast('Give the reason they are called to work'); return; }
    run({ code: shift, call: true, reason }, call ? 'Call to work changed' : `${row.name} called to work on ${fmtD(date)}`);
  };
  const removeCall = async () => {
    if (!confirm(`Take back the call to work of ${row.name} on ${fmtD(date)}?`)) return;
    setBusy(true);
    try { await api(`/attendance/called/${cell.call.id}`, { method: 'DELETE' }); toast('Call to work removed'); onClose(true); }
    catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const now = call ? `Called to work, ${call.shift}${call.time ? ' ' + call.time : ''}` : `${cell.code}${cell.time ? ' ' + cell.time : ''}${cell.leave ? ` (${cell.leave})` : ''}`;
  const T = (k: keyof Time, label: string) => <Field label={label}><input type="time" value={tm[k] ?? ''} onChange={(e) => setTm({ ...tm, [k]: e.target.value })} /></Field>;
  return <Modal onClose={() => onClose(false)}>
    <div className="row sp" style={{ marginBottom: 6 }}><h3 style={{ margin: 0 }}>{row.name} · {dlab(date)}</h3><button className="btn" onClick={() => onClose(false)}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Now: <b>{now}</b>{cell.holiday && !call ? ' · a holiday on their calendar' : ''}</p>

    {call && <div className="card" style={{ marginBottom: 12 }}><b>Called to work</b>
      <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 8px' }}>{call.kind === 'holiday' ? 'Holiday' : call.leave}. {call.reason}. By {call.by}, {call.at}. If they come (any punch) the day is Present{call.kind === 'leave' ? ' and the leave comes back' : ''}; if not, the {call.kind === 'holiday' ? 'holiday' : 'leave'} stays with no penalty. Change the roster after the call is removed.</p>
      {canCall && <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>{cell.call.removable && <button className="btn bad" disabled={busy} onClick={removeCall}>Remove the call</button>}</div>}</div>}

    {!call && cell.call && !cell.call.active && <div className="note warn" style={{ marginBottom: 12 }}>A call to work on this date does nothing now: the day is no longer a holiday or leave day. {canCall && cell.call.removable && <button className="btn sm" disabled={busy} onClick={removeCall}>Remove the call</button>}</div>}

    {!call && canRoster && (paidLeave
      ? <div className="note" style={{ marginBottom: 12 }}>Approved leave{cell.leave ? ` (${cell.leave})` : ''}. Change it by cancelling the leave{canCall ? ', or call them to work below' : ''}.</div>
      : <div style={{ marginBottom: 12 }}><b>Roster</b>
        {cell.holiday && <p className="mute" style={{ fontSize: 12.5, margin: '4px 0' }}>A holiday on their calendar: a shift here calls them to work (below).</p>}
        <div className="tw"><table><tbody>
          {!cell.holiday && shifts.map((code) => <tr key={code}><td style={{ width: 70 }}><button className={`cell ${code}`} disabled={busy || cell.code === code} onClick={() => setCode(code)}>{code}</button></td><td className="mono mute">{timings[code] ?? ''}</td></tr>)}
          <tr><td><button className="cell WO" disabled={busy || cell.code === 'WO'} onClick={() => setCode('WO')}>WO</button></td><td className="mute">Weekly off</td></tr>
          <tr><td><button className="cell PH" disabled={busy || cell.code === 'PH'} onClick={() => setCode('PH')}>PH</button></td><td className="mute">Holiday</td></tr>
        </tbody></table></div>
        {!cell.holiday && <div style={{ marginTop: 10 }}><b>Other time for this day</b>
          <p className="mute" style={{ fontSize: 12.5, margin: '4px 0' }}>Timings for this day only, without a shift. Attendance treats the day like a shift with these timings. A day in two parts runs on the same day; the gap between the parts is not counted as hours worked.</p>
          <div className="form" style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(120px,1fr))' }}>{T('start', 'Start')}{T('end', 'End')}{two && T('start2', 'Second part start')}{two && T('end2', 'Second part end')}</div>
          <div className="row" style={{ gap: 10, marginTop: 8 }}><label className="row" style={{ gap: 6 }}><input type="checkbox" checked={two} onChange={(e) => setTwo(e.target.checked)} />Two parts</label>
            <button className="btn primary" disabled={busy} onClick={setOther}>Set other time</button></div></div>}
      </div>)}

    {callable && canCall && <div className="card"><b>{call ? 'Change the shift of the call' : 'Called to work'}</b>
      {!call && <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 8px' }}>{cell.holiday ? 'A holiday' : `Approved leave (${cell.leave})`}: choose the shift they are called to work. If they come (any punch) the day is Present{cell.leave ? ' and the leave comes back to the balance (an unpaid leave loses its loss of pay)' : ''}; if not, the {cell.holiday ? 'holiday' : 'leave'} stays with no penalty.</p>}
      <div className="form" style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(180px,1fr))', marginTop: 6 }}>
        <Field label="Shift"><select value={shift} onChange={(e) => setShift(e.target.value)}><option value="">Choose</option>{shifts.map((c) => <option key={c} value={c}>{c}{timings[c] ? ` · ${timings[c]}` : ''}</option>)}</select></Field>
        <Field label="Reason"><input type="text" value={reason} maxLength={500} onChange={(e) => setReason(e.target.value)} /></Field></div>
      <div className="row" style={{ marginTop: 8 }}><button className="btn primary" disabled={busy} onClick={callToWork}>{call ? 'Save' : 'Call to work'}</button></div></div>}
    {!canRoster && !(callable && canCall) && !call && <p className="mute">You can see this day but not change it.</p>}
  </Modal>;
}

const RULES: [string, string][] = [['earlyIn', 'Early check-in'], ['lateIn', 'Late check-in'], ['earlyOut', 'Early check-out']];
// A rule On before the Action existed has none: it works as No action (the server reads it so).
const beyond = (a?: string) => `beyond: ${devActText(a || 'none')}`;
const ruleText = (r?: any) => (r?.on && r.mins && r.n ? `on, ${r.mins} min, ${r.n} times a month, ${beyond(r.act)}` : 'off');
const ownText = (o?: any, p?: any) => (o?.use === 'on' ? `on, ${o.mins} min, ${o.n} times a month, ${beyond(o.act)}` : o?.use === 'off' ? 'off for this employee' : `the policy's (${ruleText(p)})`);

/** One employee's own deviation allowances: each rule uses the policy's, or is On with its own numbers, or Off. */
export function DevOwnModal({ id, onClose }: { id: string; onClose: () => void }) {
  const { data, error, reload } = useLoad(() => api(`/attendance/dev-allow/${encodeURIComponent(id)}`), [id]);
  const [f, setF] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const form = f ?? (data ? { from: '', note: '', ...Object.fromEntries(RULES.map(([k]) => [k, { use: data.current?.[k]?.use || 'policy', mins: data.current?.[k]?.mins || '', n: data.current?.[k]?.n || '', act: data.current?.[k]?.act || '' }])) } : null);
  const put = (k: string, patch: any) => setF({ ...form, [k]: { ...form[k], ...patch } });
  const save = async () => {
    if (!form.from) { toast('Give the date the rules apply from'); return; }
    setBusy(true);
    try {
      const body: any = { effectiveFrom: form.from, note: form.note };
      RULES.forEach(([k]) => { const x = form[k]; body[k] = { use: x.use, ...(x.use === 'on' ? { mins: +x.mins || 0, n: +x.n || 0, act: x.act || '' } : {}) }; });
      const r = await api(`/attendance/dev-allow/${encodeURIComponent(id)}`, { body });
      toast(r.saved ? 'Deviation allowances saved' : 'Nothing changed: these are already the rules from that date'); setF(null); reload();
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  return <Modal wide onClose={onClose}>
    <div className="row sp" style={{ marginBottom: 8 }}><h3 style={{ margin: 0 }}>Deviation allowances{data ? ` · ${data.employee.name}` : ''}</h3><button className="btn" onClick={onClose}>Close</button></div>
    {!data ? <Loading error={error} /> : <>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Work policy {data.employee.policy}. Give this employee their own Early check-in, Late check-in or Early check-out allowance without a separate policy. Each rule uses the policy's rule, its action too, unless you choose On (with the minutes, times and action beyond them you give) or Off. The action decides a day beyond the allowance: No action, Absent, Half day until regularized or Present until regularized; the harsher wins when two rules are beyond on one day. The rules apply from the date you give; a date in a paid payroll month is refused.</p>
      <div className="tw"><table><thead><tr><th>Rule</th><th>The policy's</th><th>In effect today</th></tr></thead><tbody>
        {RULES.map(([k, label]) => <tr key={k}><td><b>{label}</b></td><td>{ruleText(data.policyRules?.[k])}</td><td>{ruleText(data.effective?.[k])}</td></tr>)}</tbody></table></div>
      {data.canEdit && form && <div className="card" style={{ marginTop: 12 }}><b>Change from a date</b>
        <div className="tw" style={{ marginTop: 8 }}><table><thead><tr><th>Rule</th><th>Choice</th><th>Minutes allowed</th><th>Times allowed in a month</th><th>Action</th></tr></thead><tbody>
          {RULES.map(([k, label]) => { const x = form[k]; return <tr key={k}><td><b>{label}</b></td>
            <td><select value={x.use} onChange={(e) => put(k, { use: e.target.value })}><option value="policy">Use the policy's rule</option><option value="on">On</option><option value="off">Off for this employee</option></select></td>
            <td><input type="number" min={1} max={720} style={{ width: 110 }} disabled={x.use !== 'on'} aria-label={label + ', minutes allowed'} placeholder="Minutes" value={x.use === 'on' ? x.mins : ''} onChange={(e) => put(k, { mins: e.target.value })} /></td>
            <td><input type="number" min={1} max={31} style={{ width: 110 }} disabled={x.use !== 'on'} aria-label={label + ', times allowed in a month'} placeholder="Times" value={x.use === 'on' ? x.n : ''} onChange={(e) => put(k, { n: e.target.value })} /></td>
            <td><DevActSelect value={x.use === 'on' ? x.act : ''} label={label} disabled={x.use !== 'on'} onChange={(v) => put(k, { act: v })} /></td></tr>; })}
        </tbody></table></div>
        <div className="form" style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(180px,1fr))', marginTop: 8 }}>
          <Field label="Applies from"><input type="date" value={form.from} onChange={(e) => setF({ ...form, from: e.target.value })} /></Field>
          <Field label="Note, optional"><input type="text" maxLength={500} value={form.note} onChange={(e) => setF({ ...form, note: e.target.value })} /></Field></div>
        <div className="row" style={{ marginTop: 10 }}><button className="btn primary" disabled={busy} onClick={save}>Save</button></div></div>}
      <h3 style={{ marginTop: 16 }}>History</h3>
      {data.versions.length ? <div className="tw"><table><thead><tr><th>From</th><th>Rules</th><th>Note</th><th>Saved by</th><th /></tr></thead><tbody>
        {data.versions.map((v: any) => <tr key={v.id}><td style={{ whiteSpace: 'nowrap' }}>{fmtD(v.from)}</td>
          <td style={{ fontSize: 12.5 }}>{RULES.map(([k, label]) => <div key={k}>{label}: {ownText(v.rules?.[k], data.policyRules?.[k])}</div>)}</td>
          <td className="mute" style={{ fontSize: 12.5 }}>{v.note || ''}</td><td className="mute" style={{ fontSize: 12 }}>{v.by}<div>{v.at}</div></td>
          <td>{v.current ? <Pill k="ok">In effect</Pill> : v.future ? <Pill k="info">From {fmtD(v.from)}</Pill> : null}</td></tr>)}</tbody></table></div>
        : <p className="mute">No rules of their own: the policy's rules apply.</p>}
    </>}
  </Modal>;
}
