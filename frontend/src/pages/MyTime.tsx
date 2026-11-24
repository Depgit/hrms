import { useState } from 'react';
import { api, fmtD, recentYm, today } from '../api';
import { useAuth } from '../auth';
import { Field, Loading, Pill, toast, useLoad } from '../components/ui';
import { RegularizeModal, extraPills, hrs, ymLabel } from './Attendance';

const tone = (t: string) => (t || 'n') as 'ok' | 'warn' | 'bad' | 'info' | 'n';
const DN = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

/** My attendance register: every day of the month with its status, and the loss of pay it adds up to (prototype vAttEss). */
export function MyAttendance() {
  const { me } = useAuth();
  const [ym, setYm] = useState(recentYm());
  const [reg, setReg] = useState<any>(null);
  const { data, error, reload } = useLoad(() => api(`/attendance/month/me?ym=${ym}`), [ym]);
  const m = data?.month, c = m?.counts ?? {};
  const pend = new Set<string>((data?.pending ?? []).map((x: string) => x.slice(0, 10)));
  const fix = (d: any) => ['No punch', 'Late'].includes(d.status) || /Missing/.test(d.status) || /^Absent/.test(d.status) || !!d.reg; // reg: beyond a deviation allowance with an action
  const stat = (l: string, v: number) => <div className="card stat"><div className="l">{l}</div><div className="n">{v}</div></div>;
  return <>
    <div className="row" style={{ marginBottom: 12, gap: 10, flexWrap: 'wrap' }}><Field label="Month"><input type="month" value={ym} max={today().slice(0, 7)} onChange={(e) => e.target.value && setYm(e.target.value)} /></Field></div>
    {!m ? <Loading error={error} /> : <>
      <div className="grid g4" style={{ marginBottom: 14 }}>{stat('Present', (c.Present || 0) + (c.Late || 0))}{stat('Late marks', c.Late || 0)}{stat('Leave', (c['Paid leave'] || 0) + (c['Loss of pay leave'] || 0))}{stat('Loss of pay days', m.lop)}</div>
      {extraPills(c).length > 0 && <div className="row" style={{ gap: 8, marginBottom: 12, flexWrap: 'wrap' }}>{extraPills(c)}</div>}
      <div className="card"><div className="tw"><table><thead><tr><th>Date</th><th>Day</th><th>Status</th><th>In</th><th>Out</th><th className="num">Hours</th><th>Note</th><th></th></tr></thead><tbody>
        {m.days.map((d: any) => <tr key={d.date}>
          <td>{fmtD(d.date)}</td><td>{DN[new Date(d.date + 'T00:00:00').getDay()]}</td><td><Pill k={tone(d.tone)}>{d.status}</Pill></td><td>{d.in || '—'}</td><td>{d.out || '—'}</td><td className="num">{hrs(d.in, d.out)}</td>
          <td className="mute" style={{ fontSize: 12.5 }}>{d.leave || ''}{pend.has(d.date) ? 'Request waiting for approval' : ''}{d.src === 'Regularized' ? ' Regularized' : ''}{d.note ? <div>{d.note}</div> : null}</td>
          <td>{fix(d) && !pend.has(d.date) && d.date <= today() && <button className="btn sm" onClick={() => setReg({ employeeId: me!.ctx.employeeId, name: me!.ctx.name, date: d.date, rec: d })}>Regularize</button>}</td></tr>)}
      </tbody></table></div></div>
    </>}
    {reg && <RegularizeModal {...reg} onClose={() => { setReg(null); reload(); }} />}
  </>;
}

const band = (b: string) => (b === 'Good' ? 'ok' : b === 'Watch' ? 'warn' : 'bad') as 'ok' | 'warn' | 'bad';
const consState = (c: any): [string, 'ok' | 'warn' | 'bad' | 'info' | 'n'] => (c.status === 'Active' && c.to_date && c.to_date < today() ? ['Review due', 'warn']
  : c.status === 'Completed' ? [c.outcome === 'Improved' ? 'Completed, improved' : 'Completed, not improved', c.outcome === 'Improved' ? 'ok' : 'bad']
  : [c.status, c.status === 'Issued' || c.status === 'Active' ? 'info' : 'n']);

