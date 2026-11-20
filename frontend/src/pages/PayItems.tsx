import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api, inr, fmtD, openDoc, uploadDoc, addMonths, postForm } from '../api';
import { useAuth } from '../auth';
import { Loading, Modal, Pill, download, toast, useLoad } from '../components/ui';
import EmpSearch, { type EmpHit } from '../components/EmpSearch';
import Payslip, { saveSlipHtml, useDocUrl, type SlipView } from '../components/Payslip';
import { ymLabel } from './PayrollRun';
import { WORKFLOWS } from './Approvals';
import Inventory, { Assets, InvSetup, Uniform, UniformSizes } from './Inventory';
import { SkipApprovalButton } from '../components/SkipApproval';

const nextYm = (ym: string, n = 1) => addMonths(ym, n);
const months = (from: string, n: number) => Array.from({ length: n }, (_, i) => nextYm(from, i));
const nm = (name?: string, id?: string) => <><b>{name}</b> <span className="mono mute">{id}</span></>;
const nmPlain = (name?: string, id?: string) => <>{name} <span className="mono mute">{id}</span></>;
const PS = { fontSize: 12 } as const;
/** Approval status as the prototype words it for pay items raised by Payroll. */
const stText = (s: string) => (s === 'Pending' ? 'Pending approval' : s);
const stPill = (s: string, info = false) => <Pill k={/^Pending/.test(s) ? 'warn' : s === 'Rejected' || s === 'Cancelled' ? 'bad' : info && s !== 'Closed' && s !== 'Waived' ? 'info' : 'ok'}>{s}</Pill>;
/** Go to Approvals (optionally at one request; Approvals may read ?id=). */
const useGoAppr = () => { const nav = useNavigate(); return (id?: string) => nav(id ? `/approvals?id=${encodeURIComponent(id)}` : '/approvals'); };

/** Kind of a pay line from its source (prototype kindOf). */
const kindOf = (l: any) => {
  const s = String(l.source_id || '');
  return s.startsWith('ARR') ? 'Arrear' : s.startsWith('CLM') ? 'Reimbursement' : s.startsWith('UNI') ? (l.kind === 'earn' ? 'Uniform refund' : 'Uniform recovery')
    : s.startsWith('ADV') ? (l.kind === 'earn' ? 'Salary advance' : 'Advance recovery') : s.startsWith('AST') ? 'Asset recovery' : s === 'TAX' ? 'Tax adjustment' : 'Other';
};
/** One-line summary of a pending pay request (prototype payItems sub). */
const subOf = (r: any) => {
  const p = r.payload || {};
  if (r.wf === 'p_claim') return `${p.claimType} · ${inr(p.amount)}`;
  if (r.wf === 'p_change') return p.kind === 'Bank' ? 'Bank account change' : 'Personal details change';
  if (r.wf === 'p_arrear') return `For ${p.forYm ? ymLabel(p.forYm) : '—'} · ${inr(p.amount)}`;
  if (r.wf === 'p_adv') return `Advance · ${inr(p.amount)} in ${p.instalments} instalments`;
  if (r.wf === 'p_ded') return `${p.dedType} · ${inr(p.amount)}`;
  if (r.wf === 'p_fnf') return `Last day ${fmtD(p.lwd)} · net ${inr(p.amount)}`;
  if (r.wf === 'p_pip') return r.sub || `PIP · ${p.days} days`;
  return p.amount ? inr(p.amount) : '';
};
/** Workflow name (prototype wfLabel); improvement plans come from consequence management. */
const wfName = (wf: string) => WORKFLOWS[wf] || (wf === 'p_pip' ? 'Performance improvement plan' : wf);

/** Upcoming pay lines (prototype vFwd): the next six months, the lines of one month by kind, changes ahead and what is waiting.
 *  `ym` opens on that month when it is one of the six (prototype S.fwdm). */
export function Upcoming({ ym }: { ym?: string } = {}) {
  const d = useLoad(() => api('/payroll/upcoming'), []);
  const [pick, setPick] = useState(ym ?? '');
  const goAppr = useGoAppr();
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, ms: string[] = x.months, m = pick && ms.includes(pick) ? pick : ms[0];
  const lines = x.lines.filter((l: any) => l.ym === m).sort((a: any, b: any) => (a.employee_id < b.employee_id ? -1 : 1));
  const kinds = [...new Set<string>(lines.map(kindOf))];
  const chg: [any, string, string][] = x.changes.filter((c: any) => c.ym === m).map((c: any) => String(c.what).startsWith('Bank account')
    ? [c, 'Bank account', `${String(c.what).replace(/^Bank account ?/, '')} from this month`] : [c, c.what, `${inr(c.old || 0)} → ${inr(c.new)} a month`]);
  const pend = x.pending.filter((r: any) => r.wf !== 'p_tax');
  return (
    <>
      <div className="note" style={{ marginBottom: 14 }}>Everything already scheduled to be added to or deducted from salary in the coming months: arrears, reimbursements, uniform recovery and refund, advance instalments, asset recoveries and tax adjustments. Use it to check a month before you run it.</div>
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>Next six months</h3><button className="btn" onClick={() => download('/payroll/upcoming.xlsx', 'upcoming-pay-lines.xlsx').catch((e) => toast(e.message))}>Download all lines</button></div>
        <div className="tw"><table><thead><tr><th>Month</th><th className="num">Lines</th><th className="num">Additions</th><th className="num">Deductions</th><th className="num">Net effect on pay</th><th></th></tr></thead><tbody>
          {ms.map((ym) => { const t = x.totals[ym]; return <tr key={ym} style={ym === m ? { background: 'var(--soft,#f6f7f9)' } : undefined}>
            <td><b>{ymLabel(ym)}</b>{ym === x.openYm && <> <Pill k="info">Open month</Pill></>}</td><td className="num">{t.count}</td><td className="num">{inr(t.earn)}</td><td className="num">{inr(t.ded)}</td><td className="num">{inr(t.earn - t.ded)}</td>
            <td><button className="btn sm" onClick={() => setPick(ym)}>Show</button></td></tr>; })}
        </tbody></table></div>
      </div>
      <div className="card"><h3>{ymLabel(m)}: {lines.length} line{lines.length === 1 ? '' : 's'}</h3>
        {kinds.length ? kinds.map((k) => { const ls = lines.filter((l: any) => kindOf(l) === k); return <div key={k} style={{ marginTop: 12 }}>
          <div className="row sp"><b>{k}</b><span className="ink2" style={{ fontSize: 12.5 }}>{ls.length} · {inr(ls.reduce((s: number, l: any) => s + l.amount, 0))} {ls[0].kind === 'earn' ? 'added' : 'deducted'}</span></div>
          <div className="tw"><table><tbody>{ls.slice(0, 100).map((l: any) => <tr key={l.id}><td style={{ width: '34%' }}>{nmPlain(l.name, l.employee_id)}</td><td>{l.label}{l.reason && <div className="mute" style={PS}>{l.reason}</div>}</td><td className="num">{inr(l.amount)}</td></tr>)}</tbody></table></div>
          {ls.length > 100 && <p className="mute" style={PS}>Showing 100 of {ls.length}. Download all lines for the full list.</p>}</div>; })
          : <p className="mute" style={{ margin: '8px 0 0' }}>Nothing is scheduled for this month.</p>}
      </div>
      <div className="card" style={{ marginTop: 14 }}><h3>Approved changes taking effect in {ymLabel(m)}</h3>
        {chg.length ? <div className="tw"><table><tbody>{chg.map(([c, a, b], i) => <tr key={i}><td style={{ width: '34%' }}>{nmPlain(c.name, c.employee_id)}</td><td>{a}</td><td>{b}</td></tr>)}</tbody></table></div> : <p className="mute" style={{ margin: 0 }}>None.</p>}</div>
      <div className="card" style={{ marginTop: 14 }}>
        <div className="row sp"><h3 style={{ margin: 0 }}>Waiting for approval</h3>{pend.length > 0 && <button className="btn sm" onClick={() => goAppr()}>Open Approvals</button>}</div>
        <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 10px' }}>These are not in any month yet. Each one joins the pay lines above the moment it is approved.</p>
        {pend.length ? <div className="tw"><table><tbody>{pend.map((r: any) => <tr key={r.id}><td style={{ width: '34%' }}>{nmPlain(r.name, r.employee_id)}</td><td>{wfName(r.wf)}</td><td>{subOf(r)}</td>
          <td><button className="btn sm" onClick={() => goAppr(r.id)}>Review</button></td></tr>)}</tbody></table></div> : <p className="mute" style={{ margin: 0 }}>Nothing waiting.</p>}
      </div>
    </>
  );
}

