import type { EndpointStat, PerfWindow } from "@/lib/api/performance";
import type { TraceSearchParams } from "@/lib/api/traces";

export const DEFAULT_TRACE_LIMIT = 20;

/** Stable URL shape for an executable, absolute trace search. */
export interface TraceSearchURL {
  service?: string;
  operation?: string;
  start?: number;
  end?: number;
  minDuration?: string;
  limit?: number;
  tags?: string;
  run?: number;
}

type EndpointTraceFields = Pick<EndpointStat, "service" | "method" | "route" | "p95">;
type AbsoluteWindow = Pick<PerfWindow, "start" | "end">;

/** Keep route parsing permissive; execution validation is deliberately all-or-nothing below. */
export function parseTraceSearchURL(search: Record<string, unknown>): TraceSearchURL {
  return {
    service: scalarString(search.service),
    operation: scalarString(search.operation),
    start: scalarInteger(search.start),
    end: scalarInteger(search.end),
    minDuration: scalarString(search.minDuration),
    limit: scalarInteger(search.limit),
    tags: scalarString(search.tags),
    run: flagNumber(search.run),
  };
}

/** Convert the Performance API's millisecond window and latency into Jaeger units. */
export function buildEndpointTraceSearch(
  endpoint: EndpointTraceFields,
  window: AbsoluteWindow,
): TraceSearchParams {
  const start = millisecondsToMicros(window.start);
  const end = millisecondsToMicros(window.end);
  const minDuration = finiteMicros(endpoint.p95 * 1_000);
  if (!endpoint.service.trim() || !endpoint.method.trim() || !endpoint.route.trim()) {
    throw new TypeError("endpoint trace search requires service, method, and route");
  }
  if (start === null || end === null || end <= start || minDuration === null) {
    throw new TypeError("endpoint trace search requires a valid window and P95");
  }
  return {
    service: endpoint.service,
    operation: `${endpoint.method} ${endpoint.route}`,
    start,
    end,
    minDuration: `${minDuration}us`,
    limit: DEFAULT_TRACE_LIMIT,
  };
}

/** Serialize only canonical executable fields, including the explicit run intent. */
export function traceSearchToURL(params: TraceSearchParams): TraceSearchURL {
  const minDuration = params.minDuration === undefined
    ? undefined
    : canonicalDuration(params.minDuration);
  if (
    !params.service.trim() ||
    !validTimestamp(params.start) ||
    !validTimestamp(params.end) ||
    params.end <= params.start ||
    !validLimit(params.limit) ||
    (params.minDuration !== undefined && minDuration === null)
  ) {
    throw new TypeError("cannot serialize invalid trace search parameters");
  }

  return compactSearch({
    service: params.service,
    operation: params.operation?.trim() || undefined,
    start: params.start,
    end: params.end,
    minDuration: minDuration ?? undefined,
    limit: params.limit === DEFAULT_TRACE_LIMIT ? undefined : params.limit,
    tags: params.tags?.trim() || undefined,
    run: 1,
  });
}

/** Decode an executable URL without filling invalid absolute fields from the current clock. */
export function traceSearchFromURL(search: TraceSearchURL): TraceSearchParams | null {
  if (search.run !== 1) return null;
  const service = search.service?.trim() ?? "";
  const start = search.start ?? null;
  const end = search.end ?? null;
  const limit = search.limit ?? DEFAULT_TRACE_LIMIT;
  const minDuration = search.minDuration === undefined
    ? undefined
    : canonicalDuration(search.minDuration);
  if (
    !service ||
    start === null ||
    end === null ||
    !validTimestamp(start) ||
    !validTimestamp(end) ||
    end <= start ||
    !validLimit(limit) ||
    (search.minDuration !== undefined && minDuration === null)
  ) {
    return null;
  }

  return {
    service,
    start,
    end,
    limit,
    ...(search.operation?.trim() ? { operation: search.operation.trim() } : {}),
    ...(minDuration !== undefined && minDuration !== null ? { minDuration } : {}),
    ...(search.tags?.trim() ? { tags: search.tags.trim() } : {}),
  };
}

/** Parse the two duration units used at the Performance/Jaeger boundary. */
export function durationMicros(value: string): number | null {
  const match = /^\s*(\d+(?:\.\d+)?)\s*(us|µs|ms)\s*$/.exec(value);
  if (!match) return null;
  const amount = Number(match[1]);
  return finiteMicros(amount * (match[2] === "ms" ? 1_000 : 1));
}

function canonicalDuration(value: string): string | null {
  const micros = durationMicros(value);
  return micros === null ? null : `${micros}us`;
}

function millisecondsToMicros(value: number): number | null {
  if (!Number.isSafeInteger(value) || value < 0) return null;
  return finiteMicros(value * 1_000);
}

function finiteMicros(value: number): number | null {
  if (!Number.isFinite(value) || value < 0) return null;
  const rounded = Math.ceil(value);
  return Number.isSafeInteger(rounded) ? rounded : null;
}

function validTimestamp(value: number): boolean {
  return Number.isSafeInteger(value) && value >= 0;
}

function validLimit(value: number): boolean {
  return Number.isSafeInteger(value) && value >= 1 && value <= 100;
}

function scalarString(value: unknown): string | undefined {
  if (typeof value === "string") return value;
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  return undefined;
}

function scalarInteger(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isSafeInteger(value)) return value;
  if (typeof value !== "string" || !/^\d+$/.test(value)) return undefined;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}

function flagNumber(value: unknown): number | undefined {
  if (value === true || value === 1 || value === "1" || value === "true") return 1;
  return undefined;
}

function compactSearch(search: TraceSearchURL): TraceSearchURL {
  return Object.fromEntries(
    Object.entries(search).filter(([, value]) => value !== undefined),
  ) as TraceSearchURL;
}
