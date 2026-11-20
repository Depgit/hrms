import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api } from '../api';
import { useAuth } from '../auth';
import PersonPicker, { type Person } from '../components/PersonPicker';
import { Adv, Modal, Pill, Switch, Tabs, toast, useLoad } from '../components/ui';
import { AREA_LABEL, EffectiveRights, PersonalRights } from '../components/AccessCards';

// Same order as the prototype's role list (ROLEDEF); roles added later follow.
const ROLE_ORDER = ['HR Admin', 'HRBP', 'Line Manager', 'Property Manager', 'Cluster Manager', 'City Head', 'Operations Head', 'Function Head', 'HRBP Corporate',
  'Payroll', 'Payroll Manager', 'CHRO', 'Finance Head', 'Finance', 'MIS Executive', 'Employee'];
const byRoleOrder = <T,>(l: T[], name: (x: T) => string) => {
  const i = (x: T) => { const k = ROLE_ORDER.indexOf(name(x)); return k < 0 ? ROLE_ORDER.length : k; };
  return l.map((x, n) => [x, n] as const).sort((a, b) => i(a[0]) - i(b[0]) || a[1] - b[1]).map(([x]) => x);
};
// Whose records each built-in role normally sees (prototype "Sees employees").
const SCOPE: Record<string, string> = { 'HR Admin': 'All', HRBP: 'Cluster', 'Line Manager': 'Team', 'Property Manager': 'Location', 'Cluster Manager': 'Cluster', 'City Head': 'City',
  'Operations Head': 'Region', 'Function Head': 'Department', 'HRBP Corporate': 'All', Payroll: 'All', 'Payroll Manager': 'All', CHRO: 'All', 'Finance Head': 'All', Finance: 'All', 'MIS Executive': 'All' };
const RM_HIDE = ['Employee', 'Line Manager'];

const RGROUPS: [string, string][] = [['dash', 'Dashboard'], ['emp', 'Employees'], ['onb', 'Onboarding'], ['comp', 'Compensation'], ['appr', 'Approvals'], ['att', 'Attendance'], ['ros', 'Shift and roster'],
  ['pay', 'Payroll'], ['pol', 'Work policies'], ['mas', 'Masters'], ['hst', 'Accommodation'], ['cost', 'Cost and budget'], ['bulk', 'Bulk update'], ['acc', 'Access control'], ['rep', 'Reports'], ['ess', 'My pay and profile']];
const V12 = [0, 1, 2], E02 = [0, 2], V01 = [0, 1];
// Prototype RIGHTS: [id, label, hint, allowed levels, confidential].
const RIGHTS: [string, string, string, number[], boolean?][] = [
  ['dash.overview', 'Summary cards and charts', 'Headcount, requests and city split for the data the user can access', V01],
  ['emp.directory', 'Employee directory and job details', 'Name, ID, type, department, designation, location, shift, reporting manager', V12],
  ['emp.personal', 'Personal details', 'Date of birth, mobile, email, address, emergency contact', V12, true],
  ['emp.govid', 'Aadhaar and PAN', '', V12, true],
  ['emp.bank', 'Bank details', '', V12, true],
  ['emp.salary', 'Salary and CTC', 'Gross, CTC, net pay and salary structure of an employee', V12, true],
  ['emp.medical', 'Medical and nominations', '', V12, true],
  ['emp.status', 'Mark inactive or reactivate', '', E02],
  ['emp.export', 'Export employee data', '', E02],
  ['onb.pipeline', 'Offers and joining details', 'View the pipeline. Edit to collect and update joining details', V12],
  ['onb.raise', 'Raise a new offer', '', E02],
  ['onb.setup', 'Onboarding form and document setup', '', E02],
  ['comp.bands', 'Pay bands and offer checks', '', V12, true],
  ['comp.budget', 'Budgets by property', 'Headcount and payroll budgets for the properties the user can access', V12],
  ['comp.structure', 'Salary structures, LWF and structure mapping', '', V12, true],
  ['comp.approve', 'Publish pay changes', '', E02, true],
  ['appr.queue', 'Requests and approvals', 'View requests. Edit to approve or reject', V12],
  ['appr.raise', 'Raise leave, regularization, transfer and resignation', '', E02],
  ['appr.setup', 'Approval flow and approver names', '', E02],
  ['att.records', 'Attendance records and upload', 'View records. Edit to upload attendance data', V12],
  ['att.setup', 'Punch machine setup', '', E02],
  ['att.portal', 'Realtime portal: send employees', 'View who is waiting to be sent. Edit to send new joiners, changes and exits to the Realtime portal (each send is previewed and confirmed)', V12],
  ['rep.reports', 'Reports and downloads', 'View reports. Edit to download report files, statutory and insurance files', V12],
  ['ros.roster', 'Shifts and rosters', '', V12],
  ['ros.publish', 'Approve and publish rosters', '', E02],
  ['pay.run', 'Payroll calculator and monthly run', '', V12, true],
  ['pay.approve', 'Approve payroll', '', E02, true],
  ['pay.setup', 'Payroll settings', '', E02, true],
  ['pay.export', 'Export payroll files', '', E02, true],
  ['pol.policies', 'Work policies and leave rules', '', V12],
  ['mas.org', 'Masters: regions, states, cities, clusters, locations', '', V12],
  ['mas.job', 'Masters: departments, designations, grades, shifts and others', '', V12],
  ['bulk.update', 'Bulk update templates', '', V12],
  ['emp.assets', 'Employee dues', 'Assets, uniform and salary advances: issue them, set items and kits, and schedule recoveries', V12],
  ['pay.claims', 'Claims, arrears and profile changes', 'View. Edit to approve claims, verify bank changes and add arrears', V12, true],
  ['ess.me', 'My pay and profile', 'Own payslips, tax declaration, claims and detail changes', [0, 2]],
  ['acc.roles', 'Roles and rights', '', V12],
  ['acc.users', 'Users and data access', '', V12],
  ['emp.sep', 'Separation', 'View to follow exits and sign your part of the clearance. Edit to start exits, send letters and close cases', V12],
  ['hst.setup', 'Accommodation: units, leases and costs', 'View the units, leases, bills and month cost split. Edit to add and change them', V12],
  ['hst.allot', 'Accommodation: check-in and check-out', 'View who stays where. Edit to assign accommodation and check people in and out', V12],
  ['cost.view', 'Cost and budget', 'The month’s cost by place (salary, contributions, accommodation) against the budgets, and the headcount plan', V01, true],
  ['cost.budget', 'Cost and budget: budgets', 'Edit the monthly cost and headcount budgets of properties and clusters', E02],
];
const LVLN2: Record<number, string> = { 0: 'No access', 1: 'View', 2: 'Edit' };
const isE02 = (allowed: number[]) => allowed.length === 2 && allowed[0] === 0 && allowed[1] === 2;
// A view-only right stored above View still reads as View; any other level outside the allowed list is shown as it is.
const lvShown = (allowed: number[], cur: number) => (!allowed.includes(2) && cur > Math.max(...allowed) ? Math.max(...allowed) : cur);
const lvOpts = (allowed: number[], cur: number) => {
  const l = allowed.includes(cur) ? allowed : [...allowed, cur].sort();
  return l.map((v) => [v, v === 2 && isE02(allowed) ? 'Allowed' : LVLN2[v]] as [number, string]);
};
const PRIV: [string, string][] = [['emp.salary', 'Salary and CTC'], ['emp.govid', 'Aadhaar and PAN'], ['emp.bank', 'Bank details'], ['emp.personal', 'Personal details'], ['emp.medical', 'Medical and nominations'], ['pay.run', 'Payroll run']];
const lvPill = (v: number) => (v >= 2 ? <Pill k="bad">Edit</Pill> : v === 1 ? <Pill k="warn">View</Pill> : <Pill k="n">Hidden</Pill>);
const SENS: [string, string][] = [['salary', 'Salary and CTC'], ['govId', 'Aadhaar and PAN'], ['bank', 'Bank details'], ['medical', 'Medical and nominations']];
const chips = (l: string[]) => (l.length ? l.map((x, i) => <span key={x}>{i > 0 && ' '}<Pill k="info">{x}</Pill></span>) : <span className="mute">Nothing</span>);
const more3 = (l: string[]) => (l.length > 3 ? l.slice(0, 3).join(', ') + ' and ' + (l.length - 3) + ' more' : l.join(', '));

