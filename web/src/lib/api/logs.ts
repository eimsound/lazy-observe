import type { LogsExtraFilterParams } from "../logs-explorer.ts";
import {
  ApiError,
  apiGet,
  apiPostForm,
  apiPostFormResponse,
  IS_MOCK,
  type FormFields,
} from "./client";
import {
  normalizeFieldValuesResponse,
  normalizeHits,
  normalizeHitsSeries,
  normalizeLine,
  normalizeQueryTimeRange,
  normalizeStatsQueryRange,
  parseJsonLines,
  parseNdjsonStream,
  type LogRow,
  type LogsFieldValuesResponse,
  type LogsHistogram,
  type LogsHitsResponse,
  type LogsQueryTimeRange,
  type LogsStatsQueryRangeResponse,
} from "./logs-contract.ts";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockLogs = () => import("./mock-logs");

export {
  createNdjsonParser,
  detectLevel,
  normalizeFieldValuesResponse,
  normalizeHits,
  normalizeHitsSeries,
  normalizeLine,
  normalizeQueryTimeRange,
  normalizeStatsQueryRange,
  parseJsonLines,
  parseLogsTimestamp,
  parseNdjsonStream,
} from "./logs-contract.ts";
export type {
  LogLevel,
  LogRow,
  LogsFieldValue,
  LogsFieldValuesResponse,
  LogsHistogram,
  LogsHitSeries,
  LogsHitsResponse,
  LogsQueryTimeRange,
  LogsStatsQueryRangeResponse,
  LogsStatsSample,
  LogsStatsSeries,
  NdjsonMapper,
  NdjsonParser,
  ParseNdjsonStreamOptions,
} from "./logs-contract.ts";
export type { LogsExtraFilterParams } from "../logs-explorer.ts";

/**
 * Logs query API. Every real request uses the authenticated project-scoped
 * `/api/v1/logs/*` gateway aliases. VITE_MOCK swaps only the transport.
 */

export type LogsTimeValue = string | number;
export type LogsDurationValue = string | number;
export type LogsRequestFilters = Partial<LogsExtraFilterParams>;

export interface LogsQueryParams extends LogsRequestFilters {
  query: string;
  /** Unix seconds for legacy callers; RFC3339 and Unix-nanosecond strings also work. */
  start: LogsTimeValue;
  end: LogsTimeValue;
  limit: number;
  offset?: number;
}

export interface LogsHitsParams {
  query: string;
  /** Unix seconds. */
  start: number;
  /** Unix seconds. */
  end: number;
  /** Bucket width in seconds. */
  step: number;
}

export interface LogsRangeParams extends LogsRequestFilters {
  query: string;
  start: LogsTimeValue;
  end: LogsTimeValue;
}

export interface LogsHitSeriesParams extends LogsRangeParams {
  step: LogsDurationValue;
  /** Align buckets to this VictoriaLogs duration, e.g. `-480m`. */
  offset?: LogsDurationValue;
  /** One or more fields to group the histogram by. */
  field?: string | readonly string[];
  fieldsLimit?: number;
}

export interface LogsStatsRangeParams extends LogsRangeParams {
  step: LogsDurationValue;
  offset?: LogsDurationValue;
  fieldsLimit?: number;
}

export interface LogsMetadataParams extends LogsRequestFilters {
  query?: string;
  start: LogsTimeValue;
  end: LogsTimeValue;
  filter?: string;
  limit?: number;
  ignorePipes?: boolean;
}

export interface LogsFieldValuesParams extends LogsMetadataParams {
  field: string;
}

export interface LogsTailParams extends LogsRequestFilters {
  query: string;
  offset?: LogsDurationValue;
  startOffset?: LogsDurationValue;
  refreshInterval?: LogsDurationValue;
}

export interface LogsExportParams extends LogsRangeParams {
  format?: "jsonl" | "csv";
}

function duration(value: LogsDurationValue | undefined): string | undefined {
  return typeof value === "number" ? `${value}s` : value;
}

function filterFields(params: LogsRequestFilters): FormFields {
  return {
    extra_filters: params.extraFilters,
    extra_stream_filters: params.extraStreamFilters,
  };
}

function rangeFields(params: LogsRangeParams): FormFields {
  return {
    query: params.query.trim(),
    start: params.start,
    end: params.end,
    ...filterFields(params),
  };
}

async function postLogsJson(
  path: string,
  fields: FormFields,
  signal?: AbortSignal,
): Promise<unknown> {
  const response = await apiPostFormResponse(path, fields, {
    signal,
    accept: "application/json",
  });
  try {
    return await response.json();
  } catch {
    throw new ApiError(`invalid JSON response from ${path}`, response.status);
  }
}

/** The original paged query API retained for existing logs-page callers. */
export async function queryLogs(
  params: LogsQueryParams,
  signal?: AbortSignal,
): Promise<LogRow[]> {
  if (IS_MOCK) return (await loadMockLogs()).mockQueryLogs(params, signal);
  const text = await apiPostForm(
    "/logs/query",
    {
      query: params.query,
      start: params.start,
      end: params.end,
      limit: params.limit,
      offset: params.offset,
      ...filterFields(params),
    },
    { signal, accept: "application/stream+json, application/x-ndjson" },
  );
  return parseJsonLines(text);
}

/**
 * The original flattened histogram API retained for the existing chart.
 * New explorer code should use queryLogHitsSeries to preserve grouping fields.
 */
