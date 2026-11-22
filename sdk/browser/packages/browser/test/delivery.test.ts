import { afterEach, describe, expect, it, vi } from "vitest";
import { Client, trimErrorEventToBudget } from "../src/client";
import { DeliveryQueue, type DeliverySendOptions } from "../src/delivery";
import { MAX_KEEPALIVE_BYTES, parseRetryAfterMs, Transport, TransportError } from "../src/transport";

afterEach(() => {
  vi.unstubAllGlobals();
});

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
    expect(send).toHaveBeenCalledWith([1], { keepalive: false, signal: expect.any(AbortSignal) });
    expect(queue.diagnostics().pending).toBe(0);
  });

  it("retries the same batch with a finite jittered exponential budget", async () => {
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
    const delays = wait.mock.calls.map(([delay]) => delay);
    expect(delays).toHaveLength(2);
    expect(delays[0]).toBeGreaterThanOrEqual(0);
    expect(delays[0]).toBeLessThanOrEqual(10);
    expect(delays[1]).toBeGreaterThanOrEqual(0);
    expect(delays[1]).toBeLessThanOrEqual(15);
    expect(drops).toEqual([{ reason: "retry_exhausted", count: 2, error: failure }]);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 2, lastError: failure });
  });

  it("applies Full Jitter backoff in [0, min(retryMaxMs, retryInitialMs * 2^attempt)]", async () => {
    const samples = [0, 0.5, 1];
    let idx = 0;
    const wait = vi.fn(async (_delay: number) => undefined);
    const queue = new DeliveryQueue<number>({
      capacity: 2,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 4,
      retryInitialMs: 100,
      retryMaxMs: 250,
      random: () => samples[idx++] ?? 0.5,
      send: async () => { throw new TransportError(503, "unavailable"); },
      wait,
    });

    queue.enqueue(1);
    await queue.flush();
    expect(wait.mock.calls.map(([delay]) => delay)).toEqual([0, 100, 250]);
  });

  it("splits batches by keepalive (64KB) and normal (200KB) byte budgets and retries per-attempt timeouts", async () => {
    const keepaliveBatches: string[][] = [];
    const keepaliveQueue = new DeliveryQueue<string>({
      capacity: 10,
      batchSize: 10,
      flushIntervalMs: 60_000,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      maxBatchBytes: 200 * 1024,
      keepaliveMaxBatchBytes: MAX_KEEPALIVE_BYTES,
      batchOverheadBytes: 16,
      estimateItemBytes: (item) => new TextEncoder().encode(item).byteLength,
      send: async (batch) => { keepaliveBatches.push(batch); },
    });

    // Three 35KB items fit inside one 200KB normal batch, but under keepalive (60KB) must split into 3 batches.
    keepaliveQueue.enqueue("a".repeat(35 * 1024));
    keepaliveQueue.enqueue("b".repeat(35 * 1024));
    keepaliveQueue.enqueue("c".repeat(35 * 1024));
    await keepaliveQueue.flush(true);
    expect(keepaliveBatches.map((batch) => batch.length)).toEqual([1, 1, 1]);

    const normalBatches: string[][] = [];
    const normalQueue = new DeliveryQueue<string>({
      capacity: 10,
      batchSize: 10,
      flushIntervalMs: 60_000,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      maxBatchBytes: 200 * 1024,
      batchOverheadBytes: 16,
      estimateItemBytes: (item) => new TextEncoder().encode(item).byteLength,
      send: async (batch) => { normalBatches.push(batch); },
    });
    // Three 90KB items exceed 200KB together -> split into [2, 1]
    normalQueue.enqueue("x".repeat(90 * 1024));
    normalQueue.enqueue("y".repeat(90 * 1024));
    normalQueue.enqueue("z".repeat(90 * 1024));
    await normalQueue.flush(false);
    expect(normalBatches.map((batch) => batch.length)).toEqual([2, 1]);

    let attempts = 0;
    const timeoutQueue = new DeliveryQueue<number>({
      capacity: 2,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 2,
      retryInitialMs: 1,
      retryMaxMs: 1,
      requestTimeoutMs: 20,
      wait: async () => undefined,
      send: async (_batch, options) => {
        attempts++;
        if (attempts === 1) {
          await new Promise<void>((_resolve, reject) => {
            options.signal.addEventListener("abort", () => reject(options.signal.reason), { once: true });
          });
        }
      },
    });
    timeoutQueue.enqueue(1);
    await timeoutQueue.close({ timeoutMs: 500 });
    expect(attempts).toBe(2);
    expect(timeoutQueue.diagnostics().dropped).toBe(0);
  });

  it("aborts a hung delivery when close({ timeoutMs }) expires without throwing into page code", async () => {
    const drops: Array<{ reason: string; count: number }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 1,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 3,
      retryInitialMs: 10,
      retryMaxMs: 20,
      send: async () => new Promise<void>(() => {}),
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });
    queue.enqueue(1);

    const started = Date.now();
    await expect(queue.close({ timeoutMs: 25 })).resolves.toBeUndefined();
    expect(Date.now() - started).toBeLessThan(250);
    expect(drops).toEqual([{ reason: "shutdown_budget", count: 1 }]);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 1 });
  });

  it.each([400, 401, 403, 404, 413, 422])("drops permanent HTTP %s immediately as server_rejected without retrying", async (status) => {
    const rejection = new TransportError(status, `HTTP ${status}`);
    const send = vi.fn(async () => { throw rejection; });
    const wait = vi.fn(async () => undefined);
    const drops: Array<{ reason: string; count: number }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 2,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 3,
      retryInitialMs: 10,
      retryMaxMs: 20,
      send,
      wait,
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });

    queue.enqueue(1);
    await queue.flush();

    expect(send).toHaveBeenCalledTimes(1);
    expect(wait).not.toHaveBeenCalled();
    expect(drops).toEqual([{ reason: "server_rejected", count: 1 }]);
  });

  it("retries 429 and 408 and honors Retry-After capped at maxRetryAfterMs", async () => {
    let calls = 0;
    const send = vi.fn(async () => {
      calls++;
      if (calls === 1) throw new TransportError(429, "rate limited", 4_500);
      if (calls === 2) throw new TransportError(408, "timeout", 120_000);
    });
    const wait = vi.fn(async (_delay: number) => undefined);
    const queue = new DeliveryQueue<number>({
      capacity: 2,
      batchSize: 1,
      flushIntervalMs: 1,
      maxAttempts: 3,
      retryInitialMs: 10,
      retryMaxMs: 20,
      maxRetryAfterMs: 30_000,
      send,
      wait,
    });

    queue.enqueue(1);
    await queue.flush();

    expect(send).toHaveBeenCalledTimes(3);
    expect(wait.mock.calls.map(([delay]) => delay)).toEqual([4_500, 30_000]);
    expect(queue.diagnostics().lastError).toBeUndefined();
  });

  it("parses Retry-After delta-seconds and HTTP-date headers", () => {
    const now = Date.parse("2026-01-01T00:00:00Z");
    expect(parseRetryAfterMs("5", now)).toBe(5_000);
    expect(parseRetryAfterMs("Thu, 01 Jan 2026 00:00:12 GMT", now)).toBe(12_000);
    expect(parseRetryAfterMs("not-a-date", now)).toBeUndefined();
  });

  it("downgrades keepalive to false when payload exceeds the 64KB browser budget and aborts on timeout", async () => {
    const seen: Array<RequestInit | undefined> = [];
    const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(init);
      return new Response(null, { status: 204 });
    });
    const transport = new Transport(
      { endpoint: "https://observe.example", project: "p", token: "lo_tok" },
      fetcher as unknown as typeof fetch,
      50,
    );

    await transport.diagnostics([], true);
    expect(seen[0]?.keepalive).toBe(true);

    const bigBody = "x".repeat(MAX_KEEPALIVE_BYTES + 4_096);
    await transport.replay("s", 0, bigBody, false, false, true);
    expect(seen[1]?.keepalive).toBe(false);
    expect(seen[1]?.signal).toBeInstanceOf(AbortSignal);

    const hangingTransport = new Transport(
      { endpoint: "https://observe.example", project: "p", token: "lo_tok" },
      ((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () => reject(new Error("aborted by timeout")));
      })) as unknown as typeof fetch,
      20,
    );
    await expect(hangingTransport.diagnostics([], false)).rejects.toThrow("aborted by timeout");
  });

  it("uploads replay segments to fixed /api/replay/segments with metadata headers", async () => {
    const calls: Array<{ url: string; init: RequestInit | undefined }> = [];
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init });
      return new Response(null, { status: 202 });
    });
    const transport = new Transport(
      { endpoint: "https://observe.example", project: "p", token: "lo_tok" },
      fetcher as unknown as typeof fetch,
    );

    const gzBody = new Uint8Array([0x1f, 0x8b, 0x08, 0x00]);
    await transport.replay("sess-fixed", 3, gzBody, true, true, false);
    expect(calls[0]?.url).toBe("https://observe.example/api/replay/segments");
    expect(calls[0]?.init?.headers).toEqual({
      Authorization: "Bearer lo_tok",
      "Content-Type": "application/json",
      "X-LO-Replay-Session": "sess-fixed",
      "X-LO-Replay-Seq": "3",
      "Content-Encoding": "gzip",
      "X-LO-Replay-Error": "1",
    });

    await transport.replay("sess-fixed", 4, "[]", false, false, true);
    expect(calls[1]?.url).toBe("https://observe.example/api/replay/segments");
    expect(calls[1]?.init?.keepalive).toBe(true);
    expect(calls[1]?.init?.headers).toEqual({
      Authorization: "Bearer lo_tok",
      "Content-Type": "application/json",
      "X-LO-Replay-Session": "sess-fixed",
      "X-LO-Replay-Seq": "4",
      "X-LO-Replay-Compression": "identity",
    });
  });

  it("trims oversized >200KB error events so a single event never poisons the batch", () => {
    const trimmed = trimErrorEventToBudget({
      event_id: "11111111-1111-4111-a111-111111111111",
      timestamp: new Date().toISOString(),
      platform: "javascript",
      level: "error",
      message: "m".repeat(150_000),
      exception: {
        type: "Error",
        value: "v".repeat(150_000),
        stacktrace: { frames: [{ function: "fn", filename: "app.js", lineno: 1, colno: 1, in_app: true }] },
      },
      tags: { "react.component_stack": "c".repeat(120_000) },
    });
    expect(trimmed).toBeDefined();
    expect(new TextEncoder().encode(JSON.stringify(trimmed)).byteLength).toBeLessThanOrEqual(200 * 1024);
  });

  it("preserves window.onerror filename/lineno/colno for non-Error events, strips URL query/hash, formats objects, and flushes on visibilitychange", async () => {
    const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);
    window.history.replaceState({}, "", "/checkout?token=secret-query#frag=secret");

    const captured: Array<Record<string, unknown>> = [];
    const client = new Client({
      dsn: "https://observe.example/p/browser-delivery@secret",
      replay: false,
      vitals: false,
      fetchTracing: false,
      errorBatchSize: 10,
      errorFlushIntervalMs: 60_000,
      beforeSend(event) {
        captured.push(event as unknown as Record<string, unknown>);
        return event;
      },
    }).start();

    // Non-Error window.onerror event
    window.dispatchEvent(new ErrorEvent("error", {
      message: "Script failure",
      filename: "https://app.example/assets/bundle.js",
      lineno: 42,
      colno: 9,
      error: null,
    }));
    // Plain object capture
    client.captureException({ code: 400, detail: "bad card" });

    expect(captured).toHaveLength(2);
    const first = captured[0] as { url?: string; exception: { stacktrace: { frames: Array<Record<string, unknown>> } } };
    expect(first.url).not.toContain("secret-query");
    expect(first.url).not.toContain("secret");
    expect(first.exception.stacktrace.frames).toEqual([
      expect.objectContaining({ filename: "https://app.example/assets/bundle.js", lineno: 42, colno: 9 }),
    ]);
    const second = captured[1] as { message?: string };
    expect(second.message).not.toBe("[object Object]");
    expect(second.message).toContain("bad card");

    // visibilitychange hidden triggers keepalive flush
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "hidden" });
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.waitFor(() => {
      const errorCall = fetcher.mock.calls.find(([input]) => String(input).endsWith("/api/events/errors"));
      expect(errorCall).toBeDefined();
      expect(errorCall?.[1]?.keepalive).toBe(true);
    });

    await client.close();
  });

  it("does not clobber third-party fetch/XHR patches on close() and avoids duplicating XHR traceparent", async () => {
    const baseFetch = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", baseFetch);

    class FakeXHR extends EventTarget {
      readonly headers: Array<[string, string]> = [];
      status = 200;
      open(_method: string, _url: string | URL): void {}
      send(_body?: Document | XMLHttpRequestBodyInit | null): void {}
      setRequestHeader(name: string, value: string): void { this.headers.push([name, value]); }
    }
    vi.stubGlobal("XMLHttpRequest", FakeXHR);

    const client = new Client({
      dsn: "https://observe.example/p/p@tok",
      errors: false,
      replay: false,
      vitals: false,
      fetchTracing: true,
    }).start();

    // Caller already set traceparent on XHR: SDK must NOT call setRequestHeader("traceparent") a second time.
    const xhr = new FakeXHR();
    xhr.open("GET", "/api/items");
    xhr.setRequestHeader("traceparent", "00-11111111111111111111111111111111-2222222222222222-01");
    xhr.send();
    expect(xhr.headers.filter(([k]) => k.toLowerCase() === "traceparent")).toHaveLength(1);

    // Third-party library wraps fetch and XHR.open after SDK init.
    const sdkFetch = window.fetch;
    const thirdPartyFetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => sdkFetch(input, init));
    window.fetch = thirdPartyFetch as unknown as typeof fetch;

    const sdkOpen = FakeXHR.prototype.open;
    const thirdPartyOpen = function(this: FakeXHR, method: string, url: string | URL) {
      return sdkOpen.call(this, method, url);
    };
    FakeXHR.prototype.open = thirdPartyOpen;

    await client.close();
    // close() must NOT overwrite third-party wrappers!
    expect(window.fetch).toBe(thirdPartyFetch);
    expect(FakeXHR.prototype.open).toBe(thirdPartyOpen);
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

    expect(send).toHaveBeenCalledWith([1], { keepalive: true, signal: expect.any(AbortSignal) });
  });

  it("dispatches keepalive batches synchronously from flush(true) while an async drain is in flight", async () => {
    let releaseFirst!: () => void;
    const firstGate = new Promise<void>((resolve) => { releaseFirst = resolve; });
    const sent: Array<{ batch: number[]; keepalive: boolean }> = [];
    const send = vi.fn(async (batch: number[], options: DeliverySendOptions) => {
      sent.push({ batch, keepalive: options.keepalive });
      if (batch[0] === 1) await firstGate;
    });
    const queue = new DeliveryQueue<number>({
      capacity: 10,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      send,
    });

    queue.enqueue(1);
    await vi.waitFor(() => expect(send).toHaveBeenCalledTimes(1));

    queue.enqueue(2);
    queue.enqueue(3);

    const keepaliveFlush = queue.flush(true);
    // Remaining batches must be dispatched synchronously with keepalive: true without waiting for item 1.
    expect(sent).toEqual([
      { batch: [1], keepalive: false },
      { batch: [2], keepalive: true },
      { batch: [3], keepalive: true },
    ]);
    await keepaliveFlush;
    expect(queue.diagnostics()).toMatchObject({ pending: 1, dropped: 0 });

    releaseFirst();
    await queue.flush();
    expect(send).toHaveBeenCalledTimes(3);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 0 });
  });

  it("dispatches keepalive batches synchronously from flush(true) while an async drain is stalled in retry backoff", async () => {
    let releaseRetry!: () => void;
    const retryGate = new Promise<void>((resolve) => { releaseRetry = resolve; });
    let firstAttempt = true;
    const sent: Array<{ batch: string[]; keepalive: boolean }> = [];
    const send = vi.fn(async (batch: string[], options: DeliverySendOptions) => {
      sent.push({ batch, keepalive: options.keepalive });
      if (batch[0].startsWith("a") && firstAttempt) {
        firstAttempt = false;
        throw new TransportError(503, "unavailable");
      }
    });
    const wait = vi.fn(async () => retryGate);
    const queue = new DeliveryQueue<string>({
      capacity: 10,
      batchSize: 10,
      flushIntervalMs: 60_000,
      maxAttempts: 2,
      retryInitialMs: 100,
      retryMaxMs: 200,
      maxBatchBytes: 200 * 1024,
      keepaliveMaxBatchBytes: MAX_KEEPALIVE_BYTES,
      batchOverheadBytes: 16,
      estimateItemBytes: (item) => new TextEncoder().encode(item).byteLength,
      send,
      wait,
    });

    const itemA = "a".repeat(35 * 1024);
    const itemB = "b".repeat(35 * 1024);
    const itemC = "c".repeat(35 * 1024);

    // Trigger an async drain for itemA alone and stall inside retry wait.
    queue.enqueue(itemA);
    const normalFlush = queue.flush(false);
    await vi.waitFor(() => expect(wait).toHaveBeenCalledTimes(1));

    // Enqueue two 35KB items while itemA is waiting to retry.
    queue.enqueue(itemB);
    queue.enqueue(itemC);

    const keepaliveFlush = queue.flush(true);
    // Keepalive budget (60KB) must split [itemB] and [itemC] and dispatch both synchronously.
    expect(sent).toEqual([
      { batch: [itemA], keepalive: false },
      { batch: [itemB], keepalive: true },
      { batch: [itemC], keepalive: true },
    ]);
    await keepaliveFlush;
    expect(queue.diagnostics().pending).toBe(1);

    // Let itemA retry and succeed; ensure itemB/itemC are never sent a second time.
    releaseRetry();
    await normalFlush;
    expect(sent).toEqual([
      { batch: [itemA], keepalive: false },
      { batch: [itemB], keepalive: true },
      { batch: [itemC], keepalive: true },
      { batch: [itemA], keepalive: false },
    ]);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 0, lastError: undefined });
  });

  it("aborts both in-flight async drains and in-flight keepalive deliveries when close({ timeoutMs }) expires", async () => {
    const drops: Array<{ reason: string; count: number }> = [];
    const queue = new DeliveryQueue<number>({
      capacity: 4,
      batchSize: 1,
      flushIntervalMs: 60_000,
      maxAttempts: 2,
      retryInitialMs: 10,
      retryMaxMs: 20,
      send: async () => new Promise<void>(() => {}),
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });

    queue.enqueue(1);
    const normalFlush = queue.flush(false);
    queue.enqueue(2);
    const keepaliveFlush = queue.flush(true);

    await expect(queue.close({ timeoutMs: 25 })).resolves.toBeUndefined();
    await Promise.all([normalFlush, keepaliveFlush]);
    expect(drops).toEqual([
      { reason: "shutdown_budget", count: 1 },
      { reason: "shutdown_budget", count: 1 },
    ]);
    expect(queue.diagnostics()).toMatchObject({ pending: 0, dropped: 2 });
  });

  it("deduplicates unsent items by dedupeKey keeping the latest value without mutating in-flight batches", async () => {
    const sent: Array<Array<{ key: string; value: number }>> = [];
    const drops: Array<{ reason: string; count: number }> = [];
    const cappedQueue = new DeliveryQueue<{ key: string; value: number }>({
      capacity: 2,
      batchSize: 10,
      flushIntervalMs: 60_000,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      estimateItemBytes: (item) => JSON.stringify(item).length,
      dedupeKey: (item) => item.key,
      send: async (batch) => { sent.push(batch); },
      onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
    });

    expect(cappedQueue.enqueue({ key: "LCP:/a", value: 100 })).toBe(true);
    expect(cappedQueue.enqueue({ key: "LCP:/a", value: 250 })).toBe(true);
    expect(cappedQueue.enqueue({ key: "CLS:/a", value: 0.1 })).toBe(true);
    // Queue is at capacity (2 resident keys), so updating an existing key must still succeed in-place.
    expect(cappedQueue.enqueue({ key: "CLS:/a", value: 0.2 })).toBe(true);
    // A brand-new key overflows capacity and drops as queue_full.
    expect(cappedQueue.enqueue({ key: "INP:/a", value: 50 })).toBe(false);
    expect(drops).toEqual([{ reason: "queue_full", count: 1 }]);
    expect(cappedQueue.diagnostics()).toMatchObject({ pending: 2, dropped: 1 });
    await cappedQueue.flush();
    expect(sent).toEqual([[{ key: "LCP:/a", value: 250 }, { key: "CLS:/a", value: 0.2 }]]);

    // While a batch is in flight, a new reading for the same key must queue separately without mutating it.
    let releaseFirst!: () => void;
    const firstGate = new Promise<void>((resolve) => { releaseFirst = resolve; });
    const inFlightSent: Array<Array<{ key: string; value: number }>> = [];
    const inFlightQueue = new DeliveryQueue<{ key: string; value: number }>({
      capacity: 3,
      batchSize: 2,
      flushIntervalMs: 60_000,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      dedupeKey: (item) => item.key,
      send: async (batch) => {
        inFlightSent.push(batch);
        if (inFlightSent.length === 1) await firstGate;
      },
    });
    expect(inFlightQueue.enqueue({ key: "LCP:/a", value: 250 })).toBe(true);
    expect(inFlightQueue.enqueue({ key: "CLS:/a", value: 0.2 })).toBe(true);
    const firstFlush = inFlightQueue.flush();
    await vi.waitFor(() => expect(inFlightSent).toHaveLength(1));

    expect(inFlightQueue.enqueue({ key: "LCP:/a", value: 300 })).toBe(true);
    releaseFirst();
    await firstFlush;

    expect(inFlightSent).toEqual([
      [
        { key: "LCP:/a", value: 250 },
        { key: "CLS:/a", value: 0.2 },
      ],
      [{ key: "LCP:/a", value: 300 }],
    ]);
    expect(inFlightQueue.diagnostics()).toMatchObject({ pending: 0, dropped: 0 });
  });

  it("splits batches so each batch contains at most maxBatchDistinctKeys distinct keys", async () => {
    const sent: Array<Array<{ page: string; name: string }>> = [];
    const queue = new DeliveryQueue<{ page: string; name: string }>({
      capacity: 100,
      batchSize: 100,
      flushIntervalMs: 60_000,
      maxAttempts: 1,
      retryInitialMs: 1,
      retryMaxMs: 1,
      batchDistinctKey: (item) => item.page,
      maxBatchDistinctKeys: 32,
      send: async (batch) => { sent.push(batch); },
    });

    for (let i = 0; i < 32; i++) {
      expect(queue.enqueue({ page: `/p${i}`, name: "LCP" })).toBe(true);
    }
    // Same page as an already-included page fits inside the first batch (still 32 distinct pages).
    expect(queue.enqueue({ page: "/p0", name: "CLS" })).toBe(true);
    // 33rd distinct page must split into a second batch.
    expect(queue.enqueue({ page: "/p32", name: "LCP" })).toBe(true);

    await queue.flush();
    expect(sent).toHaveLength(2);
    expect(sent[0]).toHaveLength(33);
    expect(new Set(sent[0].map((item) => item.page)).size).toBe(32);
    expect(sent[1]).toEqual([{ page: "/p32", name: "LCP" }]);
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
