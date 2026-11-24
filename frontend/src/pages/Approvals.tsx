import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api, fmtD, inr, openDoc, uploadDoc, addDays, today, postForm, session, ApiError } from '../api';
import { useAuth } from '../auth';
import { Chain, Field, Loading, Modal, Pill, Switch, Tabs, download, toast, useLoad } from '../components/ui';
import PersonPicker, { type Person } from '../components/PersonPicker';
import { CandidateBlock, InterviewForm } from './Onboarding';
import { SkipApprovalButton, SkipWaitingButton } from '../components/SkipApproval';
import { shiftOffered } from './ShiftForm';

/** Request types, in the prototype's order (WFS). */
export const WORKFLOWS: Record<string, string> = {
  shortlist: 'Shortlist approval', joining: 'Offer approval', verify: 'Joining verification', leave: 'Leave', regularize: 'Attendance regularization', transfer: 'Employee transfer',
  resign: 'Separation', p_claim: 'Reimbursement claim', p_adv: 'Salary advance', p_arrear: 'Arrear', p_change: 'Bank or profile change', p_fnf: 'Full and final', p_ded: 'Salary deduction',
  p_ack: 'Company policy acknowledgement',
};
const WFNOTE: Record<string, string> = {
  shortlist: 'The first stage of every new hire. HR shortlists a candidate (SI-0001); the Line Manager column is the hiring manager named on the shortlist, then the Function Head of the department. After approval the hiring manager takes the interview and marks Selected or Not selected.',
  joining: 'First approval for a new hire, before the offer is released. Pick who approves the offer; going over budget adds more approvers.',
  verify: 'Second approval, after the candidate joins. Payroll checks every joining detail and document before the employee becomes active.',
  leave: 'Used when an employee applies for leave. Days are added up over the last 30 days for the same leave type, including this request.',
  regularize: 'Used when an employee corrects a punch. Requests are counted by calendar month. The policy decides whether No Punch and All Other are counted separately.',
  transfer: 'Used when an employee moves to another location, city or department. The receiving side approves too.',
  resign: 'Every exit: resignation raised by the employee or HR, and termination, absconding, contract end, retirement and death raised by HR. Exit type, grade and an early release decide who approves.',
  p_claim: 'Reimbursement claims. Amount and claim type decide how many approvers a claim needs. When the last approver approves, the amount is added to the open month.',
  p_adv: 'Salary advances. A larger advance adds more approvers. The payment and every recovery instalment are scheduled when the last approver approves.',
  p_arrear: 'Arrears raised by Payroll. The amount decides who approves. PF and ESI are not applied to arrears.',
  p_change: 'Bank and profile changes asked for by employees. A bank change keeps salary on hold until it is verified.',
  p_fnf: 'Full and final settlement after exit. The net amount decides who approves. When the last approver approves, the lines are added to pay lines.',
  p_ded: 'Manual deductions such as cash shortage or damage, raised by Payroll or HR. The amount decides who approves. Every instalment is deducted automatically once the last approver approves.',
  p_pip: 'A PIP goes to the HRBP (HRBP Corporate for corporate staff). Approving it starts the plan.',
  p_ack: 'A signed copy of a company policy, uploaded by the employee. The reporting manager approves it or returns it with a comment; HR Admin and the HRBP can act too. While it is pending the salary for the month is held.',
  interview: 'After the shortlist is approved, the hiring manager takes the interview and records Selected or Not selected. Who takes it cannot be changed; only the approval hierarchy can be switched off.',
};
/** Inbox items that are not approval requests (prototype payItems): improvement plans and tax proofs. */
const EXTRA_WF: Record<string, string> = { p_pip: 'Performance improvement plan', p_tax: 'Tax proofs', interview: 'Interview' };
/** The request type filter: the prototype's WFS (p_pip before p_ded) and PAYX. */
const FILTER_WF: [string, string][] = [...Object.entries(WORKFLOWS).flatMap(([k, l]): [string, string][] =>
  (k === 'p_ded' ? [['p_pip', EXTRA_WF.p_pip], [k, l]] : k === 'shortlist' ? [[k, l], ['interview', EXTRA_WF.interview]] : [[k, l]])), ['p_tax', EXTRA_WF.p_tax]];
/** The prototype's WFS: every request type with an approval flow, the improvement plan included (MX.p_pip). */
const FLOW_WF: [string, string][] = FILTER_WF.filter(([k]) => k !== 'p_tax' && k !== 'interview'); // the interview is always the hiring manager's
/** The Approval flow tab: every request type with a flow (the "Approval required" tick is in Check a flow › All flows
 *  at a glance only, 21 Nov 2026). */
const EDIT_WF: [string, string][] = FLOW_WF;
export const wfName = (k: string) => WORKFLOWS[k] ?? EXTRA_WF[k] ?? (k.startsWith('leave') ? `Leave, ${k === 'leaveC' ? 'casual and sick' : 'privilege and other'}` : k);
const COND: Record<string, string> = {
  always: 'Every request', days: 'Leave days in the last 30 days', grades: 'Grade is', cnt: 'Requests this month', budget: 'Above budget (headcount or pay band)',
  waive: 'Notice waiver requested', tkind: 'Transfer is', amount: 'Amount in rupees', ctype: 'Claim type is', ckind: 'Change is', pct: 'Pay change in percent',
  bandx: 'Outside the grade pay band', skind: 'Exit type is',
};
const COND_OK: Record<string, string[]> = {
  shortlist: ['always'],
  joining: ['always', 'budget'], verify: ['always'], leave: ['always', 'days', 'grades'], regularize: ['always', 'cnt', 'grades'], transfer: ['always', 'grades', 'tkind'],
  resign: ['always', 'grades', 'waive', 'skind'], p_claim: ['always', 'amount', 'ctype', 'grades'], p_adv: ['always', 'amount', 'grades'], p_arrear: ['always', 'amount'],
  p_ded: ['always', 'amount', 'grades'], p_change: ['always', 'ckind'], p_fnf: ['always', 'amount'],
  p_pip: ['always', 'amount'], // the prototype copies the arrear's (COND_OK.p_pip)
  p_ack: ['always', 'grades'],
};
const TKINDS = ['Within city', 'Cross city', 'Cross state', 'Cross region'];
const CLAIM_TYPES = ['Travel', 'Mobile and internet', 'Other'];
const EXIT_TYPES = ['Resignation', 'Termination', 'Absconding', 'Contract end', 'Retirement', 'Death'];
const LEAVE_TYPES = ['Casual leave', 'Privilege leave', 'Sick leave', 'Comp-off', 'Loss of pay'];
const REASONS = ['Forgot to punch', 'Biometric or device issue', 'Field or site duty', 'Work from home', 'Roster changed', 'Other'];
const NORESP: [string, string][] = [['escalate', 'Escalate to the next level'], ['auto', 'Auto-approve'], ['wait', 'Keep waiting and remind']];
/** "All flows at a glance" lists request types in the prototype's order. */
const GLANCE = ['shortlist', 'interview', 'joining', 'verify', 'leave', 'regularize', 'transfer', 'p_claim', 'p_adv', 'p_arrear', 'p_change', 'resign', 'p_fnf', 'p_pip', 'p_ded', 'p_ack'];

const ymLabel = (ym?: string) => (ym ? new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 15).toLocaleString('en-IN', { month: 'long', year: 'numeric' }) : '');
const dayNo = (s: string) => Date.UTC(+s.slice(0, 4), +s.slice(5, 7) - 1, +s.slice(8, 10)) / 864e5;
const dayDiff = (a: string, b: string) => Math.round(dayNo(b) - dayNo(a));
export const statusTone = (s: string) => (s === 'Approved' ? 'ok' : s === 'Rejected' ? 'bad' : s === 'Cancelled' ? 'n' : 'warn');
/** When a history entry happened, as the prototype writes it (nowS): "04 Nov, 11:37 am". */
const atS = (s: string) => { const d = new Date(s); return isNaN(d.getTime()) ? s : d.toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }); };
/** The prototype's stage order (STAGE): the "Add column" list follows it. */
const STAGE_ORDER = ['Line Manager', 'Functional Manager', 'Receiving Manager', 'Receiving Property Manager', 'Receiving Cluster Manager', 'Receiving Function Head',
  'Property Manager', 'Function Head', 'HRBP', 'Cluster Manager', 'Operations Head', 'HRBP Corporate', 'Payroll', 'Payroll Manager', 'CHRO', 'Finance Head'];
/** Masters shared by every request detail (names for a transfer's location and city). */
let mastersP: Promise<{ locs: any[]; cities: any[] }> | null = null;
const masters = () => (mastersP ||= Promise.all([api<any[]>('/masters/location'), api<any[]>('/masters/city')]).then(([locs, cities]) => ({ locs, cities }))
  .catch((e) => { mastersP = null; throw e; }));

type T = 'queue' | 'pay' | 'join' | 'rules' | 'names' | 'check';

/** Payroll view's own tabs: the two kinds of approval Payroll handles, each a filtered Requests list. */
const PAY_GROUP = ['regularize', 'leave', 'p_claim', 'p_adv', 'p_ded', 'p_arrear', 'p_fnf', 'p_change', 'p_tax'];
const JOIN_GROUP = ['joining', 'verify'];
const GROUP_NOTE: Record<string, string> = {
  pay: 'Requests that change pay: attendance, leave, claims, advances, deductions, arrears, full and final, bank changes and tax proofs.',
  join: 'New joiners: offer approval before the offer is released, and joining verification before the employee becomes active.',
};

export default function Approvals() {
  const { can, me } = useAuth();
  // Other pages link here: ?tab=rules opens the approval flow, ?id=REQ opens that request (prototype goAppr).
  const [params] = useSearchParams();
  const [tab, setTab] = useState<T>((['pay', 'join', 'rules', 'names', 'check'] as T[]).find((k) => k === params.get('tab')) ?? 'queue');
  const cfg = can('appr.setup', 2);
  // The prototype's Employee role has the approvals view, so "Check a flow" shows for it too (for their own route).
  const check = cfg || can('appr.queue') || me?.ctx.role === 'Employee';
  // Payroll approvals and Joining approvals show in the Payroll view only.
  const payroll = me?.ctx.role === 'Payroll';
  const tabs: [T, string][] = [['queue', 'Requests'], ...(payroll ? [['pay', 'Payroll approvals'], ['join', 'Joining approvals']] as [T, string][] : []), ...(cfg ? [['rules', 'Approval flow'], ['names', 'Approver names']] as [T, string][] : []), ...(check ? [['check', 'Check a flow']] as [T, string][] : [])];
  const t = tabs.some(([k]) => k === tab) ? tab : 'queue';
  return (
    <>
      <Tabs tabs={tabs} value={t} onChange={setTab} />
      {t === 'queue' && <Queue key="all" />}
      {t === 'pay' && <Queue key="pay" group={PAY_GROUP} note={GROUP_NOTE.pay} bulk="pay" />}
      {t === 'join' && <Queue key="join" group={JOIN_GROUP} note={GROUP_NOTE.join} bulk="join" />}
      {t === 'rules' && <FlowEditor goNames={() => setTab('names')} />}
      {t === 'names' && <ApproverNames />}
      {/* "Open names" shows for everyone, as in the prototype; without the Approver names tab it lands on Requests. */}
      {t === 'check' && <CheckFlow goNames={() => setTab(cfg ? 'names' : 'queue')} />}
    </>
  );
}

// ---------- Requests ----------

type F = 'mine' | 'open' | 'done' | 'info';

