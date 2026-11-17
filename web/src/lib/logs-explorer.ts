import dayjs from "./dayjs.ts";
import {
  formatPrometheusDuration,
  parsePrometheusDuration,
  type MetricsRange,
} from "./metrics-query.ts";
import type {
  LogsHitSeries,
  LogsStatsSeries,
} from "./api/logs-contract.ts";

export type LogsViewMode = "group" | "table" | "json" | "live";
export type LogsGraphMode = "hits" | "stats";
export type LogsExtraFilterOperator = "=" | "!=" | "=~" | "!~";

export interface LogsExtraFilter {
  field: string;
  value: string;
  operator: LogsExtraFilterOperator;
  isStream: boolean;
}

export interface LogsExtraFilterParams {
  extraFilters: string[];
  extraStreamFilters: string[];
}

export interface LogsHistoryEntry {
  query: string;
  lastRunAt: number;
  favorite: boolean;
}

export interface LogsStatsChartSeries extends LogsHitSeries {
  label?: string;
}

export interface LogsQueryExample {
  id: string;
  category: "quickStart" | "recent" | "aggregation" | "formatting" | "timeSeries";
  query: string;
  titleKey: string;
  descriptionKey: string;
}

export type LogsAutocompleteContext =
  | { kind: "pipe"; token: string; from: number; to: number }
  | { kind: "field"; token: string; from: number; to: number }
  | {
    kind: "value";
    field: string;
    token: string;
    from: number;
    to: number;
    quoted: boolean;
  };

export const DEFAULT_LOGS_QUERY = "*";
export const DEFAULT_LOGS_LIMIT = 50;
export const MAX_LOGS_LIMIT = 10_000;
export const LOGS_LIMIT_CONFIRM_THRESHOLD = 1_000;
export const DEFAULT_LOGS_RANGE_SECONDS = 60 * 60;
export const DEFAULT_LOGS_ROWS_PER_PAGE = 100;
export const MAX_LOGS_HISTORY = 25;
export const LOGS_REFRESH_OPTIONS = [0, 15, 30, 60, 300, 900, 1800] as const;

export const LOGSQL_PIPES = [
  "blocks_count",
  "collapse_nums",
  "copy",
  "delete",
  "drop_empty_fields",
  "extract",
  "extract_regexp",
  "field_names",
  "fields",
  "filter",
  "first",
  "format",
  "hash",
  "join",
  "keep",
  "last",
  "limit",
  "math",
  "offset",
  "pack_json",
  "pack_logfmt",
  "rename",
  "replace",
  "replace_regexp",
  "running_stats",
  "sort",
  "stats",
  "stream_context",
  "top",
  "total_stats",
  "uniq",
  "unpack_json",
  "unpack_logfmt",
  "unroll",
] as const;

export const LOGS_QUERY_EXAMPLES: LogsQueryExample[] = [
  {
    id: "all",
    category: "quickStart",
    query: "*",
    titleKey: "logs.exampleAllTitle",
    descriptionKey: "logs.exampleAllDesc",
  },
  {
    id: "errors",
    category: "quickStart",
    query: 'error OR level:in("error", "fatal")',
    titleKey: "logs.exampleErrorsTitle",
    descriptionKey: "logs.exampleErrorsDesc",
  },
  {
    id: "recent-errors",
    category: "recent",
    query: '_time:15m AND (error OR level:="error")',
    titleKey: "logs.exampleRecentErrorsTitle",
    descriptionKey: "logs.exampleRecentErrorsDesc",
  },
  {
    id: "count-level",
    category: "aggregation",
    query: "* | stats by (level) count() as logs",
    titleKey: "logs.exampleCountLevelTitle",
    descriptionKey: "logs.exampleCountLevelDesc",
  },
  {
    id: "top-services",
    category: "aggregation",
    query: "* | top 10 by (service.name)",
    titleKey: "logs.exampleTopServicesTitle",
    descriptionKey: "logs.exampleTopServicesDesc",
  },
  {
    id: "format-http",
    category: "formatting",
    query: '* | format "<http_method> <http_path> — <_msg>" as _msg',
    titleKey: "logs.exampleFormatTitle",
    descriptionKey: "logs.exampleFormatDesc",
  },
  {
    id: "rate",
    category: "timeSeries",
    query: "* | stats by (_time:1m) count() as logs",
    titleKey: "logs.exampleRateTitle",
    descriptionKey: "logs.exampleRateDesc",
  },
];

