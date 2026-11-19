import assert from "node:assert/strict";
import { test } from "vitest";

import {
  analyzeLogsQLContext,
  buildLogsExtraFilterParams,
  escapeLogsQLString,
  logsAutocompleteBaseQuery,
  logsRangeFromSearch,
  logsRangeToSearch,
  parseLogsFilters,
  serializeLogsFilters,
  statsChartSeries,
  upsertLogsFilter,
  type LogsExtraFilter,
} from "../lib/logs-explorer.ts";

test("LogsQL autocomplete distinguishes fields, values, and pipes", () => {
  assert.deepEqual(analyzeLogsQLContext("serv", 4), {
    kind: "field",
    token: "serv",
    from: 0,
    to: 4,
  });
  assert.deepEqual(analyzeLogsQLContext('service:="ap', 12), {
    kind: "value",
    field: "service",
    token: "ap",
    from: 10,
    to: 12,
    quoted: true,
  });
  assert.deepEqual(analyzeLogsQLContext("service:api", 11), {
    kind: "value",
    field: "service",
    token: "api",
    from: 8,
    to: 11,
    quoted: false,
  });
  assert.deepEqual(analyzeLogsQLContext("* | sta", 7), {
    kind: "pipe",
    token: "sta",
    from: 4,
    to: 7,
  });

  const valueQuery = 'env:="prod" AND service:="ap';
  const valueContext = analyzeLogsQLContext(valueQuery, valueQuery.length);
  assert.equal(
    logsAutocompleteBaseQuery(valueQuery, valueContext),
    'env:="prod"',
  );
  const fieldQuery = 'env:="prod" AND lev';
  const fieldContext = analyzeLogsQLContext(fieldQuery, fieldQuery.length);
  assert.equal(
    logsAutocompleteBaseQuery(fieldQuery, fieldContext),
    'env:="prod"',
  );

  const groupedQuery = '(env:="prod" AND service:="ap';
  const groupedContext = analyzeLogsQLContext(groupedQuery, groupedQuery.length);
  assert.equal(
    logsAutocompleteBaseQuery(groupedQuery, groupedContext),
    '(env:="prod")',
  );

  const pipedQuery = '* | filter service:="ap';
  const pipedContext = analyzeLogsQLContext(pipedQuery, pipedQuery.length);
  assert.equal(logsAutocompleteBaseQuery(pipedQuery, pipedContext), "*");
});

test("extra filters escape quotes and group repeated fields", () => {
  const filters: LogsExtraFilter[] = [
    { field: "service", value: "api", operator: "=", isStream: false },
    { field: "service", value: 'pay"ments', operator: "=", isStream: false },
    { field: "message", value: "timeout.*", operator: "!~", isStream: false },
    { field: "trace_id", value: "*", operator: "=", isStream: false },
    { field: "_time", value: "15m", operator: "=", isStream: false },
    { field: "env", value: "prod", operator: "=", isStream: true },
    { field: "env", value: "staging", operator: "=", isStream: true },
  ];

  assert.equal(escapeLogsQLString('a\\"b'), 'a\\\\\\"b');
  assert.deepEqual(buildLogsExtraFilterParams(filters), {
    extraFilters: [
      'service:in("api", "pay\\"ments")',
      'NOT (message:~"timeout.*")',
      "trace_id:*",
      "_time:15m",
    ],
    extraStreamFilters: ['{env in ("prod", "staging")}'],
  });
});

test("URL filter serialization is bounded, validated, and round-trips", () => {
  const filters: LogsExtraFilter[] = [
    { field: "service", value: "api", operator: "=", isStream: false },
    { field: "env", value: "prod", operator: "!=", isStream: true },
  ];
  assert.deepEqual(parseLogsFilters(serializeLogsFilters(filters)), filters);
  assert.deepEqual(parseLogsFilters("not-json"), []);
  assert.deepEqual(parseLogsFilters(JSON.stringify([
    { f: "ok", v: "value", o: "=~", s: 1 },
    { f: 7, v: "bad", o: "=" },
    { f: "bad-op", v: "value", o: "<" },
  ])), [
    { field: "ok", value: "value", operator: "=~", isStream: true },
  ]);
});

test("relative and absolute log ranges round-trip through search params", () => {
  const relative = logsRangeFromSearch({ range: "2h30m" });
  assert.deepEqual(relative, {
    mode: "relative",
    presetId: "2h30m",
    seconds: 9_000,
  });
  assert.deepEqual(logsRangeToSearch(relative), { range: "2h30m" });

  const absolute = logsRangeFromSearch({
    start: "1784887200000",
    end: "1784887260000",
  });
  assert.deepEqual(absolute, {
    mode: "absolute",
    startMs: 1784887200000,
    endMs: 1784887260000,
  });
  assert.deepEqual(logsRangeToSearch(absolute), {
    start: 1784887200000,
    end: 1784887260000,
  });
  assert.deepEqual(logsRangeFromSearch({
    range: "not-a-duration",
    start: 2_000,
    end: 1_000,
  }), {
    mode: "relative",
    presetId: "1h",
    seconds: 3_600,
  });
});

test("upserting a field/value filter changes its operator without duplication", () => {
  const current: LogsExtraFilter[] = [
    { field: "level", value: "error", operator: "=", isStream: false },
  ];
  assert.deepEqual(upsertLogsFilter(current, {
    field: "level",
    value: "error",
    operator: "!=",
    isStream: false,
  }), [
    { field: "level", value: "error", operator: "!=", isStream: false },
  ]);
  assert.deepEqual(upsertLogsFilter(current, {
    field: "service",
    value: "api",
    operator: "=",
    isStream: false,
  }), [
    ...current,
    { field: "service", value: "api", operator: "=", isStream: false },
  ]);
});

test("stats chart keeps top series and aggregates the remainder as Other", () => {
  const series = statsChartSeries([
    {
      metric: { __name__: "requests", service: "api" },
      values: [{ timestamp: 10, value: "5" }, { timestamp: 20, value: "7" }],
    },
    {
      metric: { __name__: "requests", service: "worker" },
      values: [{ timestamp: 10, value: "3" }, { timestamp: 20, value: "4" }],
    },
    {
      metric: { __name__: "requests", service: "cron" },
      values: [{ timestamp: 10, value: "2" }, { timestamp: 20, value: "NaN" }],
    },
  ], 1);

  assert.deepEqual(series, [
    {
      fields: {},
      timestamps: [10_000, 20_000],
      values: [5, 4],
      total: 9,
      isOther: true,
    },
    {
      label: 'requests{service="api"}',
      fields: { service: "api" },
      timestamps: [10_000, 20_000],
      values: [5, 7],
      total: 12,
      isOther: false,
    },
  ]);
});
