import dayjs from "../lib/dayjs.ts";
import {
  formatPrometheusDuration,
  METRICS_RANGE_PRESETS,
  metricsRangeSeconds,
  parsePrometheusDuration,
  resolveMetricsRange,
  type MetricsGraphMode,
  type MetricsQueryTab,
  type MetricsRange,
  type MetricsResolution,
  type MetricsTableMode,
  type MetricsTimeFormat,
  type ResolvedMetricsRange,
} from "../lib/metrics-query.ts";
import {
  parseMetricsEvidenceSearch,
  type MetricsEvidenceSearch,
} from "./trace-evidence-links.ts";

const DUMMY_ORIGIN = "https://lazy-observe.invalid";

export const DEFAULT_METRICS_RANGE =
  METRICS_RANGE_PRESETS.find((item) => item.presetId === "1h") ?? METRICS_RANGE_PRESETS[0];

export interface MetricsPanelState {
  id: string;
  query: string;
  activeTab: MetricsQueryTab;
  range: MetricsRange;
  resolution: MetricsResolution;
  graphMode: MetricsGraphMode;
  tableMode: MetricsTableMode;
  timeFormat: MetricsTimeFormat;
  yAxisZero: boolean;
}

export interface ExecutedMetricsQuery {
  query: string;
  runId: number;
  /** Time window frozen when the query was executed or its range changed. */
  range: ResolvedMetricsRange;
}

export interface InitialMetricsState {
  panels: MetricsPanelState[];
  executed: Record<string, ExecutedMetricsQuery>;
}

/**
 * Full TanStack Router search schema for `/metrics`: preserves both evidence
 * deep-link parameters (`q`, `run`, `start`, `end`, `evidence`) and indexed
 * panel parameters (`g0.expr`, `g0.tab`, `g0.range_input`, ...).
 */
export type MetricsRouteSearch = MetricsEvidenceSearch & Record<string, unknown>;

const PANEL_KEY_PATTERN = /^g(\d+)\.(expr|tab|range_input|end_input|res_type|res_density|res_step|display_mode|table_mode|time_format|y_axis_min)$/;

export function newMetricsPanel(id: string, query = ""): MetricsPanelState {
  return {
    id,
    query,
    activeTab: "table",
    range: DEFAULT_METRICS_RANGE,
    resolution: { type: "auto", density: "medium" },
    graphMode: "lines",
    tableMode: "labels",
    timeFormat: "readable",
    yAxisZero: false,
  };
}

/** TanStack Router `validateSearch` for `/metrics`. */
export function parseMetricsSearch(search: Record<string, unknown>): MetricsRouteSearch {
  const result: MetricsRouteSearch = { ...parseMetricsEvidenceSearch(search) };
  for (const [key, value] of Object.entries(search)) {
    if (!PANEL_KEY_PATTERN.test(key)) continue;
    if (typeof value === "string") {
      result[key] = value;
    } else if (typeof value === "number" && Number.isFinite(value)) {
      result[key] = String(value);
    }
  }
  return result;
}

/**
 * Hydrate metrics workbench panels exclusively from a TanStack Router location
 * href and validated search params (never reading `window.location.search`).
 */
