import assert from "node:assert/strict";
import { test } from "vitest";

import { aggregateTrend, buildVitalPromQL, buildVitalTrendPromQL, lastValue } from "./performance.ts";
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

test("buildVitalPromQL attaches low-cardinality dimension matchers and regex fallbacks for unknown/other", () => {
  assert.equal(buildVitalPromQL("web_vitals_lcp"), "web_vitals_lcp");
  assert.equal(
    buildVitalPromQL("web_vitals_lcp", { page: "/checkout", device_type: "mobile", country: "US" }),
    'web_vitals_lcp{page="/checkout",device_type="mobile",country="US"}',
  );
  assert.equal(
    buildVitalPromQL("web_vitals_cls", {
      device_type: "unknown",
      browser_family: "other",
      os_family: "other",
      country: "unknown",
    }),
    'web_vitals_cls{device_type=~"|unknown",browser_family=~"|other",os_family=~"|other",country=~"|unknown"}',
  );
});

test("buildVitalTrendPromQL aggregates multi-dimensional series into a bounded sample-weighted query", () => {
  assert.equal(
    buildVitalTrendPromQL("web_vitals_lcp", { start: 1000, end: 4600, step: 30 }),
    "sum(sum_over_time(web_vitals_lcp[60s])) / sum(count_over_time(web_vitals_lcp[60s]))",
  );
  assert.equal(
    buildVitalTrendPromQL(
      "web_vitals_inp",
      { start: 1000, end: 87400, step: 300 },
      { page: "/checkout", device_type: "mobile", country: "US" },
    ),
    'sum(sum_over_time(web_vitals_inp{page="/checkout",device_type="mobile",country="US"}[300s])) / sum(count_over_time(web_vitals_inp{page="/checkout",device_type="mobile",country="US"}[300s]))',
  );
  assert.equal(
    buildVitalTrendPromQL("web_vitals_ttfb", { start: 1000, end: 1020, step: 5 }),
    "sum(sum_over_time(web_vitals_ttfb[20s])) / sum(count_over_time(web_vitals_ttfb[20s]))",
  );
});