const HISTORY_PREFIX = "lo-logs-explorer-history:";

function storageKey(project: string): string {
  return `${HISTORY_PREFIX}${project || "default"}`;
}

function validHistoryEntry(value: unknown): LogsHistoryEntry | null {
  if (!value || typeof value !== "object") return null;
  const entry = value as Partial<LogsHistoryEntry>;
  if (typeof entry.query !== "string" || !entry.query.trim()) return null;
  return {
    query: entry.query.trim(),
    lastRunAt: Number.isFinite(entry.lastRunAt) ? Number(entry.lastRunAt) : 0,
    favorite: entry.favorite === true,
  };
}

export function getLogsExplorerHistory(project: string): LogsHistoryEntry[] {
  try {
    const raw = localStorage.getItem(storageKey(project));
    if (raw) {
      const parsed = JSON.parse(raw);
      if (!Array.isArray(parsed)) return [];
      return parsed
        .map(validHistoryEntry)
        .filter((entry): entry is LogsHistoryEntry => entry !== null)
        .slice(0, MAX_LOGS_HISTORY);
    }
    return [];
  } catch {
    return [];
  }
}

function saveLogsHistory(project: string, entries: LogsHistoryEntry[]): LogsHistoryEntry[] {
  const next = entries.slice(0, MAX_LOGS_HISTORY);
  try {
    localStorage.setItem(storageKey(project), JSON.stringify(next));
  } catch {
    /* Browser storage is optional. */
  }
  return next;
}

export function pushLogsExplorerHistory(
  project: string,
  query: string,
  now = dayjs().valueOf(),
): LogsHistoryEntry[] {
  const normalized = query.trim();
  if (!normalized) return getLogsExplorerHistory(project);
  const current = getLogsExplorerHistory(project);
  const previous = current.find((entry) => entry.query === normalized);
  return saveLogsHistory(project, [
    { query: normalized, lastRunAt: now, favorite: previous?.favorite ?? false },
    ...current.filter((entry) => entry.query !== normalized),
  ]);
}

export function removeLogsExplorerHistory(
  project: string,
  query: string,
): LogsHistoryEntry[] {
  return saveLogsHistory(
    project,
    getLogsExplorerHistory(project).filter((entry) => entry.query !== query),
  );
}

export function clearLogsExplorerHistory(project: string): LogsHistoryEntry[] {
  return saveLogsHistory(
    project,
    getLogsExplorerHistory(project).filter((entry) => entry.favorite),
  );
}

export function toggleLogsExplorerFavorite(
  project: string,
  query: string,
): LogsHistoryEntry[] {
  return saveLogsHistory(
    project,
    getLogsExplorerHistory(project).map((entry) => (
      entry.query === query ? { ...entry, favorite: !entry.favorite } : entry
    )),
  );
}

export function escapeLogsQLString(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

function buildRegularFilter(
  field: string,
  operator: LogsExtraFilterOperator,
  values: string[],
): string {
  const regex = operator === "=~" || operator === "!~";
  const negative = operator === "!=" || operator === "!~";
  const special = values.filter((value) => value === "*" || value === '""');
  const quoted = values
    .filter((value) => value !== "*" && value !== '""')
    .map((value) => `"${escapeLogsQLString(value)}"`);
  const parts: string[] = [];
  if (quoted.length) {
    parts.push(regex
      ? quoted.map((value) => `${field}:~${value}`).join(" OR ")
      : `${field}:in(${quoted.join(", ")})`);
  }
  if (special.length) {
    parts.push(special.map((value) => `${field}:${value}`).join(" OR "));
  }
  const expression = parts.join(" OR ");
  return negative ? `NOT (${expression})` : expression;
}

function buildTimeFilter(
  field: string,
  operator: LogsExtraFilterOperator,
  values: string[],
): string {
  const negative = operator === "!=" || operator === "!~";
  const expression = values
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => `${field}:${value}`)
    .join(" OR ");
  return negative ? `NOT (${expression})` : expression;
}

