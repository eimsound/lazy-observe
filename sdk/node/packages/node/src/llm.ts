import { context, SpanKind, SpanStatusCode, trace, type Context, type Span, type Tracer } from "@opentelemetry/api";

const CONTENT_LIMIT = 4 * 1024;
const BODY_INSPECTION_LIMIT = 1 << 20;

/** Canonical GenAI span attribute keys shared across Go, Node, and Python SDKs. */
export const GEN_AI_ATTRS = {
  operation_name: "gen_ai.operation.name",
  provider_name: "gen_ai.provider.name",
  system: "gen_ai.system",
  request_model: "gen_ai.request.model",
  response_model: "gen_ai.response.model",
  usage_input_tokens: "gen_ai.usage.input_tokens",
  usage_output_tokens: "gen_ai.usage.output_tokens",
  usage_cache_read_input_tokens: "gen_ai.usage.cache_read.input_tokens",
  usage_cache_creation_input_tokens: "gen_ai.usage.cache_creation.input_tokens",
  usage_reasoning_tokens: "gen_ai.usage.reasoning_tokens",
  usage_missing: "gen_ai.usage.missing",
  prompt: "gen_ai.prompt",
  completion: "gen_ai.completion",
} as const;

type LLMSystem = "openai" | "anthropic" | "ollama";

interface LLMRequest { model?: string; prompt?: string; stream: boolean }
interface LLMResponse {
  model?: string;
  completion: string;
  completionBytes: number;
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheCreationTokens?: number;
  reasoningTokens?: number;
}
interface LLMContextManager { active(): Context; with<T>(ctx: Context, fn: () => T): T }

export function createWrappedLLMFetch(tracer: Tracer, captureContent: boolean, baseFetch: typeof fetch = globalThis.fetch, contexts: LLMContextManager = context): typeof fetch {
  if (typeof baseFetch !== "function") throw new TypeError("wrapLLMFetch requires a fetch implementation");
  return (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = requestURL(input);
    let pathname: string;
    try {
      pathname = new URL(url).pathname;
    } catch {
      return baseFetch(input, init);
    }
    const system = detectLLMSystem(pathname);
    if (!system) return baseFetch(input, init);

    const body = await requestBody(input, init);
    const request = parseLLMRequest(system, body, captureContent);
    const attributes: Record<string, string> = {
      [GEN_AI_ATTRS.operation_name]: "chat",
      [GEN_AI_ATTRS.provider_name]: system,
      // Retained for compatibility with older OpenTelemetry GenAI consumers.
      [GEN_AI_ATTRS.system]: system,
      ...(request.model ? { [GEN_AI_ATTRS.request_model]: request.model } : {}),
      ...(captureContent && request.prompt ? { [GEN_AI_ATTRS.prompt]: truncateContent(request.prompt) } : {}),
    };
    const parent = contexts.active();
    const span = tracer.startSpan(`chat ${system}`, { kind: SpanKind.CLIENT, attributes }, parent);
    const spanContext = trace.setSpan(parent, span);
    let response: Response;
    try {
      response = await contexts.with(spanContext, () => baseFetch(input, init));
    } catch (error) {
      span.recordException(asError(error));
      span.setStatus({ code: SpanStatusCode.ERROR, message: asError(error).message });
      span.end();
      throw error;
    }
    if (!response.ok) span.setStatus({ code: SpanStatusCode.ERROR, message: `HTTP ${response.status}` });

    if (isStreamingResponse(response, request.stream) && response.body) {
      return wrapStreamingResponse(response, system, span, captureContent);
    }
    try {
      const inspected = await readBoundedResponseText(response);
      if (inspected === undefined) {
        span.setAttributes({ [GEN_AI_ATTRS.usage_missing]: true });
      } else {
        applyLLMResponse(span, parseLLMResponse(system, inspected, captureContent), captureContent);
      }
    } catch (error) {
      span.recordException(asError(error));
      span.setStatus({ code: SpanStatusCode.ERROR, message: asError(error).message });
    } finally {
      span.end();
    }
    return response;
  }) as typeof fetch;
}

export function detectLLMSystem(path: string): LLMSystem | undefined {
  const normalized = path.replace(/\/+$/, "");
  if (normalized.endsWith("/chat/completions")) return "openai";
  if (normalized.endsWith("/v1/messages")) return "anthropic";
  if (normalized.endsWith("/api/chat")) return "ollama";
  return undefined;
}

