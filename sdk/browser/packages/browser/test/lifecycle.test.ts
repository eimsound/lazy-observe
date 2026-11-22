import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Metric } from "web-vitals";

const webVitalCallbacks = vi.hoisted(() => [] as Array<(metric: Metric) => void>);

vi.mock("web-vitals", () => ({
  onCLS: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onFCP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onINP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onLCP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onTTFB: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
}));

import { init, sanitizeErrorEvent } from "../src";
import { ReplayController, replayDefaults, ReplaySegmentDelivery } from "../src/replay";
import { MAX_MESSAGE_LENGTH, MAX_STACK_FRAMES } from "../src/sanitize";
import type { Transport } from "../src/transport";

beforeEach(() => {
  webVitalCallbacks.splice(0);
  sessionStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("Browser SDK lifecycle regression matrix", () => {
  it("init -> close -> init leaves zero residual listeners, timers, or fetch/XHR patches", async () => {
    const baseFetch = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", baseFetch);

    class FakeXHR extends EventTarget {
      readonly headers: Array<[string, string]> = [];
      status = 200;
      open(_method: string, _url: string | URL): void {}
      send(_body?: Document | XMLHttpRequestBodyInit | null): void {}
      setRequestHeader(name: string, value: string): void { this.headers.push([name, value]); }
    }
    const origOpen = FakeXHR.prototype.open;
    const origSend = FakeXHR.prototype.send;
    const origSetHeader = FakeXHR.prototype.setRequestHeader;
    vi.stubGlobal("XMLHttpRequest", FakeXHR);

    const activeWindowListeners = new Map<string, Set<EventListenerOrEventListenerObject>>();
    const activeDocListeners = new Map<string, Set<EventListenerOrEventListenerObject>>();
    const realWinAdd = window.addEventListener.bind(window);
    const realWinRemove = window.removeEventListener.bind(window);
    const realDocAdd = document.addEventListener.bind(document);
    const realDocRemove = document.removeEventListener.bind(document);

    vi.spyOn(window, "addEventListener").mockImplementation((type, listener, options) => {
      if (listener) {
        if (!activeWindowListeners.has(type)) activeWindowListeners.set(type, new Set());
        activeWindowListeners.get(type)!.add(listener);
      }
      return realWinAdd(type, listener, options);
    });
    vi.spyOn(window, "removeEventListener").mockImplementation((type, listener, options) => {
      if (listener) activeWindowListeners.get(type)?.delete(listener);
      return realWinRemove(type, listener, options);
    });
    vi.spyOn(document, "addEventListener").mockImplementation((type, listener, options) => {
      if (listener) {
        if (!activeDocListeners.has(type)) activeDocListeners.set(type, new Set());
        activeDocListeners.get(type)!.add(listener);
      }
      return realDocAdd(type, listener, options);
    });
    vi.spyOn(document, "removeEventListener").mockImplementation((type, listener, options) => {
      if (listener) activeDocListeners.get(type)?.delete(listener);
      return realDocRemove(type, listener, options);
    });

    const activeTimeouts = new Set<ReturnType<typeof setTimeout>>();
    const activeIntervals = new Set<ReturnType<typeof setInterval>>();
    const realSetTimeout = globalThis.setTimeout;
    const realClearTimeout = globalThis.clearTimeout;
    const realSetInterval = globalThis.setInterval;
    const realClearInterval = globalThis.clearInterval;

    vi.spyOn(globalThis, "setTimeout").mockImplementation(((fn: (...args: unknown[]) => void, ms?: number, ...rest: unknown[]) => {
      const id = realSetTimeout((...args: unknown[]) => {
        activeTimeouts.delete(id);
        fn(...args);
      }, ms, ...rest);
      activeTimeouts.add(id);
      return id;
    }) as typeof setTimeout);
    vi.spyOn(globalThis, "clearTimeout").mockImplementation(((id: ReturnType<typeof setTimeout> | undefined) => {
      if (id !== undefined) activeTimeouts.delete(id);
      realClearTimeout(id);
    }) as typeof clearTimeout);
    vi.spyOn(globalThis, "setInterval").mockImplementation(((...args: Parameters<typeof setInterval>) => {
      const id = realSetInterval(...args);
      activeIntervals.add(id);
      return id;
    }) as typeof setInterval);
    vi.spyOn(globalThis, "clearInterval").mockImplementation(((id: ReturnType<typeof setInterval> | undefined) => {
      if (id !== undefined) activeIntervals.delete(id);
      realClearInterval(id);
    }) as typeof clearInterval);

    const countListeners = (map: Map<string, Set<EventListenerOrEventListenerObject>>) =>
      Array.from(map.values()).reduce((sum, set) => sum + set.size, 0);

    // Cycle 1: init -> capture -> close
    const captured1: string[] = [];
    const client1 = init({
      dsn: "https://observe.example/p/life@lo_tok",
      replay: false,
      vitals: true,
      fetchTracing: true,
      errorFlushIntervalMs: 60_000,
      beforeSend(event) {
        captured1.push(event.message ?? "");
        return event;
      },
    });
    expect(countListeners(activeWindowListeners)).toBe(3); // error, unhandledrejection, pagehide
    expect(countListeners(activeDocListeners)).toBe(1);    // visibilitychange
    expect(window.fetch).not.toBe(baseFetch);
    expect(FakeXHR.prototype.open).not.toBe(origOpen);

    client1.captureException(new Error("cycle-1"));
    expect(activeTimeouts.size).toBeGreaterThanOrEqual(1);

    await client1.close();
    // Idempotent second close
    await client1.close();

    expect(countListeners(activeWindowListeners)).toBe(0);
    expect(countListeners(activeDocListeners)).toBe(0);
    expect(activeTimeouts.size).toBe(0);
    expect(activeIntervals.size).toBe(0);
    expect(window.fetch).toBe(baseFetch);
    expect(FakeXHR.prototype.open).toBe(origOpen);
    expect(FakeXHR.prototype.send).toBe(origSend);
    expect(FakeXHR.prototype.setRequestHeader).toBe(origSetHeader);

    // Cycle 2: re-init and verify no duplicate captures from closed client1,
    // then implicit re-init (client3 replaces client2 without explicit close)
    const captured2: string[] = [];
    const captured3: string[] = [];
    const client2 = init({
      dsn: "https://observe.example/p/life@lo_tok",
      replay: false,
      vitals: true,
      fetchTracing: true,
      beforeSend(event) {
        captured2.push(event.message ?? "");
        return event;
      },
    });
    const client3 = init({
      dsn: "https://observe.example/p/life@lo_tok",
      replay: false,
      vitals: true,
      fetchTracing: true,
      beforeSend(event) {
        captured3.push(event.message ?? "");
        return event;
      },
    });
    window.dispatchEvent(new ErrorEvent("error", { error: new Error("after-reinit"), message: "after-reinit" }));
    expect(captured1).toEqual(["cycle-1"]);
    expect(captured2).toEqual([]);
    expect(captured3).toEqual(["after-reinit"]);
    expect(countListeners(activeWindowListeners)).toBe(3);
    expect(countListeners(activeDocListeners)).toBe(1);

    await client2.close();
    await client3.close();
    expect(countListeners(activeWindowListeners)).toBe(0);
    expect(countListeners(activeDocListeners)).toBe(0);
    expect(activeTimeouts.size).toBe(0);
    expect(activeIntervals.size).toBe(0);
    expect(window.fetch).toBe(baseFetch);
    expect(FakeXHR.prototype.open).toBe(origOpen);
  });

  it("aborts async rrweb loading cleanly when close() runs mid-import and degrades silently if rrweb is unavailable", async () => {
    let resolveImport!: (mod: typeof import("rrweb")) => void;
    const loadRrweb = () => new Promise<typeof import("rrweb")>((resolve) => { resolveImport = resolve; });
    const record = vi.fn(() => vi.fn());
    const setIntervalSpy = vi.spyOn(globalThis, "setInterval");
    const transport = { replay: vi.fn(async () => undefined) } as unknown as Transport;

    const controller = new ReplayController(
      replayDefaults({ sampleRate: 1, flushIntervalMs: 1_000 }),
      "mid-import-close",
      transport,
      { loadRrweb },
    );
    const startPromise = controller.start();
    await controller.close();

    resolveImport({ record, addCustomEvent: vi.fn() } as unknown as typeof import("rrweb"));
    await startPromise;

    expect(record).not.toHaveBeenCalled();
    expect(setIntervalSpy).not.toHaveBeenCalled();

    // Optional peer dependency rrweb missing: warns once and resolves without throwing
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const missingController = new ReplayController(
      replayDefaults({ sampleRate: 1 }),
      "missing-rrweb",
      transport,
      { loadRrweb: async () => { throw new Error("Cannot find package 'rrweb'"); } },
    );
    await expect(missingController.start()).resolves.toBeUndefined();
    expect(warnSpy).toHaveBeenCalledWith(
      expect.stringContaining("optional peer dependency \"rrweb\" is unavailable"),
      expect.any(Error),
    );
    await missingController.close();
  });

  it("ignores late web-vitals callbacks fired after close() while keeping subsequent init() working", async () => {
    const fetcher = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);

    const client1 = init({
      dsn: "https://observe.example/p/vitals-life@lo_tok",
      replay: false,
      fetchTracing: false,
      vitals: true,
    });
    expect(webVitalCallbacks).toHaveLength(5);
    const client1Callbacks = [...webVitalCallbacks];

    client1Callbacks[0]({ name: "LCP", value: 1200, rating: "good" } as Metric);
    await client1.flush();
    expect(fetcher).toHaveBeenCalledTimes(1);

    await client1.close();

    // Fire all 5 callbacks from the closed client: none may post or warn
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    for (const cb of client1Callbacks) {
      cb({ name: "CLS", value: 0.05, rating: "good" } as Metric);
    }
    await Promise.resolve();
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(warnSpy).not.toHaveBeenCalled();

    // New init registers fresh callbacks that DO report
    const client2 = init({
      dsn: "https://observe.example/p/vitals-life@lo_tok",
      replay: false,
      fetchTracing: false,
      vitals: true,
    });
    expect(webVitalCallbacks).toHaveLength(10);
    webVitalCallbacks[5]({ name: "FCP", value: 800, rating: "good" } as Metric);
    await client2.flush();
    expect(fetcher).toHaveBeenCalledTimes(2);
    await client2.close();
  });

  it("persists Replay segment sequence monotonically across page reloads", async () => {
    vi.stubGlobal("CompressionStream", undefined);
    const uploadedSeqs: number[] = [];
    const transport = {
      replay: vi.fn(async (_sessionId: string, seq: number) => { uploadedSeqs.push(seq); }),
    };

    const session1 = new ReplaySegmentDelivery("reload-session", transport, { storage: sessionStorage, maxAttempts: 1 });
    session1.enqueue([{ timestamp: 1, type: 4 }, { timestamp: 1, type: 2 }], false);
    session1.enqueue([{ timestamp: 2, type: 3 }], false);
    await session1.close();

    // Simulated page reload with same sessionStorage and sessionId
    const session2 = new ReplaySegmentDelivery("reload-session", transport, { storage: sessionStorage, maxAttempts: 1 });
    session2.enqueue([{ timestamp: 3, type: 4 }, { timestamp: 3, type: 2 }], true);
    await session2.close();

    expect(uploadedSeqs).toEqual([0, 1, 2]);
    expect(sessionStorage.getItem("lazy-observe:replay-seq:reload-session")).toBe("3");
  });

  it("sanitizes error event URLs, stack frame URLs, tags, messages, and frame counts", () => {
    const sanitized = sanitizeErrorEvent({
      event_id: "11111111-1111-4111-a111-111111111111",
      timestamp: new Date().toISOString(),
      platform: "javascript",
      level: "error",
      url: "https://user:pw@app.example/checkout?token=secret#step2",
      message: "m".repeat(MAX_MESSAGE_LENGTH + 500),
      exception: {
        type: "Error",
        value: "v".repeat(MAX_MESSAGE_LENGTH + 500),
        stacktrace: {
          frames: Array.from({ length: 70 }, (_, idx) => ({
            function: `frame_${idx}`,
            filename: `https://user:pw@app.example/chunk-${idx}.js?sig=secret#h`,
            lineno: idx + 1,
            colno: 1,
            in_app: true,
          })),
        },
      },
      tags: { huge: "t".repeat(10_000) },
    });

    expect(sanitized).toBeDefined();
    expect(sanitized?.url).toBe("https://app.example/checkout");
    expect(sanitized?.message?.length).toBeLessThanOrEqual(MAX_MESSAGE_LENGTH + "...[truncated]".length);
    expect(sanitized?.exception.stacktrace.frames).toHaveLength(MAX_STACK_FRAMES);
    expect(sanitized?.exception.stacktrace.frames[0]?.function).toBe("frame_20");
    expect(sanitized?.exception.stacktrace.frames[0]?.filename).toBe("https://app.example/chunk-20.js");
    expect(JSON.stringify(sanitized)).not.toContain("secret");
  });
});
