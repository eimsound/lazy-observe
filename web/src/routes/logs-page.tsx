import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useTranslation } from "react-i18next";
import {
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  useNavigate,
  useRouter,
  useRouterState,
} from "@tanstack/react-router";
import {
  AlertCircle,
  BarChart3,
  Braces,
  Database,
  Eye,
  EyeOff,
  Filter,
  ListTree,
  Loader2,
  Pause,
  Play,
  Radio,
  RotateCcw,
  Search,
  Square,
  Table2,
} from "lucide-react";
import dayjs from "@/lib/dayjs";
import {
  exportLogs,
  parseLogsTimestamp,
  queryLogFieldNames,
  queryLogFieldValues,
  queryLogStreamFieldNames,
  queryLogStreamFieldValues,
  type LogRow,
  type LogsFieldValue,
} from "@/lib/api/logs";
import {
  buildLogsExtraFilterParams,
  DEFAULT_LOGS_LIMIT,
  DEFAULT_LOGS_QUERY,
  LOGS_LIMIT_CONFIRM_THRESHOLD,
  logsAutocompleteBaseQuery,
  logsRangeFromSearch,
  MAX_LOGS_LIMIT,
  parseLogsFilters,
  statsChartSeries,
  upsertLogsFilter,
  type LogsAutocompleteContext,
  type LogsExtraFilter,
  type LogsGraphMode,
  type LogsViewMode,
} from "@/lib/logs-explorer";
import {
  formatPrometheusDuration,
  parsePrometheusDuration,
  resolveMetricsRange,
  type MetricsRange,
} from "@/lib/metrics-query";
import { IS_MOCK } from "@/lib/settings";
import { useProject } from "@/lib/project/project-provider";
import { PageActions, PageContainer } from "@/components/layout/page";
import {
  LogsHitsChart,
  type LogsHitsChartSeries,
} from "@/components/charts/logs-histogram";
import { LogDetailDrawer } from "@/components/logs/log-detail-drawer";
import {
  ExtraFiltersBar,
  LogsFilterSidebar,
} from "@/components/logs/logs-filter-sidebar";
import {
  LogsGroupView,
  LogsJsonView,
  LogsTableView,
  type LogsGroupViewSettings,
  type LogsTableViewSettings,
} from "@/components/logs/logs-explorer-views";
import { createDefaultLogsGroupViewSettings, createDefaultLogsTableViewSettings } from "@/components/logs/logs-explorer-model";
import { LogsLiveView } from "@/components/logs/logs-live-view";
import { LogsStreamContextDialog } from "@/components/logs/logs-stream-context";
import {
  LogsChartOptions,
  LogsLimitInput,
  LogsTimeControls,
  type LogsChartOptionsValue,
} from "@/components/logs/logs-query-controls";
import {
  LogsDownloadMenu,
  LogsExamplesMenu,
  LogsHistoryMenu,
} from "@/components/logs/logs-query-menus";
import {
  LogsQLEditor,
  type LogsQLEditorSuggestion,
} from "@/components/logs/logsql-editor";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";
import { queryViewState } from "./query-view-state";
import { useLogsWorkspace } from "./use-logs-workspace";

interface LogsContextAnchor {
  project: string;
  row: LogRow;
}

interface InitialLogsState {
  query: string;
  range: MetricsRange;
  limit: number;
  view: LogsViewMode;
  filters: LogsExtraFilter[];
  refreshSeconds: number;
  autocomplete: boolean;
  hideLogs: boolean;
  sidebarOpen: boolean;
  chart: LogsChartOptionsValue;
  groupSettings: LogsGroupViewSettings;
  tableSettings: LogsTableViewSettings;
  liveLimit: number;
  liveOffset: string;
  shouldRun: boolean;
}

const LOCAL_TIME_KEY = "lo-logs-local-time";
const LIVE_RAW_KEY = "lo-logs-live-raw";

interface LogsFileHandle {
  createWritable: () => Promise<WritableStream<Uint8Array>>;
}

