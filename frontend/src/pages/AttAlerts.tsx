import { useEffect, useState } from 'react';
import { api } from '../api';
import { Field, Loading, Pill, toast, useLoad } from '../components/ui';

/** Attendance alerts (Attendance › Punch machine setup): which messages the HRMS sends from the punches it
 *  receives, their times, and what went out today. GET/PUT /api/attendance/alerts, POST /api/attendance/alerts/test. */

type Set = {
  missingOn: boolean; missingAt: string; lateOn: boolean; notInOn: boolean; notInAfter: number; notInManager: boolean;
  digestOn: boolean; digestAt: string; cutoffOn: boolean; cutoffDays: number; cutoffAt: string;
};

const KINDS: [string, string][] = [['missing', 'Missing punch'], ['late', 'Late arrival'], ['notin', 'Not punched in'], ['notin-mgr', 'Not punched in (to managers)'],
  ['digest', 'Manager digest'], ['cutoff', 'Payroll cut-off']];

const whenT = (at: string) => new Date(at).toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' });

export default function AttAlerts({ ed }: { ed: boolean }) {
  const d = useLoad(() => api('/attendance/alerts'), []);
  const [f, setF] = useState<Set | null>(null);
  const [busy, setBusy] = useState('');
  useEffect(() => { if (d.data && !f) setF({ ...d.data.settings }); }, [d.data, f]);
  const save = async () => {
    setBusy('save');
    try { await api('/attendance/alerts', { method: 'PUT', body: { ...f, notInAfter: +f!.notInAfter || 0, cutoffDays: +f!.cutoffDays || 0 } }); toast('Settings saved'); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const test = async () => {
    setBusy('test');
    try { const r = await api('/attendance/alerts/test', { body: {} }); toast(`Test sent to ${r.to}`); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const anyOn = f && (f.missingOn || f.lateOn || f.notInOn || f.digestOn || f.cutoffOn);
  const sw = (k: keyof Set, label: string) => <Field label={label}><select value={f![k] ? 'on' : 'off'} disabled={!ed} onChange={(e) => setF({ ...f!, [k]: e.target.value === 'on' })}>
    <option value="off">Off</option><option value="on">On</option></select></Field>;
  const tm = (k: keyof Set, label: string) => <Field label={label}><input type="time" value={String(f![k])} disabled={!ed} onChange={(e) => setF({ ...f!, [k]: e.target.value })} /></Field>;
  const nm = (k: keyof Set, label: string, min: number, max: number) => <Field label={label}><input type="number" min={min} max={max} value={String(f![k])} disabled={!ed}
    onChange={(e) => setF({ ...f!, [k]: e.target.value as any })} /></Field>;
  const last = d.data?.last;
  const sent: Record<string, number> = last?.sent ?? {};
  const skipped: Record<string, number> = last?.skipped ?? {};
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Attendance alerts</h3>{f && <Pill k={anyOn ? 'ok' : 'n'}>{anyOn ? 'On' : 'Off'}</Pill>}</div>
    {!d.data || !f ? <Loading error={d.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5 }}>Messages to employees and their reporting managers from the punches the HRMS receives. They go to the bell{d.data.email ? ', and by email to people with an email address' : ' (email is not set up on this server)'}{d.data.channel ? ', and by SMS or WhatsApp' : ''}. Each goes out once per person and day. People whose attendance mode is “Manager marked” get none.</p>
      {d.data.feed && <div className="note warn" style={{ marginBottom: 12 }}>Right now the {d.data.feed.replace('punch feed stale: ', '')}, so no one is told they are missing a punch or not punched in until punches arrive again.</div>}
      <h3 style={{ margin: '12px 0 6px', fontSize: 13 }}>To the employee</h3>
      <div className="form">
        {sw('missingOn', 'Missing punch (yesterday)')}{tm('missingAt', 'Send each morning at')}
        {sw('lateOn', 'Late arrival (today, once)')}
        {sw('notInOn', 'Not punched in')}{nm('notInAfter', 'Minutes after shift start and grace', 0, 240)}
        {sw('cutoffOn', 'Payroll cut-off reminder')}{nm('cutoffDays', 'Days before the cut-off', 1, 15)}{tm('cutoffAt', 'Reminder time')}
      </div>
      <h3 style={{ margin: '16px 0 6px', fontSize: 13 }}>To the reporting manager (L1)</h3>
      <div className="form">
        {sw('notInManager', 'Not punched in: tell the manager too')}
        {sw('digestOn', 'Daily team digest')}{tm('digestAt', 'Digest time')}
      </div>
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>An absence is only reported when punches are arriving: if the Realtime portal pull has failed or is late, or nobody at a location has punched yet today (a machine may be offline), those people are not called absent. Leave (approved or waiting), week offs, holidays and regularized days are left out.</p>
      {ed && <div className="row" style={{ marginTop: 12, gap: 8 }}>
        <button className="btn primary" disabled={!!busy} onClick={save}>Save settings</button>
        <button className="btn" disabled={!!busy} onClick={test}>{busy === 'test' ? 'Sending…' : 'Send a test to me'}</button>
      </div>}
      {last?.at && <div className={`note ${String(last.note ?? '').startsWith('Failed') ? 'bad' : ''}`} style={{ marginTop: 12 }}>
        Last check {whenT(last.at)}{last.note ? `: ${last.note}` : ''}.
        <div className="tw" style={{ marginTop: 8 }}><table><thead><tr><th>Sent today</th><th className="num">Messages</th></tr></thead><tbody>
          {KINDS.map(([k, l]) => <tr key={k}><td>{l}</td><td className="num">{sent[k] ?? 0}</td></tr>)}
        </tbody></table></div>
        {Object.keys(skipped).length > 0 && <><div style={{ margin: '8px 0 4px', fontWeight: 600 }}>Not sent today</div>
          <ul style={{ margin: 0, paddingLeft: 18 }}>{Object.entries(skipped).map(([why, n]) => <li key={why}>{why}: {n}</li>)}</ul></>}
      </div>}
    </>}
  </div>;
}
