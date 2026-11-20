import { useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api, fmtD, inr, postForm } from '../api';
import { Adv, Field, Loading, Modal, Pill, download, toast, useLoad, usePage } from '../components/ui';
import Payslip, { saveSlipHtml, type SlipView } from '../components/Payslip';
import { useAuth } from '../auth';

export const ymLabel = (ym: string) => new Date(ym + '-01T00:00:00').toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });
const STEPS = ['Open', 'Dry run', 'Verified', 'Approved', 'Published'];

type State = {
  openYm: string; open: any; waiting: any; months: any[]; cutoff: string; canRun: boolean; canApprove: boolean;
  flags: { Key: string; Label: string }[];
};

export const usePayState = () => useLoad(() => api<State>('/payroll'), []);

/** Saves company payroll settings (TAN, PAN, cut-off …) through the payslip setup, keeping every other field as it is. */
export async function saveCompanySetup(patch: Partial<{ tan: string; pan: string; cutoff: string; firstYm: string }>) {
  const x = await api('/payslip-setup');
  const body = { setup: x.setup, logo: x.logo, cin: x.company.CIN, pan: x.company.PAN, tan: x.company.TAN, pfCode: x.company.PFCode, esiCode: x.company.ESICode, cutoff: x.cutoff, firstYm: x.firstYm ?? '', ...patch };
  await api('/payslip-setup', { method: 'PUT', body });
}

type CItem = {
  ref: string; group: string; employeeId: string; name: string; what: string; when?: string; date?: string; amount?: number; requestId?: string;
  waitsOn: string; waitsOnId?: string; waitsAs: string; note?: string; canVerify?: boolean; canLop?: boolean; messaged?: string; late?: boolean; text?: string;
  lopHalf?: boolean; canHold?: boolean; holdId?: string;
};
type CMsg = { to: string; toName: string; as: string; refs: string[]; text: string };
type Closure = {
  key: string; title: string; help: string; status: 'Done' | 'Pending' | 'Closed'; pending: number; notRaised: number; waiting: number; hidden: number; late: number;
  closedAt?: string; closedBy?: string; reopened?: { at: string; by: string; reason: string }; items: CItem[]; messages: CMsg[]; desk: CMsg[];
  held?: number; onHold?: CItem[]; uanN?: number; uan?: CItem[];
};
type Check = {
  ym: string; month: any; from: string; to: string; cutoff: string; by: string; cycleEnded: boolean; current: boolean; closures: Closure[];
  allClosed: boolean; open: string[]; canClose: boolean; canReopen: boolean; warnings?: string[];
};
type Ask = {
  key: string; action: 'reopen' | 'reject-waiting' | 'reject-bank' | 'mark-lop' | 'hold'; title: string; text: string; ok: string; body: Record<string, unknown>; optional?: boolean;
  initial?: string; // the reason, prefilled
  punches?: number; // mark-lop: missing punches among the days, marked as a full or a half day (absences are always a full day)
  absences?: number;
};

const AFTER_CLOSE: Record<string, string> = {
  attendance: 'Attendance is locked: a regularization for these dates is refused, and a later correction is paid as an arrear.',
  leave: 'New leave for the cycle, up to the day it was closed, is refused.',
  bank: 'A bank change raised from now on shows here, flagged. That salary stays on hold until the change is verified. A salary held here is paid in the next open month once the account is on record.',
  claim: 'Claims approved from now on are paid next month.',
  deduction: 'Deductions, advances and recoveries approved from now on start next month.',
};

/** "Before you run payroll" (in place of Month close): the month's five closures, each with what is still pending,
 *  split into Not raised (waits on the employee) and Waiting for approval (waits on the approver). A closure is Done
 *  when nothing is pending, and Payroll closes it; the dry run and every later step need all five closed. There is
 *  no way to set an item aside: it clears when the person it waits on acts. Payroll decides bank changes inline, as
 *  their approver, and can still reject waiting regularizations and leave at the cut-off, as Month close did. */
