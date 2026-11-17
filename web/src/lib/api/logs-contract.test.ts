import assert from "node:assert/strict";
import test from "node:test";

import {
  buildLogsExtraFilterParams,
  type LogsExtraFilter,
} from "../logs-explorer.ts";
import {
  createNdjsonParser,
  normalizeFieldValuesResponse,
  normalizeHits,
  normalizeHitsSeries,
  normalizeStatsQueryRange,
  parseJsonLines,
} from "./logs-contract.ts";

test("parses and normalizes JSON-lines while preserving the original fields", () => {
  const rows = parseJsonLines([
    '{"_time":"2026-07-24T10:00:00Z","_msg":"request failed","level":"ERROR","trace_id":"abc","n":1}',
    "not json",
    "",
    '{"_time":1753351201,"_msg":"warning: retrying","_stream":"{app=\\"api\\"}"}',
  ].join("\n"));

  assert.equal(rows.length, 2);
  assert.deepEqual(rows[0], {
    id: "1784887200000-0",
    time: 1784887200000,
    timeRaw: "2026-07-24T10:00:00Z",
    message: "request failed",
    stream: "",
    level: "error",
    traceId: "abc",
    raw: {
      _time: "2026-07-24T10:00:00Z",
      _msg: "request failed",
      level: "ERROR",
      trace_id: "abc",
      n: 1,
    },
  });
  assert.equal(rows[1].level, "warn");
  assert.equal(rows[1].time, 1753351201000);
  // The malformed and blank physical lines still reserve line indexes.
  assert.equal(rows[1].id, "1753351201000-3");
});

test("normalizes grouped hit series and sums the legacy histogram by timestamp", () => {
  const response = {
    hits: [
      {
        fields: { app: "api" },
        timestamps: ["2026-07-24T10:01:00Z", "2026-07-24T10:00:00Z"],
        values: ["3", 2],
        total: 5,
      },
      {
        fields: { app: "worker" },
        timestamps: ["2026-07-24T10:01:00Z"],
        values: [7],
      },
      {
        fields: {},
        timestamps: ["2026-07-24T10:00:00Z"],
        values: [11],
        total: 11,
      },
    ],
  };

  const series = normalizeHitsSeries(response);
  assert.equal(series.length, 3);
  assert.equal(series[0].isOther, true);
  assert.deepEqual(series[1].fields, { app: "worker" });
  assert.deepEqual(series[2].timestamps, [1784887200000, 1784887260000]);
  assert.deepEqual(series[2].values, [2, 3]);

  assert.deepEqual(normalizeHits(response), {
    timestamps: [1784887200000, 1784887260000],
    values: [13, 10],
  });
});

test("normalizes stats matrices and metadata without trusting malformed rows", () => {
  assert.deepEqual(normalizeStatsQueryRange({
    status: "success",
    data: {
      resultType: "matrix",
      result: [
        {
          metric: { __name__: "logs", level: "error", numeric: 42 },
          values: [[1753351200, "4"], ["bad", "9"], [1753351260, 5]],
        },
        null,
      ],
    },
  }), {
    status: "success",
    data: {
      resultType: "matrix",
      result: [{
        metric: { __name__: "logs", level: "error", numeric: "42" },
        values: [
          { timestamp: 1753351200, value: "4" },
          { timestamp: 1753351260, value: "5" },
        ],
      }],
    },
  });

  assert.deepEqual(normalizeFieldValuesResponse({
    values: [
      { value: "api", hits: "12" },
      { value: "worker", hits: null },
      { value: 7, hits: 99 },
    ],
  }), {
    values: [
      { value: "api", hits: 12 },
      { value: "worker", hits: 0 },
    ],
  });
});

test("incremental NDJSON parsing survives byte and UTF-8 code-point boundaries", () => {
  const encoder = new TextEncoder();
  const payload = encoder.encode(
    '{"_msg":"你好","n":1}\r\nmalformed\n{"_msg":"done","n":2}',
  );
  const chineseStart = payload.indexOf(0xe4);
  assert.notEqual(chineseStart, -1);

  const parser = createNdjsonParser<Record<string, unknown>>((value) => (
    value && typeof value === "object" && !Array.isArray(value)
      ? value as Record<string, unknown>
      : null
  ));
  assert.deepEqual(parser.push(payload.slice(0, chineseStart + 1)), []);
  assert.deepEqual(parser.push(payload.slice(chineseStart + 1, payload.length - 3)), [
    { _msg: "你好", n: 1 },
  ]);
  assert.deepEqual(parser.push(payload.slice(payload.length - 3)), []);
  assert.deepEqual(parser.finish(), [{ _msg: "done", n: 2 }]);
  assert.deepEqual(parser.finish(), []);
  assert.throws(() => parser.push("\n"), /finished NDJSON parser/);
});

test("builds grouped regular and stream extra-filter expressions safely", () => {
  const filters: LogsExtraFilter[] = [
    { field: "service", value: "api", operator: "=", isStream: false },
    { field: "service", value: 'pay"ments', operator: "=", isStream: false },
    { field: "level", value: "warn.*", operator: "!~", isStream: false },
    { field: "env", value: "prod", operator: "=", isStream: true },
    { field: "env", value: "staging", operator: "=", isStream: true },
    { field: "app", value: "api.*", operator: "=~", isStream: true },
  ];

  assert.deepEqual(buildLogsExtraFilterParams(filters), {
    extraFilters: [
      'service:in("api", "pay\\"ments")',
      'NOT (level:~"warn.*")',
    ],
    extraStreamFilters: [
      '{env in ("prod", "staging")}',
      '{app=~"api.*"}',
    ],
  });
});
