import assert from "node:assert/strict";
import { afterEach, test, vi } from "vitest";

import {
  evaluationIntervalProblem,
  repeatIntervalProblem,
  normalizeAlertEventList,
  normalizeAlertRuleList,
  parsePromDurationMs,
  normalizeChannel,
  normalizeChannelList,
  normalizeChannelTestResult,
  normalizeInhibitionRule,
  normalizeInhibitionRuleList,
  normalizeSilence,
  normalizeSilenceList,
} from "./alerts-contract.ts";
import {
  createInhibitionRule,
  deleteInhibitionRule,
  listInhibitionRules,
  testChannel,
  updateInhibitionRule,
} from "./alerts.ts";
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
      evaluation_interval: "30s",
      effective_evaluation_interval: "30s",
      severity: "critical",
      labels: { team: "sre" },
      annotations: { summary: "down" },
      enabled: true,
      channel_ids: ["1", "2"],
      created_at: "2026-11-21T00:00:00Z",
      updated_at: "2026-11-21T00:00:00Z",
    },
    {
      id: "8",
      name: "ErrorLogs",
      signal: "logs",
      evaluation_engine: "native",
      expr: "error | stats count() as n | filter n:>0",
      for: "0s",
      evaluation_interval: "",
      effective_evaluation_interval: "1m",
      severity: "warning",
      labels: {},
      annotations: {},
      enabled: true,
      channel_ids: [],
    },
  ]);
  assert.equal(rules.length, 2);
  assert.deepEqual(rules[0].channel_ids, ["1", "2"]);
  assert.equal(rules[0].evaluation_interval, "30s");
  assert.equal(rules[0].effective_evaluation_interval, "30s");
  assert.equal(rules[1].evaluation_interval, "");
  assert.equal(rules[1].effective_evaluation_interval, "1m");
  assert.throws(
    () => normalizeAlertRuleList([{ ...rules[0], evaluation_interval: 30 }]),
    /alert rule\.evaluation_interval/,
  );

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

test("inhibition rules contract aligns with Go meta.InhibitionRuleView including omitempty fields", () => {
  // Exact Go meta.InhibitionRuleView when enabled=true and all optional fields present
  const enabledWire = {
    id: 14,
    project_id: 3,
    source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
    target_matchers: [{ name: "service", op: "=~", value: "api-.*" }],
    equal_labels: ["instance", "cluster"],
    enabled: true,
    comment: "HostDown suppresses service alerts",
    created_at: "2026-11-21T00:00:00Z",
    updated_at: "2026-11-21T00:05:00Z",
  };
  assert.deepEqual(normalizeInhibitionRule(enabledWire), {
    id: "14",
    project_id: "3",
    source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
    target_matchers: [{ name: "service", op: "=~", value: "api-.*" }],
    equal_labels: ["instance", "cluster"],
    enabled: true,
    comment: "HostDown suppresses service alerts",
    created_at: "2026-11-21T00:00:00Z",
    updated_at: "2026-11-21T00:05:00Z",
  });

  // Exact Go meta.InhibitionRuleView when enabled=false, equal_labels=[], comment="":
  // Go `omitempty` omits `enabled`, `equal_labels`, and `comment` completely from JSON.
  const disabledOmitEmptyWire = {
    id: 15,
    project_id: 3,
    source_matchers: [{ name: "severity", op: "=", value: "critical" }],
    target_matchers: [{ name: "severity", op: "=", value: "warning" }],
    created_at: "2026-11-21T00:00:00Z",
    updated_at: "2026-11-21T00:05:00Z",
  };
  assert.deepEqual(normalizeInhibitionRule(disabledOmitEmptyWire), {
    id: "15",
    project_id: "3",
    source_matchers: [{ name: "severity", op: "=", value: "critical" }],
    target_matchers: [{ name: "severity", op: "=", value: "warning" }],
    equal_labels: [],
    enabled: false,
    comment: "",
    created_at: "2026-11-21T00:00:00Z",
    updated_at: "2026-11-21T00:05:00Z",
  });

  assert.deepEqual(normalizeInhibitionRuleList(null), []);
  assert.equal(normalizeInhibitionRuleList([enabledWire, disabledOmitEmptyWire]).length, 2);

  // Backend matcher.go only supports "=" and "=~"; reject unsupported operators.
  assert.throws(
    () =>
      normalizeInhibitionRule({
        ...enabledWire,
        source_matchers: [{ name: "alertname", op: "!=", value: "HostDown" }],
      }),
    /source_matchers\[0\]\.op/,
  );
  assert.throws(
    () =>
      normalizeInhibitionRule({
        ...enabledWire,
        target_matchers: [{ name: "service", op: "!~", value: "api-.*" }],
      }),
    /target_matchers\[0\]\.op/,
  );
  assert.throws(
    () => normalizeInhibitionRule({ ...enabledWire, enabled: "true" }),
    /inhibition rule\.enabled/,
  );
  assert.throws(
    () => normalizeInhibitionRule({ ...enabledWire, equal_labels: [42] }),
    /equal_labels\[0\]/,
  );
});

