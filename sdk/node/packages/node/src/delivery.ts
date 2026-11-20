export const MAX_RETRY_AFTER_MS: number = 30_000;
export const DEFAULT_REQUEST_TIMEOUT_MS: number = 10_000;

export type DeliveryDropReason = "queue_full" | "retry_exhausted" | "shutdown_budget" | "server_rejected";

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

export function isServerRejection(error: unknown): boolean {
  if (!(error instanceof TransportError)) return false;
  const { status } = error;
  if (status === 408 || status === 429) return false;
  return status >= 400 && status < 500;
}

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
  batchOverheadBytes?: number;
  estimateItemBytes?: (item: T) => number;
  send: (batch: T[], signal: AbortSignal) => Promise<void>;
  wait?: (delayMs: number, signal: AbortSignal) => Promise<void>;
  onDrop?: (drop: DeliveryDrop) => void;
  isNonRetryable?: (error: unknown) => boolean;
  retryAfterMs?: (error: unknown) => number | undefined;
  random?: () => number;
}

export interface DeliveryCloseOptions {
  timeoutMs?: number;
}

export interface DeliveryEnqueueOptions {
  immediate?: boolean;
}

interface QueuedEntry<T> {
  item: T;
  bytes: number;
}

export class DeliveryQueue<T> {
  private readonly items: Array<QueuedEntry<T>> = [];
  private readonly controller = new AbortController();
  private readonly wait: (delayMs: number, signal: AbortSignal) => Promise<void>;
  private readonly isNonRetryable: (error: unknown) => boolean;
  private readonly retryAfterMs: (error: unknown) => number | undefined;
  private readonly random: () => number;
  private readonly maxRetryAfterMs: number;
  private readonly requestTimeoutMs: number;
  private readonly maxBatchBytes?: number;
  private readonly batchOverheadBytes: number;
  private readonly estimateItemBytes?: (item: T) => number;
  private timer?: ReturnType<typeof setTimeout>;
  private running?: Promise<void>;
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
    if (options.batchOverheadBytes !== undefined && (!Number.isFinite(options.batchOverheadBytes) || options.batchOverheadBytes < 0)) {
      throw new Error("delivery batchOverheadBytes cannot be negative");
    }
    this.wait = options.wait ?? waitForRetry;
    this.isNonRetryable = options.isNonRetryable ?? isServerRejection;
    this.retryAfterMs = options.retryAfterMs ?? defaultRetryAfterMs;
    this.random = options.random ?? Math.random;
    this.maxRetryAfterMs = options.maxRetryAfterMs ?? MAX_RETRY_AFTER_MS;
    this.requestTimeoutMs = options.requestTimeoutMs ?? 0;
    this.maxBatchBytes = options.maxBatchBytes;
    this.batchOverheadBytes = options.batchOverheadBytes ?? 0;
    this.estimateItemBytes = options.estimateItemBytes;
  }

  enqueue(item: T, options: DeliveryEnqueueOptions = {}): boolean {
    if (this.closed) {
      this.recordDrop({ reason: "shutdown_budget", count: 1 });
      return false;
    }
    if (this.resident >= this.options.capacity) {
      this.recordDrop({ reason: "queue_full", count: 1 });
      return false;
    }
    const bytes = this.estimateItemBytes ? Math.max(0, this.estimateItemBytes(item)) : 0;
    this.items.push({ item, bytes });
    this.queuedBytes += bytes;
    this.resident++;
    const effectiveBatchSize = Math.min(this.options.batchSize, this.options.capacity);
    const bytesFull = this.maxBatchBytes !== undefined
      && this.estimateItemBytes !== undefined
      && (this.batchOverheadBytes + this.queuedBytes + Math.max(0, this.items.length - 1)) >= this.maxBatchBytes;
    if (options.immediate) {
      this.clearTimer();
      void this.startDrain();
    } else if (this.items.length >= effectiveBatchSize || bytesFull) {
      this.clearTimer();
      queueMicrotask(() => { void this.startDrain(); });
    } else if (!this.timer) {
      this.timer = setTimeout(() => {
        this.timer = undefined;
        void this.startDrain();
      }, this.options.flushIntervalMs);
      this.timer.unref();
    }
    return true;
  }

  diagnostics(): DeliveryDiagnostics {
    return { pending: this.resident, dropped: this.dropped, lastError: this.lastError };
  }

  async flush(): Promise<void> {
    this.clearTimer();
    while (this.running || this.items.length > 0) {
      if (!this.running) this.startDrain();
      await this.running;
    }
  }

  async close(options: DeliveryCloseOptions = {}): Promise<void> {
    if (!this.closed) {
      this.closed = true;
      this.clearTimer();
    }
    const timeoutMs = options.timeoutMs ?? 10_000;
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
      throw new Error("delivery shutdown timeout must be positive");
    }
    const timeout = setTimeout(() => {
      this.controller.abort(new Error(`delivery shutdown timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    timeout.unref();
    try {
      await this.flush();
    } finally {
      clearTimeout(timeout);
    }
    if (this.lastError !== undefined) throw this.lastError;
  }

  private startDrain(): Promise<void> {
    if (this.running) return this.running;
    const run = this.drain().finally(() => {
      if (this.running === run) this.running = undefined;
      if (this.items.length > 0 && !this.timer) queueMicrotask(() => { void this.startDrain(); });
    });
    this.running = run;
    return run;
  }

  private takeBatch(): T[] {
    const limit = Math.min(this.options.batchSize, this.options.capacity, this.items.length);
    if (limit <= 0) return [];
    let count = limit;
    if (this.maxBatchBytes !== undefined && this.estimateItemBytes) {
      let batchBytes = this.batchOverheadBytes + this.items[0]!.bytes;
      count = 1;
      while (count < limit) {
        const nextBytes = this.items[count]!.bytes + 1;
        if (batchBytes + nextBytes > this.maxBatchBytes) break;
        batchBytes += nextBytes;
        count++;
      }
    }
    const entries = this.items.splice(0, count);
    for (const entry of entries) this.queuedBytes -= entry.bytes;
    return entries.map((entry) => entry.item);
  }

  private async drain(): Promise<void> {
    while (this.items.length > 0) {
      if (this.controller.signal.aborted) {
        this.dropQueued(abortReason(this.controller.signal));
        return;
      }
      const batch = this.takeBatch();
      await this.deliver(batch);
      this.resident -= batch.length;
    }
  }

  private async deliver(batch: T[]): Promise<void> {
    let lastError: unknown;
    for (let attempt = 0; attempt < this.options.maxAttempts; attempt++) {
      const attemptScope = createAttemptSignal(this.controller.signal, this.requestTimeoutMs);
      try {
        await abortable(this.options.send(batch, attemptScope.signal), attemptScope.signal);
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
          this.lastError = error;
          this.recordDrop({ reason: "shutdown_budget", count: batch.length, error });
          return;
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
  timer.unref();
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

function waitForRetry(delayMs: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.reject(abortReason(signal));
  return new Promise((resolve, reject) => {
    const timer = setTimeout(done, delayMs);
    timer.unref();
    signal.addEventListener("abort", aborted, { once: true });
    function done() {
      signal.removeEventListener("abort", aborted);
      resolve();
    }
    function aborted() {
      clearTimeout(timer);
      reject(abortReason(signal));
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
