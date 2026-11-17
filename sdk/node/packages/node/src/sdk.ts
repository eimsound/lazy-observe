import process from "node:process";
import { context, ProxyTracerProvider, ROOT_CONTEXT, SpanKind, SpanStatusCode, trace, type Attributes, type Context, type Span, type SpanContext } from "@opentelemetry/api";
import { SeverityNumber, type AnyValueMap } from "@opentelemetry/api-logs";
import { AsyncLocalStorageContextManager } from "@opentelemetry/context-async-hooks";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { OTLPLogExporter } from "@opentelemetry/exporter-logs-otlp-proto";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-proto";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { BatchLogRecordProcessor, LoggerProvider } from "@opentelemetry/sdk-logs";
import { MeterProvider, PeriodicExportingMetricReader } from "@opentelemetry/sdk-metrics";
import { BatchSpanProcessor, NodeTracerProvider, ParentBasedSampler, TraceIdRatioBasedSampler } from "@opentelemetry/sdk-trace-node";
import { ATTR_SERVICE_NAME, ATTR_SERVICE_VERSION } from "@opentelemetry/semantic-conventions";
import pinoFactory, { type Logger as PinoLogger, type LoggerOptions as PinoOptions } from "pino";
import TransportStream from "winston-transport";
import type { ErrorRequestHandler, Request, RequestHandler, Response } from "express";
import { errorEvent, type ErrorEvent } from "./errors.js";
import { parseDSN } from "./dsn.js";
import { createHTTPServerRequestDurationHistogram } from "./http-metrics.js";
import { registerRuntimeMetrics } from "./runtime.js";
import { resolveProfilingConfig, startProfiling } from "./profiling.js";
import { createWrappedLLMFetch } from "./llm.js";
import { DeliveryQueue, type DeliveryDrop } from "./delivery.js";

const instrumentationName = "@lazy-observe/node";

const OTEL_TRACE_QUEUE_SIZE = 2_048;
const OTEL_TRACE_BATCH_SIZE = 512;
const OTEL_LOG_QUEUE_SIZE = 2_048;
const OTEL_LOG_BATCH_SIZE = 512;
const ERROR_QUEUE_CAPACITY = 100;
const ERROR_BATCH_SIZE = 20;
const ERROR_FLUSH_INTERVAL_MS = 100;
const ERROR_SEND_TIMEOUT_MS = 5_000;
const PROFILE_QUEUE_CAPACITY = 6;
const DELIVERY_MAX_ATTEMPTS = 3;
const DELIVERY_RETRY_INITIAL_MS = 100;
const DELIVERY_RETRY_MAX_MS = 2_000;
const SHUTDOWN_TIMEOUT_MS = 10_000;

export interface DeliveryConfig {
  errorQueueCapacity?: number;
  profileQueueCapacity?: number;
  maxAttempts?: number;
  retryInitialMs?: number;
  retryMaxMs?: number;
}

export interface ShutdownOptions {
  timeoutMs?: number;
}

export interface SDKDiagnostics {
  droppedTelemetry: number;
}

export interface Config {
  dsn: string;
  serviceName?: string;
  serviceVersion?: string;
  metrics?: boolean;
  logs?: boolean;
  traces?: boolean;
  sampleRate?: number;
  teeToStdout?: boolean;
  exportIntervalMs?: number;
  runtimeMetricsIntervalMs?: number;
  captureUnhandledErrors?: boolean;
  profiling?: boolean;
  profilingInterval?: number;
  release?: string;
  environment?: string;
  tags?: Record<string, string>;
  /** Capture prompt/completion content up to 4 KiB. Disabled by default; callers must redact sensitive data. */
  captureLLMContent?: boolean;
  delivery?: DeliveryConfig;
  shutdownTimeoutMs?: number;
}

