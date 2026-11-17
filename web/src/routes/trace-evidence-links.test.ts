import assert from "node:assert/strict";
import test from "node:test";

import type { JaegerTrace } from "../lib/api/traces.ts";
import {
  buildTraceEvidenceLinks,
  parseMetricsEvidenceSearch,
  parseProfilingEvidenceSearch,
} from "./trace-evidence-links.ts";

const trace: JaegerTrace = {
  traceID: "0123456789abcdef0123456789abcdef",
  spans: [
    {
      traceID: "0123456789abcdef0123456789abcdef",
      spanID: "0123456789abcdef",
      operationName: "POST /orders/:id",
      startTime: 1_787_472_000_123_456,
      duration: 1_234_567,
      references: [],
      tags: [
        { key: "http.request.method", value: "POST" },
        { key: "http.route", value: "/orders/:id" },
      ],
      processID: "p1",
    },
  ],
  processes: {
    p1: { serviceName: 'checkout "edge"', tags: [] },
  },
};

test("trace evidence links separate exact logs from contextual profile and metrics", () => {
  const links = buildTraceEvidenceLinks(trace);

  assert.deepEqual(links.logs, {
    to: "/logs",
    relationship: "exact",
    search: { q: 'trace_id:"0123456789abcdef0123456789abcdef"', run: 1 },
  });
  assert.deepEqual(links.profiling, {
    to: "/profiling",
    relationship: "contextual",
    search: {
      service: 'checkout "edge"',
      start: 1_787_471_700,
      end: 1_787_472_302,
      evidence: "contextual",
    },
  });
  assert.equal(links.metrics.relationship, "contextual");
  assert.equal(links.metrics.to, "/metrics");
  assert.deepEqual(
    {
      start: links.metrics.search.start,
      end: links.metrics.search.end,
      run: links.metrics.search.run,
      evidence: links.metrics.search.evidence,
    },
    {
      start: 1_787_471_700,
      end: 1_787_472_302,
      run: "1",
      evidence: "contextual",
    },
  );
  assert.match(links.metrics.search.q, /http\.server\.request\.duration_count/);
  assert.match(links.metrics.search.q, /service\\\.name="checkout \\"edge\\""/);
  assert.match(links.metrics.search.q, /http\\\.request\\\.method="POST"/);
  assert.match(links.metrics.search.q, /http\\\.route="\/orders\/:id"/);
});

test("evidence URL parsers preserve valid absolute windows and reject drifting fallbacks", () => {
  assert.deepEqual(
    parseProfilingEvidenceSearch({
      service: "checkout",
      type: "cpu",
      start: "100",
      end: 200,
      evidence: "contextual",
    }),
    {
      service: "checkout",
      type: "cpu",
      start: 100,
      end: 200,
      evidence: "contextual",
    },
  );
  assert.deepEqual(
    parseMetricsEvidenceSearch({
      q: "up",
      run: 1,
      start: "100",
      end: "200",
      evidence: "contextual",
    }),
    {
      q: "up",
      run: "1",
      start: 100,
      end: 200,
      evidence: "contextual",
    },
  );
  assert.deepEqual(
    parseMetricsEvidenceSearch({ q: "up", run: "1", start: "bad", end: 200 }),
    { q: "up", run: "1" },
  );
});
