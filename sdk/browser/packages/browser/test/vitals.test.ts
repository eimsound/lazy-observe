import { beforeEach, expect, it, vi } from "vitest";
import type { Metric } from "web-vitals";

const webVitalCallbacks = vi.hoisted(() => [] as Array<(metric: Metric) => void>);

vi.mock("web-vitals", () => ({
  onCLS: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onFCP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onINP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onLCP: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
  onTTFB: (callback: (metric: Metric) => void) => webVitalCallbacks.push(callback),
}));

import { startVitals } from "../src/vitals";

beforeEach(() => webVitalCallbacks.splice(0));

it("mirrors name, value and rating into Replay before queuing and includes normalized dimensions", () => {
  const enqueue = vi.fn();
  const replay = vi.fn();
  vi.spyOn(Date, "now").mockReturnValue(1_721_234_567_890);

  startVitals(
    enqueue,
    () => "/checkout",
    replay,
    () => ({
      device_type: "desktop",
      browser_family: "chrome",
      os_family: "macos",
      country: "us",
    }),
  );
  expect(webVitalCallbacks).toHaveLength(5);
  webVitalCallbacks[0]({
    name: "LCP",
    value: 2500,
    rating: "needs-improvement",
  } as Metric);

  expect(replay).toHaveBeenCalledWith({
    timestamp: 1_721_234_567_890,
    name: "LCP",
    value: 2500,
    rating: "needs-improvement",
  });
  expect(enqueue).toHaveBeenCalledWith({
    name: "LCP",
    value: 2500,
    page: "/checkout",
    timestamp: "2024-07-17T16:42:47.890Z",
    device_type: "desktop",
    browser_family: "chrome",
    os_family: "macos",
    country: "US",
  });
});

it("omits country when not explicitly provided and survives throwing readers or callbacks", () => {
  const enqueue = vi.fn(() => { throw new Error("queue boom"); });
  vi.spyOn(Date, "now").mockReturnValue(1_721_234_567_890);

  startVitals(
    enqueue,
    () => "/home",
    () => { throw new Error("replay boom"); },
    () => {
      throw new Error("dims boom");
    },
  );
  expect(() => webVitalCallbacks[0]({
    name: "CLS",
    value: 0.05,
    rating: "good",
  } as Metric)).not.toThrow();

  expect(enqueue).toHaveBeenCalledWith({
    name: "CLS",
    value: 0.05,
    page: "/home",
    timestamp: "2024-07-17T16:42:47.890Z",
  });
});
