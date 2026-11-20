import { apiGet, ApiError, IS_MOCK } from "./client";
import { estimateClockDriftSeconds } from "@/lib/metrics-query";
import dayjs from "../dayjs.ts";
import {
  formatPromSeriesLabel,
  MetricsContractError,
  normalizePrettifyResponse,
  normalizePromInstantResponse,
  normalizePromRangeResponse,
  normalizePromStringsResponse,
} from "./metrics-contract";
import type {
  MetricSample,
  MetricTableResult,
  MetricsQueryEnvelope,
  PromInstantResponse,
  PromQueryRangeResponse,
  QueryRangeParams,
  TimeSeries,
} from "./types";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMock = () => import("./mock");

/**
 * Metrics query API. Wraps GET /api/v1/query_range (Prometheus contract) and
 * normalizes the matrix response into TimeSeries the chart layer can render.
 * In VITE_MOCK mode it returns built-in sine-wave data instead of hitting HTTP.
 */

function rethrowContractError(error: unknown): never {
  if (error instanceof MetricsContractError) {
    throw new ApiError(error.message, error.status, error.errorType);
  }
  throw error;
}

export async function queryRange(
  params: QueryRangeParams,
  signal?: AbortSignal,
  project?: string,
): Promise<TimeSeries[]> {
  return (await queryRangeDetailed(params, signal, project)).data;
}

export async function queryRangeDetailed(
  params: QueryRangeParams,
  signal?: AbortSignal,
  project?: string,
): Promise<MetricsQueryEnvelope<TimeSeries[]>> {
  const started = performance.now();
  const resp: unknown = IS_MOCK
    ? await simulate(params)
    : await apiGet<unknown>("/query_range", {
        params: {
          query: params.query,
          start: params.start,
          end: params.end,
          step: `${params.step}s`,
        },
        signal,
        project,
      });

  let normalized;
  try {
    normalized = normalizePromRangeResponse(resp);
  } catch (error) {
    rethrowContractError(error);
  }
  return {
    data: normalized.data,
    warnings: normalized.warnings,
    infos: normalized.infos,
    durationMs: Math.max(0, performance.now() - started),
  };
}

export async function queryInstant(
  query: string,
  time: number,
  signal?: AbortSignal,
  project?: string,
): Promise<MetricSample[]> {
  const detailed = await queryInstantDetailed(query, time, signal, project);
  return detailed.data.rows.flatMap((row) => {
    const value = row.values.at(-1);
    return value ? [{ name: row.name, labels: row.labels, timestamp: value.timestamp, value: value.value }] : [];
  });
}

export interface ServerClockSample {
  driftSeconds: number;
  roundTripMs: number;
}

/** Query the metrics server's current clock without overriding its evaluation time. */
export async function queryServerClock(
  signal?: AbortSignal,
  project?: string,
): Promise<ServerClockSample> {
  const requestStartedMs = dayjs().valueOf();
  const resp = await apiGet<unknown>("/query", {
    params: { query: "time()" },
    signal,
    project,
  });
  const responseReceivedMs = dayjs().valueOf();

  let normalized;
  try {
    normalized = normalizePromInstantResponse(resp);
  } catch (error) {
    rethrowContractError(error);
  }

  const raw = normalized.data.rows[0]?.values.at(-1)?.raw;
  const serverSeconds = Number(raw);
  if (!Number.isFinite(serverSeconds)) throw new ApiError("metrics server returned an invalid time", 502);

  return {
    driftSeconds: estimateClockDriftSeconds(serverSeconds, requestStartedMs, responseReceivedMs),
    roundTripMs: Math.max(0, responseReceivedMs - requestStartedMs),
  };
}

export async function queryInstantDetailed(
  query: string,
  time: number,
  signal?: AbortSignal,
  project?: string,
): Promise<MetricsQueryEnvelope<MetricTableResult>> {
  const started = performance.now();
  const resp: unknown = IS_MOCK
    ? instantFromRange(await simulate({ query, start: time - 60, end: time, step: 60 }))
    : await apiGet<unknown>("/query", {
        params: { query, time },
        signal,
        project,
      });

  let normalized;
  try {
    normalized = normalizePromInstantResponse(resp);
  } catch (error) {
    rethrowContractError(error);
  }
  return {
    data: normalized.data,
    warnings: normalized.warnings,
    infos: normalized.infos,
    durationMs: Math.max(0, performance.now() - started),
  };
}

export interface MetricsMetadataParams {
  start: number;
  end: number;
  match?: string;
  limit?: number;
  project?: string;
}

async function metadataValues(
  path: string,
  params: MetricsMetadataParams,
  signal?: AbortSignal,
): Promise<string[]> {
  if (IS_MOCK) {
    if (path === "/labels") return ["__name__", "service.name", "http.request.method", "http.response.status_code"];
    if (path.includes("__name__")) return ["http.server.request.duration_count", "http.server.active_requests", "example.work.requests"];
    return ["lazy-observe-fiber-example", "GET", "200", "503"];
  }
  const resp = await apiGet<unknown>(path, {
    params: {
      start: params.start,
      end: params.end,
      "match[]": params.match,
      limit: params.limit,
    },
    signal,
    project: params.project,
  });
  try {
    return normalizePromStringsResponse(resp);
  } catch (error) {
    rethrowContractError(error);
  }
}

export function metricNames(params: MetricsMetadataParams, signal?: AbortSignal) {
  return metadataValues("/label/__name__/values", params, signal);
}

export function labelNames(params: MetricsMetadataParams, signal?: AbortSignal) {
  return metadataValues("/labels", params, signal);
}

export function labelValues(label: string, params: MetricsMetadataParams, signal?: AbortSignal) {
  return metadataValues(`/label/${encodeURIComponent(label)}/values`, params, signal);
}

export async function prettifyPromQL(
  query: string,
  signal?: AbortSignal,
  project?: string,
): Promise<string> {
  if (IS_MOCK) return query.trim();
  const resp = await apiGet<unknown>("/prettify-query", {
    params: { query },
    signal,
    project,
  });
  try {
    return normalizePrettifyResponse(resp);
  } catch (error) {
    rethrowContractError(error);
  }
}

/** Small delay so loading states are observable in mock mode. */
async function simulate(params: QueryRangeParams): Promise<PromQueryRangeResponse> {
  const { mockQueryRange } = await loadMock();
  return new Promise((resolve) => setTimeout(() => resolve(mockQueryRange(params)), 350));
}

function instantFromRange(resp: PromQueryRangeResponse): PromInstantResponse {
  if (resp.status === "error") return resp;
  return {
    status: "success",
    data: {
      resultType: "vector",
      result: resp.data.result.flatMap((item) => {
        const value = item.values.at(-1);
        return value ? [{ metric: item.metric, value }] : [];
      }),
    },
  };
}

/** Build a stable, readable series label from Prometheus labels. */
export function seriesLabel(metric: Record<string, string>): string {
  return formatPromSeriesLabel(metric);
}