function requestURL(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

async function requestBody(input: RequestInfo | URL, init?: RequestInit): Promise<string> {
  if (init?.body != null) {
    if (typeof init.body === "string") return init.body.length <= BODY_INSPECTION_LIMIT ? init.body : "";
    if (init.body instanceof URLSearchParams) {
      const text = init.body.toString();
      return text.length <= BODY_INSPECTION_LIMIT ? text : "";
    }
    if (init.body instanceof ArrayBuffer) {
      return init.body.byteLength <= BODY_INSPECTION_LIMIT ? new TextDecoder().decode(init.body) : "";
    }
    if (ArrayBuffer.isView(init.body)) {
      return init.body.byteLength <= BODY_INSPECTION_LIMIT ? new TextDecoder().decode(init.body) : "";
    }
    return "";
  }
  if (typeof Request !== "undefined" && input instanceof Request && input.body) {
    try {
      const inspected = await readBoundedStreamText(input.clone().body);
      return inspected ?? "";
    } catch {
      return "";
    }
  }
  return "";
}

async function readBoundedResponseText(response: Response): Promise<string | undefined> {
  const cloned = response.clone();
  if (!cloned.body) return cloned.text();
  return readBoundedStreamText(cloned.body);
}

async function readBoundedStreamText(body: ReadableStream<Uint8Array> | null): Promise<string | undefined> {
  if (!body) return "";
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let totalBytes = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!value || value.byteLength === 0) continue;
      totalBytes += value.byteLength;
      if (totalBytes > BODY_INSPECTION_LIMIT) {
        await reader.cancel();
        return undefined;
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const merged = new Uint8Array(totalBytes);
  let offset = 0;
  for (const chunk of chunks) {
    merged.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(merged);
}

function parseLLMRequest(system: LLMSystem, body: string, captureContent: boolean): LLMRequest {
  const raw = parseObject(body);
  const result: LLMRequest = {
    model: stringValue(raw.model),
    stream: raw.stream === true,
  };
  if (captureContent) {
    const prompt: Record<string, unknown> = { messages: raw.messages };
    if (system === "anthropic" && raw.system !== undefined) prompt.system = raw.system;
    result.prompt = JSON.stringify(prompt);
  }
  return result;
}

function parseLLMResponse(system: LLMSystem, body: string, captureContent: boolean): LLMResponse {
  return parseLLMResponseObject(system, parseObject(body), captureContent);
}

function parseLLMResponseObject(system: LLMSystem, raw: Record<string, unknown>, captureContent: boolean): LLMResponse {
  const result: LLMResponse = { model: stringValue(raw.model), completion: "", completionBytes: 0 };
  if (system === "openai") {
    const usage = objectValue(raw.usage);
    result.inputTokens = numberValue(usage.prompt_tokens) ?? numberValue(usage.input_tokens);
    result.outputTokens = numberValue(usage.completion_tokens) ?? numberValue(usage.output_tokens);
    result.cacheReadTokens =
      numberValue(objectValue(usage.prompt_tokens_details).cached_tokens) ??
      numberValue(usage.cache_read_input_tokens);
    result.reasoningTokens = numberValue(objectValue(usage.completion_tokens_details).reasoning_tokens);
    if (captureContent) appendBoundedCompletion(result, openAICompletion(raw));
  } else if (system === "anthropic") {
    const usage = objectValue(raw.usage);
    result.inputTokens = numberValue(usage.input_tokens);
    result.outputTokens = numberValue(usage.output_tokens);
    result.cacheReadTokens = numberValue(usage.cache_read_input_tokens);
    result.cacheCreationTokens = numberValue(usage.cache_creation_input_tokens);
    if (captureContent) appendBoundedCompletion(result, anthropicCompletion(raw.content));
  } else {
    result.inputTokens = numberValue(raw.prompt_eval_count);
    result.outputTokens = numberValue(raw.eval_count);
    if (captureContent) appendBoundedCompletion(result, stringValue(objectValue(raw.message).content) ?? "");
  }
  return result;
}

function applyLLMResponse(span: Span, response: LLMResponse, captureContent: boolean): void {
  span.setAttributes({
    ...(response.model ? { [GEN_AI_ATTRS.response_model]: response.model } : {}),
    ...(response.inputTokens !== undefined ? { [GEN_AI_ATTRS.usage_input_tokens]: response.inputTokens } : {}),
    ...(response.outputTokens !== undefined ? { [GEN_AI_ATTRS.usage_output_tokens]: response.outputTokens } : {}),
    ...(response.cacheReadTokens !== undefined ? { [GEN_AI_ATTRS.usage_cache_read_input_tokens]: response.cacheReadTokens } : {}),
    ...(response.cacheCreationTokens !== undefined ? { [GEN_AI_ATTRS.usage_cache_creation_input_tokens]: response.cacheCreationTokens } : {}),
    ...(response.reasoningTokens !== undefined ? { [GEN_AI_ATTRS.usage_reasoning_tokens]: response.reasoningTokens } : {}),
    ...(response.inputTokens === undefined || response.outputTokens === undefined ? { [GEN_AI_ATTRS.usage_missing]: true } : {}),
    ...(captureContent && response.completion ? { [GEN_AI_ATTRS.completion]: truncateContent(response.completion) } : {}),
  });
}

function isStreamingResponse(response: Response, requested: boolean): boolean {
  if (!response.ok) return false;
  const contentType = response.headers.get("content-type")?.toLowerCase() ?? "";
  return requested || contentType.includes("text/event-stream") || contentType.includes("application/x-ndjson");
}

function wrapStreamingResponse(response: Response, system: LLMSystem, span: Span, captureContent: boolean): Response {
  const decoder = new TextDecoder();
  let pending = "";
  let discardingLine = false;
  const result: LLMResponse = { completion: "", completionBytes: 0 };
  let ended = false;
  const finish = (error?: unknown) => {
    if (ended) return;
    ended = true;
    if (!discardingLine && pending.trim()) parseStreamLine(result, system, pending, captureContent);
    pending = "";
    if (error !== undefined) {
      span.recordException(asError(error));
      span.setStatus({ code: SpanStatusCode.ERROR, message: asError(error).message });
    }
    applyLLMResponse(span, result, captureContent);
    span.end();
  };
  const reader = response.body!.getReader();
  const processText = (text: string) => {
    let offset = 0;
    while (offset < text.length) {
      const newline = text.indexOf("\n", offset);
      if (discardingLine) {
        if (newline < 0) return;
        offset = newline + 1;
        discardingLine = false;
        continue;
      }
      if (newline < 0) {
        const tail = text.slice(offset);
        if (pending.length + tail.length > BODY_INSPECTION_LIMIT) {
          pending = "";
          discardingLine = true;
          return;
        }
        pending += tail;
        return;
      }
      const part = text.slice(offset, newline);
      offset = newline + 1;
      if (pending.length + part.length > BODY_INSPECTION_LIMIT) {
        pending = "";
        continue;
      }
      const line = pending ? pending + part : part;
      pending = "";
      parseStreamLine(result, system, line, captureContent);
    }
  };
  const stream = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const { done, value } = await reader.read();
        if (done) {
          processText(decoder.decode());
          finish();
          controller.close();
          return;
        }
        processText(decoder.decode(value, { stream: true }));
        controller.enqueue(value);
      } catch (error) {
        finish(error);
        controller.error(error);
      }
    },
    async cancel(reason: unknown) {
      try { await reader.cancel(reason); } finally { finish(reason); }
    },
  });
  return new Response(stream, { status: response.status, statusText: response.statusText, headers: response.headers });
}

