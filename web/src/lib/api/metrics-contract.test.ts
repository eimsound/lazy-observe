import assert from "node:assert/strict";
import { test } from "vitest";

import {
  MetricsContractError,
  formatPromSeriesLabel,
  normalizePrettifyResponse,
  normalizePromInstantResponse,
  normalizePromRangeResponse,
  normalizePromStringsResponse,
} from "./metrics-contract.ts";

test("normalizePromRangeResponse parses real Prometheus/VictoriaMetrics matrix responses including NaN/Inf and notices", () => {
  const wire = {
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        {
          metric: { __name__: "http_requests_total", service: "api", status: "200" },
          values: [
            [1_700_000_000, "12.5"],
            [1_700_000_060, "NaN"],
            [1_700_000_120, "+Inf"],
            [1_700_000_180, "-Inf"],
          ],
        },
      ],
    },
    warnings: ["high cardinality"],
    infos: ["evaluated in 3ms"],
  };

  const normalized = normalizePromRangeResponse(wire);
  assert.equal(normalized.data.length, 1);
  assert.equal(normalized.data[0].name, 'http_requests_total{service="api", status="200"}');
  assert.deepEqual(normalized.data[0].points, [
    [1_700_000_000_000, 12.5],
    [1_700_000_060_000, null],
    [1_700_000_120_000, null],
    [1_700_000_180_000, null],
  ]);
  assert.deepEqual(normalized.warnings, ["high cardinality"]);
  assert.deepEqual(normalized.infos, ["evaluated in 3ms"]);
});

test("normalizePromRangeResponse rejects Prometheus error envelopes and wrong resultType", () => {
  assert.throws(
    () =>
      normalizePromRangeResponse({
        status: "error",
        errorType: "bad_data",
        error: "parse error at char 4",
      }),
    (err: unknown) =>
      err instanceof MetricsContractError &&
      err.status === 400 &&
      err.errorType === "bad_data" &&
      err.message === "parse error at char 4",
  );

  assert.throws(
    () =>
      normalizePromRangeResponse({
        status: "success",
        data: { resultType: "vector", result: [] },
      }),
    /resultType must be "matrix"/,
  );
});

test("normalizePromInstantResponse handles vector, matrix, scalar, and string result types", () => {
  const vector = normalizePromInstantResponse({
    status: "success",
    data: {
      resultType: "vector",
      result: [{ metric: { __name__: "up", job: "gateway" }, value: [1_700_000_000, "1"] }],
    },
  });
  assert.equal(vector.data.resultType, "vector");
  assert.equal(vector.data.rows[0].name, 'up{job="gateway"}');
  assert.deepEqual(vector.data.rows[0].values, [
    { timestamp: 1_700_000_000_000, raw: "1", value: 1 },
  ]);

  const scalar = normalizePromInstantResponse({
    status: "success",
    data: { resultType: "scalar", result: [1_700_000_000, "1700000000.25"] },
  });
  assert.equal(scalar.data.resultType, "scalar");
  assert.equal(scalar.data.rows[0].values[0].value, 1_700_000_000.25);

  const str = normalizePromInstantResponse({
    status: "success",
    data: { resultType: "string", result: [1_700_000_000, "hello"] },
  });
  assert.equal(str.data.resultType, "string");
  assert.equal(str.data.rows[0].values[0].raw, "hello");
  assert.equal(str.data.rows[0].values[0].value, null);

  const matrix = normalizePromInstantResponse({
    status: "success",
    data: {
      resultType: "matrix",
      result: [{ metric: { job: "api" }, values: [[1_700_000_000, "42"]] }],
    },
  });
  assert.equal(matrix.data.resultType, "matrix");
  assert.equal(matrix.data.rows[0].name, '{job="api"}');

  assert.throws(
    () => normalizePromInstantResponse({ status: "success", data: { resultType: "unknown", result: [] } }),
    /unsupported query\.data\.resultType/,
  );
});

test("normalizePromStringsResponse and normalizePrettifyResponse match gateway metadata & prettify contracts", () => {
  assert.deepEqual(
    normalizePromStringsResponse({ status: "success", data: ["__name__", "job", 123] }),
    ["__name__", "job"],
  );
  assert.deepEqual(normalizePromStringsResponse({ status: "success", data: null }), []);
  assert.throws(
    () => normalizePromStringsResponse({ status: "error", error: "bad matcher" }),
    (err: unknown) => err instanceof MetricsContractError && err.message === "bad matcher",
  );

  assert.equal(
    normalizePrettifyResponse({ status: "success", query: "sum(\n  rate(up[5m])\n)" }),
    "sum(\n  rate(up[5m])\n)",
  );
  assert.throws(
    () => normalizePrettifyResponse({ status: "error", msg: "unexpected EOF" }),
    (err: unknown) => err instanceof MetricsContractError && err.message === "unexpected EOF",
  );
  assert.equal(formatPromSeriesLabel({}), "{}");
});
