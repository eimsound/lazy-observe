import type { ErrorEventPayload, VitalName } from "./types";
import type { ParsedDsn } from "./dsn";
import type { RecordedSpan, SpanAttribute } from "./trace";
import {
  DEFAULT_REQUEST_TIMEOUT_MS,
  isServerRejection,
  MAX_KEEPALIVE_BYTES,
  MAX_RETRY_AFTER_MS,
  parseRetryAfterMs,
  TransportError,
} from "./delivery";

export {
  isServerRejection,
  MAX_KEEPALIVE_BYTES,
  MAX_RETRY_AFTER_MS,
  parseRetryAfterMs,
  TransportError,
};

export interface VitalPayload { name: VitalName; value: number; page: string; timestamp: string }
export type DiagnosticSignal = "error" | "trace" | "vitals" | "replay" | "diagnostic";
/**
 * Why a telemetry signal was dropped. The first three describe the SDK's own bounded
 * delivery queues; `server_rejected` describes a 4xx ingest refusal that retrying cannot fix
 * (408 and 429 are transient and report `retry_exhausted` instead).
 */
export type DiagnosticReason =
  | "queue_full"
  | "retry_exhausted"
  | "shutdown_budget"
  | "server_rejected";

export interface DiagnosticMeasurement {
  measurement_id: string;
  timestamp: string;
  service: string;
  signal: DiagnosticSignal;
  reason: DiagnosticReason;
  dropped: number;
}

export const DEFAULT_TRANSPORT_TIMEOUT_MS: number = DEFAULT_REQUEST_TIMEOUT_MS;

function bodyByteLength(body: BodyInit): number {
  if (typeof body === "string") return new TextEncoder().encode(body).byteLength;
  if (typeof Blob !== "undefined" && body instanceof Blob) return body.size;
  if (ArrayBuffer.isView(body)) return body.byteLength;
  if (body instanceof ArrayBuffer) return body.byteLength;
  return 0;
}

export class Transport {
  constructor(
    private readonly dsn: ParsedDsn,
    private readonly fetcher: typeof fetch | undefined,
    private readonly timeoutMs: number = DEFAULT_TRANSPORT_TIMEOUT_MS,
  ) {}
  private async post(
    path: string,
    body: BodyInit,
    extra: Record<string, string> = {},
    keepalive = false,
    externalSignal?: AbortSignal,
  ): Promise<void> {
    if (!this.fetcher) throw new Error("fetch is unavailable");
    const effectiveKeepalive = keepalive && bodyByteLength(body) <= MAX_KEEPALIVE_BYTES;
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let signal: AbortSignal | undefined;
    if (!effectiveKeepalive) {
      if (externalSignal) {
        signal = externalSignal;
      } else if (this.timeoutMs > 0 && typeof AbortController !== "undefined") {
        controller = new AbortController();
        timer = setTimeout(() => controller?.abort(), this.timeoutMs);
        signal = controller.signal;
      }
    }
    try {
      const init: RequestInit = {
        method: "POST",
        headers: { Authorization: `Bearer ${this.dsn.token}`, ...extra },
        body,
        keepalive: effectiveKeepalive,
        ...(signal ? { signal } : {}),
      };
      const response = await this.fetcher(`${this.dsn.endpoint}${path}`, init);
      if (!response.ok) {
        const retryAfterMs = parseRetryAfterMs(response.headers?.get?.("retry-after"));
        throw new TransportError(response.status, `lazy-observe transport returned ${response.status}`, retryAfterMs);
      }
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }
  errors(events: ErrorEventPayload[], keepalive = false, signal?: AbortSignal): Promise<void> {
    return this.post("/api/events/errors", JSON.stringify({ events }), { "Content-Type": "application/json" }, keepalive, signal);
  }
  traces(serviceName: string, spans: RecordedSpan[], keepalive = false, signal?: AbortSignal): Promise<void> {
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
    return this.post("/v1/traces", JSON.stringify(payload), { "Content-Type": "application/json" }, keepalive, signal);
  }
  vitals(vitals: VitalPayload[], keepalive = true, signal?: AbortSignal): Promise<void> {
    return this.post("/api/events/vitals", JSON.stringify({ vitals }), { "Content-Type": "application/json" }, keepalive, signal);
  }
  diagnostics(measurements: DiagnosticMeasurement[], keepalive = false, signal?: AbortSignal): Promise<void> {
    return this.post(
      "/api/events/diagnostics",
      JSON.stringify({ measurements }),
      { "Content-Type": "application/json" },
      keepalive,
      signal,
    );
  }
  replay(
    sessionId: string,
    seq: number,
    body: BodyInit,
    compressed: boolean,
    error: boolean,
    keepalive = false,
    signal?: AbortSignal,
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
      signal,
    );
  }
}

function otlpAttribute(key: string, value: SpanAttribute) {
  if (typeof value === "boolean") return { key, value: { boolValue: value } };
  if (typeof value === "number") return { key, value: { intValue: String(Math.trunc(value)) } };
  return { key, value: { stringValue: value } };
}
