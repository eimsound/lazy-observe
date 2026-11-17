import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import {
  Activity,
  AlertCircle,
  BellRing,
  Bug,
  Database,
  Gauge,
  Percent,
  ScrollText,
  Timer,
} from "lucide-react";
import { queryRange } from "@/lib/api/metrics";
import dayjs from "@/lib/dayjs";
import { logsHits, type LogsHistogram } from "@/lib/api/logs";
import { listAlerts, eventSeverity } from "@/lib/api/alerts";
import { listIssues, type IssueListResult } from "@/lib/api/issues";
import { rangeById, resolveRange } from "@/lib/api/ranges";
import { IS_MOCK } from "@/lib/settings";
import { formatRelativeTime } from "@/lib/format";
import type { TimeSeries } from "@/lib/api/types";
import { PageActions, PageContainer } from "@/components/layout/page";
import { TimeSeriesChart } from "@/components/charts/time-series-chart";
import { LogsHistogramChart } from "@/components/charts/logs-histogram";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { IssueLevelBadge } from "@/components/issues/issue-level-badge";
import {
  RECENT_ISSUES_PARAMS,
  recentIssuesView,
} from "@/routes/overview-issues";

/** Latest numeric value across the first series (last non-null point). */
function lastValue(series: TimeSeries[] | undefined): number | null {
  const pts = series?.[0]?.points;
  if (!pts) return null;
  for (let i = pts.length - 1; i >= 0; i--) {
    const v = pts[i][1];
    if (v != null && Number.isFinite(v)) return v;
  }
  return null;
}

const DASH_QUERIES = {
  requestRate: "sum(rate(http_requests_total[5m]))",
  p95: "histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))",
  errorRate:
    'sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))',
};

export function OverviewPage() {
  const { t } = useTranslation();

  // Capture a single window for the whole overview so every panel aligns.
  const window = useMemo(() => {
    const preset = rangeById("1h");
    return { preset, ...resolveRange(preset, dayjs().valueOf()) };
  }, []);

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("metrics.mockBadge")}
          </Badge>
        </PageActions>
      )}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
        <MetricPanel
          icon={<Gauge className="size-4" />}
          title={t("overview.cardRequestRate")}
          query={DASH_QUERIES.requestRate}
          unit={t("overview.unitReqs")}
          window={window}
        />
        <MetricPanel
          icon={<Timer className="size-4" />}
          title={t("overview.cardP95")}
          query={DASH_QUERIES.p95}
          unit={t("overview.unitMs")}
          window={window}
        />
        <MetricPanel
          icon={<Percent className="size-4" />}
          title={t("overview.cardErrorRate")}
          query={DASH_QUERIES.errorRate}
          unit={t("overview.unitPercent")}
          window={window}
        />

        <ActiveAlertsCard />
        <LogVolumeCard window={window} />
        <IssuesCard />
      </div>
    </PageContainer>
  );
}

interface Window {
  start: number;
  end: number;
  step: number;
}

function MetricPanel({
  icon,
  title,
  query,
  unit,
  window,
}: {
  icon: React.ReactNode;
  title: string;
  query: string;
  unit: string;
  window: Window;
}) {
  const { t } = useTranslation();
  const result = useQuery<TimeSeries[]>({
    queryKey: ["overview", query, window.end],
    queryFn: ({ signal }) =>
      queryRange({ query, start: window.start, end: window.end, step: window.step }, signal),
  });

  const value = lastValue(result.data);

  return (
    <Card>
      <CardHeader className="p-4 pb-0">
        <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <span className="text-foreground/70">{icon}</span>
          {title}
        </CardTitle>
        <div className="flex items-baseline gap-1.5">
          {result.isLoading ? (
            <Skeleton className="h-8 w-24" />
          ) : result.isError ? (
            <span className="text-sm text-destructive">{t("overview.errorTitle")}</span>
          ) : (
            <>
              <span className="text-2xl font-semibold tabular-nums">
                {value != null ? value.toFixed(1) : "—"}
              </span>
              <span className="text-xs text-muted-foreground">{unit}</span>
            </>
          )}
        </div>
      </CardHeader>
      <CardContent className="p-2 pt-1">
        {result.isLoading ? (
          <Skeleton className="h-[120px] w-full" />
        ) : result.data && result.data.length > 0 ? (
          <TimeSeriesChart series={result.data.slice(0, 1)} height={120} />
        ) : (
          <div className="h-[120px]" />
        )}
      </CardContent>
    </Card>
  );
}

