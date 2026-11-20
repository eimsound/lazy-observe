import { SpanStatusCode } from "@opentelemetry/api";
import { InMemorySpanExporter, SimpleSpanProcessor, TracerProvider, type ReadableSpan } from "@opentelemetry/sdk-trace";
import { describe, expect, it, vi } from "vitest";
import { createWrappedLLMFetch, detectLLMSystem } from "../src/llm.js";

function harness(captureContent: boolean, implementation: typeof fetch) {
  const exporter = new InMemorySpanExporter();
  const provider = new TracerProvider({ spanProcessors: [new SimpleSpanProcessor({ exporter })] });
  const wrapped = createWrappedLLMFetch(provider.getTracer("test/llm"), captureContent, implementation);
  return { exporter, provider, wrapped };
}

function onlySpan(exporter: InMemorySpanExporter): ReadableSpan {
  const spans = exporter.getFinishedSpans();
  expect(spans).toHaveLength(1);
  return spans[0]!;
}

describe("LLM fetch instrumentation", () => {
  it("detects OpenAI-compatible, Anthropic, and Ollama paths", () => {
    expect(detectLLMSystem("/v1/chat/completions")).toBe("openai");
    expect(detectLLMSystem("/proxy/v1/messages")).toBe("anthropic");
    expect(detectLLMSystem("/api/chat")).toBe("ollama");
    expect(detectLLMSystem("/v1/models")).toBeUndefined();
    expect(detectLLMSystem("/v1/messages/count_tokens")).toBeUndefined();
    expect(detectLLMSystem("/v1/messages/msg_123")).toBeUndefined();
    expect(detectLLMSystem("/api/chat/history")).toBeUndefined();
    expect(detectLLMSystem("/v1/chat/completions/abc")).toBeUndefined();
  });

  it.each([
    ["openai", "/v1/chat/completions", { model: "gpt-response", choices: [{ message: { content: "hello" } }], usage: { prompt_tokens: 11, completion_tokens: 7 } }, "gpt-response", 11, 7],
    ["anthropic", "/v1/messages", { model: "claude-response", content: [{ type: "text", text: "hello" }], usage: { input_tokens: 13, output_tokens: 9 } }, "claude-response", 13, 9],
    ["ollama", "/api/chat", { model: "llama-response", message: { content: "hello" }, prompt_eval_count: 17, eval_count: 5 }, "llama-response", 17, 5],
  ] as const)("records non-streaming %s metadata and usage", async (system, path, responseBody, responseModel, inputTokens, outputTokens) => {
    const { exporter, provider, wrapped } = harness(false, async () => Response.json(responseBody));
    const response = await wrapped(`http://llm.test${path}`, {
      method: "POST",
      body: JSON.stringify({ model: "request-model", messages: [{ role: "user", content: "secret prompt" }] }),
    });
    await response.text();
    const attrs = onlySpan(exporter).attributes;
    expect(attrs).toMatchObject({
      "gen_ai.operation.name": "chat",
      "gen_ai.provider.name": system,
      "gen_ai.system": system,
      "gen_ai.request.model": "request-model",
      "gen_ai.response.model": responseModel,
      "gen_ai.usage.input_tokens": inputTokens,
      "gen_ai.usage.output_tokens": outputTokens,
    });
    expect(attrs).not.toHaveProperty("gen_ai.prompt");
    expect(attrs).not.toHaveProperty("gen_ai.completion");
    await provider.shutdown();
  });

  it("does not serialize LLM messages when captureContent is false", async () => {
    const requestPayload = JSON.stringify({
      model: "gpt-4o",
      messages: [{ role: "user", content: "large sensitive prompt" }],
      system: "secret system instructions",
    });
    const responsePayload = JSON.stringify({
      model: "gpt-4o",
      choices: [{ message: { content: "ok" } }],
      usage: { prompt_tokens: 10, completion_tokens: 2 },
    });
    const { exporter, provider, wrapped } = harness(false, async () =>
      new Response(responsePayload, { headers: { "content-type": "application/json" } }),
    );

    const stringifySpy = vi.spyOn(JSON, "stringify");
    try {
      const response = await wrapped("http://llm.test/v1/chat/completions", {
        method: "POST",
        body: requestPayload,
      });
      await response.text();
      expect(stringifySpy).not.toHaveBeenCalled();
      const attrs = onlySpan(exporter).attributes;
      expect(attrs["gen_ai.request.model"]).toBe("gpt-4o");
      expect(attrs).not.toHaveProperty("gen_ai.prompt");
    } finally {
      stringifySpy.mockRestore();
      await provider.shutdown();
    }
  });

  it.each([
    ["with usage", "data: {\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"usage\":{\"prompt_tokens\":19,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", false],
    ["without usage", "data: {\"model\":\"gpt-stream\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n", true],
  ])("aggregates streaming usage %s", async (_name, stream, missing) => {
    const { exporter, provider, wrapped } = harness(true, async () => new Response(stream, { headers: { "content-type": "text/event-stream" } }));
    const response = await wrapped("http://llm.test/v1/chat/completions", {
      method: "POST",
      body: JSON.stringify({ model: "gpt-stream", stream: true, messages: [{ role: "user", content: "prompt" }] }),
    });
    expect(exporter.getFinishedSpans()).toHaveLength(0);
    expect(await response.text()).toContain("[DONE]");
    const attrs = onlySpan(exporter).attributes;
    expect(attrs["gen_ai.response.model"]).toBe("gpt-stream");
    expect(attrs["gen_ai.completion"]).toBe("hello");
    if (missing) {
      expect(attrs["gen_ai.usage.missing"]).toBe(true);
      expect(attrs).not.toHaveProperty("gen_ai.usage.input_tokens");
    } else {
      expect(attrs["gen_ai.usage.input_tokens"]).toBe(19);
      expect(attrs["gen_ai.usage.output_tokens"]).toBe(3);
    }
    await provider.shutdown();
  });

  it("captures and truncates content only when enabled", async () => {
    const content = "界".repeat(2_000);
    const { exporter, provider, wrapped } = harness(true, async () => Response.json({ model: "m", choices: [{ message: { content } }], usage: { prompt_tokens: 1, completion_tokens: 2 } }));
    const response = await wrapped("http://llm.test/chat/completions", { method: "POST", body: JSON.stringify({ model: "m", messages: [{ role: "user", content }] }) });
    await response.text();
    const attrs = onlySpan(exporter).attributes;
    for (const key of ["gen_ai.prompt", "gen_ai.completion"]) {
      expect(typeof attrs[key]).toBe("string");
      expect(new TextEncoder().encode(attrs[key] as string).byteLength).toBeLessThanOrEqual(4_096);
      expect(attrs[key] as string).not.toContain("�");
    }
    await provider.shutdown();
  });

  it("passes through non-LLM calls without spans and records fetch errors", async () => {
    const unavailable = new Error("provider unavailable");
    const { exporter, provider, wrapped } = harness(false, async (input) => new URL(typeof input === "string" ? input : input instanceof URL ? input : input.url).pathname === "/health" ? new Response("ok") : Promise.reject(unavailable));
    expect(await (await wrapped("http://llm.test/health")).text()).toBe("ok");
    expect(exporter.getFinishedSpans()).toHaveLength(0);
    await expect(wrapped("http://llm.test/v1/messages", { method: "POST", body: JSON.stringify({ model: "m" }) })).rejects.toThrow("provider unavailable");
    const span = onlySpan(exporter);
    expect(span.status.code).toBe(SpanStatusCode.ERROR);
    expect(span.events.some((event) => event.name === "exception")).toBe(true);
    await provider.shutdown();
  });

  it("ends span immediately on non-2xx streaming responses without leaking", async () => {
    const { exporter, provider, wrapped } = harness(false, async () =>
      new Response(JSON.stringify({ error: { message: "rate limited" } }), {
        status: 429,
        headers: { "content-type": "application/json" },
      }),
    );
    await wrapped("http://llm.test/v1/chat/completions", {
      method: "POST",
      body: JSON.stringify({ model: "gpt-4o", stream: true, messages: [{ role: "user", content: "hi" }] }),
    });
    const span = onlySpan(exporter);
    expect(span.status.code).toBe(SpanStatusCode.ERROR);
    await provider.shutdown();
  });

  it("captures cache and reasoning tokens for OpenAI and Anthropic streams and bounds oversized lines", async () => {
    const oversizedLine = `data: ${"x".repeat((1 << 20) + 1024)}\n`;
    const anthropicStream = [
      oversizedLine,
      `event: message_start`,
      `data: {"type":"message_start","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"cache_creation_input_tokens":5000,"cache_read_input_tokens":20000}}}`,
      ``,
      `event: content_block_delta`,
      `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello cache"}}`,
      ``,
      `event: message_delta`,
      `data: {"type":"message_delta","usage":{"output_tokens":42}}`,
      ``,
    ].join("\n");
    const { exporter, provider, wrapped } = harness(true, async () =>
      new Response(anthropicStream, { headers: { "content-type": "text/event-stream" } }),
    );
    const resp = await wrapped("http://llm.test/v1/messages", {
      method: "POST",
      body: JSON.stringify({ model: "claude-sonnet-4", stream: true, messages: [{ role: "user", content: "hi" }] }),
    });
    await resp.text();
    const attrs = onlySpan(exporter).attributes;
    expect(attrs).toMatchObject({
      "gen_ai.response.model": "claude-sonnet-4-20250514",
      "gen_ai.usage.input_tokens": 100,
      "gen_ai.usage.output_tokens": 42,
      "gen_ai.usage.cache_creation.input_tokens": 5000,
      "gen_ai.usage.cache_read.input_tokens": 20000,
      "gen_ai.completion": "hello cache",
    });
    await provider.shutdown();

    const openai = harness(false, async () =>
      Response.json({
        model: "o3-mini",
        choices: [{ message: { content: "ok" } }],
        usage: {
          prompt_tokens: 1000,
          completion_tokens: 300,
          prompt_tokens_details: { cached_tokens: 600 },
          completion_tokens_details: { reasoning_tokens: 200 },
        },
      }),
    );
    const openaiResp = await openai.wrapped("http://llm.test/v1/chat/completions", {
      method: "POST",
      body: JSON.stringify({ model: "o3-mini", messages: [{ role: "user", content: "hi" }] }),
    });
    await openaiResp.text();
    expect(onlySpan(openai.exporter).attributes).toMatchObject({
      "gen_ai.usage.input_tokens": 1000,
      "gen_ai.usage.output_tokens": 300,
      "gen_ai.usage.cache_read.input_tokens": 600,
      "gen_ai.usage.reasoning_tokens": 200,
    });
    await openai.provider.shutdown();
  });
});
