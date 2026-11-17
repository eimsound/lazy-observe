import { context, SpanKind, SpanStatusCode, trace, type Context, type Span, type Tracer } from "@opentelemetry/api";

const CONTENT_LIMIT = 4 * 1024;
type LLMSystem = "openai" | "anthropic" | "ollama";

interface LLMRequest { model?: string; prompt?: string; stream: boolean }
interface LLMResponse { model?: string; completion: string; inputTokens?: number; outputTokens?: number }
interface LLMContextManager { active(): Context; with<T>(ctx: Context, fn: () => T): T }

export function createWrappedLLMFetch(tracer: Tracer, captureContent: boolean, baseFetch: typeof fetch = globalThis.fetch, contexts: LLMContextManager = context): typeof fetch {
  if (typeof baseFetch !== "function") throw new TypeError("wrapLLMFetch requires a fetch implementation");
  return (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = requestURL(input);
    const system = detectLLMSystem(new URL(url).pathname);
    if (!system) return baseFetch(input, init);

    const body = await requestBody(input, init);
    const request = parseLLMRequest(system, body);
    const attributes: Record<string, string> = {
      "gen_ai.operation.name": "chat",
      "gen_ai.provider.name": system,
      // Retained for compatibility with older OpenTelemetry GenAI consumers.
      "gen_ai.system": system,
      ...(request.model ? { "gen_ai.request.model": request.model } : {}),
      ...(captureContent && request.prompt ? { "gen_ai.prompt": truncateContent(request.prompt) } : {}),
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
      const responseBody = await response.clone().text();
      applyLLMResponse(span, parseLLMResponse(system, responseBody), captureContent);
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
  if (path.includes("/chat/completions")) return "openai";
  if (path.includes("/v1/messages")) return "anthropic";
  if (path.includes("/api/chat")) return "ollama";
  return undefined;
}

function requestURL(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

async function requestBody(input: RequestInfo | URL, init?: RequestInit): Promise<string> {
  if (init?.body != null) {
    if (typeof init.body === "string") return init.body;
    if (init.body instanceof URLSearchParams) return init.body.toString();
    if (init.body instanceof ArrayBuffer) return new TextDecoder().decode(init.body);
    if (ArrayBuffer.isView(init.body)) return new TextDecoder().decode(init.body);
    return "";
  }
  if (typeof Request !== "undefined" && input instanceof Request && input.body) {
    try { return await input.clone().text(); } catch { return ""; }
  }
  return "";
}

function parseLLMRequest(system: LLMSystem, body: string): LLMRequest {
  const raw = parseObject(body);
  const prompt: Record<string, unknown> = { messages: raw.messages };
  if (system === "anthropic" && raw.system !== undefined) prompt.system = raw.system;
  return {
    model: stringValue(raw.model),
    stream: raw.stream === true,
    prompt: JSON.stringify(prompt),
  };
}

function parseLLMResponse(system: LLMSystem, body: string): LLMResponse {
  const raw = parseObject(body);
  const result: LLMResponse = { model: stringValue(raw.model), completion: "" };
  if (system === "openai") {
    const usage = objectValue(raw.usage);
    result.inputTokens = numberValue(usage.prompt_tokens);
    result.outputTokens = numberValue(usage.completion_tokens);
    result.completion = openAICompletion(raw);
  } else if (system === "anthropic") {
    const usage = objectValue(raw.usage);
    result.inputTokens = numberValue(usage.input_tokens);
    result.outputTokens = numberValue(usage.output_tokens);
    result.completion = anthropicCompletion(raw.content);
  } else {
    result.inputTokens = numberValue(raw.prompt_eval_count);
    result.outputTokens = numberValue(raw.eval_count);
    result.completion = stringValue(objectValue(raw.message).content) ?? "";
  }
  return result;
}

function applyLLMResponse(span: Span, response: LLMResponse, captureContent: boolean): void {
  span.setAttributes({
    ...(response.model ? { "gen_ai.response.model": response.model } : {}),
    ...(response.inputTokens !== undefined ? { "gen_ai.usage.input_tokens": response.inputTokens } : {}),
    ...(response.outputTokens !== undefined ? { "gen_ai.usage.output_tokens": response.outputTokens } : {}),
    ...(response.inputTokens === undefined || response.outputTokens === undefined ? { "gen_ai.usage.missing": true } : {}),
    ...(captureContent && response.completion ? { "gen_ai.completion": truncateContent(response.completion) } : {}),
  });
}

function isStreamingResponse(response: Response, requested: boolean): boolean {
  const contentType = response.headers.get("content-type")?.toLowerCase() ?? "";
  return requested || contentType.includes("text/event-stream") || contentType.includes("application/x-ndjson");
}

function wrapStreamingResponse(response: Response, system: LLMSystem, span: Span, captureContent: boolean): Response {
  const decoder = new TextDecoder();
  let pending = "";
  const result: LLMResponse = { completion: "" };
  let ended = false;
  const finish = (error?: unknown) => {
    if (ended) return;
    ended = true;
    if (pending.trim()) parseStreamLine(result, system, pending);
    if (error !== undefined) {
      span.recordException(asError(error));
      span.setStatus({ code: SpanStatusCode.ERROR, message: asError(error).message });
    }
    applyLLMResponse(span, result, captureContent);
    span.end();
  };
  const reader = response.body!.getReader();
  const processChunk = (chunk: Uint8Array) => {
      pending += decoder.decode(chunk, { stream: true });
      let newline: number;
      while ((newline = pending.indexOf("\n")) >= 0) {
        const line = pending.slice(0, newline);
        pending = pending.slice(newline + 1);
        parseStreamLine(result, system, line);
      }
  };
  const stream = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const { done, value } = await reader.read();
        if (done) {
          pending += decoder.decode();
          finish();
          controller.close();
          return;
        }
        processChunk(value);
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

function parseStreamLine(result: LLMResponse, system: LLMSystem, value: string): void {
  let line = value.trim();
  if (line.startsWith("data:")) line = line.slice(5).trim();
  if (!line || line === "[DONE]" || !line.startsWith("{")) return;
  const raw = parseObject(line);
  mergeResponse(result, parseLLMResponse(system, line));
  if (system === "openai") result.completion += openAIStreamContent(raw);
  if (system === "anthropic") {
    const message = objectValue(raw.message);
    result.model ??= stringValue(message.model);
    result.inputTokens ??= numberValue(objectValue(message.usage).input_tokens);
    result.outputTokens = numberValue(objectValue(raw.usage).output_tokens) ?? result.outputTokens;
    result.completion += stringValue(objectValue(raw.delta).text) ?? "";
  }
}

function mergeResponse(target: LLMResponse, value: LLMResponse): void {
  if (value.model) target.model = value.model;
  if (value.inputTokens !== undefined) target.inputTokens = value.inputTokens;
  if (value.outputTokens !== undefined) target.outputTokens = value.outputTokens;
  if (value.completion) target.completion += value.completion;
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
  const bytes = new TextEncoder().encode(value);
  if (bytes.byteLength <= CONTENT_LIMIT) return value;
  let end = CONTENT_LIMIT;
  const decoder = new TextDecoder("utf-8", { fatal: true });
  while (end > 0) {
    try { return decoder.decode(bytes.slice(0, end)); } catch { end -= 1; }
  }
  return "";
}
