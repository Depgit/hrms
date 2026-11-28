import { useState } from 'react';
import { api, inr } from '../api';
import { Loading, Pill, toast, usePage, useLoad } from '../components/ui';
import { CYCLE } from '../components/PayEntry';
import { ymLabel } from './PayrollRun';

/** Payroll › Variable pay (pay components, server paycomp.go): the people on a structure made from components, each
 *  variable component's target and payout cycle, and the amount HR enters when a payout is due. Not entered: not paid,
 *  and listed in the payroll checks. In bulk: Bulk update › Salary breakup (the payout columns). */
export default function VariablePay() {
  const [ym, setYm] = useState('');
  const [q, setQ] = useState('');
  const [dueOnly, setDueOnly] = useState(true);
  const d = useLoad(() => api(`/pay/variable${ym ? `?ym=${ym}` : ''}`), [ym]);
  const x = d.data;
  const rows: any[] = (x?.rows ?? []).filter((r: any) => (!q || `${r.employeeId} ${r.name}`.toLowerCase().includes(q.toLowerCase())) && (!dueOnly || r.comps.some((c: any) => c.due)));
  const pg = usePage(rows, 100);
  if (!x) return <Loading error={d.error} />;
  const save = async (r: any, c: any, v: string) => {
    const cur = c.paid == null ? '' : String(c.paid);
    if (v === cur) return;
    try { await api('/pay/variable', { method: 'PUT', body: { ym: x.ym, employeeId: r.employeeId, code: c.code, amount: v === '' ? null : +v } }); toast('Saved'); d.reload(); } catch (e: any) { toast(e.message); }
  };
  // The target, as a % of the fixed gross or ₹ a month (chosen each time), from the month shown.
  const saveTarget = async (r: any, mode: string, v: string) => {
    const cur = r.target ? (r.target.mode === 'pct' ? r.target.pct : r.target.total) : -1;
    if (v === '' || (+v === cur && mode === (r.target?.mode ?? 'amount'))) return;
    try { await api('/pay/variable/target', { method: 'PUT', body: { employeeId: r.employeeId, fromMonth: x.ym, mode, value: +v, reason: 'Variable pay screen' } }); toast(`Target from ${ymLabel(x.ym)} saved`); d.reload(); } catch (e: any) { toast(e.message); }
  };
  const TargetCell = ({ r }: { r: any }) => {
    const [mode, setMode] = useState<string>(r.target?.mode ?? 'pct');
    const [val, setVal] = useState<string>(r.target ? String(r.target.mode === 'pct' ? r.target.pct : r.target.total) : '');
    return <div className="row" style={{ gap: 4, justifyContent: 'flex-end' }}>
      <select value={mode} onChange={(e) => { setMode(e.target.value); if (val !== '') saveTarget(r, e.target.value, val); }}><option value="pct">% of gross</option><option value="amount">₹ a month</option></select>
      <input type="number" min={0} style={{ width: 90, textAlign: 'right' }} value={val} placeholder="None" onChange={(e) => setVal(e.target.value)} onBlur={() => saveTarget(r, mode, val)} />
      {mode === 'pct' && r.target?.mode === 'pct' && <small className="mute">{inr(r.target.monthly)}</small>}</div>;
  };
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Variable pay of a structure made from components is paid as the amount HR enters when the payout is due: the cycle is counted from each person's joining month
      (or the month their variable pay started) and the payout falls when a cycle is complete: joined in May, quarterly is paid in July, October, January and April, half-yearly in October and April, yearly in April.
      A due component with no amount is not paid and shows in the payroll checks. Someone with no target (none or 0) cannot be given a payout.
      The suggestion is the target a month × the months in the cycle; change it as needed. Enter 0 to pay nothing this cycle. The target is a % of the fixed gross or ₹ a month. In bulk: Bulk update › Salary breakup (the payout columns).</div>
    <div className="card">
      <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}>
        <div className="row" style={{ gap: 8 }}>
          <label className="field" style={{ margin: 0 }}><span>Payroll month</span><input type="month" value={x.ym} onChange={(e) => setYm(e.target.value)} /></label>
          <label className="field" style={{ margin: 0 }}><span>Search</span><input value={q} placeholder="Name or ID" onChange={(e) => setQ(e.target.value)} /></label>
          <label style={{ fontSize: 13, alignSelf: 'end' }}><input type="checkbox" checked={dueOnly} onChange={(e) => setDueOnly(e.target.checked)} /> Due this month only</label>
        </div>
        <div className="row" style={{ gap: 8 }}><Pill k="info">{x.due} due</Pill><Pill k={x.entered === x.due ? 'ok' : 'warn'}>{x.entered} entered</Pill>
          {x.due > x.entered && <Pill k="bad">{x.due - x.entered} not entered</Pill>}</div>
      </div>
      {x.allInNote && <div className="note" style={{ marginTop: 10 }}>{x.allInNote}. Nothing is entered here for these months; the payslip shows the amount.</div>}
      {!x.can && !x.allInNote && <p className="mute" style={{ fontSize: 12.5 }}>{x.ym < x.open ? `${ymLabel(x.ym)} is already paid.` : 'You can see the amounts but not enter them.'}</p>}
      <div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Employee</th><th>Structure</th><th className="num">Target a month</th><th>Component</th><th>Payout</th><th className="num">Target for the cycle</th><th className="num">Amount to pay</th></tr></thead>
        <tbody>{(pg.rows ?? []).flatMap((r: any) => r.comps.filter((c: any) => !dueOnly || c.due).map((c: any, i: number) => <tr key={r.employeeId + c.code}>
          {i === 0 && <><td rowSpan={r.comps.filter((y: any) => !dueOnly || y.due).length}><b>{r.name}</b> <span className="mono mute">{r.employeeId}</span><div className="mute" style={{ fontSize: 12 }}>{r.location} · {r.type}</div></td>
            <td rowSpan={r.comps.filter((y: any) => !dueOnly || y.due).length}>{r.structure}</td>
            <td className="num" rowSpan={r.comps.filter((y: any) => !dueOnly || y.due).length}>{x.canTarget && x.ym >= x.open
              ? <TargetCell key={JSON.stringify(r.target)} r={r} />
              : r.target ? r.target.mode === 'pct' ? `${r.target.pct}% (${inr(r.target.monthly)})` : inr(r.target.total) : <span className="mute">None</span>}</td></>}
          <td>{c.name}</td><td>{CYCLE[c.cycle] ?? 'monthly'} {c.due ? <Pill k="info">Due</Pill> : <Pill k="n">Not due</Pill>}
            {c.cycle > 1 && c.next && <div className="mute" style={{ fontSize: 12 }}>From {ymLabel(c.from)}{c.due ? '' : `, next ${ymLabel(c.next)}`}</div>}</td><td className="num">{inr(c.cycleTarget)}</td>
          <td className="num">{c.due && c.noTarget && c.paid == null ? <span className="mute">No target: set one first</span> : c.due && x.can
            ? <div className="row" style={{ gap: 4, justifyContent: 'flex-end' }}><input key={String(c.paid)} type="number" min={0} style={{ width: 110, textAlign: 'right' }} defaultValue={c.paid ?? ''} placeholder={c.cycleTarget > 0 ? `Suggest ${c.cycleTarget}` : 'Not entered'} onBlur={(e) => save(r, c, e.target.value)} />
              {c.paid == null && c.cycleTarget > 0 && <button className="btn sm" title="Target a month × the months in the cycle" onClick={() => save(r, c, String(c.cycleTarget))}>Use {inr(c.cycleTarget)}</button>}</div>
            : c.paid != null ? inr(c.paid) : <span className="mute">{c.due ? 'Not entered' : '—'}</span>}</td>
        </tr>))}
          {!rows.length && <tr><td colSpan={7} className="mute">Nobody paid in {ymLabel(x.ym)} has a variable component {dueOnly ? 'due ' : ''}on a structure made from components.</td></tr>}</tbody></table></div>
      {pg.pager}
    </div>
  </>;
}
