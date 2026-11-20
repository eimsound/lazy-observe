import assert from "node:assert/strict";
import { test } from "vitest";

import {
  normalizeJaegerTrace,
  normalizeJaegerTraceListResponse,
  normalizeTraceStringListResponse,
} from "./traces-contract.ts";
import { findRootSpan, isErrorSpan, summarizeTrace } from "./traces.ts";

test("normalizeTraceStringListResponse handles services/operations payloads including null data", () => {
  assert.deepEqual(normalizeTraceStringListResponse({ data: ["checkout", "billing"] }), [
    "checkout",
    "billing",
  ]);
  assert.deepEqual(normalizeTraceStringListResponse({ data: null }), []);
  assert.deepEqual(normalizeTraceStringListResponse({ data: ["valid", 42, null] }), ["valid"]);
  assert.throws(() => normalizeTraceStringListResponse("not-json-object"), /must be an object/);
  assert.throws(() => normalizeTraceStringListResponse({ data: "bad" }), /array or null/);
});

test("normalizeJaegerTraceListResponse parses real VictoriaTraces Jaeger JSON responses with nested spans, refs, tags, logs, and processes", () => {
  const wire = {
    data: [
      {
        traceID: "0123456789abcdef0123456789abcdef",
        spans: [
          {
            traceID: "0123456789abcdef0123456789abcdef",
            spanID: "1111111111111111",
            operationName: "POST /checkout",
            startTime: 1_787_472_000_000_000,
            duration: 250_000,
            references: [],
            tags: [
              { key: "http.request.method", type: "string", value: "POST" },
              { key: "http.status_code", type: "int64", value: 503 },
            ],
            logs: [
              {
                timestamp: 1_787_472_000_100_000,
                fields: [{ key: "event", type: "string", value: "retry" }],
              },
            ],
            processID: "p1",
            warnings: null,
          },
          {
            traceID: "0123456789abcdef0123456789abcdef",
            spanID: "2222222222222222",
            operationName: "SELECT orders",
            startTime: 1_787_472_000_050_000,
            duration: 120_000,
            references: [
              {
                refType: "CHILD_OF",
                traceID: "0123456789abcdef0123456789abcdef",
                spanID: "1111111111111111",
              },
            ],
            tags: [{ key: "otel.status_code", type: "string", value: "ERROR" }],
            processID: "p2",
          },
        ],
        processes: {
          p1: { serviceName: "checkout", tags: [{ key: "env", value: "prod" }] },
          p2: { serviceName: "postgres", tags: [] },
        },
        warnings: null,
      },
    ],
  };

  const traces = normalizeJaegerTraceListResponse(wire);
  assert.equal(traces.length, 1);
  const trace = traces[0];
  assert.equal(trace.spans.length, 2);
  assert.equal(trace.spans[0].logs?.length, 1);
  assert.equal(findRootSpan(trace)?.spanID, "1111111111111111");
  assert.equal(isErrorSpan(trace.spans[0]), true);
  assert.equal(isErrorSpan(trace.spans[1]), true);

  const summary = summarizeTrace(trace);
  assert.deepEqual(summary, {
    traceID: "0123456789abcdef0123456789abcdef",
    rootName: "POST /checkout",
    rootService: "checkout",
    spanCount: 2,
    serviceCount: 2,
    errorCount: 2,
    startTime: 1_787_472_000_000_000,
    duration: 250_000,
  });

  // Null / empty results from VictoriaTraces are valid empty lists.
  assert.deepEqual(normalizeJaegerTraceListResponse({ data: null }), []);
  assert.deepEqual(normalizeJaegerTraceListResponse({ data: [] }), []);
});

test("normalizeJaegerTrace rejects malformed span identifiers and negative durations", () => {
  assert.throws(
    () => normalizeJaegerTrace({ traceID: "", spans: [], processes: {} }),
    /traceID/,
  );
  assert.throws(
    () =>
      normalizeJaegerTrace({
        traceID: "abc",
        spans: [
          {
            traceID: "abc",
            spanID: "s1",
            operationName: "op",
            startTime: 1000,
            duration: -5,
            references: [],
            tags: [],
            processID: "p1",
          },
        ],
        processes: {},
      }),
    /duration/,
  );
});
