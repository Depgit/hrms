import { useState } from 'react';
import { api, addDays, addMonths, fmtD, today, ymOf } from '../api';
import { useAuth } from '../auth';
import { Field, download, toast, useLoad } from '../components/ui';
import { usePayState, ymLabel } from './PayrollRun';

// Prototype SFILES, keyed by the server's file names.
const SFILES: { id: string; n: string; d: string; range: boolean }[] = [
  { id: 'insurance', n: 'Insurance: enrolment and removal', d: 'Group medical cover. Joiners to add and leavers to remove. Usually sent weekly.', range: true },
  { id: 'pf', n: 'PF: member enrolment and exit', d: 'Joiners to enrol with PF (UAN) and leavers to mark as exited. Usually sent monthly.', range: true },
  { id: 'esi', n: 'ESI: IP enrolment and exit', d: 'Joiners to register with ESI and leavers to exit. Usually sent monthly.', range: true },
  { id: 'ecr', n: 'PF contribution file (ECR)', d: 'Month’s PF wages and contributions, upload-ready text file.', range: false },
  { id: 'esic', n: 'ESI contribution file', d: 'Month’s ESI wages and days for each insured person.', range: false },
];
// Prototype rpCell: numbers in Indian grouping, ISO dates as "01 Jan 2026".
const cell = (v: any) => v === '' || v == null ? '' : typeof v === 'number' ? v.toLocaleString('en-IN') : /^\d{4}-\d{2}-\d{2}$/.test(String(v)) ? fmtD(String(v)) : String(v);
const lastDay = (ym: string) => addDays(addMonths(ym, 1) + '-01', -1);
const sfRange = (k: string): [string, string] => {
  const t = today();
  if (k === 'week') return [addDays(t, -6), t];
  if (k === 'last') { const p = addMonths(t.slice(0, 7), -1); return [p + '-01', lastDay(p)]; }
  return [t.slice(0, 7) + '-01', t];
};

