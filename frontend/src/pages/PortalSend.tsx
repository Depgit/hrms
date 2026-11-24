import { useState } from 'react';
import { api, fmtD } from '../api';
import { Field, Loading, Pill, toast, useLoad } from '../components/ui';

/** Attendance › Punch machine setup › Realtime portal (backend portal_write.go, portal_auto.go). The HRMS writes to
 *  the portal by itself (19 Nov 2026): joiners are created at Mark joined, changed details updated, exits marked
 *  left, one job each, never retried by itself. This is the waiting list of what did not go out (sending off,
 *  held by a mapping, failed, unclear) with Retry, "Send waiting" for the backlog, and the one-time mapping. */

const KINDS: [string, string][] = [['dept', 'Departments'], ['desig', 'Designations'], ['branch', 'Properties → portal branch'], ['shift', 'Shifts']];
const WK: Record<string, [string, 'ok' | 'warn' | 'bad' | 'info' | 'n']> = { Waiting: ['Waiting (sending off)', 'warn'], Held: ['Held: mapping', 'warn'], Failed: ['Failed', 'bad'], Sending: ['Sending', 'info'], Check: ['Check on the portal', 'bad'] };
const VERB: Record<string, string> = { create: 'Create', update: 'Update', left: 'Mark left' };
/** A group the screen does not know (a newer server) still shows, as its own name (from main's fix, 24 Nov 2026). */
const wk = (k: string): [string, 'ok' | 'warn' | 'bad' | 'info' | 'n'] => WK[k] ?? [k || 'Other', 'n'];

