import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { type UseQueryResult } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import {
  AlertCircle,
  ArrowUpRight,
  Bot,
  Coins,
  Database,
  Hash,
  RefreshCw,
} from "lucide-react";
import {
  getLLMCalls,
  getLLMSummary,
  getLLMTimeseries,
  type LLMAggregate,
  type LLMCall,
  type LLMCallsResult,
  type LLMCallStatus,
  type LLMSummary,
  type LLMTimeseriesResult,
  type LLMTrendMetric,
} from "@/lib/api/llm";
import { RANGE_PRESETS } from "@/lib/api/ranges";
import { ApiError } from "@/lib/api/client";
import { IS_MOCK } from "@/lib/settings";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { useTimeWindow } from "@/hooks/use-time-window";
import { useRecentIngestNotice } from "@/hooks/use-recent-ingest-notice";
import { timeWindowEndMs } from "@/lib/ingesting-window";
import { formatDurationMicros } from "@/lib/format";
import dayjs, { dayjsLocale } from "@/lib/dayjs";
import type { TimeSeries } from "@/lib/api/types";
import { cn } from "@/lib/utils";
import { EmptyState } from "@/components/common/empty-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { RecentIngestNotice } from "@/components/common/recent-ingest-notice";
import { TimeSeriesChart } from "@/components/charts/time-series-chart";
import { PageActions, PageContainer } from "@/components/layout/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { queryViewState } from "./query-view-state";

const ALL = "__all__";

