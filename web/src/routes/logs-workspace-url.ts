import {
  DEFAULT_LOGS_LIMIT,
  DEFAULT_LOGS_QUERY,
  DEFAULT_LOGS_RANGE_SECONDS,
  MAX_LOGS_LIMIT,
  logsRangeFromSearch,
  logsRangeToSearch,
  parseLogsFilters,
  serializeLogsFilters,
} from "../lib/logs-explorer.ts";
import {
  formatPrometheusDuration,
  parsePrometheusDuration,
} from "../lib/metrics-query.ts";
import type {
  ExecutedLogsQuery,
  LogsExplorationIntent,
  LogsHistoricalView,
} from "./logs-workspace-types.ts";

export function boundedInteger(
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

export function normalizedLiveOffset(value: string | null): string {
  const seconds = boundedInteger(value, 5, 1, 3_600);
  return `${seconds}s`;
}

export function relativeLocation(value: string): string {
  const url = new URL(value, "https://lazy-observe.invalid");
  return `${url.pathname}${url.search}${url.hash}`;
}

export function parseLogsWorkspaceIntent(
  project: string,
  location: string,
): LogsExplorationIntent {
  const url = new URL(location, "https://lazy-observe.invalid");
  const params = url.searchParams;
  const draft = params.get("q")?.trim() || DEFAULT_LOGS_QUERY;
  const rawView = params.get("view");
  const historicalView: LogsHistoricalView = rawView === "table" || rawView === "json"
    ? rawView
    : "group";
  return {
    project,
    draft,
    range: logsRangeFromSearch({
      range: params.get("range") ?? undefined,
      start: params.get("start") ?? undefined,
      end: params.get("end") ?? undefined,
    }),
    filters: parseLogsFilters(params.get("filters")),
    limit: boundedInteger(params.get("limit"), DEFAULT_LOGS_LIMIT, 1, MAX_LOGS_LIMIT),
    refreshSeconds: boundedInteger(params.get("refresh"), 0, 0, 3_600),
    mode: rawView === "live" ? "live" : "historical",
    historicalView,
    liveOffset: normalizedLiveOffset(params.get("live_tailing_offset")),
    liveLimit: boundedInteger(params.get("rows_per_page"), 100, 10, 50_000),
  };
}

export function locationRequestsExecution(location: string): boolean {
  const value = new URL(location, "https://lazy-observe.invalid").searchParams.get("run");
  return value === "1" || value === "true";
}

export function buildCanonicalLogsLocation(
  currentLocation: string,
  intent: LogsExplorationIntent,
  run: boolean,
): string {
  const url = new URL(currentLocation, "https://lazy-observe.invalid");
  const params = url.searchParams;
  const setOptional = (key: string, value: string | number | undefined) => {
    if (value === undefined || value === "") params.delete(key);
    else params.set(key, String(value));
  };
  const range = logsRangeToSearch(intent.range);
  const draft = intent.draft || DEFAULT_LOGS_QUERY;
  const defaultRange = intent.range.mode === "relative"
    && intent.range.seconds === DEFAULT_LOGS_RANGE_SECONDS;
  setOptional("q", draft === DEFAULT_LOGS_QUERY ? undefined : draft);
  setOptional("run", run ? "1" : undefined);
  setOptional("range", defaultRange ? undefined : range.range);
  setOptional("start", range.start);
  setOptional("end", range.end);
  setOptional("limit", intent.limit === DEFAULT_LOGS_LIMIT
    ? undefined
    : intent.limit);
  setOptional("filters", serializeLogsFilters(intent.filters));
  setOptional("refresh", intent.refreshSeconds || undefined);
  setOptional("view", intent.mode === "live"
    ? "live"
    : intent.historicalView === "group"
      ? undefined
      : intent.historicalView);
  const offsetSeconds = parsePrometheusDuration(intent.liveOffset);
  setOptional("live_tailing_offset", offsetSeconds === 5 ? undefined : offsetSeconds ?? 5);
  if (intent.mode === "live") {
    setOptional("rows_per_page", intent.liveLimit === 100
      ? undefined
      : intent.liveLimit);
  }
  return `${url.pathname}?${params.toString()}${url.hash}`.replace("?#", "#");
}

export function executedMatchesIntent(
  executed: ExecutedLogsQuery | null,
  intent: LogsExplorationIntent,
): boolean {
  return Boolean(
    executed
    && executed.query === intent.draft.trim()
    && executed.limit === intent.limit
    && JSON.stringify(executed.range) === JSON.stringify(intent.range)
    && serializeLogsFilters(executed.filters) === serializeLogsFilters(intent.filters),
  );
}

export function intentBehaviorSignature(
  intent: LogsExplorationIntent,
  run: boolean,
): string {
  return JSON.stringify({
    draft: intent.draft,
    range: intent.range,
    filters: intent.filters,
    limit: intent.limit,
    mode: intent.mode,
    liveOffset: intent.mode === "live" ? intent.liveOffset : undefined,
    liveLimit: intent.mode === "live" ? intent.liveLimit : undefined,
    run,
  });
}

export function hasQueryTimeFilter(query: string): boolean {
  return /(?:^|[\s(])_time\s*:/.test(query);
}

export function timestampSeconds(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value)) {
    const absolute = Math.abs(value);
    return absolute >= 1e15 ? value / 1e9 : absolute >= 1e11 ? value / 1e3 : value;
  }
  if (typeof value !== "string" || !value.trim()) return null;
  const numeric = Number(value);
  if (Number.isFinite(numeric)) return timestampSeconds(numeric);
  const milliseconds = Date.parse(value);
  return Number.isFinite(milliseconds) ? milliseconds / 1_000 : null;
}

export function logsLiveOffsetLabel(seconds: number): string {
  return formatPrometheusDuration(Math.max(1, Math.floor(seconds)));
}
