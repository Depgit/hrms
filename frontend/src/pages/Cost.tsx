import { useState, type ReactNode } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, inr } from '../api';
import { Bars, Field, Loading, Modal, Pill, Tabs, download, toast, useLoad } from '../components/ui';
import { BulkModal } from './Bulk';
import { useAllIn } from '../components/AllIn';

/** Cost and budget console (workforce plan, module 6; backend cost.go). What the company spends in a month for the
 *  whole company or any state, city, cluster or property, with a drill-down to each person: salary paid (minimum-wage
 *  top-ups left out), company contributions and accommodation, against the budgets. Approved and published months are
 *  actual (frozen when published); the open month and later are projected from the salary structures. */

type Node = {
  kind: 'company' | 'state' | 'city' | 'cluster' | 'property'; id: string; name: string; active: boolean; people: number;
  salary: number; reimb?: number; topUp: number; pf: number; admin: number; esi: number; lwf: number; grat: number; plEnc?: number; bonus: number; med: number; contrib: number;
  accomShare: number; accomEmpty: number; accom: number; total: number; perHead: number;
  budget: number; budgetOwn?: number; hcBudget: number; hcOwn?: number; diff: number; pct: number; over: boolean; children?: Node[];
};
type Meta = { ym: string; openYm: string; mode: 'actual' | 'projected' | 'none'; status: string; frozen: boolean; frozenAt?: string; frozenBy?: string };
type Resp = { meta: Meta; tree: Node; canBudget: boolean; canPeople: boolean; scopeAll: boolean };

const KIND: Record<string, string> = { company: 'Company', state: 'State', city: 'City', cluster: 'Cluster', property: 'Property' };
const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });
const keyOf = (n: Node) => `${n.kind}:${n.id}`;
const red = { color: 'var(--bad)' };

function Basis({ m }: { m: Meta }) {
  if (m.frozen) return <Pill k="info">Actual · frozen on {m.frozenAt ? new Date(m.frozenAt).toLocaleDateString('en-IN', { day: '2-digit', month: 'short', year: 'numeric' }) : ''}</Pill>;
  if (m.mode === 'projected') return <Pill k="warn">Projected</Pill>;
  if (m.mode === 'actual') return <Pill k="ok">Actual ({m.status.toLowerCase()})</Pill>;
  return <Pill k="n">No approved payroll</Pill>;
}

export default function Cost() {
  const [qs, setQs] = useSearchParams();
  const tab = qs.get('tab') === 'seats' ? 'seats' : 'cost';
  return <>
    <Tabs tabs={[['cost', 'Cost by place'], ['seats', 'Headcount plan']]} value={tab} onChange={(t) => setQs({ tab: t })} />
    {tab === 'cost' ? <CostView /> : <Seats />}
  </>;
}

// ---------- Cost by place ----------

