import { SpanStatusCode } from "@opentelemetry/api";
import { InMemorySpanExporter, SimpleSpanProcessor, TracerProvider, type ReadableSpan } from "@opentelemetry/sdk-trace";
import { describe, expect, it } from "vitest";
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
});
