import assert from "node:assert/strict";
import { test } from "vitest";

import type { TimeSeries } from "../lib/api/types.ts";
import {
  lastSeriesValue,
  OVERVIEW_DASH_QUERIES,
  OVERVIEW_UNIT_SCALES,
  scaleTimeSeries,
} from "./overview-metrics.ts";

test("overview P95 latency converts seconds to milliseconds (1000x)", () => {
  assert.match(OVERVIEW_DASH_QUERIES.p95, /http_request_duration_seconds_bucket/);
  assert.equal(OVERVIEW_UNIT_SCALES.p95SecondsToMs, 1_000);

  const raw: TimeSeries[] = [
    {
      name: "p95",
      labels: {},
      points: [
        [1_000, 0.042],
        [2_000, null],
        [3_000, 0.1255],
      ],
    },
  ];
  const scaled = scaleTimeSeries(raw, OVERVIEW_UNIT_SCALES.p95SecondsToMs);
  assert.deepEqual(scaled?.[0].points, [
    [1_000, 42],
    [2_000, null],
    [3_000, 125.5],
  ]);
  assert.equal(lastSeriesValue(scaled), 125.5);
});

test("overview error rate converts [0, 1] ratio to percentage (100x)", () => {
  assert.equal(OVERVIEW_UNIT_SCALES.errorRatioToPercent, 100);

  const raw: TimeSeries[] = [
    {
      name: "error_rate",
      labels: {},
      points: [
        [1_000, 0.015],
        [2_000, 0.0235],
        [3_000, null],
      ],
    },
  ];
  const scaled = scaleTimeSeries(raw, OVERVIEW_UNIT_SCALES.errorRatioToPercent);
  assert.equal(lastSeriesValue(scaled)?.toFixed(2), "2.35");
  assert.deepEqual(scaled?.[0].points, [
    [1_000, 1.5],
    [2_000, 2.35],
    [3_000, null],
  ]);
});