interface LogsFilePickerWindow extends Window {
  showSaveFilePicker?: (options: {
    suggestedName: string;
    types: Array<{
      description: string;
      accept: Record<string, string[]>;
    }>;
  }) => Promise<LogsFileHandle>;
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

function queryBoolean(value: string | null, fallback = false): boolean {
  if (value === null) return fallback;
  return value === "1" || value === "true";
}

function queryNumber(
  value: string | null,
  fallback: number,
  minimum: number,
  maximum: number,
): number {
  if (value === null || !value.trim()) return fallback;
  const parsed = Number(value);
  return Number.isFinite(parsed)
    ? Math.max(minimum, Math.min(maximum, Math.floor(parsed)))
    : fallback;
}

function queryColumnWidths(value: string | null): Record<string, number> {
  if (!value) return {};
  try {
    const parsed = JSON.parse(value) as unknown;
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    return Object.fromEntries(
      Object.entries(parsed as Record<string, unknown>)
        .slice(0, 100)
        .flatMap(([field, width]) => (
          field
          && Number.isFinite(Number(width))
            ? [[field.slice(0, 256), Math.max(96, Math.min(800, Number(width)))]]
            : []
        )),
    );
  } catch {
    return {};
  }
}

function initialLogsState(locationHref?: string): InitialLogsState {
  const params = locationHref !== undefined
    ? new URL(locationHref, "https://lazy-observe.invalid").searchParams
    : typeof window === "undefined"
      ? new URLSearchParams()
      : new URLSearchParams(window.location.search);
  const query = params.get("q")?.trim() || DEFAULT_LOGS_QUERY;
  const viewValue = params.get("view");
  const view: LogsViewMode = ["group", "table", "json", "live"].includes(viewValue ?? "")
    ? viewValue as LogsViewMode
    : "group";
  const graphMode: LogsGraphMode = params.get("graph") === "stats" ? "stats" : "hits";
  const range = logsRangeFromSearch({
    range: params.get("range") ?? undefined,
    start: params.get("start") ?? undefined,
    end: params.get("end") ?? undefined,
  });
  const groupSettings = createDefaultLogsGroupViewSettings();
  groupSettings.groupBy = params.get("group_by") || null;
  groupSettings.displayFields = params.get("display_fields")
    ?.split(",")
    .map((field) => field.trim())
    .filter(Boolean) ?? [];
  groupSettings.singleLine = queryBoolean(params.get("single_line"), true);
  groupSettings.compactHeader = queryBoolean(params.get("compact_groups"));
  groupSettings.markdownParsing = queryBoolean(params.get("markdown"));
  groupSettings.ansiParsing = queryBoolean(params.get("ansi"));
  groupSettings.disableHover = queryBoolean(params.get("disable_hover"));
  const tableSettings = createDefaultLogsTableViewSettings();
  const rowsPerPage = queryNumber(params.get("rows_per_page"), 100, 10, 50_000);
  groupSettings.pageSize = rowsPerPage;
  tableSettings.pageSize = rowsPerPage;
  const tableFields = params.get("table_fields")
    ?.split(",")
    .map((field) => field.trim())
    .filter(Boolean);
  if (tableFields?.length) tableSettings.visibleFields = tableFields;
  tableSettings.columnWidths = queryColumnWidths(params.get("table_widths"));

  return {
    query,
    range,
    limit: queryNumber(params.get("limit"), DEFAULT_LOGS_LIMIT, 1, MAX_LOGS_LIMIT),
    view,
    filters: parseLogsFilters(params.get("filters")),
    refreshSeconds: queryNumber(params.get("refresh"), 0, 0, 3_600),
    autocomplete: !queryBoolean(params.get("autocomplete"), false),
    hideLogs: queryBoolean(params.get("hide_logs")),
    sidebarOpen: typeof window !== "undefined" && window.innerWidth >= 1024,
    chart: {
      graphMode,
      groupBy: params.get("graph_by") ?? "",
      topN: queryNumber(params.get("graph_top"), 5, 1, 50),
      step: params.get("graph_step") ?? "",
      stacked: queryBoolean(params.get("stacked")),
      cumulative: queryBoolean(params.get("cumulative")),
      hidden: queryBoolean(params.get("hide_chart")),
    },
    groupSettings,
    tableSettings,
    liveLimit: rowsPerPage,
    liveOffset: params.get("live_tailing_offset")
      ? `${queryNumber(params.get("live_tailing_offset"), 5, 1, 3_600)}s`
      : "5s",
    shouldRun: params.get("run") === "1"
      || (params.has("q") && !params.has("run")),
  };
}

function localTimePreference(): boolean {
  try {
    return localStorage.getItem(LOCAL_TIME_KEY) !== "false";
  } catch {
    return true;
  }
}

function liveRawPreference(): boolean {
  try {
    return localStorage.getItem(LIVE_RAW_KEY) === "true";
  } catch {
    return false;
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function LogsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const router = useRouter();
  const queryClient = useQueryClient();
  const { currentSlug: project } = useProject();
  // Re-read the raw history href whenever the router commits a navigation.
  const routeLocation = useRouterState({ select: () => router.history.location.href });
  const initialRef = useRef<InitialLogsState | null>(null);
  if (!initialRef.current) initialRef.current = initialLogsState(routeLocation);
  const initial = initialRef.current;
  const selfAuthoredLocationsRef = useRef<Set<string>>(new Set([routeLocation]));
  const [localTime, setLocalTime] = useState(localTimePreference);
  const [autocomplete, setAutocomplete] = useState(initial.autocomplete);
  const [hideLogs, setHideLogs] = useState(initial.hideLogs);
  const [sidebarOpen, setSidebarOpen] = useState(initial.sidebarOpen);
  const [chart, setChart] = useState<LogsChartOptionsValue>(initial.chart);
  const [groupSettings, setGroupSettings] = useState(initial.groupSettings);
  const [tableSettings, setTableSettings] = useState(initial.tableSettings);
  const [selected, setSelected] = useState<LogRow | null>(null);
  const selectedProject = useRef(project);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [contextAnchor, setContextAnchor] = useState<LogsContextAnchor | null>(null);
  const [historyCursor, setHistoryCursor] = useState(-1);
  const [fieldValues, setFieldValues] = useState<Record<string, LogsFieldValue[]>>({});
  const [fieldValuesLoading, setFieldValuesLoading] = useState<Record<string, boolean>>({});
  const [fieldValuesErrors, setFieldValuesErrors] = useState<Record<string, string | null>>({});
  const fieldValueRequestKeys = useRef<Record<string, string>>({});
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState("");
  const downloadController = useRef<AbortController | null>(null);
  const [requestDuration, setRequestDuration] = useState(0);
  const requestStartedAt = useRef(0);
  const [liveRaw, setLiveRaw] = useState(liveRawPreference);
  const [liveFollowing, setLiveFollowing] = useState(true);
  const previousProject = useRef(project);

  const auxiliaryConfig = useMemo(() => ({
    rowsEnabled: !hideLogs,
    chartEnabled: !chart.hidden,
    graphMode: chart.graphMode,
    groupBy: chart.groupBy,
    topN: chart.topN,
    step: parsePrometheusDuration(chart.step),
  }), [
    chart.graphMode,
    chart.groupBy,
    chart.hidden,
    chart.step,
    chart.topN,
    hideLogs,
  ]);
  const { workspace, snapshot } = useLogsWorkspace({
    project,
    location: routeLocation,
    auxiliaryConfig,
    replaceLocation: (location) => {
      selfAuthoredLocationsRef.current.add(location);
      router.history.replace(location, router.history.location.state);
    },
  });
  const { intent, executed, finite, live, history } = snapshot;
  const queryText = intent.draft;
  const range = intent.range;
  const limit = intent.limit;
  const filters = intent.filters;
  const refreshSeconds = intent.refreshSeconds;
  const view: LogsViewMode = intent.mode === "live" ? "live" : intent.historicalView;
  const liveOffset = intent.liveOffset;
  const liveLimit = intent.liveLimit;
  const liveRows = live.rows;
  const liveStopped = live.status === "stopped";
  const liveConnected = live.status === "open";
  const liveReconnecting = live.status === "retrying";
  const liveRate = live.rate;
  const liveDropped = live.dropped;
  const liveError = live.error ?? "";
  const historyDraft = useRef(queryText);

  const setQueryText = useCallback((draft: string) => {
    workspace.send({ type: "draftChanged", draft });
  }, [workspace]);
  const setLimit = useCallback((nextLimit: number) => {
    workspace.send({ type: "limitChanged", limit: nextLimit });
  }, [workspace]);
  const setView = useCallback((nextView: LogsViewMode) => {
    workspace.send({ type: "viewChanged", view: nextView });
  }, [workspace]);
  const setRefreshSeconds = useCallback((seconds: number) => {
    workspace.send({ type: "refreshSecondsChanged", refreshSeconds: seconds });
  }, [workspace]);
  const setLiveOffset = useCallback((offset: string) => {
    workspace.send({ type: "liveOffsetChanged", offset });
  }, [workspace]);
  const setLiveLimit = useCallback((nextLimit: number) => {
    workspace.send({ type: "liveLimitChanged", limit: nextLimit });
  }, [workspace]);

  useEffect(() => {
    if (previousProject.current === project) return;
    previousProject.current = project;
    downloadController.current?.abort();
    setSelected(null);
    setDrawerOpen(false);
    setContextAnchor(null);
    setHistoryCursor(-1);
  }, [project]);
  useEffect(() => {
    try {
      localStorage.setItem(LOCAL_TIME_KEY, String(localTime));
    } catch {
      // The in-memory preference remains usable without browser storage.
    }
  }, [localTime]);
  useEffect(() => {
    try {
      localStorage.setItem(LIVE_RAW_KEY, String(liveRaw));
    } catch {
      // The in-memory preference remains usable without browser storage.
    }
  }, [liveRaw]);
  useEffect(() => () => downloadController.current?.abort(), []);

  useEffect(() => {
    // TanStack queues history writes for a microtask, so window.location can
    // still hold a superseded URL here. Read the router's pending location
    // instead, or a stale snapshot would clobber the canonical one.
    const url = new URL(router.history.location.href, window.location.origin);
    const params = url.searchParams;
    const setOptional = (key: string, value: string | number | undefined) => {
      if (value === undefined || value === "") params.delete(key);
      else params.set(key, String(value));
    };
    setOptional("autocomplete", autocomplete ? undefined : "1");
    setOptional("hide_logs", hideLogs ? "1" : undefined);
    setOptional("graph", chart.graphMode === "hits" ? undefined : chart.graphMode);
    setOptional("graph_by", chart.groupBy || undefined);
    setOptional("graph_top", chart.topN === 5 ? undefined : chart.topN);
    setOptional("graph_step", chart.step || undefined);
    setOptional("stacked", chart.stacked ? "1" : undefined);
    setOptional("cumulative", chart.cumulative ? "1" : undefined);
    setOptional("hide_chart", chart.hidden ? "1" : undefined);
    setOptional("group_by", groupSettings.groupBy ?? undefined);
    setOptional(
      "display_fields",
      groupSettings.displayFields.length ? groupSettings.displayFields.join(",") : undefined,
    );
    setOptional("single_line", groupSettings.singleLine ? undefined : "0");
    setOptional("compact_groups", groupSettings.compactHeader ? "1" : undefined);
    setOptional("markdown", groupSettings.markdownParsing ? "1" : undefined);
    setOptional("ansi", groupSettings.ansiParsing ? "1" : undefined);
    setOptional("disable_hover", groupSettings.disableHover ? "1" : undefined);
    setOptional(
      "table_fields",
      tableSettings.visibleFields.join(",") === "_time,_msg,_stream"
        ? undefined
        : tableSettings.visibleFields.join(","),
    );
    setOptional(
      "table_widths",
      Object.keys(tableSettings.columnWidths ?? {}).length
        ? JSON.stringify(tableSettings.columnWidths)
        : undefined,
    );
    if (view !== "live") {
      const rowsPerPage = view === "table"
        ? tableSettings.pageSize
        : groupSettings.pageSize;
      setOptional("rows_per_page", rowsPerPage === 100 ? undefined : rowsPerPage);
    }
    const target = `${url.pathname}${url.search}${url.hash}`;
    if (target === router.history.location.href) return;
    selfAuthoredLocationsRef.current.add(target);
    router.history.replace(target, router.history.location.state);
  }, [
    autocomplete,
    chart,
    groupSettings.compactHeader,
    groupSettings.displayFields,
    groupSettings.disableHover,
    groupSettings.groupBy,
    groupSettings.markdownParsing,
    groupSettings.ansiParsing,
    groupSettings.pageSize,
    groupSettings.singleLine,
    hideLogs,
    tableSettings.visibleFields,
    tableSettings.columnWidths,
    tableSettings.pageSize,
    router,
    view,
  ]);

  // Only resync presentation state on genuine external navigations (back/forward
  // or deep links), never in response to URL writes authored by this page itself.
  useEffect(() => {
    if (selfAuthoredLocationsRef.current.has(routeLocation)) {
      selfAuthoredLocationsRef.current.delete(routeLocation);
      return;
    }
    selfAuthoredLocationsRef.current.clear();
    const next = initialLogsState(routeLocation);
    setAutocomplete(next.autocomplete);
    setHideLogs(next.hideLogs);
    setChart(next.chart);
    setGroupSettings((current) => ({
      ...next.groupSettings,
      pageSize: next.view === "live" ? current.pageSize : next.groupSettings.pageSize,
    }));
    setTableSettings((current) => ({
      ...next.tableSettings,
      pageSize: next.view === "live" ? current.pageSize : next.tableSettings.pageSize,
    }));
    setHistoryCursor(-1);
    setSelected(null);
    setDrawerOpen(false);
    setContextAnchor(null);
  }, [routeLocation]);

  const execute = useCallback((query = queryText) => {
    if (
      limit > LOGS_LIMIT_CONFIRM_THRESHOLD
      && !window.confirm(t("logs.largeLimitConfirm", { count: limit }))
    ) return;
    if (query !== queryText) setQueryText(query);
    setHistoryCursor(-1);
    workspace.send({ type: "run" });
  }, [limit, queryText, setQueryText, t, workspace]);

  const refresh = useCallback(() => {
    workspace.send({ type: "refresh" });
  }, [workspace]);

  const filtersForRequest = useMemo(
    () => buildLogsExtraFilterParams(executed?.filters ?? filters),
    [executed?.filters, filters],
  );
  const draftRange = useMemo(() => resolveMetricsRange(range), [range]);
  const effectiveRange = snapshot.effectiveRange;
  const readyForQuery = Boolean(executed && effectiveRange);
  const automaticStep = effectiveRange
    ? Math.max(1, Math.ceil((effectiveRange.end - effectiveRange.start) / 300))
    : 1;
  const chartStep = parsePrometheusDuration(chart.step) ?? automaticStep;
  const metadataRange = useMemo(() => effectiveRange ?? ({
    start: draftRange.start,
    end: draftRange.end,
  }), [draftRange.end, draftRange.start, effectiveRange]);
  const metadataQuery = executed?.query || DEFAULT_LOGS_QUERY;
  const metadataFilters = useMemo(
    () => buildLogsExtraFilterParams(executed?.filters ?? filters),
    [executed?.filters, filters],
  );
  const metadataIdentity = useMemo(() => JSON.stringify([
    project,
    metadataQuery,
    metadataRange.start,
    metadataRange.end,
    metadataFilters.extraFilters,
    metadataFilters.extraStreamFilters,
  ]), [
    metadataFilters.extraFilters,
    metadataFilters.extraStreamFilters,
    metadataQuery,
    metadataRange.end,
    metadataRange.start,
    project,
  ]);

  useEffect(() => {
    fieldValueRequestKeys.current = {};
    setFieldValues({});
    setFieldValuesLoading({});
    setFieldValuesErrors({});
  }, [metadataIdentity]);

  const timeRangeResult = {
    data: finite.timeRange.data,
    error: finite.timeRange.error,
    isLoading: finite.timeRange.status === "loading" && finite.timeRange.data === null,
    isFetching: finite.timeRange.status === "loading",
  };
  const logsResult = {
    data: finite.rows.data,
    error: finite.rows.error,
    isLoading: finite.rows.status === "loading" && finite.rows.data === null,
    isFetching: finite.rows.status === "loading",
  };
  const hitsResult = {
    data: finite.hits.data,
    error: finite.hits.error,
    isLoading: finite.hits.status === "loading" && finite.hits.data === null,
    isFetching: finite.hits.status === "loading",
  };
  const statsResult = {
    data: finite.stats.data,
    error: finite.stats.error,
    isLoading: finite.stats.status === "loading" && finite.stats.data === null,
    isFetching: finite.stats.status === "loading",
  };

  const fieldNamesResult = useQuery({
    queryKey: [
      "logs-explorer",
      project,
      "field-names",
      metadataQuery,
      metadataRange,
      metadataFilters,
    ],
    enabled: autocomplete || sidebarOpen || readyForQuery,
    staleTime: 30_000,
    retry: false,
    queryFn: ({ signal }) => queryLogFieldNames({
      query: metadataQuery,
      start: metadataRange.start,
      end: metadataRange.end,
      limit: 1_000,
      project,
      ...metadataFilters,
    }, signal),
  });

  const streamFieldsResult = useQuery({
    queryKey: [
      "logs-explorer",
      project,
      "stream-fields",
      metadataQuery,
      metadataRange,
      metadataFilters,
    ],
    enabled: autocomplete || sidebarOpen || readyForQuery,
    staleTime: 30_000,
    retry: false,
    queryFn: ({ signal }) => queryLogStreamFieldNames({
      query: metadataQuery,
      start: metadataRange.start,
      end: metadataRange.end,
      limit: 1_000,
      project,
      ...metadataFilters,
    }, signal),
  });

  const chartSeries = useMemo<LogsHitsChartSeries[]>(() => {
    if (chart.graphMode === "hits") {
      return (hitsResult.data?.hits ?? []).map((series) => ({
        ...series,
        isOther: Boolean(chart.groupBy) && series.isOther,
      }));
    }
    return statsChartSeries(statsResult.data?.data.result ?? [], chart.topN);
  }, [
    chart.graphMode,
    chart.groupBy,
    chart.topN,
    hitsResult.data,
    statsResult.data,
  ]);

  const allFields = useMemo(() => {
    const fields = new Set<string>(["_time", "_msg", "_stream"]);
    fieldNamesResult.data?.values.forEach((field) => fields.add(field.value));
    streamFieldsResult.data?.values.forEach((field) => fields.add(field.value));
    return [...fields].sort((left, right) => left.localeCompare(right));
  }, [fieldNamesResult.data, streamFieldsResult.data]);

  const queryFetching = timeRangeResult.isFetching
    || logsResult.isFetching
    || hitsResult.isFetching
    || statsResult.isFetching;
  useEffect(() => {
    if (queryFetching && requestStartedAt.current === 0) {
      requestStartedAt.current = performance.now();
    } else if (!queryFetching && requestStartedAt.current > 0) {
      setRequestDuration(performance.now() - requestStartedAt.current);
      requestStartedAt.current = 0;
    }
  }, [queryFetching]);

  const cancel = useCallback(() => {
    workspace.send({ type: "cancelFinite" });
  }, [workspace]);

  const requestFieldValues = useCallback((field: string) => {
    const activeFilters = executed?.filters ?? filters;
    const fieldFilters = buildLogsExtraFilterParams(
      activeFilters.filter((filter) => filter.field !== field),
    );
    const requestKey = JSON.stringify([metadataIdentity, field]);
    fieldValueRequestKeys.current[field] = requestKey;
    setFieldValuesLoading((current) => ({ ...current, [field]: true }));
    setFieldValuesErrors((current) => ({ ...current, [field]: null }));
    void queryClient.fetchQuery({
      queryKey: [
        "logs-explorer",
        project,
        "stream-field-values",
        metadataQuery,
        metadataRange,
        activeFilters,
        field,
      ],
      staleTime: 30_000,
      queryFn: ({ signal }) => queryLogStreamFieldValues({
        field,
        query: metadataQuery,
        start: metadataRange.start,
        end: metadataRange.end,
        limit: 1_000,
        project,
        ...fieldFilters,
      }, signal),
    }).then((response) => {
      if (fieldValueRequestKeys.current[field] !== requestKey) return;
      setFieldValues((current) => ({ ...current, [field]: response.values }));
    }).catch((error: unknown) => {
      if (fieldValueRequestKeys.current[field] !== requestKey) return;
      setFieldValuesErrors((current) => ({
        ...current,
        [field]: errorMessage(error),
      }));
    }).finally(() => {
      if (fieldValueRequestKeys.current[field] !== requestKey) return;
      setFieldValuesLoading((current) => ({ ...current, [field]: false }));
    });
  }, [
    executed?.filters,
    filters,
    metadataIdentity,
    metadataQuery,
    metadataRange,
    project,
    queryClient,
  ]);

  const completeQuery = useCallback(async (
    context: LogsAutocompleteContext,
    signal: AbortSignal,
    currentQuery: string,
  ): Promise<LogsQLEditorSuggestion[]> => {
    if (context.kind === "pipe") return [];
    if (context.kind === "field") {
      return allFields.map((value) => ({
        value,
        kind: "field" as const,
        detail: t("logs.fieldSuggestion"),
      }));
    }
    const autocompleteQuery = logsAutocompleteBaseQuery(currentQuery, context);
    const values = await queryClient.fetchQuery({
      queryKey: [
        "logs-explorer",
        project,
        "autocomplete-values",
        metadataIdentity,
        autocompleteQuery,
        context.field,
        context.token,
      ],
      staleTime: 30_000,
      queryFn: ({ signal: querySignal }) => queryLogFieldValues({
        field: context.field,
        query: autocompleteQuery,
        start: metadataRange.start,
        end: metadataRange.end,
        filter: context.token,
        limit: 100,
        project,
        ...filtersForRequest,
      }, AbortSignal.any([signal, querySignal])),
    });
    return values.values.map((item) => {
      const escaped = item.value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
      return {
        value: context.quoted ? escaped : `"${escaped}"`,
        label: item.value,
        kind: "value" as const,
        detail: t("logs.valueHits", { count: item.hits }),
      };
    });
  }, [
    allFields,
    filtersForRequest,
    metadataIdentity,
    metadataRange,
    project,
    queryClient,
    t,
  ]);

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
    applyFilters(filters.filter((filter) => filter.field !== field));
  }, [applyFilters, filters]);

