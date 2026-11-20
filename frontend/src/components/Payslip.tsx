import { useEffect, useState } from 'react';
import { session, fmtD } from '../api';

/** Rupees with the paise when there are any, so the payslip shows the figure paid to the bank (ITM-59). */
const inr = (n: unknown) => {
  if (typeof n !== 'number') return n == null ? '—' : String(n);
  const v = Math.round(n * 100) / 100, frac = Math.abs(v % 1) > 0.0001;
  return '₹' + v.toLocaleString('en-IN', { minimumFractionDigits: frac ? 2 : 0, maximumFractionDigits: 2 });
};

type Line = { label: string; amount: number; note?: string };
export type SlipView = {
  ym: string; label: string; preview?: boolean; logo?: string | null;
  setup: { accent: string; footer: string; showEmployer: boolean; website?: string; email?: string; phone?: string; showIds?: boolean };
  snap: {
    days: number; paid: number; lop: number; outside?: number; earn: Line[]; ded: Line[]; er: Line[]; totE: number; totD: number; roundOff?: number; net: number; words: string;
    emp: Record<string, string>; company: Record<string, string>;
  };
};

/** Loads a stored image as a blob URL (the token travels in a header, so a plain src cannot be used). */
export function useDocUrl(id?: string | null) {
  const [url, setUrl] = useState('');
  useEffect(() => {
    if (!id) { setUrl(''); return; }
    let u = '';
    fetch(`/api/files/${id}`, { headers: { Authorization: `Bearer ${session.token}`, 'X-Role': session.role } })
      .then((r) => (r.ok ? r.blob() : null)).then((b) => { if (b) { u = URL.createObjectURL(b); setUrl(u); } });
    return () => { if (u) URL.revokeObjectURL(u); };
  }, [id]);
  return url;
}

/** The prototype's payslip style sheet (slipCss), scoped to .sl, with the accent colour; plus the print view. */
const slipCss = (a: string) => `.sl{font-family:Inter,Segoe UI,Arial,sans-serif;color:#1b1f23;max-width:820px;margin:0 auto;background:#fff;border:1px solid #e3e6ea;border-radius:10px;overflow:hidden;font-size:13px}.sl *{box-sizing:border-box}.sl-top{height:6px;background:${a}}.sl-h{display:flex;justify-content:space-between;gap:16px;padding:22px 28px 16px;align-items:flex-start}.sl-brand{display:flex;gap:14px;align-items:center}.sl-brand img{max-height:54px;max-width:150px;object-fit:contain}.sl-mark{width:48px;height:48px;border-radius:10px;background:${a};color:#fff;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:20px}.sl-co{font-size:17px;font-weight:700}.sl-ad{color:#5d6670;font-size:12px;line-height:1.5}.sl-ti{text-align:right}.sl-pt{font-size:11px;letter-spacing:.18em;color:${a};font-weight:700}.sl-mo{font-size:18px;font-weight:700;margin-top:2px}.sl-id{display:grid;grid-template-columns:repeat(4,1fr);gap:12px 18px;padding:14px 28px;background:#f7f8fa;border-top:1px solid #e3e6ea;border-bottom:1px solid #e3e6ea}.sl-id div span{display:block;font-size:10.5px;text-transform:uppercase;letter-spacing:.06em;color:#7a838c}.sl-id div b{font-weight:600}.sl table{width:100%;border-collapse:collapse}.sl-tb{padding:6px 28px 0}.sl-tb th{font-size:11px;text-transform:uppercase;letter-spacing:.06em;color:${a};text-align:left;padding:12px 8px 8px;border-bottom:2px solid ${a}}.sl-tb th.r,.sl-tb td.r{text-align:right}.sl-tb td{padding:7px 8px;border-bottom:1px solid #eef0f2;vertical-align:top}.sl-tb td small{display:block;color:#7a838c;font-size:11px}.sl-tb tr.t td{font-weight:700;background:#f7f8fa;border-bottom:0}.sl-gap{width:20px;border:0!important}.sl-net{display:flex;justify-content:space-between;align-items:center;margin:18px 28px 6px;padding:16px 20px;border-radius:10px;background:${a};color:#fff}.sl-net b{font-size:24px}.sl-w{padding:6px 28px 0;color:#5d6670;font-size:12px}.sl-er{padding:10px 28px 0;color:#5d6670;font-size:12px}.sl-ft{padding:16px 28px 20px;color:#7a838c;font-size:11px;border-top:1px solid #eef0f2;margin-top:16px;display:flex;justify-content:space-between;gap:12px;flex-wrap:wrap}@media print{.sl{border:0}body.printing-slip .sl,body.printing-slip .sl *{visibility:visible}body.printing-slip .sl{position:absolute;left:0;top:0;max-width:none;width:100%}}`;

const mask = (v?: string) => (v ? String(v).replace(/.(?=.{4})/g, '•') : '—');
/** "HDFC Bank XXXXXXXXXX7977" as the prototype shows it: "HDFC Bank ••••7977". */
const bankMask = (v: string) => v.replace(/[X•*]{2,}(?=\w{4}$)/, '••••');

