import assert from "node:assert/strict";
import { test } from "vitest";

import {
  CUSTOM_TIME_RANGE_ID,
  advanceWindowNowMs,
  resolveTimeWindow,
} from "./use-time-window.ts";

test("resolveTimeWindow resolves preset ranges in unix seconds with a positive step", () => {
  const nowMs = Date.UTC(2026, 10, 21, 12, 0, 0);
  const window1h = resolveTimeWindow("1h", nowMs);
  assert.equal(window1h.end, Math.floor(nowMs / 1000));
  assert.equal(window1h.start, window1h.end - 3600);
  assert.equal(window1h.step, 30);

  const window24h = resolveTimeWindow("24h", nowMs);
  assert.equal(window24h.end - window24h.start, 24 * 3600);
  assert.ok(window24h.step > 0);
});

test("resolveTimeWindow honors an explicit custom window when CUSTOM_TIME_RANGE_ID is active", () => {
  const custom = { start: 1_700_000_000, end: 1_700_001_200 };
  const resolved = resolveTimeWindow(CUSTOM_TIME_RANGE_ID, Date.UTC(2026, 0, 1), custom);
  assert.equal(resolved.start, custom.start);
  assert.equal(resolved.end, custom.end);
  assert.equal(resolved.step, 10);
});

test("advanceWindowNowMs strictly advances the window end even within the same second", () => {
  const baseSec = 1_795_165_200;
  const sameSecondMs = baseSec * 1000 + 250;
  const advanced = advanceWindowNowMs(baseSec, sameSecondMs);
  assert.equal(advanced, (baseSec + 1) * 1000);

  const nextWindow = resolveTimeWindow("1h", advanced);
  assert.equal(nextWindow.end, baseSec + 1);

  // When wall clock has already moved further ahead, use wall clock.
  const laterMs = (baseSec + 15) * 1000;
  assert.equal(advanceWindowNowMs(baseSec, laterMs), laterMs);
});
