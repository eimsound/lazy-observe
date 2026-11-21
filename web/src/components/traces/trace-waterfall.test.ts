import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { test } from "vitest";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { JaegerSpan, JaegerTrace } from "@/lib/api/traces";
import { TraceWaterfall } from "./trace-waterfall.tsx";

function makeTrace(spanCount: number, traceID = "0123456789abcdef0123456789abcdef"): JaegerTrace {
  const startBase = 1_700_000_000_000_000;
  const spans: JaegerSpan[] = [];
  for (let i = 0; i < spanCount; i++) {
    const parentID = i === 0 ? undefined : `span-${Math.floor((i - 1) / 4)}`;
    spans.push({
      traceID,
      spanID: `span-${i}`,
      operationName: `HTTP GET /api/v1/resource/${i}`,
      startTime: startBase + i * 50,
      duration: Math.max(100, (spanCount - i) * 20),
      references: parentID
        ? [{ refType: "CHILD_OF", traceID, spanID: parentID }]
        : [],
      tags: i === 2 ? [{ key: "error", type: "bool", value: true }] : [],
      processID: `p${(i % 3) + 1}`,
    });
  }
  return {
    traceID,
    spans,
    processes: {
      p1: { serviceName: "api-gateway", tags: [] },
      p2: { serviceName: "orders-service", tags: [] },
      p3: { serviceName: "postgres", tags: [] },
    },
  };
}

function renderWaterfall(trace: JaegerTrace, selectedSpanId?: string): string {
  return renderToStaticMarkup(
    React.createElement(
      TooltipProvider,
      null,
      React.createElement(TraceWaterfall, {
        trace,
        selectedSpanId,
        onSelectSpan: () => {},
      }),
    ),
  );
}

test("TraceWaterfall renders all rows without virtualization for small traces (<= 80 spans)", () => {
  const html = renderWaterfall(makeTrace(20), "span-2");
  const renderedRows = (html.match(/role="button"/g) ?? []).length;

  assert.equal(renderedRows, 20);
  assert.match(html, /data-virtualized="false"/);
  assert.match(html, /HTTP GET \/api\/v1\/resource\/0/);
  assert.match(html, /HTTP GET \/api\/v1\/resource\/19/);
  assert.match(html, /aria-expanded="true"/);
});

test("TraceWaterfall virtualizes 2,000 spans to a bounded DOM window with accurate bottom spacer", () => {
  const html = renderWaterfall(makeTrace(2_000));
  const renderedRows = (html.match(/role="button"/g) ?? []).length;

  assert.match(html, /data-virtualized="true"/);
  // 640px default viewport / 30px row height = 22 visible + 12 overscan = 34 rows.
  assert.equal(renderedRows, 34);
  assert.ok(renderedRows <= 60, `expected bounded DOM rows, got ${renderedRows}`);

  // Bottom spacer must preserve remaining scroll height: (2000 - 34) * 30 = 58980px.
  assert.match(html, /aria-hidden="true" style="height:58980px"/);
  assert.match(html, /HTTP GET \/api\/v1\/resource\/0/);
  assert.doesNotMatch(html, /HTTP GET \/api\/v1\/resource\/1999/);
});

test("TraceWaterfall isolates state across distinct traceIDs sharing identical spanIDs", () => {
  const htmlA = renderWaterfall(makeTrace(12, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "span-0");
  const htmlB = renderWaterfall(makeTrace(15, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), "span-0");

  assert.equal((htmlA.match(/role="button"/g) ?? []).length, 12);
  assert.equal((htmlB.match(/role="button"/g) ?? []).length, 15);
  assert.match(htmlB, /HTTP GET \/api\/v1\/resource\/14/);
});
