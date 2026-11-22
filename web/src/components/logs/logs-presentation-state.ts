import {
  DEFAULT_LOGS_LIMIT,
  DEFAULT_LOGS_QUERY,
  MAX_LOGS_LIMIT,
  logsRangeFromSearch,
  parseLogsFilters,
  type LogsExtraFilter,
  type LogsGraphMode,
  type LogsViewMode,
} from "@/lib/logs-explorer";
import type { MetricsRange } from "@/lib/metrics-query";
import {
  createDefaultLogsGroupViewSettings,
  createDefaultLogsTableViewSettings,
  type LogsGroupViewSettings,
  type LogsTableViewSettings,
} from "./logs-explorer-model";
import type { LogsChartOptionsValue } from "./logs-query-controls";

export interface InitialLogsState {
  query: string;
  range: MetricsRange;
  limit: number;
  view: LogsViewMode;
  filters: LogsExtraFilter[];
  refreshSeconds: number;
  autocomplete: boolean;
  hideLogs: boolean;
  chart: LogsChartOptionsValue;
  groupSettings: LogsGroupViewSettings;
  tableSettings: LogsTableViewSettings;
  liveLimit: number;
  liveOffset: string;
  shouldRun: boolean;
}

export interface LogsPresentationUrlState {
  autocomplete: boolean;
  hideLogs: boolean;
  chart: LogsChartOptionsValue;
  groupSettings: LogsGroupViewSettings;
  tableSettings: LogsTableViewSettings;
  view: LogsViewMode;
}

const LOCAL_TIME_KEY = "lo-logs-local-time";
const LIVE_RAW_KEY = "lo-logs-live-raw";

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

export function queryColumnWidths(value: string | null): Record<string, number> {
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

export function parseLogsPresentationState(locationHref: string): InitialLogsState {
  const params = new URL(locationHref, "https://lazy-observe.invalid").searchParams;
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

export function writeLogsPresentationParams(
  currentHref: string,
  {
    autocomplete,
    hideLogs,
    chart,
    groupSettings,
    tableSettings,
    view,
  }: LogsPresentationUrlState,
): string {
  const url = new URL(currentHref, "https://lazy-observe.invalid");
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
  return `${url.pathname}${url.search}${url.hash}`;
}

export function readLocalTimePreference(): boolean {
  try {
    return localStorage.getItem(LOCAL_TIME_KEY) !== "false";
  } catch {
    return true;
  }
}

export function writeLocalTimePreference(localTime: boolean) {
  try {
    localStorage.setItem(LOCAL_TIME_KEY, String(localTime));
  } catch {
    // The in-memory preference remains usable without browser storage.
  }
}

export function readLiveRawPreference(): boolean {
  try {
    return localStorage.getItem(LIVE_RAW_KEY) === "true";
  } catch {
    return false;
  }
}

export function writeLiveRawPreference(liveRaw: boolean) {
  try {
    localStorage.setItem(LIVE_RAW_KEY, String(liveRaw));
  } catch {
    // The in-memory preference remains usable without browser storage.
  }
}
