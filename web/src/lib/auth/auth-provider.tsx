import { createContext, useContext, useMemo, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  getMe,
  login as apiLogin,
  logout as apiLogout,
  type Role,
  type User,
} from "@/lib/api/auth";
import { setProjectSlug } from "@/lib/settings";
import { AUTH_QUERY_KEY, signOutLocally } from "./session-expiry";

/**
 * Session state. Wraps the whole app (above the router) so the login page and
 * the route guard can both read it. The current user is a react-query resource
 * (`/api/auth/me`); `null` means unauthenticated. Charts/pages branch on
 * `status` for the loading / anon / authed tri-state.
 */

export type AuthStatus = "loading" | "anon" | "authed";

interface AuthContextValue {
  user: User | null;
  role: Role | null;
  status: AuthStatus;
  login: (username: string, password: string) => Promise<User>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const AUTH_KEY = AUTH_QUERY_KEY;

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();

  const query = useQuery<User | null>({
    queryKey: AUTH_KEY,
    queryFn: ({ signal }) => getMe(signal),
    retry: false,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  });

  const value = useMemo<AuthContextValue>(() => {
    const user = query.data ?? null;
    const status: AuthStatus = query.isLoading
      ? "loading"
      : user
        ? "authed"
        : "anon";
    return {
      user,
      role: user?.role ?? null,
      status,
      login: async (username, password) => {
        const u = await apiLogin(username, password);
        queryClient.setQueryData(AUTH_KEY, u);
        return u;
      },
      refresh: async () => {
        await queryClient.invalidateQueries({ queryKey: AUTH_KEY });
      },
      logout: async () => {
        await apiLogout();
        setProjectSlug("");
        // Drop everything cached for this user, not just the project list.
        signOutLocally(queryClient);
      },
    };
  }, [query.data, query.isLoading, queryClient]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

// eslint-disable-next-line react-refresh/only-export-components
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
