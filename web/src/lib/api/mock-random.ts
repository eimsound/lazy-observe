import { hashString } from "../hash.ts";

export { hashString };
/**
 * Deterministic PRNG helpers shared by the offline mock generators. Mock data
 * must be reproducible: the same query / traceID always yields the same result
 * so that cross-page navigation (a log's trace_id -> its waterfall) resolves and
 * manual verification of a "known" trace is stable across reloads.
 */


/** mulberry32: a tiny, fast, seedable PRNG returning floats in [0, 1). */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return function () {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** A seeded random-number source with convenience helpers. */
export interface Rng {
  next(): number;
  int(minInclusive: number, maxInclusive: number): number;
  pick<T>(items: readonly T[]): T;
  bool(probability: number): boolean;
}

export function makeRng(seed: number | string): Rng {
  const rand = mulberry32(typeof seed === "string" ? hashString(seed) : seed);
  return {
    next: rand,
    int: (min, max) => min + Math.floor(rand() * (max - min + 1)),
    pick: (items) => items[Math.floor(rand() * items.length)],
    bool: (p) => rand() < p,
  };
}

/** Deterministic lowercase hex string of `len` chars, seeded by `seed`. */
export function hexId(seed: number | string, len: number): string {
  const rand = mulberry32(typeof seed === "string" ? hashString(seed) : seed);
  const chars = "0123456789abcdef";
  let out = "";
  for (let i = 0; i < len; i++) out += chars[Math.floor(rand() * 16)];
  return out;
}
