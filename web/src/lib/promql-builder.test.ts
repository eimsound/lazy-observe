import assert from "node:assert/strict";
import { describe, test } from "vitest";

import {
  addBuilderFilter,
  applyBuilderHistogramQuantile,
  buildMetadataMatchSelector,
  buildPromQL,
  createBuilderController,
  createEmptyBuilderState,
  escapePromQLLabelValue,
  isValidPromQLQuantile,
  isValidPromQLRangeInterval,
  parsePromQLToBuilder,
  removeBuilderFilter,
  resetBuilderController,
  syncBuilderControllerFromQuery,
  toggleBuilderAggregationLabel,
  unescapePromQLLabelValue,
  updateBuilderController,
  updateBuilderFilter,
  type PromQLBuilderState,
} from "./promql-builder.ts";

function stripFilterIds(state: PromQLBuilderState) {
  return {
    ...state,
    filters: state.filters.map(({ label, operator, value }) => ({ label, operator, value })),
  };
}

describe("promql-builder escaping & validation", () => {
  test("escapes and unescapes quotes, backslashes, and whitespace controls round-trip", () => {
    const samples = [
      "plain-value",
      'quote "inside" value',
      "C:\\Program Files\\app\\bin",
      'combo \\"escaped\\" \\\\ slash',
      "line1\nline2\tindented\r",
      "",
    ];
    for (const sample of samples) {
      const escaped = escapePromQLLabelValue(sample);
      assert.equal(unescapePromQLLabelValue(escaped), sample);
    }
  });

  test("validates range intervals including $__rate_interval and compound durations", () => {
    assert.equal(isValidPromQLRangeInterval("5m"), true);
    assert.equal(isValidPromQLRangeInterval("1h30m"), true);
    assert.equal(isValidPromQLRangeInterval("500ms"), true);
    assert.equal(isValidPromQLRangeInterval("$__rate_interval"), true);
    assert.equal(isValidPromQLRangeInterval("$__interval"), true);
    assert.equal(isValidPromQLRangeInterval("1h:1m"), false);
    assert.equal(isValidPromQLRangeInterval(""), false);
    assert.equal(isValidPromQLRangeInterval("five"), false);
  });

  test("validates histogram quantile bounds [0, 1]", () => {
    assert.equal(isValidPromQLQuantile("0"), true);
    assert.equal(isValidPromQLQuantile("0.5"), true);
    assert.equal(isValidPromQLQuantile("0.95"), true);
    assert.equal(isValidPromQLQuantile("0.999"), true);
    assert.equal(isValidPromQLQuantile("1"), true);
    assert.equal(isValidPromQLQuantile(".95"), true);
    assert.equal(isValidPromQLQuantile("1.5"), false);
    assert.equal(isValidPromQLQuantile("-0.1"), false);
    assert.equal(isValidPromQLQuantile("abc"), false);
  });
});

