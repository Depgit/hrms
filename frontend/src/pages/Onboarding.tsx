import { useEffect, useMemo, useRef, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, fmtD, inr, openDoc, today, uploadDoc } from '../api';
import { useAuth } from '../auth';
import { Chain, Field, Loading, Modal, Pill, Switch, Tabs, toast, useLoad, usePage } from '../components/ui';
import PersonPicker from '../components/PersonPicker';
import { PAY_IN, PayEntryFields, VarTargetField, splitPay, type PayIn, type VarMode } from '../components/PayEntry';
import { Statement, VC, bandMid } from './Compensation';
import { typeNames } from './Dashboard';
import { DocWindowSetting } from './DocCollection';
import { PortalCard } from '../components/PortalCard';
import { SkipApprovalButton } from '../components/SkipApproval';
import { shiftOffered } from './ShiftForm';
import { BgvPanel, EarlyExit, HireSettings, JoiningOutcomes, OfferAtJoiningChoose, OfferAtJoiningNote, OfferVersions, Timeline, WaitingPanel } from './Hiring';

/* Onboarding in stages (owner's process, 11 Nov 2026; hiring lifecycle 16 Nov 2026): every new hire starts as a
   shortlist and keeps one number, SI-0001 → OI-0001 when the candidate accepts → JI-0001 at joining (a normal employee
   from day 1) → the Employee ID once every step is approved (onboarding_stages.go, hiring.go).
   Offers raised before stages (OFR-…) finish in their own table under the pipeline. */

type FilledBy = 'joiner' | 'hr';
type SetupField = { key: string; label: string; section: string; kind: string; options: string | null; on: boolean; sys: boolean; custom: boolean; offer: number; join: number; types: string[]; filledBy: FilledBy; threshold?: number };
type SetupDoc = { key: string; label: string; on: boolean; sys: boolean; offer: number; join: number; types: string[]; depts: string[]; filledBy: FilledBy };
type Data = Record<string, string>;
type Exp = { company: string; designation: string; from: string; to: string };
type Pipe = { rows: any[]; counts: Record<string, number>; stages: string[] };

const SECTIONS = ['Personal', 'Identity and statutory', 'Bank', 'Job', 'Location and reporting', 'Benefits', 'Uniform', 'Hiring', 'Compensation'];
const TYPES = ['Permanent', 'Retainer', 'Contractual'];
const STATES: [number, string][] = [[0, 'Not needed'], [1, 'Optional'], [2, 'Mandatory']];
const STAGES = ['Shortlisted', 'Interview', 'Offer', 'Offer at joining', 'Waiting', 'Joined', 'With Payroll', 'Done', 'Closed'];
const STAGE_LABEL: Record<string, string> = { Joined: 'Joined, approvals pending' };
const GENDERS = ['Male', 'Female', 'Other', 'Prefer not to say'];
/** Fields asked only on other answers (as the server's onbField.asked): UAN number when an existing UAN is declared,
 *  spouse name when married, the PF choice when there is no UAN and the gross is above the threshold (₹25,000), or when
 *  the offer said No to PF (owner, 26 Nov 2026). */
const asked = (f: { key: string; threshold?: number }, d: Data) =>
  f.key === 'uan' ? d.uanAvail === 'Yes'
    : f.key === 'spouse' ? d.marital === 'Married'
      : f.key === 'pfChoice' ? (d.pfWant === 'No' && d.uanAvail !== 'Yes') || (d.uanAvail === 'No' && Number(d.gross) > (f.threshold || 25000))
        : true;
const PF_CHOICE_HINT = 'Yes: 12% of Basic is deducted for PF and the company adds its share; the salary is split into Basic, HRA and special allowance. No: no PF from you or the company, and the whole fixed salary is paid as Basic.';
const fieldApplies = (f: SetupField, d: Data) => f.on && f.types.includes(d.type || 'Permanent') && asked(f, d);
const docApplies = (x: SetupDoc, type: string, dept: string) => x.on && x.types.includes(type) && (!x.depts.length || x.depts.includes(dept));
const strMap = (m: Record<string, unknown> | null | undefined): Data => Object.fromEntries(Object.entries(m || {}).filter(([, v]) => v != null && typeof v !== 'object').map(([k, v]) => [k, String(v)]));
const blankExp = (): Exp => ({ company: '', designation: '', from: '', to: '' });
const stageTone = (b: string): 'ok' | 'warn' | 'bad' | 'info' | 'n' => (b === 'Done' ? 'ok' : b === 'Closed' ? 'bad' : b === 'Joined' || b === 'With Payroll' ? 'info' : 'warn');
const ym = (s?: string) => (s ? new Date(+s.slice(0, 4), +s.slice(5, 7) - 1, 15).toLocaleString('en-IN', { month: 'short', year: 'numeric' }) : '');
const Req = () => <> <b style={{ color: 'var(--bad)' }}>*</b></>;
/** Marks what the new joiner fills from their link (Form setup › Filled by). */
const JoinerMark = ({ filled }: { filled?: boolean }) => filled
  ? <span className="pill info jpmark" title="Entered by the joiner from their link. HR can still correct it.">From joiner</span>
  : <span className="pill n jpmark" title="The joiner fills this from their link (Form setup › Filled by).">Joiner fills</span>;
// Fields HR always fills (store.JoinerAllowed): system fields, Compensation, and fields that pick from the company's lists.
const HR_ONLY = ['grade', 'l1', 'l2', 'shift', 'attMode', 'structId', 'type', 'dept', 'desig', 'city', 'loc', 'endDate', 'agency'];
const joinerAllowed = (f: SetupField) => !f.sys && f.kind !== 'flow' && f.kind !== 'policy' && f.section !== 'Compensation' && !HR_ONLY.includes(f.key) && (f.kind !== 'sel' || !!(f.options ?? '').trim() || f.key === 'qual');

type T = 'new' | 'pipe' | 'setup';

export default function Onboarding() {
  const { can } = useAuth();
  const [qp, setQp] = useSearchParams();
  const rejoin = qp.get('rejoin') || '';
  const linked = qp.get('id') || '';
  const [tab, setTab] = useState<T>(rejoin || (!linked && can('onb.raise', 2)) ? 'new' : 'pipe');
  const [offerFor, setOfferFor] = useState<any>(null);
  const [open, setOpen] = useState(linked);
  const pipe = useLoad(() => api<Pipe>('/candidates'), [tab, offerFor, open]);
  const legacy = useLoad(() => api<any[]>('/offers?legacy=1'), [tab]);
  // The prototype's vOnb tabs; "New joining" is now Shortlist, the first stage of every new hire.
  const tabs: [T, string][] = [['new', 'Shortlist'], ['pipe', `Pipeline (${pipe.data?.rows.length ?? 0})`], ...(can('onb.setup', 2) ? [['setup', 'Form setup']] as [T, string][] : [])];
  const go = (t: T) => { setTab(t); setOfferFor(null); if (rejoin || linked) setQp({}); };
  return (
    <>
      <Tabs tabs={tabs} value={offerFor ? 'pipe' : tab} onChange={go} />
      {offerFor
        ? <OfferForm cand={offerFor} onBack={() => { setOpen(offerFor.id); setOfferFor(null); }} onSent={() => { setOpen(offerFor.id); setOfferFor(null); setTab('pipe'); }} />
        : <>
          {tab === 'new' && <ShortlistForm key={rejoin} rejoin={rejoin} onSent={(id) => { setQp({}); setTab('pipe'); setOpen(id); }} />}
          {tab === 'pipe' && <Pipeline data={pipe.data} error={pipe.error} legacy={legacy.data} reloadLegacy={legacy.reload} onOpen={setOpen} />}
          {tab === 'setup' && <FormSetup />}
        </>}
      {open && !offerFor && <CandidateModal id={open} onClose={() => { setOpen(''); if (linked) setQp({}); pipe.reload(); }} onOffer={(c) => { setOpen(''); setOfferFor(c); }} />}
    </>
  );
}

// ---------- Shared lookups for the forms ----------

function useLookups(d: Data) {
  const types = useLoad(() => api<any[]>('/masters/employment_type?status=Active'), []);
  const depts = useLoad(() => api<any[]>('/masters/department?status=Active'), []);
  const desigs = useLoad(() => api<any[]>('/masters/designation?status=Active'), []);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const shifts = useLoad(() => api<any[]>('/masters/shift?status=Active'), []);
  const modes = useLoad(() => api<any[]>('/masters/attendance_mode?status=Active'), []);
  const quals = useLoad(() => api<any[]>('/masters/qualification?status=Active'), []);
  const structs = useLoad(() => api<any[]>('/salary/structures').catch(() => []), []);
  const desig = desigs.data?.find((x) => x.name === d.desig && x.department === d.dept);
  const opts = (key: string, f?: SetupField): [string, string][] => {
    switch (key) {
      case 'type': return typeNames(types.data).map((x: string) => [x, x]);
      case 'dept': return (depts.data ?? []).map((x) => [x.name, x.name]);
      case 'desig': return (desigs.data ?? []).filter((x) => x.department === d.dept).map((x) => [x.name, x.name]);
      case 'grade': return (desig?.grades ?? []).map((g: string) => [g, g]);
      case 'city': return (cities.data ?? []).map((x) => [x.id, x.name]);
      case 'loc': return (locs.data ?? []).filter((x) => x.city_id === d.city).map((x) => [x.id, x.name]);
      case 'shift': { // the shifts mapped to the department and location (shift_maps.go)
        const deptId = (depts.data ?? []).find((x) => x.name === d.dept)?.id ?? '';
        return (shifts.data ?? []).filter((s) => shiftOffered(s, deptId, d.loc ?? '')).map((s) => [s.code, `${s.name} (${s.start_time}–${s.end_time}${s.start2_time ? ` + ${s.start2_time}–${s.end2_time}` : ''})`]);
      }
      case 'attMode': return (modes.data ?? []).map((x) => [x.name, x.name]);
      case 'qual': return (quals.data ?? []).map((x) => [x.name, x.name]);
      case 'structId': return (structs.data ?? []).filter((x) => x.status === 'Active').map((x) => [x.id, x.name + (x.is_default ? ' (default)' : '')]);
      case 'varCycle': return VC;
      default: return (f?.options ?? '').split(',').map((x) => x.trim()).filter(Boolean).map((x) => [x, x]);
    }
  };
  return { opts, cities: cities.data ?? [], depts: depts.data ?? [], structs: structs.data ?? [] };
}

/** The work policy the rules give the candidate (POST /offers/check `policy`, prototype `obPol`). */
type Policy = { name: string; via: string; weekOff?: string; probation?: number; pl?: string };

