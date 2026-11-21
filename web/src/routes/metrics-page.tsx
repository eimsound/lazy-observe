import { useEffect, useMemo, useRef, useState } from "react";
import { useSearch } from "@tanstack/react-router";
import {
  AlertCircle,
  BarChart3,
  Code2,
  Copy,
  Database,
  History,
  Info,
  ListFilter,
  Loader2,
  Play,
  Plus,
  Search,
  Settings2,
  SlidersHorizontal,
  Table2,
  Trash2,
  WandSparkles,
  X,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  prettifyPromQL,
  queryInstantDetailed,
  queryRangeDetailed,
  queryServerClock,
} from "@/lib/api/metrics";
import { ApiError } from "@/lib/api/client";
import type {
  MetricTableResult,
  MetricsQueryEnvelope,
  TimeSeries,
} from "@/lib/api/types";
import { IS_MOCK } from "@/lib/settings";
import { useProject } from "@/lib/project/project-provider";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { useRouterLocationSync } from "@/lib/router-url-sync";
import dayjs, { dayjsLocale } from "@/lib/dayjs";
import {
  clearMetricsHistory,
  dateParts,
  effectiveMetricsStep,
  getMetricsHistory,
  metricsLabelColumns,
  pushMetricsHistory,
  resolveMetricsRange,
  type MetricsHistoryEntry,
  type MetricsQueryTab,
  type MetricsRange,
  type MetricsTableMode,
  type MetricsTimeFormat,
} from "@/lib/metrics-query";
import { PageActions, PageContainer } from "@/components/layout/page";
import { TimeSeriesChart } from "@/components/charts/time-series-chart";
import { MetricsExplorer } from "@/components/metrics/metrics-explorer";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { PrometheusQueryControls } from "@/components/metrics/prometheus-query-controls";
import { PromQLBuilder } from "@/components/metrics/promql-builder";
import { PromQLEditor, type PromQLEditorHandle } from "@/components/metrics/promql-editor";
import { MetricsTimeRangePicker } from "@/components/metrics/metrics-time-range-picker";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";
import type { MetricsEvidenceSearch } from "./trace-evidence-links";
import {
  buildMetricsPanelsURL,
  newMetricsPanel,
  parseMetricsSearch,
  parseMetricsStateFromLocation,
  type ExecutedMetricsQuery,
  type InitialMetricsState,
  type MetricsPanelState,
} from "./metrics-search";
import { queryViewState } from "./query-view-state";

type QueryPanelState = MetricsPanelState;
type ExecutedQuery = ExecutedMetricsQuery;

const LOCAL_TIME_KEY = "lo-metrics-use-local-time";

function initialLocalTime() {
  try {
    const stored = localStorage.getItem(LOCAL_TIME_KEY);
    return stored === null ? true : stored !== "false";
  } catch {
    return true;
  }
}

function createInitialMetricsState(
  routeLocation: string,
  search: MetricsEvidenceSearch,
): { state: InitialMetricsState; initialDeepLinkKey: string | null; initialDeepLinkQuery: string | null } {
  const parsed = parseMetricsStateFromLocation(routeLocation, search);
  if (!search.q) {
    return { state: parsed, initialDeepLinkKey: null, initialDeepLinkQuery: null };
  }
  const key = JSON.stringify([search.q, search.run ?? "", search.start ?? null, search.end ?? null]);
  const trimmed = search.q.trim();
  const target = parsed.panels[0] ?? newMetricsPanel("query-1");
  const evidenceRange: MetricsRange | undefined =
    search.start !== undefined && search.end !== undefined
      ? { mode: "absolute", startMs: search.start * 1_000, endMs: search.end * 1_000 }
      : undefined;
  const nextRange = evidenceRange ?? target.range;
  const updated: QueryPanelState = { ...target, query: search.q, range: nextRange };
  const panels = parsed.panels.length > 0 ? [updated, ...parsed.panels.slice(1)] : [updated];
  const executed = { ...parsed.executed };
  if (search.run === "1" && trimmed) {
    executed[updated.id] = {
      query: trimmed,
      runId: panels.length + 1,
      range: resolveMetricsRange(nextRange),
    };
  }
  return {
    state: { panels, executed },
    initialDeepLinkKey: key,
    initialDeepLinkQuery: search.run === "1" && trimmed ? trimmed : null,
  };
}