function parseStreamLine(result: LLMResponse, system: LLMSystem, value: string, captureContent: boolean): void {
  let line = value.trim();
  if (line.startsWith("data:")) line = line.slice(5).trim();
  if (!line || line === "[DONE]" || !line.startsWith("{")) return;
  const raw = parseObject(line);
  mergeResponse(result, parseLLMResponseObject(system, raw, captureContent), captureContent);
  if (system === "openai" && captureContent) appendBoundedCompletion(result, openAIStreamContent(raw));
  if (system === "anthropic") {
    const message = objectValue(raw.message);
    result.model ??= stringValue(message.model);
    const messageUsage = objectValue(message.usage);
    result.inputTokens = numberValue(messageUsage.input_tokens) ?? result.inputTokens;
    result.cacheReadTokens = numberValue(messageUsage.cache_read_input_tokens) ?? result.cacheReadTokens;
    result.cacheCreationTokens = numberValue(messageUsage.cache_creation_input_tokens) ?? result.cacheCreationTokens;
    const usage = objectValue(raw.usage);
    result.inputTokens = numberValue(usage.input_tokens) ?? result.inputTokens;
    result.outputTokens = numberValue(usage.output_tokens) ?? result.outputTokens;
    result.cacheReadTokens = numberValue(usage.cache_read_input_tokens) ?? result.cacheReadTokens;
    result.cacheCreationTokens = numberValue(usage.cache_creation_input_tokens) ?? result.cacheCreationTokens;
    if (captureContent) appendBoundedCompletion(result, stringValue(objectValue(raw.delta).text) ?? "");
  }
}

