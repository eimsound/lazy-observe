import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useIsFetching, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { ArrowDown, ArrowUp, Database, Loader2, RefreshCw } from "lucide-react";
import {
  VITALS,
  aggregateTrend,
  rateVital,
  queryVitalSeries,
  getVitalPages,
  getVitalsByPage,
  getEndpointStats,
  lastValue,
  type EndpointStatsResult,
  type PageVitals,
  type PerfWindow,
  type VitalKey,
  type VitalMeta,
  type VitalRating,
} from "@/lib/api/performance";
import { IS_MOCK } from "@/lib/settings";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { useTimeWindow } from "@/hooks/use-time-window";
import type { TimeSeries } from "@/lib/api/types";
import { formatPercent } from "@/lib/format";
import { PageActions, PageContainer } from "@/components/layout/page";
import { TimeSeriesChart } from "@/components/charts/time-series-chart";
import { Sparkline } from "@/components/common/sparkline";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { buildEndpointTraceSearch, traceSearchToURL } from "./traces-search";
import { queryViewState } from "./query-view-state";

const ALL_PAGES = "__all__";

const RATING_CLASS: Record<VitalRating, string> = {
  good: "border-vital-good/40 text-vital-good",
  ni: "border-vital-ni/40 text-vital-ni",
  poor: "border-vital-poor/40 text-vital-poor",
};
const RATING_KEY: Record<VitalRating, string> = {
  good: "performance.ratingGood",
  ni: "performance.ratingNi",
  poor: "performance.ratingPoor",
};

function formatVital(meta: VitalMeta, value: number): string {
  return `${value.toFixed(meta.decimals)}${meta.unit === "ms" ? "ms" : ""}`;
}

const PERFORMANCE_QUERY_ROOTS = new Set([
  "vital-pages",
  "vital",
  "vital-trend",
  "vitals-by-page",
  "endpoints",
]);

export function PerformancePage() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const { getWindow, advance } = useTimeWindow("1h");
  const isFetching =
    useIsFetching({
      predicate: (query) => PERFORMANCE_QUERY_ROOTS.has(String(query.queryKey[0])),
    }) > 0;

  function advanceWindow() {
    advance();
    void queryClient.invalidateQueries({
      predicate: (query) => PERFORMANCE_QUERY_ROOTS.has(String(query.queryKey[0])),
    });
  }

  return (
    <PageContainer>
      <PageActions>
        {IS_MOCK && (
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("performance.mockBadge")}
          </Badge>
        )}
        <Button variant="outline" size="sm" onClick={advanceWindow} disabled={isFetching}>
          {isFetching ? <Loader2 className="size-3.5 animate-spin" /> : <RefreshCw className="size-3.5" />}
          {t("common.refresh")}
        </Button>
      </PageActions>

      <Tabs defaultValue="vitals">
        <TabsList>
          <TabsTrigger value="vitals">{t("performance.tabVitals")}</TabsTrigger>
          <TabsTrigger value="endpoints">{t("performance.tabEndpoints")}</TabsTrigger>
        </TabsList>

        <TabsContent value="vitals" className="mt-4">
          <WebVitalsTab getWindow={getWindow} />
        </TabsContent>
        <TabsContent value="endpoints" className="mt-4">
          <EndpointsTab getWindow={getWindow} />
        </TabsContent>
      </Tabs>
    </PageContainer>
  );
}

// ── Web Vitals ────────────────────────────────────────────────────────────────

