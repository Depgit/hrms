import { Switch } from '../components/ui';

// Work policy › Attendance › Deviation allowances (owner, 24 Nov 2026). Three rules, each off until the admin turns it
// on with both numbers and an Action for a day beyond the allowance; nothing is prefilled (the Action starts at
// "Select"). The server refuses a rule that is on without them.

type Rule = { on?: boolean; mins?: number; n?: number; act?: string };

/** What a day beyond the allowance gets (policy.DevActs): the server's values and the words the admin sees. */
export const DEV_ACTS: [string, string][] = [['none', 'No action'], ['absent', 'Absent'], ['half', 'Half day until regularized'], ['present', 'Present until regularized']];
export const devActText = (a?: string) => DEV_ACTS.find((x) => x[0] === a)?.[1] ?? 'Not chosen';
/** The Action dropdown: "Select" until the admin picks one. */
export function DevActSelect({ value, label, disabled, onChange }: { value?: string; label: string; disabled?: boolean; onChange: (v: string) => void }) {
  return <select value={value || ''} disabled={disabled} aria-label={label + ', action beyond the allowance'} onChange={(e) => onChange(e.target.value)}>
    <option value="">Select</option>{DEV_ACTS.map(([v, t]) => <option key={v} value={v}>{t}</option>)}</select>;
}
const RULES: [string, string, string][] = [
  ['earlyIn', 'Early check-in', 'A check-in more than the early check-in allowance (Working time tab) and at most these minutes before the shift start. On the allowed days the shift runs that much earlier: hours count from the in punch.'],
  ['lateIn', 'Late check-in', 'A check-in after the shift start and the late-mark grace, at most these minutes late. On the allowed days it is no late mark (not counted in Late marks allowed in a month). No hours are credited: the full-day hours still decide the day.'],
  ['earlyOut', 'Early check-out', 'A check-out more than the early-out grace and at most these minutes before the shift end (later than an allowed early check-in moved it). On the allowed days: a note only, no early-exit deviation. No hours are credited.'],
];

/** The three deviation allowances of a policy: On/Off, minutes allowed and times allowed in a month, all typed by the admin. */
export function DevAllowCard({ s, set }: { s: any; set: (path: string, v: any) => void }) {
  const r = (k: string): Rule => s.dev?.[k] ?? {};
  const put = (k: string, patch: Rule) => set('dev.' + k, { on: false, mins: 0, n: 0, ...r(k), ...patch });
  const num = (v: string, max: number) => (v === '' ? 0 : Math.min(max, Math.max(0, Math.floor(+v || 0))));
  return <div className="card" style={{ marginBottom: 14 }}><h3>Deviation allowances</h3>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 10px' }}>Let a check-in or check-out differ from the shift by up to the minutes you set, on up to the times you set in a calendar month. A day beyond that (more times in the month, or more minutes) gets the rule's Action, and the HR Admins and the employee's reporting manager get a bell message in every case. <b>No action</b>: the usual rules apply (late mark, Late marks allowed in a month, hours). <b>Absent</b>: loss of pay 1 day. <b>Half day until regularized</b>: loss of pay half a day. <b>Present until regularized</b>: no loss of pay, flagged. Absent, Half day and Present last until a regularization of that date is approved, and replace the late mark that day (it is not counted in Late marks allowed in a month); a day whose hours give more loss of pay keeps it, never both. When two rules are beyond on the same day, the harsher action wins (Absent, then Half day, then Present, then No action). Leave days, regularized days and days called to work are never counted. For one person, click their name on Shift and roster › Roster to give them their own rules from a date.</p>
    <div className="tw"><table><thead><tr><th>Rule</th><th>On</th><th>Minutes allowed</th><th>Times allowed in a month</th><th>Action</th></tr></thead><tbody>
      {RULES.map(([k, label, hint]) => { const x = r(k); return <tr key={k}>
        <td><b>{label}</b><div className="mute" style={{ fontSize: 12, maxWidth: 460 }}>{hint}</div></td>
        <td><Switch on={!!x.on} label={label} onChange={() => put(k, { on: !x.on })} /></td>
        <td><input type="number" min={1} max={720} style={{ width: 110 }} aria-label={label + ', minutes allowed'} placeholder="Minutes" value={x.mins ? x.mins : ''} onChange={(e) => put(k, { mins: num(e.target.value, 720) })} /></td>
        <td><input type="number" min={1} max={31} style={{ width: 110 }} aria-label={label + ', times allowed in a month'} placeholder="Times" value={x.n ? x.n : ''} onChange={(e) => put(k, { n: num(e.target.value, 31) })} /></td>
        <td><DevActSelect value={x.act} label={label} onChange={(v) => put(k, { act: v || undefined })} /></td>
      </tr>; })}
    </tbody></table></div></div>;
}

/** A history entry's setting in words: dev.lateIn.mins → "Late check-in: minutes allowed". */
export const devLabel = (k: string) => {
  const [, rule, f] = k.split('.');
  const r = RULES.find((x) => x[0] === rule)?.[1] ?? 'Deviation allowance';
  return f ? `${r}: ${({ on: 'on', mins: 'minutes allowed', n: 'times allowed in a month', act: 'action beyond the allowance' } as Record<string, string>)[f] ?? f}` : r;
};
