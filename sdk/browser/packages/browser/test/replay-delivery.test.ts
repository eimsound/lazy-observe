import { expect, it, vi } from "vitest";
import { ReplayController, replayDefaults, ReplaySegmentDelivery } from "../src/replay";
import type { TimedReplayEvent } from "../src/replay-buffer";
import type { Transport } from "../src/transport";

it("retains a drained Replay segment through the entire retry budget", async () => {
  vi.stubGlobal("CompressionStream", undefined);
  let releaseRetry!: () => void;
  const firstRetry = new Promise<void>((resolve) => { releaseRetry = resolve; });
  const wait = vi.fn()
    .mockImplementationOnce(async () => firstRetry)
    .mockImplementation(async () => undefined);
  const transport = {
    replay: vi.fn(async (
      _sessionId: string,
      _seq: number,
      _body: BodyInit,
      _compressed: boolean,
      _error: boolean,
      _keepalive?: boolean,
    ) => { throw new Error("offline"); }),
  };
  const drops: Array<{ reason: string; count: number }> = [];
  const delivery = new ReplaySegmentDelivery("session-1", transport, {
    capacity: 1,
    maxAttempts: 3,
    retryInitialMs: 10,
    retryMaxMs: 20,
    wait,
    onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
  });
  const events: TimedReplayEvent[] = [{ timestamp: 1, type: 2 }];

  expect(delivery.enqueue(events, true)).toBe(true);
  await vi.waitFor(() => expect(transport.replay).toHaveBeenCalledTimes(1));
  expect(delivery.diagnostics()).toMatchObject({ pending: 1, dropped: 0 });
  expect(delivery.enqueue([{ timestamp: 2, type: 3 }], false)).toBe(false);

  releaseRetry();
  await delivery.flush();

  expect(transport.replay).toHaveBeenCalledTimes(3);
  expect(transport.replay.mock.calls.map((call) => call[1])).toEqual([0, 0, 0]);
  expect(drops).toEqual([
    { reason: "queue_full", count: 1 },
    { reason: "retry_exhausted", count: 1 },
  ]);
  expect(delivery.diagnostics()).toMatchObject({ pending: 0, dropped: 2 });
});

it("persists and increments nextSequence in sessionStorage across page reloads", async () => {
  vi.stubGlobal("CompressionStream", undefined);
  const store = new Map<string, string>();
  const storage = {
    get length() { return store.size; },
    clear: () => store.clear(),
    getItem: (key: string) => store.get(key) ?? null,
    key: (index: number) => Array.from(store.keys())[index] ?? null,
    removeItem: (key: string) => { store.delete(key); },
    setItem: (key: string, value: string) => { store.set(key, value); },
  } as Storage;

  const seqs: number[] = [];
  const transport = {
    replay: vi.fn(async (_sessionId: string, seq: number) => { seqs.push(seq); }),
  };

  const page1 = new ReplaySegmentDelivery("shared-tab-session", transport, { storage, maxAttempts: 1 });
  expect(page1.enqueue([{ timestamp: 1, type: 4 }, { timestamp: 1, type: 2 }], true)).toBe(true);
  await page1.flush();

  // Simulate full page reload reusing the same sessionStorage and sessionId.
  const page2 = new ReplaySegmentDelivery("shared-tab-session", transport, { storage, maxAttempts: 1 });
  expect(page2.enqueue([{ timestamp: 2, type: 4 }, { timestamp: 2, type: 2 }], false)).toBe(true);
  await page2.flush();

  expect(seqs).toEqual([0, 1]);
  expect(storage.getItem("lazy-observe:replay-seq:shared-tab-session")).toBe("2");
});

it("configures checkoutEveryNms and aborts start() cleanly if close() runs during async import('rrweb')", async () => {
  let resolveImport!: (mod: typeof import("rrweb")) => void;
  const loadRrweb = () => new Promise<typeof import("rrweb")>((resolve) => { resolveImport = resolve; });
  const record = vi.fn(() => vi.fn());
  const transport = { replay: vi.fn(async () => undefined) } as unknown as Transport;

  const defaults = replayDefaults({ onErrorOnly: true });
  expect(defaults.checkoutEveryNms).toBe(30_000);

  const controller = new ReplayController(defaults, "race-session", transport, { loadRrweb });
  const startPromise = controller.start();
  await controller.close();

  resolveImport({ record, addCustomEvent: vi.fn() } as unknown as typeof import("rrweb"));
  await startPromise;

  expect(record).not.toHaveBeenCalled();
});

it("passes checkoutEveryNms to rrweb.record and synchronously dispatches keepalive replay without awaiting CompressionStream", async () => {
  const replay = vi.fn(async (
    _sessionId: string,
    _seq: number,
    _body: BodyInit,
    _compressed: boolean,
    _error: boolean,
    _keepalive?: boolean,
  ) => undefined);
  const transport = { replay };
  // Provide a fake CompressionStream that would stall if awaited during keepalive unload.
  vi.stubGlobal("CompressionStream", class {
    constructor() { throw new Error("CompressionStream should not be used during keepalive unload"); }
  });

  const delivery = new ReplaySegmentDelivery("unload-session", transport, { maxAttempts: 1 });
  delivery.enqueue([{ timestamp: 10, type: 4 }, { timestamp: 10, type: 2 }], false);
  const flushPromise = delivery.flush(true);
  // transport.replay must be invoked synchronously before the first microtask tick.
  expect(replay).toHaveBeenCalledTimes(1);
  expect(replay.mock.calls[0]?.[3]).toBe(false); // compressed === false
  expect(replay.mock.calls[0]?.[5]).toBe(true);  // keepalive === true
  await flushPromise;
  vi.unstubAllGlobals();
});