function Checklist({ x, reload }: { x: Check; reload: () => void }) {
  const nav = useNavigate();
  const [sel, setSel] = useState('');
  const [msg, setMsg] = useState<{ title: string; key: string; list: CMsg[] } | null>(null);
  const [ask, setAsk] = useState<Ask | null>(null);
  const [busy, setBusy] = useState(false);
  const ym = x.ym, closed = x.closures.filter((c) => c.status === 'Closed').length;
  const live = x.current && (x.month.status === 'Open' || x.month.status === 'Dry run');
  const post = async (key: string, action: string, body: Record<string, unknown>, said: (r: any) => string) => {
    setBusy(true);
    try { const r = await api(`/payroll/${ym}/closures/${key}/${action}`, { body }); toast(said(r)); setAsk(null); setMsg(null); reload(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const decide = async (it: CItem, action: 'approve' | 'reject', note: string) => {
    setBusy(true);
    try { await api(`/approvals/${it.requestId}/${action}`, { body: { note } }); toast(action === 'approve' ? 'Bank account verified' : 'Bank change rejected'); setAsk(null); reload(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const rejectBank = (c: Closure, it: CItem) => setAsk({ key: c.key, action: 'reject-bank', title: `Reject the bank change: ${it.name}`,
    text: 'The employee is told why, and can raise a new change. The salary stays on hold until a bank change is verified.', ok: 'Reject', body: { requestId: it.requestId } });
  const markLop = (c: Closure, its: CItem[]) => {
    const punches = its.filter((it) => it.lopHalf).length, absences = its.length - punches;
    const many = [absences ? `${absences} absence${absences === 1 ? '' : 's'}` : '', punches ? `${punches} missing punch${punches === 1 ? '' : 'es'}` : ''].filter(Boolean).join(' and ');
    setAsk({ key: c.key, action: 'mark-lop', title: its.length === 1 ? `Mark as loss of pay: ${its[0].name}, ${its[0].when}` : `Mark ${many} as loss of pay`, punches, absences,
      text: `For ${punches && !absences ? 'a missing punch' : punches ? 'an absence or a missing punch' : 'an absence'} nobody explained by the cut-off (also a leaver's days, or someone whose leave plan has no Loss of pay). Each day is recorded as approved Loss of pay leave raised by you, with this reason, and is audited.${punches ? ' An absence is always a full day; for a missing punch choose a full or a half day (a half day is recorded as half-day leave, and the punched half is paid as worked).' : ''} A later regularization or leave for the day is not possible once attendance is closed: correct it as an arrear.`,
      ok: 'Mark as loss of pay', body: { refs: its.map((it) => it.ref) } });
  };
  const holdPay = (c: Closure, its: CItem[]) => setAsk({ key: c.key, action: 'hold', title: its.length === 1 ? `Hold salary: ${its[0].name}` : `Hold ${its.length} salaries`, initial: 'No bank account on record',
    text: `The salary for ${ymLabel(ym)} is calculated but not paid: it is left out of the bank file and listed on Salary holds, and the item moves to On hold so the closure can close. Once ${its.length === 1 ? 'an account is' : 'their accounts are'} on record (added, or a bank change verified), the held salary is paid in the next open month as "Held salary released". Still without an account next month, ${its.length === 1 ? 'the person is' : 'they are'} listed again. Payroll can release a hold by hand in Salary holds. Audited.`,
    ok: its.length === 1 ? 'Hold salary' : `Hold ${its.length} salaries`, body: { refs: its.map((it) => it.ref) } });
  const one = (c: Closure, it: CItem) => setMsg({ title: 'Send message', key: c.key, list: [{ to: it.waitsOnId!, toName: it.waitsOn, as: it.waitsAs, refs: [it.ref], text: it.text || '' }] });
  return (
    <div className="card" style={{ marginBottom: 14 }}>
      <div className="row sp" style={{ flexWrap: 'wrap', gap: 8, alignItems: 'flex-start' }}>
        <div style={{ flex: '1 1 300px', minWidth: 0 }}>
          <h3 style={{ margin: 0 }}>Before you run payroll, {ymLabel(ym)}</h3>
          <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>Cycle {fmtD(x.from)} to {fmtD(x.to)} ({x.cutoff === '26-25' ? '26th to 25th' : 'calendar month'}). Salary cannot be processed until all five are closed.</div>
        </div>
        <Pill k={x.allClosed ? 'ok' : 'warn'}>{closed} of 5 closed</Pill>
      </div>
      {!!x.warnings?.length && <div className="note warn" style={{ marginTop: 10 }}><b>Check the set-up before the dry run</b>
        <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{x.warnings.map((w) => <li key={w} style={{ fontSize: 12.5 }}>{w}</li>)}</ul></div>}
      <div style={{ marginTop: 10 }}>
        {x.closures.map((c, i) => {
          const on = sel === c.key, open = c.status !== 'Closed', act = x.canClose && live && open;
          const nr = c.items.filter((it) => it.group === 'Not raised'), wt = c.items.filter((it) => it.group !== 'Not raised');
          const msgs = [...c.messages, ...c.desk];
          const cycle = c.key === 'attendance' || c.key === 'leave';
          const why = c.pending > 0 ? `${c.pending} item${c.pending === 1 ? ' is' : 's are'} still pending` : '';
          return (
            <div key={c.key} style={{ borderTop: '1px solid var(--line)' }}>
              <button type="button" aria-expanded={on} onClick={() => setSel(on ? '' : c.key)}
                style={{ display: 'flex', alignItems: 'center', gap: 10, width: '100%', padding: '11px 0', background: 'none', border: 0, color: 'inherit', textAlign: 'left', cursor: 'pointer', flexWrap: 'wrap' }}>
                <span aria-hidden="true" style={{ width: 12, color: 'var(--mute)' }}>{on ? '▾' : '▸'}</span>
                <b style={{ flex: '1 1 170px' }}>{i + 1}. {c.title}</b>
                <span className="mute" style={{ fontSize: 12.5 }}>{c.status === 'Closed' ? `${c.closedBy}, ${c.closedAt}` : c.status === 'Done' ? 'Nothing pending' : `${c.notRaised} not raised · ${c.waiting} waiting`}{c.late ? ` · ${c.late} after close` : ''}</span>
                {c.status === 'Closed' ? <Pill k="ok">Closed</Pill> : c.status === 'Done' ? <Pill k="info">Done</Pill> : <Pill k="warn">Pending {c.pending}</Pill>}
              </button>
              {on && <div style={{ padding: '0 0 14px 22px' }}>
                <p className="ink2" style={{ fontSize: 12.5, margin: '0 0 10px' }}>{c.help}</p>
                {c.status === 'Closed' ? <div className="note ok" style={{ marginBottom: 10 }}>Closed by {c.closedBy} on {c.closedAt}. {AFTER_CLOSE[c.key]}</div>
                  : c.reopened ? <div className="note warn" style={{ marginBottom: 10 }}>Reopened by {c.reopened.by} on {c.reopened.at}: {c.reopened.reason}</div> : null}
                <div className="row" style={{ flexWrap: 'wrap', gap: 8, marginBottom: 10 }}>
                  {c.key === 'attendance' && <button className="btn sm" onClick={() => download(`/payroll/${ym}/attendance.xlsx`, `attendance-${ym}.xlsx`).catch((e) => toast(e.message))}>Download attendance summary</button>}
                  {c.key === 'bank' && <button className="btn sm" onClick={() => nav('/payroll?tab=claims&sub=changes')}>Open profile and bank changes</button>}
                  {x.canClose && live && msgs.length > 0 && <button className="btn sm" disabled={busy} onClick={() => setMsg({ title: `Message all: ${c.title}`, key: c.key, list: msgs })}>Message all ({msgs.length})</button>}
                  {act && cycle && c.waiting > 0 && <button className="btn sm" disabled={busy} onClick={() => setAsk({ key: c.key, action: 'reject-waiting', title: 'Reject waiting requests at the cut-off',
                    text: `Rejects the ${c.waiting} ${c.key === 'leave' ? 'leave requests' : 'regularizations'} still waiting for approval, with your note, as Month close did. The days then stay as recorded.`, ok: 'Reject', body: { all: true }, optional: true })}>Reject waiting at cut-off</button>}
                  {act && cycle && nr.some((it) => it.canLop) && <button className="btn sm" disabled={busy} onClick={() => markLop(c, nr.filter((it) => it.canLop))}>
                    {nr.some((it) => it.canLop && it.lopHalf) ? 'Mark all as loss of pay' : 'Mark all absences as loss of pay'} ({nr.filter((it) => it.canLop).length})</button>}
                  {act && c.key === 'bank' && nr.some((it) => it.canHold) && <button className="btn sm" disabled={busy} onClick={() => holdPay(c, nr.filter((it) => it.canHold))}>Hold all ({nr.filter((it) => it.canHold).length})</button>}
                  {act && <button className="btn sm primary" disabled={busy || !!why} title={why} onClick={() => post(c.key, 'close', {}, () => `${c.title} closed`)}>Close</button>}
                  {!open && live && x.canReopen && <button className="btn sm" disabled={busy} onClick={() => setAsk({ key: c.key, action: 'reopen', title: `Reopen ${c.title.toLowerCase()}`,
                    text: c.key === 'attendance' ? 'Unlocks attendance for the month. The dry run and later steps wait until it is closed again.' : 'The dry run and later steps wait until it is closed again.', ok: 'Reopen', body: {} })}>Reopen</button>}
                </div>
                {c.hidden > 0 && <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>{c.hidden} more pending item{c.hidden === 1 ? ' is' : 's are'} about people outside your access.</p>}
                <ItemList title="Not raised" items={nr} act={act || (x.canClose && live && c.key === 'bank')} busy={busy} onMsg={(it) => one(c, it)} onLop={act ? (it) => markLop(c, [it]) : undefined}
                  onHold={act ? (it) => holdPay(c, [it]) : undefined} onVerify={(it) => decide(it, 'approve', 'Verified from the payroll checklist')} onReject={(it) => rejectBank(c, it)} />
                <ItemList title="Waiting for approval" items={wt} act={act || (x.canClose && live && c.key === 'bank')} busy={busy} onMsg={(it) => one(c, it)}
                  onVerify={(it) => decide(it, 'approve', 'Verified from the payroll checklist')} onReject={(it) => rejectBank(c, it)} />
                {!!c.onHold?.length && <OnHold items={c.onHold} more={(c.held ?? 0) - c.onHold.length} />}
                {!!c.uan?.length && <UanMissing items={c.uan} more={(c.uanN ?? 0) - c.uan.length} />}
                {!c.items.length && open && !c.hidden && <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>{live ? 'Nothing is pending. Close it to lock it for the run.' : 'Closures are checked for the open month.'}</p>}
              </div>}
            </div>
          );
        })}
      </div>
      {msg && <MessageDialog title={msg.title} list={msg.list} busy={busy} onClose={() => setMsg(null)}
        onSend={(list) => post(msg.key, 'message', { messages: list }, (r) => (r.sent === 1 ? 'Message sent' : `${r.sent} messages sent`))} />}
      {ask && <AskDialog a={ask} busy={busy} onClose={() => setAsk(null)}
        onOk={(v, half) => (ask.action === 'reject-bank' ? decide({ requestId: String(ask.body.requestId) } as CItem, 'reject', v)
          : post(ask.key, ask.action, ask.action === 'reopen' || ask.action === 'mark-lop' || ask.action === 'hold' ? { ...ask.body, reason: v, ...(ask.action === 'mark-lop' ? { half } : {}) } : { ...ask.body, note: v },
            (r) => (ask.action === 'reopen' ? 'Reopened' : ask.action === 'hold' ? (r.held === 1 ? 'Salary put on hold' : `${r.held} salaries put on hold`)
              : ask.action === 'mark-lop' ? `${r.marked} day${r.marked === 1 ? '' : 's'} marked as loss of pay${r.halfDays ? ` (${r.halfDays} as a half day)` : ''}` : `${r.rejected} rejected at the cut-off`)))} />}
    </div>
  );
}

/** One group of a closure's pending items, a page at a time. */
function ItemList({ title, items, act, busy, onMsg, onVerify, onReject, onLop, onHold }: { title: string; items: CItem[]; act: boolean; busy: boolean;
  onMsg: (it: CItem) => void; onVerify: (it: CItem) => void; onReject: (it: CItem) => void; onLop?: (it: CItem) => void; onHold?: (it: CItem) => void }) {
  const pg = usePage(items, 25);
  if (!items.length) return null;
  const both = items.find((it) => it.note && it.group === 'Not raised' && it.ref.includes(':attendance:'));
  return (
    <div style={{ marginBottom: 12 }}>
      <div style={{ fontWeight: 700, fontSize: 13, margin: '4px 0 6px' }}>{title} ({items.length})</div>
      {both && <p className="mute" style={{ fontSize: 12, margin: '0 0 6px' }}>Days marked “Also an absence” would show under Leave closure too. Each is listed once, here: {both.note?.replace(/^.*?here: /, '')}</p>}
      <div className="tw"><table><thead><tr><th>Employee</th><th>What</th><th>Date or amount</th><th>Waits on</th><th></th></tr></thead><tbody>
        {(pg.rows ?? []).map((it) => <tr key={it.ref}>
          <td><b>{it.name}</b><div className="mono mute" style={{ fontSize: 12 }}>{it.employeeId}</div></td>
          <td>{it.what}{it.requestId && <span className="mono mute"> {it.requestId}</span>}
            {it.note && (it.ref.includes(':attendance:') && it.group === 'Not raised' ? <> <Pill k="n">Also an absence</Pill></> : <div className="mute" style={{ fontSize: 12 }}>{it.note}</div>)}
            {it.late && <> <Pill k="bad">After close</Pill></>}</td>
          <td style={{ whiteSpace: 'nowrap' }}>{it.amount != null && <div>{inr(it.amount)}</div>}{it.when && <div className={it.amount != null ? 'mute' : ''} style={{ fontSize: it.amount != null ? 12 : undefined }}>{it.when}</div>}</td>
          <td>{it.waitsOn}<div className="mute" style={{ fontSize: 12 }}>{it.waitsAs}{it.messaged ? ` · Messaged ${it.messaged}` : ''}</div></td>
          <td style={{ whiteSpace: 'nowrap' }}><div className="row" style={{ gap: 6, justifyContent: 'flex-end', flexWrap: 'nowrap' }}>
            {act && it.waitsOnId && <button className="btn sm" disabled={busy} onClick={() => onMsg(it)}>Send message</button>}
            {onLop && it.canLop && <button className="btn sm" disabled={busy} onClick={() => onLop(it)}>Mark as LOP</button>}
            {onHold && it.canHold && <button className="btn sm" disabled={busy} onClick={() => onHold(it)}>Hold salary</button>}
            {it.canVerify && <><button className="btn sm primary" disabled={busy} onClick={() => onVerify(it)}>Verify</button>
              <button className="btn sm" disabled={busy} onClick={() => onReject(it)}>Reject</button></>}
          </div></td>
        </tr>)}
      </tbody></table></div>{pg.pager}
    </div>
  );
}

/** Bank verification › PF / UAN: people who pay PF with no UAN on record. Their salary is held at the calculation and
 *  released at the next one once the UAN is entered (owner, 14 Nov 2026). Not pending. */
function UanMissing({ items, more }: { items: CItem[]; more: number }) {
  const pg = usePage(items, 25);
  return (
    <div style={{ marginBottom: 12 }} data-testid="uan-missing">
      <div style={{ fontWeight: 700, fontSize: 13, margin: '4px 0 6px' }}>PF / UAN: UAN not provided ({items.length + Math.max(0, more)})</div>
      <p className="mute" style={{ fontSize: 12, margin: '0 0 6px' }}>PF applies and no UAN is on record. The salary is held when payroll is calculated (Salary holds, "UAN not provided") and released at the next calculation once the UAN is entered. These do not stop the closure from closing.{more > 0 ? ` ${more} more ${more === 1 ? 'is' : 'are'} about people outside your access.` : ''}</p>
      <div className="tw"><table><thead><tr><th>Employee</th><th>Salary</th></tr></thead><tbody>
        {(pg.rows ?? []).map((it) => <tr key={it.ref}>
          <td><b>{it.name}</b><div className="mono mute" style={{ fontSize: 12 }}>{it.employeeId}</div></td>
          <td>{it.holdId ? <><span className="mono">{it.holdId}</span> <Pill k="bad">On hold</Pill> <span className="mute" style={{ fontSize: 12 }}>{it.when}</span></> : it.note?.includes('switched off') ? <Pill k="n">Not held</Pill> : <Pill k="warn">Held at the calculation</Pill>}
            {it.note && <div className="mute" style={{ fontSize: 12 }}>{it.note}</div>}</td>
        </tr>)}
      </tbody></table></div>{pg.pager}
    </div>
  );
}

/** Bank verification: people with no bank account whose salary for the month is on hold (not pending). */
function OnHold({ items, more }: { items: CItem[]; more: number }) {
  const pg = usePage(items, 25);
  return (
    <div style={{ marginBottom: 12 }}>
      <div style={{ fontWeight: 700, fontSize: 13, margin: '4px 0 6px' }}>On hold ({items.length + Math.max(0, more)})</div>
      <p className="mute" style={{ fontSize: 12, margin: '0 0 6px' }}>No bank account on record, and the salary for the month is held, so these are not pending. Paid as held salary released once an account is on record; Payroll can also release a hold in Salary holds.{more > 0 ? ` ${more} more ${more === 1 ? 'is' : 'are'} about people outside your access.` : ''}</p>
      <div className="tw"><table><thead><tr><th>Employee</th><th>Hold</th><th>Months held</th></tr></thead><tbody>
        {(pg.rows ?? []).map((it) => <tr key={it.ref}>
          <td><b>{it.name}</b><div className="mono mute" style={{ fontSize: 12 }}>{it.employeeId}</div></td>
          <td><span className="mono">{it.holdId}</span> <Pill k="bad">On hold</Pill>{it.note && <div className="mute" style={{ fontSize: 12 }}>{it.note}</div>}</td>
          <td style={{ whiteSpace: 'nowrap' }}>{it.when}</td>
        </tr>)}
      </tbody></table></div>{pg.pager}
    </div>
  );
}

/** Messages to the people items wait on, each editable before it is sent. */
function MessageDialog({ title, list, busy, onSend, onClose }: { title: string; list: CMsg[]; busy: boolean; onSend: (l: { to: string; refs: string[]; text: string }[]) => void; onClose: () => void }) {
  const [texts, setTexts] = useState(list.map((m) => m.text));
  return (
    <Modal wide onClose={onClose}>
      <div className="row sp"><h2>{title}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 0' }}>An in-app message: it shows under the bell at the top and on the Dashboard, and is marked done when the item is raised, approved or rejected. Edit the text before sending.</p>
      <div style={{ maxHeight: '60vh', overflow: 'auto' }}>
        {list.map((m, i) => <label key={m.to + i} className="field" style={{ marginTop: 12 }}><span>To {m.toName} ({m.as}){m.refs.length > 1 ? ` · ${m.refs.length} items` : ''}</span>
          <textarea rows={3} value={texts[i]} onChange={(e) => setTexts(texts.map((t, j) => (j === i ? e.target.value : t)))} /></label>)}
      </div>
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button>
        <button className="btn primary" disabled={busy || texts.some((t) => t.trim().length < 3)} onClick={() => onSend(list.map((m, i) => ({ to: m.to, refs: m.refs, text: texts[i].trim() })))}>
          {list.length === 1 ? 'Send message' : `Send ${list.length} messages`}</button></div>
    </Modal>
  );
}

/** A checklist action that needs a reason (reopen, mark as loss of pay, hold a salary, rejecting a bank change) or takes an
 *  optional note (reject at the cut-off). Marking missing punches as loss of pay also asks for a full or a half day. */
function AskDialog({ a, busy, onOk, onClose }: { a: Ask; busy: boolean; onOk: (v: string, half: boolean) => void; onClose: () => void }) {
  const [v, setV] = useState(a.initial ?? '');
  const [half, setHalf] = useState(false);
  const five = a.action === 'reopen' || a.action === 'mark-lop' || a.action === 'hold';
  const short = five ? v.trim().length < 5 : v.trim().length < 3;
  return (
    <Modal onClose={onClose}>
      <h2>{a.title}</h2>
      <p className="mute" style={{ fontSize: 12.5 }}>{a.text}</p>
      {!!a.punches && <div className="field" style={{ marginBottom: 10 }}><span>{a.punches === 1 ? 'The missing punch counts as' : `The ${a.punches} missing punches count as`}</span>
        <div className="seg" role="radiogroup" aria-label="Loss of pay for a missing punch">
          <button type="button" role="radio" aria-checked={!half} className={half ? '' : 'on'} onClick={() => setHalf(false)}>Full day</button>
          <button type="button" role="radio" aria-checked={half} className={half ? 'on' : ''} onClick={() => setHalf(true)}>Half day</button></div>
        <div className="mute" style={{ fontSize: 12, marginTop: 4 }}>{half ? 'Half a day of loss of pay; the punched half is paid as worked.' : 'A whole day of loss of pay.'}{a.absences ? ' Absences stay a full day.' : ''}</div></div>}
      <label className="field"><span>{a.optional ? 'Note' : 'Reason'}</span><textarea rows={3} value={v} placeholder={a.optional ? 'Closed at payroll cut-off' : ''} onChange={(e) => setV(e.target.value)} /></label>
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button>
        <button className={`btn ${five ? 'primary' : 'bad'}`} disabled={busy || (!a.optional && short)} onClick={() => onOk(v.trim(), half)}>{a.ok}</button></div>
    </Modal>
  );
}

/** Payroll run (prototype vCycle): stepper, pilot, checks, filters, the payroll sheet, bulk correction and the register. */
export function PayrollRun({ onArrear }: { onArrear?: (employeeId: string) => void } = {}) {
  const st = usePayState();
  const [pick, setPick] = useState('');
  const [ver, setVer] = useState(0); // bumped on every reload of the run, so the register below shows an approval at once
  if (st.error) return <div className="note bad">{st.error}</div>;
  if (!st.data) return <Loading error={st.error} />;
  const s = st.data;
  const months = [s.openYm, ...(s.waiting ? [s.waiting.ym] : [])];
  const ym = pick && months.includes(pick) ? pick : s.waiting ? s.waiting.ym : s.openYm;
  return (
    <>
      {months.length > 1 && <div className="row" style={{ marginBottom: 10 }}>
        <span className="mute" style={{ fontSize: 13 }}>Month</span>
        <div className="seg">{months.map((m) => <button key={m} className={m === ym ? 'on' : ''} onClick={() => setPick(m)}>{ymLabel(m)}</button>)}</div>
      </div>}
      <RunMonth key={ym} ym={ym} state={s} reloadState={() => { st.reload(); setVer((v) => v + 1); }} onArrear={onArrear} />
      <Register ver={ver} />
    </>
  );
}

const sumAmt = (l: { amount: number }[] = []) => l.reduce((a, x) => a + x.amount, 0);

function RunMonth({ ym, state, reloadState, onArrear }: { ym: string; state: State; reloadState: () => void; onArrear?: (id: string) => void }) {
  const nav = useNavigate();
  const m = useLoad(() => api<Check>(`/payroll/${ym}/checklist`), [ym]);
  const [f, setF] = useState<Record<string, string>>({ q: '', city: '', location: '', type: '', dept: '', flag: '' });
  const [q, setQ] = useState('');
  const [page, setPage] = useState(1);
  const [size, setSize] = useState(50);
  const rows = useLoad(() => api(`/payroll/${ym}/rows?` + new URLSearchParams({ ...f, page: String(page), size: String(size) })), [ym, JSON.stringify(f), page, size]);
  const checks = useLoad(() => api<any[]>(`/payroll/${ym}/checks`), [ym, rows.data?.scoped, m.data?.month?.computedAt]);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const locs = useLoad(() => api<any[]>('/masters/location?status=Active'), []);
  const depts = useLoad(() => api<any[]>('/masters/department?status=Active'), []);
  const [adj, setAdj] = useState<any>(null);
  const [slip, setSlip] = useState('');
  const [busy, setBusy] = useState(false);
  if (m.error) return <div className="note bad">{m.error}</div>;
  if (!m.data) return <Loading error={m.error} />;
  const mo = m.data.month, status: string = mo.status, si = STEPS.indexOf(status);
  // Salary is processed only once the checklist's five closures are closed (the server refuses it too).
  const gate = m.data.allClosed ? '' : `Close the checklist first. Still open: ${m.data.open.join(', ')}`;
  const ed = state.canRun, ap = state.canApprove;
  const editable = ed && (status === 'Open' || status === 'Dry run');
  const reload = () => { m.reload(); rows.reload(); checks.reload(); reloadState(); };
  const step = async (k: string, msg: string) => {
    setBusy(true);
    try { const r = await api(`/payroll/${ym}/${k}`, { method: 'POST', body: {} }); toast(r.queued ? `Running in the background for ${r.employees} people` : msg); reload(); }
    catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const pilot = async (ids: string[]) => {
    try { await api(`/payroll/${ym}/pilot`, { method: 'PUT', body: { ids } }); toast(ids.length ? `Payroll will run for ${ids.length} employees` : 'Payroll will run for everyone'); reload(); } catch (e: any) { toast(e.message); }
  };
  const setFilter = (k: string, v: string) => { setF({ ...f, [k]: v, ...(k === 'city' ? { location: '' } : {}) }); setPage(1); };
  const r = rows.data;
  const all: number = r?.scoped ?? 0, has = si >= 1 && all > 0;
  const filtered = !!(f.q || f.city || f.location || f.type || f.dept || f.flag);
  const pages = r ? Math.max(1, Math.ceil(r.total / size)) : 1;
  const dl = (view: boolean) => download(`/payroll/${ym}/sheet.xlsx?` + new URLSearchParams(view ? f : {}), `payroll-sheet-${ym}.xlsx`).catch((e) => toast(e.message));
  const paidDays = async (x: any, v: string) => {
    const pd = v === '' ? null : +v;
    if (pd === x.paid || (pd == null && !x.edited)) return;
    try { await api(`/payroll/${ym}/rows/${x.employeeId}`, { method: 'PATCH', body: { paidDays: pd, otherEarn: x.oe || 0, otherDed: x.od || 0, reason: x.reason || 'Paid days edited' } }); reload(); } catch (e: any) { toast(e.message); rows.reload(); }
  };
  const flagPills = (x: any) => <>
    {x.hold && <Pill k="bad">On hold</Pill>} {x.flags.includes('edited') && <Pill k="warn">Days edited</Pill>} {(x.oe || x.od) ? <Pill k="warn">Adjusted</Pill> : null}{' '}
    {x.flags.includes('miss') && <Pill k="warn">{x.missing} missing punch</Pill>} {x.flags.includes('var') && <Pill k="warn">Net moved</Pill>}
  </>;
  const btns = status === 'Open' ? (ed && <button className="btn primary" disabled={busy || !!mo.job || !!gate} title={gate} onClick={() => step('dry-run', 'Dry run ready')}>Run dry run</button>)
    : status === 'Dry run' ? (ed && <><button className="btn" disabled={busy || !!mo.job || !!gate} title={gate} onClick={() => step('dry-run', 'Recalculated')}>Recalculate</button>
      <button className="btn primary" disabled={busy || !!mo.job || !!gate || !!mo.pilot?.length} title={gate || (mo.pilot?.length ? 'Run for everyone before verifying' : '')} onClick={() => step('verify', 'Marked verified')}>Mark verified</button></>)
      : status === 'Verified' ? <>{ed && <button className="btn" disabled={busy} onClick={() => step('back', 'Sent back to the dry run')}>Send back to dry run</button>}
        {ap && <button className="btn primary" disabled={busy || !!mo.job || !!gate} title={gate} onClick={() => step('approve', 'Register updated')}>Approve and update register</button>}</>
        : status === 'Approved' ? (ap && <button className="btn primary" disabled={busy || !!gate} title={gate} onClick={() => confirm('Publish payslips to employees? This locks the month.') && step('publish', 'Payslips published to employees')}>Publish payslips</button>) : null;
  return (
    <>
      <Checklist x={m.data} reload={reload} />
      <div className="row sp" style={{ marginBottom: 14, flexWrap: 'wrap' }}>
        <div className="chain">{STEPS.map((x, i) => <span key={x} style={{ display: 'contents' }}>{i > 0 && <span className="arrow">→</span>}<span className={`step${i < si ? ' done' : i === si ? ' cur' : ''}`}>{x}</span></span>)}<span className="mute" style={{ marginLeft: 6 }}>{ymLabel(ym)}</span></div>
        <div className="row">{btns}</div>
      </div>
      {mo.job && <div className="note warn" style={{ marginBottom: 14 }}>Calculating {ymLabel(ym)} in the background ({mo.job}). <button className="btn sm" onClick={reload}>Refresh</button></div>}
      {mo.jobError && <div className="note bad" style={{ marginBottom: 14 }}>The last background run failed: {mo.jobError}</div>}
      <Pilot ed={editable} status={status} current={mo.pilot || []} onSet={pilot} />
      {gate && status !== 'Published' && <div className="note warn" style={{ marginBottom: 14 }}>Salary for {ymLabel(ym)} cannot be processed until all five closures in the checklist are closed. Still open: {m.data.open.join(', ')}.</div>}
      {mo.stale && all > 0 && si <= 2 && <div className="note warn" style={{ marginBottom: 14 }}>Pay lines, holds, edits, gross, attendance, leave, bank details or pay policies changed after the last calculation. {status === 'Verified' ? 'Send it back to the dry run and run it again before approving.' : 'Recalculate to include them.'}</div>}
      {has && r ? <>
        <div className="card" style={{ marginBottom: 14 }}><h3>Checks before you verify</h3><ul className="log" style={{ margin: 0 }}>
          {(checks.data ?? []).map((c) => <li key={c.title}>{c.ok ? <Pill k="ok">OK</Pill> : <Pill k="warn">Review</Pill>} <b>{c.title}</b><div className="mute" style={{ fontSize: 12.5 }}>{c.detail}</div></li>)}
        </ul></div>
        <div className="card" style={{ marginBottom: 14 }}><div className="row" style={{ flexWrap: 'wrap', gap: 8 }}>
          <input type="search" value={q} placeholder="Search name or ID, press Enter" style={{ minWidth: 220, maxWidth: 260 }} onChange={(e) => { setQ(e.target.value); if (!e.target.value && f.q) setFilter('q', ''); }}
            onKeyDown={(e) => e.key === 'Enter' && setFilter('q', q.trim())} onBlur={() => q.trim() !== f.q && setFilter('q', q.trim())} />
          <select value={f.city} onChange={(e) => setFilter('city', e.target.value)}><option value="">All cities</option>{(cities.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select>
          <select value={f.location} onChange={(e) => setFilter('location', e.target.value)}><option value="">All locations</option>{(locs.data ?? []).filter((l) => !f.city || l.city_id === f.city).map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</select>
          <select value={f.type} onChange={(e) => setFilter('type', e.target.value)}><option value="">All types</option>{['Permanent', 'Retainer', 'Contractual'].map((x) => <option key={x}>{x}</option>)}</select>
          <select value={f.dept} onChange={(e) => setFilter('dept', e.target.value)}><option value="">All departments</option>{(depts.data ?? []).map((d) => <option key={d.id} value={d.name}>{d.name}</option>)}</select>
          <select value={f.flag} onChange={(e) => setFilter('flag', e.target.value)}><option value="">Any flag</option>{state.flags.map((x) => <option key={x.Key} value={x.Key}>{x.Label}</option>)}</select>
          {filtered && <button className="btn sm" onClick={() => { setF({ q: '', city: '', location: '', type: '', dept: '', flag: '' }); setQ(''); setPage(1); }}>Clear filters</button>}
        </div></div>
        <div className="grid g4" style={{ marginBottom: 14 }}>
          <div className="card stat"><div className="l">{filtered ? 'Employees in this view' : 'Employees'}</div><div className="n">{r.total}</div><div className="mute" style={{ fontSize: 12 }}>of {all}</div></div>
          <div className="card stat"><div className="l">Total earnings</div><div className="n">{inr(r.sum.earned)}</div></div>
          <div className="card stat"><div className="l">Deductions</div><div className="n">{inr(r.sum.ded)}</div></div>
          <div className="card stat"><div className="l">Net payable</div><div className="n">{inr(r.sum.net)}</div><div className="mute" style={{ fontSize: 12 }}>{r.sum.held ? `${r.sum.held} on hold, ${inr(r.sum.heldNet)} not included` : 'None on hold'}</div></div>
        </div>
        {(status === 'Approved' || status === 'Published') && <BankFiles ym={ym} />}
        <div className="card" style={{ marginBottom: 14 }}>
          <div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap' }}><h3 style={{ margin: 0 }}>Payroll sheet</h3>
            <div className="row"><button className="btn" onClick={() => dl(true)}>Download {filtered ? `this view (${r.total})` : `all (${all})`}</button>{filtered && <button className="btn" onClick={() => dl(false)}>Download all ({all})</button>}
              <FullRegisterButton ym={ym} status={status} /></div></div>
          <div className="tw"><table><thead><tr><th>Employee</th><th>Type</th><th>City</th><th className="num">Paid days</th><th className="num">Earnings</th><th className="num">Extra earnings</th><th className="num">Deductions</th><th className="num">Net</th><th>Flags</th><th></th></tr></thead><tbody>
            {r.rows.map((x: any) => <tr key={x.employeeId}>
              <td><b>{x.name}</b> <span className="mono mute">{x.employeeId}</span></td><td>{x.type}</td><td>{x.city}</td>
              <td className="num">{editable ? <input key={`${x.employeeId}-${x.paid}`} type="number" step="0.5" min="0" max={x.days} defaultValue={x.paid} style={{ width: 70 }}
                onBlur={(e) => paidDays(x, e.target.value)} onKeyDown={(e) => e.key === 'Enter' && (e.target as HTMLInputElement).blur()} /> : x.paid}</td>
              <td className="num">{inr(x.totalEarn)}</td><td className="num">{inr(sumAmt(x.add) + (x.oe || 0))}</td><td className="num">{inr(x.totalDed)}</td><td className="num"><b>{inr(x.net)}</b></td>
              <td>{flagPills(x)}</td>
              <td><button className="btn sm" onClick={() => nav(`/file?id=${x.employeeId}`)}>File</button>{' '}
                {x.type === 'Permanent' && <><button className="btn sm" onClick={() => setSlip(x.employeeId)}>Slip</button>{' '}</>}
                {editable && <button className="btn sm" onClick={() => setAdj(x)}>Adjust</button>}</td>
            </tr>)}
            {!r.rows.length && <tr><td colSpan={10} className="mute">No employee matches these filters.</td></tr>}
          </tbody></table></div>
          <div className="row sp" style={{ marginTop: 10, flexWrap: 'wrap' }}><span className="mute" style={{ fontSize: 12.5 }}>{r.total ? `Showing ${(page - 1) * size + 1} to ${Math.min(r.total, page * size)} of ${r.total}` : 'No rows'}</span>
            <div className="row"><button className="btn sm" disabled={page <= 1} onClick={() => setPage(page - 1)}>Previous</button><span className="mute" style={{ fontSize: 12.5 }}>Page {page} of {pages}</span><button className="btn sm" disabled={page >= pages} onClick={() => setPage(page + 1)}>Next</button></div></div>
          <div className="row" style={{ marginTop: 6, gap: 8, alignItems: 'center' }}><span className="mute" style={{ fontSize: 12.5 }}>Rows per page</span>
            <select value={size} onChange={(e) => { setSize(+e.target.value); setPage(1); }}>{[25, 50, 100, 200].map((n) => <option key={n} value={n}>{n}</option>)}</select></div>
        </div>
        {editable && <Upload ym={ym} onDone={reload} />}
      </> : si >= 1 && !r ? <Loading error={rows.error} />
        : <div className="card"><p className="mute" style={{ margin: 0 }}>Run the dry run to calculate everyone from attendance, arrears, reimbursements and recoveries. Nothing is final until it is approved.</p></div>}
      {adj && <AdjustModal ym={ym} x={adj} onArrear={onArrear ? () => { setAdj(null); onArrear(adj.employeeId); } : undefined} onClose={() => setAdj(null)} onSaved={() => { setAdj(null); reload(); }} />}
      {slip && <PayslipPreview ym={ym} id={slip} onClose={() => setSlip('')} />}
    </>
  );
}

/** Prototype vPilot: try payroll on a small group first, or show the pilot that is running. */
function Pilot({ ed, status, current, onSet }: { ed: boolean; status: string; current: string[]; onSet: (ids: string[]) => void }) {
  const [ids, setIds] = useState('');
  if (current.length) return <div className="note" style={{ marginBottom: 14 }}><div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}>
    <span><b>Pilot run:</b> payroll covers {current.length} selected employee{current.length === 1 ? '' : 's'} only.</span>
    {ed && <button className="btn sm" onClick={() => onSet([])}>Run for everyone</button>}</div></div>;
  if (!ed || status !== 'Open') return null;
  const go = () => { const l = ids.split(/[\s,]+/).map((x) => x.trim().toUpperCase()).filter(Boolean); if (!l.length) { toast('Enter at least one employee ID'); return; } onSet(l); };
  return <div className="note" style={{ marginBottom: 14 }}>
    <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}><span>Try payroll on a small group first.</span></div>
    <details className="adv" style={{ marginTop: 8 }}><summary>Or pick your own employees</summary><div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
      <input type="text" value={ids} onChange={(e) => setIds(e.target.value)} placeholder="Employee IDs separated by commas, for example EMP-1001, EMP-1004" style={{ flex: 1, minWidth: 280 }} />
      <button className="btn" onClick={go}>Run for these employees</button></div></details>
  </div>;
}

/** Prototype cyAdj: other earning and other deduction for one employee, with a reason. */
function AdjustModal({ ym, x, onArrear, onClose, onSaved }: { ym: string; x: any; onArrear?: () => void; onClose: () => void; onSaved: () => void }) {
  const [v, setV] = useState({ e: x.oe ? String(x.oe) : '', d: x.od ? String(x.od) : '', r: x.reason || '' });
  const save = async () => {
    const e = +v.e || 0, d = +v.d || 0, why = v.r.trim();
    if ((e || d) && !why) { toast('Give a reason'); return; }
    const body = { paidDays: x.edited ? x.paid : null, otherEarn: e, otherDed: d, reason: why || (x.edited ? x.reason || 'Paid days edited' : '') };
    try { await api(`/payroll/${ym}/rows/${x.employeeId}`, { method: 'PATCH', body }); toast('Saved'); onSaved(); } catch (er: any) { toast(er.message); }
  };
  const lines = [...(x.add || []).map((l: any) => ({ ...l, sign: '+' })), ...(x.ded || []).map((l: any) => ({ ...l, sign: '−' }))];
  return (
    <Modal onClose={onClose}>
      <div className="row sp"><h2>Adjust: {x.name}</h2><div className="row">{onArrear && <button className="btn" onClick={onArrear}>Add an arrear</button>}<button className="btn sm" onClick={onClose}>Close</button></div></div>
      {lines.length > 0 && <ul className="log" style={{ margin: '10px 0' }}>{lines.map((l: any, i: number) => <li key={i}>{l.label}: <b>{l.sign}{inr(l.amount)}</b>{l.reason && <div className="mute" style={{ fontSize: 12 }}>{l.reason}</div>}</li>)}</ul>}
      <p className="mute" style={{ fontSize: 12.5 }}>Computed net {inr(x.net - (x.oe || 0) + (x.od || 0))}. An adjustment is added after the statutory calculation and needs a reason.</p>
      <div className="form">
        <Field label="Other earning (₹)"><input type="number" value={v.e} onChange={(e) => setV({ ...v, e: e.target.value })} /></Field>
        <Field label="Other deduction (₹)"><input type="number" value={v.d} onChange={(e) => setV({ ...v, d: e.target.value })} /></Field>
        <label className="field full"><span>Reason</span><input type="text" value={v.r} onChange={(e) => setV({ ...v, r: e.target.value })} /></label>
      </div>
      <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={save}>Save</button></div>
    </Modal>
  );
}

/** Prototype slipPrev: one employee's payslip for a month, in any state of the run (GET /payslip-preview:
 *  the dry-run row before publishing, the published payslip after). ym defaults to the open month. */
export function PayslipPreview({ ym, id, onClose }: { ym?: string; id: string; onClose: () => void }) {
  const d = useLoad(() => api<SlipView>(`/payslip-preview?` + new URLSearchParams({ employeeId: id, ...(ym ? { ym } : {}) })), [ym, id]);
  const box = useRef<HTMLDivElement>(null);
  const dl = () => { if (box.current && d.data) saveSlipHtml(box.current, `payslip-preview-${d.data.ym}-${id}.html`, `Payslip ${d.data.label}, ${id}`).catch((e) => toast(e.message)); };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Payslip preview</h2><div className="row">{d.data && <button className="btn" onClick={dl}>Download</button>}<button className="btn sm" onClick={onClose}>Close</button></div></div>
    <div ref={box} style={{ marginTop: 12 }}>{d.data ? <Payslip v={d.data} /> : <Loading error={d.error} />}</div>
  </Modal>;
}

/** Prototype vUpload: correct Paid days, Other earnings, Other deductions and Adjustment reason in a downloaded sheet. */
function Upload({ ym, onDone }: { ym: string; onDone: () => void }) {
  const [file, setFile] = useState<File | null>(null);
  const [text, setText] = useState('');
  const [res, setRes] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  const src = () => file ?? (text.trim() ? new File([text.trim() + '\n'], 'pasted-sheet.csv', { type: 'text/csv' }) : null);
  const run = async (a: 'check' | 'apply') => {
    const f = src();
    if (!f) return;
    setBusy(true);
    try {
      const r = await postForm(`/payroll/${ym}/sheet/${a}`, {}, f);
      setRes(r);
      if (r.applied) { toast(`${r.ready} employee${r.ready === 1 ? '' : 's'} updated`); setRes(null); setFile(null); setText(''); onDone(); }
    } catch (e: any) { setRes({ fatal: e.message }); } finally { setBusy(false); }
  };
  const okN = res && !res.fatal ? res.ready : 0, errN = res && !res.fatal ? res.problems : 0;
  return (
    <div className="card"><h3>Correct in bulk: upload a corrected sheet</h3>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Download the sheet (filter first to work on one city, location or flag), correct <b>Paid days</b>, <b>Other earnings</b>, <b>Other deductions</b> and <b>Adjustment reason</b> offline, and upload it here. Only those four columns are read, and the rest of the sheet can stay as it is. Nothing changes until you check the file and apply it.</p>
      <div className="row" style={{ flexWrap: 'wrap', gap: 8 }}>
        <label className="btn">Choose file<input type="file" accept=".xlsx,.csv,text/csv" hidden onChange={(e) => { setFile(e.target.files?.[0] ?? null); setRes(null); e.target.value = ''; }} /></label>
        <button className="btn primary" disabled={busy || !(file || text.trim())} onClick={() => run('check')}>Check file</button>
        {res && <><button className="btn primary" disabled={busy || !okN || errN > 0 || !!res.fatal} onClick={() => run('apply')}>Apply {okN} change{okN === 1 ? '' : 's'}</button>
          <button className="btn" onClick={() => { setRes(null); setFile(null); setText(''); }}>Clear</button></>}
        {file ? <span className="mute" style={{ fontSize: 12.5 }}>{file.name}</span> : text.trim() ? <span className="mute" style={{ fontSize: 12.5 }}>{text.trim().split('\n').length - 1} lines loaded</span> : null}
      </div>
      <Adv title="Or paste the sheet"><textarea rows={4} style={{ width: '100%' }} placeholder="Paste CSV with the header row" value={text} onChange={(e) => { setText(e.target.value); setFile(null); setRes(null); }} /></Adv>
      {res && <div style={{ marginTop: 14 }}>{res.fatal ? <div className="note bad">{res.fatal}</div> : <>
        <div className="row" style={{ gap: 10, flexWrap: 'wrap', marginBottom: 8 }}><Pill k="ok">{okN} with changes</Pill><Pill k="n">{res.unchanged ?? 0} unchanged</Pill>{errN > 0 && <Pill k="bad">{errN} with errors</Pill>}</div>
        <div className="tw"><table><thead><tr><th>Line</th><th>Employee</th><th>Result</th></tr></thead><tbody>
          {res.changes.slice(0, 150).map((c: any) => <tr key={c.row}><td className="mono">{c.row}</td><td>{c.id} <span className="mute">{c.name}</span></td>
            <td>{c.problem ? <Pill k="bad">{c.problem}</Pill> : (c.what || []).map((w: string) => <div key={w} style={{ fontSize: 12.5 }}>{w.charAt(0).toUpperCase() + w.slice(1)}</div>)}</td></tr>)}
          {!res.changes.length && <tr><td colSpan={3} className="mute">No changes found in the sheet.</td></tr>}
        </tbody></table></div>
        {res.changes.length > 150 && <p className="mute" style={{ fontSize: 12 }}>Showing the first 150 rows. All valid rows are applied.</p>}
      </>}</div>}
    </div>
  );
}

/** Whether the role view may download payroll files: Export payroll files at Edit and the role's download right (the
 *  rule of the register, the bank advice, the ICICI upload and the full register on the server). */
export function usePayExport() {
  const { me, can } = useAuth();
  return can('pay.export', 2) && !!me?.ctx.canDownload;
}

const dlErr = (e: any) => toast(e.message);
const monShort = (ym: string) => new Date(ym + '-01T00:00:00').toLocaleDateString('en-GB', { month: 'short' }) + '-' + ym.slice(0, 4);

/** "Download full register": every column of each person's salary for the month, from the dry run on (a DRAFT before
 *  approval). */
export function FullRegisterButton({ ym, status, label = 'Download full register', className = 'btn' }: { ym: string; status?: string; label?: string; className?: string }) {
  const ok = usePayExport();
  if (!ok || (status && !['Dry run', 'Verified', 'Approved', 'Published'].includes(status))) return null;
  const draft = status === 'Dry run' || status === 'Verified';
  return <button className={className} title={draft ? 'Not approved yet: the file is marked DRAFT' : undefined}
    onClick={() => download(`/payroll/${ym}/full-register.xlsx`, `full-salary-register-${monShort(ym)}${draft ? '-DRAFT' : ''}.xlsx`).catch(dlErr)}>{label}{draft ? ' (draft)' : ''}</button>;
}

type IciciCheck = { ym: string; file: string; rows: number; ft: number; neft: number; amount: number; debitAcc: string; narration: string; notIncluded: { employeeId: string; name: string; reason: string }[] };

/** The bank files of an approved or published month: the bank advice and ICICI's bulk upload, with the people the
 *  upload leaves out (and why) shown before the download, so Payroll can fix them and download again. */
export function BankFiles({ ym, card = true }: { ym: string; card?: boolean }) {
  const ok = usePayExport();
  const d = useLoad(() => (ok ? api<IciciCheck>(`/payroll/${ym}/icici-upload`) : Promise.resolve(null)), [ym, ok]);
  if (!ok) return null;
  const x = d.data, left = x?.notIncluded ?? [];
  const body = <>
    <div className="row sp" style={{ flexWrap: 'wrap', gap: 8 }}>{card && <h3 style={{ margin: 0 }}>Bank files, {ymLabel(ym)}</h3>}
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
        <button className="btn" onClick={() => download(`/payroll/${ym}/bank-advice.xlsx`, `bank-advice-${ym}.xlsx`).catch(dlErr)}>Bank advice</button>
        <button className="btn primary" disabled={!x} onClick={() => download(`/payroll/${ym}/icici-upload.xlsx`, x?.file || `ICICI-bank-upload-${monShort(ym)}.xlsx`).catch(dlErr)}>ICICI bank upload</button>
      </div></div>
    {d.error ? <div className="note" style={{ marginTop: 10 }}>{d.error}</div> : !x ? <Loading /> : <>
      <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 0' }}>ICICI upload: {x.rows} payment{x.rows === 1 ? '' : 's'}, {inr(x.amount)} ({x.ft} within ICICI as FT, {x.neft} by NEFT), from debit account <span className="mono">{x.debitAcc}</span>, narration “{x.narration}”. People on salary hold or with no net pay are not paid, as in the bank advice. The debit account is set in Payroll › Pay schedule.</p>
      {left.length > 0 && <div className="note warn" style={{ marginTop: 10 }}>
        <b>{left.length} {left.length === 1 ? 'person is' : 'people are'} not in the ICICI upload.</b> Fix the bank details and download again; the file also lists them on a second sheet, “Not included”, which ICICI does not read.
        <div className="tw" style={{ marginTop: 8 }}><table><thead><tr><th>Employee</th><th>Code</th><th>Reason</th></tr></thead><tbody>
          {left.map((p) => <tr key={p.employeeId}><td>{p.name}</td><td className="mono">{p.employeeId}</td><td>{p.reason}</td></tr>)}
        </tbody></table></div>
      </div>}
    </>}
  </>;
  return card ? <div className="card" style={{ marginBottom: 14 }}>{body}</div> : <div style={{ marginBottom: 12 }}>{body}</div>;
}

/** Prototype register: approved months, with each month's files. */
function Register({ ver }: { ver: number }) {
  const d = useLoad(() => api<any[]>('/payroll/register'), [ver]);
  const ex = usePayExport();
  return (
    <div className="card" style={{ marginTop: 14 }}>
      <h3>Payroll register</h3>
      <div className="tw"><table><thead><tr><th>Month</th><th className="num">Employees</th><th className="num">Net paid</th><th>Approved by</th><th>On</th>{ex && <th>Files</th>}</tr></thead><tbody>
        {(d.data ?? []).map((m) => <tr key={m.ym}><td><b>{ymLabel(m.ym)}</b></td><td className="num">{m.totals?.employees}</td><td className="num">{inr(m.totals?.net)}</td><td>{m.approved_by}</td><td className="mute">{m.approved_at}</td>
          {ex && <td><div className="row" style={{ gap: 6, flexWrap: 'wrap' }}><FullRegisterButton ym={m.ym} className="btn sm" label="Full register" />
            <button className="btn sm" onClick={() => download(`/payroll/${m.ym}/bank-advice.xlsx`, `bank-advice-${m.ym}.xlsx`).catch(dlErr)}>Bank advice</button>
            <button className="btn sm" onClick={() => download(`/payroll/${m.ym}/icici-upload.xlsx`, `ICICI-bank-upload-${monShort(m.ym)}.xlsx`).catch(dlErr)}>ICICI bank upload</button></div></td>}</tr>)}
        {d.data && !d.data.length && <tr><td colSpan={ex ? 6 : 5} className="mute">No month approved yet.</td></tr>}
      </tbody></table></div>
    </div>
  );
}
