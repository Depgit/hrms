import { useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { useParams } from 'react-router-dom';
import { fmtD, today } from '../api';
import { toast } from '../components/ui';

/* The new joiner's own page (joining_link.go), opened from the link HR sends: no sign-in. A 6-digit code confirms
   it is them, then they fill their joining details, upload their documents (photos from the phone camera work) and
   submit to HR. Built for phones first. */

type Field = { key: string; label: string; section: string; kind: string; options: string[]; required: boolean; onlyIf?: Record<string, string> };
type Doc = { key: string; label: string; required: boolean; uploaded: { fileName: string; uploadedAt: string } | null };
type Form = { company: string; name: string; offerNo: string; type: string; doj: string; note: string | null; editable: boolean; submittedAt: string | null; fields: Field[]; values: Record<string, string>; docs: Doc[] };
type Status = { company: string; state: 'code' | 'submitted'; codeBy: 'email' | 'phone'; codeTo: string; codeOut?: boolean; hrName?: string; askHR?: boolean };
type Data = Record<string, string>;

class JoinError extends Error {
  status: number; details?: string[];
  constructor(status: number, message: string, details?: string[]) { super(message); this.status = status; this.details = details; }
}

// This tab keeps the session, and what the joiner typed but has not saved yet (so a session that ends loses nothing).
const sessKey = (t: string) => 'hrms.join.' + t.slice(0, 12);
const draftKey = (t: string) => 'hrms.joindraft.' + t.slice(0, 12);
const getSess = (t: string) => { try { return sessionStorage.getItem(sessKey(t)) || ''; } catch { return ''; } };
const setSess = (t: string, v: string) => { try { if (v) sessionStorage.setItem(sessKey(t), v); else sessionStorage.removeItem(sessKey(t)); } catch { /* private mode */ } };
const getDraft = (t: string): Data => { try { return JSON.parse(sessionStorage.getItem(draftKey(t)) || '{}') || {}; } catch { return {}; } };
const setDraft = (t: string, v: Data) => { try { if (Object.keys(v).length) sessionStorage.setItem(draftKey(t), JSON.stringify(v)); else sessionStorage.removeItem(draftKey(t)); } catch { /* private mode */ } };

async function call<T = any>(token: string, path: string, opts: { method?: string; body?: unknown; form?: FormData } = {}): Promise<T> {
  const headers: Record<string, string> = {};
  const s = getSess(token);
  if (s) headers['X-Join-Session'] = s;
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(`/api/join/${token}${path}`, {
    method: opts.method || (opts.body !== undefined || opts.form ? 'POST' : 'GET'), headers,
    body: opts.form ?? (opts.body !== undefined ? JSON.stringify(opts.body) : undefined),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new JoinError(res.status, data.error || 'Something went wrong. Try again.', data.details);
  return data as T;
}

// The same checks the server makes, shown as the joiner types (the server checks again). Each says what to type.
const CHECKS: Record<string, [RegExp, string]> = {
  aadhaar: [/^\d{12}$/, 'enter the 12 digits'],
  pan: [/^[A-Z]{5}\d{4}[A-Z]$/, '10 characters, like ABCDE1234F'],
  ifsc: [/^[A-Z]{4}0[A-Z0-9]{6}$/, '11 characters, like HDFC0001234'],
  acct: [/^\d{6,18}$/, '6 to 18 digits, no spaces or dashes'],
  uan: [/^\d{12}$/, '12 digits'],
};
const HINTS: Record<string, string> = {
  aadhaar: '12 digits, on your Aadhaar card', pan: 'Like ABCDE1234F, on your PAN card', ifsc: 'Printed on your cheque book, like HDFC0001234',
  acct: 'Digits only', uan: '12 digits, on your payslip or PF passbook', ecPhone: '10-digit mobile number', ecName: 'Someone we can call in an emergency',
  addr: 'Where you live now, with the PIN code',
  uanAvail: 'Your UAN is your 12-digit PF number. Choose Yes if you had PF at an earlier job, so it can be moved here.',
  esiIp: 'On your old ESIC card or e-Pehchan card. Leave it empty if you never had ESIC.',
  tax: 'Not sure? Choose New regime: it is the default. HR can explain the difference.',
  pfChoice: 'Yes: 12% of your Basic is deducted for PF and the company adds its share; your salary is split into Basic, HRA and special allowance. No: no PF from you or the company, and your whole fixed salary is paid as Basic. If HR recorded your answer at the offer it is filled in; change it if you wish.',
  spouse: 'The company medical insurance covers you, your spouse and up to 2 children.',
  child1: 'Optional. The medical insurance covers up to 2 children.',
};
// Clearer wording for a few standard fields (only while Form setup keeps the standard label).
const PLAIN: Record<string, [string, string]> = {
  esiIp: ['ESI IP number, if existing', 'ESIC insurance number (on your old ESIC card), if you had one'],
  uanAvail: ['Existing UAN available?', 'Do you already have a UAN (PF number)?'],
};
const labelOf = (f: Field) => (PLAIN[f.key] && PLAIN[f.key][0] === f.label ? PLAIN[f.key][1] : f.label);
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function fieldError(f: Field, raw: string): string {
  const v = raw.trim();
  if (!v) return '';
  const norm = ['aadhaar', 'acct', 'uan'].includes(f.key) ? v.replace(/\s/g, '') : f.key === 'pan' || f.key === 'ifsc' ? v.toUpperCase() : v;
  const c = CHECKS[f.key];
  if (c && !c[0].test(norm)) return c[1];
  if (f.kind === 'tel' && !/^\d{10}$/.test(v.replace(/\s/g, ''))) return 'enter a 10-digit mobile number';
  if (f.kind === 'email' && !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(v)) return 'enter a valid email address';
  if (f.kind === 'number' && isNaN(Number(v))) return 'enter a number';
  if (f.key === 'dob' && v >= today()) return 'must be a date in the past';
  return '';
}
const shown = (f: Field, d: Data) => !f.onlyIf || Object.entries(f.onlyIf).every(([k, v]) => d[k] === v);

function Shell({ company, children }: { company?: string; children: ReactNode }) {
  useEffect(() => { document.title = `${company || 'Saltstayz'} · Joining details`; }, [company]);
  return <div className="jp">
    <header className="jphead"><i>{(company || 'S').slice(0, 1)}</i><span>{company || 'Saltstayz'}</span></header>
    <main className="jpmain">{children}</main>
  </div>;
}

export default function Join() {
  const { token = '' } = useParams();
  const [status, setStatus] = useState<Status | null>(null);
  const [form, setForm] = useState<Form | null>(null);
  const [fatal, setFatal] = useState('');
  const [doneAt, setDoneAt] = useState<string | null>(null);
  const [info, setInfo] = useState('');

  const loadForm = async (quiet = false) => {
    try { setForm(await call<Form>(token, '/form')); return true; } catch (e: any) {
      if (e.status === 401) { setSess(token, ''); if (!quiet) setInfo('Your session ended. Enter a new code to continue.'); return false; }
      setFatal(e.message); return false;
    }
  };
  useEffect(() => {
    (async () => {
      try {
        const s = await call<Status>(token, '');
        setStatus(s);
        // With a session still open in this tab, the form (or, after submitting, the thank-you page with the details).
        if (getSess(token)) await loadForm(s.state === 'submitted');
      } catch (e: any) { setFatal(e.message); }
    })();
  }, [token]); // eslint-disable-line react-hooks/exhaustive-deps

  if (fatal) return <Shell company={status?.company}><div className="jpcard"><h1>This link is not working</h1><p className="jpsub">{fatal}</p></div></Shell>;
  if (!status) return <Shell><div className="jpcard"><p className="jpsub">Opening…</p></div></Shell>;
  if (doneAt || status.state === 'submitted' || (form && !form.editable)) return <Shell company={status.company}><ThankYou status={status} form={form} sentAt={doneAt || form?.submittedAt || null} /></Shell>;
  if (!form) return <Shell company={status.company}><CodeStep token={token} status={status} info={info} onIn={async (session) => { setSess(token, session); setInfo(''); await loadForm(); }} /></Shell>;
  return <Shell company={status.company}><FormStep token={token} form={form} setForm={setForm}
    onSessionEnd={() => { setSess(token, ''); setForm(null); setInfo('Your session ended. Enter a new code to continue. What you typed is kept on this phone or computer and comes back after the code.'); }}
    onDone={(at) => { setDraft(token, {}); setDoneAt(at || new Date().toISOString()); }} /></Shell>;
}

function ThankYou({ status, form, sentAt }: { status: Status; form: Form | null; sentAt: string | null }) {
  const originals = (form?.docs || []).filter((x) => x.uploaded).map((x) => x.label);
  return <>
    <div className="jpcard jpdone">
      <div className="jptick" aria-hidden="true">✓</div>
      <h1>Thank you{form ? `, ${form.name.split(' ')[0]}` : ''}</h1>
      <p className="jpsub">Your joining details are with HR at {status.company}. They will contact you if anything else is needed. You can close this page.</p>
      {form && <p className="jpsub">Offer number {form.offerNo}{sentAt ? ` · sent on ${fmtD(sentAt)}` : ''}</p>}
    </div>
    {form && <div className="jpcard">
      <h2>Your first day</h2>
      <ul className="jplist">
        <li>Your joining date is <b>{fmtD(form.doj)}</b>. HR will confirm where and when to report.</li>
        {originals.length > 0 && <li>Bring the originals of the documents you uploaded, to be checked: {originals.join(', ')}.</li>}
        <li>If something changes before you join, call HR or reply to their email.</li>
      </ul>
    </div>}
  </>;
}

function CodeStep({ token, status, info, onIn }: { token: string; status: Status; info: string; onIn: (session: string) => Promise<void> }) {
  const email = status.codeBy === 'email';
  const fromHR = !!status.askHR; // no email: HR gives the code on the phone
  const [sent, setSent] = useState(!!status.codeOut || !email);
  const [code, setCode] = useState('');
  const [err, setErr] = useState('');
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [wait, setWait] = useState(0);
  const [dev, setDev] = useState('');
  useEffect(() => { if (wait <= 0) return; const t = setTimeout(() => setWait(wait - 1), 1000); return () => clearTimeout(t); }, [wait]);
  const send = async () => {
    if (busy || wait > 0) return;
    setErr(''); setNote(''); setBusy(true);
    try {
      const r = await call(token, '/code', { method: 'POST' });
      if (r.asked) { setNote(r.message); setWait(60); return; }
      setSent(true); setWait(30); setDev(r.devCode || ''); setCode('');
    } catch (e: any) { setErr(e.message); } finally { setBusy(false); }
  };
  const verify = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setErr(''); setBusy(true);
    try { const r = await call(token, '/verify', { body: { code: code.trim() } }); await onIn(r.session); }
    catch (x: any) { setErr(x.message); } finally { setBusy(false); }
  };
  const hr = status.hrName ? <b>{status.hrName}</b> : <>HR</>;
  return <form className="jpcard" onSubmit={verify} noValidate>
    <h1>Your joining details</h1>
    <p className="jpsub">Welcome to {status.company}. First, let us confirm it is you.</p>
    {info && <div className="lginfo">{info}</div>}
    {err && <div className="lgerr" role="alert">{err}</div>}
    {email && !sent && <>
      <p>We will email a 6-digit code to <b>{status.codeTo}</b>.</p>
      <button type="button" className="btn primary jpbig" disabled={busy} onClick={send}>Email me the code</button>
    </>}
    {sent && <>
      <p>{email ? <>Enter the 6-digit code we emailed to <b>{status.codeTo}</b>. It works for 10 minutes.</> : <>{hr} from {status.company} HR will call you with a 6-digit code. Enter it here. It works for 10 minutes.</>}</p>
      <label className="lgf"><span>6-digit code</span><input autoFocus autoComplete="one-time-code" inputMode="numeric" maxLength={6} placeholder="------" value={code} onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))} /></label>
      <button className="btn primary jpbig" disabled={busy || code.length !== 6}>Continue</button>
      {email && <div className="lglinks"><a role="button" tabIndex={0} style={wait > 0 ? { opacity: 0.5 } : undefined} onClick={send}>{wait > 0 ? `Send a new code in ${wait}s` : 'Send a new code'}</a></div>}
      {!email && fromHR && <>
        {note && <div className="lginfo" role="status" style={{ marginTop: 12 }}>{note}</div>}
        <p className="jpsub" style={{ marginTop: 12 }}>No call yet, or the code did not work? Press the button and {hr} will be asked to call you on the mobile number on your offer.</p>
        <button type="button" className="btn jpbig" disabled={busy || wait > 0} onClick={send}>{wait > 0 ? `Ask HR to call me (again in ${wait}s)` : 'Ask HR to call me'}</button>
      </>}
      {!email && !fromHR && <div className="lglinks"><a role="button" tabIndex={0} style={wait > 0 ? { opacity: 0.5 } : undefined} onClick={send}>{wait > 0 ? `Show a new code in ${wait}s` : 'Show me a code (development)'}</a></div>}
      {dev && <div className="lgsms"><b>Development: email preview</b><div>Your code for the {status.company} joining form is <b className="mono">{dev}</b>.</div></div>}
    </>}
    <p className="jpfine">Do not share this link or the code. The code only confirms it is you; nobody from {status.company} will ask you for it.</p>
  </form>;
}

