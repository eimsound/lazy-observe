import http from "node:http";
import type { AddressInfo } from "node:net";
import { afterEach, describe, expect, it, vi } from "vitest";
import { loadOptionalPeer } from "../src/optional-peer.js";
import { startProfiling } from "../src/profiling.js";
import { init, startUnhandledErrorCapture } from "../src/sdk.js";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function startSinkServer(): Promise<{ origin: string; close(): Promise<void> }> {
  const server = http.createServer((req, res) => {
    req.resume();
    res.writeHead(200);
    res.end();
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    origin: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

describe("Node SDK lifecycle regression matrix", () => {
  it("init -> shutdown -> init leaves zero residual listeners or timers and delivers cleanly on re-init", async () => {
    const sink = await startSinkServer();
    try {
      const beforeListeners = process.listenerCount("uncaughtExceptionMonitor");
      const activeIntervals = new Set<ReturnType<typeof setInterval>>();
      const realSetInterval = globalThis.setInterval;
      const realClearInterval = globalThis.clearInterval;
      vi.spyOn(globalThis, "setInterval").mockImplementation(((...args: Parameters<typeof setInterval>) => {
        const handle = realSetInterval(...args);
        activeIntervals.add(handle);
        return handle;
      }) as typeof setInterval);
      vi.spyOn(globalThis, "clearInterval").mockImplementation(((handle: ReturnType<typeof setInterval> | undefined) => {
        if (handle !== undefined) activeIntervals.delete(handle);
        realClearInterval(handle);
      }) as typeof clearInterval);

      const fetcher = vi.fn(async (_input: string | URL | Request, _init?: RequestInit) => (
        new Response(null, { status: 204 })
      ));
      vi.stubGlobal("fetch", fetcher);

      const dsn = `${sink.origin}/p/lifecycle@lo_tok`;
      const sdk1 = init({
        dsn,
        metrics: true,
        logs: false,
        traces: false,
        profiling: false,
        captureUnhandledErrors: true,
        runtimeMetricsIntervalMs: 5_000,
      });
      expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(beforeListeners + 1);
      expect(activeIntervals.size).toBeGreaterThanOrEqual(1);

      await sdk1.reportError(new Error("from-sdk-1"), { url: "https://user:pw@api.example/a?token=secret#h" });
      await sdk1.shutdown({ timeoutMs: 2_000 });
      // Idempotent second shutdown
      await expect(sdk1.shutdown({ timeoutMs: 2_000 })).resolves.toBeUndefined();

      expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(beforeListeners);
      expect(activeIntervals.size).toBe(0);

      // Ensure reportError URL was sanitized (no credentials, query, or hash)
      const errorCall1 = fetcher.mock.calls.find(([input]) => String(input).endsWith("/api/events/errors"));
      expect(errorCall1).toBeDefined();
      const payload1 = JSON.parse(String(errorCall1?.[1]?.body)) as { events: Array<{ url?: string; message: string }> };
      expect(payload1.events[0]?.url).toBe("https://api.example/a");
      expect(payload1.events[0]?.url).not.toContain("secret");

      // Re-init after shutdown
      const sdk2 = init({
        dsn,
        metrics: true,
        logs: false,
        traces: false,
        profiling: false,
        captureUnhandledErrors: true,
        runtimeMetricsIntervalMs: 5_000,
      });
      expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(beforeListeners + 1);
      await sdk2.reportError(new Error("from-sdk-2"));
      await sdk2.shutdown({ timeoutMs: 2_000 });

      expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(beforeListeners);
      expect(activeIntervals.size).toBe(0);
    } finally {
      await sink.close();
    }
  });

  it("startUnhandledErrorCapture returns an idempotent dispose function", () => {
    const before = process.listenerCount("uncaughtExceptionMonitor");
    const dispose = startUnhandledErrorCapture(() => undefined);
    expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(before + 1);
    dispose();
    expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(before);
    // Second call is a no-op
    dispose();
    expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(before);
  });

  it("aborts Node profiling cleanly when shutdown() interrupts an active sampling window", async () => {
    const fetcher = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);

    const started = Date.now();
    const sdk = init({
      dsn: "https://observe.example/p/prof-interrupt@lo_tok",
      metrics: false,
      logs: false,
      traces: false,
      profiling: true,
      profilingInterval: 60_000,
    });

    // Wait briefly so the 10s CPU sampling window has started in the inspector session.
    await new Promise((resolve) => setTimeout(resolve, 40));
    await sdk.shutdown({ timeoutMs: 2_000 });

    expect(Date.now() - started).toBeLessThan(2_000);
    expect(fetcher).not.toHaveBeenCalled();

    // A subsequent profiling session can start and stop cleanly after interruption.
    const stop2 = startProfiling(
      {
        endpoint: "https://observe.example",
        token: "lo_tok",
        service: "prof-interrupt-2",
        intervalMs: 60_000,
        warn: () => undefined,
        queueCapacity: 2,
        maxAttempts: 1,
        retryInitialMs: 1,
        retryMaxMs: 1,
        onDrop: () => undefined,
      },
      { cpuWindowMs: 5_000 },
    );
    await new Promise((resolve) => setTimeout(resolve, 30));
    await stop2({ timeoutMs: 1_000 });
    expect(fetcher).not.toHaveBeenCalled();
  });

  it("surfaces a clear actionable error when an optional peer dependency is missing", () => {
    const missingLoader = (specifier: string) => {
      const error = new Error(`Cannot find module '${specifier}'`) as NodeJS.ErrnoException;
      error.code = "MODULE_NOT_FOUND";
      throw error;
    };
    expect(() => loadOptionalPeer("pino", "lo.pino()", missingLoader)).toThrow(
      /lo\.pino\(\) requires optional peer dependency "pino"/,
    );
    expect(() => loadOptionalPeer("winston-transport", "lo.winston()", missingLoader)).toThrow(
      /lo\.winston\(\) requires optional peer dependency "winston-transport"/,
    );
  });
});
