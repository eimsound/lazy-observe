export interface TimedReplayEvent { timestamp: number; type?: number; [key: string]: unknown }

const RRWEB_FULL_SNAPSHOT = 2;
const RRWEB_META = 4;
const MAX_CHECKOUT_GENERATIONS = 2;

export const MAX_SIZE_ESTIMATE_DEPTH = 64;
const WEAKSET_PROMOTION_DEPTH = 16;
const OMITTED = -1;
const hasOwn = Object.prototype.hasOwnProperty;
const SPECIAL_JSON_RE = /[\x00-\x1f"\\\u0080-\uffff]/;
const SIX_BYTE_JSON_RE = /[\x00-\x07\x0b\x0e-\x1f\ud800-\udfff]/;
const ancestorStack: Array<object | undefined> = new Array(MAX_SIZE_ESTIMATE_DEPTH);
let activeInUse = false;
let maxDepthUsed = 0;

function estimateStringBytes(str: string): number {
  if (!SPECIAL_JSON_RE.test(str)) return str.length + 2;
  if (!SIX_BYTE_JSON_RE.test(str)) return str.length * 3 + 2;
  let bytes = 2;
  for (let i = 0; i < str.length; i++) {
    const c = str.charCodeAt(i);
    if (c < 0x80) {
      if (
        c === 0x22 ||
        c === 0x5c ||
        c === 0x08 ||
        c === 0x09 ||
        c === 0x0a ||
        c === 0x0c ||
        c === 0x0d
      ) {
        bytes += 2;
      } else if (c < 0x20) {
        bytes += 6;
      } else {
        bytes += 1;
      }
    } else if (c < 0x800) {
      bytes += 2;
    } else if (c >= 0xd800 && c <= 0xdfff) {
      if (c <= 0xdbff && i + 1 < str.length) {
        const next = str.charCodeAt(i + 1);
        if (next >= 0xdc00 && next <= 0xdfff) {
          i++;
          bytes += 4;
          continue;
        }
      }
      bytes += 6;
    } else {
      bytes += 3;
    }
  }
  return bytes;
}

function estimateNumberBytes(n: number): number {
  if (!Number.isFinite(n)) return 4;
  if (n >= 0 && n < 10_000 && Number.isInteger(n)) {
    return n < 10 ? 1 : n < 100 ? 2 : n < 1_000 ? 3 : 4;
  }
  return 24;
}

function walkFast(
  value: unknown,
  depth: number,
  seen: WeakSet<object> | undefined,
  key: string | number,
): number {
  if (value === null) return 4;
  const t = typeof value;
  if (t === "string") return estimateStringBytes(value as string);
  if (t === "number") return estimateNumberBytes(value as number);
  if (t === "boolean") return value ? 4 : 5;
  if (t !== "object") {
    return t === "bigint" ? (value as bigint).toString().length : OMITTED;
  }

  if (depth >= MAX_SIZE_ESTIMATE_DEPTH) return 2;
  const obj = value as object;

  let useWS = false;
  if (depth < WEAKSET_PROMOTION_DEPTH) {
    for (let i = 0; i < depth; i++) {
      if (ancestorStack[i] === obj) return 0;
    }
    ancestorStack[depth] = obj;
    if (depth >= maxDepthUsed) maxDepthUsed = depth + 1;
  } else {
    if (seen === undefined) {
      seen = new WeakSet<object>();
      for (let i = 0; i < WEAKSET_PROMOTION_DEPTH; i++) {
        const ancestor = ancestorStack[i];
        if (ancestor !== undefined) seen.add(ancestor);
      }
    }
    if (seen.has(obj)) return 0;
    seen.add(obj);
    useWS = true;
  }

  let res: number | undefined;
  const toJSON = (obj as { toJSON?: unknown }).toJSON;
  if (typeof toJSON === "function") {
    res = walkFast(
      (toJSON as (k: string) => unknown).call(obj, typeof key === "string" ? key : String(key)),
      depth + 1,
      seen,
      "",
    );
  } else if (Array.isArray(obj)) {
    const len = obj.length;
    res = len > 1 ? len + 1 : 2;
    for (let i = 0; i < len; i++) {
      const elemBytes = walkFast(obj[i], depth + 1, seen, i);
      res += elemBytes === OMITTED ? 4 : elemBytes;
    }
  } else {
    const ctor = (obj as { constructor?: unknown }).constructor;
    const isPlain = ctor === Object;
    if (!isPlain && ctor !== undefined) {
      if (obj instanceof String) res = estimateStringBytes(obj.valueOf());
      else if (obj instanceof Number) res = estimateNumberBytes(obj.valueOf());
      else if (obj instanceof Boolean) res = obj.valueOf() ? 4 : 5;
    }
    if (res === undefined) {
      const record = obj as Record<string, unknown>;
      res = 2;
      let propCount = 0;
      for (const k in record) {
        if (!isPlain && !hasOwn.call(record, k)) continue;
        const valBytes = walkFast(record[k], depth + 1, seen, k);
        if (valBytes === OMITTED) continue;
        if (propCount++ > 0) res += 1;
        res += estimateStringBytes(k) + 1 + valBytes;
      }
    }
  }

  if (useWS) seen!.delete(obj);
  return res;
}

function safeGetToJSON(obj: object): ((key: string) => unknown) | undefined {
  try {
    const fn = (obj as { toJSON?: unknown }).toJSON;
    return typeof fn === "function" ? (fn as (key: string) => unknown) : undefined;
  } catch {
    return undefined;
  }
}

function safeGetConstructor(obj: object): unknown {
  try {
    return (obj as { constructor?: unknown }).constructor;
  } catch {
    return Object;
  }
}

function safeGetProp(obj: Record<string, unknown>, key: string): unknown {
  try {
    return obj[key];
  } catch {
    return undefined;
  }
}

function walkSafe(value: unknown, depth: number, seen: WeakSet<object>, key: string | number): number {
  if (value === null) return 4;
  const t = typeof value;
  if (t === "string") return estimateStringBytes(value as string);
  if (t === "number") return estimateNumberBytes(value as number);
  if (t === "boolean") return value ? 4 : 5;
  if (t !== "object") {
    return t === "bigint" ? (value as bigint).toString().length : OMITTED;
  }

  if (depth >= MAX_SIZE_ESTIMATE_DEPTH) return 2;
  const obj = value as object;
  if (seen.has(obj)) return 0;

  seen.add(obj);
  try {
    const toJSON = safeGetToJSON(obj);
    if (toJSON !== undefined) {
      let converted: unknown;
      try {
        converted = toJSON.call(obj, typeof key === "string" ? key : String(key));
      } catch {
        return 0;
      }
      return walkSafe(converted, depth + 1, seen, "");
    }

    if (Array.isArray(obj)) {
      const len = obj.length;
      if (len === 0) return 2;
      let bytes = 2 + (len - 1);
      for (let i = 0; i < len; i++) {
        const elemBytes = walkSafe(obj[i], depth + 1, seen, i);
        bytes += elemBytes === OMITTED ? 4 : elemBytes;
      }
      return bytes;
    }

    const ctor = safeGetConstructor(obj);
    if (ctor !== Object && ctor !== undefined) {
      if (obj instanceof String) return estimateStringBytes(obj.valueOf());
      if (obj instanceof Number) return estimateNumberBytes(obj.valueOf());
      if (obj instanceof Boolean) return obj.valueOf() ? 4 : 5;
    }

    const record = obj as Record<string, unknown>;
    let bytes = 2;
    let propCount = 0;
    for (const k in record) {
      if (!hasOwn.call(record, k)) continue;
      const valBytes = walkSafe(safeGetProp(record, k), depth + 1, seen, k);
      if (valBytes === OMITTED) continue;
      if (propCount++ > 0) bytes += 1;
      bytes += estimateStringBytes(k) + 1 + valBytes;
    }
    return bytes;
  } finally {
    seen.delete(obj);
  }
}

/**
 * Zero-allocation recursive UTF-8 / CJK upper-bound JSON byte estimator for Replay ring
 * budgeting. Uses exact byte counts for unescaped plain ASCII strings and small integers,
 * a 3x upper bound for 2–3 byte escapes and non-ASCII strings, an exact fallback scan for
 * 6-byte control characters and UTF-16 surrogates, a depth cap, WeakSet cycle promotion,
 * and .toJSON() support.
 */
export function sizeOf(value: unknown): number {
  if (activeInUse) {
    const n = walkSafe(value, 0, new WeakSet<object>(), "");
    return n < 0 ? 0 : n;
  }
  activeInUse = true;
  maxDepthUsed = 0;
  try {
    const n = walkFast(value, 0, undefined, "");
    return n < 0 ? 0 : n;
  } catch {
    const n = walkSafe(value, 0, new WeakSet<object>(), "");
    return n < 0 ? 0 : n;
  } finally {
    for (let i = 0; i < maxDepthUsed; i++) ancestorStack[i] = undefined;
    activeInUse = false;
  }
}

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
