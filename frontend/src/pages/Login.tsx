import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { api } from '../api';
import { useAuth } from '../auth';

const RULES: [string, (p: string) => boolean][] = [
  ['At least 8 characters', (p) => p.length >= 8], ['An upper case letter', (p) => /[A-Z]/.test(p)], ['A lower case letter', (p) => /[a-z]/.test(p)],
  ['A number', (p) => /\d/.test(p)], ['A special character, like @ # $ !', (p) => /[^A-Za-z0-9]/.test(p)],
];

type Step = 'signin' | 'id' | 'otp' | 'password';

/** A link-styled action inside the card (prototype `.lglinks a`, no href). */
const Lk = ({ on, children, dim, id }: { on: () => void; children: ReactNode; dim?: boolean; id?: string }) =>
  <a id={id} role="button" tabIndex={0} style={dim ? { opacity: 0.5 } : undefined} onClick={on} onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); on(); } }}>{children}</a>;

/** Employee ID + password. First time and forgotten password go through an OTP to the registered mobile (spec 6.5). */
export default function Login() {
  const { signedIn } = useAuth();
  const [step, setStep] = useState<Step>('signin');
  const [mode, setMode] = useState<'first' | 'forgot'>('first');
  const [id, setId] = useState('');
  const [pw, setPw] = useState('');
  const [pw2, setPw2] = useState('');
  const [show, setShow] = useState(false);
  const [otp, setOtp] = useState('');
  const [vt, setVt] = useState('');
  const [sent, setSent] = useState<{ mask: string; devOtp?: string } | null>(null);
  const [err, setErr] = useState('');
  const [info, setInfo] = useState('');
  const [busy, setBusy] = useState(false);
  const [wait, setWait] = useState(0); // seconds until another OTP can be sent
  useEffect(() => {
    if (wait <= 0) return;
    const t = setTimeout(() => setWait(wait - 1), 1000);
    return () => clearTimeout(t);
  }, [wait]);
  useEffect(() => { document.body.classList.add('locked'); return () => document.body.classList.remove('locked'); }, []);

  const run = (fn: () => Promise<void>) => async (e?: FormEvent) => {
    e?.preventDefault();
    if (busy) return;
    setErr('');
    setBusy(true);
    try { await fn(); } catch (x: any) { setErr(x.message); } finally { setBusy(false); }
  };
  const go = (s: Step, m?: 'first' | 'forgot') => { setErr(''); setInfo(''); if (m) setMode(m); if (s === 'signin') { setSent(null); setWait(0); } setStep(s); };

  const signin = run(async () => {
    try { const r = await api<{ token: string }>('/auth/login', { body: { employeeId: id, password: pw } }); setPw(''); await signedIn(r.token); } catch (x) { setPw(''); throw x; }
  });
  const sendOtp = run(async () => {
    setInfo('');
    const r = await api<{ employeeId: string; mask: string; purpose: 'first' | 'forgot'; devOtp?: string }>('/auth/otp', { body: { employeeId: id, purpose: mode } });
    if (mode === 'first' && r.purpose === 'forgot') setInfo('You already have a password, so this is a password reset.');
    setId(r.employeeId); setMode(r.purpose); setSent(r); setOtp(''); setStep('otp'); setWait(30);
  });
  // The verify call returns a one-time token that the password call must carry (only the person who entered the OTP sets the password).
  const verify = run(async () => { const v = await api<{ verifyToken: string }>('/auth/otp/verify', { body: { employeeId: id, code: otp.trim() } }); setVt(v?.verifyToken || ''); setInfo(''); setPw(''); setPw2(''); setStep('password'); });
  const setPassword = run(async () => {
    const bad = RULES.filter(([, t]) => !t(pw)).map(([l]) => l);
    if (bad.length) throw new Error('Your password needs: ' + bad.join(', ').toLowerCase() + '.');
    if (pw !== pw2) throw new Error('The two passwords do not match.');
    const r = await api<{ token: string }>('/auth/password', { body: { employeeId: id, password: pw, verifyToken: vt } });
    await signedIn(r.token);
  });

  const first = mode === 'first';
  const card = (h: string, sub: string, body: ReactNode, submit: (e?: FormEvent) => void) =>
    <form className="lgcard" onSubmit={submit} noValidate>
      <div className="lglogo"><i>S</i>Saltstayz HRMS</div>
      <h1>{h}</h1>
      {sub && <p className="lgsub">{sub}</p>}
      {err && <div className="lgerr" role="alert">{err}</div>}
      {info && <div className="lginfo">{info}</div>}
      {body}
    </form>;
  const idField = <label className="lgf"><span>Employee ID</span><input autoFocus autoComplete="off" value={id} onChange={(e) => setId(e.target.value)} placeholder="For example EMP-1001" /></label>;
  const showBtn = <button type="button" onClick={() => setShow(!show)}>{show ? 'Hide' : 'Show'}</button>;

  return (
    <div id="login" style={{ display: 'flex' }}>
      {step === 'signin' && card('Sign in', 'Use your Employee ID and password.', <>
        {idField}
        <label className="lgf"><span>Password</span><div className="lgpw"><input type={show ? 'text' : 'password'} autoComplete="off" value={pw} onChange={(e) => setPw(e.target.value)} />{showBtn}</div></label>
        <button className="btn primary lgbtn" disabled={busy}>Sign in</button>
        <div className="lglinks"><Lk on={() => go('id', 'first')}>First time here? Activate with OTP</Lk><Lk on={() => go('id', 'forgot')}>Forgot password?</Lk></div>
      </>, signin)}
      {step === 'id' && card(first ? 'Activate your account' : 'Reset your password',
        first ? 'Enter your Employee ID. We send a one-time password to the mobile number HR has on record.' : 'Enter your Employee ID. We send a one-time password to your registered mobile number.', <>
        {idField}
        <button className="btn primary lgbtn" disabled={busy}>Send OTP</button>
        <div className="lglinks"><Lk on={() => go('signin')}>Back to sign in</Lk></div>
      </>, sendOtp)}
      {step === 'otp' && card('Enter the OTP', `Sent to ${sent?.mask ?? ''}. It is valid for 5 minutes.`, <>
        <label className="lgf"><span>6-digit OTP</span><input autoFocus autoComplete="one-time-code" inputMode="numeric" maxLength={6} placeholder="------" value={otp} onChange={(e) => setOtp(e.target.value)} /></label>
        <button className="btn primary lgbtn" disabled={busy}>Verify</button>
        <div className="lglinks"><Lk id="lg_resend" dim={wait > 0} on={() => { if (wait <= 0) sendOtp(); }}>{wait > 0 ? `Resend OTP in ${wait}s` : 'Resend OTP'}</Lk><Lk on={() => go('signin')}>Back to sign in</Lk></div>
        {sent?.devOtp && <div className="lgsms"><b>Development: SMS preview</b><div>To {sent.mask}: Your Saltstayz HRMS OTP is <b className="mono">{sent.devOtp}</b>. Valid for 5 minutes. Do not share it.</div></div>}
      </>, verify)}
      {step === 'password' && card(first ? 'Create your password' : 'Set a new password', 'You will use your Employee ID and this password to sign in.', <>
        <label className="lgf"><span>New password</span><div className="lgpw"><input autoFocus type={show ? 'text' : 'password'} autoComplete="new-password" value={pw} onChange={(e) => setPw(e.target.value)} />{showBtn}</div></label>
        <label className="lgf"><span>Confirm password</span><input type={show ? 'text' : 'password'} autoComplete="new-password" value={pw2} onChange={(e) => setPw2(e.target.value)} /></label>
        <ul className="lgrules">{RULES.map(([l, t]) => <li key={l} className={t(pw) ? 'ok' : ''}>{l}</li>)}</ul>
        <button className="btn primary lgbtn" disabled={busy}>Save password and sign in</button>
      </>, setPassword)}
    </div>
  );
}
