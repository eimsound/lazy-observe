import type { ErrorEventPayload, VitalName } from "./types";
import type { ParsedDsn } from "./dsn";
import type { RecordedSpan, SpanAttribute } from "./trace";

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

export const MAX_KEEPALIVE_BYTES: number = 60 * 1024;
export const DEFAULT_TRANSPORT_TIMEOUT_MS: number = 10_000;
export const MAX_RETRY_AFTER_MS: number = 30_000;

/** Parse an HTTP Retry-After header (delta-seconds or HTTP-date) into milliseconds. */
export function parseRetryAfterMs(header: string | null | undefined, now = Date.now()): number | undefined {
  const raw = header?.trim();
  if (!raw) return undefined;
  if (/^\d+(?:\.\d+)?$/.test(raw)) {
    const seconds = Number(raw);
    return Number.isFinite(seconds) ? Math.max(0, Math.round(seconds * 1_000)) : undefined;
  }
  const timestamp = Date.parse(raw);
  if (Number.isFinite(timestamp)) return Math.max(0, timestamp - now);
  return undefined;
}

/** A non-2xx ingest response, so callers can tell a refusal from a network failure. */
export class TransportError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly retryAfterMs?: number,
  ) {
    super(message);
    this.name = "TransportError";
  }
}

/**
 * A 4xx (except 408 and 429) means the server will never accept this payload as-is: retrying the
 * same body is pointless. 408 (request timeout) and 429 (rate limited) are transient — the same
 * payload may well be accepted later — so they are treated like a network error instead.
 */
export function isServerRejection(error: unknown): boolean {
  if (!(error instanceof TransportError)) return false;
  const { status } = error;
  if (status === 408 || status === 429) return false;
  return status >= 400 && status < 500;
}

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
  private async post(path: string, body: BodyInit, extra: Record<string, string> = {}, keepalive = false): Promise<void> {
    if (!this.fetcher) throw new Error("fetch is unavailable");
    const effectiveKeepalive = keepalive && bodyByteLength(body) <= MAX_KEEPALIVE_BYTES;
    let controller: AbortController | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    if (!effectiveKeepalive && this.timeoutMs > 0 && typeof AbortController !== "undefined") {
      controller = new AbortController();
      timer = setTimeout(() => controller?.abort(), this.timeoutMs);
    }
    try {
      const init: RequestInit = {
        method: "POST",
        headers: { Authorization: `Bearer ${this.dsn.token}`, ...extra },
        body,
        keepalive: effectiveKeepalive,
        ...(controller ? { signal: controller.signal } : {}),
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
  vitals(vitals: VitalPayload[], keepalive = true): Promise<void> {
    return this.post("/api/events/vitals", JSON.stringify({ vitals }), { "Content-Type": "application/json" }, keepalive);
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