test("inhibition rules API client uses /api/inhibition-rules with project scope, CSRF, and surfaces HTTP errors", async () => {
  installBrowserOrigin();

  const calls: Array<{ url: string; init?: RequestInit }> = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      calls.push({ url, init });
      if (init?.method === "POST") {
        return new Response(
          JSON.stringify({
            id: 21,
            project_id: 2,
            source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
            target_matchers: [{ name: "alertname", op: "=", value: "ServiceDown" }],
            equal_labels: ["instance"],
            enabled: true,
            comment: "host down",
            created_at: "2026-11-21T00:00:00Z",
            updated_at: "2026-11-21T00:00:00Z",
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        );
      }
      if (init?.method === "PATCH") {
        return new Response(
          JSON.stringify({
            id: 21,
            project_id: 2,
            source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
            target_matchers: [{ name: "alertname", op: "=", value: "ServiceDown" }],
            equal_labels: ["instance"],
            comment: "host down",
            created_at: "2026-11-21T00:00:00Z",
            updated_at: "2026-11-21T00:01:00Z",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      if (init?.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      return new Response(JSON.stringify([]), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );

  const headerOf = (init: RequestInit | undefined, name: string): string | undefined => {
    const headers = init?.headers as Record<string, string> | undefined;
    return headers?.[name];
  };

  const list = await listInhibitionRules(undefined, "proj-alpha");
  assert.deepEqual(list, []);
  assert.equal(calls[0].url, "/api/inhibition-rules");
  assert.equal(headerOf(calls[0].init, "X-LO-Project"), "proj-alpha");

  const created = await createInhibitionRule(
    {
      source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
      target_matchers: [{ name: "alertname", op: "=", value: "ServiceDown" }],
      equal_labels: ["instance"],
      enabled: true,
      comment: "host down",
    },
    "proj-alpha",
  );
  assert.equal(created.id, "21");
  assert.equal(created.enabled, true);
  assert.equal(headerOf(calls[1].init, "X-CSRF-Token"), "test-token");
  assert.equal(headerOf(calls[1].init, "X-LO-Project"), "proj-alpha");

  // Disable via PATCH; response omits `enabled` due to Go `omitempty`, normalized to `false`.
  const disabled = await updateInhibitionRule("21", { enabled: false }, "proj-alpha");
  assert.equal(calls[2].url, "/api/inhibition-rules/21");
  assert.equal(disabled.enabled, false);

  await deleteInhibitionRule("21", "proj-alpha");
  assert.equal(calls[3].url, "/api/inhibition-rules/21");
  assert.equal(calls[3].init?.method, "DELETE");

  // Never swallow 403 / 404 / 500 errors.
  for (const [status, message] of [
    [403, "forbidden"],
    [404, "project_not_found"],
    [500, "internal error"],
  ] as const) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ error: message }), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    await assert.rejects(
      () => listInhibitionRules(undefined, "proj-alpha"),
      (err: unknown) => err instanceof ApiError && err.status === status && err.message === message,
    );
  }
});

test("evaluation interval pre-check mirrors the server bounds in internal/alert", () => {
  assert.equal(parsePromDurationMs("1m30s"), 90_000);
  assert.equal(parsePromDurationMs("1h"), 3_600_000);
  assert.equal(parsePromDurationMs("abc"), null);
  assert.equal(parsePromDurationMs(""), null);
  assert.equal(parsePromDurationMs("60"), null);

  // Empty means "use the signal default".
  assert.equal(evaluationIntervalProblem("metrics", ""), null);
  assert.equal(evaluationIntervalProblem("logs", "  "), null);

  assert.equal(evaluationIntervalProblem("metrics", "5s"), null);
  assert.equal(evaluationIntervalProblem("metrics", "1h"), null);
  assert.equal(evaluationIntervalProblem("metrics", "4s"), "range");
  assert.equal(evaluationIntervalProblem("metrics", "7s"), "range");
  assert.equal(evaluationIntervalProblem("metrics", "1h5s"), "range");
  assert.equal(evaluationIntervalProblem("metrics", "2500ms"), "range");
  assert.equal(evaluationIntervalProblem("metrics", "0s"), "range");
  assert.equal(evaluationIntervalProblem("metrics", "fast"), "format");
  assert.equal(evaluationIntervalProblem("metrics", "60"), "format");

  assert.equal(evaluationIntervalProblem("logs", "25s"), "range");
  assert.equal(evaluationIntervalProblem("logs", "30s"), null);
  assert.equal(evaluationIntervalProblem("logs", "1m30s"), null);
});

test("alert rules carry repeat_interval and the read-only effective_repeat_interval", () => {
  const base = {
    id: "9",
    name: "DiskFull",
    signal: "metrics",
    evaluation_engine: "native",
    expr: "up == 0",
    for: "5m",
    severity: "warning",
    labels: {},
    annotations: {},
    enabled: true,
    channel_ids: [],
  };
  const [custom, inherited, legacy] = normalizeAlertRuleList([
    { ...base, repeat_interval: "15m", effective_repeat_interval: "15m" },
    { ...base, id: "10", repeat_interval: "", effective_repeat_interval: "4h" },
    // Older servers omit both fields: treat as inheriting the documented default.
    { ...base, id: "11" },
  ]);
  assert.equal(custom.repeat_interval, "15m");
  assert.equal(custom.effective_repeat_interval, "15m");
  assert.equal(inherited.repeat_interval, "");
  assert.equal(inherited.effective_repeat_interval, "4h");
  assert.equal(legacy.repeat_interval, "");
  assert.equal(legacy.effective_repeat_interval, "1h");
  assert.throws(
    () => normalizeAlertRuleList([{ ...base, repeat_interval: 60 }]),
    /alert rule\.repeat_interval/,
  );
});

test("repeatIntervalProblem mirrors the server's 1m–7d whole-second bounds", () => {
  for (const ok of ["", "  ", "1m", "60s", "1h", "90m", "1d", "7d", "168h", "1w"]) {
    assert.equal(repeatIntervalProblem(ok), null, ok);
  }
  for (const bad of ["soon", "1 h", "-1h", "1.5h"]) {
    assert.equal(repeatIntervalProblem(bad), "format", bad);
  }
  for (const out of ["0s", "59s", "7d1s", "8d", "1m500ms"]) {
    assert.equal(repeatIntervalProblem(out), "range", out);
  }
});
