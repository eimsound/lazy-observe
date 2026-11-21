import { ApiError } from "./client";
import type { TimeSeries } from "./types";
import type {
  PerfWindow,
  VitalKey,
} from "./performance";
import { VITALS } from "./performance";
import type {
  EndpointPoint,
  EndpointStat,
  EndpointStatsResult,
} from "./performance-contract";
import type {
  VitalBreakdownRow,
  VitalBrowserFamily,
  VitalDeviceType,
  VitalFilters,
  VitalOSFamily,
  VitalsBreakdownResult,
  VitalValuesDTO,
} from "./performance-vitals-contract";
import { makeRng } from "./mock-random";

/**
 * Offline performance data (VITE_MOCK=1). Deterministic so ratings/tables are
 * stable across reloads. Per-page vital values are spread across the good /
 * needs-improvement / poor bands on purpose, so the threshold coloring is
 * visibly exercised in the UI.
 */

const PERF_PAGES = [
  "/checkout",
  "/orders/:id",
  "/settings/profile",
  "/product/:sku",
];

/** Mock mode exposes the same discovered-page domain contract as real mode. */
export function mockVitalPages(_window: PerfWindow, signal?: AbortSignal): Promise<string[]> {
  return resolveAfter([...PERF_PAGES], 180, signal);
}

const ENDPOINTS = [
  { service: "orders", route: "/api/orders", method: "GET" },
  { service: "orders", route: "/api/orders/:id", method: "GET" },
  { service: "checkout", route: "/api/checkout", method: "POST" },
  { service: "identity", route: "/api/auth/login", method: "POST" },
  { service: "catalog", route: "/api/products/search", method: "GET" },
  { service: "identity", route: "/api/users/:id", method: "PATCH" },
  { service: "checkout", route: "/api/cart/items", method: "POST" },
  { service: "observability", route: "/api/metrics/query", method: "GET" },
];

/** Deterministic value for (page, vital), landing in a chosen rating band. */
function vitalValue(key: VitalKey, pageIndex: number): number {
  const m = VITALS.find((v) => v.key === key)!;
  const rng = makeRng(`vital|${key}|${pageIndex}`);
  const band = (pageIndex + VITALS.findIndex((v) => v.key === key)) % 3;
  if (band === 0) return round(m.good * (0.5 + rng.next() * 0.45), m.decimals); // good
  if (band === 1) return round(m.good + rng.next() * (m.poor - m.good), m.decimals); // ni
  return round(m.poor + rng.next() * m.poor * 0.6, m.decimals); // poor
}

function round(v: number, decimals: number): number {
  const f = 10 ** decimals;
  return Math.round(v * f) / f;
}

function buildPoints(window: PerfWindow, center: number, jitter: number, seed: string): TimeSeries["points"] {
  const rng = makeRng(seed);
  const points: TimeSeries["points"] = [];
  for (let t = window.start; t <= window.end; t += window.step) {
    const wave = Math.sin((t - window.start) / (window.step * 12));
    const v = Math.max(0, center + wave * jitter + (rng.next() - 0.5) * jitter);
    points.push([t * 1000, v]);
  }
  return points;
}

interface MockVitalCohort {
  page: string;
  device_type: VitalDeviceType;
  browser_family: VitalBrowserFamily;
  os_family: VitalOSFamily;
  country: string;
  samples: number;
  factor: number;
}

const MOCK_VITAL_COHORTS: MockVitalCohort[] = [
  { page: "/checkout", device_type: "desktop", browser_family: "chrome", os_family: "macos", country: "US", samples: 180, factor: 0.88 },
  { page: "/checkout", device_type: "mobile", browser_family: "safari", os_family: "ios", country: "US", samples: 140, factor: 1.24 },
  { page: "/checkout", device_type: "mobile", browser_family: "chrome", os_family: "android", country: "CN", samples: 120, factor: 1.32 },
  { page: "/orders/:id", device_type: "desktop", browser_family: "edge", os_family: "windows", country: "DE", samples: 95, factor: 0.96 },
  { page: "/orders/:id", device_type: "tablet", browser_family: "safari", os_family: "ios", country: "JP", samples: 65, factor: 1.08 },
  { page: "/settings/profile", device_type: "desktop", browser_family: "firefox", os_family: "linux", country: "GB", samples: 75, factor: 0.82 },
  { page: "/product/:sku", device_type: "mobile", browser_family: "chrome", os_family: "android", country: "CN", samples: 110, factor: 1.18 },
  { page: "/product/:sku", device_type: "desktop", browser_family: "chrome", os_family: "windows", country: "US", samples: 130, factor: 0.92 },
  { page: "/settings/profile", device_type: "unknown", browser_family: "other", os_family: "other", country: "unknown", samples: 25, factor: 1.05 },
];

