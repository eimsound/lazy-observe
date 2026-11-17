/** One editable key/value row. Order is preserved by index. */
export interface Pair {
  key: string;
  value: string;
}

/** Convert an ordered pair list into a plain record (dropping empty keys). */
export function pairsToRecord(pairs: Pair[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const p of pairs) {
    const k = p.key.trim();
    if (k) out[k] = p.value;
  }
  return out;
}

/** Seed the editor from an existing record. */
export function recordToPairs(record: Record<string, string>): Pair[] {
  return Object.entries(record).map(([key, value]) => ({ key, value }));
}
