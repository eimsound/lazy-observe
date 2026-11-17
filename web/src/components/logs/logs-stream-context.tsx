import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, Loader2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  queryLogs,
  type LogRow,
} from "@/lib/api/logs";
import type { LogsExtraFilterParams } from "@/lib/logs-explorer";
import { escapeLogsQLString } from "@/lib/logs-explorer";
import { useProject } from "@/lib/project/project-provider";
import { formatDateTime } from "@/lib/format";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { LevelDot } from "./level-indicator";
import { cn } from "@/lib/utils";

const CONTEXT_PAGE_SIZE = 30;

function contextQuery(
  anchor: LogRow,
  direction: "before" | "after",
  lines: number,
): string | null {
  const streamID = anchor.raw._stream_id;
  const timestamp = anchor.raw._time;
  if (
    (typeof streamID !== "string" && typeof streamID !== "number")
    || (typeof timestamp !== "string" && typeof timestamp !== "number")
  ) return null;
  const comparator = direction === "before" ? "<=" : ">=";
  const sort = direction === "before" ? "desc" : "asc";
  return [
    `_stream_id:"${escapeLogsQLString(String(streamID))}"`,
    `_time:${comparator}${String(timestamp)}`,
    `| sort by (_time) ${sort}`,
    `| limit ${lines}`,
  ].join(" ");
}

function withoutOneAnchor(rows: LogRow[], anchor: LogRow): LogRow[] {
  let removed = false;
  return rows.filter((row) => {
    const matches = row.raw._stream_id === anchor.raw._stream_id
      && row.raw._time === anchor.raw._time;
    if (!removed && matches) {
      removed = true;
      return false;
    }
    return true;
  });
}

export function LogsStreamContextDialog({
  anchor,
  open,
  start,
  end,
  filters,
  onOpenChange,
}: {
  anchor: LogRow | null;
  open: boolean;
  start: number;
  end: number;
  filters: LogsExtraFilterParams;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const { currentSlug: project } = useProject();
  const [beforeCount, setBeforeCount] = useState(CONTEXT_PAGE_SIZE);
  const [afterCount, setAfterCount] = useState(CONTEXT_PAGE_SIZE);
  const [wrap, setWrap] = useState(true);

  useEffect(() => {
    setBeforeCount(CONTEXT_PAGE_SIZE);
    setAfterCount(CONTEXT_PAGE_SIZE);
  }, [anchor?.id]);

  const context = useQuery({
    queryKey: [
      "logs-stream-context",
      project,
      anchor?.id,
      start,
      end,
      beforeCount,
      afterCount,
      filters,
    ],
    enabled: open && anchor !== null,
    retry: false,
    queryFn: async ({ signal }) => {
      const beforeQuery = contextQuery(anchor!, "before", beforeCount);
      const afterQuery = contextQuery(anchor!, "after", afterCount);
      if (!beforeQuery || !afterQuery) {
        throw new Error(t("logs.contextMissingAnchor"));
      }
      const [before, after] = await Promise.all([
        queryLogs({
          query: beforeQuery,
          start,
          end,
          limit: beforeCount,
          ...filters,
        }, signal),
        queryLogs({
          query: afterQuery,
          start,
          end,
          limit: afterCount,
          ...filters,
        }, signal),
      ]);
      return {
        before: withoutOneAnchor(before, anchor!).sort((left, right) => left.time - right.time),
        after: withoutOneAnchor(after, anchor!).sort((left, right) => left.time - right.time),
      };
    },
  });

  const rows = useMemo(
    () => anchor
      ? [...(context.data?.before ?? []), anchor, ...(context.data?.after ?? [])]
      : [],
    [anchor, context.data],
  );
  const duplicateTimestamps = useMemo(() => {
    const seen = new Set<number>();
    return rows.some((row) => {
      if (seen.has(row.time)) return true;
      seen.add(row.time);
      return false;
    });
  }, [rows]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-5xl">
        <DialogHeader>
          <DialogTitle>{t("logs.contextTitle")}</DialogTitle>
          <DialogDescription>{t("logs.contextDescription")}</DialogDescription>
        </DialogHeader>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <code className="max-w-3xl truncate rounded bg-muted px-2 py-1 text-xs">
            {String(anchor?.raw._stream ?? anchor?.raw._stream_id ?? "")}
          </code>
          <Label className="flex items-center gap-2 text-xs font-normal">
            {t("logs.wrapLines")}
            <Switch checked={wrap} onCheckedChange={setWrap} />
          </Label>
        </div>
        {duplicateTimestamps && (
          <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs">
            <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
            {t("logs.contextDuplicateTime")}
          </div>
        )}
        {context.error && (
          <div className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            {context.error.message}
          </div>
        )}
        <div className="flex justify-center">
          <Button
            variant="outline"
            size="sm"
            disabled={context.isFetching}
            onClick={() => setBeforeCount((count) => count + CONTEXT_PAGE_SIZE)}
          >
            {context.isFetching && <Loader2 className="animate-spin" />}
            {t("logs.loadOlderContext")}
          </Button>
        </div>
        <div className="max-h-[60vh] overflow-auto rounded-md border border-border">
          {context.isLoading ? (
            <div className="flex min-h-64 items-center justify-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("logs.loadingContext")}
            </div>
          ) : (
            rows.map((row, index) => (
              <div
                key={`${row.id}-${index}`}
                className={cn(
                  "flex items-start gap-2 border-b border-border/50 px-3 py-1.5 font-mono text-xs last:border-0",
                  row === anchor ? "bg-primary/10 ring-1 ring-inset ring-primary/30" : "hover:bg-muted/40",
                )}
              >
                <span className="w-[148px] shrink-0 tabular-nums text-muted-foreground">
                  {formatDateTime(row.time)}
                </span>
                <LevelDot level={row.level} />
                <span className={cn(
                  "min-w-0 flex-1 break-words",
                  wrap ? "whitespace-pre-wrap" : "truncate whitespace-nowrap",
                )}>
                  {row.message}
                </span>
              </div>
            ))
          )}
        </div>
        <div className="flex justify-center">
          <Button
            variant="outline"
            size="sm"
            disabled={context.isFetching}
            onClick={() => setAfterCount((count) => count + CONTEXT_PAGE_SIZE)}
          >
            {context.isFetching && <Loader2 className="animate-spin" />}
            {t("logs.loadNewerContext")}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