function matchMockCohort(cohort: MockVitalCohort, filters?: VitalFilters): boolean {
  if (!filters) return true;
  if (filters.page && cohort.page !== filters.page) return false;
  if (filters.device_type && cohort.device_type !== filters.device_type) return false;
  if (filters.browser_family && cohort.browser_family !== filters.browser_family) return false;
  if (filters.os_family && cohort.os_family !== filters.os_family) return false;
  if (filters.country && cohort.country !== filters.country) return false;
  return true;
}

function cohortVitalValue(key: VitalKey, cohort: MockVitalCohort): number {
  const m = VITALS.find((v) => v.key === key)!;
  const pageIndex = Math.max(0, PERF_PAGES.indexOf(cohort.page));
  return round(vitalValue(key, pageIndex) * cohort.factor, m.decimals);
}

export function mockVitalSeries(
  key: VitalKey,
  page: string | undefined,
  window: PerfWindow,
  filters?: VitalFilters,
): Promise<TimeSeries[]> {
  const effectiveFilters: VitalFilters = { ...filters, ...(page ? { page } : {}) };
  const matched = MOCK_VITAL_COHORTS.filter((c) => matchMockCohort(c, effectiveFilters));
  if (matched.length === 0) {
    return new Promise((resolve) => setTimeout(() => resolve([]), 160));
  }
  let weightedSum = 0;
  let totalWeight = 0;
  for (const cohort of matched) {
    weightedSum += cohortVitalValue(key, cohort) * cohort.samples;
    totalWeight += cohort.samples;
  }
  const center = totalWeight > 0 ? weightedSum / totalWeight : 0;
  const m = VITALS.find((v) => v.key === key)!;
  const jitter = m.unit === "ms" ? center * 0.18 : center * 0.3;
  const seedKey = [
    key,
    effectiveFilters.page ?? "all",
    effectiveFilters.device_type ?? "all",
    effectiveFilters.browser_family ?? "all",
    effectiveFilters.os_family ?? "all",
    effectiveFilters.country ?? "all",
    window.end,
  ].join("|");
  const labels: Record<string, string> = { __name__: m.metric };
  if (effectiveFilters.page) labels.page = effectiveFilters.page;
  if (effectiveFilters.device_type) labels.device_type = effectiveFilters.device_type;
  if (effectiveFilters.browser_family) labels.browser_family = effectiveFilters.browser_family;
  if (effectiveFilters.os_family) labels.os_family = effectiveFilters.os_family;
  if (effectiveFilters.country) labels.country = effectiveFilters.country;
  const series: TimeSeries = {
    name: page ? `${m.metric}{page="${page}"}` : m.metric,
    labels,
    points: buildPoints(window, center, jitter, seedKey),
  };
  return new Promise((resolve) => setTimeout(() => resolve([series]), 260));
}

function buildMockBreakdownRows(
  cohorts: MockVitalCohort[],
  keyOf: (cohort: MockVitalCohort) => string,
): VitalBreakdownRow[] {
  const groups = new Map<string, { samples: number; sums: Record<VitalKey, number> }>();
  for (const cohort of cohorts) {
    const groupKey = keyOf(cohort);
    const entry = groups.get(groupKey) ?? {
      samples: 0,
      sums: { lcp: 0, inp: 0, cls: 0, ttfb: 0, fcp: 0 },
    };
    entry.samples += cohort.samples;
    for (const v of VITALS) {
      entry.sums[v.key] += cohortVitalValue(v.key, cohort) * cohort.samples;
    }
    groups.set(groupKey, entry);
  }
  return [...groups.entries()]
    .map(([value, entry]) => {
      const values: VitalValuesDTO = {};
      for (const v of VITALS) {
        values[v.key] = round(entry.sums[v.key] / entry.samples, v.decimals);
      }
      return { value, samples: entry.samples * VITALS.length, values };
    })
    .sort((a, b) => (b.samples !== a.samples ? b.samples - a.samples : a.value.localeCompare(b.value)));
}