/** The request inbox. `group` limits it to some request types (Payroll view's Payroll and Joining approvals tabs). */
function Queue({ group, note, bulk }: { group?: string[]; note?: string; bulk?: Bulk } = {}) {
  const { me, can } = useAuth();
  const role = me?.ctx.role || '';
  const [params] = useSearchParams();
  const linked = params.get('id') || '';
  const [f, setF] = useState<F>(linked ? 'open' : 'mine');
  const [wf, setWf] = useState('');
  const [sel, setSel] = useState(linked);
  const [nr, setNr] = useState('');
  // Ticked requests for "Approve selected" (Payroll and Joining approvals tabs).
  const [picked, setPicked] = useState<Set<string>>(new Set());
  useEffect(() => { setPicked(new Set()); }, [f, wf]);
  const wfq = encodeURIComponent(wf || (group ?? []).join(','));
  const { data, error, reload } = useLoad(() => api<any[]>(`/approvals?tab=${f}&wf=${wfq}`), [f, wfq, role]);
  const counts = useLoad(() => api<Record<F, number>>(`/approvals/counts?wf=${wfq}`), [wfq, role, data]);
  const types = group ? FILTER_WF.filter(([k]) => group.includes(k)) : FILTER_WF;
  // A linked request that is no longer open is looked for once among the completed ones.
  const [looked, setLooked] = useState(false);
  useEffect(() => {
    if (linked && data && !looked && f === 'open' && !data.some((r) => r.id === linked)) { setLooked(true); setF('done'); }
  }, [data, linked, looked, f]);
  const current = data?.find((r) => r.id === sel) ?? data?.[0];
  const seg: [F, string][] = [['mine', 'To approve'], ['open', 'All open'], ['done', 'Completed'], ...(role !== 'HR Admin' ? [['info', 'For information']] as [F, string][] : [])];
  // Employees raise their own requests; anyone else needs the right to raise for others.
  const canNew = role === 'Employee' || can('appr.raise', 2);
  const newTypes = ['leave', 'regularize', 'transfer', 'resign'].filter((k) =>
    !(role === 'Employee' && k === 'transfer') && !(k === 'resign' && !['Employee', 'HR Admin', 'HRBP', 'HRBP Corporate'].includes(role)));
  return (
    <>
      {!group && (role === 'HR Admin' || can('emp.directory', 2)) && <ManagerLeft onDone={reload} />}
      {note && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>{note}</p>}
      <div className="row sp" style={{ marginBottom: 14 }}>
        <div className="seg">{seg.map(([k, l]) => <button key={k} className={f === k ? 'on' : ''} onClick={() => { setF(k); setSel(''); }}>{l}<b>{counts.data?.[k] ?? ''}</b></button>)}</div>
        <div className="row">
          <select aria-label="Request type" value={wf} onChange={(e) => { setWf(e.target.value); setSel(''); }}><option value="">{group ? 'All of these types' : 'All request types'}</option>{types.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
          {canNew && !group && <select aria-label="New request" value="" onChange={(e) => setNr(e.target.value)}><option value="">New request…</option>{newTypes.map((k) => <option key={k} value={k}>{WORKFLOWS[k]}</option>)}</select>}
        </div>
      </div>
      {bulk && <BulkBar group={bulk} rows={data ?? []} picked={picked} setPicked={setPicked} onDone={() => { setPicked(new Set()); reload(); }} />}
      {!data && <Loading error={error} />}
      {data && (data.length
        ? <div className="inbox">
          <div className="ilist">
            {data.map((r) => {
              const row = <button key={r.id} className={`irow ${current?.id === r.id ? 'on' : ''}`} style={bulk ? { borderBottom: 0, flex: 1, minWidth: 0 } : undefined} onClick={() => setSel(r.id)}>
                <div className="t"><span>{r.employee_name}</span>{r.status === 'Pending' ? <Pill k="warn">{r.waitingOn}</Pill> : <Pill k={statusTone(r.status)}>{r.status}</Pill>}</div>
                <div className="s">{wfName(r.wf)} · {subOf(r)}</div>
              </button>;
              if (!bulk) return row;
              // A tick box on each request you can approve now.
              return <div key={r.id} style={{ display: 'flex', alignItems: 'stretch', borderBottom: '1px solid var(--line)', background: current?.id === r.id ? 'var(--accent-soft)' : undefined }}>
                <label style={{ display: 'flex', alignItems: 'flex-start', padding: '13px 0 0 12px', cursor: canPick(bulk, r) ? 'pointer' : 'default' }} title={canPick(bulk, r) ? 'Tick to approve with others' : 'Not waiting on you'}>
                  <input type="checkbox" aria-label={`Select ${r.employee_name} ${r.id}`} disabled={!canPick(bulk, r)} checked={picked.has(r.id)}
                    onChange={(e) => { const n = new Set(picked); if (e.target.checked) n.add(r.id); else n.delete(r.id); setPicked(n); }} /></label>
                {row}
              </div>;
            })}
          </div>
          <div>{current && <Detail id={current.id} row={current} onChange={reload} />}</div>
        </div>
        : <div className="card mute">{f === 'mine' ? 'Nothing is waiting on you.' : 'Nothing here right now.'}</div>)}
      {nr && <NewRequest wf={nr} onClose={() => setNr('')} onDone={() => { setNr(''); setF('open'); setWf(''); setSel(''); reload(); }} />}
    </>
  );
}

/** Reports of a manager who has left: reassign them, and their open requests follow. */
function ManagerLeft({ onDone }: { onDone: () => void }) {
  const { data, reload } = useLoad(() => api<any[]>('/managers/left'), []);
  const [to, setTo] = useState<Record<string, Person | null>>({});
  if (!data?.length) return null;
  const go = async (from: string) => {
    const m = to[from];
    if (!m) { toast('Choose the new manager'); return; }
    try { const r = await api('/managers/reassign', { body: { from, to: m.id } }); const n = data.find((g) => g.id === from)?.reports.length ?? r.moved; toast(`${n} employee${n === 1 ? '' : 's'} now report to ${m.name}`); reload(); onDone(); } catch (x: any) { toast(x.message); }
  };
  return <div className="card" style={{ marginBottom: 14, borderLeft: '3px solid var(--warn,#d98a00)' }}>
    <h3>Reporting manager has left</h3>
    <p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>Until you reassign, their requests go to the manager's own manager, then the HRBP. Reassigning also moves open requests.</p>
    {data.map((g) => <div key={g.id} style={{ padding: '10px 0', borderTop: '1px solid var(--line)' }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}>
      <div><b>{g.name}</b> <span className="mute">{g.lwd ? `left ${fmtD(g.lwd)}` : 'inactive'}</span>
        <div className="mute" style={{ fontSize: 12.5 }}>{g.reports.length} employee{g.reports.length > 1 ? 's' : ''}: {g.reports.slice(0, 4).map((p: any) => p.name).join(', ')}{g.reports.length > 4 ? ` and ${g.reports.length - 4} more` : ''}</div></div>
      <div className="row" style={{ gap: 8 }}><PersonPicker label="New manager" placeholder="New manager" search="&status=Active" exclude={[g.id]} value={to[g.id]?.id ?? ''} selected={to[g.id]}
        onChange={(_, p) => setTo({ ...to, [g.id]: p })} style={{ width: 280, maxWidth: '100%' }} />
        <button className="btn primary sm" onClick={() => go(g.id)}>Reassign</button></div>
    </div></div>)}
  </div>;
}

/** The prototype's reqSub: what a request is about, under the name. Pay items describe the item itself. */
export function subOf(r: any, offer?: any): string {
  const p = r.payload || {};
  switch (r.wf) {
    case 'p_claim': return `${p.claimType} · ${inr(p.amount)}`;
    case 'p_change': return p.kind === 'Bank' ? 'Bank account change' : 'Personal details change';
    case 'p_arrear': return `${p.forYm ? `For ${ymLabel(p.forYm)} · ` : ''}${inr(p.amount)}`;
    case 'p_adv': return `Advance · ${inr(p.amount)}${p.instalments ? ` in ${p.instalments} instalments` : ''}`;
    case 'p_ded': return `${p.dedType || 'Deduction'} · ${inr(p.amount)}`;
    case 'p_fnf': return p.lwd ? `Last day ${fmtD(p.lwd)} · net ${inr(p.amount || 0)}` : `Net ${inr(p.amount || 0)}`;
    case 'p_pip': case 'p_tax': return r.sub || '';
    case 'p_ack': return `${p.policyTitle} · version ${p.version}`;
    case 'shortlist': case 'interview': return [r.candidate_id, r.designation, r.city].filter(Boolean).join(' · ');
  }
  if (offer) return `${offer.rejoin_of ? 'Rejoin · ' : ''}${offer.designation} · ${offer.city}`;
  return [r.wf === 'joining' ? r.designation : r.department ?? r.dept, r.city, r.wf === 'joining' ? '' : r.type].filter(Boolean).join(' · ');
}

/** The line under the header: what is asked, as the prototype's title and meta. */
function titleOf(r: any, ctx: { offer?: any; locs?: { locs: any[]; cities: any[] } | null }): ReactNode {
  const p = r.payload || {}, f = r.facts || {};
  switch (r.wf) {
    case 'joining': {
      const o = ctx.offer; if (!o) return null;
      const bp = o.band_pos && o.band_pos !== 'none' ? <Pill k={o.band_pos === 'within' ? 'ok' : 'warn'}>{o.band_pos === 'within' ? 'Within pay band' : o.band_pos === 'above' ? 'Above pay band' : 'Below pay band'}</Pill> : null;
      return <>{o.type} · joining {fmtD(o.doj)} · CTC {typeof o.ctcYear === 'number' ? `${inr(o.ctcYear)} a year` : 'Hidden'} {bp}</>;
    }
    case 'shortlist': return `Shortlist ${r.candidate_id} · ${r.designation ?? ''} · approve to send the candidate to the interview`;
    case 'interview': return `Interview of ${r.candidate_id} · the hiring manager records Selected or Not selected`;
    case 'verify': { const o = ctx.offer; return o ? `Joining verification · ${o.joining?.done ?? 0} of ${o.joining?.total ?? 0} mandatory items complete · ${Object.keys(o.docs || {}).length} documents uploaded` : 'Joining verification'; }
    case 'leave': { if (p.short) return `Short leave, ${p.fromTime} to ${p.toTime} · ${fmtD(p.from)}`; const d = f.days ?? 1, roll = f.roll ?? d; return `${p.ltype}, ${d === 0.5 ? `half a day${p.halfOf ? ` (${p.halfOf} half)` : ''}` : `${d} day${d > 1 ? 's' : ''}`}${roll > d ? ` (${roll} in 30 days)` : ''} · ${fmtD(p.from)} to ${fmtD(p.to)}`; }
    case 'regularize': {
      // A "Roster changed" regularization names the change behind it (prototype makeWfReq).
      const x = r.rosterChange;
      return `Regularize ${fmtD(p.date)} · ${p.rtype || 'All Other'} · ${p.reason || '—'} · ${p.in || '—'} to ${p.out || '—'} · number ${f.cnt ?? 1} this month${x ? ` · Roster for this day was changed${x.late ? ' late' : ''} by ${x.by}${x.why ? ': ' + x.why : ''}` : ''}`;
    }
    case 'transfer': {
      const l = ctx.locs?.locs.find((x) => x.id === p.toLocation), c = l && ctx.locs?.cities.find((x) => x.id === l.city_id);
      return `Transfer to ${c?.name ?? l?.name ?? 'another location'} · ${l?.name ?? ''}${p.toDepartment ? ` · ${p.toDepartment}` : ''} · effective ${fmtD(p.effective)}`;
    }
    case 'resign': {
      const kind = p.exitType || 'Resignation';
      if (kind !== 'Resignation') return [kind, p.lwd ? `Last working day ${fmtD(p.lwd)}` : '', p.reason].filter(Boolean).join(' · ');
      const serve = p.lwd ? Math.max(0, dayDiff(String(r.created_at).slice(0, 10), p.lwd)) : 0;
      const head = p.rkind === 'withdraw' ? 'Withdraw resignation' : p.rkind === 'change' ? 'Change last working day' : 'Resignation';
      return `${head} · Last working day ${fmtD(p.lwd)}${p.noticeDays ? ` · policy notice ${p.noticeDays} days, serving ${serve}` : ''}${p.waive ? ' · waiver requested' : ''}`;
    }
    case 'p_claim': return `${p.claimType} claim, ${inr(p.amount)}${p.billDate ? ` · bill dated ${fmtD(p.billDate)}` : ''}`;
    case 'p_change': return `${p.kind === 'Bank' ? 'Bank account change' : 'Personal details change'} · asked by ${r.raised_by === r.employee_id ? 'the employee' : r.raised_by_name || r.raised_by}`;
    case 'p_arrear': return `${p.amount < 0 ? 'Recovery' : 'Arrear'} ${inr(Math.abs(p.amount))}${p.forYm ? ` · for ${ymLabel(p.forYm)}` : ''}`;
    case 'p_adv': return `Advance ${inr(p.amount)}${p.instalments ? ` · ${p.instalments} instalments` : ''}`;
    case 'p_ded': return `${p.dedType || 'Deduction'}, ${inr(p.amount)} · ${p.instalments || 1} instalment${(p.instalments || 1) > 1 ? 's' : ''}`;
    case 'p_fnf': return `Full and final, ${inr(p.amount || 0)}${p.lwd ? ` · last working day ${fmtD(p.lwd)}` : p.payMonth ? ` · pay in ${ymLabel(p.payMonth)}` : ''}`;
    case 'p_pip': return `Performance improvement plan, ${p.days} days · raised by ${p.issuedBy}`;
    case 'p_tax': return `${r.title} · ${r.meta}`;
    case 'p_ack': return `Signed copy of ${p.policyTitle}, version ${p.version} · in force from ${fmtD(p.effectiveFrom)}`;
  }
  return null;
}

/** Approved pay items that land in pay lines (prototype payItemRows "See in Upcoming pay lines"). */
const PAY_LINES = ['p_claim', 'p_adv', 'p_arrear', 'p_ded', 'p_fnf'];
/** Who agreed an earlier last working day, when the separation recorded it. */
const termsBy = (p: any): string => { const n = p.termsBy || p.termsByName; return n ? `${n}${p.termsRole ? ` (${p.termsRole})` : ''}` : ''; };

/** The prototype's payItemRows: the facts of a pay item as a two-column table. */
function payRows(r: any): [string, ReactNode][] {
  const p = r.payload || {}, by = r.raised_by_name || r.raised_by;
  // The prototype names the attached file in the row; here the row opens it.
  const file = (label: string) => (p.doc ? <a href="#" onClick={(e) => { e.preventDefault(); openDoc(p.doc).catch((x) => toast(x.message)); }}>{label}</a> : 'None');
  switch (r.wf) {
    case 'p_claim': return [['Claim type', p.claimType], ['Bill date', fmtD(p.billDate)], ['Amount', inr(p.amount)], ['Note', p.note || '—'], ['Bill attached', file('Open the bill')]];
    case 'p_change': return p.kind === 'Bank' ? [['Bank', p.bankName], ['Account', p.accountNo], ['IFSC', p.ifsc]]
      : ([['Mobile', p.mobile], ['Email', p.email], ['Address', p.address], ['Emergency contact', p.ecName], ['Relation', p.ecRelation], ['Emergency contact mobile', p.ecMobile]] as [string, string][]).filter((x) => x[1]);
    case 'p_arrear': return [['Arrear for', `${ymLabel(p.forYm)}${p.toYm && p.toYm !== p.forYm ? ` to ${ymLabel(p.toYm)}` : ''}`], ['Amount', inr(p.amount)], ['Reason', p.slipText || p.note || '—'], ['Pay in', ymLabel(p.payMonth) || '—'], ['Raised by', by]];
    case 'p_adv': {
      const n = p.instalments || 1;
      if (p.opening) return [['Opening balance', 'Given before the HRMS went live; nothing is paid again'], ['Advance given on', fmtD(p.advanceDate)], ['Original amount', inr(p.amount)],
        ['Balance at go-live', inr(p.balance)], ['Recovery', `${inr(p.monthly)} a month from ${ymLabel(p.startYm)}, ${n} instalment${n === 1 ? '' : 's'}`], ['Reference', p.reference || '—'], ['Loaded by', by]];
      return [['Amount', inr(p.amount)], ['Paid with', ymLabel(p.payMonth) || '—'], ['Recovery', `${n} × ${inr(Math.round((p.amount || 0) / n))}${p.startYm ? ` from ${ymLabel(p.startYm)}` : ''}`], ['Reason', p.note || '—'],
        ['Raised by', r.raised_by && r.raised_by === r.employee_id ? `${by} (the employee)` : by]];
    }
    case 'p_ded': {
      const sched = (r.schedule as { ym: string; amount: number }[] | undefined)?.map((x) => `${ymLabel(x.ym)} ${inr(x.amount)}`).join(', ');
      return [['Type', p.dedType || 'Deduction'], ['Amount', inr(p.amount)], ['Schedule', sched || `${p.instalments || 1} instalment${(p.instalments || 1) > 1 ? 's' : ''}${p.startYm ? ` from ${ymLabel(p.startYm)}` : ''}`], ['What happened', p.note || '—'], ['Employee heard', p.heard || '—'], ['Proof', file('Open the proof')], ['Raised by', by]];
    }
    case 'p_fnf': {
      // The prototype's fnfRows: the settlement lines between the service and the net.
      const f = r.fnf, lwd = f?.lwd || p.lwd;
      if (!f) return [['Net amount', inr(p.amount || 0)], ['Pay in', ymLabel(p.payMonth) || '—']];
      return [['Last working day', fmtD(lwd)], ['Service', `${fmtD(f.doj)} to ${fmtD(lwd)}`],
        ...(f.lines as any[]).map((l): [string, string] => [l.label + (l.kind === 'info' ? ' (information)' : ''), (l.kind === 'ded' ? '− ' : '') + inr(l.amount) + (l.note ? ' · ' + l.note : '')]),
        ['Final month salary', `Paid in the normal payroll run for ${ymLabel(String(lwd).slice(0, 7))}`], ['Net full and final', inr(f.net ?? p.amount)], ['Pay in', ymLabel(f.payYm || p.payMonth) || '—']];
    }
    case 'p_pip': return [['Reason', p.reason], ['Duration', `${p.days} days`], ['Details', p.details || '—'], ['Raised by', `${p.issuedBy} (${p.issuedRole})`]];
    case 'p_tax': return [['Proofs waiting', String(p.proofs)], ['Regime', p.regime === 'Old' ? 'Old regime' : 'New regime']];
    case 'p_ack': {
      // The signed copy next to the policy it signs (policy_ack.go).
      const doc = (id: string, label: string) => (id ? <a href="#" onClick={(e) => { e.preventDefault(); openDoc(id).catch((x) => toast(x.message)); }}>{label}</a> : 'None');
      return [['Policy', `${p.policyTitle}, version ${p.version}`], ['In force from', fmtD(p.effectiveFrom)], ['Policy document', doc(p.policyDoc, 'Open the policy')],
        ['Signed copy', doc(p.signedDoc, 'Open the signed copy')], ['Salary', 'Held for the month while this is pending. Returning it keeps the hold until a new copy is approved.']];
    }
  }
  return [];
}

/** One request with its approval progress and history. `readOnly` (My view › Dashboard › My requests) never shows
 *  Approve or Reject; the requester's Cancel stays, as the server allows it (canCancel). */
export function Detail({ id, row, onChange, readOnly }: { id: string; row: any; onChange: () => void; readOnly?: boolean }) {
  const { me, can } = useAuth();
  const myId = me?.ctx.employeeId;
  const nav = useNavigate();
  const { data: r, error, reload } = useLoad(() => api(`/approvals/${id}`), [id]);
  const sla = useLoad(() => api<{ slaHours: number }>('/approvals/settings').then((x) => x.slaHours).catch(() => 0), []);
  const offerId = r ? r.offer_id || r.payload?.offerId : '';
  const offer = useLoad(() => (offerId && (r.wf === 'joining' || r.wf === 'verify') ? api(`/offers/${offerId}`) : Promise.resolve(null)), [offerId]);
  const locs = useLoad(() => (r?.wf === 'transfer' ? masters() : Promise.resolve(null)), [r?.wf]);
  // An approver may let a resigning employee go before the notice ends (prototype relCan): the separation case holds the terms.
  const rel = !!r && r.wf === 'resign' && (r.payload?.exitType || 'Resignation') === 'Resignation' && (r.payload?.rkind || 'new') === 'new' && r.status === 'Pending' && r.canAct && !!r.payload?.noticeLwd;
  const caseId: string | undefined = rel ? r.caseId : undefined;
  const [note, setNote] = useState('');
  const [lwd, setLwd] = useState('');
  const [short, setShort] = useState('waive');
  const [busy, setBusy] = useState(false);
  // A salary advance: the approver sets the monthly instalments when approving (owner, 14 Nov 2026), up to the company's most.
  const advMax = useLoad(() => (r?.wf === 'p_adv' ? api<any>('/me/advance').then((x) => x.maxInstalments as number).catch(() => 12) : Promise.resolve(0)), [r?.wf]);
  const [inst, setInst] = useState('');
  useEffect(() => { setNote(''); setLwd(''); setInst(''); }, [id]);
  useEffect(() => { if (r?.wf === 'resign') { setLwd(r.payload?.lwd || ''); setShort(r.payload?.shortfall === 'recover' ? 'recover' : 'waive'); } }, [r]);
  if (!r) return <Loading error={error} />;
  const p = r.payload || {};
  const admin = me?.ctx.role === 'HR Admin';
  const stage = r.chain[r.step];
  const override = admin && stage !== 'HR Admin'; // as the API: HR Admin on another role's stage needs a comment
  const done = () => { reload(); onChange(); };
  const approve = async () => {
    if (override && !note.trim()) { toast(`Add a comment: you are approving on behalf of ${stage}`); return; }
    setBusy(true);
    try {
      if (rel && caseId && lwd && lwd !== p.lwd) {
        if (lwd < today() || lwd > p.noticeLwd) { toast(lwd < today() ? 'The last working day cannot be in the past' : 'An approver can only bring the date earlier. To extend it, raise a change of last working day'); return; }
        await api(`/separations/${caseId}/terms`, { body: { lwd, shortfall: short } });
      }
      // An improvement plan is decided in consequence management; approving it starts the plan.
      if (r.wf === 'p_pip') { await api(`/consequences/${r.consId}/approve`, { body: { note } }); toast('Fully approved. Change applied.'); done(); return; }
      const n0 = String(p.instalments || 1);
      const out = await api(`/approvals/${id}/approve`, { body: inst && inst !== n0 ? { note, instalments: +inst } : { note } });
      const what = r.wf === 'joining' ? (String(r.offer_id || '').startsWith('OI-') ? 'Offer approved, ready to release.' : 'Employee created.') : r.wf === 'shortlist' ? 'Interview is next.' : 'Change applied.';
      toast(out.status === 'Approved' ? `Fully approved. ${what}` : `Approved. Next: ${out.next}`);
      done();
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const reject = async () => {
    if (!note.trim()) { toast(r.wf === 'p_ack' ? 'Add a comment to return it' : 'Add a comment to reject'); return; }
    setBusy(true);
    try { await api(r.wf === 'p_pip' ? `/consequences/${r.consId}/reject` : `/approvals/${id}/reject`, { body: { note } }); toast(r.wf === 'p_ack' ? 'Returned to the employee' : 'Rejected'); done(); } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const cancel = async () => {
    try { await api(`/approvals/${id}/cancel`, { method: 'POST' }); toast('Request cancelled'); done(); } catch (x: any) { toast(x.message); }
  };
  // A skip is written without a stage ("… Moved to Payroll Manager"): it belongs to the stage just before the one it moved to,
  // and shows on that stage's line, as the prototype's history does ("Skipped by System · …").
  const stageOf = (h: any): string | null => {
    if (h.stage) return h.stage;
    const m = h.action === 'Skipped' ? /Moved to (.+)$/.exec(h.note || '') : null;
    const j = m ? r.chain.indexOf(m[1]) : -1;
    return j > 0 ? r.chain[j - 1] : null;
  };
  const hist = (st: string) => [...r.history].reverse().find((h: any) => stageOf(h) === st);
  const loose = r.history.filter((h: any) => { const s = stageOf(h); return !s || !r.chain.includes(s); }); // reroutes and the final result
  const result = loose.filter((h: any) => h.action === 'Applied');
  const sub = subOf({ ...row, ...r, department: row?.department }, r.wf === 'joining' ? offer.data : undefined);
  const rows = payRows(r);
  const nd = p.noticeLwd as string | undefined;
  const title = titleOf(r, { offer: offer.data, locs: locs.data });
  return (
    <div className="card">
      <div className="row sp"><div><h2 style={{ margin: 0, fontSize: 18 }}>{r.employee_name}</h2><div className="mute" style={{ fontSize: 13 }}>{sub}</div></div>
        <div className="row" style={{ gap: 6 }}><Pill k="info">{wfName(r.wf)}</Pill><Pill k={r.status === 'Approved' ? 'ok' : r.status === 'Rejected' ? 'bad' : 'warn'}>{r.status}</Pill></div></div>
      <div className="ink2" style={{ marginTop: 10, fontSize: 13.5 }}>{r.id}{title ? <> · {title}</> : null}</div>
      {p.note && <div className="ink2" style={{ fontSize: 13, marginTop: 8 }}>“{p.note}”</div>}
      {p.doc && r.wf !== 'p_claim' && r.wf !== 'p_ded' && <p className="mute" style={{ fontSize: 13 }}>Attached: <a href="#" onClick={(e) => { e.preventDefault(); openDoc(p.doc).catch((x) => toast(x.message)); }}>supporting document</a></p>}
      {r.raised_by && r.raised_by !== r.employee_id && (r.wf === 'leave' || r.wf === 'regularize') && <p className="mute" style={{ fontSize: 13 }}>Applied on behalf of the employee by {r.raised_by_name || r.raised_by}</p>}
      {r.wf === 'leave' && r.status === 'Pending' && r.teamOff && <TeamCover from={p.from} to={p.to} teamOff={r.teamOff} team={r.team} location={r.location} dept={r.department ?? row?.department} />}
      {r.notify?.length > 0 && <div className="mute" style={{ fontSize: 12.5, marginTop: 6 }}>Informed when raised: {r.notify.map((n: any) => `${n.role} (${n.person})`).join(', ')}</div>}
      {r.why?.length > 0 && <div className="mute" style={{ fontSize: 12.5, marginTop: 6 }}>Approvers added because: {r.why.join(', ')}</div>}
      {/* Documents asked for at offer release (prototype docNeed): green when uploaded, red when a mandatory one is missing. */}
      {r.wf === 'joining' && offer.data && (() => {
        const need = (offer.data.docList || []).filter((x: any) => x.offer > 0), docs = offer.data.docs || {};
        return need.length > 0 && <div className="row" style={{ marginTop: 10, gap: 6 }}>{need.map((x: any) => <Pill key={x.key} k={docs[x.key] ? 'ok' : x.offer === 2 ? 'bad' : 'warn'}>{x.label}</Pill>)}</div>;
      })()}
      {r.wf === 'verify' && offer.data && <VerifyPanel id={r.id} o={offer.data} canEdit={!!r.canAct && !readOnly && r.status === 'Pending'} onSaved={() => { offer.reload(); reload(); }} />}
      {(r.candidate_id || (r.wf === 'joining' && offer.data?.candidate_id)) && <CandidateBlock id={r.candidate_id || offer.data.candidate_id} />}
      {rows.length > 0 && <div className="tw" style={{ marginTop: 12 }}><table><tbody>{rows.map(([k, v]) => <tr key={k}><td style={{ width: '34%' }}>{k}</td><td>{v}</td></tr>)}</tbody></table></div>}
      {r.wf === 'p_tax' && <div className="row" style={{ marginTop: 12 }}>{can('pay.run') && <button className="btn primary" onClick={() => nav(`/payroll?tab=claims&sub=tax&emp=${encodeURIComponent(r.employee_id)}`)}>Review proofs</button>}<span className="mute" style={{ fontSize: 12.5 }}>Each document is reviewed and the approved amount entered on the tax screen.</span></div>}
      {r.status === 'Approved' && PAY_LINES.includes(r.wf) && can('pay.run') && <div className="row" style={{ marginTop: 10 }}><button className="btn sm" onClick={() => nav(`/payroll?tab=fwd${p.payMonth || p.startYm ? `&ym=${p.payMonth || p.startYm}` : ''}`)}>See in Upcoming pay lines</button></div>}
      {r.wf === 'resign' && nd && p.lwd && p.lwd < nd && <div className="note" style={{ marginTop: 10, fontSize: 13 }}>Last working day changed from {fmtD(nd)} to <b>{fmtD(p.lwd)}</b>{termsBy(p) ? ` by ${termsBy(p)}` : ''}. Notice shortfall {dayDiff(p.lwd, nd)} day{dayDiff(p.lwd, nd) === 1 ? '' : 's'}: {p.shortfall === 'recover' ? 'recovered from the employee' : 'waived'}.</div>}
      <h3 style={{ margin: '18px 0 10px' }}>Approval progress</h3>
      <ul className="tl">
        {r.people.map((x: any, i: number) => {
          const h = hist(x.stage);
          const isDone = i < r.step || r.status === 'Approved';
          const cur = i === r.step && r.status === 'Pending';
          const rej = r.status === 'Rejected' && i === r.step;
          // An interview reads Selected / Not selected rather than Approved / Rejected.
          const said = (a: string) => (r.wf === 'interview' ? ({ Approved: 'Selected', Rejected: 'Not selected' } as Record<string, string>)[a] ?? a : a);
          const who = isDone && h ? `${said(h.action)} by ${h.by_name} · ${atS(h.at)}${h.note ? ` · “${h.note}”` : ''}`
            : rej && h ? `${said('Rejected')} by ${h.by_name} · ${atS(h.at)} · “${h.note}”`
              : cur ? `Waiting · ${x.person}${sla.data ? ` · ${sla.data}h turnaround` : ''}` : x.person;
          return <li key={i} className={rej ? 'rej' : isDone ? 'done' : cur ? 'cur' : ''}><i /><b>{(r.wf === 'shortlist' || r.wf === 'interview') && x.stage === 'Line Manager' ? 'Line Manager (hiring manager)' : x.stage}</b><div className="mute" style={{ fontSize: 12.5 }}>{who}</div></li>;
        })}
      </ul>
      {loose.filter((h: any) => h.action !== 'Applied').map((h: any) => <div key={h.id} className="note" style={{ fontSize: 13, marginBottom: 6 }}>{h.note}</div>)}
      {result.map((h: any) => <div key={h.id} className="note ok" style={{ fontSize: 13 }}>{h.note}</div>)}
      {r.stuck && <div className="note warn" style={{ fontSize: 13, marginTop: 6 }}>{r.stuck}</div>}
      {r.wf === 'interview' && r.canAct && !readOnly ? <InterviewForm id={r.candidate_id} name={r.employee_name} onDone={done} />
        : r.canAct && !readOnly && r.wf !== 'p_tax' ? <div style={{ marginTop: 14, paddingTop: 14, borderTop: '1px solid var(--line)' }}>
        {rel && caseId && nd && <div style={{ border: '1px solid var(--line,#ddd)', borderRadius: 10, padding: 12, marginBottom: 10 }}><b>Last working day</b>
          <div className="mute" style={{ fontSize: 12.5, margin: '2px 0 8px' }}>As per notice: {fmtD(nd)}{p.lwd !== nd ? ` · currently ${fmtD(p.lwd)}` : ''}. To let {r.employee_name} leave earlier, pick the date before you approve.</div>
          <div className="row" style={{ gap: 10, flexWrap: 'wrap' }}>
            <label className="field" style={{ margin: 0 }}><span>Release on</span><input type="date" value={lwd} min={today()} max={nd} onChange={(e) => setLwd(e.target.value)} /></label>
            <label className="field" style={{ margin: 0 }}><span>Notice shortfall</span><select value={short} onChange={(e) => setShort(e.target.value)}><option value="waive">Waive it (company agrees to release early)</option><option value="recover">Recover it from the employee as per policy</option></select></label>
          </div></div>}
        {r.wf === 'p_adv' && !p.opening && ['Payroll', 'Payroll Manager'].includes(stage) && <AdvanceTerms id={id} p={p} onSaved={reload} />}
        {r.wf === 'p_adv' && !p.opening && !['Payroll', 'Payroll Manager'].includes(stage) && (advMax.data ?? 0) > 0 && <div style={{ border: '1px solid var(--line,#ddd)', borderRadius: 10, padding: 12, marginBottom: 10 }} data-testid="adv-instalments">
          <b>Recovery</b>
          <div className="mute" style={{ fontSize: 12.5, margin: '2px 0 8px' }}>Now {p.instalments || 1} monthly instalment{(p.instalments || 1) === 1 ? '' : 's'}{p.startYm ? ` from ${ymLabel(p.startYm)}` : ''}. Read the employee's wish in the reason above and set the instalments before you approve.</div>
          <label className="field" style={{ margin: 0, maxWidth: 260 }}><span>Recover in</span><select value={inst || String(p.instalments || 1)} onChange={(e) => setInst(e.target.value)}>
            {Array.from({ length: advMax.data ?? 12 }, (_, i) => i + 1).map((k) => <option key={k} value={k}>{k} monthly instalment{k === 1 ? '' : 's'}</option>)}</select></label>
        </div>}
        <input type="text" placeholder={r.wf === 'p_ack' ? 'Comment (needed to return it)' : 'Comment (needed to reject)'} value={note} onChange={(e) => setNote(e.target.value)} style={{ width: '100%', marginBottom: 10 }} />
        <div className="row"><button className="btn primary" disabled={busy} onClick={approve}>Approve{override ? ' (admin override)' : ''}</button><button className="btn bad" disabled={busy} onClick={reject}>{r.wf === 'p_ack' ? 'Return' : 'Reject'}</button></div>
      </div>
        : r.status === 'Pending' && <div className="row" style={{ marginTop: 12, flexWrap: 'wrap' }}>
          <span className="mute" style={{ fontSize: 12.5 }}>Waiting on {stage}{r.people[r.step]?.person ? ` (${r.people[r.step].person})` : ''}.{r.whyNot ? ` ${r.whyNot}` : ''}</span>
          {/* The requester's own Cancel; the server says when the employee a request is about may not cancel it (myrequests.go). */}
          {(r.raised_by === myId || r.employee_id === myId) && r.canCancel !== false && <button className="btn sm" onClick={cancel}>Cancel request</button>}
        </div>}
      {/* HR's "Skip approval" on a waiting request whose flow does not require the approval hierarchy (server canSkip). */}
      {r.canSkip && !readOnly && <div className="row" style={{ marginTop: 10 }}><SkipWaitingButton id={id} onDone={done} /></div>}
    </div>
  );
}

/** Who else in the team is off on the leave dates and the day either side (prototype coverHtml). */
export function TeamCover({ from, to, teamOff, team, location, dept }: { from: string; to: string; teamOff?: any[]; team?: number; location?: string; dept?: string }) {
  const days: { d: string; list: string[] }[] = [];
  if (from && to && to >= from) {
    for (let d = addDays(from, -1), end = addDays(to, 1); d <= end && days.length < 62; d = addDays(d, 1)) {
      const list = (teamOff || []).filter((x) => x.from <= d && x.to >= d).map((x) => `${x.name} ${x.status === 'Pending' ? 'has leave waiting for approval' : 'on approved leave'}`);
      if (list.length) days.push({ d, list });
    }
  }
  const peers = Math.max(0, (team ?? 1) - 1);
  const inside = days.some((x) => x.d >= from && x.d <= to);
  return <div className={`note ${inside ? 'bad' : ''}`} style={{ marginTop: 10, fontSize: 13 }}><b>Team cover</b> · {[location, dept].filter(Boolean).join(', ')}{location || dept ? ', ' : ''}{peers} other{peers === 1 ? '' : 's'}
    {days.length ? <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{days.map((x) => <li key={x.d}><b>{fmtD(x.d)}{x.d < from ? ' (day before)' : x.d > to ? ' (day after)' : ''}</b>: {x.list.join(', ')}</li>)}</ul>
      : <div style={{ marginTop: 4 }}>Nobody else in the team is off on these days or either side.</div>}</div>;
}

/** A short leave's length, "2h" or "1h 30m". */
export const shortHrs = (p: any) => {
  const m = (t: string) => { const [h, mm] = String(t || '').split(':').map(Number); return h * 60 + mm; };
  const n = m(p.toTime) - m(p.fromTime);
  return n > 0 ? `${Math.floor(n / 60) ? `${Math.floor(n / 60)}h` : ''}${n % 60 ? ` ${n % 60}m` : ''}`.trim() : '';
};

export const SHORT = 'Short leave';

/** The leave payload as the server takes it: a short leave's times, a half day's half, else the dates. */
export function leavePayload(cur: any, p: any, to: string) {
  if (cur?.short) return { short: true, ltype: SHORT, from: p.from, to: p.from, fromTime: p.fromTime, toTime: p.toTime };
  return { ltype: cur?.type, from: p.from, to, half: p.half || undefined, halfOf: p.half ? p.halfOf : undefined, doc: p.doc || undefined };
}

/** The prototype's leave count line: how many days the leave type counts, and the weekly offs and holidays inside. */
/** What a paid day is for leave earned by working days (owner, 24 Nov 2026, evening: "Working days means Paid days for leaves"). */
export const PAID_DAYS = 'paid days (present, half day 0.5, paid leave, weekly off and holiday; loss of pay days do not count)';
/** 7 → "7th", 1 → "1st", 22 → "22nd"; a half number as it is ("10.5th"). */
export const ordinal = (v: number) => `${v}${Number.isInteger(v) && (v % 100 < 11 || v % 100 > 13) ? (['th', 'st', 'nd', 'rd'][v % 10] ?? 'th') : 'th'}`;
/** How a balance earned by working days was earned (owner, 24 Nov 2026; evening: paid days, credited the day after the
 *  Nth paid day), from the balance's `worked`; '' for others. */
export function workedLine(w: any): string {
  if (!w) return '';
  const n = (v: number, x: string) => `${v} ${x}${v === 1 ? '' : 's'}`;
  const capped = w.earned > w.credit ? `, capped at ${w.cap} for the leave year` : '';
  const upto = w.through ? `${n(w.days, 'paid day')} up to ${fmtD(w.through)}` : 'no paid day counted yet';
  const last = w.credits?.length ? `, last credited on ${fmtD(w.credits[w.credits.length - 1].on)}` : '';
  return `Earned by working days, ${n(w.per, 'day')} for every ${n(w.every, 'paid day')}, credited the day after the ${ordinal(w.every)} paid day: `
    + `${w.credit} earned from ${upto}${last}${capped}; ${n(w.toNext, 'more paid day')} to the next block`;
}

export function leaveCountLine(ltype: string, days: number, offs: number, range: number): ReactNode {
  const hide = range - days;
  return <><b>{days === 0.5 ? 'Half a day' : `${days} day${days === 1 ? '' : 's'}`}</b> will be counted as {ltype.toLowerCase()}{offs ? `. ${offs} weekly off or holiday day${offs > 1 ? 's are' : ' is'} inside these dates${hide > 0 ? ` and ${hide > 1 ? 'are' : 'is'} not counted` : ' and counted'}` : ''}.</>;
}

/** Joining details Payroll checks before the employee becomes active (prototype verifyBlock): every detail, with
 *  Edit details while the request waits on you, and every document with View (and Replace while it waits on you). */
type VField = { key: string; label: string; kind: string; options?: string | null; join: number; types: string[]; threshold?: number };
type VDoc = { key: string; label: string; state: 'Mandatory' | 'Optional' | 'Other'; uploaded: boolean; id?: string; file_name?: string; uploaded_by?: string; uploaded_at?: string };
const vStr = (m: Record<string, unknown> | null | undefined): Record<string, string> => Object.fromEntries(Object.entries(m || {}).filter(([, v]) => v != null && typeof v !== 'object').map(([k, v]) => [k, String(v)]));
const vApplies = (f: VField, d: Record<string, string>) => f.key === 'uan' ? d.uanAvail === 'Yes'
  : f.key === 'spouse' ? d.marital === 'Married'
    : f.key === 'pfChoice' ? d.uanAvail === 'No' && Number(d.gross) > (f.threshold || 25000)
      : true;

/** Open a document of a joining verification in a new tab (through the request, so whoever may see it may open it). */
async function openVerifyDoc(id: string, doc: string) {
  const w = window.open('', '_blank');
  const res = await fetch(`/api/approvals/${id}/verify-docs/${doc}`, { headers: { Authorization: `Bearer ${session.token}`, 'X-Role': session.role } });
  if (!res.ok) { w?.close(); throw new ApiError(res.status, (await res.json().catch(() => ({}))).error || 'Could not open the file'); }
  const url = URL.createObjectURL(await res.blob());
  if (w) w.location.href = url; else window.open(url, '_blank');
}

/** The lists the choices of a joining detail come from, loaded when editing starts. */
function useVerifyLists(on: boolean) {
  const get = (path: string) => (on ? api<any>(path).catch(() => null) : Promise.resolve(null));
  const shifts = useLoad(() => get('/masters/shift?status=Active'), [on]);
  const depts = useLoad(() => get('/masters/department'), [on]);
  const modes = useLoad(() => get('/masters/attendance_mode?status=Active'), [on]);
  const quals = useLoad(() => get('/masters/qualification?status=Active'), [on]);
  return (f: VField, d: Record<string, string>): [string, string][] | null => {
    switch (f.key) {
      case 'shift': { // the shifts mapped to the joiner's department and location (shift_maps.go)
        if (!shifts.data) return null;
        const deptId = ((depts.data as any[]) ?? []).find((x) => x.name === d.dept)?.id ?? '';
        return (shifts.data as any[]).filter((s) => shiftOffered(s, deptId, d.loc ?? '')).map((s) => [s.code, `${s.name} (${s.start_time}–${s.end_time}${s.start2_time ? ` + ${s.start2_time}–${s.end2_time}` : ''})`]);
      }
      case 'attMode': return modes.data ? (modes.data as any[]).map((x) => [x.name, x.name]) : null;
      case 'qual': return quals.data ? (quals.data as any[]).map((x) => [x.name, x.name]) : null;
    }
    const o = (f.options ?? '').split(',').map((x) => x.trim()).filter(Boolean);
    return f.kind === 'sel' && o.length ? o.map((x) => [x, x]) : null;
  };
}

function VerifyPanel({ id, o, canEdit, onSaved }: { id: string; o: any; canEdit: boolean; onSaved: () => void }) {
  const v = useLoad(() => api<{ docs: VDoc[]; fields: VField[]; mandatoryDone: number; mandatoryTotal: number; canReplace: boolean }>(`/approvals/${id}/verify-docs`), [id, o]);
  const stored = vStr(o.data);
  const [edit, setEdit] = useState(false);
  const [d, setD] = useState<Record<string, string>>(stored);
  const [errs, setErrs] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  useEffect(() => { setEdit(false); setErrs([]); }, [id]);
  const lists = useVerifyLists(edit);
  const all: VField[] = v.data?.fields ?? (o.joinFields || []);
  const vals = edit ? d : stored;
  const fields = all.filter((f) => vApplies(f, vals));
  const start = () => { setD(stored); setErrs([]); setEdit(true); };
  const save = async () => {
    const changed = Object.fromEntries(fields.filter((f) => (d[f.key] ?? '') !== (stored[f.key] ?? '')).map((f) => [f.key, d[f.key] ?? '']));
    if (!Object.keys(changed).length) { setEdit(false); return; }
    setBusy(true); setErrs([]);
    try {
      const r = await api(`/approvals/${id}/verify-details`, { method: 'PUT', body: { data: changed } });
      toast(r.changed?.length ? `Saved: ${r.changed.join(', ')}${r.employeeUpdated ? '. The employee record is updated too' : ''}` : 'Nothing changed');
      setEdit(false); onSaved();
    } catch (x: any) { setErrs((x.details as string[]) || [x.message]); } finally { setBusy(false); }
  };
  const replace = async (key: string, file?: File) => {
    if (!file) return;
    try { const r = await postForm(`/approvals/${id}/verify-docs`, { docKey: key }, file); toast(r.note || `Replaced with ${file.name}`); v.reload(); onSaved(); } catch (x: any) { toast(x.message); }
  };
  const control = (f: VField) => {
    const val = d[f.key] ?? '';
    const set = (x: string) => setD({ ...d, [f.key]: x });
    // Reporting managers: any active employee, found by typing (PersonPicker).
    if (f.key === 'l1' || f.key === 'l2') return <PersonPicker label={f.label} value={val} onChange={(id) => set(id)} search="&status=Active" placeholder="Select" />;
    const opts = lists(f, d);
    if (opts) {
      const list = val && !opts.some(([k]) => k === val) ? [[val, val] as [string, string], ...opts] : opts;
      return <select aria-label={f.label} value={val} onChange={(e) => set(e.target.value)} style={{ width: '100%' }}><option value="">Select</option>{list.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>;
    }
    return <input aria-label={f.label} type={f.kind === 'date' ? 'date' : f.kind === 'email' ? 'email' : f.kind === 'tel' ? 'tel' : f.kind === 'number' ? 'number' : 'text'} value={val} onChange={(e) => set(e.target.value)} style={{ width: '100%' }} />;
  };
  const docs = v.data?.docs ?? [];
  return <>
    <div className="row sp" style={{ margin: '18px 0 8px', flexWrap: 'wrap', gap: 8 }}>
      <h3 style={{ margin: 0 }}>Details to verify</h3>
      {canEdit && (edit
        ? <div className="row" style={{ gap: 8 }}><button className="btn sm" disabled={busy} onClick={() => { setEdit(false); setErrs([]); }}>Cancel</button><button className="btn primary sm" disabled={busy} onClick={save}>Save</button></div>
        : <button className="btn sm" onClick={start}>Edit details</button>)}
    </div>
    {edit && <p className="mute" style={{ margin: '0 0 8px', fontSize: 12.5 }}>Correct what is wrong and save. The change is written in the request's history and on the employee record.</p>}
    <div className="tw"><table><tbody>
      {fields.map((f) => <tr key={f.key}><td style={{ width: '40%' }}>{f.label} {f.join === 2 && <Pill k="n">Mandatory</Pill>}</td>
        <td>{edit ? control(f) : vals[f.key] ? vals[f.key] : <span className="mute">Not given</span>}</td></tr>)}
    </tbody></table></div>
    {errs.length > 0 && <div className="note bad" style={{ marginTop: 8 }}><ul style={{ margin: 0, paddingLeft: 18 }}>{errs.map((x) => <li key={x}>{x}</li>)}</ul></div>}
    {edit && <div className="row" style={{ marginTop: 8, justifyContent: 'flex-end', gap: 8 }}><button className="btn" disabled={busy} onClick={() => { setEdit(false); setErrs([]); }}>Cancel</button><button className="btn primary" disabled={busy} onClick={save}>Save</button></div>}
    <h3 style={{ margin: '18px 0 8px' }}>Documents{v.data ? <span className="mute" style={{ fontWeight: 400, fontSize: 13 }}> · {v.data.mandatoryDone} of {v.data.mandatoryTotal} mandatory uploaded</span> : null}</h3>
    {!v.data ? <Loading error={v.error} /> : <div className="tw"><table><tbody>
      {docs.length ? docs.map((x) => <tr key={x.key}>
        <td style={{ width: '40%' }}><b>{x.label}</b> {x.state === 'Mandatory' ? <Pill k="bad">Mandatory</Pill> : x.state === 'Optional' ? <Pill k="n">Optional</Pill> : <Pill k="info">Also on file</Pill>}</td>
        <td>{x.uploaded ? <Pill k="ok">Uploaded</Pill> : <Pill k={x.state === 'Mandatory' ? 'bad' : 'warn'}>Missing</Pill>}
          {x.uploaded && <div className="mute" style={{ fontSize: 12, marginTop: 2, wordBreak: 'break-all' }}>{x.file_name}{x.uploaded_by ? ` · by ${x.uploaded_by}` : ''}</div>}</td>
        <td><div className="upl" style={{ justifyContent: 'flex-end' }}>
          {x.uploaded && x.id && <button className="btn sm" onClick={() => openVerifyDoc(id, x.id!).catch((e) => toast(e.message))}>View</button>}
          {v.data!.canReplace && x.state !== 'Other' && <label className="btn sm">{x.uploaded ? 'Replace' : 'Upload'}<input type="file" accept=".pdf,.jpg,.jpeg,.png" hidden onChange={(e) => { replace(x.key, e.target.files?.[0]); e.target.value = ''; }} /></label>}
        </div></td>
      </tr>) : <tr><td className="mute">No documents are asked at joining.</td></tr>}
    </tbody></table></div>}
  </>;
}

/** Payroll, at its own step, changes how an advance is paid and recovered before approving (PUT advance-terms). */
function AdvanceTerms({ id, p, onSaved }: { id: string; p: any; onSaved: () => void }) {
  const sch = useLoad(() => api<any>('/payroll/schedule').catch(() => null), []);
  const open: string = sch.data?.openYm ?? '';
  const pay0 = p.payMonth || open, start0 = p.startYm || (pay0 && addMonthsYm(pay0, 1));
  const [edit, setEdit] = useState(false);
  const [v, setV] = useState({ instalments: String(p.instalments || 1), payMonth: pay0, startYm: start0 });
  const [busy, setBusy] = useState(false);
  useEffect(() => { setV({ instalments: String(p.instalments || 1), payMonth: pay0, startYm: start0 }); }, [p.instalments, pay0, start0]);
  if (!open) return null;
  const opt = (ms: string[]) => ms.map((m) => <option key={m} value={m}>{ymLabel(m)}</option>);
  const save = async () => {
    setBusy(true);
    try { const r = await api(`/approvals/${id}/advance-terms`, { method: 'PUT', body: { instalments: +v.instalments, payMonth: v.payMonth, startYm: v.startYm } }); toast(r.message || 'Saved'); setEdit(false); onSaved(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  return <div style={{ border: '1px solid var(--line,#ddd)', borderRadius: 10, padding: 12, marginBottom: 10 }} data-testid="adv-terms">
    <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}><div><b>Recovery</b>
      <div className="mute" style={{ fontSize: 12.5 }}>{p.instalments || 1} instalment{(p.instalments || 1) === 1 ? '' : 's'} from {ymLabel(start0)}, paid with {ymLabel(pay0)}. You can change this before you approve.</div></div>
      {!edit && <button className="btn sm" onClick={() => setEdit(true)}>Change recovery</button>}</div>
    {edit && <><div className="row" style={{ gap: 10, flexWrap: 'wrap', marginTop: 10 }}>
      <label className="field" style={{ margin: 0 }}><span>Paid with payroll of</span><select value={v.payMonth} onChange={(e) => setV({ ...v, payMonth: e.target.value, startYm: v.startYm > e.target.value ? v.startYm : addMonthsYm(e.target.value, 1) })}>{opt([open, addMonthsYm(open, 1)])}</select></label>
      <label className="field" style={{ margin: 0 }}><span>Instalments</span><select value={v.instalments} onChange={(e) => setV({ ...v, instalments: e.target.value })}>{Array.from({ length: 12 }, (_, i) => i + 1).map((k) => <option key={k} value={k}>{k}</option>)}</select></label>
      <label className="field" style={{ margin: 0 }}><span>First recovery month</span><select value={v.startYm} onChange={(e) => setV({ ...v, startYm: e.target.value })}>{opt([1, 2, 3].map((k) => addMonthsYm(v.payMonth, k)))}</select></label>
    </div>
    <div className="row" style={{ marginTop: 10, gap: 8 }}><button className="btn sm primary" disabled={busy} onClick={save}>Save the change</button><button className="btn sm" onClick={() => setEdit(false)}>Cancel</button></div></>}
  </div>;
}
const addMonthsYm = (ym: string, n: number) => { const d = new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1 + n, 1); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`; };

// ---------- Bulk approval (Payroll approvals and Joining approvals tabs) ----------

type Bulk = 'pay' | 'join';
/** Request types decided in bulk on each tab: tax proofs are reviewed on the tax screen. */
const BULK_WF: Record<Bulk, string[]> = { pay: PAY_GROUP.filter((k) => k !== 'p_tax'), join: JOIN_GROUP };
const canPick = (g: Bulk, r: any) => !!r.canAct && r.status === 'Pending' && BULK_WF[g].includes(r.wf);
type BulkRow = { line?: number; id: string; name: string; type: string; decision: string; comment: string; stage: string; outcome: string; reason?: string; next?: string; changes?: { field: string; from: string; to: string }[] };
const OUTCOME: Record<string, [string, 'ok' | 'warn' | 'bad' | 'info' | 'n']> = {
  approve: ['Will approve', 'ok'], return: ['Will return', 'warn'], correct: ['Corrections only', 'info'], skip: ['Skipped', 'n'],
  approved: ['Approved', 'ok'], returned: ['Returned', 'warn'], corrected: ['Corrected', 'info'], skipped: ['Skipped', 'n'], failed: ['Failed', 'bad'],
};

/** Download for bulk approval, Upload decisions, and Approve selected. */
function BulkBar({ group, rows, picked, setPicked, onDone }: { group: Bulk; rows: any[]; picked: Set<string>; setPicked: (s: Set<string>) => void; onDone: () => void }) {
  const { me } = useAuth();
  const [file, setFile] = useState<File | null>(null);
  const [preview, setPreview] = useState<any>(null);
  const [result, setResult] = useState<any>(null);
  const [ask, setAsk] = useState(false);
  const [busy, setBusy] = useState(false);
  const pickable = rows.filter((r) => canPick(group, r));
  const all = pickable.length > 0 && pickable.every((r) => picked.has(r.id));
  const tab = group === 'pay' ? 'Payroll approvals' : 'Joining approvals';
  const fetchFile = async () => {
    try { await download(`/approvals/bulk/export?group=${group}`, `${tab.toLowerCase().replace(' ', '-')}.xlsx`); } catch (x: any) { toast(x.message); }
  };
  const upload = async (f?: File) => {
    if (!f) return;
    setBusy(true);
    try { setPreview(await postForm(`/approvals/bulk/preview?group=${group}`, {}, f)); setFile(f); } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const confirm = async () => {
    if (!file) return;
    setBusy(true);
    try { const r = await postForm(`/approvals/bulk/apply-file?group=${group}`, {}, file); setPreview(null); setFile(null); setResult(r); onDone(); } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const approveSel = async () => {
    setBusy(true);
    try {
      const items = rows.filter((r) => picked.has(r.id)).map((r) => ({ id: r.id, decision: 'approve', stage: r.waitingOn, comment: '' }));
      const r = await api('/approvals/bulk/apply', { body: { group, source: 'select', items } });
      setAsk(false); setResult(r); onDone();
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const admin = me?.ctx.role === 'HR Admin';
  return <>
    <div className="card" style={{ marginBottom: 14, padding: '12px 14px' }}>
      <div className="row sp" style={{ flexWrap: 'wrap', gap: 10 }}>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <label className="row" style={{ gap: 6, fontSize: 13, cursor: pickable.length ? 'pointer' : 'default' }}>
            <input type="checkbox" disabled={!pickable.length} checked={all} onChange={(e) => setPicked(new Set(e.target.checked ? pickable.map((r) => r.id) : []))} />
            Select all I can approve{pickable.length ? ` (${pickable.length})` : ''}</label>
          <button className="btn primary sm" disabled={!picked.size || busy || admin} title={admin ? 'HR Admin approves on behalf of a stage with a comment: use the file or open each request' : ''} onClick={() => setAsk(true)}>Approve selected ({picked.size})</button>
        </div>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <button className="btn sm" onClick={fetchFile}>Download for bulk approval</button>
          <label className="btn sm" aria-disabled={busy}>{busy && !ask ? 'Reading…' : 'Upload decisions'}<input type="file" accept=".xlsx" hidden onChange={(e) => { upload(e.target.files?.[0]); e.target.value = ''; }} /></label>
        </div>
      </div>
      <div className="mute" style={{ fontSize: 12.5, marginTop: 6 }}>Many to decide? Download the Excel file of everything waiting on you, write Approve or Return against each{group === 'join' ? ' (and correct joining details in the blue columns)' : ''}, then upload it. You see a preview first; nothing changes until you confirm.</div>
    </div>
    {ask && <Modal onClose={() => setAsk(false)}>
      <h2>Approve {picked.size} request{picked.size === 1 ? '' : 's'}?</h2>
      <p className="ink2" style={{ fontSize: 13.5 }}>Each is approved for your own stage only, as with the Approve button. Where another stage follows, the request moves on to it.</p>
      <ul style={{ margin: '0 0 12px', paddingLeft: 18, fontSize: 13, maxHeight: 220, overflow: 'auto' }}>{rows.filter((r) => picked.has(r.id)).map((r) => <li key={r.id}>{r.employee_name} · {wfName(r.wf)} · {r.id}</li>)}</ul>
      <div className="row" style={{ justifyContent: 'flex-end', gap: 8 }}><button className="btn" onClick={() => setAsk(false)}>Cancel</button><button className="btn primary" disabled={busy} onClick={approveSel}>Approve {picked.size}</button></div>
    </Modal>}
    {preview && <Modal wide onClose={() => { setPreview(null); setFile(null); }}>
      <h2>Check before you confirm</h2>
      <p className="mute" style={{ fontSize: 12.5, marginTop: 0 }}>{file?.name} · {preview.total} row{preview.total === 1 ? '' : 's'}. Nothing has changed yet. Every row is checked again when you confirm.</p>
      <BulkCounts r={preview} keys={['approve', 'return', 'correct', 'skip']} />
      <BulkTable rows={preview.rows} />
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}>
        <button className="btn" onClick={() => { setPreview(null); setFile(null); }}>Cancel</button>
        <button className="btn primary" disabled={busy || !(preview.approve + preview.return + preview.correct)} onClick={confirm}>
          {busy ? 'Applying…' : `Confirm: ${[preview.approve && `approve ${preview.approve}`, preview.return && `return ${preview.return}`, preview.corrections && `save corrections on ${preview.corrections}`].filter(Boolean).join(', ') || 'nothing to do'}`}</button>
      </div>
    </Modal>}
    {result && <Modal wide onClose={() => setResult(null)}>
      <h2>Done</h2>
      <BulkCounts r={result} keys={['approved', 'returned', 'corrected', 'skipped', 'failed']} />
      {result.corrections > 0 && <p className="mute" style={{ fontSize: 12.5 }}>Joining details corrected on {result.corrections} request{result.corrections === 1 ? '' : 's'}.</p>}
      <BulkTable rows={result.rows} />
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn primary" onClick={() => setResult(null)}>Close</button></div>
    </Modal>}
  </>;
}

function BulkCounts({ r, keys }: { r: any; keys: string[] }) {
  return <div className="row" style={{ gap: 8, flexWrap: 'wrap', margin: '6px 0 12px' }}>{keys.filter((k) => r[k] > 0 || ['approve', 'approved', 'skip', 'skipped'].includes(k)).map((k) => <Pill key={k} k={OUTCOME[k][1]}>{OUTCOME[k][0]}: {r[k] ?? 0}</Pill>)}</div>;
}

function BulkTable({ rows }: { rows: BulkRow[] }) {
  return <div className="tw" style={{ maxHeight: 380, overflow: 'auto' }}><table>
    <thead><tr><th>Row</th><th>Request</th><th>What happens</th><th>Why, or what changes</th></tr></thead>
    <tbody>{rows.map((x, i) => <tr key={i}>
      <td className="mute">{x.line ?? i + 1}</td>
      <td><b>{x.name || x.id || '—'}</b><div className="mute" style={{ fontSize: 12 }}>{[x.id, x.type].filter(Boolean).join(' · ')}</div></td>
      <td><Pill k={OUTCOME[x.outcome]?.[1] ?? 'n'}>{OUTCOME[x.outcome]?.[0] ?? x.outcome}</Pill>{x.next && <div className="mute" style={{ fontSize: 12 }}>{x.next === 'Fully approved' ? 'Fully approved' : `Next: ${x.next}`}</div>}</td>
      <td style={{ fontSize: 12.5 }}>{x.reason}{x.comment && !x.reason ? <span className="mute">“{x.comment}”</span> : null}
        {x.changes?.length ? <ul style={{ margin: '4px 0 0', paddingLeft: 16 }}>{x.changes.map((c) => <li key={c.field}>{c.field}: {c.from} → <b>{c.to}</b></li>)}</ul> : null}</td>
    </tr>)}</tbody></table></div>;
}

// ---------- New request ----------

/** The new request dialog (prototype nrModal). My view › Separation opens it for "Withdraw resignation" (`title`). */
export function NewRequest({ wf, title, onClose, onDone }: { wf: string; title?: string; onClose: () => void; onDone: () => void }) {
  const { can, me } = useAuth();
  const role = me?.ctx.role || '';
  const myId = me?.ctx.employeeId || '';
  const [emp, setEmp] = useState('');
  const [p, setP] = useState<Record<string, any>>(() => (
    wf === 'leave' ? { from: today() } : wf === 'regularize' ? { date: today(), reason: REASONS[0], in: '09:00', out: '18:00' } : {}));
  const [errs, setErrs] = useState<string[]>([]);
  const [preview, setPreview] = useState<any>(null);
  const [simErr, setSimErr] = useState('');
  const [docName, setDocName] = useState('');
  // Everyone in the caller's data access (the prototype's scoped()); an employee raises only for themselves.
  // The first few people in the role view's data access, for the default choice; the picker searches all of them.
  // Whether the person themselves is in it decides the default, as before (an HR Admin raises for themselves first, a
  // line manager for their first report).
  const team = useLoad(() => (role !== 'Employee' && can('emp.directory')
    ? Promise.all([api('/employees?status=Active&size=3'), myId ? api(`/employees?status=Active&size=1&q=${encodeURIComponent(myId)}`) : Promise.resolve({ rows: [] })])
      .then(([a, b]) => { const me1 = (b.rows as Person[]).find((x) => x.id === myId); return [...(me1 ? [me1] : []), ...(a.rows as Person[]).filter((x) => x.id !== myId)]; })
    : Promise.resolve([] as Person[])), [role]);
  const [picked, setPicked] = useState<Person | null>(null);
  const [mgr, setMgr] = useState<Person | null>(null);
  const self: Person = { id: myId, name: me?.employee.name || me?.ctx.name || myId };
  // As the prototype's nrEmps: the people in the role view's data access (a line manager's team, without themselves);
  // only someone who sees nobody else raises for themselves.
  const solo = !team.data?.length;
  const list: Person[] = team.data?.length ? team.data : [self];
  const who = emp || (wf === 'transfer' ? list.find((e) => e.id !== myId)?.id || myId : list.some((e) => e.id === myId) ? myId : list[0].id);
  const target = picked?.id === who ? picked : list.find((e) => e.id === who);
  const cities = useLoad(() => (wf === 'transfer' ? api<any[]>('/masters/city?status=Active') : Promise.resolve([])), [wf]);
  const locs = useLoad(() => (wf === 'transfer' ? api<any[]>('/masters/location?status=Active') : Promise.resolve([])), [wf]);
  const depts = useLoad(() => (wf === 'transfer' ? api<any[]>('/masters/department?status=Active') : Promise.resolve([])), [wf]);
  const desigs = useLoad(() => (wf === 'transfer' ? api<any[]>('/masters/designation?status=Active') : Promise.resolve([])), [wf]);
  const sep = useLoad(() => (wf === 'resign' ? api(`/me/separation${who !== myId ? `?employeeId=${who}` : ''}`) : Promise.resolve(null)), [wf, who]);
  // Leave types and balances come from the employee's leave plan.
  const bal = useLoad(() => (wf === 'leave' ? api(`/leave/balances/${who === myId ? 'me' : who}`).catch(() => null) : Promise.resolve(null)), [wf, who]);
  // A short leave (part of a day) is offered as the type "Short leave" when the employee's leave plan allows it.
  const ltypes: any[] = [...(bal.data?.types ?? []), ...(bal.data?.short ? [{ type: SHORT, short: true }] : [])];
  const lt = ltypes.find((t) => t.type === p.ltype) ?? ltypes[0];
  const isShort = !!lt?.short;
  // The system record for the day being corrected.
  const ym = String(p.date || '').slice(0, 7);
  const recOk = wf === 'regularize' && !!ym && (who === myId || can('att.records'));
  const month = useLoad(() => (recOk ? api(`/attendance/month/${who === myId ? 'me' : who}?ym=${ym}`).catch(() => null) : Promise.resolve(null)), [recOk, who, ym]);
  const rec = (month.data?.month?.days as any[] | undefined)?.find((d) => d.date === p.date);
  const rtype = p.rtype || (rec?.in && rec?.out ? 'All Other' : 'No Punch');
  const set = (k: string, v: any) => setP({ ...p, [k]: v });
  const city = p.toCity || target?.city_id || cities.data?.[0]?.id;
  const cityLocs = (locs.data || []).filter((l) => l.city_id === city);
  // A resignation already accepted is changed or withdrawn instead (prototype nrForm rkind); the Employee view only withdraws.
  const n = sep.data?.notice;
  const onNotice = n?.exitStatus === 'On notice', emp0 = role === 'Employee';
  const rkinds: [string, string][] = onNotice ? (emp0 ? [['withdraw', 'Withdraw resignation']] : [['change', 'Change last working day'], ['withdraw', 'Withdraw resignation']]) : [['new', 'New resignation']];
  const rkind = rkinds.some(([k]) => k === p.rkind) ? p.rkind as string : rkinds[0][0];
  const body = () => {
    let x: Record<string, any> = {};
    // scoreOverride: HR's reason for letting this one request through a low discipline score (audited).
    if (wf === 'leave') x = { ...leavePayload(lt, { ...p, halfOf: p.halfOf || 'first' }, p.half ? p.from : p.to || p.from), note: p.note, doc: isShort ? undefined : p.doc,
      scoreOverride: p.scoreOverride?.trim() || undefined };
    if (wf === 'regularize') x = { date: p.date, rtype, reason: p.reason, in: p.in, out: p.out, note: p.note };
    if (wf === 'transfer') x = { toLocation: p.toLocation || cityLocs[0]?.id, toDepartment: p.toDepartment, toDesignation: p.toDesignation, toManager: p.toManager, effective: p.effective, note: p.note };
    if (wf === 'resign') {
      const reason = p.reason || sep.data?.reasons?.[0];
      x = rkind === 'change' ? { rkind, lwd: p.lwd || n?.lwd, waive: !!p.waive, reason, note: p.note }
        : rkind === 'withdraw' ? { rkind, reason, note: p.note } : { exitType: 'Resignation', reason, note: p.note };
    }
    return { wf, employeeId: who, payload: x };
  };
  // Anyone previews the route of their own request; another employee's needs the approvals view.
  // An employee who takes no leave has nothing to apply for (prototype: "No eligible employee in your scope").
  const noLeave = wf === 'leave' && !!bal.data && !ltypes.length;
  const simulate = (who === myId || can('appr.queue')) && !(wf === 'leave' && !lt);
  useEffect(() => {
    if (!simulate) { setPreview(null); setSimErr(''); return; }
    const t = setTimeout(() => api('/approvals/simulate', { body: body() }).then((x) => { setPreview(x); setSimErr(''); }).catch((x) => { setPreview(null); setSimErr(x.message); }), 250);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [JSON.stringify(p), who, rtype, lt?.type, cityLocs.length, sep.data]);
  // The prototype checks a transfer as it is filled in (nrErrors); the server checks the same when it is sent.
  const trErrs: string[] = [];
  if (wf === 'transfer' && target) {
    const loc = p.toLocation || cityLocs[0]?.id;
    if (!loc) trErrs.push('Pick a location');
    else if (loc === target.location_id && (!p.toDepartment || p.toDepartment === target.department)) trErrs.push('That is the employee\'s current location and department');
    if (!p.effective) trErrs.push('Pick the effective date');
    else if (p.effective <= today()) trErrs.push('Effective date must be in the future');
  }
  const send = async (skip = '') => {
    if (trErrs.length) { toast(trErrs[0]); return; }
    if (noLeave && solo) return; // nothing to apply for, as in the prototype
    setErrs([]);
    try { await api('/approvals' + skip, { body: body() }); toast(skip ? 'Approved: approval skipped' : 'Sent for approval'); onDone(); } catch (x: any) { const l = (x.details as string[]) || [x.message]; setErrs(l); toast(l[0]); }
  };
  const problems = [...new Set([...(simErr ? [simErr] : []), ...(preview?.problems ?? []), ...trErrs, ...(wf === 'resign' && rkind === 'new' && n && !n.canResign && n.why ? [n.why] : []), ...errs])];
  const empSel = <Field label="Employee">{solo
    ? <PersonPicker label="Employee" people={list} value={who} onChange={() => {}} required />
    : <PersonPicker label="Employee" search="&status=Active" value={who} selected={target ?? null} required
      onChange={(id, x) => { if (!id) return; setPicked(x); setEmp(id); setErrs([]); if (p.toManager === id) { setP({ ...p, toManager: '' }); setMgr(null); } }} />}</Field>;
  return (
    <Modal wide onClose={onClose}>
      <div className="row sp"><h2>{title ?? `New ${WORKFLOWS[wf].toLowerCase()} request`}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
      <div style={{ marginTop: 12 }}>
        {wf === 'leave' && noLeave && solo && <p className="mute">No eligible employee in your scope. Contractual and retainer staff cannot apply for leave.</p>}
        {wf === 'leave' && !(noLeave && solo) && <div className="form">
          {empSel}
          <Field label="Leave type"><select value={lt?.type ?? ''} onChange={(e) => set('ltype', e.target.value)}>{ltypes.map((t) => <option key={t.type}>{t.type}</option>)}</select></Field>
          <Field label={isShort ? 'Date' : 'From'}><input type="date" value={p.from || ''} onChange={(e) => set('from', e.target.value)} /></Field>
          {isShort ? <><Field label="From time"><input type="time" value={p.fromTime || ''} onChange={(e) => set('fromTime', e.target.value)} /></Field>
            <Field label="To time"><input type="time" value={p.toTime || ''} onChange={(e) => set('toTime', e.target.value)} /></Field></>
            : !p.half && <Field label="To"><input type="date" value={p.to || p.from || ''} onChange={(e) => set('to', e.target.value)} /></Field>}
          {bal.data?.half && !isShort && <Field label="Half day"><select value={p.half ? p.halfOf || 'first' : ''} onChange={(e) => setP({ ...p, half: !!e.target.value, halfOf: e.target.value || undefined })}>
            <option value="">No, full days</option><option value="first">First half on leave</option><option value="second">Second half on leave</option></select></Field>}
          <label className="field full"><span>Reason</span><input type="text" value={p.note || ''} onChange={(e) => set('note', e.target.value)} /></label>
          {!isShort && lt?.docAfter > 0 && <label className="field full"><span>{lt.docLabel || 'Document'} (needed when more than {lt.docAfter} day{lt.docAfter > 1 ? 's' : ''})</span>
            <input type="file" accept="application/pdf,image/png,image/jpeg" onChange={async (e) => {
              const f = e.target.files?.[0]; if (!f) return;
              try { const d = await uploadDoc('employee', who, `leave-${Date.now()}`, f); setP({ ...p, doc: d.id }); setDocName(f.name); } catch (x: any) { toast(x.message); }
            }} />{docName && <small className="mute">Attached: {docName}</small>}</label>}
        </div>}
        {wf === 'regularize' && <div className="form">
          {empSel}
          <Field label="Date"><input type="date" value={p.date || ''} max={today()} onChange={(e) => set('date', e.target.value)} /></Field>
          <Field label="Reason"><select value={p.reason} onChange={(e) => set('reason', e.target.value)}>{REASONS.map((r) => <option key={r}>{r}</option>)}</select></Field>
          <Field label="What needs correcting"><select value={rtype} onChange={(e) => set('rtype', e.target.value)}><option value="No Punch">No punch (missing in or out)</option><option value="All Other">Late, early or wrong time</option></select></Field>
          <Field label="In time"><input type="time" value={p.in || ''} onChange={(e) => set('in', e.target.value)} /></Field>
          <Field label="Out time"><input type="time" value={p.out || ''} onChange={(e) => set('out', e.target.value)} /></Field>
          <label className="field full"><span>Note</span><input type="text" value={p.note || ''} onChange={(e) => set('note', e.target.value)} /></label>
        </div>}
        {wf === 'transfer' && <div className="form">
          {empSel}
          <Field label="Move to city"><select value={city || ''} onChange={(e) => setP({ ...p, toCity: e.target.value, toLocation: '' })}>{cities.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
          <Field label="Move to location"><select value={p.toLocation || cityLocs[0]?.id || ''} onChange={(e) => set('toLocation', e.target.value)}>{cityLocs.map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</select></Field>
          <Field label="Move to department (optional)"><select value={p.toDepartment || ''} onChange={(e) => setP({ ...p, toDepartment: e.target.value, toDesignation: '' })}><option value="">Same department</option>{depts.data?.map((d) => <option key={d.id}>{d.name}</option>)}</select></Field>
          {p.toDepartment && <Field label="New designation"><select value={p.toDesignation || ''} onChange={(e) => set('toDesignation', e.target.value)}><option value="">Choose</option>{desigs.data?.filter((d) => d.department === p.toDepartment).map((d) => <option key={d.id}>{d.name}</option>)}</select></Field>}
          <Field label="Receiving manager">{solo
            ? <PersonPicker label="Receiving manager" people={[]} value="" onChange={() => {}} empty="Keep current manager" />
            : <PersonPicker label="Receiving manager" search="&status=Active" exclude={[who]} value={p.toManager || ''} selected={mgr} empty="Keep current manager"
              onChange={(id, x) => { setMgr(x); set('toManager', id); }} />}</Field>
          <Field label="Effective from"><input type="date" value={p.effective || ''} min={addDays(today(), 1)} onChange={(e) => set('effective', e.target.value)} /></Field>
          <label className="field full"><span>Reason</span><input type="text" value={p.note || ''} onChange={(e) => set('note', e.target.value)} /></label>
        </div>}
        {wf === 'resign' && <>
          <div className="form">
            {empSel}
            <Field label="Request"><select value={rkind} onChange={(e) => set('rkind', e.target.value)}>{rkinds.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
            <Field label={onNotice ? 'New last working day' : 'Proposed last working day'}>{!onNotice || emp0
              ? <input type="date" value={(onNotice ? n?.currentLwd : '') || n?.lwd || ''} readOnly style={{ background: 'var(--bg,#f4f5f6)' }} />
              : <input type="date" value={p.lwd || n?.lwd || ''} onChange={(e) => set('lwd', e.target.value)} />}</Field>
            <Field label="Reason for leaving"><select value={p.reason || ''} onChange={(e) => set('reason', e.target.value)}>{sep.data?.reasons?.map((x: string) => <option key={x}>{x}</option>)}</select></Field>
            {onNotice && !emp0 && <Field label="Request notice waiver"><select value={p.waive ? '1' : '0'} onChange={(e) => set('waive', e.target.value === '1')}><option value="0">No, serve the notice</option><option value="1">Yes, request a waiver</option></select></Field>}
            <label className="field full"><span>Note</span><input type="text" value={p.note || ''} onChange={(e) => set('note', e.target.value)} /></label>
          </div>
          {n && (onNotice
            ? <p className="mute" style={{ fontSize: 12 }}>Policy notice for this employee is {n.noticeDays} days{n.policy ? ` (${n.policy})` : ''}. Earliest last day without a waiver is {fmtD(n.lwd)}.</p>
            : n.canResign && <p className="mute" style={{ fontSize: 12 }}>Notice period is {n.noticeDays} days{n.policy ? ` (${n.policy})` : n.inProbation ? ' (in probation)' : ''}, so the last working day is {fmtD(n.lwd)}. It is set from the notice and cannot be edited. An approver can agree an earlier date when approving.</p>)}
        </>}
      </div>
      <div className="card" style={{ marginTop: 14 }}>
        {wf === 'leave' && isShort && bal.data?.short && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>Short leave: up to {bal.data.short.maxHours} hours, {bal.data.short.perMonth} a month. No balance is used.</p>}
        {wf === 'leave' && lt && !isShort && p.from && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>{lt.type} balance on {fmtD(p.from)}: <b>{lt.balance?.on ? lt.balance.bal : 'not applicable'}</b>{lt.balance?.on ? ` (credited ${lt.balance.credit}, used ${lt.balance.used}, waiting ${lt.balance.pending})` : ''}{lt.balance?.worked && <><br /><span className="mute" style={{ fontSize: 12 }}>{workedLine(lt.balance.worked)}</span></>}</p>}
        {noLeave && !solo && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>No leave in this employee's plan.</p>}
        {wf === 'leave' && preview?.days != null && lt && !isShort && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>{leaveCountLine(lt.type, preview.days, preview.offs ?? 0, preview.range ?? 0)}</p>}
        {wf === 'leave' && preview?.team != null && p.from && <div style={{ marginBottom: 12 }}><TeamCover from={p.from} to={p.to || p.from} teamOff={preview.teamOff} team={preview.team} location={preview.location} dept={preview.department} /></div>}
        {wf === 'regularize' && p.date && month.data && <><h3>System record for {fmtD(p.date)}</h3>
          <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>In <b>{rec?.in || '—'}</b> · Out <b>{rec?.out || '—'}</b> · {rec?.src || 'No data'} {rec?.status && <Pill k={rec.tone || 'n'}>{rec.status}</Pill>}</p></>}
        {preview && <><h3>Approval route</h3>
          {preview.chain.length ? <Chain steps={preview.chain} /> : <div className="mute" style={{ fontSize: 13 }}>No approver is needed. It is approved straight away.</div>}
          {preview.people.length > 0 && <div className="mute" style={{ fontSize: 12, marginTop: 6 }}>{preview.people.map((x: any) => `${x.stage}: ${x.name}`).join(' · ')}</div>}
          {preview.notify?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Will be informed: {preview.notify.map((x: any) => `${x.role} (${x.person})`).join(', ')}</p>}
          {preview.why?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Added because: {preview.why.join(', ')}</p>}
        </>}
        {problems.length > 0 && <div className="note bad" style={{ marginTop: 8 }}><ul style={{ margin: 0, paddingLeft: 18 }}>{problems.map((x) => <li key={x}>{x}</li>)}</ul></div>}
        {wf === 'leave' && preview?.scoreBlock && preview?.canOverride && <div className="note" style={{ marginTop: 8 }}>
          <b>Allow this request anyway</b>
          <div className="mute" style={{ fontSize: 12.5, margin: '2px 0 8px' }}>{preview.scoreBlock}. As {role} you can let this one request through. The reason is kept with the request and in the audit log.</div>
          <label className="field" style={{ margin: 0 }}><span>Reason for allowing it</span><input type="text" value={p.scoreOverride || ''} onChange={(e) => set('scoreOverride', e.target.value)} placeholder="For example: family emergency, agreed with the manager" /></label>
        </div>}
      </div>
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><SkipApprovalButton wf={wf} subject={who} onSkip={(q) => send(q)} /><button className="btn primary" onClick={() => send()}>Send for approval</button></div>
    </Modal>
  );
}

// ---------- Approver names ----------

const LOC_ROLES = ['Property Manager', 'HRBP', 'Cluster Manager', 'HRBP Corporate', 'Operations Head'];
const COMPANY_ROLES = ['Payroll', 'Payroll Manager', 'CHRO', 'Finance Head'];
const inOrder = (keys: string[], order: string[]) => [...order.filter((k) => keys.includes(k)), ...keys.filter((k) => !order.includes(k))];
const NameCell = ({ p }: { p?: { id: string; name: string } }) => (p?.id ? <>{p.name}</> : <span style={{ color: 'var(--bad,#b3261e)', fontWeight: 600 }}>Not mapped</span>);

function ApproverNames() {
  const { can } = useAuth();
  const nav = useNavigate();
  const { data: d, error } = useLoad(() => api('/approvals/approvers'), []);
  if (!d) return <Loading error={error} />;
  const locRoles = inOrder(Object.keys(d.locations[0]?.people || {}), LOC_ROLES);
  return <>
    <p className="ink2" style={{ margin: '0 0 14px', fontSize: 13.5 }}>These names come from Access control, under Role mapping. To change an approver, map the role there. The Line Manager comes from the employee record.</p>
    {d.gaps.length ? <div className="note bad" style={{ marginBottom: 14 }}>{d.gaps.length} not mapped: {d.gaps.slice(0, 8).join(', ')}{d.gaps.length > 8 ? '…' : ''}</div>
      : <div className="note ok" style={{ marginBottom: 14 }}>Every approver in use has a name.</div>}
    {d.overlaps?.length > 0 && <div className="note" style={{ marginBottom: 14 }}><b>Check overlaps:</b> {d.overlaps.slice(0, 4).join(' ')}</div>}
    {can('acc.users') && <div style={{ marginBottom: 14 }}><button className="btn" onClick={() => nav('/access?tab=users')}>Open Role mapping</button></div>}
    <div className="card" style={{ marginBottom: 14 }}><h3>Properties</h3><div className="tw"><table>
      <thead><tr><th>Property</th>{locRoles.map((r) => <th key={r}>{r}</th>)}</tr></thead>
      <tbody>{d.locations.map((l: any) => <tr key={l.id}><td><b>{l.name}</b><div className="mute" style={{ fontSize: 12 }}>{l.path}</div></td>{locRoles.map((r) => <td key={r}><NameCell p={l.people[r]} /></td>)}</tr>)}</tbody>
    </table></div></div>
    <div className="card" style={{ marginBottom: 14 }}><h3>Departments</h3><div className="tw"><table>
      <thead><tr><th>Department</th><th>Approval flow</th><th>Function Head</th><th>HRBP (corporate departments)</th></tr></thead>
      <tbody>{d.departments.map((x: any) => <tr key={x.id}><td><b>{x.name}</b></td><td><Pill k={x.corporate ? 'info' : 'n'}>{x.corporate ? 'Corporate' : 'Operations'}</Pill></td>
        <td><NameCell p={x.people['Function Head']} /></td><td>{x.corporate ? <NameCell p={x.people.HRBP} /> : <span className="mute">Uses the property HRBP</span>}</td></tr>)}</tbody>
    </table></div></div>
    <div className="card"><h3>Company-wide</h3><p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>The Receiving Function Head is the Function Head of the receiving department.</p>
      <div className="tw"><table><thead><tr><th>Role</th><th>Name</th></tr></thead><tbody>{inOrder(Object.keys(d.company), COMPANY_ROLES).map((r) => <tr key={r}><td><b>{r}</b></td><td><NameCell p={d.company[r]} /></td></tr>)}</tbody></table></div></div>
  </>;
}

// ---------- Check a flow ----------

const RULES: [string, string][] = [
  ['Which flow', 'The request type and the employee decide the flow: Operations or Corporate, from the employee’s department.'],
  ['Which approvers', 'The flow’s rules are checked from the top. The first rule that fits the request decides who approves, one after the other. If none fits, the Otherwise rule decides.'],
  ['Who is the person', 'Line Manager comes from the employee record. Property Manager, Cluster Manager, City Head and Operations Head come from the property. Function Head and HRBP (corporate) come from the department. Payroll, Payroll Manager, CHRO and Finance Head are company-wide.'],
  ['Who can act', 'The named person, and anyone with that role whose data access covers the employee. Nobody approves their own request. A person who appears twice is asked once.'],
  ['If someone has left', 'Their stage is skipped and the next stage approves. If it was the last stage, the role set under “if no response, escalate to” approves. HR Admin can always step in.'],
  ['Record', 'Every approval, rejection, skip and reroute is written to the request history with who and when.'],
];

/** What happens at a stage, from the person's state (prototype simFlow notes). */
function whatHappens(res: any, i: number): string {
  const x = res.people[i], nx = res.people[i + 1];
  if (x.state === 'Has left') return nx ? `Skipped. Goes to ${nx.stage} (${nx.name})` : 'Last stage, so it goes to the escalation role';
  if (x.state === 'No name set') return 'Waits for HR Admin until a name is set';
  if (x.state === 'Is the employee' || x.state === 'Is the requester') return 'Cannot approve their own request. HR Admin steps in';
  if (x.state === 'No login yet') return `This person cannot sign in. Anyone with the ${x.stage} role who can see the employee can approve`;
  return 'Approves in turn';
}

function CheckFlow({ goNames }: { goNames?: () => void }) {
  const { me, can } = useAuth();
  const myId = me?.ctx.employeeId || '';
  // Without the approvals view (the Employee role) only one's own route can be checked (the prototype's scoped() is the
  // employee themselves). The setup health and the flows at a glance are read by everyone, as in the prototype.
  const own = !can('appr.queue');
  const [wf, setWf] = useState('leave');
  const [emp, setEmp] = useState('');
  const [p, setP] = useState<{ ltype: string; days: string; amount: string }>({ ltype: '', days: '2', amount: '' });
  // The first person in the role view's data access is checked first; the picker searches all of them.
  const emps = useLoad(() => (own ? Promise.resolve([{ id: myId, name: me?.employee.name || myId, location: me?.employee.location }] as Person[])
    : api('/employees?status=Active&size=1').then((d) => d.rows as Person[])), [own, myId]);
  const [picked, setPicked] = useState<Person | null>(null);
  // The server words each check as the prototype does ("23 missing, for example …"), with the items and the count.
  const health = useLoad(() => api<any[]>('/approvals/health').catch(() => null), []);
  const who = emp || emps.data?.[0]?.id || '';
  const lts = useLoad(() => (who ? api(`/leave/balances/${who === myId ? 'me' : who}`).then((b) => (b.types as any[]).map((t) => t.type as string)).catch(() => LEAVE_TYPES) : Promise.resolve(LEAVE_TYPES)), [who]);
  const ltList = lts.data?.length ? lts.data : LEAVE_TYPES;
  const ltype = ltList.includes(p.ltype) ? p.ltype : ltList[0];
  const [res, setRes] = useState<any>(null);
  const [err, setErr] = useState('');
  const needAmt = /^p_(claim|adv|arrear|ded)$/.test(wf), needDays = wf === 'leave';
  const run = async () => {
    if (!who) { setRes(null); setErr('No employee to check'); return; }
    const days = Math.max(1, +p.days || 1), from = today();
    const payload = { ltype, from, to: addDays(from, days - 1), amount: +p.amount || 1, rtype: 'All Other', date: today(), kind: 'Bank', claimType: CLAIM_TYPES[0], accountNo: '50100234567891', ifsc: 'HDFC0001234', bankName: 'HDFC' };
    try { setRes(await api('/approvals/simulate', { body: { wf, employeeId: who, payload } })); setErr(''); } catch (x: any) { setErr(x.message); setRes(null); }
  };
  const first = useRef(true);
  useEffect(() => { if (first.current && who && lts.data) { first.current = false; run(); } }, [who, lts.data]); // eslint-disable-line react-hooks/exhaustive-deps
  const bad = health.data?.filter((h) => !h.ok).length ?? 0;
  return <>
    {health.data && <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp"><h3 style={{ margin: 0 }}>Setup health</h3>{bad ? <Pill k="warn">{bad} to fix</Pill> : <Pill k="ok">All clear</Pill>}</div>
      <div style={{ marginTop: 8 }}>{health.data.map((h) => <div key={h.title} className="row sp" style={{ padding: '8px 0', borderTop: '1px solid var(--line)', gap: 12 }}>
        <div><span>{h.ok ? '✓' : '!'}</span> <b style={{ marginLeft: 6 }}>{h.title}</b>{h.detail && <div className="mute" style={{ fontSize: 12.5, margin: '2px 0 0 22px' }}>{h.detail}</div>}</div>
        {goNames && h.fix === 'names' && <button className="btn sm" onClick={goNames}>Open names</button>}
      </div>)}</div>
    </div>}
    <div className="card" style={{ marginBottom: 14 }}>
      <h3>Check a flow</h3>
      <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>Pick a request type and an employee. You will see who approves, in order, and what happens if someone has left. Nothing is raised.</p>
      <div className="form">
        <Field label="Request type"><select value={wf} onChange={(e) => setWf(e.target.value)}>{FLOW_WF.filter(([k]) => !['shortlist', 'joining', 'verify'].includes(k)).map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
        <Field label="Employee">{own
          ? <PersonPicker label="Employee" people={emps.data} value={who} onChange={() => {}} required />
          : <PersonPicker label="Employee" search="&status=Active" value={who} selected={picked?.id === who ? picked : emps.data?.find((e) => e.id === who) ?? null} required
            onChange={(id, x) => { if (id) { setPicked(x); setEmp(id); } }} />}</Field>
        {needDays && <><Field label="Leave type"><select value={ltype} onChange={(e) => setP({ ...p, ltype: e.target.value })}>{ltList.map((t) => <option key={t}>{t}</option>)}</select></Field>
          <Field label="Days"><input type="number" min={1} value={p.days} onChange={(e) => setP({ ...p, days: e.target.value })} /></Field></>}
        {needAmt && <Field label="Amount"><input type="number" min={0} value={p.amount} onChange={(e) => setP({ ...p, amount: e.target.value })} /></Field>}
      </div>
      <div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={run}>Show approvers</button></div>
      {err && <div className="note bad" style={{ marginTop: 12 }}>{err}</div>}
      {res && <div style={{ marginTop: 16 }}>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap', marginBottom: 8 }}><Pill k="info">{res.flow} flow</Pill><span className="mute" style={{ fontSize: 12.5 }}>{!res.rule || res.rule === 'Otherwise' ? 'No rule fits, so the Otherwise rule decides' : `Rule used: ${res.rule}`}
          {res.why.filter((w: string) => w !== res.rule).length > 0 && ` · Also added because: ${res.why.filter((w: string) => w !== res.rule).join(', ')}`}</span></div>
        {res.people.length ? <div className="tw"><table><thead><tr><th>Order</th><th>Role</th><th>Person</th><th>Status</th><th>What happens</th></tr></thead><tbody>
          {res.people.map((x: any, i: number) => <tr key={i} style={{ verticalAlign: 'top' }}><td>{i + 1}</td><td><b>{x.stage}</b></td><td>{x.name}</td>
            <td><Pill k={x.state === 'Ready' ? 'ok' : x.state === 'No name set' ? 'bad' : 'warn'}>{x.state}</Pill></td><td className="ink2" style={{ fontSize: 12.5 }}>{whatHappens(res, i)}</td></tr>)}
        </tbody></table></div> : <div className="note">No approver is needed for this request. It is approved automatically.</div>}
        {res.notify.length > 0 && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Informed only: {res.notify.map((n: any) => `${n.role} (${n.person})`).join(', ')}</p>}
      </div>}
    </div>
    <div className="card" style={{ marginBottom: 14 }}><h3>The rules, in six lines</h3>
      {RULES.map(([h, t], i) => <div key={h} style={{ display: 'grid', gridTemplateColumns: '24px 1fr', gap: 8, padding: '8px 0', borderTop: i ? '1px solid var(--line)' : 0 }}><div style={{ fontWeight: 600 }}>{i + 1}</div><div><b>{h}.</b> <span className="ink2" style={{ fontSize: 13 }}>{t}</span></div></div>)}
    </div>
    <FlowGlance />
  </>;
}

/** Ticks or clears a flow's "Approval required" in All flows at a glance (21 Nov 2026). Cleared: a new request of this type and flow
 *  goes to nobody and is approved at once; requests already waiting keep their approvers (HR can skip them). Asks
 *  first before clearing. Returns how many requests wait, or null when not changed. */
async function putHierarchy(key: string, flow: string, on: boolean): Promise<number | null> {
  if (!on && !confirm(`Untick "Approval hierarchy required" for ${wfName(key)}, ${flow} flow?\n\nNo approval: every new request of this type in this flow is approved at once, nobody is asked. The rules and approvers are kept for when you tick it again. Requests already waiting stay with their approvers (HR can skip them).`)) return null;
  try {
    const r = await api<{ waiting: number }>(`/approvals/flows/${key}/${flow}/hierarchy`, { method: 'PUT', body: { on } });
    toast(on ? 'Approval hierarchy on. The rules apply again' : 'No approval needed. New requests are approved automatically');
    return r.waiting;
  } catch (x: any) { toast(x.message || 'Not saved'); return null; }
}

/** Every flow on one page, read as sentences: each rule in use, in order, with its approvers. Who sets up approval
 *  flows ticks or clears "Approval required" on each flow here. */
function FlowGlance() {
  const { can } = useAuth();
  const edit = can('appr.setup', 2);
  const { data, error, reload } = useLoad(() => api<any[]>('/approvals/flows'), []);
  if (!data) return <Loading error={error} />;
  const tick = async (m: any, on: boolean) => { if ((await putHierarchy(m.wf, m.flow, on)) !== null) reload(); };
  const groups: Record<string, any[]> = {};
  data.forEach((m) => { const b = m.wf.startsWith('leave') ? 'leave' : m.wf; (groups[b] ||= []).push(m); });
  const keys = inOrder(Object.keys(groups), GLANCE);
  const flowRank = (f: string) => (f === 'Operations' ? 0 : 1);
  return <div className="card"><h3>All flows at a glance</h3>
    <p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>Each flow is a list of rules, checked from the top. The first rule that fits a request decides who approves. Open a request type to see it. Change it in the Approval flow tab.{edit && <> Untick <b>Approval required</b> on a flow and its requests are approved automatically, nobody is asked.</>}</p>
    {keys.map((b) => <details key={b} style={{ borderTop: '1px solid var(--line)', padding: '8px 0' }}>
      <summary style={{ cursor: 'pointer', fontWeight: 600 }}>{wfName(b)}</summary>
      <div className="tw" style={{ marginTop: 8 }}><table><thead><tr><th>Flow</th><th>Rule</th><th>Approvers, in order</th><th>Also told</th></tr></thead><tbody>
        {[...groups[b]].sort((x, y) => x.wf.localeCompare(y.wf) || flowRank(x.flow) - flowRank(y.flow)).flatMap((m) => [
          <tr key={m.wf + m.flow + 'req'} style={{ verticalAlign: 'top', background: 'var(--bg2, transparent)' }}><td className="ink2" style={{ fontSize: 12.5 }}>{b === 'leave' ? wfName(m.wf) + ' · ' : ''}{m.flow}</td>
            <td colSpan={3}>{edit
              ? <label className="chkl" style={{ fontWeight: 600 }}><input type="checkbox" checked={!m.hierarchy_off} onChange={(e) => tick(m, e.target.checked)} />Approval required</label>
              : <b>{m.hierarchy_off ? 'No approval' : 'Approval required'}</b>}
              {m.hierarchy_off && <div className="mute" style={{ fontSize: 12 }}>Every new request is approved automatically, nobody is asked.{m.waiting > 0 && ` ${m.waiting} raised before still ${m.waiting === 1 ? 'waits' : 'wait'} (HR can skip).`}</div>}</td></tr>,
          ...(m.hierarchy_off ? [] : m.rules as Rule[]).map((x, i) => {
          const last = i === m.rules.length - 1;
          if (!x.on && !last) return null;
          return <tr key={m.wf + m.flow + x.id} style={{ verticalAlign: 'top' }}><td className="ink2" style={{ fontSize: 12.5 }}>{b === 'leave' ? wfName(m.wf) + ' · ' : ''}{m.flow}</td>
            <td>{last ? <><b>Otherwise</b><div className="mute" style={{ fontSize: 12 }}>When no rule above fits</div></> : <>{i + 1}. {x.label}{whenText(x.when) !== x.label && <div className="mute" style={{ fontSize: 12 }}>If {whenText(x.when, ' and ')}</div>}</>}</td>
            <td>{x.approvers.length ? x.approvers.join(' → ') : <span className="mute">Approved automatically</span>}</td><td className="mute" style={{ fontSize: 12.5 }}>{x.notify.join(', ')}</td></tr>;
        })])}
      </tbody></table></div>
    </details>)}
  </div>;
}

// ---------- Approval flow editor ----------

type Cond = { k: string; min?: number; max?: number; list?: string[]; rt?: string; val?: string };
type Rule = { id: string; label: string; on: boolean; when: Cond[]; approvers: string[]; notify: string[] };
type FlowDraft = { rules: Rule[]; noResponse: string };

let ruleSeq = 0;
/** The order conditions are named in (as the server names a rule). */
const KIND_ORDER = ['skind', 'ckind', 'ctype', 'tkind', 'grades', 'cnt', 'days', 'amount', 'pct', 'budget', 'bandx', 'waive'];
const blankCond = (k: string, grades: string[]): Cond => ({
  pct: { k, min: 0 }, amount: { k, min: 0 }, days: { k, min: 1 }, grades: { k, list: grades.slice(0, 1) }, cnt: { k, rt: 'Any', min: 1 },
  ctype: { k, val: CLAIM_TYPES[0] }, ckind: { k, val: 'Bank' }, tkind: { k, val: 'Cross city' }, skind: { k, val: EXIT_TYPES[0] },
}[k] ?? { k });
const numS = (v: number) => String(+v);
/** Grades as runs, as the server names rules: "G1–G3, G5". */
function gradeList(l: string[]): string {
  const split = (g: string) => { const m = /^(.*?)(\d+)$/.exec(g); return m ? { p: m[1], n: +m[2] } : null; };
  const g = [...l].sort((a, b) => { const x = split(a), y = split(b); return x && y && x.p === y.p ? x.n - y.n : a < b ? -1 : a > b ? 1 : 0; });
  const next = (a: string, b: string) => { const x = split(a), y = split(b); return !!x && !!y && x.p === y.p && y.n === x.n + 1; };
  const parts: string[] = [];
  for (let i = 0; i < g.length;) {
    let j = i;
    while (j + 1 < g.length && next(g[j], g[j + 1])) j++;
    parts.push(j > i ? `${g[i]}–${g[j]}` : g[i]);
    i = j + 1;
  }
  return parts.join(', ');
}
const rupees = (v: number) => '₹' + (+v || 0).toLocaleString('en-IN');
/** A range in words: "2 to 5 days", "up to 1 day", "6 or more days". */
function span(c: Cond, n: (v: number) => string, one: string, many: string, any: string): string {
  const lo = c.min ?? 0, hi = c.max;
  const unit = (v: number) => (many ? ' ' + (v === 1 ? one : many) : '');
  if (hi == null && lo <= 0) return any;
  if (hi == null) return `${n(lo)} or more${unit(2)}`;
  if (lo <= 0) return `up to ${n(hi)}${unit(hi)}`;
  if (lo === hi) return `${n(lo)}${unit(lo)}`;
  return `${n(lo)} to ${n(hi)}${unit(2)}`;
}
/** One condition in plain words (the server's CondLabel). */
function condText(c: Cond): string {
  switch (c.k) {
    case 'skind': return c.val || '';
    case 'ckind': return c.val === 'Bank' ? 'bank account change' : c.val === 'Personal' ? 'personal details change' : `change of ${c.val}`;
    case 'ctype': return `claim type ${c.val}`;
    case 'tkind': return `${(c.val || '').toLowerCase()} transfer`;
    case 'grades': return c.list?.length ? 'grade ' + gradeList(c.list) : 'no grade picked yet';
    case 'cnt': { const noun = c.rt === 'No Punch' ? 'no-punch ' : c.rt === 'All Other' ? 'all-other ' : ''; return span(c, numS, `${noun}request this month`, `${noun}requests this month`, `any number of ${noun}requests this month`); }
    case 'days': return span(c, numS, 'day', 'days', 'any number of days') + ' in 30 days';
    case 'amount': return span(c, rupees, '', '', 'any amount');
    case 'pct': return 'pay change ' + span(c, (v) => `${+v}%`, '', '', 'of any size');
    case 'budget': return 'above budget';
    case 'bandx': return 'outside the grade pay band';
    case 'waive': return 'notice waiver requested';
  }
  return COND[c.k] || c.k;
}
/** A rule's conditions in words, in kind order: "Grade G3, 6 to 10 days in 30 days". */
function whenText(when: Cond[], sep = ', '): string {
  const t = [...when].sort((a, b) => KIND_ORDER.indexOf(a.k) - KIND_ORDER.indexOf(b.k)).map(condText).join(sep);
  return t ? t[0].toUpperCase() + t.slice(1) : 'Otherwise';
}

/** What is wrong with a condition, in the server's words, or '' (checked before saving, shown next to it). */
function condProblem(c: Cond): string {
  if (['days', 'amount', 'pct', 'cnt'].includes(c.k)) {
    if ((c.min != null && c.min < 0) || (c.max != null && c.max < 0)) return 'The numbers cannot be below 0';
    if (c.max != null && c.max < (c.min ?? 0)) return 'The upper number cannot be below the lower one';
  }
  if (c.k === 'grades' && !c.list?.length) return 'Pick at least one grade';
  return '';
}
/** The first rule that cannot be saved, with what is wrong, as the server would say it. */
function listProblem(rules: Rule[]): { id: string; msg: string } | null {
  for (let i = 0; i < rules.length - 1; i++) {
    for (const c of rules[i].when) {
      const p = condProblem(c);
      if (p) return { id: rules[i].id, msg: `Rule ${i + 1} (“${rules[i].label}”): ${p.replace(/^./, (x) => x.toLowerCase())}` };
    }
  }
  return null;
}

function CondEdit({ c, grades, set, commit }: { c: Cond; grades: string[]; set: (c: Cond) => void; commit: (c?: Cond) => void }) {
  const num = (f: 'min' | 'max', w = 64, ph = '') => <input type="number" min={0} value={c[f] ?? ''} placeholder={ph} style={{ width: w }}
    onChange={(e) => set({ ...c, [f]: e.target.value === '' ? undefined : +e.target.value })} onBlur={() => commit()} />;
  const problem = condProblem(c);
  const wrap = (x: ReactNode) => <div className="row" style={{ gap: 6, flexWrap: 'wrap', fontSize: 12.5 }}>{x}
    {problem && <span role="alert" style={{ color: 'var(--bad)', fontWeight: 600, flexBasis: '100%' }}>{problem}</span>}</div>;
  const pick = (opts: string[]) => wrap(<select value={c.val || ''} onChange={(e) => commit({ ...c, val: e.target.value })}>{opts.map((o) => <option key={o}>{o}</option>)}</select>);
  switch (c.k) {
    case 'days': return wrap(<><span className="mute">From</span>{num('min')}<span className="mute">to</span>{num('max', 84, 'no limit')}<span className="mute">days</span></>);
    case 'cnt': return wrap(<><select value={c.rt || 'Any'} onChange={(e) => commit({ ...c, rt: e.target.value })}>{['No Punch', 'All Other', 'Any'].map((o) => <option key={o}>{o}</option>)}</select><span className="mute">number</span>{num('min')}<span className="mute">to</span>{num('max', 84, 'no limit')}</>);
    case 'grades': return wrap(grades.map((g) => <label key={g} className="chkl"><input type="checkbox" checked={(c.list || []).includes(g)}
      onChange={(e) => commit({ ...c, list: e.target.checked ? [...(c.list || []), g] : (c.list || []).filter((x: string) => x !== g) })} />{g}</label>));
    case 'amount': return wrap(<><span className="mute">From ₹</span>{num('min', 90)}<span className="mute">to ₹</span>{num('max', 100, 'no limit')}</>);
    case 'pct': return wrap(<><span className="mute">From</span>{num('min', 70)}<span className="mute">% to</span>{num('max', 80, 'no limit')}<span className="mute">%</span></>);
    case 'ctype': return pick(CLAIM_TYPES);
    case 'skind': return pick(EXIT_TYPES);
    case 'ckind': return pick(['Bank', 'Personal']);
    case 'tkind': return pick(TKINDS);
    default: return null;
  }
}

/** Ordered role pills with move and remove buttons, and a list to add one more. */
function RolePicker({ roles, free, ordered, empty, onChange }: { roles: string[]; free: string[]; ordered?: boolean; empty: string; onChange: (r: string[]) => void }) {
  const [add, setAdd] = useState('');
  const pickable = free.filter((r) => !roles.includes(r));
  const sel = pickable.includes(add) ? add : pickable[0];
  const move = (i: number, d: number) => { const l = [...roles]; [l[i], l[i + d]] = [l[i + d], l[i]]; onChange(l); };
  return <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
    {roles.length ? <div className="chain">{roles.map((r, i) => <span key={r} className="chain-i">{ordered && i > 0 && <span className="arrow">→</span>}
      <span className="step">{ordered && <span className="mute">{i + 1}</span>}{r}
        {ordered && <button type="button" className="stepx" title="Earlier" aria-label={`Move ${r} earlier`} disabled={!i} onClick={() => move(i, -1)}>◀</button>}
        {ordered && <button type="button" className="stepx" title="Later" aria-label={`Move ${r} later`} disabled={i === roles.length - 1} onClick={() => move(i, 1)}>▶</button>}
        <button type="button" className="stepx" title="Remove" aria-label={`Remove ${r}`} onClick={() => onChange(roles.filter((x) => x !== r))}>✕</button></span></span>)}</div>
      : <span className="mute" style={{ fontSize: 12.5 }}>{empty}</span>}
    {sel && <span className="row" style={{ gap: 4 }}><select value={sel} onChange={(e) => setAdd(e.target.value)} aria-label="Role to add">{pickable.map((r) => <option key={r}>{r}</option>)}</select>
      <button type="button" className="btn sm" onClick={() => onChange([...roles, sel])}>Add</button></span>}
  </div>;
}

/** Sample facts for "Test a request", by the condition kinds the request type uses. */
type Sample = { grade: string; roll: string; cnt: string; rtype: string; amt: string; pct: string; aboveBudget: boolean; waive: boolean; bandx: boolean; tkind: string; ctype: string; ckind: string; skind: string };
const SAMPLE: Sample = { grade: '', roll: '2', cnt: '1', rtype: 'No Punch', amt: '1000', pct: '5', aboveBudget: false, waive: false, bandx: false, tkind: TKINDS[0], ctype: CLAIM_TYPES[0], ckind: 'Bank', skind: EXIT_TYPES[0] };

/** `saves` counts the saves of the flow: a result on screen is worked out again after each one, so it never shows
 *  a route the rules no longer give. */
function TestRequest({ wf, path, grades, saves }: { wf: string; path: string; grades: string[]; saves: number }) {
  const [x, setX] = useState<Sample>(SAMPLE);
  const [res, setRes] = useState<any>(null);
  const [err, setErr] = useState('');
  useEffect(() => { setRes(null); setErr(''); }, [path]);
  const kinds = COND_OK[wf] || [];
  const has = (k: string) => kinds.includes(k);
  const run = async () => {
    const f: Record<string, unknown> = {};
    if (has('grades') && x.grade) f.grade = x.grade;
    if (has('days')) f.roll = +x.roll || 0;
    if (has('cnt')) { f.cnt = +x.cnt || 0; f.rtype = x.rtype; }
    if (has('amount')) f.amt = +x.amt || 0;
    if (has('pct')) f.pct = +x.pct || 0;
    for (const k of ['budget', 'waive', 'bandx'] as const) if (has(k)) f[k === 'budget' ? 'aboveBudget' : k] = x[k === 'budget' ? 'aboveBudget' : k];
    for (const k of ['tkind', 'ctype', 'ckind', 'skind'] as const) if (has(k)) f[k] = x[k];
    try { setRes(await api(`/approvals/flows/${path}/test`, { body: { facts: f } })); setErr(''); } catch (e: any) { setErr(e.message); setRes(null); }
  };
  const shown = useRef(false);
  shown.current = !!res;
  useEffect(() => { if (saves && shown.current) run(); }, [saves]); // eslint-disable-line react-hooks/exhaustive-deps
  const set = (k: keyof Sample) => (e: { target: { value: string; checked?: boolean; type?: string } }) => setX({ ...x, [k]: e.target.type === 'checkbox' ? !!e.target.checked : e.target.value });
  const sel = (k: keyof Sample, opts: string[]) => <select value={String(x[k])} onChange={set(k)}>{opts.map((o) => <option key={o}>{o}</option>)}</select>;
  return <div style={{ marginTop: 16, paddingTop: 12, borderTop: '1px solid var(--line)' }}>
    <h3 style={{ margin: '0 0 4px' }}>Test a request</h3>
    <p className="mute" style={{ margin: '0 0 10px', fontSize: 12.5 }}>Enter what a request would have. You will see which rule decides and who approves, in order. Nothing is raised.</p>
    <div className="form">
      {has('grades') && <Field label="Grade"><select value={x.grade} onChange={set('grade')}><option value="">Any other grade</option>{grades.map((g) => <option key={g}>{g}</option>)}</select></Field>}
      {has('days') && <Field label="Leave days in the last 30 days, with this request"><input type="number" min={0} step={0.5} value={x.roll} onChange={set('roll')} /></Field>}
      {has('cnt') && <><Field label="Type">{sel('rtype', ['No Punch', 'All Other'])}</Field><Field label="Number this month, with this request"><input type="number" min={1} value={x.cnt} onChange={set('cnt')} /></Field></>}
      {has('amount') && <Field label="Amount (₹)"><input type="number" min={0} value={x.amt} onChange={set('amt')} /></Field>}
      {has('pct') && <Field label="Pay change (%)"><input type="number" value={x.pct} onChange={set('pct')} /></Field>}
      {has('tkind') && <Field label="Transfer">{sel('tkind', TKINDS)}</Field>}
      {has('ctype') && <Field label="Claim type">{sel('ctype', CLAIM_TYPES)}</Field>}
      {has('ckind') && <Field label="Change">{sel('ckind', ['Bank', 'Personal'])}</Field>}
      {has('skind') && <Field label="Exit type">{sel('skind', EXIT_TYPES)}</Field>}
    </div>
    {(has('budget') || has('waive') || has('bandx')) && <div className="row" style={{ gap: 16, marginTop: 8, flexWrap: 'wrap' }}>
      {has('budget') && <label className="chkl"><input type="checkbox" checked={x.aboveBudget} onChange={set('aboveBudget')} />Above budget</label>}
      {has('bandx') && <label className="chkl"><input type="checkbox" checked={x.bandx} onChange={set('bandx')} />Outside the grade pay band</label>}
      {has('waive') && <label className="chkl"><input type="checkbox" checked={x.waive} onChange={set('waive')} />Notice waiver requested</label>}
    </div>}
    <div className="row" style={{ marginTop: 10 }}><button className="btn" onClick={run}>Test</button></div>
    {err && <div className="note bad" style={{ marginTop: 10 }}>{err}</div>}
    {res && <div id="flow-test-result" style={{ marginTop: 10 }}>
      <p style={{ margin: '0 0 6px', fontSize: 13 }}>{res.otherwise ? <>No rule fits, so <b>Otherwise</b> decides.</> : <>Rule {res.rule} decides: <b>{res.label}</b></>}</p>
      {res.chain.length ? <Chain steps={res.chain} /> : <div className="note">Nobody needs to approve. The request is approved automatically.</div>}
      {res.notify.length > 0 && <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 0' }}>Also told: {res.notify.map((n: any) => n.role).join(', ')}</p>}
    </div>}
  </div>;
}

/** The approval flow: per request type and flow, a list of rules checked from the top. As in the prototype, every
 *  change is saved as it is made. A change that cannot be saved stays on screen, marked as not saved, with what is
 *  wrong next to the rule, until it is fixed or put back. */
function FlowEditor({ goNames }: { goNames: () => void }) {
  const { data } = useLoad(() => api<any[]>('/approvals/flows'), []);
  const cols = useLoad(() => api<string[]>('/approvals/columns'), []);
  const grades = useLoad(() => api<any[]>('/masters/grade?status=Active').then((l) => l.map((g) => g.name as string)), []);
  const names = useLoad(() => api('/approvals/approvers'), []);
  const settings = useLoad(() => api<{ slaHours: number }>('/approvals/settings'), []);
  const [wf, setWf] = useState('joining');
  const [flow, setFlow] = useState<'Operations' | 'Corporate'>('Operations');
  const [grp, setGrp] = useState<'C' | 'P'>('C');
  const [draft, setDraft] = useState<FlowDraft | null>(null);
  const [open, setOpen] = useState('');
  const [sla, setSla] = useState<number | ''>('');
  const [slaErr, setSlaErr] = useState('');
  const [kind, setKind] = useState('days');
  /** What stops the list on screen from being saved: the rule it is about (if any) and the message. */
  const [problem, setProblem] = useState<{ id?: string; msg: string } | null>(null);
  const [saves, setSaves] = useState(0);
  const saved = useRef<Record<string, FlowDraft>>({});
  /** Rules added here that are switched off only because they have no approver yet. */
  const fresh = useRef<Set<string>>(new Set());
  const key = wf === 'leave' ? 'leave' + grp : wf;
  const k = `${key}/${flow}`;
  const draftRef = useRef<FlowDraft | null>(null);
  draftRef.current = draft;
  /** The list as last saved (or loaded). */
  const lastSaved = (): FlowDraft | null => {
    const m = data?.find((x) => x.wf === key && x.flow === flow);
    return saved.current[k] ?? (m ? { rules: m.rules, noResponse: m.no_response } : null);
  };
  useEffect(() => {
    const s = lastSaved();
    setDraft(s ? structuredClone(s) : null);
    setOpen('');
    setProblem(null);
  }, [k, data]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { if (settings.data) setSla(settings.data.slaHours); }, [settings.data]);
  const kinds = (COND_OK[wf] || []).filter((c) => c !== 'always');
  useEffect(() => { setKind(kinds[0] || ''); }, [wf]); // eslint-disable-line react-hooks/exhaustive-deps
  const wfs = EDIT_WF.map(([w]) => w).filter((w) => data?.some((x) => x.wf === w || (w === 'leave' && x.wf.startsWith('leave'))));
  const gl = grades.data || [];
  /** Moving to another flow drops a change that is not saved: ask first. */
  const leave = (go: () => void) => { if (!problem || confirm('Your last change to this flow is not saved. Leave it and lose it?')) go(); };
  const putBack = () => { const s = lastSaved(); setDraft(s ? structuredClone(s) : null); setProblem(null); };
  const save = async (d: FlowDraft, msg = 'Saved') => {
    setDraft(d);
    const local = listProblem(d.rules);
    if (local) { setProblem(local); return; } // nothing is sent while a rule cannot be saved
    try {
      const r = await api<{ rules: Rule[] }>(`/approvals/flows/${key}/${flow}`, { method: 'PUT', body: d });
      const next = { ...d, rules: r.rules };
      saved.current[k] = structuredClone(next);
      if (draftRef.current === d) setDraft(next); // the names and ids the server gave, unless typing has moved on
      setProblem(null);
      setSaves((n) => n + 1);
      toast(msg);
    } catch (x: any) {
      const n = /^Rule (\d+)/.exec(x.message || '');
      setProblem({ id: n ? d.rules[+n[1] - 1]?.id : undefined, msg: x.message || 'Not saved' });
    }
  };
  /** Change the draft and save it; `local` only changes the draft (typing), saved on blur. */
  const dirty = useRef(false);
  const mod = (fn: (d: FlowDraft) => void, local = false, msg?: string) => {
    const d = structuredClone(draftRef.current!); fn(d);
    if (local) { dirty.current = true; setDraft(d); } else { dirty.current = false; save(d, msg); }
  };
  /** Save typed changes when the field loses focus. */
  const commitTyped = (fn: (d: FlowDraft) => void = () => undefined) => { if (dirty.current) mod(fn); };
  /** New conditions rename a rule whose name describes its conditions (as the server names a rule); a name HR
   *  typed is kept, and the editor offers the new description. */
  const setWhen = (r: Rule, when: Cond[]) => { if (!r.label.trim() || r.label === whenText(r.when)) r.label = whenText(when); r.when = when; };
  const saveSla = async () => {
    if (sla === '' || sla === settings.data?.slaHours) { setSlaErr(''); if (sla === '') setSla(settings.data?.slaHours ?? ''); return; }
    if (!Number.isInteger(+sla) || +sla < 1 || +sla > 720) { setSlaErr('Enter whole hours from 1 to 720. Not saved.'); return; }
    try { await api('/approvals/settings', { method: 'PUT', body: { slaHours: +sla } }); setSlaErr(''); toast('Saved for every request type'); settings.reload(); }
    catch (x: any) { setSlaErr(`${x.message}. Not saved.`); }
  };
  // The receiving side only takes part in a transfer.
  const roles = inOrder((cols.data || []).filter((c) => c !== 'Functional Manager' && (wf === 'transfer' || !c.startsWith('Receiving '))), STAGE_ORDER);
  const gaps: string[] = names.data?.gaps ?? [];
  const rules = draft?.rules ?? [];
  const n = rules.length;
  const steps = (r: Rule) => (r.approvers.length ? r.approvers.join(' → ') : 'approved automatically');
  const otherwiseRoute = () => { const o = rules[n - 1]; return o?.approvers.length ? `Otherwise, which goes to ${o.approvers.join(' → ')}` : 'Otherwise, which approves them automatically'; };
  return (
    <>
      <Tabs tabs={wfs.map((w) => [w, wfName(w)] as [string, string])} value={wf} onChange={(w) => leave(() => setWf(w))} />
      <p className="ink2" style={{ margin: '0 0 14px', fontSize: 13.5 }}>{WFNOTE[wf]}</p>
      {gaps.length > 0 && <div className="note bad" style={{ marginBottom: 14 }}><b>Approver names missing:</b> {gaps.slice(0, 8).join(', ')}{gaps.length > 8 ? ` and ${gaps.length - 8} more` : ''}. <button className="btn sm" onClick={goNames}>Add names</button></div>}
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap' }}>
          <div className="seg">{(['Operations', 'Corporate'] as const).map((f) => <button key={f} className={flow === f ? 'on' : ''} onClick={() => leave(() => setFlow(f))}>{f} flow</button>)}</div>
          {wf === 'leave' && <div className="seg">{([['C', 'Casual and sick leave'], ['P', 'Privilege and other leave']] as const).map(([g, l]) => <button key={g} className={grp === g ? 'on' : ''} onClick={() => leave(() => setGrp(g))}>{l}</button>)}</div>}
        </div>
        {wf === 'interview' ? null : <>
        {wf === 'leave' && <p className="mute" style={{ margin: '0 0 8px', fontSize: 12.5 }}>Which leave type uses which list is set in the work policy, under leave rules.</p>}
        <p className="mute" style={{ margin: '0 0 10px', fontSize: 12.5 }}>Rules are checked from the top. The <b>first rule that fits</b> the request decides who approves, one after the other, and who is also told. If no rule fits, <b>Otherwise</b> decides. A rule that is switched off is skipped. Operations or corporate is set on the department.</p>
        {problem && <div className="note bad" role="alert" style={{ marginBottom: 10 }}><b>Not saved.</b> {problem.msg}. Your changes to this flow are kept on screen but are not saved until this is fixed.{' '}
          <button className="btn sm" onClick={putBack}>Put back the saved rules</button></div>}
        {!data ? <Loading /> : !draft ? <p className="mute">This flow has no rules yet.</p> : <>
          <div id="flow-rules">{rules.map((r, i) => {
            const last = i === n - 1, isOpen = open === r.id, bad = problem?.id === r.id;
            const named = whenText(r.when);
            return <div key={r.id} className={`flow-rule${bad ? ' bad' : ''}`} style={{ borderTop: '1px solid var(--line)', padding: '10px 0', opacity: r.on || last ? 1 : 0.5 }}>
              <div className="row sp flow-rule-head" style={{ gap: 10, alignItems: 'flex-start' }}>
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ fontWeight: 700 }}>{last ? 'Otherwise' : `${i + 1}. ${r.label}`}{!r.on && !last && <span className="mute" style={{ fontWeight: 400 }}> · switched off{!r.approvers.length ? ' until it has an approver' : ''}</span>}
                    {bad && <span style={{ color: 'var(--bad)', fontWeight: 600 }}> · not saved</span>}</div>
                  <div className="ink2" style={{ fontSize: 13, marginTop: 2 }}>
                    <b>{last ? 'If no rule above fits' : `If ${named.replace(/^./, (x) => x.toLowerCase())}`}</b> → {steps(r)}
                    {r.notify.length > 0 && <span className="mute"> · also tell {r.notify.join(', ')}</span>}
                  </div>
                  {bad && !(isOpen && listProblem([r, rules[n - 1]])) && <div role="alert" style={{ color: 'var(--bad)', fontSize: 12.5, marginTop: 4 }}>{problem!.msg.replace(/^Rule \d+ \(“[^”]*”\)[:]? ?/, '').replace(/^./, (x) => x.toUpperCase())}</div>}
                </div>
                <div className="row flow-rule-btns" style={{ gap: 4, flex: 'none' }}>
                  {!last && <>
                    <button className="btn sm" title="Move up" aria-label={`Move “${r.label}” up`} disabled={!i} onClick={() => mod((d) => { [d.rules[i - 1], d.rules[i]] = [d.rules[i], d.rules[i - 1]]; }, false, 'Moved up')}>↑</button>
                    <button className="btn sm" title="Move down" aria-label={`Move “${r.label}” down`} disabled={i >= n - 2} onClick={() => mod((d) => { [d.rules[i + 1], d.rules[i]] = [d.rules[i], d.rules[i + 1]]; }, false, 'Moved down')}>↓</button>
                    <Switch on={r.on} label={`Use “${r.label}”`} onChange={() => {
                      if (!r.on && !r.approvers.length && !confirm(`“${r.label}” has no approvers, so every request that fits it is approved automatically. Switch it on anyway?`)) return;
                      fresh.current.delete(r.id);
                      mod((d) => { d.rules[i].on = !d.rules[i].on; }, false, r.on ? 'Switched off' : 'Switched on');
                    }} />
                  </>}
                  <button className="btn sm" onClick={() => setOpen(isOpen ? '' : r.id)}>{isOpen ? 'Done' : 'Edit'}</button>
                  {!last && <button className="btn sm" title="Delete rule" aria-label={`Delete “${r.label}”`} onClick={() => {
                    if (confirm(`Delete the rule “${r.label}”?\n\nRequests that fit it will go to the next rule below that fits them, or else to ${otherwiseRoute()}.`)) mod((d) => { d.rules.splice(i, 1); }, false, 'Rule deleted');
                  }}>✕</button>}
                </div>
              </div>
              {isOpen && <div style={{ marginTop: 10, padding: 12, background: 'var(--soft)', borderRadius: 8, display: 'grid', gap: 12 }}>
                {!last && <>
                  <div><Field label="Name"><input type="text" value={r.label} onChange={(e) => mod((d) => { d.rules[i].label = e.target.value; }, true)}
                    onBlur={() => { const v = String(draftRef.current?.rules[i]?.label || '').trim(); commitTyped((d) => { d.rules[i].label = v || whenText(d.rules[i].when); }); }} /></Field>
                    {r.label.trim() && r.label !== named && !listProblem([r, rules[n - 1]]) && <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>
                      The name does not describe the conditions below.{' '}
                      <button className="btn sm" onClick={() => mod((d) => { d.rules[i].label = named; }, false, 'Renamed')}>Name it “{named}”</button></div>}</div>
                  <div><div style={{ fontWeight: 600, fontSize: 13, marginBottom: 6 }}>When all of these are true</div>
                    {r.when.map((c, ci) => <div key={ci} className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center', marginBottom: 6 }}>
                      <select value={c.k} aria-label="What the condition checks" onChange={(e) => { const v = e.target.value; mod((d) => { const w = [...d.rules[i].when]; w[ci] = blankCond(v, gl); setWhen(d.rules[i], w); }); }}>
                        {[c.k, ...kinds.filter((x) => !r.when.some((y) => y.k === x))].map((x) => <option key={x} value={x}>{COND[x] || x}</option>)}</select>
                      <CondEdit c={c} grades={gl} set={(nc) => mod((d) => { const w = [...d.rules[i].when]; w[ci] = nc; setWhen(d.rules[i], w); }, true)}
                        commit={(nc) => (nc ? mod((d) => { const w = [...d.rules[i].when]; w[ci] = nc; setWhen(d.rules[i], w); }) : commitTyped())} />
                      {r.when.length > 1 && <button className="btn sm" title="Remove condition" aria-label="Remove condition" onClick={() => mod((d) => { setWhen(d.rules[i], d.rules[i].when.filter((_, j) => j !== ci)); })}>✕</button>}
                    </div>)}
                    {kinds.some((x) => !r.when.some((y) => y.k === x)) && <AddKind kinds={kinds.filter((x) => !r.when.some((y) => y.k === x))} label="Add condition"
                      onAdd={(x) => mod((d) => { setWhen(d.rules[i], [...d.rules[i].when, blankCond(x, gl)]); })} />}
                  </div>
                </>}
                <div><div style={{ fontWeight: 600, fontSize: 13, marginBottom: 6 }}>Approvers, in order</div>
                  <RolePicker roles={r.approvers} free={roles.filter((x) => !r.notify.includes(x))} ordered empty="Nobody: a request that fits is approved automatically"
                    onChange={(l) => {
                      // A rule added here was switched off only until it had an approver: the first one switches it on.
                      const wake = fresh.current.has(r.id) && !r.on && l.length > 0;
                      if (wake) fresh.current.delete(r.id);
                      mod((d) => { d.rules[i].approvers = l; if (wake) d.rules[i].on = true; }, false, wake ? 'Saved. The rule is now switched on' : undefined);
                    }} /></div>
                <div><div style={{ fontWeight: 600, fontSize: 13, marginBottom: 6 }}>Also tell</div>
                  <RolePicker roles={r.notify} free={roles.filter((x) => !r.approvers.includes(x))} empty="Nobody else"
                    onChange={(l) => mod((d) => { d.rules[i].notify = l; })} /></div>
              </div>}
            </div>;
          })}</div>
          {kinds.length > 0 && <div className="row" style={{ marginTop: 12, flexWrap: 'wrap' }}>
            <span style={{ flex: 1 }} />
            <AddKind kinds={kinds} label="Add rule" value={kind} onPick={setKind} onAdd={(x) => {
              const c = blankCond(x, gl), id = `new-${Date.now()}-${ruleSeq++}`;
              fresh.current.add(id);
              // Switched off until it has an approver, so no request is approved automatically meanwhile.
              mod((d) => { d.rules.splice(d.rules.length - 1, 0, { id, label: whenText([c]), on: false, when: [c], approvers: [], notify: [] }); }, false,
                'Rule added, switched off. It switches on when you pick its first approver');
              setOpen(id);
            }} />
          </div>}
          <div style={{ marginTop: 14, display: 'grid', gap: 8 }}>
            <div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
              <span className="ink2" style={{ fontSize: 13 }}>If an approver of <b>this flow</b> does not act in time</span>
              <select value={draft.noResponse} aria-label="If an approver does not act in time" onChange={(e) => { const v = e.target.value; mod((d) => { d.noResponse = v; }); }}>{NORESP.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select>
            </div>
            <div className="row" style={{ flexWrap: 'wrap', gap: 10, alignItems: 'center' }}>
              <span className="ink2" style={{ fontSize: 13 }}>Time to act, the same for <b>every request type</b>:</span>
              <span className="row nowrap" style={{ gap: 6, alignItems: 'center' }}>
                <input type="number" min={1} max={720} step={1} value={sla} aria-label="Hours to act, for every request type" aria-invalid={!!slaErr}
                  onChange={(e) => setSla(e.target.value === '' ? '' : +e.target.value)} onBlur={saveSla} style={{ width: 72 }} />
                <span className="ink2" style={{ fontSize: 13 }}>hours</span></span>
              {slaErr && <span role="alert" style={{ color: 'var(--bad)', fontSize: 12.5, fontWeight: 600 }}>{slaErr}</span>}
            </div>
          </div>
          <TestRequest wf={wf} path={k} grades={gl} saves={saves} />
        </>}
        </>}
      </div>
    </>
  );
}

/** A list of condition kinds and a button that adds the one picked. */
function AddKind({ kinds, label, value, onPick, onAdd }: { kinds: string[]; label: string; value?: string; onPick?: (k: string) => void; onAdd: (k: string) => void }) {
  const [own, setOwn] = useState('');
  const cur = value ?? own;
  const sel = kinds.includes(cur) ? cur : kinds[0];
  return <span className="row" style={{ gap: 6 }}>
    <select value={sel} aria-label={`${label}: what it checks`} onChange={(e) => (onPick ? onPick(e.target.value) : setOwn(e.target.value))}>{kinds.map((c) => <option key={c} value={c}>{COND[c]}</option>)}</select>
    <button className="btn" onClick={() => onAdd(sel)}>{label}</button>
  </span>;
}
