import assert from "node:assert/strict";
import { test } from "vitest";

import {
  findRootSpan,
  isErrorSpan,
  summarizeTrace,
  type JaegerSpan,
  type JaegerTrace,
} from "./api/traces.ts";
import { bounds, buildTree, flatten, maxDepth, type TraceNode } from "./trace-tree.ts";
import { buildTraceEvidenceLinks } from "../routes/trace-evidence-links.ts";

function makeSpan(
  spanID: string,
  startTime: number,
  duration: number,
  parentID?: string,
  extra: Partial<JaegerSpan> = {},
): JaegerSpan {
  return {
    traceID: "0123456789abcdef0123456789abcdef",
    spanID,
    operationName: `op-${spanID}`,
    startTime,
    duration,
    references: parentID
      ? [{ refType: "CHILD_OF", traceID: "0123456789abcdef0123456789abcdef", spanID: parentID }]
      : [],
    tags: [],
    processID: "p1",
    ...extra,
  };
}

function makeTrace(spans: JaegerSpan[]): JaegerTrace {
  return {
    traceID: "0123456789abcdef0123456789abcdef",
    spans,
    processes: {
      p1: { serviceName: "api", tags: [] },
    },
  };
}

test("buildTree deduplicates repeated Zipkin/B3 spanIDs and ignores self-loops", () => {
  const trace = makeTrace([
    makeSpan("s1", 1_000, 500, "s1"),
    makeSpan("s1", 1_010, 480, "s1"),
    makeSpan("s2", 1_100, 100, "s1"),
  ]);

  const roots = buildTree(trace);
  assert.equal(roots.length, 1);
  assert.equal(roots[0].span.spanID, "s1");
  assert.equal(roots[0].children.length, 1);
  assert.equal(roots[0].children[0].span.spanID, "s2");
  assert.equal(maxDepth(roots), 2);
  assert.deepEqual(
    flatten(roots, trace, new Set()).map((r) => [r.span.spanID, r.depth]),
    [
      ["s1", 0],
      ["s2", 1],
    ],
  );
});

test("buildTree breaks pure cycles and promotes the earliest span to root so no spans disappear", () => {
  const trace = makeTrace([
    makeSpan("b", 2_000, 200, "a"),
    makeSpan("a", 1_000, 500, "b"),
    makeSpan("c", 2_100, 50, "b"),
  ]);

  const roots = buildTree(trace);
  assert.equal(roots.length, 1);
  assert.equal(roots[0].span.spanID, "a");
  assert.equal(maxDepth(roots), 3);
  assert.deepEqual(
    flatten(roots, trace, new Set()).map((r) => [r.span.spanID, r.depth]),
    [
      ["a", 0],
      ["b", 1],
      ["c", 2],
    ],
  );
});

test("maxDepth and flatten guard against manually constructed cycles", () => {
  const trace = makeTrace([makeSpan("x", 1_000, 100), makeSpan("y", 1_010, 50)]);
  const nodeX: TraceNode = { span: trace.spans[0], service: "api", children: [], error: false };
  const nodeY: TraceNode = { span: trace.spans[1], service: "api", children: [], error: false };
  nodeX.children.push(nodeY);
  nodeY.children.push(nodeX);

  assert.equal(maxDepth([nodeX]), 2);
  assert.deepEqual(
    flatten([nodeX], trace, new Set()).map((r) => r.span.spanID),
    ["x", "y"],
  );
});

test("deeply nested traces (12,000 spans) do not overflow the call stack", () => {
  const depth = 12_000;
  const spans: JaegerSpan[] = [];
  for (let i = 0; i < depth; i++) {
    spans.push(
      makeSpan(
        `s-${i}`,
        1_700_000_000_000_000 + i * 10,
        (depth - i) * 10,
        i === 0 ? undefined : `s-${i - 1}`,
      ),
    );
  }
  const trace = makeTrace(spans);

  const roots = buildTree(trace);
  assert.equal(roots.length, 1);
  assert.equal(maxDepth(roots), depth);
  const rows = flatten(roots, trace, new Set());
  assert.equal(rows.length, depth);
  assert.equal(rows[depth - 1].depth, depth - 1);

  const summary = summarizeTrace(trace);
  assert.equal(summary.spanCount, depth);
  assert.equal(summary.startTime, 1_700_000_000_000_000);
  assert.equal(summary.duration, depth * 10);

  const links = buildTraceEvidenceLinks(trace);
  assert.equal(links.profiling.search.service, "api");
});

test("null references, tags, and processes do not throw TypeError", () => {
  const malformedSpan = {
    traceID: "0123456789abcdef0123456789abcdef",
    spanID: "null-fields",
    operationName: "GET /health",
    startTime: 1_700_000_000_000_000,
    duration: 250,
    references: null,
    tags: null,
    processID: "p-missing",
  } as unknown as JaegerSpan;
  const trace = {
    traceID: "0123456789abcdef0123456789abcdef",
    spans: [malformedSpan],
    processes: null,
  } as unknown as JaegerTrace;

  assert.equal(isErrorSpan(malformedSpan), false);
  assert.equal(findRootSpan(trace)?.spanID, "null-fields");
  assert.equal(bounds(trace).total, 250);
  assert.equal(summarizeTrace(trace).spanCount, 1);
  assert.equal(buildTree(trace).length, 1);
  assert.equal(buildTraceEvidenceLinks(trace).profiling.search.service, "p-missing");
});
