import { useState } from 'react';
import { postForm } from '../api';
import { useAuth } from '../auth';
import { download, toast } from '../components/ui';

type Change = { field: string; from?: string; to?: string; note?: string };
type Row = { line: number; name: string; kind: 'new' | 'changed' | 'same' | 'error'; errors: string[]; changes: Change[] };
type Sheet = { key: string; sheet: string; new: number; changed: number; same: number; errors: number; note?: string; refused?: boolean; rows: Row[] };
type Result = { applied: boolean; changes: number; errors: number; sheets: Sheet[]; missing: string[] };

// The workbook's sheets, in the order they are read (a parent before what belongs to it).
const ORG = ['Regions', 'States', 'Cities', 'Clusters', 'Locations'];
const JOB = ['Departments', 'Grades', 'Designations', 'Shifts', 'Qualifications', 'Employment types', 'Attendance modes', 'Change reasons', 'Roles', 'Skills'];

/** "Added", "Name: North → North India", "Marked inactive from 12 Nov 2026 · Discontinued". */
const changeText = (c: Change) => (c.from || c.to ? `${c.field}: ${c.from || '—'} → ${c.to || '—'}` : c.field) + (c.note ? ` · ${c.note}` : '');
const n = (k: number, one: string, many = one + 's') => `${k} ${k === 1 ? one : many}`;
// A long message wraps inside its pill.
const WRAP = { whiteSpace: 'normal', borderRadius: 8, lineHeight: 1.45 } as const;

/** Masters › Master update: one workbook with a sheet per master, uploaded once, checked, then applied. */
export default function MasterUpdate() {
  const { can } = useAuth();
  const sheets = [...(can('mas.org', 2) ? ORG : []), ...(can('mas.job', 2) ? JOB : [])];
  const [file, setFile] = useState<File | null>(null);
  const [res, setRes] = useState<Result | null>(null);
  const [busy, setBusy] = useState(false);
  const dl = (data: boolean) => download(`/master-update/download${data ? '?data=1' : ''}`, data ? 'master-update.xlsx' : 'master-update-format.xlsx').catch((e) => toast(e.message));
  const run = async (action: 'check' | 'apply') => {
    if (!file) return;
    setBusy(true);
    try {
      const r = await postForm<Result>(`/master-update/${action}`, {}, file);
      setRes(r);
      if (action === 'apply') toast(`${n(r.changes, 'change')} applied${r.errors ? `. ${n(r.errors, 'row')} with errors skipped` : ''}`);
    } catch (x: any) { toast(x.message); setRes(null); } finally { setBusy(false); }
  };
  const choose = (f: File | undefined) => {
    if (!f) return;
    setRes(null);
    if (!/\.xlsx$/i.test(f.name)) { toast('Choose the Excel workbook (.xlsx). A CSV file holds one sheet only'); return; }
    setFile(f);
  };
  return (
    <>
      <div className="grid split">
        <div className="card">
          <h3>Update many masters at once</h3>
          <p className="ink2" style={{ marginTop: 0 }}>Download one workbook with a sheet for each master you can change, fill it in offline and upload it here.
            Every row is checked with the same rules as the lists, and nothing changes until you apply it.</p>
          <div className="row" style={{ margin: '12px 0' }}>
            <button className="btn primary" onClick={() => dl(true)}>Download with current data</button>
            <button className="btn" onClick={() => dl(false)}>Download blank format</button>
          </div>
          <p className="mute" style={{ fontSize: 12.5, margin: '0 0 12px' }}>Sheets: {sheets.join(', ')}.</p>
          <div className="upl">
            <label className="btn">Choose Excel file<input type="file" accept=".xlsx" hidden onChange={(e) => { choose(e.target.files?.[0]); e.target.value = ''; }} /></label>
            {file ? <><span className="ink2" style={{ fontSize: 13 }}>{file.name}</span><button className="btn sm" onClick={() => { setFile(null); setRes(null); }}>Remove</button></>
              : <span className="mute" style={{ fontSize: 12.5 }}>No file chosen</span>}
          </div>
          <div className="row" style={{ marginTop: 12 }}>
            <button className="btn primary" disabled={!file || busy} onClick={() => run('check')}>Check file</button>
            {res && !res.applied && <button className="btn primary" disabled={!res.changes || busy} onClick={() => run('apply')}>Apply {n(res.changes, 'change')}</button>}
          </div>
        </div>
        <div className="card"><h3>How master update works</h3>
          <ul className="ink2" style={{ paddingLeft: 18, margin: 0, display: 'grid', gap: 8, fontSize: 13 }}>
            <li>Sheets are read in order, so a region added on Regions can be used by a new state on States in the same file.</li>
            <li>Keep the Row ID to change an item, its name too. Leave it blank for a new item.</li>
            <li>Give parents by name; where a name repeats, write “Name (Parent)”, like “Aurangabad (Bihar)”.</li>
            <li>A sheet or row left out changes nothing. Set Status to Inactive, with a date and a reason, to retire an item.</li>
            <li>A rename updates every record that uses the name. An employment type's name and a shift's code cannot change.</li>
            <li>Budgets are in Bulk update › Budgets, and a shift's attendance rules in Set shift.</li>
          </ul>
        </div>
      </div>
      {res && <CheckResult res={res} />}
    </>
  );
}

