import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { type UseQueryResult } from "@tanstack/react-query";
import { useSearch } from "@tanstack/react-router";
import {
  AlertCircle,
  Database,
  Flame,
  Info,
  Layers,
  Loader2,
  RefreshCw,
  Scissors,
  Search,
} from "lucide-react";
import {
  countNodes,
  getFlamegraph,
  getProfileServices,
  hasTruncated,
  listProfiles,
  PROFILE_TYPES,
  type FlameNode,
  type ProfileRecord,
  type ProfileType,
} from "@/lib/api/profiles";
import { RANGE_PRESETS, DEFAULT_RANGE_ID } from "@/lib/api/ranges";
import { countMatches } from "@/lib/flamegraph";
import { IS_MOCK } from "@/lib/settings";
import { ApiError } from "@/lib/api/client";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { useResetOnProjectChange } from "@/hooks/use-reset-on-project-change";
import { CUSTOM_TIME_RANGE_ID, useTimeWindow } from "@/hooks/use-time-window";
import { formatDateTime } from "@/lib/format";
import { PageActions, PageContainer } from "@/components/layout/page";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Flamegraph } from "@/components/profiling/flamegraph";
import { ProfileListDrawer } from "@/components/profiling/profile-list-drawer";
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
import { cn } from "@/lib/utils";
import type { ProfilingEvidenceSearch } from "./trace-evidence-links";
import { queryViewState } from "./query-view-state";

const CUSTOM_RANGE = CUSTOM_TIME_RANGE_ID;

function isProfileType(v: string | undefined): v is ProfileType {
  return v === "cpu" || v === "heap" || v === "goroutine";
}

