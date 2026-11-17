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

export interface DeliverySendOptions {
  keepalive: boolean;
}

export interface DeliveryQueueOptions<T> {
  capacity: number;
  batchSize: number;
  flushIntervalMs: number;
  maxAttempts: number;
  retryInitialMs: number;
  retryMaxMs: number;
  send: (batch: T[], options: DeliverySendOptions) => Promise<void>;
  wait?: (delayMs: number) => Promise<void>;
  onDrop?: (drop: DeliveryDrop) => void;
}

export class DeliveryQueue<T> {
  private readonly items: T[] = [];
  private readonly wait: (delayMs: number) => Promise<void>;
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
      queueMicrotask(() => { void this.startDrain(false); });
    } else if (!this.timer) {
      this.timer = setTimeout(() => {
        this.timer = undefined;
        void this.startDrain(false);
      }, this.options.flushIntervalMs);
    }
    return true;
  }

  async flush(keepalive = false): Promise<void> {
    this.clearTimer();
    while (this.running || this.items.length > 0) {
      if (!this.running) this.startDrain(keepalive);
      await this.running;
    }
  }

  async close(): Promise<void> {
    if (this.closed) {
      await this.flush();
      return;
    }
    this.closed = true;
    await this.flush();
  }

  diagnostics(): DeliveryDiagnostics {
    return { pending: this.resident, dropped: this.dropped, lastError: this.lastError };
  }

  private startDrain(keepalive: boolean): Promise<void> {
    if (this.running) return this.running;
    const run = this.drain(keepalive).finally(() => {
      if (this.running === run) this.running = undefined;
      if (this.items.length > 0 && !this.timer) {
        queueMicrotask(() => { void this.startDrain(false); });
      }
    });
    this.running = run;
    return run;
  }

  private async drain(keepalive: boolean): Promise<void> {
    while (this.items.length > 0) {
      const batch = this.items.splice(0, Math.min(this.options.batchSize, this.options.capacity));
      await this.deliver(batch, keepalive);
      this.resident -= batch.length;
    }
  }

  private async deliver(batch: T[], keepalive: boolean): Promise<void> {
    let lastError: unknown;
    for (let attempt = 0; attempt < this.options.maxAttempts; attempt++) {
      try {
        await this.options.send(batch, { keepalive });
        return;
      } catch (error) {
        lastError = error;
      }
      if (attempt + 1 < this.options.maxAttempts) {
        try {
          await this.wait(this.retryDelay(attempt));
        } catch (error) {
          lastError = error;
          break;
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

function waitForRetry(delayMs: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, delayMs));
}
