import { ReplayRingBuffer, type TimedReplayEvent } from "./replay-buffer";
import {
  DEFAULT_REQUEST_TIMEOUT_MS,
  DeliveryQueue,
  type DeliveryCloseOptions,
  type DeliveryDiagnostics,
  type DeliveryDrop,
} from "./delivery";
import type { ErrorEventPayload, ReplayConfig, ReplayVitalSample } from "./types";
import type { Transport } from "./transport";

type StopRecording = () => void;
type CustomEvent = { tag: string; payload: unknown };

export const REPLAY_CUSTOM_EVENT_TAGS = {
  error: "lazy-observe:error",
  http: "lazy-observe:http",
  vital: "lazy-observe:vital",
} as const;

export interface ReplayHTTPMarkerPayload {
  timestamp: number;
  method: string;
  path: string;
  status: number;
  duration_ms: number;
}

export type ReplayVitalMarkerPayload = ReplayVitalSample;

const MAX_PENDING_CUSTOM_EVENTS = 200;
const utf8Encoder = new TextEncoder();

interface ReplayTransport {
  replay(
    sessionId: string,
    seq: number,
    body: BodyInit,
    compressed: boolean,
    error: boolean,
    keepalive?: boolean,
    signal?: AbortSignal,
  ): Promise<void>;
}

export interface ReplayDeliveryOptions {
  capacity?: number;
  maxAttempts?: number;
  retryInitialMs?: number;
  retryMaxMs?: number;
  requestTimeoutMs?: number;
  wait?: (delayMs: number, signal?: AbortSignal) => Promise<void>;
  onDrop?: (drop: DeliveryDrop) => void;
  storage?: Storage;
  loadRrweb?: () => Promise<typeof import("rrweb")>;
}

interface ReplaySegment {
  seq: number;
  events: TimedReplayEvent[];
  error: boolean;
  encoded?: Promise<{ body: BodyInit; compressed: boolean }>;
}

const REPLAY_SEQ_STORAGE_PREFIX = "lazy-observe:replay-seq:";

function replaySeqKey(sessionId: string): string {
  return `${REPLAY_SEQ_STORAGE_PREFIX}${sessionId}`;
}

function readPersistedSequence(storage: Storage | undefined, key: string): number {
  try {
    const raw = storage?.getItem(key);
    if (raw == null) return 0;
    const parsed = Number(raw);
    return Number.isInteger(parsed) && parsed >= 0 ? parsed : 0;
  } catch {
    return 0;
  }
}

function writePersistedSequence(storage: Storage | undefined, key: string, nextSequence: number): void {
  try {
    storage?.setItem(key, String(nextSequence));
  } catch {
    /* storage unavailable or quota exceeded */
  }
}

export class ReplaySegmentDelivery {
  private readonly queue: DeliveryQueue<ReplaySegment>;
  private readonly storage?: Storage;
  private readonly seqKey: string;
  private nextSequence: number;

  constructor(sessionId: string, transport: ReplayTransport, options: ReplayDeliveryOptions = {}) {
    this.storage = options.storage;
    this.seqKey = replaySeqKey(sessionId);
    this.nextSequence = readPersistedSequence(this.storage, this.seqKey);
    this.queue = new DeliveryQueue({
      capacity: options.capacity ?? 10,
      batchSize: 1,
      flushIntervalMs: 0,
      maxAttempts: options.maxAttempts ?? 3,
      retryInitialMs: options.retryInitialMs ?? 100,
      retryMaxMs: options.retryMaxMs ?? 2_000,
      requestTimeoutMs: options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS,
      wait: options.wait,
      onDrop: options.onDrop,
      send: async ([segment], sendOptions) => {
        // On page unload (keepalive), avoid awaiting async CompressionStream so fetch()
        // is dispatched synchronously before the browser tears down the event loop.
        const encoded = sendOptions.keepalive && !segment.encoded
          ? encodeReplayEventsSync(segment.events)
          : await (segment.encoded ??= encodeReplayEvents(segment.events));
        await transport.replay(
          sessionId,
          segment.seq,
          encoded.body,
          encoded.compressed,
          segment.error,
          sendOptions.keepalive,
          sendOptions.signal,
        );
      },
    });
  }

  enqueue(events: TimedReplayEvent[], error: boolean): boolean {
    if (!events.length) return true;
    const seq = Math.max(this.nextSequence, readPersistedSequence(this.storage, this.seqKey));
    const accepted = this.queue.enqueue({
      seq,
      events: [...events],
      error,
    });
    if (accepted) {
      this.nextSequence = seq + 1;
      writePersistedSequence(this.storage, this.seqKey, this.nextSequence);
    }
    return accepted;
  }