export function mockVitalsBreakdown(
  window: PerfWindow,
  filters?: VitalFilters,
  signal?: AbortSignal,
): Promise<VitalsBreakdownResult> {
  const matched = MOCK_VITAL_COHORTS.filter((c) => matchMockCohort(c, filters));
  const summary: VitalValuesDTO = {};
  let totalWeight = 0;
  for (const cohort of matched) totalWeight += cohort.samples;
  if (totalWeight > 0) {
    for (const v of VITALS) {
      let sum = 0;
      for (const cohort of matched) sum += cohortVitalValue(v.key, cohort) * cohort.samples;
      summary[v.key] = round(sum / totalWeight, v.decimals);
    }
  }
  const result: VitalsBreakdownResult = {
    start: window.start * 1000,
    end: window.end * 1000,
    step_seconds: window.step,
    filters: filters ?? {},
    summary,
    total_samples: totalWeight * VITALS.length,
    by_page: buildMockBreakdownRows(matched, (c) => c.page),
    by_device: buildMockBreakdownRows(matched, (c) => c.device_type),
    by_browser: buildMockBreakdownRows(matched, (c) => c.browser_family),
    by_os: buildMockBreakdownRows(matched, (c) => c.os_family),
    by_country: buildMockBreakdownRows(matched, (c) => c.country),
    truncated: false,
  };
  return resolveAfter(result, 200, signal);
}

function resolveAfter<T>(value: T, delay: number, signal?: AbortSignal): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve(value);
    }, delay);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason ?? new DOMException("The operation was aborted", "AbortError"));
    };
    if (signal?.aborted) onAbort();
    else signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export function mockEndpointStats(window: PerfWindow): Promise<EndpointStatsResult> {
  const mockControl = globalThis as { __loMockFailNextEndpoints?: boolean };
  if (mockControl.__loMockFailNextEndpoints) {
    mockControl.__loMockFailNextEndpoints = false;
    return new Promise((_, reject) =>
      setTimeout(() => reject(new ApiError("mock endpoint refresh failed", 500)), 120),
    );
  }
  const rows: EndpointStat[] = ENDPOINTS.map((e) => {
    const rng = makeRng(`endpoint|${e.route}|${e.method}`);
    const p50 = rng.int(8, 120);
    const p95 = p50 + rng.int(30, 400);
    const p99 = p95 + rng.int(40, 700);
    const throughput = round(rng.next() * 240 + 5, 1);
    const errorRate = round(rng.next() * rng.next() * 0.09, 4);
    const series: EndpointPoint[] = [];
    for (let timestamp = window.start; timestamp <= window.end; timestamp += window.step) {
      const phase = (timestamp - window.start) / Math.max(window.step, 1);
      const latencyFactor = 0.82 + 0.18 * (1 + Math.sin(phase / 3));
      const loadFactor = 0.75 + 0.25 * (1 + Math.cos(phase / 4));
      series.push({
        time: timestamp * 1000,
        throughput: round(throughput * loadFactor, 3),
        p50: round(p50 * latencyFactor, 2),
        p95: round(p95 * latencyFactor, 2),
        p99: round(p99 * latencyFactor, 2),
        error_rate: round(Math.min(1, errorRate * latencyFactor), 5),
      });
    }
    return {
      service: e.service,
      route: e.route,
      method: e.method,
      throughput,
      p50,
      p95,
      p99,
      error_rate: errorRate,
      series,
    };
  });
  const result: EndpointStatsResult = {
    start: window.start * 1000,
    end: window.end * 1000,
    step_seconds: window.step,
    endpoints: rows,
    truncated: false,
  };
  return new Promise((resolve) => setTimeout(() => resolve(result), 240));
}
