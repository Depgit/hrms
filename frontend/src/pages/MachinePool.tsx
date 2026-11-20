import { useState } from 'react';
import { api, fmtD } from '../api';
import { Adv, Field, Loading, Modal, Pill, download, toast, useLoad } from '../components/ui';

/** Punch machines as one pool per cluster (Attendance › Punch machine setup; backend machines.go). Each machine
 *  holds about 1,000 people; the machines of a cluster are one shared store. Import the machines from the Realtime
 *  portal, map each to a property, watch each cluster's capacity, and work through the enrol/remove to-do list. */

type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'n';
const LEVEL: Record<string, [Tone, string]> = { ok: ['ok', 'OK'], warn: ['warn', 'Nearly full'], full: ['bad', 'Full'], none: ['n', 'No machines'] };
const whenT = (at: string) => new Date(at).toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' });
const n = (v: number) => (v ?? 0).toLocaleString('en-IN');

export default function MachinePool({ ed }: { ed: boolean }) {
  const d = useLoad(() => api('/attendance/machines'), []);
  const [kind, setKind] = useState('All');
  const td = useLoad(() => api(`/attendance/machines/todo?kind=${kind}`), [kind]);
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const [busy, setBusy] = useState('');
  const [sel, setSel] = useState<Record<string, boolean>>({});
  const [plan, setPlan] = useState<any>(null);
  const [defCap, setDefCap] = useState('');
  const reload = () => { d.reload(); td.reload(); };
  const run = async (key: string, fn: () => Promise<any>, msg: (r: any) => string) => {
    setBusy(key);
    try { const r = await fn(); toast(msg(r)); reload(); } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const map = (id: number, body: any) => run('map' + id, () => api(`/attendance/machines/${id}/map`, { body }), () => 'Saved');
  const openPlan = async (c: any) => { try { setPlan(await api(`/attendance/machines/plan?clusterId=${c.id}`)); } catch (e: any) { toast(e.message); } };
  const data = d.data;
  const open: any[] = td.data?.open ?? [];
  const ticked = Object.keys(sel).filter((k) => sel[k]);
  const hints = (data?.machines ?? []).filter((m: any) => m.hint?.locationId && m.status === 'Active').length;
  const check = td.data?.lastCheck ?? data?.lastCheck;

  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Punch machines by cluster</h3>
      {data && <Pill k={data.clusters.some((c: any) => c.level === 'full') ? 'bad' : data.clusters.some((c: any) => c.level === 'warn') ? 'warn' : 'ok'}>{n(data.people)} people · {data.machines.length} machines</Pill>}</div>
    {!data ? <Loading error={d.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5 }}>Each machine holds about {n(data.defaultCapacity)} people. The machines of one cluster work as one shared store: a person can punch at any machine of their cluster, so a move between properties of a cluster needs nothing, and a move to another cluster needs enrolment there. A cluster’s capacity is the total of its machines; it shows a warning from {data.warnPct}% and refuses new joiners and transfers when full unless you choose to go ahead.</p>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
        {ed && <button className="btn primary" disabled={!!busy || !data.envSet} onClick={() => run('import', () => api('/attendance/machines/import', { method: 'POST' }), (r) => `${r.machines} machines read: ${r.added} new, ${r.updated} updated`)}>{busy === 'import' ? 'Reading…' : 'Import machines from the portal'}</button>}
        {ed && hints > 0 && <button className="btn" disabled={!!busy} onClick={() => run('hints', () => api('/attendance/machines/use-hints', { method: 'POST' }), (r) => `${r.mapped} machines mapped`)}>Use the {hints} suggested properties</button>}
        {ed && data.enrolled === 0 && data.people > 0 && <button className="btn" disabled={!!busy} onClick={() => confirm(`Record all ${data.people} active employees as enrolled on their cluster’s machines? Do this once, when the machines already hold today’s staff.`) && run('base', () => api('/attendance/machines/baseline', { method: 'POST' }), (r) => `${r.recorded} recorded as enrolled`)}>Record everyone as enrolled</button>}
        {!data.envSet && <span className="mute" style={{ fontSize: 12.5 }}>Add the portal account (REALTIME_URL, REALTIME_USER, REALTIME_PASS) to the server first.</span>}
        {data.lastImport && <span className="mute" style={{ fontSize: 12.5 }}>Last import {whenT(data.lastImport.at)} by {data.lastImport.by}: {data.lastImport.machines} machines.</span>}
      </div>
      {data.enrolled === 0 && data.people > 0 && <div className="note warn" style={{ marginTop: 10 }}>No one is recorded as enrolled yet, so every active employee shows as a joiner to enrol. If the machines already hold today’s staff, use “Record everyone as enrolled”.</div>}

      <h3 style={{ margin: '16px 0 8px', fontSize: 13 }}>Clusters</h3>
      <div className="tw"><table><thead><tr><th>Cluster</th><th className="num">Machines</th><th className="num">Capacity</th><th className="num">People</th><th className="num">Free</th><th style={{ width: 140 }}>Used</th><th>Status</th><th className="num">To-dos</th><th></th></tr></thead><tbody>
        {data.clusters.map((c: any) => <tr key={c.id}>
          <td><b>{c.name}</b><div className="mute" style={{ fontSize: 12 }}>{c.city}{Object.keys(c.noMachine ?? {}).length > 0 && ` · no machine at ${Object.entries(c.noMachine).map(([k, v]) => `${k} (${v})`).join(', ')}`}</div></td>
          <td className="num">{c.machines}<div className="mute" style={{ fontSize: 12 }}>{c.online ? `${c.online} online` : ''}{c.offline ? ` ${c.offline} offline` : ''}{c.unknown && c.machines ? `${c.unknown} not reported` : ''}</div></td>
          <td className="num">{n(c.capacity)}</td><td className="num">{n(c.people)}</td><td className="num" style={{ color: c.free < 0 ? 'var(--bad)' : undefined }}>{n(c.free)}</td>
          <td>{c.capacity > 0 && <div className="bar" style={{ gridTemplateColumns: 'minmax(0,1fr) 40px', margin: 0 }}><i><b style={{ width: `${Math.min(100, c.pct)}%`, background: c.level === 'full' ? 'var(--bad)' : c.level === 'warn' ? 'var(--warn)' : undefined }} /></i><span>{c.pct}%</span></div>}</td>
          <td><Pill k={LEVEL[c.level][0]}>{LEVEL[c.level][1]}</Pill>{c.unplaced > 0 && <div className="mute" style={{ fontSize: 12 }}>{c.unplaced} without a place</div>}</td>
          <td className="num">{c.todo || ''}</td>
          <td>{c.machines > 0 && <button className="btn sm" onClick={() => openPlan(c)}>Where records go</button>}</td>
        </tr>)}
      </tbody></table></div>

      <h3 style={{ margin: '16px 0 8px', fontSize: 13 }}>Machines</h3>
      <div className="tw"><table><thead><tr><th>Machine</th><th>Portal branch</th><th>Property</th><th>Cluster</th><th className="num">Holds</th><th className="num">Records here</th><th>Online</th></tr></thead><tbody>
        {data.machines.length === 0 && <tr><td colSpan={7} className="mute">No machines yet. Import them from the portal.</td></tr>}
        {data.machines.map((m: any) => <tr key={m.id} style={{ opacity: m.status === 'Active' ? 1 : 0.55 }}>
          <td><b>{m.name}</b> <span className="mono mute">{m.serial}</span><div className="mute" style={{ fontSize: 12 }}>{[m.model, m.mode, m.vendor].filter(Boolean).join(' · ')}{m.status !== 'Active' ? ' · retired' : ''}{m.gone ? ' · no longer in the portal’s list' : ''}</div></td>
          <td>{m.branch || '—'}</td>
          <td>{ed && m.status === 'Active' ? <select value={m.locationId ?? ''} disabled={!!busy} onChange={(e) => map(m.id, { locationId: e.target.value })} style={{ maxWidth: 220 }}>
            <option value="">Not mapped</option>{(locs.data ?? []).map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</select> : m.location ?? '—'}
            {!m.locationId && m.hint && <div className="mute" style={{ fontSize: 12 }}>Suggested: {m.hint.location ?? `a property of ${m.hint.cluster}`}{ed && m.hint.locationId && <> <button className="btn sm" disabled={!!busy} onClick={() => map(m.id, { locationId: m.hint.locationId })}>Use</button></>}</div>}</td>
          <td>{m.cluster ?? '—'}</td>
          <td className="num">{ed && m.status === 'Active' ? <input type="number" min={0} defaultValue={m.capacitySet ? m.capacity : ''} placeholder={String(data.defaultCapacity)} style={{ width: 80 }}
            onBlur={(e) => { const v = e.target.value.trim(); if (v === (m.capacitySet ? String(m.capacity) : '')) return; map(m.id, v === '' ? { defaultCapacity: true } : { capacity: +v }); }} /> : n(m.capacity)}</td>
          <td className="num">{m.cluster ? n(m.planned) : ''}</td>
          <td>{m.online == null ? <span className="mute" style={{ fontSize: 12 }}>{m.vendor === 'Realtime' ? 'Not shown by the portal' : '—'}</span> : <Pill k={m.online ? 'ok' : 'bad'}>{m.online ? 'Online' : 'Offline'}</Pill>}</td>
        </tr>)}
      </tbody></table></div>
      {ed && <div className="row" style={{ gap: 8, marginTop: 10, alignItems: 'flex-end' }}>
        <Field label="A machine holds (people, by default)"><input type="number" min={1} value={defCap} placeholder={String(data.defaultCapacity)} onChange={(e) => setDefCap(e.target.value)} style={{ width: 120 }} /></Field>
        <button className="btn sm" disabled={!!busy || !defCap} onClick={() => run('cap', () => api('/attendance/machines/settings', { body: { defaultCapacity: +defCap } }), () => 'Saved').then(() => setDefCap(''))}>Save</button>
      </div>}

      <div className="row sp" style={{ margin: '18px 0 8px' }}><h3 style={{ margin: 0, fontSize: 13 }}>To-do on the machines</h3>
        <div className="seg">{['All', 'Enrol', 'Move', 'Remove'].map((k) => <button key={k} className={kind === k ? 'on' : ''} onClick={() => { setKind(k); setSel({}); }}>{k}</button>)}</div></div>
      <p className="mute" style={{ fontSize: 12.5, marginTop: 0 }}>The HRMS does not add or delete anyone on the portal; the portal operator does, and ticks the task here. Fetching the portal’s employee list (“Match portal codes” above) ticks the tasks it can confirm: the person’s Attendance code is in a branch of the cluster to enrol on, and no longer in one of the cluster to remove from.
        {check && <> Last checked {whenT(check.at)}: {check.done} ticked from {check.portal} portal employees.{check.unmappedBranches?.length > 0 && ` ${check.unmappedBranches.length} portal branches are not tied to a cluster (map their machines).`}</>}</p>
      {ed && <div className="row" style={{ gap: 8, flexWrap: 'wrap', marginBottom: 8 }}>
        <button className="btn sm primary" disabled={!!busy || !ticked.length} onClick={() => run('done', () => api('/attendance/machines/todo/done', { body: { employeeIds: ticked } }), (r) => `${r.done} ticked done`).then(() => setSel({}))}>Tick done ({ticked.length})</button>
        <button className="btn sm" disabled={!!busy || !open.length} onClick={() => setSel(Object.fromEntries(open.map((t) => [t.employeeId, true])))}>Select all</button>
        <button className="btn sm" onClick={() => download('/attendance/machines/todo.xlsx', 'punch-machine-todo.xlsx').catch((e) => toast(e.message))}>Download Excel</button>
      </div>}
      {!td.data ? <Loading error={td.error} /> : <div className="tw"><table><thead><tr>{ed && <th style={{ width: 28 }}></th>}<th>What to do</th><th>Employee</th><th>Code</th><th>Property</th><th>Since</th><th>Portal check</th></tr></thead><tbody>
        {open.length === 0 && <tr><td colSpan={ed ? 7 : 6} className="mute">Nothing to do.</td></tr>}
        {open.map((t) => <tr key={t.employeeId}>
          {ed && <td><input type="checkbox" checked={!!sel[t.employeeId]} onChange={() => setSel({ ...sel, [t.employeeId]: !sel[t.employeeId] })} /></td>}
          <td><Pill k={t.kind === 'Remove' ? 'warn' : t.kind === 'Move' ? 'info' : 'ok'}>{t.kind}</Pill> {t.task}</td>
          <td>{t.name}<div className="mute mono" style={{ fontSize: 12 }}>{t.employeeId}</div></td>
          <td className="mono">{t.code || '—'}</td><td>{t.location || '—'}</td><td>{t.since ? fmtD(t.since) : '—'}</td>
          <td style={{ fontSize: 12.5 }}>{t.note || ''}</td>
        </tr>)}
      </tbody></table></div>}
      {(td.data?.done ?? []).length > 0 && <Adv title={`Done in the last 30 days (${td.data.done.length})`}>
        <ul className="log">{td.data.done.map((x: any, i: number) => <li key={i}><div>{x.name} ({x.employeeId}): {x.task}</div><div className="mute" style={{ fontSize: 12 }}>{x.how === 'portal' ? 'Seen in the portal' : `Ticked by ${x.by}`} · {whenT(x.at)}</div></li>)}</ul>
      </Adv>}
    </>}
    {plan && <Modal wide onClose={() => setPlan(null)}><h2 style={{ marginTop: 0 }}>Where records go: {plan.cluster}</h2>
      <p className="ink2" style={{ fontSize: 13 }}>Each person’s record goes on their own property’s machine first; when it is full, on the next machine of the cluster with space. Earliest joiners keep their place. {plan.unplaced > 0 && <b>{plan.unplaced} people have no place: add a machine or raise a capacity.</b>}</p>
      <div className="tw"><table><thead><tr><th>Machine</th><th>Property</th><th className="num">Holds</th><th className="num">Records</th></tr></thead><tbody>
        {plan.machines.map((m: any) => <tr key={m.id}><td>{m.name} <span className="mono mute">{m.serial}</span></td><td>{m.location}</td><td className="num">{n(m.capacity)}</td><td className="num">{n(m.planned)}</td></tr>)}
      </tbody></table></div>
      <div className="tw" style={{ marginTop: 12, maxHeight: 360, overflow: 'auto' }}><table><thead><tr><th>Employee</th><th>Property</th><th>Machine</th></tr></thead><tbody>
        {plan.people.map((p: any) => <tr key={p.employeeId}><td>{p.name} <span className="mono mute">{p.employeeId}</span></td><td>{p.location}</td>
          <td>{p.machine ?? <span style={{ color: 'var(--bad)' }}>No place</span>}{p.overflow && <span className="mute"> (overflow)</span>}</td></tr>)}
      </tbody></table></div>
    </Modal>}
  </div>;
}