  flush(keepalive = false): Promise<void> {
    return this.queue.flush(keepalive);
  }

  close(options?: DeliveryCloseOptions): Promise<void> {
    return this.queue.close(options);
  }

  diagnostics(): DeliveryDiagnostics {
    return this.queue.diagnostics();
  }
}

function fallbackReplayPath(raw: string): string {
  let value = raw.split(/[?#]/, 1)[0] || "/";
  const scheme = value.indexOf("://");
  const authorityStart = scheme >= 0 ? scheme + 3 : value.startsWith("//") ? 2 : -1;
  if (authorityStart >= 0) {
    const pathStart = value.indexOf("/", authorityStart);
    value = pathStart >= 0 ? value.slice(pathStart) : "/";
  }
  return value.startsWith("/") ? value : `/${value}`;
}

/** Return a path only: origins, credentials, query strings and fragments never enter Replay. */
export function replayRequestPath(input: RequestInfo | URL): string {
  const raw = typeof Request !== "undefined" && input instanceof Request ? input.url : String(input);
  try {
    const base = typeof location === "undefined" ? "http://localhost" : location.href;
    return new URL(raw, base).pathname || "/";
  } catch {
    return fallbackReplayPath(raw);
  }
}

export function replayRequestMethod(input: RequestInfo | URL, init?: RequestInit): string {
  const requestMethod = typeof Request !== "undefined" && input instanceof Request ? input.method : undefined;
  const method = String(init?.method ?? requestMethod ?? "GET").trim().toUpperCase();
  return /^[A-Z0-9!#$%&'*+.^_`|~-]{1,32}$/.test(method) ? method : "GET";
}

export function buildReplayHTTPMarker(
  method: string,
  input: RequestInfo | URL,
  status: number,
  durationMs: number,
  timestamp: number = Date.now(),
): ReplayHTTPMarkerPayload {
  return {
    timestamp: Math.max(0, Math.trunc(timestamp)),
    method: /^[A-Z0-9!#$%&'*+.^_`|~-]{1,32}$/.test(method.toUpperCase()) ? method.toUpperCase() : "GET",
    path: replayRequestPath(input).slice(0, 2048),
    status: Number.isFinite(status) ? Math.min(599, Math.max(0, Math.trunc(status))) : 0,
    duration_ms: Number.isFinite(durationMs) ? Math.round(Math.max(0, durationMs) * 100) / 100 : 0,
  };
}

export function buildReplayVitalMarker(sample: ReplayVitalSample): ReplayVitalMarkerPayload {
  return {
    timestamp: Math.max(0, Math.trunc(sample.timestamp)),
    name: sample.name,
    value: Number.isFinite(sample.value) ? Math.max(0, sample.value) : 0,
    rating: sample.rating,
  };
}

export class ReplayController {
  private readonly ring: ReplayRingBuffer<TimedReplayEvent>;
  private readonly sampled: boolean;
  private readonly loadRrweb: () => Promise<typeof import("rrweb")>;
  private stopRecording?: StopRecording;
  private timer?: ReturnType<typeof setInterval>;
  private addCustomEvent?: (tag: string, payload: unknown) => void;
  private pendingCustomEvents: CustomEvent[] = [];
  private readonly delivery: ReplaySegmentDelivery;
  private closed = false;

  constructor(
    private readonly config: Required<ReplayConfig>,
    sessionId: string,
    transport: Transport,
    deliveryOptions: ReplayDeliveryOptions = {},
  ) {
    this.ring = new ReplayRingBuffer(config.bufferMs, Math.max(config.maxSegmentBytes * 2, 5_000_000));
    this.sampled = !config.onErrorOnly && Math.random() < config.sampleRate;
    this.loadRrweb = deliveryOptions.loadRrweb ?? (() => import("rrweb"));
    this.delivery = new ReplaySegmentDelivery(sessionId, transport, deliveryOptions);
  }

  async start(): Promise<void> {
    if (this.closed || typeof window === "undefined") return;
    if (!this.sampled && !this.config.onErrorFullCapture) return;
    let rrweb: typeof import("rrweb");
    try {
      rrweb = await this.loadRrweb();
    } catch (error) {
      if (!this.closed) {
        console.warn(
          "[lazy-observe] session replay disabled: optional peer dependency \"rrweb\" is unavailable",
          error,
        );
      }
      return;
    }
    if (this.closed) return;
    const stop = rrweb.record({
      emit: (event, isCheckout) => {
        if (this.closed) return;
        this.ring.push(event as TimedReplayEvent, Boolean(isCheckout));
        // rrweb rejects addCustomEvent until recording has produced its initial
        // snapshot. Retry queued markers after each recorder emission.
        queueMicrotask(() => this.flushPendingCustomEvents());
        if (this.sampled && this.ring.byteLength >= this.config.maxSegmentBytes) this.scheduleUpload(false, true);
      },
      checkoutEveryNms: this.config.checkoutEveryNms,
      maskAllInputs: true,
      blockSelector: this.config.blockSelector
    });
    if (this.closed) {
      stop?.();
      return;
    }
    this.addCustomEvent = rrweb.addCustomEvent;
    if (stop) this.stopRecording = stop;
    this.flushPendingCustomEvents();
    if (this.sampled) this.timer = setInterval(() => this.scheduleUpload(false, true), this.config.flushIntervalMs);
  }

  private custom(tag: string, payload: unknown): void {
    if (this.closed) return;
    if (this.addCustomEvent) {
      try {
        this.addCustomEvent(tag, payload);
        return;
      } catch {
        // rrweb is imported but may still be waiting for DOMContentLoaded.
      }
    }
    if (this.pendingCustomEvents.length >= MAX_PENDING_CUSTOM_EVENTS) this.pendingCustomEvents.shift();
    this.pendingCustomEvents.push({ tag, payload });
  }

  private flushPendingCustomEvents(): void {
    if (this.closed || !this.addCustomEvent || !this.pendingCustomEvents.length) return;
    const pending = this.pendingCustomEvents;
    this.pendingCustomEvents = [];
    for (let index = 0; index < pending.length; index++) {
      const event = pending[index];
      try {
        this.addCustomEvent(event.tag, event.payload);
      } catch {
        this.pendingCustomEvents.unshift(...pending.slice(index));
        return;
      }
    }
  }

  page(page: string): void { this.custom("lazy-observe:page", { page }); }

  error(event: ErrorEventPayload): void {
    if (this.closed) return;
    this.custom(REPLAY_CUSTOM_EVENT_TAGS.error, {
      timestamp: event.timestamp,
      event_id: event.event_id,
      title: event.message ?? event.exception.value,
      level: event.level,
    });
    if (!this.sampled && this.config.onErrorFullCapture) this.scheduleUpload(true, false);
  }

  http(method: string, input: RequestInfo | URL, status: number, durationMs: number, timestamp: number = Date.now()): void {
    this.custom(REPLAY_CUSTOM_EVENT_TAGS.http, buildReplayHTTPMarker(method, input, status, durationMs, timestamp));
  }

  vital(sample: ReplayVitalSample): void {
    this.custom(REPLAY_CUSTOM_EVENT_TAGS.vital, buildReplayVitalMarker(sample));
  }

  private scheduleUpload(error: boolean, _drain: boolean): void {
    const events = this.ring.snapshot();
    if (!events.length) return;
    if (this.delivery.enqueue(events, error)) this.ring.clear();
  }

  async flush(keepalive = false): Promise<void> {
    if (this.sampled) this.scheduleUpload(false, true);
    await this.delivery.flush(keepalive);
  }

  async close(options?: DeliveryCloseOptions): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    this.stopRecording?.();
    this.stopRecording = undefined;
    this.addCustomEvent = undefined;
    this.pendingCustomEvents = [];
    await this.flush();
    await this.delivery.close(options);
  }
}

function encodeReplayEventsSync(events: TimedReplayEvent[]): { body: BodyInit; compressed: boolean } {
  return { body: utf8Encoder.encode(JSON.stringify(events)), compressed: false };
}

async function encodeReplayEvents(events: TimedReplayEvent[]): Promise<{ body: BodyInit; compressed: boolean }> {
  const bytes = utf8Encoder.encode(JSON.stringify(events));
  if (typeof CompressionStream === "undefined") return { body: bytes, compressed: false };
  const stream = new Blob([bytes]).stream().pipeThrough(new CompressionStream("gzip"));
  return { body: await new Response(stream).blob(), compressed: true };
}

export function replayDefaults(config: ReplayConfig = {}): Required<ReplayConfig> {
  const sampleRate = config.sampleRate ?? 0.1;
  if (sampleRate < 0 || sampleRate > 1) throw new Error("replay.sampleRate must be between 0 and 1");
  const bufferMs = config.bufferMs ?? 60_000;
  const checkoutEveryNms = config.checkoutEveryNms ?? Math.max(1_000, Math.floor(bufferMs / 2));
  return {
    sampleRate,
    onErrorFullCapture: config.onErrorFullCapture ?? true,
    onErrorOnly: config.onErrorOnly ?? false,
    bufferMs,
    checkoutEveryNms,
    flushIntervalMs: config.flushIntervalMs ?? 10_000,
    maxSegmentBytes: config.maxSegmentBytes ?? 500_000,
    blockSelector: config.blockSelector ?? "[data-lo-block]"
  };
}
