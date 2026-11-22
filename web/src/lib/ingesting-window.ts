import type { JaegerTrace } from "./api/traces-contract";

export const RECENT_INGEST_WINDOW_MS = 30_000;

/**
 * Compute the latest span end timestamp in epoch milliseconds for a trace.
 * Jaeger spans encode `startTime` and `duration` in microseconds (µs).
 */
export function traceLatestEndMs(trace: JaegerTrace | null | undefined): number | null {
  const spans = trace?.spans;
  if (!spans || spans.length === 0) return null;

  let latestEndUs = -Infinity;
  for (const span of spans) {
    if (!Number.isFinite(span.startTime) || span.startTime <= 0) continue;
    const durationUs = Number.isFinite(span.duration) ? Math.max(0, span.duration) : 0;
    const endUs = span.startTime + durationUs;
    if (Number.isFinite(endUs) && endUs > latestEndUs) {
      latestEndUs = endUs;
    }
  }

  if (!Number.isFinite(latestEndUs) || latestEndUs <= 0) return null;
  return latestEndUs / 1_000;
}

/**
 * Convert a `useTimeWindow` `window.end` value (Unix seconds) into epoch milliseconds.
 */
export function timeWindowEndMs(windowEndSeconds: number): number | null {
  if (!Number.isFinite(windowEndSeconds) || windowEndSeconds <= 0) return null;
  const endMs = windowEndSeconds * 1_000;
  if (!Number.isFinite(endMs) || endMs <= 0) return null;
  return endMs;
}

/**
 * Remaining milliseconds before `timestampMs` exits the 30-second recent-ingest window.
 * Returns 0 once `referenceNowMs - timestampMs >= RECENT_INGEST_WINDOW_MS` or for invalid inputs.
 */
export function recentIngestRemainingMs(timestampMs: number, referenceNowMs: number): number {
  if (!Number.isFinite(timestampMs) || timestampMs <= 0 || !Number.isFinite(referenceNowMs)) {
    return 0;
  }
  const elapsedMs = referenceNowMs - timestampMs;
  if (elapsedMs >= RECENT_INGEST_WINDOW_MS) {
    return 0;
  }
  return RECENT_INGEST_WINDOW_MS - elapsedMs;
}

export function isTraceRecentlyIngesting(trace: JaegerTrace, referenceNowMs: number): boolean {
  const traceEndMs = traceLatestEndMs(trace);
  return traceEndMs !== null && recentIngestRemainingMs(traceEndMs, referenceNowMs) > 0;
}

export function isTimeWindowRecentlyIngesting(
  windowEndSeconds: number,
  referenceNowMs: number,
): boolean {
  const windowEndMs = timeWindowEndMs(windowEndSeconds);
  return windowEndMs !== null && recentIngestRemainingMs(windowEndMs, referenceNowMs) > 0;
}