type T = 'roles' | 'users' | 'rights' | 'check';

export default function Access() {
  const { can } = useAuth();
  const [qp, setQp] = useSearchParams();
  const seeR = can('acc.roles'), seeU = can('acc.users');
  let t = (qp.get('tab') as T | null) || 'roles';
  if (t === 'roles' && !seeR && seeU) t = 'users';
  if (t === 'users' && !seeU && seeR) t = 'roles';
  const tabs: [T, string][] = [...(seeR ? [['roles', 'Roles and rights']] as [T, string][] : []), ...(seeU ? [['users', 'Role mapping'], ['rights', 'Personal rights']] as [T, string][] : []), ...(seeR || seeU ? [['check', 'Check access']] as [T, string][] : [])];
  return (
    <>
      <Tabs tabs={tabs} value={t} onChange={(x) => setQp({ tab: x })} />
      {t === 'users' && <RoleMap />}{t === 'roles' && <Roles />}{t === 'rights' && <PersonalRights />}{t === 'check' && <CheckAccess />}
    </>
  );
}

// ---------- Role mapping ----------

type MapRow = { id: string; employee_id: string; name: string; role_name: string; access: string; location_ids: string[]; departments: string[]; area_ids?: string[]; active: boolean; department?: string; city?: string; email?: string | null; created_at?: string };
type MapDraft = { id?: string; employeeId: string; name: string; dept: string; city: string; role: string; access: string; locationIds: string[]; departments: string[]; areaIds: string[] };
const AREAS = ['states', 'cities', 'clusters'];
/** The locations inside the clusters, cities or states of a mapping, as they are now (the server works it out the same way). */
const areaLocs = (access: string, ids: string[], locs: any[], cities: any[]) => locs.filter((l) => access === 'clusters' ? ids.includes(l.cluster_id)
  : access === 'cities' ? ids.includes(l.city_id) : access === 'states' ? ids.includes(cities.find((c) => c.id === l.city_id)?.state_id) : false).map((l) => l.id as string);

