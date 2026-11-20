import { apiGet, apiSend, ApiError, IS_MOCK, API_ROOT } from "./client";
import {
  AI_CONFIDENCES,
  buildAssistantQueryPayload,
  normalizeAiSettings,
  normalizeAiStatus,
  normalizeAiTestResult,
  normalizeAssistantAnswer,
  normalizeAssistantExplain,
  normalizeIssueAiAnalysis,
  type AiConfidence,
  type AiSettings,
  type AiSettingsUpdate,
  type AiSignal,
  type AiStatus,
  type AiTestResult,
  type AssistantAnswer,
  type AssistantQueryRequest,
  type AssistantResult,
  type AssistantTimeRange,
  type IssueAiAnalysis,
  type ProbableCause,
} from "./ai-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockAi = () => import("./mock-ai");

export { AI_CONFIDENCES };
export type {
  AiConfidence,
  AiSettings,
  AiSettingsUpdate,
  AiSignal,
  AiStatus,
  AiTestResult,
  AssistantAnswer,
  AssistantQueryRequest,
  AssistantResult,
  AssistantTimeRange,
  IssueAiAnalysis,
  ProbableCause,
};

/**
 * AI features API. All endpoints live under /api (platform scope, not /api/v1):
 *   GET  /api/ai/status                       -> { enabled, provider, model }
 *   POST /api/ai/test           (admin)       -> { latency_ms, model?, echo } (409 when disabled)
 *   POST /api/ai/assistant/query              -> generated query + explanation + inline result
 *   POST /api/ai/assistant/explain            -> { explanation }
 *   GET  /api/issues/:id/ai-analysis          -> analysis | 404 (not generated)
 *   POST /api/issues/:id/ai-analysis[?refresh]-> generate + return analysis
 */

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

export async function getAiStatus(signal?: AbortSignal): Promise<AiStatus> {
  if (IS_MOCK) return (await loadMockAi()).mockAiStatus();
  return normalizeAiStatus(await apiGet<unknown>("/ai/status", { base: API_ROOT, signal }));
}

export async function testAi(): Promise<AiTestResult> {
  if (IS_MOCK) return (await loadMockAi()).mockAiTest();
  return normalizeAiTestResult(
    await apiSend<unknown>("POST", "/ai/test", undefined, { base: API_ROOT }),
  );
}

/** Admin: read the persisted AI configuration (never includes the key). */
export async function getAiSettings(signal?: AbortSignal): Promise<AiSettings> {
  if (IS_MOCK) return (await loadMockAi()).mockAiSettingsGet();
  return normalizeAiSettings(await apiGet<unknown>("/ai/settings", { base: API_ROOT, signal }));
}

/** Admin: save the AI configuration; the provider client hot-swaps server-side. */
export async function updateAiSettings(req: AiSettingsUpdate): Promise<AiSettings> {
  if (IS_MOCK) return (await loadMockAi()).mockAiSettingsPut(req);
  return normalizeAiSettings(await apiSend<unknown>("PUT", "/ai/settings", req, { base: API_ROOT }));
}

export async function assistantQuery(
  req: AssistantQueryRequest,
  signal?: AbortSignal,
  project?: string,
): Promise<AssistantAnswer> {
  if (IS_MOCK) return (await loadMockAi()).mockAssistantQuery(req);
  const payload = buildAssistantQueryPayload(req);
  const wire = await apiSend<unknown>(
    "POST",
    "/ai/assistant/query",
    payload,
    { base: API_ROOT, signal, project },
  );
  return normalizeAssistantAnswer(wire);
}

export async function assistantExplain(
  req: { signal: "metrics" | "logs"; query: string; stats?: Record<string, unknown> },
  project?: string,
): Promise<{ explanation: string }> {
  if (IS_MOCK) return (await loadMockAi()).mockAssistantExplain(req);
  return normalizeAssistantExplain(
    await apiSend<unknown>("POST", "/ai/assistant/explain", req, {
      base: API_ROOT,
      project,
    }),
  );
}

/** Returns the stored analysis, or null (HTTP 404) when none has been generated. */
export async function getIssueAiAnalysis(
  id: string,
  signal?: AbortSignal,
  project?: string,
): Promise<IssueAiAnalysis | null> {
  if (IS_MOCK) return (await loadMockAi()).mockGetIssueAnalysis(id);
  try {
    const raw = await apiGet<unknown>(
      `/issues/${encodeURIComponent(id)}/ai-analysis`,
      { base: API_ROOT, signal, project },
    );
    return normalizeIssueAiAnalysis(raw);
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return null;
    throw e;
  }
}

export async function generateIssueAiAnalysis(
  id: string,
  refresh = false,
  project?: string,
): Promise<IssueAiAnalysis> {
  if (IS_MOCK) return (await loadMockAi()).mockGenerateIssueAnalysis(id, refresh);
  const raw = await apiSend<unknown>(
    "POST",
    `/issues/${encodeURIComponent(id)}/ai-analysis`,
    undefined,
    { base: API_ROOT, params: refresh ? { refresh: "1" } : undefined, project },
  );
  return normalizeIssueAiAnalysis(raw);
}
