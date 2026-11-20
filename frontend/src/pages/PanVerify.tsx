import { useState } from 'react';
import { api } from '../api';
import { Field, Loading, Pill, Switch, toast, useLoad } from '../components/ui';

type Setting = { provider: string; baseUrl: string; on: boolean; keySet: boolean; keyLast4: string; updatedBy: string; updatedAt: string | null; providers: string[]; ready: boolean };

const when = (s?: string | null) => s ? new Date(s).toLocaleString('en-IN', { day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }) : '—';

/** Masters › Company › PAN verification (HR Admin): the provider, its API key (sealed, shown as its last four
 *  characters), base URL and the switch. Test works offline; the real providers say "not configured" until a key is
 *  saved, and send nothing in this build. */
export default function PanVerify() {
  const { data, error, reload } = useLoad(() => api<Setting>('/settings/pan-verify'), []);
  const [v, setV] = useState<{ provider: string; baseUrl: string; on: boolean; apiKey: string } | null>(null);
  const [busy, setBusy] = useState(false);
  if (!data) return <div className="card" style={{ marginTop: 14 }}><h3>PAN verification</h3><Loading error={error} /></div>;
  const cur = v ?? { provider: data.provider, baseUrl: data.baseUrl, on: data.on, apiKey: '' };
  const real = cur.provider !== 'None' && cur.provider !== 'Test';
  const save = async (clearKey = false) => {
    setBusy(true);
    try {
      await api('/settings/pan-verify', { method: 'PUT', body: { provider: cur.provider, baseUrl: cur.baseUrl, on: cur.on, apiKey: cur.apiKey || undefined, clearKey } });
      toast('PAN verification saved'); setV(null); reload();
    } catch (x: any) { toast(x.message); } finally { setBusy(false); }
  };
  return <div className="card" style={{ marginTop: 14 }} data-testid="pan-verify">
    <div className="row sp"><h3 style={{ margin: 0 }}>PAN verification</h3>
      {data.ready ? <Pill k="ok">On · {data.provider}</Pill> : data.on ? <Pill k="warn">On, not configured</Pill> : <Pill k="n">Off</Pill>}</div>
    <p className="mute" style={{ fontSize: 12.5 }}>When on, a PAN is checked when it is entered on the joining form, and with Verify PAN on an employee's statutory IDs. The result (Verified, Name mismatch or Invalid), the name returned and the time are kept and audited. Test works offline for trying the app: a PAN whose 4th letter is P verifies when its 5th letter is the first letter of the surname.</p>
    <div className="form">
      <Field label="Provider"><select value={cur.provider} onChange={(e) => setV({ ...cur, provider: e.target.value })}>{data.providers.map((p) => <option key={p}>{p}</option>)}</select></Field>
      {real && <Field label="Base URL"><input placeholder="https://…" value={cur.baseUrl} onChange={(e) => setV({ ...cur, baseUrl: e.target.value })} /></Field>}
      {real && <Field label={data.keySet ? `API key (saved, ends ${data.keyLast4})` : 'API key'}><input type="password" autoComplete="off" placeholder={data.keySet ? 'Leave blank to keep the saved key' : 'Paste the key'} value={cur.apiKey} onChange={(e) => setV({ ...cur, apiKey: e.target.value })} /></Field>}
      <Field label="Verification"><Switch on={cur.on} label={cur.on ? 'On' : 'Off'} onChange={() => setV({ ...cur, on: !cur.on })} /></Field>
    </div>
    {real && <p className="mute" style={{ fontSize: 12 }}>{cur.provider} is not connected in this build yet: with a key saved it still says "not configured" and sends nothing.</p>}
    <div className="row" style={{ marginTop: 10, flexWrap: 'wrap' }}>
      <button className="btn primary" disabled={busy} onClick={() => save()}>Save</button>
      {data.keySet && <button className="btn" disabled={busy} onClick={() => save(true)}>Remove the key</button>}
      {data.updatedBy && <span className="mute" style={{ fontSize: 12 }}>Last saved by {data.updatedBy}, {when(data.updatedAt)}</span>}
    </div>
  </div>;
}
