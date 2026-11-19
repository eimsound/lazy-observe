import assert from "node:assert/strict";
import { test } from "vitest";
import { closeBrackets, insertBracket } from "@codemirror/autocomplete";
import { EditorSelection, EditorState } from "@codemirror/state";
import dayjs, { dayjsLocale } from "../lib/dayjs.ts";

import {
  analyzePromQLContext,
  calendarCells,
  dateParts,
  effectiveMetricsStep,
  estimateClockDriftSeconds,
  formatMetricsAxisValue,
  formatPrometheusDuration,
  insertPromQLSuggestion,
  METRICSQL_FUNCTIONS,
  metricsAutocompleteRange,
  metricsLabelColumns,
  parseDateTime,
  parsePrometheusDuration,
  resolveMetricsRange,
  shiftMetricsRange,
  stepMetricsRange,
  toggleMetricsSeriesSelection,
} from "../lib/metrics-query.ts";

test("server clock drift uses the request midpoint instead of cache age", () => {
  assert.equal(estimateClockDriftSeconds(100.1, 100_000, 100_200), 0);
  assert.equal(estimateClockDriftSeconds(130.1, 100_000, 100_200), 30);
});

test("Day.js time output is localized with only the supported application locales", () => {
  const value = dayjs.utc("2026-07-22T07:08:19Z");
  assert.equal(dayjsLocale("zh-CN"), "zh-cn");
  assert.equal(dayjsLocale("en-US"), "en");
  assert.equal(value.locale(dayjsLocale("zh-CN")).format("L LTS"), "2026/07/22 07:08:19");
  assert.equal(value.locale(dayjsLocale("en-US")).format("L LTS"), "07/22/2026 7:08:19 AM");
});

test("metrics axis labels stay compact across large and tiny values", () => {
  assert.equal(formatMetricsAxisValue(1_234), "1.234k");
  assert.equal(formatMetricsAxisValue(-9_876_543_210), "-9.877G");
  assert.equal(formatMetricsAxisValue(1e30), "1.00e+30");
  assert.equal(formatMetricsAxisValue(0.000_012_3), "1.23e-5");
  assert.equal(formatMetricsAxisValue(12.345_67), "12.35");
});

test("metrics autocomplete uses stable local-day cache buckets like vmui", () => {
  const input = {
    start: dayjs("2026-07-22 07:08:19", "YYYY-MM-DD HH:mm:ss", true).valueOf() / 1_000,
    end: dayjs("2026-07-23 09:10:11", "YYYY-MM-DD HH:mm:ss", true).valueOf() / 1_000,
  };
  const range = metricsAutocompleteRange(input);
  const start = dayjs(range.start * 1_000);
  const end = dayjs(range.end * 1_000);
  assert.deepEqual(
    [start.year(), start.month(), start.date(), start.hour(), start.minute()],
    [2026, 6, 22, 0, 0],
  );
  assert.deepEqual(
    [end.year(), end.month(), end.date(), end.hour(), end.minute(), end.second()],
    [2026, 6, 23, 23, 59, 59],
  );
  assert.ok(range.start <= input.start);
  assert.ok(range.end >= input.end);
});

test("PromQL table label columns use the union of sorted label names", () => {
  assert.deepEqual(
    metricsLabelColumns([
      { labels: { __name__: "requests_total", service: "api", method: "GET" } },
      { labels: { __name__: "requests_total", region: "cn" } },
    ]),
    ["method", "region", "service"],
  );
});

test("chart legend supports isolate and modifier-based multi-series selection", () => {
  assert.deepEqual(toggleMetricsSeriesSelection([], 1, 4, false), [1]);
  assert.deepEqual(toggleMetricsSeriesSelection([1], 1, 4, false), []);
  assert.deepEqual(toggleMetricsSeriesSelection([], 1, 4, true), [0, 2, 3]);
  assert.deepEqual(toggleMetricsSeriesSelection([0, 2, 3], 2, 4, true), [0, 3]);
  assert.deepEqual(toggleMetricsSeriesSelection([0, 3], 1, 4, true), [0, 1, 3]);
});

