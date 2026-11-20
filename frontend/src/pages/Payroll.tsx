import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { useAuth } from '../auth';
import { api, fmtD, inr, today } from '../api';
import { Field, Loading, Pill, toast, useLoad, usePage } from '../components/ui';
import Salary from './Salary';
import Pending from './Pending';
import { PayrollRun } from './PayrollRun';
import { ClaimsChanges, PayslipSetup, Upcoming } from './PayItems';
import { TaxAdmin, TaxDeclarations } from './Tax';
import { NominationsAdmin } from './Nominations';
import Statutory from './Statutory';
import { FnfAdmin } from './Separation';
import VariablePay from './VariablePay';

type T = 'calc' | 'run' | 'fwd' | 'claims' | 'var' | 'fnf' | 'files' | 'tax' | 'slip' | 'map' | 'setup';
// Month close is gone: its lock, cycle and reminders are the checklist at the top of Payroll run, which comes after
// the tabs that feed it (pay lines, claims and changes, full and final).
const TABS: [T, string][] = [['calc', 'Salary calculator'], ['fwd', 'Upcoming pay lines'], ['claims', 'Claims and changes'], ['var', 'Variable pay'], ['fnf', 'Full and final'], ['run', 'Payroll run'],
  ['files', 'Statutory and insurance files'], ['tax', 'Tax and Form 16'], ['slip', 'Payslip design'], ['map', 'Structure mapping'], ['setup', 'Pay schedule and components']];
const BASIC: T[] = ['calc', 'fwd', 'claims', 'var', 'fnf', 'run', 'files', 'tax'];

/** Payroll (prototype vPay): the last three tabs are for people who can configure payroll.
 *  Deep links: ?tab=<key> opens a tab (e.g. ?tab=fwd&ym=2026-11 from Approvals opens Upcoming pay lines; the old
 *  ?tab=close opens Payroll run, where Month close now lives);
 *  ?tab=claims&sub=tax&emp=EMP-1001 opens Claims and changes with that employee's tax review. */
