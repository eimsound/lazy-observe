import { queryRange } from "./metrics";
import { apiGet, IS_MOCK } from "./client";
import type { TimeSeries } from "./types";
import {
  normalizeEndpointStatsResponse,
  type EndpointStatsResult,
} from "./performance-contract";
import { requestVitalPages } from "./performance-pages-contract";

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

/** Trend series for a single vital, optionally scoped to one page. */
export async function queryVitalSeries(
  key: VitalKey,
  page: string | undefined,
  window: PerfWindow,
  signal?: AbortSignal,
): Promise<TimeSeries[]> {
  if (IS_MOCK) return (await loadMockPerformance()).mockVitalSeries(key, page, window);
  const m = vitalByKey(key);
  const query = page ? `${m.metric}{page=${JSON.stringify(page)}}` : m.metric;
  return queryRange({ query, start: window.start, end: window.end, step: window.step }, signal);
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
      }),
    signal,
  );
}

/** Per-page aggregate table (latest value of each vital, grouped by page). */
export async function getVitalsByPage(
  window: PerfWindow,
  signal?: AbortSignal,
): Promise<PageVitals[]> {
  if (IS_MOCK) return (await loadMockPerformance()).mockVitalsByPage(window);

  // Real mode: one query per vital (series come back labelled by `page`); join.
  const perVital = await Promise.all(
    VITALS.map(async (m) => {
      const series = await queryRange(
        { query: m.metric, start: window.start, end: window.end, step: window.step },
        signal,
      );
      return { key: m.key, series };
    }),
  );
  const byPage = new Map<string, Partial<Record<VitalKey, number>>>();
  for (const { key, series } of perVital) {
    for (const s of series) {
      const page = s.labels.page;
      const value = lastValue([s]);
      if (!page || value == null) continue;
      const row = byPage.get(page) ?? {};
      row[key] = value;
      byPage.set(page, row);
    }
  }
  return [...byPage.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([page, values]) => ({ page, values }));
}

export type { EndpointPoint, EndpointStat, EndpointStatsResult } from "./performance-contract";

export async function getEndpointStats(
  window: PerfWindow,
  signal?: AbortSignal,
): Promise<EndpointStatsResult> {
  if (IS_MOCK) return (await loadMockPerformance()).mockEndpointStats(window);
  const response = await apiGet<unknown>("/performance/endpoints", {
    params: {
      start: window.start,
      end: window.end,
      step: window.step,
    },
    signal,
  });
  return normalizeEndpointStatsResponse(response);
}
