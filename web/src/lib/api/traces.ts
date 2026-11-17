import { apiGet, API_ROOT, IS_MOCK } from "./client";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockTraces = () => import("./mock-traces");

/**
 * Traces API. The backend re-exposes Jaeger's JSON HTTP API verbatim, so these
 * types mirror the upstream Jaeger model (times in microseconds). Keeping them
 * faithful means the same UI works against any Jaeger-compatible query service.
 * VITE_MOCK serves a deterministic offline generator instead.
 */

/** A Jaeger tag / typed key-value pair. */
export interface JaegerKeyValue {
  key: string;
  type?: string;
  value: string | number | boolean;
}

export interface JaegerSpanRef {
  refType: "CHILD_OF" | "FOLLOWS_FROM";
  traceID: string;
  spanID: string;
}

export interface JaegerSpan {
  traceID: string;
  spanID: string;
  operationName: string;
  /** Epoch microseconds. */
  startTime: number;
  /** Microseconds. */
  duration: number;
  references: JaegerSpanRef[];
  tags: JaegerKeyValue[];
  processID: string;
  logs?: Array<{ timestamp: number; fields: JaegerKeyValue[] }>;
  warnings?: string[] | null;
}

export interface JaegerProcess {
  serviceName: string;
  tags: JaegerKeyValue[];
}

export interface JaegerTrace {
  traceID: string;
  spans: JaegerSpan[];
  processes: Record<string, JaegerProcess>;
  warnings?: string[] | null;
}

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
  return span.tags.some(
    (t) =>
      (t.key === "error" && (t.value === true || t.value === "true")) ||
      (t.key === "otel.status_code" && t.value === "ERROR") ||
      (t.key === "http.status_code" && Number(t.value) >= 500),
  );
}

/** The root span = the one with no CHILD_OF reference inside the trace. */
export function findRootSpan(trace: JaegerTrace): JaegerSpan | undefined {
  const ids = new Set(trace.spans.map((s) => s.spanID));
  return (
    trace.spans.find(
      (s) => !s.references.some((r) => r.refType === "CHILD_OF" && ids.has(r.spanID)),
    ) ?? trace.spans[0]
  );
}

/** Collapse a full trace into a list-row summary. */
export function summarizeTrace(trace: JaegerTrace): TraceSummary {
  const root = findRootSpan(trace);
  const start = Math.min(...trace.spans.map((s) => s.startTime));
  const end = Math.max(...trace.spans.map((s) => s.startTime + s.duration));
  const services = new Set(
    trace.spans.map((s) => trace.processes[s.processID]?.serviceName ?? s.processID),
  );
  return {
    traceID: trace.traceID,
    rootName: root?.operationName ?? "unknown",
    rootService: root ? trace.processes[root.processID]?.serviceName ?? "" : "",
    spanCount: trace.spans.length,
    serviceCount: services.size,
    errorCount: trace.spans.filter(isErrorSpan).length,
    startTime: start,
    duration: end - start,
  };
}

export async function getServices(signal?: AbortSignal): Promise<string[]> {
  if (IS_MOCK) return (await loadMockTraces()).mockServices();
  const resp = await apiGet<{ data: string[] | null }>("/traces/services", {
    base: API_ROOT,
    signal,
  });
  return resp.data ?? [];
}

export async function getOperations(
  service: string,
  signal?: AbortSignal,
): Promise<string[]> {
  if (IS_MOCK) return (await loadMockTraces()).mockOperations(service);
  const resp = await apiGet<{ data: string[] | null }>(
    `/traces/services/${encodeURIComponent(service)}/operations`,
    { base: API_ROOT, signal },
  );
  return resp.data ?? [];
}

export async function searchTraces(
  params: TraceSearchParams,
  signal?: AbortSignal,
): Promise<JaegerTrace[]> {
  if (IS_MOCK) return (await loadMockTraces()).mockSearchTraces(params);
  const resp = await apiGet<{ data: JaegerTrace[] | null }>("/traces/search", {
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
  });
  return resp.data ?? [];
}

export async function getTrace(
  traceID: string,
  signal?: AbortSignal,
): Promise<JaegerTrace> {
  if (IS_MOCK) return (await loadMockTraces()).mockGetTrace(traceID);
  const resp = await apiGet<{ data: JaegerTrace[] | null }>(
    `/traces/${encodeURIComponent(traceID)}`,
    { base: API_ROOT, signal },
  );
  const trace = resp.data?.[0];
  if (!trace) throw new Error(`trace ${traceID} not found`);
  return trace;
}