describe("promql-builder generation (buildPromQL)", () => {
  test("returns empty string for empty state or blank-label filters only", () => {
    assert.equal(buildPromQL(createEmptyBuilderState()), "");
    const withBlank = addBuilderFilter(createEmptyBuilderState());
    assert.equal(buildPromQL(withBlank), "");
  });

  test("builds bare metric, OTel dotted metric, and label-only selector", () => {
    assert.equal(
      buildPromQL({ ...createEmptyBuilderState(), metric: "up" }),
      "up",
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http.server.request.duration_count",
      }),
      "http.server.request.duration_count",
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        filters: [{ id: "f1", label: "service.name", operator: "=", value: "checkout" }],
      }),
      '{service.name="checkout"}',
    );
  });

  test("builds multi-label selector with all four operators and proper escaping", () => {
    const query = buildPromQL({
      ...createEmptyBuilderState(),
      metric: "http_requests_total",
      filters: [
        { id: "1", label: "service.name", operator: "=", value: "api" },
        { id: "2", label: "status", operator: "!=", value: "200" },
        { id: "3", label: "path", operator: "=~", value: '/v1/"users"\\d+' },
        { id: "4", label: "env", operator: "!~", value: "dev|test" },
        { id: "5", label: "   ", operator: "=", value: "ignored" },
      ],
    });
    assert.equal(
      query,
      'http_requests_total{service.name="api", status!="200", path=~"/v1/\\"users\\"\\\\d+", env!~"dev|test"}',
    );
  });

  test("builds range functions with custom and $__rate_interval durations", () => {
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        rangeFunction: "rate",
        rangeInterval: "$__rate_interval",
      }),
      "rate(http_requests_total[$__rate_interval])",
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "node_memory_MemAvailable_bytes",
        rangeFunction: "avg_over_time",
        rangeInterval: "15m",
      }),
      "avg_over_time(node_memory_MemAvailable_bytes[15m])",
    );
  });

  test("builds nested rate + sum by / without and topk aggregations", () => {
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        filters: [{ id: "1", label: "method", operator: "=", value: "POST" }],
        rangeFunction: "rate",
        rangeInterval: "5m",
        aggregation: "sum",
        aggregationMode: "by",
        aggregationLabels: ["service.name", "status"],
      }),
      'sum by (service.name, status) (rate(http_requests_total{method="POST"}[5m]))',
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        rangeFunction: "increase",
        rangeInterval: "1h",
        aggregation: "avg",
        aggregationMode: "without",
        aggregationLabels: ["instance", "pod"],
      }),
      "avg without (instance, pod) (increase(http_requests_total[1h]))",
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        rangeFunction: "rate",
        rangeInterval: "5m",
        aggregation: "topk",
        aggregationParam: "10",
      }),
      "topk(10, rate(http_requests_total[5m]))",
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        aggregation: "topk",
        aggregationMode: "by",
        aggregationLabels: ["env"],
        aggregationParam: "3",
      }),
      "topk by (env) (3, http_requests_total)",
    );
  });

  test("builds histogram_quantile with sum by (le, ...) and rate", () => {
    const query = buildPromQL({
      ...createEmptyBuilderState(),
      metric: "http.server.request.duration_bucket",
      filters: [{ id: "1", label: "service.name", operator: "=", value: "gateway" }],
      rangeFunction: "rate",
      rangeInterval: "5m",
      aggregation: "sum",
      aggregationMode: "by",
      aggregationLabels: ["le", "http.route"],
      histogramQuantile: "0.95",
    });
    assert.equal(
      query,
      'histogram_quantile(0.95, sum by (le, http.route) (rate(http.server.request.duration_bucket{service.name="gateway"}[5m])))',
    );
  });

  test("builds metadata match[] selector excluding an actively edited filter", () => {
    const state: PromQLBuilderState = {
      ...createEmptyBuilderState(),
      metric: "http.server.request.duration_count",
      filters: [
        { id: "f1", label: "service.name", operator: "=", value: "api" },
        { id: "f2", label: "http.request.method", operator: "=", value: "GET" },
      ],
    };
    assert.equal(
      buildMetadataMatchSelector(state),
      '{__name__="http.server.request.duration_count",service.name="api",http.request.method="GET"}',
    );
    assert.equal(
      buildMetadataMatchSelector(state, { excludeFilterId: "f2" }),
      '{__name__="http.server.request.duration_count",service.name="api"}',
    );
    assert.equal(buildMetadataMatchSelector(createEmptyBuilderState()), undefined);
  });
});

