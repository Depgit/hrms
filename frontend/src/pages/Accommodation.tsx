import { useEffect, useState, type ReactNode } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, fmtD, inr, openDoc, today, uploadDoc } from '../api';
import { Drawer, Field, Loading, Modal, Pill, Tabs, download, toast, useLoad } from '../components/ui';
import PersonPicker from '../components/PersonPicker';

/** Staff accommodation (workforce plan, module 4): flats, houses and PGs the company rents for staff, their leases
 *  and running costs, who stays in which bed, and the month's cost split among the people who stayed (equally per
 *  person by default; "Split by days stayed" in Settings). The split is the company's own cost calculation only: it is
 *  never part of payroll, the payslip or CTC. GET/PUT /api/hst/… (hst.go). */

type T = 'board' | 'units' | 'stays' | 'waiting' | 'split' | 'settings';
type Item = { label: string; amount: number };
type Setup = {
  settings: { byDays: boolean; leaseDays: number; incDays: number; boardDays: number };
  clusters: { id: string; name: string; city: string; active: boolean }[];
  locations: { id: string; name: string; clusterId: string; active: boolean }[];
  today: string; openYm: string; can: { setup: number; allot: number };
};

const ymNow = () => today().slice(0, 7);
const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });
const Stat = ({ l, n, s }: { l: string; n: ReactNode; s?: ReactNode }) => <div className="card stat"><div className="l">{l}</div><div className="n">{n}</div><div className="s">{s}</div></div>;
const genderPill = (g: string) => <Pill k={g === 'Any' ? 'n' : 'info'}>{g === 'Any' ? 'Any gender' : g}</Pill>;

export default function Accommodation() {
  const [qs, setQs] = useSearchParams();
  const setup = useLoad(() => api<Setup>('/hst/setup'), []);
  const s = setup.data;
  const tabs: [T, string][] = [['board', 'Occupancy board'], ['units', 'Units and leases'],
    ...(s && s.can.allot > 0 ? [['stays', 'Check-in and check-out'], ['waiting', 'Waiting for a bed']] as [T, string][] : []),
    ...(s && s.can.setup > 0 ? [['split', 'Month cost split'], ['settings', 'Settings']] as [T, string][] : [])];
  const t = (tabs.find(([k]) => k === qs.get('tab'))?.[0] ?? 'board') as T;
  const unit = qs.get('unit') || '';
  const go = (k: T) => setQs({ tab: k });
  const openUnit = (id: string) => setQs({ tab: t, unit: id });
  if (!s) return <Loading error={setup.error} />;
  return <>
    <Tabs tabs={tabs} value={t} onChange={go} />
    {t === 'board' ? <Board onUnit={openUnit} />
      : t === 'units' ? <Units s={s} onUnit={openUnit} />
      : t === 'stays' ? <Stays s={s} />
      : t === 'waiting' ? <Waiting />
      : t === 'split' ? <Split />
      : <Settings s={s} onSaved={setup.reload} />}
    {unit && <UnitDrawer id={unit} s={s} onClose={() => setQs({ tab: t })} />}
  </>;
}

// ---------- Waiting for a bed (hst_auto.go) ----------

/** Joiners whose offer had company accommodation but no bed was free when they joined. The next free bed in the
 *  cluster is given automatically (the 5-minute sweep); a check-in by hand also takes them off the list. */
function Waiting() {
  const d = useLoad(() => api('/hst/waiting'), []);
  const x = d.data;
  const drop = async (id: string) => {
    const reason = window.prompt('Why does this person no longer need a bed?');
    if (!reason?.trim()) return;
    try { await api(`/hst/waiting/${encodeURIComponent(id)}/drop`, { body: { reason } }); toast('Taken off the list'); d.reload(); } catch (e: any) { toast(e.message); }
  };
  if (!x) return <Loading error={d.error} />;
  return <div className="card">
    <h3>Waiting for a bed</h3>
    <p className="mute" style={{ fontSize: 12.5 }}>Joiners tagged for company accommodation on the offer when no bed was free. They get the next free bed in their cluster by themselves; you can also check them in by hand under Check-in and check-out.</p>
    {x.rows.length === 0 ? <p className="mute" style={{ fontSize: 12.5 }}>Nobody is waiting.</p>
      : <div className="tw"><table><thead><tr><th>Employee</th><th>Gender</th><th>Property</th><th>Cluster</th><th>Waiting since</th><th></th></tr></thead><tbody>
        {x.rows.map((r: any) => <tr key={r.employeeId}>
          <td><b>{r.name}</b><div className="mute mono" style={{ fontSize: 12 }}>{r.employeeId}</div></td><td>{r.gender || '—'}</td><td>{r.location}</td>
          <td>{r.cluster}{r.city ? `, ${r.city}` : ''}</td><td>{fmtD(r.since)}</td>
          <td>{x.canEdit && <button className="btn sm" onClick={() => drop(r.employeeId)}>No longer needed</button>}</td>
        </tr>)}</tbody></table></div>}
  </div>;
}

// ---------- Occupancy board ----------

