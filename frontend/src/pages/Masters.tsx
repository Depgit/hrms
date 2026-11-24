import { useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api, fmtD, today } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Modal, Pill, Switch, toast, useLoad } from '../components/ui';
import DriveStorage from './DriveStorage';
import Integrations from './Integrations';
import MasterUpdate from './MasterUpdate';
import PanVerify from './PanVerify';
import { BulkModal } from './Bulk';
import ShiftForm, { timingText } from './ShiftForm';

// One field of a master's add form (prototype MCFG[t].f). Location budgets are set in Compensation, Location budgets.
type FieldDef = { k: string; l: string; kind: 'text' | 'ref' | 'int' | 'bool' | 'time' | 'grades' | 'enum'; ref?: string; options?: string[] };
type Cfg = { label: string; right: 'mas.org' | 'mas.job'; fields: FieldDef[]; note?: string };

// Same tabs, order, labels and notes as the prototype's MCFG and vMas.
const TYPES: Record<string, Cfg> = {
  region: { label: 'Regions', right: 'mas.org', fields: [], note: 'Corporate → Region → State → City → Cluster → Location.' },
  state: { label: 'States', right: 'mas.org', fields: [{ k: 'region_id', l: 'Region', kind: 'ref', ref: 'region' }], note: 'Each state belongs to a region. Labour welfare fund is set by state.' },
  city: { label: 'Cities', right: 'mas.org', fields: [{ k: 'state_id', l: 'State', kind: 'ref', ref: 'state' }, { k: 'headcount_budget', l: 'Headcount budget', kind: 'int' }], note: 'Each city belongs to a state. City rules also feed payroll.' },
  cluster: { label: 'Clusters', right: 'mas.org', fields: [{ k: 'city_id', l: 'City', kind: 'ref', ref: 'city' }], note: 'A cluster is a group of 5 to 6 properties inside one city. Cluster heads approve at this level.' },
  location: { label: 'Locations', right: 'mas.org', fields: [{ k: 'cluster_id', l: 'Cluster', kind: 'ref', ref: 'cluster' }],
    note: 'Pick the cluster. City, state and region follow automatically, so every location sits in the full hierarchy.' },
  department: { label: 'Departments', right: 'mas.job', fields: [{ k: 'corporate', l: 'Corporate department (follows the corporate approval flow)', kind: 'bool' }] },
  designation: { label: 'Designations', right: 'mas.job', fields: [{ k: 'department_id', l: 'Department', kind: 'ref', ref: 'department' }, { k: 'grades', l: 'Grades this designation can be offered', kind: 'grades' }],
    note: 'Designations belong to a department and are mapped to the grades they can be offered. At offer time, picking the designation lists only those grades.' },
  grade: { label: 'Grades', right: 'mas.job', fields: [] },
  shift: { label: 'Shifts', right: 'mas.job', fields: [{ k: 'code', l: 'Code', kind: 'text' }, { k: 'start_time', l: 'Start', kind: 'time' }, { k: 'end_time', l: 'End', kind: 'time' }],
    note: 'A shift can run at all locations or only at selected ones. Forms and rosters show only shifts valid for the location. Each shift can set its own attendance rules from a date; a rule left empty follows the employee\'s work policy.' },
  qualification: { label: 'Qualifications', right: 'mas.job', fields: [] },
  // The three types are fixed: pay rules exist for Permanent, Retainer and Contractual only (spec 8.1, 13).
  employment_type: { label: 'Employment types', right: 'mas.job', fields: [], note: 'Employment types are fixed: Permanent, Retainer and Contractual, each with its own pay rules. They cannot be added, renamed or made inactive.' },
  attendance_mode: { label: 'Attendance modes', right: 'mas.job', fields: [] },
  // Job roles and skills (owner, 14 Nov 2026): what the employee does, not the access roles of Access control.
  job_role: { label: 'Roles', right: 'mas.job', fields: [], note: 'Job roles (front desk, kitchen, maintenance …). Each employee has one; new joiners need one on the offer. These are not the access roles in Access control.' },
  skill: { label: 'Skills', right: 'mas.job', fields: [], note: 'Skills or trades. Each employee has one; new joiners need one on the offer.' },
  change_reason: { label: 'Change reasons', right: 'mas.job', fields: [{ k: 'used_for', l: 'Used for', kind: 'enum', options: ['Employee update', 'Salary', 'Separation', 'Master data', 'Hold', 'Deduction'] }] },
};