export interface LazyObserve {
  express(): RequestHandler;
  expressError(): ErrorRequestHandler;
  pino(options?: PinoOptions): PinoLogger;
  winston(): TransportStream;
  reportError(error: unknown, options?: { url?: string; spanContext?: SpanContext }): Promise<void>;
  wrapLLMFetch(baseFetch?: typeof fetch): typeof fetch;
  diagnostics(): SDKDiagnostics;
  shutdown(options?: ShutdownOptions): Promise<void>;
  readonly signals: Readonly<{ metrics: boolean; logs: boolean; traces: boolean }>;
}

export function resolveSignals(config: Pick<Config, "metrics" | "logs" | "traces">): Readonly<{ metrics: boolean; logs: boolean; traces: boolean }> {
  return Object.freeze({ metrics: config.metrics ?? true, logs: config.logs ?? true, traces: config.traces ?? true });
}

export function extractTraceparent(headers: Record<string, string | string[] | undefined>): SpanContext | undefined {
  const extracted = new W3CTraceContextPropagator().extract(ROOT_CONTEXT, headers, {
    keys: (carrier) => Object.keys(carrier),
    get: (carrier, key) => carrier[key.toLowerCase()],
  });
  const value = trace.getSpanContext(extracted);
  return value && trace.isSpanContextValid(value) ? value : undefined;
}

interface RequestState { span: Span; context: Context; start: bigint; method: string; path: string }

function severity(level: number): { number: SeverityNumber; text: string } {
  if (level >= 60) return { number: SeverityNumber.FATAL, text: "fatal" };
  if (level >= 50) return { number: SeverityNumber.ERROR, text: "error" };
  if (level >= 40) return { number: SeverityNumber.WARN, text: "warn" };
  if (level >= 30) return { number: SeverityNumber.INFO, text: "info" };
  if (level >= 20) return { number: SeverityNumber.DEBUG, text: "debug" };
  return { number: SeverityNumber.TRACE, text: "trace" };
}

function attributeValue(value: unknown): string | number | boolean | undefined {
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") return value;
  if (value instanceof Error) return value.stack ?? value.message;
  if (value == null) return undefined;
  try { return JSON.stringify(value); } catch { return String(value); }
}

function attributes(record: Record<string, unknown>): AnyValueMap {
  const result: AnyValueMap = {};
  for (const [key, value] of Object.entries(record)) {
    const converted = attributeValue(value);
    if (converted !== undefined) result[key] = converted;
  }
  return result;
}

