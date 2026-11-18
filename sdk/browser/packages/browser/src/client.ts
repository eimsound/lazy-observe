import { parseDsn } from "./dsn";
import { DeliveryQueue } from "./delivery";
import { MinuteRateLimiter } from "./rate-limit";
import { ReplayController, replayDefaults, replayRequestMethod } from "./replay";
import { parseStack } from "./stack";
import {
  createTraceContext,
  parseTraceparent,
  shouldTrace,
  traceparent,
  TraceWindow,
  unixNanoAfter,
  unixNanoNow,
} from "./trace";
import type { RecordedSpan } from "./trace";
import { isServerRejection, Transport } from "./transport";
import type { DiagnosticMeasurement, DiagnosticReason, DiagnosticSignal } from "./transport";
import type {
  ActiveSpan,
  BrowserClient,
  BrowserConfig,
  CaptureOptions,
  ErrorEventPayload,
  StartSpanOptions,
  TraceTarget,
} from "./types";
import { eventId, getSessionId, normalizePage, randomHex } from "./utils";
import { startVitals } from "./vitals";

const XHR_URL = Symbol("lazy-observe:xhr-url");
const XHR_METHOD = Symbol("lazy-observe:xhr-method");
const XHR_DONE = Symbol("lazy-observe:xhr-done");

const DEFAULT_ERROR_QUEUE_CAPACITY = 100;
const DEFAULT_TRACE_QUEUE_CAPACITY = 1_000;
const DEFAULT_REPLAY_QUEUE_CAPACITY = 10;
const DEFAULT_DIAGNOSTIC_QUEUE_CAPACITY = 20;
const DEFAULT_DELIVERY_MAX_ATTEMPTS = 3;
const DEFAULT_DELIVERY_RETRY_INITIAL_MS = 100;
const DEFAULT_DELIVERY_RETRY_MAX_MS = 2_000;

function monotonicNow(): number {
  return typeof performance !== "undefined" && typeof performance.now === "function" ? performance.now() : Date.now();
}

function browserContext(): ErrorEventPayload["contexts"] {
  if (typeof navigator === "undefined") return undefined;
  const ua = navigator.userAgent;
  const browser = ua.match(/(Firefox|Edg|Chrome|Version)\/([\d.]+)/);
  const os = ua.match(/(Windows NT|Android|iPhone OS|Mac OS X)\s?([\d_\.]*)/);
  return {
    browser: browser ? { name: browser[1] === "Version" ? "Safari" : browser[1], version: browser[2] } : { name: "unknown" },
    os: os ? { name: os[1] } : undefined
  };
}

export class Client implements BrowserClient {
  readonly sessionId: string;
  private readonly transport: Transport;
  private readonly limiter: MinuteRateLimiter;
  private readonly errorDelivery: DeliveryQueue<ErrorEventPayload>;
  private readonly traceDelivery: DeliveryQueue<RecordedSpan>;
  private readonly diagnosticDelivery: DeliveryQueue<DiagnosticMeasurement>;
  private readonly traces = new TraceWindow();
  private readonly allowlist: TraceTarget[];
  private readonly serviceName: string;
  private readonly nativeFetch?: typeof fetch;
  private replay?: ReplayController;
  private droppedTelemetry = 0;
  private currentPage: string;
  private closed = false;
  private restoreFetch?: () => void;
  private restoreXhr?: () => void;
  private readonly onError = (event: ErrorEvent) => {
    const error = event.error instanceof Error ? event.error : new Error(event.message || "Script error");
    if (!error.stack && event.filename) error.stack = `${error.name}: ${error.message}\n    at <anonymous> (${event.filename}:${event.lineno}:${event.colno})`;
    this.captureException(error);
  };
  private readonly onRejection = (event: PromiseRejectionEvent) => {
    this.captureException(event.reason instanceof Error ? event.reason : new Error(String(event.reason)));
  };
  private readonly onUnload = () => { void this.flush(true); };

