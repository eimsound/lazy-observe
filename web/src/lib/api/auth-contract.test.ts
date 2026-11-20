import assert from "node:assert/strict";
import { test } from "vitest";

import {
  AUTH_ERROR_CODES,
  normalizeAuthUser,
  normalizeSessionList,
} from "./auth-contract.ts";
import { loginErrorTranslationKey } from "./login-error.ts";

test("normalizeAuthUser accepts exact GET /api/auth/me and POST /api/auth/login gateway payloads", () => {
  // Exact wire shape of Gateway.me (internal/gateway/api.go).
  const meWire = {
    id: 1,
    username: "admin",
    role: "admin",
    display_name: "Administrator",
    email: "admin@example.com",
    last_login_at: "2026-11-21T08:30:00Z",
    must_change_password: false,
  };
  assert.deepEqual(normalizeAuthUser(meWire), meWire);

  // Exact wire shape of Gateway.login (no id or last_login_at, first-login flag true).
  const loginWire = {
    username: "admin",
    role: "admin",
    display_name: "",
    email: "",
    must_change_password: true,
  };
  assert.deepEqual(normalizeAuthUser(loginWire), loginWire);

  // Nullable last_login_at when user has never logged in before.
  assert.deepEqual(
    normalizeAuthUser({
      id: 2,
      username: "viewer",
      role: "viewer",
      last_login_at: null,
      must_change_password: false,
    }),
    {
      id: 2,
      username: "viewer",
      role: "viewer",
      last_login_at: null,
      must_change_password: false,
    },
  );
});

test("normalizeAuthUser rejects drifted roles, empty usernames, and invalid field types", () => {
  assert.throws(() => normalizeAuthUser(null), /must be an object/);
  assert.throws(() => normalizeAuthUser({ username: "", role: "admin" }), /username/);
  assert.throws(() => normalizeAuthUser({ username: "alice", role: "superuser" }), /role/);
  assert.throws(() => normalizeAuthUser({ id: "1", username: "alice", role: "member" }), /id/);
  assert.throws(
    () => normalizeAuthUser({ username: "alice", role: "member", must_change_password: "true" }),
    /must_change_password/,
  );
});

test("normalizeSessionList matches GET /api/auth/sessions JSON output and handles null/empty", () => {
  // Mirrors internal/account/session.go SessionInfo struct.
  const wire = [
    {
      id: "sess_abc123",
      created_at: "2026-11-20T08:00:00Z",
      expires_at: "2026-11-27T08:00:00Z",
      current: true,
    },
    {
      id: "sess_def456",
      created_at: "2026-11-19T12:00:00Z",
      expires_at: "2026-11-26T12:00:00Z",
      current: false,
    },
  ];
  assert.deepEqual(normalizeSessionList(wire), wire);
  assert.deepEqual(normalizeSessionList([]), []);
  assert.deepEqual(normalizeSessionList(null), []);

  assert.throws(() => normalizeSessionList({ sessions: [] }), /array/);
  assert.throws(
    () =>
      normalizeSessionList([
        { id: "sess_1", created_at: "2026-11-20T08:00:00Z", expires_at: "2026-11-27T08:00:00Z", current: "yes" },
      ]),
    /current/,
  );
});

test("auth error codes stay aligned with gateway loginError and accountAPIError", () => {
  assert.equal(AUTH_ERROR_CODES.invalidCredentials, "invalid_credentials");
  assert.equal(AUTH_ERROR_CODES.rateLimited, "rate_limited");
  assert.equal(AUTH_ERROR_CODES.lockedOut, "locked_out");
  assert.equal(AUTH_ERROR_CODES.passwordChangeRequired, "password_change_required");
  assert.equal(AUTH_ERROR_CODES.csrfFailed, "csrf_failed");

  assert.equal(loginErrorTranslationKey({ status: 401, code: AUTH_ERROR_CODES.invalidCredentials }), "auth.invalidCredentials");
  assert.equal(loginErrorTranslationKey({ status: 429, code: AUTH_ERROR_CODES.rateLimited }), "auth.rateLimited");
  assert.equal(loginErrorTranslationKey({ status: 429, code: AUTH_ERROR_CODES.lockedOut }), "auth.rateLimited");
  assert.equal(loginErrorTranslationKey({ status: 503, code: AUTH_ERROR_CODES.serviceUnavailable }), "auth.serverError");
});
