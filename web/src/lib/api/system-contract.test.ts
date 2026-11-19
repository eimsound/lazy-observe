import assert from "node:assert/strict";
import { test } from "vitest";

import {
  normalizeHealthDiagnostics,
  normalizeInboxDiagnostics,
} from "./system-contract.ts";

test("system health diagnostics expose only known components and stable states", () => {
  assert.deepEqual(normalizeHealthDiagnostics({
    status: "degraded",
    components: {
      gateway: "ok",
      logs: "component restarting at http://127.0.0.1:9428/private",
      traces: "dial tcp 10.0.0.4:10428",
      malicious_component: "secret-key",
    },
  }), {
    status: "degraded",
    components: [
      { name: "gateway", state: "ok" },
      { name: "logs", state: "restarting" },
      { name: "traces", state: "unavailable" },
    ],
  });
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
