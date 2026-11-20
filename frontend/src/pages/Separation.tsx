import { useState, type ReactNode } from 'react';
import { addDays, api, fmtD, inr, today, ymd } from '../api';
import { useAuth } from '../auth';
import { Loading, Modal, Pill, toast, usePage, useLoad } from '../components/ui';
import PersonPicker, { type Person } from '../components/PersonPicker';
import { printSlip } from '../components/Payslip';
import { ymLabel } from './PayrollRun';

type Tone = 'ok' | 'n' | 'info' | 'warn' | 'bad';
/** Stage → the prototype's label and pill tone (prototype STG). "Ready to close" is the app's explicit close step. */
const STG: Record<string, [string, Tone]> = {
  'Show-cause': ['Show-cause', 'warn'], Approval: ['Approval', 'info'], Notice: ['Notice period', 'info'], Clearance: ['Clearance', 'warn'],
  'Full and final': ['Full and final', 'warn'], Letters: ['Letters', 'warn'], 'Ready to close': ['Ready to close', 'warn'], Closed: ['Closed', 'ok'],
  Rejected: ['Rejected', 'n'], Withdrawn: ['Withdrawn', 'n'], Cancelled: ['Cancelled', 'n'],
};
const stgPill = (s: string) => { const [l, k] = STG[s] ?? [s, 'n']; return <Pill k={k}>{l}</Pill>; };
const ENDED = ['Rejected', 'Withdrawn', 'Cancelled'];
const isOpen = (s: string) => !ENDED.includes(s) && s !== 'Closed';
const GROUP: Record<string, string> = { 'Show-cause': 'showcause', Approval: 'approval', Notice: 'notice', Clearance: 'clearance', 'Full and final': 'fnf', Letters: 'letters', 'Ready to close': 'letters', Closed: 'closed' };
const grpOf = (s: string) => (ENDED.includes(s) ? 'ended' : GROUP[s] ?? '');
const FILTERS: [string, string][] = [['open', 'Open'], ['showcause', 'Show-cause'], ['approval', 'Approval'], ['notice', 'Notice period'], ['clearance', 'Clearance'],
  ['fnf', 'Full and final'], ['letters', 'Letters'], ['closed', 'Closed'], ['ended', 'Rejected or withdrawn']];
const STEPS: [string, string][] = [['start', 'Started'], ['approval', 'Approval'], ['notice', 'Notice'], ['clearance', 'Clearance'], ['fnf', 'Full and final'], ['letters', 'Letters'], ['closed', 'Closed']];
const EXIT_TYPES = ['Resignation', 'Termination', 'Absconding', 'Contract end', 'Retirement', 'Death'];
const SEP_REASON: Record<string, string> = { Absconding: 'Absconded', 'Contract end': 'Contract ended', Retirement: 'Retired', Death: 'Death or permanent disability' };
const EX_SEE = ['HR Admin', 'HRBP', 'HRBP Corporate', 'CHRO'];
const OWNERS = ['Reporting manager', 'HRBP', 'Finance'];
const REHIRE: [string, string][] = [['', 'Not decided'], ['Eligible', 'Yes'], ['Only with CHRO approval', 'Only with CHRO approval'], ['Not eligible', 'No']];
const rehireLabel = (v: string) => REHIRE.find(([k]) => k === v)?.[1] ?? v;
const dayDiff = (a: string, b: string) => Math.round((new Date(b + 'T00:00:00').getTime() - new Date(a + 'T00:00:00').getTime()) / 864e5);
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;
/** A sortable key for the dates the API returns: YYYY-MM-DD, ISO timestamps and "04 Nov 2026". */
const sortKey = (s?: string | null) => (!s ? '' : /^\d{4}-\d{2}-\d{2}/.test(s) ? s : (() => { const d = new Date(s); return isNaN(+d) ? s : ymd(d); })());
/** A history time as the prototype writes it (nowS): "04 Nov, 10:30 am". */
const atS = (s: string) => { const d = new Date(s); return isNaN(+d) ? s : d.toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }); };

/** Separation (spec 18): every exit as one tracked case. Prototype vSep. */
export default function Separation() {
  const { me } = useAuth();
  const role = me?.ctx.role ?? '';
  const [tab, setTab] = useState<'cases' | 'set'>('cases');
  const t = tab === 'set' && role === 'HR Admin' ? 'set' : 'cases';
  const d = useLoad(() => api('/separations'), [role]);
  const [f, setF] = useState({ st: 'open', ty: '', q: '' });
  const [open, setOpen] = useState('');
  const [start, setStart] = useState(false);
  const all: any[] = d.data?.cases ?? [];
  const q = f.q.trim().toLowerCase();
  const rows = all.filter((c) => (f.st === 'open' ? isOpen(c.stage) : f.st === '' || grpOf(c.stage) === f.st) && (!f.ty || c.type === f.ty)
    && (!q || c.name.toLowerCase().includes(q) || c.employeeId.toLowerCase().includes(q)))
    .sort((a, b) => ((a.lwd || '') < (b.lwd || '') ? -1 : 1));
  const pg = usePage(rows, 100);
  const tabs = <div className="tabs">
    <button className={`tab ${t === 'cases' ? 'on' : ''}`} onClick={() => setTab('cases')}>Exits</button>
    {role === 'HR Admin' && <button className={`tab ${t === 'set' ? 'on' : ''}`} onClick={() => setTab('set')}>Settings</button>}
  </div>;
  if (t === 'set') return <>{tabs}<SepSettings /></>;
  if (!d.data) return <>{tabs}<Loading error={d.error} /></>;
  const x = d.data;
  const openC = all.filter((c) => isOpen(c.stage));
  // The server flags each case waiting on the caller: approver now, an open part of the clearance they sign, or HR's step.
  const mine = openC.filter((c) => c.waitingForMe).length;
  const count = (k: string) => (k === 'open' ? openC.length : all.filter((c) => grpOf(c.stage) === k).length);
  return (
    <>
      {tabs}
      <div className="row sp" style={{ marginBottom: 12, flexWrap: 'wrap', gap: 8 }}>
        <div className="mute" style={{ fontSize: 13 }}>{openC.length} open exit{openC.length === 1 ? '' : 's'}{mine > 0 && <> · <b style={{ color: 'var(--ink,#111)' }}>{mine} waiting for you</b></>}</div>
        {x.canStart && <button className="btn primary" onClick={() => setStart(true)}>Start a separation</button>}
      </div>
      <div className="row" style={{ gap: 6, flexWrap: 'wrap', marginBottom: 12 }}>
        {FILTERS.map(([k, l]) => <button key={k} className={`btn sm ${f.st === k ? 'primary' : ''}`} onClick={() => setF({ ...f, st: k })}>{l} <span className="mute">{count(k)}</span></button>)}
      </div>
      <div className="card">
        <div className="row" style={{ gap: 8, marginBottom: 10, flexWrap: 'wrap' }}>
          <input type="search" placeholder="Search name or code" value={f.q} onChange={(e) => setF({ ...f, q: e.target.value })} style={{ maxWidth: 260 }} />
          <select value={f.ty} onChange={(e) => setF({ ...f, ty: e.target.value })}><option value="">All types</option>{(x.types ?? EXIT_TYPES).map((y: string) => <option key={y}>{y}</option>)}</select>
        </div>
        <div className="tw"><table><thead><tr><th>Employee</th><th>Type</th><th>Started</th><th>Last working day</th><th>Stage</th><th>Next step</th><th></th></tr></thead><tbody>
          {(pg.rows ?? []).map((c: any) => <tr key={c.id}>
            <td><b>{c.name}</b> <span className="mono mute">{c.employeeId}</span><div className="mute" style={{ fontSize: 12 }}>{c.designation || ''} · {c.location}</div></td>
            <td>{c.type}</td><td>{fmtD(c.givenOn)}</td><td>{c.lwd ? fmtD(c.lwd) : '—'}</td><td>{stgPill(c.stage)}</td>
            <td className="ink2" style={{ fontSize: 12.5, maxWidth: 280 }}>{c.next}</td>
            <td><button className="btn sm" onClick={() => setOpen(c.id)}>Open</button></td></tr>)}
          {!rows.length && <tr><td colSpan={7} className="mute">No exits match</td></tr>}
        </tbody></table></div>
        {pg.pager}
      </div>
      {open && <CaseModal id={open} onClose={() => { setOpen(''); d.reload(); }} />}
      {start && <StartModal x={x} onClose={() => setStart(false)} onDone={(id) => { setStart(false); d.reload(); setOpen(id); }} />}
    </>
  );
}

