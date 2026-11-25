import { useState, type ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { api, fmtD, inr, openDoc, postForm, uploadDoc, today } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Modal, Pill, Switch, download, toast, useLoad } from '../components/ui';
import { usePayState, ymLabel } from './PayrollRun';

type Item = { id: string; group: string; name: string; cap: number; active?: boolean };
const QN: Record<number, string> = { 1: 'Q1 (Apr to Jun)', 2: 'Q2 (Jul to Sep)', 3: 'Q3 (Oct to Dec)', 4: 'Q4 (Jan to Mar)' };
const pill2 = (s: string) => <Pill k={s === 'Approved' ? 'ok' : s === 'Rejected' ? 'bad' : s === 'Pending' ? 'warn' : 'n'}>{s}</Pill>;
const METRO = ['Delhi', 'Mumbai', 'Kolkata', 'Chennai'];
const SECTION_GROUPS = ['80C', '80CCD(1B)', '80D', '80E', '80G', '80TTA', '24(b)', 'LTA'];
/** Financial year months, April to March, for '2026-27'. */
const fyMonths = (fy: string) => { const y = +fy.slice(0, 4); return [4, 5, 6, 7, 8, 9, 10, 11, 12, 1, 2, 3].map((m) => `${m >= 4 ? y : y + 1}-${String(m).padStart(2, '0')}`); };
const qOf = (ym: string) => { const m = +ym.slice(5, 7); return m >= 4 && m <= 6 ? 1 : m <= 9 && m >= 7 ? 2 : m >= 10 ? 3 : 4; };
const nextYm = (ym: string) => { const y = +ym.slice(0, 4), m = +ym.slice(5, 7); return m === 12 ? `${y + 1}-01` : `${y}-${String(m + 1).padStart(2, '0')}`; };
const dueDate = (ym: string) => (ym.slice(5, 7) === '03' ? ym.slice(0, 4) + '-04-30' : nextYm(ym) + '-07');
/** Prototype nowS: "20 Oct, 10:15" (day, short month, 24-hour time), from a server timestamp. */
const stamp = (s: string) => {
  const d = new Date(s);
  if (isNaN(+d)) return fmtD(String(s).slice(0, 10));
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getDate())} ${d.toLocaleString('en-IN', { month: 'short' })}, ${p(d.getHours())}:${p(d.getMinutes())}`;
};
/** "1 April 2027" from 2027-04-01. */
const fmtLong = (d?: string) => (d ? new Date(+d.slice(0, 4), +d.slice(5, 7) - 1, +d.slice(8, 10)).toLocaleDateString('en-GB', { day: 'numeric', month: 'long', year: 'numeric' }) : '');
const curFy = () => { const t = today(), y = +t.slice(0, 4), m = +t.slice(5, 7), s = m >= 4 ? y : y - 1; return `${s}-${String((s + 1) % 100).padStart(2, '0')}`; };

/** Prototype taxStatus. */
function taxStatus(decl: any, proofs: any[]): [string, 'ok' | 'warn' | 'info' | 'n'] {
  if (decl.finalAt) return ['Finalised', 'ok'];
  if (decl.status === 'Draft') return [Object.values(decl.items || {}).some((v: any) => v > 0) ? 'Not submitted' : 'Not declared', 'n'];
  if (proofs.some((p) => p.status === 'Pending')) return ['Proofs to review', 'warn'];
  if (proofs.some((p) => p.status === 'Approved')) return ['Proofs reviewed', 'info'];
  return ['Declared', 'info'];
}
const statusTone = (s: string) => (s === 'Finalised' ? 'ok' : s === 'Proofs to review' ? 'warn' : s === 'Proofs reviewed' || s === 'Declared' ? 'info' : 'n') as 'ok' | 'warn' | 'info' | 'n';

/** Prototype txEstHtml: both regimes side by side on the declared amounts. */
function Estimate({ f, regime }: { f: any; regime: string }) {
  if (!f) return <p className="mute" style={{ margin: 0 }}>No salary on record for this year.</p>;
  const a = f.declared ?? f.est, old = regime === 'Old';
  return <>
    <table><thead><tr><th></th><th className="num">New regime</th><th className="num">Old regime</th></tr></thead><tbody>
      {f.reimb > 0 && <tr><td>Reimbursement a month, outside the gross</td><td className="num" colSpan={2}>{inr(f.reimb)}</td></tr>}
      <tr><td>Taxable income</td><td className="num">{inr(a.newTaxable)}</td><td className="num">{inr(a.oldTaxable)}</td></tr>
      <tr><td>Deductions counted</td><td className="num">Standard ₹75,000</td><td className="num">{inr(a.oldDeductions + a.hraExempt)}{a.hraExempt > 0 && <> <span className="mute" style={{ fontSize: 12 }}>(HRA {inr(a.hraExempt)})</span></>}</td></tr>
      <tr><td><b>Estimated yearly tax</b></td><td className="num"><b>{inr(a.newTax)}</b></td><td className="num"><b>{inr(a.oldTax)}</b></td></tr>
    </tbody></table>
    <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>{a.newTax === a.oldTax ? 'Both regimes give the same tax.' : a.newTax < a.oldTax ? `The new regime is lower by ${inr(a.oldTax - a.newTax)}.` : `The old regime is lower by ${inr(a.newTax - a.oldTax)}.`} You have chosen the <b>{old ? 'old' : 'new'}</b> regime. Estimate only, after 4% cess; surcharge is not included.</p>
  </>;
}

/** My tax declaration (prototype vTaxEss and vTaxProofs): regime, deductions, rent, other income, estimate, proofs. */
export function MyTax() {
  const d = useLoad(() => api('/tax/me'), []);
  const [v, setV] = useState<any>(null);
  const [pf, setPf] = useState<Record<string, { amount?: string; remark?: string; file?: File }>>({});
  const [busy, setBusy] = useState(false);
  if (d.error) return <div className="note bad">{d.error}</div>;
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data;
  if (!x.applies) return <div className="note">{x.employee.type} staff do not declare tax with the company. Tax, if any, is deducted as per the contract.</div>;
  const saved = x.declaration;
  const decl = v ?? { regime: saved.regime, items: saved.items || {}, landlordName: saved.landlordName, landlordPan: saved.landlordPan, otherIncome: saved.otherIncome, prevIncome: saved.prevIncome, prevTds: saved.prevTds };
  const w = x.windows, final = !!saved.finalAt, dO = w.declOpen, pO = w.proofOpen, dis = !dO || final, old = decl.regime === 'Old';
  // The regime is chosen once a financial year: once saved it stays until the next year, unless Payroll unlocks it.
  const lk = x.regimeLock ?? {}, locked = !!lk.locked, nextFy = fmtLong(lk.nextFrom);
  const st = taxStatus(saved, x.proofs), items: Item[] = (x.items as Item[]).filter((i) => i.active !== false);
  const setItem = (id: string, val: string) => setV({ ...decl, items: { ...decl.items, [id]: val === '' ? 0 : +val } });
  const submit = async () => {
    setBusy(true);
    try { await api('/tax/me', { method: 'PUT', body: { ...decl, submit: true } }); toast('Declaration submitted'); setV(null); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const upload = async (it: Item) => {
    const p = pf[it.id] || {};
    if (!p.file) { toast('Choose the proof file'); return; }
    const amt = p.amount != null ? +p.amount : Math.max(0, (saved.items[it.id] || 0) - approvedOf(it.id));
    if (!(amt > 0)) { toast('Enter the amount on the proof'); return; }
    try {
      const doc = await uploadDoc('employee', x.employee.id, 'Tax proof', p.file);
      await api('/tax/me/proofs', { body: { item: it.id, doc: doc.id, amount: amt, remark: p.remark || '' } });
      toast('Proof uploaded'); setPf({ ...pf, [it.id]: {} }); d.reload();
    } catch (e: any) { toast(e.message); }
  };
  const withdraw = async (id: string) => { try { await api(`/tax/me/proofs/${id}/withdraw`, { body: {} }); d.reload(); } catch (e: any) { toast(e.message); } };
  const approvedOf = (id: string) => x.proofs.filter((p: any) => p.item === id && p.status === 'Approved').reduce((a: number, p: any) => a + (p.approved || 0), 0);
  const win = (from: string, to: string) => `${fmtD(from)} to ${fmtD(to)}`;
  const rows = (g: string) => items.filter((i) => i.group === g).map((i) => <tr key={i.id}><td>{i.name}</td><td className="mute" style={{ fontSize: 12.5 }}>{i.cap ? 'Up to ' + inr(i.cap) : ''}</td>
    <td className="num"><input type="number" min="0" disabled={dis} value={decl.items[i.id] || ''} onChange={(e) => setItem(i.id, e.target.value)} style={{ width: 130 }} /></td></tr>);
  const rent = decl.items.hra || 0;
  const declared = items.filter((i) => old && (saved.items[i.id] || 0) > 0);
  return (
    <>
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="row sp" style={{ flexWrap: 'wrap' }}>
          <div><h3 style={{ margin: 0 }}>Income tax declaration, FY {x.fy}</h3><div className="mute" style={{ fontSize: 12.5, marginTop: 2 }}>Status: <Pill k={st[1]}>{st[0]}</Pill></div></div>
          <div style={{ textAlign: 'right', fontSize: 12.5 }}>
            <div>Declaration <Pill k={dO ? 'ok' : 'n'}>{dO ? 'Open' : 'Closed'}</Pill> <span className="mute">{win(w.declFrom, w.declTo)}</span></div>
            <div style={{ marginTop: 4 }}>Proof upload <Pill k={pO ? 'ok' : 'n'}>{pO ? 'Open' : 'Closed'}</Pill> <span className="mute">{win(w.proofFrom, w.proofTo)}</span></div>
          </div>
        </div>
        <div className="row" style={{ gap: 22, margin: '14px 0 4px' }}>{['New', 'Old'].map((rg) => <label key={rg} className="chkl"><input type="radio" name="tx_reg" checked={decl.regime === rg} disabled={dis || locked} onChange={() => setV({ ...decl, regime: rg })} />{rg} tax regime</label>)}</div>
        {locked ? <div className="note" style={{ margin: '8px 0 0', fontSize: 12.5 }}>You chose the {String(saved.regime).toLowerCase()} regime for FY {x.fy}. You can choose again from {nextFy}. Your declarations and proofs can still be changed. If the choice was a mistake, ask Payroll to unlock it.</div>
          : !final && dO && <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 0' }}>You choose the regime once for FY {x.fy}. When you submit, it is kept for the whole year and you can choose again from {nextFy}.
            {lk.unlockedAt && <> Payroll unlocked your choice on {fmtD(String(lk.unlockedAt).slice(0, 10))}{lk.reason ? ` (${lk.reason})` : ''}, so choose again and submit.</>}</p>}
        <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 0' }}>{old ? 'Old regime: standard deduction of ₹50,000 plus the deductions you declare below.' : 'New regime: lower slab rates and a standard deduction of ₹75,000. Section deductions and HRA do not apply, so you only need to fill income from other sources.'}</p>
      </div>
      {old && <>
        <div className="card" style={{ marginBottom: 14 }}><h3>Deductions</h3><div className="tw"><table><thead><tr><th>Investment or expense</th><th>Limit</th><th className="num">Amount for the year (₹)</th></tr></thead><tbody>
          <tr><td colSpan={3} className="mute" style={{ fontSize: 12, paddingTop: 10 }}>Section 80C, limit {inr(x.caps['80C'])} in total</td></tr>
          <tr><td>Provident fund (from your payroll)</td><td className="mute" style={{ fontSize: 12.5 }}>Counted automatically</td><td className="num">{inr(x.figure?.pfYear ?? 0)}</td></tr>
          {rows('80C')}
          {SECTION_GROUPS.filter((g) => g !== '80C' && items.some((i) => i.group === g)).map((g) => <Rows key={g}><tr><td colSpan={3} className="mute" style={{ fontSize: 12, paddingTop: 10 }}>{x.groups[g]}{x.caps[g] ? `, limit ${inr(x.caps[g])} in total` : ''}</td></tr>{rows(g)}</Rows>)}
        </tbody></table></div></div>
        <div className="card" style={{ marginBottom: 14 }}><h3>House rent (HRA)</h3><div className="form">
          <Field label="Rent paid in the year (₹)"><input type="number" min="0" disabled={dis} value={rent || ''} onChange={(e) => setItem('hra', e.target.value)} /></Field>
          <Field label="Landlord name"><input type="text" disabled={dis} value={decl.landlordName} onChange={(e) => setV({ ...decl, landlordName: e.target.value })} /></Field>
          <Field label={`Landlord PAN ${rent > 100000 ? '(required, rent is above ₹1,00,000)' : '(optional)'}`}><input type="text" maxLength={10} disabled={dis} style={{ textTransform: 'uppercase' }} value={decl.landlordPan} onChange={(e) => setV({ ...decl, landlordPan: e.target.value.toUpperCase() })} /></Field>
        </div><p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Your work city, {x.employee.city}, is treated as {METRO.includes(x.employee.city) ? 'a metro (50% of basic)' : 'a non-metro (40% of basic)'} for the HRA exemption.</p></div>
      </>}
      <div className="card" style={{ marginBottom: 14 }}><h3>Other income</h3><div className="form">
        <Field label="Income from a previous employer this year (₹)"><input type="number" min="0" disabled={dis} value={decl.prevIncome || ''} onChange={(e) => setV({ ...decl, prevIncome: +e.target.value })} /></Field>
        <Field label="Tax already deducted by previous employer (₹)"><input type="number" min="0" disabled={dis} value={decl.prevTds || ''} onChange={(e) => setV({ ...decl, prevTds: +e.target.value })} /></Field>
        <Field label="Other income: interest, rent, etc. (₹)"><input type="number" min="0" disabled={dis} value={decl.otherIncome || ''} onChange={(e) => setV({ ...decl, otherIncome: +e.target.value })} /></Field>
      </div><p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>If you joined mid-year, give Form 12B or Form 16 of your previous employer to Payroll.</p></div>
      <div className="card" style={{ marginBottom: 14 }}><h3>Estimated tax</h3><div className="tw"><Estimate f={x.figure} regime={decl.regime} /></div></div>
      <div className="row" style={{ marginBottom: 6, alignItems: 'center' }}>
        {dO && !final && <button className="btn primary" disabled={busy} onClick={submit}>Submit declaration</button>}
        {v && <span className="mute" style={{ fontSize: 12.5 }}>You have changes that are not submitted.</span>}
        {!v && saved.submittedAt && saved.status !== 'Draft' && <span className="mute" style={{ fontSize: 12.5 }}>Submitted {stamp(saved.submittedAt)}</span>}
      </div>
      {saved.regime !== 'Old' || (!declared.length && !x.proofs.length)
        ? <div className="card"><h3>Proofs</h3><p className="mute" style={{ margin: 0 }}>Proofs are needed only for old-regime deductions you declared. Declare them above first.</p></div>
        : <div className="card"><div className="row sp"><h3 style={{ margin: 0 }}>Proofs</h3><Pill k={pO ? 'ok' : 'n'}>{pO ? 'Upload window open' : 'Upload window closed'}</Pill></div>
          <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 12px' }}>{final ? 'Your tax for the year is finalised.' : pO ? `Upload a bill or certificate for each declaration, with a short remark, by ${fmtD(w.proofTo)}. Payroll reviews each proof. Only approved amounts count in your final tax; the rest is recovered from your last salaries.` : `The upload window is ${win(w.proofFrom, w.proofTo)}. Until then your declaration is used to estimate monthly tax.`}</p>
          {declared.map((i) => {
            const dd = saved.items[i.id] || 0, ap = approvedOf(i.id), ps = x.proofs.filter((p: any) => p.item === i.id), p = pf[i.id] || {};
            return <div key={i.id} style={{ borderTop: '1px solid var(--line)', padding: '12px 0' }}>
              <div className="row sp" style={{ flexWrap: 'wrap' }}><b>{i.name}</b><span className="ink2" style={{ fontSize: 12.5 }}>Declared {inr(dd)} · Approved {inr(ap)} {ap >= dd && <Pill k="ok">Complete</Pill>}</span></div>
              {ps.length > 0 && <ul className="log" style={{ margin: '8px 0' }}>{ps.map((q: any) => <li key={q.id}>
                <a href="#" onClick={(e) => { e.preventDefault(); openDoc(q.doc).catch((er) => toast(er.message)); }}>{q.file_name || 'Proof'}</a> · {inr(q.amount)} {pill2(q.status)}{q.status === 'Approved' && q.approved !== q.amount && <> <span className="mute" style={{ fontSize: 12 }}>approved {inr(q.approved)}</span></>}
                <div className="mute" style={{ fontSize: 12 }}>{q.at}{q.remark ? ' · ' + q.remark : ''}{q.note ? ' · Payroll: ' + q.note : ''} {q.status === 'Pending' && pO && !final && <button className="btn sm" onClick={() => withdraw(q.id)}>Withdraw</button>}</div></li>)}</ul>}
              {pO && !final && ap < dd && <div className="row" style={{ flexWrap: 'wrap', gap: 8, marginTop: 6 }}>
                <input type="number" min="0" placeholder="Amount (₹)" value={p.amount ?? String(dd - ap)} onChange={(e) => setPf({ ...pf, [i.id]: { ...p, amount: e.target.value } })} style={{ width: 130 }} />
                <input type="file" accept="application/pdf,image/png,image/jpeg" style={{ maxWidth: 220 }} onChange={(e) => setPf({ ...pf, [i.id]: { ...p, file: e.target.files?.[0] } })} />
                <input type="text" placeholder="Remark" value={p.remark ?? ''} onChange={(e) => setPf({ ...pf, [i.id]: { ...p, remark: e.target.value } })} style={{ flex: 1, minWidth: 160 }} />
                <button className="btn" onClick={() => upload(i)}>Upload</button></div>}
            </div>;
          })}
        </div>}
    </>
  );
}
const Rows = ({ children }: { children: ReactNode }) => <>{children}</>;

/** Tax declarations for Payroll (prototype vTaxInbox): windows, counts, the declaration list and proof review. Shown in Claims and changes.
 *  emp opens that employee's review straight away (deep link from Approvals). */
export function TaxDeclarations({ emp = '', onChange }: { emp?: string; onChange?: () => void } = {}) {
  const { can } = useAuth();
  const d0 = useLoad(() => api('/tax'), []);
  const d = { ...d0, reload: () => { d0.reload(); onChange?.(); } };
  const its = useLoad(() => api('/tax/items'), []);
  const [q, setQ] = useState(''), [qq, setQq] = useState(''), [f, setF] = useState(''), [page, setPage] = useState(1);
  const [open, setOpen] = useState(emp); // a deep link opens that employee's review
  const [caps, setCaps] = useState<Record<string, string>>({});
  if (d.error) return <div className="note bad">{d.error}</div>;
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, w = x.windows, ed = x.canSetup;
  const all: any[] = x.rows, filed = all.filter((r) => r.status !== 'Not declared' && r.status !== 'Not submitted'), none = all.length - filed.length;
  const sel0 = filed.filter((r) => (!f || r.status === f) && (!qq || `${r.name} ${r.id}`.toLowerCase().includes(qq.toLowerCase())));
  const pages = Math.max(1, Math.ceil(sel0.length / 50)), pg = Math.min(page, pages), sel = sel0.slice((pg - 1) * 50, pg * 50);
  const tot = filed.reduce((a, r) => a + Math.max(0, r.diff || 0), 0), pend = filed.reduce((a, r) => a + (r.pending || 0), 0);
  const fig = (r: any) => { const mine = r.regime === 'Old' ? r.oldTax : r.newTax, decl = r.status === 'Finalised' ? mine - (r.diff || 0) : mine; return { decl, appr: decl + (r.diff || 0) }; };
  const saveWin = async (patch: any) => {
    const body = { declFrom: w.declFrom, declTo: w.declTo, declForce: w.declForce, proofFrom: w.proofFrom, proofTo: w.proofTo, proofForce: w.proofForce, ...patch };
    try { await api('/tax/windows', { method: 'PUT', body }); toast('Saved'); d.reload(); } catch (e: any) { toast(e.message); }
  };
  // Before the proof window's end date finalising is an explicit early step with a reason (kept in the audit).
  const fin = async () => {
    const early = today() <= w.proofTo;
    let reason = '';
    if (early) {
      const r = prompt(`The proof window runs to ${fmtD(w.proofTo)}. Employees can still upload proofs until then. To finalise early, give the reason:`);
      if (r == null) return;
      if (r.trim().length < 3) { toast('Give the reason for finalising early'); return; }
      reason = r.trim();
    } else if (!confirm('Finalise tax for the year? Only approved proofs will count, and any extra tax is spread over the months left.')) return;
    try { const r = await api('/tax/finalise', { body: early ? { force: true, reason } : {} }); toast(`Finalised for ${r.employees} people; ${inr(r.extra)} more tax to recover`); d.reload(); } catch (e: any) { toast(e.message); }
  };
  const reopen = async () => {
    const r = prompt('Reopen the finalisation? Declarations count again as declared and employees can declare and upload proofs while the windows are open. Give the reason:');
    if (r == null) return;
    if (r.trim().length < 3) { toast('Give the reason for reopening'); return; }
    try { await api('/tax/finalise/reopen', { body: { reason: r.trim() } }); toast('Finalisation reopened'); d.reload(); } catch (e: any) { toast(e.message); }
  };
  const csv = () => download(`/tax/declarations.xlsx?fy=${x.fy}`, `tax-declarations-${x.fy}.xlsx`).catch((e) => toast(e.message));
  const remind = async () => { try { const r = await api(`/tax/remind?fy=${x.fy}`, { body: {} }); toast(`Reminder sent to ${r.count} employee${r.count === 1 ? '' : 's'}`); } catch (e: any) { toast(e.message); } };
  const item = async (id: string, patch: { cap?: number; active?: boolean }) => {
    try { await api('/tax/items', { method: 'PUT', body: { items: [{ id, ...patch }] } }); toast('Saved'); its.reload(); d.reload(); } catch (e: any) { toast(e.message); its.reload(); }
  };
  const itemsEd = !!its.data?.canEdit;
  const win = (k: 'decl' | 'proof', l: string) => {
    const isOpen = w[k + 'Open'], force = w[k + 'Force'];
    return <tr><td><b>{l}</b></td>
      <td><input type="date" disabled={!ed} value={w[k + 'From']} onChange={(e) => saveWin({ [k + 'From']: e.target.value })} /></td>
      <td><input type="date" disabled={!ed} value={w[k + 'To']} onChange={(e) => saveWin({ [k + 'To']: e.target.value })} /></td>
      <td><Pill k={isOpen ? 'ok' : 'n'}>{isOpen ? 'Open' : 'Closed'}</Pill>{force != null && <> <span className="mute" style={{ fontSize: 12 }}>set by hand</span></>}</td>
      <td>{ed && <>{isOpen ? <button className="btn sm" onClick={() => saveWin({ [k + 'Force']: false })}>Close now</button> : <button className="btn sm" onClick={() => saveWin({ [k + 'Force']: true })}>Open now</button>}
        {force != null && <> <button className="btn sm" onClick={() => saveWin({ [k + 'Force']: null })}>Follow dates</button></>}</>}</td></tr>;
  };
  const fyEnd = `${+x.fy.slice(0, 4) + 1}-03`;
  return (
    <>
      <div className="card" style={{ marginBottom: 14 }}><h3>Declaration and proof windows</h3>
        <div className="tw"><table><thead><tr><th></th><th>From</th><th>To</th><th>Status</th><th></th></tr></thead><tbody>{win('decl', 'Declaration of investments')}{win('proof', 'Proof upload')}</tbody></table></div>
        <div className="row" style={{ marginTop: 12, gap: 14, flexWrap: 'wrap', alignItems: 'center' }}><span className="mute" style={{ fontSize: 12.5 }}>of the year, ending {ymLabel(fyEnd)}.</span></div>
      </div>
      <div className="grid g4" style={{ marginBottom: 14 }}>
        <div className="card stat"><div className="l">Declared</div><div className="n">{filed.length}</div><div className="mute" style={{ fontSize: 12 }}>of {all.length} permanent staff</div></div>
        <div className="card stat"><div className="l">Not declared</div><div className="n">{none}</div>{x.canReview && none > 0 && w.declOpen && <button className="btn sm" style={{ marginTop: 6 }} onClick={remind}>Remind all</button>}</div>
        <div className="card stat"><div className="l">Proofs to review</div><div className="n">{pend}</div></div>
        <div className="card stat"><div className="l">Tax to recover from unproven declarations</div><div className="n">{inr(tot)}</div></div>
      </div>
      <div className="card">
        <div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap' }}>
          <div className="row"><input type="search" value={q} placeholder="Search name or ID, press Enter" style={{ minWidth: 200, width: 'auto' }} onChange={(e) => { setQ(e.target.value); if (!e.target.value) { setQq(''); setPage(1); } }}
            onKeyDown={(e) => { if (e.key === 'Enter') { setQq(q.trim()); setPage(1); } }} />
            <select value={f} onChange={(e) => { setF(e.target.value); setPage(1); }}><option value="">All status</option>{['Not submitted', 'Declared', 'Proofs to review', 'Proofs reviewed', 'Finalised'].map((s) => <option key={s}>{s}</option>)}</select></div>
          <div className="row"><button className="btn" onClick={csv}>Download declarations</button>
            {x.canReview && x.canFinalise && !w.finalised && <button className="btn primary" disabled={w.proofOpen} title={w.proofOpen ? 'Close the proof window first' : ''} onClick={fin}>Finalise tax and recover shortfall</button>}
            {x.canFinalise && w.finalised && <button className="btn" onClick={reopen}>Reopen finalisation</button>}</div>
        </div>
        <div className="tw"><table><thead><tr><th>Employee</th><th>Regime</th><th className="num">Declared deductions</th><th className="num">Approved</th><th className="num">Tax on declaration</th><th className="num">Tax on approved proofs</th><th className="num">To recover</th><th>Status</th><th></th></tr></thead><tbody>
          {sel.map((r) => { const t = fig(r); return <tr key={r.id}><td><b>{r.name}</b> <span className="mono mute">{r.id}</span></td><td>{r.regime}</td>
            <td className="num">{r.regime === 'Old' ? inr(r.declaredDed ?? r.declared) : '—'}</td><td className="num">{r.regime === 'Old' ? inr(r.approved ?? 0) : '—'}</td><td className="num">{inr(t.decl)}</td><td className="num">{inr(t.appr)}</td>
            <td className="num">{r.diff > 0 ? <b>{inr(r.diff)}</b> : '—'}</td><td><Pill k={statusTone(r.status)}>{r.status}</Pill></td>
            <td><button className="btn sm" onClick={() => setOpen(r.id)}>Review</button></td></tr>; })}
          {!sel.length && <tr><td colSpan={9} className="mute">No declarations yet.</td></tr>}
        </tbody></table></div>
        <div className="row sp" style={{ marginTop: 10, flexWrap: 'wrap' }}><span className="mute" style={{ fontSize: 12.5 }}>{sel0.length ? `Showing ${(pg - 1) * 50 + 1} to ${Math.min(sel0.length, pg * 50)} of ${sel0.length}` : 'No rows'}</span>
          <div className="row"><button className="btn sm" disabled={pg <= 1} onClick={() => setPage(pg - 1)}>Previous</button><span className="mute" style={{ fontSize: 12.5 }}>Page {pg} of {pages}</span><button className="btn sm" disabled={pg >= pages} onClick={() => setPage(pg + 1)}>Next</button></div></div>
        <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>“To recover” is the extra yearly tax for old-regime employees when only approved proofs count instead of the declaration. Finalising switches the monthly salary TDS to approved proofs only. The balance tax is spread over the months left in the year.</p>
      </div>
      <details className="adv" style={{ marginTop: 14 }}><summary>Declaration items and limits</summary><div><p className="mute" style={{ margin: '0 0 8px', fontSize: 12.5 }}>Switch an item off to hide it from new declarations. Items are never deleted, and existing amounts stay on record.</p>
        {its.data ? <div className="tw"><table><thead><tr><th>Item</th><th>Section</th><th className="num">Limit (₹, 0 = none)</th><th>Active</th></tr></thead><tbody>
          {(its.data.items as Item[]).map((i) => <tr key={i.id}><td>{i.name}</td><td>{i.group}</td>
            <td className="num"><input type="number" min="0" disabled={!itemsEd} style={{ width: 110 }} value={caps[i.id] ?? String(i.cap)} onChange={(e) => setCaps({ ...caps, [i.id]: e.target.value })}
              onBlur={() => { const v = caps[i.id]; if (v == null) return; setCaps(({ [i.id]: _, ...rest }) => rest); if (Math.max(0, +v || 0) !== i.cap) item(i.id, { cap: Math.max(0, +v || 0) }); }}
              onKeyDown={(e) => e.key === 'Enter' && (e.target as HTMLInputElement).blur()} /></td>
            <td>{itemsEd ? <Switch on={i.active !== false} label={`${i.name} active`} onChange={() => item(i.id, { active: i.active === false })} /> : <Pill k={i.active !== false ? 'ok' : 'n'}>{i.active !== false ? 'Active' : 'Off'}</Pill>}</td></tr>)}
        </tbody></table></div> : <Loading error={its.error} />}</div></details>
      {open && <Review id={open} canReview={x.canReview && can('pay.claims', 2)} onClose={() => { setOpen(''); d.reload(); }} />}
    </>
  );
}

/** Prototype taxReviewModal: one employee's declaration and proofs, with approve and reject. */
function Review({ id, canReview, onClose }: { id: string; canReview: boolean; onClose: () => void }) {
  const d = useLoad(() => api(`/tax/employee/${id}`), [id]);
  const [amt, setAmt] = useState<Record<string, string>>({});
  const [unl, setUnl] = useState<string | null>(null);
  if (!d.data) return <Modal wide onClose={onClose}><Loading error={d.error} /></Modal>;
  const x = d.data, items: Item[] = x.items, dl = x.declaration, old = dl.regime === 'Old', f = x.figure;
  const act = async (p: any, a: 'approve' | 'reject') => {
    const note = amt[p.id + 'n'] ?? '';
    if (a === 'reject' && !note.trim()) { toast('Give a note to reject'); return; }
    try { await api(`/tax/proofs/${p.id}/${a}`, { body: { amount: +(amt[p.id + 'a'] ?? p.amount), note } }); toast(a === 'approve' ? 'Approved' : 'Rejected'); d.reload(); } catch (e: any) { toast(e.message); }
  };
  const declTax = f ? (old ? f.declared.oldTax : f.declared.newTax) : 0, apprTax = f ? (old ? f.approved.oldTax : f.approved.newTax) : 0;
  const its = items.filter((i) => (dl.items[i.id] || 0) > 0 || x.proofs.some((p: any) => p.item === i.id));
  const apprOf = (iid: string) => x.proofs.filter((p: any) => p.item === iid && p.status === 'Approved').reduce((a: number, p: any) => a + (p.approved || 0), 0);
  const rent = dl.items.hra || 0;
  const lk = x.regimeLock ?? {};
  const unlock = async () => {
    if ((unl ?? '').trim().length < 3) { toast('Give the reason for unlocking'); return; }
    try { await api(`/tax/employee/${id}/regime-unlock?fy=${x.fy}`, { body: { reason: unl!.trim() } }); toast('Regime choice unlocked. The employee can choose again'); setUnl(null); d.reload(); } catch (e: any) { toast(e.message); }
  };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><div><h2>{x.employee.name}: tax proofs</h2><span className="mono mute">{x.employee.id}</span> <Pill k="info">{dl.regime} regime</Pill></div><button className="btn sm" onClick={onClose}>Close</button></div>
    {/* The regime is chosen once a year; Payroll or HR Admin can unlock it for a correction, with a reason (audited). */}
    {lk.locked ? <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center', marginTop: 10 }}>
      <span className="ink2" style={{ fontSize: 12.5 }}>{dl.regime} regime chosen {stamp(lk.at)}, locked for FY {x.fy}.</span>
      {x.canUnlock && !dl.finalAt && (unl == null ? <button className="btn sm" onClick={() => setUnl('')}>Unlock regime choice</button>
        : <><input type="text" autoFocus placeholder="Reason, for example: chose the wrong regime" value={unl} onChange={(e) => setUnl(e.target.value)} style={{ minWidth: 240, flex: 1 }} />
          <button className="btn sm primary" onClick={unlock}>Unlock</button><button className="btn sm" onClick={() => setUnl(null)}>Cancel</button></>)}
    </div> : lk.unlockedAt ? <p className="ink2" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Regime choice unlocked by {lk.unlockedBy} on {stamp(lk.unlockedAt)}{lk.reason ? `: ${lk.reason}` : ''}. Waiting for the employee to choose again.</p> : null}
    <div className="grid g3" style={{ margin: '14px 0' }}>
      <div className="card stat"><div className="l">Tax on declaration</div><div className="n" style={{ fontSize: 18 }}>{inr(declTax)}</div></div>
      <div className="card stat"><div className="l">Tax on approved proofs</div><div className="n" style={{ fontSize: 18 }}>{inr(apprTax)}</div></div>
      <div className="card stat"><div className="l">To recover</div><div className="n" style={{ fontSize: 18 }}>{inr(old ? Math.max(0, apprTax - declTax) : 0)}</div></div>
    </div>
    {rent > 0 && <p className="ink2" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Landlord: {dl.landlordName || '—'} · PAN {dl.landlordPan || '—'}{rent > 100000 && !dl.landlordPan && <> <Pill k="bad">PAN missing</Pill></>}</p>}
    {(dl.prevIncome || dl.prevTds || dl.otherIncome) ? <p className="ink2" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Previous employer income {inr(dl.prevIncome || 0)} · tax deducted {inr(dl.prevTds || 0)} · other income {inr(dl.otherIncome || 0)}</p> : null}
    {old ? <div className="tw"><table><thead><tr><th>Item</th><th className="num">Declared</th><th className="num">Approved</th><th>Proofs</th></tr></thead><tbody>
      {its.map((i) => { const ps = x.proofs.filter((p: any) => p.item === i.id); return <tr key={i.id} style={{ verticalAlign: 'top' }}><td><b>{i.name}</b><div className="mute" style={{ fontSize: 12 }}>{i.group}</div></td>
        <td className="num">{inr(dl.items[i.id] || 0)}</td><td className="num">{inr(apprOf(i.id))}</td>
        <td>{ps.length ? ps.map((p: any) => <div key={p.id} style={{ marginBottom: 8 }}><a href="#" onClick={(e) => { e.preventDefault(); openDoc(p.doc).catch((er) => toast(er.message)); }}><b>{p.file_name || 'Proof'}</b></a> · claimed {inr(p.amount)} {pill2(p.status)}
          <div className="mute" style={{ fontSize: 12 }}>{p.at}{p.remark ? ' · ' + p.remark : ''}{p.note ? ' · ' + p.note : ''}</div>
          {p.status === 'Pending' && canReview && !dl.finalAt && <div className="row" style={{ gap: 6, marginTop: 4, flexWrap: 'wrap' }}>
            <input type="number" style={{ width: 110 }} value={amt[p.id + 'a'] ?? p.amount} onChange={(e) => setAmt({ ...amt, [p.id + 'a']: e.target.value })} />
            <input type="text" placeholder="Note (needed to reject)" style={{ width: 170 }} value={amt[p.id + 'n'] ?? ''} onChange={(e) => setAmt({ ...amt, [p.id + 'n']: e.target.value })} />
            <button className="btn sm primary" onClick={() => act(p, 'approve')}>Approve</button><button className="btn sm" onClick={() => act(p, 'reject')}>Reject</button></div>}
        </div>) : <span className="mute">No proof uploaded</span>}</td></tr>; })}
      {!its.length && <tr><td colSpan={4} className="mute">Nothing declared.</td></tr>}
    </tbody></table></div> : <p className="mute">New regime: no section deductions to verify.</p>}
    {dl.finalAt && <div className="note" style={{ marginTop: 12 }}>Finalised {fmtD(String(dl.finalAt).slice(0, 10))}. Monthly TDS now uses approved proofs{dl.finalDiff > 0 ? `, so ${inr(dl.finalDiff)} more tax is spread over the months left` : ''}.</div>}
  </Modal>;
}

/** Payroll: Tax and Form 16 (prototype vTax16): deductor details, TDS by month (192 and 194J), quarterly returns, Form 16. */
export function TaxAdmin() {
  const st = usePayState();
  const d = useLoad(() => api('/tds'), []);
  const f16 = useLoad(() => api('/form16'), []);
  const [co, setCo] = useState<Record<string, string> | null>(null);
  const [rec, setRec] = useState<Record<number, string>>({});
  const [dep, setDep] = useState<any>(null);
  const [view, setView] = useState<any>(null);
  const [page, setPage] = useState(1);
  if (d.error || f16.error) return <div className="note bad">{d.error || f16.error}</div>;
  if (!d.data || !f16.data || !st.data) return <Loading error={st.error} />;
  const x = d.data, F = f16.data, ap = x.canEdit, ex = x.canExport;
  const dc = x.deductor || {};
  const c: Record<string, string> = co ?? { tan: dc.tan || '', pan: dc.pan || '', rp: dc.responsible_name || '', rpd: dc.responsible_desig || '' };
  const ok = !!(dc.tan && dc.pan);
  const open = st.data.openYm, cm = fyMonths(x.fy).filter((m) => m <= open);
  const reload = () => { d.reload(); f16.reload(); };
  const saved: Record<string, string> = { tan: dc.tan || '', pan: dc.pan || '', rp: dc.responsible_name || '', rpd: dc.responsible_desig || '' };
  const saveCo = async (k: string) => {
    if (!co || (c[k] || '').trim() === saved[k]) return;
    try {
      const v = k === 'tan' || k === 'pan' ? c[k].trim().toUpperCase() : c[k].trim();
      await api('/tds/deductor', { method: 'PUT', body: { [k]: v } });
      toast('Deductor details updated'); setCo(null); reload();
    } catch (e: any) { toast(e.message); }
  };
  const saveRec = async (q: number) => { try { await api(`/tds/return/24Q/${q}?fy=${x.fy}`, { method: 'PUT', body: { receipt: (rec[q] ?? '').trim() } }); toast('Saved'); d.reload(); } catch (e: any) { toast(e.message); } };
  const saveDep = async () => {
    if (!dep.date || !/^\d{7}$/.test(dep.bsr) || !/^\d{5}$/.test(dep.challan)) { toast('Enter the date, a 7 digit BSR code and a 5 digit challan number'); return; }
    try { await api(`/tds/deposit/${dep.kind}/${dep.ym}`, { method: 'PUT', body: { date: dep.date, bsr: dep.bsr, challan: dep.challan } }); toast('Deposit recorded'); setDep(null); d.reload(); } catch (e: any) { toast(e.message); }
  };
  const release = async (on: boolean) => {
    try { const r = await api(`/form16/release?fy=${F.fy}`, { body: { on } }); toast(!on ? 'Form 16 hidden from employees' : r.final ? 'Final Form 16 released to employees' : 'Form 16 released to employees'); f16.reload(); } catch (e: any) { toast(e.message); }
  };
  const allDl = async () => { try { const list = await api<any[]>(`/form16/all?fy=${F.fy}`); saveF16(list, `form16-all-${F.fy}.html`); } catch (e: any) { toast(e.message); } };
  const monthRow = (kind: 's' | 'f', m: string) => {
    const r = x.months.find((y: any) => y.kind === kind && y.ym === m), amt = r?.amount ?? 0, dp = r?.deposit;
    return <tr key={kind + m}><td>{ymLabel(m)}</td><td className="num">{r?.people ?? 0}</td><td className="num">{inr(amt)}</td><td>{fmtD(r?.due ?? dueDate(m))}</td>
      <td>{amt === 0 ? <span className="mute">Nil</span> : dp ? <Pill k="ok">Deposited {fmtD(dp.date)}</Pill> : r?.status === 'Before go-live' ? <Pill k="n">Before go-live</Pill> : <Pill k="warn">{r?.status === 'Overdue' ? 'Overdue' : 'Due'}</Pill>}</td>
      <td>{dp ? `${dp.bsr} · ${dp.challan}` : ''}</td>
      <td>{ap && amt > 0 && <button className="btn" onClick={() => setDep({ kind, ym: m, date: dp?.date ?? today(), bsr: dp?.bsr ?? '', challan: dp?.challan ?? '' })}>{dp ? 'Edit' : 'Record deposit'}</button>}</td></tr>;
  };
  const head = <thead><tr><th>Month</th><th className="num">People</th><th className="num">TDS deducted</th><th>Deposit by</th><th>Status</th><th>Challan</th><th></th></tr></thead>;
  const qrow = (form: '24Q' | '26Q', lab: string) => [1, 2, 3, 4].map((k) => {
    const qd = x.quarters.find((y: any) => y.form === form && y.quarter === k) || {}, pend = cm.some((m) => qOf(m) === k);
    return <tr key={form + k}><td>{lab} · {QN[k]}</td><td className="num">{inr(qd.amount ?? 0)}</td>
      <td>{form === '24Q' && <input value={rec[k] ?? qd.receipt ?? ''} placeholder="Receipt number after filing" disabled={!ap} style={{ minWidth: 190, width: 'auto' }}
        onChange={(e) => setRec({ ...rec, [k]: e.target.value })} onBlur={() => rec[k] != null && rec[k] !== (qd.receipt ?? '') && saveRec(k)} />}</td>
      <td>{ex && pend && <button className="btn" onClick={() => download(`/tds/return/${form}/${k}/sheet?fy=${x.fy}`, `${form}-${x.fy}-Q${k}.xlsx`).catch((e) => toast(e.message))}>Download return data</button>}</td></tr>;
  });
  const rows: any[] = F.rows, pages = Math.max(1, Math.ceil(rows.length / 50)), pg = Math.min(page, pages), slice = rows.slice((pg - 1) * 50, pg * 50);
  const running = open < `${+F.fy.slice(0, 4) + 1}-03`;
  const inp = (k: string, l: string, ph: string, on: boolean) => <Field label={l}><input value={c[k]} placeholder={ph} disabled={!on} onChange={(e) => setCo({ ...c, [k]: k === 'tan' || k === 'pan' ? e.target.value.toUpperCase() : e.target.value })} onBlur={() => saveCo(k)} /></Field>;
  return <>
    {!ok && <div className="note bad" style={{ marginBottom: 14 }}>Add the company TAN and PAN below before you download Form 16 or return data.</div>}
    <div className="card" style={{ marginBottom: 14 }}><h3>Deductor details</h3><div className="form" style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(180px,1fr))' }}>
      {inp('tan', 'TAN', 'DELS12345A', ap)}{inp('pan', 'PAN of the company', 'AAACS1234B', ap)}{inp('rp', 'Person responsible for deduction', 'Name', ap)}{inp('rpd', 'Designation', 'Designation', ap)}
    </div></div>
    <div className="grid" style={{ alignItems: 'start', marginBottom: 14 }}>
      <div className="card"><h3>Tax on salary, section 192</h3><div className="tw"><table>{head}<tbody>{cm.map((m) => monthRow('s', m))}</tbody></table></div>
        <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>TDS is worked out every month: yearly tax on the chosen regime, less tax already deducted, divided by the months left. A new declaration or a proof review corrects it from the next run. Tax is deducted only when a payroll month is published.</p></div>
      <div className="card"><h3>Tax on retainer fees, section 194J</h3><div className="tw"><table>{head}<tbody>{cm.map((m) => monthRow('f', m))}</tbody></table></div></div>
    </div>
    <OpeningTds fy={x.fy} onSaved={reload} />
    <div className="card" style={{ marginBottom: 14 }}><h3>Quarterly returns</h3><div className="tw"><table><thead><tr><th>Return</th><th className="num">Tax deducted</th><th>Receipt number</th><th></th></tr></thead><tbody>
      {qrow('24Q', '24Q salary')}{qrow('26Q', '26Q retainers')}</tbody></table></div></div>
    <div className="card">
      <div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap' }}>
        <div><h3 style={{ margin: 0 }}>Form 16, FY {F.fy}</h3><div className="mute" style={{ fontSize: 12 }}>{running ? 'The year is still running, so these are provisional. Final Form 16 is ready after the March run is published and the Q4 return is filed.' : 'Year complete.'}</div></div>
        <div className="row" style={{ gap: 8, alignItems: 'center' }}><span className="mute" style={{ fontSize: 12.5 }}>Employees can see their Form 16</span>
          <Switch on={!!F.released} disabled={!F.canRelease || !ok} label="Employees can see their Form 16" onChange={() => release(!F.released)} />
          {ex && ok && rows.length > 0 && <button className="btn primary" onClick={allDl}>Download all (one printable file)</button>}</div>
      </div>
      <div className="tw"><table><thead><tr><th>Employee</th><th>Regime</th><th className="num">Yearly tax</th><th className="num">Deducted so far</th><th className="num">Still to deduct</th><th>Basis</th><th></th></tr></thead><tbody>
        {slice.map((r) => <tr key={r.id}><td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.id}</div></td><td>{r.regime}</td><td className="num">{inr(r.annual)}</td><td className="num">{inr(r.done)}</td>
          <td className="num">{inr(r.toDeduct ?? Math.max(0, r.annual - r.done - (r.prevTds || 0)))}</td><td>{r.basis === 'approved' ? 'Approved proofs' : 'Declaration'}</td><td><button className="btn" onClick={() => setView(r)}>View</button></td></tr>)}
        {!rows.length && <tr><td colSpan={7} className="mute" style={{ padding: 18, textAlign: 'center' }}>No permanent employees in this view</td></tr>}
      </tbody></table></div>
      {rows.length > 50 && <div className="row" style={{ gap: 8, marginTop: 10 }}><button className="btn" disabled={pg <= 1} onClick={() => setPage(pg - 1)}>Previous</button><span className="mute" style={{ fontSize: 12.5, alignSelf: 'center' }}>Page {pg} of {pages}</span><button className="btn" disabled={pg >= pages} onClick={() => setPage(pg + 1)}>Next</button></div>}
    </div>
    {dep && <Modal onClose={() => setDep(null)}><h2>Record deposit, {ymLabel(dep.ym)}</h2>
      <div className="form"><Field label="Date of deposit"><input type="date" value={dep.date} onChange={(e) => setDep({ ...dep, date: e.target.value })} /></Field>
        <Field label="BSR code of the bank"><input value={dep.bsr} placeholder="7 digits" onChange={(e) => setDep({ ...dep, bsr: e.target.value.trim() })} /></Field>
        <Field label="Challan serial number"><input value={dep.challan} placeholder="5 digits" onChange={(e) => setDep({ ...dep, challan: e.target.value.trim() })} /></Field></div>
      <div className="row" style={{ marginTop: 16, gap: 8 }}><button className="btn primary" onClick={saveDep}>Save</button><button className="btn" onClick={() => setDep(null)}>Cancel</button></div></Modal>}
    {view && <Form16Modal id={view.id} fy={F.fy} title={`Form 16, ${view.name}`} canDownload={ex} blocked={ok ? '' : 'Add the company TAN and PAN first'} onClose={() => setView(null)} />}
  </>;
}

/** Opening TDS: salary TDS already deducted this year before the first payroll on the system (loaded from a file,
 *  checked first, then saved), so the monthly TDS spreads only the tax still due. */
function OpeningTds({ fy, onSaved }: { fy: string; onSaved: () => void }) {
  const d = useLoad(() => api(`/tds/opening?fy=${fy}`), [fy]);
  const [file, setFile] = useState<File | null>(null);
  const [chk, setChk] = useState<any>(null);
  if (!d.data) return d.error ? null : <Loading />;
  const x = d.data, rows: any[] = x.rows;
  const send = async (action: 'check' | 'apply') => {
    if (!file) { toast('Choose the filled file first'); return; }
    try {
      const r = await postForm(`/tds/opening/${action}`, {}, file);
      if (action === 'apply') { toast(`Opening TDS saved for ${r.ok} row${r.ok === 1 ? '' : 's'}`); setChk(null); setFile(null); d.reload(); onSaved(); } else setChk(r);
    } catch (e: any) { toast(e.message); }
  };
  const people = new Set(rows.map((r) => r.id)).size;
  return <div className="card" style={{ marginBottom: 14 }}><h3>Opening TDS, before go-live</h3>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Salary TDS already deducted this year before the first payroll on this system ({ymLabel(x.openYm)} is the open month). Load it before the first run, so the monthly TDS spreads only the tax still due. Months with a payroll run take their TDS from payroll.</p>
    <p style={{ fontSize: 13, margin: '0 0 10px' }}>{rows.length ? <>{inr(x.total)} for {people} employee{people === 1 ? '' : 's'} ({rows.length} month{rows.length === 1 ? '' : 's'}) on record.</> : <span className="mute">No opening TDS loaded for FY {fy}.</span>}</p>
    {x.canEdit && <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
      <button className="btn" onClick={() => download(`/tds/opening/template.xlsx?fy=${fy}`, `opening-tds-${fy}.xlsx`).catch((e) => toast(e.message))}>Download template</button>
      <input type="file" accept=".xlsx,.csv" onChange={(e) => { setFile(e.target.files?.[0] ?? null); setChk(null); }} />
      <button className="btn" disabled={!file} onClick={() => send('check')}>Check file</button>
      {chk && chk.problems === 0 && <button className="btn primary" onClick={() => send('apply')}>Save {chk.ok} row{chk.ok === 1 ? '' : 's'} ({inr(chk.total)})</button>}
    </div>}
    {chk && <div className="tw" style={{ marginTop: 10, maxHeight: 280, overflow: 'auto' }}><table><thead><tr><th>Line</th><th>Employee</th><th>Month</th><th className="num">TDS deducted</th><th>Check</th></tr></thead><tbody>
      {chk.rows.map((r: any) => <tr key={r.line}><td>{r.line}</td><td>{r.name || r.id} <span className="mono mute">{r.id}</span></td><td>{r.ym ? ymLabel(r.ym) : '—'}</td><td className="num">{inr(r.amount)}</td>
        <td>{r.problem ? <Pill k="bad">{r.problem}</Pill> : <Pill k="ok">OK</Pill>}</td></tr>)}
    </tbody></table></div>}
    {chk && chk.problems > 0 && <div className="note bad" style={{ marginTop: 8 }}>{chk.problems} row{chk.problems === 1 ? ' has a problem' : 's have problems'}. Fix the file and check it again. Nothing is saved until every row is OK.</div>}
  </div>;
}

/** Saves one or more Form 16 as a single printable HTML file (prototype f16Page). */
function saveF16(list: any[], fileName: string) {
  const body = list.map((f, i) => `<div style="${i < list.length - 1 ? 'page-break-after:always' : ''}">${renderToStaticMarkup(<Form16 f={f} />)}</div>`).join('');
  const html = `<!doctype html><html><head><meta charset="utf-8"><title>Form 16</title><style>body{font:13px/1.5 Arial,Helvetica,sans-serif;color:#111;max-width:800px;margin:24px auto;padding:0 16px}h1{font-size:20px;margin:0 0 4px}table{width:100%;border-collapse:collapse;margin:6px 0 14px}td,th{border:1px solid #d4d4d4;padding:5px 8px;text-align:left;vertical-align:top}th{background:#f3f4f6}</style></head><body>${body}</body></html>`;
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([html], { type: 'text/html' })); a.download = fileName; a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

