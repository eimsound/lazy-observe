import assert from "node:assert/strict";
import { afterEach, test, vi } from "vitest";

import {
  buildCreateUserPayload,
  normalizeManagedUser,
  normalizeManagedUserList,
} from "./users-contract.ts";
import { createUser } from "./users.ts";

const originalWindow = globalThis.window;
const originalDocument = globalThis.document;

function installBrowserOrigin() {
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "http://localhost:5173" } },
  });
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: { cookie: "lo_csrf=csrf-tok" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  if (originalWindow === undefined) {
    Reflect.deleteProperty(globalThis, "window");
  } else {
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: originalWindow,
    });
  }
  if (originalDocument === undefined) {
    Reflect.deleteProperty(globalThis, "document");
  } else {
    Object.defineProperty(globalThis, "document", {
      configurable: true,
      value: originalDocument,
    });
  }
});

test("POST /api/users payload includes display_name when provided and omits empty display_name", () => {
  assert.deepEqual(
    buildCreateUserPayload({
      username: "  alice  ",
      password: "Password123!",
      role: "member",
      display_name: "  Alice Chen  ",
    }),
    {
      username: "alice",
      password: "Password123!",
      role: "member",
      display_name: "Alice Chen",
    },
  );

  assert.deepEqual(
    buildCreateUserPayload({
      username: "bob",
      password: "Password123!",
      role: "viewer",
      display_name: "   ",
    }),
    {
      username: "bob",
      password: "Password123!",
      role: "viewer",
    },
  );
});

test("normalizeManagedUser matches real Go meta.UserInfo JSON (numeric id + display_name)", () => {
  // Exact wire JSON emitted by internal/gateway/api.go createUser / listUsers:
  const wire = {
    id: 7,
    username: "alice",
    role: "member",
    display_name: "Alice Chen",
    email: "alice@example.com",
    created_at: "2026-11-21T00:00:00Z",
    last_login_at: null,
    must_change_password: false,
  };

  assert.deepEqual(normalizeManagedUser(wire), {
    id: "7",
    username: "alice",
    role: "member",
    display_name: "Alice Chen",
    email: "alice@example.com",
    created_at: "2026-11-21T00:00:00Z",
    last_login_at: null,
    must_change_password: false,
  });

  assert.deepEqual(normalizeManagedUserList([wire]), [normalizeManagedUser(wire)]);
  assert.throws(() => normalizeManagedUser({ ...wire, role: "superuser" }), /role/);
  assert.throws(() => normalizeManagedUser({ ...wire, id: 0 }), /id/);
});

test("createUser sends display_name in POST /api/users body and normalizes numeric id response", async () => {
  installBrowserOrigin();
  const captured: Array<{ url: string; body: string }> = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      captured.push({ url: String(input), body: String(init?.body ?? "") });
      return new Response(
        JSON.stringify({
          id: 42,
          username: "charlie",
          role: "member",
          display_name: "Charlie",
          created_at: "2026-11-21T00:00:00Z",
          must_change_password: false,
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      );
    }),
  );

  const created = await createUser("charlie", "SecretPass123!", "member", {
    display_name: "Charlie",
  });
  assert.equal(captured.length, 1);
  assert.equal(captured[0]?.url, "/api/users");
  assert.deepEqual(JSON.parse(captured[0]!.body), {
    username: "charlie",
    password: "SecretPass123!",
    role: "member",
    display_name: "Charlie",
  });
  assert.equal(created.id, "42");
  assert.equal(created.display_name, "Charlie");
});