function Board({ onUnit }: { onUnit: (id: string) => void }) {
  const [ym, setYm] = useState(ymNow());
  const d = useLoad(() => api(`/hst/board?ym=${ym}`), [ym]);
  const x = d.data;
  const dl = () => download(`/hst/board.xlsx?ym=${ym}`, `accommodation-occupancy-${ym}.xlsx`).catch((e) => toast(e.message));
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 10 }}>
      <div className="row" style={{ gap: 10 }}><Field label="Cost month"><input type="month" value={ym} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field>
        <span className="mute" style={{ fontSize: 12.5 }}>Beds as of today; costs for {ymLabel(ym)}.</span></div>
      <button className="btn" onClick={dl}>Download Excel</button>
    </div></div>
    {!x ? <Loading error={d.error} /> : <>
      <div className="grid g4" style={{ marginBottom: 14 }}>
        <Stat l="Beds" n={x.total.beds} s={`${x.total.units} active units`} />
        <Stat l="Filled · free" n={`${x.total.filled} · ${x.total.free}`} s={`Free: ${x.total.freeMale} male, ${x.total.freeFemale} female, ${x.total.freeAny} any`} />
        <Stat l={`Cost, ${ymLabel(ym)}`} n={inr(x.total.cost)} s={`${inr(x.total.allocated)} allocated to staff`} />
        <Stat l="Empty-unit cost" n={inr(x.total.empty)} s="charged to the properties the units serve" />
      </div>
      <div className="card" style={{ marginBottom: 14 }}><h3>By cluster</h3>
        {x.rows.length === 0 ? <p className="mute">No units yet. Add one under Units and leases.</p> :
          <div className="tw"><table><thead><tr><th>City</th><th>Cluster</th><th className="num">Units</th><th className="num">Beds</th><th className="num">Filled</th><th className="num">Free</th>
            <th className="num">Free male</th><th className="num">Free female</th><th className="num">Free any</th><th className="num">Month cost</th><th className="num">Allocated</th><th className="num">Empty-unit cost</th><th className="num">Leases ending</th></tr></thead>
            <tbody>{x.rows.map((r: any) => <tr key={r.clusterId}><td>{r.city}</td><td>{r.cluster}</td><td className="num">{r.units}</td><td className="num">{r.beds}</td><td className="num">{r.filled}</td>
              <td className="num"><b>{r.free}</b></td><td className="num">{r.freeMale}</td><td className="num">{r.freeFemale}</td><td className="num">{r.freeAny}</td><td className="num">{inr(r.cost)}</td>
              <td className="num">{inr(r.allocated)}</td><td className="num">{inr(r.empty)}</td><td className="num">{r.ending || ''}</td></tr>)}</tbody></table></div>}
      </div>
      <div className="card"><h3>Leases ending in the next {x.boardDays} days</h3>
        {x.ending.length === 0 ? <p className="mute">None.</p> :
          <div className="tw"><table><thead><tr><th>Unit</th><th>Cluster</th><th>Ends</th><th className="num">Days left</th><th>Give notice by</th></tr></thead>
            <tbody>{x.ending.map((e: any) => <tr key={e.unitId + e.end}><td><a href="#" onClick={(ev) => { ev.preventDefault(); onUnit(e.unitId); }}>{e.unit}</a></td><td>{e.cluster}, {e.city}</td>
              <td>{fmtD(e.end)}</td><td className="num">{e.daysLeft}</td><td>{e.noticeBy ? fmtD(e.noticeBy) : '—'}</td></tr>)}</tbody></table></div>}
      </div>
    </>}
  </>;
}

// ---------- Units ----------

function Units({ s, onUnit }: { s: Setup; onUnit: (id: string) => void }) {
  const d = useLoad(() => api('/hst/units'), []);
  const [add, setAdd] = useState(false);
  const [q, setQ] = useState('');
  const rows = (d.data?.units ?? []).filter((u: any) => !q || `${u.name} ${u.cluster} ${u.city} ${u.address}`.toLowerCase().includes(q.toLowerCase()));
  return <div className="card">
    <div className="row sp" style={{ marginBottom: 10, gap: 10, flexWrap: 'wrap' }}>
      <input type="search" placeholder="Find a unit, cluster or city" value={q} onChange={(e) => setQ(e.target.value)} style={{ maxWidth: 320 }} />
      {s.can.setup >= 2 && <button className="btn primary" onClick={() => setAdd(true)}>Add unit</button>}
    </div>
    {!d.data ? <Loading error={d.error} /> : rows.length === 0 ? <p className="mute">No units{q ? ' match' : ' yet'}.</p> :
      <div className="tw"><table><thead><tr><th>Unit</th><th>Cluster</th><th>For</th><th className="num">Beds</th><th className="num">Filled</th><th className="num">Free</th>
        <th className="num">Rent now</th><th className="num">Monthly cost</th><th>Lease ends</th><th>Status</th></tr></thead>
        <tbody>{rows.map((u: any) => <tr key={u.id} style={{ cursor: 'pointer' }} onClick={() => onUnit(u.id)}>
          <td><b>{u.name}</b><div className="mute" style={{ fontSize: 12 }}>{u.kind} · serves {u.nearNames.join(', ')}</div></td><td>{u.cluster}, {u.city}</td><td>{genderPill(u.gender)}</td>
          <td className="num">{u.beds}</td><td className="num">{u.filled}</td><td className="num"><b>{u.free}</b></td><td className="num">{u.leased ? inr(u.rentNow) : '—'}</td>
          <td className="num">{u.leased ? inr(u.monthlyNow) : '—'}</td><td>{u.leased ? (u.leaseEnd ? fmtD(u.leaseEnd) : 'Ongoing') : <Pill k="warn">No lease</Pill>}</td>
          <td><Pill k={u.status === 'Active' ? 'ok' : 'n'}>{u.status}</Pill></td></tr>)}</tbody></table></div>}
    {add && <UnitForm s={s} onClose={() => setAdd(false)} onSaved={(id) => { setAdd(false); d.reload(); onUnit(id); }} />}
  </div>;
}

