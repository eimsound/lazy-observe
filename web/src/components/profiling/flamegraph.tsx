import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ChevronRight, Home } from "lucide-react";
import type { FlameNode } from "@/lib/api/profiles";
import {
  chartVar,
  computeLayout,
  resolveFocus,
  type FlameLayoutNode,
} from "@/lib/flamegraph";
import { formatPercent } from "@/lib/format";
import { cn } from "@/lib/utils";

/**
 * Self-built flamegraph (icicle, root on top). Layered absolutely-positioned
 * DOM: one block per node, width proportional to value, depth downward. All
 * geometry comes from lib/flamegraph.ts (pure). Colors are CSS-var tints of the
 * --chart-N palette (via color-mix on tokens, so text stays readable and the
 * whole thing re-themes for free); runtime/aggregate frames use --muted grey.
 *
 * Interactions: hover → tooltip (full name, samples, % of total, % of parent);
 * click → zoom (the node fills the width, breadcrumb to return); an active
 * search desaturates non-matching frames. Sub-0.5%-wide frames are folded into
 * a "…" placeholder and re-expand when their parent is zoomed.
 */

const ROW_H = 23;

interface FlamegraphProps {
  tree: FlameNode;
  /** Absolute total (root value) for the "% of total" readout. */
  total: number;
  query: string;
  className?: string;
}

interface HoverState {
  node: FlameLayoutNode;
  x: number;
  y: number;
}

export function Flamegraph({ tree, total, query, className }: FlamegraphProps) {
  const { t } = useTranslation();
  const [focusPath, setFocusPath] = useState<FlameNode[]>([tree]);
  const [hover, setHover] = useState<HoverState | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  // Re-anchor the focus to the (possibly new) tree on every data refresh,
  // tolerating a stale path by walking child names.
  useEffect(() => {
    setFocusPath((prev) => resolveFocus(tree, prev));
  }, [tree]);

  const safePath = useMemo(() => resolveFocus(tree, focusPath), [tree, focusPath]);
  const layout = useMemo(() => computeLayout(safePath, query), [safePath, query]);
  const searchActive = query.trim().length > 0;

  function zoom(node: FlameLayoutNode) {
    if (node.aggregated || node.pathFromFocus.length === 0) return;
    setFocusPath([...safePath, ...node.pathFromFocus]);
    setHover(null);
  }

  return (
    <div className={cn("overflow-hidden rounded-lg border border-border", className)}>
      {/* Breadcrumb / zoom trail */}
      <div className="flex items-center gap-1 overflow-x-auto border-b border-border bg-muted/30 px-3 py-1.5 text-xs">
        <button
          type="button"
          onClick={() => setFocusPath([tree])}
          className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          aria-label={t("profiling.resetZoom")}
        >
          <Home className="size-3" />
        </button>
        {safePath.map((node, i) => {
          const last = i === safePath.length - 1;
          return (
            <div key={`${i}:${node.name}`} className="flex shrink-0 items-center gap-1">
              <ChevronRight className="size-3 text-muted-foreground/50" />
              <button
                type="button"
                onClick={() => setFocusPath(safePath.slice(0, i + 1))}
                disabled={last}
                className={cn(
                  "max-w-[280px] truncate rounded px-1.5 py-0.5 font-mono",
                  last
                    ? "text-foreground"
                    : "text-muted-foreground hover:bg-accent hover:text-foreground",
                )}
                title={node.name}
              >
                {node.name}
              </button>
            </div>
          );
        })}
      </div>

      {/* Icicle canvas */}
      <div
        ref={containerRef}
        className="relative w-full bg-background"
        style={{ height: layout.depthCount * ROW_H }}
        onMouseLeave={() => setHover(null)}
      >
        {layout.nodes.map((node) => {
          const wide = node.x1 - node.x0;
          const showLabel = wide > 0.02;
          const dim = searchActive && !node.matched && !node.aggregated;
          const isHover = hover?.node.key === node.key;
          const tint = tintFor(node, isHover);
          const accent =
            node.aggregated || node.runtime ? "var(--border)" : chartVar(node.colorIndex);
          return (
            <div
              key={node.key}
              role="button"
              tabIndex={node.aggregated ? -1 : 0}
              title={node.aggregated ? undefined : node.name}
              onClick={() => zoom(node)}
              onKeyDown={(e) => {
                if (e.key === "Enter") zoom(node);
              }}
              onMouseMove={(e) => setHover({ node, x: e.clientX, y: e.clientY })}
              onMouseEnter={(e) => setHover({ node, x: e.clientX, y: e.clientY })}
              className={cn(
                "absolute flex items-center overflow-hidden rounded-[3px] border-l-2 px-1.5 text-[10px] leading-none transition-[filter,opacity]",
                node.aggregated ? "cursor-default text-muted-foreground italic" : "cursor-pointer text-foreground",
                node.matched && searchActive && "ring-1 ring-ring ring-inset",
              )}
              style={{
                left: `${node.x0 * 100}%`,
                width: `${wide * 100}%`,
                top: node.depth * ROW_H,
                height: ROW_H - 2,
                backgroundColor: tint,
                borderLeftColor: accent,
                opacity: dim ? 0.32 : 1,
              }}
            >
              {showLabel && (
                <span className="truncate font-mono">
                  {node.aggregated ? node.name : shortName(node.name)}
                </span>
              )}
            </div>
          );
        })}
      </div>

      {hover && <FlameTooltip hover={hover} total={total} />}
    </div>
  );
}

