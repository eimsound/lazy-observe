import dayjs from "../dayjs.ts";

/** Time-range presets for the metrics query page. */

export interface RangePreset {
  id: string;
  labelKey: string;
  /** Window length in seconds. */
  seconds: number;
  /** Target number of points -> derived step. */
  points: number;
}

export const RANGE_PRESETS: RangePreset[] = [
  { id: "15m", labelKey: "metrics.range15m", seconds: 15 * 60, points: 90 },
  { id: "1h", labelKey: "metrics.range1h", seconds: 60 * 60, points: 120 },
  { id: "6h", labelKey: "metrics.range6h", seconds: 6 * 60 * 60, points: 180 },
  { id: "24h", labelKey: "metrics.range24h", seconds: 24 * 60 * 60, points: 240 },
];

export const DEFAULT_RANGE_ID = "1h";

/** Resolve a preset into concrete start/end/step (unix seconds) at call time. */
export function resolveRange(preset: RangePreset, nowMs = dayjs().valueOf()) {
  const end = Math.floor(nowMs / 1000);
  const start = end - preset.seconds;
  const step = Math.max(1, Math.floor(preset.seconds / preset.points));
  return { start, end, step };
}

/** Same window in epoch microseconds — the unit the Jaeger trace API expects. */
export function resolveRangeMicros(preset: RangePreset, nowMs = dayjs().valueOf()) {
  const { start, end } = resolveRange(preset, nowMs);
  return { start: start * 1_000_000, end: end * 1_000_000 };
}

/** Look up a preset by id, falling back to the default. */
export function rangeById(id: string): RangePreset {
  return (
    RANGE_PRESETS.find((r) => r.id === id) ??
    RANGE_PRESETS.find((r) => r.id === DEFAULT_RANGE_ID) ??
    RANGE_PRESETS[0]
  );
}
