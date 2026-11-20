import assert from "node:assert/strict";
import { test } from "vitest";
import { QueryClient } from "@tanstack/react-query";

import {
  AUTH_QUERY_KEY,
  PROJECTS_QUERY_KEY,
  handleSessionExpiry,
  isProjectNotFoundError,
  isSessionExpired,
  performLogout,
  registerLocalSignOutCleanup,
  type StorageIterable,
} from "./session-expiry.ts";

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

function signedInClient(): QueryClient {
  const client = new QueryClient();
  client.setQueryData(AUTH_QUERY_KEY, { username: "previous-user" });
  client.setQueryData(["issues"], ["private issue"]);
  client.setQueryData(PROJECTS_QUERY_KEY, [{ slug: "alpha" }]);
  return client;
}

test("a bare 401 or explicit session-expiry code signs out and clears user/project storage", () => {
  const client = signedInClient();
  const storage = memoryStorage({
    "lo-project": "deleted-project",
    "lo-mock-session": '{"username":"alice","role":"admin"}',
    "lo-assistant-history": '[{"id":"legacy"}]',
    "lo-assistant-history:u%3Aalice:alpha": '[{"id":"scoped"}]',
    "lo-logs-explorer-history:alpha": '[{"query":"secret"}]',
    "lo-metrics-history:alpha": '[{"query":"secret_metric"}]',
    "lo-language": "zh",
    "lo-theme": "dark",
  });
  let scopeCleared = false;
  let secondCleanupRan = false;
  const unregisterThrowing = registerLocalSignOutCleanup(() => {
    throw new Error("cleanup boom");
  });
  const unregisterFirst = registerLocalSignOutCleanup(() => {
    scopeCleared = true;
  });
  const unregisterSecond = registerLocalSignOutCleanup(() => {
    secondCleanupRan = true;
  });

  try {
    handleSessionExpiry(client, { status: 401 }, storage);
    assert.equal(scopeCleared, true);
    assert.equal(secondCleanupRan, true);
    assert.equal(client.getQueryData(AUTH_QUERY_KEY), null);
    assert.equal(client.getQueryData(["issues"]), undefined);
    assert.equal(client.getQueryData(PROJECTS_QUERY_KEY), undefined);
    assert.deepEqual(storage.snapshot(), {
      "lo-language": "zh",
      "lo-theme": "dark",
    });

    assert.equal(isSessionExpired({ status: 401, code: "session_expired" }), true);
    assert.equal(isSessionExpired({ status: 401, code: "unauthorized" }), true);
    assert.equal(isSessionExpired({ status: 401, code: "unauthenticated" }), true);
  } finally {
    unregisterThrowing();
    unregisterFirst();
    unregisterSecond();
  }
});

test("coded business 401s, 403s, and 404 project_not_found do not sign the user out", () => {
  const client = signedInClient();
  const storage = memoryStorage({ "lo-project": "alpha" });

  handleSessionExpiry(client, { status: 401, code: "invalid_current_password" }, storage);
  handleSessionExpiry(client, { status: 401, code: "project_not_found" }, storage);
  handleSessionExpiry(client, { status: 403 }, storage);
  handleSessionExpiry(client, { status: 404, message: "AI analysis not found" }, storage);
  handleSessionExpiry(client, new Error("network"), storage);

  assert.deepEqual(client.getQueryData(AUTH_QUERY_KEY), { username: "previous-user" });
  assert.deepEqual(client.getQueryData(["issues"]), ["private issue"]);
  assert.deepEqual(storage.snapshot(), { "lo-project": "alpha" });
  assert.equal(client.getQueryState(PROJECTS_QUERY_KEY)?.isInvalidated, false);

  assert.equal(isProjectNotFoundError({ status: 404, code: "project_not_found" }), true);
  assert.equal(isProjectNotFoundError({ status: 404, message: "project not found" }), true);
  assert.equal(isProjectNotFoundError({ status: 404, message: "AI analysis not found" }), false);
  handleSessionExpiry(client, { status: 404, code: "project_not_found" }, storage);
  assert.deepEqual(client.getQueryData(AUTH_QUERY_KEY), { username: "previous-user" });
  assert.equal(client.getQueryState(PROJECTS_QUERY_KEY)?.isInvalidated, true);

  const clientUncoded = signedInClient();
  handleSessionExpiry(clientUncoded, { status: 404, message: "project not found" }, storage);
  assert.deepEqual(clientUncoded.getQueryData(AUTH_QUERY_KEY), { username: "previous-user" });
  assert.equal(clientUncoded.getQueryState(PROJECTS_QUERY_KEY)?.isInvalidated, true);
});

test("performLogout recovers from 401 expired session and 403 CSRF failure without trapping user", async () => {
  for (const status of [401, 403]) {
    const client = signedInClient();
    const storage = memoryStorage({
      "lo-project": "alpha",
      "lo-logs-explorer-history:alpha": "[]",
    });

    await performLogout(
      async () => {
        throw Object.assign(new Error("rejected"), { status });
      },
      client,
      storage,
    );

    assert.equal(client.getQueryData(AUTH_QUERY_KEY), null);
    assert.equal(client.getQueryData(["issues"]), undefined);
    assert.deepEqual(storage.snapshot(), {});
  }
});

test("performLogout rethrows 5xx errors so the user can retry server-side logout", async () => {
  const client = signedInClient();
  const storage = memoryStorage({ "lo-project": "alpha" });

  await assert.rejects(
    performLogout(
      async () => {
        throw Object.assign(new Error("server unavailable"), { status: 503 });
      },
      client,
      storage,
    ),
    /server unavailable/,
  );

  assert.deepEqual(client.getQueryData(AUTH_QUERY_KEY), { username: "previous-user" });
  assert.deepEqual(storage.snapshot(), { "lo-project": "alpha" });
});
