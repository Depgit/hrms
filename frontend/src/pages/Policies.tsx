import { useEffect, useState } from 'react';
import { addDays, addMonths, api, fmtD, today, ymOf } from '../api';
import { useAuth } from '../auth';
import { download, Loading, Modal, Pill, Switch, toast, useLoad } from '../components/ui';
import PersonPicker, { type Person } from '../components/PersonPicker';
import { NominationsAdmin } from './Nominations';
import CompanyPolicies from './CompanyPolicies';
import { BulkModal } from './Bulk';
import { DevAllowCard, devActText, devLabel } from './DevAllow';
import { PAID_DAYS } from './Approvals';

// Work policies (prototype vPol and everything under it). Policy settings, leave plans, holiday
// calendars, coverage, nominations and exceptions. Changes apply from the next request or roster.

/** One setting: [path, label, kind, options, hint, soft]. A soft (◦) setting is saved but not applied everywhere yet. */
type PF = [string, string, 'num' | 'yn' | 'sel' | 'time' | 'text' | 'hm', [string, string][] | null, string?, 1?];
const YN: [string, string][] = [['true', 'Yes'], ['false', 'No']];
/** Decimal hours as whole minutes (9.75 → 585), rounded so 8 h 20 min (8.333…) never drifts to 8 h 19 min. */
const hmMin = (v: any) => Math.max(0, Math.round((+v || 0) * 60));
/** Decimal hours in words: 8.5 → "8 h 30 min", 8 → "8 h". */
const hmText = (v: any) => { const t = hmMin(v); return t % 60 ? `${Math.floor(t / 60)} h ${t % 60} min` : `${t / 60} h`; };
/** Settings entered as hours and minutes and kept as decimal hours (owner, 24 Nov 2026). */
const HM_KEYS = ['fullH', 'halfH', 'hrs.spread', 'att.earlyInMax', 'att.lateOutMax'];

// The prototype's PS sections, as they stand after every later addition (roster discipline, deviations, rest).
const PS: Record<string, [string, PF[]][]> = {
  time: [
    ['Weekly off', [['sun', 'Sundays', 'sel', [['all', 'All Sundays off'], ['none', 'No fixed Sunday off']]], ['sat', 'Saturdays', 'sel', [['none', 'All Saturdays working'], ['2,4', '2nd and 4th off'], ['1,3', '1st and 3rd off'], ['all', 'All Saturdays off']]],
      ['offMode', 'Weekly off decided by', 'sel', [['fixed', 'Fixed days in this policy'], ['rotational', 'Roster manager, in Shift and roster']], 'For 24x7 properties. The roster itself is built under Shift and roster.'],
      ['wo.min', 'Minimum weekly offs in 7 days', 'num', null, '', 1], ['wo.consec', 'Maximum days worked in a row', 'num', null, 'The roster flags anyone scheduled beyond this.']]],
    ['Working hours', [['fullH', 'Full-day hours', 'hm', null], ['halfH', 'Half-day hours', 'hm', null], ['hrs.week', 'Maximum hours in a week', 'num', null, '48 under most state Shops and Establishments rules.', 1],
      ['hrs.spread', 'Maximum spread over in a day (hours)', 'hm', null, 'Time from first in to last out, including breaks.', 1]]],
    // The one place for rest and shift-change notice (8 Nov 2026): rest is att.minRest / att.restAct, the notice is rs.lead.
    ['Shifts and rest', [['att.minRest', 'Minimum rest between shifts (hours)', 'num', null, 'The roster blocks (or warns about) a shift that leaves less rest than this after the previous one. 0 switches it off.'],
      ['att.restAct', 'When a roster breaks the rest gap', 'sel', [['block', 'Do not allow the change'], ['warn', 'Allow it with a warning']], 'Do not allow: the roster refuses the shift. Allow it with a warning: the shift is saved and the warning is shown.'],
      ['rs.lead', 'Notice before a shift change (days)', 'num', null, 'A change made with less notice than this is a late change: it needs a reason and counts in the manager’s planning score.']]],
    ['Roster discipline', [['rs.reason', 'Reason required for a late change', 'yn', null], ['rs.alert', 'Flag a property when late changes pass this share of its changes (%)', 'num', null, 'Shown as an alert on the roster page and in the discipline report. Needs at least 5 changes in the month.'],
      ['rs.past', 'Dates that have already passed', 'sel', [['lock', 'Locked. Attendance follows the roster as it stood'], ['free', 'Open to edit']], 'A locked date cannot be changed. If the day was worked differently, the employee regularizes with the reason Roster changed.'],
      ['rs.bulkPast', 'Past dates in the roster bulk upload', 'sel', [['lock', 'Blocked. The upload never changes a past date'], ['same', 'Same as the roster screen (the rule above)']], 'Blocked: a file with a changed cell for a date before today is refused for that employee, whatever the rule above says. Same as the roster screen: past dates follow “Dates that have already passed”.'],
      ['rs.devTol', 'Late arrival: more than this many minutes after the planned start is a deviation', 'num', null, 'Inside this window the day follows the roster and only the usual late mark applies. Outside it, the system shows Planned against Actual and asks the manager to decide.'],
      ['rs.pts', 'Discipline points lost for each deviation marked Not OK', 'num', null, 'Every employee starts a month at 100, or pro-rata for part of the month: someone who joined on the 21st of a 30-day month starts at 33.'],
      ['rs.leaveBelow', 'Leave not allowed when the discipline score is below', 'num', null, '0 is off. While this month’s score is below it, every leave request is refused, whoever raises it. HR Admin, or the HRBP, can allow one request with a reason. Scaled pro-rata like the score.'],
      ['rs.devWin', 'Days a manager has to decide a deviation', 'num', null, 'After this the deviation shows as overdue.'],
      ['rs.mgrAlert', 'Flag a manager when deviations pass this share of the team’s rostered days (%)', 'num', null, 'A high share means the roster is not matching how the team really works.']]],
    ['What counts as a deviation', [['rs.earlyIn', 'Early check-in allowed (minutes before the planned start, no deviation)', 'num', null, 'Arriving up to this early is normal. Earlier than this is a deviation. 0 means any early arrival is a deviation.'],
      ['rs.earlyCount', 'Time worked before the planned start', 'sel', [['no', 'Not counted as work hours'], ['yes', 'Counted as work hours']], 'Applies to the early check-in allowance only.'],
      ['rs.outTol', 'Leaving early: more than this many minutes before the shift end is a deviation', 'num', null, '0 switches this off.'], ['rs.devStart', 'Arrival outside the allowed window is a deviation', 'yn', null]]],
  ],
  // At the end of the Working time tab.
  claims: [['Reimbursements', [['claims.on', 'Employees can claim reimbursements', 'yn', null, 'When No, My pay has no Reimbursements tab and employees cannot raise a claim. Payroll or HR can still raise one for them.']]]],
  hol: [['Holiday rules', [['optHol', 'Optional holidays per employee a year', 'num', null],
    ['hol.onOff', 'Holiday that falls on a weekly off', 'sel', [['once', 'Counted once'], ['extra', 'Extra day in lieu']], '', 1], ['hol.prob', 'Holidays paid in probation', 'yn', null, '', 1]]]],
  att: [
    ['Punching', [['att.mode', 'Allowed ways to punch', 'sel', [['any', 'Any method'], ['bio', 'Biometric only'], ['app', 'Geo-fenced app only'], ['both', 'Biometric or app']], '', 1], ['att.geo', 'Geo-fence radius (metres)', 'num', null, '', 1],
      ['att.selfie', 'Selfie required at punch', 'yn', null, '', 1], ['att.autoOut', 'If out punch is missing', 'sel', [['none', 'Leave as missing punch'], ['end', 'Auto-close at shift end']], '', 1],
      ['att.night', 'Night shift counts on', 'sel', [['start', 'Day the shift starts'], ['end', 'Day the shift ends']], 'Decides which date a shift that crosses midnight belongs to.', 1],
      // The punch window, moved here from each shift (owner, 24 Nov 2026). 0 h 0 min is no limit.
      ['att.earlyInMax', 'Maximum early in punch allowed', 'hm', null, 'A punch earlier than the shift start minus this is not taken as that day’s in punch. 0 h 0 min is no limit. Up to 12 h.'],
      ['att.lateOutMax', 'Maximum late out punch allowed', 'hm', null, 'A punch later than the shift end (the end of its second part for a split shift) plus this is not taken as that day’s out punch. 0 h 0 min is no limit. Up to 12 h.']]],
    ['Lateness and early exit', [['grace', 'Late-mark grace (minutes)', 'num', null], ['att.lateN', 'Late marks allowed in a month', 'num', null, '', 1], ['att.lateAct', 'After that limit', 'sel', [['half', 'Half day of leave or loss of pay'], ['warn', 'Warning only'], ['none', 'No action']], '', 1],
      ['att.early', 'Early-out grace (minutes)', 'num', null, '', 1]]],
    ['Absence and missing punches', [['att.missing', 'A day with a missing punch counts as', 'sel', [['half', 'Half day until regularized'], ['absent', 'Absent'], ['present', 'Present until regularized']], '', 1],
      ['att.absent', 'No punch at all counts as', 'sel', [['lop', 'Loss of pay after the regularization window'], ['lopnow', 'Loss of pay straight away'], ['present', 'Present']], '', 1],
      ['att.maxSpan', 'Longest gap between check-in and check-out (hours)', 'num', null, 'Above this the check-out is treated as a missing punch and that punch becomes the next check-in. Use 0 to switch it off. Keep it at 8 or more.']]],
    ['Field work and remote work', [['att.od', 'On-duty and field visits allowed', 'yn', null, 'For sales, purchase and city teams.', 1], ['att.wfh', 'Work from home allowed', 'yn', null, '', 1], ['att.wfhDays', 'Work-from-home days a month', 'num', null, '', 1]]],
  ],
  reg: [['Regularization window', [['rr.back', 'Apply within (days of the date)', 'num', null], ['rr.cutoff', 'Also stop at payroll cycle end', 'yn', null], ['rr.extra', 'Extra days after cycle last day', 'num', null], ['rr.max', 'Maximum requests a month, 0 for no limit', 'num', null],
    ['regCount', 'Counting for approval levels', 'sel', [['separate', 'No Punch and All Other separately'], ['combined', 'All regularizations together']]]]],
    ['Conditions', [['rg.reason', 'A reason is required', 'yn', null, '', 1], ['rg.mgr', 'Manager can regularize for the team', 'yn', null, '', 1]]]],
  exit: [['Probation', [['prob', 'Probation (months)', 'num', null], ['ex.ext', 'Extension allowed', 'yn', null, '', 1], ['ex.extMax', 'Longest extension (months)', 'num', null, '', 1], ['ex.review', 'Confirmation review is required', 'yn', null, '', 1], ['ex.auto', 'Confirm automatically if no review', 'yn', null, '', 1]]],
    ['Notice period', [['notice.prob', 'Default notice in probation (days)', 'num', null], ['notice.conf', 'Default notice after confirmation (days)', 'num', null]]],
    ['Exit', [['ex.buyout', 'Notice buy-out', 'sel', [['no', 'Not allowed'], ['approve', 'Allowed with approval'], ['yes', 'Allowed']], '', 1], ['ex.recover', 'Recover pay for short notice', 'yn', null, '', 1], ['ex.leave', 'Leave can be used to serve notice', 'sel', [['no', 'No'], ['hr', 'With HR approval'], ['yes', 'Yes']], '', 1],
      ['ex.withdraw', 'Resignation can be withdrawn within (days)', 'num', null, '', 1], ['ex.fnf', 'Full and final settlement within (days)', 'num', null, '', 1], ['ex.exitInt', 'Exit interview required', 'yn', null, '', 1],
      ['ex.waive', 'Notice waiver approved by', 'sel', [['HR Admin', 'HR Admin'], ['CHRO', 'CHRO'], ['Function Head', 'Function Head and HRBP']], '', 1]]]],
};
const PTAB: [string, string][] = [['sum', 'Summary'], ['time', 'Working time'], ['hol', 'Holidays'], ['att', 'Attendance'], ['reg', 'Regularization'], ['leave', 'Leave plan'], ['lrule', 'Blackout periods'], ['exit', 'Probation and notice'], ['map', 'Applies to'], ['hist', 'History']];
/** Mapping levels in the order they win when a person matches more than one policy (the server's policyLevels). */
const LVL: Record<string, string> = { employee: 'Employee', dept_location: 'Department + Location', dept_cluster: 'Department + Cluster', dept_city: 'Department + City', dept_state: 'Department + State',
  location: 'Location', cluster: 'Cluster', city: 'City', state: 'State', department: 'Department', designation: 'Designation', grade: 'Grade', type: 'Employment type' };
const ORDER_TEXT = 'Individual (employee) > Department + Location > Department + Cluster > Department + City > Department + State > Location > Cluster > City > State > Department > Designation > Grade > Employment type';
/** A department together with one place (owner, 24 Nov 2026): the place level of each combined level. Its ref is "<department>|<place id>". */
const COMBO: Record<string, string> = { dept_location: 'location', dept_cluster: 'cluster', dept_city: 'city', dept_state: 'state' };
/** A mapping's value in words: a place by its name, a combined mapping as "People Ops at Gurgaon office". */
const refLabel = (level: string, ref: string, names?: Record<string, string> | null) => {
  if (!COMBO[level]) return names?.[ref] ?? ref;
  const i = ref.lastIndexOf('|');
  return i < 0 ? ref : `${ref.slice(0, i)} at ${names?.[ref.slice(i + 1)] ?? ref.slice(i + 1)}`;
};
/** Settings removed on 8 Nov 2026 (overtime, comp-off, holiday work, break, late half day, extra-hours, rest and off-day
 * deviations, split shifts, and the old rest and notice fields) and on 24 Nov 2026 (the Night shift card: night hours,
 * the night shift allowance and night work by women). Older history entries about them are not shown. */
const REMOVED = /^(ot\.|compoff$|hol\.work$|att\.(brkMin|brkAfter|lateHalf)$|rs\.(otTol|devExtra|devRest|devOff)$|shift\.(split|rest|change|nightFrom|nightTo|nightAllow|women)$)/;
const SATL: Record<string, string> = { all: 'all Saturdays', '2,4': '2nd and 4th Saturdays', '1,3': '1st and 3rd Saturdays' };
const LT_EMP = ['Permanent', 'Retainer', 'Contractual'];

const getp = (o: any, path: string) => path.split('.').reduce((a, k) => (a == null ? undefined : a[k]), o);
const setp = (o: any, path: string, v: any) => { const a = path.split('.'); const l = a.pop()!; a.reduce((x, k) => (x[k] ??= {}), o)[l] = v; };
const flat = (o: any, pre = '', out: Record<string, string> = {}) => {
  Object.entries(o ?? {}).forEach(([k, v]) => { const p = pre ? pre + '.' + k : k; if (v && typeof v === 'object' && !Array.isArray(v)) flat(v, p, out); else out[p] = JSON.stringify(v); });
  return out;
};
const nChanges = (a: any, b: any) => { const x = flat(a), y = flat(b); return new Set([...Object.keys(x), ...Object.keys(y)].filter((k) => x[k] !== y[k])).size; };
const fmtAt = (s: string) => new Date(s).toLocaleString('en-IN', { day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' });
const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });
const iso = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const lastDayOf = (ym: string) => iso(new Date(+ym.slice(0, 4), +ym.slice(5, 7), 0));
const s_ = (n: number, one: string, many = one + 's') => `${n} ${n === 1 ? one : many}`;

