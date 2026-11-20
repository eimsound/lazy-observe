/**
 * Strict wire-contract normalizers for Auth endpoints
 * (`internal/gateway/api.go`, `internal/gateway/account_api.go`,
 * `internal/account/account.go`, `internal/account/session.go`).
 */

export type Role = "admin" | "member" | "viewer";

export const ROLES: Role[] = ["admin", "member", "viewer"];

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

/** Stable backend error codes emitted by `/api/auth/*` and auth middlewares. */
export const AUTH_ERROR_CODES = {
  invalidRequest: "invalid_request",
  invalidCredentials: "invalid_credentials",
  rateLimited: "rate_limited",
  lockedOut: "locked_out",
  serviceUnavailable: "service_unavailable",
  passwordTooShort: "password_too_short",
  invalidCurrentPassword: "invalid_current_password",
  invalidEmail: "invalid_email",
  passwordChangeRequired: "password_change_required",
  csrfFailed: "csrf_failed",
} as const;

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

function assertNonEmptyString(value: unknown, field: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new TypeError(`${field} must be a non-empty string`);
  }
  return value;
}

/** Validate and normalize a `/api/auth/me`, `/api/auth/login`, or `PUT /api/auth/me` user payload. */
export function normalizeAuthUser(value: unknown): User {
  const raw = assertRecord(value, "auth user response");
  const username = assertNonEmptyString(raw.username, "user.username");
  if (typeof raw.role !== "string" || !VALID_ROLES.has(raw.role)) {
    throw new TypeError(`user.role must be one of ${ROLES.join(", ")}`);
  }

  let id: number | undefined;
  if (raw.id !== undefined && raw.id !== null) {
    if (typeof raw.id !== "number" || !Number.isSafeInteger(raw.id) || raw.id < 0) {
      throw new TypeError("user.id must be a non-negative integer");
    }
    id = raw.id;
  }

  let displayName: string | undefined;
  if (raw.display_name !== undefined && raw.display_name !== null) {
    if (typeof raw.display_name !== "string") {
      throw new TypeError("user.display_name must be a string");
    }
    displayName = raw.display_name;
  }

  let email: string | undefined;
  if (raw.email !== undefined && raw.email !== null) {
    if (typeof raw.email !== "string") {
      throw new TypeError("user.email must be a string");
    }
    email = raw.email;
  }

  let lastLoginAt: string | null | undefined;
  if (raw.last_login_at !== undefined) {
    if (raw.last_login_at !== null && typeof raw.last_login_at !== "string") {
      throw new TypeError("user.last_login_at must be a string or null");
    }
    lastLoginAt = raw.last_login_at;
  }

  let mustChangePassword: boolean | undefined;
  if (raw.must_change_password !== undefined) {
    if (typeof raw.must_change_password !== "boolean") {
      throw new TypeError("user.must_change_password must be a boolean");
    }
    mustChangePassword = raw.must_change_password;
  }

  return {
    ...(id !== undefined ? { id } : {}),
    username,
    role: raw.role as Role,
    ...(displayName !== undefined ? { display_name: displayName } : {}),
    ...(email !== undefined ? { email } : {}),
    ...(lastLoginAt !== undefined ? { last_login_at: lastLoginAt } : {}),
    ...(mustChangePassword !== undefined ? { must_change_password: mustChangePassword } : {}),
  };
}

/** Validate and normalize a `GET /api/auth/sessions` response array. */
export function normalizeSessionList(value: unknown): SessionInfo[] {
  if (value === null) return [];
  if (!Array.isArray(value)) {
    throw new TypeError("sessions response must be an array");
  }
  return value.map((item, index) => {
    const raw = assertRecord(item, `sessions[${index}]`);
    const id = assertNonEmptyString(raw.id, `sessions[${index}].id`);
    const createdAt = assertNonEmptyString(raw.created_at, `sessions[${index}].created_at`);
    const expiresAt = assertNonEmptyString(raw.expires_at, `sessions[${index}].expires_at`);
    if (typeof raw.current !== "boolean") {
      throw new TypeError(`sessions[${index}].current must be a boolean`);
    }
    return {
      id,
      created_at: createdAt,
      expires_at: expiresAt,
      current: raw.current,
    };
  });
}
