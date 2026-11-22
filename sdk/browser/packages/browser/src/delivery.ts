export const MAX_KEEPALIVE_BYTES: number = 60 * 1024;
export const DEFAULT_REQUEST_TIMEOUT_MS: number = 10_000;
export const MAX_RETRY_AFTER_MS: number = 30_000;

export type DeliveryDropReason = "queue_full" | "retry_exhausted" | "shutdown_budget" | "server_rejected";

/** Parse an HTTP Retry-After header (delta-seconds or HTTP-date) into milliseconds. */
export function parseRetryAfterMs(header: string | null | undefined, now: number = Date.now()): number | undefined {
  const raw = header?.trim();
  if (!raw) return undefined;
  if (/^\d+(?:\.\d+)?$/.test(raw)) {
    const seconds = Number(raw);
    return Number.isFinite(seconds) && seconds >= 0 ? Math.max(0, Math.round(seconds * 1_000)) : undefined;
  }
  const timestamp = Date.parse(raw);
  if (Number.isFinite(timestamp)) return Math.max(0, timestamp - now);
  return undefined;
}

/** A non-2xx ingest response, so callers can tell a refusal from a network failure. */
export class TransportError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly retryAfterMs?: number,
  ) {
    super(message);
    this.name = "TransportError";
  }
}

/**
 * A 4xx (except 408 and 429) means the server will never accept this payload as-is: retrying the
 * same body is pointless. 408 (request timeout) and 429 (rate limited) are transient — the same
 * payload may well be accepted later — so they are treated like a network error instead.
 */
export function isServerRejection(error: unknown): boolean {
  if (!(error instanceof TransportError)) return false;
  const { status } = error;
  if (status === 408 || status === 429) return false;
  return status >= 400 && status < 500;
}

export interface DeliveryDrop {
  reason: DeliveryDropReason;
  count: number;
  error?: unknown;
}

export interface DeliveryDiagnostics {
  pending: number;
  dropped: number;
  lastError?: unknown;
}

export interface DeliverySendOptions {
  keepalive: boolean;
  signal: AbortSignal;
}

export interface DeliveryCloseOptions {
  timeoutMs?: number;
}

export interface DeliveryQueueOptions<T> {
  capacity: number;
  batchSize: number;
  flushIntervalMs: number;
  maxAttempts: number;
  retryInitialMs: number;
  retryMaxMs: number;
  maxRetryAfterMs?: number;
  requestTimeoutMs?: number;
  maxBatchBytes?: number;
  keepaliveMaxBatchBytes?: number;
  batchOverheadBytes?: number;
  estimateItemBytes?: (item: T) => number;
  dedupeKey?: (item: T) => string;
  batchDistinctKey?: (item: T) => string;
  maxBatchDistinctKeys?: number;
  send: (batch: T[], options: DeliverySendOptions) => Promise<void>;
  wait?: (delayMs: number, signal?: AbortSignal) => Promise<void>;
  onDrop?: (drop: DeliveryDrop) => void;
  isNonRetryable?: (error: unknown) => boolean;
  retryAfterMs?: (error: unknown) => number | undefined;
  random?: () => number;
}

interface QueuedEntry<T> {
  item: T;
  bytes: number;
  dedupeKey?: string;
}

export class DeliveryQueue<T> {
  private readonly items: Array<QueuedEntry<T>> = [];
  private readonly pendingByKey = new Map<string, QueuedEntry<T>>();
  private readonly controller = new AbortController();
  private readonly wait: (delayMs: number, signal?: AbortSignal) => Promise<void>;
  private readonly isNonRetryable: (error: unknown) => boolean;
  private readonly retryAfterMs: (error: unknown) => number | undefined;
  private readonly random: () => number;
  private readonly maxRetryAfterMs: number;
  private readonly requestTimeoutMs: number;
  private readonly maxBatchBytes?: number;
  private readonly keepaliveMaxBatchBytes?: number;
  private readonly batchOverheadBytes: number;
  private readonly estimateItemBytes?: (item: T) => number;
  private readonly dedupeKey?: (item: T) => string;
  private readonly batchDistinctKey?: (item: T) => string;
  private readonly maxBatchDistinctKeys?: number;
  private timer?: ReturnType<typeof setTimeout>;
  private running?: Promise<void>;
  private readonly inFlight = new Set<Promise<void>>();
  private readonly keepaliveInFlight = new Set<Promise<void>>();
  private queuedBytes = 0;
  private resident = 0;
  private dropped = 0;
  private lastError?: unknown;
  private closed = false;

