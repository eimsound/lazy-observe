import dayjs from "../dayjs.ts";
import { normalizeLine, type LogRow } from "./logs-contract.ts";
import { rangeById, resolveRange } from "./ranges.ts";
import type { PromQueryRangeResponse, TimeSeries } from "./types.ts";

/** Which signal the assistant should target; "auto" lets the model decide. */
export type AiSignal = "metrics" | "logs" | "auto";

/** On-the-wire time range object expected by POST /api/ai/assistant/query. */
export interface AssistantTimeRange {
  start: string | number;
  end: string | number;
}

/** Caller input accepted by assistantQuery (preset string or explicit range). */
export interface AssistantQueryRequest {
  question: string;
  signal?: AiSignal;
  time_range?: string | AssistantTimeRange;
}

/** Exact JSON payload sent to POST /api/ai/assistant/query. */
export interface AssistantQueryPayload {
  question: string;
  signal?: AiSignal;
  time_range?: AssistantTimeRange;
}

/** Normalized inline result rendered under an assistant answer. */
export type AssistantResult =
  | { kind: "metrics"; series: TimeSeries[] }
  | { kind: "logs"; rows: LogRow[] };

/** A single assistant answer (normalized from the backend response). */
export interface AssistantAnswer {
  /** The concrete signal the model chose (never "auto"). */
  signal: "metrics" | "logs";
  /** Generated PromQL / LogsQL — always shown, always copyable. */
  query: string;
  /** Natural-language explanation of the query. */
  explanation: string;
  /** Inline result; absent when the generated query failed to execute. */
  result?: AssistantResult;
  /** Present when the query was generated but failed to run against the store. */
  executionError?: string;
}

/** Raw on-the-wire response from POST /api/ai/assistant/query. */
export interface AssistantQueryWire {
  signal: "metrics" | "logs";
  query: string;
  explanation: string;
  result?: unknown;
  execution_error?: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function formatSeriesLabel(metric: Record<string, string>): string {
  const name = metric.__name__ ?? "";
  const rest = Object.entries(metric)
    .filter(([k]) => k !== "__name__")
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}="${v}"`)
    .join(", ");
  if (!rest) return name || "{}";
  return name ? `${name}{${rest}}` : `{${rest}}`;
}

/**
 * Convert a UI preset id (e.g. "1h") or explicit {start, end} into the
 * structured `time_range` object required by `assistantQueryInput.TimeRange`
 * in `internal/gateway/ai_assistant.go`.
 */
export function resolveAssistantTimeRange(
  timeRange: string | AssistantTimeRange | undefined,
  nowMs = dayjs().valueOf(),
): AssistantTimeRange | undefined {
  if (timeRange === undefined) return undefined;
  if (typeof timeRange === "string") {
    const trimmed = timeRange.trim();
    if (!trimmed) return undefined;
    const { start, end } = resolveRange(rangeById(trimmed), nowMs);
    return {
      start: dayjs(start * 1_000).toISOString(),
      end: dayjs(end * 1_000).toISOString(),
    };
  }
  return { start: timeRange.start, end: timeRange.end };
}

export function buildAssistantQueryPayload(
  req: AssistantQueryRequest,
  nowMs = dayjs().valueOf(),
): AssistantQueryPayload {
  const timeRange = resolveAssistantTimeRange(req.time_range, nowMs);
  return {
    question: req.question,
    ...(req.signal !== undefined ? { signal: req.signal } : {}),
    ...(timeRange !== undefined ? { time_range: timeRange } : {}),
  };
}

/** Normalize a Prometheus matrix response into chart-ready TimeSeries. */
export function normalizeAssistantMetrics(raw: unknown): TimeSeries[] {
  const resp = raw as PromQueryRangeResponse | null | undefined;
  if (!resp || resp.status !== "success" || !Array.isArray(resp.data?.result)) {
    return [];
  }
  return resp.data.result.map((r) => ({
    name: formatSeriesLabel(r.metric ?? {}),
    labels: r.metric ?? {},
    points: (Array.isArray(r.values) ? r.values : []).map(([t, v]) => {
      const n = Number(v);
      return [t * 1000, Number.isFinite(n) ? n : null] as [number, number | null];
    }),
  }));
}

/**
 * Normalize a logs result into LogRows.
 *
 * The backend returns `{ rows, count, limit, truncated }` for `signal: "logs"`
 * (see `executeAssistantQuery` in `internal/gateway/ai_assistant.go`); bare
 * arrays are also accepted for backward compatibility.
 */
export function normalizeAssistantLogs(raw: unknown): LogRow[] {
  const items = Array.isArray(raw)
    ? raw
    : isRecord(raw) && Array.isArray(raw.rows)
      ? raw.rows
      : null;
  if (!items) return [];
  const rows: LogRow[] = [];
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    if (isRecord(item)) rows.push(normalizeLine(item, i));
  }
  return rows;
}

export function normalizeAssistantAnswer(wire: AssistantQueryWire): AssistantAnswer {
  const base: AssistantAnswer = {
    signal: wire.signal,
    query: wire.query,
    explanation: wire.explanation,
    executionError: wire.execution_error,
  };
  if (wire.execution_error || wire.result === undefined) return base;
  base.result =
    wire.signal === "metrics"
      ? { kind: "metrics", series: normalizeAssistantMetrics(wire.result) }
      : { kind: "logs", rows: normalizeAssistantLogs(wire.result) };
  return base;
}
