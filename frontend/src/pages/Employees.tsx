import { useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api, fmtD, inr, openDoc, today } from '../api';
import { useAuth } from '../auth';
import { Drawer, Loading, Modal, Pill, Tabs, download, toast, useLoad } from '../components/ui';
import DocCollection from './DocCollection';
import Payslip, { printSlip, type SlipView } from '../components/Payslip';
import { scopeAll, scopeText, typeNames } from './Dashboard';
import { Statement } from './Compensation';
import { UniformBlock } from '../components/Uniform';
import { PfExemptBlock } from './PfExempt';
import { VarTargetField, type VarMode } from '../components/PayEntry';

const TYPES = ['Permanent', 'Retainer', 'Contractual'];
/** What each employment type allows (backend calc.DefaultTypeRules; prototype CFG.types). */
const RULES: Record<string, { variable: boolean; ded: 'all' | 'tds' | 'none'; leave: boolean; docs: boolean }> = {
  Permanent: { variable: true, ded: 'all', leave: true, docs: true },
  Contractual: { variable: true, ded: 'none', leave: false, docs: false },
  Retainer: { variable: false, ded: 'tds', leave: false, docs: false },
};
const rules = (t: string) => RULES[t] ?? RULES.Permanent;
const PILL: Record<string, 'ok' | 'info' | 'n'> = { Active: 'ok', 'Pre-boarding': 'info', Inactive: 'n' };
const VCT: Record<number, string> = { 1: 'every month', 3: 'every 3 months', 6: 'every 6 months', 12: 'once a year' };
const PER = 12;


/** Employees, and for roles that may see employee documents, Document collection (DocCollection.tsx). */
export default function Employees() {
  const { me, can } = useAuth();
  const [qp, setQp] = useSearchParams();
  const docs = can('emp.directory') && can('emp.govid') && me?.ctx.role !== 'Employee';
  const tab = docs && qp.get('tab') === 'docs' ? 'docs' : 'list';
  if (!docs) return <EmployeeList />;
  return <>
    <Tabs tabs={[['list', 'Employees'], ['docs', 'Document collection']]} value={tab} onChange={(t) => setQp(t === 'docs' ? { tab: 'docs' } : {})} />
    {tab === 'docs' ? <DocCollection initialFilter={qp.get('filter') ?? undefined} /> : <EmployeeList />}
  </>;
}

