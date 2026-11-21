export const VITAL_DEVICE_TYPES = ["desktop", "mobile", "tablet", "unknown"] as const;
export type VitalDeviceType = (typeof VITAL_DEVICE_TYPES)[number];

export const VITAL_BROWSER_FAMILIES = [
  "chrome",
  "edge",
  "safari",
  "firefox",
  "opera",
  "other",
] as const;
export type VitalBrowserFamily = (typeof VITAL_BROWSER_FAMILIES)[number];

export const VITAL_OS_FAMILIES = [
  "windows",
  "macos",
  "ios",
  "android",
  "linux",
  "chromeos",
  "other",
] as const;
export type VitalOSFamily = (typeof VITAL_OS_FAMILIES)[number];

export type VitalDimensionKey = "device" | "country" | "browser" | "os";

export interface VitalFilters {
  page?: string;
  device_type?: VitalDeviceType;
  browser_family?: VitalBrowserFamily;
  os_family?: VitalOSFamily;
  country?: string;
}

export interface VitalValuesDTO {
  lcp?: number;
  inp?: number;
  cls?: number;
  ttfb?: number;
  fcp?: number;
}

export interface VitalBreakdownRowDTO {
  value: string;
  samples: number;
  values: VitalValuesDTO;
}

export interface VitalsBreakdownResponseDTO {
  /** Requested window bounds as Unix milliseconds. */
  start: number;
  end: number;
  step_seconds: number;
  filters: VitalFilters;
  summary: VitalValuesDTO;
  total_samples: number;
  by_page: VitalBreakdownRowDTO[];
  by_device: VitalBreakdownRowDTO[];
  by_browser: VitalBreakdownRowDTO[];
  by_os: VitalBreakdownRowDTO[];
  by_country: VitalBreakdownRowDTO[];
  truncated: boolean;
}

export type VitalBreakdownRow = VitalBreakdownRowDTO;
export type VitalsBreakdownResult = VitalsBreakdownResponseDTO;

const VITAL_VALUE_KEYS = ["lcp", "inp", "cls", "ttfb", "fcp"] as const;

/** Validate the gateway Web Vitals breakdown DTO before rendering tables and filters. */
export function normalizeVitalsBreakdownResponse(value: unknown): VitalsBreakdownResult {
  const response = objectValue(value, "vitals breakdown response");
  const start = integerValue(response.start, "start");
  const end = integerValue(response.end, "end");
  const stepSeconds = integerValue(response.step_seconds, "step_seconds");
  if (end <= start) throw new TypeError("vitals breakdown response end must be after start");
  if (stepSeconds <= 0) throw new TypeError("vitals breakdown response step_seconds must be positive");
  const totalSamples = nonNegativeInteger(response.total_samples, "total_samples");
  if (typeof response.truncated !== "boolean") {
    throw new TypeError("vitals breakdown response truncated must be boolean");
  }

  return {
    start,
    end,
    step_seconds: stepSeconds,
    filters: normalizeFilters(response.filters),
    summary: normalizeVitalValues(response.summary, "summary"),
    total_samples: totalSamples,
    by_page: normalizeRows(response.by_page, "by_page"),
    by_device: normalizeRows(response.by_device, "by_device"),
    by_browser: normalizeRows(response.by_browser, "by_browser"),
    by_os: normalizeRows(response.by_os, "by_os"),
    by_country: normalizeRows(response.by_country, "by_country"),
    truncated: response.truncated,
  };
}

function normalizeFilters(value: unknown): VitalFilters {
  if (value === undefined || value === null) return {};
  const raw = objectValue(value, "filters");
  const filters: VitalFilters = {};
  if (typeof raw.page === "string" && raw.page.trim() !== "") filters.page = raw.page;
  if (typeof raw.device_type === "string" && (VITAL_DEVICE_TYPES as readonly string[]).includes(raw.device_type)) {
    filters.device_type = raw.device_type as VitalDeviceType;
  }
  if (
    typeof raw.browser_family === "string" &&
    (VITAL_BROWSER_FAMILIES as readonly string[]).includes(raw.browser_family)
  ) {
    filters.browser_family = raw.browser_family as VitalBrowserFamily;
  }
  if (typeof raw.os_family === "string" && (VITAL_OS_FAMILIES as readonly string[]).includes(raw.os_family)) {
    filters.os_family = raw.os_family as VitalOSFamily;
  }
  if (typeof raw.country === "string" && raw.country.trim() !== "") filters.country = raw.country;
  return filters;
}

function normalizeRows(value: unknown, path: string): VitalBreakdownRow[] {
  if (!Array.isArray(value)) {
    throw new TypeError(`vitals breakdown response ${path} must be an array`);
  }
  return value.map((item, index) => normalizeRow(item, `${path}[${index}]`));
}

function normalizeRow(value: unknown, path: string): VitalBreakdownRow {
  const row = objectValue(value, path);
  return {
    value: nonEmptyString(row.value, `${path}.value`),
    samples: nonNegativeInteger(row.samples, `${path}.samples`),
    values: normalizeVitalValues(row.values, `${path}.values`),
  };
}

function normalizeVitalValues(value: unknown, path: string): VitalValuesDTO {
  const raw = objectValue(value, path);
  const out: VitalValuesDTO = {};
  for (const key of VITAL_VALUE_KEYS) {
    const entry = raw[key];
    if (entry === undefined || entry === null) continue;
    if (typeof entry !== "number" || !Number.isFinite(entry) || entry < 0) {
      throw new TypeError(`vitals breakdown response ${path}.${key} must be a non-negative finite number`);
    }
    out[key] = entry;
  }
  return out;
}

function objectValue(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TypeError(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}

function nonEmptyString(value: unknown, path: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new TypeError(`vitals breakdown response ${path} must be a non-empty string`);
  }
  return value;
}

function integerValue(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) {
    throw new TypeError(`vitals breakdown response ${path} must be a safe integer`);
  }
  return value;
}

function nonNegativeInteger(value: unknown, path: string): number {
  const n = integerValue(value, path);
  if (n < 0) {
    throw new TypeError(`vitals breakdown response ${path} must be non-negative`);
  }
  return n;
}
