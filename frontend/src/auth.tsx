import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react';
import { api, session, setUnauthorizedHandler } from './api';

export interface Me {
  ctx: {
    employeeId: string; name: string; roles: string[]; role: string; access: string; rights: Record<string, number>; sensitive: Record<string, string>; canDownload: boolean;
    /** Names of the locations and departments the role view is limited to (empty when it sees all). */
    scopeLocations?: string[]; scopeDepartments?: string[];
    /** Open separation cases where this person is the reporting manager (they sign the manager's clearance). */
    mgrCases?: number;
  };
  employee: { id: string; name: string; designation: string; department: string; location: string; status: string; type?: string };
  /** The person's work policy lets them raise their own reimbursement claims (missing means yes). */
  claimsOn?: boolean;
}

interface Auth {
  me: Me | null;
  loading: boolean;
  signedIn: (token: string) => Promise<void>;
  signOut: () => Promise<void>;
  switchRole: (role: string) => Promise<void>;
  /** Right level for the current role view: 0 none, 1 view, 2 edit. */
  can: (right: string, min?: 1 | 2) => boolean;
}

const Ctx = createContext<Auth>(null!);
export const useAuth = () => useContext(Ctx);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    if (!session.token) { setMe(null); setLoading(false); return; }
    try {
      const m = await api<Me>('/me');
      if (m.ctx.role !== session.role) session.role = m.ctx.role; // the server falls back to Employee
      setMe(m);
    } catch { setMe(null); } finally { setLoading(false); }
  }, []);

  useEffect(() => {
    setUnauthorizedHandler(() => { session.token = ''; setMe(null); });
    load();
  }, [load]);

  const value: Auth = {
    me,
    loading,
    signedIn: async (token) => { session.token = token; session.role = 'Employee'; await load(); },
    signOut: async () => { try { await api('/auth/logout', { method: 'POST' }); } catch { /* already ended */ } session.token = ''; setMe(null); },
    switchRole: async (role) => { session.role = role; await load(); },
    can: (right, min = 1) => (me?.ctx.rights[right] ?? 0) >= min,
  };
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}
