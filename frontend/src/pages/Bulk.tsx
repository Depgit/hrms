import { useState, type ReactNode } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, fmtD, postForm, today } from '../api';
import { Loading, Modal, download, toast, useLoad } from '../components/ui';

type Tpl = { key: string; name: string; cols: string[]; hint: string; data: boolean; note?: string; zip?: boolean };
/** A checked row; eff and reason are the row's Effective From and Reason for change, shown on every change (prototype c.eff, c.reason). */
type Change = { field: string; from: string; to: string; note?: string; reason?: string };
type Res = { line: number; id: string; name?: string; errors: string[]; eff?: string; reason?: string; changes: Change[] };
/** The prototype's "· from 24 Nov 2026 (scheduled)". */
const fromEff = (d: string) => ` · from ${fmtD(d)}${d > today() ? ' (scheduled)' : d < today() ? ' (back-dated)' : ''}`;

// The templates by group, in the prototype's order (TPL) where it had them; templates only the app has follow. Attendance
// and roster uploads live on the Attendance and Shift and roster pages, and a gross revision is a Salary breakup row, as in
// the prototype. A template not listed here goes under Other.
const GROUPS: [string, string[]][] = [
  ['Employees', ['upd', 'off', 'new', 'pers', 'uni', 'cards']],
  ['Pay and dues', ['sal', 'band', 'bud', 'incentive', 'openadv', 'openuni']],
  ['Leave and time', ['lvopen', 'polmap', 'shiftmap']], // polmap: Work policy mapping (Work policies › Applies to); shiftmap: Shift mapping (Masters › Shifts)
];
const ELSEWHERE = ['att', 'roster', 'gross'];
// The prototype writes a change as "from 24 Nov 2026 (scheduled) · Promotion"; the server's note says "scheduled from …".
const when = (n = '') => n === 'from today' ? `from ${fmtD(today())}` : n.replace(/^scheduled from (.+)$/, 'from $1 (scheduled)').replace(/^back-dated to (.+)$/, 'from $1 (back-dated)');
// The prototype's help text (TPL[..].hint); the server's text is used for the app-only templates.
const HINT: Record<string, string> = {
  upd: 'Blank cells keep the current value. Every row that changes something needs a Reason for change from the list in Masters and an Effective From date. A past date is recorded as back-dated, a future date is scheduled and applies by itself on that day. Values must match an active master. If the department changes, give the designation too. If the city changes, give the location too. Managers are given by Employee ID. Role and Skill must be active masters. Medical is the monthly medical insurance (₹) the company pays for the employee, replacing the salary structure\'s rate (Standard goes back to it); Accommodation is the monthly accommodation deduction for permanent staff, as in Salary breakup. Medical and Accommodation need the salary right. Import employees is for new people, once: the full record with opening CL/PL/SL balances, uniform sizes, personal, bank and ID details and Attendance code. Employee master update, Personal details, Uniform sizes and Opening leave balances are for later changes: download the current data (it already holds what the import gave) and change only what changed.',
  off: 'Last Working Day is the date the employee becomes inactive. Reason must be one of the separation reasons in Masters. Employees are retired, never deleted.',
  sal: 'Every changed row needs a Reason for change and an Effective From date. Pay changes apply from the payroll month of that date: a past month raises arrears for approval, a future month is scheduled. Download the current data first. Basic, HRA, special allowance, bonus, PF, ESI, LWF, gratuity, CTC and net are calculated from the salary structure and shown for reference in the download; they are ignored on upload. Blank cells keep the current value. Variable pay applies to types that earn it, and meal and accommodation deductions apply to permanent staff only. Reimbursement (₹) is the fixed Reimbursement a month inside the gross (Basic and HRA first, then the Reimbursement, the special allowance takes the rest); it cannot be more than what Basic and HRA leave, and 0 removes it. Variable payouts go in the same sheet: Payout Month, Payout Component and Variable Payout (₹) (and a Payout Note) pay a variable component in a month it is due; a blank Variable Payout (₹) pays nothing new. A row may change the salary, give a payout, or both. Payouts need Payroll (run or claims at Edit) or HR Admin.',
  bud: 'One row for each city or location. Level is City or Location, and Name is its name. City rows take a headcount budget only; a city cost budget is the total of its locations. Monthly Cost Budget (CTC) is the total monthly cost to the company: salary plus the employer PF, ESI and other contributions. Blank cells keep the current value. Use 0 for no budget.',
};