test("Prometheus-style ranges and resolutions round-trip", () => {
  assert.equal(formatPrometheusDuration(7_261), "2h1m1s");
  assert.equal(parsePrometheusDuration("2h1m1s"), 7_261);
  assert.equal(parsePrometheusDuration("nonsense"), null);
  assert.equal(effectiveMetricsStep({ type: "auto", density: "medium" }, 3600), 14);

  const oneHour = { mode: "relative" as const, presetId: "1h", seconds: 3600 };
  assert.deepEqual(stepMetricsRange(oneHour, 1), {
    mode: "relative",
    presetId: "2h",
    seconds: 7200,
  });
  assert.deepEqual(shiftMetricsRange(oneHour, -1, 10_000_000), {
    mode: "absolute",
    startMs: 4_600_000,
    endMs: 8_200_000,
  });
});

test("absolute metrics ranges allow an omitted end and resolve it to now", () => {
  const now = dayjs.utc("2026-07-22 08:30:00", "YYYY-MM-DD HH:mm:ss", true).valueOf();
  const start = now - 60 * 60 * 1000;
  assert.deepEqual(resolveMetricsRange({ mode: "absolute", startMs: start }, now), {
    start: start / 1000,
    end: now / 1000,
    step: 12,
  });
  assert.throws(
    () => resolveMetricsRange({ mode: "absolute", startMs: now, endMs: now }, now),
    /invalid metrics time range/,
  );
});

test("UTC date/time parsing round-trips and rejects normalized invalid dates", () => {
  const timestamp = parseDateTime("2026-07-22", "07:08:19", false);
  assert.equal(
    timestamp,
    dayjs.utc("2026-07-22 07:08:19", "YYYY-MM-DD HH:mm:ss", true).valueOf(),
  );
  assert.deepEqual(dateParts(timestamp!, false), { date: "2026-07-22", time: "07:08:19" });
  assert.equal(parseDateTime("2026-02-30", "07:08:19", false), null);
  assert.equal(parseDateTime("2026-07-22", "24:00:00", false), null);
});

test("calendar model always returns a six-week grid", () => {
  const cells = calendarCells(2026, 6, false);
  assert.equal(cells.length, 42);
  assert.equal(cells[0].day, 29);
  assert.equal(cells[2].day, 1);
  assert.equal(cells.filter((cell) => cell.monthOffset === 0).length, 31);
  assert.equal(cells.find((cell) => cell.day === 22 && cell.monthOffset === 0)?.key, "2026-6-22");
});

test("PromQL autocomplete distinguishes metrics, labels, and label values", () => {
  assert.deepEqual(analyzePromQLContext("rat", 3).kind, "metric");
  const label = analyzePromQLContext('http_requests_total{met', 23);
  assert.equal(label.kind, "label");
  assert.equal(label.metric, "http_requests_total");
  assert.equal(label.token, "met");
  const value = analyzePromQLContext('http_requests_total{method="GE', 30);
  assert.equal(value.kind, "value");
  assert.equal(value.label, "method");
  assert.equal(value.token, "GE");
});

test("label-value insertion adds balanced quotes and function insertion adds a call", () => {
  const raw = 'http_requests_total{method=GE}';
  const cursor = raw.indexOf("GE") + 2;
  const context = analyzePromQLContext(raw, cursor);
  const inserted = insertPromQLSuggestion(raw, cursor, context, { kind: "value", value: "GET" });
  assert.equal(inserted.query, 'http_requests_total{method="GET"}');

  const fnContext = analyzePromQLContext("rat", 3);
  assert.equal(
    insertPromQLSuggestion("rat", 3, fnContext, { kind: "function", value: "rate" }).query,
    "rate(",
  );
  assert.ok(METRICSQL_FUNCTIONS.includes("histogram_quantile"));
  assert.ok(METRICSQL_FUNCTIONS.length > 200);
});

test("CodeMirror surrounds selected PromQL when an opening bracket is typed", () => {
  const state = EditorState.create({
    doc: "rate(foo[5m])",
    selection: EditorSelection.range(5, 12),
    extensions: [closeBrackets()],
  });
  const transaction = insertBracket(state, "(");
  assert.ok(transaction);
  assert.equal(transaction.state.doc.toString(), "rate((foo[5m]))");
  assert.deepEqual(
    [transaction.state.selection.main.from, transaction.state.selection.main.to],
    [6, 13],
  );
});
