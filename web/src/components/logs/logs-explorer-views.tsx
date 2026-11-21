import { Braces, Check, Copy } from "lucide-react";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { safeJsonStringify } from "./logs-explorer-model";
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

export function LogsJsonView({
  rows,
  onCopy,
  className,
}: LogsJsonViewProps) {
  const { t } = useTranslation();
  const { copiedKey, copy } = useCopyFeedback(onCopy);
  const jsonLines = useMemo(
    () => rows.map((row) => safeJsonStringify(row.raw)).join("\n"),
    [rows],
  );

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
          onClick={() => void copy(jsonLines, "jsonl")}
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
        <pre
          className="max-h-[640px] overflow-auto whitespace-pre p-3 font-mono text-xs leading-relaxed text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
          tabIndex={0}
        >
          {jsonLines}
        </pre>
      )}
    </section>
  );
}
