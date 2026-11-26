import assert from "node:assert/strict";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { I18nextProvider } from "react-i18next";
import i18next from "i18next";
import { describe, test } from "vitest";

import { zh } from "../../lib/i18n/locales/zh.ts";
import { en } from "../../lib/i18n/locales/en.ts";
import {
  addBuilderFilter,
  applyBuilderHistogramQuantile,
  createBuilderController,
  syncBuilderControllerFromQuery,
  toggleBuilderAggregationLabel,
  updateBuilderController,
  updateBuilderFilter,
  type PromQLBuilderState,
  type PromQLBuilderStatus,
} from "../../lib/promql-builder.ts";
import { PromQLBuilderView, type PromQLBuilderMetadataState } from "./promql-builder.tsx";

function createTestI18n(lng: "zh" | "en" = "zh") {
  const instance = i18next.createInstance();
  void instance.init({
    lng,
    fallbackLng: "zh",
    resources: {
      zh: { translation: zh },
      en: { translation: en },
    },
    interpolation: { escapeValue: false },
  });
  return instance;
}

function renderBuilderView(options: {
  draft: PromQLBuilderState;
  status: PromQLBuilderStatus;
  rawQuery?: string;
  metricsMetadata?: PromQLBuilderMetadataState;
  labelsMetadata?: PromQLBuilderMetadataState;
  lng?: "zh" | "en";
}) {
  const i18n = createTestI18n(options.lng ?? "zh");
  const noop = () => undefined;
  return renderToStaticMarkup(
    createElement(
      I18nextProvider,
      { i18n },
      createElement(PromQLBuilderView, {
        draft: options.draft,
        status: options.status,
        rawQuery: options.rawQuery ?? "",
        metricsMetadata: options.metricsMetadata ?? {
          options: ["http_requests_total", "http.server.request.duration_bucket"],
          isLoading: false,
          isError: false,
        },
        labelsMetadata: options.labelsMetadata ?? {
          options: ["service.name", "http.request.method", "le"],
          isLoading: false,
          isError: false,
        },
        renderFilterValueInput: (filter) =>
          createElement("input", {
            "data-test-filter-value": filter.id,
            defaultValue: filter.value,
          }),
        onUpdateDraft: noop,
        onSwitchToCode: noop,
        onReset: noop,
      }),
    ),
  );
}

describe("PromQLBuilderView component rendering & interaction states", () => {
  test("renders empty synced builder with metric input, add-filter CTA, and no filter rows", () => {
    const ctrl = createBuilderController("");
    const html = renderBuilderView({ draft: ctrl.draft, status: ctrl.status });

    assert.match(html, /data-promql-builder-status="synced"/);
    assert.match(html, /aria-label="指标"/);
    assert.match(html, /aria-label="展开指标候选"/);
    assert.match(html, /搜索或手动输入指标名/);
    assert.match(html, /添加筛选/);
    assert.doesNotMatch(html, /data-promql-filter-row/);
    assert.doesNotMatch(html, /未添加标签筛选条件/);
  });

  test("renders populated filters, range interval, aggregation badges, and quantile after builder interactions", () => {
    const initial = createBuilderController("");
    const step1 = updateBuilderController(initial, (d) => ({
      ...d,
      metric: "http.server.request.duration_bucket",
    }));
    assert.equal(step1.query, "http.server.request.duration_bucket");

    const step2 = updateBuilderController(step1.controller, addBuilderFilter);
    const filterId = step2.controller.draft.filters[0].id;
    const step3 = updateBuilderController(step2.controller, (d) =>
      updateBuilderFilter(d, filterId, {
        label: "service.name",
        operator: "=~",
        value: 'api-"v1"',
      }),
    );
    const step4 = updateBuilderController(step3.controller, (d) =>
      applyBuilderHistogramQuantile(d, "0.95"),
    );
    const step5 = updateBuilderController(step4.controller, (d) =>
      toggleBuilderAggregationLabel(d, "http.route"),
    );
    const ctrl = step5.controller;
    const emitted = step5.query;

    assert.equal(
      emitted,
      'histogram_quantile(0.95, sum by (le, http.route) (rate(http.server.request.duration_bucket{service.name=~"api-\\"v1\\""}[5m])))',
    );

    const html = renderBuilderView({
      draft: ctrl.draft,
      status: ctrl.status,
      rawQuery: emitted,
    });
    assert.match(html, /http\.server\.request\.duration_bucket/);
    assert.match(html, new RegExp(`data-promql-filter-row="${filterId}"`));
    assert.match(html, /service\.name/);
    assert.match(html, /http\.route/);
    assert.match(html, />le</);
  });

  test("renders non-destructive complex expression alert preserving original PromQL", () => {
    const complexQuery = "rate(http_errors_total[5m]) / rate(http_requests_total[5m])";
    const ctrl = syncBuilderControllerFromQuery(createBuilderController("up"), complexQuery);
    assert.equal(ctrl.status, "complex");

    const htmlZh = renderBuilderView({
      draft: ctrl.draft,
      status: ctrl.status,
      rawQuery: complexQuery,
      lng: "zh",
    });
    assert.match(htmlZh, /data-promql-builder-status="complex"/);
    assert.match(htmlZh, /复杂表达式请使用代码模式编辑/);
    assert.match(htmlZh, /切换到代码模式/);
    assert.match(htmlZh, /rate\(http_errors_total\[5m\]\) \/ rate\(http_requests_total\[5m\]\)/);

    const htmlEn = renderBuilderView({
      draft: ctrl.draft,
      status: ctrl.status,
      rawQuery: complexQuery,
      lng: "en",
    });
    assert.match(htmlEn, /Use Code mode for complex expressions/);
    assert.match(htmlEn, /Switch to Code mode/);
  });

  test("renders syntax error notice without wiping incomplete CodeMirror expression", () => {
    const broken = 'http_requests_total{job="api';
    const ctrl = createBuilderController(broken);
    assert.equal(ctrl.status, "syntax");

    const html = renderBuilderView({
      draft: ctrl.draft,
      status: ctrl.status,
      rawQuery: broken,
    });
    assert.match(html, /data-promql-builder-status="syntax"/);
    assert.match(html, /当前 PromQL 语法尚未完整/);
    assert.match(html, /切换到代码模式/);
  });

  test("degrades gracefully with manual input fallback when metadata request errors", () => {
    const ctrl = createBuilderController("custom_metric");
    const html = renderBuilderView({
      draft: ctrl.draft,
      status: ctrl.status,
      metricsMetadata: { options: [], isLoading: false, isError: true },
      labelsMetadata: { options: [], isLoading: false, isError: true },
    });
    assert.match(html, /元数据暂不可用，可直接手动输入/);
    assert.match(html, /value="custom_metric"/);
  });
});
