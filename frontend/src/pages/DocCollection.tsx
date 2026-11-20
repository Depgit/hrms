import { useState } from 'react';
import { api, fmtD, postForm } from '../api';
import { useAuth } from '../auth';
import { Drawer, Loading, Modal, Pill, Switch, download, toast, useLoad } from '../components/ui';
import { DocTable, WindowNote, daysText, type DocDetail, type DocItem } from './MyDocuments';

// Employees › Document collection (backend doc_collect.go): existing employees (imported or added directly) who
// upload their missing documents within a window. HR Admin and the HRBP in scope verify, send back, extend, remind,
// upload on the employee's behalf and export; roles that may view employee documents read.

type Row = {
  id: string; name: string; department: string; location: string; start: string; due: string; daysLeft: number; overdueDays: number; extraDays: number;
  status: string; missingDocs: string[]; mandatory: number; done: number; verified: number; missing: number; toCheck: number; returned: number;
};
type Tracker = { rows: Row[]; totals: Record<string, number>; settings: { days: number; on: boolean }; canEdit: boolean };

const FILTERS: [string, string][] = [['all', 'All'], ['overdue', 'Overdue'], ['due7', 'Due in 7 days'], ['tocheck', 'To check'], ['complete', 'Complete']];
const STATUS: Record<string, 'ok' | 'warn' | 'bad' | 'info' | 'n'> = { Complete: 'ok', 'To check': 'info', Overdue: 'bad', Open: 'warn' };

export default function DocCollection({ initialFilter }: { initialFilter?: string }) {
  const { me } = useAuth();
  const [filter, setFilter] = useState(FILTERS.some(([k]) => k === initialFilter) ? initialFilter! : 'all');
  const [q, setQ] = useState('');
  const [sel, setSel] = useState<Set<string>>(new Set());
  const [open, setOpen] = useState('');
  const [extend, setExtend] = useState(false);
  const { data, error, reload } = useLoad(() => api<Tracker>(`/doc-collection?filter=${filter}`), [filter, me?.ctx.role]);
  if (!data) return <Loading error={error} />;
  const ql = q.trim().toLowerCase();
  const rows = data.rows.filter((r) => !ql || r.name.toLowerCase().includes(ql) || r.id.toLowerCase().includes(ql));
  const ids = [...sel].filter((id) => data.rows.some((r) => r.id === id));
  const all = rows.length > 0 && rows.every((r) => sel.has(r.id));
  const toggle = (id: string) => setSel((s) => { const n = new Set(s); if (n.has(id)) n.delete(id); else n.add(id); return n; });
  const remind = async () => {
    try {
      const r = await api<{ sent: string[]; skipped: { id: string; reason: string }[] }>('/doc-collection/remind', { body: { ids } });
      toast(`Reminder sent to ${r.sent.length}${r.skipped.length ? ` · skipped ${r.skipped.length}: ${r.skipped.map((x) => `${x.id} (${x.reason})`).join(', ')}` : ''}`);
      setSel(new Set()); reload();
    } catch (x: any) { toast(x.message); }
  };
  return <>
    <div className="note" style={{ marginBottom: 14 }}>
      Employees who were imported or added directly (not through onboarding) upload their missing documents from <b>My view › My documents</b> within <b>{data.settings.days} days</b>{data.settings.on ? '' : ' (switched off: change it in Onboarding › Form setup)'}. They are reminded when the window opens and at 30, 15, 7 and 1 days left; their HRBP hears when it ends with documents missing.
    </div>
    <div className="card">
      <div className="row sp" style={{ marginBottom: 12, flexWrap: 'wrap', gap: 10 }}>
        <span className="seg" aria-label="Filter">{FILTERS.map(([k, l]) => <button key={k} className={filter === k ? 'on' : ''} onClick={() => { setFilter(k); setSel(new Set()); }}>{l}<b>{data.totals[k] ?? 0}</b></button>)}</span>
        <span className="row">
          <input type="text" placeholder="Search name or employee ID" value={q} onChange={(e) => setQ(e.target.value)} style={{ maxWidth: 240 }} />
          {data.canEdit && <><button className="btn" disabled={!ids.length} onClick={() => setExtend(true)}>Extend ({ids.length})</button>
            <button className="btn" disabled={!ids.length} onClick={remind}>Send reminder ({ids.length})</button></>}
          {me?.ctx.canDownload && <button className="btn" onClick={() => download(`/doc-collection/export?filter=${filter}`, 'document-collection.xlsx').catch((e) => toast(e.message))}>Export to Excel</button>}
        </span>
      </div>
      <div className="tw"><table>
        <thead><tr>
          {data.canEdit && <th style={{ width: 28 }}><input type="checkbox" aria-label="Select all" checked={all} onChange={() => setSel(all ? new Set() : new Set(rows.map((r) => r.id)))} /></th>}
          <th>Employee</th><th>Department · Location</th><th>Mandatory uploaded</th><th>To check</th><th>Window</th><th>Status</th>
        </tr></thead>
        <tbody>{rows.map((r) => <tr key={r.id} className="click" onClick={() => setOpen(r.id)}>
          {data.canEdit && <td onClick={(e) => e.stopPropagation()}><input type="checkbox" aria-label={`Select ${r.name}`} checked={sel.has(r.id)} onChange={() => toggle(r.id)} /></td>}
          <td><b>{r.name}</b><div className="mono mute" style={{ fontSize: 12 }}>{r.id}</div></td>
          <td>{r.department}<div className="mute" style={{ fontSize: 12 }}>{r.location}</div></td>
          <td><b>{r.done} of {r.mandatory}</b>{r.returned > 0 && <> <Pill k="bad">{r.returned} sent back</Pill></>}
            {r.missingDocs.length > 0 && <div className="mute" style={{ fontSize: 11.5, maxWidth: 260 }} title={r.missingDocs.join(', ')}>Missing: {r.missingDocs.slice(0, 2).join(', ')}{r.missingDocs.length > 2 ? ` +${r.missingDocs.length - 2} more` : ''}</div>}</td>
          <td>{r.toCheck || '—'}</td>
          <td>{r.daysLeft >= 0 ? `${r.daysLeft} days left` : <span style={{ color: 'var(--bad)', fontWeight: 700 }}>Overdue {r.overdueDays} days</span>}
            <div className="mute" style={{ fontSize: 11.5 }}>until {fmtD(r.due)}{r.extraDays ? ` · +${r.extraDays} days` : ''}</div></td>
          <td><Pill k={STATUS[r.status] ?? 'n'}>{r.status}</Pill></td>
        </tr>)}
          {!rows.length && <tr><td colSpan={7} className="mute">{data.totals.all ? 'Nobody matches this filter.' : 'Nobody is collecting documents: everyone came through onboarding or has every mandatory document.'}</td></tr>}
        </tbody>
      </table></div>
    </div>
    {open && <DocDrawer id={open} onClose={() => { setOpen(''); reload(); }} />}
    {extend && <ExtendModal ids={ids} onClose={(done) => { setExtend(false); if (done) { setSel(new Set()); reload(); } }} />}
  </>;
}