/** Download a warning letter or plan as a page to keep or print. */
export async function letterFile(id: number, name: string) {
  try {
    const l = await api(`/consequences/${id}/letter`);
    const esc = (s: string) => s.replace(/[&<>]/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' })[ch]!);
    const html = `<!doctype html><html><head><meta charset="utf-8"><title>${esc(l.title)}</title><style>body{font:14px/1.6 Arial,sans-serif;max-width:720px;margin:40px auto;padding:0 24px;color:#111;white-space:pre-wrap}</style></head><body>${esc(l.text)}</body></html>`;
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([html], { type: 'text/html' }));
    a.download = name; a.click();
    URL.revokeObjectURL(a.href);
  } catch (x: any) { toast(x.message); }
}

/** My roster discipline: score and deviations this month, then letters and plans on record (prototype vDiscEss). */
export function MyDiscipline() {
  const { me } = useAuth();
  const ym = recentYm();
  const { data, error } = useLoad(() => api(`/me/discipline?ym=${ym}`), [ym]);
  if (!data) return <Loading error={error} />;
  const cons = data.consequences.filter((c: any) => ['Issued', 'Active', 'Completed'].includes(c.status));
  return <>
    <div className="card"><div className="row sp"><div><h3 style={{ margin: 0 }}>Roster discipline, {ymLabel(ym)}</h3><div className="mute" style={{ fontSize: 12.5 }}>Your score starts at {data.start ?? 100}{data.startNote ? ` this month (${data.startNote}: 100 × the days you are employed in the month ÷ the days in it)` : ''}. It drops when you work a different shift or time from your roster and your manager marks it Not OK.{data.leaveBelow > 0 ? ` Below ${data.leaveBelow}, leave is not allowed.` : ''}</div></div>
      <div style={{ textAlign: 'right' }}><div style={{ fontSize: 30, fontWeight: 800 }}>{data.score}</div><Pill k={band(data.band)}>{data.band}</Pill>
        {data.start < 100 && <div className="mute" style={{ fontSize: 12, marginTop: 4 }}>Started at {data.start}{data.startNote ? ` (${data.startNote})` : ''}</div>}</div></div>
      <div className="tw" style={{ marginTop: 12 }}><table><thead><tr><th>Date</th><th>Planned</th><th>Actual</th><th>What differs</th><th>Decision</th></tr></thead><tbody>
        {data.deviations.map((d: any) => <tr key={d.date}><td>{fmtD(d.date)}</td><td>{d.dev.planned}</td><td>{d.dev.actual === 'Custom' ? 'Other time' : d.dev.actual}</td><td>{d.dev.kinds.join(', ')}</td>
          <td>{d.status === 'OK' || d.status === 'Not OK' ? <Pill k={d.status === 'OK' ? 'ok' : 'bad'}>{d.status}</Pill> : <Pill k="warn">Waiting</Pill>}</td></tr>)}
        {!data.deviations.length && <tr><td colSpan={5} className="mute">No differences from your roster this month.</td></tr>}
      </tbody></table></div></div>
    <div className="card" style={{ marginTop: 14 }}><h3 style={{ margin: '0 0 6px' }}>Warning letters and improvement plans</h3>
      {cons.length ? <div className="tw"><table><thead><tr><th>Type</th><th>Date</th><th>Reason</th><th>Status</th><th></th></tr></thead><tbody>
        {cons.map((c: any) => { const st = consState(c); return <tr key={c.id}><td>{c.kind === 'PIP' ? `PIP ${c.seq} · ${c.days} days` : `Warning letter ${c.seq}`}</td><td>{fmtD(c.issued_at)}</td><td>{c.reason}</td><td><Pill k={st[1]}>{st[0]}</Pill></td>
          <td><button className="btn" onClick={() => letterFile(c.id, `${c.kind === 'PIP' ? 'pip' : 'warning'}-${me?.ctx.employeeId}-${c.seq}.html`)}>Download</button></td></tr>; })}
      </tbody></table></div> : <p className="mute" style={{ margin: 0 }}>None on record.</p>}</div>
  </>;
}
