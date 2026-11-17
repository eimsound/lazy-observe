export type DeliveryDropReason = "queue_full" | "retry_exhausted" | "shutdown_budget";

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
  send: (batch: T[], signal: AbortSignal) => Promise<void>;
  wait?: (delayMs: number, signal: AbortSignal) => Promise<void>;
  onDrop?: (drop: DeliveryDrop) => void;
}

export interface DeliveryCloseOptions {
  timeoutMs?: number;
}

export class DeliveryQueue<T> {
  private readonly items: T[] = [];
  private readonly controller = new AbortController();
  private readonly wait: (delayMs: number, signal: AbortSignal) => Promise<void>;
  private timer?: ReturnType<typeof setTimeout>;
  private running?: Promise<void>;
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
    this.wait = options.wait ?? waitForRetry;
  }

  enqueue(item: T): boolean {
    if (this.closed) {
      this.recordDrop({ reason: "shutdown_budget", count: 1 });
      return false;
    }
    if (this.resident >= this.options.capacity) {
      this.recordDrop({ reason: "queue_full", count: 1 });
      return false;
    }
    this.items.push(item);
    this.resident++;
    if (this.items.length >= Math.min(this.options.batchSize, this.options.capacity)) {
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

  private async flush(): Promise<void> {
    while (this.running || this.items.length > 0) {
      if (!this.running) this.startDrain();
      await this.running;
    }
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

  private async drain(): Promise<void> {
    while (this.items.length > 0) {
      if (this.controller.signal.aborted) {
        this.dropQueued(abortReason(this.controller.signal));
        return;
      }
      const batch = this.items.splice(0, Math.min(this.options.batchSize, this.options.capacity));
      await this.deliver(batch);
      this.resident -= batch.length;
    }
  }

  private async deliver(batch: T[]): Promise<void> {
    let lastError: unknown;
    for (let attempt = 0; attempt < this.options.maxAttempts; attempt++) {
      try {
        await abortable(this.options.send(batch, this.controller.signal), this.controller.signal);
        return;
      } catch (error) {
        lastError = error;
      }
      if (this.controller.signal.aborted) {
        this.lastError = lastError;
        this.recordDrop({ reason: "shutdown_budget", count: batch.length, error: lastError });
        return;
      }
      if (attempt + 1 < this.options.maxAttempts) {
        try {
          await abortable(
            this.wait(this.retryDelay(attempt), this.controller.signal),
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

  private retryDelay(retryIndex: number): number {
    const scaled = this.options.retryInitialMs * (2 ** retryIndex);
    return Math.min(Number.isFinite(scaled) ? scaled : this.options.retryMaxMs, this.options.retryMaxMs);
  }

  private dropQueued(error: unknown): void {
    const count = this.items.length;
    this.items.length = 0;
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