function ExtendModal({ ids, onClose }: { ids: string[]; onClose: (done: boolean) => void }) {
  const [days, setDays] = useState('15');
  const save = async () => {
    try {
      const r = await api<{ done: any[]; skipped: { id: string; reason: string }[] }>('/doc-collection/extend', { body: { ids, days: Number(days) } });
      toast(`Extended for ${r.done.length}${r.skipped.length ? ` · skipped ${r.skipped.length}: ${r.skipped.map((x) => `${x.id} (${x.reason})`).join(', ')}` : ''}`);
      onClose(true);
    } catch (x: any) { toast(x.message); }
  };
  return <Modal onClose={() => onClose(false)}>
    <h3>Extend the upload window</h3>
    <p className="mute" style={{ fontSize: 13 }}>For {ids.length} employee{ids.length === 1 ? '' : 's'}. A window still open moves its last day; one that has ended reopens from today. Each one is told in their bell.</p>
    <label className="field" style={{ maxWidth: 200 }}><span>Extra days</span><input type="number" min={1} max={365} value={days} onChange={(e) => setDays(e.target.value)} aria-label="Extra days" /></label>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}>
      <button className="btn" onClick={() => onClose(false)}>Cancel</button>
      <button className="btn primary" disabled={!(Number(days) >= 1 && Number(days) <= 365)} onClick={save}>Extend by {days || 0} days</button>
    </div>
  </Modal>;
}