function CheckResult({ res }: { res: Result }) {
  const counted = res.sheets.filter((s) => !s.refused);
  return (
    <div className="card" style={{ marginTop: 14 }}>
      <div className="row sp" style={{ marginBottom: 10 }}><h3 style={{ margin: 0 }}>{res.applied ? 'Applied' : 'Check result'}</h3>
        <span className="ink2" style={{ fontSize: 13 }}>{n(res.changes, 'change')} · {n(res.errors, 'row')} with errors</span></div>
      {res.applied && <div className="note ok" style={{ marginBottom: 12 }}>{n(res.changes, 'change')} applied.{res.errors ? ' The rows with errors below were skipped: correct them in the file and upload it again.' : ''}</div>}
      {!res.applied && !!res.errors && !!res.changes && <div className="note warn" style={{ marginBottom: 12 }}>Rows with errors are skipped when you apply; the other {n(res.changes, 'change')} go ahead.</div>}
      <div className="tw"><table>
        <thead><tr><th>Sheet</th><th className="num">New</th><th className="num">Changed</th><th className="num">Errors</th><th className="num">No change</th></tr></thead>
        <tbody>{res.sheets.map((s) => <tr key={s.key}>
          <td><b>{s.sheet}</b>{s.note && <div className={s.refused || s.errors ? 'note bad' : 'mute'} style={{ fontSize: 12, marginTop: 4, padding: s.refused || s.errors ? '6px 8px' : 0 }}>{s.note}</div>}</td>
          <td className="num">{s.refused ? '—' : s.new}</td><td className="num">{s.refused ? '—' : s.changed}</td>
          <td className="num">{s.errors ? <span className="pill bad">{s.errors}</span> : s.refused ? '—' : 0}</td><td className="num">{s.refused ? '—' : s.same}</td>
        </tr>)}</tbody>
      </table></div>
      {res.missing.length > 0 && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Not in the file, so nothing in them changes: {res.missing.join(', ')}.</p>}
      {counted.filter((s) => s.rows.length).map((s) => <div key={s.key} style={{ marginTop: 16 }}>
        <h3 style={{ margin: '0 0 6px' }}>{s.sheet}</h3>
        <div className="tw"><table>
          <thead><tr><th style={{ width: 56 }}>Row</th><th style={{ width: '26%' }}>Name</th><th>Result</th></tr></thead>
          <tbody>{s.rows.map((r) => <tr key={r.line}>
            <td className="mono">{r.line}</td>
            <td><b>{r.name}</b>{r.kind === 'new' && <> <span className="pill info">New</span></>}</td>
            <td><div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>{r.errors.length ? r.errors.map((e, j) => <span key={j} className="pill bad" style={WRAP}>{e}</span>)
              : r.changes.map((c, j) => <span key={j} className="pill ok" style={WRAP}>{changeText(c)}</span>)}</div></td>
          </tr>)}</tbody>
        </table></div>
        {s.same > 0 && <p className="mute" style={{ fontSize: 12, margin: '6px 0 0' }}>{n(s.same, 'more row')} with no change.</p>}
      </div>)}
    </div>
  );
}
