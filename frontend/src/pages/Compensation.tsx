import { Fragment, useEffect, useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, fmtD, inr, postForm, today } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Modal, Pill, Switch, Tabs, download, toast, useLoad } from '../components/ui';
import PersonPicker from '../components/PersonPicker';
import { CYCLE, PAY_IN, PayEntryFields, VarTargetField, WageNote, splitPay, type PayIn, type VarMode } from '../components/PayEntry';

// ---------- Shared with Onboarding: the salary statement (prototype `breakHtml`) ----------

export type TypeRules = { statutory: boolean; lwf: boolean; bonus: boolean; grat: boolean; plEnc: boolean; variable: boolean; ded: string; leave: boolean; docs: boolean; leaveTypes?: string[] };
/** What each employment type switches on: the starting values (server `calc.DefaultTypeRules`). The rules in force are
 *  stored and editable: GET /comp returns them as `types`, POST /offers/check and /salary/preview as `rules`. */
export const TYPE_RULES: Record<string, TypeRules> = {
  Permanent: { statutory: true, lwf: true, bonus: true, grat: true, plEnc: true, variable: true, ded: 'all', leave: true, docs: true },
  Retainer: { statutory: false, lwf: false, bonus: false, grat: false, plEnc: false, variable: false, ded: 'tds', leave: false, docs: false },
  Contractual: { statutory: false, lwf: false, bonus: true, grat: false, plEnc: false, variable: true, ded: 'none', leave: false, docs: false },
};
export const ALL_TYPES = ['Permanent', 'Retainer', 'Contractual'];
/** Variable pay payout cycles (prototype `VC`). */
export const VC: [string, string][] = [['1', 'Monthly'], ['3', 'Every 3 months'], ['6', 'Every 6 months'], ['12', 'Once a year']];
const VCT: Record<number, string> = { 1: 'every month', 3: 'every 3 months', 6: 'every 6 months', 12: 'once a year' };
/** Midpoint of a band, rounded to ₹100 (prototype `bandFor`). */
export const bandMid = (min: number, max: number) => Math.round((min + max) / 2 / 100) * 100;

/** The note on the labour welfare fund rows: "Every month" or "Twice a year, months 6 and 12". */
export function lwfNote(rule: any): string {
  if (!rule?.on) return '';
  if (rule.freq === 'monthly') return 'Every month';
  const ms = String(rule.months || '').split(',').map((x) => +x.trim()).filter(Boolean);
  return `${rule.freq === 'year' ? 'Once a year' : 'Twice a year'}, month${ms.length > 1 ? 's ' : ' '}${ms.join(' and ')}`;
}

const pct = (n: number) => String(+(+n || 0).toFixed(2));
/** A copy of `o` without key `k`. */
const omit = <T,>(o: Record<string, T>, k: string) => { const n = { ...o }; delete n[k]; return n; };

/** Salary statement (prototype `breakHtml`): `c` is the calculation result, `std` the structure rates it used. */
export function Statement({ c, type, std, rules, lwf, month = 0 }: { c: any; type: string; std: any; rules?: Record<string, TypeRules>; lwf?: string; month?: number }) {
  const T: Partial<TypeRules> = rules?.[type] ?? TYPE_RULES[type] ?? {};
  const S = std || {};
  const R = (a: string, v: number, note?: ReactNode, total?: boolean) => <tr className={total ? 't' : ''}><td>{a}</td><td className="num">{inr(v || 0)}</td><td className="r">{note}</td></tr>;
  const H = (a: string) => <tr className="h"><td colSpan={3}>{a}</td></tr>;
  const auto = !T.statutory ? '' : c.esiOn ? `ESI applies automatically, gross is at or below ${inr(S.esiCeil)}` : c.medOn ? `Medical insurance applies automatically, gross is above ${inr(S.esiCeil)}` : '';
  const vp = c.gross ? pct((c.variable / c.gross) * 100) : '0';
  const vnote = `${vp}% of gross, paid ${VCT[c.varCycle] || 'every month'}, counted from the joining month`;
  const note = lwf ?? '';
  // A structure made from components (pay components): its own lines, by name.
  const comps: any[] = c.comps ?? [];
  const defs: any[] = S.comps ?? [];
  const how = (code: string) => compHow(defs.find((d) => d.code === code), defs);
  const fixedC = comps.filter((x) => x.kind === 'fixed' && x.side === 'earn'), varC = comps.filter((x) => x.kind === 'variable'), dedC = comps.filter((x) => x.side === 'ded');
  // A retainer is shown as the retainer value only: one fee line, no Basic, HRA or Special allowance split (calc retainerOnly).
  const ret = type === 'Retainer' || c.fee > 0;
  return <>
    <div className="tw"><table className="stmt"><tbody>
      {H(ret ? 'Retainer' : 'Fixed salary')}
      {ret ? R('Retainer fee', c.fee ?? c.gross, 'A month; TDS under section 194J')
        : comps.length ? fixedC.map((x) => <Fragment key={x.code}>{R(x.name, x.amount, how(x.code))}</Fragment>)
        : <>{R('Basic + DA', c.basic, `${S.basicPct}% of gross`)}{R('HRA', c.hra, `${S.hraPct}% of basic`)}{R('Special allowance', c.sa, 'Balancing figure')}</>}
      {!ret && R('Gross salary', c.gross, '', true)}
      {c.bonus > 0 && R('Bonus', c.bonus, `${S.bonusPct}% of basic up to ${inr(S.bonusCalc || 7000)}, for gross up to ${inr(S.bonusCeil)}`)}
      {c.variable > 0 && H('Variable pay, on top of fixed')}
      {comps.length ? varC.map((x) => <Fragment key={x.code}>{R(`${x.name}, target a month`, x.target, `Paid ${CYCLE[x.cycle] || 'monthly'}, the amount HR enters when due`)}
        {month > 0 && R(`${x.name} paid this month`, x.amount, x.amount ? 'Entered by HR' : 'Not entered')}</Fragment>)
        : <>{c.variable > 0 && R('Variable pay a month (provision)', c.variable, vnote)}
          {c.variable > 0 && month > 0 && R('Variable paid this month', c.varPay, c.varPay ? 'Payout month' : 'Not a payout month')}</>}
      {H('Deductions')}
      {T.statutory && R('Employee PF', c.eePF, `${S.pfEe}${S.pfWage === 'basicSa' ? '% of basic + special allowance' : '% of basic'}${S.pfCap ? `, wage ceiling ${inr(S.pfCeil)}` : ''}`)}
      {T.statutory && R('Employee ESI', c.eeESI, c.esiOn ? `${S.esiEe}% of gross` : `Not applicable above ${inr(S.esiCeil)}`)}
      {c.lwfOn && R('Labour welfare fund', c.lwfEe, note)}
      {c.meal > 0 && R('Meal', c.meal, 'Set by HR')}
      {c.accom > 0 && R('Accommodation', c.accom, 'Set by HR')}
      {c.tds > 0 && R('TDS', c.tds, `${S.tdsRate}% of gross, set in the structure`)}
      {dedC.map((x) => <Fragment key={x.code}>{x.amount > 0 && R(x.name, x.amount, how(x.code))}</Fragment>)}
      {T.ded === 'none' && <tr><td colSpan={3} className="mute">No deductions for this employment type.</td></tr>}
      {R('Net in hand' + (month > 0 ? '' : ' (without variable)'), c.net, 'Before income tax, if any', true)}
      {(T.statutory || T.grat || c.med > 0 || c.lwfOn) && H('Employer contribution')}
      {T.statutory && R('Employer PF', c.erPF, `${S.pfEr}%`)}
      {T.statutory && R('EPFO admin charge', c.admin, `${S.epfoAdm}% of basic`)}
      {T.statutory && R('Employer ESI', c.erESI, `${S.esiEr}% of gross`)}
      {c.lwfOn && R('Employer labour welfare fund', c.lwfEr, note)}
      {c.med > 0 && R('Medical insurance', c.med, `${S.medPct}% of gross, max ${inr(S.medMax)}`)}
      {T.grat && R('Gratuity', c.grat, `${S.grat}% of basic`)}
      {T.plEnc && H('Provisions in CTC')}
      {T.plEnc && R('PL encashment provision', c.plEnc, `${S.plDays} days of basic a year`)}
      {R('CTC a month', c.ctc, '', true)}{R('CTC a year', c.ctc * 12, '', true)}
    </tbody></table></div>
    {auto && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>{auto}.</p>}
    <WageNote c={c} />
  </>;
}

/** How a component is worked out, in words. */
export function compHow(d: any, all: any[] = []): string {
  if (!d) return '';
  const cap = [d.min > 0 ? `at least ${inr(d.min)}` : '', d.max > 0 ? `at most ${inr(d.max)}` : ''].filter(Boolean).join(', ');
  const of = all.find((x) => x.code === d.of)?.name ?? d.of;
  const base = ({ amount: `${inr(d.value)} a month`, pct_ctc: `${d.value}% of CTC`, pct_gross: `${d.value}% of fixed gross`, pct_basic: `${d.value}% of Basic`,
    pct_of: `${d.value}% of ${of}`, remainder: d.kind === 'variable' ? 'The rest of the variable target' : 'Balancing figure', legacy_var: `${d.value}% of gross, accrued monthly` } as Record<string, string>)[d.method] ?? '';
  return cap ? `${base} (${cap})` : base;
}

// ---------- Compensation page ----------

