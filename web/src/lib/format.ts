import dayjs, { localizedDayjs } from "./dayjs.ts";

/**
 * Locale-agnostic formatting helpers shared by the Logs and Traces pages. These
 * emit numbers/units only (no translatable words), so they're safe to call
 * outside the i18n dictionary.
 */

/** Format a microsecond duration as µs / ms / s with sensible precision. */
export function formatDurationMicros(us: number): string {
  if (us < 1000) return `${Math.round(us)}µs`;
  if (us < 1_000_000) {
    const ms = us / 1000;
    return `${ms < 10 ? ms.toFixed(2) : ms < 100 ? ms.toFixed(1) : Math.round(ms)}ms`;
  }
  const s = us / 1_000_000;
  return `${s < 10 ? s.toFixed(2) : s.toFixed(1)}s`;
}

/** Format a millisecond duration (used for log request durations). */
export function formatDurationMs(ms: number): string {
  return formatDurationMicros(ms * 1000);
}

/** Wall-clock time only: HH:mm:ss.SSS (local). */
export function formatClock(ms: number): string {
  return dayjs(ms).format("HH:mm:ss.SSS");
}

/** Full local date-time: YYYY-MM-DD HH:mm:ss.SSS. */
export function formatDateTime(ms: number): string {
  return dayjs(ms).format("YYYY-MM-DD HH:mm:ss.SSS");
}

/** Percentage string with one decimal, e.g. "12.4%". */
export function formatPercent(fraction: number): string {
  return `${(fraction * 100).toFixed(1)}%`;
}

/** Truncate an id for compact display (keep head + tail). */
export function shortId(id: string, head = 8): string {
  return id.length <= head ? id : id.slice(0, head);
}

/**
 * Coarse human duration for a millisecond span, e.g. "3d", "5h", "12m", "<1m".
 * Used for silence "expires in / ended ago" labels (no translatable words).
 */
export function formatDurationShort(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return `${m}m`;
  return "<1m";
}

/**
 * Relative time for a past/future epoch-ms timestamp localized by Day.js.
 */
export function formatRelativeTime(
  ms: number,
  locale: string,
  nowMs = dayjs().valueOf(),
): string {
  return localizedDayjs(ms, locale).from(dayjs(nowMs));
}

/** Human-readable byte size, e.g. "1.4 MB", "820 KB". */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB"];
  let v = bytes / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

/** Compact clock duration for a millisecond span, e.g. "1:23" (m:ss) or "12s". */
export function formatClockDuration(ms: number): string {
  const totalSec = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  if (m === 0) return `${s}s`;
  return `${m}:${String(s).padStart(2, "0")}`;
}
