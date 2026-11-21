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

import { Client } from "../src/client";
import {
  collectVitalDimensions,
  detectBrowserFamily,
  detectDeviceType,
  detectOSFamily,
  normalizeCountryCode,
} from "../src/vitals-dimensions";

const dsn = "https://observe.example/p/browser-vitals@secret";

beforeEach(() => webVitalCallbacks.splice(0));
afterEach(() => vi.unstubAllGlobals());

describe("vitals low-cardinality dimension normalization", () => {
  it("classifies device types from userAgentData, UA strings, touch heuristics and viewport", () => {
    expect(detectDeviceType({
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36",
      userAgentData: { mobile: false },
    })).toBe("desktop");

    expect(detectDeviceType({
      userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1",
    })).toBe("mobile");

    expect(detectDeviceType({
      userAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/126.0.0.0 Mobile Safari/537.36",
      userAgentData: { mobile: true },
    })).toBe("mobile");

    // Android tablet omits "Mobile" token.
    expect(detectDeviceType({
      userAgent: "Mozilla/5.0 (Linux; Android 13; SM-X900) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36",
      userAgentData: { mobile: false },
    })).toBe("tablet");

    // iPadOS desktop-mode Safari reports Macintosh UA with multi-touch.
    expect(detectDeviceType({
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/17.5 Safari/605.1.15",
      maxTouchPoints: 5,
    })).toBe("tablet");

    // Viewport fallback when UA is absent or synthetic.
    expect(detectDeviceType({ viewportWidth: 390 })).toBe("mobile");
    expect(detectDeviceType({ viewportWidth: 820, maxTouchPoints: 5 })).toBe("tablet");
    expect(detectDeviceType({ viewportWidth: 1440 })).toBe("desktop");
    expect(detectDeviceType({})).toBe("unknown");
  });

  it("classifies browser family strictly into finite enum without leaking raw UA", () => {
    expect(detectBrowserFamily({
      userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0",
    })).toBe("edge");
    expect(detectBrowserFamily({
      userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36 OPR/112.0.0.0",
    })).toBe("opera");
    expect(detectBrowserFamily({
      userAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
    })).toBe("firefox");
    expect(detectBrowserFamily({
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36",
    })).toBe("chrome");
    expect(detectBrowserFamily({
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/17.5 Safari/605.1.15",
    })).toBe("safari");
    expect(detectBrowserFamily({
      userAgentData: { brands: [{ brand: "Not/A)Brand" }, { brand: "Microsoft Edge" }] },
    })).toBe("edge");
    expect(detectBrowserFamily({ userAgent: "CustomBot/9.9" })).toBe("other");
    expect(detectBrowserFamily({})).toBe("other");
  });

  it("classifies OS family strictly into finite enum", () => {
    expect(detectOSFamily({ userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64)" })).toBe("windows");
    expect(detectOSFamily({ userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)" })).toBe("macos");
    expect(detectOSFamily({
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
      maxTouchPoints: 5,
    })).toBe("ios");
    expect(detectOSFamily({ userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X)" })).toBe("ios");
    expect(detectOSFamily({ userAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 8)" })).toBe("android");
    expect(detectOSFamily({ userAgent: "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0)" })).toBe("chromeos");
    expect(detectOSFamily({ userAgent: "Mozilla/5.0 (X11; Ubuntu; Linux x86_64)" })).toBe("linux");
    expect(detectOSFamily({ userAgentData: { platform: "Windows" } })).toBe("windows");
    expect(detectOSFamily({})).toBe("other");
  });

  it("enforces privacy boundary for country: accepts only explicit 2-letter ISO codes and never fabricates geo", () => {
    expect(normalizeCountryCode("cn")).toBe("CN");
    expect(normalizeCountryCode(" US ")).toBe("US");
    expect(normalizeCountryCode("XX")).toBeUndefined();
    expect(normalizeCountryCode("T1")).toBeUndefined();
    expect(normalizeCountryCode("ZZ")).toBeUndefined();
    expect(normalizeCountryCode("USA")).toBeUndefined();
    expect(normalizeCountryCode("12")).toBeUndefined();
    expect(normalizeCountryCode("US;evil=1")).toBeUndefined();
    expect(normalizeCountryCode("")).toBeUndefined();
    expect(normalizeCountryCode(undefined)).toBeUndefined();

    const collected = collectVitalDimensions(undefined, {
      userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36",
    });
    expect(collected).toEqual({
      device_type: "desktop",
      browser_family: "chrome",
      os_family: "macos",
    });
    expect("country" in collected).toBe(false);
  });

  it("attaches configured and dynamically updated explicit country on Client vitals uploads", async () => {
    const fetcher = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);

    const client = new Client({
      dsn,
      replay: false,
      fetchTracing: false,
      vitals: { country: "jp" },
      delivery: { maxAttempts: 1 },
    });
    client.start();

    webVitalCallbacks[0]({ name: "LCP", value: 1800, rating: "good" } as Metric);
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
    const firstBody = JSON.parse(String(fetcher.mock.calls[0]?.[1]?.body));
    expect(firstBody.vitals[0]).toMatchObject({
      name: "LCP",
      value: 1800,
      country: "JP",
    });
    expect(["desktop", "mobile", "tablet", "unknown"]).toContain(firstBody.vitals[0].device_type);
    expect(["chrome", "edge", "safari", "firefox", "opera", "other"]).toContain(firstBody.vitals[0].browser_family);
    expect(["windows", "macos", "ios", "android", "linux", "chromeos", "other"]).toContain(firstBody.vitals[0].os_family);

    // Invalid country update clears the explicit country so server edge headers can decide.
    client.setCountry("invalid-country");
    webVitalCallbacks[1]({ name: "CLS", value: 0.04, rating: "good" } as Metric);
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
    const secondBody = JSON.parse(String(fetcher.mock.calls[1]?.[1]?.body));
    expect("country" in secondBody.vitals[0]).toBe(false);

    // Explicit valid update propagates to subsequent vitals.
    client.setCountry("de");
    webVitalCallbacks[2]({ name: "INP", value: 120, rating: "good" } as Metric);
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
    const thirdBody = JSON.parse(String(fetcher.mock.calls[2]?.[1]?.body));
    expect(thirdBody.vitals[0].country).toBe("DE");

    await client.close();
  });
});
