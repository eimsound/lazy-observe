import { gzipSync } from "node:zlib";
import { randomBytes } from "node:crypto";
import { Session } from "node:inspector";
import {
  Function as PprofFunction,
  Line,
  Location,
  Profile,
  Sample,
  StringTable,
  ValueType,
} from "pprof-format";
import {
  DeliveryQueue,
  type DeliveryCloseOptions,
  type DeliveryDrop,
} from "./delivery.js";

const CPU_WINDOW_MS = 10_000;
const CPU_PERIOD_NANOS = 1_000_000;
const MAX_COLLECTION_BACKOFF_MS = 15 * 60_000;

export interface V8CallFrame {
  functionName: string;
  scriptId?: string;
  url: string;
  lineNumber: number;
  columnNumber: number;
}

export interface V8CpuProfileNode {
  id: number;
  callFrame: V8CallFrame;
  hitCount?: number;
  children?: number[];
}

export interface V8CpuProfile {
  nodes: V8CpuProfileNode[];
  startTime: number;
  endTime: number;
  samples?: number[];
  timeDeltas?: number[];
}

export interface V8HeapProfileNode {
  callFrame: V8CallFrame;
  selfSize: number;
  id: number;
  children: V8HeapProfileNode[];
}

export interface V8HeapProfile {
  head: V8HeapProfileNode;
  samples?: Array<{ size: number; nodeId: number; ordinal?: number }>;
}

interface ProfileTables {
  strings: StringTable;
  functions: PprofFunction[];
  locations: Location[];
  locationFor(frame: V8CallFrame): number;
}

function profileTables(): ProfileTables {
  const strings = new StringTable();
  const functions: PprofFunction[] = [];
  const locations: Location[] = [];
  const ids = new Map<string, number>();
  const locationFor = (frame: V8CallFrame): number => {
    const name = frame.functionName || "(anonymous)";
    const filename = frame.url || "(unknown)";
    const line = Math.max(0, frame.lineNumber + 1);
    const column = Math.max(0, frame.columnNumber + 1);
    const key = `${name}\u0000${filename}\u0000${line}\u0000${column}`;
    const existing = ids.get(key);
    if (existing !== undefined) return existing;
    const id = functions.length + 1;
    functions.push(new PprofFunction({
      id,
      name: strings.dedup(name),
      systemName: strings.dedup(name),
      filename: strings.dedup(filename),
      startLine: line,
    }));
    locations.push(new Location({
      id,
      address: id,
      line: [new Line({ functionId: id, line })],
    }));
    ids.set(key, id);
    return id;
  };
  return { strings, functions, locations, locationFor };
}

function encodedProfile(
  tables: ProfileTables,
  sampleTypes: Array<[string, string]>,
  samples: Sample[],
  start: Date,
  durationNanos: number,
  period?: number,
): Uint8Array {
  const types = sampleTypes.map(([type, unit]) => new ValueType({ type: tables.strings.dedup(type), unit: tables.strings.dedup(unit) }));
  return new Profile({
    sampleType: types,
    sample: samples,
    location: tables.locations,
    function: tables.functions,
    stringTable: tables.strings,
    timeNanos: BigInt(start.getTime()) * 1_000_000n,
    durationNanos,
    ...(period === undefined ? {} : { periodType: types[types.length - 1], period }),
    defaultSampleType: types[types.length - 1]?.type,
  }).encode();
}

