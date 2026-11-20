import { useState } from 'react';
import { api, fmtD } from '../api';
import { Field, Loading, Modal, Pill, toast, useLoad } from './ui';

/** The "Realtime portal" card of the employee file (backend portal_write.go): status on the Realtime India portal,
 *  and "Send to portal" / "Mark left on portal", each previewed (exactly what will be sent) and confirmed. Nothing is
 *  sent without HR's confirmation. */

type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'n';
const TONE: Record<string, Tone> = { 'Not on portal': 'warn', 'On portal': 'ok', 'Changed since last send': 'warn', Left: 'n', 'Check on the portal': 'bad' };
const ACT: Record<string, string> = { create: 'Send to portal', update: 'Send changes to portal', left: 'Mark left on portal' };

export function PortalCard({ id }: { id: string }) {
  const d = useLoad(() => api(`/employees/${id}/portal`), [id]);
  const [act, setAct] = useState('');
  const [busy, setBusy] = useState(false);
  const x = d.data;
  if (!x) return <div className="card" style={{ marginTop: 14 }}><h3>Realtime portal</h3><Loading error={d.error} /></div>;
  const s = x.state;
  const resolve = async (on: boolean) => {
    setBusy(true);
    try { await api(`/employees/${id}/portal/resolve`, { body: { on } }); toast('Saved'); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  return <div className="card" style={{ marginTop: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Realtime portal</h3><Pill k={TONE[s.status] ?? 'n'}>{s.status}</Pill></div>
    <p className="mute" style={{ fontSize: 12.5 }}>
      {s.code ? <>Portal code <b className="mono">{s.code}</b>. </> : 'No portal code yet. '}
      {s.unsent && 'On the portal, but never sent from the HRMS. '}
      {s.changed?.length > 0 && <>Changed since the last send: {s.changed.join(', ')}. </>}
      {s.exitDue && s.status !== 'Left' && <>Exited{x.lwd ? ` (last working day ${fmtD(x.lwd)})` : ''}: mark them left on the portal. </>}
      {s.last?.at && <>Last send {fmtD(String(s.last.at).slice(0, 10))} by {s.last.by}{s.last.message ? `: “${s.last.message}”` : ''}{s.last.refused ? ' (refused)' : ''}.</>}
    </p>
    {s.matches?.length > 0 && s.status === 'Not on portal' && <div className="note warn">The portal may already have this person (Match portal codes proposed {s.matches.join('; ')}). Apply that code under Attendance › Punch machine setup instead of creating a second record.</div>}
    {s.blocked?.length > 0 && s.actions.some((a: string) => a !== 'left') && <div className="note warn">{s.blocked.map((b: string) => <div key={b}>{b}</div>)}</div>}
    {x.notReady && <p className="mute" style={{ fontSize: 12 }}>{x.notReady}</p>}
    {x.canSend && <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
      {s.actions.map((a: string) => <button key={a} className={`btn ${a === 'left' ? 'bad' : 'primary'}`} disabled={!!x.notReady || (a !== 'left' && s.blocked.length > 0)} onClick={() => setAct(a)}>{ACT[a]}</button>)}
      {s.status === 'Check on the portal' && <>
        <button className="btn" disabled={busy} onClick={() => resolve(true)}>I checked: it is on the portal</button>
        <button className="btn" disabled={busy} onClick={() => resolve(false)}>I checked: it is not</button></>}
    </div>}
    {x.sends?.length > 0 && <details style={{ marginTop: 8 }}><summary className="mute" style={{ fontSize: 12 }}>Sends ({x.sends.length})</summary>
      <ul style={{ fontSize: 12.5 }}>{x.sends.map((r: any) => <li key={r.id}>{fmtD(String(r.at).slice(0, 10))} · {r.action} · code {r.code} · {r.ok ? 'saved' : r.unclear ? 'unclear' : 'refused'}{r.message ? ` · “${r.message}”` : ''} · {r.by}</li>)}</ul></details>}
    {act && <PortalSendDialog id={id} name={x.name} action={act} code={s.code || x.suggestCode || ''} onClose={() => setAct('')} onDone={() => { setAct(''); d.reload(); }} />}
  </div>;
}

/** Preview (read from the portal's own form) → confirm → one send. */
export function PortalSendDialog({ id, name, action, code: code0, onClose, onDone }: { id: string; name: string; action: string; code: string; onClose: () => void; onDone: () => void }) {
  const [code, setCode] = useState(code0);
  const [pv, setPv] = useState<any>(null);
  const [err, setErr] = useState('');
  const [notSame, setNotSame] = useState(false);
  const [busy, setBusy] = useState(false);
  const [res, setRes] = useState<any>(null);
  const preview = async () => {
    setBusy(true); setErr(''); setPv(null);
    try { setPv(await api(`/employees/${id}/portal/preview`, { body: { action, code, notSame } })); } catch (e: any) { setErr(e.message); } finally { setBusy(false); }
  };
  const send = async () => {
    setBusy(true); setErr('');
    try { const r = await api(`/employees/${id}/portal/send`, { body: { action, code, notSame, hash: pv.plan.hash } }); setRes(r); toast(r.ok ? 'The portal saved it' : r.unclear ? 'Check the portal: the reply was not clear' : 'The portal refused it'); }
    catch (e: any) { setErr(e.message); } finally { setBusy(false); }
  };
  const plan = pv?.plan;
  return <Modal wide onClose={res ? onDone : onClose}>
    <h3>{ACT[action]}: {name}</h3>
    {res ? <>
      <div className={`note ${res.ok ? '' : 'warn'}`}>{res.ok ? 'Saved on the portal.' : res.unclear ? 'The portal’s reply was not clear. Look the person up on the portal, then say on the card whether it is there. Do not send again before that.' : 'The portal refused it.'}{res.message ? ` The portal said: “${res.message}”.` : ''}{res.todoTicked ? ` Punch machine to-do ticked: ${res.todoTicked}.` : ''}</div>
      {res.ok && action === 'create' && <p className="mute" style={{ fontSize: 12.5 }}>Code {res.code} is now the Attendance code. Saving on the portal does not put the person on the punch machine: enrol them on the machine as you do today.</p>}
      <div className="row"><button className="btn primary" onClick={onDone}>Close</button></div>
    </> : <>
      {action === 'create' && <Field label="Portal employee code (card number follows it)"><input value={code} onChange={(e) => { setCode(e.target.value); setPv(null); }} /></Field>}
      {action === 'create' && <label className="row" style={{ gap: 6, fontSize: 12.5 }}><input type="checkbox" checked={notSame} onChange={(e) => { setNotSame(e.target.checked); setPv(null); }} />The person Match portal codes proposed is someone else</label>}
      {err && <div className="note warn">{err}</div>}
      {!plan && <div className="row"><button className="btn primary" disabled={busy || (action === 'create' && !code.trim())} onClick={preview}>{busy ? 'Reading the portal’s form…' : 'Preview what will be sent'}</button><button className="btn" onClick={onClose}>Cancel</button></div>}
      {plan && <>
        <p className="mute" style={{ fontSize: 12.5 }}>Exactly what will be posted to the portal’s form {plan.page}, button “{plan.button}”. Controls without an HRMS value keep what the portal’s form has.</p>
        <div className="tw" style={{ maxHeight: 360, overflow: 'auto' }}><table><thead><tr><th>Portal field</th><th>Value</th><th>From the HRMS</th></tr></thead><tbody>
          {plan.rows.map((r: any) => <tr key={r.name}><td>{r.label || r.name}{r.required && ' *'}</td><td className="mono">{r.shown ? `${r.shown} (${r.value})` : r.value}</td><td>{r.role ? 'Yes' : ''}</td></tr>)}
        </tbody></table></div>
        {plan.unused?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>The portal’s form has no place for: {plan.unused.join(', ')} (not sent).</p>}
        {plan.problems?.length > 0 && <div className="note warn">Cannot send: {plan.problems.join('; ')}</div>}
        <div className="row"><button className={`btn ${action === 'left' ? 'bad' : 'primary'}`} disabled={busy || plan.problems?.length > 0} onClick={send}>{busy ? 'Sending…' : `Confirm: ${ACT[action].toLowerCase()}`}</button><button className="btn" onClick={onClose}>Cancel</button></div>
      </>}
    </>}
  </Modal>;
}
