import dayjs, { dayjsLocale } from "../../lib/dayjs.ts";

export interface TimeSeriesTimeWindow {
  startMs: number;
  endMs: number;
}

export interface TimeSeriesTimeConfig {
  ms: 1;
  xRange?: [number, number];
}

/** Map the chart's millisecond timestamps onto an optional explicit viewport. */
export function createTimeSeriesTimeConfig(
  timeWindow?: TimeSeriesTimeWindow,
): TimeSeriesTimeConfig {
  if (!timeWindow) return { ms: 1 };
  if (
    !Number.isFinite(timeWindow.startMs)
    || !Number.isFinite(timeWindow.endMs)
    || timeWindow.startMs >= timeWindow.endMs
  ) {
    throw new Error("invalid time-series chart window");
  }
  return { ms: 1, xRange: [timeWindow.startMs, timeWindow.endMs] };
}

export function formatTimeSeriesTimestamp(
  timestampMs: number,
  useUTC: boolean,
  locale: string,
): string {
  const value = useUTC ? dayjs.utc(timestampMs) : dayjs(timestampMs);
  return value.locale(dayjsLocale(locale)).format("LL LTS");
}