  constructor(private readonly config: BrowserConfig) {
    const dsn = parseDsn(config.dsn);
    this.nativeFetch = typeof window !== "undefined" && typeof window.fetch === "function" ? window.fetch.bind(window) : globalThis.fetch?.bind(globalThis);
    this.transport = new Transport(dsn, this.nativeFetch);
    this.limiter = new MinuteRateLimiter(config.errorLimitPerMinute ?? 60);
    this.sessionId = getSessionId(typeof sessionStorage === "undefined" ? undefined : sessionStorage);
    this.currentPage = normalizePage(typeof location === "undefined" ? "/" : location.href);
    this.allowlist = typeof config.fetchTracing === "object" ? config.fetchTracing.allowlist ?? [] : [];
    this.serviceName = config.serviceName?.trim()
      || (typeof location === "undefined" ? "browser" : location.hostname || "browser");
    const delivery = config.delivery ?? {};
    const maxAttempts = delivery.maxAttempts ?? DEFAULT_DELIVERY_MAX_ATTEMPTS;
    const retryInitialMs = delivery.retryInitialMs ?? DEFAULT_DELIVERY_RETRY_INITIAL_MS;
    const retryMaxMs = delivery.retryMaxMs ?? DEFAULT_DELIVERY_RETRY_MAX_MS;
    this.diagnosticDelivery = new DeliveryQueue({
      capacity: delivery.diagnosticQueueCapacity ?? DEFAULT_DIAGNOSTIC_QUEUE_CAPACITY,
      batchSize: 20,
      flushIntervalMs: 100,
      maxAttempts,
      retryInitialMs,
      retryMaxMs,
      send: (measurements, options) => this.transport.diagnostics(measurements, options.keepalive),
      onDrop: (drop) => this.recordTelemetryDrop("diagnostic", drop, false),
    });
    this.errorDelivery = new DeliveryQueue({
      capacity: delivery.errorQueueCapacity ?? DEFAULT_ERROR_QUEUE_CAPACITY,
      batchSize: config.errorBatchSize ?? 10,
      flushIntervalMs: config.errorFlushIntervalMs ?? 5_000,
      maxAttempts,
      retryInitialMs,
      retryMaxMs,
      send: (events, options) => this.transport.errors(events, options.keepalive),
      onDrop: (drop) => this.recordTelemetryDrop("error", drop),
    });
    this.traceDelivery = new DeliveryQueue({
      capacity: delivery.traceQueueCapacity ?? DEFAULT_TRACE_QUEUE_CAPACITY,
      batchSize: 64,
      flushIntervalMs: config.errorFlushIntervalMs ?? 5_000,
      maxAttempts,
      retryInitialMs,
      retryMaxMs,
      send: (spans, options) => this.transport.traces(this.serviceName, spans, options.keepalive),
      onDrop: (drop) => this.recordTelemetryDrop("trace", drop),
    });
  }

  start(): this {
    if (typeof window === "undefined") return this;
    if (this.config.errors !== false) {
      window.addEventListener("error", this.onError);
      window.addEventListener("unhandledrejection", this.onRejection);
    }
    window.addEventListener("pagehide", this.onUnload);
    if (this.config.replay !== false) {
      this.replay = new ReplayController(
        replayDefaults(typeof this.config.replay === "object" ? this.config.replay : {}),
        this.sessionId,
        this.transport,
        {
          capacity: this.config.delivery?.replayQueueCapacity ?? DEFAULT_REPLAY_QUEUE_CAPACITY,
          maxAttempts: this.config.delivery?.maxAttempts ?? DEFAULT_DELIVERY_MAX_ATTEMPTS,
          retryInitialMs: this.config.delivery?.retryInitialMs ?? DEFAULT_DELIVERY_RETRY_INITIAL_MS,
          retryMaxMs: this.config.delivery?.retryMaxMs ?? DEFAULT_DELIVERY_RETRY_MAX_MS,
          onDrop: (drop) => this.recordTelemetryDrop("replay", drop),
        },
      );
      void this.replay.start().catch(() => { this.replay = undefined; });
    }
    if (this.config.vitals !== false) {
      startVitals(
        this.transport,
        () => this.currentPage,
        (sample) => this.replay?.vital(sample),
        (error) => this.recordVitalsFailure(error),
      );
    }
    if (this.config.fetchTracing !== false) {
      const autoPatch = typeof this.config.fetchTracing !== "object" || this.config.fetchTracing.autoPatch !== false;
      if (autoPatch) { this.patchFetch(); this.patchXhr(); }
    }
    return this;
  }

  startSpan<T>(options: StartSpanOptions, callback: (span: ActiveSpan) => T): T {
    const parent = this.traces.currentCallback();
    const span = { name: options.name, context: createTraceContext(parent) };
    const started = unixNanoNow();
    const finish = (error?: unknown) => this.recordSpan({
      context: span.context,
      parentSpanId: parent?.span_id,
      name: span.name,
      kind: 1,
      startTimeUnixNano: started,
      endTimeUnixNano: unixNanoAfter(started),
      status: error === undefined ? { code: 1 } : { code: 2, message: errorMessage(error) },
    });
    try {
      const result = this.traces.run(span.context, () => callback(span));
      if (result instanceof Promise) {
        return result.then(
          (value) => { finish(); return value; },
          (error) => { finish(error); throw error; },
        ) as T;
      }
      finish();
      return result;
    } catch (error) {
      finish(error);
      throw error;
    }
  }

