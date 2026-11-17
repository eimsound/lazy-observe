import { apiGet, apiSend, ApiError, IS_MOCK, API_ROOT } from "./client";
import { seriesLabel } from "./metrics";
import { normalizeLine, type LogRow } from "./logs";
import type { PromQueryRangeResponse, TimeSeries } from "./types";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockAi = () => import("./mock-ai");

/**
 * AI features API. All endpoints live under /api (platform scope, not /api/v1):
 *   GET  /api/ai/status                       -> { enabled, provider, model }
 *   POST /api/ai/test           (admin)       -> { latency_ms, echo }   (409 when disabled)
 *   POST /api/ai/assistant/query              -> generated query + explanation + inline result
 *   POST /api/ai/assistant/explain            -> { explanation }
 *   GET  /api/issues/:id/ai-analysis          -> analysis | 404 (not generated)
 *   POST /api/issues/:id/ai-analysis[?refresh]-> generate + return analysis
 *
 * The raw backend result for an assistant query is isomorphic to the
 * /query_range (Prometheus) or logs contract; this domain layer normalizes both
 * into the same `AssistantResult` union the panel renders, so the UI never has
 * to branch on the wire shape. VITE_MOCK serves a deterministic offline store.
 */

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
}

/** Providers the admin UI offers; each has a default base URL. */
export const AI_PROVIDERS: { id: string; label: string; defaultBaseUrl: string }[] = [
  { id: "openai", label: "OpenAI", defaultBaseUrl: "https://api.openai.com" },
  { id: "deepseek", label: "DeepSeek", defaultBaseUrl: "https://api.deepseek.com" },
  { id: "anthropic", label: "Anthropic", defaultBaseUrl: "https://api.anthropic.com" },
  { id: "ollama", label: "Ollama", defaultBaseUrl: "http://127.0.0.1:11434" },
];

export function providerDefaultBaseUrl(provider: string): string {
  return AI_PROVIDERS.find((p) => p.id === provider)?.defaultBaseUrl ?? "";
}

export interface AiTestResult {
  latency_ms: number;
  echo: string;
}

/** Which signal the assistant should target; "auto" lets the model decide. */
export type AiSignal = "metrics" | "logs" | "auto";

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

/** Raw on-the-wire response (backend). Kept loose; normalized below. */
interface AssistantQueryWire {
  signal: "metrics" | "logs";
  query: string;
  explanation: string;
  result?: unknown;
  execution_error?: string;
}

export async function getAiStatus(signal?: AbortSignal): Promise<AiStatus> {
  if (IS_MOCK) return (await loadMockAi()).mockAiStatus();
  return apiGet<AiStatus>("/ai/status", { base: API_ROOT, signal });
}

export async function testAi(): Promise<AiTestResult> {
  if (IS_MOCK) return (await loadMockAi()).mockAiTest();
  return apiSend<AiTestResult>("POST", "/ai/test", undefined, { base: API_ROOT });
}

/** Admin: read the persisted AI configuration (never includes the key). */
export async function getAiSettings(signal?: AbortSignal): Promise<AiSettings> {
  if (IS_MOCK) return (await loadMockAi()).mockAiSettingsGet();
  return apiGet<AiSettings>("/ai/settings", { base: API_ROOT, signal });
}

/** Admin: save the AI configuration; the provider client hot-swaps server-side. */
export async function updateAiSettings(req: AiSettingsUpdate): Promise<AiSettings> {
  if (IS_MOCK) return (await loadMockAi()).mockAiSettingsPut(req);
  return apiSend<AiSettings>("PUT", "/ai/settings", req, { base: API_ROOT });
}

/** Normalize a Prometheus matrix response into chart-ready TimeSeries. */
function normalizeMetrics(raw: unknown): TimeSeries[] {
  const resp = raw as PromQueryRangeResponse;
  if (!resp || resp.status !== "success") return [];
  return resp.data.result.map((r) => ({
    name: seriesLabel(r.metric),
    labels: r.metric,
    points: r.values.map(([t, v]) => {
      const n = Number(v);
      return [t * 1000, Number.isFinite(n) ? n : null] as [number, number | null];
    }),
  }));
}

/** Normalize a logs result (array of raw objects) into LogRows. */
function normalizeLogs(raw: unknown): LogRow[] {
  if (!Array.isArray(raw)) return [];
  return raw.map((o, i) => normalizeLine(o as Record<string, unknown>, i));
}

function normalizeAnswer(wire: AssistantQueryWire): AssistantAnswer {
  const base: AssistantAnswer = {
    signal: wire.signal,
    query: wire.query,
    explanation: wire.explanation,
    executionError: wire.execution_error,
  };
  if (wire.execution_error || wire.result === undefined) return base;
  base.result =
    wire.signal === "metrics"
      ? { kind: "metrics", series: normalizeMetrics(wire.result) }
      : { kind: "logs", rows: normalizeLogs(wire.result) };
  return base;
}

export async function assistantQuery(
  req: { question: string; signal?: AiSignal; time_range?: string },
  signal?: AbortSignal,
): Promise<AssistantAnswer> {
  if (IS_MOCK) return (await loadMockAi()).mockAssistantQuery(req);
  const wire = await apiSend<AssistantQueryWire>(
    "POST",
    "/ai/assistant/query",
    { question: req.question, signal: req.signal, time_range: req.time_range },
    { base: API_ROOT, signal },
  );
  return normalizeAnswer(wire);
}

export async function assistantExplain(
  req: { signal: "metrics" | "logs"; query: string; stats?: Record<string, unknown> },
): Promise<{ explanation: string }> {
  if (IS_MOCK) return (await loadMockAi()).mockAssistantExplain(req);
  return apiSend<{ explanation: string }>("POST", "/ai/assistant/explain", req, {
    base: API_ROOT,
  });
}

// ---- Issue AI analysis ----------------------------------------------------

export type AiConfidence = "high" | "medium" | "low";
export const AI_CONFIDENCES: AiConfidence[] = ["high", "medium", "low"];

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

/** Returns the stored analysis, or null (HTTP 404) when none has been generated. */
export async function getIssueAiAnalysis(
  id: string,
  signal?: AbortSignal,
): Promise<IssueAiAnalysis | null> {
  if (IS_MOCK) return (await loadMockAi()).mockGetIssueAnalysis(id);
  try {
    return await apiGet<IssueAiAnalysis>(
      `/issues/${encodeURIComponent(id)}/ai-analysis`,
      { base: API_ROOT, signal },
    );
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

export async function generateIssueAiAnalysis(
  id: string,
  refresh = false,
): Promise<IssueAiAnalysis> {
  if (IS_MOCK) return (await loadMockAi()).mockGenerateIssueAnalysis(id, refresh);
  return apiSend<IssueAiAnalysis>(
    "POST",
    `/issues/${encodeURIComponent(id)}/ai-analysis`,
    undefined,
    { base: API_ROOT, params: refresh ? { refresh: "1" } : undefined },
  );
}
