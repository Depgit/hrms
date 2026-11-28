import { useState } from 'react';
import { api, fmtD, inr, openDoc, today, uploadDoc } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import { Statement } from './Compensation';

/* The hiring lifecycle (hiring.go, workforce plan module 1): offer versions and HR-recorded answers, the Offer ID at
   acceptance, joining-day outcomes, the waiting window, early exit, the background check, the status timeline, the
   employee stage, and the settings. Used by Onboarding (candidate), Employee file (stage and timeline). */

type Act = (fn: () => Promise<any>, ok: (r: any) => string) => Promise<void>;
type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'n';
export type LifeEvent = { at: string; action: string; from?: string | null; to?: string | null; reason?: string | null; date?: string | null; by: string; subject?: string };

const VTONE: Record<string, Tone> = { Accepted: 'ok', Sent: 'info', Approved: 'info', Question: 'warn', 'Pending approval': 'warn', Revised: 'n', 'Not approved': 'bad', Declined: 'bad', Lapsed: 'bad' };
const fmtDT = (s?: string | null) => (s ? new Date(s).toLocaleString('en-IN', { day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }) : '');
const addDays = (d: string, n: number) => { const x = new Date(d + 'T00:00:00'); x.setDate(x.getDate() + n); return `${x.getFullYear()}-${String(x.getMonth() + 1).padStart(2, '0')}-${String(x.getDate()).padStart(2, '0')}`; };

/** The status timeline: every change, who, when, from → to and why. Newest first. With the offer versions, an
 *  "Offer vN …" line opens that version's breakup: tap the gross (owner, 27 Nov 2026). */
export function Timeline({ events, empty = 'Nothing recorded yet.', versions }: { events?: LifeEvent[] | null; empty?: string; versions?: any[] }) {
  const [pv, setPv] = useState<any>(null);
  const l = [...(events ?? [])].reverse();
  if (!l.length) return <p className="mute" style={{ margin: 0, fontSize: 13 }}>{empty}</p>;
  const verOf = (a: string) => { const m = /^Offer v(\d+)\b/.exec(a); return m ? (versions ?? []).find((v) => v.version === +m[1] && v.pay) : undefined; };
  const reason = (e: LifeEvent, v: any) => {
    const r = e.reason || '';
    const m = v && /gross ₹[\d,]+/.exec(r);
    if (!v) return r;
    if (!m) return <>{r}{r ? ' · ' : ''}<a role="button" tabIndex={0} className="lnk" onClick={() => setPv(v)}>See the breakup</a></>;
    return <>{r.slice(0, m.index)}<a role="button" tabIndex={0} className="lnk" title="See the breakup of this offer" onClick={() => setPv(v)}>{m[0]}</a>{r.slice(m.index + m[0].length)}</>;
  };
  return <><ul className="log">{l.map((e, i) => { const v = verOf(e.action); return <li key={i}>
    <b>{e.action}</b>{e.from || e.to ? <span className="ink2"> · {e.from || '—'} → {e.to || '—'}</span> : null}
    <span className="mute"> · {e.by} · {fmtDT(e.at)}{e.date ? ` · effective ${fmtD(e.date)}` : ''}</span>
    {(e.reason || v) && <div className="ink2" style={{ fontSize: 12.5 }}>{reason(e, v)}</div>}
  </li>; })}</ul>
    {pv && <VersionPay v={pv} onClose={() => setPv(null)} />}</>;
}

/** One offer version's breakup as it was decided (offer_version.pay), or worked out again for an old version. */
export function VersionPay({ v, onClose }: { v: any; onClose: () => void }) {
  const p = v.pay || {};
  return <Modal wide onClose={onClose}>
    <h2 style={{ marginTop: 0 }}>Offer v{v.version} breakup · gross {inr(v.gross)} a month</h2>
    <p className="ink2" style={{ fontSize: 13, margin: '0 0 10px' }}>{v.type} · joining {fmtD(v.doj)}{v.grade ? ` · grade ${v.grade}` : ''}{p.structure ? ` · structure ${p.structure}` : ''}{p.ctcYear ? ` · CTC ${inr(p.ctcYear)} a year` : ''}</p>
    {p.live ? <div className="note warn" style={{ marginBottom: 10 }}>This version was raised before breakups were kept, so it is worked out again from its gross and details with today's salary structure. If the structure or rates changed since, the amounts sent may have differed.</div>
      : <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>As worked out {v.releasedAt ? 'when the offer was sent' : 'when the offer was raised'}{p.at ? ` (${fmtDT(p.at)})` : ''}. Later changes to the structure do not change it.</p>}
    {p.breakup ? <Statement c={p.breakup} type={v.type} std={p.rates} rules={p.rules ? { [v.type]: p.rules } : undefined} /> : <p className="mute">No breakup for this version.</p>}
    <div className="row" style={{ marginTop: 12 }}><button className="btn" onClick={onClose}>Close</button></div>
  </Modal>;
}

