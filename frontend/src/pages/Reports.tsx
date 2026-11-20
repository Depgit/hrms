import { useEffect, useState } from 'react';
import { api, addMonths, fmtD, inr, today, ymOf } from '../api';
import { useAuth } from '../auth';
import { Loading, download, toast, useLoad } from '../components/ui';
import { BankFiles, FullRegisterButton } from './PayrollRun';

/** `extra` are the report's own filters (prototype `x`): query key, label and options [value, label]. */
type Rep = { id: string; group: string; name: string; hint: string; filters: string[]; extra?: { k: string; l: string; o: [string, string][] }[] };
/** `money`: columns shown in rupees; `sum`: the Total row (prototype total rows); `note`: a line under the table. */
type Res = { name: string; cols: string[]; rows: any[][]; total: number; money?: number[]; sum?: any[]; note?: string };

// The prototype's report names, descriptions and order (REPS), by the server's report id.
const PROTO: [string, string, string][] = [
  ['headcount', 'Headcount summary', 'Count of people by city, department, type, grade or gender'],
  ['emp_master', 'Employee master list', 'Every employee with job, location and reporting details'],
  ['joiners', 'New joiners', 'People who joined in the period, with the details the medical insurer, EPF and ESIC need'],
  ['exits', 'Exits and attrition', 'Last working day in the period, with tenure and reason'],
  ['hire_dropoff', 'Hiring drop-off', 'Where candidates are lost after the offer, by month, property, role or recruiter'],
  ['on_notice', 'Employees on notice', 'Resignations in progress with last working day'],
  ['probation', 'Probation and confirmation due', 'Probation end date comes from each person’s work policy and grade'],
  ['birthdays', 'Birthdays and work anniversaries', 'For one month'],
  ['att_summary', 'Monthly attendance summary', 'Present, late, half day and missing punches with loss of pay days'],
  ['late', 'Late coming', 'Most late arrivals first'],
  ['roster_late', 'Roster discipline', 'Roster changes made close to the shift, with reason and who changed them'],
  ['leave_bal', 'Leave balances', 'Casual, privilege and sick leave as on today'],
  ['register', 'Payroll register', 'Month-wise earnings, deductions and net pay for each employee'],
  ['cost', 'Payroll cost', 'Gross, employer contributions and total cost by city, department, type or grade'],
  ['statutory', 'Statutory summary', 'PF, ESI, labour welfare fund and TDS to pay, by city'],
  ['bank', 'Bank advice', 'Who gets paid how much, to which account'],
  ['pay_lines', 'Arrears, recoveries and claims', 'Extra pay lines for the month'],
  ['retainer', 'Retainer and contractual payments', 'People paid without payslips or leave'],
  ['approvals', 'Approval requests', 'Every request with its stage and result'],
  ['workload', 'Approver workload', 'Pending requests grouped by who must act next'],
  ['missing', 'Missing details and IDs', 'Pending PAN, Aadhaar, bank, UAN, ESIC number, date of birth, mobile'],
  ['masters', 'Master data and usage', 'All masters including inactive ones, with active employees still using them'],
  ['activity', 'Activity log', 'Recent changes made in the system'],
  ['tds', 'TDS on salary', 'Yearly tax, tax deducted so far and TDS for the month, permanent staff and retainers'],
  ['deviations', 'Roster deviations', 'Planned shift against actual punches, with the manager’s decision'],
  ['disc_score', 'Employee discipline score', 'Score for the month, lowest first'],
  ['mgr_score', 'Manager planning score', 'How well each manager’s rosters match how the team really works'],
  ['consequences', 'Warning letters and PIPs', 'Every warning letter and improvement plan, with number, approver and result'],
  ['nominations', 'Nomination status', 'Every employee and nomination, with status and due date'],
  ['changes', 'Employee change history', 'Every change to an employee record with reason, effective date and who made it'],
  ['holds', 'Salary holds', 'Every salary hold with reason, held amount and release'],
  ['deductions', 'Manual deductions', 'Cash shortage, damage and other deductions with approval and balance'],
  ['inv_moves', 'Inventory: opening, additions, issues and closing', 'Uniform and assets by store for one month. Closing is matched with the serial numbers on hand.'],
  ['inv_deductions', 'Inventory issued to employees and salary deduction', 'Each issue and return in the month, with the recovery from salary where it applies'],
  ['separations', 'Separations', 'Every exit with type, stage, clearance and full and final'],
  ['exit_interviews', 'Exit interviews', 'Reasons for leaving and ratings, to spot patterns by location or manager'],
];
// The prototype's filters for each report (REPS[].flt, with ym → month and grp → group), in its order; the server takes
// the same ones. A filter the server does not take is left out. Without a range a report covers all dates.
const FLT: Record<string, string[]> = {
  headcount: ['group', 'city', 'dept', 'type'], emp_master: ['city', 'dept', 'type', 'status'], joiners: ['range', 'city', 'dept', 'type'],
  exits: ['range', 'city', 'dept', 'type'], on_notice: ['city', 'dept', 'type'], probation: ['range', 'city', 'dept', 'type'], birthdays: ['month', 'city', 'dept'],
  att_summary: ['month', 'city', 'dept', 'type'], late: ['month', 'city', 'dept'], roster_late: ['month', 'city', 'dept'], leave_bal: ['city', 'dept', 'type'],
  register: ['month', 'city', 'dept', 'type'], cost: ['month', 'group', 'city', 'dept', 'type'], statutory: ['month', 'dept', 'type'], bank: ['month', 'city', 'type'],
  pay_lines: ['month', 'city'], retainer: ['month', 'city', 'dept'], approvals: ['city'], workload: [], missing: ['city', 'dept', 'type'], masters: [], activity: [],
  tds: ['month', 'city', 'dept', 'type'], deviations: ['range', 'city', 'dept'], disc_score: ['month', 'city', 'dept'], mgr_score: ['month'],
  consequences: ['range', 'city', 'dept'], nominations: ['city', 'dept'], changes: ['range', 'city', 'dept'], holds: ['city', 'dept'], deductions: ['city', 'dept'],
  inv_moves: ['month'], inv_deductions: ['month', 'city'], separations: ['city', 'dept', 'type'], exit_interviews: ['city', 'dept'],
  hire_dropoff: ['range', 'city', 'dept'],
};
// The filters shown for a report: the prototype's that the server takes (a month also when the server takes a range).
const shown = (r: Rep) => (FLT[r.id] ?? r.filters).filter((k) => r.filters.includes(k) || (k === 'month' && r.filters.includes('range')));
const lastDay = (ym: string) => `${ym}-${String(new Date(+ym.slice(0, 4), +ym.slice(5, 7), 0).getDate()).padStart(2, '0')}`;
// The prototype's order for a report's own choices (Nomination: EPF, gratuity, group insurance, medical).
const OPT_ORDER = ['Provident fund (EPF) nomination', 'Gratuity nomination', 'Group insurance beneficiary', 'Medical insurance family members'];
const optSort = (o: [string, string][]) => { const i = (l: string) => { const k = OPT_ORDER.indexOf(l); return k < 0 ? -1 : k; }; return o.some(([, l]) => OPT_ORDER.includes(l)) ? [...o].sort((a, b) => i(a[1]) - i(b[1])) : o; };
const PI = (id: string) => { const i = PROTO.findIndex((p) => p[0] === id); return i < 0 ? PROTO.length : i; };
// Prototype GRPS: the groupings of the headcount and payroll cost reports.
const GROUPS: [string, string][] = [['city', 'City'], ['location', 'Location'], ['department', 'Department'], ['type', 'Employment type'], ['grade', 'Grade'], ['gender', 'Gender']];
const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });
// The month a From–To range covers exactly ('' when it is not one whole month): the "Joined in month" preset.
const joinedMonth = (f: Record<string, string>) => (f.from?.endsWith('-01') && f.to === lastDay(f.from.slice(0, 7)) ? f.from.slice(0, 7) : '');
// Twelve months back from the last closed month, the payroll month reports usually look at.
const months = () => Array.from({ length: 12 }, (_, i) => addMonths(ymOf(), -1 - i));
// Prototype rpCell: rupees in money columns, numbers in the Indian style, dates as 04 Nov 2026.
const cell = (v: any, money?: boolean) => {
  if (v == null || v === '') return '';
  if (typeof v === 'number') return money ? inr(v) : v.toLocaleString('en-IN', { maximumFractionDigits: 2 });
  const s = String(v);
  if (/^\d{4}-\d{2}-\d{2}$/.test(s)) return fmtD(s);
  const dt = /^(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2})$/.exec(s);
  if (dt) return `${fmtD(dt[1])}, ${dt[2]}`;
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(s)) { const d = new Date(s); return isNaN(d.getTime()) ? s : `${fmtD(s.slice(0, 10))}, ${d.toLocaleTimeString('en-IN', { hour: '2-digit', minute: '2-digit' })}`; }
  return s;
};

