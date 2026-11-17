import dayjs from "../dayjs.ts";

export type LLMCallStatus = "ok" | "error" | "unset";
export type LLMTrendMetric = "calls" | "tokens" | "cost";

export interface LLMAggregate {
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cost: number;
  unpriced: boolean;
}

export interface LLMModelAggregate extends LLMAggregate {
  model: string;
}

export interface LLMServiceAggregate extends LLMAggregate {
  service: string;
}

export interface LLMSummary extends LLMAggregate {
  by_model: LLMModelAggregate[];
  by_service: LLMServiceAggregate[];
  truncated: boolean;
}

export interface LLMCall {
  /** RFC3339 timestamp emitted by the gateway. */
  time: string;
  service: string;
  model: string;
  operation: string;
  input_tokens: number;
  output_tokens: number;
  cost: number;
  unpriced: boolean;
  /** Jaeger-compatible duration in microseconds. */
  duration: number;
  trace_id: string;
  status: LLMCallStatus;
}

export interface LLMCallsResult {
  calls: LLMCall[];
  count: number;
  limit: number;
  truncated: boolean;
}

export interface LLMTimeseriesPoint {
  /** RFC3339 bucket start. */
  time: string;
  value: number;
}

export interface LLMTimeseriesResult {
  metric: LLMTrendMetric;
  /** Go duration string returned by the gateway, for example `5m0s`. */
  step: string;
  points: LLMTimeseriesPoint[];
  truncated: boolean;
}

export interface LLMWindow {
  /** Unix seconds. */
  start: number;
  /** Unix seconds. */
  end: number;
}

export interface LLMCallFilters {
  service?: string;
  model?: string;
  limit?: number;
}

/** Turn a UI window into the exact query boundary accepted by `/api/llm/*`. */
export function llmRangeParams(window: LLMWindow): { start: number; end: number } {
  if (!Number.isSafeInteger(window.start) || !Number.isSafeInteger(window.end)) {
    throw new TypeError("LLM window bounds must be Unix seconds");
  }
  if (window.start <= 0 || window.end <= window.start || window.end - window.start > 7 * 24 * 60 * 60) {
    throw new RangeError("LLM window must be positive and no longer than 7d");
  }
  return { start: window.start, end: window.end };
}

/** Keep trend buckets within the gateway's 1m minimum and around 120 points. */
export function llmStep(window: LLMWindow): string {
  llmRangeParams(window);
  return `${Math.max(60, Math.ceil((window.end - window.start) / 120))}s`;
}

export function normalizeLLMSummaryResponse(value: unknown): LLMSummary {
  const response = objectValue(value, "LLM summary");
  const aggregate = normalizeAggregate(response, "LLM summary", true);
  if (!Array.isArray(response.by_model) || !Array.isArray(response.by_service)) {
    throw new TypeError("LLM summary must contain model and service aggregates");
  }
  if (typeof response.truncated !== "boolean") {
    throw new TypeError("LLM summary truncated must be a boolean");
  }
  const byModel = response.by_model.map((item, index) => {
    const row = objectValue(item, `LLM summary by_model[${index}]`);
    return {
      model: nonEmptyString(row.model, `LLM summary by_model[${index}].model`),
      ...normalizeAggregate(row, `LLM summary by_model[${index}]`),
    };
  });
  const byService = response.by_service.map((item, index) => {
    const row = objectValue(item, `LLM summary by_service[${index}]`);
    return {
      service: nonEmptyString(row.service, `LLM summary by_service[${index}].service`),
      ...normalizeAggregate(row, `LLM summary by_service[${index}]`),
    };
  });
  return { ...aggregate, by_model: byModel, by_service: byService, truncated: response.truncated };
}

export function normalizeLLMCallsResponse(value: unknown): LLMCallsResult {
  const response = objectValue(value, "LLM calls");
  if (!Array.isArray(response.calls)) throw new TypeError("LLM calls response must contain calls");
  const calls = response.calls.map(normalizeCall);
  const count = nonNegativeInteger(response.count, "LLM calls count");
  const limit = positiveInteger(response.limit, "LLM calls limit");
  if (count !== calls.length || count > limit) {
    throw new TypeError("LLM calls count must match the returned calls and not exceed limit");
  }
  if (typeof response.truncated !== "boolean") {
    throw new TypeError("LLM calls truncated must be a boolean");
  }
  return { calls, count, limit, truncated: response.truncated };
}

export function normalizeLLMTimeseriesResponse(value: unknown): LLMTimeseriesResult {
  const response = objectValue(value, "LLM timeseries");
  if (response.metric !== "calls" && response.metric !== "tokens" && response.metric !== "cost") {
    throw new TypeError("LLM timeseries metric is invalid");
  }
  const step = nonEmptyString(response.step, "LLM timeseries step");
  if (!Array.isArray(response.points)) {
    throw new TypeError("LLM timeseries response must contain points");
  }
  let previous = -Infinity;
  const points = response.points.map((value, index) => {
    const point = objectValue(value, `LLM timeseries points[${index}]`);
    const time = rfc3339(point.time, `LLM timeseries points[${index}].time`);
    const timestamp = dayjs(time).valueOf();
    if (timestamp <= previous) throw new TypeError("LLM timeseries points must be strictly ordered");
    previous = timestamp;
    return {
      time,
      value: nonNegativeNumber(point.value, `LLM timeseries points[${index}].value`),
    };
  });
  if (typeof response.truncated !== "boolean") {
    throw new TypeError("LLM timeseries truncated must be a boolean");
  }
  return { metric: response.metric, step, points, truncated: response.truncated };
}

