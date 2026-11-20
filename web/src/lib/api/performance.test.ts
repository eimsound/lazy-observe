import assert from "node:assert/strict";
import { test } from "vitest";

import { aggregateTrend, lastValue } from "./performance.ts";
import type { TimeSeries } from "./types.ts";

test("lastValue averages latest finite sample across all page series instead of reading only series[0]", () => {
  const series: TimeSeries[] = [
    {
      name: 'web_vitals_lcp{page="/a"}',
      labels: { page: "/a" },
      points: [
        [1000, 1000],
        [2000, 1200],
      ],
    },
    {
      name: 'web_vitals_lcp{page="/b"}',
      labels: { page: "/b" },
      points: [
        [1000, 2000],
        [2000, 2800],
        [3000, null],
      ],
    },
  ];

  assert.equal(lastValue(series), 2000);
  assert.equal(lastValue([series[0]]), 1200);
  assert.equal(lastValue([]), null);
});

test("aggregateTrend averages values per timestamp across multiple page series", () => {
  const series: TimeSeries[] = [
    {
      name: "a",
      labels: { page: "/a" },
      points: [
        [1000, 100],
        [2000, 200],
      ],
    },
    {
      name: "b",
      labels: { page: "/b" },
      points: [
        [1000, 300],
        [2000, 400],
      ],
    },
  ];

  assert.deepEqual(aggregateTrend(series), [200, 300]);
  assert.deepEqual(aggregateTrend([]), []);
});