export function init(config: Config): LazyObserve {
  const dsn = parseDSN(config.dsn);
  const sampleRate = config.sampleRate ?? 1;
  if (sampleRate < 0 || sampleRate > 1) throw new Error("sampleRate must be between 0 and 1");
  const serviceName = config.serviceName ?? process.title ?? "unknown_service";
  const exportInterval = config.exportIntervalMs ?? 5_000;
  const signals = resolveSignals(config);
  const profiling = resolveProfilingConfig(config);
  const resource = resourceFromAttributes({
    [ATTR_SERVICE_NAME]: serviceName,
    ...(config.serviceVersion ? { [ATTR_SERVICE_VERSION]: config.serviceVersion } : {}),
    "lazy_observe.project": dsn.project,
  });
  const exporterOptions = { headers: { Authorization: `Bearer ${dsn.token}` } };
  const contextManager = new AsyncLocalStorageContextManager().enable();
  const propagator = new W3CTraceContextPropagator();

  let traceProvider: NodeTracerProvider | undefined;
  if (signals.traces) {
    traceProvider = new NodeTracerProvider({
      resource,
      sampler: new ParentBasedSampler({ root: new TraceIdRatioBasedSampler(sampleRate) }),
      spanProcessors: [new BatchSpanProcessor(
        new OTLPTraceExporter({ ...exporterOptions, url: `${dsn.endpoint}/v1/traces` }),
        { maxQueueSize: OTEL_TRACE_QUEUE_SIZE, maxExportBatchSize: OTEL_TRACE_BATCH_SIZE },
      )],
    });
  }
  const tracer = traceProvider?.getTracer(instrumentationName) ?? new ProxyTracerProvider().getTracer(instrumentationName);

  let meterProvider: MeterProvider | undefined;
  let stopRuntime = () => {};
  if (signals.metrics) {
    meterProvider = new MeterProvider({
      resource,
      readers: [new PeriodicExportingMetricReader({ exporter: new OTLPMetricExporter({ ...exporterOptions, url: `${dsn.endpoint}/v1/metrics` }), exportIntervalMillis: exportInterval })],
    });
    stopRuntime = registerRuntimeMetrics(meterProvider.getMeter(`${instrumentationName}/runtime`), config.runtimeMetricsIntervalMs ?? 1_000);
  }
  const meter = meterProvider?.getMeter(`${instrumentationName}/express`);
  const requestCount = meter?.createCounter("http.server.request.count", { unit: "{request}" });
  const requestDuration = createHTTPServerRequestDurationHistogram(meter);
  const activeRequests = meter?.createUpDownCounter("http.server.active_requests", { unit: "{request}" });

  let loggerProvider: LoggerProvider | undefined;
  if (signals.logs) {
    loggerProvider = new LoggerProvider({
      resource,
      processors: [new BatchLogRecordProcessor(
        new OTLPLogExporter({ ...exporterOptions, url: `${dsn.endpoint}/v1/logs` }),
        {
          scheduledDelayMillis: exportInterval,
          maxQueueSize: OTEL_LOG_QUEUE_SIZE,
          maxExportBatchSize: OTEL_LOG_BATCH_SIZE,
        },
      )],
    });
  }
  const otelLogger = loggerProvider?.getLogger(instrumentationName);
  const requestStates = new WeakMap<Response, RequestState>();
  let closed = false;
  let shutdownPromise: Promise<void> | undefined;
  let droppedTelemetry = 0;
  const droppedCounter = meterProvider
    ?.getMeter(`${instrumentationName}/delivery`)
    .createCounter("lazy_observe_sdk_dropped_telemetry", {
      description: "Telemetry items dropped by bounded SDK delivery.",
      unit: "{item}",
    });
  const recordTelemetryDrop = (signal: "error" | "profile", drop: DeliveryDrop) => {
    droppedTelemetry += drop.count;
    droppedCounter?.add(drop.count, { signal, reason: drop.reason });
    console.warn(`lazy-observe ${signal} telemetry dropped`, {
      reason: drop.reason,
      count: drop.count,
      error: drop.error,
    });
  };
  const delivery = config.delivery ?? {};
  const maxAttempts = delivery.maxAttempts ?? DELIVERY_MAX_ATTEMPTS;
  const retryInitialMs = delivery.retryInitialMs ?? DELIVERY_RETRY_INITIAL_MS;
  const retryMaxMs = delivery.retryMaxMs ?? DELIVERY_RETRY_MAX_MS;
  const stopProfiling = profiling.enabled ? startProfiling({
    endpoint: dsn.endpoint,
    token: dsn.token,
    service: serviceName,
    intervalMs: profiling.intervalMs,
    warn: (message, error) => console.warn(message, error),
    queueCapacity: delivery.profileQueueCapacity ?? PROFILE_QUEUE_CAPACITY,
    maxAttempts,
    retryInitialMs,
    retryMaxMs,
    onDrop: (drop) => recordTelemetryDrop("profile", drop),
  }) : async () => {};

  const current = () => trace.getSpan(contextManager.active())?.spanContext();
  const emitLog = (level: number, body: string, fields: Record<string, unknown>, ctx = contextManager.active()) => {
    if (!otelLogger) return;
    const value = severity(level);
    otelLogger.emit({ severityNumber: value.number, severityText: value.text, body, attributes: attributes({ "service.name": serviceName, ...fields }), context: ctx });
  };
  const sendErrors = async (events: ErrorEvent[], signal: AbortSignal) => {
    const response = await fetch(`${dsn.endpoint}/api/events/errors`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${dsn.token}` },
      body: JSON.stringify({ events }),
      signal: AbortSignal.any([signal, AbortSignal.timeout(ERROR_SEND_TIMEOUT_MS)]),
    });
    if (!response.ok) throw new Error(`error event export failed: HTTP ${response.status}`);
  };
  const errorDelivery = new DeliveryQueue<ErrorEvent>({
    capacity: delivery.errorQueueCapacity ?? ERROR_QUEUE_CAPACITY,
    batchSize: ERROR_BATCH_SIZE,
    flushIntervalMs: ERROR_FLUSH_INTERVAL_MS,
    maxAttempts,
    retryInitialMs,
    retryMaxMs,
    send: sendErrors,
    onDrop: (drop) => recordTelemetryDrop("error", drop),
  });
  const reportError = (error: unknown, options?: { url?: string; spanContext?: SpanContext }) => {
    if (closed) return Promise.resolve();
    const event = errorEvent(error, {
      spanContext: options?.spanContext ?? current(),
      release: config.release,
      environment: config.environment,
      tags: config.tags,
      url: options?.url,
    });
    errorDelivery.enqueue(event);
    return Promise.resolve();
  };

  const express = (): RequestHandler => (req, res, next) => {
    const start = process.hrtime.bigint();
    const parent = propagator.extract(ROOT_CONTEXT, req.headers, {
      keys: (carrier) => Object.keys(carrier),
      get: (carrier, key) => {
        const value = carrier[key.toLowerCase()];
        return Array.isArray(value) ? value : value === undefined ? undefined : String(value);
      },
    });
    const method = req.method;
    const path = req.path || req.url.split("?")[0] || "/";
    const span = tracer.startSpan(`${method} ${path}`, {
      kind: SpanKind.SERVER,
      attributes: {
        "http.request.method": method,
        "url.scheme": req.protocol,
        "url.path": path,
        "server.address": req.hostname,
        "network.protocol.version": req.httpVersion,
      },
    }, parent);
    const ctx = trace.setSpan(parent, span);
    requestStates.set(res, { span, context: ctx, start, method, path });
    activeRequests?.add(1, { "http.request.method": method });
    let ended = false;
    const finish = () => {
      if (ended) return;
      ended = true;
      const state = requestStates.get(res)!;
      const route = (req.route as { path?: string } | undefined)?.path ?? state.path;
      const status = res.statusCode;
      const attrs: Attributes = { "http.request.method": method, "http.route": route, "http.response.status_code": status };
      span.updateName(`${method} ${route}`);
      span.setAttributes(attrs);
      if (status >= 500) span.setStatus({ code: SpanStatusCode.ERROR });
      const duration = Number(process.hrtime.bigint() - start) / 1e9;
      requestCount?.add(1, attrs);
      requestDuration?.record(duration, attrs);
      activeRequests?.add(-1, { "http.request.method": method });
      span.end();
    };
    res.once("finish", finish);
    res.once("close", finish);
    contextManager.with(ctx, next);
  };

  const expressError = (): ErrorRequestHandler => (error, req, res, next) => {
    const state = requestStates.get(res);
    state?.span.recordException(error instanceof Error ? error : new Error(String(error)));
    state?.span.setStatus({ code: SpanStatusCode.ERROR, message: error instanceof Error ? error.message : String(error) });
    const url = `${req.protocol}://${req.get("host") ?? req.hostname}${req.originalUrl}`;
    void reportError(error, { url, spanContext: state?.span.spanContext() });
    next(error);
  };

  const makePino = (options: PinoOptions = {}): PinoLogger => {
    const userHook = options.hooks?.logMethod;
    const hooks = {
      ...options.hooks,
      logMethod(this: PinoLogger, args: unknown[], method: (...args: unknown[]) => void, level: number) {
        const sc = current();
        const first = typeof args[0] === "object" && args[0] !== null ? args[0] as Record<string, unknown> : {};
        const message = typeof args[0] === "string" ? args[0] : typeof args[1] === "string" ? args[1] : "";
        const correlation = { "service.name": serviceName, ...(sc && trace.isSpanContextValid(sc) ? { trace_id: sc.traceId, span_id: sc.spanId } : {}) };
        emitLog(level, message, { ...first, ...correlation });
        const enriched = typeof args[0] === "object" && args[0] !== null ? [{ ...first, ...correlation }, ...args.slice(1)] : [correlation, ...args];
        if (userHook) (userHook as (...values: unknown[]) => void).call(this, enriched, method, level);
        else method.apply(this, enriched);
      },
    };
    return pinoFactory({ ...options, hooks }, (config.teeToStdout ?? true) ? pinoFactory.destination(1) : pinoFactory.destination("/dev/null"));
  };

  class LazyObserveTransport extends TransportStream {
    log(info: Record<string, unknown>, callback: () => void): void {
      setImmediate(() => this.emit("logged", info));
      const sc = current();
      if (sc && trace.isSpanContextValid(sc)) { info.trace_id = sc.traceId; info.span_id = sc.spanId; }
      info["service.name"] = serviceName;
      const level = String(info.level ?? "info");
      const numeric = level === "error" ? 50 : level === "warn" ? 40 : level === "debug" ? 20 : 30;
      emitLog(numeric, String(info.message ?? ""), info);
      if (config.teeToStdout ?? true) process.stdout.write(`${JSON.stringify(info)}\n`);
      callback();
    }
  }

  const onUncaught = (error: Error) => { void reportError(error); };
  const onRejection = (reason: unknown) => {
    void reportError(reason instanceof Error ? reason : new Error(String(reason)));
  };
  if (config.captureUnhandledErrors) {
    process.on("uncaughtExceptionMonitor", onUncaught);
    process.on("unhandledRejection", onRejection);
  }

  return {
    express, expressError, pino: makePino, winston: () => new LazyObserveTransport(), reportError,
    wrapLLMFetch: (baseFetch) => createWrappedLLMFetch(traceProvider?.getTracer(`${instrumentationName}/llm`) ?? new ProxyTracerProvider().getTracer(`${instrumentationName}/llm`), config.captureLLMContent ?? false, baseFetch, contextManager),
    signals,
    diagnostics: () => ({ droppedTelemetry }),
    shutdown(options: ShutdownOptions = {}) {
      if (shutdownPromise) return shutdownPromise;
      closed = true;
      if (config.captureUnhandledErrors) {
        process.off("uncaughtExceptionMonitor", onUncaught);
        process.off("unhandledRejection", onRejection);
      }
      stopRuntime();
      const timeoutMs = options.timeoutMs ?? config.shutdownTimeoutMs ?? SHUTDOWN_TIMEOUT_MS;
      shutdownPromise = shutdownWithin(timeoutMs, async () => {
        const deliveryResults = await Promise.allSettled([
          errorDelivery.close({ timeoutMs }),
          stopProfiling({ timeoutMs }),
        ]);
        const providerResults = await Promise.allSettled(
          [loggerProvider?.shutdown(), meterProvider?.shutdown(), traceProvider?.shutdown()].filter(
            (promise): promise is Promise<void> => promise !== undefined,
          ),
        );
        const errors = [...deliveryResults, ...providerResults]
          .filter((result): result is PromiseRejectedResult => result.status === "rejected")
          .map((result) => result.reason);
        if (errors.length === 1) throw errors[0];
        if (errors.length > 1) throw new AggregateError(errors, "lazy-observe shutdown failed");
      }).finally(() => contextManager.disable());
      return shutdownPromise;
    },
  };
}

function shutdownWithin(timeoutMs: number, shutdown: () => Promise<void>): Promise<void> {
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    return Promise.reject(new Error("shutdown timeout must be positive"));
  }
  let timer: ReturnType<typeof setTimeout> | undefined;
  const work = shutdown();
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`lazy-observe shutdown timed out after ${timeoutMs}ms`)), timeoutMs);
    timer.unref();
  });
  return Promise.race([work, timeout]).finally(() => {
    if (timer) clearTimeout(timer);
  });
}
