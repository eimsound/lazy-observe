import { apiGet, API_ROOT, IS_MOCK } from "./client";
import {
  llmRangeParams,
  llmStep,
  normalizeLLMCallsResponse,
  normalizeLLMSummaryResponse,
  normalizeLLMTimeseriesResponse,
  type LLMCallFilters,
  type LLMCallsResult,
  type LLMSummary,
  type LLMTimeseriesResult,
  type LLMTrendMetric,
  type LLMWindow,
} from "./llm-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockLlm = () => import("./mock-llm");

export type {
  LLMAggregate,
  LLMCall,
  LLMCallFilters,
  LLMCallStatus,
  LLMCallsResult,
  LLMModelAggregate,
  LLMServiceAggregate,
  LLMSummary,
  LLMTimeseriesResult,
  LLMTrendMetric,
  LLMWindow,
} from "./llm-contract";

export async function getLLMSummary(
  window: LLMWindow,
  signal?: AbortSignal,
): Promise<LLMSummary> {
  const response = IS_MOCK
    ? await (await loadMockLlm()).mockLLMSummaryResponse(window, signal)
    : await apiGet<unknown>("/llm/summary", {
        base: API_ROOT,
        params: llmRangeParams(window),
        signal,
      });
  return normalizeLLMSummaryResponse(response);
}

export async function getLLMCalls(
  window: LLMWindow,
  filters: LLMCallFilters = {},
  signal?: AbortSignal,
): Promise<LLMCallsResult> {
  const normalizedFilters = {
    service: filters.service?.trim() || undefined,
    model: filters.model?.trim() || undefined,
    limit: filters.limit ?? 100,
  };
  const response = IS_MOCK
    ? await (await loadMockLlm()).mockLLMCallsResponse(window, normalizedFilters, signal)
    : await apiGet<unknown>("/llm/calls", {
        base: API_ROOT,
        params: { ...llmRangeParams(window), ...normalizedFilters },
        signal,
      });
  return normalizeLLMCallsResponse(response);
}

export async function getLLMTimeseries(
  window: LLMWindow,
  metric: LLMTrendMetric,
  signal?: AbortSignal,
): Promise<LLMTimeseriesResult> {
  const step = llmStep(window);
  const response = IS_MOCK
    ? await (await loadMockLlm()).mockLLMTimeseriesResponse(window, metric, step, signal)
    : await apiGet<unknown>("/llm/timeseries", {
        base: API_ROOT,
        params: { ...llmRangeParams(window), metric, step },
        signal,
      });
  return normalizeLLMTimeseriesResponse(response);
}
