import type { QueryClient } from "@tanstack/react-query";

export const AUTH_QUERY_KEY = ["auth", "me"] as const;

/**
 * A bare 401 means the session cookie is no longer valid (expired, revoked,
 * or signed out in another tab). Coded 401s such as `invalid_current_password`
 * are ordinary form errors and are left to the caller.
 */
export function isSessionExpired(error: unknown): boolean {
  if (typeof error !== "object" || error === null) return false;
  const { status, code } = error as { status?: unknown; code?: unknown };
  return status === 401 && (code === undefined || code === null || code === "");
}

/**
 * Forget the signed-in user and everything cached for them. The auth query
 * itself is kept and set to null so its observers flip to "anon" (the authed
 * layout then redirects to /login); every other query is removed so a later
 * sign-in, possibly as another user, never sees the previous user's data.
 */
export function signOutLocally(client: QueryClient): void {
  const others = { predicate: (query: { queryKey: readonly unknown[] }) => query.queryKey[0] !== AUTH_QUERY_KEY[0] };
  void client.cancelQueries(others);
  client.removeQueries(others);
  client.setQueryData(AUTH_QUERY_KEY, null);
}

/** Called by the global query/mutation caches for every failed request. */
export function handleSessionExpiry(client: QueryClient, error: unknown): void {
  if (isSessionExpired(error)) signOutLocally(client);
}