function EmployeeList() {
  const { me, can } = useAuth();
  const nav = useNavigate();
  const [f, setF] = useState({ q: '', city: '', type: '', status: '', page: 1 });
  const [open, setOpen] = useState('');
  const [dlg, setDlg] = useState<'' | 'export' | 'codes'>('');
  const next = useLoad(() => api('/employees/next-code?type=Permanent'), [dlg]);
  const admin = me?.ctx.role === 'HR Admin';
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  // Prototype M.empType: the employment type master.
  const types = useLoad(() => api<any[]>('/masters/employment_type?status=Active').catch(() => []), []);
  const qs = new URLSearchParams(Object.entries(f).filter(([, v]) => v !== '').map(([k, v]) => [k, String(v)])).toString();
  const { data, error, reload } = useLoad(() => api(`/employees?${qs}&size=${PER}`), [qs, me?.ctx.role]);
  const set = (k: string, v: string) => setF({ ...f, [k]: v, page: 1 });
  const pages = data ? Math.max(1, Math.ceil(data.total / PER)) : 1;
  return (
    <>
      {!scopeAll(me?.ctx) && <div className="note" style={{ marginBottom: 14 }}>Your data scope: {scopeText(me?.ctx)}. Other employees are not visible to this role.</div>}
      <div className="row sp" style={{ marginBottom: 12 }}>
        <span className="mute" style={{ fontSize: 13 }}>Next employee code: <b className="mono">{next.data?.next ?? '—'}</b></span>
        <span className="row">
          {can('emp.export', 2) && <button className="btn" onClick={() => setDlg('export')}>Download all details</button>}
          {admin && <><button className="btn" onClick={() => nav('/bulk?tpl=new')}>Import employees</button><button className="btn" onClick={() => setDlg('codes')}>Employee code</button></>}
        </span>
      </div>
      <div className="card">
        <div className="row" style={{ marginBottom: 14 }}>
          <input type="text" placeholder="Search name or employee ID" value={f.q} onChange={(e) => set('q', e.target.value)} style={{ maxWidth: 280 }} />
          <select value={f.city} onChange={(e) => set('city', e.target.value)}><option value="">All cities</option>{cities.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select>
          <select value={f.type} onChange={(e) => set('type', e.target.value)}><option value="">All types</option>{typeNames(types.data).map((t) => <option key={t}>{t}</option>)}</select>
          <select value={f.status} onChange={(e) => set('status', e.target.value)}><option value="">All status</option>{['Active', 'Pre-boarding', 'Inactive'].map((t) => <option key={t}>{t}</option>)}</select>
        </div>
        {error ? <div className="note bad">{error}</div> : !data ? <Loading /> : <>
          <div className="tw"><table>
            <thead><tr><th>ID</th><th>Name</th><th>Type</th><th>City</th><th>Department</th><th>Designation</th><th>Shift</th><th>Status</th></tr></thead>
            <tbody>{data.rows.map((e: any) => (
              <tr key={e.id} className="click" onClick={() => setOpen(e.id)}>
                <td className="mono">{e.id}</td><td><b>{e.name}</b></td><td>{e.type}</td><td>{e.city}</td><td>{e.department}</td><td>{e.designation}</td><td className="mono">{e.shift_code}</td><td><Pill k={PILL[e.status] ?? 'n'}>{e.status}</Pill></td>
              </tr>))}
              {!data.rows.length && <tr><td colSpan={8} className="mute">No employees match these filters.</td></tr>}
            </tbody>
          </table></div>
          <div className="row sp" style={{ marginTop: 12 }}>
            <span className="mute">{data.total} employees · page {f.page} of {pages}</span>
            <span className="row"><button className="btn sm" disabled={f.page <= 1} onClick={() => setF({ ...f, page: f.page - 1 })}>Previous</button><button className="btn sm" disabled={f.page >= pages} onClick={() => setF({ ...f, page: f.page + 1 })}>Next</button></span>
          </div>
        </>}
      </div>
      {open && <EmployeeDrawer id={open} onClose={() => { setOpen(''); reload(); }} />}
      {dlg === 'export' && <ExportModal onClose={() => setDlg('')} />}
      {dlg === 'codes' && <CodesModal onClose={() => setDlg('')} />}
    </>
  );
}

/** Groups the server offers to a role with every right (GET /employees/export/options may send `total`). */
const ALL_SECTIONS = 5;
/** Prototype expModal: Download employee details. */
function ExportModal({ onClose }: { onClose: () => void }) {
  const { data: o, error } = useLoad(() => api('/employees/export/options'), []);
  const types = useLoad(() => api<any[]>('/masters/employment_type?status=Active').catch(() => []), []);
  const [secs, setSecs] = useState<string[] | null>(null);
  const [st, setSt] = useState('');
  const [ty, setTy] = useState('');
  const [err, setErr] = useState('');
  // Prototype expModal: these groups are ticked the first time; Statutory IDs and Service history are left for the user to add.
  const chosen = secs ?? (o?.sections.map((x: any) => x.key).filter((k: string) => ['emp', 'personal', 'bank', 'salary', 'policy', 'appr', 'uniform'].includes(k)) || []);
  const go = async () => {
    setErr('');
    if (!chosen.length) { setErr('Choose at least one group'); return; }
    try {
      await download(`/employees/export?${new URLSearchParams({ sections: chosen.join(','), status: st, type: ty })}`, `employees-${today()}.csv`);
      onClose();
    } catch (x: any) { setErr(x.message); }
  };
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>Download employee details</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    {error && <div className="note bad">{error}</div>}
    {o && <>
      <p className="ink2" style={{ fontSize: 13.5 }}>One row per employee, in the data scope of your role. Fields your role cannot see are shown as Hidden. {o.limited ? 'Your role can download job and location details only.'
        : o.sections.length < (o.total ?? ALL_SECTIONS) ? 'You can download only the groups your role has access to. Groups without access are not offered.' : 'Choose the groups to include.'}</p>
      {!o.canDownload && <div className="note bad">Downloads are switched off for your role. Ask HR Admin to allow them under Access control.</div>}
      <div className="chkgrp" style={{ margin: '10px 0' }}>{o.sections.map((x: any) => <label key={x.key} className="chkl"><input type="checkbox" checked={chosen.includes(x.key)} onChange={(e) => setSecs(e.target.checked ? [...chosen, x.key] : chosen.filter((k: string) => k !== x.key))} />{x.label} <span className="mute">({x.count})</span></label>)}</div>
      <div className="form">
        <label className="field"><span>Status</span><select value={st} onChange={(e) => setSt(e.target.value)}><option value="">All employees</option>{['Active', 'Inactive', 'Pre-boarding'].map((x) => <option key={x}>{x}</option>)}</select></label>
        <label className="field"><span>Employment type</span><select value={ty} onChange={(e) => setTy(e.target.value)}><option value="">All types</option>{typeNames(types.data).map((x) => <option key={x}>{x}</option>)}</select></label>
      </div>
      {!o.limited && <p className="mute" style={{ fontSize: 12.5, marginTop: 10 }}>The file always has code, name, status, type, joining date, last working day and exit reason. Use the same file as the starting point for a bulk update.</p>}
      {err && <div className="note bad">{err}</div>}
      <div className="row" style={{ marginTop: 14 }}><button className="btn primary" disabled={!o.canDownload} onClick={go}>Download CSV</button><button className="btn" onClick={onClose}>Cancel</button></div>
    </>}
  </Modal>;
}

/** Prototype ecModal: Employee code. */
function CodesModal({ onClose }: { onClose: () => void }) {
  const { data, error } = useLoad(() => api('/employee-codes'), []);
  const [v, setV] = useState<any>(null);
  const [err, setErr] = useState('');
  const order = (k: string) => (k === 'All' ? -1 : TYPES.includes(k) ? TYPES.indexOf(k) : TYPES.length);
  const cur = v ?? (data && { ...data, series: [...data.series].sort((a: any, b: any) => order(a.key) - order(b.key)) });
  const setS = (i: number, k: string, val: string | number) => setV({ ...cur, series: cur.series.map((x: any, j: number) => (j === i ? { ...x, [k]: val } : x)) });
  const save = async () => {
    setErr('');
    try {
      await api('/employee-codes', { method: 'PUT', body: { byType: cur.byType, rejoin: cur.rejoin, noEmpId: cur.noEmpId ?? [], series: cur.series.map(({ key, prefix, pad, next }: any) => ({ key, prefix, pad: +pad, next: +next })) } });
      const n = await api('/employees/next-code?type=Permanent').catch(() => null);
      toast(n?.next ? `Saved. Next code: ${n.next}` : 'Saved'); onClose();
    } catch (x: any) { setErr(x.message); }
  };
  const code = (x: any) => x.prefix + String(x.next).padStart(+x.pad || 0, '0');
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>Employee code</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="ink2" style={{ fontSize: 13.5 }}>Codes already on the system, including those you upload, are kept exactly as they are. New employees get the next code in the series. A code is never reused, even after the employee leaves.</p>
    {error && <div className="note bad">{error}</div>}
    {cur && <>
      <label className="field" style={{ maxWidth: 360 }}><span>Series</span><select value={String(cur.byType)} onChange={(e) => setV({ ...cur, byType: e.target.value === 'true' })}><option value="false">One series for everyone</option><option value="true">Separate series for each employment type</option></select></label>
      <div className="tw" style={{ marginTop: 12 }}><table><thead><tr><th>Series</th><th>Prefix</th><th>Digits</th><th>Next number</th><th>Next code</th><th title="Types set to No never get an Employee ID: they keep this series' number from joining">Employee ID</th></tr></thead><tbody>
        {cur.series.map((x: any, i: number) => <tr key={x.key}>
          <td><b>{x.key === 'All' ? 'Everyone' : x.key}</b></td>
          <td><input type="text" value={x.prefix} onChange={(e) => setS(i, 'prefix', e.target.value)} style={{ maxWidth: 110 }} /></td>
          <td><input type="number" min={3} max={8} value={x.pad} onChange={(e) => setS(i, 'pad', e.target.value)} style={{ maxWidth: 80 }} /></td>
          <td><input type="number" min={1} value={x.next} onChange={(e) => setS(i, 'next', e.target.value)} style={{ maxWidth: 120 }} /></td>
          <td className="mono">{code(x)}</td>
          <td>{TYPES.includes(x.key) ? <select aria-label={`${x.key} gets an Employee ID`} value={(cur.noEmpId ?? []).includes(x.key) ? 'no' : 'yes'}
            onChange={(e) => setV({ ...cur, noEmpId: e.target.value === 'no' ? [...(cur.noEmpId ?? []).filter((k: string) => k !== x.key), x.key] : (cur.noEmpId ?? []).filter((k: string) => k !== x.key) })}>
            <option value="yes">Issued after approval</option><option value="no">None: keeps this number</option></select> : <span className="mute">—</span>}</td></tr>)}
      </tbody></table></div>
      <label className="field" style={{ maxWidth: 420, marginTop: 12 }}><span>When someone rejoins</span><select value={cur.rejoin} onChange={(e) => setV({ ...cur, rejoin: e.target.value })}><option value="same">Keep the old code and add a new service period</option><option value="new">Issue a new code and link it to the old one</option></select></label>
      <p className="mute" style={{ fontSize: 12 }}>The next number can only move up. It is always above the highest code already used with that prefix.</p>
      <p className="mute" style={{ fontSize: 12 }}>New joiners work on a Joining ID (JI-…) from their first day and get the Employee ID once every step is approved. An employment type set to <b>None</b> (contract staff, for example) never gets an Employee ID: at joining it takes the next number of its own series above, for good, and is paid and tracked like everyone else.</p>
      {err && <div className="note bad">{err}</div>}
      <div className="row" style={{ marginTop: 14 }}><button className="btn primary" onClick={save}>Save</button><button className="btn" onClick={onClose}>Cancel</button></div>
    </>}
  </Modal>;
}

/** Minimal CSV reader: quoted fields, doubled quotes, commas and new lines inside quotes. */
export function parseCSV(text: string): string[][] {
  const rows: string[][] = [];
  let row: string[] = [], cell = '', q = false;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (q) {
      if (ch === '"' && text[i + 1] === '"') { cell += '"'; i++; } else if (ch === '"') q = false; else cell += ch;
    } else if (ch === '"') q = true;
    else if (ch === ',') { row.push(cell); cell = ''; } else if (ch === '\n' || ch === '\r') {
      if (ch === '\r' && text[i + 1] === '\n') i++;
      row.push(cell); cell = ''; if (row.some((c) => c.trim())) rows.push(row); row = [];
    } else cell += ch;
  }
  row.push(cell);
  if (row.some((c) => c.trim())) rows.push(row);
  return rows;
}