const SESSION_WARN = 50 * 60 * 1000; // the session ends after 60 minutes without a call to the server

function FormStep({ token, form, setForm, onSessionEnd, onDone }: { token: string; form: Form; setForm: (f: Form) => void; onSessionEnd: () => void; onDone: (submittedAt?: string) => void }) {
  const [restored] = useState(() => getDraft(token));
  const [d, setD] = useState<Data>(() => ({ ...form.values, ...restored }));
  const [touched, setTouched] = useState<Record<string, boolean>>(() => Object.fromEntries(Object.keys(restored).map((k) => [k, true])));
  const [mode, setMode] = useState<'' | 'save' | 'submit'>(''); // after a Save or Submit that found problems, the list stays live
  const [serverErrs, setServerErrs] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [scroll, setScroll] = useState(0);
  const [stale, setStale] = useState(false);
  const lastCall = useRef(Date.now());
  const errBox = useRef<HTMLDivElement>(null);
  const changedKeys = (x: Data) => form.fields.map((f) => f.key).filter((k) => x[k] !== undefined && (x[k] ?? '') !== (form.values[k] ?? ''));
  const dirty = changedKeys(d).length > 0;
  const fields = form.fields.filter((f) => shown(f, d));
  const sections = useMemo(() => {
    const out: [string, Field[]][] = [];
    for (const f of fields) { const s = out.find(([n]) => n === f.section); if (s) s[1].push(f); else out.push([f.section, [f]]); }
    return out;
  }, [fields]);
  const okField = (f: Field) => !!(d[f.key] || '').trim() && !fieldError(f, d[f.key] || '');
  const left = fields.filter((f) => f.required && !okField(f)).length + form.docs.filter((x) => x.required && !x.uploaded).length;
  const total = fields.filter((f) => f.required).length + form.docs.filter((x) => x.required).length;
  // What to fix, field by field, in one pass: wrong formats, and (for Submit) required items and documents.
  const formatProblems = () => fields.map((f) => { const e = fieldError(f, d[f.key] || ''); return e ? `${labelOf(f)}: ${e}` : ''; }).filter(Boolean);
  const allProblems = () => [
    ...fields.filter((f) => f.required && !(d[f.key] || '').trim()).map((f) => `${labelOf(f)}: ${f.kind === 'sel' ? 'choose one' : 'fill this in'}`),
    ...formatProblems(),
    ...form.docs.filter((x) => x.required && !x.uploaded).map((x) => `Upload your ${x.label}`),
  ];
  const errs = [...(mode === 'submit' ? allProblems() : mode === 'save' ? formatProblems() : []), ...serverErrs];

  // A restored draft (the session ended before Save) is shown as unsaved.
  useEffect(() => { if (Object.keys(restored).length) toast('We kept what you typed before. Press Save to keep it.'); }, []); // eslint-disable-line react-hooks/exhaustive-deps
  // Scroll to the list once it is on the screen.
  useEffect(() => { if (scroll) errBox.current?.scrollIntoView({ behavior: 'smooth', block: 'center' }); }, [scroll]);
  // A warning before the session ends with unsaved typing.
  useEffect(() => { const t = setInterval(() => setStale(Date.now() - lastCall.current > SESSION_WARN), 30000); return () => clearInterval(t); }, []);

  const touch = (k: string) => setTouched((t) => ({ ...t, [k]: true }));
  const set = (k: string, v: string) => {
    const next = { ...d, [k]: v };
    setD(next); setServerErrs([]);
    setDraft(token, Object.fromEntries(changedKeys(next).map((x) => [x, next[x]])));
  };
  const showList = (m: 'save' | 'submit', server: string[] = []) => { setMode(m); setServerErrs(server); setScroll((n) => n + 1); };
  const fail = (e: any) => {
    if (e.status === 401) { onSessionEnd(); return; }
    showList(mode || 'save', e.details?.length ? e.details : [e.message]);
    toast(e.message);
  };
  const called = () => { lastCall.current = Date.now(); setStale(false); };
  // Save sends only what the joiner changed, so a correction HR made meanwhile is not overwritten.
  const save = async (quiet = false): Promise<boolean> => {
    const keys = changedKeys(d);
    const data = Object.fromEntries(keys.map((k) => [k, d[k] ?? '']));
    const r = keys.length ? await call(token, '/form', { method: 'PUT', body: { data } }) : { problems: [], skipped: [] };
    called();
    const skipped: string[] = r.skipped || [];
    const kept = Object.fromEntries(keys.filter((k) => !skipped.includes(k)).map((k) => [k, d[k] ?? '']));
    const values = { ...form.values, ...kept };
    setForm({ ...form, values });
    setDraft(token, Object.fromEntries(skipped.map((k) => [k, d[k] ?? ''])));
    if (r.problems?.length) {
      setTouched((t) => ({ ...t, ...Object.fromEntries(skipped.map((k) => [k, true])) }));
      showList(quiet ? 'submit' : 'save', formatProblems().length ? [] : r.problems);
      toast(Object.keys(kept).length ? `Saved, except ${r.problems.length === 1 ? 'one item' : r.problems.length + ' items'} to fix. See the list below.` : 'Not saved: fix the items in the list below.');
      return false;
    }
    if (!quiet) toast('Saved. You can close this page and come back to the link later.');
    return true;
  };
  const run = (fn: () => Promise<void>) => async () => { if (busy) return; setBusy(true); try { await fn(); } catch (e: any) { fail(e); } finally { setBusy(false); } };
  const doSave = run(async () => {
    if (await save()) { setMode(''); setServerErrs([]); }
  });
  const submit = run(async () => {
    const bad = allProblems();
    if (bad.length) {
      setTouched(Object.fromEntries(fields.map((f) => [f.key, true])));
      showList('submit');
      toast(bad.length === 1 ? 'One item needs your attention. See the list below.' : `${bad.length} items need your attention. See the list below.`);
      return;
    }
    if (!(await save(true))) return;
    try {
      const r = await call(token, '/submit', { method: 'POST' });
      onDone(r.submittedAt);
    } catch (e: any) {
      if (e.status === 401) { onSessionEnd(); return; }
      showList('submit', e.details?.length ? e.details : [e.message]);
      toast(e.message);
    }
  });
  const upload = async (doc: Doc, file?: File) => {
    if (!file) return;
    if (file.size > 5 * 1024 * 1024) { toast('The file must be 5 MB or smaller. Take the photo again at a lower size, or use a PDF.'); return; }
    const fd = new FormData();
    fd.append('docKey', doc.key); fd.append('file', file);
    setBusy(true);
    try {
      const r = await call(token, '/files', { form: fd });
      called(); setServerErrs([]);
      setForm({ ...form, docs: form.docs.map((x) => (x.key === doc.key ? { ...x, uploaded: { fileName: r.fileName, uploadedAt: r.uploadedAt } } : x)) });
      toast(`${doc.label}: uploaded`);
    } catch (e: any) { if (e.status === 401) onSessionEnd(); else toast(e.message); } finally { setBusy(false); }
  };
  return <>
    <div className="jpcard">
      <h1>Hello {form.name}</h1>
      <p className="jpsub">Offer number <b>{form.offerNo}</b> · {form.type} · joining on <b>{fmtD(form.doj)}</b></p>
      {form.note && <div className="lginfo"><b>A note from HR:</b> {form.note}</div>}
      <p style={{ margin: '10px 0 0', fontSize: 14 }}>Fill in your details and upload your documents. Items marked <b className="jpreq">*</b> are required. You can save and come back later with the same link.</p>
      {total > 0 && <div className="jpprog" aria-label="Progress"><i style={{ width: `${Math.round(((total - left) / total) * 100)}%` }} /><span>{total - left} of {total} required items done</span></div>}
    </div>
    {sections.map(([name, list]) => <section key={name} className="jpcard">
      <h2>{name === 'Identity and statutory' ? 'Identity and PF / ESI' : name === 'Bank' ? 'Bank account for salary' : name}</h2>
      {list.map((f) => {
        const v = d[f.key] ?? '', e = touched[f.key] || mode ? fieldError(f, v) : '';
        return <label key={f.key} className="lgf"><span>{labelOf(f)}{f.required && <b className="jpreq"> *</b>}</span>
          {f.kind === 'sel'
            ? <select value={v} onChange={(x) => set(f.key, x.target.value)} onBlur={() => touch(f.key)}><option value="">Select</option>{f.options.map((o) => <option key={o}>{o}</option>)}</select>
            : <input type={f.kind === 'date' ? 'date' : f.kind === 'email' ? 'email' : 'text'} inputMode={f.kind === 'tel' || ['aadhaar', 'acct', 'uan'].includes(f.key) ? 'numeric' : f.kind === 'number' ? 'decimal' : undefined}
              autoCapitalize={['pan', 'ifsc'].includes(f.key) ? 'characters' : undefined} value={v} aria-invalid={!!e}
              onChange={(x) => set(f.key, x.target.value)} onBlur={() => touch(f.key)} />}
          {e ? <em className="jperr">{cap(e)}</em> : HINTS[f.key] ? <em className="jphint">{HINTS[f.key]}</em> : null}
        </label>;
      })}
    </section>)}
    {form.docs.length > 0 && <section className="jpcard">
      <h2>Documents</h2>
      <p className="jpsub">Upload a clear photo or a PDF of each, up to 5 MB. You can take the photo with your phone camera.</p>
      {form.docs.map((x) => <div key={x.key} className="jpdoc">
        <div><b>{x.label}</b>{x.required && <b className="jpreq"> *</b>}
          <div className={x.uploaded ? 'jpok' : 'jphint'}>{x.uploaded ? `Uploaded: ${x.uploaded.fileName}` : x.required ? 'Required, not uploaded yet' : 'Optional'}</div></div>
        <label className="btn sm">{x.uploaded ? 'Replace' : 'Upload'}<input type="file" accept="image/*,application/pdf,.pdf" hidden disabled={busy} onChange={(e) => { upload(x, e.target.files?.[0]); e.target.value = ''; }} /></label>
      </div>)}
    </section>}
    {errs.length > 0 && <div className="jpcard" ref={errBox}><div className="lgerr" role="alert"><b>{mode === 'submit' ? 'Before you submit:' : 'Please check:'}</b><ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>{errs.map((x) => <li key={x}>{x}</li>)}</ul></div></div>}
    {dirty && <p className="jpfine" style={{ textAlign: 'center' }}>You have changes that are not saved yet.</p>}
    {dirty && stale && <div className="lginfo" role="status">Press <b>Save and continue later</b> now. For your security, this page asks for a new code after 60 minutes without saving.</div>}
    <div className="jpbar">
      <button className="btn" disabled={busy} onClick={doSave}>Save and continue later</button>
      <button className="btn primary" disabled={busy} onClick={submit}>Submit to HR</button>
    </div>
  </>;
}