describe("promql-builder reverse parsing (parsePromQLToBuilder)", () => {
  test("parses empty and whitespace queries into empty builder state", () => {
    const parsed = parsePromQLToBuilder("   \n\t ");
    assert.equal(parsed.ok, true);
    if (parsed.ok) {
      assert.deepEqual(stripFilterIds(parsed.state), stripFilterIds(createEmptyBuilderState()));
    }
  });

  test("round-trips metric, escaped multi-label matchers, rate, and sum by", () => {
    const original =
      'sum by (service.name, status) (rate(http.server.request.duration_count{service.name="api", path=~"/v1/\\"users\\"\\\\d+", env!="dev"}[$__rate_interval]))';
    const parsed = parsePromQLToBuilder(original);
    assert.equal(parsed.ok, true);
    if (parsed.ok) {
      assert.equal(parsed.state.metric, "http.server.request.duration_count");
      assert.equal(parsed.state.rangeFunction, "rate");
      assert.equal(parsed.state.rangeInterval, "$__rate_interval");
      assert.equal(parsed.state.aggregation, "sum");
      assert.equal(parsed.state.aggregationMode, "by");
      assert.deepEqual(parsed.state.aggregationLabels, ["service.name", "status"]);
      assert.deepEqual(
        parsed.state.filters.map(({ label, operator, value }) => ({ label, operator, value })),
        [
          { label: "service.name", operator: "=", value: "api" },
          { label: "path", operator: "=~", value: '/v1/"users"\\d+' },
          { label: "env", operator: "!=", value: "dev" },
        ],
      );
      assert.equal(buildPromQL(parsed.state), original);
    }
  });

  test("parses trailing grouping clauses, backtick strings, trailing commas, and __name__ matcher", () => {
    const parsed = parsePromQLToBuilder(
      'histogram_quantile(0.99, sum(rate({__name__="http_duration_bucket", path=~`a"b\\c`,}[5m])) by (le, service,))',
    );
    assert.equal(parsed.ok, true);
    if (parsed.ok) {
      assert.equal(parsed.state.histogramQuantile, "0.99");
      assert.equal(parsed.state.aggregation, "sum");
      assert.equal(parsed.state.aggregationMode, "by");
      assert.deepEqual(parsed.state.aggregationLabels, ["le", "service"]);
      assert.equal(parsed.state.rangeFunction, "rate");
      assert.equal(parsed.state.rangeInterval, "5m");
      assert.equal(parsed.state.metric, "http_duration_bucket");
      assert.deepEqual(
        parsed.state.filters.map(({ label, operator, value }) => ({ label, operator, value })),
        [{ label: "path", operator: "=~", value: 'a"b\\c' }],
      );
    }
  });

  test("parses topk with and without grouping clause", () => {
    const simple = parsePromQLToBuilder("topk(5, max_over_time(cpu_usage[10m]))");
    assert.equal(simple.ok, true);
    if (simple.ok) {
      assert.equal(simple.state.aggregation, "topk");
      assert.equal(simple.state.aggregationParam, "5");
      assert.equal(simple.state.rangeFunction, "max_over_time");
      assert.equal(simple.state.rangeInterval, "10m");
      assert.equal(buildPromQL(simple.state), "topk(5, max_over_time(cpu_usage[10m]))");
    }

    const grouped = parsePromQLToBuilder("topk(3, http_requests_total) by (env)");
    assert.equal(grouped.ok, true);
    if (grouped.ok) {
      assert.equal(grouped.state.aggregation, "topk");
      assert.equal(grouped.state.aggregationMode, "by");
      assert.deepEqual(grouped.state.aggregationLabels, ["env"]);
      assert.equal(grouped.state.aggregationParam, "3");
    }
  });

  test("safely rejects complex expressions without mutating input", () => {
    const complexQueries = [
      "rate(http_errors_total[5m]) / rate(http_requests_total[5m])",
      "a + fill(0) b",
      "up == 0",
      "max_over_time(rate(http_requests_total[5m])[1h:1m])",
      "http_requests_total offset 5m",
      "http_requests_total @ 1700000000",
      "absent(up{job=\"api\"})",
      'label_replace(up, "svc", "$1", "job", "(.*)")',
      "topk(5, sum by (service) (rate(http_requests_total[5m])))",
      "sum by (service) (histogram_quantile(0.95, rate(foo_bucket[5m])))",
      "sum without () (http_requests_total)",
      "histogram_quantile(1.5, rate(foo_bucket[5m]))",
      "up # inline comment must not be stripped",
      "rate(http_requests_total)",
    ];
    for (const expr of complexQueries) {
      const result = parsePromQLToBuilder(expr);
      assert.equal(result.ok, false, `expected complex rejection for: ${expr}`);
      if (!result.ok) {
        assert.equal(result.reason, "complex", `expected reason=complex for: ${expr}`);
      }
    }
  });

  test("distinguishes incomplete syntax errors from valid complex expressions", () => {
    const syntaxErrors = [
      'http_requests_total{job="unterminated}',
      "rate(http_requests_total[5m]",
      "sum by (job) ()",
      "sum by (job) (up) without (pod)",
      "histogram_quantile(0.95)",
    ];
    for (const expr of syntaxErrors) {
      const result = parsePromQLToBuilder(expr);
      assert.equal(result.ok, false, `expected syntax error for: ${expr}`);
      if (!result.ok) {
        assert.equal(result.reason, "syntax", `expected reason=syntax for: ${expr}`);
      }
    }
  });
});