/** The employee record as a side drawer (prototype empDrawer, opened with modal(...,'right')). */
export function EmployeeDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  return <Drawer onClose={onClose}><EmployeeDetail id={id} onClose={onClose} /></Drawer>;
}

/** A dialog opened from the drawer goes to the page root, so the drawer's own styles (.ov.right .dlg) do not apply to it. */
const Top = ({ children }: { children: ReactNode }) => createPortal(children, document.body);
const hid = (v: unknown) => (v == null || v === '' ? 'Hidden' : String(v));
/** Prototype tenureText: whole years and months between joining and the last working day (or today). */
const tenure = (doj: string, lwd?: string | null) => {
  const a = new Date(doj), b = new Date(lwd || today()), m = Math.max(0, Math.round((b.getTime() - a.getTime()) / 2629800000));
  return `${Math.floor(m / 12)} years ${m % 12} months`;
};

/** A person's payslip for a month: the preview from the pay row (GET /api/payslip-preview), or the published payslip when
 *  no preview is available. Without a month: the open payroll month, else the latest published payslip. */
export async function loadSlip(id: string, ym?: string): Promise<SlipView | null> {
  try {
    const v = await api<SlipView>(`/payslip-preview?employeeId=${encodeURIComponent(id)}${ym ? `&ym=${ym}` : ''}`);
    if (v?.snap?.emp?.id === id && (!ym || v.ym === ym)) return v;
  } catch { /* no preview for this person or month: use the published payslip */ }
  if (ym) return api<SlipView>(`/payslips/${ym}/${id}`).catch(() => null);
  const l = await api(`/payslips?employeeId=${encodeURIComponent(id)}`).catch(() => null);
  const last: string | undefined = l?.slips?.[0]?.ym;
  return last ? api<SlipView>(`/payslips/${last}/${id}`) : null;
}

