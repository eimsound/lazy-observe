import type { JaegerSpan, JaegerTrace } from "../lib/api/traces";

const CONTEXT_PADDING_SECONDS = 5 * 60;

export interface ProfilingEvidenceSearch {
  service?: string;
  type?: string;
  range?: string;
  start?: number;
  end?: number;
  evidence?: "contextual";
}

export interface MetricsEvidenceSearch {
  q?: string;
  run?: "1";
  start?: number;
  end?: number;
  evidence?: "contextual";
}

export interface TraceEvidenceLinks {
  logs: {
    to: "/logs";
    relationship: "exact";
    search: { q: string; run: 1 };
  };
  profiling: {
    to: "/profiling";
    relationship: "contextual";
    search: Required<Pick<ProfilingEvidenceSearch, "service" | "start" | "end" | "evidence">>;
  };
  metrics: {
    to: "/metrics";
    relationship: "contextual";
    search: Required<Pick<MetricsEvidenceSearch, "q" | "run" | "start" | "end" | "evidence">>;
  };
}

export function buildTraceEvidenceLinks(trace: JaegerTrace): TraceEvidenceLinks {
  const spans = trace.spans ?? [];
  if (spans.length === 0) throw new TypeError("trace evidence requires at least one span");
  const root = rootSpan(trace);
  const service = trace.processes?.[root.processID]?.serviceName?.trim() || root.processID;
  if (!service) throw new TypeError("trace evidence requires a root service");

  let startUs = Infinity;
  let endUs = -Infinity;
  for (const span of spans) {
    if (span.startTime < startUs) startUs = span.startTime;
    const spanEnd = span.startTime + span.duration;
    if (spanEnd > endUs) endUs = spanEnd;
  }
  if (!Number.isSafeInteger(startUs) || !Number.isSafeInteger(endUs) || endUs < startUs) {
    throw new TypeError("trace evidence requires valid span bounds");
  }
  const start = Math.max(0, Math.floor(startUs / 1_000_000) - CONTEXT_PADDING_SECONDS);
  const end = Math.ceil(endUs / 1_000_000) + CONTEXT_PADDING_SECONDS;
  const route = stringTag(root, "http.route") || operationRoute(root.operationName);
  const method = stringTag(root, "http.request.method")
    || stringTag(root, "http.method")
    || operationMethod(root.operationName);

  return {
    logs: {
      to: "/logs",
      relationship: "exact",
      search: { q: `trace_id:"${logsValue(trace.traceID)}"`, run: 1 },
    },
    profiling: {
      to: "/profiling",
      relationship: "contextual",
      search: { service, start, end, evidence: "contextual" },
    },
    metrics: {
      to: "/metrics",
      relationship: "contextual",
      search: {
        q: httpMetricsQuery(service, method, route),
        run: "1",
        start,
        end,
        evidence: "contextual",
      },
    },
  };
}

export function parseProfilingEvidenceSearch(
  search: Record<string, unknown>,
): ProfilingEvidenceSearch {
  const window = absoluteWindow(search);
  return compact({
    service: stringValue(search.service),
    type: stringValue(search.type),
    range: stringValue(search.range),
    ...window,
    evidence: search.evidence === "contextual" ? "contextual" : undefined,
  });
}

export function parseMetricsEvidenceSearch(search: Record<string, unknown>): MetricsEvidenceSearch {
  const window = absoluteWindow(search);
  return compact({
    q: stringValue(search.q),
    run: flag(search.run),
    ...window,
    evidence: search.evidence === "contextual" ? "contextual" : undefined,
  });
}

function rootSpan(trace: JaegerTrace): JaegerSpan {
  const spans = trace.spans ?? [];
  const ids = new Set(spans.map((span) => span.spanID));
  return spans.find(
    (span) => !(span.references ?? []).some(
      (reference) =>
        reference.refType === "CHILD_OF"
        && reference.spanID !== span.spanID
        && ids.has(reference.spanID),
    ),
  ) ?? spans[0];
}

function stringTag(span: JaegerSpan, key: string): string {
  const value = (span.tags ?? []).find((tag) => tag.key === key)?.value;
  return typeof value === "string" ? value.trim() : "";
}

function operationMethod(operation: string): string {
  const match = /^([A-Z]+)\s+/.exec(operation.trim());
  return match?.[1] ?? "";
}

function operationRoute(operation: string): string {
  const match = /^[A-Z]+\s+(\/\S*)/.exec(operation.trim());
  return match?.[1] ?? "";
}

function httpMetricsQuery(service: string, method: string, route: string): string {
  const filters = [`service\\.name="${promQLValue(service)}"`];
  if (method) filters.push(`http\\.request\\.method="${promQLValue(method)}"`);
  if (route) filters.push(`http\\.route="${promQLValue(route)}"`);
  return `sum(rate({__name__="http.server.request.duration_count",${filters.join(",")}}[5m]))`;
}

function promQLValue(value: string): string {
  return value.replaceAll("\\", "\\\\").replaceAll("\n", "\\n").replaceAll('"', '\\"');
}

function logsValue(value: string): string {
  return value.replaceAll("\\", "\\\\").replaceAll('"', '\\"');
}

function absoluteWindow(search: Record<string, unknown>): { start?: number; end?: number } {
  const start = finiteInteger(search.start);
  const end = finiteInteger(search.end);
  return start !== null && end !== null && start >= 0 && end > start
    ? { start, end }
    : {};
}

function finiteInteger(value: unknown): number | null {
  const parsed = typeof value === "number"
    ? value
    : typeof value === "string" && /^\d+$/.test(value)
      ? Number(value)
      : Number.NaN;
  return Number.isSafeInteger(parsed) ? parsed : null;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function flag(value: unknown): "1" | undefined {
  return value === true || value === 1 || value === "1" || value === "true" ? "1" : undefined;
}

function compact<T extends object>(value: T): T {
  return Object.fromEntries(Object.entries(value).filter(([, item]) => item !== undefined)) as T;
}
