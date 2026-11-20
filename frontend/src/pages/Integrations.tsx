import { useState } from 'react';
import { api } from '../api';
import { Adv, Field, Loading, Modal, Pill, Switch, toast, useLoad } from '../components/ui';

// Masters › Company › Integrations (HR Admin only): the client's master data SOP (SOP-ARCH-MDM-01). HRMS is the
// source of truth for employee codes and publishes employee events to PMS, Purchase Desk and Moneyball; the CMS is
// the source of truth for properties, which HRMS keeps as a read-only copy that locations link to.

type Sub = {
  id: string; name: string; url: string; secretLast4: string; eventTypes: string[]; active: boolean; cursor: number; behind: number;
  attempts: number; nextAttemptAt: string | null; lastError: string | null; failingSince: string | null; lastStatus: number | null;
  lastDeliveredAt: string | null; createdAt: string | null; createdBy: string | null; state: 'Up to date' | 'Delivering' | 'Failing' | 'Paused';
};
type Key = { id: string; name: string; keyLast4: string; active: boolean; createdAt: string | null; createdBy: string | null; lastUsedAt: string | null };
type Ev = { pos: number; eventId: string; eventType: string; employeeCode: string; name: string | null; sequence: number; timestamp: string };
type Run = { id: string; at: string | null; finishedAt: string | null; trigger: string; checked: number; fixed: number; errors: string[];
  employeesChecked: number; employeesFixed: number; propertiesChecked: number; propertiesFixed: number };
type Inbox = { eventId: string; eventType: string; schemaVersion: string; propertyId: string | null; eventTime: string | null; status: string; reason: string | null; receivedAt: string | null };
type Settings = { inboundSecretSet: boolean; inboundSecretLast4: string; cmsBaseUrl: string; cmsApiKeySet: boolean; cmsApiKeyLast4: string;
  propertiesFromCms: boolean; updatedAt: string | null; updatedBy: string; inboundPath: string; supportedVersions: string[] };
type Overview = {
  settings: Settings; subscribers: Sub[]; keys: Key[]; head: number; published: number; events: Ev[]; runs: Run[]; inbox: Inbox[];
  properties: number; linkedLocations: number; unlinkedLocations: number; eventTypes: string[]; schemaVersion: string;
  watcher: { running: boolean; server: string; lastSeenAt: string | null; resumable: boolean };
};

const when = (s?: string | null) => s ? new Date(s).toLocaleString('en-IN', { day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }) : '—';
const subPill = (s: Sub['state']) => <Pill k={s === 'Up to date' ? 'ok' : s === 'Failing' ? 'bad' : s === 'Delivering' ? 'info' : 'n'}>{s}</Pill>;
const typeLabel = (t: string) => t.replace('EMPLOYEE_', '').toLowerCase();

/** The secret or key just made: shown once, in full. */
function Secret({ what, value, onClose }: { what: string; value: string; onClose: () => void }) {
  const copy = () => navigator.clipboard?.writeText(value).then(() => toast('Copied'));
  return <Modal onClose={onClose}>
    <h2>{what}</h2>
    <p className="ink2">Copy it now and give it to the other system's team over a safe channel. HRMS keeps it sealed and shows only its last four characters from now on.</p>
    <div className="row" style={{ gap: 8, margin: '10px 0' }}><span className="mono" style={{ background: 'var(--soft)', padding: '6px 8px', borderRadius: 6, overflowWrap: 'anywhere', flex: 1 }}>{value}</span>
      <button className="btn sm" type="button" onClick={copy}>Copy</button></div>
    <div className="row" style={{ marginTop: 16 }}><button className="btn primary" onClick={onClose}>Done</button></div>
  </Modal>;
}

