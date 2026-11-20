import dayjs from "../dayjs.ts";
import { normalizeLine, parseLogsTimestamp, type LogRow } from "./logs-contract.ts";
import { rangeById, resolveRange } from "./ranges.ts";
import type { PromQueryRangeResponse, TimeSeries } from "./types.ts";

/** Which signal the assistant should target; "auto" lets the model decide. */
export type AiSignal = "metrics" | "logs" | "auto";

export interface AiStatus {
  enabled: boolean;
  /** Provider name (never a key). Empty when disabled/unconfigured. */
  provider: string;
  /** Model id. Empty when disabled/unconfigured. */
  model: string;
}

/** Full admin view of the persisted AI configuration. The key is never
 *  returned; `has_api_key` reports whether one is stored. */
export interface AiSettings {
  enabled: boolean;
  provider: string;
  base_url: string;
  model: string;
  max_tokens: number;
  has_api_key: boolean;
  source: string;
  updated_by: string;
  revision?: number;
}

/** Admin write model. Absent fields keep their stored values; an absent or
 *  empty `api_key` keeps the stored key; `clear_api_key` removes it. */
export interface AiSettingsUpdate {
  enabled?: boolean;
  provider?: string;
  base_url?: string;
  model?: string;
  api_key?: string;
  max_tokens?: number;
  clear_api_key?: boolean;
  expected_revision?: number;
}

export interface AiTestResult {
  latency_ms: number;
  echo: string;
  model?: string;
}

export type AiConfidence = "high" | "medium" | "low";
export const AI_CONFIDENCES: AiConfidence[] = ["high", "medium", "low"];
const VALID_CONFIDENCES = new Set<string>(AI_CONFIDENCES);

export interface ProbableCause {
  cause: string;
  confidence: AiConfidence;
  evidence: string;
}

export interface IssueAiAnalysis {
  summary: string;
  probable_causes: ProbableCause[];
  suggested_actions: string[];
  /** True when served from a stored analysis (not freshly generated). */
  cached: boolean;
  /** True when newer events arrived after the analysis was produced. */
  stale: boolean;
  /** Epoch ms the analysis was generated at. */
  ai_analysis_at: number;
}

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