const weekOffText = (s: any) => s.offMode === 'rotational' ? 'One rotational weekly off, set in the roster' : (s.sun === 'all' ? 'All Sundays' : 'No fixed Sunday off') + (SATL[s.sat] ? ' + ' + SATL[s.sat] : '');
const isWO = (s: any, d: Date) => {
  if (s.offMode === 'rotational') return false;
  const w = d.getDay();
  if (w === 0) return s.sun === 'all';
  if (w === 6) { const n = Math.ceil(d.getDate() / 7); return s.sat === 'all' || (s.sat === '2,4' && (n === 2 || n === 4)) || (s.sat === '1,3' && (n === 1 || n === 3)); }
  return false;
};
/** Offs in a month for the off cap (prototype monthOffsPol): a holiday on a weekly off counts once. */
const monthOffs = (s: any, hol: Set<string>, ym: string) => {
  const y = +ym.slice(0, 4), m = +ym.slice(5, 7), n = new Date(y, m, 0).getDate();
  let sun = 0, sat = 0, h = 0;
  for (let i = 1; i <= n; i++) { const d = new Date(y, m - 1, i), wd = d.getDay(); if (isWO(s, d)) { if (wd === 0) sun++; else if (wd === 6) sat++; else h++; } else if (hol.has(iso(d))) h++; }
  if (s.offMode === 'rotational') sun = Math.floor(n / 7);
  const c = s.offCap, total = (c.sun ? sun : 0) + (c.sat ? sat : 0) + (c.hol ? h : 0);
  return { sun, sat, hol: h, total, over: c.on && total > c.max };
};
/** Leave rules of one type from the policy's leave plan (prototype lpApply → p.lr). */
const lrOf = (plan: any, name: string) => { const c = plan?.types?.[name]; return c ? { adv: c.adv, back: c.advOnly ? 0 : c.back } : { adv: 0, back: 0 }; };
const ruleLines = (s: any, plan: any) => {
  const o = s.offCap, l = lrOf(plan, 'Casual leave'), pl = lrOf(plan, 'Privilege leave'), r = s.rr;
  return [`Off cap ${o.on ? o.max + ' a month' : 'not set'}`, `CL ${l.back ? 'up to ' + l.back + ' days after' : 'no back-dating'}${l.adv ? ', ' + l.adv + ' days notice' : ''} · PL ${pl.adv ? pl.adv + ' days notice' : 'no notice rule'}`,
    `Regularization within ${r.back} days${r.cutoff ? ', cycle end + ' + r.extra + ' days' : ''}`];
};
const lines = (l: string[]) => l.map((x, i) => <span key={i}>{i > 0 && <br />}{x}</span>);
const ltOf = (plan: any, n: string) => plan?.types?.[n] ?? { on: false };
/** Earn by working days (owner, 24 Nov 2026): the plan's rule a leave type is in (`lv.earn`), as { every, per }, or null. */
type Earn = { every: number; per: number };
const earnOf = (plan: any, n: string): Earn | null => {
  for (const r of plan?.lv?.earn ?? []) for (const t of r.types ?? []) if (t.type === n) return { every: +r.days || 0, per: +t.days || 0 };
  return null;
};
const dw = (v: number, w: string) => `${v} ${w}${v === 1 ? '' : 's'}`;
/** "1 day for every 7 paid days", with ", up to 8 days a year" when Days a year (cap) is set. */
const earnText = (b: Earn, cap?: number) => `${dw(b.per, 'day')} for every ${dw(b.every, 'paid day')}${+(cap ?? 0) > 0 ? `, up to ${dw(+cap!, 'day')} a year` : ''}`;
const clText = (plan: any) => { const c = ltOf(plan, 'Casual leave'), b = earnOf(plan, 'Casual leave'); return c.on ? (b ? `CL earned by working days, ${earnText(b, c.days)}` : `CL ${c.days} a year, ${c.credit === 'monthly' ? 'credited monthly from joining' : 'credited upfront'}`) : 'CL not applicable'; };
const plText = (plan: any) => { const c = ltOf(plan, 'Privilege leave'), b = earnOf(plan, 'Privilege leave'); return c.on ? `PL ${b ? `earned by working days, ${earnText(b, c.days)}` : `${c.days} a year`}, accrues ${c.from === 'prob' ? 'after probation is completed' : 'from joining'}${c.catchup ? ' with back-dated credit' : ''}` : 'PL not applicable'; };
const ltBrief = (kind: string, paid: boolean, c: any, b?: Earn | null) => !c.on ? 'Off' : kind === 'event' ? `${c.days} days an occasion` : kind === 'balance' ? (b ? `Earned by working days: ${earnText(b, c.days)}` : `${c.days} days a year`) : !paid ? 'Unpaid' : 'No balance';
const lpBrief = (pl: any, types: any[]) => {
  const on = types.filter((i) => i.kind === 'balance' && pl.types[i.name]?.on), short = (n: string) => n.replace(' leave', '');
  const fixed = on.filter((i) => !earnOf(pl, i.name)).map((i) => `${short(i.name)} ${pl.types[i.name].days}`), earned = on.filter((i) => earnOf(pl, i.name)).map((i) => short(i.name));
  const t = [...(fixed.length ? [fixed.join(' · ') + ' days a year'] : []), ...(earned.length ? [earned.join(', ') + ' by working days'] : [])];
  return t.length ? t.join(' · ') : 'No leave credited';
};

const legend = <p className="mute" style={{ fontSize: 12, margin: '0 0 12px' }}>A ◦ marks a setting that is saved with the policy. Attendance, roster and payroll processing will apply it in the full build. Settings without it are already applied.</p>;

/** A prototype fld(): label (◦ for soft settings), the control and its hint. */
function Fld({ f, v, set }: { f: PF; v: any; set: (v: any) => void }) {
  const [, l, kind, o, h, soft] = f;
  if (kind === 'hm') return <div className="field"><span>{l}{soft ? ' ◦' : ''}</span><HM v={v} set={set} label={l} />
    {h && <small className="mute" style={{ fontSize: 11.5, marginTop: 3, display: 'block' }}>{h}</small>}</div>;
  return <label className="field"><span>{l}{soft ? ' ◦' : ''}</span>
    {kind === 'sel' ? <select value={v ?? ''} onChange={(e) => set(e.target.value)}>{o!.map(([k, x]) => <option key={k} value={k}>{x}</option>)}</select>
      : kind === 'yn' ? <select value={String(!!v)} onChange={(e) => set(e.target.value === 'true')}>{YN.map(([k, x]) => <option key={k} value={k}>{x}</option>)}</select>
      : <input type={kind === 'num' ? 'number' : kind === 'time' ? 'time' : 'text'} value={v ?? ''} onChange={(e) => set(kind === 'num' ? (e.target.value === '' ? 0 : +e.target.value) : e.target.value)} />}
    {h && <small className="mute" style={{ fontSize: 11.5, marginTop: 3, display: 'block' }}>{h}</small>}</label>;
}

/** Hours and minutes (0–59) in two boxes, kept as decimal hours: 8 h 30 min = 8.5. */
function HM({ v, set, label }: { v: any; set: (v: number) => void; label: string }) {
  const t = hmMin(v), h = Math.floor(t / 60), m = t % 60;
  const put = (hh: number, mm: number) => set((hh * 60 + mm) / 60);
  const int = (x: string, max: number) => Math.min(max, Math.max(0, Math.floor(+x || 0)));
  return <div className="row" style={{ gap: 6, flexWrap: 'nowrap', alignItems: 'center' }}>
    <input type="number" min={0} max={24} step={1} aria-label={label + ', hours'} value={h} onChange={(e) => put(int(e.target.value, 24), m)} style={{ maxWidth: 90 }} /><span className="mute">h</span>
    <input type="number" min={0} max={59} step={1} aria-label={label + ', minutes'} value={m} onChange={(e) => put(h, int(e.target.value, 59))} style={{ maxWidth: 90 }} /><span className="mute">min</span>
  </div>;
}

type Shared ={ pols: any[]; plans: { plans: any[]; types: any[] }; cals: any[]; reload: () => void };

