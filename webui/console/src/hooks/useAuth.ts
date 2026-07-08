import { useCallback, useEffect, useState } from "react";
import { api, setUnauthorizedHandler } from "../lib/api";
import type { Role, User } from "../lib/types";

// The console's auth state, bootstrapped from GET /api/me.
//   authEnabled false            → open mode (no root configured); render the app,
//                                    hide account management and logout.
//   authEnabled true, user null  → must log in; render the login screen.
//   authEnabled true, user set   → logged in; render the app gated by user.role.
export interface Auth {
  loading: boolean;
  authEnabled: boolean;
  user: User | null;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
}

export function useAuth(): Auth {
  const [loading, setLoading] = useState(true);
  const [authEnabled, setAuthEnabled] = useState(false);
  const [user, setUser] = useState<User | null>(null);

  const refresh = useCallback(async () => {
    try {
      const me = await api.me();
      setAuthEnabled(me.auth_enabled);
      setUser(
        me.authenticated && me.username && me.role
          ? { username: me.username, role: me.role as Role, reports_to: me.reports_to ?? null }
          : null,
      );
    } catch {
      // Backend unreachable: assume auth is on and unauthenticated so we show the
      // login screen rather than a half-rendered app.
      setAuthEnabled(true);
      setUser(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // A 401 from any poll means the session lapsed — drop to the login screen. Only
  // meaningful when auth is enabled (open mode never 401s).
  useEffect(() => {
    setUnauthorizedHandler(() => setUser(null));
    return () => setUnauthorizedHandler(null);
  }, []);

  const login = useCallback(
    async (username: string, password: string) => {
      await api.login(username, password);
      await refresh();
    },
    [refresh],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setUser(null);
    }
  }, []);

  return { loading, authEnabled, user, login, logout };
}
