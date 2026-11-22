import type {
  LogLevel,
  LogRow,
  LogsExportParams,
  LogsFieldValue,
  LogsFieldValuesParams,
  LogsFieldValuesResponse,
  LogsHistogram,
  LogsHitSeries,
  LogsHitSeriesParams,
  LogsHitsParams,
  LogsMetadataParams,
  LogsQueryParams,
  LogsQueryTimeRange,
  LogsRangeParams,
  LogsRequestFilters,
  LogsStatsQueryRangeResponse,
  LogsStatsRangeParams,
  LogsTailParams,
  LogsTimeValue,
} from "./logs.ts";
import { hexId, makeRng, type Rng } from "./mock-random.ts";
import dayjs from "../dayjs.ts";

/**
 * Offline logs generator (VITE_MOCK=1). Emits a deterministic page of realistic
 * structured log lines across several streams, with a level distribution and a
 * fraction carrying `trace_id`. When the LogsQL query filters on a specific
 * trace_id, every returned row is stamped with it — so a span's "jump to logs"
 * action lands on matching lines.
 */

const STREAMS = [
  { app: "api-gateway", env: "prod", pod: "api-gateway-7c9f", container: "gateway" },
  { app: "orders-service", env: "prod", pod: "orders-6b4d", container: "orders" },
  { app: "auth-service", env: "prod", pod: "auth-5f21", container: "auth" },
  { app: "payments-service", env: "prod", pod: "payments-9a0c", container: "payments" },
  { app: "frontend", env: "prod", pod: "frontend-2de8", container: "web" },
  { app: "worker", env: "staging", pod: "worker-11ab", container: "worker" },
];

const METHODS = ["GET", "POST", "PUT", "DELETE"];
const PATHS = [
  "/api/orders",
  "/api/orders/{id}",
  "/api/checkout",
  "/api/user",
  "/api/auth/token",
  "/api/payments/charge",
  "/healthz",
];

const MESSAGES: Record<LogLevel, string[]> = {
  info: [
    "request completed",
    "handled request",
    "cache hit for session",
    "published event order.created",
    "connection established to postgres",
  ],
  debug: [
    "resolving route handler",
    "acquired db connection from pool",
    "serializing response body",
    "evaluating feature flag checkout_v2",
  ],
  warn: [
    "slow query detected (>500ms)",
    "retrying upstream call after timeout",
    "rate limit approaching for tenant",
    "deprecated API version requested",
  ],
  error: [
    "unhandled exception while processing request",
    "failed to connect to payments upstream",
    "database transaction rolled back",
    "panic recovered in request handler",
  ],
  trace: ["entering span", "exiting span"],
  unknown: ["log line"],
};

const FIELD_NAMES: LogsFieldValue[] = [
  { value: "_msg", hits: 18_420 },
  { value: "_stream", hits: 18_420 },
  { value: "_time", hits: 18_420 },
  { value: "app", hits: 18_420 },
  { value: "container", hits: 18_420 },
  { value: "duration_ms", hits: 17_980 },
  { value: "env", hits: 18_420 },
  { value: "host", hits: 18_420 },
  { value: "http_method", hits: 16_840 },
  { value: "http_path", hits: 16_840 },
  { value: "http_status", hits: 16_840 },
  { value: "level", hits: 18_420 },
  { value: "pod", hits: 18_420 },
  { value: "span_id", hits: 3_316 },
  { value: "trace_id", hits: 3_316 },
];

const STREAM_FIELD_NAMES: LogsFieldValue[] = [
  { value: "app", hits: STREAMS.length },
  { value: "env", hits: 2 },
  { value: "pod", hits: STREAMS.length },
];

/** Weighted level pick: mostly info/debug, some warn, few error. */
function pickLevel(rng: Rng): LogLevel {
  const r = rng.next();
  if (r < 0.62) return "info";
  if (r < 0.8) return "debug";
  if (r < 0.92) return "warn";
  return "error";
}

/** Extract a trace_id the query is filtering on, if any. */
function traceFilter(query: string): string | undefined {
  const m = query.match(/trace_id[:=]\s*"?([0-9a-fA-F]{8,})"?/);
  return m?.[1];
}