  const changeRange = useCallback((next: MetricsRange) => {
    workspace.send({ type: "rangeChanged", range: next });
  }, [workspace]);

  const selectChartRange = useCallback((startMs: number, endMs: number) => {
    changeRange({ mode: "absolute", startMs, endMs });
  }, [changeRange]);

  const download = useCallback(async (
    format: "jsonl" | "csv",
    filenameBase: string,
  ) => {
    if (!executed || !effectiveRange) return;
    setDownloadError("");
    const filename = `${filenameBase}.${format}`;
    const picker = (window as LogsFilePickerWindow).showSaveFilePicker;
    if (!picker && !window.confirm(t("logs.downloadFallbackConfirm"))) return;
    const controller = new AbortController();
    downloadController.current?.abort();
    downloadController.current = controller;
    setDownloading(true);
    let writable: WritableStream<Uint8Array> | null = null;
    try {
      if (picker) {
        const handle = await picker.call(window, {
          suggestedName: filename,
          types: [{
            description: format === "csv" ? "CSV" : "JSON Lines",
            accept: format === "csv"
              ? { "text/csv": [".csv"] }
              : { "application/x-ndjson": [".jsonl"] },
          }],
        });
        writable = await handle.createWritable();
      }
      const response = await exportLogs({
        query: executed.query,
        start: effectiveRange.start,
        end: effectiveRange.end,
        format,
        project: executed.project,
        ...filtersForRequest,
      }, controller.signal);
      if (writable) {
        if (!response.body) throw new Error(t("logs.downloadMissingBody"));
        await response.body.pipeTo(writable, { signal: controller.signal });
        writable = null;
      } else {
        const blob = await response.blob();
        const url = URL.createObjectURL(blob);
        const anchor = document.createElement("a");
        anchor.href = url;
        anchor.download = filename;
        document.body.appendChild(anchor);
        anchor.click();
        anchor.remove();
        URL.revokeObjectURL(url);
      }
    } catch (error) {
      if (writable && !writable.locked) {
        await writable.abort(error).catch(() => undefined);
      }
      if (!isAbortError(error)) setDownloadError(errorMessage(error));
    } finally {
      if (downloadController.current === controller) {
        downloadController.current = null;
        setDownloading(false);
      }
    }
  }, [effectiveRange, executed, filtersForRequest, t]);

