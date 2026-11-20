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
import { eventId, formatNonError, getSessionId, normalizePage, randomHex, safeSessionStorage, sanitizeUrl } from "./utils";
import { startVitals } from "./vitals";

const XHR_URL = Symbol("lazy-observe:xhr-url");
const XHR_METHOD = Symbol("lazy-observe:xhr-method");
const XHR_DONE = Symbol("lazy-observe:xhr-done");
const XHR_HAS_TRACEPARENT = Symbol("lazy-observe:xhr-has-traceparent");

const MAX_ERROR_EVENT_BYTES = 200 * 1024;
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
  private disposeVitals?: () => void;
  private readonly onError = (event: ErrorEvent) => {
    if (event.error instanceof Error) {
      if (!event.error.stack && event.filename) {
        event.error.stack = syntheticStack(
          event.error.name || "Error",
          event.error.message,
          event.filename,
          event.lineno,
          event.colno,
        );
      }
      this.captureException(event.error);
      return;
    }
    const message = event.message || (event.error != null ? formatNonError(event.error) : "Script error");
    const error = new Error(message);
    if (event.filename) {
      error.stack = syntheticStack(error.name, error.message, event.filename, event.lineno, event.colno);
    }
    this.captureException(error);
  };
  private readonly onRejection = (event: PromiseRejectionEvent) => {
    this.captureException(event.reason instanceof Error ? event.reason : new Error(formatNonError(event.reason)));
  };
  private readonly onUnload = () => { void this.flush(true); };
  private readonly onVisibilityChange = () => {
    if (typeof document !== "undefined" && document.visibilityState === "hidden") void this.flush(true);
  };

  constructor(private readonly config: BrowserConfig) {
    const dsn = parseDsn(config.dsn);
    this.nativeFetch = typeof window !== "undefined" && typeof window.fetch === "function" ? window.fetch.bind(window) : globalThis.fetch?.bind(globalThis);
    this.transport = new Transport(dsn, this.nativeFetch);
    this.limiter = new MinuteRateLimiter(config.errorLimitPerMinute ?? 60);
    this.sessionId = getSessionId(safeSessionStorage());
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
    if (typeof document !== "undefined") {
      document.addEventListener("visibilitychange", this.onVisibilityChange);
    }
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
          storage: safeSessionStorage(),
          onDrop: (drop) => this.recordTelemetryDrop("replay", drop),
        },
      );
      void this.replay.start().catch(() => { this.replay = undefined; });
    }
    if (this.config.vitals !== false) {
      this.disposeVitals = startVitals(
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
    const error = value instanceof Error ? value : new Error(formatNonError(value));
    const trace = options.trace ?? this.traces.current();
    const contexts = browserContext() ?? {};
    if (trace) contexts.trace = trace;
    let payload: ErrorEventPayload = {
      event_id: eventId(), timestamp: new Date().toISOString(), platform: "javascript",
      release: this.config.release, environment: this.config.environment,
      level: options.level ?? "error", message: error.message,
      exception: { type: error.name || "Error", value: error.message, stacktrace: { frames: parseStack(error.stack) } },
      tags: options.tags, contexts: Object.keys(contexts).length ? contexts : undefined,
      session_id: this.sessionId, url: typeof location === "undefined" ? undefined : sanitizeUrl(location.href)
    };
    if (this.config.beforeSend) {
      try { const changed = this.config.beforeSend(payload); if (!changed) return undefined; payload = changed; }
      catch { return undefined; }
    }
    const trimmed = trimErrorEventToBudget(payload, MAX_ERROR_EVENT_BYTES);
    if (!trimmed) {
      this.recordTelemetryDrop("error", { reason: "server_rejected", count: 1 });
      return undefined;
    }
    payload = trimmed;
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
    const traced = this.wrapFetch(this.nativeFetch);
    let active = true;
    const wrapped = ((input: RequestInfo | URL, init?: RequestInit) => (
      active ? traced(input, init) : previous.call(window, input, init)
    )) as typeof fetch;
    window.fetch = wrapped;
    this.restoreFetch = () => {
      active = false;
      if (window.fetch === wrapped) window.fetch = previous;
    };
  }

  private patchXhr(): void {
    if (typeof XMLHttpRequest === "undefined") return;
    const prototype = XMLHttpRequest.prototype as XMLHttpRequest & Record<symbol, unknown>;
    const originalOpen = prototype.open;
    const originalSend = prototype.send;
    const originalSetRequestHeader = prototype.setRequestHeader;
    const client = this;
    let active = true;

    const wrappedOpen = function(this: XMLHttpRequest, method: string, url: string | URL, ...rest: unknown[]) {
      if (active) {
        const xhr = this as unknown as Record<symbol, unknown>;
        xhr[XHR_URL] = String(url);
        xhr[XHR_METHOD] = method;
        delete xhr[XHR_DONE];
        delete xhr[XHR_HAS_TRACEPARENT];
      }
      return originalOpen.call(this, method, url, ...(rest as [boolean, string?, string?]));
    } as typeof XMLHttpRequest.prototype.open;

    const wrappedSetRequestHeader = function(this: XMLHttpRequest, name: string, value: string) {
      if (active && String(name).toLowerCase() === "traceparent") {
        (this as unknown as Record<symbol, unknown>)[XHR_HAS_TRACEPARENT] = true;
      }
      return originalSetRequestHeader.call(this, name, value);
    };

    const wrappedSend = function(this: XMLHttpRequest, body?: Document | XMLHttpRequestBodyInit | null) {
      if (!active) return originalSend.call(this, body);
      const xhr = this as unknown as XMLHttpRequest & Record<symbol, unknown>;
      const url = String(xhr[XHR_URL] ?? "");
      const method = String(xhr[XHR_METHOD] ?? "GET");
      const started = monotonicNow();
      let requestSpan: Omit<RecordedSpan, "endTimeUnixNano" | "status"> | undefined;
      if (shouldTrace(url, client.allowlist) && !xhr[XHR_HAS_TRACEPARENT]) {
        const parent = client.traces.current();
        const context = createTraceContext(parent);
        try {
          originalSetRequestHeader.call(this, "traceparent", traceparent(context));
          xhr[XHR_HAS_TRACEPARENT] = true;
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

    prototype.open = wrappedOpen;
    prototype.setRequestHeader = wrappedSetRequestHeader;
    prototype.send = wrappedSend;
    this.restoreXhr = () => {
      active = false;
      if (prototype.open === wrappedOpen) prototype.open = originalOpen;
      if (prototype.setRequestHeader === wrappedSetRequestHeader) prototype.setRequestHeader = originalSetRequestHeader;
      if (prototype.send === wrappedSend) prototype.send = originalSend;
    };
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
    if (typeof document !== "undefined") {
      document.removeEventListener("visibilitychange", this.onVisibilityChange);
    }
    this.disposeVitals?.();
    this.disposeVitals = undefined;
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
  return error instanceof Error ? error.message : formatNonError(error);
}

function syntheticStack(
  name: string,
  message: string,
  filename: string,
  lineno?: number,
  colno?: number,
): string {
  const loc = lineno && lineno > 0
    ? (colno && colno > 0 ? `${filename}:${lineno}:${colno}` : `${filename}:${lineno}`)
    : `${filename}:0:0`;
  return `${name}: ${message}\n    at <anonymous> (${loc})`;
}

function jsonByteLength(value: unknown): number {
  return new TextEncoder().encode(JSON.stringify(value)).byteLength;
}

function truncateText(value: string, maxChars: number): string {
  return value.length > maxChars ? `${value.slice(0, maxChars)}...[truncated]` : value;
}

export function trimErrorEventToBudget(
  payload: ErrorEventPayload,
  maxBytes: number = MAX_ERROR_EVENT_BYTES,
): ErrorEventPayload | undefined {
  if (jsonByteLength(payload) <= maxBytes) return payload;
  const trimmed: ErrorEventPayload = {
    ...payload,
    exception: {
      ...payload.exception,
      stacktrace: { frames: payload.exception.stacktrace.frames.map((frame) => ({ ...frame })) },
    },
    ...(payload.tags ? { tags: { ...payload.tags } } : {}),
  };

  const fits = () => jsonByteLength(trimmed) <= maxBytes;

  if (trimmed.tags) {
    for (const [key, value] of Object.entries(trimmed.tags)) {
      if (value.length > 4_096) trimmed.tags[key] = truncateText(value, 4_096);
    }
    if (fits()) return trimmed;
  }

  if (trimmed.message) trimmed.message = truncateText(trimmed.message, 8_192);
  trimmed.exception.value = truncateText(trimmed.exception.value, 8_192);
  if (trimmed.url) trimmed.url = truncateText(trimmed.url, 2_048);
  if (fits()) return trimmed;

  if (trimmed.tags) {
    for (const [key, value] of Object.entries(trimmed.tags)) {
      if (value.length > 512) trimmed.tags[key] = truncateText(value, 512);
    }
    if (fits()) return trimmed;
    delete trimmed.tags;
    if (fits()) return trimmed;
  }

  if (trimmed.exception.stacktrace.frames.length > 50) {
    trimmed.exception.stacktrace.frames = trimmed.exception.stacktrace.frames.slice(-50);
    if (fits()) return trimmed;
  }

  trimmed.message = trimmed.message ? truncateText(trimmed.message, 1_024) : trimmed.message;
  trimmed.exception.value = truncateText(trimmed.exception.value, 1_024);
  for (const frame of trimmed.exception.stacktrace.frames) {
    frame.filename = truncateText(frame.filename, 512);
    frame.function = truncateText(frame.function, 256);
  }
  if (fits()) return trimmed;

  trimmed.exception.stacktrace.frames = trimmed.exception.stacktrace.frames.slice(-10);
  if (fits()) return trimmed;

  trimmed.exception.stacktrace.frames = [];
  return fits() ? trimmed : undefined;
}