function streamLabel(s: (typeof STREAMS)[number]): string {
  return `{app="${s.app}",env="${s.env}",pod="${s.pod}"}`;
}

type FilterRecord = Record<string, unknown>;
type FilterMatch = boolean | undefined;

// This intentionally recognizes only the expressions emitted by
// buildLogsExtraFilterParams. Unknown LogsQL remains a no-op in mock mode.
function hasRequestFilters(params: LogsRequestFilters): boolean {
  return Boolean(params.extraFilters?.length || params.extraStreamFilters?.length);
}

function fieldValue(record: FilterRecord, field: string): string | undefined {
  let value = record[field];
  if (value === undefined && (field === "service" || field === "service.name")) {
    value = record.app;
  }
  if (value === null || value === undefined) return undefined;
  return typeof value === "string" ? value : String(value);
}

function unescapeFilterString(value: string): string {
  let result = "";
  for (let index = 0; index < value.length; index++) {
    const char = value[index];
    if (char === "\\" && index + 1 < value.length) {
      const next = value[index + 1];
      if (next === "\\" || next === '"') {
        result += next;
        index++;
        continue;
      }
    }
    result += char;
  }
  return result;
}

function quotedValues(value: string): string[] {
  const values: string[] = [];
  const matcher = /"((?:\\.|[^"\\])*)"/g;
  let match: RegExpExecArray | null;
  while ((match = matcher.exec(value))) {
    values.push(unescapeFilterString(match[1]));
  }
  return values;
}

function splitTopLevel(value: string, delimiter: "OR" | ","): string[] {
  const parts: string[] = [];
  let start = 0;
  let braces = 0;
  let parentheses = 0;
  let quoted = false;
  let escaped = false;

  for (let index = 0; index < value.length; index++) {
    const char = value[index];
    if (quoted) {
      if (escaped) {
        escaped = false;
      } else if (char === "\\") {
        escaped = true;
      } else if (char === '"') {
        quoted = false;
      }
      continue;
    }
    if (char === '"') {
      quoted = true;
      continue;
    }
    if (char === "{") braces++;
    if (char === "}") braces = Math.max(0, braces - 1);
    if (char === "(") parentheses++;
    if (char === ")") parentheses = Math.max(0, parentheses - 1);
    if (braces || parentheses) continue;

    if (delimiter === "," && char === ",") {
      parts.push(value.slice(start, index).trim());
      start = index + 1;
      continue;
    }
    if (
      delimiter === "OR"
      && value.slice(index, index + 2).toUpperCase() === "OR"
      && (index === 0 || /\s/.test(value[index - 1]))
      && (index + 2 === value.length || /\s/.test(value[index + 2]))
    ) {
      parts.push(value.slice(start, index).trim());
      start = index + 2;
      index++;
    }
  }
  parts.push(value.slice(start).trim());
  return parts.filter(Boolean);
}

function regexMatches(value: string | undefined, pattern: string): FilterMatch {
  if (value === undefined) return false;
  // JavaScript regexes do not have RE2's complexity guarantees. Keep the mock
  // responsive by bounding both operands and rejecting quantified groups, the
  // most common catastrophic-backtracking shape.
  if (pattern.length > 256 || value.length > 1_000 || /\)(?:[*+]|\{\d)/.test(pattern)) {
    return undefined;
  }
  try {
    return new RegExp(pattern).test(value);
  } catch {
    return undefined;
  }
}

