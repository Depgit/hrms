import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, fmtD, today } from '../api';
import { useAuth } from '../auth';
import { Chain, Field, Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import { NewRequest } from './Approvals';
import { MyExit } from './Separation';

// My view › Separation: the person's own exit, moved here from My pay and profile. The cards and the
// resignation dialog keep the rules they had there; the case progress is the employee's own case only.
// Approvals is not in My view, so every request about the exit starts here: Start resignation, Withdraw before
// approval, and Withdraw resignation while on notice (the Approvals form and route, with the same rules). The
// requests are followed on the Dashboard › My requests.

type Tone = 'ok' | 'n' | 'info' | 'warn' | 'bad';
const STG: Record<string, [string, Tone]> = {
  'Show-cause': ['Show-cause', 'warn'], Approval: ['Approval', 'info'], Notice: ['Notice period', 'info'], Clearance: ['Clearance', 'warn'],
  'Full and final': ['Full and final', 'warn'], Letters: ['Letters', 'warn'], 'Ready to close': ['Ready to close', 'warn'], Closed: ['Closed', 'ok'],
  Rejected: ['Rejected', 'n'], Withdrawn: ['Withdrawn', 'n'], Cancelled: ['Cancelled', 'n'],
};
const ENDED = ['Rejected', 'Withdrawn', 'Cancelled'];
const GROUP: Record<string, string> = { 'Show-cause': 'approval', Approval: 'approval', Notice: 'notice', Clearance: 'clearance', 'Full and final': 'fnf', Letters: 'letters', 'Ready to close': 'letters', Closed: 'closed' };
const STEPS: [string, string][] = [['start', 'Started'], ['approval', 'Approval'], ['notice', 'Notice'], ['clearance', 'Clearance'], ['fnf', 'Full and final'], ['letters', 'Letters'], ['closed', 'Closed']];
const OWNERS = ['Reporting manager', 'HRBP', 'Finance'];
const dayDiff = (a: string, b: string) => Math.round((new Date(b + 'T00:00:00').getTime() - new Date(a + 'T00:00:00').getTime()) / 864e5);

async function cancel(id: string, done: () => void, msg: string) {
  try { await api(`/approvals/${id}/cancel`, { method: 'POST' }); toast(msg); done(); } catch (x: any) { toast(x.message); }
}

export default function MyExitPage() {
  const { me } = useAuth();
  const sep = useLoad(() => api('/me/separation'), []);
  const resign = useLoad(() => api<any[]>('/me/requests?wf=resign').catch(() => []), []);
  if (!sep.data) return <Loading error={sep.error} />;
  const reload = () => { sep.reload(); resign.reload(); };
  const c = sep.data.case, n = sep.data.notice;
  const reqs: any[] = resign.data ?? [];
  const pending = reqs.find((r) => r.status === 'Pending');
  const open = !!(c && c.open);
  const onNotice = n?.exitStatus === 'On notice';
  const gone = me?.employee.status === 'Inactive';
  return (
    <>
      {open ? <MyExit c={c} onChange={reload} /> : <ExitCard n={n} pending={pending} onChange={reload} />}
      {open && c.stage === 'Approval' && <ApprovalNote c={c} pending={pending} onChange={reload} />}
      {(open || onNotice) && <OnNoticeNote c={c} onNotice={onNotice} reqs={reqs} onChange={reload} />}
      {open && <Progress c={c} />}
      {!gone && <ResignCard sep={sep.data} pending={!!pending} onChange={reload} />}
      {!open && !onNotice && !pending && !n?.canResign && <div className="card"><h3>Separation</h3><p className="mute" style={{ margin: 0 }}>{n?.why || 'You cannot start a resignation now.'}</p></div>}
      {c && !open && <p className="mute" style={{ fontSize: 12.5, margin: '12px 0 0' }}>{c.stage === 'Closed' ? `Your exit case ${c.id} is closed.` : `Your earlier ${String(c.type).toLowerCase()} (${c.id}, ${fmtD(c.givenOn)}) was ${String(STG[c.stage]?.[0] ?? c.stage).toLowerCase()}.`}</p>}
    </>
  );
}

/** While the resignation is waiting, or accepted with no open case: the short "My exit" card (as on My pay before). */
function ExitCard({ n, pending, onChange }: { n: any; pending?: any; onChange: () => void }) {
  if (!n || !(n.exitStatus === 'On notice' || pending)) return null;
  const lwd = pending ? pending.payload.lwd : n.currentLwd;
  return <div className="card" style={{ marginBottom: 14, borderLeft: '4px solid #2a5db0' }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>My exit</h3>{pending ? <Pill k="warn">Waiting for {pending.waiting_on}</Pill> : <Pill k="info">On notice</Pill>}</div>
    <p className="ink2" style={{ fontSize: 13.5, margin: '6px 0' }}>Resignation{lwd ? <> · last working day <b>{fmtD(lwd)}</b></> : null}</p>
    {pending && (!pending.payload.rkind || pending.payload.rkind === 'new') && <button className="btn sm" onClick={() => cancel(pending.id, onChange, 'Resignation withdrawn')}>Withdraw before approval</button>}
  </div>;
}

/** The case as it moves: stage, last working day, approval, clearance by owner, exit interview and full and final. Read only. */
function Progress({ c }: { c: any }) {
  const st: string = c.stage, ended = ENDED.includes(st), req = c.request;
  const order = STEPS.map(([k]) => k);
  const cur = st === 'Ready to close' ? 6 : Math.max(0, order.indexOf(GROUP[st] ?? ''));
  const items: any[] = c.clearance ?? [];
  const clearOpen = !ended && !['Show-cause', 'Approval'].includes(st);
  const secDone = (o: string) => items.filter((i) => i.owner === o).every((i) => i.done);
  const exDone = !!c.exitInterview || items.some((i) => i.auto === 'exit' && i.done);
  const stepIdx: number = req ? (typeof req.stepIndex === 'number' ? req.stepIndex : req.chain.indexOf(req.step)) : -1;
  const fnf = c.fnf;
  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}><h3 style={{ margin: 0 }}>Case progress</h3><span className="mono mute">{c.id}</span></div>
    <div className="row" style={{ gap: 4, flexWrap: 'wrap', margin: '12px 0 14px' }}>{STEPS.map(([k, l], i) => {
      const done = !ended && (i < cur || st === 'Closed'), now = !ended && i === cur && st !== 'Closed';
      return <span key={k} style={{ display: 'inline-flex', alignItems: 'center', gap: 6, padding: '5px 10px', borderRadius: 999, fontSize: 12.5, border: `1px solid ${done ? 'var(--ok,#1a7f4b)' : now ? 'var(--accent,#2a5db0)' : 'var(--line,#ddd)'}`,
        color: done ? 'var(--ok,#1a7f4b)' : now ? 'var(--accent,#2a5db0)' : 'var(--ink2,#666)', fontWeight: now ? 700 : 500 }}>{done ? '✓' : i + 1} {l}</span>;
    })}</div>
    <dl className="kv">
      <dt>Stage</dt><dd><Pill k={STG[st]?.[1] ?? 'n'}>{STG[st]?.[0] ?? st}</Pill>{c.next ? <> · next step: {c.next}</> : null}</dd>
      <dt>{c.type === 'Resignation' ? 'Resigned on' : 'Started on'}</dt><dd>{fmtD(c.givenOn)}{c.reason ? ` · ${c.reason}` : ''}</dd>
      <dt>Last working day</dt><dd>{c.lwd ? <><b>{fmtD(c.lwd)}</b>{st === 'Notice' ? ` · ${dayDiff(today(), c.lwd)} days to go` : ''}</> : '—'}{c.noticeDays ? ` · notice ${c.noticeDays} days` : ''}</dd>
      <dt>Approval</dt><dd>{req ? (req.chain?.length ? <Chain steps={req.chain} step={stepIdx} status={req.status} /> : 'No approver needed') : '—'}{req && req.status !== 'Pending' && <> <span className="mute" style={{ fontSize: 12 }}>{req.status}</span></>}</dd>
      <dt>Exit interview</dt><dd>{!c.exitNeeded ? `Not needed for ${String(c.type).toLowerCase()}` : exDone ? `Done${c.exitInterview?.at ? ' on ' + c.exitInterview.at : ''}` : 'Not done yet'}</dd>
      <dt>Full and final</dt><dd>{fnf ? fnf.status : 'Prepared by Payroll after clearance is complete'}</dd>
    </dl>
    <h3 style={{ margin: '16px 0 8px' }}>Clearance</h3>
    {!clearOpen ? <p className="mute" style={{ margin: 0, fontSize: 13 }}>Clearance opens after your resignation is approved.</p>
      : <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(200px,1fr))', gap: 10 }}>{OWNERS.map((o) => <div key={o} style={{ border: '1px solid var(--line,#ddd)', borderRadius: 10, padding: 10 }}>
        <div className="row sp"><b>{o}</b>{secDone(o) ? <Pill k="ok">Done</Pill> : <Pill k="warn">Open</Pill>}</div>
        <ul style={{ margin: '6px 0 0', paddingLeft: 0, listStyle: 'none', fontSize: 13 }}>{items.filter((i) => i.owner === o).map((it) => <li key={it.id} style={{ margin: '3px 0' }}>
          <span style={{ color: it.done ? 'var(--ok,#1a7f4b)' : 'var(--ink2,#666)' }}>{it.done ? '✓' : '○'}</span> {it.name}</li>)}</ul>
      </div>)}</div>}
    {(c.assets ?? []).length > 0 && clearOpen && <p style={{ fontSize: 13, margin: '10px 0 0' }}>Return before you leave: <b>{c.assets.map((a: any) => a.name).join(', ')}</b>.</p>}
  </div>;
}

