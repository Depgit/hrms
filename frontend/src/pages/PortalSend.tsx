import { useState } from 'react';
import { api, fmtD } from '../api';
import { Field, Loading, Pill, toast, useLoad } from '../components/ui';
import { PortalSendDialog } from '../components/PortalCard';

/** Attendance › Punch machine setup › Realtime portal: sending employees (backend portal_write.go). The list of
 *  people waiting to be sent (new joiners, changes, exits, unclear sends) and the one-time mapping of HRMS
 *  departments, designations, properties and shifts to the portal's lists. Every send is previewed and confirmed. */

const KINDS: [string, string][] = [['dept', 'Departments'], ['desig', 'Designations'], ['branch', 'Properties → portal branch'], ['shift', 'Shifts']];
const WK: Record<string, [string, 'ok' | 'warn' | 'bad' | 'info' | 'n']> = { New: ['Not on portal', 'warn'], Changed: ['Changed', 'info'], Leave: ['Mark left', 'bad'], Check: ['Check', 'bad'] };
const ACTION: Record<string, string> = { New: 'create', Changed: 'update', Leave: 'left' };

export default function PortalSend() {
  const w = useLoad(() => api('/attendance/portal'), []);
  const [kind, setKind] = useState('All');
  const [showMap, setShowMap] = useState(false);
  const [send, setSend] = useState<any>(null);
  const d = w.data;
  const rows = (d?.rows ?? []).filter((r: any) => kind === 'All' || r.kind === kind);
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Realtime portal: waiting to send</h3>
      {d && <Pill k={d.rows.length ? 'warn' : 'ok'}>{d.rows.length} waiting</Pill>}</div>
    {!d ? <Loading error={w.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5 }}>New joiners, changed details and exits are sent to the Realtime portal only when you press Send and confirm the preview. People already on the portal without an Attendance code should get their code from “Match portal codes” first, or the portal gets a second record.</p>
      {d.notReady && <div className="note warn">{d.notReady}</div>}
      <div className="row" style={{ gap: 6, flexWrap: 'wrap' }}>
        {['All', 'New', 'Changed', 'Leave', 'Check'].map((k) => <button key={k} className={`btn sm ${kind === k ? 'primary' : ''}`} onClick={() => setKind(k)}>{k === 'All' ? 'All' : WK[k][0]}{k !== 'All' ? ` (${d.counts[k] ?? 0})` : ''}</button>)}
        <button className="btn sm" onClick={() => setShowMap(!showMap)}>{showMap ? 'Hide mapping' : 'Mapping and code rule'}</button>
      </div>
      {rows.length > 0 && <div className="tw" style={{ marginTop: 10, maxHeight: 420, overflow: 'auto' }}><table><thead><tr><th>Employee</th><th>To do</th><th>Property</th><th>Joined / left</th><th>Code</th><th>Note</th><th></th></tr></thead><tbody>
        {rows.map((r: any) => <tr key={r.employeeId + r.kind}>
          <td><b>{r.name}</b><div className="mute mono" style={{ fontSize: 12 }}>{r.employeeId}</div></td>
          <td><Pill k={WK[r.kind][1]}>{WK[r.kind][0]}</Pill></td>
          <td>{r.location}<div className="mute" style={{ fontSize: 12 }}>{r.dept}{r.desig ? ` · ${r.desig}` : ''}</div></td>
          <td>{r.kind === 'Leave' ? (r.lwd ? fmtD(r.lwd) : '') : fmtD(r.doj)}</td>
          <td className="mono">{r.code}</td>
          <td style={{ fontSize: 12 }}>{r.blocked?.length > 0 && r.kind !== 'Leave' ? <span style={{ color: 'var(--bad)' }}>{r.blocked.join('; ')}</span> : r.changed?.length > 0 ? `Changed: ${r.changed.join(', ')}` : r.matches?.length > 0 ? `Portal may have: ${r.matches.join('; ')}` : ''}</td>
          <td>{d.canSend && !d.notReady && ACTION[r.kind] && !(r.blocked?.length > 0 && r.kind !== 'Leave') && <button className="btn sm" onClick={async () => {
            let code = r.code;
            if (r.kind === 'New') { try { code = (await api(`/employees/${r.employeeId}/portal`)).suggestCode ?? ''; } catch { code = ''; } }
            setSend({ ...r, action: ACTION[r.kind], code });
          }}>{r.kind === 'Leave' ? 'Mark left' : 'Send'}</button>}</td>
        </tr>)}
      </tbody></table></div>}
      {rows.length === 0 && <p className="mute" style={{ fontSize: 12.5 }}>Nothing waiting.</p>}
      {showMap && <PortalMapping />}
    </>}
    {send && <PortalSendDialog id={send.employeeId} name={send.name} action={send.action} code={send.code} onClose={() => setSend(null)} onDone={() => { setSend(null); w.reload(); }} />}
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