function UnitForm({ s, unit, onClose, onSaved }: { s: Setup; unit?: any; onClose: () => void; onSaved: (id: string) => void }) {
  const [f, setF] = useState<any>(() => unit ? { ...unit, beds: String(unit.beds), fixed: unit.fixed ?? [] }
    : { name: '', kind: 'Flat', address: '', clusterId: '', near: [], gender: 'Any', beds: '2', landlord: '', landlordPhone: '', status: 'Active', fixed: [], note: '' });
  const [busy, setBusy] = useState(false);
  const locs = s.locations.filter((l) => l.clusterId === f.clusterId && (l.active || f.near.includes(l.id)));
  const set = (k: string, v: any) => setF({ ...f, [k]: v });
  const toggleNear = (id: string) => set('near', f.near.includes(id) ? f.near.filter((x: string) => x !== id) : [...f.near, id]);
  const move = (i: number, d: number) => { const l = [...f.near]; const j = i + d; if (j < 0 || j >= l.length) return; [l[i], l[j]] = [l[j], l[i]]; set('near', l); };
  const save = async () => {
    setBusy(true);
    try {
      const body = { name: f.name, kind: f.kind, address: f.address, clusterId: f.clusterId, near: f.near, gender: f.gender, beds: +f.beds || 0, landlord: f.landlord ?? '',
        landlordPhone: f.landlordPhone ?? '', status: f.status, fixed: f.fixed.filter((x: Item) => x.label.trim()).map((x: Item) => ({ label: x.label, amount: +x.amount || 0 })), note: f.note ?? '' };
      const r = await api(unit ? `/hst/units/${unit.id}` : '/hst/units', { method: unit ? 'PUT' : 'POST', body });
      toast(unit ? 'Unit saved' : 'Unit added'); onSaved(r.id);
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const lname = (id: string) => s.locations.find((l) => l.id === id)?.name ?? id;
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>{unit ? `Edit ${unit.name}` : 'Add a unit'}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form" style={{ marginTop: 12 }}>
      <Field label="Name"><input value={f.name} onChange={(e) => set('name', e.target.value)} placeholder="e.g. Sector 45 flat B-203" /></Field>
      <Field label="Type"><select value={f.kind} onChange={(e) => set('kind', e.target.value)}>{['Flat', 'House', 'PG'].map((k) => <option key={k}>{k}</option>)}</select></Field>
      <Field label="Cluster"><select value={f.clusterId} onChange={(e) => setF({ ...f, clusterId: e.target.value, near: [] })}>
        <option value="">Choose…</option>{s.clusters.filter((c) => c.active || c.id === f.clusterId).map((c) => <option key={c.id} value={c.id}>{c.name}, {c.city}</option>)}</select></Field>
      <Field label="For"><select value={f.gender} onChange={(e) => set('gender', e.target.value)}><option value="Any">Any gender</option><option>Male</option><option>Female</option></select></Field>
      <Field label="Beds"><input type="number" min={1} max={200} value={f.beds} onChange={(e) => set('beds', e.target.value)} /></Field>
      <Field label="Status"><select value={f.status} onChange={(e) => set('status', e.target.value)}><option>Active</option><option>Inactive</option></select></Field>
      <label className="field full"><span>Address</span><input value={f.address ?? ''} onChange={(e) => set('address', e.target.value)} /></label>
      <Field label="Landlord"><input value={f.landlord ?? ''} onChange={(e) => set('landlord', e.target.value)} /></Field>
      <Field label="Landlord mobile"><input value={f.landlordPhone ?? ''} onChange={(e) => set('landlordPhone', e.target.value)} placeholder="10 digits" /></Field>
    </div>
    <h3 style={{ margin: '14px 0 4px', fontSize: 13 }}>Properties it serves, nearest first</h3>
    <p className="mute" style={{ fontSize: 12, margin: '0 0 6px' }}>Suggestions put the unit first for people of these properties. The first one carries the unit's cost on days nobody stays.</p>
    {!f.clusterId ? <p className="mute">Choose the cluster first.</p> : <>
      <ol style={{ margin: '0 0 8px', paddingLeft: 20 }}>{f.near.map((id: string, i: number) => <li key={id}>{lname(id)} {i === 0 && <Pill k="info">Home property</Pill>}{' '}
        <button className="btn sm" onClick={() => move(i, -1)} disabled={i === 0}>↑</button> <button className="btn sm" onClick={() => move(i, 1)} disabled={i === f.near.length - 1}>↓</button>{' '}
        <button className="btn sm" onClick={() => toggleNear(id)}>Remove</button></li>)}</ol>
      <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>{locs.filter((l) => !f.near.includes(l.id)).map((l) => <button key={l.id} className="btn sm" onClick={() => toggleNear(l.id)}>+ {l.name}</button>)}</div>
    </>}
    <h3 style={{ margin: '14px 0 4px', fontSize: 13 }}>Other fixed monthly items</h3>
    <p className="mute" style={{ fontSize: 12, margin: '0 0 6px' }}>Charged every month while a lease is in force (for example a fixed Wi-Fi plan or a cook). Rent and maintenance go on the lease; bills that change go in Month costs.</p>
    {f.fixed.map((x: Item, i: number) => <div className="row" key={i} style={{ gap: 6, marginBottom: 6 }}>
      <input value={x.label} placeholder="Item" onChange={(e) => set('fixed', f.fixed.map((y: Item, j: number) => (j === i ? { ...y, label: e.target.value } : y)))} />
      <input type="number" min={0} value={x.amount} style={{ maxWidth: 140 }} onChange={(e) => set('fixed', f.fixed.map((y: Item, j: number) => (j === i ? { ...y, amount: e.target.value } : y)))} />
      <button className="btn sm" onClick={() => set('fixed', f.fixed.filter((_: Item, j: number) => j !== i))}>Remove</button></div>)}
    <button className="btn sm" onClick={() => set('fixed', [...f.fixed, { label: '', amount: '' }])}>Add an item</button>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={busy} onClick={save}>Save</button></div>
  </Modal>;
}

function UnitDrawer({ id, s, onClose }: { id: string; s: Setup; onClose: () => void }) {
  const [ym, setYm] = useState(ymNow());
  const d = useLoad(() => api(`/hst/units/${id}?ym=${ym}`), [id, ym]);
  const [modal, setModal] = useState<ReactNode>(null);
  const x = d.data;
  const ed = s.can.setup >= 2, allot = s.can.allot >= 2;
  const done = () => { setModal(null); d.reload(); };
  return <Drawer onClose={onClose}>
    {!x ? <Loading error={d.error} /> : <>
      <div className="row sp"><h2 style={{ margin: 0 }}>{x.unit.name} <Pill k={x.unit.status === 'Active' ? 'ok' : 'n'}>{x.unit.status}</Pill></h2><button className="btn sm" onClick={onClose}>Close</button></div>
      <p className="mute" style={{ margin: '4px 0 12px' }}>{x.unit.kind} · {x.unit.cluster}, {x.unit.city} · {genderPill(x.unit.gender)} · {x.unit.beds} beds, {x.unit.filled} filled</p>
      <dl className="kv" style={{ marginBottom: 10 }}>
        <dt>Address</dt><dd>{x.unit.address || '—'}</dd>
        <dt>Serves</dt><dd>{x.unit.nearNames.join(' → ') || '—'}</dd>
        <dt>Landlord</dt><dd>{x.unit.landlord || '—'}{x.unit.landlordPhone ? ` · ${x.unit.landlordPhone}` : ''}</dd>
        <dt>Monthly cost now</dt><dd>{x.unit.leased ? inr(x.unit.monthlyNow) : 'No lease in force'}</dd>
        {x.unit.fixed?.length > 0 && <><dt>Fixed items</dt><dd>{x.unit.fixed.map((f: Item) => `${f.label} ${inr(f.amount)}`).join(', ')}</dd></>}
      </dl>
      {ed && <button className="btn sm" onClick={() => setModal(<UnitForm s={s} unit={x.unit} onClose={() => setModal(null)} onSaved={done} />)}>Edit unit</button>}

      <h3 style={{ marginTop: 18 }}>Leases</h3>
      {ed && <button className="btn sm" onClick={() => setModal(<LeaseForm unitId={id} onClose={() => setModal(null)} onSaved={done} />)}>Add lease</button>}
      {x.leases.length === 0 ? <p className="mute">No lease yet: the unit costs nothing until one is added.</p> : x.leases.map((l: any) => <div key={l.lease.id} className="card" style={{ margin: '8px 0', padding: 12 }}>
        <div className="row sp"><b>{fmtD(l.lease.start)} to {l.lease.end ? fmtD(l.lease.end) : 'ongoing'}</b>{l.current ? <Pill k="ok">In force</Pill> : <Pill k="n">{l.lease.start > x.today ? 'Upcoming' : 'Ended'}</Pill>}</div>
        <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>Rent {inr(l.lease.rent)}{l.current && l.rentNow !== l.lease.rent ? ` (now ${inr(l.rentNow)})` : ''} · maintenance {inr(l.lease.maintenance)} · deposit {inr(l.lease.deposit)} · notice {l.lease.noticeDays} days
          {l.lease.incPct > 0 && <> · {l.lease.incPct}% a year from {fmtD(l.lease.incOn)}{l.nextIncrease ? `, next on ${fmtD(l.nextIncrease)}` : ''}</>}</div>
        {l.lease.note && <div style={{ fontSize: 12.5, marginTop: 4 }}>{l.lease.note}</div>}
        <div className="row" style={{ gap: 6, marginTop: 6, flexWrap: 'wrap' }}>
          {Object.values(l.docs ?? {}).map((doc: any) => <a key={doc.id} href="#" onClick={(e) => { e.preventDefault(); openDoc(doc.id).catch((er) => toast(er.message)); }}><Pill k="info">{doc.doc_key}: {doc.file_name}</Pill></a>)}
          {ed && <><button className="btn sm" onClick={() => setModal(<LeaseForm unitId={id} lease={l.lease} onClose={() => setModal(null)} onSaved={done} />)}>Edit</button>
            <LeaseUpload leaseId={l.lease.id} onDone={d.reload} /></>}
        </div>
      </div>)}

      <h3 style={{ marginTop: 18 }}>Month</h3>
      <div className="row" style={{ gap: 10, alignItems: 'flex-end' }}><Field label="Month"><input type="month" value={ym} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field>
        {ed && <button className="btn sm" onClick={() => setModal(<CostForm unitId={id} ym={ym} cur={x.costs.find((c: any) => c.ym === ym)} onClose={() => setModal(null)} onSaved={done} />)}>Enter bills for {ymLabel(ym)}</button>}</div>
      <p style={{ fontSize: 13 }}>Cost {inr(x.month.cost)} · allocated {inr(x.month.allocated)} · empty-unit cost {inr(x.month.empty)}{x.month.empty ? ` (charged to ${x.month.home || 'no property'})` : ''}</p>
      {x.month.shares.length > 0 && <div className="tw"><table><thead><tr><th>Person</th><th className="num">Days</th><th className="num">Share</th></tr></thead>
        <tbody>{x.month.shares.map((p: any) => <tr key={p.employeeId}><td>{p.name} <span className="mute">{p.employeeId}</span></td><td className="num">{p.days}</td><td className="num">{inr(p.share)}</td></tr>)}</tbody></table></div>}

      <h3 style={{ marginTop: 18 }}>Who stays here</h3>
      {allot && x.unit.status === 'Active' && <button className="btn sm primary" onClick={() => setModal(<AssignModal unitId={id} unitName={x.unit.name} onClose={() => setModal(null)} onDone={done} />)}>Check someone in</button>}
      {x.stays.length === 0 ? <p className="mute">Nobody has stayed here yet.</p> :
        <div className="tw" style={{ marginTop: 8 }}><table><thead><tr><th>Person</th><th className="num">Bed</th><th>Check-in</th><th>Check-out</th><th /></tr></thead>
          <tbody>{x.stays.map((r: any) => <tr key={r.stay.id}><td>{r.name} <span className="mute">{r.stay.employeeId}</span>{r.now && <> <Pill k="ok">Staying</Pill></>}</td><td className="num">{r.stay.bed}</td>
            <td>{fmtD(r.stay.checkIn)}</td><td>{r.stay.checkOut ? fmtD(r.stay.checkOut) : '—'}{r.stay.outReason && <div className="mute" style={{ fontSize: 12 }}>{r.stay.outReason}</div>}</td>
            <td>{allot && !r.stay.checkOut && <button className="btn sm" onClick={() => setModal(<CheckOutModal stay={r.stay} name={r.name} onClose={() => setModal(null)} onDone={done} />)}>Check out</button>}</td></tr>)}</tbody></table></div>}
    </>}
    {modal}
  </Drawer>;
}

function LeaseForm({ unitId, lease, onClose, onSaved }: { unitId: string; lease?: any; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState<any>(() => lease ? { ...lease } : { start: today(), end: '', noticeDays: 30, rent: '', deposit: '', maintenance: '', incPct: '', incOn: '', note: '' });
  const set = (k: string, v: any) => setF({ ...f, [k]: v });
  const save = async () => {
    const body = { start: f.start, end: f.end || '', noticeDays: +f.noticeDays || 0, rent: +f.rent || 0, deposit: +f.deposit || 0, maintenance: +f.maintenance || 0, incPct: +f.incPct || 0,
      incOn: +f.incPct ? f.incOn : '', note: f.note ?? '' };
    try { await api(lease ? `/hst/leases/${lease.id}` : `/hst/units/${unitId}/leases`, { method: lease ? 'PUT' : 'POST', body }); toast('Lease saved'); onSaved(); } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>{lease ? 'Edit lease' : 'Add a lease'}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5 }}>A unit has one lease at a time; earlier leases stay as its history. Leave the end date empty for an ongoing lease.</p>
    <div className="form">
      <Field label="Start"><input type="date" value={f.start} onChange={(e) => set('start', e.target.value)} /></Field>
      <Field label="End (empty = ongoing)"><input type="date" value={f.end ?? ''} onChange={(e) => set('end', e.target.value)} /></Field>
      <Field label="Monthly rent (₹)"><input type="number" min={0} value={f.rent} onChange={(e) => set('rent', e.target.value)} /></Field>
      <Field label="Maintenance a month (₹)"><input type="number" min={0} value={f.maintenance} onChange={(e) => set('maintenance', e.target.value)} /></Field>
      <Field label="Deposit (₹)"><input type="number" min={0} value={f.deposit} onChange={(e) => set('deposit', e.target.value)} /></Field>
      <Field label="Notice period (days)"><input type="number" min={0} max={365} value={f.noticeDays} onChange={(e) => set('noticeDays', e.target.value)} /></Field>
      <Field label="Yearly increase (%)"><input type="number" min={0} max={100} step="0.1" value={f.incPct} onChange={(e) => set('incPct', e.target.value)} /></Field>
      <Field label="First increase on"><input type="date" value={f.incOn ?? ''} disabled={!+f.incPct} onChange={(e) => set('incOn', e.target.value)} /></Field>
      <label className="field full"><span>Note</span><input value={f.note ?? ''} onChange={(e) => set('note', e.target.value)} /></label>
    </div>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Save</button></div>
  </Modal>;
}

function LeaseUpload({ leaseId, onDone }: { leaseId: string; onDone: () => void }) {
  const [key, setKey] = useState('agreement');
  return <span className="row" style={{ gap: 4 }}>
    <select value={key} onChange={(e) => setKey(e.target.value)} aria-label="Document"><option value="agreement">Agreement</option><option value="landlord-id">Landlord ID</option>
      <option value="receipt">Deposit receipt</option><option value="other">Other</option></select>
    <label className="btn sm">Upload<input type="file" accept=".pdf,.jpg,.jpeg,.png" hidden onChange={async (e) => {
      const file = e.target.files?.[0]; e.target.value = '';
      if (!file) return;
      try { await uploadDoc('hst_lease', leaseId, key, file); toast('Document uploaded'); onDone(); } catch (er: any) { toast(er.message); }
    }} /></label>
  </span>;
}

const COSTS: [string, string][] = [['electricity', 'Electricity'], ['water', 'Water'], ['wifi', 'Wi-Fi'], ['housekeeping', 'Housekeeping'], ['other', 'Other']];

function CostForm({ unitId, ym, cur, onClose, onSaved }: { unitId: string; ym: string; cur?: any; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState<Record<string, string>>(() => Object.fromEntries(COSTS.map(([k]) => [k, cur?.items?.[k] != null ? String(cur.items[k]) : ''])));
  const save = async () => {
    try {
      await api(`/hst/units/${unitId}/costs/${ym}`, { method: 'PUT', body: Object.fromEntries(COSTS.map(([k]) => [k, +f[k] || 0])) });
      toast('Bills saved'); onSaved();
    } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Bills for {ymLabel(ym)}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5 }}>Added to the month's rent, maintenance and fixed items, and shared among the people who stayed in the month (the split method in Settings).</p>
    <div className="form">{COSTS.map(([k, l]) => <Field key={k} label={`${l} (₹)`}><input type="number" min={0} value={f[k]} onChange={(e) => setF({ ...f, [k]: e.target.value })} /></Field>)}</div>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Save</button></div>
  </Modal>;
}

// ---------- Check-in and check-out ----------

function CheckOutModal({ stay, name, suggest, why, onClose, onDone }: { stay: any; name: string; suggest?: string; why?: string; onClose: () => void; onDone: () => void }) {
  const [date, setDate] = useState(suggest || today());
  const [reason, setReason] = useState(why || '');
  const save = async () => {
    try { await api(`/hst/stays/${stay.id}/checkout`, { body: { date, reason } }); toast('Checked out'); onDone(); } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Check out {name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5 }}>Checked in on {fmtD(stay.checkIn)}, bed {stay.bed}. The check-out day is not charged and the bed is free for the next person that day.</p>
    <div className="form"><Field label="Check-out date"><input type="date" value={date} onChange={(e) => setDate(e.target.value)} /></Field>
      <Field label="Reason"><input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Moved, left, transferred…" /></Field></div>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Check out</button></div>
  </Modal>;
}

/** Check a person in to a unit (from the unit) or to a suggested unit (from the person). */
function AssignModal({ unitId, unitName, employeeId, onClose, onDone }: { unitId?: string; unitName?: string; employeeId?: string; onClose: () => void; onDone: () => void }) {
  const [who, setWho] = useState(employeeId || '');
  const [date, setDate] = useState(today());
  const [pick, setPick] = useState(unitId || '');
  const [bed, setBed] = useState('0');
  const [note, setNote] = useState('');
  const sg = useLoad(() => (who && !unitId ? api(`/hst/suggest?employeeId=${encodeURIComponent(who)}&date=${date}`) : Promise.resolve(null)), [who, date, unitId]);
  useEffect(() => { if (!unitId) setPick(''); setBed('0'); }, [who, date, unitId]);
  const chosen = sg.data?.units?.find((u: any) => u.id === pick);
  const save = async () => {
    try {
      const r = await api('/hst/stays', { body: { employeeId: who, unitId: pick, bed: +bed, checkIn: date, checkOut: '', note } });
      toast(`Checked in to ${r.unit}, bed ${r.bed}`); onDone();
    } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>{unitName ? `Check someone in to ${unitName}` : 'Assign accommodation'}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form" style={{ marginTop: 10 }}>
      {!employeeId && <label className="field full"><span>Employee</span><PersonPicker value={who} onChange={(id) => setWho(id)} search="&status=Active" placeholder="Find an employee" /></label>}
      <Field label="Check-in date"><input type="date" value={date} onChange={(e) => setDate(e.target.value)} /></Field>
      {unitId && <Field label="Bed"><input type="number" min={0} value={bed} onChange={(e) => setBed(e.target.value)} title="0 = the first free bed" /></Field>}
      <Field label="Note"><input value={note} onChange={(e) => setNote(e.target.value)} /></Field>
    </div>
    {!unitId && who && (!sg.data ? <Loading error={sg.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5, marginTop: 10 }}>{sg.data.employee.name}: {sg.data.employee.location}, {sg.data.employee.cluster} · {sg.data.employee.gender || 'gender not recorded'}.
        Units of the cluster with a free bed from {fmtD(date)}, those serving {sg.data.employee.location} first{sg.data.otherGender ? `; ${sg.data.otherGender} for the other gender not shown` : ''}.</p>
      {sg.data.current && <div className="note warn">Already staying at {sg.data.current.unit} since {fmtD(sg.data.current.stay.checkIn)}. Check them out there first (the check-out day can be the new check-in day).</div>}
      {sg.data.units.length === 0 ? <div className="note">No unit in {sg.data.employee.cluster} has a free bed for them. Add a unit or free a bed, or check them in from a unit's page.</div> :
        <div className="tw"><table><thead><tr><th /><th>Unit</th><th>Why</th><th>For</th><th className="num">Free beds</th><th className="num">Monthly cost</th></tr></thead>
          <tbody>{sg.data.units.map((u: any) => <tr key={u.id} onClick={() => { setPick(u.id); setBed(String(u.freeBeds[0])); }} style={{ cursor: 'pointer' }}>
            <td><input type="radio" checked={pick === u.id} readOnly aria-label={u.name} /></td><td><b>{u.name}</b><div className="mute" style={{ fontSize: 12 }}>{u.address}</div></td><td>{u.why}</td>
            <td>{genderPill(u.gender)}</td><td className="num">{u.freeBeds.length} of {u.beds}</td><td className="num">{u.leased ? inr(u.monthlyNow) : '—'}</td></tr>)}</tbody></table></div>}
      {chosen && <div className="form" style={{ marginTop: 8 }}><Field label="Bed"><select value={bed} onChange={(e) => setBed(e.target.value)}>{chosen.freeBeds.map((b: number) => <option key={b} value={b}>Bed {b}</option>)}</select></Field></div>}
    </>)}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}><button className="btn" onClick={onClose}>Cancel</button>
      <button className="btn primary" disabled={!who || !pick} onClick={save}>Check in</button></div>
  </Modal>;
}

function Attention({ list, onDone }: { list: any[]; onDone: () => void }) {
  const [m, setM] = useState<any>(null);
  if (list.length === 0) return null;
  return <div className="card" style={{ marginBottom: 14 }}>
    <h3>To check out</h3>
    <p className="mute" style={{ fontSize: 12.5 }}>Still checked in, but they have left, are leaving, or moved to a property outside the unit's cluster.</p>
    <div className="tw"><table><thead><tr><th>Person</th><th>Unit</th><th>Why</th><th>Since</th><th /></tr></thead>
      <tbody>{list.map((a) => <tr key={a.stay.id}><td>{a.name} <span className="mute">{a.stay.employeeId}</span></td><td>{a.unit}, bed {a.stay.bed}</td>
        <td><Pill k={a.kind === 'exit' ? 'warn' : 'info'}>{a.kind === 'exit' ? 'Exit' : 'Transfer'}</Pill> {a.why}</td><td>{fmtD(a.stay.checkIn)}</td>
        <td><button className="btn sm" onClick={() => setM(a)}>Check out</button></td></tr>)}</tbody></table></div>
    {m && <CheckOutModal stay={m.stay} name={m.name} suggest={m.suggest} why={m.why} onClose={() => setM(null)} onDone={() => { setM(null); onDone(); }} />}
  </div>;
}

function Stays({ s }: { s: Setup }) {
  const att = useLoad(() => api('/hst/attention'), []);
  const [assign, setAssign] = useState(false);
  return <>
    {att.data ? <Attention list={att.data.list} onDone={att.reload} /> : <Loading error={att.error} />}
    <div className="card">
      <div className="row sp"><h3 style={{ margin: 0 }}>Assign accommodation</h3>{s.can.allot >= 2 && <button className="btn primary" onClick={() => setAssign(true)}>Assign accommodation</button>}</div>
      <p className="mute" style={{ fontSize: 12.5 }}>Pick an employee: the units of their cluster with a free bed are listed, the ones serving their property first, matching gender. A bed is never booked twice and a person never has two stays at once. Open a unit to check someone in there directly or to see its full history.</p>
    </div>
    {assign && <AssignModal onClose={() => setAssign(false)} onDone={() => { setAssign(false); att.reload(); }} />}
  </>;
}

// ---------- Month split and settings ----------

function Split() {
  const [ym, setYm] = useState(ymNow());
  const d = useLoad(() => api(`/hst/split?ym=${ym}`), [ym]);
  const x = d.data;
  return <div className="card">
    <div className="row" style={{ gap: 10, alignItems: 'flex-end' }}><Field label="Month"><input type="month" value={ym} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field></div>
    <p className="mute" style={{ fontSize: 12.5 }}>{x?.method === 'days'
      ? <>Split by days stayed: each day's cost (the month cost ÷ {x?.days ?? 'the'} days) is divided equally among the people staying that day. A day with nobody in the unit is empty-unit cost for its home property.</>
      : <>Equal per person: the unit's month cost is divided equally among everyone who stayed in it at any time in the month. Only a month with nobody in the unit is empty-unit cost for its home property.</>}
      {' '}This is the company's own cost figure: it is not part of payroll or the payslip.</p>
    {!x ? <Loading error={d.error} /> : <>
      <div className="tw"><table><thead><tr><th>Unit</th><th>Cluster</th><th className="num">Cost</th><th>People (days: share)</th><th className="num">Empty-unit cost</th></tr></thead>
        <tbody>{x.units.map((u: any) => <tr key={u.unitId}><td>{u.unit}</td><td>{u.cluster}, {u.city}</td><td className="num">{inr(u.cost)}</td>
          <td>{u.people.length === 0 ? <span className="mute">Nobody</span> : u.people.map((p: any) => <div key={p.employeeId}>{p.name} <span className="mute">{p.employeeId}</span> · {p.days} days: {inr(p.share)}</div>)}</td>
          <td className="num">{u.empty ? `${inr(u.empty)} (${u.home || '—'})` : '—'}</td></tr>)}</tbody></table></div>
      {x.empty.length > 0 && <><h3 style={{ marginTop: 14 }}>Empty-unit cost by property</h3>
        <div className="tw"><table><tbody>{x.empty.map((e: any) => <tr key={e.locationId}><td>{e.location || 'No property set'}</td><td className="num">{inr(e.empty)}</td></tr>)}</tbody></table></div></>}
    </>}
  </div>;
}

function Settings({ s, onSaved }: { s: Setup; onSaved: () => void }) {
  const [f, setF] = useState(() => ({ ...s.settings }));
  const ed = s.can.setup >= 2;
  const save = async () => {
    try { await api('/hst/settings', { method: 'PUT', body: { byDays: !!f.byDays, leaseDays: +f.leaseDays, incDays: +f.incDays, boardDays: +f.boardDays } }); toast('Settings saved'); onSaved(); } catch (e: any) { toast(e.message); }
  };
  return <div className="card">
    <h3>Cost split</h3>
    <label className="row" style={{ gap: 8, fontSize: 13.5 }}><input type="checkbox" checked={!!f.byDays} disabled={!ed} onChange={(e) => setF({ ...f, byDays: e.target.checked })} /> Split by days stayed</label>
    <p className="mute" style={{ fontSize: 12.5 }}>Off (the default): a unit's month cost is divided equally among everyone who stayed in it at any time in the month (₹12,000 with one person = ₹12,000; two = ₹6,000 each; four = ₹3,000 each), and only a month with nobody in the unit is empty-unit cost. On: each day's cost is divided among the people staying that day, and a day with nobody is empty-unit cost. Either way it is the company's own cost figure and never part of payroll.</p>
    <h3 style={{ marginTop: 16 }}>Reminders</h3>
    <div className="form">
      <Field label="Remind before a lease ends (days)"><input type="number" min={1} max={365} value={f.leaseDays} disabled={!ed} onChange={(e) => setF({ ...f, leaseDays: e.target.value as any })} /></Field>
      <Field label="Remind before a rent increase (days)"><input type="number" min={1} max={365} value={f.incDays} disabled={!ed} onChange={(e) => setF({ ...f, incDays: e.target.value as any })} /></Field>
      <Field label="Board: leases ending within (days)"><input type="number" min={1} max={365} value={f.boardDays} disabled={!ed} onChange={(e) => setF({ ...f, boardDays: e.target.value as any })} /></Field>
    </div>
    <p className="mute" style={{ fontSize: 12, marginTop: 8 }}>Reminders go to the bell of HR Admins and of roles that can edit accommodation: a lease ending, the last week to give notice, a rent increase, and people still checked in after they left or moved.</p>
    {ed && <div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={save}>Save settings</button></div>}
  </div>;
}

// ---------- On the employee file ----------

/** The employee file's Accommodation tab: the person's stays, what needs doing, and "Assign accommodation". */
export function EmployeeAccommodation({ id, name }: { id: string; name: string }) {
  const d = useLoad(() => api(`/hst/stays?employeeId=${encodeURIComponent(id)}`), [id]);
  const [m, setM] = useState<ReactNode>(null);
  const x = d.data;
  const done = () => { setM(null); d.reload(); };
  if (!x) return <Loading error={d.error} />;
  return <>
    <Attention list={x.attention} onDone={d.reload} />
    <div className="card">
      <div className="row sp"><h3 style={{ margin: 0 }}>Staff accommodation</h3>
        {x.canEdit && <button className="btn primary" onClick={() => setM(<AssignModal employeeId={id} onClose={() => setM(null)} onDone={done} />)}>Assign accommodation</button>}</div>
      <p className="mute" style={{ fontSize: 12.5 }}>Where {name} stays. The unit's cost is the company's own cost: it is not part of {name}'s pay, payslip or CTC.</p>
      {x.stays.length === 0 ? <p className="mute">No stays.</p> :
        <div className="tw"><table><thead><tr><th>Unit</th><th className="num">Bed</th><th>Check-in</th><th>Check-out</th><th /></tr></thead>
          <tbody>{x.stays.map((r: any) => <tr key={r.stay.id}><td>{r.unit}{r.now && <> <Pill k="ok">Staying</Pill></>}</td><td className="num">{r.stay.bed}</td><td>{fmtD(r.stay.checkIn)}</td>
            <td>{r.stay.checkOut ? fmtD(r.stay.checkOut) : '—'}</td>
            <td>{x.canEdit && !r.stay.checkOut && <button className="btn sm" onClick={() => setM(<CheckOutModal stay={r.stay} name={name} onClose={() => setM(null)} onDone={done} />)}>Check out</button>}</td></tr>)}</tbody></table></div>}
    </div>
    {m}
  </>;
}