  captureException(value: unknown, options: CaptureOptions = {}): string | undefined {
    if (this.closed || !this.limiter.take()) return undefined;
    const error = value instanceof Error ? value : new Error(String(value));
    const trace = options.trace ?? this.traces.current();
    const contexts = browserContext() ?? {};
    if (trace) contexts.trace = trace;
    let payload: ErrorEventPayload = {
      event_id: eventId(), timestamp: new Date().toISOString(), platform: "javascript",
      release: this.config.release, environment: this.config.environment,
      level: options.level ?? "error", message: error.message,
      exception: { type: error.name || "Error", value: error.message, stacktrace: { frames: parseStack(error.stack) } },
      tags: options.tags, contexts: Object.keys(contexts).length ? contexts : undefined,
      session_id: this.sessionId, url: typeof location === "undefined" ? undefined : location.href
    };
    if (this.config.beforeSend) {
      try { const changed = this.config.beforeSend(payload); if (!changed) return undefined; payload = changed; }
      catch { return undefined; }
    }
    this.errorDelivery.enqueue(payload);
    this.replay?.error(payload);
    return payload.event_id;
  }

  wrapFetch(fetchImpl: typeof fetch = this.nativeFetch as typeof fetch): typeof fetch {
    if (!fetchImpl) throw new Error("fetch is unavailable");
    return (async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = replayRequestMethod(input, init);
      const started = monotonicNow();
      let status = 0;
      let requestError: unknown;
      let requestInit = init;
      let requestSpan: Omit<RecordedSpan, "endTimeUnixNano" | "status"> | undefined;
      if (shouldTrace(input, this.allowlist)) {
        const headers = new Headers(typeof Request !== "undefined" && input instanceof Request ? input.headers : undefined);
        new Headers(init?.headers).forEach((value, key) => headers.set(key, value));
        const existing = parseTraceparent(headers.get("traceparent") ?? "");
        const parent = this.traces.current();
        const context = existing ?? createTraceContext(parent);
        if (!existing) {
          headers.set("traceparent", traceparent(context));
          requestSpan = {
            context,
            parentSpanId: parent?.span_id,
            name: `${method.toUpperCase()} ${requestPath(input)}`,
            kind: 3,
            startTimeUnixNano: unixNanoNow(),
            attributes: {
              "http.request.method": method.toUpperCase(),
              "url.path": requestPath(input),
            },
          };
        }
        requestInit = { ...init, headers };
      }
      try {
        const response = await fetchImpl(input, requestInit);
        status = response.status;
        return response;
      } catch (error) {
        requestError = error;
        throw error;
      } finally {
        if (requestSpan) {
          this.recordSpan({
            ...requestSpan,
            endTimeUnixNano: unixNanoAfter(requestSpan.startTimeUnixNano),
            attributes: {
              ...requestSpan.attributes,
              ...(status > 0 ? { "http.response.status_code": status } : {}),
            },
            status: requestError !== undefined || status === 0 || status >= 500
              ? { code: 2, ...(requestError === undefined ? {} : { message: errorMessage(requestError) }) }
              : { code: 1 },
          });
        }
        this.replay?.http(method, input, status, monotonicNow() - started);
      }
    }) as typeof fetch;
  }

  private patchFetch(): void {
    if (!this.nativeFetch) return;
    const previous = window.fetch;
    window.fetch = this.wrapFetch(this.nativeFetch);
    this.restoreFetch = () => { window.fetch = previous; };
  }

  private patchXhr(): void {
    if (typeof XMLHttpRequest === "undefined") return;
    const prototype = XMLHttpRequest.prototype as XMLHttpRequest & Record<symbol, unknown>;
    const originalOpen = prototype.open;
    const originalSend = prototype.send;
    const client = this;
    prototype.open = function(this: XMLHttpRequest, method: string, url: string | URL, ...rest: unknown[]) {
      (this as unknown as Record<symbol, unknown>)[XHR_URL] = String(url);
      (this as unknown as Record<symbol, unknown>)[XHR_METHOD] = method;
      delete (this as unknown as Record<symbol, unknown>)[XHR_DONE];
      return originalOpen.call(this, method, url, ...(rest as [boolean, string?, string?]));
    } as typeof XMLHttpRequest.prototype.open;
    prototype.send = function(body?: Document | XMLHttpRequestBodyInit | null) {
      const xhr = this as unknown as XMLHttpRequest & Record<symbol, unknown>;
      const url = String(xhr[XHR_URL] ?? "");
      const method = String(xhr[XHR_METHOD] ?? "GET");
      const started = monotonicNow();
      let requestSpan: Omit<RecordedSpan, "endTimeUnixNano" | "status"> | undefined;
      if (shouldTrace(url, client.allowlist)) {
        const parent = client.traces.current();
        const context = createTraceContext(parent);
        try {
          this.setRequestHeader("traceparent", traceparent(context));
          requestSpan = {
            context,
            parentSpanId: parent?.span_id,
            name: `${method.toUpperCase()} ${requestPath(url)}`,
            kind: 3,
            startTimeUnixNano: unixNanoNow(),
            attributes: { "http.request.method": method.toUpperCase(), "url.path": requestPath(url) },
          };
        } catch { /* request may already be sending */ }
      }
      const done = () => {
        if (xhr[XHR_DONE]) return;
        xhr[XHR_DONE] = true;
        let status = 0;
        try { status = this.status; } catch { /* network or aborted request */ }
        if (requestSpan) {
          client.recordSpan({
            ...requestSpan,
            endTimeUnixNano: unixNanoAfter(requestSpan.startTimeUnixNano),
            attributes: {
              ...requestSpan.attributes,
              ...(status > 0 ? { "http.response.status_code": status } : {}),
            },
            status: status === 0 || status >= 500 ? { code: 2 } : { code: 1 },
          });
        }
        client.replay?.http(method, url, status, monotonicNow() - started);
      };
      this.addEventListener("loadend", done, { once: true });
      try { return originalSend.call(this, body); } catch (error) { done(); throw error; }
    };
    this.restoreXhr = () => { prototype.open = originalOpen; prototype.send = originalSend; };
  }

  setPage(page: string): void { this.currentPage = normalizePage(page); this.replay?.page(this.currentPage); }

  diagnostics(): { droppedTelemetry: number } {
    return { droppedTelemetry: this.droppedTelemetry };
  }

  async flush(keepalive = false): Promise<void> {
    await Promise.all([
      this.errorDelivery.flush(keepalive),
      this.traceDelivery.flush(keepalive),
      this.replay?.flush(keepalive),
    ]);
    await this.diagnosticDelivery.flush(keepalive);
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    if (typeof window !== "undefined") {
      window.removeEventListener("error", this.onError);
      window.removeEventListener("unhandledrejection", this.onRejection);
      window.removeEventListener("pagehide", this.onUnload);
    }
    this.restoreFetch?.(); this.restoreXhr?.();
    await Promise.all([
      this.errorDelivery.close(),
      this.traceDelivery.close(),
      this.replay?.close(),
    ]);
    await this.diagnosticDelivery.close();
  }

  private recordSpan(span: RecordedSpan): void {
    if (!this.closed) this.traceDelivery.enqueue(span);
  }

  private recordVitalsFailure(error: unknown): void {
    // A 4xx is the server refusing this exact payload, so no retry can help. Everything
    // else (network failure, 5xx, missing fetch) exhausts the single attempt a vital has,
    // which is the same outcome the delivery queues report as retry_exhausted.
    this.recordTelemetryDrop("vitals", {
      reason: isServerRejection(error) ? "server_rejected" : "retry_exhausted",
      count: 1,
      error,
    });
  }

  private recordTelemetryDrop(
    signal: DiagnosticSignal,
    drop: { reason: DiagnosticReason; count: number; error?: unknown },
    exportDiagnostic = true,
  ): void {
    this.droppedTelemetry += drop.count;
    console.warn(
      `[lazy-observe] ${signal} telemetry dropped`,
      { reason: drop.reason, count: drop.count, error: drop.error },
    );
    if (!exportDiagnostic) return;
    this.diagnosticDelivery.enqueue({
      measurement_id: randomHex(16),
      timestamp: new Date().toISOString(),
      service: this.serviceName,
      signal,
      reason: drop.reason,
      dropped: drop.count,
    });
  }
}

function requestPath(input: RequestInfo | URL | string): string {
  const raw = typeof Request !== "undefined" && input instanceof Request ? input.url : String(input);
  try {
    const base = typeof location === "undefined" ? "http://browser.invalid/" : location.href;
    return new URL(raw, base).pathname || "/";
  } catch {
    return "/";
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
