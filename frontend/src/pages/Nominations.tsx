import { useState } from 'react';
import { api, fmtD, openDoc, today, uploadDoc } from '../api';
import { useAuth } from '../auth';
import { Loading, Modal, Pill, Switch, toast, useLoad } from '../components/ui';

/** `on` false: the document is switched off (kept for earlier submissions, not asked). */
type Doc = { id: string; label: string; per: 'nominee' | 'employee'; req: boolean; on?: boolean };
/** `verify` false: final when the employee sends it (no Payroll check). */
type Scheme = { id: number; name: string; form: string; note: string; on: boolean; types: string[]; mandatory: boolean; max: number; shares: boolean; relations: string[]; guardian: boolean; dueDays: number; docs: Doc[]; verify?: boolean };
const docOn = (d: Doc) => d.on !== false;
type Person = { name: string; rel: string; dob: string; share: number | ''; guardian: string };
const RELATIONS = ['Spouse', 'Son', 'Daughter', 'Father', 'Mother', 'Brother', 'Sister', 'Other'];
const ETYPES = ['Permanent', 'Retainer', 'Contractual'];
const blank = (): Person => ({ name: '', rel: '', dob: '', share: '', guardian: '' });

// The employee's state of one nomination as the prototype words it (nmState).
const statePill = (x: any) => {
  const [l, k] = x.state === 'Verified' ? ['Verified', 'ok'] : x.state === 'Returned' ? ['Returned to you', 'bad'] : x.state === 'Waiting for Payroll' ? ['Waiting for Payroll', 'warn'] : ['Not submitted', x.due && x.due < today() ? 'bad' : 'warn'];
  return <Pill k={k as 'ok' | 'bad' | 'warn'}>{l}</Pill>;
};

/** Employee view (prototype vNomEss): one card per nomination that applies, with the form to submit. */
export function MyNominations() {
  const { me } = useAuth();
  const { data, error, reload } = useLoad(() => api<any[]>('/me/nominations'), []);
  const emp = useLoad(() => (data && !data.length ? api(`/employees/${me!.ctx.employeeId}`) : Promise.resolve(null)), [data?.length]);
  const [open, setOpen] = useState<any>(null);
  if (error) return <div className="note bad">{error}</div>;
  if (!data) return <Loading error={error} />;
  if (!data.length) return <div className="card"><h3>Nominations</h3><p className="mute" style={{ margin: 0 }}>No nominations are needed for {emp.data?.type ? emp.data.type.toLowerCase() + ' ' : ''}staff.</p></div>;
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Add your nominees within the time shown. Payroll checks the documents and verifies. You can change a nomination later. The earlier one stays on record until the new one is verified.</div>
    {data.map((x) => {
      const s: Scheme = x.scheme, c = x.current, waiting = x.state === 'Waiting for Payroll';
      return <div key={s.id} className="card" style={{ marginBottom: 14 }}>
        <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}>
          <div><h3 style={{ margin: 0 }}>{s.name}</h3><div className="mute" style={{ fontSize: 12.5 }}>{s.note} {s.mandatory ? 'Required' : 'Optional'}{x.due ? ', due ' + fmtD(x.due) : ''}.</div></div>
          <div className="row" style={{ gap: 8 }}>{statePill(x)}
            <button className={`btn ${c ? '' : 'primary'}`} disabled={waiting} title={waiting ? 'Waiting for Payroll. You can change it after it is verified or returned.' : undefined} onClick={() => setOpen(x)}>{c ? 'Change' : 'Add nominees'}</button></div>
        </div>
        {c && <>
          <div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Name</th><th>Relation</th><th>Date of birth</th>{s.shares && <th className="num">Share</th>}<th>Guardian</th></tr></thead><tbody>
            {c.people.map((p: any, i: number) => <tr key={i}><td>{p.name}</td><td>{p.rel}</td><td>{fmtD(p.dob)}</td>{s.shares && <td className="num">{p.share}%</td>}<td>{p.guardian || '—'}</td></tr>)}
          </tbody></table></div>
          {c.status === 'Returned' && <div className="note bad" style={{ marginTop: 10 }}>HR returned this: {c.note || ''}. Correct it and send again.</div>}
          <div className="mute" style={{ fontSize: 12, marginTop: 6 }}>Sent {fmtD(c.submitted_at)}{c.status === 'Verified' && c.decided_at ? ', verified ' + fmtD(c.decided_at) : ''}</div>
        </>}
      </div>;
    })}
    {open && <NominationForm emp={me!.ctx.employeeId} st={open} onClose={() => { setOpen(null); reload(); }} />}
  </>;
}