function ActiveAlertsCard() {
  const { t } = useTranslation();
  const alerts = useQuery({
    queryKey: ["overview", "alerts"],
    queryFn: ({ signal }) => listAlerts("firing", signal),
    refetchInterval: 30_000,
  });

  const firing = alerts.data ?? [];
  const critical = firing.filter((e) => eventSeverity(e) === "critical").length;
  const warning = firing.filter((e) => eventSeverity(e) === "warning").length;

  return (
    <Card>
      <CardHeader className="p-4 pb-0">
        <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <span className="text-foreground/70">
            <BellRing className="size-4" />
          </span>
          {t("overview.cardActiveAlerts")}
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3 p-4 pt-2">
        {alerts.isLoading ? (
          <Skeleton className="h-8 w-16" />
        ) : (
          <>
            <div className="flex items-baseline gap-2">
              <span className="text-3xl font-semibold tabular-nums">{firing.length}</span>
              {firing.length === 0 && (
                <span className="text-sm text-muted-foreground">
                  {t("overview.activeAlertsHealthy")}
                </span>
              )}
            </div>
            {firing.length > 0 && (
              <div className="flex flex-wrap gap-2 text-xs">
                {critical > 0 && (
                  <span className="rounded border border-level-error/30 bg-level-error/10 px-1.5 py-0.5 text-level-error">
                    {t("overview.bySeverityCritical", { count: critical })}
                  </span>
                )}
                {warning > 0 && (
                  <span className="rounded border border-level-warn/30 bg-level-warn/10 px-1.5 py-0.5 text-level-warn">
                    {t("overview.bySeverityWarning", { count: warning })}
                  </span>
                )}
              </div>
            )}
          </>
        )}
        <Button asChild variant="outline" size="sm" className="mt-auto w-fit gap-1.5">
          <Link to="/alerts">
            <Activity className="size-3.5" />
            {t("overview.viewAll")}
          </Link>
        </Button>
      </CardContent>
    </Card>
  );
}

function LogVolumeCard({ window }: { window: Window }) {
  const { t } = useTranslation();
  const hits = useQuery<LogsHistogram>({
    queryKey: ["overview", "logs-hits", window.end],
    queryFn: ({ signal }) =>
      logsHits({ query: "*", start: window.start, end: window.end, step: 60 }, signal),
  });

  return (
    <Card className="md:col-span-2">
      <CardHeader className="p-4 pb-0">
        <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <span className="text-foreground/70">
            <ScrollText className="size-4" />
          </span>
          {t("overview.cardLogVolume")}
        </CardTitle>
        <p className="text-xs text-muted-foreground">{t("overview.logVolumeSubtitle")}</p>
      </CardHeader>
      <CardContent className="p-2 pt-1">
        {hits.isLoading ? (
          <Skeleton className="h-[160px] w-full" />
        ) : hits.data && hits.data.timestamps.length > 0 ? (
          <LogsHistogramChart data={hits.data} height={160} />
        ) : (
          <div className="flex h-[160px] items-center justify-center text-sm text-muted-foreground">
            {t("common.empty")}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function IssuesCard() {
  const { t, i18n } = useTranslation();
  const query = useQuery<IssueListResult>({
    queryKey: ["overview", "issues", RECENT_ISSUES_PARAMS],
    queryFn: ({ signal }) => listIssues(RECENT_ISSUES_PARAMS, signal),
    retry: false,
    refetchInterval: 30_000,
  });
  const view = recentIssuesView(query);

  return (
    <Card className="flex flex-col">
      <CardHeader className="p-4 pb-0">
        <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
          <span className="text-foreground/70">
            <Bug className="size-4" />
          </span>
          {t("overview.cardIssues")}
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-1 flex-col p-4 pt-2">
        {view.status === "pending" ? (
          <div className="space-y-2" role="status" aria-label={t("overview.loading")}>
            {[0, 1, 2].map((index) => (
              <Skeleton key={index} className="h-12 w-full" />
            ))}
          </div>
        ) : view.status === "error" ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 py-5 text-center" role="alert">
            <AlertCircle className="size-6 text-destructive" />
            <div className="text-sm font-medium">{t("overview.errorTitle")}</div>
            {view.message && (
              <p className="max-w-[220px] text-xs text-muted-foreground">{view.message}</p>
            )}
            <Button variant="outline" size="sm" onClick={() => query.refetch()}>
              {t("common.retry")}
            </Button>
          </div>
        ) : view.status === "empty" ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 py-5 text-center">
            <div className="flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground">
              <Bug className="size-5" />
            </div>
            <div className="text-sm font-medium">{t("overview.issuesEmpty")}</div>
            <p className="max-w-[220px] text-xs text-muted-foreground">
              {t("overview.issuesEmptyDesc")}
            </p>
          </div>
        ) : (
          <ul className="divide-y divide-border/60">
            {view.issues.map((issue) => (
              <li key={issue.id}>
                <Link
                  to="/issues/$issueId"
                  params={{ issueId: issue.id }}
                  className="block py-2.5 first:pt-0 hover:text-primary"
                >
                  <div className="flex min-w-0 items-center gap-2">
                    <IssueLevelBadge level={issue.level} className="shrink-0" />
                    <span className="truncate text-sm font-medium">{issue.title}</span>
                  </div>
                  <div className="mt-1 flex items-center justify-between gap-3 text-xs text-muted-foreground">
                    <span>{formatRelativeTime(issue.last_seen, i18n.language)}</span>
                    <span>{t("issues.events", { count: issue.count })}</span>
                  </div>
                </Link>
              </li>
            ))}
          </ul>
        )}

        {view.status !== "pending" && view.status !== "error" && (
          <Button asChild variant="outline" size="sm" className="mt-3 w-fit gap-1.5">
            <Link to="/issues">
              <Bug className="size-3.5" />
              {t("overview.viewAll")}
            </Link>
          </Button>
        )}
      </CardContent>
    </Card>
  );
}