describe("promql-builder controller & interaction workflows", () => {
  test("preserves in-progress blank filter rows without echo-loop clobbering", () => {
    let ctrl = createBuilderController("http_requests_total");
    assert.equal(ctrl.status, "synced");

    const added = updateBuilderController(ctrl, addBuilderFilter);
    ctrl = added.controller;
    assert.equal(ctrl.draft.filters.length, 1);
    assert.equal(added.query, "http_requests_total");

    // Parent re-passes unchanged query back down -> blank row stays intact.
    const resynced = syncBuilderControllerFromQuery(ctrl, added.query);
    assert.equal(resynced.draft.filters.length, 1);

    const filterId = resynced.draft.filters[0].id;
    const edited = updateBuilderController(resynced, (draft) =>
      updateBuilderFilter(draft, filterId, { label: "method", operator: "=", value: "GET" }),
    );
    assert.equal(edited.query, 'http_requests_total{method="GET"}');

    const removed = updateBuilderController(edited.controller, (draft) =>
      removeBuilderFilter(draft, filterId),
    );
    assert.equal(removed.query, "http_requests_total");
    assert.equal(removed.controller.draft.filters.length, 0);
  });

  test("bidirectional sync updates builder from CodeMirror edits and guards complex queries", () => {
    let ctrl = createBuilderController("up");
    ctrl = syncBuilderControllerFromQuery(ctrl, 'rate(http_requests_total{env="prod"}[5m])');
    assert.equal(ctrl.status, "synced");
    assert.equal(ctrl.draft.metric, "http_requests_total");
    assert.equal(ctrl.draft.rangeFunction, "rate");
    assert.equal(ctrl.draft.rangeInterval, "5m");

    const complexExpr = "rate(errors[5m]) / rate(requests[5m])";
    const guarded = syncBuilderControllerFromQuery(ctrl, complexExpr);
    assert.equal(guarded.status, "complex");
    assert.equal(guarded.lastSyncedQuery, complexExpr);
    // Previous draft is untouched and no mutated PromQL is emitted unless user explicitly resets.
    assert.equal(guarded.draft.metric, "http_requests_total");

    const reset = resetBuilderController();
    assert.equal(reset.query, "");
    assert.equal(reset.controller.status, "synced");
    assert.equal(reset.controller.draft.metric, "");
  });

  test("histogram_quantile helper sets up rate + sum by (le) on raw metrics and preserves custom labels", () => {
    const initial = {
      ...createEmptyBuilderState(),
      metric: "http_request_duration_seconds_bucket",
    };
    const enabled = applyBuilderHistogramQuantile(initial, "0.95");
    assert.equal(
      buildPromQL(enabled),
      "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))",
    );

    const withService = toggleBuilderAggregationLabel(enabled, "service.name");
    assert.equal(
      buildPromQL(withService),
      "histogram_quantile(0.95, sum by (le, service.name) (rate(http_request_duration_seconds_bucket[5m])))",
    );

    const toggledOff = toggleBuilderAggregationLabel(withService, "service.name");
    assert.deepEqual(toggledOff.aggregationLabels, ["le"]);

    const disabled = applyBuilderHistogramQuantile(withService, "");
    assert.equal(
      buildPromQL(disabled),
      "sum by (le, service.name) (rate(http_request_duration_seconds_bucket[5m]))",
    );
  });
});
