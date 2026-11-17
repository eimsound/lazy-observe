import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";
import type { Role } from "./auth";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockAdmin = () => import("./mock-admin");

/**
 * User management API (B1, admin-only). Endpoints live under /api. Passwords are
 * write-only (sent on create, never read back).
 */

export interface ManagedUser {
  id: string;
  username: string;
  role: Role;
  created_at: string;
}

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export async function listUsers(signal?: AbortSignal): Promise<ManagedUser[]> {
  if (IS_MOCK) return delay([...(await loadMockAdmin()).mockUsers]);
  return apiGet<ManagedUser[]>("/users", { base: API_ROOT, signal });
}

export async function createUser(
  username: string,
  password: string,
  role: Role,
): Promise<ManagedUser> {
  if (IS_MOCK) return delay((await loadMockAdmin()).createMockUser(username, role), 250);
  return apiSend<ManagedUser>(
    "POST",
    "/users",
    { username, password, role },
    { base: API_ROOT },
  );
}

export async function updateUserRole(id: string, role: Role): Promise<ManagedUser> {
  if (IS_MOCK) return delay((await loadMockAdmin()).updateMockUserRole(id, role), 200);
  return apiSend<ManagedUser>("PATCH", `/users/${id}`, { role }, { base: API_ROOT });
}

/** Admin resets a user's password. */
export async function resetUserPassword(id: string, password: string): Promise<ManagedUser> {
  if (IS_MOCK) return delay((await loadMockAdmin()).updateMockUserRole(id, "member"), 200);
  return apiSend<ManagedUser>("PATCH", `/users/${id}`, { password }, { base: API_ROOT });
}

export async function deleteUser(id: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAdmin()).deleteMockUser(id);
    return delay(undefined, 200);
  }
  await apiSend<void>("DELETE", `/users/${id}`, undefined, { base: API_ROOT });
}