  constructor(private readonly options: DeliveryQueueOptions<T>) {
    if (!Number.isInteger(options.capacity) || options.capacity <= 0) {
      throw new Error("delivery queue capacity must be a positive integer");
    }
    if (!Number.isInteger(options.batchSize) || options.batchSize <= 0) {
      throw new Error("delivery batch size must be a positive integer");
    }
    if (!Number.isInteger(options.maxAttempts) || options.maxAttempts <= 0) {
      throw new Error("delivery maxAttempts must be a positive integer");
    }
    if (options.flushIntervalMs < 0 || options.retryInitialMs < 0 || options.retryMaxMs < 0) {
      throw new Error("delivery timing values cannot be negative");
    }
    if (options.maxRetryAfterMs !== undefined && (!Number.isFinite(options.maxRetryAfterMs) || options.maxRetryAfterMs < 0)) {
      throw new Error("delivery maxRetryAfterMs cannot be negative");
    }
    if (options.requestTimeoutMs !== undefined && (!Number.isFinite(options.requestTimeoutMs) || options.requestTimeoutMs < 0)) {
      throw new Error("delivery requestTimeoutMs cannot be negative");
    }
    if (options.maxBatchBytes !== undefined && (!Number.isFinite(options.maxBatchBytes) || options.maxBatchBytes <= 0)) {
      throw new Error("delivery maxBatchBytes must be positive");
    }
    if (options.keepaliveMaxBatchBytes !== undefined && (!Number.isFinite(options.keepaliveMaxBatchBytes) || options.keepaliveMaxBatchBytes <= 0)) {
      throw new Error("delivery keepaliveMaxBatchBytes must be positive");
    }
    if (options.batchOverheadBytes !== undefined && (!Number.isFinite(options.batchOverheadBytes) || options.batchOverheadBytes < 0)) {
      throw new Error("delivery batchOverheadBytes cannot be negative");
    }
    if (options.maxBatchDistinctKeys !== undefined) {
      if (!Number.isInteger(options.maxBatchDistinctKeys) || options.maxBatchDistinctKeys <= 0) {
        throw new Error("delivery maxBatchDistinctKeys must be a positive integer");
      }
      if (!options.batchDistinctKey) {
        throw new Error("delivery maxBatchDistinctKeys requires batchDistinctKey");
      }
    }
    this.wait = options.wait ?? waitForRetry;
    this.isNonRetryable = options.isNonRetryable ?? isServerRejection;
    this.retryAfterMs = options.retryAfterMs ?? defaultRetryAfterMs;
    this.random = options.random ?? Math.random;
    this.maxRetryAfterMs = options.maxRetryAfterMs ?? MAX_RETRY_AFTER_MS;
    this.requestTimeoutMs = options.requestTimeoutMs ?? 0;
    this.maxBatchBytes = options.maxBatchBytes;
    this.keepaliveMaxBatchBytes = options.keepaliveMaxBatchBytes;
    this.batchOverheadBytes = options.batchOverheadBytes ?? 0;
    this.estimateItemBytes = options.estimateItemBytes;
    this.dedupeKey = options.dedupeKey;
    this.batchDistinctKey = options.batchDistinctKey;
    this.maxBatchDistinctKeys = options.maxBatchDistinctKeys;
  }

