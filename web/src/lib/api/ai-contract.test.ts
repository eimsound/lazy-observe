import assert from "node:assert/strict";
import { test } from "vitest";

import {
  buildAssistantQueryPayload,
  normalizeAiSettings,
  normalizeAiStatus,
  normalizeAiTestResult,
  normalizeAssistantAnswer,
  normalizeAssistantExplain,
  normalizeAssistantLogs,
  normalizeIssueAiAnalysis,
  resolveAssistantTimeRange,
} from "./ai-contract.ts";

test("assistant query payload encodes preset time ranges as backend {start, end} RFC3339 objects", () => {
  const nowMs = Date.UTC(2026, 10, 21, 10, 0, 0);
  const payload = buildAssistantQueryPayload(
    { question: "show recent 5xx errors", signal: "auto", time_range: "1h" },
    nowMs,
  );

  assert.deepEqual(payload, {
    question: "show recent 5xx errors",
    signal: "auto",
    time_range: {
      start: "2026-11-21T09:00:00.000Z",
      end: "2026-11-21T10:00:00.000Z",
    },
  });
  assert.ok(Date.parse(String(payload.time_range!.end)) > Date.parse(String(payload.time_range!.start)));
});

test("assistant query payload passes through explicit {start, end} and omits empty ranges", () => {
  assert.deepEqual(
    resolveAssistantTimeRange({ start: 1_700_000_000, end: 1_700_003_600 }),
    { start: 1_700_000_000, end: 1_700_003_600 },
  );
  assert.equal(resolveAssistantTimeRange(""), undefined);
  assert.deepEqual(
    buildAssistantQueryPayload({ question: "up" }),
    { question: "up" },
  );
});

test("normalizeAssistantLogs unpacks the gateway {rows, count, limit, truncated} envelope", () => {
  const wireResult = {
    rows: [
      {
        _time: "2026-11-21T09:59:00Z",
        _msg: "upstream timeout",
        _stream: '{app="checkout"}',
        level: "error",
        trace_id: "0123456789abcdef0123456789abcdef",
      },
      "ignore-non-object",
      {
        _time: "2026-11-21T09:59:05Z",
        _msg: "retrying request",
        _stream: '{app="checkout"}',
        level: "warn",
      },
    ],
    count: 2,
    limit: 100,
    truncated: false,
  };

  const rows = normalizeAssistantLogs(wireResult);
  assert.equal(rows.length, 2);
  assert.equal(rows[0].message, "upstream timeout");
  assert.equal(rows[0].level, "error");
  assert.equal(rows[0].traceId, "0123456789abcdef0123456789abcdef");
  assert.equal(rows[1].message, "retrying request");
  assert.equal(rows[1].level, "warn");
});

test("normalizeAssistantAnswer handles gateway logs envelope, metrics matrix, and execution errors", () => {
  const logsAnswer = normalizeAssistantAnswer({
    signal: "logs",
    query: 'level:="error"',
    explanation: "Matches recent error logs.",
    result: {
      rows: [{ _time: "2026-11-21T09:55:00Z", _msg: "db failed", level: "error" }],
      count: 1,
      limit: 100,
      truncated: false,
    },
  });
  assert.equal(logsAnswer.result?.kind, "logs");
  assert.equal(logsAnswer.result.rows.length, 1);
  assert.equal(logsAnswer.result.rows[0].message, "db failed");

  const metricsAnswer = normalizeAssistantAnswer({
    signal: "metrics",
    query: "up",
    explanation: "Instance availability.",
    result: {
      status: "success",
      data: {
        resultType: "matrix",
        result: [
          {
            metric: { __name__: "up", job: "api" },
            values: [
              [1_700_000_000, "1"],
              [1_700_000_060, "NaN"],
            ],
          },
        ],
      },
    },
  });
  assert.equal(metricsAnswer.result?.kind, "metrics");
  assert.equal(metricsAnswer.result.series.length, 1);
  assert.equal(metricsAnswer.result.series[0].name, 'up{job="api"}');
  assert.deepEqual(metricsAnswer.result.series[0].points, [
    [1_700_000_000_000, 1],
    [1_700_000_060_000, null],
  ]);

  const erroredAnswer = normalizeAssistantAnswer({
    signal: "logs",
    query: "bad query",
    explanation: "Broken query",
    execution_error: "parse error",
    result: { rows: [{ _msg: "should be ignored" }] },
  });
  assert.equal(erroredAnswer.executionError, "parse error");
  assert.equal(erroredAnswer.result, undefined);

  // Strict drift rejection: invalid signal or missing query must fail fast.
  assert.throws(
    () => normalizeAssistantAnswer({ signal: "traces", query: "x", explanation: "y" }),
    /signal/,
  );
  assert.throws(
    () => normalizeAssistantAnswer({ signal: "logs", query: "  ", explanation: "y" }),
    /query/,
  );
});

