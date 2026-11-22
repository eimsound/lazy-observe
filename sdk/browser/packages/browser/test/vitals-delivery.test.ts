import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { Metric } from "web-vitals";

const webVitalCallbacks = vi.hoisted(() => [] as Array<(metric: Metric) => void>);

vi.mock("web-vitals", () => ({
  onCLS: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onFCP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onINP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onLCP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onTTFB: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
}));

import { Client } from "../src/client";
import { DeliveryQueue } from "../src/delivery";
import type { VitalPayload } from "../src/transport";
import { MAX_VITALS_DISTINCT_PAGES_PER_BATCH, vitalDedupeKey, vitalPageKey } from "../src/vitals";

const dsn = "https://observe.example/p/browser-vitals@secret";

beforeEach(() => webVitalCallbacks.splice(0));
afterEach(() => {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function metric(name = "LCP", value = 2500): Metric {
  return { name, value, rating: "needs-improvement" } as Metric;
}

function startClient(fetcher: typeof fetch, delivery: { maxAttempts?: number; vitalQueueCapacity?: number; retryInitialMs?: number; retryMaxMs?: number } = { maxAttempts: 1 }) {
  vi.stubGlobal("fetch", fetcher);
  const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
  const client = new Client({
    dsn,
    replay: false,
    fetchTracing: false,
    serviceName: "checkout-browser",
    delivery,
  });
  client.start();
  return { client, warning };
}

function vitalsCalls(fetcher: ReturnType<typeof vi.fn>) {
  return fetcher.mock.calls.filter(([input]) => String(input).endsWith("/api/events/vitals"));
}

function diagnosticsBody(fetcher: ReturnType<typeof vi.fn>): unknown {
  const call = fetcher.mock.calls.find(([input]) => String(input).endsWith("/api/events/diagnostics"));
  expect(call).toBeDefined();
  return JSON.parse(String(call?.[1]?.body));
}

it("counts a 4xx vitals refusal and reports it as server_rejected without throwing", async () => {
  const fetcher = vi.fn(async (input: RequestInfo | URL) => (
    String(input).endsWith("/api/events/vitals")
      ? new Response(null, { status: 422 })
      : new Response(null, { status: 204 })
  ));
  const { client, warning } = startClient(fetcher as unknown as typeof fetch);

  expect(webVitalCallbacks).toHaveLength(5);
  expect(() => webVitalCallbacks[0](metric())).not.toThrow();

  await client.flush();
  expect(client.diagnostics().droppedTelemetry).toBe(1);
  expect(warning).toHaveBeenCalledWith(
    "[lazy-observe] vitals telemetry dropped",
    expect.objectContaining({ reason: "server_rejected", count: 1 }),
  );
  expect(diagnosticsBody(fetcher)).toEqual({
    measurements: [{
      measurement_id: expect.stringMatching(/^[0-9a-f]{32}$/),
      timestamp: expect.any(String),
      service: "checkout-browser",
      signal: "vitals",
      reason: "server_rejected",
      dropped: 1,
    }],
  });
  await client.close();
});

it.each([
  ["network error", async () => { throw new TypeError("Failed to fetch"); }],
  ["5xx", async () => new Response(null, { status: 503 })],
  ["408", async () => new Response(null, { status: 408 })],
  ["429", async () => new Response(null, { status: 429 })],
])("counts a vitals %s as an exhausted attempt without throwing", async (_name, vitalsResponse) => {
  const fetcher = vi.fn(async (input: RequestInfo | URL) => (
    String(input).endsWith("/api/events/vitals")
      ? vitalsResponse()
      : new Response(null, { status: 204 })
  ));
  const { client, warning } = startClient(fetcher as unknown as typeof fetch);

  expect(() => webVitalCallbacks[0](metric())).not.toThrow();

  await client.flush();
  expect(client.diagnostics().droppedTelemetry).toBe(1);
  expect(warning).toHaveBeenCalledWith(
    "[lazy-observe] vitals telemetry dropped",
    expect.objectContaining({ reason: "retry_exhausted", count: 1 }),
  );
  expect(diagnosticsBody(fetcher)).toEqual({
    measurements: [{
      measurement_id: expect.stringMatching(/^[0-9a-f]{32}$/),
      timestamp: expect.any(String),
      service: "checkout-browser",
      signal: "vitals",
      reason: "retry_exhausted",
      dropped: 1,
    }],
  });
  await client.close();
});

it.each([400, 404, 422])("treats a %s vitals refusal as server_rejected", async (status) => {
  const fetcher = vi.fn(async (input: RequestInfo | URL) => (
    String(input).endsWith("/api/events/vitals")
      ? new Response(null, { status })
      : new Response(null, { status: 204 })
  ));
  const { client, warning } = startClient(fetcher as unknown as typeof fetch);

  webVitalCallbacks[0](metric());

  await client.flush();
  expect(client.diagnostics().droppedTelemetry).toBe(1);
  expect(warning).toHaveBeenCalledWith(
    "[lazy-observe] vitals telemetry dropped",
    expect.objectContaining({ reason: "server_rejected", count: 1 }),
  );
  await client.close();
});

it("does not recurse when the diagnostics report itself is rejected", async () => {
  const fetcher = vi.fn(async (input: RequestInfo | URL) => (
    String(input).endsWith("/api/events/vitals")
      ? new Response(null, { status: 422 })
      : new Response(null, { status: 422 })
  ));
  const { client } = startClient(fetcher as unknown as typeof fetch);

  webVitalCallbacks[0](metric());

  await client.flush();
  await client.flush();
  // One for the rejected vital, one for the diagnostics batch that tried to report it.
  // That second drop is counted locally only, so the loop terminates.
  expect(client.diagnostics().droppedTelemetry).toBe(2);
  const diagnosticsCalls = fetcher.mock.calls.filter(([input]) => (
    String(input).endsWith("/api/events/diagnostics")
  ));
  expect(diagnosticsCalls).toHaveLength(1);
  await client.close();
});

it("sends vitals with keepalive: true on flush(true) and stops reporting after close()", async () => {
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
  const { client } = startClient(fetcher as unknown as typeof fetch);

  webVitalCallbacks[0](metric("CLS", 0.05));
  await client.flush(true);
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(fetcher.mock.calls[0]?.[1]?.keepalive).toBe(true);

  await client.close();

  // Late web-vitals callback after close() must be ignored (no duplicate reports / no leaked client).
  webVitalCallbacks[1](metric("INP", 120));
  await Promise.resolve();
  expect(fetcher).toHaveBeenCalledTimes(1);
});

it("batches Web Vitals on the 1500ms flush interval and immediately when batchSize (10) is reached", async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
  const { client } = startClient(fetcher as unknown as typeof fetch);

  webVitalCallbacks[0](metric("TTFB", 120));
  webVitalCallbacks[1](metric("FCP", 500));
  webVitalCallbacks[2](metric("LCP", 1400));
  expect(vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>)).toHaveLength(0);

  await vi.advanceTimersByTimeAsync(1_499);
  expect(vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>)).toHaveLength(0);

  await vi.advanceTimersByTimeAsync(1);
  const timerCalls = vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>);
  expect(timerCalls).toHaveLength(1);
  const timerBody = JSON.parse(String(timerCalls[0]?.[1]?.body)) as { vitals: VitalPayload[] };
  expect(timerBody.vitals.map((v) => v.name)).toEqual(["TTFB", "FCP", "LCP"]);

  // Now enqueue 10 distinct (name, page) vitals (5 metrics x 2 pages) to trigger batchSize = 10 immediately.
  const names = ["LCP", "CLS", "INP", "TTFB", "FCP"];
  for (const page of ["/p1", "/p2"]) {
    client.setPage(page);
    names.forEach((name, idx) => webVitalCallbacks[idx](metric(name, idx + 1)));
  }
  await Promise.resolve();
  await Promise.resolve();
  const batchCalls = vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>);
  expect(batchCalls).toHaveLength(2);
  const fullBatch = JSON.parse(String(batchCalls[1]?.[1]?.body)) as { vitals: VitalPayload[] };
  expect(fullBatch.vitals).toHaveLength(10);

  await client.close();
});

