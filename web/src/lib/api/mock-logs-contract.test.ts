import assert from "node:assert/strict";
import { test } from "vitest";

import {
  mockExportLogs,
  mockLogFieldNames,
  mockLogFieldValues,
  mockLogHitsSeries,
  mockLogStatsRange,
  mockLogsHits,
  mockLogsQueryTimeRange,
  mockQueryLogs,
  mockStreamFieldNames,
  mockStreamFieldValues,
  mockTailLogs,
} from "./mock-logs.ts";

const RANGE = {
  start: "2026-07-24T00:00:00Z",
  end: "2026-07-24T01:00:00Z",
};

test("mock rows honor regular, negative, regex, and stream filters", async () => {
  const regularParams = {
    ...RANGE,
    query: "*",
    limit: 80,
    extraFilters: [
      'app:in("api-gateway")',
      'NOT (level:~"warn|error")',
    ],
  };
  const [regular, streamList, streamCombination] = await Promise.all([
    mockQueryLogs(regularParams),
    mockQueryLogs({
      ...RANGE,
      query: "*",
      limit: 80,
      extraStreamFilters: ['{env in ("staging")}'],
    }),
    mockQueryLogs({
      ...RANGE,
      query: "*",
      limit: 80,
      extraStreamFilters: [
        '{app="api-gateway",env="prod"} OR {app="worker",env="staging"}',
        '{app not_in ("payments-service")}',
      ],
    }),
  ]);

  assert.ok(regular.length > 0);
  assert.ok(regular.every((row) => row.raw.app === "api-gateway"));
  assert.ok(regular.every((row) => row.level !== "warn" && row.level !== "error"));

  assert.ok(streamList.length > 0);
  assert.ok(streamList.every((row) => row.raw.env === "staging"));

  assert.ok(streamCombination.length > 0);
  assert.ok(streamCombination.every((row) => (
    row.raw.app === "api-gateway" || row.raw.app === "worker"
  )));
  assert.deepEqual(await mockQueryLogs(regularParams), regular);
});

test("mock hits and stats derive series from the filtered population", async () => {
  const params = {
    ...RANGE,
    query: "*",
    step: "5m",
  } as const;
  const [unfiltered, filtered, stats] = await Promise.all([
    mockLogHitsSeries(params),
    mockLogHitsSeries({
      ...params,
      field: "app",
      extraStreamFilters: ['{app in ("api-gateway")}'],
    }),
    mockLogStatsRange({
      ...params,
      query: "* | stats count() by (app)",
      extraStreamFilters: ['{env in ("staging")}'],
    }),
  ]);

  assert.equal(unfiltered.hits.length, 1);
  assert.ok(filtered.hits.length > 0);
  assert.ok(filtered.hits.every((series) => series.fields.app === "api-gateway"));
  assert.ok(filtered.hits.reduce((sum, series) => sum + series.total, 0)
    < unfiltered.hits.reduce((sum, series) => sum + series.total, 0));
  assert.ok(stats.data.result.length > 0);
  assert.ok(stats.data.result.every((series) => series.metric.app === "worker"));
});

test("mock metadata hit counts and values reflect active filters", async () => {
  const [allFields, filteredFields, logValues, streamValues] = await Promise.all([
    mockLogFieldNames({ ...RANGE, query: "*" }),
    mockLogFieldNames({
      ...RANGE,
      query: "*",
      extraStreamFilters: ['{env in ("staging")}'],
    }),
    mockLogFieldValues({
      ...RANGE,
      query: "*",
      field: "app",
      extraStreamFilters: ['{env in ("staging")}'],
    }),
    mockStreamFieldValues({
      ...RANGE,
      query: "*",
      field: "app",
      extraStreamFilters: ['{env in ("staging")}'],
    }),
  ]);

  const allAppHits = allFields.values.find((field) => field.value === "app")?.hits ?? 0;
  const filteredAppHits =
    filteredFields.values.find((field) => field.value === "app")?.hits ?? 0;
  assert.ok(filteredAppHits > 0 && filteredAppHits < allAppHits);
  assert.deepEqual(logValues.values.map((value) => value.value), ["worker"]);
  assert.deepEqual(streamValues.values.map((value) => value.value), ["worker"]);
});

test("mock tail applies stream filters and remains abortable", async () => {
  const controller = new AbortController();
  const tail = mockTailLogs({
    query: "*",
    extraStreamFilters: ['{app in ("api-gateway")}'],
  }, controller.signal);

  const first = await tail.next();
  assert.equal(first.done, false);
  assert.equal(first.value?.raw.app, "api-gateway");

  controller.abort();
  assert.equal((await tail.next()).done, true);
});

test("all finite mock log requests reject with AbortError when cancelled", async () => {
  const controller = new AbortController();
  const signal = controller.signal;
  const requests = [
    mockQueryLogs({ ...RANGE, query: "*", limit: 20 }, signal),
    mockLogsHits({
      query: "*",
      start: Date.parse(RANGE.start) / 1000,
      end: Date.parse(RANGE.end) / 1000,
      step: 60,
    }, signal),
    mockLogHitsSeries({ ...RANGE, query: "*", step: "1m" }, signal),
    mockLogStatsRange({
      ...RANGE,
      query: "* | stats count() by (app)",
      step: "1m",
    }, signal),
    mockLogsQueryTimeRange({ ...RANGE, query: "*" }, signal),
    mockLogFieldNames({ ...RANGE, query: "*" }, signal),
    mockLogFieldValues({ ...RANGE, query: "*", field: "app" }, signal),
    mockStreamFieldNames({ ...RANGE, query: "*" }, signal),
    mockStreamFieldValues({ ...RANGE, query: "*", field: "app" }, signal),
    mockExportLogs({ ...RANGE, query: "*", format: "jsonl" }, signal),
  ];

  controller.abort();

  await Promise.all(requests.map((request) => assert.rejects(
    request,
    (error: unknown) => error instanceof DOMException && error.name === "AbortError",
  )));
});

test("a pre-aborted mock request preserves the AbortSignal reason", async () => {
  const controller = new AbortController();
  const reason = new Error("superseded logs query");
  controller.abort(reason);

  await assert.rejects(
    mockQueryLogs({ ...RANGE, query: "__error", limit: 20 }, controller.signal),
    (error: unknown) => error === reason,
  );
});