export function Form16Modal({ id, fy, onClose, title = 'Form 16', canDownload = true, blocked = '' }: { id: string; fy: string; onClose: () => void; title?: string; canDownload?: boolean; blocked?: string }) {
  const d = useLoad(() => api(`/form16/${id}?fy=${fy}`), [id, fy]);
  return <Modal wide onClose={onClose}>
    <div className="row sp" style={{ marginBottom: 10 }}><h2 style={{ margin: 0 }}>{title}</h2><div className="row" style={{ gap: 8 }}>{d.data && canDownload && <button className="btn primary" onClick={() => (blocked ? toast(blocked) : saveF16([d.data], `form16-${d.data.employee.id}-${fy}.html`))}>Download</button>}<button className="btn" onClick={onClose}>Close</button></div></div>
    {d.error && <div className="note bad">{d.error}</div>}
    {d.data ? <div style={{ fontSize: 13, lineHeight: 1.5, maxHeight: '68vh', overflow: 'auto' }}><Form16 f={d.data} /></div> : !d.error && <Loading />}
  </Modal>;
}

/** Form 16 Part A and Part B (prototype f16Html). */
function Form16({ f }: { f: any }) {
  const co = f.employer || {}, e = f.employee, b = f.partB, old = f.regime === 'Old', prov = !f.final;
  const R = (a: string, v: any) => <tr key={a}><td>{a}</td><td>{typeof v === 'number' ? inr(v) : v}</td></tr>;
  const salary = Math.max(0, b.gross - b.hra - b.standard), other = (b.other || 0) + (b.prevIncome || 0);
  return <div>
    <h1>Form No. 16{prov ? ' (provisional)' : ''}</h1>
    <p style={{ margin: '0 0 12px', color: '#555' }}>Certificate under section 203 of the Income-tax Act, 1961 for tax deducted at source on salary · Certificate no. {f.cert}</p>
    <h2 style={{ fontSize: 15 }}>Part A</h2>
    <table><tbody>
      <tr><td>Employer</td><td>{co.legal_name}<br />{co.address}</td></tr>
      {R('PAN and TAN of the deductor', `${(co.pan || '').toUpperCase()} · ${(co.tan || '').toUpperCase()}`)}{R('Employee', `${e.name} (${e.id})`)}{R('PAN of the employee', e.pan || '')}
      {R('Assessment year · Financial year', `${f.ay} · ${f.fy}`)}{R('Period with the employer', `${ymLabel(f.period[0])} to ${ymLabel(f.period[1])}`)}
    </tbody></table>
    <h3 style={{ fontSize: 14 }}>Summary of tax deducted and deposited</h3>
    <table><thead><tr><th>Quarter</th><th>Receipt number of the return</th><th>Amount paid or credited</th><th>Tax deducted</th><th>Tax deposited</th></tr></thead><tbody>
      {f.quarters.map((q: any) => <tr key={q.quarter}><td>{QN[q.quarter]}</td><td>{q.receipt || 'Pending'}</td><td>{inr(q.paid)}</td><td>{inr(q.tds)}</td><td>{inr(q.tds)}</td></tr>)}
    </tbody></table>
    <h3 style={{ fontSize: 14 }}>Challan details</h3>
    {f.challans.length ? <table><thead><tr><th>Month</th><th>Tax deposited</th><th>BSR code</th><th>Date of deposit</th><th>Challan serial number</th></tr></thead><tbody>
      {f.challans.map((c: any) => <tr key={c.ym}><td>{ymLabel(c.ym)}</td><td>{inr(c.tds)}</td><td>{c.bsr || 'Pending'}</td><td>{c.date ? fmtD(c.date) : 'Pending'}</td><td>{c.challan || 'Pending'}</td></tr>)}
    </tbody></table> : <p>No tax was deducted in this year.</p>}
    <h2 style={{ fontSize: 15, marginTop: 24 }}>Part B</h2>
    <p style={{ margin: '0 0 6px' }}>Tax regime chosen: <b>{old ? 'Old regime' : 'New regime (section 115BAC)'}</b>{f.basis === 'approved' ? ' · on approved proofs' : f.basis ? ' · on declared investments' : ''}</p>
    <table><tbody>
      {R('1. Gross salary, section 17(1)', b.gross)}{old ? R('2. Less: house rent allowance exempt under section 10(13A)', b.hra) : R('2. Less: allowances exempt under section 10', 0)}
      {R('3. Less: standard deduction under section 16(ia)', b.standard)}{R('4. Income chargeable under the head Salaries', salary)}{R('5. Add: other income and income from the previous employer', other)}{R('6. Gross total income', salary + other)}
      {old ? <>{b.vi.map((v: any) => <tr key={v.section}><td>&nbsp;&nbsp;Chapter VI-A: {v.section}</td><td>{inr(v.amount)}</td></tr>)}{R('7. Total deductions under Chapter VI-A (after limits)', b.viTotal)}</> : R('7. Deductions under Chapter VI-A (not available in the new regime)', 0)}
      {R('8. Total taxable income', b.taxable)}{R('9. Tax on total income, after rebate under section 87A', b.tax)}{R('10. Health and education cess at 4%', b.cess)}{R('11. Total tax payable', b.total)}
      {R('12. Less: tax deducted by the previous employer', b.prev)}{R('13. Less: tax deducted by this employer', b.done)}{R(b.balance >= 0 ? '14. Balance tax payable' : '14. Tax refundable', Math.abs(b.balance))}
    </tbody></table>
    <p style={{ marginTop: 22 }}>I, {co.responsible_name || 'the person responsible for deduction'}{co.responsible_desig ? ', ' + co.responsible_desig : ''}, certify that the sum of {inr(b.done)} has been deducted at source and paid to the credit of the Central Government, and that the details above are true and correct.{prov && <b> This is a provisional statement for a year still in progress. Tax for the remaining months is projected, not deducted.</b>}<br /><br />{co.place ? `Place: ${co.place} · ` : ''}Date: {fmtD(today())}<br />Signature of the person responsible for deduction</p>
  </div>;
}

