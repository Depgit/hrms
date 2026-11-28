import { useEffect, useState } from 'react';
import { api } from '../api';
import { Pill, toast, useLoad } from './ui';

/** The all-inclusive gross (owner, 26 Nov 2026; server api/allin.go, calc/allin.go): from a month the admin picks, the
 *  monthly gross offered (G) also pays the employer's PF, ESI, PF admin + EDLI and LWF, with the statutory bonus inside G
 *  when G is at most the bonus ceiling; variable pay and reimbursement are paid on top, pro-rata on paid days. */
export type AllInView = { from: string; label?: string; open: string; can: boolean };

/** The setting ('' when off). */
export function useAllIn(): string {
  const d = useLoad(() => api<AllInView>('/comp/all-in').catch(() => ({ from: '' } as AllInView)), []);
  return d.data?.from ?? '';
}

/** How a screen names the monthly gross: the all-inclusive G when the rule is set. */
export const grossWord = (allIn: string, base = 'Monthly gross') => (allIn ? `${base} offered (all-inclusive G)` : base);

/** Compensation › Salary structure: the "All-inclusive gross from month" setting (pay.setup at Edit; audited). */
export function AllInSetting({ v, onSaved }: { v?: AllInView; onSaved: () => void }) {
  const [m, setM] = useState(v?.from ?? '');
  useEffect(() => setM(v?.from ?? ''), [v?.from]);
  if (!v) return null;
  const save = async (from: string) => {
    try { await api('/comp/all-in', { method: 'PUT', body: { from } }); toast(from ? `All-inclusive gross from ${from}` : 'All-inclusive gross switched off'); onSaved(); } catch (e: any) { toast(e.message); setM(v.from); }
  };
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>All-inclusive gross from month</h3>{v.from ? <Pill k="ok">On from {v.label ?? v.from}</Pill> : <Pill k="n">Off</Pill>}</div>
    <p className="ink2" style={{ fontSize: 12.5, margin: '8px 0 10px' }}>From this payroll month the monthly gross offered (G) is the gross salary: Basic, HRA, Special allowance and, when G is at most the bonus ceiling, a Statutory bonus line, adding up to G (the bonus is inside G, never on top).
      The employer PF (with EPS), ESI, EDLI and LWF are deducted from the employee's salary with the employee's own PF, ESI and LWF. The company bears the EPFO admin charges, gratuity, PL encashment (and medical insurance, if any): CTC = G + those + variable pay target + reimbursement.
      Variable pay (target × paid days ÷ days, automatically every month, no HR entry) and reimbursement are paid on top. Applies to everyone from the month chosen; earlier months and approved or published payrolls never change.</p>
    <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
      <input type="month" value={m} disabled={!v.can} onChange={(e) => setM(e.target.value)} />
      {v.can && <button className="btn primary" disabled={m === v.from || !m} onClick={() => save(m)}>Save</button>}
      {v.can && v.from && <button className="btn" onClick={() => save('')}>Switch off</button>}
      <span className="mute" style={{ fontSize: 12 }}>Open payroll month: {v.open}. A month already approved or published cannot be changed.</span>
    </div>
  </div>;
}
