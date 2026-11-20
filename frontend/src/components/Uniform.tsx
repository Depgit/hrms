import { useState } from 'react';
import { createPortal } from 'react-dom';
import { api, fmtD, inr } from '../api';
import { Loading, Modal, Pill, toast, useLoad } from './ui';

/* Uniform sizes and the uniform recovery and refund of one employee (uniform.go): the block on the employee record and the
   payroll file, and the issue list My view shows. */

export const SIZE_KEYS = ['shirt', 'waist', 'length', 'shoe'] as const;
export type SizeKey = (typeof SIZE_KEYS)[number];
export const SIZE_LABEL: Record<SizeKey, string> = { shirt: 'Shirt / T-shirt', waist: 'Trouser waist', length: 'Trouser length', shoe: 'Shoe size' };
/** "32 in" for a trouser size, "UK 8" for shoes. */
export const sizeText = (k: SizeKey, v?: string) => (!v ? '' : k === 'waist' || k === 'length' ? `${v} in` : k === 'shoe' ? `UK ${v}` : v);

export type UniLine = { itemId: number; name: string; qty: number; price: number; ret?: number; size?: string };
export type UniIssue = {
  id: string; employeeId: string; name?: string; date: string; lines: UniLine[]; total: number; recover: boolean;
  schedule: { ym: string; amount: number; before?: boolean }[]; refundYm: string | null; refundStatus: 'scheduled' | 'paid' | 'cancelled' | 'fnf' | 'none';
  refundNote?: string | null; refundRule?: string | null; recovered: number; toRecover: number;
  /** Issued before the HRMS (Bulk update › Opening uniform): no items or store; issuedYm is its month, note what the file said. */
  opening?: boolean; issuedYm?: string; note?: string | null;
};

const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 15).toLocaleString('en-IN', { month: 'long', year: 'numeric' });
/** "2 × Shirt (M), 1 × Trousers (32/30)". */
export const linesText = (l: UniLine[]) => l.map((x) => `${x.qty} × ${x.name}${x.size ? ` (${x.size})` : ''}`).join(', ');
/** The items of an issue; an opening issue has none, only the note given when it was loaded. */
export const issueItems = (u: UniIssue) => (u.opening ? <><Pill k="info">Opening balance</Pill>{u.note ? <div>{u.note}</div> : null}</> : linesText(u.lines));
/** When an issue was made: its day, or the month of an opening issue. */
export const issueWhen = (u: UniIssue) => (u.opening && u.issuedYm ? ymLabel(u.issuedYm) : fmtD(u.date));

/** The refund of an issue in words, with its state. */
export function RefundCell({ u }: { u: UniIssue }) {
  if (!u.recover || u.refundStatus === 'none') return <span className="mute">No recovery, no refund</span>;
  const when = u.refundYm ? ymLabel(u.refundYm) : '';
  if (u.refundStatus === 'cancelled') return <><Pill k="bad">Not refunded</Pill><div className="mute" style={{ fontSize: 12 }}>Left before {when}. The recovery stands.</div></>;
  if (u.refundStatus === 'fnf') return <><Pill k="n">In full and final</Pill><div className="mute" style={{ fontSize: 12 }}>Was due {when}</div></>;
  return <><Pill k={u.refundStatus === 'paid' ? 'ok' : 'info'}>{u.refundStatus === 'paid' ? 'Refunded' : 'Expected'}</Pill> {inr(u.total)} in {when}
    {u.refundRule && <div className="mute" style={{ fontSize: 12 }}>Refund {u.refundRule}, if still employed then</div>}</>;
}

/** The recovery of an issue: the months, and what is left. */
export function RecoveryCell({ u }: { u: UniIssue }) {
  if (!u.recover) return <span className="mute">Not recovered</span>;
  return <div style={{ fontSize: 12.5 }}>{u.schedule.map((s) => <div key={s.ym}>{ymLabel(s.ym)}: {inr(s.amount)}{s.before && <span className="mute"> · recovered before HRMS</span>}</div>)}
    <div className="mute">{u.toRecover > 0 ? `${inr(u.toRecover)} still to recover` : 'Fully recovered'}</div></div>;
}

