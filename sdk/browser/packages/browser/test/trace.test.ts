import { describe, expect, it, vi } from "vitest";
import type { BrowserConfig, ErrorEventPayload } from "../src";
import { init, startSpan } from "../src";
import { createTraceContext, isTraceparent, parseTraceparent, shouldTrace, traceparent } from "../src/trace";

function createCapturingClient(overrides: Partial<BrowserConfig> = {}) {
  const captured: ErrorEventPayload[] = [];
  vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 204 })));
  const client = init({
    dsn: "https://observe.example/p/project@token",
    errors: false,
    fetchTracing: false,
    replay: false,
    vitals: false,
    ...overrides,
    beforeSend(event) {
      captured.push(event);
      return undefined;
    },
  });
  return { captured, client };
}

describe("traceparent", () => {
  it("creates W3C sampled identifiers", () => {
    const context = createTraceContext();
    expect(context.trace_id).toMatch(/^[0-9a-f]{32}$/);
    expect(context.span_id).toMatch(/^[0-9a-f]{16}$/);
    expect(isTraceparent(traceparent(context))).toBe(true);
    expect(parseTraceparent(traceparent(context))).toEqual(context);
    expect(parseTraceparent("invalid")).toBeUndefined();
  });
  it("only traces same-origin and allowlisted targets", () => {
    expect(shouldTrace("/api", [], "https://app.example")).toBe(true);
    expect(shouldTrace("https://other.example/api", [], "https://app.example")).toBe(false);
    expect(shouldTrace("https://api.example/v1", ["https://api.example/"], "https://app.example")).toBe(true);
    expect(shouldTrace("https://api.example.evil.com/v1", ["https://api.example"], "https://app.example")).toBe(false);
    expect(shouldTrace("https://api.example/v1-other", ["https://api.example/v1"], "https://app.example")).toBe(false);
    expect(shouldTrace("https://api.example/v1/orders", ["https://api.example/v1"], "https://app.example")).toBe(true);
  });
});