function CostView() {
  const [ym, setYm] = useState('');
  const d = useLoad(() => api<Resp>(`/cost${ym ? `?ym=${ym}` : ''}`), [ym]);
  const [open, setOpen] = useState<Record<string, boolean>>({ 'company:': true });
  const [sel, setSel] = useState<Node | null>(null);
  const [people, setPeople] = useState<Node | null>(null);
  const [edit, setEdit] = useState<Node | null>(null);
  const x = d.data;
  const month = ym || x?.meta.ym || '';
  const toggle = (n: Node) => setOpen({ ...open, [keyOf(n)]: !open[keyOf(n)] });
  const dl = () => download(`/cost/export.xlsx?ym=${month}`, `cost-${month}.xlsx`).catch((e) => toast(e.message));
  const [bulk, setBulk] = useState(false); // budgets uploaded here, in a dialog (owner, 24 Nov 2026)
  const allIn = useAllIn(); // the all-inclusive gross from a month (server allin.go)

  const rows: ReactNode[] = [];
  const walk = (n: Node, depth: number) => {
    const kids = n.children ?? [];
    const isOpen = !!open[keyOf(n)];
    const editable = x!.canBudget && !x!.meta.frozen && (n.kind === 'property' ? n.id !== '' : n.kind === 'cluster');
    rows.push(<tr key={keyOf(n)} style={{ opacity: n.active ? 1 : 0.6, background: sel && keyOf(sel) === keyOf(n) ? 'var(--hover, rgba(0,0,0,.03))' : undefined }}>
      <td style={{ paddingLeft: 8 + depth * 18 }}>
        {kids.length > 0 ? <button className="btn sm" style={{ minWidth: 24, padding: '0 6px', marginRight: 6 }} onClick={() => toggle(n)} aria-label={isOpen ? 'Collapse' : 'Expand'}>{isOpen ? '−' : '+'}</button> : <span style={{ display: 'inline-block', width: 30 }} />}
        <b style={{ fontWeight: n.kind === 'property' ? 400 : 600 }}>{n.name}</b> <span className="mute" style={{ fontSize: 11.5 }}>{KIND[n.kind]}{!n.active ? ' · inactive' : ''}</span>
      </td>
      <td className="num">{n.people}</td>
      <td className="num" title={[n.topUp ? `Minimum-wage top-ups (meal and accommodation deductions) left out: ${inr(n.topUp)}` : '', n.reimb ? `Plus reimbursement outside the gross: ${inr(n.reimb)}` : ''].filter(Boolean).join('. ') || undefined}>{inr(n.salary + (n.reimb || 0))}{n.topUp ? <sup className="mute">*</sup> : null}</td>
      <td className="num" title={`Employer PF ${inr(n.pf)} · EPFO admin and EDLI ${inr(n.admin)} · ESI ${inr(n.esi)} · LWF ${inr(n.lwf)} · Gratuity ${inr(n.grat)}${n.plEnc ? ` · PL encashment ${inr(n.plEnc)}` : ''} · Bonus ${inr(n.bonus)} · Medical insurance ${inr(n.med)}`}>{inr(n.contrib)}</td>
      <td className="num" title={`Staff shares ${inr(n.accomShare)} · empty units ${inr(n.accomEmpty)}`}>{inr(n.accom)}</td>
      <td className="num"><b>{inr(n.total)}</b></td>
      <td className="num">{n.people ? inr(n.perHead) : '—'}</td>
      <td className="num">{n.budget > 0 ? inr(n.budget) : <span className="mute">—</span>}{n.kind === 'cluster' && n.budgetOwn == null && n.budget > 0 && <div className="mute" style={{ fontSize: 11 }}>total of properties</div>}</td>
      <td className="num" style={n.over ? red : undefined}>{n.budget > 0 ? inr(n.diff) : ''}</td>
      <td className="num" style={n.over ? red : undefined}>{n.budget > 0 ? <>{n.pct}%{n.over && <> <Pill k="bad">Over</Pill></>}</> : ''}</td>
      <td style={{ whiteSpace: 'nowrap' }}>
        <button className="btn sm" onClick={() => setSel(n)}>Trend</button>{' '}
        {x!.canPeople && <button className="btn sm" onClick={() => setPeople(n)}>People</button>}{' '}
        {editable && <button className="btn sm" onClick={() => setEdit(n)}>Budget</button>}
      </td>
    </tr>);
    if (isOpen) kids.forEach((k) => walk(k, depth + 1));
  };
  if (x) walk(x.tree, 0);

  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 10 }}>
      <div className="row" style={{ gap: 10, alignItems: 'flex-end', flexWrap: 'wrap' }}>
        <Field label="Month"><input type="month" value={month} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field>
        {x && <Basis m={x.meta} />}
        {x && <span className="mute" style={{ fontSize: 12.5 }}>Open payroll month: {ymLabel(x.meta.openYm)}</span>}
      </div>
      <div className="row" style={{ gap: 8 }}>
        {/* Budgets for many places at once: the Bulk update "Budgets" template (Level, Name, Headcount Budget, Monthly Cost Budget (CTC)). */}
        {x?.canBudget && <button className="btn" onClick={() => setBulk(true)}>Bulk upload budgets</button>}
        {bulk && <BulkModal tpl="bud" title="Bulk upload: budgets" onClose={() => setBulk(false)} onApplied={() => d.reload()} />}
        <button className="btn" onClick={dl} disabled={!month}>Download Excel</button>
      </div>
    </div>
      {x?.meta.mode === 'projected' && <div className="note warn" style={{ marginTop: 10 }}>Projected: everyone paid in {ymLabel(month)} on their current salary structure for the full month, with the pay lines already entered. Changes until the run is approved.</div>}
      {x?.meta.frozen && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Frozen when payroll was published{x.meta.frozenBy ? ` (${x.meta.frozenBy})` : ''}: these figures and budgets no longer change.</p>}
      {x?.meta.mode === 'none' && <div className="note" style={{ marginTop: 10 }}>No approved payroll run for {ymLabel(month)}.</div>}
      {allIn && month >= allIn && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>From {ymLabel(allIn)} pay is on the all-inclusive gross: the statutory bonus (gross at most the bonus ceiling) is inside the gross, and the employer PF, ESI, EDLI and LWF are deducted from the employee (still remitted), so company contributions are only EPFO admin, gratuity, PL encashment and medical insurance; variable pay and reimbursement are on top.</p>}
    </div>
    {!x ? <Loading error={d.error} /> : <div className="card" style={{ marginBottom: 14 }}>
      <div className="tw"><table><thead><tr><th>Place</th><th className="num">People</th><th className="num">Salary paid</th><th className="num">Company contributions</th>
        <th className="num">Accommodation</th><th className="num">Total cost</th><th className="num">Per head</th><th className="num">Budget</th><th className="num">Difference</th><th className="num">% used</th><th></th></tr></thead>
        <tbody>{rows}</tbody></table></div>
      <p className="mute" style={{ fontSize: 12, marginBottom: 0 }}>Salary paid is earnings (fixed, variable, incentives, arrears and claims) without the bonus, which is with the company contributions (PF, EPFO admin and EDLI, ESI, LWF, gratuity, bonus, medical insurance).
        {x.tree.topUp > 0 && <> * Minimum-wage top-ups (meal and accommodation deductions) are not company cost and are left out: {inr(x.tree.topUp)}.</>} Accommodation is each person’s share plus the cost of empty units, on the property the unit serves. Hover a figure for its parts.</p>
    </div>}
    {sel && <Trend node={sel} ym={month} onClose={() => setSel(null)} />}
    {people && <People node={people} ym={month} onClose={() => setPeople(null)} />}
    {edit && <BudgetEdit node={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); d.reload(); }} />}
  </>;
}