  enqueue(item: T): boolean {
    if (this.closed) {
      this.recordDrop({ reason: "shutdown_budget", count: 1 });
      return false;
    }
    const bytes = this.estimateItemBytes ? Math.max(0, this.estimateItemBytes(item)) : 0;
    let key: string | undefined;
    if (this.dedupeKey) {
      key = this.dedupeKey(item);
      const existing = this.pendingByKey.get(key);
      if (existing) {
        this.queuedBytes += bytes - existing.bytes;
        existing.item = item;
        existing.bytes = bytes;
        this.scheduleFlush();
        return true;
      }
    }
    if (this.resident >= this.options.capacity) {
      this.recordDrop({ reason: "queue_full", count: 1 });
      return false;
    }
    const entry: QueuedEntry<T> = key !== undefined ? { item, bytes, dedupeKey: key } : { item, bytes };
    this.items.push(entry);
    if (key !== undefined) this.pendingByKey.set(key, entry);
    this.queuedBytes += bytes;
    this.resident++;
    this.scheduleFlush();
    return true;
  }

  async flush(keepalive = false): Promise<void> {
    this.clearTimer();
    if (keepalive) {
      await this.drainKeepalive();
      return;
    }
    while (this.running || this.items.length > 0 || this.inFlight.size > 0) {
      if (this.items.length > 0 && !this.running) {
        this.startDrain();
      }
      if (this.running) {
        await this.running;
      } else if (this.inFlight.size > 0) {
        await Promise.all(Array.from(this.inFlight));
      }
    }
  }

  async close(options: DeliveryCloseOptions = {}): Promise<void> {
    if (!this.closed) {
      this.closed = true;
      this.clearTimer();
    }
    const timeoutMs = options.timeoutMs ?? 10_000;
    let timeout: ReturnType<typeof setTimeout> | undefined;
    if (Number.isFinite(timeoutMs) && timeoutMs > 0) {
      timeout = setTimeout(() => {
        this.controller.abort(new Error(`delivery shutdown timed out after ${timeoutMs}ms`));
      }, timeoutMs);
    }
    try {
      await this.flush();
    } finally {
      if (timeout !== undefined) clearTimeout(timeout);
    }
  }

  diagnostics(): DeliveryDiagnostics {
    return { pending: this.resident, dropped: this.dropped, lastError: this.lastError };
  }

  private scheduleFlush(): void {
    const effectiveBatchSize = Math.min(this.options.batchSize, this.options.capacity);
    const bytesFull = this.maxBatchBytes !== undefined
      && this.estimateItemBytes !== undefined
      && (this.batchOverheadBytes + this.queuedBytes + Math.max(0, this.items.length - 1)) >= this.maxBatchBytes;
    if (this.items.length >= effectiveBatchSize || bytesFull || this.hasFullDistinctKeyBatch()) {
      this.clearTimer();
      queueMicrotask(() => { void this.startDrain(); });
    } else if (!this.timer) {
      this.timer = setTimeout(() => {
        this.timer = undefined;
        void this.startDrain();
      }, this.options.flushIntervalMs);
    }
  }

  private hasFullDistinctKeyBatch(): boolean {
    if (
      this.maxBatchDistinctKeys === undefined
      || !this.batchDistinctKey
      || this.items.length < this.maxBatchDistinctKeys
    ) {
      return false;
    }
    const seen = new Set<string>();
    for (const entry of this.items) {
      seen.add(this.batchDistinctKey(entry.item));
      if (seen.size >= this.maxBatchDistinctKeys) return true;
    }
    return false;
  }

  private startDrain(): Promise<void> {
    if (this.running) return this.running;
    if (this.items.length === 0) return Promise.resolve();
    const run = this.drain().finally(() => {
      if (this.running === run) this.running = undefined;
      if (this.items.length > 0 && !this.timer) {
        queueMicrotask(() => { void this.startDrain(); });
      }
    });
    this.running = run;
    return run;
  }

