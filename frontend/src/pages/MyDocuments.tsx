import { useNavigate } from 'react-router-dom';
import { api, fmtD, openDoc, postForm } from '../api';
import { useAuth } from '../auth';
import { Loading, Pill, toast, useLoad } from '../components/ui';

// Documents from existing employees (backend doc_collect.go). An employee who was imported or added directly
// uploads the documents of Form setup's list from My view › My documents while their window is open (90 days by
// default); HR checks each one (Employees › Document collection) and may send it back with a reason.

export type DocWin = { start: string; due: string; daysLeft: number; overdueDays: number; open: boolean; extraDays: number; source: string };
export type DocItem = {
  key: string; label: string; mandatory: boolean; byHR: boolean; status: 'missing' | 'uploaded' | 'verified' | 'returned';
  file: { id: string; name: string; uploadedAt: string; uploadedBy: string; byHR: boolean } | null;
  reason?: string; reviewedBy?: string; reviewedAt?: string; canUpload: boolean;
};
export type DocCounts = { mandatory: number; done: number; verified: number; missing: number; toCheck: number; returned: number };
export type DocDetail = { employee: { id: string; name: string; type: string; department: string }; window: DocWin | null; docs: DocItem[]; counts: DocCounts; status: string; settings: { days: number; on: boolean }; canEdit?: boolean };

const ACCEPT = '.pdf,.jpg,.jpeg,.png,application/pdf,image/jpeg,image/png';

export const docStatusPill = (d: DocItem) => d.status === 'verified' ? <Pill k="ok">Verified</Pill>
  : d.status === 'uploaded' ? <Pill k="info">Uploaded, waiting for HR check</Pill>
  : d.status === 'returned' ? <Pill k="bad">Sent back</Pill>
  : <Pill k={d.mandatory ? 'warn' : 'n'}>Missing</Pill>;

/** "45 days left", "Last day today" or "Closed 3 days ago". */
export const daysText = (w: DocWin) => w.daysLeft > 1 ? `${w.daysLeft} days left` : w.daysLeft === 1 ? '1 day left' : w.daysLeft === 0 ? 'Last day today' : `Overdue ${w.overdueDays} day${w.overdueDays === 1 ? '' : 's'}`;

/** The list of documents with their state; upload / replace where allowed. Shared by My documents and HR's view. */
export function DocTable({ d, onUpload, hr, review }: { d: DocDetail; onUpload: (key: string, f?: File) => void; hr?: boolean; review?: (x: DocItem) => JSX.Element | null }) {
  return <div className="tw"><table>
    <thead><tr><th>Document</th><th>Status</th><th>File</th><th></th></tr></thead>
    <tbody>{d.docs.map((x) => <tr key={x.key}>
      <td><b>{x.label}</b> {x.mandatory ? <Pill k="bad">Mandatory</Pill> : <Pill k="n">Optional</Pill>}
        {x.byHR && !hr && <div className="mute" style={{ fontSize: 11.5 }}>HR adds this one to your record</div>}</td>
      <td>{docStatusPill(x)}
        {x.status === 'returned' && x.reason && <div style={{ fontSize: 12.5, marginTop: 4 }}><b>Reason:</b> {x.reason}</div>}
        {x.reviewedBy && x.status !== 'uploaded' && <div className="mute" style={{ fontSize: 11.5 }}>by {x.reviewedBy}</div>}</td>
      <td>{x.file ? <div>
        <button className="btn sm" onClick={() => openDoc(x.file!.id).catch((e) => toast(e.message))}>View</button>{' '}
        <span className="mute" style={{ fontSize: 12 }}>{x.file.name}</span>
        <div className="mute" style={{ fontSize: 11.5 }}>{fmtD(x.file.uploadedAt)}{x.file.byHR ? ' · Uploaded by HR' : ''}</div>
      </div> : <span className="mute">—</span>}</td>
      <td><div className="upl">
        {x.canUpload && <label className="btn sm">{x.file ? 'Replace' : 'Upload'}<input type="file" accept={ACCEPT} hidden aria-label={`Upload ${x.label}`}
          onChange={(e) => { onUpload(x.key, e.target.files?.[0]); e.target.value = ''; }} /></label>}
        {review?.(x)}
      </div></td>
    </tr>)}
      {!d.docs.length && <tr><td colSpan={4} className="mute">Form setup asks no documents for your employment type and department.</td></tr>}
    </tbody>
  </table></div>;
}

