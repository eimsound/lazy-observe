import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Profile } from "pprof-format";
import { describe, expect, it } from "vitest";
import {
  cpuProfileToPprof,
  createProfileUploadID,
  heapProfileToPprof,
  resolveProfilingConfig,
  type V8CpuProfile,
  type V8HeapProfile,
} from "../src/profiling.js";

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