export async function logsHits(
  params: LogsHitsParams,
  signal?: AbortSignal,
): Promise<LogsHistogram> {
  if (IS_MOCK) return (await loadMockLogs()).mockLogsHits(params, signal);
  const response = await apiGet<unknown>("/logs/hits", {
    params: {
      query: params.query,
      start: params.start,
      end: params.end,
      step: `${params.step}s`,
    },
    signal,
  });
  return normalizeHits(response);
}

/** Query the grouped, multi-series VictoriaLogs hits response. */
export async function queryLogHitsSeries(
  params: LogsHitSeriesParams,
  signal?: AbortSignal,
): Promise<LogsHitsResponse> {
  if (IS_MOCK) return (await loadMockLogs()).mockLogHitsSeries(params, signal);
  const field = typeof params.field === "string" ? [params.field] : params.field;
  const response = await postLogsJson(
    "/logs/hits",
    {
      ...rangeFields(params),
      step: duration(params.step),
      offset: duration(params.offset),
      field,
      fields_limit: params.fieldsLimit,
    },
    signal,
  );
  return { hits: normalizeHitsSeries(response) };
}

/** Query a LogsQL `stats` expression over a time range. */
export async function queryLogStatsRange(
  params: LogsStatsRangeParams,
  signal?: AbortSignal,
): Promise<LogsStatsQueryRangeResponse> {
  if (IS_MOCK) return (await loadMockLogs()).mockLogStatsRange(params, signal);
  const response = await postLogsJson(
    "/logs/stats_query_range",
    {
      ...rangeFields(params),
      step: duration(params.step),
      offset: duration(params.offset),
      fields_limit: params.fieldsLimit,
    },
    signal,
  );
  return normalizeStatsQueryRange(response);
}

/** Resolve the effective time range after LogsQL `_time` filters are applied. */
export async function queryLogsTimeRange(
  params: LogsRangeParams,
  signal?: AbortSignal,
): Promise<LogsQueryTimeRange> {
  if (IS_MOCK) return (await loadMockLogs()).mockLogsQueryTimeRange(params, signal);
  const response = await postLogsJson(
    "/logs/query_time_range",
    rangeFields(params),
    signal,
  );
  return normalizeQueryTimeRange(response);
}

async function queryMetadata(
  path: string,
  params: LogsMetadataParams,
  field: string | undefined,
  defaultIgnorePipes: boolean,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  const response = await postLogsJson(
    path,
    {
      query: params.query?.trim() || "*",
      start: params.start,
      end: params.end,
      field,
      filter: params.filter,
      limit: params.limit,
      ignore_pipes: (params.ignorePipes ?? defaultIgnorePipes) ? "1" : undefined,
      ...filterFields(params),
    },
    signal,
  );
  return normalizeFieldValuesResponse(response);
}

export async function queryLogFieldNames(
  params: LogsMetadataParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  if (IS_MOCK) return (await loadMockLogs()).mockLogFieldNames(params, signal);
  return queryMetadata("/logs/field_names", params, undefined, false, signal);
}

export async function queryLogFieldValues(
  params: LogsFieldValuesParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  if (IS_MOCK) return (await loadMockLogs()).mockLogFieldValues(params, signal);
  return queryMetadata("/logs/field_values", params, params.field, false, signal);
}

export async function queryLogStreamFieldNames(
  params: LogsMetadataParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  if (IS_MOCK) return (await loadMockLogs()).mockStreamFieldNames(params, signal);
  return queryMetadata("/logs/stream_field_names", params, undefined, true, signal);
}

export async function queryLogStreamFieldValues(
  params: LogsFieldValuesParams,
  signal?: AbortSignal,
): Promise<LogsFieldValuesResponse> {
  if (IS_MOCK) return (await loadMockLogs()).mockStreamFieldValues(params, signal);
  return queryMetadata("/logs/stream_field_values", params, params.field, true, signal);
}

/**
 * Open a live tail and yield normalized rows as soon as complete NDJSON lines
 * arrive. Aborting the supplied signal cancels both fetch and stream reader.
 */
export async function* tailLogs(
  params: LogsTailParams,
  signal?: AbortSignal,
): AsyncGenerator<LogRow, void, undefined> {
  if (IS_MOCK) {
    yield* (await loadMockLogs()).mockTailLogs(params, signal);
    return;
  }

  const response = await apiPostFormResponse(
    "/logs/tail",
    {
      query: params.query.trim(),
      offset: duration(params.offset),
      start_offset: duration(params.startOffset),
      refresh_interval: duration(params.refreshInterval),
      ...filterFields(params),
    },
    {
      signal,
      accept: "application/x-ndjson, application/stream+json",
    },
  );
  if (!response.body) throw new ApiError("logs tail response has no body", response.status);

  yield* parseNdjsonStream(response.body, {
    signal,
    mapper: (value, index) => (
      typeof value === "object" && value !== null && !Array.isArray(value)
        ? normalizeLine(value as Record<string, unknown>, index)
        : null
    ),
  });
}

/**
 * Return an unconsumed response suitable for pipeTo/save-file workflows.
 * No `limit` field exists on this API, so exports cannot accidentally truncate.
 */
export async function exportLogs(
  params: LogsExportParams,
  signal?: AbortSignal,
): Promise<Response> {
  if (IS_MOCK) return (await loadMockLogs()).mockExportLogs(params, signal);
  const isCsv = params.format === "csv";
  return apiPostFormResponse(
    "/logs/query",
    {
      ...rangeFields(params),
      format: isCsv ? "csv" : undefined,
    },
    {
      signal,
      accept: isCsv ? "text/csv" : "application/stream+json, application/x-ndjson",
    },
  );
}