/** Statutory and insurance files (prototype vStat): prepare a file for any period, look at it, download it. */
export default function Statutory() {
  const { can } = useAuth();
  const st = usePayState();
  const ex = can('pay.export', 2);
  const open = st.data?.openYm ?? ymOf();
  const [s, setS] = useState({ type: 'insurance', from: sfRange('week')[0], to: sfRange('week')[1], ym: '', city: '', skip: true });
  const [res, setRes] = useState<any>(null);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const made = useLoad(() => api('/statutory/history'), []);
  const sp = SFILES.find((x) => x.id === s.type)!;
  const ym = s.ym || open;
  const months = Array.from({ length: 12 }, (_, i) => addMonths(open, -i));
  const upd = (p: Partial<typeof s>) => { setS({ ...s, ...p }); setRes(null); };
  const qs = (sp.range ? `from=${s.from}&to=${s.to}` : `ym=${ym}`) + (s.city ? `&city=${s.city}` : '');
  const period = sp.range ? `${s.from}_to_${s.to}` : ym;
  const gen = async () => {
    if (sp.range && s.from > s.to) { toast('From date must be before the To date'); return; }
    try { const r = await api(`/statutory/${s.type}?${qs}`); setRes({ ...r, sp, period, qs, skip: s.skip }); } catch (e: any) { toast(e.message); setRes(null); }
  };
  const rows: any[] = res ? res.rows.filter((x: any) => !(res.skip && x.sent)) : [];
  const dl = async (txt: boolean) => {
    if (!ex) { toast('You do not have download access'); return; }
    try {
      if (txt) { await download(`/statutory/ecr.txt?${res.qs}`, `ecr_${res.period}.txt`); return; }
      await download(`/statutory/${s.type}/sheet?${res.qs}${sp.range && res.skip ? '&skipSent=1' : ''}&format=csv`, `${s.type}_${res.period}.csv`);
      made.reload();
    } catch (e: any) { toast(e.message); }
  };
  const files: any[] = made.data?.files ?? [];
  const missing = rows.some((x) => x.cells.includes('UAN missing') || x.cells.includes('IP number missing') || x.missing);
  return (
    <>
      <div className="grid g2" style={{ alignItems: 'start', marginBottom: 14 }}>
        <div className="card"><h3>Create a file</h3><p className="ink2" style={{ margin: '0 0 12px', fontSize: 13 }}>{sp.d} Create it whenever you need it, for any period.</p>
          <div className="form">
            <Field label="File"><select value={s.type} onChange={(e) => upd({ type: e.target.value })}>{SFILES.map((x) => <option key={x.id} value={x.id}>{x.n}</option>)}</select></Field>
            {sp.range ? <><Field label="From"><input type="date" value={s.from} onChange={(e) => upd({ from: e.target.value })} /></Field>
              <Field label="To"><input type="date" value={s.to} onChange={(e) => upd({ to: e.target.value })} /></Field></>
              : <Field label="Payroll month"><select value={ym} onChange={(e) => upd({ ym: e.target.value })}>{months.map((m) => <option key={m} value={m}>{ymLabel(m)}</option>)}</select></Field>}
            <Field label="City"><select value={s.city} onChange={(e) => upd({ city: e.target.value })}><option value="">All cities</option>{(cities.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
          </div>
          {sp.range && <>
            <div className="row" style={{ gap: 8, margin: '10px 0 0' }}>{[['week', 'Last 7 days'], ['month', 'This month'], ['last', 'Last month']].map(([k, l]) => <button key={k} className="btn" onClick={() => { const [a, b] = sfRange(k); upd({ from: a, to: b }); }}>{l}</button>)}</div>
            <label className="row" style={{ gap: 8, marginTop: 12, fontSize: 13 }}><input type="checkbox" checked={s.skip} onChange={(e) => upd({ skip: e.target.checked })} /> Leave out people already sent in an earlier file</label>
          </>}
          <div className="row" style={{ marginTop: 14 }}><button className="btn primary" onClick={gen}>Prepare file</button></div>
        </div>
        <div className="card"><h3>Files created so far</h3>{files.length ? <div className="tw"><table><thead><tr><th>File</th><th>Period</th><th className="num">Rows</th><th>By</th><th></th></tr></thead><tbody>
          {files.slice(0, 8).map((g) => <tr key={g.id}><td>{SFILES.find((x) => x.id === g.file)?.n ?? g.name}{g.city ? `, ${g.city}` : ''}<div className="mute" style={{ fontSize: 12 }}>{g.at}</div></td><td>{g.period}</td><td className="num">{g.rows}</td><td>{g.by}</td>
            <td>{ex && <button className="btn" onClick={() => download(`/statutory/history/${g.id}/sheet?format=csv`, `${g.file}_${g.period}.csv`).catch((e) => toast(e.message))}>Download</button>}</td></tr>)}
        </tbody></table></div> : <p className="mute" style={{ margin: 0 }}>Nothing created yet.</p>}</div>
      </div>
      {res && <div className="card">
        <div className="row sp" style={{ marginBottom: 10 }}><div><h3 style={{ margin: 0 }}>{res.sp.n}</h3><div className="mute" style={{ fontSize: 12 }}>{res.period} · {rows.length} row{rows.length === 1 ? '' : 's'}</div></div>
          {ex && rows.length > 0 && <div className="row" style={{ gap: 8 }}><button className="btn primary" onClick={() => dl(false)}>Download CSV</button>{s.type === 'ecr' && <button className="btn" onClick={() => dl(true)}>Download ECR text</button>}</div>}</div>
        {rows.length ? <><div className="tw"><table><thead><tr>{res.cols.map((c: string) => <th key={c}>{c}</th>)}</tr></thead><tbody>
          {rows.slice(0, 50).map((x: any) => <tr key={x.key}>{x.cells.map((v: any, i: number) => <td key={i}>{cell(v)}</td>)}</tr>)}
        </tbody></table></div>{rows.length > 50 && <div className="mute" style={{ fontSize: 12, marginTop: 8 }}>Showing 50 of {rows.length}. The download has all rows.</div>}</>
          : <p className="mute" style={{ margin: 0 }}>No one to include for this period.</p>}
        {missing && <div className="note bad" style={{ marginTop: 10 }}>Some people have no UAN or IP number. Collect it before you upload.</div>}
      </div>}
    </>
  );
}