function matchStreamSelector(record: FilterRecord, expression: string): FilterMatch {
  if (!expression.startsWith("{") || !expression.endsWith("}")) return undefined;
  const clauses = splitTopLevel(expression.slice(1, -1), ",");
  if (!clauses.length) return undefined;
  let recognized = false;
  for (const clause of clauses) {
    const list = /^([A-Za-z_@][\w.@-]*)\s+(in|not_in)\s*\(([\s\S]*)\)$/i.exec(clause);
    if (list) {
      recognized = true;
      const actual = fieldValue(record, list[1]);
      const contains = actual !== undefined && quotedValues(list[3]).includes(actual);
      if (list[2].toLowerCase() === "in" ? !contains : contains) return false;
      continue;
    }

    const comparison =
      /^([A-Za-z_@][\w.@-]*)\s*(=~|!~|=|!=)\s*"((?:\\.|[^"\\])*)"$/i.exec(clause);
    if (!comparison) return undefined;
    recognized = true;
    const actual = fieldValue(record, comparison[1]);
    const expected = unescapeFilterString(comparison[3]);
    const operator = comparison[2];
    if (operator === "=" && actual !== expected) return false;
    if (operator === "!=" && actual === expected) return false;
    if (operator === "=~" || operator === "!~") {
      const matched = regexMatches(actual, expected);
      if (matched === undefined) return undefined;
      if (operator === "=~" ? !matched : matched) return false;
    }
  }
  return recognized ? true : undefined;
}

function matchRegularFilter(record: FilterRecord, expression: string): FilterMatch {
  const list =
    /^([A-Za-z_@][\w.@-]*)\s*:\s*in\s*\(([\s\S]*)\)$/i.exec(expression);
  if (list) {
    const actual = fieldValue(record, list[1]);
    return actual !== undefined && quotedValues(list[2]).includes(actual);
  }

  const regex =
    /^([A-Za-z_@][\w.@-]*)\s*:\s*~\s*"((?:\\.|[^"\\])*)"$/i.exec(expression);
  if (regex) {
    return regexMatches(fieldValue(record, regex[1]), unescapeFilterString(regex[2]));
  }

  const direct = /^([A-Za-z_@][\w.@-]*)\s*:\s*([\s\S]+)$/.exec(expression);
  if (!direct) return undefined;
  const actual = fieldValue(record, direct[1]);
  const expected = direct[2].trim();
  if (expected === "*") return actual !== undefined && actual !== "";
  if (expected === '""') return actual === undefined || actual === "";
  if (expected.startsWith('"') && expected.endsWith('"')) {
    return actual === unescapeFilterString(expected.slice(1, -1));
  }
  // Relative _time expressions and other LogsQL syntax are outside this small
  // matcher. Ignoring them is safer than pretending that the mock understands
  // the complete query language.
  if (direct[1] === "_time") return undefined;
  return actual === expected;
}

function matchFilterExpression(record: FilterRecord, expression: string): FilterMatch {
  const source = expression.trim();
  const negated = /^NOT\s*\(([\s\S]*)\)$/i.exec(source);
  if (negated) {
    const result = matchFilterExpression(record, negated[1]);
    return result === undefined ? undefined : !result;
  }

  const alternatives = splitTopLevel(source, "OR");
  if (alternatives.length > 1) {
    const results = alternatives.map((alternative) => (
      matchFilterExpression(record, alternative)
    ));
    if (results.some((result) => result === true)) return true;
    return results.some((result) => result === undefined) ? undefined : false;
  }

  return source.startsWith("{")
    ? matchStreamSelector(record, source)
    : matchRegularFilter(record, source);
}

function matchesRequestFilters(record: FilterRecord, params: LogsRequestFilters): boolean {
  const expressions = [
    ...(params.extraFilters ?? []),
    ...(params.extraStreamFilters ?? []),
  ];
  return expressions.every((expression) => (
    matchFilterExpression(record, expression) !== false
  ));
}

function timeValueToMs(value: LogsTimeValue): number {
  const numeric = typeof value === "number" ? value : Number(value);
  if (Number.isFinite(numeric)) {
    if (numeric > 1e15) return numeric / 1e6;
    if (numeric > 1e12) return numeric;
    return numeric * 1000;
  }
  const parsed = dayjs(String(value)).valueOf();
  return Number.isNaN(parsed) ? dayjs().valueOf() : parsed;
}

function mockRange(params: { start: LogsTimeValue; end: LogsTimeValue }): {
  startMs: number;
  endMs: number;
} {
  const startMs = timeValueToMs(params.start);
  const endMs = Math.max(startMs + 1, timeValueToMs(params.end));
  return { startMs, endMs };
}