  const cancelDownload = useCallback(() => {
    downloadController.current?.abort();
  }, []);

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

  const rows = logsResult.data ?? [];
  const selectedChartError = chart.graphMode === "hits" ? hitsResult.error : statsResult.error;
  const chartError = timeRangeResult.error ?? selectedChartError;
  const chartRetrySlot = timeRangeResult.error ? "timeRange" : chart.graphMode;
  const chartLoading = timeRangeResult.isLoading || (chart.graphMode === "hits"
    ? hitsResult.isLoading
    : statsResult.isLoading);
  const chartFetching = timeRangeResult.isFetching || (chart.graphMode === "hits"
    ? hitsResult.isFetching
    : statsResult.isFetching);
  const selectedChartData = chart.graphMode === "hits" ? hitsResult.data : statsResult.data;
  const chartViewState = queryViewState({
    hasData: selectedChartData !== null,
    isLoading: chartLoading,
    isError: Boolean(chartError),
    isEmpty: chartSeries.length === 0,
  });
  const rowsViewState = queryViewState({
    hasData: logsResult.data !== null,
    isLoading: timeRangeResult.isLoading || logsResult.isLoading,
    isError: Boolean(logsResult.error || timeRangeResult.error),
    isEmpty: rows.length === 0,
  });
  const totalHits = chartSeries.reduce((sum, series) => sum + series.total, 0);