function Trend({ node, ym, onClose }: { node: Node; ym: string; onClose: () => void }) {
  const d = useLoad(() => api<{ months: any[] }>(`/cost/trend?ym=${ym}&kind=${node.kind}&id=${encodeURIComponent(node.id)}`), [ym, node.kind, node.id]);
  const m = d.data?.months ?? [];
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Last six months: {node.name}</h3><button className="btn sm" onClick={onClose}>Close</button></div>
    {!d.data ? <Loading error={d.error} /> : <>
      <div style={{ margin: '12px 0' }}><Bars rows={m.map((x) => ({ label: ymLabel(x.ym).replace(/ \d{4}$/, '') + ' ' + x.ym.slice(2, 4), value: Math.round(x.total) }))} /></div>
      <div className="tw"><table><thead><tr><th>Month</th><th>Basis</th><th className="num">People</th><th className="num">Salary paid</th><th className="num">Contributions</th><th className="num">Accommodation</th><th className="num">Total cost</th><th className="num">Per head</th><th className="num">Budget</th></tr></thead>
        <tbody>{m.map((x) => <tr key={x.ym}><td>{ymLabel(x.ym)}</td><td>{x.frozen ? 'Frozen' : x.mode === 'projected' ? 'Projected' : x.mode === 'actual' ? 'Actual' : '—'}</td>
          <td className="num">{x.people}</td><td className="num">{inr(x.salary + (x.reimb || 0))}</td><td className="num">{inr(x.contrib)}</td><td className="num">{inr(x.accom)}</td>
          <td className="num" style={x.over ? red : undefined}><b>{inr(x.total)}</b></td><td className="num">{x.people ? inr(x.perHead) : '—'}</td><td className="num">{x.budget > 0 ? inr(x.budget) : '—'}</td></tr>)}</tbody></table></div>
    </>}
  </div>;
}

function People({ node, ym, onClose }: { node: Node; ym: string; onClose: () => void }) {
  const d = useLoad(() => api<{ people: any[] }>(`/cost/people?ym=${ym}&kind=${node.kind}&id=${encodeURIComponent(node.id)}`), [ym, node.kind, node.id]);
  return <Modal wide onClose={onClose}>
    <h2 style={{ marginTop: 0 }}>{node.name}: cost per person, {ymLabel(ym)}</h2>
    {!d.data ? <Loading error={d.error} /> : <div className="tw" style={{ maxHeight: 480, overflow: 'auto' }}><table><thead><tr><th>Employee</th><th>Property</th><th className="num">Salary paid</th>
      <th className="num">Contributions</th><th className="num">Accommodation</th><th className="num">Total cost</th></tr></thead>
      <tbody>{d.data.people.length === 0 && <tr><td colSpan={6} className="mute">No one.</td></tr>}
        {d.data.people.map((p) => <tr key={p.employeeId}><td>{p.name} <span className="mono mute">{p.employeeId}</span>{!p.paid && <div className="mute" style={{ fontSize: 12 }}>Accommodation only (not on this month’s payroll)</div>}</td>
          <td>{p.location || '—'}</td>
          <td className="num" title={[p.topUp ? `Minimum-wage top-up left out: ${inr(p.topUp)}` : '', p.reimb ? `Salary ${inr(p.salary)} + reimbursement outside the gross ${inr(p.reimb)}` : ''].filter(Boolean).join('. ') || undefined}>{inr(p.salary + (p.reimb || 0))}{p.topUp ? <sup className="mute">*</sup> : null}</td>
          <td className="num" title={`PF ${inr(p.pf)} · admin/EDLI ${inr(p.admin)} · ESI ${inr(p.esi)} · LWF ${inr(p.lwf)} · gratuity ${inr(p.grat)} · bonus ${inr(p.bonus)} · medical ${inr(p.med)}`}>{inr(p.contrib)}</td>
          <td className="num">{inr(p.accom)}</td><td className="num"><b>{inr(p.total)}</b></td></tr>)}</tbody></table></div>}
    <div className="row" style={{ justifyContent: 'flex-end', marginTop: 12 }}><button className="btn" onClick={onClose}>Close</button></div>
  </Modal>;
}

