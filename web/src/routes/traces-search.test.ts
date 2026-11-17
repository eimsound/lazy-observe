import assert from "node:assert/strict";
import test from "node:test";

import {
  buildEndpointTraceSearch,
  DEFAULT_TRACE_LIMIT,
  durationMicros,
  parseTraceSearchURL,
  traceSearchFromURL,
  traceSearchToURL,
} from "./traces-search.ts";

test("endpoint row round-trips through an escaped absolute trace search URL", () => {
  const params = buildEndpointTraceSearch(
    {
      service: "checkout api",
      method: "GET",
      route: "/orders/:id & returns",
      p95: 12.345,
    },
    { start: 1_787_472_000_000, end: 1_787_475_600_000 },
  );

  assert.deepEqual(params, {
    service: "checkout api",
    operation: "GET /orders/:id & returns",
    start: 1_787_472_000_000_000,
    end: 1_787_475_600_000_000,
    minDuration: "12345us",
    limit: 20,
  });

  const search = traceSearchToURL(params);
  assert.equal(search.start, 1_787_472_000_000_000);
  assert.equal(search.end, 1_787_475_600_000_000);
  assert.equal(search.limit, undefined);
  assert.equal(search.run, 1);

  const encoded = new URLSearchParams(
    Object.entries(search).map(([key, value]) => [key, String(value)]),
  ).toString();
  assert.match(encoded, /service=checkout\+api/);
  assert.match(encoded, /operation=GET\+%2Forders%2F%3Aid\+%26\+returns/);

  const raw = Object.fromEntries(new URLSearchParams(encoded));
  assert.deepEqual(traceSearchFromURL(parseTraceSearchURL(raw)), params);
});

test("trace duration codec accepts microseconds and milliseconds and emits canonical microseconds", () => {
  assert.equal(durationMicros("250us"), 250);
  assert.equal(durationMicros("250µs"), 250);
  assert.equal(durationMicros("0.75ms"), 750);
  assert.equal(durationMicros(" 12.345 ms "), 12_345);
  assert.equal(durationMicros("-1ms"), null);
  assert.equal(durationMicros("1second"), null);
});

test("invalid or incomplete trace URLs fall back without executing a drifting search", () => {
  const valid = traceSearchToURL({
    service: "checkout",
    start: 1_787_472_000_000_000,
    end: 1_787_475_600_000_000,
    minDuration: "1ms",
    limit: 20,
  });

  assert.equal(traceSearchFromURL(parseTraceSearchURL({ ...valid, run: undefined })), null);
  assert.equal(traceSearchFromURL(parseTraceSearchURL({ ...valid, service: " " })), null);
  assert.equal(traceSearchFromURL(parseTraceSearchURL({ ...valid, end: valid.start })), null);
  assert.equal(traceSearchFromURL(parseTraceSearchURL({ ...valid, start: "not-a-time" })), null);
  assert.equal(traceSearchFromURL(parseTraceSearchURL({ ...valid, minDuration: "NaNms" })), null);
  assert.equal(traceSearchFromURL(parseTraceSearchURL({ ...valid, limit: "0" })), null);
  assert.equal(
    traceSearchFromURL(parseTraceSearchURL({ ...valid }))?.limit,
    DEFAULT_TRACE_LIMIT,
  );
});