// Prototype accessText: whose records a mapping covers.
const accessText = (access: string, locs: string[], depts: string[]) => {
  const ds = depts.length ? ' · ' + depts.join(', ') + ' only' : '';
  if (access === 'all') return 'All locations' + ds;
  if (access === 'team') return 'Own team' + ds;
  if (access === 'self') return 'Self only';
  if (AREAS.includes(access)) return AREA_LABEL[access] + ': ' + (locs.length ? locs.join(', ') : 'none picked') + ds;
  return locs.length ? (locs.length > 3 ? locs.length + ' locations: ' + locs.slice(0, 3).join(', ') + '…' : locs.join(', ')) : 'No locations picked';
};
// One holder per place (same rule as the server): the places or departments a mapping holds its role for.
const LOC_ROLES = ['Property Manager', 'HRBP', 'Cluster Manager', 'HRBP Corporate', 'Operations Head', 'City Head'];
const CO_ROLES = ['CHRO', 'Payroll', 'Payroll Manager', 'Finance Head'];
function mapUnits(role: string, access: string, locs: string[], depts: string[], allLocs: string[], allDepts: string[]): string[] {
  const dp = depts.length > 0;
  if (LOC_ROLES.includes(role) && !(role === 'HRBP' && dp)) return (access === 'all' ? allLocs : locs).map((id) => 'loc|' + id);
  if (role === 'Function Head') return (dp ? depts : allDepts).map((n) => 'dept|' + n);
  if (role === 'HRBP') return depts.map((n) => 'hdept|' + n);
  if (CO_ROLES.includes(role)) return ['co|' + role];
  return [];
}

function RoleMap() {
  const { can } = useAuth();
  const canE = can('acc.users', 2);
  const { data, reload } = useLoad(() => api<MapRow[]>('/role-map'), []);
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const clusters = useLoad(() => api<any[]>('/masters/cluster?status=Active'), []);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const states = useLoad(() => api<any[]>('/masters/state?status=Active'), []);
  const regions = useLoad(() => api<any[]>('/masters/region?status=Active'), []);
  const depts = useLoad(() => api<any[]>('/masters/department?status=Active'), []);
  const roles = useLoad(() => (can('acc.roles') ? api<any[]>('/roles').then((l) => byRoleOrder(l.filter((r) => r.active && !RM_HIDE.includes(r.name)), (r) => r.name).map((r) => r.name as string)) : Promise.resolve(ROLE_ORDER.filter((r) => !RM_HIDE.includes(r)))), []);
  const dir = useLoad(() => (can('appr.queue') ? api<any>('/approvals/approvers') : Promise.resolve(null)), [data]);
  const [pe, setPe] = useState<Person | null>(null);
  const [m, setM] = useState<MapDraft | null>(null);
  const locName = (id: string) => locs.data?.find((l) => l.id === id)?.name ?? id;
  const areaName = (id: string) => [...(clusters.data || []), ...(cities.data || []), ...(states.data || [])].find((x) => x.id === id)?.name ?? id;
  // Where each mapping is the named approver, from the same directory the approval engine uses (prototype rmCovers).
  const covers = (r: MapRow) => {
    const d = dir.data, out: string[] = [];
    if (!d || !r.active) return out;
    d.locations.forEach((l: any) => { if (l.people[r.role_name]?.id === r.employee_id) out.push(l.name); });
    d.departments.forEach((x: any) => { if (x.people[r.role_name]?.id === r.employee_id) out.push(r.role_name === 'HRBP' ? x.name + ' (corporate)' : x.name); });
    if (d.company[r.role_name]?.id === r.employee_id) out.push('Whole company');
    const at = (x: string) => { const i = r.location_ids.map(locName).indexOf(x); return i < 0 ? r.location_ids.length : i; };
    return out.map((x, n) => [x, n] as const).sort((a, b) => at(a[0]) - at(b[0]) || a[1] - b[1]).map(([x]) => x);
  };
  const toggle = async (row: MapRow) => {
    try { await api(`/role-map/${row.id}`, { method: 'PATCH', body: { active: !row.active } }); reload(); } catch (x: any) { toast(x.message); }
  };
  const gaps: string[] = dir.data?.gaps ?? [];
  const overlaps: string[] = dir.data?.overlaps ?? [];
  return (
    <>
      <p className="ink2" style={{ margin: '0 0 14px', fontSize: 13.5 }}>Everyone is an employee by default, and a Line Manager comes from the reporting manager on the employee record. Map only the other roles here. A role says what the person can do, and the places or departments say whose records they see. Approver names in every approval flow come from this mapping.</p>
      {canE && <div className="card" style={{ marginBottom: 14 }}><h3>Map a role to an employee</h3>
        <PersonPicker label="Employee" value={pe?.id ?? ''} selected={pe} onChange={(_, p) => setPe(p)} search="&status=Active" placeholder="Search employee by name, code or place"
          noMatch="No active employee matches. Only active employees can hold a role." style={{ maxWidth: 420, margin: '6px 0' }} />
        <div>{!pe ? <div className="mute" style={{ fontSize: 13, padding: '8px 0' }}>Type part of a name, an employee code or a place, then pick the person.</div>
          : [pe].map((e: any) => {
            const rs = (data || []).filter((x) => x.employee_id === e.id && !RM_HIDE.includes(x.role_name)).map((x) => x.role_name);
            return <div key={e.id} className="row sp" style={{ padding: '10px 0', borderTop: '1px solid var(--line)' }}>
              <div><b>{e.name}</b> <span className="mono mute">{e.id}</span><div className="mute" style={{ fontSize: 12 }}>{e.department} · {e.city}{rs.length ? ' · Holds: ' + rs.join(', ') : ''}</div></div>
              <button className="btn sm primary" onClick={() => setM({ employeeId: e.id, name: e.name, dept: e.department, city: e.city, role: 'HRBP', access: 'locs', locationIds: [], departments: [], areaIds: [] })}>Map a role</button>
            </div>;
          })}</div>
      </div>}
      {dir.data && (gaps.length ? <div className="note bad" style={{ marginBottom: 14 }}><b>No one mapped for:</b> {gaps.slice(0, 8).join(', ')}{gaps.length > 8 ? ' and ' + (gaps.length - 8) + ' more' : ''}. Requests for these steps wait for HR Admin.</div>
        : <div className="note ok" style={{ marginBottom: 14 }}>Every approver step in use has someone mapped.</div>)}
      {overlaps.length > 0 && <div className="note" style={{ marginBottom: 14 }}><b>Check overlaps:</b> {overlaps.slice(0, 4).join(' ')}{overlaps.length > 4 ? ' and ' + (overlaps.length - 4) + ' more.' : ''}</div>}
      <div className="card"><div className="tw"><table>
        <thead><tr><th>Person</th><th>Role</th><th>Data access</th><th>Approver for</th><th>Active</th><th></th></tr></thead>
        <tbody>{data?.filter((r) => !RM_HIDE.includes(r.role_name)).map((r, n) => [r, n] as const).sort((a, b) => String(a[0].created_at ?? '').localeCompare(String(b[0].created_at ?? '')) || a[1] - b[1]).map(([r]) => {
          const c = covers(r);
          return <tr key={r.id}>
            <td><b>{r.name}</b><div className="mute" style={{ fontSize: 12 }}>{r.employee_id}{r.email ? ' · ' + r.email : ''}</div></td><td>{r.role_name}</td>
            <td className="ink2">{accessText(r.access, AREAS.includes(r.access) ? (r.area_ids ?? []).map(areaName) : r.location_ids.map(locName), r.departments)}</td>
            <td className="ink2" style={{ fontSize: 12.5, maxWidth: 260 }}>{c.length ? more3(c) : <span className="mute">{r.active ? 'Not an approver' : 'Switched off'}</span>}</td>
            <td>{canE ? <Switch on={r.active} onChange={() => toggle(r)} /> : <Pill k={r.active ? 'ok' : 'n'}>{r.active ? 'Active' : 'Inactive'}</Pill>}</td>
            <td>{canE && <button className="btn sm" onClick={() => setM({ id: r.id, employeeId: r.employee_id, name: r.name, dept: r.department || '', city: r.city || '', role: r.role_name, access: ['all', 'locs', ...AREAS].includes(r.access) ? r.access : 'all', locationIds: r.location_ids, departments: r.departments, areaIds: r.area_ids ?? [] })}>Edit</button>}</td>
          </tr>;
        })}</tbody>
      </table></div><p className="mute" style={{ fontSize: 12, margin: '12px 0 0' }}>Mappings are never deleted. Switch one off to stop the access. The most specific mapping wins: a person mapped to a property is used before a person mapped to all locations.</p></div>
      {m && <MapDialog m={m} setM={setM} rows={data || []} roles={roles.data || []} locs={locs.data || []} clusters={clusters.data || []} cities={cities.data || []}
        states={states.data || []} regions={regions.data || []} depts={depts.data || []} onSaved={() => { setM(null); setPe(null); reload(); }} />}
    </>
  );
}