/** The templates the role has, in their groups (empty groups left out). */
function grouped(all: Tpl[]): [string, Tpl[]][] {
  const tpls = all.filter((x) => !ELSEWHERE.includes(x.key));
  const out: [string, Tpl[]][] = GROUPS.map(([g, keys]) => [g, keys.map((k) => tpls.find((x) => x.key === k)).filter((x): x is Tpl => !!x)]);
  const known = GROUPS.flatMap(([, keys]) => keys);
  out.push(['Other', tpls.filter((x) => !known.includes(x.key))]);
  return out.filter(([, l]) => l.length > 0);
}

/** One checked change. A change with no "from" is a line of a plan (the opening templates): what was recovered before the
 *  HRMS, the HRMS deductions month by month, the refund. */
function ChangeText({ r, c }: { r: Res; c: Change }) {
  if (!c.from) return <div><span className="ink2" style={{ fontWeight: 700 }}>{c.field}:</span> {c.to}{c.note ? <span className="mute"> · {c.note}</span> : null}</div>;
  return <span className="pill ok">{c.field}: {c.from} → {c.to}{r.eff ? fromEff(r.eff) : c.note ? ` · ${when(c.note)}` : ''}{(r.reason || c.reason) ? ` · ${r.reason || c.reason}` : ''}</span>;
}

/** Bulk update (spec 9): choose a template, download it, fill it offline, upload or paste it, check every row, then apply. */
export default function Bulk() {
  const { data, error } = useLoad(() => api<Tpl[]>('/bulk'), []);
  // ?tpl= opens a template, e.g. /bulk?tpl=new from Employees, "Import employees" (HR Admin only).
  const [qp, setQp] = useSearchParams();
  const key = qp.get('tpl') || '';
  if (!data) return <Loading error={error} />;
  const groups = grouped(data);
  const tpls = groups.flatMap(([, l]) => l);
  if (!tpls.length) return <div className="card"><p className="mute" style={{ margin: 0 }}>Your role has no bulk templates.</p></div>;
  const t = tpls.find((x) => x.key === key) ?? tpls[0];
  const group = groups.find(([, l]) => l.includes(t))?.[0] ?? '';
  const pick = (k: string) => { const p = new URLSearchParams(qp); p.set('tpl', k); setQp(p, { replace: true }); };
  return (
    <>
      <div className="card" style={{ marginBottom: 14 }} data-testid="bulk-picker">
        <h3>Choose a template</h3>
        <div className="bulk-groups">
          {groups.map(([g, l]) => <div key={g} className="bulk-group" role="group" aria-label={g}>
            <div className="bulk-glabel">{g}</div>
            <div className="seg">{l.map((x) => <button key={x.key} className={x.key === t.key ? 'on' : ''} aria-pressed={x.key === t.key} onClick={() => pick(x.key)}>{x.name}</button>)}</div>
          </div>)}
        </div>
      </div>
      <BulkPanel key={t.key} t={t} group={group} aside={<div className="card"><h3>How bulk update works</h3>
        <ul className="ink2" style={{ paddingLeft: 18, margin: 0, display: 'grid', gap: 8 }}>
          <li>Choose a template, download it, fill it offline, upload it here.</li>
          <li>Every row is checked against active masters before anything changes.</li>
          <li>Designations must belong to the department, and shifts must be mapped to the department and location.</li>
          <li>Valid rows apply, invalid rows are listed with the reason.</li>
          <li>The “How to fill” sheet in each template explains every column, with an example.</li>
        </ul>
      </div>} />
    </>
  );
}

