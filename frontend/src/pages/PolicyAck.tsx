import { useEffect, useState } from 'react';
import { api, fmtD, openDoc, uploadDoc } from '../api';
import { useAuth } from '../auth';
import { Loading, Pill, toast, useLoad } from '../components/ui';

// Company policy acknowledgement, the employee's side (GET /api/me/policies). Each policy that applies to the
// person: download it, print and sign it, upload the signed copy. The copy goes to the reporting manager, who
// approves it or returns it with a comment. While anything is pending the salary for the month is held.

export interface MyAck {
  policy_id: string; title: string; version_id: number; version: string; effective_from: string; policy_doc: string;
  status: 'Signed' | 'Waiting for manager' | 'Returned' | 'Not submitted';
  signed_doc?: string | null; submitted_at?: string | null; request_id?: string | null;
  comment?: string | null; comment_by?: string | null; approved_by?: string | null; approved_at?: string | null;
  approver?: string; holdLabel?: string;
}
export interface MyAcks { items: MyAck[]; pending: number; holdLabel?: string; heldSince?: string }

export const ACK_TONE: Record<string, 'ok' | 'warn' | 'bad' | 'n'> = { Signed: 'ok', 'Waiting for manager': 'warn', Returned: 'bad', 'Not submitted': 'bad' };

// One change signal, so the count in a tab title follows an upload made in the list.
const subs = new Set<() => void>();
const changed = () => subs.forEach((f) => f());
function useAckData() {
  const [n, setN] = useState(0);
  useEffect(() => { const f = () => setN((x) => x + 1); subs.add(f); return () => { subs.delete(f); }; }, []);
  return useLoad(() => api<MyAcks>('/me/policies'), [n]);
}

/** Policies still pending for the signed-in person (not submitted, returned, or waiting for the manager). 0 while loading. */
export function useMyPolicyAckCount(): number {
  return useAckData().data?.pending ?? 0;
}

const when = (s?: string | null) => (s ? fmtD(String(s).slice(0, 10)) : '');

/** Company policy acknowledgements: the employee's own list (Dashboard › Compliance in My view). */
export function MyPolicyAcks() {
  const { me } = useAuth();
  const { data, error, reload } = useAckData();
  const [busy, setBusy] = useState(0);
  if (!data) return <div className="card"><h3>Company policies</h3><Loading error={error} /></div>;
  const send = async (a: MyAck, file?: File) => {
    if (!file || !me) return;
    setBusy(a.version_id);
    try {
      const doc = await uploadDoc('policy_ack', `${me.ctx.employeeId}/${a.version_id}`, 'signed', file);
      await api(`/me/policies/${a.version_id}/submit`, { body: { docId: doc.id } });
      toast('Signed copy sent to your manager for approval');
      reload(); changed();
    } catch (x: any) { toast(x.message); } finally { setBusy(0); }
  };
  const open = (id?: string | null) => { if (id) openDoc(id).catch((x) => toast(x.message)); };
  return <div className="card">
    <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>Company policies</h3>
      {data.items.length > 0 && <span className="mute" style={{ fontSize: 12.5 }}>{data.items.length - data.pending} of {data.items.length} acknowledged</span>}</div>
    {data.pending > 0 && (() => {
      // The earliest month held, and how many pending policies hold it (one in force later holds a later month).
      const t = data.items.filter((a) => a.status !== 'Signed' && a.holdLabel === data.holdLabel).map((a) => a.title);
      const what = t.length === 1 ? `${t[0]} is` : t.length === 2 ? `${t[0]} and ${t[1]} are` : 'these are';
      return <div className="note bad" role="alert" style={{ marginBottom: 12 }}>
        {data.heldSince && data.heldSince !== data.holdLabel && <>Your salary is on hold since {data.heldSince}. </>}
        Your salary for {data.holdLabel} {data.heldSince ? 'is held' : 'will be held'} until {what} approved.</div>;
    })()}
    {!data.items.length ? <p className="mute" style={{ margin: 0 }}>No company policy needs your signature.</p>
      : <ul className="log">{data.items.map((a) => {
        const canSend = a.status === 'Not submitted' || a.status === 'Returned';
        return <li key={a.version_id} style={{ display: 'flex', gap: 12, justifyContent: 'space-between', alignItems: 'flex-start', flexWrap: 'wrap', padding: '12px 0' }}>
          <div style={{ minWidth: 0, flex: '1 1 260px' }}>
            <div className="row" style={{ gap: 8 }}><b>{a.title}</b><Pill k={ACK_TONE[a.status]}>{a.status}</Pill></div>
            <div className="mute" style={{ fontSize: 12.5, marginTop: 2 }}>Version {a.version} · in force from {fmtD(a.effective_from)}</div>
            <div className="ink2" style={{ fontSize: 12.5, marginTop: 4 }}>
              {a.status === 'Signed' && <>Approved{a.approved_by ? ` by ${a.approved_by}` : ''}{a.approved_at ? ` on ${when(a.approved_at)}` : ''}.</>}
              {a.status === 'Waiting for manager' && <>Sent on {when(a.submitted_at)}{a.approver ? `, with ${a.approver}` : ''}. Your salary for {a.holdLabel} is held until it is approved.</>}
              {a.status === 'Not submitted' && <>Download the policy, print and sign it, then upload the signed copy.</>}
            </div>
            {a.status === 'Returned' && <div className="note warn" style={{ marginTop: 6, fontSize: 12.5 }}>Returned{a.comment_by ? ` by ${a.comment_by}` : ''}: “{a.comment || 'No comment'}”. Sign it again and upload the new copy.</div>}
          </div>
          <div className="row" style={{ gap: 6 }}>
            <button className="btn sm" onClick={() => open(a.policy_doc)}>Download</button>
            {a.signed_doc && <button className="btn sm" onClick={() => open(a.signed_doc)}>{a.status === 'Returned' ? 'Returned copy' : 'Signed copy'}</button>}
            {canSend && <label className="btn sm primary" aria-disabled={!!busy}>{busy === a.version_id ? 'Sending…' : 'Upload signed copy'}
              <input type="file" accept="application/pdf,image/png,image/jpeg" hidden disabled={!!busy} onChange={(e) => { send(a, e.target.files?.[0]); e.target.value = ''; }} /></label>}
          </div>
        </li>;
      })}</ul>}
    {data.items.length > 0 && <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Upload a PDF or a photo (JPG or PNG) of the signed pages, up to 5 MB. Your reporting manager approves it.</p>}
  </div>;
}
