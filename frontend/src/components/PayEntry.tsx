import { inr } from '../api';

/** How pay is entered (pay components, server paycomp.go `payEntry`): by the fixed monthly gross or by CTC (fixed +
 *  variable a month), with the variable part as an amount or a % of the total. ₹35,000 = ₹20,000 fixed + ₹15,000 variable. */
export type PayIn = { payBy: 'gross' | 'ctc'; amount: string; varBy: 'amount' | 'pct'; var: string };

export const PAY_IN: PayIn = { payBy: 'gross', amount: '', varBy: 'amount', var: '' };

/** The fixed monthly gross and the variable a month (the server's payEntry.split). null when it does not work out. */
export function splitPay(p: PayIn): { fixed: number; variable: number } | null {
  const a = +p.amount || 0, v = +p.var || 0;
  if (a <= 0 || v < 0 || (p.varBy === 'pct' && v >= 100)) return null;
  let fixed: number, variable: number;
  if (p.payBy === 'ctc') {
    variable = p.varBy === 'pct' ? (a * v) / 100 : v;
    fixed = a - variable;
  } else {
    fixed = a;
    variable = p.varBy === 'pct' ? (a * v) / (100 - v) : v;
  }
  const r2 = (x: number) => Math.round(x * 100) / 100;
  return fixed > 0 ? { fixed: r2(fixed), variable: r2(variable) } : null;
}

/** The entry fields: by gross or CTC, the amount, and the variable part as ₹ or %. */
export function PayEntryFields({ p, set, variable = true }: { p: PayIn; set: (p: PayIn) => void; variable?: boolean }) {
  const s = splitPay(p);
  return <>
    <label className="field"><span>Enter pay as</span><select value={p.payBy} onChange={(e) => set({ ...p, payBy: e.target.value as PayIn['payBy'] })}>
      <option value="gross">Fixed monthly gross</option><option value="ctc">CTC a month (fixed + variable)</option></select></label>
    <label className="field"><span>{p.payBy === 'ctc' ? 'CTC a month (fixed + variable), ₹' : 'Fixed monthly gross, ₹'}</span>
      <input type="number" min={0} value={p.amount} onChange={(e) => set({ ...p, amount: e.target.value })} /></label>
    {variable && <>
      <label className="field"><span>Variable part as</span><select value={p.varBy} onChange={(e) => set({ ...p, varBy: e.target.value as PayIn['varBy'] })}>
        <option value="amount">₹ a month</option><option value="pct">% of the total</option></select></label>
      <label className="field"><span>{p.varBy === 'pct' ? 'Variable, % of fixed + variable' : 'Variable, ₹ a month'}</span>
        <input type="number" min={0} value={p.var} onChange={(e) => set({ ...p, var: e.target.value })} /></label>
    </>}
    {s && <p className="mute" style={{ fontSize: 12.5, margin: 0, gridColumn: '1 / -1' }}>Fixed {inr(s.fixed)} + variable {inr(s.variable)} = {inr(s.fixed + s.variable)} a month.</p>}
  </>;
}

/** Basic (+ DA) as a % of total pay (fixed gross + variable): the labour codes count wages as at least half of it. */
export function wageShare(c: any): { pct: number; ok: boolean } | null {
  const total = (+c?.gross || 0) + (+c?.variable || 0);
  if (!c || total <= 0) return null;
  const pct = Math.round(((+c.basic || 0) / total) * 10000) / 100;
  return { pct, ok: pct >= 50 };
}

/** The labour-code wage warning under a breakup (a warning only, never a block). */
export function WageNote({ c }: { c: any }) {
  const w = wageShare(c);
  if (!w || w.ok) return null;
  return <div className="note warn" style={{ marginTop: 10, fontSize: 12.5 }}>Basic + DA is {w.pct}% of total pay ({inr(c.basic)} of {inr((+c.gross || 0) + (+c.variable || 0))}). The labour codes count
    wages as at least 50% of total pay; the part above half is added back to wages for PF, gratuity and other dues. Saving is allowed.</div>;
}

export const CYCLE: Record<number, string> = { 1: 'monthly', 3: 'quarterly (every 3 months from joining)', 6: 'half-yearly (every 6 months from joining)', 12: 'yearly (every 12 months from joining)' };

/** Variable pay target (17 Nov 2026): the admin chooses each time how it is given, a % of the fixed monthly gross or ₹ a
 *  month. A % shows its ₹ equivalent on the gross. It is a target: HR pays it by entering the amount when it is due. */
export type VarMode = 'pct' | 'amount';
export function VarTargetField({ mode, value, gross, onChange, label = 'Variable pay target', labels }: { mode: VarMode; value: string; gross?: number; onChange: (mode: VarMode, value: string) => void; label?: string; labels?: [string, string] }) {
  const v = +value || 0;
  return <>
    <label className="field"><span>{labels?.[0] ?? `${label}, given as`}</span><select value={mode} onChange={(e) => onChange(e.target.value as VarMode, value)}>
      <option value="pct">% of gross</option><option value="amount">₹ a month</option></select></label>
    <label className="field"><span>{labels?.[1] ?? (mode === 'pct' ? 'Variable target, % of the fixed monthly gross' : 'Variable target, ₹ a month')}</span>
      <input type="number" min={0} max={mode === 'pct' ? 100 : undefined} value={value} onChange={(e) => onChange(mode, e.target.value)} />
      {mode === 'pct' && value !== '' && <small className="mute">{gross && gross > 0 ? `= ${inr(Math.round(gross * v) / 100)} a month on a gross of ${inr(gross)}` : 'Enter the gross to see the ₹ amount'}</small>}</label>
  </>;
}