// The local copy of the CMS's properties (GET /api/mdm/properties).
type CmsProps = { fromCms: boolean; properties: { propertyId: string; name: string; city: string | null; state: string | null; status: string; locationId: string | null; locationName: string | null }[] };

// The prototype's MCFG keys, accepted in ?tab= links (e.g. /masters?tab=desig from the Compensation setup guide).
const PROTO_KEYS: Record<string, string> = { loc: 'location', dept: 'department', desig: 'designation', qual: 'qualification', empType: 'employment_type', attMode: 'attendance_mode', chgReason: 'change_reason' };

export default function Masters() {
  const { me, can } = useAuth();
  const [qs] = useSearchParams();
  // Master update (one workbook for every master) comes first, for people who can change at least one master.
  const book = can('mas.org', 2) || can('mas.job', 2);
  const [t, setT] = useState(() => { const q = qs.get('tab') || ''; const k = PROTO_KEYS[q] ?? q; return TYPES[k] || (k === 'company' && me?.ctx.role === 'HR Admin') || (k === 'update' && book) ? k : 'city'; });
  // Company details (letters, payslips, statutory files) have no other home in the app: HR Admin only.
  const tabs: [string, string][] = [...(book ? [['update', 'Master update']] as [string, string][] : []), ...Object.entries(TYPES).map(([k, v]) => [k, v.label] as [string, string]),
    ...(me?.ctx.role === 'HR Admin' ? [['company', 'Company']] as [string, string][] : [])];
  return (
    <>
      <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${t === k ? 'on' : ''}`} onClick={() => setT(k)}>{l}</button>)}</div>
      {t === 'update' ? <MasterUpdate /> : t === 'company' ? <Company /> : <MasterList key={t} t={t} />}
    </>
  );
}

// Masters other records hold by name: Edit says what a rename updates (the server does it, in the same save).
const RENAMES: Record<string, string> = {
  grade: 'employee records, designations, pay bands, work policies and approval rules',
  department: 'work policy mappings, role mappings, onboarding documents and company policies',
  designation: 'work policy mappings and open offers',
  attendance_mode: 'employee records and open offers',
  qualification: 'open offers',
  job_role: 'employee records and open offers',
  skill: 'employee records and open offers',
};
// What follows a place that moves (a location to another cluster …).
const MOVES: Record<string, string> = { location: 'cluster, city, state and region', cluster: 'city, state and region', city: 'state and region', state: 'region' };
type EditState = { x: any; v: Record<string, any>; n: number; err: string };

/** Company details used on letters, payslips and statutory files, the offer letter template, and where uploaded
 *  documents are kept (Google Drive) (HR Admin). */
function Company() {
  const { data, error } = useLoad(() => api('/settings/company'), []);
  const [v, setV] = useState<any>(null);
  const [err, setErr] = useState('');
  const cur = v ?? data;
  if (error) return <div className="note bad">{error}</div>;
  if (!cur) return <Loading error={error} />;
  const set = (k: string, x: string) => setV({ ...cur, [k]: x });
  const save = async () => {
    setErr('');
    try { await api('/settings/company', { method: 'PUT', body: cur }); toast('Company details saved'); setV(null); } catch (x: any) { setErr(x.message); }
  };
  const F = (k: string, l: string) => <Field label={l}><input value={cur[k] ?? ''} onChange={(e) => set(k, e.target.value)} /></Field>;
  return <><div className="grid g2" style={{ alignItems: 'start' }}>
    <div className="card"><h3>Company details</h3>
      <p className="mute" style={{ fontSize: 12.5, marginTop: -6 }}>Shown on offer letters, payslips and statutory files. Placeholders until you fill them in.</p>
      <div className="form">{F('name', 'Short name')}{F('legalName', 'Legal name')}{F('pan', 'Company PAN')}{F('tan', 'TAN')}{F('gstin', 'GSTIN')}{F('pfCode', 'PF establishment code')}{F('esiCode', 'ESI employer code')}</div>
      <Field label="Registered address"><textarea rows={3} value={cur.address} onChange={(e) => set('address', e.target.value)} /></Field>
    </div>
    <div className="card"><h3>Offer letter template</h3>
      <p className="mute" style={{ fontSize: 12.5, marginTop: -6 }}>Merge fields: {'{{name}} {{designation}} {{department}} {{company}} {{location}} {{city}} {{grade}} {{doj}} {{gross}} {{fixed}} {{variable}} {{ctcYear}} {{net}} {{probation}}'}</p>
      <textarea rows={16} value={cur.offerTemplate} onChange={(e) => set('offerTemplate', e.target.value)} style={{ width: '100%', fontFamily: 'inherit' }} />
    </div>
    {err && <div className="note bad">{err}</div>}
    <div className="row"><button className="btn primary" onClick={save}>Save</button></div>
  </div>
    <DriveStorage />
    <PanVerify />
    <Integrations />
  </>;
}

function MasterList({ t }: { t: string }) {
  const { can } = useAuth();
  const cfg = TYPES[t];
  const edit = can(cfg.right, 2);
  const { data, error, reload } = useLoad(() => api<any[]>(`/masters/${t}`), [t]);
  // Lists the details column and the add form need (names up the hierarchy, counts below it).
  const need = [...new Set([...cfg.fields.filter((f) => f.ref).map((f) => f.ref!),
    ...({ location: ['state'], cluster: ['city', 'location'], city: ['state', 'cluster'], department: ['designation'], shift: ['location', 'department'], designation: ['grade'] } as Record<string, string[]>)[t] ?? []])];
  const refs = useLoad(async () => {
    const out: Record<string, any[]> = {};
    for (const k of need) out[k] = await api(`/masters/${k}`);
    return out;
  }, [t]);
  const reasons = useLoad(() => api<any[]>('/masters/change_reason?status=Active'), []);
  // Locations: the CMS's properties (master data SOP: the CMS owns property_id). A location linked to one shows
  // property_id and cmsLocked (its name and status come from the CMS); with "Properties come from the CMS" on, a
  // location is added by picking its property.
  const props = useLoad(() => (t === 'location' ? api<CmsProps>('/mdm/properties').catch(() => null) : Promise.resolve(null)), [t]);
  const [link, setLink] = useState<{ x: any; pid: string; err: string } | null>(null);
  const fromCms = t === 'location' && !!props.data?.fromCms;
  const unlinked = (props.data?.properties || []).filter((p) => !p.locationId);
  const propName = (pid: string) => props.data?.properties.find((p) => p.propertyId === pid)?.name;
  const [form, setForm] = useState<Record<string, any>>({});
  const [retire, setRetire] = useState<{ x: any; since: string; reason: string; n?: number } | null>(null);
  const [ed, setEd] = useState<EditState | null>(null);
  const [shForm, setShForm] = useState<string | null>(null); // Set shift: a shift id, or 'new'
  const [shBulk, setShBulk] = useState(false); // Bulk upload of the shifts' Department + Location rows, on this page
  const R = refs.data || {};
  const act = (k: string) => (R[k] || []).filter((x) => x.status === 'Active');
  const mreasons = (reasons.data || []).filter((r) => r.used_for === 'Master data').map((r) => r.name as string);
  const fail = (x: any) => toast(x.message);

  // A select defaults to its first option, as in the prototype (no blank choice).
  const val = (f: FieldDef) => form[f.k] ?? (f.kind === 'ref' ? act(f.ref!)[0]?.id ?? '' : f.kind === 'bool' ? false : f.kind === 'enum' ? f.options![0] : f.kind === 'grades' ? [] : '');
  const add = async () => {
    const pid = fromCms ? String(form.property_id || '') : '';
    if (fromCms && !pid) { toast('Pick the CMS property'); return; }
    const name = fromCms ? propName(pid) || '' : String(form.name || '').trim();
    if (!name) { toast('Enter a name'); return; }
    const body: Record<string, any> = { name, ...(pid ? { property_id: pid } : {}) };
    for (const f of cfg.fields) {
      const v = val(f);
      if (f.kind === 'ref' && !v) { toast('Pick a ' + f.l.toLowerCase()); return; }
      if (f.kind === 'grades' && !v.length) { toast('Pick at least one grade'); return; }
      if (v === '' || v == null) continue;
      body[f.k] = v;
    }
    try { await api(`/masters/${t}`, { body }); toast('Added'); setForm({}); reload(); refs.reload(); props.reload(); } catch (x: any) { fail(x); }
  };
  const doLink = async () => {
    const l = link!;
    if (!l.pid) { setLink({ ...l, err: 'Pick the CMS property' }); return; }
    try {
      const r = await api<{ notes: string[] }>(`/mdm/locations/${l.x.id}/link`, { body: { propertyId: l.pid } });
      toast(['Linked to CMS property ' + l.pid, ...(r.notes || [])].join('. ')); setLink(null); reload(); props.reload();
    } catch (x: any) { setLink({ ...l, err: x.message }); }
  };
  const patch = async (id: string, body: Record<string, any>, done?: () => void) => {
    try { await api(`/masters/${t}/${id}`, { method: 'PATCH', body }); toast('Saved'); done?.(); reload(); } catch (x: any) { fail(x); }
  };
  const doRetire = async () => {
    const r = retire!;
    if (r.since < today()) { toast('Inactive from cannot be a past date'); return; }
    try {
      const res = await api(`/masters/${t}/${r.x.id}/inactive`, { body: { since: r.since, reason: r.reason } });
      const n = res?.activeEmployeesUsingIt || 0;
      toast((r.since <= today() ? 'Marked inactive' : 'Scheduled to go inactive on ' + fmtD(r.since)) + (n ? `. ${n} active employee${n > 1 ? 's' : ''} still use${n > 1 ? '' : 's'} it. They are not changed` : ''));
      setRetire(null); reload();
    } catch (x: any) { fail(x); }
  };
  const tog = async (x: any) => {
    if (x.status === 'Active') {
      // Prototype masterImpact: the active employees still using it, shown before confirming.
      const n = await api(`/masters/${t}/${x.id}/impact`).then((r) => +(r?.n || 0)).catch(() => 0);
      setRetire({ x, since: today(), reason: mreasons[0] || '', n });
      return;
    }
    try { await api(`/masters/${t}/${x.id}/reactivate`, { method: 'POST' }); toast('Reactivated'); reload(); } catch (e: any) { fail(e); }
  };

  const region = (state: string) => R.state?.find((s) => s.name === state)?.region ?? '—';
  const detail = (x: any): string => {
    if (t === 'location') return `${x.cluster} › ${x.city} › ${x.state} › ${region(x.state)}`;
    if (t === 'cluster') { const n = (R.location || []).filter((l) => l.cluster_id === x.id && l.status === 'Active').length; return `${x.city} › ${R.city?.find((c) => c.id === x.city_id)?.state ?? '—'} · ${n} active propert${n === 1 ? 'y' : 'ies'}`; }
    if (t === 'city') return `${x.state} › ${region(x.state)} · ${(R.cluster || []).filter((c) => c.city_id === x.id && c.status === 'Active').length} clusters · budget ${x.headcount_budget || 0}`;
    if (t === 'designation') return `Department: ${x.department} · Grades: ${x.grades?.join(', ') || 'none mapped'}`;
    if (t === 'department') return `${(R.designation || []).filter((d) => d.department_id === x.id && d.status === 'Active').length} active designations`;
    if (t === 'shift') return `${timingText(x.start_time, x.end_time, x.start2_time, x.end2_time)}${x.break_minutes ? ` · ${x.break_minutes} min break` : ''} · Applies to: ${(x.applies || []).join('; ') || 'nobody (no row switched on)'}`;
    if (t === 'state') return `Region: ${x.region}`;
    if (t === 'change_reason') return x.used_for ? `Used for: ${x.used_for}` : '';
    return '';
  };
  const btnCol = edit || t === 'shift';

  // Edit: the Add form's fields, prefilled. A select lists active items plus the current one, even if inactive.
  const openEdit = async (x: any) => {
    const v: Record<string, any> = { name: x.name };
    for (const f of cfg.fields) v[f.k] = f.kind === 'grades' ? [...(x.grades || [])] : f.kind === 'bool' ? !!x[f.k] : x[f.k] ?? '';
    setEd({ x, v, n: 0, err: '' });
    if (MOVES[t] || RENAMES[t]) {
      const k = await api(`/masters/${t}/${x.id}/impact`).then((r) => +(r?.n || 0)).catch(() => 0);
      setEd((e) => (e && e.x.id === x.id ? { ...e, n: k } : e));
    }
  };
  const fixedWhy = (x: any): string => x.fixed || (t === 'employment_type' ? "An employment type's name cannot change: pay rules and employee records use it." : '');
  const optName = (list: any[], o: any) => list.filter((i) => i.name === o.name).length > 1 ? `${o.name} (${o.state || o.city || o.department || o.region || ''})` : o.name;
  const saveEdit = async () => {
    const e = ed!;
    const body: Record<string, any> = {};
    const name = String(e.v.name || '').trim();
    if (!name) { setEd({ ...e, err: 'Enter a name' }); return; }
    if (name !== e.x.name) body.name = name;
    for (const f of cfg.fields) {
      const a = e.v[f.k], b = e.x[f.k];
      if (f.kind === 'grades') {
        if (!a.length) { setEd({ ...e, err: 'Pick at least one grade' }); return; }
        if ([...a].sort().join('|') !== [...(e.x.grades || [])].sort().join('|')) body.grades = a;
      } else if (f.kind === 'int') { const nv = a === '' || a == null ? 0 : +a; if (nv !== (+b || 0)) body[f.k] = nv; }
      else if (f.kind === 'bool') { if (!!a !== !!b) body[f.k] = !!a; }
      else if (a !== (b ?? '')) body[f.k] = a;
    }
    if (!Object.keys(body).length) { setEd(null); toast('No changes'); return; }
    try {
      const r = await api(`/masters/${t}/${e.x.id}`, { method: 'PATCH', body });
      const m = +(r?.activeEmployeesMoved || 0);
      toast('Saved' + (m ? `. ${m} active employee${m > 1 ? 's' : ''} moved with it` : ''));
      setEd(null); reload(); refs.reload();
    } catch (x: any) { setEd({ ...e, err: x.message }); }
  };
  // Change reasons read grouped by what they are used for, in the prototype's order.
  const rows = t === 'change_reason' && data ? [...data].sort((a, b) => TYPES.change_reason.fields[0].options!.indexOf(a.used_for) - TYPES.change_reason.fields[0].options!.indexOf(b.used_for)) : data;

  if (t === 'shift' && shForm) {
    return <ShiftForm id={shForm === 'new' ? null : shForm} locs={act('location')} depts={act('department')} edit={edit} onBack={(changed) => { setShForm(null); if (changed) reload(); }} />;
  }

  return (
    <>
      {cfg.note && <div className="note" style={{ marginBottom: 14 }}>{cfg.note}</div>}
      {edit && t === 'shift' && <div className="row" style={{ marginBottom: 14 }}><button className="btn primary" onClick={() => setShForm('new')}>Add shift</button>
        <button className="btn" onClick={() => setShBulk(true)}>Bulk upload</button>
        <span className="mute" style={{ fontSize: 12 }}>Add shift opens Set shift: timings and where it applies. Bulk upload changes where shifts apply (Department + Location rows). The work policy decides the attendance rules.</span></div>}
      {shBulk && <BulkModal tpl="shiftmap" title="Bulk upload: shift mapping" onClose={() => setShBulk(false)} onApplied={reload} />}
      {edit && t !== 'shift' && t !== 'employment_type' && <div className="card" style={{ marginBottom: 14 }}>
        <h3>Add to {cfg.label.toLowerCase()}</h3>
        <div className="form">
          {fromCms ? <Field label="CMS property"><select value={form.property_id || ''} onChange={(e) => setForm({ ...form, property_id: e.target.value })}>
            <option value="">Pick a property</option>{unlinked.filter((p) => p.status === 'Active').map((p) => <option key={p.propertyId} value={p.propertyId}>{p.name} ({p.propertyId}{p.city ? `, ${p.city}` : ''})</option>)}</select></Field>
            : <Field label="Name"><input type="text" value={form.name || ''} onChange={(e) => setForm({ ...form, name: e.target.value })} /></Field>}
          {cfg.fields.map((f) => {
            const v = val(f), set = (x: any) => setForm({ ...form, [f.k]: x });
            if (f.kind === 'grades') return <div key={f.k} className="field" style={{ gridColumn: '1/-1' }}><span>{f.l}</span><div className="chkgrp">{act('grade').map((g) => <label key={g.id} className="chkl"><input type="checkbox" checked={v.includes(g.name)} onChange={(e) => set(e.target.checked ? [...v, g.name] : v.filter((y: string) => y !== g.name))} />{g.name}</label>)}</div></div>;
            return <Field key={f.k} label={f.l}>
              {f.kind === 'ref' ? <select value={v} onChange={(e) => set(e.target.value)}>{act(f.ref!).map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select>
                : f.kind === 'bool' ? <select value={String(v)} onChange={(e) => set(e.target.value === 'true')}><option value="false">No</option><option value="true">Yes</option></select>
                : f.kind === 'enum' ? <select value={v} onChange={(e) => set(e.target.value)}>{f.options!.map((o) => <option key={o}>{o}</option>)}</select>
                : <input type={f.kind === 'int' ? 'number' : f.kind === 'time' ? 'time' : 'text'} value={v} onChange={(e) => set(f.kind === 'int' ? (e.target.value === '' ? '' : +e.target.value) : e.target.value)} />}
            </Field>;
          })}
        </div>
        {fromCms && <div className="note" style={{ marginTop: 10 }}>Properties come from the CMS: the location takes the property's name, and its name and status follow the CMS from then on.</div>}
        <div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={add}>Add</button><span className="mute" style={{ fontSize: 12 }}>Inactive items stay in history but do not appear in forms.</span></div>
      </div>}
      <div className="card">
        {!data ? <Loading error={error} /> : <div className="tw"><table>
          <thead><tr><th>Name</th>{t === 'shift' && <th>Code</th>}<th>Details</th>{t === 'location' && <th>CMS property</th>}{t === 'shift' && <th>Effective from</th>}{t === 'department' && <th>Approval flow</th>}<th>Added</th><th>Status</th>{edit && <th>Active</th>}{btnCol && <th></th>}</tr></thead>
          <tbody>{rows!.map((x) => <tr key={x.id}>
            <td><b>{x.name}</b></td>
            {t === 'shift' && <td className="mono">{x.code}</td>}
            <td className="ink2">{detail(x)}</td>
            {t === 'location' && <td>{x.property_id
              ? <span title={x.cmsLocked || ''}><span className="mono">{x.property_id}</span> <Pill k="info">Linked</Pill>
                {x.cmsLocked && <div className="mute" style={{ fontSize: 11.5 }}>Name and status from the CMS</div>}</span>
              : edit && unlinked.length > 0 ? <button className="btn sm" onClick={() => setLink({ x, pid: '', err: '' })}>Link</button> : <span className="mute">—</span>}</td>}
            {t === 'shift' && <td className="mute" style={{ whiteSpace: 'nowrap' }}>{!x.effective_from || x.effective_from === '2000-01-01' ? 'From the start' : fmtD(x.effective_from)}
              {x.next_from && <div style={{ fontSize: 11.5 }}><Pill k="warn">Next from {fmtD(x.next_from)}</Pill></div>}</td>}
            {t === 'department' && <td>{edit
              ? <select value={x.corporate ? 'Yes' : 'No'} onChange={(e) => patch(x.id, { corporate: e.target.value === 'Yes' })}><option value="No">Operations flow</option><option value="Yes">Corporate flow</option></select>
              : x.corporate ? 'Corporate flow' : 'Operations flow'}</td>}
            <td className="mute">{x.created_at ? fmtD(x.created_at) : '—'}</td>
            <td><Pill k={x.status === 'Active' ? 'ok' : 'n'}>{x.status}</Pill>{x.inactive_since && <div className="mute" style={{ fontSize: 11.5 }}>{x.status === 'Active' ? 'Inactive from ' : 'Inactive since '}{fmtD(x.inactive_since)}{x.inactive_reason ? ` · ${x.inactive_reason}` : ''}</div>}</td>
            {edit && <td><Switch on={x.status === 'Active'} onChange={() => tog(x)} /></td>}
            {btnCol && <td>
              {t === 'shift' ? <button className="btn sm" onClick={() => setShForm(x.id)}>{edit ? 'Edit' : 'View'}</button>
                : <button className="btn sm" onClick={() => openEdit(x)}>Edit</button>}
            </td>}
          </tr>)}</tbody>
        </table></div>}
        <p className="mute" style={{ fontSize: 12, margin: '12px 0 0' }}>There is no delete. Switch an item off to retire it.</p>
      </div>

      {retire && <Modal onClose={() => setRetire(null)}>
        <h2>Mark “{retire.x.name}” inactive?</h2>
        <p className="ink2">Existing records keep this value. It will not appear in new forms. It cannot be deleted.</p>
        <div className="grid g2" style={{ margin: '10px 0' }}>
          <Field label="Inactive from"><input type="date" min={today()} value={retire.since} onChange={(e) => setRetire({ ...retire, since: e.target.value })} /></Field>
          <Field label="Reason"><select value={retire.reason} onChange={(e) => setRetire({ ...retire, reason: e.target.value })}>{mreasons.map((r) => <option key={r}>{r}</option>)}</select></Field>
        </div>
        {!!retire.n && <div className="note" style={{ margin: '10px 0' }}>{retire.n} active employee{retire.n > 1 ? 's' : ''} still use{retire.n > 1 ? '' : 's'} it. They are not changed. Move them with Bulk update when you are ready.</div>}
        <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={doRetire}>Mark inactive</button><button className="btn" onClick={() => setRetire(null)}>Cancel</button></div>
      </Modal>}

      {link && <Modal onClose={() => setLink(null)}>
        <h2>Link “{link.x.name}” to its CMS property</h2>
        <p className="ink2">A link is set once and cannot change. From then on the location's name follows the CMS, and it is retired when the CMS deactivates the property. Employee events carry the property_id.</p>
        <Field label="CMS property"><select value={link.pid} onChange={(e) => setLink({ ...link, pid: e.target.value, err: '' })}>
          <option value="">Pick a property</option>{unlinked.map((p) => <option key={p.propertyId} value={p.propertyId}>{p.name} ({p.propertyId}{p.city ? `, ${p.city}` : ''}){p.status !== 'Active' ? ' (inactive in the CMS)' : ''}</option>)}</select></Field>
        {link.pid && propName(link.pid) !== link.x.name && <div className="note warn" style={{ margin: '10px 0' }}>The location will be renamed “{propName(link.pid)}”, as in the CMS.</div>}
        {link.err && <div className="note bad" style={{ margin: '10px 0' }}>{link.err}</div>}
        <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={doLink}>Link</button><button className="btn" onClick={() => setLink(null)}>Cancel</button></div>
      </Modal>}

      {ed && (() => {
        const e = ed, fixed = fixedWhy(e.x);
        const set = (k: string, v: any) => setEd({ ...e, v: { ...e.v, [k]: v }, err: '' });
        const pf = cfg.fields.find((f) => f.kind === 'ref');
        const moved = !!pf && e.v[pf.k] !== e.x[pf.k];
        const to = moved ? (R[pf!.ref!] || []).find((i) => i.id === e.v[pf!.k]) : null;
        const renamed = String(e.v.name || '').trim() !== e.x.name;
        const blocked = moved && t === 'designation' && e.n > 0;
        return <Modal onClose={() => setEd(null)}>
          <h2>Edit “{e.x.name}”</h2>
          <div className="form" style={{ margin: '10px 0' }}>
            <Field label="Name"><input type="text" value={e.v.name} disabled={!!fixed || !!e.x.cmsLocked} onChange={(ev) => set('name', ev.target.value)} /></Field>
            {cfg.fields.map((f) => {
              const v = e.v[f.k];
              if (f.kind === 'grades') {
                const gs = (R.grade || []).filter((g) => g.status === 'Active' || (e.x.grades || []).includes(g.name));
                return <div key={f.k} className="field" style={{ gridColumn: '1/-1' }}><span>{f.l}</span><div className="chkgrp">{gs.map((g) => <label key={g.id} className="chkl">
                  <input type="checkbox" checked={v.includes(g.name)} onChange={(ev) => set(f.k, ev.target.checked ? [...v, g.name] : v.filter((y: string) => y !== g.name))} />{g.name}{g.status !== 'Active' ? ' (inactive)' : ''}</label>)}</div></div>;
              }
              const label = t === 'department' && f.k === 'corporate' ? 'Approval flow' : f.l;
              return <Field key={f.k} label={label}>
                {f.kind === 'ref' ? (() => { const all = R[f.ref!] || [], l = all.filter((o) => o.status === 'Active' || o.id === e.x[f.k]);
                  return <select value={v} onChange={(ev) => set(f.k, ev.target.value)}>{l.map((o) => <option key={o.id} value={o.id}>{optName(all, o)}{o.status !== 'Active' ? ' (inactive)' : ''}</option>)}</select>; })()
                  : f.kind === 'bool' ? <select value={String(!!v)} onChange={(ev) => set(f.k, ev.target.value === 'true')}>{t === 'department'
                    ? <><option value="false">Operations flow</option><option value="true">Corporate flow</option></> : <><option value="false">No</option><option value="true">Yes</option></>}</select>
                  : f.kind === 'enum' ? <select value={v} disabled={!!fixed || !!e.x.appUse} onChange={(ev) => set(f.k, ev.target.value)}>{f.options!.map((o) => <option key={o}>{o}</option>)}</select>
                  : <input type={f.kind === 'int' ? 'number' : 'text'} value={v ?? ''} onChange={(ev) => set(f.k, f.kind === 'int' ? (ev.target.value === '' ? '' : +ev.target.value) : ev.target.value)} />}
              </Field>;
            })}
          </div>
          {fixed && <div className="note" style={{ margin: '10px 0' }}>{fixed}</div>}
          {e.x.cmsLocked && <div className="note" style={{ margin: '10px 0' }}>{e.x.cmsLocked}.</div>}
          {!fixed && e.x.appUse && <div className="note" style={{ margin: '10px 0' }}>{e.x.appUse}.</div>}
          {moved && MOVES[t] && <div className="note warn" style={{ margin: '10px 0' }}>Moving “{e.x.name}” to {to?.name} changes the {MOVES[t]} of {e.n ? <b>its {e.n} active employee{e.n > 1 ? 's' : ''}</b> : 'everyone at it'} where they differ. They move with it.</div>}
          {blocked && <div className="note bad" style={{ margin: '10px 0' }}>{e.n} active employee{e.n > 1 ? 's' : ''} hold this designation in {e.x.department}. Move them first with Bulk update, or add a new designation.</div>}
          {renamed && !fixed && RENAMES[t] && <div className="note" style={{ margin: '10px 0' }}>Renaming also updates the {RENAMES[t]} that use “{e.x.name}”{e.n && (t === 'grade' || t === 'attendance_mode') ? `, including ${e.n} active employee${e.n > 1 ? 's' : ''}` : ''}.</div>}
          {e.err && <div className="note bad" style={{ margin: '10px 0' }}>{e.err}</div>}
          <div className="row" style={{ marginTop: 16 }}><button className="btn primary" disabled={blocked || (!!fixed && cfg.fields.length === 0)} onClick={saveEdit}>Save</button><button className="btn" onClick={() => setEd(null)}>Cancel</button></div>
        </Modal>;
      })()}


    </>
  );
}
