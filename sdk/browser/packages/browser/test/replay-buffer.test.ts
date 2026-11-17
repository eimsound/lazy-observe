import { expect, it } from "vitest";
import { ReplayRingBuffer } from "../src/replay-buffer";

it("keeps only events inside the time window", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; value: string }>(1_000);
  ring.push({ timestamp: 1_000, value: "old" });
  ring.push({ timestamp: 1_500, value: "middle" });
  ring.push({ timestamp: 2_001, value: "new" });
  expect(ring.snapshot().map((event) => event.value)).toEqual(["middle", "new"]);
  expect(ring.drain()).toHaveLength(2);
  expect(ring.length).toBe(0);
});

it("evicts oldest events when byte budget is exceeded", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; value: string }>(60_000, 80);
  ring.push({ timestamp: 1, value: "a".repeat(30) });
  ring.push({ timestamp: 2, value: "b".repeat(30) });
  expect(ring.snapshot()).toHaveLength(1);
  expect(ring.snapshot()[0].value).toContain("b");
});