function buildStreamFilter(
  field: string,
  operator: LogsExtraFilterOperator,
  values: string[],
): string {
  const regex = operator === "=~" || operator === "!~";
  const negative = operator === "!=" || operator === "!~";
  if (field === "_stream") {
    const expression = values.join(" OR ");
    return negative ? `NOT (${expression})` : expression;
  }
  if (regex) {
    const value = values.map(escapeLogsQLString).join("|");
    return `{${field}${negative ? "!~" : "=~"}"${value}"}`;
  }
  const value = values.map((item) => `"${escapeLogsQLString(item)}"`).join(", ");
  return `{${field} ${negative ? "not_in" : "in"} (${value})}`;
}

export function buildLogsExtraFilterParams(
  filters: readonly LogsExtraFilter[],
): LogsExtraFilterParams {
  const grouped = new Map<string, LogsExtraFilter[]>();
  for (const filter of filters) {
    const field = filter.field.trim();
    if (!field) continue;
    const key = `${field}\u0000${filter.operator}\u0000${filter.isStream ? "1" : "0"}`;
    const values = grouped.get(key) ?? [];
    values.push({ ...filter, field });
    grouped.set(key, values);
  }

  const result: LogsExtraFilterParams = { extraFilters: [], extraStreamFilters: [] };
  for (const values of grouped.values()) {
    const first = values[0];
    const expressionValues = [...new Set(values.map((filter) => filter.value))];
    const expression = first.field === "_time"
      ? buildTimeFilter(first.field, first.operator, expressionValues)
      : first.isStream
        ? buildStreamFilter(first.field, first.operator, expressionValues)
        : buildRegularFilter(first.field, first.operator, expressionValues);
    if (!expression) continue;
    (first.isStream ? result.extraStreamFilters : result.extraFilters).push(expression);
  }
  return result;
}

export function serializeLogsFilters(filters: readonly LogsExtraFilter[]): string | undefined {
  if (!filters.length) return undefined;
  return JSON.stringify(filters.map((filter) => ({
    f: filter.field,
    v: filter.value,
    o: filter.operator,
    s: filter.isStream ? 1 : 0,
  })));
}

export function parseLogsFilters(value: unknown): LogsExtraFilter[] {
  if (typeof value !== "string" || !value) return [];
  try {
    const parsed = JSON.parse(value);
    if (!Array.isArray(parsed)) return [];
    return parsed.slice(0, 100).flatMap((item): LogsExtraFilter[] => {
      if (!item || typeof item !== "object") return [];
      const raw = item as { f?: unknown; v?: unknown; o?: unknown; s?: unknown };
      if (typeof raw.f !== "string" || typeof raw.v !== "string") return [];
      if (!["=", "!=", "=~", "!~"].includes(String(raw.o))) return [];
      return [{
        field: raw.f.slice(0, 256),
        value: raw.v.slice(0, 10_000),
        operator: raw.o as LogsExtraFilterOperator,
        isStream: raw.s === 1 || raw.s === true,
      }];
    });
  } catch {
    return [];
  }
}

export function logsFilterLabel(filter: LogsExtraFilter): string {
  const token = filter.operator === "=" ? "=" : filter.operator;
  return `${filter.field}${token}"${escapeLogsQLString(filter.value)}"`;
}

export function upsertLogsFilter(
  filters: readonly LogsExtraFilter[],
  next: LogsExtraFilter,
): LogsExtraFilter[] {
  const index = filters.findIndex((filter) => (
    filter.field === next.field && filter.value === next.value
  ));
  if (index < 0) return [...filters, next];
  return filters.map((filter, itemIndex) => itemIndex === index ? next : filter);
}