export function MetricsPage() {
  const { t } = useTranslation();
  const {
    routeLocation,
    currentHistoryHref,
    replaceLocation,
    subscribeExternalLocation,
  } = useRouterLocationSync();
  const search = useSearch({ strict: false }) as MetricsEvidenceSearch;
  const { currentSlug: project } = useProject();
  const [initial] = useState(() => createInitialMetricsState(routeLocation, search));
  const [panels, setPanels] = useState<QueryPanelState[]>(initial.state.panels);
  const panelsRef = useRef(panels);
  const nextPanelId = useRef(initial.state.panels.length + 1);
  const nextRunId = useRef(initial.state.panels.length + 2);
  const projectRef = useRef(project);
  const [localTime, setLocalTime] = useState(initialLocalTime);
  const [executed, setExecuted] = useState<Record<string, ExecutedQuery>>(initial.state.executed);
  const [history, setHistory] = useState<MetricsHistoryEntry[]>(() => (
    initial.initialDeepLinkQuery
      ? pushMetricsHistory(project, [initial.initialDeepLinkQuery])
      : getMetricsHistory(project)
  ));
  const [prettifying, setPrettifying] = useState<Record<string, boolean>>({});
  const [editorErrors, setEditorErrors] = useState<Record<string, string>>({});
  const [trackedProject, setTrackedProject] = useState(project);
  const previousProject = useRef(project);
  const lastDeepLinkKey = useRef<string | null>(initial.initialDeepLinkKey);

  if (trackedProject !== project) {
    setTrackedProject(project);
    setHistory(getMetricsHistory(project));
    setPanels([newMetricsPanel("query-1")]);
    setExecuted({});
    setPrettifying({});
    setEditorErrors({});
  }

  useEffect(() => {
    panelsRef.current = panels;
    projectRef.current = project;
  });

  useEffect(() => {
    if (previousProject.current !== project) {
      previousProject.current = project;
      nextPanelId.current = 2;
      nextRunId.current = 1;
      lastDeepLinkKey.current = null;
    }
  }, [project]);

  useEffect(() => {
    const target = buildMetricsPanelsURL(currentHistoryHref(), panels);
    replaceLocation(target);
  }, [panels, currentHistoryHref, replaceLocation]);

  // Resync panels and deep links only on genuine external navigations (browser
  // Back/Forward or same-page deep links like "Open in Metrics" from AI), never
  // on URL writes authored by this page itself.
  useEffect(() => subscribeExternalLocation((nextLocation) => {
    const params = new URL(nextLocation, "https://lazy-observe.invalid").searchParams;
    const nextSearch = parseMetricsSearch(Object.fromEntries(params.entries()));
    if (nextSearch.q) {
      const key = JSON.stringify([
        nextSearch.q,
        nextSearch.run ?? "",
        nextSearch.start ?? null,
        nextSearch.end ?? null,
      ]);
      if (lastDeepLinkKey.current === key) return;
      lastDeepLinkKey.current = key;

      const trimmed = nextSearch.q.trim();
      const evidenceRange: MetricsRange | undefined =
        nextSearch.start !== undefined && nextSearch.end !== undefined
          ? {
            mode: "absolute",
            startMs: nextSearch.start * 1_000,
            endMs: nextSearch.end * 1_000,
          }
          : undefined;

      const currentPanels = panelsRef.current;
      const target = currentPanels[0] ?? newMetricsPanel("query-1");
      const nextRange = evidenceRange ?? target.range;
      const updated: QueryPanelState = {
        ...target,
        query: nextSearch.q,
        range: nextRange,
      };
      setPanels(currentPanels.length > 0 ? [updated, ...currentPanels.slice(1)] : [updated]);
      if (nextSearch.run === "1" && trimmed) {
        const frozenRange = resolveMetricsRange(nextRange);
        setExecuted((prev) => ({
          ...prev,
          [updated.id]: {
            query: trimmed,
            runId: nextRunId.current++,
            range: frozenRange,
          },
        }));
        setHistory(pushMetricsHistory(projectRef.current, [trimmed]));
      }
      return;
    }
    lastDeepLinkKey.current = null;
    const next = parseMetricsStateFromLocation(nextLocation, nextSearch);
    setPanels(next.panels);
    setExecuted(next.executed);
    nextPanelId.current = Math.max(nextPanelId.current, next.panels.length + 1);
    nextRunId.current = Math.max(nextRunId.current, next.panels.length + 1);
  }), [subscribeExternalLocation]);

  useEffect(() => {
    try {
      localStorage.setItem(LOCAL_TIME_KEY, String(localTime));
    } catch {
      // Keep the in-memory preference when browser storage is unavailable.
    }
  }, [localTime]);

  function updatePanel(id: string, patch: Partial<QueryPanelState>) {
    setPanels((current) => current.map((panel) => panel.id === id ? { ...panel, ...patch } : panel));
    if (patch.range) {
      const nextRange = resolveMetricsRange(patch.range);
      setExecuted((current) => (
        current[id]
          ? { ...current, [id]: { ...current[id], range: nextRange } }
          : current
      ));
    }
  }

  function execute(id: string) {
    const panel = panels.find((item) => item.id === id);
    if (!panel?.query.trim()) return;
    const trimmed = panel.query.trim();
    setExecuted((current) => ({
      ...current,
      [id]: {
        query: trimmed,
        runId: nextRunId.current++,
        range: resolveMetricsRange(panel.range),
      },
    }));
    setHistory(pushMetricsHistory(project, [trimmed]));
  }

  function addPanel(afterId?: string, duplicate?: QueryPanelState) {
    const id = `query-${nextPanelId.current++}`;
    const panel = duplicate ? { ...duplicate, id } : newMetricsPanel(id);
    setPanels((current) => {
      if (!afterId) return [...current, panel];
      const index = current.findIndex((item) => item.id === afterId);
      return index < 0 ? [...current, panel] : [...current.slice(0, index + 1), panel, ...current.slice(index + 1)];
    });
  }

  function removePanel(id: string) {
    if (panels.length === 1) return;
    setPanels((current) => current.filter((panel) => panel.id !== id));
    setExecuted((current) => {
      const next = { ...current };
      delete next[id];
      return next;
    });
  }

  async function prettify(id: string) {
    const panel = panels.find((item) => item.id === id);
    if (!panel?.query.trim()) return;
    const requestProject = project;
    setPrettifying((current) => ({ ...current, [id]: true }));
    setEditorErrors((current) => ({ ...current, [id]: "" }));
    try {
      const formatted = await prettifyPromQL(panel.query.trim(), undefined, requestProject);
      if (projectRef.current !== requestProject) return;
      updatePanel(id, { query: formatted });
    } catch (error) {
      if (projectRef.current !== requestProject) return;
      setEditorErrors((current) => ({
        ...current,
        [id]: error instanceof Error ? error.message : t("metrics.prettifyFailed"),
      }));
    } finally {
      if (projectRef.current === requestProject) {
        setPrettifying((current) => ({ ...current, [id]: false }));
      }
    }
  }

  const serverTime = useProjectQuery({
    queryKey: ["metrics-server-time"],
    enabled: !IS_MOCK,
    staleTime: 30_000,
    refetchInterval: 60_000,
    retry: false,
    queryFn: ({ signal, project: activeProject }) => queryServerClock(signal, activeProject),
  });
  const driftSeconds = serverTime.data?.driftSeconds ?? 0;

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5"><Database className="size-3" />{t("metrics.mockBadge")}</Badge>
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

      {driftSeconds > 30 && (
        <div className="mb-4 flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm">
          <AlertCircle className="mt-0.5 size-4 shrink-0 text-destructive" />
          <div><div className="font-medium">{t("metrics.serverTimeDrift")}</div><p className="text-xs text-muted-foreground">{t("metrics.serverTimeDriftDesc", { seconds: Math.round(driftSeconds) })}</p></div>
        </div>
      )}

      <div className="space-y-6">
        {panels.map((panel) => (
          <QueryCard
            key={panel.id}
            project={project}
            panel={panel}
            executed={executed[panel.id]}
            history={history}
            localTime={localTime}
            canRemove={panels.length > 1}
            prettifying={prettifying[panel.id]}
            editorError={editorErrors[panel.id]}
            onChange={(patch) => updatePanel(panel.id, patch)}
            onExecute={() => execute(panel.id)}
            onRetry={() => setExecuted((current) => current[panel.id]
              ? {
                ...current,
                [panel.id]: {
                  ...current[panel.id],
                  runId: nextRunId.current++,
                  range: resolveMetricsRange(panel.range),
                },
              }
              : current)}
            onPrettify={() => void prettify(panel.id)}
            onDuplicate={() => addPanel(panel.id, panel)}
            onRemove={() => removePanel(panel.id)}
            onLocalTimeChange={setLocalTime}
            onClearHistory={() => setHistory(clearMetricsHistory(project))}
          />
        ))}
        <div className="flex justify-end">
          <Button variant="outline" onClick={() => addPanel()} disabled={panels.length >= 10}><Plus />{t("metrics.addPanel")}</Button>
        </div>
      </div>
    </PageContainer>
  );
}