function assertRecord(value: unknown, label: string): Record<string, unknown> {
  if (!isRecord(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value;
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

/** Validate `GET /api/ai/status` (`internal/gateway/ai_api.go`). */
export function normalizeAiStatus(value: unknown): AiStatus {
  const raw = assertRecord(value, "ai status response");
  if (typeof raw.enabled !== "boolean") {
    throw new TypeError("ai status.enabled must be a boolean");
  }
  if (typeof raw.provider !== "string") {
    throw new TypeError("ai status.provider must be a string");
  }
  if (typeof raw.model !== "string") {
    throw new TypeError("ai status.model must be a string");
  }
  return {
    enabled: raw.enabled,
    provider: raw.provider,
    model: raw.model,
  };
}

/**
 * Validates `PUT /api/ai/settings` client payload against the Go gateway rule
 * (`internal/gateway/ai_settings.go`): setting both a non-empty `api_key` and
 * `clear_api_key: true` is rejected with HTTP 400.
 */
export function validateAiSettingsUpdate(req: AiSettingsUpdate): AiSettingsUpdate {
  const trimmedKey = typeof req.api_key === "string" ? req.api_key.trim() : "";
  if (req.clear_api_key && trimmedKey !== "") {
    throw new TypeError("api_key and clear_api_key cannot both be set");
  }
  return req;
}

/** Validate `GET`/`PUT /api/ai/settings` (`internal/gateway/ai_settings.go`). */
export function normalizeAiSettings(value: unknown): AiSettings {
  const raw = assertRecord(value, "ai settings response");
  if (typeof raw.enabled !== "boolean") {
    throw new TypeError("ai settings.enabled must be a boolean");
  }
  if (typeof raw.provider !== "string") {
    throw new TypeError("ai settings.provider must be a string");
  }
  if (typeof raw.base_url !== "string") {
    throw new TypeError("ai settings.base_url must be a string");
  }
  if (typeof raw.model !== "string") {
    throw new TypeError("ai settings.model must be a string");
  }
  if (typeof raw.max_tokens !== "number" || !Number.isFinite(raw.max_tokens) || raw.max_tokens < 0) {
    throw new TypeError("ai settings.max_tokens must be a non-negative number");
  }
  if (typeof raw.has_api_key !== "boolean") {
    throw new TypeError("ai settings.has_api_key must be a boolean");
  }
  if (typeof raw.source !== "string") {
    throw new TypeError("ai settings.source must be a string");
  }
  if (typeof raw.updated_by !== "string") {
    throw new TypeError("ai settings.updated_by must be a string");
  }
  let revision: number | undefined;
  if (raw.revision !== undefined) {
    if (typeof raw.revision !== "number" || !Number.isSafeInteger(raw.revision) || raw.revision < 0) {
      throw new TypeError("ai settings.revision must be a non-negative integer");
    }
    revision = raw.revision;
  }
  return {
    enabled: raw.enabled,
    provider: raw.provider,
    base_url: raw.base_url,
    model: raw.model,
    max_tokens: raw.max_tokens,
    has_api_key: raw.has_api_key,
    source: raw.source,
    updated_by: raw.updated_by,
    ...(revision !== undefined ? { revision } : {}),
  };
}

/** Validate `POST /api/ai/test` (`internal/gateway/ai_api.go`). */
export function normalizeAiTestResult(value: unknown): AiTestResult {
  const raw = assertRecord(value, "ai test response");
  if (typeof raw.latency_ms !== "number" || !Number.isFinite(raw.latency_ms) || raw.latency_ms < 0) {
    throw new TypeError("ai test.latency_ms must be a non-negative number");
  }
  if (typeof raw.echo !== "string") {
    throw new TypeError("ai test.echo must be a string");
  }
  if (raw.model !== undefined && typeof raw.model !== "string") {
    throw new TypeError("ai test.model must be a string");
  }
  return {
    latency_ms: raw.latency_ms,
    echo: raw.echo,
    ...(typeof raw.model === "string" ? { model: raw.model } : {}),
  };
}

/** Validate `POST /api/ai/assistant/explain` (`internal/gateway/ai_assistant.go`). */
export function normalizeAssistantExplain(value: unknown): { explanation: string } {
  const raw = assertRecord(value, "ai assistant explain response");
  if (typeof raw.explanation !== "string") {
    throw new TypeError("ai assistant explain.explanation must be a string");
  }
  return { explanation: raw.explanation };
}

/**
 * Validate `GET`/`POST /api/issues/:id/ai-analysis` (`internal/gateway/issue_ai_analysis.go`).
 *
 * The Go gateway serializes `ai_analysis_at` as an RFC3339 `*time.Time` string
 * and omits `stale` when false; mock data may pass epoch milliseconds. Both are
 * normalized into epoch milliseconds and an explicit boolean `stale`.
 */
export function normalizeIssueAiAnalysis(value: unknown): IssueAiAnalysis {
  const raw = assertRecord(value, "issue ai analysis response");
  if (typeof raw.summary !== "string") {
    throw new TypeError("issue ai analysis.summary must be a string");
  }
  if (!Array.isArray(raw.probable_causes)) {
    throw new TypeError("issue ai analysis.probable_causes must be an array");
  }
  const probableCauses: ProbableCause[] = raw.probable_causes.map((item, idx) => {
    const entry = assertRecord(item, `issue ai analysis.probable_causes[${idx}]`);
    if (typeof entry.cause !== "string") {
      throw new TypeError(`probable_causes[${idx}].cause must be a string`);
    }
    if (typeof entry.confidence !== "string" || !VALID_CONFIDENCES.has(entry.confidence)) {
      throw new TypeError(`probable_causes[${idx}].confidence must be high, medium, or low`);
    }
    if (typeof entry.evidence !== "string") {
      throw new TypeError(`probable_causes[${idx}].evidence must be a string`);
    }
    return {
      cause: entry.cause,
      confidence: entry.confidence as AiConfidence,
      evidence: entry.evidence,
    };
  });
  if (!Array.isArray(raw.suggested_actions)) {
    throw new TypeError("issue ai analysis.suggested_actions must be an array");
  }
  const suggestedActions = raw.suggested_actions.map((action, idx) => {
    if (typeof action !== "string") {
      throw new TypeError(`suggested_actions[${idx}] must be a string`);
    }
    return action;
  });
  if (typeof raw.cached !== "boolean") {
    throw new TypeError("issue ai analysis.cached must be a boolean");
  }
  if (raw.stale !== undefined && typeof raw.stale !== "boolean") {
    throw new TypeError("issue ai analysis.stale must be a boolean when present");
  }

  let analyzedAtMs = 0;
  if (raw.ai_analysis_at !== undefined && raw.ai_analysis_at !== null) {
    const parsed = parseLogsTimestamp(raw.ai_analysis_at);
    if (parsed.ms <= 0 && raw.ai_analysis_at !== 0) {
      throw new TypeError("issue ai analysis.ai_analysis_at must be a valid timestamp");
    }
    analyzedAtMs = parsed.ms;
  }

  return {
    summary: raw.summary,
    probable_causes: probableCauses,
    suggested_actions: suggestedActions,
    cached: raw.cached,
    stale: raw.stale === true,
    ai_analysis_at: analyzedAtMs,
  };
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

export function normalizeAssistantAnswer(value: unknown): AssistantAnswer {
  const wire = assertRecord(value, "ai assistant query response");
  if (wire.signal !== "metrics" && wire.signal !== "logs") {
    throw new TypeError(`ai assistant query.signal must be "metrics" or "logs"`);
  }
  if (typeof wire.query !== "string" || wire.query.trim() === "") {
    throw new TypeError("ai assistant query.query must be a non-empty string");
  }
  if (typeof wire.explanation !== "string") {
    throw new TypeError("ai assistant query.explanation must be a string");
  }
  if (wire.execution_error !== undefined && typeof wire.execution_error !== "string") {
    throw new TypeError("ai assistant query.execution_error must be a string");
  }

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
