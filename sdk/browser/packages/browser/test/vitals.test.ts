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

import type { Transport } from "../src/transport";
import { startVitals } from "../src/vitals";

beforeEach(() => webVitalCallbacks.splice(0));

it("mirrors name, value and rating into Replay before transport and includes normalized dimensions", () => {
  const vitals = vi.fn().mockResolvedValue(undefined);
  const replay = vi.fn();
  vi.spyOn(Date, "now").mockReturnValue(1_721_234_567_890);

  startVitals(
    { vitals } as unknown as Transport,
    () => "/checkout",
    replay,
    undefined,
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
  expect(vitals).toHaveBeenCalledWith([{
    name: "LCP",
    value: 2500,
    page: "/checkout",
    timestamp: "2024-07-17T16:42:47.890Z",
    device_type: "desktop",
    browser_family: "chrome",
    os_family: "macos",
    country: "US",
  }]);
});

it("omits country when not explicitly provided and survives a throwing dimension reader", () => {
  const vitals = vi.fn().mockResolvedValue(undefined);
  vi.spyOn(Date, "now").mockReturnValue(1_721_234_567_890);

  startVitals(
    { vitals } as unknown as Transport,
    () => "/home",
    undefined,
    undefined,
    () => {
      throw new Error("boom");
    },
  );
  webVitalCallbacks[0]({
    name: "CLS",
    value: 0.05,
    rating: "good",
  } as Metric);

  expect(vitals).toHaveBeenCalledWith([{
    name: "CLS",
    value: 0.05,
    page: "/home",
    timestamp: "2024-07-17T16:42:47.890Z",
  }]);
});
