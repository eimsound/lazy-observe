import type { TimeSeries } from "../lib/api/types.ts";

/**
 * Overview KPI PromQL queries and their physical unit conversion factors.
 *
 * - `requestRate`: `rate(http_requests_total[5m])` -> req/s (scale = 1).
 * - `p95`: `histogram_quantile(0.95, ..._seconds_bucket)` returns seconds, while
 *   the card renders milliseconds (`ms`) -> scale = 1000 (seconds -> ms).
 * - `errorRate`: `5xx / total` returns a [0, 1] ratio, while the card renders
 *   a percentage (`%`) -> scale = 100 (ratio -> %).
 */
export const OVERVIEW_DASH_QUERIES = {
  requestRate: "sum(rate(http_requests_total[5m]))",
  p95: "histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))",
  errorRate:
    'sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))',
} as const;

export const OVERVIEW_UNIT_SCALES = {
  requestRate: 1,
  /** Seconds -> milliseconds (1000x). */
  p95SecondsToMs: 1_000,
  /** [0, 1] ratio -> [0, 100] percentage (100x). */
  errorRatioToPercent: 100,
} as const;

/** Scale every finite point in each time series by `factor` (e.g. s -> ms, ratio -> %). */
export function scaleTimeSeries(
  series: TimeSeries[] | undefined,
  factor: number,
): TimeSeries[] | undefined {
  if (!series || factor === 1) return series;
  return series.map((item) => ({
    ...item,
    points: item.points.map(([timestamp, value]) => [
      timestamp,
      value != null && Number.isFinite(value) ? value * factor : null,
    ]),
  }));
}

/** Latest finite numeric value across the first series. */
export function lastSeriesValue(series: TimeSeries[] | undefined): number | null {
  const pts = series?.[0]?.points;
  if (!pts) return null;
  for (let i = pts.length - 1; i >= 0; i--) {
    const v = pts[i][1];
    if (v != null && Number.isFinite(v)) return v;
  }
  return null;
}
