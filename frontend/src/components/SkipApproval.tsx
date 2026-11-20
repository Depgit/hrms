import { useEffect, useState } from 'react';
import { api } from '../api';
import { toast } from './ui';

/** HR's "Skip approval" (19 Nov 2026): offered only when the request type's flow has "Approval hierarchy required"
 *  unticked and the viewer is an HR role who may skip (the server decides: /approvals/skip-check, approvals.go). */

/** Asks for the reason to skip approval; null when cancelled or left blank. */
export function askSkipReason(): string | null {
  const r = window.prompt('Skip approval: why does this not need the approvers? The reason is kept in the history and the audit log.');
  if (r === null) return null;
  if (!r.trim()) { toast('Give a reason to skip approval'); return null; }
  return r.trim();
}

/** The query that raises a request with approval skipped. */
export const skipQuery = (reason: string) => `?skipApproval=${encodeURIComponent(reason)}`;

/** Whether to offer "Skip approval" when raising a request of type wf about subject (employee, candidate or offer). */
export function useCanSkip(wf: string, subject?: string, leaveGroup?: string): boolean {
  const [ok, setOk] = useState(false);
  useEffect(() => {
    let live = true;
    setOk(false);
    if (!wf) return;
    const q = new URLSearchParams({ wf, ...(subject ? { subject } : {}), ...(leaveGroup ? { leaveGroup } : {}) });
    api<{ skip: boolean }>(`/approvals/skip-check?${q}`).then((x) => live && setOk(!!x.skip)).catch(() => {});
    return () => { live = false; };
  }, [wf, subject, leaveGroup]);
  return ok;
}

/** "Skip approval" next to "Send for approval": asks the reason, then onSkip gets the query to raise it with. */
export function SkipApprovalButton({ wf, subject, leaveGroup, disabled, onSkip }: { wf: string; subject?: string; leaveGroup?: string; disabled?: boolean; onSkip: (query: string) => void }) {
  const ok = useCanSkip(wf, subject, leaveGroup);
  if (!ok) return null;
  return <button className="btn" disabled={disabled} title="Approve at once without the approvers (HR, with a reason)" onClick={() => { const r = askSkipReason(); if (r) onSkip(skipQuery(r)); }}>Skip approval</button>;
}

/** "Skip approval" on a request already waiting (the server's canSkip). */
export function SkipWaitingButton({ id, onDone }: { id: string; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const go = async () => {
    const reason = askSkipReason();
    if (!reason) return;
    setBusy(true);
    try { await api(`/approvals/${id}/skip`, { body: { reason } }); toast('Approval skipped. The request is approved'); onDone(); } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  return <button className="btn sm" disabled={busy} onClick={go}>Skip approval</button>;
}
