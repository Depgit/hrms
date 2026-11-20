import { useEffect, useRef, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { api } from '../api';
import { useAuth } from '../auth';
import { Pill, toast } from './ui';

/** In-app messages (e.g. Payroll asking someone to raise or approve something before the payroll run).
 *  NoticeBell sits in the header; NoticeStrip shows open messages at the top of the Dashboard. Both read
 *  GET /api/notices (the person's own messages, in any view) and share what they loaded, so a message
 *  opened or dismissed in one shows in the other. */

type Notice = { id: number; kind: string; text: string; link: string; view: string; from_name: string; at: string; read: boolean; done: boolean; dismissed: boolean };
type List = { notices: Notice[]; unread: number; open: number };

const subs = new Set<(l: List) => void>();
let last: List | null = null;
async function refresh() {
  try { last = await api<List>('/notices'); subs.forEach((f) => f(last!)); } catch { /* signed out, or offline: keep what is shown */ }
}
function useNotices() {
  const [l, setL] = useState<List | null>(last);
  useEffect(() => { subs.add(setL); return () => { subs.delete(setL); }; }, []);
  return l;
}

/** Opens a message: marks it read, switches to the role view it belongs to (an approver's, or My view), then goes to its page. */
function useOpen() {
  const { me, switchRole } = useAuth();
  const nav = useNavigate();
  return async (n: Notice) => {
    if (!n.read) await api(`/notices/${n.id}/read`, { method: 'POST' }).catch(() => undefined);
    if (n.link) {
      if (n.view && me && n.view !== me.ctx.role && me.ctx.roles.includes(n.view)) await switchRole(n.view);
      nav(n.link);
    }
    refresh();
  };
}

const dismiss = async (n: Notice) => {
  try { await api(`/notices/${n.id}/dismiss`, { method: 'POST' }); refresh(); } catch (e: any) { toast(e.message); }
};

const Bell = () => <svg viewBox="0 0 24 24" aria-hidden="true" style={{ width: 16, height: 16, stroke: 'currentColor', fill: 'none', strokeWidth: 1.8, strokeLinecap: 'round', strokeLinejoin: 'round' }}>
  <path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9M13.7 21a2 2 0 0 1-3.4 0" /></svg>;

export function NoticeBell() {
  const l = useNotices();
  const loc = useLocation();
  const { me } = useAuth();
  const go = useOpen();
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState({ top: 0, right: 16, width: 380 });
  const box = useRef<HTMLSpanElement>(null);
  useEffect(() => { refresh(); }, [loc.pathname, me?.ctx.role]);
  useEffect(() => { const t = setInterval(refresh, 60000); return () => clearInterval(t); }, []);
  useEffect(() => {
    if (!open) return;
    const click = (e: MouseEvent) => { if (box.current && !box.current.contains(e.target as Node)) setOpen(false); };
    const key = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', click);
    window.addEventListener('keydown', key);
    return () => { document.removeEventListener('mousedown', click); window.removeEventListener('keydown', key); };
  }, [open]);
  const toggle = () => {
    if (!open && box.current) {
      // The list hangs below the bell, inside the window at any width (the header wraps on a phone).
      const r = box.current.getBoundingClientRect(), vw = window.innerWidth, width = Math.min(380, vw - 32);
      setPos({ top: r.bottom + 6, width, right: Math.max(16, Math.min(vw - r.right, vw - width - 16)) });
      refresh();
    }
    setOpen(!open);
  };
  const n = l?.unread ?? 0, list = l?.notices ?? [];
  const readAll = async () => { await api('/notices/read-all', { method: 'POST' }).catch(() => undefined); refresh(); };
  return (
    <span ref={box} style={{ display: 'inline-flex' }}>
      <button className="btn sm" onClick={toggle} aria-expanded={open} aria-haspopup="dialog" aria-label={n ? `Messages, ${n} unread` : 'Messages'} title="Messages"
        style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
        <Bell />{n > 0 && <span style={{ background: 'var(--accent)', color: 'var(--brand-ink)', borderRadius: 99, fontSize: 11, padding: '1px 7px', fontWeight: 700, lineHeight: 1.4 }}>{n}</span>}
      </button>
      {open && <div role="dialog" aria-label="Messages" style={{ position: 'fixed', top: pos.top, right: pos.right, width: pos.width, zIndex: 60, background: 'var(--surface)', color: 'var(--ink)',
        border: '1px solid var(--line)', borderRadius: 'var(--r)', boxShadow: '0 8px 30px rgba(0,0,0,.14)', padding: '12px 14px' }}>
        <div className="row sp" style={{ marginBottom: 4 }}><b>Messages</b>{n > 0 && <button className="btn sm" onClick={readAll}>Mark all read</button>}</div>
        {list.length ? <ul className="log" style={{ maxHeight: 'min(420px, 60vh)', overflow: 'auto' }}>
          {list.map((x) => {
            const live = !x.done && !x.dismissed;
            return <li key={x.id} style={{ display: 'flex', gap: 10, alignItems: 'flex-start' }}>
              <span aria-hidden="true" style={{ width: 7, height: 7, borderRadius: 99, marginTop: 6, flex: 'none', background: live && !x.read ? 'var(--accent)' : 'transparent' }} />
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontWeight: live && !x.read ? 700 : 400, color: live ? 'var(--ink)' : 'var(--ink2)' }}>{x.text}</div>
                <div className="mute" style={{ fontSize: 12, marginTop: 2 }}>{x.from_name ? `From ${x.from_name} · ` : ''}{x.at} {x.done ? <Pill k="ok">Done</Pill> : x.dismissed ? <Pill>Dismissed</Pill> : null}</div>
              </div>
              {x.link && <button className="btn sm" onClick={() => { setOpen(false); go(x); }}>Open</button>}
            </li>;
          })}
        </ul> : <p className="mute" style={{ margin: '6px 0 0', fontSize: 13 }}>No messages.</p>}
      </div>}
    </span>
  );
}

export function NoticeStrip() {
  const l = useNotices();
  const go = useOpen();
  useEffect(() => { refresh(); }, []);
  const open = (l?.notices ?? []).filter((n) => !n.done && !n.dismissed);
  if (!open.length) return null;
  return (
    <div className="note" role="region" aria-label="Messages for you" style={{ marginBottom: 14, fontSize: 13 }}>
      <b>Messages for you</b>
      <ul className="log" style={{ marginTop: 4 }}>
        {open.map((n) => <li key={n.id} style={{ display: 'flex', justifyContent: 'space-between', gap: 10, alignItems: 'center', flexWrap: 'wrap', borderColor: 'var(--line)' }}>
          <div style={{ flex: '1 1 260px', minWidth: 0 }}>
            <div style={{ fontWeight: n.read ? 400 : 700 }}>{n.text}</div>
            <div className="mute" style={{ fontSize: 12, marginTop: 2 }}>{n.from_name ? `From ${n.from_name} · ` : ''}{n.at}</div>
          </div>
          <div className="row" style={{ gap: 6 }}>
            {n.link && <button className="btn sm primary" onClick={() => go(n)}>Open</button>}
            <button className="btn sm" onClick={() => dismiss(n)}>Dismiss</button>
          </div>
        </li>)}
      </ul>
    </div>
  );
}