export function analyzeLogsQLContext(query: string, cursor: number): LogsAutocompleteContext {
  const before = query.slice(0, cursor);
  const valueMatch = /([A-Za-z_][\w.-]*)\s*:\s*(?:=|~)?\s*(?:["']([^"']*)|([^\s|(){}]*))$/.exec(before);
  if (valueMatch) {
    const token = valueMatch[2] ?? valueMatch[3] ?? "";
    return {
      kind: "value",
      field: valueMatch[1],
      token,
      from: cursor - token.length,
      to: cursor,
      quoted: valueMatch[2] !== undefined,
    };
  }

  const tokenMatch = /[A-Za-z_][\w.-]*$/.exec(before);
  const token = tokenMatch?.[0] ?? "";
  const pipeIndex = before.lastIndexOf("|");
  const boundaryIndex = Math.max(before.lastIndexOf("\n"), before.lastIndexOf("("));
  return {
    kind: pipeIndex > boundaryIndex ? "pipe" : "field",
    token,
    from: cursor - token.length,
    to: cursor,
  };
}

function isEscaped(query: string, index: number): boolean {
  let backslashes = 0;
  for (let cursor = index - 1; cursor >= 0 && query[cursor] === "\\"; cursor -= 1) {
    backslashes += 1;
  }
  return backslashes % 2 === 1;
}

function lastTopLevelPipe(query: string): number {
  const brackets: string[] = [];
  let quote = "";
  let result = -1;
  const closingBrackets: Record<string, string> = {
    ")": "(",
    "]": "[",
    "}": "{",
  };

  for (let index = 0; index < query.length; index += 1) {
    const character = query[index];
    if (quote) {
      if (character === quote && !isEscaped(query, index)) quote = "";
      continue;
    }
    if ((character === '"' || character === "'" || character === "`") && !isEscaped(query, index)) {
      quote = character;
      continue;
    }
    if (character === "(" || character === "[" || character === "{") {
      brackets.push(character);
      continue;
    }
    const opening = closingBrackets[character];
    if (opening) {
      if (brackets.at(-1) === opening) brackets.pop();
      continue;
    }
    if (character === "|" && brackets.length === 0) result = index;
  }
  return result;
}

function closeAutocompleteBrackets(query: string): string | null {
  const brackets: string[] = [];
  let quote = "";
  const closingBrackets: Record<string, string> = {
    ")": "(",
    "]": "[",
    "}": "{",
  };
  const matchingBrackets: Record<string, string> = {
    "(": ")",
    "[": "]",
    "{": "}",
  };

  for (let index = 0; index < query.length; index += 1) {
    const character = query[index];
    if (quote) {
      if (character === quote && !isEscaped(query, index)) quote = "";
      continue;
    }
    if ((character === '"' || character === "'" || character === "`") && !isEscaped(query, index)) {
      quote = character;
      continue;
    }
    if (matchingBrackets[character]) {
      brackets.push(character);
      continue;
    }
    const opening = closingBrackets[character];
    if (!opening) continue;
    if (brackets.pop() !== opening) return null;
  }

  if (quote) return null;
  const suffix = brackets.reverse().map((opening) => matchingBrackets[opening]).join("");
  return `${query}${suffix}`;
}

export function logsAutocompleteBaseQuery(
  query: string,
  context: LogsAutocompleteContext,
): string {
  let cut = context.from;
  if (context.kind === "value") {
    const beforeToken = query.slice(0, context.from);
    const escapedField = context.field.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const filterStart = new RegExp(
      `${escapedField}\\s*:\\s*(?:=|~)?\\s*(?:["']?)$`,
    ).exec(beforeToken);
    if (filterStart) cut = filterStart.index;
  }

  let prefix = query.slice(0, cut).trim();
  let previous = "";
  while (prefix && prefix !== previous) {
    previous = prefix;
    prefix = prefix
      .replace(/(?:\b(?:AND|OR|NOT)|[|({])\s*$/i, "")
      .trim();
  }

  // A value being completed inside a pipe leaves that pipe without its final
  // argument (for example `* | filter service:="ap`). Keep only the preceding
  // complete pipeline so VictoriaLogs never receives an invalid query.
  const pipeIndex = lastTopLevelPipe(prefix);
  if (context.kind === "value" && pipeIndex >= 0) {
    prefix = prefix.slice(0, pipeIndex).trim();
  }

  const balanced = closeAutocompleteBrackets(prefix);
  return balanced || DEFAULT_LOGS_QUERY;
}

export function logsRangeFromSearch(
  search: { range?: unknown; start?: unknown; end?: unknown },
): MetricsRange {
  const startMs = finiteNumber(search.start);
  const endMs = finiteNumber(search.end);
  const absoluteEndMs = endMs ?? dayjs().valueOf();
  if (
    startMs !== undefined
    && startMs >= 0
    && absoluteEndMs > startMs
  ) {
    return {
      mode: "absolute",
      startMs,
      ...(endMs !== undefined ? { endMs } : {}),
    };
  }
  const rangeText = typeof search.range === "string" ? search.range : "";
  const parsedSeconds = parsePrometheusDuration(rangeText);
  const seconds = parsedSeconds ?? DEFAULT_LOGS_RANGE_SECONDS;
  return {
    mode: "relative",
    presetId: parsedSeconds === null
      ? formatPrometheusDuration(seconds)
      : rangeText || formatPrometheusDuration(seconds),
    seconds,
  };
}

export function logsRangeToSearch(range: MetricsRange): {
  range?: string;
  start?: number;
  end?: number;
} {
  if (range.mode === "relative") {
    return {
      range: range.presetId || formatPrometheusDuration(range.seconds),
    };
  }
  return {
    start: Math.floor(range.startMs),
    ...(range.endMs !== undefined ? { end: Math.floor(range.endMs) } : {}),
  };
}

function finiteNumber(value: unknown): number | undefined {
  const number = typeof value === "number"
    ? value
    : typeof value === "string" && value.trim() ? Number(value) : Number.NaN;
  return Number.isFinite(number) ? number : undefined;
}

function statsSeriesLabel(metric: Record<string, string>): string {
  const name = metric.__name__ ?? "";
  const labels = Object.entries(metric)
    .filter(([field]) => field !== "__name__")
    .map(([field, value]) => `${field}=${JSON.stringify(value)}`)
    .join(", ");
  if (name && labels) return `${name}{${labels}}`;
  return name || labels;
}

export function statsChartSeries(
  result: readonly LogsStatsSeries[],
  fieldsLimit: number,
): LogsStatsChartSeries[] {
  const normalized = result.map((series) => {
    const values = series.values.map((sample) => {
      const value = Number(sample.value);
      return Number.isFinite(value) ? value : 0;
    });
    return {
      label: statsSeriesLabel(series.metric),
      fields: Object.fromEntries(
        Object.entries(series.metric).filter(([field]) => field !== "__name__"),
      ),
      timestamps: series.values.map((sample) => sample.timestamp * 1_000),
      values,
      total: values.reduce((sum, value) => sum + value, 0),
      isOther: false,
    };
  }).sort((left, right) => right.total - left.total);

  const limit = Math.max(1, Math.floor(fieldsLimit));
  const top = normalized.slice(0, limit);
  const rest = normalized.slice(limit);
  if (!rest.length) return top;

  const totalsByTimestamp = new Map<number, number>();
  for (const series of rest) {
    series.timestamps.forEach((timestamp, index) => {
      totalsByTimestamp.set(
        timestamp,
        (totalsByTimestamp.get(timestamp) ?? 0) + (series.values[index] ?? 0),
      );
    });
  }
  const timestamps = [...totalsByTimestamp.keys()].sort((left, right) => left - right);
  const values = timestamps.map((timestamp) => totalsByTimestamp.get(timestamp) ?? 0);
  return [{
    fields: {},
    timestamps,
    values,
    total: values.reduce((sum, value) => sum + value, 0),
    isOther: true,
  }, ...top];
}