it("deduplicates queued vitals by (name, page) keeping the latest value and records queue_full drops", async () => {
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
  const { client, warning } = startClient(fetcher as unknown as typeof fetch, { maxAttempts: 1, vitalQueueCapacity: 2 });

  client.setPage("/cart");
  webVitalCallbacks[0](metric("CLS", 0.05));
  webVitalCallbacks[0](metric("CLS", 0.18));
  webVitalCallbacks[1](metric("LCP", 1900));
  // Capacity is 2; updating an existing (name, page) stays in-place without dropping.
  webVitalCallbacks[1](metric("LCP", 2100));
  // A 3rd distinct (name, page) exceeds capacity = 2 and drops as queue_full.
  client.setPage("/checkout");
  webVitalCallbacks[0](metric("CLS", 0.03));

  expect(client.diagnostics().droppedTelemetry).toBe(1);
  expect(warning).toHaveBeenCalledWith(
    "[lazy-observe] vitals telemetry dropped",
    expect.objectContaining({ reason: "queue_full", count: 1 }),
  );

  await client.flush();
  const calls = vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>);
  expect(calls).toHaveLength(1);
  const body = JSON.parse(String(calls[0]?.[1]?.body)) as { vitals: VitalPayload[] };
  expect(body.vitals.map((v) => ({ name: v.name, page: v.page, value: v.value }))).toEqual([
    { name: "CLS", page: "/cart", value: 0.18 },
    { name: "LCP", page: "/cart", value: 2100 },
  ]);
  expect(diagnosticsBody(fetcher)).toEqual({
    measurements: [{
      measurement_id: expect.stringMatching(/^[0-9a-f]{32}$/),
      timestamp: expect.any(String),
      service: "checkout-browser",
      signal: "vitals",
      reason: "queue_full",
      dropped: 1,
    }],
  });
  await client.close();
});

