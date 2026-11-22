import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { Database } from "lucide-react";
import type { LogRow } from "@/lib/api/logs";
import {
  buildLogsExtraFilterParams,
  LOGS_LIMIT_CONFIRM_THRESHOLD,
  statsChartSeries,
  upsertLogsFilter,
  type LogsExtraFilter,
  type LogsViewMode,
} from "@/lib/logs-explorer";
import {
  parsePrometheusDuration,
  resolveMetricsRange,
  type MetricsRange,
} from "@/lib/metrics-query";
import { IS_MOCK } from "@/lib/settings";
import { useProject } from "@/lib/project/project-provider";
import { useRouterLocationSync } from "@/lib/router-url-sync";
import { PageContainer } from "@/components/layout/page";
import type { LogsHitsChartSeries } from "@/components/charts/logs-histogram";
import { LogDetailDrawer } from "@/components/logs/log-detail-drawer";
import { LogsChartPanel } from "@/components/logs/logs-chart-panel";
import { LogsFilterPopover } from "@/components/logs/logs-filter-popover";
import { LogsQueryPanel } from "@/components/logs/logs-query-panel";
import { LogsResultsPanel } from "@/components/logs/logs-results-panel";
import { LogsStreamContextDialog } from "@/components/logs/logs-stream-context";
import { useLogsExport } from "@/components/logs/use-logs-export";
import { useLogsFieldMetadata } from "@/components/logs/use-logs-field-metadata";
import {
  useLogsPresentationState,
  useLogsPresentationUrlSync,
} from "@/components/logs/use-logs-presentation";
import { Badge } from "@/components/ui/badge";
import { queryViewState } from "./query-view-state";
import { useLogsWorkspace } from "./use-logs-workspace";

interface LogsContextAnchor {
  project: string;
  row: LogRow;
}

