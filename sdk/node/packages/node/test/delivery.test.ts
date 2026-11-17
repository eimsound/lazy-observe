import { describe, expect, it, vi } from "vitest";
import { DeliveryQueue } from "../src/delivery.js";
import { init } from "../src/sdk.js";

describe("DeliveryQueue", () => {
  it("returns immediately and counts an in-flight item against capacity", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const send = vi.fn(async (_batch: number[], _signal: AbortSignal) => gate);
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
    await queue.close({ timeoutMs: 100 });
    expect(send).toHaveBeenCalledTimes(1);
  });

  it("retries the same batch and returns the terminal export error", async () => {
    const failure = new Error("offline");
    const send = vi.fn(async (_batch: number[], _signal: AbortSignal) => { throw failure; });
    const wait = vi.fn(async (_delay: number, _signal: AbortSignal) => undefined);
    const drops: Array<{ reason: string; count: number }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 2,
      batchSize: 2,
      flushIntervalMs: 1,
      maxAttempts: 3,
      retryInitialMs: 10,
      retryMaxMs: 15,
      send,
      wait,
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });

    queue.enqueue(7);
    queue.enqueue(8);
    await expect(queue.close({ timeoutMs: 100 })).rejects.toThrow("offline");

    expect(send).toHaveBeenCalledTimes(3);
    expect(send.mock.calls.map(([batch]) => batch)).toEqual([[7, 8], [7, 8], [7, 8]]);
    expect(wait.mock.calls.map(([delay]) => delay)).toEqual([10, 15]);
    expect(drops).toEqual([{ reason: "retry_exhausted", count: 2 }]);
  });

  it("aborts a hung exporter when the shutdown budget expires", async () => {
    const send = vi.fn(async (_batch: number[], _signal: AbortSignal) => new Promise<void>(() => {}));
    const drops: Array<{ reason: string; count: number }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 1,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 3,
      retryInitialMs: 10,
      retryMaxMs: 20,
      send,
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });
    queue.enqueue(1);

    const started = Date.now();
    await expect(queue.close({ timeoutMs: 20 })).rejects.toThrow("timed out");

    expect(Date.now() - started).toBeLessThan(200);
    expect(drops).toEqual([{ reason: "shutdown_budget", count: 1 }]);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 1 });
  });
});

it("queues reportError without awaiting export and exposes local drops", async () => {
  const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const fetcher = vi.fn(async (_input: string | URL | Request, _init?: RequestInit) => {
    await gate;
    return new Response(null, { status: 204 });
  });
  vi.stubGlobal("fetch", fetcher);
  const sdk = init({
    dsn: "https://observe.example/p/node-delivery@secret",
    serviceName: "checkout-node",
    metrics: false,
    logs: false,
    traces: false,
    profiling: false,
    delivery: { errorQueueCapacity: 1, maxAttempts: 1 },
  });

  let firstSettled = false;
  void sdk.reportError(new Error("first")).then(() => { firstSettled = true; });
  await Promise.resolve();
  const settledBeforeExport = firstSettled;
  await sdk.reportError(new Error("second"));
  const diagnostics = sdk.diagnostics();
  release();
  await sdk.shutdown({ timeoutMs: 500 });

  expect(settledBeforeExport).toBe(true);
  expect(diagnostics).toEqual({ droppedTelemetry: 1 });
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(warning).toHaveBeenCalledWith(
    "lazy-observe error telemetry dropped",
    expect.objectContaining({ reason: "queue_full", count: 1 }),
  );
});

it("returns the terminal custom export failure from SDK shutdown", async () => {
  vi.spyOn(console, "warn").mockImplementation(() => undefined);
  vi.stubGlobal("fetch", vi.fn(async (_input: string | URL | Request, _init?: RequestInit) => (
    new Response("offline", { status: 503 })
  )));
  const sdk = init({
    dsn: "https://observe.example/p/node-delivery@secret",
    metrics: false,
    logs: false,
    traces: false,
    profiling: false,
    delivery: { maxAttempts: 1 },
  });

  await sdk.reportError(new Error("failure"));

  await expect(sdk.shutdown({ timeoutMs: 500 })).rejects.toThrow("HTTP 503");
  expect(sdk.diagnostics()).toEqual({ droppedTelemetry: 1 });
});
