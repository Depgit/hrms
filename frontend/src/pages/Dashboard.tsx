import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { api, addDays, fmtD, inr, today } from '../api';
import { SIZE_KEYS, SIZE_LABEL, UniformIssues, sizeText, type SizeKey, type UniIssue } from '../components/Uniform';
import { useAuth, type Me } from '../auth';
import { Bars, Chain, Loading, Modal, Pill, toast, useLoad } from '../components/ui';
import { NoticeStrip } from '../components/Notices';
import { Detail, WORKFLOWS, statusTone, subOf, wfName } from './Approvals';
import { MyPolicyAcks, useMyPolicyAckCount } from './PolicyAck';
import { MyDocsCard } from './MyDocuments';

type G = { label: string; active: number; total: number };
type B = { label: string; n: number };
type Tab = 'ov' | 'wf' | 'pay' | 'att' | 'cmp' | 'ap';

// Prototype DS_TABS: every role sees every tab; a tab the role cannot use says so.
const DS_TABS: [Tab, string][] = [['ov', 'Overview'], ['wf', 'Workforce'], ['pay', 'Payroll'], ['att', 'Attendance'], ['cmp', 'Compliance'], ['ap', 'Approvals']];
const TYPES = ['Permanent', 'Retainer', 'Contractual'];
/** Employment type names from the master (GET /masters/employment_type), in the prototype's master order
 *  (Permanent, Retainer, Contractual, then any others). */
export const typeNames = (l?: { name: string }[] | null) => {
  const P = ['Permanent', 'Retainer', 'Contractual'], n = l?.length ? l.map((t) => t.name) : P, at = (x: string) => (P.includes(x) ? P.indexOf(x) : P.length);
  return [...n].sort((a, b) => at(a) - at(b));
};

/** Prototype dsCard. With `go`, the whole card opens what it counts. */
const Card = ({ l, n, s, big, go }: { l: ReactNode; n: ReactNode; s?: ReactNode; big?: boolean; go?: () => void }) =>
  <div className="card stat" {...(go ? { role: 'button', tabIndex: 0, onClick: go, onKeyDown: (e: KeyboardEvent) => (e.key === 'Enter' || e.key === ' ') && (e.preventDefault(), go()), style: { cursor: 'pointer' } } : {})}>
    <div className="l">{l}</div><div className="n" style={big ? { fontSize: 20, paddingTop: 6 } : undefined}>{n}</div><div className="s">{s}</div></div>;
/** Prototype dsGo. */
const Go = ({ to, children }: { to: string; children: ReactNode }) => { const nav = useNavigate(); return <button className="btn" onClick={() => nav(to)}>{children}</button>; };
const bars = (l: B[]) => l.map((x) => ({ label: x.label, value: x.n }));
const s = (n: number, one: string, many = one + 's') => `${n} ${n === 1 ? one : many}`;
const pctS = (a: number, b: number) => (b ? Math.round((a / b) * 100) + '%' : '0%');
const ymLabel = (ym: string) => new Date(+ym.slice(0, 4), +ym.slice(5, 7) - 1, 1).toLocaleDateString('en-IN', { month: 'long', year: 'numeric' });

/** Prototype accessText for the signed-in role view: the names of the mapped locations and departments
 *  (GET /api/me ctx.scopeLocations, ctx.scopeDepartments), or a general wording while they are not supplied. */
export function scopeText(c?: Me['ctx']): string {
  const deps = c?.scopeDepartments ?? [], ds = deps.length ? ` · ${deps.join(', ')} only` : '';
  if (c?.access === 'all') return 'All locations' + ds;
  if (c?.access === 'team') return 'Own team' + ds;
  if (c?.access === 'self') return 'Self only';
  const ls = c?.scopeLocations;
  if (!ls) return 'your mapped locations';
  return ls.length ? (ls.length > 3 ? `${ls.length} locations: ${ls.slice(0, 3).join(', ')}…` : ls.join(', ')) : 'No locations picked';
}
/** Prototype scopeAll: every location and no department limit. */
export const scopeAll = (c?: Me['ctx']) => c?.access === 'all' && !c?.scopeDepartments?.length;

