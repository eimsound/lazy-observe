import { isErrorSpan, type JaegerSpan, type JaegerTrace } from "@/lib/api/traces";

/**
 * Turns a flat Jaeger span list into the tree + timeline model the waterfall
 * renders. This is the core of the self-built waterfall:
 *
 *  - buildTree() reconstructs an acyclic forest from CHILD_OF references,
 *    deduplicating repeated spanIDs (Zipkin/B3), breaking self-loops and pure
 *    cycles by promoting the earliest cycle member to root, and ordering
 *    siblings by start time;
 *  - bounds() gives the trace's absolute [start, end] so every span can be
 *    expressed as offset/width fractions of the total duration;
 *  - maxDepth() and flatten() use explicit stacks with cycle guards so deep
 *    chains or malformed graphs never overflow the JS call stack;
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
  return trace.processes?.[span.processID]?.serviceName ?? span.processID;
}

export function buildTree(trace: JaegerTrace): TraceNode[] {
  const spans = trace.spans ?? [];
  const nodeById = new Map<string, TraceNode>();

  // Deduplicate shared spanIDs (e.g. Zipkin/B3 client+server spans) so each
  // spanID maps to one node and can never form duplicate edges or self-cycles.
  for (const span of spans) {
    if (nodeById.has(span.spanID)) continue;
    nodeById.set(span.spanID, {
      span,
      service: serviceOf(trace, span),
      children: [],
      error: isErrorSpan(span),
    });
  }

  const byStart = (a: TraceNode, b: TraceNode) => a.span.startTime - b.span.startTime;
  const allNodes = [...nodeById.values()].sort(byStart);

  // Build candidate parent pointers while ignoring self-references and missing parents.
  const parentOf = new Map<TraceNode, TraceNode>();
  for (const node of allNodes) {
    const parentRef = (node.span.references ?? []).find(
      (r) => r.refType === "CHILD_OF" && r.spanID !== node.span.spanID,
    );
    const parent = parentRef ? nodeById.get(parentRef.spanID) : undefined;
    if (parent && parent !== node) {
      parentOf.set(node, parent);
    }
  }

  // Iteratively detect and break cycles so unreachable cycle members are
  // promoted to roots instead of disappearing or causing infinite loops.
  const verified = new Set<TraceNode>();
  for (const startNode of allNodes) {
    if (verified.has(startNode)) continue;
    const trail: TraceNode[] = [];
    const trailIndex = new Map<TraceNode, number>();
    let cursor: TraceNode | undefined = startNode;

    while (cursor && !verified.has(cursor)) {
      const existingIndex = trailIndex.get(cursor);
      if (existingIndex !== undefined) {
        // Pick the earliest-starting span in the cycle to become a root.
        let earliest = trail[existingIndex];
        for (let i = existingIndex + 1; i < trail.length; i++) {
          if (trail[i].span.startTime < earliest.span.startTime) {
            earliest = trail[i];
          }
        }
        parentOf.delete(earliest);
        break;
      }
      trailIndex.set(cursor, trail.length);
      trail.push(cursor);
      cursor = parentOf.get(cursor);
    }

    for (const node of trail) verified.add(node);
  }

  const roots: TraceNode[] = [];
  for (const node of allNodes) {
    const parent = parentOf.get(node);
    if (parent) parent.children.push(node);
    else roots.push(node);
  }

  roots.sort(byStart);
  for (const node of allNodes) node.children.sort(byStart);
  return roots;
}

export function bounds(trace: JaegerTrace): { start: number; end: number; total: number } {
  let start = Infinity;
  let end = -Infinity;
  for (const s of trace.spans ?? []) {
    if (!Number.isFinite(s.startTime)) continue;
    if (s.startTime < start) start = s.startTime;
    const spanEnd = s.startTime + (Number.isFinite(s.duration) ? s.duration : 0);
    if (spanEnd > end) end = spanEnd;
  }
  if (!Number.isFinite(start)) start = 0;
  if (!Number.isFinite(end)) end = start;
  return { start, end, total: Math.max(1, end - start) };
}

export function maxDepth(roots: TraceNode[]): number {
  let deepest = 0;
  const visited = new Set<TraceNode>();
  const stack: Array<{ node: TraceNode; depth: number }> = [];
  for (let i = roots.length - 1; i >= 0; i--) {
    stack.push({ node: roots[i], depth: 1 });
  }
  while (stack.length > 0) {
    const current = stack.pop()!;
    if (visited.has(current.node)) continue;
    visited.add(current.node);
    if (current.depth > deepest) deepest = current.depth;
    const children = current.node.children;
    for (let i = children.length - 1; i >= 0; i--) {
      if (!visited.has(children[i])) {
        stack.push({ node: children[i], depth: current.depth + 1 });
      }
    }
  }
  return deepest;
}

/** Pre-order DFS to flat rows via explicit stack, skipping collapsed subtrees. */
export function flatten(
  roots: TraceNode[],
  trace: JaegerTrace,
  collapsed: ReadonlySet<string>,
): WaterfallRow[] {
  const { start, total } = bounds(trace);
  const rows: WaterfallRow[] = [];
  const visited = new Set<TraceNode>();

  const MIN_WIDTH = 0.004; // keep sub-microsecond spans visible
  const stack: Array<{ node: TraceNode; depth: number }> = [];
  for (let i = roots.length - 1; i >= 0; i--) {
    stack.push({ node: roots[i], depth: 0 });
  }

  while (stack.length > 0) {
    const { node, depth } = stack.pop()!;
    if (visited.has(node)) continue;
    visited.add(node);

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
    if (!isCollapsed) {
      for (let i = node.children.length - 1; i >= 0; i--) {
        if (!visited.has(node.children[i])) {
          stack.push({ node: node.children[i], depth: depth + 1 });
        }
      }
    }
  }

  return rows;
}

/** Ordered unique services -> palette slot (1..5, cycling). */
export function serviceColorIndex(trace: JaegerTrace): Map<string, number> {
  const map = new Map<string, number>();
  let i = 0;
  for (const span of trace.spans ?? []) {
    const svc = serviceOf(trace, span);
    if (!map.has(svc)) map.set(svc, (i++ % 5) + 1);
  }
  return map;
}

/** CSS var reference for a service's palette color (a token, not a hex). */
export function chartVar(index: number): string {
  return `var(--chart-${index})`;
}
