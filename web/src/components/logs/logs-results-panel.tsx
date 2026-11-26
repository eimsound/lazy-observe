import {
  AlertCircle,
  Braces,
  Eye,
  EyeOff,
  ListTree,
  Loader2,
  Pause,
  Radio,
  Search,
  SearchX,
  Table2,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import type { LogRow } from "@/lib/api/logs";
import type { LogsViewMode } from "@/lib/logs-explorer";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { LogsLiveSnapshot } from "@/routes/logs-workspace";
import type { QueryViewState } from "@/routes/query-view-state";
import type {
  LogsGroupViewSettings,
  LogsTableViewSettings,
} from "./logs-explorer-model";
import { ExplorerError, ExplorerMessage } from "./logs-explorer-shared";
import {
  LogsGroupView,
  LogsJsonView,
  LogsTableView,
} from "./logs-explorer-views";
import { LogsLiveView } from "./logs-live-view";
import { LogsDownloadMenu } from "./logs-query-menus";

export interface LogsResultsPanelProps {
  project: string;
  view: LogsViewMode;
  hideLogs: boolean;
  hasExecuted: boolean;
  effectiveRange: { start: number; end: number } | null;
  downloading: boolean;
  downloadError: string;
  rows: LogRow[];
  rowsViewState: QueryViewState;
  rowsFetching: boolean;
  rowsStaleRetrying: boolean;
  groupSettings: LogsGroupViewSettings;
  tableSettings: LogsTableViewSettings;
  live: LogsLiveSnapshot;
  liveFollowing: boolean;
  liveRaw: boolean;
  liveOffset: string;
  liveLimit: number;
  onViewChange: (view: LogsViewMode) => void;
  onToggleHideLogs: () => void;
  onCancelDownload: () => void;
  onDownload: (format: "jsonl" | "csv", filename: string) => void;
  onRetryRows: () => void;
  onGroupSettingsChange: (settings: LogsGroupViewSettings) => void;
  onTableSettingsChange: (settings: LogsTableViewSettings) => void;
  onIncludeField: (field: string, value: string) => void;
  onExcludeField: (field: string, value: string) => void;
  onViewTrace: (traceId: string) => void;
  onShowContext: (row: LogRow) => void;
  onOpenRow: (row: LogRow) => void;
  onLiveStoppedChange: (stopped: boolean) => void;
  onLiveFollowingChange: (following: boolean) => void;
  onLiveRawChange: (raw: boolean) => void;
  onLiveOffsetChange: (offset: string) => void;
  onLiveLimitChange: (limit: number) => void;
  onLiveRetry: () => void;
  onLiveClear: () => void;
}

export function LogsResultsPanel({
  project,
  view,
  hideLogs,
  hasExecuted,
  effectiveRange,
  downloading,
  downloadError,
  rows,
  rowsViewState,
  rowsFetching,
  rowsStaleRetrying,
  groupSettings,
  tableSettings,
  live,
  liveFollowing,
  liveRaw,
  liveOffset,
  liveLimit,
  onViewChange,
  onToggleHideLogs,
  onCancelDownload,
  onDownload,
  onRetryRows,
  onGroupSettingsChange,
  onTableSettingsChange,
  onIncludeField,
  onExcludeField,
  onViewTrace,
  onShowContext,
  onOpenRow,
  onLiveStoppedChange,
  onLiveFollowingChange,
  onLiveRawChange,
  onLiveOffsetChange,
  onLiveLimitChange,
  onLiveRetry,
  onLiveClear,
}: LogsResultsPanelProps) {
  const { t } = useTranslation();

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2 p-4 pb-3">
        <Tabs
          value={view}
          onValueChange={(value) => onViewChange(value as LogsViewMode)}
        >
          <TabsList>
            <TabsTrigger value="group" className="gap-1.5">
              <ListTree className="size-4" />
              {t("logs.groupView")}
            </TabsTrigger>
            <TabsTrigger value="table" className="gap-1.5">
              <Table2 className="size-4" />
              {t("logs.tableView")}
            </TabsTrigger>
            <TabsTrigger value="json" className="gap-1.5">
              <Braces className="size-4" />
              JSON
            </TabsTrigger>
            <TabsTrigger value="live" className="gap-1.5">
              <Radio className="size-4" />
              {t("logs.liveView")}
            </TabsTrigger>
          </TabsList>
        </Tabs>
        <div className="flex flex-wrap items-center gap-2">
          {view !== "live" && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onToggleHideLogs}
            >
              {hideLogs ? <Eye /> : <EyeOff />}
              {hideLogs ? t("logs.showLogs") : t("logs.hideLogs")}
            </Button>
          )}
          <LogsDownloadMenu
            disabled={!hasExecuted}
            downloading={downloading}
            project={project}
            start={effectiveRange?.start}
            end={effectiveRange?.end}
            onCancel={onCancelDownload}
            onDownload={onDownload}
          />
        </div>
      </CardHeader>
      <CardContent className="p-4 pt-0">
        {downloadError && (
          <div className="mb-3 flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            <AlertCircle className="mt-0.5 size-3.5 shrink-0" />
            <span>{t("logs.downloadError", { error: downloadError })}</span>
          </div>
        )}
        {view === "live" ? (
          <LogsLiveView
            rows={live.rows}
            stopped={live.status === "stopped"}
            following={liveFollowing}
            connected={live.status === "open"}
            reconnecting={live.status === "retrying"}
            rawJson={liveRaw}
            offset={liveOffset}
            limit={liveLimit}
            rate={live.rate}
            dropped={live.dropped}
            error={live.error ?? ""}
            onStoppedChange={onLiveStoppedChange}
            onFollowingChange={onLiveFollowingChange}
            onRawJsonChange={onLiveRawChange}
            onOffsetChange={onLiveOffsetChange}
            onLimitChange={onLiveLimitChange}
            onRetry={onLiveRetry}
            onClear={onLiveClear}
            onOpenRow={onOpenRow}
          />
        ) : hideLogs ? (
          <ExplorerMessage
            icon={<Pause />}
            title={t("logs.logsHidden")}
            description={t("logs.logsHiddenDesc")}
          />
        ) : !hasExecuted ? (
          <ExplorerMessage
            icon={<Search />}
            title={t("common.waitingForQuery")}
          />
        ) : rowsViewState === "pending" ? (
          <div className="space-y-2">
            <div
              role="status"
              className="flex items-center gap-2 text-xs text-muted-foreground"
            >
              <Loader2 className="size-3 animate-spin" />
              {t("logs.loading")}
            </div>
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-[480px] w-full" />
          </div>
        ) : rowsViewState === "error" ? (
          <ExplorerError
            title={t("logs.errorTitle")}
            onRetry={onRetryRows}
          />
        ) : rowsViewState === "empty" ? (
          <ExplorerMessage
            icon={<SearchX />}
            title={t("common.noData")}
          />
        ) : (
          <>
            {rowsViewState === "stale" && (
              <QueryStaleNotice
                isRetrying={rowsStaleRetrying}
                onRetry={onRetryRows}
              />
            )}
            <div className="mb-2 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span>{t("logs.matchedLines", { count: rows.length })}</span>
              {rowsFetching && (
                <span role="status" className="inline-flex items-center gap-1">
                  <Loader2 className="size-3 animate-spin" />
                  {t("logs.loading")}
                </span>
              )}
            </div>
            {view === "group" && (
              <LogsGroupView
                rows={rows}
                settings={groupSettings}
                onSettingsChange={onGroupSettingsChange}
                onIncludeField={(field, value) => onIncludeField(field, value)}
                onExcludeField={(field, value) => onExcludeField(field, value)}
                onViewTrace={(traceId) => onViewTrace(traceId)}
                onShowContext={onShowContext}
              />
            )}
            {view === "table" && (
              <LogsTableView
                rows={rows}
                settings={tableSettings}
                onSettingsChange={onTableSettingsChange}
                onOpenRow={onOpenRow}
                onViewTrace={(traceId) => onViewTrace(traceId)}
              />
            )}
            {view === "json" && <LogsJsonView rows={rows} />}
          </>
        )}
      </CardContent>
    </Card>
  );
}