function makeMockRows(
  query: string,
  startMs: number,
  endMs: number,
  total: number,
  offset = 0,
): LogRow[] {
  const rng = makeRng(`logs|${query}`);
  const filterTrace = traceFilter(query);
  const window = Math.max(1000, endMs - startMs);

  const rows: LogRow[] = [];
  for (let i = 0; i < total; i++) {
    // Newest first; spread across the window with a little jitter.
    const position = i + offset;
    const t = Math.round(
      endMs - (window * position) / (total + offset) - rng.next() * (window / total),
    );
    const stream = rng.pick(STREAMS);
    const level = filterTrace ? weightedForTrace(rng) : pickLevel(rng);
    const method = rng.pick(METHODS);
    const path = rng.pick(PATHS);
    const status =
      level === "error" ? rng.pick([500, 502, 503]) : rng.pick([200, 201, 204, 404]);
    const durationMs = level === "warn" ? rng.int(500, 2400) : rng.int(1, 480);
    const msg = rng.pick(MESSAGES[level]);

    const hasTrace = filterTrace ? true : rng.bool(0.18);
    const traceId = filterTrace ?? (hasTrace ? hexId(`logtrace|${query}|${i}`, 32) : undefined);

    const raw: Record<string, unknown> = {
      _time: dayjs(t).toISOString(),
      _msg: msg,
      _stream: streamLabel(stream),
      level,
      app: stream.app,
      env: stream.env,
      pod: stream.pod,
      container: stream.container,
      http_method: method,
      http_path: path,
      http_status: status,
      duration_ms: durationMs,
      host: `${stream.app}-node-${rng.int(1, 8)}`,
    };
    if (traceId) {
      raw.trace_id = traceId;
      raw.span_id = hexId(`logspan|${query}|${i}`, 16);
    }

    rows.push({
      id: `${t}-${i}`,
      time: t,
      timeRaw: raw._time as string,
      message: msg,
      stream: raw._stream as string,
      level,
      traceId,
      raw,
    });
  }

  rows.sort((a, b) => b.time - a.time);
  return rows;
}

function filteredRangeRows(
  params: LogsRangeParams,
  limit: number,
  offset = 0,
): LogRow[] {
  const requested = Math.max(1, Math.min(limit, 5000));
  const candidateCount = hasRequestFilters(params)
    ? Math.min(5000, Math.max(requested * 6, requested + 192))
    : requested;
  const { startMs, endMs } = mockRange(params);
  return makeMockRows(params.query.trim(), startMs, endMs, candidateCount, offset)
    .filter((row) => matchesRequestFilters(row.raw, params))
    .slice(0, requested);
}

export function mockQueryLogs(
  params: LogsQueryParams,
  signal?: AbortSignal,
): Promise<LogRow[]> {
  const query = params.query.trim();
  if (signal?.aborted) return rejectedAbort(signal);
  if (query.includes("__empty")) return delay([], 250, signal);
  if (query.includes("__error")) {
    return Promise.reject(new Error(`LogsQL parse error near: ${query}`));
  }

  const rows = filteredRangeRows(params, params.limit, Math.max(0, params.offset ?? 0));
  return delay(rows, query.includes("__slow") ? 1_600 : 300, signal);
}

/** When filtering by a trace, weight toward the "interesting" levels. */
function weightedForTrace(rng: Rng): LogLevel {
  const r = rng.next();
  if (r < 0.5) return "info";
  if (r < 0.7) return "debug";
  if (r < 0.88) return "warn";
  return "error";
}

export function mockLogsHits(
  params: LogsHitsParams,
  signal?: AbortSignal,
): Promise<LogsHistogram> {
  const { query, start, end, step } = params;
  if (query.includes("__empty")) {
    return delay({ timestamps: [], values: [] }, 200, signal);
  }
  const rng = makeRng(`hits|${query}`);
  const buckets = Math.max(1, Math.min(Math.floor((end - start) / step), 240));
  const phase = rng.next() * Math.PI * 2;
  const amp = rng.int(30, 120);
  const base = rng.int(40, 160);

  const timestamps: number[] = [];
  const values: number[] = [];
  for (let i = 0; i < buckets; i++) {
    const tSec = start + i * step;
    const wave = Math.sin((i / buckets) * Math.PI * 4 + phase);
    const noise = (rng.next() - 0.5) * base * 0.4;
    timestamps.push(tSec * 1000);
    values.push(Math.max(0, Math.round(base + amp * wave + noise)));
  }
  return delay({ timestamps, values }, 200, signal);
}