/** Convert V8's leaf node samples and microsecond deltas to leaf-first pprof stacks. */
export function cpuProfileToPprof(profile: V8CpuProfile, start: Date = new Date()): Uint8Array {
  const tables = profileTables();
  const nodes = new Map(profile.nodes.map((node) => [node.id, node]));
  const parents = new Map<number, number>();
  for (const node of profile.nodes) for (const child of node.children ?? []) parents.set(child, node.id);
  const sampleIds = profile.samples ?? profile.nodes.flatMap((node) => Array(Math.max(0, node.hitCount ?? 0)).fill(node.id));
  const durationNanos = Math.max(0, Math.round((profile.endTime - profile.startTime) * 1_000));
  const fallbackDelta = sampleIds.length === 0 ? 0 : Math.max(1, Math.round(durationNanos / sampleIds.length));
  const samples: Sample[] = [];
  for (let index = 0; index < sampleIds.length; index++) {
    const locations: number[] = [];
    const seen = new Set<number>();
    let id: number | undefined = sampleIds[index];
    while (id !== undefined && !seen.has(id)) {
      seen.add(id);
      const node = nodes.get(id);
      if (!node) break;
      locations.push(tables.locationFor(node.callFrame));
      id = parents.get(id);
    }
    if (locations.length > 0) {
      const delta = profile.timeDeltas?.[index];
      samples.push(new Sample({ locationId: locations, value: [1, delta === undefined ? fallbackDelta : Math.max(0, Math.round(delta * 1_000))] }));
    }
  }
  return encodedProfile(tables, [["samples", "count"], ["cpu", "nanoseconds"]], samples, start, durationNanos, CPU_PERIOD_NANOS);
}

/** Convert sampled live heap bytes to pprof's inuse_space sample semantics. */
export function heapProfileToPprof(profile: V8HeapProfile, start: Date = new Date(), end: Date = start): Uint8Array {
  const tables = profileTables();
  const nodes = new Map<number, V8HeapProfileNode>();
  const parents = new Map<number, number>();
  const visit = (node: V8HeapProfileNode, parent?: number) => {
    nodes.set(node.id, node);
    if (parent !== undefined) parents.set(node.id, parent);
    for (const child of node.children ?? []) visit(child, node.id);
  };
  visit(profile.head);
  const allocations = profile.samples?.length
    ? profile.samples.map((sample) => ({ nodeId: sample.nodeId, size: sample.size }))
    : [...nodes.values()].filter((node) => node.selfSize > 0).map((node) => ({ nodeId: node.id, size: node.selfSize }));
  const samples: Sample[] = [];
  for (const allocation of allocations) {
    const locations: number[] = [];
    const seen = new Set<number>();
    let id: number | undefined = allocation.nodeId;
    while (id !== undefined && !seen.has(id)) {
      seen.add(id);
      const node = nodes.get(id);
      if (!node) break;
      locations.push(tables.locationFor(node.callFrame));
      id = parents.get(id);
    }
    if (locations.length > 0 && allocation.size > 0) {
      samples.push(new Sample({ locationId: locations, value: [1, Math.round(allocation.size)] }));
    }
  }
  return encodedProfile(tables, [["inuse_objects", "count"], ["inuse_space", "bytes"]], samples, start, Math.max(0, (end.getTime() - start.getTime()) * 1_000_000));
}

export interface ProfilingOptions {
  endpoint: string;
  token: string;
  service: string;
  intervalMs: number;
  warn: (message: string, error: unknown) => void;
  queueCapacity: number;
  maxAttempts: number;
  retryInitialMs: number;
  retryMaxMs: number;
  onDrop: (drop: DeliveryDrop) => void;
}

interface CollectedProfile {
  uploadId: string;
  type: "cpu" | "heap";
  start: Date;
  end: Date;
  data: Uint8Array;
}

export function createProfileUploadID(): string {
  return randomBytes(16).toString("hex");
}

function post(session: Session, method: string, params?: Record<string, unknown>): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => session.post(method, params ?? {}, (error, result) => error ? reject(error) : resolve(result as Record<string, unknown>)));
}

function wait(ms: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms);
    timer.unref();
    signal.addEventListener("abort", done, { once: true });
    function done() { clearTimeout(timer); signal.removeEventListener("abort", done); resolve(); }
  });
}

function inspectorUnavailable(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return /already started|not connected|session.*closed|method.*not found|wasn't found/i.test(message);
}

async function upload(
  options: ProfilingOptions,
  collected: CollectedProfile,
  signal: AbortSignal,
): Promise<void> {
  const form = new FormData();
  form.append("profile", new Blob([gzipSync(collected.data)], { type: "application/octet-stream" }), "profile.pb.gz");
  form.append("upload_id", collected.uploadId);
  form.append("type", collected.type);
  form.append("service", options.service);
  form.append("start", collected.start.toISOString());
  form.append("end", collected.end.toISOString());
  form.append("labels", JSON.stringify({ platform: "node" }));
  const response = await fetch(`${options.endpoint}/api/profiles`, {
    method: "POST",
    headers: { Authorization: `Bearer ${options.token}` },
    body: form,
    signal: AbortSignal.any([signal, AbortSignal.timeout(15_000)]),
  });
  if (!response.ok) throw new Error(`profile export failed: HTTP ${response.status}`);
}

