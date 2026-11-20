import { useState } from 'react';
import { Link } from 'react-router-dom';
import { api, inr, fmtD, ymOf, addMonths, today } from '../api';
import { useAuth } from '../auth';
import { Loading, Modal, Pill, Switch, download, toast, useLoad } from '../components/ui';
import EmpSearch, { type EmpHit } from '../components/EmpSearch';
import PersonPicker, { type Person } from '../components/PersonPicker';
import { RecoveryCell, RefundCell, SIZE_KEYS, SIZE_LABEL, issueItems, issueWhen, sizeText, type SizeKey, type UniIssue } from '../components/Uniform';
import { ymLabel } from './PayrollRun';

/* Inventory, assets and uniform (prototype vAst assets / uni / setup and vInv). A central store and one store per HRBP:
   HRBP Corporate adds stock centrally and transfers it to HRBPs; an HRBP issues to employees and takes returns. */

const nmPlain = (name?: string, id?: string) => <>{name} <span className="mono mute">{id}</span></>;
const short = (name: string) => name.replace(/'s store$/, '');
const SEC = { fontSize: 12, fontWeight: 700, background: 'var(--bg,#f6f7f8)' } as const;

type Kit = { id: number; name: string; lines: Record<string, number>; status?: string };
type ULine = { itemId: number; name: string; qty: number; price: number; ret?: number };
/** A uniform issue with its salary plan (GET /inventory/uniform-issues, prototype UNI.issues). */
type Issue = { id: string; employeeId: string; name: string; date: string; lines: ULine[]; total: number; schedule: { ym: string; amount: number }[]; refundYm: string | null; refundStatus?: string };

/** The salary plan of a uniform issue (prototype invDedText). */
const dedText = (u: Issue) => u.schedule.length ? u.schedule.map((s) => `${ymLabel(s.ym)} ${inr(s.amount)}`).join(', ') + (u.refundYm ? `; refund ${ymLabel(u.refundYm)}${u.refundStatus === 'cancelled' ? ' (cancelled: left before it)' : ''}` : '') : '';
/** What a uniform item is issued by (Setup › Uniform items › Size by). */
const SIZE_TYPE: Record<string, string> = { shirt: 'Shirt / T-shirt size', trouser: 'Trouser waist and length', shoes: 'Shoe size', other: 'No size' };
/** "Laptop: Dell Latitude 5440" (an asset added without a name shows its type, as the prototype). */
const astName = (a: any) => a.name || a.type;
/** Items in the Setup order (prototype invItems: uniform items, then asset types, each as created). */
const byItem = (a: any, b: any) => (a.kind === b.kind ? a.id - b.id : a.kind === 'uniform' ? -1 : 1);

/** Every store the user sees (or one), for a month: the first call names the stores, then one call per further store. */
function useInv(ym: string, store = '') {
  return useLoad(async () => {
    const first = await api(`/inventory?ym=${ym}`);
    const ids: string[] = store ? [store] : first.stores.map((s: any) => s.id);
    const list = await Promise.all(ids.map((id) => (id === first.store ? first : api(`/inventory?ym=${ym}&store=${encodeURIComponent(id)}`))));
    const sname = (id: string) => first.stores.find((s: any) => s.id === id)?.name ?? id;
    return { ...first, list, sname };
  }, [ym, store]);
}

function useRole() {
  const { me } = useAuth();
  return { own: me?.ctx.role === 'HRBP' };
}

/** Inventory tab (prototype vInv): month and store, stock summary, movements, and the stock dialogs. */
export default function Inventory() {
  const { own } = useRole();
  const [ym, setYm] = useState(ymOf());
  const [store, setStore] = useState('');
  const d = useInv(ym, store);
  const iss = useLoad(() => api<Issue[]>('/inventory/uniform-issues').catch(() => [] as Issue[]), []);
  const [dlg, setDlg] = useState<'' | 'add' | 'tr' | 'ret' | 'wo'>('');
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, ed = x.canEdit, cen = x.canCentral, one = own || !!store;
  const kind = (name: string) => x.items.find((i: any) => i.name === name)?.kind;
  const price = (id: number) => x.items.find((i: any) => i.id === id)?.price ?? 0;
  const ms = [0, 1, 2, 3, 4, 5].map((i) => addMonths(ymOf(), -i));
  const rows = x.list.flatMap((s: any) => [...s.summary].sort(byItem).filter((r: any) => r.opening || r.additions || r.issued || r.returns || r.out || r.closing).map((r: any) => ({ ...r, store: s.store })));
  const moves = x.list.flatMap((s: any) => s.moves.map((m: any) => ({ ...m, store: s.store }))).filter((m: any) => String(m.date).slice(0, 7) === ym)
    .sort((a: any, b: any) => (a.date < b.date ? 1 : a.date > b.date ? -1 : b.id - a.id)).slice(0, 40);
  const done = () => { setDlg(''); d.reload(); iss.reload(); };
  /** The other store of a transfer: the matching move in another store, else the central store for a transfer in. */
  const other = (m: any) => {
    if (m.kind !== 'Transfer in' && m.kind !== 'Transfer out') return '—';
    const pair = moves.find((y: any) => y.store !== m.store && y.item === m.item && y.qty === m.qty && y.at === m.at && y.kind === (m.kind === 'Transfer in' ? 'Transfer out' : 'Transfer in'));
    return pair ? short(x.sname(pair.store)) : m.kind === 'Transfer in' ? 'Central store' : '—';
  };
  /** Salary deduction of an issue to an employee (prototype vInv ded): the uniform's plan, or none for an asset. */
  const ded = (m: any) => {
    if (m.kind !== 'Issue' || !m.employee_id) return null;
    const u = (iss.data ?? []).find((y) => y.id === m.ref);
    if (u) return <span className="ink2" style={{ fontSize: 12 }}>{dedText(u)}</span>;
    return kind(m.item) === 'asset' ? <span className="mute" style={{ fontSize: 12 }}>None, unless lost or damaged</span> : null;
  };
  return (
    <>
      <div className="row sp" style={{ marginBottom: 12, flexWrap: 'wrap', gap: 8 }}>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          <select value={ym} onChange={(e) => setYm(e.target.value)}>{ms.map((m) => <option key={m} value={m}>{ymLabel(m)}</option>)}</select>
          {own ? <span className="pill info">{x.sname(x.stores[0]?.id)}</span>
            : <select value={store} onChange={(e) => setStore(e.target.value)}><option value="">All stores</option>{x.stores.map((s: any) => <option key={s.id} value={s.id}>{short(s.name)}</option>)}</select>}
        </div>
        <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
          {ed && cen && <><button className="btn" onClick={() => setDlg('add')}>Add stock</button><button className="btn" onClick={() => setDlg('tr')}>Transfer to an HRBP</button></>}
          {ed && <button className="btn" onClick={() => setDlg('ret')}>Return from an employee</button>}
          {ed && (own || cen) && <button className="btn" onClick={() => setDlg('wo')}>Write off</button>}
          <button className="btn primary" onClick={() => download(`/reports/inv_moves/sheet?ym=${ym}`, `inventory-${ym}.xlsx`).catch((e) => toast(e.message))}>Download report</button>
        </div>
      </div>
      <div className="card" style={{ marginBottom: 14 }}>
        <div className="tw"><table><thead><tr>{!one && <th>Store</th>}<th>Item</th><th className="num">Opening</th><th className="num">Added</th><th className="num">Returned</th><th className="num">Issued to employees</th><th className="num">Transferred or written off</th><th className="num">Closing</th><th className="num">Value</th></tr></thead><tbody>
          {rows.length ? rows.map((r: any) => <tr key={r.store + r.id}>{!one && <td>{short(x.sname(r.store))}</td>}<td><b>{r.name}</b></td><td className="num">{r.opening}</td><td className="num">{r.additions || ''}</td><td className="num">{r.returns || ''}</td>
            <td className="num">{r.issued || ''}</td><td className="num">{r.out || ''}</td><td className="num"><b style={r.closing < 0 ? { color: 'var(--bad,#b3261e)' } : undefined}>{r.closing}</b></td><td className="num">{r.price ? inr(r.price * r.closing) : '—'}</td></tr>)
            : <tr><td colSpan={9} className="mute">No stock movement for {ymLabel(ym)}.</td></tr>}
        </tbody></table></div>
        <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Closing = opening + added + returned − issued − transferred or written off. {own ? 'Stock added by HRBP Corporate shows as Added. Issues to employees reduce it, and returns come back into this store.' : 'Added includes stock transferred in from the central store.'} Uniform issues are recovered from salary as set in Setup. Assets are not deducted unless lost or damaged.</p>
      </div>
      <div className="card"><h3>Movements, {ymLabel(ym)}</h3>
        <div className="tw"><table><thead><tr><th>Date</th>{!one && <th>Store</th>}<th>Type</th><th>Item</th><th className="num">Qty</th><th>Employee or other store</th><th>Salary deduction</th><th>By</th></tr></thead><tbody>
          {moves.length ? moves.map((m: any) => <tr key={m.store + m.id}><td>{fmtD(m.date)}</td>{!one && <td>{short(x.sname(m.store))}</td>}
            <td><Pill k={['Addition', 'Transfer in', 'Return'].includes(m.kind) ? 'ok' : m.kind === 'Issue' ? 'info' : 'n'}>{m.kind}</Pill></td>
            <td>{m.item}{m.serials?.length > 0 && <> <span className="mono mute">{m.serials.join(', ')}</span></>}</td><td className="num">{m.qty}</td>
            <td className="ink2">{m.employee ? nmPlain(m.employee, m.employee_id) : other(m)}{m.note && <div className="mute" style={{ fontSize: 12 }}>{m.note}</div>}</td>
            <td>{ded(m)}</td>
            <td className="ink2">{m.by_name}</td></tr>)
            : <tr><td colSpan={8} className="mute">Nothing yet.</td></tr>}
        </tbody></table></div></div>
      {dlg === 'add' && <StockDlg x={x} price={price} onClose={() => setDlg('')} onDone={done} />}
      {dlg === 'tr' && <StockDlg x={x} price={price} transfer onClose={() => setDlg('')} onDone={done} />}
      {dlg === 'ret' && <ReturnDlg onClose={() => setDlg('')} onDone={done} />}
      {dlg === 'wo' && <WriteOff x={x} own={own} onClose={() => setDlg('')} onDone={done} />}
    </>
  );
}

function Stepper({ v, max, onChange }: { v: number; max: number; onChange: (n: number) => void }) {
  const put = (n: number) => { const c = Math.max(0, Math.min(max, Math.floor(n) || 0)); if (n > max) toast(`Only ${max} available`); onChange(c); };
  return <div className="stp"><button type="button" onClick={() => put(v - 1)}>−</button><input type="number" min={0} max={max} value={v || 0} onChange={(e) => put(+e.target.value)} /><button type="button" onClick={() => put(v + 1)}>+</button></div>;
}

/** "Fill uniform from a kit: [kit] × sets [n] Fill · Clear all" (prototype invKitBar). */
function KitBar({ onFill, onClear }: { onFill: (k: Kit, sets: number) => void; onClear: () => void }) {
  const kits = useLoad(() => api<Kit[]>('/inventory/kits').catch(() => [] as Kit[]), []);
  const [kit, setKit] = useState('');
  const [n, setN] = useState('10');
  const list = kits.data ?? [];
  const k = list.find((x) => String(x.id) === kit) ?? list[0];
  return <div className="row" style={{ gap: 8, flexWrap: 'wrap', margin: '10px 0' }}>
    <span className="mute" style={{ fontSize: 12.5 }}>Fill uniform from a kit:</span>
    <select value={k ? String(k.id) : ''} onChange={(e) => setKit(e.target.value)}>{list.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select>
    <span className="mute" style={{ fontSize: 12.5 }}>× sets</span>
    <input type="number" min={1} value={n} onChange={(e) => setN(e.target.value)} style={{ width: 70 }} />
    <button className="btn sm" type="button" onClick={() => k && onFill(k, Math.max(1, Math.floor(+n) || 1))}>Fill</button>
    <button className="btn sm" type="button" onClick={onClear}>Clear all</button>
  </div>;
}

/** Add stock to the central store, or transfer it to an HRBP (prototype invAdd / invTr with invKitBar and invGridHtml). */
function StockDlg({ x, price, transfer, onClose, onDone }: { x: any; price: (id: number) => number; transfer?: boolean; onClose: () => void; onDone: () => void }) {
  const [to, setTo] = useState('');
  const c = useLoad(() => api('/inventory?store=central'), []);
  const h = useLoad(() => (to ? api(`/inventory?store=${encodeURIComponent(to)}`) : Promise.resolve(null)), [to]);
  const [q, setQ] = useState<Record<number, number>>({});
  const [sr, setSr] = useState<Record<number, string>>({});
  const [pick, setPick] = useState<Record<number, string[]>>({});
  const [note, setNote] = useState('');
  const [date, setDate] = useState(today());
  const [busy, setBusy] = useState(false);
  const items: any[] = [...(c.data?.items ?? x.items)].sort(byItem);
  const uni = items.filter((i) => i.kind === 'uniform'), ast = items.filter((i) => i.kind === 'asset');
  const closing = (d: any, id: number) => d?.summary?.find((r: any) => r.id === id)?.closing ?? 0;
  const avail = (d: any, name: string) => (d?.assets ?? []).filter((a: any) => a.type === name && a.status === 'Available');
  const serials = (id: number) => (sr[id] ?? '').split('\n').map((s) => s.trim()).filter(Boolean);
  const nAst = (id: number) => (transfer ? pick[id]?.length ?? 0 : serials(id).length);
  const lineU = uni.filter((i) => (q[i.id] ?? 0) > 0), lineA = ast.filter((i) => nAst(i.id) > 0);
  const units = lineU.reduce((s, i) => s + q[i.id], 0) + lineA.reduce((s, i) => s + nAst(i.id), 0);
  const val = lineU.reduce((s, i) => s + price(i.id) * q[i.id], 0), nItems = lineU.length + lineA.length;
  const cols = transfer ? 5 : 4, toName = to ? x.sname(to) : '';
  const fill = (k: Kit, sets: number) => {
    let cl = 0;
    const nq = { ...q };
    uni.forEach((i) => {
      const per = k.lines[String(i.id)] || 0;
      if (!per) return;
      const want = per * sets, v = Math.min(want, transfer ? closing(c.data, i.id) : 9999);
      nq[i.id] = v;
      if (v < want) cl++;
    });
    setQ(nq);
    if (cl) toast(`Some items are limited by the stock available (${cl})`);
  };
  const go = async () => {
    if (transfer && !to) { toast('Choose the HRBP'); return; }
    if (!date) { toast('Choose the date'); return; }
    for (const i of ast) { const s = serials(i.id); if (!transfer && new Set(s).size !== s.length) { toast('The same serial number is listed twice for ' + i.name); return; } }
    setBusy(true);
    try {
      const lines = [...lineU.map((i) => ({ itemId: i.id, qty: q[i.id] })), ...lineA.map((i) => ({ itemId: i.id, serials: transfer ? pick[i.id] : serials(i.id) }))];
      await api(`/inventory/stock/${transfer ? 'transfer' : 'add'}`, { body: transfer ? { from: 'central', to, date, lines } : { to: 'central', date, lines, note: note.trim() || 'Purchase' } });
      toast(transfer ? `${units} units transferred. They show as Added for ${short(toName)}` : `${units} units added to the central store`); onDone();
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const hrbps = x.stores.filter((s: any) => s.id !== 'central');
  return <Modal wide onClose={onClose}>
    <div className="row sp"><h2>{transfer ? 'Transfer stock to an HRBP' : 'Add stock to the central store'}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <p className="ink2" style={{ fontSize: 13, margin: '6px 0 0' }}>{transfer ? "It leaves the central store and shows as Added in the HRBP's store. You can only send what the central store has." : 'Enter everything you received in one go. Leave an item at 0 to skip it.'}</p>
    <div className="form" style={{ marginTop: 10 }}>
      {transfer ? <>
        <label className="field"><span>Transfer to</span><select value={to} onChange={(e) => setTo(e.target.value)}><option value="">Choose an HRBP</option>{hrbps.map((s: any) => <option key={s.id} value={s.id}>{short(s.name)}</option>)}</select></label>
        <label className="field"><span>Date</span><input type="date" value={date} max={today()} onChange={(e) => setDate(e.target.value)} /></label>
      </> : <>
        <label className="field"><span>Date received</span><input type="date" value={date} max={today()} onChange={(e) => setDate(e.target.value)} /></label>
        <label className="field"><span>Supplier or invoice (optional)</span><input type="text" value={note} onChange={(e) => setNote(e.target.value)} placeholder="For example Kaya Uniforms, INV-2291" /></label>
      </>}
    </div>
    <KitBar onFill={fill} onClear={() => { setQ({}); setSr({}); setPick({}); }} />
    {!c.data ? <Loading error={c.error} /> : <div className="tw" style={{ maxHeight: '48vh', overflow: 'auto' }}><table>
      <thead><tr><th>Item</th><th className="num">{transfer ? 'Central store has' : 'In central store now'}</th>{transfer && <th className="num">{toName ? `${short(toName)} has` : 'HRBP has'}</th>}<th style={{ width: 190 }}>{transfer ? 'Transfer' : 'Add'}</th><th className="num">Value</th></tr></thead>
      <tbody>
        <tr><td colSpan={cols} className="mute" style={SEC}>UNIFORM</td></tr>
        {uni.map((i) => { const have = closing(c.data, i.id); return <tr key={i.id}><td><b>{i.name}</b></td><td className="num">{have}</td>{transfer && <td className="num">{to ? closing(h.data, i.id) : 0}</td>}
          <td><Stepper v={q[i.id] ?? 0} max={transfer ? have : 9999} onChange={(n) => setQ({ ...q, [i.id]: n })} /></td><td className="num mute">{q[i.id] ? inr(price(i.id) * q[i.id]) : '—'}</td></tr>; })}
        <tr><td colSpan={cols} className="mute" style={SEC}>ASSETS</td></tr>
        {ast.map((i) => { const av = avail(c.data, i.name); return <tr key={i.id}><td><b>{i.name}</b>
          {transfer ? <details className="adv" style={{ marginTop: 4 }}><summary>Choose serial numbers ({av.length} available)</summary><div className="chkgrp">{av.length ? av.map((a: any) => <label key={a.id} className="chkl">
            <input type="checkbox" checked={pick[i.id]?.includes(a.serial) ?? false} onChange={(e) => setPick({ ...pick, [i.id]: e.target.checked ? [...(pick[i.id] ?? []), a.serial] : (pick[i.id] ?? []).filter((s) => s !== a.serial) })} />{a.serial}</label>)
            : <span className="mute">None in the central store</span>}</div></details>
            : <details className="adv" style={{ marginTop: 4 }}><summary>Enter serial numbers</summary><textarea rows={3} value={sr[i.id] ?? ''} onChange={(e) => setSr({ ...sr, [i.id]: e.target.value })} placeholder="One serial, IMEI or SIM number per line" style={{ width: '100%' }} /></details>}</td>
          <td className="num">{av.length}</td>{transfer && <td className="num">{to ? avail(h.data, i.name).length : 0}</td>}<td className="num"><b>{nAst(i.id)}</b> {transfer ? 'selected' : 'added'}</td><td className="num mute">—</td></tr>; })}
      </tbody></table></div>}
    <div className="row sp" style={{ marginTop: 14, flexWrap: 'wrap', gap: 8 }}>
      <span className="ink2" style={{ fontSize: 13.5 }}><b>{nItems}</b> item{nItems === 1 ? '' : 's'} · <b>{units}</b> unit{units === 1 ? '' : 's'}{val ? <> · uniform value <b>{inr(val)}</b></> : null}</span>
      <span className="row"><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={!units || busy} onClick={go}>{transfer ? 'Transfer' : 'Add to central store'}</button></span>
    </div>
  </Modal>;
}

/** Return from an employee (prototype invRet / invRetBody / invRetSave): assets with their condition and a recovery from salary,
 *  uniform by item with the quantity returned now. It goes back into the employee's store. */
function ReturnDlg({ emp0, onClose, onDone }: { emp0?: EmpHit; onClose: () => void; onDone: () => void }) {
  const [emp, setEmp] = useState<EmpHit | null>(emp0 ?? null);
  const held = useLoad(() => (emp ? api(`/inventory/employee/${emp.id}`) : Promise.resolve(null)), [emp?.id]);
  const [sel, setSel] = useState<Record<string, { on?: boolean; cond?: string; amt?: string }>>({});
  const [back, setBack] = useState<Record<number, string>>({});
  const [busy, setBusy] = useState(false);
  const as = (held.data?.assets ?? []).filter((a: any) => a.status === 'Issued');
  const un: { itemId: number; name: string; held: number }[] = [];
  (held.data?.uniforms ?? []).forEach((u: any) => (u.lines ?? []).forEach((l: ULine) => {
    const r = un.find((y) => y.itemId === l.itemId) ?? (un.push({ itemId: l.itemId, name: l.name, held: 0 }), un[un.length - 1]);
    r.held += l.qty - (l.ret || 0);
  }));
  const ul = un.filter((u) => u.held > 0);
  const central = held.data?.store === 'central', sname = short(held.data?.storeName ?? '');
  // Recovery is for permanent staff (the server checks it too); a type not known here is left to the server.
  const perm = emp?.type ? emp.type === 'Permanent' : true;
  const put = (k: string, v: object) => setSel({ ...sel, [k]: { ...sel[k], ...v } });
  const save = async () => {
    const pa = as.filter((a: any) => sel['a' + a.id]?.on);
    const lines = ul.map((u) => ({ itemId: u.itemId, qty: Math.min(u.held, Math.max(0, Math.floor(+(back[u.itemId] || 0)))) })).filter((l) => l.qty > 0);
    if (!pa.length && !lines.length) { toast('Choose what was returned'); return; }
    setBusy(true);
    try {
      for (const a of pa) {
        const s = sel['a' + a.id], cond = s.cond ?? 'Good', amt = perm ? +(s.amt || 0) : 0;
        if (cond === 'Lost') await api(`/inventory/assets/${a.id}/lost`, { body: { note: 'Asset not returned', recover: amt } });
        else await api('/inventory/return', { body: { assetId: a.id, damaged: cond === 'Damaged', recover: cond === 'Damaged' ? amt : 0, note: cond } });
      }
      if (lines.length) await api('/inventory/return', { body: { employeeId: emp!.id, lines, note: 'Uniform returned' } });
      toast(central ? 'Return recorded in the central store' : `Return recorded in ${sname}'s store`); onDone();
    } catch (e: any) { toast(e.message); held.reload(); } finally { setBusy(false); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Return from an employee</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form"><div className="field"><span>Employee</span><EmpSearch value={emp} onPick={(e) => { setEmp(e); setSel({}); setBack({}); }} /></div></div>
    {!emp ? <p className="mute" style={{ fontSize: 13 }}>Pick an employee to see what they hold.</p> : !held.data ? <Loading error={held.error} /> : <>
      <p className="ink2" style={{ fontSize: 13, margin: '12px 0 8px' }}>It goes back into {central ? <b>the central store</b> : <><b>{sname}</b>'s store</>}.</p>
      {as.length > 0 && <div className="tw"><table><thead><tr><th></th><th>Asset</th><th>Condition</th><th className="num">Recover from salary (₹)</th></tr></thead><tbody>
        {as.map((a: any) => { const s = sel['a' + a.id] ?? {}; return <tr key={a.id}><td><input type="checkbox" checked={!!s.on} onChange={(e) => put('a' + a.id, { on: e.target.checked })} /></td>
          <td>{a.type}: {astName(a)} <span className="mono mute">{a.serial}</span></td>
          <td><select value={s.cond ?? 'Good'} onChange={(e) => put('a' + a.id, { cond: e.target.value })}><option>Good</option><option>Damaged</option><option>Lost</option></select></td>
          <td className="num"><input type="number" style={{ width: 90 }} placeholder="Optional" value={s.amt ?? ''} onChange={(e) => put('a' + a.id, { amt: e.target.value })} /></td></tr>; })}
      </tbody></table></div>}
      {ul.length > 0 && <><div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Uniform item</th><th className="num">Held</th><th className="num">Returned now</th></tr></thead><tbody>
        {ul.map((u) => <tr key={u.itemId}><td>{u.name}</td><td className="num">{u.held}</td>
          <td className="num"><input type="number" min={0} max={u.held} value={back[u.itemId] ?? '0'} onChange={(e) => setBack({ ...back, [u.itemId]: e.target.value })} style={{ width: 70 }} /></td></tr>)}
      </tbody></table></div><p className="mute" style={{ fontSize: 12 }}>The uniform recovery and refund already scheduled are not changed by a return.</p></>}
      {!as.length && !ul.length ? <p className="mute">This employee holds nothing.</p>
        : <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" disabled={busy} onClick={save}>Record return</button></div>}
    </>}
  </Modal>;
}

/** Write off stock (prototype invWo): a store, an item (uniform or asset type), the quantity and a reason. */
function WriteOff({ x, own, onClose, onDone }: { x: any; own: boolean; onClose: () => void; onDone: () => void }) {
  const sts = own ? [x.stores[0]] : x.stores;
  const items = [...x.items].sort(byItem) as any[];
  const [st, setSt] = useState(sts[0]?.id ?? '');
  const [item, setItem] = useState(String(items[0]?.id ?? ''));
  const [n, setN] = useState('1');
  const [note, setNote] = useState('');
  const go = async () => {
    const q = Math.floor(+n);
    if (!(q > 0) || !note.trim()) { toast('Enter the quantity and a reason'); return; }
    try { await api('/inventory/stock/writeoff', { body: { store: st, lines: [{ itemId: +item, qty: q }], note: note.trim() } }); toast('Written off'); onDone(); } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Write off stock</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form">
      <label className="field"><span>Store</span><select value={st} onChange={(e) => setSt(e.target.value)}>{sts.map((s: any) => <option key={s.id} value={s.id}>{short(s.name)}</option>)}</select></label>
      <label className="field"><span>Item</span><select value={item} onChange={(e) => setItem(e.target.value)}>{items.map((i: any) => <option key={i.id} value={i.id}>{i.name}</option>)}</select></label>
      <label className="field"><span>Quantity</span><input type="number" min={1} value={n} onChange={(e) => setN(e.target.value)} /></label>
      <label className="field"><span>Reason</span><input type="text" value={note} onChange={(e) => setNote(e.target.value)} placeholder="Damaged, worn out, lost in store" /></label>
    </div>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={go}>Write off</button></div>
  </Modal>;
}

/** Assets tab (prototype vAst 'assets'): every asset in the stores the user sees, with issue, return and lost. */
export function Assets() {
  const { own } = useRole();
  const d = useInv(ymOf());
  const [f, setF] = useState('');
  const [iss, setIss] = useState<any>(null);
  const [lost, setLost] = useState<any>(null);
  const [ret, setRet] = useState<EmpHit | null>(null);
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, ed = x.canEdit;
  const list = x.list.flatMap((s: any) => s.assets.map((a: any) => ({ ...a, store: s.store }))).filter((a: any) => !f || a.status === f).sort((a: any, b: any) => a.id - b.id);
  const done = () => { setIss(null); setLost(null); setRet(null); d.reload(); };
  return <>
    <div className="row sp" style={{ marginBottom: 14 }}><select value={f} onChange={(e) => setF(e.target.value)}><option value="">All status</option>{['Available', 'Issued', 'Damaged', 'Lost'].map((s) => <option key={s}>{s}</option>)}</select></div>
    {ed && (x.canAddAsset ?? x.canCentral) && !own && <AddAsset items={x.items.filter((i: any) => i.kind === 'asset').sort(byItem)} onDone={d.reload} />}
    <div className="card"><div className="tw"><table><thead><tr><th>ID</th><th>Type</th><th>Name</th><th>Serial</th><th>Store</th><th>Status</th><th>With</th><th>Issued</th><th></th></tr></thead><tbody>
      {list.map((a: any) => <tr key={a.id}><td className="mono">AST-{a.id}</td><td>{a.type}</td><td>{astName(a)}</td><td className="mono">{a.serial}</td><td className="ink2">{short(x.sname(a.store))}</td>
        <td><Pill k={a.status === 'Issued' ? 'info' : a.status === 'Lost' || a.status === 'Damaged' ? 'bad' : 'ok'}>{a.status}</Pill></td>
        <td>{a.employee ? nmPlain(a.employee, a.employee_id) : '—'}</td><td>{fmtD(a.issued_at)}</td>
        <td>{ed ? a.status === 'Available' ? <button className="btn sm" onClick={() => setIss(a)}>Issue</button>
          : a.status === 'Issued' ? <><button className="btn sm" onClick={() => setRet({ id: a.employee_id, name: a.employee })}>Returned</button> <button className="btn sm" onClick={() => setLost(a)}>Lost</button></> : null : null}</td></tr>)}
    </tbody></table></div>
      <p className="mute" style={{ fontSize: 12, margin: '10px 0 0' }}>Assets are never deleted. An asset stays on record when it is returned, so its history is kept. Final settlement cannot close while an employee holds an asset.</p></div>
    {iss && <IssueAsset a={iss} onClose={() => setIss(null)} onDone={done} />}
    {lost && <LostAsset a={lost} onClose={() => setLost(null)} onDone={done} />}
    {ret && <ReturnDlg emp0={ret} onClose={() => setRet(null)} onDone={done} />}
  </>;
}

/** Add an asset (prototype astAdd): type, name or model, serial. It goes into the central store. */
function AddAsset({ items, onDone }: { items: any[]; onDone: () => void }) {
  const [t, setT] = useState(items[0]?.id ?? 0);
  const [n, setN] = useState('');
  const [s, setS] = useState('');
  const go = async () => {
    if (!n.trim() || !s.trim()) { toast('Enter the name and serial number'); return; }
    try { await api('/inventory/stock/add', { body: { to: 'central', lines: [{ itemId: +t, serials: [s.trim()], name: n.trim() }], note: 'Purchase' } }); toast('Asset added'); setN(''); setS(''); onDone(); } catch (e: any) { toast(e.message); }
  };
  return <div className="card" style={{ marginBottom: 14 }}><h3>Add an asset</h3>
    <div className="form">
      <label className="field"><span>Type</span><select value={t} onChange={(e) => setT(+e.target.value)}>{items.map((i) => <option key={i.id} value={i.id}>{i.name}</option>)}</select></label>
      <label className="field"><span>Name or model</span><input type="text" value={n} onChange={(e) => setN(e.target.value)} /></label>
      <label className="field"><span>Serial number, IMEI or SIM number</span><input type="text" value={s} onChange={(e) => setS(e.target.value)} /></label>
    </div>
    <div className="row" style={{ marginTop: 10 }}><button className="btn primary" onClick={go}>Add asset</button></div></div>;
}

function IssueAsset({ a, onClose, onDone }: { a: any; onClose: () => void; onDone: () => void }) {
  const [emp, setEmp] = useState<EmpHit | null>(null);
  const [date, setDate] = useState(today());
  const go = async () => {
    if (!emp) { toast('Choose the employee'); return; }
    try { await api('/inventory/issue', { body: { store: a.store, employeeId: emp.id, assetId: a.id, date } }); toast('Issued'); onDone(); } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Issue {astName(a)}</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form" style={{ marginTop: 12 }}>
      <div className="field"><span>Employee</span><EmpSearch value={emp} onPick={setEmp} filter="&status=Active" /></div>
      <label className="field"><span>Issue date</span><input type="date" value={date} max={today()} onChange={(e) => setDate(e.target.value)} /></label>
    </div>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={go}>Issue</button></div>
  </Modal>;
}

function LostAsset({ a, onClose, onDone }: { a: any; onClose: () => void; onDone: () => void }) {
  const [amt, setAmt] = useState('');
  const go = async () => {
    try { await api(`/inventory/assets/${a.id}/lost`, { body: { note: 'Asset marked lost', recover: +(amt || 0) } }); toast('Marked lost'); onDone(); } catch (e: any) { toast(e.message); }
  };
  return <Modal onClose={onClose}>
    <div className="row sp"><h2>Mark {astName(a)} lost</h2><button className="btn sm" onClick={onClose}>Close</button></div>
    <div className="form" style={{ marginTop: 12 }}><label className="field"><span>Recovery from salary (₹, optional)</span><input type="number" value={amt} onChange={(e) => setAmt(e.target.value)} /></label></div>
    <p className="mute" style={{ fontSize: 12 }}>Recovered in the open payroll month, for permanent staff only.</p>
    <div className="row" style={{ marginTop: 14, justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>Cancel</button><button className="btn primary" onClick={go}>Save</button></div>
  </Modal>;
}

/** Uniform tab (prototype vAst 'uni'): issue a uniform from a kit with salary recovery as set in Setup, and every uniform issued
 *  with its sizes, recovery and refund. The employee's sizes on record pre-fill the size of each item (by the item's size type,
 *  set in Setup); the size can be changed here. The server issues from the employee's store (their HRBP's, else the central store). */
export function Uniform() {
  const { can } = useAuth();
  const ed = can('emp.assets', 2);
  const d = useLoad(() => Promise.all([api('/inventory/setup'), api<UniIssue[]>('/inventory/uniform-issues')]).then(([setup, issues]) => ({ setup, issues })), []);
  const [emp, setEmp] = useState<Person | null>(null);
  const mine = useLoad(() => (emp ? api(`/employees/${encodeURIComponent(emp.id)}/uniform`).catch(() => null) : Promise.resolve(null)), [emp?.id]);
  const [kit, setKit] = useState('');
  const [date, setDate] = useState(today());
  const [q, setQ] = useState<Record<number, string>>({});
  const [sz, setSz] = useState<Record<number, string>>({});
  const [busy, setBusy] = useState(false);
  if (!d.data) return <Loading error={d.error} />;
  const { setup, issues } = d.data;
  const lists: Record<SizeKey, string[]> = setup.sizes;
  const items = setup.items.filter((i: any) => i.kind === 'uniform' && i.status === 'Active');
  const kits: Kit[] = setup.kits.filter((k: Kit) => k.status !== 'Inactive');
  const k = kits.find((x) => String(x.id) === kit) ?? kits[0];
  const qty = (id: number) => q[id] ?? String(k?.lines[String(id)] ?? 0);
  const has: Record<SizeKey, string> = mine.data?.sizes ?? { shirt: '', waist: '', length: '', shoe: '' };
  /** The size pre-filled for an item from the employee's sizes, by the item's size type. */
  const pre = (i: any) => i.size_type === 'shirt' ? has.shirt : i.size_type === 'shoes' ? has.shoe : i.size_type === 'trouser' ? (has.waist ? has.waist + (has.length ? '/' + has.length : '') : '') : '';
  const size = (i: any) => sz[i.id] ?? pre(i);
  const pick = (id: string, p: Person | null) => { setEmp(id ? p ?? { id, name: id } : null); setSz({}); };
  const go = async () => {
    if (!emp || (emp.type && emp.type !== 'Permanent')) { toast('Uniform recovery applies to permanent staff'); return; }
    const lines = items.map((i: any) => ({ itemId: i.id, qty: Math.floor(+qty(i.id)) || 0, size: size(i) })).filter((l: any) => l.qty > 0);
    if (!lines.length) { toast('Add at least one item'); return; }
    if (!date) { toast('Choose the issue date'); return; }
    setBusy(true);
    try { await api('/inventory/issue', { body: { employeeId: emp.id, lines, date } }); toast('Issued. Recovery scheduled'); setEmp(null); setQ({}); setSz({}); d.reload(); } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const sizeCtl = (i: any) => {
    const v = size(i), set = (x: string) => setSz({ ...sz, [i.id]: x });
    const sel = (l: string[], val: string, on: (x: string) => void, label: string, fmt: (x: string) => string = (x) => x) =>
      <select aria-label={label} value={val} onChange={(e) => on(e.target.value)} style={{ minWidth: 70 }}><option value="">—</option>{(val && !l.includes(val) ? [val, ...l] : l).map((o) => <option key={o} value={o}>{fmt(o)}</option>)}</select>;
    if (i.size_type === 'shirt') return sel(lists.shirt, v, set, `${i.name} size`);
    if (i.size_type === 'shoes') return sel(lists.shoe, v, set, `${i.name} size`, (x) => 'UK ' + x);
    if (i.size_type === 'trouser') {
      const [w, l] = v.split('/');
      return <span className="row" style={{ gap: 4, flexWrap: 'nowrap' }}>{sel(lists.waist, w ?? '', (x) => set(x ? x + (l ? '/' + l : '') : ''), `${i.name} waist`, (x) => 'W ' + x)}
        {sel(lists.length, l ?? '', (x) => set((w ?? '') + (x ? '/' + x : '')), `${i.name} length`, (x) => 'L ' + x)}</span>;
    }
    return <span className="mute">—</span>; // an item without a size (Setup › Uniform items › Size by)
  };
  return <>
    {ed && <div className="card" style={{ marginBottom: 14 }}><h3>Issue a uniform</h3>
      <div className="form">
        <div className="field"><span>Employee (permanent staff)</span><PersonPicker label="Employee" value={emp?.id ?? ''} selected={emp} onChange={pick} search="&type=Permanent&status=Active" placeholder="Find a permanent employee" /></div>
        <label className="field"><span>Kit</span><select value={k ? String(k.id) : ''} onChange={(e) => { setKit(e.target.value); setQ({}); }}>{kits.map((x) => <option key={x.id} value={x.id}>{x.name}</option>)}</select></label>
        <label className="field"><span>Issue date</span><input type="date" value={date} max={today()} onChange={(e) => setDate(e.target.value)} /></label>
      </div>
      {emp && mine.data && <div className="note" data-testid="issue-sizes" style={{ marginTop: 10, fontSize: 13 }}><b>Sizes on record for {mine.data.employee.name}:</b>{' '}
        {SIZE_KEYS.some((x) => has[x]) ? SIZE_KEYS.map((x) => `${SIZE_LABEL[x]} ${has[x] ? sizeText(x, has[x]) : '—'}`).join(' · ') : 'none yet. Choose the sizes below, and add them to the employee record.'}</div>}
      <div className="tw" style={{ marginTop: 10 }}><table><thead><tr><th>Item</th><th>Size</th><th className="num">Price</th><th className="num">Quantity</th></tr></thead><tbody>
        {items.map((i: any) => <tr key={i.id}><td>{i.name}{i.size_type && i.size_type !== 'other' && <div className="mute" style={{ fontSize: 11.5 }}>{SIZE_TYPE[i.size_type]}</div>}</td><td>{sizeCtl(i)}</td><td className="num">{inr(i.price)}</td>
          <td className="num"><input type="number" min={0} aria-label={`${i.name} quantity`} value={qty(i.id)} onChange={(e) => setQ({ ...q, [i.id]: e.target.value })} style={{ width: 70 }} /></td></tr>)}
      </tbody></table></div>
      <p className="mute" style={{ fontSize: 12.5, margin: '8px 0' }}>Recovered from salary: {setup.firstPct}% in the issue month and {100 - setup.firstPct}% in the next month. Refunded in full {setup.rule}, only if the employee is still employed then; someone who leaves earlier gets no refund and the recovery stands.</p>
      <button className="btn primary" disabled={busy} onClick={go}>Issue and schedule recovery</button>
      {can('bulk.update') && <p className="mute" style={{ fontSize: 12.5, margin: '10px 0 0' }}>Uniform given before the HRMS, with recovery already started? Load it in <Link to="/bulk?tpl=openuni">Bulk update › Opening uniform</Link>.</p>}</div>}
    <div className="card"><div className="tw"><table><thead><tr><th>Employee</th><th>Items and sizes</th><th className="num">Total</th><th>Recovery</th><th>Refund</th></tr></thead><tbody>
      {issues.length ? issues.map((u) => <tr key={u.id}><td>{nmPlain(u.name, u.employeeId)}<div className="mute" style={{ fontSize: 12 }}>{issueWhen(u)}</div></td>
        <td className="ink2" style={{ fontSize: 12.5 }}>{issueItems(u)}</td><td className="num">{inr(u.total)}</td>
        <td><RecoveryCell u={u} /></td>
        <td style={{ fontSize: 12.5 }}><RefundCell u={u} /></td></tr>)
        : <tr><td colSpan={5} className="mute">No uniform issued yet.</td></tr>}
    </tbody></table></div></div>
  </>;
}

/** Uniform sizes (Employee dues › Uniform sizes): active employees by size, per item, for ordering stock, with those who have no
 *  sizes on record. Narrowed to a location and a department; downloads as Excel. */
export function UniformSizes() {
  const [loc, setLoc] = useState('');
  const [dept, setDept] = useState('');
  const qs = new URLSearchParams({ ...(loc ? { loc } : {}), ...(dept ? { dept } : {}) }).toString();
  const d = useLoad(() => api(`/inventory/sizes${qs ? '?' + qs : ''}`), [qs]);
  const [all, setAll] = useState(false);
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, miss = x.missing as any[];
  const unit: Record<string, (v: string) => string> = { shirt: (v) => v, waist: (v) => v + ' in', length: (v) => v + ' in', shoe: (v) => 'UK ' + v };
  return <>
    <div className="row sp" style={{ marginBottom: 12, flexWrap: 'wrap', gap: 8 }}>
      <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
        <select aria-label="Location" value={loc} onChange={(e) => setLoc(e.target.value)}><option value="">All locations</option>{(x.locations as any[]).map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}</select>
        <select aria-label="Department" value={dept} onChange={(e) => setDept(e.target.value)}><option value="">All departments</option>{(x.departments as string[]).map((n) => <option key={n}>{n}</option>)}</select>
      </div>
      <button className="btn primary" onClick={() => download(`/inventory/sizes/sheet${qs ? '?' + qs : ''}`, `uniform-sizes-${today()}.xlsx`).catch((e) => toast(e.message))}>Download Excel</button>
    </div>
    <div className="grid g4" style={{ marginBottom: 14 }}>
      <div className="card"><div className="mute" style={{ fontSize: 12 }}>Active employees</div><div style={{ fontSize: 22, fontWeight: 700 }}>{x.total}</div></div>
      <div className="card"><div className="mute" style={{ fontSize: 12 }}>With sizes on record</div><div style={{ fontSize: 22, fontWeight: 700 }}>{x.recorded}</div></div>
      <div className="card"><div className="mute" style={{ fontSize: 12 }}>No sizes yet</div><div style={{ fontSize: 22, fontWeight: 700, color: miss.length ? 'var(--bad,#b3261e)' : undefined }}>{miss.length}</div></div>
    </div>
    <div className="grid g2" style={{ alignItems: 'start', marginBottom: 14 }} data-testid="size-groups">
      {(x.groups as any[]).map((g) => <div key={g.key} className="card"><h3>{g.label}</h3>
        <div className="tw"><table><thead><tr><th>Size</th><th className="num">Employees</th></tr></thead><tbody>
          {(g.rows as any[]).map((r) => <tr key={r.size}><td>{unit[g.key](r.size)}</td><td className="num">{r.count || <span className="mute">0</span>}</td></tr>)}
          <tr><td className="mute">Not recorded</td><td className="num mute">{g.notRecorded}</td></tr>
        </tbody></table></div></div>)}
    </div>
    <div className="card" style={{ marginBottom: 14 }}><h3>Trousers by waist and length</h3>
      {(x.trousers as any[]).length ? <div className="tw"><table><thead><tr><th>Waist</th><th>Length</th><th className="num">Employees</th></tr></thead><tbody>
        {(x.trousers as any[]).map((t) => <tr key={t.waist + '/' + t.length}><td>{t.waist} in</td><td>{t.length === '—' ? <span className="mute">not recorded</span> : t.length + ' in'}</td><td className="num">{t.count}</td></tr>)}
      </tbody></table></div> : <p className="mute" style={{ margin: 0 }}>No trouser sizes on record.</p>}</div>
    <div className="card"><h3>No sizes on record ({miss.length})</h3>
      {miss.length ? <><div className="tw"><table><thead><tr><th>Employee</th><th>Department</th><th>Location</th></tr></thead><tbody>
        {(all ? miss : miss.slice(0, 25)).map((m) => <tr key={m.id}><td>{nmPlain(m.name, m.id)}<div className="mute" style={{ fontSize: 12 }}>{m.designation}</div></td><td>{m.department}</td><td>{m.location}</td></tr>)}
      </tbody></table></div>{miss.length > 25 && <button className="btn sm" style={{ marginTop: 8 }} onClick={() => setAll(!all)}>{all ? 'Show fewer' : `Show all ${miss.length}`}</button>}
        <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }}>Add sizes on the employee record (Employees, open the person, Uniform sizes), or for many people at once with Bulk update › Uniform sizes.</p></>
        : <p className="mute" style={{ margin: 0 }}>Everyone here has sizes on record.</p>}</div>
  </>;
}

/** Setup tab (prototype vAst 'setup', HR Admin): uniform recovery, uniform items with prices, kits, and asset types. Items and types
 *  are switched off, never deleted. Each change saves as it is made. */
export function InvSetup() {
  const d = useLoad(() => api('/inventory/setup'), []);
  const [ni, setNi] = useState({ name: '', price: '' });
  const [nk, setNk] = useState('');
  const [na, setNa] = useState('');
  if (!d.data) return <Loading error={d.error} />;
  const x = d.data, ed = !!x.canEdit;
  const uni = x.items.filter((i: any) => i.kind === 'uniform'), ast = x.items.filter((i: any) => i.kind === 'asset');
  const run = async (p: Promise<unknown>, msg = 'Saved') => {
    let ok = false;
    try { await p; ok = true; if (msg) toast(msg); } catch (e: any) { toast(e.message); }
    d.reload();
    return ok;
  };
  const cfg = (patch: object) => run(api('/inventory/setup', { method: 'PUT', body: { firstPct: x.firstPct, refundAfter: x.refundAfter, refundBasis: x.refundBasis, serviceMonths: x.serviceMonths, ...patch } }));
  const svc = x.refundBasis === 'service';
  const item = (i: any, patch: object) => run(api(`/inventory/items/${i.id}`, { method: 'PATCH', body: patch }));
  const onOff = (i: any) => ed ? <Switch on={i.status === 'Active'} onChange={() => item(i, { status: i.status === 'Active' ? 'Inactive' : 'Active' })} />
    : <Pill k={i.status === 'Active' ? 'ok' : 'n'}>{i.status === 'Active' ? 'Active' : 'Off'}</Pill>;
  const kitQty = (kk: Kit, id: number, v: string) => {
    const n = Math.max(0, Math.floor(+v) || 0);
    if (n === (kk.lines[String(id)] || 0)) return;
    run(api(`/inventory/kits/${kk.id}`, { method: 'PUT', body: { name: kk.name, status: kk.status || 'Active', lines: { ...kk.lines, [String(id)]: n } } }));
  };
  const addItem = () => {
    const name = ni.name.trim(), price = +ni.price;
    if (!name || !price) { toast('Enter the name and price'); return; }
    run(api('/inventory/items', { body: { kind: 'uniform', name, price } }), '').then((ok) => ok && setNi({ name: '', price: '' }));
  };
  const addKit = () => { const name = nk.trim(); if (!name) { toast('Name the kit'); return; } run(api('/inventory/kits', { body: { name, lines: {} } }), '').then((ok) => ok && setNk('')); };
  const addType = () => { const name = na.trim(); if (!name) return; run(api('/inventory/items', { body: { kind: 'asset', name } }), '').then((ok) => ok && setNa('')); };
  return <>
    <div className="card" style={{ marginBottom: 14 }}><h3>Uniform recovery</h3>
      <div className="form">
        <label className="field"><span>Recover in the issue month (%)</span><input type="number" min={0} max={100} key={'p' + x.firstPct} defaultValue={x.firstPct} disabled={!ed}
          onBlur={(e) => { const v = Math.max(0, Math.min(100, +e.target.value || 0)); if (v !== x.firstPct) cfg({ firstPct: v }); }} /></label>
        <label className="field"><span>Refund basis</span><select value={x.refundBasis} disabled={!ed} onChange={(e) => cfg({ refundBasis: e.target.value })}>
          <option value="deduction">After the last deduction</option><option value="service">After months of employment</option></select></label>
        {svc ? <label className="field"><span>Refund after (months of employment)</span><input type="number" min={1} max={60} key={'s' + x.serviceMonths} defaultValue={x.serviceMonths} disabled={!ed}
            onBlur={(e) => { const v = Math.max(1, Math.min(60, Math.floor(+e.target.value) || 1)); if (v !== x.serviceMonths) cfg({ serviceMonths: v }); }} /></label>
          : <label className="field"><span>Refund after (months from the last deduction)</span><select value={x.refundAfter} disabled={!ed} onChange={(e) => cfg({ refundAfter: +e.target.value })}><option value={6}>6 months</option><option value={12}>12 months</option></select></label>}
      </div>
      <p className="mute" style={{ fontSize: 12, margin: '8px 0 0' }} data-testid="refund-rule">The rest is recovered in the next month. Recovery applies to permanent staff only. The full amount is refunded {x.rule}{svc ? ' (in the month the employee completes them, or the month after the last deduction if that is later)' : ''}, only if the employee is still employed then. Someone who leaves earlier gets no refund and the recovery stands. A change applies to uniform issued from now on.</p></div>
    <SizeLists x={x} ed={ed} onSaved={d.reload} />
    <div className="card" style={{ marginBottom: 14 }}><h3>Uniform items</h3><div className="tw"><table><thead><tr><th>Item</th><th>Size by</th><th className="num">Price</th><th>Active</th></tr></thead><tbody>
      {uni.map((i: any) => <tr key={i.id}><td><b>{i.name}</b></td>
        <td>{ed ? <select aria-label={`${i.name} size by`} value={i.size_type || 'other'} onChange={(e) => item(i, { sizeType: e.target.value })}>{Object.entries(SIZE_TYPE).map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
          : SIZE_TYPE[i.size_type || 'other']}</td>
        <td className="num">{ed ? <input type="number" key={'u' + i.id + i.price} defaultValue={i.price} style={{ width: 90 }} onBlur={(e) => { const v = +e.target.value || 0; if (v !== i.price) item(i, { price: v }); }} /> : inr(i.price)}</td>
        <td>{onOff(i)}</td></tr>)}
    </tbody></table></div>
      {ed && <div className="row" style={{ marginTop: 10 }}><input type="text" placeholder="New item name" style={{ maxWidth: 220 }} value={ni.name} onChange={(e) => setNi({ ...ni, name: e.target.value })} />
        <input type="number" placeholder="Price" style={{ maxWidth: 110 }} value={ni.price} onChange={(e) => setNi({ ...ni, price: e.target.value })} /><button className="btn" onClick={addItem}>Add item</button></div>}</div>
    <div className="card" style={{ marginBottom: 14 }}><h3>Kits</h3><div className="tw"><table><thead><tr><th>Kit</th>{uni.map((i: any) => <th key={i.id} className="num">{i.name}</th>)}</tr></thead><tbody>
      {x.kits.map((kk: Kit) => <tr key={kk.id}><td><b>{kk.name}</b></td>{uni.map((i: any) => <td key={i.id} className="num">{ed
        ? <input type="number" min={0} key={`k${kk.id}-${i.id}-${kk.lines[String(i.id)] || 0}`} defaultValue={kk.lines[String(i.id)] || 0} style={{ width: 64 }} onBlur={(e) => kitQty(kk, i.id, e.target.value)} />
        : kk.lines[String(i.id)] || 0}</td>)}</tr>)}
    </tbody></table></div>
      {ed && <div className="row" style={{ marginTop: 10 }}><input type="text" placeholder="New kit name" style={{ maxWidth: 220 }} value={nk} onChange={(e) => setNk(e.target.value)} /><button className="btn" onClick={addKit}>Add kit</button></div>}</div>
    <div className="card"><h3>Asset types</h3><div className="tw"><table><tbody>
      {ast.map((i: any) => <tr key={i.id}><td><b>{i.name}</b></td><td>{onOff(i)}</td></tr>)}
    </tbody></table></div>
      {ed && <div className="row" style={{ marginTop: 10 }}><input type="text" placeholder="New asset type" style={{ maxWidth: 220 }} value={na} onChange={(e) => setNa(e.target.value)} /><button className="btn" onClick={addType}>Add type</button></div>}</div>
  </>;
}

/** Size lists (Setup, HR Admin): the sizes offered on the employee record, the joining form, bulk upload and when issuing uniform.
 *  A size taken off a list stays on the records that have it. */
function SizeLists({ x, ed, onSaved }: { x: any; ed: boolean; onSaved: () => void }) {
  const [v, setV] = useState<Record<string, string>>(() => Object.fromEntries(SIZE_KEYS.map((k) => [k, (x.sizes[k] as string[]).join(', ')])));
  const [busy, setBusy] = useState(false);
  const dirty = SIZE_KEYS.some((k) => v[k] !== (x.sizes[k] as string[]).join(', '));
  const save = async () => {
    setBusy(true);
    try {
      await api('/inventory/size-lists', { method: 'PUT', body: Object.fromEntries(SIZE_KEYS.map((k) => [k, v[k].split(',').map((y) => y.trim()).filter(Boolean)])) });
      toast('Size lists saved'); onSaved();
    } catch (e: any) { toast(e.message); } finally { setBusy(false); }
  };
  const hint: Record<SizeKey, string> = { shirt: 'For example XS, S, M, L, XL', waist: 'Inches, for example 28, 30, 32', length: 'Inches, for example 28, 30, 32', shoe: 'UK sizes, for example 6, 7, 8' };
  return <div className="card" style={{ marginBottom: 14 }}><h3>Uniform sizes</h3>
    <p className="mute" style={{ fontSize: 12.5, marginTop: -4 }}>The sizes offered on the employee record, the joining form, bulk upload and when issuing uniform. Separate sizes with commas.</p>
    <div className="form">{SIZE_KEYS.map((k) => <label key={k} className="field"><span>{SIZE_LABEL[k]}{k === 'shoe' ? ' (UK)' : k === 'shirt' ? '' : ' (inches)'}</span>
      <input type="text" aria-label={`${SIZE_LABEL[k]} sizes`} value={v[k]} disabled={!ed} placeholder={hint[k]} onChange={(e) => setV({ ...v, [k]: e.target.value })} /></label>)}</div>
    {ed && <div className="row" style={{ marginTop: 10 }}><button className="btn" disabled={!dirty || busy} onClick={save}>Save size lists</button>
      <span className="mute" style={{ fontSize: 12 }}>A size taken off a list stays on the records that have it.</span></div>}
  </div>;
}