  private takeBatch(keepalive: boolean): T[] {
    const limit = Math.min(this.options.batchSize, this.options.capacity, this.items.length);
    if (limit <= 0) return [];
    const budget = keepalive && this.keepaliveMaxBatchBytes !== undefined
      ? Math.min(this.keepaliveMaxBatchBytes, this.maxBatchBytes ?? Number.POSITIVE_INFINITY)
      : this.maxBatchBytes;
    const enforceBytes = budget !== undefined && this.estimateItemBytes !== undefined;
    const distinctKeys = this.maxBatchDistinctKeys !== undefined && this.batchDistinctKey
      ? new Set<string>([this.batchDistinctKey(this.items[0].item)])
      : undefined;
    let batchBytes = enforceBytes ? this.batchOverheadBytes + this.items[0].bytes : 0;
    let count = 1;
    while (count < limit) {
      const entry = this.items[count];
      let nextBatchBytes = batchBytes;
      if (enforceBytes) {
        nextBatchBytes += entry.bytes + 1;
        if (nextBatchBytes > budget!) break;
      }
      let nextKey: string | undefined;
      if (distinctKeys) {
        nextKey = this.batchDistinctKey!(entry.item);
        if (!distinctKeys.has(nextKey) && distinctKeys.size >= this.maxBatchDistinctKeys!) break;
      }
      batchBytes = nextBatchBytes;
      if (distinctKeys && nextKey !== undefined) distinctKeys.add(nextKey);
      count++;
    }
    const entries = this.items.splice(0, count);
    for (const entry of entries) {
      this.queuedBytes -= entry.bytes;
      if (entry.dedupeKey !== undefined) this.pendingByKey.delete(entry.dedupeKey);
    }
    return entries.map((entry) => entry.item);
  }

  private startBatchDelivery(batch: T[], keepalive: boolean): Promise<void> {
    const delivery = this.deliver(batch, keepalive);
    const tracked = delivery.finally(() => {
      this.resident -= batch.length;
      this.inFlight.delete(tracked);
      if (keepalive) this.keepaliveInFlight.delete(tracked);
    });
    this.inFlight.add(tracked);
    if (keepalive) this.keepaliveInFlight.add(tracked);
    return tracked;
  }

  private async drain(): Promise<void> {
    while (this.items.length > 0) {
      if (this.controller.signal.aborted) {
        this.dropQueued(abortReason(this.controller.signal));
        return;
      }
      const batch = this.takeBatch(false);
      await this.startBatchDelivery(batch, false);
    }
  }

  private async drainKeepalive(): Promise<void> {
    while (this.items.length > 0 || this.keepaliveInFlight.size > 0) {
      if (this.items.length > 0) {
        if (this.controller.signal.aborted) {
          this.dropQueued(abortReason(this.controller.signal));
        } else {
          // Dispatch all queued batches synchronously before awaiting so pagehide/visibilitychange
          // unload handlers fire fetch(..., { keepalive: true }) even if an async drain is stalled.
          while (this.items.length > 0) {
            if (this.controller.signal.aborted) {
              this.dropQueued(abortReason(this.controller.signal));
              break;
            }
            const batch = this.takeBatch(true);
            if (batch.length === 0) break;
            this.startBatchDelivery(batch, true);
          }
        }
      }
      if (this.keepaliveInFlight.size > 0) {
        await Promise.all(Array.from(this.keepaliveInFlight));
      }
    }
  }

  private async deliver(batch: T[], keepalive: boolean): Promise<void> {
    let lastError: unknown;
    for (let attempt = 0; attempt < this.options.maxAttempts; attempt++) {
      const attemptScope = createAttemptSignal(
        this.controller.signal,
        keepalive ? 0 : this.requestTimeoutMs,
      );
      try {
        await abortable(
          this.options.send(batch, { keepalive, signal: attemptScope.signal }),
          attemptScope.signal,
        );
        this.lastError = undefined;
        return;
      } catch (error) {
        lastError = error;
        if (this.controller.signal.aborted) {
          this.lastError = lastError;
          this.recordDrop({ reason: "shutdown_budget", count: batch.length, error: lastError });
          return;
        }
        if (this.isNonRetryable(error)) {
          this.lastError = error;
          this.recordDrop({ reason: "server_rejected", count: batch.length, error });
          return;
        }
      } finally {
        attemptScope.cleanup();
      }
      if (attempt + 1 < this.options.maxAttempts) {
        try {
          await abortable(
            this.wait(this.retryDelay(attempt, lastError), this.controller.signal),
            this.controller.signal,
          );
        } catch (error) {
          lastError = error;
          if (this.controller.signal.aborted) {
            this.lastError = error;
            this.recordDrop({ reason: "shutdown_budget", count: batch.length, error });
            return;
          }
          break;
        }
      }
    }
    this.lastError = lastError;
    this.recordDrop({ reason: "retry_exhausted", count: batch.length, error: lastError });
  }