/** Settings (HR Admin): absconding rules, exit interview types, clearance checklist. Prototype vSepSet. */
function SepSettings() {
  const d = useLoad(() => api('/separations/settings'), []);
  const [f, setF] = useState<{ absDays: string; absLetters: string; absWait: string; exitReq: string[] } | null>(null);
  const [names, setNames] = useState<Record<string, string>>({});
  const [adds, setAdds] = useState<Record<string, string>>({});
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, items: any[] = x.items ?? [];
  const v = f ?? { absDays: String(x.absDays), absLetters: String(x.absLetters), absWait: String(x.absWait), exitReq: x.exitReq as string[] };
  const save = async () => {
    const a = +v.absDays, l = +v.absLetters, w = +v.absWait;
    if (!(a >= 1 && l >= 1 && l <= 5 && w >= 0)) { toast('Check the numbers'); return; }
    try { await api('/separations/settings', { method: 'PUT', body: { absDays: a, absLetters: l, absWait: w, exitReq: v.exitReq } }); setF(null); d.reload(); toast('Saved'); } catch (er: any) { toast(er.message); }
  };
  const item = async (it: any, body: any, msg: string) => {
    try { await api(`/separations/clearance-items/${it.id}`, { method: 'PUT', body }); d.reload(); toast(msg); } catch (er: any) { toast(er.message); }
  };
  const rename = (it: any) => { const n = (names[it.id] ?? it.name).trim(); if (n && n !== it.name) item(it, { name: n }, 'Renamed'); };
  const add = async (o: string) => {
    const n = (adds[o] ?? '').trim();
    if (!n) { toast('Type the item'); return; }
    try { await api('/separations/clearance-items', { body: { owner: o, name: n } }); setAdds({ ...adds, [o]: '' }); d.reload(); } catch (er: any) { toast(er.message); }
  };
  const tog = (t: string) => setF({ ...v, exitReq: v.exitReq.includes(t) ? v.exitReq.filter((y) => y !== t) : [...v.exitReq, t] });
  return <>
    <div className="card" style={{ marginBottom: 14 }}><h3>Absconding</h3><div className="form">
      <label className="field"><span>Days absent without leave before absconding can be started</span><input type="number" min={1} value={v.absDays} onChange={(e) => setF({ ...v, absDays: e.target.value })} /></label>
      <label className="field"><span>Show-cause letters before approval</span><input type="number" min={1} max={5} value={v.absLetters} onChange={(e) => setF({ ...v, absLetters: e.target.value })} /></label>
      <label className="field"><span>Days to wait after each letter</span><input type="number" min={0} value={v.absWait} onChange={(e) => setF({ ...v, absWait: e.target.value })} /></label></div>
      <h3 style={{ marginTop: 14 }}>Exit interview is required for</h3>
      <div className="chkgrp">{(x.types ?? EXIT_TYPES).map((t: string) => <label key={t} className="chkl"><input type="checkbox" checked={v.exitReq.includes(t)} onChange={() => tog(t)} />{t}</label>)}</div>
      <div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={save}>Save</button></div>
    </div>
    <div className="card"><h3>Clearance checklist</h3>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Items can be renamed or made inactive, never deleted. Items marked automatic tick themselves from the records.</p>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(260px,1fr))', gap: 12 }}>
        {OWNERS.map((o) => <div key={o}><b>{o}</b>
          {items.filter((it) => it.owner === o).map((it) => { const on = it.status === 'Active'; return <div key={it.id} className="row" style={{ gap: 6, marginTop: 6, ...(on ? {} : { opacity: 0.55 }) }}>
            <input type="text" value={names[it.id] ?? it.name} onChange={(e) => setNames({ ...names, [it.id]: e.target.value })} onBlur={() => rename(it)} onKeyDown={(e) => e.key === 'Enter' && rename(it)} style={{ flex: 1 }} />
            {it.auto && <span className="mute" style={{ fontSize: 11 }}>automatic</span>}
            <button className="btn sm" onClick={() => item(it, { status: on ? 'Inactive' : 'Active' }, on ? 'Made inactive' : 'Made active')}>{on ? 'Make inactive' : 'Make active'}</button></div>; })}
          <div className="row" style={{ gap: 6, marginTop: 8 }}><input type="text" placeholder="New item" value={adds[o] ?? ''} onChange={(e) => setAdds({ ...adds, [o]: e.target.value })} style={{ flex: 1 }} /><button className="btn sm" onClick={() => add(o)}>Add</button></div>
        </div>)}
      </div>
      <p className="mute" style={{ fontSize: 12.5, margin: '12px 0 0' }}>Who approves each type of exit is set in Approvals, approval flow, under Termination and other exits. Resignations use the Resignation flow.</p>
    </div>
  </>;
}

