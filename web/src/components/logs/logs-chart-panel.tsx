import { BarChart3, Eye, Search, SearchX } from "lucide-react";
import { useTranslation } from "react-i18next";
import { formatPrometheusDuration } from "@/lib/metrics-query";
import {
  LogsHitsChart,
  type LogsHitsChartSeries,
} from "@/components/charts/logs-histogram";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import type { QueryViewState } from "@/routes/query-view-state";
import { ExplorerError, ExplorerMessage } from "./logs-explorer-shared";
import {
  LogsChartOptions,
  type LogsChartOptionsValue,
} from "./logs-query-controls";

export interface LogsChartPanelProps {
  chart: LogsChartOptionsValue;
  allFields: string[];
  chartSeries: LogsHitsChartSeries[];
  totalHits: number;
  requestDuration: number;
  chartStep: number;
  hasExecuted: boolean;
  chartViewState: QueryViewState;
  chartFetching: boolean;
  onChartChange: (patch: Partial<LogsChartOptionsValue>) => void;
  onShowChart: () => void;
  onRetry: () => void;
  onSelectRange: (startMs: number, endMs: number) => void;
  onFilterSeries: (fields: Record<string, string>) => void;
}

export function LogsChartPanel({
  chart,
  allFields,
  chartSeries,
  totalHits,
  requestDuration,
  chartStep,
  hasExecuted,
  chartViewState,
  chartFetching,
  onChartChange,
  onShowChart,
  onRetry,
  onSelectRange,
  onFilterSeries,
}: LogsChartPanelProps) {
  const { t } = useTranslation();

  if (chart.hidden) {
    return (
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={onShowChart}
      >
        <Eye />
        {t("logs.showChart")}
      </Button>
    );
  }

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2 p-4 pb-2">
        <div className="space-y-1">
          <CardTitle className="flex items-center gap-2 text-sm">
            <BarChart3 className="size-4" />
            {t("logs.hitsChart")}
          </CardTitle>
          <div className="flex flex-wrap gap-x-3 text-xs text-muted-foreground">
            <span>
              {chart.graphMode === "hits"
                ? t("logs.chartTotal", { count: totalHits })
                : t("logs.statsSeriesCount", { count: chartSeries.length })}
            </span>
            {requestDuration > 0 && (
              <span>{t("logs.queryDuration", {
                duration: `${Math.round(requestDuration)} ms`,
              })}</span>
            )}
            <span>{t("logs.chartStepValue", {
              duration: formatPrometheusDuration(chartStep),
            })}</span>
          </div>
        </div>
        <LogsChartOptions
          value={chart}
          fields={allFields}
          onChange={onChartChange}
        />
      </CardHeader>
      <CardContent className="p-4 pt-2">
        {!hasExecuted ? (
          <ExplorerMessage
            icon={<Search />}
            title={t("common.waitingForQuery")}
          />
        ) : chartViewState === "pending" ? (
          <Skeleton className="h-[280px] w-full" />
        ) : chartViewState === "error" ? (
          <ExplorerError
            title={t("logs.chartError")}
            onRetry={onRetry}
          />
        ) : chartViewState === "empty" ? (
          <ExplorerMessage
            icon={<SearchX />}
            title={t("common.noData")}
          />
        ) : (
          <>
            {chartViewState === "stale" && (
              <QueryStaleNotice
                isRetrying={chartFetching}
                onRetry={onRetry}
              />
            )}
            <LogsHitsChart
              series={chartSeries}
              height={280}
              stacked={chart.stacked}
              cumulative={chart.cumulative}
              onSelectRange={onSelectRange}
              onFilterSeries={onFilterSeries}
            />
          </>
        )}
      </CardContent>
    </Card>
  );
}
