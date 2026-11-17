import dayjs from "../dayjs.ts";

export type LogLevel =
  | "error"
  | "warn"
  | "info"
  | "debug"
  | "trace"
  | "unknown";

/** A single normalized log line. `raw` preserves the original JSON object. */
export interface LogRow {
  /** Stable id for list keys (index within the returned page or stream). */
  id: string;
  /** Epoch milliseconds (parsed from `_time`). */
  time: number;
  /** Original `_time` value as returned by the backend. */
  timeRaw: string;
  /** `_msg`. */
  message: string;
  /** `_stream` label set, e.g. `{app="api",env="prod"}`. */
  stream: string;
  level: LogLevel;
  /** Present when the line carries a `trace_id` field. */
  traceId?: string;
  /** Full original line (all fields, including `_time`/`_msg`/`_stream`). */
  raw: Record<string, unknown>;
}

/** Normalized histogram: parallel arrays of bucket start (ms) + count. */
export interface LogsHistogram {
  timestamps: number[];
  values: number[];
}

/** One normalized series returned by VictoriaLogs `/hits`. */
export interface LogsHitSeries {
  fields: Record<string, string>;
  timestamps: number[];
  values: number[];
  total: number;
  isOther: boolean;
}

export interface LogsHitsResponse {
  hits: LogsHitSeries[];
}

export interface LogsStatsSample {
  /** Unix seconds, as returned by the Prometheus-compatible matrix response. */
  timestamp: number;
  /** The raw numeric string. It may contain values such as `NaN` or `+Inf`. */
  value: string;
}

export interface LogsStatsSeries {
  metric: Record<string, string>;
  values: LogsStatsSample[];
}

export interface LogsStatsQueryRangeResponse {
  status: string;
  data: {
    resultType: string;
    result: LogsStatsSeries[];
  };
  error?: string;
  errorType?: string;
}

export interface LogsQueryTimeRange {
  start: string;
  end: string;
  hasTimeFilter: boolean;
}

export interface LogsFieldValue {
  value: string;
  hits: number;
}

export interface LogsFieldValuesResponse {
  values: LogsFieldValue[];
}

