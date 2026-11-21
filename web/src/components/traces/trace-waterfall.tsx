import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ChevronDown, ChevronRight, AlertTriangle } from "lucide-react";
import type { JaegerSpan, JaegerTrace } from "@/lib/api/traces";
import {
  bounds,
  buildTree,
  chartVar,
  flatten,
  serviceColorIndex,
} from "@/lib/trace-tree";
import { formatDurationMicros, formatPercent } from "@/lib/format";
import { useVirtualWindow } from "@/lib/use-virtual-window";
import { cn } from "@/lib/utils";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

/**
 * Self-built trace waterfall. Left = collapsible span tree (depth-indented,
 * service-color-dotted, error-flagged); right = a proportional timeline where
 * each bar's left/width are fractions of the total trace duration. Services are
 * colored from the --chart-N palette (via CSS vars, so it re-themes for free),
 * error spans use the --level-error token, and hovering a bar reveals its
 * duration and share of the trace. No hardcoded colors anywhere.
 */

const ROW_H = 30;
const WATERFALL_VIEWPORT_MAX_H = 640;
const WATERFALL_OVERSCAN = 12;
const TICKS = [0, 0.25, 0.5, 0.75, 1];

interface TraceWaterfallProps {
  trace: JaegerTrace;
  selectedSpanId?: string;
  onSelectSpan: (span: JaegerSpan) => void;
}