function durationSeconds(value: string | number): number {
  if (typeof value === "number") return Math.max(1, value);
  const match = /^\s*(-?\d+(?:\.\d+)?)\s*(ms|s|m|h|d)?\s*$/.exec(value);
  if (!match) return 60;
  const number = Math.abs(Number(match[1]));
  const unit = match[2] ?? "s";
  const multiplier = unit === "ms" ? 0.001
    : unit === "m" ? 60
      : unit === "h" ? 3600
        : unit === "d" ? 86_400
          : 1;
  return Math.max(0.001, number * multiplier);
}

function hitFieldValue(field: string, index: number): string {
  const stream = STREAMS[index % STREAMS.length];
  switch (field) {
    case "app":
    case "service":
    case "service.name":
      return stream.app;
    case "env":
      return stream.env;
    case "pod":
      return stream.pod;
    case "container":
      return stream.container;
    case "level":
      return ["info", "debug", "warn", "error"][index % 4];
    case "http_method":
      return METHODS[index % METHODS.length];
    case "http_status":
      return String([200, 201, 404, 500, 502][index % 5]);
    default:
      return `${field}-${index + 1}`;
  }
}

function makeHitSeries(
  params: LogsHitSeriesParams,
  fields: Record<string, string>,
  seriesIndex: number,
  scale = 1,
): LogsHitSeries {
  const { startMs, endMs } = mockRange(params);
  const stepMs = durationSeconds(params.step) * 1000;
  const bucketCount = Math.max(1, Math.min(Math.ceil((endMs - startMs) / stepMs), 240));
  const rng = makeRng(`hits-series|${params.query}|${JSON.stringify(fields)}`);
  const base = Math.max(2, 90 - seriesIndex * 11);
  const timestamps: number[] = [];
  const values: number[] = [];
  for (let index = 0; index < bucketCount; index++) {
    timestamps.push(Math.round(startMs + index * stepMs));
    const wave = Math.sin((index / bucketCount) * Math.PI * 4 + seriesIndex);
    values.push(Math.max(
      0,
      Math.round((base + base * 0.35 * wave + rng.next() * 12) * scale),
    ));
  }
  return {
    fields,
    timestamps,
    values,
    total: values.reduce((sum, value) => sum + value, 0),
    isOther: Object.keys(fields).length === 0,
  };
}

function filterSample(
  params: LogsRangeParams,
  sampleSize = 480,
): { rows: LogRow[]; ratio: number } {
  const { startMs, endMs } = mockRange(params);
  const candidates = makeMockRows(params.query.trim(), startMs, endMs, sampleSize);
  if (!hasRequestFilters(params)) return { rows: candidates, ratio: 1 };
  const rows = candidates.filter((row) => matchesRequestFilters(row.raw, params));
  return { rows, ratio: rows.length / candidates.length };
}

function groupFieldsFromRows(
  rows: readonly LogRow[],
  requestedFields: readonly string[],
): Record<string, string>[] {
  const fieldsByKey = new Map<string, Record<string, string>>();
  for (const row of rows) {
    const fields: Record<string, string> = {};
    let complete = true;
    for (const field of requestedFields) {
      const value = fieldValue(row.raw, field);
      if (value === undefined) {
        complete = false;
        break;
      }
      fields[field] = value;
    }
    if (!complete) continue;
    const key = requestedFields.map((field) => `${field}\u0000${fields[field]}`).join("\u0001");
    fieldsByKey.set(key, fields);
    if (fieldsByKey.size >= 8) break;
  }
  return [...fieldsByKey.values()];
}