describe("active spans", () => {
  it("supports synchronous callbacks through the package interface", async () => {
    const { captured, client } = createCapturingClient();

    const result = startSpan({ name: "render" }, (span) => {
      client.captureException(new Error("render failed"));
      expect(captured[0].contexts?.trace).toEqual(span.context);
      return "rendered";
    });
    client.captureException(new Error("outside"));

    expect(result).toBe("rendered");
    expect(captured[1].contexts?.trace).toBeUndefined();
    await client.close();
  });

  it("associates errors only for the asynchronous callback lifetime", async () => {
    const { captured, client } = createCapturingClient();

    await client.startSpan({ name: "checkout" }, async (span) => {
      await Promise.resolve();
      client.captureException(new Error("inside"));
      expect(captured[0].contexts?.trace).toEqual(span.context);
    });
    client.captureException(new Error("outside"));

    expect(captured[1].contexts?.trace).toBeUndefined();
    await client.close();
  });

  it("creates a request child without replacing the active error context", async () => {
    let finishRequest!: (response: Response) => void;
    const pendingResponse = new Promise<Response>((resolve) => { finishRequest = resolve; });
    const fetcherMock = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => pendingResponse);
    const fetcher = fetcherMock as unknown as typeof fetch;
    const { captured, client } = createCapturingClient();

    await client.startSpan({ name: "checkout" }, async (span) => {
      const request = client.wrapFetch(fetcher)("/api/orders");
      const requestHeaders = new Headers(fetcherMock.mock.calls[0][1]?.headers);
      const requestContext = parseTraceparent(requestHeaders.get("traceparent") ?? "");

      expect(requestContext?.trace_id).toBe(span.context.trace_id);
      expect(requestContext?.span_id).not.toBe(span.context.span_id);
      client.captureException(new Error("request pending"));
      expect(captured[0].contexts?.trace).toEqual(span.context);

      finishRequest(new Response(null, { status: 204 }));
      await request;
    });
    await client.close();
  });

  it("uses an explicitly supplied trace context outside an active span", async () => {
    const { captured, client } = createCapturingClient();
    const explicitContext = createTraceContext();

    client.captureException(new Error("delayed framework error"), { trace: explicitContext });

    expect(captured[0].contexts?.trace).toEqual(explicitContext);
    await client.close();
  });

  it("creates an XHR child without replacing the active error context", async () => {
    class FakeXMLHttpRequest extends EventTarget {
      readonly requestHeaders = new Headers();
      status = 200;
      open(_method: string, _url: string | URL): void {}
      send(_body?: Document | XMLHttpRequestBodyInit | null): void {}
      setRequestHeader(name: string, value: string): void { this.requestHeaders.set(name, value); }
    }
    vi.stubGlobal("XMLHttpRequest", FakeXMLHttpRequest);
    const { captured, client } = createCapturingClient({ fetchTracing: true });

    await client.startSpan({ name: "checkout" }, async (span) => {
      const xhr = new FakeXMLHttpRequest();
      xhr.open("POST", "/api/orders");
      xhr.send();
      const requestContext = parseTraceparent(xhr.requestHeaders.get("traceparent") ?? "");

      expect(requestContext?.trace_id).toBe(span.context.trace_id);
      expect(requestContext?.span_id).not.toBe(span.context.span_id);
      client.captureException(new Error("request pending"));
      expect(captured[0].contexts?.trace).toEqual(span.context);

      xhr.dispatchEvent(new Event("loadend"));
    });
    await client.close();
    vi.unstubAllGlobals();
  });

  it("keeps overlapping top-level callbacks in separate traces", async () => {
    let finishFirst!: () => void;
    let finishSecond!: () => void;
    const firstGate = new Promise<void>((resolve) => { finishFirst = resolve; });
    const secondGate = new Promise<void>((resolve) => { finishSecond = resolve; });
    const { captured, client } = createCapturingClient();

    let firstContext = createTraceContext();
    const first = client.startSpan({ name: "first" }, async (span) => {
      firstContext = span.context;
      await firstGate;
      client.captureException(new Error("first"));
    });
    let secondContext = createTraceContext();
    const second = client.startSpan({ name: "second" }, async (span) => {
      secondContext = span.context;
      client.captureException(new Error("second"));
      await secondGate;
    });

    expect(secondContext.trace_id).not.toBe(firstContext.trace_id);
    expect(captured[0].contexts?.trace).toEqual(secondContext);
    finishSecond();
    await second;
    finishFirst();
    await first;
    expect(captured[1].contexts?.trace).toEqual(firstContext);
    await client.close();
  });

  it("inherits and restores nested asynchronous callback contexts", async () => {
    const { captured, client } = createCapturingClient();

    await client.startSpan({ name: "outer" }, async (outer) => {
      await client.startSpan({ name: "inner" }, async (inner) => {
        await Promise.resolve();
        expect(inner.context.trace_id).toBe(outer.context.trace_id);
        expect(inner.context.span_id).not.toBe(outer.context.span_id);
        client.captureException(new Error("inner"));
        expect(captured[0].contexts?.trace).toEqual(inner.context);
      });
      client.captureException(new Error("outer"));
      expect(captured[1].contexts?.trace).toEqual(outer.context);
    });
    await client.close();
  });

  it("does not guess between overlapping asynchronous callbacks", async () => {
    let finishFirst!: () => void;
    let finishSecond!: () => void;
    const firstGate = new Promise<void>((resolve) => { finishFirst = resolve; });
    const secondGate = new Promise<void>((resolve) => { finishSecond = resolve; });
    const { captured, client } = createCapturingClient();

    const first = client.startSpan({ name: "first" }, async () => { await firstGate; });
    const second = client.startSpan({ name: "second" }, async () => { await secondGate; });
    client.captureException(new Error("unrelated"));

    expect(captured[0].contexts?.trace).toBeUndefined();
    finishSecond();
    await second;
    finishFirst();
    await first;
    await client.close();
  });

  it("cleans up the active context when an asynchronous callback rejects", async () => {
    const { captured, client } = createCapturingClient();

    await expect(client.startSpan({ name: "failure" }, async () => {
      await Promise.resolve();
      throw new Error("callback failed");
    })).rejects.toThrow("callback failed");
    client.captureException(new Error("after rejection"));

    expect(captured[0].contexts?.trace).toBeUndefined();
    await client.close();
  });

  it("exports the browser action and request child as one OTLP trace", async () => {
    const fetcherMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => (
      String(input) === "/api/checkout"
        ? new Response(null, { status: 503 })
        : new Response(null, { status: 204 })
    ));
    vi.stubGlobal("fetch", fetcherMock);
    const client = init({
      dsn: "https://observe.example/p/project@token",
      errors: false,
      fetchTracing: false,
      replay: false,
      vitals: false,
      serviceName: "checkout-browser",
    });

    let actionSpanID = "";
    await client.startSpan({ name: "submit checkout" }, async (span) => {
      actionSpanID = span.context.span_id;
      const response = await client.wrapFetch(fetcherMock as unknown as typeof fetch)("/api/checkout");
      expect(response.status).toBe(503);
    });
    await client.flush();

    const traceCall = fetcherMock.mock.calls.find(([input]) => String(input).endsWith("/v1/traces"));
    expect(traceCall).toBeDefined();
    const payload = JSON.parse(String(traceCall?.[1]?.body)) as {
      resourceSpans: Array<{
        resource: { attributes: Array<{ key: string; value: { stringValue: string } }> };
        scopeSpans: Array<{ spans: Array<Record<string, unknown>> }>;
      }>;
    };
    expect(payload.resourceSpans[0].resource.attributes).toContainEqual({
      key: "service.name",
      value: { stringValue: "checkout-browser" },
    });
    expect(payload.resourceSpans[0].scopeSpans[0].spans).toEqual(expect.arrayContaining([
      expect.objectContaining({ name: "submit checkout", spanId: actionSpanID }),
      expect.objectContaining({
        name: "GET /api/checkout",
        parentSpanId: actionSpanID,
        status: { code: 2 },
      }),
    ]));
    await client.close();
    vi.unstubAllGlobals();
  });

  it("marks aborted fetch spans as UNSET with http.aborted while failing network errors remain ERROR", async () => {
    const fetcherMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      if (String(input) === "/api/aborted") {
        const abortErr = new Error("The operation was aborted.");
        abortErr.name = "AbortError";
        throw abortErr;
      }
      if (String(input) === "/api/network-fail") {
        throw new TypeError("Failed to fetch");
      }
      return new Response(null, { status: 204 });
    });
    vi.stubGlobal("fetch", fetcherMock);
    const client = init({
      dsn: "https://observe.example/p/project@token",
      errors: false,
      fetchTracing: false,
      replay: false,
      vitals: false,
      serviceName: "abort-browser",
    });

    await expect(client.wrapFetch(fetcherMock as unknown as typeof fetch)("/api/aborted")).rejects.toMatchObject({
      name: "AbortError",
    });
    await expect(client.wrapFetch(fetcherMock as unknown as typeof fetch)("/api/network-fail")).rejects.toThrow(
      "Failed to fetch",
    );
    await client.flush();

    const traceCall = fetcherMock.mock.calls.find(([input]) => String(input).endsWith("/v1/traces"));
    expect(traceCall).toBeDefined();
    const payload = JSON.parse(String(traceCall?.[1]?.body)) as {
      resourceSpans: Array<{
        scopeSpans: Array<{
          spans: Array<{
            name: string;
            status: { code: number; message?: string };
            attributes: Array<{ key: string; value: { boolValue?: boolean } }>;
          }>;
        }>;
      }>;
    };
    const spans = payload.resourceSpans[0].scopeSpans[0].spans;
    const abortedSpan = spans.find((span) => span.name === "GET /api/aborted");
    const failedSpan = spans.find((span) => span.name === "GET /api/network-fail");
    expect(abortedSpan?.status).toEqual({ code: 0 });
    expect(abortedSpan?.attributes).toContainEqual({ key: "http.aborted", value: { boolValue: true } });
    expect(failedSpan?.status).toEqual({ code: 2, message: "Failed to fetch" });

    await client.close();
    vi.unstubAllGlobals();
  });
});
