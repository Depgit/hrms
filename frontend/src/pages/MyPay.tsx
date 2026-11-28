import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, fmtD, inr, uploadDoc, today } from '../api';
import { useAuth } from '../auth';
import { Chain, Field, Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import PersonPicker from '../components/PersonPicker';
import { PAID_DAYS, SHORT, TeamCover, leaveCountLine, leavePayload, shortHrs, workedLine } from './Approvals';
import { TYPE_RULES } from './Compensation';
import { MyNominations } from './Nominations';
import { MyAttendance, MyDiscipline } from './MyTime';
import { MyPayslips } from './PayItems';
import { MyForm16, MyTax } from './Tax';

type Tab = 'slips' | 'tax' | 'f16' | 'leave' | 'att' | 'nom' | 'disc' | 'claims' | 'adv' | 'me';
const stPill = (s: string) => <Pill k={s === 'Pending' ? 'warn' : s === 'Rejected' ? 'bad' : s === 'Cancelled' || s === 'Withdrawn' ? 'n' : 'ok'}>{s}</Pill>;
const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });

/** My pay and profile: the employee's self-service page (prototype vEss). The exit and "Start resignation" cards are on My view › Separation (MyExit.tsx). */
export default function MyPay() {
  const { me } = useAuth();
  const [qp, setQp] = useSearchParams();
  const emp = useLoad(() => api(`/employees/${me!.ctx.employeeId}`), [me?.ctx.employeeId]);
  const changes = useLoad(() => api<any[]>('/me/requests?wf=p_change'), []);
  const bal = useLoad(() => api('/leave/balances/me').catch(() => null), []);
  const e = emp.data;
  if (!e) return <Loading error={emp.error} />;
  const T = TYPE_RULES[e.type];
  const gone = e.status === 'Inactive';
  // Prototype canLeave: the employment type takes leave, or a leave type of the plan applies to this person.
  const canLeave = T?.leave !== false || (bal.data?.types?.length ?? 0) > 0;
  // Prototype isSalTds: Form 16 is for salaried staff (all deductions, with payslips).
  const salaried = T ? T.ded === 'all' && T.docs !== false : e.type === 'Permanent';
  const okT = (x: Tab) => !gone || ['slips', 'tax', 'f16', 'me'].includes(x);
  const tabs = ([['slips', 'Payslips'], ['tax', 'Tax declaration'], ...(salaried ? [['f16', 'Form 16']] : []), ...(canLeave ? [['leave', 'My leave']] : []), ['att', 'My attendance'],
    ['nom', 'Nominations'], ['disc', 'My roster discipline'], ...(me!.claimsOn !== false ? [['claims', 'Reimbursements']] : []), ...(e.type === 'Permanent' ? [['adv', 'Salary advance']] : []), ['me', 'My details']] as [Tab, string][]).filter(([k]) => okT(k));
  const want = qp.get('tab') as Tab | null;
  const t: Tab = want && tabs.some(([k]) => k === want) ? want : 'slips';
  const hold = changes.data?.some((r) => r.status === 'Pending' && r.payload.kind === 'Bank');
  return (
    <>
      <div className="row sp" style={{ marginBottom: 14 }}>
        <div><h2 style={{ margin: 0, fontSize: 18 }}>{e.name}</h2><div className="mute" style={{ fontSize: 13 }}>{e.id} · {e.designation} · {e.location}</div></div>
        {hold && <Pill k="bad">Salary on hold until your bank change is verified</Pill>}
      </div>
      <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${t === k ? 'on' : ''}`} onClick={() => setQp({ tab: k })}>{l}</button>)}</div>
      {t === 'slips' && <MyPayslips />}
      {t === 'tax' && <MyTax />}
      {t === 'f16' && <MyForm16 />}
      {t === 'leave' && <MyLeave e={e} bal={bal.data} reloadBal={bal.reload} canLeave={canLeave} />}
      {t === 'att' && <MyAttendance />}
      {t === 'nom' && <MyNominations />}
      {t === 'disc' && <MyDiscipline />}
      {t === 'claims' && <Reimbursements />}
      {t === 'adv' && <MyAdvance />}
      {t === 'me' && <MyDetails e={e} changes={changes.data ?? []} onChange={() => { changes.reload(); emp.reload(); }} />}
    </>
  );
}

async function cancel(id: string, done: () => void, msg = 'Request cancelled') {
  try { await api(`/approvals/${id}/cancel`, { method: 'POST' }); toast(msg); done(); } catch (x: any) { toast(x.message); }
}

// ---------- My leave (prototype vLeaveEss) ----------

const LEAVE_SHOW = ['Casual leave', 'Privilege leave', 'Sick leave'];

function MyLeave({ e, bal, reloadBal, canLeave }: { e: any; bal: any; reloadBal: () => void; canLeave: boolean }) {
  const list = useLoad(() => api<any[]>('/me/requests?wf=leave'), []);
  const [open, setOpen] = useState(false);
  const all: any[] = bal?.types ?? [];
  const shown = [...LEAVE_SHOW.map((k) => all.find((x) => x.type === k) ?? { type: k }), ...all.filter((x) => !LEAVE_SHOW.includes(x.type) && x.balance)];
  const mine = list.data ? [...list.data].reverse() : null;
  const done = () => { list.reload(); reloadBal(); };
  return (
    <>
      <div className="grid g3" style={{ marginBottom: 14 }}>
        {shown.map((x) => !x.balance ? <div key={x.type} className="card stat"><div className="l">{x.type}</div><div className="n" style={{ fontSize: 16 }}>Not credited</div></div>
          : <div key={x.type} className="card stat"><div className="l">{x.type}</div><div className="n">{x.balance.bal}<span className="mute" style={{ fontSize: 13, fontWeight: 500 }}> days left</span></div>
            <div className="mute" style={{ fontSize: 12 }}>Credited {x.balance.credit} · taken {x.balance.used}{x.balance.pending ? ` · waiting ${x.balance.pending}` : ''}{x.balance.inProbBlock ? ' · not usable in probation' : ''}</div>
            {x.balance.worked && <details className="mute" style={{ fontSize: 12, marginTop: 4 }}><summary style={{ cursor: 'pointer' }}>{workedLine(x.balance.worked)}</summary>
              <div>Counted on {PAID_DAYS}, once each day is over, while you are active.</div>
              {x.balance.worked.months.length ? x.balance.worked.months.map((m: any) => <div key={m.month}>{fmtD(m.month + '-01').replace(/^\d+ /, '')}: {m.days} paid days{(x.balance.worked.credits ?? []).filter((c: any) => c.on.slice(0, 7) === m.month).map((c: any) => `; ${c.credit} credited on ${fmtD(c.on)}`).join('')}</div>) : <div>No paid day counted yet this leave year.</div>}
              {(x.balance.worked.credits ?? []).filter((c: any) => !x.balance.worked.months.some((m: any) => m.month === c.on.slice(0, 7))).map((c: any) => <div key={c.on}>{c.credit} credited on {fmtD(c.on)}</div>)}</details>}</div>)}
      </div>
      <div className="card">
        <div className="row sp" style={{ flexWrap: 'wrap', gap: 8, marginBottom: 10 }}>
          <div><h3 style={{ margin: 0 }}>My leave requests</h3><div className="mute" style={{ fontSize: 12.5 }}>{bal?.plan ? `Leave plan: ${bal.plan}. ` : ''}You can cancel a request until it is approved.</div></div>
          <button className="btn primary" onClick={() => {
            if (!canLeave) { toast('Leave does not apply to your employment type'); return; }
            if (!all.some((x) => x.applyBy !== 'hr')) { toast('Your leave plan has no leave you can apply for yourself'); return; }
            setOpen(true);
          }}>Apply for leave</button>
        </div>
        <div className="tw"><table>
          <thead><tr><th>Request</th><th>Leave</th><th>Dates</th><th className="num">Days</th><th>Status</th><th></th></tr></thead>
          <tbody>{mine?.map((r) => <tr key={r.id}>
            <td className="mono">{r.id}</td><td>{r.payload.ltype}{r.payload.half && r.payload.halfOf ? ` (${r.payload.halfOf} half)` : ''}</td><td>{fmtD(r.payload.from)}{r.payload.short ? `, ${r.payload.fromTime} to ${r.payload.toTime}` : r.payload.to !== r.payload.from ? ` to ${fmtD(r.payload.to)}` : ''}</td>
            <td className="num">{r.payload.short ? shortHrs(r.payload) : r.facts?.days}</td><td>{stPill(r.status)}{r.status === 'Pending' && r.waiting_on && <div className="mute" style={{ fontSize: 12 }}>Waiting for {r.waiting_on}{r.waiting_name ? ` (${r.waiting_name})` : ''}</div>}</td>
            <td>{r.status === 'Pending' && <button className="btn sm" onClick={() => cancel(r.id, done, 'Request cancelled. The days are back in your balance')}>Cancel request</button>}</td>
          </tr>)}
            {mine && !mine.length && <tr><td colSpan={6} className="mute">No leave requests yet.</td></tr>}</tbody>
        </table></div>
      </div>
      {open && <LeaveDialog e={e} bal={bal} onClose={() => setOpen(false)} onDone={() => { setOpen(false); done(); }} />}
    </>
  );
}

/** The leave request dialog (prototype nrModal('leave')). A short leave (part of a day, 20 Nov 2026) is chosen as the leave type
 *  "Short leave" when the leave plan allows it: a date, a from-time and a to-time, no balance. */
function LeaveDialog({ e, bal, onClose, onDone }: { e: any; bal: any; onClose: () => void; onDone: () => void }) {
  const types: any[] = [...(bal?.types ?? []).filter((x: any) => x.applyBy !== 'hr'), ...(bal?.short ? [{ type: SHORT, short: true }] : [])];
  const [p, setP] = useState<any>({ ltype: types[0]?.type ?? '', from: today(), to: today(), note: '', half: false, halfOf: 'first', doc: '', fromTime: '', toTime: '' });
  const [err, setErr] = useState<string[]>([]);
  const [sim, setSim] = useState<any>(null);
  const [simErr, setSimErr] = useState('');
  const cur = types.find((x) => x.type === p.ltype) ?? types[0];
  // Prototype nrPrev: the balance is shown as on the From date.
  const onFrom = useLoad(() => (p.from ? api<any>(`/leave/balances/me?on=${p.from}`).catch(() => null) : Promise.resolve(null)), [p.from]);
  const curBal = (onFrom.data?.types ?? []).find((x: any) => x.type === cur?.type)?.balance ?? (onFrom.data ? undefined : cur?.balance);
  const [docName, setDocName] = useState('');
  const isShort = !!cur?.short;
  const to = p.half || isShort ? p.from : p.to || p.from;
  const days = p.from && to >= p.from ? Math.round((new Date(to + 'T00:00:00').getTime() - new Date(p.from + 'T00:00:00').getTime()) / 864e5) + 1 : 0;
  // The route preview (prototype nrPrev): days counted, team cover and who approves, as the dates change.
  useEffect(() => {
    if (!cur || !days) { setSim(null); return; }
    if (isShort && (!p.fromTime || !p.toTime)) { setSim(null); return; }
    const t = setTimeout(() => api('/approvals/simulate', { body: { wf: 'leave', employeeId: e.id, payload: leavePayload(cur, p, to) } })
      .then((x) => { setSim(x); setSimErr(''); }).catch((x) => { setSim(null); setSimErr(x.message); }), 250);
    return () => clearTimeout(t);
  }, [cur?.type, p.from, to, p.half, p.halfOf, p.fromTime, p.toTime, days, e.id]);
  const send = async () => {
    setErr([]);
    try {
      const r = await api('/approvals', { body: { wf: 'leave', payload: { ...leavePayload(cur, p, to), note: p.note } } });
      toast(`${r.id} sent for approval`); onDone();
    } catch (x: any) { setErr((x.details as string[]) || [x.message]); }
  };
  const upload = async (f?: File) => {
    if (!f) return;
    try { const d = await uploadDoc('employee', e.id, `leave-${Date.now()}`, f); setP({ ...p, doc: d.id }); setDocName(f.name); toast('Attached'); } catch (x: any) { toast(x.message); }
  };
  const problems = [...new Set([...(simErr ? [simErr] : []), ...(sim?.problems ?? []), ...err])];
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>New leave request</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div style={{ marginTop: 12 }}><div className="form">
      <Field label="Employee"><PersonPicker label="Employee" people={[e]} value={e.id} onChange={() => {}} disabled /></Field>
      <Field label="Leave type"><select value={cur?.type ?? ''} onChange={(x) => { setP({ ...p, ltype: x.target.value, doc: '' }); setDocName(''); }}>{types.map((x) => <option key={x.type}>{x.type}</option>)}</select></Field>
      <Field label={isShort ? 'Date' : 'From'}><input type="date" value={p.from} onChange={(x) => setP({ ...p, from: x.target.value, to: p.to < x.target.value ? x.target.value : p.to })} /></Field>
      {!p.half && !isShort && <Field label="To"><input type="date" value={p.to} onChange={(x) => setP({ ...p, to: x.target.value })} /></Field>}
      {isShort && <><Field label="From time"><input type="time" value={p.fromTime} onChange={(x) => setP({ ...p, fromTime: x.target.value })} /></Field>
        <Field label="To time"><input type="time" value={p.toTime} onChange={(x) => setP({ ...p, toTime: x.target.value })} /></Field></>}
      <label className="field full"><span>Reason</span><input type="text" value={p.note} onChange={(x) => setP({ ...p, note: x.target.value })} /></label>
      {cur?.docAfter > 0 && <label className="field full"><span>{cur.docLabel || 'Document'} (needed when more than {cur.docAfter} day{cur.docAfter > 1 ? 's' : ''})</span>
        <input type="file" accept=".pdf,.jpg,.jpeg,.png" onChange={(x) => upload(x.target.files?.[0])} />{docName && <small className="mute">Attached: {docName}</small>}</label>}
    </div>
    {/* Half-day leave is a leave plan setting ("Half-day leave allowed"); kept below the prototype's fields. */}
    {bal?.half && !isShort && <div className="row" style={{ marginTop: 10, gap: 12 }}><label className="chkl"><input type="checkbox" checked={p.half} onChange={(x) => setP({ ...p, half: x.target.checked })} />Half day</label>
      {p.half && <select value={p.halfOf} onChange={(x) => setP({ ...p, halfOf: x.target.value })}><option value="first">First half on leave</option><option value="second">Second half on leave</option></select>}</div>}
    {isShort && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Up to {bal.short.maxHours} hours, {bal.short.perMonth} a month. No balance is used; once approved the time is not counted as late, early or short hours.</p>}</div>
    {cur && <div className="card" style={{ marginTop: 14 }}>
      {!isShort && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>{cur.type} balance on {fmtD(p.from || today())}: <b>{curBal ? curBal.bal : 'not applicable'}</b>{curBal ? ` (credited ${curBal.credit}, used ${curBal.used}, waiting ${curBal.pending || 0})` : ''}{curBal?.worked && <><br /><span className="mute" style={{ fontSize: 12 }}>{workedLine(curBal.worked)}</span></>}</p>}
      {sim?.days != null && !isShort && <p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>{leaveCountLine(cur.type, sim.days, sim.offs ?? 0, sim.range ?? 0)}</p>}
      {sim?.team != null && <TeamCover from={p.from} to={to} teamOff={sim.teamOff} team={sim.team} location={sim.location} dept={sim.department} />}
      {sim && <><h3>Approval route</h3>
        {sim.chain.length ? <Chain steps={sim.chain} /> : <div className="mute" style={{ fontSize: 13 }}>No approver is needed. It is approved straight away.</div>}
        {sim.people.length > 0 && <div className="mute" style={{ fontSize: 12, marginTop: 6 }}>{sim.people.map((x: any) => `${x.stage}: ${x.name}`).join(' · ')}</div>}
        {sim.notify?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Will be informed: {sim.notify.map((x: any) => `${x.role} (${x.person})`).join(', ')}</p>}
        {sim.why?.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Added because: {sim.why.join(', ')}</p>}
      </>}
      {problems.length > 0 && <div className="note bad" style={{ marginTop: 8 }}><ul style={{ margin: 0, paddingLeft: 18 }}>{problems.map((x) => <li key={x}>{x}</li>)}</ul></div>}
    </div>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={send}>Send for approval</button></div>
  </Modal>;
}

// ---------- Reimbursements ----------

const CLAIM_TYPES = ['Travel', 'Mobile and internet', 'Other'];

function Reimbursements() {
  const { me } = useAuth();
  const list = useLoad(() => api<any[]>('/me/requests?wf=p_claim'), []);
  const [p, setP] = useState({ claimType: CLAIM_TYPES[0], billDate: today(), amount: '', note: '' });
  const [bill, setBill] = useState<File | null>(null);
  const [k, setK] = useState(0);
  const send = async () => {
    if (!+p.amount) { toast('Enter the amount'); return; }
    if (!bill) { toast('Attach the bill'); return; }
    try {
      const doc = await uploadDoc('employee', me!.ctx.employeeId, 'Claim bill', bill);
      await api('/approvals', { body: { wf: 'p_claim', payload: { ...p, amount: +p.amount, doc: doc.id } } });
      toast('Sent to Payroll'); setP({ ...p, amount: '', note: '' }); setBill(null); setK(k + 1); list.reload();
    } catch (x: any) { toast(((x.details as string[]) || [x.message]).join(' · ')); }
  };
  const mine = list.data ? [...list.data].reverse() : null;
  return (
    <>
      <div className="card" style={{ marginBottom: 14 }}>
        <h3>New claim</h3>
        <div className="form">
          <Field label="Type"><select value={p.claimType} onChange={(x) => setP({ ...p, claimType: x.target.value })}>{CLAIM_TYPES.map((t) => <option key={t}>{t}</option>)}</select></Field>
          <Field label="Bill date"><input type="date" value={p.billDate} max={today()} onChange={(x) => setP({ ...p, billDate: x.target.value })} /></Field>
          <Field label="Amount (₹)"><input type="number" min={1} value={p.amount} onChange={(x) => setP({ ...p, amount: x.target.value })} /></Field>
          <Field label="Bill (file)"><input key={k} type="file" accept="application/pdf,image/png,image/jpeg" onChange={(x) => setBill(x.target.files?.[0] ?? null)} /></Field>
          <label className="field full"><span>Note</span><input type="text" value={p.note} onChange={(x) => setP({ ...p, note: x.target.value })} placeholder="For example: client visit, Gurugram to Jaipur" /></label>
        </div>
        <div className="row" style={{ marginTop: 10 }}><button className="btn primary" onClick={send}>Submit claim</button></div>
        <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Payroll approves it, and it is added to your salary.</p>
      </div>
      <div className="card"><div className="tw"><table>
        <thead><tr><th>Claim</th><th>Type</th><th>Date</th><th className="num">Amount</th><th>Status</th><th></th></tr></thead>
        <tbody>{mine?.map((r) => <tr key={r.id}><td className="mono">{r.id}</td><td>{r.payload.claimType}</td><td>{fmtD(r.payload.billDate)}</td><td className="num">{inr(r.payload.amount)}</td>
          <td>{stPill(r.status)}{r.payload.payMonth && <> <span className="mute" style={{ fontSize: 12 }}>{ymLabel(r.payload.payMonth)}</span></>}
            {r.status === 'Pending' && r.waiting_on && <div className="mute" style={{ fontSize: 12 }}>Waiting for {r.waiting_on}{r.waiting_name ? ` (${r.waiting_name})` : ''}</div>}</td>
          <td>{r.status === 'Pending' && <button className="btn sm" onClick={() => cancel(r.id, list.reload)}>Cancel request</button>}</td></tr>)}
          {mine && !mine.length && <tr><td colSpan={6} className="mute">No claims yet.</td></tr>}</tbody>
      </table></div></div>
    </>
  );
}

// ---------- Salary advance (the employee's own request, advances.go) ----------

/** Equal instalments with any rounding in the last one, as the server schedules them (calc.Instalments). */
export const splitInstalments = (amount: number, n: number) => {
  const k = Math.max(1, n), each = Math.floor(amount / k);
  return { each, last: Math.round((amount - each * (k - 1)) * 100) / 100 };
};

function MyAdvance() {
  const f = useLoad(() => api<any>('/me/advance'), []);
  const list = useLoad(() => api<any[]>('/me/requests?wf=all').then((l) => l.filter((r) => r.wf === 'p_adv')), []);
  const [p, setP] = useState({ amount: '', note: '' });
  const [sim, setSim] = useState<any>(null);
  const [errs, setErrs] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const amt = +p.amount;
  // The approval route and the server's checks, as the amount changes.
  useEffect(() => {
    if (!(amt > 0) || !f.data?.eligible) { setSim(null); return; }
    const t = setTimeout(() => api('/approvals/simulate', { body: { wf: 'p_adv', payload: { amount: amt, note: p.note || 'check' } } })
      .then(setSim).catch(() => setSim(null)), 300);
    return () => clearTimeout(t);
  }, [amt, f.data?.eligible]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!f.data) return <Loading error={f.error} />;
  const x = f.data, r = x.rules;
  const reload = () => { f.reload(); list.reload(); };
  const send = async () => {
    setErrs([]);
    if (!(amt > 0)) { setErrs(['Enter the amount']); return; }
    if (p.note.trim().length < 3) { setErrs(['Give the reason for the advance']); return; }
    setBusy(true);
    try {
      const out = await api('/approvals', { body: { wf: 'p_adv', payload: { amount: amt, note: p.note.trim() } } });
      toast(`${out.id} sent for approval`); setP({ amount: '', note: '' }); setSim(null); reload();
    } catch (e: any) { setErrs((e.details as string[]) || [e.message]); } finally { setBusy(false); }
  };
  const problems = [...new Set([...(sim?.problems ?? []), ...errs])];
  const mine = list.data ?? [];
  return (
    <div className="grid g2" style={{ alignItems: 'start' }}>
      <div className="card"><h3>Request a salary advance</h3>
        <ul className="ink2" style={{ fontSize: 13, margin: '0 0 12px', paddingLeft: 18, lineHeight: 1.7 }} data-testid="adv-rules">
          <li>{x.maxAmount > 0 ? <>You can ask for up to <b>{inr(x.maxAmount)}</b> ({r.maxMultiple} × your monthly gross of {inr(x.gross)}).</> : 'There is no set limit on the amount; your approvers decide.'}</li>
          {r.oneOpen && <li>One advance at a time: a new one can be asked for once the last one is fully recovered.</li>}
          <li>Paid with your <b>{ymLabel(x.payMonth)}</b> salary and recovered in full from your <b>{ymLabel(x.startYm)}</b> salary.</li>
          <li>Need it recovered over more months? Write it in the reason (for example "deduct in 3 months"). Your approver sets the instalments when approving, up to {x.maxInstalments}.</li>
          <li>For permanent employees with at least {r.minMonths ?? 3} months' service.</li>
          <li>It goes to your approvers; you can cancel it until it is approved.</li>
        </ul>
        {!x.eligible ? <div className="note warn" style={{ fontSize: 13 }}>{x.why}</div> : <>
          <div className="form">
            <Field label="Amount (₹)"><input type="number" min={1} max={x.maxAmount || undefined} value={p.amount} onChange={(e) => setP({ ...p, amount: e.target.value })} /></Field>
            <label className="field full"><span>Reason and recovery wish</span><input type="text" value={p.note} onChange={(e) => setP({ ...p, note: e.target.value })} placeholder="For example: medical emergency in the family, please deduct in 3 months" /></label>
          </div>
          {amt > 0 && <div className="note" style={{ marginTop: 12, fontSize: 13 }} data-testid="adv-projection">
            One deduction of <b>{inr(amt)}</b> from your {ymLabel(x.startYm)} salary, unless your approver sets more instalments.</div>}
          {sim?.chain && <div style={{ marginTop: 12 }}><h3 style={{ fontSize: 14 }}>Approval route</h3>
            {sim.chain.length ? <Chain steps={sim.chain} /> : <div className="mute" style={{ fontSize: 13 }}>No approver is needed.</div>}
            {sim.people?.length > 0 && <div className="mute" style={{ fontSize: 12, marginTop: 6 }}>{sim.people.map((y: any) => `${y.stage}: ${y.name}`).join(' · ')}</div>}</div>}
          {problems.length > 0 && <div className="note bad" style={{ marginTop: 10 }}><ul style={{ margin: 0, paddingLeft: 18 }}>{problems.map((y) => <li key={y}>{y}</li>)}</ul></div>}
          <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={busy} onClick={send}>Send for approval</button></div>
        </>}
      </div>
      <div className="card"><h3>My advances</h3>
        <div className="tw"><table>
          <thead><tr><th>Advance</th><th className="num">Amount</th><th>Recovery</th><th>Status</th><th></th></tr></thead>
          <tbody>{mine.map((a) => { const q = a.payload || {}, k = q.instalments || 1; return <tr key={a.id}>
            <td className="mono">{a.id}{q.opening && <div><Pill k="info">Opening balance</Pill></div>}</td><td className="num">{inr(q.amount)}</td>
            <td style={{ fontSize: 12.5 }}>{q.opening ? `${inr(q.balance)} left at go-live · ${inr(q.monthly)} a month from ${ymLabel(q.startYm)}` : `${k} × ${inr(splitInstalments(q.amount || 0, k).each)} from ${q.startYm ? ymLabel(q.startYm) : '—'}`}</td>
            <td>{stPill(a.status)}{a.status === 'Pending' && a.waiting_on && <div className="mute" style={{ fontSize: 12 }}>Waiting for {a.waiting_on}{a.waiting_name ? ` (${a.waiting_name})` : ''}</div>}</td>
            <td>{a.status === 'Pending' && a.canCancel && <button className="btn sm" onClick={() => cancel(a.id, reload)}>Cancel request</button>}</td></tr>; })}
            {list.data && !mine.length && <tr><td colSpan={5} className="mute">No advances yet.</td></tr>}</tbody>
        </table></div>
        {x.open && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>{x.open.status === 'Pending' ? `${x.open.id} is waiting for approval.` : `${inr(x.open.balance)} of ${x.open.id} is still to be recovered from salary.`}</p>}
      </div>
    </div>
  );
}

// ---------- My details ----------

/** Prototype nowS: "04 Nov, 11:37 am". */
const stamp = (t: string) => new Date(t).toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' });
const last4 = (v?: string | null) => (v ? String(v).replace(/\s/g, '').replace(/.(?=.{4})/g, '•') : '—');
const PKEYS: [string, string][] = [['mobile', 'mobile'], ['email', 'email'], ['address', 'address'], ['ecName', 'ec_name'], ['ecRelation', 'ec_relation'], ['ecMobile', 'ec_mobile']];

function MyDetails({ e, changes, onChange }: { e: any; changes: any[]; onChange: () => void }) {
  const { me } = useAuth();
  const lk = useLoad(() => api('/lookups'), []);
  const pd = e.personal ?? {};
  const init = () => Object.fromEntries(PKEYS.map(([k, f]) => [k, pd[f] ?? '']));
  const [kind, setKind] = useState<'Personal' | 'Bank'>('Personal');
  const [p, setP] = useState<Record<string, string>>(init);
  const [b, setB] = useState({ bankName: '', accountNo: '', ifsc: '' });
  const [cheque, setCheque] = useState<File | null>(null);
  const set = (k: string, v: string) => setP({ ...p, [k]: v });
  const send = async () => {
    const payload: Record<string, string> = { kind };
    if (kind === 'Bank') {
      if (!b.bankName.trim() || !/^\d{6,18}$/.test(b.accountNo.trim()) || !/^[A-Z]{4}0[A-Z0-9]{6}$/.test(b.ifsc.trim())) { toast('Check the bank name, account number and IFSC'); return; }
      Object.assign(payload, { bankName: b.bankName.trim(), accountNo: b.accountNo.trim(), ifsc: b.ifsc.trim() });
    } else {
      // Prototype ecErr: the emergency contact is checked before it is sent.
      const ecN = (p.ecName ?? '').trim(), ecR = p.ecRelation ?? '', ecM = (p.ecMobile ?? '').trim();
      if (ecN || ecR || ecM) {
        const er = ecN.length < 2 ? 'Enter the emergency contact name' : !ecR ? 'Choose the relation of the emergency contact' : !/^[6-9]\d{9}$/.test(ecM) ? 'Emergency contact mobile must be a 10 digit number'
          : String(p.mobile || pd.mobile || '').replace(/\D/g, '').slice(-10) === ecM ? 'Emergency contact must have a different number from the employee' : '';
        if (er) { toast(er); return; }
      }
      PKEYS.forEach(([k, f]) => { const v = (p[k] ?? '').trim(); if (v && v !== (pd[f] ?? '')) payload[k] = v; });
      // An emergency contact goes as a whole: name, relation and mobile.
      if (payload.ecName || payload.ecRelation || payload.ecMobile) ['ecName', 'ecRelation', 'ecMobile'].forEach((k) => { payload[k] = (p[k] ?? '').trim(); });
    }
    try {
      if (kind === 'Bank' && cheque) payload.doc = (await uploadDoc('employee', me!.ctx.employeeId, 'Cancelled cheque', cheque)).id;
      await api('/approvals', { body: { wf: 'p_change', payload } });
      toast(kind === 'Bank' ? 'Sent to Payroll. Salary is held until verified' : 'Sent to Payroll');
      setB({ bankName: '', accountNo: '', ifsc: '' }); setCheque(null); onChange();
    } catch (x: any) { toast(x.message); }
  };
  const ec = pd.ec_name ? `${pd.ec_name} (${pd.ec_relation}) · ${pd.ec_mobile}` : '—';
  return (
    <div className="grid g2" style={{ alignItems: 'start' }}>
      <div className="card"><h3>My details</h3>
        <dl className="kv">
          <dt>Mobile</dt><dd>{pd.mobile || '—'}</dd><dt>Email</dt><dd>{pd.email || '—'}</dd><dt>Address</dt><dd>{pd.address || '—'}</dd>
          <dt>Emergency contact</dt><dd>{ec}</dd>
          <dt>Bank</dt><dd>{e.bank ? `${e.bank.bank_name} ${last4(e.bank.account_no)}` : '—'}</dd>
          <dt>PAN</dt><dd>{last4(e.govId?.pan)}</dd><dt>Aadhaar</dt><dd>{last4(e.govId?.aadhaar)}</dd>
        </dl>
      </div>
      <div className="card"><h3>Request a change</h3>
        <Field label="What do you want to change?"><select value={kind} onChange={(x) => setKind(x.target.value as 'Personal' | 'Bank')}><option value="Personal">Personal details</option><option value="Bank">Bank account</option></select></Field>
        {kind === 'Bank' ? <>
          <div className="form" style={{ marginTop: 10 }}>
            <Field label="Bank name"><input type="text" value={b.bankName} onChange={(x) => setB({ ...b, bankName: x.target.value })} /></Field>
            <Field label="Account number"><input type="text" value={b.accountNo} onChange={(x) => setB({ ...b, accountNo: x.target.value })} /></Field>
            <Field label="IFSC"><input type="text" value={b.ifsc} onChange={(x) => setB({ ...b, ifsc: x.target.value.toUpperCase() })} /></Field>
            <Field label="Cancelled cheque"><input type="file" accept="application/pdf,image/png,image/jpeg" onChange={(x) => setCheque(x.target.files?.[0] ?? null)} /></Field>
          </div>
          <div className="note bad" style={{ marginTop: 10, fontSize: 12.5 }}>Your salary is held until Payroll verifies the new account.</div>
        </> : <div className="form" style={{ marginTop: 10 }}>
          <Field label="Mobile"><input type="text" value={p.mobile} onChange={(x) => set('mobile', x.target.value)} /></Field>
          <Field label="Email"><input type="text" value={p.email} onChange={(x) => set('email', x.target.value)} /></Field>
          <label className="field full"><span>Address</span><input type="text" value={p.address} onChange={(x) => set('address', x.target.value)} /></label>
          <Field label="Emergency contact name"><input type="text" value={p.ecName} onChange={(x) => set('ecName', x.target.value)} /></Field>
          <Field label="Relation"><select value={p.ecRelation} onChange={(x) => set('ecRelation', x.target.value)}><option value="">Select</option>{(lk.data?.ecRelations ?? []).map((r: string) => <option key={r}>{r}</option>)}</select></Field>
          <Field label="Emergency contact mobile"><input type="text" value={p.ecMobile} placeholder="10 digit number" onChange={(x) => set('ecMobile', x.target.value)} /></Field>
        </div>}
        <div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={send}>Send to Payroll</button></div>
        {changes.length > 0 && <><h3 style={{ margin: '16px 0 8px' }}>My requests</h3>
          <ul className="log">{[...changes].reverse().map((r) => <li key={r.id}>{r.payload.kind} · {stPill(r.status)}
            <div className="mute" style={{ fontSize: 12 }}>{stamp(r.created_at)}{r.status === 'Pending' && r.waiting_on ? ` · waiting for ${r.waiting_on}${r.waiting_name ? ` (${r.waiting_name})` : ''}` : ''}</div></li>)}</ul></>}
      </div>
    </div>
  );
}