export function TraceWaterfall({
  trace,
  selectedSpanId,
  onSelectSpan,
}: TraceWaterfallProps) {
  const { t } = useTranslation();
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());

  const roots = useMemo(() => buildTree(trace), [trace]);
  const colorIndex = useMemo(() => serviceColorIndex(trace), [trace]);
  const rows = useMemo(
    () => flatten(roots, trace, collapsed),
    [roots, trace, collapsed],
  );
  const total = useMemo(() => bounds(trace).total, [trace]);

  const virtual = useVirtualWindow({
    count: rows.length,
    estimateSize: ROW_H,
    overscan: WATERFALL_OVERSCAN,
    defaultViewportHeight: WATERFALL_VIEWPORT_MAX_H,
  });
  const { scrollToIndex } = virtual;

  const lastScrolledSpanRef = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!selectedSpanId) {
      lastScrolledSpanRef.current = undefined;
      return;
    }
    if (lastScrolledSpanRef.current === selectedSpanId) return;
    const targetIndex = rows.findIndex((row) => row.span.spanID === selectedSpanId);
    if (targetIndex < 0) return;
    lastScrolledSpanRef.current = selectedSpanId;
    scrollToIndex(targetIndex, { align: "auto", behavior: "smooth" });
  }, [rows, selectedSpanId, scrollToIndex]);

  const visibleRows = useMemo(
    () => rows.slice(virtual.startIndex, virtual.endIndex),
    [rows, virtual.startIndex, virtual.endIndex],
  );

  function toggle(spanID: string) {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(spanID)) next.delete(spanID);
      else next.add(spanID);
      return next;
    });
  }

  const services = useMemo(
    () => Array.from(colorIndex.entries()),
    [colorIndex],
  );

  return (
    <div className="overflow-hidden rounded-lg border border-border">
      {/* Service legend + hint */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 border-b border-border bg-muted/30 px-4 py-2">
        {services.map(([svc, idx]) => (
          <span key={svc} className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <span
              className="size-2.5 rounded-sm"
              style={{ backgroundColor: chartVar(idx) }}
            />
            {svc}
          </span>
        ))}
        <span className="ml-auto text-xs text-muted-foreground/70">
          {t("traces.waterfallHint")}
        </span>
      </div>

      {/* Ruler */}
      <div className="flex border-b border-border bg-background">
        <div className="w-[38%] min-w-[220px] max-w-[440px] shrink-0 border-r border-border px-4 py-1.5 text-xs font-medium text-muted-foreground">
          {t("traces.rootLabel")}
        </div>
        <div className="relative flex-1">
          {TICKS.map((frac) => (
            <div
              key={frac}
              className="absolute top-0 flex h-full items-center"
              style={{
                left: `${frac * 100}%`,
                transform: frac === 1 ? "translateX(-100%)" : frac === 0 ? "none" : "translateX(-50%)",
              }}
            >
              <span className="whitespace-nowrap px-1 py-1.5 text-[10px] tabular-nums text-muted-foreground/70">
                {formatDurationMicros(total * frac)}
              </span>
            </div>
          ))}
        </div>
      </div>

      {/* Rows */}
      <div
        ref={virtual.scrollRef}
        data-virtualized={virtual.isVirtualized ? "true" : "false"}
        className={cn(virtual.isVirtualized && "max-h-[640px] overflow-y-auto")}
      >
        {virtual.paddingTop > 0 && (
          <div aria-hidden="true" style={{ height: virtual.paddingTop }} />
        )}
        {visibleRows.map((row) => {
          const colorIdx = colorIndex.get(row.service) ?? 1;
          const selected = row.span.spanID === selectedSpanId;
          const labelRight = row.offset + row.width > 0.82;
          return (
            <div
              key={row.span.spanID}
              role="button"
              tabIndex={0}
              onClick={() => onSelectSpan(row.span)}
              onKeyDown={(e) => {
                if (e.target !== e.currentTarget) return;
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  onSelectSpan(row.span);
                }
              }}
              className={cn(
                "flex cursor-pointer border-b border-border/50 hover:bg-muted/50",
                selected && "bg-accent",
              )}
              style={{ height: ROW_H }}
            >
              {/* Left: tree label */}
              <div
                className="flex w-[38%] min-w-[220px] max-w-[440px] shrink-0 items-center gap-1 border-r border-border pr-3"
                style={{ paddingLeft: 8 + row.depth * 14 }}
              >
                {row.hasChildren ? (
                  <button
                    type="button"
                    aria-label={row.span.operationName}
                    aria-expanded={!row.collapsed}
                    className="shrink-0 rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
                    onClick={(e) => {
                      e.stopPropagation();
                      toggle(row.span.spanID);
                    }}
                  >
                    {row.collapsed ? (
                      <ChevronRight className="size-3.5" />
                    ) : (
                      <ChevronDown className="size-3.5" />
                    )}
                  </button>
                ) : (
                  <span className="w-[18px] shrink-0" />
                )}
                <span
                  className="size-2 shrink-0 rounded-sm"
                  style={{ backgroundColor: chartVar(colorIdx) }}
                />
                {row.error && (
                  <AlertTriangle className="size-3 shrink-0 text-level-error" />
                )}
                <span
                  className={cn(
                    "truncate text-xs",
                    row.error ? "text-level-error" : "text-foreground",
                  )}
                  title={row.span.operationName}
                >
                  {row.span.operationName}
                </span>
                <span className="ml-1 truncate text-[10px] text-muted-foreground">
                  {row.service}
                </span>
              </div>

              {/* Right: timeline bar */}
              <div className="relative flex-1">
                <Tooltip delayDuration={80}>
                  <TooltipTrigger asChild>
                    <div
                      className="absolute top-1/2 h-3 -translate-y-1/2 rounded-sm"
                      style={{
                        left: `${row.offset * 100}%`,
                        width: `${row.width * 100}%`,
                        minWidth: 2,
                        backgroundColor: row.error ? "var(--level-error)" : chartVar(colorIdx),
                      }}
                    />
                  </TooltipTrigger>
                  <TooltipContent side="top" className="font-mono">
                    <div className="space-y-0.5">
                      <div className="font-sans font-medium">{row.span.operationName}</div>
                      <div>{formatDurationMicros(row.span.duration)}</div>
                      <div className="text-primary-foreground/70">
                        {formatPercent(row.durationFraction)}
                      </div>
                    </div>
                  </TooltipContent>
                </Tooltip>
                <span
                  className={cn(
                    "pointer-events-none absolute top-1/2 -translate-y-1/2 whitespace-nowrap px-1.5 text-[10px] tabular-nums text-muted-foreground",
                  )}
                  style={
                    labelRight
                      ? { right: `${(1 - row.offset) * 100}%`, transform: "translate(0, -50%)" }
                      : { left: `${(row.offset + row.width) * 100}%` }
                  }
                >
                  {formatDurationMicros(row.span.duration)}
                </span>
              </div>
            </div>
          );
        })}
        {virtual.paddingBottom > 0 && (
          <div aria-hidden="true" style={{ height: virtual.paddingBottom }} />
        )}
      </div>
    </div>
  );
}
