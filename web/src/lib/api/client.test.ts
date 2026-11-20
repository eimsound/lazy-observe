import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "vitest";
import { QueryClient } from "@tanstack/react-query";

import {
  ApiError,
  API_ROOT,
  apiGet,
  apiGetResponse,
  apiPostForm,
  apiPostFormResponse,
  apiSend,
  registerSessionExpiryHandler,
} from "./client.ts";
import {
  AUTH_QUERY_KEY,
  PROJECTS_QUERY_KEY,
  handleSessionExpiry,
  type StorageIterable,
} from "../auth/session-expiry.ts";
import { setProjectSlug } from "../settings.ts";

interface RecordedFetch {
  url: string;
  init?: RequestInit;
}

const originalFetch = globalThis.fetch;
const originalWindow = globalThis.window;
const originalDocument = globalThis.document;

function stubBrowserGlobals(cookie = ""): void {
  Object.defineProperty(globalThis, "window", {
    value: { location: { origin: "https://observe.local" } },
    configurable: true,
    writable: true,
  });
  Object.defineProperty(globalThis, "document", {
    value: { cookie },
    configurable: true,
    writable: true,
  });
}

function memoryStorage(initial: Record<string, string> = {}): StorageIterable & {
  snapshot(): Record<string, string>;
} {
  const map = new Map<string, string>(Object.entries(initial));
  return {
    get length() {
      return map.size;
    },
    key(index: number) {
      return [...map.keys()][index] ?? null;
    },
    removeItem(key: string) {
      map.delete(key);
    },
    snapshot() {
      return Object.fromEntries(map.entries());
    },
  };
}

beforeEach(() => {
  stubBrowserGlobals("lo_csrf=csrf-token-123");
  setProjectSlug("implicit-fallback");
});

afterEach(() => {
  registerSessionExpiryHandler(null);
  setProjectSlug("");
  globalThis.fetch = originalFetch;
  Object.defineProperty(globalThis, "window", {
    value: originalWindow,
    configurable: true,
    writable: true,
  });
  Object.defineProperty(globalThis, "document", {
    value: originalDocument,
    configurable: true,
    writable: true,
  });
});

test("explicit opts.project overrides implicit project slug across all transport methods", async () => {
  const calls: RecordedFetch[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return new Response(JSON.stringify({ ok: true }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as typeof fetch;

  await apiGet("/query_range", { params: { query: "up" }, project: "explicit-alpha" });
  await apiGetResponse("/profiles/1/raw", { base: API_ROOT, project: "explicit-beta" });
  await apiSend("POST", "/ai/assistant/query", { question: "hi" }, {
    base: API_ROOT,
    project: "explicit-gamma",
  });
  await apiPostFormResponse("/logs/tail", { query: "*" }, { project: "explicit-delta" });
  await apiPostForm("/logs/query", { query: "*" }, { project: "explicit-epsilon" });

  assert.equal(calls.length, 5);
  const headerOf = (index: number, name: string) =>
    (calls[index].init?.headers as Record<string, string> | undefined)?.[name];

  assert.equal(headerOf(0, "X-LO-Project"), "explicit-alpha");
  assert.equal(headerOf(1, "X-LO-Project"), "explicit-beta");
  assert.equal(headerOf(2, "X-LO-Project"), "explicit-gamma");
  assert.equal(headerOf(2, "X-CSRF-Token"), "csrf-token-123");
  assert.equal(headerOf(3, "X-LO-Project"), "explicit-delta");
  assert.equal(headerOf(4, "X-LO-Project"), "explicit-epsilon");
});

test("all transport methods notify the global 401 session-expiry handler (including Live Tail, export, prettify, AI)", async () => {
  const expiredErrors: ApiError[] = [];
  registerSessionExpiryHandler((err) => expiredErrors.push(err));

  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ error: "unauthorized" }), {
      status: 401,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;

  await assert.rejects(
    apiGet("/prettify-query", { params: { query: "up" }, project: "prod" }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );
  await assert.rejects(
    apiGetResponse("/profiles/1/raw", { base: API_ROOT, project: "prod" }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );
  await assert.rejects(
    apiSend("POST", "/ai/assistant/query", { question: "errors" }, { base: API_ROOT, project: "prod" }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );
  await assert.rejects(
    apiPostFormResponse("/logs/tail", { query: "*" }, { project: "prod" }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );
  await assert.rejects(
    apiPostForm("/logs/query", { query: "*" }, { project: "prod" }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );

  assert.equal(expiredErrors.length, 5);
  assert.ok(expiredErrors.every((err) => err.status === 401));
});

test("skipSessionExpiry suppresses the global 401 handler for auth endpoints", async () => {
  const seen: ApiError[] = [];
  registerSessionExpiryHandler((err) => seen.push(err));

  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ error: "unauthorized" }), {
      status: 401,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;

  await assert.rejects(
    apiGet("/auth/me", { base: API_ROOT, skipSessionExpiry: true }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );
  await assert.rejects(
    apiSend("POST", "/auth/logout", undefined, { base: API_ROOT, skipSessionExpiry: true }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );

  assert.equal(seen.length, 0);
});

test("transport + handleSessionExpiry distinguishes 401 session expiry from 404 project_not_found and coded business 401s", async () => {
  const client = new QueryClient();
  client.setQueryData(AUTH_QUERY_KEY, { username: "alice", role: "member" });
  client.setQueryData(PROJECTS_QUERY_KEY, [{ id: "1", slug: "alpha" }]);
  const storage = memoryStorage({ "lo-project": "alpha", "lo-language": "zh" });

  registerSessionExpiryHandler((err) => handleSessionExpiry(client, err, storage));

  // 1. Gateway uncoded 404 {"error": "project not found"} refreshes projects without signing out.
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ error: "project not found" }), {
      status: 404,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;

  await assert.rejects(
    apiPostFormResponse("/logs/tail", { query: "*" }, { project: "deleted-slug" }),
    (err: unknown) => err instanceof ApiError && err.status === 404,
  );
  assert.deepEqual(client.getQueryData(AUTH_QUERY_KEY), { username: "alice", role: "member" });
  assert.equal(client.getQueryState(PROJECTS_QUERY_KEY)?.isInvalidated, true);

  // 2. Coded 401 invalid_current_password must NOT sign out.
  globalThis.fetch = (async () =>
    new Response(
      JSON.stringify({ code: "invalid_current_password", error: "current password is incorrect" }),
      { status: 401, headers: { "Content-Type": "application/json" } },
    )) as typeof fetch;

  await assert.rejects(
    apiSend("POST", "/auth/password", { current_password: "wrong", new_password: "newpassword" }, { base: API_ROOT }),
    (err: unknown) => err instanceof ApiError && err.code === "invalid_current_password",
  );
  assert.deepEqual(client.getQueryData(AUTH_QUERY_KEY), { username: "alice", role: "member" });

  // 3. Bare 401 from Live Tail / export signs out locally and clears sensitive storage.
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ error: "unauthorized" }), {
      status: 401,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;

  await assert.rejects(
    apiPostFormResponse("/logs/tail", { query: "*" }, { project: "alpha" }),
    (err: unknown) => err instanceof ApiError && err.status === 401,
  );
  assert.equal(client.getQueryData(AUTH_QUERY_KEY), null);
  assert.deepEqual(storage.snapshot(), { "lo-language": "zh" });
});