function BudgetEdit({ node, onClose, onSaved }: { node: Node; onClose: () => void; onSaved: () => void }) {
  const cl = node.kind === 'cluster';
  const [cost, setCost] = useState(cl ? (node.budgetOwn != null ? String(node.budgetOwn) : '') : String(node.budget || 0));
  const [hc, setHc] = useState(cl ? (node.hcOwn != null ? String(node.hcOwn) : '') : String(node.hcBudget || 0));
  const [busy, setBusy] = useState(false);
  const save = async () => {
    const body: any = {};
    if (cost.trim() === '') { if (cl) body.clearCost = true; } else body.cost = +cost;
    if (hc.trim() === '') { if (cl) body.clearHeadcount = true; } else body.headcount = Math.round(+hc);
    setBusy(true);
    try { await api(`/cost/budget/${cl ? 'cluster' : 'location'}/${node.id}`, { method: 'PUT', body }); toast('Budget saved'); onSaved(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  return <Modal onClose={onClose}>
    <h2 style={{ marginTop: 0 }}>Budget: {node.name}</h2>
    <p className="ink2" style={{ fontSize: 13 }}>{cl ? 'Leave a box empty to use the total of the cluster’s properties.' : 'The property’s monthly cost budget and seats (the same figures as Compensation › Budgets). 0 means no budget.'}</p>
    <div className="grid g2">
      <Field label="Monthly cost budget (₹)"><input type="number" min={0} value={cost} placeholder={cl ? 'Total of properties' : ''} onChange={(e) => setCost(e.target.value)} /></Field>
      <Field label="Headcount budget (seats)"><input type="number" min={0} value={hc} placeholder={cl ? 'Total of properties' : ''} onChange={(e) => setHc(e.target.value)} /></Field>
    </div>
    <div className="row" style={{ justifyContent: 'flex-end', gap: 8, marginTop: 14 }}>
      <button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={busy} onClick={save}>{busy ? 'Saving…' : 'Save'}</button>
    </div>
  </Modal>;
}

// ---------- Headcount plan ----------

function Seats() {
  const d = useLoad(() => api<{ clusters: any[] }>('/cost/headcount'), []);
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const cell = (p: any) => <>
    <td className="num">{p.seats || <span className="mute">—</span>}</td><td className="num">{p.inPost}</td><td className="num">{p.inFlight}</td><td className="num">{p.awaiting}</td>
    <td className="num" style={p.over ? red : undefined}>{p.free == null ? '' : p.free}{p.over && <> <Pill k="bad">Over</Pill></>}</td>
  </>;
  return <div className="card">
    <p className="mute" style={{ fontSize: 12.5, marginTop: 0 }}>Seats budgeted against people in post (active, Joining IDs included), offers in flight (accepted and not joined yet) and offers still in approval. The offer budget check counts the same numbers. A cluster’s seats are its own figure when set, else the total of its properties.</p>
    {!d.data ? <Loading error={d.error} /> : <div className="tw"><table><thead><tr><th>Cluster / property</th><th className="num">Seats</th><th className="num">In post</th><th className="num">Offers in flight</th><th className="num">In approval</th><th className="num">Free</th></tr></thead>
      <tbody>{d.data.clusters.length === 0 && <tr><td colSpan={6} className="mute">No properties in your access.</td></tr>}
        {d.data.clusters.flatMap((c) => [
          <tr key={c.id}><td><button className="btn sm" style={{ minWidth: 24, padding: '0 6px', marginRight: 6 }} onClick={() => setOpen({ ...open, [c.id]: !open[c.id] })}>{open[c.id] ? '−' : '+'}</button>
            <b>{c.name}</b> <span className="mute" style={{ fontSize: 12 }}>{c.city}{c.own ? ' · own seat budget' : ''}</span></td>{cell(c)}</tr>,
          ...(open[c.id] ? c.properties.map((p: any) => <tr key={c.id + p.id}><td style={{ paddingLeft: 44 }}>{p.name}</td>{cell(p)}</tr>) : []),
        ])}</tbody></table></div>}
  </div>;
}
