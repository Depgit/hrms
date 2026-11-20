import { useState } from 'react';
import { api, fmtD } from '../api';
import { Loading, Pill, toast, useLoad } from './ui';

/** The "Realtime portal" card of the employee file (backend portal_write.go, portal_auto.go). The HRMS writes to the
 *  portal by itself (19 Nov 2026): created at Mark joined, updated when the details change, marked left after the
 *  exit. The card only shows where it stands; Retry (after a failure or a hold) is the one control left. */

type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'n';
const VERB: Record<string, string> = { create: 'create', update: 'update', left: 'mark left' };

export function PortalCard({ id }: { id: string }) {
  const d = useLoad(() => api(`/employees/${id}/portal`), [id]);
  const [busy, setBusy] = useState(false);
  const x = d.data;
  if (!x) return <div className="card" style={{ marginTop: 14 }}><h3>Realtime portal</h3><Loading error={d.error} /></div>;
  const s = x.state;
  const a = x.auto ?? {};
  const call = async (path: string, body: any, ok: string) => {
    setBusy(true);
    try { await api(`/employees/${id}/portal/${path}`, { body }); toast(ok); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  let status: string = s.status, tone: Tone = 'n';
  if (a.phase === 'sending' || (a.due && !a.phase && !x.notReady)) { status = 'Sending'; tone = 'info'; }
  else if (a.phase === 'failed') { status = 'Failed'; tone = 'bad'; }
  else if (a.phase === 'blocked') { status = 'Held: mapping needed'; tone = 'warn'; }
  else if (a.phase === 'waiting') { status = 'Waiting to send'; tone = 'warn'; }
  else tone = ({ 'Not on portal': 'warn', 'On portal': 'ok', 'Changed since last send': 'info', Left: 'n', 'Check on the portal': 'bad' } as Record<string, Tone>)[s.status] ?? 'n';
  const matchHeld = a.phase === 'blocked' && s.matches?.length > 0;
  return <div className="card" style={{ marginTop: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Realtime portal</h3><Pill k={tone}>{status}</Pill></div>
    <p className="mute" style={{ fontSize: 12.5 }}>
      {s.code ? <>Portal code <b className="mono">{s.code}</b>. </> : 'No portal code yet. '}
      {a.phase && a.action && <>Pending: {VERB[a.action] ?? a.action}{a.at ? ` (since ${fmtD(String(a.at).slice(0, 10))})` : ''}. </>}
      {s.last?.at && <>Last result {fmtD(String(s.last.at).slice(0, 10))} by {s.last.by}{s.last.message ? `: “${s.last.message}”` : ''}{s.last.refused ? ' (refused)' : ''}.</>}
    </p>
    {a.message && <div className={`note ${a.phase === 'failed' ? 'bad' : 'warn'}`}>{a.message}</div>}
    {x.notReady && <p className="mute" style={{ fontSize: 12 }}>{x.notReady}. Writes due wait and go out when HR confirms them under Attendance › Punch machine setup.</p>}
    {x.canSend && <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
      {(a.phase === 'failed' || a.phase === 'blocked') && !x.notReady && <button className="btn primary" disabled={busy} onClick={() => call('retry', {}, 'Queued: sending once more')}>Retry</button>}
      {matchHeld && !x.notReady && <button className="btn" disabled={busy} onClick={() => call('retry', { notSame: true }, 'Queued: creating them as a new person')}>Retry: they are someone else</button>}
      {s.status === 'Check on the portal' && <>
        <button className="btn" disabled={busy} onClick={() => call('resolve', { on: true }, 'Saved')}>I checked: it is on the portal</button>
        <button className="btn" disabled={busy} onClick={() => call('resolve', { on: false }, 'Saved')}>I checked: it is not</button></>}
      <button className="btn sm" disabled={busy} onClick={() => d.reload()}>Refresh</button>
    </div>}
    {x.sends?.length > 0 && <details style={{ marginTop: 8 }}><summary className="mute" style={{ fontSize: 12 }}>Writes ({x.sends.length})</summary>
      <ul style={{ fontSize: 12.5 }}>{x.sends.map((r: any) => <li key={r.id}>{fmtD(String(r.at).slice(0, 10))} · {r.action} · code {r.code} · {r.ok ? 'saved' : r.unclear ? 'unclear' : 'refused'}{r.message ? ` · “${r.message}”` : ''} · {r.by}</li>)}</ul></details>}
  </div>;
}
