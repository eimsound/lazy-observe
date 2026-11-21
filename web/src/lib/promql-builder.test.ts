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
  formatPromQLLabel,
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
  test("escapes and unescapes quotes, backslashes, and all control chars 0x00..0x1f round-trip", () => {
    const allControlChars = Array.from({ length: 0x20 }, (_, code) => String.fromCharCode(code)).join("");
    const samples = [
      "plain-value",
      'quote "inside" value',
      "C:\\Program Files\\app\\bin",
      'combo \\"escaped\\" \\\\ slash',
      "line1\nline2\tindented\r",
      allControlChars,
      "",
    ];
    for (const sample of samples) {
      const escaped = escapePromQLLabelValue(sample);
      const decoded = unescapePromQLLabelValue(escaped, '"');
      assert.deepEqual(decoded, { ok: true, value: sample });
    }
    assert.equal(escapePromQLLabelValue("\x00\x07\x1f"), "\\x00\\x07\\x1f");
  });

  test("strictly unescapes PromQL escapes including octal, \\xHH, \\uHHHH, \\UHHHHHHHH and rejects invalid escapes", () => {
    assert.deepEqual(
      unescapePromQLLabelValue("\\a\\b\\f\\n\\r\\t\\v\\\\\\\"", '"'),
      { ok: true, value: "\x07\b\f\n\r\t\v\\\"" },
    );
    assert.deepEqual(
      unescapePromQLLabelValue("\\'\\\\", "'"),
      { ok: true, value: "'\\" },
    );
    assert.deepEqual(
      unescapePromQLLabelValue("\\101\\x42\\u4e2d\\U0001F600", '"'),
      { ok: true, value: "AB中😀" },
    );
    assert.deepEqual(unescapePromQLLabelValue("\\000\\377", '"'), {
      ok: true,
      value: "\x00\xff",
    });

    // Invalid escapes must return { ok: false } instead of silently mutating.
    const invalidDouble = [
      "\\d+",
      "\\'",
      "\\x1",
      "\\xGG",
      "\\u123",
      "\\uD800",
      "\\U00110000",
      "\\U0000DFFF",
      "\\400",
      "\\7",
      "trailing\\",
    ];
    for (const bad of invalidDouble) {
      assert.deepEqual(unescapePromQLLabelValue(bad, '"'), { ok: false }, `expected invalid: ${bad}`);
    }
    assert.deepEqual(unescapePromQLLabelValue('\\"', "'"), { ok: false });
  });

  test("formats bare identifiers unquoted and UTF-8 / punctuation labels quoted", () => {
    assert.equal(formatPromQLLabel("service.name"), "service.name");
    assert.equal(formatPromQLLabel("http_method"), "http_method");
    assert.equal(formatPromQLLabel("k8s-app"), '"k8s-app"');
    assert.equal(formatPromQLLabel("app/name"), '"app/name"');
    assert.equal(formatPromQLLabel('bad"label'), '"bad\\"label"');
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

  test("builds multi-label selector with all four operators, UTF-8 quoted labels, and proper escaping", () => {
    const query = buildPromQL({
      ...createEmptyBuilderState(),
      metric: "http_requests_total",
      filters: [
        { id: "1", label: "service.name", operator: "=", value: "api" },
        { id: "2", label: "k8s-app", operator: "!=", value: "canary" },
        { id: "3", label: "path", operator: "=~", value: '/v1/"users"\\d+' },
        { id: "4", label: "env", operator: "!~", value: "dev|test" },
        { id: "5", label: "   ", operator: "=", value: "ignored" },
      ],
    });
    assert.equal(
      query,
      'http_requests_total{service.name="api", "k8s-app"!="canary", path=~"/v1/\\"users\\"\\\\d+", env!~"dev|test"}',
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

  test("builds nested rate + sum by / without (with UTF-8 label quoting) and topk aggregations", () => {
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        filters: [{ id: "1", label: "method", operator: "=", value: "POST" }],
        rangeFunction: "rate",
        rangeInterval: "5m",
        aggregation: "sum",
        aggregationMode: "by",
        aggregationLabels: ["service.name", "k8s-app"],
      }),
      'sum by (service.name, "k8s-app") (rate(http_requests_total{method="POST"}[5m]))',
    );
    assert.equal(
      buildPromQL({
        ...createEmptyBuilderState(),
        metric: "http_requests_total",
        rangeFunction: "increase",
        rangeInterval: "1h",
        aggregation: "avg",
        aggregationMode: "without",
        aggregationLabels: ["instance", "app/pod"],
      }),
      'avg without (instance, "app/pod") (increase(http_requests_total[1h]))',
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

  test("builds metadata match[] selector with UTF-8 quoting and skips invalid unclosed regex filters", () => {
    const state: PromQLBuilderState = {
      ...createEmptyBuilderState(),
      metric: "http.server.request.duration_count",
      filters: [
        { id: "f1", label: "k8s-app", operator: "=", value: "api" },
        { id: "f2", label: "path", operator: "=~", value: "(unclosed" },
        { id: "f3", label: "env", operator: "!~", value: "[a-z" },
        { id: "f4", label: "route", operator: "=~", value: "(?i)/api/v[12]" },
        { id: "f5", label: "http.request.method", operator: "=", value: "GET" },
      ],
    };
    assert.equal(
      buildMetadataMatchSelector(state),
      '{__name__="http.server.request.duration_count","k8s-app"="api",route=~"(?i)/api/v[12]",http.request.method="GET"}',
    );
    assert.equal(
      buildMetadataMatchSelector(state, { excludeFilterId: "f5" }),
      '{__name__="http.server.request.duration_count","k8s-app"="api",route=~"(?i)/api/v[12]"}',
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

  test("round-trips metric, escaped multi-label matchers, UTF-8 labels, rate, and sum by", () => {
    const original =
      'sum by (service.name, "k8s-app") (rate(http.server.request.duration_count{service.name="api", "k8s-app"="web", path=~"/v1/\\"users\\"\\\\d+", env!="dev"}[$__rate_interval]))';
    const parsed = parsePromQLToBuilder(original);
    assert.equal(parsed.ok, true);
    if (parsed.ok) {
      assert.equal(parsed.state.metric, "http.server.request.duration_count");
      assert.equal(parsed.state.rangeFunction, "rate");
      assert.equal(parsed.state.rangeInterval, "$__rate_interval");
      assert.equal(parsed.state.aggregation, "sum");
      assert.equal(parsed.state.aggregationMode, "by");
      assert.deepEqual(parsed.state.aggregationLabels, ["service.name", "k8s-app"]);
      assert.deepEqual(
        parsed.state.filters.map(({ label, operator, value }) => ({ label, operator, value })),
        [
          { label: "service.name", operator: "=", value: "api" },
          { label: "k8s-app", operator: "=", value: "web" },
          { label: "path", operator: "=~", value: '/v1/"users"\\d+' },
          { label: "env", operator: "!=", value: "dev" },
        ],
      );
      assert.equal(buildPromQL(parsed.state), original);
    }
  });

  test("parses UTF-8 quoted metric shorthand and quoted label matchers", () => {
    const withFilters = parsePromQLToBuilder(
      'sum by ("k8s-app") ({"http.server.duration", "k8s-app"="web", env="prod"})',
    );
    assert.equal(withFilters.ok, true);
    if (withFilters.ok) {
      assert.equal(withFilters.state.metric, "http.server.duration");
      assert.deepEqual(withFilters.state.aggregationLabels, ["k8s-app"]);
      assert.deepEqual(
        withFilters.state.filters.map(({ label, operator, value }) => ({ label, operator, value })),
        [
          { label: "k8s-app", operator: "=", value: "web" },
          { label: "env", operator: "=", value: "prod" },
        ],
      );
    }

    const soloQuotedMetric = parsePromQLToBuilder('{"my-utf8-metric"}');
    assert.equal(soloQuotedMetric.ok, true);
    if (soloQuotedMetric.ok) {
      assert.equal(soloQuotedMetric.state.metric, "my-utf8-metric");
      assert.deepEqual(soloQuotedMetric.state.filters, []);
      assert.equal(buildPromQL(soloQuotedMetric.state), '{__name__="my-utf8-metric"}');
    }

    const hexUnicode = parsePromQLToBuilder('up{label="\\x41\\u4e2d\\U0001F600"}');
    assert.equal(hexUnicode.ok, true);
    if (hexUnicode.ok) {
      assert.equal(hexUnicode.state.filters[0]?.value, "A中😀");
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
      'absent(up{job="api"})',
      'label_replace(up, "svc", "$1", "job", "(.*)")',
      "topk(5, sum by (service) (rate(http_requests_total[5m])))",
      "sum by (service) (histogram_quantile(0.95, rate(foo_bucket[5m])))",
      "sum without () (http_requests_total)",
      "histogram_quantile(1.5, rate(foo_bucket[5m]))",
      "up # inline comment must not be stripped",
      "rate(http_requests_total)",
      'foo{"bar"}',
    ];
    for (const expr of complexQueries) {
      const result = parsePromQLToBuilder(expr);
      assert.equal(result.ok, false, `expected complex rejection for: ${expr}`);
      if (!result.ok) {
        assert.equal(result.reason, "complex", `expected reason=complex for: ${expr}`);
      }
    }
  });

  test("distinguishes incomplete syntax errors and invalid string escapes from valid complex expressions", () => {
    const syntaxErrors = [
      'http_requests_total{job="unterminated}',
      'http_requests_total{path="\\d+"}',
      'http_requests_total{path="\\\'"}',
      "http_requests_total{path='\\\"'}",
      'http_requests_total{path="\\x1"}',
      'http_requests_total{path="\\uD800"}',
      '{""}',
      '{""="v"}',
      'sum by ("") (up)',
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

  test("histogram_quantile helper fills rate + sum by (le) across all left-to-right selection orders", () => {
    // 1. Raw metric -> enables rate + sum by (le)
    const raw = {
      ...createEmptyBuilderState(),
      metric: "http_request_duration_seconds_bucket",
    };
    const fromRaw = applyBuilderHistogramQuantile(raw, "0.95");
    assert.equal(
      buildPromQL(fromRaw),
      "histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))",
    );

    // 2. User picked rate[15m] first, then quantile -> preserves rate[15m] and adds sum by (le)
    const afterRate = applyBuilderHistogramQuantile(
      { ...raw, rangeFunction: "rate", rangeInterval: "15m" },
      "0.99",
    );
    assert.equal(
      buildPromQL(afterRate),
      "histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[15m])))",
    );

    // 3. User picked rate + ungrouped sum first, then quantile -> adds by (le)
    const afterSum = applyBuilderHistogramQuantile(
      { ...raw, rangeFunction: "increase", rangeInterval: "1h", aggregation: "sum", aggregationMode: "none" },
      "0.9",
    );
    assert.equal(
      buildPromQL(afterSum),
      "histogram_quantile(0.9, sum by (le) (increase(http_request_duration_seconds_bucket[1h])))",
    );

    // 4. User picked sum by (service.name) first -> prepends le
    const afterSumBy = applyBuilderHistogramQuantile(
      {
        ...raw,
        rangeFunction: "rate",
        rangeInterval: "5m",
        aggregation: "sum",
        aggregationMode: "by",
        aggregationLabels: ["service.name"],
      },
      "0.5",
    );
    assert.equal(
      buildPromQL(afterSumBy),
      "histogram_quantile(0.5, sum by (le, service.name) (rate(http_request_duration_seconds_bucket[5m])))",
    );

    const withService = toggleBuilderAggregationLabel(fromRaw, "service.name");
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