export function mockLogHitsSeries(
  params: LogsHitSeriesParams,
  signal?: AbortSignal,
): Promise<{ hits: LogsHitSeries[] }> {
  if (params.query.includes("__chart_error")) {
    return Promise.reject(new Error(`LogsQL chart error near: ${params.query}`));
  }
  if (params.query.includes("__empty")) return delay({ hits: [] }, 120, signal);
  const sample = filterSample(params);
  if (!sample.rows.length) return delay({ hits: [] }, 160, signal);
  const requestedFields = typeof params.field === "string"
    ? [params.field]
    : [...(params.field ?? [])];
  if (!requestedFields.length) {
    return delay({ hits: [makeHitSeries(params, {}, 0, sample.ratio)] }, 160, signal);
  }

  const sampledFields = groupFieldsFromRows(sample.rows, requestedFields);
  const candidateFields = sampledFields.length
    ? sampledFields
    : Array.from({ length: Math.max(4, Math.min(STREAMS.length, 8)) }, (_, index) => (
        Object.fromEntries(
          requestedFields.map((field) => [field, hitFieldValue(field, index)]),
        )
      ));
  const candidates = candidateFields
    .map((fields, index) => makeHitSeries(params, fields, index, sample.ratio))
    .sort((left, right) => right.total - left.total);

  const limit = params.fieldsLimit && params.fieldsLimit > 0
    ? Math.min(params.fieldsLimit, candidates.length)
    : candidates.length;
  const visible = candidates.slice(0, limit);
  const rest = candidates.slice(limit);
  if (rest.length) {
    const timestamps = rest[0].timestamps;
    const values = timestamps.map((_, bucketIndex) => (
      rest.reduce((sum, series) => sum + (series.values[bucketIndex] ?? 0), 0)
    ));
    visible.unshift({
      fields: {},
      timestamps,
      values,
      total: values.reduce((sum, value) => sum + value, 0),
      isOther: true,
    });
  }
  return delay({ hits: visible }, 160, signal);
}

function statsGroupFields(query: string): string[] {
  const match = /\bby\s*\(([^)]+)\)/i.exec(query);
  if (!match) return [];
  return match[1].split(",").map((field) => field.trim()).filter(Boolean);
}

export async function mockLogStatsRange(
  params: LogsStatsRangeParams,
  signal?: AbortSignal,
): Promise<LogsStatsQueryRangeResponse> {
  if (params.query.includes("__empty")) {
    return delay({
      status: "success",
      data: { resultType: "matrix", result: [] },
    }, 120, signal);
  }
  const groupFields = statsGroupFields(params.query);
  const seriesCount = groupFields.length ? 4 : 1;
  const hits = await mockLogHitsSeries({
    ...params,
    field: groupFields,
  }, signal);
  throwIfAborted(signal);
  return {
    status: "success",
    data: {
      resultType: "matrix",
      result: hits.hits.slice(0, seriesCount).map((series) => ({
        metric: { __name__: "logs", ...series.fields },
        values: series.timestamps.map((timestamp, index) => ({
          timestamp: timestamp / 1000,
          value: String(series.values[index] ?? 0),
        })),
      })),
    },
  };
}

export function mockLogsQueryTimeRange(
  params: LogsRangeParams,
  signal?: AbortSignal,
): Promise<LogsQueryTimeRange> {
  const range = mockRange(params);
  let startMs = range.startMs;
  const relative = /_time\s*:\s*(\d+(?:\.\d+)?)\s*(ms|s|m|h|d)\b/i.exec(params.query);
  if (relative) {
    startMs = Math.max(
      startMs,
      range.endMs - durationSeconds(`${relative[1]}${relative[2]}`) * 1000,
    );
  }
  return delay({
    start: dayjs(startMs).toISOString(),
    end: dayjs(range.endMs).toISOString(),
    hasTimeFilter: /(?:^|\W)_time\s*:/.test(params.query),
  }, 80, signal);
}

function metadataValues(
  values: readonly LogsFieldValue[],
  params: Pick<LogsMetadataParams, "filter" | "limit" | "query">,
): LogsFieldValuesResponse {
  if (params.query?.includes("__empty")) return { values: [] };
  const filter = params.filter?.trim().toLowerCase();
  const filtered = filter
    ? values.filter((item) => item.value.toLowerCase().includes(filter))
    : [...values];
  const limit = params.limit && params.limit > 0 ? params.limit : filtered.length;
  return { values: filtered.slice(0, limit) };
}