/** The nomination form (prototype nmEdit / nmForm). Files upload as soon as they are picked. */
function NominationForm({ emp, st, onClose }: { emp: string; st: any; onClose: () => void }) {
  const s: Scheme = st.scheme;
  const owner = `${emp}/${s.id}`;
  const files = useLoad(() => api<Record<string, any>>(`/files?ownerKind=nomination&ownerId=${encodeURIComponent(owner)}`), []);
  const [rows, setRows] = useState<Person[]>(st.current?.people?.map((p: any) => ({ ...p, share: p.share || '' })) ?? [blank()]);
  const [err, setErr] = useState<string[]>([]);
  const set = (i: number, k: keyof Person, v: string) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: v } : r)));
  const up = async (key: string, f?: File) => {
    if (!f) return;
    try { await uploadDoc('nomination', owner, key, f); files.reload(); toast('Uploaded'); } catch (x: any) { toast(x.message); }
  };
  const docField = (key: string, d: Doc) => {
    const have = files.data?.[key];
    return <label key={key} className="field"><span>{d.label}{d.req ? ' *' : ''}</span>
      <input type="file" accept=".pdf,.jpg,.jpeg,.png" onChange={(e) => { up(key, e.target.files?.[0]); e.target.value = ''; }} />
      {have && <small className="mute"><a href="#" onClick={(e) => { e.preventDefault(); openDoc(have.id).catch((x) => toast(x.message)); }}>{have.file_name}</a></small>}</label>;
  };
  const send = async () => {
    setErr([]);
    try { await api(`/me/nominations/${s.id}`, { body: { people: rows.map((r) => ({ ...r, name: r.name.trim(), share: r.share === '' ? 0 : +r.share })) } }); toast(verify ? 'Sent to Payroll for checking' : 'Saved'); onClose(); }
    catch (x: any) { setErr((x.details as string[]) || [x.message]); }
  };
  const nd = s.docs.filter((d) => docOn(d) && d.per === 'nominee'), ed = s.docs.filter((d) => docOn(d) && d.per === 'employee'), verify = s.verify !== false;
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>{s.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 12px' }}>Allowed relations: {s.relations.join(', ')}. Up to {s.max}.</p>
    {rows.map((r, i) => <div key={i} className="card" style={{ marginBottom: 10, background: 'var(--bg2,transparent)' }}>
      <div className="row sp"><b>Person {i + 1}</b>{rows.length > 1 && <button className="btn sm" onClick={() => setRows(rows.filter((_, j) => j !== i))}>Remove</button>}</div>
      <div className="form">
        <label className="field"><span>Full name</span><input value={r.name} onChange={(e) => set(i, 'name', e.target.value)} /></label>
        <label className="field"><span>Relation</span><select value={r.rel} onChange={(e) => set(i, 'rel', e.target.value)}><option value="">Select</option>{s.relations.map((x) => <option key={x}>{x}</option>)}</select></label>
        <label className="field"><span>Date of birth</span><input type="date" max={today()} value={r.dob} onChange={(e) => set(i, 'dob', e.target.value)} /></label>
        {s.shares && <label className="field"><span>Share (%)</span><input type="number" min={1} max={100} value={r.share} onChange={(e) => set(i, 'share', e.target.value)} /></label>}
        {s.guardian && <label className="field"><span>Guardian, if under 18</span><input value={r.guardian} onChange={(e) => set(i, 'guardian', e.target.value)} /></label>}
      </div>
      {nd.length > 0 && <div className="form" style={{ marginTop: 8 }}>{nd.map((d) => docField(`n${i}:${d.id}`, d))}</div>}
    </div>)}
    <div className="row" style={{ margin: '8px 0' }}>{rows.length < s.max ? <button className="btn" onClick={() => setRows([...rows, blank()])}>Add another person</button> : <span className="mute" style={{ fontSize: 12.5 }}>Maximum {s.max} reached</span>}</div>
    {ed.length > 0 && <div className="form">{ed.map((d) => docField(`e:${d.id}`, d))}</div>}
    <p className="mute" style={{ fontSize: 12 }}>* needed. {s.shares ? 'Shares must add up to 100%. ' : ''}{verify ? 'Payroll checks the documents before this is final.' : 'This is final once you send it.'}</p>
    {err.length > 0 && <div className="note bad">{err.map((x) => <div key={x}>{x}</div>)}</div>}
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={send}>{verify ? 'Send to HR' : 'Save'}</button></div>
  </Modal>;
}

