import { isErrorSpan, type JaegerSpan, type JaegerTrace } from "@/lib/api/traces";

/**
 * Turns a flat Jaeger span list into the tree + timeline model the waterfall
 * renders. This is the core of the self-built waterfall:
 *
 *  - buildTree() reconstructs parent/child from CHILD_OF references and orders
 *    siblings by start time (multiple roots are supported and listed in order);
 *  - bounds() gives the trace's absolute [start, end] so every span can be
 *    expressed as offset/width fractions of the total duration;
 *  - flatten() produces the ordered, depth-tagged rows actually drawn, honoring
 *    a collapsed-set so subtrees can be folded;
 *  - serviceColorIndex() maps each service to a --chart-N slot for coloring.
 */

export interface TraceNode {
  span: JaegerSpan;
  service: string;
  children: TraceNode[];
  /** True if this span itself is flagged as an error. */
  error: boolean;
}

/** A flattened, positioned row ready to render. */
export interface WaterfallRow {
  span: JaegerSpan;
  service: string;
  depth: number;
  error: boolean;
  hasChildren: boolean;
  collapsed: boolean;
  /** Fraction [0,1] of the trace duration where this span starts. */
  offset: number;
  /** Fraction [0,1] of the trace duration this span spans (min-clamped). */
  width: number;
  /** Fraction of the total trace duration (for the tooltip). */
  durationFraction: number;
}

function serviceOf(trace: JaegerTrace, span: JaegerSpan): string {
  return trace.processes[span.processID]?.serviceName ?? span.processID;
}

export function buildTree(trace: JaegerTrace): TraceNode[] {
  const nodeById = new Map<string, TraceNode>();
  for (const span of trace.spans) {
    nodeById.set(span.spanID, {
      span,
      service: serviceOf(trace, span),
      children: [],
      error: isErrorSpan(span),
    });
  }

  const roots: TraceNode[] = [];
  for (const span of trace.spans) {
    const node = nodeById.get(span.spanID)!;
    const parentRef = span.references.find((r) => r.refType === "CHILD_OF");
    const parent = parentRef ? nodeById.get(parentRef.spanID) : undefined;
    if (parent) parent.children.push(node);
    else roots.push(node);
  }

  const byStart = (a: TraceNode, b: TraceNode) => a.span.startTime - b.span.startTime;
  roots.sort(byStart);
  for (const node of nodeById.values()) node.children.sort(byStart);
  return roots;
}

export function bounds(trace: JaegerTrace): { start: number; end: number; total: number } {
  let start = Infinity;
  let end = -Infinity;
  for (const s of trace.spans) {
    if (s.startTime < start) start = s.startTime;
    if (s.startTime + s.duration > end) end = s.startTime + s.duration;
  }
  if (!Number.isFinite(start)) start = 0;
  if (!Number.isFinite(end)) end = start;
  return { start, end, total: Math.max(1, end - start) };
}

export function maxDepth(roots: TraceNode[]): number {
  let deepest = 0;
  const walk = (node: TraceNode, depth: number) => {
    deepest = Math.max(deepest, depth);
    node.children.forEach((c) => walk(c, depth + 1));
  };
  roots.forEach((r) => walk(r, 1));
  return deepest;
}

/** Pre-order DFS to flat rows, skipping children under any collapsed span. */
export function flatten(
  roots: TraceNode[],
  trace: JaegerTrace,
  collapsed: ReadonlySet<string>,
): WaterfallRow[] {
  const { start, total } = bounds(trace);
  const rows: WaterfallRow[] = [];

  const MIN_WIDTH = 0.004; // keep sub-microsecond spans visible
  const walk = (node: TraceNode, depth: number) => {
    const isCollapsed = collapsed.has(node.span.spanID);
    const durationFraction = node.span.duration / total;
    rows.push({
      span: node.span,
      service: node.service,
      depth,
      error: node.error,
      hasChildren: node.children.length > 0,
      collapsed: isCollapsed,
      offset: (node.span.startTime - start) / total,
      width: Math.max(MIN_WIDTH, durationFraction),
      durationFraction,
    });
    if (!isCollapsed) node.children.forEach((c) => walk(c, depth + 1));
  };
  roots.forEach((r) => walk(r, 0));
  return rows;
}

/** Ordered unique services -> palette slot (1..5, cycling). */
export function serviceColorIndex(trace: JaegerTrace): Map<string, number> {
  const map = new Map<string, number>();
  let i = 0;
  for (const span of trace.spans) {
    const svc = serviceOf(trace, span);
    if (!map.has(svc)) map.set(svc, (i++ % 5) + 1);
  }
  return map;
}

/** CSS var reference for a service's palette color (a token, not a hex). */
export function chartVar(index: number): string {
  return `var(--chart-${index})`;
}