function valuesForField(field: string): LogsFieldValue[] {
  let values: string[];
  switch (field) {
    case "app":
    case "service":
    case "service.name":
      values = STREAMS.map((stream) => stream.app);
      break;
    case "env":
      values = ["prod", "staging"];
      break;
    case "pod":
      values = STREAMS.map((stream) => stream.pod);
      break;
    case "container":
      values = STREAMS.map((stream) => stream.container);
      break;
    case "level":
      values = ["info", "debug", "warn", "error"];
      break;
    case "http_method":
      values = METHODS;
      break;
    case "http_path":
      values = PATHS;
      break;
    case "http_status":
      values = ["200", "201", "204", "404", "500", "502", "503"];
      break;
    default:
      values = [`${field}-a`, `${field}-b`, `${field}-c`];
  }
  return [...new Set(values)].map((value, index) => ({
    value,
    hits: Math.max(1, 9_000 - index * 1_137),
  }));
}

function metadataRows(params: LogsMetadataParams): LogRow[] {
  if (params.query?.includes("__empty")) return [];
  const { startMs, endMs } = mockRange(params);
  return makeMockRows(params.query?.trim() || "*", startMs, endMs, 720)
    .filter((row) => matchesRequestFilters(row.raw, params));
}

function fieldNamesForRows(
  fields: readonly LogsFieldValue[],
  rows: readonly LogRow[],
): LogsFieldValue[] {
  return fields.flatMap((field): LogsFieldValue[] => {
    const hits = rows.reduce((count, row) => (
      fieldValue(row.raw, field.value) === undefined ? count : count + 1
    ), 0);
    return hits ? [{ value: field.value, hits }] : [];
  });
}

