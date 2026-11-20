import assert from "node:assert/strict";
import { afterEach, test, vi } from "vitest";

import {
  normalizeAlertEventList,
  normalizeAlertRuleList,
  normalizeChannel,
  normalizeChannelList,
  normalizeChannelTestResult,
  normalizeSilence,
  normalizeSilenceList,
} from "./alerts-contract.ts";
import { testChannel } from "./alerts.ts";
import { ApiError } from "./client.ts";

const originalWindow = globalThis.window;
const originalDocument = globalThis.document;

function installBrowserOrigin() {
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "http://localhost:5173" } },
  });
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: { cookie: "lo_csrf=test-token" },
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

test("GET /api/channels accepts both member minimal read-only projection and admin redacted config", () => {
  // Exact Go notificationChannelSummaryResponse (member role, PR #229):
  // intentionally omits `config`, `created_at`, `updated_at`.
  const memberWire = [
    { id: "1", type: "webhook", name: "Ops Webhook", enabled: true },
    { id: "2", type: "email", name: "On-call Email", enabled: false },
  ];
  assert.deepEqual(normalizeChannelList(memberWire), [
    {
      id: "1",
      type: "webhook",
      name: "Ops Webhook",
      config: {},
      enabled: true,
      created_at: "",
      updated_at: "",
    },
    {
      id: "2",
      type: "smtp",
      name: "On-call Email",
      config: {},
      enabled: false,
      created_at: "",
      updated_at: "",
    },
  ]);

  // Exact Go notificationChannelResponse (admin role, redacted secrets):
  const adminWire = {
    id: "1",
    type: "webhook",
    name: "Ops Webhook",
    config: { url: "https://hooks.example.com/lo/ops", secret: "[REDACTED]" },
    enabled: true,
    created_at: "2026-11-21T00:00:00Z",
    updated_at: "2026-11-21T00:05:00Z",
  };
  assert.deepEqual(normalizeChannel(adminWire), adminWire);
  assert.throws(() => normalizeChannel({ id: "1", type: "pagerduty", name: "Bad", enabled: true }), /channel\.type/);
});

test("POST /api/channels/:id/test validates 200 JSON and surfaces 502/404 as ApiError", async () => {
  // Exact Go channelTestResponse on 200 OK:
  assert.deepEqual(
    normalizeChannelTestResult({ ok: true, message: "test notification sent" }),
    { ok: true, message: "test notification sent" },
  );
  assert.throws(() => normalizeChannelTestResult({ ok: "yes", message: "ok" }), /channel test\.ok/);

  installBrowserOrigin();

  // Exact Go 502 Bad Gateway response from testChannel when upstream fails:
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify({ error: "webhook returned HTTP 500" }), {
        status: 502,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
  await assert.rejects(
    () => testChannel("1"),
    (err: unknown) =>
      err instanceof ApiError &&
      err.status === 502 &&
      err.message === "webhook returned HTTP 500",
  );

  // Exact Go 404 Not Found response from testChannel:
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify({ error: "not found" }), {
        status: 404,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
  await assert.rejects(
    () => testChannel("999"),
    (err: unknown) =>
      err instanceof ApiError &&
      err.status === 404 &&
      err.message === "not found",
  );
});

test("silences contract accepts empty comment and omitted comment from Go silenceResponse", () => {
  // Exact Go silenceResponse with empty comment "":
  const wireWithEmptyComment = {
    id: "12",
    matchers: [
      { name: "service", op: "=", value: "checkout" },
      { name: "instance", op: "=~", value: "web-.*" },
    ],
    starts_at: "2026-11-21T00:00:00Z",
    ends_at: "2026-11-21T02:00:00Z",
    comment: "",
    creator: "alice",
    created_at: "2026-11-21T00:00:00Z",
  };
  assert.deepEqual(normalizeSilence(wireWithEmptyComment), wireWithEmptyComment);

  // Also tolerates omitted comment (normalizes to ""):
  const wireWithoutComment = { ...wireWithEmptyComment };
  delete (wireWithoutComment as { comment?: string }).comment;
  assert.equal(normalizeSilence(wireWithoutComment).comment, "");

  assert.deepEqual(normalizeSilenceList([wireWithEmptyComment]), [wireWithEmptyComment]);
  assert.throws(
    () =>
      normalizeSilence({
        ...wireWithEmptyComment,
        matchers: [{ name: "service", op: "!=", value: "checkout" }],
      }),
    /matchers\[0\]\.op/,
  );
  assert.throws(
    () => normalizeSilence({ ...wireWithEmptyComment, comment: 123 }),
    /silence\.comment/,
  );
});

test("alert rules and alert events match Go gateway alertRuleResponse & alertEventResponse", () => {
  const rules = normalizeAlertRuleList([
    {
      id: "7",
      name: "HighErrorRate",
      signal: "metrics",
      evaluation_engine: "native",
      expr: "up == 0",
      for: "5m",
      severity: "critical",
      labels: { team: "sre" },
      annotations: { summary: "down" },
      enabled: true,
      channel_ids: ["1", "2"],
      created_at: "2026-11-21T00:00:00Z",
      updated_at: "2026-11-21T00:00:00Z",
    },
  ]);
  assert.equal(rules.length, 1);
  assert.deepEqual(rules[0].channel_ids, ["1", "2"]);

  const events = normalizeAlertEventList([
    {
      id: "evt-1",
      fingerprint: "fp-1",
      rule: "HighErrorRate",
      status: "firing",
      labels: { alertname: "HighErrorRate", severity: "critical" },
      annotations: { summary: "down" },
      starts_at: "2026-11-21T00:00:00Z",
      inhibited: false,
      updated_at: "2026-11-21T00:01:00Z",
    },
  ]);
  assert.equal(events.length, 1);
  assert.equal(events[0].ends_at, undefined);
  assert.equal(events[0].inhibited, false);
});