  return (
    <PageContainer>
      <PageActions>
        <Button
          variant={sidebarOpen ? "secondary" : "outline"}
          size="sm"
          onClick={() => setSidebarOpen((open) => !open)}
        >
          <Filter />
          {t("logs.streamFields")}
        </Button>
        {IS_MOCK && (
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("logs.mockBadge")}
          </Badge>
        )}
      </PageActions>

      <div className="flex items-start gap-4">
        <LogsFilterSidebar
          open={sidebarOpen}
          fields={streamFieldsResult.data?.values ?? []}
          fieldsLoading={streamFieldsResult.isFetching}
          fieldsError={streamFieldsResult.error ? errorMessage(streamFieldsResult.error) : null}
          fieldValues={fieldValues}
          fieldValuesLoading={fieldValuesLoading}
          fieldValuesErrors={fieldValuesErrors}
          filters={filters.filter((filter) => filter.isStream)}
          onRequestFieldValues={requestFieldValues}
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
          onClose={() => setSidebarOpen(false)}
        />

        <div className="min-w-0 flex-1 space-y-4">
          <Card className="overflow-visible">
            <CardContent className="space-y-3 p-4">
              <div className="flex items-start gap-2">
                <LogsQLEditor
                  id="logs-query"
                  value={queryText}
                  autocompleteEnabled={autocomplete}
                  onChange={(value) => {
                    setQueryText(value);
                    setHistoryCursor(-1);
                  }}
                  onRun={() => queryFetching ? cancel() : execute()}
                  onComplete={completeQuery}
                  onHistoryPrevious={historyPrevious}
                  onHistoryNext={historyNext}
                />
                <Button
                  className="h-10 shrink-0"
                  variant={queryFetching ? "destructive" : "default"}
                  disabled={!queryText.trim() || limit < 1 || limit > MAX_LOGS_LIMIT}
                  onClick={() => queryFetching ? cancel() : execute()}
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
                  onRangeChange={changeRange}
                  onLocalTimeChange={setLocalTime}
                  onRefreshSecondsChange={setRefreshSeconds}
                  onRefresh={refresh}
                />
                <div className="flex flex-wrap items-center gap-2">
                  <Label className="flex items-center gap-2 text-xs font-normal text-muted-foreground">
                    {t("logs.autocomplete")}
                    <Switch checked={autocomplete} onCheckedChange={setAutocomplete} />
                  </Label>
                  <LogsLimitInput value={limit} onChange={setLimit} />
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => workspace.send({ type: "reset" })}
                  >
                    <RotateCcw />
                    {t("common.reset")}
                  </Button>
                  <LogsExamplesMenu onSelect={setQueryText} />
                  <LogsHistoryMenu
                    history={history}
                    onLoad={setQueryText}
                    onRun={execute}
                    onFavorite={(query) => workspace.send({
                      type: "historyFavoriteToggled",
                      query,
                    })}
                    onRemove={(query) => workspace.send({
                      type: "historyRemoved",
                      query,
                    })}
                    onClear={() => workspace.send({ type: "historyCleared" })}
                  />
                </div>
              </div>

              <ExtraFiltersBar
                filters={filters}
                onRemoveFilter={removeFilter}
                onClearFilters={() => applyFilters([])}
              />

              {timeRangeResult.data?.hasTimeFilter && (
                <div className="flex items-start gap-2 rounded-md border border-primary/30 bg-primary/5 px-3 py-2 text-xs">
                  <Search className="mt-0.5 size-3.5 shrink-0 text-primary" />
                  <span>
                    {t("logs.queryTimeOverride", {
                      start: dayjs(parseLogsTimestamp(timeRangeResult.data.start).ms)
                        .format("YYYY-MM-DD HH:mm:ss"),
                      end: dayjs(parseLogsTimestamp(timeRangeResult.data.end).ms)
                        .format("YYYY-MM-DD HH:mm:ss"),
                    })}
                  </span>
                </div>
              )}
            </CardContent>
          </Card>

          {!chart.hidden && (
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
                  onChange={(patch) => setChart((current) => ({ ...current, ...patch }))}
                />
              </CardHeader>
              <CardContent className="p-4 pt-2">
                {!executed ? (
                  <ExplorerMessage
                    icon={<BarChart3 />}
                    title={t("logs.initialChartTitle")}
                    description={t("logs.initialChartDesc")}
                  />
                ) : chartViewState === "pending" ? (
                  <Skeleton className="h-[280px] w-full" />
                ) : chartViewState === "error" ? (
                  <ExplorerError
                    title={t("logs.chartError")}
                    onRetry={() => workspace.send({
                      type: "retry",
                      slot: chartRetrySlot,
                    })}
                  />
                ) : chartViewState === "empty" ? (
                  <ExplorerMessage
                    icon={<Database />}
                    title={t("logs.emptyChart")}
                    description={t("logs.emptyChartDesc")}
                  />
                ) : (
                  <>
                    {chartViewState === "stale" && (
                      <QueryStaleNotice
                        isRetrying={chartFetching}
                        onRetry={() => workspace.send({ type: "retry", slot: chartRetrySlot })}
                      />
                    )}
                    <LogsHitsChart
                      series={chartSeries}
                      height={280}
                      stacked={chart.stacked}
                      cumulative={chart.cumulative}
                      onSelectRange={selectChartRange}
                      onFilterSeries={(fields) => {
                      let next = filters;
                      for (const [field, value] of Object.entries(fields)) {
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
                  </>
                )}
              </CardContent>
            </Card>
          )}

          {chart.hidden && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setChart((current) => ({ ...current, hidden: false }))}
            >
              <Eye />
              {t("logs.showChart")}
            </Button>
          )}

          <Card>
            <CardHeader className="flex-row flex-wrap items-center justify-between gap-2 p-4 pb-3">
              <Tabs
                value={view}
                onValueChange={(value) => setView(value as LogsViewMode)}
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
                    onClick={() => setHideLogs((hidden) => !hidden)}
                  >
                    {hideLogs ? <Eye /> : <EyeOff />}
                    {hideLogs ? t("logs.showLogs") : t("logs.hideLogs")}
                  </Button>
                )}
                <LogsDownloadMenu
                  disabled={!executed}
                  downloading={downloading}
                  project={project}
                  start={effectiveRange?.start}
                  end={effectiveRange?.end}
                  onCancel={cancelDownload}
                  onDownload={(format, filename) => void download(format, filename)}
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
                  rows={liveRows}
                  stopped={liveStopped}
                  following={liveFollowing}
                  connected={liveConnected}
                  reconnecting={liveReconnecting}
                  rawJson={liveRaw}
                  offset={liveOffset}
                  limit={liveLimit}
                  rate={liveRate}
                  dropped={liveDropped}
                  error={liveError}
                  onStoppedChange={(stopped) => {
                    workspace.send({ type: stopped ? "liveStopped" : "liveResumed" });
                    if (!stopped) setLiveFollowing(true);
                  }}
                  onFollowingChange={setLiveFollowing}
                  onRawJsonChange={setLiveRaw}
                  onOffsetChange={setLiveOffset}
                  onLimitChange={setLiveLimit}
                  onRetry={() => workspace.send({ type: "liveRetried" })}
                  onClear={() => workspace.send({ type: "liveCleared" })}
                  onOpenRow={(row) => {
                    selectedProject.current = project;
                    setSelected(row);
                    setDrawerOpen(true);
                  }}
                />
              ) : hideLogs ? (
                <ExplorerMessage
                  icon={<Pause />}
                  title={t("logs.logsHidden")}
                  description={t("logs.logsHiddenDesc")}
                />
              ) : !executed ? (
                <ExplorerMessage
                  icon={<Search />}
                  title={t("logs.initialTitle")}
                  description={t("logs.initialDesc")}
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
                  onRetry={() => workspace.send({
                    type: "retry",
                    slot: timeRangeResult.error ? "timeRange" : "rows",
                  })}
                />
              ) : rowsViewState === "empty" ? (
                <ExplorerMessage
                  icon={<Database />}
                  title={t("logs.emptyTitle")}
                  description={t("logs.emptyDesc")}
                />
              ) : (
                <>
                  {rowsViewState === "stale" && (
                    <QueryStaleNotice
                      isRetrying={logsResult.isFetching || timeRangeResult.isFetching}
                      onRetry={() => workspace.send({
                        type: "retry",
                        slot: timeRangeResult.error ? "timeRange" : "rows",
                      })}
                    />
                  )}
                  <div className="mb-2 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span>{t("logs.matchedLines", { count: rows.length })}</span>
                    {logsResult.isFetching && (
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
                      onSettingsChange={setGroupSettings}
                      onIncludeField={(field, value) => addFilter(field, value, "=")}
                      onExcludeField={(field, value) => addFilter(field, value, "!=")}
                      onViewTrace={(traceId) => viewTrace(traceId)}
                      onShowContext={(row) => setContextAnchor({ project, row })}
                    />
                  )}
                  {view === "table" && (
                    <LogsTableView
                      rows={rows}
                      settings={tableSettings}
                      onSettingsChange={setTableSettings}
                      onOpenRow={(row) => {
                        selectedProject.current = project;
                        setSelected(row);
                        setDrawerOpen(true);
                      }}
                      onViewTrace={(traceId) => viewTrace(traceId)}
                    />
                  )}
                  {view === "json" && <LogsJsonView rows={rows} />}
                </>
              )}
            </CardContent>
          </Card>
        </div>
      </div>

      <LogDetailDrawer
        row={selectedProject.current === project ? selected : null}
        open={drawerOpen && selectedProject.current === project}
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

function ExplorerMessage({
  icon,
  title,
  description,
}: {
  icon: React.ReactNode;
  title: string;
  description: string;
}) {
  return (
    <div className="flex min-h-40 flex-col items-center justify-center gap-2 px-4 text-center">
      <div className="flex size-10 items-center justify-center rounded-full bg-muted text-muted-foreground [&>svg]:size-5">
        {icon}
      </div>
      <div className="text-sm font-medium">{title}</div>
      <p className="max-w-lg text-xs text-muted-foreground">{description}</p>
    </div>
  );
}

function ExplorerError({
  title,
  onRetry,
}: {
  title: string;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  return (
    <div
      className={cn(
        "flex min-h-40 flex-col items-center justify-center gap-2 rounded-md",
        "border border-destructive/30 bg-destructive/5 px-4 text-center",
      )}
    >
      <AlertCircle className="size-5 text-destructive" />
      <div className="text-sm font-medium">{title}</div>
      <p className="max-w-2xl break-words text-xs text-muted-foreground">
        {t("common.queryErrorDesc")}
      </p>
      <Button variant="outline" size="sm" onClick={onRetry}>
        {t("common.retry")}
      </Button>
    </div>
  );
}