export default function Payroll() {
  const { can } = useAuth();
  const cfgOk = can('pay.setup', 2);
  const tabs = TABS.filter(([k]) => cfgOk || BASIC.includes(k));
  const [sp] = useSearchParams();
  const linked = (): T => { const x = sp.get('tab') === 'close' ? 'run' : sp.get('tab') as T; return TABS.some(([k]) => k === x) ? x : 'calc'; };
  const [t, setT] = useState<T>(linked);
  const link = sp.toString();
  useEffect(() => { if (sp.get('tab')) setT(linked()); }, [link]); // eslint-disable-line react-hooks/exhaustive-deps
  const taxEmp = sp.get('tab') === 'claims' && sp.get('sub') === 'tax' ? sp.get('emp') ?? '' : '';
  const [arrearFor, setArrearFor] = useState('');
  const cur = tabs.some(([k]) => k === t) ? t : 'calc';
  // Counts on the Tax declarations and Nominations sub-tabs (prototype vInbox cnt): declarations with proofs waiting, nominations waiting.
  const cnt = useLoad(() => (cur === 'claims' ? Promise.all([api('/tax').catch(() => null), can('pay.claims') ? api('/nominations').catch(() => null) : Promise.resolve(null)]) : Promise.resolve(null)), [cur, link]);
  const taxN: number | undefined = cnt.data?.[0]?.rows?.filter((r: any) => r.pending > 0 && r.status !== 'Not declared' && r.status !== 'Not submitted').length;
  const nomN: number | undefined = cnt.data?.[1]?.submitted?.length;
  return (
    <>
      <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${cur === k ? 'on' : ''}`} onClick={() => setT(k)}>{l}</button>)}</div>
      {cur === 'calc' ? <Salary /> : cur === 'run' ? <PayrollRun onArrear={(id) => { setArrearFor(id); setT('claims'); }} /> : cur === 'fwd' ? <Upcoming key={'fwd' + link} ym={sp.get('tab') === 'fwd' ? sp.get('ym') ?? undefined : undefined} />
        : cur === 'claims' ? <ClaimsChanges key={'claims' + link} arrearFor={arrearFor} initial={sp.get('tab') === 'claims' ? sp.get('sub') ?? undefined : undefined} extra={[['tax', 'Tax declarations', <TaxDeclarations key={'tax' + taxEmp} emp={taxEmp} onChange={cnt.reload} />, taxN],
          ...(can('pay.claims') ? [['nom', 'Nominations', <NominationsAdmin key="nom" queueOnly onChange={cnt.reload} />, nomN] as [string, string, JSX.Element, number?]] : [])]} /> : cur === 'var' ? <VariablePay /> : cur === 'fnf' ? <FnfAdmin /> : cur === 'files' ? <Statutory /> : cur === 'tax' ? <TaxAdmin />
          : cur === 'slip' ? <PayslipSetup /> : cur === 'map' ? <StructureMap /> : cur === 'setup' ? <PaySchedule /> : <Pending name={cur} />}
    </>
  );
}
export { Pending };

/** Structure mapping (prototype vPay 'map'): move employees from one salary structure version to another from a date.
 *  The server lists who is on the version on that date (GET /comp/structure-map/candidates). */
function StructureMap() {
  const [m, setM] = useState<{ from: string; to: string; eff: string; city: string; type: string; sel: Record<string, boolean> }>({ from: '', to: '', eff: today(), city: '', type: '', sel: {} });
  const eff = m.eff || today();
  const d = useLoad(() => api('/comp/structure-map/candidates?' + new URLSearchParams({ ...(m.from ? { from: m.from } : {}), date: eff, ...(m.city ? { city: m.city } : {}), ...(m.type ? { type: m.type } : {}) })), [m.from, eff, m.city, m.type]);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const [busy, setBusy] = useState(false);
  const x = d.data, cands: any[] = x?.rows ?? [];
  const pg = usePage(cands, 200);
  if (!x) return <Loading error={d.error} />;
  const vers: any[] = x.versions, from: string = x.from;
  const name = (id: string) => vers.find((v) => v.id === id)?.name ?? '—';
  const nsel = cands.filter((e) => m.sel[e.id]).length;
  const apply = async () => {
    if (!m.to) { toast('Choose the version to move to'); return; }
    if (!m.eff) { toast('Choose the date it applies from'); return; }
    const ids = cands.filter((e) => m.sel[e.id]).map((e) => e.id);
    if (!ids.length) { toast('Select at least one employee'); return; }
    setBusy(true);
    try {
      const r = await api('/comp/structure-map', { body: { structureId: m.to, fromDate: eff, ids, from } });
      toast(`${r.mapped} employees mapped`);
      setM({ ...m, sel: {} }); d.reload();
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const logText = (l: any) => { const n = l.count ?? 0; return `${n} employee${n === 1 ? '' : 's'} moved ${l.fromName ? `from ${l.fromName} ` : ''}to ${l.toName}, applicable from ${fmtD(l.date)}, by ${l.role || l.by}`; };
  return (
    <>
      <div className="note" style={{ marginBottom: 14 }}>Move employees from one salary structure version to another from a date you choose. Payroll uses the old version before that date and the new one from it. If the date falls inside a month, that month is split by days. The old mapping stays in the employee's history.</div>
      <div className="grid g3" style={{ marginBottom: 14 }}>{vers.map((v) => <div key={v.id} className="card stat"><div className="l">{v.name} {v.isDefault && <Pill k="ok">Default</Pill>}{v.status !== 'Active' && <Pill>Switched off</Pill>}</div><div className="n">{v.today}</div><div className="mute" style={{ fontSize: 12 }}>employees today</div></div>)}</div>
      <div className="card" style={{ marginBottom: 14 }}><h3>Move employees to another version</h3><div className="form">
        <Field label="From version"><select value={from} onChange={(e) => setM({ ...m, from: e.target.value, to: m.to === e.target.value ? '' : m.to, sel: {} })}>{vers.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}</select></Field>
        <Field label="To version"><select value={m.to} onChange={(e) => setM({ ...m, to: e.target.value })}><option value="">Select</option>{vers.filter((v) => v.status === 'Active' && v.id !== from).map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}</select></Field>
        <Field label="Applicable from"><input type="date" value={m.eff} onChange={(e) => setM({ ...m, eff: e.target.value, sel: {} })} /></Field>
        <Field label="City"><select value={m.city} onChange={(e) => setM({ ...m, city: e.target.value, sel: {} })}><option value="">All cities</option>{(cities.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
        <Field label="Employment type"><select value={m.type} onChange={(e) => setM({ ...m, type: e.target.value, sel: {} })}><option value="">All types</option>{['Permanent', 'Retainer', 'Contractual'].map((t) => <option key={t}>{t}</option>)}</select></Field>
      </div>
        <div className="row sp" style={{ margin: '14px 0 8px' }}><span className="mute" style={{ fontSize: 13 }}>{cands.length} employees are on {name(from)} on {fmtD(eff)}. {nsel} selected.</span><button className="btn sm" onClick={() => setM({ ...m, sel: Object.fromEntries(cands.map((e) => [e.id, true])) })}>Select all</button></div>
        <div className="tw"><table><thead><tr><th></th><th>Employee</th><th>City</th><th>Type</th><th className="num">Gross</th><th>History</th></tr></thead><tbody>
          {(pg.rows ?? []).map((e) => <tr key={e.id}><td><input type="checkbox" checked={!!m.sel[e.id]} onChange={(v) => setM({ ...m, sel: { ...m.sel, [e.id]: v.target.checked } })} /></td><td><b>{e.name}</b> <span className="mono mute">{e.id}</span></td><td>{e.city}</td><td>{e.type}</td>
            <td className="num">{inr(e.gross)}</td><td className="mute" style={{ fontSize: 12 }}>{(e.history || []).map((h: any, i: number) => (i ? ' → ' : '') + h.structure + (h.from < '1990' ? '' : ' from ' + fmtD(h.from))).join('')}</td></tr>)}
          {!cands.length && <tr><td colSpan={6} className="mute">No employees match.</td></tr>}
        </tbody></table></div>{pg.pager}
        <div className="row" style={{ marginTop: 14 }}><button className="btn primary" disabled={busy || !x.canApply} onClick={apply}>Apply to {nsel} employee{nsel === 1 ? '' : 's'}</button></div></div>
      <div className="card"><h3>Mapping history</h3><ul className="log">
        {x.log.map((l: any, i: number) => <li key={i}><div>{logText(l)}</div><div className="mute" style={{ fontSize: 12 }}>{l.at}</div></li>)}
        {!x.log.length && <li className="mute">No changes yet.</li>}
      </ul></div>
    </>
  );
}

const COMPONENTS: [string, string, string, string][] = [['Basic + DA', 'Earning', '% of gross, from the standard salary structure', 'Taxable'], ['HRA', 'Earning', '% of basic', 'Partly exempt'], ['Special allowance', 'Earning', 'Balancing figure', 'Taxable'],
  ['Variable pay', 'Earning, on top of fixed salary', "Standard %, HR can change it. Paid monthly or every 3, 6 or 12 months on each person's own cycle, counted from the joining month (or the month variable pay started)", 'Taxable'], ['Bonus', 'Earning, on top of gross', 'Automatic below the gross ceiling', 'Taxable'],
  ['Incentive', 'Earning, a pay line', 'Uploaded month by month in Bulk update › Incentives', 'Taxable; not PF or ESI wages'],
  ['Employee PF', 'Deduction', '12% of basic, capped at the wage ceiling (set in the salary structure). Not for a PF exempt joiner (below)', 'Statutory'], ['Labour welfare fund', 'Deduction and employer retiral', 'By state, switched on by employment type', 'Statutory'],
  ['ESI', 'Deduction', 'Automatic when gross is at or below the ESI ceiling', 'Statutory'], ['Meal and accommodation', 'Deduction', 'Entered by HR, permanent employees', ''], ['TDS', 'Deduction', 'Retainers only', 'Statutory'],
  ['Employer PF, EPFO admin, ESI', 'Employer retiral', 'Same rates as above', ''], ['Medical insurance', 'Employer retiral', 'Automatic when gross is above the ESI ceiling', ''], ['Gratuity', 'Employer retiral', '% of basic, permanent employees', ''],
  ['Meal and accommodation cost', 'Benefit in CTC', 'Internal cost, every employee who gets them', ''], ['PL encashment provision', 'Benefit in CTC', 'Days of basic a year, permanent employees', '']];

/** Limits on salary advances employees ask for themselves (My pay › Salary advance): saved through /advances/settings. */
function AdvanceRules() {
  const d = useLoad(() => api('/advances/settings'), []);
  const [mult, setMult] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  if (!d.data) return <Loading error={d.error} />;
  const r = d.data.rules, ed = !!d.data.canEdit;
  const save = async (patch: { maxMultiple?: number; oneOpen?: boolean; minMonths?: number; maxInstalments?: number }) => {
    setBusy(true);
    try { await api('/advances/settings', { method: 'PUT', body: patch }); toast('Saved'); setMult(null); } catch (e: any) { toast(e.message); } finally { setBusy(false); d.reload(); }
  };
  const m = mult ?? String(r.maxMultiple);
  return <div className="card" style={{ marginBottom: 14 }} data-testid="adv-settings"><h3>Salary advances asked for by employees</h3>
    <div className="form">
      <Field label="Most an employee can ask for (× monthly gross, 0 = no limit)"><input type="number" min={0} max={12} step={0.5} disabled={!ed || busy} value={m}
        onChange={(e) => setMult(e.target.value)} onBlur={() => { if (mult != null && +mult !== r.maxMultiple) save({ maxMultiple: +mult }); else setMult(null); }} /></Field>
      <Field label="Advances open at a time"><select disabled={!ed || busy} value={r.oneOpen ? 'one' : 'any'} onChange={(e) => save({ oneOpen: e.target.value === 'one' })}>
        <option value="one">One (a new one once the last is recovered)</option><option value="any">No limit</option></select></Field>
      <Field label="Months of service before asking"><select disabled={!ed || busy} value={String(r.minMonths ?? 3)} onChange={(e) => save({ minMonths: +e.target.value })}>
        {[0, 1, 2, 3, 4, 5, 6, 9, 12].map((n) => <option key={n} value={n}>{n === 0 ? 'No minimum' : `${n} month${n === 1 ? '' : 's'}`}</option>)}</select></Field>
      <Field label="Most monthly instalments"><select disabled={!ed || busy} value={String(r.maxInstalments ?? 12)} onChange={(e) => save({ maxInstalments: +e.target.value })}>
        {Array.from({ length: 24 }, (_, i) => i + 1).map((n) => <option key={n} value={n}>{n}</option>)}</select></Field>
    </div>
    <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Employees ask from My pay › Salary advance: permanent staff with the months of service above. They give the amount and a remark; the recovery is the full amount in the first month after the payout unless the approver (usually the manager) sets more instalments, up to the most above, when approving. Every request goes through the salary advance approval route. Advances Payroll or HR give from Employee dues are not limited by these settings.</p>
  </div>;
}

/** PF for new joiners (owner, 14 Nov 2026): the gross above which a joiner with no UAN is PF exempt, and the UAN check. */
function PfRules() {
  const d = useLoad(() => api('/payroll/pf-settings'), []);
  const [g, setG] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  if (!d.data) return <Loading error={d.error} />;
  const r = d.data.rules, ed = !!d.data.canEdit;
  const save = async (patch: { exemptGross?: number; uanHold?: boolean }) => {
    setBusy(true);
    try { await api('/payroll/pf-settings', { method: 'PUT', body: patch }); toast('Saved'); setG(null); } catch (e: any) { toast(e.message); } finally { setBusy(false); d.reload(); }
  };
  const v = g ?? String(r.exemptGross);
  return <div className="card" style={{ marginBottom: 14 }} data-testid="pf-settings"><h3>PF for new joiners</h3>
    <div className="form">
      <Field label="PF exempt above this monthly gross, with no existing UAN (₹)"><input type="number" min={1000} step={500} disabled={!ed || busy} value={v}
        onChange={(e) => setG(e.target.value)} onBlur={() => { if (g != null && +g !== r.exemptGross) save({ exemptGross: +g }); else setG(null); }} /></Field>
      <Field label="Hold salary when the UAN is missing"><select disabled={!ed || busy} value={r.uanHold ? 'on' : 'off'} onChange={(e) => save({ uanHold: e.target.value === 'on' })}>
        <option value="on">On: held at the payroll run, released once the UAN is entered</option><option value="off">Off</option></select></Field>
    </div>
    <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>A joiner whose monthly gross is above this amount and who has no existing UAN is PF exempt: no employee or employer PF, EPS or EDLI, and not in the ECR. Anyone with an existing UAN is never exempt, and someone already paid PF is not changed. With Basic at ₹15,000 or less the law may still require PF: the employee and the payroll checklist show a warning. Payroll or HR Admin can override it on the employee. A PF member with no UAN on record has the salary held at the payroll run (Salary holds, "UAN not provided"); the next run releases it once the UAN is entered. Changes are audited.</p>
  </div>;
}

/** The company's ICICI debit account for the bank upload file (Payroll run and Reports › Bank advice): digits only, 9 to 18. */
function BankUpload({ acc, ed, busy, onSave }: { acc: string; ed: boolean; busy: boolean; onSave: (v: string) => void }) {
  const [v, setV] = useState(acc ?? '');
  useEffect(() => setV(acc ?? ''), [acc]);
  const t = v.trim(), ok = /^\d{9,18}$/.test(t), changed = t !== (acc ?? '');
  return <div className="card" style={{ marginBottom: 14 }}><h3>Bank upload</h3><div className="form">
    <Field label="Bank upload: ICICI debit account number"><input type="text" inputMode="numeric" disabled={!ed || busy} value={v} maxLength={18}
      onChange={(e) => setV(e.target.value.replace(/[^\d]/g, ''))} onKeyDown={(e) => e.key === 'Enter' && ok && changed && onSave(t)} /></Field>
    {ed && <div className="field" style={{ alignSelf: 'end' }}><button className="btn" disabled={busy || !ok || !changed} onClick={() => onSave(t)}>Save</button></div>}
  </div>
    <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>The company account every salary in the ICICI bank upload is paid from. Digits only, 9 to 18. {t && !ok ? <b>Enter 9 to 18 digits.</b> : null} ICICI's template describes a 12-digit debit account; check the number with the bank if the upload is refused.</p>
  </div>;
}

/** Pay schedule and components (prototype vPay 'setup'): saved through /payroll/schedule, field by field. */
function PaySchedule() {
  const d = useLoad(() => api('/payroll/schedule'), []);
  const [busy, setBusy] = useState(false);
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, ed = !!x.canEdit;
  const save = async (patch: { cutoff?: string; firstYm?: string; payDay?: string; iciciDebitAcc?: string }, done: string) => {
    setBusy(true);
    try { await api('/payroll/schedule', { method: 'PUT', body: patch }); toast(done); } catch (e: any) { toast(e.message); } finally { setBusy(false); d.reload(); }
  };
  return (
    <>
      <div className="note" style={{ marginBottom: 14 }}>Rates and the days basis come from the standard salary structure in Compensation, Salary structure.</div>
      <div className="card" style={{ marginBottom: 14 }}><h3>Pay schedule</h3><div className="form">
        <Field label="Pay frequency"><select disabled><option>Monthly</option></select></Field>
        <Field label="Attendance cut-off"><select disabled={!ed || busy} value={x.cutoff} onChange={(e) => save({ cutoff: e.target.value }, 'Saved')}><option value="calendar">Calendar month</option><option value="26-25">26th to 25th</option></select></Field>
        <Field label="Salary paid on"><select disabled={!ed || busy} value={x.payDay} onChange={(e) => save({ payDay: e.target.value }, 'Saved')}>{(x.payDays as string[]).map((p) => <option key={p}>{p}</option>)}</select></Field>
        <Field label="First payroll month in this system"><input type="month" disabled={!ed || busy || x.started} value={x.firstYm ?? ''} onChange={(e) => save({ firstYm: e.target.value }, 'First payroll month saved')} /></Field>
      </div></div>
      <BankUpload acc={x.iciciDebitAcc} ed={ed} busy={busy} onSave={(v) => save({ iciciDebitAcc: v }, 'Debit account saved')} />
      <PfRules />
      <AdvanceRules />
      <div className="card"><h3>Salary components</h3><p className="mute" style={{ fontSize: 12.5, margin: '-4px 0 8px' }}>These are the components of the percentage structures. Structures made
        from components (their own components, variable pay entered by HR) are set up in Compensation › Pay components.</p><div className="tw"><table><thead><tr><th>Component</th><th>Type</th><th>Calculation</th><th>Tax treatment</th></tr></thead><tbody>
        {COMPONENTS.map((r) => <tr key={r[0]}><td><b>{r[0]}</b></td><td>{r[1]}</td><td className="ink2">{r[2]}</td><td>{r[3]}</td></tr>)}
      </tbody></table></div></div>
    </>
  );
}
