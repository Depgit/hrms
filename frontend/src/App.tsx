import { NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { useAuth, type Me } from './auth';
import { BusyBar, Toaster, useLoad } from './components/ui';
import { Icon } from './components/icons';
import { ThemeToggle } from './components/ThemeToggle';
import { api } from './api';
import Login from './pages/Login';
import Join from './pages/Join';
import Dashboard from './pages/Dashboard';
import Employees from './pages/Employees';
import Approvals from './pages/Approvals';
import Masters from './pages/Masters';
import Access from './pages/Access';
import Payroll from './pages/Payroll';
import MyPay from './pages/MyPay';
import Onboarding from './pages/Onboarding';
import Bulk from './pages/Bulk';
import Policies from './pages/Policies';
import Roster from './pages/Roster';
import Attendance from './pages/Attendance';
import Compensation from './pages/Compensation';
import EmployeeFile from './pages/EmployeeFile';
import { Dues } from './pages/PayItems';
import Separation from './pages/Separation';
import Reports from './pages/Reports';
import MyExitPage from './pages/MyExit';
import Directory from './pages/Directory';
import MyDocuments from './pages/MyDocuments';
import Accommodation from './pages/Accommodation';
import Cost from './pages/Cost';
import { NoticeBell } from './components/Notices';

function DuesPage() {
  return <Dues />;
}

type Can = (r: string, m?: 1 | 2) => boolean;
const NO_APPROVE = ['Finance', 'MIS Executive'];
interface Page { path: string; label: string; icon: string; show: (can: Can, role: string, me: Me) => boolean; el: JSX.Element }

// Same order and visibility rules as the prototype's navigation.
const PAGES: Page[] = [
  { path: '/', label: 'Dashboard', icon: 'home', show: () => true, el: <Dashboard /> },
  { path: '/employees', label: 'Employees', icon: 'users', show: (can) => can('emp.directory'), el: <Employees /> },
  { path: '/onboarding', label: 'Onboarding', icon: 'userplus', show: (can) => can('onb.pipeline'), el: <Onboarding /> },
  { path: '/separation', label: 'Separation', icon: 'exit', show: (can, _role, me) => can('emp.sep') || (me.ctx.mgrCases ?? 0) > 0, el: <Separation /> },
  { path: '/compensation', label: 'Compensation', icon: 'rupee', show: (can) => can('comp.bands') || can('comp.budget') || can('comp.structure'), el: <Compensation /> },
  // Approvals belong to the role views; in My view each request starts from its own page (leave, attendance, claims…).
  { path: '/approvals', label: 'Approvals', icon: 'check', show: (_, role) => role !== 'Employee', el: <Approvals /> },
  { path: '/roster', label: 'Shift and roster', icon: 'calendar', show: (can) => can('ros.roster'), el: <Roster /> },
  { path: '/attendance', label: 'Attendance', icon: 'clock', show: (can) => can('att.records'), el: <Attendance /> },
  { path: '/file', label: 'Employee file', icon: 'users', show: (can, role) => (can('emp.directory') || can('pay.run')) && role !== 'Employee', el: <EmployeeFile /> },
  { path: '/payroll', label: 'Payroll', icon: 'wallet', show: (can) => can('pay.run'), el: <Payroll /> },
  { path: '/reports', label: 'Reports', icon: 'chart', show: (can) => can('rep.reports'), el: <Reports /> },
  { path: '/policies', label: 'Work policies', icon: 'shield', show: (can) => can('pol.policies'), el: <Policies /> },
  { path: '/masters', label: 'Masters', icon: 'layers', show: (can) => can('mas.org') || can('mas.job'), el: <Masters /> },
  // Staff accommodation: units, leases, check-in and check-out, the month's cost split and the occupancy board (hst.go).
  { path: '/accommodation', label: 'Accommodation', icon: 'bed', show: (can) => can('hst.setup') || can('hst.allot'), el: <Accommodation /> },
  // Cost and budget console: the month's cost by place against the budgets, and the headcount plan (cost.go).
  { path: '/cost', label: 'Cost and budget', icon: 'chart', show: (can) => can('cost.view'), el: <Cost /> },
  { path: '/bulk', label: 'Bulk update', icon: 'upload', show: (can) => can('bulk.update'), el: <Bulk /> },
  { path: '/access', label: 'Access control', icon: 'lock', show: (can) => can('acc.roles') || can('acc.users'), el: <Access /> },
  { path: '/dues', label: 'Employee dues', icon: 'layers', show: (can) => can('emp.assets') || can('pay.claims'), el: <DuesPage /> },
  { path: '/me', label: 'My pay and profile', icon: 'wallet', show: (_, role) => role === 'Employee', el: <MyPay /> },
  // My view only: the employee's own exit (resignation and its progress), and colleagues in their department.
  { path: '/my-exit', label: 'Separation', icon: 'exit', show: (_, role) => role === 'Employee', el: <MyExitPage /> },
  { path: '/directory', label: 'Employee directory', icon: 'users', show: (_, role) => role === 'Employee', el: <Directory /> },
  // My view only: the employee's own documents of Form setup's list, uploaded within their window (doc_collect.go).
  { path: '/my-documents', label: 'My documents', icon: 'file', show: (_, role) => role === 'Employee', el: <MyDocuments /> },
];

export default function App() {
  const { me, loading, can, switchRole, signOut } = useAuth();
  const loc = useLocation();
  // The Approvals badge. My view follows its own requests on the Dashboard; it shows Approvals only while a request
  // waits on the person by name (a Receiving Manager with no role view, ESS-166).
  const pending = useLoad(() => (me ? api<any[]>('/approvals?tab=mine').catch(() => []) : Promise.resolve([])), [me?.ctx.role, loc.pathname]);
  // The new joiner's page (link sent by HR): public, outside the signed-in app, whoever is signed in on this browser.
  if (loc.pathname.startsWith('/join/')) return <><Routes><Route path="/join/:token" element={<Join />} /></Routes><Toaster /></>;
  if (loading) return null;
  if (!me) return <><Login /><Toaster /></>;
  const role = me.ctx.role;
  const namedWaiting = role === 'Employee' && (pending.data?.length ?? 0) > 0;
  const visible = PAGES.filter((p) => p.show(can, role, me) || (p.path === '/approvals' && namedWaiting));
  const current = visible.find((p) => (p.path === '/' ? loc.pathname === '/' : loc.pathname.startsWith(p.path)));
  // Prototype: one "<role> view" per mapped role. A line manager's team approvals come with any other role that can
  // approve (the server lets the named manager act from it), so the Line Manager view shows only when it is the
  // person's sole approving role.
  const other = me.ctx.roles.filter((r) => !['Employee', 'Line Manager'].includes(r));
  const views = me.ctx.roles.filter((r) => r !== 'Employee' && (r !== 'Line Manager' || !other.some((o) => !NO_APPROVE.includes(o)) || role === 'Line Manager'));
  return (
    <div className="app">
      <aside className="side">
        <div className="logo"><i>S</i>Saltstayz HRMS</div>
        {visible.map((p) => (
          <NavLink key={p.path} to={p.path} end={p.path === '/'} className="nav">
            <Icon name={p.icon} />{p.label}
            {p.path === '/approvals' && (pending.data?.length ?? 0) > 0 && <span className="badge">{pending.data!.length}</span>}
          </NavLink>
        ))}
        <div className="sidefoot">Built for 5,000+ employees across cities, clusters and corporate structure.</div>
      </aside>
      <div className="main">
        <header className="top">
          <h1>{current?.label ?? ''}</h1>
          {views.length > 0 && <span className="seg" aria-label="Role view">
            <button className={role === 'Employee' ? 'on' : ''} onClick={() => switchRole('Employee')}>My view</button>
            {views.map((r) => <button key={r} className={r === role ? 'on' : ''} onClick={() => switchRole(r)}>{r} view</button>)}
          </span>}
          <span style={{ fontSize: 13, display: 'inline-flex', alignItems: 'center', gap: 8 }}><b>{me.employee.name}</b> · {me.ctx.employeeId} <button className="btn sm" onClick={signOut}>Sign out</button></span>
          <NoticeBell />
          <ThemeToggle />
        </header>
        <Routes>
          {visible.map((p) => <Route key={p.path} path={p.path === '/' ? '/' : p.path + '/*'} element={p.el} />)}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </div>
      <Toaster />
      <BusyBar />
    </div>
  );
}
