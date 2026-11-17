import assert from "node:assert/strict";
import test from "node:test";

import {
  extractGenAISpanDetails,
  llmRangeParams,
  llmStep,
  normalizeLLMCallsResponse,
  normalizeLLMSummaryResponse,
  normalizeLLMTimeseriesResponse,
} from "./llm-contract.ts";
import {
  mockLLMCallsResponse,
  mockLLMSummaryResponse,
  mockLLMTimeseriesResponse,
} from "./mock-llm.ts";

const window = { start: 1_784_368_800, end: 1_784_455_200 };

test("normalizes the gateway LLM summary, calls, and trend contracts", () => {
  const summary = normalizeLLMSummaryResponse({
    total_calls: 2,
    total_input_tokens: 2_100,
    total_output_tokens: 520,
    total_cost: 0.00066,
    unpriced: true,
    by_model: [
      {
        model: "gpt-4o-mini-2024-07-18",
        calls: 1,
        input_tokens: 2_000,
        output_tokens: 500,
        cost: 0.0006,
        unpriced: false,
      },
      {
        model: "private-model",
        calls: 1,
        input_tokens: 100,
        output_tokens: 20,
        cost: 0,
        unpriced: true,
      },
    ],
    by_service: [
      {
        service: "checkout-ai",
        calls: 2,
        input_tokens: 2_100,
        output_tokens: 520,
        cost: 0.00066,
        unpriced: true,
      },
    ],
    truncated: false,
  });
  assert.equal(summary.calls, 2);
  assert.equal(summary.by_model[1]?.unpriced, true);

  const calls = normalizeLLMCallsResponse({
    calls: [
      {
        time: "2026-07-18T10:00:00Z",
        service: "checkout-ai",
        model: "gpt-4o-mini-2024-07-18",
        operation: "chat",
        input_tokens: 2_000,
        output_tokens: 500,
        cost: 0.0006,
        unpriced: false,
        duration: 125_000,
        trace_id: "33333333333333333333333333333333",
        status: "ok",
      },
    ],
    count: 1,
    limit: 100,
    truncated: false,
  });
  assert.equal(calls.calls[0]?.duration, 125_000);

  const trend = normalizeLLMTimeseriesResponse({
    metric: "tokens",
    step: "5m0s",
    points: [
      { time: "2026-07-18T10:00:00Z", value: 2_500 },
      { time: "2026-07-18T10:05:00Z", value: 120 },
    ],
    truncated: false,
  });
  assert.deepEqual(trend.points.map((point) => point.value), [2_500, 120]);
});

test("rejects legacy or internally inconsistent LLM envelopes", () => {
  assert.throws(() => normalizeLLMSummaryResponse([]), /must be an object/);
  assert.throws(
    () =>
      normalizeLLMSummaryResponse({
        calls: 0,
        input_tokens: 0,
        output_tokens: 0,
        cost: 0,
        unpriced: false,
        by_model: [],
        by_service: [],
        truncated: false,
      }),
    /calls must be a non-negative safe integer/,
  );
  assert.throws(
    () =>
      normalizeLLMCallsResponse({
        calls: [],
        count: 1,
        limit: 100,
        truncated: false,
      }),
    /count must match/,
  );
  assert.throws(
    () =>
      normalizeLLMCallsResponse({
        calls: [{
          time: "2026-07-18T10:00:00Z",
          service: "api",
          model: "model",
          operation: "chat",
          input_tokens: 1,
          output_tokens: 1,
          cost: 0,
          unpriced: true,
          duration: 1,
          trace_id: "1234567890abcdef12345678",
          status: "ok",
        }],
        count: 1,
        limit: 100,
        truncated: false,
      }),
    /64-bit or 128-bit/,
  );
  assert.throws(
    () =>
      normalizeLLMTimeseriesResponse({
        metric: "latency",
        step: "1m0s",
        points: [],
        truncated: false,
      }),
    /metric is invalid/,
  );
  assert.throws(
    () =>
      normalizeLLMTimeseriesResponse({
        metric: "calls",
        step: "1m0s",
        points: [
          { time: "2026-07-18T10:01:00Z", value: 1 },
          { time: "2026-07-18T10:00:00Z", value: 1 },
        ],
        truncated: false,
      }),
    /strictly ordered/,
  );
});

test("builds protected range and trend parameters", () => {
  assert.deepEqual(llmRangeParams(window), window);
  assert.equal(llmStep(window), "720s");
  assert.throws(
    () => llmRangeParams({ start: window.start - 8 * 24 * 60 * 60, end: window.end }),
    /no longer than 7d/,
  );
});

test("mock mode obeys the same strict contracts and supports cancellation", async () => {
  const summary = normalizeLLMSummaryResponse(await mockLLMSummaryResponse(window));
  const calls = normalizeLLMCallsResponse(await mockLLMCallsResponse(window, { limit: 10 }));
  const trend = normalizeLLMTimeseriesResponse(
    await mockLLMTimeseriesResponse(window, "cost", llmStep(window)),
  );
  assert.ok(summary.calls > 0);
  assert.equal(calls.calls.length, 10);
  assert.ok(trend.points.length > 0);

  const controller = new AbortController();
  const pending = mockLLMSummaryResponse(window, controller.signal);
  controller.abort();
  await assert.rejects(
    pending,
    (error: unknown) => error instanceof DOMException && error.name === "AbortError",
  );
});

test("trace UI prefers current GenAI attributes and reads privacy-gated legacy content", () => {
  assert.deepEqual(
    extractGenAISpanDetails([
      { key: "gen_ai.operation.name", value: "chat" },
      { key: "gen_ai.provider.name", value: "openai" },
      { key: "gen_ai.system", value: "legacy-provider" },
      { key: "gen_ai.request.model", value: "gpt-request" },
      { key: "gen_ai.response.model", value: "gpt-response" },
      { key: "gen_ai.usage.input_tokens", value: "23" },
      { key: "gen_ai.usage.output_tokens", value: 4 },
      { key: "gen_ai.input.messages", value: '[{"role":"user"}]' },
      { key: "gen_ai.completion", value: "legacy completion" },
    ]),
    {
      operation: "chat",
      provider: "openai",
      requestModel: "gpt-request",
      responseModel: "gpt-response",
      inputTokens: 23,
      outputTokens: 4,
      inputContent: '[{"role":"user"}]',
      outputContent: "legacy completion",
      hasCapturedContent: true,
    },
  );
  assert.equal(extractGenAISpanDetails([{ key: "http.method", value: "GET" }]), null);
});