test("AI status, settings, test, and explain match gateway JSON shapes strictly", () => {
  assert.deepEqual(
    normalizeAiStatus({ enabled: true, provider: "openai", model: "gpt-4o-mini" }),
    { enabled: true, provider: "openai", model: "gpt-4o-mini" },
  );
  assert.throws(() => normalizeAiStatus({ enabled: "yes", provider: "openai", model: "m" }));

  // Mirrors aiSettingsResponse in internal/gateway/ai_settings.go (includes revision).
  assert.deepEqual(
    normalizeAiSettings({
      enabled: true,
      provider: "openai",
      base_url: "https://api.openai.com",
      model: "gpt-4o-mini",
      max_tokens: 1024,
      has_api_key: true,
      source: "database",
      updated_by: "admin",
      revision: 3,
    }),
    {
      enabled: true,
      provider: "openai",
      base_url: "https://api.openai.com",
      model: "gpt-4o-mini",
      max_tokens: 1024,
      has_api_key: true,
      source: "database",
      updated_by: "admin",
      revision: 3,
    },
  );
  assert.throws(() =>
    normalizeAiSettings({
      enabled: true,
      provider: "openai",
      base_url: "",
      model: "m",
      max_tokens: -1,
      has_api_key: false,
      source: "database",
      updated_by: "admin",
    }),
  );

  // Mirrors aiTest in internal/gateway/ai_api.go.
  assert.deepEqual(
    normalizeAiTestResult({ latency_ms: 142, model: "gpt-4o-mini", echo: "pong" }),
    { latency_ms: 142, model: "gpt-4o-mini", echo: "pong" },
  );
  assert.throws(() => normalizeAiTestResult({ latency_ms: "142", echo: "pong" }));

  // Mirrors aiAssistantExplain in internal/gateway/ai_assistant.go.
  assert.deepEqual(normalizeAssistantExplain({ explanation: "Counts 5xx per minute." }), {
    explanation: "Counts 5xx per minute.",
  });
  assert.throws(() => normalizeAssistantExplain({}));
});

test("normalizeIssueAiAnalysis matches Go issueAIResponse (RFC3339 ai_analysis_at and omitted stale)", () => {
  // Exact wire output of issueAIResponse(row, true) in internal/gateway/issue_ai_analysis.go:
  // row.AIAnalysisAt is *time.Time -> RFC3339 string, and `stale` is omitted when not stale.
  const freshWire = {
    cached: true,
    ai_analysis_at: "2026-11-21T09:00:00Z",
    summary: "Null dereference on empty cart.",
    probable_causes: [
      { cause: "Missing null check", confidence: "high", evidence: "checkout.ts:42" },
    ],
    suggested_actions: ["Guard empty cart before reading items"],
  };

  const normalized = normalizeIssueAiAnalysis(freshWire);
  assert.equal(normalized.cached, true);
  assert.equal(normalized.stale, false);
  assert.equal(normalized.ai_analysis_at, Date.parse("2026-11-21T09:00:00Z"));
  assert.equal(normalized.probable_causes[0].confidence, "high");

  const staleWire = { ...freshWire, cached: false, stale: true, ai_analysis_at: 1_795_165_200_000 };
  const normalizedStale = normalizeIssueAiAnalysis(staleWire);
  assert.equal(normalizedStale.stale, true);
  assert.equal(normalizedStale.ai_analysis_at, 1_795_165_200_000);

  assert.throws(
    () =>
      normalizeIssueAiAnalysis({
        ...freshWire,
        probable_causes: [{ cause: "x", confidence: "certain", evidence: "e" }],
      }),
    /confidence/,
  );
});
