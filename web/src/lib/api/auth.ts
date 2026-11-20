import { apiGet, apiSend, ApiError, IS_MOCK, API_ROOT } from "./client";

/**
 * Authentication API. Speaks the B1 session contract:
 *   POST /api/auth/login  {username,password} -> sets an httpOnly cookie
 *   GET  /api/auth/me      -> { username, role } (401 when not signed in)
 *   POST /api/auth/logout
 * The frontend never handles the token itself — the cookie rides on every
 * request via `credentials: include` (see client.ts). In VITE_MOCK mode a fake
 * session is kept in localStorage so RBAC views are demoable without a backend.
 */

export type Role = "admin" | "member" | "viewer";

export interface User {
  id?: number;
  username: string;
  role: Role;
  display_name?: string;
  email?: string;
  last_login_at?: string | null;
  /**
   * Set for the generated bootstrap admin password: the backend answers every
   * API except /auth/me, /auth/password and /auth/logout with 403
   * `password_change_required` until the password is changed.
   */
  must_change_password?: boolean;
}

export interface SessionInfo {
  id: string;
  created_at: string;
  expires_at: string;
  current: boolean;
}

export const ROLES: Role[] = ["admin", "member", "viewer"];

const MOCK_SESSION_KEY = "lo-mock-session";

function mockRoleFor(username: string): Role {
  const u = username.trim().toLowerCase();
  if (u.includes("admin")) return "admin";
  if (u.includes("viewer") || u.includes("readonly")) return "viewer";
  return "member";
}

function readMockSession(): User | null {
  try {
    const raw = localStorage.getItem(MOCK_SESSION_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as User;
    if (parsed && typeof parsed.username === "string") return parsed;
    return null;
  } catch {
    return null;
  }
}

function writeMockSession(user: User | null): void {
  try {
    if (user) localStorage.setItem(MOCK_SESSION_KEY, JSON.stringify(user));
    else localStorage.removeItem(MOCK_SESSION_KEY);
  } catch {
    /* ignore */
  }
}

/** Resolve the current user, or `null` when unauthenticated (HTTP 401). */
export async function getMe(signal?: AbortSignal): Promise<User | null> {
  if (IS_MOCK) return delay(readMockSession(), 150);
  try {
    return await apiGet<User>("/auth/me", {
      base: API_ROOT,
      signal,
      skipSessionExpiry: true,
    });
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return null;
    throw e;
  }
}

export async function login(username: string, password: string): Promise<User> {
  if (IS_MOCK) {
    if (!username.trim() || !password) {
      throw new ApiError("invalid username or password", 401);
    }
    const user: User = { username: username.trim(), role: mockRoleFor(username) };
    writeMockSession(user);
    return delay(user, 250);
  }
  await apiSend<void>(
    "POST",
    "/auth/login",
    { username, password },
    { base: API_ROOT, skipSessionExpiry: true },
  );
  const me = await getMe();
  if (!me) throw new ApiError("login failed", 401);
  return me;
}

export async function logout(): Promise<void> {
  if (IS_MOCK) {
    writeMockSession(null);
    return delay(undefined, 150);
  }
  await apiSend<void>("POST", "/auth/logout", undefined, {
    base: API_ROOT,
    skipSessionExpiry: true,
  });
}

export interface ProfileUpdate {
  display_name?: string;
  email?: string;
}

/** Update the signed-in user's display name and/or email. */
export async function updateMe(profile: ProfileUpdate): Promise<User> {
  if (IS_MOCK) {
    const user = readMockSession();
    if (!user) throw new ApiError("unauthenticated", 401);
    const merged: User = {
      ...user,
      display_name: profile.display_name ?? user.display_name,
      email: profile.email ?? user.email,
    };
    writeMockSession(merged);
    return delay(merged, 150);
  }
  return apiSend<User>("PUT", "/auth/me", profile, { base: API_ROOT });
}

/** Change the signed-in user's password. */
export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  if (IS_MOCK) {
    if (!currentPassword || !newPassword) throw new ApiError("invalid request", 400);
    const user = readMockSession();
    if (user?.must_change_password) writeMockSession({ ...user, must_change_password: false });
    return delay(undefined, 200);
  }
  await apiSend<void>(
    "POST",
    "/auth/password",
    { current_password: currentPassword, new_password: newPassword },
    { base: API_ROOT },
  );
}

export async function listMySessions(signal?: AbortSignal): Promise<SessionInfo[]> {
  if (IS_MOCK) return delay([], 150);
  return apiGet<SessionInfo[]>("/auth/sessions", { base: API_ROOT, signal });
}

export async function revokeSession(id: string): Promise<void> {
  if (IS_MOCK) return delay(undefined, 150);
  await apiSend<void>("DELETE", `/auth/sessions/${encodeURIComponent(id)}`, undefined, {
    base: API_ROOT,
  });
}

function delay<T>(value: T, ms: number): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}