const waitingFor = (r: any) => `Waiting for ${r.waiting_on}${r.waiting_name ? ` (${r.waiting_name})` : ''}`;

/** While the resignation waits for approval (the case is open at Approval): who it waits on, and withdrawing it
 *  before approval, which cancels the request (the old Approvals › Cancel request). */
function ApprovalNote({ c, pending, onChange }: { c: any; pending?: any; onChange: () => void }) {
  const nav = useNavigate();
  const req = pending && (!pending.payload.rkind || pending.payload.rkind === 'new') ? pending : null;
  if (!req) return null;
  return <div className="note" style={{ marginBottom: 14 }}>
    Your {String(c.type).toLowerCase()} <span className="mono">{req.id}</span> is waiting for approval · <Pill k="warn">{waitingFor(req)}</Pill>
    {c.type === 'Resignation' && <div className="mute" style={{ marginTop: 4 }}>You can withdraw it until it is approved.</div>}
    <div className="row" style={{ marginTop: 8, gap: 8, flexWrap: 'wrap' }}>
      {c.type === 'Resignation' && <button className="btn sm" onClick={() => confirm('Withdraw your resignation before it is approved?') && cancel(req.id, onChange, 'Resignation withdrawn')}>Withdraw before approval</button>}
      <button className="btn sm" onClick={() => nav('/#my-requests')}>See it in My requests</button></div>
  </div>;
}

