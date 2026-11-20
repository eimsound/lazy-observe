import type { QueryClient } from "@tanstack/react-query";
import { PROJECT_STORAGE_KEY } from "../project/project-scope.ts";

export const AUTH_QUERY_KEY = ["auth", "me"] as const;
export const PROJECTS_QUERY_KEY = ["projects"] as const;

const SESSION_EXPIRED_CODES = new Set([
  "session_expired",
  "unauthorized",
  "unauthenticated",
]);

const SENSITIVE_EXACT_KEYS = new Set([
  PROJECT_STORAGE_KEY,
  "lo-assistant-history",
  "lo-mock-session",
]);

const SENSITIVE_KEY_PREFIXES = [
  "lo-assistant-history:",
  "lo-logs-explorer-history:",
  "lo-metrics-history:",
];

export interface StorageIterable {
  readonly length: number;
  key(index: number): string | null;
  removeItem(key: string): void;
}

const localSignOutCleanups = new Set<() => void>();

/** Register a synchronous cleanup hook invoked whenever local sign-out runs. */
export function registerLocalSignOutCleanup(cleanup: () => void): () => void {
  localSignOutCleanups.add(cleanup);
  return () => {
    localSignOutCleanups.delete(cleanup);
  };
}

function defaultStorage(): StorageIterable | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/**
 * Clear user/project-scoped persisted data on sign-out while preserving
 * non-sensitive UI preferences (theme, language, sidebar state).
 */
export function clearSensitiveLocalStorage(
  storage: StorageIterable | null = defaultStorage(),
): void {
  if (!storage) return;
  try {
    const keysToRemove: string[] = [];
    for (let i = 0; i < storage.length; i++) {
      const key = storage.key(i);
      if (!key) continue;
      if (
        SENSITIVE_EXACT_KEYS.has(key)
        || SENSITIVE_KEY_PREFIXES.some((prefix) => key.startsWith(prefix))
      ) {
        keysToRemove.push(key);
      }
    }
    for (const key of keysToRemove) {
      storage.removeItem(key);
    }
  } catch {
    /* ignore unavailable storage */
  }
}

/**
 * A bare 401 or explicit session-expiry code means the session cookie is no
 * longer valid (expired, revoked, or signed out in another tab). Coded business
 * errors such as `invalid_current_password` or `project_not_found` are left to
 * the caller and must not log the user out.
 */
export function isSessionExpired(error: unknown): boolean {
  if (typeof error !== "object" || error === null) return false;
  const { status, code } = error as { status?: unknown; code?: unknown };
  if (status !== 401) return false;
  if (code === undefined || code === null || code === "") return true;
  return typeof code === "string" && SESSION_EXPIRED_CODES.has(code);
}

/**
 * True when the backend reports that the selected project no longer exists
 * (HTTP 404 `project_not_found`, or current gateway's uncoded 404
 * `"project not found"`). Instead of signing the user out, refresh the project
 * list so ProjectProvider can fall back to an active project.
 */
export function isProjectNotFoundError(error: unknown): boolean {
  if (typeof error !== "object" || error === null) return false;
  const { status, code, message } = error as {
    status?: unknown;
    code?: unknown;
    message?: unknown;
  };
  if (status !== 404) return false;
  if (code === "project_not_found") return true;
  return (
    (code === undefined || code === null || code === "") &&
    typeof message === "string" &&
    message.trim().toLowerCase() === "project not found"
  );
}

/**
 * True when POST /api/auth/logout fails because the session is already gone
 * (401) or CSRF verification failed (403). Retrying will not recover either
 * case, so the client must still clear local auth state to escape a zombie
 * signed-in shell.
 */
export function isRecoverableLogoutError(error: unknown): boolean {
  if (typeof error !== "object" || error === null) return false;
  const { status } = error as { status?: unknown };
  return status === 401 || status === 403;
}

/**
 * Forget the signed-in user and everything cached for them. The auth query
 * itself is kept and set to null so its observers flip to "anon" (the authed
 * layout then redirects to /login); every other query and user/project-scoped
 * localStorage entry is removed so a later sign-in, possibly as another user,
 * never sees the previous user's data or stale project slug.
 */
export function signOutLocally(
  client: QueryClient,
  storage: StorageIterable | null = defaultStorage(),
): void {
  for (const cleanup of localSignOutCleanups) {
    try {
      cleanup();
    } catch {
      /* keep sign-out moving even if one hook throws */
    }
  }
  clearSensitiveLocalStorage(storage);
  const others = {
    predicate: (query: { queryKey: readonly unknown[] }) =>
      query.queryKey[0] !== AUTH_QUERY_KEY[0],
  };
  void client.cancelQueries(others);
  client.removeQueries(others);
  client.setQueryData(AUTH_QUERY_KEY, null);
}

/**
 * Sign out against the server and clear local state. When the server rejects
 * logout with 401 (already expired) or 403 (CSRF failure), recover by clearing
 * local state anyway so the user is never trapped in a zombie signed-in shell.
 */
export async function performLogout(
  apiLogoutFn: () => Promise<void>,
  client: QueryClient,
  storage: StorageIterable | null = defaultStorage(),
): Promise<void> {
  try {
    await apiLogoutFn();
  } catch (error) {
    if (!isRecoverableLogoutError(error)) {
      throw error;
    }
  }
  signOutLocally(client, storage);
}

/** Called by the global transport and query/mutation caches for failed requests. */
export function handleSessionExpiry(
  client: QueryClient,
  error: unknown,
  storage: StorageIterable | null = defaultStorage(),
): void {
  if (isProjectNotFoundError(error)) {
    void client.invalidateQueries({ queryKey: PROJECTS_QUERY_KEY });
    return;
  }
  if (isSessionExpired(error)) signOutLocally(client, storage);
}
