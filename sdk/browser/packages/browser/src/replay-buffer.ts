export interface TimedReplayEvent { timestamp: number; type?: number; [key: string]: unknown }

const RRWEB_FULL_SNAPSHOT = 2;
const RRWEB_META = 4;
const MAX_CHECKOUT_GENERATIONS = 2;

function sizeOf(value: unknown): number { return new TextEncoder().encode(JSON.stringify(value)).byteLength; }

interface BufferEntry<T> {
  event: T;
  bytes: number;
}

interface Generation<T> {
  entries: BufferEntry<T>[];
  bytes: number;
  hasFullSnapshot: boolean;
}

export class ReplayRingBuffer<T extends TimedReplayEvent> {
  private generations: Generation<T>[] = [];
  private total = 0;
  private count = 0;
  private trackingSnapshots = false;

  constructor(private readonly durationMs = 60_000, private readonly maxBytes = 5_000_000) {}

  push(event: T, isCheckout = false): void {
    const bytes = sizeOf(event);
    const isMeta = event.type === RRWEB_META;
    const isFullSnapshot = event.type === RRWEB_FULL_SNAPSHOT;

    if (isCheckout || isFullSnapshot) {
      this.trackingSnapshots = true;
    }

    let current = this.generations[this.generations.length - 1];
    const shouldStartNewGeneration = !current || (
      this.trackingSnapshots && (
        (isCheckout && (isMeta || current.hasFullSnapshot)) ||
        ((isMeta || isFullSnapshot) && current.hasFullSnapshot)
      )
    );
    if (shouldStartNewGeneration) {
      current = { entries: [], bytes: 0, hasFullSnapshot: false };
      this.generations.push(current);
    }

    current.entries.push({ event, bytes });
    current.bytes += bytes;
    if (isFullSnapshot) current.hasFullSnapshot = true;
    this.total += bytes;
    this.count++;

    this.evict(event.timestamp);
  }

  private evict(latestTimestamp: number): void {
    const cutoff = latestTimestamp - this.durationMs;

    if (this.trackingSnapshots) {
      // Keep at most the latest two checkout generations once a newer complete generation exists,
      // so snapshot() always starts with Meta + FullSnapshot even after 60s+ on a page.
      while (this.generations.length > MAX_CHECKOUT_GENERATIONS && this.generations[1].hasFullSnapshot) {
        this.dropOldestGeneration();
      }
      while (this.generations.length > 1 && this.generations[1].hasFullSnapshot) {
        const oldest = this.generations[0];
        const oldestEnd = oldest.entries[oldest.entries.length - 1]?.event.timestamp ?? -Infinity;
        if (oldestEnd < cutoff || this.total > this.maxBytes) {
          this.dropOldestGeneration();
        } else {
          break;
        }
      }
      // Enforce maxBytes even within a single remaining generation.
      while (this.total > this.maxBytes && this.generations.length > 0) {
        if (this.generations.length > 1) {
          this.dropOldestGeneration();
          continue;
        }
        const only = this.generations[0];
        if (!only.entries.length) {
          this.generations.shift();
          break;
        }
        const removed = only.entries.shift()!;
        only.bytes -= removed.bytes;
        this.total -= removed.bytes;
        this.count--;
        if (!only.entries.length) this.generations.shift();
      }
      return;
    }

    // Generic timed-event ring mode (no checkout/FullSnapshot events observed).
    const only = this.generations[0];
    if (!only) return;
    while (only.entries.length && (only.entries[0].event.timestamp < cutoff || this.total > this.maxBytes)) {
      const removed = only.entries.shift()!;
      only.bytes -= removed.bytes;
      this.total -= removed.bytes;
      this.count--;
    }
    if (!only.entries.length) this.generations = [];
  }

  private dropOldestGeneration(): void {
    const removed = this.generations.shift();
    if (!removed) return;
    this.total -= removed.bytes;
    this.count -= removed.entries.length;
  }

  snapshot(): T[] {
    const result: T[] = [];
    for (const generation of this.generations) {
      for (const { event } of generation.entries) result.push(event);
    }
    return result;
  }

  drain(): T[] { const result = this.snapshot(); this.clear(); return result; }

  clear(): void {
    this.generations = [];
    this.total = 0;
    this.count = 0;
    this.trackingSnapshots = false;
  }

  get byteLength(): number { return this.total; }
  get length(): number { return this.count; }
}