export function EmployeeDetail({ id, onClose }: { id: string; onClose?: () => void }) {
  const { can } = useAuth();
  const nav = useNavigate();
  const { data: e, error, reload } = useLoad(() => api(`/employees/${id}`), [id]);
  const hist = useLoad(() => api<any[]>(`/employees/${id}/history`), [id]);
  const reasons = useLoad(() => api<any[]>('/masters/change_reason?status=Active'), []);
  const structs = useLoad(() => (can('comp.structure') || can('onb.raise', 2) || can('pay.run') ? api<any[]>('/salary/structures').catch(() => []) : Promise.resolve([])), []);
  const [dlg, setDlg] = useState<'' | 'pay' | 'slip' | 'appt' | 'rejoin'>('');
  const [off, setOff] = useState({ reason: '', lwd: today() });
  const [err, setErr] = useState('');
  if (!e) return <Loading error={error} />;
  const R = rules(e.type);
  const pol = e.policy, prob = +(pol?.probMonths || 0), probEnd: string = pol?.probEnds || '', inProb = !!probEnd && probEnd > today();
  const plText = !pol?.pl?.on ? 'Not applicable' : pol.pl.from === 'doj' ? 'Accruing from joining' : inProb ? `Starts after probation, ${fmtD(probEnd)}` : 'Accruing';
  const sepReasons = reasons.data?.filter((r) => r.used_for === 'Separation').map((r) => r.name) || [];
  const reason = off.reason || sepReasons[0] || '';
  const inactivate = async () => {
    setErr('');
    if (off.lwd > today()) { setErr('For a future date, raise a resignation request so notice and approvals apply'); return; }
    if (e.doj && off.lwd < e.doj) { setErr('Last working day cannot be before the joining date'); return; }
    try { await api(`/employees/${id}/inactive`, { body: { lwd: off.lwd, reason } }); toast('Marked inactive. Prepare the full and final in Payroll.'); reload(); hist.reload(); } catch (x: any) { setErr(x.message); }
  };
  const verifyPan = async () => {
    try { const r = await api(`/employees/${id}/pan/verify`, { method: 'POST' }); toast(`PAN check: ${r.status}${r.message ? ' · ' + r.message : ''}`); reload(); hist.reload(); } catch (x: any) { toast(x.message); }
  };
  const undo = async () => {
    setErr('');
    try { await api(`/employees/${id}/reactivate`, { method: 'POST' }); toast('Reactivated'); reload(); hist.reload(); } catch (x: any) { setErr(x.message); }
  };
  const p = e.salary?.pay;
  const structName: string = e.structure ?? e.salary?.structure ?? '';
  const sd = (structs.data?.find((x) => x.name === structName) ?? structs.data?.find((x) => x.is_default))?.rates ?? e.structureStd ?? {};
  const vPct = p?.varPct ?? sd.varPct, vCyc = p?.varCycle ?? sd.varCycle;
  // Prototype payBlock: every structure version the person has had, from its date.
  const sHist = ((e.structureHistory ?? []) as { name: string; from_date: string }[]).map((x) => x.name + (x.from_date < '1990' ? '' : ' from ' + fmtD(x.from_date))).join(' → ');
  return (
    <>
      <div className="row sp"><div><h2>{e.name}</h2><span className="mono mute">{e.id}</span> <Pill k={PILL[e.status] ?? 'n'}>{e.status}</Pill></div>{onClose && <button className="btn sm" onClick={onClose}>Close</button>}</div>
      <dl className="kv" style={{ marginTop: 16 }}>
        <dt>Employment type</dt><dd>{e.type}</dd>
        <dt>City and location</dt><dd>{e.city} · {e.location}</dd>
        <dt>Cluster · state · region</dt><dd>{e.cluster || '—'} · {e.state || '—'} · {e.region || '—'}</dd>
        <dt>Role</dt><dd>{e.department} · {e.designation} · {e.grade}</dd>
        <dt>Job role · skill</dt><dd>{e.job_role || '—'} · {e.skill || '—'}</dd>
        <dt>Approval flow</dt><dd>{e.corporate ? 'Corporate flow' : 'Operations flow'} <span className="mute">from department</span></dd>
        <dt>Shift</dt><dd>{e.shift_code} · {e.attendance_mode}</dd>
        <dt>Reports to</dt><dd>{e.manager ?? '—'} (L1) · {e.functional_manager ?? '—'} (L2)</dd>
        <dt>Date of joining</dt><dd>{fmtD(e.doj)}</dd>
        {pol && <><dt>Work policy</dt><dd>{pol.name} <span className="mute">via {pol.via}</span></dd>
          <dt>Probation</dt><dd>{prob ? `${prob} months, ${inProb ? 'ends ' + fmtD(probEnd) : 'completed'}` : 'Not applicable'}</dd></>}
        {R.leave ? pol && <>
          <dt>Optional holidays</dt><dd>{pol.optHol || 0} a year to choose from the calendar{e.optHolOpening?.left != null ? `; ${e.optHolOpening.left} left this year (opening as at ${fmtD(e.optHolOpening.at)})` : ''}</dd>
          <dt>Casual leave</dt><dd>{pol.cl?.on ? `${pol.cl.days} days a year` : 'Not applicable'}</dd>
          <dt>Privilege leave</dt><dd>{plText}</dd>
        </> : <><dt>Leave</dt><dd>{e.type} staff cannot apply for leave</dd></>}
        <dt>Monthly gross</dt><dd>{e.salary ? inr(e.salary.gross) : 'Hidden'}</dd>
        <dt>CTC · Net</dt><dd>{e.salary?.ctc != null ? `${inr(e.salary.ctc)} · ${inr(e.salary.net)}` : 'Hidden'}</dd>
        <dt>Mobile · Email</dt><dd>{e.personal ? `${e.personal.mobile || '—'} · ${e.personal.email || '—'}` : 'Hidden'}</dd>
        <dt>Date of birth</dt><dd>{e.personal ? fmtD(e.personal.dob) : 'Hidden'}</dd>
        <dt>Emergency contact</dt><dd>{!e.personal ? 'Hidden' : e.personal.ec_name ? `${e.personal.ec_name} (${e.personal.ec_relation || '—'}) · ${e.personal.ec_mobile || '—'}` : '—'}</dd>
        <dt>Father or spouse · children</dt><dd>{!e.personal ? 'Hidden' : [e.personal.father_spouse || '—', e.personal.spouse ? `spouse ${e.personal.spouse}` : '', [e.personal.child1, e.personal.child2].filter(Boolean).join(', ') || 'no children recorded'].filter(Boolean).join(' · ')}</dd>
        <dt>Aadhaar · PAN</dt><dd>{e.govId ? <>{hid(e.govId.aadhaar)} · {hid(e.govId.pan)} <PanBadge c={e.govId.panCheck} /></> : 'Hidden'}
          {e.govId?.pan && can('emp.govid', 2) && <button className="btn sm" style={{ marginLeft: 8 }} onClick={verifyPan}>Verify PAN</button>}</dd>
        <dt>UAN · existing UAN?</dt><dd>{e.govId ? `${hid(e.govId.uan)} · ${e.existingUan || 'Not known'}` : 'Hidden'}</dd>
        <dt>Bank</dt><dd>{e.bank ? `${e.bank.bank_name} ${e.bank.account_no ? '••••' + String(e.bank.account_no).slice(-4) : ''}`.trim() : 'Hidden'}</dd>
        {e.exit_reason && <><dt>Inactive reason</dt><dd>{e.exit_reason}</dd></>}
      </dl>
      {(can('emp.directory') || can('pay.run')) && <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={() => nav(`/file?id=${e.id}`)}>Open payroll file</button><span className="mute" style={{ fontSize: 12.5 }}>Personal, bank, tax, attendance and pay in one place</span></div>}
      <h3 style={{ marginTop: 18 }}>Pay and documents</h3>
      <dl className="kv">
        <dt>Salary structure</dt><dd>{sHist || structName || 'Default'}</dd>
        {R.variable && e.salary?.designed && <><dt>Variable pay target</dt><dd>{e.salary.varTarget == null ? (e.salary.varMode ? 'Hidden' : 'None') : e.salary.varMode === 'pct' ? `${e.salary.varPct}% of fixed gross (${inr(e.salary.varTarget)} a month)` : `${inr(e.salary.varTarget)} a month`}, paid {VCT[e.salary.varCycle] || 'every month'} as HR enters it when due</dd></>}
        {R.variable && !e.salary?.designed && <><dt>Variable pay</dt><dd>{vPct != null ? `${vPct}% of gross` : 'Structure standard'}, paid {VCT[vCyc] || 'every month'}
          {e.salary?.varPayout && <div className="mute" style={{ fontSize: 12.5 }}>Own cycle from {fmtYm(e.salary.varPayout.from)}: next payout {fmtYm(e.salary.varPayout.next)}</div>}</dd></>}
        {e.type !== 'Retainer' && <><dt>Medical insurance</dt><dd>{!p ? 'Hidden' : p.medical != null ? `${inr(p.medical)} a month (set for this employee)` : 'Structure standard'}</dd></>}
        {R.ded === 'all' ? <><dt>Meal · accommodation deduction</dt><dd>{p ? `${inr(p.meal)} · ${inr(p.accommodation)}` : 'Hidden'}</dd></>
          : R.ded === 'tds' ? <><dt>TDS</dt><dd>{sd.tdsRate != null ? `${sd.tdsRate}% of gross` : 'A share of gross'} (set in the structure)</dd></> : <><dt>Deductions</dt><dd>None</dd></>}
      </dl>
      <div className="row" style={{ marginTop: 8 }}>
        {can('emp.salary', 2) && e.status !== 'Inactive' && p && (R.variable || R.ded === 'all') && <button className="btn sm" onClick={() => setDlg('pay')}>Edit pay options</button>}
        <button className="btn sm" disabled={!R.docs} onClick={() => setDlg('slip')}>Payslip</button>
        <button className="btn sm" disabled={!R.docs} onClick={() => setDlg('appt')}>Appointment letter</button>
      </div>
      {!R.docs && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>{e.type} staff cannot download a payslip or appointment letter.</p>}
      {(can('emp.salary') || can('pay.run')) && <PfExemptBlock id={e.id} />}
      {can('emp.govid') && <EmployeeDocs id={e.id} />}
      <UniformBlock id={e.id} />
      {can('emp.assets') && <AssetBlock id={e.id} />}
      <h3 style={{ marginTop: 18 }}>Change history</h3>
      {hist.data?.length ? <div className="tw"><table><thead><tr><th>Effective</th><th>What changed</th><th>Reason</th><th>Done by</th><th>Status</th></tr></thead><tbody>
        {hist.data.map((h, i) => <tr key={i}><td>{fmtD(h.effective_from)}</td><td><b>{h.field}</b>: {h.old_value || '—'} → {h.new_value}</td><td>{h.reason || '—'}</td><td className="mute">{h.changed_by}, {fmtD(h.changed_at)}</td>
          <td><Pill k={h.state === 'Scheduled' ? 'warn' : 'ok'}>{h.state || 'Applied'}</Pill></td></tr>)}
      </tbody></table></div> : <p className="mute" style={{ margin: 0 }}>No changes recorded since the history was switched on.</p>}
      <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Joined on {fmtD(e.doj)}. Every bulk change keeps the old value, the new value, the reason and the date it takes effect.</p>
      <div className="note" style={{ marginTop: 16 }}>Employee records are never deleted. Marking inactive keeps history and payroll trail intact.</div>
      {can('emp.status', 2) && <div className="row" style={{ marginTop: 14 }}>
        {e.status === 'Inactive'
          ? <><button className="btn primary" onClick={() => setDlg('rejoin')}>Rejoin</button><button className="btn" title="Use only if this person was marked inactive by mistake" onClick={undo}>Undo inactive</button></>
          : <><select value={reason} onChange={(x) => setOff({ ...off, reason: x.target.value })} aria-label="Reason">{sepReasons.map((r) => <option key={r}>{r}</option>)}</select>
            <label className="mute" style={{ fontSize: 12.5, display: 'flex', gap: 6, alignItems: 'center' }}>Last working day <input type="date" value={off.lwd} max={today()} onChange={(x) => setOff({ ...off, lwd: x.target.value })} /></label>
            <button className="btn bad" onClick={inactivate}>Mark inactive</button></>}
      </div>}
      {err && <div className="note bad" role="alert" style={{ marginTop: 10 }}>{err}</div>}
      {dlg && <Top>{dlg === 'pay' && <PayModal e={e} std={sd} structId={(structs.data?.find((x) => x.name === structName))?.id} stdPct={sd.varPct} reasons={reasons.data?.filter((r) => r.used_for === 'Salary').map((r) => r.name) || []} onClose={() => setDlg('')} onDone={() => { setDlg(''); reload(); hist.reload(); }} />}
      {dlg === 'slip' && <SlipModal e={e} onClose={() => setDlg('')} />}
      {dlg === 'appt' && <ApptModal e={e} policy={pol?.name} prob={pol ? prob : null} onClose={() => setDlg('')} />}
      {dlg === 'rejoin' && <RejoinModal e={e} onClose={() => setDlg('')} />}</Top>}
    </>
  );
}