export default function Dashboard() {
  const { me, can } = useAuth();
  const [tab, setTab] = useState<Tab>('ov');
  const [f, setF] = useState({ city: '', department: '', type: '' });
  // My view (the Employee role view) is the person's own self-service: own figures only, no Workforce or Approvals.
  const my = me?.ctx.role === 'Employee';
  const cities = useLoad(() => (my ? Promise.resolve([]) : api<any[]>('/masters/city?status=Active')), [my]);
  const depts = useLoad(() => (my ? Promise.resolve([]) : api<any[]>('/masters/department?status=Active')), [my]);
  // Prototype rpTypes(): the employment type master.
  const types = useLoad(() => (my ? Promise.resolve([]) : api<any[]>('/masters/employment_type?status=Active').catch(() => [])), [my]);
  // In-app messages (components/Notices.tsx) head the Dashboard in every view.
  if (my) return <><NoticeStrip /><MyDashboard tab={tab} setTab={setTab} /></>;
  // Prototype bars follow the master order (M.city, M.dept), leaving out the empty ones.
  const order = { city: (cities.data ?? []).map((c) => c.name as string), dept: (depts.data ?? []).map((d) => d.name as string) };
  const fn: Record<Tab, () => JSX.Element> = {
    ov: () => <Overview f={f} order={order} />, wf: () => <Workforce f={f} />, pay: () => can('pay.run') ? <PayrollTab f={f} order={order} /> : <div className="card"><p className="mute" style={{ margin: 0 }}>Payroll figures are visible to payroll roles only.</p></div>,
    att: () => <AttendanceTab f={f} />, cmp: () => <Compliance f={f} />, ap: () => <ApprovalsTab f={f} />,
  };
  return (
    <>
      <NoticeStrip />
      {!scopeAll(me?.ctx) && <div className="note" style={{ marginBottom: 14 }}>Showing data for your scope: {scopeText(me?.ctx)}.</div>}
      <div className="row sp" style={{ marginBottom: 14, flexWrap: 'wrap', gap: 10 }}>
        <div className="tabs" style={{ margin: 0 }}>{DS_TABS.map(([k, l]) => <button key={k} className={`tab ${tab === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}</button>)}</div>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <select aria-label="City" value={f.city} onChange={(e) => setF({ ...f, city: e.target.value })}><option value="">All cities</option>{cities.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select>
          <select aria-label="Department" value={f.department} onChange={(e) => setF({ ...f, department: e.target.value })}><option value="">All departments</option>{depts.data?.map((x) => <option key={x.id}>{x.name}</option>)}</select>
          <select aria-label="Employment type" value={f.type} onChange={(e) => setF({ ...f, type: e.target.value })}><option value="">All types</option>{typeNames(types.data).map((t) => <option key={t}>{t}</option>)}</select>
        </div>
      </div>
      {fn[tab]()}
    </>
  );
}

type F = { city: string; department: string; type: string };
type Order = { city: string[]; dept: string[] };
/** Rows in master order (unknown labels last, in the server's order). */
const inOrder = <T extends { label: string }>(rows: T[], names: string[]) => {
  const at = (l: string) => { const i = names.indexOf(l); return i < 0 ? names.length : i; };
  return [...rows].sort((a, b) => at(a.label) - at(b.label));
};
/** Prototype LOG time: "Today, 10:42", "Yesterday, 17:10", "23 Oct, 12:30". */
const when = (at: string) => {
  const d = new Date(at), ds = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  const hm = d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false });
  if (ds === today()) return `Today, ${hm}`;
  if (ds === addDays(today(), -1)) return `Yesterday, ${hm}`;
  return `${d.getDate()} ${d.toLocaleDateString('en-GB', { month: 'short' })}${d.getFullYear() !== new Date().getFullYear() ? ' ' + d.getFullYear() : ''}, ${hm}`;
};
const query = (f: F, extra: Record<string, string> = {}) => new URLSearchParams({ ...Object.fromEntries(Object.entries(f).filter(([, v]) => v)), ...extra }).toString();
/** One dashboard tab's figures from the API, for the role view and filters (skipped when `on` is false). */
function useDash(f: F, tab?: string, on = true) {
  const { me } = useAuth();
  const qs = query(f, tab ? { tab } : {});
  return useLoad(() => (on ? api(`/dashboard?${qs}`) : Promise.resolve(null)), [me?.ctx.role, qs, on]);
}

function Overview({ f, order }: { f: F; order: Order }) {
  const { me, can } = useAuth();
  const role = me?.ctx.role || '';
  const { data: d, error } = useDash(f);
  if (!d) return <Loading error={error} />;
  const act = (l: G[], names: string[]) => inOrder(l.filter((x) => x.active > 0), names).map((x) => ({ label: x.label, value: x.active }));
  const byType = TYPES.map((t) => ({ label: t, value: (d.byType as G[]).find((x) => x.label === t)?.active ?? 0 }));
  const pr = d.payroll as { ym: string; status: string } | undefined;
  return <>
    <div className="grid g4">
      <Card l="Active headcount" n={d.active} s={`${d.inactive} inactive · planned for 5,000`} />
      <Card l="Joined this month" n={d.joinedMonth} s={`${d.preboarding} pre-boarding`} />
      <Card l="Left this month" n={d.leftMonth} s={`${d.onNotice} on notice`} />
      <Card l="Attrition, last 12 months" n={`${d.attrition.toFixed(1)}%`} s="exits over average headcount" />
    </div>
    <div className="grid g4" style={{ marginTop: 14 }}>
      <Card l="Requests awaiting approval" n={d.pendingAll} s={`${d.pendingMine} waiting on ${role}`} />
      <Card l="Cities · clusters" n={`${d.cities} · ${d.clusters}`} s={`${d.regions} regions under one corporate`} />
      <Card l={pr ? `Payroll, ${ymLabel(pr.ym)}` : 'Payroll'} n={pr?.status ?? '—'} s={can('pay.run') ? 'Open Payroll to continue' : 'Visible to Payroll roles'} big />
      <Card l="Average tenure" n={d.active ? `${d.avgTenure.toFixed(1)} years` : '—'} s="active employees" />
    </div>
    <div className="grid g3" style={{ marginTop: 14 }}>
      <div className="card"><h3>Headcount by city</h3><Bars rows={act(d.byCity, order.city)} /></div>
      <div className="card"><h3>By employment type</h3><Bars rows={byType} /><h3 style={{ marginTop: 18 }}>By department</h3><Bars rows={act(d.byDept, order.dept)} /></div>
      <div className="card"><h3>Needs attention</h3>
        {d.alerts.length ? <ul className="log">{d.alerts.map((a: any) => <li key={a.title} style={{ display: 'flex', justifyContent: 'space-between', gap: 8, alignItems: 'center' }}>
          <div><b>{a.title}</b><div className="mute" style={{ fontSize: 12.5 }}>{a.detail}</div></div><Go to={a.page}>Open</Go></li>)}</ul>
          : <p className="mute" style={{ margin: 0 }}>Nothing needs attention.</p>}
        <h3 style={{ marginTop: 18 }}>Recent activity</h3>
        <ul className="log">{(d.recent as any[]).slice(0, 5).map((x, i) => <li key={i}><div>{x.action}</div><div className="mute" style={{ fontSize: 12 }}>{when(x.at)}{x.actor ? ` · ${x.actor}` : ''}</div></li>)}</ul>
      </div>
    </div>
  </>;
}

function Workforce({ f }: { f: F }) {
  const { data: d, error } = useDash(f, 'wf');
  if (!d) return <Loading error={error} />;
  const t = d.trend as { label: string; joined: number; left: number; headcount: number }[];
  return <>
    <div className="grid g3">
      <div className="card"><h3>Joiners, last 6 months</h3><Bars rows={t.map((m) => ({ label: m.label, value: m.joined }))} /></div>
      <div className="card"><h3>Exits, last 6 months</h3><Bars rows={t.map((m) => ({ label: m.label, value: m.left }))} /></div>
      <div className="card"><h3>Headcount at month end</h3><Bars rows={t.map((m) => ({ label: m.label, value: m.headcount }))} /></div>
    </div>
    <div className="grid g3" style={{ marginTop: 14 }}>
      <div className="card"><h3>By grade</h3><Bars rows={bars(d.byGrade)} /></div>
      <div className="card"><h3>By gender</h3><Bars rows={bars(d.byGender)} /><h3 style={{ marginTop: 18 }}>By tenure</h3>
        <Bars rows={['Under 1 year', '1 to 2 years', '2 to 3 years', '3 years and more'].map((k) => ({ label: k, value: (d.byTenure as B[]).find((x) => x.label === k)?.n ?? 0 }))} /></div>
      <div className="card"><h3>Largest locations</h3><Bars rows={bars(d.byLocation)} />
        <h3 style={{ marginTop: 18 }}>This month</h3>
        <div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.7 }}>{s(d.birthdays, 'birthday')}<br />{d.anniversaries} work anniversar{d.anniversaries === 1 ? 'y' : 'ies'}</div>
        <div style={{ marginTop: 10 }}><Go to="/reports">Open reports</Go></div>
      </div>
    </div>
  </>;
}

/** Payroll tab: the open payroll month as it stands (prototype dsPayroll), totals computed on the server. */
function PayrollTab({ f, order }: { f: F; order: Order }) {
  const { data: d, error } = useDash(f, 'pay');
  if (!d?.open) return <Loading error={error} />;
  const o = d.open, pend = d.pendingItems as number;
  return <>
    <div className="grid g4">
      <Card l={`People in ${ymLabel(o.ym)}`} n={o.people} s={o.status} />
      <Card l="Gross earnings" n={inr(o.earned)} s="before deductions" />
      <Card l="Net pay" n={inr(o.net)} s="to be paid out" />
      <Card l="Cost to company" n={inr(o.ctc)} s="with employer PF, ESI and insurance" />
    </div>
    <div className="grid g3" style={{ marginTop: 14 }}>
      <div className="card"><h3>Gross by city (₹ thousand)</h3><Bars rows={inOrder(o.byCity as { label: string; value: number }[], order.city)} /></div>
      <div className="card"><h3>Gross by type (₹ thousand)</h3><Bars rows={o.byType} /></div>
      <div className="card"><h3>Before you run payroll</h3>
        <div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.8 }}>{o.holds} person{o.holds === 1 ? '' : 's'} on hold for a bank change<br />{s(pend, 'pay line')} waiting for approval<br />
          {o.missing} with missing punches<br />{o.carry} with a negative net carried forward</div>
        <div className="row" style={{ gap: 8, marginTop: 12 }}><Go to="/payroll">Open payroll</Go><Go to="/reports">Payroll reports</Go></div>
      </div>
    </div>
  </>;
}

/** Attendance tab: this month so far for the people in view (prototype dsAttendance), computed on the server with the filters. */
function AttendanceTab({ f }: { f: F }) {
  const { data: d, error } = useDash(f, 'att');
  if (!d) return <Loading error={error} />;
  const c = d.counts as Record<string, number>, days = d.workDays as number, rl = d.roster, op = d.open;
  return <>
    <div className="grid g4">
      <Card l="On time" n={pctS(c.Present, days)} s="of working days this month" />
      <Card l="Late arrivals" n={c.Late} s={`${pctS(c.Late, days)} of working days`} />
      <Card l="Missing punches" n={c['Missing punch'] + c['No punch']} s="need regularization or loss of pay" />
      <Card l="On approved leave today" n={d.onLeave} s="in this view" />
    </div>
    <div className="grid g3" style={{ marginTop: 14 }}>
      <div className="card"><h3>Late share by location (%)</h3><Bars rows={d.lateByLocation} /></div>
      <div className="card"><h3>Roster changes this month</h3>
        {rl ? <><div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.8 }}>{s(rl.changes, 'change')}, {rl.late} made late ({pctS(rl.late, rl.changes)})</div>
          {rl.alerts.length ? <div className="note bad" style={{ marginTop: 10 }}>{rl.alerts.map((x: any, i: number) => <span key={x.key}>{i > 0 && <br />}{x.key}: {x.pct}% late (limit {x.limit ?? 30}%)</span>)}</div>
            : <div className="note ok" style={{ marginTop: 10 }}>Every location is within its limit.</div>}</>
          : <p className="mute" style={{ margin: 0 }}>Roster changes are visible to roster roles only.</p>}
      </div>
      <div className="card"><h3>Open items</h3>
        <div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.8 }}>{op.regularize} regularizations pending<br />{op.leave} leave requests pending<br />{op.swap} shift swaps pending</div>
        <div className="row" style={{ gap: 8, marginTop: 12 }}><Go to="/roster">Roster</Go><Go to="/attendance">Attendance</Go></div>
      </div>
    </div>
  </>;
}

function Compliance({ f }: { f: F }) {
  const { me, can } = useAuth();
  const { data: d, error } = useDash(f, 'cmp');
  // Statutory files made so far (GET /api/statutory/history, payroll roles); without it, say where they are made.
  const made = useLoad(() => (can('pay.run') ? api('/statutory/history').then((x: any) => (Array.isArray(x) ? x : Array.isArray(x?.files) ? x.files : null)).catch(() => null) : Promise.resolve(null)), [me?.ctx.role]);
  if (!d) return <Loading error={error} />;
  const files = made.data as any[] | null;
  return <>
    <div className="grid g4">
      <Card l="PAN pending" n={d.pan} s="active people" />
      <Card l="Aadhaar pending" n={d.aadhaar} s="active people" />
      <Card l="Bank pending" n={d.bank} s="salary cannot be paid" />
      <Card l="UAN missing" n={d.uan} s="PF applies, no UAN yet" />
    </div>
    <div className="grid g3" style={{ marginTop: 14 }}>
      <div className="card"><h3>Statutory enrolment</h3><div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.8 }}>
        {d.esi} person{d.esi === 1 ? '' : 's'} with ESI applicable and no IP number<br />{s(d.joinersWeek, 'joiner')} in the last 7 days for insurance<br />{d.preboarding} pre-boarding, not yet enrolled</div>
        <div className="row" style={{ gap: 8, marginTop: 12 }}>{can('pay.run') && <Go to="/payroll">Statutory files</Go>}<Go to="/reports">Missing details report</Go></div></div>
      <div className="card"><h3>Master data</h3><div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.8 }}>
        {s(d.staleMasters, 'inactive master')} still used by active employees<br />{d.dob} people without a date of birth</div></div>
      <div className="card"><h3>Files created</h3>
        {!files ? <p className="mute" style={{ margin: 0 }}>Statutory files are made in Payroll.</p>
          : files.length ? <ul className="log">{files.slice(0, 4).map((g: any, i: number) => <li key={g.id ?? i}><div>{g.name ?? g.file}</div><div className="mute" style={{ fontSize: 12 }}>{g.period} · {Array.isArray(g.rows) ? g.rows.length : g.rows} rows · {g.at}</div></li>)}</ul>
          : <p className="mute" style={{ margin: 0 }}>No statutory file created yet.</p>}</div>
    </div>
  </>;
}

function ApprovalsTab({ f }: { f: F }) {
  const { me } = useAuth();
  const { data: d, error } = useDash(f, 'ap');
  if (!d) return <Loading error={error} />;
  return <>
    <div className="grid g4">
      <Card l="Pending" n={d.pending} s={`${d.mine} waiting on ${me?.ctx.role}`} />
      <Card l="Approved" n={d.approved} s="all time in this view" />
      <Card l="Rejected" n={d.rejected} s="all time in this view" />
      <Card l="Approval rate" n={`${d.rate}%`} s="of closed requests" />
    </div>
    <div className="grid g2" style={{ marginTop: 14 }}>
      <div className="card"><h3>Pending by workflow</h3>{d.byWorkflow.length ? <Bars rows={bars(d.byWorkflow)} /> : <p className="mute" style={{ margin: 0 }}>Nothing pending.</p>}</div>
      <div className="card"><h3>Waiting on</h3>{d.byStage.length ? <Bars rows={bars(d.byStage)} /> : <p className="mute" style={{ margin: 0 }}>Nothing pending.</p>}
        <div style={{ marginTop: 14 }}><Go to="/approvals">Open approvals</Go></div></div>
    </div>
  </>;
}

// ---------- My view: the employee's own dashboard (GET /api/me/dashboard) ----------

const MY_TABS: [Tab, string][] = [['ov', 'Overview'], ['pay', 'Payroll'], ['att', 'Attendance'], ['cmp', 'Compliance']];
const useMy = (tab: string) => { const { me } = useAuth(); return useLoad(() => api(`/me/dashboard?tab=${tab}`), [me?.ctx.employeeId, tab]); };
/** "20 Nov": a day and month only, never a year. */
const dm = (d: string) => new Date(+d.slice(0, 4), +d.slice(5, 7) - 1, +d.slice(8, 10)).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' });
const wkd = (d: string) => new Date(+d.slice(0, 4), +d.slice(5, 7) - 1, +d.slice(8, 10)).toLocaleDateString('en-GB', { weekday: 'short' });

function MyDashboard({ tab, setTab }: { tab: Tab; setTab: (t: Tab) => void }) {
  const acks = useMyPolicyAckCount(); // company policies still to acknowledge (PolicyAck.tsx)
  const t: Tab = MY_TABS.some(([k]) => k === tab) ? tab : 'ov';
  return <>
    <div className="row sp" style={{ marginBottom: 14, flexWrap: 'wrap', gap: 10 }}>
      <div className="tabs" style={{ margin: 0 }}>{MY_TABS.map(([k, l]) => <button key={k} className={`tab ${t === k ? 'on' : ''}`} onClick={() => setTab(k)}>{l}{k === 'cmp' && acks > 0 && <> <Pill k="bad">{acks}</Pill></>}</button>)}</div>
    </div>
    {t === 'ov' && <MyOverview acks={acks} onCmp={() => setTab('cmp')} />}
    {t === 'pay' && <MyPayTab />}
    {t === 'att' && <MyAttTab />}
    {t === 'cmp' && <MyCompliance />}
  </>;
}

type Cel = { kind: string; name: string; designation: string; location: string; department: string; date: string; years?: number; thisWeek: boolean };
const CelList = ({ l }: { l: Cel[] }) => <ul className="log">{l.map((x, i) => <li key={i} style={{ display: 'flex', justifyContent: 'space-between', gap: 10, alignItems: 'center' }}>
  <div style={{ minWidth: 0 }}><b>{x.name}</b><div className="mute" style={{ fontSize: 12.5 }}>{x.designation} · {x.location}</div></div>
  <div style={{ textAlign: 'right', whiteSpace: 'nowrap' }}><div>{wkd(x.date)}, {dm(x.date)}</div>
    {x.kind === 'Birthday' ? <Pill k="info">Birthday</Pill> : <Pill k="ok">{x.years} year{x.years === 1 ? '' : 's'}</Pill>}</div></li>)}</ul>;

function MyOverview({ acks, onCmp }: { acks: number; onCmp: () => void }) {
  const nav = useNavigate();
  const { data: d, error, reload } = useMy('ov');
  if (!d) return <Loading error={error} />;
  const pr = d.payroll as { ym: string; status: string } | undefined;
  const cel = d.celebrations as Cel[], wk = cel.filter((x) => x.thisWeek), later = cel.filter((x) => !x.thisWeek);
  const alerts = d.alerts as { title: string; detail: string; page: string }[];
  return <>
    <div className="grid g4">
      <Card l="My requests waiting" n={d.pending} s="for approval · see My requests" go={() => nav('/#my-requests')} />
      <Card l="Present this month" n={d.present ?? '—'} s={`of ${s(d.workDays ?? 0, 'day')} so far`} />
      <Card l={pr ? `Payroll, ${ymLabel(pr.ym)}` : 'Payroll'} n={pr?.status ?? '—'} s="Your payslip is in My pay once published" big />
      <Card l="With Saltstayz" n={`${d.tenure.toFixed(1)} years`} s={`since ${fmtD(d.doj)}`} />
    </div>
    <MyDocsCard />
    <MyRequests onChange={reload} />
    <div className="grid g2" style={{ marginTop: 14 }}>
      <div className="card"><h3>Needs attention</h3>
        {alerts.length || acks > 0 ? <ul className="log">
          {acks > 0 && <li style={{ display: 'flex', justifyContent: 'space-between', gap: 8, alignItems: 'center' }}><div><b>Company policies</b><div className="mute" style={{ fontSize: 12.5 }}>{s(acks, 'policy', 'policies')} to acknowledge. Your salary will be held until they are approved.</div></div><button className="btn" onClick={onCmp}>Open</button></li>}
          {alerts.map((a) => <li key={a.title + a.detail} style={{ display: 'flex', justifyContent: 'space-between', gap: 8, alignItems: 'center' }}>
            <div><b>{a.title}</b><div className="mute" style={{ fontSize: 12.5 }}>{a.detail}</div></div><Go to={a.page}>Open</Go></li>)}</ul>
          : <p className="mute" style={{ margin: 0 }}>Nothing needs attention.</p>}
        <h3 style={{ marginTop: 18 }}>Recent activity</h3>
        {(d.recent as any[]).length ? <ul className="log">{(d.recent as any[]).slice(0, 5).map((x, i) => <li key={i}><div>{x.action}</div><div className="mute" style={{ fontSize: 12 }}>{when(x.at)}{x.actor ? ` · ${x.actor}` : ''}</div></li>)}</ul>
          : <p className="mute" style={{ margin: 0 }}>No recent activity on your record.</p>}
      </div>
      <div className="card"><h3>Birthdays and work anniversaries</h3>
        <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>Colleagues in your department and at your location.</p>
        <h3 style={{ marginTop: 10, fontSize: 13 }}>This week</h3>
        {wk.length ? <CelList l={wk} /> : <p className="mute" style={{ margin: 0, fontSize: 13 }}>None this week.</p>}
        <h3 style={{ marginTop: 16, fontSize: 13 }}>Next 30 days</h3>
        {later.length ? <CelList l={later} /> : <p className="mute" style={{ margin: 0, fontSize: 13 }}>None in the next 30 days.</p>}
      </div>
    </div>
  </>;
}

// ---------- My view › Overview › My requests ----------
// Approvals is not in My view: each request starts from its own page and is followed here (GET /api/me/requests?wf=all).

type RF = 'open' | 'done' | 'all';
/** Where each request an employee raises starts in My view (the old Approvals › New request, and the rest). */
const RAISE: [string, string][] = [['leave', '/me?tab=leave'], ['regularize', '/me?tab=att'], ['p_claim', '/me?tab=claims'], ['p_adv', '/me?tab=adv'], ['p_change', '/me?tab=me'], ['resign', '/my-exit']];
/** How long a request has waited at its stage: "3 hours", "2 days". */
const waited = (at?: string) => { if (!at) return ''; const h = Math.max(0, (Date.now() - new Date(at).getTime()) / 36e5); return h < 1 ? 'under an hour' : h < 48 ? s(Math.floor(h), 'hour') : s(Math.floor(h / 24), 'day'); };
/** What a request is about, in a few words: the dates of time off, the amount of money, the change asked for. */
function about(r: any): string {
  const p = r.payload || {}, f = r.facts || {};
  switch (r.wf) {
    case 'leave': { const d = f.days ?? (p.half ? 0.5 : 1); return `${p.ltype} · ${fmtD(p.from)}${p.to && p.to !== p.from ? ` to ${fmtD(p.to)}` : ''} · ${d === 0.5 ? 'half a day' : s(d, 'day')}`; }
    case 'regularize': return `${fmtD(p.date)} · ${p.rtype === 'No Punch' ? 'missing punch' : 'time correction'} · ${p.in || '—'} to ${p.out || '—'}`;
    case 'resign': {
      const k = p.exitType && p.exitType !== 'Resignation' ? p.exitType : p.rkind === 'withdraw' ? 'Withdraw resignation' : p.rkind === 'change' ? 'Change last working day' : 'Resignation';
      return `${k}${p.lwd ? ` · last working day ${fmtD(p.lwd)}` : ''}`;
    }
    case 'transfer': return `Transfer${p.effective ? `, effective ${fmtD(p.effective)}` : ''}`;
  }
  return subOf(r);
}

/** The employee's own requests: status, approval progress, who each waits on, Cancel for their own pending ones,
 *  and the request's detail and history (the Approvals detail, read-only). */
function MyRequests({ onChange }: { onChange: () => void }) {
  const { me } = useAuth();
  const nav = useNavigate();
  const loc = useLocation();
  const ref = useRef<HTMLDivElement>(null);
  const { data, error, reload } = useLoad(() => api<any[]>('/me/requests?wf=all'), [me?.ctx.employeeId]);
  const lv = useLoad(() => api<any>('/leave/balances/me').catch(() => null), [me?.ctx.employeeId]);
  const [f, setF] = useState<RF | ''>('');
  const [sel, setSel] = useState<any>(null);
  // "My requests waiting" and the Needs attention item link here (/#my-requests).
  useEffect(() => { if (loc.hash === '#my-requests' && data) ref.current?.scrollIntoView({ behavior: 'smooth', block: 'start' }); }, [loc.hash, loc.key, data]);
  const all = data ?? [];
  const open = all.filter((r) => r.status === 'Pending');
  const fl: RF = f || (open.length ? 'open' : 'all');
  const rows = fl === 'open' ? open : fl === 'done' ? all.filter((r) => r.status !== 'Pending') : all;
  const done = () => { reload(); onChange(); };
  const cancel = async (r: any) => {
    try { await api(`/approvals/${r.id}/cancel`, { method: 'POST' }); toast(r.wf === 'leave' ? 'Request cancelled. The days are back in your balance' : 'Request cancelled'); done(); } catch (x: any) { toast(x.message); }
  };
  // New requests start from their own page; only the ones this person can raise are offered.
  const active = me?.employee.status !== 'Inactive';
  const canRaise = (k: string) => k === 'p_change' || (active && (k !== 'p_claim' || me?.claimsOn !== false) && (k !== 'p_adv' || me?.employee.type === 'Permanent') && (k !== 'leave' || ((lv.data?.types ?? []) as any[]).some((t) => t.applyBy !== 'hr')));
  const seg: [RF, string, number][] = [['open', 'Waiting', open.length], ['done', 'Decided', all.length - open.length], ['all', 'All', all.length]];
  return <div className="card" style={{ marginTop: 14, scrollMarginTop: 12 }} id="my-requests" ref={ref}>
    <div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap', gap: 8 }}>
      <div><h3 style={{ margin: 0 }}>My requests</h3><div className="mute" style={{ fontSize: 12.5 }}>Everything raised by you or for you, with who it is waiting on. You can cancel your own until they are approved.</div></div>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
        <div className="seg">{seg.map(([k, l, n]) => <button key={k} className={fl === k ? 'on' : ''} onClick={() => setF(k)}>{l}<b>{n}</b></button>)}</div>
        <select aria-label="New request" value="" onChange={(e) => e.target.value && nav(e.target.value)}><option value="">New request…</option>
          {RAISE.filter(([k]) => canRaise(k)).map(([k, to]) => <option key={k} value={to}>{WORKFLOWS[k]}</option>)}</select>
      </div>
    </div>
    {/* One line per request on a wide screen; the three blocks stack on a phone (flex wrap), so nothing scrolls sideways. */}
    {!data ? <Loading error={error} /> : <div>{rows.map((r) => {
      const pend = r.status === 'Pending', other = r.raised_by !== me?.ctx.employeeId;
      return <div key={r.id} className="mreq" style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '8px 18px', padding: '12px 0', borderTop: '1px solid var(--line)' }}>
        <div style={{ flex: '3 1 260px', minWidth: 0 }}>
          <div><b>{wfName(r.wf)}</b> <span className="mono mute" style={{ fontSize: 12 }}>{r.id}</span></div>
          <div className="ink2" style={{ fontSize: 13 }}>{about(r)}</div>
          <div className="mute" style={{ fontSize: 12 }}>Raised {fmtD(String(r.created_at))}{other ? ` by ${r.raised_by_name}` : ''}</div>
        </div>
        <div style={{ flex: '3 1 260px', minWidth: 0 }}>
          {pend && <div style={{ fontSize: 13, marginBottom: 6 }}>Waiting on <b>{r.waiting_name || r.waiting_on}</b>{r.waiting_name ? ` (${r.waiting_on})` : ''}<span className="mute"> · {waited(r.stage_since)}</span></div>}
          {(r.chain ?? []).length ? <Chain steps={r.chain} step={r.step} status={r.status} /> : <span className="mute" style={{ fontSize: 12.5 }}>No approver needed</span>}
        </div>
        <div className="row" style={{ flex: '0 0 auto', minWidth: 200, justifyContent: 'flex-end', gap: 6, flexWrap: 'nowrap', marginLeft: 'auto' }}>
          <Pill k={statusTone(r.status)}>{r.status}</Pill>
          <button className="btn sm" onClick={() => setSel(r)}>Open</button>
          {pend && r.canCancel && <button className="btn sm" onClick={() => cancel(r)}>Cancel</button>}
        </div>
      </div>;
    })}
      {!rows.length && <p className="mute" style={{ margin: 0, padding: '12px 0', borderTop: '1px solid var(--line)' }}>{all.length ? (fl === 'open' ? 'Nothing is waiting for approval.' : 'No decided requests yet.') : 'No requests yet. Pick one under New request to start it from its own page.'}</p>}
    </div>}
    {sel && <Modal wide onClose={() => setSel(null)}>
      <div className="row sp" style={{ marginBottom: 10 }}><h2 style={{ margin: 0 }}>{wfName(sel.wf)} <span className="mono mute" style={{ fontSize: 13 }}>{sel.id}</span></h2><button className="btn sm" onClick={() => setSel(null)}>Close</button></div>
      <Detail id={sel.id} row={sel} readOnly onChange={done} />
    </Modal>}
  </div>;
}

/** My view › Payroll: only the employee's own pay. */
function MyPayTab() {
  const { me } = useAuth();
  const { data: d, error } = useMy('pay');
  if (!d) return <Loading error={error} />;
  const l = d.last, fy = d.fy, nx = d.next, items = d.items as any[], holds = d.holds as any[];
  return <>
    {holds.length > 0 && <div className="note bad" style={{ marginBottom: 14 }}>{holds.map((h, i) => <span key={i}>{i > 0 && <br />}{h.to_ym ? `Your salary for ${h.to_ym === h.from_ym ? ymLabel(h.from_ym) : `${ymLabel(h.from_ym)} to ${ymLabel(h.to_ym)}`} is on hold` : `Your salary is on hold from ${ymLabel(h.from_ym)}`}: {h.reason}</span>)}</div>}
    <div className="grid g4">
      <Card l="Last net pay" n={l ? inr(l.net) : '—'} s={l ? `${l.label}${l.held ? ' · held' : ''}` : 'No salary paid yet'} />
      <Card l={`Earnings, ${fy.label}`} n={inr(fy.earned)} s={`${s(fy.months, 'month')} paid, before deductions`} />
      <Card l="TDS deducted" n={inr(fy.tds)} s={`income tax, ${fy.label}`} />
      <Card l="Next pay date" n={fmtD(nx.date)} s={nx.passed ? `${nx.label} pay, once payroll is published` : `${nx.label} pay · ${nx.payDay.toLowerCase()}`} big />
    </div>
    <div className="card" style={{ marginTop: 14 }}><div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap', gap: 8 }}><h3 style={{ margin: 0 }}>My claims and advances</h3>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>{d.payslips && <Go to="/me?tab=slips">My payslips</Go>}<Go to="/me?tab=claims">Reimbursements</Go>
        {me?.employee.type === 'Permanent' && me?.employee.status !== 'Inactive' && <Go to="/me?tab=adv">Request a salary advance</Go>}</div></div>
      <div className="tw"><table><thead><tr><th>Request</th><th>Item</th><th className="num">Amount</th><th>Status</th><th>Paid with</th></tr></thead><tbody>
        {items.map((x) => <tr key={x.id}><td className="mono">{x.id}</td><td>{x.kind}</td><td className="num">{inr(x.amount)}{x.balance > 0 && <div className="mute" style={{ fontSize: 12 }}>{inr(x.balance)} to recover</div>}</td>
          <td>{x.status === 'Pending' ? <><Pill k="warn">Pending</Pill>{(x.waitingName || x.waitingOn) && <div className="mute" style={{ fontSize: 12 }}>with {x.waitingName || x.waitingOn}</div>}</> : <Pill k="ok">{x.status}</Pill>}</td>
          <td>{x.payMonth ? ymLabel(x.payMonth) : '—'}</td></tr>)}
        {!items.length && <tr><td colSpan={5} className="mute">No claims or advances open.</td></tr>}</tbody></table></div></div>
    {d.uniform && <MyUniform u={d.uniform} />}
  </>;
}

/** My view › Payroll › My uniform: the sizes on record, and the uniform issued with what is recovered from salary and when the
 *  refund is expected (uniform.go). Shown once there are sizes or uniform. */
function MyUniform({ u }: { u: any }) {
  const issues = u.issues as UniIssue[], sz = u.sizes as Record<SizeKey, string>;
  if (!issues.length && SIZE_KEYS.every((k) => !sz[k])) return null;
  const open = issues.filter((x) => x.recover && x.refundStatus === 'scheduled');
  return <div className="card" style={{ marginTop: 14 }} data-testid="my-uniform"><h3>My uniform</h3>
    <p className="ink2" style={{ margin: '0 0 10px', fontSize: 13 }}>Sizes on record: {SIZE_KEYS.map((k) => `${SIZE_LABEL[k]} ${sz[k] ? sizeText(k, sz[k]) : '—'}`).join(' · ')}. To change them, ask your HR.</p>
    {open.length > 0 && <div className="note" style={{ marginBottom: 10, fontSize: 13 }}>{open.map((x) => <div key={x.id}>Uniform of {fmtD(x.date)}: {x.toRecover > 0 ? `${inr(x.toRecover)} still to be recovered from salary. ` : `${inr(x.recovered)} recovered from salary. `}
      Expected refund <b>{inr(x.total)} in {ymLabel(x.refundYm!)}</b>, if you are still employed then.</div>)}</div>}
    <UniformIssues issues={issues} empty="No uniform issued to you yet." />
  </div>;
}

/** My view › Attendance: the employee's own month so far. */
function MyAttTab() {
  const { data: d, error } = useMy('att');
  if (!d) return <Loading error={error} />;
  const miss = d.missing + d.noPunch;
  return <>
    <div className="grid g4">
      <Card l="Days present" n={d.present} s={`${d.label}, ${s(d.upTo, 'day')} so far`} />
      <Card l="Late arrivals" n={d.late} s={d.lateOver ? `${d.lateOver} over the allowed limit` : `${s(d.halfDay, 'half day')}`} />
      <Card l="Missing punches" n={miss} s={d.toFix ? `${s(d.toFix, 'day')} to regularize` : 'nothing to regularize'} />
      <Card l="Leave" n={d.leave} s={`${d.lopLeave} unpaid · ${d.lop} loss of pay days`} />
    </div>
    <div className="grid g2" style={{ marginTop: 14 }}>
      <div className="card"><h3>This month</h3><div className="ink2" style={{ fontSize: 13.5, lineHeight: 1.8 }}>{s(d.weekOff, 'weekly off', 'weekly offs')}<br />{s(d.holiday, 'holiday')}<br />{s(d.pending, 'request')} for leave or regularization waiting for approval</div>
        <div className="mute" style={{ fontSize: 12.5, marginTop: 8 }}>Work policy: {d.policy}</div></div>
      <div className="card"><h3>Fix a day</h3><p className="ink2" style={{ fontSize: 13.5, margin: 0 }}>Regularize a missing punch or apply for leave from My pay and profile.</p>
        <div className="row" style={{ gap: 8, marginTop: 12 }}><Go to="/me?tab=att">My attendance</Go><Go to="/me?tab=leave">My leave</Go></div></div>
    </div>
  </>;
}

/** My view › Compliance: company policies to acknowledge (PolicyAck.tsx) and the employee's own missing details. */
function MyCompliance() {
  const { data: d, error } = useMy('cmp');
  return <>
    <MyPolicyAcks />
    {!d ? <Loading error={error} /> : <div className="card" style={{ marginTop: 14 }}><div className="row sp" style={{ marginBottom: 10, flexWrap: 'wrap', gap: 8 }}><h3 style={{ margin: 0 }}>My details on record</h3><Go to="/me?tab=me">Update my details</Go></div>
      <div className="tw"><table><thead><tr><th>Detail</th><th>Status</th><th>Why it is needed</th></tr></thead><tbody>
        {(d.items as { label: string; ok: boolean; note: string }[]).map((x) => <tr key={x.label}><td><b>{x.label}</b></td><td>{x.ok ? <Pill k="ok">On record</Pill> : <Pill k="bad">Missing</Pill>}</td><td className="mute">{x.note}</td></tr>)}
      </tbody></table></div>
      {d.missing.length > 0 && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Send bank details from My pay and profile › My details. For PAN, Aadhaar, UAN or date of birth, contact HR.</p>}</div>}
  </>;
}
