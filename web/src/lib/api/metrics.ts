import { apiGet, ApiError, IS_MOCK } from "./client";
import { estimateClockDriftSeconds } from "@/lib/metrics-query";
import dayjs from "../dayjs.ts";
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

export async function queryRange(
  params: QueryRangeParams,
  signal?: AbortSignal,
): Promise<TimeSeries[]> {
  return (await queryRangeDetailed(params, signal)).data;
}

export async function queryRangeDetailed(
  params: QueryRangeParams,
  signal?: AbortSignal,
): Promise<MetricsQueryEnvelope<TimeSeries[]>> {
  const started = performance.now();
  const resp: PromQueryRangeResponse = IS_MOCK
    ? await simulate(params)
    : await apiGet<PromQueryRangeResponse>("/query_range", {
        params: {
          query: params.query,
          start: params.start,
          end: params.end,
          step: `${params.step}s`,
        },
        signal,
      });

  if (resp.status === "error") {
    throw new ApiError(resp.error || "query failed", 400);
  }
  return {
    data: toTimeSeries(resp),
    warnings: resp.warnings ?? [],
    infos: resp.infos ?? [],
    durationMs: Math.max(0, performance.now() - started),
  };
}

export async function queryInstant(
  query: string,
  time: number,
  signal?: AbortSignal,
): Promise<MetricSample[]> {
  const detailed = await queryInstantDetailed(query, time, signal);
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
export async function queryServerClock(signal?: AbortSignal): Promise<ServerClockSample> {
  const requestStartedMs = dayjs().valueOf();
  const resp = await apiGet<PromInstantResponse>("/query", {
    params: { query: "time()" },
    signal,
  });
  const responseReceivedMs = dayjs().valueOf();
  if (resp.status === "error") throw new ApiError(resp.error || "server time query failed", 400);

  const raw = resp.data.resultType === "scalar" || resp.data.resultType === "string"
    ? resp.data.result[1]
    : resp.data.resultType === "vector"
      ? resp.data.result[0]?.value[1]
      : resp.data.result[0]?.values.at(-1)?.[1];
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
): Promise<MetricsQueryEnvelope<MetricTableResult>> {
  const started = performance.now();
  const resp: PromInstantResponse = IS_MOCK
    ? instantFromRange(await simulate({ query, start: time - 60, end: time, step: 60 }))
    : await apiGet<PromInstantResponse>("/query", {
        params: { query, time },
        signal,
      });

  if (resp.status === "error") {
    throw new ApiError(resp.error || "query failed", 400);
  }
  let result: MetricTableResult;
  if (resp.data.resultType === "scalar" || resp.data.resultType === "string") {
    const [timestamp, raw] = resp.data.result;
    result = {
      resultType: resp.data.resultType,
      rows: [{
        name: resp.data.resultType === "scalar" ? "scalar" : "string",
        labels: {},
        values: [{ timestamp: timestamp * 1000, raw, value: finiteValue(raw) }],
      }],
    };
  } else if (resp.data.resultType === "matrix") {
    result = {
      resultType: "matrix",
      rows: resp.data.result.map((item) => ({
        name: seriesLabel(item.metric),
        labels: item.metric,
        values: item.values.map(([timestamp, raw]) => ({
          timestamp: timestamp * 1000,
          raw,
          value: finiteValue(raw),
        })),
      })),
    };
  } else {
    result = {
      resultType: "vector",
      rows: resp.data.result.map((item) => ({
        name: seriesLabel(item.metric),
        labels: item.metric,
        values: [{
          timestamp: item.value[0] * 1000,
          raw: item.value[1],
          value: finiteValue(item.value[1]),
        }],
      })),
    };
  }
  return {
    data: result,
    warnings: resp.warnings ?? [],
    infos: resp.infos ?? [],
    durationMs: Math.max(0, performance.now() - started),
  };
}

export interface MetricsMetadataParams {
  start: number;
  end: number;
  match?: string;
  limit?: number;
}

interface PromStringsResponse {
  status: "success" | "error";
  data?: string[];
  error?: string;
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
  const resp = await apiGet<PromStringsResponse>(path, {
    params: {
      start: params.start,
      end: params.end,
      "match[]": params.match,
      limit: params.limit,
    },
    signal,
  });
  if (resp.status === "error") throw new ApiError(resp.error || "metadata query failed", 400);
  return (resp.data ?? []).filter((item): item is string => typeof item === "string");
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

interface PrettifyResponse {
  status: "success" | "error";
  query?: string;
  msg?: string;
}

export async function prettifyPromQL(query: string, signal?: AbortSignal): Promise<string> {
  if (IS_MOCK) return query.trim();
  const resp = await apiGet<PrettifyResponse>("/prettify-query", {
    params: { query },
    signal,
  });
  if (resp.status !== "success" || typeof resp.query !== "string") {
    throw new ApiError(resp.msg || "invalid PromQL", 400);
  }
  return resp.query;
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

function finiteValue(raw: string): number | null {
  const value = Number(raw);
  return Number.isFinite(value) ? value : null;
}

/** Build a stable, readable series label from Prometheus labels. */
export function seriesLabel(metric: Record<string, string>): string {
  const name = metric.__name__ ?? "";
  const rest = Object.entries(metric)
    .filter(([k]) => k !== "__name__")
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}="${v}"`)
    .join(", ");
  if (!rest) return name || "{}";
  return name ? `${name}{${rest}}` : `{${rest}}`;
}

function toTimeSeries(resp: Extract<PromQueryRangeResponse, { status: "success" }>): TimeSeries[] {
  return resp.data.result.map((r) => ({
    name: seriesLabel(r.metric),
    labels: r.metric,
    points: r.values.map(([t, v]) => {
      const n = Number(v);
      // Prometheus encodes NaN/Inf as strings; render them as gaps.
      return [t * 1000, Number.isFinite(n) ? n : null] as [number, number | null];
    }),
  }));
}