/** A small inline form: a reason (and maybe a date or days) before an action. */
function Ask({ label, date, days, rehire, minDate, maxDate, onCancel, onGo, go, tone = 'primary', optional }: {
  label: string; date?: string; days?: boolean; rehire?: boolean; minDate?: string; maxDate?: string; onCancel: () => void; go: string; tone?: 'primary' | 'bad'; optional?: boolean;
  onGo: (v: { reason: string; date: string; days: number; rehire: string }) => void;
}) {
  const [v, setV] = useState({ reason: '', date: date ?? '', days: 3, rehire: 'Yes' });
  return <div className="card" style={{ marginTop: 10 }}>
    <div className="form">
      {date !== undefined && <Field label="Date"><input type="date" value={v.date} min={minDate} max={maxDate} onChange={(e) => setV({ ...v, date: e.target.value })} /></Field>}
      {days && <Field label="Extend by (days)"><input type="number" min={1} max={60} value={v.days} onChange={(e) => setV({ ...v, days: +e.target.value })} /></Field>}
      {rehire && <Field label="Eligible for rehire"><select value={v.rehire} onChange={(e) => setV({ ...v, rehire: e.target.value })}><option>Yes</option><option>No</option></select></Field>}
      <label className="field" style={{ gridColumn: '1 / -1' }}><span>{label}{!optional && <b style={{ color: 'var(--bad)' }}> *</b>}</span><input type="text" value={v.reason} onChange={(e) => setV({ ...v, reason: e.target.value })} /></label>
    </div>
    <div className="row" style={{ marginTop: 10 }}><button className="btn" onClick={onCancel}>Cancel</button><button className={`btn ${tone}`} onClick={() => onGo(v)}>{go}</button></div>
  </div>;
}

