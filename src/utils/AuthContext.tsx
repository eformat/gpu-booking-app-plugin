import * as React from 'react';
import { getAuthInfo, AuthInfo } from './api';

interface AuthState {
  username: string;
  isAdmin: boolean;
  loading: boolean;
}

// Module-level cache so all pages share the same auth state
// Cache expires after 30s so impersonation changes are picked up
let cachedAuth: AuthInfo | null = null;
let authPromise: Promise<AuthInfo> | null = null;
let cacheTimestamp = 0;
const CACHE_TTL_MS = 30000;

function fetchAuth(): Promise<AuthInfo> {
  const now = Date.now();
  if (authPromise && cachedAuth && (now - cacheTimestamp) < CACHE_TTL_MS) {
    return authPromise;
  }
  authPromise = getAuthInfo()
    .then((info) => {
      cachedAuth = info;
      cacheTimestamp = Date.now();
      return info;
    })
    .catch(() => {
      const fallback: AuthInfo = { username: '', groups: [], is_admin: false };
      cachedAuth = fallback;
      cacheTimestamp = Date.now();
      return fallback;
    });
  return authPromise;
}

export function useAuth(): AuthState {
  const [auth, setAuth] = React.useState<AuthState>(() => {
    if (cachedAuth && (Date.now() - cacheTimestamp) < CACHE_TTL_MS) {
      return { username: cachedAuth.username, isAdmin: cachedAuth.is_admin, loading: false };
    }
    return { username: '', isAdmin: false, loading: true };
  });

  React.useEffect(() => {
    fetchAuth().then((info) => {
      setAuth({ username: info.username, isAdmin: info.is_admin, loading: false });
    });
  }, []);

  return auth;
}