export default function PortalSend() {
  const w = useLoad(() => api('/attendance/portal'), []);
  const [kind, setKind] = useState('All');
  const [showMap, setShowMap] = useState(false);
  const [busy, setBusy] = useState('');
  const d = w.data;
  const rows = (d?.rows ?? []).filter((r: any) => kind === 'All' || r.kind === kind);
  const run = async (key: string, path: string, body: any, ok: (r: any) => string) => {
    setBusy(key);
    try { const r = await api(path, { body }); toast(ok(r)); w.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Realtime portal: automatic sending</h3>
      {d && <Pill k={(d.rows ?? []).length ? 'warn' : 'ok'}>{(d.rows ?? []).length ? `${(d.rows ?? []).length} not sent` : 'All sent'}</Pill>}</div>
    {!d ? <Loading error={w.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5 }}>New joiners are created on the portal when marked joined, changed details are updated and exits are marked left, by the HRMS itself. A write is made once and never repeated by itself: what failed or was held waits here with the portal’s message until you press Retry. People already on the portal without an Attendance code should get their code from “Match portal codes”.</p>
      {d.notReady && <div className="note warn">{d.notReady}. Writes due wait here; once sending is on, press “Send waiting”.</div>}
      <div className="row" style={{ gap: 6, flexWrap: 'wrap' }}>
        {['All', 'Waiting', 'Held', 'Failed', 'Sending', 'Check'].map((k) => <button key={k} className={`btn sm ${kind === k ? 'primary' : ''}`} onClick={() => setKind(k)}>{k === 'All' ? 'All' : wk(k)[0]}{k !== 'All' ? ` (${d.counts?.[k] ?? 0})` : ''}</button>)}
        {d.canSend && !d.notReady && (d.counts.Waiting ?? 0) > 0 && <button className="btn sm primary" disabled={!!busy} onClick={() => window.confirm(`Send the ${d.counts.Waiting} waiting write(s) to the portal now?`) && run('backlog', '/attendance/portal/backlog', {}, (r) => `${r.queued} queued`)}>Send waiting ({d.counts.Waiting})</button>}
        <button className="btn sm" onClick={() => setShowMap(!showMap)}>{showMap ? 'Hide mapping' : 'Mapping and code rule'}</button>
      </div>
      {rows.length > 0 && <div className="tw" style={{ marginTop: 10, maxHeight: 420, overflow: 'auto' }}><table><thead><tr><th>Employee</th><th>Status</th><th>Write</th><th>Property</th><th>Joined / left</th><th>Code</th><th>Message</th><th></th></tr></thead><tbody>
        {rows.map((r: any) => <tr key={r.employeeId}>
          <td><b>{r.name}</b><div className="mute mono" style={{ fontSize: 12 }}>{r.employeeId}</div></td>
          <td><Pill k={wk(r.kind)[1]}>{wk(r.kind)[0]}</Pill></td>
          <td>{VERB[r.action] ?? r.action}</td>
          <td>{r.location}<div className="mute" style={{ fontSize: 12 }}>{r.dept}{r.desig ? ` · ${r.desig}` : ''}</div></td>
          <td>{r.action === 'left' ? (r.lwd ? fmtD(r.lwd) : '') : fmtD(r.doj)}</td>
          <td className="mono">{r.code}</td>
          <td style={{ fontSize: 12 }}>{r.message}{r.at ? <div className="mute">{fmtD(String(r.at).slice(0, 10))}</div> : null}</td>
          <td>{d.canSend && !d.notReady && (r.kind === 'Failed' || r.kind === 'Held') && <button className="btn sm" disabled={!!busy} onClick={() => run(r.employeeId, `/employees/${r.employeeId}/portal/retry`, {}, () => 'Queued')}>Retry</button>}
            {r.kind === 'Check' && <a className="btn sm" href={`/file?id=${encodeURIComponent(r.employeeId)}`}>Settle</a>}</td>
        </tr>)}
      </tbody></table></div>}
      {rows.length === 0 && <p className="mute" style={{ fontSize: 12.5 }}>Nothing waiting.</p>}
      {showMap && <PortalMapping />}
    </>}
  </div>;
}

function PortalMapping() {
  const m = useLoad(() => api('/attendance/portal/map'), []);
  const [busy, setBusy] = useState('');
  const [start, setStart] = useState('');
  const [pad, setPad] = useState('');
  const x = m.data;
  const run = async (key: string, fn: () => Promise<any>, msg: (r: any) => string) => {
    setBusy(key);
    try { const r = await fn(); toast(msg(r)); m.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  if (!x) return <Loading error={m.error} />;
  const ed = x.canEdit;
  const put = (kind: string, ref: string, value: string) => run(kind + ref, () => api('/attendance/portal/map', { method: 'PUT', body: { kind, ref, value } }), () => 'Saved');
  return <div style={{ marginTop: 14 }}>
    <h3 style={{ fontSize: 13 }}>Mapping to the portal’s lists</h3>
    <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
      {ed && <button className="btn" disabled={!!busy} onClick={() => run('lists', () => api('/attendance/portal/lists', { method: 'POST' }), (r) => `Read: ${r.dept} departments, ${r.desig} designations, ${r.branch} branches, ${r.shift} shifts`)}>{busy === 'lists' ? 'Reading…' : 'Read the lists from the portal'}</button>}
      <span className="mute" style={{ fontSize: 12.5 }}>{x.listsAt ? `Lists read ${fmtD(String(x.listsAt).slice(0, 10))}.` : 'The portal’s lists have not been read yet; you can also type the portal’s exact text.'}</span>
    </div>
    {KINDS.map(([k, label]) => <div key={k} style={{ marginTop: 10 }}>
      <b style={{ fontSize: 12.5 }}>{label}</b>
      <datalist id={`pl-${k}`}>{(x.portal[k] ?? []).map((o: string) => <option key={o} value={o} />)}</datalist>
      <div className="tw"><table><thead><tr><th>HRMS</th><th className="num">People</th><th>Portal</th></tr></thead><tbody>
        {(x.kinds[k] ?? []).map((r: any) => <tr key={r.ref}>
          <td>{r.name}</td><td className="num">{r.people}</td>
          <td>{ed ? <input list={`pl-${k}`} defaultValue={r.value} placeholder={r.machine ? `${r.machine} (from its machine)` : r.suggest ? `${r.suggest} (same name)` : 'Not mapped'}
            onBlur={(e) => e.target.value.trim() !== (r.value ?? '') && put(k, r.ref, e.target.value)} /> : (r.value || r.machine || <span className="mute">Not mapped</span>)}
            {ed && !r.value && r.suggest && <button className="btn sm" style={{ marginLeft: 6 }} onClick={() => put(k, r.ref, r.suggest)}>Use</button>}</td>
        </tr>)}
      </tbody></table></div>
    </div>)}
    <h3 style={{ fontSize: 13, marginTop: 14 }}>Code rule for new people</h3>
    <p className="mute" style={{ fontSize: 12.5 }}>The HRMS suggests the next free number (above every Attendance code and every code on the portal’s last fetched list); HR can change it before sending. The card number is the code padded with zeros to {x.cardPad} digits.</p>
    {ed && <div className="row" style={{ gap: 8, alignItems: 'flex-end' }}>
      <Field label="Lowest code to suggest"><input type="number" min={0} value={start} placeholder={String(x.codeStart ?? '')} onChange={(e) => setStart(e.target.value)} /></Field>
      <Field label="Card number digits (0 = as the code)"><input type="number" min={0} max={12} value={pad} placeholder={String(x.cardPad ?? 8)} onChange={(e) => setPad(e.target.value)} /></Field>
      <button className="btn" disabled={!!busy || (start === '' && pad === '')} onClick={() => run('set', () => api('/attendance/portal/settings', { method: 'PUT', body: { ...(start !== '' ? { codeStart: Number(start) } : {}), ...(pad !== '' ? { cardPad: Number(pad) } : {}) } }), () => 'Saved')}>Save</button>
    </div>}
  </div>;
}
