import { useState } from 'react';
import { api, fmtD, today } from '../api';
import { useAuth } from '../auth';
import PersonPicker, { type Person } from './PersonPicker';
import { download, Pill, Switch, toast, useLoad } from './ui';

// Access additions (workforce plan module 2): personal rights, "who can see what" for a person, the role-change card
// and the effective work policy on the employee page.

const LV: Record<number, string> = { 0: 'No access', 1: 'View', 2: 'Edit' };
const RM_HIDE = ['Employee', 'Line Manager'];
// Built-in roles that can be mapped, for a caller who cannot list roles (no Roles and rights view).
const BUILTIN = ['HR Admin', 'HRBP', 'Property Manager', 'Cluster Manager', 'City Head', 'Operations Head', 'Function Head', 'HRBP Corporate', 'Payroll', 'Payroll Manager', 'CHRO', 'Finance Head', 'Finance', 'MIS Executive'];
export const AREA_LABEL: Record<string, string> = { clusters: 'Clusters', cities: 'Cities', states: 'States' };
const AREA_MASTER: Record<string, string> = { clusters: 'cluster', cities: 'city', states: 'state' };

type URight = { id: string; employeeId: string; name: string; right: string; rightLabel: string; kind: 'grant' | 'block'; level: number; levelText: string;
  until?: string | null; reason?: string | null; state: string; createdBy?: string; createdAt?: string; endedBy?: string | null; endedAt?: string | null };

const rightText = (x: URight) => (x.kind === 'grant' ? 'Given: ' : 'Blocked to ') + x.levelText;

/** Form to give or block one right for one person; personPicker hidden when the person is fixed (employee page). */
function RightForm({ fixed, onSaved }: { fixed?: { id: string; name: string }; onSaved: () => void }) {
  const rights = useLoad(() => api<{ id: string; label: string; confidential: boolean }[]>('/rights'), []);
  const [pe, setPe] = useState<Person | null>(null);
  const [f, setF] = useState({ right: '', kind: 'grant' as 'grant' | 'block', level: 1, until: '', reason: '' });
  const emp = fixed?.id ?? pe?.id ?? '';
  const save = async () => {
    if (!emp) { toast('Pick the person'); return; }
    if (!f.right) { toast('Pick a right'); return; }
    try {
      await api('/user-rights', { body: { employeeId: emp, right: f.right, kind: f.kind, level: f.level, until: f.until, reason: f.reason } });
      toast(f.kind === 'grant' ? 'Right given' : 'Right blocked'); setF({ ...f, right: '', reason: '', until: '' }); onSaved();
    } catch (x: any) { toast(x.message); }
  };
  const levels = f.kind === 'grant' ? [1, 2] : [0, 1];
  return <div className="form" style={{ alignItems: 'end' }}>
    {!fixed && <PersonPicker label="Employee" value={pe?.id ?? ''} selected={pe} onChange={(_, p) => setPe(p)} placeholder="Search employee by name, code or place" style={{ maxWidth: 360 }} />}
    <label className="field"><span>Right</span><select value={f.right} onChange={(e) => setF({ ...f, right: e.target.value })}>
      <option value="">Pick a right</option>{(rights.data || []).map((r) => <option key={r.id} value={r.id}>{r.label}{r.confidential ? ' (confidential)' : ''}</option>)}</select></label>
    <label className="field"><span>Give or block</span><select value={f.kind} onChange={(e) => { const k = e.target.value as 'grant' | 'block'; setF({ ...f, kind: k, level: k === 'grant' ? 1 : 0 }); }}>
      <option value="grant">Give on top of roles</option><option value="block">Block</option></select></label>
    <label className="field"><span>{f.kind === 'grant' ? 'Level given' : 'Leaves at most'}</span><select value={f.level} onChange={(e) => setF({ ...f, level: +e.target.value })}>
      {levels.map((l) => <option key={l} value={l}>{LV[l]}</option>)}</select></label>
    <label className="field"><span>Last day (optional)</span><input type="date" min={today()} value={f.until} onChange={(e) => setF({ ...f, until: e.target.value })} /></label>
    <label className="field"><span>Reason</span><input type="text" maxLength={300} value={f.reason} onChange={(e) => setF({ ...f, reason: e.target.value })} placeholder="Why this person needs it" /></label>
    <div><button className="btn primary" onClick={save}>{f.kind === 'grant' ? 'Give right' : 'Block right'}</button></div>
  </div>;
}

