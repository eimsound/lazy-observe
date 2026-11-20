/**
 * Strict wire-contract validators for Jaeger-compatible trace responses served
 * by `/api/traces/*` (`internal/gateway/traces_proxy.go`).
 */

/** A Jaeger tag / typed key-value pair. */
export interface JaegerKeyValue {
  key: string;
  type?: string;
  value: string | number | boolean;
}

export interface JaegerSpanRef {
  refType: "CHILD_OF" | "FOLLOWS_FROM";
  traceID: string;
  spanID: string;
}

export interface JaegerSpan {
  traceID: string;
  spanID: string;
  operationName: string;
  /** Epoch microseconds. */
  startTime: number;
  /** Microseconds. */
  duration: number;
  references: JaegerSpanRef[];
  tags: JaegerKeyValue[];
  processID: string;
  logs?: Array<{ timestamp: number; fields: JaegerKeyValue[] }>;
  warnings?: string[] | null;
}

export interface JaegerProcess {
  serviceName: string;
  tags: JaegerKeyValue[];
}

export interface JaegerTrace {
  traceID: string;
  spans: JaegerSpan[];
  processes: Record<string, JaegerProcess>;
  warnings?: string[] | null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function assertRecord(value: unknown, label: string): Record<string, unknown> {
  if (!isRecord(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value;
}

function normalizeTags(value: unknown): JaegerKeyValue[] {
  if (!Array.isArray(value)) return [];
  const tags: JaegerKeyValue[] = [];
  for (const item of value) {
    if (!isRecord(item) || typeof item.key !== "string") continue;
    const rawVal = item.value;
    let normalizedValue: string | number | boolean;
    if (typeof rawVal === "string" || typeof rawVal === "number" || typeof rawVal === "boolean") {
      normalizedValue = rawVal;
    } else if (rawVal === null || rawVal === undefined) {
      normalizedValue = "";
    } else {
      normalizedValue = JSON.stringify(rawVal);
    }
    tags.push({
      key: item.key,
      ...(typeof item.type === "string" ? { type: item.type } : {}),
      value: normalizedValue,
    });
  }
  return tags;
}

function normalizeReferences(value: unknown): JaegerSpanRef[] {
  if (!Array.isArray(value)) return [];
  const refs: JaegerSpanRef[] = [];
  for (const item of value) {
    if (!isRecord(item)) continue;
    if (item.refType !== "CHILD_OF" && item.refType !== "FOLLOWS_FROM") continue;
    if (typeof item.traceID !== "string" || typeof item.spanID !== "string") continue;
    refs.push({
      refType: item.refType,
      traceID: item.traceID,
      spanID: item.spanID,
    });
  }
  return refs;
}

function normalizeSpanLogs(
  value: unknown,
): Array<{ timestamp: number; fields: JaegerKeyValue[] }> | undefined {
  if (!Array.isArray(value)) return undefined;
  const logs: Array<{ timestamp: number; fields: JaegerKeyValue[] }> = [];
  for (const item of value) {
    if (!isRecord(item)) continue;
    const ts = typeof item.timestamp === "number" ? item.timestamp : Number(item.timestamp);
    if (!Number.isFinite(ts)) continue;
    logs.push({
      timestamp: ts,
      fields: normalizeTags(item.fields),
    });
  }
  return logs;
}

function normalizeWarnings(value: unknown): string[] | null | undefined {
  if (value === undefined) return undefined;
  if (value === null) return null;
  if (!Array.isArray(value)) return undefined;
  return value.filter((item): item is string => typeof item === "string");
}

function normalizeSpan(value: unknown, label: string): JaegerSpan {
  const raw = assertRecord(value, label);
  if (typeof raw.traceID !== "string" || !raw.traceID) {
    throw new TypeError(`${label}.traceID must be a non-empty string`);
  }
  if (typeof raw.spanID !== "string" || !raw.spanID) {
    throw new TypeError(`${label}.spanID must be a non-empty string`);
  }
  const startTime = typeof raw.startTime === "number" ? raw.startTime : Number(raw.startTime);
  const duration = typeof raw.duration === "number" ? raw.duration : Number(raw.duration);
  if (!Number.isFinite(startTime)) {
    throw new TypeError(`${label}.startTime must be a finite number`);
  }
  if (!Number.isFinite(duration) || duration < 0) {
    throw new TypeError(`${label}.duration must be a non-negative finite number`);
  }
  const logs = normalizeSpanLogs(raw.logs);
  const warnings = normalizeWarnings(raw.warnings);
  return {
    traceID: raw.traceID,
    spanID: raw.spanID,
    operationName: typeof raw.operationName === "string" ? raw.operationName : "",
    startTime,
    duration,
    references: normalizeReferences(raw.references),
    tags: normalizeTags(raw.tags),
    processID: typeof raw.processID === "string" ? raw.processID : "",
    ...(logs !== undefined ? { logs } : {}),
    ...(warnings !== undefined ? { warnings } : {}),
  };
}

function normalizeProcesses(value: unknown, label: string): Record<string, JaegerProcess> {
  if (value === undefined || value === null) return {};
  const raw = assertRecord(value, label);
  const processes: Record<string, JaegerProcess> = {};
  for (const [processID, entry] of Object.entries(raw)) {
    if (!isRecord(entry)) continue;
    processes[processID] = {
      serviceName: typeof entry.serviceName === "string" ? entry.serviceName : processID,
      tags: normalizeTags(entry.tags),
    };
  }
  return processes;
}

export function normalizeJaegerTrace(value: unknown, label = "trace"): JaegerTrace {
  const raw = assertRecord(value, label);
  if (typeof raw.traceID !== "string" || !raw.traceID) {
    throw new TypeError(`${label}.traceID must be a non-empty string`);
  }
  const rawSpans = raw.spans === null || raw.spans === undefined ? [] : raw.spans;
  if (!Array.isArray(rawSpans)) {
    throw new TypeError(`${label}.spans must be an array`);
  }
  const warnings = normalizeWarnings(raw.warnings);
  return {
    traceID: raw.traceID,
    spans: rawSpans.map((span, idx) => normalizeSpan(span, `${label}.spans[${idx}]`)),
    processes: normalizeProcesses(raw.processes, `${label}.processes`),
    ...(warnings !== undefined ? { warnings } : {}),
  };
}

/** Validate `/api/traces/services` and `/api/traces/services/:svc/operations`. */
export function normalizeTraceStringListResponse(value: unknown): string[] {
  const root = assertRecord(value, "traces list response");
  if (root.data === null || root.data === undefined) return [];
  if (!Array.isArray(root.data)) {
    throw new TypeError("traces list response data must be an array or null");
  }
  return root.data.filter((item): item is string => typeof item === "string");
}

/** Validate `/api/traces/search` and `/api/traces/:traceID` envelope `{ data: JaegerTrace[] | null }`. */
export function normalizeJaegerTraceListResponse(value: unknown): JaegerTrace[] {
  const root = assertRecord(value, "traces response");
  if (root.data === null || root.data === undefined) return [];
  if (!Array.isArray(root.data)) {
    throw new TypeError("traces response data must be an array or null");
  }
  return root.data.map((trace, idx) => normalizeJaegerTrace(trace, `traces.data[${idx}]`));
}