const PLAB: Record<string, string> = { mobile: 'Mobile', email: 'Email', address: 'Address', ecName: 'Emergency contact', ecRelation: 'Relation', ecMobile: 'Emergency mobile' };

/** Claims and changes (prototype vInbox): reimbursement claims, profile and bank changes, arrears with the arrear form, and the
 *  sub-tabs other pages supply (tax declarations, nominations) as `extra` [key, label, content, count?]. */
export function ClaimsChanges({ extra, arrearFor, initial }: { extra?: [string, string, ReactNode, number?][]; arrearFor?: string; initial?: string }) {
  const { can } = useAuth();
  const goAppr = useGoAppr();
  // initial: the sub-tab to open on ('claims' | 'changes' | 'arrears' | an extra key such as 'tax' or 'nom').
  const [t, setT] = useState(initial ?? (arrearFor ? 'arrears' : 'claims'));
  const d = useLoad(() => Promise.all(['claims', 'changes', 'arrears'].map((k) => api(`/pay-items/${k}`))), []);
  const ed = can('pay.claims', 2), ap = can('pay.approve', 2);
  const [cl, ch, ar] = d.data ?? [];
  const pend = (x: any, s = 'Pending') => (x?.items ?? []).filter((i: any) => i.status === s).length;
  const tabs: [string, string, number | undefined][] = [['claims', 'Reimbursement claims', cl && pend(cl)], ['changes', 'Profile and bank changes', ch && pend(ch)], ['arrears', 'Arrears', ar && pend(ar)],
    ...(extra ?? []).map(([k, l, , n]) => [k, l, n] as [string, string, number | undefined])];
  const ex = extra?.find((x) => x[0] === t);
  const review = (x: any, show: boolean) => x.status === 'Pending' && show ? <button className="btn sm primary" onClick={() => goAppr(x.id)}>Review in Approvals</button> : null;
  return (
    <>
      <div className="note" style={{ marginBottom: 14 }}>Approving claims, changes, arrears and advances now happens in <a href="/approvals" onClick={(e) => { e.preventDefault(); goAppr(); }}>Approvals</a>. This page is the record and where arrears are raised.</div>
      <div className="seg" style={{ marginBottom: 14 }}>{tabs.map(([k, l, n]) => <button key={k} className={t === k ? 'on' : ''} onClick={() => setT(k)}>{l}{n != null && <b>{n}</b>}</button>)}</div>
      {ex ? ex[2] : !d.data ? <Loading error={d.error} /> : <>
        {t === 'claims' && <div className="card"><p className="mute" style={{ margin: '0 0 10px', fontSize: 12.5 }}>Approved claims are added to the salary of the current open month.</p>
          <div className="tw"><table><thead><tr><th>Claim</th><th>Employee</th><th>Type</th><th>Date</th><th className="num">Amount</th><th>Note</th><th>Status</th><th></th></tr></thead><tbody>
            {cl.items.map((c: any) => <tr key={c.id}><td className="mono">{c.id}</td><td>{nm(c.name, c.employee_id)}</td><td>{c.payload.claimType}</td><td>{fmtD(c.payload.billDate)}</td><td className="num">{inr(c.payload.amount)}</td>
              <td className="ink2">{c.payload.note || ''} {c.payload.doc && <span className="pill n" style={{ cursor: 'pointer' }} onClick={() => openDoc(c.payload.doc).catch((e) => toast(e.message))}>Bill</span>}</td>
              <td>{stPill(c.status)}{c.lines?.[0] && <div className="mute" style={PS}>{ymLabel(c.lines[0].ym)}</div>}</td><td>{review(c, ed)}</td></tr>)}
            {!cl.items.length && <tr><td colSpan={8} className="mute">No claims yet.</td></tr>}
          </tbody></table></div></div>}
        {t === 'changes' && <div className="card"><p className="mute" style={{ margin: '0 0 10px', fontSize: 12.5 }}>A pending bank change puts that employee's salary on hold in the payroll run until it is verified.</p>
          <div className="tw"><table><thead><tr><th>Request</th><th>Employee</th><th>Change</th><th>Details</th><th>Status</th><th></th></tr></thead><tbody>
            {ch.items.map((p: any) => { const x = p.payload; return <tr key={p.id}><td className="mono">{p.id}</td><td>{nm(p.name, p.employee_id)}</td><td>{x.kind}</td>
              <td className="ink2" style={{ fontSize: 12.5 }}>{x.kind === 'Bank' ? `${x.bankName} · A/c ${x.accountNo} · IFSC ${x.ifsc}` : Object.keys(PLAB).filter((k) => x[k]).map((k) => `${PLAB[k]}: ${x[k]}`).join(' · ')}</td>
              <td>{stPill(p.status)}</td><td>{review(p, ed)}</td></tr>; })}
            {!ch.items.length && <tr><td colSpan={6} className="mute">No change requests.</td></tr>}
          </tbody></table></div></div>}
        {t === 'arrears' && <>
          {ed && <RaiseItem wf="p_arrear" preset={arrearFor} onDone={d.reload} />}
          <div className="card"><div className="tw"><table><thead><tr><th>Arrear</th><th>Employee</th><th>For</th><th>Pay in</th><th className="num">Amount</th><th>Reason</th><th>Status</th><th></th></tr></thead><tbody>
            {ar.items.map((a: any) => { const x = a.payload; return <tr key={a.id}><td className="mono">{a.id}</td><td>{nm(a.name, a.employee_id)}</td>
              <td>{x.forYm ? ymLabel(x.forYm) : '—'}{x.toYm && x.toYm !== x.forYm ? ` to ${ymLabel(x.toYm)}` : ''}</td><td>{x.payMonth ? ymLabel(x.payMonth) : a.lines?.[0] ? ymLabel(a.lines[0].ym) : '—'}</td>
              <td className="num">{inr(x.amount)}</td><td className="ink2">{[x.arrearType, x.slipText || x.note].filter(Boolean).join(': ')}</td><td>{stPill(stText(a.status))}</td><td>{review(a, ap)}</td></tr>; })}
            {!ar.items.length && <tr><td colSpan={8} className="mute">No arrears yet.</td></tr>}
          </tbody></table></div></div></>}
      </>}
    </>
  );
}

