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

it("retains latest two checkout generations so snapshot() always starts with Meta + FullSnapshot after 60s+", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; type: number; label: string }>(60_000, 5_000_000);
  ring.push({ timestamp: 0, type: 4, label: "meta-0" });
  ring.push({ timestamp: 0, type: 2, label: "full-0" });
  ring.push({ timestamp: 10_000, type: 3, label: "inc-10" });
  ring.push({ timestamp: 30_000, type: 4, label: "meta-30" }, true);
  ring.push({ timestamp: 30_000, type: 2, label: "full-30" }, true);
  ring.push({ timestamp: 45_000, type: 3, label: "inc-45" });
  ring.push({ timestamp: 60_000, type: 4, label: "meta-60" }, true);
  ring.push({ timestamp: 60_000, type: 2, label: "full-60" }, true);
  ring.push({ timestamp: 75_000, type: 5, label: "error-75" });

  const labels = ring.snapshot().map((event) => event.label);
  expect(labels[0]).toBe("meta-30");
  expect(labels[1]).toBe("full-30");
  expect(labels).not.toContain("meta-0");
  expect(labels).not.toContain("full-0");
  expect(labels).toContain("meta-60");
  expect(labels).toContain("full-60");
  expect(labels[labels.length - 1]).toBe("error-75");
});

it("preserves the initial Meta + FullSnapshot during >60s idle when no checkout occurred yet", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; type: number; label: string }>(60_000, 5_000_000);
  ring.push({ timestamp: 0, type: 4, label: "meta-0" });
  ring.push({ timestamp: 0, type: 2, label: "full-0" });
  ring.push({ timestamp: 75_000, type: 5, label: "error-75" });

  expect(ring.snapshot().map((event) => event.label)).toEqual(["meta-0", "full-0", "error-75"]);
});