async function collect(session: Session, signal: AbortSignal): Promise<CollectedProfile[]> {
  const start = new Date();
  let cpuStarted = false;
  let heapStarted = false;
  try {
    await post(session, "HeapProfiler.startSampling");
    heapStarted = true;
    await post(session, "Profiler.start");
    cpuStarted = true;
    await wait(CPU_WINDOW_MS, signal);
    const cpuResult = await post(session, "Profiler.stop");
    cpuStarted = false;
    const cpuEnd = new Date();
    const heapResult = await post(session, "HeapProfiler.stopSampling");
    heapStarted = false;
    const heapEnd = new Date();
    return [
      {
        uploadId: createProfileUploadID(),
        type: "cpu",
        start,
        end: cpuEnd,
        data: cpuProfileToPprof(cpuResult.profile as V8CpuProfile, start),
      },
      {
        uploadId: createProfileUploadID(),
        type: "heap",
        start,
        end: heapEnd,
        data: heapProfileToPprof(heapResult.profile as V8HeapProfile, start, heapEnd),
      },
    ];
  } finally {
    if (cpuStarted) await post(session, "Profiler.stop").catch(() => {});
    if (heapStarted) await post(session, "HeapProfiler.stopSampling").catch(() => {});
  }
}

export function startProfiling(options: ProfilingOptions): (closeOptions?: DeliveryCloseOptions) => Promise<void> {
  const session = new Session();
  const controller = new AbortController();
  const delivery = new DeliveryQueue<CollectedProfile>({
    capacity: options.queueCapacity,
    batchSize: 1,
    flushIntervalMs: 0,
    maxAttempts: options.maxAttempts,
    retryInitialMs: options.retryInitialMs,
    retryMaxMs: options.retryMaxMs,
    send: (profiles, signal) => {
      const profile = profiles[0];
      if (!profile) return Promise.resolve();
      return upload(options, profile, signal);
    },
    onDrop: options.onDrop,
  });
  let warned = false;
  let stopped = false;
  const warnOnce = (error: unknown) => {
    if (warned) return;
    warned = true;
    options.warn("lazy-observe profiling disabled: Node inspector unavailable", error);
  };
  const run = async () => {
    try {
      session.connect();
      await post(session, "Profiler.enable");
      await post(session, "Profiler.setSamplingInterval", { interval: 1_000 });
      await post(session, "HeapProfiler.enable");
    } catch (error) {
      warnOnce(error);
      try { session.disconnect(); } catch { /* disconnected */ }
      return;
    }
    let failures = 0;
    while (!controller.signal.aborted) {
      const cycleStart = Date.now();
      let failed = false;
      try {
        const profiles = await collect(session, controller.signal);
        for (const profile of profiles) delivery.enqueue(profile);
      } catch (error) {
        if (inspectorUnavailable(error)) {
          warnOnce(error);
          break;
        }
        failed = true;
      }
      failures = failed ? failures + 1 : 0;
      if (controller.signal.aborted) break;
      const delay = failures > 0
        ? Math.min(MAX_COLLECTION_BACKOFF_MS, options.intervalMs * (2 ** Math.min(failures, 8)))
        : Math.max(0, options.intervalMs - (Date.now() - cycleStart));
      await wait(delay, controller.signal);
    }
    try { session.disconnect(); } catch { /* disconnected */ }
  };
  const done = run().catch((error) => warnOnce(error));
  return async (closeOptions = {}) => {
    if (stopped) return;
    stopped = true;
    controller.abort();
    await done;
    await delivery.close(closeOptions);
  };
}

export function resolveProfilingConfig(config: { profiling?: boolean; profilingInterval?: number }): { enabled: boolean; intervalMs: number } {
  return { enabled: config.profiling ?? true, intervalMs: config.profilingInterval && config.profilingInterval > 0 ? config.profilingInterval : 60_000 };
}