export interface GenAISpanDetails {
  operation: string;
  provider?: string;
  requestModel?: string;
  responseModel?: string;
  inputTokens?: number;
  outputTokens?: number;
  inputContent?: string;
  outputContent?: string;
  /** True only when prompt/completion content was present in the authorized trace payload. */
  hasCapturedContent: boolean;
}

/**
 * Build the trace drawer's GenAI view model from Jaeger tags. Current OTel
 * names are preferred, while the deprecated SDK fields remain readable during
 * migration. Content is never fetched separately or inferred.
 */
export function extractGenAISpanDetails(
  tags: Array<{ key: string; value: string | number | boolean }>,
): GenAISpanDetails | null {
  const values = new Map(tags.map((tag) => [tag.key, tag.value]));
  const operation = textTag(values.get("gen_ai.operation.name"));
  if (!operation) return null;
  const inputContent = textTag(values.get("gen_ai.input.messages")) ?? textTag(values.get("gen_ai.prompt"));
  const outputContent = textTag(values.get("gen_ai.output.messages")) ?? textTag(values.get("gen_ai.completion"));
  return {
    operation,
    provider: textTag(values.get("gen_ai.provider.name")) ?? textTag(values.get("gen_ai.system")),
    requestModel: textTag(values.get("gen_ai.request.model")),
    responseModel: textTag(values.get("gen_ai.response.model")),
    inputTokens: numericTag(values.get("gen_ai.usage.input_tokens")),
    outputTokens: numericTag(values.get("gen_ai.usage.output_tokens")),
    inputContent,
    outputContent,
    hasCapturedContent: inputContent !== undefined || outputContent !== undefined,
  };
}

function normalizeAggregate(
  value: Record<string, unknown>,
  path: string,
  totals = false,
): LLMAggregate {
  return {
    calls: nonNegativeInteger(totals ? value.total_calls : value.calls, `${path} calls`),
    input_tokens: nonNegativeInteger(
      totals ? value.total_input_tokens : value.input_tokens,
      `${path} input tokens`,
    ),
    output_tokens: nonNegativeInteger(
      totals ? value.total_output_tokens : value.output_tokens,
      `${path} output tokens`,
    ),
    cost: nonNegativeNumber(totals ? value.total_cost : value.cost, `${path} cost`),
    unpriced: booleanValue(value.unpriced, `${path} unpriced`),
  };
}

function normalizeCall(value: unknown, index: number): LLMCall {
  const call = objectValue(value, `LLM calls[${index}]`);
  if (call.status !== "ok" && call.status !== "error" && call.status !== "unset") {
    throw new TypeError(`LLM calls[${index}].status is invalid`);
  }
  const traceID = nonEmptyString(call.trace_id, `LLM calls[${index}].trace_id`);
  if (!/^(?:[0-9a-f]{16}|[0-9a-f]{32})$/i.test(traceID)) {
    throw new TypeError(`LLM calls[${index}].trace_id must be a 64-bit or 128-bit hexadecimal trace ID`);
  }
  return {
    time: rfc3339(call.time, `LLM calls[${index}].time`),
    service: nonEmptyString(call.service, `LLM calls[${index}].service`),
    model: nonEmptyString(call.model, `LLM calls[${index}].model`),
    operation: nonEmptyString(call.operation, `LLM calls[${index}].operation`),
    input_tokens: nonNegativeInteger(call.input_tokens, `LLM calls[${index}].input_tokens`),
    output_tokens: nonNegativeInteger(call.output_tokens, `LLM calls[${index}].output_tokens`),
    cost: nonNegativeNumber(call.cost, `LLM calls[${index}].cost`),
    unpriced: booleanValue(call.unpriced, `LLM calls[${index}].unpriced`),
    duration: nonNegativeInteger(call.duration, `LLM calls[${index}].duration`),
    trace_id: traceID,
    status: call.status,
  };
}

function objectValue(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TypeError(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}

function nonEmptyString(value: unknown, path: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new TypeError(`${path} must be a non-empty string`);
  }
  return value;
}

function rfc3339(value: unknown, path: string): string {
  const text = nonEmptyString(value, path);
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(text)) {
    throw new TypeError(`${path} must be RFC3339`);
  }
  if (!dayjs(text).isValid()) throw new TypeError(`${path} must be RFC3339`);
  return text;
}

function nonNegativeInteger(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new TypeError(`${path} must be a non-negative safe integer`);
  }
  return value;
}

function positiveInteger(value: unknown, path: string): number {
  const result = nonNegativeInteger(value, path);
  if (result === 0) throw new TypeError(`${path} must be positive`);
  return result;
}

function nonNegativeNumber(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new TypeError(`${path} must be a non-negative finite number`);
  }
  return value;
}

function booleanValue(value: unknown, path: string): boolean {
  if (typeof value !== "boolean") throw new TypeError(`${path} must be a boolean`);
  return value;
}

function textTag(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

function numericTag(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : typeof value === "string" ? Number(value) : NaN;
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined;
}