/** Offer versions (v1, v2…) and the candidate's answer, recorded by HR: Accepted, Question about the breakup, Declined. */
export function OfferVersions({ x, act, busy }: { x: any; act: Act; busy: boolean }) {
  const [ask, setAsk] = useState<'' | 'query' | 'decline' | 'expiry'>('');
  const [pv, setPv] = useState<any>(null);
  const c = x.can || {};
  const vs: any[] = x.offerVersions || [];
  const step = (s: string, body: any, ok: string) => act(() => api(`/offers/${x.offer_id}/${s}`, { body }), (r) => r?.result || ok).then(() => setAsk(''));
  return <>
    {vs.length > 0 && <div className="tw" style={{ marginTop: 8 }}><table><thead><tr><th>Version</th><th>Status</th><th className="num">Gross a month</th><th>Joining</th><th>Answer due</th><th>Answer</th></tr></thead>
      <tbody>{vs.map((v) => <tr key={v.version}><td className="mono">v{v.version}{v.frozen ? ' · frozen' : ''}</td><td><Pill k={VTONE[v.status] ?? 'n'}>{v.status}</Pill></td>
        <td className="num">{v.pay ? <a role="button" tabIndex={0} className="lnk" title="See the breakup" onClick={() => setPv(v)}>{inr(v.gross)}</a> : inr(v.gross)}</td><td>{fmtD(v.doj)}</td><td>{v.expiresOn ? fmtD(v.expiresOn) : '—'}</td>
        <td style={{ fontSize: 12.5 }}>{v.answer || <span className="mute">—</span>}{v.answeredBy && <div className="mute">{v.answeredBy} · {fmtDT(v.answeredAt)}</div>}</td></tr>)}</tbody></table></div>}
    {c.accept && <div className="row" style={{ marginTop: 10 }}>
      <span className="ink2" style={{ fontSize: 13 }}>Candidate's answer{x.expires_on ? ` (due by ${fmtD(x.expires_on)})` : ''}:</span>
      <button className="btn primary" disabled={busy} onClick={() => step('accept', {}, 'Marked accepted')}>Accepted</button>
      {c.answer && <button className="btn" onClick={() => setAsk('query')}>Question about the breakup</button>}
      <button className="btn bad" onClick={() => setAsk('decline')}>Declined</button>
      {c.answer && <button className="btn sm" onClick={() => setAsk('expiry')}>Move the expiry date</button>}
    </div>}
    {x.query_text && !x.accepted_at && !x.declined_at && <div className="note warn" style={{ marginTop: 10 }}><b>Question about the breakup:</b> {x.query_text}<div className="mute" style={{ fontSize: 12 }}>Revise the offer: a new version goes for approval and is sent again.</div></div>}
    {ask === 'query' && <Ask label="What did the candidate ask?" go="Record the question" onCancel={() => setAsk('')} onGo={(v) => step('query', { reason: v.reason }, 'Question recorded')} />}
    {ask === 'decline' && <Ask label="Why did the candidate decline?" go="Mark declined" tone="bad" onCancel={() => setAsk('')} onGo={(v) => step('decline', { reason: v.reason }, 'Marked declined')} />}
    {pv && <VersionPay v={pv} onClose={() => setPv(null)} />}
    {ask === 'expiry' && <Ask label="Why move it?" date={x.expires_on || today()} minDate={today()} go="Move the expiry date" onCancel={() => setAsk('')} onGo={(v) => step('expiry', { expires: v.date, reason: v.reason }, 'Expiry moved')} />}
  </>;
}

/** Offer at joining (offer_at_joining.go): HR chooses it for a selected candidate instead of raising the offer now,
 *  with the expected joining date and a note. */
export function OfferAtJoiningChoose({ x, act, busy }: { x: any; act: Act; busy: boolean }) {
  const [open, setOpen] = useState(false);
  const [v, setV] = useState({ date: today(), note: '' });
  if (!open) return <button className="btn" onClick={() => setOpen(true)}>Offer at joining</button>;
  return <div className="card" style={{ marginTop: 10, flexBasis: '100%' }}>
    <p className="ink2" style={{ fontSize: 13, margin: '0 0 8px' }}>No offer now. On the joining day you raise the offer (the same form), it goes for Offer approval as usual, and Mark joined after the approval is the acceptance: the Offer ID is issued and the offer letter given at joining.</p>
    <div className="form">
      <Field label="Expected joining date"><input type="date" value={v.date} min={today()} onChange={(e) => setV({ ...v, date: e.target.value })} /></Field>
      <label className="field" style={{ gridColumn: '1 / -1' }}><span>Note (optional)</span><input type="text" value={v.note} onChange={(e) => setV({ ...v, note: e.target.value })} /></label>
    </div>
    <div className="row" style={{ marginTop: 10 }}><button className="btn" onClick={() => setOpen(false)}>Cancel</button>
      <button className="btn primary" disabled={busy || !v.date} onClick={() => act(() => api(`/candidates/${x.id}/offer-at-joining`, { body: v }), (r) => r.result).then(() => setOpen(false))}>Choose offer at joining</button></div>
  </div>;
}

/** Where an offer-at-joining candidate stands: the expected date, the note and what Mark joined waits for. */
export function OfferAtJoiningNote({ x }: { x: any }) {
  const o = x.offerAtJoining;
  if (!o) return null;
  return <div className="note" style={{ marginTop: 8 }}><b>Offer at joining</b>: expected to join on {fmtD(o.doj)}{o.note ? ` · ${o.note}` : ''} <span className="mute">(chosen by {o.by})</span>.
    <div className="ink2" style={{ fontSize: 12.5 }}>{!x.offer_id ? (o.due ? 'The joining day has come: raise the offer at joining. It goes for Offer approval; Mark joined comes after the approval.' : `Raise the offer at joining on ${fmtD(o.doj)}. Until then you can still raise the usual offer or close the candidate.`)
      : o.waitingFor || 'Approved. Mark joined: joining is the acceptance, the Offer ID is issued and the offer letter is given.'}</div></div>;
}

