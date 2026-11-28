import { useState } from 'react';
import { api, fmtD, recentYm, today } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import PersonPicker, { type Person } from '../components/PersonPicker';

// Attendance › Called to work (owner, 24 Nov 2026): HR calls employees to work on a holiday or a day of their approved
// leave. If they come (any punch) it is Present; a leave day's leave comes back. If not, nothing changes.

const RES: Record<string, 'ok' | 'warn' | 'n' | 'bad'> = { Worked: 'ok', 'Not worked': 'warn', Upcoming: 'n', 'No longer a holiday or leave day': 'bad' };

export default function CalledToWork({ list }: { list: Person[] }) {
  const { can } = useAuth();
  const edit = can('att.records', 2);
  const [ym, setYm] = useState(recentYm());
  const [add, setAdd] = useState(false);
  const { data, error, reload } = useLoad(() => api(`/attendance/called?ym=${ym}`), [ym]);
  const remove = async (r: any) => {
    if (!confirm(`Take back the call to work of ${r.name} on ${fmtD(r.date)}?`)) return;
    try { await api(`/attendance/called/${r.id}`, { method: 'DELETE' }); toast('Call to work removed'); reload(); } catch (x: any) { toast(x.message); }
  };
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
      <Field label="Month"><input type="month" value={ym} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field>
      {edit && <button className="btn primary" onClick={() => setAdd(true)}>Call to work</button>}</div>
      <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>For a holiday on the employee's calendar or a day of their approved leave. If they come that day (any punch, whatever the hours), it is Present (Called to work): a leave day's leave is returned to their balance and an unpaid leave loses its loss of pay. If they do not, the holiday or leave stays as it is, with no penalty. A call can be removed while the month is not paid.</p>
      {data?.paid && <div className="note warn" style={{ marginTop: 10 }}>{data.paid}</div>}</div>
    {!data ? <Loading error={error} /> : <div className="card"><div className="tw"><table><thead><tr><th>Date</th><th>Employee</th><th>The day</th><th>Reason</th><th>Result</th><th>In</th><th>Out</th><th>Note</th><th>Called by</th><th></th></tr></thead><tbody>
      {data.rows.map((r: any) => <tr key={r.id}>
        <td style={{ whiteSpace: 'nowrap' }}>{fmtD(r.date)}</td><td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.employeeId} · {r.location}</div></td>
        <td>{r.kind === 'holiday' ? 'Holiday' : `${r.leaveType}${r.days === 0.5 ? ' (half day)' : ''}`}{r.shift && <div className="mute" style={{ fontSize: 12 }}>Shift {r.shift} (from the roster)</div>}</td><td>{r.reason}</td>
        <td><Pill k={RES[r.result] ?? 'n'}>{r.result}</Pill></td><td className="mono">{r.in || '—'}</td><td className="mono">{r.out || '—'}</td>
        <td className="mute" style={{ fontSize: 12 }}>{r.note || ''}</td><td className="mute" style={{ fontSize: 12 }}>{r.by}<div>{r.at}</div></td>
        <td>{edit && r.removable && <button className="btn sm" onClick={() => remove(r)}>Remove</button>}</td></tr>)}
      {!data.rows.length && <tr><td colSpan={10} className="mute">Nobody is called to work this month.</td></tr>}
    </tbody></table></div></div>}
    {add && <AddCall list={list} onClose={(done) => { setAdd(false); if (done) reload(); }} />}
  </>;
}

function AddCall({ list, onClose }: { list: Person[]; onClose: (done: boolean) => void }) {
  const [who, setWho] = useState<Person[]>([]);
  const [date, setDate] = useState(today());
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const save = async () => {
    if (!who.length) { toast('Choose the employees'); return; }
    if (!date) { toast('Choose the date'); return; }
    if (!reason.trim()) { toast('Give the reason'); return; }
    setBusy(true);
    try {
      const r = await api<any>('/attendance/called', { method: 'POST', body: { employeeIds: who.map((p) => p.id), date, reason } });
      toast(`${r.added} called to work on ${fmtD(date)}`); onClose(true);
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  return <Modal onClose={() => onClose(false)}>
    <h3 style={{ marginTop: 0 }}>Call to work on a holiday or leave day</h3>
    <div className="form">
      <Field label="Add an employee"><PersonPicker label="Employee" people={list} value="" exclude={who.map((p) => p.id)} onChange={(_, p) => p && setWho([...who, p])} /></Field>
      <Field label="Date"><input type="date" value={date} onChange={(e) => setDate(e.target.value)} /></Field>
      <label className="field" style={{ gridColumn: 'span 2' }}><span>Reason</span><input type="text" value={reason} maxLength={500} onChange={(e) => setReason(e.target.value)} /></label>
    </div>
    <div className="row" style={{ flexWrap: 'wrap', gap: 6, marginTop: 10 }}>{who.map((p) => <span key={p.id} className="pill info">{p.name} ({p.id}) <button className="btn sm" aria-label={`Remove ${p.name}`} onClick={() => setWho(who.filter((x) => x.id !== p.id))}>×</button></span>)}
      {!who.length && <span className="mute" style={{ fontSize: 12.5 }}>No one chosen yet.</span>}</div>
    <p className="mute" style={{ fontSize: 12.5 }}>The date must be a holiday on each person's calendar or a day of their approved leave. Week offs are not covered.</p>
    <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={busy} onClick={save}>Call to work</button><button className="btn" onClick={() => onClose(false)}>Cancel</button></div>
  </Modal>;
}