function fieldValuesForRows(
  field: string,
  rows: readonly LogRow[],
): LogsFieldValue[] {
  const counts = new Map<string, number>();
  for (const row of rows) {
    const value = fieldValue(row.raw, field);
    if (value === undefined) continue;
    counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  return [...counts]
    .map(([value, hits]) => ({ value, hits }))
    .sort((left, right) => right.hits - left.hits || left.value.localeCompare(right.value));
}

export function mockLogFieldNames(
  params: LogsMetadataParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  return delay(
    metadataValues(fieldNamesForRows(FIELD_NAMES, metadataRows(params)), params),
    90,
    signal,
  );
}

export function mockLogFieldValues(
  params: LogsFieldValuesParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  const values = fieldValuesForRows(params.field, metadataRows(params));
  return delay(metadataValues(
    values.length || hasRequestFilters(params) ? values : valuesForField(params.field),
    params,
  ), 90, signal);
}

const consumedTransientErrors = new Set<string>();

async function rejectOnceAfterDelay(
  key: string,
  message: string,
  ms: number,
  signal?: AbortSignal,
): Promise<void> {
  if (consumedTransientErrors.has(key)) return;
  await delay(undefined, ms, signal);
  if (consumedTransientErrors.has(key)) return;
  consumedTransientErrors.add(key);
  throw new Error(message);
}

export async function mockStreamFieldNames(
  params: LogsMetadataParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  if (params.query?.includes("__stream_fields_error_once")) {
    await rejectOnceAfterDelay(
      JSON.stringify([
        "stream_fields",
        params.project ?? "",
        params.query,
        params.start,
        params.end,
      ]),
      `Mock stream field names error near: ${params.query}`,
      90,
      signal,
    );
  }
  return delay(
    metadataValues(fieldNamesForRows(STREAM_FIELD_NAMES, metadataRows(params)), params),
    90,
    signal,
  );
}

export async function mockStreamFieldValues(
  params: LogsFieldValuesParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  if (params.query?.includes("__field_values_error_once")) {
    await rejectOnceAfterDelay(
      JSON.stringify([
        "stream_field_values",
        params.project ?? "",
        params.query,
        params.start,
        params.end,
        params.field,
      ]),
      `Mock stream field values error for ${params.field}`,
      90,
      signal,
    );
  }
  const values = fieldValuesForRows(params.field, metadataRows(params));
  return delay(metadataValues(
    values.length || hasRequestFilters(params) ? values : valuesForField(params.field),
    params,
  ), 90, signal);
}

function mockTailRow(query: string, index: number): LogRow {
  const rng = makeRng(`tail|${query}|${index}`);
  const stream = STREAMS[index % STREAMS.length];
  const level = pickLevel(rng);
  const timestamp = dayjs().valueOf();
  const message = rng.pick(MESSAGES[level]);
  const method = rng.pick(METHODS);
  const path = rng.pick(PATHS);
  const status =
    level === "error" ? rng.pick([500, 502, 503]) : rng.pick([200, 201, 204, 404]);
  const durationMs = level === "warn" ? rng.int(500, 2400) : rng.int(1, 480);
  const requestedTrace = traceFilter(query);
  const traceId = requestedTrace
    ?? (rng.bool(0.18) ? hexId(`tail-trace|${query}|${index}`, 32) : undefined);
  const raw: Record<string, unknown> = {
    _time: dayjs(timestamp).toISOString(),
    _msg: message,
    _stream: streamLabel(stream),
    level,
    app: stream.app,
    env: stream.env,
    pod: stream.pod,
    container: stream.container,
    http_method: method,
    http_path: path,
    http_status: status,
    duration_ms: durationMs,
    host: `${stream.app}-node-${rng.int(1, 8)}`,
  };
  if (traceId) {
    raw.trace_id = traceId;
    raw.span_id = hexId(`tail-span|${query}|${index}`, 16);
  }
  return {
    id: `tail-${timestamp}-${index}`,
    time: timestamp,
    timeRaw: raw._time as string,
    message,
    stream: raw._stream as string,
    level,
    traceId,
    raw,
  };
}

function abortableDelay(ms: number, signal?: AbortSignal): Promise<boolean> {
  if (signal?.aborted) return Promise.resolve(false);
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve(true);
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      resolve(false);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export async function* mockTailLogs(
  params: LogsTailParams,
  signal?: AbortSignal,
): AsyncGenerator<LogRow, void, undefined> {
  if (params.query.includes("__error")) {
    if (!await abortableDelay(100, signal)) return;
    throw new Error(`LogsQL parse error near: ${params.query}`);
  }
  let index = 0;
  while (!signal?.aborted) {
    if (!await abortableDelay(250, signal)) return;
    if (params.query.includes("__empty")) continue;
    const row = mockTailRow(params.query, index++);
    if (matchesRequestFilters(row.raw, params)) yield row;
  }
}

function csvCell(value: unknown): string {
  const text = value === null || value === undefined ? "" : String(value);
  return /[",\r\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
}

export async function mockExportLogs(
  params: LogsExportParams,
  signal?: AbortSignal,
): Promise<Response> {
  const rows = await mockQueryLogs({ ...params, limit: 250 }, signal);
  throwIfAborted(signal);
  if (params.format !== "csv") {
    const body = rows.map((row) => JSON.stringify(row.raw)).join("\n") + (rows.length ? "\n" : "");
    return new Response(body, {
      headers: { "Content-Type": "application/x-ndjson" },
    });
  }

  const fields = [...new Set(rows.flatMap((row) => Object.keys(row.raw)))].sort();
  const lines = [
    fields.map(csvCell).join(","),
    ...rows.map((row) => fields.map((field) => csvCell(row.raw[field])).join(",")),
  ];
  return new Response(lines.join("\n") + "\n", {
    headers: { "Content-Type": "text/csv" },
  });
}

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException("The operation was aborted.", "AbortError");
}

function rejectedAbort<T>(signal: AbortSignal): Promise<T> {
  return Promise.reject(abortReason(signal));
}

function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw abortReason(signal);
}

function delay<T>(value: T, ms: number, signal?: AbortSignal): Promise<T> {
  if (signal?.aborted) return rejectedAbort(signal);
  return new Promise((resolve, reject) => {
    const cleanup = () => signal?.removeEventListener("abort", onAbort);
    const timer = setTimeout(() => {
      cleanup();
      resolve(value);
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      cleanup();
      reject(abortReason(signal!));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