export function ProfilingPage() {
  const { t } = useTranslation();
  const search = useSearch({ strict: false }) as ProfilingEvidenceSearch;
  const hasCustomWindow = search.start !== undefined && search.end !== undefined;

  // Prefill from the URL once (cross-page link from a trace passes service +
  // start/end; the metrics-style range prefill is also honored).
  const [selectedService, setSelectedService] = useState(search.service ?? "");
  const [type, setType] = useState<ProfileType>(
    isProfileType(search.type) ? search.type : "cpu",
  );
  const [customWindow] = useState<{ start: number; end: number } | null>(
    hasCustomWindow ? { start: search.start!, end: search.end! } : null,
  );
  const { rangeId, setRangeId, window, advance } = useTimeWindow(
    hasCustomWindow ? CUSTOM_RANGE : search.range ?? DEFAULT_RANGE_ID,
    { customWindow },
  );
  const [query, setQuery] = useState("");
  const [drawerOpen, setDrawerOpen] = useState(false);

  // Reset service selection, drawer, and search when switching projects so the
  // prior project's service name never leaks into the newly selected project.
  const currentSlug = useResetOnProjectChange(() => {
    setSelectedService("");
    setDrawerOpen(false);
    setQuery("");
  });

  const servicesQuery = useProjectQuery<string[], ApiError>({
    queryKey: ["profile-services"],
    queryFn: ({ signal, project }) => getProfileServices(signal, project),
    retry: false,
  });

  // Default to the first service once the list loads (unless prefilled).
  const service = selectedService || servicesQuery.data?.[0] || "";

  // The dropdown always includes the active service, even if the services API
  // doesn't list it (e.g. arriving from a trace for an unprofiled-looking svc).
  const serviceOptions = useMemo(() => {
    const set = new Set(servicesQuery.data ?? []);
    if (service) set.add(service);
    return [...set];
  }, [servicesQuery.data, service]);

  const flameQuery = useProjectQuery<FlameNode | null, ApiError>({
    queryKey: ["flamegraph", service, type, window.start, window.end],
    enabled: !!service,
    retry: false,
    queryFn: ({ signal, project }) =>
      getFlamegraph({ service, type, start: window.start, end: window.end }, signal, project),
  });

  const profilesQuery = useProjectQuery<ProfileRecord[], ApiError>({
    queryKey: ["profiles-list", service, type, window.start, window.end],
    enabled: !!service && drawerOpen,
    retry: false,
    queryFn: ({ signal, project }) =>
      listProfiles({ service, type, start: window.start, end: window.end }, signal, project),
  });

  const profilesState = queryViewState({
    hasData: profilesQuery.data !== undefined,
    isLoading: profilesQuery.isLoading || (profilesQuery.isFetching && profilesQuery.data === undefined),
    isError: profilesQuery.isError,
    isEmpty: (profilesQuery.data?.length ?? 0) === 0,
  });

  const tree = flameQuery.data;
  const matchCount = useMemo(
    () => (tree && query.trim() ? countMatches(tree, query) : 0),
    [tree, query],
  );

  function refresh() {
    advance();
  }

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("profiling.mockBadge")}
          </Badge>
        </PageActions>
      )}

      {search.evidence === "contextual" && (
        <div
          className="mb-4 flex items-start gap-2 rounded-md border border-border bg-muted/40 p-3 text-sm"
          role="note"
        >
          <Info className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <div>
            <div className="font-medium">{t("traces.contextualEvidence")}</div>
            <p className="text-xs text-muted-foreground">{t("traces.contextualEvidenceHint")}</p>
          </div>
        </div>
      )}

      {/* Controls */}
      <Card className="mb-4">
        <CardContent className="grid grid-cols-1 gap-4 p-4 sm:grid-cols-2 lg:grid-cols-12 lg:items-end">
          <div className="space-y-1.5 lg:col-span-3">
            <Label>{t("profiling.service")}</Label>
            <Select value={service} onValueChange={setSelectedService}>
              <SelectTrigger>
                <SelectValue placeholder={t("profiling.selectService")} />
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

          <div className="space-y-1.5 lg:col-span-4">
            <Label>{t("profiling.type")}</Label>
            <div className="flex rounded-md border border-border p-0.5">
              {PROFILE_TYPES.map((pt) => (
                <button
                  key={pt}
                  type="button"
                  onClick={() => setType(pt)}
                  className={cn(
                    "flex-1 rounded-[5px] px-2 py-1 text-xs font-medium capitalize transition-colors",
                    type === pt
                      ? "bg-primary text-primary-foreground"
                      : "text-muted-foreground hover:bg-accent hover:text-foreground",
                  )}
                >
                  {t(`profiling.type_${pt}`)}
                </button>
              ))}
            </div>
          </div>

          <div className="space-y-1.5 lg:col-span-3">
            <Label>{t("profiling.timeRange")}</Label>
            <Select value={rangeId} onValueChange={setRangeId}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {customWindow && (
                  <SelectItem value={CUSTOM_RANGE}>
                    {t("profiling.rangeCustom")}
                  </SelectItem>
                )}
                {RANGE_PRESETS.map((r) => (
                  <SelectItem key={r.id} value={r.id}>
                    {t(r.labelKey)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="flex items-end gap-2 lg:col-span-2">
            <Button
              variant="outline"
              className="flex-1"
              onClick={() => setDrawerOpen(true)}
              disabled={!service}
            >
              <Layers className="size-4" />
              <span className="optical-center hidden sm:inline">{t("profiling.profileList")}</span>
            </Button>
            <Button variant="ghost" size="icon" onClick={refresh} aria-label={t("common.refresh")}>
              <RefreshCw className={cn("size-4", flameQuery.isFetching && "animate-spin")} />
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Search + window meta */}
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <div className="relative min-w-[220px] flex-1 sm:max-w-sm">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={t("profiling.searchPlaceholder")}
            className="pl-8 font-mono text-sm"
            spellCheck={false}
          />
        </div>
        {query.trim() && (
          <Badge variant={matchCount > 0 ? "secondary" : "outline"}>
            {t("profiling.matchCount", { count: matchCount })}
          </Badge>
        )}
        <span className="ml-auto text-xs text-muted-foreground tabular-nums">
          {formatDateTime(window.start * 1000)} — {formatDateTime(window.end * 1000)}
        </span>
      </div>

      <FlamegraphSection query={query} flameQuery={flameQuery} onRetry={refresh} />

      <ProfileListDrawer
        open={drawerOpen}
        onOpenChange={setDrawerOpen}
        projectSlug={currentSlug}
        records={profilesQuery.data ?? []}
        isLoading={profilesState === "pending"}
        hasError={profilesState === "error"}
        stale={profilesState === "stale"}
        isRetrying={profilesQuery.isFetching}
        onRetry={() => void profilesQuery.refetch()}
      />
    </PageContainer>
  );
}

function FlamegraphSection({
  query,
  flameQuery,
  onRetry,
}: {
  query: string;
  flameQuery: UseQueryResult<FlameNode | null, ApiError>;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  const tree = flameQuery.data;
  const viewState = queryViewState({
    hasData: tree !== undefined,
    isLoading: flameQuery.isLoading,
    isError: flameQuery.isError,
    isEmpty: !tree || tree.value <= 0 || !tree.children || tree.children.length === 0,
  });

  if (viewState === "pending") {
    return (
      <Card>
        <CardContent className="space-y-2 p-4">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            {t("profiling.loading")}
          </div>
          <Skeleton className="h-64 w-full" />
        </CardContent>
      </Card>
    );
  }

  if (viewState === "error") {
    return (
      <ProfilingEmpty
        icon={<AlertCircle className="size-6 text-destructive" />}
        title={t("profiling.errorTitle")}
        desc={t("common.queryErrorDesc")}
        action={
          <Button variant="outline" size="sm" onClick={onRetry}>
            {t("common.retry")}
          </Button>
        }
      />
    );
  }

  if (viewState === "empty" || !tree) {
    return (
      <ProfilingEmpty
        icon={<Flame className="size-6" />}
        title={t("profiling.emptyTitle")}
        desc={t("profiling.emptyDesc")}
      />
    );
  }

  const truncated = hasTruncated(tree);

  return (
    <div className="space-y-3">
      {viewState === "stale" && (
        <QueryStaleNotice isRetrying={flameQuery.isFetching} onRetry={onRetry} />
      )}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        <span>{t("profiling.totalSamples", { count: tree.value })}</span>
        <span>{t("profiling.nodeCount", { count: countNodes(tree) })}</span>
      </div>

      {truncated && (
        <div className="flex items-center gap-2 rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          <Scissors className="size-3.5 shrink-0" />
          {t("profiling.truncated")}
        </div>
      )}

      <Flamegraph tree={tree} total={tree.value} query={query} />
    </div>
  );
}

function ProfilingEmpty({
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
