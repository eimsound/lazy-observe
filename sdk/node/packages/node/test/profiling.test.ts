import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Profile } from "pprof-format";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  computeCollectionDelay,
  cpuProfileToPprof,
  createProfileUploadID,
  heapProfileToPprof,
  MAX_COLLECTION_BACKOFF_MS,
  resolveProfilingConfig,
  startProfiling,
  type V8CpuProfile,
  type V8HeapProfile,
} from "../src/profiling.js";

afterEach(() => {
  vi.unstubAllGlobals();
});

function fixture<T>(name: string): T {
  return JSON.parse(readFileSync(fileURLToPath(new URL(`fixtures/${name}`, import.meta.url)), "utf8")) as T;
}

function numeric(value: number | bigint): number { return Number(value); }

function decodedNames(profile: Profile): string[] {
  return profile.function.map((fn) => profile.stringTable.strings[numeric(fn.name)] ?? "");
}

function stackNames(profile: Profile, sampleIndex: number): string[] {
  const locations = new Map(profile.location.map((location) => [numeric(location.id), location]));
  const functions = new Map(profile.function.map((fn) => [numeric(fn.id), fn]));
  return profile.sample[sampleIndex]!.locationId.map((locationId) => {
    const line = locations.get(numeric(locationId))!.line[0]!;
    const fn = functions.get(numeric(line.functionId))!;
    return profile.stringTable.strings[numeric(fn.name)]!;
  });
}

describe("V8 to pprof conversion", () => {
  it("converts CPU functions, leaf-first call trees, and wall nanoseconds", () => {
    const decoded = Profile.decode(cpuProfileToPprof(fixture<V8CpuProfile>("cpu-profile.json"), new Date("2026-01-02T03:04:05Z")));
    expect(decodedNames(decoded)).toEqual(expect.arrayContaining(["(root)", "exampleWork", "innerLoop"]));
    expect(stackNames(decoded, 0)).toEqual(["innerLoop", "exampleWork", "(root)"]);
    expect(decoded.sample.reduce((sum, sample) => sum + numeric(sample.value[0]!), 0)).toBe(3);
    expect(decoded.sample.reduce((sum, sample) => sum + numeric(sample.value[1]!), 0)).toBe(6_000_000);
    expect(decoded.stringTable.strings[numeric(decoded.sampleType[1]!.type)]).toBe("cpu");
    expect(decoded.stringTable.strings[numeric(decoded.sampleType[1]!.unit)]).toBe("nanoseconds");
  });

  it("converts sampled heap call trees and inuse_space bytes", () => {
    const start = new Date("2026-01-02T03:04:05Z");
    const decoded = Profile.decode(heapProfileToPprof(fixture<V8HeapProfile>("heap-profile.json"), start, new Date(start.getTime() + 25)));
    expect(decodedNames(decoded)).toEqual(expect.arrayContaining(["(root)", "allocateExample", "makeBuffer"]));
    expect(stackNames(decoded, 1)).toEqual(["makeBuffer", "allocateExample", "(root)"]);
    expect(decoded.sample.reduce((sum, sample) => sum + numeric(sample.value[0]!), 0)).toBe(2);
    expect(decoded.sample.reduce((sum, sample) => sum + numeric(sample.value[1]!), 0)).toBe(6144);
    expect(decoded.stringTable.strings[numeric(decoded.sampleType[1]!.type)]).toBe("inuse_space");
    expect(decoded.stringTable.strings[numeric(decoded.sampleType[1]!.unit)]).toBe("bytes");
  });
});

describe("profiling switches", () => {
  it("defaults to enabled with a 60 second interval", () => expect(resolveProfilingConfig({})).toEqual({ enabled: true, intervalMs: 60_000 }));
  it("honors explicit disable and interval", () => {
    expect(resolveProfilingConfig({ profiling: false, profilingInterval: 1234 })).toEqual({ enabled: false, intervalMs: 1234 });
  });
  it("normalizes a non-positive interval", () => expect(resolveProfilingConfig({ profilingInterval: 0 })).toEqual({ enabled: true, intervalMs: 60_000 }));
});

it("creates a stable-format profile upload identifier", () => {
  expect(createProfileUploadID()).toMatch(/^[0-9a-f]{32}$/);
});

describe("profiling lifecycle and backoff", () => {
  it("caps exponential collection backoff at 15 minutes", () => {
    expect(computeCollectionDelay(60_000, 10_000, 0)).toBe(50_000);
    expect(computeCollectionDelay(60_000, 10_000, 1)).toBe(120_000);
    expect(computeCollectionDelay(60_000, 10_000, 4)).toBe(MAX_COLLECTION_BACKOFF_MS);
    expect(computeCollectionDelay(60_000, 10_000, 10)).toBe(MAX_COLLECTION_BACKOFF_MS);
  });

  it("aborts an in-flight CPU window on shutdown without uploading a truncated sample", async () => {
    const fetcher = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);

    const stop = startProfiling(
      {
        endpoint: "https://observe.example",
        token: "lo_tok",
        service: "test-svc",
        intervalMs: 60_000,
        warn: () => undefined,
        queueCapacity: 4,
        maxAttempts: 1,
        retryInitialMs: 1,
        retryMaxMs: 1,
        onDrop: () => undefined,
      },
      { cpuWindowMs: 5_000 },
    );

    await new Promise((resolve) => setTimeout(resolve, 30));
    await stop({ timeoutMs: 1_000 });

    expect(fetcher).not.toHaveBeenCalled();
  });

  it("activates collection backoff when async profile uploads fail and uses async gzip", async () => {
    const sleepDelays: number[] = [];
    let releaseSecondSleep!: () => void;
    const secondSleep = new Promise<void>((resolve) => { releaseSecondSleep = resolve; });
    const fetcher = vi.fn(async () => new Response("rejected", { status: 422 }));
    vi.stubGlobal("fetch", fetcher);

    const drops: Array<{ reason: string; count: number }> = [];
    const stop = startProfiling(
      {
        endpoint: "https://observe.example",
        token: "lo_tok",
        service: "test-svc",
        intervalMs: 60_000,
        warn: () => undefined,
        queueCapacity: 4,
        maxAttempts: 3,
        retryInitialMs: 1,
        retryMaxMs: 1,
        onDrop: (drop) => drops.push({ reason: drop.reason, count: drop.count }),
      },
      {
        cpuWindowMs: 15,
        sleep: async (ms, signal) => {
          sleepDelays.push(ms);
          if (sleepDelays.length === 1) {
            // Wait until the first cycle's async uploads have failed & recorded drops.
            await vi.waitFor(() => expect(drops.length).toBeGreaterThanOrEqual(1));
            return;
          }
          releaseSecondSleep();
          await new Promise<void>((resolve) => {
            if (signal.aborted) return resolve();
            signal.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      },
    );

    await secondSleep;
    await expect(stop({ timeoutMs: 1_000 })).rejects.toThrow(/HTTP 422/);
    // Permanent 422 must be dropped as server_rejected without retrying 3x per profile.
    expect(drops.every((drop) => drop.reason === "server_rejected")).toBe(true);
    // Second cycle must observe uploadFailures >= 1 and back off beyond intervalMs (60_000 -> >= 120_000).
    expect(sleepDelays[1]).toBeGreaterThanOrEqual(120_000);
  });
});