/** One employee's documents: view, verify, send back, upload on their behalf. */
function DocDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const { data: d, error, reload } = useLoad(() => api<DocDetail>(`/doc-collection/${encodeURIComponent(id)}`), [id]);
  const [back, setBack] = useState<DocItem | null>(null);
  const upload = async (key: string, f?: File) => {
    if (!f) return;
    try { await postForm(`/doc-collection/${encodeURIComponent(id)}/files`, { docKey: key }, f); toast('Uploaded ' + f.name); reload(); } catch (x: any) { toast(x.message); }
  };
  const verify = async (x: DocItem) => {
    try { await api(`/doc-collection/${encodeURIComponent(id)}/review`, { body: { docKey: x.key, action: 'verify' } }); toast(`${x.label} verified`); reload(); } catch (e: any) { toast(e.message); }
  };
  const review = (x: DocItem) => !d?.canEdit || !x.file ? null : <>
    {x.status !== 'verified' && <button className="btn sm primary" onClick={() => verify(x)}>Verify</button>}
    {x.status !== 'returned' && <button className="btn sm" onClick={() => setBack(x)}>Send back</button>}
  </>;
  return <Drawer onClose={onClose}>
    {!d ? <Loading error={error} /> : <>
      <div className="row sp" style={{ marginBottom: 10 }}>
        <div><h3 style={{ margin: 0 }}>{d.employee.name}</h3><div className="mute" style={{ fontSize: 12.5 }}><span className="mono">{d.employee.id}</span> · {d.employee.type} · {d.employee.department}</div></div>
        <button className="btn sm" onClick={onClose}>Close</button>
      </div>
      <WindowNote d={d} hr />
      <p style={{ margin: '0 0 10px', fontSize: 13 }}><b>{d.counts.done} of {d.counts.mandatory}</b> mandatory uploaded · {d.counts.verified} verified · {d.counts.toCheck} to check{d.window ? ` · ${daysText(d.window)}` : ''}</p>
      {!d.canEdit && <div className="note" style={{ marginBottom: 10 }}>Read only for this role.</div>}
      <DocTable d={d} onUpload={upload} hr review={review} />
    </>}
    {back && <SendBack item={back} id={id} onClose={(done) => { setBack(null); if (done) reload(); }} />}
  </Drawer>;
}

function SendBack({ item, id, onClose }: { item: DocItem; id: string; onClose: (done: boolean) => void }) {
  const [reason, setReason] = useState('');
  const save = async () => {
    try {
      const r = await api<{ windowOpen: boolean }>(`/doc-collection/${encodeURIComponent(id)}/review`, { body: { docKey: item.key, action: 'return', reason } });
      toast(r.windowOpen ? `${item.label} sent back. The employee is told.` : `${item.label} sent back. Upload is closed for them: extend the window so they can upload again.`);
      onClose(true);
    } catch (x: any) { toast(x.message); }
  };
  return <Modal onClose={() => onClose(false)}>
    <h3>Send back {item.label}</h3>
    <p className="mute" style={{ fontSize: 13 }}>The employee sees the reason in My documents and in their bell, and can upload it again.</p>
    <label className="field"><span>Reason</span><textarea rows={3} maxLength={300} value={reason} onChange={(e) => setReason(e.target.value)} aria-label="Reason" placeholder="e.g. The photo is blurred; upload a clear scan" /></label>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}>
      <button className="btn" onClick={() => onClose(false)}>Cancel</button>
      <button className="btn primary" disabled={!reason.trim()} onClick={save}>Send back</button>
    </div>
  </Modal>;
}

/** Onboarding › Form setup: the window's length and the switch (HR Admin). */
export function DocWindowSetting() {
  const { can } = useAuth();
  const { data, reload } = useLoad(() => api<{ days: number; on: boolean }>('/doc-collection/settings'), []);
  const [days, setDays] = useState<string | null>(null);
  const edit = can('onb.setup', 2);
  if (!data) return null;
  const save = async (b: { days: number; on: boolean }) => {
    try { await api('/doc-collection/settings', { method: 'PUT', body: b }); toast('Saved'); setDays(null); reload(); } catch (x: any) { toast(x.message); }
  };
  const d = days ?? String(data.days);
  return <div className="card" style={{ marginBottom: 14 }}><h3>Documents from existing employees</h3>
    <div className="row" style={{ alignItems: 'flex-end', gap: 14, flexWrap: 'wrap' }}>
      <label className="field" style={{ margin: 0 }}><span>Collect documents</span><Switch on={data.on} disabled={!edit} label="Collect documents from existing employees" onChange={() => save({ days: data.days, on: !data.on })} /></label>
      <label className="field" style={{ margin: 0, maxWidth: 160 }}><span>Days to upload</span><input type="number" min={1} max={365} disabled={!edit} value={d} onChange={(e) => setDays(e.target.value)} aria-label="Days to upload" /></label>
      {edit && days !== null && <button className="btn primary" onClick={() => save({ days: Number(d), on: data.on })}>Save</button>}
    </div>
    <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 0' }}>Employees who were imported or added directly, not through onboarding, and miss a mandatory document of the list below upload it themselves from My view › My documents within this many days. The days count from when collection started for them (an import: the import date). Follow them in Employees › Document collection.</p>
  </div>;
}