function WebVitalsTab({ getWindow }: { getWindow: () => PerfWindow }) {
  const { t } = useTranslation();
  const [page, setPage] = useState(ALL_PAGES);
  const [selectedVital, setSelectedVital] = useState<VitalKey>("lcp");

  const pageParam = page === ALL_PAGES ? undefined : page;
  const pagesQuery = useProjectQuery<string[]>({
    queryKey: ["vital-pages"],
    queryFn: ({ signal, project }) => getVitalPages(getWindow(), signal, project),
    retry: false,
  });
  const pagesState = queryViewState({
    hasData: pagesQuery.data !== undefined,
    isLoading: pagesQuery.isLoading,
    isError: pagesQuery.isError,
    isEmpty: pagesQuery.data?.length === 0,
  });

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-2">
        <span className="text-sm text-muted-foreground">{t("performance.page")}</span>
        <Select value={page} onValueChange={setPage}>
          <SelectTrigger className="w-56">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL_PAGES}>{t("performance.allPages")}</SelectItem>
            {(pagesQuery.data ?? []).map((p) => (
              <SelectItem key={p} value={p}>
                {p}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {pagesState === "pending" ? (
          <span className="text-xs text-muted-foreground">{t("performance.loading")}</span>
        ) : pagesState === "error" ? (
          <button
            type="button"
            className="text-xs text-destructive underline-offset-2 hover:underline"
            onClick={() => void pagesQuery.refetch()}
          >
            {t("performance.pageDiscoveryError")} · {t("common.retry")}
          </button>
        ) : pagesState === "empty" ? (
          <span className="text-xs text-muted-foreground">{t("performance.noPages")}</span>
        ) : null}
      </div>
      {pagesState === "stale" && (
        <QueryStaleNotice
          isRetrying={pagesQuery.isFetching}
          onRetry={() => void pagesQuery.refetch()}
        />
      )}

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-5">
        {VITALS.map((meta) => (
          <VitalCard
            key={meta.key}
            meta={meta}
            page={pageParam}
            getWindow={getWindow}
            active={selectedVital === meta.key}
            onSelect={() => setSelectedVital(meta.key)}
          />
        ))}
      </div>

      <VitalTrend meta={VITALS.find((v) => v.key === selectedVital)!} page={pageParam} getWindow={getWindow} />

      <VitalsByPageTable getWindow={getWindow} />
    </div>
  );
}

function VitalCard({
  meta,
  page,
  getWindow,
  active,
  onSelect,
}: {
  meta: VitalMeta;
  page: string | undefined;
  getWindow: () => PerfWindow;
  active: boolean;
  onSelect: () => void;
}) {
  const { t } = useTranslation();
  const query = useProjectQuery<TimeSeries[]>({
    queryKey: ["vital", meta.key, page ?? "all"],
    queryFn: ({ signal, project }) => queryVitalSeries(meta.key, page, getWindow(), signal, project),
    retry: false,
  });

  const value = lastValue(query.data);
  const rating = value != null ? rateVital(meta.key, value) : null;
  const trend = aggregateTrend(query.data);
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: query.data?.length === 0,
  });

  return (
    <button
      type="button"
      onClick={() => {
        if (viewState === "error") void query.refetch();
        else onSelect();
      }}
      className={cn(
        "rounded-lg border bg-card p-3 text-left transition-colors",
        active ? "border-ring" : "border-border hover:border-ring/60",
      )}
    >
      <div className="text-xs font-medium text-muted-foreground">{t(`performance.vital_${meta.key}`)}</div>
      {viewState === "pending" ? (
        <Skeleton className="mt-2 h-7 w-20" />
      ) : viewState === "error" ? (
        <div className="mt-2 text-xs font-medium text-destructive">
          {t("performance.errorTitle")} · {t("common.retry")}
        </div>
      ) : (
        <div className="mt-1 flex items-baseline justify-between gap-2">
          <span className={cn("text-2xl font-semibold tabular-nums", rating && RATING_CLASS[rating].split(" ")[1])}>
            {value != null ? formatVital(meta, value) : "—"}
          </span>
          {rating && (
            <span
              className={cn(
                "rounded border px-1.5 py-0.5 text-[10px] font-medium",
                RATING_CLASS[rating],
              )}
            >
              {t(RATING_KEY[rating])}
            </span>
          )}
        </div>
      )}
      <div className={cn("mt-2 h-6", rating ? RATING_CLASS[rating].split(" ")[1] : "text-muted-foreground")}>
        {viewState !== "pending" && viewState !== "error" && (
          <Sparkline data={trend} width={160} height={24} className="w-full" />
        )}
      </div>
      {viewState === "stale" && (
        <span className="mt-1 block text-[10px] text-amber-600">
          {t("common.staleDataTitle")}
        </span>
      )}
    </button>
  );
}

