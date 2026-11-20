import { useEffect, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { api } from '../api';
import { Field, Loading, Pill, toast, useLoad } from '../components/ui';

type Drive = {
  configured: boolean; redirectUri: string; redirectFixed: boolean;
  connected: boolean; email: string; connectedBy: string; connectedAt: string | null; broken: string;
  folder: { id: string; name: string; link: string; url: string } | null;
  waiting: number; drafts: number; existing: number; inDrive: number;
  lastUploadAt: string | null; lastError: string; lastErrorAt: string | null; moved?: number;
};

const when = (s?: string | null) => s ? new Date(s).toLocaleString('en-IN', { day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }) : '—';
const files = (n: number) => `${n} ${n === 1 ? 'file' : 'files'}`;

/** Masters › Company: every uploaded document is kept in the company's Google Drive (HR Admin only). */
export default function DriveStorage() {
  const [qs, setQs] = useSearchParams();
  const { data, error, reload } = useLoad(() => api<Drive>('/drive'), []);
  const [link, setLink] = useState<string | null>(null);
  const [busy, setBusy] = useState('');
  // Back from Google's sign-in: ?drive=connected, or ?drive=error&msg=…
  const [back] = useState(() => ({ ok: qs.get('drive') === 'connected', err: qs.get('drive') === 'error' ? qs.get('msg') || 'Google sign-in did not finish' : '' }));
  useEffect(() => {
    if (!qs.get('drive')) return;
    const n = new URLSearchParams(qs); n.delete('drive'); n.delete('msg'); setQs(n, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  // While files are going up, look again every few seconds.
  useEffect(() => {
    if (!data?.connected || !data.folder || data.waiting === 0) return;
    const t = setTimeout(reload, 4000);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data]);
  if (!data) return <div className="card" style={{ marginTop: 14 }}><h3>Document storage (Google Drive)</h3><Loading error={error} /></div>;
  const d = data;
  const ready = d.connected && !!d.folder;
  const run = async (key: string, fn: () => Promise<unknown>) => { setBusy(key); try { await fn(); } catch (x: any) { toast(x.message); } finally { setBusy(''); } };
  const connect = () => run('connect', async () => { const r = await api<{ url: string }>('/drive/connect', { method: 'POST' }); window.location.assign(r.url); });
  const disconnect = () => confirm('Disconnect Google Drive? Files already in Drive stay there, but cannot be opened from the app until Drive is connected again. New uploads wait on the server.')
    && run('disconnect', async () => { await api('/drive/disconnect', { method: 'POST' }); toast('Google Drive disconnected'); reload(); });
  const saveFolder = () => run('folder', async () => { const r = await api<Drive>('/drive/folder', { method: 'PUT', body: { link: link ?? '' } }); toast(`Folder saved: ${r.folder?.name}`); setLink(null); reload(); });
  const move = () => confirm(`Move ${files(d.existing)} uploaded before Drive was set up to Google Drive? Each is removed from the server once it is safely in Drive.`)
    && run('move', async () => { const r = await api<Drive>('/drive/move-existing', { method: 'POST' }); toast(`${files(r.moved ?? 0)} on the way to Google Drive`); reload(); });
  const tryNow = () => run('sync', async () => { await api('/drive/sync', { method: 'POST' }); toast('Trying the waiting files now'); reload(); });
  const copy = () => navigator.clipboard?.writeText(d.redirectUri).then(() => toast('Copied'));
  const problem = !!d.lastError && (!d.lastUploadAt || (!!d.lastErrorAt && new Date(d.lastErrorAt) > new Date(d.lastUploadAt)));
  const uri = <div className="row" style={{ gap: 8, marginTop: 4 }}><span className="mono" style={{ background: 'var(--soft)', padding: '4px 8px', borderRadius: 6, overflowWrap: 'anywhere' }}>{d.redirectUri}</span>
    <button className="btn sm" type="button" onClick={copy}>Copy</button></div>;
  return <div className="card" style={{ marginTop: 14 }} id="drive-storage">
    <div className="row sp"><h3 style={{ margin: 0 }}>Document storage (Google Drive)</h3>
      <Pill k={d.connected ? 'ok' : d.broken ? 'bad' : 'warn'}>{d.connected ? 'Connected' : d.broken ? 'Reconnect needed' : 'Not connected'}</Pill></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 12px' }}>Every uploaded document is saved in your company's Google Drive, in a folder per person. The app keeps no copy once a file is safely in Drive; opening a document fetches it from Drive through the app, so people need no Drive access of their own.</p>
    {back.ok && d.connected && <div className="note ok" style={{ marginBottom: 12 }}>Google Drive connected as <b>{d.email}</b>.{!d.folder && ' Now paste the folder link below.'}</div>}
    {back.err && <div className="note bad" role="alert" style={{ marginBottom: 12 }}>{back.err}</div>}
    {d.broken && <div className="note bad" style={{ marginBottom: 12 }}>{d.broken}.</div>}

    {!d.configured ? <div className="note warn">
      <b>One-time setup in Google Cloud</b> (by an admin of your Google Workspace):
      <ol style={{ margin: '8px 0 0', paddingLeft: 20, lineHeight: 1.7 }}>
        <li>Open <span className="mono">console.cloud.google.com</span> and create a project, for example “Saltstayz HRMS”.</li>
        <li>APIs &amp; Services › Library: enable the <b>Google Drive API</b>.</li>
        <li>OAuth consent screen: user type <b>Internal</b>, so only your company's Google accounts can sign in and Google does not need to review the app.</li>
        <li>Credentials › Create credentials › OAuth client ID, type <b>Web application</b>. Under Authorised redirect URIs add exactly:{uri}</li>
        <li>Put the client ID and secret in <span className="mono">backend/.env</span> as <span className="mono">GOOGLE_CLIENT_ID=…</span> and <span className="mono">GOOGLE_CLIENT_SECRET=…</span>, then restart the API.</li>
      </ol>
    </div> : <>
      <div className="row sp" style={{ alignItems: 'flex-start' }}>
        <div style={{ fontSize: 13 }}>{d.connected
          ? <>Connected as <b>{d.email || 'a Google account'}</b><div className="mute" style={{ fontSize: 12 }}>by {d.connectedBy || '—'}, {when(d.connectedAt)}. Files belong to this account.</div></>
          : <span className="mute">Sign in with the company Google account that should own the documents.</span>}</div>
        <div className="row" style={{ gap: 8 }}>
          {!d.connected && <button className="btn primary" disabled={!!busy} onClick={connect}>{busy === 'connect' ? 'Opening Google…' : d.broken ? 'Reconnect Google Drive' : 'Connect Google Drive'}</button>}
          {(d.connected || d.broken) && <button className="btn bad" disabled={!!busy} onClick={disconnect}>Disconnect</button>}
        </div>
      </div>
      {!d.connected && <div className="mute" style={{ fontSize: 12, marginTop: 8 }}>Redirect URI registered in Google Cloud:{uri}</div>}
    </>}

    <div className="row" style={{ alignItems: 'flex-end', marginTop: 14 }}>
      <div style={{ flex: '1 1 260px', minWidth: 0 }}><Field label="Google Drive folder link">
        <input value={link ?? d.folder?.link ?? ''} placeholder="https://drive.google.com/drive/folders/…" disabled={!d.connected} onChange={(e) => setLink(e.target.value)} /></Field></div>
      <button className="btn" disabled={!d.connected || !!busy || !(link ?? '').trim()} onClick={saveFolder}>{busy === 'folder' ? 'Checking…' : 'Save folder'}</button>
    </div>
    <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>{d.folder
      ? <>Saving in <a href={d.folder.url} target="_blank" rel="noreferrer" style={{ color: 'var(--accent)', fontWeight: 600 }}>{d.folder.name}</a>. </>
      : d.connected ? 'Paste the link of a folder this account can edit (My Drive or a shared drive). ' : 'Connect first, then paste the folder link. '}
      Inside it: <span className="mono">SI-0001/SI-0001 Aadhaar card.pdf</span> for a candidate (the label comes from Onboarding setup), <span className="mono">EMP-1020/EMP-1020 PAN card.pdf</span> for people hired before onboarding stages, <span className="mono">Company/</span> for the payslip logo and policy files. A replaced file is renamed “(replaced 11 Nov 2026)”; nothing is deleted in Drive.</p>

    <dl className="kv" style={{ marginTop: 14 }}>
      <dt>Waiting</dt><dd>{d.waiting ? <>{files(d.waiting)} on the server, {ready ? 'going to Drive' : 'until Drive is connected and a folder is set'}</> : 'Nothing waiting'}</dd>
      <dt>In Drive</dt><dd>{files(d.inDrive)}</dd>
      {d.existing > 0 && <><dt>Before Drive</dt><dd>{files(d.existing)} uploaded before Drive was set up, still on the server</dd></>}
      <dt>Last upload</dt><dd>{d.lastUploadAt ? when(d.lastUploadAt) : 'None yet'}</dd>
      {problem && <><dt>Last problem</dt><dd style={{ color: 'var(--bad)' }}>{d.lastError} <span className="mute">({when(d.lastErrorAt)})</span></dd></>}
    </dl>
    {(d.existing > 0 || (d.waiting > 0 && ready)) && <div className="row" style={{ marginTop: 12 }}>
      {d.existing > 0 && <button className="btn" disabled={!ready || !!busy} title={ready ? '' : 'Connect Google Drive and set the folder first'} onClick={move}>
        {busy === 'move' ? 'Moving…' : `Move ${d.existing} existing ${d.existing === 1 ? 'file' : 'files'} to Drive`}</button>}
      {d.waiting > 0 && ready && <button className="btn" disabled={!!busy} onClick={tryNow}>{busy === 'sync' ? 'Trying…' : 'Try now'}</button>}
    </div>}
  </div>;
}
