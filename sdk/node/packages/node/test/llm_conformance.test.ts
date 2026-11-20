import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { SpanKind, SpanStatusCode } from "@opentelemetry/api";
import { InMemorySpanExporter, SimpleSpanProcessor, TracerProvider, type ReadableSpan } from "@opentelemetry/sdk-trace";
import { describe, expect, it } from "vitest";
import { createWrappedLLMFetch, detectLLMSystem, GEN_AI_ATTRS } from "../src/llm.js";

interface ContractFixture {
  version: number;
  content_limit_bytes: number;
  body_inspection_limit_bytes: number;
  attributes: Record<string, string>;
  path_detection_cases: Array<{ path: string; system: string }>;
}

interface ConformanceCase {
  name: string;
  capture_content: boolean;
  request: {
    method: string;
    path: string;
    body: unknown;
  };
  response: {
    status: number;
    content_type: string;
    body?: string;
    chunks?: string[];
    consume_body: boolean;
  };
  expect: {
    span_name: string;
    span_kind: string;
    status: "UNSET" | "ERROR";
    ended_before_consume: boolean;
    attributes: Record<string, string | number | boolean>;
    absent_attributes?: string[];
    prompt_present?: boolean;
    prompt_contains?: string;
    prompt_max_utf8_bytes?: number;
    completion_exact?: string;
    completion_max_utf8_bytes?: number;
  };
}

interface CasesFixture {
  version: number;
  cases: ConformanceCase[];
}

const FIXTURE_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../../testdata/genai");
const utf8Encoder = new TextEncoder();

function loadFixture<T>(name: string): T {
  const fullPath = path.join(FIXTURE_DIR, name);
  return JSON.parse(readFileSync(fullPath, "utf8")) as T;
}

function expandTemplates(value: string): string {
  return value.replace(/\{\{repeat:([^:}]+):(\d+)\}\}/g, (_match, unit: string, countStr: string) =>
    unit.repeat(Number(countStr)),
  );
}

function onlySpan(exporter: InMemorySpanExporter): ReadableSpan {
  const spans = exporter.getFinishedSpans();
  expect(spans).toHaveLength(1);
  return spans[0]!;
}

const contract = loadFixture<ContractFixture>("contract.json");
const suite = loadFixture<CasesFixture>("cases.json");

describe("GenAI conformance contract (shared across Go/Node/Python SDKs)", () => {
  it("matches canonical gen_ai.* attribute keys", () => {
    expect(GEN_AI_ATTRS).toEqual(contract.attributes);
  });

  it.each(contract.path_detection_cases)("detects LLM system for $path -> $system", ({ path: reqPath, system }) => {
    const got = detectLLMSystem(reqPath) ?? "";
    expect(got).toBe(system);
  });

  it.each(suite.cases)("conforms to shared fixture: $name", async (tc) => {
    const exporter = new InMemorySpanExporter();
    const provider = new TracerProvider({ spanProcessors: [new SimpleSpanProcessor({ exporter })] });
    try {
      const wrapped = createWrappedLLMFetch(provider.getTracer("conformance/llm"), tc.capture_content, async () => {
        const headers = { "content-type": tc.response.content_type };
        if (tc.response.chunks && tc.response.chunks.length > 0) {
          const chunks = tc.response.chunks.map((c) => utf8Encoder.encode(expandTemplates(c)));
          const stream = new ReadableStream<Uint8Array>({
            start(controller) {
              for (const chunk of chunks) controller.enqueue(chunk);
              controller.close();
            },
          });
          return new Response(stream, { status: tc.response.status, headers });
        }
        return new Response(expandTemplates(tc.response.body ?? ""), { status: tc.response.status, headers });
      });

      const reqBody = expandTemplates(JSON.stringify(tc.request.body));
      const response = await wrapped(`http://llm.conformance.test${tc.request.path}`, {
        method: tc.request.method,
        headers: { "content-type": "application/json" },
        body: reqBody,
      });

      if (tc.expect.ended_before_consume) {
        expect(exporter.getFinishedSpans()).toHaveLength(1);
      } else {
        expect(exporter.getFinishedSpans()).toHaveLength(0);
      }

      if (tc.response.consume_body) {
        await response.text();
      } else if (response.body) {
        await response.body.cancel();
      }

      const span = onlySpan(exporter);
      expect(span.name).toBe(tc.expect.span_name);
      if (tc.expect.span_kind === "CLIENT") {
        expect(span.kind).toBe(SpanKind.CLIENT);
      }
      if (tc.expect.status === "ERROR") {
        expect(span.status.code).toBe(SpanStatusCode.ERROR);
      } else {
        expect(span.status.code).not.toBe(SpanStatusCode.ERROR);
      }

      for (const [key, want] of Object.entries(tc.expect.attributes)) {
        expect(span.attributes[key]).toBe(want);
      }
      for (const absentKey of tc.expect.absent_attributes ?? []) {
        expect(span.attributes).not.toHaveProperty(absentKey);
      }

      if (tc.expect.prompt_present) {
        const prompt = span.attributes[GEN_AI_ATTRS.prompt];
        expect(typeof prompt).toBe("string");
        expect((prompt as string).length).toBeGreaterThan(0);
        expect(prompt as string).not.toContain("\uFFFD");
        if (tc.expect.prompt_max_utf8_bytes) {
          expect(utf8Encoder.encode(prompt as string).byteLength).toBeLessThanOrEqual(tc.expect.prompt_max_utf8_bytes);
        }
        if (tc.expect.prompt_contains) {
          expect(prompt as string).toContain(tc.expect.prompt_contains);
        }
      }

      if (tc.expect.completion_exact !== undefined) {
        const wantCompletion = expandTemplates(tc.expect.completion_exact);
        const completion = span.attributes[GEN_AI_ATTRS.completion];
        expect(typeof completion).toBe("string");
        expect(completion as string).not.toContain("\uFFFD");
        if (tc.expect.completion_max_utf8_bytes) {
          expect(utf8Encoder.encode(completion as string).byteLength).toBeLessThanOrEqual(
            tc.expect.completion_max_utf8_bytes,
          );
        }
        expect(completion).toBe(wantCompletion);
      }
    } finally {
      await provider.shutdown();
    }
  });
});
