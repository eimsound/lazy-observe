import { Braces, Check, ChevronDown, ChevronRight, Copy } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { useVirtualWindow } from "@/lib/use-virtual-window";
import { cn } from "@/lib/utils";
import {
  buildStableLogRowKeys,
  safeJsonStringify,
} from "./logs-explorer-model";
import { EmptyLogs } from "./logs-explorer-shared";
import { useCopyFeedback, type CommonViewProps } from "./logs-explorer-utils";

export type {
  LogsFieldStats,
  LogsGroupViewSettings,
  LogsSortDirection,
  LogsTableSort,
  LogsTableViewSettings,
} from "./logs-explorer-model";
export type {
  LogsContextActionHandler,
  LogsCopyHandler,
  LogsFieldActionHandler,
  LogsTraceActionHandler,
} from "./logs-explorer-utils";
export { LogsGroupView, type LogsGroupViewProps } from "./logs-group-view";
export { LogsTableView, type LogsTableViewProps } from "./logs-table-view";

export type LogsJsonViewProps = CommonViewProps;

const JSON_COMPACT_ROW_H = 24;
const JSON_LINE_H = 20;
const JSON_VIEWPORT_MAX_H = 640;
const JSON_OVERSCAN = 12;

export function LogsJsonView({
  rows,
  onCopy,
  className,
}: LogsJsonViewProps) {
  const { t } = useTranslation();
  const { copiedKey, copy } = useCopyFeedback(onCopy);
  const rowKeys = useMemo(() => buildStableLogRowKeys(rows), [rows]);
  const [expandedIds, setExpandedIds] = useState<ReadonlySet<string>>(
    () => new Set(),
  );

  // Prune expanded keys that no longer exist in the current result set.
  useEffect(() => {
    setExpandedIds((prev) => {
      if (prev.size === 0) return prev;
      const activeKeys = new Set(rowKeys);
      let changed = false;
      const next = new Set<string>();
      for (const key of prev) {
        if (activeKeys.has(key)) next.add(key);
        else changed = true;
      }
      return changed ? next : prev;
    });
  }, [rowKeys]);

  const toggleExpanded = useCallback((rowKey: string) => {
    setExpandedIds((prev) => {
      const next = new Set(prev);
      if (next.has(rowKey)) next.delete(rowKey);
      else next.add(rowKey);
      return next;
    });
  }, []);

  const estimateRowSize = useMemo(
    () =>
      expandedIds.size === 0
        ? JSON_COMPACT_ROW_H
        : (index: number) => {
            const row = rows[index];
            const rowKey = rowKeys[index];
            if (!row || !rowKey || !expandedIds.has(rowKey)) {
              return JSON_COMPACT_ROW_H;
            }
            const estimatedLines = Math.max(3, Object.keys(row.raw).length + 2);
            return estimatedLines * JSON_LINE_H + 8;
          },
    [rows, rowKeys, expandedIds],
  );

  const getItemKey = useCallback(
    (index: number) => {
      const rowKey = rowKeys[index];
      if (!rowKey) return index;
      return `${rowKey}:${expandedIds.has(rowKey) ? 1 : 0}`;
    },
    [rowKeys, expandedIds],
  );

  const virtual = useVirtualWindow({
    count: rows.length,
    estimateSize: estimateRowSize,
    overscan: JSON_OVERSCAN,
    defaultViewportHeight: JSON_VIEWPORT_MAX_H,
    getItemKey,
  });

  const handleCopyAll = useCallback(() => {
    const payload = rows.map((row) => safeJsonStringify(row.raw)).join("\n");
    void copy(payload, "jsonl");
  }, [copy, rows]);

  return (
    <section
      className={cn(
        "overflow-hidden rounded-md border border-border/70 bg-background",
        className,
      )}
      aria-label={t("logs.jsonLines")}
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/60 p-2">
        <span className="inline-flex items-center gap-2 text-xs text-muted-foreground">
          <Braces className="size-4" />
          {t("logs.jsonLines")}
          <span className="tabular-nums">
            {t("logs.matchedLines", { count: rows.length })}
          </span>
        </span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={rows.length === 0}
          onClick={handleCopyAll}
        >
          {copiedKey === "jsonl" ? <Check /> : <Copy />}
          {copiedKey === "jsonl"
            ? t("common.copied")
            : t("logs.copyAllJson")}
        </Button>
      </div>

      {rows.length === 0 ? (
        <EmptyLogs />
      ) : (
        <div
          ref={virtual.scrollRef}
          role="list"
          data-virtualized={virtual.isVirtualized ? "true" : "false"}
          tabIndex={0}
          className="max-h-[640px] overflow-auto py-2 font-mono text-xs leading-relaxed text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
        >
          <div className="w-max min-w-full">
            {virtual.paddingTop > 0 && (
              <div aria-hidden="true" style={{ height: virtual.paddingTop }} />
            )}
            {virtual.items.map((item) => {
              const row = rows[item.index];
              if (!row) return null;
              const rowKey = rowKeys[item.index] ?? String(item.index);
              const isExpanded = expandedIds.has(rowKey);
              const itemKey = `${rowKey}:${isExpanded ? 1 : 0}`;
              const content = safeJsonStringify(row.raw, isExpanded);
              const rowCopied = copiedKey === rowKey;
              return (
                <div
                  key={itemKey}
                  ref={virtual.measureElement}
                  role="listitem"
                  data-virtual-index={item.index}
                  className="group flex items-start gap-1.5 px-2 py-0.5 hover:bg-muted/40"
                >
                  <button
                    type="button"
                    aria-expanded={isExpanded}
                    aria-label={
                      isExpanded ? t("logs.collapseRow") : t("logs.expandRow")
                    }
                    className="mt-0.5 inline-flex size-4 shrink-0 items-center justify-center rounded text-muted-foreground hover:bg-accent hover:text-foreground"
                    onClick={() => toggleExpanded(rowKey)}
                  >
                    {isExpanded ? (
                      <ChevronDown className="size-3.5" />
                    ) : (
                      <ChevronRight className="size-3.5" />
                    )}
                  </button>
                  <button
                    type="button"
                    aria-label={t("logs.copyRow")}
                    title={t("logs.copyRow")}
                    className="mt-0.5 inline-flex size-4 shrink-0 items-center justify-center rounded text-muted-foreground opacity-70 hover:bg-accent hover:text-foreground group-hover:opacity-100 focus-visible:opacity-100"
                    onClick={() => void copy(content, rowKey)}
                  >
                    {rowCopied ? (
                      <Check className="size-3" />
                    ) : (
                      <Copy className="size-3" />
                    )}
                  </button>
                  <span
                    aria-hidden="true"
                    className="min-w-[2.5rem] shrink-0 select-none pr-1 text-right text-[11px] tabular-nums text-muted-foreground/60"
                  >
                    {item.index + 1}
                  </span>
                  {isExpanded ? (
                    <pre className="m-0 whitespace-pre font-mono text-xs leading-relaxed text-foreground">
                      {content}
                    </pre>
                  ) : (
                    <code className="whitespace-pre font-mono text-xs leading-relaxed text-foreground">
                      {content}
                    </code>
                  )}
                </div>
              );
            })}
            {virtual.paddingBottom > 0 && (
              <div aria-hidden="true" style={{ height: virtual.paddingBottom }} />
            )}
          </div>
        </div>
      )}
    </section>
  );
}