/** Prototype rejoinModal: the earlier service, then the rejoin starts as a shortlist in Onboarding, prefilled from
 *  this record (onboarding in stages: SI → OI → JI → Employee ID, with the code choice on the shortlist). */
function RejoinModal({ e, onClose }: { e: any; onClose: () => void }) {
  const nav = useNavigate();
  const { can } = useAuth();
  const rj = e.rejoin ?? {}, pend = !!rj.pending;
  const elig = !['Terminated', 'Absconded'].includes(e.exit_reason) && rj.rehire !== 'No';
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>Rejoin {e.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <dl className="kv" style={{ marginTop: 10 }}>
      <dt>Code</dt><dd className="mono">{e.id}</dd>
      <dt>Earlier service</dt><dd>{fmtD(e.doj)} to {e.lwd ? fmtD(e.lwd) : '—'} ({tenure(e.doj, e.lwd)}){rj.before ? `, rejoined ${rj.before} time${rj.before > 1 ? 's' : ''} before` : ''}</dd>
      <dt>Left because</dt><dd>{e.exit_reason || '—'} {!elig && <Pill k="bad">Not recommended for rehire</Pill>}</dd>
      <dt>Last role</dt><dd>{e.department} · {e.designation} · {e.grade} · {e.location}</dd>
    </dl>
    {pend && <div className="note bad">A rejoin is already on its way in Onboarding.</div>}
    {!elig && <div className="note bad" style={{ marginTop: 10 }}>This person left as “{e.exit_reason || rj.rehire}”. The reason is shown to every approver.</div>}
    <p className="ink2" style={{ fontSize: 13 }}>A rejoin starts as a shortlist, like every new hire: the hiring manager and the Function Head approve it, the hiring manager interviews, then the offer goes through offer approval (budget and pay band checks). The shortlist opens prefilled from this record, with the choice to keep {e.id} or issue a new code. Earlier salary history, bank and statutory details stay on the record.</p>
    <div className="row" style={{ marginTop: 14 }}><button className="btn primary" disabled={pend || !can('onb.raise', 2)} title={can('onb.raise', 2) ? undefined : 'Needs the right to raise a new offer'} onClick={() => nav(`/onboarding?rejoin=${encodeURIComponent(e.id)}`)}>Start the shortlist</button><button className="btn" onClick={onClose}>Cancel</button></div>
  </Modal>;
}

/** Prototype payModal: Pay options. The API keeps a reason with every pay change, so the reason is asked here too. */
function PayModal({ e, std, structId, stdPct, reasons, onClose, onDone }: { e: any; std: any; structId?: string; stdPct?: number; reasons: string[]; onClose: () => void; onDone: () => void }) {
  const p = e.salary.pay, R = rules(e.type);
  // Variable pay target (17 Nov 2026): % of the fixed gross or ₹ a month, chosen each time; from the open month.
  const tgt = e.salary.designed;
  const [v, setV] = useState({ varMode: (e.salary.varMode || 'pct') as VarMode, varVal: String(tgt ? (e.salary.varMode === 'pct' ? e.salary.varPct ?? '' : e.salary.varMode ? e.salary.varTarget ?? '' : '') : p.varPct ?? ''),
    meal: p.meal || 0, accommodation: p.accommodation || 0, reason: '' });
  const [err, setErr] = useState('');
  const vv = R.variable && v.varVal !== '' ? +v.varVal : undefined;
  const body = { type: e.type, locationId: e.location_id, grade: e.grade, gross: e.salary.gross, structureId: structId, joinMonth: +String(e.doj).slice(5, 7) || 1, month: new Date().getMonth() + 1,
    varMode: v.varMode, varPct: v.varMode === 'pct' ? vv : undefined, varAmt: v.varMode === 'amount' ? vv : undefined, varCycle: !tgt ? p.varCycle ?? undefined : undefined, meal: R.ded === 'all' ? +v.meal || 0 : 0, accom: R.ded === 'all' ? +v.accommodation || 0 : 0 };
  const prev = useLoad(() => (e.salary.gross > 0 ? api('/salary/preview', { body }).catch(() => null) : Promise.resolve(null)), [JSON.stringify(body)]);
  const save = async () => {
    setErr('');
    try {
      await api(`/employees/${e.id}/pay`, { method: 'PUT', body: { varMode: v.varMode, varVal: vv ?? null, varPct: !tgt && v.varMode === 'pct' ? vv ?? null : undefined, varCycle: !tgt ? p.varCycle ?? null : undefined,
        meal: R.ded === 'all' ? +v.meal : 0, accommodation: R.ded === 'all' ? +v.accommodation : 0, reason: v.reason } });
      toast('Pay options saved'); onDone();
    } catch (x: any) { setErr(x.message); }
  };
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>Pay options: {e.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5 }}>{e.type} · {e.city} · gross {e.salary.gross != null ? inr(e.salary.gross) : 'Hidden'} · {e.salary.structure}. The salary structure version is changed in Payroll, Structure mapping.</p>
    <div className="form" style={{ marginTop: 10 }}>
      {R.variable && <VarTargetField mode={v.varMode} value={v.varVal} gross={+e.salary.gross || 0} onChange={(m, x) => setV({ ...v, varMode: m, varVal: x })} />}
      {R.ded === 'all' && <><label className="field"><span>Meal deduction, ₹ a month</span><input type="number" min={0} value={v.meal} onChange={(x) => setV({ ...v, meal: +x.target.value })} /></label>
        <label className="field"><span>Accommodation deduction, ₹ a month</span><input type="number" min={0} value={v.accommodation} onChange={(x) => setV({ ...v, accommodation: +x.target.value })} /></label></>}
      <label className="field"><span>Reason</span><select value={v.reason} onChange={(x) => setV({ ...v, reason: x.target.value })}><option value="">Choose</option>{reasons.map((r) => <option key={r}>{r}</option>)}</select></label>
    </div>
    {prev.data?.breakup && <div style={{ marginTop: 14 }}><Statement c={prev.data.breakup} type={e.type} std={std} rules={prev.data.rules ? { [e.type]: prev.data.rules } : undefined} lwf={prev.data.lwfNote ?? ''} month={body.month} /></div>}
    {err && <div className="note bad" style={{ marginTop: 10 }}>{err}</div>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Save</button></div>
  </Modal>;
}

/** Prototype docModal for the payslip: the open month's payslip from the pay row, else the latest published one. */
function SlipModal({ e, onClose }: { e: any; onClose: () => void }) {
  // Wrapped, so "still loading" (null) and "no payslip" ({ v: null }) differ.
  const slip = useLoad(() => loadSlip(e.id).then((v) => ({ v })), [e.id]);
  const v = slip.data?.v;
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>Payslip: {e.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div style={{ margin: '12px 0' }}>
      {slip.error ? <div className="note bad">{slip.error}</div> : !slip.data ? <Loading /> : !v ? <p className="mute" style={{ margin: 0 }}>No payslip yet. A payslip appears once payroll publishes the month.</p>
        : <Payslip v={v} />}
    </div>
    <div className="row" style={{ justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Close</button><button className="btn primary" disabled={!v} onClick={printSlip}>Download</button></div>
  </Modal>;
}

/** Prototype docModal for the appointment letter, written from the record. */
function ApptModal({ e, policy, prob, onClose }: { e: any; policy?: string; prob: number | null; onClose: () => void }) {
  const text = `Appointment letter\n\nDear ${e.name},\n\nWe are pleased to confirm your appointment as ${e.designation} (${e.grade}) in the ${e.department} department at Saltstayz, ${e.location}, ${e.city}, from ${fmtD(e.doj)}.\n\n`
    + `Your monthly gross is ${e.salary?.gross != null ? inr(e.salary.gross) : 'as per your offer'}.${prob != null ? ` Probation is ${prob ? prob + ' months' : 'not applicable'}.` : ''}${policy ? ` Your work policy is ${policy}.` : ''}\n\nPeople Team, Saltstayz`;
  const save = () => {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' })); a.download = `appointment-letter-${e.id}.txt`; a.click();
    URL.revokeObjectURL(a.href); toast('Saved');
  };
  return <Modal onClose={onClose} wide>
    <div className="row sp"><h2>Appointment letter: {e.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div style={{ whiteSpace: 'pre-wrap', fontSize: 13, lineHeight: 1.6, border: '1px solid var(--line)', borderRadius: 10, padding: 14, margin: '12px 0' }}>{text}</div>
    <div className="row" style={{ justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Close</button><button className="btn primary" onClick={save}>Download</button></div>
  </Modal>;
}

/** Prototype assetBlock: assets and uniform issued to the person. */
function AssetBlock({ id }: { id: string }) {
  const { data } = useLoad(() => api(`/inventory/employee/${id}`).catch(() => null), [id]);
  if (!data) return null;
  // Uniform issued is listed with the sizes, recovery and refund in the Uniform sizes block above.
  const a = (data.assets as any[]).filter((x) => x.status === 'Issued');
  return <>
    <h3 style={{ marginTop: 18 }}>Assets</h3>
    {a.length ? <ul className="log">{a.map((x) => <li key={x.id}>{x.type} <span className="mono mute">{x.serial}</span><div className="mute" style={{ fontSize: 12 }}>Issued {fmtD(x.issued_at)}</div></li>)}</ul>
      : <p className="mute" style={{ margin: '0 0 6px' }}>No assets issued.</p>}
  </>;
}

/** Documents on the record, including those collected during onboarding (app: the prototype keeps no uploaded files).
 *  Hidden for roles that cannot see ID details. */
function EmployeeDocs({ id }: { id: string }) {
  const { data } = useLoad(() => api<any[]>(`/employees/${id}/documents`).catch(() => null), [id]);
  if (!data?.length) return null;
  return <ul className="log" style={{ marginTop: 10 }}>{data.map((d) => <li key={d.id} style={{ display: 'flex', justifyContent: 'space-between', gap: 8, alignItems: 'center' }}>
    <div><b>{d.label}</b><div className="mute" style={{ fontSize: 12 }}>{d.owner_kind === 'offer' || d.owner_kind === 'candidate' ? 'From onboarding' : d.owner_kind === 'nomination' ? 'Nomination' : 'Employee record'} · {fmtD(d.uploaded_at)}</div></div>
    <button className="btn sm" onClick={() => openDoc(d.id).catch((x) => toast(x.message))}>{d.file_name}</button></li>)}</ul>;
}

/** "July 2026" for "2026-07". */
const fmtYm = (ym?: string) => (ym ? new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 15).toLocaleString('en-IN', { month: 'long', year: 'numeric' }) : '');

/** The PAN check next to a PAN: Verified, Name mismatch, Invalid or Not checked, with the name the provider returned. */
function PanBadge({ c }: { c?: { status: string; name?: string; at?: string } | null }) {
  if (!c) return null;
  const k = c.status === 'Verified' ? 'ok' : c.status === 'Not checked' ? 'n' : 'bad';
  const title = [c.name ? `Name on PAN: ${c.name}` : '', c.at ? `Checked ${fmtD(c.at.slice(0, 10))}` : ''].filter(Boolean).join(' · ');
  return <span title={title}><Pill k={k}>{c.status}</Pill>{c.name && c.status !== 'Verified' ? <span className="mute" style={{ fontSize: 12 }}> {c.name}</span> : null}</span>;
}
