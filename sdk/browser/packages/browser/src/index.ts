import { Client } from "./client";
import type { ActiveSpan, BrowserClient, BrowserConfig, CaptureOptions, StartSpanOptions } from "./types";

let current: BrowserClient | undefined;

export function init(config: BrowserConfig): BrowserClient {
  if (current) void current.close();
  current = new Client(config).start();
  return current;
}

export function captureException(error: unknown, options?: CaptureOptions): string | undefined {
  return current?.captureException(error, options);
}

export function startSpan<T>(options: StartSpanOptions, callback: (span: ActiveSpan) => T): T {
  if (!current) throw new Error("Call init() before startSpan()");
  return current.startSpan(options, callback);
}

export function setPage(page: string): void { current?.setPage(page); }

export function wrapFetch(fetchImpl?: typeof fetch): typeof fetch {
  if (!current) throw new Error("Call init() before wrapFetch()");
  return current.wrapFetch(fetchImpl);
}

export { parseDsn } from "./dsn";
export { parseStack, isInApp } from "./stack";
export { MinuteRateLimiter } from "./rate-limit";
export { ReplayRingBuffer } from "./replay-buffer";
export { createTraceContext, isTraceparent, parseTraceparent, traceparent } from "./trace";
export type { TraceContext } from "./trace";
export type {
  ActiveSpan,
  BrowserClient,
  BrowserConfig,
  BrowserDiagnostics,
  CaptureOptions,
  DeliveryConfig,
  ErrorEventPayload,
  ReplayConfig,
  StackFrame,
  StartSpanOptions,
  TraceTarget,
  VitalName,
} from "./types";
