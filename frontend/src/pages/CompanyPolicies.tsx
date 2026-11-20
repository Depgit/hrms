import { useState } from 'react';
import { api, fmtD, openDoc, today, uploadDoc } from '../api';
import { Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import PersonPicker, { type Person } from '../components/PersonPicker';
import { ACK_TONE } from './PolicyAck';

// Work policies › Company policies (HR Admin). A policy document with a version, an effective date and whom it
// applies to. Publishing makes it due for everyone it applies to; a new version makes it due again; retiring
// ends it. Each person's signed copy goes to their reporting manager. While anything is pending when the
// payroll run is made, the salary for that month is held, and released once everything is approved.

const STATUSES = ['Signed', 'Waiting for manager', 'Returned', 'Not submitted'] as const;
const TYPES = ['Permanent', 'Retainer', 'Contractual'];
type Aud = { all: boolean; departments?: string[]; locations?: string[]; types?: string[]; clusters?: string[]; cities?: string[]; employees?: string[] };
type Masters = { depts: { id: string; name: string }[]; locs: { id: string; name: string }[]; clusters?: { id: string; name: string }[]; cities?: { id: string; name: string }[] };

const draftKey = () => 'DRAFT-' + Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) => b.toString(16).padStart(2, '0')).join('');
const when = (s?: string | null) => (s ? fmtD(String(s).slice(0, 10)) : '—');

function audText(a: Aud | undefined, m: Masters | null): string {
  if (!a || a.all) return 'Everyone';
  const loc = (id: string) => m?.locs.find((l) => l.id === id)?.name ?? 'a location';
  const cl = (id: string) => m?.clusters?.find((l) => l.id === id)?.name ?? 'a cluster';
  const ci = (id: string) => m?.cities?.find((l) => l.id === id)?.name ?? 'a city';
  const places = [...(a.cities ?? []).map(ci), ...(a.clusters ?? []).map(cl), ...(a.locations ?? []).map(loc)];
  return [a.departments?.length ? a.departments.join(', ') : '', places.join(', '), a.types?.length ? a.types.join(', ') : '',
    a.employees?.length ? `${a.employees.length} named employee${a.employees.length > 1 ? 's' : ''}` : ''].filter(Boolean).join(' · ');
}

export default function CompanyPolicies() {
  const list = useLoad(() => api<{ policies: any[]; canEdit: boolean }>('/company-policies'), []);
  const masters = useLoad(async (): Promise<Masters> => ({ depts: await api<any[]>('/masters/department?status=Active'), locs: await api<any[]>('/masters/location?status=Active'),
    clusters: await api<any[]>('/masters/cluster?status=Active'), cities: await api<any[]>('/masters/city?status=Active') }), []);
  const [open, setOpen] = useState('');
  const [form, setForm] = useState<null | { mode: 'add' | 'edit' | 'version'; p?: any }>(null);
  if (!list.data) return <Loading error={list.error} />;
  const { policies, canEdit } = list.data;
  const done = (id?: string) => { setForm(null); list.reload(); if (id) setOpen(id); };
  const cur = open ? policies.find((p) => p.id === open) : null;
  return <>
    {cur ? <PolicyDetail key={cur.id} p={cur} m={masters.data} canEdit={canEdit} onBack={() => setOpen('')} onChanged={() => list.reload()} onForm={(mode) => setForm({ mode, p: cur })} />
      : <>
        <div className="note" style={{ marginBottom: 14 }}>Policies every employee it applies to must download, sign and upload. The signed copy goes to the reporting manager for approval (HR Admin and the HRBP can approve too). While a policy is still pending when the payroll run is made, that person’s salary for the month is held, and released once everything is approved.</div>
        <div className="card">
          <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>Company policies</h3>{canEdit && <button className="btn primary" onClick={() => setForm({ mode: 'add' })}>Add a policy</button>}</div>
          <div className="tw"><table><thead><tr><th>Policy</th><th>Applies to</th><th>Status</th><th className="num">Signed</th><th className="num">Waiting for manager</th><th className="num">Returned</th><th className="num">Not submitted</th><th></th></tr></thead><tbody>
            {policies.map((p) => {
              const n = p.counts || {};
              return <tr key={p.id}>
                <td><b>{p.title}</b><div className="mute" style={{ fontSize: 12 }}>Version {p.version} · in force from {fmtD(p.effective_from)}</div></td>
                <td className="ink2" style={{ fontSize: 12.5 }}>{audText(p.audience, masters.data)}{!p.new_joiners && <div className="mute" style={{ fontSize: 12 }}>New joiners need not sign</div>}</td>
                <td><Pill k={p.status === 'Published' ? 'ok' : p.status === 'Draft' ? 'warn' : 'n'}>{p.status}</Pill></td>
                {STATUSES.map((st) => <td key={st} className="num">{p.status === 'Published' ? n[st] ?? 0 : '—'}</td>)}
                <td><button className="btn sm" onClick={() => setOpen(p.id)}>Open</button></td></tr>;
            })}
            {!policies.length && <tr><td colSpan={8} className="mute">No company policy yet.{canEdit ? ' Add one to collect signed copies.' : ''}</td></tr>}
          </tbody></table></div>
        </div>
      </>}
    {form && <PolicyForm mode={form.mode} p={form.p} m={masters.data} onClose={() => setForm(null)} onDone={done} />}
  </>;
}