export function LogsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { currentSlug: project } = useProject();
  const {
    routeLocation,
    currentHistoryHref,
    replaceLocation,
    subscribeExternalLocation,
  } = useRouterLocationSync();

  const [selection, setSelection] = useState<LogsContextAnchor | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [contextAnchor, setContextAnchor] = useState<LogsContextAnchor | null>(null);
  const [filterPickerOpen, setFilterPickerOpen] = useState(false);
  const [historyCursor, setHistoryCursor] = useState(-1);
  const [requestDuration, setRequestDuration] = useState(0);
  const requestStartedAt = useRef(0);
  const previousProject = useRef(project);

  const resetTransientUi = useCallback(() => {
    setHistoryCursor(-1);
    setSelection(null);
    setDrawerOpen(false);
    setContextAnchor(null);
    setFilterPickerOpen(false);
  }, []);

  const presentation = useLogsPresentationState(routeLocation);
  const { chart, hideLogs, autocomplete } = presentation;

  const auxiliaryConfig = useMemo(() => ({
    rowsEnabled: !hideLogs,
    chartEnabled: !chart.hidden,
    graphMode: chart.graphMode,
    groupBy: chart.groupBy,
    topN: chart.topN,
    step: parsePrometheusDuration(chart.step),
  }), [chart.graphMode, chart.groupBy, chart.hidden, chart.step, chart.topN, hideLogs]);

  const { workspace, snapshot } = useLogsWorkspace({
    project,
    location: routeLocation,
    auxiliaryConfig,
    replaceLocation,
  });
  const { intent, executed, finite, live, history } = snapshot;
  const { draft: queryText, range, limit, filters, refreshSeconds } = intent;
  const view: LogsViewMode = intent.mode === "live" ? "live" : intent.historicalView;
  const historyDraft = useRef(queryText);

  useLogsPresentationUrlSync({
    presentation,
    currentHistoryHref,
    replaceLocation,
    subscribeExternalLocation,
    view,
    onExternalNavigation: resetTransientUi,
  });

  const setQueryText = useCallback((draft: string) => {
    workspace.send({ type: "draftChanged", draft });
  }, [workspace]);

  const filtersForRequest = useMemo(
    () => buildLogsExtraFilterParams(executed?.filters ?? filters),
    [executed?.filters, filters],
  );
  const draftRange = useMemo(() => resolveMetricsRange(range), [range]);
  const effectiveRange = snapshot.effectiveRange;
  const automaticStep = effectiveRange
    ? Math.max(1, Math.ceil((effectiveRange.end - effectiveRange.start) / 300))
    : 1;
  const chartStep = parsePrometheusDuration(chart.step) ?? automaticStep;
  const metadataRange = useMemo(() => effectiveRange ?? ({
    start: draftRange.start,
    end: draftRange.end,
  }), [draftRange.end, draftRange.start, effectiveRange]);

  const { downloading, downloadError, download, cancelDownload } = useLogsExport({
    executed,
    effectiveRange,
    filtersForRequest,
  });

  const metadata = useLogsFieldMetadata({
    project,
    executedQuery: executed?.query,
    executedFilters: executed?.filters,
    filters,
    filtersForRequest,
    metadataRange,
    enabled: autocomplete || filterPickerOpen || Boolean(executed && effectiveRange),
  });

  useEffect(() => {
    if (previousProject.current === project) return;
    previousProject.current = project;
    cancelDownload();
    resetTransientUi();
  }, [cancelDownload, project, resetTransientUi]);

  const execute = useCallback((query = queryText) => {
    if (
      limit > LOGS_LIMIT_CONFIRM_THRESHOLD
      && !window.confirm(t("logs.largeLimitConfirm", { count: limit }))
    ) return;
    if (query !== queryText) setQueryText(query);
    setHistoryCursor(-1);
    workspace.send({ type: "run" });
  }, [limit, queryText, setQueryText, t, workspace]);

  const applyFilters = useCallback((next: LogsExtraFilter[]) => {
    workspace.send({ type: "filtersChanged", filters: next });
  }, [workspace]);

  const addFilter = useCallback((
    field: string,
    value: string,
    operator: LogsExtraFilter["operator"],
    isStream = false,
  ) => {
    applyFilters(upsertLogsFilter(filters, { field, value, operator, isStream }));
  }, [applyFilters, filters]);

  const removeFilter = useCallback((field: string, value: string) => {
    applyFilters(filters.filter((filter) => (
      filter.field !== field || filter.value !== value
    )));
  }, [applyFilters, filters]);

  const removeFieldFilters = useCallback((field: string) => {
    applyFilters(filters.filter((filter) => !(filter.isStream && filter.field === field)));
  }, [applyFilters, filters]);

  const streamFilters = useMemo(
    () => filters.filter((filter) => filter.isStream),
    [filters],
  );

  const changeRange = useCallback((next: MetricsRange) => {
    workspace.send({ type: "rangeChanged", range: next });
  }, [workspace]);

  const selectChartRange = useCallback((startMs: number, endMs: number) => {
    changeRange({ mode: "absolute", startMs, endMs });
  }, [changeRange]);

  const openRowDetail = useCallback((row: LogRow) => {
    setSelection({ project, row });
    setDrawerOpen(true);
  }, [project]);

  function viewTrace(traceId: string) {
    void navigate({ to: "/traces/$traceID", params: { traceID: traceId } });
  }

  function historyPrevious() {
    if (!history.length) return;
    setHistoryCursor((current) => {
      if (current < 0) historyDraft.current = queryText;
      const next = Math.min(current + 1, history.length - 1);
      setQueryText(history[next].query);
      return next;
    });
  }

  function historyNext() {
    if (historyCursor < 0) return;
    setHistoryCursor((current) => {
      const next = current - 1;
      setQueryText(next < 0 ? historyDraft.current : history[next].query);
      return next;
    });
  }

  const timeRangeLoading = finite.timeRange.status === "loading" && finite.timeRange.data === null;
  const timeRangeFetching = finite.timeRange.status === "loading";
  const rowsLoading = finite.rows.status === "loading" && finite.rows.data === null;
  const rowsFetching = finite.rows.status === "loading";
  const hitsLoading = finite.hits.status === "loading" && finite.hits.data === null;
  const hitsFetching = finite.hits.status === "loading";
  const statsLoading = finite.stats.status === "loading" && finite.stats.data === null;
  const statsFetching = finite.stats.status === "loading";

  const chartSeries = useMemo<LogsHitsChartSeries[]>(() => {
    if (chart.graphMode === "hits") {
      return (finite.hits.data?.hits ?? []).map((series) => ({
        ...series,
        isOther: Boolean(chart.groupBy) && series.isOther,
      }));
    }
    return statsChartSeries(finite.stats.data?.data.result ?? [], chart.topN);
  }, [chart.graphMode, chart.groupBy, chart.topN, finite.hits.data, finite.stats.data]);

  const queryFetching = timeRangeFetching || rowsFetching || hitsFetching || statsFetching;
  useEffect(() => {
    if (queryFetching && requestStartedAt.current === 0) {
      requestStartedAt.current = performance.now();
    } else if (!queryFetching && requestStartedAt.current > 0) {
      setRequestDuration(performance.now() - requestStartedAt.current);
      requestStartedAt.current = 0;
    }
  }, [queryFetching]);

  const rows = finite.rows.data ?? [];
  const selectedChartError = chart.graphMode === "hits" ? finite.hits.error : finite.stats.error;
  const chartError = finite.timeRange.error ?? selectedChartError;
  const chartRetrySlot = finite.timeRange.error ? "timeRange" : chart.graphMode;
  const chartLoading = timeRangeLoading || (chart.graphMode === "hits" ? hitsLoading : statsLoading);
  const chartFetching = timeRangeFetching || (chart.graphMode === "hits" ? hitsFetching : statsFetching);
  const selectedChartData = chart.graphMode === "hits" ? finite.hits.data : finite.stats.data;

  const chartViewState = queryViewState({
    hasData: selectedChartData !== null,
    isLoading: chartLoading,
    isError: Boolean(chartError),
    isEmpty: chartSeries.length === 0,
  });
  const rowsViewState = queryViewState({
    hasData: finite.rows.data !== null,
    isLoading: timeRangeLoading || rowsLoading,
    isError: Boolean(finite.rows.error || finite.timeRange.error),
    isEmpty: rows.length === 0,
  });
  const totalHits = chartSeries.reduce((sum, series) => sum + series.total, 0);

  return (
    <PageContainer>
      <div className="min-w-0 space-y-4">
        <LogsQueryPanel
          queryText={queryText}
          autocomplete={autocomplete}
          queryFetching={queryFetching}
          range={range}
          localTime={presentation.localTime}
          refreshSeconds={refreshSeconds}
          limit={limit}
          filters={filters}
          history={history}
          timeRangeData={finite.timeRange.data}
          onQueryChange={(value) => {
            setQueryText(value);
            setHistoryCursor(-1);
          }}
          onRunOrCancel={() => queryFetching
            ? workspace.send({ type: "cancelFinite" })
            : execute()}
          onComplete={metadata.completeQuery}
          onHistoryPrevious={historyPrevious}
          onHistoryNext={historyNext}
          onRangeChange={changeRange}
          onLocalTimeChange={presentation.setLocalTime}
          onRefreshSecondsChange={(seconds) => workspace.send({
            type: "refreshSecondsChanged",
            refreshSeconds: seconds,
          })}
          onRefresh={() => workspace.send({ type: "refresh" })}
          onAutocompleteChange={presentation.setAutocomplete}
          onLimitChange={(nextLimit) => workspace.send({
            type: "limitChanged",
            limit: nextLimit,
          })}
          onReset={() => workspace.send({ type: "reset" })}
          onSelectExample={setQueryText}
          onLoadHistory={setQueryText}
          onRunHistory={execute}
          onFavoriteHistory={(query) => workspace.send({
            type: "historyFavoriteToggled",
            query,
          })}
          onRemoveHistory={(query) => workspace.send({
            type: "historyRemoved",
            query,
          })}
          onClearHistory={() => workspace.send({ type: "historyCleared" })}
          onRemoveFilter={removeFilter}
          onClearFilters={() => applyFilters([])}
          filterControl={
            <LogsFilterPopover
              open={filterPickerOpen}
              onOpenChange={setFilterPickerOpen}
              fields={metadata.streamFields}
              fieldsLoading={metadata.streamFieldsLoading}
              fieldsError={metadata.streamFieldsError}
              fieldValues={metadata.fieldValues}
              fieldValuesLoading={metadata.fieldValuesLoading}
              fieldValuesErrors={metadata.fieldValuesErrors}
              filters={streamFilters}
              onRetryFields={metadata.retryStreamFields}
              onRequestFieldValues={metadata.requestFieldValues}
              onUpsertFilter={(filter) => addFilter(
                filter.field,
                filter.value,
                filter.operator,
                true,
              )}
              onRemoveFilter={removeFilter}
              onRemoveField={removeFieldFilters}
              onClearFilters={() => applyFilters(
                filters.filter((filter) => !filter.isStream),
              )}
            />
          }
          badge={IS_MOCK ? (
            <Badge variant="secondary" className="gap-1.5">
              <Database className="size-3" />
              {t("logs.mockBadge")}
            </Badge>
          ) : undefined}
        />

        <LogsChartPanel
          chart={chart}
          allFields={metadata.allFields}
          chartSeries={chartSeries}
          totalHits={totalHits}
          requestDuration={requestDuration}
          chartStep={chartStep}
          hasExecuted={Boolean(executed)}
          chartViewState={chartViewState}
          chartFetching={chartFetching}
          onChartChange={(patch) => presentation.setChart((current) => ({ ...current, ...patch }))}
          onShowChart={() => presentation.setChart((current) => ({ ...current, hidden: false }))}
          onRetry={() => workspace.send({ type: "retry", slot: chartRetrySlot })}
          onSelectRange={selectChartRange}
          onFilterSeries={(seriesFields) => {
            let next = filters;
            for (const [field, value] of Object.entries(seriesFields)) {
              next = upsertLogsFilter(next, {
                field,
                value,
                operator: "=",
                isStream: false,
              });
            }
            applyFilters(next);
          }}
        />

        <LogsResultsPanel
          project={project}
          view={view}
          hideLogs={hideLogs}
          hasExecuted={Boolean(executed)}
          effectiveRange={effectiveRange}
          downloading={downloading}
          downloadError={downloadError}
          rows={rows}
          rowsViewState={rowsViewState}
          rowsFetching={rowsFetching}
          rowsStaleRetrying={rowsFetching || timeRangeFetching}
          groupSettings={presentation.groupSettings}
          tableSettings={presentation.tableSettings}
          live={live}
          liveFollowing={presentation.liveFollowing}
          liveRaw={presentation.liveRaw}
          liveOffset={intent.liveOffset}
          liveLimit={intent.liveLimit}
          onViewChange={(nextView) => workspace.send({ type: "viewChanged", view: nextView })}
          onToggleHideLogs={() => presentation.setHideLogs((hidden) => !hidden)}
          onCancelDownload={cancelDownload}
          onDownload={(format, filename) => void download(format, filename)}
          onRetryRows={() => workspace.send({
            type: "retry",
            slot: finite.timeRange.error ? "timeRange" : "rows",
          })}
          onGroupSettingsChange={presentation.setGroupSettings}
          onTableSettingsChange={presentation.setTableSettings}
          onIncludeField={(field, value) => addFilter(field, value, "=")}
          onExcludeField={(field, value) => addFilter(field, value, "!=")}
          onViewTrace={viewTrace}
          onShowContext={(row) => setContextAnchor({ project, row })}
          onOpenRow={openRowDetail}
          onLiveStoppedChange={(stopped) => {
            workspace.send({ type: stopped ? "liveStopped" : "liveResumed" });
            if (!stopped) presentation.setLiveFollowing(true);
          }}
          onLiveFollowingChange={presentation.setLiveFollowing}
          onLiveRawChange={presentation.setLiveRaw}
          onLiveOffsetChange={(offset) => workspace.send({ type: "liveOffsetChanged", offset })}
          onLiveLimitChange={(nextLimit) => workspace.send({
            type: "liveLimitChanged",
            limit: nextLimit,
          })}
          onLiveRetry={() => workspace.send({ type: "liveRetried" })}
          onLiveClear={() => workspace.send({ type: "liveCleared" })}
        />
      </div>

      <LogDetailDrawer
        row={selection?.project === project ? selection.row : null}
        open={drawerOpen && selection?.project === project}
        onOpenChange={setDrawerOpen}
        onAddFilter={(field, value) => {
          setDrawerOpen(false);
          addFilter(field, value, "=");
        }}
        onViewTrace={(traceId) => {
          setDrawerOpen(false);
          viewTrace(traceId);
        }}
      />
      <LogsStreamContextDialog
        anchor={contextAnchor?.project === project ? contextAnchor.row : null}
        open={contextAnchor?.project === project}
        start={metadataRange.start}
        end={metadataRange.end}
        filters={filtersForRequest}
        onOpenChange={(open) => {
          if (!open) setContextAnchor(null);
        }}
      />
    </PageContainer>
  );
}
