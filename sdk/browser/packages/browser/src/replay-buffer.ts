export interface TimedReplayEvent { timestamp: number; [key: string]: unknown }

function sizeOf(value: unknown): number { return new TextEncoder().encode(JSON.stringify(value)).byteLength; }

export class ReplayRingBuffer<T extends TimedReplayEvent> {
  private entries: Array<{ event: T; bytes: number }> = [];
  private total = 0;
  constructor(private readonly durationMs = 60_000, private readonly maxBytes = 5_000_000) {}
  push(event: T): void {
    const bytes = sizeOf(event);
    this.entries.push({ event, bytes }); this.total += bytes;
    const cutoff = event.timestamp - this.durationMs;
    while (this.entries.length && (this.entries[0].event.timestamp < cutoff || this.total > this.maxBytes)) {
      this.total -= this.entries.shift()!.bytes;
    }
  }
  snapshot(): T[] { return this.entries.map(({ event }) => event); }
  drain(): T[] { const result = this.snapshot(); this.clear(); return result; }
  clear(): void { this.entries = []; this.total = 0; }
  get byteLength(): number { return this.total; }
  get length(): number { return this.entries.length; }
}