/** Payroll's verify queue (prototype nomQueue). Also shown alone under Payroll → Claims and changes. */
function NomQueue({ q, onChange }: { q: { data: any; reload: () => void }; onChange?: () => void }) {
  const { can, me } = useAuth();
  const [note, setNote] = useState<Record<string, string>>({});
  const canE = can('pay.claims', 2), med = can('emp.medical');
  const decide = async (id: string, action: 'verify' | 'return') => {
    if (action === 'return' && !(note[id] || '').trim()) { toast('Write what the employee should correct'); return; }
    try { await api(`/nominations/${id}/${action}`, { body: { note: note[id] || '' } }); toast(action === 'verify' ? 'Verified' : 'Returned to the employee'); q.reload(); onChange?.(); } catch (x: any) { toast(x.message); }
  };
  const w: any[] = q.data.submitted;
  // Documents asked for: each required nominee document once per person, each required employee document once.
  const docCount = (x: any) => {
    let have = 0, need = 0;
    (x.docs as Doc[]).filter((d) => d.req && docOn(d)).forEach((d) => {
      if (d.per === 'employee') { need++; if (x.files?.['e:' + d.id]) have++; }
      else x.people.forEach((_: any, i: number) => { need++; if (x.files?.[`n${i}:${d.id}`]) have++; });
    });
    return [have, need];
  };
  return <div className="card">{!w.length ? <p className="mute" style={{ margin: 0 }}>Nothing is waiting to be verified.</p> : <div className="tw"><table>
    <thead><tr><th>Employee</th><th>Nomination</th><th>People</th><th>Documents</th><th>Sent</th><th></th></tr></thead>
    <tbody>{w.slice(0, 80).map((x) => {
      const [h, n] = docCount(x), mine = x.employee_id === me?.ctx.employeeId;
      return <tr key={x.id}>
        <td>{x.name} <span className="mono mute">{x.employee_id}</span>{x.department && <div className="mute" style={{ fontSize: 12 }}>{x.department}</div>}</td>
        <td>{x.scheme}{x.version != null && <div className="mute" style={{ fontSize: 12 }}>Version {x.version}</div>}</td>
        <td style={{ maxWidth: 260 }}>{med ? x.people.map((p: any) => `${p.name} (${p.rel}${x.shares ? ', ' + p.share + '%' : ''})`).join(', ') : 'Hidden'}</td>
        <td>{h} of {n} {h < n ? <Pill k="bad">Missing</Pill> : <Pill k="ok">Complete</Pill>}
          {Object.values(x.files || {}).length > 0 && <div style={{ fontSize: 12, marginTop: 4 }}>{Object.values(x.files || {}).map((f: any) => <div key={f.id}><a href="#" onClick={(e) => { e.preventDefault(); openDoc(f.id).catch((er) => toast(er.message)); }}>{f.file_name}</a></div>)}</div>}</td>
        <td>{fmtD(x.submitted_at)}</td>
        <td style={{ minWidth: 230 }}>{canE && !mine ? <div className="row" style={{ gap: 6, flexWrap: 'nowrap' }}>
          <input placeholder="Note, needed to return" value={note[x.id] || ''} onChange={(e) => setNote({ ...note, [x.id]: e.target.value })} style={{ minWidth: 130, width: 'auto', flex: 1 }} />
          <button className="btn sm primary" onClick={() => decide(String(x.id), 'verify')}>Verify</button><button className="btn sm" onClick={() => decide(String(x.id), 'return')}>Return</button></div>
          : <span className="mute">—</span>}</td>
      </tr>;
    })}</tbody></table></div>}</div>;
}

/** Nominations under Work policies (prototype vNomAdmin): To verify, Not yet submitted, Nomination policy.
 *  `queueOnly` shows just the verify queue (prototype nomQueue, used under Payroll). */
