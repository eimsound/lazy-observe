import assert from "node:assert/strict";
import { test } from "vitest";

import {
  createTimeSeriesTimeConfig,
  formatTimeSeriesTimestamp,
} from "../components/charts/time-series-time.ts";

test("time-series chart preserves the explicitly selected time window", () => {
  const config = createTimeSeriesTimeConfig({
    startMs: 1_787_552_192_000,
    endMs: 1_787_555_792_000,
  });

  assert.equal(config.ms, 1);
  assert.deepEqual(config.xRange, [1_787_552_192_000, 1_787_555_792_000]);
});

test("time-series chart formats millisecond timestamps as the real calendar time", () => {
  assert.equal(
    formatTimeSeriesTimestamp(1_787_552_942_000, true, "zh-CN"),
    "2026年8月24日 06:29:02",
  );
});