/** Reports (spec 20): every report within your data access; downloads need the report download right. */
export default function Reports() {
  const { can } = useAuth();
  const d = useLoad(() => api<{ reports: Rep[]; canDownload: boolean }>('/reports'), []);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const depts = useLoad(() => api<any[]>('/masters/department?status=Active'), []);
  const types = useLoad(() => api<any[]>('/masters/employment_type?status=Active'), []);
  const [id, setId] = useState('');
  const [f, setF] = useState<Record<string, string>>({ from: addMonths(ymOf(), -1) + '-01', to: today(), ym: addMonths(ymOf(), -1), group: 'city' });
  const [q, setQ] = useState('');
  const [res, setRes] = useState<Res | null>(null);
  const [err, setErr] = useState('');
  // Prototype rpAllowed: master data needs the masters right; separation reports need the separation right.
  const allowed = (r: Rep) => (r.id !== 'masters' || can('mas.org') || can('mas.job')) && (!['separations', 'exit_interviews'].includes(r.id) || can('emp.sep'));
  const reps = d.data ? d.data.reports.filter(allowed).sort((a, b) => PI(a.id) - PI(b.id)) : [];
  const rp = reps.find((r) => r.id === id) ?? reps[0];
  const fl = rp ? shown(rp) : [];
  const qs = () => {
    const p: Record<string, string> = {};
    fl.forEach((k) => {
      if (k === 'range') { p.from = f.from; p.to = f.to; } else if (k === 'month' && !rp!.filters.includes('month')) { p.from = f.ym + '-01'; p.to = lastDay(f.ym); }
      else if (k === 'month') p.ym = f.ym; else if (k === 'group') p.group = f.group || 'city'; else if (f[k]) p[k] = f[k];
    });
    rp?.extra?.forEach((x) => { if (f[x.k]) p[x.k] = f[x.k]; });
    return new URLSearchParams(p).toString();
  };
  const key = rp ? rp.id + '?' + qs() : '';
  // The report runs as soon as it is picked and again on every filter change, like the prototype.
  useEffect(() => {
    if (!rp) return;
    let live = true;
    setErr('');
    api<Res>(`/reports/${rp.id}?${qs()}`).then((r) => live && setRes(r)).catch((e) => { if (live) { setRes(null); setErr(e.message); } });
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  if (!d.data) return <Loading error={d.error} />;
  if (!rp) return <div className="card"><p className="mute" style={{ margin: 0 }}>No reports for your role.</p></div>;
  const cats = [...new Set(reps.map((r) => r.group))];
  // A report's own filters (Workflow, Decision, …) start at All on every report.
  const pick = (x: string) => { if (x === rp.id) return; setId(x); setQ(''); setRes(null); setF((o) => ({ ...o, wf: '', rs: '', ds: '', late: '' })); };
  const setFl = (k: string, v: string) => { setF({ ...f, [k]: v }); setQ(''); };
  const meta = PROTO.find((p) => p[0] === rp.id);
  const sel = (k: string, label: string, list: [string, string][], blank?: string) => <label className="field" key={k}><span>{label}</span>
    <select value={f[k] || ''} onChange={(e) => setFl(k, e.target.value)}>{blank != null && <option value="">{blank}</option>}{list.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></label>;
  const fresh = res && res.name ? res : null;
  const body = fresh?.rows ?? [];
  const ql = q.toLowerCase();
  const rows = ql ? body.filter((x) => x.some((c) => String(c ?? '').toLowerCase().includes(ql))) : body;
  const cap = 300, show = rows.slice(0, cap), count = ql ? rows.length : fresh?.total ?? 0;
  const mon = new Set(fresh?.money ?? []);
  const numCol = (i: number) => mon.has(i) || typeof body.find((x) => x[i] != null && x[i] !== '')?.[i] === 'number';
  // The server's Total row covers every row of the report; like the prototype it is hidden while searching.
  const tot = fresh?.sum && !ql && body.length ? fresh.sum : null;
  return (
    <div className="grid" style={{ gridTemplateColumns: '260px minmax(0,1fr)', alignItems: 'start', gap: 16 }}>
      <div className="card" style={{ padding: 12 }}>
        {cats.map((c) => <div key={c}><div className="mute" style={{ fontSize: 11, fontWeight: 700, letterSpacing: '.06em', textTransform: 'uppercase', margin: '10px 8px 4px' }}>{c}</div>
          {reps.filter((r) => r.group === c).map((r) => <button key={r.id} className={`nav ${r.id === rp.id ? 'on' : ''}`} style={{ width: '100%', textAlign: 'left' }} onClick={() => pick(r.id)}>{PROTO.find((p) => p[0] === r.id)?.[1] ?? r.name}</button>)}</div>)}
      </div>
      <div className="card">
        <div className="row sp" style={{ marginBottom: 12, alignItems: 'flex-start' }}>
          <div><h2 style={{ margin: 0, fontSize: 18 }}>{meta?.[1] ?? rp.name}</h2><div className="mute" style={{ fontSize: 13 }}>{meta?.[2] ?? rp.hint}</div></div>
          <div className="row" style={{ gap: 8, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
            {rp.id === 'register' && <FullRegisterButton ym={f.ym} label={`Download full register, ${ymLabel(f.ym)}`} />}
            {d.data.canDownload ? <>
              <button className="btn" onClick={() => download(`/reports/${rp.id}/sheet?${qs()}`, `${rp.id}.xlsx`).catch((e) => toast(e.message))}>Download Excel</button>
              <button className="btn primary" onClick={() => download(`/reports/${rp.id}/sheet?${qs()}${qs() ? '&' : ''}format=csv`, `${rp.id}.csv`).catch((e) => toast(e.message))}>Download CSV</button></>
              : <span className="mute" style={{ fontSize: 12 }}>Download needs export access</span>}
          </div>
        </div>
        {rp.id === 'bank' && <BankFiles key={f.ym} ym={f.ym} card={false} />}
        <div className="form" style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', marginBottom: 12 }}>
          {fl.map((k) => k === 'month' ? sel('ym', 'Month', months().map((m) => [m, ymLabel(m)]))
            : k === 'group' ? sel('group', 'Group by', GROUPS)
            : k === 'city' ? sel('city', 'City', (cities.data || []).map((c) => [c.id, c.name]), 'All cities')
            : k === 'dept' ? sel('dept', 'Department', (depts.data || []).map((x) => [x.name, x.name]), 'All departments')
            : k === 'type' ? sel('type', 'Employment type', (types.data || []).map((x) => [x.name, x.name]), 'All types')
            : k === 'status' ? sel('status', 'Status', [['Active', 'Active'], ['Pre-boarding', 'Pre-boarding'], ['On notice', 'On notice'], ['Inactive', 'Inactive']], 'All')
            : k === 'range' ? <span key={k} style={{ display: 'contents' }}>
              {rp.id === 'joiners' && <label className="field"><span>Joined in month</span>
                <select value={joinedMonth(f)} onChange={(e) => { const m = e.target.value; if (m) { setF({ ...f, from: m + '-01', to: lastDay(m) }); setQ(''); } }}>
                  <option value="">Dates below</option>{[ymOf(), ...months()].map((m) => <option key={m} value={m}>{ymLabel(m)}</option>)}</select></label>}
              <label className="field"><span>From</span><input type="date" value={f.from} onChange={(e) => setFl('from', e.target.value)} /></label>
              <label className="field"><span>To</span><input type="date" value={f.to} onChange={(e) => setFl('to', e.target.value)} /></label></span> : null)}
          {rp.extra?.map((x) => sel(x.k, x.l, optSort(x.o)))}
          <label className="field"><span>Search in results</span><input type="search" value={q} placeholder="Type to filter" onChange={(e) => setQ(e.target.value)} /></label>
        </div>
        {err ? <div className="note bad">{err}</div> : !fresh ? <Loading /> : <>
          <div className="tw"><table><thead><tr>{fresh.cols.map((c, i) => <th key={c + i} className={numCol(i) ? 'num' : ''}>{c}</th>)}</tr></thead><tbody>
            {show.length ? show.map((r, i) => <tr key={i}>{r.map((v, j) => <td key={j} className={typeof v === 'number' ? 'num' : ''}>{cell(v, mon.has(j))}</td>)}</tr>)
              : <tr><td colSpan={fresh.cols.length} className="mute" style={{ padding: 22, textAlign: 'center' }}>No records for these filters</td></tr>}
            {tot && <tr style={{ fontWeight: 700, background: 'var(--soft)' }}>{tot.map((v, j) => <td key={j} className={typeof v === 'number' ? 'num' : ''}>{cell(v, mon.has(j))}</td>)}</tr>}
          </tbody></table></div>
          <div className="mute" style={{ fontSize: 12, marginTop: 8 }}>{count} record{count === 1 ? '' : 's'}{count > show.length ? ` · showing the first ${show.length}. Download the file for all of them` : ''}</div>
          {fresh.note && <div className="note" style={{ marginTop: 10 }}>{fresh.note}</div>}
        </>}
      </div>
    </div>
  );
}