/** Start a separation (HR Admin, HRBP, HRBP Corporate). Prototype ACT2.sepStart + sepStartBody. */
function StartModal({ x, onClose, onDone }: { x: any; onClose: () => void; onDone: (id: string) => void }) {
  const [ty, setTy] = useState('Resignation');
  const [e, setE] = useState<Person | null>(null);
  const [p, setP] = useState<Record<string, string>>({});
  const openIds = new Set((x.cases ?? []).filter((c: any) => isOpen(c.stage)).map((c: any) => c.employeeId));
  // Anyone active in the caller's access who is not already on notice or in an open case.
  const free = (r: Person) => r.exit_status !== 'On notice' && !openIds.has(r.id);
  const n = useLoad(() => (e ? api(`/me/separation?employeeId=${e.id}`) : Promise.resolve(null)), [e?.id]);
  const nd: number | undefined = n.data?.notice?.noticeDays;
  const sim = useLoad(() => (e && !['Resignation', 'Absconding'].includes(ty) ? api('/approvals/simulate', { body: { wf: 'resign', employeeId: e.id, payload: { exitType: ty } } }).catch(() => null) : Promise.resolve(null)), [e?.id, ty]);
  // Absconding: the last punch and the days absent without leave in the configured window (GET /separations/absent).
  const ab = useLoad(() => (e && ty === 'Absconding' ? api(`/separations/absent?employeeId=${e.id}`).catch(() => null) : Promise.resolve(null)), [e?.id, ty]);
  const set = (k: string) => (ev: any) => setP({ ...p, [k]: ev.target.value });
  const rs: string[] = ty === 'Resignation' ? (n.data?.reasons ?? []) : ty === 'Termination' ? x.termReasons ?? [] : [SEP_REASON[ty]];
  const notice = p.notice ?? 'pay';
  const on = p.on ?? today();
  const def = ['Death', 'Termination'].includes(ty) || nd == null ? today() : addDays(today(), nd);
  const lwd = ty === 'Resignation' ? (nd != null ? addDays(on, nd) : '') : ty === 'Termination' && notice === 'serve' && nd != null ? addDays(today(), nd) : ty === 'Absconding' ? p.lwd ?? (ab.data?.lastPunch || today()) : p.lwd ?? def;
  const reason = p.reason ?? rs[0] ?? '';
  const go = async () => {
    if (!e) { toast('Pick an employee'); return; }
    const noticeOption = ty === 'Termination' ? notice : '';
    const body: any = { employeeId: e.id, type: ty, reason, note: (p.note ?? '').trim(), lwd: ty === 'Resignation' ? '' : lwd, noticeOption, givenOn: ty === 'Resignation' ? on : '' };
    try {
      const r = await api('/separations', { body });
      toast(ty === 'Resignation' ? 'Resignation sent for approval' : ty === 'Absconding' ? 'Started. Send the first show-cause letter' : `${ty} sent for approval`);
      onDone(r.id);
    } catch (er: any) { toast(er.message); }
  };
  const people: any[] = sim.data?.people ?? [];
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Start a separation</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form" style={{ marginTop: 10 }}>
      <label className="field"><span>Type of exit</span><select value={ty} onChange={(ev) => { setTy(ev.target.value); setP({}); }}>{(x.types ?? EXIT_TYPES).map((y: string) => <option key={y}>{y}</option>)}</select></label>
      <label className="field"><span>Employee</span>
        <PersonPicker label="Employee" search="&status=Active" accept={free} value={e?.id ?? ''} selected={e} onChange={(_, r) => { setE(r); setP({}); }}
          placeholder="Name, code or place" noMatch="No active employee in your access matches (people on notice or with an open exit are not listed)" /></label>
    </div>
    {!e ? <p className="mute">Search and pick an employee.</p> : <>
      <div className="note" style={{ margin: '8px 0' }}>{e.name} · {e.type} · joined {fmtD(e.doj)} · reports to {e.manager || '—'} · notice in policy {nd ?? '…'} days</div>
      <div className="form">
        <label className="field"><span>{ty === 'Death' ? 'Date of death' : ty === 'Absconding' ? 'Last day present' : 'Last working day'}</span>
          <input type="date" value={lwd} onChange={set('lwd')} readOnly={ty === 'Resignation' || (ty === 'Termination' && notice === 'serve')} style={ty === 'Resignation' ? { background: 'var(--bg,#f4f5f6)' } : undefined} /></label>
        <label className="field"><span>Reason</span><select value={reason} onChange={set('reason')}>{rs.map((r) => <option key={r}>{r}</option>)}</select></label>
        {ty === 'Termination' && <label className="field"><span>Notice period</span><select value={notice} onChange={set('notice')}>
          <option value="pay">Pay {nd ?? ''} days in lieu of notice</option><option value="serve">Employee serves the notice</option><option value="none">No notice or notice pay (misconduct)</option></select></label>}
        {ty === 'Resignation' && <label className="field"><span>Resignation received on</span><input type="date" value={on} max={today()} onChange={set('on')} /></label>}
        <label className="field" style={{ gridColumn: '1/-1' }}><span>Note</span><input type="text" value={p.note ?? ''} onChange={set('note')} placeholder={ty === 'Termination' ? 'What happened, and the show-cause or warning reference' : 'Optional'} /></label>
      </div>
      {ty === 'Absconding' && ab.data && (() => {
        const a = ab.data, ok = a.run >= a.absDays; // the server's rule: days absent in a row
        return <div className={`note ${ok ? '' : 'bad'}`} style={{ marginTop: 8 }}>Last punch {a.lastPunch ? fmtD(a.lastPunch) : 'not found in 90 days'} · {a.absentDays} of the last {a.window} days absent without leave. {ok
          ? `Starting this sends nothing yet: HRBP sends ${a.absLetters} show-cause letters, then it goes for approval. Salary is put on hold.`
          : `The policy needs ${a.absDays} days absent in a row before absconding can be started. You can still start it with a note.`}</div>;
      })()}
      {ty === 'Resignation' && <p className="mute" style={{ fontSize: 12.5, marginTop: 8 }}>The last working day is the received date plus {nd ?? '…'} days of notice and cannot be edited here. An approver can agree an earlier date while approving.</p>}
      {!['Resignation', 'Absconding'].includes(ty) && sim.data && <p className="mute" style={{ fontSize: 12.5, marginTop: 8 }}>Approvers: {people.map((s) => `${s.stage} (${s.name || 'nobody mapped'})`).join(' → ') || 'none, approved straight away'}</p>}
    </>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={!e} onClick={go}>Start</button></div>
  </Modal>;
}

/** One case: progress, summary, show-cause, clearance, exit interview, full and final, letters, history. Prototype sepModal. */
export function CaseModal({ id, onClose }: { id: string; onClose: () => void }) {
  const { me, can } = useAuth();
  const role = me?.ctx.role ?? '';
  const d = useLoad(() => api(`/separations/${id}`), [id]);
  const [exitForm, setExitForm] = useState(false);
  const [letter, setLetter] = useState<any>(null);
  const [fnfOpen, setFnfOpen] = useState(false);
  const [terms, setTerms] = useState<{ lwd?: string; shortfall?: string }>({});
  if (!d.data) return <Modal wide onClose={onClose}><Loading error={d.error} /></Modal>;
  const x = d.data, e = x.employee || {}, st: string = x.stage, ended = ENDED.includes(st), hr = !!x.isHR, req = x.request, lwd: string = x.lwd;
  const cfg = x.settings ?? { absLetters: 2, absWait: 3 };
  const people: any[] = req?.people ?? [];
  const person = (s: string) => people.find((p) => p.stage === s)?.person as string | undefined;
  const act = async (a: string, body?: any, msg?: string) => { try { await api(`/separations/${id}/${a}`, { body: body ?? {} }); toast(msg || 'Saved'); d.reload(); } catch (er: any) { toast(er.message); } };
  const sign = async (item: number) => { try { await api(`/separations/${id}/clearance/${item}`, { body: {} }); toast('Ticked'); d.reload(); } catch (er: any) { toast(er.message); } };
  const view = async (l: any) => { try { setLetter(await api(`/separations/${id}/letters/${l.id}`)); } catch (er: any) { toast(er.message); } };
  const issue = async (kind: string) => {
    try { await api(`/separations/${id}/letter`, { body: { kind } }); toast(`${kind} issued`); const r = await api(`/separations/${id}`); d.reload(); const l = (r.letters ?? []).filter((y: any) => y.kind === kind).pop(); if (l) view(l); } catch (er: any) { toast(er.message); }
  };

  // Stepper (prototype: pills with ✓ for done steps and the current one in blue).
  const order = STEPS.map(([k]) => k);
  const cur = st === 'Show-cause' ? 1 : st === 'Ready to close' ? 6 : Math.max(0, order.indexOf(GROUP[st] ?? ''));
  const stepper = <div className="row" style={{ gap: 4, flexWrap: 'wrap', margin: '12px 0 14px' }}>{STEPS.map(([k, l], i) => {
    const done = !ended && (i < cur || st === 'Closed'), now = !ended && i === cur && st !== 'Closed';
    return <span key={k} style={{ display: 'inline-flex', alignItems: 'center', gap: 6, padding: '5px 10px', borderRadius: 999, fontSize: 12.5, border: `1px solid ${done ? '#1a7f4b' : now ? '#2a5db0' : 'var(--line,#ddd)'}`, color: done ? '#1a7f4b' : now ? '#2a5db0' : 'var(--ink2,#666)', fontWeight: now ? 700 : 500 }}>
      {done ? '✓' : i + 1} {k === 'approval' && x.type === 'Absconding' && st === 'Show-cause' ? 'Show-cause' : l}</span>;
  })}</div>;

  // Summary
  const nd: number = x.noticeDays;
  const stepIdx: number = typeof req?.stepIndex === 'number' ? req.stepIndex : req ? req.chain.indexOf(req.step) : -1;
  const approvalTxt = req ? `${req.id} · ${req.chain.map((s: string, i: number) => `${req.status === 'Approved' || i < stepIdx ? '✓ ' : i === stepIdx && req.status === 'Pending' ? '→ ' : ''}${s}${person(s) ? ` (${person(s)})` : ''}`).join(' · ') || 'no approver needed'} · ${req.status}`
    : x.type === 'Absconding' ? 'After the show-cause letters' : 'Not sent';
  const termsBox = req?.canAct && x.type === 'Resignation' && req.status === 'Pending' && <div style={{ border: '1px solid var(--line,#ddd)', borderRadius: 10, padding: 12, marginTop: 12 }}>
    <b>Last working day</b>
    <div className="mute" style={{ fontSize: 12.5, margin: '2px 0 8px' }}>As per notice: {fmtD(req.noticeLwd)}{req.lwd !== req.noticeLwd ? ` · currently ${fmtD(req.lwd)}` : ''}{req.termsBy ? `, set by ${req.termsBy}${req.termsRole ? ` (${req.termsRole})` : ''}` : ''}. To let {e.name} leave earlier, pick the date before you approve.</div>
    <div className="row" style={{ gap: 10, flexWrap: 'wrap', alignItems: 'flex-end' }}>
      <label className="field" style={{ margin: 0 }}><span>Release on</span><input type="date" min={today()} max={req.noticeLwd} value={terms.lwd ?? req.lwd} onChange={(ev) => setTerms({ ...terms, lwd: ev.target.value })} /></label>
      <label className="field" style={{ margin: 0 }}><span>Notice shortfall</span><select value={terms.shortfall ?? (req.shortfall || 'waive')} onChange={(ev) => setTerms({ ...terms, shortfall: ev.target.value })}>
        <option value="waive">Waive it (company agrees to release early)</option><option value="recover">Recover it from the employee as per policy</option></select></label>
      <button className="btn sm" onClick={() => act('terms', { lwd: terms.lwd || req.lwd, shortfall: terms.shortfall ?? (req.shortfall || 'waive') }, 'Last working day saved. Approve it in Approvals')}>Save</button>
    </div></div>;
  const by: string = x.startedBy || '';
  const sum = <div className="card" style={{ marginBottom: 12 }}><dl className="kv">
    <dt>Case</dt><dd>{x.id} · started {fmtD(x.givenOn)} by {by}</dd>
    <dt>Reason</dt><dd>{x.reason}{x.note && <> · <span className="ink2" style={{ whiteSpace: 'pre-wrap' }}>{x.note}</span></>}</dd>
    <dt>{x.type === 'Death' ? 'Date' : 'Last working day'}</dt><dd>{lwd ? fmtD(lwd) : '—'}{st === 'Notice' && lwd ? ` · ${dayDiff(today(), lwd)} days to go` : ''}</dd>
    {x.type === 'Termination' && <><dt>Notice</dt><dd>{x.noticeOption === 'pay' ? `${nd} days paid in lieu, added to the full and final` : x.noticeOption === 'serve' ? `Serves ${nd} days` : 'No notice pay'}</dd></>}
    {x.type === 'Absconding' && <><dt>Notice</dt><dd>{nd} days not served{x.noticeRecover !== false ? ', recovered in the full and final' : ''}</dd></>}
    <dt>Approval</dt><dd>{approvalTxt}</dd>
    <dt>Next step</dt><dd><b>{x.next || '—'}</b></dd>
  </dl>{termsBox}</div>;

  // Show-cause letters (absconding), by the separation settings.
  let abs: ReactNode = null;
  if (x.type === 'Absconding') {
    const L: any[] = x.showCause ?? [], n = L.length, w = n ? cfg.absWait - dayDiff(L[n - 1].at, today()) : 0;
    abs = <div className="card" style={{ marginBottom: 12 }}><h3>Show-cause letters</h3>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Absent since {lwd ? fmtD(lwd) : '—'}. Policy: {cfg.absLetters} letters, {cfg.absWait} days apart, before it goes for approval. Salary is on hold.</p>
      {L.length ? L.map((l) => <div key={l.n} style={{ fontSize: 13 }}>✓ Letter {l.n} sent on {fmtD(l.at)} by {l.by}</div>) : <div className="mute" style={{ fontSize: 13 }}>No letter sent yet</div>}
      {hr && st === 'Show-cause' && <div className="row" style={{ marginTop: 10, gap: 8, flexWrap: 'wrap' }}>
        {n < cfg.absLetters ? (n > 0 && w > 0 ? <><span className="mute" style={{ fontSize: 12.5 }}>Letter {n + 1} can go in {plural(w, 'day')}</span><button className="btn primary sm" disabled>Send letter {n + 1}</button></>
          : <button className="btn primary sm" onClick={() => act('show-cause', {}, `Show-cause letter ${n + 1} sent`)}>Send letter {n + 1}</button>)
          : w > 0 ? <><span className="mute" style={{ fontSize: 12.5 }}>Can go for approval in {plural(w, 'day')}</span><button className="btn sm" disabled>Send for approval now</button></>
            : <button className="btn primary sm" onClick={() => act('send', {}, 'Sent for approval')}>Send for approval</button>}
        <button className="btn sm" onClick={() => act('reported-back', {}, 'Case closed; salary hold released')}>Employee reported back</button>
      </div>}
    </div>;
  }

  // Exit clearance
  const items: any[] = x.clearance ?? [];
  const ei = x.exitInterview;
  const exDone = !!ei || (x.exitNeeded && items.some((i) => i.auto === 'exit' && i.done));
  const canClear = !ended && !['Show-cause', 'Approval'].includes(st);
  const secDone = (o: string) => items.filter((i) => i.owner === o).every((i) => i.done);
  const openSecs = OWNERS.filter((o) => !secDone(o)).length;
  const clearAll = items.every((i) => i.done);
  const why = (it: any) => it.auto === 'assets' ? (it.done ? 'Nothing pending' : (x.assets ?? []).length ? `${(x.assets ?? []).length} still issued: ${(x.assets ?? []).map((a: any) => a.name).join(', ')}` : it.why)
    : it.auto === 'exit' ? (!x.exitNeeded ? `Not needed for ${x.type}` : exDone ? `Done${ei?.at ? ' ' + ei.at : ''}` : 'Not done yet') : '';
  const clr = <div className="card" style={{ marginBottom: 12 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Exit clearance</h3>{canClear ? (clearAll ? <Pill k="ok">Complete</Pill> : <Pill k="warn">{openSecs} of 3 open</Pill>) : <Pill k="n">Opens after approval</Pill>}</div>
    <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 10px' }}>Each owner ticks their items. Full and final can be prepared only after all three are complete.</p>
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(220px,1fr))', gap: 10 }}>{OWNERS.map((o) => {
      const ok = canClear && !!x.canSign?.[o] && ['Notice', 'Clearance'].includes(st);
      const last = items.filter((i) => i.owner === o && i.by).sort((a, b) => (sortKey(a.at) < sortKey(b.at) ? -1 : 1)).pop();
      return <div key={o} style={{ border: '1px solid var(--line,#ddd)', borderRadius: 10, padding: 10 }}>
        <div className="row sp"><b>{o}</b>{secDone(o) ? <Pill k="ok">Done</Pill> : <Pill k="warn">Open</Pill>}</div>
        <div className="mute" style={{ fontSize: 12, margin: '2px 0 6px' }}>{o === 'Reporting manager' ? e.manager || '—' : o === 'HRBP' ? x.hrbp || 'Unassigned' : 'Finance or Payroll'}</div>
        {items.filter((i) => i.owner === o).map((it) => { const w = why(it); return <label key={it.id} className="chkl" style={{ display: 'flex', alignItems: 'flex-start', gap: 6, margin: '4px 0', fontSize: 13 }}>
          <input type="checkbox" checked={!!it.done} disabled={!ok || !!it.auto || it.done} onChange={() => sign(it.id)} />
          <span>{it.name}{w && <><br /><span className="mute" style={{ fontSize: 11.5 }}>{w}{it.auto ? ' · automatic' : ''}</span></>}</span></label>; })}
        {last && <div className="mute" style={{ fontSize: 11.5, marginTop: 6 }}>Last ticked by {last.by}, {last.at}</div>}
      </div>;
    })}</div>
  </div>;

  // Exit interview, and the rehire decision HR can save at any stage (prototype sp_rh).
  const rehire = async (v: string) => { try { await api(`/separations/${id}/rehire`, { body: { rehire: v } }); toast('Saved'); d.reload(); } catch (er: any) { toast(er.message); } };
  const exitC = <div className="card" style={{ marginBottom: 12 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Exit interview</h3>{x.exitNeeded ? (exDone ? <Pill k="ok">Done</Pill> : <Pill k="warn">Pending</Pill>) : <Pill k="n">Optional for {x.type}</Pill>}</div>
    {exDone && !EX_SEE.includes(role) && <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 0' }}>Answers are visible to HR only.</p>}
    {ei && EX_SEE.includes(role) && <dl className="kv" style={{ marginTop: 8 }}>
      <dt>Main reason</dt><dd>{ei.main_reason}</dd>
      <dt>Ratings</dt><dd>{(x.exitQs ?? []).map((q: string) => `${q} ${ei.ratings?.[q] ?? '—'}/5`).join(' · ')}</dd>
      <dt>Would recommend us</dt><dd>{ei.recommend}</dd>
      {ei.comments && <><dt>Comments</dt><dd>{ei.comments}</dd></>}
      <dt>Filled by</dt><dd>{ei.by_name}, {ei.at}</dd></dl>}
    <div className="row" style={{ gap: 8, marginTop: 8, flexWrap: 'wrap' }}>
      {hr && !ended && !exDone && st !== 'Show-cause' && x.open && <button className="btn sm" onClick={() => setExitForm(true)}>Fill exit interview</button>}
      {hr ? <label className="row" style={{ gap: 6, fontSize: 13 }}>Eligible for rehire
        <select value={x.rehire ?? ''} onChange={(ev) => rehire(ev.target.value)}>{REHIRE.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></label>
        : x.rehire ? <span style={{ fontSize: 13 }}>Eligible for rehire: <b>{rehireLabel(x.rehire)}</b></span> : null}
    </div>
  </div>;

  // Full and final
  const f = x.fnf, fOk = f?.status === 'Approved';
  const payView = can('pay.run') || can('pay.claims');
  const fnfC = <div className="card" style={{ marginBottom: 12 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Full and final</h3>{f ? <Pill k={fOk ? 'ok' : 'info'}>{f.status}</Pill> : <Pill k="n">{clearAll ? 'Ready to prepare' : 'Waits for clearance'}</Pill>}</div>
    <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 8px' }}>{f ? `${f.id} · net ${inr(f.net)} · paid in ${ymLabel(f.pay_ym)}` : 'Leave encashment, gratuity, notice pay or recovery, and pending recoveries are worked out from the records.'}</p>
    {payView && lwd && ['Notice', 'Clearance', 'Full and final', 'Letters', 'Ready to close', 'Closed'].includes(st) && <button className="btn sm" onClick={() => setFnfOpen(true)}>Open full and final</button>}
  </div>;

  // Letters. The no dues certificate: HR issues it in the case; Payroll reads it from the full and final.
  const need: string = x.letterNeeded;
  const has = (k: string) => (x.letters ?? []).filter((l: any) => l.kind === k).pop();
  const rel = x.relieving ?? x.exitNeeded;
  const lets = [...(x.type === 'Termination' && req?.status === 'Approved' ? ['Termination letter'] : []), ...(rel && fOk ? ['Relieving letter', 'Experience letter'] : [])];
  const nod = has('No dues certificate');
  const noDues = async () => {
    if (nod) return view(nod);
    if (hr) return issue('No dues certificate');
    try { setLetter(await api(`/fnf/${e.id}/no-dues`)); } catch (er: any) { toast(er.message); }
  };
  const letC = <div className="card" style={{ marginBottom: 12 }}><h3>Letters</h3>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>{need ? `The ${need.toLowerCase()} closes the case.` : `No closing letter is needed for ${x.type.toLowerCase()}.`} {rel ? 'Relieving and experience letters open after the full and final is approved.' : ''}</p>
    <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
      {lets.map((k) => { const l = has(k); return <button key={k} className={`btn sm ${l ? '' : 'primary'}`} disabled={!hr && !l} onClick={() => (l ? view(l) : issue(k))}>{l ? '✓ ' : ''}{k}</button>; })}
      {fOk && <button className="btn sm" disabled={!hr && !nod && !payView} onClick={noDues}>No dues certificate</button>}
      {!lets.length && !fOk && <span className="mute" style={{ fontSize: 13 }}>Nothing to issue yet</span>}
      {hr && st === 'Ready to close' && <button className="btn sm primary" disabled={!x.rehire} title={!x.rehire ? 'Choose Eligible for rehire first' : undefined} onClick={() => act('close', { rehire: x.rehire }, 'Case closed')}>Close the case</button>}
    </div>
  </div>;

  // History: the server's record of the case, newest first (prototype c.hist reversed).
  const hist: any[] = [...(x.history ?? [])].reverse();
  const canCancel = hr && ['Show-cause', 'Approval'].includes(st) && x.type !== 'Resignation';
  const canWithdraw = x.type === 'Resignation' && x.open && (x.isMe || hr) && ['Approval', 'Notice'].includes(st) && (!x.withdrawBy || today() <= x.withdrawBy);
  const canLwd = hr && st === 'Notice';
  const histC = <div className="card"><h3>History</h3>
    {hist.map((h, i) => <div key={i} style={{ fontSize: 12.5, padding: '4px 0', borderBottom: '1px solid var(--line,#eee)' }}><span className="mute">{atS(h.at)}</span> · {h.t}{h.by && <span className="mute"> · {h.by}</span>}</div>)}
    {(canCancel || canWithdraw || canLwd) && <div className="row" style={{ marginTop: 10, gap: 8, flexWrap: 'wrap' }}>
      {canCancel && <button className="btn sm" onClick={() => act('cancel', { note: 'Cancelled' }, 'Exit cancelled')}>Cancel this exit</button>}
      {canLwd && <button className="btn sm" onClick={() => { const l = prompt('New last working day (YYYY-MM-DD)', lwd); const n = l && prompt('Reason for the change'); if (l && n) act('lwd', { lwd: l, note: n }, 'Last working day changed'); }}>Change last working day</button>}
      {canWithdraw && <button className="btn sm" onClick={() => confirm('Withdraw this resignation?') && act('withdraw', {}, 'Resignation withdrawn')}>Withdraw resignation</button>}
    </div>}
    {x.type === 'Resignation' && ['Approval', 'Notice'].includes(st) && x.withdrawBy && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>A resignation can be withdrawn up to {fmtD(x.withdrawBy)}.</p>}
  </div>;

  // Like the prototype, a dialog opened from the case takes its place, and the case comes back when it closes.
  if (exitForm) return <ExitForm x={x} onClose={() => setExitForm(false)} onSaved={() => { setExitForm(false); d.reload(); }} />;
  if (fnfOpen) return <FnfDialog empId={e.id} sepId={x.id} canPrepare={can('pay.claims', 2) || can('pay.run', 2)} onClose={() => { setFnfOpen(false); d.reload(); }} onDone={() => { setFnfOpen(false); d.reload(); }} />;
  if (letter) return <LetterView l={letter} onClose={() => setLetter(null)} />;
  return (
    <Modal wide onClose={onClose}>
      <div className="row sp"><div><h2 style={{ margin: 0 }}>{e.name} <span className="mono mute" style={{ fontSize: 13 }}>{e.id}</span></h2>
        <div className="mute" style={{ fontSize: 12.5 }}>{e.designation} · {e.department} · {e.location} · reports to {e.manager || '—'}</div></div>
        <div className="row" style={{ gap: 6 }}><Pill k="n">{x.type}</Pill>{stgPill(st)}<button className="btn sm" onClick={onClose}>Close</button></div></div>
      {stepper}{sum}{abs}{!ended && <>{clr}{exitC}{fnfC}{letC}</>}{histC}
    </Modal>
  );
}

/** A letter: the case's letters as text; the settlement letter as the prototype's standalone HTML document. */
function LetterView({ l, onClose }: { l: { kind: string; body?: string; html?: string; file?: string; by_name?: string; at?: string }; onClose: () => void }) {
  const [frame, setFrame] = useState<HTMLIFrameElement | null>(null);
  if (l.html) {
    const save = () => { const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([l.html!], { type: 'text/html' })); a.download = l.file || 'letter.html'; a.click(); URL.revokeObjectURL(a.href); };
    return <Modal wide onClose={onClose}>
      <div className="row sp" style={{ marginBottom: 10 }}><h2 style={{ margin: 0 }}>{l.kind}</h2><div className="row"><button className="btn" onClick={save}>Download</button><button className="btn primary" onClick={() => frame?.contentWindow?.print()}>Print or save as PDF</button><button className="btn sm" onClick={onClose}>Close</button></div></div>
      <iframe ref={setFrame} title={l.kind} srcDoc={l.html} style={{ width: '100%', height: 560, border: '1px solid var(--line,#ddd)', borderRadius: 8, background: '#fff' }} />
    </Modal>;
  }
  return <Modal wide onClose={onClose}>
    <div className="row sp" style={{ marginBottom: 10 }}><h2 style={{ margin: 0 }}>{l.kind}</h2><div className="row"><button className="btn primary" onClick={printSlip}>Print or save as PDF</button><button className="btn sm" onClick={onClose}>Close</button></div></div>
    <div className="slip" style={{ whiteSpace: 'pre-wrap', fontSize: 13.5, lineHeight: 1.6 }}>{l.body}{l.by_name && <div className="mute" style={{ marginTop: 16, fontSize: 11 }}>Issued by {l.by_name}, {l.at}</div>}</div>
  </Modal>;
}

/** Exit interview form (HR with the employee, or the employee themselves). Prototype exitForm. */
function ExitForm({ x, self, onClose, onSaved }: { x: any; self?: boolean; onClose: () => void; onSaved: () => void }) {
  const qs: string[] = x.exitQs ?? [], main: string[] = x.exitMain ?? [];
  const [b, setB] = useState<{ mainReason: string; recommend: string; ratings: Record<string, number>; comments: string }>({ mainReason: main[0] ?? '', recommend: 'Yes', ratings: Object.fromEntries(qs.map((q) => [q, 3])), comments: '' });
  const save = async () => {
    try { await api(`/separations/${x.id}/interview`, { body: b }); toast(self ? 'Thank you. Your exit interview is saved' : 'Exit interview saved'); onSaved(); } catch (er: any) { toast(er.message); }
  };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Exit interview</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="ink2" style={{ fontSize: 13 }}>{self ? 'Your answers help us improve. Only HR sees them, not your manager.' : 'Fill this with the employee, or from their written answers.'}</p>
    <div className="form" style={{ marginTop: 8 }}>
      <label className="field"><span>Main reason for leaving</span><select value={b.mainReason} onChange={(ev) => setB({ ...b, mainReason: ev.target.value })}>{main.map((r) => <option key={r}>{r}</option>)}</select></label>
      <label className="field"><span>Would you recommend us as a place to work</span><select value={b.recommend} onChange={(ev) => setB({ ...b, recommend: ev.target.value })}>{['Yes', 'Maybe', 'No'].map((r) => <option key={r}>{r}</option>)}</select></label>
      {qs.map((q) => <label key={q} className="field"><span>{q} (1 poor, 5 very good)</span>
        <select value={String(b.ratings[q] ?? 3)} onChange={(ev) => setB({ ...b, ratings: { ...b.ratings, [q]: +ev.target.value } })}>{['5', '4', '3', '2', '1'].map((n) => <option key={n}>{n}</option>)}</select></label>)}
      <label className="field" style={{ gridColumn: '1/-1' }}><span>Anything else</span><input type="text" value={b.comments} onChange={(ev) => setB({ ...b, comments: ev.target.value })} /></label>
    </div>
    <div className="row" style={{ marginTop: 12, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Save</button></div>
  </Modal>;
}

/** Payroll → Full and final: everyone serving notice or already inactive. Prototype vFnf. */
export function FnfAdmin() {
  const { can } = useAuth();
  const d = useLoad(() => api('/fnf'), []);
  const seps = useLoad(() => (can('emp.sep') ? api('/separations').then((r) => r.cases as any[]) : Promise.resolve([] as any[])), []);
  const [pv, setPv] = useState<any>(null);
  const rows = [...(d.data?.rows ?? [])].sort((a: any, b: any) => ((a.lwd || '') < (b.lwd || '') ? -1 : 1));
  const pg = usePage(rows, 100);
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data;
  const caseOf = (empId: string) => (seps.data ?? []).find((c) => c.employeeId === empId && isOpen(c.stage)) ?? (seps.data ?? []).find((c) => c.employeeId === empId && c.stage === 'Closed');
  return <>
    <div className="card"><h3>Full and final settlement</h3>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Everyone serving notice or already inactive. Leave encashment, gratuity (after 5 years), notice recovery, outstanding advances and uniform or asset recoveries are worked out from each employee's work policy and records. The final month's salary is paid in the normal payroll run.</p>
      <div className="tw"><table><thead><tr><th>Employee</th><th>Last working day</th><th>Status</th><th>Clearance</th><th className="num">Net payable</th><th></th></tr></thead><tbody>
        {(pg.rows ?? []).map((r: any) => { const f = r.fnf_id && r.fnf_status !== 'Rejected', ok = r.fnf_status === 'Approved', inact = r.status === 'Inactive';
          return <tr key={r.id}><td><b>{r.name}</b> <span className="mono mute">{r.id}</span></td><td>{fmtD(r.lwd)}</td>
            <td>{f ? (ok && r.receivable ? <Pill k={r.receivable.status === 'Open' ? 'bad' : 'ok'}>{r.receivable.status === 'Open' ? 'Owed by employee' : `Owed amount ${r.receivable.status.toLowerCase()}`}</Pill>
              : <Pill k={ok ? 'ok' : 'info'}>{ok ? `Approved, in ${ymLabel(r.pay_ym)}` : r.fnf_status}</Pill>) : r.fnf_status === 'Rejected' ? <Pill k="bad">Rejected</Pill> : <Pill k={inact ? 'warn' : 'n'}>{inact ? 'Not prepared' : 'On notice'}</Pill>}</td>
            <td>{r.assets > 0 ? <Pill k="bad">{r.assets} pending</Pill> : <Pill k="ok">Clear</Pill>}</td>
            <td className="num">{f && r.net != null ? inr(r.net) : r.estNet != null ? inr(r.estNet) : '—'}</td>
            <td><button className="btn sm" onClick={() => setPv(r)}>{f ? 'View' : 'Prepare'}</button></td></tr>; })}
        {!rows.length && <tr><td colSpan={6} className="mute">Nobody is serving notice or inactive.</td></tr>}
      </tbody></table></div>
      {pg.pager}
    </div>
    {pv && <FnfDialog empId={pv.id} sepId={caseOf(pv.id)?.id} fnf={pv.fnf_id ? { id: pv.fnf_id, status: pv.fnf_status, receivable: pv.receivable } : null} canPrepare={!!x.canPrepare}
      onClose={() => setPv(null)} onDone={() => { setPv(null); d.reload(); seps.reload(); }} />}
  </>;
}

/** The variable pay of a structure made from components, for the current cycle: HR enters the amount the settlement
 *  pays (blank: none). PUT /api/fnf/{id}/variable. */
function VarAmount({ empId, code, amount, onSaved }: { empId: string; code: string; amount: number | null; onSaved: () => void }) {
  const [v, setV] = useState(amount == null ? '' : String(amount));
  const save = async () => {
    if (v === (amount == null ? '' : String(amount))) return;
    try { await api(`/fnf/${empId}/variable`, { method: 'PUT', body: { code, amount: v === '' ? null : +v } }); toast('Saved'); onSaved(); } catch (er: any) { toast(er.message); }
  };
  return <input type="number" min={0} style={{ width: 110, textAlign: 'right' }} placeholder="Amount" value={v} onChange={(e) => setV(e.target.value)} onBlur={save} />;
}

/** The settlement for one person (prototype ACT2.fnfOpen): lines, clearance, send for approval, settlement letter, no dues certificate. */
function FnfDialog({ empId, sepId, fnf, canPrepare, onClose, onDone }: { empId: string; sepId?: string; fnf?: { id: string; status: string; receivable?: any } | null; canPrepare: boolean; onClose: () => void; onDone: () => void }) {
  const pv = useLoad(() => api(`/fnf/${empId}`), [empId]);
  const sc = useLoad(() => (sepId ? api(`/separations/${sepId}`) : Promise.resolve(null)), [sepId]);
  const [letter, setLetter] = useState<any>(null);
  const [owedNote, setOwedNote] = useState('');
  if (!pv.data) return <Modal wide onClose={onClose}><Loading error={pv.error} /></Modal>;
  const p = pv.data, e = p.employee, c = sc.data;
  const f = c?.fnf ? { id: c.fnf.id, status: c.fnf.status, net: c.fnf.net, lines: c.fnf.lines } : fnf ? { ...fnf, net: undefined, lines: undefined } : null;
  const live = f && f.status !== 'Rejected';
  const approved = f?.status === 'Approved';
  const lines: any[] = approved && f?.lines ? f.lines : p.lines;
  const net: number = approved && f?.net != null ? f.net : p.net;
  const assets: any[] = p.assets ?? [];
  const pend: string[] = c ? OWNERS.filter((o) => (c.clearance ?? []).some((i: any) => i.owner === o && !i.done)) : [];
  const prep = async () => { try { const r = await api(`/fnf/${empId}`, { body: {} }); toast(`${r.id} sent for approval, net ${inr(r.net)}`); onDone(); } catch (er: any) { toast(er.message); } };
  // Prototype fnfLetter and fnfNoDues: the settlement letter (HTML) and the no dues certificate, from the server.
  const doc = async (k: 'letter' | 'no-dues') => { try { setLetter(await api(`/fnf/${empId}/${k}`)); } catch (er: any) { toast(er.message); } };
  // A settlement below zero is owed by the employee: not paid through payroll; Payroll records it recovered or waived.
  const owed = c?.fnf?.receivable ?? fnf?.receivable;
  const closeOwed = async (action: 'recovered' | 'waived') => {
    try { await api(`/fnf/${empId}/receivable`, { body: { action, note: owedNote } }); toast(action === 'recovered' ? 'Recovery recorded' : 'Amount waived'); onDone(); } catch (er: any) { toast(er.message); }
  };
  const blocked = p.assetsPending > 0 || pend.length > 0;
  if (letter) return <LetterView l={letter} onClose={() => setLetter(null)} />;
  return <Modal wide onClose={onClose}>
    <div className="row sp"><div><h2>Full and final: {e.name}</h2><span className="mono mute">{e.id}</span></div><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '8px 0' }}>Last working day {fmtD(e.lwd)} · joined {fmtD(e.doj)} · {e.type}. Settled within the policy limit of 45 days, in {ymLabel(p.payYm)}.</p>
    {p.assetsPending > 0 && <div className="note bad" style={{ margin: '10px 0' }}>Clearance pending: {assets.length ? assets.map((a) => `${a.name} ${a.serial}`).join(', ') : plural(p.assetsPending, 'asset') + ' still issued'}. Mark the asset returned or recovered under Employee file, then prepare the settlement.</div>}
    {pend.length > 0 && <div className="note bad" style={{ margin: '10px 0' }}>Exit clearance open in {sepId}: waiting for {pend.join(', ')}. Prepare the settlement after it is complete.</div>}
    {!blocked && <div className="note ok" style={{ margin: '10px 0' }}>{sepId && c ? `Exit clearance complete in ${sepId}. No asset is pending.` : 'Clearance complete. No asset is pending.'}</div>}
    <div className="tw"><table><thead><tr><th>Item</th><th>How it is worked out</th><th className="num">Amount</th></tr></thead><tbody>
      {lines.map((l, i) => <tr key={i}><td><b>{l.label}</b></td><td className="ink2" style={{ fontSize: 12.5 }}>{l.note || ''}</td>
        <td className="num">{String(l.key || '').startsWith('var:') && !live && canPrepare
          ? <VarAmount empId={empId} code={String(l.key).slice(4)} amount={l.kind === 'earn' ? l.amount : null} onSaved={pv.reload} />
          : <>{l.kind === 'ded' ? '− ' : ''}{inr(l.amount)}{l.kind === 'info' && <> <span className="mute">info</span></>}</>}</td></tr>)}
      {!lines.length && <tr><td colSpan={3} className="mute">Nothing extra is due. Final month salary is paid in the normal run.</td></tr>}
      <tr><td colSpan={2}><b>Net full and final</b><div className="mute" style={{ fontSize: 12 }}>The final month's salary is paid in the payroll run for {ymLabel(String(e.lwd).slice(0, 7))}</div></td><td className="num"><b>{inr(net)}</b></td></tr>
    </tbody></table></div>
    {approved && owed && <div className={`note ${owed.status === 'Open' ? 'bad' : 'ok'}`} style={{ margin: '10px 0' }}>
      Recoverable from employee: <b>{inr(owed.amount)}</b> · {owed.status}{owed.note ? ` · ${owed.note}` : ''}{owed.by_name ? ` (${owed.by_name}, ${fmtD(owed.at)})` : ''}.
      {owed.status === 'Open' && <> Nothing is paid through payroll. The no dues certificate waits until this is recovered or waived.</>}
      {owed.status === 'Open' && canPrepare && <div className="row" style={{ marginTop: 8, gap: 8 }}>
        <input placeholder="Payment reference, or why it is waived" value={owedNote} onChange={(ev) => setOwedNote(ev.target.value)} style={{ flex: 1 }} />
        <button className="btn sm" onClick={() => closeOwed('recovered')}>Record recovery</button>
        <button className="btn sm" onClick={() => closeOwed('waived')}>Waive</button></div>}
    </div>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}>
      <button className="btn" onClick={onClose}>Close</button>
      {!live && !blocked && canPrepare && <button className="btn primary" onClick={prep}>Send for approval</button>}
      {approved && <><button className="btn" onClick={() => doc('letter')}>Settlement letter</button>
        <button className="btn" onClick={() => doc('no-dues')}>No dues certificate</button></>}
    </div>
  </Modal>;
}

/** My exit: the employee's own case, shown above the tabs in My pay and profile. Prototype vEss wrapper. */
export function MyExit({ c, onChange }: { c: any; onChange: () => void }) {
  const [form, setForm] = useState(false);
  const st: string = c.stage;
  if (ENDED.includes(st)) return null;
  const pend = OWNERS.filter((o) => (c.clearance ?? []).some((i: any) => i.owner === o && !i.done));
  const assets: any[] = c.assets ?? [];
  const exDone = !!c.exitInterview;
  // As in the prototype, the card has no withdraw button: an employee withdraws by raising a
  // "Withdraw resignation" request from My view › Separation (MyExit.tsx), which goes through the resignation route.
  return <div className="card" style={{ marginBottom: 14, borderLeft: '4px solid #2a5db0' }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>My exit</h3>{stgPill(st)}</div>
    <p className="ink2" style={{ fontSize: 13.5, margin: '6px 0' }}>{c.type}{c.lwd && <> · last working day <b>{fmtD(c.lwd)}</b></>}{st === 'Notice' && c.lwd ? ` · ${dayDiff(today(), c.lwd)} days to go` : ''}</p>
    {['Notice', 'Clearance'].includes(st) && <p style={{ fontSize: 13, margin: '4px 0' }}>{assets.length > 0 && <>Return before you leave: <b>{assets.map((a) => a.name).join(', ')}</b>. </>}{pend.length ? `Clearance open with ${pend.join(', ')}.` : 'Clearance complete.'}</p>}
    {st === 'Full and final' && <p style={{ fontSize: 13, margin: '4px 0' }}>Your full and final settlement is being prepared.</p>}
    {st === 'Closed' && <p style={{ fontSize: 13, margin: '4px 0' }}>Your exit is complete. Your letters are with HR.</p>}
    {c.exitNeeded && !exDone && ['Notice', 'Clearance'].includes(st) ? <button className="btn primary sm" onClick={() => setForm(true)}>Fill my exit interview</button>
      : exDone ? <span className="mute" style={{ fontSize: 12.5 }}>Exit interview done. Thank you.</span> : null}
    {form && <ExitForm x={c} self onClose={() => setForm(false)} onSaved={() => { setForm(false); onChange(); }} />}
  </div>;
}