function VitalTrend({
  meta,
  page,
  getWindow,
}: {
  meta: VitalMeta;
  page: string | undefined;
  getWindow: () => PerfWindow;
}) {
  const { t } = useTranslation();
  const query = useProjectQuery<TimeSeries[]>({
    queryKey: ["vital-trend", meta.key, page ?? "all"],
    queryFn: ({ signal, project }) => queryVitalSeries(meta.key, page, getWindow(), signal, project),
    retry: false,
  });
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: query.data?.length === 0,
  });

  return (
    <Card>
      <CardContent className="p-4">
        <div className="mb-2 text-sm font-medium">
          {t("performance.trendTitle", { name: t(`performance.vital_${meta.key}`) })}
        </div>
        {viewState === "stale" && (
          <QueryStaleNotice
            isRetrying={query.isFetching}
            onRetry={() => void query.refetch()}
          />
        )}
        {viewState === "pending" ? (
          <Skeleton className="h-[240px] w-full" />
        ) : viewState === "error" ? (
          <div className="flex h-[240px] items-center justify-center text-sm text-destructive" role="alert">
            <button type="button" className="underline-offset-2 hover:underline" onClick={() => void query.refetch()}>
              {t("performance.errorTitle")} · {t("common.retry")}
            </button>
          </div>
        ) : viewState === "ready" || viewState === "stale" ? (
          <TimeSeriesChart series={query.data ?? []} height={240} />
        ) : (
          <div className="flex h-[240px] items-center justify-center text-sm text-muted-foreground">
            {t("performance.empty")}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function VitalsByPageTable({ getWindow }: { getWindow: () => PerfWindow }) {
  const { t } = useTranslation();
  const query = useProjectQuery<PageVitals[]>({
    queryKey: ["vitals-by-page"],
    queryFn: ({ signal, project }) => getVitalsByPage(getWindow(), signal, project),
    retry: false,
  });
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: query.data?.length === 0,
  });

  return (
    <Card>
      <CardContent className="p-0">
        <div className="border-b border-border px-4 py-2.5 text-sm font-medium">
          {t("performance.byPageTitle")}
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
          <div className="space-y-2 p-4">
            {Array.from({ length: 5 }).map((_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : viewState === "error" ? (
          <div className="p-6 text-center text-sm text-destructive" role="alert">
            <button type="button" className="underline-offset-2 hover:underline" onClick={() => void query.refetch()}>
              {t("performance.errorTitle")} · {t("common.retry")}
            </button>
          </div>
        ) : viewState === "empty" ? (
          <div className="p-6 text-center text-sm text-muted-foreground">
            {t("performance.empty")}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border text-left text-xs text-muted-foreground">
                  <th className="px-4 py-2 font-medium">{t("performance.colPage")}</th>
                  {VITALS.map((v) => (
                    <th key={v.key} className="px-4 py-2 text-right font-medium uppercase">
                      {v.key}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {(query.data ?? []).map((row) => (
                  <tr key={row.page} className="border-b border-border/60 last:border-b-0 hover:bg-muted/30">
                    <td className="px-4 py-2 font-mono text-xs">{row.page}</td>
                    {VITALS.map((v) => {
                      const value = row.values[v.key];
                      if (value == null) {
                        return (
                          <td key={v.key} className="px-4 py-2 text-right text-muted-foreground">
                            —
                          </td>
                        );
                      }
                      const rating = rateVital(v.key, value);
                      return (
                        <td
                          key={v.key}
                          className={cn("px-4 py-2 text-right tabular-nums", RATING_CLASS[rating].split(" ")[1])}
                        >
                          {formatVital(v, value)}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// ── Backend endpoints ─────────────────────────────────────────────────────────

type EndpointSortKey = "route" | "throughput" | "p50" | "p95" | "p99" | "error_rate";

function EndpointsTab({ getWindow }: { getWindow: () => PerfWindow }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [sortKey, setSortKey] = useState<EndpointSortKey>("p95");
  const [dir, setDir] = useState<"asc" | "desc">("desc");

  const query = useProjectQuery<EndpointStatsResult>({
    queryKey: ["endpoints"],
    queryFn: ({ signal, project }) => getEndpointStats(getWindow(), signal, project),
    retry: false,
  });

  const rows = useMemo(() => {
    const data = [...(query.data?.endpoints ?? [])];
    data.sort((a, b) => {
      const av = a[sortKey];
      const bv = b[sortKey];
      const cmp = typeof av === "number" && typeof bv === "number"
        ? av - bv
        : String(av).localeCompare(String(bv));
      return dir === "asc" ? cmp : -cmp;
    });
    return data;
  }, [query.data, sortKey, dir]);
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: rows.length === 0,
  });

  function toggleSort(key: EndpointSortKey) {
    if (key === sortKey) setDir((d) => (d === "asc" ? "desc" : "asc"));
    else {
      setSortKey(key);
      setDir(key === "route" ? "asc" : "desc");
    }
  }

  function openEndpointTraces(endpoint: EndpointStatsResult["endpoints"][number]) {
    if (!query.data) return;
    const params = buildEndpointTraceSearch(endpoint, query.data);
    void navigate({ to: "/traces", search: traceSearchToURL(params) });
  }

  const COLUMNS: Array<{ key: EndpointSortKey; labelKey: string; numeric: boolean }> = [
    { key: "route", labelKey: "performance.colEndpoint", numeric: false },
    { key: "throughput", labelKey: "performance.colThroughput", numeric: true },
    { key: "p50", labelKey: "performance.colP50", numeric: true },
    { key: "p95", labelKey: "performance.colP95", numeric: true },
    { key: "p99", labelKey: "performance.colP99", numeric: true },
    { key: "error_rate", labelKey: "performance.colErrorRate", numeric: true },
  ];

  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">{t("performance.endpointsHint")}</p>
      {query.data?.truncated && (
        <p className="text-xs text-vital-ni" role="status">
          {t("performance.endpointsTruncated")}
        </p>
      )}
      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={query.isFetching}
          onRetry={() => void query.refetch()}
        />
      )}
      <Card>
        <CardContent className="p-0">
          {viewState === "pending" ? (
            <div className="space-y-2 p-4">
              {Array.from({ length: 6 }).map((_, i) => (
                <Skeleton key={i} className="h-9 w-full" />
              ))}
            </div>
          ) : viewState === "error" ? (
            <div className="space-y-2 p-6 text-center text-sm" role="alert">
              <div className="font-medium text-destructive">{t("performance.errorTitle")}</div>
              <p className="text-xs text-muted-foreground">{t("common.queryErrorDesc")}</p>
              <Button variant="outline" size="sm" onClick={() => void query.refetch()}>
                {t("common.retry")}
              </Button>
            </div>
          ) : viewState === "empty" ? (
            <div className="p-6 text-center text-sm text-muted-foreground">
              {t("performance.empty")}
            </div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs text-muted-foreground">
                    {COLUMNS.map((c) => (
                      <th
                        key={c.key}
                        className={cn("px-4 py-2 font-medium", c.numeric && "text-right")}
                      >
                        <button
                          type="button"
                          onClick={() => toggleSort(c.key)}
                          className={cn(
                            "inline-flex items-center gap-1 hover:text-foreground",
                            c.numeric && "flex-row-reverse",
                          )}
                        >
                          {t(c.labelKey)}
                          {sortKey === c.key &&
                            (dir === "asc" ? (
                              <ArrowUp className="size-3" />
                            ) : (
                              <ArrowDown className="size-3" />
                            ))}
                        </button>
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <tr
                      key={`${r.service} ${r.method} ${r.route}`}
                      role="link"
                      tabIndex={0}
                      aria-label={t("performance.inspectTraces", {
                        method: r.method,
                        route: r.route,
                        service: r.service,
                      })}
                      onClick={() => openEndpointTraces(r)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          openEndpointTraces(r);
                        }
                      }}
                      className="group cursor-pointer border-b border-border/60 last:border-b-0 hover:bg-muted/30 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
                    >
                      <td className="px-4 py-2">
                        <div className="flex items-center gap-1.5 text-left">
                          <Badge variant="outline" className="font-mono text-[10px]">
                            {r.method}
                          </Badge>
                          <span>
                            <span className="block font-mono text-xs">{r.route}</span>
                            <span className="block text-[10px] text-muted-foreground">{r.service}</span>
                          </span>
                        </div>
                      </td>
                      <td className="px-4 py-2 text-right tabular-nums">
                        {r.throughput < 1 ? r.throughput.toFixed(2) : r.throughput.toFixed(1)}
                        <span className="ml-1 text-xs text-muted-foreground">{t("performance.unitReqs")}</span>
                      </td>
                      <td className="px-4 py-2 text-right tabular-nums">{r.p50.toFixed(1)}ms</td>
                      <td className="px-4 py-2 text-right tabular-nums">
                        <span>{r.p95.toFixed(1)}ms</span>
                        <Sparkline
                          data={r.series.map((point) => point.p95)}
                          width={64}
                          height={16}
                          className="ml-auto mt-1 text-muted-foreground"
                        />
                      </td>
                      <td className="px-4 py-2 text-right tabular-nums">{r.p99.toFixed(1)}ms</td>
                      <td
                        className={cn(
                          "px-4 py-2 text-right tabular-nums",
                          r.error_rate > 0.05
                            ? "text-vital-poor"
                            : r.error_rate > 0.01
                              ? "text-vital-ni"
                              : "text-vital-good",
                        )}
                      >
                        {formatPercent(r.error_rate)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