function mergeResponse(target: LLMResponse, value: LLMResponse, captureContent: boolean): void {
  if (value.model) target.model = value.model;
  if (value.inputTokens !== undefined) target.inputTokens = value.inputTokens;
  if (value.outputTokens !== undefined) target.outputTokens = value.outputTokens;
  if (value.cacheReadTokens !== undefined) target.cacheReadTokens = value.cacheReadTokens;
  if (value.cacheCreationTokens !== undefined) target.cacheCreationTokens = value.cacheCreationTokens;
  if (value.reasoningTokens !== undefined) target.reasoningTokens = value.reasoningTokens;
  if (captureContent && value.completion) appendBoundedCompletion(target, value.completion);
}

const MAX_RETAINED_COMPLETION_BYTES = CONTENT_LIMIT + 4;
const utf8Encoder = new TextEncoder();

function appendBoundedCompletion(target: LLMResponse, chunk: string): void {
  if (!chunk || target.completionBytes >= MAX_RETAINED_COMPLETION_BYTES) return;
  const chunkBytes = utf8Encoder.encode(chunk);
  if (target.completionBytes + chunkBytes.byteLength <= MAX_RETAINED_COMPLETION_BYTES) {
    target.completion += chunk;
    target.completionBytes += chunkBytes.byteLength;
    return;
  }
  const remaining = MAX_RETAINED_COMPLETION_BYTES - target.completionBytes;
  target.completion += decodeValidPrefix(chunkBytes, remaining);
  target.completionBytes = MAX_RETAINED_COMPLETION_BYTES;
}

function decodeValidPrefix(bytes: Uint8Array, limit: number): string {
  let end = Math.min(bytes.byteLength, limit);
  const decoder = new TextDecoder("utf-8", { fatal: true });
  while (end > 0) {
    try { return decoder.decode(bytes.slice(0, end)); } catch { end -= 1; }
  }
  return "";
}

function openAICompletion(raw: Record<string, unknown>): string {
  return arrayValue(raw.choices).map((choice) => stringValue(objectValue(objectValue(choice).message).content) ?? "").join("");
}
function openAIStreamContent(raw: Record<string, unknown>): string {
  return arrayValue(raw.choices).map((choice) => stringValue(objectValue(objectValue(choice).delta).content) ?? "").join("");
}
function anthropicCompletion(value: unknown): string {
  return arrayValue(value).map((part) => stringValue(objectValue(part).text) ?? "").join("");
}
function parseObject(value: string): Record<string, unknown> {
  try { return objectValue(JSON.parse(value)); } catch { return {}; }
}
function objectValue(value: unknown): Record<string, unknown> { return typeof value === "object" && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : {}; }
function arrayValue(value: unknown): unknown[] { return Array.isArray(value) ? value : []; }
function stringValue(value: unknown): string | undefined { return typeof value === "string" ? value : undefined; }
function numberValue(value: unknown): number | undefined { return typeof value === "number" && Number.isFinite(value) ? value : undefined; }
function asError(value: unknown): Error { return value instanceof Error ? value : new Error(String(value)); }

function truncateContent(value: string): string {
  const bytes = utf8Encoder.encode(value);
  if (bytes.byteLength <= CONTENT_LIMIT) return value;
  return decodeValidPrefix(bytes, CONTENT_LIMIT);
}
