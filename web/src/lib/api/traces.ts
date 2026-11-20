import { apiGet, ApiError, API_ROOT, IS_MOCK } from "./client";
import {
  normalizeJaegerTraceListResponse,
  normalizeTraceStringListResponse,
  type JaegerKeyValue,
  type JaegerProcess,
  type JaegerSpan,
  type JaegerSpanRef,
  type JaegerTrace,
} from "./traces-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockTraces = () => import("./mock-traces");

export type {
  JaegerKeyValue,
  JaegerProcess,
  JaegerSpan,
  JaegerSpanRef,
  JaegerTrace,
};

/**
 * Traces API. The backend re-exposes Jaeger's JSON HTTP API verbatim, so these
 * types mirror the upstream Jaeger model (times in microseconds). Keeping them
 * faithful means the same UI works against any Jaeger-compatible query service.
 * VITE_MOCK serves a deterministic offline generator instead.
 */

export interface TraceSearchParams {
  service: string;
  operation?: string;
  /** Epoch microseconds. */
  start: number;
  /** Epoch microseconds. */
  end: number;
  limit: number;
  /** Jaeger duration such as `250us` or `1.5ms`. */
  minDuration?: string;
  /** Free-form `key=value key2=value2` tag filter. */
  tags?: string;
}

/** Row summary derived from a full trace for the results list. */
export interface TraceSummary {
  traceID: string;
  rootName: string;
  rootService: string;
  spanCount: number;
  serviceCount: number;
  errorCount: number;
  /** Epoch microseconds of the earliest span. */
  startTime: number;
  /** Total trace duration in microseconds. */
  duration: number;
}

/** True when a span is flagged as errored (Jaeger convention: tag error=true). */
export function isErrorSpan(span: JaegerSpan): boolean {
  const tags = span.tags ?? [];
  return tags.some(
    (t) =>
      (t.key === "error" && (t.value === true || t.value === "true")) ||
      (t.key === "otel.status_code" && t.value === "ERROR") ||
      (t.key === "http.status_code" && Number(t.value) >= 500),
  );
}

/** The root span = the one with no non-self CHILD_OF reference inside the trace. */
export function findRootSpan(trace: JaegerTrace): JaegerSpan | undefined {
  const spans = trace.spans ?? [];
  if (spans.length === 0) return undefined;
  const ids = new Set(spans.map((s) => s.spanID));
  return (
    spans.find(
      (s) =>
        !(s.references ?? []).some(
          (r) => r.refType === "CHILD_OF" && r.spanID !== s.spanID && ids.has(r.spanID),
        ),
    ) ?? spans[0]
  );
}

/** Collapse a full trace into a list-row summary without spread-argument stack overflow. */
export function summarizeTrace(trace: JaegerTrace): TraceSummary {
  const spans = trace.spans ?? [];
  const processes = trace.processes ?? {};
  const root = findRootSpan(trace);

  let start = Infinity;
  let end = -Infinity;
  let errorCount = 0;
  const services = new Set<string>();

  for (const span of spans) {
    if (Number.isFinite(span.startTime)) {
      if (span.startTime < start) start = span.startTime;
      const spanEnd = span.startTime + (Number.isFinite(span.duration) ? span.duration : 0);
      if (spanEnd > end) end = spanEnd;
    }
    services.add(processes[span.processID]?.serviceName ?? span.processID);
    if (isErrorSpan(span)) errorCount += 1;
  }

  if (!Number.isFinite(start)) start = 0;
  if (!Number.isFinite(end)) end = start;

  return {
    traceID: trace.traceID,
    rootName: root?.operationName ?? "unknown",
    rootService: root ? processes[root.processID]?.serviceName ?? "" : "",
    spanCount: spans.length,
    serviceCount: services.size,
    errorCount,
    startTime: start,
    duration: Math.max(0, end - start),
  };
}

export async function getServices(
  signal?: AbortSignal,
  project?: string,
): Promise<string[]> {
  if (IS_MOCK) return (await loadMockTraces()).mockServices();
  const resp = await apiGet<unknown>("/traces/services", {
    base: API_ROOT,
    signal,
    project,
  });
  return normalizeTraceStringListResponse(resp);
}

export async function getOperations(
  service: string,
  signal?: AbortSignal,
  project?: string,
): Promise<string[]> {
  if (IS_MOCK) return (await loadMockTraces()).mockOperations(service);
  const resp = await apiGet<unknown>(
    `/traces/services/${encodeURIComponent(service)}/operations`,
    { base: API_ROOT, signal, project },
  );
  return normalizeTraceStringListResponse(resp);
}

export async function searchTraces(
  params: TraceSearchParams,
  signal?: AbortSignal,
  project?: string,
): Promise<JaegerTrace[]> {
  if (IS_MOCK) return (await loadMockTraces()).mockSearchTraces(params);
  const resp = await apiGet<unknown>("/traces/search", {
    base: API_ROOT,
    params: {
      service: params.service,
      operation: params.operation || undefined,
      start: params.start,
      end: params.end,
      limit: params.limit,
      minDuration: params.minDuration || undefined,
      tags: params.tags || undefined,
    },
    signal,
    project,
  });
  return normalizeJaegerTraceListResponse(resp);
}

export async function getTrace(
  traceID: string,
  signal?: AbortSignal,
  project?: string,
): Promise<JaegerTrace> {
  if (IS_MOCK) return (await loadMockTraces()).mockGetTrace(traceID);
  const resp = await apiGet<unknown>(
    `/traces/${encodeURIComponent(traceID)}`,
    { base: API_ROOT, signal, project },
  );
  const trace = normalizeJaegerTraceListResponse(resp)[0];
  if (!trace) throw new ApiError(`trace ${traceID} not found`, 404);
  return trace;
}
