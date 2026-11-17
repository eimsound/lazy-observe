import dayjs from "../dayjs.ts";

/** Browser-safe scrape target after validating the management API response. */
export interface ScrapeTarget {
  id: string;
  name: string;
  url: string;
  interval: string;
  labels: Record<string, string>;
  enabled: boolean;
}

export type ScrapeTargetInput = Omit<ScrapeTarget, "id">;
export type TargetHealthStatus = "up" | "down" | "unknown";

export interface TargetHealth {
  status: TargetHealthStatus;
  /** RFC3339 timestamp; null means no scrape sample has been observed. */
  last_scrape_at: string | null;
  scrape_job: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(row: Record<string, unknown>, key: string, path: string): string {
  const value = row[key];
  if (typeof value !== "string" || value.trim() === "") {
    throw new TypeError(`${path}.${key} must be a non-empty string`);
  }
  return value;
}

function labels(value: unknown): Record<string, string> {
  if (!isRecord(value)) throw new TypeError("scrape target.labels must be an object");
  for (const [key, item] of Object.entries(value)) {
    if (typeof item !== "string") {
      throw new TypeError(`scrape target.labels.${key} must be a string`);
    }
  }
  return value as Record<string, string>;
}

export function normalizeScrapeTargetResponse(value: unknown): ScrapeTarget {
  if (!isRecord(value)) throw new TypeError("scrape target must be an object");
  if (typeof value.enabled !== "boolean") {
    throw new TypeError("scrape target.enabled must be a boolean");
  }
  return {
    id: requiredString(value, "id", "scrape target"),
    name: requiredString(value, "name", "scrape target"),
    url: requiredString(value, "url", "scrape target"),
    interval: requiredString(value, "interval", "scrape target"),
    labels: labels(value.labels),
    enabled: value.enabled,
  };
}

export function normalizeScrapeTargetListResponse(value: unknown): ScrapeTarget[] {
  if (!Array.isArray(value)) throw new TypeError("scrape target response must be an array");
  return value.map(normalizeScrapeTargetResponse);
}

export function normalizeTargetHealthResponse(value: unknown): TargetHealth {
  if (!isRecord(value)) throw new TypeError("target health must be an object");
  if (!("last_scrape_at" in value)) {
    throw new TypeError("target health.last_scrape_at must be RFC3339 or null");
  }
  if (value.status !== "up" && value.status !== "down" && value.status !== "unknown") {
    throw new TypeError("target health.status is invalid");
  }
  const lastScrapeAt = value.last_scrape_at;
  if (
    lastScrapeAt !== null &&
    (typeof lastScrapeAt !== "string"
      || lastScrapeAt.trim() === ""
      || !dayjs(lastScrapeAt).isValid())
  ) {
    throw new TypeError("target health.last_scrape_at must be RFC3339 or null");
  }
  return {
    status: value.status,
    last_scrape_at: lastScrapeAt,
    scrape_job: requiredString(value, "scrape_job", "target health"),
  };
}