/** My Form 16 (prototype vF16Ess): the current year, then any earlier released years. */
export function MyForm16() {
  const d = useLoad(() => api('/me/form16'), []);
  const [fy, setFy] = useState('');
  if (!d.data) return <Loading error={d.error} />;
  const cur = curFy(), years: any[] = d.data.years ?? [];
  if (!d.data.applies) return <div className="card"><h3>Form 16, FY {cur}</h3><p className="ink2" style={{ margin: 0 }}>No Form 16 for this year.</p></div>;
  return <>
    {!years.some((y) => y.fy === cur) && <div className="card" style={{ marginBottom: 14 }}><h3>Form 16, FY {cur}</h3><p className="ink2" style={{ margin: 0 }}>Your Form 16 is not released yet. Payroll releases it after the year-end return is filed. Until then your payslips show the tax deducted each month.</p></div>}
    {years.map((y) => <MyYear key={y.fy} y={y} onView={() => setFy(y.fy)} />)}
    {fy && <Form16Modal id="me" fy={fy} canDownload={false} onClose={() => setFy('')} />}
  </>;
}

function MyYear({ y, onView }: { y: any; onView: () => void }) {
  const d = useLoad(() => api(`/form16/me?fy=${y.fy}`).catch(() => null), [y.fy]);
  const f = d.data;
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp" style={{ marginBottom: 10 }}><div><h3 style={{ margin: 0 }}>Form 16, FY {y.fy}</h3><div className="mute" style={{ fontSize: 12.5 }}>{y.final ? 'Final' : 'Provisional, the year is still running'}{f ? ` · tax deducted ${inr(f.partB.done)} of ${inr(f.partB.total)}` : ''}</div></div>
      <div className="row" style={{ gap: 8 }}><button className="btn" onClick={onView}>View</button><button className="btn primary" disabled={!f} onClick={() => f && saveF16([f], `form16-${f.employee.id}-${y.fy}.html`)}>Download</button></div></div>
    <p className="mute" style={{ fontSize: 12.5, margin: 0 }}>File this with your income tax return. Check it against your tax credit statement (Form 26AS) on the income tax portal.</p>
  </div>;
}