export default function Integrations() {
  const { data, error, reload } = useLoad(() => api<Overview>('/mdm/settings'), []);
  const [busy, setBusy] = useState('');
  const [secret, setSecret] = useState<{ what: string; value: string } | null>(null);
  const [subForm, setSubForm] = useState<{ name: string; url: string; types: string[]; start: string } | null>(null);
  const [keyName, setKeyName] = useState<string | null>(null);
  const [cms, setCms] = useState<{ base: string; key: string } | null>(null);
  const [showEvents, setShowEvents] = useState(false);

  if (!data) return <div className="card" style={{ marginTop: 14 }}><h3>Integrations (master data)</h3><Loading error={error} /></div>;
  const d = data, s = d.settings;
  const run = async (key: string, fn: () => Promise<unknown>) => { setBusy(key); try { await fn(); } catch (x: any) { toast(x.message); } finally { setBusy(''); } };

  const addSub = () => run('sub', async () => {
    const f = subForm!;
    const r = await api<{ secret: string }>('/mdm/subscribers', { body: { name: f.name, url: f.url, eventTypes: f.types, start: f.start } });
    setSubForm(null); setSecret({ what: `Signing secret for ${f.name}`, value: r.secret }); reload();
  });
  const pauseSub = (x: Sub) => run('p' + x.id, async () => { await api(`/mdm/subscribers/${x.id}`, { method: 'PATCH', body: { active: !x.active } }); toast(x.active ? 'Paused: events wait for it' : 'Resumed'); reload(); });
  const delSub = (x: Sub) => confirm(`Remove ${x.name}? It stops receiving events. The events stay in HRMS; adding it again later can start from the beginning.`)
    && run('d' + x.id, async () => { await api(`/mdm/subscribers/${x.id}`, { method: 'DELETE' }); toast('Removed'); reload(); });
  const rotSub = (x: Sub) => confirm(`Make a new signing secret for ${x.name}? Deliveries are signed with the new one at once; ${x.name} must switch to it.`)
    && run('r' + x.id, async () => { const r = await api<{ secret: string }>(`/mdm/subscribers/${x.id}/secret`, { method: 'POST' }); setSecret({ what: `New signing secret for ${x.name}`, value: r.secret }); reload(); });
  const testSub = (x: Sub) => run('t' + x.id, async () => {
    const r = await api<{ ok: boolean; status: number; error: string }>(`/mdm/subscribers/${x.id}/test`, { method: 'POST' });
    toast(r.ok ? `Test event delivered (HTTP ${r.status})` : `Test event failed: ${r.error}`);
  });
  const addKey = () => run('key', async () => {
    const r = await api<{ secret: string }>('/mdm/keys', { body: { name: keyName } });
    setSecret({ what: `API key for ${keyName}`, value: r.secret }); setKeyName(null); reload();
  });
  const pauseKey = (k: Key) => run('kp' + k.id, async () => { await api(`/mdm/keys/${k.id}`, { method: 'PATCH', body: { active: !k.active } }); reload(); });
  const rotKey = (k: Key) => confirm(`Make a new API key for ${k.name}? The old key stops working at once.`)
    && run('kr' + k.id, async () => { const r = await api<{ secret: string }>(`/mdm/keys/${k.id}/secret`, { method: 'POST' }); setSecret({ what: `New API key for ${k.name}`, value: r.secret }); reload(); });
  const delKey = (k: Key) => confirm(`Delete the API key ${k.name}? It stops working at once.`)
    && run('kd' + k.id, async () => { await api(`/mdm/keys/${k.id}`, { method: 'DELETE' }); reload(); });
  const rotInbound = () => (!s.inboundSecretSet || confirm('Make a new CMS secret? Events signed with the old one are refused from now on; the CMS must switch to the new one.'))
    && run('in', async () => { const r = await api<{ secret: string }>('/mdm/settings/cms/secret', { method: 'POST' }); setSecret({ what: 'Secret for the CMS', value: r.secret }); reload(); });
  const saveCms = () => run('cms', async () => {
    const body: Record<string, any> = { cmsBaseUrl: cms!.base };
    if (cms!.key.trim()) body.cmsApiKey = cms!.key;
    await api('/mdm/settings/cms', { method: 'PUT', body }); toast('CMS connection saved'); setCms(null); reload();
  });
  const clearCmsKey = () => run('cmsk', async () => { await api('/mdm/settings/cms', { method: 'PUT', body: { cmsApiKey: '' } }); reload(); });
  const fromCms = () => {
    const on = !s.propertiesFromCms;
    if (on && !confirm(`Turn on “Properties come from the CMS”? New locations can then only be added by picking their CMS property. ${d.unlinkedLocations} active location${d.unlinkedLocations === 1 ? ' is' : 's are'} not linked yet.`)) return;
    run('fc', async () => { await api('/mdm/settings/cms', { method: 'PUT', body: { propertiesFromCms: on } }); reload(); });
  };
  const reconcile = () => run('rec', async () => {
    const r = await api<Run>('/mdm/reconcile', { method: 'POST' });
    toast(`Reconciled: ${r.checked} checked, ${r.fixed} fixed${r.errors.length ? `, ${r.errors.length} problem${r.errors.length > 1 ? 's' : ''}` : ''}`); reload();
  });
  const lastRun = d.runs[0];

  return <div className="card" style={{ marginTop: 14 }} id="integrations">
    <div className="row sp"><h3 style={{ margin: 0 }}>Integrations (master data)</h3>
      <Pill k={d.watcher.running ? 'ok' : 'warn'}>{d.watcher.running ? 'Publishing' : 'Publisher starting'}</Pill></div>
    <p className="mute" style={{ fontSize: 12.5, margin: '8px 0 12px' }}>Under the company's master data SOP, HRMS owns the employee code: every change to an employee's published details goes to PMS, Purchase Desk and Moneyball as a signed event (schema {d.schemaVersion}), in order, and they read the current list each night to check they hold the same. The CMS owns properties: HRMS keeps a read-only copy, and a location linked to a property takes its name and status from the CMS. Contact details, salary, bank and government ids are never published.</p>

    <dl className="kv">
      <dt>Employees</dt><dd>{d.published} published · last event #{d.head}{d.watcher.lastSeenAt && <span className="mute"> · publisher seen {when(d.watcher.lastSeenAt)}</span>}</dd>
      <dt>Properties</dt><dd>{d.properties} from the CMS · {d.linkedLocations} location{d.linkedLocations === 1 ? '' : 's'} linked{d.unlinkedLocations > 0 && <> · <span style={{ color: 'var(--warn, inherit)' }}>{d.unlinkedLocations} active not linked</span></>}</dd>
      <dt>Last check</dt><dd>{lastRun ? <>{when(lastRun.at)}: {lastRun.checked} checked, {lastRun.fixed} fixed{lastRun.errors.length > 0 && <span style={{ color: 'var(--bad)' }}> · {lastRun.errors.join('; ')}</span>}</> : 'Not run yet (runs every night after 02:00)'}</dd>
    </dl>
    <div className="row" style={{ marginTop: 10 }}><button className="btn" disabled={!!busy} onClick={reconcile}>{busy === 'rec' ? 'Checking…' : 'Reconcile now'}</button>
      <span className="mute" style={{ fontSize: 12 }}>Recomputes every employee and, when the CMS address is set, compares its property list.</span></div>

    <h4 style={{ margin: '18px 0 6px' }}>Systems that receive employee events (push)</h4>
    {d.subscribers.length === 0 ? <p className="mute" style={{ fontSize: 12.5 }}>None yet. Add each system that should be told about employee changes; it receives a POST for every event, signed with its own secret.</p>
      : <div className="tw"><table>
        <thead><tr><th>System</th><th>Address</th><th>Events</th><th>State</th><th>Active</th><th></th></tr></thead>
        <tbody>{d.subscribers.map((x) => <tr key={x.id}>
          <td><b>{x.name}</b><div className="mute" style={{ fontSize: 11.5 }}>secret …{x.secretLast4}</div></td>
          <td className="mono" style={{ maxWidth: 260, overflowWrap: 'anywhere' }}>{x.url}</td>
          <td className="ink2" style={{ fontSize: 12.5 }}>{x.eventTypes.length ? x.eventTypes.map(typeLabel).join(', ') : 'All'}</td>
          <td>{subPill(x.state)}
            <div className="mute" style={{ fontSize: 11.5 }}>{x.behind ? `${x.behind} waiting` : `at #${x.cursor}`}{x.lastDeliveredAt ? ` · last ${when(x.lastDeliveredAt)}` : ''}</div>
            {x.lastError && x.failingSince && <div style={{ fontSize: 11.5, color: 'var(--bad)', maxWidth: 260 }}>{x.lastError} · since {when(x.failingSince)}{x.nextAttemptAt ? ` · next try ${when(x.nextAttemptAt)}` : ''}</div>}</td>
          <td><Switch on={x.active} disabled={!!busy} onChange={() => pauseSub(x)} label={x.active ? 'Pause' : 'Resume'} /></td>
          <td style={{ whiteSpace: 'nowrap' }}><button className="btn sm" disabled={!!busy} onClick={() => testSub(x)}>{busy === 't' + x.id ? 'Sending…' : 'Test'}</button>{' '}
            <button className="btn sm" disabled={!!busy} onClick={() => rotSub(x)}>New secret</button>{' '}
            <button className="btn sm bad" disabled={!!busy} onClick={() => delSub(x)}>Remove</button></td>
        </tr>)}</tbody>
      </table></div>}
    {subForm ? <div className="form" style={{ marginTop: 10 }}>
      <Field label="System"><input value={subForm.name} placeholder="PMS, Purchase Desk, Moneyball" onChange={(e) => setSubForm({ ...subForm, name: e.target.value })} /></Field>
      <Field label="Webhook address"><input value={subForm.url} placeholder="https://…/hrms/events" onChange={(e) => setSubForm({ ...subForm, url: e.target.value })} /></Field>
      <Field label="Start from"><select value={subForm.start} onChange={(e) => setSubForm({ ...subForm, start: e.target.value })}>
        <option value="beginning">Every event so far (first sync)</option><option value="now">New events only</option></select></Field>
      <div className="field" style={{ gridColumn: '1/-1' }}><span>Events (none ticked: all)</span><div className="chkgrp">{d.eventTypes.map((t) => <label key={t} className="chkl">
        <input type="checkbox" checked={subForm.types.includes(t)} onChange={(e) => setSubForm({ ...subForm, types: e.target.checked ? [...subForm.types, t] : subForm.types.filter((y) => y !== t) })} /><span className="mono">{t}</span></label>)}</div></div>
      <div className="row" style={{ gridColumn: '1/-1' }}><button className="btn primary" disabled={!!busy || !subForm.name.trim() || !subForm.url.trim()} onClick={addSub}>{busy === 'sub' ? 'Adding…' : 'Add system'}</button>
        <button className="btn" onClick={() => setSubForm(null)}>Cancel</button></div>
    </div> : <div className="row" style={{ marginTop: 10 }}><button className="btn" onClick={() => setSubForm({ name: '', url: '', types: [], start: 'beginning' })}>Add system</button></div>}

    <h4 style={{ margin: '18px 0 6px' }}>API keys for the pull feed</h4>
    <p className="mute" style={{ fontSize: 12.5, margin: '0 0 8px' }}>A system can also read the events (<span className="mono">GET /api/mdm/events</span>) and the nightly list (<span className="mono">/api/mdm/employees</span>, <span className="mono">/summary</span>) with its own key. HRMS keeps only a fingerprint of each key.</p>
    {d.keys.length > 0 && <div className="tw"><table>
      <thead><tr><th>System</th><th>Key</th><th>Last used</th><th>Active</th><th></th></tr></thead>
      <tbody>{d.keys.map((k) => <tr key={k.id}>
        <td><b>{k.name}</b><div className="mute" style={{ fontSize: 11.5 }}>by {k.createdBy || '—'}, {when(k.createdAt)}</div></td>
        <td className="mono">…{k.keyLast4}</td><td className="mute">{when(k.lastUsedAt)}</td>
        <td><Switch on={k.active} disabled={!!busy} onChange={() => pauseKey(k)} label={k.active ? 'Pause' : 'Resume'} /></td>
        <td style={{ whiteSpace: 'nowrap' }}><button className="btn sm" disabled={!!busy} onClick={() => rotKey(k)}>New key</button>{' '}
          <button className="btn sm bad" disabled={!!busy} onClick={() => delKey(k)}>Delete</button></td>
      </tr>)}</tbody>
    </table></div>}
    {keyName != null ? <div className="row" style={{ alignItems: 'flex-end', marginTop: 10 }}>
      <div style={{ flex: '1 1 220px' }}><Field label="System"><input value={keyName} autoFocus placeholder="Moneyball" onChange={(e) => setKeyName(e.target.value)} /></Field></div>
      <button className="btn primary" disabled={!!busy || !keyName.trim()} onClick={addKey}>Make key</button><button className="btn" onClick={() => setKeyName(null)}>Cancel</button>
    </div> : <div className="row" style={{ marginTop: 10 }}><button className="btn" onClick={() => setKeyName('')}>Add API key</button></div>}

    <h4 style={{ margin: '18px 0 6px' }}>CMS (properties)</h4>
    <dl className="kv">
      <dt>Events from the CMS</dt><dd><span className="mono">POST {s.inboundPath}</span>, signed with {s.inboundSecretSet ? <>the secret …{s.inboundSecretLast4}</> : <b>a secret not made yet</b>} · versions {s.supportedVersions.join(', ')}</dd>
      <dt>CMS address</dt><dd>{s.cmsBaseUrl ? <span className="mono">{s.cmsBaseUrl}/properties</span> : <span className="mute">Not set: the nightly check compares employees only</span>}{s.cmsApiKeySet && <span className="mute"> · API key …{s.cmsApiKeyLast4}</span>}</dd>
      <dt>New locations</dt><dd className="row" style={{ gap: 8 }}><Switch on={s.propertiesFromCms} disabled={!!busy} onChange={fromCms} label="Properties come from the CMS" />
        <span>{s.propertiesFromCms ? 'Properties come from the CMS: a location is added by picking its CMS property' : 'Added in HRMS (link each to its CMS property in Masters › Locations)'}</span></dd>
    </dl>
    <div className="row" style={{ marginTop: 10 }}>
      <button className="btn" disabled={!!busy} onClick={rotInbound}>{s.inboundSecretSet ? 'New CMS secret' : 'Make CMS secret'}</button>
      {!cms && <button className="btn" onClick={() => setCms({ base: s.cmsBaseUrl, key: '' })}>Set CMS address</button>}
      {s.cmsApiKeySet && !cms && <button className="btn bad" disabled={!!busy} onClick={clearCmsKey}>Remove CMS API key</button>}
    </div>
    {cms && <div className="form" style={{ marginTop: 10 }}>
      <Field label="CMS address"><input value={cms.base} placeholder="https://cms.example.com/api" onChange={(e) => setCms({ ...cms, base: e.target.value })} /></Field>
      <Field label={s.cmsApiKeySet ? `CMS API key (…${s.cmsApiKeyLast4}; empty keeps it)` : 'CMS API key'}><input type="password" value={cms.key} autoComplete="off" onChange={(e) => setCms({ ...cms, key: e.target.value })} /></Field>
      <div className="row" style={{ gridColumn: '1/-1' }}><button className="btn primary" disabled={!!busy} onClick={saveCms}>Save</button><button className="btn" onClick={() => setCms(null)}>Cancel</button></div>
    </div>}
    {d.inbox.length > 0 && <Adv title={`Last ${d.inbox.length} events from the CMS`}>
      <div className="tw"><table><thead><tr><th>Received</th><th>Event</th><th>Property</th><th>Result</th></tr></thead>
        <tbody>{d.inbox.map((i) => <tr key={i.eventId}><td className="mute">{when(i.receivedAt)}</td><td className="mono">{i.eventType} {i.schemaVersion}</td><td className="mono">{i.propertyId ?? '—'}</td>
          <td><Pill k={i.status === 'applied' ? 'ok' : i.status === 'rejected' ? 'bad' : 'n'}>{i.status}</Pill>{i.reason && <div className="mute" style={{ fontSize: 11.5 }}>{i.reason}</div>}</td></tr>)}</tbody></table></div>
    </Adv>}

    <div className="row" style={{ marginTop: 16 }}><button className="btn sm" onClick={() => setShowEvents(!showEvents)}>{showEvents ? 'Hide' : 'Show'} recent events</button></div>
    {showEvents && <div className="tw" style={{ marginTop: 8 }}><table>
      <thead><tr><th>#</th><th>Event</th><th>Employee</th><th>Seq.</th><th>Time (UTC)</th></tr></thead>
      <tbody>{d.events.length === 0 ? <tr><td colSpan={5} className="mute">No events yet</td></tr> : d.events.map((e) => <tr key={e.pos}>
        <td className="mono">{e.pos}</td><td className="mono">{e.eventType}</td><td><b>{e.employeeCode}</b> {e.name}</td><td className="mono">{e.sequence}</td><td className="mono">{e.timestamp}</td></tr>)}</tbody>
    </table></div>}
    {showEvents && d.runs.length > 1 && <div className="tw" style={{ marginTop: 8 }}><table>
      <thead><tr><th>Reconciliation</th><th>By</th><th>Employees</th><th>Properties</th><th>Problems</th></tr></thead>
      <tbody>{d.runs.map((r) => <tr key={r.id}><td className="mute">{when(r.at)}</td><td>{r.trigger}</td><td>{r.employeesChecked} checked, {r.employeesFixed} fixed</td>
        <td>{r.propertiesChecked} checked, {r.propertiesFixed} fixed</td><td style={{ color: r.errors.length ? 'var(--bad)' : undefined }}>{r.errors.join('; ') || '—'}</td></tr>)}</tbody>
    </table></div>}

    {secret && <Secret what={secret.what} value={secret.value} onClose={() => setSecret(null)} />}
  </div>;
}