export default function Policies() {
  const pols = useLoad(() => api<any[]>('/policies'), []);
  const plans = useLoad(() => api<{ plans: any[]; types: any[] }>('/leave-plans'), []);
  const cals = useLoad(() => api<any[]>('/calendars'), []);
  const [tab, setTab] = useState(() => new URLSearchParams(window.location.search).get('tab') || 'pol');
  const [open, setOpen] = useState('');
  const [ptab, setPtab] = useState('sum');
  const [lp, setLp] = useState('');
  const tabs: [string, string][] = [['pol', 'Policies'], ['cov', 'Coverage'], ['cal', 'Holiday calendars'], ['lplan', 'Leave plans'], ['nom', 'Nominations'], ['exc', 'Exceptions'], ['cp', 'Company policies']];
  const err = pols.error || plans.error || cals.error;
  const sh: Shared | null = pols.data && plans.data && cals.data ? { pols: pols.data, plans: plans.data, cals: cals.data, reload: () => { pols.reload(); plans.reload(); cals.reload(); } } : null;
  return <>
    <div className="tabs">{tabs.map(([k, l]) => <button key={k} className={`tab ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}</button>)}</div>
    {tab === 'cp' ? <CompanyPolicies /> : tab === 'nom' ? <NominationsAdmin /> : tab === 'exc' ? <Exceptions /> : !sh ? <Loading error={err} />
      : tab === 'cov' ? <Coverage sh={sh} />
      : tab === 'cal' ? <Calendars sh={sh} />
      : tab === 'lplan' ? <LeavePlans sh={sh} open={lp} setOpen={setLp} />
      : open && sh.pols.some((p) => p.id === open) ? <PolicyOne key={open} id={open} sh={sh} tab={ptab} setTab={setPtab} onBack={() => setOpen('')} onOpen={(id, t) => { setOpen(id); setPtab(t); }} goPlan={(id) => { setLp(id); setTab('lplan'); }} />
      : <PolicyList sh={sh} onOpen={(id, t = 'sum') => { setOpen(id); setPtab(t); }} />}
  </>;
}

// ---------- Policies ----------

/** Names for mapping refs held as ids (location, cluster, city, state). */
function useRefNames() {
  return useLoad(async () => {
    const out: Record<string, string> = {};
    for (const k of ['location', 'cluster', 'city', 'state']) (await api<any[]>(`/masters/${k}`)).forEach((x) => (out[x.id] = x.name));
    return out;
  }, []);
}

async function duplicate(p: any, onOpen: (id: string, t: string) => void, reload: () => void) {
  try { const r = await api('/policies', { body: { name: `${p.name} (copy)`, copyFrom: p.id } }); toast('Duplicated. Add its mapping.'); reload(); onOpen(r.id, 'sum'); } catch (x: any) { toast(x.message); }
}

function PolicyList({ sh, onOpen }: { sh: Shared; onOpen: (id: string, t?: string) => void }) {
  const { can } = useAuth();
  const canC = can('pol.policies', 2);
  const names = useRefNames();
  const [f, setF] = useState('Active');
  const [nw, setNw] = useState<{ name: string; from: string } | null>(null);
  const list = sh.pols.filter((p) => f === 'All' || p.status === f);
  const create = async () => {
    const name = nw!.name.trim();
    if (!name) { toast('Name the policy'); return; }
    try {
      const r = await api('/policies', { body: { name, copyFrom: nw!.from } });
      if (!nw!.from) { // standard defaults: attach the first active leave plan and holiday calendar, as a new policy needs both
        const def = sh.pols.find((x) => x.status === 'Active'), g = await api(`/policies/${r.id}`);
        const pl = def?.leavePlanId ?? sh.plans.plans.find((x) => x.plan.status === 'Active')?.plan.id ?? null, cal = def?.calendarId ?? sh.cals.find((c) => c.status === 'Active')?.id ?? null;
        await api(`/policies/${r.id}`, { method: 'PUT', body: { name, settings: g.policy.settings, leavePlanId: pl, calendarId: cal, calMap: {} } });
      }
      setNw(null); sh.reload(); onOpen(r.id, 'map'); toast('Created. Map it to the people it covers');
    } catch (x: any) { toast(x.message); }
  };
  return <>
    <div className="row sp" style={{ marginBottom: 14 }}>
      <div className="seg">{['Active', 'Inactive', 'All'].map((x) => <button key={x} className={f === x ? 'on' : ''} onClick={() => setF(x)}>{x}<b>{sh.pols.filter((p) => x === 'All' || p.status === x).length}</b></button>)}</div>
      {canC && <button className="btn primary" onClick={() => setNw({ name: '', from: '' })}>New work policy</button>}
    </div>
    <div className="note" style={{ marginBottom: 14 }}>A policy holds everything about how a group of people works: weekly offs, holidays, hours, attendance, regularization, leave, probation and notice. Map it to a location, cluster, department, designation, grade, employment type, city, state, a department at one place (for example People Ops at the Gurgaon office) or one employee, and it applies to everyone matching, including future joiners.</div>
    <div className="card"><div className="tw"><table><thead><tr><th>Policy</th><th className="num">Employees</th><th>Applies to</th><th>Rules in brief</th><th>Status</th><th></th></tr></thead><tbody>
      {list.map((p) => {
        const ms = p.maps.filter((m: any) => m.on), plan = sh.plans.plans.find((x) => x.plan.id === p.leavePlanId)?.plan;
        return <tr key={p.id}>
          <td><b>{p.name}</b><div className="mute" style={{ fontSize: 12 }}>{p.settings.desc || weekOffText(p.settings)}</div></td>
          <td className="num">{p.employees}</td>
          <td>{ms.slice(0, 3).map((m: any, i: number) => <span key={i}>{i > 0 && ' '}<Pill k="info">{LVL[m.level]}: {refLabel(m.level, m.ref, names.data)}</Pill></span>)}{ms.length > 3 && <span className="mute"> +{ms.length - 3} more</span>}{!ms.length && <span className="mute">Nobody yet</span>}</td>
          <td className="ink2" style={{ fontSize: 12.5 }}>{lines(ruleLines(p.settings, plan))}</td>
          <td><Pill k={p.status === 'Active' ? 'ok' : 'n'}>{p.status}</Pill></td>
          <td style={{ whiteSpace: 'nowrap' }}><button className="btn sm primary" onClick={() => onOpen(p.id)}>Open</button> {canC && <button className="btn sm" onClick={() => duplicate(p, onOpen, sh.reload)}>Duplicate</button>}</td>
        </tr>;
      })}
      {!list.length && <tr><td colSpan={6} className="mute">No policies here.</td></tr>}
    </tbody></table></div></div>
    {nw && <Modal onClose={() => setNw(null)}>
      <div className="row sp"><h2>New work policy</h2><button className="btn sm" onClick={() => setNw(null)}>Close</button></div>
      <div className="form" style={{ marginTop: 12 }}>
        <label className="field"><span>Policy name</span><input type="text" value={nw.name} onChange={(e) => setNw({ ...nw, name: e.target.value })} placeholder="For example: Property operations – 24x7" /></label>
        <label className="field"><span>Start from</span><select value={nw.from} onChange={(e) => setNw({ ...nw, from: e.target.value })}><option value="">Standard defaults</option>{sh.pols.map((p) => <option key={p.id} value={p.id}>Copy of {p.name}</option>)}</select></label>
      </div>
      <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={create}>Create and configure</button><button className="btn" onClick={() => setNw(null)}>Cancel</button></div>
    </Modal>}
  </>;
}

function PolicyOne({ id, sh, tab, setTab, onBack, onOpen, goPlan }: { id: string; sh: Shared; tab: string; setTab: (t: string) => void; onBack: () => void; onOpen: (id: string, t: string) => void; goPlan: (id: string) => void }) {
  const { can } = useAuth();
  const canE = can('pol.policies', 2);
  const { data, error, reload } = useLoad(() => api(`/policies/${id}`), [id]);
  const cities = useLoad(() => api<any[]>('/masters/city?status=Active'), []);
  const grades = useLoad(() => api<any[]>('/masters/grade?status=Active'), []);
  // The open payroll month anchors the off-cap preview and the regularization example (prototype openYm).
  const open = useLoad(() => (can('pay.run') ? api<any>('/payroll').then((x) => x.openYm as string).catch(() => '') : Promise.resolve('')), []);
  const [p, setP] = useState<any>(null);
  const [sim, setSim] = useState(false);
  const [eff, setEff] = useState(today());
  // Unsaved edits belong to the tab they were made on, as in the prototype: switching tab starts from the saved policy.
  useEffect(() => { if (data) setP(structuredClone(data.policy)); }, [data, tab]);
  const sum = sh.pols.find((x) => x.id === id);
  if (!p || !sum) return <Loading error={error} />;
  const s = p.settings, plan = sh.plans.plans.find((x) => x.plan.id === p.leavePlanId)?.plan;
  const set = (path: string, v: any) => { const n = structuredClone(p); setp(n.settings, path, v); setP(n); };
  const body = (x: any, extra: any = {}) => ({ name: x.name, settings: x.settings, leavePlanId: x.leavePlanId || null, calendarId: x.calendarId || null, calMap: x.calMap || {}, ...extra });
  const after = () => { reload(); sh.reload(); };
  const save = async () => {
    if (!String(p.name).trim()) { toast('Name the policy'); return; }
    const n = nChanges(body(data.policy), body(p));
    if (!eff) { toast('Pick the date the change applies from'); return; }
    try { await api(`/policies/${id}`, { method: 'PUT', body: body(p, { effectiveFrom: eff }) }); toast(n ? `Saved, ${n} change${n === 1 ? '' : 's'} recorded` : 'Nothing changed'); after(); } catch (x: any) { toast(x.message); }
  };
  /** Changes that apply at once (switches, blackout periods, leave plan): start from the saved policy. */
  const saveNow = async (fn: (x: any) => void, extra: any = {}, msg = '') => {
    const x = structuredClone(data.policy); fn(x);
    try { await api(`/policies/${id}`, { method: 'PUT', body: body(x, extra) }); if (msg) toast(msg); after(); } catch (e: any) { toast(e.message); }
  };
  const toggle = () => {
    if (p.status === 'Active' && sh.pols.filter((x) => x.status === 'Active').length < 2) { toast('Keep at least one active policy'); return; }
    saveNow(() => {}, { active: p.status !== 'Active' });
  };
  const calName = (cid?: string) => sh.cals.find((c) => c.id === cid)?.name;
  const holSet = (cityId: string) => new Set<string>((sh.cals.find((c) => c.id === (p.calMap?.[cityId] || p.calendarId))?.holidays ?? []).filter((h: any) => h.on_list && h.kind !== 'Optional').map((h: any) => h.date));
  const optAvail = (sh.cals.find((c) => c.id === p.calendarId)?.holidays ?? []).filter((h: any) => h.on_list && h.kind === 'Optional').length;
  const ym = open.data || ymOf(), mos = [ym, addMonths(ym, 1), addMonths(ym, 2)];
  const sections = (t: string) => PS[t].map(([h, fs]) => <div key={h} className="card" style={{ marginBottom: 14 }}><h3>{h}</h3><div className="form">{fs.map((f) => <Fld key={f[0]} f={f} v={getp(s, f[0])} set={(v) => set(f[0], v)} />)}</div></div>);
  const save_ = canE && !['map', 'hist', 'leave', 'lrule'].includes(tab);

  let content: JSX.Element | null = null;
  if (tab === 'sum') {
    const warn: string[] = [], act = sh.pols.filter((x) => x.status === 'Active');
    if (!p.maps.some((m: any) => m.on) && act[0]?.id !== p.id) warn.push('Not mapped to anyone, so nobody is on it');
    if (s.optHol > optAvail) warn.push('The holiday calendar has fewer optional holidays than allowed');
    if ((cities.data ?? []).some((c) => [ym, addMonths(ym, 1)].some((m) => monthOffs(s, holSet(c.id), m).over))) warn.push('Some city and month combinations are over the off cap');
    if (!['Casual leave', 'Privilege leave', 'Sick leave'].some((n) => ltOf(plan, n).on)) warn.push('No leave is credited. This is normal for contract and retainer staff');
    const sl = ltOf(plan, 'Sick leave'), blk = s.blackouts.filter((b: any) => b.on).length;
    content = <>
      <div className="card" style={{ marginBottom: 14 }}><h3>About this policy</h3><div className="form">
        <label className="field"><span>Policy name</span><input type="text" value={p.name} onChange={(e) => setP({ ...p, name: e.target.value })} /></label>
        <label className="field" style={{ gridColumn: 'span 2' }}><span>Description, who it is for</span><input type="text" value={s.desc || ''} onChange={(e) => set('desc', e.target.value)} placeholder="For example: 24x7 front office and housekeeping at managed properties" /></label>
      </div></div>
      {warn.length > 0 && <div className="note bad" style={{ marginBottom: 14 }}>{lines(warn)}</div>}
      <div className="grid g3" style={{ marginBottom: 14 }}>
        <div className="card stat"><div className="l">Employees on this policy</div><div className="n">{sum.employees}</div></div>
        <div className="card stat"><div className="l">Mappings</div><div className="n">{p.maps.filter((m: any) => m.on).length}</div></div>
        <div className="card stat"><div className="l">Last changed</div><div className="n" style={{ fontSize: 16 }}>{data.history.length ? fmtAt(data.history[0].at) : 'Not changed'}</div></div>
      </div>
      <div className="card"><dl className="kv">
        <dt>Weekly off</dt><dd>{weekOffText(s)}{s.wo.consec ? `, at most ${s.wo.consec} days in a row` : ''}</dd>
        <dt>Off cap</dt><dd>{s.offCap.on ? s.offCap.max + ' a month' : 'None'}</dd>
        <dt>Hours</dt><dd>Full day {hmText(s.fullH)}, half day {hmText(s.halfH)}, up to {s.hrs.week} h a week, spread over at most {hmText(s.hrs.spread)} a day</dd>
        <dt>Shifts and rest</dt><dd>{+s.att.minRest ? `At least ${s.att.minRest}h between shifts (${s.att.restAct === 'warn' ? 'warned' : 'blocked'} on the roster)` : 'No rest check'}; {s.rs.lead} days notice before a shift change</dd>
        <dt>Leave plan</dt><dd>{plan?.name ?? 'None'}</dd>
        <dt>Holidays</dt><dd>{calName(p.calendarId) ?? 'None'}, {s.optHol} optional a year</dd>
        <dt>Leave</dt><dd>{clText(plan)}<br />{plText(plan)}{sl.on && <><br />SL {sl.days} a year</>}</dd>
        <dt>Leave rules</dt><dd>{lines(ruleLines(s, plan))}{blk > 0 && <><br />{blk} blackout period(s)</>}</dd>
        <dt>Probation</dt><dd>{s.prob ? s.prob + ' months' : 'None'}; notice {s.notice.prob} days in probation, {s.notice.conf} after{Object.keys(s.notice.grade || {}).length ? ', with grade overrides' : ''}</dd>
        <dt>Discipline</dt><dd>{s.rs.pts} points for each Not OK{+s.rs.leaveBelow ? `; no leave below a score of ${s.rs.leaveBelow}` : ''}</dd>
        <dt>Reimbursements</dt><dd>{s.claims?.on !== false ? 'Employees can claim' : 'Raised by Payroll or HR only'}</dd>
      </dl></div>
    </>;
  }
  if (tab === 'time') {
    const o = s.offCap, ocSel = (k: string, l: string) => <label className="field"><span>{l}</span><select value={String(!!o[k])} onChange={(e) => set('offCap.' + k, e.target.value === 'true')}>{YN.map(([k2, x]) => <option key={k2} value={k2}>{x}</option>)}</select></label>;
    const g = (k: string) => getp(s, k), dev: string[] = [];
    if (g('rs.devStart') !== false) dev.push(`Arriving more than ${g('rs.earlyIn')} minutes before, or more than ${g('rs.devTol')} minutes after, the planned start.`);
    if (+g('rs.outTol') > 0) dev.push(`Leaving more than ${g('rs.outTol')} minutes before the shift ends.`);
    content = <>{legend}{sections('time')}
      <div className="card" style={{ marginBottom: 14 }}><h3>Cap on offs in a month</h3>
        <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Limits weekly offs plus holidays in one month, for example 7 = Sundays + 2 Saturdays + holidays. A holiday on a weekly off counts once. Rosters flag any month above the cap.</p>
        <div className="form">{ocSel('on', 'Apply a cap')}<label className="field"><span>Maximum offs in a month</span><input type="number" min={1} value={o.max} onChange={(e) => set('offCap.max', Math.max(1, +e.target.value || 7))} /></label>
          {ocSel('sun', 'Count Sundays')}{ocSel('sat', 'Count Saturdays')}{ocSel('hol', 'Count holidays')}</div>
        <div className="tw" style={{ marginTop: 12 }}><table><thead><tr><th>City</th>{mos.map((m) => <th key={m}>{ymLabel(m)}</th>)}</tr></thead><tbody>
          {(cities.data ?? []).map((c) => <tr key={c.id}><td><b>{c.name}</b></td>{mos.map((m) => { const r = monthOffs(s, holSet(c.id), m); return <td key={m}>{r.sun} Sun + {r.sat} Sat + {r.hol} hol = <b>{r.total}</b> {o.on && <Pill k={r.over ? 'warn' : 'ok'}>{r.over ? 'Over cap' : 'Within cap'}</Pill>}</td>; })}</tr>)}
        </tbody></table></div></div>
      <div className="card" style={{ marginBottom: 14 }}><h3>In this policy a deviation is</h3>
        {dev.length ? <ul style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 13.5, lineHeight: 1.6 }}>{dev.map((x) => <li key={x}>{x}</li>)}</ul> : <p className="mute" style={{ fontSize: 13 }}>Nothing is marked as a deviation.</p>}
        <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Every deviation goes to the manager to mark OK or Not OK. A check-in up to {g('rs.earlyIn')} minutes early is never a deviation. A check-in and check-out more than {g('att.maxSpan')} hours apart is a missing punch, not a deviation. Hours beyond the shift, work on a weekly off, holiday or leave day, and short rest are not deviations: rest is checked on the roster.</p></div>
      {sections('claims')}
    </>;
  }
  if (tab === 'hol') {
    // Only active calendars can be picked; one already on this policy stays listed, marked inactive.
    const calOpts = sh.cals.filter((c) => c.status === 'Active' || c.id === data.policy.calendarId || Object.values(data.policy.calMap || {}).includes(c.id));
    const calLbl = (c: any) => c.name + (c.status !== 'Active' ? ' (inactive)' : '');
    content = <>{legend}
      <div className="card" style={{ marginBottom: 14 }}><h3>Holiday calendar</h3>
        <div className="form"><label className="field"><span>Default calendar</span><select value={p.calendarId || ''} onChange={(e) => setP({ ...p, calendarId: e.target.value })}>{!p.calendarId && <option value="" />}{calOpts.map((c) => <option key={c.id} value={c.id}>{calLbl(c)}</option>)}</select></label></div>
        <h3 style={{ margin: '14px 0 8px', fontSize: 14 }}>By city (overrides the default)</h3>
        <div className="form">{(cities.data ?? []).map((c) => <label key={c.id} className="field"><span>{c.name}</span>
          <select value={p.calMap?.[c.id] || ''} onChange={(e) => { const m = { ...(p.calMap || {}) }; if (e.target.value) m[c.id] = e.target.value; else delete m[c.id]; setP({ ...p, calMap: m }); }}><option value="">Use default</option>{calOpts.map((x) => <option key={x.id} value={x.id}>{calLbl(x)}</option>)}</select></label>)}</div>
        {s.optHol > optAvail && <div className="note bad" style={{ marginTop: 12 }}>The calendar has {optAvail} optional holidays but {s.optHol} are allowed for each employee.</div>}
      </div>
      {sections('hol')}</>;
  }
  if (tab === 'att') content = <>{legend}{sections('att')}<DevAllowCard s={s} set={set} /></>;
  if (tab === 'reg') {
    const eg = [addMonths(ym, -1), ym].map((m) => { const d = m + '-14', a = addDays(d, s.rr.back), b = s.rr.cutoff ? addDays(lastDayOf(m), s.rr.extra) : '9999-12-31'; return { d, a, b: s.rr.cutoff ? b : '', last: a < b ? a : b }; });
    content = <>{sections('reg')}
      <div className="card" style={{ marginBottom: 14 }}><h3>Example</h3><div className="tw"><table><thead><tr><th>Date</th><th>{s.rr.back}-day limit</th><th>Payroll cycle limit</th><th>Last day to apply</th></tr></thead><tbody>
        {eg.map((x) => <tr key={x.d}><td>{fmtD(x.d)}</td><td>{fmtD(x.a)}</td><td>{x.b ? fmtD(x.b) : 'No cycle limit'}</td><td><b>{fmtD(x.last)}</b></td></tr>)}
      </tbody></table></div><p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>A locked month or a missed window can be opened for one employee under Exceptions.</p></div></>;
  }
  if (tab === 'leave') {
    const opts = sh.plans.plans.map((x) => x.plan).filter((x) => x.status === 'Active' || x.id === p.leavePlanId);
    const yr: Record<string, string> = { cal: 'January to December', fin: 'April to March', anniv: 'Joining anniversary' };
    const on = plan ? sh.plans.types.filter((i) => i.status === 'Active' && plan.types[i.name]?.on) : [];
    content = <div className="card"><h3>Leave plan</h3>
      <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Leave credit and the rules for each leave type come from a leave plan, the same way holidays come from a holiday calendar. Change the plan under Leave plans and every policy using it follows. Blackout periods stay with the policy.</p>
      <div className="form"><label className="field"><span>Leave plan for this policy</span>
        <select value={p.leavePlanId || ''} disabled={!canE} onChange={(e) => saveNow((x) => { x.leavePlanId = e.target.value || null; }, {}, 'Leave plan changed')}>{!p.leavePlanId && <option value="" />}{opts.map((x) => <option key={x.id} value={x.id}>{x.name}{x.status !== 'Active' ? ' (inactive)' : ''}</option>)}</select></label></div>
      {plan && <dl className="kv" style={{ marginTop: 12 }}>
        {on.length ? on.map((i) => { const c = plan.types[i.name]; return <span key={i.name} style={{ display: 'contents' }}><dt>{i.name}</dt><dd>{ltBrief(i.kind, i.paid, c, earnOf(plan, i.name))}, {c.types.join(', ')}{c.applyBy === 'hr' ? ', applied by HR on behalf' : ''}{c.onNotice === 'restrict' ? ', not during resignation' : ''}</dd></span>; })
          : <><dt>Leave</dt><dd>None credited</dd></>}
        <dt>Leave year</dt><dd>{yr[plan.lv.year] || plan.lv.year}{plan.lv.prorate ? ', pro-rated for joiners' : ''}</dd></dl>}
      {plan && <div className="row" style={{ marginTop: 12 }}><button className="btn" onClick={() => goPlan(plan.id)}>Open this plan</button></div>}
    </div>;
  }
  if (tab === 'lrule') content = <Blackouts list={s.blackouts} edit={canE} save={(fn, msg) => saveNow((x) => fn(x.settings.blackouts), {}, msg)} />;
  if (tab === 'exit') content = <>{legend}{sections('exit')}
    <div className="card" style={{ marginBottom: 14 }}><h3>Probation and notice by grade</h3><p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Blank means the policy default above. A grade's probation decides when its people are confirmed, the notice they serve and the leave they can use in probation. 0 months means no probation.</p>
      <div className="tw"><table><thead><tr><th>Grade</th><th>Probation (months)</th><th>Notice in probation (days)</th><th>Notice after confirmation (days)</th><th className="num">Employees</th></tr></thead><tbody>
        {(grades.data ?? []).map((g) => <tr key={g.id}><td><b>{g.name}</b></td>
          {['probM', 'prob', 'conf'].map((k) => <td key={k}><input type="number" min={0} max={k === 'probM' ? 24 : 365} style={{ width: 110 }} aria-label={`${g.name} ${k === 'probM' ? 'probation months' : k === 'prob' ? 'notice in probation' : 'notice after confirmation'}`} value={s.notice.grade?.[g.name]?.[k] ?? ''} onChange={(e) => {
            const n = structuredClone(p); n.settings.notice.grade ??= {}; const row = { ...(n.settings.notice.grade[g.name] || {}) };
            if (e.target.value === '') delete row[k]; else row[k] = Math.max(0, +e.target.value);
            if (Object.keys(row).length) n.settings.notice.grade[g.name] = row; else delete n.settings.notice.grade[g.name]; setP(n);
          }} /></td>)}<td className="num">{data.gradeCounts?.[g.name] ?? 0}</td></tr>)}
      </tbody></table></div></div></>;
  if (tab === 'map') content = <Mappings p={p} n={sum.employees} sh={sh} edit={canE} onSaved={after} />;
  if (tab === 'hist') content = <History hist={data.history} sh={sh} />;

  return <>
    <div className="row sp" style={{ marginBottom: 12 }}>
      <div className="row"><button className="btn sm" onClick={onBack}>All policies</button><h2 style={{ margin: 0, fontSize: 20 }}>{data.policy.name}</h2><Pill k={p.status === 'Active' ? 'ok' : 'n'}>{p.status}</Pill><span className="mute">{sum.employees} employees</span></div>
      <div className="row"><button className="btn sm" onClick={() => setSim(true)}>Leave simulator</button>{canE && <button className="btn sm" onClick={() => duplicate(data.policy, onOpen, sh.reload)}>Duplicate</button>}{canE && <Switch on={p.status === 'Active'} onChange={toggle} />}</div>
    </div>
    <div className="tabs" style={{ marginBottom: 14 }}>{PTAB.map(([k, l]) => <button key={k} className={`tab ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}</button>)}</div>
    <fieldset disabled={!canE} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>{content}</fieldset>
    {save_ && <div className="row" style={{ marginTop: 16, position: 'sticky', bottom: 0, background: 'var(--bg,#f5f6f8)', padding: '10px 0' }}>
      <label className="field" style={{ margin: 0 }}><span>Applies from</span><input type="date" value={eff} onChange={(e) => setEff(e.target.value)} /></label>
      <button className="btn primary" onClick={save} style={{ alignSelf: 'flex-end' }}>Save changes</button></div>}
    {sim && <Simulator name={data.policy.name} s={s} plan={plan} onClose={() => setSim(false)} />}
  </>;
}