function Control({ f, d, set, opts, corporate, policy }: { f: SetupField; d: Data; set: (k: string, v: string) => void; opts: (k: string, f: SetupField) => [string, string][]; corporate?: boolean; policy?: Policy }) {
  const v = d[f.key] ?? '';
  if (f.kind === 'flow') return d.dept ? <div className="ro"><b>{corporate ? 'Corporate flow' : 'Operations flow'}</b><div className="mute" style={{ fontSize: 12 }}>Set by the department (corporate department: {corporate ? 'Yes' : 'No'}). Change it in Masters, not here.</div></div> : <div className="ro mute">Select a department first</div>;
  if (f.kind === 'policy') return policy
    ? <div className="ro"><b>{policy.name}</b><div className="mute" style={{ fontSize: 12 }}>Set by work policy rules ({policy.via}). Change the rules under Work policies, not here.</div></div>
    : <div className="ro"><b>Picked by work policy rules</b><div className="mute" style={{ fontSize: 12 }}>From the location, department and grade. Set under Work policies.</div></div>;
  // Reporting managers: any active employee, found by typing.
  if (f.key === 'l1' || f.key === 'l2') return <PersonPicker label={f.label} value={v} onChange={(id) => set(f.key, id)} search="&status=Active" placeholder="Select" />;
  if (f.kind === 'sel') {
    const list = opts(f.key, f);
    // Prototype obCtl: only the grade waits for the designation, and only the payout cycle has a "Standard" blank.
    const blank = f.key === 'grade' && !d.desig ? 'Select designation first' : f.key === 'varCycle' ? 'Standard' : 'Select';
    return <select value={v} onChange={(e) => set(f.key, e.target.value)}><option value="">{blank}</option>{list.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>;
  }
  return <input type={f.kind === 'number' ? 'number' : f.kind === 'tel' ? 'tel' : f.kind} value={v} onChange={(e) => set(f.key, e.target.value)} />;
}

/** The one number through the stages: SI → OI (at acceptance) → JI (at joining) → Employee ID (approved), the reached
 *  ones in green, the current one dark. */
function Trail({ x }: { x: any }) {
  const reached = (x.trail as any[]) ?? [];
  const steps = [
    { id: x.ids.si, label: 'Shortlisted', wait: 'Shortlisted' }, { id: x.ids.oi || 'Offer ID', label: 'Offer accepted', wait: 'at acceptance' },
    { id: x.ids.ji, label: 'Joined', wait: 'at joining' },
    { id: 'Employee ID', label: 'Employee ID', wait: x.newFlow || !x.joined_on ? 'when approved' : x.employeeIdAt === 'joining' ? 'at joining' : 'after documents' },
  ];
  const closed = x.stage === 'Closed';
  const last = reached.length ? reached[reached.length - 1].label : '';
  return <div className="chain trail">{steps.map((s, i) => {
    const r = reached.find((t) => t.label === s.label), cur = !closed && r && r.label === last && x.bucket !== 'Done';
    return <span key={s.label + i} style={{ display: 'contents' }}>{i > 0 && <span className="arrow">→</span>}
      <span className={`step${r ? (cur ? ' cur' : ' done') : ''}${closed && r && r.label === last ? ' rej' : ''}`} title={r?.at ? fmtD(r.at) : undefined}>
        <span className="mono">{r?.id ?? s.id}</span><span style={{ fontWeight: 600, opacity: 0.8 }}>{r ? `${s.label}${r.at ? ' ' + fmtD(String(r.at).slice(0, 10)) : ''}` : s.wait}</span></span></span>;
  })}</div>;
}

// ---------- Stage 1: Shortlist ----------

function ShortlistForm({ rejoin, onSent }: { rejoin: string; onSent: (id: string) => void }) {
  const { can } = useAuth();
  const canCreate = can('onb.raise', 2);
  const [d, setD] = useState<Data>({});
  const [exp, setExp] = useState<Exp[]>([blankExp()]);
  const [resume, setResume] = useState<File | null>(null);
  const [res, setRes] = useState<any>(null);
  const [show, setShow] = useState(false);
  const [busy, setBusy] = useState(false);
  const pre = useLoad(() => (rejoin ? api(`/candidates/rejoin/${rejoin}`) : Promise.resolve(null)), [rejoin]);
  useEffect(() => {
    if (!pre.data) return;
    setD((x) => ({ ...x, ...strMap(pre.data.data) }));
    if (pre.data.experience?.length) setExp(pre.data.experience);
  }, [pre.data]);
  const L = useLookups(d);
  const mgrs = useLoad(() => api<any[]>('/candidates/managers').catch(() => []), []);
  const set = (k: string, v: string) => setD((x) => {
    const n = { ...x, [k]: v };
    if (k === 'dept') n.desig = '';
    if (k === 'city') n.loc = '';
    return n;
  });
  const setRow = (i: number, k: keyof Exp, v: string) => setExp((l) => l.map((r, j) => (j === i ? { ...r, [k]: v } : r)));
  const body = useMemo(() => JSON.stringify({ data: d, experience: exp }), [d, exp]);
  const check = () => api('/candidates/check', { body: JSON.parse(body) }).then((r) => { setRes(r); return r; });
  useEffect(() => {
    if (!canCreate) return;
    const t = setTimeout(() => check().catch(() => {}), 300);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [body]);
  const errs: string[] = res?.errors ?? [];
  const miss = (...words: string[]) => show && errs.some((e) => words.some((w) => e.startsWith(w)));
  const send = async (skip = '') => {
    setShow(true);
    setBusy(true);
    try {
      const now = await check();
      if (now.errors?.length) { toast(`${now.errors.length} thing${now.errors.length > 1 ? 's' : ''} to fix`); window.scrollTo(0, 0); return; }
      const r = await api('/candidates' + skip, { body: { data: d, experience: exp } });
      if (resume) {
        try { await uploadDoc('candidate', r.id, 'resume', resume); } catch (x: any) { toast(`${r.id} sent for approval. The résumé did not upload: ${x.message}`); onSent(r.id); return; }
      }
      toast(`${r.id} sent for approval`); onSent(r.id);
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const fld = (k: string, label: string, el: JSX.Element, req = true, words: string[] = [label]) =>
    <label className={`field ${miss(...words) ? 'err' : ''}`}><span>{label}{req && <Req />}</span>{el}</label>;
  const inp = (k: string, type = 'text', extra: Record<string, unknown> = {}) => <input type={type} value={d[k] ?? ''} onChange={(e) => set(k, e.target.value)} {...extra} />;
  const sel = (k: string, list: [string, string][], blank = 'Select') => <select value={d[k] ?? ''} onChange={(e) => set(k, e.target.value)}><option value="">{blank}</option>{list.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select>;
  const plan = res?.plan;
  return (
    <div className="grid split">
      <div className="grid">
        {pre.data && <div className="note"><b>Rejoin of {rejoin}.</b> Earlier exit: {pre.data.exitReason || '—'}{pre.data.lwd ? `, last working day ${fmtD(pre.data.lwd)}` : ''}. Rehire eligibility: {pre.data.rehire}.{pre.data.open ? ` ${pre.data.open} is already open.` : ''}</div>}
        {pre.error && <div className="note bad">{pre.error}</div>}
        <div className="card"><h3>Personal details</h3><div className="form">
          {fld('first', 'First name', inp('first'))}
          {fld('last', 'Last name', inp('last'))}
          {fld('phone', 'Mobile number', inp('phone', 'tel', { inputMode: 'numeric', maxLength: 10 }))}
          {fld('email', 'Personal email ID', inp('email', 'email'), true, ['Personal email'])}
          {fld('dob', 'Date of birth', inp('dob', 'date', { max: today() }), true, ['Date of birth', 'The candidate must'])}
          {fld('gender', 'Gender', sel('gender', GENDERS.map((g) => [g, g])))}
          <label className="field" style={{ gridColumn: '1 / -1' }}><span>Current address</span><input type="text" value={d.addr ?? ''} onChange={(e) => set('addr', e.target.value)} /></label>
        </div></div>
        <div className="card"><h3>Work experience</h3>
          <p className="mute" style={{ marginTop: -6, fontSize: 12 }}>Latest first. Leave the To month empty for the current job, and the rows empty for a fresher.</p>
          <div className="exprow head" aria-hidden="true"><span>Company</span><span>Designation</span><span>From</span><span>To</span><span /></div>
          {exp.map((r, i) => <div key={i} className="exprow">
            <input type="text" aria-label={`Company ${i + 1}`} placeholder="Company" value={r.company} onChange={(e) => setRow(i, 'company', e.target.value)} />
            <input type="text" aria-label={`Designation ${i + 1}`} placeholder="Designation" value={r.designation} onChange={(e) => setRow(i, 'designation', e.target.value)} />
            <input type="month" aria-label={`From ${i + 1}`} title="From" value={r.from} max={today().slice(0, 7)} onChange={(e) => setRow(i, 'from', e.target.value)} />
            <input type="month" aria-label={`To ${i + 1}`} title="To (empty for the current job)" value={r.to} max={today().slice(0, 7)} onChange={(e) => setRow(i, 'to', e.target.value)} />
            <button className="btn sm" title="Remove this row" aria-label={`Remove row ${i + 1}`} onClick={() => setExp((l) => (l.length > 1 ? l.filter((_, j) => j !== i) : [blankExp()]))}>✕</button>
          </div>)}
          <div className="row" style={{ marginTop: 10 }}><button className="btn sm" disabled={exp.length >= 15} onClick={() => setExp((l) => [...l, blankExp()])}>Add a row</button></div>
        </div>
        <div className="card"><h3>Existing CTC and location</h3><div className="form">
          {fld('ctc', 'Existing CTC (₹ a year)', inp('ctc', 'number', { min: 0, placeholder: '0 for a fresher' }), true, ['Existing CTC'])}
          {fld('city', 'Work city', sel('city', L.opts('city')))}
          {fld('loc', 'Reporting location', sel('loc', L.opts('loc'), d.city ? 'Select' : 'Select city first'), true, ['Reporting location', 'Location'])}
        </div></div>
        <div className="card"><h3>Role and hiring manager</h3><div className="form">
          {fld('dept', 'Department', sel('dept', L.opts('dept')), true, ['Department'])}
          {fld('desig', 'Designation', sel('desig', L.opts('desig'), d.dept ? 'Select' : 'Select department first'), true, ['Designation'])}
          {fld('manager', 'Hiring manager', <PersonPicker label="Hiring manager" people={mgrs.data} value={d.manager ?? ''} onChange={(id) => set('manager', id)} placeholder="Select" />, true, ['Hiring manager', 'The hiring manager', 'Choose a hiring manager'])}
          {d.rejoinOf && <label className="field"><span>Employee code on rejoining</span><select value={d.codeMode || 'same'} onChange={(e) => set('codeMode', e.target.value)}>
            <option value="same">Keep {d.rejoinOf} with a new service period</option><option value="new">A new code linked to {d.rejoinOf}</option></select></label>}
        </div>
          <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>The hiring manager is the reporting manager for the role. They approve the shortlist first and then take the interview.</p>
        </div>
        <div className="card"><h3>Résumé <span className="mute" style={{ fontWeight: 600 }}>(optional)</span></h3>
          <div className="upl"><label className="btn sm">{resume ? 'Replace' : 'Choose file'}<input type="file" accept=".pdf,.jpg,.jpeg,.png" hidden onChange={(e) => { setResume(e.target.files?.[0] ?? null); e.target.value = ''; }} /></label>
            {resume ? <><span className="mute" style={{ fontSize: 12 }}>{resume.name}</span><button className="btn sm" onClick={() => setResume(null)}>Remove</button></> : <span className="mute" style={{ fontSize: 12 }}>PDF, JPG or PNG, up to 5 MB. Uploaded with the shortlist.</span>}</div>
        </div>
      </div>
      <div className="card" style={{ position: 'sticky', top: 12 }}>
        <h3>Shortlisting ID</h3>
        <div className="bigid">{res?.next ?? 'SI-…'}</div>
        <p className="mute" style={{ fontSize: 12, margin: '4px 0 0' }}>Given when you send. The number stays with the candidate: SI at shortlist, OI when they accept the offer, JI at joining (a normal employee from day 1). The Employee ID comes once every step is approved, so nobody who does not join uses one.</p>
        <h3 style={{ marginTop: 16 }}>Approval route</h3>
        {plan ? <><div className="chain">{(plan.chain as string[]).map((c, i) => {
          const p = plan.people?.find((x: any) => x.stage === c);
          return <span key={i} style={{ display: 'contents' }}>{i > 0 && <span className="arrow">→</span>}<span className="step" title={p?.state !== 'Ready' ? p?.state : undefined}>{c}{p?.name && p.name !== 'Unassigned' ? `: ${p.name}` : ''}</span></span>;
        })}</div>
          {plan.people?.some((p: any) => p.state !== 'Ready') && <p className="mute" style={{ fontSize: 12 }}>A stage with nobody named moves on, or goes to the next level, when the shortlist is sent.</p>}
          <p className="mute" style={{ fontSize: 12 }}>Then the interview with the hiring manager: Selected or Not selected.</p></>
          : <p className="mute" style={{ fontSize: 12.5, margin: 0 }}>Choose the location, department and hiring manager to see who approves. The route is set in Approvals › Approval flow (Shortlist approval).</p>}
        {show && errs.length > 0 && <div className="note bad" style={{ marginTop: 12 }}><b>Fix before sending</b><ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{errs.map((x) => <li key={x}>{x}</li>)}</ul></div>}
        <div className="row" style={{ marginTop: 16 }}><button className="btn primary" disabled={!canCreate || busy} onClick={() => send()}>Send for approval</button><SkipApprovalButton wf="shortlist" disabled={!canCreate || busy} onSkip={(q) => send(q)} /></div>
      </div>
    </div>
  );
}

// ---------- Pipeline ----------

function Pipeline({ data, error, legacy, reloadLegacy, onOpen }: { data: Pipe | null; error: string; legacy: any[] | null; reloadLegacy: () => void; onOpen: (id: string) => void }) {
  const [f, setF] = useState('');
  const rows = data ? data.rows.filter((r) => !f || r.bucket === f) : null;
  const page = usePage(rows, 100);
  if (!data) return <Loading error={error} />;
  return <>
    <div className="seg" style={{ marginBottom: 14 }}>
      <button className={!f ? 'on' : ''} onClick={() => setF('')}>All<b>{data.rows.length}</b></button>
      {STAGES.map((s) => <button key={s} className={f === s ? 'on' : ''} onClick={() => setF(s)}>{STAGE_LABEL[s] ?? s}<b>{data.counts[s] ?? 0}</b></button>)}
    </div>
    <div className="card">
      {rows!.length ? <div className="tw"><table>
        <thead><tr><th>ID</th><th>Candidate</th><th className="hide-m">Department</th><th className="hide-m">Location</th><th className="hide-m">Stage</th><th className="hide-m">Status</th><th className="hide-m">Waiting on</th><th /></tr></thead>
        <tbody>{page.rows!.map((r) => <tr key={r.id} className="click" onClick={() => onOpen(r.id)}>
          <td className="mono" style={{ whiteSpace: 'nowrap' }}><b>{r.current_id}</b>{r.current_id !== r.id && <div className="mute">{r.id}</div>}</td>
          <td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.designation}</div>{r.rejoin_of && <Pill k="info">Rejoin</Pill>}
            {/* Narrow screens: the stage and who it waits on under the name, in place of the columns. */}
            <div className="only-m" style={{ fontSize: 12, marginTop: 4 }}><Pill k={stageTone(r.bucket)}>{STAGE_LABEL[r.bucket] ?? r.bucket}</Pill> {r.department} · {r.location}<div className="ink2">{r.status}{r.waiting ? ` · ${r.waiting}` : ''}{r.joinerLink ? ` · ${r.joinerLink}` : ''}</div></div></td>
          <td className="hide-m">{r.department}</td>
          <td className="hide-m">{r.location}<div className="mute" style={{ fontSize: 12 }}>{r.city}</div></td>
          <td className="hide-m"><Pill k={stageTone(r.bucket)}>{STAGE_LABEL[r.bucket] ?? r.bucket}</Pill></td>
          <td className="hide-m" style={{ fontSize: 12.5, minWidth: 180 }}>{r.status}{r.joinerLink && <div style={{ marginTop: 3 }}><Pill k={r.joinerLink === 'Joiner submitted' ? 'ok' : r.joinerLink === 'Link expired' || r.joinerLink === 'Link locked' ? 'warn' : 'info'}>{r.joinerLink}</Pill></div>}</td>
          <td className="hide-m" style={{ fontSize: 12.5 }}>{r.waiting || <span className="mute">—</span>}</td>
          <td><button className="btn sm" onClick={(e) => { e.stopPropagation(); onOpen(r.id); }}>Open</button></td>
        </tr>)}</tbody>
      </table></div>
        : <p className="mute" style={{ margin: 0 }}>{f ? 'No candidates at this stage.' : 'No candidates yet. Every new hire starts with a shortlist.'}</p>}
      {page.pager}
    </div>
    {legacy && legacy.length > 0 && <div style={{ marginTop: 18 }}>
      <h3 style={{ margin: '0 0 4px', fontSize: 14, fontWeight: 800 }}>Offers raised before stages</h3>
      <p className="mute" style={{ margin: '0 0 10px', fontSize: 12.5 }}>These OFR offers finish the way they started: the Employee ID was or is created at offer approval.</p>
      <LegacyPipeline rows={legacy} reload={reloadLegacy} />
    </div>}
  </>;
}

// ---------- The candidate ----------

function CandidateModal({ id, onClose, onOffer }: { id: string; onClose: () => void; onOffer: (c: any) => void }) {
  const { can } = useAuth();
  const { data: x, error, reload } = useLoad(() => api(`/candidates/${id}`), [id]);
  const [view, setView] = useState<'' | 'offer' | 'join'>('');
  const [why, setWhy] = useState('');
  const [closing, setClosing] = useState<'' | 'not-joined' | 'close'>('');
  const [busy, setBusy] = useState(false);
  if (!x) return <Modal wide onClose={onClose}>{error ? <><div className="note bad">{error}</div><div className="row" style={{ marginTop: 12 }}><button className="btn" onClick={onClose}>Close</button></div></> : <Loading />}</Modal>;
  if (view === 'offer') return <OfferModal id={x.offer_id} onClose={() => { setView(''); reload(); }} />;
  if (view === 'join') return <JoiningModal id={x.offer_id} onClose={() => { setView(''); reload(); }} />;
  const act = async (fn: () => Promise<any>, ok: (r: any) => string): Promise<void> => {
    setBusy(true);
    try { const r = await fn(); toast(ok(r)); setClosing(''); setWhy(''); reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const d = x.data || {}, c = x.can || {}, docs = x.docs || {}, exp: Exp[] = x.experience || [];
  const edit = can('onb.raise', 2) || can('onb.pipeline', 2);
  const resume = docs.resume;
  const upload = async (file?: File) => {
    if (!file) return;
    try { await uploadDoc('candidate', x.id, 'resume', file); toast('Attached ' + file.name); reload(); } catch (e: any) { toast(e.message); }
  };
  const hist = (r: any) => (r?.history ?? []).filter((h: any) => h.action !== 'Skipped' || h.note).map((h: any, i: number) =>
    <li key={i}><b>{h.stage ? `${h.stage}: ` : ''}{h.action}</b> <span className="mute">by {h.by_name} · {fmtD(String(h.at))}</span>{h.note && <div className="ink2" style={{ fontSize: 12.5 }}>“{h.note}”</div>}</li>);
  const offerOpen = !!x.offer_id && x.offer_status !== 'Rejected' && x.offer_status !== 'Cancelled';
  // Offer at joining (offer_at_joining.go): chosen instead of raising; the offer is raised on the joining day.
  const oaj = x.offerAtJoining;
  const joined = x.stage === 'Joined';
  const jn = x.joining;
  const verified = !!x.verified_at;
  return <Modal wide onClose={onClose}>
    <div className="row sp" style={{ alignItems: 'flex-start' }}>
      <div><div className="bigid">{x.current_id}</div><h2 style={{ marginTop: 2 }}>{x.name}</h2>
        <div className="mute" style={{ fontSize: 13 }}>{x.designation} · {x.department} · {x.location}, {x.city}{x.rejoin_of ? ` · rejoin of ${x.rejoin_of}` : ''}</div></div>
      <div className="row" style={{ gap: 6 }}><Pill k={stageTone(x.bucket)}>{STAGE_LABEL[x.bucket] ?? x.bucket}</Pill><button className="btn sm" onClick={onClose}>Close</button></div>
    </div>
    <div style={{ margin: '14px 0 10px' }}><Trail x={x} /></div>
    {x.accommodation && <div className="note" style={{ marginBottom: 10 }}>Company accommodation: <b>Yes</b>, in {x.accommodation.cluster}. {x.accommodation.status}.</div>}
    <div className={`note ${x.stage === 'Closed' ? 'bad' : verified ? 'ok' : ''}`}>{x.status}{x.waiting ? <> · waiting on <b>{x.waiting}</b></> : null}</div>

    <div className="grid g2" style={{ marginTop: 14 }}>
      <div><h3 className="sec">Shortlist</h3>
        <dl className="kv">
          <dt>Mobile</dt><dd>{d.phone || '—'}</dd><dt>Email</dt><dd>{d.email || '—'}</dd>
          <dt>Date of birth</dt><dd>{fmtD(d.dob)}</dd><dt>Gender</dt><dd>{d.gender || '—'}</dd>
          <dt>Address</dt><dd>{d.addr || '—'}</dd><dt>Existing CTC</dt><dd>{typeof x.existing_ctc === 'number' ? `${inr(x.existing_ctc)} a year` : '—'}</dd>
          <dt>Hiring manager</dt><dd>{x.manager_name} · {x.manager_id}</dd><dt>Raised by</dt><dd>{x.raised_by_name} · {fmtD(String(x.created_at))}</dd>
          <dt>Résumé</dt><dd><div className="upl">{resume ? <a href="#" onClick={(e) => { e.preventDefault(); openDoc(resume.id).catch((er) => toast(er.message)); }}>{resume.file_name}</a> : <span className="mute">None</span>}
            {c.upload && <label className="btn sm">{resume ? 'Replace' : 'Upload'}<input type="file" accept=".pdf,.jpg,.jpeg,.png" hidden onChange={(e) => { upload(e.target.files?.[0]); e.target.value = ''; }} /></label>}</div></dd>
        </dl>
      </div>
      <div><h3 className="sec">Work experience</h3>
        {exp.length ? <div className="tw"><table><thead><tr><th>Company</th><th>Designation</th><th>Period</th></tr></thead>
          <tbody>{exp.map((r, i) => <tr key={i}><td>{r.company}</td><td>{r.designation}</td><td style={{ whiteSpace: 'nowrap' }}>{ym(r.from)} to {r.to ? ym(r.to) : 'now'}</td></tr>)}</tbody></table></div>
          : <p className="mute" style={{ margin: 0 }}>Fresher, no work experience.</p>}
      </div>
    </div>

    <h3 className="sec">Shortlist approval</h3>
    {x.shortlist ? <><Chain steps={x.shortlist.people.map((p: any) => `${p.stage}: ${p.person}`)} step={x.shortlist.step} status={x.shortlist.status} />
      <ul className="log" style={{ marginTop: 6 }}>{hist(x.shortlist)}</ul></> : <p className="mute">—</p>}

    <h3 className="sec">Interview</h3>
    {x.interview_result ? <dl className="kv"><dt>Result</dt><dd><Pill k={x.interview_result === 'Selected' ? 'ok' : 'bad'}>{x.interview_result}</Pill></dd>
      <dt>Date</dt><dd>{fmtD(x.interview_date)}</dd><dt>By</dt><dd>{x.interviewed_by_name}</dd><dt>Feedback</dt><dd>{x.interview_feedback}</dd></dl>
      : x.stage === 'Interview' ? <p className="ink2" style={{ margin: 0, fontSize: 13 }}>Waiting on {x.waiting || 'the hiring manager'}. They record Selected or Not selected in their Approvals.</p>
        : <p className="mute" style={{ margin: 0, fontSize: 13 }}>After the shortlist is approved.</p>}

    <h3 className="sec">Offer {x.ids.oi ? <span className="mono mute">{x.ids.oi}</span> : x.offer_id ? <span className="mono mute">{x.id} offer v{x.offer_version || 1}</span> : null}</h3>
    {x.offer_id ? <div className="row" style={{ gap: 8 }}>
      <Pill k={x.offer_status === 'Approved' ? 'ok' : x.offer_status === 'Pending' ? 'warn' : 'bad'}>{x.declined_at ? 'Declined' : x.lapsed_at ? 'Lapsed' : x.offer_status === 'Approved' ? (x.accepted_at ? 'Accepted' : x.query_at ? 'Question asked' : x.released_at ? `Sent ${fmtD(String(x.released_at))}` : 'Approved') : x.offer_status === 'Pending' ? 'Pending approval' : `Offer ${String(x.offer_status).toLowerCase()}`}</Pill>
      <span className="ink2" style={{ fontSize: 13 }}>{x.type} · {x.grade} · gross {inr(x.gross)} a month · joining {fmtD(x.doj)}</span>
      {x.offerRequest && x.offer_status === 'Pending' && <Chain steps={x.offerRequest.chain} step={x.offerRequest.step} status={x.offerRequest.status} />}
    </div> : !oaj && <p className="mute" style={{ margin: 0, fontSize: 13 }}>{x.stage === 'Offer' ? 'Selected. Raise the offer: the form starts from the shortlist. The Offer ID is issued when the candidate accepts.' : 'After the interview.'}</p>}
    {oaj && <OfferAtJoiningNote x={x} />}
    {x.offer_id && <OfferVersions x={x} act={act} busy={busy} />}
    <div className="row" style={{ marginTop: 10 }}>
      {oaj && c.raiseOffer && <button className="btn primary" disabled={!oaj.due} title={oaj.due ? undefined : `On the joining day, ${fmtD(oaj.doj)}`} onClick={() => onOffer({ ...x, atJoining: true })}>{x.offer_id ? `Revise at joining: v${(x.offer_version || 1) + 1}` : 'Raise offer at joining'}</button>}
      {c.raiseOffer && <button className={`btn${oaj ? '' : ' primary'}`} onClick={() => onOffer(x)}>{x.offer_id ? `Revise: send v${(x.offer_version || 1) + 1}` : 'Raise offer v1'}{oaj ? ' instead' : ''}</button>}
      {c.offerAtJoining && <OfferAtJoiningChoose x={x} act={act} busy={busy} />}
      {offerOpen && <button className="btn" onClick={() => setView('offer')}>{x.offer_status === 'Approved' && !x.released_at && edit && !x.at_joining ? 'Release offer' : 'Offer letter'}</button>}
      {c.close && <button className="btn bad" onClick={() => setClosing('close')}>Close candidate</button>}
    </div>

    <h3 className="sec">Joining {joined || verified ? <span className="mono mute">{x.ids.ji}</span> : null}</h3>
    <JoiningOutcomes x={x} act={act} busy={busy} onDetails={() => setView('join')} />
    <WaitingPanel x={x} act={act} busy={busy} />
    {(joined || verified) ? <>
      <dl className="kv"><dt>Joined on</dt><dd>{fmtD(x.joined_on)}</dd>
        <dt>Joining details</dt><dd>{jn ? <Pill k={jn.done >= jn.total ? 'ok' : 'warn'}>{jn.done} of {jn.total} mandatory</Pill> : '—'}</dd>
        {x.newFlow ? <><dt>Record</dt><dd><b className="mono">{x.employee_id}</b> {x.approved_at ? <Pill k="ok">Approved</Pill> : <span className="mute" style={{ fontSize: 12 }}>· a normal employee from {fmtD(x.joined_on)}: punches, attendance and payroll. The Employee ID is issued once the joining details, Payroll verification{x.hiring?.bgvBlocks ? ' and a clear background check' : ''} are approved.</span>}</dd></>
          : <><dt>Employee ID</dt><dd>{x.employee_id ? <b className="mono">{x.employee_id}</b> : x.employeeIdAt === 'joining' ? 'Created at joining' : 'Created when the mandatory joining details and documents are complete'}</dd></>}
        {x.verify && <><dt>Payroll</dt><dd>{verified ? <Pill k="ok">Verified by Payroll</Pill> : x.verify_status === 'Rejected' ? <Pill k="bad">Returned by Payroll</Pill> : <Chain steps={x.verify.chain} step={x.verify.step} status={x.verify.status} />}</dd></>}
      </dl>
      <div className="row" style={{ marginTop: 10 }}>
        <button className="btn" onClick={() => setView('join')}>{edit && !verified && x.verify_status !== 'Pending' ? 'Joining details and documents' : 'View joining details'}</button>
        {!x.newFlow && edit && !x.employee_id && jn && (x.employeeIdAt === 'joining' || jn.done >= jn.total) && <button className="btn" disabled={busy} onClick={() => act(() => api(`/candidates/${x.id}/employee-id`, { method: 'POST' }), (r) => `Employee ID ${r.employeeId} created`)}>Create Employee ID</button>}
        {edit && x.employee_id && jn && jn.done >= jn.total && !verified && x.verify_status !== 'Pending' && x.stage !== 'Closed' && <><button className="btn primary" disabled={busy} onClick={() => act(() => api(`/offers/${x.offer_id}/send`, { method: 'POST' }), (r) => `Sent to ${r.plan?.chain?.[0] ?? 'Payroll'}`)}>{x.verify_status === 'Rejected' ? 'Resend to Payroll' : 'Send to Payroll'}</button>
          <SkipApprovalButton wf="verify" subject={x.employee_id} disabled={busy} onSkip={(q) => act(() => api(`/offers/${x.offer_id}/send${q}`, { method: 'POST' }), () => 'Verification approved (approval skipped)')} /></>}
      </div>
      {!x.newFlow && x.employeeIdAt !== 'joining' && !x.employee_id && <p className="mute" style={{ fontSize: 12 }}>Until the Employee ID exists, {x.name} is not in attendance, roster or payroll. Pay still counts from the joining date; earlier days can be added with Attendance upload or regularization.</p>}
      {x.newFlow && <EarlyExit x={x} act={act} />}
      {x.employee_id && x.joined_on && can('att.portal') && <PortalCard id={x.employee_id} />}
    </> : !c.markJoined && !c.outcome && !x.waitingWindow && <p className="mute" style={{ margin: 0, fontSize: 13 }}>{x.stage === 'Closed' ? '—' : 'After the candidate accepts the offer.'}</p>}
    {!x.hiring?.bgvOff && (c.bgv || x.bgv) && <><h3 className="sec">Background check</h3><BgvPanel x={x} reload={reload} /></>}

    <h3 className="sec">Timeline</h3>
    <Timeline events={x.timeline} />

    {closing && <div className="card" style={{ marginTop: 14 }}>
      <Field label={closing === 'not-joined' ? 'Why did the candidate not join?' : 'Why is the candidate closed?'}><input type="text" value={why} onChange={(e) => setWhy(e.target.value)} /></Field>
      <p className="mute" style={{ fontSize: 12 }}>The candidate is closed for good and never uses an Employee ID. A new shortlist gets a new number.</p>
      <div className="row"><button className="btn" onClick={() => setClosing('')}>Cancel</button>
        <button className="btn bad" disabled={busy} onClick={() => act(() => api(`/candidates/${x.id}/${closing}`, { body: { reason: why } }), () => (closing === 'not-joined' ? 'Marked not joined' : 'Candidate closed'))}>{closing === 'not-joined' ? 'Mark not joined' : 'Close candidate'}</button></div>
    </div>}
  </Modal>;
}

// ---------- Offer for a selected candidate (today's offer form, prefilled from the shortlist) ----------

function OfferForm({ cand, onBack, onSent }: { cand: any; onBack: () => void; onSent: () => void }) {
  const { can } = useAuth();
  const canCreate = can('onb.raise', 2);
  const setup = useLoad(() => api<{ fields: SetupField[]; docs: SetupDoc[] }>('/onboarding/setup'), []);
  const [d, setD] = useState<Data>(() => ({ type: 'Permanent', nationality: 'Indian', uanAvail: 'No', ...strMap(cand.prefill) }));
  const owner: string = cand.id; // documents and "Ask candidate" stay with the candidate (SI-0001)
  const [docs, setDocs] = useState<Record<string, any>>(cand.docs || {});
  const [asked, setAsked] = useState<Record<string, boolean>>({});
  const [res, setRes] = useState<any>(null);
  const [show, setShow] = useState(false);
  const [busy, setBusy] = useState(false);
  const L = useLookups(d);
  // Like the prototype, the form starts on the default salary structure version.
  const defStruct = L.structs.find((s) => s.is_default && s.status === 'Active')?.id;
  useEffect(() => { if (defStruct) setD((x) => (x.structId ? x : { ...x, structId: defStruct })); }, [defStruct]);
  const set = (k: string, v: string) => setD((x) => {
    const n = { ...x, [k]: v };
    if (k === 'dept') { n.desig = ''; n.grade = ''; }
    if (k === 'desig') n.grade = '';
    if (k === 'city') { n.loc = ''; n.shift = ''; }
    if (k === 'loc') n.shift = '';
    return n;
  });
  const body = useMemo(() => JSON.stringify({ data: d, candidate: owner }), [d, owner]);
  // The check runs again on every change and after every upload; `uploads` counts the uploads.
  const [uploads, setUploads] = useState(0);
  const check = () => api('/offers/check', { body: JSON.parse(body) }).then((r) => { setRes(r); setDocs(r.docs || {}); setAsked(r.asked || {}); return r; });
  useEffect(() => {
    if (!canCreate) return;
    const t = setTimeout(() => check().catch(() => {}), 300);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [body, uploads]);
  if (!setup.data) return <Loading error={setup.error} />;
  const corporate = L.depts.find((x) => x.name === d.dept)?.corporate;
  const fields = setup.data.fields.filter((f) => f.offer > 0 && fieldApplies(f, d));
  const docList = setup.data.docs.filter((x) => x.offer > 0 && docApplies(x, d.type, d.dept || ''));
  const missing = (f: SetupField) => show && f.offer === 2 && f.kind !== 'flow' && f.kind !== 'policy' && !d[f.key];
  const city = L.cities.find((c) => c.id === d.city);
  const std = (L.structs.find((s) => s.id === d.structId) ?? L.structs.find((s) => s.is_default))?.rates;
  const lwf: string = res?.lwfNote ?? '';
  const rules = res?.rules ? { [d.type]: res.rules } : undefined;
  const policy: Policy | undefined = res?.policy;
  const ask = async (key: string) => {
    try { const r = await api(`/offers/draft/${owner}/docs/${key}/${asked[key] ? 'cancel' : 'request'}`, { method: 'POST' }); setAsked(r.asked || {}); } catch (x: any) { toast(x.message); }
  };
  const upload = async (key: string, file?: File) => {
    if (!file) return;
    try { const r = await uploadDoc('candidate', owner, key, file); setDocs((x) => ({ ...x, [key]: r })); setUploads((n) => n + 1); toast('Attached ' + file.name); } catch (x: any) { toast(x.message); }
  };
  // Send checks again first, so a list from before the last upload or keystroke never blocks it.
  const send = async (skip = '') => {
    setShow(true);
    setBusy(true);
    try {
      const now = await check();
      if (now.errors?.length) { toast(`${now.errors.length} thing${now.errors.length > 1 ? 's' : ''} to fix`); window.scrollTo(0, 0); return; }
      const r = await api('/offers' + skip, { body: { data: d, candidate: owner, atJoining: !!cand.atJoining } }); toast(`Offer v${r.version ?? 1}${cand.atJoining ? ' at joining' : ''} for ${cand.id} sent for approval`); onSent();
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const band = res?.band, gross = +d.gross || 0;
  const variable = rules?.[d.type]?.variable ?? d.type !== 'Retainer';
  const statutory = rules?.[d.type]?.statutory ?? d.type === 'Permanent'; // PF applies: "Does the employee want PF?" is asked
  const pos = !gross ? '' : band?.position;
  return (
    <div className="grid split">
      <div className="grid">
        <div className="note row sp"><span><b className="mono">{cand.id} offer v{(cand.offer_version || 0) + 1}{cand.atJoining ? ' at joining' : ''}</b> · for <b>{cand.name}</b>, selected by {cand.interviewed_by_name || cand.manager_name}{cand.interview_date ? ` on ${fmtD(cand.interview_date)}` : ''}. Prefilled from the shortlist {cand.id}.{cand.rejoin_of ? ` Rejoin of ${cand.rejoin_of}.` : ''}</span>
          <button className="btn sm" onClick={onBack}>Back to the candidate</button></div>
        {SECTIONS.map((sec) => {
          const fs = fields.filter((f) => f.section === sec);
          // Compensation: the variable pay target ("given as" and the value) sits where "Variable pay, % of gross" was,
          // in the row with Monthly gross, Salary structure version and Variable pay payout; Enter pay by CTC under it.
          return fs.length ? <div key={sec} className="card"><h3>{sec}</h3><div className="form">
            {fs.map((f) => f.key === 'varPct' ? (variable ? <OfferVarFields key={f.key} d={d} set={set} /> : null)
              : <label key={f.key} className={`field ${missing(f) ? 'err' : ''}`}><span>{f.label}{f.offer === 2 && f.kind !== 'flow' && f.kind !== 'policy' && <Req />}</span>
                <Control f={f} d={d} set={set} opts={L.opts} corporate={corporate} policy={policy} /></label>)}
          </div>
          {sec === 'Compensation' && d.type !== 'Retainer' && <div className="form" style={{ marginTop: 10 }}><label className="field"><span>Reimbursement, ₹ a month (outside the gross)</span>
            <input type="number" min={0} value={d.reimb ?? ''} onChange={(e) => set('reimb', e.target.value)} />
            <small className="mute">Paid on top of the gross like variable pay; the gross split is unchanged. Goes to the employee at joining.</small></label></div>}
          {sec === 'Compensation' && <OfferCtcEntry d={d} set={set} variable={variable} />}</div> : null;
        })}
        {statutory && <PfWantCard d={d} set={set} show={show} limit={res?.pfLimit} />}
        <AccomCard d={d} set={set} show={show} />
        <div className="card"><h3>Documents for offer release</h3>
          <p className="mute" style={{ marginTop: -6, fontSize: 12 }}>Documents needed before the offer goes out. The rest are collected at joining. They are kept with {cand.id}.</p>
          <DocTable list={docList} stage="offer" docs={docs} onUpload={upload} asked={asked} onAsk={ask} empty="No documents are needed at this stage." />
        </div>
      </div>
      <div className="card" style={{ position: 'sticky', top: 12 }}>
        <h3>Offer</h3><div className="bigid">{cand.id} v{(cand.offer_version || 0) + 1}</div>
        <p className="mute" style={{ fontSize: 12, margin: '4px 0 0' }}>The Offer ID (OI) is issued when the candidate accepts. A question about the breakup is answered with a new version.</p>
        <h3 style={{ marginTop: 16 }}>Salary breakup</h3>
        {res?.breakup ? <Statement c={res.breakup} type={d.type} std={std} rules={rules} lwf={lwf} /> : <p className="mute" style={{ fontSize: 13 }}>Enter the type and monthly gross to see the breakup.</p>}
        <h3 style={{ marginTop: 16 }}>Pay band</h3>
        {!d.grade ? <p className="mute" style={{ fontSize: 12.5, margin: 0 }}>Choose a grade to check the offer against the band.</p>
          : !band ? <p className="mute" style={{ fontSize: 12.5, margin: 0 }}>No band set for this grade in this city. Add one in Compensation.</p>
          : <><p style={{ fontSize: 13, margin: 0 }}>{d.grade} in {city?.name}: <b>{inr(band.min)} to {inr(band.max)}</b> <Pill k={pos === 'within' ? 'ok' : 'warn'}>{pos === 'within' ? 'Within band' : pos === 'above' ? 'Above band' : pos === 'below' ? 'Below band' : 'Enter gross'}</Pill></p>
            {(pos === 'above' || pos === 'below') && <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>{pos === 'above' ? 'Above the band. This counts as above budget and adds approvers.' : 'Below the band. Check the offer against the pay band before sending.'}</p>}
            <div className="row" style={{ marginTop: 8 }}><button className="btn sm" onClick={() => set('gross', String(bandMid(band.min, band.max)))}>Use midpoint {inr(bandMid(band.min, band.max))}</button></div></>}
        {policy && <><h3 style={{ marginTop: 16 }}>Work policy</h3>
          <p className="ink2" style={{ margin: 0, fontSize: 13 }}><b>{policy.name}</b> via {policy.via}.<br />{policy.weekOff}.<br />Probation {policy.probation ? `${policy.probation} months` : 'not applicable'}. {policy.pl}.</p></>}
        {res?.budget?.length > 0 && <div className="note warn" style={{ marginTop: 12 }}>This hire is above the {res.budget.join(' and ')} for {city?.name}. It needs extra approval.</div>}
        {res?.plan && <><h3 style={{ marginTop: 16 }}>Approval route</h3>
          <div className="chain">{(res.plan.chain as string[]).map((c, i) => <span key={i} style={{ display: 'contents' }}>{i > 0 && <span className="arrow">→</span>}<span className="step" title={res.plan.people?.find((p: any) => p.stage === c)?.name || undefined}>{c}</span></span>)}</div>
          {res.plan.why.length > 0 && <p className="mute" style={{ fontSize: 12 }}>Added because: {res.plan.why.join(', ')}</p>}</>}
        {res?.later > 0 && <p className="mute" style={{ fontSize: 12 }}>{res.later} more mandatory item{res.later > 1 ? 's' : ''} (details and documents) will be collected at joining.</p>}
        <p className="mute" style={{ fontSize: 12 }}>No Employee ID is used at offer approval. {cand.name} works on a Joining ID from the joining day; the Employee ID comes once every step is approved.</p>
        {show && res?.errors?.length > 0 && <div className="note bad" style={{ marginTop: 12 }}><b>Fix before sending</b><ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{res.errors.map((x: string) => <li key={x}>{x}</li>)}</ul></div>}
        <div className="row" style={{ marginTop: 16 }}><button className="btn" onClick={onBack}>Back</button><button className="btn primary" disabled={!canCreate || busy} onClick={() => send()}>Send offer for approval</button><SkipApprovalButton wf="joining" subject={owner} disabled={!canCreate || busy} onSkip={(q) => send(q)} /></div>
      </div>
    </div>
  );
}

/** Documents of a stage (prototype `docsHtml` / the joining dialog). `onAsk` adds "Ask candidate" (offer stage). */
function DocTable({ list, stage, docs, onUpload, readOnly, asked, onAsk, empty, marks }: { list: SetupDoc[]; stage: 'offer' | 'join'; docs: Record<string, any>; onUpload: (k: string, f?: File) => void; readOnly?: boolean; asked?: Record<string, boolean>; onAsk?: (k: string) => void; empty: string; marks?: boolean }) {
  return <div className="tw"><table><tbody>{list.length ? list.map((x) => {
    const st = stage === 'offer' ? x.offer : x.join, up = docs[x.key];
    return <tr key={x.key}>
      <td><b>{x.label}</b> {st === 2 ? <Pill k="bad">Mandatory</Pill> : <Pill k="n">Optional</Pill>}{marks && x.filledBy === 'joiner' && (String(up?.uploaded_by || '').endsWith('(joiner)') ? <JoinerMark filled /> : !up ? <JoinerMark /> : null)}{marks && up?.uploaded_by && <div className="mute" style={{ fontSize: 11.5 }}>by {up.uploaded_by}</div>}</td>
      <td>{up ? <Pill k="ok">Uploaded</Pill> : asked?.[x.key] ? <Pill k="info">Requested from candidate</Pill> : <Pill k="warn">Pending</Pill>}</td>
      <td><div className="upl">
        {!readOnly && <label className="btn sm">{up ? 'Replace' : 'Upload'}<input type="file" accept=".pdf,.jpg,.jpeg,.png" hidden onChange={(e) => { onUpload(x.key, e.target.files?.[0]); e.target.value = ''; }} /></label>}
        {up ? <span className="mute" style={{ fontSize: 12, cursor: 'pointer' }} title="Open" onClick={() => openDoc(up.id).catch((e) => toast(e.message))}>{up.file_name}</span>
          : onAsk && !readOnly && <button className="btn sm" onClick={() => onAsk(x.key)}>{asked?.[x.key] ? 'Cancel request' : 'Ask candidate'}</button>}
      </div></td>
    </tr>;
  }) : <tr><td className="mute">{empty}</td></tr>}</tbody></table></div>;
}

// ---------- Offers raised before stages (OFR-…): today's table, unchanged ----------

const reqKind = (s: string): 'ok' | 'warn' | 'bad' | 'n' => (s === 'Approved' ? 'ok' : s === 'Rejected' ? 'bad' : s === 'Cancelled' ? 'n' : 'warn');

function LegacyPipeline({ rows, reload }: { rows: any[]; reload: () => void }) {
  const { can } = useAuth();
  const [open, setOpen] = useState<{ id: string; view: 'offer' | 'join' } | null>(null);
  const page = usePage(rows, 100);
  const canRel = can('onb.raise', 2) || can('onb.pipeline', 2);
  const send = async (id: string) => { try { const r = await api(`/offers/${id}/send`, { method: 'POST' }); toast(`Sent to ${r.chain?.[0] ?? r.plan?.chain?.[0] ?? 'Payroll'}`); reload(); } catch (x: any) { toast(x.message); } };
  const accept = async (id: string) => { try { await api(`/offers/${id}/accept`, { body: {} }); toast('Marked accepted'); reload(); } catch (x: any) { toast(x.message); } };
  const offerCell = (r: any) => {
    if (r.request_status === 'Rejected' || r.request_status === 'Cancelled' || r.status === 'Declined') return <span className="mute">—</span>;
    if (r.request_status !== 'Approved') return <button className="btn sm" onClick={() => setOpen({ id: r.id, view: 'offer' })}>Preview</button>;
    if (!r.released_at) return canRel ? <button className="btn sm primary" onClick={() => setOpen({ id: r.id, view: 'offer' })}>Release offer</button> : <span className="mute">Ready</span>;
    const acc = !!r.accepted_at;
    return <>
      <button className={`pill ${acc ? 'ok' : 'info'}`} style={{ border: 0, cursor: 'pointer' }} onClick={() => setOpen({ id: r.id, view: 'offer' })}>{acc ? 'Accepted' : `Released ${fmtD(r.released_at)}`}</button>
      {' '}{!acc && canRel && <button className="btn sm" onClick={() => accept(r.id)}>Mark accepted</button>}
    </>;
  };
  const joinCell = (r: any) => {
    if (r.request_status !== 'Approved') return <span className="mute">After offer approval</span>;
    if (r.status === 'Declined') return <span className="mute">—</span>;
    if (!r.released_at) return <span className="mute">After the offer is released</span>;
    if (!r.accepted_at) return <span className="mute">After the candidate accepts the offer</span>;
    if (r.status === 'Verified') return <div className="row" style={{ gap: 8 }}><Pill k="ok">Verified by Payroll</Pill></div>;
    if (r.status === 'With Payroll') return <div className="row" style={{ gap: 8 }}><Pill k="info">With {r.verify_chain?.[r.verify_step] ?? 'Payroll'}</Pill><button className="btn sm" onClick={() => setOpen({ id: r.id, view: 'join' })}>View</button></div>;
    const done = r.joining.done >= r.joining.total;
    return <div className="row" style={{ gap: 8 }}>
      {r.status === 'Returned by Payroll' ? <Pill k="bad">Returned by Payroll</Pill> : r.joining.total ? <Pill k={done ? 'ok' : 'warn'}>{r.joining.done} of {r.joining.total} mandatory</Pill> : <Pill k="n">Nothing mandatory</Pill>}
      <button className="btn sm" onClick={() => setOpen({ id: r.id, view: 'join' })}>Open</button>
      {done && can('onb.pipeline', 2) && <button className="btn sm primary" onClick={() => send(r.id)}>{r.status === 'Returned by Payroll' ? 'Resend to Payroll' : 'Send to Payroll'}</button>}
    </div>;
  };
  return <div className="card">
    <div className="tw"><table>
      <thead><tr><th>Request</th><th>Candidate</th><th>Type</th><th>City</th><th>Joining</th><th>Offer documents</th><th>Approval progress</th><th>Status</th><th>Offer</th><th>Joining details and Payroll verification</th></tr></thead>
      <tbody>{page.rows!.map((r) => <tr key={r.id}>
        <td className="mono">{r.id}</td>
        <td><b>{r.candidate_name}</b><div className="mute" style={{ fontSize: 12 }}>{r.designation}</div>{r.rejoin_of && <Pill k="info">Rejoin</Pill>}</td>
        <td>{r.type}</td><td>{r.city}</td><td>{fmtD(r.doj)}</td>
        <td>{r.offerDocs.total ? <Pill k={r.offerDocs.done >= r.offerDocs.total ? 'ok' : 'warn'}>{r.offerDocs.done} of {r.offerDocs.total} uploaded</Pill> : <span className="mute">—</span>}</td>
        <td>{r.chain?.length ? <Chain steps={r.chain} step={r.step} status={r.request_status} /> : <span className="mute">—</span>}</td>
        <td>{r.status === 'Declined' ? <Pill k="bad">Declined</Pill> : <Pill k={reqKind(r.request_status)}>{r.request_status}</Pill>}</td>
        <td>{offerCell(r)}</td>
        <td>{joinCell(r)}</td>
      </tr>)}</tbody>
    </table></div>
    {page.pager}
    {open?.view === 'offer' && <OfferModal id={open.id} onClose={() => { setOpen(null); reload(); }} />}
    {open?.view === 'join' && <JoiningModal id={open.id} onClose={() => { setOpen(null); reload(); }} />}
  </div>;
}

function OfferModal({ id, onClose }: { id: string; onClose: () => void }) {
  const { can } = useAuth();
  const { data: o, error, reload } = useLoad(() => api(`/offers/${id}`), [id]);
  const [why, setWhy] = useState('');
  const [declining, setDeclining] = useState(false);
  const canRel = can('onb.raise', 2) || can('comp.approve', 2);
  const [acc, setAcc] = useState<Data | null>(null);
  const accom = acc ?? { accom: String(o?.data?.accom || 'No'), accomCluster: String(o?.data?.accomCluster || '') };
  const step = async (s: 'release' | 'decline') => {
    try { await api(`/offers/${id}/${s}`, { body: s === 'decline' ? { reason: why } : o?.candidate_id ? accom : {} }); toast(s === 'release' ? 'Offer released' : 'Marked declined'); if (s === 'release') onClose(); else { reload(); setDeclining(false); } } catch (x: any) { toast(x.message); }
  };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Offer letter{o ? `: ${o.candidate_name}` : ''} {o && <span className="mono mute" style={{ fontSize: 13 }}>{o.candidate_id ? (o.offer_no || `${o.candidate_id} offer v${o.version || 1}`) : o.id}</span>}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    {error && <div className="note bad">{error}</div>}
    {o && <>
      {o.letterRule && <div className={`note ${o.letterHeld ? 'warn' : 'ok'}`} style={{ marginTop: 10 }}>{o.letterRule}{o.letterReleasedAt ? ` Released at joining on ${fmtD(o.letterReleasedAt)}.` : ''}</div>}
      {o.letterHeld
        ? <div className="mute" style={{ border: '1px dashed var(--line)', borderRadius: 10, padding: 14, margin: '12px 0', fontSize: 13 }}>The offer letter is held until joining. It is generated when the candidate is marked joined.</div>
        : <div style={{ whiteSpace: 'pre-wrap', fontSize: 13, lineHeight: 1.6, border: '1px solid var(--line)', borderRadius: 10, padding: 14, margin: '12px 0' }}>{o.letter}</div>}
      {o.accommodation && <p style={{ fontSize: 13 }}>{o.accommodation}</p>}
      {o.status === 'Approved' && canRel && o.candidate_id && !o.at_joining && <AccomCard d={accom} set={(k, v) => setAcc({ ...accom, [k]: v, ...(k === 'accom' && v === 'No' ? { accomCluster: '' } : {}) })} show plain />}
      {declining && <Field label="Why did the candidate decline?"><input value={why} onChange={(e) => setWhy(e.target.value)} /></Field>}
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        <button className="btn" onClick={onClose}>Close</button>
        {canRel && ['Approved', 'Released', 'Accepted'].includes(o.status) && (declining ? <button className="btn bad" onClick={() => step('decline')}>Mark declined</button> : <button className="btn" onClick={() => setDeclining(true)}>Candidate declined</button>)}
        {o.status === 'Approved' && canRel && !o.at_joining && <button className="btn primary" onClick={() => step('release')}>Release offer</button>}
      </div>
      {o.request_status !== 'Approved' && !o.at_joining && <p className="mute" style={{ fontSize: 12 }}>The offer can be released after all approvals are complete.</p>}
      {o.candidate_id && o.status === 'Approved' && !o.at_joining && <p className="mute" style={{ fontSize: 12 }}>On release the answer is due within the days set in Form setup › Hiring; without an answer by then the offer lapses. Record the candidate's answer on the candidate.</p>}
    </>}
  </Modal>;
}

function JoiningModal({ id, onClose }: { id: string; onClose: () => void }) {
  const { can } = useAuth();
  const { data: o, error, reload } = useLoad(() => api(`/offers/${id}`), [id]);
  const [d, setD] = useState<Data | null>(null);
  // base: the details as last read from the server. HR's Save sends only what HR changed from it, so what the
  // joiner saved meanwhile is not overwritten, and Refresh brings in the joiner's latest without losing HR's typing.
  const base = useRef<Data>({});
  const [link, setLink] = useState<JoinStatus | null>(null);
  const [err, setErr] = useState<string[]>([]);
  const fresh = useRef(false);
  useEffect(() => {
    if (!o || (d && !fresh.current)) return;
    const server: Data = Object.fromEntries(Object.entries(o.data || {}).map(([k, v]) => [k, String(v)]));
    if (d) {
      const mine = Object.keys(d).filter((k) => (d[k] ?? '') !== (base.current[k] ?? ''));
      const next = { ...server };
      for (const k of mine) next[k] = d[k];
      if (mine.length) toast(`Updated with the joiner's latest. Your ${mine.length === 1 ? 'unsaved change was' : mine.length + ' unsaved changes were'} kept.`);
      setD(next);
    } else setD(server);
    base.current = server;
    fresh.current = false;
  }, [o, d]);
  const L = useLookups({ ...(d || {}), dept: o?.department, desig: o?.designation, city: o?.city_id, loc: o?.location_id });
  const edit = (can('onb.raise', 2) || can('onb.pipeline', 2)) && ['Accepted', 'Returned by Payroll', 'Released'].includes(o?.status);
  if (error) return <Modal onClose={onClose}><div className="note bad">{error}</div></Modal>;
  if (!o || !d) return null;
  const fields = (o.joinFields as SetupField[]).filter((f) => fieldApplies(f, d));
  const save = async () => {
    setErr([]);
    const changed = fields.map((f) => f.key).filter((k) => (d[k] ?? '') !== (base.current[k] ?? ''));
    try {
      const r = await api(`/offers/${id}/joining`, { body: { data: Object.fromEntries(changed.map((k) => [k, d[k] ?? ''])) } });
      toast(r?.employeeId ? `Joining details saved. Employee ID ${r.employeeId} created` : 'Joining details saved'); reload(); onClose();
    } catch (x: any) { setErr((x.details as string[]) || [x.message]); }
  };
  // A candidate's documents stay with the candidate (SI-…); an offer raised before stages keeps its own.
  const own = o.docOwner || { kind: 'offer', id };
  const upload = async (key: string, file?: File) => {
    if (!file) return;
    try { const r = await uploadDoc(own.kind, own.id, key, file); setErr([]); toast(r.note || 'Attached ' + file.name); reload(); } catch (x: any) { toast(x.message); }
  };
  const docList = (o.docList as SetupDoc[]).map((x) => ({ ...x, join: Math.max(x.join, x.offer) }));
  // HR's own fields first; the joiner's after, each marked by whether the joiner actually entered it.
  const fromJoiner = new Set(link?.joinerKeys || []);
  const linkOut = !!link?.link;
  const hrFields = fields.filter((f) => f.filledBy !== 'joiner'), jFields = fields.filter((f) => f.filledBy === 'joiner');
  const hrEmpty = hrFields.filter((f) => f.join === 2 && !(d[f.key] || '').trim()).length;
  const one = (f: SetupField, hr: boolean) => {
    const empty = !(d[f.key] || '').trim();
    return <label key={f.key} className={`field${hr && edit && f.join === 2 && empty ? ' need' : ''}`}><span>{f.label}{f.join === 2 && <Req />}
      {!hr && (fromJoiner.has(f.key) ? <JoinerMark filled /> : linkOut && empty ? <JoinerMark /> : null)}</span>
      {edit ? <Control f={f} d={d} set={(k, v) => setD({ ...d, [k]: v })} opts={L.opts} /> : <div className="ro">{d[f.key] || <span className="mute">—</span>}</div>}
      {f.key === 'pfChoice' && <em className="mute" style={{ fontSize: 12 }}>{PF_CHOICE_HINT}</em>}</label>;
  };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Joining details: {o.candidate_name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5 }}>{o.id}{o.employee_id ? ` · ${o.employee_id}` : ''} · {o.type} · joining {fmtD(o.doj)}. Fields marked * and mandatory documents must be complete.</p>
    {o.panCheck && <p style={{ fontSize: 12.5, margin: '0 0 8px' }}>PAN check: <Pill k={o.panCheck.status === 'Verified' ? 'ok' : o.panCheck.status === 'Not checked' ? 'n' : 'bad'}>{o.panCheck.status}</Pill>
      {o.panCheck.name ? <span className="mute"> · name on PAN {o.panCheck.name}</span> : null}{o.panCheck.status === 'Not checked' ? <span className="mute"> · checked when the PAN is saved and PAN verification is on</span> : null}</p>}
    <JoinerLinkPanel id={id} name={o.candidate_name} onStatus={setLink} onChange={() => { fresh.current = true; reload(); }} />
    {!fields.length && <p className="mute">No fields are asked at joining.</p>}
    {hrFields.length > 0 && <>
      <h3 style={{ marginTop: 12 }}>Your fields (HR){edit && hrEmpty > 0 && <span className="pill warn jpmark">{hrEmpty} required to fill</span>}</h3>
      <div className="form" style={{ marginTop: 6 }}>{hrFields.map((f) => one(f, true))}</div>
    </>}
    {jFields.length > 0 && <>
      <h3 style={{ marginTop: 16 }}>The joiner's fields</h3>
      <p className="mute" style={{ fontSize: 12, margin: '2px 0 0' }}>{linkOut ? <><JoinerMark filled /> the joiner entered it · <JoinerMark /> still empty, for the joiner to fill. You can correct any of them.</> : 'The joiner fills these from their link. Without a link, fill them here.'}</p>
      <div className="form" style={{ marginTop: 6 }}>{jFields.map((f) => one(f, false))}</div>
    </>}
    <h3 style={{ marginTop: 16 }}>Documents</h3>
    <DocTable list={docList} stage="join" docs={o.docs || {}} onUpload={upload} readOnly={!edit} asked={o.asked} empty="No documents are asked at joining." marks />
    {err.length > 0 && <div className="note bad" style={{ marginTop: 10 }}><ul style={{ margin: 0, paddingLeft: 18 }}>{err.map((x) => <li key={x}>{x}</li>)}</ul></div>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>{edit ? 'Cancel' : 'Close'}</button>{edit && <button className="btn primary" onClick={save}>Save</button>}</div>
  </Modal>;
}

// ---------- The joiner's link (joining_link.go): the new joiner fills their own details ----------

type JoinLink = { id: string; state: 'live' | 'opened' | 'submitted' | 'expired' | 'locked' | 'stopped'; createdAt: string; createdBy: string; expiresAt: string; sentAt: string | null; sentTo: string | null;
  openedAt: string | null; verifiedAt: string | null; savedAt: string | null; submittedAt: string | null; via: string[]; note: string | null; joinerKeys: string[]; lockedAt: string | null; revokedAt: string | null; revokeReason: string | null };
type JoinStatus = { mail: 'smtp' | 'echo' | 'off' | string; email: string; mobile: string; editable: boolean; status: string; canSend: boolean; company: string; link: JoinLink | null; joinerKeys: string[] };
const LINK_STATE: Record<JoinLink['state'], [string, 'ok' | 'warn' | 'bad' | 'info' | 'n']> = {
  live: ['Sent, not opened yet', 'info'], opened: ['Opened by the joiner', 'info'], submitted: ['Submitted by the joiner', 'ok'],
  expired: ['Expired', 'warn'], locked: ['Locked: too many wrong codes', 'bad'], stopped: ['Stopped', 'n'],
};
const fmtDT = (s?: string | null) => (s ? new Date(s).toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }) : '');
// The raw link is known only when it is made, so this browser keeps the links it made (for Copy and WhatsApp later).
const knownLink = (id?: string) => { try { return (id && sessionStorage.getItem('hrms.joinlink.' + id)) || ''; } catch { return ''; } };
const keepLink = (id: string, url: string) => { try { sessionStorage.setItem('hrms.joinlink.' + id, url); } catch { /* private mode */ } };

function JoinerLinkPanel({ id, name, onChange, onStatus }: { id: string; name: string; onChange: () => void; onStatus?: (s: JoinStatus) => void }) {
  const { data: st, error, reload } = useLoad(() => api<JoinStatus>(`/joiner-link/${id}`), [id]);
  const [url, setUrl] = useState('');
  const [busy, setBusy] = useState(false);
  const [code, setCode] = useState('');
  const [reopen, setReopen] = useState<string | null>(null);
  const [warn, setWarn] = useState('');
  useEffect(() => { if (st && onStatus) onStatus(st); }, [st]); // eslint-disable-line react-hooks/exhaustive-deps
  if (error || !st) return null; // e.g. Payroll reading the details while verifying: the link is Onboarding's
  const l = st.link, live = !!l && (l.state === 'live' || l.state === 'opened');
  const known = live ? (url && url.includes('/join/') ? url : knownLink(l!.id)) : '';
  const first = name.split(' ')[0];
  const mailOn = st.mail !== 'off';
  const sentWhen = l ? (l.sentAt ? `emailed ${fmtDT(l.sentAt)}` : `made ${fmtDT(l.createdAt)}`) : '';
  // A live link this browser does not hold (made in another tab or by a colleague) can only be replaced.
  const replaceOK = (via: 'email' | 'copy' | 'whatsapp') => !live || !!known ||
    window.confirm(`${first} already has a link (${sentWhen}). This browser does not have a copy of it, so a new link will be made${via === 'email' ? ' and emailed' : ''}, and the link ${first} has now will stop working.${via === 'email' ? '' : ` Send ${first} the new one.`} Continue?`);
  const issue = async (via: 'email' | 'copy' | 'whatsapp' | 'renew', reuse = true) => {
    setBusy(true); setWarn('');
    try {
      const token = reuse && known ? known.split('/join/')[1] : '';
      const r = await api(`/joiner-link/${id}`, { body: { via, ...(token ? { token } : {}) } });
      keepLink(r.linkId, r.url); setUrl(r.url); setCode('');
      if (r.emailError) setWarn(r.emailError);
      reload();
      return r as { url: string; emailed: boolean };
    } catch (x: any) { toast(x.message); return null; } finally { setBusy(false); }
  };
  const copy = async () => {
    if (!replaceOK('copy')) return;
    const p = issue('copy');
    // Safari allows writing to the clipboard only inside the click, so the write starts now with the link to come.
    let wrote = false;
    try {
      if (typeof ClipboardItem !== 'undefined' && navigator.clipboard?.write) {
        await navigator.clipboard.write([new ClipboardItem({ 'text/plain': p.then((r) => { if (!r) throw new Error('no link'); return new Blob([r.url], { type: 'text/plain' }); }) })]);
        wrote = true;
      }
    } catch { /* the plain write below, or the link shown in the panel */ }
    const r = await p;
    if (!r) return;
    if (!wrote) { try { await navigator.clipboard.writeText(r.url); wrote = true; } catch { /* shown below */ } }
    toast(wrote ? 'Link copied. Paste it in a message to ' + first : 'Copy the link shown below');
  };
  const whatsapp = async () => {
    if (!replaceOK('whatsapp')) return;
    // Opened inside the click (Safari blocks a window opened after a network call), pointed at WhatsApp after.
    const w = window.open('about:blank', '_blank');
    const r = await issue('whatsapp');
    if (!r) { w?.close(); return; }
    const text = `Hello ${first}, welcome to ${st.company}! Please fill in your joining details and upload your documents here: ${r.url}\n` +
      (mailOn && st.email ? `When you open it, you will get a 6-digit code by email at ${st.email}.` : 'When you open it, HR will call you with your 6-digit code.');
    const to = `https://wa.me/${st.mobile ? '91' + st.mobile : ''}?text=${encodeURIComponent(text)}`;
    if (w) { w.opener = null; w.location.href = to; } else window.open(to, '_blank', 'noopener');
  };
  const email = async () => { if (!replaceOK('email')) return; const r = await issue('email'); if (r?.emailed) toast(`Link emailed to ${st.email}`); };
  const renew = async () => { if (!window.confirm(`Make a new link? The link ${first} has now stops working.`)) return; const r = await issue('renew', false); if (r) toast(r.emailed ? `New link emailed to ${st.email}` : 'New link made. Copy it or share it on WhatsApp'); };
  const act = async (action: 'stop' | 'code' | 'reopen', body?: any) => {
    setBusy(true);
    try {
      const r = await api(`/joiner-link/${id}/${action}`, { body: body ?? {} });
      if (action === 'code') setCode(r.code);
      if (action === 'reopen') { keepLink(r.linkId, r.url); setUrl(r.url); setReopen(null); toast(r.emailed ? `Sent back to ${first}. New link emailed to ${st.email}` : `Sent back to ${first}. Copy the new link or share it on WhatsApp`); if (r.emailError) setWarn(r.emailError); }
      if (action === 'stop') { setUrl(''); toast('The link no longer works'); }
      reload(); if (action === 'reopen') onChange();
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const [label, tone] = l ? LINK_STATE[l.state] : ['Not sent', 'n' as const];
  const can = st.canSend && st.editable;
  const submitted = l?.state === 'submitted';
  // What the panel says follows the link and the offer.
  const intro = !st.editable
    ? st.status === 'With Payroll' ? <>The joining details are with Payroll for verification, so the link is closed. If Payroll returns them, you can send {first} a link again.</>
      : st.status === 'Verified' ? <>Payroll has verified the joining details. The link is no longer needed.</>
        : st.status === 'Declined' ? <>{first} declined the offer, so no link can be sent.</>
          : ['Accepted', 'Released', 'Returned by Payroll'].includes(st.status) ? <>The hire is closed, so no link can be sent.</>
            : <>The link can be sent once the offer is released.</>
    : submitted ? <>{first} sent their details{l?.submittedAt ? ` on ${fmtDT(l.submittedAt)}` : ''}. Check them below, fill your own fields (HR) and Save. To ask {first} to correct or add something, use <b>Send back to {first}</b>.</>
      : live ? <>{first} has the link ({sentWhen}). What they save shows here: press <b>Refresh</b> to see the latest. Meanwhile, fill your own fields (HR).{!mailOn && <> {first} needs a code to open it: you get a message in the bell when they open it or ask; call them and use <b>Give a code by phone</b>.</>}</>
        : l?.state === 'locked' ? <>The link locked after too many wrong codes. Send {first} a new link.</>
          : <>Send {first} a link to fill in their own details and upload their documents from their phone. You can still correct anything, fill your own fields (HR) and Save as usual.</>;
  return <div className="jlink" aria-label="Joiner's link">
    <div className="row sp"><b>Joiner's link</b><Pill k={tone}>{label}</Pill></div>
    <p className="ink2" style={{ fontSize: 12.5, margin: '4px 0 8px' }}>{intro}</p>
    <div style={{ fontSize: 12.5 }}>
      Email: {st.email ? <b>{st.email}</b> : <span className="mute">none on the offer</span>} · Mobile: {st.mobile ? <b>{st.mobile}</b> : <span className="mute">none on the offer</span>}
      {l && <div className="mute" style={{ marginTop: 4 }}>
        Made {fmtDT(l.createdAt)} by {l.createdBy}{l.sentAt ? ` · emailed ${fmtDT(l.sentAt)}${l.sentTo ? ' to ' + l.sentTo : ''}` : ''}{live || submitted ? ` · works until ${fmtD(l.expiresAt)}` : ''}
        {l.openedAt ? ` · opened ${fmtDT(l.openedAt)}` : ''}{l.savedAt ? ` · last saved ${fmtDT(l.savedAt)}` : ''}{l.submittedAt ? ` · submitted ${fmtDT(l.submittedAt)}` : ''}
        {l.state === 'stopped' && l.revokeReason ? ` · ${l.revokeReason.toLowerCase()}` : ''}{l.note ? ` · note: “${l.note}”` : ''}
      </div>}
    </div>
    {can && st.mail === 'off' && !submitted && <div className="note" style={{ marginTop: 8, fontSize: 12.5 }}>Email is not set up on this server, so the link cannot be emailed. Use <b>Copy link</b> or <b>Share on WhatsApp</b>. When {first} opens it, call them on their mobile and read out a code from <b>Give a code by phone</b>.</div>}
    {can && st.mail === 'echo' && <div className="note" style={{ marginTop: 8, fontSize: 12.5 }}>Development: emails are written to the server log, not sent.</div>}
    {can && live && !known && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>The link was made in another browser tab or by a colleague, so it is not shown here. Copy link, Share on WhatsApp or Email it again make a new link, and the one {first} has stops working.</p>}
    {can && <div className="row" style={{ marginTop: 10, gap: 8 }}>
      {!submitted && <>
        {!mailOn && live && <button className="btn sm primary" disabled={busy} onClick={() => act('code')}>Give a code by phone</button>}
        {mailOn && <button className="btn sm primary" disabled={busy || !st.email} title={!st.email ? 'There is no email address on the offer' : undefined} onClick={email}>{live && l?.sentAt ? 'Email it again' : 'Email the link'}</button>}
        <button className="btn sm" disabled={busy} onClick={copy}>Copy link</button>
        <button className="btn sm" disabled={busy} onClick={whatsapp} title={st.mobile ? undefined : 'No mobile on the offer: WhatsApp asks whom to send it to'}>Share on WhatsApp</button>
      </>}
      {submitted && reopen === null && <button className="btn sm primary" disabled={busy} onClick={() => setReopen('')}>Send back to {first}</button>}
      {l && <button className="btn sm" disabled={busy} onClick={() => { reload(); onChange(); }}>Refresh</button>}
    </div>}
    {can && live && <div className="row" style={{ marginTop: 6, gap: 14, fontSize: 12.5 }}>
      {mailOn && <a role="button" tabIndex={0} className="lnk" onClick={() => !busy && act('code')}>Give a code by phone</a>}
      <a role="button" tabIndex={0} className="lnk" onClick={() => !busy && renew()}>Send a new link</a>
      <a role="button" tabIndex={0} className="lnk bad" onClick={() => { if (!busy && window.confirm(`Stop this link? ${first} can no longer open it.`)) act('stop'); }}>Stop the link</a>
    </div>}
    {reopen !== null && <div className="row" style={{ marginTop: 10, alignItems: 'flex-end' }}>
      <Field label={`Note for ${first}: what to correct or add`}><input type="text" maxLength={300} value={reopen} onChange={(e) => setReopen(e.target.value)} placeholder="e.g. Upload a clearer photo of your PAN card" /></Field>
      <button className="btn sm primary" disabled={busy} onClick={() => act('reopen', { note: reopen })}>Send back with a new link</button>
      <button className="btn sm" onClick={() => setReopen(null)}>Cancel</button>
    </div>}
    {known && <label className="field" style={{ marginTop: 10 }}><span>The link (works for 7 days; share it only with {first})</span><input readOnly value={known} onFocus={(e) => e.target.select()} /></label>}
    {code && <div className="note ok" style={{ marginTop: 10 }}>Code <b className="mono" style={{ fontSize: 16, letterSpacing: 2 }}>{code}</b>. Call {first}{st.mobile ? ` on ${st.mobile}` : ''} and read it out. It works for 10 minutes and is shown only once.</div>}
    {warn && <div className="note bad" style={{ marginTop: 10 }}>{warn}</div>}
  </div>;
}

// ---------- Form setup (prototype `vFormSetup`) and when the Employee ID is created ----------

function FormSetup() {
  const { can } = useAuth();
  const { data, error, reload } = useLoad(() => api<{ fields: SetupField[]; docs: SetupDoc[] }>('/onboarding/setup'), []);
  const depts = useLoad(() => api<any[]>('/masters/department?status=Active'), []);
  const edit = can('onb.setup', 2);
  const [cf, setCf] = useState({ label: '', section: 'Job', kind: 'text', options: '', offer: 0, join: 1 });
  const [cd, setCd] = useState({ label: '', offer: 0, join: 1 });
  const [dep, setDep] = useState<Record<string, string>>({});
  if (!data) return <Loading error={error} />;
  const patch = async (kind: 'fields' | 'docs', key: string, body: any) => {
    try { await api(`/onboarding/setup/${kind}/${key}`, { method: 'PATCH', body }); reload(); } catch (x: any) { toast(x.message); }
  };
  const add = async (kind: 'fields' | 'docs', body: any, done: () => void) => {
    try { await api(`/onboarding/setup/${kind}`, { body }); toast('Added'); done(); reload(); } catch (x: any) { toast(x.message); }
  };
  const stOpts = STATES.map(([v, l]) => <option key={v} value={v}>{l}</option>);
  const bySel = (kind: 'fields' | 'docs', x: SetupField | SetupDoc, fixed: boolean) => fixed
    ? <span className="mute" style={{ fontSize: 12.5 }} title="System fields, pay and fields that pick from the company's lists are always filled by HR">HR</span>
    : <select aria-label={`Who fills ${x.label}`} disabled={!edit} value={x.filledBy} onChange={(e) => patch(kind, x.key, { filledBy: e.target.value })}><option value="joiner">Joiner</option><option value="hr">HR</option></select>;
  const stSel = (kind: 'fields' | 'docs', x: SetupField | SetupDoc, stg: 'offer' | 'join') =>
    <select disabled={!edit || (x.sys && stg === 'offer')} value={x[stg]} onChange={(e) => patch(kind, x.key, { [stg]: +e.target.value })}>{stOpts}</select>;
  const typePills = (kind: 'fields' | 'docs', x: SetupField | SetupDoc) => TYPES.map((t, i) => {
    const on = x.types.includes(t);
    return <span key={t}>{i > 0 && ' '}<button className={`pill ${on ? 'info' : 'n'}`} disabled={!edit || x.sys} style={{ border: 0, cursor: 'pointer' }}
      onClick={() => patch(kind, x.key, { types: on ? x.types.filter((y) => y !== t) : [...x.types, t] })}>{t === 'Permanent' ? 'Perm' : t === 'Retainer' ? 'Retain' : 'Contract'}</button></span>;
  });
  const saveDepts = (x: SetupDoc) => {
    const v = dep[x.key];
    if (v === undefined) return;
    const list = v.split(',').map((y) => y.trim()).filter(Boolean);
    const unknown = list.filter((y) => !depts.data?.some((dp) => dp.name === y));
    setDep((o) => { const n = { ...o }; delete n[x.key]; return n; });
    if (unknown.length) { toast(`Not an active department: ${unknown.join(', ')}`); return; }
    if (list.join(',') !== x.depts.join(',')) patch('docs', x.key, { depts: list });
  };
  return <>
    <HireSettings />
    <div className="note" style={{ marginBottom: 14 }}>Every field and document is asked at two stages. <b>Offer release</b> is what HR enters on the offer for a selected candidate, before it goes for approval. <b>Joining</b> is what is collected once the candidate accepts and joins. At each stage choose Mandatory, Optional or Not needed. Nothing is deleted, only switched off. The shortlist asks personal details, work experience, existing CTC and location.
      <div style={{ marginTop: 6 }}><b>Filled by</b> says who fills a joining detail: the <b>Joiner</b> from the link HR sends them (Onboarding › Joining details › Joiner's link), or <b>HR</b>. HR can still correct what the joiner filled. System fields, pay and fields that pick from the company's lists are always HR's.</div></div>
    <div className="card" style={{ marginBottom: 14 }}><h3>Form fields</h3><div className="tw"><table>
      <thead><tr><th>Field</th><th>Section</th><th>Shown</th><th>Offer release</th><th>Joining</th><th>Filled by</th><th>Applies to</th></tr></thead>
      <tbody>{data.fields.map((f) => <tr key={f.key}><td><b>{f.label}</b> {f.sys ? <Pill k="n">System</Pill> : f.custom ? <Pill k="info">Custom</Pill> : null}</td><td className="mute">{f.section}</td>
        <td><Switch on={f.on} disabled={!edit || f.sys} label={`Show ${f.label}`} onChange={() => patch('fields', f.key, { on: !f.on })} /></td><td>{stSel('fields', f, 'offer')}</td><td>{stSel('fields', f, 'join')}</td><td>{bySel('fields', f, !joinerAllowed(f))}</td><td>{typePills('fields', f)}</td></tr>)}</tbody>
    </table></div></div>
    {edit && <div className="card" style={{ marginBottom: 14 }}><h3>Add a custom field</h3><div className="form">
      <label className="field"><span>Label</span><input type="text" value={cf.label} onChange={(e) => setCf({ ...cf, label: e.target.value })} /></label>
      <label className="field"><span>Section</span><select value={cf.section} onChange={(e) => setCf({ ...cf, section: e.target.value })}>{SECTIONS.map((s) => <option key={s}>{s}</option>)}</select></label>
      <label className="field"><span>Type</span><select value={cf.kind} onChange={(e) => setCf({ ...cf, kind: e.target.value })}><option value="text">Text</option><option value="number">Number</option><option value="date">Date</option><option value="sel">Dropdown</option></select></label>
      <label className="field"><span>Dropdown options</span><input type="text" placeholder="Comma separated" value={cf.options} onChange={(e) => setCf({ ...cf, options: e.target.value })} /></label>
      <label className="field"><span>At offer release</span><select value={cf.offer} onChange={(e) => setCf({ ...cf, offer: +e.target.value })}>{stOpts}</select></label>
      <label className="field"><span>At joining</span><select value={cf.join} onChange={(e) => setCf({ ...cf, join: +e.target.value })}>{stOpts}</select></label>
    </div><div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={() => add('fields', { ...cf, options: cf.kind === 'sel' ? cf.options : '' }, () => setCf({ ...cf, label: '', options: '' }))}>Add field</button></div></div>}
    <DocWindowSetting />
    <div className="card" style={{ marginBottom: 14 }}><h3>Documents</h3><div className="tw"><table>
      <thead><tr><th>Document</th><th>Shown</th><th>Offer release</th><th>Joining</th><th>Filled by</th><th>Applies to</th><th>Departments</th></tr></thead>
      <tbody>{data.docs.map((x) => <tr key={x.key}><td><b>{x.label}</b> {x.sys && <Pill k="n">System</Pill>}</td>
        <td><Switch on={x.on} disabled={!edit || x.sys} label={`Show ${x.label}`} onChange={() => patch('docs', x.key, { on: !x.on })} /></td><td>{stSel('docs', x, 'offer')}</td><td>{stSel('docs', x, 'join')}</td><td>{bySel('docs', x, false)}</td><td>{typePills('docs', x)}</td>
        <td><input type="text" disabled={!edit} placeholder="All departments" style={{ minWidth: 150 }} value={dep[x.key] ?? x.depts.join(', ')} onChange={(e) => setDep({ ...dep, [x.key]: e.target.value })} onBlur={() => saveDepts(x)} /></td></tr>)}</tbody>
    </table></div></div>
    {edit && <div className="card"><h3>Add a document</h3><div className="row">
      <input type="text" placeholder="e.g. Driving licence" value={cd.label} onChange={(e) => setCd({ ...cd, label: e.target.value })} style={{ maxWidth: 260 }} />
      <label className="field" style={{ margin: 0 }}><span>At offer release</span><select value={cd.offer} onChange={(e) => setCd({ ...cd, offer: +e.target.value })}>{stOpts}</select></label>
      <label className="field" style={{ margin: 0 }}><span>At joining</span><select value={cd.join} onChange={(e) => setCd({ ...cd, join: +e.target.value })}>{stOpts}</select></label>
      <button className="btn primary" onClick={() => add('docs', cd, () => setCd({ ...cd, label: '' }))}>Add document</button>
    </div></div>}
  </>;
}

/** What the Approvals page shows about a candidate on a shortlist or interview request (Approvals.tsx). */
export function CandidateBlock({ id }: { id: string }) {
  const { data: x, error } = useLoad(() => api(`/candidates/${id}`), [id]);
  if (!x) return error ? <div className="note bad" style={{ marginTop: 10 }}>{error}</div> : <Loading />;
  const d = x.data || {}, exp: Exp[] = x.experience || [], resume = x.docs?.resume;
  return <div style={{ marginTop: 12 }}>
    <dl className="kv">
      <dt>Shortlisting ID</dt><dd className="mono">{x.id}</dd>
      <dt>Role</dt><dd>{x.designation} · {x.department}</dd><dt>Location</dt><dd>{x.location}, {x.city}</dd>
      <dt>Existing CTC</dt><dd>{typeof x.existing_ctc === 'number' ? `${inr(x.existing_ctc)} a year` : '—'}</dd>
      <dt>Mobile</dt><dd>{d.phone || '—'}</dd><dt>Email</dt><dd>{d.email || '—'}</dd><dt>Date of birth</dt><dd>{fmtD(d.dob)}</dd><dt>Gender</dt><dd>{d.gender || '—'}</dd>
      <dt>Hiring manager</dt><dd>{x.manager_name}</dd><dt>Raised by</dt><dd>{x.raised_by_name}</dd>
      {x.interview_result && <><dt>Interview</dt><dd><Pill k={x.interview_result === 'Selected' ? 'ok' : 'bad'}>{x.interview_result}</Pill> {fmtD(x.interview_date)} by {x.interviewed_by_name}: “{x.interview_feedback}”</dd></>}
      <dt>Résumé</dt><dd>{resume ? <a href="#" onClick={(e) => { e.preventDefault(); openDoc(resume.id).catch((er) => toast(er.message)); }}>{resume.file_name}</a> : <span className="mute">None</span>}</dd>
    </dl>
    <div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Company</th><th>Designation</th><th>Period</th></tr></thead>
      <tbody>{exp.length ? exp.map((r, i) => <tr key={i}><td>{r.company}</td><td>{r.designation}</td><td style={{ whiteSpace: 'nowrap' }}>{ym(r.from)} to {r.to ? ym(r.to) : 'now'}</td></tr>)
        : <tr><td colSpan={3} className="mute">Fresher, no work experience</td></tr>}</tbody></table></div>
  </div>;
}

/** The hiring manager's interview form on the interview request (Approvals.tsx): Selected or Not selected. */
export function InterviewForm({ id, name, onDone }: { id: string; name: string; onDone: () => void }) {
  const [date, setDate] = useState(today());
  const [fb, setFb] = useState('');
  const [busy, setBusy] = useState(false);
  const save = async (result: 'Selected' | 'Not selected') => {
    if (!fb.trim()) { toast('Add your interview feedback'); return; }
    setBusy(true);
    try {
      const r = await api(`/candidates/${id}/interview`, { body: { date, result, feedback: fb } });
      toast(result === 'Selected' ? `${name} selected. HR raises the offer` : `${name} not selected`); onDone(); void r;
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  return <div style={{ marginTop: 14, paddingTop: 14, borderTop: '1px solid var(--line)' }}>
    <h3 style={{ margin: '0 0 10px' }}>Your interview</h3>
    <div className="form">
      <Field label="Interview date"><input type="date" value={date} max={today()} onChange={(e) => setDate(e.target.value)} /></Field>
      <label className="field" style={{ gridColumn: '1 / -1' }}><span>Feedback <b style={{ color: 'var(--bad)' }}>*</b></span>
        <textarea style={{ fontFamily: 'var(--font)', fontSize: 13, minHeight: 80 }} value={fb} onChange={(e) => setFb(e.target.value)} placeholder="What you saw: skills, attitude, fit for the role" /></label>
    </div>
    <div className="row" style={{ marginTop: 10 }}><button className="btn primary" disabled={busy} onClick={() => save('Selected')}>Selected</button><button className="btn bad" disabled={busy} onClick={() => save('Not selected')}>Not selected</button></div>
    <p className="mute" style={{ fontSize: 12 }}>Selected tells HR to raise the offer; the Offer ID is issued when the candidate accepts. Not selected closes the candidate.</p>
  </div>;
}

/** The offer's variable pay target (17 Nov 2026), in the Compensation card: the admin chooses each time, a % of the
 *  fixed gross (d.varMode "pct", d.varPct) or ₹ a month ("amount", d.varAmt); it goes to the employee at joining. */
function offerVarMode(d: Data): VarMode { return (d.varMode as VarMode) || (d.varAmt ? 'amount' : 'pct'); }
function offerSetVar(set: (k: string, v: string) => void, m: VarMode, v: string) { set('varMode', m); set(m === 'pct' ? 'varPct' : 'varAmt', v); set(m === 'pct' ? 'varAmt' : 'varPct', ''); }
function OfferVarFields({ d, set }: { d: Data; set: (k: string, v: string) => void }) {
  const mode = offerVarMode(d);
  return <VarTargetField mode={mode} value={mode === 'pct' ? d.varPct ?? '' : d.varAmt ?? ''} gross={+d.gross || 0} labels={['Variable pay given as', 'Variable pay target']}
    onChange={(m, v) => offerSetVar(set, m, v)} />;
}

/** Enter pay by CTC (fixed + variable), under the Compensation card: sets the monthly gross and the target in ₹. */
function OfferCtcEntry({ d, set, variable }: { d: Data; set: (k: string, v: string) => void; variable: boolean }) {
  const [p, setP] = useState<PayIn>({ ...PAY_IN, payBy: 'ctc' });
  const s = splitPay(p);
  const use = () => {
    if (!s) return;
    set('gross', String(s.fixed));
    if (variable) offerSetVar(set, 'amount', String(s.variable));
  };
  return <>
    {variable && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>The variable pay target is paid by HR entering the amount when the payout is due (Payroll › Variable pay).</p>}
    <details className="adv" style={{ marginTop: 10 }}><summary>Enter pay by CTC</summary><div><div className="form">
      <PayEntryFields p={p} set={setP} variable={variable} /></div>
      <button className="btn sm" style={{ marginTop: 8 }} disabled={!s} onClick={use}>Use this: gross {s ? inr(s.fixed) : '—'}{variable && s?.variable ? `, variable ${inr(s.variable)}` : ''}</button></div></details>
  </>;
}

/** "Company accommodation: Yes / No" on the offer (backend hst_auto.go): Yes needs a cluster with accommodation units;
 *  the joiner gets a bed there automatically when marked joined. No pay effect. */
/** "Does the employee want PF?" on the offer (owner, 26 Nov 2026): required, no default; only for a type PF applies to.
 *  No above the threshold = the whole gross as Basic with no PF; No up to it is refused (the type must be Retainer). */
function PfWantCard({ d, set, show, limit }: { d: Data; set: (k: string, v: string) => void; show?: boolean; limit?: number }) {
  const lim = limit || 25000;
  const low = d.pfWant === 'No' && Number(d.gross) > 0 && Number(d.gross) <= lim;
  return <div className="card"><h3>Provident Fund</h3><div className="form">
    <label className={`field ${(show && !d.pfWant) || low ? 'err' : ''}`}><span>Does the employee want PF?<Req /></span>
      <select value={d.pfWant ?? ''} onChange={(e) => set('pfWant', e.target.value)}><option value="">Select</option><option>Yes</option><option>No</option></select>
      <small className="mute">Yes: the normal structure (Basic, HRA, special allowance) with PF. No: no PF from the employee or the company, and the whole gross is paid as Basic; only above {inr(lim)} a month, else the type must be Retainer. The joining form asks again and its answer wins.</small></label>
  </div>
    {low && <div className="note bad" style={{ marginTop: 8, fontSize: 13 }}>Gross is up to {inr(lim)}: an employee who does not want PF must be a Retainer. Change the employment type to Retainer.</div>}
  </div>;
}

function AccomCard({ d, set, show, plain }: { d: Data; set: (k: string, v: string) => void; show?: boolean; plain?: boolean }) {
  const cl = useLoad(() => api('/hst/clusters'), []);
  const list: any[] = cl.data?.clusters ?? [];
  const yes = d.accom === 'Yes';
  const body = <div className="form">
    <label className="field"><span>Company accommodation</span>
      <select value={yes ? 'Yes' : 'No'} onChange={(e) => set('accom', e.target.value)}><option>No</option><option disabled={!list.length}>Yes</option></select></label>
    {yes && <label className={`field ${show && !d.accomCluster ? 'err' : ''}`}><span>Cluster<Req /></span>
      <select value={d.accomCluster ?? ''} onChange={(e) => set('accomCluster', e.target.value)}><option value="">Select</option>
        {list.map((c) => <option key={c.id} value={c.id}>{c.name}{c.city ? ` (${c.city})` : ''}: {c.free} of {c.beds} beds free</option>)}</select></label>}
  </div>;
  const note = <p className="mute" style={{ fontSize: 12, margin: '4px 0 0' }}>{list.length ? 'When they are marked joined, the nearest free bed in the cluster is allocated by itself from the joining date (a waiting list when none is free). Accommodation is outside payroll.' : 'No cluster has accommodation units yet (Accommodation › Units).'}</p>;
  return plain ? <div style={{ margin: '10px 0' }}>{body}{note}</div> : <div className="card"><h3>Company accommodation</h3>{body}{note}</div>;
}