export function LLMObservabilityPage() {
  const { t } = useTranslation();
  const { rangeId, setRangeId, window, advance } = useTimeWindow("24h");
  const [trendMetric, setTrendMetric] = useState<LLMTrendMetric>("calls");
  const [service, setService] = useState(ALL);
  const [model, setModel] = useState(ALL);

  const llmWindow = { start: window.start, end: window.end };
  const isRecentlyIngesting = useRecentIngestNotice(timeWindowEndMs(llmWindow.end));

  const summaryQuery = useProjectQuery<LLMSummary, ApiError>({
    queryKey: ["llm-summary", llmWindow.start, llmWindow.end],
    queryFn: ({ signal, project }) => getLLMSummary(llmWindow, signal, project),
    retry: false,
  });
  const trendQuery = useProjectQuery<LLMTimeseriesResult, ApiError>({
    queryKey: ["llm-timeseries", trendMetric, llmWindow.start, llmWindow.end],
    queryFn: ({ signal, project }) => getLLMTimeseries(llmWindow, trendMetric, signal, project),
    retry: false,
  });
  const callsQuery = useProjectQuery<LLMCallsResult, ApiError>({
    queryKey: ["llm-calls", service, model, llmWindow.start, llmWindow.end],
    queryFn: ({ signal, project }) =>
      getLLMCalls(
        llmWindow,
        {
          service: service === ALL ? undefined : service,
          model: model === ALL ? undefined : model,
          limit: 100,
        },
        signal,
        project,
      ),
    retry: false,
  });

  const refreshing = summaryQuery.isFetching || trendQuery.isFetching || callsQuery.isFetching;
  const summaryState = queryViewState({
    hasData: summaryQuery.data !== undefined,
    isLoading: summaryQuery.isLoading,
    isError: summaryQuery.isError,
    isEmpty: (summaryQuery.data?.calls ?? 0) === 0,
  });

  return (
    <PageContainer>
      <PageActions>
        {IS_MOCK && (
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("llm.mockBadge")}
          </Badge>
        )}
        <Select value={rangeId} onValueChange={setRangeId}>
          <SelectTrigger className="w-40" aria-label={t("llm.timeRange")}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {RANGE_PRESETS.map((range) => (
              <SelectItem key={range.id} value={range.id}>
                {t(range.labelKey)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button variant="outline" size="sm" onClick={() => advance()} disabled={refreshing}>
          <RefreshCw className={cn("size-4", refreshing && "animate-spin")} />
          {t("common.refresh")}
        </Button>
      </PageActions>

      {summaryState === "stale" && (
        <QueryStaleNotice
          isRetrying={summaryQuery.isFetching}
          onRetry={() => void summaryQuery.refetch()}
        />
      )}

      {isRecentlyIngesting && summaryState !== "error" && (
        <RecentIngestNotice
          isRefreshing={refreshing}
          onRefresh={() => advance()}
        />
      )}

      {summaryState === "pending" ? (
        <LLMPageSkeleton />
      ) : summaryState === "error" ? (
        <Card>
          <CardContent className="p-6">
            <EmptyState
              icon={<AlertCircle className="size-6 text-destructive" />}
              title={t("llm.errorTitle")}
              desc={summaryQuery.error?.message ?? t("common.queryErrorDesc")}
              action={
                <Button variant="outline" size="sm" onClick={() => void summaryQuery.refetch()}>
                  {t("common.retry")}
                </Button>
              }
            />
          </CardContent>
        </Card>
      ) : summaryState === "empty" || !summaryQuery.data ? (
        <Card>
          <CardContent className="p-6">
            <EmptyState
              icon={<Bot className="size-6" />}
              title={t("llm.emptyTitle")}
              desc={t("llm.emptyDesc")}
            />
          </CardContent>
        </Card>
      ) : (
        <LLMContent
          summary={summaryQuery.data}
          trendMetric={trendMetric}
          setTrendMetric={setTrendMetric}
          trendQuery={trendQuery}
          callsQuery={callsQuery}
          service={service}
          setService={setService}
          model={model}
          setModel={setModel}
        />
      )}
    </PageContainer>
  );
}

function LLMContent({
  summary,
  trendMetric,
  setTrendMetric,
  trendQuery,
  callsQuery,
  service,
  setService,
  model,
  setModel,
}: {
  summary: LLMSummary;
  trendMetric: LLMTrendMetric;
  setTrendMetric: (metric: LLMTrendMetric) => void;
  trendQuery: UseQueryResult<Awaited<ReturnType<typeof getLLMTimeseries>>, ApiError>;
  callsQuery: UseQueryResult<Awaited<ReturnType<typeof getLLMCalls>>, ApiError>;
  service: string;
  setService: (service: string) => void;
  model: string;
  setModel: (model: string) => void;
}) {
  const { t } = useTranslation();
  const totalTokens = summary.input_tokens + summary.output_tokens;
  const trendState = queryViewState({
    hasData: trendQuery.data !== undefined,
    isLoading: trendQuery.isLoading || (trendQuery.isFetching && !trendQuery.data),
    isError: trendQuery.isError,
    isEmpty: (trendQuery.data?.points.length ?? 0) === 0,
  });

  return (
    <div className="space-y-4">
      {(summary.truncated || trendQuery.data?.truncated || callsQuery.data?.truncated) && (
        <div className="rounded-md border border-vital-ni/40 bg-vital-ni/5 px-3 py-2 text-xs text-vital-ni" role="status">
          {t("llm.truncated")}
        </div>
      )}

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <KPI icon={<Hash className="size-4" />} label={t("llm.totalCalls")} value={formatInteger(summary.calls)} />
        <KPI icon={<Bot className="size-4" />} label={t("llm.totalTokens")} value={formatInteger(totalTokens)} />
        <KPI icon={<Coins className="size-4" />} label={t("llm.totalCost")} value={formatCost(summary.cost)} />
        <KPI
          icon={<AlertCircle className="size-4" />}
          label={t("llm.pricingCoverage")}
          value={summary.unpriced ? t("llm.hasUnpriced") : t("llm.allPriced")}
          warning={summary.unpriced}
        />
      </div>

      <Card>
        <CardContent className="p-4">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
            <div>
              <div className="text-sm font-medium">{t("llm.trendTitle")}</div>
              <div className="text-xs text-muted-foreground">{t("llm.trendHint")}</div>
            </div>
            <Select value={trendMetric} onValueChange={(value) => setTrendMetric(value as LLMTrendMetric)}>
              <SelectTrigger className="w-40" aria-label={t("llm.trendMetric")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="calls">{t("llm.metricCalls")}</SelectItem>
                <SelectItem value="tokens">{t("llm.metricTokens")}</SelectItem>
                <SelectItem value="cost">{t("llm.metricCost")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {trendState === "stale" && (
            <QueryStaleNotice
              isRetrying={trendQuery.isFetching}
              onRetry={() => void trendQuery.refetch()}
            />
          )}
          {trendState === "pending" ? (
            <Skeleton className="h-[260px] w-full" />
          ) : trendState === "error" ? (
            <QueryError
              message={trendQuery.error?.message ?? t("common.queryErrorDesc")}
              onRetry={() => void trendQuery.refetch()}
            />
          ) : trendState === "empty" || !trendQuery.data ? (
            <div className="flex h-[260px] items-center justify-center text-sm text-muted-foreground">
              {t("llm.emptyTrend")}
            </div>
          ) : (
            <TimeSeriesChart series={trendSeries(trendQuery.data.metric, trendQuery.data.points)} height={260} />
          )}
        </CardContent>
      </Card>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        <AggregateTable title={t("llm.byModel")} dimensionLabel={t("llm.model")} rows={summary.by_model.map((row) => ({ name: row.model, ...row }))} />
        <AggregateTable title={t("llm.byService")} dimensionLabel={t("llm.service")} rows={summary.by_service.map((row) => ({ name: row.service, ...row }))} />
      </div>

      <CallsTable
        query={callsQuery}
        models={summary.by_model.map((row) => row.model)}
        services={summary.by_service.map((row) => row.service)}
        model={model}
        setModel={setModel}
        service={service}
        setService={setService}
      />
    </div>
  );
}

function KPI({ icon, label, value, warning = false }: { icon: ReactNode; label: string; value: string; warning?: boolean }) {
  return (
    <Card>
      <CardContent className="p-4">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">{icon}{label}</div>
        <div className={cn("mt-2 text-2xl font-semibold tabular-nums", warning && "text-vital-ni")}>{value}</div>
      </CardContent>
    </Card>
  );
}

function AggregateTable({
  title,
  dimensionLabel,
  rows,
}: {
  title: string;
  dimensionLabel: string;
  rows: Array<LLMAggregate & { name: string }>;
}) {
  const { t } = useTranslation();
  return (
    <Card>
      <CardContent className="p-0">
        <div className="border-b border-border px-4 py-3 text-sm font-medium">{title}</div>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border text-left text-xs text-muted-foreground">
                <th className="px-4 py-2 font-medium">{dimensionLabel}</th>
                <th className="px-3 py-2 text-right font-medium">{t("llm.calls")}</th>
                <th className="px-3 py-2 text-right font-medium">{t("llm.tokens")}</th>
                <th className="px-4 py-2 text-right font-medium">{t("llm.cost")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.name} className="border-b border-border/60 last:border-b-0">
                  <td className="max-w-64 truncate px-4 py-2 font-mono text-xs" title={row.name}>{row.name}</td>
                  <td className="px-3 py-2 text-right tabular-nums">{formatInteger(row.calls)}</td>
                  <td className="px-3 py-2 text-right tabular-nums">{formatInteger(row.input_tokens + row.output_tokens)}</td>
                  <td className="px-4 py-2 text-right tabular-nums">
                    <span>{formatCost(row.cost)}</span>
                    {row.unpriced && <span className="ml-1 text-vital-ni" title={t("llm.unpricedHint")}>*</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </CardContent>
    </Card>
  );
}

function CallsTable({
  query,
  models,
  services,
  model,
  setModel,
  service,
  setService,
}: {
  query: UseQueryResult<Awaited<ReturnType<typeof getLLMCalls>>, ApiError>;
  models: string[];
  services: string[];
  model: string;
  setModel: (model: string) => void;
  service: string;
  setService: (service: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const navigate = useNavigate();
  const rows = query.data?.calls ?? [];
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <Card>
      <CardContent className="p-0">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border px-4 py-3">
          <div>
            <div className="text-sm font-medium">{t("llm.recentCalls")}</div>
            <div className="text-xs text-muted-foreground">{t("llm.recentCallsHint")}</div>
          </div>
          <div className="flex flex-wrap gap-2">
            <Select value={service} onValueChange={setService}>
              <SelectTrigger className="w-44" aria-label={t("llm.service")}><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>{t("llm.allServices")}</SelectItem>
                {services.map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}
              </SelectContent>
            </Select>
            <Select value={model} onValueChange={setModel}>
              <SelectTrigger className="w-56" aria-label={t("llm.model")}><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>{t("llm.allModels")}</SelectItem>
                {models.map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
        </div>
        {viewState === "stale" && (
          <div className="p-4 pb-0">
            <QueryStaleNotice
              isRetrying={query.isFetching}
              onRetry={() => void query.refetch()}
            />
          </div>
        )}
        {viewState === "pending" ? (
          <div className="space-y-2 p-4">{Array.from({ length: 6 }).map((_, index) => <Skeleton key={index} className="h-9 w-full" />)}</div>
        ) : viewState === "error" ? (
          <QueryError
            message={query.error?.message ?? t("common.queryErrorDesc")}
            onRetry={() => void query.refetch()}
          />
        ) : viewState === "empty" ? (
          <div className="p-8 text-center text-sm text-muted-foreground">{t("llm.noMatchingCalls")}</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border text-left text-xs text-muted-foreground">
                  <th className="px-4 py-2 font-medium">{t("llm.time")}</th>
                  <th className="px-3 py-2 font-medium">{t("llm.service")}</th>
                  <th className="px-3 py-2 font-medium">{t("llm.model")}</th>
                  <th className="px-3 py-2 text-right font-medium">{t("llm.tokens")}</th>
                  <th className="px-3 py-2 text-right font-medium">{t("llm.duration")}</th>
                  <th className="px-3 py-2 text-right font-medium">{t("llm.cost")}</th>
                  <th className="px-4 py-2 text-right font-medium">{t("common.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((call) => (
                  <CallRow
                    key={`${call.trace_id}-${call.time}`}
                    call={call}
                    locale={i18n.language}
                    onOpen={() => void navigate({ to: "/traces/$traceID", params: { traceID: call.trace_id } })}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function CallRow({ call, locale, onOpen }: { call: LLMCall; locale: string; onOpen: () => void }) {
  const { t } = useTranslation();
  return (
    <tr className="border-b border-border/60 last:border-b-0 hover:bg-muted/30">
      <td className="whitespace-nowrap px-4 py-2 text-xs text-muted-foreground">
        {dayjs(call.time).locale(dayjsLocale(locale)).format("L LTS")}
      </td>
      <td className="px-3 py-2"><div className="flex items-center gap-2"><StatusBadge status={call.status} /><span className="max-w-44 truncate" title={call.service}>{call.service}</span></div></td>
      <td className="max-w-64 truncate px-3 py-2 font-mono text-xs" title={call.model}>{call.model}</td>
      <td className="px-3 py-2 text-right tabular-nums" title={`${t("llm.inputTokens")}: ${call.input_tokens} · ${t("llm.outputTokens")}: ${call.output_tokens}`}>{formatInteger(call.input_tokens + call.output_tokens)}</td>
      <td className="px-3 py-2 text-right tabular-nums">{formatDurationMicros(call.duration)}</td>
      <td className="px-3 py-2 text-right tabular-nums">{formatCost(call.cost)}{call.unpriced && <span className="ml-1 text-vital-ni" title={t("llm.unpricedHint")}>*</span>}</td>
      <td className="px-4 py-2 text-right"><Button variant="ghost" size="sm" onClick={onOpen}><ArrowUpRight className="size-4" />{t("llm.viewTrace")}</Button></td>
    </tr>
  );
}

function StatusBadge({ status }: { status: LLMCallStatus }) {
  const { t } = useTranslation();
  return (
    <Badge
      variant={status === "error" ? "destructive" : "outline"}
      className={cn(status === "ok" && "border-vital-good/40 text-vital-good", status === "unset" && "text-muted-foreground")}
    >
      {t(`llm.status_${status}`)}
    </Badge>
  );
}

function QueryError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-40 flex-col items-center justify-center gap-2 p-6 text-center" role="alert">
      <AlertCircle className="size-6 text-destructive" />
      <div className="text-sm font-medium">{t("llm.errorTitle")}</div>
      <div className="max-w-lg text-xs text-muted-foreground">{message}</div>
      <Button variant="outline" size="sm" onClick={onRetry}>{t("common.retry")}</Button>
    </div>
  );
}

function LLMPageSkeleton() {
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">{Array.from({ length: 4 }).map((_, index) => <Skeleton key={index} className="h-24 w-full" />)}</div>
      <Skeleton className="h-[320px] w-full" />
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2"><Skeleton className="h-56 w-full" /><Skeleton className="h-56 w-full" /></div>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

function trendSeries(metric: LLMTrendMetric, points: Array<{ time: string; value: number }>): TimeSeries[] {
  return [{
    name: metric,
    labels: { metric },
    points: points.map((point) => [dayjs(point.time).valueOf(), point.value]),
  }];
}

function formatInteger(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: value >= 100_000 ? "compact" : "standard", maximumFractionDigits: 1 }).format(value);
}

function formatCost(value: number): string {
  const digits = value > 0 && value < 0.01 ? 6 : value < 1 ? 4 : 2;
  return new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", minimumFractionDigits: digits, maximumFractionDigits: digits }).format(value);
}
