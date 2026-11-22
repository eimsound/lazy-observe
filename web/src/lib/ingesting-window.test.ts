import assert from "node:assert/strict";
import { test } from "vitest";
import type { JaegerSpan, JaegerTrace } from "./api/traces-contract.ts";
import {
  RECENT_INGEST_WINDOW_MS,
  isTimeWindowRecentlyIngesting,
  isTraceRecentlyIngesting,
  recentIngestRemainingMs,
  timeWindowEndMs,
  traceLatestEndMs,
} from "./ingesting-window.ts";

function makeSpan(
  spanID: string,
  startTimeUs: number,
  durationUs: number,
): JaegerSpan {
  return {
    traceID: "trace-1",
    spanID,
    operationName: `op-${spanID}`,
    startTime: startTimeUs,
    duration: durationUs,
    references: [],
    tags: [],
    processID: "p-1",
  };
}

function makeTrace(spans: JaegerSpan[]): JaegerTrace {
  return {
    traceID: "trace-1",
    spans,
    processes: {},
  };
}

const NOW_MS = Date.UTC(2026, 10, 22, 10, 0, 0);

test("RECENT_INGEST_WINDOW_MS equals 30 seconds", () => {
  assert.equal(RECENT_INGEST_WINDOW_MS, 30_000);
});

test("isTraceRecentlyIngesting handles 5s, 29.9s, exact 30.0s boundary, and 31s", () => {
  const trace5s = makeTrace([makeSpan("s1", (NOW_MS - 5_500) * 1_000, 500_000)]);
  assert.equal(isTraceRecentlyIngesting(trace5s, NOW_MS), true);

  const trace299s = makeTrace([makeSpan("s1", (NOW_MS - 30_000) * 1_000, 100_000)]);
  assert.equal(isTraceRecentlyIngesting(trace299s, NOW_MS), true);

  const trace30s = makeTrace([makeSpan("s1", (NOW_MS - 31_000) * 1_000, 1_000_000)]);
  assert.equal(isTraceRecentlyIngesting(trace30s, NOW_MS), false);

  const trace31s = makeTrace([makeSpan("s1", (NOW_MS - 32_000) * 1_000, 1_000_000)]);
  assert.equal(isTraceRecentlyIngesting(trace31s, NOW_MS), false);
});

test("isTraceRecentlyIngesting inspects latest child span and span duration", () => {
  const rootOldChildRecent = makeTrace([
    makeSpan("root", (NOW_MS - 300_000) * 1_000, 1_000_000),
    makeSpan("child", (NOW_MS - 12_000) * 1_000, 2_000_000),
  ]);
  assert.equal(traceLatestEndMs(rootOldChildRecent), NOW_MS - 10_000);
  assert.equal(isTraceRecentlyIngesting(rootOldChildRecent, NOW_MS), true);

  // Span started 40s ago but ran for 20s -> ended 20s ago -> still inside 30s window.
  const longSpan = makeTrace([makeSpan("long", (NOW_MS - 40_000) * 1_000, 20_000_000)]);
  assert.equal(traceLatestEndMs(longSpan), NOW_MS - 20_000);
  assert.equal(isTraceRecentlyIngesting(longSpan, NOW_MS), true);
});

test("traceLatestEndMs and isTraceRecentlyIngesting guard against invalid, negative, empty, and future spans", () => {
  assert.equal(traceLatestEndMs(null), null);
  assert.equal(traceLatestEndMs(undefined), null);
  assert.equal(traceLatestEndMs(makeTrace([])), null);
  assert.equal(isTraceRecentlyIngesting(makeTrace([]), NOW_MS), false);

  const allInvalid = makeTrace([
    makeSpan("nan", Number.NaN, 1_000),
    makeSpan("zero", 0, 1_000),
    makeSpan("neg", -1_000_000, 500),
    makeSpan("inf", Number.POSITIVE_INFINITY, 500),
  ]);
  assert.equal(traceLatestEndMs(allInvalid), null);
  assert.equal(isTraceRecentlyIngesting(allInvalid, NOW_MS), false);

  // Negative or NaN duration is clamped to 0; invalid siblings are ignored.
  const mixed = makeTrace([
    makeSpan("bad", Number.NaN, 1_000),
    makeSpan("neg-dur", (NOW_MS - 5_000) * 1_000, -500_000),
    makeSpan("nan-dur", (NOW_MS - 4_000) * 1_000, Number.NaN),
  ]);
  assert.equal(traceLatestEndMs(mixed), NOW_MS - 4_000);
  assert.equal(isTraceRecentlyIngesting(mixed, NOW_MS), true);

  // Slight future clock skew remains true.
  const futureTrace = makeTrace([makeSpan("future", (NOW_MS + 1_500) * 1_000, 500_000)]);
  assert.equal(isTraceRecentlyIngesting(futureTrace, NOW_MS), true);
});

test("isTimeWindowRecentlyIngesting converts seconds to milliseconds accurately", () => {
  const nowSec = NOW_MS / 1_000;
  assert.equal(timeWindowEndMs(nowSec), NOW_MS);
  assert.equal(isTimeWindowRecentlyIngesting(nowSec - 5, NOW_MS), true);
  assert.equal(isTimeWindowRecentlyIngesting(nowSec - 29.9, NOW_MS), true);
  assert.equal(isTimeWindowRecentlyIngesting(nowSec - 30, NOW_MS), false);
  assert.equal(isTimeWindowRecentlyIngesting(nowSec - 31, NOW_MS), false);
  assert.equal(isTimeWindowRecentlyIngesting(nowSec + 2, NOW_MS), true);

  assert.equal(timeWindowEndMs(0), null);
  assert.equal(timeWindowEndMs(-100), null);
  assert.equal(timeWindowEndMs(Number.NaN), null);
  assert.equal(timeWindowEndMs(Number.POSITIVE_INFINITY), null);
  assert.equal(isTimeWindowRecentlyIngesting(0, NOW_MS), false);
  assert.equal(isTimeWindowRecentlyIngesting(-10, NOW_MS), false);
  assert.equal(isTimeWindowRecentlyIngesting(Number.NaN, NOW_MS), false);
});

test("recentIngestRemainingMs computes exact boundaries", () => {
  assert.equal(recentIngestRemainingMs(NOW_MS - 5_000, NOW_MS), 25_000);
  assert.equal(recentIngestRemainingMs(NOW_MS - 29_900, NOW_MS), 100);
  assert.equal(recentIngestRemainingMs(NOW_MS - 30_000, NOW_MS), 0);
  assert.equal(recentIngestRemainingMs(NOW_MS - 31_000, NOW_MS), 0);
  assert.equal(recentIngestRemainingMs(NOW_MS, NOW_MS), 30_000);
  assert.equal(recentIngestRemainingMs(NOW_MS + 2_000, NOW_MS), 32_000);

  assert.equal(recentIngestRemainingMs(0, NOW_MS), 0);
  assert.equal(recentIngestRemainingMs(-1, NOW_MS), 0);
  assert.equal(recentIngestRemainingMs(Number.NaN, NOW_MS), 0);
  assert.equal(recentIngestRemainingMs(NOW_MS, Number.NaN), 0);
});
