import { describe, expect, it, vi } from "vitest";
import { Client } from "../src/client";
import { DeliveryQueue, type DeliverySendOptions } from "../src/delivery";

describe("DeliveryQueue", () => {
  it("returns from enqueue immediately and counts an in-flight item against capacity", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const send = vi.fn(async (_batch: number[], _options: DeliverySendOptions) => gate);
    const drops: Array<{ reason: string; count: number }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 1,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      send,
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });

    expect(queue.enqueue(1)).toBe(true);
    expect(queue.enqueue(2)).toBe(false);
    expect(queue.diagnostics()).toMatchObject({ pending: 1, dropped: 1 });
    expect(drops).toEqual([{ reason: "queue_full", count: 1 }]);

    release();
    await queue.flush();
    expect(send).toHaveBeenCalledWith([1], { keepalive: false });
    expect(queue.diagnostics().pending).toBe(0);
  });

  it("retries the same batch with a finite exponential budget", async () => {
    const failure = new Error("offline");
    const send = vi.fn(async (_batch: number[], _options: DeliverySendOptions) => { throw failure; });
    const wait = vi.fn(async (_delay: number) => undefined);
    const drops: Array<{ reason: string; count: number; error?: unknown }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 2,
      batchSize: 2,
      flushIntervalMs: 1,
      maxAttempts: 3,
      retryInitialMs: 10,
      retryMaxMs: 15,
      send,
      wait,
      onDrop: (drop) => drops.push(drop),
    });

    expect(queue.enqueue(7)).toBe(true);
    expect(queue.enqueue(8)).toBe(true);
    await queue.flush();

    expect(send).toHaveBeenCalledTimes(3);
    expect(send.mock.calls.map(([batch]) => batch)).toEqual([[7, 8], [7, 8], [7, 8]]);
    expect(wait.mock.calls.map(([delay]) => delay)).toEqual([10, 15]);
    expect(drops).toEqual([{ reason: "retry_exhausted", count: 2, error: failure }]);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 2, lastError: failure });
  });

  it("marks page lifecycle flushes as keepalive best effort", async () => {
    const send = vi.fn(async (_batch: number[], _options: DeliverySendOptions) => undefined);
    const queue = new DeliveryQueue<number>({
      capacity: 1,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      send,
    });

    queue.enqueue(1);
    await queue.flush(true);

    expect(send).toHaveBeenCalledWith([1], { keepalive: true });
  });

  it("exports a low-cardinality diagnostic when the browser error queue is full", async () => {
    const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => (
      new Response(null, { status: 204 })
    ));
    const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    vi.stubGlobal("fetch", fetcher);
    const client = new Client({
      dsn: "https://observe.example/p/browser-delivery@secret",
      replay: false,
      vitals: false,
      fetchTracing: false,
      serviceName: "checkout-browser",
      errorBatchSize: 1,
      delivery: {
        errorQueueCapacity: 1,
        maxAttempts: 1,
      },
    });

    expect(client.captureException(new Error("first"))).toMatch(/^[0-9a-f-]{36}$/);
    expect(client.captureException(new Error("second"))).toMatch(/^[0-9a-f-]{36}$/);
    expect(client.diagnostics()).toEqual({ droppedTelemetry: 1 });
    expect(warning).toHaveBeenCalledWith(
      "[lazy-observe] error telemetry dropped",
      expect.objectContaining({ reason: "queue_full", count: 1 }),
    );

    await client.flush();
    const diagnosticCall = fetcher.mock.calls.find(([input]) => String(input).endsWith("/api/events/diagnostics"));
    expect(diagnosticCall).toBeDefined();
    expect(JSON.parse(String(diagnosticCall?.[1]?.body))).toEqual({
      measurements: [{
        measurement_id: expect.stringMatching(/^[0-9a-f]{32}$/),
        timestamp: expect.any(String),
        service: "checkout-browser",
        signal: "error",
        reason: "queue_full",
        dropped: 1,
      }],
    });
    await client.close();
  });
});
