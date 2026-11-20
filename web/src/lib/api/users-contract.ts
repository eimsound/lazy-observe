import { ROLES, type Role } from "./auth-contract.ts";

/**
 * Strict wire-contract normalizers for User Management endpoints
 * (`internal/gateway/api.go`, `internal/meta/store.go` `UserInfo`).
 *
 * Kept free of Vite/React imports so contract tests run directly under Vitest.
 */

export interface ManagedUser {
  id: string;
  username: string;
  role: Role;
  display_name?: string;
  email?: string;
  created_at: string;
  last_login_at?: string | null;
  must_change_password?: boolean;
}

export interface CreateUserRequest {
  username: string;
  password: string;
  role: Role;
  display_name?: string;
  email?: string;
}

export interface CreateUserPayload {
  username: string;
  password: string;
  role: Role;
  display_name?: string;
  email?: string;
}

const VALID_ROLES = new Set<string>(ROLES);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function assertRecord(value: unknown, label: string): Record<string, unknown> {
  if (!isRecord(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value;
}

function normalizeUserId(value: unknown): string {
  if (typeof value === "string" && value.trim() !== "") return value;
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) {
    return String(value);
  }
  throw new TypeError("managed user.id must be a positive integer or non-empty string");
}

/**
 * Builds the `POST /api/users` JSON payload matching `createUser` in
 * `internal/gateway/api.go` (including optional `display_name` and `email`).
 */
export function buildCreateUserPayload(input: CreateUserRequest): CreateUserPayload {
  const username = input.username.trim();
  if (!username) {
    throw new TypeError("username must be a non-empty string");
  }
  if (typeof input.password !== "string" || input.password === "") {
    throw new TypeError("password must be a non-empty string");
  }
  if (!VALID_ROLES.has(input.role)) {
    throw new TypeError(`role must be one of ${ROLES.join(", ")}`);
  }
  const displayName = input.display_name?.trim();
  const email = input.email?.trim();
  return {
    username,
    password: input.password,
    role: input.role,
    ...(displayName ? { display_name: displayName } : {}),
    ...(email ? { email } : {}),
  };
}

/**
 * Validates and normalizes `meta.UserInfo` JSON returned by `GET /api/users`,
 * `POST /api/users`, and `PATCH /api/users/:id`.
 */
export function normalizeManagedUser(value: unknown): ManagedUser {
  const raw = assertRecord(value, "managed user response");
  const id = normalizeUserId(raw.id);
  if (typeof raw.username !== "string" || raw.username.trim() === "") {
    throw new TypeError("managed user.username must be a non-empty string");
  }
  if (typeof raw.role !== "string" || !VALID_ROLES.has(raw.role)) {
    throw new TypeError(`managed user.role must be one of ${ROLES.join(", ")}`);
  }
  if (raw.display_name !== undefined && raw.display_name !== null && typeof raw.display_name !== "string") {
    throw new TypeError("managed user.display_name must be a string");
  }
  if (raw.email !== undefined && raw.email !== null && typeof raw.email !== "string") {
    throw new TypeError("managed user.email must be a string");
  }
  if (raw.created_at !== undefined && raw.created_at !== null && typeof raw.created_at !== "string") {
    throw new TypeError("managed user.created_at must be a string");
  }
  if (raw.last_login_at !== undefined && raw.last_login_at !== null && typeof raw.last_login_at !== "string") {
    throw new TypeError("managed user.last_login_at must be a string or null");
  }
  if (raw.must_change_password !== undefined && typeof raw.must_change_password !== "boolean") {
    throw new TypeError("managed user.must_change_password must be a boolean");
  }

  return {
    id,
    username: raw.username,
    role: raw.role as Role,
    ...(typeof raw.display_name === "string" && raw.display_name !== ""
      ? { display_name: raw.display_name }
      : {}),
    ...(typeof raw.email === "string" && raw.email !== "" ? { email: raw.email } : {}),
    created_at: typeof raw.created_at === "string" ? raw.created_at : "",
    ...(raw.last_login_at !== undefined ? { last_login_at: raw.last_login_at as string | null } : {}),
    ...(raw.must_change_password !== undefined
      ? { must_change_password: raw.must_change_password }
      : {}),
  };
}

export function normalizeManagedUserList(value: unknown): ManagedUser[] {
  if (value === null || value === undefined) return [];
  if (!Array.isArray(value)) {
    throw new TypeError("managed users response must be an array");
  }
  return value.map(normalizeManagedUser);
}