function RightRows({ rows, canE, onEnd, showName }: { rows: URight[]; canE: boolean; onEnd: (id: string) => void; showName?: boolean }) {
  if (!rows.length) return <p className="mute" style={{ margin: 0, fontSize: 13 }}>No personal rights. Everything comes from roles.</p>;
  return <div className="tw"><table><thead><tr>{showName && <th>Person</th>}<th>Right</th><th>Personal</th><th>Until</th><th>Reason</th><th>Set by</th><th>State</th><th></th></tr></thead>
    <tbody>{rows.map((x) => <tr key={x.id}>
      {showName && <td><b>{x.name}</b><div className="mute mono" style={{ fontSize: 12 }}>{x.employeeId}</div></td>}
      <td>{x.rightLabel}</td><td><Pill k={x.kind === 'grant' ? 'info' : 'bad'}>{rightText(x)}</Pill></td>
      <td>{x.until ? fmtD(x.until) : <span className="mute">No end date</span>}</td><td className="ink2" style={{ fontSize: 12.5, maxWidth: 240 }}>{x.reason || '—'}</td>
      <td className="ink2" style={{ fontSize: 12.5 }}>{x.createdBy}{x.createdAt ? ', ' + fmtD(x.createdAt) : ''}</td>
      <td><Pill k={x.state === 'In force' ? 'ok' : 'n'}>{x.state}</Pill>{x.endedBy && <div className="mute" style={{ fontSize: 11.5 }}>by {x.endedBy}</div>}</td>
      <td>{canE && x.state !== 'Ended' && <button className="btn sm" onClick={() => onEnd(x.id)}>{x.kind === 'grant' ? 'End' : 'Lift'}</button>}</td>
    </tr>)}</tbody></table></div>;
}

const endRight = async (id: string, done: () => void) => {
  try { await api(`/user-rights/${id}`, { method: 'DELETE' }); toast('Ended'); done(); } catch (x: any) { toast(x.message); }
};

/** Access control › Personal rights. */
export function PersonalRights() {
  const { can } = useAuth();
  const canE = can('acc.users', 2);
  const [all, setAll] = useState(false);
  const { data, reload } = useLoad(() => api<URight[]>(`/user-rights${all ? '?all=1' : ''}`), [all]);
  return <>
    <p className="ink2" style={{ margin: '0 0 14px', fontSize: 13.5 }}>Give one person a single right on top of their roles, or block one, with an optional last day. A block wins over every role and grant. HR Admin always has every right and cannot be blocked. Rights given here work within the data access of the role view the person signs in with.</p>
    {canE && <div className="card" style={{ marginBottom: 14 }}><h3>Give or block a right</h3><RightForm onSaved={reload} /></div>}
    <div className="card"><div className="row sp"><h3 style={{ margin: 0 }}>Personal rights</h3>
      <label className="chkl"><input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />Show ended ones too</label></div>
      <div style={{ marginTop: 10 }}>{data && <RightRows rows={data} canE={canE} showName onEnd={(id) => endRight(id, reload)} />}</div></div>
  </>;
}

type Eff = { employeeId: string; name: string; admin: boolean; roles: { role: string; scope: string }[]; personal: number;
  rights: { id: string; label: string; confidential: boolean; level: number; blocked: boolean; sources: { kind: string; role?: string; level: number; until?: string; note?: string }[] }[] };

const srcPill = (s: Eff['rights'][0]['sources'][0], i: number) => {
  const t = s.kind === 'admin' ? 'HR Admin: every right' : s.kind === 'role' ? `${s.role}: ${LV[s.level]}` : s.kind === 'grant' ? `Personal: ${LV[s.level]}${s.until ? ' until ' + fmtD(s.until) : ''}`
    : `Blocked to ${LV[s.level]}${s.until ? ' until ' + fmtD(s.until) : ''}`;
  return <span key={i}>{i > 0 && ' '}<Pill k={s.kind === 'block' ? 'bad' : s.kind === 'role' || s.kind === 'admin' ? 'n' : 'info'}>{t}</Pill></span>;
};