function MapDialog({ m, setM, rows, roles, locs, clusters, cities, states, regions, depts, onSaved }: {
  m: MapDraft; setM: (m: MapDraft | null) => void; rows: MapRow[]; roles: string[]; locs: any[]; clusters: any[]; cities: any[]; states: any[]; regions: any[]; depts: any[]; onSaved: () => void;
}) {
  const allLocs = locs.map((l) => l.id), allDepts = depts.map((d) => d.name as string);
  // Places another active holder already has for this role (prototype rmTaken).
  const taken: Record<string, string> = {};
  rows.filter((x) => x.active && x.id !== m.id && x.role_name === m.role && x.employee_id !== m.employeeId)
    .forEach((x) => mapUnits(x.role_name, AREAS.includes(x.access) ? 'locs' : x.access, AREAS.includes(x.access) ? areaLocs(x.access, x.area_ids ?? [], locs, cities) : x.location_ids, x.departments, allLocs, allDepts).forEach((k) => { taken[k] = x.name; }));
  const anyLoc = Object.keys(taken).some((k) => k.startsWith('loc|'));
  const isTaken = (id: string) => !!taken['loc|' + id];
  const deptTaken = (n: string) => taken['dept|' + n] || taken['hdept|' + n];
  const sel = new Set(m.locationIds.filter((id) => !isTaken(id)));
  const setLocs = (ids: Set<string>) => setM({ ...m, locationIds: [...ids] });
  const flip = (ids: string[], on: boolean) => { const s = new Set(sel); ids.filter((id) => !isTaken(id)).forEach((id) => (on ? s.add(id) : s.delete(id))); setLocs(s); };
  const regionOf = (cityId: string) => states.find((s) => s.id === cities.find((c) => c.id === cityId)?.state_id)?.region_id;
  const save = async () => {
    const locIds = m.access === 'locs' ? [...sel] : [], dsel = m.departments.filter((d) => !deptTaken(d));
    if (m.access === 'locs' && !locIds.length) { toast('Pick at least one location'); return; }
    const areaIds = AREAS.includes(m.access) ? m.areaIds : [];
    if (AREAS.includes(m.access) && !areaIds.length) { toast('Pick at least one ' + ({ states: 'state', cities: 'city', clusters: 'cluster' } as Record<string, string>)[m.access]); return; }
    if (m.role === 'Function Head' && !dsel.length) { toast('Pick the departments this Function Head leads'); return; }
    if (rows.some((x) => x.employee_id === m.employeeId && x.role_name === m.role && x.id !== m.id)) { toast(`${m.name} already has the ${m.role} role. Edit that mapping instead.`); return; }
    try {
      if (m.id) await api(`/role-map/${m.id}`, { method: 'PATCH', body: { role: m.role, access: m.access, locationIds: locIds, departments: dsel, areaIds } });
      else await api('/role-map', { body: { employeeId: m.employeeId, role: m.role, access: m.access, locationIds: locIds, departments: dsel, areaIds } });
      toast('Saved'); onSaved();
    } catch (x: any) { toast(x.message); }
  };
  const close = () => setM(null);
  return <Modal wide onClose={close}>
    <div className="row sp"><h2>{m.id ? 'Edit role mapping' : 'Map a role'}</h2><button className="btn sm" onClick={close}>Close</button></div>
    <div className="note" style={{ marginTop: 12 }}><b>{m.name}</b> <span className="mono mute">{m.employeeId}</span>{m.dept && <div className="mute" style={{ fontSize: 12.5 }}>{m.dept}{m.city ? ' · ' + m.city : ''}</div>}</div>
    <div className="form" style={{ marginTop: 14 }}><label className="field"><span>Role (what they can do)</span>
      <select value={m.role} onChange={(e) => setM({ ...m, role: e.target.value })}>{(roles.includes(m.role) ? roles : [m.role, ...roles]).map((r) => <option key={r}>{r}</option>)}</select></label></div>
    <h3 style={{ margin: '16px 0 8px' }}>Data access (whose records they can see)</h3>
    <div className="row" style={{ flexWrap: 'wrap', gap: 14 }}>{([['all', 'All locations'], ['locs', 'Selected locations'], ['states', 'By state'], ['cities', 'By city'], ['clusters', 'By cluster']] as const).map(([v, l]) => <label key={v} className="chkl">
      <input type="radio" name="ua" value={v} checked={m.access === v} disabled={v === 'all' && anyLoc} onChange={() => setM({ ...m, access: v })} />{l}
      {v === 'all' && anyLoc && <> <span className="mute" style={{ fontSize: 11.5 }}>(some places already have a holder)</span></>}</label>)}</div>
    {AREAS.includes(m.access) && <div style={{ marginTop: 12 }}>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Covers every location inside the {AREA_LABEL[m.access].toLowerCase()} you pick, including locations added there later.</p>
      <div className="chkgrp">{(m.access === 'states' ? states : m.access === 'cities' ? cities : clusters).map((a) => <label key={a.id} className="chkl">
        <input type="checkbox" checked={m.areaIds.includes(a.id)} onChange={(e) => setM({ ...m, areaIds: e.target.checked ? [...m.areaIds, a.id] : m.areaIds.filter((x) => x !== a.id) })} />{a.name}
        <span className="mute" style={{ fontSize: 11.5 }}>{areaLocs(m.access, [a.id], locs, cities).filter(isTaken).length ? ' (some locations already have a holder)' : ''}</span></label>)}</div>
    </div>}
    {m.access === 'locs' && <div style={{ marginTop: 12 }}>
      <div className="row" style={{ marginBottom: 10, flexWrap: 'wrap' }}><span className="mute" style={{ fontSize: 12.5 }}>Quick select a region:</span>
        {regions.map((r) => <button key={r.id} className="btn sm" onClick={() => flip(locs.filter((l) => regionOf(l.city_id) === r.id).map((l) => l.id), true)}>{r.name}</button>)}
        <button className="btn sm" onClick={() => setLocs(new Set())}>Clear</button></div>
      <div style={{ maxHeight: 300, overflow: 'auto', border: '1px solid var(--line)', borderRadius: 10, padding: 12 }}>
        {cities.map((c) => {
          const cls = clusters.filter((k) => k.city_id === c.id), cityLocs = locs.filter((l) => l.city_id === c.id).map((l) => l.id).filter((id) => !isTaken(id));
          return <div key={c.id} style={{ marginBottom: 12 }}>
            <label className="chkl" style={{ fontWeight: 700 }}><input type="checkbox" checked={cityLocs.length > 0 && cityLocs.every((id) => sel.has(id))} onChange={(e) => flip(cityLocs, e.target.checked)} />{c.name}</label>
            {cls.map((k) => {
              const ls = locs.filter((l) => l.cluster_id === k.id), free = ls.map((l) => l.id).filter((id) => !isTaken(id));
              return ls.length ? <div key={k.id} style={{ margin: '6px 0 0 18px' }}>
                <label className="chkl mute" style={{ fontSize: 12 }}><input type="checkbox" checked={free.length > 0 && free.every((id) => sel.has(id))} onChange={(e) => flip(free, e.target.checked)} />{k.name}</label>
                <div className="chkgrp" style={{ marginLeft: 18 }}>{ls.map((l) => <label key={l.id} className="chkl">
                  <input type="checkbox" checked={sel.has(l.id)} disabled={isTaken(l.id)} onChange={(e) => flip([l.id], e.target.checked)} />{l.name}
                  {isTaken(l.id) && <> <span className="mute" style={{ fontSize: 11.5 }}>held by {taken['loc|' + l.id]}</span></>}</label>)}</div>
              </div> : null;
            })}
          </div>;
        })}
      </div>
    </div>}
    <h3 style={{ margin: '16px 0 8px' }}>Limit to departments (optional)</h3>
    <div className="chkgrp">{depts.map((d) => {
      const h = deptTaken(d.name);
      return <label key={d.id} className="chkl"><input type="checkbox" checked={m.departments.includes(d.name) && !h} disabled={!!h}
        onChange={(e) => setM({ ...m, departments: e.target.checked ? [...m.departments, d.name] : m.departments.filter((x) => x !== d.name) })} />{d.name}
        {h && <> <span className="mute" style={{ fontSize: 11.5 }}>held by {h}</span></>}</label>;
    })}</div>
    <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>Function Head: pick the departments they lead. HRBP for corporate departments: pick those departments. Otherwise leave empty.</p>
    <div className="row" style={{ marginTop: 16, justifyContent: 'flex-end' }}><button className="btn" onClick={close}>Cancel</button><button className="btn primary" onClick={save}>Save mapping</button></div>
  </Modal>;
}