type TabK = 'guide' | 'bands' | 'structure' | 'components' | 'lwf' | 'budget' | 'offer';
const TABS: [TabK, string][] = [['guide', 'Setup guide'], ['bands', 'Pay bands'], ['structure', 'Salary structure'], ['components', 'Pay components'], ['lwf', 'Labour welfare fund'], ['budget', 'Budgets'], ['offer', 'Offer letter']];

export default function Compensation() {
  const { can } = useAuth();
  const d = useLoad(() => api('/comp'), []);
  const [t, setT] = useState<TabK>('guide');
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data;
  // HRBP sees budgets only (prototype `budOnly`). Other tabs show when the role's rights return their data.
  const budOnly = !can('comp.bands') && can('comp.budget');
  const has: Record<TabK, boolean> = { guide: true, bands: !!x.bands, structure: !!x.structures, components: !!x.structures, lwf: !!x.lwf, budget: !!x.budgets, offer: true };
  const tabs = TABS.filter(([k]) => (budOnly ? k === 'budget' : has[k]));
  const cur = tabs.some(([k]) => k === t) ? t : tabs[0][0];
  const go = (k: TabK) => { setT(k); window.scrollTo(0, 0); };
  return (
    <>
      <Tabs tabs={tabs} value={cur} onChange={setT} />
      {cur === 'guide' && <Guide x={x} go={go} hasTab={(k) => tabs.some(([y]) => y === k)} />}
      {cur === 'bands' && <><Bands x={x} reload={d.reload} /><BulkBox t="band" ed={can('comp.bands', 2)} onApplied={d.reload} /></>}
      {cur === 'structure' && <Structures x={x} reload={d.reload} />}
      {cur === 'components' && <PayComponents x={x} reload={d.reload} />}
      {cur === 'lwf' && <Lwf x={x} reload={d.reload} />}
      {cur === 'budget' && <><Budgets x={x} reload={d.reload} /><BulkBox t="bud" ed={can('comp.budget', 2)} onApplied={d.reload} /></>}
      {cur === 'offer' && <OfferLetter x={x} />}
    </>
  );
}

// ---------- Setup guide (prototype `compChecks`) ----------

function Guide({ x, go, hasTab }: { x: any; go: (k: TabK) => void; hasTab: (k: TabK) => boolean }) {
  const nav = useNavigate();
  const { can } = useAuth();
  const data = useLoad(() => Promise.all([
    api<any[]>('/masters/grade?status=Active'),
    api<any[]>('/masters/designation?status=Active'),
    x.structures ? Promise.resolve(x.structures) : api<any[]>('/salary/structures').catch(() => []),
    can('appr.queue') ? api<any[]>('/approvals/flows').catch(() => []) : Promise.resolve([]),
    can('appr.queue') ? api('/approvals/approvers').catch(() => ({ gaps: [] })) : Promise.resolve({ gaps: [] }),
    api('/settings/company').catch(() => ({ offerTemplate: '' })),
  ]), [x]);
  if (!data.data) return <Loading error={data.error} />;
  const [grades, desigs, structs, matrix, dir, company] = data.data;
  const cities = [...new Set((x.bands ?? []).map((b: any) => b.city_id))];
  const miss = (x.bands ?? []).filter((b: any) => !(b.max > 0 && b.max >= b.min)).map((b: any) => `${b.grade} in ${b.city}`);
  const unmapped = desigs.filter((g) => !(g.grades || []).length);
  const on = structs.filter((s: any) => s.status === 'Active'), def = structs.find((s: any) => s.is_default);
  const flows = ['Operations', 'Corporate'].filter((f) => matrix.some((m) => m.wf === 'joining' && m.flow === f && (m.rules || []).some((r: any) => r.on && (r.when || []).some((c: any) => c.k === 'budget'))));
  const gaps: string[] = dir.gaps || [];
  const tpl: string = company.offerTemplate || '';
  type Check = { t: string; ok: boolean; d: string; open: () => void; can?: boolean };
  const ck: Check[] = [
    { t: 'Grades are defined', ok: grades.length > 0, d: grades.length ? `${grades.length} active grades: ${grades.map((g) => g.name).join(', ')}` : 'Add grades in Masters', open: () => nav('/masters') },
    { t: 'Every designation is mapped to grades', ok: !unmapped.length, d: unmapped.length ? `${unmapped.length} designations have no grade` : 'Offers list only the mapped grades for a designation', open: () => nav('/masters?tab=desig') },
    { t: 'Standard salary structure is set', ok: (def?.rates?.basicPct ?? 0) > 0, d: def ? `${on.length} version${on.length > 1 ? 's' : ''}. Default: ${def.name}. Basic ${def.rates.basicPct}% of gross, ESI up to ${inr(def.rates.esiCeil)}, medical above that` : 'Set a default version in Salary structure', open: () => go('structure'), can: hasTab('structure') },
    { t: 'Every city and grade has a pay band', ok: !miss.length, d: miss.length ? `${miss.length} missing, for example ${miss.slice(0, 3).join(', ')}` : `Bands set for ${cities.length} cities and ${grades.length} grades`, open: () => go('bands'), can: hasTab('bands') },
    { t: 'Over-budget offers trigger extra approval', ok: flows.length === 2, d: flows.length === 2 ? 'Offers above budget or above the pay band get extra approvers in both flows' : 'Switch on an above-budget condition for both flows in the joining workflow', open: () => nav('/approvals?tab=rules') },
    { t: 'Approvers exist for every cluster', ok: !gaps.length, d: gaps.length ? `Missing: ${gaps.slice(0, 3).join(', ')}${gaps.length > 3 ? '…' : ''}` : 'Directory covers all clusters', open: () => nav('/approvals?tab=rules') },
    { t: 'Offer letter template has the pay merge fields', ok: /\{\{gross\}\}/.test(tpl) && /\{\{ctcYear\}\}/.test(tpl), d: 'Template uses gross and annual CTC fields', open: () => go('offer') },
  ];
  const done = ck.filter((c) => c.ok).length;
  const flow = ['Candidate form', 'Band check', 'Approvals', 'Release offer', 'Accepted', 'Pre-boarding'];
  return <>
    <div className="note" style={{ marginBottom: 14 }}>How an offer is priced and released: set pay bands for each grade and zone, define how gross splits into basic, HRA and allowances, let approval rules catch exceptions, then release the letter from Onboarding once the joining is approved. {done} of {ck.length} steps are ready.</div>
    <div className="grid">
      {ck.map((c, i) => <div className="card" key={c.t}><div className="row sp">
        <div><span className="mute mono">{i + 1}</span> <b>{c.t}</b><div className="mute" style={{ fontSize: 12.5, marginTop: 2 }}>{c.d}</div></div>
        <div className="row"><Pill k={c.ok ? 'ok' : 'warn'}>{c.ok ? 'Ready' : 'Needs attention'}</Pill><button className="btn sm" disabled={c.can === false} onClick={c.open}>Open</button></div>
      </div></div>)}
      <div className="card"><h3>Release flow</h3><div className="chain">{flow.map((s, i) => <span key={s} style={{ display: 'contents' }}>{i > 0 && <span className="arrow">→</span>}<span className="step">{s}</span></span>)}</div></div>
    </div>
  </>;
}

// ---------- Pay bands ----------

function Bands({ x, reload }: { x: any; reload: () => void }) {
  const ed = x.can.bands;
  const cities = [...new Map<string, string>(x.bands.map((b: any) => [b.city_id, b.city] as [string, string])).entries()];
  const grades = [...new Set<string>(x.bands.map((b: any) => b.grade))];
  const [city, setCity] = useState(cities[0]?.[0] ?? '');
  const [from, setFrom] = useState('');
  const [draft, setDraft] = useState<Record<string, [string, string]>>({});
  const saved = (cid: string, g: string) => x.bands.find((b: any) => b.city_id === cid && b.grade === g) ?? {};
  const bandFor = (cid: string, g: string) => { const b = saved(cid, g); return b.max > 0 && b.max >= b.min ? { min: b.min, max: b.max, mid: bandMid(b.min, b.max) } : null; };
  const val = (g: string): [string, string] => draft[`${city}|${g}`] ?? [saved(city, g).min ? String(saved(city, g).min) : '', saved(city, g).max ? String(saved(city, g).max) : ''];
  const edit = (g: string, i: 0 | 1, v: string) => { const n = [...val(g)] as [string, string]; n[i] = v; setDraft({ ...draft, [`${city}|${g}`]: n }); };
  const commit = async (g: string) => {
    const k = `${city}|${g}`, v = draft[k];
    if (!v) return;
    const [mn, mx] = v.map((y) => +y || 0), b = saved(city, g);
    if (mn === (b.min || 0) && mx === (b.max || 0)) { setDraft((o) => omit(o, k)); return; }
    if (!mn || !mx) return; // a band needs both ends; keep the half-typed row until the other is entered
    try { await api('/comp/bands', { method: 'PUT', body: { cityId: city, grade: g, min: mn, max: mx } }); setDraft((o) => omit(o, k)); reload(); } catch (e: any) { toast(e.message); }
  };
  const copy = async () => {
    if (!from || from === city) { toast('Pick another city to copy from'); return; }
    try { await api('/comp/bands/copy', { body: { from, to: city, factor: 1 } }); setFrom(''); setDraft({}); reload(); toast('Bands copied. Review them against the minimum wage.'); } catch (e: any) { toast(e.message); }
  };
  return <>
    <div className="note" style={{ marginBottom: 14 }}>A pay band is the monthly gross range for a grade in a city. Minimum wage compliance is handled through these bands, so set each band minimum at or above the minimum wage that applies to the grade.</div>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ marginBottom: 12 }}>
        <div className="row"><label className="field" style={{ margin: 0 }}><span>City</span><select value={city} onChange={(e) => setCity(e.target.value)}>{cities.map(([id, n]) => <option key={id} value={id}>{n}</option>)}</select></label></div>
        {ed && <div className="row"><select value={from} onChange={(e) => setFrom(e.target.value)}><option value="">Copy bands from…</option>{cities.filter(([id]) => id !== city).map(([id, n]) => <option key={id} value={id}>{n}</option>)}</select><button className="btn" onClick={copy}>Copy</button></div>}
      </div>
      <div className="tw"><table><thead><tr><th>Grade</th><th>Minimum gross</th><th>Maximum gross</th><th className="num">Midpoint</th><th>Status</th></tr></thead><tbody>
        {grades.map((g) => {
          const v = val(g), bf = bandFor(city, g);
          return <tr key={g}><td><b>{g}</b></td>
            <td><input type="number" value={v[0]} style={{ width: 110 }} disabled={!ed} onChange={(e) => edit(g, 0, e.target.value)} onBlur={() => commit(g)} /></td>
            <td><input type="number" value={v[1]} style={{ width: 110 }} disabled={!ed} onChange={(e) => edit(g, 1, e.target.value)} onBlur={() => commit(g)} /></td>
            <td className="num">{bf ? inr(bf.mid) : '—'}</td><td>{bf ? <Pill k="ok">Set</Pill> : <Pill k="warn">Set the band</Pill>}</td></tr>;
        })}
      </tbody></table></div>
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>An offer inside the band follows the normal route. Above the band and below the band each add the approvers set in the rules. Change this in Approvals, Approval structure, New joining.</p>
    </div>
    <div className="card"><h3>All cities at a glance</h3><div className="tw"><table>
      <thead><tr><th>Grade</th>{cities.map(([id, n]) => <th key={id}>{n}</th>)}</tr></thead>
      <tbody>{grades.map((g) => <tr key={g}><td><b>{g}</b></td>{cities.map(([id]) => { const b = bandFor(id, g); return <td key={id} className="mono" style={{ fontSize: 12 }}>{b ? `${inr(b.min)} to ${inr(b.max)}` : '—'}</td>; })}</tr>)}</tbody>
    </table></div></div>
  </>;
}