const LEVEL_ALIASES: Record<string, LogLevel> = {
  error: "error",
  err: "error",
  fatal: "error",
  critical: "error",
  crit: "error",
  warn: "warn",
  warning: "warn",
  info: "info",
  information: "info",
  notice: "info",
  debug: "debug",
  trace: "trace",
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function finiteNumber(value: unknown): number | undefined {
  const result = typeof value === "number" ? value : Number(value);
  return Number.isFinite(result) ? result : undefined;
}

/** Best-effort level detection from common field names, else from the message. */
export function detectLevel(raw: Record<string, unknown>): LogLevel {
  const candidates = [raw.level, raw.severity, raw.severity_text, raw.lvl];
  for (const candidate of candidates) {
    if (typeof candidate === "string") {
      const hit = LEVEL_ALIASES[candidate.trim().toLowerCase()];
      if (hit) return hit;
    }
  }
  const message = typeof raw._msg === "string" ? raw._msg.toLowerCase() : "";
  if (/\b(error|exception|fatal|panic)\b/.test(message)) return "error";
  if (/\bwarn(ing)?\b/.test(message)) return "warn";
  return "unknown";
}

/** Parse an RFC3339 or numeric VictoriaLogs timestamp into epoch milliseconds. */
export function parseLogsTimestamp(value: unknown): { ms: number; raw: string } {
  if (typeof value === "number") {
    const absolute = Math.abs(value);
    // VictoriaLogs may expose Unix seconds, milliseconds, or nanoseconds.
    const ms = absolute >= 1e15 ? value / 1e6 : absolute >= 1e11 ? value : value * 1000;
    return { ms: Number.isFinite(ms) ? ms : 0, raw: String(value) };
  }
  if (typeof value === "string") {
    if (/^-?\d+(?:\.\d+)?$/.test(value.trim())) {
      const parsed = parseLogsTimestamp(Number(value));
      return { ms: parsed.ms, raw: value };
    }
    const ms = dayjs(value).valueOf();
    return { ms: Number.isNaN(ms) ? 0 : ms, raw: value };
  }
  return { ms: 0, raw: "" };
}

/** Normalize one parsed JSON line into a LogRow. */
export function normalizeLine(raw: Record<string, unknown>, index: number): LogRow {
  const { ms, raw: timeRaw } = parseLogsTimestamp(raw._time);
  const traceId =
    typeof raw.trace_id === "string" && raw.trace_id ? raw.trace_id : undefined;
  return {
    id: `${ms}-${index}`,
    time: ms,
    timeRaw,
    message: typeof raw._msg === "string" ? raw._msg : "",
    stream: typeof raw._stream === "string" ? raw._stream : "",
    level: detectLevel(raw),
    traceId,
    raw,
  };
}

export type NdjsonMapper<T> = (value: unknown, lineIndex: number) => T | null | undefined;

export interface NdjsonParser<T> {
  /** Parse every complete line in a chunk and retain an incomplete suffix. */
  push(chunk: string | Uint8Array): T[];
  /** Flush the decoder and parse a final line even when it has no trailing LF. */
  finish(): T[];
}

/**
 * Incremental NDJSON parser. A single JSON value may span arbitrary byte
 * chunks, including chunks split in the middle of a UTF-8 code point.
 * Malformed and blank lines are skipped without terminating the stream.
 */
export function createNdjsonParser<T = unknown>(
  mapper: NdjsonMapper<T> = (value) => value as T,
): NdjsonParser<T> {
  const decoder = new TextDecoder();
  let buffer = "";
  let nextLineIndex = 0;
  let finished = false;

  const consume = (final: boolean): T[] => {
    const lines = buffer.split("\n");
    buffer = final ? "" : (lines.pop() ?? "");
    const result: T[] = [];

    for (const sourceLine of lines) {
      const lineIndex = nextLineIndex++;
      const line = sourceLine.endsWith("\r") ? sourceLine.slice(0, -1) : sourceLine;
      if (!line.trim()) continue;
      try {
        const mapped = mapper(JSON.parse(line) as unknown, lineIndex);
        if (mapped !== null && mapped !== undefined) result.push(mapped);
      } catch {
        // One corrupt stream line must not discard later valid records.
      }
    }
    return result;
  };

  return {
    push(chunk) {
      if (finished) throw new Error("cannot push to a finished NDJSON parser");
      buffer += typeof chunk === "string" ? chunk : decoder.decode(chunk, { stream: true });
      return consume(false);
    },
    finish() {
      if (finished) return [];
      finished = true;
      buffer += decoder.decode();
      // A sentinel LF makes the incomplete suffix part of `lines`.
      if (buffer) buffer += "\n";
      return consume(true);
    },
  };
}

/** Parse a complete JSON-lines payload, skipping blank or malformed lines. */
export function parseJsonLines(text: string): LogRow[] {
  const parser = createNdjsonParser<LogRow>((value, index) => (
    isRecord(value) ? normalizeLine(value, index) : null
  ));
  return [...parser.push(text), ...parser.finish()];
}

export interface ParseNdjsonStreamOptions<T> {
  signal?: AbortSignal;
  mapper?: NdjsonMapper<T>;
}

/** Incrementally consume a web ReadableStream as NDJSON. */
export async function* parseNdjsonStream<T = unknown>(
  stream: ReadableStream<Uint8Array>,
  options: ParseNdjsonStreamOptions<T> = {},
): AsyncGenerator<T, void, undefined> {
  const reader = stream.getReader();
  const parser = createNdjsonParser(options.mapper);
  let completed = false;
  const cancel = () => {
    void reader.cancel(options.signal?.reason).catch(() => undefined);
  };
  options.signal?.addEventListener("abort", cancel, { once: true });

  try {
    if (options.signal?.aborted) return;
    while (!options.signal?.aborted) {
      const { done, value } = await reader.read();
      if (done) {
        completed = true;
        break;
      }
      if (!value) continue;
      for (const item of parser.push(value)) yield item;
    }
    if (!options.signal?.aborted) {
      for (const item of parser.finish()) yield item;
    }
  } finally {
    options.signal?.removeEventListener("abort", cancel);
    if (!completed) {
      await reader.cancel().catch(() => undefined);
    }
    reader.releaseLock();
  }
}

function normalizeFields(value: unknown): Record<string, string> {
  if (!isRecord(value)) return {};
  const fields: Record<string, string> = {};
  for (const [key, fieldValue] of Object.entries(value)) {
    if (typeof fieldValue === "string") fields[key] = fieldValue;
    else if (fieldValue !== null && fieldValue !== undefined) fields[key] = String(fieldValue);
  }
  return fields;
}

function normalizeTimestampValues(
  timestampsValue: unknown,
  valuesValue: unknown,
): { timestamps: number[]; values: number[] } {
  if (!Array.isArray(timestampsValue) || !Array.isArray(valuesValue)) {
    return { timestamps: [], values: [] };
  }
  const points: Array<{ timestamp: number; value: number }> = [];
  for (let index = 0; index < timestampsValue.length; index++) {
    const rawTimestamp = timestampsValue[index];
    const timestamp = parseLogsTimestamp(rawTimestamp).ms;
    const validEpoch = timestamp !== 0
      || rawTimestamp === 0
      || rawTimestamp === "0"
      || (typeof rawTimestamp === "string" && dayjs(rawTimestamp).isValid());
    if (!validEpoch) continue;
    points.push({
      timestamp,
      value: finiteNumber(valuesValue[index]) ?? 0,
    });
  }
  points.sort((left, right) => left.timestamp - right.timestamp);
  return {
    timestamps: points.map((point) => point.timestamp),
    values: points.map((point) => point.value),
  };
}

/** Normalize every VictoriaLogs `/hits` series and mark the aggregate "other". */
export function normalizeHitsSeries(response: unknown): LogsHitSeries[] {
  if (!isRecord(response) || !Array.isArray(response.hits)) return [];
  const hits = response.hits.flatMap((value): LogsHitSeries[] => {
    if (!isRecord(value)) return [];
    const fields = normalizeFields(value.fields);
    const { timestamps, values } = normalizeTimestampValues(value.timestamps, value.values);
    const declaredTotal = finiteNumber(value.total);
    return [{
      fields,
      timestamps,
      values,
      total: declaredTotal ?? values.reduce((sum, item) => sum + item, 0),
      isOther: Object.keys(fields).length === 0,
    }];
  });
  return hits.sort((left, right) => {
    if (left.isOther !== right.isOther) return left.isOther ? -1 : 1;
    return right.total - left.total;
  });
}

/**
 * Normalize a histogram. Flat legacy payloads remain supported; multi-series
 * responses are summed by timestamp rather than by array position.
 */
export function normalizeHits(response: unknown): LogsHistogram {
  if (isRecord(response) && Array.isArray(response.timestamps)) {
    return normalizeTimestampValues(response.timestamps, response.values);
  }

  const valuesByTimestamp = new Map<number, number>();
  for (const series of normalizeHitsSeries(response)) {
    for (let index = 0; index < series.timestamps.length; index++) {
      const timestamp = series.timestamps[index];
      valuesByTimestamp.set(
        timestamp,
        (valuesByTimestamp.get(timestamp) ?? 0) + (series.values[index] ?? 0),
      );
    }
  }
  const timestamps = [...valuesByTimestamp.keys()].sort((left, right) => left - right);
  return {
    timestamps,
    values: timestamps.map((timestamp) => valuesByTimestamp.get(timestamp) ?? 0),
  };
}

/** Normalize VictoriaLogs' Prometheus-compatible matrix response. */
export function normalizeStatsQueryRange(response: unknown): LogsStatsQueryRangeResponse {
  const root = isRecord(response) ? response : {};
  const data = isRecord(root.data) ? root.data : {};
  const rawResult = Array.isArray(data.result) ? data.result : [];
  const result = rawResult.flatMap((value): LogsStatsSeries[] => {
    if (!isRecord(value) || !Array.isArray(value.values)) return [];
    const values = value.values.flatMap((sample): LogsStatsSample[] => {
      if (!Array.isArray(sample) || sample.length < 2) return [];
      const timestamp = finiteNumber(sample[0]);
      if (timestamp === undefined) return [];
      return [{ timestamp, value: String(sample[1]) }];
    });
    return [{ metric: normalizeFields(value.metric), values }];
  });

  return {
    status: typeof root.status === "string" ? root.status : "success",
    data: {
      resultType: typeof data.resultType === "string" ? data.resultType : "matrix",
      result,
    },
    ...(typeof root.error === "string" ? { error: root.error } : {}),
    ...(typeof root.errorType === "string" ? { errorType: root.errorType } : {}),
  };
}

export function normalizeQueryTimeRange(response: unknown): LogsQueryTimeRange {
  const root = isRecord(response) ? response : {};
  return {
    start: typeof root.start === "string" ? root.start : "",
    end: typeof root.end === "string" ? root.end : "",
    hasTimeFilter: root.hasTimeFilter === true,
  };
}

export function normalizeFieldValuesResponse(response: unknown): LogsFieldValuesResponse {
  const root = isRecord(response) ? response : {};
  const rawValues = Array.isArray(root.values) ? root.values : [];
  return {
    values: rawValues.flatMap((item): LogsFieldValue[] => {
      if (!isRecord(item) || typeof item.value !== "string") return [];
      return [{ value: item.value, hits: finiteNumber(item.hits) ?? 0 }];
    }),
  };
}
