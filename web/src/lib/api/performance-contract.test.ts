import assert from "node:assert/strict";
import test from "node:test";

import { normalizeEndpointStatsResponse } from "./performance-contract.ts";

test("normalizes the trace-derived endpoint envelope and time series", () => {
  const response = normalizeEndpointStatsResponse(JSON.parse(`{
    "start": 1784368800000,
    "end": 1784368920000,
    "step_seconds": 60,
    "endpoints": [{
      "service": "checkout",
      "method": "POST",
      "route": "/orders/:id",
      "throughput": 0.025,
      "p50": 30,
      "p95": 48,
      "p99": 49.6,
      "error_rate": 0.3333333333333333,
      "series": [{
        "time": 1784368800000,
        "throughput": 0.03333333333333333,
        "p50": 20,
        "p95": 29,
        "p99": 29.8,
        "error_rate": 0.5
      }]
    }],
    "truncated": false
  }`));

  assert.equal(response.endpoints[0]?.service, "checkout");
  assert.equal(response.endpoints[0]?.p99, 49.6);
  assert.equal(response.endpoints[0]?.series[0]?.p95, 29);
});

test("rejects legacy arrays and incomplete endpoint statistics", () => {
  assert.throws(() => normalizeEndpointStatsResponse([]), /must be an object/);
  assert.throws(
    () =>
      normalizeEndpointStatsResponse({
        start: 1,
        end: 2,
        step_seconds: 1,
        endpoints: [
          {
            service: "api",
            method: "GET",
            route: "/healthz",
            throughput: 1,
            p50: 1,
            p95: 2,
            p99: 3,
            error_rate: 0,
          },
        ],
        truncated: false,
      }),
    /series must be an array/,
  );
});

test("rejects impossible rates and non-finite derived values", () => {
  const valid = {
    start: 1,
    end: 2,
    step_seconds: 1,
    endpoints: [
      {
        service: "api",
        method: "GET",
        route: "/healthz",
        throughput: 1,
        p50: 1,
        p95: 2,
        p99: 3,
        error_rate: 0,
        series: [],
      },
    ],
    truncated: false,
  };
  assert.throws(
    () => normalizeEndpointStatsResponse({ ...valid, endpoints: [{ ...valid.endpoints[0], error_rate: 1.1 }] }),
    /must be at most 1/,
  );
  assert.throws(
    () => normalizeEndpointStatsResponse({ ...valid, endpoints: [{ ...valid.endpoints[0], p95: Number.NaN }] }),
    /non-negative finite number/,
  );
});