function FlameTooltip({ hover, total }: { hover: HoverState; total: number }) {
  const { t } = useTranslation();
  const { node } = hover;
  // Position near the cursor, flipping left/up near the viewport edge.
  const flipX = hover.x > window.innerWidth - 320;
  const flipY = hover.y > window.innerHeight - 160;
  return (
    <div
      className="pointer-events-none fixed z-50 max-w-[320px] rounded-md border border-border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-md"
      style={{
        left: flipX ? undefined : hover.x + 14,
        right: flipX ? window.innerWidth - hover.x + 14 : undefined,
        top: flipY ? undefined : hover.y + 14,
        bottom: flipY ? window.innerHeight - hover.y + 14 : undefined,
      }}
    >
      <div className="mb-1 break-all font-mono font-medium">
        {node.aggregated ? t("profiling.aggregatedFrames", { count: node.aggregatedCount }) : node.name}
      </div>
      <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-muted-foreground">
        <span>{t("profiling.samples")}</span>
        <span className="text-right font-medium tabular-nums text-foreground">
          {node.value.toLocaleString()}
        </span>
        <span>{t("profiling.ofTotal")}</span>
        <span className="text-right font-medium tabular-nums text-foreground">
          {formatPercent(total > 0 ? node.value / total : 0)}
        </span>
        <span>{t("profiling.ofParent")}</span>
        <span className="text-right font-medium tabular-nums text-foreground">
          {formatPercent(node.parentValue > 0 ? node.value / node.parentValue : 0)}
        </span>
      </div>
    </div>
  );
}

/** Tint a block: a light wash of its palette color over the card surface. */
function tintFor(node: FlameLayoutNode, hovered: boolean): string {
  if (node.aggregated || node.runtime) {
    return `color-mix(in oklab, var(--muted) ${hovered ? 90 : 62}%, var(--card))`;
  }
  const pct = hovered ? 42 : node.matched ? 34 : 22;
  return `color-mix(in oklab, ${chartVar(node.colorIndex)} ${pct}%, var(--card))`;
}

/** Trim the leading module path for the on-block label (tooltip has the full). */
function shortName(name: string): string {
  const slash = name.lastIndexOf("/");
  return slash >= 0 ? name.slice(slash + 1) : name;
}
