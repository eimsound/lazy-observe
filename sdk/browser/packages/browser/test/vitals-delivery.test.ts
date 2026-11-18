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

const dsn = "https://observe.example/p/browser-vitals@secret";

beforeEach(() => webVitalCallbacks.splice(0));
afterEach(() => vi.unstubAllGlobals());

function metric(name = "LCP"): Metric {
  return { name, value: 2500, rating: "needs-improvement" } as Metric;
}

function startClient(fetcher: typeof fetch) {
  vi.stubGlobal("fetch", fetcher);
  const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
  const client = new Client({
    dsn,
    replay: false,
    fetchTracing: false,
    serviceName: "checkout-browser",
    delivery: { maxAttempts: 1 },
  });
  client.start();
  return { client, warning };
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

  await vi.waitFor(() => expect(client.diagnostics().droppedTelemetry).toBe(1));
  expect(warning).toHaveBeenCalledWith(
    "[lazy-observe] vitals telemetry dropped",
    expect.objectContaining({ reason: "server_rejected", count: 1 }),
  );

  await client.flush();
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
])("counts a vitals %s as an exhausted attempt without throwing", async (_name, vitalsResponse) => {
  const fetcher = vi.fn(async (input: RequestInfo | URL) => (
    String(input).endsWith("/api/events/vitals")
      ? vitalsResponse()
      : new Response(null, { status: 204 })
  ));
  const { client, warning } = startClient(fetcher as unknown as typeof fetch);

  expect(() => webVitalCallbacks[0](metric())).not.toThrow();

  await vi.waitFor(() => expect(client.diagnostics().droppedTelemetry).toBe(1));
  expect(warning).toHaveBeenCalledWith(
    "[lazy-observe] vitals telemetry dropped",
    expect.objectContaining({ reason: "retry_exhausted", count: 1 }),
  );

  await client.flush();
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

it("does not recurse when the diagnostics report itself is rejected", async () => {
  const fetcher = vi.fn(async (input: RequestInfo | URL) => (
    String(input).endsWith("/api/events/vitals")
      ? new Response(null, { status: 422 })
      : new Response(null, { status: 422 })
  ));
  const { client } = startClient(fetcher as unknown as typeof fetch);

  webVitalCallbacks[0](metric());

  await vi.waitFor(() => expect(client.diagnostics().droppedTelemetry).toBe(2));
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
