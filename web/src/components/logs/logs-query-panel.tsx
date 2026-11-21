import { Play, RotateCcw, Search, Square } from "lucide-react";
import { useTranslation } from "react-i18next";
import dayjs from "@/lib/dayjs";
import {
  parseLogsTimestamp,
  type LogsQueryTimeRange,
} from "@/lib/api/logs";
import {
  MAX_LOGS_LIMIT,
  type LogsAutocompleteContext,
  type LogsExtraFilter,
  type LogsHistoryEntry,
} from "@/lib/logs-explorer";
import type { MetricsRange } from "@/lib/metrics-query";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { ExtraFiltersBar } from "./logs-extra-filters-bar";
import { LogsLimitInput, LogsTimeControls } from "./logs-query-controls";
import { LogsExamplesMenu, LogsHistoryMenu } from "./logs-query-menus";
import { LogsQLEditor, type LogsQLEditorSuggestion } from "./logsql-editor";

export interface LogsQueryPanelProps {
  queryText: string;
  autocomplete: boolean;
  queryFetching: boolean;
  range: MetricsRange;
  localTime: boolean;
  refreshSeconds: number;
  limit: number;
  filters: LogsExtraFilter[];
  history: LogsHistoryEntry[];
  timeRangeData: LogsQueryTimeRange | null;
  onQueryChange: (value: string) => void;
  onRunOrCancel: () => void;
  onComplete: (
    context: LogsAutocompleteContext,
    signal: AbortSignal,
    currentQuery: string,
  ) => Promise<LogsQLEditorSuggestion[]>;
  onHistoryPrevious: () => void;
  onHistoryNext: () => void;
  onRangeChange: (range: MetricsRange) => void;
  onLocalTimeChange: (localTime: boolean) => void;
  onRefreshSecondsChange: (seconds: number) => void;
  onRefresh: () => void;
  onAutocompleteChange: (enabled: boolean) => void;
  onLimitChange: (limit: number) => void;
  onReset: () => void;
  onSelectExample: (query: string) => void;
  onLoadHistory: (query: string) => void;
  onRunHistory: (query: string) => void;
  onFavoriteHistory: (query: string) => void;
  onRemoveHistory: (query: string) => void;
  onClearHistory: () => void;
  onRemoveFilter: (field: string, value: string) => void;
  onClearFilters: () => void;
}

export function LogsQueryPanel({
  queryText,
  autocomplete,
  queryFetching,
  range,
  localTime,
  refreshSeconds,
  limit,
  filters,
  history,
  timeRangeData,
  onQueryChange,
  onRunOrCancel,
  onComplete,
  onHistoryPrevious,
  onHistoryNext,
  onRangeChange,
  onLocalTimeChange,
  onRefreshSecondsChange,
  onRefresh,
  onAutocompleteChange,
  onLimitChange,
  onReset,
  onSelectExample,
  onLoadHistory,
  onRunHistory,
  onFavoriteHistory,
  onRemoveHistory,
  onClearHistory,
  onRemoveFilter,
  onClearFilters,
}: LogsQueryPanelProps) {
  const { t } = useTranslation();

  return (
    <Card className="overflow-visible">
      <CardContent className="space-y-3 p-4">
        <div className="flex items-start gap-2">
          <LogsQLEditor
            id="logs-query"
            value={queryText}
            autocompleteEnabled={autocomplete}
            onChange={onQueryChange}
            onRun={onRunOrCancel}
            onComplete={onComplete}
            onHistoryPrevious={onHistoryPrevious}
            onHistoryNext={onHistoryNext}
          />
          <Button
            className="h-10 shrink-0"
            variant={queryFetching ? "destructive" : "default"}
            disabled={!queryText.trim() || limit < 1 || limit > MAX_LOGS_LIMIT}
            onClick={onRunOrCancel}
          >
            {queryFetching ? <Square className="fill-current" /> : <Play />}
            {queryFetching ? t("logs.cancel") : t("logs.run")}
          </Button>
        </div>

        <div className="flex flex-wrap items-center justify-between gap-2">
          <LogsTimeControls
            range={range}
            localTime={localTime}
            refreshSeconds={refreshSeconds}
            disabled={queryFetching}
            onRangeChange={onRangeChange}
            onLocalTimeChange={onLocalTimeChange}
            onRefreshSecondsChange={onRefreshSecondsChange}
            onRefresh={onRefresh}
          />
          <div className="flex flex-wrap items-center gap-2">
            <Label className="flex items-center gap-2 text-xs font-normal text-muted-foreground">
              {t("logs.autocomplete")}
              <Switch checked={autocomplete} onCheckedChange={onAutocompleteChange} />
            </Label>
            <LogsLimitInput value={limit} onChange={onLimitChange} />
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onReset}
            >
              <RotateCcw />
              {t("common.reset")}
            </Button>
            <LogsExamplesMenu onSelect={onSelectExample} />
            <LogsHistoryMenu
              history={history}
              onLoad={onLoadHistory}
              onRun={onRunHistory}
              onFavorite={onFavoriteHistory}
              onRemove={onRemoveHistory}
              onClear={onClearHistory}
            />
          </div>
        </div>

        <ExtraFiltersBar
          filters={filters}
          onRemoveFilter={onRemoveFilter}
          onClearFilters={onClearFilters}
        />

        {timeRangeData?.hasTimeFilter && (
          <div className="flex items-start gap-2 rounded-md border border-primary/30 bg-primary/5 px-3 py-2 text-xs">
            <Search className="mt-0.5 size-3.5 shrink-0 text-primary" />
            <span>
              {t("logs.queryTimeOverride", {
                start: dayjs(parseLogsTimestamp(timeRangeData.start).ms)
                  .format("YYYY-MM-DD HH:mm:ss"),
                end: dayjs(parseLogsTimestamp(timeRangeData.end).ms)
                  .format("YYYY-MM-DD HH:mm:ss"),
              })}
            </span>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