/** Who can see what: one person's effective rights across all their roles and where each comes from. */
export function EffectiveRights({ empId }: { empId: string }) {
  const { me } = useAuth();
  const { data, error } = useLoad(() => (empId ? api<Eff>(`/access/person/${empId}/effective`) : Promise.resolve(null)), [empId]);
  const [onlyGiven, setOnlyGiven] = useState(true);
  if (error) return <div className="note bad">{error}</div>;
  if (!data) return null;
  const rows = data.rights.filter((r) => !onlyGiven || r.level > 0 || r.sources.length > 0);
  return <div className="card" style={{ marginTop: 14 }}>
    <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}><div><h3 style={{ margin: 0 }}>Who can see what: {data.name}</h3>
      <div className="mute" style={{ fontSize: 12.5 }}>{data.roles.map((r) => `${r.role} (${r.scope})`).join(' · ')}{data.personal ? ` · ${data.personal} personal right${data.personal > 1 ? 's' : ''}` : ''}</div></div>
      <div className="row" style={{ gap: 8 }}><label className="chkl"><input type="checkbox" checked={onlyGiven} onChange={(e) => setOnlyGiven(e.target.checked)} />Only rights they have</label>
        {me?.ctx.canDownload && <button className="btn sm" onClick={() => download(`/access/person/${empId}/effective.xlsx`, `access-${empId}.xlsx`).catch((e) => toast(e.message))}>Download</button>}</div></div>
    {data.admin && <div className="note" style={{ marginTop: 10 }}>HR Admin has every right, always. Personal blocks do not apply to an HR Admin.</div>}
    <p className="mute" style={{ fontSize: 12, margin: '8px 0' }}>The level is the best of their roles, plus personal grants, minus blocks. A role's level applies when they use that role view, within its data access.</p>
    <div className="tw"><table><thead><tr><th>Right</th><th>Effective</th><th>Where it comes from</th></tr></thead>
      <tbody>{rows.map((r) => <tr key={r.id}>
        <td><b>{r.label}</b> {r.confidential && <Pill k="bad">Confidential</Pill>}</td>
        <td><Pill k={r.level >= 2 ? 'bad' : r.level === 1 ? 'warn' : 'n'}>{LV[r.level]}</Pill>{r.blocked && <div className="mute" style={{ fontSize: 11.5 }}>blocked</div>}</td>
        <td>{r.sources.length ? r.sources.map(srcPill) : <span className="mute">No role gives it</span>}</td>
      </tr>)}</tbody></table></div>
  </div>;
}

type MapRow = { id: string; employee_id: string; role_name: string; access: string; location_ids: string[]; departments: string[]; area_ids?: string[]; active: boolean };