const TITLES = { p_adv: 'Give a salary advance', p_ded: 'Raise a deduction', p_arrear: 'Add an arrear' };

/** Raise an advance, deduction or arrear (through Approvals): the prototype's inline card, or a dialog when `onClose` is given. */
export function RaiseItem({ wf, onClose, onDone, preset }: { wf: 'p_adv' | 'p_ded' | 'p_arrear'; onClose?: () => void; onDone: () => void; preset?: string }) {
  const meta = useLoad(() => api('/pay-items/deductions').catch(() => null), []);
  const [emp, setEmp] = useState<EmpHit | null>(null);
  // Opened from a payroll row's "Add an arrear": start with that employee chosen.
  useEffect(() => {
    if (!preset) return;
    api(`/employees?q=${encodeURIComponent(preset)}&size=1`).then((d) => { const e = (d.rows as EmpHit[]).find((x) => x.id === preset); if (e) setEmp(e); }).catch(() => {});
  }, [preset]);
  const [p, setP] = useState<Record<string, any>>({});
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [n, setN] = useState(0);
  const open: string = meta.data?.openYm ?? '';
  const dedTypes: string[] = meta.data?.dedTypes ?? [], arrTypes: string[] = meta.data?.arrearTypes ?? [];
  const def: Record<string, any> = wf === 'p_adv' ? { payMonth: open, instalments: 3, startYm: open && nextYm(open) }
    : wf === 'p_ded' ? { dedType: dedTypes[0] ?? '', instalments: 1, startYm: open, heard: 'No' }
    : { forYm: open && nextYm(open, -1), payMonth: open, arrearType: arrTypes[0] ?? '' };
  const val = (k: string) => p[k] ?? def[k] ?? '';
  const set = (k: string) => (e: any) => setP({ ...p, [k]: e.target.value });
  const opt = (ms: string[]) => ms.map((m) => <option key={m} value={m}>{ymLabel(m)}</option>);
  const inst = (l: number[]) => l.map((x) => <option key={x} value={x}>{x} {x === 1 ? 'instalment' : 'instalments'}</option>);
  /** The prototype's checks before sending (advAdd, arAdd, dedErrors), worded the same. */
  const check = () => {
    const amt = +val('amount'), why = String(val(wf === 'p_arrear' ? 'slipText' : 'note')).trim();
    if (wf !== 'p_ded') return !emp || !(amt > 0 || (wf === 'p_arrear' && amt)) || !why ? 'Pick the employee, amount and reason' : '';
    if (!emp) return 'Choose an employee';
    if (emp.type === 'Contractual') return 'Contractual staff are paid without deductions. Recover this another way';
    if (!val('dedType')) return 'Choose the type of deduction';
    if (!(amt > 0)) return 'Enter the amount';
    if (why.length < 5) return 'Write what happened, with the date and the amount involved';
    if (val('dedType') !== (meta.data?.noHearingType ?? 'Excess payment recovery') && val('heard') !== 'Yes') return 'Under the Payment of Wages Act the employee must be given a chance to explain before this kind of deduction. Record that they were heard';
    return '';
  };
  const send = async (skip = '') => {
    const err = check();
    if (err || !emp) { toast(err || 'Choose an employee'); return; }
    setBusy(true);
    try {
      const payload: any = { ...def, ...p, amount: +val('amount'), instalments: +val('instalments') || undefined };
      if (wf === 'p_arrear') payload.toYm = payload.forYm;
      // Proof is kept with the request (owner kind 'approval'), so whoever raises deductions can attach it.
      if (wf === 'p_ded' && file) payload.doc = (await uploadDoc('approval', `${emp.id}/DED-${Date.now()}`, 'Deduction proof', file)).id;
      const r = await api('/approvals' + skip, { body: { wf, employeeId: emp.id, payload } });
      toast(skip ? `${r.id} approved: approval skipped` : `Sent for approval (${r.id})`); setEmp(null); setP({}); setFile(null); setN(n + 1); onDone();
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const empField = <div className="field"><span>{wf === 'p_adv' ? 'Employee (permanent staff)' : 'Employee'}</span><EmpSearch value={emp} onPick={setEmp} filter={wf === 'p_adv' ? '&type=Permanent&status=Active' : wf === 'p_arrear' ? '&status=Active' : ''} /></div>;
  const body = <>
    <div className="form" key={n}>
      {empField}
      {wf === 'p_adv' && <>
        <label className="field"><span>Amount (₹)</span><input type="number" min={1} value={val('amount')} onChange={set('amount')} /></label>
        <label className="field"><span>Paid with payroll of</span><select value={val('payMonth')} onChange={set('payMonth')}>{open && opt(months(open, 2))}</select></label>
        <label className="field"><span>Recover in how many instalments</span><select value={val('instalments')} onChange={set('instalments')}>{inst([1, 2, 3, 4, 5, 6, 8, 10, 12])}</select></label>
        <label className="field"><span>First recovery month</span><select value={val('startYm')} onChange={set('startYm')}>{open && opt(months(nextYm(open), 2))}</select></label>
        <label className="field full"><span>Reason</span><input type="text" value={val('note')} onChange={set('note')} placeholder="For example: medical emergency" /></label>
      </>}
      {wf === 'p_ded' && <>
        <label className="field"><span>Type</span><select value={val('dedType')} onChange={set('dedType')}>{dedTypes.map((r) => <option key={r}>{r}</option>)}</select></label>
        <label className="field"><span>Amount (₹)</span><input type="number" min={1} value={val('amount')} onChange={set('amount')} /></label>
        <label className="field"><span>Instalments</span><select value={val('instalments')} onChange={set('instalments')}>{inst([1, 2, 3, 4, 6])}</select></label>
        <label className="field"><span>First deduction in</span><select value={val('startYm')} onChange={set('startYm')}>{open && opt(months(open, 3))}</select></label>
        <label className="field"><span>Employee was heard</span><select value={val('heard')} onChange={set('heard')}><option>Yes</option><option>No</option></select></label>
        <label className="field full"><span>What happened</span><input type="text" value={val('note')} onChange={set('note')} placeholder="For example: cash short by ₹2,000 on night shift, 14 Oct, counted by two staff" /></label>
        <label className="field full"><span>Proof (optional)</span><input type="file" onChange={(e) => setFile(e.target.files?.[0] ?? null)} /></label>
      </>}
      {wf === 'p_arrear' && <>
        <label className="field"><span>Arrear for month</span><select value={val('forYm')} onChange={set('forYm')}>{open && opt(months(nextYm(open, -3), 3))}</select></label>
        <label className="field"><span>Pay in</span><select value={val('payMonth')} onChange={set('payMonth')}>{open && opt(months(open, 2))}</select></label>
        <label className="field"><span>Amount (₹)</span><input type="number" value={val('amount')} onChange={set('amount')} /></label>
        <label className="field"><span>Reason type</span><select value={val('arrearType')} onChange={set('arrearType')}>{arrTypes.map((r) => <option key={r}>{r}</option>)}</select></label>
        <label className="field full"><span>Details shown on the payslip</span><input type="text" value={val('slipText')} onChange={set('slipText')} placeholder="For example: revision effective 1 August, 3 months" /></label>
      </>}
    </div>
    <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={busy} onClick={() => send()}>Send for approval</button><SkipApprovalButton wf={wf} subject={emp?.id} disabled={busy || !emp} onSkip={(q) => send(q)} /></div>
    {wf !== 'p_ded' && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>{wf === 'p_adv' ? 'Equal instalments, with any rounding in the last one. Once approved, the advance is paid in the chosen month and every instalment is deducted automatically.'
      : 'PF and ESI are not applied to arrears. A user who can approve payroll approves each arrear.'}</p>}
  </>;
  if (onClose) return <Modal wide onClose={onClose}><div className="row sp"><h2>{TITLES[wf]}</h2><button className="btn sm" onClick={onClose}>Close</button></div><div style={{ marginTop: 12 }}>{body}</div></Modal>;
  return <div className="card" style={{ marginBottom: 14 }}><h3>{TITLES[wf]}</h3>{body}</div>;
}

/** Employee dues (prototype vAst): assets, uniform, inventory, advances, salary holds, deductions and setup, by role. Tabs other pages
 *  pass in `extra` are added at the end (an 'inv' entry is the Inventory tab, which is now built in). */
export function Dues({ extra }: { extra?: [string, string, JSX.Element][] }) {
  const { can, me } = useAuth();
  const role = me?.ctx.role ?? '';
  const inv = can('emp.assets'), admin = role === 'HR Admin', pay = can('pay.claims', 2) || admin;
  const more = (extra ?? []).filter((x) => x[0] !== 'inv');
  // Payroll's own desk (claims at Edit, not HR Admin) opens on Advances: it comes first for them.
  const advFirst = can('pay.claims', 2) && !admin;
  const invTabs: [string, string][] = inv ? [['assets', 'Assets'], ['uni', 'Uniform'], ['sizes', 'Uniform sizes'], ['inv', 'Inventory']] : [];
  const tabs: [string, string][] = [...(advFirst ? [['adv', 'Advances'], ...invTabs] : [...invTabs, ['adv', 'Advances']]) as [string, string][],
    ...(pay ? [['hold', 'Salary holds'], ['ded', 'Deductions']] as [string, string][] : []), ...(admin && inv ? [['setup', 'Setup']] as [string, string][] : []), ...more.map(([k, l]) => [k, l] as [string, string])];
  // ?tab=adv (and the other keys) open that tab.
  const [sp] = useSearchParams();
  const [pick, setT] = useState(sp.get('tab') ?? '');
  const t = tabs.some((x) => x[0] === pick) ? pick : tabs[0][0];
  const ex = more.find((x) => x[0] === t);
  return (
    <>
      <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${t === k ? 'on' : ''}`} onClick={() => setT(k)}>{l}</button>)}</div>
      {ex ? ex[2] : t === 'assets' ? <Assets /> : t === 'uni' ? <Uniform /> : t === 'sizes' ? <UniformSizes /> : t === 'inv' ? <Inventory /> : t === 'adv' ? <Advances /> : t === 'hold' ? <Holds /> : t === 'ded' ? <Deductions /> : <InvSetup />}
    </>
  );
}

function Advances() {
  const d = useLoad(() => api('/pay-items/advances'), []);
  const { can, me } = useAuth();
  const nav = useNavigate();
  const goAppr = useGoAppr();
  const [rec, setRec] = useState<any>(null);
  const role = me?.ctx.role ?? '';
  const ed = can('pay.claims', 2) || can('emp.assets', 2), ap = can('pay.approve', 2);
  const opening = can('pay.claims', 2) || role === 'HR Admin';
  const act = async (id: string, a: string, body?: any) => { try { const r = await api(`/advances/${id}/${a}`, { body: body ?? {} }); toast(r.message); setRec(null); d.reload(); } catch (e: any) { toast(e.message); } };
  const waiting = (d.data?.items ?? []).filter((a: any) => a.status === 'Pending').length;
  // Where requests waiting for approval are decided: Payroll's own tab, or the Requests inbox for other roles.
  const apprTab = role === 'Payroll' ? 'Approvals › Payroll approvals' : 'Approvals › Requests';
  return <>
    {can('pay.claims', 2) && role !== 'HR Admin' && <div className="note" style={{ marginBottom: 14 }} data-testid="adv-waiting">
      {waiting ? <><b>{waiting} advance request{waiting === 1 ? ' is' : 's are'} waiting for approval.</b> </> : 'Employees ask for advances from My pay › Salary advance. '}
      Requests are approved in <a href="#" onClick={(e) => { e.preventDefault(); nav(role === 'Payroll' ? '/approvals?tab=pay' : '/approvals'); }}>{apprTab}</a>, where Payroll can change the instalments and months at its step before approving.</div>}
    {ed && <RaiseItem wf="p_adv" onDone={d.reload} />}
    {opening && <OpeningAdvances onDone={d.reload} />}
    {!d.data ? <Loading error={d.error} /> : <div className="card">
      <div className="tw"><table><thead><tr><th>Advance</th><th>Employee</th><th className="num">Amount</th><th>Recovery schedule</th><th className="num">Balance</th><th>Status</th><th></th></tr></thead><tbody>
        {d.data.items.map((a: any) => {
          const x = a.payload, open = d.data.openYm, ls = (a.lines || []).filter((l: any) => l.kind === 'ded' && !l.void), st = a.status === 'Approved' && !a.balance ? 'Closed' : stText(a.status);
          return <tr key={a.id} style={{ verticalAlign: 'top' }}>
            <td className="mono">{a.id}{x.opening && <div><Pill k="info">Opening balance</Pill></div>}<div className="mute" style={PS}>{x.note}</div></td><td>{nm(a.name, a.employee_id)}</td>
            <td className="num">{inr(x.amount)}{x.opening ? <div className="mute" style={PS}>given {String(x.advanceDate ?? '').length === 7 ? ymLabel(x.advanceDate) : fmtD(x.advanceDate)}, {inr(x.balance)} left at go-live</div> : x.payMonth && <div className="mute" style={PS}>paid {ymLabel(x.payMonth)}</div>}</td>
            <td style={{ fontSize: 12.5 }}>{a.status === 'Approved' ? ls.map((l: any) => <div key={l.id}>{ymLabel(l.ym)}: {inr(l.amount)} {l.ym < open && <Pill k="ok">Recovered</Pill>}</div>)
              : `${x.instalments} × ${inr(Math.round(x.amount / x.instalments))} from ${ymLabel(x.startYm || nextYm(x.payMonth || open))}`}</td>
            <td className="num">{a.status === 'Approved' ? inr(a.balance) : '—'}</td><td>{stPill(st, true)}</td>
            <td>{a.status === 'Pending' && ap ? <button className="btn sm primary" onClick={() => goAppr(a.id)}>Review in Approvals</button>
              : a.status === 'Approved' && a.balance > 0 && ed ? <><button className="btn sm" onClick={() => act(a.id, 'skip')}>Skip next</button> <button className="btn sm" onClick={() => setRec(a)}>Recover balance</button></> : null}</td></tr>;
        })}
        {!d.data.items.length && <tr><td colSpan={7} className="mute">No advances yet.</td></tr>}
      </tbody></table></div>
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>“Skip next” moves the next instalment to the end of the schedule. “Recover balance” collects what is left in one month. Both keep a record; nothing is deleted. All scheduled recoveries are also listed month by month in Payroll, Upcoming pay lines.</p>
    </div>}
    {rec && <RecoverModal a={rec} open={d.data.openYm} onClose={() => setRec(null)} onGo={(ym) => act(rec.id, 'recover', { ym })} />}
  </>;
}

type OpenRow = { line: number; employeeId: string; name: string; advanceDate: string; original: number; balance: number; monthly: number; startYm: string; reference: string;
  months: number; last: number; outcome: string; reason?: string; id?: string };
const OPEN_OUT: Record<string, [string, 'ok' | 'warn' | 'bad' | 'info' | 'n']> = { add: ['Will add', 'ok'], skip: ['Skipped', 'n'], added: ['Added', 'ok'], skipped: ['Skipped', 'n'], failed: ['Failed', 'bad'] };

/** Opening advances (advances given before the HRMS went live and still being recovered): download the template, upload
 *  it, check the preview, confirm. Nothing changes before Confirm, which checks every row again. */
function OpeningAdvances({ onDone }: { onDone: () => void }) {
  const nav = useNavigate();
  const { can } = useAuth();
  const [file, setFile] = useState<File | null>(null);
  const [preview, setPreview] = useState<any>(null);
  const [result, setResult] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const upload = async (f?: File) => {
    if (!f) return;
    if (!/\.xlsx$/i.test(f.name)) { toast('Upload the Excel file (.xlsx) made from Download template'); return; }
    if (f.size > 5 * 1024 * 1024) { toast('The file must be 5 MB or smaller'); return; }
    setBusy(true);
    try { setPreview(await postForm('/advances/opening/preview', {}, f)); setFile(f); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const confirm = async () => {
    if (!file) return;
    setBusy(true);
    try { const r = await postForm('/advances/opening/apply', {}, file); setPreview(null); setFile(null); setResult(r); onDone(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const close = () => { setPreview(null); setFile(null); };
  return <>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ flexWrap: 'wrap', gap: 10 }}>
        <div><h3 style={{ margin: 0 }}>Opening advances</h3>
          <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>Advances given before the HRMS went live and still being recovered. Fill the template, upload it and check the preview; nothing changes until you confirm. Only the balance is deducted from salary.</div>
          {can('bulk.update') && <div style={{ fontSize: 12.5, marginTop: 4 }} data-testid="adv-bulk-link">Know only the months? Use <a href="#" onClick={(e) => { e.preventDefault(); nav('/bulk?tpl=openadv'); }}>Bulk update › Opening advances</a>: give the amount, the given, start and closing months and the monthly deduction, and it works out what was recovered before the HRMS.</div>}</div>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <button className="btn sm" onClick={() => download('/advances/opening/template', 'opening-advances.xlsx').catch((e) => toast(e.message))}>Download template</button>
          <label className="btn sm primary" aria-disabled={busy}>{busy && !preview ? 'Reading…' : 'Upload opening advances'}<input type="file" accept=".xlsx" hidden onChange={(e) => { upload(e.target.files?.[0]); e.target.value = ''; }} /></label>
        </div>
      </div>
    </div>
    {preview && <Modal wide onClose={close}>
      <h2>Check before you confirm</h2>
      <p className="mute" style={{ fontSize: 12.5, marginTop: 0 }}>{file?.name} · {preview.total} row{preview.total === 1 ? '' : 's'}. Nothing has changed yet. Every row is checked again when you confirm.</p>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap', margin: '6px 0 12px' }}><Pill k="ok">Will add: {preview.add}</Pill><Pill>Skipped: {preview.skip}</Pill><Pill k="info">Balance to recover: {inr(preview.balance)}</Pill></div>
      <OpeningTable rows={preview.rows} />
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end', gap: 8 }}>
        <button className="btn" onClick={close}>Cancel</button>
        <button className="btn primary" disabled={busy || !preview.add} onClick={confirm}>{busy ? 'Adding…' : preview.add ? `Confirm: add ${preview.add} opening advance${preview.add === 1 ? '' : 's'}` : 'Nothing to add'}</button>
      </div>
    </Modal>}
    {result && <Modal wide onClose={() => setResult(null)}>
      <h2>Done</h2>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap', margin: '6px 0 12px' }}><Pill k="ok">Added: {result.added}</Pill><Pill>Skipped: {result.skipped}</Pill>{result.failed > 0 && <Pill k="bad">Failed: {result.failed}</Pill>}</div>
      <OpeningTable rows={result.rows} />
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn primary" onClick={() => setResult(null)}>Close</button></div>
    </Modal>}
  </>;
}

function OpeningTable({ rows }: { rows: OpenRow[] }) {
  return <div className="tw" style={{ maxHeight: 380, overflow: 'auto' }}><table>
    <thead><tr><th>Row</th><th>Employee</th><th className="num">Balance</th><th>Recovery</th><th>What happens</th><th>Why</th></tr></thead>
    <tbody>{rows.map((x) => <tr key={x.line}>
      <td className="mute">{x.line}</td>
      <td><b>{x.name || x.employeeId || '—'}</b><div className="mute" style={PS}>{[x.employeeId, x.reference].filter(Boolean).join(' · ')}</div></td>
      <td className="num">{x.balance ? inr(x.balance) : '—'}{x.original ? <div className="mute" style={PS}>of {inr(x.original)}</div> : null}</td>
      <td style={{ fontSize: 12.5 }}>{x.months ? `${x.months > 1 ? (x.last === x.monthly ? `${x.months} × ${inr(x.monthly)}` : `${x.months - 1} × ${inr(x.monthly)}, then ${inr(x.last)}`) : inr(x.last)} from ${ymLabel(x.startYm)}` : '—'}</td>
      <td><Pill k={OPEN_OUT[x.outcome]?.[1] ?? 'n'}>{OPEN_OUT[x.outcome]?.[0] ?? x.outcome}</Pill>{x.id && <div className="mono mute" style={PS}>{x.id}</div>}</td>
      <td style={{ fontSize: 12.5 }}>{x.reason}</td>
    </tr>)}</tbody></table></div>;
}

function RecoverModal({ a, open, onClose, onGo }: { a: any; open: string; onClose: () => void; onGo: (ym: string) => void }) {
  const [ym, setYm] = useState(open);
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Recover the balance of {a.id}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="ink2">Balance {inr(a.balance)}. All remaining instalments are replaced by one deduction.</p>
    <label className="field"><span>Recover in</span><select value={ym} onChange={(e) => setYm(e.target.value)}>{months(open, 6).map((m) => <option key={m} value={m}>{ymLabel(m)}</option>)}</select></label>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={() => onGo(ym)}>Recover</button></div>
  </Modal>;
}

function Holds() {
  const d = useLoad(() => api('/holds'), []);
  const [win, setWin] = useState(10);
  const ab = useLoad(() => (d.data?.canEdit ? api<any[]>(`/holds/absent?days=${win}`) : Promise.resolve([] as any[])), [win, !!d.data?.canEdit]);
  const [rel, setRel] = useState<any>(null);
  const [emp, setEmp] = useState<EmpHit | null>(null);
  const [absent, setAbsent] = useState<{ days: number; win: number } | null>(null);
  const [f, setF] = useState<Record<string, string>>({});
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, ed = x.canEdit;
  const reasons: string[] = x.reasons.map((r: any) => r.name);
  const absentReason: string = x.absentReason ?? 'Absent without notice';
  const reason = f.reason ?? (absent && reasons.includes(absentReason) ? absentReason : reasons[0] ?? '');
  const sus: any[] = (ab.data ?? []).slice(0, 12);
  /** "Hold salary" on someone not at work (prototype holdFor): pick them in the form below. */
  const holdFor = (a: any) => { setEmp({ id: a.employeeId, name: a.name, department: a.department, location: a.location }); setAbsent({ days: a.days, win }); setF({}); };
  const pick = (e: EmpHit | null) => { setEmp(e); setAbsent(null); };
  const save = async () => {
    if (!emp) { toast('Choose the employee'); return; }
    if ((f.note ?? '').trim().length < 5) { toast('Write why the salary is held'); return; }
    try { await api('/holds', { body: { employeeId: emp.id, reason, note: (f.note ?? '').trim(), fromYm: f.fromYm || x.openYm } }); toast('Salary put on hold'); setEmp(null); setAbsent(null); setF({}); d.reload(); ab.reload(); } catch (e: any) { toast(e.message); }
  };
  const release = async () => {
    if ((f.rel ?? '').trim().length < 3) { toast('Enter the reason for releasing'); return; }
    try { await api(`/holds/${rel.id}/release`, { body: { note: (f.rel ?? '').trim() } }); toast('Hold released'); setRel(null); setF({}); d.reload(); } catch (e: any) { toast(e.message); }
  };
  return <>
    <div className="note" style={{ marginBottom: 14 }}><b>How a hold works.</b> The employee stays in the payroll run, but the net pay is left out of the bank file until Payroll releases it. Held months are added to the pay lines when released. An employee marked inactive keeps the hold, and the full and final shows the held amount.</div>
    {ed && <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ flexWrap: 'wrap' }}><h3 style={{ margin: 0 }}>Not at work without notice</h3>
        <select value={win} onChange={(e) => setWin(+e.target.value)}><option value={5}>Last 5 days</option><option value={10}>Last 10 days</option><option value={15}>Last 15 days</option></select></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 10px' }}>Employees with no punch, no leave and no regularization on at least {Math.min(win, 5)} of the last {win} days.</p>
      {!ab.data ? <Loading error={ab.error} /> : sus.length ? <div className="tw"><table><tbody>
        {sus.map((a) => <tr key={a.employeeId}><td>{nm(a.name, a.employeeId)}<div className="mute" style={PS}>{a.department} · {a.location}</div></td>
          <td><Pill k="bad">{a.days} days absent</Pill></td>
          <td style={{ textAlign: 'right' }}><button className="btn sm primary" onClick={() => holdFor(a)}>Hold salary</button></td></tr>)}
      </tbody></table></div> : <p className="mute" style={{ margin: 0 }}>Nobody matches.</p>}
    </div>}
    {ed && <div className="card" style={{ marginBottom: 14 }}><h3>Hold a salary</h3>
      <div className="form">
        <div className="field"><span>Employee</span><EmpSearch value={emp} onPick={pick} /></div>
        <label className="field"><span>Reason</span><select value={reason} onChange={(e) => setF({ ...f, reason: e.target.value })}>{reasons.map((r) => <option key={r}>{r}</option>)}</select></label>
        <label className="field"><span>Hold from payroll of</span><select value={f.fromYm || x.openYm} onChange={(e) => setF({ ...f, fromYm: e.target.value })}>{months(x.openYm, 2).map((m) => <option key={m} value={m}>{ymLabel(m)}</option>)}</select></label>
        <label className="field full"><span>Details</span><input type="text" value={f.note ?? ''} onChange={(e) => setF({ ...f, note: e.target.value })}
          placeholder={absent ? `Absent ${absent.days} of the last ${absent.win} days. Manager informed on ...` : 'For example: absent since 12 Oct, not reachable'} /></label>
      </div>
      <div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={save}>Put on hold</button></div></div>}
    <div className="card"><div className="tw"><table><thead><tr><th>Hold</th><th>Employee</th><th>Reason</th><th>From</th><th className="num">Held so far</th><th>Status</th><th></th></tr></thead><tbody>
      {x.holds.map((h: any) => { const paid = (h.paid || []).reduce((a: number, p: any) => a + p.amount, 0); return <tr key={h.id} style={{ verticalAlign: 'top' }}>
        <td className="mono">{h.id}<div className="mute" style={PS}>{h.by_name}, {h.at}</div></td><td>{nm(h.name, h.employee_id)}{h.emp_status === 'Inactive' && <div><Pill>Inactive</Pill></div>}</td>
        <td>{h.reason}<div className="mute" style={PS}>{h.note}</div></td><td>{ymLabel(h.from_ym)}{h.to_ym && <div className="mute" style={PS}>{h.to_ym === h.from_ym ? 'that month only' : `to ${ymLabel(h.to_ym)}`}</div>}</td>
        <td className="num">{h.status === 'Active' ? inr(h.held) : paid ? `${inr(paid)} paid` : '—'}</td>
        <td><Pill k={h.status === 'Active' ? 'bad' : 'ok'}>{h.status}</Pill>{h.status === 'Released' && <div className="mute" style={PS}>{h.released_by}, {h.released_at}: {h.release_note}</div>}</td>
        <td>{h.status === 'Active' && ed && <button className="btn sm" onClick={() => setRel(h)}>Release</button>}</td></tr>; })}
      {!x.holds.length && <tr><td colSpan={7} className="mute">No salary holds yet.</td></tr>}
    </tbody></table></div></div>
    {rel && <Modal onClose={() => setRel(null)}><h2>Release salary hold {rel.id}?</h2>
      <p className="ink2">Salary held in earlier payroll months is added to the open month's pay lines. The open month is paid with the normal run.</p>
      <label className="field"><span>Reason for releasing</span><input type="text" value={f.rel ?? ''} onChange={(e) => setF({ ...f, rel: e.target.value })} /></label>
      <div className="row" style={{ marginTop: 14 }}><button className="btn primary" onClick={release}>Release</button><button className="btn" onClick={() => setRel(null)}>Cancel</button></div></Modal>}
  </>;
}

function Deductions() {
  const d = useLoad(() => api('/pay-items/deductions'), []);
  const { can } = useAuth();
  const goAppr = useGoAppr();
  const ed = can('pay.claims', 2) || can('emp.assets', 2), ap = can('pay.approve', 2), canWaive = can('pay.claims', 2);
  const [wv, setWv] = useState<any>(null);
  const [why, setWhy] = useState('');
  const cancel = async (id: string) => { try { await api(`/approvals/${id}/cancel`, { body: {} }); toast('Cancelled'); d.reload(); } catch (e: any) { toast(e.message); } };
  /** Waive what is left (prototype dedWaive): instalments not yet deducted are voided, with the reason. */
  const waive = async () => {
    if (why.trim().length < 3) { toast('Enter the reason'); return; }
    try { await api(`/pay-items/deductions/${wv.id}/waive`, { body: { note: why.trim() } }); toast('Balance waived'); setWv(null); setWhy(''); d.reload(); } catch (e: any) { toast(e.message); }
  };
  return <>
    <div className="note" style={{ marginBottom: 14 }}><b>Pushing a deduction.</b> Raise it here with the type, the amount, what happened and proof. It goes to Approvals, and once approved every instalment is deducted from salary and shown on the payslip. For an employee who has left, the amount is taken in the full and final.</div>
    {ed && <RaiseItem wf="p_ded" onDone={d.reload} />}
    {!d.data ? <Loading error={d.error} /> : <div className="card"><div className="tw"><table><thead><tr><th>Deduction</th><th>Employee</th><th>Type</th><th className="num">Amount</th><th>Schedule</th><th>Status</th><th></th></tr></thead><tbody>
      {d.data.items.map((x: any) => {
        const p = x.payload, open = d.data.openYm, ls = x.lines || [], st = x.status === 'Approved' && !x.balance ? (x.waived != null ? 'Waived' : 'Closed') : stText(x.status);
        const by = x.raised_by_name || x.first_action;
        return <tr key={x.id} style={{ verticalAlign: 'top' }}>
          <td className="mono">{x.id}<div className="mute" style={PS}>{by ? `${by}, ` : ''}{x.created}</div></td><td>{nm(x.name, x.employee_id)}</td>
          <td>{p.dedType}<div className="mute" style={{ fontSize: 12, maxWidth: 260 }}>{p.note}{p.doc && <> · <span className="pill n" style={{ cursor: 'pointer' }} onClick={() => openDoc(p.doc).catch((e) => toast(e.message))}>Proof</span></>}</div></td>
          <td className="num">{inr(p.amount)}</td>
          <td style={{ fontSize: 12.5 }}>{x.status === 'Approved' ? ls.map((l: any) => <div key={l.id}>{ymLabel(l.ym)}: {inr(l.amount)} {l.void ? <Pill>Waived</Pill> : l.ym < open ? <Pill k="ok">Deducted</Pill> : null}</div>)
            : `${p.instalments || 1} × ${inr(Math.round(p.amount / (p.instalments || 1)))} from ${ymLabel(p.startYm || open)}`}
            {x.waived != null && <div className="mute" style={PS}>Balance {inr(x.waived)} waived: {x.waive_note}</div>}</td>
          <td>{stPill(st, true)}</td>
          <td>{x.status === 'Pending' ? <>{ap && <button className="btn sm primary" onClick={() => goAppr(x.id)}>Review in Approvals</button>} {ed && <button className="btn sm" onClick={() => cancel(x.id)}>Cancel</button>}</>
            : x.status === 'Approved' && x.balance > 0 && canWaive ? <button className="btn sm" onClick={() => { setWv(x); setWhy(''); }}>Waive balance</button> : null}</td></tr>;
      })}
      {!d.data.items.length && <tr><td colSpan={7} className="mute">No deductions raised yet.</td></tr>}
    </tbody></table></div></div>}
    {wv && <Modal onClose={() => setWv(null)}><h2>Waive the balance of {wv.id}?</h2>
      <p className="ink2">Instalments not yet deducted are removed. Instalments already deducted stay as they are.</p>
      <label className="field"><span>Reason</span><input type="text" value={why} onChange={(e) => setWhy(e.target.value)} /></label>
      <div className="row" style={{ marginTop: 14 }}><button className="btn primary" onClick={waive}>Waive balance</button><button className="btn" onClick={() => setWv(null)}>Cancel</button></div></Modal>}
  </>;
}

/** Payslip design (prototype vSlipSetup): company, logo and accent, statutory details, footer, and a live preview. */
export function PayslipSetup() {
  const { me } = useAuth();
  const admin = me?.ctx.role === 'HR Admin';
  const d = useLoad(() => api('/payslip-setup'), []);
  const pv = useLoad(() => api<SlipView>('/payslip-preview').catch(() => ({ none: true } as any)), []);
  const [v, setV] = useState<any>(null);
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, co = x.company;
  const saved = { name: co.LegalName || co.Name, addr: co.Address, setup: x.setup, logo: x.logo, cin: co.CIN, pan: co.PAN, tan: co.TAN, pfCode: co.PFCode, esiCode: co.ESICode };
  const f = v ?? saved;
  const ed = x.canEdit;
  /** Each change is saved as it is made, as in the prototype (no Save button): text on leaving the field, the rest at once. */
  const save = async (n: any) => {
    if (JSON.stringify(n) === JSON.stringify(saved)) return;
    try {
      if (admin && (n.name !== saved.name || n.addr !== saved.addr)) {
        const c = await api('/settings/company');
        await api('/settings/company', { method: 'PUT', body: { ...c, legalName: n.name, address: n.addr, pan: n.pan, tan: n.tan, pfCode: n.pfCode, esiCode: n.esiCode } });
      }
      await api('/payslip-setup', { method: 'PUT', body: { setup: n.setup, logo: n.logo ?? '', cin: n.cin, pan: n.pan, tan: n.tan, pfCode: n.pfCode, esiCode: n.esiCode, cutoff: x.cutoff, firstYm: x.firstYm ?? '' } });
      toast('Saved'); d.reload(); pv.reload();
    } catch (e: any) { toast(e.message); setV(null); }
  };
  const set = (k: string, val: any) => setV({ ...f, [k]: val });
  const setS = (k: string, val: any) => setV({ ...f, setup: { ...f.setup, [k]: val } });
  const putS = (k: string, val: any) => { const n = { ...f, setup: { ...f.setup, [k]: val } }; setV(n); save(n); };
  const logo = async (file?: File) => {
    if (!file) return;
    if (file.size > 500 * 1024) { toast('Logo is above 500 KB'); return; }
    try { const r = await uploadDoc('company', 'company', 'Payslip logo', file); const n = { ...f, logo: r.id }; setV(n); save(n); } catch (e: any) { toast(e.message); }
  };
  const fld = (k: string, l: string, full?: boolean, off?: boolean) => <label className={`field${full ? ' full' : ''}`}><span>{l}</span><input type="text" disabled={!ed || off} value={f[k] ?? ''} onChange={(e) => set(k, e.target.value)} onBlur={() => save(f)} title={off ? 'HR Admin changes this in Masters, Company' : undefined} /></label>;
  const preview = pv.data && !(pv.data as any).none && { ...pv.data, setup: f.setup, logo: f.logo, snap: { ...pv.data.snap, company: { ...pv.data.snap.company, legalName: f.name, address: f.addr, cin: f.cin, pan: f.pan, tan: f.tan, pf: f.pfCode, esi: f.esiCode } } };
  return (
    <div className="grid g2" style={{ alignItems: 'start' }}>
      <div>
        <div className="card" style={{ marginBottom: 14 }}><h3>Company on the payslip</h3>
          <div className="form">{fld('name', 'Company name', true, !admin)}{fld('addr', 'Address', true, !admin)}
            <label className="field"><span>Email</span><input type="text" disabled={!ed} value={f.setup.email ?? ''} onChange={(e) => setS('email', e.target.value)} onBlur={() => save(f)} /></label>
            <label className="field"><span>Phone</span><input type="text" disabled={!ed} value={f.setup.phone ?? ''} onChange={(e) => setS('phone', e.target.value)} onBlur={() => save(f)} /></label>
            <label className="field"><span>Website</span><input type="text" disabled={!ed} value={f.setup.website ?? ''} onChange={(e) => setS('website', e.target.value)} onBlur={() => save(f)} /></label></div>
          <div className="row" style={{ marginTop: 12, gap: 14, flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <div><LogoImg id={f.logo} />{ed && <><label className="btn">{f.logo ? 'Change logo' : 'Upload logo'}<input type="file" accept="image/png,image/jpeg" hidden onChange={(e) => logo(e.target.files?.[0])} /></label>{f.logo && <> <button className="btn" onClick={() => { const n = { ...f, logo: null }; setV(n); save(n); }}>Remove</button></>}</>}</div>
            <label className="field" style={{ margin: 0 }}><span>Accent colour</span><input type="color" disabled={!ed} value={f.setup.accent} onChange={(e) => setS('accent', e.target.value)} onBlur={() => save(f)} style={{ height: 38, width: 70, padding: 2 }} /></label>
          </div>
          <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Logo up to 500 KB. A wide logo on a white background works best.</p></div>
        <div className="card" style={{ marginBottom: 14 }}><h3>Statutory details</h3>
          <div className="form">{fld('cin', 'CIN')}{fld('pan', 'Company PAN')}{fld('tan', 'TAN')}{fld('pfCode', 'PF establishment code')}{fld('esiCode', 'ESI code')}</div>
          <div className="row" style={{ marginTop: 12, gap: 18, flexWrap: 'wrap' }}><label className="chkl"><input type="checkbox" disabled={!ed} checked={f.setup.showIds !== false} onChange={(e) => putS('showIds', e.target.checked)} />Show these on the payslip</label>
            <label className="chkl"><input type="checkbox" disabled={!ed} checked={!!f.setup.showEmployer} onChange={(e) => putS('showEmployer', e.target.checked)} />Show employer contributions</label></div></div>
        <div className="card"><h3>Footer note</h3>
          <label className="field"><span>Printed at the bottom of every payslip</span><textarea rows={2} disabled={!ed} value={f.setup.footer} onChange={(e) => setS('footer', e.target.value)} onBlur={() => save(f)} /></label>
          <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Changes apply to every payslip, including ones already published, because the layout is applied when it is opened.</p></div>
      </div>
      <div>{preview ? <><div className="mute" style={{ fontSize: 12.5, marginBottom: 8 }}>Preview with {preview.snap.emp?.name}, {preview.label}</div><Payslip v={preview} /></>
        : !pv.data ? <Loading error={pv.error} /> : <p className="mute">No permanent employee to preview a payslip for.</p>}</div>
    </div>
  );
}

function LogoImg({ id }: { id?: string | null }) {
  const url = useDocUrl(id);
  return id && url ? <img src={url} alt="Logo" style={{ maxHeight: 54, maxWidth: 160, display: 'block', marginBottom: 6 }} />
    : <div className="mute" style={{ fontSize: 12.5, marginBottom: 6 }}>No logo yet. The first letter of the company name is used.</div>;
}

/** My payslips (prototype vEss 'slips'): every published month, viewed or downloaded. */
export function MyPayslips({ employeeId }: { employeeId?: string }) {
  const d = useLoad(() => api(`/payslips${employeeId ? `?employeeId=${employeeId}` : ''}`), [employeeId]);
  const [open, setOpen] = useState('');
  const [dl, setDl] = useState('');
  const me = useAuth().me;
  const id = employeeId || me?.ctx.employeeId || '';
  const slip = useLoad(() => (open ? api<SlipView>(`/payslips/${open}/${id}`) : Promise.resolve(null)), [open, id]);
  const box = useRef<HTMLDivElement>(null);
  if (!d.data) return <Loading error={d.error} />;
  if (!d.data.docs) return <div className="note">{d.data.type} staff do not receive a payslip.</div>;
  const name = (ym: string) => `payslip-${ym}-${id}.html`;
  return (
    <div className="card">
      <div className="tw"><table><thead><tr><th>Month</th><th className="num">Net pay</th><th></th></tr></thead><tbody>
        {d.data.slips.map((s: any) => <tr key={s.ym}><td><b>{ymLabel(s.ym)}</b></td><td className="num">{inr(s.net)}</td>
          <td><button className="btn sm" onClick={() => setOpen(s.ym)}>View</button> <button className="btn sm" disabled={dl === s.ym} onClick={() => setDl(s.ym)}>Download</button></td></tr>)}
        {!d.data.slips.length && <tr><td colSpan={3} className="mute">No payslips yet.</td></tr>}
      </tbody></table></div>
      {open && slip.data && <Modal wide onClose={() => setOpen('')}>
        <div className="row sp"><h2>Payslip, {slip.data.label}</h2><div className="row"><button className="btn" onClick={() => box.current && saveSlipHtml(box.current, name(open), `Payslip ${slip.data!.label} ${id}`)}>Download</button><button className="btn sm" onClick={() => setOpen('')}>Close</button></div></div>
        <div ref={box} style={{ marginTop: 12 }}><Payslip v={slip.data} /></div>
      </Modal>}
      {dl && <SlipFile ym={dl} id={id} name={name(dl)} onDone={() => setDl('')} />}
    </div>
  );
}

/** Renders one payslip off screen and saves it as an HTML file (the list's Download). */
function SlipFile({ ym, id, name, onDone }: { ym: string; id: string; name: string; onDone: () => void }) {
  const s = useLoad(() => api<SlipView>(`/payslips/${ym}/${id}`), [ym, id]);
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (s.error) { toast(s.error); onDone(); return; }
    if (!s.data) return;
    let n = 0;
    const t = setInterval(() => {
      const el = box.current;
      if (el && (!s.data!.logo || el.querySelector('img') || ++n > 20)) { clearInterval(t); saveSlipHtml(el, name, `Payslip ${s.data!.label} ${id}`).finally(onDone); }
    }, 100);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.data, s.error]);
  return s.data ? <div ref={box} aria-hidden="true" style={{ position: 'fixed', left: -10000, top: 0, width: 820 }}><Payslip v={s.data} /></div> : null;
}