/** The payslip (prototype slipHtml): company header, employee details, earnings and deductions, net pay and words. */
export default function Payslip({ v }: { v: SlipView }) {
  const s = v.snap, co = s.company || {}, e = s.emp || {};
  const logo = useDocUrl(v.logo);
  const accent = v.setup.accent || '#0f6e6e';
  const name = co.legalName || co.name || '';
  const n = Math.max(s.earn.length, s.ded.length);
  const contact = [v.setup.email, v.setup.phone].filter(Boolean).join(' · ');
  const ids = [co.cin && 'CIN ' + co.cin, co.pan && 'PAN ' + co.pan, co.tan && 'TAN ' + co.tan, co.pf && 'PF ' + co.pf, co.esi && 'ESI ' + co.esi].filter(Boolean);
  const cell = (x?: Line) => x ? <><td>{x.label}{x.note && <small>{x.note}</small>}</td><td className="r">{inr(x.amount)}</td></> : <><td /><td /></>;
  const id: [string, string][] = [['Employee', e.name], ['Employee ID', e.id], ['Designation', e.desig], ['Department', e.dept], ['Location', `${e.location}, ${e.city}`],
    ['Date of joining', e.doj ? fmtD(e.doj) : '—'], ['PAN', mask(e.pan)], ['Bank', e.bank ? bankMask(e.bank) : '—'], ['Days in month', String(s.days)], ['Paid days', String(s.paid)],
    ['Loss of pay days', String(s.lop)], ...(s.outside ? [['Days before joining or after leaving', String(s.outside)] as [string, string]] : []), ['Employment type', e.type || '—']];
  return (
    <div className="sl">
      <style>{slipCss(accent)}</style>
      <div className="sl-top" />
      <div className="sl-h">
        <div className="sl-brand">
          {logo ? <img src={logo} alt="" /> : <div className="sl-mark">{(name || 'S').trim().charAt(0).toUpperCase()}</div>}
          <div><div className="sl-co">{name}</div><div className="sl-ad">{co.address}{contact && <><br />{contact}</>}{v.setup.showIds !== false && ids.length > 0 && <><br />{ids.join(' · ')}</>}</div></div>
        </div>
        <div className="sl-ti"><div className="sl-pt">PAYSLIP</div><div className="sl-mo">{v.label}</div></div>
      </div>
      <div className="sl-id">{id.map(([k, x]) => <div key={k}><span>{k}</span><b>{x}</b></div>)}</div>
      <div className="sl-tb"><table>
        <thead><tr><th>Earnings</th><th className="r">Amount (₹)</th><th className="sl-gap" /><th>Deductions</th><th className="r">Amount (₹)</th></tr></thead>
        <tbody>
          {Array.from({ length: n }, (_, i) => <tr key={i}>{cell(s.earn[i])}<td className="sl-gap" />{cell(s.ded[i])}</tr>)}
          <tr className="t"><td>Total earnings</td><td className="r">{inr(s.totE)}</td><td className="sl-gap" /><td>Total deductions</td><td className="r">{inr(s.totD)}</td></tr>
          {!!s.roundOff && <tr><td colSpan={3} /><td>Rounding off<small>Net pay is paid in whole rupees</small></td><td className="r">{(s.roundOff > 0 ? '+' : '−') + inr(Math.abs(s.roundOff))}</td></tr>}
        </tbody>
      </table></div>
      <div className="sl-net"><div>Net pay for the month</div><b>{inr(s.net)}</b></div>
      <div className="sl-w">{s.words}</div>
      {v.setup.showEmployer && s.er.length > 0 && <div className="sl-er">Employer contributions, not part of your net pay: {s.er.map((x) => `${x.label} ${inr(x.amount)}`).join(' · ')}</div>}
      <div className="sl-ft"><span>{v.setup.footer}</span>{v.setup.website && <span>{v.setup.website}</span>}</div>
    </div>
  );
}

/** Opens the payslip in a print view (save as PDF from the browser's print dialog). */
export function printSlip() {
  document.body.classList.add('printing-slip');
  window.print();
  setTimeout(() => document.body.classList.remove('printing-slip'), 500);
}

/** Saves a rendered payslip (an element holding one .sl) as a standalone HTML file, as the prototype's Download does. */
export async function saveSlipHtml(root: HTMLElement, fileName: string, title: string) {
  const el = root.querySelector('.sl')?.cloneNode(true) as HTMLElement | undefined;
  if (!el) return;
  for (const img of Array.from(el.querySelectorAll('img'))) {
    try {
      const b = await (await fetch(img.src)).blob();
      img.src = await new Promise<string>((ok) => { const r = new FileReader(); r.onload = () => ok(String(r.result)); r.readAsDataURL(b); });
    } catch { img.remove(); }
  }
  const esc = (t: string) => t.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c] as string));
  const html = `<!doctype html><html><head><meta charset="utf-8"><title>${esc(title)}</title><style>body{margin:24px;background:#f3f4f6}@media print{body{margin:0;background:#fff}}</style></head><body>${el.outerHTML}<p style="text-align:center;font:12px Arial;color:#6b7280">Use your browser's Print and choose Save as PDF to keep a PDF copy.</p></body></html>`;
  const url = URL.createObjectURL(new Blob([html], { type: 'text/html' }));
  const a = document.createElement('a');
  a.href = url; a.download = fileName; a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
