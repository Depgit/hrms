import { useEffect, useState } from 'react';
import { api, fmtD } from '../api';
import { Pill, download, toast, useLoad, Loading } from '../components/ui';

// "Match portal codes" (backend realtime_match.go): fetch the Realtime portal's employee list, review the proposed
// Attendance code for each employee, and apply the ticked ones (or download them for Bulk update).

type Cand = { code: string; card: string; name: string; dept: string; desig: string; branch: string; doj: string; why?: string };
type Prop = { employeeId: string; name: string; dept: string; level: Level; reason: string; pick?: Cand; cands?: Cand[] };
type Level = 'Sure' | 'Likely' | 'Check' | 'None';
type ApplyRes = { employeeId: string; name: string; code: string; ok: boolean; error?: string };

const LEVELS: Level[] = ['Sure', 'Likely', 'Check', 'None'];
const tone = (l: Level) => (l === 'Sure' ? 'ok' : l === 'Likely' ? 'info' : l === 'Check' ? 'warn' : 'n') as 'ok' | 'info' | 'warn' | 'n';
const whenT = (at: string) => new Date(at).toLocaleString('en-IN', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' });
const SIZE = 100;

export default function RealtimeMatch({ ed }: { ed: boolean }) {
  const [level, setLevel] = useState<'All' | Level>('Sure');
  const [q, setQ] = useState('');
  const [page, setPage] = useState(1);
  const qs = `level=${level}&page=${page}&size=${SIZE}${q.trim() ? `&q=${encodeURIComponent(q.trim())}` : ''}`;
  const d = useLoad(() => api(`/attendance/realtime/match?${qs}`), [qs]);
  const [sel, setSel] = useState<Record<string, string>>({}); // employee → code
  const [busy, setBusy] = useState('');
  const [result, setResult] = useState<{ applied: number; failed: number; results: ApplyRes[] } | null>(null);
  const running = d.data?.running;
  useEffect(() => { // while the list is being fetched, look again every few seconds
    if (!running) return;
    const t = setInterval(d.reload, 4000);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [running]);
  useEffect(() => setPage(1), [level, q]);

  const start = async () => {
    setBusy('fetch');
    try { await api('/attendance/realtime/match', { body: {} }); toast('Fetching the employee list. It takes about a minute'); setSel({}); setResult(null); d.reload(); }
    catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const selectAllSure = async () => {
    setBusy('sure');
    try {
      const add: Record<string, string> = {};
      for (let p = 1; ; p++) {
        const r = await api(`/attendance/realtime/match?level=Sure&page=${p}&size=500`);
        (r.rows as Prop[]).forEach((x) => { if (x.pick) add[x.employeeId] = x.pick.code || x.pick.card; });
        if (p * 500 >= r.total) break;
      }
      setSel((s) => ({ ...s, ...add }));
      toast(`${Object.keys(add).length} Sure proposals ticked`);
    } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const apply = async () => {
    const pairs = Object.entries(sel).map(([employeeId, code]) => ({ employeeId, code }));
    if (!pairs.length) return;
    setBusy('apply');
    try {
      const r = await api('/attendance/realtime/match/apply', { body: { pairs } });
      setResult(r); setSel({}); d.reload();
      toast(`${r.applied} Attendance code${r.applied === 1 ? '' : 's'} given${r.failed ? `, ${r.failed} not` : ''}`);
    } catch (e: any) { toast(e.message); } finally { setBusy(''); }
  };
  const toggle = (p: Prop, code?: string) => setSel((s) => {
    const n = { ...s };
    if (code === undefined ? n[p.employeeId] : !code) delete n[p.employeeId];
    else n[p.employeeId] = code ?? (p.pick ? p.pick.code || p.pick.card : '');
    return n;
  });

  const data = d.data;
  const rows: Prop[] = data?.rows ?? [];
  const counts: Record<Level, number> = data?.counts ?? { Sure: 0, Likely: 0, Check: 0, None: 0 };
  const all = LEVELS.reduce((a, l) => a + (counts[l] || 0), 0);
  const nSel = Object.keys(sel).length;
  const pill = !data ? null : !data.envSet ? <Pill k="warn">Account not set</Pill> : running ? <Pill k="info">Fetching…</Pill>
    : data.error && (!data.fetchedAt || data.errorAt > data.fetchedAt) ? <Pill k="bad">Last fetch failed</Pill> : data.fetchedAt ? <Pill k="ok">{all} to review</Pill> : <Pill k="n">Not fetched yet</Pill>;
  const portalLine = (c: Cand) => <>
    <span className="mono">{c.code}</span>{c.card && c.card !== c.code ? <span className="mute mono"> · {c.card}</span> : ''} — {c.name || '—'}
    <div className="mute" style={{ fontSize: 12 }}>{[c.dept, c.desig, c.branch].filter(Boolean).join(' · ')}{c.doj ? ` · joined ${fmtD(c.doj)}` : ''}</div>
  </>;

  return <div className="card" style={{ marginBottom: 14 }}>
    <div className="row sp"><h3 style={{ margin: 0 }}>Match portal codes</h3>{pill}</div>
    {!data ? <Loading error={d.error} /> : <>
      <p className="mute" style={{ fontSize: 12.5 }}>Gives each employee their <b>Attendance code</b> without typing them. The HRMS downloads the portal’s list of active employees and proposes, for everyone who has no code yet, the portal employee who is the same person: by mobile, name, dates of birth and joining, department, designation and location. Nothing is given until you tick it and apply.</p>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
        {ed && <button className="btn primary" disabled={!!busy || !!running || !data.envSet} onClick={start}>{running ? 'Fetching…' : busy === 'fetch' ? 'Starting…' : data.fetchedAt ? 'Fetch the list again' : 'Fetch employee list'}</button>}
        {data.fetchedAt && <span className="mute" style={{ fontSize: 12.5 }}>Fetched {whenT(data.fetchedAt)} by {data.by}: {data.portal} portal employees, {data.used} already have their code in the HRMS.</span>}
      </div>
      {running && <div className="note" style={{ marginTop: 10 }}>Fetching the employee list, started by {running.by}…</div>}
      {data.error && (!data.fetchedAt || data.errorAt > data.fetchedAt) && <div className="note bad" style={{ marginTop: 10 }}>The last fetch failed: {data.error}</div>}

      {data.fetchedAt && <>
        <div className="row sp" style={{ marginTop: 14, gap: 8, flexWrap: 'wrap' }}>
          <div className="seg">{(['All', ...LEVELS] as const).map((l) => <button key={l} className={level === l ? 'on' : ''} onClick={() => setLevel(l)}>{l}<b>{l === 'All' ? all : counts[l]}</b></button>)}</div>
          <input type="search" placeholder="Search name or Employee ID" value={q} onChange={(e) => setQ(e.target.value)} style={{ maxWidth: 240 }} />
        </div>
        <p className="mute" style={{ fontSize: 12, margin: '8px 0' }}><b>Codes only, never names</b> (many people share a name). <b>Sure</b>: the portal code or card is the number of the Employee ID, or of the Joining ID the person had before it (EMP-3201 / JI-3201 → 003201). <b>Check</b>: that number is on more than one portal employee; pick one. <b>None</b>: no portal code has that number; give the code by Bulk update › Employee master update. The portal's name is shown only for you to look at.</p>
        {ed && <div className="row" style={{ gap: 8, flexWrap: 'wrap', marginBottom: 8 }}>
          <button className="btn sm" disabled={!!busy || !counts.Sure} onClick={selectAllSure}>{busy === 'sure' ? 'Ticking…' : `Tick all Sure (${counts.Sure})`}</button>
          <button className="btn sm primary" disabled={!!busy || !nSel} onClick={apply}>{busy === 'apply' ? 'Applying…' : `Apply ticked (${nSel})`}</button>
          {nSel > 0 && <button className="btn sm" disabled={!!busy} onClick={() => setSel({})}>Clear ticks</button>}
          <button className="btn sm" onClick={() => download('/attendance/realtime/match/download?levels=Sure,Likely', 'attendance-codes.xlsx').catch((e) => toast(e.message))}>Download Sure for Bulk update</button>
        </div>}
        <div className="tw"><table><thead><tr>{ed && <th style={{ width: 28 }}></th>}<th>Employee</th><th>Portal employee</th><th>Level</th><th>Why</th></tr></thead><tbody>
          {rows.length === 0 && <tr><td colSpan={ed ? 5 : 4} className="mute">Nothing here.</td></tr>}
          {rows.map((p) => {
            const chosen = sel[p.employeeId];
            return <tr key={p.employeeId}>
              {ed && <td>{p.pick && <input type="checkbox" checked={!!chosen} onChange={() => toggle(p)} />}</td>}
              <td>{p.name}<div className="mute mono" style={{ fontSize: 12 }}>{p.employeeId}{p.dept ? <span style={{ fontFamily: 'inherit' }}> · {p.dept}</span> : ''}</div></td>
              <td>{p.pick ? portalLine(p.pick) : p.cands?.length ? <>
                {ed ? <select value={chosen ?? ''} onChange={(e) => toggle(p, e.target.value)} style={{ maxWidth: 320 }}>
                  <option value="">Pick the right one…</option>
                  {p.cands.map((c) => <option key={c.code + c.card} value={c.code || c.card}>{c.code} — {c.name} ({[c.dept, c.desig].filter(Boolean).join(', ') || 'no department'}){c.why ? ` · ${c.why}` : ''}</option>)}
                </select> : <span className="mute">{p.cands.length} candidates</span>}
              </> : <span className="mute">—</span>}</td>
              <td><Pill k={tone(p.level)}>{p.level}</Pill></td>
              <td style={{ fontSize: 12.5 }}>{p.reason}</td>
            </tr>;
          })}
        </tbody></table></div>
        {data.total > SIZE && <div className="row" style={{ gap: 8, marginTop: 8, alignItems: 'center' }}>
          <button className="btn sm" disabled={page <= 1} onClick={() => setPage(page - 1)}>Previous</button>
          <span className="mute" style={{ fontSize: 12.5 }}>{(page - 1) * SIZE + 1}–{Math.min(page * SIZE, data.total)} of {data.total}</span>
          <button className="btn sm" disabled={page * SIZE >= data.total} onClick={() => setPage(page + 1)}>Next</button>
        </div>}
        {result && <div className={`note ${result.failed ? 'warn' : 'ok'}`} style={{ marginTop: 12 }}>
          {result.applied} given{result.failed ? `, ${result.failed} not:` : '.'}
          {result.failed > 0 && <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{result.results.filter((r) => !r.ok).slice(0, 20).map((r) => <li key={r.employeeId + r.code}>{r.name || r.employeeId} ({r.employeeId}), {r.code}: {r.error}</li>)}</ul>}
        </div>}
        {data.leftover > 0 && <p className="mute" style={{ fontSize: 12.5, marginTop: 12 }}>
          {data.leftover} portal employee{data.leftover === 1 ? ' looks' : 's look'} like nobody in the HRMS (perhaps not imported yet).{' '}
          <button className="btn sm" onClick={() => download('/attendance/realtime/match/download?what=portal', 'portal-employees.xlsx').catch((e) => toast(e.message))}>Download the list</button>
        </p>}
      </>}
    </>}
  </div>;
}