/** Uniform issued, with sizes, recovery and refund (newest first). */
export function UniformIssues({ issues, empty = 'No uniform issued yet.' }: { issues: UniIssue[]; empty?: string }) {
  if (!issues.length) return <p className="mute" style={{ margin: 0 }}>{empty}</p>;
  return <div className="tw"><table><thead><tr><th>Issued</th><th>Items and sizes</th><th className="num">Value</th><th>Salary recovery</th><th>Refund</th></tr></thead><tbody>
    {issues.map((u) => <tr key={u.id}><td>{issueWhen(u)}<div className="mono mute" style={{ fontSize: 11.5 }}>{u.id}</div></td>
      <td className="ink2" style={{ fontSize: 12.5 }}>{issueItems(u)}</td><td className="num">{inr(u.total)}</td>
      <td><RecoveryCell u={u} /></td><td style={{ fontSize: 12.5 }}><RefundCell u={u} /></td></tr>)}
  </tbody></table></div>;
}

/** The "Uniform sizes" block: the sizes on record (editable by whoever edits employee details or manages inventory) and the
 *  uniform issued with its recovery and expected refund. */
export function UniformBlock({ id, heading = true }: { id: string; heading?: boolean }) {
  const d = useLoad(() => api(`/employees/${encodeURIComponent(id)}/uniform`), [id]);
  const [edit, setEdit] = useState(false);
  if (!d.data) return d.error ? null : <Loading />;
  const x = d.data, sz = x.sizes as Record<SizeKey, string>;
  const none = SIZE_KEYS.every((k) => !sz[k]);
  return <div data-testid="uniform-block">
    {heading && <h3 style={{ marginTop: 18 }}>Uniform sizes</h3>}
    <div className="row sp" style={{ alignItems: 'flex-start', gap: 10, flexWrap: 'wrap' }}>
      <dl className="kv" style={{ margin: 0, flex: '1 1 260px' }}>
        {SIZE_KEYS.map((k) => <span key={k} style={{ display: 'contents' }}><dt>{SIZE_LABEL[k]}</dt><dd>{sz[k] ? sizeText(k, sz[k]) : <span className="mute">Not recorded</span>}</dd></span>)}
      </dl>
      {x.canEdit && <button className="btn sm" onClick={() => setEdit(true)}>{none ? 'Add sizes' : 'Edit sizes'}</button>}
    </div>
    {x.updated && <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>Last changed by {x.updated.by}, {x.updated.at}.</p>}
    {none && <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>No sizes on record yet. They are used to issue uniform in the right size and to order stock.</p>}
    {(x.issues as UniIssue[]).length > 0 && <><h3 style={{ margin: '14px 0 8px', fontSize: 14 }}>Uniform issued</h3><UniformIssues issues={x.issues} />
      <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>Uniform for permanent staff is recovered from salary ({x.firstPct}% in the issue month, the rest the next month) and refunded in full {x.rule}, only if still employed then. A return does not change the recovery or the refund.
        {(x.issues as UniIssue[]).some((u) => u.opening) && ' Uniform issued before the HRMS (Opening balance) follows the instalments it was loaded with; the months before the HRMS were recovered under the old system.'}</p></>}
    {/* At the page root, so a drawer's own styles (.ov.right .dlg) do not apply to the dialog. */}
    {edit && createPortal(<SizesModal x={x} onClose={() => setEdit(false)} onDone={() => { setEdit(false); d.reload(); }} />, document.body)}
  </div>;
}

/** Edit an employee's sizes: one drop-down per size, from the lists in Employee dues › Setup. */
function SizesModal({ x, onClose, onDone }: { x: any; onClose: () => void; onDone: () => void }) {
  const [v, setV] = useState<Record<SizeKey, string>>({ ...x.sizes });
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    try {
      const r = await api(`/employees/${encodeURIComponent(x.employee.id)}/uniform`, { method: 'PUT', body: v });
      toast(r.changes?.length ? 'Uniform sizes saved' : 'Nothing changed'); onDone();
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const opts = (k: SizeKey) => { const l: string[] = x.lists[k] ?? []; return x.sizes[k] && !l.includes(x.sizes[k]) ? [x.sizes[k], ...l] : l; };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Uniform sizes: {x.employee.name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form" style={{ marginTop: 10 }}>
      {SIZE_KEYS.map((k) => <label key={k} className="field"><span>{SIZE_LABEL[k]}{k === 'waist' || k === 'length' ? ' (inches)' : k === 'shoe' ? ' (UK)' : ''}</span>
        <select value={v[k] ?? ''} onChange={(e) => setV({ ...v, [k]: e.target.value })}><option value="">Not recorded</option>{opts(k).map((o) => <option key={o} value={o}>{sizeText(k, o)}</option>)}</select></label>)}
    </div>
    <p className="mute" style={{ fontSize: 12 }}>The size lists are kept by HR Admin in Employee dues › Setup. Every change is kept in the change history.</p>
    <div className="row" style={{ marginTop: 12, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={busy} onClick={save}>Save sizes</button></div>
  </Modal>;
}
