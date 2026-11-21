import type { LogRow } from "@/lib/api/logs";
import { hashString } from "@/lib/hash";

/** Pure model for the logs explorer views: settings, defaults, and field access. */

export type LogsSortDirection = "asc" | "desc";

export interface LogsGroupViewSettings {
  /** A raw field name, or null to render an ungrouped log stream. */
  groupBy: string | null;
  /** Additional raw fields rendered alongside the normalized message. */
  displayFields: string[];
  /** Direction used when sorting groups by their row count. */
  groupSort: LogsSortDirection;
  /** Keep each message on one visual line. */
  singleLine: boolean;
  /** Reduce group and row heading padding. */
  compactHeader: boolean;
  /** Render a safe Markdown subset in the message field. */
  markdownParsing?: boolean;
  /** Render terminal SGR colors/styles in the message field. */
  ansiParsing?: boolean;
  /** Disable row hover styling for large result sets. */
  disableHover?: boolean;
  /** One-based page. Pages contain groups when grouped and rows otherwise. */
  page: number;
  pageSize: number;
}

export interface LogsTableSort {
  field: string;
  direction: LogsSortDirection;
}

export interface LogsTableViewSettings {
  /** Raw/normalized fields shown as table columns, in display order. */
  visibleFields: string[];
  /** User-resized widths, in CSS pixels, keyed by field. */
  columnWidths?: Record<string, number>;
  sort: LogsTableSort | null;
  /** One-based page. */
  page: number;
  pageSize: number;
}

export interface LogsFieldStats {
  presentRows: number;
  uniqueValues: number;
  coveragePercent: number;
}

export const DEFAULT_LOG_TABLE_FIELDS = ["_time", "_msg", "_stream"];

export function createDefaultLogsGroupViewSettings(): LogsGroupViewSettings {
  return {
    groupBy: null,
    displayFields: [],
    groupSort: "desc",
    singleLine: true,
    compactHeader: false,
    markdownParsing: false,
    ansiParsing: false,
    disableHover: false,
    page: 1,
    pageSize: 100,
  };
}

export function createDefaultLogsTableViewSettings(): LogsTableViewSettings {
  return {
    visibleFields: [...DEFAULT_LOG_TABLE_FIELDS],
    columnWidths: {},
    sort: null,
    page: 1,
    pageSize: 100,
  };
}

export interface RowField {
  present: boolean;
  value: unknown;
}

function bigintReplacer(_key: string, item: unknown): unknown {
  return typeof item === "bigint" ? item.toString() : item;
}

export function safeJsonStringify(value: unknown, pretty = false): string {
  try {
    return (
      (pretty ? JSON.stringify(value, null, 2) : JSON.stringify(value)) ??
      String(value)
    );
  } catch {
    try {
      return (
        JSON.stringify(value, bigintReplacer, pretty ? 2 : undefined) ??
        String(value)
      );
    } catch {
      return String(value);
    }
  }
}

/**
 * Builds position-independent, collision-resistant stable keys for log rows
 * without eagerly serializing `row.raw` on the hot path.
 *
 * Pass 1 buckets rows by lightweight O(1) coarse identity (`timeRaw + stream + message`).
 * Only buckets with > 1 colliding rows fall back to `safeJsonStringify(row.raw)`
 * + FNV-1a hashing + occurrence counter so distinct payloads and exact duplicates
 * remain uniquely and stably keyed across refreshes.
 */
export function buildStableLogRowKeys(rows: ReadonlyArray<LogRow>): string[] {
  const count = rows.length;
  const keys = new Array<string>(count);
  const buckets = new Map<string, number | number[]>();

  for (let i = 0; i < count; i++) {
    const row = rows[i];
    const coarse = `${row.timeRaw}\0${row.stream}\0${row.message}`;
    const existing = buckets.get(coarse);
    if (existing === undefined) {
      buckets.set(coarse, i);
      keys[i] = coarse;
    } else if (typeof existing === "number") {
      buckets.set(coarse, [existing, i]);
    } else {
      existing.push(i);
    }
  }

  for (const [coarse, bucket] of buckets) {
    if (typeof bucket === "number") continue;
    const seen = new Map<string, number>();
    for (const idx of bucket) {
      const serialized = safeJsonStringify(rows[idx].raw);
      const fingerprint = `${coarse}\0${hashString(serialized).toString(36)}-${serialized.length}`;
      const occurrence = seen.get(fingerprint) ?? 0;
      seen.set(fingerprint, occurrence + 1);
      keys[idx] = `${fingerprint}#${occurrence}`;
    }
  }

  return keys;
}

export function stringifyFieldValue(value: unknown): string {
  if (value === null) return "null";
  if (value === undefined) return "undefined";
  if (typeof value === "string") return value || '""';
  if (typeof value === "object") return safeJsonStringify(value);
  return String(value);
}

export function getRowField(row: LogRow, field: string): RowField {
  if (Object.prototype.hasOwnProperty.call(row.raw, field)) {
    return { present: true, value: row.raw[field] };
  }

  switch (field) {
    case "_time":
      return { present: Boolean(row.timeRaw || row.time), value: row.timeRaw };
    case "_msg":
      return { present: true, value: row.message };
    case "_stream":
      return { present: true, value: row.stream };
    case "level":
      return { present: true, value: row.level };
    default:
      return { present: false, value: undefined };
  }
}

export function collectFields(
  rows: LogRow[],
  additionalFields: ReadonlyArray<string> = [],
): string[] {
  const fields = new Set<string>(DEFAULT_LOG_TABLE_FIELDS);
  fields.add("level");
  for (const field of additionalFields) fields.add(field);
  for (const row of rows) {
    for (const field of Object.keys(row.raw)) fields.add(field);
    if (row.traceId) fields.add("trace_id");
  }

  const priority = new Map([
    ["_time", 0],
    ["_msg", 1],
    ["_stream", 2],
    ["level", 3],
    ["trace_id", 4],
  ]);

  return [...fields].sort((left, right) => {
    const leftPriority = priority.get(left);
    const rightPriority = priority.get(right);
    if (leftPriority !== undefined || rightPriority !== undefined) {
      return (leftPriority ?? Number.MAX_SAFE_INTEGER) -
        (rightPriority ?? Number.MAX_SAFE_INTEGER);
    }
    return left.localeCompare(right);
  });
}

export function groupValueId(value: RowField): string {
  if (!value.present) return "missing";
  return `${typeof value.value}:${safeJsonStringify(value.value)}`;
}

export function getLogsFieldStats(
  rows: LogRow[],
  fields: ReadonlyArray<string> = collectFields(rows),
): Map<string, LogsFieldStats> {
  const stats = new Map<string, LogsFieldStats>();
  for (const field of fields) {
    let presentRows = 0;
    const unique = new Set<string>();
    for (const row of rows) {
      const value = getRowField(row, field);
      if (!value.present) continue;
      presentRows++;
      unique.add(groupValueId(value));
    }
    stats.set(field, {
      presentRows,
      uniqueValues: unique.size,
      coveragePercent: rows.length === 0 ? 0 : (presentRows / rows.length) * 100,
    });
  }
  return stats;
}