// ---------- Roles and rights ----------

function Roles() {
  const { can } = useAuth();
  const { data, reload } = useLoad(() => api<any[]>('/roles').then((l) => byRoleOrder(l, (r) => r.name)), []);
  const rights = useLoad(() => api<{ id: string; label: string; confidential: boolean }[]>('/rights'), []);
  const [sel, setSel] = useState('');
  const [nn, setNn] = useState('');
  const [copy, setCopy] = useState('');
  const role = data?.find((r) => r.name === sel) ?? data?.[0];
  const canEdit = can('acc.roles', 2);
  const lock = !canEdit || role?.name === 'HR Admin';
  const put = async (body: any) => {
    try { await api(`/roles/${encodeURIComponent(role.name)}/rights`, { method: 'PUT', body }); reload(); } catch (x: any) { toast(x.message); }
  };
  const add = async () => {
    const n = nn.trim();
    if (!n) { toast('Name the role'); return; }
    if (data?.some((r) => r.name.toLowerCase() === n.toLowerCase())) { toast('A role with that name exists'); return; }
    try { const r = await api('/roles', { body: { name: n, copyFrom: copy || role.name } }); toast('Role created'); setNn(''); setCopy(''); setSel(r.name); reload(); } catch (x: any) { toast(x.message); }
  };
  const flip = async () => {
    try { await api(`/roles/${encodeURIComponent(role.name)}`, { method: 'PATCH', body: { active: !role.active } }); reload(); } catch (x: any) { toast(x.message); }
  };
  // The prototype's labels, hints and levels; any right the server adds later is listed after them.
  const known = new Set((rights.data || []).map((x) => x.id));
  const list: [string, string, string, number[], boolean?][] = [...RIGHTS.filter((x) => known.has(x[0])),
    ...(rights.data || []).filter((x) => !RIGHTS.some((p) => p[0] === x.id)).map((x) => [x.id, x.label, '', V12, x.confidential] as [string, string, string, number[], boolean])];
  const conf = role ? list.filter((x) => x[4] && (role.rights[x[0]] || 0) > 0).length : 0;
  return (
    <div className="grid split2">
      <div className="card"><h3>Roles</h3>
        {data?.map((r) => <button key={r.name} className={`rl ${r.name === role?.name ? 'on' : ''}`} onClick={() => setSel(r.name)}>{r.name} {!r.active && <Pill k="n">Off</Pill>}</button>)}
        {canEdit && <div style={{ marginTop: 14, display: 'grid', gap: 8 }}>
          <input type="text" placeholder="New role name" value={nn} onChange={(e) => setNn(e.target.value)} />
          <select value={copy || role?.name || ''} onChange={(e) => setCopy(e.target.value)}>{data?.map((r) => <option key={r.name} value={r.name}>Copy rights from {r.name}</option>)}</select>
          <button className="btn primary" onClick={add}>Add role</button>
        </div>}
      </div>
      {role && <div className="grid">
        <div className="card">
          <div className="row sp"><div><h3 style={{ margin: 0 }}>{role.name}</h3><div className="mute" style={{ fontSize: 12 }}>{role.description}</div></div>
            {canEdit && !role.system && <div className="row"><Switch on={role.active} onChange={flip} /></div>}</div>
          {role.name === 'HR Admin' && <div className="note" style={{ marginTop: 12 }}>HR Admin always keeps full rights so the system cannot be locked out.</div>}
          <div className={`note ${conf ? 'bad' : 'ok'}`} style={{ marginTop: 12 }}>{conf ? <>This role can see or change <b>{conf}</b> confidential item{conf > 1 ? 's' : ''} (marked <b>Confidential</b>): salary, personal details, ID, bank and payroll.</> : 'This role has no access to confidential data.'}</div>
        </div>
        {RGROUPS.map(([g, gl]) => {
          const rows = list.filter((x) => x[0].startsWith(g + '.'));
          return rows.length ? <div key={g} className="card"><h3>{gl}</h3><div className="tw"><table><tbody>{rows.map((x) => {
            const v = lvShown(x[3], role.rights[x[0]] || 0);
            return <tr key={x[0]}>
              <td><b>{x[1]}</b> {x[4] && <Pill k="bad">Confidential</Pill>}{x[2] && <div className="mute" style={{ fontSize: 12 }}>{x[2]}</div>}</td>
              <td style={{ width: 170 }}><select disabled={lock} value={v} style={v === 0 ? undefined : { fontWeight: 600 }} onChange={(e) => put({ rights: { [x[0]]: +e.target.value } })}>
                {lvOpts(x[3], v).map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></td>
            </tr>;
          })}</tbody></table></div></div> : null;
        })}
        {/* App addition (spec 6.3): how confidential fields show (Full, Masked, Hidden) and whether the role may download files. */}
        <Adv title="Masking and downloads"><div className="tw"><table><tbody>
          <tr><td><b>Download files</b><div className="mute" style={{ fontSize: 12 }}>Report, export and statutory files</div></td>
            <td style={{ width: 170 }}><select disabled={lock} value={role.can_download ? 'y' : 'n'} onChange={(e) => put({ canDownload: e.target.value === 'y' })}><option value="y">Allowed</option><option value="n">No access</option></select></td></tr>
          {SENS.map(([g, l]) => <tr key={g}><td><b>{l}</b> <Pill k="bad">Confidential</Pill><div className="mute" style={{ fontSize: 12 }}>Shown in full, masked to the last four characters, or hidden</div></td>
            <td style={{ width: 170 }}><select disabled={lock} value={role.sensitive[g] || 'Hidden'} onChange={(e) => put({ sensitive: { [g]: e.target.value } })}>{['Full', 'Masked', 'Hidden'].map((x) => <option key={x}>{x}</option>)}</select></td></tr>)}
        </tbody></table></div></Adv>
        <RoleReports key={role.name} role={role.name} canEdit={canEdit} />
      </div>}
    </div>
  );
}

type RoleRep = { id: string; name: string; hint: string; on: boolean; rights: boolean };

/** Which reports the role sees (test sheet #21): a tick per report, grouped as on the Reports page. A report also
 *  needs the rights it uses; one the role's rights do not allow is marked. HR Admin always sees every report. */
function RoleReports({ role, canEdit }: { role: string; canEdit: boolean }) {
  const { data, error, reload } = useLoad(() => api<{ locked: boolean; groups: { group: string; reports: RoleRep[] }[] }>(`/access/reports?role=${encodeURIComponent(role)}`), [role]);
  const lock = !canEdit || !!data?.locked;
  const set = async (ids: string[], on: boolean) => {
    if (!ids.length) return;
    try { await api(`/roles/${encodeURIComponent(role)}/reports`, { method: 'PUT', body: { reports: Object.fromEntries(ids.map((id) => [id, on])) } }); reload(); } catch (x: any) { toast(x.message); }
  };
  const all = data?.groups.flatMap((g) => g.reports) ?? [];
  const n = all.filter((r) => r.on && r.rights).length;
  return <Adv title={`Reports this role sees${data ? ` (${n} of ${all.length})` : ''}`}>
    {error && <div className="note bad">{error}</div>}
    {data?.locked && <div className="note" style={{ marginBottom: 10 }}>HR Admin always sees every report.</div>}
    {!data?.locked && <p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>Untick a report to take it away from this role: it leaves the Reports page, and running or downloading it is refused. A report also needs the rights it uses (marked when this role’s rights do not allow it).</p>}
    {data?.groups.map((g) => <div key={g.group} style={{ marginBottom: 12 }}>
      <div className="row sp"><div className="mute" style={{ fontSize: 11, fontWeight: 700, letterSpacing: '.06em', textTransform: 'uppercase' }}>{g.group}</div>
        {!lock && <div className="row" style={{ gap: 6 }}><button className="btn sm" onClick={() => set(g.reports.filter((r) => !r.on).map((r) => r.id), true)}>Tick all</button>
          <button className="btn sm" onClick={() => set(g.reports.filter((r) => r.on).map((r) => r.id), false)}>Untick all</button></div>}</div>
      <div className="tw"><table><tbody>{g.reports.map((r) => <tr key={r.id}>
        <td style={{ width: 36 }}><input type="checkbox" aria-label={r.name} disabled={lock} checked={data.locked || r.on} onChange={(e) => set([r.id], e.target.checked)} /></td>
        <td><b>{r.name}</b>{!r.rights && <> <Pill k="n">Rights do not allow</Pill></>}<div className="mute" style={{ fontSize: 12 }}>{r.hint}</div></td>
      </tr>)}</tbody></table></div>
    </div>)}
  </Adv>;
}

// ---------- Check access ----------

// The app's navigation rules (App.tsx), so "Pages they can open" is what the person gets when they sign in.
const NAV: [string, (r: Record<string, number>) => boolean][] = [
  ['Dashboard', () => true], ['Employees', (r) => r['emp.directory'] >= 1], ['Onboarding', (r) => r['onb.pipeline'] >= 1], ['Separation', (r) => r['emp.sep'] >= 1],
  ['Compensation', (r) => r['comp.bands'] >= 1 || r['comp.budget'] >= 1 || r['comp.structure'] >= 1], ['Approvals', () => true], ['Shift and roster', (r) => r['ros.roster'] >= 1],
  ['Attendance', (r) => r['att.records'] >= 1], ['Employee file', (r) => r['pay.run'] >= 1 || r['emp.salary'] >= 1], ['Payroll', (r) => r['pay.run'] >= 1], ['Reports', (r) => r['rep.reports'] >= 1],
  ['Work policies', (r) => r['pol.policies'] >= 1], ['Masters', (r) => r['mas.org'] >= 1 || r['mas.job'] >= 1], ['Bulk update', (r) => r['bulk.update'] >= 1],
  ['Access control', (r) => r['acc.roles'] >= 1 || r['acc.users'] >= 1], ['Employee dues', (r) => r['emp.assets'] >= 1 || r['pay.claims'] >= 1],
];

function CheckAccess() {
  const { can } = useAuth();
  const roles = useLoad(() => (can('acc.roles') ? api<any[]>('/roles').then((l) => byRoleOrder(l, (r) => r.name)) : Promise.resolve([])), []);
  const users = useLoad(() => (can('acc.users') ? api<MapRow[]>('/role-map').then((l) => l.filter((x) => !RM_HIDE.includes(x.role_name)).sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))) : Promise.resolve(null)), []);
  const [pick, setPick] = useState('');
  const [pe, setPe] = useState<Person | null>(null);
  const [who, setWho] = useState<{ emp: string; role: string; on: boolean; email?: string | null; access?: string; locs?: string[]; depts?: string[] } | null>(null);
  const locs = useLoad(() => api<any[]>('/masters/location'), []);
  const locName = (id: string) => locs.data?.find((l) => l.id === id)?.name ?? id;
  // Open on the first person in the list, like the prototype.
  useEffect(() => {
    const u = users.data?.find((x) => x.id === pick) ?? users.data?.[0];
    if (u) setWho({ emp: u.employee_id, role: u.role_name, on: u.active, email: u.email, access: u.access, locs: u.location_ids, depts: u.departments });
  }, [users.data, pick]);
  const p = useLoad(() => (who ? api(`/access/person/${who.emp}?role=${encodeURIComponent(who.role)}`) : Promise.resolve(null)), [who?.emp, who?.role]);
  const d = p.data;
  const rs = (roles.data || []).filter((r) => r.active && r.name !== 'Employee');
  const accText = !d ? '' : who?.locs && who.access === d.access ? accessText(d.access, who.locs.map(locName), d.departments || [])
    : accessText(d.access, d.access === 'locs' ? [`${d.locations} location${d.locations === 1 ? '' : 's'}`] : [], d.departments || []);
  const salary = d && d.confidential['emp.salary'] >= 1, bankId = d && (d.confidential['emp.bank'] >= 1 || d.confidential['emp.govid'] >= 1);
  const pages = d ? [...NAV.filter(([l, f]) => f(d.rights || {}) && !(l === 'Employee file' && d.role === 'Employee')).map(([l]) => l), 'My pay and profile'] : [];
  return <>
    {rs.length > 0 && <div className="card" style={{ marginBottom: 14 }}><h3>Who can see what</h3>
      <p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>One row per role. Red means the role can change that data, amber means view only, grey means hidden. Change a role in Roles and rights.</p>
      <div className="tw"><table><thead><tr><th>Role</th><th>Sees employees</th>{PRIV.map(([, l]) => <th key={l}>{l}</th>)}</tr></thead>
        <tbody>{rs.map((r) => <tr key={r.name}><td><b>{r.name}</b></td><td className="ink2" style={{ fontSize: 12.5 }}>{SCOPE[r.name] || ''}</td>{PRIV.map(([k]) => <td key={k}>{lvPill(r.rights[k] || 0)}</td>)}</tr>)}</tbody></table></div></div>}
    <div className="card"><h3>Check a person</h3>
      <p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>Pick a user to see exactly what they can see, do and approve. This is what they get when they sign in.</p>
      {users.data ? <PersonPicker label="User" required style={{ maxWidth: 360 }} value={pick || users.data[0]?.id || ''} onChange={(id) => id && setPick(id)}
          people={users.data.map((u) => ({ id: String(u.id), name: u.name, code: u.employee_id, detail: u.role_name + (u.active ? '' : ' · login off'), department: u.role_name, email: u.email ?? undefined }))} />
        : <PersonPicker label="Employee" value={pe?.id ?? ''} selected={pe} onChange={(_, e) => { setPe(e); if (e) setWho({ emp: e.id, role: '', on: true }); }}
          placeholder="Search employee by name, code or place" style={{ maxWidth: 360 }} />}
    </div>
    {p.error && <div className="note bad" style={{ marginTop: 14 }}>{p.error}</div>}
    {d && <div className="card" style={{ marginTop: 14 }}>
      <div className="row sp" style={{ flexWrap: 'wrap' }}><div><h3 style={{ margin: 0 }}>{d.name}</h3>
        <div className="mute" style={{ fontSize: 12.5 }}>{who?.email || d.employeeId} · {d.role}{who?.on === false ? ' · login off' : ''}{d.signedUp ? '' : ' · has not activated sign-in yet'}</div></div>
        {who?.on === false ? <Pill k="n">Login off</Pill> : <Pill k="ok">Active</Pill>}</div>
      <div className="grid g3" style={{ marginTop: 14, gap: 14 }}>
        <div className="card stat"><div className="l">Employees they can see</div><div className="n">{d.visible} <span className="mute" style={{ fontSize: 13 }}>of {d.total}</span></div><div className="mute" style={{ fontSize: 12 }}>{accText}</div></div>
        <div className="card stat"><div className="l">Can see salary</div><div className="n">{salary ? d.visible : 0}</div><div className="mute" style={{ fontSize: 12 }}>{salary ? 'employees' : 'Own payslips only'}</div></div>
        <div className="card stat"><div className="l">Can see bank and ID</div><div className="n">{bankId ? d.visible : 0}</div><div className="mute" style={{ fontSize: 12 }}>{bankId ? 'employees' : 'Own details only'}</div></div>
      </div>
      <div style={{ marginTop: 14 }}><b style={{ fontSize: 13 }}>Confidential data</b><div className="tw" style={{ marginTop: 6 }}><table><tbody>{PRIV.map(([k, l]) => <tr key={k}><td>{l}</td><td style={{ width: 120 }}>{lvPill(d.confidential[k] || 0)}</td></tr>)}</tbody></table></div></div>
      <div style={{ marginTop: 14 }}><b style={{ fontSize: 13 }}>Pages they can open</b><div style={{ marginTop: 6 }}>{chips(pages)}</div></div>
      <div style={{ marginTop: 14 }}><b style={{ fontSize: 13 }}>Can do</b><div style={{ marginTop: 6 }}>{chips(d.actions || [])}</div></div>
      <div style={{ marginTop: 14 }}><b style={{ fontSize: 13 }}>Approves by role</b><div style={{ marginTop: 6 }}>{chips(d.approves || [])}</div></div>
      <div style={{ marginTop: 14 }}><b style={{ fontSize: 13 }}>Named as</b><div style={{ marginTop: 6 }}>{chips(d.named || [])}</div></div>
      {!d.canDownload && <div className="note warn" style={{ marginTop: 14 }}>Downloads are switched off for this role.</div>}
    </div>}
    {who?.emp && <EffectiveRights empId={who.emp} />}
  </>;
}