/** Leave simulator (prototype simModal / leaveSim): monthly credit of casual and privilege leave from a joining date.
 *  A type earned by working days counts paid days day by day from the paid days in a month typed here, spread evenly
 *  over each month (the month's length: nothing is loss of pay), from the accrual start (the end of probation, or
 *  joining when back-dated at confirmation, then credited when probation ends). The day after each whole block of the
 *  rule's paid days, its days earned are credited; the rest counts toward the next block, the count starts again with
 *  each leave year, up to Days a year when set. */
function Simulator({ name, s, plan, onClose }: { name: string; s: any; plan: any; onClose: () => void }) {
  const [doj, setDoj] = useState(today());
  const [wd, setWd] = useState('');
  const cl = ltOf(plan, 'Casual leave'), pl = ltOf(plan, 'Privilege leave');
  const ecl = cl.on ? earnOf(plan, 'Casual leave') : null, epl = pl.on ? earnOf(plan, 'Privilege leave') : null;
  const d0 = new Date(doj + 'T00:00:00'), pe = new Date(d0); pe.setMonth(pe.getMonth() + (+s.prob || 0));
  let a = 0, b = 0, caught = false;
  const r2 = (x: number) => Math.round(x * 100) / 100;
  const yr = plan?.lv?.year, ysM = yr === 'fin' ? 3 : yr === 'anniv' ? d0.getMonth() : 0, ysD = yr === 'anniv' ? d0.getDate() : 1; // the leave year's first day
  /** Each credit of a type earned by working days in the 13 months shown: the date it comes and the days. */
  const earnRun = (c: any, e: Earn | null) => {
    const out: { on: Date; n: number }[] = [];
    if (!e || !(e.every > 0) || wd === '' || isNaN(+d0)) return out;
    const unit = Math.round(e.every * 2), start = c.from === 'prob' && !c.catchup ? pe : d0, end = new Date(d0.getFullYear(), d0.getMonth() + 13, 0);
    let cum = 0, got = 0;
    for (const d = new Date(d0); d <= end; d.setDate(d.getDate() + 1)) {
      if (+d > +d0 && d.getMonth() === ysM && d.getDate() === ysD) { cum = 0; got = 0; } // a new leave year: the count starts again
      if (d < start) continue;
      const dim = new Date(d.getFullYear(), d.getMonth() + 1, 0).getDate();
      cum += Math.min(+wd, dim) / dim;
      let now = Math.floor(cum * 2 / unit + 1e-9) * e.per;
      if (+c.days > 0) now = Math.min(now, +c.days);
      if (now > got) {
        const on = new Date(d); on.setDate(on.getDate() + 1); // credited the day after the Nth paid day
        out.push({ on: c.from === 'prob' && c.catchup && on < pe ? new Date(pe) : on, n: r2(now - got) });
        got = now;
      }
    }
    return out;
  };
  const runCl = earnRun(cl, ecl), runPl = earnRun(pl, epl);
  const inMonth = (run: { on: Date; n: number }[], m: Date) => run.filter((x) => x.on.getFullYear() === m.getFullYear() && x.on.getMonth() === m.getMonth());
  const dayM = (d: Date) => d.toLocaleDateString('en-IN', { day: 'numeric', month: 'short' });
  const rows = isNaN(+d0) ? [] : Array.from({ length: 13 }, (_, i) => {
    const m = new Date(d0.getFullYear(), d0.getMonth() + i, 1), end = new Date(m.getFullYear(), m.getMonth() + 1, 0);
    const xc = inMonth(runCl, m), xp = inMonth(runPl, m);
    let c = 0, p = 0;
    if (cl.on) { if (ecl) c = xc.reduce((t, x) => t + x.n, 0); else if (cl.credit === 'monthly') c = cl.days / 12; else if (i === 0) c = cl.days * (12 - d0.getMonth()) / 12; }
    if (pl.on) { if (epl) p = xp.reduce((t, x) => t + x.n, 0); else if (pl.from === 'doj') p = pl.days / 12; else if (end >= pe) { p = pl.days / 12; if (!caught) { caught = true; if (pl.catchup) p += pl.days / 12 * i; } } }
    a += c; b += p;
    const on = [...(xc.length ? [`CL ${[...new Set(xc.map((x) => dayM(x.on)))].join(', ')}`] : []), ...(xp.length ? [`PL ${[...new Set(xp.map((x) => dayM(x.on)))].join(', ')}`] : [])].join(' · ');
    return { m: m.toLocaleDateString('en-IN', { month: 'short', year: 'numeric' }), c: r2(c), p: r2(p), cl: r2(a), pl: r2(b), on };
  });
  const anyEarn = !!(ecl || epl);
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Leave simulator: {name}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="row" style={{ gap: 12, margin: '10px 0' }}><label className="field" style={{ maxWidth: 220, margin: 0 }}><span>Date of joining</span><input type="date" value={doj} onChange={(e) => setDoj(e.target.value)} /></label>
      {anyEarn && <label className="field" style={{ maxWidth: 220, margin: 0 }}><span>Paid days in a month</span><input type="number" min={0} max={31} step={0.5} placeholder="Paid days in a month" value={wd} onChange={(e) => setWd(e.target.value)} /></label>}</div>
    <p className="ink2">Joining {fmtD(doj)}. Probation {s.prob ? 'ends ' + fmtD(iso(pe)) : 'not applicable'}. {ecl ? clText(plan) + '. ' : ''}{plText(plan)}.</p>
    {anyEarn && wd === '' && <p className="mute" style={{ fontSize: 12.5 }}>Type the paid days in a month to see what the leave earned by working days gives.</p>}
    <div className="tw"><table><thead><tr><th>Month</th><th className="num">CL credit</th><th className="num">CL balance</th><th className="num">PL credit</th><th className="num">PL balance</th>{anyEarn && <th>Credited on</th>}</tr></thead><tbody>
      {rows.map((r) => <tr key={r.m}><td>{r.m}</td><td className="num">{r.c}</td><td className="num"><b>{r.cl}</b></td><td className="num">{r.p}</td><td className="num"><b>{r.pl}</b></td>{anyEarn && <td className="mute" style={{ fontSize: 12.5 }}>{r.on}</td>}</tr>)}
    </tbody></table></div>
    <p className="mute" style={{ fontSize: 12 }}>Balances before any leave taken or carry forward.{anyEarn ? ` Earned by working days: ${PAID_DAYS}, here the paid days typed above spread evenly over each month (type the month's length when nothing is loss of pay). Credited the day after the Nth paid day; the count starts again with each leave year. Real balances use each person's attendance, and only active employees earn.` : ''}</p>
  </Modal>;
}

function Blackouts({ list, edit, save }: { list: any[]; edit: boolean; save: (fn: (l: any[]) => void, msg?: string) => void }) {
  const [n, setN] = useState({ name: '', from: '', to: '' });
  const add = () => {
    if (!n.name.trim() || !n.from || !n.to || n.to < n.from) { toast('Enter a name and a valid date range'); return; }
    save((l) => l.push({ name: n.name.trim(), from: n.from, to: n.to, on: true }), 'Added'); setN({ name: '', from: '', to: '' });
  };
  return <div className="card"><h3>Blackout periods</h3>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Peak seasons, events and festivals when planned leave (casual, privilege, comp-off, loss of pay) cannot be taken. Sick and maternity leave are never blocked. Switch a period off to retire it.</p>
    <div className="tw"><table><thead><tr><th>Period</th><th>From</th><th>To</th><th>Active</th></tr></thead><tbody>
      {list.map((b, i) => <tr key={i}><td>{b.name}</td><td>{fmtD(b.from)}</td><td>{fmtD(b.to)}</td><td><Switch on={b.on} disabled={!edit} onChange={() => save((l) => { l[i].on = !l[i].on; })} /></td></tr>)}
      {!list.length && <tr><td colSpan={4} className="mute">None yet.</td></tr>}
    </tbody></table></div>
    {edit && <div className="row" style={{ marginTop: 12 }}><input type="text" value={n.name} onChange={(e) => setN({ ...n, name: e.target.value })} placeholder="Name, e.g. New Year peak" style={{ maxWidth: 240 }} />
      <input type="date" value={n.from} onChange={(e) => setN({ ...n, from: e.target.value })} style={{ maxWidth: 170 }} /><input type="date" value={n.to} onChange={(e) => setN({ ...n, to: e.target.value })} style={{ maxWidth: 170 }} />
      <button className="btn" onClick={add}>Add period</button></div>}
  </div>;
}

/** The master each mapping level picks its value from (a place by id; department, designation, grade and type by name). */
const MAP_SRC: Record<string, string> = { location: 'location', cluster: 'cluster', department: 'department', designation: 'designation', grade: 'grade', type: 'employment_type', city: 'city', state: 'state' };

/** The value of a mapping at one level: one select, or a department and a place for Department + place. A blank value
 * takes the first choice; a current value no longer active stays listed. Key it by level so a new level starts fresh. */
