import { queryRange } from "./metrics";
import { apiGet, IS_MOCK } from "./client";
import type { TimeSeries } from "./types";
import {
  normalizeEndpointStatsResponse,
  type EndpointStatsResult,
} from "./performance-contract";
import { requestVitalPages } from "./performance-pages-contract";
import {
  normalizeVitalsBreakdownResponse,
  type VitalFilters,
  type VitalsBreakdownResult,
} from "./performance-vitals-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockPerformance = () => import("./mock-performance");

/**
 * Performance API. Two surfaces backed by their native storage:
 *  - Web Vitals: `web_vitals_{lcp,cls,inp,ttfb,fcp}` time series (optionally
 *    filtered by page), rated good / needs-improvement / poor against the
 *    standard Core Web Vitals thresholds.
 *  - Backend endpoints: p50/p95/p99, throughput, error rate and bucketed
 *    trends derived by the gateway from VictoriaTraces server spans.
 * VITE_MOCK serves deterministic offline data.
 */

export type VitalKey = "lcp" | "inp" | "cls" | "ttfb" | "fcp";
export type VitalRating = "good" | "ni" | "poor";

export interface VitalMeta {
  key: VitalKey;
  /** Prometheus metric name (real mode). */
  metric: string;
  /** "ms" for timings, "" for the unitless CLS. */
  unit: "ms" | "";
  /** Upper bound of the "good" band. */
  good: number;
  /** Lower bound of the "poor" band (values above are poor). */
  poor: number;
  /** Decimal places for display. */
  decimals: number;
}

/** Core Web Vitals thresholds (https://web.dev/articles/vitals). */
export const VITALS: VitalMeta[] = [
  { key: "lcp", metric: "web_vitals_lcp", unit: "ms", good: 2500, poor: 4000, decimals: 0 },
  { key: "inp", metric: "web_vitals_inp", unit: "ms", good: 200, poor: 500, decimals: 0 },
  { key: "cls", metric: "web_vitals_cls", unit: "", good: 0.1, poor: 0.25, decimals: 2 },
  { key: "ttfb", metric: "web_vitals_ttfb", unit: "ms", good: 800, poor: 1800, decimals: 0 },
  { key: "fcp", metric: "web_vitals_fcp", unit: "ms", good: 1800, poor: 3000, decimals: 0 },
];

export function vitalByKey(key: VitalKey): VitalMeta {
  return VITALS.find((v) => v.key === key) ?? VITALS[0];
}

/** Rate a vital value against its thresholds. */
export function rateVital(key: VitalKey, value: number): VitalRating {
  const m = vitalByKey(key);
  if (value <= m.good) return "good";
  if (value <= m.poor) return "ni";
  return "poor";
}

export interface PerfWindow {
  /** Unix seconds. */
  start: number;
  /** Unix seconds. */
  end: number;
  /** Step in seconds. */
  step: number;
}

/** Latest finite value of a single series. */
function seriesLastValue(s: TimeSeries): number | null {
  const pts = s.points;
  for (let i = pts.length - 1; i >= 0; i--) {
    const v = pts[i][1];
    if (v != null && Number.isFinite(v)) return v;
  }
  return null;
}

/**
 * Latest value across one or more series (for KPI tiles). When "All pages" is
 * selected in real mode, VictoriaMetrics returns one series per page; average
 * every series' latest sample rather than reading only `series[0]`.
 */
export function lastValue(series: TimeSeries[] | undefined): number | null {
  if (!series || series.length === 0) return null;
  let sum = 0;
  let count = 0;
  for (const s of series) {
    const v = seriesLastValue(s);
    if (v != null) {
      sum += v;
      count += 1;
    }
  }
  return count > 0 ? sum / count : null;
}

/**
 * Aggregates one or more series into a single sparkline value array by
 * averaging finite values at each timestamp across all series.
 */
export function aggregateTrend(series: TimeSeries[] | undefined): number[] {
  if (!series || series.length === 0) return [];
  if (series.length === 1) {
    return series[0].points.map((p) => (p[1] != null && Number.isFinite(p[1]) ? p[1] : 0));
  }
  const byTime = new Map<number, { sum: number; count: number }>();
  for (const s of series) {
    for (const [ts, val] of s.points) {
      if (val == null || !Number.isFinite(val)) continue;
      const entry = byTime.get(ts) ?? { sum: 0, count: 0 };
      entry.sum += val;
      entry.count += 1;
      byTime.set(ts, entry);
    }
  }
  return [...byTime.entries()]
    .sort(([a], [b]) => a - b)
    .map(([, { sum, count }]) => (count > 0 ? sum / count : 0));
}

/** Build a PromQL selector for a Web Vital metric scoped by low-cardinality filters. */
export function buildVitalPromQL(metric: string, filters?: VitalFilters): string {
  const matchers: string[] = [];
  if (filters?.page) {
    matchers.push(`page=${JSON.stringify(filters.page)}`);
  }
  if (filters?.device_type) {
    matchers.push(
      filters.device_type === "unknown"
        ? `device_type=~"|unknown"`
        : `device_type=${JSON.stringify(filters.device_type)}`,
    );
  }
  if (filters?.browser_family) {
    matchers.push(
      filters.browser_family === "other"
        ? `browser_family=~"|other"`
        : `browser_family=${JSON.stringify(filters.browser_family)}`,
    );
  }
  if (filters?.os_family) {
    matchers.push(
      filters.os_family === "other"
        ? `os_family=~"|other"`
        : `os_family=${JSON.stringify(filters.os_family)}`,
    );
  }
  if (filters?.country) {
    matchers.push(
      filters.country === "unknown"
        ? `country=~"|unknown"`
        : `country=${JSON.stringify(filters.country)}`,
    );
  }
  if (matchers.length === 0) return metric;
  return `${metric}{${matchers.join(",")}}`;
}