function PolicyDetail({ p, m, canEdit, onBack, onChanged, onForm }: { p: any; m: Masters | null; canEdit: boolean; onBack: () => void; onChanged: () => void; onForm: (mode: 'edit' | 'version') => void }) {
  const [st, setSt] = useState('');
  const d = useLoad(() => api<any>(`/company-policies/${p.id}${st ? `?status=${encodeURIComponent(st)}` : ''}`), [p.id, st, p.version_id, p.status]);
  const [retire, setRetire] = useState<string | null>(null);
  const [q, setQ] = useState('');
  const act = async (path: string, body?: unknown, msg = 'Saved') => {
    try { await api(`/company-policies/${p.id}/${path}`, { method: 'POST', body }); toast(msg); setRetire(null); onChanged(); d.reload(); } catch (x: any) { toast(x.message); }
  };
  const open = (id?: string | null) => { if (id) openDoc(id).catch((x) => toast(x.message)); };
  const x = d.data;
  const n = x?.counts || {};
  const people = (x?.people as any[] | undefined)?.filter((r) => !q || `${r.employee_id} ${r.name}`.toLowerCase().includes(q.toLowerCase()));
  return <>
    <div className="row" style={{ marginBottom: 12 }}><button className="btn sm" onClick={onBack}>← All company policies</button></div>
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ alignItems: 'flex-start' }}>
        <div><h2 style={{ margin: 0, fontSize: 18 }}>{p.title}</h2>
          <div className="mute" style={{ fontSize: 13, marginTop: 2 }}>Version {p.version} · in force from {fmtD(p.effective_from)} · applies to {audText(p.audience, m).toLowerCase() === 'everyone' ? 'everyone' : audText(p.audience, m)}{p.new_joiners ? '' : ' · new joiners need not sign'}</div></div>
        <div className="row" style={{ gap: 6 }}><Pill k={p.status === 'Published' ? 'ok' : p.status === 'Draft' ? 'warn' : 'n'}>{p.status}</Pill></div>
      </div>
      <div className="row" style={{ gap: 8, marginTop: 12 }}>
        <button className="btn sm" onClick={() => open(p.policy_doc)}>Open the policy</button>
        {canEdit && p.status === 'Draft' && <button className="btn sm primary" onClick={() => act('publish', undefined, 'Published. It is now due for everyone it applies to')}>Publish</button>}
        {canEdit && p.status !== 'Retired' && <button className="btn sm" onClick={() => onForm('edit')}>Edit</button>}
        {canEdit && p.status === 'Published' && <button className="btn sm" onClick={() => onForm('version')}>New version</button>}
        {canEdit && p.status !== 'Retired' && <button className="btn sm bad" onClick={() => setRetire('')}>Retire</button>}
      </div>
      {p.status === 'Draft' && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>A draft is not shown to employees. Publishing makes it due for everyone it applies to.</p>}
      {p.status === 'Retired' && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Retired by {p.retired_by} on {when(p.retired_at)}: {p.retire_note}</p>}
    </div>
    {p.status === 'Published' && <>
      <div className="grid" style={{ marginBottom: 14, gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))' }}>
        {STATUSES.map((s) => <button key={s} className="card stat" style={{ textAlign: 'left', cursor: 'pointer', outline: st === s ? '2px solid var(--accent)' : undefined }} onClick={() => setSt(st === s ? '' : s)}>
          <div className="l">{s}</div><div className="n">{n[s] ?? 0}</div><div className="s">{st === s ? 'Shown below. Click again for all' : 'Show who'}</div></button>)}
      </div>
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>{st ? st : 'Everyone it applies to'}{x ? ` (${people?.length ?? 0})` : ''}</h3>
          <input type="search" placeholder="Search by name or ID" value={q} onChange={(e) => setQ(e.target.value)} style={{ maxWidth: 240 }} /></div>
        {!x ? <Loading error={d.error} /> : <div className="tw"><table><thead><tr><th>Employee</th><th>Department and location</th><th>Reporting manager</th><th>Status</th><th>Submitted</th><th>Decision</th><th></th></tr></thead><tbody>
          {people!.map((r) => <tr key={r.employee_id}>
            <td><b>{r.name}</b><div className="mono mute" style={{ fontSize: 12 }}>{r.employee_id}</div></td>
            <td className="ink2" style={{ fontSize: 12.5 }}>{r.department} · {r.location}</td>
            <td>{r.manager || '—'}</td>
            <td><Pill k={ACK_TONE[r.status]}>{r.status}</Pill></td>
            <td style={{ whiteSpace: 'nowrap' }}>{when(r.submitted_at)}</td>
            <td className="ink2" style={{ fontSize: 12.5 }}>{r.status === 'Signed' ? `${r.approved_by ?? ''}, ${when(r.approved_at)}` : r.status === 'Returned' ? `${r.comment_by ?? ''}: “${r.comment ?? ''}”` : '—'}</td>
            <td>{r.signed_doc && <button className="btn sm" onClick={() => open(r.signed_doc)}>Signed copy</button>}</td></tr>)}
          {!people!.length && <tr><td colSpan={7} className="mute">Nobody here.</td></tr>}
        </tbody></table></div>}
      </div>
    </>}
    {x && <div className="card"><h3>Versions</h3><div className="tw"><table><thead><tr><th>Version</th><th>In force from</th><th>Published</th><th>Note</th><th></th></tr></thead><tbody>
      {(x.versions as any[]).map((v) => <tr key={v.id}><td><b>{v.version}</b>{v.id === p.version_id && <> <Pill k="info">Current</Pill></>}</td><td>{fmtD(v.effective_from)}</td>
        <td>{v.published_at ? `${when(v.published_at)} by ${v.published_by}` : 'Not yet'}</td><td className="ink2">{v.note || '—'}</td>
        <td><button className="btn sm" onClick={() => open(v.document_id)}>Open</button></td></tr>)}
    </tbody></table></div></div>}
    {retire !== null && <Modal onClose={() => setRetire(null)}>
      <h2>Retire {p.title}</h2>
      <p className="mute" style={{ fontSize: 13 }}>Nobody needs to sign it after this. Signed copies waiting for a manager are cancelled, and a salary held only for company policies is released.</p>
      <label className="field"><span>Reason</span><input value={retire} onChange={(e) => setRetire(e.target.value)} placeholder="For example: replaced by the new code of conduct" autoFocus /></label>
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={() => setRetire(null)}>Cancel</button><button className="btn primary" onClick={() => act('retire', { note: retire }, 'Policy retired')}>Retire policy</button></div>
    </Modal>}
  </>;
}