/** Joining-day outcomes for an accepted offer: joined on the actual date, no-show (waiting), date moved, withdrew,
 *  declined on the day. */
export function JoiningOutcomes({ x, act, busy, onDetails }: { x: any; act: Act; busy: boolean; onDetails: () => void }) {
  const c = x.can || {};
  const [jd, setJd] = useState(today());
  const [ask, setAsk] = useState<'' | 'doj' | 'decline' | 'no-show'>('');
  const ev = (body: any) => act(() => api(`/candidates/${x.id}/event`, { body }), (r) => r.result).then(() => setAsk(''));
  if (!c.markJoined && !c.outcome) return null;
  // Four outcomes only (user, 18 Nov): Mark joined, Reschedule joining, No show, Offer declined. "Offer declined" is
  // recorded as declined on the joining day once that day has come, else as withdrawn before joining (drop-off report).
  const declineAction = c.noShow ? 'declined-day' : 'withdrew';
  return <>
    <div className="row" style={{ alignItems: 'flex-end' }}>
      {c.markJoined && <><Field label="Actual joining date"><input type="date" value={jd} max={today()} onChange={(e) => setJd(e.target.value)} /></Field>
        <button className="btn primary" disabled={busy} onClick={() => act(() => api(`/candidates/${x.id}/joined`, { body: { date: jd } }), (r) => `${x.name} joined: ${r.id}. Punches and pay count from ${fmtD(jd)}`)}>Mark joined</button></>}
      {c.outcome && <>
        <button className="btn" onClick={() => setAsk('doj')}>Reschedule joining</button>
        {c.noShow && <button className="btn" onClick={() => setAsk('no-show')}>No show</button>}
        <button className="btn bad" onClick={() => setAsk('decline')}>Offer declined</button>
      </>}
    </div>
    {x.offerAtJoining && !c.markJoined && x.offerAtJoining.waitingFor && x.offer_id && <p className="ink2" style={{ fontSize: 12.5, margin: '6px 0 0' }}>Mark joined: {x.offerAtJoining.waitingFor}.</p>}
    {(x.doj || x.offerAtJoining) && <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>{x.offerAtJoining ? `Expected joining date: ${fmtD(x.offerAtJoining.doj)}.` : `Joining date on the offer: ${fmtD(x.doj)}.`}</p>}
    {ask === 'no-show' && <Ask label="What happened (not answering, phone off…)" optional go={`Start waiting (${x.hiring?.waitDays ?? 7} days)`} onCancel={() => setAsk('')} onGo={(v) => ev({ action: 'no-show', reason: v.reason })} />}
    {ask === 'doj' && <Ask label="Why is the joining date changing?" date={addDays(x.offerAtJoining?.doj || x.doj || today(), 7)} go="Reschedule joining" onCancel={() => setAsk('')} onGo={(v) => ev({ action: 'doj', date: v.date, reason: v.reason })} />}
    {ask === 'decline' && <Ask label="Why did the candidate decline?" go="Close: offer declined" tone="bad" onCancel={() => setAsk('')} onGo={(v) => ev({ action: declineAction, reason: v.reason })} />}
    {/* The joiner's link and joining details are their own section, not a joining outcome (after joining for an offer at joining). */}
    {!x.offerAtJoining && <div className="card" style={{ marginTop: 14 }}><h3>Joiner's link and joining details</h3>
      <p className="mute" style={{ fontSize: 12.5, margin: '-4px 0 8px' }}>Send the joiner their link so their details and documents are in before the first day. Required before the Employee ID is issued.</p>
      <button className="btn" onClick={onDetails}>Open joiner's link and joining details</button></div>}
  </>;
}

/** The waiting window: no-show on the joining day or stopped coming in the first days. Day counter, calls,
 *  extension, back within the window, close when it ends. */
export function WaitingPanel({ x, act, busy }: { x: any; act: Act; busy: boolean }) {
  const w = x.waitingWindow;
  const c = x.can || {};
  const [ask, setAsk] = useState<'' | 'call' | 'extend' | 'back' | 'close'>('');
  if (!w) return null;
  const ev = (body: any) => act(() => api(`/candidates/${x.id}/event`, { body }), (r) => r.result).then(() => setAsk(''));
  const what = w.kind === 'no-show' ? 'Did not come on the joining day' : 'Stopped coming';
  return <div className="card" style={{ marginTop: 10, borderColor: 'var(--warn)' }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Waiting: {what.toLowerCase()}</h3>
      <Pill k={w.over ? 'bad' : 'warn'}>{w.over ? `Window ended ${fmtD(w.ends)}` : `Day ${w.day} of ${w.total}`}</Pill></div>
    <p className="ink2" style={{ fontSize: 13, margin: '6px 0' }}>Since {fmtD(w.since)}{w.reason ? ` · ${w.reason}` : ''}. {w.kind === 'no-show'
      ? 'If they come within the window, mark them joined on the actual date: the same offer, documents and link.'
      : 'If they come back within the window the process resumes; the missed days count as absent.'} When the window ends the case closes as {w.kind === 'no-show' ? 'No-show' : 'Early exit'}{x.hiring?.waitAutoClose ? ' automatically' : ' once you confirm'}.</p>
    {w.ext?.length > 0 && <p className="mute" style={{ fontSize: 12, margin: '0 0 6px' }}>Extended: {w.ext.map((e: any) => `${e.days} day${e.days === 1 ? '' : 's'} (${e.reason}, ${e.by})`).join('; ')}</p>}
    <h4 style={{ margin: '8px 0 4px', fontSize: 13 }}>Call attempts</h4>
    {w.calls?.length ? <ul className="log">{[...w.calls].reverse().map((k: any, i: number) => <li key={i}>{k.note} <span className="mute">· {k.by} · {fmtDT(k.at)}</span></li>)}</ul> : <p className="mute" style={{ margin: 0, fontSize: 12.5 }}>None logged yet.</p>}
    {c.wait && <div className="row" style={{ marginTop: 10 }}>
      <button className="btn primary" disabled={busy} onClick={() => setAsk('back')}>{w.kind === 'no-show' ? 'Came: mark joined' : 'Came back'}</button>
      <button className="btn" onClick={() => setAsk('call')}>Log a call</button>
      <button className="btn" onClick={() => setAsk('extend')}>Extend the window</button>
      {c.closeWait && <button className="btn bad" onClick={() => setAsk('close')}>Close as {w.kind === 'no-show' ? 'No-show' : 'Early exit'}</button>}
    </div>}
    {ask === 'call' && <Ask label="What happened on the call?" go="Log the call" onCancel={() => setAsk('')} onGo={(v) => ev({ action: 'call', reason: v.reason })} />}
    {ask === 'extend' && <Ask label="Why extend?" days go="Extend" onCancel={() => setAsk('')} onGo={(v) => ev({ action: 'extend', days: v.days, reason: v.reason })} />}
    {ask === 'back' && <Ask label="Note" optional date={today()} minDate={w.since} maxDate={today()} go={w.kind === 'no-show' ? 'Mark joined on this date' : 'Mark back'} onCancel={() => setAsk('')} onGo={(v) => ev({ action: 'back', date: v.date, reason: v.reason })} />}
    {ask === 'close' && <Ask label="Reason" optional rehire={w.kind !== 'no-show'} go={`Close as ${w.kind === 'no-show' ? 'No-show' : 'Early exit'}`} tone="bad" onCancel={() => setAsk('')} onGo={(v) => ev({ action: 'close', reason: v.reason, rehire: v.rehire })} />}
  </div>;
}

/** Early exit in the first days (on a Joining ID): stopped coming (waiting) or says they quit (closes at once). */
export function EarlyExit({ x, act }: { x: any; act: Act }) {
  const [ask, setAsk] = useState<'' | 'stopped' | 'quit'>('');
  if (!x.can?.earlyExit) return null;
  const ev = (body: any) => act(() => api(`/candidates/${x.id}/event`, { body }), (r) => r.result).then(() => setAsk(''));
  return <>
    <div className="row" style={{ marginTop: 10 }}>
      <button className="btn" onClick={() => setAsk('stopped')}>Stopped coming</button>
      <button className="btn bad" onClick={() => setAsk('quit')}>Quit in the first days</button>
    </div>
    {ask === 'stopped' && <Ask label="What happened" optional date={today()} minDate={addDays(x.joined_on, 1)} maxDate={today()} go={`Start waiting (${x.hiring?.waitDays ?? 7} days)`} onCancel={() => setAsk('')}
      onGo={(v) => ev({ action: 'stopped', date: v.date, reason: v.reason })} />}
    {ask === 'quit' && <Ask label="Why are they leaving?" date={today()} minDate={x.joined_on} maxDate={today()} rehire go="Close as Early exit" tone="bad" onCancel={() => setAsk('')}
      onGo={(v) => ev({ action: 'quit', date: v.date, reason: v.reason, rehire: v.rehire })} />}
    {(ask === 'stopped' || ask === 'quit') && <p className="mute" style={{ fontSize: 12 }}>{ask === 'stopped' ? 'Date: the first day they did not come.' : 'Date: the last day they worked. They are paid for the days worked through normal payroll; no separation or full and final case.'}</p>}
  </>;
}

/** The background check: Pending, Clear or Discrepancy with the uploaded report. */
export function BgvPanel({ x, reload }: { x: any; reload: () => void }) {
  const b = x.bgv || {};
  const [st, setSt] = useState<string>(b.status || 'Pending');
  const [note, setNote] = useState<string>(b.note || '');
  const [busy, setBusy] = useState(false);
  const rep = x.docs?.bgvReport;
  const edit = !!x.can?.bgv;
  const upload = async (f?: File) => {
    if (!f) return;
    try { await uploadDoc('candidate', x.id, 'bgvReport', f); toast('Attached ' + f.name); reload(); } catch (e: any) { toast(e.message); }
  };
  const save = async () => {
    setBusy(true);
    try { const r = await api(`/candidates/${x.id}/bgv`, { body: { status: st, note } }); toast(r.employeeId ? `Background check ${st}. Employee ID ${r.employeeId} issued` : `Background check: ${st}`); reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  return <div style={{ marginTop: 10 }}>
    <dl className="kv"><dt>Background check</dt><dd>{b.status ? <Pill k={b.status === 'Clear' ? 'ok' : b.status === 'Discrepancy' ? 'bad' : 'warn'}>{b.status}</Pill> : <span className="mute">Not recorded</span>}
      {b.by && <span className="mute" style={{ fontSize: 12 }}> · {b.by} · {fmtDT(b.at)}</span>}{b.note && <div className="ink2" style={{ fontSize: 12.5 }}>{b.note}</div>}
      {x.hiring?.bgvBlocks && b.status !== 'Clear' && <div className="mute" style={{ fontSize: 12 }}>The Employee ID waits for a clear check.</div>}</dd>
      <dt>Report</dt><dd><div className="upl">{rep ? <a href="#" onClick={(e) => { e.preventDefault(); openDoc(rep.id).catch((er) => toast(er.message)); }}>{rep.file_name}</a> : <span className="mute">None</span>}
        {edit && <label className="btn sm">{rep ? 'Replace' : 'Upload'}<input type="file" accept=".pdf,.jpg,.jpeg,.png" hidden onChange={(e) => { upload(e.target.files?.[0]); e.target.value = ''; }} /></label>}</div></dd></dl>
    {edit && <div className="row" style={{ alignItems: 'flex-end', marginTop: 6 }}>
      <Field label="Result"><select value={st} onChange={(e) => setSt(e.target.value)}><option>Pending</option><option>Clear</option><option>Discrepancy</option></select></Field>
      <label className="field" style={{ flex: '1 1 260px' }}><span>Note (vendor, what was checked{st === 'Discrepancy' ? ', the discrepancy' : ''})</span><input type="text" value={note} onChange={(e) => setNote(e.target.value)} /></label>
      <button className="btn" disabled={busy} onClick={save}>Save the check</button>
    </div>}
  </div>;
}

/** Form setup › Hiring: the waiting window, automatic close, the background check rule and offer expiry. */
export function HireSettings() {
  const { can } = useAuth();
  const { data, reload } = useLoad(() => api<{ hiring: any; nextNo?: number }>('/onboarding/settings'), []);
  const [nextNo, setNextNo] = useState('');
  const saveNext = async () => {
    if (!window.confirm(`The next candidate gets SI-${String(+nextNo).padStart(4, '0')} (then OI/JI with the same number). Numbers cannot be moved back. Save?`)) return;
    try { await api('/onboarding/settings', { method: 'PUT', body: { nextNo: +nextNo } }); toast('Saved'); setNextNo(''); reload(); } catch (x: any) { toast(x.message); }
  };
  const depts = useLoad(() => api<any[]>('/masters/department?status=Active'), []);
  const types = useLoad(() => api<any[]>('/masters/employment_type?status=Active'), []);
  const [v, setV] = useState<any>(null);
  const edit = can('onb.setup', 2);
  const cur = v ?? data?.hiring;
  const save = async () => {
    try { await api('/onboarding/settings', { method: 'PUT', body: { hiring: { ...cur, waitDays: +cur.waitDays, offerDays: +cur.offerDays, offerRemindDays: +cur.offerRemindDays } } }); toast('Saved'); setV(null); reload(); } catch (x: any) { toast(x.message); }
  };
  if (!cur) return <Loading />;
  const set = (k: string, val: any) => setV({ ...cur, [k]: val });
  // Offer letter at joining (hirefix.go): departments and employment types whose letter waits for Mark joined.
  const pick = (k: 'letterJoinDepts' | 'letterJoinTypes', names: string[]) => <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>
    {names.map((n) => { const on = (cur[k] ?? []).includes(n); return <button key={n} type="button" className={`pill ${on ? 'info' : 'n'}`} disabled={!edit} style={{ cursor: edit ? 'pointer' : 'default', border: 0 }}
      onClick={() => set(k, on ? (cur[k] ?? []).filter((x: string) => x !== n) : [...(cur[k] ?? []), n])}>{on ? '✓ ' : ''}{n}</button>; })}</div>;
  return <div className="card" style={{ marginBottom: 14 }}><h3>Hiring</h3>
    <div className="form">
      <Field label="An offer stays open (days)"><input type="number" min={1} max={90} disabled={!edit} value={cur.offerDays} onChange={(e) => set('offerDays', e.target.value)} /></Field>
      <Field label="Remind HR before it lapses (days)"><input type="number" min={0} max={90} disabled={!edit} value={cur.offerRemindDays} onChange={(e) => set('offerRemindDays', e.target.value)} /></Field>
      <Field label="Waiting window (days)"><input type="number" min={1} max={60} disabled={!edit} value={cur.waitDays} onChange={(e) => set('waitDays', e.target.value)} /></Field>
      <Field label="When the window ends"><select disabled={!edit} value={String(!!cur.waitAutoClose)} onChange={(e) => set('waitAutoClose', e.target.value === 'true')}><option value="false">HR confirms the close</option><option value="true">Close automatically</option></select></Field>
      <Field label="Background check"><select disabled={!edit} value={cur.bgvOff ? 'off' : String(!!cur.bgvBlocks)} onChange={(e) => { const v = e.target.value; setV({ ...cur, bgvOff: v === 'off', bgvBlocks: v === 'true' }); }}><option value="off">Not used: we do no background check</option><option value="true">Pending or discrepancy holds the Employee ID</option><option value="false">Recorded, does not hold the Employee ID</option></select></Field>
    </div>
    <h3 className="sec">Hiring number (SI / OI / JI)</h3>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>The next candidate gets <b>SI-{String(data?.nextNo ?? '').padStart(4, '0')}</b>; the offer and the Joining ID keep the same number, and the Realtime portal code is that number in 6 digits. Set it past the portal's highest code so new codes never clash. It only moves forward.</p>
    {edit && <div className="row" style={{ gap: 8, alignItems: 'flex-end' }}><Field label="Next hiring number"><input type="number" min={data?.nextNo ?? 1} max={999999} value={nextNo} placeholder={String(data?.nextNo ?? '')} onChange={(e) => setNextNo(e.target.value)} /></Field>
      <button className="btn primary" disabled={!nextNo || +nextNo <= (data?.nextNo ?? 0) - 1} onClick={saveNext}>Set</button></div>}
    <h3 className="sec">Release the offer letter at</h3>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Offer release is the default. For the departments and employment types chosen here the letter is released at <b>joining</b>: the offer is still approved, released and accepted as usual, but the letter is not generated or given to the candidate until HR marks them joined, when it is released automatically (audited). A department or a type chosen is enough.</p>
    <Field label="Departments: letter at joining">{pick('letterJoinDepts', (depts.data ?? []).map((x) => x.name))}</Field>
    <Field label="Employment types: letter at joining">{pick('letterJoinTypes', (types.data ?? []).map((x) => x.name))}</Field>
    {edit && v && <div className="row" style={{ marginTop: 10 }}><button className="btn primary" onClick={save}>Save</button><button className="btn" onClick={() => setV(null)}>Cancel</button></div>}
    <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>The Offer ID (OI) is issued when HR records the candidate's acceptance; before that the offer is a version under the shortlist number (SI-0042 offer v2). At joining the person gets a Joining ID (JI) and is a normal employee from day 1: punches, attendance and payroll. Once the joining details, Payroll verification and the background check are approved, the Employee ID is issued from the code series and the record moves to it; the JI stays as an alias. Employment types set to give no Employee ID (Employees › Employee codes) keep their own series' number from joining.</p>
    <div className="note" style={{ marginTop: 10 }}>A no-show on the joining day and someone who stops coming in the first days both go to <b>Waiting</b> for this many days. HR gets a reminder each day, can log calls and extend the window for one person with a reason. Candidates who joined before 17 Nov 2026 finish the old way: the Employee ID is created when their mandatory joining documents are complete.</div>
  </div>;
}

/** Employee file › Status: the lifecycle stage (set by HR with a reason) and the timeline. */
export function EmployeeLifecycle({ id }: { id: string }) {
  const { data, error, reload } = useLoad(() => api(`/employees/${encodeURIComponent(id)}/timeline`), [id]);
  const [ask, setAsk] = useState('');
  if (!data) return <Loading error={error} />;
  const set = async (v: { reason: string; date: string }) => {
    try { await api(`/employees/${encodeURIComponent(data.id)}/stage`, { body: { stage: ask, reason: v.reason, date: v.date } }); toast(`Stage: ${ask}`); setAsk(''); reload(); } catch (e: any) { toast(e.message); }
  };
  return <div className="card">
    <div className="row sp" style={{ alignItems: 'flex-start' }}>
      <div><h3 style={{ margin: 0 }}>Status and stage</h3>
        <div className="ink2" style={{ fontSize: 13, marginTop: 4 }}>Status <Pill k={data.status === 'Active' ? 'ok' : data.status === 'Inactive' ? 'n' : 'warn'}>{data.status}</Pill>
          {' '}· Stage {data.stage ? <><Pill k="info">{data.stage}</Pill>{data.stageSince ? <span className="mute"> since {fmtD(data.stageSince)}</span> : null}</> : <span className="mute">not set</span>}
          {data.aliases?.length > 0 && <span className="mute"> · also found as {data.aliases.join(', ')}</span>}</div>
        {data.hireIds && <div className="ink2" style={{ fontSize: 13, marginTop: 4 }}>Shortlisting ID <b className="mono">{data.hireIds.si}</b> · Offer ID <b className="mono">{data.hireIds.oi}</b> · Joining ID <b className="mono">{data.hireIds.ji}</b>
          {data.hireIds.via === 'code' && <span className="mute"> · from the Employee code</span>}
          {data.hireIds.via === 'import' && <span className="mute"> · a number of the candidate series (the Employee code’s number was taken or missing)</span>}</div>}</div>
      {data.canSetStage && <label className="field" style={{ margin: 0 }}><span>Set the stage</span><select value={ask} onChange={(e) => setAsk(e.target.value)}><option value="">Choose</option>
        {(data.stages as string[]).filter((s) => s !== 'Joining' && s !== data.stage).map((s) => <option key={s}>{s}</option>)}</select></label>}
    </div>
    {ask && <Ask key={ask} label={`Why ${ask.toLowerCase()}?`} date={today()} go={`Set ${ask}`} onCancel={() => setAsk('')} onGo={set} />}
    <h3 className="sec">Timeline</h3>
    <Timeline events={data.events} />
  </div>;
}
