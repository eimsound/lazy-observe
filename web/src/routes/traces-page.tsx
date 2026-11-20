import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import {
  AlertCircle,
  Database,
  Loader2,
  Search,
  Waypoints,
} from "lucide-react";
import {
  getServices,
  getOperations,
  searchTraces,
  summarizeTrace,
  type JaegerTrace,
  type TraceSearchParams,
  type TraceSummary,
} from "@/lib/api/traces";
import dayjs from "@/lib/dayjs";
import {
  RANGE_PRESETS,
  DEFAULT_RANGE_ID,
  rangeById,
  resolveRangeMicros,
} from "@/lib/api/ranges";
import { IS_MOCK } from "@/lib/settings";
import { useProject } from "@/lib/project/project-provider";
import { ApiError } from "@/lib/api/client";
import { formatDateTime, formatDurationMicros } from "@/lib/format";
import { PageActions, PageContainer } from "@/components/layout/page";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  durationMicros,
  parseTraceSearchURL,
  traceSearchFromURL,
  traceSearchToURL,
  type TraceSearchURL,
} from "./traces-search";
import { queryViewState } from "./query-view-state";

const LIMIT_OPTIONS = [10, 20, 40];
const ALL_OPERATIONS = "__all__";

export function TracesPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { currentSlug: project } = useProject();
  const previousProject = useRef(project);
  const routeSearch = useSearch({ strict: false }) as TraceSearchURL;
  const {
    service: urlService, operation: urlOperation, start: urlStart, end: urlEnd,
    minDuration: urlMinDuration, limit: urlLimit, tags: urlTags, run: urlRun,
  } = routeSearch;
  // Keyed by the URL fields' values, not the search object's identity.
  const urlSearch = useMemo(
    () => traceSearchFromURL(parseTraceSearchURL({
      service: urlService, operation: urlOperation, start: urlStart, end: urlEnd,
      minDuration: urlMinDuration, limit: urlLimit, tags: urlTags, run: urlRun,
    })),
    [urlService, urlOperation, urlStart, urlEnd, urlMinDuration, urlLimit, urlTags, urlRun],
  );
  const [initialSearch] = useState(urlSearch);

  const [service, setService] = useState(initialSearch?.service ?? "");
  const [operation, setOperation] = useState(initialSearch?.operation ?? ALL_OPERATIONS);
  const [rangeId, setRangeId] = useState(DEFAULT_RANGE_ID);
  const [minDuration, setMinDuration] = useState(initialSearch?.minDuration ?? "");
  const [limit, setLimit] = useState(initialSearch?.limit ?? 20);
  const [tags, setTags] = useState(initialSearch?.tags ?? "");
  const [executed, setExecuted] = useState<TraceSearchParams | null>(initialSearch);

  useEffect(() => {
    if (previousProject.current === project) return;
    previousProject.current = project;
    setService("");
    setOperation(ALL_OPERATIONS);
    setMinDuration("");
    setTags("");
    setExecuted(null);
    void navigate({ to: "/traces", search: {}, replace: true });
  }, [project, navigate]);

  useEffect(() => {
    setExecuted(urlSearch);
    if (!urlSearch) return;
    setService(urlSearch.service);
    setOperation(urlSearch.operation ?? ALL_OPERATIONS);
    setMinDuration(urlSearch.minDuration ?? "");
    setLimit(urlSearch.limit);
    setTags(urlSearch.tags ?? "");
  }, [urlSearch]);

  const servicesQuery = useQuery<string[], ApiError>({
    queryKey: ["trace-services", project],
    queryFn: ({ signal }) => getServices(signal, project),
    retry: false,
  });

  // Default to the first service once the list loads.
  useEffect(() => {
    if (!service && servicesQuery.data && servicesQuery.data.length > 0) {
      setService(servicesQuery.data[0]);
    }
  }, [service, servicesQuery.data]);

  const operationsQuery = useQuery<string[], ApiError>({
    queryKey: ["trace-operations", project, service],
    enabled: !!service,
    queryFn: ({ signal }) => getOperations(service, signal, project),
    retry: false,
  });
  const serviceOptions = useMemo(
    () => Array.from(new Set(service ? [service, ...(servicesQuery.data ?? [])] : servicesQuery.data ?? [])),
    [service, servicesQuery.data],
  );
  const operationOptions = useMemo(
    () => Array.from(
      new Set(
        operation !== ALL_OPERATIONS
          ? [operation, ...(operationsQuery.data ?? [])]
          : operationsQuery.data ?? [],
      ),
    ),
    [operation, operationsQuery.data],
  );

  function execute() {
    if (!service) return;
    const parsedMinDuration = minDuration.trim() ? durationMicros(minDuration) : null;
    if (minDuration.trim() && parsedMinDuration === null) return;
    const { start, end } = resolveRangeMicros(rangeById(rangeId), dayjs().valueOf());
    const params: TraceSearchParams = {
      service,
      operation: operation === ALL_OPERATIONS ? undefined : operation,
      start,
      end,
      minDuration: parsedMinDuration === null ? undefined : `${parsedMinDuration}us`,
      limit,
      tags: tags.trim() || undefined,
    };
    setExecuted(params);
    void navigate({ to: "/traces", search: traceSearchToURL(params) });
  }

  const minDurationValid = !minDuration.trim() || durationMicros(minDuration) !== null;

  const searchQuery = useQuery<JaegerTrace[], ApiError>({
    queryKey: [
      "trace-search",
      project,
      executed?.service,
      executed?.operation,
      executed?.start,
      executed?.end,
      executed?.minDuration,
      executed?.limit,
      executed?.tags,
    ],
    enabled: executed !== null,
    retry: false,
    placeholderData: (previous) => previous,
    queryFn: ({ signal }) => searchTraces(executed!, signal, project),
  });

  const summaries = useMemo<TraceSummary[]>(
    () => (searchQuery.data ?? []).map(summarizeTrace),
    [searchQuery.data],
  );
  const maxDuration = useMemo(
    () => summaries.reduce((m, s) => Math.max(m, s.duration), 1),
    [summaries],
  );

  function openTrace(traceID: string) {
    void navigate({ to: "/traces/$traceID", params: { traceID } });
  }

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("traces.mockBadge")}
          </Badge>
        </PageActions>
      )}

      {/* Search bar */}
      <Card className="mb-4">
        <CardContent className="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 lg:grid-cols-12 lg:items-end">
          <div className="space-y-1.5 lg:col-span-2">
            <Label>{t("traces.service")}</Label>
            <Select
              value={service}
              onValueChange={(v) => {
                setService(v);
                setOperation(ALL_OPERATIONS);
              }}
            >
              <SelectTrigger>
                <SelectValue placeholder={t("traces.selectService")} />
              </SelectTrigger>
              <SelectContent>
                {serviceOptions.map((s) => (
                  <SelectItem key={s} value={s}>
                    {s}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5 lg:col-span-3">
            <Label>{t("traces.operation")}</Label>
            <Select value={operation} onValueChange={setOperation} disabled={!service}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL_OPERATIONS}>{t("traces.allOperations")}</SelectItem>
                {operationOptions.map((op) => (
                  <SelectItem key={op} value={op}>
                    {op}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5 lg:col-span-2">
            <Label>{t("traces.timeRange")}</Label>
            <Select value={rangeId} onValueChange={setRangeId}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {RANGE_PRESETS.map((r) => (
                  <SelectItem key={r.id} value={r.id}>
                    {t(r.labelKey)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5 lg:col-span-2">
            <Label htmlFor="trace-min-duration">{t("traces.minDuration")}</Label>
            <Input
              id="trace-min-duration"
              value={minDuration}
              onChange={(event) => setMinDuration(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter") execute();
              }}
              placeholder={t("traces.minDurationPlaceholder")}
              className="font-mono text-sm"
              aria-invalid={!minDurationValid}
              spellCheck={false}
            />
          </div>

          <div className="space-y-1.5 lg:col-span-1">
            <Label>{t("traces.limit")}</Label>
            <Select value={String(limit)} onValueChange={(v) => setLimit(Number(v))}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {LIMIT_OPTIONS.map((n) => (
                  <SelectItem key={n} value={String(n)}>
                    {n}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5 sm:col-span-2 lg:col-span-2">
            <Label htmlFor="trace-tags">{t("traces.tags")}</Label>
            <div className="flex gap-2">
              <Input
                id="trace-tags"
                value={tags}
                onChange={(e) => setTags(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") execute();
                }}
                placeholder={t("traces.tagsPlaceholder")}
                className="font-mono text-sm"
                spellCheck={false}
              />
              <Button
                onClick={execute}
                disabled={!service || !minDurationValid || searchQuery.isFetching}
              >
                {searchQuery.isFetching ? (
                  <Loader2 className="size-4 animate-spin" />
                ) : (
                  <Search className="size-4" />
                )}
                <span className="hidden sm:inline">{t("traces.search")}</span>
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>

      <TraceSearchResults
        executed={executed}
        query={searchQuery}
        summaries={summaries}
        maxDuration={maxDuration}
        onRetry={() => void searchQuery.refetch()}
        onOpen={openTrace}
      />
    </PageContainer>
  );
}

function TraceSearchResults({
  executed,
  query,
  summaries,
  maxDuration,
  onRetry,
  onOpen,
}: {
  executed: TraceSearchParams | null;
  query: ReturnType<typeof useQuery<JaegerTrace[], ApiError>>;
  summaries: TraceSummary[];
  maxDuration: number;
  onRetry: () => void;
  onOpen: (traceID: string) => void;
}) {
  const { t } = useTranslation();
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: summaries.length === 0,
  });

  if (!executed) {
    return (
      <TracesEmptyState
        icon={<Search className="size-6" />}
        title={t("traces.initialTitle")}
        desc={t("traces.initialDesc")}
      />
    );
  }

  if (viewState === "pending") {
    return (
      <Card>
        <CardContent className="space-y-2 p-4">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            {t("traces.loading")}
          </div>
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-16 w-full" />
          ))}
        </CardContent>
      </Card>
    );
  }

  if (viewState === "error") {
    return (
      <TracesEmptyState
        icon={<AlertCircle className="size-6 text-destructive" />}
        title={t("traces.errorTitle")}
        desc={t("common.queryErrorDesc")}
        action={
          <Button variant="outline" size="sm" onClick={onRetry}>
            {t("common.retry")}
          </Button>
        }
      />
    );
  }

  if (viewState === "empty") {
    return (
      <TracesEmptyState
        icon={<Database className="size-6" />}
        title={t("traces.emptyTitle")}
        desc={t("traces.emptyDesc")}
      />
    );
  }

  return (
    <div className="space-y-3">
      {viewState === "stale" && (
        <QueryStaleNotice isRetrying={query.isFetching} onRetry={onRetry} />
      )}
      <div className="text-xs text-muted-foreground">
        {t("traces.resultsCount", { count: summaries.length })}
      </div>
      <div className="space-y-2">
        {summaries.map((s) => (
          <TraceRow key={s.traceID} summary={s} maxDuration={maxDuration} onOpen={onOpen} />
        ))}
      </div>
    </div>
  );
}

function TraceRow({
  summary,
  maxDuration,
  onOpen,
}: {
  summary: TraceSummary;
  maxDuration: number;
  onOpen: (traceID: string) => void;
}) {
  const { t } = useTranslation();
  const widthPct = Math.max(4, (summary.duration / maxDuration) * 100);
  return (
    <Card
      role="button"
      tabIndex={0}
      onClick={() => onOpen(summary.traceID)}
      onKeyDown={(e) => {
        if (e.key === "Enter") onOpen(summary.traceID);
      }}
      className="cursor-pointer transition-colors hover:border-ring hover:bg-muted/30"
    >
      <CardContent className="flex items-center gap-4 p-3">
        <Waypoints className="size-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="truncate text-sm font-medium">{summary.rootName}</span>
            <span className="shrink-0 text-xs text-muted-foreground">{summary.rootService}</span>
            {summary.errorCount > 0 && (
              <Badge variant="destructive" className="shrink-0">
                {t("traces.errors", { count: summary.errorCount })}
              </Badge>
            )}
          </div>
          <div className="mt-1 flex items-center gap-3 text-xs text-muted-foreground">
            <span>{t("traces.spans", { count: summary.spanCount })}</span>
            <span>{t("traces.servicesCount", { count: summary.serviceCount })}</span>
            <span className="truncate font-mono">{summary.traceID.slice(0, 16)}</span>
          </div>
          {/* Duration micro-bar (relative to the longest trace in the page). */}
          <div className="mt-1.5 h-1 w-full overflow-hidden rounded-full bg-muted">
            <div
              className="h-full rounded-full"
              style={{
                width: `${widthPct}%`,
                backgroundColor: summary.errorCount > 0 ? "var(--level-error)" : "var(--chart-1)",
              }}
            />
          </div>
        </div>
        <div className="shrink-0 text-right">
          <div className="text-sm font-semibold tabular-nums">
            {formatDurationMicros(summary.duration)}
          </div>
          <div className="text-[11px] text-muted-foreground">
            {formatDateTime(summary.startTime / 1000)}
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

function TracesEmptyState({
  icon,
  title,
  desc,
  action,
}: {
  icon: React.ReactNode;
  title: string;
  desc: string;
  action?: React.ReactNode;
}) {
  return (
    <Card>
      <CardContent className="flex flex-col items-center justify-center gap-3 py-24 text-center">
        <div className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">
          {icon}
        </div>
        <div className="text-base font-medium">{title}</div>
        <p className="max-w-md break-words text-sm text-muted-foreground">{desc}</p>
        {action}
      </CardContent>
    </Card>
  );
}
