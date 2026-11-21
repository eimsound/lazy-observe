import assert from "node:assert/strict";
import { test } from "vitest";

import {
  buildMetricsPanelsURL,
  newMetricsPanel,
  parseMetricsSearch,
  parseMetricsStateFromLocation,
} from "./metrics-search.ts";

test("parseMetricsSearch preserves both evidence deep-link fields and indexed gN.* panel params", () => {
  const parsed = parseMetricsSearch({
    q: "up",
    run: "1",
    start: "1700000000",
    end: 1700003600,
    evidence: "contextual",
    "g0.expr": "rate(http_requests_total[5m])",
    "g0.tab": "graph",
    "g0.y_axis_min": 0,
    "g1.expr": "up",
    unknown_key: "drop-me",
  });

  assert.deepEqual(parsed, {
    q: "up",
    run: "1",
    start: 1700000000,
    end: 1700003600,
    evidence: "contextual",
    "g0.expr": "rate(http_requests_total[5m])",
    "g0.tab": "graph",
    "g0.y_axis_min": "0",
    "g1.expr": "up",
  });
});

test("multi-panel metrics state round-trips through TanStack Router URLs without window.location.search", () => {
  const nowMs = Date.UTC(2026, 10, 21, 10, 0, 0);
  const p0 = newMetricsPanel("query-1", 'rate(http_requests_total{job="api"}[5m])');
  p0.editorMode = "builder";
  p0.activeTab = "graph";
  p0.range = { mode: "relative", presetId: "6h", seconds: 6 * 3600 };
  p0.resolution = { type: "fixed", step: 30 };
  p0.graphMode = "stacked";
  p0.tableMode = "series";
  p0.timeFormat = "timestamp";
  p0.yAxisZero = true;

  const p1 = newMetricsPanel("query-2", "up");
  p1.range = {
    mode: "absolute",
    startMs: nowMs - 30 * 60 * 1000,
    endMs: nowMs,
  };
  p1.resolution = { type: "auto", density: "high" };

  const href = buildMetricsPanelsURL("/metrics", [p0, p1]);
  const hydrated = parseMetricsStateFromLocation(href, undefined, nowMs);

  assert.equal(hydrated.panels.length, 2);
  assert.equal(hydrated.panels[0].query, p0.query);
  assert.equal(hydrated.panels[0].editorMode, "builder");
  assert.equal(hydrated.panels[0].activeTab, "graph");
  assert.deepEqual(hydrated.panels[0].range, { mode: "relative", presetId: "6h", seconds: 21600 });
  assert.deepEqual(hydrated.panels[0].resolution, { type: "fixed", step: 30 });
  assert.equal(hydrated.panels[0].graphMode, "stacked");
  assert.equal(hydrated.panels[0].tableMode, "series");
  assert.equal(hydrated.panels[0].timeFormat, "timestamp");
  assert.equal(hydrated.panels[0].yAxisZero, true);
  assert.equal(hydrated.executed["query-1"]?.query, p0.query);

  assert.equal(hydrated.panels[1].query, "up");
  assert.deepEqual(hydrated.panels[1].range, p1.range);
  assert.deepEqual(hydrated.panels[1].resolution, { type: "auto", density: "high" });
  assert.equal(hydrated.executed["query-2"]?.query, "up");
});

test("buildMetricsPanelsURL preserves evidence=contextual while normalizing deep-link q/start/end into g0.*", () => {
  const initial = parseMetricsStateFromLocation(
    "/metrics?q=up&run=1&start=1700000000&end=1700003600&evidence=contextual",
    {
      q: "up",
      run: "1",
      start: 1700000000,
      end: 1700003600,
      evidence: "contextual",
    },
  );
  assert.equal(initial.panels.length, 1);
  assert.equal(initial.panels[0].query, "up");
  assert.deepEqual(initial.panels[0].range, {
    mode: "absolute",
    startMs: 1_700_000_000_000,
    endMs: 1_700_003_600_000,
  });

  const canonical = buildMetricsPanelsURL(
    "/metrics?q=up&run=1&start=1700000000&end=1700003600&evidence=contextual",
    initial.panels,
  );
  const params = new URL(canonical, "https://lazy-observe.invalid").searchParams;
  assert.equal(params.get("evidence"), "contextual");
  assert.equal(params.get("g0.expr"), "up");
  assert.equal(params.get("q"), null);
  assert.equal(params.get("run"), null);
});