export function parseMetricsStateFromLocation(
  locationHref: string,
  evidenceSearch?: MetricsEvidenceSearch,
  nowMs = dayjs().valueOf(),
): InitialMetricsState {
  const params = new URL(locationHref, DUMMY_ORIGIN).searchParams;
  const indices = [...new Set([...params.keys()].flatMap((key) => {
    const match = /^g(\d+)\./.exec(key);
    return match ? [Number(match[1])] : [];
  }))].sort((left, right) => left - right);

  const panels: MetricsPanelState[] = [];
  const executed: Record<string, ExecutedMetricsQuery> = {};
  for (const index of indices) {
    const panel = newMetricsPanel(`query-${panels.length + 1}`, params.get(`g${index}.expr`) ?? "");
    const tab = params.get(`g${index}.tab`);
    if (tab === "graph" || tab === "table") panel.activeTab = tab;
    const rangeSeconds = parsePrometheusDuration(params.get(`g${index}.range_input`) ?? "");
    const endInput = params.get(`g${index}.end_input`);
    const endMs = endInput ? dayjs(endInput).valueOf() : Number.NaN;
    if (rangeSeconds !== null) {
      panel.range = Number.isFinite(endMs)
        ? { mode: "absolute", startMs: endMs - rangeSeconds * 1000, endMs }
        : { mode: "relative", presetId: formatPrometheusDuration(rangeSeconds), seconds: rangeSeconds };
    }
    const resType = params.get(`g${index}.res_type`);
    if (resType === "auto") {
      const density = params.get(`g${index}.res_density`);
      if (density === "low" || density === "medium" || density === "high") {
        panel.resolution = { type: "auto", density };
      }
    } else if (resType === "fixed" || resType === "custom") {
      const step = Number(params.get(`g${index}.res_step`));
      if (Number.isFinite(step) && step >= 1) panel.resolution = { type: resType, step };
    }
    panel.graphMode = params.get(`g${index}.display_mode`) === "stacked" ? "stacked" : "lines";
    panel.tableMode = params.get(`g${index}.table_mode`) === "series" ? "series" : "labels";
    panel.timeFormat = params.get(`g${index}.time_format`) === "timestamp"
      ? "timestamp"
      : "readable";
    panel.yAxisZero = params.get(`g${index}.y_axis_min`) === "0";
    panels.push(panel);
    const query = panel.query.trim();
    if (query && params.has(`g${index}.expr`)) {
      executed[panel.id] = {
        query,
        runId: panels.length,
        range: resolveMetricsRange(panel.range, nowMs),
      };
    }
  }
  if (panels.length > 0) return { panels, executed };

  const fallbackQuery = evidenceSearch?.q ?? params.get("q") ?? "";
  const panel = newMetricsPanel("query-1", fallbackQuery);
  const start = evidenceSearch?.start;
  const end = evidenceSearch?.end;
  if (start !== undefined && end !== undefined) {
    panel.range = {
      mode: "absolute",
      startMs: start * 1_000,
      endMs: end * 1_000,
    };
  }
  return { panels: [panel], executed };
}

/**
 * Build the canonical relative URL for the active metrics panels while
 * preserving unrelated search params such as `evidence=contextual`.
 */
export function buildMetricsPanelsURL(
  currentHref: string,
  panels: readonly MetricsPanelState[],
): string {
  const url = new URL(currentHref, DUMMY_ORIGIN);
  for (const key of [...url.searchParams.keys()]) {
    if (/^g\d+\./.test(key) || key === "q" || key === "run" || key === "start" || key === "end") {
      url.searchParams.delete(key);
    }
  }
  panels.forEach((panel, index) => {
    const prefix = `g${index}.`;
    const setOptional = (key: string, value: string | undefined) => {
      if (value === undefined || value === "") url.searchParams.delete(key);
      else url.searchParams.set(key, value);
    };
    const rangeSeconds = metricsRangeSeconds(panel.range);
    const defaultRange = panel.range.mode === "relative" && rangeSeconds === DEFAULT_METRICS_RANGE.seconds;
    setOptional(prefix + "expr", panel.query.trim() ? panel.query : undefined);
    setOptional(prefix + "tab", panel.activeTab === "table" ? undefined : panel.activeTab);
    setOptional(
      prefix + "range_input",
      defaultRange ? undefined : formatPrometheusDuration(rangeSeconds),
    );
    if (panel.range.mode === "absolute" && panel.range.endMs !== undefined) {
      url.searchParams.set(prefix + "end_input", dayjs(panel.range.endMs).toISOString());
    }
    if (panel.resolution.type === "auto" && panel.resolution.density !== "medium") {
      url.searchParams.set(prefix + "res_type", "auto");
      url.searchParams.set(prefix + "res_density", panel.resolution.density);
    } else if (panel.resolution.type !== "auto") {
      url.searchParams.set(prefix + "res_type", panel.resolution.type);
      url.searchParams.set(prefix + "res_step", String(panel.resolution.step));
    }
    setOptional(prefix + "display_mode", panel.graphMode === "lines" ? undefined : panel.graphMode);
    setOptional(prefix + "table_mode", panel.tableMode === "labels" ? undefined : panel.tableMode);
    setOptional(
      prefix + "time_format",
      panel.timeFormat === "readable" ? undefined : panel.timeFormat,
    );
    if (panel.yAxisZero) url.searchParams.set(prefix + "y_axis_min", "0");
  });
  return `${url.pathname}${url.search}${url.hash}`;
}
