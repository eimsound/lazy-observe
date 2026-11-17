import type { ErrorEventPayload, VitalName } from "./types";
import type { ParsedDsn } from "./dsn";
import type { RecordedSpan, SpanAttribute } from "./trace";

export interface VitalPayload { name: VitalName; value: number; page: string; timestamp: string }
export type DiagnosticSignal = "error" | "trace" | "replay" | "diagnostic";
export type DiagnosticReason = "queue_full" | "retry_exhausted" | "shutdown_budget";
export interface DiagnosticMeasurement {
  measurement_id: string;
  timestamp: string;
  service: string;
  signal: DiagnosticSignal;
  reason: DiagnosticReason;
  dropped: number;
}

export class Transport {
  constructor(private readonly dsn: ParsedDsn, private readonly fetcher: typeof fetch | undefined) {}
  private async post(path: string, body: BodyInit, extra: Record<string, string> = {}, keepalive = false): Promise<void> {
    if (!this.fetcher) throw new Error("fetch is unavailable");
    const response = await this.fetcher(`${this.dsn.endpoint}${path}`, {
      method: "POST",
      headers: { Authorization: `Bearer ${this.dsn.token}`, ...extra },
      body,
      keepalive
    });
    if (!response.ok) throw new Error(`lazy-observe transport returned ${response.status}`);
  }
  errors(events: ErrorEventPayload[], keepalive = false): Promise<void> {
    return this.post("/api/events/errors", JSON.stringify({ events }), { "Content-Type": "application/json" }, keepalive);
  }
  traces(serviceName: string, spans: RecordedSpan[], keepalive = false): Promise<void> {
    const payload = {
      resourceSpans: [{
        resource: {
          attributes: [
            otlpAttribute("service.name", serviceName),
            otlpAttribute("telemetry.sdk.language", "webjs"),
            otlpAttribute("telemetry.sdk.name", "@lazy-observe/browser"),
          ],
        },
        scopeSpans: [{
          scope: { name: "@lazy-observe/browser" },
          spans: spans.map((span) => ({
            traceId: span.context.trace_id,
            spanId: span.context.span_id,
            ...(span.parentSpanId ? { parentSpanId: span.parentSpanId } : {}),
            name: span.name,
            kind: span.kind,
            startTimeUnixNano: span.startTimeUnixNano,
            endTimeUnixNano: span.endTimeUnixNano,
            attributes: Object.entries(span.attributes ?? {}).map(([key, value]) => otlpAttribute(key, value)),
            status: span.status,
          })),
        }],
      }],
    };
    return this.post("/v1/traces", JSON.stringify(payload), { "Content-Type": "application/json" }, keepalive);
  }
  vitals(vitals: VitalPayload[]): Promise<void> {
    return this.post("/api/events/vitals", JSON.stringify({ vitals }), { "Content-Type": "application/json" });
  }
  diagnostics(measurements: DiagnosticMeasurement[], keepalive = false): Promise<void> {
    return this.post(
      "/api/events/diagnostics",
      JSON.stringify({ measurements }),
      { "Content-Type": "application/json" },
      keepalive,
    );
  }
  replay(
    sessionId: string,
    seq: number,
    body: BodyInit,
    compressed: boolean,
    error: boolean,
    keepalive = false,
  ): Promise<void> {
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (compressed) headers["Content-Encoding"] = "gzip";
    else headers["X-LO-Replay-Compression"] = "identity";
    if (error) headers["X-LO-Replay-Error"] = "1";
    return this.post(
      `/api/replay/segments?session_id=${encodeURIComponent(sessionId)}&seq=${seq}`,
      body,
      headers,
      keepalive,
    );
  }
}

function otlpAttribute(key: string, value: SpanAttribute) {
  if (typeof value === "boolean") return { key, value: { boolValue: value } };
  if (typeof value === "number") return { key, value: { intValue: String(Math.trunc(value)) } };
  return { key, value: { stringValue: value } };
}