export function NominationsAdmin({ queueOnly, onChange }: { queueOnly?: boolean; onChange?: () => void } = {}) {
  const { can, me } = useAuth();
  const claims = can('pay.claims');
  const q = useLoad(() => (claims ? api('/nominations') : Promise.resolve(null)), []);
  const schemes = useLoad(() => (queueOnly ? Promise.resolve([] as Scheme[]) : api<Scheme[]>('/nominations/schemes')), []);
  const [v, setV] = useState(claims ? 'verify' : 'policy');
  const [f, setF] = useState({ q: '', sc: '' });
  const [edit, setEdit] = useState<Scheme | null>(null);
  const admin = me?.ctx.role === 'HR Admin';
  if (queueOnly) return !claims ? null : q.data ? <NomQueue q={q} onChange={onChange} /> : <Loading error={q.error} />;
  const seg: [string, string][] = [...(claims ? [['verify', 'To verify'], ['pend', 'Not yet submitted']] as [string, string][] : []), ['policy', 'Nomination policy']];
  const tabs = <div className="row sp" style={{ marginBottom: 14 }}><div className="seg">{seg.map(([k, l]) => <button key={k} className={v === k ? 'on' : ''} onClick={() => setV(k)}>{l}{k === 'verify' && <b>{q.data?.submitted.length ?? 0}</b>}</button>)}</div></div>;
  const toggle = async (s: Scheme) => {
    try { await api(`/nominations/schemes/${s.id}`, { method: 'PUT', body: { ...s, on: !s.on } }); toast(!s.on ? 'Active' : 'Inactive. Existing nominations stay on record'); schemes.reload(); } catch (x: any) { toast(x.message); }
  };
  if (v === 'policy') return <>{tabs}
    <div className="note" style={{ marginBottom: 14 }}>Set which nominations exist, who must give them, how many people and what documents are needed. Employees see this in their own Nominations tab after joining. A nomination is switched off, never deleted.</div>
    <div className="card">{!schemes.data ? <Loading error={schemes.error} /> : <div className="tw"><table>
      <thead><tr><th>Nomination</th><th>Applies to</th><th>Needed</th><th>Due</th><th>People</th><th>Documents</th><th>Status</th><th></th></tr></thead>
      <tbody>{schemes.data.map((s) => <tr key={s.id}>
        <td><b>{s.name}</b><div className="mute" style={{ fontSize: 12 }}>{s.form}</div></td>
        <td>{s.types.join(', ')}</td><td>{s.mandatory ? 'Mandatory' : 'Optional'}</td><td>{s.dueDays} days</td>
        <td>Up to {s.max}{s.shares ? ', total 100%' : ''}</td><td>{s.docs.filter(docOn).length} ({s.docs.filter((d) => docOn(d) && d.req).length} required)</td>
        <td><Pill k={s.on ? 'ok' : 'n'}>{s.on ? 'Active' : 'Inactive'}</Pill></td>
        <td style={{ whiteSpace: 'nowrap' }}><button className="btn sm" onClick={() => setEdit(structuredClone(s))}>{admin ? 'Edit' : 'View'}</button>{admin && <> <button className="btn sm" onClick={() => toggle(s)}>{s.on ? 'Make inactive' : 'Reactivate'}</button></>}</td>
      </tr>)}</tbody></table></div>}
      {admin && <div className="row" style={{ marginTop: 12 }}><button className="btn" onClick={() => setEdit({ id: 0, name: 'New nomination', form: '', note: '', on: true, types: ['Permanent'], mandatory: false, max: 3, shares: true, relations: ['Spouse', 'Son', 'Daughter', 'Father', 'Mother'], guardian: true, dueDays: 30, docs: [{ id: '', label: 'ID proof', per: 'nominee', req: true, on: true }], verify: true })}>New nomination</button></div>}
    </div>
    {edit && <SchemeModal s={edit} all={schemes.data || []} ed={admin} onClose={() => { setEdit(null); schemes.reload(); }} />}
  </>;
  if (!q.data) return <>{tabs}<Loading error={q.error} /></>;
  if (v === 'pend') {
    const all: any[] = q.data.pending.filter((x: any) => (!f.q || `${x.name} ${x.id}`.toLowerCase().includes(f.q.toLowerCase())) && (!f.sc || x.scheme === f.sc));
    const od = all.filter((x) => x.overdue).length;
    const canE = can('pay.claims', 2);
    const remind = async (x: any) => {
      try { await api('/nominations/remind', { body: { employeeId: x.id, schemeId: x.scheme_id } }); toast('Reminder sent'); q.reload(); } catch (e: any) { toast(e.message); }
    };
    return <>{tabs}
      <div className="row" style={{ gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
        <input value={f.q} onChange={(e) => setF({ ...f, q: e.target.value })} placeholder="Search employee" style={{ minWidth: 180, width: 'auto' }} />
        <select value={f.sc} onChange={(e) => setF({ ...f, sc: e.target.value })}><option value="">All nominations</option>{schemes.data?.filter((s) => s.on && s.mandatory).map((s) => <option key={s.id} value={s.name}>{s.name}</option>)}</select>
        <span className="mute" style={{ alignSelf: 'center' }}>{all.length} pending, {od} overdue</span>
      </div>
      <div className="card"><div className="tw"><table><thead><tr><th>Employee</th><th>Nomination</th><th>Joined</th><th>Due</th><th>Status</th><th></th></tr></thead><tbody>
        {all.slice(0, 60).map((x, i) => <tr key={i}><td>{x.name} <span className="mono mute">{x.id}</span>{x.department && <div className="mute" style={{ fontSize: 12 }}>{x.department}</div>}</td><td>{x.scheme}</td><td>{fmtD(x.doj)}</td><td>{x.due ? fmtD(x.due) : '—'}</td>
          <td>{x.state === 'Returned' ? <Pill k="bad">Returned to employee</Pill> : x.overdue ? <Pill k="bad">Overdue</Pill> : <Pill k="warn">Waiting for employee</Pill>}
            {x.reminded && <div className="mute" style={{ fontSize: 12 }}>Reminded {fmtD(x.reminded)}</div>}</td>
          <td>{canE && <button className="btn sm" onClick={() => remind(x)}>Send reminder</button>}</td></tr>)}
        {!all.length && <tr><td colSpan={6} className="mute">Everyone has submitted.</td></tr>}
      </tbody></table></div>
        {all.length > 60 && <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Showing 60 of {all.length}. Search or pick a nomination to narrow it down.</p>}</div>
    </>;
  }
  return <>{tabs}<NomQueue q={q} /></>;
}

/** Nomination policy editor (prototype nsModal). */
function SchemeModal({ s: init, all, ed, onClose }: { s: Scheme; all: Scheme[]; ed: boolean; onClose: () => void }) {
  const [s, setS] = useState(init);
  const [nd, setNd] = useState({ label: '', per: 'nominee' as Doc['per'], req: true });
  const save = async () => {
    if (!s.name.trim()) { toast('Name the nomination'); return; }
    if (!s.types.length) { toast('Pick who it applies to'); return; }
    if (!s.relations.length) { toast('Allow at least one relation'); return; }
    if (!(s.max >= 1)) { toast('Maximum people must be at least 1'); return; }
    if (all.some((x) => x.id !== s.id && x.name.toLowerCase() === s.name.trim().toLowerCase())) { toast('That name already exists'); return; }
    try { await api(s.id ? `/nominations/schemes/${s.id}` : '/nominations/schemes', { method: s.id ? 'PUT' : 'POST', body: s }); toast('Saved. New nominations follow it from now on'); onClose(); } catch (x: any) { toast(x.message); }
  };
  const tog = (k: 'types' | 'relations', v: string) => setS({ ...s, [k]: s[k].includes(v) ? s[k].filter((x) => x !== v) : [...s[k], v] });
  const setDoc = (i: number, d: Partial<Doc>) => setS({ ...s, docs: s.docs.map((x, j) => (j === i ? { ...x, ...d } : x)) });
  const addDoc = () => {
    const l = nd.label.trim();
    if (!l) { toast('Name the document'); return; }
    if (s.docs.some((x) => x.label.toLowerCase() === l.toLowerCase())) { toast('That document is already listed. Switch it on instead'); return; }
    setS({ ...s, docs: [...s.docs, { id: '', label: l, per: nd.per, req: nd.req, on: true }] }); setNd({ ...nd, label: '' });
  };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>{s.name || 'New nomination'}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <fieldset disabled={!ed} style={{ border: 0, padding: 0, margin: '12px 0 0', minWidth: 0 }}>
      <div className="form">
        <label className="field"><span>Name</span><input value={s.name} onChange={(e) => setS({ ...s, name: e.target.value })} /></label>
        <label className="field"><span>Form or reference</span><input value={s.form} onChange={(e) => setS({ ...s, form: e.target.value })} /></label>
        <label className="field" style={{ gridColumn: '1/-1' }}><span>What it is for</span><input value={s.note} onChange={(e) => setS({ ...s, note: e.target.value })} /></label>
        <label className="field"><span>Required or optional</span><select value={s.mandatory ? 'Mandatory' : 'Optional'} onChange={(e) => setS({ ...s, mandatory: e.target.value === 'Mandatory' })}><option>Mandatory</option><option>Optional</option></select></label>
        <label className="field"><span>Due within (days of joining)</span><input type="number" min={0} value={s.dueDays} onChange={(e) => setS({ ...s, dueDays: +e.target.value })} /></label>
        <label className="field"><span>Most people allowed</span><input type="number" min={1} value={s.max} onChange={(e) => setS({ ...s, max: +e.target.value })} /></label>
        <label className="field"><span>Shares</span><select value={s.shares ? '100' : 'none'} onChange={(e) => setS({ ...s, shares: e.target.value === '100' })}><option value="100">Shares must add up to 100%</option><option value="none">No share, a list of people</option></select></label>
        <label className="field"><span>Guardian needed if under 18</span><select value={s.guardian ? '1' : '0'} onChange={(e) => setS({ ...s, guardian: e.target.value === '1' })}><option value="1">Yes</option><option value="0">No</option></select></label>
        <label className="field"><span>HR checks and verifies</span><select value={s.verify === false ? '0' : '1'} onChange={(e) => setS({ ...s, verify: e.target.value === '1' })}><option value="1">Yes</option><option value="0">No, final when sent</option></select></label>
      </div>
      <div style={{ marginTop: 10 }}><b style={{ fontSize: 13 }}>Applies to</b><div className="row" style={{ gap: 14, marginTop: 6 }}>{ETYPES.map((t) => <label key={t}><input type="checkbox" checked={s.types.includes(t)} onChange={() => tog('types', t)} /> {t}</label>)}</div></div>
      <div style={{ marginTop: 10 }}><b style={{ fontSize: 13 }}>Relations allowed</b><div className="row" style={{ gap: 14, marginTop: 6, flexWrap: 'wrap' }}>{RELATIONS.map((t) => <label key={t}><input type="checkbox" checked={s.relations.includes(t)} onChange={() => tog('relations', t)} /> {t}</label>)}</div></div>
      <div style={{ marginTop: 14 }}><b style={{ fontSize: 13 }}>Documents needed to enrol</b>
        <div className="tw" style={{ marginTop: 6 }}><table><thead><tr><th>Document</th><th>From</th><th>Needed</th><th>Active</th></tr></thead><tbody>
          {s.docs.map((d, i) => <tr key={i}><td><input value={d.label} onChange={(e) => setDoc(i, { label: e.target.value })} style={{ minWidth: 200, width: 'auto' }} /></td><td>{d.per === 'nominee' ? 'Each person' : 'Employee'}</td>
            <td><select value={d.req ? '1' : '0'} onChange={(e) => setDoc(i, { req: e.target.value === '1' })}><option value="1">Required</option><option value="0">Optional</option></select></td>
            <td><Switch on={docOn(d)} onChange={() => setDoc(i, { on: !docOn(d) })} /></td></tr>)}
        </tbody></table></div>
        <div className="row" style={{ marginTop: 8, gap: 8, flexWrap: 'wrap' }}>
          <input value={nd.label} onChange={(e) => setNd({ ...nd, label: e.target.value })} placeholder="New document, e.g. Marriage certificate" style={{ minWidth: 220, width: 'auto' }} />
          <select value={nd.per} onChange={(e) => setNd({ ...nd, per: e.target.value as Doc['per'] })}><option value="nominee">From each person</option><option value="employee">From the employee</option></select>
          <select value={nd.req ? '1' : '0'} onChange={(e) => setNd({ ...nd, req: e.target.value === '1' })}><option value="1">Required</option><option value="0">Optional</option></select>
          <button className="btn" onClick={addDoc}>Add document</button></div>
        <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>Documents are switched off, never deleted, so earlier submissions still show what was asked.</p></div>
    </fieldset>
    {ed && <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn primary" onClick={save}>Save</button></div>}
  </Modal>;
}