/** A short line about the window. */
export function WindowNote({ d, hr }: { d: DocDetail; hr?: boolean }) {
  const w = d.window;
  if (!w) return <div className="note" style={{ marginBottom: 14 }}>{hr ? 'No upload window: this person joined through onboarding, or had every mandatory document when collection started.' : 'HR keeps your documents. If one is missing, ask HR to add it.'}</div>;
  if (!d.settings.on) return <div className="note" style={{ marginBottom: 14 }}>Document collection is switched off for now. Ask HR if a document is missing.</div>;
  if (w.open) return <div className="note" style={{ marginBottom: 14 }}>{hr ? 'Self-upload is open' : 'Please upload your documents'} until <b>{fmtD(w.due)}</b> ({daysText(w)}). Accepted: PDF, JPG or PNG, up to 5 MB. HR checks each document; one sent back can be uploaded again.{w.extraDays > 0 ? ` HR has given ${w.extraDays} extra day${w.extraDays === 1 ? '' : 's'}.` : ''}</div>;
  return <div className="note bad" style={{ marginBottom: 14 }}>Self-upload closed on <b>{fmtD(w.due)}</b>{d.counts.missing > 0 ? ` with ${d.counts.missing} mandatory document${d.counts.missing === 1 ? '' : 's'} missing` : ''}. {hr ? 'Extend the window to let them upload again.' : 'Ask HR to reopen it.'}</div>;
}

export default function MyDocuments() {
  const { me } = useAuth();
  const { data: d, error, reload } = useLoad(() => api<DocDetail>('/me/documents'), [me?.ctx.employeeId]);
  if (!d) return <Loading error={error} />;
  const upload = async (key: string, f?: File) => {
    if (!f) return;
    try { await postForm('/me/documents', { docKey: key }, f); toast('Uploaded ' + f.name); reload(); } catch (x: any) { toast(x.message); }
  };
  const c = d.counts;
  return <>
    <WindowNote d={d} />
    <div className="grid g4" style={{ marginBottom: 14 }}>
      <div className="card stat"><div className="l">Mandatory uploaded</div><div className="n">{c.done} of {c.mandatory}</div><div className="s">{c.verified} verified by HR</div></div>
      <div className="card stat"><div className="l">Waiting for HR check</div><div className="n">{c.toCheck}</div><div className="s">HR verifies or sends back</div></div>
      <div className="card stat"><div className="l">Sent back</div><div className="n">{c.returned}</div><div className="s">{c.returned ? 'See the reason and upload again' : 'Nothing to redo'}</div></div>
      <div className="card stat"><div className="l">Days left</div><div className="n">{d.window ? (d.window.open ? Math.max(d.window.daysLeft, 0) : 'Closed') : '—'}</div><div className="s">{d.window ? `Last day ${fmtD(d.window.due)}` : 'No upload window'}</div></div>
    </div>
    <div className="card"><h3>My documents</h3><DocTable d={d} onUpload={upload} /></div>
  </>;
}

/** My view dashboard: shown while the window is open and something mandatory is missing or was sent back. */
export function MyDocsCard() {
  const nav = useNavigate();
  const { me } = useAuth();
  const { data: d } = useLoad(() => api<DocDetail>('/me/documents').catch(() => null), [me?.ctx.employeeId]);
  if (!d?.window || !d.window.open || (d.counts.missing === 0 && d.counts.returned === 0)) return null;
  return <div className="card" style={{ marginTop: 14 }}>
    <div className="row sp" style={{ flexWrap: 'wrap', gap: 10 }}>
      <div><h3 style={{ margin: 0 }}>Upload your documents</h3>
        <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>{d.counts.done} of {d.counts.mandatory} mandatory uploaded{d.counts.returned ? ` · ${d.counts.returned} sent back by HR` : ''} · {daysText(d.window)} (until {fmtD(d.window.due)})</div></div>
      <button className="btn primary" onClick={() => nav('/my-documents')}>Open My documents</button>
    </div>
  </div>;
}
