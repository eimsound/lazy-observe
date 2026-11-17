/** Wire contract for the node_exporter-derived host inventory. */
export interface Host {
  /** node_exporter scrape target, e.g. "10.0.0.11:9100". Stable id. */
  instance: string;
  hostname: string;
  /** `null` means no `up` sample has been observed for this instance. */
  up: boolean | null;
  cpu_cores: number | null;
  /** 1-minute load average. */
  load1: number | null;
  mem_used_percent: number | null;
  /** Highest filesystem usage across mounts. */
  disk_used_percent_max: number | null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(row: Record<string, unknown>, key: string): string {
  const value = row[key];
  if (typeof value !== "string" || value.length === 0) {
    throw new TypeError(`host.${key} must be a non-empty string`);
  }
  return value;
}

function nullableBoolean(row: Record<string, unknown>, key: string): boolean | null {
  const value = row[key];
  if (value == null) return null;
  if (typeof value !== "boolean") throw new TypeError(`host.${key} must be boolean or null`);
  return value;
}

function nullableNumber(row: Record<string, unknown>, key: string): number | null {
  const value = row[key];
  if (value == null) return null;
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new TypeError(`host.${key} must be a finite number or null`);
  }
  return value;
}

/** Validate untrusted HTTP JSON and normalize omitted optional samples to null. */
export function normalizeHostsResponse(value: unknown): Host[] {
  if (!Array.isArray(value)) throw new TypeError("host response must be an array");
  return value.map((item) => {
    if (!isRecord(item)) throw new TypeError("host row must be an object");
    return {
      instance: requiredString(item, "instance"),
      hostname: requiredString(item, "hostname"),
      up: nullableBoolean(item, "up"),
      cpu_cores: nullableNumber(item, "cpu_cores"),
      load1: nullableNumber(item, "load1"),
      mem_used_percent: nullableNumber(item, "mem_used_percent"),
      disk_used_percent_max: nullableNumber(item, "disk_used_percent_max"),
    };
  });
}

/** Rating buckets for a 0..100 usage percentage — drive semantic vital colors. */
export type UsageRating = "good" | "ni" | "poor";

export function usageRating(percent: number): UsageRating {
  if (percent >= 90) return "poor";
  if (percent >= 70) return "ni";
  return "good";
}

/** Load pressure relative to the core count (load per core). */
export function loadRating(
  load1: number | null,
  cores: number | null,
): UsageRating | null {
  if (load1 == null) return null;
  const perCore = cores != null && cores > 0 ? load1 / cores : load1;
  if (perCore >= 1) return "poor";
  if (perCore >= 0.7) return "ni";
  return "good";
}
