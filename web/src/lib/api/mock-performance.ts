import type { TimeSeries } from "./types";
import type {
  PageVitals,
  PerfWindow,
  VitalKey,
} from "./performance";
import { VITALS } from "./performance";
import type {
  EndpointPoint,
  EndpointStat,
  EndpointStatsResult,
} from "./performance-contract";
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

export function mockVitalSeries(
  key: VitalKey,
  page: string | undefined,
  window: PerfWindow,
): Promise<TimeSeries[]> {
  const pageIndex = page ? Math.max(0, PERF_PAGES.indexOf(page)) : -1;
  // page === undefined => "all pages" aggregate (average across pages).
  const center =
    pageIndex >= 0
      ? vitalValue(key, pageIndex)
      : PERF_PAGES.reduce((sum, _p, i) => sum + vitalValue(key, i), 0) / PERF_PAGES.length;
  const m = VITALS.find((v) => v.key === key)!;
  const jitter = m.unit === "ms" ? center * 0.18 : center * 0.3;
  const series: TimeSeries = {
    name: page ? `${m.metric}{page="${page}"}` : m.metric,
    labels: page ? { __name__: m.metric, page } : { __name__: m.metric },
    points: buildPoints(window, center, jitter, `${key}|${page ?? "all"}|${window.end}`),
  };
  return new Promise((resolve) => setTimeout(() => resolve([series]), 260));
}

export function mockVitalsByPage(_window: PerfWindow): Promise<PageVitals[]> {
  const rows: PageVitals[] = PERF_PAGES.map((page, i) => {
    const values = {} as Record<VitalKey, number>;
    for (const v of VITALS) values[v.key] = vitalValue(v.key, i);
    return { page, values };
  });
  return new Promise((resolve) => setTimeout(() => resolve(rows), 240));
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