it("splits vitals batches at <= 32 distinct pages to stay within the gateway page limit", async () => {
  const sentBatches: VitalPayload[][] = [];
  const queue = new DeliveryQueue<VitalPayload>({
    capacity: 100,
    batchSize: 100,
    flushIntervalMs: 60_000,
    maxAttempts: 1,
    retryInitialMs: 1,
    retryMaxMs: 1,
    dedupeKey: vitalDedupeKey,
    batchDistinctKey: vitalPageKey,
    maxBatchDistinctKeys: MAX_VITALS_DISTINCT_PAGES_PER_BATCH,
    send: async (batch) => { sentBatches.push(batch); },
  });

  for (let i = 0; i < 32; i++) {
    queue.enqueue({ name: "LCP", value: 1000 + i, page: `/page-${i}`, timestamp: "2026-01-01T00:00:00.000Z" });
  }
  queue.enqueue({ name: "CLS", value: 0.01, page: "/page-0", timestamp: "2026-01-01T00:00:00.000Z" });
  for (let i = 32; i < 35; i++) {
    queue.enqueue({ name: "LCP", value: 1000 + i, page: `/page-${i}`, timestamp: "2026-01-01T00:00:00.000Z" });
  }

  await queue.flush();
  expect(sentBatches).toHaveLength(2);
  expect(sentBatches[0]).toHaveLength(33);
  expect(new Set(sentBatches[0].map((v) => v.page)).size).toBe(32);
  expect(sentBatches[1]).toHaveLength(3);
  expect(new Set(sentBatches[1].map((v) => v.page)).size).toBe(3);

  // End-to-end via Client across 35 distinct pages: every POST stays <= 32 distinct pages.
  const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
  const { client } = startClient(fetcher as unknown as typeof fetch);
  for (let i = 0; i < 35; i++) {
    client.setPage(`/route-${i}`);
    webVitalCallbacks[0](metric("LCP", 1000 + i));
  }
  await client.flush();
  const requests = vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>);
  expect(requests.length).toBeGreaterThanOrEqual(2);
  const seenPages = new Set<string>();
  for (const [, init] of requests) {
    const batch = (JSON.parse(String(init?.body)) as { vitals: VitalPayload[] }).vitals;
    const batchPages = new Set(batch.map((v) => v.page));
    expect(batchPages.size).toBeLessThanOrEqual(MAX_VITALS_DISTINCT_PAGES_PER_BATCH);
    batchPages.forEach((p) => seenPages.add(p));
  }
  expect(seenPages.size).toBe(35);
  await client.close();
});

it("retries transient vitals failures and keeps visibilitychange/pagehide keepalive flushes unblocked", async () => {
  let vitalsAttempt = 0;
  const fetcher = vi.fn(async (input: RequestInfo | URL) => {
    if (String(input).endsWith("/api/events/vitals")) {
      vitalsAttempt++;
      if (vitalsAttempt === 1) return new Response(null, { status: 503 });
    }
    return new Response(null, { status: 204 });
  });
  const { client } = startClient(fetcher as unknown as typeof fetch, {
    maxAttempts: 2,
    retryInitialMs: 5_000,
    retryMaxMs: 5_000,
  });

  client.setPage("/initial");
  webVitalCallbacks[0](metric("LCP", 1500));
  // Start an async flush that hits 503 on batch 1 and enters the 5s retry backoff.
  const stalledFlush = client.flush(false);
  await vi.waitFor(() => expect(vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>)).toHaveLength(1));

  // While batch 1 is waiting in retry backoff, report another vital and hide the page.
  client.setPage("/checkout");
  webVitalCallbacks[1](metric("INP", 95));
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
  document.dispatchEvent(new Event("visibilitychange"));

  // visibilitychange must dispatch fetch(..., { keepalive: true }) synchronously without waiting 5s.
  const callsAfterHide = vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>);
  expect(callsAfterHide).toHaveLength(2);
  expect(callsAfterHide[1]?.[1]?.keepalive).toBe(true);
  expect(JSON.parse(String(callsAfterHide[1]?.[1]?.body))).toMatchObject({
    vitals: [expect.objectContaining({ name: "INP", page: "/checkout", value: 95 })],
  });

  // pagehide also triggers synchronous keepalive dispatch for newly queued vitals.
  webVitalCallbacks[2](metric("CLS", 0.04));
  window.dispatchEvent(new Event("pagehide"));
  const callsAfterPagehide = vitalsCalls(fetcher as unknown as ReturnType<typeof vi.fn>);
  expect(callsAfterPagehide).toHaveLength(3);
  expect(callsAfterPagehide[2]?.[1]?.keepalive).toBe(true);
  expect(JSON.parse(String(callsAfterPagehide[2]?.[1]?.body))).toMatchObject({
    vitals: [expect.objectContaining({ name: "CLS", page: "/checkout", value: 0.04 })],
  });

  await client.close({ timeoutMs: 25 });
  await stalledFlush;
});