/**
 * Build a bounded, sample-weighted PromQL trend expression for a Web Vital metric.
 * Aggregates across all matched multi-dimensional series in storage so the UI receives
 * one weighted trend series instead of unbounded raw dimension combinations.
 */
export function buildVitalTrendPromQL(
  metric: string,
  window: PerfWindow,
  filters?: VitalFilters,
): string {
  const selector = buildVitalPromQL(metric, filters);
  const windowSpan = Math.max(1, Math.trunc(window.end - window.start));
  const stepSeconds = Math.max(1, Math.min(Math.max(Math.trunc(window.step), 60), windowSpan));
  const range = `${stepSeconds}s`;
  return `sum(sum_over_time(${selector}[${range}])) / sum(count_over_time(${selector}[${range}]))`;
}

/** Trend series for a single vital, optionally scoped to page and dimension filters. */
export async function queryVitalSeries(
  key: VitalKey,
  page: string | undefined,
  window: PerfWindow,
  signal?: AbortSignal,
  project?: string,
  filters?: Omit<VitalFilters, "page">,
): Promise<TimeSeries[]> {
  const combined: VitalFilters = { ...filters, ...(page ? { page } : {}) };
  if (IS_MOCK) return (await loadMockPerformance()).mockVitalSeries(key, page, window, combined);
  const m = vitalByKey(key);
  const query = buildVitalTrendPromQL(m.metric, window, combined);
  const label = buildVitalPromQL(m.metric, combined);
  const series = await queryRange({ query, start: window.start, end: window.end, step: window.step }, signal, project);
  return series.map((s) => (s.name === "{}" ? { ...s, name: label } : s));
}

/** Aggregated Web Vitals breakdown across page, device, browser, OS and country dimensions. */
export async function getVitalsBreakdown(
  window: PerfWindow,
  filters?: VitalFilters,
  signal?: AbortSignal,
  project?: string,
): Promise<VitalsBreakdownResult> {
  if (IS_MOCK) return (await loadMockPerformance()).mockVitalsBreakdown(window, filters, signal);
  const response = await apiGet<unknown>("/performance/vitals", {
    params: {
      start: window.start,
      end: window.end,
      step: window.step,
      page: filters?.page,
      device_type: filters?.device_type,
      browser_family: filters?.browser_family,
      os_family: filters?.os_family,
      country: filters?.country,
    },
    signal,
    project,
  });
  return normalizeVitalsBreakdownResponse(response);
}

export interface PageVitals {
  page: string;
  values: Partial<Record<VitalKey, number>>;
}

const VITAL_SERIES_SELECTOR = `{__name__=~"web_vitals_(lcp|inp|cls|ttfb|fcp)"}`;

/** Pages observed in Web Vital series during the selected time window. */
export async function getVitalPages(
  window: PerfWindow,
  signal?: AbortSignal,
  project?: string,
): Promise<string[]> {
  if (IS_MOCK) return (await loadMockPerformance()).mockVitalPages(window, signal);
  return requestVitalPages(
    (requestSignal) =>
      apiGet<unknown>("/series", {
        params: {
          "match[]": VITAL_SERIES_SELECTOR,
          start: window.start,
          end: window.end,
        },
        signal: requestSignal,
        project,
      }),
    signal,
  );
}

/** Per-page aggregate table backed by the bounded, sample-weighted breakdown endpoint. */
export async function getVitalsByPage(
  window: PerfWindow,
  signal?: AbortSignal,
  project?: string,
  filters?: Omit<VitalFilters, "page">,
): Promise<PageVitals[]> {
  const breakdown = await getVitalsBreakdown(window, filters, signal, project);
  return [...breakdown.by_page]
    .sort((a, b) => a.value.localeCompare(b.value))
    .map((row) => ({ page: row.value, values: row.values }));
}

export type { EndpointPoint, EndpointStat, EndpointStatsResult } from "./performance-contract";
export {
  VITAL_BROWSER_FAMILIES,
  VITAL_DEVICE_TYPES,
  VITAL_OS_FAMILIES,
  type VitalBreakdownRow,
  type VitalBrowserFamily,
  type VitalDeviceType,
  type VitalDimensionKey,
  type VitalFilters,
  type VitalOSFamily,
  type VitalsBreakdownResult,
  type VitalValuesDTO,
} from "./performance-vitals-contract";

export async function getEndpointStats(
  window: PerfWindow,
  signal?: AbortSignal,
  project?: string,
): Promise<EndpointStatsResult> {
  if (IS_MOCK) return (await loadMockPerformance()).mockEndpointStats(window);
  const response = await apiGet<unknown>("/performance/endpoints", {
    params: {
      start: window.start,
      end: window.end,
      step: window.step,
    },
    signal,
    project,
  });
  return normalizeEndpointStatsResponse(response);
}