function MapValuePick({ level, value, set, names }: { level: string; value: string; set: (v: string) => void; names?: Record<string, string> | null }) {
  const place = COMBO[level];
  const src = MAP_SRC[place || level];
  const opts = useLoad(() => (src ? api<any[]>(`/masters/${src}?status=Active`) : Promise.resolve([] as any[])), [src]);
  const depts = useLoad(() => (place ? api<any[]>('/masters/department?status=Active') : Promise.resolve([] as any[])), [place]);
  const byName = !place && ['department', 'designation', 'grade', 'type'].includes(level);
  const ov = (opts.data ?? []).map((x) => [byName ? x.name : x.id, x.name] as [string, string]);
  const cut = place ? value.lastIndexOf('|') : -1;
  const dep = place ? (cut > 0 ? value.slice(0, cut) : depts.data?.[0]?.name ?? '') : '';
  const pv = place ? (cut > 0 ? value.slice(cut + 1) : ov[0]?.[0] ?? '') : value || ov[0]?.[0] || '';
  const full = place ? (dep && pv ? dep + '|' + pv : '') : pv;
  useEffect(() => { if (full && full !== value) set(full); }, [full]); // eslint-disable-line react-hooks/exhaustive-deps
  if (level === 'employee') return <b>{refLabel(level, value, names)}</b>;
  const pOpts = pv && !ov.some(([k]) => k === pv) ? [[pv, names?.[pv] ?? pv] as [string, string], ...ov] : ov;
  const dOpts = (depts.data ?? []).map((x) => x.name as string);
  return <>
    {place && <select aria-label="Department" value={dep} onChange={(e) => set(e.target.value + '|' + pv)}>{(dep && !dOpts.includes(dep) ? [dep, ...dOpts] : dOpts).map((x) => <option key={x} value={x}>{x}</option>)}</select>}
    {place && <span className="mute">at</span>}
    <select aria-label={place ? LVL[place] : 'Applies to'} value={pv} onChange={(e) => set(place ? dep + '|' + e.target.value : e.target.value)}>{pOpts.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
  </>;
}

/** Applies to (prototype mapBlock). Mappings are switched off, never deleted; a mapping is on one policy at a time. */
function Mappings({ p, n, sh, edit, onSaved }: { p: any; n: number; sh: Shared; edit: boolean; onSaved: () => void }) {
  const { can } = useAuth();
  const [bulk, setBulk] = useState(false); // bulk upload in a dialog here, not on Bulk update (owner, 24 Nov 2026)
  const names = useRefNames();
  const [lvl, setLvl] = useState('dept_location'); // Department + Location first (owner, 24 Nov 2026)
  const [cur, setCur] = useState('');
  const [ed, setEd] = useState<any>(null); // the mapping being edited
  // Policy conflict check: every save is previewed first; people who already get another policy through another
  // mapping need HR's choice (Replace, Keep, Pick people or Cancel), which the server stores as exceptions. A mapping
  // another policy already has is refused ("Already applied to …").
  const [conf, setConf] = useState<{ maps: any[]; pv: any; done: string } | null>(null);
  const body = (maps: any[]) => maps.map((m) => ({ level: m.level, ref: m.ref, on: m.on }));
  const put = async (id: string, maps: any[], done: string) => {
    const pv = await api<any>(`/policies/${id}/maps/preview`, { body: { maps: body(maps) } });
    if (pv.conflicts?.length) { setConf({ maps, pv, done }); return false; }
    await api(`/policies/${id}/maps`, { method: 'PUT', body: { maps: body(maps) } });
    return true;
  };
  const resolve = async (resolution: { mode: string; move?: string[] }) => {
    if (!conf) return;
    try {
      await api(`/policies/${p.id}/maps`, { method: 'PUT', body: { maps: body(conf.maps), resolution } });
      toast(conf.done); setConf(null); setCur(''); onSaved();
    } catch (x: any) { toast(x.message); }
  };
  const tog = async (i: number) => { try { if (await put(p.id, p.maps.map((m: any, j: number) => (j === i ? { ...m, on: !m.on } : m)), 'Saved')) onSaved(); } catch (x: any) { toast(x.message); } };
  const add = async () => {
    if (!cur) { toast('Pick what to map'); return; }
    if (p.maps.some((m: any) => m.on && m.level === lvl && m.ref === cur)) { toast('Already applied to this policy'); return; }
    try {
      if (await put(p.id, p.maps.some((m: any) => m.level === lvl && m.ref === cur) ? p.maps.map((m: any) => (m.level === lvl && m.ref === cur ? { ...m, on: true } : m)) : [...p.maps, { level: lvl, ref: cur, on: true }], 'Mapped')) {
        setCur(''); toast('Mapped'); onSaved();
      }
    } catch (x: any) { toast(x.message); }
  };
  const levels = [...Object.entries(LVL).filter(([k]) => k === 'dept_location'), ...Object.entries(LVL).filter(([k]) => k !== 'employee' && k !== 'dept_location' && !COMBO[k]),
    ...Object.entries(LVL).filter(([k]) => COMBO[k] && k !== 'dept_location')];
  return <>
    <div className="note" style={{ marginBottom: 14 }}>{n} employees are on this policy. When a person matches more than one policy: {ORDER_TEXT}. The first level that matches decides, so a person mapped by name always gets that policy, and a department at a place (Department + Location and so on) comes before the place or the department alone. A new employee picks up a policy automatically from their location, department, designation, grade, type or city. For thousands of changes at once, use Bulk update with the Work Policy column.</div>
    <div className="card">
      {edit && can('bulk.update') && <div className="row sp" style={{ marginBottom: 10 }}><span className="mute" style={{ fontSize: 12.5 }}>Many Department + Location pairs at once:</span>
        <button className="btn sm" onClick={() => setBulk(true)}>Bulk upload</button></div>}
      {bulk && <BulkModal tpl="polmap" title="Bulk upload: work policy mapping" onClose={() => setBulk(false)} onApplied={onSaved} />}
      <div className="tw"><table><thead><tr><th>Level</th><th>Applies to</th><th>Employees</th><th>Active</th>{edit && <th></th>}</tr></thead><tbody>
      {p.maps.map((m: any, i: number) => <tr key={i}><td>{LVL[m.level]}</td><td><b>{refLabel(m.level, m.ref, names.data)}</b>{m.exception && <> <Pill k="warn">Exception</Pill><div className="mute" style={{ fontSize: 12 }}>{m.note}</div></>}</td><td>{m.on ? m.employees ?? 0 : '—'}</td><td><Switch on={m.on} disabled={!edit} onChange={() => tog(i)} /></td>
        {edit && <td>{m.on && m.id ? <button className="btn sm" onClick={() => setEd(m)}>Edit</button> : null}</td>}</tr>)}
      {!p.maps.length && <tr><td colSpan={edit ? 5 : 4} className="mute">Not mapped to anyone yet.</td></tr>}
    </tbody></table></div>
      {edit && <><h3 style={{ margin: '16px 0 8px', fontSize: 14 }}>Add mapping</h3>
        <div className="row"><select aria-label="Level" value={lvl} onChange={(e) => { setLvl(e.target.value); setCur(''); }}>{levels.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
          <MapValuePick key={lvl} level={lvl} value={cur} set={setCur} names={names.data} />
          <button className="btn primary" onClick={add}>Add</button></div>
        <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>A mapping is applied to one policy at a time: one that another policy already has is refused ("Already applied to …"); switch it off there, or Edit it there to move it here. Mappings are switched off, never deleted. If the people it covers already get another policy through a different level, you are asked what to do before anything is saved.
          {COMBO[lvl] && ` ${LVL[lvl]} covers only the people of that department at that ${LVL[COMBO[lvl]].toLowerCase()}, and comes before the ${LVL[COMBO[lvl]].toLowerCase()} or the department mapped alone.`}</p></>}
    </div>
    {conf && <ConflictDialog policy={p.name} pv={conf.pv} onChoose={resolve} onClose={() => setConf(null)} />}
    {ed && <MapEdit m={ed} p={p} sh={sh} names={names.data} onClose={() => setEd(null)} onSaved={() => { setEd(null); onSaved(); }} />}
  </>;
}

/** Edit one mapping: its value and/or the policy it is on. Previewed first: a mapping another policy has is refused,
 * people it moves who get another policy through a different level need HR's choice, and a move to another policy
 * is confirmed. */
function MapEdit({ m, p, sh, names, onClose, onSaved }: { m: any; p: any; sh: Shared; names?: Record<string, string> | null; onClose: () => void; onSaved: () => void }) {
  const [ref, setRef] = useState<string>(m.ref);
  const [to, setTo] = useState<string>(p.id);
  const [pv, setPv] = useState<any>(null); // the preview of a move, waiting for Confirm
  const [conf, setConf] = useState<any>(null);
  const url = `/policies/${p.id}/maps/${m.id}`;
  useEffect(() => setPv(null), [ref]); // a changed value is previewed again
  const save = async (extra: any = {}) => {
    try { await api(url, { method: 'PUT', body: { ref, policyId: to, ...extra } }); toast(to !== p.id ? `Moved to ${sh.pols.find((x) => x.id === to)?.name}` : 'Mapping saved'); onSaved(); } catch (x: any) { toast(x.message); }
  };
  const go = async () => {
    try {
      const r = await api<any>(`${url}/preview`, { body: { ref, policyId: to } });
      if (r.unchanged) { toast('Nothing changed'); return; }
      if (r.conflicts?.length) { setConf(r); return; }
      if (r.move) { setPv(r); return; }
      await save();
    } catch (x: any) { toast(x.message); }
  };
  if (conf) return <ConflictDialog policy={conf.to} pv={conf} onChoose={(resolution) => save({ resolution })} onClose={() => setConf(null)} />;
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Edit mapping</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 12px' }}>{LVL[m.level]}: {refLabel(m.level, m.ref, names)} on {p.name}. The old mapping is switched off and kept in the history; the change is audited on both policies.</p>
    <div className="form">
      <div className="field"><span>{LVL[m.level]}</span><div className="row" style={{ gap: 6 }}><MapValuePick level={m.level} value={ref} set={setRef} names={names} /></div></div>
      <label className="field"><span>Work policy</span><select value={to} onChange={(e) => { setTo(e.target.value); setPv(null); }}>{sh.pols.map((x) => <option key={x.id} value={x.id}>{x.name}{x.status !== 'Active' ? ' (inactive)' : ''}</option>)}</select></label>
    </div>
    {pv && <div className="note warn" style={{ marginTop: 12 }}>Move {refLabel(m.level, ref, names)} from {pv.from} to {pv.to}?{pv.moves?.length ? ' ' + pv.moves.map((g: any) => g.text).join('. ') + '.' : ''}{pv.newly ? ` ${pv.newly} other ${pv.newly === 1 ? 'person moves' : 'people move'} to ${pv.to}.` : ''}</div>}
    <div className="row" style={{ marginTop: 16, justifyContent: 'flex-end' }}>
      <button className="btn" onClick={onClose}>Cancel</button>
      {pv ? <button className="btn primary" onClick={() => save()}>Confirm move</button> : <button className="btn primary" onClick={go}>Save</button>}
    </div>
  </Modal>;
}

/** The policy conflict check: who the new mapping covers that already gets another policy, and HR's choice. */
function ConflictDialog({ policy, pv, onChoose, onClose }: { policy: string; pv: any; onChoose: (r: { mode: string; move?: string[] }) => void; onClose: () => void }) {
  const [pick, setPick] = useState(false);
  const [move, setMove] = useState<Set<string>>(new Set());
  const flip = (id: string, on: boolean) => { const s = new Set(move); if (on) s.add(id); else s.delete(id); setMove(s); };
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>Other policies already apply here</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="note warn" style={{ marginTop: 12 }}>{pv.conflicts.map((g: any) => <div key={g.policyId + g.level + g.ref}>{g.text}.</div>)}</div>
    {pv.moves?.length > 0 && <div className="note" style={{ marginTop: 8 }}>{pv.moves.map((g: any) => <div key={g.policyId + g.ref}>{g.text}.</div>)}</div>}
    {pv.newly > 0 && <p className="mute" style={{ fontSize: 12.5 }}>{pv.newly} other {pv.newly === 1 ? 'person moves' : 'people move'} to {policy} with no conflict.</p>}
    <p className="ink2" style={{ fontSize: 13 }}>Choose what happens to the {pv.people} {pv.people === 1 ? 'person' : 'people'} above. The choice is saved as an exception for each person, so the result never depends on which mapping ranks higher. It applies from today ({fmtD(pv.applyFrom)}).</p>
    {pick && <div style={{ maxHeight: 320, overflow: 'auto', border: '1px solid var(--line)', borderRadius: 10, padding: 10 }}>
      {pv.conflicts.map((g: any) => <div key={g.policyId + g.level + g.ref} style={{ marginBottom: 10 }}>
        <div className="row sp"><b style={{ fontSize: 13 }}>Now on {g.policy} (through {g.through})</b>
          <button className="btn sm" onClick={() => { const s = new Set(move); const all = g.people.every((x: any) => s.has(x.id)); g.people.forEach((x: any) => (all ? s.delete(x.id) : s.add(x.id))); setMove(s); }}>Tick all</button></div>
        <div className="chkgrp" style={{ marginTop: 6 }}>{g.people.map((x: any) => <label key={x.id} className="chkl"><input type="checkbox" checked={move.has(x.id)} onChange={(e) => flip(x.id, e.target.checked)} />
          {x.name} <span className="mono mute">{x.id}</span> <span className="mute" style={{ fontSize: 11.5 }}>{x.department} · {x.location}</span></label>)}</div>
      </div>)}
      <p className="mute" style={{ fontSize: 12, margin: 0 }}>Ticked people move to {policy}; the others stay where they are.</p>
    </div>}
    <div className="row" style={{ marginTop: 16, justifyContent: 'flex-end', flexWrap: 'wrap', gap: 8 }}>
      <button className="btn" onClick={onClose}>Cancel</button>
      {!pick ? <>
        <button className="btn" onClick={() => setPick(true)}>Pick people</button>
        <button className="btn" onClick={() => onChoose({ mode: 'keep' })}>Keep their current policy</button>
        <button className="btn primary" onClick={() => onChoose({ mode: 'replace' })}>Replace: move them to {policy}</button>
      </> : <button className="btn primary" onClick={() => onChoose({ mode: 'pick', move: [...move] })}>Save: move {move.size}, keep {pv.people - move.size}</button>}
    </div>
  </Modal>;
}

/** History (prototype histBlock). Field paths from the server read as the setting's label. */
function History({ hist, sh }: { hist: any[]; sh: Shared }) {
  const names = useRefNames();
  const label = (f: string) => {
    const k = f.replace(/^settings\./, '');
    for (const secs of Object.values(PS)) for (const [, fs] of secs) for (const x of fs) if (x[0] === k) return x[1];
    if (k === 'name') return 'Policy name';
    if (k === 'desc') return 'Description';
    if (k === 'leavePlanId') return 'Leave plan';
    if (k === 'calendarId') return 'Holiday calendar';
    if (k === 'status') return 'Status';
    if (k === 'blackouts') return 'Blackout periods';
    if (k.startsWith('calMap.')) return 'Holiday calendar for ' + (names.data?.[k.slice(7)] ?? k.slice(7));
    if (k.startsWith('offCap.')) return 'Off cap ' + k.slice(7);
    if (k.startsWith('dev.')) return devLabel(k);
    const ng = /^notice\.grade\.(.+)\.(probM|prob|conf)$/.exec(k);
    if (ng) return `${({ probM: 'Probation (months)', prob: 'Notice in probation', conf: 'Notice after confirmation' } as Record<string, string>)[ng[2]]}, ${ng[1]}`;
    if (k.startsWith('notice.grade.')) return 'Probation and notice ' + k.slice(13);
    const m = /^applies to (\w+)\|(.+)$/.exec(k);
    if (m) return `Applies to ${LVL[m[1]] ?? m[1]}: ${refLabel(m[1], m[2], names.data)}`;
    return k;
  };
  const val = (f: string, v: any) => {
    if (v == null) return '—';
    if (HM_KEYS.includes(f.replace(/^settings\./, '')) && typeof v === 'number') return hmText(v);
    if (/^(settings\.)?dev\.\w+\.act$/.test(f)) return devActText(v);
    if (f === 'leavePlanId') return sh.plans.plans.find((x) => x.plan.id === v)?.plan.name ?? String(v);
    if (f === 'calendarId' || f.startsWith('calMap.')) return sh.cals.find((c) => c.id === v)?.name ?? String(v);
    if (Array.isArray(v)) return v.map((b) => (b && typeof b === 'object' ? (b.from ? `${b.name} ${fmtD(b.from)} to ${fmtD(b.to)}${b.on ? '' : ' (off)'}` : JSON.stringify(b)) : String(b))).join(', ') || 'none';
    return typeof v === 'object' ? JSON.stringify(v) : String(v);
  };
  const onOff = (v: any) => (v === true || v === 'on' ? 'On' : 'Off');
  /** One server change as the prototype's history lines: a blackout period added or switched, a status or mapping switched. */
  const lines = (c: any): [string, string, string][] => {
    const k = c.field.replace(/^settings\./, '');
    if (c.field.startsWith('settings.') && REMOVED.test(k)) return [];
    if (k === 'status') return [[label(c.field), c.from ?? (c.to === 'Inactive' ? 'Active' : 'Inactive'), String(c.to)]];
    if (k.startsWith('applies to ')) return [[label(c.field), c.from != null ? onOff(c.from) : onOff(c.to) === 'On' ? 'Off' : 'On', onOff(c.to)]];
    if (k === 'mapping moved') return [[`Mapping moved${c.note ? ' (' + c.note + ')' : ''}`, String(c.from), String(c.to)]];
    if (k === 'blackouts' && Array.isArray(c.to)) {
      const a: any[] = Array.isArray(c.from) ? c.from : [], out: [string, string, string][] = [];
      c.to.forEach((x: any, i: number) => {
        const y = a[i];
        if (!y) out.push(['Blackout period added', x.name, `${fmtD(x.from)} to ${fmtD(x.to)}`]);
        else if (!!y.on !== !!x.on) out.push(['Blackout ' + x.name, onOff(y.on), onOff(x.on)]);
        else if (y.from !== x.from || y.to !== x.to || y.name !== x.name) out.push(['Blackout ' + x.name, `${fmtD(y.from)} to ${fmtD(y.to)}`, `${fmtD(x.from)} to ${fmtD(x.to)}`]);
      });
      if (out.length) return out;
    }
    return [[label(c.field), val(c.field, c.from), val(c.field, c.to)]];
  };
  return <div className="card"><p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Every saved change is recorded with who made it and when it applies from. Earlier requests and approved leave are not rewritten.</p>
    {hist.length ? <ul className="log">{hist.map((h, i) => { const ch = (h.changes as any[]).flatMap(lines); if (!ch.length) return null; return <li key={i}><b>{fmtAt(h.at)}</b> by {h.by_name}, applies from {fmtD(h.effective_from || iso(new Date(h.at)))}
      <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>{ch.slice(0, 12).map((c, j) => <span key={j}>{j > 0 && <br />}{c[0]}: {c[1]} → {c[2]}</span>)}{ch.length > 12 && <><br />and {ch.length - 12} more</>}</div></li>; })}</ul>
      : <p className="mute" style={{ margin: 0 }}>No changes recorded yet.</p>}
  </div>;
}

// ---------- Coverage ----------

function Coverage({ sh }: { sh: Shared }) {
  const { data, error } = useLoad(() => api<{ rows: any[]; total: number; matrix?: Record<string, Record<string, number>>; defaults?: any[]; defaultTotal?: number }>('/policies/coverage'), []);
  if (!data) return <Loading error={error} />;
  // The full matrix and every default row come from the server; the first rows are the fallback.
  const cnt: Record<string, Record<string, number>> = data.matrix ?? {};
  if (!data.matrix) data.rows.forEach((r) => { (cnt[r.policyId] ??= {})[r.via] = (cnt[r.policyId]?.[r.via] || 0) + 1; });
  // Department + place columns only when someone is on a policy through one.
  const vias = [...Object.entries(LVL).filter(([k, v]) => !COMBO[k] || Object.values(cnt).some((x) => x[v])).map(([, v]) => v), 'Default'];
  const def = data.defaults ?? data.rows.filter((r) => r.via === 'Default'), defN = data.defaultTotal ?? def.length;
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Shows which policy each of the {data.total.toLocaleString('en-IN')} employees ends up on, and through which mapping. Anyone on “Default” matches no mapping and falls back to the first active policy. Fix those by mapping their location, department or type.</div>
    <div className="card"><div className="tw"><table><thead><tr><th>Policy</th>{vias.map((v) => <th key={v} className="num">{v}</th>)}<th className="num">Total</th></tr></thead><tbody>
      {sh.pols.map((p) => <tr key={p.id}><td><b>{p.name}</b> {p.status !== 'Active' && <Pill>Inactive</Pill>}</td>{vias.map((v) => <td key={v} className="num">{cnt[p.id]?.[v] || '·'}</td>)}<td className="num"><b>{Object.values(cnt[p.id] || {}).reduce((a, b) => a + b, 0)}</b></td></tr>)}
    </tbody></table></div>
      {!data.matrix && data.rows.length < data.total && <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Counted from the first {data.rows.length} of {data.total} employees.</p>}</div>
    <Overlaps />
    <div className="card" style={{ marginTop: 14 }}><h3>Employees on the default policy ({defN})</h3>
      {def.length ? <><div className="tw"><table><thead><tr><th>Employee</th><th>Type</th><th>Department</th><th>Grade</th><th>City</th></tr></thead><tbody>
        {def.slice(0, 25).map((e) => <tr key={e.id}><td>{e.name} <span className="mono mute">{e.id}</span></td><td>{e.type}</td><td>{e.department}</td><td>{e.grade ?? '—'}</td><td>{e.city ?? '—'}</td></tr>)}
      </tbody></table></div>{defN > 25 && <p className="mute" style={{ fontSize: 12 }}>Showing 25 of {defN}.</p>}</>
        : <p className="mute" style={{ margin: 0 }}>Everyone is covered by a mapping.</p>}</div>
  </>;
}

/** Overlap report: everyone matched by mappings of two or more policies, and which one wins. */
function Overlaps() {
  const { me } = useAuth();
  const { data, error } = useLoad(() => api<{ rows: any[]; total: number }>('/policies/overlaps'), []);
  if (!data) return error ? <div className="note bad" style={{ marginTop: 14 }}>{error}</div> : null;
  return <div className="card" style={{ marginTop: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Overlaps ({data.total})</h3>
      {me?.ctx.canDownload && data.total > 0 && <button className="btn sm" onClick={() => download('/policies/overlaps.xlsx', 'policy-overlaps.xlsx').catch((e) => toast(e.message))}>Download</button>}</div>
    <p className="mute" style={{ fontSize: 12.5, margin: '4px 0 10px' }}>People matched by switched-on mappings of two or more different policies. Today the first level in the order decides; clean these up before go-live by switching a mapping off or mapping the person by name.</p>
    {data.rows.length ? <div className="tw"><table><thead><tr><th>Employee</th><th>Gets</th><th>Also matched by</th></tr></thead><tbody>
      {data.rows.slice(0, 50).map((r) => <tr key={r.id}><td>{r.name} <span className="mono mute">{r.id}</span><div className="mute" style={{ fontSize: 12 }}>{r.department} · {r.location}</div></td>
        <td><b>{r.policy}</b><div className="mute" style={{ fontSize: 12 }}>through {r.through}</div></td>
        <td style={{ fontSize: 12.5 }}>{r.matches.slice(1).map((m: any, i: number) => <div key={i}>{m.policy} <span className="mute">through {m.through}</span></div>)}</td></tr>)}
    </tbody></table></div> : <p className="mute" style={{ margin: 0 }}>No one is matched by more than one policy.</p>}
    {data.rows.length > 50 && <p className="mute" style={{ fontSize: 12 }}>Showing 50 of {data.total}. Download for the full list.</p>}
  </div>;
}

// ---------- Holiday calendars ----------

const HOL_KINDS = ['National', 'Regional', 'Optional'];
/** A year written in a calendar's name ("Gujarat 2027"), as the server reads it. */
const yearIn = (n: string) => /(?:^|\D)((?:19|20)\d{2})(?:\D|$)/.exec(n)?.[1] ?? '';

/** One holiday in the calendar dialog: date, name and type are edited in place and saved per row; the switch saves at once. */
function HolRow({ x, edit, year, save }: { x: any; edit: boolean; year: number; save: (h: any) => Promise<boolean> }) {
  const [d, setD] = useState({ date: x.date, name: x.name, kind: x.kind });
  useEffect(() => setD({ date: x.date, name: x.name, kind: x.kind }), [x.date, x.name, x.kind]);
  const changed = d.date !== x.date || d.name.trim() !== x.name || d.kind !== x.kind;
  if (!edit) return <tr><td>{fmtD(x.date)}</td><td>{x.name}</td><td><Pill k={x.kind === 'National' ? 'info' : 'n'}>{x.kind}</Pill></td><td><Switch on={x.on_list} disabled /></td><td /></tr>;
  const lim = year ? { min: `${year}-01-01`, max: `${year}-12-31` } : {};
  const saveRow = async () => {
    if (!d.date || !d.name.trim()) { toast('Enter date and name'); return; }
    if (await save({ id: x.id, date: d.date, name: d.name.trim(), kind: d.kind })) toast('Holiday saved');
  };
  return <tr>
    <td><input type="date" aria-label="Date" {...lim} value={d.date} onChange={(e) => setD({ ...d, date: e.target.value })} style={{ maxWidth: 160 }} /></td>
    <td><input type="text" aria-label="Holiday" value={d.name} onChange={(e) => setD({ ...d, name: e.target.value })} style={{ minWidth: 150 }} /></td>
    <td><select aria-label="Type" value={d.kind} onChange={(e) => setD({ ...d, kind: e.target.value })}>{HOL_KINDS.map((k) => <option key={k}>{k}</option>)}</select></td>
    <td><Switch on={x.on_list} onChange={() => save({ id: x.id, on: !x.on_list })} /></td>
    <td style={{ whiteSpace: 'nowrap' }}>{changed && <><button className="btn sm primary" onClick={saveRow}>Save</button> <button className="btn sm" onClick={() => setD({ date: x.date, name: x.name, kind: x.kind })}>Undo</button></>}</td>
  </tr>;
}

function Calendars({ sh }: { sh: Shared }) {
  const { can } = useAuth();
  const edit = can('pol.policies', 2);
  const [nc, setNc] = useState({ name: '', from: '', year: '' });
  const [open, setOpen] = useState('');
  const [h, setH] = useState({ date: '', name: '', kind: 'National' });
  const [nm, setNm] = useState('');
  const cal = sh.cals.find((c) => c.id === open);
  useEffect(() => setNm(cal?.name ?? ''), [open, cal?.name]);
  const put = async (id: string, body: any) => { try { await api(`/calendars/${id}`, { method: 'PUT', body }); sh.reload(); return true; } catch (x: any) { toast(x.message); return false; } };
  const add = async () => {
    if (!nc.name.trim()) { toast('Name the calendar'); return; }
    try {
      await api('/calendars', { body: { name: nc.name.trim(), copyFrom: nc.from, ...(+nc.year ? { year: +nc.year } : {}) } });
      setNc({ name: '', from: '', year: '' }); sh.reload(); toast(nc.from ? 'Calendar added with the holidays copied' : 'Calendar added. Add its holidays');
    } catch (x: any) { toast(x.message); }
  };
  const addH = async () => {
    if (!h.date || !h.name.trim()) { toast('Enter date and name'); return; }
    if (await put(cal.id, { holiday: { date: h.date, name: h.name.trim(), kind: h.kind } })) setH({ date: '', name: '', kind: 'National' });
  };
  const flip = async (c: any) => { const on = c.status !== 'Active'; if (await put(c.id, { active: on })) toast(`${c.name} is ${on ? 'active' : 'inactive'}`); };
  const rename = async () => { if (!nm.trim()) { toast('Name the calendar'); return; } if (await put(cal.id, { name: nm.trim() })) toast('Name saved'); };
  const lim = cal?.year ? { min: `${cal.year}-01-01`, max: `${cal.year}-12-31` } : {};
  return <>
    {edit && <div className="card" style={{ marginBottom: 14 }}><h3>Add calendar</h3><div className="row">
      <input type="text" aria-label="Calendar name" value={nc.name} onChange={(e) => setNc({ ...nc, name: e.target.value, year: yearIn(e.target.value) || nc.year })} placeholder="e.g. Gujarat 2026" style={{ maxWidth: 260 }} />
      <input type="number" aria-label="Year" min={2000} max={2100} value={nc.year} onChange={(e) => setNc({ ...nc, year: e.target.value })} placeholder={`Year, ${new Date().getFullYear()}`} style={{ maxWidth: 130 }} />
      <select aria-label="Start from" value={nc.from} onChange={(e) => setNc({ ...nc, from: e.target.value })}><option value="">Start empty</option>{sh.cals.map((c) => <option key={c.id} value={c.id}>Copy from {c.name}</option>)}</select>
      <button className="btn primary" onClick={add}>Add calendar</button></div>
      <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>A new calendar starts empty. Copy another calendar to start from its holidays, then edit their dates for the new year.</p></div>}
    <div className="card"><div className="tw"><table><thead><tr><th>Calendar</th><th>Year</th><th>Holidays</th><th>Used by</th><th>Active</th><th></th></tr></thead><tbody>
      {sh.cals.map((c) => <tr key={c.id}><td><b>{c.name}</b></td><td>{c.year || '—'}</td><td>{c.holidays.filter((x: any) => x.on_list).length} active, {c.holidays.length} total</td>
        <td style={{ fontSize: 12.5 }}>{c.usedBy?.length ? c.usedBy.join(', ') : <span className="mute">No policy</span>}</td>
        <td><Switch on={c.status === 'Active'} disabled={!edit} onChange={() => flip(c)} /></td><td><button className="btn sm" onClick={() => setOpen(c.id)}>Holidays</button></td></tr>)}
    </tbody></table></div><p className="mute" style={{ fontSize: 12, margin: '12px 0 0' }}>Confirm each state's notified holiday list before use. A calendar a policy uses cannot be made inactive, and an inactive calendar cannot be picked for a policy.</p></div>
    {cal && <Modal wide onClose={() => setOpen('')}>
      <div className="row sp"><div className="row">{edit ? <><input type="text" aria-label="Calendar name" value={nm} onChange={(e) => setNm(e.target.value)} style={{ maxWidth: 260, fontWeight: 700 }} />
        {nm.trim() !== cal.name && <button className="btn sm primary" onClick={rename}>Save name</button>}</> : <h2 style={{ margin: 0 }}>{cal.name}</h2>}
        {cal.status !== 'Active' && <Pill>Inactive</Pill>}</div><button className="btn sm" onClick={() => setOpen('')}>Close</button></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 10px' }}>{cal.year ? `Holidays of ${cal.year}. ` : ''}Change a date, name or type and press Save on that row. A holiday in a payroll month already approved or published cannot be changed.</p>
      <div className="tw" style={{ maxHeight: '44vh', overflow: 'auto' }}><table><thead><tr><th>Date</th><th>Holiday</th><th>Type</th><th>Active</th><th></th></tr></thead><tbody>
        {[...cal.holidays].sort((a: any, b: any) => a.date.localeCompare(b.date)).map((x: any) => <HolRow key={x.id} x={x} edit={edit} year={cal.year} save={(hol) => put(cal.id, { holiday: hol })} />)}
        {!cal.holidays.length && <tr><td colSpan={5} className="mute">No holidays yet.</td></tr>}
      </tbody></table></div>
      {edit && <><h3 style={{ margin: '16px 0 8px' }}>Add holiday</h3><div className="row">
        <input type="date" aria-label="New holiday date" {...lim} value={h.date} onChange={(e) => setH({ ...h, date: e.target.value })} style={{ maxWidth: 170 }} />
        <input type="text" value={h.name} onChange={(e) => setH({ ...h, name: e.target.value })} placeholder="Holiday name" style={{ maxWidth: 220 }} />
        <select value={h.kind} onChange={(e) => setH({ ...h, kind: e.target.value })}>{HOL_KINDS.map((k) => <option key={k}>{k}</option>)}</select>
        <button className="btn primary" onClick={addH}>Add</button></div></>}
    </Modal>}
  </>;
}

// ---------- Leave plans ----------

/** Fields of a leave type in a plan (prototype ltFields). A type in an Earn by working days rule (earned) shows its
 *  Credit as that rule and Days a year as an optional cap. */
const ltFields = (kind: string, earned = false) => {
  const a: PF[] = [['on', 'Available in this plan', 'yn', null], ['gender', 'For', 'sel', [['all', 'Everyone'], ['Female', 'Women only'], ['Male', 'Men only']]], ['minSvc', 'Months of service needed', 'num', null], ['inProb', 'Usable in probation', 'yn', null]];
  if (kind === 'balance') a.push(['days', earned ? 'Days a year, the most that can be earned (optional)' : 'Days a year', 'num', null, earned ? 'Blank or 0: no cap. Earned only by paid days (tab Earn by working days).' : ''],
    ['credit', 'Credit', 'sel', earned ? [['earn', 'By working days (see Earn by working days)']] : [['monthly', 'Monthly'], ['upfront', 'Upfront for the year']]],
    ['from', 'Accrual starts', 'sel', [['doj', 'From joining'], ['prob', 'After probation is completed']]],
    ['catchup', 'Back-date at confirmation', 'yn', null], ['cf', 'Carry forward (days)', 'num', null, '', 1], ['encash', 'Encashment on exit', 'yn', null, '', 1]);
  if (kind === 'event') a.push(['days', 'Days for one occasion', 'num', null], ['times', 'Most times in service, 0 for no limit', 'num', null]);
  a.push(['adv', 'Minimum notice (days before the leave starts)', 'num', null], ['advOnly', 'Must be applied in advance, no back-dating', 'yn', null], ['back', 'Days after the leave date it can still be applied, when back-dating is allowed', 'num', null],
    ['offs', 'Weekly offs and holidays inside the leave dates', 'sel', [['exclude', 'Not counted as leave'], ['sandwich', 'Counted only when between leave days'], ['include', 'Always counted as leave']]]);
  if (kind !== 'event') a.push(['maxReq', 'Most days in one request, 0 for no limit', 'num', null]);
  a.push(['maxMonth', 'Most days in a month, 0 for no limit', 'num', null], ['docAfter', 'Document needed when more than (days), 0 for never', 'num', null], ['docLabel', 'Document name', 'text', null],
    ['onNotice', 'During resignation or notice period', 'sel', [['allow', 'Allowed'], ['restrict', 'Not allowed']]], ['applyBy', 'Applied by', 'sel', [['self', 'Employee or manager'], ['hr', 'HRBP or Corporate HR on behalf of the employee']]],
    ['blk', 'Blackout periods apply', 'yn', null], ['grp', 'Approval route', 'sel', [['C', 'Casual and sick rules'], ['P', 'Privilege and other rules']]]);
  return a;
};

function LeavePlans({ sh, open, setOpen }: { sh: Shared; open: string; setOpen: (id: string) => void }) {
  const { can } = useAuth();
  const canC = can('pol.policies', 2);
  const [f, setF] = useState('Active');
  const [nplan, setNplan] = useState<{ name: string; from: string } | null>(null);
  const [ntype, setNtype] = useState<{ name: string; kind: string; paid: string; from: string } | null>(null);
  const all = sh.plans.plans.map((x) => x.plan);
  const emps = (id: string) => sh.pols.filter((p) => p.leavePlanId === id).reduce((a, p) => a + p.employees, 0);
  const one = all.find((x) => x.id === open);
  const createPlan = async () => {
    const nm = nplan!.name.trim();
    if (!nm) { toast('Name the plan'); return; }
    if (all.some((x) => x.name.toLowerCase() === nm.toLowerCase())) { toast('That name already exists. Reactivate or rename it'); return; }
    try { const r = await api('/leave-plans', { body: { name: nm, copyFrom: nplan!.from || all[0]?.id || '' } }); setNplan(null); sh.reload(); setOpen(r.id); toast('Created. Attach it to a work policy'); } catch (x: any) { toast(x.message); }
  };
  const createType = async () => {
    const nm = ntype!.name.trim().replace(/\s+/g, ' ');
    if (!/^[A-Za-z0-9 -]{2,40}$/.test(nm)) { toast('Use 2 to 40 letters, numbers, spaces or hyphens'); return; }
    if (sh.plans.types.some((x) => x.name.toLowerCase() === nm.toLowerCase())) { toast('That leave type already exists. Reactivate it instead'); return; }
    try { await api('/leave-types', { body: { name: nm, kind: ntype!.kind, paid: ntype!.paid === 'paid', copyFrom: ntype!.from } }); setNtype(null); sh.reload(); toast('Added. Switch it on in the plans that should give it'); } catch (x: any) { toast(x.message); }
  };
  /** Prototype ltTog: a leave type made inactive disappears from new requests; pending ones continue. */
  const togType = async (i: any) => {
    const on = i.status !== 'Active';
    try {
      const r = await api(`/leave-types/${encodeURIComponent(i.name)}/${on ? 'reactivate' : 'inactive'}`, { method: 'POST' });
      const n = +(r?.pending || 0);
      toast(on ? 'Reactivated' : `Inactive. It disappears from new requests${n ? `, ${n} pending request${n > 1 ? 's' : ''} continue` : ''}`); sh.reload();
    } catch (x: any) { toast(x.message); }
  };
  if (one) return <PlanOne pl={one} sh={sh} emps={emps(one.id)} onBack={() => setOpen('')} onOpen={setOpen} />;
  const list = all.filter((x) => f === 'All' || x.status === f);
  return <>
    <div className="row sp" style={{ marginBottom: 14 }}>
      <div className="seg">{['Active', 'Inactive', 'All'].map((x) => <button key={x} className={f === x ? 'on' : ''} onClick={() => setF(x)}>{x}<b>{all.filter((y) => x === 'All' || y.status === x).length}</b></button>)}</div>
      <div className="row" style={{ gap: 8 }}>{canC && <><button className="btn" onClick={() => setNtype({ name: '', kind: 'event', paid: 'paid', from: '' })}>New leave type</button><button className="btn primary" onClick={() => setNplan({ name: '', from: all[0]?.id ?? '' })}>New leave plan</button></>}</div>
    </div>
    <div className="note" style={{ marginBottom: 14 }}>A leave plan holds the leave types an employee gets and the rules for each one: credit, who it applies to, notice, documents, resignation and who applies. Attach a plan to a work policy, the same way a holiday calendar is attached. Plans and leave types are made inactive, never deleted.</div>
    <div className="card"><div className="tw"><table><thead><tr><th>Leave plan</th><th>Leave credited</th><th>Used by policies</th><th className="num">Employees</th><th>Status</th><th></th></tr></thead><tbody>
      {list.map((pl) => { const ps = sh.pols.filter((p) => p.leavePlanId === pl.id); return <tr key={pl.id}>
        <td><b>{pl.name}</b><div className="mute" style={{ fontSize: 12 }}>{pl.description || ''}</div></td><td>{lpBrief(pl, sh.plans.types)}</td>
        <td>{ps.length ? ps.map((p) => p.name).join(', ') : <span className="mute">None</span>}</td><td className="num">{emps(pl.id)}</td>
        <td><Pill k={pl.status === 'Active' ? 'ok' : 'n'}>{pl.status}</Pill></td><td><button className="btn sm" onClick={() => setOpen(pl.id)}>Open</button></td></tr>; })}
      {!list.length && <tr><td colSpan={6} className="mute">None.</td></tr>}
    </tbody></table></div></div>
    <div className="card" style={{ marginTop: 14 }}><h3>Leave types</h3><p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Every leave type available to plans. Add a type once, then switch it on in the plans that should give it.</p>
      <div className="tw"><table><thead><tr><th>Leave type</th><th>Kind</th><th>Pay</th><th>Plans using it</th><th>Status</th><th></th></tr></thead><tbody>
        {sh.plans.types.map((i) => <tr key={i.name}><td><b>{i.name}</b></td><td>{i.kind === 'balance' ? 'Days a year' : i.kind === 'event' ? 'Fixed days per occasion' : 'No balance'}</td><td>{i.paid ? 'Paid' : 'Unpaid'}</td>
          <td>{all.filter((p) => p.types[i.name]?.on).length}</td><td><Pill k={i.status === 'Active' ? 'ok' : 'n'}>{i.status}</Pill></td>
          <td>{canC && <button className="btn sm" onClick={() => togType(i)}>{i.status === 'Active' ? 'Make inactive' : 'Reactivate'}</button>}</td></tr>)}
      </tbody></table></div></div>
    {nplan && <Modal onClose={() => setNplan(null)}>
      <div className="row sp"><h2>New leave plan</h2><button className="btn sm" onClick={() => setNplan(null)}>Close</button></div>
      <div className="form" style={{ marginTop: 12 }}>
        <label className="field"><span>Plan name</span><input type="text" value={nplan.name} onChange={(e) => setNplan({ ...nplan, name: e.target.value })} placeholder="For example: Managers leave plan" /></label>
        <label className="field"><span>Start from</span><select value={nplan.from} onChange={(e) => setNplan({ ...nplan, from: e.target.value })}>{all.map((x) => <option key={x.id} value={x.id}>Copy of {x.name}</option>)}</select></label>
      </div>
      <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={createPlan}>Create</button></div>
    </Modal>}
    {ntype && <Modal onClose={() => setNtype(null)}>
      <div className="row sp"><h2>New leave type</h2><button className="btn sm" onClick={() => setNtype(null)}>Close</button></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '6px 0 12px' }}>Examples: Paternity leave, Marriage leave, Bereavement leave, Birthday leave, Study leave, Unpaid sabbatical. After adding, set the details in each plan.</p>
      <div className="form">
        <label className="field"><span>Name</span><input value={ntype.name} onChange={(e) => setNtype({ ...ntype, name: e.target.value })} placeholder="For example: Paternity leave" /></label>
        <label className="field"><span>Kind</span><select value={ntype.kind} onChange={(e) => setNtype({ ...ntype, kind: e.target.value })}><option value="event">Fixed days for one occasion</option><option value="balance">Days a year, with a balance</option><option value="none">No balance</option></select></label>
        <label className="field"><span>Pay</span><select value={ntype.paid} onChange={(e) => setNtype({ ...ntype, paid: e.target.value })}><option value="paid">Paid</option><option value="unpaid">Unpaid, loss of pay</option></select></label>
        <label className="field"><span>Copy settings from</span><select value={ntype.from} onChange={(e) => setNtype({ ...ntype, from: e.target.value })}><option value="">Standard defaults</option>{sh.plans.types.map((x) => <option key={x.name}>{x.name}</option>)}</select></label>
      </div>
      <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={createType}>Add leave type</button></div>
    </Modal>}
  </>;
}

function PlanOne({ pl: saved, sh, emps, onBack, onOpen }: { pl: any; sh: Shared; emps: number; onBack: () => void; onOpen: (id: string) => void }) {
  const { can } = useAuth();
  const canE = can('pol.policies', 2);
  const [t, setT] = useState('bal');
  const [pl, setPl] = useState<any>(() => structuredClone(saved));
  const [eff, setEff] = useState(today());
  const hist = useLoad(() => (t === 'hist' ? api<any[]>(`/leave-plans/${saved.id}/history`) : Promise.resolve(null)), [saved.id, t, saved.version]);
  useEffect(() => setPl(structuredClone(saved)), [saved, t]);
  const ps = sh.pols.filter((p) => p.leavePlanId === saved.id);
  const types = sh.plans.types.filter((i) => i.status === 'Active' && pl.types[i.name]);
  const setLv = (k: string, v: any) => setPl({ ...pl, lv: { ...pl.lv, [k]: v } });
  const setLrx = (k: string, v: any) => setPl({ ...pl, lrx: { ...pl.lrx, [k]: v } });
  const put = async (x: any, extra: any = {}) => api(`/leave-plans/${saved.id}`, { method: 'PUT', body: { name: x.name, description: x.description, lv: x.lv, lrx: x.lrx, types: x.types, ...extra } });
  const save = async () => {
    if (!String(pl.name).trim()) { toast('Name the plan'); return; }
    if (sh.plans.plans.some((x) => x.plan.id !== saved.id && x.plan.name.toLowerCase() === pl.name.trim().toLowerCase())) { toast('That name already exists'); return; }
    for (const [n, c] of Object.entries<any>(pl.types)) {
      const i = sh.plans.types.find((x) => x.name === n) || {};
      if (c.on && !c.types.length) { toast(`${n}: pick at least one employment type`); return; }
      if (c.on && i.kind === 'event' && !(c.days >= 1)) { toast(`${n}: days for one occasion must be at least 1`); return; }
      if (c.docAfter > 0 && !String(c.docLabel || '').trim()) { toast(`${n}: name the document that is needed`); return; }
    }
    const why = earnProblem(pl.lv.earn ?? []);
    if (why) { toast(why); setT('earn'); return; }
    const n = nChanges({ a: saved.name, b: saved.description, lv: saved.lv, lrx: saved.lrx, t: saved.types }, { a: pl.name, b: pl.description, lv: pl.lv, lrx: pl.lrx, t: pl.types });
    if (!eff) { toast('Pick the date the change applies from'); return; }
    try { await put(pl, { effectiveFrom: eff }); toast(n ? `Saved, ${n} change${n === 1 ? '' : 's'} recorded. ${ps.length} polic${ps.length === 1 ? 'y' : 'ies'} updated` : 'Nothing changed'); sh.reload(); } catch (x: any) { toast(x.message); }
  };
  const toggle = async () => {
    try {
      await put(saved, { active: saved.status !== 'Active' });
      const used = ps.filter((p) => p.status === 'Active').length;
      toast(saved.status === 'Active' ? (used ? `Inactive. ${used} polic${used === 1 ? 'y' : 'ies'} keep using it, but it cannot be picked for another policy` : 'Inactive') : 'Reactivated'); sh.reload();
    } catch (x: any) { toast(x.message); }
  };
  const dup = async () => {
    let name = saved.name + ' copy', i = 2;
    while (sh.plans.plans.some((x) => x.plan.name === name)) name = `${saved.name} copy ${i++}`;
    try { const r = await api('/leave-plans', { body: { name, copyFrom: saved.id } }); sh.reload(); onOpen(r.id); toast('Duplicated'); } catch (x: any) { toast(x.message); }
  };
  const yn = (k: string, l: string, v: boolean, set: (v: boolean) => void) => <label className="field"><span>{l}</span><select value={String(!!v)} onChange={(e) => set(e.target.value === 'true')}>{YN.map(([a, b]) => <option key={a} value={a}>{b}</option>)}</select></label>;
  let body: JSX.Element | null = null;
  if (t === 'bal') body = <>
    <div className="card" style={{ marginBottom: 14 }}><div className="form">
      <label className="field"><span>Plan name</span><input type="text" value={pl.name} onChange={(e) => setPl({ ...pl, name: e.target.value })} /></label>
      <label className="field" style={{ gridColumn: 'span 2' }}><span>Description, who it is for</span><input type="text" value={pl.description || ''} onChange={(e) => setPl({ ...pl, description: e.target.value })} /></label>
    </div></div>
    <div className="card" style={{ marginBottom: 14 }}><h3>Leave year</h3><div className="form">
      <Fld f={['lv.year', 'Leave year', 'sel', [['cal', 'January to December'], ['fin', 'April to March'], ['anniv', 'Joining anniversary']]]} v={pl.lv.year} set={(v) => setLv('year', v)} />
      <Fld f={['lv.prorate', 'Pro-rate for joiners and leavers', 'yn', null]} v={pl.lv.prorate} set={(v) => setLv('prorate', v)} />
      <Fld f={['lv.half', 'Half-day leave allowed', 'yn', null, 'First or second half of one day: 0.5 of the balance; the other half follows the shift\'s half-day hours.']} v={pl.lv.half} set={(v) => setLv('half', v)} />
      <Fld f={['lv.neg', 'Negative balance allowed (days)', 'num', null, '0 means not allowed.']} v={pl.lv.neg} set={(v) => setLv('neg', v)} />
    </div></div>
    <div className="card" style={{ marginBottom: 14 }}><h3>Short leave</h3><div className="form">
      <Fld f={['lv.shortOff', 'Short leave allowed', 'yn', null, 'Part of one day, from a time to a time, through the leave approval. No balance is used.']} v={!pl.lv.shortOff} set={(v) => setLv('shortOff', !v)} />
      {!pl.lv.shortOff && <><Fld f={['lv.shortN', 'Short leaves allowed in a month', 'num', null, 'Blank or 0 means 2.']} v={pl.lv.shortN || 2} set={(v) => setLv('shortN', Math.max(0, Math.floor(+v || 0)))} />
        <Fld f={['lv.shortH', 'Most hours in one short leave', 'num', null, 'Blank or 0 means 2.']} v={pl.lv.shortH || 2} set={(v) => setLv('shortH', Math.max(0, +v || 0))} /></>}
    </div><p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>An approved short leave excuses that time: no late mark, early going or short-hours penalty, and the day stays Present.</p></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Open a leave type to set who it applies to, notice, documents, resignation and who applies. A ◦ marks a setting that is saved with the plan and applied in the full build.</p>
    {types.map((i) => {
      const c = pl.types[i.name], setT_ = (k: string, v: any) => setPl({ ...pl, types: { ...pl.types, [i.name]: { ...c, [k]: v } } }), earned = i.kind === 'balance' ? earnOf(pl, i.name) : null;
      return <details key={i.name} className="card" style={{ marginBottom: 10 }}>
        <summary style={{ cursor: 'pointer', display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}><b>{i.name}</b><Pill k={c.on ? 'ok' : 'n'}>{c.on ? 'On' : 'Off'}</Pill><span className="mute" style={{ fontSize: 12.5 }}>{ltBrief(i.kind, i.paid, c, earned)} · {c.types.join(', ') || 'No employment type'}</span></summary>
        <div style={{ marginTop: 12 }}><div className="form">{ltFields(i.kind, !!earned).map((f) => <Fld key={f[0]} f={f} v={earned && f[0] === 'credit' ? 'earn' : earned && f[0] === 'days' && !c.days ? '' : f[2] === 'num' ? Math.max(0, c[f[0]] ?? 0) : c[f[0]]} set={(v) => { if (!(earned && f[0] === 'credit')) setT_(f[0], f[2] === 'num' ? Math.max(0, Math.floor(+v || 0)) : v); }} />)}</div>
          <div style={{ marginTop: 12 }}><b style={{ fontSize: 13 }}>Applies to employment types</b><div className="row" style={{ gap: 16, marginTop: 6 }}>{LT_EMP.map((e) => <label key={e}><input type="checkbox" checked={c.types.includes(e)} onChange={(ev) => setT_('types', ev.target.checked ? [...c.types, e] : c.types.filter((x: string) => x !== e))} /> {e}</label>)}</div></div>
          <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>{i.kind === 'balance' ? (earned ? `Earned only by working days (tab Earn by working days): ${PAID_DAYS}, credited the day after the Nth paid day, active employees only; no monthly or upfront credit; checked against the balance when someone applies. ` : 'Days build up through the year and are checked against the balance when someone applies. ') : i.kind === 'event' ? 'A fixed number of days for one occasion, with no yearly balance. ' : 'No balance is kept. '}{!i.paid ? 'Unpaid: these days are loss of pay in payroll. ' : ''}{c.applyBy === 'hr' ? 'Only HRBP or Corporate HR can raise this request, for the employee. ' : ''}</p></div>
      </details>;
    })}
    {sh.plans.types.some((i) => i.status !== 'Active') && <p className="mute" style={{ fontSize: 12 }}>{s_(sh.plans.types.filter((i) => i.status !== 'Active').length, 'inactive leave type is', 'inactive leave types are')} hidden. Reactivate from the Leave plans list.</p>}
  </>;
  if (t === 'earn') body = <EarnRules pl={pl} types={sh.plans.types} set={(r) => setLv('earn', r)} />;
  if (t === 'rules') body = <div className="card" style={{ marginBottom: 14 }}><h3>Protecting service levels</h3><div className="form">
    {yn('staffOn', 'Limit people on leave on one day', pl.lrx.staffOn, (v) => setLrx('staffOn', v))}
    <label className="field"><span>Most of a department at a location on leave (%)</span><input type="number" value={pl.lrx.staffPct} onChange={(e) => setLrx('staffPct', +e.target.value || 0)} /></label>
    <label className="field"><span>Casual leave joined to privilege leave</span><select value={pl.lrx.club || 'ok'} onChange={(e) => setLrx('club', e.target.value)}><option value="ok">Allowed</option><option value="no">Not allowed</option></select></label>
  </div></div>;
  if (t === 'hist') body = !hist.data ? <Loading error={hist.error} /> : <div className="card">{hist.data.length ? <ul className="log">{hist.data.map((h, i) => <li key={i}><b>{fmtAt(h.at)}</b> by {h.by_name}, applies from {fmtD(h.effective_from || iso(new Date(h.at)))}
      <div className="mute" style={{ fontSize: 12.5, marginTop: 4 }}>{(h.changes as any[]).slice(0, 16).map((c, j) => <span key={j}>{j > 0 && <br />}{lpLabel(c.field, sh.plans.types)}: {lpVal(c.from)} → {lpVal(c.to)}</span>)}</div></li>)}</ul>
      : <p className="mute" style={{ margin: 0 }}>No changes yet.</p>}</div>;
  return <>
    <div className="row sp" style={{ marginBottom: 12 }}>
      <div className="row"><button className="btn sm" onClick={onBack}>All leave plans</button><h2 style={{ margin: 0, fontSize: 20 }}>{saved.name}</h2><Pill k={saved.status === 'Active' ? 'ok' : 'n'}>{saved.status}</Pill><span className="mute">{ps.length} polic{ps.length === 1 ? 'y' : 'ies'} · {emps} employees</span></div>
      <div className="row">{canE && <><button className="btn sm" onClick={dup}>Duplicate</button><button className="btn sm" onClick={toggle}>{saved.status === 'Active' ? 'Make inactive' : 'Reactivate'}</button></>}</div>
    </div>
    <div className="tabs" style={{ marginBottom: 14 }}>{[['bal', 'Leave types'], ['earn', 'Earn by working days'], ['rules', 'Cover and limits'], ['hist', 'History']].map(([k, l]) => <button key={k} className={`tab ${t === k ? 'on' : ''}`} onClick={() => setT(k)}>{l}</button>)}</div>
    <fieldset disabled={!canE} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }}>{body}</fieldset>
    {canE && t !== 'hist' && <div className="row" style={{ marginTop: 16, position: 'sticky', bottom: 0, background: 'var(--bg,#f5f6f8)', padding: '10px 0' }}>
      <label className="field" style={{ margin: 0 }}><span>Applies from</span><input type="date" value={eff} onChange={(e) => setEff(e.target.value)} /></label>
      <button className="btn primary" onClick={save} style={{ alignSelf: 'flex-end' }}>Save changes</button></div>}
  </>;
}

/** What is wrong with the Earn by working days rules before saving ('' when nothing; the server checks the same). */
const earnProblem = (rules: any[]) => {
  const seen: Record<string, number> = {}, half = (v: number) => Math.round(v * 2) === v * 2;
  for (const r of rules) {
    if (!(+r.days > 0) || !half(+r.days)) return 'Earn by working days: type the paid days of each rule, in whole or half days';
    if (!(r.types ?? []).length) return `Earn by working days, every ${r.days} paid days: add at least one leave type`;
    for (const x of r.types) {
      if (!x.type) return `Earn by working days, every ${r.days} paid days: choose the leave type of each row`;
      if (!(+x.days > 0) || !half(+x.days)) return `Earn by working days, ${x.type}: type the days earned, in whole or half days`;
      if (seen[x.type] != null) return `${x.type} is already in the rule for every ${seen[x.type]} paid days. A leave type can be in only one rule`;
      seen[x.type] = r.days;
    }
  }
  return '';
};

/** Earn by working days (owner, 24 Nov 2026; evening: working days are paid days, credited the day after the Nth): rules
 *  of "every time the employee completes these paid days, these leave types are credited these days". Every number is
 *  typed by the admin; nothing is filled in. */
function EarnRules({ pl, types, set }: { pl: any; types: any[]; set: (rules: any[]) => void }) {
  const rules: any[] = pl.lv.earn ?? [];
  const bal = types.filter((i) => i.kind === 'balance' && i.status === 'Active' && pl.types[i.name]);
  const upd = (i: number, r: any) => set(rules.map((x, j) => (j === i ? r : x)));
  const row = (i: number, k: number, x: any) => upd(i, { ...rules[i], types: rules[i].types.map((y: any, j: number) => (j === k ? { ...y, ...x } : y)) });
  const num = (v: string) => (v === '' ? null : Math.max(0, +v));
  return <>
    <div className="note" style={{ marginBottom: 14 }}>Each rule: every time an employee completes the paid days, each leave type in it is credited its days earned, from the day after the Nth paid day (not at the month end). Working days here are {PAID_DAYS}: a day counts 1 less its loss of pay as payroll sees it, so a no-punch day counts until it turns to loss of pay after the regularization window. Days count once they are over, from joining (or the accrual start set on the Leave types tab), and only while the employee is active: nothing after the last working day. The count runs through the leave year, so a block can run across months; the days left over count toward the next block, and the count starts again with each leave year. A leave type in a rule is earned only this way, with no monthly or upfront credit; its Days a year is an optional cap. A leave type can be in only one rule.</div>
    {rules.map((r, i) => <div key={i} className="card" style={{ marginBottom: 10 }}>
      <div className="row sp" style={{ alignItems: 'flex-end' }}><label className="field" style={{ maxWidth: 220, margin: 0 }}><span>Paid days</span>
        <input type="number" min={0} step={0.5} placeholder="Paid days" value={r.days ?? ''} onChange={(e) => upd(i, { ...r, days: num(e.target.value) })} /></label>
        <button className="btn sm" onClick={() => set(rules.filter((_, j) => j !== i))}>Remove rule</button></div>
      <div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Leave type</th><th>Days earned</th><th></th></tr></thead><tbody>
        {(r.types ?? []).map((x: any, k: number) => <tr key={k}>
          <td><select aria-label="Leave type" value={x.type ?? ''} onChange={(e) => row(i, k, { type: e.target.value })}><option value="">Choose a leave type</option>
            {bal.map((b) => <option key={b.name} value={b.name}>{b.name}{pl.types[b.name]?.on ? '' : ' (off in this plan)'}</option>)}</select></td>
          <td><input type="number" min={0} step={0.5} placeholder="Days earned" aria-label="Days earned" value={x.days ?? ''} onChange={(e) => row(i, k, { days: num(e.target.value) })} /></td>
          <td><button className="btn sm" onClick={() => upd(i, { ...r, types: r.types.filter((_: any, j: number) => j !== k) })}>Remove</button></td></tr>)}
        {!(r.types ?? []).length && <tr><td colSpan={3} className="mute">No leave type in this rule yet.</td></tr>}
      </tbody></table></div>
      <div className="row" style={{ marginTop: 8 }}><button className="btn sm" onClick={() => upd(i, { ...r, types: [...(r.types ?? []), { type: '', days: null }] })}>Add leave type</button></div>
    </div>)}
    {!rules.length && <p className="mute" style={{ fontSize: 12.5 }}>No rule. Every leave type is credited as set on the Leave types tab.</p>}
    <button className="btn" onClick={() => set([...rules, { days: null, types: [{ type: '', days: null }] }])}>Add rule</button>
  </>;
}

/** A leave plan history field (server path such as types.Casual leave.days) as the setting's label. */
const LP_LAB: Record<string, string> = { name: 'Plan name', description: 'Description', 'lv.earn': 'Earn by working days', 'lv.year': 'Leave year', 'lv.prorate': 'Pro-rate for joiners and leavers', 'lv.half': 'Half-day leave allowed',
  'lv.neg': 'Negative balance allowed (days)', 'lv.shortOff': 'Short leave switched off', 'lv.shortN': 'Short leaves allowed in a month', 'lv.shortH': 'Most hours in one short leave', 'lrx.staffOn': 'Limit people on leave on one day', 'lrx.staffPct': 'Most of a department at a location on leave (%)', 'lrx.club': 'Casual leave joined to privilege leave', status: 'Status' };
const lpLabel = (f: string, types: any[]) => {
  if (LP_LAB[f]) return LP_LAB[f];
  const m = /^types\.(.+)\.([^.]+)$/.exec(f);
  if (m) { const k = types.find((x) => x.name === m[1])?.kind ?? 'balance', l = ltFields(k).find((x) => x[0] === m[2])?.[1]; return `${m[1]}: ${l ?? (m[2] === 'types' ? 'Applies to employment types' : m[2])}`; }
  return f;
};
const lpVal = (v: any) => (v == null ? '—' : typeof v === 'boolean' ? (v ? 'Yes' : 'No') : Array.isArray(v) && v.some((x) => x && typeof x === 'object')
  ? v.map((r: any) => `every ${r.days} paid days: ${(r.types ?? []).map((x: any) => `${x.type} ${x.days}`).join(', ')}`).join('; ') : Array.isArray(v) ? v.join(', ') || 'none' : typeof v === 'object' ? JSON.stringify(v) : String(v));

// ---------- Exceptions ----------

function Exceptions() {
  const { me, can } = useAuth();
  const ok = me?.ctx.role === 'HR Admin' || me?.ctx.role === 'Payroll Manager' || can('pay.approve', 2) || can('pay.run', 2);
  const canList = ok || can('pay.run');
  const { data, error, reload } = useLoad(() => (canList ? api<any[]>('/exceptions') : Promise.resolve([])), []);
  const [f, setF] = useState({ emp: '', kind: 'regularize', from: '', to: '', until: addDays(today(), 3), why: '' });
  const [pe, setPe] = useState<Person | null>(null);
  const add = async () => {
    const emp = f.emp.split(' · ')[0].trim().toUpperCase(), to = f.to || f.from;
    if (!emp || !f.from || !f.until || !f.why.trim()) { toast('Pick the employee, dates, open-until date and a reason'); return; }
    if (to < f.from) { toast('The end date is before the start date'); return; }
    if (f.until < today()) { toast('Open-until date is in the past'); return; }
    try { await api('/exceptions', { body: { employeeId: emp, kind: f.kind, from: f.from, to, until: f.until, reason: f.why.trim() } }); toast('Exception opened'); setF({ emp: '', kind: 'regularize', from: '', to: '', until: addDays(today(), 3), why: '' }); setPe(null); reload(); } catch (x: any) { toast(x.message); }
  };
  const revoke = async (id: number) => { try { await api(`/exceptions/${id}/revoke`, { method: 'POST' }); reload(); } catch (x: any) { toast(x.message); } };
  const dt = today();
  return <>
    <div className="note" style={{ marginBottom: 14 }}>An exception opens a closed leave or regularization window for one employee and specific dates, until a chosen date. It is for resolving conflicts, and each one is recorded with a reason. Exceptions are revoked, never deleted.</div>
    {ok ? <div className="card" style={{ marginBottom: 14 }}><h3>Open an exception</h3><div className="form">
      <label className="field"><span>Employee</span>{can('emp.directory')
        ? <PersonPicker label="Employee" search="&status=Active" value={f.emp} selected={pe} onChange={(id, x) => { setPe(x); setF({ ...f, emp: id }); }} placeholder="Employee ID or name" />
        : <input type="text" value={f.emp} onChange={(e) => setF({ ...f, emp: e.target.value })} placeholder="Employee ID" />}</label>
      <label className="field"><span>For</span><select value={f.kind} onChange={(e) => setF({ ...f, kind: e.target.value })}><option value="regularize">Attendance regularization</option><option value="leave">Leave application</option></select></label>
      <label className="field"><span>Dates from</span><input type="date" value={f.from} onChange={(e) => setF({ ...f, from: e.target.value })} /></label>
      <label className="field"><span>Dates to</span><input type="date" value={f.to} onChange={(e) => setF({ ...f, to: e.target.value })} /></label>
      <label className="field"><span>Open until</span><input type="date" value={f.until} onChange={(e) => setF({ ...f, until: e.target.value })} /></label>
      <label className="field"><span>Reason</span><input type="text" value={f.why} onChange={(e) => setF({ ...f, why: e.target.value })} placeholder="For example: system outage on 28 Oct" /></label>
    </div><div className="row" style={{ marginTop: 12 }}><button className="btn primary" onClick={add}>Open exception</button></div></div>
      : <div className="note" style={{ marginBottom: 14 }}>Only a payroll approver can open exceptions.</div>}
    {canList && <div className="card">{!data ? <Loading error={error} /> : <div className="tw"><table><thead><tr><th>Employee</th><th>For</th><th>Dates</th><th>Open until</th><th>Reason</th><th>By</th><th>Status</th><th></th></tr></thead><tbody>
      {data.map((x) => {
        const live = x.status === 'Active' && String(x.until).slice(0, 10) >= dt;
        return <tr key={x.id}><td><b>{x.name}</b> <span className="mono mute">{x.employee_id}</span></td><td>{x.kind === 'leave' ? 'Leave' : 'Regularization'}</td><td>{fmtD(x.from_date)} to {fmtD(x.to_date)}</td><td>{fmtD(x.until)}</td>
          <td className="ink2">{x.reason}</td><td>{x.created_by}<div className="mute" style={{ fontSize: 12 }}>{x.created_at ? fmtAt(x.created_at) : ''}</div></td>
          <td><Pill k={x.status === 'Revoked' ? 'bad' : live ? 'ok' : 'n'}>{x.status === 'Revoked' ? 'Revoked' : live ? 'Open' : 'Expired'}</Pill></td>
          <td>{live && ok && <button className="btn sm" onClick={() => revoke(x.id)}>Revoke</button>}</td></tr>;
      })}
      {!data.length && <tr><td colSpan={8} className="mute">No exceptions yet.</td></tr>}
    </tbody></table></div>}</div>}
  </>;
}