function Chips({ all, on, set }: { all: [string, string][]; on: string[]; set: (v: string[]) => void }) {
  return <div className="chips">{all.map(([v, l]) => <button key={v} type="button" className={on.includes(v) ? 'on' : ''} onClick={() => set(on.includes(v) ? on.filter((x) => x !== v) : [...on, v])}>{l}</button>)}</div>;
}

function PolicyForm({ mode, p, m, onClose, onDone }: { mode: 'add' | 'edit' | 'version'; p?: any; m: Masters | null; onClose: () => void; onDone: (id?: string) => void }) {
  const draft = mode === 'add' || (mode === 'edit' && p?.status === 'Draft');
  const [f, setF] = useState(() => ({
    title: p?.title ?? '', version: mode === 'version' ? '' : p?.version ?? '1.0', effective: mode === 'version' ? today() : p?.effective_from ?? today(),
    all: p ? !!p.audience?.all : true, depts: (p?.audience?.departments ?? []) as string[], locs: (p?.audience?.locations ?? []) as string[], types: (p?.audience?.types ?? []) as string[],
    clusters: (p?.audience?.clusters ?? []) as string[], cities: (p?.audience?.cities ?? []) as string[], emps: (p?.audience?.employees ?? []) as string[],
    joiners: p ? !!p.new_joiners : true, note: '',
  }));
  const [doc, setDoc] = useState<{ id: string; name: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [key] = useState(draftKey);
  const up = async (file?: File) => {
    if (!file) return;
    if (file.type && file.type !== 'application/pdf') { toast('The policy document must be a PDF'); return; }
    try { const r = await uploadDoc('policy', key, 'document', file); setDoc({ id: r.id, name: r.file_name }); } catch (x: any) { toast(x.message); }
  };
  const [pe, setPe] = useState<Person | null>(null);
  const aud: Aud = f.all ? { all: true } : { all: false, departments: f.depts, locations: f.locs, types: f.types, clusters: f.clusters, cities: f.cities, employees: f.emps };
  const save = async (publish = false) => {
    setBusy(true);
    try {
      if (mode === 'add') {
        const r = await api<{ id: string }>('/company-policies', { body: { title: f.title, version: f.version, effectiveFrom: f.effective, audience: aud, newJoiners: f.joiners, docId: doc?.id ?? '', publish } });
        toast(publish ? 'Published. It is now due for everyone it applies to' : 'Saved as a draft'); onDone(r.id);
      } else if (mode === 'version') {
        await api(`/company-policies/${p.id}/version`, { body: { version: f.version, effectiveFrom: f.effective, docId: doc?.id ?? '', note: f.note } });
        toast(`Version ${f.version} published. It is due again for everyone it applies to`); onDone(p.id);
      } else {
        await api(`/company-policies/${p.id}`, { method: 'PUT', body: { title: f.title, audience: aud, newJoiners: f.joiners, ...(draft ? { version: f.version, effectiveFrom: f.effective, docId: doc?.id ?? '' } : {}) } });
        toast('Saved'); onDone(p.id);
      }
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  const showDoc = mode !== 'edit' || draft, showVer = mode !== 'edit' || draft, showAud = mode !== 'version';
  return <Modal onClose={onClose} wide>
    <h2>{mode === 'add' ? 'Add a company policy' : mode === 'version' ? `New version of ${p.title}` : `Edit ${p.title}`}</h2>
    <p className="mute" style={{ fontSize: 13, marginTop: 0 }}>{mode === 'version' ? 'Publishing a new version makes the policy due again for everyone it applies to. Copies of the earlier version waiting for a manager are cancelled.'
      : mode === 'edit' && !draft ? 'The document and version change with a new version. Changing whom it applies to takes effect at once.' : 'Upload the policy as a PDF. Employees download it, sign it and upload the signed copy.'}</p>
    <div className="form">
      {mode !== 'version' && <label className="field" style={{ gridColumn: '1 / -1' }}><span>Title</span><input value={f.title} onChange={(e) => setF({ ...f, title: e.target.value })} placeholder="For example: Code of conduct" /></label>}
      {showVer && <label className="field"><span>Version</span><input value={f.version} onChange={(e) => setF({ ...f, version: e.target.value })} placeholder="1.0" /></label>}
      {showVer && <label className="field"><span>In force from</span><input type="date" value={f.effective} onChange={(e) => setF({ ...f, effective: e.target.value })} />
        <small>Salaries are held for months on or after this date while it is pending.</small></label>}
      {showDoc && <div className="field"><span>Policy document (PDF)</span>
        <label className="btn">{doc ? 'Choose another file' : mode === 'edit' ? 'Replace the file' : 'Choose PDF'}<input type="file" accept="application/pdf" hidden onChange={(e) => { up(e.target.files?.[0]); e.target.value = ''; }} /></label>
        <small>{doc ? `Uploaded: ${doc.name}` : mode === 'edit' ? 'The current file stays unless you replace it.' : 'Up to 5 MB.'}</small></div>}
      {mode === 'version' && <label className="field" style={{ gridColumn: '1 / -1' }}><span>What changed</span><input value={f.note} onChange={(e) => setF({ ...f, note: e.target.value })} placeholder="For example: new leave rules in section 4" /></label>}
    </div>
    {showAud && <>
      <h3 style={{ margin: '16px 0 8px', fontSize: 14 }}>Applies to</h3>
      <div className="seg" style={{ marginBottom: 10 }}><button type="button" className={f.all ? 'on' : ''} onClick={() => setF({ ...f, all: true })}>Everyone</button><button type="button" className={!f.all ? 'on' : ''} onClick={() => setF({ ...f, all: false })}>Chosen groups</button></div>
      {!f.all && <div style={{ display: 'grid', gap: 10 }}>
        <div><div className="mute" style={{ fontSize: 12, fontWeight: 700, marginBottom: 4 }}>Departments</div><Chips all={(m?.depts ?? []).map((d) => [d.name, d.name])} on={f.depts} set={(v) => setF({ ...f, depts: v })} /></div>
        <div><div className="mute" style={{ fontSize: 12, fontWeight: 700, marginBottom: 4 }}>Cities</div><Chips all={(m?.cities ?? []).map((l) => [l.id, l.name])} on={f.cities} set={(v) => setF({ ...f, cities: v })} /></div>
        <div><div className="mute" style={{ fontSize: 12, fontWeight: 700, marginBottom: 4 }}>Clusters</div><Chips all={(m?.clusters ?? []).map((l) => [l.id, l.name])} on={f.clusters} set={(v) => setF({ ...f, clusters: v })} /></div>
        <div><div className="mute" style={{ fontSize: 12, fontWeight: 700, marginBottom: 4 }}>Locations</div><Chips all={(m?.locs ?? []).map((l) => [l.id, l.name])} on={f.locs} set={(v) => setF({ ...f, locs: v })} /></div>
        <div><div className="mute" style={{ fontSize: 12, fontWeight: 700, marginBottom: 4 }}>Employment types</div><Chips all={TYPES.map((t) => [t, t])} on={f.types} set={(v) => setF({ ...f, types: v })} /></div>
        <p className="mute" style={{ fontSize: 12, margin: 0 }}>A person must match every group you pick from (being in any one of the chosen cities, clusters or locations is enough). Leave a group empty to include all of it.</p>
        <div><div className="mute" style={{ fontSize: 12, fontWeight: 700, marginBottom: 4 }}>Named employees (on top of the groups)</div>
          <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}><PersonPicker value={pe?.id ?? ''} selected={pe} onChange={(_, x) => setPe(x)} search="&status=Active" placeholder="Search employee" style={{ minWidth: 260 }} />
            <button type="button" className="btn sm" onClick={() => { if (pe && !f.emps.includes(pe.id)) setF({ ...f, emps: [...f.emps, pe.id] }); setPe(null); }}>Add</button></div>
          {f.emps.length > 0 && <div className="chips" style={{ marginTop: 6 }}>{f.emps.map((id) => <button key={id} type="button" className="on" title="Remove" onClick={() => setF({ ...f, emps: f.emps.filter((x) => x !== id) })}>{id} ✕</button>)}</div>}</div>
      </div>}
      <label className="row" style={{ marginTop: 12, fontSize: 13.5, gap: 8 }}><input type="checkbox" checked={f.joiners} onChange={(e) => setF({ ...f, joiners: e.target.checked })} />New joiners must sign it too</label>
    </>}
    <div className="row" style={{ marginTop: 16, justifyContent: 'flex-end' }}>
      <button className="btn" onClick={onClose}>Cancel</button>
      {mode === 'add' && <button className="btn" disabled={busy} onClick={() => save(false)}>Save as draft</button>}
      <button className="btn primary" disabled={busy} onClick={() => save(mode === 'add')}>{mode === 'add' ? 'Publish' : mode === 'version' ? 'Publish version' : 'Save'}</button>
    </div>
  </Modal>;
}
