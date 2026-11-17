import { hashString } from "@/lib/hash";
import type { FlameNode } from "@/lib/api/profiles";

/**
 * Self-built flamegraph layout model (no d3 / no chart lib). Pure functions
 * only — no rendering. The component in components/profiling/flamegraph.tsx
 * consumes these to draw a classic icicle (root on top, depth downward).
 *
 * Design (see web/README.md §Profiling):
 *  - computeLayout() flattens the tree into absolutely-positioned rows. Widths
 *    are fractions [0,1] of the CURRENT view; the focused node fills [0,1] and
 *    its subtree is laid out beneath it, so "zoom" is just re-focusing.
 *  - Children are ordered by value (desc) and packed left-to-right within their
 *    parent's x-extent; a node's self-time shows as the unfilled tail gap.
 *  - Nodes narrower than MIN_WIDTH of the view are folded into a trailing "…"
 *    aggregate (virtualization: we never emit sub-pixel nodes). Zooming the
 *    parent widens the view, so those frames re-expand and become visible.
 *  - frameColor() hashes each frame's top-level module to a --chart-N slot so
 *    same-module frames share a color family; runtime frames render grey. All
 *    color comes from CSS vars, so theme/palette switches follow for free.
 */

/** Frames folded into the "…" placeholder below this fraction of the view. */
export const MIN_WIDTH = 0.005;

export interface FlameLayoutNode {
  /** Stable React key (unique per position). */
  key: string;
  name: string;
  value: number;
  /** 0 = the focused node (top row); increases downward. */
  depth: number;
  /** Left edge as a fraction [0,1] of the full view width. */
  x0: number;
  /** Right edge as a fraction [0,1] of the full view width. */
  x1: number;
  /** Value of this node's parent (for the "share of parent" tooltip). */
  parentValue: number;
  /** --chart-N slot (1..5), or 0 for runtime/grey frames. */
  colorIndex: number;
  runtime: boolean;
  matched: boolean;
  /** True for a synthetic "…" placeholder folding N tiny siblings. */
  aggregated: boolean;
  aggregatedCount: number;
  truncated: boolean;
  /** Nodes from just below the focus down to this node (drives zoom). */
  pathFromFocus: FlameNode[];
}

export interface FlameLayout {
  nodes: FlameLayoutNode[];
  /** Number of depth rows (>=1). */
  depthCount: number;
  /** Value of the focused node (denominator for widths within the view). */
  focusValue: number;
}

/** Runtime / scheduler / syscall frames — rendered grey, never module-colored. */
export function isRuntimeFrame(name: string): boolean {
  return (
    /^runtime[./]/.test(name) ||
    /^syscall\./.test(name) ||
    name === "runtime" ||
    /^internal\/(runtime|poll)/.test(name)
  );
}

/**
 * Top-level module key used for coloring. For a Go-style path
 * `github.com/org/repo/pkg.(*T).Fn` we take the first three path segments
 * (`github.com/org/repo`) so a whole repo shares a color family; for a
 * slash-less stdlib frame (`encoding/json.Marshal` handled above, `sync.Mutex`)
 * we take the segment before the first dot.
 */
export function moduleKey(name: string): string {
  const paren = name.indexOf("(");
  const base = paren >= 0 ? name.slice(0, paren) : name;
  if (base.includes("/")) {
    const segs = base.split("/");
    return segs.slice(0, Math.min(3, segs.length)).join("/");
  }
  const dot = base.indexOf(".");
  return dot >= 0 ? base.slice(0, dot) : base;
}

/** Map a frame name to a palette slot: 0 = grey runtime, else 1..5. */
export function frameColor(name: string): { colorIndex: number; runtime: boolean } {
  if (isRuntimeFrame(name)) return { colorIndex: 0, runtime: true };
  return { colorIndex: (hashString(moduleKey(name)) % 5) + 1, runtime: false };
}

/** CSS var reference for a palette slot (a token, not a hex). */
export function chartVar(index: number): string {
  return `var(--chart-${index})`;
}

function matcher(query: string): (name: string) => boolean {
  const q = query.trim().toLowerCase();
  if (!q) return () => false;
  return (name) => name.toLowerCase().includes(q);
}

/** Count frames whose name matches the query across the WHOLE tree. */
export function countMatches(tree: FlameNode, query: string): number {
  const isMatch = matcher(query);
  let n = 0;
  const walk = (node: FlameNode) => {
    if (isMatch(node.name)) n++;
    node.children?.forEach(walk);
  };
  walk(tree);
  return n;
}

/**
 * Resolve a focus path (root..focus) to the node it points at, tolerating a
 * stale path after a data refresh by walking child names and falling back to
 * the deepest still-valid node.
 */
export function resolveFocus(tree: FlameNode, path: readonly FlameNode[]): FlameNode[] {
  const resolved: FlameNode[] = [tree];
  let cur = tree;
  for (let i = 1; i < path.length; i++) {
    const next = cur.children?.find((c) => c.name === path[i].name);
    if (!next) break;
    resolved.push(next);
    cur = next;
  }
  return resolved;
}

/**
 * Build the flat, positioned node list for one view. `focusPath` is root..focus
 * (focus fills the full width); `query` drives match flags.
 */
export function computeLayout(
  focusPath: readonly FlameNode[],
  query: string,
): FlameLayout {
  const focus = focusPath[focusPath.length - 1];
  const isMatch = matcher(query);
  const nodes: FlameLayoutNode[] = [];
  let depthCount = 1;

  const emit = (
    node: FlameNode,
    depth: number,
    x0: number,
    x1: number,
    parentValue: number,
    pathFromFocus: FlameNode[],
  ) => {
    const { colorIndex, runtime } = frameColor(node.name);
    depthCount = Math.max(depthCount, depth + 1);
    nodes.push({
      key: `${depth}:${x0.toFixed(6)}:${node.name}`,
      name: node.name,
      value: node.value,
      depth,
      x0,
      x1,
      parentValue,
      colorIndex,
      runtime,
      matched: isMatch(node.name),
      aggregated: false,
      aggregatedCount: 0,
      truncated: !!node.truncated,
      pathFromFocus,
    });

    const children = node.children;
    if (!children || children.length === 0 || node.value <= 0) return;
    const span = x1 - x0;

    const sorted = [...children].sort((a, b) => b.value - a.value);
    let cursor = x0;
    const tiny: FlameNode[] = [];
    for (const child of sorted) {
      const w = span * (child.value / node.value);
      if (w < MIN_WIDTH) {
        tiny.push(child);
        continue;
      }
      emit(child, depth + 1, cursor, cursor + w, node.value, [
        ...pathFromFocus,
        child,
      ]);
      cursor += w;
    }
    if (tiny.length > 0) {
      const tinyValue = tiny.reduce((s, c) => s + c.value, 0);
      const w = span * (tinyValue / node.value);
      depthCount = Math.max(depthCount, depth + 2);
      nodes.push({
        key: `${depth + 1}:${cursor.toFixed(6)}:__agg__`,
        name: `… ${tiny.length}`,
        value: tinyValue,
        depth: depth + 1,
        x0: cursor,
        x1: cursor + w,
        parentValue: node.value,
        colorIndex: 0,
        runtime: false,
        matched: false,
        aggregated: true,
        aggregatedCount: tiny.length,
        truncated: false,
        pathFromFocus: [...pathFromFocus],
      });
    }
  };

  emit(focus, 0, 0, 1, focus.value, []);
  return { nodes, depthCount, focusValue: focus.value };
}
