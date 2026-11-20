import { useState, type ReactNode } from 'react';
import { api, inr } from '../api';
import { Field, useLoad } from '../components/ui';
import { PAY_IN, PayEntryFields, VarTargetField, WageNote, splitPay, type PayIn, type VarMode } from '../components/PayEntry';
import { Statement } from './Compensation';

// Prototype MONTHS: the financial year order.
const MONTHS: [number, string][] = [[4, 'April'], [5, 'May'], [6, 'June'], [7, 'July'], [8, 'August'], [9, 'September'], [10, 'October'], [11, 'November'], [12, 'December'], [1, 'January'], [2, 'February'], [3, 'March']];
const MNAME = ['', 'January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
const VCT: Record<number, string> = { 1: 'every month', 3: 'every 3 months', 6: 'every 6 months', 12: 'once a year' };
// What each employment type gets (the server's default type rules; /comp returns the same when the role may read it).
const TYPES: Record<string, { statutory: boolean; grat: boolean; plEnc: boolean; variable: boolean; ded: string }> = {
  Permanent: { statutory: true, grat: true, plEnc: true, variable: true, ded: 'all' },
  Retainer: { statutory: false, grat: false, plEnc: false, variable: false, ded: 'tds' },
  Contractual: { statutory: false, grat: false, plEnc: false, variable: true, ded: 'none' },
};

/** Salary calculator (prototype vPay 'calc', pcOut, breakHtml): HR enters gross only; the breakup comes from the structure. */
export default function Salary() {
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const grades = useLoad(() => api<any[]>('/masters/grade?status=Active').catch(() => []), []);
  const structs = useLoad(() => api<any[]>('/salary/structures').catch(() => []), []);
  const comp = useLoad(() => api('/comp').catch(() => null), []);
  const [p, setP] = useState<Record<string, any>>({ type: 'Permanent', city: '', grade: 'G1', structureId: '', joinMonth: new Date().getMonth() + 1, month: new Date().getMonth() + 1, gross: 19900, inc: 0, uan: 'Yes', varPct: '', varCycle: '', varMode: 'pct', varVal: '', meal: 0, accom: 0 });
  const set = (k: string, v: any) => setP({ ...p, [k]: v });
  // Pay entered by CTC (fixed + variable) instead of the gross: the split gives the gross and the variable part.
  const [pe, setPe] = useState<PayIn>({ ...PAY_IN, payBy: 'ctc', amount: '35000', var: '15000' });
  const [byCtc, setByCtc] = useState(false);
  const city = p.city || cities.data?.[0]?.id || '';
  const cityName = cities.data?.find((c) => c.id === city)?.name ?? '';
  const loc = locs.data?.find((l) => l.city_id === city);
  const active = (structs.data ?? []).filter((x) => x.status === 'Active');
  const sid = p.structureId || active.find((x) => x.is_default)?.id || active[0]?.id || '';
  const S_ = (structs.data ?? []).find((x) => x.id === sid)?.rates ?? {};
  const T = { ...(TYPES[p.type] ?? TYPES.Permanent), ...(comp.data?.types?.[p.type] ?? {}) };
  const designed = !!S_.designed; // a structure made from components: variable is a target a month, paid as HR enters it
  const split = byCtc ? splitPay(pe) : null;
  const gross = split ? split.fixed : +p.gross;
  // Variable pay target: by CTC the split's ₹ a month; else as entered, a % of the fixed gross or ₹ a month.
  const vMode: VarMode = split ? 'amount' : p.varMode, vVal = split ? split.variable : p.varVal !== '' ? +p.varVal : undefined;
  const varPct = vMode === 'pct' ? vVal : !designed && split && split.fixed > 0 ? Math.round((split.variable / split.fixed) * 10000) / 100 : undefined;
  const varAmt = vMode === 'amount' ? vVal : undefined;
  const body = { type: p.type, locationId: loc?.id ?? '', grade: p.grade, gross, inc: +p.inc || 0, structureId: sid || undefined, joinMonth: +p.joinMonth, month: +p.month,
    varMode: T.variable ? vMode : undefined, varPct: T.variable && varPct !== undefined ? varPct : undefined,
    varAmt: T.variable ? varAmt : undefined, meal: T.ded === 'all' ? +p.meal || 0 : 0, accom: T.ded === 'all' ? +p.accom || 0 : 0 };
  const { data: r, error } = useLoad(() => (city && gross > 0 ? api('/salary/preview', { body }) : Promise.resolve(null)), [JSON.stringify(body)]);
  const c = r?.breakup;
  const lwfRule = comp.data?.lwf?.find((x: any) => x.state === loc?.state)?.rule;
  const lwfNote = !lwfRule ? '' : lwfRule.freq === 'monthly' ? 'Every month'
    : `${lwfRule.freq === 'year' ? 'Once a year' : 'Twice a year'}, month${String(lwfRule.months).split(',').length > 1 ? 's ' : ' '}${String(lwfRule.months).split(',').filter(Boolean).map((m: string) => MNAME[+m] ?? m).join(' and ')}`;
  const inp = (k: string, l: string) => <Field label={l}><input type="number" value={p[k]} onChange={(e) => set(k, e.target.value)} /></Field>;
  const R = (a: string, v: number, note: ReactNode = '', t = false) => <tr className={t ? 't' : ''}><td>{a}</td><td className="num">{inr(v)}</td><td className="r">{note}</td></tr>;
  const H = (a: string) => <tr className="h"><td colSpan={3}>{a}</td></tr>;
  const mo = +p.month > 0, vp = varPct !== undefined ? varPct : S_.varPct ?? 0;
  const auto = !T.statutory || !c ? '' : c.esiOn ? `ESI applies automatically, gross is at or below ${inr(S_.esiCeil)}` : c.medOn ? `Medical insurance applies automatically, gross is above ${inr(S_.esiCeil)}` : '';
  return (
    <div className="grid g2" style={{ alignItems: 'start' }}>
      <div className="card"><h3>Inputs</h3><div className="form">
        <Field label="Employment type"><select value={p.type} onChange={(e) => set('type', e.target.value)}>{Object.keys(TYPES).map((t) => <option key={t}>{t}</option>)}</select></Field>
        <Field label="City"><select value={city} onChange={(e) => set('city', e.target.value)}>{cities.data?.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select></Field>
        <Field label="Grade"><select value={p.grade} onChange={(e) => set('grade', e.target.value)}>{(grades.data?.length ? grades.data.map((g) => g.name) : ['G1', 'G2', 'G3', 'G4', 'G5', 'G6']).map((g) => <option key={g}>{g}</option>)}</select></Field>
        <Field label="Salary structure version"><select value={sid} onChange={(e) => set('structureId', e.target.value)}>{active.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select></Field>
        <Field label="Joining month"><select value={p.joinMonth} onChange={(e) => set('joinMonth', +e.target.value)}>{MONTHS.map(([n, m]) => <option key={n} value={n}>{m}</option>)}</select></Field>
        <Field label="Payroll month"><select value={p.month} onChange={(e) => set('month', +e.target.value)}>{MONTHS.map(([n, m]) => <option key={n} value={n}>{m}</option>)}</select></Field>
        <Field label="Enter pay as"><select value={byCtc ? 'ctc' : 'gross'} onChange={(e) => setByCtc(e.target.value === 'ctc')}><option value="gross">Monthly gross</option><option value="ctc">CTC a month (fixed + variable)</option></select></Field>
        {byCtc ? <PayEntryFields p={pe} set={(x) => setPe({ ...x, payBy: 'ctc' })} variable={T.variable} /> : inp('gross', 'Monthly gross')}{inp('inc', 'Increase gross by')}
        <Field label="Existing UAN available?"><select value={p.uan} onChange={(e) => set('uan', e.target.value)}>{['Yes', 'No'].map((x) => <option key={x}>{x}</option>)}</select></Field>
      </div>
        <h3 style={{ marginTop: 18 }}>Options for this employee</h3><div className="form">
          {T.variable && !byCtc && <VarTargetField mode={p.varMode} value={p.varVal} gross={gross} onChange={(m, v) => setP({ ...p, varMode: m, varVal: v })} />}
          {T.ded === 'all' && <>{inp('meal', 'Meal deduction, ₹ a month')}{inp('accom', 'Accommodation deduction, ₹ a month')}</>}
        </div>
      </div>
      <div className="card"><h3>Salary breakup · {cityName}</h3>
        {error && <div className="note bad">{error}</div>}
        {c && designed && <Statement c={c} type={p.type} std={S_} rules={comp.data?.types} lwf={lwfNote} month={+p.month} />}
        {c && !designed && <><div className="tw"><table className="stmt"><tbody>
          {p.type === 'Retainer' || c.fee > 0 ? <>{H('Retainer')}{R('Retainer fee', c.fee ?? c.gross, 'A month; TDS under section 194J')}</>
            : <>{H('Fixed salary')}{R('Basic + DA', c.basic, `${S_.basicPct}% of gross`)}{R('HRA', c.hra, `${S_.hraPct}% of basic`)}{R('Special allowance', c.sa, 'Balancing figure')}
          {R('Gross salary', c.gross, '', true)}</>}{c.bonus > 0 && R('Bonus', c.bonus, `${S_.bonusPct}% of basic up to ${inr(S_.bonusCalc || 7000)}, for gross up to ${inr(S_.bonusCeil)}`)}
          {c.variable > 0 && <>{H('Variable pay, on top of fixed')}{R('Variable pay a month (provision)', c.variable, `${vp}% of gross, paid ${VCT[c.varCycle] || 'every month'}, counted from the joining month`)}
            {mo && R('Variable paid this month', c.varPay, c.varPay ? 'Payout month' : 'Not a payout month')}</>}
          {H('Deductions')}
          {T.statutory && R('Employee PF', c.eePF, `${S_.pfEe}${S_.pfWage === 'basicSa' ? '% of basic + special allowance' : '% of basic'}${S_.pfCap ? ', wage ceiling ' + inr(S_.pfCeil) : ''}`)}
          {T.statutory && R('Employee ESI', c.eeESI, c.esiOn ? `${S_.esiEe}% of gross` : `Not applicable above ${inr(S_.esiCeil)}`)}
          {c.lwfOn && R('Labour welfare fund', c.lwfEe, lwfNote)}{c.meal > 0 && R('Meal', c.meal, 'Set by HR')}{c.accom > 0 && R('Accommodation', c.accom, 'Set by HR')}
          {c.tds > 0 && R('TDS', c.tds, `${S_.tdsRate}% of gross, set in the structure`)}
          {T.ded === 'none' && <tr><td colSpan={3} className="mute">No deductions for this employment type.</td></tr>}
          {R('Net in hand' + (mo ? '' : ' (without variable)'), c.net, 'Before income tax, if any', true)}
          {(T.statutory || T.grat || c.med > 0 || c.lwfOn) && H('Employer contribution')}
          {T.statutory && <>{R('Employer PF', c.erPF, `${S_.pfEr}%`)}{R('EPFO admin charge', c.admin, `${S_.epfoAdm}% of basic`)}{R('Employer ESI', c.erESI, `${S_.esiEr}% of gross`)}</>}
          {c.lwfOn && R('Employer labour welfare fund', c.lwfEr, lwfNote)}{c.med > 0 && R('Medical insurance', c.med, `${S_.medPct}% of gross, max ${inr(S_.medMax)}`)}{T.grat && R('Gratuity', c.grat, `${S_.grat}% of basic`)}
          {T.plEnc && <>{H('Provisions in CTC')}{R('PL encashment provision', c.plEnc, `${S_.plDays} days of basic a year`)}</>}
          {R('CTC a month', c.ctc, '', true)}{R('CTC a year', r.ctcYear, '', true)}
        </tbody></table></div>
          {auto && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>{auto}.</p>}
          <WageNote c={c} />
          <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>{p.uan === 'No' ? 'UAN will be generated at joining. ' : ''}Lines are rounded for display, so totals can differ by ₹1.</p></>}
      </div>
    </div>
  );
}