/** While on notice: "Withdraw resignation" opens the same form and route as Approvals › New request › Resignation,
 *  with the same rules (only a resignation, only while serving notice, within the policy's withdrawal window).
 *  A change of last working day is raised by HR, as before. */
function OnNoticeNote({ c, onNotice, reqs, onChange }: { c: any; onNotice: boolean; reqs: any[]; onChange: () => void }) {
  const nav = useNavigate();
  const [form, setForm] = useState(false);
  const st: string = c?.stage ?? '';
  if (!onNotice && !['Notice', 'Clearance'].includes(st)) return null;
  const open = reqs.filter((r) => r.status === 'Pending' && (r.payload.rkind === 'withdraw' || r.payload.rkind === 'change'));
  const isResign = !c || c.type === 'Resignation';
  const late = !!c?.withdrawBy && today() > c.withdrawBy;
  const can = isResign && (!c || st === 'Notice') && !late && !open.some((r) => r.payload.rkind === 'withdraw');
  return <div className="note" style={{ marginBottom: 14 }}>
    {!isResign ? <>Only a resignation can be withdrawn. Ask your HRBP about your {String(c.type).toLowerCase()}.</>
      : late ? <>Your policy allowed withdrawing the resignation until {fmtD(c.withdrawBy)}, so it can no longer be withdrawn.</>
        : st && st !== 'Notice' ? <>Your clearance has started, so the resignation can no longer be withdrawn.</>
          : open.some((r) => r.payload.rkind === 'withdraw') ? <>Your request to withdraw the resignation is with the same approvers as your resignation. Until it is approved, your notice continues.</>
            : <>Changed your mind? <b>Withdraw resignation</b> goes to the same approvers as your resignation{c?.withdrawBy ? `. Your policy allows it until ${fmtD(c.withdrawBy)}` : ''}.</>}
    {' '}A <b>Change last working day</b> request is raised by HR, so ask your HRBP if you need a different date.
    {open.map((r) => <div key={r.id} className="row" style={{ marginTop: 6, gap: 8, flexWrap: 'wrap' }}><span><span className="mono">{r.id}</span> {r.payload.rkind === 'withdraw' ? 'Withdraw resignation' : 'Change last working day'}</span><Pill k="warn">{waitingFor(r)}</Pill>
      {r.payload.rkind === 'withdraw' && <button className="btn sm" onClick={() => cancel(r.id, onChange, 'Withdrawal cancelled. Your resignation stands')}>Cancel request</button>}</div>)}
    <div className="row" style={{ marginTop: 8, gap: 8, flexWrap: 'wrap' }}>
      {can && <button className="btn sm primary" onClick={() => setForm(true)}>Withdraw resignation</button>}
      {open.length > 0 && <button className="btn sm" onClick={() => nav('/#my-requests')}>See it in My requests</button>}</div>
    {form && <NewRequest wf="resign" title="Withdraw resignation" onClose={() => setForm(false)} onDone={() => { setForm(false); onChange(); }} />}
  </div>;
}