/** Employee page: the person's roles (assign, switch off) and personal rights, gated by Role mapping edit. */
export function EmployeeAccessCard({ id, name }: { id: string; name: string }) {
  const { can } = useAuth();
  const see = can('acc.users'), canE = can('acc.users', 2);
  const maps = useLoad(() => (see ? api<MapRow[]>('/role-map').then((l) => l.filter((x) => x.employee_id === id && !RM_HIDE.includes(x.role_name))) : Promise.resolve(null)), [id, see]);
  const rights = useLoad(() => (see ? api<URight[]>(`/user-rights?employee=${encodeURIComponent(id)}`) : Promise.resolve(null)), [id, see]);
  const roles = useLoad(() => (canE ? api<any[]>('/roles').then((l) => l.filter((r) => r.active && !RM_HIDE.includes(r.name)).map((r) => r.name as string)).catch(() => BUILTIN) : Promise.resolve([] as string[])), [canE]);
  const [area, setArea] = useState('all');
  const areas = useLoad(() => (area !== 'all' ? api<any[]>(`/masters/${AREA_MASTER[area]}?status=Active`) : Promise.resolve([])), [area]);
  const [add, setAdd] = useState<{ role: string; ids: string[] } | null>(null);
  const [rightOpen, setRightOpen] = useState(false);
  if (!see) return null;
  const flip = async (m: MapRow) => {
    try { await api(`/role-map/${m.id}`, { method: 'PATCH', body: { active: !m.active } }); toast(m.active ? 'Role switched off' : 'Role switched on'); maps.reload(); } catch (x: any) { toast(x.message); }
  };
  const save = async () => {
    if (!add?.role) { toast('Pick a role'); return; }
    if (area !== 'all' && !add.ids.length) { toast('Pick at least one ' + AREA_MASTER[area]); return; }
    try {
      await api('/role-map', { body: { employeeId: id, role: add.role, access: area, areaIds: area === 'all' ? [] : add.ids, locationIds: [], departments: [] } });
      toast('Role assigned'); setAdd(null); setArea('all'); maps.reload();
    } catch (x: any) { toast(x.message); }
  };
  const scope = (m: MapRow) => (m.access === 'all' ? 'All locations' : m.access === 'locs' ? `${m.location_ids.length} location${m.location_ids.length === 1 ? '' : 's'}`
    : `${AREA_LABEL[m.access]}: ${m.area_ids?.length ?? 0}`) + (m.departments.length ? ' · ' + m.departments.join(', ') : '');
  return <div className="card" style={{ marginTop: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Roles and access</h3>{canE && !add && <button className="btn sm" onClick={() => setAdd({ role: '', ids: [] })}>Assign a role</button>}</div>
    <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 10px' }}>{name} is an Employee; Line Manager comes from having reports. Every change here is in the audit log. Map by selected locations or departments under Access control › Role mapping.</p>
    {maps.data && (maps.data.length ? <div className="tw"><table><tbody>{maps.data.map((m) => <tr key={m.id}><td><b>{m.role_name}</b></td><td className="ink2">{scope(m)}</td>
      <td style={{ width: 90 }}>{canE ? <Switch on={m.active} onChange={() => flip(m)} /> : <Pill k={m.active ? 'ok' : 'n'}>{m.active ? 'Active' : 'Off'}</Pill>}</td></tr>)}</tbody></table></div>
      : <p className="mute" style={{ margin: 0, fontSize: 13 }}>No roles mapped.</p>)}
    {add && <div className="form" style={{ marginTop: 10, alignItems: 'end' }}>
      <label className="field"><span>Role</span><select value={add.role} onChange={(e) => setAdd({ ...add, role: e.target.value })}><option value="">Pick a role</option>{(roles.data || []).map((r) => <option key={r}>{r}</option>)}</select></label>
      <label className="field"><span>Data access</span><select value={area} onChange={(e) => { setArea(e.target.value); setAdd({ ...add, ids: [] }); }}>
        <option value="all">All locations</option><option value="states">By state</option><option value="cities">By city</option><option value="clusters">By cluster</option></select></label>
      {area !== 'all' && <div className="chkgrp" style={{ minWidth: 220 }}>{(areas.data || []).map((a) => <label key={a.id} className="chkl">
        <input type="checkbox" checked={add.ids.includes(a.id)} onChange={(e) => setAdd({ ...add, ids: e.target.checked ? [...add.ids, a.id] : add.ids.filter((x) => x !== a.id) })} />{a.name}</label>)}</div>}
      <div className="row" style={{ gap: 8 }}><button className="btn" onClick={() => setAdd(null)}>Cancel</button><button className="btn primary" onClick={save}>Assign</button></div>
    </div>}
    <div className="row sp" style={{ marginTop: 16 }}><b style={{ fontSize: 13 }}>Personal rights</b>{canE && <button className="btn sm" onClick={() => setRightOpen(!rightOpen)}>{rightOpen ? 'Close' : 'Give or block a right'}</button>}</div>
    {rightOpen && <div style={{ marginTop: 8 }}><RightForm fixed={{ id, name }} onSaved={() => { setRightOpen(false); rights.reload(); }} /></div>}
    <div style={{ marginTop: 8 }}>{rights.data && <RightRows rows={rights.data} canE={canE} onEnd={(rid) => endRight(rid, rights.reload)} />}</div>
  </div>;
}

/** Employee page: the work policy the person gets and why (the policy conflict check, module 5). */
export function EffectivePolicyCard({ id }: { id: string }) {
  const { data, error } = useLoad(() => api<any>(`/employees/${id}/policy`), [id]);
  if (error || !data) return null;
  return <div className="card" style={{ marginTop: 14 }}><h3>Work policy</h3>
    <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}><b>{data.policy}</b>{data.exception ? <Pill k="warn">Exception for this person</Pill> : <Pill k="n">{data.via}</Pill>}
      {data.leavePlan && <span className="mute" style={{ fontSize: 12.5 }}>Leave plan: {data.leavePlan}</span>}</div>
    <p className="ink2" style={{ fontSize: 13, margin: '6px 0 0' }}>{data.why}</p>
    {data.others?.length > 0 && <div style={{ marginTop: 8 }}><span className="mute" style={{ fontSize: 12.5 }}>Also matched (lower in the order, so not used):</span>
      <ul style={{ margin: '4px 0 0', paddingLeft: 18, fontSize: 12.5 }}>{data.others.map((o: any, i: number) => <li key={i}>{o.policy} through {o.through}{o.overridden ? '' : ' (same policy)'}</li>)}</ul></div>}
  </div>;
}
