import { expect, it, vi } from "vitest";
import { ReplaySegmentDelivery } from "../src/replay";
import type { TimedReplayEvent } from "../src/replay-buffer";

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