  private retryDelay(retryIndex: number, error?: unknown): number {
    const retryAfter = this.retryAfterMs(error);
    if (retryAfter !== undefined && Number.isFinite(retryAfter) && retryAfter >= 0) {
      return Math.min(retryAfter, this.maxRetryAfterMs);
    }
    const scaled = this.options.retryInitialMs * (2 ** retryIndex);
    const base = Math.min(Number.isFinite(scaled) ? scaled : this.options.retryMaxMs, this.options.retryMaxMs);
    if (base <= 0) return 0;
    // Full Jitter: uniform [0, min(retryMaxMs, retryInitialMs * 2^attempt)].
    const jitter = Math.min(1, Math.max(0, this.random()));
    return Math.round(base * jitter);
  }

  private dropQueued(error: unknown): void {
    const count = this.items.length;
    this.items.length = 0;
    this.pendingByKey.clear();
    this.queuedBytes = 0;
    this.resident -= count;
    this.lastError = error;
    if (count > 0) this.recordDrop({ reason: "shutdown_budget", count, error });
  }

  private recordDrop(drop: DeliveryDrop): void {
    this.dropped += drop.count;
    this.options.onDrop?.(drop);
  }

  private clearTimer(): void {
    if (!this.timer) return;
    clearTimeout(this.timer);
    this.timer = undefined;
  }
}

function createAttemptSignal(
  shutdownSignal: AbortSignal,
  timeoutMs: number,
): { signal: AbortSignal; cleanup: () => void } {
  if (timeoutMs <= 0) return { signal: shutdownSignal, cleanup: () => {} };
  const attemptController = new AbortController();
  if (shutdownSignal.aborted) {
    attemptController.abort(abortReason(shutdownSignal));
    return { signal: attemptController.signal, cleanup: () => {} };
  }
  const onShutdown = () => attemptController.abort(abortReason(shutdownSignal));
  shutdownSignal.addEventListener("abort", onShutdown, { once: true });
  const timer = setTimeout(() => {
    attemptController.abort(new Error(`delivery request timed out after ${timeoutMs}ms`));
  }, timeoutMs);
  return {
    signal: attemptController.signal,
    cleanup: () => {
      clearTimeout(timer);
      shutdownSignal.removeEventListener("abort", onShutdown);
    },
  };
}

function defaultRetryAfterMs(error: unknown): number | undefined {
  return error instanceof TransportError ? error.retryAfterMs : undefined;
}

function waitForRetry(delayMs: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(abortReason(signal));
  return new Promise((resolve, reject) => {
    const timer = setTimeout(done, delayMs);
    signal?.addEventListener("abort", aborted, { once: true });
    function done() {
      signal?.removeEventListener("abort", aborted);
      resolve();
    }
    function aborted() {
      clearTimeout(timer);
      reject(abortReason(signal!));
    }
  });
}

function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) return Promise.reject(abortReason(signal));
  return new Promise((resolve, reject) => {
    signal.addEventListener("abort", aborted, { once: true });
    promise.then(
      (value) => { signal.removeEventListener("abort", aborted); resolve(value); },
      (error) => { signal.removeEventListener("abort", aborted); reject(error); },
    );
    function aborted() {
      reject(abortReason(signal));
    }
  });
}

function abortReason(signal: AbortSignal): unknown {
  return signal.reason ?? new Error("delivery aborted");
}
