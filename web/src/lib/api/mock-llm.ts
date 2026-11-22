import type {
  LLMCall,
  LLMCallFilters,
  LLMTimeseriesPoint,
  LLMTrendMetric,
  LLMWindow,
} from "./llm-contract.ts";
import { hexId } from "./mock-random.ts";
import dayjs from "../dayjs.ts";

interface MockModel {
  model: string;
  provider: string;
  service: string;
  inputPrice: number | null;
  outputPrice: number | null;
}

const MODELS: MockModel[] = [
  {
    model: "gpt-4o-mini-2024-07-18",
    provider: "openai",
    service: "checkout-ai",
    inputPrice: 0.00015,
    outputPrice: 0.0006,
  },
  {
    model: "claude-3-5-sonnet-20241022",
    provider: "anthropic",
    service: "support-agent",
    inputPrice: 0.003,
    outputPrice: 0.015,
  },
  {
    model: "llama-3.1",
    provider: "ollama",
    service: "document-worker",
    inputPrice: 0,
    outputPrice: 0,
  },
  {
    model: "private-eval-model-v2",
    provider: "openai",
    service: "evaluation-worker",
    inputPrice: null,
    outputPrice: null,
  },
];

const CALL_COUNT = 36;

function callDefinition(index: number): MockModel {
  return MODELS[index % MODELS.length];
}

export interface MockLLMTraceMetadata {
  provider: string;
  model: string;
  service: string;
  inputTokens: number;
  outputTokens: number;
  captureContent: boolean;
}

/** Metadata used by mock trace detail so every LLM table link has a real target. */
export function mockLLMTraceMetadata(traceID: string): MockLLMTraceMetadata | null {
  for (let index = 0; index < CALL_COUNT; index++) {
    if (traceID !== hexId(`llm-call:${index}`, 32)) continue;
    const definition = callDefinition(index);
    return {
      provider: definition.provider,
      model: definition.model,
      service: definition.service,
      inputTokens: 180 + index * 37,
      outputTokens: 24 + (index % 9) * 11,
      captureContent: index % 5 === 0,
    };
  }
  return null;
}

export function mockLLMSummaryResponse(window: LLMWindow, signal?: AbortSignal): Promise<unknown> {
  const calls = buildCalls(window);
  const total = aggregate(calls);
  const models = group(calls, "model");
  const services = group(calls, "service");
  return resolveAfter(
    {
      total_calls: total.calls,
      total_input_tokens: total.input_tokens,
      total_output_tokens: total.output_tokens,
      total_cost: total.cost,
      unpriced: total.unpriced,
      by_model: [...models.entries()].map(([model, value]) => ({ model, ...value })),
      by_service: [...services.entries()].map(([service, value]) => ({ service, ...value })),
      truncated: false,
    },
    240,
    signal,
  );
}

export function mockLLMCallsResponse(
  window: LLMWindow,
  filters: LLMCallFilters,
  signal?: AbortSignal,
): Promise<unknown> {
  const limit = filters.limit ?? 100;
  const filtered = buildCalls(window).filter(
    (call) =>
      (!filters.service || call.service === filters.service) &&
      (!filters.model || call.model === filters.model),
  );
  return resolveAfter(
    {
      calls: filtered.slice(0, limit),
      count: Math.min(filtered.length, limit),
      limit,
      truncated: filtered.length > limit,
    },
    260,
    signal,
  );
}

export function mockLLMTimeseriesResponse(
  window: LLMWindow,
  metric: LLMTrendMetric,
  step: string,
  signal?: AbortSignal,
): Promise<unknown> {
  const seconds = Number.parseInt(step, 10);
  const firstBucket = Math.floor(window.start / seconds) * seconds;
  const points: LLMTimeseriesPoint[] = [];
  for (let time = firstBucket; time < window.end; time += seconds) {
    points.push({ time: dayjs(time * 1000).toISOString(), value: 0 });
  }
  for (const call of buildCalls(window)) {
    const timestamp = Math.floor(dayjs(call.time).valueOf() / 1000);
    const index = Math.floor((timestamp - firstBucket) / seconds);
    const point = points[index];
    if (!point) continue;
    if (metric === "calls") point.value += 1;
    else if (metric === "tokens") point.value += call.input_tokens + call.output_tokens;
    else point.value += call.cost;
  }
  return resolveAfter({ metric, step: `${seconds}s`, points, truncated: false }, 220, signal);
}

function buildCalls(window: LLMWindow): LLMCall[] {
  const mockControl = globalThis as { __loMockLLMEmpty?: boolean };
  if (mockControl.__loMockLLMEmpty) return [];
  const width = Math.max(1, window.end - window.start);
  const calls: LLMCall[] = [];
  for (let index = 0; index < CALL_COUNT; index++) {
    const definition = callDefinition(index);
    const inputTokens = 180 + index * 37;
    const outputTokens = 24 + (index % 9) * 11;
    const unpriced = definition.inputPrice === null || definition.outputPrice === null;
    const cost = unpriced
      ? 0
      : (inputTokens / 1000) * definition.inputPrice! +
        (outputTokens / 1000) * definition.outputPrice!;
    const at = window.end - Math.max(1, Math.floor(((index + 0.5) / CALL_COUNT) * width));
    calls.push({
      time: dayjs(at * 1000).toISOString(),
      service: definition.service,
      model: definition.model,
      operation: "chat",
      input_tokens: inputTokens,
      output_tokens: outputTokens,
      cost,
      unpriced,
      duration: 38_000 + index * 7_310,
      trace_id: hexId(`llm-call:${index}`, 32),
      status: index % 11 === 0 ? "error" : "ok",
    });
  }
  return calls.sort((a, b) => dayjs(b.time).valueOf() - dayjs(a.time).valueOf());
}

interface MutableAggregate {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cost: number;
  unpriced: boolean;
}

function aggregate(calls: LLMCall[]): MutableAggregate {
  return calls.reduce<MutableAggregate>(
    (result, call) => ({
      calls: result.calls + 1,
      input_tokens: result.input_tokens + call.input_tokens,
      output_tokens: result.output_tokens + call.output_tokens,
      cost: result.cost + call.cost,
      unpriced: result.unpriced || call.unpriced,
    }),
    { calls: 0, input_tokens: 0, output_tokens: 0, cost: 0, unpriced: false },
  );
}

function group(calls: LLMCall[], key: "model" | "service"): Map<string, MutableAggregate> {
  const result = new Map<string, MutableAggregate>();
  for (const call of calls) {
    const name = call[key];
    const current = result.get(name) ?? {
      calls: 0,
      input_tokens: 0,
      output_tokens: 0,
      cost: 0,
      unpriced: false,
    };
    current.calls += 1;
    current.input_tokens += call.input_tokens;
    current.output_tokens += call.output_tokens;
    current.cost += call.cost;
    current.unpriced ||= call.unpriced;
    result.set(name, current);
  }
  return result;
}

function resolveAfter<T>(value: T, delay: number, signal?: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve(value);
    }, delay);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason ?? new DOMException("The operation was aborted", "AbortError"));
    };
    if (signal?.aborted) onAbort();
    else signal?.addEventListener("abort", onAbort, { once: true });
  });
}
