import { useEffect, useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api, fmtD, inr, today } from '../api';
import { useAuth, type Me } from '../auth';
import { Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import PersonPicker from '../components/PersonPicker';
import Payslip, { printSlip } from '../components/Payslip';
import { loadSlip } from './Employees';
import { UniformBlock } from '../components/Uniform';
import { EmployeeLifecycle } from './Hiring';
import { EffectivePolicyCard, EmployeeAccessCard } from '../components/AccessCards';
import { EmployeeAccommodation } from './Accommodation';
import { PortalCard } from '../components/PortalCard';

type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'n';
type FTab = 'ov' | 'personal' | 'bank' | 'tax' | 'attendance' | 'nom' | 'uni' | 'pay' | 'life' | 'hst';
const FTABS: [FTab, string][] = [['ov', 'Overview'], ['personal', 'Personal'], ['bank', 'Bank'], ['tax', 'Tax'], ['attendance', 'Attendance'], ['nom', 'Nominations'], ['uni', 'Uniform'], ['pay', 'Pay and history'], ['life', 'Status timeline']];
/** Prototype STP: readiness state → pill. */
const STP: Record<string, [string, Tone]> = { ready: ['Ready', 'ok'], pending: ['Waiting', 'warn'], issue: ['Fix needed', 'bad'], hold: ['Salary held', 'bad'], na: ['Not applicable', 'n'] };
/** The tab each readiness item opens. */
const ITEM_TAB: Record<string, FTab> = { personal: 'personal', statutory: 'personal', bank: 'bank', hold: 'pay', tax: 'tax', approvals: 'attendance', final: 'attendance' };
const WF: Record<string, string> = { leave: 'Leave', regularize: 'Regularization', transfer: 'Transfer', resign: 'Separation', p_claim: 'Claim', p_adv: 'Salary advance',
  p_ded: 'Deduction', p_arrear: 'Arrear', p_change: 'Bank or profile change', p_fnf: 'Full and final', p_ack: 'Company policy' };
const ECREL = ['Spouse', 'Father', 'Mother', 'Son', 'Daughter', 'Brother', 'Sister', 'Friend', 'Other relative'];
const PLAB: Record<string, string> = { mobile: 'Mobile', email: 'Email', address: 'Address', ecName: 'Emergency contact', ecRelation: 'Relation', ecMobile: 'Emergency mobile' };

const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });
const prevYm = (ym: string) => { const d = new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 2, 1); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`; };
const mask = (v?: string | null) => (v ? String(v).replace(/\s/g, '').replace(/.(?=.{4})/g, '•') : '—');
const rnd2 = (n: number) => Math.round(n * 100) / 100;
const reqTone = (s: string): Tone => (s === 'Approved' ? 'ok' : s === 'Rejected' ? 'bad' : s === 'Pending' ? 'warn' : 'n');
/** The role sees salary figures in full (Access control, confidential data: Salary = Full). */
const salFull = (me: Me | null | undefined) => (me?.ctx.sensitive?.salary ?? 'Full') === 'Full';
/** Prototype flowNote. */
const FlowNote = ({ children }: { children: ReactNode }) => <div className="note" style={{ marginBottom: 14 }}><b>How this reaches payroll.</b> {children}</div>;

/** The employee payroll file (prototype vFile): find a person, then readiness, personal, bank, tax, attendance, nominations and pay. */
export default function EmployeeFile() {
  const [qs, setQs] = useSearchParams();
  const id = qs.get('id') ?? '';
  const [tab, setTab] = useState<FTab>('ov');
  const [m, setM] = useState('');
  const [open, setOpen] = useState('');
  const d = useLoad(() => (id ? api(`/employees/${id}/file${m ? `?ym=${m}` : ''}`) : Promise.resolve(null)), [id, m]);
  useEffect(() => { if (d.data && !m) setOpen(d.data.ym); }, [d.data, m]);
  const pick = (x: string) => { setTab('ov'); setM(''); setQs(x ? { id: x } : {}); };
  return (
    <>
      <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ gap: 10, flexWrap: 'wrap', alignItems: 'center' }}>
        <PersonPicker label="Employee" value={id} selected={d.data?.employee?.ID === id ? { id, name: d.data.employee.Name } : null} onChange={(x) => pick(x)} placeholder="Find an employee by name, code or place"
          style={{ minWidth: 0, flex: '1 1 300px', maxWidth: 460 }} />
      </div></div>
      {!id ? <div className="card"><h3 style={{ margin: '0 0 6px' }}>Employee payroll file</h3><p className="ink2" style={{ margin: 0 }}>Pick an employee to see everything payroll needs about them in one place: personal details, bank, tax, attendance and leave requests, and what will be paid. You can also open it from the Employees list or from a row in the payroll run.</p></div>
        : !d.data ? <Loading error={d.error} />
        : <File key={id} x={d.data} tab={tab} setTab={setTab} months={[open || d.data.ym, prevYm(open || d.data.ym)]} onMonth={setM} reload={d.reload} />}
    </>
  );
}

function File({ x, tab, setTab, months, onMonth, reload }: { x: any; tab: FTab; setTab: (t: FTab) => void; months: string[]; onMonth: (ym: string) => void; reload: () => void }) {
  const id = x.employee.ID, ym = x.ym as string;
  const rec = useLoad(() => api(`/employees/${id}`), [id]);
  const f = x.employee, e = rec.data;
  const { can } = useAuth();
  const tabs: [FTab, string][] = can('hst.allot') ? [...FTABS, ['hst', 'Accommodation']] : FTABS;
  return <>
    <div className="card" style={{ marginBottom: 14 }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 12 }}>
      <div className="row" style={{ gap: 14 }}>
        <div style={{ width: 48, height: 48, borderRadius: '50%', background: 'var(--ink,#111)', color: '#fff', display: 'flex', alignItems: 'center', justifyContent: 'center', fontWeight: 700, fontSize: 18 }}>{f.Name.charAt(0)}</div>
        <div><h2 style={{ margin: 0, fontSize: 19 }}>{f.Name} <Pill k={f.Status === 'Active' ? 'ok' : 'n'}>{f.Status}</Pill></h2>
          <div className="mute" style={{ fontSize: 13 }}>{f.ID} · {f.Type} · {f.Desig}, {f.Dept} · {f.Location}{e ? `, ${e.city}` : ''}</div>
          {e?.hireIds && <div className="mute mono" style={{ fontSize: 12 }}>{e.hireIds.si} · {e.hireIds.oi} · {e.hireIds.ji}</div>}</div>
      </div>
      <div style={{ textAlign: 'right' }}><div className="mute" style={{ fontSize: 12 }}>Payroll month</div>
        <select value={ym} onChange={(ev) => onMonth(ev.target.value === months[0] ? '' : ev.target.value)} aria-label="Payroll month">{months.map((mo) => <option key={mo} value={mo}>{ymLabel(mo)}</option>)}</select>{' '}
        <span style={{ marginLeft: 6 }}><Pill k={x.overall[1]}>{x.overall[0]}</Pill></span></div>
    </div></div>
    <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}</button>)}</div>
    {tab === 'ov' ? <><Overview x={x} e={e} setTab={setTab} />{can('att.portal') && <PortalCard id={id} />}</>
      : !e ? <Loading error={rec.error} />
      : tab === 'personal' ? <Personal x={x} e={e} reload={() => { rec.reload(); reload(); }} />
      : tab === 'bank' ? <Bank x={x} e={e} reload={() => { rec.reload(); reload(); }} />
      : tab === 'tax' ? <Tax x={x} />
      : tab === 'attendance' ? <Attendance x={x} />
      : tab === 'nom' ? <Nominations x={x} />
      : tab === 'uni' ? <div className="card"><h3>Uniform sizes</h3><UniformBlock id={id} heading={false} /></div>
      : tab === 'life' ? <EmployeeLifecycle id={id} />
      : tab === 'hst' ? <EmployeeAccommodation id={id} name={f.Name} />
      : <PayTab x={x} e={e} />}
  </>;
}

/** Request details (chain, stage, people, history) for the listed request ids. */
function useReqs(ids: string[]) {
  const key = ids.join(',');
  return useLoad(() => Promise.all(ids.map((i) => api(`/approvals/${i}`).catch(() => null))).then((l) => Object.fromEntries(l.filter(Boolean).map((r: any) => [r.id, r]))), [key]);
}
const nowWith = (r: any) => (r && r.status === 'Pending' ? `${r.chain?.[r.step] ?? ''}${r.people?.[r.step]?.person ? ' · ' + r.people[r.step].person : ''}` : '—');
/** A request's title and detail line, as the prototype lists them ("Casual leave, 2 days" / "30 Oct 2026 to 31 Oct 2026"). */
const title = (wf: string, p: any) => {
  if (wf === 'leave') { const n = p.half ? 0.5 : p.cov?.length ?? 0; return `${p.ltype || 'Leave'}${n ? `, ${n} day${n === 1 ? '' : 's'}` : ''}`; }
  return wf === 'regularize' ? `Regularization, ${(p.rtype || '').toLowerCase()}` : WF[wf] ?? wf;
};
const meta = (wf: string, p: any) => wf === 'leave' ? `${fmtD(p.from)} to ${fmtD(p.to)}` : wf === 'regularize' ? `${fmtD(p.date)}${p.reason ? ' · ' + p.reason : ''}`
  : wf === 'p_change' ? p.kind : wf === 'p_ack' ? `${p.policyTitle}, version ${p.version}` : p.amount ? inr(p.amount) : '';
const bankText = (b: any) => (b ? `${b.bank_name}${b.account_no ? ' ••••' + String(b.account_no).slice(-4) : ''}` : '');

function Overview({ x, e, setTab }: { x: any; e: any; setTab: (t: FTab) => void }) {
  const { can, me } = useAuth();
  const r = x.row;
  const pend = (x.requests as any[]).filter((q) => q.status === 'Pending');
  const det = useReqs(pend.map((q) => q.id));
  const tax = useLoad(() => (can('pay.run') || can('pay.claims') ? api(`/tax/employee/${x.employee.ID}`).catch(() => null) : Promise.resolve(null)), [x.employee.ID]);
  const hist = useLoad(() => api<any[]>(`/employees/${x.employee.ID}/history`).catch(() => []), [x.employee.ID]);
  const area = (q: any) => q.wf === 'p_change' ? (q.payload.kind === 'Bank' ? 'Bank' : 'Personal') : q.wf === 'leave' ? 'Leave' : q.wf === 'regularize' ? 'Attendance'
    : q.wf === 'p_ack' ? 'Policy' : q.wf === 'p_claim' ? 'Claim' : q.wf === 'p_arrear' ? 'Arrear' : q.wf === 'p_adv' ? 'Advance' : 'Request';
  const items: { area: string; text: string; who: string }[] = pend.map((q) => ({ area: area(q), who: det.data?.[q.id] ? nowWith(det.data[q.id]) : 'Approvers',
    text: q.wf === 'p_change' ? `${q.payload.kind} change requested ${q.created}` : `${title(q.wf, q.payload)}${meta(q.wf, q.payload) ? ' · ' + meta(q.wf, q.payload) : ''}` }));
  const names: Record<string, string> = Object.fromEntries(((tax.data?.items ?? []) as any[]).map((i) => [i.id, i.name]));
  ((tax.data?.proofs ?? []) as any[]).filter((p) => p.status === 'Pending').forEach((p) => items.push({ area: 'Tax', text: `Proof for ${names[p.item] ?? p.item}, ${inr(p.amount)}`, who: 'Payroll' }));
  (hist.data ?? []).filter((h) => h.state === 'Scheduled' && /gross/i.test(h.field)).forEach((h) => items.push({ area: 'Salary', text: `Revision to ${h.new_value} from ${fmtD(h.effective_from)}`, who: 'Scheduled' }));
  // Prototype sv('salary', …): figures only for a role that sees salaries in full (a masked amount says nothing).
  const hidden = (v: number | undefined) => (r && salFull(me) ? inr(v) : 'Hidden');
  return <div className="grid g2" style={{ alignItems: 'start' }}>
    <div className="card"><h3>Ready for {ymLabel(x.ym)} payroll?</h3>
      <div style={{ display: 'grid', gap: 2 }}>{(x.items as any[]).map((i) => <div key={i.key} style={{ display: 'grid', gridTemplateColumns: '1fr auto', gap: 10, padding: '10px 0', borderTop: '1px solid var(--line,#e5e7eb)' }}>
        <div><b>{i.title}</b><div className="ink2" style={{ fontSize: 12.5, marginTop: 2 }}>{i.text}</div>
          <div className="mute" style={{ fontSize: 12, marginTop: 2 }}>{i.owner === '—' || i.state === 'ready' || i.state === 'na' ? '' : 'Next step with ' + i.owner}</div></div>
        <div style={{ textAlign: 'right' }}><Pill k={(STP[i.state] ?? STP.na)[1]}>{(STP[i.state] ?? STP.na)[0]}</Pill><div><button className="btn sm" style={{ marginTop: 6 }} onClick={() => setTab(ITEM_TAB[i.key] ?? 'ov')}>Open</button></div></div>
      </div>)}</div>
    </div>
    <div style={{ display: 'grid', gap: 14 }}>
      <div className="card"><h3>What will be paid</h3>
        {r?.hold && <div className="note bad" style={{ marginBottom: 10 }}>Salary is on hold, so this amount is not in the payable total.</div>}
        {r ? <dl className="kv"><dt>Paid days</dt><dd>{r.paid} of {r.days}</dd><dt>Earnings</dt><dd>{hidden(r.totalEarn)}</dd><dt>Deductions</dt><dd>{hidden(r.totalDed)}</dd>
          <dt><b>Net pay</b></dt><dd><b>{hidden(r.net)}</b></dd><dt>Paid to</dt><dd>{e ? (e.bank ? bankText(e.bank) : e.bank === undefined ? 'Hidden' : '—') : '…'}</dd></dl>
          : <p className="mute" style={{ margin: 0 }}>Not in the {ymLabel(x.ym)} payroll.</p>}
      </div>
      <div className="card"><h3>Open items</h3>
        {items.length ? <ul className="log" style={{ margin: 0 }}>{items.map((o, i) => <li key={i}><Pill k="n">{o.area}</Pill> {o.text}<div className="mute" style={{ fontSize: 12 }}>With {o.who}</div></li>)}</ul>
          : <p className="mute" style={{ margin: 0 }}>Nothing is waiting on anyone.</p>}
      </div>
      <div><EffectivePolicyCard id={x.employee.ID} /><EmployeeAccessCard id={x.employee.ID} name={x.employee.Name} /></div>
    </div>
  </div>;
}

/** Change requests (p_change) of one kind, with who asked and who verified. */
function ChangeList({ x, kind, render }: { x: any; kind: 'Personal' | 'Bank'; render: (p: any) => ReactNode }) {
  const { can } = useAuth();
  const nav = useNavigate();
  const list = (x.requests as any[]).filter((q) => q.wf === 'p_change' && q.payload?.kind === kind);
  const det = useReqs(list.map((q) => q.id));
  if (!list.length) return <p className="mute" style={{ margin: 0 }}>No change requests yet.</p>;
  return <ul className="log">{list.map((q) => {
    const r = det.data?.[q.id], v = r?.history?.filter((h: any) => h.action === 'Approved').pop();
    const by = q.raised_by_name ?? r?.raised_by_name ?? (r ? r.raised_by : '…');
    return <li key={q.id}><Pill k={reqTone(q.status)}>{q.status}</Pill> {render(q.payload)}
      <div className="mute" style={{ fontSize: 12 }}>Asked by {by}, {q.created}{v && q.status === 'Approved' ? ` · verified by ${v.by_name}, ${fmtD(v.at)}` : ''}</div>
      {q.status === 'Pending' && can('pay.claims', 2) && <div className="row" style={{ marginTop: 6 }}><button className="btn sm primary" onClick={() => nav('/approvals')}>Review in Approvals</button></div>}</li>;
  })}</ul>;
}

function Personal({ x, e, reload }: { x: any; e: any; reload: () => void }) {
  const { can } = useAuth();
  const [chg, setChg] = useState(false);
  const ed = can('emp.personal', 2) || can('pay.claims', 2);
  const p = e.personal, g = e.govId;
  const sv = (has: boolean, v: string) => (has ? v : 'Hidden');
  return <>
    <FlowNote>The employee or HR raises a change. Payroll verifies it, and the new value is used from that moment. Every change stays on record with who asked, who verified and when.</FlowNote>
    <div className="grid g2" style={{ alignItems: 'start' }}>
      <div className="card"><div className="row sp"><h3 style={{ margin: 0 }}>Current details</h3>{ed && <button className="btn" onClick={() => setChg(true)}>Raise a change</button>}</div>
        <dl className="kv" style={{ marginTop: 10 }}>
          <dt>Date of birth</dt><dd>{sv(!!p, fmtD(p?.dob))}</dd><dt>Mobile</dt><dd>{sv(!!p, p?.mobile || '—')}</dd><dt>Email</dt><dd>{sv(!!p, p?.email || '—')}</dd>
          <dt>Address</dt><dd>{sv(!!p, p?.address || '—')}</dd><dt>Emergency contact</dt><dd>{sv(!!p, p?.ec_name ? `${p.ec_name} (${p.ec_relation || '—'}) · ${p.ec_mobile || '—'}` : '—')}</dd>
          <dt>PAN</dt><dd>{sv(!!g, mask(g?.pan))}</dd><dt>Aadhaar</dt><dd>{sv(!!g, mask(g?.aadhaar))}</dd>
          <dt>Reports to</dt><dd>{e.manager ?? '—'}</dd><dt>Date of joining</dt><dd>{fmtD(e.doj)}</dd>
        </dl></div>
      <div className="card"><h3>Change history</h3>
        <ChangeList x={x} kind="Personal" render={(d) => Object.entries(PLAB).filter(([k]) => d[k]).map(([k, l]) => `${l}: ${d[k]}`).join(' · ')} /></div>
    </div>
    {chg && <ChangeModal kind="Personal" e={e} onClose={() => setChg(false)} onDone={() => { setChg(false); reload(); }} />}
  </>;
}

function Bank({ x, e, reload }: { x: any; e: any; reload: () => void }) {
  const { can } = useAuth();
  const [chg, setChg] = useState(false);
  const ed = can('emp.bank', 2) || can('pay.claims', 2);
  const hold = (x.requests as any[]).some((q) => q.wf === 'p_change' && q.payload?.kind === 'Bank' && q.status === 'Pending');
  return <>
    <FlowNote>A new account is requested by the employee or HR and goes to Payroll. <b>Salary stays on hold until Payroll verifies it.</b> Once verified, the account is used from that payroll month onward; earlier months keep the old account.</FlowNote>
    {hold && <div className="note bad" style={{ marginBottom: 14 }}>A bank change is waiting for verification. This employee is held out of the payable total.</div>}
    <div className="grid g2" style={{ alignItems: 'start' }}>
      <div className="card"><div className="row sp"><h3 style={{ margin: 0 }}>Account used for {ymLabel(x.ym)}</h3>{ed && <button className="btn" onClick={() => setChg(true)}>Change bank account</button>}</div>
        <p style={{ margin: '12px 0 0', fontSize: 15 }}><b>{e.bank === undefined ? 'Hidden' : e.bank ? bankText(e.bank) : '—'}</b></p>
        <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 0' }}>{hold ? 'A new account is pending.' : 'Verified.'}</p>
        {(e.bankHistory as any[] | undefined)?.length ? <><h3 style={{ margin: '16px 0 8px' }}>Account history</h3><div className="tw"><table><thead><tr><th>From payroll month</th><th>Account</th></tr></thead><tbody>
          {(e.bankHistory as any[]).map((h, i) => <tr key={i}><td>{h.from_month === '1900-01' ? 'Earlier' : ymLabel(h.from_month)}</td><td>{h.account_no == null ? 'Hidden' : bankText(h)}</td></tr>)}
        </tbody></table></div></> : null}
      </div>
      <div className="card"><h3>Change requests</h3>
        <ChangeList x={x} kind="Bank" render={(d) => `${d.bankName} · A/c ${mask(d.accountNo)} · IFSC ${d.ifsc}`} /></div>
    </div>
    {chg && <ChangeModal kind="Bank" e={e} onClose={() => setChg(false)} onDone={() => { setChg(false); reload(); }} />}
  </>;
}

/** Prototype fileChange: a personal or bank change raised for the employee, verified by Payroll. */
function ChangeModal({ kind, e, onClose, onDone }: { kind: 'Personal' | 'Bank'; e: any; onClose: () => void; onDone: () => void }) {
  const p = e.personal || {};
  const [v, setV] = useState<Record<string, string>>(kind === 'Bank' ? { bankName: '', accountNo: '', ifsc: '' }
    : { mobile: p.mobile || '', email: p.email || '', address: p.address || '', ecName: p.ec_name || '', ecRelation: p.ec_relation || '', ecMobile: p.ec_mobile || '' });
  const [err, setErr] = useState('');
  const F = (k: string, l: string, full?: boolean, ph?: string) => <label className={`field${full ? ' full' : ''}`}><span>{l}</span><input type="text" value={v[k]} placeholder={ph} onChange={(x) => setV({ ...v, [k]: x.target.value })} /></label>;
  const send = async () => {
    setErr('');
    if (kind === 'Bank' && (!v.bankName.trim() || !/^\d{6,18}$/.test(v.accountNo.trim()) || !/^[A-Z]{4}0[A-Z0-9]{6}$/.test(v.ifsc.trim().toUpperCase()))) { setErr('Check the bank name, account number and IFSC'); return; }
    try {
      await api('/approvals', { body: { wf: 'p_change', employeeId: e.id, payload: kind === 'Bank' ? { kind, bankName: v.bankName.trim(), accountNo: v.accountNo.trim(), ifsc: v.ifsc.trim().toUpperCase() }
        : { kind, ...Object.fromEntries(Object.entries(v).map(([k, x]) => [k, x.trim()])) } } });
      toast('Sent to Payroll for verification'); onDone();
    } catch (x: any) { setErr(x.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>{kind === 'Bank' ? 'Change bank account' : 'Change personal details'}: {e.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    {kind === 'Bank' ? <>
      <div className="form" style={{ marginTop: 12 }}>{F('bankName', 'Bank name')}{F('accountNo', 'Account number')}{F('ifsc', 'IFSC')}</div>
      <div className="note bad" style={{ marginTop: 12, fontSize: 12.5 }}>Salary is held until Payroll verifies the new account.</div>
    </> : <div className="form" style={{ marginTop: 12 }}>
      {F('mobile', 'Mobile')}{F('email', 'Email')}{F('address', 'Address', true)}{F('ecName', 'Emergency contact name')}
      <label className="field"><span>Relation</span><select value={v.ecRelation} onChange={(x) => setV({ ...v, ecRelation: x.target.value })}><option value="">Select</option>{ECREL.map((r) => <option key={r}>{r}</option>)}</select></label>
      {F('ecMobile', 'Emergency contact mobile', false, '10 digit number')}
    </div>}
    {err && <div className="note bad" role="alert" style={{ marginTop: 12 }}>{err}</div>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={send}>Send to Payroll</button></div>
  </Modal>;
}

function Tax({ x }: { x: any }) {
  const { can } = useAuth();
  const nav = useNavigate();
  const t = useLoad(() => api(`/tax/employee/${x.employee.ID}`), [x.employee.ID]);
  const f = x.employee;
  if (f.Type !== 'Permanent') return <div className="note">{f.Type} staff do not declare tax with the company. Tax, if any, is deducted as per the contract.</div>;
  if (!t.data) return <Loading error={t.error} />;
  const d = t.data, w = d.windows, dec = d.declaration, items = (dec?.items ?? {}) as Record<string, number>;
  const win = (a: string, b: string) => `${fmtD(a)} to ${fmtD(b)}`;
  const declWin = win(w.declFrom, w.declTo), proofWin = win(w.proofFrom, w.proofTo);
  if (!dec || (dec.status === 'Draft' && !Object.values(items).some((v) => v > 0)))
    return <><FlowNote>The employee declares a tax regime and deductions in My pay and profile while the declaration window is open. Payroll reviews the proofs in the proof window, and only approved amounts count in the final tax.</FlowNote>
      <div className="card"><h3>Not declared</h3><p className="ink2" style={{ margin: 0 }}>{f.Name} has not declared yet. The new regime applies until they do. Declaration window: {declWin} ({w.declOpen ? 'open' : 'closed'}).</p></div></>;
  const proofs = d.proofs as any[];
  const appr = (id: string) => proofs.filter((p) => p.item === id && p.status === 'Approved').reduce((s, p) => s + (p.approved ?? p.amount ?? 0), 0);
  const decl = Object.values(items).reduce((s, v) => s + (+v || 0), 0), apprAll = (d.items as any[]).reduce((s, i) => s + appr(i.id), 0);
  const st: [string, Tone] = dec.finalAt ? ['Finalised', 'ok'] : dec.status === 'Draft' ? ['Not submitted', 'n'] : proofs.some((p) => p.status === 'Pending') ? ['Proofs to review', 'warn']
    : proofs.some((p) => p.status === 'Approved') ? ['Proofs reviewed', 'info'] : ['Declared', 'info'];
  const old = dec.regime === 'Old', fig = d.figure;
  const mine = (b: any) => (b ? (old ? b.oldTax : b.newTax) : 0);
  return <>
    <FlowNote>Declaration window {declWin}, proof window {proofWin}. Tax is projected on the declaration. After Payroll reviews proofs, only approved amounts count, and any shortfall is recovered from the last months of the year.</FlowNote>
    <div className="grid g3" style={{ marginBottom: 14 }}>
      <div className="card stat"><div className="l">Regime</div><div className="n" style={{ fontSize: 20 }}>{dec.regime}</div><div><Pill k={st[1]}>{st[0]}</Pill></div></div>
      <div className="card stat"><div className="l">Declared · Approved</div><div className="n" style={{ fontSize: 20 }}>{inr(decl)} · {inr(apprAll)}</div></div>
      <div className="card stat"><div className="l">Yearly tax: declared · on approved proofs</div><div className="n" style={{ fontSize: 20 }}>{inr(mine(fig?.declared))} · {inr(mine(fig?.approved))}</div></div>
    </div>
    <div className="card"><div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>Declared items</h3>{can('pay.claims', 2) && <button className="btn" onClick={() => nav('/payroll')}>Review proofs</button>}</div>
      <div className="tw"><table><thead><tr><th>Item</th><th className="num">Declared</th><th className="num">Approved</th><th>Proofs</th></tr></thead><tbody>
        {(d.items as any[]).filter((i) => (items[i.id] || 0) > 0).map((i) => { const ps = proofs.filter((p) => p.item === i.id); return <tr key={i.id}><td>{i.name}</td><td className="num">{inr(items[i.id])}</td><td className="num">{inr(appr(i.id))}</td>
          <td>{ps.length ? ps.map((p) => <Pill key={p.id} k={p.status === 'Approved' ? 'ok' : p.status === 'Rejected' ? 'bad' : 'warn'}>{p.status}</Pill>) : <span className="mute">None</span>}</td></tr>; })}
        {!Object.values(items).some((v) => v > 0) && <tr><td colSpan={4} className="mute">No deductions declared.</td></tr>}
      </tbody></table></div>
      {dec.finalAt && <div className="note" style={{ marginTop: 12 }}>Finalised {fmtD(dec.finalAt)}. Monthly TDS now uses approved proofs{dec.finalDiff > 0 ? `, so ${inr(dec.finalDiff)} more tax is spread over the months left` : ''}.</div>}
    </div>
  </>;
}

const CODE: Record<string, string> = { Present: 'P', Late: 'LT', 'Half day': 'H', 'No punch': 'N', 'Week off': 'W', Holiday: 'PH', 'On leave': 'L' };
const dayCode = (s: string) => CODE[s] ?? (/^Absent/.test(s) ? 'A' : /^Missing/.test(s) ? 'M' : /leave/i.test(s) ? 'L' : '');

function Attendance({ x }: { x: any }) {
  const ym = x.ym as string, r = x.row, id = x.employee.ID;
  const a = useLoad(() => api(`/attendance?ym=${ym}&employee=${id}`), [ym, id]);
  const reqs = (x.requests as any[]).filter((q) => (q.wf === 'regularize' && String(q.payload?.date).slice(0, 7) === ym) || (q.wf === 'leave' && String(q.payload?.from).slice(0, 7) <= ym && String(q.payload?.to).slice(0, 7) >= ym));
  const det = useReqs(reqs.map((q) => q.id));
  const c: Record<string, number> = r?.counts ?? a.data?.counts ?? {};
  const n = new Date(+ym.slice(0, 4), +ym.slice(5, 7), 0).getDate(), first = new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).getDay();
  const byDate: Record<string, any> = Object.fromEntries(((a.data?.days ?? []) as any[]).map((d) => [d.date, d]));
  const doj = x.employee.DOJ as string, lwd = x.employee.LWD as string | undefined;
  const on = (q: any, d: string) => (q.wf === 'regularize' ? q.payload?.date === d : q.payload?.from <= d && q.payload?.to >= d);
  const all = x.requests as any[];
  const pendOn = (d: string) => all.some((q) => q.status === 'Pending' && (q.wf === 'leave' || q.wf === 'regularize') && on(q, d));
  const regOn = (d: string) => all.some((q) => q.status === 'Approved' && q.wf === 'regularize' && on(q, d));
  const days = Array.from({ length: n }, (_, i) => {
    const d = `${ym}-${String(i + 1).padStart(2, '0')}`, v = byDate[d];
    if (doj && d < doj) return { d, s: 'Before joining', tone: 'n', code: '–' };
    if (lwd && d > lwd) return { d, s: 'After last working day', tone: 'n', code: '–' };
    if (!v) return { d, s: 'Upcoming', tone: 'n', code: '' };
    return { d, s: v.status, tone: v.status === 'On leave' ? 'info' : v.tone, code: dayCode(v.status), v };
  });
  return <>
    <FlowNote>Punches come from the attendance machine or app. Gaps are fixed by regularization and leave requests, which follow the approval flow. When Payroll locks attendance, pending requests are closed and the paid days below become final.</FlowNote>
    <div className="grid g4" style={{ marginBottom: 14 }}>
      <div className="card stat"><div className="l">Paid days</div><div className="n">{r ? r.paid : '—'} <span className="mute" style={{ fontSize: 14 }}>of {r ? r.days : n}</span></div></div>
      <div className="card stat"><div className="l">Loss of pay days</div><div className="n">{r ? rnd2(r.days - r.paid) : '—'}</div><div className="mute" style={{ fontSize: 12 }}>System calculated {r ? r.lop : '—'}</div></div>
      <div className="card stat"><div className="l">Present · Late · Half day</div><div className="n" style={{ fontSize: 20 }}>{c.Present || 0} · {c.Late || 0} · {c['Half day'] || 0}</div></div>
      <div className="card stat"><div className="l">Attendance</div><div className="n" style={{ fontSize: 18 }}>{x.locked ? 'Locked' : 'Open'}</div><div className="mute" style={{ fontSize: 12 }}>{c['Missing punch'] || 0} missing punch · {c['No punch'] || 0} no punch</div></div>
    </div>
    <div className="card" style={{ marginBottom: 14 }}><h3>{ymLabel(ym)}</h3>
      {!a.data ? <Loading error={a.error} /> : <div className="fg">
        {['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].map((w) => <div key={w} className="fh">{w}</div>)}
        {Array.from({ length: first }, (_, i) => <div key={'b' + i} />)}
        {days.map((x) => { const pd = pendOn(x.d), rg = regOn(x.d);
          return <div key={x.d} className={`fc ${x.tone}`} title={`${x.s}${x.v?.in ? ' · In ' + x.v.in + (x.v.out ? ', out ' + x.v.out : '') : ''}${rg ? ' · Regularized' : ''}${pd ? ' · Request pending' : ''}`}>
            <b>{+x.d.slice(8)}</b>{pd && <i className="dot" />}<span className="cd">{x.code}{rg && <small style={{ fontWeight: 500 }}> R</small>}</span></div>; })}
      </div>}
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>P present · LT late · H half day · M missing punch · N no punch (loss of pay) · W week off · L approved leave · LOP loss of pay leave · R regularized · orange dot: a request is pending for that day.</p>
    </div>
    <div className="card"><h3>Regularization and leave requests</h3>
      <div className="tw"><table><thead><tr><th>Request</th><th>Details</th><th>Status</th><th>Now with</th><th>Last action</th></tr></thead><tbody>
        {reqs.map((q) => { const dd = det.data?.[q.id], h = dd?.history?.[dd.history.length - 1];
          return <tr key={q.id}><td className="mono">{q.id}</td><td>{title(q.wf, q.payload)}<div className="mute" style={{ fontSize: 12 }}>{meta(q.wf, q.payload)}</div></td><td><Pill k={reqTone(q.status)}>{q.status}</Pill></td>
            <td>{nowWith(dd ?? q)}</td><td className="mute" style={{ fontSize: 12.5 }}>{h ? `${h.action} by ${h.by_name}, ${fmtD(h.at)}` : `Raised ${q.created}`}</td></tr>; })}
        {!reqs.length && <tr><td colSpan={5} className="mute">No requests for this month.</td></tr>}
      </tbody></table></div>
    </div>
  </>;
}

/** Nominations on record (prototype fNom): every nomination that applies, its current version and the earlier ones
 *  (GET /api/employees/{id}/nominations). */
function Nominations({ x }: { x: any }) {
  const { can, me } = useAuth();
  const f = x.employee;
  const q = useLoad(() => api<any[]>(`/employees/${f.ID}/nominations`), [f.ID]);
  const [busy, setBusy] = useState(false);
  const [ret, setRet] = useState<{ id: number; note: string } | null>(null);
  if (!q.data) return q.error ? <div className="note">{q.error}.</div> : <Loading />;
  const list = q.data;
  if (!list.length) return <div className="card"><h3>Nominations</h3><p className="mute" style={{ margin: 0 }}>No nominations are needed for {String(f.Type).toLowerCase()} staff.</p></div>;
  const act = async (nid: number, a: 'verify' | 'return') => {
    if (a === 'return' && !ret?.note.trim()) { toast('Write what the employee should correct'); return; }
    setBusy(true);
    try { await api(`/nominations/${nid}/${a}`, { body: a === 'return' ? { note: ret!.note.trim() } : {} }); toast(a === 'verify' ? 'Verified' : 'Returned to the employee'); setRet(null); q.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const ed = can('pay.claims', 2) && me?.ctx.employeeId !== f.ID;
  return <>
    <div className="note" style={{ marginBottom: 14 }}><b>Nominations on record.</b> Provident fund, gratuity and insurance nominees as submitted by the employee. Payroll verifies them against the documents.</div>
    {list.map((n) => {
      const s = n.scheme, c = n.current, old = (n.versions as any[]).filter((v) => v.id !== c?.id);
      const st: [string, Tone] = n.state === 'Verified' ? ['Verified', 'ok'] : n.state === 'Waiting for Payroll' ? ['Waiting for Payroll', 'warn'] : n.state === 'Returned' ? ['Returned to you', 'bad']
        : ['Not submitted', n.due && n.due < today() ? 'bad' : 'warn'];
      return <div key={s.id} className="card" style={{ marginBottom: 14 }}>
        <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}>
          <div><h3 style={{ margin: 0 }}>{s.name}</h3><div className="mute" style={{ fontSize: 12.5 }}>{s.mandatory ? 'Required' : 'Optional'}{n.due ? ', due ' + fmtD(n.due) : ''}{c ? ` · version ${c.ver}, submitted ${fmtD(c.submitted_at)}` : ''}</div></div>
          <div className="row" style={{ gap: 8 }}><Pill k={st[1]}>{st[0]}</Pill>{c?.status === 'Submitted' && ed && <><button className="btn sm primary" disabled={busy} onClick={() => act(c.id, 'verify')}>Verify</button><button className="btn sm" disabled={busy} onClick={() => (ret?.id === c.id ? act(c.id, 'return') : setRet({ id: c.id, note: '' }))}>Return</button></>}</div>
        </div>
        {c && ret?.id === c.id && <input type="text" autoFocus style={{ marginTop: 10 }} placeholder="What should the employee correct? Then press Return again" value={ret?.note ?? ''} onChange={(ev) => setRet({ id: c.id, note: ev.target.value })} />}
        {c ? <><div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Name</th><th>Relation</th><th>Date of birth</th>{s.shares && <th className="num">Share</th>}<th>Guardian</th></tr></thead><tbody>
          {(c.people as any[]).map((p, i) => <tr key={i}><td>{p.name ?? 'Hidden'}</td><td>{p.rel}</td><td>{p.dob ? (/^\d{4}-/.test(p.dob) ? fmtD(p.dob) : p.dob) : 'Hidden'}</td>{s.shares && <td className="num">{p.share}%</td>}<td>{p.guardian || '—'}</td></tr>)}
        </tbody></table></div><div className="mute" style={{ fontSize: 12.5, marginTop: 6 }}>Documents attached: {n.docs?.have ?? 0} of {n.docs?.need ?? 0}{c.note ? ' · Note: ' + c.note : ''}</div></>
          : <p className="mute" style={{ margin: '10px 0 0' }}>Not submitted yet.</p>}
        {old.length > 0 && <div className="mute" style={{ fontSize: 12, marginTop: 8 }}>Earlier versions: {old.map((o) => `v${o.ver} (${String(o.status).toLowerCase()})`).join(', ')}</div>}
      </div>;
    })}
  </>;
}

function PayTab({ x, e }: { x: any; e: any }) {
  const { can, me } = useAuth();
  const [slip, setSlip] = useState(false);
  const r = x.row, ym = x.ym as string;
  const hist = useLoad(() => api<any[]>(`/employees/${x.employee.ID}/history`).catch(() => []), [x.employee.ID]);
  const own = (can('emp.salary') || can('pay.run')) && salFull(me);
  const fut = (x.lines as any[]).filter((l) => l.ym > ym);
  const rev = (hist.data ?? []).filter((h) => /gross|structure/i.test(h.field));
  const sMap = ((e.structureHistory ?? []) as { name: string; from_date: string }[]).filter((m) => m.from_date > '1901');
  const L = (label: string, v: number, note?: string, neg?: boolean) => v ? <tr key={label}><td>{label}{note && <div className="mute" style={{ fontSize: 12 }}>{note}</div>}</td><td className="num">{neg ? '−' : ''}{inr(v)}</td></tr> : null;
  return <>
    <FlowNote>Salary revisions, structure changes and recoveries each carry an effective month. Payroll for a month uses the values effective in that month, and a revision effective in an earlier month creates an arrear for approval.</FlowNote>
    {!own ? <div className="note">Your role cannot see salary details.</div> : <div className="grid g2" style={{ alignItems: 'start' }}>
      <div className="card"><div className="row sp"><h3 style={{ margin: 0 }}>{ymLabel(ym)} calculation</h3>{x.employee.Type === 'Permanent' && <button className="btn sm" onClick={() => setSlip(true)}>Payslip preview</button>}</div>
        {!r ? <p className="mute" style={{ margin: '8px 0 0' }}>Not in the {ymLabel(ym)} payroll.</p> : <div className="tw" style={{ marginTop: 8 }}><table><tbody>
          {L('Retainer fee', r.r.fee, 'Section 194J')}{L('Basic', r.r.basic)}{L('House rent allowance', r.r.hra)}{L('Special allowance', r.r.sa)}{L('Bonus', r.r.bonus)}{L('Variable pay', r.r.varPay)}
          {(r.add ?? []).map((a: any, i: number) => <tr key={'a' + i}><td>{a.label}{a.reason && <div className="mute" style={{ fontSize: 12 }}>{a.reason}</div>}</td><td className="num">{inr(a.amount)}</td></tr>)}
          {L('Other earning', r.oe, r.reason)}
          <tr><td><b>Total earnings</b></td><td className="num"><b>{inr(r.totalEarn)}</b></td></tr>
          {L('Provident fund', r.r.eePF, undefined, true)}{L('ESI', r.r.eeESI, undefined, true)}{L('Labour welfare fund', r.r.lwfEe, undefined, true)}{L('Meal', r.r.meal, undefined, true)}
          {L('Accommodation', r.r.accom, undefined, true)}{L('TDS', r.r.tds, 'Section 194J', true)}{L('Income tax (TDS)', r.stds, 'Section 192', true)}
          {(r.ded ?? []).map((a: any, i: number) => <tr key={'d' + i}><td>{a.label}{a.reason && <div className="mute" style={{ fontSize: 12 }}>{a.reason}</div>}</td><td className="num">−{inr(a.amount)}</td></tr>)}
          {L('Other deduction', r.od, r.reason, true)}
          <tr><td><b>Total deductions</b></td><td className="num"><b>{inr(r.totalDed)}</b></td></tr>
          <tr><td><b>Net pay</b></td><td className="num"><b>{inr(r.net)}</b></td></tr>
        </tbody></table></div>}
      </div>
      <div style={{ display: 'grid', gap: 14 }}>
        <div className="card"><h3>Salary and structure history</h3>
          <dl className="kv"><dt>Gross now</dt><dd>{e.salary?.gross != null ? inr(e.salary.gross) : 'Hidden'}</dd><dt>Structure</dt><dd>{e.structure || e.salary?.structure || r?.structure || 'Default'}</dd></dl>
          {rev.length ? <ul className="log">{rev.map((h, i) => <li key={i}>{h.old_value || '—'} → <b>{h.new_value}</b> from {fmtD(h.effective_from)}<div className="mute" style={{ fontSize: 12 }}>Entered by {h.changed_by}, {fmtD(h.changed_at)}{h.state === 'Scheduled' ? ' · scheduled' : ''}</div></li>)}</ul>
            : <p className="mute" style={{ margin: '0 0 6px' }}>No revisions recorded.</p>}
          {sMap.map((m, i) => <div key={i} className="mute" style={{ fontSize: 12.5 }}>{m.name} from {fmtD(m.from_date)}</div>)}
        </div>
        <div className="card"><h3>Scheduled for later months</h3>
          {fut.length ? <ul className="log">{fut.map((l, i) => <li key={i}>{ymLabel(l.ym)} · {l.label} · <b>{l.kind === 'ded' ? '−' : '+'}{inr(l.amount)}</b>{l.reason && <div className="mute" style={{ fontSize: 12 }}>{l.reason}</div>}</li>)}</ul>
            : <p className="mute" style={{ margin: 0 }}>Nothing scheduled.</p>}
        </div>
      </div>
    </div>}
    {slip && <SlipModal ym={ym} id={x.employee.ID} name={x.employee.Name} onClose={() => setSlip(false)} />}
  </>;
}

/** Prototype slipPrev: the payslip as it will be for the month (preview), else the published one. */
function SlipModal({ ym, id, onClose }: { ym: string; id: string; name: string; onClose: () => void }) {
  // Wrapped, so "still loading" (null) and "no payslip" ({ v: null }) differ.
  const s = useLoad(() => loadSlip(id, ym).then((v) => ({ v })), [ym, id]);
  const v = s.data?.v;
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Payslip preview</h2><div className="row"><button className="btn" disabled={!v} onClick={printSlip}>Download</button><button className="btn sm" onClick={onClose}>Close</button></div></div>
    <div style={{ marginTop: 12 }}>{s.error ? <div className="note bad">{s.error}</div> : !s.data ? <Loading /> : v ? <Payslip v={v} /> : <p className="mute" style={{ margin: 0 }}>No payslip for {ymLabel(ym)} yet.</p>}</div>
  </Modal>;
}
