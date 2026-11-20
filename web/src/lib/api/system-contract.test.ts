import assert from "node:assert/strict";
import { afterEach, test, vi } from "vitest";

import {
  normalizeHealthDiagnostics,
  normalizeInboxDiagnostics,
} from "./system-contract.ts";
import { getHealthDiagnostics } from "./system.ts";

afterEach(() => {
  vi.unstubAllGlobals();
});

test("system health diagnostics expose only known components and stable states", () => {
  assert.deepEqual(normalizeHealthDiagnostics({
    status: "degraded",
    components: {
      gateway: "ok",
      logs: "component restarting at http://127.0.0.1:9428/private",
      traces: "dial tcp 10.0.0.4:10428",
      error_inbox: "active_items=0 active_bytes=0",
      malicious_component: "secret-key",
    },
  }), {
    status: "degraded",
    components: [
      { name: "gateway", state: "ok" },
      { name: "logs", state: "restarting" },
      { name: "traces", state: "unavailable" },
      { name: "error_inbox", state: "ok" },
    ],
  });
});

test("error_inbox distinguishes healthy summary strings from diagnostics unavailable", () => {
  assert.deepEqual(
    normalizeHealthDiagnostics({
      status: "ok",
      components: {
        gateway: "ok",
        error_inbox: "active_items=12 active_bytes=4096",
      },
    }).components,
    [
      { name: "gateway", state: "ok" },
      { name: "error_inbox", state: "ok" },
    ],
  );
  assert.deepEqual(
    normalizeHealthDiagnostics({
      status: "degraded",
      components: {
        gateway: "ok",
        error_inbox: "diagnostics unavailable",
      },
    }).components,
    [
      { name: "gateway", state: "ok" },
      { name: "error_inbox", state: "unavailable" },
    ],
  );
});

test("Inbox diagnostics discard payloads, event identities, and raw errors", () => {
  const diagnostics = normalizeInboxDiagnostics({
    pending_items: 2,
    processing_items: 1,
    active_items: 3,
    active_bytes: 4096,
    oldest_accepted_at: "2026-08-23T01:02:03Z",
    retries_total: 4,
    rejections_total: 5,
    terminal_failures_total: 6,
    drops_total: 7,
    payload: "must-not-escape",
    recent_terminal: [{
      event_id: "private-event",
      last_error: "http://127.0.0.1:9428/private",
    }],
  });

  assert.deepEqual(diagnostics, {
    pendingItems: 2,
    processingItems: 1,
    activeItems: 3,
    activeBytes: 4096,
    oldestAcceptedAt: "2026-08-23T01:02:03Z",
    retriesTotal: 4,
    rejectionsTotal: 5,
    terminalFailuresTotal: 6,
    dropsTotal: 7,
  });
  assert.equal(JSON.stringify(diagnostics).includes("private"), false);
});

test("getHealthDiagnostics accepts real Go /healthz HTTP 200 ok and HTTP 503 degraded responses", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(
        JSON.stringify({
          status: "ok",
          components: {
            gateway: "ok",
            metrics: "ok",
            logs: "ok",
            traces: "ok",
            vmagent: "ok",
            error_inbox: "active_items=0 active_bytes=0",
          },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    ),
  );
  const okDiag = await getHealthDiagnostics();
  assert.equal(okDiag.status, "ok");
  assert.equal(okDiag.components.every((c) => c.state === "ok"), true);

  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(
        JSON.stringify({
          status: "degraded",
          components: {
            gateway: "ok",
            logs: "component restarting",
            error_inbox: "diagnostics unavailable",
          },
        }),
        { status: 503, headers: { "Content-Type": "application/json" } },
      ),
    ),
  );
  const degradedDiag = await getHealthDiagnostics();
  assert.equal(degradedDiag.status, "degraded");
  assert.deepEqual(degradedDiag.components, [
    { name: "gateway", state: "ok" },
    { name: "logs", state: "restarting" },
    { name: "error_inbox", state: "unavailable" },
  ]);
});
