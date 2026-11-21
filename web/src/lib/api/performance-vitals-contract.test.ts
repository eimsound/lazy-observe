import assert from "node:assert/strict";
import { test } from "vitest";

import { normalizeVitalsBreakdownResponse } from "./performance-vitals-contract.ts";

test("normalizes valid Web Vitals breakdown response with all dimension groups", () => {
  const got = normalizeVitalsBreakdownResponse({
    start: 1_784_368_800_000,
    end: 1_784_372_400_000,
    step_seconds: 60,
    filters: { page: "/checkout", device_type: "mobile", country: "US" },
    summary: { lcp: 2350, inp: 165, cls: 0.09, ttfb: 610, fcp: 1420 },
    total_samples: 42,
    by_page: [{ value: "/checkout", samples: 42, values: { lcp: 2350, cls: 0.09 } }],
    by_device: [{ value: "mobile", samples: 42, values: { lcp: 2350 } }],
    by_browser: [{ value: "safari", samples: 30, values: { lcp: 2400 } }],
    by_os: [{ value: "ios", samples: 30, values: { lcp: 2400 } }],
    by_country: [{ value: "US", samples: 42, values: { lcp: 2350 } }],
    truncated: false,
  });

  assert.equal(got.total_samples, 42);
  assert.deepEqual(got.filters, { page: "/checkout", device_type: "mobile", country: "US" });
  assert.equal(got.by_device[0].value, "mobile");
  assert.equal(got.by_country[0].values.lcp, 2350);
  assert.equal(got.truncated, false);
});

test("accepts empty breakdowns and rejects malformed envelopes or non-finite values", () => {
  const empty = normalizeVitalsBreakdownResponse({
    start: 1000,
    end: 2000,
    step_seconds: 60,
    filters: {},
    summary: {},
    total_samples: 0,
    by_page: [],
    by_device: [],
    by_browser: [],
    by_os: [],
    by_country: [],
    truncated: false,
  });
  assert.equal(empty.total_samples, 0);
  assert.deepEqual(empty.by_device, []);

  assert.throws(
    () =>
      normalizeVitalsBreakdownResponse({
        start: 2000,
        end: 1000,
        step_seconds: 60,
        filters: {},
        summary: {},
        total_samples: 0,
        by_page: [],
        by_device: [],
        by_browser: [],
        by_os: [],
        by_country: [],
        truncated: false,
      }),
    /end must be after start/,
  );

  assert.throws(
    () =>
      normalizeVitalsBreakdownResponse({
        start: 1000,
        end: 2000,
        step_seconds: 60,
        filters: {},
        summary: { lcp: Number.NaN },
        total_samples: 1,
        by_page: [],
        by_device: [],
        by_browser: [],
        by_os: [],
        by_country: [],
        truncated: false,
      }),
    /non-negative finite number/,
  );

  assert.throws(
    () =>
      normalizeVitalsBreakdownResponse({
        start: 1000,
        end: 2000,
        step_seconds: 60,
        filters: {},
        summary: {},
        total_samples: 0,
        by_page: null,
        by_device: [],
        by_browser: [],
        by_os: [],
        by_country: [],
        truncated: false,
      }),
    /by_page must be an array/,
  );
});