/** "Start resignation" while the person can resign (same dialog and rules as on My pay before). */
function ResignCard({ sep, pending, onChange }: { sep: any; pending?: boolean; onChange: () => void }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState('');
  const [note, setNote] = useState('');
  const n = sep?.notice;
  if (!n || !n.canResign || pending || (sep.case && sep.case.open) || n.exitStatus === 'On notice') return null;
  const reasons: string[] = sep.reasons ?? [];
  const submit = async () => {
    try {
      await api('/approvals', { body: { wf: 'resign', payload: { reason: reason || reasons[0], note: note.trim() } } });
      const s2 = await api<any>('/me/separation').catch(() => null), q = s2?.case?.request;
      toast(`Submitted. Last working day ${fmtD(q?.lwd || n.lwd)}, waiting for ${q?.chain?.[0] || 'approval'}`); setOpen(false); onChange();
    } catch (x: any) { toast(x.message); }
  };
  const ro = { background: 'var(--bg,#f4f5f6)' };
  return (
    <>
      <div className="card">
        <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}><div><h3 style={{ margin: 0 }}>Start resignation</h3><div className="mute" style={{ fontSize: 12.5 }}>Planning to leave? Start your resignation here. Your notice period is {n.noticeDays} days{n.policy ? ` as per ${n.policy}` : ''}.</div></div>
          <button className="btn" onClick={() => setOpen(true)}>Start resignation</button></div>
      </div>
      {open && <Modal wide onClose={() => setOpen(false)}>
        <div className="row sp"><h2>Start resignation</h2><button className="btn sm" onClick={() => setOpen(false)}>Close</button></div>
        <div className="form" style={{ marginTop: 10 }}>
          <Field label="Resignation date"><input type="date" value={today()} readOnly style={ro} /></Field>
          <Field label="Last working day"><input type="date" value={n.lwd} readOnly style={ro} /></Field>
          <Field label="Reason for leaving"><select value={reason || reasons[0] || ''} onChange={(x) => setReason(x.target.value)}>{reasons.map((r) => <option key={r}>{r}</option>)}</select></Field>
          <Field label="Note to your manager (optional)"><input type="text" value={note} onChange={(x) => setNote(x.target.value)} /></Field>
        </div>
        <div className="note" style={{ marginTop: 10 }}>Your notice period is <b>{n.noticeDays} days</b>{n.policy ? ` as per ${n.policy}` : ''}, so your last working day is <b>{fmtD(n.lwd)}</b>. If you need to leave earlier, say so in the note. Your manager can agree an earlier date when approving.</div>
        <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 0' }}>After approval you serve the notice, return company assets and uniform, and fill a short exit interview. You can withdraw it from this page within the policy window.</p>
        <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={() => setOpen(false)}>Cancel</button><button className="btn primary" onClick={submit}>Submit resignation</button></div>
      </Modal>}
    </>
  );
}