/** Bulk upload of one template inside the page that owns the data (owner, 24 Nov 2026: upload there, no move to Bulk
 *  update): the same download, check and apply as Bulk update, in a dialog. onApplied runs after rows are applied. */
export function BulkModal({ tpl, title, onClose, onApplied }: { tpl: string; title?: string; onClose: () => void; onApplied?: () => void }) {
  const { data, error } = useLoad(() => api<Tpl[]>('/bulk'), []);
  const t = data?.find((x) => x.key === tpl);
  return <Modal wide onClose={onClose}>
    <div className="row sp" style={{ marginBottom: 6 }}><h3 style={{ margin: 0 }}>{title ?? t?.name ?? 'Bulk upload'}</h3><button className="btn sm" onClick={onClose}>Close</button></div>
    {!data ? <Loading error={error} /> : !t ? <p className="mute" style={{ margin: 0 }}>Your role does not have this bulk template.</p>
      : <BulkPanel t={t} plain onApplied={onApplied} />}
  </Modal>;
}

/** One template: columns, downloads, the file or pasted rows, Check file, Apply, and the check result. */
function BulkPanel({ t, group, aside, plain, onApplied }: { t: Tpl; group?: string; aside?: ReactNode; plain?: boolean; onApplied?: () => void }) {
  const [text, setText] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [res, setRes] = useState<{ ok: number; results: Res[]; summary?: { matched: number; unmatched: number; invalid: number } } | null>(null);
  const [busy, setBusy] = useState(false);
  // A pasted sheet goes to the server as a CSV file; an Excel file goes as it is.
  const payload = () => file ?? new File([text], `${t.key}.csv`, { type: 'text/csv' });
  const run = async (action: 'check' | 'apply') => {
    setBusy(true);
    try {
      const r = await postForm(`/bulk/${t.key}/${action}`, {}, payload());
      if (action === 'apply') { toast(`${r.ok} row${r.ok === 1 ? '' : 's'} applied`); setRes(null); setText(''); setFile(null); onApplied?.(); } else setRes(r);
    } catch (x: any) { toast(x.message); setRes(null); } finally { setBusy(false); }
  };
  const choose = async (f: File | undefined) => {
    if (!f) return;
    setRes(null);
    if (t.zip) { setFile(f); setText(''); return; } // a ZIP of card files goes as it is
    // A CSV file is loaded into the box, like the prototype; an Excel file is checked as it is.
    if (/\.csv$/i.test(f.name) || f.type === 'text/csv') { setText(await f.text()); setFile(null); } else { setFile(f); setText(''); }
  };
  const copy = () => (navigator.clipboard ? navigator.clipboard.writeText(t.cols.join(',')) : Promise.reject()).then(() => toast('Header copied')).catch(() => toast('Copy is not available here. Type the header from the list.'));
  const dl = (data: boolean) => download(`/bulk/${t.key}/download${data ? '?data=1' : ''}`, `${t.key}.xlsx`).catch((e) => toast(e.message));
  const ok = res?.ok ?? 0;
  const bad = res ? res.results.filter((r) => r.errors.length).length : 0;
  const ready = !!text.trim() || !!file;
  const body = <>
    {group && <div className="bulk-glabel">{group}</div>}
    {!plain && <h3 style={{ fontSize: 16 }}>{t.name}</h3>}
    <p className="ink2" style={{ marginTop: 0 }}>{HINT[t.key] ?? t.hint}</p>
    {t.note && <div className="note" style={{ marginBottom: 12 }}>{t.note}</div>}
    {t.zip ? <>
      <label className="field"><span>File names in the ZIP</span><div className="mono" style={{ background: 'var(--soft)', padding: 10, borderRadius: 8, overflowWrap: 'anywhere' }}>EMP-1007_ESIC.pdf · EMP-1007_MEDICAL.pdf · EMP-1008-esic.jpg</div></label>
      <div className="row" style={{ margin: '12px 0', flexWrap: 'wrap' }}>
        <label className="btn">Choose ZIP file<input type="file" accept=".zip,application/zip" hidden onChange={(e) => { choose(e.target.files?.[0]); e.target.value = ''; }} /></label>
      </div>
    </> : <>
    <label className="field"><span>Template columns</span><div className="mono" style={{ background: 'var(--soft)', padding: 10, borderRadius: 8, overflowWrap: 'anywhere' }}>{t.cols.join(',')}</div></label>
    <div className="row" style={{ margin: '12px 0', flexWrap: 'wrap' }}>
      <button className="btn" onClick={copy}>Copy header row</button>
      <button className="btn" onClick={() => dl(false)}>Download template</button>
      {t.data && <button className="btn" onClick={() => dl(true)}>Download current data</button>}
      <label className="btn">Choose Excel or CSV file<input type="file" accept=".xlsx,.csv,text/csv" hidden onChange={(e) => { choose(e.target.files?.[0]); e.target.value = ''; }} /></label>
    </div>
    <label className="field"><span>Paste or load CSV</span>
      <textarea placeholder={t.cols.join(',')} value={file ? '' : text} disabled={!!file} onChange={(e) => { setText(e.target.value); setRes(null); }} /></label>
    </>}
    {file && <div className="row mute" style={{ fontSize: 12.5, marginTop: 6 }}>Loaded {file.name}<button className="btn sm" onClick={() => { setFile(null); setRes(null); }}>Remove</button></div>}
    <div className="row" style={{ marginTop: 12, flexWrap: 'wrap' }}>
      <button className="btn primary" disabled={!ready || busy} onClick={() => run('check')}>Check file</button>
      {res && <button className="btn primary" disabled={!ok || busy} onClick={() => run('apply')}>Apply {ok} valid row{ok === 1 ? '' : 's'}</button>}
    </div>
  </>;
  const result = res && <div className={plain ? '' : 'card'} style={{ marginTop: 14 }} data-testid="bulk-result"><h3>Check result</h3>
    <p className="mute" style={{ marginTop: -6, fontSize: 12.5 }}>{res.summary
      ? `${res.results.length} file${res.results.length === 1 ? '' : 's'}: ${res.summary.matched} matched, ${res.summary.unmatched} unmatched, ${res.summary.invalid} invalid. Nothing has changed yet.`
      : `${res.results.length} row${res.results.length === 1 ? '' : 's'}: ${ok} ready to apply, ${bad} with problems. Nothing has changed yet.`}</p>
    <div className="tw"><table className="bulk-res"><thead><tr><th>Row</th><th>Employee</th><th>Result</th></tr></thead><tbody>
      {res.results.map((r, i) => <tr key={i} style={{ verticalAlign: 'top' }}>
        <td className="mono"><span className="mute bulk-rowlbl">Row </span>{r.line || '—'}</td>
        <td>{r.id || ''} {r.name && <span className="mute">{r.name}</span>}</td>
        <td>{r.errors.length ? r.errors.map((x, j) => <span key={j}>{j > 0 && ' '}<span className="pill bad" style={{ whiteSpace: 'normal' }}>{x}</span></span>)
          : r.changes.length ? (r.changes.some((c) => !c.from)
            ? <div className="bulk-plan">{r.changes.map((c, j) => <ChangeText key={j} r={r} c={c} />)}</div>
            : r.changes.map((c, j) => <span key={j}>{j > 0 && ' '}<ChangeText r={r} c={c} /></span>))
          : <span className="pill n">No change</span>}</td>
      </tr>)}
    </tbody></table></div>
  </div>;
  if (plain) return <>{body}{result}</>;
  return <>
    <div className="grid split"><div className="card" data-testid="bulk-template">{body}</div>{aside}</div>
    {result}
  </>;
}