function QueryCard({
  project,
  panel,
  executed,
  history,
  localTime,
  canRemove,
  prettifying,
  editorError,
  onChange,
  onExecute,
  onRetry,
  onPrettify,
  onDuplicate,
  onRemove,
  onLocalTimeChange,
  onClearHistory,
}: {
  project: string;
  panel: QueryPanelState;
  executed?: ExecutedQuery;
  history: MetricsHistoryEntry[];
  localTime: boolean;
  canRemove: boolean;
  prettifying?: boolean;
  editorError?: string;
  onChange: (patch: Partial<QueryPanelState>) => void;
  onExecute: () => void;
  onRetry: () => void;
  onPrettify: () => void;
  onDuplicate: () => void;
  onRemove: () => void;
  onLocalTimeChange: (enabled: boolean) => void;
  onClearHistory: () => void;
}) {
  const { t } = useTranslation();
  const editorRef = useRef<PromQLEditorHandle>(null);
  const [explorerOpen, setExplorerOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  // Resolve autocomplete/explorer range only when the range selection itself
  // changes, never on every keystroke in the PromQL editor.
  const autocompleteRange = useMemo(() => resolveMetricsRange(panel.range), [panel.range]);

  return (
    <Card className="overflow-visible">
      <CardContent className="flex flex-col gap-4 p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div
            className="inline-flex h-8 rounded-md border border-input bg-background p-0.5"
            role="group"
            aria-label={t("metrics.editorModeLabel")}
          >
            <Button
              type="button"
              variant={panel.editorMode === "builder" ? "secondary" : "ghost"}
              size="sm"
              className="h-full gap-1.5 px-2.5 text-xs"
              aria-pressed={panel.editorMode === "builder"}
              onClick={() => onChange({ editorMode: "builder" })}
            >
              <SlidersHorizontal className="size-3.5" />
              {t("metrics.modeBuilder")}
            </Button>
            <Button
              type="button"
              variant={panel.editorMode === "code" ? "secondary" : "ghost"}
              size="sm"
              className="h-full gap-1.5 px-2.5 text-xs"
              aria-pressed={panel.editorMode === "code"}
              onClick={() => onChange({ editorMode: "code" })}
            >
              <Code2 className="size-3.5" />
              {t("metrics.modeCode")}
            </Button>
          </div>
          {panel.editorMode === "builder" && (
            <span className="text-xs text-muted-foreground">
              {t("metrics.builder.liveSyncHint")}
            </span>
          )}
        </div>

        {panel.editorMode === "builder" && (
          <PromQLBuilder
            project={project}
            query={panel.query}
            range={autocompleteRange}
            onQueryChange={(query) => onChange({ query })}
            onSwitchToCode={() => onChange({ editorMode: "code" })}
          />
        )}

        <div className="flex items-start gap-2">
          <PromQLEditor
            ref={editorRef}
            id={`${panel.id}-promql`}
            value={panel.query}
            range={autocompleteRange}
            onChange={(query) => onChange({ query })}
            onRun={onExecute}
            actions={(
              <Button
                variant="ghost"
                size="icon"
                className="size-9"
                disabled={!panel.query.trim() || prettifying}
                aria-label={t("metrics.prettify")}
                title={t("metrics.prettify")}
                onClick={onPrettify}
              >
                {prettifying ? <Loader2 className="animate-spin" /> : <WandSparkles />}
              </Button>
            )}
          />
          <Button className="shrink-0" disabled={!panel.query.trim()} onClick={onExecute}><Play />{t("metrics.run")}</Button>
          <Button
            variant="outline"
            size="icon"
            className="shrink-0"
            disabled={!canRemove}
            aria-label={t("metrics.removePanel")}
            title={t("metrics.removePanel")}
            onClick={onRemove}
          >
            <X />
          </Button>
        </div>
        {editorError && <p className="text-xs text-destructive">{editorError}</p>}

        <Tabs
          className="flex flex-col gap-4"
          value={panel.activeTab}
          onValueChange={(value) => onChange({ activeTab: value as MetricsQueryTab })}
        >
          <div className="flex flex-wrap items-center justify-between gap-2">
            <TabsList className="h-9">
              <TabsTrigger value="table" className="gap-2"><Table2 className="size-4" />{t("metrics.table")}</TabsTrigger>
              <TabsTrigger value="graph" className="gap-2"><BarChart3 className="size-4" />{t("metrics.graph")}</TabsTrigger>
            </TabsList>
            <div className="ml-auto flex items-center gap-2">
              <Button
                variant="outline"
                size="icon"
                className="size-9"
                aria-label={t("metrics.duplicateQuery")}
                title={t("metrics.duplicateQuery")}
                onClick={onDuplicate}
              >
                <Copy />
              </Button>
              <Button variant="outline" size="sm" className="h-9" onClick={() => setHistoryOpen(true)}>
                <History />{t("metrics.history")}
              </Button>
              <Button variant="outline" size="sm" className="h-9" onClick={() => setExplorerOpen(true)}>
                <ListFilter />{t("metrics.exploreMetrics")}
              </Button>
            </div>
          </div>
          <TabsContent value="table" className="mt-0 flex flex-col gap-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <MetricsTimeRangePicker value={panel.range} localTime={localTime} onChange={(range) => onChange({ range })} onLocalTimeChange={onLocalTimeChange} />
              <MetricsTableSettings
                mode={panel.tableMode}
                timeFormat={panel.timeFormat}
                onModeChange={(tableMode) => onChange({ tableMode })}
                onTimeFormatChange={(timeFormat) => onChange({ timeFormat })}
              />
            </div>
            <QueryResult
              project={project}
              panel={panel}
              executed={executed}
              localTime={localTime}
              onRetry={onRetry}
            />
          </TabsContent>
          <TabsContent value="graph" className="mt-0 flex flex-col gap-4">
            <PrometheusQueryControls
              range={panel.range}
              resolution={panel.resolution}
              graphMode={panel.graphMode}
              tableMode={panel.tableMode}
              yAxisZero={panel.yAxisZero}
              localTime={localTime}
              onRangeChange={(range) => onChange({ range })}
              onResolutionChange={(resolution) => onChange({ resolution })}
              onGraphModeChange={(graphMode) => onChange({ graphMode })}
              onTableModeChange={(tableMode) => onChange({ tableMode })}
              onYAxisZeroChange={(yAxisZero) => onChange({ yAxisZero })}
              onLocalTimeChange={onLocalTimeChange}
            />
            <QueryResult
              project={project}
              panel={panel}
              executed={executed}
              localTime={localTime}
              onRetry={onRetry}
            />
          </TabsContent>
        </Tabs>
      </CardContent>

      {explorerOpen && (
        <MetricsExplorer
          open={explorerOpen}
          range={autocompleteRange}
          onOpenChange={setExplorerOpen}
          onInsert={(selector) => editorRef.current?.insertText(selector)}
        />
      )}
      {historyOpen && (
        <QueryHistoryDialog
          open={historyOpen}
          history={history}
          localTime={localTime}
          onOpenChange={setHistoryOpen}
          onPick={(query) => onChange({ query })}
          onClear={onClearHistory}
        />
      )}
    </Card>
  );
}

function QueryResult({ project, panel, executed, localTime, onRetry }: {
  project: string;
  panel: QueryPanelState;
  executed?: ExecutedQuery;
  localTime: boolean;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  const range = executed?.range;
  const step = range ? effectiveMetricsStep(panel.resolution, range.end - range.start) : 1;
  const graph = useProjectQuery<MetricsQueryEnvelope<TimeSeries[]>, ApiError>({
    project,
    queryKey: [
      "metrics-panel",
      panel.id,
      "graph",
      executed?.query,
      range?.start,
      range?.end,
      step,
      executed?.runId,
    ],
    enabled: panel.activeTab === "graph" && Boolean(executed && range),
    retry: false,
    placeholderData: (previous) => previous,
    queryFn: ({ signal, project: activeProject }) => queryRangeDetailed(
      { query: executed!.query, start: range!.start, end: range!.end, step },
      signal,
      activeProject,
    ),
  });
  const table = useProjectQuery<MetricsQueryEnvelope<MetricTableResult>, ApiError>({
    project,
    queryKey: [
      "metrics-panel",
      panel.id,
      "table",
      executed?.query,
      range?.end,
      executed?.runId,
    ],
    enabled: panel.activeTab === "table" && Boolean(executed && range),
    retry: false,
    placeholderData: (previous) => previous,
    queryFn: ({ signal, project: activeProject }) => queryInstantDetailed(executed!.query, range!.end, signal, activeProject),
  });
  const result = panel.activeTab === "table" ? table : graph;
  const count = panel.activeTab === "table"
    ? table.data?.data.rows.length ?? 0
    : graph.data?.data.length ?? 0;
  const viewState = queryViewState({
    hasData: result.data !== undefined,
    isLoading: result.isLoading,
    isError: result.isError,
    isEmpty: count === 0,
  });

  if (!executed || !range) {
    return <ResultMessage icon={<Search />} title={t("metrics.initialTitle")} desc={t("metrics.initialDesc")} />;
  }
  if (viewState === "pending") return <div className="space-y-2"><Skeleton className="h-8 w-full" /><Skeleton className="h-[320px] w-full" /></div>;
  if (viewState === "error") {
    return <ResultMessage icon={<AlertCircle className="text-destructive" />} title={t("metrics.errorTitle")} desc={t("common.queryErrorDesc")} action={<Button variant="outline" size="sm" onClick={onRetry}>{t("common.retry")}</Button>} />;
  }
  if (!result.data) return <ResultMessage icon={<Database />} title={t("metrics.emptyTitle")} desc={t("metrics.emptyDesc")} />;

  return (
    <>
      {viewState === "stale" && (
        <QueryStaleNotice isRetrying={result.isFetching} onRetry={onRetry} />
      )}
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{t("metrics.loadStats", { duration: result.data.durationMs.toFixed(0), count })}</span>
        {result.isFetching && <span className="inline-flex items-center gap-1"><Loader2 className="size-3 animate-spin" />{t("metrics.loading")}</span>}
      </div>
      <QueryNotices warnings={result.data.warnings} infos={result.data.infos} />
      {panel.activeTab === "table"
        ? table.data!.data.rows.length > 0
          ? (
            <MetricsTable
              result={table.data!.data}
              localTime={localTime}
              mode={panel.tableMode}
              timeFormat={panel.timeFormat}
            />
          )
          : <ResultMessage icon={<Database />} title={t("metrics.emptyTitle")} desc={t("metrics.emptyDesc")} />
        : graph.data!.data.length > 0
          ? (
            <TimeSeriesChart
              series={graph.data!.data}
              useUTC={!localTime}
              stacked={panel.graphMode === "stacked"}
              yAxisMin={panel.yAxisZero ? 0 : null}
              tableMode={panel.tableMode}
              timeWindow={{
                startMs: range.start * 1_000,
                endMs: range.end * 1_000,
              }}
              height={520}
            />
          )
          : <ResultMessage icon={<Database />} title={t("metrics.emptyTitle")} desc={t("metrics.emptyDesc")} />}
    </>
  );
}

function QueryNotices({ warnings, infos }: { warnings: string[]; infos: string[] }) {
  const { t } = useTranslation();
  if (warnings.length === 0 && infos.length === 0) return null;

  return (
    <div className="flex flex-col gap-2">
      {warnings.map((warning, index) => <div key={`warning-${index}`} className="flex gap-2 rounded-md border border-destructive/30 bg-destructive/5 p-2 text-xs"><AlertCircle className="size-4 shrink-0 text-destructive" /><div><strong>{t("metrics.queryWarning")}</strong><p className="text-muted-foreground">{warning}</p></div></div>)}
      {infos.map((info, index) => <div key={`info-${index}`} className="flex gap-2 rounded-md border border-border bg-muted/30 p-2 text-xs"><Info className="size-4 shrink-0" /><div><strong>{t("metrics.queryInfo")}</strong><p className="text-muted-foreground">{info}</p></div></div>)}
    </div>
  );
}

function MetricsTableSettings({
  mode,
  timeFormat,
  onModeChange,
  onTimeFormatChange,
}: {
  mode: MetricsTableMode;
  timeFormat: MetricsTimeFormat;
  onModeChange: (mode: MetricsTableMode) => void;
  onTimeFormatChange: (format: MetricsTimeFormat) => void;
}) {
  const { t } = useTranslation();

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          size="icon"
          className="size-9"
          aria-label={t("metrics.tableSettings")}
        >
          <Settings2 />
        </Button>
      </PopoverTrigger>
      <PopoverContent
        align="end"
        className="w-max max-w-[calc(100vw-2rem)] space-y-4"
      >
        <div className="space-y-2">
          <p className="text-xs font-medium text-muted-foreground">
            {t("metrics.columnMode")}
          </p>
          <div className="inline-flex rounded-md border border-input bg-background p-0.5">
            <Button
              variant={mode === "labels" ? "secondary" : "ghost"}
              size="sm"
              className="h-7 px-2.5"
              onClick={() => onModeChange("labels")}
            >
              {t("metrics.labelColumns")}
            </Button>
            <Button
              variant={mode === "series" ? "secondary" : "ghost"}
              size="sm"
              className="h-7 px-2.5"
              onClick={() => onModeChange("series")}
            >
              {t("metrics.seriesColumns")}
            </Button>
          </div>
        </div>
        <div className="space-y-2">
          <p className="text-xs font-medium text-muted-foreground">
            {t("metrics.timeFormat")}
          </p>
          <div className="inline-flex rounded-md border border-input bg-background p-0.5">
            <Button
              variant={timeFormat === "readable" ? "secondary" : "ghost"}
              size="sm"
              className="h-7 px-2.5"
              onClick={() => onTimeFormatChange("readable")}
            >
              {t("metrics.readableTime")}
            </Button>
            <Button
              variant={timeFormat === "timestamp" ? "secondary" : "ghost"}
              size="sm"
              className="h-7 px-2.5"
              onClick={() => onTimeFormatChange("timestamp")}
            >
              {t("metrics.timestamp")}
            </Button>
          </div>
        </div>
      </PopoverContent>
    </Popover>
  );
}

function MetricsTable({
  result,
  localTime,
  mode,
  timeFormat,
}: {
  result: MetricTableResult;
  localTime: boolean;
  mode: MetricsTableMode;
  timeFormat: MetricsTimeFormat;
}) {
  const { t, i18n } = useTranslation();
  const [showAll, setShowAll] = useState(false);
  const limit = 10_000;
  const rows = showAll ? result.rows : result.rows.slice(0, limit);
  const labelNames = useMemo(() => metricsLabelColumns(result.rows), [result.rows]);

  return (
    <div className="space-y-2">
      {!showAll && result.rows.length > limit && (
        <div className="flex items-center justify-between rounded-md border border-border bg-muted/30 p-2 text-xs">
          <span>{t("metrics.limitedResults", { shown: limit, total: result.rows.length })}</span>
          <Button variant="link" size="sm" onClick={() => setShowAll(true)}>{t("metrics.showAll")}</Button>
        </div>
      )}
      <div className="select-text overflow-auto rounded-md border border-border">
        <table className="w-full min-w-max table-auto text-left text-sm">
          <thead className="sticky top-0 z-10 border-b border-border bg-muted/95 text-xs text-muted-foreground backdrop-blur">
            {mode === "series" ? (
              <tr>
                <th className="px-3 py-2 font-medium">
                  {t("metrics.fullMetric", { count: result.rows.length })}
                </th>
                <th className="px-3 py-2 text-right font-medium">{t("metrics.samples")}</th>
              </tr>
            ) : (
              <tr>
                <th className="min-w-40 max-w-80 px-3 py-2 font-medium">
                  {t("metrics.metricName")}
                </th>
                {labelNames.map((name) => (
                  <th key={name} className="min-w-40 max-w-80 break-all px-3 py-2 font-mono font-medium">
                    {name}
                  </th>
                ))}
                <th className="whitespace-nowrap px-3 py-2 text-right font-medium">
                  {t("metrics.value")}
                </th>
                <th className="whitespace-nowrap px-3 py-2 text-right font-medium">
                  {t(timeFormat === "timestamp" ? "metrics.timestamp" : "metrics.readableTime")}
                </th>
              </tr>
            )}
          </thead>
          <tbody>
            {rows.map((row, rowIndex) => (
              <tr key={`${row.name}-${rowIndex}`} className="border-b border-border/60 last:border-0 hover:bg-muted/40">
                {mode === "series" ? (
                  <>
                    <td className="max-w-2xl break-all px-3 py-2 align-top font-mono text-xs">
                      {row.name}
                    </td>
                    <td className="px-3 py-2 text-right align-top font-mono text-xs">
                      {row.values.map((sample, sampleIndex) => {
                        const readable = formatMetricSampleTime(
                          sample.timestamp,
                          localTime,
                          i18n.language,
                        );
                        const timestamp = Math.floor(sample.timestamp / 1000);
                        return (
                          <div key={`${sample.timestamp}-${sampleIndex}`}>
                            <span>{sample.raw}</span>{" "}
                            <span
                              className="text-muted-foreground"
                              title={timeFormat === "readable" ? String(timestamp) : readable}
                            >
                              @ {timeFormat === "readable" ? readable : timestamp}
                            </span>
                          </div>
                        );
                      })}
                    </td>
                  </>
                ) : (
                  <>
                    <td className="min-w-40 max-w-80 px-3 py-2 align-top font-mono text-xs">
                      <span
                        className="block break-all leading-5"
                        title={row.labels.__name__ ?? row.name}
                      >
                        {row.labels.__name__ ?? row.name}
                      </span>
                    </td>
                    {labelNames.map((name) => (
                      <td key={name} className="min-w-40 max-w-80 px-3 py-2 align-top font-mono text-xs">
                        {row.labels[name] === undefined
                          ? <span className="text-muted-foreground">—</span>
                          : (
                              <span
                                className="block break-all leading-5"
                                title={row.labels[name]}
                              >
                                {row.labels[name]}
                              </span>
                            )}
                      </td>
                    ))}
                    <td className="whitespace-nowrap px-3 py-2 text-right align-top font-mono text-xs">
                      {row.values.map((sample, sampleIndex) => (
                        <div key={`${sample.timestamp}-${sampleIndex}`}>{sample.raw}</div>
                      ))}
                    </td>
                    <td className="whitespace-nowrap px-3 py-2 text-right align-top font-mono text-xs">
                      {row.values.map((sample, sampleIndex) => {
                        const readable = formatMetricSampleTime(
                          sample.timestamp,
                          localTime,
                          i18n.language,
                        );
                        const timestamp = Math.floor(sample.timestamp / 1000);
                        return (
                          <div
                            key={`${sample.timestamp}-${sampleIndex}`}
                            title={timeFormat === "readable" ? String(timestamp) : readable}
                          >
                            {timeFormat === "readable" ? readable : timestamp}
                          </div>
                        );
                      })}
                    </td>
                  </>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function formatMetricSampleTime(ms: number, localTime: boolean, language: string): string {
  const value = localTime ? dayjs(ms) : dayjs.utc(ms);
  return value.locale(dayjsLocale(language)).format("L LTS");
}

function QueryHistoryDialog({ open, history, localTime, onOpenChange, onPick, onClear }: {
  open: boolean;
  history: MetricsHistoryEntry[];
  localTime: boolean;
  onOpenChange: (open: boolean) => void;
  onPick: (query: string) => void;
  onClear: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogHeader><DialogTitle>{t("metrics.history")}</DialogTitle><DialogDescription>{t("metrics.historyProjectScoped")}</DialogDescription></DialogHeader>
        <div className="flex justify-end">{history.length > 0 && <Button variant="ghost" size="sm" onClick={onClear}><Trash2 />{t("metrics.clearHistory")}</Button>}</div>
        {history.length === 0 ? <p className="py-12 text-center text-sm text-muted-foreground">{t("metrics.historyEmpty")}</p> : (
          <div className="max-h-[60vh] overflow-auto rounded-md border border-border">
            {history.map((entry) => {
              const at = entry.at ? dateParts(entry.at, localTime) : null;
              return <button key={`${entry.query}-${entry.at}`} type="button" className="block w-full border-b border-border px-3 py-2 text-left last:border-0 hover:bg-muted" onClick={() => { onPick(entry.query); onOpenChange(false); }}><code className="block break-all text-xs">{entry.query}</code>{at && <span className="mt-1 block text-[10px] text-muted-foreground">{at.date} {at.time}</span>}</button>;
            })}
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}

function ResultMessage({ icon, title, desc, action }: { icon: React.ReactNode; title: string; desc: string; action?: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-14 text-center">
      <div className={cn("flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground [&>svg]:size-5")}>{icon}</div>
      <div className="text-sm font-medium">{title}</div>
      <p className="max-w-lg break-words text-xs text-muted-foreground">{desc}</p>
      {action}
    </div>
  );
}
