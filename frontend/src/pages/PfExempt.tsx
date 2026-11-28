import { useState } from 'react';
import { api, inr } from '../api';
import { Modal, Pill, toast, useLoad } from '../components/ui';

/** PF for a new joiner (owner, 14 Nov 2026): exempt when the monthly gross is above the company's threshold and they have
 *  no existing UAN. Shown on the employee's pay section with the reason, the Basic ≤ ₹15,000 warning, and Payroll's or
 *  HR Admin's override (a reason, audited). */
export function PfExemptBlock({ id }: { id: string }) {
  const d = useLoad(() => api<any>(`/employees/${id}/pf-exempt`).catch(() => null), [id]);
  const [edit, setEdit] = useState(false);
  const [mode, setMode] = useState('exempt');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [choiceEdit, setChoiceEdit] = useState(false);
  const [choice, setChoice] = useState('member');
  const x = d.data;
  if (!x || !x.statutory) return null;
  const save = async () => {
    if (reason.trim().length < 5) { toast('Give the reason for the change'); return; }
    setBusy(true);
    try { await api(`/employees/${id}/pf-exempt`, { method: 'PUT', body: { mode, reason: reason.trim() } }); toast('PF exemption saved'); setEdit(false); setReason(''); d.reload(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const saveChoice = async () => {
    if (reason.trim().length < 5) { toast('Give the reason for the change'); return; }
    setBusy(true);
    try { await api(`/employees/${id}/pf-choice`, { method: 'PUT', body: { choice, reason: reason.trim() } }); toast('PF choice saved'); setChoiceEdit(false); setReason(''); d.reload(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const ch = x.choice || {};
  const choiceText = ch.value === 'optout' ? 'No PF: paid as all Basic' : ch.value === 'member' ? 'PF deducted (normal structure)' : 'Not asked';
  const existing = x.existingUan == null ? 'Not answered' : x.existingUan ? 'Yes' : 'No';
  return <>
    <h3 style={{ marginTop: 18 }}>PF</h3>
    <dl className="kv" data-testid="pf-exempt">
      <dt>PF status</dt><dd>{x.exempt ? <Pill k="warn">PF exempt</Pill> : <Pill k="ok">PF member</Pill>} {x.manual && <Pill k="info">Set by hand</Pill>}
        {x.reason && <div className="mute" style={{ fontSize: 12.5 }}>{x.reason}{x.by ? ` · ${x.by}` : ''}{x.at ? `, ${x.at}` : ''}</div>}</dd>
      <dt>Existing UAN at joining</dt><dd>{existing}{x.hasUan ? ' · UAN on record' : ' · no UAN on record'}</dd>
      <dt>PF choice</dt><dd>{ch.value === 'optout' ? <Pill k="warn">{choiceText}</Pill> : choiceText}
        {ch.value === 'optout' && <span className="mute" style={{ fontSize: 12.5 }}> · {ch.allBasic ? 'all Basic in force' : 'all Basic only while PF exempt'}{ch.from ? `, from ${ch.from}` : ''}</span>}
        {ch.by && <div className="mute" style={{ fontSize: 12.5 }}>{ch.by}{ch.at ? `, ${ch.at}` : ''}{ch.reason ? ` · ${ch.reason}` : ''}</div>}
        {!ch.open && !ch.value && ch.why && <div className="mute" style={{ fontSize: 12.5 }}>{ch.why}</div>}</dd>
      <dt>The rule</dt><dd className="ink2" style={{ fontSize: 13 }}>{x.rule?.reason} <span className="mute">(exempt above {inr(x.threshold)} with no existing UAN)</span></dd>
    </dl>
    {x.retainer && <div className="note bad" style={{ fontSize: 13, marginTop: 6 }}>{x.retainer}</div>}
    {x.warning && <div className="note warn" style={{ fontSize: 13, marginTop: 6 }}>{x.warning}</div>}
    {!x.exempt && !x.hasUan && x.uanHold && <div className="note" style={{ fontSize: 13, marginTop: 6 }}>No UAN on record: the salary is held at the payroll run until the UAN is entered.</div>}
    {x.canEdit && <div className="row" style={{ marginTop: 8, gap: 8 }}><button className="btn sm" onClick={() => { setMode(x.exempt ? 'member' : 'exempt'); setEdit(true); }}>Change PF exemption</button>
      {(ch.open || ch.value) && <button className="btn sm" onClick={() => { setChoice(ch.value === 'optout' ? 'member' : 'optout'); setChoiceEdit(true); }}>Change PF choice</button>}</div>}
    {choiceEdit && <Modal onClose={() => setChoiceEdit(false)}>
      <h3>PF choice</h3>
      <label className="field"><span>Do they want PF deducted from the salary?</span><select value={choice} onChange={(e) => setChoice(e.target.value)}>
        <option value="member">Yes: PF member, normal structure (Basic, HRA, special allowance)</option><option value="optout">No: no PF, the whole fixed salary as Basic</option></select></label>
      <label className="field" style={{ marginTop: 8 }}><span>Reason</span><input type="text" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="For example: asked in writing to join PF" /></label>
      <p className="mute" style={{ fontSize: 12 }}>Applies from the open payroll month (earlier months stay as paid). Only for someone with no existing UAN and a monthly gross above {inr(x.threshold)}; someone already paid PF cannot stop it. Audited.</p>
      <div className="row" style={{ marginTop: 10, gap: 8 }}><button className="btn primary" disabled={busy} onClick={saveChoice}>Save</button><button className="btn" onClick={() => setChoiceEdit(false)}>Cancel</button></div>
    </Modal>}
    {edit && <Modal onClose={() => setEdit(false)}>
      <h3>PF exemption</h3>
      <label className="field"><span>Set to</span><select value={mode} onChange={(e) => setMode(e.target.value)}>
        <option value="exempt">PF exempt (set by hand)</option><option value="member">PF member (set by hand)</option><option value="auto">Decided by the rule</option></select></label>
      <label className="field" style={{ marginTop: 8 }}><span>Reason</span><input type="text" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="For example: asked to stay in PF" /></label>
      <p className="mute" style={{ fontSize: 12 }}>Set by hand, the automatic decision leaves it alone until you choose "Decided by the rule". The change is audited and the payroll run for the open month is recalculated with it.</p>
      <div className="row" style={{ marginTop: 10, gap: 8 }}><button className="btn primary" disabled={busy} onClick={save}>Save</button><button className="btn" onClick={() => setEdit(false)}>Cancel</button></div>
    </Modal>}
  </>;
}