// ---------- Bulk update box under Pay bands and Budgets (prototype `bulkBox`) ----------

const BULK: Record<'band' | 'bud', { name: string; cols: string[]; hint: string }> = {
  band: { name: 'Pay bands', cols: ['City', 'Grade', 'Minimum Gross', 'Maximum Gross'], hint: 'One row for each city and grade. Download the current bands, change the minimum and maximum, and upload. Blank cells keep the current value. The check shows how many employees fall outside the new band.' },
  bud: { name: 'Budgets', cols: ['Level', 'Name', 'Headcount Budget', 'Monthly Cost Budget (CTC)'], hint: 'One row for each city or location. Level is City or Location, and Name is its name. City rows take a headcount budget only; a city cost budget is the total of its locations. The cost budget is the total monthly cost to the company: salary plus the employer PF, ESI and other contributions. Blank cells keep the current value. Use 0 for no budget.' },
};

type BulkRes = { ok: number; results: { line: number; id: string; name?: string; errors: string[]; changes: { field: string; from: string; to: string; note?: string }[] }[] };

function BulkBox({ t, ed, onApplied }: { t: 'band' | 'bud'; ed: boolean; onApplied: () => void }) {
  const tp = BULK[t];
  const [file, setFile] = useState<File | null>(null);
  const [text, setText] = useState('');
  const [res, setRes] = useState<BulkRes | null>(null);
  const [busy, setBusy] = useState(false);
  if (!ed) return <div className="card" style={{ marginTop: 14 }}><h3>Bulk update</h3><p className="mute" style={{ margin: 0 }}>You can view this data but not change it in bulk.</p></div>;
  const src = () => file ?? (text.trim() ? new File([text], `${t}.csv`, { type: 'text/csv' }) : null);
  const clear = () => { setFile(null); setText(''); setRes(null); };
  const run = async (action: 'check' | 'apply') => {
    const f = src();
    if (!f) return;
    setBusy(true);
    try {
      const r = await postForm<BulkRes>(`/bulk/${t}/${action}`, {}, f);
      if (action === 'apply') { toast(`${r.ok} row${r.ok === 1 ? '' : 's'} applied`); clear(); onApplied(); } else setRes(r);
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const ok = res?.ok ?? 0, rows = res?.results ?? [];
  const unchanged = rows.filter((x) => !x.errors.length && !x.changes.length).length, bad = rows.filter((x) => x.errors.length).length;
  return <div className="card" style={{ marginTop: 14 }}><h3>Bulk update: {tp.name}</h3><p className="ink2" style={{ fontSize: 12.5, margin: '0 0 10px' }}>{tp.hint}</p>
    <div className="row" style={{ flexWrap: 'wrap', gap: 8 }}>
      <button className="btn" onClick={() => download(`/bulk/${t}/download?data=1`, `bulk-${t}-current.xlsx`).catch((e) => toast(e.message))}>Download template with current data</button>
      <label className="btn">Choose file<input type="file" accept=".xlsx,.csv,text/csv" hidden onChange={(e) => { setFile(e.target.files?.[0] ?? null); setRes(null); e.target.value = ''; }} /></label>
      <button className="btn primary" disabled={(!file && !text.trim()) || busy} onClick={() => run('check')}>Check file</button>
      {res && <><button className="btn primary" disabled={!ok || busy} onClick={() => run('apply')}>Apply {ok} row{ok === 1 ? '' : 's'}</button><button className="btn" onClick={clear}>Clear</button></>}
      {file ? <span className="mute" style={{ fontSize: 12.5 }}>{file.name}</span> : text && <span className="mute" style={{ fontSize: 12.5 }}>{text.split('\n').length - 1} lines loaded</span>}
    </div>
    <details className="adv" style={{ marginTop: 10 }}><summary>Or paste the file</summary><div><textarea rows={4} style={{ width: '100%' }} placeholder={tp.cols.join(',')} value={text} onChange={(e) => { setText(e.target.value); setRes(null); }} /></div></details>
    {res && <div style={{ marginTop: 14 }}>
      <div className="row" style={{ gap: 10, marginBottom: 8, flexWrap: 'wrap' }}><Pill k="ok">{ok} with changes</Pill><Pill k="n">{unchanged} unchanged</Pill>{bad > 0 && <Pill k="bad">{bad} with errors</Pill>}</div>
      <div className="tw"><table><thead><tr><th>Line</th><th>Row</th><th>Result</th></tr></thead><tbody>
        {rows.filter((x) => x.errors.length || x.changes.length).slice(0, 150).map((x) => <tr key={x.line}>
          <td className="mono">{x.line || '—'}</td><td>{x.id}</td>
          <td>{x.errors.length ? x.errors.map((m) => <Pill key={m} k="bad">{m}</Pill>)
            : x.changes.map((c, i) => <div key={i} style={{ fontSize: 12.5 }}>{c.field}: {c.from} → <b>{c.to}</b>{c.note && <div><Pill k="warn">{c.note}</Pill></div>}</div>)}</td>
        </tr>)}
      </tbody></table></div>
    </div>}
  </div>;
}

// ---------- Salary structure ----------

type Fld = [string, string, ('n' | 's' | 'b')?, [string, string][]?];
const YN: [string, string][] = [['true', 'Yes'], ['false', 'No']];
const GROUPS: [string, Fld[], string?][] = [
  ['Split of gross', [['basicPct', 'Basic + DA, % of gross'], ['hraPct', 'HRA, % of basic']]],
  ['Variable pay, on top of fixed salary', [['varPct', 'Standard variable, % of gross'], ['varCycle', 'Standard payout', 's', VC]],
    'HR can change the percentage and the payout for any employee. Payout cycles are counted from the joining month, so someone who joined in October and is paid every 3 months gets it in December, March, June and September.'],
  ['Provident fund', [['pfWage', 'PF wages are', 's', [['basic', 'Basic only'], ['basicSa', 'Basic + special allowance']]], ['pfEe', 'Employee, % of PF wages'], ['pfEr', 'Employer, % of PF wages'],
    ['pfCeil', 'Wage ceiling, ₹ a month'], ['pfCap', 'Apply the wage ceiling', 'b', YN], ['epfoAdm', 'EPFO admin charge (employer), % of basic']]],
  ['ESI and medical, chosen automatically by gross', [['esiCeil', 'ESI applies when gross is at most, ₹'], ['esiEe', 'ESI employee, % of gross'], ['esiEr', 'ESI employer, % of gross'],
    ['medOn', 'Medical insurance above the ESI ceiling', 'b', YN], ['medPct', 'Medical insurance, % of gross'], ['medMax', 'Medical insurance maximum, ₹']]],
  ['Bonus (automatic)', [['bonusOn', 'Pay bonus automatically', 'b', YN], ['bonusCeil', 'Applies when gross is up to, ₹ a month'],
    ['bonusCalc', 'Bonus is worked on basic up to, ₹ (law: ₹7,000 or minimum wage, whichever is higher)'], ['bonusPct', 'Bonus, % of basic, paid monthly']]],
  ['Retainer TDS', [['tdsRate', 'TDS, % of gross']]],
  ['Other', [['grat', 'Gratuity, % of basic'], ['plDays', 'PL encashment, days of basic a year'], ['denom', 'Days basis for proration', 's', [['calendar', 'Calendar days'], ['30', '30 days'], ['26', '26 days']]]]],
];

function Structures({ x, reload }: { x: any; reload: () => void }) {
  const list: any[] = x.structures;
  const [sel, setSel] = useState<string>(list.find((s) => s.is_default)?.id ?? list[0]?.id);
  const cur = list.find((s) => s.id === sel) ?? list[0];
  const cfg = x.can.structure;
  const [r, setR] = useState<any>(cur?.rates ?? {});
  const [name, setName] = useState(cur?.name ?? '');
  const key = cur ? cur.id + JSON.stringify(cur.rates) + cur.name : '';
  useEffect(() => { if (cur) { setR(cur.rates); setName(cur.name); } }, [key]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!cur) return <div className="card"><p className="mute" style={{ margin: 0 }}>No salary structure yet.</p></div>;
  const save = async (rates: any, nm = cur.name) => {
    try { await api(`/comp/structures/${cur.id}`, { method: 'PUT', body: { name: nm, rates } }); toast('Saved'); reload(); } catch (e: any) { toast(e.message); setR(cur.rates); setName(cur.name); }
  };
  const setK = (k: string, v: any, now: boolean) => { const n = { ...r, [k]: v }; setR(n); if (now) save(n); };
  const blur = (k: string) => { if (r[k] !== cur.rates[k]) save(r); };
  const fld = ([k, l, kind, opts]: Fld) => <label className="field" key={k}><span>{l}</span>{kind
    ? <select disabled={!cfg} value={String(r[k])} onChange={(e) => setK(k, kind === 'b' ? e.target.value === 'true' : k === 'varCycle' ? +e.target.value : e.target.value, true)}>{opts!.map(([v, t]) => <option key={v} value={v}>{t}</option>)}</select>
    : <input type="number" step="any" disabled={!cfg} value={r[k] ?? ''} onChange={(e) => setK(k, +e.target.value || 0, false)} onBlur={() => blur(k)} />}</label>;
  const newVersion = async () => {
    let n = list.length + 1;
    while (list.some((s) => s.name === `Version ${n}`)) n++;
    try { const res = await api('/comp/structures', { body: { name: `Version ${n}`, copyFrom: cur.id } }); setSel(res.id); reload(); toast(`Version ${n} created. Edit it, then map employees in Payroll`); } catch (e: any) { toast(e.message); }
  };
  const makeDefault = async () => { try { await api(`/comp/structures/${cur.id}/default`, { body: {} }); reload(); toast(`${cur.name} is the default for new joiners`); } catch (e: any) { toast(e.message); } };
  const on = cur.status === 'Active';
  // Prototype stOffV: the default stays on, and a version stays on while active employees are on it (the server checks).
  const switchOn = async () => { try { await api(`/comp/structures/${cur.id}/status`, { body: { on: !on } }); reload(); toast(`${cur.name} ${on ? 'switched off' : 'switched on'}`); } catch (e: any) { toast(e.message); } };
  const setRule = async (t: string, k: keyof TypeRules, v: string) => {
    const T = x.types?.[t] ?? TYPE_RULES[t];
    try { await api(`/comp/types/${t}`, { method: 'PUT', body: { ...T, [k]: k === 'ded' ? v : v === 'true' } }); toast('Saved'); reload(); } catch (e: any) { toast(e.message); }
  };
  return <>
    <div className="note" style={{ marginBottom: 14 }}>A salary structure is a set of rules for the breakup. Create as many versions as you need and pick one as the default for new joiners. Existing employees stay on their version until Payroll moves them, from a date, in Payroll, Structure mapping. HR enters only the monthly gross. ESI applies automatically when gross is at or below the ESI ceiling and medical insurance above it. Pay bands handle minimum wage.</div>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp"><div className="seg">{list.map((y) => <button key={y.id} className={y.id === cur.id ? 'on' : ''} onClick={() => setSel(y.id)}>{y.name}{y.is_default && <b>Default</b>}{y.status !== 'Active' && <b>Off</b>}</button>)}</div>
        {cfg && <div className="row"><button className="btn" onClick={newVersion}>New version from {cur.name}</button>{!cur.is_default && on && <button className="btn" onClick={makeDefault}>Make default</button>}{!cur.is_default && <button className="btn" onClick={switchOn}>{on ? 'Switch off' : 'Switch on'}</button>}</div>}</div>
      <div className="row" style={{ marginTop: 12 }}><label className="field" style={{ margin: 0 }}><span>Version name</span><input type="text" value={name} disabled={!cfg} onChange={(e) => setName(e.target.value)} onBlur={() => { if (name.trim() && name !== cur.name) save(cur.rates, name.trim()); }} /></label>
        <span className="mute" style={{ fontSize: 13, alignSelf: 'end' }}>{cur.active ?? 0} active employee{cur.active === 1 ? '' : 's'} on this version</span></div>
      {cur.rates?.converted && <div className="note" style={{ marginTop: 12 }}>Variable pay on {cur.name} is a target per person, entered as a % of the fixed gross or ₹ a month, and paid as HR enters
        the amount when it is due (Payroll › Variable pay). The standard variable % below is not used; the standard payout is the payout cycle, counted from each person's joining month (quarterly, joined in May: July, October, January, April).</div>}
      {cur.rates?.designed && !cur.rates?.converted && <div className="note" style={{ marginTop: 12 }}>{cur.name} is made from components: the split of gross and the standard variable below are not used.
        Its components are in the Pay components tab; the statutory settings here (PF, ESI, bonus, medical, gratuity, days basis) still apply.</div>}
    </div>
    <div className="grid split" style={{ marginBottom: 14 }}>
      <div className="card"><h3>{cur.name}</h3>
        {GROUPS.map(([g, fs, note], i) => <div key={g}>
          <h3 style={{ margin: i ? '16px 0 8px' : '6px 0 8px', fontSize: 13 }}>{g}</h3>
          <div className="form">{fs.map(fld)}{i === 0 && <label className="field"><span>Special allowance</span><input type="text" value="Balancing figure" disabled /></label>}</div>
          {note && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>{note}</p>}
        </div>)}
        <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Labour welfare fund is set by state in the next tab. Meal and accommodation are deductions entered per employee by HR. Nothing is assumed.</p>
      </div>
      <TryOffer x={x} cur={cur} />
    </div>
    <div className="card"><h3>Rules by employment type</h3><p className="mute" style={{ margin: '-6px 0 8px', fontSize: 12.5 }}>These apply to every version.</p>
      <div className="tw"><table><thead><tr><th>Employment type</th><th>PF, ESI, medical</th><th>Labour welfare fund</th><th>Gratuity</th><th>PL encashment</th><th>Bonus allowed</th><th>Variable pay allowed</th><th>Employee deductions</th><th>Leave</th><th>Payslip and letters</th></tr></thead>
        <tbody>{ALL_TYPES.map((t) => {
          const T = x.types?.[t] ?? TYPE_RULES[t];
          // Stored on the server (PUT /comp/types/{type}); the payroll engine reads them. Editable with comp.structure 2.
          const yn = (k: keyof TypeRules) => <td><select disabled={!cfg} value={String(T[k])} onChange={(e) => setRule(t, k, e.target.value)}>{YN.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></td>;
          return <tr key={t}><td><b>{t}</b></td>{yn('statutory')}{yn('lwf')}{yn('grat')}{yn('plEnc')}{yn('bonus')}{yn('variable')}
            <td><select disabled={!cfg} value={T.ded} onChange={(e) => setRule(t, 'ded', e.target.value)}><option value="all">PF, ESI, meal, accommodation</option><option value="tds">TDS only</option><option value="none">None</option></select></td><td><LeaveRule t={t} T={T} edit={!!cfg} names={x.leaveTypes ?? []} onSaved={reload} /></td>{yn('docs')}</tr>;
        })}</tbody></table></div>
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Contractual staff have no deductions. Retainers have TDS only and no other variable. Neither can apply for leave (Leave: No leave) or download a payslip or appointment letter. Leave can also be limited to chosen leave types; the leave plan still decides days and rules. Contractual staff are assumed to be paid through the vendor, so PF, ESI and LWF are off. Switch them on here if Saltstayz deducts them directly.</p>
    </div>
  </>;
}

/** Leave eligibility of an employment type: none, only the chosen leave types, or all (the leave plan decides the rest).
 *  Enforced on leave requests, balances and opening balances. */
function LeaveRule({ t, T, edit, names, onSaved }: { t: string; T: TypeRules; edit: boolean; names: string[]; onSaved: () => void }) {
  const chosen = T.leaveTypes ?? [];
  const [some, setSome] = useState(chosen.length > 0);
  const mode = !T.leave ? 'none' : some || chosen.length ? 'some' : 'all';
  const put = async (leave: boolean, leaveTypes: string[]) => {
    try { await api(`/comp/types/${t}`, { method: 'PUT', body: { ...T, leave, leaveTypes } }); toast('Saved'); onSaved(); } catch (e: any) { toast(e.message); }
  };
  return <div style={{ minWidth: 150 }}>
    <select disabled={!edit} value={mode} onChange={(e) => { const v = e.target.value; setSome(v === 'some'); if (v === 'none') put(false, []); else if (v === 'all') put(true, []); else if (!T.leave) put(true, chosen); }}>
      <option value="all">All leave types</option><option value="some">Only chosen types</option><option value="none">No leave</option></select>
    {mode === 'some' && <div style={{ marginTop: 6, display: 'grid', gap: 4 }}>{names.map((n) => <label key={n} className="chkl" style={{ fontWeight: 500, fontSize: 12 }}>
      <input type="checkbox" disabled={!edit} checked={chosen.includes(n)} onChange={(e) => put(true, e.target.checked ? [...chosen, n] : chosen.filter((x) => x !== n))} />{n}</label>)}
      {!chosen.length && <span className="mute" style={{ fontSize: 11.5 }}>Tick at least one; with none ticked every type is allowed.</span>}</div>}
  </div>;
}

/** "Try an offer on <version>": the breakup for a type, city, grade and gross on the selected version. */
function TryOffer({ x, cur }: { x: any; cur: any }) {
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const grades = useLoad(() => api<any[]>('/masters/grade?status=Active'), []);
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const [c, setC] = useState<Record<string, string>>({ type: 'Permanent', city: '', grade: '' });
  const [res, setRes] = useState<any>(null);
  const city = c.city || cities.data?.[0]?.id || '';
  const grade = c.grade || grades.data?.[0]?.name || '';
  const loc = locs.data?.find((l) => l.city_id === city)?.id ?? '';
  const Tc = x.types?.[c.type] ?? TYPE_RULES[c.type];
  const bandRow = x.bands?.find((b: any) => b.city_id === city && b.grade === grade);
  const band = bandRow ? (bandRow.max > 0 && bandRow.max >= bandRow.min ? bandRow : null) : res?.band ? { min: res.band.min_gross, max: res.band.max_gross } : null;
  const gross = c.gross === undefined ? (band ? bandMid(band.min, band.max) : 0) : +c.gross || 0;
  const tgt = !!cur.rates?.designed, vMode: VarMode = (c.varMode as VarMode) || 'pct', vv = Tc.variable && c.varVal !== undefined && c.varVal !== '' ? +c.varVal : undefined;
  const body = JSON.stringify({ type: c.type, locationId: loc, grade, gross, structureId: cur.id, varMode: vMode, varPct: vMode === 'pct' ? vv : undefined, varAmt: vMode === 'amount' ? vv : undefined,
    varCycle: Tc.variable && !tgt && c.varCycle ? +c.varCycle : undefined, meal: Tc.ded === 'all' ? +c.dMeal || 0 : 0, accom: Tc.ded === 'all' ? +c.dAccom || 0 : 0, rates: cur.rates });
  useEffect(() => {
    if (!gross || !loc) { setRes(null); return; }
    const t = setTimeout(() => { const b = JSON.parse(body); delete b.rates; api('/salary/preview', { body: b }).then(setRes).catch(() => setRes(null)); }, 250);
    return () => clearTimeout(t);
  }, [body]); // eslint-disable-line react-hooks/exhaustive-deps
  const cityName = cities.data?.find((y) => y.id === city)?.name ?? '';
  const stateRule = x.lwf?.find((l: any) => l.state_id === cities.data?.find((y) => y.id === city)?.state_id)?.rule;
  const pos = band ? (gross < band.min ? 'below' : gross > band.max ? 'above' : 'within') : '';
  const set = (k: string, v: string) => setC({ ...c, [k]: v });
  const fin = (k: string, l: string, ph?: string) => <label className="field" key={k}><span>{l}</span><input type="number" placeholder={ph} value={k === 'gross' ? c.gross ?? (gross ? String(gross) : '') : c[k] ?? ''} onChange={(e) => set(k, e.target.value)} /></label>;
  return <div className="card"><h3>Try an offer on {cur.name}</h3>
    <div className="form">
      <label className="field"><span>Employment type</span><select value={c.type} onChange={(e) => set('type', e.target.value)}>{ALL_TYPES.map((t) => <option key={t}>{t}</option>)}</select></label>
      <label className="field"><span>City</span><select value={city} onChange={(e) => set('city', e.target.value)}>{(cities.data ?? []).map((y) => <option key={y.id} value={y.id}>{y.name}</option>)}</select></label>
      <label className="field"><span>Grade</span><select value={grade} onChange={(e) => set('grade', e.target.value)}>{(grades.data ?? []).map((g) => <option key={g.id}>{g.name}</option>)}</select></label>
      {fin('gross', 'Monthly gross')}
    </div>
    <h3 style={{ margin: '14px 0 8px', fontSize: 13 }}>Options HR can set</h3>
    <div className="form">
      {Tc.variable && <VarTargetField mode={vMode} value={c.varVal ?? ''} gross={gross} onChange={(m, v) => setC({ ...c, varMode: m, varVal: v })} />}
      {Tc.ded === 'all' && <>{fin('dMeal', 'Meal deduction, ₹ a month')}{fin('dAccom', 'Accommodation deduction, ₹ a month')}</>}
    </div>
    {band ? <p style={{ fontSize: 13, margin: '12px 0 4px' }}>Band for {grade} in {cityName}: <b>{inr(band.min)} to {inr(band.max)}</b> <Pill k={pos === 'within' ? 'ok' : 'warn'}>{pos === 'within' ? 'Within band' : pos === 'above' ? 'Above band' : 'Below band'}</Pill></p>
      : <p className="mute" style={{ fontSize: 12.5 }}>No band set for {grade} in {cityName}.</p>}
    <div style={{ marginTop: 8 }}>{res?.breakup && <Statement c={res.breakup} type={c.type} std={cur.rates} rules={x.types} lwf={res.lwfNote ?? lwfNote(stateRule)} />}</div>
  </div>;
}

// ---------- Labour welfare fund ----------

function Lwf({ x, reload }: { x: any; reload: () => void }) {
  const ce = x.can.lwf;
  const cities = useLoad(() => api<any[]>('/masters/city'), []);
  const blank = { on: false, ee: { mode: 'fixed', val: 0, max: 0 }, er: { mode: 'fixed', val: 0, max: 0 }, freq: 'monthly', months: '6,12' };
  const init = () => Object.fromEntries(x.lwf.map((l: any) => [l.state_id, l.rule ?? blank]));
  const [rules, setRules] = useState<Record<string, any>>(init);
  useEffect(() => setRules(init()), [x]); // eslint-disable-line react-hooks/exhaustive-deps
  const save = async (id: string, rule: any) => {
    try { await api(`/comp/lwf/${id}`, { method: 'PUT', body: { rule, effectiveFrom: today() } }); toast('Saved'); reload(); } catch (e: any) { toast(e.message); setRules(init()); }
  };
  const upd = (id: string, path: string, v: any, now: boolean) => {
    const L = JSON.parse(JSON.stringify(rules[id]));
    const [a, b] = path.split('.');
    if (b) L[a][b] = v; else L[a] = v;
    if (path === 'freq' && v !== 'monthly' && !String(L.months || '').trim()) L.months = '6,12';
    setRules({ ...rules, [id]: L });
    if (now) save(id, L);
  };
  const blur = (id: string) => { const was = x.lwf.find((l: any) => l.state_id === id)?.rule ?? blank; if (JSON.stringify(was) !== JSON.stringify(rules[id])) save(id, rules[id]); };
  const amt = (id: string, w: 'ee' | 'er', a: any) => <div className="row" style={{ gap: 6, flexWrap: 'nowrap' }}>
    <select disabled={!ce} value={a.mode} onChange={(e) => upd(id, `${w}.mode`, e.target.value, true)}><option value="fixed">₹ fixed</option><option value="pct">% of gross</option></select>
    <input type="number" step="any" style={{ width: 78 }} disabled={!ce} value={a.val} onChange={(e) => upd(id, `${w}.val`, +e.target.value || 0, false)} onBlur={() => blur(id)} />
    <input type="number" style={{ width: 70 }} title="Maximum, 0 means no maximum" disabled={!ce} value={a.max} onChange={(e) => upd(id, `${w}.max`, +e.target.value || 0, false)} onBlur={() => blur(id)} />
  </div>;
  const payers = ALL_TYPES.filter((t) => (x.types?.[t] ?? TYPE_RULES[t]).lwf);
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Labour welfare fund is set for each state, and each city follows its state. Which employment types pay it is chosen in Salary structure, Rules by employment type. For each amount enter the basis, the value and a maximum (0 for none). Delhi, Haryana and Chandigarh come from your payroll sheet. Karnataka, Telangana and Maharashtra were not in the sheet, so they are switched off until you enter the notified rates.</div>
    <div className="card"><div className="tw"><table><thead><tr><th>State</th><th>Cities</th><th>Applicable</th><th>Employee: basis, value, max</th><th>Employer: basis, value, max</th><th>Deducted</th><th>In months</th></tr></thead><tbody>
      {x.lwf.map((l: any) => {
        const L = rules[l.state_id] ?? blank, cs = (cities.data ?? []).filter((c) => c.state_id === l.state_id).map((c) => c.name).join(', ');
        return <tr key={l.state_id}><td><b>{l.state}</b>{l.next_from && <div className="mute" style={{ fontSize: 12 }}>A new rule applies from {fmtD(l.next_from)}</div>}</td><td className="mute">{cs || '—'}</td>
          <td><Switch on={!!L.on} disabled={!ce} label={`Labour welfare fund in ${l.state}`} onChange={() => upd(l.state_id, 'on', !L.on, true)} /></td>
          <td>{amt(l.state_id, 'ee', L.ee)}</td><td>{amt(l.state_id, 'er', L.er)}</td>
          <td><select disabled={!ce} value={L.freq} onChange={(e) => upd(l.state_id, 'freq', e.target.value, true)}><option value="monthly">Every month</option><option value="half">Twice a year</option><option value="year">Once a year</option></select></td>
          <td><input type="text" placeholder="e.g. 6,12" style={{ width: 90 }} disabled={!(ce && L.freq !== 'monthly')} value={L.months || ''} onChange={(e) => upd(l.state_id, 'months', e.target.value, false)} onBlur={() => blur(l.state_id)} /></td></tr>;
      })}
    </tbody></table></div>
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Add a new state in Masters, States, then set its fund here. Months are numbers, for example 6,12 for June and December. Employment types that pay it: {payers.join(', ') || 'none'}.</p></div>
  </>;
}

// ---------- Budgets (prototype `vBudget`) ----------

function Budgets({ x, reload }: { x: any; reload: () => void }) {
  const ed = x.can.budget;
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const [draft, setDraft] = useState<Record<string, string>>({});
  const cluster = (id: string) => locs.data?.find((l) => l.id === id)?.cluster ?? '';
  const pillU = (b: number, u: number) => !b ? <Pill k="n">Not set</Pill> : u > 1 ? <Pill k="bad">Over</Pill> : u >= 0.9 ? <Pill k="warn">Near limit</Pill> : <Pill k="ok">Within</Pill>;
  const save = async (kind: 'city' | 'location', id: string, field: 'headcount' | 'payroll', k: string, was: number) => {
    const v = draft[k];
    if (v === undefined) return;
    if ((+v || 0) === (was || 0)) { setDraft((o) => omit(o, k)); return; }
    try { await api(`/comp/budgets/${kind}/${id}`, { method: 'PUT', body: { [field]: +v || 0 } }); setDraft((o) => omit(o, k)); toast('Saved'); reload(); } catch (e: any) { toast(e.message); setDraft((o) => omit(o, k)); }
  };
  const inp = (kind: 'city' | 'location', id: string, field: 'headcount' | 'payroll', was: number, w: number) => {
    const k = `${kind}|${id}|${field}`;
    return <input type="number" min={0} style={{ width: w }} value={draft[k] ?? String(was || 0)} onChange={(e) => setDraft({ ...draft, [k]: e.target.value })} onBlur={() => save(kind, id, field, k, was)} />;
  };
  const cityRows: any[] | undefined = x.cityBudgets;
  const groups: [any, any[]][] = cityRows
    ? cityRows.map((c) => [c, x.budgets.filter((b: any) => b.city === c.name)])
    : [[null, x.budgets]];
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Set a headcount budget and a monthly cost budget (CTC) for each location: the total monthly cost to the company, salary (without the meal and accommodation minimum-wage top-ups) plus the employer PF, ESI and other contributions, as Cost and budget counts it. Leave 0 where there is no budget. A new offer that takes a city or a location over its budget gets the extra approvers set under “Above budget” in the joining approval flow. Update many at once with Bulk update, Location budget.</div>
    <div className="card"><div className="tw"><table>
      <thead><tr><th>City and location</th><th className="num">Headcount budget</th><th className="num">Active</th><th className="num">Offers pending</th><th className="num">Open seats</th><th className="num">Monthly cost budget, CTC (₹)</th><th className="num">Current monthly cost (CTC)</th><th>Status</th></tr></thead>
      <tbody>{groups.map(([c, ls], gi) => {
        const pend = ls.reduce((s, l) => s + (l.pending_offers || 0), 0), cost = ls.reduce((s, l) => s + (+l.payroll_budget || 0), 0), actual = ls.reduce((s, l) => s + (+l.cost || 0), 0);
        return <Fragment key={c?.id ?? gi}>
          {c && <tr style={{ background: 'var(--soft,#f6f7f9)' }}><td><b>{c.name}</b> <span className="mute" style={{ fontSize: 12 }}>city</span></td>
            <td className="num">{ed ? inp('city', c.id, 'headcount', c.headcount_budget, 90) : c.headcount_budget || 0}</td>
            <td className="num">{c.headcount}</td><td className="num">{pend}</td><td className="num">{c.headcount_budget ? c.headcount_budget - c.headcount - pend : '—'}</td>
            <td className="num mute">{cost ? inr(cost) : '—'}</td><td className="num">{inr(actual)}</td>
            <td>{pillU(c.headcount_budget, c.headcount_budget ? (c.headcount + pend) / c.headcount_budget : 0)}</td></tr>}
          {ls.map((l) => {
            const u = Math.max(l.headcount_budget ? (l.headcount + l.pending_offers) / l.headcount_budget : 0, l.payroll_budget ? l.cost / l.payroll_budget : 0);
            return <tr key={l.id}><td style={{ paddingLeft: 26 }}>{l.name} <span className="mute" style={{ fontSize: 12 }}>{cluster(l.id)}</span></td>
              <td className="num">{ed ? inp('location', l.id, 'headcount', l.headcount_budget, 90) : l.headcount_budget || 0}</td>
              <td className="num">{l.headcount}</td><td className="num">{l.pending_offers}</td><td className="num">{l.headcount_budget ? l.headcount_budget - l.headcount - l.pending_offers : '—'}</td>
              <td className="num">{ed ? inp('location', l.id, 'payroll', l.payroll_budget, 120) : l.payroll_budget ? inr(l.payroll_budget) : '—'}</td>
              <td className="num">{inr(l.cost)}</td><td>{pillU(l.headcount_budget || l.payroll_budget, u)}</td></tr>;
          })}
        </Fragment>;
      })}</tbody>
    </table></div></div>
  </>;
}

// ---------- Offer letter ----------

const PLACEHOLDERS = ['name', 'designation', 'department', 'company', 'location', 'city', 'grade', 'doj', 'gross', 'fixed', 'variable', 'ctcYear', 'net', 'probation'];

function OfferLetter({ x }: { x: any }) {
  const { can } = useAuth();
  const company = useLoad(() => api('/settings/company'), []);
  const seeOffers = can('onb.pipeline') || can('onb.raise') || can('comp.structure');
  const cands = useLoad(() => (seeOffers ? api<any[]>('/offers') : Promise.resolve([])), [seeOffers]);
  const [tpl, setTpl] = useState<string | null>(null);
  const [req, setReq] = useState('');
  const sel = cands.data?.find((r) => r.id === req) ?? cands.data?.[0];
  const offer = useLoad(() => (sel ? api(`/offers/${sel.id}`) : Promise.resolve(null)), [sel?.id]);
  // Saved with Edit on salary structures (prototype `cfg`).
  const cfg = !!x.can.structure;
  const cur = tpl ?? company.data?.offerTemplate ?? '';
  const save = async () => {
    if (tpl === null || tpl === company.data?.offerTemplate) return;
    try { await api('/comp/offer-template', { method: 'PUT', body: { template: tpl } }); toast('Saved'); setTpl(null); company.reload(); offer.reload(); } catch (e: any) { toast(e.message); }
  };
  return <div className="grid split">
    <div className="card"><h3>Offer letter template</h3>
      <textarea rows={16} style={{ width: '100%', fontFamily: 'inherit' }} disabled={!cfg} value={cur} onChange={(e) => setTpl(e.target.value)} onBlur={save} />
      <p className="mute" style={{ fontSize: 12, margin: '8px 0 6px' }}>Merge fields:</p>
      <div className="row" style={{ gap: 6, flexWrap: 'wrap' }}>{PLACEHOLDERS.map((p) => <span key={p} className="pill n mono">{`{{${p}}}`}</span>)}</div>
    </div>
    <div className="card"><h3>Preview</h3>
      {sel ? <>
        <label className="field" style={{ marginBottom: 10 }}><span>Candidate</span><PersonPicker label="Candidate" required value={sel.id} onChange={(id) => id && setReq(id)}
          people={cands.data!.map((r) => ({ id: r.id, name: r.candidate_name, designation: r.designation, department: r.department,
            detail: [r.designation, r.department, r.status].filter(Boolean).join(' · ') }))} /></label>
        <div style={{ whiteSpace: 'pre-wrap', fontSize: 13, lineHeight: 1.6, border: '1px solid var(--line)', borderRadius: 10, padding: 14 }}>{offer.data?.letter ?? ''}</div>
      </> : cands.data ? <p className="mute">No candidates yet.</p> : <Loading error={cands.error} />}
    </div>
  </div>;
}

// ---------- Structure mapping (prototype: Payroll, Structure mapping) ----------

/** Map people to a salary structure version from a date. Exported for the Payroll page's "Structure mapping" tab. */
export function MapPeople({ s, onClose, onDone }: { s: { id: string; name: string }; onClose: () => void; onDone: () => void }) {
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const [f, setF] = useState<Record<string, string>>({ who: 'type', type: 'Permanent' });
  const go = async () => {
    const body: any = { structureId: s.id, fromDate: f.from ?? '' };
    if (f.who === 'ids') body.ids = (f.ids ?? '').split(/[\s,]+/).filter(Boolean);
    if (f.who === 'type') body.type = f.type;
    if (f.who === 'loc') body.locationId = f.loc ?? '';
    if (f.who === 'all') body.all = true;
    try { const r = await api('/comp/structure-map', { body }); toast(`${r.mapped} people mapped to ${s.name}`); onDone(); } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}><h2 style={{ marginTop: 0 }}>Map people to {s.name}</h2>
    <div className="form">
      <Field label="From date"><input type="date" value={f.from ?? ''} onChange={(e) => setF({ ...f, from: e.target.value })} /></Field>
      <Field label="Who"><select value={f.who} onChange={(e) => setF({ ...f, who: e.target.value })}><option value="type">An employment type</option><option value="loc">A location</option><option value="ids">Chosen employee IDs</option><option value="all">Everyone</option></select></Field>
      {f.who === 'type' && <Field label="Type"><select value={f.type} onChange={(e) => setF({ ...f, type: e.target.value })}>{ALL_TYPES.map((t) => <option key={t}>{t}</option>)}</select></Field>}
      {f.who === 'loc' && <Field label="Location"><select value={f.loc ?? ''} onChange={(e) => setF({ ...f, loc: e.target.value })}><option value="">Choose</option>{(locs.data ?? []).map((l: any) => <option key={l.id} value={l.id}>{l.name}</option>)}</select></Field>}
      {f.who === 'ids' && <label className="field full"><span>Employee IDs</span><textarea rows={3} value={f.ids ?? ''} onChange={(e) => setF({ ...f, ids: e.target.value })} /></label>}
    </div>
    <p className="mute" style={{ fontSize: 12 }}>Months already approved by payroll are not recalculated; map from the open month.</p>
    <div className="row" style={{ justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={go}>Map</button></div></Modal>;
}

// ---------- Pay components (workforce plan, module 3): the library, structures made from components, a preview ----------

type Comp = { code: string; name: string; kind: 'fixed' | 'variable'; side: 'earn' | 'ded'; role?: string; method: string; value: number; of?: string; min?: number; max?: number; cycle?: number;
  taxable: boolean; pf: boolean; esi: boolean; bonus: boolean; payslip: boolean; id?: string; status?: string };
const METHODS: [string, string][] = [['amount', 'Fixed ₹ a month'], ['pct_ctc', '% of CTC'], ['pct_gross', '% of fixed gross'], ['pct_basic', '% of Basic'], ['pct_of', '% of another component'], ['remainder', 'Remainder (balancing)']];
const ROLES: [string, string][] = [['', '—'], ['basic', 'Basic'], ['da', 'DA'], ['hra', 'HRA']];
const CYCLES: [number, string][] = [[1, 'Monthly'], [3, 'Quarterly'], [6, 'Half-yearly'], [12, 'Yearly']];
const FLAGS: [keyof Comp, string][] = [['taxable', 'Taxable'], ['pf', 'PF wage'], ['esi', 'ESI wage'], ['bonus', 'Bonus wage'], ['payslip', 'On payslip']];
const kindText = (c: Comp) => `${c.kind === 'variable' ? 'Variable' : 'Fixed'} ${c.side === 'ded' ? 'deduction' : 'earning'}`;
const flagText = (c: Comp) => FLAGS.filter(([k]) => c[k]).map(([, l]) => l).join(', ') || '—';

function PayComponents({ x, reload }: { x: any; reload: () => void }) {
  const lib = useLoad(() => api('/comp/components'), []);
  const list: any[] = x.structures ?? [];
  const cfg: boolean = x.can.structure;
  const [sel, setSel] = useState<string>(list.find((s) => s.rates?.designed)?.id ?? list.find((s) => s.is_default)?.id ?? list[0]?.id ?? '');
  const [ver, setVer] = useState(0);
  const det = useLoad(() => (sel && sel !== 'new' ? api(`/comp/structures/${sel}/components`) : Promise.resolve(null)), [sel, ver]);
  const [edit, setEdit] = useState<Comp | null>(null);
  const [draft, setDraft] = useState<Comp[] | null>(null);
  const [nm, setNm] = useState('');
  const [copyFrom, setCopyFrom] = useState('');
  const comps: Comp[] = lib.data?.components ?? [];
  if (!lib.data) return <Loading error={lib.error} />;
  const designed = sel === 'new' || det.data?.designed;
  const shown: Comp[] = draft ?? det.data?.comps ?? [];
  const startNew = () => { setSel('new'); setNm(''); setDraft(comps.filter((c) => c.status === 'Active' && ['basic', 'hra', 'sa'].includes(c.code)).map(fromLib)); };
  const saveLib = async (c: Comp) => {
    try { await api(c.id ? `/comp/components/${c.id}` : '/comp/components', { method: c.id ? 'PUT' : 'POST', body: c }); toast('Saved'); setEdit(null); lib.reload(); } catch (e: any) { toast(e.message); }
  };
  const saveStructure = async () => {
    if (!draft) return;
    try {
      if (sel === 'new') {
        const r = await api('/comp/structures/designed', { body: { name: nm.trim(), copyFrom, comps: draft } });
        toast(`${nm.trim()} created. Map people to it in Payroll › Structure mapping`); setDraft(null); reload(); setSel(r.id);
      } else {
        await api(`/comp/structures/${sel}/components`, { method: 'PUT', body: { comps: draft } }); toast('Saved'); setDraft(null); setVer((v) => v + 1);
      }
    } catch (e: any) { toast(e.message); }
  };
  const w = det.data?.wage;
  return <>
    <div className="note" style={{ marginBottom: 14 }}>A structure is made of components: fixed or variable, earning or deduction, each worked out as a fixed amount, a % of CTC, of the fixed gross,
      of Basic or of another component, or as the balancing figure, with an optional minimum and maximum. Variable components are paid as the amount HR enters when the payout is due
      (Payroll › Variable pay). The versions made before components are converted into templates that give exactly the same pay; their rates stay in Salary structure.</div>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp"><h3 style={{ margin: 0 }}>Component library</h3>{cfg && <button className="btn" onClick={() => setEdit({ ...BLANK })}>Add component</button>}</div>
      <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 10px' }}>A structure keeps its own copy of each component, so a change here reaches only structures made or edited afterwards.</p>
      <div className="tw"><table><thead><tr><th>Component</th><th>Type</th><th>Calculation</th><th>Counts as</th><th>Payout</th><th>Status</th>{cfg && <th></th>}</tr></thead><tbody>
        {comps.map((c) => <tr key={c.code}><td><b>{c.name}</b> <span className="mono mute">{c.code}</span>{c.role && <> <Pill k="info">{ROLES.find(([k]) => k === c.role)?.[1]}</Pill></>}</td><td>{kindText(c)}</td>
          <td className="ink2">{compHow(c, comps)}</td><td className="ink2" style={{ fontSize: 12.5 }}>{flagText(c)}</td><td>{c.kind === 'variable' ? CYCLE[c.cycle || 1] : '—'}</td>
          <td>{c.status === 'Active' ? <Pill k="ok">Active</Pill> : <Pill k="n">Off</Pill>}</td>{cfg && <td><button className="btn sm" onClick={() => setEdit({ ...c })}>Edit</button></td>}</tr>)}
      </tbody></table></div>
    </div>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp"><div className="seg">{list.map((y) => <button key={y.id} className={y.id === sel ? 'on' : ''} onClick={() => { setSel(y.id); setDraft(null); }}>{y.name}{y.rates?.designed ? <b>Components</b> : null}{y.is_default && <b>Default</b>}</button>)}
        {sel === 'new' && <button className="on">New structure</button>}</div>
        {cfg && sel !== 'new' && <button className="btn" onClick={startNew}>New structure from components</button>}</div>
      {sel === 'new' && <div className="form" style={{ marginTop: 12 }}>
        <label className="field"><span>Structure name</span><input value={nm} onChange={(e) => setNm(e.target.value)} placeholder="For example Sales 2027" /></label>
        <label className="field"><span>PF, ESI, bonus and other statutory settings from</span><select value={copyFrom} onChange={(e) => setCopyFrom(e.target.value)}>
          <option value="">The default version</option>{list.map((y) => <option key={y.id} value={y.id}>{y.name}</option>)}</select></label></div>}
      {sel !== 'new' && det.data && !designed && <div className="note" style={{ marginTop: 12 }}>{det.data.name} uses percentage rates. Its components below are converted from them and give exactly the pay
        it gave before. Change its rates in Salary structure; to use other components, make a new structure from components and map people to it from a date.</div>}
      {sel !== 'new' && !det.data && <Loading error={det.error} />}
      {(sel === 'new' || det.data) && <CompEditor comps={shown} lib={comps} readOnly={!cfg || !designed} onChange={setDraft} />}
      {w && <p style={{ margin: '10px 0 0', fontSize: 13 }}>Labour-code wage check: Basic + DA is at least {w.pct}% of fixed pay (lowest of ₹15,000, ₹30,000 and ₹60,000 gross, before any variable).{' '}
        {w.ok ? <Pill k="ok">At least 50%</Pill> : <Pill k="warn">Below 50%</Pill>}</p>}
      {cfg && designed && <div className="row" style={{ marginTop: 12 }}>
        <button className="btn primary" disabled={!draft || (sel === 'new' && !nm.trim())} onClick={saveStructure}>{sel === 'new' ? 'Create structure' : 'Save components'}</button>
        {draft && <button className="btn" onClick={() => { setDraft(null); if (sel === 'new') setSel(list[0]?.id ?? ''); }}>Cancel</button>}
        {sel !== 'new' && <span className="mute" style={{ fontSize: 12.5 }}>A change applies to every month not yet approved. To change pay from a date, make a new structure and map people to it from that date.</span>}</div>}
    </div>
    <ComponentPreview x={x} structureId={sel === 'new' ? '' : sel} comps={draft ?? undefined} />
    {edit && <LibEditor c={edit} all={comps} onClose={() => setEdit(null)} onSave={saveLib} />}
  </>;
}

const BLANK: Comp = { code: '', name: '', kind: 'fixed', side: 'earn', role: '', method: 'amount', value: 0, taxable: true, pf: false, esi: true, bonus: false, payslip: true, status: 'Active' };
const fromLib = (c: Comp): Comp => ({ code: c.code, name: c.name, kind: c.kind, side: c.side, role: c.role || '', method: c.method, value: +c.value || 0, of: c.of || '', min: +(c.min ?? 0) || 0,
  max: +(c.max ?? 0) || 0, cycle: c.kind === 'variable' ? c.cycle || 1 : 0, taxable: c.taxable, pf: c.pf, esi: c.esi, bonus: c.bonus, payslip: c.payslip });

/** The components of a structure: method, value, cap, cycle and flags per line; add from the library, remove. */
function CompEditor({ comps, lib, readOnly, onChange }: { comps: Comp[]; lib: Comp[]; readOnly: boolean; onChange: (c: Comp[]) => void }) {
  const set = (i: number, k: keyof Comp, v: any) => onChange(comps.map((c, j) => (j === i ? { ...c, [k]: v } : c)));
  const add = (code: string) => { const c = lib.find((x) => x.code === code); if (c) onChange([...comps, fromLib(c)]); };
  const avail = lib.filter((c) => c.status === 'Active' && !comps.some((x) => x.code === c.code));
  if (readOnly) return <div className="tw" style={{ marginTop: 12 }}><table><thead><tr><th>Component</th><th>Type</th><th>Calculation</th><th>Counts as</th><th>Payout</th></tr></thead><tbody>
    {comps.map((c) => <tr key={c.code}><td><b>{c.name}</b></td><td>{kindText(c)}</td><td className="ink2">{compHow(c, comps)}</td><td className="ink2" style={{ fontSize: 12.5 }}>{flagText(c)}</td>
      <td>{c.kind === 'variable' ? (c.method === 'legacy_var' ? `Every ${c.cycle || 1} month(s) from joining, automatic` : CYCLE[c.cycle || 1]) : '—'}</td></tr>)}</tbody></table></div>;
  const num = (i: number, k: keyof Comp, w = 90) => <input type="number" min={0} step="any" style={{ width: w }} value={(comps[i][k] as number) || ''} onChange={(e) => set(i, k, +e.target.value || 0)} />;
  return <>
    <div className="tw" style={{ marginTop: 12 }}><table><thead><tr><th>Component</th><th>Type</th><th>Role</th><th>Worked out as</th><th>Value</th><th>Min ₹</th><th>Max ₹</th><th>Payout</th><th>Counts as</th><th></th></tr></thead><tbody>
      {comps.map((c, i) => <tr key={c.code}>
        <td><b>{c.name}</b> <span className="mono mute">{c.code}</span></td><td style={{ fontSize: 12.5 }}>{kindText(c)}</td>
        <td><select value={c.role || ''} disabled={c.kind !== 'fixed' || c.side !== 'earn'} onChange={(e) => set(i, 'role', e.target.value)}>{ROLES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></td>
        <td><select value={c.method} onChange={(e) => set(i, 'method', e.target.value)}>{METHODS.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
          {c.method === 'pct_of' && <select value={c.of || ''} onChange={(e) => set(i, 'of', e.target.value)} style={{ marginLeft: 4 }}><option value="">of…</option>
            {comps.filter((o) => o.code !== c.code).map((o) => <option key={o.code} value={o.code}>{o.name}</option>)}</select>}</td>
        <td>{c.method === 'remainder' ? <span className="mute">—</span> : num(i, 'value')}</td>
        <td>{c.method === 'remainder' ? '' : num(i, 'min', 80)}</td><td>{c.method === 'remainder' ? '' : num(i, 'max', 80)}</td>
        <td>{c.kind === 'variable' ? <select value={c.cycle || 1} onChange={(e) => set(i, 'cycle', +e.target.value)}>{CYCLES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select> : '—'}</td>
        <td style={{ whiteSpace: 'nowrap', fontSize: 12 }}>{FLAGS.filter(([k]) => c.side === 'earn' && (c.kind === 'fixed' || k === 'taxable' || k === 'payslip') || k === 'payslip').map(([k, l]) =>
          <label key={k} style={{ marginRight: 8 }}><input type="checkbox" checked={!!c[k]} onChange={(e) => set(i, k, e.target.checked)} /> {l}</label>)}</td>
        <td><button className="btn sm" onClick={() => onChange(comps.filter((_, j) => j !== i))}>Remove</button></td>
      </tr>)}
    </tbody></table></div>
    {avail.length > 0 && <div className="row" style={{ marginTop: 8 }}><select value="" onChange={(e) => add(e.target.value)}><option value="">Add a component from the library…</option>
      {avail.map((c) => <option key={c.code} value={c.code}>{c.name} ({kindText(c).toLowerCase()})</option>)}</select></div>}
    <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Exactly one fixed earning is the balancing figure, and one is marked Basic. Variable pay is never PF, ESI or bonus wage.</p>
  </>;
}

/** Add or change a library component. */
function LibEditor({ c: c0, all, onClose, onSave }: { c: Comp; all: Comp[]; onClose: () => void; onSave: (c: Comp) => void }) {
  const [c, setC] = useState<Comp>(c0);
  const set = (k: keyof Comp, v: any) => setC({ ...c, [k]: v });
  return <Modal onClose={onClose}><h2>{c0.id ? `Edit ${c0.name}` : 'Add a component'}</h2>
    <div className="form">
      <Field label="Name"><input value={c.name} onChange={(e) => set('name', e.target.value)} /></Field>
      <Field label="Code (letters, digits, _)"><input value={c.code} disabled={!!c0.id} onChange={(e) => set('code', e.target.value.toLowerCase())} /></Field>
      <Field label="Fixed or variable"><select value={c.kind} onChange={(e) => set('kind', e.target.value)}><option value="fixed">Fixed</option><option value="variable">Variable</option></select></Field>
      <Field label="Earning or deduction"><select value={c.side} onChange={(e) => set('side', e.target.value)}><option value="earn">Earning</option><option value="ded">Deduction</option></select></Field>
      <Field label="Worked out as"><select value={c.method} onChange={(e) => set('method', e.target.value)}>{METHODS.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
      {c.method === 'pct_of' && <Field label="% of which component"><select value={c.of || ''} onChange={(e) => set('of', e.target.value)}><option value="">Choose</option>{all.filter((o) => o.code !== c.code).map((o) => <option key={o.code} value={o.code}>{o.name}</option>)}</select></Field>}
      {c.method !== 'remainder' && <><Field label={c.method === 'amount' ? '₹ a month' : '%'}><input type="number" min={0} step="any" value={c.value || ''} onChange={(e) => set('value', +e.target.value || 0)} /></Field>
        <Field label="Minimum ₹ a month (optional)"><input type="number" min={0} value={c.min || ''} onChange={(e) => set('min', +e.target.value || 0)} /></Field>
        <Field label="Maximum ₹ a month (optional)"><input type="number" min={0} value={c.max || ''} onChange={(e) => set('max', +e.target.value || 0)} /></Field></>}
      {c.kind === 'fixed' && c.side === 'earn' && <Field label="Role"><select value={c.role || ''} onChange={(e) => set('role', e.target.value)}>{ROLES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>}
      {c.kind === 'variable' && <Field label="Payout"><select value={c.cycle || 1} onChange={(e) => set('cycle', +e.target.value)}>{CYCLES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>}
      <Field label="Status"><select value={c.status || 'Active'} onChange={(e) => set('status', e.target.value)}><option>Active</option><option value="Inactive">Off</option></select></Field>
    </div>
    <div className="row" style={{ margin: '12px 0', flexWrap: 'wrap' }}>{FLAGS.map(([k, l]) => <label key={k} style={{ marginRight: 12, fontSize: 13 }}><input type="checkbox" checked={!!c[k]} onChange={(e) => set(k, e.target.checked)} /> {l}</label>)}</div>
    <div className="row" style={{ justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={() => onSave(c)}>Save</button></div>
  </Modal>;
}

/** Try pay on a structure (or on components being designed): by CTC or by fixed gross, the variable as ₹ or %. */
function ComponentPreview({ x, structureId, comps }: { x: any; structureId: string; comps?: Comp[] }) {
  const [p, setP] = useState<PayIn>({ ...PAY_IN, payBy: 'ctc', amount: '35000', var: '15000' });
  const [type, setType] = useState('Permanent');
  const [res, setRes] = useState<any>(null);
  const [err, setErr] = useState('');
  const body = JSON.stringify({ ...p, amount: +p.amount || 0, var: +p.var || 0, type, structureId: structureId || undefined, comps });
  useEffect(() => {
    if (!splitPay(p)) { setRes(null); return; }
    const t = setTimeout(() => api('/comp/pay-preview', { body: JSON.parse(body) }).then((r) => { setRes(r); setErr(''); }).catch((e) => { setRes(null); setErr(e.message); }), 300);
    return () => clearTimeout(t);
  }, [body]); // eslint-disable-line react-hooks/exhaustive-deps
  const std = structureId ? x.structures?.find((s: any) => s.id === structureId)?.rates : { ...(x.structures?.find((s: any) => s.is_default)?.rates ?? {}), comps };
  return <div className="card"><h3>Try pay on this structure</h3>
    <div className="form">
      <label className="field"><span>Employment type</span><select value={type} onChange={(e) => setType(e.target.value)}>{ALL_TYPES.map((t) => <option key={t}>{t}</option>)}</select></label>
      <PayEntryFields p={p} set={setP} />
    </div>
    {err && <div className="note bad" style={{ marginTop: 10 }}>{err}</div>}
    {res?.warn && <div className="note warn" style={{ marginTop: 10 }}>{res.warn}.</div>}
    {res?.breakup && <div style={{ marginTop: 10 }}><Statement c={res.breakup} type={type} std={std} rules={x.types} lwf={res.lwfNote} />
      {!res.designed && res.varPct > 0 && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>On a percentage structure the variable part is {res.varPct}% of gross, accrued monthly and paid on its cycle.</p>}</div>}
  </div>;
}
